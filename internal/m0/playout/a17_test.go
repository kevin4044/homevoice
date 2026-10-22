package playout_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/playout"
)

// TestA17AbortStopsSendsAndRoundReusable is the canonical A17 check: after
// Abort returns, the round returns promptly, no new audio frame starts (even
// when the clock advances), TTSStop is still attempted, and a later round
// works.
func TestA17AbortStopsSendsAndRoundReusable(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
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
	ev.wait(t, 1, playout.EventFrameSent)
	if got := sink.count(); got != 2 {
		t.Fatalf("sink received %d frames before abort, want 2", got)
	}

	p.Abort()
	if ev.wait(t, 1, playout.EventAborted).Kind != playout.EventAborted {
		t.Fatal("unreachable")
	}
	err := waitRound(t, errCh)
	if !errors.Is(err, playout.ErrAborted) {
		t.Fatalf("StartRound error=%v, want ErrAborted", err)
	}

	// The A17 guarantee: no new sends after Abort returned, even if the clock
	// keeps running.
	after := sink.count()
	fc.Advance(5 * time.Second)
	if got := sink.count(); got != after {
		t.Fatalf("%d frame(s) sent after Abort returned", got-after)
	}

	starts, stops, _, _, order := markers.snapshot()
	if starts != 1 || stops != 1 {
		t.Fatalf("markers after abort: %d starts / %d stops, want 1/1 (TTSStop best effort)", starts, stops)
	}
	if len(order) != 2 || order[0] != "start" || order[1] != "stop" {
		t.Fatalf("marker order %v, want [start stop]", order)
	}
	stats := p.Stats()
	if stats.AbortedRounds != 1 || stats.Completed != 0 || stats.Sent != 2 {
		t.Fatalf("stats after abort %+v, want 1 aborted / 0 completed / 2 sent", stats)
	}
	if stats.AbortedFrames != 3 {
		t.Fatalf("AbortedFrames=%d, want 3 queued frames discarded", stats.AbortedFrames)
	}

	// Reuse: preload a full round and run it to completion.
	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh = startRound(t, p, sink, markers)
	drivePreloadedRoundProduced(t, p, fc, ev, sink, markers, 2, testPrebuffer)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("second round ended with error: %v", err)
	}
	stats = p.Stats()
	if stats.Rounds != 2 || stats.Completed != 1 || stats.Sent != 2+testPrebuffer {
		t.Fatalf("stats after reuse %+v, want 2 rounds / 1 completed / %d sent", stats, 2+testPrebuffer)
	}
	if stats.AbortedRounds != 1 {
		t.Fatalf("AbortedRounds=%d after reuse, want 1", stats.AbortedRounds)
	}
}

// drivePreloadedRoundProduced drives the events of an already-started round
// whose input was preloaded and finished, and returns the sink's frame count.
func drivePreloadedRoundProduced(t *testing.T, p *playout.Player, fc *clock.Fake, ev *eventCollector, sink *recordingSink, markers playout.Markers, round uint64, frames int) (int, time.Time) {
	t.Helper()
	settleEv := ev.wait(t, round, playout.EventSettleWait)
	fc.Advance(settleEv.Deadline.Sub(fc.Now()))
	last := ev.wait(t, round, playout.EventFirstSend).At
	for sent := 2; sent <= frames; sent++ {
		ev.wait(t, round, playout.EventPacingWait)
		fc.Advance(testFrameDuration)
		last = ev.wait(t, round, playout.EventFrameSent).At
	}
	tailEv := ev.wait(t, round, playout.EventTailWait)
	if want := last.Add(tailWaitDuration()); !tailEv.Deadline.Equal(want) {
		t.Fatalf("tail deadline %s, want %s", tailEv.Deadline, want)
	}
	fc.Advance(tailEv.Deadline.Sub(fc.Now()))
	if markers != nil {
		ev.wait(t, round, playout.EventTTSStop)
	}
	return sink.count(), last
}

