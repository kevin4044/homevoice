package broker

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/config"
)

// TestA02IdentityAuthentication covers acceptance A02 (R02): only the exact
// pre-registered credentials succeed; empty, wrong and unknown credentials get
// a CONNACK failure and create no device session; login errors never log
// credentials.
func TestA02IdentityAuthentication(t *testing.T) {
	cfg := testConfig(t)
	devA, devB := cfg.Devices[0], cfg.Devices[1]
	logger, logs := captureLogger(cfg.Secrets())
	b, rec := startBroker(t, cfg, func(o *Options) { o.Logger = logger })
	addr := b.Addr()

	failedCases := []struct {
		name     string
		clientID string
		username string
		password string
	}{
		{"empty credentials", devA.ClientID, "", ""},
		{"wrong password", devA.ClientID, devA.Username, "not-the-registered-password"},
		{"unknown client id", "GID_homevoice@@@00_00_00_00_00_09", devA.Username, devA.Password},
		{"credentials of another device", devA.ClientID, devB.Username, devB.Password},
		{"unknown username", devA.ClientID, "hv-unregistered", devA.Password},
	}
	for _, tc := range failedCases {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialRaw(t, addr)
			if err := writeAll(t, conn, connectPacket(tc.clientID, tc.username, tc.password, 4, 10)); err != nil {
				t.Fatalf("write CONNECT: %v", err)
			}
			packetType, body := readRawPacket(t, conn)
			if packetType != 0x20 {
				t.Fatalf("expected CONNACK, got packet type 0x%02x", packetType)
			}
			if len(body) < 2 {
				t.Fatalf("short CONNACK body: %v", body)
			}
			if body[0] != 0x00 {
				t.Fatalf("failed CONNACK claims a session was present: %v", body)
			}
			if body[1] != 0x05 {
				t.Fatalf("CONNACK return code 0x%02x, want 0x05 (not authorized)", body[1])
			}
			// The rejected connection must be closed after the failure CONNACK.
			silentRead(t, conn, 2*time.Second)

			for _, dev := range []config.Device{devA, devB} {
				if gen, ok := b.Online(dev.DeviceID); ok {
					t.Fatalf("rejected credentials created an online session for %q (generation %d)", dev.DeviceID, gen)
				}
				if _, found := b.server.Clients.Get(dev.ClientID); found {
					t.Fatalf("rejected credentials created a broker client for %q", dev.DeviceID)
				}
			}
			if msgs := rec.messagesSnapshot(); len(msgs) != 0 {
				t.Fatalf("rejected credentials produced %d trusted uplink messages", len(msgs))
			}
			if offs := rec.offlinesSnapshot(); len(offs) != 0 {
				t.Fatalf("rejected credentials produced %d offline events", len(offs))
			}
		})
	}

	// Correct credentials succeed and the earlier failures did not consume a
	// connection generation.
	clientA, _ := connectDevice(t, addr, devA)
	generation, ok := b.Online(devA.DeviceID)
	if !ok {
		t.Fatal("correct credentials did not create an online session")
	}
	if generation != 1 {
		t.Fatalf("first authenticated generation = %d, want 1 (failed logins must not consume a generation)", generation)
	}

	// A real MQTT client with wrong credentials is rejected as well.
	wrong := devA
	wrong.Password = "paho-wrong-password"
	badClient := newPahoClient(addr, wrong, pahoOptions{})
	tok := badClient.Connect()
	if !tok.WaitTimeout(3 * time.Second) {
		t.Fatal("paho wrong-credential CONNECT timed out instead of failing")
	}
	if err := tok.Error(); err == nil {
		t.Fatal("paho wrong-credential CONNECT unexpectedly succeeded")
	}
	if got, stillOnline := b.Online(devA.DeviceID); !stillOnline || got != generation {
		t.Fatalf("wrong-credential attempt disturbed the valid session (gen=%d ok=%v)", got, stillOnline)
	}

	// Clean DISCONNECT removes the session and reports the correct generation.
	clientA.Disconnect(100)
	ev := rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
		return e.DeviceID == devA.DeviceID && e.Generation == generation
	})
	if ev.Reason != reasonClientDisconnect {
		t.Fatalf("offline reason = %q, want %q", ev.Reason, reasonClientDisconnect)
	}
	waitOffline(t, b, devA.DeviceID, 2*time.Second)

	// Login/ACL logs must not contain any credential.
	assertLogsDoNotContain(t, logs,
		devA.Password, devB.Password,
		devA.Username, devB.Username,
		"not-the-registered-password", "paho-wrong-password",
	)
}

