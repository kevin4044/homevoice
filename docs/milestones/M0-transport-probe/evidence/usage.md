# M0 offline candidate: usage, evidence and re-run guide

Specification: `M0-v0.1` (docs/milestones/M0-transport-probe). This candidate is
**offline-only** and is not an M0 acceptance decision. The recorded verification
did not contact cloud services, Home Assistant or real devices. It does not
authorize OTA, public deployment or Git publication. See [reconciliation.md](reconciliation.md)
for the fixed `4421991` evidence and the separate uncommitted report-tool changes.

## Prerequisites

- Go 1.27.1 (recorded in evidence; `go.mod` uses language version 1.27.1).
- CGO enabled and native libopus 1.6.1 headers/library. On Apple silicon
  Homebrew: `/opt/homebrew/include/opus/opus.h`, `/opt/homebrew/lib/libopus.dylib`.
  No `pkg-config` binary is required; the cgo binding carries the include/lib
  directories. Override `OPUS_PREFIX` for other locations.
- First build may download public Go modules (mochi-mqtt, paho.mqtt.golang and
  their public transitive deps). Build caches are kept in the repository
  (`.cache/`, ignored) because the user's global Go caches are sandboxed. The
  verification itself makes **no runtime network calls**.

## Offline verification (the only executed entry)

```bash
cd <homevoice repo>
make check-deps     # Go/OS/libopus/fixtures/dependency versions
make verify-m0      # gofmt + go vet + go test -race + report validation + snapshot
```

`make verify-m0` writes:

- `docs/milestones/M0-transport-probe/evidence/offline-report.md`
- `docs/milestones/M0-transport-probe/evidence/offline-report.json`
- `docs/milestones/M0-transport-probe/evidence/snapshot/` (HEAD, branch, dirty
  status, SHA-256 of every `cmd/` and `internal/` Go file)

It exits non-zero if any of A01-A28 has no passing named test, any package or
test fails, or a required test is skipped. There is no "skip = pass" path.

When preserving an earlier fixed-candidate run, use a separate output directory,
for example `make verify-m0 EVIDENCE=/tmp/homevoice-m0-rerun`. A report generated
from a dirty tree verifies those changes above HEAD, not the commit alone.
Metadata status is collected before writing reports; the snapshot is collected
afterward and may additionally list generated report changes. Neither status is
filtered to conceal authored document or source changes.

Run one acceptance family directly, for example:

```bash
export GOPATH=$PWD/.cache/gopath GOMODCACHE=$PWD/.cache/gopath/pkg/mod \
       GOCACHE=$PWD/.cache/gocache CGO_ENABLED=1 \
       CGO_CFLAGS=-I/opt/homebrew/include CGO_LDFLAGS="-L/opt/homebrew/lib"
go test -race -count=1 -run 'TestA07|TestA08|TestA09' ./internal/m0/session ./internal/m0/udp
```

## Authorized live / hardware entries (BLOCKED in this run)

```bash
make verify-m0-live       # requires ignored local/live.json
make verify-m0-hardware   # requires ignored local/hardware.json
```

Missing authorization/config is reported as **BLOCKED** and the command exits
non-zero (the CLI exits with code 3; make reports a non-zero failure). The
sanitized blocked evidence is written to
`evidence/live-report.md` / `evidence/hardware-report.md`. The entries do not
fabricate results and do not fall back to plaintext, WebSocket or public binds.

Do not create these files with production secrets or real device credentials
inside the repository. Keep them in the ignored `local/` directory (mode 0600)
and only publish sanitized conclusions.

### Live config shape (fill locally; placeholders are not real credentials)

```json
{
  "mode": "live",
  "network": {
    "mqtt_bind": "127.0.0.1:0",
    "udp_bind": "127.0.0.1:0",
    "advertise_host": "127.0.0.1",
    "plaintext_isolated_approved": true
  },
  "devices": [
    {
      "device_id": "live-a",
      "client_id": "<exact firmware client_id>",
      "username": "<per-device username>",
      "password": "<local secret>",
      "publish_topic": "device-server",
      "subscribe_topic": "devices/p2p/<device-identifier>"
    }
  ],
  "audio": {
    "uplink":   {"format":"opus","sample_rate":16000,"channels":1,"frame_duration":60},
    "downlink": {"format":"opus","sample_rate":24000,"channels":1,"frame_duration":60}
  },
  "live": {
    "enabled": true,
    "authorization": "<explicit user authorization reference>",
    "cloud": {
      "api_base": "https://<authorized-provider>/v1",
      "api_key": "<local secret>",
      "model": "<model id>",
      "asr_model": "<asr model id>",
      "tts_model": "<tts model id>"
    },
    "remote_mcp": {
      "url": "https://<authorized-read-only-mcp>/mcp",
      "bearer_token": "<local secret>",
      "transport": "streamable-http",
      "timeout_ms": 5000
    },
    "max_rounds": 1
  },
  "hardware": {},
  "paths": {"evidence_dir": "docs/milestones/M0-transport-probe/evidence", "local_dir": "local"}
}
```

`network.plaintext_isolated_approved=true` is required when MQTT TLS files are
not configured for an authorized mode; it should only be used for an explicitly
authorized isolated loopback/private test. TLS never skips certificate
verification. `live.max_rounds` is bounded to 1..3 and the cloud budget per
round is ASR <= 1, LLM <= 2, TTS <= 1.

### Hardware config shape (fill locally)

