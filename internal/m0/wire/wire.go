// Package wire defines the MQTT application-message JSON contract from the
// approved M0-v0.1 protocol document. It is a strict, bounded parser for
// synthetic probe traffic; it is not a firmware SDK.
//
// Identity is never read from payloads. Messages carrying identity-looking
// fields (device_id, client_id, mac, username, ...) are rejected, and the
// authenticated MQTT connection remains the only identity source.
package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// Application message types.
const (
	TypeHello   = "hello"
	TypeListen  = "listen"
	TypeAbort   = "abort"
	TypeGoodbye = "goodbye"
	TypeTTS     = "tts"
	TypeNotify  = "notify"
	TypeMCP     = "mcp"
)

// Listen states and modes accepted by the probe.
const (
	ListenStart = "start"
	ListenStop  = "stop"

	ModeManual   = "manual"
	ModeAuto     = "auto"
	ModeRealtime = "realtime"
)

// TTS states.
const (
	TTSStart = "start"
	TTSStop  = "stop"
)

// Bounds for outbound application messages.
const (
	MaxNotifyTextBytes = 2048
	MaxSessionIDBytes  = 128
	MaxReasonBytes     = 256
)

// ErrTooLarge reports an application message over the configured limit.
var ErrTooLarge = errors.New("application message too large")

// ErrUnknownType reports an unhandled MQTT application message type.
var ErrUnknownType = errors.New("unknown application message type")

// ErrInvalid reports a malformed message.
var ErrInvalid = errors.New("invalid application message")

// ErrIdentityClaim reports a payload attempting to assert identity.
var ErrIdentityClaim = errors.New("payload asserts device identity; identity comes only from the authenticated MQTT connection")

// ErrUnsupported reports a syntactically valid but unsupported direction,
// transport, version or audio format.
var ErrUnsupported = errors.New("unsupported message variant")

