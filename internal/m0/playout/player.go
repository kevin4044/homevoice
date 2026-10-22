package playout

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// Player is a bounded, fake-clock-friendly playback scheduler. Use New to
// construct one; the zero value is not usable. A Player may run one round at a
// time, but Push, FinishInput, Abort and Stats may be called concurrently with
// StartRound.
type Player struct {
	mu     sync.Mutex
	sendMu sync.Mutex // order of audio sends versus Abort

	ready bool
	cfg   Config

	stats Stats

	queue [][]byte
	head  int

	active    bool // a round is running
	aborted   bool // current round was aborted
	inputDone bool // producer finished current (or preloaded next) round
	abortCh   chan struct{}
	notifyCh  chan struct{}
	roundDone chan struct{}
	round     uint64
}

// New validates cfg and applies the probe defaults.
func New(cfg Config) (*Player, error) {
	if cfg.Clock == nil {
		return nil, fmt.Errorf("%w: Clock is required (use clock.Real in production)", ErrInvalidConfig)
	}
	if cfg.FrameDuration <= 0 {
		return nil, fmt.Errorf("%w: FrameDuration must be positive, got %s", ErrInvalidConfig, cfg.FrameDuration)
	}
	if cfg.Settle < 0 {
		return nil, fmt.Errorf("%w: Settle must not be negative, got %s", ErrInvalidConfig, cfg.Settle)
	}
	if cfg.Settle == 0 {
		cfg.Settle = limits.TTSSettle
	}
	if cfg.PrebufferFrames < 0 {
		return nil, fmt.Errorf("%w: PrebufferFrames must not be negative, got %d", ErrInvalidConfig, cfg.PrebufferFrames)
	}
	if cfg.PrebufferFrames == 0 {
		cfg.PrebufferFrames = limits.TTSPrebufferFrames
	}
	if cfg.TailExtraFrames < 0 {
		return nil, fmt.Errorf("%w: TailExtraFrames must not be negative, got %d", ErrInvalidConfig, cfg.TailExtraFrames)
	}
	if cfg.TailExtraFrames == 0 {
		cfg.TailExtraFrames = limits.TTSTailExtraFrames
	}
	if cfg.MaxQueue < 0 {
		return nil, fmt.Errorf("%w: MaxQueue must not be negative, got %d", ErrInvalidConfig, cfg.MaxQueue)
	}
	if cfg.MaxQueue == 0 {
		cfg.MaxQueue = limits.AudioQueueFrames
	}
	if cfg.MaxQueue > limits.AudioQueueFrames {
		return nil, fmt.Errorf("%w: MaxQueue %d exceeds the probe bound %d", ErrInvalidConfig, cfg.MaxQueue, limits.AudioQueueFrames)
	}
	if cfg.PrebufferFrames > cfg.MaxQueue {
		return nil, fmt.Errorf("%w: PrebufferFrames %d exceeds MaxQueue %d", ErrInvalidConfig, cfg.PrebufferFrames, cfg.MaxQueue)
	}

	p := &Player{
		ready:    true,
		cfg:      cfg,
		queue:    make([][]byte, 0, cfg.MaxQueue),
		notifyCh: make(chan struct{}, 1),
	}
	return p, nil
}

