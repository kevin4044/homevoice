//go:build cgo

package audio

/*
#cgo CFLAGS: -I/opt/homebrew/include -I/usr/local/include
#cgo LDFLAGS: -L/opt/homebrew/lib -L/usr/local/lib -lopus

#include <stdlib.h>
#include <opus/opus.h>

// The request macros in opus_defines.h expand to (request, value) argument
// pairs, which are awkward to pass through cgo. Small wrappers keep the Go
// side readable and keep the cgo calls plain.
static int hv_opus_set_bitrate(OpusEncoder *st, opus_int32 bitrate) {
	return opus_encoder_ctl(st, OPUS_SET_BITRATE(bitrate));
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// targetBitrateBPS is a probe starting point for 60 ms voice frames
// (about 240 bytes per frame), comfortably inside one M0 UDP datagram. It is
// not a validated firmware or product parameter.
const targetBitrateBPS = 32000

// OpusVersion returns the version string of the linked libopus at runtime. It
// is used by the probe's selftest/report to record the actual native
// dependency instead of the build-time expectation.
func OpusVersion() string {
	return C.GoString(C.opus_get_version_string())
}

// opusEncoder is one continuous libopus encoder state.
type opusEncoder struct {
	ptr *C.OpusEncoder
}

func opusEncoderNew(sampleRate, channels int) (*opusEncoder, error) {
	if sampleRate <= 0 || channels < 1 || channels > 2 {
		return nil, fmt.Errorf("%w: encoder %d Hz %d channels", ErrInvalidParams, sampleRate, channels)
	}
	var code C.int
	ptr := C.opus_encoder_create(C.opus_int32(sampleRate), C.int(channels), C.OPUS_APPLICATION_VOIP, &code)
	if code != C.OPUS_OK || ptr == nil {
		return nil, opusError("opus_encoder_create", int(code))
	}
	e := &opusEncoder{ptr: ptr}
	if rc := C.hv_opus_set_bitrate(ptr, C.opus_int32(targetBitrateBPS)); rc != C.OPUS_OK {
		e.close()
		return nil, opusError("opus_encoder_ctl(OPUS_SET_BITRATE)", int(rc))
	}
	return e, nil
}

// encode encodes exactly frameSamples samples per channel. samples is
// interleaved and must contain frameSamples*channels values.
func (e *opusEncoder) encode(samples []int16, frameSamples int) ([]byte, error) {
	if e == nil || e.ptr == nil {
		return nil, ErrOpusUnavailable
	}
	if frameSamples <= 0 || len(samples) == 0 {
		return nil, fmt.Errorf("%w: encode with %d samples", ErrInvalidParams, len(samples))
	}
	out := make([]byte, MaxOpusPacketBytes)
	n := C.opus_encode(
		e.ptr,
		(*C.opus_int16)(unsafe.Pointer(&samples[0])),
		C.int(frameSamples),
		(*C.uchar)(unsafe.Pointer(&out[0])),
		C.opus_int32(len(out)),
	)
	if n < 0 {
		return nil, opusError("opus_encode", int(n))
	}
	if n == 0 {
		return nil, fmt.Errorf("audio: opus_encode produced an empty packet")
	}
	return out[:int(n)], nil
}

func (e *opusEncoder) close() {
	if e == nil || e.ptr == nil {
		return
	}
	C.opus_encoder_destroy(e.ptr)
	e.ptr = nil
}

// opusDecoder is one continuous libopus decoder state.
type opusDecoder struct {
	ptr        *C.OpusDecoder
	channels   int
	maxSamples int // per channel; 120 ms at the configured rate
}

func opusDecoderNew(sampleRate, channels, maxSamples int) (*opusDecoder, error) {
	if sampleRate <= 0 || channels < 1 || channels > 2 || maxSamples <= 0 {
		return nil, fmt.Errorf("%w: decoder %d Hz %d channels %d max samples", ErrInvalidParams, sampleRate, channels, maxSamples)
	}
	var code C.int
	ptr := C.opus_decoder_create(C.opus_int32(sampleRate), C.int(channels), &code)
	if code != C.OPUS_OK || ptr == nil {
		return nil, opusError("opus_decoder_create", int(code))
	}
	return &opusDecoder{ptr: ptr, channels: channels, maxSamples: maxSamples}, nil
}

// decode decodes one bounded Opus packet and returns interleaved PCM16
// samples. The result is copied out so the returned slice never aliases a
// reusable internal buffer.
func (d *opusDecoder) decode(frame []byte) ([]int16, error) {
	if d == nil || d.ptr == nil {
		return nil, ErrOpusUnavailable
	}
	if len(frame) == 0 {
		return nil, ErrEmptyPacket
	}
	pcm := make([]int16, d.maxSamples)
	n := C.opus_decode(
		d.ptr,
		(*C.uchar)(unsafe.Pointer(&frame[0])),
		C.opus_int32(len(frame)),
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])),
		C.int(d.maxSamples),
		0, // do not run packet-loss concealment
	)
	if n < 0 {
		return nil, opusError("opus_decode", int(n))
	}
	total := int(n) * d.channels
	out := make([]int16, total)
	copy(out, pcm[:total])
	return out, nil
}

func (d *opusDecoder) close() {
	if d == nil || d.ptr == nil {
		return
	}
	C.opus_decoder_destroy(d.ptr)
	d.ptr = nil
}

func opusError(op string, code int) error {
	msg := C.GoString(C.opus_strerror(C.int(code)))
	return fmt.Errorf("audio: %s failed: %s (code %d)", op, msg, code)
}
