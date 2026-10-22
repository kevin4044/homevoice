// Package broker is the M0 MQTT device-facing transport: an isolated MQTT
// 3.1.1 broker built on mochi-mqtt with up to two pre-registered synthetic
// identities.
//
// The broker is deliberately small and security-focused. Identity comes only
// from credentials verified on the MQTT connection (never from payloads),
// topics are granted exactly per device, control publishes must not be
// retained, and every login has a monotonically increasing connection
// generation so a replaced connection can never clear or feed the new one.
//
// It is probe tooling, not a production registry or a public MQTT service.
// Plaintext is only acceptable for explicitly approved isolated tests; TLS is
// supported through Options.TLSConfig and is never silently downgraded.
package broker

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
)

// Message is one trusted device-to-server uplink message.
type Message struct {
	DeviceID   string
	Generation uint64
	Topic      string
	Payload    []byte // copy; the broker does not retain caller-owned slices
	QoS        byte
}

// OfflineEvent reports that the currently registered MQTT session went away.
// Generation identifies the session that was lost; a superseded session never
// produces an event for its replacement.
type OfflineEvent struct {
	DeviceID   string
	Generation uint64
	Reason     string
}

// Options configures a Broker.
//
// OnMessage and OnOffline are invoked synchronously from broker goroutines
// (the MQTT connection goroutine and the connection cleanup path
// respectively). They must not block for long and must not call Close.
// They are always called without Broker locks held, so calling Publish,
// Online or Disconnect from a callback is safe. OnOffline is not called for a
// superseded duplicate-client-id connection.
type Options struct {
	// BindAddr is the required host:port listener address. Wildcard and
	// public binds are rejected by config.ValidateBindAddress; port 0
	// requests an ephemeral test port.
	BindAddr string

	// Devices is the exact pre-registered identity set (probe limit: 2).
	Devices []config.Device

	// TLSConfig optionally enables TLS on the listener. There is no
	// plaintext fallback after a TLS or certificate failure.
	TLSConfig *tls.Config

	// Logger receives sanitised broker and MQTT-library logs. Nil discards
	// them. Credentials, keys and payloads are never logged.
	Logger *slog.Logger

	// MaxPacketBytes is the largest accepted or emitted MQTT packet
	// including the fixed header. Zero uses limits.MaxMQTTPacketBytes
	// (64 KiB); larger values are rejected to keep the probe bound.
	MaxPacketBytes uint32

	// ConnectTimeout bounds the MQTT CONNECT handshake on a new connection.
	// Zero uses limits.MQTTConnectTimeout (5s); larger values are rejected.
	ConnectTimeout time.Duration

	// PublishQoS is the downlink QoS. Zero means the default (QoS1). Set
	// PublishQoSZero to force QoS0. QoS2 is rejected.
	PublishQoS byte

	// PublishQoSZero forces QoS0 downlink publishes when the caller does not
	// want the default QoS1. It cannot be combined with PublishQoS: 1.
	PublishQoSZero bool

	// AuthorizedPrivateCIDRs allows explicit isolated private bind ranges
	// (loopback is always allowed). It never permits a wildcard or public
	// bind.
	AuthorizedPrivateCIDRs []string

	// OnMessage receives attributed uplink messages.
	OnMessage func(Message)

	// OnOffline receives the loss of the currently registered session.
	OnOffline func(OfflineEvent)
}

// Broker owns the MQTT server, its listener and the trusted session registry.
type Broker struct {
	opts     Options
	log      *slog.Logger
	redactor *logging.Redactor

	devices    []config.Device
	byUsername map[string]config.Device

	server   *mqtt.Server
	listener *deadlineListener
	hook     *identityHook

	mu            sync.Mutex
	online        map[string]*session
	generations   map[string]uint64
	forcedReasons map[*mqtt.Client]string
	superseded    map[*mqtt.Client]struct{}
	started       bool
	closed        bool

	closeOnce sync.Once
}

type session struct {
	device     config.Device
	generation uint64
	client     *mqtt.Client
}

// Stable, secret-free offline/disconnect reasons.
const (
	reasonClientDisconnect = "client_disconnect"
	reasonSessionTakenOver = "session_taken_over"
	reasonKeepaliveTimeout = "keepalive_timeout"
	reasonPeerClosed       = "peer_closed"
	reasonConnectionError  = "connection_error"
	reasonNotAuthorized    = "not_authorized"
	reasonRejected         = "rejected"
	reasonAdministrative   = "administrative"

	maxCallbackReasonBytes = 128
	defaultListenerID      = "mqtt"
	// takeoverCleanupWait bounds how long registering a replacement connection
	// waits for the superseded connection's broker cleanup. The wait is
	// normally sub-millisecond; the bound protects the CONNECT handshake.
	takeoverCleanupWait = 2 * time.Second
)

