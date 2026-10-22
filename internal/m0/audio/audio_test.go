package audio

import (
	"math"
	"strings"
	"testing"
)

// Test helpers shared by the A14/A15 round-trip tests. The energy helpers are
// synthetic sanity checks, not audio quality metrics.

const (
	// Synthetic signal sanity bounds. A generated 440 Hz tone at amplitude 0.5
	// decodes at roughly 18k peak / 11k RMS with libopus 1.6.1. A silence
	// stream decodes at peak <= 2. These loose bounds only distinguish
	// "tone present" from "silence", they do not claim perceptual quality.
	sineMinPeak    = 4000
	sineMinRMS     = 2000
	silenceMaxPeak = 64
	silenceMaxRMS  = 16.0
)

func peakAmplitude(samples []int16) int {
	m := 0
	for _, v := range samples {
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a > m {
			m = a
		}
	}
	return m
}

func rmsAmplitude(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(samples)))
}

func mustEncoder(t *testing.T, p Params) *Encoder {
	t.Helper()
	enc, err := NewEncoder(p)
	if err != nil {
		t.Fatalf("NewEncoder(%+v): %v", p, err)
	}
	t.Cleanup(func() { enc.Close() })
	return enc
}

func mustDecoder(t *testing.T, p Params) *Decoder {
	t.Helper()
	dec, err := NewDecoder(p)
	if err != nil {
		t.Fatalf("NewDecoder(%+v): %v", p, err)
	}
	t.Cleanup(func() { dec.Close() })
	return dec
}

func uplinkParams() Params   { return UplinkParams() }
func downlinkParams() Params { return DownlinkParams() }

func concatSamples(frames [][]byte, t *testing.T, dec *Decoder) []int16 {
	t.Helper()
	var out []int16
	for i, f := range frames {
		decoded, err := dec.Decode(f)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		out = append(out, decoded...)
	}
	return out
}

func encodeChunks(enc *Encoder, samples []int16, sizes []int) ([][]byte, error) {
	var frames [][]byte
	pos := 0
	for i := 0; pos < len(samples); i++ {
		n := sizes[i%len(sizes)]
		if n <= 0 {
			continue
		}
		if pos+n > len(samples) {
			n = len(samples) - pos
		}
		produced, err := enc.EncodeSamples(samples[pos : pos+n])
		if err != nil {
			return frames, err
		}
		frames = append(frames, produced...)
		pos += n
	}
	return frames, nil
}

func TestOpusVersion(t *testing.T) {
	v := OpusVersion()
	t.Logf("runtime libopus version: %q", v)
	if v == "" {
		t.Fatal("OpusVersion() returned empty string")
	}
	if !strings.HasPrefix(v, "libopus") {
		// libopus reports "libopus x.y.z"; anything else means the binding is
		// not talking to the expected native library.
		t.Fatalf("unexpected libopus version string %q", v)
	}
}

func TestFrameSamples(t *testing.T) {
	cases := []struct {
		p    Params
		want int
	}{
		{Params{SampleRate: 16000, Channels: 1, FrameDurationMS: 60}, 960},
		{Params{SampleRate: 24000, Channels: 1, FrameDurationMS: 60}, 1440},
		{Params{SampleRate: 8000, Channels: 1, FrameDurationMS: 20}, 160},
		{Params{SampleRate: 48000, Channels: 2, FrameDurationMS: 120}, 5760},
		{Params{}, 0},
		{Params{SampleRate: -1, Channels: 1, FrameDurationMS: 60}, 0},
	}
	for _, tc := range cases {
		if got := tc.p.FrameSamples(); got != tc.want {
			t.Errorf("Params%+v.FrameSamples()=%d want %d", tc.p, got, tc.want)
		}
	}
}

func TestParamsValidate(t *testing.T) {
	valid := []Params{
		{SampleRate: 8000, Channels: 1, FrameDurationMS: 60},
		{SampleRate: 16000, Channels: 1, FrameDurationMS: 60},
		{SampleRate: 24000, Channels: 2, FrameDurationMS: 20},
		{SampleRate: 48000, Channels: 1, FrameDurationMS: 40},
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("expected valid %+v: %v", p, err)
		}
	}
	invalid := []Params{
		{SampleRate: 0, Channels: 1, FrameDurationMS: 60},
		{SampleRate: 44100, Channels: 1, FrameDurationMS: 60},
		{SampleRate: 16000, Channels: 0, FrameDurationMS: 60},
		{SampleRate: 16000, Channels: 3, FrameDurationMS: 60},
		{SampleRate: 16000, Channels: 1, FrameDurationMS: 0},
		{SampleRate: 16000, Channels: 1, FrameDurationMS: 15},
		{SampleRate: 16000, Channels: 1, FrameDurationMS: 120},
		{SampleRate: 16000, Channels: 1, FrameDurationMS: 200},
	}
	for _, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Errorf("expected invalid %+v", p)
		}
	}
}

