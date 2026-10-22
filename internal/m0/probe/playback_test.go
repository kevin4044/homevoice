package probe

import (
	"testing"
)

// TestFinishPlaybackIgnoresSupersededRound reproduces the Cursor Bugbot finding
// "Playback defer clears newer round": a delayed/aborted round completion must
// not clear the playing/busy state of a newer playback round.
func TestFinishPlaybackIgnoresSupersededRound(t *testing.T) {
	p := &Probe{states: map[string]*deviceState{}}
	newerCancelHit := false
	cancelNewer := func() { newerCancelHit = true }
	p.states["dev-a"] = &deviceState{
		deviceID:   "dev-a",
		playing:    true,
		playToken:  2,
		playCancel: cancelNewer,
	}

	// Older round (token 1) returns after the newer round (token 2) started.
	p.finishPlayback("dev-a", 1)
	st := p.states["dev-a"]
	if !st.playing {
		t.Fatal("superseded playback completion cleared the newer round's playing state")
	}
	if st.playCancel == nil {
		t.Fatal("superseded playback completion cleared the newer round's cancel function")
	}
	st.playCancel()
	if !newerCancelHit {
		t.Fatal("newer round's cancel function was replaced")
	}

	// The current token still clears the state.
	p.finishPlayback("dev-a", 2)
	if st.playing || st.playCancel != nil {
		t.Fatalf("current playback completion did not clear state: playing=%v cancel=%v", st.playing, st.playCancel != nil)
	}

	// Unknown devices and stale tokens are no-ops.
	p.finishPlayback("missing", 1)
	p.finishPlayback("dev-a", 1)
}

// TestPlayTokenAdvancesAcrossRounds ensures each playback gets a distinct token,
// including after cancelDeviceTasks resets the visible playing flag.
func TestPlayTokenAdvancesAcrossRounds(t *testing.T) {
	p := &Probe{states: map[string]*deviceState{}}
	p.states["dev-a"] = &deviceState{deviceID: "dev-a"}
	first := p.states["dev-a"]
	first.playToken++
	if first.playToken != 1 {
		t.Fatalf("first token = %d", first.playToken)
	}
	p.cancelDeviceTasks("dev-a")
	first.playToken++
	if first.playToken != 2 {
		t.Fatalf("token after cancel = %d", first.playToken)
	}
	if first.playing || first.playCancel != nil {
		t.Fatal("cancelDeviceTasks left playback state set")
	}
}
