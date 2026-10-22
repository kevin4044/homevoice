package acceptance_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/logging"
	"github.com/homevoice/homevoice/internal/m0/simulator"
	"github.com/homevoice/homevoice/internal/m0/udp"
)

// TestA05ProbeTakeoverAcceptsNewGeneration is the probe-level regression for
// duplicate-client-id takeover: the broker bumps the generation and emits
// OnOnline; the probe must drop the old audio session without clearing the new
// MQTT registration, accept the new HELLO and reject old-SSRC frames.
func TestA05ProbeTakeoverAcceptsNewGeneration(t *testing.T) {
	h := newHarness(t, nil, nil)
	first := h.connect("dev-a")
	firstHello := h.hello("dev-a")
	firstSession, ok := h.probe.SessionSnapshot("dev-a")
	if !ok {
		t.Fatal("no first session")
	}
	firstGen, online := h.probe.Online("dev-a")
	if !online {
		t.Fatal("first device not online")
	}

	// Second connection with the same client_id/credentials while the first is
	// still open: broker takeover, not a clean disconnect.
	devCfg, ok := h.cfg.DeviceByID("dev-a")
	if !ok {
		t.Fatal("missing dev-a config")
	}
	second, err := simulator.NewDevice(devCfg, simulator.DeviceOptions{
		Logger:    logging.Discard(),
		MCPServer: simulator.DefaultMCPServer(),
	})
	if err != nil {
		t.Fatalf("second device: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := second.Connect(ctx, h.probe.BrokerURL()); err != nil {
		t.Fatalf("second connect: %v", err)
	}

	// The probe must follow the broker's generation bump before HELLO.
	waitFor(t, "takeover generation", 5*time.Second, func() bool {
		gen, ok := h.probe.Online("dev-a")
		return ok && gen > firstGen
	})
	waitFor(t, "old audio session dropped on takeover", 5*time.Second, func() bool {
		_, ok := h.probe.SessionSnapshot("dev-a")
		return !ok
	})

	// The new generation's HELLO must succeed and rotate the UDP session.
	resp, err := second.SendHello(ctx, "udp", 3)
	if err != nil {
		t.Fatalf("new-generation HELLO after takeover: %v", err)
	}
	h.waitSession("dev-a")
	newSession, ok := h.probe.SessionSnapshot("dev-a")
	if !ok {
		t.Fatal("no session after new HELLO")
	}
	if newSession.SessionID == firstSession.SessionID || resp.SessionID == firstSession.SessionID {
		t.Fatalf("session was not rotated: old=%q new=%q resp=%q", firstSession.SessionID, newSession.SessionID, resp.SessionID)
	}
	if newSession.SSRC == firstSession.SSRC {
		t.Fatal("SSRC was not rotated")
	}
	if gen, ok := h.probe.Online("dev-a"); !ok || gen <= firstGen {
		t.Fatalf("new broker registration lost (gen=%d ok=%v)", gen, ok)
	}

	// Old SSRC/keys must not be accepted by the new session.
	keyRaw, err := hex.DecodeString(firstHello.UDP.Key)
	if err != nil || len(keyRaw) != 16 {
		t.Fatalf("first hello key: %v", err)
	}
	var key [16]byte
	copy(key[:], keyRaw)
	nonceRaw, err := hex.DecodeString(firstHello.UDP.Nonce)
	if err != nil {
		t.Fatalf("first hello nonce: %v", err)
	}
	tpl, err := udp.ParseHeader(nonceRaw)
	if err != nil {
		t.Fatalf("first hello template: %v", err)
	}
	oldPacket, err := udp.Encrypt(key, udp.Header{
		PacketType: tpl.PacketType,
		Flags:      tpl.Flags,
		SSRC:       tpl.SSRC,
		Timestamp:  60,
		Sequence:   99,
	}, []byte("old-generation-frame"))
	if err != nil {
		t.Fatalf("encrypt old frame: %v", err)
	}
	before, _, _ := h.probe.UDPManagerCounters()
	if err := first.SendRawUDP(oldPacket); err != nil {
		t.Fatalf("send old frame: %v", err)
	}
	waitFor(t, "old SSRC rejected", 3*time.Second, func() bool {
		after, _, _ := h.probe.UDPManagerCounters()
		return after > before
	})
}
