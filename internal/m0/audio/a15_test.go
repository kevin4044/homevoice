package audio

import (
	"errors"
	"testing"
)

// TestA15UnevenChunksNoSampleLoss is the acceptance A15 check for streaming
// input: arbitrary uneven chunk sizes must produce exactly the same Opus
// frames as one-shot input (state and residual preserved), and the final
// partial frame must be completed by Flush with exactly one validated frame.
func TestA15UnevenChunksNoSampleLoss(t *testing.T) {
	for _, params := range []Params{uplinkParams(), downlinkParams()} {
		t.Run(paramsName(params), func(t *testing.T) {
			frameSamples := params.FrameSamples()
			total := frameSamples*7 + 321 // 7 full frames plus an uneven tail

			// One-shot reference stream.
			oneShot := mustEncoder(t, params)
			ref, err := oneShot.EncodeSamples(Sine(params.SampleRate, 523, 0.45, total))
			if err != nil {
				t.Fatalf("one-shot EncodeSamples: %v", err)
			}
			if len(ref) != 7 {
				t.Fatalf("one-shot encoded %d full frames, want 7", len(ref))
			}

			// Same samples in deliberately uneven chunks, including single
			// samples and chunks larger than a frame.
			chunked := mustEncoder(t, params)
			sizes := []int{1, 7, frameSamples - 1, 3*frameSamples + 17, 2, 1234, 5, 1}
			frames, err := encodeChunks(chunked, Sine(params.SampleRate, 523, 0.45, total), sizes)
			if err != nil {
				t.Fatalf("chunked EncodeSamples: %v", err)
			}
			if len(frames) != len(ref) {
				t.Fatalf("chunked encoded %d frames, want %d", len(frames), len(ref))
			}
			// Byte equality is used only between two encoders fed the same PCM
			// with the same codec version: it detects lost samples or lost
			// streaming state. It is not a quality claim or a fixed Opus vector.
			for i := range ref {
				if string(frames[i]) != string(ref[i]) {
					t.Fatalf("frame %d differs between one-shot and chunked encoding; streaming state or residual was lost", i)
				}
			}

			// The residual must be completed as one padded, validated tail frame.
			tail, err := chunked.Flush()
			if err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if len(tail) != 1 {
				t.Fatalf("Flush produced %d frames, want exactly 1 padded tail frame", len(tail))
			}
			refTail, err := oneShot.Flush()
			if err != nil {
				t.Fatalf("reference Flush: %v", err)
			}
			if len(refTail) != 1 || string(tail[0]) != string(refTail[0]) {
				t.Fatal("tail frame differs between one-shot and chunked encoding")
			}

			dec := mustDecoder(t, params)
			decoded := concatSamples(append(frames, tail...), t, dec)
			wantFrames := 8 // ceil((7 frames + 321 samples) / frame)
			if got, want := len(decoded), wantFrames*frameSamples; got != want {
				t.Fatalf("decoded %d samples, want %d (tolerance %d)", got, want, DurationToleranceSamples)
			}
			if len(decoded) < total {
				t.Fatalf("decoded %d samples, fewer than the %d input samples; samples were lost", len(decoded), total)
			}
			if pad := len(decoded) - total; pad >= frameSamples {
				t.Fatalf("tail padding %d samples, want fewer than one frame (%d)", pad, frameSamples)
			}
			if p := peakAmplitude(decoded); p < sineMinPeak {
				t.Fatalf("decoded sine peak=%d, expected tone energy after decode", p)
			}
		})
	}
}

