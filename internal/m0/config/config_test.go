package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

func TestDefaultOfflineIsValidAndSynthetic(t *testing.T) {
	cfg := DefaultOffline()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultOffline().Validate() = %v", err)
	}
	if cfg.Mode != Offline {
		t.Fatalf("mode = %q, want %q", cfg.Mode, Offline)
	}
	if len(cfg.Devices) != limits.MaxDevices {
		t.Fatalf("devices = %d, want %d", len(cfg.Devices), limits.MaxDevices)
	}
	seen := map[string]bool{}
	for _, d := range cfg.Devices {
		if len(d.Password) != 32 {
			t.Fatalf("synthetic password for %q is not 32 hex chars", d.DeviceID)
		}
		if seen[d.Password] {
			t.Fatalf("duplicate synthetic password for %q", d.DeviceID)
		}
		seen[d.Password] = true
		if d.DeviceID == d.ClientID || d.DeviceID == d.Username {
			t.Fatalf("device alias %q must not double as a credential", d.DeviceID)
		}
	}
	// Different processes must not reuse a synthetic secret.
	other := DefaultOffline()
	if other.Devices[0].Password == cfg.Devices[0].Password {
		t.Fatal("synthetic credentials are not random per process")
	}
	secrets := cfg.Secrets()
	if len(secrets) != 2 {
		t.Fatalf("Secrets() = %d entries, want 2", len(secrets))
	}
	for _, s := range secrets {
		if strings.TrimSpace(s) == "" {
			t.Fatal("empty secret in Secrets()")
		}
	}
}

func TestValidateBindAddressPolicy(t *testing.T) {
	privateCIDRs := []string{"10.20.0.0/16", "192.168.50.0/24"}
	cases := []struct {
		name    string
		addr    string
		cidrs   []string
		wantErr bool
	}{
		{"loopback v4", "127.0.0.1:1883", nil, false},
		{"loopback ephemeral", "127.0.0.1:0", nil, false},
		{"loopback v6", "[::1]:1883", nil, false},
		{"localhost", "localhost:1883", nil, false},
		{"wildcard v4", "0.0.0.0:1883", nil, true},
		{"wildcard v6", "[::]:1883", nil, true},
		{"missing host", ":1883", nil, true},
		{"empty", "", nil, true},
		{"public ip", "8.8.8.8:1883", nil, true},
		{"private but not authorized", "10.1.2.3:1883", nil, true},
		{"private authorized", "10.20.3.4:1883", privateCIDRs, false},
		{"private other cidr", "10.21.3.4:1883", privateCIDRs, true},
		{"private authorized 192.168", "192.168.50.7:1883", privateCIDRs, false},
		{"hostname bind", "example.invalid:1883", privateCIDRs, true},
		{"bad port", "127.0.0.1:notaport", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBindAddress(tc.addr, tc.cidrs)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateBindAddress(%q) = nil, want error", tc.addr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateBindAddress(%q) = %v, want nil", tc.addr, err)
			}
		})
	}
}

