package playout_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/playout"
)

// Shared fixtures for the A16/A17 fake-clock tests. All scheduling time comes
// from clock.Fake; real time is used only as a test-failure timeout.

const (
	testFrameDuration = 60 * time.Millisecond
	testSettle        = limits.TTSSettle
	testPrebuffer     = limits.TTSPrebufferFrames
	testTailExtra     = limits.TTSTailExtraFrames
	testMaxQueue      = limits.AudioQueueFrames
)

var testBase = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func tailWaitDuration() time.Duration {
	return time.Duration(testPrebuffer+testTailExtra) * testFrameDuration
}

func makeFrame(i int) []byte {
	return []byte{byte(i), 0x5a, byte(i >> 8)}
}

func newConfig(fc clock.Clock, observer func(playout.Event)) playout.Config {
	return playout.Config{
		Clock:         fc,
		FrameDuration: testFrameDuration,
		Observer:      observer,
	}
}

type recordingSink struct {
	mu     sync.Mutex
	frames [][]byte
	failAt int   // 1-based send number that fails; 0 never fails
	err    error // error returned at failAt
}

func (s *recordingSink) SendFrame(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt > 0 && len(s.frames)+1 == s.failAt {
		if s.err == nil {
			return errors.New("synthetic sink failure")
		}
		return s.err
	}
	s.frames = append(s.frames, append([]byte(nil), data...))
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func (s *recordingSink) snapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.frames))
	copy(out, s.frames)
	return out
}

type markerRecorder struct {
	mu sync.Mutex

	clk clock.Clock

	failStart error
	failStop  error

	starts  int
	stops   int
	startAt time.Time
	stopAt  time.Time
	order   []string
}

func (m *markerRecorder) TTSStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starts++
	m.startAt = m.clk.Now()
	m.order = append(m.order, "start")
	return m.failStart
}

func (m *markerRecorder) TTSStop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	m.stopAt = m.clk.Now()
	m.order = append(m.order, "stop")
	return m.failStop
}

func (m *markerRecorder) snapshot() (starts, stops int, startAt, stopAt time.Time, order []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.starts, m.stops, m.startAt, m.stopAt, append([]string(nil), m.order...)
}

type eventCollector struct {
	ch chan playout.Event
}

func newEventCollector() *eventCollector {
	return &eventCollector{ch: make(chan playout.Event, 4096)}
}

func (e *eventCollector) observe(ev playout.Event) {
	select {
	case e.ch <- ev:
	default: // never block the scheduler in tests
	}
}

// wait returns the next event of the requested kind for the given round,
// failing after a real-time guard timeout. Events from other rounds are
// discarded so stale notifications from an aborted round cannot mask a later
// round's timing.
func (e *eventCollector) wait(t *testing.T, round uint64, kind playout.EventKind) playout.Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev := <-e.ch:
			if ev.Kind == kind && ev.Round == round {
				return ev
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for observer event %s of round %d", kind, round)
		}
	}
}

func mustPlayer(t *testing.T, cfg playout.Config) *playout.Player {
	t.Helper()
	p, err := playout.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func startRound(t *testing.T, p *playout.Player, sink playout.Sink, markers playout.Markers) <-chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.StartRound(context.Background(), sink, markers)
	}()
	return errCh
}

func waitRound(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for StartRound to return")
		return nil
	}
}

func pushN(t *testing.T, p *playout.Player, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !p.Push(makeFrame(i)) {
			t.Fatalf("Push frame %d was rejected", i)
		}
	}
}
