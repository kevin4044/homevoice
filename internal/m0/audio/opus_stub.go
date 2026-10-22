//go:build !cgo

package audio

// This build has no cgo, therefore no libopus. The probe fails explicitly
// instead of degrading to another codec or silently passing PCM through. The
// offline acceptance run requires CGO_ENABLED=1 (see the Makefile).

type opusEncoder struct{}

type opusDecoder struct {
	channels   int
	maxSamples int
}

// OpusVersion reports that the native codec is unavailable in this build.
func OpusVersion() string {
	return "unavailable: cgo disabled"
}

func opusEncoderNew(sampleRate, channels int) (*opusEncoder, error) {
	return nil, ErrOpusUnavailable
}

func (e *opusEncoder) encode(samples []int16, frameSamples int) ([]byte, error) {
	return nil, ErrOpusUnavailable
}

func (e *opusEncoder) close() {}

func opusDecoderNew(sampleRate, channels, maxSamples int) (*opusDecoder, error) {
	return nil, ErrOpusUnavailable
}

func (d *opusDecoder) decode(frame []byte) ([]int16, error) {
	return nil, ErrOpusUnavailable
}

func (d *opusDecoder) close() {}
