// Package config defines the isolated M0 probe configuration: run modes,
// pre-registered synthetic identities, bind-address policy, TLS settings and
// the explicit authorization files required by live/hardware modes.
//
// Nothing in this package is a production device registry. Credentials in the
// offline mode are synthetic random values generated at process start and are
// never persisted or printed. Live/hardware configs live in the ignored
// local/ directory and require an explicit authorization reference.
package config

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// Mode selects one of the three verification environments.
type Mode string

const (
	// Offline is the isolated simulation environment. It never contacts a
	// real device, cloud service, HA instance or public network endpoint.
	Offline Mode = "offline"

	// Live is the authorized cloud / remote-MCP verification environment.
	Live Mode = "live"

	// Hardware is the authorized real-device verification environment.
	Hardware Mode = "hardware"
)

// ErrBlocked marks a verification environment that cannot run because of a
// missing authorization or configuration. Callers must exit non-zero and
// report BLOCKED; they must never convert this into a pass or skip.
var ErrBlocked = errors.New("verification blocked")

// BlockedError describes why one environment is blocked.
type BlockedError struct {
	Mode   Mode
	Reason string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("%s verification blocked: %s", e.Mode, e.Reason)
}

// Unwrap allows errors.Is(err, ErrBlocked).
func (e *BlockedError) Unwrap() error { return ErrBlocked }

// AudioParams is the application-level audio format carried by HELLO.
type AudioParams struct {
	Format        string `json:"format"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	FrameDuration int    `json:"frame_duration"`
}

// Device is one pre-registered synthetic probe identity. DeviceID is an
// internal alias only; it is never accepted from MQTT payloads.
type Device struct {
	DeviceID       string `json:"device_id"`
	ClientID       string `json:"client_id"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	PublishTopic   string `json:"publish_topic"`
	SubscribeTopic string `json:"subscribe_topic"`
}

// Network holds isolated bind and TLS settings.
type Network struct {
	// MQTTBind and UDPBind are explicit host:port bind strings. Wildcard
	// binds are rejected. Port 0 requests an ephemeral test port.
	MQTTBind string `json:"mqtt_bind"`
	UDPBind  string `json:"udp_bind"`

	// AdvertiseHost is returned to the device in the HELLO response as
	// udp.server. It must be reachable by the device; it is never derived
	// from a container-internal address.
	AdvertiseHost string `json:"advertise_host"`

	// MQTTServerCertFile/MQTTServerKeyFile enable TLS on the broker
	// listener. MQTTClientCAFile optionally pins the client-side CA.
	MQTTServerCertFile string `json:"mqtt_server_cert_file"`
	MQTTServerKeyFile  string `json:"mqtt_server_key_file"`

	// MQTTClientCAFile is the CA bundle the probe's own MQTT client (used
	// for live/hardware verification) trusts. Empty means system roots.
	MQTTClientCAFile string `json:"mqtt_client_ca_file"`

	// AuthorizedPrivateCIDRs lists operator-authorized private CIDRs for
	// non-loopback binds. Loopback is always allowed. Nothing is accepted
	// by default beyond loopback.
	AuthorizedPrivateCIDRs []string `json:"authorized_private_cidrs"`

	// PlaintextIsolatedApproved records that the operator explicitly
	// approved plaintext MQTT for this isolated test only. It can never
	// enable a public bind and never starts a fallback after TLS failure.
	PlaintextIsolatedApproved bool `json:"plaintext_isolated_approved"`
}

// Audio holds the negotiated uplink/downlink formats. Uplink and downlink are
// stored separately: they are not one global configuration.
type Audio struct {
	Uplink   AudioParams `json:"uplink"`
	Downlink AudioParams `json:"downlink"`
}

