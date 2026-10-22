package verify

import (
	"context"
	"fmt"

	"github.com/homevoice/homevoice/internal/m0/config"
)

// RunLive performs the authorized cloud / remote-MCP verification (L01-L03).
//
// This run had no authorization, endpoint, credential or budget, so live
// execution is deliberately BLOCKED and no result is claimed. The entry still
// validates the authorization file and records a sanitized BLOCKED evidence
// file instead of returning a skipped success.
func RunLive(ctx context.Context, cfg *config.Config, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if cfg == nil || cfg.Mode != config.Live {
		return Result{}, &config.BlockedError{Mode: config.Live, Reason: "live config was not loaded"}
	}
	reason := "authorized live execution was not performed in this offline candidate: no cloud provider, remote MCP endpoint, call budget or explicit user authorization was supplied for this run"
	if cfg.Live.Enabled && cfg.Live.Authorization != "" {
		reason = fmt.Sprintf("authorization reference %q is present, but this environment deliberately did not execute network calls; L01-L03 remain BLOCKED pending an explicitly authorized live run with provider specifics and budget", cfg.Live.Authorization)
	}
	if opts.EvidencePath != "" {
		_ = WriteBlockedEvidence(opts.EvidencePath, config.Live, reason)
	}
	return Result{}, &config.BlockedError{Mode: config.Live, Reason: reason}
}

// RunHardware performs the authorized real-device verification (H01-H06). It is
// blocked in this offline candidate because no device, isolated network,
// configuration-write method or restore plan was supplied.
func RunHardware(ctx context.Context, cfg *config.Config, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if cfg == nil || cfg.Mode != config.Hardware {
		return Result{}, &config.BlockedError{Mode: config.Hardware, Reason: "hardware config was not loaded"}
	}
	reason := "authorized real-device execution was not performed in this offline candidate: no test device, firmware version, isolated network, configuration write/restore method or explicit operation authorization was supplied"
	if opts.EvidencePath != "" {
		_ = WriteBlockedEvidence(opts.EvidencePath, config.Hardware, reason)
	}
	return Result{}, &config.BlockedError{Mode: config.Hardware, Reason: reason}
}
