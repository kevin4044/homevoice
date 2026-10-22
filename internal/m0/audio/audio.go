package audio

import (
	"errors"
	"fmt"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// Opus packet and frame bounds. OPUS_MAX_PACKET is 1275 bytes (RFC 6716); a
// 120 ms frame is the longest legal Opus packet duration.
const (
	// MaxOpusPacketBytes is the largest Opus packet accepted or produced.
	MaxOpusPacketBytes = 1275

	// MaxPacketDurationMS is the longest legal Opus packet duration.
	MaxPacketDurationMS = 120

	// DurationToleranceSamples is the allowed deviation between the number of
	// input and decoded samples for the fixed-frame streaming API. The probe
	// frames input exactly (each Opus frame consumes FrameSamples() samples per
	// channel and decoding returns the same count), so the allowed deviation is
	// zero samples. Encoded bytes are not compared against the input PCM or a
	// fixed reference vector.
	DurationToleranceSamples = 0
)

// Errors reported by the codec layer. They are wrapped with context where a
// caller needs the underlying detail, but the sentinels remain matchable with
// errors.Is.
var (
	// ErrInvalidParams reports codec parameters Opus or the probe does not
	// support.
	ErrInvalidParams = errors.New("audio: invalid codec params")

	// ErrOddPCMBytes reports an odd-length PCM16LE byte slice. A half sample is
	// an explicit error, never silently dropped.
	ErrOddPCMBytes = errors.New("audio: odd-length PCM16LE byte count (expected whole 16-bit samples)")

	// ErrEncoderClosed reports use of an Encoder after Close.
	ErrEncoderClosed = errors.New("audio: encoder closed")

	// ErrEncoderFailed reports use of an Encoder after an unrecoverable codec
	// or tail-validation error. A new encoder must be created; the probe must
	// not continue a desynchronised stream.
	ErrEncoderFailed = errors.New("audio: encoder failed; create a new encoder")

	// ErrDecoderClosed reports use of a Decoder after Close.
	ErrDecoderClosed = errors.New("audio: decoder closed")

	// ErrNotInitialized reports use of a zero-value codec; constructors must
	// be used.
	ErrNotInitialized = errors.New("audio: zero-value codec; use NewEncoder or NewDecoder")

	// ErrEmptyPacket reports an empty Opus packet. Decoding an empty packet is
	// packet-loss concealment, i.e. synthesized audio, not a received frame, so
	// the probe rejects it instead of pretending audio was decoded.
	ErrEmptyPacket = errors.New("audio: empty Opus packet is not audio")

	// ErrPacketTooLarge reports a packet larger than MaxOpusPacketBytes.
	ErrPacketTooLarge = errors.New("audio: Opus packet exceeds 1275-byte limit")

	// ErrOpusUnavailable reports that the build has no usable libopus binding
	// (for example CGO_ENABLED=0). The probe must fail loudly rather than
	// degrade to another codec.
	ErrOpusUnavailable = errors.New("audio: libopus unavailable; build with CGO_ENABLED=1 and libopus installed")
)

// Params describes one codec direction. The probe creates separate instances
// for uplink and downlink; there is no global audio configuration.
//
// FrameSamples is measured per channel, matching the Opus frame_size
// convention. For the mono M0 formats it is also the number of samples in one
// frame.
type Params struct {
	SampleRate      int // samples per second per channel (8000/12000/16000/24000/48000)
	Channels        int // 1 (probe) or 2, interleaved PCM
	FrameDurationMS int // Opus frame duration in whole milliseconds
}

// FrameSamples returns the number of samples per channel in one frame, or 0
// when the parameters are invalid (constructors reject those).
func (p Params) FrameSamples() int {
	if p.SampleRate <= 0 || p.FrameDurationMS <= 0 {
		return 0
	}
	// Every supported Opus sample rate is a multiple of 1000/FrameDurationMS
	// inputs; integer division is exact for the validated set.
	return p.SampleRate * p.FrameDurationMS / 1000
}

// Validate reports whether Opus and the probe support these parameters. It is
// exported so callers can validate configuration without constructing codecs.
func (p Params) Validate() error {
	switch p.SampleRate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return fmt.Errorf("%w: unsupported sample rate %d Hz", ErrInvalidParams, p.SampleRate)
	}
	if p.Channels != 1 && p.Channels != 2 {
		return fmt.Errorf("%w: unsupported channel count %d (mono or stereo)", ErrInvalidParams, p.Channels)
	}
	// opus_encode() accepts exactly one of these frame durations; longer packet
	// durations cannot be produced by the streaming encoder this package wraps.
	switch p.FrameDurationMS {
	case 5, 10, 20, 40, 60:
	default:
		return fmt.Errorf("%w: unsupported frame duration %d ms", ErrInvalidParams, p.FrameDurationMS)
	}
	if p.FrameSamples() <= 0 {
		return fmt.Errorf("%w: frame duration produces no samples", ErrInvalidParams)
	}
	return nil
}

// UplinkParams returns the candidate M0 uplink format (16 kHz mono 60 ms).
// Uplink and downlink codec instances are always separate.
func UplinkParams() Params {
	return Params{
		SampleRate:      limits.UplinkSampleRate,
		Channels:        limits.UplinkChannels,
		FrameDurationMS: limits.UplinkFrameDurationMS,
	}
}

// DownlinkParams returns the candidate M0 downlink format (24 kHz mono 60 ms).
func DownlinkParams() Params {
	return Params{
		SampleRate:      limits.DownlinkSampleRate,
		Channels:        limits.DownlinkChannels,
		FrameDurationMS: limits.DownlinkFrameDurationMS,
	}
}

// maxPacketSamples returns the largest per-channel sample count a legal Opus
// packet can decode to at sampleRate.
func maxPacketSamples(sampleRate int) int {
	if sampleRate <= 0 {
		return 0
	}
	return sampleRate * MaxPacketDurationMS / 1000
}
