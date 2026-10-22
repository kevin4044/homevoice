package session

// ReplayDecision is the outcome of one inbound sequence window check.
type ReplayDecision int

const (
	// ReplayAccept means the packet is new (or the first packet) and in window.
	ReplayAccept ReplayDecision = iota
	// ReplayDuplicate means the sequence was already accepted.
	ReplayDuplicate
	// ReplayTooOld means the sequence is outside the sliding window.
	ReplayTooOld
)

func (d ReplayDecision) String() string {
	switch d {
	case ReplayAccept:
		return "accept"
	case ReplayDuplicate:
		return "duplicate"
	case ReplayTooOld:
		return "too-old"
	default:
		return "unknown"
	}
}

// ReplayWindow is a 64-packet sliding replay window. It is deliberately
// separate from authentication: passing this window does not authenticate a
// UDP peer (AES-CTR has no MAC).
//
// The first accepted packet initialises the window, so a firmware that starts
// at sequence 0 is handled explicitly: 0 is recorded as the first sequence and
// a later 0 is a duplicate. An uninitialised window never pretends to have
// already seen 0.
type ReplayWindow struct {
	size        uint32
	seen        [64]bool
	highest     uint32
	initialized bool
	accepted    uint64
	duplicates  uint64
	tooOld      uint64
}

// NewReplayWindow creates a window. size is clamped to the documented 64-packet
// limit; values <= 0 use the default.
func NewReplayWindow(size int) *ReplayWindow {
	if size <= 0 || size > 64 {
		size = 64
	}
	return &ReplayWindow{size: uint32(size)}
}

// Accept checks one inbound sequence number and updates the window.
func (w *ReplayWindow) Accept(seq uint32) ReplayDecision {
	if !w.initialized {
		w.initialized = true
		w.highest = seq
		w.seen[seq%w.size] = true
		w.accepted++
		return ReplayAccept
	}
	delta := int32(seq - w.highest)
	switch {
	case delta > 0:
		if uint32(delta) >= w.size {
			for i := range w.seen {
				w.seen[i] = false
			}
		} else {
			for i := uint32(1); i <= uint32(delta); i++ {
				w.seen[(w.highest+i)%w.size] = false
			}
		}
		w.highest = seq
		w.seen[seq%w.size] = true
		w.accepted++
		return ReplayAccept
	case delta == 0:
		w.duplicates++
		return ReplayDuplicate
	default:
		if uint32(-delta) >= w.size {
			w.tooOld++
			return ReplayTooOld
		}
		idx := seq % w.size
		if w.seen[idx] {
			w.duplicates++
			return ReplayDuplicate
		}
		w.seen[idx] = true
		w.accepted++
		return ReplayAccept
	}
}

// Stats returns cumulative counters and the current window high water mark.
func (w *ReplayWindow) Stats() (accepted, duplicates, tooOld uint64, highest uint32, initialized bool) {
	return w.accepted, w.duplicates, w.tooOld, w.highest, w.initialized
}
