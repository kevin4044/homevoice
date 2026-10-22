// Command m0probe is the isolated Homevoice M0 transport probe entry point.
//
// Subcommands:
//
//	m0probe selftest                 toolchain/native dependency/fixture check
//	m0probe report offline ...       build sanitized offline evidence from go test -json
//	m0probe live --config ...        authorized cloud/remote-MCP verification (BLOCKED without config)
//	m0probe hardware --config ...    authorized real-device verification (BLOCKED without config)
//
// Exit codes: 0 success, 1 verification failure, 2 usage error, 3 BLOCKED
// (missing authorization/config). A blocked environment is never reported as a
// pass or a skip.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/fixtures"
	"github.com/homevoice/homevoice/internal/m0/report"
	"github.com/homevoice/homevoice/internal/m0/verify"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitBlocked = 3
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "selftest":
		return runSelftest(stdout, stderr)
	case "report":
		if len(args) < 2 || args[1] != "offline" {
			fmt.Fprintln(stderr, "usage: m0probe report offline --test-json <file> --out <file> [--json-out <file>] [--gate name=status]")
			return exitUsage
		}
		return runOfflineReport(args[2:], stdout, stderr)
	case "live":
		return runAuthorized(ctx, config.Live, args[1:], stdout, stderr)
	case "hardware":
		return runAuthorized(ctx, config.Hardware, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return exitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `homevoice M0 probe (isolated transport verification)

  m0probe selftest
  m0probe report offline --test-json <file> --out <report.md> [--json-out <report.json>] [--gate name=status]
  m0probe live --config <local/live.json> --report <raw.json> --evidence <evidence.md>
  m0probe hardware --config <local/hardware.json> --report <raw.json> --evidence <evidence.md>

Offline mode never contacts a real device, cloud, HA or public endpoint.
Live/hardware modes exit 3 (BLOCKED) without an explicit authorized config.
`)
}

// gateFlag collects repeated --gate name=status[:detail] values.
type gateFlag []report.Check

func (g *gateFlag) String() string { return fmt.Sprint([]report.Check(*g)) }

func (g *gateFlag) Set(v string) error {
	if v == "" {
		return errors.New("empty gate")
	}
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return fmt.Errorf("gate %q must be name=status[:detail]", v)
	}
	status, detail, _ := strings.Cut(rest, ":")
	*g = append(*g, report.Check{Name: name, Status: status, Detail: detail})
	return nil
}

func runSelftest(stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "homevoice M0 offline selftest\n")
	fmt.Fprintf(stdout, "go: %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(stdout, "cgo_enabled: %s\n", os.Getenv("CGO_ENABLED"))
	opus := audio.OpusVersion()
	fmt.Fprintf(stdout, "libopus: %s\n", opus)
	if opus == "" || strings.Contains(opus, "unavailable") {
		fmt.Fprintln(stderr, "FAIL: libopus is unavailable; native dependency is required for audio verification")
		return exitFailure
	}
	root, err := fixtures.RepoRoot()
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: repository root: %v\n", err)
		return exitFailure
	}
	hashes, err := report.CollectFixtureHashes(root)
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: fixtures: %v\n", err)
		return exitFailure
	}
	names := make([]string, 0, len(hashes))
	for n := range hashes {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(stdout, "fixtures_ok: true\n")
	for _, n := range names {
		fmt.Fprintf(stdout, "fixture_sha256 %s %s\n", n, hashes[n])
	}
	deps, err := report.CollectDependencyVersionsFromGoMod(filepath.Join(root, "go.mod"))
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: read dependencies: %v\n", err)
		return exitFailure
	}
	depNames := make([]string, 0, len(deps))
	for d := range deps {
		depNames = append(depNames, d)
	}
	sort.Strings(depNames)
	for _, d := range depNames {
		fmt.Fprintf(stdout, "dependency %s %s\n", d, deps[d])
	}
	// The offline config must validate and bind to loopback only.
	cfg, err := config.Load(config.Offline, "")
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: offline config: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "offline_devices: %d\n", len(cfg.Devices))
	fmt.Fprintf(stdout, "offline_bind: %s / %s\n", cfg.Network.MQTTBind, cfg.Network.UDPBind)
	fmt.Fprintln(stdout, "selftest: PASS")
	return exitOK
}

func runOfflineReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report offline", flag.ContinueOnError)
	fs.SetOutput(stderr)
	testJSON := fs.String("test-json", "", "path to go test -json output")
	out := fs.String("out", "", "markdown evidence output path")
	jsonOut := fs.String("json-out", "", "optional JSON evidence output path")
	var gates gateFlag
	fs.Var(&gates, "gate", "offline gate result, e.g. fmt-check=PASS")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *testJSON == "" || *out == "" {
		fmt.Fprintln(stderr, "report offline: --test-json and --out are required")
		return exitUsage
	}
	f, err := os.Open(*testJSON)
	if err != nil {
		fmt.Fprintf(stderr, "report offline: open test JSON: %v\n", err)
		return exitFailure
	}
	defer f.Close()
	events, err := report.ParseTestEvents(f)
	if err != nil {
		fmt.Fprintf(stderr, "report offline: %v\n", err)
		return exitFailure
	}

	root, err := fixtures.RepoRoot()
	if err != nil {
		fmt.Fprintf(stderr, "report offline: repository root: %v\n", err)
		return exitFailure
	}
	meta := report.RuntimeMetadata()
	meta.OpusVersion = audio.OpusVersion()
	meta.Branch = gitValue(root, "branch", "--show-current")
	meta.Head = gitValue(root, "rev-parse", "HEAD")
	status := gitValue(root, "status", "--porcelain=v1")
	switch status {
	case "":
		meta.WorkingTree = "clean"
	case "unknown":
		meta.WorkingTree = "unknown (git status could not be read)"
	default:
		meta.WorkingTree = "dirty (uncommitted changes; see snapshot/git-status.txt)"
	}
	meta.Dependencies, err = report.CollectDependencyVersionsFromGoMod(filepath.Join(root, "go.mod"))
	if err != nil {
		fmt.Fprintf(stderr, "report offline: dependencies: %v\n", err)
		return exitFailure
	}
	meta.FixtureSHA256, err = report.CollectFixtureHashes(root)
	if err != nil {
		fmt.Fprintf(stderr, "report offline: fixture hashes: %v\n", err)
		return exitFailure
	}
	meta.Checks = []report.Check(gates)
	if _, err := config.Load(config.Live, ""); err != nil {
		meta.LiveBlockedReason = err.Error()
	}
	if _, err := config.Load(config.Hardware, ""); err != nil {
		meta.HardwareBlockedReason = err.Error()
	}

	rep, buildErr := report.Build(events, meta)
	if rep != nil {
		if err := os.MkdirAll(filepathDir(*out), 0o755); err != nil {
			fmt.Fprintf(stderr, "report offline: create output dir: %v\n", err)
			return exitFailure
		}
		if err := writeMarkdown(*out, rep); err != nil {
			fmt.Fprintf(stderr, "report offline: write markdown: %v\n", err)
			return exitFailure
		}
		if *jsonOut != "" {
			if err := os.MkdirAll(filepathDir(*jsonOut), 0o755); err != nil {
				fmt.Fprintf(stderr, "report offline: create JSON dir: %v\n", err)
				return exitFailure
			}
			if err := writeJSON(*jsonOut, rep); err != nil {
				fmt.Fprintf(stderr, "report offline: write JSON: %v\n", err)
				return exitFailure
			}
		}
	}
	if buildErr != nil {
		fmt.Fprintf(stderr, "OFFLINE VERIFICATION FAILED: %v\n", buildErr)
		if rep != nil {
			fmt.Fprintf(stderr, "partial evidence written to %s\n", *out)
		}
		return exitFailure
	}
	fmt.Fprintf(stdout, "offline evidence: %s\n", *out)
	if *jsonOut != "" {
		fmt.Fprintf(stdout, "offline evidence JSON: %s\n", *jsonOut)
	}
	fmt.Fprintln(stdout, rep.Overall)
	return exitOK
}

func runAuthorized(ctx context.Context, mode config.Mode, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(mode), flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "authorized config file under ignored local/")
	reportPath := fs.String("report", "", "raw (sensitive) result JSON path under ignored local/")
	evidencePath := fs.String("evidence", "", "sanitized evidence markdown path")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cfgPath == "" {
		fmt.Fprintf(stderr, "%s: --config is required; missing authorization/config is BLOCKED\n", mode)
		return exitBlocked
	}
	cfg, err := config.Load(mode, *cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrBlocked) {
			reason := err.Error()
			fmt.Fprintf(stderr, "BLOCKED: %s\n", reason)
			if *evidencePath != "" {
				_ = verify.WriteBlockedEvidence(*evidencePath, mode, reason)
			}
			return exitBlocked
		}
		fmt.Fprintf(stderr, "%s: %v\n", mode, err)
		return exitFailure
	}
	res, err := verify.RunAuthorized(ctx, cfg, verify.Options{ReportPath: *reportPath, EvidencePath: *evidencePath, Stdout: stdout})
	if err != nil {
		if errors.Is(err, config.ErrBlocked) {
			fmt.Fprintf(stderr, "BLOCKED: %v\n", err)
			return exitBlocked
		}
		fmt.Fprintf(stderr, "%s verification failed: %v\n", mode, err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "%s verification: %s\n", mode, res.Summary)
	return exitOK
}

func gitValue(root string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func writeMarkdown(path string, rep *report.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return rep.WriteMarkdown(f)
}

func writeJSON(path string, rep *report.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return rep.WriteJSON(f)
}

func filepathDir(path string) string {
	i := strings.LastIndex(path, string(os.PathSeparator))
	if i < 0 {
		return "."
	}
	return path[:i]
}
