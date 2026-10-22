// Package simulator provides the synthetic counterparties for offline M0
// verification: a simulated firmware device (MQTT control + UDP audio + device
// MCP) and a simulated upstream (ASR/LLM/TTS). It never talks to a real
// device, cloud service or Home Assistant.
package simulator

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
	"github.com/homevoice/homevoice/internal/m0/udp"
	"github.com/homevoice/homevoice/internal/m0/wire"
)

// Received is one downlink message observed by the simulated device.
type Received struct {
	Type    string
	Topic   string
	Payload []byte
	At      time.Time
	Notify  *wire.Notify
	TTS     *wire.TTS
	MCP     *wire.MCP
}

// DownlinkAudio is one decrypted UDP frame received by the device.
type DownlinkAudio struct {
	Header  udp.Header
	Payload []byte
	At      time.Time
}

// DeviceOptions configures the simulated firmware.
type DeviceOptions struct {
	Logger           *slog.Logger
	Clock            clock.Clock
	KeepAliveSeconds uint16
	MCPServer        *MCPServer
	// FirstUDPSequence is the first outbound UDP sequence. The protocol asks
	// the probe to record first-sequence-0 compatibility explicitly; 0 is a
	// legal simulator setting.
	FirstUDPSequence uint32
	// AutoSubscribeDownlink controls the normal per-device downlink
	// subscription. Tests may disable it to prove the broker ACL.
	AutoSubscribeDownlink bool
}

func (o *DeviceOptions) defaults() {
	if o.Logger == nil {
		o.Logger = logging.Discard()
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.KeepAliveSeconds == 0 {
		o.KeepAliveSeconds = 30
	}
	o.AutoSubscribeDownlink = true
}

// Device is a simulated MQTT 3.1.1 device with a UDP audio socket and an
// optional device MCP server.
type Device struct {
	cfg  config.Device
	opts DeviceOptions
	clk  clock.Clock

	mu          sync.Mutex
	client      mqtt.Client
	udpConn     *net.UDPConn
	serverUDP   *net.UDPAddr
	hello       *wire.HelloResponse
	helloCh     chan wire.HelloResponse
	received    []Received
	audioFrames []DownlinkAudio
	mcpRequests [][]byte
	connected   bool
	closed      bool
	linkErr     error
	udpSeq      uint32
	udpTS       uint32
	notifyCh    chan struct{}
	closeOnce   sync.Once
	udpDone     chan struct{}
}

// NewDevice creates a simulated device for one synthetic identity.
func NewDevice(cfg config.Device, opts DeviceOptions) (*Device, error) {
	opts.defaults()
	if cfg.ClientID == "" || cfg.Username == "" || cfg.Password == "" ||
		cfg.PublishTopic == "" || cfg.SubscribeTopic == "" {
		return nil, errors.New("simulator: incomplete device identity")
	}
	d := &Device{
		cfg:      cfg,
		opts:     opts,
		clk:      opts.Clock,
		helloCh:  make(chan wire.HelloResponse, 1),
		notifyCh: make(chan struct{}, 1),
		udpDone:  make(chan struct{}),
		udpSeq:   opts.FirstUDPSequence,
		udpTS:    60,
	}
	if d.opts.MCPServer == nil {
		d.opts.MCPServer = DefaultMCPServer()
	}
	return d, nil
}

// Connect establishes the MQTT connection and (by default) subscribes to the
// device's exact downlink topic.
func (d *Device) Connect(ctx context.Context, brokerURL string) error {
	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(d.cfg.ClientID).
		SetUsername(d.cfg.Username).
		SetPassword(d.cfg.Password).
		SetProtocolVersion(4).
		SetCleanSession(true).
		SetAutoReconnect(false).
		SetKeepAlive(time.Duration(d.opts.KeepAliveSeconds) * time.Second).
		SetConnectTimeout(limits.MQTTConnectTimeout).
		SetDefaultPublishHandler(func(_ mqtt.Client, msg mqtt.Message) {
			d.handleDownlink(msg.Topic(), msg.Payload())
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			d.mu.Lock()
			d.linkErr = err
			d.connected = false
			d.mu.Unlock()
			d.signal()
		}).
		SetOnConnectHandler(func(_ mqtt.Client) {
			d.mu.Lock()
			d.connected = true
			d.mu.Unlock()
			d.signal()
		})
	client := mqtt.NewClient(opts)
	tok := client.Connect()
	timeout := limits.MQTTConnectTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remain := time.Until(dl); remain < timeout {
			timeout = remain
		}
	}
	if !tok.WaitTimeout(timeout) {
		return errors.New("simulator: mqtt connect timeout")
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("simulator: mqtt connect: %w", err)
	}
	d.mu.Lock()
	d.client = client
	d.mu.Unlock()
	if d.opts.AutoSubscribeDownlink {
		st := client.Subscribe(d.cfg.SubscribeTopic, 1, nil)
		if !st.WaitTimeout(timeout) {
			return errors.New("simulator: subscribe timeout")
		}
		if err := st.Error(); err != nil {
			return fmt.Errorf("simulator: subscribe: %w", err)
		}
	}
	return nil
}

