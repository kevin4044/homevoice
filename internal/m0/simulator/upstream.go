package simulator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/homevoice/homevoice/internal/m0/agent"
	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

// UpstreamOptions configures the synthetic ASR/LLM/TTS providers.
type UpstreamOptions struct {
	Clock clock.Clock
	// ASROp/LLMOp/TTSOp simulate upstream latency (bounded by
	// limits.UpstreamOpMax).
	ASROp, LLMOp, TTSOp       time.Duration
	FailASR, FailLLM, FailTTS bool
	// ASRText is the fixed synthetic transcript.
	ASRText string
	// ModelText is the fixed synthetic final answer.
	ModelText string
	// UseToolCall makes the first model response request the synthetic
	// session-bound volume tool.
	UseToolCall bool
	VolumeLevel int
	// TTSFrames bounds synthetic TTS audio (<=64 frames).
	TTSFrames int
}

func (o *UpstreamOptions) defaults() {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.ASRText == "" {
		o.ASRText = "请把音量调到二十（合成测试指令，无私人信息）"
	}
	if o.ModelText == "" {
		o.ModelText = "已按测试指令处理（合成响应）"
	}
	if o.VolumeLevel <= 0 || o.VolumeLevel > 30 {
		o.VolumeLevel = 20
	}
	if o.TTSFrames <= 0 || o.TTSFrames > limits.AudioQueueFrames {
		o.TTSFrames = 5
	}
	for _, d := range []time.Duration{o.ASROp, o.LLMOp, o.TTSOp} {
		if d > limits.UpstreamOpMax {
			// The caller supplied an unbounded delay; clamp with an error at
			// New rather than sleeping forever.
			o.ASROp = limits.UpstreamOpMax
		}
	}
}

// Upstream is a deterministic synthetic ASR/LLM/TTS. It never contacts a
// network service.
type Upstream struct {
	opts UpstreamOptions

	mu        sync.Mutex
	chatCalls int
	lastText  string
}

// NewUpstream creates the synthetic upstream.
func NewUpstream(opts UpstreamOptions) (*Upstream, error) {
	opts.defaults()
	for _, d := range []time.Duration{opts.ASROp, opts.LLMOp, opts.TTSOp} {
		if d < 0 || d > limits.UpstreamOpMax {
			return nil, fmt.Errorf("simulator: upstream delay %v out of bounds", d)
		}
	}
	return &Upstream{opts: opts}, nil
}

func (u *Upstream) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-u.opts.Clock.After(d):
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Transcribe is the synthetic ASR. The PCM content is not interpreted (the
// offline probe cannot prove recognition quality); the configured synthetic
// transcript is returned after a bounded delay.
func (u *Upstream) Transcribe(ctx context.Context, pcm []int16) (string, error) {
	opCtx, cancel := context.WithTimeout(ctx, limits.UpstreamOpMax)
	defer cancel()
	if len(pcm) == 0 {
		return "", errors.New("simulator: ASR requires non-empty PCM")
	}
	if err := u.wait(opCtx, u.opts.ASROp); err != nil {
		return "", fmt.Errorf("simulator: ASR: %w", err)
	}
	if u.opts.FailASR {
		return "", errors.New("simulator: synthetic ASR failure")
	}
	return u.opts.ASRText, nil
}

// Chat implements agent.ModelProvider with a deterministic bounded script.
func (u *Upstream) Chat(ctx context.Context, req agent.Request) (agent.Response, error) {
	opCtx, cancel := context.WithTimeout(ctx, limits.UpstreamOpMax)
	defer cancel()
	if err := u.wait(opCtx, u.opts.LLMOp); err != nil {
		return agent.Response{}, fmt.Errorf("simulator: LLM: %w", err)
	}
	if u.opts.FailLLM {
		return agent.Response{}, errors.New("simulator: synthetic LLM failure")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chatCalls++
	if u.opts.UseToolCall && u.chatCalls == 1 {
		return agent.Response{
			ToolCalls: []agent.ToolCall{{
				ID:        "sim-call-1",
				Name:      agent.SyntheticToolName,
				Arguments: fmt.Appendf(nil, `{"level":%d}`, u.opts.VolumeLevel),
			}},
		}, nil
	}
	return agent.Response{Content: u.opts.ModelText}, nil
}

// Synthesize turns the synthetic text into bounded downlink PCM (a sine tone
// whose duration depends on the configured frame count). It does not perform
// real speech synthesis; the probe only verifies the audio pipeline and
// bounded playback.
func (u *Upstream) Synthesize(ctx context.Context, text string) ([][]byte, error) {
	opCtx, cancel := context.WithTimeout(ctx, limits.UpstreamOpMax)
	defer cancel()
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("simulator: TTS requires non-empty text")
	}
	if err := u.wait(opCtx, u.opts.TTSOp); err != nil {
		return nil, fmt.Errorf("simulator: TTS: %w", err)
	}
	if u.opts.FailTTS {
		return nil, errors.New("simulator: synthetic TTS failure")
	}
	params := audio.DownlinkParams()
	enc, err := audio.NewEncoder(params)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	pcm := audio.Sine(params.SampleRate, 440, 0.2, params.FrameSamples()*u.opts.TTSFrames)
	frames, err := enc.EncodeSamples(pcm)
	if err != nil {
		return nil, err
	}
	tail, err := enc.Flush()
	if err != nil {
		return nil, err
	}
	frames = append(frames, tail...)
	if len(frames) > limits.AudioQueueFrames {
		frames = frames[:limits.AudioQueueFrames]
	}
	u.mu.Lock()
	u.lastText = text
	u.mu.Unlock()
	return frames, nil
}

// ChatCalls reports how many model calls the synthetic provider served.
func (u *Upstream) ChatCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chatCalls
}

// LastSpokenText returns the last text synthesized.
func (u *Upstream) LastSpokenText() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastText
}
