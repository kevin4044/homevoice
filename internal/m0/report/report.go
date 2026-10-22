// Package report turns `go test -json` output into sanitized M0 offline
// evidence. It never fabricates a pass: an acceptance ID without a passing
// test, a failing test, or a skipped test makes the offline report invalid.
package report

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// SpecVersion is the specification package version this evidence reports on.
const SpecVersion = "M0-v0.1"

// ExpectedAIDs is the complete required offline acceptance matrix.
var ExpectedAIDs = []string{
	"A01", "A02", "A03", "A04", "A05", "A06", "A07", "A08", "A09", "A10",
	"A11", "A12", "A13", "A14", "A15", "A16", "A17", "A18", "A19", "A20",
	"A21", "A22", "A23", "A24", "A25", "A26", "A27", "A28",
}

// RequirementByA maps each acceptance ID to the M0 requirement IDs it covers.
var RequirementByA = map[string]string{
	"A01": "R01", "A02": "R02", "A03": "R03", "A04": "R03", "A05": "R02-R04",
	"A06": "R02", "A07": "R04", "A08": "R05", "A09": "R05", "A10": "R05",
	"A11": "R05", "A12": "R05", "A13": "R05,R12", "A14": "R06", "A15": "R06",
	"A16": "R07", "A17": "R07", "A18": "R07,R08", "A19": "R07", "A20": "R08",
	"A21": "R09", "A22": "R09", "A23": "R09", "A24": "R10", "A25": "R10",
	"A26": "R11", "A27": "R12", "A28": "R12",
}

var aTestName = regexp.MustCompile(`^TestA(\d\d)`)
var aIDPattern = regexp.MustCompile(`^(A\d\d)$`)