// Push enqueues one frame for the current round, or for the next round when no
// round is active. It never blocks. It returns false when the frame is empty,
// larger than MaxFrameBytes, the input was already marked complete, the current
// round was aborted, or the bounded queue is full; every rejected frame is
// counted in Stats.Dropped. The frame is copied, so the caller may reuse data.
func (p *Player) Push(frame []byte) bool {
	if p == nil || !p.ready {
		return false
	}
	if len(frame) == 0 || len(frame) > MaxFrameBytes {
		p.mu.Lock()
		p.stats.Dropped++
		p.mu.Unlock()
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inputDone || (p.active && p.aborted) {
		p.stats.Dropped++
		return false
	}
	if p.depthLocked() >= p.cfg.MaxQueue {
		p.stats.Dropped++
		return false
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	p.compactLocked()
	p.queue = append(p.queue, cp)
	if depth := p.depthLocked(); depth > p.stats.QueueHighWater {
		p.stats.QueueHighWater = depth
	}
	p.signalLocked()
	return true
}

// FinishInput declares that the producer has pushed its last frame for the
// current round (or for the next round when none is active). After the queue is
// drained the round performs the tail wait and stops. It returns false if the
// input was already marked complete or the active round was aborted.
func (p *Player) FinishInput() bool {
	if p == nil || !p.ready {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inputDone || (p.active && p.aborted) {
		return false
	}
	p.inputDone = true
	p.signalLocked()
	return true
}

// StartRound runs one playback round and returns when it ends: nil on natural
// completion, ErrAborted when Abort stopped it, the context error when ctx was
// cancelled, or a sink/marker error. It returns ErrRoundActive if another round
// is running, unless that round was aborted, in which case StartRound waits for
// its cleanup and then starts the new round.
//
// Push/FinishInput may be called concurrently from the producer. Context
// cancellation and Abort behave the same for audio sends: no frame is started
// after the round has observed them.
func (p *Player) StartRound(ctx context.Context, sink Sink, markers Markers) error {
	if p == nil || !p.ready {
		return ErrNotInitialized
	}
	if sink == nil {
		return ErrNilSink
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		p.mu.Lock()
		if !p.active {
			break
		}
		if p.aborted {
			done := p.roundDone
			p.mu.Unlock()
			<-done
			continue
		}
		p.mu.Unlock()
		return ErrRoundActive
	}

	p.round++
	round := p.round
	p.active = true
	p.aborted = false
	p.abortCh = make(chan struct{})
	p.roundDone = make(chan struct{})
	abortCh := p.abortCh
	done := p.roundDone
	p.stats.Rounds++
	startAt := p.cfg.Clock.Now()
	startDepth := p.depthLocked()
	p.mu.Unlock()

	p.emit(Event{Kind: EventRoundStart, Round: round, At: startAt, QueueDepth: startDepth})
	err := p.runRound(ctx, sink, markers, round, abortCh)

	p.mu.Lock()
	if err == nil {
		p.stats.Completed++
	}
	if discarded := p.depthLocked(); discarded > 0 {
		p.stats.AbortedFrames += discarded
	}
	p.clearQueueLocked()
	p.active = false
	p.aborted = false
	p.inputDone = false
	p.abortCh = nil
	close(done)
	endAt := p.cfg.Clock.Now()
	sent := p.stats.Sent
	p.mu.Unlock()

	p.emit(Event{Kind: EventRoundEnd, Round: round, At: endAt, Sent: sent})
	return err
}

// Abort stops the active round. Once it returns, no new audio frame is started;
// queued frames are discarded and counted, TTSStop is attempted best effort by
// the round goroutine, and the Player can start a new round. Abort is a no-op
// when no round is active.
func (p *Player) Abort() {
	if p == nil || !p.ready {
		return
	}
	p.sendMu.Lock()
	p.mu.Lock()
	if !p.active || p.aborted {
		p.mu.Unlock()
		p.sendMu.Unlock()
		return
	}
	round := p.round
	p.aborted = true
	close(p.abortCh)
	p.stats.AbortedRounds++
	discarded := p.depthLocked()
	p.stats.AbortedFrames += discarded
	p.clearQueueLocked()
	now := p.cfg.Clock.Now()
	p.mu.Unlock()
	p.sendMu.Unlock()

	p.emit(Event{Kind: EventAborted, Round: round, At: now})
}

// Stats returns a consistent snapshot of the player counters.
func (p *Player) Stats() Stats {
	if p == nil || !p.ready {
		return Stats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.QueueDepth = p.depthLocked()
	return s
}

func (p *Player) runRound(ctx context.Context, sink Sink, markers Markers, round uint64, abortCh <-chan struct{}) (err error) {
	if err := p.roundStopped(ctx); err != nil {
		return err
	}

	markerStarted := false
	if markers != nil {
		if e := markers.TTSStart(); e != nil {
			p.noteMarkerError()
			return fmt.Errorf("playout: tts start marker: %w", e)
		}
		markerStarted = true
	}
	defer func() {
		if !markerStarted || markers == nil {
			return
		}
		if e := markers.TTSStop(); e != nil {
			p.noteMarkerError()
			if err == nil {
				err = fmt.Errorf("playout: tts stop marker: %w", e)
			}
			return
		}
		p.emit(Event{Kind: EventTTSStop, Round: round, At: p.cfg.Clock.Now()})
	}()

	if err = p.waitSettle(ctx, round, abortCh); err != nil {
		return err
	}
	p.emit(Event{Kind: EventSettleDone, Round: round, At: p.cfg.Clock.Now()})
	if err = p.waitPrebuffer(ctx, round, abortCh); err != nil {
		return err
	}
	p.emit(Event{Kind: EventPrebufferReady, Round: round, At: p.cfg.Clock.Now(), QueueDepth: p.queueDepth()})
	return p.sendLoop(ctx, sink, round, abortCh)
}

func (p *Player) waitSettle(ctx context.Context, round uint64, abortCh <-chan struct{}) error {
	d := p.cfg.Settle
	if d > 0 {
		now := p.cfg.Clock.Now()
		timer := p.cfg.Clock.After(d)
		p.emit(Event{Kind: EventSettleWait, Round: round, At: now, Deadline: now.Add(d)})
		select {
		case <-timer:
		case <-ctx.Done():
			return p.roundStopped(ctx)
		case <-abortCh:
			return ErrAborted
		}
	}
	return p.roundStopped(ctx)
}

func (p *Player) waitPrebuffer(ctx context.Context, round uint64, abortCh <-chan struct{}) error {
	for {
		p.mu.Lock()
		if p.aborted {
			p.mu.Unlock()
			return ErrAborted
		}
		depth := p.depthLocked()
		done := p.inputDone
		p.mu.Unlock()
		if depth >= p.cfg.PrebufferFrames || done {
			return nil
		}
		select {
		case <-ctx.Done():
			return p.roundStopped(ctx)
		case <-abortCh:
			return ErrAborted
		case <-p.notifyCh:
		}
	}
}

func (p *Player) sendLoop(ctx context.Context, sink Sink, round uint64, abortCh <-chan struct{}) error {
	var (
		lastSend time.Time
		haveSent bool
	)

	for {
		p.mu.Lock()
		if p.aborted {
			p.mu.Unlock()
			return ErrAborted
		}
		depth := p.depthLocked()
		done := p.inputDone
		p.mu.Unlock()

		if depth == 0 {
			if done {
				return p.waitTail(ctx, round, abortCh, lastSend, haveSent)
			}
			select {
			case <-ctx.Done():
				return p.roundStopped(ctx)
			case <-abortCh:
				return ErrAborted
			case <-p.notifyCh:
			}
			continue
		}

		if haveSent {
			if err := p.waitPacing(ctx, round, abortCh, lastSend.Add(p.cfg.FrameDuration)); err != nil {
				return err
			}
		}

		// Check abort/context and start the send under sendMu, so Abort can
		// guarantee that no frame is started after it returns.
		p.sendMu.Lock()
		if err := p.roundStopped(ctx); err != nil {
			p.sendMu.Unlock()
			return err
		}
		p.mu.Lock()
		if p.aborted {
			p.mu.Unlock()
			p.sendMu.Unlock()
			return ErrAborted
		}
		if p.depthLocked() == 0 {
			p.mu.Unlock()
			p.sendMu.Unlock()
			continue
		}
		frame := p.popLocked()
		sendAt := p.cfg.Clock.Now()
		sendErr := sink.SendFrame(frame)
		if sendErr != nil {
			p.stats.SendErrors++
			p.mu.Unlock()
			p.sendMu.Unlock()
			return fmt.Errorf("playout: sink send: %w", sendErr)
		}
		p.stats.Sent++
		sent := p.stats.Sent
		remaining := p.depthLocked()
		p.mu.Unlock()
		p.sendMu.Unlock()

		kind := EventFrameSent
		if !haveSent {
			kind = EventFirstSend
			haveSent = true
		}
		p.emit(Event{Kind: kind, Round: round, At: sendAt, Sent: sent, QueueDepth: remaining})
		lastSend = sendAt
	}
}

func (p *Player) waitPacing(ctx context.Context, round uint64, abortCh <-chan struct{}, deadline time.Time) error {
	now := p.cfg.Clock.Now()
	if !deadline.After(now) {
		// The upstream stalled past this pacing slot. Never send a backlog in
		// one burst: the next frame is sent now and following frames are paced
		// from this send, not from the missed deadline.
		return nil
	}
	timer := p.cfg.Clock.After(deadline.Sub(now))
	p.emit(Event{Kind: EventPacingWait, Round: round, At: now, Deadline: deadline})
	select {
	case <-timer:
		return nil
	case <-ctx.Done():
		return p.roundStopped(ctx)
	case <-abortCh:
		return ErrAborted
	}
}

func (p *Player) waitTail(ctx context.Context, round uint64, abortCh <-chan struct{}, lastSend time.Time, haveSent bool) error {
	tail := time.Duration(p.cfg.PrebufferFrames+p.cfg.TailExtraFrames) * p.cfg.FrameDuration
	now := p.cfg.Clock.Now()
	anchor := now
	if haveSent {
		anchor = lastSend
	}
	deadline := anchor.Add(tail)
	if !deadline.After(now) {
		return nil
	}
	timer := p.cfg.Clock.After(deadline.Sub(now))
	p.emit(Event{Kind: EventTailWait, Round: round, At: now, Deadline: deadline})
	select {
	case <-timer:
		return nil
	case <-ctx.Done():
		return p.roundStopped(ctx)
	case <-abortCh:
		return ErrAborted
	}
}

// roundStopped reports how the active round should end when cancellation is
// observed. An explicit Abort wins over context cancellation: the probe calls
// Abort and then cancels the round context, and both paths must report
// ErrAborted so the abort is not mistaken for an unrelated failure.
func (p *Player) roundStopped(ctx context.Context) error {
	p.mu.Lock()
	aborted := p.aborted
	p.mu.Unlock()
	if aborted {
		return ErrAborted
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (p *Player) noteMarkerError() {
	p.mu.Lock()
	p.stats.MarkerErrors++
	p.mu.Unlock()
}

func (p *Player) emit(ev Event) {
	if p.cfg.Observer == nil {
		return
	}
	p.cfg.Observer(ev)
}

func (p *Player) queueDepth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.depthLocked()
}

func (p *Player) depthLocked() int { return len(p.queue) - p.head }

// compactLocked discards the consumed prefix of the queue so total capacity
// stays bounded by roughly two times MaxQueue even across many pushes.
func (p *Player) compactLocked() {
	if p.head == 0 {
		return
	}
	if p.head < len(p.queue)/2 && p.head < p.cfg.MaxQueue {
		return
	}
	n := copy(p.queue, p.queue[p.head:])
	for i := n; i < len(p.queue); i++ {
		p.queue[i] = nil
	}
	p.queue = p.queue[:n]
	p.head = 0
}

func (p *Player) popLocked() []byte {
	f := p.queue[p.head]
	p.queue[p.head] = nil
	p.head++
	return f
}

func (p *Player) clearQueueLocked() {
	for i := range p.queue {
		p.queue[i] = nil
	}
	p.queue = p.queue[:0]
	p.head = 0
}

func (p *Player) signalLocked() {
	select {
	case p.notifyCh <- struct{}{}:
	default:
	}
}
