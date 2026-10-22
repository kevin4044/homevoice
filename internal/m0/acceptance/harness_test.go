package acceptance_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
	"github.com/homevoice/homevoice/internal/m0/probe"
	"github.com/homevoice/homevoice/internal/m0/session"
	"github.com/homevoice/homevoice/internal/m0/simulator"
	"github.com/homevoice/homevoice/internal/m0/wire"
)

// lockedBuffer is a concurrency-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// harness runs one isolated probe with simulated devices on loopback.
type harness struct {
	t             *testing.T
	cfg           *config.Config
	probe         *probe.Probe
	logs          *lockedBuffer
	red           *logging.Redactor
	devices       map[string]*simulator.Device
	deviceMutator func(deviceID string, opts *simulator.DeviceOptions)
}

func fastPlayback() probe.PlaybackOptions {
	return probe.PlaybackOptions{
		Settle:          2 * time.Millisecond,
		FrameDuration:   5 * time.Millisecond,
		PrebufferFrames: 1,
		TailExtraFrames: 1,
		MaxQueue:        limits.AudioQueueFrames,
	}
}

func newHarness(t *testing.T, mutate func(*config.Config), mutateProbe func(*probe.Options)) *harness {
	return newHarnessWithDeviceMutator(t, mutate, mutateProbe, nil)
}

func newHarnessWithDeviceMutator(t *testing.T, mutate func(*config.Config), mutateProbe func(*probe.Options), deviceMutator func(deviceID string, opts *simulator.DeviceOptions)) *harness {
	t.Helper()
	cfg := config.DefaultOffline()
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("harness config: %v", err)
	}
	logs := &lockedBuffer{}
	red := logging.NewRedactor(cfg.Secrets()...)
	logger := logging.New(logs, slog.LevelDebug, red)
	opts := &probe.Options{
		Config:         cfg,
		Logger:         logger,
		Redactor:       red,
		Playback:       fastPlayback(),
		ListenRoundMax: 2 * time.Second,
	}
	if mutateProbe != nil {
		mutateProbe(opts)
	}
	p, err := probe.New(*opts)
	if err != nil {
		t.Fatalf("probe.New: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("probe.Start: %v", err)
	}
	h := &harness{t: t, cfg: cfg, probe: p, logs: logs, red: red, devices: map[string]*simulator.Device{}, deviceMutator: deviceMutator}
	t.Cleanup(func() {
		for _, d := range h.devices {
			_ = d.Close()
		}
		_ = h.probe.Close()
	})
	return h
}

func (h *harness) connect(deviceID string) *simulator.Device {
	h.t.Helper()
	if d := h.devices[deviceID]; d != nil {
		return d
	}
	devCfg, ok := h.cfg.DeviceByID(deviceID)
	if !ok {
		h.t.Fatalf("unknown device %q", deviceID)
	}
	opts := simulator.DeviceOptions{
		Logger:    logging.Discard(),
		MCPServer: simulator.DefaultMCPServer(),
	}
	if h.deviceMutator != nil {
		h.deviceMutator(deviceID, &opts)
	}
	d, err := simulator.NewDevice(devCfg, opts)
	if err != nil {
		h.t.Fatalf("simulator.NewDevice(%s): %v", deviceID, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx, h.probe.BrokerURL()); err != nil {
		h.t.Fatalf("device %s connect: %v", deviceID, err)
	}
	h.devices[deviceID] = d
	return d
}

func (h *harness) hello(deviceID string) wire.HelloResponse {
	h.t.Helper()
	d := h.connect(deviceID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := d.SendHello(ctx, "udp", 3)
	if err != nil {
		h.t.Fatalf("device %s HELLO: %v", deviceID, err)
	}
	h.waitSession(deviceID)
	return resp
}

func (h *harness) waitSession(deviceID string) session.Snapshot {
	h.t.Helper()
	var snap session.Snapshot
	waitFor(h.t, "audio session for "+deviceID, 5*time.Second, func() bool {
		s, ok := h.probe.SessionSnapshot(deviceID)
		if !ok {
			return false
		}
		snap = s
		return true
	})
	return snap
}

func (h *harness) waitNoSession(deviceID string) {
	h.t.Helper()
	waitFor(h.t, "audio session removal for "+deviceID, 5*time.Second, func() bool {
		_, ok := h.probe.SessionSnapshot(deviceID)
		return !ok
	})
}

func (h *harness) waitOffline(deviceID string) {
	h.t.Helper()
	waitFor(h.t, "MQTT offline for "+deviceID, 5*time.Second, func() bool {
		_, ok := h.probe.Online(deviceID)
		return !ok
	})
}

func waitFor(t *testing.T, desc string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", desc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fixedUpstream returns one deterministic synthetic response with a configurable
// number of encoded frames.
type fixedUpstream struct {
	frames [][]byte
	err    error
}

func (u *fixedUpstream) Transcribe(context.Context, []int16) (string, error) {
	if u.err != nil {
		return "", u.err
	}
	return "合成测试指令", nil
}

func (u *fixedUpstream) Synthesize(context.Context, string) ([][]byte, error) {
	if u.err != nil {
		return nil, u.err
	}
	return u.frames, nil
}

func newSyntheticUpstream(ttsFrames int) (probe.Upstream, error) {
	return simulator.NewUpstream(simulator.UpstreamOptions{TTSFrames: ttsFrames})
}

func synthFrames(n int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		frames[i] = []byte{byte(i), 0x55, 0xaa}
	}
	return frames
}

func mustParseNotify(t *testing.T, payload []byte) wire.Notify {
	t.Helper()
	n, err := wire.ParseNotify(payload)
	if err != nil {
		t.Fatalf("notify parse: %v", err)
	}
	return n
}

func isBlocked(err error) bool {
	return errors.Is(err, config.ErrBlocked)
}

func containsAny(s string, subs []string) string {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return sub
		}
	}
	return ""
}
