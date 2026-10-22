package audio

import "math"

// Sine generates a synthetic sine tone as PCM16 samples. amplitude is clamped
// to [0,1] and NaN/infinite values are treated as 0 so the generator can never
// produce implementation-defined sample values. The generator is stateless:
// every call starts at phase 0, so callers that need a phase-continuous tone
// should generate one slice and split it into frames.
func Sine(sampleRate int, freqHz, amplitude float64, samples int) []int16 {
	if sampleRate <= 0 || samples <= 0 {
		return nil
	}
	if math.IsNaN(amplitude) || math.IsInf(amplitude, 0) || amplitude < 0 {
		amplitude = 0
	}
	if amplitude > 1 {
		amplitude = 1
	}
	if math.IsNaN(freqHz) || math.IsInf(freqHz, 0) {
		freqHz = 0
	}
	if freqHz < 0 {
		freqHz = -freqHz
	}
	out := make([]int16, samples)
	step := 2 * math.Pi * freqHz / float64(sampleRate)
	for i := range out {
		out[i] = int16(math.Round(amplitude * math.Sin(step*float64(i)) * 32767))
	}
	return out
}

// Silence generates a synthetic silent block of PCM16 samples. Negative or
// zero lengths return nil.
func Silence(samples int) []int16 {
	if samples <= 0 {
		return nil
	}
	return make([]int16, samples)
}
