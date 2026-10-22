// Package playout implements the M0 probe's bounded, clock-driven downlink
// playback scheduler.
//
// A round is one playback attempt with the documented protocol ordering:
//
//	TTSStart -> cancellable settle -> prebuffer -> paced frames -> tail wait -> TTSStop
//
// StartRound runs the round on the caller's goroutine; the package starts no
// hidden scheduler goroutine, so ending a round releases every resource. The
// producer calls Push to enqueue Opus frames and FinishInput when no more
// frames are coming. Without FinishInput a round ends only through Abort or
// context cancellation; the caller owns the overall round deadline.
//
// Timing is driven exclusively by Config.Clock (clock.Fake in tests,
// clock.Real in production): the package never sleeps. Defaults come from
// internal/m0/limits: Settle=limits.TTSSettle, PrebufferFrames=
// limits.TTSPrebufferFrames, TailExtraFrames=limits.TTSTailExtraFrames and
// MaxQueue=limits.AudioQueueFrames.
//
// # Round semantics
//
//   - TTSStart is called before any wait or audio frame.
//   - After TTSStart the round waits Settle; no SendFrame happens before the
//     settle timer fires.
//   - The first SendFrame happens only once at least PrebufferFrames frames are
//     queued, or once FinishInput proves that no more frames will arrive (a
//     short input cannot prebuffer more; its available frames are sent after
//     settle).
//   - After the first send, frames are sent one per FrameDuration measured
//     from the previous send, even when the queue is backlogged. A gap caused
//     by a slow upstream therefore never becomes a burst: at most one frame is
//     sent when data resumes, the rest remain paced.
//   - When the queue is empty and FinishInput was called, the round waits
//     (PrebufferFrames+TailExtraFrames)*FrameDuration after the last send
//     (from the moment input completion is observed if no frame was sent), then
//     calls TTSStop.
//   - Abort (or context cancellation) ends the round promptly: once Abort
//     returns no new audio SendFrame is started, queued frames are discarded
//     and counted, TTSStop is still attempted (best effort), and the Player can
//     be used for the next round immediately.
//
// # Bounds and safety
//
// The queue is bounded by MaxQueue frames; Push never blocks. Frames rejected
// by the bound, by an already-finished input, or by an aborted round return
// false and are counted in Stats.Dropped. Push copies each frame, so callers
// may reuse their buffer. Frames must be non-empty and at most MaxFrameBytes.
//
// Sink.SendFrame and the marker methods are called from the StartRound
// goroutine and never concurrently with each other. They must be fast, bounded
// and must not call back into the Player (re-entrancy can deadlock). A
// SendFrame error ends the round with that error. Marker errors are returned
// and counted; Markers may be nil.
//
// Config.Observer is an optional test/report hook for phase transitions. It is
// called synchronously from the round goroutine, and for wait events it is
// called after the corresponding clock timer has been registered, so a
// fake-clock test can advance deterministically without sleeping. It must not
// block.
//
// # Preloading and input completion
//
// Push and FinishInput may also be called while no round is active; such
// frames/finish mark belong to the next round. This makes an all-at-once
// producer race-free: it can preload frames and call FinishInput before (or
// while) StartRound begins. At the end of every round the queue is cleared and
// the input-complete flag is reset, so frames preloaded for one round never
// leak into a later round. Producers should not push late frames after their
// round ends, because they would be treated as preload for the next round.
package playout

