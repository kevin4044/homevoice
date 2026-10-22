package broker

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mochi-mqtt/server/v2/packets"
)

// TestA05DuplicateClientReconnect covers acceptance A05 (R02-R04): a duplicate
// client id replaces the old connection, increments the generation, the old
// close callback cannot clear the new registration, and old-generation
// messages/frames never reach the new session.
func TestA05DuplicateClientReconnect(t *testing.T) {
	cfg := testConfig(t)
	devA := cfg.Devices[0]
	b, rec := startBroker(t, cfg, nil)
	addr := b.Addr()

	a1, incoming1 := connectDevice(t, addr, devA)
	gen1, ok := b.Online(devA.DeviceID)
	if !ok || gen1 != 1 {
		t.Fatalf("first connection generation = %d (ok=%v), want 1", gen1, ok)
	}
	subscribe(t, a1, devA.SubscribeTopic, 1)
	publish(t, a1, devA.PublishTopic, []byte("uplink-gen1"), 0, false)
	msg1 := rec.waitMessage(t, 3*time.Second, func(m Message) bool {
		return m.Generation == gen1 && string(m.Payload) == "uplink-gen1"
	})
	if msg1.DeviceID != devA.DeviceID {
		t.Fatalf("gen1 message attributed to %q, want %q", msg1.DeviceID, devA.DeviceID)
	}
	b.mu.Lock()
	staleClient := b.online[devA.DeviceID].client
	b.mu.Unlock()
	if staleClient == nil {
		t.Fatal("gen1 client was not registered")
	}
	if err := b.Publish(devA.DeviceID, []byte("downlink-gen1")); err != nil {
		t.Fatalf("Publish to gen1: %v", err)
	}
	waitPahoMessage(t, incoming1, 3*time.Second)

	// A new connection with the same registered client id replaces the old
	// one and must still be accepted (takeover, not a second identity).
	a2, incoming2 := connectDevice(t, addr, devA)
	gen2 := waitGeneration(t, b, devA.DeviceID, gen1+1)
	waitFor(t, 3*time.Second, "old connection closed by takeover", func() bool {
		return !a1.IsConnectionOpen()
	})
	// Hold the new registration open while the old connection unwinds: its
	// OnDisconnect must not clear or downgrade the replacement.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got, ok := b.Online(devA.DeviceID); !ok || got != gen2 {
			t.Fatalf("superseded connection disturbed the new registration: gen=%d ok=%v (want %d)", got, ok, gen2)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if offs := rec.offlinesSnapshot(); len(offs) != 0 {
		t.Fatalf("connection takeover produced offline events for the superseded session: %+v", offs)
	}

	// White-box: even if a buffered frame from the superseded connection were
	// handed to the publish hook, the generation check drops it.
	if _, current := b.currentGeneration(devA.DeviceID, staleClient); current {
		t.Fatal("superseded client is still the current generation")
	}
	if _, err := b.hook.OnPublish(staleClient, packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish},
		TopicName:   devA.PublishTopic,
		Payload:     []byte("uplink-from-old-generation"),
	}); !errors.Is(err, packets.ErrRejectPacket) {
		t.Fatalf("stale connection publish error = %v, want ErrRejectPacket", err)
	}

	// Frames from the old client cannot affect the new session.
	a1.Publish(devA.PublishTopic, 0, false, []byte("uplink-from-old-generation")).
		WaitTimeout(500 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	for _, m := range rec.messagesSnapshot() {
		if string(m.Payload) == "uplink-from-old-generation" {
			t.Fatalf("stale generation message reached the broker: %+v", m)
		}
	}

	// The new session is fully functional and its messages carry gen2.
	publish(t, a2, devA.PublishTopic, []byte("uplink-gen2"), 0, false)
	msg2 := rec.waitMessage(t, 3*time.Second, func(m Message) bool {
		return m.Generation == gen2 && string(m.Payload) == "uplink-gen2"
	})
	if msg2.DeviceID != devA.DeviceID {
		t.Fatalf("gen2 message attributed to %q, want %q", msg2.DeviceID, devA.DeviceID)
	}
	subscribe(t, a2, devA.SubscribeTopic, 1)
	if err := b.Publish(devA.DeviceID, []byte("downlink-gen2")); err != nil {
		t.Fatalf("Publish to gen2: %v", err)
	}
	down2 := waitPahoMessage(t, incoming2, 3*time.Second)
	if !bytes.Equal(down2.Payload(), []byte("downlink-gen2")) {
		t.Fatalf("downlink after reconnect = %q", down2.Payload())
	}

	// A third connection increments the generation again.
	a3, incoming3 := connectDevice(t, addr, devA)
	gen3 := waitGeneration(t, b, devA.DeviceID, gen2+1)
	waitFor(t, 3*time.Second, "second old connection closed by takeover", func() bool {
		return !a2.IsConnectionOpen()
	})
	if got, ok := b.Online(devA.DeviceID); !ok || got != gen3 {
		t.Fatalf("online generation after third connect = %d (ok=%v), want %d", got, ok, gen3)
	}
	subscribe(t, a3, devA.SubscribeTopic, 1)
	if err := b.Publish(devA.DeviceID, []byte("downlink-gen3")); err != nil {
		t.Fatalf("Publish to gen3: %v", err)
	}
	waitPahoMessage(t, incoming3, 3*time.Second)

	// Concurrent Publish/Online/Disconnect must not deadlock (run under -race).
	var done sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		done.Add(1)
		go func(worker int) {
			defer done.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := b.Online(devA.DeviceID); !ok && worker == 0 {
					return
				}
				_ = b.Publish(devA.DeviceID, []byte("concurrent-downlink-probe"))
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	if err := b.Disconnect(devA.DeviceID, "test-admin"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	close(stop)
	done.Wait()

	ev := rec.waitOffline(t, 3*time.Second, func(e OfflineEvent) bool {
		return e.DeviceID == devA.DeviceID && e.Generation == gen3
	})
	if ev.Reason != "test-admin" {
		t.Fatalf("offline reason = %q, want %q", ev.Reason, "test-admin")
	}
	waitOffline(t, b, devA.DeviceID, 2*time.Second)
	if err := b.Disconnect(devA.DeviceID, "already-offline"); !errors.Is(err, ErrOffline) {
		t.Fatalf("Disconnect offline device = %v, want ErrOffline", err)
	}

	// The broker remains usable and generations keep increasing.
	a4, _ := connectDevice(t, addr, devA)
	gen4 := waitGeneration(t, b, devA.DeviceID, gen3+1)
	if gen4 != gen3+1 {
		t.Fatalf("generation after fourth connect = %d, want %d", gen4, gen3+1)
	}
	subscribe(t, a4, devA.SubscribeTopic, 1)
	publish(t, a4, devA.PublishTopic, []byte("uplink-gen4"), 0, false)
	rec.waitMessage(t, 3*time.Second, func(m Message) bool {
		return m.Generation == gen4 && string(m.Payload) == "uplink-gen4"
	})
}

// waitGeneration waits until deviceID reports the expected generation.
func waitGeneration(t *testing.T, b *Broker, deviceID string, want uint64) uint64 {
	t.Helper()
	var got uint64
	waitFor(t, 3*time.Second, "online generation", func() bool {
		g, ok := b.Online(deviceID)
		if !ok {
			return false
		}
		got = g
		return g == want
	})
	if got != want {
		t.Fatalf("generation = %d, want %d", got, want)
	}
	return got
}
