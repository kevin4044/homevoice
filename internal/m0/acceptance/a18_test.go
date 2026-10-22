package acceptance_test

import (
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/probe"
)

// TestA18AbortKeepsSessionGoodbyeKeepsMQTTDisconnectClearsBoth covers the two
// MQTT/UDP lifecycles: playback abort preserves MQTT and a valid UDP session,
// goodbye releases audio only, and disconnect clears both.
func TestA18AbortKeepsSessionGoodbyeKeepsMQTTDisconnectClearsBoth(t *testing.T) {
	h := newHarness(t, nil, nil)
	dev := h.connect("dev-a")
	h.hello("dev-a")
	first, _ := h.probe.SessionSnapshot("dev-a")
	if first.SessionID == "" {
		t.Fatal("no session id after HELLO")
	}

	dev.SendListen("start", "manual")
	waitFor(t, "listening", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.Listening
	})
	pcm := audio.Sine(audio.UplinkParams().SampleRate, 440, 0.2, 2*audio.UplinkParams().FrameSamples())
	if err := dev.SendPCM(pcm); err != nil {
		t.Fatalf("SendPCM: %v", err)
	}
	waitFor(t, "uplink audio", 2*time.Second, func() bool {
		return h.probe.Stats().UplinkFrames >= 2
	})

	// Abort during listening ends the round (reference behaviour) and keeps the
	// valid UDP session + MQTT connection.
	if err := dev.SendAbort("wake_word_detected"); err != nil {
		t.Fatalf("SendAbort: %v", err)
	}
	waitFor(t, "abort stops listening", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return !s.Listening
	})
	afterAbort, ok := h.probe.SessionSnapshot("dev-a")
	if !ok || afterAbort.SessionID != first.SessionID {
		t.Fatalf("abort released/rotated the audio session: %+v ok=%v", afterAbort, ok)
	}
	if _, online := h.probe.Online("dev-a"); !online {
		t.Fatal("abort dropped MQTT online state")
	}

	// A later listen round still works on the same session.
	dev.SendListen("start", "manual")
	waitFor(t, "second listen", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.Listening
	})
	dev.SendListen("stop", "")
	waitFor(t, "second listen stop", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return !s.Listening
	})
	if _, ok := h.probe.SessionSnapshot("dev-a"); !ok {
		t.Fatal("listen stop released the audio session")
	}

	// goodbye releases the audio session but not MQTT.
	dev.SendGoodbye()
	h.waitNoSession("dev-a")
	if _, online := h.probe.Online("dev-a"); !online {
		t.Fatal("goodbye dropped MQTT online state")
	}
	if s, _ := h.probe.Snapshot("dev-a"); !s.Online || s.SessionID != "" {
		t.Fatalf("snapshot after goodbye = %+v", s)
	}

	// HELLO can create a new UDP session while MQTT stays up.
	h.hello("dev-a")
	if _, ok := h.probe.SessionSnapshot("dev-a"); !ok {
		t.Fatal("re-HELLO did not create a session")
	}

	// MQTT disconnect clears the audio session and the online registration.
	dev.Close()
	h.waitOffline("dev-a")
	h.waitNoSession("dev-a")
	if s, _ := h.probe.Snapshot("dev-a"); s.Online || s.Busy || s.SessionID != "" {
		t.Fatalf("snapshot after disconnect = %+v", s)
	}
	if stats := h.probe.Stats(); stats.ActiveSessions != 0 || stats.PendingTasks != 0 || stats.BusyDevices != 0 {
		t.Fatalf("residual tasks after disconnect: %+v", stats)
	}
}

// TestIntegratedListenTTSPlayback exercises the offline end-to-end path:
// listen -> simulated upstream -> MQTT tts markers -> paced UDP frames.
func TestIntegratedListenTTSPlayback(t *testing.T) {
	h := newHarness(t, nil, func(o *probe.Options) {
		up, err := newSyntheticUpstream(3)
		if err != nil {
			t.Fatalf("synthetic upstream: %v", err)
		}
		o.Upstream = up
	})
	dev := h.connect("dev-a")
	h.hello("dev-a")
	dev.SendListen("start", "manual")
	waitFor(t, "listening", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.Listening
	})
	pcm := audio.Sine(audio.UplinkParams().SampleRate, 440, 0.2, 2*audio.UplinkParams().FrameSamples())
	if err := dev.SendPCM(pcm); err != nil {
		t.Fatalf("SendPCM: %v", err)
	}
	dev.SendListen("stop", "")

	waitFor(t, "tts start + udp audio + tts stop", 5*time.Second, func() bool {
		var starts, stops int
		for _, m := range dev.Messages() {
			if m.Type == "tts" && m.TTS != nil {
				if m.TTS.State == "start" {
					starts++
				}
				if m.TTS.State == "stop" {
					stops++
				}
			}
		}
		return starts == 1 && stops == 1 && len(dev.AudioFrames()) > 0
	})
	if len(dev.Messages()) == 0 {
		t.Fatal("device received no downlink markers")
	}
	// Marker order: tts start must arrive before tts stop.
	firstStart, firstStop := -1, -1
	for i, m := range dev.Messages() {
		if m.TTS == nil {
			continue
		}
		if m.TTS.State == "start" && firstStart < 0 {
			firstStart = i
		}
		if m.TTS.State == "stop" && firstStop < 0 {
			firstStop = i
		}
	}
	if firstStart < 0 || firstStop < 0 || firstStart > firstStop {
		t.Fatalf("tts marker order start=%d stop=%d", firstStart, firstStop)
	}
	if _, ok := h.probe.SessionSnapshot("dev-a"); !ok {
		t.Fatal("playback released the audio session")
	}
	if stats, _ := h.probe.PlayerStats("dev-a"); stats.Rounds != 1 || stats.Completed != 1 {
		t.Fatalf("player stats = %+v", stats)
	}
	if frames := dev.AudioFrames(); len(frames) == 0 {
		t.Fatal("no downlink UDP audio frames decrypted by the simulated device")
	}
}
