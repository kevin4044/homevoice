package devicemcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/logging"
)

type countingSender struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSender) SendMCP(json.RawMessage) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil
}

func (s *countingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type blockingContextSender struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (s *blockingContextSender) SendMCP(json.RawMessage) error {
	return errors.New("legacy sender must not be used when ContextSender is implemented")
}

func (s *blockingContextSender) SendMCPContext(ctx context.Context, _ json.RawMessage) error {
	close(s.started)
	<-ctx.Done()
	s.once.Do(func() { close(s.canceled) })
	return ctx.Err()
}

type blockingLegacySender struct {
	release chan struct{}
	calls   atomic.Int64
}

func (s *blockingLegacySender) SendMCP(json.RawMessage) error {
	s.calls.Add(1)
	<-s.release
	return nil
}

func newTestClient(t *testing.T, sender Sender) *Client {
	t.Helper()
	return New(Identity{DeviceID: "dev-a", Generation: 1, SessionID: "session-a"},
		sender, clock.NewFake(time.Unix(1700000000, 0)), logging.Discard())
}

// TestA25PreCanceledToolIsNotSent verifies the entry/pre-send cancellation
// check: a pre-canceled tools/call must not publish even one request.
func TestA25PreCanceledToolIsNotSent(t *testing.T) {
	sender := &countingSender{}
	c := newTestClient(t, sender)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CallTool(ctx, "demo.speaker.set_volume", json.RawMessage(`{"volume":20}`))
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("CallTool error = %v, want ErrCanceled", err)
	}
	if got := sender.count(); got != 0 {
		t.Fatalf("pre-canceled call published %d request(s), want 0", got)
	}
}

// TestA25BlockedSenderExitsOnCancel verifies that a context-aware sender can
// be interrupted by caller cancellation and the call returns promptly.
func TestA25BlockedSenderExitsOnCancel(t *testing.T) {
	sender := &blockingContextSender{started: make(chan struct{}), canceled: make(chan struct{})}
	c := newTestClient(t, sender)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "demo.speaker.set_volume", json.RawMessage(`{"volume":20}`))
		done <- err
	}()
	select {
	case <-sender.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sender was not invoked")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("CallTool error = %v, want ErrCanceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallTool did not return after cancellation")
	}
	select {
	case <-sender.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("context-aware sender did not observe cancellation")
	}
}

// TestA25LegacyBlockedSenderBounded verifies the legacy fallback cannot make
// the caller wait forever; the sender is released by the test afterwards.
func TestA25LegacyBlockedSenderBounded(t *testing.T) {
	sender := &blockingLegacySender{release: make(chan struct{})}
	defer close(sender.release)
	c := newTestClient(t, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "demo.speaker.set_volume", json.RawMessage(`{"volume":20}`))
	if err == nil {
		t.Fatal("blocked legacy sender unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("legacy sender wait was not bounded: %s", elapsed)
	}
	if sender.calls.Load() != 1 {
		t.Fatalf("legacy sender calls = %d, want 1 (in-flight side effect)", sender.calls.Load())
	}
}
