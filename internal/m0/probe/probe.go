// Package probe wires the isolated M0 components into one in-process
// verifiable stack: MQTT control (trusted identity), UDP audio sessions,
// playback scheduling, idle notify, device MCP and the bounded model loop.
//
// It is test/probe infrastructure, not a production service. It binds to
// loopback (or explicitly authorized private addresses) only; live cloud and
// real-hardware paths are separate authorized entries.
package probe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/homevoice/homevoice/internal/m0/agent"
	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/broker"
	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/devicemcp"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
	"github.com/homevoice/homevoice/internal/m0/notify"
	"github.com/homevoice/homevoice/internal/m0/playout"
	"github.com/homevoice/homevoice/internal/m0/session"
	"github.com/homevoice/homevoice/internal/m0/udp"
	"github.com/homevoice/homevoice/internal/m0/wire"
)

// Upstream is the synthetic or authorized ASR/TTS surface used by the probe's
// listen->response pipeline. Offline tests inject the simulator; live runs
// inject the authorized providers.
type Upstream interface {
	Transcribe(ctx context.Context, pcm []int16) (string, error)
	Synthesize(ctx context.Context, text string) ([][]byte, error)
}

// PlaybackOptions tunes the M0 playback timeline. Zero values use the
// specification defaults (150ms settle, 5-frame prebuffer, 2 extra tail
// frames, 60ms frames, 64-frame queue). Tests may shorten them while still
// exercising the bounded phases.
type PlaybackOptions struct {
	Settle          time.Duration
	FrameDuration   time.Duration
	PrebufferFrames int
	TailExtraFrames int
	MaxQueue        int
}

func (o PlaybackOptions) withDefaults() PlaybackOptions {
	if o.Settle <= 0 {
		o.Settle = limits.TTSSettle
	}
	if o.FrameDuration <= 0 {
		o.FrameDuration = limits.DownlinkFrameDurationMS * time.Millisecond
	}
	if o.PrebufferFrames < 0 {
		o.PrebufferFrames = 0
	}
	if o.TailExtraFrames < 0 {
		o.TailExtraFrames = 0
	}
	if o.MaxQueue <= 0 || o.MaxQueue > limits.AudioQueueFrames {
		o.MaxQueue = limits.AudioQueueFrames
	}
	return o
}

// Options configures a Probe.
type Options struct {
	Config   *config.Config
	Logger   *slog.Logger
	Redactor *logging.Redactor
	Clock    clock.Clock
	Upstream Upstream
	Playback PlaybackOptions
	// ListenRoundMax bounds one listen round. Defaults to limits.ListenRoundMax.
	ListenRoundMax time.Duration
	// NotifyText/NotifyAudioURL are the synthetic idle notification payload.
	NotifyText     string
	NotifyAudioURL string
}

// Stats is a point-in-time probe snapshot safe for evidence.
type Stats struct {
	OnlineDevices  int
	ActiveSessions int
	BusyDevices    int
	UplinkFrames   uint64
	UplinkDropped  uint64
	DecodeErrors   uint64
	Malformed      uint64
	PlaybackRounds uint64
	AgentTurns     uint64
	PendingTasks   int
}

// DeviceSnapshot describes one simulated/real device session.
type DeviceSnapshot struct {
	DeviceID     string
	Online       bool
	Generation   uint64
	SessionID    string
	Listening    bool
	Playing      bool
	Busy         bool
	Tools        int
	UplinkFrames uint64
	LastError    string
}

type deviceState struct {
	deviceID   string
	online     bool
	generation uint64
	sessionID  string

	mcp     *devicemcp.Client
	tools   []devicemcp.Tool
	binding *agent.ToolBinding
	// mcpID remains allocated for the device lifetime so JSON-RPC request ids
	// stay unique across UDP session rotations/generations.
	mcpID *atomic.Int64

	player  *playout.Player
	decoder *audio.Decoder

	listening     bool
	listenMode    string
	listenCancel  context.CancelFunc
	listenGen     uint64
	listenSession string
	pcm           []int16
	uplinkFrames  uint64
	decodeErrors  uint64

	playing        bool
	playCancel     context.CancelFunc
	pipelineCancel context.CancelFunc
	pipelineBusy   bool

	lastError string
}

type probeCounters struct {
	uplinkDropped  uint64
	malformed      uint64
	playbackRounds uint64
	agentTurns     uint64
}

// Probe is the in-process M0 stack.
type Probe struct {
	cfg *config.Config
	log *slog.Logger
	red *logging.Redactor
	clk clock.Clock
	up  Upstream

	listenMax      time.Duration
	notifyText     string
	notifyAudioURL string

	ctx    context.Context
	cancel context.CancelFunc
	tasks  *taskGroup

	broker      *broker.Broker
	transport   *udp.Transport
	sessions    *session.Manager
	notifier    *notify.Service
	playOptions playout.Config

	mu       sync.Mutex
	states   map[string]*deviceState
	counters probeCounters
	closed   bool
	started  bool
}

