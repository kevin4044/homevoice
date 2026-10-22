# Homevoice M0 transport probe.
#
# The probe is isolated test tooling, not a production service. All entry
# points below are offline unless the target name contains -live or
# -hardware; those two require explicit local authorization files and are
# blocked (non-zero exit) when those are absent.
#
# Build caches are kept inside the repository (.cache/) because this
# environment does not allow writing to the user's global Go caches. Run
# `make clean` to remove them; it never touches local/ or evidence/.

SHELL := /bin/bash
ROOT := $(CURDIR)
CACHE := $(ROOT)/.cache
EVIDENCE := $(ROOT)/docs/milestones/M0-transport-probe/evidence
LOCAL := $(ROOT)/local

export GOPATH := $(CACHE)/gopath
export GOMODCACHE := $(CACHE)/gopath/pkg/mod
export GOCACHE := $(CACHE)/gocache
export CGO_ENABLED := 1
# libopus is a native dependency of the offline audio tests. Homebrew on
# Apple silicon installs it under /opt/homebrew; override OPUS_PREFIX on
# other hosts (the cgo directives also probe /usr/local and system paths).
OPUS_PREFIX ?= /opt/homebrew
export CGO_CFLAGS := -I$(OPUS_PREFIX)/include
export CGO_LDFLAGS := -L$(OPUS_PREFIX)/lib

HV_LIVE_CONFIG ?= local/live.json
HV_HARDWARE_CONFIG ?= local/hardware.json

.PHONY: all check-deps fmt-check vet test verify-m0 verify-m0-live verify-m0-hardware snapshot clean help

all: verify-m0

help:
	@echo "homevoice M0 (isolated probe) targets:"
	@echo "  make check-deps            print toolchain/native dependency versions"
	@echo "  make fmt-check             fail if any Go file is not gofmt-ed"
	@echo "  make vet                   go vet ./..."
	@echo "  make test                  go test -race -count=1 ./..."
	@echo "  make verify-m0             offline acceptance (A01-A28) + sanitized report"
	@echo "  make verify-m0-live        authorized cloud/remote-MCP verification (blocked without local config)"
	@echo "  make verify-m0-hardware    authorized real-device verification (blocked without local config)"
	@echo "  make snapshot              write candidate working-tree snapshot evidence"
	@echo "  make clean                 remove repository-local build caches only"

check-deps:
	@go version
	@go env GOOS GOARCH CGO_ENABLED GOPATH GOMODCACHE
	@go run ./cmd/m0probe selftest

fmt-check:
	@out="$$(gofmt -l cmd internal)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt required for:"; echo "$$out"; exit 1; \
	fi

vet:
	@go vet ./...

test:
	@go test -race -count=1 ./...

# Offline acceptance. No live cloud, real device, HA or public-network
# endpoint is contacted. Missing live/hardware conditions are reported as
# BLOCKED in the generated evidence, never as a pass.
verify-m0: fmt-check vet
	@mkdir -p "$(CACHE)" "$(EVIDENCE)"
	@set -o pipefail; go test -race -count=1 -json ./... | tee "$(CACHE)/test-results.json" >/dev/null
	@go run ./cmd/m0probe report offline --test-json "$(CACHE)/test-results.json" --out "$(EVIDENCE)/offline-report.md" --json-out "$(EVIDENCE)/offline-report.json" --gate "gofmt=PASS:make fmt-check prerequisite" --gate "go-vet=PASS:make vet prerequisite" --gate "race=PASS:go test -race -count=1 ./..."
	@$(MAKE) --no-print-directory snapshot

# Live mode is intentionally blocked unless the operator has placed an
# explicit authorization file in the ignored local/ directory. A missing
# file is a BLOCKED result (exit code 3), not a skipped success.
verify-m0-live:
	@mkdir -p "local/reports"
	@go run ./cmd/m0probe live --config "$(HV_LIVE_CONFIG)" --report "local/reports/live-report.json" --evidence "docs/milestones/M0-transport-probe/evidence/live-report.md"

verify-m0-hardware:
	@mkdir -p "local/reports"
	@go run ./cmd/m0probe hardware --config "$(HV_HARDWARE_CONFIG)" --report "local/reports/hardware-report.json" --evidence "docs/milestones/M0-transport-probe/evidence/hardware-report.md"

snapshot:
	@mkdir -p "$(EVIDENCE)/snapshot"
	@git rev-parse HEAD > "$(EVIDENCE)/snapshot/git-head.txt"
	@git branch --show-current > "$(EVIDENCE)/snapshot/git-branch.txt"
	@git status --porcelain=v1 > "$(EVIDENCE)/snapshot/git-status.txt"
	@find cmd internal -type f \( -name '*.go' -o -name '*.json' \) -print0 \
		| sort -z \
		| xargs -0 shasum -a 256 > "$(EVIDENCE)/snapshot/sha256.txt"
	@echo "candidate snapshot written to $(EVIDENCE)/snapshot (see recorded HEAD, worktree status and hashes)"

clean:
	@if [ -d "$(CACHE)" ]; then 		chmod -R u+w "$(CACHE)" 2>/dev/null || true; 		rm -rf "$(CACHE)"; 	fi
	@echo "removed repository-local build caches; local/ and evidence/ were preserved"
