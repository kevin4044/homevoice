package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestA28MissingLiveAndHardwareConfigsBlockNonZero proves that the authorized
// entry points return the BLOCKED exit code (3) and write BLOCKED evidence when
// no authorization/config exists. They must never skip and report success.
func TestA28MissingLiveAndHardwareConfigsBlockNonZero(t *testing.T) {
	for _, mode := range []string{"live", "hardware"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "missing-"+mode+".json")
			evidencePath := filepath.Join(dir, "evidence-"+mode+".md")
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{
				mode,
				"--config", configPath,
				"--report", filepath.Join(dir, "raw.json"),
				"--evidence", evidencePath,
			}, &stdout, &stderr)
			if code != exitBlocked {
				t.Fatalf("%s exit = %d, want %d (BLOCKED); stderr=%s", mode, code, exitBlocked, stderr.String())
			}
			if !strings.Contains(stderr.String(), "BLOCKED") {
				t.Fatalf("%s stderr does not say BLOCKED: %s", mode, stderr.String())
			}
			data, err := os.ReadFile(evidencePath)
			if err != nil {
				t.Fatalf("%s blocked evidence: %v", mode, err)
			}
			text := string(data)
			for _, want := range []string{"BLOCKED", "not executed", "not a pass"} {
				if !strings.Contains(text, want) {
					t.Fatalf("%s evidence missing %q:\n%s", mode, want, text)
				}
			}
		})
	}
}

func TestReportOfflineRequiresInputs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"report", "offline"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("report offline exit = %d, want %d", code, exitUsage)
	}
}