import (
	"errors"
	"strconv"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

// MaxFrameBytes is the largest single frame Push accepts: the probe's maximum
// UDP payload. Opus packets are smaller (audio.MaxOpusPacketBytes = 1275) and
// the codec layer enforces that bound; the playout queue additionally keeps
// the transport bound.
const MaxFrameBytes = limits.MaxUDPPayloadBytes

// Errors returned by the player. The sentinels are matchable with errors.Is.
var (
	// ErrInvalidConfig reports a Config that cannot be used safely.
	ErrInvalidConfig = errors.New("playout: invalid config")

	// ErrNotInitialized reports use of a zero-value Player; use New.
	ErrNotInitialized = errors.New("playout: zero-value player; use New")

	// ErrNilSink reports StartRound without a Sink.
	ErrNilSink = errors.New("playout: nil sink")

	// ErrRoundActive reports StartRound while another round is running.
	ErrRoundActive = errors.New("playout: a round is already in progress")

	// ErrAborted reports a round stopped by Abort.
	ErrAborted = errors.New("playout: round aborted")
)

// Sink sends one encoded audio frame. It must be fast, bounded, and must not
// call back into the Player.
type Sink interface {
	SendFrame(data []byte) error
}

// Markers emits the MQTT TTS start/stop markers around a round. A nil Markers
// disables marker calls.
type Markers interface {
	TTSStart() error
	TTSStop() error
}

// Config configures a Player. Clock and FrameDuration are required; the other
// zero values select the probe defaults from internal/m0/limits.
type Config struct {
	// Clock drives all timing. Tests use clock.Fake.
	Clock clock.Clock

	// FrameDuration is the pacing interval between frames (60 ms for the M0
	// downlink candidate). It is required and has no default.
	FrameDuration time.Duration

	// Settle is the cancellable wait between TTSStart and the first frame.
	// Default: limits.TTSSettle.
	Settle time.Duration

	// PrebufferFrames is the number of queued frames required before the first
	// SendFrame. Zero selects the default limits.TTSPrebufferFrames; set an
	// explicit small value for short inputs.
	PrebufferFrames int

	// TailExtraFrames is added to PrebufferFrames for the final tail wait.
	// Default: limits.TTSTailExtraFrames.
	TailExtraFrames int

	// MaxQueue bounds the frame queue. Default: limits.AudioQueueFrames. The
	// probe specification caps the queue at limits.AudioQueueFrames, so larger
	// values are rejected.
	MaxQueue int

	// Observer optionally receives every scheduling phase transition. It is
	// called synchronously from the round goroutine and must not block.
	Observer func(Event)
}

// Stats is a snapshot of player counters. QueueDepth is observed at the time
// of the call; the other values are cumulative.
type Stats struct {
	Rounds         int // rounds started
	Completed      int // rounds that ended without error
	AbortedRounds  int // explicit Abort calls that terminated a running round
	Sent           int // frames successfully delivered to Sink.SendFrame
	Dropped        int // Push calls rejected by bounds/state
	AbortedFrames  int // queued frames discarded by Abort or a failed round
	SendErrors     int // Sink.SendFrame errors
	MarkerErrors   int // TTSStart/TTSStop errors
	QueueDepth     int // frames currently queued
	QueueHighWater int // maximum queue depth observed
}

// EventKind identifies one scheduler phase transition, reported through
// Config.Observer for tests and timing evidence.
type EventKind int

// Scheduling phase transitions, in the order a normal round emits them.
const (
	EventRoundStart     EventKind = iota + 1 // round state initialised
	EventSettleWait                          // settle timer registered
	EventSettleDone                          // settle timer fired
	EventPrebufferReady                      // prebuffer condition satisfied
	EventFirstSend                           // first audio frame was sent
	EventFrameSent                           // an audio frame was sent
	EventPacingWait                          // pacing timer registered for the next frame
	EventTailWait                            // tail timer registered
	EventTTSStop                             // TTSStop marker succeeded
	EventRoundEnd                            // round finished (success or error)
	EventAborted                             // Abort terminated the active round
)

// String renders an EventKind for test failure messages.
func (k EventKind) String() string {
	switch k {
	case EventRoundStart:
		return "round-start"
	case EventSettleWait:
		return "settle-wait"
	case EventSettleDone:
		return "settle-done"
	case EventPrebufferReady:
		return "prebuffer-ready"
	case EventFirstSend:
		return "first-send"
	case EventFrameSent:
		return "frame-sent"
	case EventPacingWait:
		return "pacing-wait"
	case EventTailWait:
		return "tail-wait"
	case EventTTSStop:
		return "tts-stop"
	case EventRoundEnd:
		return "round-end"
	case EventAborted:
		return "aborted"
	default:
		return "event(" + strconv.Itoa(int(k)) + ")"
	}
}

// Event is one observer notification. At is the clock time when the phase was
// observed. Deadline is set for wait events (settle, pacing, tail). Sent and
// QueueDepth are snapshot counters relevant to the event kind.
type Event struct {
	Kind       EventKind
	Round      uint64
	At         time.Time
	Deadline   time.Time
	Sent       int
	QueueDepth int
}
