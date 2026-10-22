# M0 evidence reconciliation — 2026-10-22

Implementation candidate: `442199166be14df64d43b56d38b01fcddbffcf61`,
branch `feature/m0-transport-probe`, specification `M0-v0.1`.
The worktree was clean at handoff inspection. This reconciliation changes
reports, report tooling and its tests; it remains uncommitted. The local branch
was one commit behind its existing remote-tracking ref; HEAD was not advanced.

## Fixed-candidate evidence

The prior independent run at `/tmp/homevoice-m0-review-new/` recorded this
HEAD and a clean worktree. Its original reports and snapshot are preserved
byte-for-byte in [history/4421991-independent/](history/4421991-independent/).
Every entry in its source SHA-256 manifest was checked against the corresponding
Git blob at `4421991`, rather than the subsequently edited worktree.

The top-level [offline report](offline-report.md), [JSON](offline-report.json)
and [snapshot](snapshot/) now describe that run. The original generated time,
environment, checks and A01–A28 results are unchanged. Corrections are editorial:

- Remove contradictory “uncommitted candidate / no commit authorized” boilerplate
  from a run whose actual metadata and snapshot both say clean `4421991`.
- Carry forward the legacy synchronous sender limitation: caller waiting is
  bounded, but the underlying send goroutine cannot be forcibly stopped.
  Context-aware senders cooperate with cancellation; published/in-flight side
  effects may remain unknown and are not automatically retried.
- Add explicit provenance and original-report hashes to the JSON and Markdown.

This is an import and correction of existing independent evidence, not a fresh
independent acceptance run. The original invocation exited `0`:

```bash
GIT_OPTIONAL_LOCKS=0 make verify-m0 \
  CACHE=/tmp/homevoice-m0-review-new/cache \
  EVIDENCE=/tmp/homevoice-m0-review-new \
  GOMODCACHE=/Users/kevin/Documents/works/homevoice/.cache/gopath/pkg/mod \
  GOPROXY=off
```

The [repaired-candidate review](../homevoice-m0-acceptance-4421991.md) imports
the handoff's updated external acceptance report, retaining F1–F4 findings,
regression results and the clarified legacy sender limitation. Its links and
evidence-status paragraph were updated for this repository. The external file
was read before import and was not edited in this reconciliation.

## Historical evidence

[history/460626c-working-tree/](history/460626c-working-tree/) preserves the
previously committed reports/snapshot exactly as stored by `4421991`; they
record `460626c0105c05389fe99eb57f68b3365ae2a81f` and a dirty tree. They are
historical execution records, not proof of the later fixed clean candidate.

The [4152a4c failure report](../homevoice-m0-acceptance-4152a4c.md) retains
its original body, with an added historical banner pointing to the repaired
review. The earlier [9674ad2 defect handoff](acceptance-fix-handoff.md) is also
marked historical. No failure findings were silently overwritten.
Raw report/snapshot checksums are in [history/sha256.txt](history/sha256.txt).

## Report-tool changes and verification

The report generator now derives clean/dirty/unknown wording from recorded
worktree status, and preserves the legacy sender warning. Git status includes
authored source, documents and evidence. The snapshot message no longer asserts
that every candidate is uncommitted or that publication permission was absent.
Regression coverage checks clean, dirty and unknown provenance in both output
formats and ensures the sender limitation remains present.

The separate verification output is in
[report-tool-verification/](report-tool-verification/). It records a dirty
worktree above `4421991`; its source hashes include the modified report code
and regression test. It is developer verification of these uncommitted changes,
not an independent review of a new fixed commit.

```bash
GIT_OPTIONAL_LOCKS=0 make verify-m0 \
  CACHE=/tmp/homevoice-m0-reconcile/cache \
  EVIDENCE=docs/milestones/M0-transport-probe/evidence/report-tool-verification \
  GOMODCACHE=/Users/kevin/Documents/works/homevoice/.cache/gopath/pkg/mod \
  GOPROXY=off
```

Result: exit `0` in the approved local test execution environment. Formatting,
vet, race tests and A01–A28 all passed: 19 package pass events and 338 passing
test/subtest events, including clean/dirty/unknown provenance regressions.
The final report's source manifest matches the current Go sources. This result
paragraph was finalized afterward; it is not part of a new fixed-commit review.
The initial sandboxed attempt could not bind loopback TCP/UDP listeners
(`operation not permitted`); that attempt was an environment block, not a
product regression. No public endpoints, cloud providers, real devices or
Home Assistant were contacted by the successful offline run.

## Remaining gates

L01–L03 and H01–H06 were not executed. The live/hardware entries validate config
and return BLOCKED even with configuration; actual workflows still require
implementation and separate approval of scope, endpoints, budgets, firmware,
isolated network, device operations and restoration plan.

UDP AES-CTR provides no authenticity. Peer locking, SSRC checks, replay filtering
and Opus decoding do not repair that limitation. An explicit security solution
and approval are required before public deployment. Overall M0 remains BLOCKED;
the designated referee owns the final decision. No commit, push, merge,
deployment, paid-service call or real-device/HA operation was performed here.
