// Package fixtures locates and loads the self-contained M0 test fixtures in
// docs/milestones/M0-transport-probe/fixtures. The fixtures are synthetic
// public test data; they are not firmware captures and must not be edited to
// accommodate an implementation.
package fixtures

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// DirName is the fixture directory relative to the repository root.
const DirName = "docs/milestones/M0-transport-probe/fixtures"

// RepoRoot walks up from the process working directory (or the source file
// directory as a fallback) until it finds go.mod.
func RepoRoot() (string, error) {
	if wd, err := os.Getwd(); err == nil {
		if root, ok := findRoot(wd); ok {
			return root, nil
		}
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		if root, ok := findRoot(filepath.Dir(file)); ok {
			return root, nil
		}
	}
	return "", fmt.Errorf("repository root (go.mod) not found")
}

func findRoot(start string) (string, bool) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Path returns the absolute path of a fixture file by base name.
func Path(name string) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, DirName, name), nil
}

// UDPVector is one public UDP crypto/frame vector.
type UDPVector struct {
	ID           string `json:"id"`
	KeyHex       string `json:"key_hex"`
	BaseNonceHex string `json:"base_nonce_hex"`
	Header       struct {
		PacketType int    `json:"packet_type"`
		Flags      int    `json:"flags"`
		PayloadLen int    `json:"payload_len"`
		SSRC       uint32 `json:"ssrc"`
		Timestamp  uint32 `json:"timestamp"`
		Sequence   uint32 `json:"sequence"`
	} `json:"header"`
	HeaderHex            string `json:"header_hex"`
	PlaintextHex         string `json:"plaintext_hex"`
	CiphertextHex        string `json:"ciphertext_hex"`
	PacketHex            string `json:"packet_hex"`
	ExpectedPlaintextHex string `json:"expected_plaintext_hex"`
	Expected             string `json:"expected"`
	Note                 string `json:"note"`
}

// UDPSuite is the UDP vector fixture file.
type UDPSuite struct {
	FixtureVersion string      `json:"fixture_version"`
	Purpose        string      `json:"purpose"`
	Verification   string      `json:"verification"`
	Vectors        []UDPVector `json:"vectors"`
}

// LoadUDPVectors loads the public UDP AES-CTR frame vectors.
func LoadUDPVectors() (*UDPSuite, error) {
	path, err := Path("udp-vectors.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var suite UDPSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if suite.FixtureVersion == "" || len(suite.Vectors) == 0 {
		return nil, fmt.Errorf("%s is empty or missing fixture_version", path)
	}
	return &suite, nil
}

// ControlMessage is one synthetic MQTT application message fixture.
type ControlMessage struct {
	ID        string          `json:"id"`
	Direction string          `json:"direction"`
	Body      json.RawMessage `json:"body"`
}

// ControlSuite is the control-message fixture file.
type ControlSuite struct {
	FixtureVersion string           `json:"fixture_version"`
	Purpose        string           `json:"purpose"`
	Messages       []ControlMessage `json:"messages"`
}

// LoadControlMessages loads the synthetic MQTT application messages.
func LoadControlMessages() (*ControlSuite, error) {
	path, err := Path("control-messages.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var suite ControlSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if suite.FixtureVersion == "" || len(suite.Messages) == 0 {
		return nil, fmt.Errorf("%s is empty or missing fixture_version", path)
	}
	return &suite, nil
}

// Body returns the raw JSON body of a control message fixture by id.
func (s *ControlSuite) Body(id string) ([]byte, error) {
	for _, m := range s.Messages {
		if m.ID == id {
			return append([]byte(nil), m.Body...), nil
		}
	}
	return nil, fmt.Errorf("control fixture %q not found", id)
}
