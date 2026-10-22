package acceptance_test

import (
	"testing"
	"time"
)

// TestA18GoodbyeMismatchPreservesSession reproduces Defect 3: a goodbye with a
// wrong session_id must not clear the probe's session mapping or orphan the UDP
// session. The correct goodbye still releases it.
func TestA18GoodbyeMismatchPreservesSession(t *testing.T) {
	h := newHarness(t, nil, nil)
	dev := h.connect("dev-a")
	h.hello("dev-a")
	before, ok := h.probe.SessionSnapshot("dev-a")
	if !ok {
		t.Fatal("no session after HELLO")
	}

	if err := dev.PublishRaw([]byte(`{"type":"goodbye","session_id":"synthetic-wrong-session"}`)); err != nil {
		t.Fatalf("publish mismatched goodbye: %v", err)
	}
	waitFor(t, "goodbye rejection recorded", 3*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.LastError != ""
	})
	after, ok := h.probe.SessionSnapshot("dev-a")
	if !ok {
		t.Fatal("mismatched goodbye released the UDP session")
	}
	if after.SessionID != before.SessionID {
		t.Fatalf("probe session mapping changed: before=%q after=%q", before.SessionID, after.SessionID)
	}
	if snap, _ := h.probe.Snapshot("dev-a"); snap.SessionID != before.SessionID {
		t.Fatalf("probe Snapshot.SessionID = %q, want %q", snap.SessionID, before.SessionID)
	}

	// The session is still functional: a valid device frame is accepted by the
	// session manager (the probe is not listening, so it is counted as dropped
	// uplink audio, but the UDP session itself must not be orphaned).
	if err := dev.SendOpusFrame([]byte("still-live-frame")); err != nil {
		t.Fatalf("send after mismatched goodbye: %v", err)
	}
	waitFor(t, "session accepts frame after rejected goodbye", 3*time.Second, func() bool {
		s, ok := h.probe.SessionSnapshot("dev-a")
		return ok && s.Counters.Accepted > after.Counters.Accepted
	})

	// The matching goodbye releases the UDP session and keeps MQTT online.
	if err := dev.SendGoodbye(); err != nil {
		t.Fatalf("matching goodbye: %v", err)
	}
	h.waitNoSession("dev-a")
	if _, online := h.probe.Online("dev-a"); !online {
		t.Fatal("matching goodbye dropped MQTT online state")
	}
}