// TestA03TrustedPublisherIdentity covers acceptance A03 (R03): two devices
// publishing the same shared uplink topic are attributed correctly, and a
// payload claiming another device's identity never changes attribution.
func TestA03TrustedPublisherIdentity(t *testing.T) {
	cfg := testConfig(t)
	devA, devB := cfg.Devices[0], cfg.Devices[1]
	const marker = "A03-PAYLOAD-MARKER-DO-NOT-LOG"
	logger, logs := captureLogger(cfg.Secrets())
	b, rec := startBroker(t, cfg, func(o *Options) { o.Logger = logger })
	addr := b.Addr()

	clientA, downA := connectDevice(t, addr, devA)
	clientB, _ := connectDevice(t, addr, devB)
	generationA, ok := b.Online(devA.DeviceID)
	if !ok {
		t.Fatal("device A not online")
	}
	generationB, ok := b.Online(devB.DeviceID)
	if !ok {
		t.Fatal("device B not online")
	}
	if generationA != 1 || generationB != 1 {
		t.Fatalf("initial generations = %d/%d, want 1/1", generationA, generationB)
	}
	if devA.PublishTopic != devB.PublishTopic {
		t.Fatalf("test requires a shared uplink topic, got %q/%q", devA.PublishTopic, devB.PublishTopic)
	}
	subscribe(t, clientA, devA.SubscribeTopic, 1)
	subscribe(t, clientB, devB.SubscribeTopic, 1)

	// A publishes to the shared uplink topic with a payload that claims B's
	// identity. The payload must not change attribution.
	forgedA := []byte(fmt.Sprintf(
		`{"type":"hello","version":3,"transport":"udp","device_id":%q,"client_id":%q,"marker":%q}`,
		devB.DeviceID, devB.ClientID, marker))
	tok := publish(t, clientA, devA.PublishTopic, forgedA, 1, false)
	if err := tok.Error(); err != nil {
		t.Fatalf("QoS1 uplink publish failed: %v", err)
	}
	msgA := rec.waitMessage(t, 3*time.Second, func(m Message) bool { return m.DeviceID == devA.DeviceID })
	if msgA.DeviceID != devA.DeviceID {
		t.Fatalf("shared-topic message attributed to %q, want %q", msgA.DeviceID, devA.DeviceID)
	}
	if msgA.Generation != generationA {
		t.Fatalf("message generation = %d, want %d", msgA.Generation, generationA)
	}
	if msgA.Topic != devA.PublishTopic {
		t.Fatalf("message topic = %q, want %q", msgA.Topic, devA.PublishTopic)
	}
	if msgA.QoS != 1 {
		t.Fatalf("message QoS = %d, want 1", msgA.QoS)
	}
	if !bytes.Equal(msgA.Payload, forgedA) {
		t.Fatalf("broker altered the uplink payload:\n got %q\nwant %q", msgA.Payload, forgedA)
	}

	// B publishes to the same topic with a payload claiming A's identity; the
	// callback must still attribute the message to B.
	forgedB := []byte(fmt.Sprintf(
		`{"type":"hello","version":3,"transport":"udp","device_id":%q,"marker":%q}`,
		devA.DeviceID, marker+"-B"))
	publish(t, clientB, devB.PublishTopic, forgedB, 0, false)
	msgB := rec.waitMessage(t, 3*time.Second, func(m Message) bool { return m.DeviceID == devB.DeviceID })
	if msgB.Generation != generationB {
		t.Fatalf("message generation = %d, want %d", msgB.Generation, generationB)
	}
	if msgB.QoS != 0 {
		t.Fatalf("message QoS = %d, want 0", msgB.QoS)
	}
	if !bytes.Equal(msgB.Payload, forgedB) {
		t.Fatalf("broker altered B's uplink payload")
	}

	// No message may be attributed to the identity claimed inside a payload.
	for _, m := range rec.messagesSnapshot() {
		if m.DeviceID == devB.DeviceID && bytes.Equal(m.Payload, forgedA) {
			t.Fatalf("payload-supplied identity overrode the authenticated connection")
		}
		if m.DeviceID == devA.DeviceID && bytes.Equal(m.Payload, forgedB) {
			t.Fatalf("payload-supplied identity overrode the authenticated connection")
		}
	}

	// A broker-originated downlink is not a trusted device uplink and must not
	// invoke OnMessage.
	before := len(rec.messagesSnapshot())
	downlink := []byte(`{"type":"notify","text":"A03 downlink"}`)
	if err := b.Publish(devA.DeviceID, downlink); err != nil {
		t.Fatalf("Publish downlink: %v", err)
	}
	downMsg := waitPahoMessage(t, downA, 3*time.Second)
	if downMsg.Topic() != devA.SubscribeTopic {
		t.Fatalf("downlink topic = %q, want %q", downMsg.Topic(), devA.SubscribeTopic)
	}
	if !bytes.Equal(downMsg.Payload(), downlink) {
		t.Fatalf("downlink payload mismatch: %q", downMsg.Payload())
	}
	time.Sleep(100 * time.Millisecond) // allow any stray callback to land
	if after := len(rec.messagesSnapshot()); after != before {
		t.Fatalf("broker-originated downlink was reported as a trusted uplink (%d -> %d messages)", before, after)
	}

	// mochi publishes its own retained $SYS topics, so assert the device
	// topics themselves have no retained message rather than a global count.
	if retained := b.server.Topics.Messages(devA.PublishTopic); len(retained) != 0 {
		t.Fatalf("uplink topic retained %d message(s)", len(retained))
	}
	if retained := b.server.Topics.Messages(devA.SubscribeTopic); len(retained) != 0 {
		t.Fatalf("downlink topic retained %d message(s)", len(retained))
	}

	// Payload content must never reach logs.
	assertLogsDoNotContain(t, logs, marker)

	// Sanity: ErrOffline for a device that never connected (identity is not
	// inferred from topics or payloads).
	if err := b.Publish("dev-never-connected", []byte("x")); !errors.Is(err, ErrOffline) {
		t.Fatalf("Publish to unknown device error = %v, want ErrOffline", err)
	}
}
