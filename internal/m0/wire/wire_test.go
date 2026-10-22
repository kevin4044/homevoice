package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/fixtures"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

func testPolicy() HelloPolicy {
	return DefaultHelloPolicy(AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60})
}

func mustFixture(t *testing.T) *fixtures.ControlSuite {
	t.Helper()
	suite, err := fixtures.LoadControlMessages()
	if err != nil {
		t.Fatalf("load control fixtures: %v", err)
	}
	return suite
}

func fixtureBody(t *testing.T, id string) []byte {
	t.Helper()
	b, err := mustFixture(t).Body(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseHelloFixture(t *testing.T) {
	msg, err := ParseUplink(fixtureBody(t, "hello-a"), testPolicy())
	if err != nil {
		t.Fatalf("ParseUplink(hello-a) = %v", err)
	}
	hello, ok := msg.(Hello)
	if !ok {
		t.Fatalf("message type = %T, want Hello", msg)
	}
	if hello.Version != 3 || hello.Transport != "udp" || !hello.Features["mcp"] {
		t.Fatalf("hello = %+v", hello)
	}
	if hello.AudioParams != (AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60}) {
		t.Fatalf("audio params = %+v", hello.AudioParams)
	}
}

func TestParseHelloRejectsUnsupportedVariants(t *testing.T) {
	base := fixtureBody(t, "hello-a")
	mutate := func(fn func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		fn(m)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"websocket transport", mutate(func(m map[string]any) { m["transport"] = "websocket" }), ErrUnsupported},
		{"old version", mutate(func(m map[string]any) { m["version"] = 2 }), ErrUnsupported},
		{"pcm format", mutate(func(m map[string]any) {
			m["audio_params"].(map[string]any)["format"] = "pcm"
		}), ErrUnsupported},
		{"stereo", mutate(func(m map[string]any) {
			m["audio_params"].(map[string]any)["channels"] = 2
		}), ErrUnsupported},
		{"wrong uplink rate", mutate(func(m map[string]any) {
			m["audio_params"].(map[string]any)["sample_rate"] = 24000
		}), ErrUnsupported},
		{"incomplete audio params", mutate(func(m map[string]any) { delete(m, "audio_params") }), ErrInvalid},
		{"unknown field", mutate(func(m map[string]any) { m["future_field"] = true }), ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUplink(tc.body, testPolicy())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseUplinkRejectsIdentityClaims(t *testing.T) {
	body := fixtureBody(t, "spoofed-device-id")
	_, err := ParseUplink(body, testPolicy())
	if !errors.Is(err, ErrIdentityClaim) {
		t.Fatalf("error = %v, want ErrIdentityClaim", err)
	}
}

func TestParseListenAbortGoodbyeMCP(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"manual-start", TypeListen},
		{"listen-stop", TypeListen},
		{"abort-playback", TypeAbort},
		{"audio-goodbye", TypeGoodbye},
		{"mcp-initialize", TypeMCP},
		{"mcp-list-request", TypeMCP},
		{"mcp-list-response", TypeMCP},
		{"mcp-volume-call", TypeMCP},
		{"mcp-tool-error", TypeMCP},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			msg, err := ParseUplink(fixtureBody(t, tc.id), testPolicy())
			if err != nil {
				t.Fatalf("ParseUplink = %v", err)
			}
			var got string
			switch m := msg.(type) {
			case Listen:
				got = m.Type
				if m.State == ListenStart && m.Mode != ModeManual {
					t.Fatalf("listen mode = %q", m.Mode)
				}
			case Abort:
				got = m.Type
			case Goodbye:
				got = m.Type
			case MCP:
				got = m.Type
			default:
				t.Fatalf("unexpected type %T", msg)
			}
			if got != tc.want {
				t.Fatalf("type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseUplinkRejectsWrongDirectionAndBadSignals(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"tts from device", fixtureBody(t, "tts-start"), ErrInvalid},
		{"notify from device", fixtureBody(t, "idle-notify"), ErrInvalid},
		{"empty", nil, ErrInvalid},
		{"not json", []byte("{not json"), ErrInvalid},
		{"unknown type", []byte(`{"type":"play","audio":"x"}`), ErrUnknownType},
		{"missing type", []byte(`{"state":"stop"}`), ErrInvalid},
		{"listen bad state", []byte(`{"type":"listen","state":"pause"}`), ErrUnsupported},
		{"listen bad mode", []byte(`{"type":"listen","state":"start","mode":"turbo"}`), ErrUnsupported},
		{"mcp empty payload", []byte(`{"type":"mcp"}`), ErrInvalid},
		{"mcp array payload", []byte(`{"type":"mcp","payload":[1,2]}`), ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUplink(tc.body, testPolicy())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseUplinkOversize(t *testing.T) {
	big := make([]byte, limits.MaxJSONBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	_, err := ParseUplink(big, testPolicy())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func TestEncodeHelloResponse(t *testing.T) {
	key := strings.Repeat("ab", 16)
	nonce := strings.Repeat("01", 16)
	resp := HelloResponse{
		Type:        TypeHello,
		Transport:   "udp",
		SessionID:   "synthetic-session-a",
		AudioParams: AudioParams{Format: "opus", SampleRate: 24000, Channels: 1, FrameDuration: 60},
		UDP:         UDPServer{Server: "127.0.0.1", Port: 12345, Key: key, Nonce: nonce},
	}
	b, err := EncodeHelloResponse(resp)
	if err != nil {
		t.Fatalf("EncodeHelloResponse = %v", err)
	}
	parsed, err := ParseHelloResponse(b)
	if err != nil {
		t.Fatalf("ParseHelloResponse = %v", err)
	}
	if parsed.SessionID != resp.SessionID || parsed.UDP.Key != key || parsed.UDP.Nonce != nonce {
		t.Fatalf("round trip = %+v", parsed)
	}

	bad := []HelloResponse{
		{Transport: "udp", SessionID: "s", AudioParams: resp.AudioParams, UDP: UDPServer{Server: "127.0.0.1", Port: 1, Key: "zz", Nonce: nonce}},
		{Transport: "websocket", SessionID: "s", AudioParams: resp.AudioParams, UDP: resp.UDP},
		{Transport: "udp", SessionID: "", AudioParams: resp.AudioParams, UDP: resp.UDP},
		{Transport: "udp", SessionID: "s", AudioParams: resp.AudioParams, UDP: UDPServer{Server: "127.0.0.1", Port: 0, Key: key, Nonce: nonce}},
	}
	for i, r := range bad {
		if _, err := EncodeHelloResponse(r); err == nil {
			t.Fatalf("case %d: EncodeHelloResponse accepted invalid response", i)
		}
	}
}

func TestEncodeNotifyAndTTS(t *testing.T) {
	n := Notify{Type: TypeNotify, Text: "Homevoice 测试通知", AudioURL: "https://test-audio.invalid/notice.wav"}
	b, err := EncodeNotify(n.Text, n.AudioURL)
	if err != nil {
		t.Fatalf("EncodeNotify = %v", err)
	}
	parsed, err := ParseNotify(b)
	if err != nil || parsed.Text != n.Text || parsed.AudioURL != n.AudioURL {
		t.Fatalf("ParseNotify = %+v, %v", parsed, err)
	}
	for _, bad := range []struct{ text, url string }{
		{"", "https://test-audio.invalid/x.wav"},
		{"hi", "http://test-audio.invalid/x.wav"},
		{"hi", "file:///etc/passwd"},
		{"hi", "not a url"},
	} {
		if _, err := EncodeNotify(bad.text, bad.url); err == nil {
			t.Fatalf("EncodeNotify(%q,%q) accepted invalid input", bad.text, bad.url)
		}
	}

	for _, state := range []string{TTSStart, TTSStop} {
		b, err := EncodeTTS(state, "synthetic-session-a")
		if err != nil {
			t.Fatalf("EncodeTTS(%q) = %v", state, err)
		}
		parsed, err := ParseTTS(b)
		if err != nil || parsed.State != state {
			t.Fatalf("ParseTTS(%q) = %+v, %v", state, parsed, err)
		}
	}
	if _, err := EncodeTTS("pause", "s"); err == nil {
		t.Fatal("EncodeTTS accepted unknown state")
	}
}

func TestEncodeMCP(t *testing.T) {
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	b, err := EncodeMCP(raw)
	if err != nil {
		t.Fatalf("EncodeMCP = %v", err)
	}
	if !bytes.Contains(b, []byte(`"payload"`)) {
		t.Fatalf("encoded MCP = %s", b)
	}
	if _, err := EncodeMCP(json.RawMessage(`{bad`)); err == nil {
		t.Fatal("EncodeMCP accepted invalid JSON")
	}
}

func TestUDPVectorHexHelpers(t *testing.T) {
	// The wire package itself does not implement crypto; this guards that
	// the fixture hex fields are well formed for the udp package.
	suite, err := fixtures.LoadUDPVectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range suite.Vectors {
		if v.ID == "truncated-payload" {
			// Intentionally malformed input used by the udp package to prove
			// length rejection; it is not expected to decode.
			continue
		}
		if v.PacketHex != "" {
			if _, err := hex.DecodeString(v.PacketHex); err != nil {
				t.Fatalf("vector %q packet_hex invalid: %v", v.ID, err)
			}
		}
	}
}
