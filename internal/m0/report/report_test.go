package report

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/fixtures"
)

func encodeEvents(t *testing.T, events []TestEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events {
		ev.Time = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func allPassingEvents() []TestEvent {
	events := []TestEvent{
		{Action: "start", Package: "github.com/homevoice/homevoice/internal/m0/acceptance"},
		{Action: "pass", Package: "github.com/homevoice/homevoice/internal/m0/acceptance"},
	}
	for _, id := range ExpectedAIDs {
		test := "Test" + id + "SyntheticEvidence"
		events = append(events,
			TestEvent{Action: "run", Package: "github.com/homevoice/homevoice/internal/m0/acceptance", Test: test},
			TestEvent{Action: "pass", Package: "github.com/homevoice/homevoice/internal/m0/acceptance", Test: test},
		)
	}
	return events
}

func baseMeta() Metadata {
	return Metadata{
		Branch:                "feature/m0-transport-probe",
		Head:                  "27da5e2",
		WorkingTree:           "dirty (uncommitted changes; see snapshot/git-status.txt)",
		GeneratedAt:           time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC),
		GoVersion:             "go1.x",
		GOOS:                  "darwin",
		GOARCH:                "arm64",
		CGOEnabled:            "1",
		OpusVersion:           "libopus 1.6.1",
		Dependencies:          map[string]string{"github.com/mochi-mqtt/server/v2": "v2.7.9"},
		FixtureSHA256:         map[string]string{"udp-vectors.json": "abc"},
		LiveBlockedReason:     "no authorized live config",
		HardwareBlockedReason: "no authorized device",
	}
}

func TestBuildPassingReport(t *testing.T) {
	events, err := ParseTestEvents(bytes.NewReader(encodeEvents(t, allPassingEvents())))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(events, baseMeta())
	if err != nil {
		t.Fatalf("Build = %v", err)
	}
	if !strings.Contains(rep.Overall, "OFFLINE VERIFICATION PASSED") {
		t.Fatalf("overall = %q", rep.Overall)
	}
	if len(rep.Acceptance) != len(ExpectedAIDs) {
		t.Fatalf("acceptance rows = %d", len(rep.Acceptance))
	}
	if len(rep.Live) != 3 || len(rep.Hardware) != 6 {
		t.Fatalf("live=%d hardware=%d rows", len(rep.Live), len(rep.Hardware))
	}
	for _, row := range append(rep.Live, rep.Hardware...) {
		if row.Status != "BLOCKED" {
			t.Fatalf("%s status = %s, want BLOCKED", row.ID, row.Status)
		}
	}

	var md bytes.Buffer
	if err := rep.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	text := md.String()
	for _, want := range []string{
		"OFFLINE VERIFICATION PASSED",
		"A01", "A28", "BLOCKED", "AES-128-CTR has no authentication tag",
		"not executed", "make verify-m0", "H06", "L03",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown missing %q", want)
		}
	}
	if strings.Contains(text, "M0 ACCEPTED") || strings.Contains(text, "M0 PASSED") {
		t.Fatal("report overclaims M0 acceptance")
	}

	var js bytes.Buffer
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON report invalid: %v", err)
	}
	if decoded.Overall != rep.Overall {
		t.Fatalf("JSON overall = %q", decoded.Overall)
	}
}

func TestReportCandidateProvenance(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"clean", "committed HEAD shown above with a clean worktree"},
		{"dirty (uncommitted changes)", "not evidence for that commit alone"},
		{"unknown", "cannot establish a clean fixed candidate"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			meta := baseMeta()
			meta.WorkingTree = tc.status
			rep, err := Build(allPassingEvents(), meta)
			if err != nil {
				t.Fatal(err)
			}
			var md, js bytes.Buffer
			if err := rep.WriteMarkdown(&md); err != nil {
				t.Fatal(err)
			}
			if err := rep.WriteJSON(&js); err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{md.String(), js.String()} {
				if !strings.Contains(output, tc.want) {
					t.Fatalf("report missing provenance %q", tc.want)
				}
				for _, stale := range []string{"no commit/push authorized", "candidate is the uncommitted working tree"} {
					if strings.Contains(output, stale) {
						t.Fatalf("report contains stale assertion %q", stale)
					}
				}
				if !strings.Contains(output, "send goroutine cannot be forcibly stopped") {
					t.Fatal("report lost legacy sender limitation")
				}
			}
		})
	}
}

