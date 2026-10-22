package playout_test

import (
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/playout"
)

// TestA16SettlePrebufferCadenceTail is the canonical A16 check: with a fake
// clock it verifies tts start -> cancellable settle -> first frame at/after
// settle -> 5-frame prebuffer gate -> one frame per frameDuration -> tail wait
// of (prebuffer+extra) frames -> tts stop.
func TestA16SettlePrebufferCadenceTail(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	// One frame short of the prebuffer target before the round starts.
	pushN(t, p, testPrebuffer-1)
	errCh := startRound(t, p, sink, markers)

	settleEv := ev.wait(t, 1, playout.EventSettleWait)
	if !settleEv.At.Equal(testBase) {
		t.Fatalf("settle wait registered at %s, want %s", settleEv.At, testBase)
	}
	if want := testBase.Add(testSettle); !settleEv.Deadline.Equal(want) {
		t.Fatalf("settle deadline %s, want %s", settleEv.Deadline, want)
	}
	if starts, _, startAt, _, _ := markers.snapshot(); starts != 1 || !startAt.Equal(testBase) {
		t.Fatalf("TTSStart must happen before the settle wait (starts=%d, at=%s)", starts, startAt)
	}

	// No frame may be sent before the settle deadline.
	fc.Advance(testSettle - time.Millisecond)
	if got := sink.count(); got != 0 {
		t.Fatalf("%d frame(s) sent before settle completed", got)
	}
	fc.Advance(time.Millisecond)

	// Settle is over but only 4 frames are queued: the 5-frame prebuffer gate
	// must still hold the first send.
	if got := sink.count(); got != 0 {
		t.Fatalf("%d frame(s) sent before the %d-frame prebuffer was ready", got, testPrebuffer)
	}
	if !p.Push(makeFrame(99)) {
		t.Fatal("Push of the 5th prebuffer frame was rejected")
	}

	first := ev.wait(t, 1, playout.EventFirstSend)
	if want := testBase.Add(testSettle); !first.At.Equal(want) {
		t.Fatalf("first send at %s, want %s (after settle)", first.At, want)
	}
	if first.Sent != 1 {
		t.Fatalf("first send reports Sent=%d, want 1", first.Sent)
	}

	// Pacing: one frame per frameDuration, measured from the previous send.
	lastSend := first.At
	for sent := 2; sent <= testPrebuffer; sent++ {
		pace := ev.wait(t, 1, playout.EventPacingWait)
		if want := lastSend.Add(testFrameDuration); !pace.Deadline.Equal(want) {
			t.Fatalf("pacing deadline for frame %d is %s, want %s", sent, pace.Deadline, want)
		}
		fc.Advance(testFrameDuration)
		frameEv := ev.wait(t, 1, playout.EventFrameSent)
		if !frameEv.At.Equal(pace.Deadline) {
			t.Fatalf("frame %d sent at %s, want %s", sent, frameEv.At, pace.Deadline)
		}
		if frameEv.Sent != sent {
			t.Fatalf("frame event reports Sent=%d, want %d", frameEv.Sent, sent)
		}
		lastSend = frameEv.At
	}
	if got := sink.count(); got != testPrebuffer {
		t.Fatalf("sink received %d frames, want %d", got, testPrebuffer)
	}

	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	tailEv := ev.wait(t, 1, playout.EventTailWait)
	wantTail := lastSend.Add(tailWaitDuration())
	if !tailEv.Deadline.Equal(wantTail) {
		t.Fatalf("tail deadline %s, want %s [(prebuffer+extra)*frameDuration after last send]", tailEv.Deadline, wantTail)
	}

	// TTSStop must not happen before the tail wait elapses.
	fc.Advance(tailWaitDuration() - time.Millisecond)
	if _, stops, _, _, _ := markers.snapshot(); stops != 0 {
		t.Fatal("TTSStop was called before the tail wait completed")
	}
	fc.Advance(time.Millisecond)
	ev.wait(t, 1, playout.EventTTSStop)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round ended with error: %v", err)
	}

	starts, stops, startAt, stopAt, order := markers.snapshot()
	if starts != 1 || stops != 1 {
		t.Fatalf("marker calls: %d starts / %d stops, want 1/1", starts, stops)
	}
	if !startAt.Equal(testBase) {
		t.Fatalf("TTSStart at %s, want %s", startAt, testBase)
	}
	if !stopAt.Equal(wantTail) {
		t.Fatalf("TTSStop at %s, want %s", stopAt, wantTail)
	}
	if len(order) != 2 || order[0] != "start" || order[1] != "stop" {
		t.Fatalf("marker order %v, want [start stop]", order)
	}
	if got := sink.count(); got != testPrebuffer {
		t.Fatalf("sink received %d frames after the round, want %d", got, testPrebuffer)
	}

	stats := p.Stats()
	if stats.Rounds != 1 || stats.Completed != 1 || stats.Sent != testPrebuffer {
		t.Fatalf("stats %+v, want 1 round / completed / %d sent", stats, testPrebuffer)
	}
	if stats.Dropped != 0 || stats.AbortedFrames != 0 || stats.QueueDepth != 0 {
		t.Fatalf("unexpected drops/queue after successful round: %+v", stats)
	}
	if pending := fc.Pending(); pending != 0 {
		t.Fatalf("%d fake-clock timer(s) left pending after the round", pending)
	}
}

