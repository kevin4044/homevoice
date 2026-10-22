package acceptance_test

import (
	"context"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/probe"
)

// TestA19ProbeQueueAndTaskClosure covers the probe-level parts of A19: the
// outbound audio queue stays <=64 with counted overflow, and 100
// HELLO/abort/goodbye/reconnect rounds leave no session, pending task or busy
// device behind.
func TestA19ProbeQueueAndTaskClosure(t *testing.T) {
	h := newHarness(t, nil, func(o *probe.Options) {
		o.Upstream = &fixedUpstream{frames: synthFrames(100)}
	})
	ctx := context.Background()
	dev := h.connect("dev-a")
	h.hello("dev-a")

	// Bound-check the playback queue with more frames than the 64-frame limit.
	dev.SendListen("start", "manual")
	waitFor(t, "listening", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.Listening
	})
	pcm := audio.Sine(audio.UplinkParams().SampleRate, 330, 0.2, 2*audio.UplinkParams().FrameSamples())
	if err := dev.SendPCM(pcm); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "uplink audio", 2*time.Second, func() bool { return h.probe.Stats().UplinkFrames >= 2 })
	dev.SendListen("stop", "")
	waitFor(t, "playback round", 5*time.Second, func() bool {
		st, _ := h.probe.PlayerStats("dev-a")
		return st.Completed >= 1
	})
	ps, _ := h.probe.PlayerStats("dev-a")
	if ps.QueueHighWater > limits.AudioQueueFrames {
		t.Fatalf("queue high water %d > %d", ps.QueueHighWater, limits.AudioQueueFrames)
	}
	if ps.Sent > limits.AudioQueueFrames {
		t.Fatalf("sent %d frames, queue limit %d", ps.Sent, limits.AudioQueueFrames)
	}
	if ps.Dropped < 100-limits.AudioQueueFrames {
		t.Fatalf("overflow not counted: dropped=%d", ps.Dropped)
	}

	// 100 reported rounds: HELLO -> abort -> goodbye. Reconnect MQTT every 25
	// rounds to exercise connection generations as well.
	sendHello := func() {
		t.Helper()
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := dev.SendHello(hctx, "udp", 3); err != nil {
			t.Fatalf("hello: %v", err)
		}
		h.waitSession("dev-a")
	}
	for i := 0; i < 100; i++ {
		sendHello()
		if err := dev.SendAbort("cycle"); err != nil {
			t.Fatalf("round %d abort: %v", i, err)
		}
		if err := dev.SendGoodbye(); err != nil {
			t.Fatalf("round %d goodbye: %v", i, err)
		}
		h.waitNoSession("dev-a")
		if i%25 == 24 {
			_ = dev.Close()
			delete(h.devices, "dev-a")
			dev = h.connect("dev-a")
		}
	}
	waitFor(t, "zero residual tasks", 5*time.Second, func() bool {
		s := h.probe.Stats()
		return s.ActiveSessions == 0 && s.PendingTasks == 0 && s.BusyDevices == 0
	})
	final := h.probe.Stats()
	if final.ActiveSessions != 0 || final.PendingTasks != 0 || final.BusyDevices != 0 {
		t.Fatalf("residual state after 100 rounds: %+v", final)
	}
	if err := h.probe.Close(); err != nil {
		t.Fatalf("probe.Close: %v", err)
	}
	after := h.probe.Stats()
	if after.ActiveSessions != 0 || after.PendingTasks != 0 {
		t.Fatalf("state after Close: %+v", after)
	}
	t.Logf("A19 probe: queueHighWater=%d sent=%d dropped=%d rounds=%d", ps.QueueHighWater, ps.Sent, ps.Dropped, ps.Rounds)
}
