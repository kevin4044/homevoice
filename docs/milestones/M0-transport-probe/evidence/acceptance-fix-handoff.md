# M0 independent acceptance → fix handoff

This is a historical defect handoff from the earlier acceptance round. It is
retained for audit history and is superseded by the refreshed offline evidence
for fixed HEAD `4421991`; it does not describe the current candidate result.

**Role of this document:** actionable defect list for a fix agent.  
**Not** a milestone pass/fail by the referee. Do not lower acceptance criteria.

| Field | Value |
|---|---|
| Spec | `M0-v0.1` (`docs/milestones/M0-transport-probe/`) |
| Spec approval in docs | still **待批准** — confirm with referee before treating as frozen; do not change acceptance thresholds |
| Candidate reviewed | `9674ad2a99d0088da76bafa59d41d37c0ac83d7c` on `feature/m0-transport-probe` |
| Reviewer | independent acceptance agent (read-only); defects reproduced in an isolated `/tmp` clone, not by editing the candidate |
| After fix | new fixed commit; re-run `make verify-m0`; independent acceptance must re-verify (especially A05/A18/A22 paths below) |

**Out of scope for this fix round (unless referee expands):** implementing live/hardware L01–H06 workflows; public-network UDP AEAD; real devices/cloud. Refresh stale `evidence/snapshot/` when re-running verify on the fixed commit.

---

## Defect 1 — Blocker / High: MQTT client-id takeover leaves probe stuck; new HELLO dead; old UDP orphaned

**Violates:** A05, R02–R04 (also breaks H03-style reconnect continuity once hardware runs)

### Symptom

1. Device connects, completes HELLO → probe `Online=true`, `Generation=N`, UDP session active.
2. Same `client_id` reconnects (broker **takeover**, not clean DISCONNECT).
3. Broker is online at `Generation=N+1`. Probe still shows `Generation=N`, old UDP session still present.
4. New connection’s HELLO is ignored as “stale”; times out with no reply.

Clean `Close()` + reconnect works (fires `OnOffline`). **Takeover does not.**

### Root cause

- Broker intentionally skips `OnOffline` for superseded connections (`internal/m0/broker/broker.go` ~445–448) so the old disconnect cannot clear the **new** registration.
- Probe only clears `st.online` / drops UDP on `OnOffline` (`probe.go` ~1147–1171).
- `handleHello` rejects when `st.online && st.generation != generation` (`probe.go` ~650–657), and `st.online` is only set on successful HELLO (~713–715), with no `OnOnline` / replace callback.

### Minimal fix direction

1. On MQTT registration / generation bump (takeover), notify the probe that generation `N` was replaced by `N+1` **without** treating it as “device offline” for the new connection. Options:
   - add `OnReplace` / `OnOnline(deviceID, newGen, oldGen)` on the broker; or
   - emit a scoped “superseded” event for the **old** generation only.
2. Probe handler for that event must: `sessions.DropGeneration(deviceID, oldGen)` (or equivalent), clear MCP/session fields for the old generation, set probe online state from **broker** current generation (or clear `st.online` until the new HELLO completes — either is fine if HELLO for `N+1` is accepted).
3. `handleHello` must accept HELLO when `msg.Generation` equals **broker** current generation for that device, even if probe still briefly holds old state; re-check generation after any unlock window before writing `st.*`.

### Required regression tests

- Integration: HELLO → second MQTT connect with same credentials **without** closing the first client → broker gen≥2 → second HELLO succeeds → UDP session id rotated → old SSRC rejected.
- Assert superseded path does **not** clear the new broker registration (keep existing A05 broker guarantees).
- Prefer naming under `TestA05…` in `internal/m0/acceptance` so `make verify-m0` evidence maps to real probe behavior (current `TestA05` is broker-only).

### Repro sketch (already failing on `9674ad2`)

```go
// connect + hello on device A
// NewDevice + Connect same credentials (keep first client open) → wait broker gen>=2
// second SendHello → context deadline exceeded
// probe Snapshot still Generation:1; SessionSnapshot still present
```

---

## Defect 2 — High: Probe stamps current `session_id` on MCP responses (A22 correlation broken at wiring)