```json
{
  "mode": "hardware",
  "network": {
    "mqtt_bind": "127.0.0.1:0",
    "udp_bind": "127.0.0.1:0",
    "advertise_host": "<device-reachable test host>",
    "plaintext_isolated_approved": true
  },
  "devices": [{"device_id":"hw-a","client_id":"<...>","username":"<...>","password":"<local secret>","publish_topic":"device-server","subscribe_topic":"devices/p2p/<...>"}],
  "audio": {"uplink":{"format":"opus","sample_rate":16000,"channels":1,"frame_duration":60},"downlink":{"format":"opus","sample_rate":24000,"channels":1,"frame_duration":60}},
  "live": {},
  "hardware": {
    "enabled": true,
    "authorization": "<explicit device/network/operation authorization reference>",
    "network_id": "<isolated test network alias>",
    "restore_plan": "<how the original device configuration is restored>",
    "acknowledged": ["udp", "notify", "mcp-discovery"],
    "devices": [
      {
        "alias": "test-device-1",
        "board": "<board>",
        "firmware": "<firmware version/digest>",
        "mqtt": {
          "broker_url": "tls://<probe-host>:8883",
          "client_id": "<firmware client_id>",
          "username": "<...>",
          "password": "<local secret>",
          "publish_topic": "device-server",
          "subscribe_topic": "devices/p2p/<device-identifier>",
          "ca_file": "<local CA path>"
        },
        "notify_audio_url": "https://<reachable-test-host>/notice.wav",
        "observer": "<how playback/notify is externally observed>"
      }
    ]
  },
  "paths": {"evidence_dir": "docs/milestones/M0-transport-probe/evidence", "local_dir": "local"}
}
```

**Important limitation**: the authorized live/hardware entry points in this
candidate validate authorization/config and then return BLOCKED; their actual
L01-L03 / H01-H06 workflows are not implemented or validated here. They must
not be recorded as passed until implemented and run with explicit authorization.

## What is implemented (offline)

- `internal/m0/broker`: MQTT 3.1.1 broker on mochi-mqtt with exact synthetic
  credentials, trusted connection identity, per-device exact-topic ACL,
  connection generations, non-retained control/downlink, 64 KiB packet and 5 s
  CONNECT bounds, loopback/authorized-private bind policy, sanitized logging.
- `internal/m0/udp`, `internal/m0/session`: 16-byte header + AES-128-CTR with
  the public vectors, per-packet header as counter IV, HELLO key/SSRC/session
  rotation, template/SSRC/source/peer checks, 64-packet replay window, outbound
  sequence/timestamp bounds, same-socket replies. AES-CTR is **not**
  authenticated; see security limitations in the report.
- `internal/m0/audio`, `internal/m0/playout`: libopus streaming codec with
  residual/tail handling, fake-clock playback scheduler (settle, prebuffer,
  pacing, tail wait, abort, bounded 64-frame queue).
- `internal/m0/notify`: MQTT-only idle notify; busy/offline explicit failures;
  published is never reported as played.
- `internal/m0/devicemcp`, `internal/m0/agent`, `internal/m0/remotemcp`:
  JSON-RPC device MCP client with generation/session correlation and paging;
  bounded model tool loop exposing only `set_current_device_volume(level)`;
  independent read-only remote MCP client over Streamable HTTP/SSE.
- `internal/m0/simulator`, `internal/m0/probe`: simulated firmware/upstream and
  the in-process integrated listen -> ASR -> TTS -> UDP playback path.
- `cmd/m0probe`: selftest, offline report builder, gated live/hardware entries.
- `Makefile`: check-deps, fmt-check, vet, test, verify-m0, verify-m0-live,
  verify-m0-hardware, snapshot, clean.

## Re-run for independent acceptance

1. Fresh workspace checkout/clone of this repository at the fixed candidate
   (branch `feature/m0-transport-probe`; current fixed HEAD is recorded in the
   evidence). Verify `evidence/snapshot/git-head.txt`,
   `git-branch.txt`, `git-status.txt`, and `sha256.txt`.
2. `make check-deps`; confirm Go/libopus versions match the report.
3. `make verify-m0`; confirm exit 0 and that all A01-A28 are **PASS** in
   `evidence/offline-report.md`.
4. `make verify-m0-live` and `make verify-m0-hardware`; confirm non-zero exit
   and BLOCKED evidence (no cloud/device was contacted).
5. Compare the generated evidence with this candidate's evidence and record any
   deviation; do not treat offline success as M0 acceptance.

## Known gaps / open decisions

- Spec package still says "待批准" (pending approval); the user must confirm the
  approval status. This candidate does not self-approve.
- Authorized live/hardware workflows are gated but not implemented/validated.
- Legacy synchronous MCP senders only bound caller waiting; cancellation cannot
  forcibly stop their underlying send goroutine. In-flight side effects can
  remain unknown and must not be automatically retried.
- The conditional T4 HTTPS configuration-distribution endpoint is not
  implemented because the firmware configuration channel is unverified. A
  synthetic loopback HTTPS test-audio server (self-signed test certificate,
  certificate-pinned client, `/notice.wav` WAV tone) exists in
  `internal/m0/simulator` for offline media tests; real firmware notify media
  and HTTPS trust remain unverified.
- Real firmware notify/MCP/TLS/NAT/Opus timing and public-network UDP security
  remain unverified and must be resolved before M1/公网.
- The recorded offline verification grants no Git publication, OTA, deployment,
  cloud-call or real-device-operation authorization.
