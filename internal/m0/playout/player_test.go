package playout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/playout"
)

func TestNewValidatesConfig(t *testing.T) {
	fc := clock.NewFake(testBase)
	if _, err := playout.New(playout.Config{Clock: fc, FrameDuration: testFrameDuration}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name string
		cfg  playout.Config
	}{
		{"missing clock", playout.Config{FrameDuration: testFrameDuration}},
		{"zero frame duration", playout.Config{Clock: fc}},
		{"negative frame duration", playout.Config{Clock: fc, FrameDuration: -testFrameDuration}},
		{"negative settle", playout.Config{Clock: fc, FrameDuration: testFrameDuration, Settle: -1}},
		{"negative prebuffer", playout.Config{Clock: fc, FrameDuration: testFrameDuration, PrebufferFrames: -1}},
		{"negative tail extra", playout.Config{Clock: fc, FrameDuration: testFrameDuration, TailExtraFrames: -1}},
		{"negative max queue", playout.Config{Clock: fc, FrameDuration: testFrameDuration, MaxQueue: -1}},
		{"queue above probe bound", playout.Config{Clock: fc, FrameDuration: testFrameDuration, MaxQueue: limits.AudioQueueFrames + 1}},
		{"prebuffer above queue", playout.Config{Clock: fc, FrameDuration: testFrameDuration, MaxQueue: 8, PrebufferFrames: 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := playout.New(tc.cfg); !errors.Is(err, playout.ErrInvalidConfig) {
				t.Fatalf("New(%+v) error=%v, want ErrInvalidConfig", tc.cfg, err)
			}
		})
	}
}

func TestZeroValuePlayerGuards(t *testing.T) {
	sink := &recordingSink{}
	var p playout.Player
	if p.Push(makeFrame(1)) {
		t.Fatal("zero-value Push should return false")
	}
	if p.FinishInput() {
		t.Fatal("zero-value FinishInput should return false")
	}
	if err := p.StartRound(context.Background(), sink, nil); !errors.Is(err, playout.ErrNotInitialized) {
		t.Fatalf("zero-value StartRound error=%v, want ErrNotInitialized", err)
	}
	if stats := p.Stats(); stats != (playout.Stats{}) {
		t.Fatalf("zero-value Stats=%+v, want zero", stats)
	}
	p.Abort() // must be a no-op, not a panic

	var nilPlayer *playout.Player
	if nilPlayer.Push(makeFrame(1)) {
		t.Fatal("nil Push should return false")
	}
	if err := nilPlayer.StartRound(context.Background(), sink, nil); !errors.Is(err, playout.ErrNotInitialized) {
		t.Fatalf("nil StartRound error=%v, want ErrNotInitialized", err)
	}
	if stats := nilPlayer.Stats(); stats != (playout.Stats{}) {
		t.Fatalf("nil Stats=%+v, want zero", stats)
	}
	nilPlayer.Abort()
}

func TestStartRoundRejectsNilSink(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	if err := p.StartRound(context.Background(), nil, nil); !errors.Is(err, playout.ErrNilSink) {
		t.Fatalf("StartRound(nil sink) error=%v, want ErrNilSink", err)
	}
	// A rejected start must not leave the player active.
	pushN(t, p, 2)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, nil)
	drivePreloadedRoundProduced(t, p, fc, ev, sink, nil, 1, 2)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round after nil-sink rejection: %v", err)
	}
}

func TestRoundActiveRejected(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, nil)
	ev.wait(t, 1, playout.EventSettleWait)
	if err := p.StartRound(context.Background(), sink, nil); !errors.Is(err, playout.ErrRoundActive) {
		t.Fatalf("concurrent StartRound error=%v, want ErrRoundActive", err)
	}
	p.Abort()
	if err := waitRound(t, errCh); !errors.Is(err, playout.ErrAborted) {
		t.Fatalf("aborted round error=%v, want ErrAborted", err)
	}
}