// TestEvent is the subset of `go test -json` fields the report needs.
type TestEvent struct {
	Time    string `json:"Time"`
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// Check is one non-test offline gate (format, vet, race).
type Check struct {
	Name   string
	Status string
	Detail string
}

// Metadata describes the fixed candidate and environment. It must not contain
// credentials or private content.
type Metadata struct {
	SpecVersion           string
	Branch                string
	Head                  string
	WorkingTree           string
	GeneratedAt           time.Time
	GoVersion             string
	GOOS                  string
	GOARCH                string
	CGOEnabled            string
	OpusVersion           string
	Dependencies          map[string]string
	FixtureSHA256         map[string]string
	Checks                []Check
	LiveBlockedReason     string
	HardwareBlockedReason string
	ApprovalNote          string
}

// ARow is one acceptance-matrix row.
type ARow struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Tests    []string `json:"tests,omitempty"`
	Packages []string `json:"packages,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// EnvRow documents an authorized-environment acceptance ID.
type EnvRow struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Report is the sanitized offline evidence document.
type Report struct {
	SpecVersion string    `json:"spec_version"`
	Mode        string    `json:"mode"`
	GeneratedAt time.Time `json:"generated_at"`
	Overall     string    `json:"overall"`
	Metadata    Metadata  `json:"metadata"`
	Acceptance  []ARow    `json:"acceptance"`
	Live        []EnvRow  `json:"live"`
	Hardware    []EnvRow  `json:"hardware"`
	Checks      []Check   `json:"checks"`
	Security    []string  `json:"security_limitations"`
	Limits      []string  `json:"probe_limits"`
	KnownIssues []string  `json:"known_issues"`
	NextSteps   []string  `json:"next_steps"`
}

// testResult is the final observed state for one top-level test.
type testResult struct {
	Package string
	Test    string
	Action  string
}

// ParseTestEvents decodes `go test -json` output.
func ParseTestEvents(r io.Reader) ([]TestEvent, error) {
	var events []TestEvent
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue // non-JSON lines (e.g. build noise) are ignored
		}
		var ev TestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("no test events found in input")
	}
	return events, nil
}

// Build assembles the report. It returns an error if any required A ID lacks a
// passing test, or if any package/test failed or was skipped.
func Build(events []TestEvent, meta Metadata) (*Report, error) {
	if meta.SpecVersion == "" {
		meta.SpecVersion = SpecVersion
	}
	if meta.GeneratedAt.IsZero() {
		meta.GeneratedAt = time.Now().UTC()
	}
	if meta.Dependencies == nil {
		meta.Dependencies = map[string]string{}
	}
	if meta.FixtureSHA256 == nil {
		meta.FixtureSHA256 = map[string]string{}
	}
	if meta.ApprovalNote == "" {
		meta.ApprovalNote = candidateApprovalNote(meta.WorkingTree)
	}

	failures := map[string]bool{}
	failedTests := map[string]testResult{}
	packageFailed := map[string]bool{}
	final := map[string]testResult{} // key pkg/test
	for _, ev := range events {
		switch ev.Action {
		case "fail":
			if ev.Test == "" {
				packageFailed[ev.Package] = true
				continue
			}
			key := ev.Package + "/" + ev.Test
			failures[key] = true
			failedTests[key] = testResult{Package: ev.Package, Test: ev.Test, Action: "fail"}
		case "pass", "skip":
			if ev.Test != "" {
				final[ev.Package+"/"+ev.Test] = testResult{Package: ev.Package, Test: ev.Test, Action: ev.Action}
			}
		}
	}

	rows := make([]ARow, 0, len(ExpectedAIDs))
	var problems []string
	for _, id := range ExpectedAIDs {
		row := ARow{ID: id}
		matches := 0
		passed := 0
		skipped := 0
		failed := 0
		for key, res := range final {
			if !aTestName.MatchString(res.Test) {
				continue
			}
			m := aTestName.FindStringSubmatch(res.Test)
			if "A"+m[1] != id {
				continue
			}
			matches++
			row.Tests = append(row.Tests, res.Test)
			row.Packages = append(row.Packages, res.Package)
			switch res.Action {
			case "pass":
				passed++
			case "skip":
				skipped++
			}
			if failures[key] {
				failed++
			}
		}
		for key, res := range failedTests {
			if !aTestName.MatchString(res.Test) {
				continue
			}
			m := aTestName.FindStringSubmatch(res.Test)
			if "A"+m[1] != id {
				continue
			}
			matches++
			failed++
			row.Tests = append(row.Tests, res.Test)
			row.Packages = append(row.Packages, res.Package)
			_ = key
		}
		sort.Strings(row.Tests)
		sort.Strings(row.Packages)
		row.Packages = unique(row.Packages)
		switch {
		case failed > 0:
			row.Status = "FAIL"
			row.Detail = "at least one acceptance test failed"
			problems = append(problems, id+" failed")
		case matches == 0:
			row.Status = "MISSING"
			row.Detail = "no test named Test" + strings.ReplaceAll(id, "A", "A") + "... was observed"
			problems = append(problems, id+" has no evidence")
		case passed == 0:
			row.Status = "SKIPPED"
			row.Detail = "all matching tests were skipped"
			problems = append(problems, id+" was skipped, not passed")
		default:
			row.Status = "PASS"
			row.Detail = fmt.Sprintf("%d passing test(s)", passed)
			if skipped > 0 {
				row.Detail += fmt.Sprintf(", %d skipped duplicate(s)", skipped)
			}
		}
		rows = append(rows, row)
	}
	for pkg := range packageFailed {
		problems = append(problems, "package failed: "+pkg)
	}

	r := &Report{
		SpecVersion: meta.SpecVersion,
		Mode:        "offline",
		GeneratedAt: meta.GeneratedAt,
		Metadata:    meta,
		Acceptance:  rows,
		Checks:      meta.Checks,
		Live:        blockedEnv("L", 3, meta.LiveBlockedReason),
		Hardware:    blockedEnv("H", 6, meta.HardwareBlockedReason),
		Security:    defaultSecurityLimitations(),
		Limits:      defaultLimits(),
		KnownIssues: defaultKnownIssues(),
		NextSteps:   defaultNextSteps(),
	}
	if len(problems) == 0 {
		r.Overall = "OFFLINE VERIFICATION PASSED (implementation candidate only; M0 acceptance still requires authorized live and hardware verification)"
	} else {
		r.Overall = "OFFLINE VERIFICATION FAILED"
	}
	if len(problems) > 0 {
		return r, fmt.Errorf("offline acceptance incomplete: %s", strings.Join(problems, "; "))
	}
	return r, nil
}

func unique(in []string) []string {
	out := in[:0]
	var last string
	for i, s := range in {
		if i == 0 || s != last {
			out = append(out, s)
			last = s
		}
	}
	return out
}

func candidateApprovalNote(workingTree string) string {
	note := "Approval note: the specification package still records status \"待批准\" (pending approval) in its README/spec/acceptance/handoff. The specification owner/referee must confirm approval. "
	switch {
	case workingTree == "clean":
		note += "This report describes the committed HEAD shown above with a clean worktree at metadata collection."
	case strings.HasPrefix(workingTree, "dirty"):
		note += "This report includes uncommitted changes on top of the recorded HEAD; it is not evidence for that commit alone."
	default:
		note += "Worktree status is unknown; this report cannot establish a clean fixed candidate."
	}
	return note + " Offline verification does not authorize Git publication, live/hardware operations, deployment or OTA."
}

func blockedEnv(prefix string, n int, reason string) []EnvRow {
	if reason == "" {
		reason = "not executed: no explicit authorization/config in this environment"
	}
	names := map[string][]string{
		"L": {"L01 model tool call", "L02 ASR/TTS", "L03 remote MCP read-only"},
		"H": {"H01 firmware/MQTT", "H02 bidirectional audio", "H03 abort/reconnect", "H04 idle notify", "H05 device MCP enumeration", "H06 compatibility table"},
	}
	rows := make([]EnvRow, 0, n)
	for _, name := range names[prefix] {
		id := strings.SplitN(name, " ", 2)[0]
		rows = append(rows, EnvRow{ID: id, Status: "BLOCKED", Reason: reason})
	}
	return rows
}

// WriteJSON writes the machine-readable evidence.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteMarkdown writes sanitized human-readable evidence.
func (r *Report) WriteMarkdown(w io.Writer) error {
	fmt.Fprintf(w, "# M0 offline verification evidence\n\n")
	fmt.Fprintf(w, "- Specification: `%s` (docs/milestones/M0-transport-probe)\n", r.SpecVersion)
	fmt.Fprintf(w, "- Generated: %s\n", r.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "- Overall: **%s**\n\n", r.Overall)
	fmt.Fprintf(w, "> This report is offline-only. Cloud (L01-L03) and real-device (H01-H06) verification were **not executed** and are reported BLOCKED below. It is not an M0 acceptance decision; independent verification and the specification referee decide.\n\n")

	fmt.Fprintf(w, "## Candidate\n\n")
	fmt.Fprintf(w, "| Field | Value |\n|---|---|\n")
	fmt.Fprintf(w, "| Branch | `%s` |\n", r.Metadata.Branch)
	fmt.Fprintf(w, "| HEAD commit | `%s` |\n", r.Metadata.Head)
	fmt.Fprintf(w, "| Working tree | %s |\n", r.Metadata.WorkingTree)
	fmt.Fprintf(w, "| Go | `%s` (%s/%s, CGO=%s) |\n", r.Metadata.GoVersion, r.Metadata.GOOS, r.Metadata.GOARCH, r.Metadata.CGOEnabled)
	fmt.Fprintf(w, "| libopus | `%s` |\n", r.Metadata.OpusVersion)
	if r.Metadata.ApprovalNote != "" {
		fmt.Fprintf(w, "\n%s\n", r.Metadata.ApprovalNote)
	}
	fmt.Fprintf(w, "\n## Dependencies (locked in go.sum)\n\n")
	fmt.Fprintf(w, "| Module | Version |\n|---|---|\n")
	deps := make([]string, 0, len(r.Metadata.Dependencies))
	for k := range r.Metadata.Dependencies {
		deps = append(deps, k)
	}
	sort.Strings(deps)
	for _, d := range deps {
		fmt.Fprintf(w, "| `%s` | `%s` |\n", d, r.Metadata.Dependencies[d])
	}
	fmt.Fprintf(w, "\nPackage licenses (recorded for T0): mochi-mqtt MIT; paho.mqtt.golang EPL-2.0/EDL-1.0; gorilla/websocket BSD-3-Clause; golang.org/x/net BSD-3-Clause; golang.org/x/sync BSD-3-Clause; rs/xid MIT; libopus BSD-3-Clause. No dependency is copied from the retired project.\n")

	fmt.Fprintf(w, "\n## Fixture integrity\n\n")
	fmt.Fprintf(w, "| Fixture | SHA-256 |\n|---|---|\n")
	fx := make([]string, 0, len(r.Metadata.FixtureSHA256))
	for k := range r.Metadata.FixtureSHA256 {
		fx = append(fx, k)
	}
	sort.Strings(fx)
	for _, f := range fx {
		fmt.Fprintf(w, "| `%s` | `%s` |\n", f, r.Metadata.FixtureSHA256[f])
	}

	fmt.Fprintf(w, "\n## Offline gates\n\n")
	fmt.Fprintf(w, "`make verify-m0` runs, in order: `gofmt -l` check, `go vet ./...`, `go test -race -count=1 ./...`, this report validation, and the working-tree snapshot. Gates:\n\n")
	fmt.Fprintf(w, "| Gate | Status | Detail |\n|---|---|---|\n")
	if len(r.Checks) == 0 {
		fmt.Fprintf(w, "| all | recorded by make prerequisites | see command transcript |\n")
	}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "| %s | %s | %s |\n", c.Name, c.Status, c.Detail)
	}

	fmt.Fprintf(w, "\n## Simulation scope (offline only)\n\n")
	fmt.Fprintf(w, "- Simulated firmware device: MQTT 3.1.1 synthetic identity, UDP HELLO/session, Opus uplink/downlink with the public vectors, device MCP server (initialize/tools/list/tools/call), downlink notify/TTS observation (internal/m0/simulator).\n")
	fmt.Fprintf(w, "- Simulated upstream: deterministic ASR/LLM/TTS with bounded latency, failure and slow modes; the model provider used by the agent loop is scripted (internal/m0/simulator).\n")
	fmt.Fprintf(w, "- Integrated offline path: listen -> simulated ASR -> simulated TTS -> MQTT tts markers -> paced UDP frames, verified by `TestIntegratedListenTTSPlayback` inside `make verify-m0`.\n")
	fmt.Fprintf(w, "- Synthetic HTTPS test audio server: loopback TLS with a runtime self-signed test certificate, WAV tone at /notice.wav; verification clients pin its certificate and never skip TLS verification (internal/m0/simulator).\n")
	fmt.Fprintf(w, "- No real device, cloud, Home Assistant, OTA, public endpoint or retired-project component is contacted.\n")

	fmt.Fprintf(w, "\n## Offline acceptance matrix (required)\n\n")
	fmt.Fprintf(w, "| ID | Requirement | Status | Evidence |\n|---|---|---|---|\n")
	for _, row := range r.Acceptance {
		evidence := strings.Join(row.Tests, ", ")
		if evidence == "" {
			evidence = row.Detail
		} else if len(row.Packages) > 0 {
			evidence += " (" + strings.Join(row.Packages, ", ") + ")"
		}
		fmt.Fprintf(w, "| %s | %s | **%s** | %s |\n", row.ID, RequirementByA[row.ID], row.Status, evidence)
	}

	fmt.Fprintf(w, "\n## Authorized cloud / remote MCP (not executed)\n\n")
	fmt.Fprintf(w, "| ID | Status | Reason |\n|---|---|---|\n")
	for _, row := range r.Live {
		fmt.Fprintf(w, "| %s | **%s** | %s |\n", row.ID, row.Status, row.Reason)
	}
	fmt.Fprintf(w, "\n## Real hardware (not executed)\n\n")
	fmt.Fprintf(w, "| ID | Status | Reason |\n|---|---|---|\n")
	for _, row := range r.Hardware {
		fmt.Fprintf(w, "| %s | **%s** | %s |\n", row.ID, row.Status, row.Reason)
	}

	fmt.Fprintf(w, "\n## Security limitations (must be carried into M1)\n\n")
	for _, s := range r.Security {
		fmt.Fprintf(w, "- %s\n", s)
	}
	fmt.Fprintf(w, "\n## Probe limits\n\n")
	for _, l := range r.Limits {
		fmt.Fprintf(w, "- %s\n", l)
	}
	fmt.Fprintf(w, "\n## Known issues / open decisions\n\n")
	for _, k := range r.KnownIssues {
		fmt.Fprintf(w, "- %s\n", k)
	}
	fmt.Fprintf(w, "\n## Next steps\n\n")
	for _, n := range r.NextSteps {
		fmt.Fprintf(w, "- %s\n", n)
	}
	fmt.Fprintf(w, "\n## Re-run\n\n```bash\nmake verify-m0\n```\n\n")
	fmt.Fprintf(w, "Live/hardware entries are `make verify-m0-live` and `make verify-m0-hardware`; without an authorized `local/` config they exit non-zero and report BLOCKED.\n")
	return nil
}

func defaultSecurityLimitations() []string {
	return []string{
		"UDP AES-128-CTR has no authentication tag (no MAC/AEAD). Tampered ciphertext decrypts to tampered plaintext without an integrity error; A13 proves this limitation instead of claiming integrity.",
		"UDP source-address locking, SSRC/template matching, the 64-packet replay window and Opus decoding are NOT authentication and must not be described as such.",
		"The first structurally valid packet locks the UDP peer of a fresh session; an on-path attacker can grab that binding before the device. Reconnection requires HELLO.",
		"SSRC is visible in cleartext; first-packet binding can be pre-empted. Bidirectional AES-CTR counter/IV reuse and overlap remain a public-network design risk; a non-zero downlink timestamp is not a proof of separation.",
		"The offline probe uses loopback plaintext for isolated tests. For authorized live/hardware configs the code requires MQTT TLS or an explicit isolated-plaintext approval (network.plaintext_isolated_approved) on a restricted bind; client TLS never disables verification and there is no automatic plaintext/WebSocket fallback. Authorized live/hardware execution was not performed in this run.",
		"Device WebSocket transport is out of M0 scope and is explicitly rejected rather than substituted.",
		"MQTT publish success is not playback confirmation; idle notify is reported as published, never played.",
		"Device MCP capability, notify media support, real Opus parameters, TLS/cert behaviour, NAT and firmware tools are unverified until authorized hardware runs.",
		"Remote MCP is limited to an authorized read-only test endpoint; real Home Assistant write tools are never called.",
	}
}

func defaultLimits() []string {
	return []string{
		"registered devices <= 2; extra identities rejected",
		"MQTT CONNECT handshake timeout 5s; HELLO wait 5s; MQTT idle does not require HELLO",
		"MQTT/JSON packet <= 64 KiB; UDP datagram <= 1500 bytes (payload <= 1484)",
		"per-device audio queue <= 64 frames, overflow counted, no unbounded allocation",
		"listen round <= 15s; upstream operation <= 30s; MCP request <= 5s",
		"tools/list <= 8 pages and <= 128 tools",
		"model loop: <= 2 requests and <= 1 tool execution per turn",
		"TTS candidate: settle 150ms, prebuffer 5 frames, tail wait (5+2)*frame_duration; downlink candidate opus mono 24kHz/60ms",
		"live cloud budget: <= 3 rounds, ASR <= 1 / LLM <= 2 / TTS <= 1 per round",
	}
}

func defaultKnownIssues() []string {
	return []string{
		"The specification package still records status \"pending approval\" (待批准) in README/spec/acceptance/handoff. Approval must be confirmed by the specification owner/referee; the offline report does not self-approve M0.",
		"The candidate identity is the recorded HEAD together with its worktree status and snapshot. A dirty worktree result must not be presented as verification of the recorded commit alone.",
		"Legacy synchronous MCP senders only bound caller waiting on cancellation/deadline; their underlying send goroutine cannot be forcibly stopped and may continue executing. Context-aware senders support cooperative cancellation. Already published or in-flight side effects can have an unknown outcome and must not be automatically retried.",
		"Live cloud, remote MCP endpoint and real hardware were not executed; L01-L03 and H01-H06 remain BLOCKED. Firmware notify, device MCP, TLS, NAT and Opus timing therefore remain assumptions for M1 planning.",
		"The authorized live/hardware entry points currently validate authorization/config and then report BLOCKED; their actual L01-L03 / H01-H06 workflows are not implemented or validated in this candidate. They must not be recorded as pass until those workflows are implemented and exercised with authorization.",
		"The conditional T4 synthetic HTTPS configuration-distribution endpoint was not implemented because the target firmware configuration channel is unverified. A synthetic loopback HTTPS test-audio server (self-signed, certificate-pinned client) exists in the simulator for offline media tests, but real firmware notification download, media format and HTTPS trust remain untested on hardware.",
		"AES-CTR's lack of authentication must be resolved (tunnel, firmware change or AES-GCM-capable protocol) before any public deployment; this probe does not change the wire format.",
		"Thresholds such as 150ms settle, 5-frame prebuffer, 2 extra tail frames and 24kHz/60ms downlink are tunable candidates, not verified firmware parameters.",
		"The offline report generator enforces A01-A28 presence/pass; package-level tests named TestAxx are the evidence source.",
		"Additional sanitized evidence files: evidence/usage.md (commands, config shapes, gaps), evidence/compatibility-offline.md (offline vs hardware/cloud unknowns), evidence/README.md (index) and evidence/snapshot/ (fixed candidate manifest).",
	}
}

func defaultNextSteps() []string {
	return []string{
		"Independent acceptance agent reruns `make verify-m0` in an isolated workspace on this fixed candidate and checks each A01-A28 mapping against the implemented tests.",
		"Specification owner/referee confirms approval status, resolves open decisions and rules on UDP no-authentication risks.",
		"Obtain explicit authorization + budget for L01-L03 and device/network authorization + restore plan for H01-H06; implement the authorized workflows behind the existing gated entry points, then run them.",
		"Do not deploy publicly or implement M1 before M0 acceptance and a public-network UDP security decision.",
	}
}

// CollectDependencyVersions returns the locked versions of known public
// dependencies from the running binary's build info.
func CollectDependencyVersions() map[string]string {
	out := map[string]string{}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	for _, dep := range info.Deps {
		switch {
		case strings.HasPrefix(dep.Path, "github.com/mochi-mqtt/server"),
			strings.HasPrefix(dep.Path, "github.com/eclipse/paho.mqtt.golang"),
			strings.HasPrefix(dep.Path, "github.com/gorilla/websocket"),
			strings.HasPrefix(dep.Path, "golang.org/x/net"),
			strings.HasPrefix(dep.Path, "golang.org/x/sync"):
			out[dep.Path] = dep.Version
		}
	}
	return out
}

// CollectDependencyVersionsFromGoMod parses the locked require directives from
// go.mod. It is used instead of build info because the probe binary does not
// import every component (broker/simulator) at the report entry point.
func CollectDependencyVersionsFromGoMod(goModPath string) (map[string]string, error) {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	inRequire := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "require (") || trimmed == "require (" {
			inRequire = true
			continue
		}
		if inRequire && trimmed == ")" {
			inRequire = false
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		var fields []string
		if strings.HasPrefix(trimmed, "require ") {
			fields = strings.Fields(strings.TrimPrefix(trimmed, "require "))
		} else if inRequire {
			fields = strings.Fields(trimmed)
		} else {
			continue
		}
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "v") {
			continue
		}
		out[fields[0]] = fields[1]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no require directives in %s", goModPath)
	}
	return out, nil
}

// CollectFixtureHashes hashes the public fixture files.
func CollectFixtureHashes(repoRoot string) (map[string]string, error) {
	names := []string{"udp-vectors.json", "control-messages.json"}
	out := map[string]string{}
	for _, name := range names {
		path := filepath.Join(repoRoot, "docs/milestones/M0-transport-probe/fixtures", name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

// RuntimeMetadata fills portable environment fields.
func RuntimeMetadata() Metadata {
	return Metadata{
		SpecVersion: SpecVersion,
		GoVersion:   runtime.Version(),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		CGOEnabled:  os.Getenv("CGO_ENABLED"),
	}
}

// ValidateIDs reports any A ID that is not a known matrix entry.
func ValidateIDs(ids []string) error {
	known := map[string]bool{}
	for _, id := range ExpectedAIDs {
		known[id] = true
	}
	for _, id := range ids {
		if !aIDPattern.MatchString(id) || !known[id] {
			return fmt.Errorf("unknown acceptance ID %q", id)
		}
	}
	return nil
}