// Disconnect sends an MQTT DISCONNECT while keeping the object reusable.
func (d *Device) Disconnect() {
	d.mu.Lock()
	client := d.client
	d.client = nil
	d.connected = false
	d.mu.Unlock()
	if client != nil {
		client.Disconnect(100)
	}
}

// PublishRaw publishes an arbitrary uplink payload on the device's exact
// publish topic (used by negative tests for payload-identity spoofing).
func (d *Device) PublishRaw(payload []byte) error {
	return d.publish(payload)
}

// SendHello publishes a HELLO and waits for the server reply.
func (d *Device) SendHello(ctx context.Context, transport string, version int) (wire.HelloResponse, error) {
	if transport == "" {
		transport = "udp"
	}
	if version == 0 {
		version = 3
	}
	body := map[string]any{
		"type":      "hello",
		"version":   version,
		"transport": transport,
		"features":  map[string]bool{"mcp": true},
		"audio_params": map[string]any{
			"format":         "opus",
			"sample_rate":    16000,
			"channels":       1,
			"frame_duration": 60,
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return wire.HelloResponse{}, err
	}
	// Replace any buffered stale reply before sending.
	select {
	case <-d.helloCh:
	default:
	}
	if err := d.publish(payload); err != nil {
		return wire.HelloResponse{}, err
	}
	deadline := time.NewTimer(limits.HelloWait)
	defer deadline.Stop()
	select {
	case h := <-d.helloCh:
		return h, nil
	case <-ctx.Done():
		return wire.HelloResponse{}, ctx.Err()
	case <-deadline.C:
		return wire.HelloResponse{}, errors.New("simulator: HELLO reply timeout")
	}
}

// SendListen publishes listen start/stop.
func (d *Device) SendListen(state, mode string) error {
	body := map[string]any{"type": "listen", "state": state}
	if mode != "" {
		body["mode"] = mode
	}
	payload, _ := json.Marshal(body)
	return d.publish(payload)
}

// SendAbort publishes an abort.
func (d *Device) SendAbort(reason string) error {
	payload, _ := json.Marshal(map[string]any{"type": "abort", "reason": reason})
	return d.publish(payload)
}

// SendGoodbye releases the UDP audio session while keeping MQTT connected.
func (d *Device) SendGoodbye() error {
	d.mu.Lock()
	var sid string
	if d.hello != nil {
		sid = d.hello.SessionID
	}
	d.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"type": "goodbye", "session_id": sid})
	return d.publish(payload)
}

// SendPCM segments synthetic PCM and sends it as Opus UDP frames using the
// current HELLO key/template.
func (d *Device) SendPCM(pcm []int16) error {
	enc, err := audio.NewEncoder(audio.UplinkParams())
	if err != nil {
		return err
	}
	defer enc.Close()
	frames, err := enc.EncodeSamples(pcm)
	if err != nil {
		return err
	}
	for _, f := range frames {
		if err := d.SendOpusFrame(f); err != nil {
			return err
		}
	}
	return nil
}