func TestBuildFailsOnMissingSkippedOrFailed(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		events := allPassingEvents()
		filtered := events[:0]
		for _, ev := range events {
			if strings.Contains(ev.Test, "A28") {
				continue
			}
			filtered = append(filtered, ev)
		}
		parsed, _ := ParseTestEvents(bytes.NewReader(encodeEvents(t, filtered)))
		rep, err := Build(parsed, baseMeta())
		if err == nil {
			t.Fatal("Build accepted a missing A28")
		}
		if rep.Overall != "OFFLINE VERIFICATION FAILED" {
			t.Fatalf("overall = %q", rep.Overall)
		}
		if !strings.Contains(err.Error(), "A28") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		events := allPassingEvents()
		for i := range events {
			if strings.Contains(events[i].Test, "A10") {
				if events[i].Action == "pass" {
					events[i].Action = "skip"
				}
			}
		}
		parsed, _ := ParseTestEvents(bytes.NewReader(encodeEvents(t, events)))
		if _, err := Build(parsed, baseMeta()); err == nil {
			t.Fatal("Build accepted a skipped A10")
		}
	})
	t.Run("failed", func(t *testing.T) {
		events := allPassingEvents()
		events = append(events, TestEvent{Action: "fail", Package: "github.com/homevoice/homevoice/internal/m0/acceptance", Test: "TestA10Something"})
		parsed, _ := ParseTestEvents(bytes.NewReader(encodeEvents(t, events)))
		if _, err := Build(parsed, baseMeta()); err == nil {
			t.Fatal("Build accepted a failed A10 test")
		}
	})
	t.Run("package failure", func(t *testing.T) {
		events := append(allPassingEvents(), TestEvent{Action: "fail", Package: "github.com/homevoice/homevoice/internal/m0/broker"})
		parsed, _ := ParseTestEvents(bytes.NewReader(encodeEvents(t, events)))
		if _, err := Build(parsed, baseMeta()); err == nil {
			t.Fatal("Build accepted a failed package")
		}
	})
}

func TestParseTestEventsIgnoresNoise(t *testing.T) {
	input := "not json\n" + string(encodeEvents(t, allPassingEvents())) + "\n# build noise\n"
	events, err := ParseTestEvents(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseTestEvents = %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}
	if _, err := ParseTestEvents(strings.NewReader("nothing here")); err == nil {
		t.Fatal("ParseTestEvents accepted input with no events")
	}
}

func TestCollectFixtureHashes(t *testing.T) {
	root, err := fixtures.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := CollectFixtureHashes(root)
	if err != nil {
		t.Fatalf("CollectFixtureHashes = %v", err)
	}
	if len(hashes["udp-vectors.json"]) != 64 || len(hashes["control-messages.json"]) != 64 {
		t.Fatalf("hashes = %+v", hashes)
	}
}

func TestCollectDependencyVersionsFromGoMod(t *testing.T) {
	root, err := fixtures.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	deps, err := CollectDependencyVersionsFromGoMod(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("CollectDependencyVersionsFromGoMod: %v", err)
	}
	for _, dep := range []string{
		"github.com/mochi-mqtt/server/v2",
		"github.com/eclipse/paho.mqtt.golang",
	} {
		if deps[dep] == "" {
			t.Fatalf("missing locked dependency %q in %+v", dep, deps)
		}
		if !strings.HasPrefix(deps[dep], "v") {
			t.Fatalf("%s version = %q", dep, deps[dep])
		}
	}
	if _, err := CollectDependencyVersionsFromGoMod(filepath.Join(t.TempDir(), "missing.mod")); err == nil {
		t.Fatal("missing go.mod accepted")
	}
}

func TestValidateIDs(t *testing.T) {
	if err := ValidateIDs([]string{"A01", "A28"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"A00", "A29", "B01", "A1"} {
		if err := ValidateIDs([]string{bad}); err == nil {
			t.Fatalf("ValidateIDs accepted %q", bad)
		}
	}
}