// New validates the configuration and constructs all components without
// starting network listeners.
func New(opts Options) (*Probe, error) {
	if opts.Config == nil {
		return nil, errors.New("probe: config is required")
	}
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("probe: config: %w", err)
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	log := opts.Logger
	red := opts.Redactor
	if red == nil {
		red = logging.NewRedactor(cfg.Secrets()...)
	}
	if log == nil {
		log = logging.New(discardWriter{}, slog.LevelInfo, red)
	}
	listenMax := opts.ListenRoundMax
	if listenMax <= 0 {
		listenMax = limits.ListenRoundMax
	}
	notifyText := opts.NotifyText
	if notifyText == "" {
		notifyText = "Homevoice 测试通知"
	}
	notifyURL := opts.NotifyAudioURL
	if notifyURL == "" {
		notifyURL = "https://test-audio.invalid/notice.wav"
	}
	playOpts := opts.Playback.withDefaults()
	playConfig := playout.Config{
		Clock:           clk,
		FrameDuration:   playOpts.FrameDuration,
		Settle:          playOpts.Settle,
		PrebufferFrames: playOpts.PrebufferFrames,
		TailExtraFrames: playOpts.TailExtraFrames,
		MaxQueue:        playOpts.MaxQueue,
	}

	transport, err := udp.Listen(cfg.Network.UDPBind)
	if err != nil {
		return nil, fmt.Errorf("probe: udp listen: %w", err)
	}
	p := &Probe{
		cfg:            cfg,
		tasks:          newTaskGroup(),
		log:            log,
		red:            red,
		clk:            clk,
		up:             opts.Upstream,
		listenMax:      listenMax,
		notifyText:     notifyText,
		notifyAudioURL: notifyURL,
		transport:      transport,
		playOptions:    playConfig,
		states:         make(map[string]*deviceState),
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	sourcePolicy, err := session.SourcePolicy(cfg.Network.AuthorizedPrivateCIDRs)
	if err != nil {
		transport.Close()
		return nil, err
	}
	sessions, err := session.NewManager(session.Options{
		Clock:          clk,
		Logger:         log,
		Send:           transport.SendTo,
		AllowedSource:  sourcePolicy,
		FrameDuration:  playOpts.FrameDuration,
		OnFrame:        p.onUDPFrame,
		OnSessionEnded: p.onSessionEnded,
	})
	if err != nil {
		transport.Close()
		return nil, err
	}
	p.sessions = sessions

	var tlsCfg *tls.Config
	if cfg.Network.MQTTServerCertFile != "" {
		tlsCfg, err = config.ServerTLSConfig(cfg.Network.MQTTServerCertFile, cfg.Network.MQTTServerKeyFile, "")
		if err != nil {
			transport.Close()
			return nil, err
		}
	}
	b, err := broker.New(broker.Options{
		BindAddr:               cfg.Network.MQTTBind,
		Devices:                cfg.Devices,
		TLSConfig:              tlsCfg,
		Logger:                 log,
		ConnectTimeout:         limits.MQTTConnectTimeout,
		AuthorizedPrivateCIDRs: cfg.Network.AuthorizedPrivateCIDRs,
		OnMessage:              p.onMQTTMessage,
		OnOnline:               p.onOnline,
		OnOffline:              p.onOffline,
	})
	if err != nil {
		transport.Close()
		return nil, err
	}
	p.broker = b

	notifier, err := notify.New(b, clk, p.isBusy, log)
	if err != nil {
		_ = b.Close()
		transport.Close()
		return nil, err
	}
	p.notifier = notifier

	p.mu.Lock()
	for _, d := range cfg.Devices {
		player, err := playout.New(playConfig)
		if err != nil {
			p.mu.Unlock()
			_ = b.Close()
			transport.Close()
			return nil, fmt.Errorf("probe: player for %s: %w", d.DeviceID, err)
		}
		p.states[d.DeviceID] = &deviceState{deviceID: d.DeviceID, player: player, mcpID: new(atomic.Int64)}
	}
	p.mu.Unlock()
	return p, nil
}

// Start starts the MQTT broker and UDP read loop.
func (p *Probe) Start() error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("probe: already started")
	}
	if p.closed {
		p.mu.Unlock()
		return errors.New("probe: closed")
	}
	p.started = true
	p.mu.Unlock()
	if err := p.broker.Start(); err != nil {
		return fmt.Errorf("probe: broker start: %w", err)
	}
	if !p.tasks.Add() {
		return errors.New("probe: closing")
	}
	go func() {
		defer p.tasks.Done()
		if err := p.transport.RunReadLoop(p.ctx, func(from *net.UDPAddr, datagram []byte) {
			if _, err := p.sessions.HandleDatagram(from, datagram); err != nil {
				p.log.Debug("udp datagram rejected", "reason", err.Error())
			}
		}); err != nil {
			p.log.Warn("udp read loop stopped", "error", err.Error())
		}
	}()
	return nil
}