var (
	// ErrOffline reports that Publish or Disconnect targeted a device without
	// an authenticated, online MQTT session.
	ErrOffline = errors.New("device offline")

	// ErrPayloadTooLarge reports a downlink payload that would exceed the
	// configured maximum MQTT packet size.
	ErrPayloadTooLarge = errors.New("downlink payload too large")

	// ErrClosed reports use of a broker that was already closed.
	ErrClosed = errors.New("broker closed")
)

// New validates options, binds the listener and prepares the MQTT server.
// The server does not accept connections until Start is called.
func New(opts Options) (*Broker, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}

	secrets := make([]string, 0, len(normalized.Devices)*2)
	for _, d := range normalized.Devices {
		secrets = append(secrets, d.Password, d.Username)
	}
	baseLogger := normalized.Logger
	if baseLogger == nil {
		baseLogger = logging.Discard()
	}
	safeLog := newSafeLogger(baseLogger, secrets)

	b := &Broker{
		opts:          normalized,
		log:           safeLog,
		redactor:      logging.NewRedactor(secrets...),
		devices:       append([]config.Device(nil), normalized.Devices...),
		byUsername:    make(map[string]config.Device, len(normalized.Devices)),
		online:        make(map[string]*session, len(normalized.Devices)),
		generations:   make(map[string]uint64, len(normalized.Devices)),
		forcedReasons: make(map[*mqtt.Client]string),
		superseded:    make(map[*mqtt.Client]struct{}),
	}
	for _, device := range b.devices {
		b.byUsername[device.Username] = device
	}

	caps := mqtt.NewDefaultServerCapabilities()
	caps.MaximumPacketSize = normalized.MaxPacketBytes
	caps.MinimumProtocolVersion = mqttProtocolLevel311
	caps.MaximumQos = 1
	caps.RetainAvailable = 0
	caps.WildcardSubAvailable = 0
	caps.SharedSubAvailable = 0
	caps.SubIDAvailable = 0

	// Bind before creating the MQTT server: a failed bind must not leave the
	// library's background tickers allocated.
	b.listener = newDeadlineListener(defaultListenerID, normalized.BindAddr, normalized.TLSConfig, normalized.ConnectTimeout)
	if err := b.listener.Init(safeLog); err != nil {
		return nil, fmt.Errorf("broker: listen %q: %w", normalized.BindAddr, err)
	}

	b.server = mqtt.New(&mqtt.Options{
		Capabilities: caps,
		InlineClient: true,
		Logger:       safeLog,
	})

	b.hook = &identityHook{b: b}
	if err := b.server.AddHook(b.hook, nil); err != nil {
		b.listener.Close(nil)
		return nil, fmt.Errorf("broker: add identity hook: %w", err)
	}
	if err := b.server.AddListener(b.listener); err != nil {
		b.listener.Close(nil)
		return nil, fmt.Errorf("broker: add listener: %w", err)
	}

	return b, nil
}

// Start begins serving MQTT connections. It is idempotent.
func (b *Broker) Start() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = true
	b.mu.Unlock()

	if err := b.server.Serve(); err != nil {
		b.mu.Lock()
		b.started = false
		b.mu.Unlock()
		return fmt.Errorf("broker: serve: %w", err)
	}
	b.log.Info("mqtt broker listening",
		"address", b.listener.Address(),
		"devices", len(b.devices),
		"protocol", b.listener.Protocol(),
	)
	return nil
}

// Close shuts down the listener, disconnects every client and stops the MQTT
// server. It is idempotent and may be called without Start.
func (b *Broker) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		if b.server != nil {
			_ = b.server.Close()
		}
		b.log.Info("mqtt broker stopped")
	})
	return nil
}

// Addr returns the bound listener address, including the actual port when
// BindAddr requested port 0.
func (b *Broker) Addr() string {
	if b.listener == nil {
		return ""
	}
	return b.listener.Address()
}

// Publish sends a non-retained downlink message to the device's exact
// SubscribeTopic. It returns ErrOffline when the device has no authenticated,
// online session, and ErrPayloadTooLarge when the payload cannot fit in one
// bounded MQTT packet.
func (b *Broker) Publish(deviceID string, payload []byte) error {
	b.mu.Lock()
	sess := b.online[deviceID]
	if sess == nil || sess.client == nil || sess.client.Closed() {
		b.mu.Unlock()
		return ErrOffline
	}
	topic := sess.device.SubscribeTopic
	qos := b.opts.PublishQoS
	maxPacket := int(b.opts.MaxPacketBytes)
	b.mu.Unlock()

	// Fixed header (<=5 bytes) + 2-byte topic length + topic + payload.
	if len(payload)+len(topic)+7 > maxPacket {
		return fmt.Errorf("%w: %d payload bytes on %q exceeds the %d-byte packet limit",
			ErrPayloadTooLarge, len(payload), topic, maxPacket)
	}

	if err := b.server.Publish(topic, payload, false, qos); err != nil {
		return fmt.Errorf("broker: publish downlink: %w", err)
	}
	b.log.Debug("mqtt downlink message", "device", deviceID, "topic", topic, "bytes", len(payload), "qos", qos)
	return nil
}