// TestA15OddByteCountError proves an odd PCM16LE byte count is an explicit,
// matchable error rather than a silently dropped half sample, and that the
// encoder remains usable afterwards.
func TestA15OddByteCountError(t *testing.T) {
	params := uplinkParams()
	enc := mustEncoder(t, params)

	for _, n := range []int{1, 3, 5, 2*params.FrameSamples() + 1} {
		frames, err := enc.EncodePCM(make([]byte, n))
		if !errors.Is(err, ErrOddPCMBytes) {
			t.Fatalf("EncodePCM(%d bytes) error=%v, want ErrOddPCMBytes", n, err)
		}
		if len(frames) != 0 {
			t.Fatalf("EncodePCM(%d odd bytes) returned %d frames despite error", n, len(frames))
		}
	}

	// Even input still works after the rejected calls.
	frames, err := enc.EncodePCM(make([]byte, 2*params.FrameSamples()))
	if err != nil {
		t.Fatalf("EncodePCM even bytes after odd errors: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("EncodePCM even bytes produced %d frames, want 1", len(frames))
	}
	empty, err := enc.EncodePCM(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("EncodePCM(nil)=(%d frames, %v), want no frames and no error", len(empty), err)
	}
}

// TestA15FlushTailStrategy checks the documented tail strategy: pad the final
// partial frame with silence, encode exactly one frame and validate its decoded
// size; Flush is idempotent and does not reset the stream.
func TestA15FlushTailStrategy(t *testing.T) {
	params := downlinkParams()
	frameSamples := params.FrameSamples()
	enc := mustEncoder(t, params)
	dec := mustDecoder(t, params)

	// Two full frames plus 100 samples.
	frames, err := enc.EncodeSamples(Sine(params.SampleRate, 300, 0.5, 2*frameSamples+100))
	if err != nil {
		t.Fatalf("EncodeSamples: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("encoded %d full frames, want 2", len(frames))
	}

	tail, err := enc.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(tail) != 1 {
		t.Fatalf("Flush returned %d frames, want 1", len(tail))
	}
	if len(tail[0]) == 0 || len(tail[0]) > MaxOpusPacketBytes {
		t.Fatalf("tail frame size %d outside 1..%d", len(tail[0]), MaxOpusPacketBytes)
	}
	tailSamples, err := dec.Decode(tail[0])
	if err != nil {
		t.Fatalf("decode tail: %v", err)
	}
	if len(tailSamples) != frameSamples {
		t.Fatalf("tail decoded %d samples, want exactly %d", len(tailSamples), frameSamples)
	}

	// Idempotent: no residual left, no extra frame invented.
	again, err := enc.Flush()
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second Flush returned %d frames, want 0", len(again))
	}

	// Flush after aligned input produces no frame.
	aligned := mustEncoder(t, params)
	if _, err := aligned.EncodeSamples(Silence(frameSamples)); err != nil {
		t.Fatalf("aligned EncodeSamples: %v", err)
	}
	if extra, err := aligned.Flush(); err != nil || len(extra) != 0 {
		t.Fatalf("Flush after aligned input=(%d frames, %v), want none", len(extra), err)
	}

	// Flush does not reset the encoder: the same stream continues and the
	// next complete frame is still produced.
	enc2 := mustEncoder(t, params)
	if _, err := enc2.EncodeSamples(Silence(frameSamples + 10)); err != nil {
		t.Fatalf("EncodeSamples: %v", err)
	}
	if tail, err := enc2.Flush(); err != nil || len(tail) != 1 {
		t.Fatalf("Flush=(%d frames, %v), want one tail frame", len(tail), err)
	}
	after, err := enc2.EncodeSamples(Sine(params.SampleRate, 700, 0.3, frameSamples))
	if err != nil {
		t.Fatalf("EncodeSamples after Flush: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after Flush the continuing stream produced %d frames, want 1", len(after))
	}
}

// TestA15StreamingStateIsolated proves that each stream keeps one continuous
// encoder state: identical chunk sequences produce identical bytes, and an
// unrelated interleaved encoder does not disturb the stream.
func TestA15StreamingStateIsolated(t *testing.T) {
	params := uplinkParams()
	frameSamples := params.FrameSamples()
	total := frameSamples*4 + 77
	pcm := Sine(params.SampleRate, 880, 0.3, total)
	sizes := []int{13, frameSamples + 1, 9, 2 * frameSamples, 1}

	makeFrames := func(enc *Encoder) [][]byte {
		frames, err := encodeChunks(enc, pcm, sizes)
		if err != nil {
			t.Fatalf("EncodeSamples: %v", err)
		}
		tail, err := enc.Flush()
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
		return append(frames, tail...)
	}

	a := makeFrames(mustEncoder(t, params))
	// Unrelated stream in between must not affect stream b.
	noise := mustEncoder(t, params)
	if _, err := noise.EncodeSamples(Sine(params.SampleRate, 123, 0.9, frameSamples*2)); err != nil {
		t.Fatalf("unrelated stream: %v", err)
	}
	b := makeFrames(mustEncoder(t, params))

	if len(a) != len(b) {
		t.Fatalf("streams produced %d and %d frames", len(a), len(b))
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			t.Fatalf("frame %d differs between identical streams; encoder state leaked across instances", i)
		}
	}
	if len(a) != 5 { // 4 full frames plus the padded tail
		t.Fatalf("stream produced %d frames, want 5", len(a))
	}
}

