package clock

import (
	"testing"
	"time"
)

func TestFakeClockFiresInDeadlineOrder(t *testing.T) {
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	c50 := f.After(50 * time.Millisecond)
	c20 := f.After(20 * time.Millisecond)
	c50b := f.After(50 * time.Millisecond)

	select {
	case <-c20:
		t.Fatal("timer fired before Advance")
	default:
	}

	f.Advance(19 * time.Millisecond)
	select {
	case <-c20:
		t.Fatal("timer fired 1ns early")
	default:
	}

	f.Advance(1 * time.Millisecond)
	select {
	case got := <-c20:
		if want := start.Add(20 * time.Millisecond); !got.Equal(want) {
			t.Fatalf("timer fired at %v, want %v", got, want)
		}
	default:
		t.Fatal("due timer did not fire")
	}
	if f.Now() != start.Add(20*time.Millisecond) {
		t.Fatalf("now = %v", f.Now())
	}

	f.Advance(30 * time.Millisecond)
	// Both 50ms timers are due; first registered fires first.
	first := <-c50
	second := <-c50b
	if !first.Equal(second) {
		t.Fatalf("same-deadline timers fired at different times: %v vs %v", first, second)
	}
	if f.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", f.Pending())
	}
}

func TestFakeClockAdvanceToAndPending(t *testing.T) {
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	c := f.After(time.Second)
	if f.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", f.Pending())
	}
	f.AdvanceTo(start.Add(500 * time.Millisecond))
	select {
	case <-c:
		t.Fatal("timer fired too early")
	default:
	}
	f.AdvanceTo(start.Add(2 * time.Second))
	select {
	case <-c:
	default:
		t.Fatal("timer did not fire after AdvanceTo")
	}
	// Advancing backwards is a no-op.
	f.AdvanceTo(start)
	if !f.Now().Equal(start.Add(2 * time.Second)) {
		t.Fatalf("now = %v", f.Now())
	}
}

func TestRealClock(t *testing.T) {
	before := time.Now()
	got := (Real{}).Now()
	if got.Before(before) {
		t.Fatalf("Real.Now() went backwards")
	}
	select {
	case <-(Real{}).After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("Real.After did not fire")
	}
}