// Online reports whether the device has an authenticated, online MQTT session
// and returns that connection's generation. A superseded connection is never
// reported as online.
func (b *Broker) Online(deviceID string) (generation uint64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess := b.online[deviceID]
	if sess == nil || sess.client == nil || sess.client.Closed() {
		return 0, false
	}
	return sess.generation, true
}

// Disconnect closes the current MQTT session for deviceID and reports reason
// to OnOffline. It returns ErrOffline when no online session exists.
func (b *Broker) Disconnect(deviceID string, reason string) error {
	if reason == "" {
		reason = reasonAdministrative
	}
	b.mu.Lock()
	sess := b.online[deviceID]
	if sess == nil || sess.client == nil || sess.client.Closed() {
		b.mu.Unlock()
		return ErrOffline
	}
	b.forcedReasons[sess.client] = sanitizeReason(b.redactor, reason, maxCallbackReasonBytes)
	cl := sess.client
	b.mu.Unlock()

	// Sends an MQTT DISCONNECT (a zero-length one for MQTT 3.1.1) and closes
	// the connection. The returned reason code is expected; the callback
	// reports the operator-supplied reason.
	_ = b.server.DisconnectClient(cl, packets.ErrAdministrativeAction)
	return nil
}

// registerSession installs a new online registration and bumps the device's
// connection generation.
//
// A duplicate client id must replace the previous connection. mochi's own
// takeover path keys clients by client id and can race the old connection's
// cleanup against the new registration (the old cleanup can delete the new
// client from the shared client-id slot). To keep the M0 generation contract
// deterministic, the broker performs the takeover itself in
// OnSessionEstablish, before mochi adds the replacement: it marks the old
// connection superseded, closes it, and waits (bounded) until the old
// connection's cleanup has removed the old client. Only then is the new
// connection registered and returned to the library. mochi therefore sees no
// pre-existing client id and cannot produce a late delete of the new session.
func (b *Broker) registerSession(cl *mqtt.Client, device config.Device) {
	b.mu.Lock()
	old := b.online[device.DeviceID]
	var superseded *session
	if old != nil && old.client != cl {
		b.superseded[old.client] = struct{}{}
		superseded = old
	}
	b.mu.Unlock()

	if superseded != nil {
		b.log.Info("mqtt replacing duplicate client connection",
			"device", device.DeviceID,
			"old_generation", superseded.generation,
		)
		// MQTT 3.1.1 has no server-to-client DISCONNECT; closing the TCP
		// connection is the standard takeover signal.
		superseded.client.Stop(packets.ErrSessionTakenOver)
		b.waitClientCleanup(superseded.client)
	}

	b.mu.Lock()
	generation := b.generations[device.DeviceID] + 1
	b.generations[device.DeviceID] = generation
	b.online[device.DeviceID] = &session{device: device, generation: generation, client: cl}
	b.mu.Unlock()

	b.log.Info("mqtt device online",
		"device", device.DeviceID,
		"generation", generation,
	)
}