// SendOpusFrame encrypts and sends one uplink Opus frame on the device UDP
// socket. The server learns the peer from the first valid frame.
func (d *Device) SendOpusFrame(frame []byte) error {
	d.mu.Lock()
	h := d.hello
	conn := d.udpConn
	server := d.serverUDP
	seq := d.udpSeq
	ts := d.udpTS
	d.udpSeq++
	d.udpTS += 60
	d.mu.Unlock()
	if h == nil || conn == nil || server == nil {
		return errors.New("simulator: no UDP session established (send HELLO first)")
	}
	key, err := hex.DecodeString(h.UDP.Key)
	if err != nil || len(key) != 16 {
		return errors.New("simulator: invalid HELLO key")
	}
	nonce, err := hex.DecodeString(h.UDP.Nonce)
	if err != nil || len(nonce) != 16 {
		return errors.New("simulator: invalid HELLO nonce")
	}
	tpl, err := udp.ParseHeader(nonce)
	if err != nil {
		return err
	}
	var keyArr [16]byte
	copy(keyArr[:], key)
	packet, err := udp.Encrypt(keyArr, udp.Header{
		PacketType: tpl.PacketType,
		Flags:      tpl.Flags,
		SSRC:       tpl.SSRC,
		Timestamp:  ts,
		Sequence:   seq,
	}, frame)
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDP(packet, server)
	return err
}

// SendRawUDP sends an arbitrary datagram to the HELLO UDP endpoint (negative
// tests: wrong template, oversized packet, new source, etc.).
func (d *Device) SendRawUDP(packet []byte) error {
	d.mu.Lock()
	conn := d.udpConn
	server := d.serverUDP
	d.mu.Unlock()
	if conn == nil || server == nil {
		return errors.New("simulator: no UDP session")
	}
	_, err := conn.WriteToUDP(packet, server)
	return err
}

// Hello returns the last HELLO reply (including synthetic key material); tests
// use it to prove logs/evidence do not leak keys. It must not be written to
// reports.
func (d *Device) Hello() *wire.HelloResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hello
}

// MCP returns the synthetic device MCP server (call counts/recorded calls).
func (d *Device) MCP() *MCPServer { return d.opts.MCPServer }

// LocalUDPAddr returns the device UDP source address.
func (d *Device) LocalUDPAddr() *net.UDPAddr {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.udpConn == nil {
		return nil
	}
	return d.udpConn.LocalAddr().(*net.UDPAddr)
}

// Messages returns a copy of received downlink application messages.
func (d *Device) Messages() []Received {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Received(nil), d.received...)
}

// Notifications returns received idle notifications.
func (d *Device) Notifications() []wire.Notify {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []wire.Notify
	for _, m := range d.received {
		if m.Type == wire.TypeNotify && m.Notify != nil {
			out = append(out, *m.Notify)
		}
	}
	return out
}

// AudioFrames returns decrypted downlink audio frames.
func (d *Device) AudioFrames() []DownlinkAudio {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DownlinkAudio(nil), d.audioFrames...)
}

// MCPRequests returns raw MCP requests seen by the device.
func (d *Device) MCPRequests() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.mcpRequests...)
}

// WaitFor blocks until predicate is true or the timeout elapses.
func (d *Device) WaitFor(predicate func() bool, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if predicate() {
			return true
		}
		select {
		case <-d.notifyCh:
		case <-deadline.C:
			return predicate()
		case <-d.udpDone:
			return predicate()
		}
	}
}

// WaitNotify waits for one idle notification.
func (d *Device) WaitNotify(timeout time.Duration) (wire.Notify, bool) {
	var got wire.Notify
	ok := d.WaitFor(func() bool {
		n := d.Notifications()
		if len(n) == 0 {
			return false
		}
		got = n[len(n)-1]
		return true
	}, timeout)
	return got, ok
}

