package acceptance_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/probe"
	"github.com/homevoice/homevoice/internal/m0/remotemcp"
	"github.com/homevoice/homevoice/internal/m0/wire"
)

// TestA27BindPolicyTLSAndNoDowngrade verifies that startup refuses wildcard or
// unauthorized public binds, TLS verification is never disabled, and a device
// WebSocket HELLO is rejected instead of silently converted to another route.
func TestA27BindPolicyTLSAndNoDowngrade(t *testing.T) {
	badBinds := []string{"0.0.0.0:0", "[::]:0", ":0", "8.8.8.8:0", "10.1.2.3:0"}
	for _, bind := range badBinds {
		cfg := config.DefaultOffline()
		cfg.Network.MQTTBind = bind
		if err := cfg.Validate(); err == nil {
			t.Fatalf("config accepted bind %q", bind)
		}
		if _, err := probe.New(probe.Options{Config: cfg}); err == nil {
			t.Fatalf("probe.New started with bind %q", bind)
		}
	}
	cfg := config.DefaultOffline()
	cfg.Network.UDPBind = "0.0.0.0:0"
	if err := cfg.Validate(); err == nil {
		t.Fatal("config accepted wildcard UDP bind")
	}
	// An explicitly authorized private bind is accepted by validation (startup
	// on a non-local address would fail at the OS layer).
	authorized := config.DefaultOffline()
	authorized.Network.MQTTBind = "10.20.0.5:0"
	authorized.Network.UDPBind = "10.20.0.5:0"
	authorized.Network.AuthorizedPrivateCIDRs = []string{"10.20.0.0/16"}
	if err := authorized.Validate(); err != nil {
		t.Fatalf("authorized private bind rejected: %v", err)
	}

	// TLS: a malformed keypair is a startup error, and client verification is
	// never disabled.
	bad := config.DefaultOffline()
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad.Network.MQTTServerCertFile = badFile
	bad.Network.MQTTServerKeyFile = badFile
	if err := bad.Validate(); err == nil {
		t.Fatal("config accepted an invalid TLS keypair")
	}
	tlsCfg, err := config.ClientTLSConfig("", "broker.test.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.InsecureSkipVerify {
		t.Fatal("client TLS verification is disabled")
	}
	if _, err := remotemcp.New(remotemcp.Config{
		URL:        "https://mcp.invalid/mcp",
		Timeout:    time.Second,
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}); err == nil {
		t.Fatal("remote MCP accepted an HTTP client that disables TLS verification")
	}

	// No WebSocket fallback: the same authenticated MQTT connection publishing a
	// transport=websocket HELLO must not create an audio session.
	h := newHarness(t, nil, nil)
	dev := h.connect("dev-a")
	raw := []byte(`{"type":"hello","version":3,"transport":"websocket","features":{"mcp":true},"audio_params":{"format":"opus","sample_rate":16000,"channels":1,"frame_duration":60}}`)
	if err := dev.PublishRaw(raw); err != nil {
		t.Fatalf("publish websocket hello: %v", err)
	}
	waitFor(t, "websocket hello rejected", 3*time.Second, func() bool {
		return h.probe.Stats().Malformed >= 1
	})
	if _, ok := h.probe.SessionSnapshot("dev-a"); ok {
		t.Fatal("websocket HELLO created a UDP session (silent fallback)")
	}
	realHello := false
	for _, m := range dev.Messages() {
		if m.Type == wire.TypeHello {
			realHello = true
		}
	}
	if realHello {
		t.Fatal("probe replied to a websocket HELLO")
	}
	if _, online := h.probe.Online("dev-a"); !online {
		t.Fatal("valid MQTT connection was lost when rejecting the websocket HELLO")
	}
}

// TestA28LogsRedactSecretsAndAudio covers the log-capture requirement: synthetic
// credentials, HELLO key material and audio markers never reach the captured
// logs. The non-zero BLOCKED exit codes for missing live/hardware config are
// verified in the command package test.
func TestA28LogsRedactSecretsAndAudio(t *testing.T) {
	h := newHarness(t, nil, nil)
	dev := h.connect("dev-a")
	h.hello("dev-a")
	hello := dev.Hello()
	if hello == nil {
		t.Fatal("no HELLO reply captured")
	}
	key := hello.UDP.Key
	if len(key) != 32 {
		t.Fatalf("HELLO key hex length = %d", len(key))
	}
	h.hello("dev-b")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, _, err := h.probe.DiscoverDeviceTools(ctx, "dev-a")
	if err != nil {
		t.Fatalf("device MCP discovery: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("no device tools discovered")
	}
	if _, err := h.probe.Notify("dev-a"); err != nil {
		t.Fatalf("notify: %v", err)
	}

	dev.SendListen("start", "manual")
	waitFor(t, "listening", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return s.Listening
	})
	const audioMarker = "AUDIO_MARKER_7f3a9c_private_bytes"
	if err := dev.SendOpusFrame([]byte(audioMarker)); err != nil {
		t.Fatalf("send audio marker: %v", err)
	}
	dev.SendListen("stop", "")
	waitFor(t, "listen stop", 2*time.Second, func() bool {
		s, _ := h.probe.Snapshot("dev-a")
		return !s.Listening
	})

	logs := h.logs.String()
	if len(logs) == 0 {
		t.Fatal("log capture is empty; redaction evidence would be meaningless")
	}
	for _, secret := range h.cfg.Secrets() {
		if strings.Contains(logs, secret) {
			t.Fatalf("captured logs leaked a synthetic credential")
		}
	}
	if strings.Contains(logs, key) {
		t.Fatal("captured logs leaked the HELLO AES key")
	}
	if strings.Contains(logs, audioMarker) {
		t.Fatal("captured logs leaked audio payload content")
	}
	if !strings.Contains(logs, "[REDACTED]") && !strings.Contains(logs, "device") {
		t.Fatalf("log capture does not look like probe logging: %.200q", logs)
	}
}