// Close releases all resources. It is safe to call once.
func (p *Probe) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cancels := make([]context.CancelFunc, 0, len(p.states)*2)
	for _, st := range p.states {
		if st.listenCancel != nil {
			cancels = append(cancels, st.listenCancel)
		}
		if st.playCancel != nil {
			cancels = append(cancels, st.playCancel)
		}
	}
	p.mu.Unlock()
	p.cancel()
	for _, c := range cancels {
		c()
	}
	p.sessions.CloseAll("probe-shutdown")
	if err := p.broker.Close(); err != nil {
		p.log.Warn("broker close", "error", err.Error())
	}
	if err := p.transport.Close(); err != nil {
		p.log.Warn("udp close", "error", err.Error())
	}
	p.tasks.CloseAndWait()
	return nil
}

// Addr returns the MQTT listener address.
func (p *Probe) Addr() string { return p.broker.Addr() }

// UDPAddr returns the UDP bind address.
func (p *Probe) UDPAddr() string { return p.transport.LocalAddr().String() }

// BrokerURL is the paho-compatible URL for simulated devices.
func (p *Probe) BrokerURL() string { return "tcp://" + p.Addr() }

// Notify sends one idle notification. It never requires a UDP session.
func (p *Probe) Notify(deviceID string) (notify.Result, error) {
	return p.notifier.Notify(deviceID, p.notifyText, p.notifyAudioURL)
}

// PlayerStats exposes bounded audio-queue accounting for one device.
func (p *Probe) PlayerStats(deviceID string) (playout.Stats, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[deviceID]
	if st == nil || st.player == nil {
		return playout.Stats{}, false
	}
	return st.player.Stats(), true
}

// Online reports the trusted MQTT connection generation for a device.
func (p *Probe) Online(deviceID string) (uint64, bool) {
	return p.broker.Online(deviceID)
}

// UDPManagerCounters exposes aggregate UDP rejection counters for tests and
// reports. It contains no key material.
func (p *Probe) UDPManagerCounters() (unknownSSRC, malformed, sourceReject uint64) {
	return p.sessions.ManagerCounters()
}

// SessionSnapshot returns the current UDP session snapshot.
func (p *Probe) SessionSnapshot(deviceID string) (session.Snapshot, bool) {
	return p.sessions.Snapshot(deviceID)
}

// Snapshot returns one device snapshot.
func (p *Probe) Snapshot(deviceID string) (DeviceSnapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.states[deviceID]
	if !ok {
		return DeviceSnapshot{}, false
	}
	return DeviceSnapshot{
		DeviceID:     st.deviceID,
		Online:       st.online,
		Generation:   st.generation,
		SessionID:    st.sessionID,
		Listening:    st.listening,
		Playing:      st.playing,
		Busy:         st.listening || st.playing || st.pipelineBusy,
		Tools:        len(st.tools),
		UplinkFrames: st.uplinkFrames,
		LastError:    st.lastError,
	}, true
}

// Stats returns aggregate probe counters.
func (p *Probe) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := Stats{
		UplinkDropped:  p.counters.uplinkDropped,
		Malformed:      p.counters.malformed,
		PlaybackRounds: p.counters.playbackRounds,
		AgentTurns:     p.counters.agentTurns,
	}
	for _, st := range p.states {
		if st.online {
			out.OnlineDevices++
		}
		if st.listening || st.playing || st.pipelineBusy {
			out.BusyDevices++
		}
		out.UplinkFrames += st.uplinkFrames
		out.DecodeErrors += st.decodeErrors
	}
	out.ActiveSessions = p.sessions.ActiveCount()
	for _, st := range p.states {
		if st.pipelineBusy {
			out.PendingTasks++
		}
		if st.playing {
			out.PendingTasks++
		}
		if st.listening {
			out.PendingTasks++
		}
	}
	return out
}

func (p *Probe) isBusy(deviceID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[deviceID]
	return st != nil && (st.listening || st.playing || st.pipelineBusy)
}