// TestA16TailWaitUsesConfiguredFormula checks the tail is
// (PrebufferFrames+TailExtraFrames)*FrameDuration and configurable.
func TestA16TailWaitUsesConfiguredFormula(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	cfg := newConfig(fc, ev.observe)
	cfg.PrebufferFrames = 2
	cfg.TailExtraFrames = 1
	p := mustPlayer(t, cfg)
	wantTail := time.Duration(cfg.PrebufferFrames+cfg.TailExtraFrames) * testFrameDuration

	pushN(t, p, 2)
	errCh := startRound(t, p, sink, markers)
	settleEv := ev.wait(t, 1, playout.EventSettleWait)
	fc.Advance(settleEv.Deadline.Sub(fc.Now()))

	first := ev.wait(t, 1, playout.EventFirstSend)
	ev.wait(t, 1, playout.EventPacingWait)
	fc.Advance(testFrameDuration)
	last := ev.wait(t, 1, playout.EventFrameSent).At
	if last.Before(first.At) {
		t.Fatalf("last send %s before first send %s", last, first.At)
	}
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	tailEv := ev.wait(t, 1, playout.EventTailWait)
	if want := last.Add(wantTail); !tailEv.Deadline.Equal(want) {
		t.Fatalf("configured tail deadline %s, want %s", tailEv.Deadline, want)
	}
	fc.Advance(wantTail - time.Millisecond)
	if _, stops, _, _, _ := markers.snapshot(); stops != 0 {
		t.Fatal("TTSStop before configured tail wait elapsed")
	}
	fc.Advance(time.Millisecond)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round ended with error: %v", err)
	}
	if _, stops, _, stopAt, _ := markers.snapshot(); stops != 1 || !stopAt.Equal(last.Add(wantTail)) {
		t.Fatalf("TTSStop stops=%d at=%s, want 1 at %s", stops, stopAt, last.Add(wantTail))
	}
}