func TestSineAndSilenceSyntheticInput(t *testing.T) {
	sine := Sine(16000, 440, 0.5, 1600)
	if len(sine) != 1600 {
		t.Fatalf("Sine length=%d want 1600", len(sine))
	}
	p := peakAmplitude(sine)
	if p < sineMinPeak {
		t.Fatalf("Sine peak=%d, expected a generated tone", p)
	}
	if p > 32767 {
		t.Fatalf("Sine peak %d out of int16 range", p)
	}
	// Amplitude clamping makes loud and malformed requests safe.
	if got := peakAmplitude(Sine(16000, 440, 99, 1600)); got != 32767 {
		t.Errorf("clamped amplitude peak=%d want 32767", got)
	}
	if got := peakAmplitude(Sine(16000, 440, math.NaN(), 1600)); got != 0 {
		t.Errorf("NaN amplitude peak=%d want 0", got)
	}
	if Sine(0, 440, 0.5, 10) != nil || Sine(16000, 440, 0.5, 0) != nil {
		t.Error("Sine with invalid rate/length should return nil")
	}

	sil := Silence(960)
	if len(sil) != 960 {
		t.Fatalf("Silence length=%d want 960", len(sil))
	}
	if peakAmplitude(sil) != 0 {
		t.Fatal("Silence must be all zeros")
	}
	if Silence(0) != nil || Silence(-5) != nil {
		t.Error("Silence with non-positive length should return nil")
	}
}

func TestZeroValueAndCloseGuards(t *testing.T) {
	var enc Encoder
	if _, err := enc.EncodeSamples([]int16{0, 0}); err == nil {
		t.Fatal("zero-value Encoder.EncodeSamples should fail")
	}
	if _, err := enc.Flush(); err == nil {
		t.Fatal("zero-value Encoder.Flush should fail")
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("nil/zero-value Close should be safe: %v", err)
	}

	var dec Decoder
	if _, err := dec.Decode([]byte{0x01}); err == nil {
		t.Fatal("zero-value Decoder.Decode should fail")
	}
	if err := dec.Close(); err != nil {
		t.Fatalf("nil/zero-value Close should be safe: %v", err)
	}

	e := mustEncoder(t, uplinkParams())
	d := mustDecoder(t, uplinkParams())
	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close should be idempotent: %v", err)
	}
	if _, err := e.EncodeSamples(make([]int16, 960)); err == nil {
		t.Fatal("EncodeSamples after Close should fail")
	}
	if _, err := e.Flush(); err == nil {
		t.Fatal("Flush after Close should fail")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("decoder Close: %v", err)
	}
	if _, err := d.Decode([]byte{0x01, 0x02}); err == nil {
		t.Fatal("Decode after Close should fail")
	}
}

// TestValidatedParamsEncodeDecode exercises every parameter combination that
// Params.Validate accepts, so the advertised parameter set is backed by real
// libopus calls rather than assumption.
func TestValidatedParamsEncodeDecode(t *testing.T) {
	rates := []int{8000, 12000, 16000, 24000, 48000}
	durations := []int{5, 10, 20, 40, 60}
	for _, rate := range rates {
		for _, channels := range []int{1, 2} {
			for _, duration := range durations {
				p := Params{SampleRate: rate, Channels: channels, FrameDurationMS: duration}
				if err := p.Validate(); err != nil {
					t.Fatalf("Validate(%+v): %v", p, err)
				}
				enc := mustEncoder(t, p)
				dec := mustDecoder(t, p)
				frameSamples := p.FrameSamples()
				pcm := Sine(rate, 440, 0.4, frameSamples*channels)
				frames, err := enc.EncodeSamples(pcm)
				if err != nil {
					t.Fatalf("%+v: EncodeSamples: %v", p, err)
				}
				if len(frames) != 1 {
					t.Fatalf("%+v: produced %d frames, want 1", p, len(frames))
				}
				if len(frames[0]) == 0 || len(frames[0]) > MaxOpusPacketBytes {
					t.Fatalf("%+v: encoded frame size %d", p, len(frames[0]))
				}
				decoded, err := dec.Decode(frames[0])
				if err != nil {
					t.Fatalf("%+v: Decode: %v", p, err)
				}
				if got, want := len(decoded), frameSamples*channels; got != want {
					t.Fatalf("%+v: decoded %d samples, want %d", p, got, want)
				}
			}
		}
	}
}