// TestA17AbortDuringSettle checks a settle-phase cancellation: no audio is sent
// at all and the player is reusable.
func TestA17AbortDuringSettle(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, markers)
	ev.wait(t, 1, playout.EventSettleWait)

	p.Abort()
	ev.wait(t, 1, playout.EventAborted)
	if err := waitRound(t, errCh); !errors.Is(err, playout.ErrAborted) {
		t.Fatalf("StartRound error=%v, want ErrAborted", err)
	}

	fc.Advance(testSettle)
	if got := sink.count(); got != 0 {
		t.Fatalf("%d frame(s) sent after abort during settle", got)
	}
	if starts, stops, _, _, _ := markers.snapshot(); starts != 1 || stops != 1 {
		t.Fatalf("markers=%d/%d after settle abort, want 1/1", starts, stops)
	}
	stats := p.Stats()
	if stats.AbortedRounds != 1 || stats.AbortedFrames != testPrebuffer || stats.Sent != 0 {
		t.Fatalf("stats after settle abort %+v", stats)
	}

	// Reusable after a settle-phase abort.
	pushN(t, p, 2)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh = startRound(t, p, sink, markers)
	drivePreloadedRoundProduced(t, p, fc, ev, sink, markers, 2, 2)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("second round after settle abort: %v", err)
	}
}

// TestA17ContextCancellation checks that cancelling StartRound's context stops
// the round with the context error, still attempts TTSStop and releases the
// player for the next round.
func TestA17ContextCancellation(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	pushN(t, p, testPrebuffer)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var errCh <-chan error
	ctxErrCh := make(chan error, 1)
	errCh = ctxErrCh
	go func() { ctxErrCh <- p.StartRound(ctx, sink, markers) }()
	ev.wait(t, 1, playout.EventSettleWait)
	cancel()
	err := waitRound(t, errCh)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartRound error=%v, want context.Canceled", err)
	}
	fc.Advance(testSettle)
	if got := sink.count(); got != 0 {
		t.Fatalf("%d frame(s) sent after context cancellation", got)
	}
	if starts, stops, _, _, _ := markers.snapshot(); starts != 1 || stops != 1 {
		t.Fatalf("markers=%d/%d after context cancellation, want 1/1", starts, stops)
	}
	if stats := p.Stats(); stats.AbortedRounds != 0 || stats.AbortedFrames != testPrebuffer {
		t.Fatalf("stats after context cancellation %+v", stats)
	}

	// Reusable.
	pushN(t, p, 2)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh = startRound(t, p, sink, markers)
	drivePreloadedRoundProduced(t, p, fc, ev, sink, markers, 2, 2)
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round after context cancellation: %v", err)
	}
}

// TestA17RepeatedRoundsNoLeak runs 100 fake-clock rounds and requires stable
// goroutine count, no leftover fake timers and an empty queue.
func TestA17RepeatedRoundsNoLeak(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	runtime.GC()
	baseline := runtime.NumGoroutine()

	const rounds = 100
	for round := 0; round < rounds; round++ {
		pushN(t, p, testPrebuffer)
		if !p.FinishInput() {
			t.Fatalf("round %d: FinishInput returned false", round)
		}
		errCh := startRound(t, p, sink, markers)
		_, _ = drivePreloadedRoundProduced(t, p, fc, ev, sink, markers, uint64(round+1), testPrebuffer)
		if err := waitRound(t, errCh); err != nil {
			t.Fatalf("round %d ended with error: %v", round, err)
		}
		if pending := fc.Pending(); pending != 0 {
			t.Fatalf("round %d left %d fake-clock timer(s) pending", round, pending)
		}
	}

	stats := p.Stats()
	if stats.Rounds != rounds || stats.Completed != rounds {
		t.Fatalf("stats after %d rounds: %+v", rounds, stats)
	}
	if want := rounds * testPrebuffer; stats.Sent != want {
		t.Fatalf("Sent=%d after %d rounds, want %d", stats.Sent, rounds, want)
	}
	if stats.QueueDepth != 0 || stats.AbortedFrames != 0 || stats.Dropped != 0 {
		t.Fatalf("queue/drops leaked across rounds: %+v", stats)
	}
	if got := sink.count(); got != rounds*testPrebuffer {
		t.Fatalf("sink received %d frames, want %d", got, rounds*testPrebuffer)
	}

	runtime.GC()
	after := runtime.NumGoroutine()
	if after > baseline+2 {
		t.Fatalf("goroutines grew from %d to %d after %d rounds (possible leak)", baseline, after, rounds)
	}
}

