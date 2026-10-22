// Package audio is the M0 probe's synthetic PCM <-> Opus codec layer.
//
// # Scope and provenance
//
// Input is generated on the fly by Sine and Silence; no recording, firmware
// capture or third-party audio is embedded in the repository. The UDP fixtures
// under docs/milestones/M0-transport-probe/fixtures contain AES-CTR test data,
// which is deliberately not valid Opus. That ciphertext (and random bytes) must
// never be routed to the decoder as audio; the decoder still treats every
// packet as untrusted and bounded.
//
// # Native dependency
//
// The codec is libopus, linked through a thin hand-written cgo binding because
// no pkg-config binary is available. The probe pins libopus 1.6.1 as installed
// by Homebrew on Apple silicon under /opt/homebrew (headers in
// /opt/homebrew/include/opus/opus.h, library /opt/homebrew/lib/libopus.dylib).
// The cgo directives also probe /usr/local and the platform defaults for other
// hosts. libopus is BSD-licensed. There is no pure-Go fallback: if cgo is
// disabled or libopus cannot be linked, the build fails or the constructors
// return ErrOpusUnavailable; audio is never silently replaced by PCM
// passthrough or another codec. OpusVersion reports the runtime library
// version for the selftest/report.
//
// # Uplink and downlink are separate
//
// The M0 candidate formats are an uplink of 16 kHz mono 60 ms frames and a
// downlink of 24 kHz mono 60 ms frames (see internal/m0/limits). Callers must
// create one Encoder/Decoder pair per direction; the package deliberately has
// no global format. Each Encoder owns one continuous libopus encoder state and
// each Decoder owns one continuous libopus decoder state.
//
// # Streaming and tail behaviour
//
// Encoder accepts PCM16 little-endian at arbitrary, possibly uneven, chunk
// sizes. Samples that do not complete a frame are retained as a bounded
// residual (strictly fewer interleaved samples than one frame); they are not
// dropped and no unbounded input is buffered. EncodePCM rejects an odd number
// of bytes with ErrOddPCMBytes instead of silently discarding a half sample.
// Flush pads the residual final frame with silence, encodes it, validates that
// decoding it yields exactly one frame of samples, and returns that single
// frame. Flush with no residual returns no frames, making it idempotent; it does
// not destroy or reset the encoder, so a later EncodeSamples call continues the
// same stream. The amount of trailing silence added by Flush is in
// [0, FrameSamples()-1] and is visible in the returned frame count.
//
// The encoder emits at most MaxOpusPacketBytes (1275) per frame. The decoder
// rejects empty packets (not audio) and packets larger than 1275 bytes, and
// never panics on malformed or random input: it either returns an explicit
// error or a bounded number of decoded samples (at most 120 ms worth, the
// longest legal Opus packet).
//
// # Tolerances for the judge
//
// Opus is lossy and version-dependent, so tests never compare encoded bytes
// against a fixed vector or against the input PCM, and never make quality
// claims. The one byte-for-byte comparison used by the A15 streaming test is
// between two encoders fed identical PCM in the same process and library; it
// detects lost samples or lost streaming state, not audio quality. Duration
// accounting is exact for fixed-frame streams: one frame of input is consumed
// per Opus frame and decoding returns the same number of samples, hence
// DurationToleranceSamples is 0. The codec's
// algorithmic delay (libopus lookahead, on the order of a few milliseconds)
// appears as latency in a real audio path, not as lost samples or shortened
// duration in this API. Energy assertions in the package tests are synthetic
// sanity bounds that distinguish a generated tone from silence; they are not a
// perceptual quality metric. The probe's UDP fixtures and reports must still
// record the native library version, because lossy output can change between
// libopus releases.
package audio
