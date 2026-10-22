package audio

import (
	"fmt"
	"sync"
)

// Decoder is a streaming Opus decoder with one continuous codec state. It is
// safe for concurrent use; incoming packets are decoded in call order.
//
// Decode is defensive with untrusted input: it rejects empty packets and
// packets larger than MaxOpusPacketBytes, and decodes any remaining bytes into
// a bounded buffer (at most 120 ms, the longest legal Opus packet). Malformed
// bytes either return an error or produce bounded garbage; Decode never panics,
// and the caller must not treat decoded audio as authenticated (M0 UDP has no
// MAC/AEAD). A zero-value Decoder must not be used; construct one with
// NewDecoder. Decoder must not be copied after first use.
type Decoder struct {
	mu sync.Mutex

	params Params
	dec    *opusDecoder
	closed bool
}

// NewDecoder creates a decoder for one direction. Uplink and downlink must use
// separate instances and parameters.
func NewDecoder(p Params) (*Decoder, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("audio: NewDecoder: %w", err)
	}
	dec, err := opusDecoderNew(p.SampleRate, p.Channels, maxPacketSamples(p.SampleRate))
	if err != nil {
		return nil, fmt.Errorf("audio: NewDecoder: %w", err)
	}
	return &Decoder{params: p, dec: dec}, nil
}

// Params returns the codec parameters this decoder was created with.
func (d *Decoder) Params() Params {
	if d == nil {
		return Params{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.params
}

// Decode decodes exactly one Opus packet and returns interleaved PCM16 samples
// at the decoder's sample rate. Empty packets are not audio and return
// ErrEmptyPacket; packets larger than MaxOpusPacketBytes return
// ErrPacketTooLarge. The returned slice is a copy and never aliases decoder
// state.
func (d *Decoder) Decode(frame []byte) ([]int16, error) {
	if d == nil || d.dec == nil {
		return nil, ErrNotInitialized
	}
	if len(frame) == 0 {
		return nil, ErrEmptyPacket
	}
	if len(frame) > MaxOpusPacketBytes {
		return nil, fmt.Errorf("%w: got %d bytes", ErrPacketTooLarge, len(frame))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, ErrDecoderClosed
	}
	return d.dec.decode(frame)
}

// Close releases the native decoder. It is idempotent and safe on a nil
// receiver.
func (d *Decoder) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.dec.close()
	return nil
}
