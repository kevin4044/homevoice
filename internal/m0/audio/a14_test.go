package audio

import (
	"encoding/hex"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/fixtures"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

// TestA14SyntheticSineRoundTrip is the acceptance A14 check for generated
// sine PCM: encode/decode both directions, require non-empty bounded frames,
// finite (int16) samples with tone energy, and exact duration accounting.
func TestA14SyntheticSineRoundTrip(t *testing.T) {
	const frameCount = 10

	cases := []struct {
		name   string
		params Params
	}{
		{"uplink_16k_mono_60ms", Params{
			SampleRate:      limits.UplinkSampleRate,
			Channels:        limits.UplinkChannels,
			FrameDurationMS: limits.UplinkFrameDurationMS,
		}},
		{"downlink_24k_mono_60ms", Params{
			SampleRate:      limits.DownlinkSampleRate,
			Channels:        limits.DownlinkChannels,
			FrameDurationMS: limits.DownlinkFrameDurationMS,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := mustEncoder(t, tc.params)
			dec := mustDecoder(t, tc.params)
			frameSamples := tc.params.FrameSamples()
			if frameSamples <= 0 {
				t.Fatalf("Params.FrameSamples()=%d", frameSamples)
			}

			pcm := Sine(tc.params.SampleRate, 440, 0.5, frameSamples*frameCount)
			frames, err := enc.EncodeSamples(pcm)
			if err != nil {
				t.Fatalf("EncodeSamples: %v", err)
			}
			if len(frames) != frameCount {
				t.Fatalf("encoded %d frames, want %d", len(frames), frameCount)
			}
			for i, f := range frames {
				if len(f) == 0 {
					t.Fatalf("frame %d is empty", i)
				}
				if len(f) > MaxOpusPacketBytes {
					t.Fatalf("frame %d is %d bytes, over the Opus packet limit %d", i, len(f), MaxOpusPacketBytes)
				}
			}

			decoded := concatSamples(frames, t, dec)
			wantSamples := frameSamples * frameCount
			if got, want := len(decoded), wantSamples; got != want {
				t.Fatalf("decoded %d samples, want %d (tolerance %d)", got, want, DurationToleranceSamples)
			}
			wantDurationMS := frameCount * tc.params.FrameDurationMS
			gotDurationMS := len(decoded) * 1000 / tc.params.SampleRate
			if gotDurationMS != wantDurationMS {
				t.Fatalf("decoded duration %d ms, want %d ms (tolerance %d samples)",
					gotDurationMS, wantDurationMS, DurationToleranceSamples)
			}
			if p := peakAmplitude(decoded); p < sineMinPeak {
				t.Fatalf("decoded sine peak=%d, expected generated tone energy (>=%d)", p, sineMinPeak)
			}
			if r := rmsAmplitude(decoded); r < sineMinRMS {
				t.Fatalf("decoded sine RMS=%.1f, expected generated tone energy (>=%d)", r, sineMinRMS)
			}
		})
	}
}

// TestA14SilenceRoundTrip checks that a synthetic silence stream decodes to a
// bounded (near-zero) signal with the same duration accounting. Silence is a
// fresh stream: a hard tone-to-silence cut can ring in the codec, so the test
// does not demand zero samples on an abrupt transition.
func TestA14SilenceRoundTrip(t *testing.T) {
	const frameCount = 5
	params := downlinkParams()
	enc := mustEncoder(t, params)
	dec := mustDecoder(t, params)
	frameSamples := params.FrameSamples()

	frames, err := enc.EncodeSamples(Silence(frameSamples * frameCount))
	if err != nil {
		t.Fatalf("EncodeSamples: %v", err)
	}
	if len(frames) != frameCount {
		t.Fatalf("encoded %d silence frames, want %d", len(frames), frameCount)
	}
	decoded := concatSamples(frames, t, dec)
	if got, want := len(decoded), frameSamples*frameCount; got != want {
		t.Fatalf("decoded %d samples, want %d", got, want)
	}
	if p := peakAmplitude(decoded); p > silenceMaxPeak {
		t.Fatalf("decoded silence peak=%d, want <=%d", p, silenceMaxPeak)
	}
	if r := rmsAmplitude(decoded); r > silenceMaxRMS {
		t.Fatalf("decoded silence RMS=%.2f, want <=%.2f", r, silenceMaxRMS)
	}
}

// TestA14DirectionsIndependent proves uplink and downlink are separate codec
// instances with separate parameters, not one global configuration.
func TestA14DirectionsIndependent(t *testing.T) {
	up := uplinkParams()
	down := downlinkParams()
	if up.SampleRate == down.SampleRate || up.FrameSamples() == down.FrameSamples() {
		t.Fatalf("probe uplink/downlink params must differ, got %+v and %+v", up, down)
	}

	upEnc := mustEncoder(t, up)
	upDec := mustDecoder(t, up)
	downEnc := mustEncoder(t, down)
	downDec := mustDecoder(t, down)

	if got := upEnc.Params(); got != up {
		t.Fatalf("uplink encoder params=%+v want %+v", got, up)
	}
	if got := downEnc.Params(); got != down {
		t.Fatalf("downlink encoder params=%+v want %+v", got, down)
	}
	if got := upDec.Params(); got != up {
		t.Fatalf("uplink decoder params=%+v want %+v", got, up)
	}
	if got := downDec.Params(); got != down {
		t.Fatalf("downlink decoder params=%+v want %+v", got, down)
	}

	// Interleave the two streams: each keeps its own framing and state.
	for i := 0; i < 3; i++ {
		upFrames, err := upEnc.EncodeSamples(Sine(up.SampleRate, 220, 0.4, up.FrameSamples()))
		if err != nil {
			t.Fatalf("uplink EncodeSamples: %v", err)
		}
		downFrames, err := downEnc.EncodeSamples(Sine(down.SampleRate, 330, 0.4, down.FrameSamples()))
		if err != nil {
			t.Fatalf("downlink EncodeSamples: %v", err)
		}
		if len(upFrames) != 1 || len(downFrames) != 1 {
			t.Fatalf("round %d produced %d uplink / %d downlink frames, want 1 each", i, len(upFrames), len(downFrames))
		}
		upSamples, err := upDec.Decode(upFrames[0])
		if err != nil {
			t.Fatalf("uplink Decode: %v", err)
		}
		downSamples, err := downDec.Decode(downFrames[0])
		if err != nil {
			t.Fatalf("downlink Decode: %v", err)
		}
		if len(upSamples) != up.FrameSamples() {
			t.Fatalf("uplink decoded %d samples, want %d", len(upSamples), up.FrameSamples())
		}
		if len(downSamples) != down.FrameSamples() {
			t.Fatalf("downlink decoded %d samples, want %d", len(downSamples), down.FrameSamples())
		}
	}
}

// TestA14FixtureCiphertextIsNotAudio proves the public UDP fixture ciphertext is
// never accepted as a valid audio frame by the codec layer: an empty payload is
// not audio, oversized input is rejected, and decoding fixture bytes either
// errors or yields bounded samples without panicking. The fixture bytes are
// AES-CTR test data, not Opus, and must not be routed to playback by the
// pipeline.
func TestA14FixtureCiphertextIsNotAudio(t *testing.T) {
	suite, err := fixtures.LoadUDPVectors()
	if err != nil {
		t.Fatalf("load UDP vectors: %v", err)
	}
	dec := mustDecoder(t, downlinkParams())
	maxSamples := maxPacketSamples(downlinkParams().SampleRate) * downlinkParams().Channels

	for _, v := range suite.Vectors {
		ciphertext, cipherErr := hex.DecodeString(v.CiphertextHex)
		packet, packetErr := hex.DecodeString(v.PacketHex)
		if cipherErr != nil || packetErr != nil {
			// Some intentionally malformed fixture vectors carry odd-length
			// hex on purpose. They are not byte-level codec inputs, so record
			// and skip instead of rewriting the fixture.
			t.Logf("vector %s has non-hex fixture bytes (ciphertext=%v, packet=%v); skipping byte-level probe",
				v.ID, cipherErr, packetErr)
			continue
		}
		for name, raw := range map[string][]byte{"ciphertext": ciphertext, "udp_packet": packet} {
			if len(raw) == 0 {
				// Fixture vectors without a ciphertext payload are
				// packaging-only cases: an empty payload is packet-loss
				// concealment, not received audio.
				if _, err := dec.Decode(raw); err == nil {
					t.Fatalf("vector %s: empty payload must not decode as audio", v.ID)
				}
				continue
			}
			samples, err := dec.Decode(raw)
			if err != nil {
				continue // rejecting non-Opus bytes is the expected outcome
			}
			if len(samples) == 0 || len(samples) > maxSamples {
				t.Fatalf("vector %s %s decoded %d samples, want 1..%d or an error", v.ID, name, len(samples), maxSamples)
			}
		}
	}

	if _, err := dec.Decode(nil); err == nil {
		t.Fatal("Decode(nil) must fail: an empty packet is not audio")
	}
	if _, err := dec.Decode(make([]byte, MaxOpusPacketBytes+1)); err == nil {
		t.Fatal("Decode must reject packets larger than 1275 bytes")
	}
}

// TestA14DecoderRobustness feeds random and malformed bytes to the decoder and
// requires bounded output or an error, never a panic.
func TestA14DecoderRobustness(t *testing.T) {
	params := uplinkParams()
	dec := mustDecoder(t, params)
	maxSamples := maxPacketSamples(params.SampleRate) * params.Channels

	inputs := [][]byte{
		{0x00},
		{0xff},
		{0x4f, 0x70, 0x75, 0x73},
		make([]byte, 3),
	}
	// Deterministic pseudo-random payloads; no external randomness needed for
	// a boundedness check.
	seed := uint32(0x9e3779b9)
	for i := 0; i < 128; i++ {
		n := i%64 + 1
		buf := make([]byte, n)
		for j := range buf {
			seed = seed*1664525 + 1013904223
			buf[j] = byte(seed >> 24)
		}
		inputs = append(inputs, buf)
	}
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Decode panicked on input %d (%d bytes): %v", i, len(in), r)
				}
			}()
			samples, err := dec.Decode(in)
			if err != nil {
				return
			}
			if len(samples) == 0 || len(samples) > maxSamples {
				t.Fatalf("input %d decoded %d samples outside 1..%d", i, len(samples), maxSamples)
			}
		}()
	}
}