// CloudConfig is the single authorized model/ASR/TTS configuration.
type CloudConfig struct {
	APIBase  string `json:"api_base"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	ASRModel string `json:"asr_model"`
	TTSModel string `json:"tts_model"`
}

// RemoteMCPConfig is the authorized read-only remote MCP endpoint.
type RemoteMCPConfig struct {
	URL          string `json:"url"`
	BearerToken  string `json:"bearer_token"`
	Transport    string `json:"transport"` // "streamable-http" (M0 choice)
	TimeoutMilli int    `json:"timeout_ms"`
}

// LiveConfig is loaded from an ignored local/live.json file. All fields are
// required to be present and explicitly authorized by the operator.
type LiveConfig struct {
	Enabled       bool            `json:"enabled"`
	Authorization string          `json:"authorization"`
	Cloud         CloudConfig     `json:"cloud"`
	RemoteMCP     RemoteMCPConfig `json:"remote_mcp"`
	MaxRounds     int             `json:"max_rounds"`
}

// HardwareMQTT is the device-side MQTT configuration used only for authorized
// hardware verification. These values are written to a real device and are
// never committed.
type HardwareMQTT struct {
	BrokerURL      string `json:"broker_url"`
	ClientID       string `json:"client_id"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	PublishTopic   string `json:"publish_topic"`
	SubscribeTopic string `json:"subscribe_topic"`
	CAFile         string `json:"ca_file"`
}

// HardwareDevice describes one authorized real device by alias.
type HardwareDevice struct {
	Alias          string       `json:"alias"`
	Board          string       `json:"board"`
	Firmware       string       `json:"firmware"`
	MQTT           HardwareMQTT `json:"mqtt"`
	NotifyAudioURL string       `json:"notify_audio_url"`
	Observer       string       `json:"observer"` // how playback/notify is externally observed
}

// HardwareConfig is loaded from an ignored local/hardware.json file.
type HardwareConfig struct {
	Enabled       bool             `json:"enabled"`
	Authorization string           `json:"authorization"`
	NetworkID     string           `json:"network_id"`
	RestorePlan   string           `json:"restore_plan"`
	Acknowledged  []string         `json:"acknowledged"` // e.g. "udp", "notify", "mcp-discovery"
	Devices       []HardwareDevice `json:"devices"`
}

// Paths control where sanitized evidence and sensitive raw output are written.
type Paths struct {
	EvidenceDir string `json:"evidence_dir"`
	LocalDir    string `json:"local_dir"`
}

// Config is the complete probe configuration.
type Config struct {
	Mode     Mode           `json:"mode"`
	Network  Network        `json:"network"`
	Devices  []Device       `json:"devices"`
	Audio    Audio          `json:"audio"`
	Live     LiveConfig     `json:"live"`
	Hardware HardwareConfig `json:"hardware"`
	Paths    Paths          `json:"paths"`
	Source   string         `json:"-"` // config file path, not persisted in reports
}

// DefaultOffline returns the fully synthetic two-device isolated config.
// Passwords are random per process so no credential-like literal is stored in
// the repository; they are only used by in-process simulations.
func DefaultOffline() *Config {
	devA, err := newSyntheticDevice(
		"dev-a",
		"GID_homevoice@@@02_00_00_00_00_01@@@02_00_00_00_00_01",
		"hv-synth-a",
		"device-server",
		"devices/p2p/02:00:00:00:00:01",
	)
	if err != nil {
		panic(err) // crypto/rand failure is not recoverable for the probe
	}
	devB, err := newSyntheticDevice(
		"dev-b",
		"GID_homevoice@@@02_00_00_00_00_02@@@02_00_00_00_00_02",
		"hv-synth-b",
		"device-server",
		"devices/p2p/02:00:00:00:00:02",
	)
	if err != nil {
		panic(err)
	}
	return &Config{
		Mode: Offline,
		Network: Network{
			MQTTBind:      "127.0.0.1:0",
			UDPBind:       "127.0.0.1:0",
			AdvertiseHost: "127.0.0.1",
		},
		Devices: []Device{devA, devB},
		Audio: Audio{
			Uplink: AudioParams{
				Format:        "opus",
				SampleRate:    limits.UplinkSampleRate,
				Channels:      limits.UplinkChannels,
				FrameDuration: limits.UplinkFrameDurationMS,
			},
			Downlink: AudioParams{
				Format:        "opus",
				SampleRate:    limits.DownlinkSampleRate,
				Channels:      limits.DownlinkChannels,
				FrameDuration: limits.DownlinkFrameDurationMS,
			},
		},
		Paths: Paths{
			EvidenceDir: "docs/milestones/M0-transport-probe/evidence",
			LocalDir:    "local",
		},
	}
}