// Close tears down MQTT and UDP resources.
func (d *Device) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.Disconnect()
		d.mu.Lock()
		d.closed = true
		conn := d.udpConn
		d.udpConn = nil
		d.mu.Unlock()
		close(d.udpDone)
		if conn != nil {
			err = conn.Close()
		}
	})
	return err
}

// LinkError returns the last MQTT connection-lost error, if any.
func (d *Device) LinkError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.linkErr
}

func (d *Device) publish(payload []byte) error {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		return errors.New("simulator: mqtt not connected")
	}
	if len(payload) > limits.MaxJSONBytes {
		return wire.ErrTooLarge
	}
	tok := client.Publish(d.cfg.PublishTopic, 1, false, payload)
	if !tok.WaitTimeout(limits.MQTTConnectTimeout) {
		return errors.New("simulator: mqtt publish timeout")
	}
	return tok.Error()
}

func (d *Device) handleDownlink(topic string, payload []byte) {
	msg := Received{Topic: topic, Payload: append([]byte(nil), payload...), At: d.clk.Now()}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &env); err == nil {
		msg.Type = env.Type
	}
	switch msg.Type {
	case wire.TypeHello:
		if h, err := wire.ParseHelloResponse(payload); err == nil {
			msg.Type = wire.TypeHello
			d.applyHello(h)
			select {
			case d.helloCh <- h:
			default:
			}
		}
	case wire.TypeNotify:
		if n, err := wire.ParseNotify(payload); err == nil {
			msg.Type = wire.TypeNotify
			msg.Notify = &n
		}
	case wire.TypeTTS:
		if t, err := wire.ParseTTS(payload); err == nil {
			msg.Type = wire.TypeTTS
			msg.TTS = &t
		}
	case wire.TypeMCP:
		m, err := wire.ParseMCPDownlink(payload)
		if err != nil {
			return
		}
		msg.Type = wire.TypeMCP
		msg.MCP = &m
		d.mu.Lock()
		d.mcpRequests = append(d.mcpRequests, append([]byte(nil), payload...))
		d.mu.Unlock()
		// Respond outside the MQTT callback's critical path.
		go func() {
			resp, ok := d.opts.MCPServer.Handle(m.Payload)
			if !ok {
				return
			}
			out, err := wire.EncodeMCP(resp)
			if err != nil {
				return
			}
			_ = d.publish(out)
		}()
	}
	d.mu.Lock()
	d.received = append(d.received, msg)
	d.mu.Unlock()
	d.signal()
}

func (d *Device) applyHello(h wire.HelloResponse) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hello = &h
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", h.UDP.Server, h.UDP.Port))
	if err != nil {
		return
	}
	d.serverUDP = serverAddr
	if d.udpConn == nil {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			return
		}
		d.udpConn = conn
		go d.readUDP(conn)
	}
	d.udpSeq = d.opts.FirstUDPSequence
	d.udpTS = 60
}

func (d *Device) readUDP(conn *net.UDPConn) {
	buf := make([]byte, limits.MaxUDPDatagramBytes+16)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-d.udpDone:
				return
			default:
				return
			}
		}
		d.mu.Lock()
		h := d.hello
		d.mu.Unlock()
		if h == nil {
			continue
		}
		key, err := hex.DecodeString(h.UDP.Key)
		if err != nil || len(key) != 16 {
			continue
		}
		var keyArr [16]byte
		copy(keyArr[:], key)
		header, payload, err := udp.Decrypt(keyArr, buf[:n])
		if err != nil {
			continue
		}
		d.mu.Lock()
		d.audioFrames = append(d.audioFrames, DownlinkAudio{Header: header, Payload: payload, At: d.clk.Now()})
		d.mu.Unlock()
		d.signal()
	}
}

func (d *Device) signal() {
	select {
	case d.notifyCh <- struct{}{}:
	default:
	}
}
