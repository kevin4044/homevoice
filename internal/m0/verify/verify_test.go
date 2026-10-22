package verify

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/config"
)

func TestRunAuthorizedRejectsOfflineConfig(t *testing.T) {
	cfg := config.DefaultOffline()
	if _, err := RunAuthorized(t.Context(), cfg, Options{}); err == nil {
		t.Fatal("RunAuthorized accepted an offline config")
	}
}

func TestLiveAndHardwareBlockWithoutExecution(t *testing.T) {
	live := &config.Config{Mode: config.Live}
	if _, err := RunLive(t.Context(), live, Options{}); !errors.Is(err, config.ErrBlocked) {
		t.Fatalf("RunLive error = %v, want ErrBlocked", err)
	}
	hw := &config.Config{Mode: config.Hardware}
	if _, err := RunHardware(t.Context(), hw, Options{}); !errors.Is(err, config.ErrBlocked) {
		t.Fatalf("RunHardware error = %v, want ErrBlocked", err)
	}
	if _, err := RunLive(t.Context(), nil, Options{}); !errors.Is(err, config.ErrBlocked) {
		t.Fatalf("RunLive(nil) error = %v", err)
	}
}

func TestBlockedEvidenceIsSanitized(t *testing.T) {
	path := t.TempDir() + "/blocked.md"
	if err := WriteBlockedEvidence(path, config.Live, "no authorized config"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BLOCKED", "not executed", "no authorized config"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("blocked evidence missing %q: %s", want, data)
		}
	}
}