func newSyntheticDevice(id, clientID, username, publishTopic, subscribeTopic string) (Device, error) {
	secret, err := randomHex(16)
	if err != nil {
		return Device{}, fmt.Errorf("synthetic credential: %w", err)
	}
	return Device{
		DeviceID:       id,
		ClientID:       clientID,
		Username:       username,
		Password:       secret,
		PublishTopic:   publishTopic,
		SubscribeTopic: subscribeTopic,
	}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Load reads a configuration for the requested mode. Offline mode uses the
// synthetic defaults when path is empty. Live and hardware modes require an
// existing, enabled, explicitly authorized file; otherwise they return a
// BlockedError.
func Load(mode Mode, path string) (*Config, error) {
	switch mode {
	case Offline:
		if path == "" {
			cfg := DefaultOffline()
			if err := cfg.Validate(); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return loadFile(mode, path)
	case Live, Hardware:
		if path == "" {
			return nil, &BlockedError{Mode: mode, Reason: "no --config path supplied; authorization file is required"}
		}
		cfg, err := loadFile(mode, path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, &BlockedError{Mode: mode, Reason: fmt.Sprintf("authorization/config file %q not found", path)}
			}
			return nil, err
		}
		return cfg, nil
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
}

func loadFile(mode Mode, path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.Source = path
	if cfg.Mode == "" {
		cfg.Mode = mode
	}
	if cfg.Mode != mode {
		return nil, fmt.Errorf("config mode %q does not match requested mode %q", cfg.Mode, mode)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if mode == Live {
		if err := cfg.validateLive(); err != nil {
			return nil, err
		}
	}
	if mode == Hardware {
		if err := cfg.validateHardware(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// Validate checks structural invariants shared by all modes.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("nil config")
	}
	switch c.Mode {
	case Offline, Live, Hardware:
	default:
		return fmt.Errorf("invalid mode %q", c.Mode)
	}
	if err := ValidateBindAddress(c.Network.MQTTBind, c.Network.AuthorizedPrivateCIDRs); err != nil {
		return fmt.Errorf("mqtt bind: %w", err)
	}
	if err := ValidateBindAddress(c.Network.UDPBind, c.Network.AuthorizedPrivateCIDRs); err != nil {
		return fmt.Errorf("udp bind: %w", err)
	}
	if err := validateAdvertiseHost(c.Network.AdvertiseHost); err != nil {
		return err
	}
	if err := validateAudio(c.Audio); err != nil {
		return err
	}
	if len(c.Devices) == 0 || len(c.Devices) > limits.MaxDevices {
		return fmt.Errorf("registered devices must be 1..%d (got %d); additional identities are rejected", limits.MaxDevices, len(c.Devices))
	}
	seenID := map[string]bool{}
	seenClient := map[string]bool{}
	seenUser := map[string]bool{}
	seenSub := map[string]bool{}
	for i, d := range c.Devices {
		if d.DeviceID == "" || d.ClientID == "" || d.Username == "" || d.Password == "" {
			return fmt.Errorf("device %d: device_id, client_id, username and password are required", i)
		}
		if seenID[d.DeviceID] {
			return fmt.Errorf("duplicate device_id %q", d.DeviceID)
		}
		if seenClient[d.ClientID] {
			return fmt.Errorf("duplicate client_id %q", d.ClientID)
		}
		if seenUser[d.Username] {
			return fmt.Errorf("duplicate username %q", d.Username)
		}
		seenID[d.DeviceID], seenClient[d.ClientID], seenUser[d.Username] = true, true, true
		if err := validateTopicName(d.PublishTopic); err != nil {
			return fmt.Errorf("device %q publish_topic: %w", d.DeviceID, err)
		}
		if err := validateTopicName(d.SubscribeTopic); err != nil {
			return fmt.Errorf("device %q subscribe_topic: %w", d.DeviceID, err)
		}
		if d.PublishTopic == d.SubscribeTopic {
			return fmt.Errorf("device %q must not publish to its own downlink topic", d.DeviceID)
		}
		if seenSub[d.SubscribeTopic] {
			return fmt.Errorf("duplicate subscribe_topic %q: downlink topics must be per-device", d.SubscribeTopic)
		}
		seenSub[d.SubscribeTopic] = true
	}
	for _, d := range c.Devices {
		for _, other := range c.Devices {
			if d.DeviceID != other.DeviceID && d.SubscribeTopic == other.PublishTopic {
				return fmt.Errorf("device %q subscribes to another device's uplink topic %q", other.DeviceID, d.SubscribeTopic)
			}
		}
	}
	if c.Network.MQTTServerCertFile != "" || c.Network.MQTTServerKeyFile != "" {
		if c.Network.MQTTServerCertFile == "" || c.Network.MQTTServerKeyFile == "" {
			return errors.New("mqtt TLS requires both server cert and key files")
		}
		if _, err := tls.LoadX509KeyPair(c.Network.MQTTServerCertFile, c.Network.MQTTServerKeyFile); err != nil {
			return fmt.Errorf("load mqtt server TLS keypair: %w", err)
		}
	}
	if c.Network.MQTTClientCAFile != "" {
		if _, err := LoadCAPool(c.Network.MQTTClientCAFile); err != nil {
			return err
		}
	}
	if c.Paths.EvidenceDir == "" {
		c.Paths.EvidenceDir = "docs/milestones/M0-transport-probe/evidence"
	}
	if c.Paths.LocalDir == "" {
		c.Paths.LocalDir = "local"
	}
	return nil
}

func validateAdvertiseHost(host string) error {
	if host == "" {
		return errors.New("network.advertise_host is required (device-reachable UDP address)")
	}
	if strings.ContainsAny(host, "/ \t") {
		return fmt.Errorf("invalid advertise_host %q", host)
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsUnspecified() || ip.Equal(net.IPv4bcast)) {
		return fmt.Errorf("advertise_host %q is not reachable by a device", host)
	}
	return nil
}

func validateAudio(a Audio) error {
	if err := validateAudioParams("uplink", a.Uplink); err != nil {
		return err
	}
	return validateAudioParams("downlink", a.Downlink)
}

func validateAudioParams(direction string, p AudioParams) error {
	if p.Format != "opus" {
		return fmt.Errorf("%s audio format %q unsupported (only opus; no silent fallback)", direction, p.Format)
	}
	if p.Channels != 1 {
		return fmt.Errorf("%s audio channels %d unsupported (probe supports mono)", direction, p.Channels)
	}
	if p.SampleRate != 16000 && p.SampleRate != 24000 && p.SampleRate != 8000 {
		return fmt.Errorf("%s audio sample_rate %d unsupported", direction, p.SampleRate)
	}
	if p.FrameDuration != 60 && p.FrameDuration != 20 && p.FrameDuration != 10 {
		return fmt.Errorf("%s audio frame_duration %d unsupported", direction, p.FrameDuration)
	}
	return nil
}

func validateTopicName(topic string) error {
	if topic == "" {
		return errors.New("empty topic")
	}
	if len(topic) > 1024 {
		return errors.New("topic too long")
	}
	if strings.ContainsAny(topic, "+#") {
		return fmt.Errorf("topic %q must be an exact topic name, not a wildcard filter", topic)
	}
	if strings.ContainsRune(topic, 0) {
		return errors.New("topic contains NUL")
	}
	return nil
}

func (c *Config) validateLive() error {
	if !c.Live.Enabled {
		return &BlockedError{Mode: Live, Reason: "live.enabled is false; no explicit authorization"}
	}
	if strings.TrimSpace(c.Live.Authorization) == "" {
		return &BlockedError{Mode: Live, Reason: "live.authorization reference is empty; explicit user authorization is required"}
	}
	if c.Live.Cloud.APIBase == "" || c.Live.Cloud.APIKey == "" || c.Live.Cloud.Model == "" {
		return &BlockedError{Mode: Live, Reason: "cloud api_base/api_key/model are required"}
	}
	if err := requireHTTPSURL(c.Live.Cloud.APIBase); err != nil {
		return fmt.Errorf("live cloud api_base: %w", err)
	}
	if c.Live.RemoteMCP.URL != "" {
		if err := requireHTTPSURL(c.Live.RemoteMCP.URL); err != nil {
			return fmt.Errorf("live remote_mcp.url: %w", err)
		}
		if c.Live.RemoteMCP.Transport == "" {
			return errors.New("live remote_mcp.transport is required (M0 uses streamable-http)")
		}
	}
	if c.Live.MaxRounds < 1 || c.Live.MaxRounds > limits.LiveMaxRounds {
		return fmt.Errorf("live.max_rounds must be 1..%d", limits.LiveMaxRounds)
	}
	return c.requireSecureOrIsolatedTransport()
}

func (c *Config) validateHardware() error {
	if !c.Hardware.Enabled {
		return &BlockedError{Mode: Hardware, Reason: "hardware.enabled is false; no explicit authorization"}
	}
	if strings.TrimSpace(c.Hardware.Authorization) == "" {
		return &BlockedError{Mode: Hardware, Reason: "hardware.authorization reference is empty; explicit device authorization is required"}
	}
	if c.Hardware.RestorePlan == "" {
		return &BlockedError{Mode: Hardware, Reason: "hardware.restore_plan is required before changing device configuration"}
	}
	if len(c.Hardware.Devices) == 0 {
		return errors.New("hardware.devices is empty")
	}
	if len(c.Hardware.Devices) > limits.MaxDevices {
		return fmt.Errorf("hardware.devices exceeds probe limit %d", limits.MaxDevices)
	}
	for i, d := range c.Hardware.Devices {
		if d.Alias == "" || d.Board == "" || d.Firmware == "" {
			return fmt.Errorf("hardware device %d: alias, board and firmware are required", i)
		}
		if d.MQTT.BrokerURL == "" || d.MQTT.ClientID == "" || d.MQTT.PublishTopic == "" || d.MQTT.SubscribeTopic == "" {
			return fmt.Errorf("hardware device %q: mqtt broker_url/client_id/publish_topic/subscribe_topic are required", d.Alias)
		}
		if err := validateTopicName(d.MQTT.PublishTopic); err != nil {
			return fmt.Errorf("hardware device %q publish topic: %w", d.Alias, err)
		}
		if err := validateTopicName(d.MQTT.SubscribeTopic); err != nil {
			return fmt.Errorf("hardware device %q subscribe topic: %w", d.Alias, err)
		}
		if d.NotifyAudioURL != "" {
			if err := requireHTTPSURL(d.NotifyAudioURL); err != nil {
				return fmt.Errorf("hardware device %q notify_audio_url: %w", d.Alias, err)
			}
		}
	}
	return c.requireSecureOrIsolatedTransport()
}

// requireSecureOrIsolatedTransport enforces the M0 transport rule for
// authorized environments: MQTT must use TLS (never skipping verification), or
// the operator must explicitly approve plaintext for an already-restricted
// isolated bind. There is no automatic plaintext fallback after TLS failure.
func (c *Config) requireSecureOrIsolatedTransport() error {
	hasTLS := c.Network.MQTTServerCertFile != "" && c.Network.MQTTServerKeyFile != ""
	if hasTLS {
		return nil
	}
	if !c.Network.PlaintextIsolatedApproved {
		return errors.New("authorized mode requires MQTT TLS (server cert+key) or network.plaintext_isolated_approved=true; plaintext is never a default or a fallback")
	}
	if c.Network.MQTTBind == "" {
		return errors.New("plaintext isolated approval requires an explicit isolated bind address")
	}
	return nil
}

func requireHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("URL %q must use https (no plaintext fallback)", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("URL %q has no host", raw)
	}
	return nil
}

// ValidateBindAddress enforces the M0 bind policy:
//   - loopback is always allowed (offline isolated tests),
//   - non-loopback addresses must be explicitly listed private CIDRs,
//   - wildcard/public addresses are always rejected.
func ValidateBindAddress(addr string, authorizedPrivateCIDRs []string) error {
	if addr == "" {
		return errors.New("bind address is required")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid bind address %q: %w", addr, err)
	}
	if portNum, err := strconv.Atoi(port); err != nil || portNum < 0 || portNum > 65535 {
		return fmt.Errorf("invalid numeric port in bind address %q", addr)
	}
	if host == "" {
		return fmt.Errorf("wildcard bind %q is rejected (public/large-scope listen)", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// DNS names are not accepted for probe binds: an operator must
		// supply the literal isolated address.
		return fmt.Errorf("bind host %q must be an IP literal", host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("wildcard bind %q is rejected (0.0.0.0/:: not allowed)", addr)
	}
	if ip.IsLoopback() {
		return nil
	}
	if !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
		return fmt.Errorf("non-private bind %q is rejected; only loopback or authorized isolated private addresses are allowed", addr)
	}
	for _, cidr := range authorizedPrivateCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid authorized_private_cidrs entry %q: %w", cidr, err)
		}
		if n.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("private bind %q is not in network.authorized_private_cidrs; explicit authorization is required", addr)
}

// LoadCAPool loads a PEM CA bundle without weakening verification.
func LoadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA file %q contains no usable certificates", path)
	}
	return pool, nil
}

// ServerTLSConfig builds a broker TLS configuration. Certificate verification
// is never disabled for clients.
func ServerTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if clientCAFile != "" {
		pool, err := LoadCAPool(clientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ClientTLSConfig builds a client TLS configuration that always verifies the
// server certificate. InsecureSkipVerify is structurally absent.
func ClientTLSConfig(caFile, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		pool, err := LoadCAPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// DeviceByID returns the registered device alias.
func (c *Config) DeviceByID(id string) (Device, bool) {
	for _, d := range c.Devices {
		if d.DeviceID == id {
			return d, true
		}
	}
	return Device{}, false
}

// DeviceByClientID returns the device registered for an MQTT client id.
func (c *Config) DeviceByClientID(clientID string) (Device, bool) {
	for _, d := range c.Devices {
		if d.ClientID == clientID {
			return d, true
		}
	}
	return Device{}, false
}

// DeviceByUsername returns the device registered for an MQTT username.
func (c *Config) DeviceByUsername(username string) (Device, bool) {
	for _, d := range c.Devices {
		if d.Username == username {
			return d, true
		}
	}
	return Device{}, false
}

// Secrets returns every synthetic/authorized secret that must be redacted from
// logs and evidence. It never returns nil markers or short/empty strings.
func (c *Config) Secrets() []string {
	var out []string
	for _, d := range c.Devices {
		if d.Password != "" {
			out = append(out, d.Password)
		}
	}
	for _, d := range c.Hardware.Devices {
		if d.MQTT.Password != "" {
			out = append(out, d.MQTT.Password)
		}
	}
	if c.Live.Cloud.APIKey != "" {
		out = append(out, c.Live.Cloud.APIKey)
	}
	if c.Live.RemoteMCP.BearerToken != "" {
		out = append(out, c.Live.RemoteMCP.BearerToken)
	}
	return out
}