func paramsName(p Params) string {
	switch {
	case p.SampleRate == 16000:
		return "uplink_16k_mono_60ms"
	case p.SampleRate == 24000:
		return "downlink_24k_mono_60ms"
	default:
		return "codec"
	}
}

// TestA15PCM16LittleEndian checks that EncodePCM interprets bytes as
// little-endian PCM16: encoding the same interleaved samples through the byte
// API and the sample API yields identical frames.
func TestA15PCM16LittleEndian(t *testing.T) {
	params := downlinkParams()
	samples := Sine(params.SampleRate, 440, 0.3, params.FrameSamples()+17)
	pcm := make([]byte, 2*len(samples))
	for i, v := range samples {
		pcm[2*i] = byte(uint16(v))
		pcm[2*i+1] = byte(uint16(v) >> 8)
	}

	viaBytes := mustEncoder(t, params)
	viaSamples := mustEncoder(t, params)
	byteFrames, err := viaBytes.EncodePCM(pcm)
	if err != nil {
		t.Fatalf("EncodePCM: %v", err)
	}
	sampleFrames, err := viaSamples.EncodeSamples(samples)
	if err != nil {
		t.Fatalf("EncodeSamples: %v", err)
	}
	if len(byteFrames) != 1 || len(sampleFrames) != 1 {
		t.Fatalf("produced %d byte-API / %d sample-API frames, want 1 each", len(byteFrames), len(sampleFrames))
	}
	if string(byteFrames[0]) != string(sampleFrames[0]) {
		t.Fatal("EncodePCM and EncodeSamples disagree; PCM16LE byte order is wrong")
	}
}

func TestStereoInterleaving(t *testing.T) {
	params := Params{SampleRate: 16000, Channels: 2, FrameDurationMS: 20}
	if err := params.Validate(); err != nil {
		t.Fatalf("stereo params rejected: %v", err)
	}
	enc := mustEncoder(t, params)
	dec := mustDecoder(t, params)
	frameSamples := params.FrameSamples()

	// Interleaved stereo: left 440 Hz, right 880 Hz.
	pcm := make([]int16, frameSamples*2*2)
	left := Sine(params.SampleRate, 440, 0.4, frameSamples*2)
	right := Sine(params.SampleRate, 880, 0.4, frameSamples*2)
	for i := 0; i < frameSamples*2; i++ {
		pcm[2*i] = left[i]
		pcm[2*i+1] = right[i]
	}
	frames, err := enc.EncodeSamples(pcm)
	if err != nil {
		t.Fatalf("EncodeSamples stereo: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("stereo encoded %d frames, want 2", len(frames))
	}
	for i, f := range frames {
		decoded, err := dec.Decode(f)
		if err != nil {
			t.Fatalf("decode stereo frame %d: %v", i, err)
		}
		if want := frameSamples * 2; len(decoded) != want {
			t.Fatalf("stereo frame %d decoded %d interleaved samples, want %d", i, len(decoded), want)
		}
		if peakAmplitude(decoded) < sineMinPeak {
			t.Fatalf("stereo frame %d decoded without tone energy", i)
		}
	}
}
