# M0 evidence index

Current independently reviewed implementation: `442199166be14df64d43b56d38b01fcddbffcf61`,
specification `M0-v0.1`. Offline verification passed; overall M0 remains BLOCKED.
Evidence reconciliation does not change specification approval or acceptance thresholds.

- [Reconciliation and provenance](reconciliation.md): explains historical
  runs, editorial corrections, source hashes and separate report-tool checks.
- [Fixed-candidate acceptance report](../homevoice-m0-acceptance-4421991.md):
  imported prior independent review of F1–F4, including the legacy sender limitation.
- [Offline report](offline-report.md) / [JSON](offline-report.json): corrected
  copy of the independent run at `4421991`, A01–A28 PASS. Original execution
  time and result data are retained; this is not a new execution.
- [Snapshot](snapshot/): original independent run's HEAD, branch, complete
  worktree status and Go/JSON source hashes. The empty status describes that
  earlier run, not the worktree after the current reconciliation edits.
- [Original independent output](history/4421991-independent/): unchanged raw
  reports and snapshot, including the old contradictory report boilerplate.
- [Previous committed output](history/460626c-working-tree/): unchanged reports
  and snapshot as stored by `4421991`, recording `460626c` and a dirty tree.
- [Historical failed acceptance](../homevoice-m0-acceptance-4152a4c.md): original
  failure body retained; superseded for the repaired candidate by the `4421991`
  review. F1–F4 fixes are in `f5964a3`, `5f7d205`/`816018e`,
  `530892c`/`460626c`, and `03e6f02`.
- [Earlier defect handoff](acceptance-fix-handoff.md): historical review of
  `9674ad2`; its defects were addressed by `881064f`, `7f9dcba`, `94e710a`.
- [Archive checksums](history/sha256.txt): hashes of both raw report/snapshot sets.
- [Report-tool verification](report-tool-verification/offline-report.md):
  separate test run on the uncommitted reconciliation changes above `4421991`;
  it does not replace independent fixed-commit acceptance.
- [Usage](usage.md) and [compatibility findings](compatibility-offline.md):
  commands, dependencies, simulated scope and unresolved real-world behavior.
- [Live](live-report.md) / [hardware](hardware-report.md): retained BLOCKED
  reports. Actual L01–L03/H01–H06 workflows are unimplemented even with config.

The specification still records “待批准”; final acceptance belongs to the
designated referee. Cloud/device execution needs separate authorization and
resources. UDP AES-CTR remains unauthenticated; no public deployment is approved.
