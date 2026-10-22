package session

import "testing"

func TestReplayWindowFirstSequenceZero(t *testing.T) {
	w := NewReplayWindow(64)
	if got := w.Accept(0); got != ReplayAccept {
		t.Fatalf("first sequence 0 = %v, want accept (window must not pretend 0 was already seen)", got)
	}
	if got := w.Accept(0); got != ReplayDuplicate {
		t.Fatalf("second sequence 0 = %v, want duplicate", got)
	}
	if got := w.Accept(1); got != ReplayAccept {
		t.Fatalf("sequence 1 = %v", got)
	}
}

func TestReplayWindowOutOfOrderAndDuplicate(t *testing.T) {
	w := NewReplayWindow(64)
	for _, seq := range []uint32{1, 3, 2} {
		if got := w.Accept(seq); got != ReplayAccept {
			t.Fatalf("sequence %d = %v, want accept", seq, got)
		}
	}
	if got := w.Accept(3); got != ReplayDuplicate {
		t.Fatalf("duplicate 3 = %v, want duplicate", got)
	}
	accepted, duplicates, tooOld, highest, initialized := w.Stats()
	if accepted != 3 || duplicates != 1 || tooOld != 0 {
		t.Fatalf("stats accepted=%d dup=%d old=%d", accepted, duplicates, tooOld)
	}
	if !initialized || highest != 3 {
		t.Fatalf("window highest=%d initialized=%v", highest, initialized)
	}
}

func TestReplayWindowTooOldAndLargeJump(t *testing.T) {
	w := NewReplayWindow(64)
	if got := w.Accept(100); got != ReplayAccept {
		t.Fatal("first packet rejected")
	}
	if got := w.Accept(36); got != ReplayTooOld {
		t.Fatalf("sequence 64 behind = %v, want too-old", got)
	}
	if got := w.Accept(37); got != ReplayAccept {
		t.Fatalf("sequence 63 behind = %v, want accept", got)
	}
	// A large forward jump clears the window but is accepted.
	if got := w.Accept(500); got != ReplayAccept {
		t.Fatalf("large jump = %v", got)
	}
	if got := w.Accept(499); got != ReplayAccept {
		t.Fatalf("out-of-order after jump = %v", got)
	}
}

func TestReplayWindowWrapAround(t *testing.T) {
	w := NewReplayWindow(64)
	if got := w.Accept(^uint32(0) - 2); got != ReplayAccept {
		t.Fatalf("pre-wrap = %v", got)
	}
	for _, seq := range []uint32{^uint32(0) - 1, ^uint32(0), 0, 1} {
		if got := w.Accept(seq); got != ReplayAccept {
			t.Fatalf("sequence %d across wrap = %v", seq, got)
		}
	}
	if got := w.Accept(0); got != ReplayDuplicate {
		t.Fatalf("duplicate across wrap = %v", got)
	}
}
