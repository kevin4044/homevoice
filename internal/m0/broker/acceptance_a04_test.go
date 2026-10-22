package broker

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestA04ACLAndDownlinkIsolation covers acceptance A04 (R03): wildcard,
// cross-device, admin/$SYS subscriptions and publishes are rejected, retained
// control publishes are not stored or replayed, and downlink messages reach
// only the approved subscriber.
func TestA04ACLAndDownlinkIsolation(t *testing.T) {
	cfg := testConfig(t)
	devA, devB := cfg.Devices[0], cfg.Devices[1]
	const marker = "A04-PAYLOAD-MARKER-DO-NOT-LOG"
	logger, logs := captureLogger(cfg.Secrets())
	b, rec := startBroker(t, cfg, func(o *Options) { o.Logger = logger })
	addr := b.Addr()

	clientA, _ := connectDevice(t, addr, devA)
	clientB, incomingB := connectDevice(t, addr, devB)

	// Approved exact subscriptions succeed.
	if got := subscribe(t, clientA, devA.SubscribeTopic, 1)[devA.SubscribeTopic]; got != 1 {
		t.Fatalf("A granted QoS %d for its own downlink, want 1", got)
	}
	if got := subscribe(t, clientB, devB.SubscribeTopic, 1)[devB.SubscribeTopic]; got != 1 {
		t.Fatalf("B granted QoS %d for its own downlink, want 1", got)
	}

	// Every subscription outside the device's exact downlink topic is
	// rejected with a failure SUBACK (0x80 in MQTT 3.1.1).
	rejectedFilters := []string{
		devB.SubscribeTopic, // other device's downlink
		devA.PublishTopic,   // shared uplink topic
		"#",
		"+",
		"devices/p2p/+",
		"devices/#",
		"$SYS/#",
		"admin/control",
	}
	for _, filter := range rejectedFilters {
		t.Run("subscribe rejected "+filter, func(t *testing.T) {
			result := subscribe(t, clientA, filter, 1)
			code, ok := result[filter]
			if !ok {
				t.Fatalf("SUBACK has no result for filter %q: %v", filter, result)
			}
			if code != 0x80 {
				t.Fatalf("SUBACK for %q granted 0x%02x, want failure 0x80", filter, code)
			}
		})
	}

	// QoS0 publishes outside the device grant are dropped without a callback,
	// without disconnecting the client and without reaching B.
	for _, topic := range []string{devB.SubscribeTopic, "admin/control", "$SYS/status", "devices/p2p/other"} {
		tok := publish(t, clientA, topic, []byte(marker+"-forbidden"), 0, false)
		if err := tok.Error(); err != nil {
			t.Fatalf("QoS0 publish to %q reported an error: %v", topic, err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if msgs := rec.messagesSnapshot(); len(msgs) != 0 {
		t.Fatalf("forbidden publishes produced trusted uplink callbacks: %+v", msgs)
	}
	assertNoPahoMessage(t, incomingB, 100*time.Millisecond, "B must not receive A's forbidden publishes")
	if _, online := b.Online(devA.DeviceID); !online {
		t.Fatal("QoS0 rejected publish must not disconnect A")
	}

	// A retained control publish is rejected: nothing is acted on or stored,
	// and the offending connection is closed so the publish cannot be read as
	// accepted.
	retainedPayload := []byte(marker + "-retained")
	retainedTok := clientA.Publish(devA.PublishTopic, 0, true, retainedPayload)
	_ = retainedTok.WaitTimeout(2 * time.Second) // the connection is closed instead of acked
	waitOffline(t, b, devA.DeviceID, 3*time.Second)
	retainedEvent := rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
		return e.DeviceID == devA.DeviceID
	})
	if retainedEvent.Reason != reasonNotAuthorized {
		t.Fatalf("retained-control disconnect reason = %q, want %q", retainedEvent.Reason, reasonNotAuthorized)
	}
	// mochi publishes retained $SYS topics internally; the device's control
	// topic itself must have no retained message.
	for _, topic := range []string{devA.PublishTopic, devA.SubscribeTopic, devB.SubscribeTopic} {
		if retained := b.server.Topics.Messages(topic); len(retained) != 0 {
			t.Fatalf("retained control publish was stored on %q (%d message(s))", topic, len(retained))
		}
	}
	for _, m := range rec.messagesSnapshot() {
		if bytes.Contains(m.Payload, []byte(marker+"-retained")) {
			t.Fatal("retained control publish was acted on as a trusted uplink")
		}
	}

	// Reconnect A: the rejected retained control must not be replayed on
	// subscription.
	clientA2, incomingA2 := connectDevice(t, addr, devA)
	subscribe(t, clientA2, devA.SubscribeTopic, 1)
	assertNoPahoMessage(t, incomingA2, 150*time.Millisecond, "rejected retained control must not be replayed")

	// Downlink isolation: only the approved subscriber receives each downlink.
	downA := []byte(`{"type":"notify","text":"to-A"}`)
	downB := []byte(`{"type":"notify","text":"to-B"}`)
	beforeDownlinks := len(rec.messagesSnapshot())
	if err := b.Publish(devA.DeviceID, downA); err != nil {
		t.Fatalf("Publish(A downlink): %v", err)
	}
	msgA := waitPahoMessage(t, incomingA2, 3*time.Second)
	if msgA.Topic() != devA.SubscribeTopic {
		t.Fatalf("A downlink topic = %q, want %q", msgA.Topic(), devA.SubscribeTopic)
	}
	if !bytes.Equal(msgA.Payload(), downA) {
		t.Fatalf("A downlink payload mismatch: %q", msgA.Payload())
	}
	if msgA.Retained() {
		t.Fatal("downlink must not be retained")
	}
	if msgA.Qos() != 1 {
		t.Fatalf("default downlink QoS = %d, want 1", msgA.Qos())
	}
	assertNoPahoMessage(t, incomingB, 150*time.Millisecond, "B must not receive A's downlink")

	if err := b.Publish(devB.DeviceID, downB); err != nil {
		t.Fatalf("Publish(B downlink): %v", err)
	}
	msgB := waitPahoMessage(t, incomingB, 3*time.Second)
	if msgB.Topic() != devB.SubscribeTopic {
		t.Fatalf("B downlink topic = %q, want %q", msgB.Topic(), devB.SubscribeTopic)
	}
	assertNoPahoMessage(t, incomingA2, 150*time.Millisecond, "A must not receive B's downlink")

	if after := len(rec.messagesSnapshot()); after != beforeDownlinks {
		t.Fatalf("downlink publishes were reported as trusted uplinks")
	}

	// Offline devices return ErrOffline; downlink identity is not inferred.
	clientB.Disconnect(100)
	rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
		return e.DeviceID == devB.DeviceID
	})
	if err := b.Publish(devB.DeviceID, downB); !errors.Is(err, ErrOffline) {
		t.Fatalf("Publish to offline B = %v, want ErrOffline", err)
	}
	if err := b.Publish("unknown-device", downB); !errors.Is(err, ErrOffline) {
		t.Fatalf("Publish to unknown device = %v, want ErrOffline", err)
	}

	// A QoS1 publish to an admin topic is explicitly rejected by closing the
	// connection (MQTT 3.1.1 has no negative PUBACK). The payload marker must
	// not leak into the logs the library writes on this error path.
	clientA3, _ := connectDevice(t, addr, devA)
	clientA3.Publish("admin/control", 1, false, []byte(marker+"-admin-qos1")).
		WaitTimeout(2 * time.Second)
	waitOffline(t, b, devA.DeviceID, 3*time.Second)
	for _, m := range rec.messagesSnapshot() {
		if bytes.Contains(m.Payload, []byte(marker+"-admin-qos1")) {
			t.Fatal("admin-topic publish was acted on as a trusted uplink")
		}
	}

	// The broker is still healthy after all rejections.
	clientA4, _ := connectDevice(t, addr, devA)
	if _, ok := b.Online(devA.DeviceID); !ok {
		t.Fatal("broker did not accept a valid reconnection after rejections")
	}
	subscribe(t, clientA4, devA.SubscribeTopic, 1)
	if err := b.Publish(devA.DeviceID, downA); err != nil {
		t.Fatalf("Publish after rejections: %v", err)
	}

	assertLogsDoNotContain(t, logs, marker, devA.Password, devB.Password)
}
