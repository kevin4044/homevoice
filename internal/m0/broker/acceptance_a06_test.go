package broker

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// TestA06HandshakeQoSAndBounds covers acceptance A06 (R02): MQTT 3.1.1
// CONNECT/CONNACK, QoS0, QoS1 with PUBACK, SUBSCRIBE/SUBACK, PINGREQ/PINGRESP,
// DISCONNECT, explicit rejection of QoS2/other protocol versions, bounded
// rejection of malformed/oversized packets, bounded CONNECT handshake timeout
// and keepalive cleanup of half-open connections.
func TestA06HandshakeQoSAndBounds(t *testing.T) {
	cfg := testConfig(t)
	devA, devB := cfg.Devices[0], cfg.Devices[1]

	t.Run("mqtt311 handshake qos puback ping disconnect", func(t *testing.T) {
		b, rec := startBroker(t, cfg, nil)
		addr := b.Addr()

		client, incoming := connectDevice(t, addr, devA)
		if got := subscribe(t, client, devA.SubscribeTopic, 1)[devA.SubscribeTopic]; got != 1 {
			t.Fatalf("SUBACK granted QoS %d, want 1", got)
		}

		// QoS0 uplink: no PUBACK, callback QoS0.
		publish(t, client, devA.PublishTopic, []byte("qos0-uplink"), 0, false)
		msg := rec.waitMessage(t, 3*time.Second, func(m Message) bool {
			return string(m.Payload) == "qos0-uplink"
		})
		if msg.QoS != 0 {
			t.Fatalf("QoS0 uplink callback QoS = %d, want 0", msg.QoS)
		}

		// QoS1 uplink: the paho token completes only after PUBACK, and the
		// callback reports QoS1.
		tok := publish(t, client, devA.PublishTopic, []byte("qos1-uplink"), 1, false)
		if err := tok.Error(); err != nil {
			t.Fatalf("QoS1 uplink without PUBACK: %v", err)
		}
		msg = rec.waitMessage(t, 3*time.Second, func(m Message) bool {
			return string(m.Payload) == "qos1-uplink"
		})
		if msg.QoS != 1 {
			t.Fatalf("QoS1 uplink callback QoS = %d, want 1", msg.QoS)
		}

		// Downlink QoS1 delivery over the SUBSCRIBE/SUBACK session.
		if err := b.Publish(devA.DeviceID, []byte("qos1-downlink")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		down := waitPahoMessage(t, incoming, 3*time.Second)
		if !bytes.Equal(down.Payload(), []byte("qos1-downlink")) || down.Qos() != 1 || down.Retained() {
			t.Fatalf("downlink delivery mismatch: topic=%q payload=%q qos=%d retained=%v",
				down.Topic(), down.Payload(), down.Qos(), down.Retained())
		}

		// Raw MQTT 3.1.1 client for PINGREQ/PINGRESP and DISCONNECT.
		rawConn, code := rawConnect(t, addr, devB, 4, 10)
		if code != 0x00 {
			t.Fatalf("raw CONNECT CONNACK = 0x%02x, want 0x00", code)
		}
		generationB, ok := b.Online(devB.DeviceID)
		if !ok {
			t.Fatal("raw CONNECT did not register device B")
		}
		if err := writeAll(t, rawConn, []byte{0xC0, 0x00}); err != nil { // PINGREQ
			t.Fatalf("write PINGREQ: %v", err)
		}
		packetType, body := readRawPacket(t, rawConn)
		if packetType != 0xD0 || len(body) != 0 {
			t.Fatalf("expected PINGRESP (0xD0, empty), got 0x%02x %v", packetType, body)
		}
		if err := writeAll(t, rawConn, []byte{0xE0, 0x00}); err != nil { // DISCONNECT
			t.Fatalf("write DISCONNECT: %v", err)
		}
		silentRead(t, rawConn, 2*time.Second)
		ev := rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
			return e.DeviceID == devB.DeviceID && e.Generation == generationB
		})
		if ev.Reason != reasonClientDisconnect {
			t.Fatalf("raw DISCONNECT offline reason = %q, want %q", ev.Reason, reasonClientDisconnect)
		}

		client.Disconnect(100)
		rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool { return e.DeviceID == devA.DeviceID })
		waitOffline(t, b, devA.DeviceID, 2*time.Second)
	})

	t.Run("unsupported protocol versions and qos2 rejected explicitly", func(t *testing.T) {
		b, rec := startBroker(t, cfg, nil)
		addr := b.Addr()

		// MQTT 3.1 (protocol level 3) is rejected with CONNACK 0x01.
		v3Conn, code := rawConnect(t, addr, devA, 3, 10)
		if code != 0x01 {
			t.Fatalf("MQTT 3.1 CONNACK = 0x%02x, want 0x01 unsupported protocol version", code)
		}
		silentRead(t, v3Conn, 2*time.Second)

		// MQTT 5 is rejected with CONNACK 0x84.
		v5Conn := dialRaw(t, addr)
		if err := writeAll(t, v5Conn, connectPacket(devA.ClientID, devA.Username, devA.Password, 5, 10)); err != nil {
			t.Fatalf("write v5 CONNECT: %v", err)
		}
		packetType, body := readRawPacket(t, v5Conn)
		if packetType != 0x20 || len(body) < 2 {
			t.Fatalf("expected v5 CONNACK, got 0x%02x %v", packetType, body)
		}
		if body[1] != 0x84 {
			t.Fatalf("MQTT 5 rejected with CONNACK 0x%02x, want 0x84 unsupported protocol version", body[1])
		}
		silentRead(t, v5Conn, 2*time.Second)
		if _, online := b.Online(devA.DeviceID); online {
			t.Fatal("rejected protocol version created an online session")
		}

		// QoS2 PUBLISH after an authenticated CONNECT is rejected by closing
		// the connection; no PUBREC, no callback, nothing forwarded.
		qos2Conn, code := rawConnect(t, addr, devA, 4, 10)
		if code != 0x00 {
			t.Fatalf("CONNECT for QoS2 case = 0x%02x", code)
		}
		_ = writeAll(t, qos2Conn, publishPacket(devA.PublishTopic, []byte("qos2-must-be-rejected"), 2, false, 7))
		silentRead(t, qos2Conn, 2*time.Second)
		waitOffline(t, b, devA.DeviceID, 2*time.Second)
		for _, m := range rec.messagesSnapshot() {
			if string(m.Payload) == "qos2-must-be-rejected" {
				t.Fatal("QoS2 publish reached the trusted uplink callback")
			}
		}

		// Broker still healthy.
		client, _ := connectDevice(t, addr, devA)
		publish(t, client, devA.PublishTopic, []byte("after-qos2"), 0, false)
		rec.waitMessage(t, 3*time.Second, func(m Message) bool { return string(m.Payload) == "after-qos2" })
	})

	t.Run("malformed packets bounded and broker healthy", func(t *testing.T) {
		b, rec := startBroker(t, cfg, nil)
		addr := b.Addr()

		// Invalid fixed-header flags on the first packet.
		conn1 := dialRaw(t, addr)
		_ = writeAll(t, conn1, []byte{0x11})
		if elapsed := silentRead(t, conn1, 2*time.Second); elapsed > 2*time.Second {
			t.Fatalf("malformed packet rejection was not bounded: %s", elapsed)
		}

		// Truncated CONNECT remaining length.
		conn2 := dialRaw(t, addr)
		_ = writeAll(t, conn2, []byte{0x10, 0x80})
		if tcp, ok := conn2.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		silentRead(t, conn2, 2*time.Second)

		// Malformed packet after a valid CONNECT.
		conn3, code := rawConnect(t, addr, devA, 4, 10)
		if code != 0x00 {
			t.Fatalf("CONNECT for malformed case = 0x%02x", code)
		}
		_ = writeAll(t, conn3, []byte{0x11})
		silentRead(t, conn3, 2*time.Second)
		waitOffline(t, b, devA.DeviceID, 2*time.Second)

		// Broker remains healthy after malformed traffic.
		client, _ := connectDevice(t, addr, devA)
		publish(t, client, devA.PublishTopic, []byte("after-malformed"), 0, false)
		rec.waitMessage(t, 3*time.Second, func(m Message) bool { return string(m.Payload) == "after-malformed" })
	})

	t.Run("oversized packet rejected bounded and broker healthy", func(t *testing.T) {
		b, rec := startBroker(t, cfg, nil)
		addr := b.Addr()

		conn, code := rawConnect(t, addr, devA, 4, 10)
		if code != 0x00 {
			t.Fatalf("CONNECT for oversized case = 0x%02x", code)
		}
		generation, ok := b.Online(devA.DeviceID)
		if !ok {
			t.Fatal("CONNECT did not register the device")
		}

		// Declare a remaining length above 64 KiB. The broker must reject on
		// the fixed header, before allocating/reading the whole packet.
		oversized := []byte{0x30}
		oversized = appendVarint(oversized, limits.MaxMQTTPacketBytes+4096)
		if err := writeAll(t, conn, oversized); err != nil {
			t.Logf("write of oversized header ended early (expected): %v", err)
		}
		_ = writeAll(t, conn, bytes.Repeat([]byte{0x41}, 8192)) // part of the body; may fail
		silentRead(t, conn, 2*time.Second)
		ev := rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
			return e.DeviceID == devA.DeviceID && e.Generation == generation
		})
		if ev.Reason == "" {
			t.Fatal("oversized packet did not produce a bounded disconnect")
		}
		for _, m := range rec.messagesSnapshot() {
			if len(m.Payload) > limits.MaxJSONBytes {
				t.Fatalf("oversized publish reached the uplink callback (%d bytes)", len(m.Payload))
			}
		}

		// Broker still healthy after the oversized packet.
		client, _ := connectDevice(t, addr, devA)
		subscribe(t, client, devA.SubscribeTopic, 0)
		if err := b.Publish(devA.DeviceID, []byte("after-oversize")); err != nil {
			t.Fatalf("Publish after oversized packet: %v", err)
		}
	})

	t.Run("connect handshake timeout bounded", func(t *testing.T) {
		b, _ := startBroker(t, cfg, func(o *Options) {
			o.ConnectTimeout = 150 * time.Millisecond
		})
		addr := b.Addr()

		conn := dialRaw(t, addr)
		start := time.Now()
		elapsed := silentRead(t, conn, 2*time.Second)
		if elapsed < 50*time.Millisecond {
			t.Fatalf("silent connection closed too early (%s), ConnectTimeout was 150ms", elapsed)
		}
		if total := time.Since(start); total > 2*time.Second {
			t.Fatalf("CONNECT handshake timeout not bounded: %s", total)
		}

		// Broker accepts a valid client after the timeout cleanup.
		client, _ := connectDevice(t, addr, devA)
		publish(t, client, devA.PublishTopic, []byte("after-connect-timeout"), 0, false)
	})

	t.Run("keepalive closes half-open connection using inbound activity only", func(t *testing.T) {
		b, rec := startBroker(t, cfg, nil)
		addr := b.Addr()

		conn, code := rawConnect(t, addr, devB, 4, 1) // keepalive = 1s
		if code != 0x00 {
			t.Fatalf("CONNECT = 0x%02x", code)
		}
		generation, ok := b.Online(devB.DeviceID)
		if !ok {
			t.Fatal("CONNECT did not register device B")
		}
		if err := writeAll(t, conn, subscribePacket(1, devB.SubscribeTopic, 0)); err != nil {
			t.Fatalf("write SUBSCRIBE: %v", err)
		}
		packetType, _ := readRawPacket(t, conn)
		if packetType != 0x90 {
			t.Fatalf("expected SUBACK 0x90, got 0x%02x", packetType)
		}

		// Stay silent inbound while the broker writes downlinks: outbound
		// activity must not keep the half-open connection alive.
		closed := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			buf := make([]byte, 512)
			for {
				if _, err := conn.Read(buf); err != nil {
					closed <- time.Since(start)
					return
				}
			}
		}()

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(4 * time.Second)
		var elapsed time.Duration
		gotClosed := false
		for !gotClosed {
			select {
			case elapsed = <-closed:
				gotClosed = true
			case <-ticker.C:
				_ = b.Publish(devB.DeviceID, []byte("keepalive-outbound-probe"))
			case <-deadline:
				t.Fatal("half-open connection was not cleaned up by keepalive")
			}
		}
		if elapsed < 900*time.Millisecond {
			t.Fatalf("keepalive closed the connection after only %s; want ~1.5s (inbound activity only)", elapsed)
		}
		if elapsed > 3500*time.Millisecond {
			t.Fatalf("keepalive cleanup took %s, want <= 3.5s", elapsed)
		}

		ev := rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
			return e.DeviceID == devB.DeviceID && e.Generation == generation
		})
		if ev.Reason != reasonKeepaliveTimeout {
			t.Fatalf("half-open offline reason = %q, want %q", ev.Reason, reasonKeepaliveTimeout)
		}
		waitOffline(t, b, devB.DeviceID, 2*time.Second)

		// Broker healthy afterwards.
		client, _ := connectDevice(t, addr, devA)
		publish(t, client, devA.PublishTopic, []byte("after-keepalive"), 0, false)
	})
}
