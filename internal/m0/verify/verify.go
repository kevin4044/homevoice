// Package verify implements the authorized live/hardware entry points.
//
// SECURITY AND HONESTY RULES:
//   - No authorization/config means BLOCKED (exit 3), never a skipped success.
//   - Results reported PASS must come from an actual authorized call; the
//     offline candidate does not fabricate live or hardware evidence.
//   - The implementation is deliberately bounded: one model config, read-only
//     remote MCP, no Home Assistant writes, no OTA, no unapproved device writes.
package verify

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/homevoice/homevoice/internal/m0/config"
)

// Options controls evidence output for authorized runs.
type Options struct {
	ReportPath   string // raw/sensitive JSON, ignored local/ directory
	EvidencePath string // sanitized markdown
	Stdout       io.Writer
}

// Result summarizes one authorized verification run.
type Result struct {
	Mode    config.Mode
	Summary string
}

// RunAuthorized dispatches live or hardware verification. The actual cloud and
// device workflows are implemented in live.go/hardware.go; this file only
// validates the common boundary.
func RunAuthorized(ctx context.Context, cfg *config.Config, opts Options) (Result, error) {
	if cfg == nil {
		return Result{}, &config.BlockedError{Mode: config.Live, Reason: "nil config"}
	}
	switch cfg.Mode {
	case config.Live:
		return RunLive(ctx, cfg, opts)
	case config.Hardware:
		return RunHardware(ctx, cfg, opts)
	default:
		return Result{}, fmt.Errorf("verify: mode %q is not an authorized environment", cfg.Mode)
	}
}

// WriteBlockedEvidence records a sanitized BLOCKED result for an entry point
// that could not run. It never writes secrets.
func WriteBlockedEvidence(path string, mode config.Mode, reason string) error {
	if path == "" {
		return nil
	}
	content := fmt.Sprintf(`# %s verification blocked

- Generated: %s
- Mode: `+"`%s`"+`
- Status: **BLOCKED** (not executed, not a pass)
- Reason: %s

No cloud, Home Assistant, public endpoint or real device was contacted. This
entry point requires an explicit authorized configuration under the ignored
`+"`local/`"+` directory; see docs/milestones/M0-transport-probe/compatibility.md.
`, mode, time.Now().UTC().Format(time.RFC3339), mode, reason)
	return os.WriteFile(path, []byte(content), 0o600)
}
