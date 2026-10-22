package audio

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// Encoder is a streaming Opus encoder with one continuous codec state. It is
// safe for concurrent use within one direction, but callers normally feed it
// from a single stream goroutine.
//
// Input chunks may be of any length and need not be frame-aligned: the
// samples of an incomplete frame are retained as a bounded residual until the
// next call. A zero-value Encoder must not be used; construct one with
// NewEncoder. Encoder must not be copied after first use.
type Encoder struct {
	mu sync.Mutex

	params       Params
	frameSamples int // per channel
	frameLen     int // interleaved samples per frame
	enc          *opusEncoder
	tailVerify   *opusDecoder // validates the padded frame produced by Flush
	residual     []int16
	closed       bool
	failed       bool
}

// NewEncoder creates an encoder for one direction. Uplink and downlink must use
// separate instances and parameters.
func NewEncoder(p Params) (*Encoder, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("audio: NewEncoder: %w", err)
	}
	frameSamples := p.FrameSamples()
	enc, err := opusEncoderNew(p.SampleRate, p.Channels)
	if err != nil {
		return nil, fmt.Errorf("audio: NewEncoder: %w", err)
	}
	verify, err := opusDecoderNew(p.SampleRate, p.Channels, maxPacketSamples(p.SampleRate))
	if err != nil {
		enc.close()
		return nil, fmt.Errorf("audio: NewEncoder tail validator: %w", err)
	}
	return &Encoder{
		params:       p,
		frameSamples: frameSamples,
		frameLen:     frameSamples * p.Channels,
		enc:          enc,
		tailVerify:   verify,
		residual:     make([]int16, 0, frameSamples*p.Channels),
	}, nil
}

// Params returns the codec parameters this encoder was created with.
func (e *Encoder) Params() Params {
	if e == nil {
		return Params{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.params
}

// EncodeSamples feeds interleaved PCM16 samples and returns the complete Opus
// frames produced. It may return no frames while it buffers a residual tail;
// callers must keep calling it and then call Flush when the input ends. The
// residual buffer is always strictly smaller than one frame.
func (e *Encoder) EncodeSamples(samples []int16) ([][]byte, error) {
	if e == nil || e.enc == nil {
		return nil, ErrNotInitialized
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrEncoderClosed
	}
	if e.failed {
		return nil, ErrEncoderFailed
	}
	if len(samples) == 0 {
		return nil, nil
	}
	frames, err := e.encodeLocked(samples)
	if err != nil {
		e.failed = true
		return nil, err
	}
	return frames, nil
}

// encodeLocked consumes samples without copying the whole chunk: it only
// retains the final incomplete frame in residual. Caller holds e.mu and has
// checked the closed/failed state.
func (e *Encoder) encodeLocked(samples []int16) ([][]byte, error) {
	var frames [][]byte
	pos := 0

	// Complete a frame carried over from a previous call first.
	if len(e.residual) > 0 {
		need := e.frameLen - len(e.residual)
		if need > len(samples) {
			e.residual = append(e.residual, samples...)
			return nil, nil
		}
		e.residual = append(e.residual, samples[:need]...)
		frame, err := e.encodeFrameLocked(e.residual)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		e.residual = e.residual[:0]
		pos = need
	}

	for len(samples)-pos >= e.frameLen {
		frame, err := e.encodeFrameLocked(samples[pos : pos+e.frameLen])
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		pos += e.frameLen
	}
	if pos < len(samples) {
		e.residual = append(e.residual[:0], samples[pos:]...)
	}
	return frames, nil
}

func (e *Encoder) encodeFrameLocked(frame []int16) ([]byte, error) {
	out, err := e.enc.encode(frame, e.frameSamples)
	if err != nil {
		return nil, fmt.Errorf("audio: encode frame: %w", err)
	}
	return out, nil
}

// EncodePCM feeds PCM16 little-endian bytes. An odd byte count is an explicit
// error (ErrOddPCMBytes) rather than a silently dropped half sample. Empty
// input is a valid no-op that returns no frames.
func (e *Encoder) EncodePCM(pcm16le []byte) ([][]byte, error) {
	if len(pcm16le)%2 != 0 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrOddPCMBytes, len(pcm16le))
	}
	if e == nil || e.enc == nil {
		return nil, ErrNotInitialized
	}
	if len(pcm16le) == 0 {
		return e.EncodeSamples(nil)
	}
	samples := make([]int16, len(pcm16le)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm16le[2*i:]))
	}
	return e.EncodeSamples(samples)
}

// Flush completes the current partial frame. The documented tail strategy is:
// pad the residual samples with silence up to exactly one frame, encode that
// frame, decode it again and require exactly one frame of samples. It returns
// that single frame, or no frames when no residual is buffered.
//
// Flush is idempotent (a second call with no new input returns no frames) and
// it does not reset the codec: subsequent EncodeSamples calls continue the same
// Opus stream. The padding adds between 0 and FrameSamples()-1 silent samples.
func (e *Encoder) Flush() ([][]byte, error) {
	if e == nil || e.enc == nil {
		return nil, ErrNotInitialized
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrEncoderClosed
	}
	if e.failed {
		return nil, ErrEncoderFailed
	}
	if len(e.residual) == 0 {
		return nil, nil
	}
	tail := make([]int16, e.frameLen)
	copy(tail, e.residual)
	e.residual = e.residual[:0]

	frame, err := e.encodeFrameLocked(tail)
	if err != nil {
		e.failed = true
		return nil, fmt.Errorf("audio: flush: %w", err)
	}
	// Validate the tail frame against a real decoder. A tail frame that does
	// not decode to exactly one frame would desynchronise the receiver.
	decoded, err := e.tailVerify.decode(frame)
	if err != nil {
		e.failed = true
		return nil, fmt.Errorf("audio: flush tail validation: %w", err)
	}
	if len(decoded) != e.frameLen {
		e.failed = true
		return nil, fmt.Errorf("audio: flush tail validation: decoded %d samples, want %d", len(decoded), e.frameLen)
	}
	return [][]byte{frame}, nil
}

// Close releases the native codec. It is idempotent and safe on a nil
// receiver. The encoder must not be used afterwards.
func (e *Encoder) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.residual = nil
	e.enc.close()
	e.tailVerify.close()
	return nil
}
