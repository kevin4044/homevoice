// Package clock provides the small time abstraction used by the probe's
// scheduling logic. Tests use Fake to verify settle/prebuffer/tail-wait
// semantics without long real sleeps.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the subset of time behaviour the probe needs.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Real is the production clock.
type Real struct{}

// Now returns the current wall-clock time.
func (Real) Now() time.Time { return time.Now() }

// After delegates to time.After.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a manually advanced clock. Advancing fires due timers in deadline
// order. Each returned channel is buffered, so firing never blocks.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	seq    int64
}

type fakeTimer struct {
	deadline time.Time
	seq      int64
	ch       chan time.Time
	fired    bool
}

// NewFake returns a fake clock starting at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now returns the fake current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After registers a timer at now+d.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d < 0 {
		d = 0
	}
	f.seq++
	t := &fakeTimer{deadline: f.now.Add(d), seq: f.seq, ch: make(chan time.Time, 1)}
	f.timers = append(f.timers, t)
	return t.ch
}

// Advance moves the fake clock forward by d, firing due timers in deadline
// order (ties by registration order).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	f.fireUntilLocked(target)
	f.mu.Unlock()
}

// AdvanceTo moves the fake clock to t.
func (f *Fake) AdvanceTo(t time.Time) {
	f.mu.Lock()
	if t.After(f.now) {
		f.fireUntilLocked(t)
	}
	f.mu.Unlock()
}

func (f *Fake) fireUntilLocked(target time.Time) {
	for {
		if len(f.timers) == 0 {
			f.now = target
			return
		}
		sort.SliceStable(f.timers, func(i, j int) bool {
			if f.timers[i].deadline.Equal(f.timers[j].deadline) {
				return f.timers[i].seq < f.timers[j].seq
			}
			return f.timers[i].deadline.Before(f.timers[j].deadline)
		})
		next := f.timers[0]
		if next.fired || next.deadline.After(target) {
			f.now = target
			return
		}
		f.timers = f.timers[1:]
		next.fired = true
		f.now = next.deadline
		next.ch <- next.deadline
	}
}

// Pending returns the number of unfired timers (test introspection).
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := 0
	for _, t := range f.timers {
		if !t.fired {
			pending++
		}
	}
	return pending
}
