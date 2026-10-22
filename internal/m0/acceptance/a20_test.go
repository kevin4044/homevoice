package acceptance_test

import (
	"errors"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/notify"
)

// TestA20IdleNotifyAcrossLifecycle covers idle notify end to end: MQTT online
// with no UDP session can notify, busy/offline fail explicitly, the other
// device does not receive it, and "published" is never reported as "played".
func TestA20IdleNotifyAcrossLifecycle(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.connect("dev-a")
	b := h.connect("dev-b")
	h.hello("dev-a")
	h.hello("dev-b")
	_ = b

	// Release A's UDP session with goodbye; MQTT stays online.
	a.SendGoodbye()
	h.waitNoSession("dev-a")
	if _, online := h.probe.Online("dev-a"); !online {
		t.Fatal("MQTT went offline after goodbye")
	}
	res, err := h.probe.Notify("dev-a")
	if err != nil {
		t.Fatalf("idle notify: %v", err)
	}
	if res.Status != notify.StatusPublished {
		t.Fatalf("notify status = %s", res.Status)
	}
	if res.Played {
		t.Fatal("notify claimed playback confirmation")
	}
	waitFor(t, "device notify", 3*time.Second, func() bool {
		return len(a.Notifications()) == 1
	})
	n := mustParseNotify(t, a.Messages()[len(a.Messages())-1].Payload)
	if n.Text == "" || n.AudioURL == "" {
		t.Fatalf("notify payload = %+v", n)
	}
	if got := len(b.Notifications()); got != 0 {
		t.Fatalf("other device received %d notifications", got)
	}

	// Busy device: explicit rejection, no forced interruption or queueing.
	h.hello("dev-a")
	a.SendListen("start", "manual")
	waitFor(t, "dev-a listening", 3*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.Listening
	})
	if _, err := h.probe.Notify("dev-a"); !errors.Is(err, notify.ErrBusy) {
		t.Fatalf("busy notify error = %v, want ErrBusy", err)
	}
	if got := len(a.Notifications()); got != 1 {
		t.Fatalf("busy notify changed notification count to %d", got)
	}
	a.SendAbort("test")
	waitFor(t, "dev-a not listening", 3*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return !s.Listening
	})

	// Offline device: explicit failure.
	a.Close()
	h.waitOffline("dev-a")
	// Disconnect cleanup is generation-aware and runs on the broker cleanup
	// path; wait for the audio session to be released before asserting.
	h.waitNoSession("dev-a")
	if _, err := h.probe.Notify("dev-a"); !errors.Is(err, notify.ErrOffline) {
		t.Fatalf("offline notify error = %v, want ErrOffline", err)
	}
	if _, ok := h.probe.SessionSnapshot("dev-a"); ok {
		t.Fatal("offline device still has a UDP session")
	}

	// Bound: no unbounded notification queue was created.
	if stats := h.probe.Stats(); stats.BusyDevices != 0 {
		t.Fatalf("busy devices after abort/disconnect = %d", stats.BusyDevices)
	}
}