// TestA17AbortIsNoOpOutsideRound checks Abort does not corrupt a later round or
// the counters when no round is running.
func TestA17AbortIsNoOpOutsideRound(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	markers := &markerRecorder{clk: fc}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	p.Abort()
	if stats := p.Stats(); stats.AbortedRounds != 0 {
		t.Fatalf("Abort with no active round changed stats: %+v", stats)
	}
	pushN(t, p, 2)
	if !p.FinishInput() {
		t.Fatal("FinishInput returned false")
	}
	errCh := startRound(t, p, sink, markers)
	drivePreloadedRoundProduced(t, p, fc, ev, sink, markers, 1, 2)
	p.Abort() // after natural completion
	if err := waitRound(t, errCh); err != nil {
		t.Fatalf("round after no-op abort: %v", err)
	}
	if stats := p.Stats(); stats.AbortedRounds != 0 || stats.Completed != 1 {
		t.Fatalf("unexpected stats after no-op abort: %+v", stats)
	}
}

// TestA17MaxQueueBoundMatchesSpec records that the probe queue bound is 64
// frames by default (A19 context) and that larger explicit queues are refused.
func TestA17MaxQueueBoundMatchesSpec(t *testing.T) {
	fc := clock.NewFake(testBase)
	cfg := newConfig(fc, nil)
	p := mustPlayer(t, cfg)
	for i := 0; i < limits.AudioQueueFrames; i++ {
		if !p.Push(makeFrame(i)) {
			t.Fatalf("Push %d within default queue bound was rejected", i)
		}
	}
	if p.Push(makeFrame(limits.AudioQueueFrames)) {
		t.Fatal("Push past the default 64-frame queue bound was accepted")
	}
	stats := p.Stats()
	if stats.QueueDepth != limits.AudioQueueFrames || stats.QueueHighWater != limits.AudioQueueFrames || stats.Dropped != 1 {
		t.Fatalf("stats at queue bound %+v", stats)
	}
	cfg.MaxQueue = limits.AudioQueueFrames + 1
	if _, err := playout.New(cfg); !errors.Is(err, playout.ErrInvalidConfig) {
		t.Fatalf("New with MaxQueue above probe bound error=%v, want ErrInvalidConfig", err)
	}
}

// TestA17AbortWinsOverContextCancellation mirrors the probe integration: the
// session layer calls Abort and then cancels the round context. Both paths must
// report ErrAborted (never a bare context error) so an intentional abort is not
// misreported as a failure.
func TestA17AbortWinsOverContextCancellation(t *testing.T) {
	fc := clock.NewFake(testBase)
	ev := newEventCollector()
	sink := &recordingSink{}
	p := mustPlayer(t, newConfig(fc, ev.observe))

	for i := 0; i < 10; i++ {
		pushN(t, p, testPrebuffer)
		if !p.FinishInput() {
			t.Fatalf("round %d: FinishInput returned false", i)
		}
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- p.StartRound(ctx, sink, nil) }()
		round := uint64(i + 1)
		ev.wait(t, round, playout.EventSettleWait)
		p.Abort()
		cancel()
		if err := waitRound(t, errCh); !errors.Is(err, playout.ErrAborted) {
			t.Fatalf("round %d error=%v, want ErrAborted when Abort and context cancellation race", i, err)
		}
		if got := sink.count(); got != 0 {
			t.Fatalf("round %d sent %d frame(s) despite abort during settle", i, got)
		}
	}
	if stats := p.Stats(); stats.AbortedRounds != 10 || stats.Completed != 0 {
		t.Fatalf("stats after abort/cancel races %+v", stats)
	}
}