// waitClientCleanup waits until the library has removed a closed connection
// from the client-id table. The table entry is deleted after the old session's
// subscriptions and inflights are cleaned, so observing its removal means the
// superseded connection can no longer touch shared broker state.
func (b *Broker) waitClientCleanup(cl *mqtt.Client) {
	if cl == nil || b.server == nil {
		return
	}
	deadline := time.Now().Add(takeoverCleanupWait)
	for {
		current, ok := b.server.Clients.Get(cl.ID)
		if !ok || current != cl {
			return
		}
		if time.Now().After(deadline) {
			b.log.Warn("timeout waiting for superseded mqtt connection cleanup",
				"client_id", cl.ID)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// currentGeneration returns the session generation for deviceID only when cl
// is still the currently registered connection for that device.
func (b *Broker) currentGeneration(deviceID string, cl *mqtt.Client) (uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess := b.online[deviceID]
	if sess == nil || sess.client != cl {
		return 0, false
	}
	return sess.generation, true
}

// removeSession deletes the registration only if it still belongs to cl.
func (b *Broker) removeSession(cl *mqtt.Client, device config.Device, reason string) {
	b.mu.Lock()
	sess := b.online[device.DeviceID]
	current := sess != nil && sess.client == cl
	_, wasSuperseded := b.superseded[cl]
	delete(b.superseded, cl)
	if current {
		delete(b.online, device.DeviceID)
	}
	delete(b.forcedReasons, cl)
	b.mu.Unlock()

	if wasSuperseded {
		b.log.Debug("superseded mqtt connection closed", "device", device.DeviceID, "reason", reason)
		return
	}
	if !current {
		b.log.Debug("stale mqtt connection closed", "device", device.DeviceID, "reason", reason)
		return
	}
	b.log.Info("mqtt device offline",
		"device", device.DeviceID,
		"generation", sess.generation,
		"reason", reason,
	)
	if b.opts.OnOffline != nil {
		b.opts.OnOffline(OfflineEvent{
			DeviceID:   device.DeviceID,
			Generation: sess.generation,
			Reason:     reason,
		})
	}
}

func normalizeOptions(opts Options) (Options, error) {
	if opts.BindAddr == "" {
		return opts, errors.New("broker: BindAddr is required; default wildcard binds are not allowed")
	}
	if err := config.ValidateBindAddress(opts.BindAddr, opts.AuthorizedPrivateCIDRs); err != nil {
		return opts, fmt.Errorf("broker: %w", err)
	}
	if len(opts.Devices) == 0 {
		return opts, errors.New("broker: at least one pre-registered device is required")
	}
	if len(opts.Devices) > limits.MaxDevices {
		return opts, fmt.Errorf("broker: %d devices registered; the probe limit is %d",
			len(opts.Devices), limits.MaxDevices)
	}

	devices := append([]config.Device(nil), opts.Devices...)
	seenIDs := map[string]bool{}
	seenClients := map[string]bool{}
	seenUsernames := map[string]bool{}
	seenSubscribe := map[string]bool{}
	for i, d := range devices {
		if d.DeviceID == "" || d.ClientID == "" || d.Username == "" || d.Password == "" {
			return opts, fmt.Errorf("broker: device %d requires device_id, client_id, username and password", i)
		}
		if seenIDs[d.DeviceID] {
			return opts, fmt.Errorf("broker: duplicate device_id %q", d.DeviceID)
		}
		if seenClients[d.ClientID] {
			return opts, fmt.Errorf("broker: duplicate client_id %q", d.ClientID)
		}
		if seenUsernames[d.Username] {
			return opts, errors.New("broker: duplicate username; credentials must be unique")
		}
		seenIDs[d.DeviceID] = true
		seenClients[d.ClientID] = true
		seenUsernames[d.Username] = true
		if !validExactTopic(d.PublishTopic) {
			return opts, fmt.Errorf("broker: device %q publish_topic must be an exact topic name without wildcards", d.DeviceID)
		}
		if !validExactTopic(d.SubscribeTopic) {
			return opts, fmt.Errorf("broker: device %q subscribe_topic must be an exact topic name without wildcards", d.DeviceID)
		}
		if d.PublishTopic == d.SubscribeTopic {
			return opts, fmt.Errorf("broker: device %q must not publish to its own downlink topic", d.DeviceID)
		}
		if seenSubscribe[d.SubscribeTopic] {
			return opts, fmt.Errorf("broker: duplicate subscribe_topic %q; downlink topics must be per-device", d.SubscribeTopic)
		}
		seenSubscribe[d.SubscribeTopic] = true
	}
	for _, d := range devices {
		for _, other := range devices {
			if d.DeviceID != other.DeviceID && d.SubscribeTopic == other.PublishTopic {
				return opts, fmt.Errorf("broker: device %q may not subscribe to another device's uplink topic", d.DeviceID)
			}
		}
	}
	opts.Devices = devices

	if opts.MaxPacketBytes == 0 {
		opts.MaxPacketBytes = limits.MaxMQTTPacketBytes
	}
	if opts.MaxPacketBytes < 64 || opts.MaxPacketBytes > limits.MaxMQTTPacketBytes {
		return opts, fmt.Errorf("broker: MaxPacketBytes must be 64..%d", limits.MaxMQTTPacketBytes)
	}

	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = limits.MQTTConnectTimeout
	}
	if opts.ConnectTimeout < time.Millisecond || opts.ConnectTimeout > limits.MQTTConnectTimeout {
		return opts, fmt.Errorf("broker: ConnectTimeout must be 1ms..%s (the M0 CONNECT bound)", limits.MQTTConnectTimeout)
	}

	switch opts.PublishQoS {
	case 0:
		if opts.PublishQoSZero {
			opts.PublishQoS = 0
		} else {
			// Default: QoS1 downlink for control/notify reliability.
			opts.PublishQoS = 1
		}
	case 1:
		if opts.PublishQoSZero {
			return opts, errors.New("broker: PublishQoSZero conflicts with PublishQoS=1")
		}
		opts.PublishQoS = 1
	default:
		return opts, fmt.Errorf("broker: PublishQoS %d unsupported; only QoS0 and QoS1 are implemented", opts.PublishQoS)
	}

	if opts.Logger == nil {
		opts.Logger = logging.Discard()
	}
	return opts, nil
}