func TestValidateRejectsUnsafeDeviceRegistry(t *testing.T) {
	base := DefaultOffline()
	mutations := []struct {
		name string
		fn   func(cfg *Config)
	}{
		{"duplicate client id", func(cfg *Config) { cfg.Devices[1].ClientID = cfg.Devices[0].ClientID }},
		{"duplicate username", func(cfg *Config) { cfg.Devices[1].Username = cfg.Devices[0].Username }},
		{"wildcard publish", func(cfg *Config) { cfg.Devices[0].PublishTopic = "device-server/#" }},
		{"wildcard subscribe", func(cfg *Config) { cfg.Devices[0].SubscribeTopic = "devices/+/#" }},
		{"cross-device subscribe", func(cfg *Config) { cfg.Devices[0].SubscribeTopic = cfg.Devices[1].SubscribeTopic }},
		{"publish equals downlink", func(cfg *Config) { cfg.Devices[0].PublishTopic = cfg.Devices[0].SubscribeTopic }},
		{"too many devices", func(cfg *Config) { cfg.Devices = append(cfg.Devices, cfg.Devices[0]) }},
		{"empty password", func(cfg *Config) { cfg.Devices[0].Password = "" }},
		{"tls key without cert", func(cfg *Config) { cfg.Network.MQTTServerKeyFile = "key.pem" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *base
			cfg.Devices = append([]Device(nil), base.Devices...)
			tc.fn(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestValidateRejectsUnsupportedAudio(t *testing.T) {
	cases := []struct {
		name string
		fn   func(cfg *Config)
	}{
		{"pcm", func(cfg *Config) { cfg.Audio.Uplink.Format = "pcm" }},
		{"stereo uplink", func(cfg *Config) { cfg.Audio.Uplink.Channels = 2 }},
		{"bad frame duration", func(cfg *Config) { cfg.Audio.Uplink.FrameDuration = 33 }},
		{"bad sample rate", func(cfg *Config) { cfg.Audio.Downlink.SampleRate = 11025 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultOffline()
			tc.fn(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() = nil, want unsupported audio error")
			}
		})
	}
}

func writeConfig(t *testing.T, cfg *Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadLiveIsBlockedWithoutAuthorization(t *testing.T) {
	if _, err := Load(Live, ""); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(live, empty) error = %v, want ErrBlocked", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Load(Live, missing); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(live, missing file) error = %v, want ErrBlocked", err)
	}

	cfg := DefaultOffline()
	cfg.Mode = Live
	cfg.Network.PlaintextIsolatedApproved = true // isolated loopback test config
	cfg.Live = LiveConfig{
		Enabled:       true,
		Authorization: "",
		Cloud:         CloudConfig{APIBase: "https://api.invalid/v1", APIKey: "synthetic-key", Model: "synthetic"},
		MaxRounds:     1,
	}
	path := writeConfig(t, cfg)
	if _, err := Load(Live, path); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(live, enabled no authorization) error = %v, want ErrBlocked", err)
	}

	cfg.Live.Authorization = "user-approved: synthetic live test 2026-10-22"
	cfg.Live.Cloud.APIBase = "http://api.invalid/v1" // plaintext cloud is never accepted
	path = writeConfig(t, cfg)
	if _, err := Load(Live, path); err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(live, plaintext cloud) error = %v, want hard validation error", err)
	}
}

func TestLoadLiveAcceptsExplicitIsolatedAuthorization(t *testing.T) {
	cfg := DefaultOffline()
	cfg.Mode = Live
	cfg.Network.PlaintextIsolatedApproved = true // isolated loopback test config
	cfg.Live = LiveConfig{
		Enabled:       true,
		Authorization: "user-approved: synthetic live test 2026-10-22",
		Cloud:         CloudConfig{APIBase: "https://api.invalid/v1", APIKey: "synthetic-key", Model: "synthetic-model"},
		RemoteMCP:     RemoteMCPConfig{URL: "https://mcp.invalid/mcp", BearerToken: "synthetic-bearer", Transport: "streamable-http", TimeoutMilli: 3000},
		MaxRounds:     1,
	}
	path := writeConfig(t, cfg)
	loaded, err := Load(Live, path)
	if err != nil {
		t.Fatalf("Load(live) = %v", err)
	}
	if loaded.Mode != Live || loaded.Source == "" {
		t.Fatalf("loaded config mode/source = %q/%q", loaded.Mode, loaded.Source)
	}
	if got := len(loaded.Secrets()); got < 3 {
		t.Fatalf("Secrets() = %d, want at least device passwords + cloud key + bearer token", got)
	}
}

func TestLoadHardwareIsBlockedWithoutAuthorization(t *testing.T) {
	if _, err := Load(Hardware, ""); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(hardware, empty) = %v, want ErrBlocked", err)
	}
	cfg := DefaultOffline()
	cfg.Mode = Hardware
	cfg.Hardware = HardwareConfig{Enabled: true, Authorization: ""}
	path := writeConfig(t, cfg)
	if _, err := Load(Hardware, path); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(hardware, no authorization) = %v, want ErrBlocked", err)
	}
	cfg.Hardware.Authorization = "user-approved: synthetic device"
	cfg.Hardware.RestorePlan = ""
	path = writeConfig(t, cfg)
	if _, err := Load(Hardware, path); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Load(hardware, no restore plan) = %v, want ErrBlocked", err)
	}
}