**Violates:** A22, R09

### Symptom

After HELLO rotation (same MQTT generation), `devicemcp.New` resets JSON-RPC ids to 1. A late response for the **old** session (e.g. delayed `initialize` id=1) is delivered to the **new** client and can complete the new session’s pending id=1.

Unit tests in `devicemcp` pass because they supply correct/incorrect `SessionID` on the envelope. The **probe undoes** that by overwriting identity.

### Root cause

```go
// probe.go handleMCP ~1080-1098
sessionID = st.sessionID  // CURRENT session, not the session that issued the request
client.HandleMessage(Envelope{ ..., SessionID: sessionID, Payload: m.Payload })
```

### Minimal fix direction

- Do **not** relabel inbound MCP with the current `st.sessionID`.
- Route by matching payload to the client that still owns the pending id **and** whose bound `Identity.SessionID` is the one that sent the request; or keep a generation/session-keyed client map and dispatch only to the matching client.
- Late responses for closed/rotated clients must be ignored (old client already `Close()`d on HELLO).
- Ensure HELLO rotation cancels old pending requests (already partly true) **and** cannot satisfy new pending with old wire responses.

### Required regression tests

- Integration: start `DiscoverDeviceTools` / Initialize with device MCP initialize held → re-HELLO → start second discover → release old initialize response → **second must not succeed from the old response**; old must fail/cancel; new must only complete on a response belonging to the new session.
- Keep existing `TestA22*` unit cases; add probe-level `TestA22…` that fails on `9674ad2`.

### Repro sketch (confirmed)

Block simulator `initialize` → HELLO rotate → second `DiscoverDeviceTools` in flight → unblock old initialize → new discover returns success while old ends canceled.

---

## Defect 3 — Medium: Goodbye clears probe state before validating `session_id`; UDP can be orphaned

**Violates:** A18 / protocol goodbye semantics, R08 session accounting

### Symptom

`goodbye` with **wrong** `session_id`: probe clears `st.sessionID` / MCP immediately; `sessions.Goodbye` returns `ErrSessionMismatch` and keeps UDP. Probe believes audio is gone; SSRC/peer remain until later HELLO/disconnect.

### Root cause

```go
// probe.go handleGoodbye ~1053-1077
// clears st.mcp / st.sessionID first
_ = p.sessions.Goodbye(deviceID, m.SessionID) // mismatch → UDP kept; error ignored
```

Simulator always sends the current HELLO `session_id`, so happy-path A18 passes.

### Minimal fix direction

1. Call `sessions.Goodbye` **before** clearing local MCP/session fields (or only clear after `nil` error).
2. On `ErrSessionMismatch` / `ErrNoSession`: leave probe session mapping intact; log; do not Close MCP spuriously.
3. On success: then nil MCP, cancel tasks, etc. (current success-path behavior).

### Required regression tests

- `PublishRaw` goodbye with non-matching `session_id` → UDP session still present **and** probe `Snapshot.SessionID` still equals the live session (or explicit error state without orphan).
- Matching goodbye still releases UDP and clears probe session (existing A18).

---

## Explicit non-findings (do not “fix” as product bugs)

- Broker identity/ACL and rejecting payload `device_id` — sound (A02–A04).
- UDP AES-CTR no MAC, peer lock, replay window — sound at session/udp layer; do not claim authentication.
- Agent foreign `device_id` / only synthetic volume tool — sound (A24).
- Notify does not require UDP; `Played` always false — sound (A20).
- No device WebSocket fallback; remote MCP refuses HA write tools — sound (A27/A26).

---

## Suggested fix order

1. Defect 1 (takeover / HELLO) — blocks reconnect continuity.
2. Defect 2 (MCP session labeling) — cross-session request completion.
3. Defect 3 (goodbye ordering) — state desync / orphan UDP.

One fix owner; after each (or as one PR), run:

```bash
make verify-m0
# plus the new TestA05 / TestA22 / goodbye mismatch tests under -race
```

Refresh `docs/milestones/M0-transport-probe/evidence/` from the **fixed** commit snapshot. Do not mark L/H as pass. Do not declare M0 accepted — return to independent acceptance + referee.