// AudioParams mirrors the protocol audio_params object.
type AudioParams struct {
	Format        string `json:"format"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	FrameDuration int    `json:"frame_duration"`
}

// Equal compares all audio parameters exactly.
func (a AudioParams) Equal(b AudioParams) bool {
	return a.Format == b.Format && a.SampleRate == b.SampleRate &&
		a.Channels == b.Channels && a.FrameDuration == b.FrameDuration
}

// Hello is a device-to-server HELLO.
type Hello struct {
	Type        string          `json:"type"`
	Version     int             `json:"version"`
	Transport   string          `json:"transport"`
	Features    map[string]bool `json:"features"`
	AudioParams AudioParams     `json:"audio_params"`
}

// Listen is a device-to-server listen command.
type Listen struct {
	Type  string `json:"type"`
	State string `json:"state"`
	Mode  string `json:"mode"`
}

// Abort is a device-to-server interruption request.
type Abort struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// Goodbye releases the UDP audio session while keeping MQTT connected.
type Goodbye struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

// TTS is the server-to-device playback marker.
type TTS struct {
	Type      string `json:"type"`
	State     string `json:"state"`
	SessionID string `json:"session_id"`
}

// Notify is the server-to-device idle notification.
type Notify struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	AudioURL string `json:"audio_url"`
}

// MCP carries an opaque JSON-RPC payload as an MQTT application message.
type MCP struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// UDPServer describes the UDP endpoint and key material in the HELLO reply.
type UDPServer struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	Key    string `json:"key"`   // 32 hex chars = 16-byte AES-128 key
	Nonce  string `json:"nonce"` // 32 hex chars = 16-byte header template
}

// HelloResponse is the server-to-device HELLO reply.
type HelloResponse struct {
	Type        string      `json:"type"`
	Transport   string      `json:"transport"`
	SessionID   string      `json:"session_id"`
	AudioParams AudioParams `json:"audio_params"`
	UDP         UDPServer   `json:"udp"`
}

// HelloPolicy states which synthetic HELLO variants the probe accepts.
// Unsupported variants are rejected; they are never downgraded or routed to
// another transport.
type HelloPolicy struct {
	AllowedVersions   []int
	AllowedTransports []string
	SupportedAudio    []AudioParams
}

// DefaultHelloPolicy returns the M0 probe policy: version 3, UDP, opus mono
// at the configured uplink format.
func DefaultHelloPolicy(uplink AudioParams) HelloPolicy {
	return HelloPolicy{
		AllowedVersions:   []int{3},
		AllowedTransports: []string{"udp"},
		SupportedAudio:    []AudioParams{uplink},
	}
}

// Message is any parsed device-to-server application message.
type Message interface {
	messageType() string
}

func (Hello) messageType() string   { return TypeHello }
func (Listen) messageType() string  { return TypeListen }
func (Abort) messageType() string   { return TypeAbort }
func (Goodbye) messageType() string { return TypeGoodbye }
func (MCP) messageType() string     { return TypeMCP }

var identityFields = map[string]bool{
	"device_id":   true,
	"deviceid":    true,
	"client_id":   true,
	"clientid":    true,
	"mac":         true,
	"mac_address": true,
	"username":    true,
	"user":        true,
	"peer":        true,
}

// ParseUplink parses and bounds one device-to-server application message.
func ParseUplink(payload []byte, policy HelloPolicy) (Message, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrInvalid)
	}
	if len(payload) > limits.MaxJSONBytes {
		return nil, fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(payload), limits.MaxJSONBytes)
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: malformed JSON", ErrInvalid)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for name := range fields {
		if identityFields[strings.ToLower(name)] {
			return nil, fmt.Errorf("%w: field %q", ErrIdentityClaim, name)
		}
	}

	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if env.Type == "" {
		return nil, fmt.Errorf("%w: missing type", ErrInvalid)
	}

	switch env.Type {
	case TypeHello:
		var h Hello
		if err := decodeStrict(payload, &h); err != nil {
			return nil, err
		}
		if err := h.Validate(policy); err != nil {
			return nil, err
		}
		return h, nil
	case TypeListen:
		var l Listen
		if err := decodeStrict(payload, &l); err != nil {
			return nil, err
		}
		if err := l.Validate(); err != nil {
			return nil, err
		}
		return l, nil
	case TypeAbort:
		var a Abort
		if err := decodeStrict(payload, &a); err != nil {
			return nil, err
		}
		if len(a.Reason) > MaxReasonBytes {
			return nil, fmt.Errorf("%w: abort reason too long", ErrInvalid)
		}
		return a, nil
	case TypeGoodbye:
		var g Goodbye
		if err := decodeStrict(payload, &g); err != nil {
			return nil, err
		}
		if len(g.SessionID) > MaxSessionIDBytes {
			return nil, fmt.Errorf("%w: session_id too long", ErrInvalid)
		}
		return g, nil
	case TypeMCP:
		var m MCP
		if err := decodeStrict(payload, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return m, nil
	case TypeTTS, TypeNotify:
		return nil, fmt.Errorf("%w: %q is server-to-device only", ErrInvalid, env.Type)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, env.Type)
	}
}

func decodeStrict(payload []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// Validate checks a HELLO against the accepted policy.
func (h Hello) Validate(policy HelloPolicy) error {
	if h.Type != TypeHello {
		return fmt.Errorf("%w: hello type %q", ErrInvalid, h.Type)
	}
	if !containsInt(policy.AllowedVersions, h.Version) {
		return fmt.Errorf("%w: hello version %d (supported: %v)", ErrUnsupported, h.Version, policy.AllowedVersions)
	}
	if !containsString(policy.AllowedTransports, h.Transport) {
		return fmt.Errorf("%w: transport %q; device WebSocket is out of scope and there is no fallback", ErrUnsupported, h.Transport)
	}
	if h.AudioParams.Format == "" || h.AudioParams.SampleRate <= 0 ||
		h.AudioParams.Channels <= 0 || h.AudioParams.FrameDuration <= 0 {
		return fmt.Errorf("%w: incomplete audio_params", ErrInvalid)
	}
	for _, supported := range policy.SupportedAudio {
		if h.AudioParams.Equal(supported) {
			return nil
		}
	}
	return fmt.Errorf("%w: audio_params %+v not in supported set %+v", ErrUnsupported, h.AudioParams, policy.SupportedAudio)
}

// Validate checks a listen command.
func (l Listen) Validate() error {
	switch l.State {
	case ListenStart:
		switch l.Mode {
		case ModeManual, ModeAuto, ModeRealtime:
			return nil
		case "":
			return fmt.Errorf("%w: listen start requires mode", ErrInvalid)
		default:
			return fmt.Errorf("%w: listen mode %q", ErrUnsupported, l.Mode)
		}
	case ListenStop:
		return nil
	default:
		return fmt.Errorf("%w: listen state %q", ErrUnsupported, l.State)
	}
}

// Validate bounds an MCP application wrapper.
func (m MCP) Validate() error {
	if len(m.Payload) == 0 {
		return fmt.Errorf("%w: mcp payload is empty", ErrInvalid)
	}
	if len(m.Payload) > limits.MaxJSONBytes {
		return fmt.Errorf("%w: mcp payload too large", ErrTooLarge)
	}
	if !json.Valid(m.Payload) || !bytes.HasPrefix(bytes.TrimSpace(m.Payload), []byte("{")) {
		return fmt.Errorf("%w: mcp payload must be a JSON object", ErrInvalid)
	}
	return nil
}

// EncodeHelloResponse builds and bounds the server HELLO reply.
func EncodeHelloResponse(h HelloResponse) ([]byte, error) {
	if h.Type == "" {
		h.Type = TypeHello
	}
	if h.Type != TypeHello {
		return nil, fmt.Errorf("%w: response type", ErrInvalid)
	}
	if h.Transport != "udp" {
		return nil, fmt.Errorf("%w: response transport %q", ErrUnsupported, h.Transport)
	}
	if h.SessionID == "" || len(h.SessionID) > MaxSessionIDBytes {
		return nil, fmt.Errorf("%w: invalid session_id", ErrInvalid)
	}
	if err := validateAudioParams(h.AudioParams); err != nil {
		return nil, err
	}
	if h.UDP.Server == "" {
		return nil, fmt.Errorf("%w: udp.server missing", ErrInvalid)
	}
	if h.UDP.Port <= 0 || h.UDP.Port > 65535 {
		return nil, fmt.Errorf("%w: udp.port %d", ErrInvalid, h.UDP.Port)
	}
	if err := validateHex16(h.UDP.Key); err != nil {
		return nil, fmt.Errorf("udp.key: %w", err)
	}
	if err := validateHex16(h.UDP.Nonce); err != nil {
		return nil, fmt.Errorf("udp.nonce: %w", err)
	}
	return marshalBounded(h)
}

func validateAudioParams(p AudioParams) error {
	if p.Format != "opus" {
		return fmt.Errorf("%w: audio format %q", ErrUnsupported, p.Format)
	}
	if p.Channels != 1 {
		return fmt.Errorf("%w: mono only, got %d channels", ErrUnsupported, p.Channels)
	}
	if p.SampleRate <= 0 || p.FrameDuration <= 0 {
		return fmt.Errorf("%w: incomplete audio params", ErrInvalid)
	}
	return nil
}

func validateHex16(s string) error {
	if len(s) != 32 {
		return fmt.Errorf("%w: want 32 hex chars, got %d", ErrInvalid, len(s))
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(raw) != 16 {
		return fmt.Errorf("%w: want 16 bytes", ErrInvalid)
	}
	return nil
}

// EncodeTTS builds a bounded server-to-device TTS marker.
func EncodeTTS(state, sessionID string) ([]byte, error) {
	if state != TTSStart && state != TTSStop {
		return nil, fmt.Errorf("%w: tts state %q", ErrUnsupported, state)
	}
	if sessionID == "" || len(sessionID) > MaxSessionIDBytes {
		return nil, fmt.Errorf("%w: invalid session_id", ErrInvalid)
	}
	return marshalBounded(TTS{Type: TypeTTS, State: state, SessionID: sessionID})
}

// EncodeNotify builds a bounded idle notification. The audio URL must be
// HTTPS; the probe does not implement plaintext media delivery.
func EncodeNotify(text, audioURL string) ([]byte, error) {
	if strings.TrimSpace(text) == "" || len(text) > MaxNotifyTextBytes {
		return nil, fmt.Errorf("%w: notify text empty or too long", ErrInvalid)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("%w: notify text is not UTF-8", ErrInvalid)
	}
	u, err := url.Parse(audioURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w: audio_url must be an absolute https URL", ErrInvalid)
	}
	return marshalBounded(Notify{Type: TypeNotify, Text: text, AudioURL: audioURL})
}

// EncodeMCP wraps a JSON-RPC payload.
func EncodeMCP(payload json.RawMessage) ([]byte, error) {
	m := MCP{Type: TypeMCP, Payload: payload}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return marshalBounded(m)
}

// ParseTTS parses a server-to-device TTS marker (used by the simulator).
func ParseTTS(payload []byte) (TTS, error) {
	var t TTS
	if len(payload) > limits.MaxJSONBytes {
		return t, fmt.Errorf("%w: too large", ErrTooLarge)
	}
	if err := decodeStrict(payload, &t); err != nil {
		return t, err
	}
	if t.State != TTSStart && t.State != TTSStop {
		return t, fmt.Errorf("%w: tts state %q", ErrUnsupported, t.State)
	}
	return t, nil
}

// ParseNotify parses a server-to-device notify (used by the simulator).
func ParseNotify(payload []byte) (Notify, error) {
	var n Notify
	if len(payload) > limits.MaxJSONBytes {
		return n, fmt.Errorf("%w: too large", ErrTooLarge)
	}
	if err := decodeStrict(payload, &n); err != nil {
		return n, err
	}
	if n.Type != TypeNotify {
		return n, fmt.Errorf("%w: notify type", ErrInvalid)
	}
	return n, nil
}

// ParseHelloResponse parses a server-to-device HELLO reply (used by the
// simulated device).
func ParseHelloResponse(payload []byte) (HelloResponse, error) {
	var h HelloResponse
	if len(payload) > limits.MaxJSONBytes {
		return h, fmt.Errorf("%w: too large", ErrTooLarge)
	}
	if err := decodeStrict(payload, &h); err != nil {
		return h, err
	}
	if h.Type != TypeHello {
		return h, fmt.Errorf("%w: hello response type", ErrInvalid)
	}
	if err := validateHex16(h.UDP.Key); err != nil {
		return h, fmt.Errorf("udp.key: %w", err)
	}
	if err := validateHex16(h.UDP.Nonce); err != nil {
		return h, fmt.Errorf("udp.nonce: %w", err)
	}
	return h, nil
}

// ParseMCPDownlink parses an MCP wrapper received by a device/simulator.
func ParseMCPDownlink(payload []byte) (MCP, error) {
	var m MCP
	if len(payload) > limits.MaxJSONBytes {
		return m, fmt.Errorf("%w: too large", ErrTooLarge)
	}
	if err := decodeStrict(payload, &m); err != nil {
		return m, err
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	return m, nil
}

func marshalBounded(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > limits.MaxJSONBytes {
		return nil, fmt.Errorf("%w: encoded %d > %d bytes", ErrTooLarge, len(b), limits.MaxJSONBytes)
	}
	return b, nil
}

func containsInt(values []int, v int) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func containsString(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