func TestAuthorizedModesRequireTLSOrExplicitIsolation(t *testing.T) {
	live := DefaultOffline()
	live.Mode = Live
	live.Live = LiveConfig{
		Enabled:       true,
		Authorization: "user-approved: synthetic",
		Cloud:         CloudConfig{APIBase: "https://api.invalid/v1", APIKey: "synthetic-key", Model: "m"},
		MaxRounds:     1,
	}
	if err := live.Validate(); err != nil {
		t.Fatalf("Validate (structural) = %v", err)
	}
	if err := live.validateLive(); err == nil {
		t.Fatal("live validation accepted plaintext without explicit isolated approval")
	}
	live.Network.PlaintextIsolatedApproved = true
	if err := live.validateLive(); err != nil {
		t.Fatalf("live validation rejected explicit isolated plaintext: %v", err)
	}

	hw := DefaultOffline()
	hw.Mode = Hardware
	hw.Hardware = HardwareConfig{
		Enabled:       true,
		Authorization: "user-approved: synthetic device",
		RestorePlan:   "restore documented in local config",
		Devices: []HardwareDevice{{
			Alias:    "test-device",
			Board:    "synthetic",
			Firmware: "synthetic-0.1",
			MQTT: HardwareMQTT{
				BrokerURL:      "tls://isolated.invalid:8883",
				ClientID:       "synthetic-client",
				PublishTopic:   "device-server",
				SubscribeTopic: "devices/p2p/synthetic",
				CAFile:         "/tmp/test-ca.pem",
			},
		}},
	}
	if err := hw.validateHardware(); err == nil {
		t.Fatal("hardware validation accepted plaintext without explicit isolated approval")
	}
	hw.Network.PlaintextIsolatedApproved = true
	if err := hw.validateHardware(); err != nil {
		t.Fatalf("hardware validation rejected explicit isolated plaintext: %v", err)
	}
}

func TestLoadRejectsUnknownFieldsAndModeMismatch(t *testing.T) {
	cfg := DefaultOffline()
	data, _ := json.Marshal(cfg)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	raw["unexpected_field"] = true
	path := filepath.Join(t.TempDir(), "bad.json")
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Offline, path); err == nil {
		t.Fatal("Load() accepted unknown field")
	}

	cfg.Mode = Live
	path = writeConfig(t, cfg)
	if _, err := Load(Offline, path); err == nil {
		t.Fatal("Load() accepted mode mismatch")
	}
}

func TestTLSConfigsNeverDisableVerification(t *testing.T) {
	client, err := ClientTLSConfig("", "broker.test.invalid")
	if err != nil {
		t.Fatalf("ClientTLSConfig() = %v", err)
	}
	if client.InsecureSkipVerify {
		t.Fatal("client TLS config has InsecureSkipVerify set")
	}
	if client.MinVersion < 0x0303 {
		t.Fatalf("client TLS MinVersion = %x, want TLS 1.2+", client.MinVersion)
	}

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClientTLSConfig(caFile, "broker.test.invalid"); err == nil {
		t.Fatal("ClientTLSConfig() accepted an invalid CA bundle")
	}
	if _, err := LoadCAPool(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("LoadCAPool() accepted a missing CA file")
	}
}

func TestBlockedErrorMessages(t *testing.T) {
	err := error(&BlockedError{Mode: Live, Reason: "no authorization file"})
	if !errors.Is(err, ErrBlocked) {
		t.Fatal("BlockedError does not unwrap to ErrBlocked")
	}
	if !strings.Contains(err.Error(), "live verification blocked") {
		t.Fatalf("BlockedError message = %q", err.Error())
	}
}