// TestA16NoBurstAfterSlowUpstream is the A16 "stalled upstream must not burst
// to catch up" check: after a long pause only one frame is sent immediately,
// the rest stay paced by frameDuration.
func TestA16NoBurstAfterSlowUpstream(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	errCh := startRound(t, p, sink, markers)
	settleEv := ev.wait(t, 1, playout.EventSettleWait)
	fc.Advance(settleEv.Deadline.Sub(fc.Now()))

	// Let the 5 prebuffered frames play out at the normal cadence.
	last := ev.wait(t, 1, playout.EventFirstSend).At
	for sent := 2; sent <= testPrebuffer; sent++ {
		ev.wait(t, 1, playout.EventPacingWait)
		fc.Advance(testFrameDuration)
		last = ev.wait(t, 1, playout.EventFrameSent).At
	}
	if got := sink.count(); got != testPrebuffer {
		t.Fatalf("sink received %d frames, want %d", got, testPrebuffer)
	}

	// Upstream stalls: the clock runs on with an empty queue.
	fc.Advance(2 * time.Second)
	if got := sink.count(); got != testPrebuffer {
		t.Fatalf("frames sent during upstream stall: %d, want %d", got, testPrebuffer)
	}

	// The upstream resumes with a burst of 8 frames at once.
	burst := 8
	pushN(t, p, burst)
	firstAfterGap := ev.wait(t, 1, playout.EventFrameSent)
	if got := sink.count(); got != testPrebuffer+1 {
		t.Fatalf("after the stall %d frames were sent immediately, want exactly 1 (no catch-up burst)", got-testPrebuffer)
	}
	if firstAfterGap.Sent != testPrebuffer+1 {
		t.Fatalf("resumed frame reports Sent=%d, want %d", firstAfterGap.Sent, testPrebuffer+1)
	}
	last = firstAfterGap.At

	// All remaining frames must be paced, not bursted.
	for sent := testPrebuffer + 2; sent <= testPrebuffer+burst; sent++ {
		pace := ev.wait(t, 1, playout.EventPacingWait)
		if want := last.Add(testFrameDuration); !pace.Deadline.Equal(want) {
			t.Fatalf("resumed pacing deadline %s, want %s", pace.Deadline, want)
		}
		fc.Advance(testFrameDuration)
		frameEv := ev.wait(t, 1, playout.EventFrameSent)
		if !frameEv.At.Equal(pace.Deadline) {
			t.Fatalf("resumed frame %d sent at %s, want %s", sent, frameEv.At, pace.Deadline)
		}
		last = frameEv.At
	}
	if got := sink.count(); got != testPrebuffer+burst {
		t.Fatalf("sink received %d frames, want %d", got, testPrebuffer+burst)
	}

	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	tailEv := ev.wait(t, 1, playout.EventTailWait)
	fc.Advance(tailEv.Deadline.Sub(fc.Now()))
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round ended with error: %v", err)
	}
	if stats := p.Stats(); stats.Sent != testPrebuffer+burst || stats.Dropped != 0 {
		t.Fatalf("stats %+v, want %d sent and no drops", stats, testPrebuffer+burst)
	}
}

// TestA16QueueBoundedAndDrops checks the bounded queue, drop accounting and
// that a full queue never grows past MaxQueue.
func TestA16QueueBoundedAndDrops(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	cfg := newConfig(fc, ev.observe)
	cfg.MaxQueue = 8
	cfg.PrebufferFrames = 8
	cfg.TailExtraFrames = testTailExtra
	p := mustPlayer(t, cfg)

	pushN(t, p, 8)
	for i := 0; i < 3; i++ {
		if p.Push(makeFrame(100 + i)) {
			t.Fatalf("Push %d should have been dropped by the 8-frame queue bound", i)
		}
	}
	if p.Push(nil) {
		t.Fatal("Push(nil) must be rejected")
	}
	if p.Push(make([]byte, playout.MaxFrameBytes+1)) {
		t.Fatal("Push of an oversized frame must be rejected")
	}
	stats := p.Stats()
	if stats.QueueDepth != 8 || stats.QueueHighWater != 8 {
		t.Fatalf("queue depth/high-water %d/%d, want 8/8", stats.QueueDepth, stats.QueueHighWater)
	}
	if stats.Dropped != 5 {
		t.Fatalf("Dropped=%d, want 5", stats.Dropped)
	}
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	if p.FinishInput() {
		t.Fatal("second FinishInput should report false")
	}

	errCh := startRound(t, p, sink, markers)
	settleEv := ev.wait(t, 1, playout.EventSettleWait)
	fc.Advance(settleEv.Deadline.Sub(fc.Now()))
	last := ev.wait(t, 1, playout.EventFirstSend).At
	for sent := 2; sent <= 8; sent++ {
		ev.wait(t, 1, playout.EventPacingWait)
		fc.Advance(testFrameDuration)
		last = ev.wait(t, 1, playout.EventFrameSent).At
	}
	tailEv := ev.wait(t, 1, playout.EventTailWait)
	// The configured PrebufferFrames contributes to the tail formula even
	// though only 8 frames were queued.
	wantTail := time.Duration(cfg.PrebufferFrames+cfg.TailExtraFrames) * testFrameDuration
	if want := last.Add(wantTail); !tailEv.Deadline.Equal(want) {
		t.Fatalf("tail deadline %s, want %s", tailEv.Deadline, want)
	}
	fc.Advance(tailEv.Deadline.Sub(fc.Now()))
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round ended with error: %v", err)
	}
	stats = p.Stats()
	if stats.Sent != 8 || stats.Completed != 1 || stats.QueueDepth != 0 {
		t.Fatalf("stats after round %+v, want 8 sent / 1 completed / empty queue", stats)
	}
	if stats.QueueHighWater != 8 {
		t.Fatalf("QueueHighWater=%d after round, want 8", stats.QueueHighWater)
	}
}