// DiscoverDeviceTools initializes the device MCP session and enumerates tools.
func (p *Probe) DiscoverDeviceTools(ctx context.Context, deviceID string) ([]devicemcp.Tool, agent.ToolBinding, error) {
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil {
		p.mu.Unlock()
		return nil, agent.ToolBinding{}, fmt.Errorf("probe: unknown device %q", deviceID)
	}
	client := st.mcp
	p.mu.Unlock()
	if client == nil {
		return nil, agent.ToolBinding{}, fmt.Errorf("probe: device %q has no active MCP session (send HELLO first)", deviceID)
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(p.ctx, limits.MCPRequestTimeout)
		defer cancel()
	}
	if _, err := client.Initialize(ctx); err != nil {
		return nil, agent.ToolBinding{}, fmt.Errorf("probe: device MCP initialize: %w", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, agent.ToolBinding{}, fmt.Errorf("probe: device MCP tools/list: %w", err)
	}
	var binding agent.ToolBinding
	found := false
	for _, tool := range tools {
		if !strings.Contains(strings.ToLower(tool.Name), "volume") {
			continue
		}
		b, err := agent.NewLoop(0, 0).BindVolumeTool(tool.Name, tool.InputSchema)
		if err != nil {
			continue
		}
		binding = b
		found = true
		break
	}
	p.mu.Lock()
	if st2 := p.states[deviceID]; st2 != nil {
		st2.tools = tools
		if found {
			st2.binding = &binding
		}
	}
	p.mu.Unlock()
	if !found {
		return tools, agent.ToolBinding{}, errors.New("probe: no session-bound volume tool discovered; device tools are not exposed to the model")
	}
	return tools, binding, nil
}

// RunAgentTurn runs one bounded model/tool turn bound to the current device
// session. The executor can only call the discovered volume tool.
func (p *Probe) RunAgentTurn(ctx context.Context, deviceID string, provider agent.ModelProvider) (agent.RunResult, error) {
	if provider == nil {
		return agent.RunResult{}, errors.New("probe: model provider is required")
	}
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil {
		p.mu.Unlock()
		return agent.RunResult{}, fmt.Errorf("probe: unknown device %q", deviceID)
	}
	sessionID, gen := st.sessionID, st.generation
	binding := st.binding
	var toolSchema json.RawMessage
	for _, t := range st.tools {
		if binding != nil && t.Name == binding.DeviceTool {
			toolSchema = t.InputSchema
		}
	}
	p.mu.Unlock()
	if binding == nil || toolSchema == nil {
		_, discovered, err := p.DiscoverDeviceTools(ctx, deviceID)
		if err != nil {
			return agent.RunResult{}, err
		}
		binding = &discovered
		p.mu.Lock()
		for _, t := range st.tools {
			if t.Name == binding.DeviceTool {
				toolSchema = t.InputSchema
			}
		}
		sessionID, gen = st.sessionID, st.generation
		p.mu.Unlock()
	}
	loop := agent.NewLoop(limits.ModelMaxRequests, limits.ModelMaxToolCalls)
	if _, err := loop.BindVolumeTool(binding.DeviceTool, toolSchema); err != nil {
		return agent.RunResult{}, err
	}
	loop.SetMessages([]agent.Message{{Role: "user", Content: "请把当前房间音量调到二十（合成测试）"}})
	caller := &deviceCaller{p: p, deviceID: deviceID, generation: gen, sessionID: sessionID, tool: binding.DeviceTool}
	executor := agent.NewVolumeExecutor(*binding, caller)
	p.mu.Lock()
	p.counters.agentTurns++
	p.mu.Unlock()
	return loop.Run(ctx, provider, executor)
}

type deviceCaller struct {
	p          *Probe
	deviceID   string
	generation uint64
	sessionID  string
	tool       string
}

func (c *deviceCaller) CallDeviceTool(ctx context.Context, name string, args json.RawMessage) (agent.DeviceCallResult, error) {
	if name != c.tool {
		return agent.DeviceCallResult{IsError: true}, fmt.Errorf("probe: tool %q is not the discovered session tool", name)
	}
	c.p.mu.Lock()
	st := c.p.states[c.deviceID]
	if st == nil || st.sessionID != c.sessionID || st.generation != c.generation || st.mcp == nil {
		c.p.mu.Unlock()
		return agent.DeviceCallResult{IsError: true}, errors.New("probe: device session changed; tool target is no longer valid")
	}
	client := st.mcp
	c.p.mu.Unlock()
	res, err := client.CallTool(ctx, name, args)
	if err != nil {
		return agent.DeviceCallResult{Content: err.Error(), IsError: true}, err
	}
	return agent.DeviceCallResult{Content: res.Text(), IsError: res.IsError}, nil
}

func (p *Probe) onMQTTMessage(msg broker.Message) {
	if msg.Payload == nil {
		return
	}
	// Ignore application messages from a superseded connection generation.
	if gen, ok := p.broker.Online(msg.DeviceID); !ok || gen != msg.Generation {
		p.log.Debug("stale connection message ignored", "device", msg.DeviceID, "generation", msg.Generation)
		return
	}
	policy := wire.DefaultHelloPolicy(wire.AudioParams{
		Format:        p.cfg.Audio.Uplink.Format,
		SampleRate:    p.cfg.Audio.Uplink.SampleRate,
		Channels:      p.cfg.Audio.Uplink.Channels,
		FrameDuration: p.cfg.Audio.Uplink.FrameDuration,
	})
	parsed, err := wire.ParseUplink(msg.Payload, policy)
	if err != nil {
		p.mu.Lock()
		p.counters.malformed++
		if st := p.states[msg.DeviceID]; st != nil {
			st.lastError = err.Error()
		}
		p.mu.Unlock()
		p.log.Warn("mqtt application message rejected", "device", msg.DeviceID, "reason", err.Error())
		return
	}
	switch m := parsed.(type) {
	case wire.Hello:
		p.handleHello(msg.DeviceID, msg.Generation, m)
	case wire.Listen:
		p.handleListen(msg.DeviceID, msg.Generation, m)
	case wire.Abort:
		p.handleAbort(msg.DeviceID, msg.Generation, m)
	case wire.Goodbye:
		p.handleGoodbye(msg.DeviceID, msg.Generation, m)
	case wire.MCP:
		p.handleMCP(msg.DeviceID, msg.Generation, m)
	default:
		p.log.Warn("unhandled application message", "device", msg.DeviceID)
	}
}

func (p *Probe) handleHello(deviceID string, generation uint64, hello wire.Hello) {
	// The broker is the source of truth for the live connection generation.
	// A takeover bumps it before the new connection's HELLO, so accepting on
	// broker state avoids rejecting a valid new-generation HELLO merely
	// because the probe still briefly holds older state.
	if cur, ok := p.broker.Online(deviceID); !ok || cur != generation {
		p.log.Warn("stale HELLO ignored", "device", deviceID, "generation", generation)
		return
	}
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil || st.generation > generation {
		p.mu.Unlock()
		p.log.Warn("stale HELLO ignored", "device", deviceID, "generation", generation)
		return
	}
	p.mu.Unlock()
	p.cancelDeviceTasks(deviceID)

	info, err := p.sessions.Hello(deviceID, generation)
	if err != nil {
		p.setDeviceError(deviceID, err)
		return
	}
	decoder, err := audio.NewDecoder(audio.UplinkParams())
	if err != nil {
		p.setDeviceError(deviceID, err)
		return
	}
	// New session-bound MCP client. Sender checks the session is still current
	// so late requests can never be delivered to a rotated session. The request
	// id source is shared for the device lifetime: a late response from the
	// previous audio session can never match a request id of the new session.
	p.mu.Lock()
	idSource := p.states[deviceID].mcpID
	if idSource == nil {
		idSource = new(atomic.Int64)
		p.states[deviceID].mcpID = idSource
	}
	p.mu.Unlock()
	mcpClient := devicemcp.NewWithIDSource(
		devicemcp.Identity{DeviceID: deviceID, Generation: generation, SessionID: info.SessionID},
		senderFunc(func(payload json.RawMessage) error {
			return p.sendMCPForSession(deviceID, generation, info.SessionID, payload)
		}),
		p.clk, p.log,
		func() int64 { return idSource.Add(1) },
	)
	resp := wire.HelloResponse{
		Type:      wire.TypeHello,
		Transport: "udp",
		SessionID: info.SessionID,
		AudioParams: wire.AudioParams{
			Format:        p.cfg.Audio.Downlink.Format,
			SampleRate:    p.cfg.Audio.Downlink.SampleRate,
			Channels:      p.cfg.Audio.Downlink.Channels,
			FrameDuration: p.cfg.Audio.Downlink.FrameDuration,
		},
		UDP: wire.UDPServer{
			Server: p.cfg.Network.AdvertiseHost,
			Port:   p.transport.LocalAddr().Port,
			Key:    info.KeyHex,
			Nonce:  info.NonceHex,
		},
	}
	payload, err := wire.EncodeHelloResponse(resp)
	if err != nil {
		p.setDeviceError(deviceID, err)
		return
	}
	// A newer takeover may have happened while the HELLO was being assembled.
	// In that case this session must not become the probe's current mapping.
	if cur, ok := p.broker.Online(deviceID); !ok || cur != generation {
		_ = p.sessions.Goodbye(deviceID, info.SessionID)
		p.log.Warn("HELLO superseded during processing", "device", deviceID, "generation", generation)
		return
	}
	p.mu.Lock()
	st = p.states[deviceID]
	if st == nil || st.generation > generation {
		p.mu.Unlock()
		return
	}
	if st.decoder != nil {
		_ = st.decoder.Close()
	}
	oldClient := st.mcp
	st.mcp = mcpClient
	st.online = true
	st.generation = generation
	st.sessionID = info.SessionID
	st.decoder = decoder
	st.pcm = nil
	st.tools = nil
	st.binding = nil
	p.mu.Unlock()
	if oldClient != nil {
		oldClient.Close()
	}
	if err := p.broker.Publish(deviceID, payload); err != nil {
		p.setDeviceError(deviceID, fmt.Errorf("hello reply publish: %w", err))
		_ = p.sessions.Goodbye(deviceID, info.SessionID)
	}
}

func (p *Probe) sendMCPForSession(deviceID string, generation uint64, sessionID string, payload json.RawMessage) error {
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil || st.generation != generation || st.sessionID != sessionID {
		p.mu.Unlock()
		return fmt.Errorf("%w: stale device session", devicemcp.ErrSend)
	}
	p.mu.Unlock()
	msg, err := wire.EncodeMCP(payload)
	if err != nil {
		return fmt.Errorf("%w: %v", devicemcp.ErrSend, err)
	}
	if err := p.broker.Publish(deviceID, msg); err != nil {
		return fmt.Errorf("%w: %v", devicemcp.ErrSend, err)
	}
	return nil
}

func (p *Probe) handleListen(deviceID string, generation uint64, m wire.Listen) {
	if !p.currentGeneration(deviceID, generation) {
		return
	}
	switch m.State {
	case wire.ListenStart:
		p.startListening(deviceID, generation, m.Mode)
	case wire.ListenStop:
		p.stopListening(deviceID, generation)
	}
}

func (p *Probe) startListening(deviceID string, generation uint64, mode string) {
	p.cancelDeviceTasks(deviceID)
	ctx, cancel := context.WithCancel(p.ctx)
	decoder, err := audio.NewDecoder(audio.UplinkParams())
	if err != nil {
		cancel()
		p.setDeviceError(deviceID, err)
		return
	}
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil || !st.online || st.generation != generation || st.sessionID == "" {
		p.mu.Unlock()
		cancel()
		_ = decoder.Close()
		return
	}
	if st.listenCancel != nil {
		st.listenCancel()
	}
	if st.decoder != nil {
		_ = st.decoder.Close()
	}
	st.listening = true
	st.listenMode = mode
	st.listenCancel = cancel
	st.listenGen = generation
	st.listenSession = st.sessionID
	st.decoder = decoder
	st.pcm = nil
	p.mu.Unlock()

	if p.tasks.Add() {
		go func() {
			defer p.tasks.Done()
			select {
			case <-ctx.Done():
			case <-p.clk.After(p.listenMax):
				p.stopListening(deviceID, generation)
			}
		}()
	}
}

func (p *Probe) stopListening(deviceID string, generation uint64) {
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil || st.generation != generation || !st.listening {
		p.mu.Unlock()
		return
	}
	st.listening = false
	st.listenMode = ""
	if st.listenCancel != nil {
		st.listenCancel()
		st.listenCancel = nil
	}
	pcm := append([]int16(nil), st.pcm...)
	st.pcm = nil
	if st.decoder != nil {
		_ = st.decoder.Close()
		st.decoder = nil
	}
	st.pipelineBusy = true
	p.mu.Unlock()
	if p.up == nil || len(pcm) == 0 {
		p.mu.Lock()
		if st2 := p.states[deviceID]; st2 != nil {
			st2.pipelineBusy = false
		}
		p.mu.Unlock()
		return
	}
	if p.tasks.Add() {
		go func() {
			defer p.tasks.Done()
			p.runPipeline(deviceID, generation, pcm)
		}()
	} else {
		p.mu.Lock()
		if st2 := p.states[deviceID]; st2 != nil {
			st2.pipelineBusy = false
		}
		p.mu.Unlock()
	}
}

func (p *Probe) runPipeline(deviceID string, generation uint64, pcm []int16) {
	defer func() {
		p.mu.Lock()
		if st := p.states[deviceID]; st != nil && st.generation == generation {
			st.pipelineBusy = false
		}
		p.mu.Unlock()
	}()
	base, cancel := context.WithTimeout(p.ctx, limits.UpstreamOpMax)
	defer cancel()
	p.mu.Lock()
	st := p.states[deviceID]
	var sessionID string
	if st != nil {
		sessionID = st.sessionID
		st.pipelineCancel = cancel
	}
	p.mu.Unlock()
	ctx := base
	text, err := p.up.Transcribe(ctx, pcm)
	if err != nil {
		if ctx.Err() == nil {
			p.setDeviceError(deviceID, fmt.Errorf("upstream ASR: %w", err))
		}
		return
	}
	frames, err := p.up.Synthesize(ctx, text)
	if err != nil {
		if ctx.Err() == nil {
			p.setDeviceError(deviceID, fmt.Errorf("upstream TTS: %w", err))
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	if err := p.playFrames(ctx, deviceID, generation, sessionID, frames); err != nil {
		if !errors.Is(err, playout.ErrAborted) {
			p.setDeviceError(deviceID, err)
		}
	}
}

func (p *Probe) playFrames(ctx context.Context, deviceID string, generation uint64, sessionID string, frames [][]byte) error {
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil || st.generation != generation || st.sessionID != sessionID {
		p.mu.Unlock()
		return errors.New("probe: session changed before playback")
	}
	player := st.player
	playCtx, cancel := context.WithCancel(ctx)
	st.playCancel = cancel
	st.playing = true
	p.counters.playbackRounds++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if st2 := p.states[deviceID]; st2 != nil {
			st2.playing = false
			if st2.playCancel != nil {
				st2.playCancel = nil
			}
		}
		p.mu.Unlock()
	}()
	sink := sinkFunc(func(frame []byte) error {
		if err := p.sessions.SendAudio(deviceID, frame); err != nil {
			return err
		}
		return nil
	})
	markers := &ttsMarkers{p: p, deviceID: deviceID, generation: generation, sessionID: sessionID}
	// Offer every frame; the bounded queue rejects and counts overflow instead
	// of growing. The scheduler never catches up by bursting after a stall.
	for _, f := range frames {
		player.Push(f)
	}
	player.FinishInput()
	err := player.StartRound(playCtx, sink, markers)
	cancel()
	if errors.Is(err, playout.ErrAborted) {
		return err
	}
	if err != nil {
		return fmt.Errorf("probe: playback: %w", err)
	}
	return nil
}

type sinkFunc func([]byte) error

func (f sinkFunc) SendFrame(b []byte) error { return f(b) }

type senderFunc func(json.RawMessage) error

func (f senderFunc) SendMCP(payload json.RawMessage) error { return f(payload) }

type ttsMarkers struct {
	p          *Probe
	deviceID   string
	generation uint64
	sessionID  string
}

func (m *ttsMarkers) TTSStart() error { return m.publish(wire.TTSStart) }
func (m *ttsMarkers) TTSStop() error  { return m.publish(wire.TTSStop) }

func (m *ttsMarkers) publish(state string) error {
	m.p.mu.Lock()
	st := m.p.states[m.deviceID]
	ok := st != nil && st.online && st.generation == m.generation && st.sessionID == m.sessionID
	m.p.mu.Unlock()
	if !ok {
		return errors.New("probe: session no longer current; TTS marker not published")
	}
	payload, err := wire.EncodeTTS(state, m.sessionID)
	if err != nil {
		return err
	}
	return m.p.broker.Publish(m.deviceID, payload)
}

// cancelDeviceTasks cancels listening, playback and upstream pipeline work for
// one device without touching the MQTT online registration or UDP session.
func (p *Probe) cancelDeviceTasks(deviceID string) {
	p.mu.Lock()
	st := p.states[deviceID]
	var listens, plays, pipeline context.CancelFunc
	if st != nil {
		listens = st.listenCancel
		plays = st.playCancel
		pipeline = st.pipelineCancel
		st.listenCancel = nil
		st.playCancel = nil
		st.pipelineCancel = nil
		if st.player != nil {
			st.player.Abort()
		}
		st.listening = false
		st.listenMode = ""
		st.playing = false
		st.pipelineBusy = false
		st.pcm = nil
		if st.decoder != nil {
			_ = st.decoder.Close()
			st.decoder = nil
		}
	}
	p.mu.Unlock()
	for _, c := range []context.CancelFunc{listens, plays, pipeline} {
		if c != nil {
			c()
		}
	}
}

func (p *Probe) cancelPlayback(deviceID string) {
	p.mu.Lock()
	st := p.states[deviceID]
	var cancel context.CancelFunc
	if st != nil {
		cancel = st.playCancel
		if st.player != nil {
			st.player.Abort()
		}
		st.playing = false
		st.playCancel = nil
	}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *Probe) handleAbort(deviceID string, generation uint64, _ wire.Abort) {
	if !p.currentGeneration(deviceID, generation) {
		return
	}
	// Playback abort cancels only the current round and keeps MQTT plus a valid
	// UDP session. Abort during listening ends the round without synthesising a
	// response (reference behaviour), also keeping the session.
	p.cancelDeviceTasks(deviceID)
}

func (p *Probe) stopListeningAborted(deviceID string, generation uint64) {
	p.mu.Lock()
	st := p.states[deviceID]
	if st == nil || st.generation != generation || !st.listening {
		p.mu.Unlock()
		return
	}
	st.listening = false
	st.listenMode = ""
	if st.listenCancel != nil {
		st.listenCancel()
		st.listenCancel = nil
	}
	st.pcm = nil
	if st.decoder != nil {
		_ = st.decoder.Close()
		st.decoder = nil
	}
	p.mu.Unlock()
}

func (p *Probe) handleGoodbye(deviceID string, generation uint64, m wire.Goodbye) {
	if !p.currentGeneration(deviceID, generation) {
		return
	}
	p.cancelDeviceTasks(deviceID)
	p.mu.Lock()
	st := p.states[deviceID]
	var sid string
	var client *devicemcp.Client
	if st != nil {
		sid = st.sessionID
		client = st.mcp
		st.mcp = nil
		st.sessionID = ""
		st.tools = nil
		st.binding = nil
		st.pipelineBusy = false
	}
	p.mu.Unlock()
	if client != nil {
		client.Close()
	}
	if sid != "" {
		_ = p.sessions.Goodbye(deviceID, m.SessionID)
	}
}

func (p *Probe) handleMCP(deviceID string, generation uint64, m wire.MCP) {
	p.mu.Lock()
	st := p.states[deviceID]
	var client *devicemcp.Client
	var sessionID string
	if st != nil {
		client = st.mcp
		sessionID = st.sessionID
	}
	p.mu.Unlock()
	if client == nil {
		return
	}
	// The MQTT application payload has no session tag, so a response cannot be
	// labelled with the session that issued its request. Instead, request ids
	// are monotonic across the device's session rotations (mcpID above), and
	// the closed client's pending requests were cancelled: an old response id
	// cannot match any pending id in the replacement client.
	client.HandleMessage(devicemcp.Envelope{
		DeviceID:   deviceID,
		Generation: generation,
		SessionID:  sessionID,
		Payload:    m.Payload,
	})
}

func (p *Probe) onUDPFrame(frame session.InboundFrame) {
	p.mu.Lock()
	st := p.states[frame.DeviceID]
	if st == nil || st.sessionID != frame.SessionID {
		p.mu.Unlock()
		return
	}
	if !st.listening || st.decoder == nil {
		p.counters.uplinkDropped++
		p.mu.Unlock()
		return
	}
	decoder := st.decoder
	pcm, err := decoder.Decode(frame.Payload)
	if err != nil {
		st.decodeErrors++
		p.mu.Unlock()
		return
	}
	maxSamples := int(p.listenMax.Seconds()*float64(audio.UplinkParams().SampleRate)) + audio.UplinkParams().FrameSamples()
	if len(st.pcm)+len(pcm) > maxSamples {
		pcm = pcm[:max(0, maxSamples-len(st.pcm))]
	}
	st.pcm = append(st.pcm, pcm...)
	st.uplinkFrames++
	p.mu.Unlock()
}

func (p *Probe) onSessionEnded(e session.SessionEnd) {
	p.mu.Lock()
	st := p.states[e.DeviceID]
	var client *devicemcp.Client
	if st != nil && st.sessionID == e.SessionID {
		st.sessionID = ""
		client = st.mcp
		st.mcp = nil
		st.tools = nil
		st.binding = nil
	}
	p.mu.Unlock()
	if client != nil {
		client.Close()
	}
	p.cancelDeviceTasks(e.DeviceID)
}

// onOnline follows the broker's authenticated registration/generation bump.
// For a duplicate-client-id takeover the old generation is dropped here, so a
// new connection's HELLO is accepted even though its predecessor's probe state
// is still present. The new MQTT registration itself is never cleared.
func (p *Probe) onOnline(ev broker.OnlineEvent) {
	p.mu.Lock()
	st := p.states[ev.DeviceID]
	if st == nil {
		p.mu.Unlock()
		return
	}
	oldLocalGen := st.generation
	replaced := ev.Replaced || (oldLocalGen != 0 && oldLocalGen != ev.Generation)
	st.online = true
	st.generation = ev.Generation
	var client *devicemcp.Client
	if replaced {
		client = st.mcp
		st.mcp = nil
		st.sessionID = ""
		st.tools = nil
		st.binding = nil
		st.pipelineBusy = false
	}
	p.mu.Unlock()
	if !replaced {
		return
	}
	oldGen := ev.PreviousGeneration
	if oldGen == 0 {
		oldGen = oldLocalGen
	}
	if oldGen != 0 {
		p.sessions.DropGeneration(ev.DeviceID, oldGen)
	}
	p.cancelDeviceTasks(ev.DeviceID)
	if client != nil {
		client.Close()
	}
}

func (p *Probe) onOffline(ev broker.OfflineEvent) {
	p.mu.Lock()
	st := p.states[ev.DeviceID]
	if st == nil || st.generation != ev.Generation {
		p.mu.Unlock()
		return
	}
	st.online = false
	client := st.mcp
	st.mcp = nil
	st.sessionID = ""
	st.tools = nil
	st.binding = nil
	st.pipelineBusy = false
	if st.listenCancel != nil {
		st.listenCancel()
		st.listenCancel = nil
	}
	st.listening = false
	if st.decoder != nil {
		_ = st.decoder.Close()
		st.decoder = nil
	}
	p.mu.Unlock()
	p.sessions.DropGeneration(ev.DeviceID, ev.Generation)
	p.cancelDeviceTasks(ev.DeviceID)
	if client != nil {
		client.Close()
	}
}

func (p *Probe) currentGeneration(deviceID string, generation uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[deviceID]
	return st != nil && st.online && st.generation == generation
}

func (p *Probe) setDeviceError(deviceID string, err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if st := p.states[deviceID]; st != nil {
		st.lastError = err.Error()
	}
	p.mu.Unlock()
	p.log.Warn("device operation failed", "device", deviceID, "error", err.Error())
}

// taskGroup tracks background probe work and lets Close wait for it without
// racing a WaitGroup Add against Wait during shutdown.
type taskGroup struct {
	mu     sync.Mutex
	cond   *sync.Cond
	active int
	closed bool
}

func newTaskGroup() *taskGroup {
	g := &taskGroup{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Add registers one task; it returns false once CloseAndWait has started.
func (g *taskGroup) Add() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.active++
	return true
}

// Done marks one task complete.
func (g *taskGroup) Done() {
	g.mu.Lock()
	g.active--
	if g.active == 0 {
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

// CloseAndWait rejects new tasks and waits for all registered tasks to finish.
func (g *taskGroup) CloseAndWait() {
	g.mu.Lock()
	g.closed = true
	for g.active > 0 {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// discardWriter drops logger output; used only when no writer was injected.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