func TestSinkErrorEndsRound(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sentinel := errors.New("synthetic sink failure")
	sink := &recordingSink{failAt: 2, err: sentinel}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, markers)
	settleEv := ev.wait(t, 1, playout.EventSettleWait)
	fc.Advance(settleEv.Deadline.Sub(fc.Now()))
	ev.wait(t, 1, playout.EventFirstSend)
	ev.wait(t, 1, playout.EventPacingWait)
	fc.Advance(testFrameDuration)
	err := waitRound(t, errCh)
	if !errors.Is(err, sentinel) {
		t.Fatalf("round error=%v, want wrapped sink error", err)
	}
	starts, stops, _, _, _ := markers.snapshot()
	if starts != 1 || stops != 1 {
		t.Fatalf("markers=%d/%d after sink error, want 1/1 (TTSStop best effort)", starts, stops)
	}
	stats := p.Stats()
	if stats.SendErrors != 1 || stats.Sent != 1 || stats.Completed != 0 {
		t.Fatalf("stats after sink error %+v", stats)
	}
	if stats.AbortedFrames != testPrebuffer-2 {
		t.Fatalf("AbortedFrames=%d, want %d", stats.AbortedFrames, testPrebuffer-2)
	}
}

func TestMarkerStartErrorFailsRoundWithoutAudio(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	sentinel := errors.New("synthetic start marker failure")
	markers := &markerRecorder{clk: fc, failStart: sentinel}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, markers)
	err := waitRound(t, errCh)
	if !errors.Is(err, sentinel) {
		t.Fatalf("round error=%v, want wrapped TTSStart error", err)
	}
	if got := sink.count(); got != 0 {
		t.Fatalf("%d audio frame(s) sent after TTSStart failure", got)
	}
	starts, stops, _, _, _ := markers.snapshot()
	if starts != 1 || stops != 0 {
		t.Fatalf("markers=%d/%d after TTSStart failure, want 1/0", starts, stops)
	}
	stats := p.Stats()
	if stats.MarkerErrors != 1 || stats.Completed != 0 || stats.Sent != 0 {
		t.Fatalf("stats after TTSStart failure %+v", stats)
	}
}

func TestMarkerStopErrorIsReported(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	sentinel := errors.New("synthetic stop marker failure")
	markers := &markerRecorder{clk: fc, failStop: sentinel}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, markers)
	settleEv := ev.wait(t, 1, playout.EventSettleWait)
	fc.Advance(settleEv.Deadline.Sub(fc.Now()))
	ev.wait(t, 1, playout.EventFirstSend)
	for sent := 2; sent <= testPrebuffer; sent++ {
		ev.wait(t, 1, playout.EventPacingWait)
		fc.Advance(testFrameDuration)
		ev.wait(t, 1, playout.EventFrameSent)
	}
	tailEv := ev.wait(t, 1, playout.EventTailWait)
	fc.Advance(tailEv.Deadline.Sub(fc.Now()))
	if err := waitRound(t, errCh); !errors.Is(err, sentinel) {
		t.Fatalf("round error=%v, want wrapped TTSStop error", err)
	}
	if got := sink.count(); got != testPrebuffer {
		t.Fatalf("sink received %d frames, want %d (audio completed before stop failed)", got, testPrebuffer)
	}
	starts, stops, _, _, _ := markers.snapshot()
	if starts != 1 || stops != 1 {
		t.Fatalf("markers=%d/%d, want 1/1", starts, stops)
	}
	stats := p.Stats()
	if stats.MarkerErrors != 1 || stats.Completed != 0 {
		t.Fatalf("stats after TTSStop failure %+v", stats)
	}
}

func TestPushCopiesFrameData(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	original := []byte{0x11, 0x22, 0x33, 0x44}
	if !p.Push(original) {
		t.Fatal("Push was rejected")
	}
	original[0] = 0xff // caller reuses its buffer; queued frame must not change
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, nil)
	drivePreloadedRoundProduced(t, p, fc, ev, sink, nil, 1, 1)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round error: %v", err)
	}
	frames := sink.snapshot()
	if len(frames) != 1 {
		t.Fatalf("sink received %d frames, want 1", len(frames))
	}
	if len(frames[0]) != 4 || frames[0][0] != 0x11 {
		t.Fatalf("queued frame data changed after the caller reused its buffer: %x", frames[0])
	}
}

func TestPushRejectsInvalidFrames(t *testing.T) {
	fc := clock.NewFake(testBase)
	p := mustPlayer(t, newConfig(fc, nil))

	if p.Push(nil) || p.Push([]byte{}) {
		t.Fatal("empty frames must be rejected")
	}
	if p.Push(make([]byte, playout.MaxFrameBytes+1)) {
		t.Fatal("oversized frames must be rejected")
	}
	if !p.Push(make([]byte, playout.MaxFrameBytes)) {
		t.Fatal("a frame at exactly MaxFrameBytes must be accepted")
	}
	stats := p.Stats()
	if stats.Dropped != 3 || stats.QueueDepth != 1 {
		t.Fatalf("stats after invalid pushes %+v", stats)
	}
}
