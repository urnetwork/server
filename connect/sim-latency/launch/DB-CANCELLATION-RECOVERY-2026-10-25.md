# Canceled dependency recovery: release correction

## Failure and launch consequence

The [first formal Stage A cohort](../baseline/prelaunch-same-seed-r9-2026-10-25/failed-cohort-01/README.md)
stopped after nine controls because control 1 logged `unexpected_recovery`.
No candidate build, candidate measurement or later cohort started. The failed
position is preserved and cannot be replaced or reclassified as successful.

The announce-time peer-profile lookup reached `dbWithPool` while its caller
was canceled. The stop helper retained the legacy database done sentinel,
original protocol/socket timeout and `context.Canceled` in a joined error.
`IsDoneError` required every joined cause to be independently benign, so the
physical socket timeout made this ordinary stopped operation unexpected.
Preserving physical evidence was correct; losing the stop classification at
the recovery boundary was not.

## Narrow correction

`server/db_stop_cause.go` marks a stopped dependency only when one captured
context observation is exactly `context.Canceled`, bounded cause inspection
is complete, a concrete `net.OpError` is present, and every original leaf is
the physical `os.ErrDeadlineExceeded` or cancellation (with at least one
physical deadline). It does not invoke arbitrary `Is`, `As` or `Timeout`
methods to grant this classification.

An internal wrapper retains the entire original diagnostic graph and text.
`server/trace.go` recognizes this verified stop before ordinary unwrapping.
An independent failure joined outside the marker is still unexpected.
Deadlines, connection resets, database refusals, application invariants,
unknown physical causes and malformed/incomplete graphs are not marked.

No SQL, retry permission, callback replay decision, transaction behavior,
scoring threshold, stability gate, workload, replicate count or timeout bound
changes. Redis and owned-transaction paths use the same stop helper; the audit
includes those callers rather than adding a separate permissive classifier.

## Deterministic qualification

Independent execution uses actual Go 1.27.3/local, management CPUs 20,22,
two Go workers and synthetic disposable services. Sol 6.1 low executes tests;
Sol 6.1 max diagnoses, fixes and reviews.

The original three-test pre-fix proof on evaluated server
`0d58d0d91b069c62bcfb39cd72166c26925b786a` deterministically failed
`TestDbCanceledSocketStopIsExpectedRecovery`, while its deadline, unrelated
failure and existing explicit-shutdown controls passed. Exit 1, seven seconds,
cleanup 0. Full log SHA-256:
`0d41905451145e8672dcdc2af578c502865eeff4fa595d83e11ae9b5db3109e5`.

`server/db_stop_recovery_test.go` adds eleven top-level regressions/controls:
raw socket cancellation; deadline; unrelated failure; mixed joins; a later
independent sibling; a live caller; the real protocol writer; bounded malformed
graphs; real pool/callback/outer recovery; Redis preflight variants and reset
controls; and a deterministic cancellation-after-observation transition.
The last case prevents a draft double-read race from granting classification
whose retained evidence lacked cancellation. No scheduler luck or sleep is
the primary proof.

Expanded old-body proof compiled and produced five deterministic failures:
raw canceled socket, wrapped stop classification, real protocol writer,
database callback recovery and Redis callback recovery. Negative controls
passed; cleanup 0. Full log SHA-256:
`8eccb88958d76489e90aa36125604cbf44c39a41edd14fea71617aa5ad26e19e`.

The separate double-read draft failed the cancellation observation regression
with exactly two reads. Cleanup 0; full log SHA-256:
`ceed69f8b9f56599e8560fe2b2933b2ff424ec6ae3bcda1fd71bd81cf148af5e`.

**Final corrected race smoke passed all 17 selected top-level tests**, including
all eleven new tests and six existing controls. Test exit 0, 115 seconds whole
invocation (package 3.870 seconds), cleanup 0. Full log SHA-256:
`f4670ce7fef8cd636c9bf9e1f299958da8d0e1ec918fa34ff5345148bc11268b`.
Only the original three containers and four networks remained afterwards.
This is evaluated-source correction smoke, not full validation of current
main, a new evaluator image, formal calibration or live launch readiness.

Exact corrected source SHA-256:

| File relative to mono root | SHA-256 |
| --- | --- |
| `server/db_stop_cause.go` | `06f1afc9c2f2ccc8a7dbfadd7469a1229e7e80a3859c06445ac4cd233b064fbb` |
| `server/trace.go` | `28834aa210052247ec963baafa9d8e2c0595f464bc9cd64eb7b5cc21e4bb3dfb` |
| `server/db_stop_recovery_test.go` | `5a1a8ed87e368b38cc2dc47e54fab109ae25991a8548031da4b9d79ea25118b8` |

## Test setup failures retained separately

- Current server main plus frozen Connect could not compile root external
  tests using a newer placement API. Log SHA-256:
  `2779f00c0f1b4ae97aceac74ea7dbccad8eb3b26781006f2305b4ce3da83118a`.
  This was not a behavioral regression result. The evaluated-source proof
  correctly uses evaluated server `0d58d0d9` and its matching dependency graph.
- The first evaluated-source invocation then required the expected sibling
  `sn` path for its exact private suite generator. The private checkout layout
  was corrected using the frozen dependency tree, without disabling the guard.
- On clean committed successor `c1f9dda60`, the repeated targeted race scope
  passed all 17 tests (116 seconds, package 3.863 seconds); log SHA-256:
  `e359961b19ee55fe1f1ff6f13c4cefa035666d2c577ab25115cd631f9ad475fa`.
  The first full simulator suite then stopped with 302 passes and 38 failures:
  the isolated root launcher omitted the global Git identity explicitly
  required by existing synthetic Git fixtures. No source defect is inferred
  from that setup failure. Exit 1, 315 seconds, cleanup 0, clean source and
  original-only resources; log SHA-256:
  `e72530fc8449ed90a1295229818b726e93a3a479377cf206a1aed072054970fc`.
  A private synthetic `GIT_CONFIG_GLOBAL` file corrects the launcher context;
  no host-global settings, repository-local identity or author override is
  changed. The existing identity guard remains enabled. The next sequence
  starts at full suite 1, not suite 2; this failed invocation remains retained.

The private evidence root is
`temp/db-stop-prefix-20261025.ZUKuHMwu/`. No operator checkout, frozen evaluated
source, original failed evidence or main database was modified by test setup.
The clean-commit targeted and initial full-suite setup evidence is separately
retained under `temp/canceled-stop-full-three-20261025.7M6m9d9r/`.

## Next release boundary

After smoke acceptance, the correction was committed, pulled (already current)
and pushed to server main as
`e2b87b8ae0fa26fb79f4ab5219e94c641be2268b` before larger validation.
In a fresh private checkout, only that three-file correction was cherry-picked
onto frozen `0d58d0d9`. Clean successor
`c1f9dda603d6279666fca5d92b99c5663306c5f7` was fast-forward pushed to
`origin/sim-latency`; all three source hashes still match accepted smoke.
The other nine evaluated repository pins are unchanged. Main control-program
changes are not included in this evaluated-source successor.

The clean-commit targeted root smoke and **three consecutive full simulator
suites passed**. Every suite ran all 340 top-level tests (one non-race lifecycle
and 339 race-enabled tests); whole walls were 117/116/116 seconds, race-package
times 109.230/108.563/108.777 seconds. All used exact Go 1.27.3 and clean `c1f9dda60`
with the nine unchanged frozen dependencies. The private synthetic global Git
identity was verified before invocation; no host-global file was changed.

| Suite | Full log SHA-256 |
| --- | --- |
| 1 | `916a86ba79f3c1b6b8c92d399c7bbcdabd676ba3d31b5e03de5b38ecce5e4dfd` |
| 2 | `d8147e00c0267dd15b647b21a3585d55214da09ce3326574a1a6df26a8df4a8e` |
| 3 | `680f6df9a327045a720b3ec19c314acb9345092662760782f278dae70eadad94` |

Evidence root: `temp/canceled-stop-three-corrected-20261025.3wbLy9aM/`.
Its launch log SHA-256 is
`588b3dff1bcd2c7bbf7483f29afa69c30214471747e2319eb545f0a64fda71e0`.
Final cleanup was 0; source remained clean, only the original three containers
and four networks remained, and the canonical singleton lock was free.

The proposed source ledger binds canonical ten-repository digest
`5e00bf5ee7b4708be94ac1ebc6daf5c024f4afe467ca30c35024f65f96b6c90e`.
The previous manifest is preserved byte-for-byte (including its final blank
line) at `config/history/sim-latency-2026-10-24-232720.yml`, SHA-256
`156aee96af328e2b4d4d838f2de2eecb0204650b8400504a0eca773b9838de2f`.
The retained blank line can produce a Git whitespace warning; changing it would
invalidate exact preservation. This is not an executable-code formatting change.

The corrected evaluated release still needs its own exact image,
Go 1.27.3 image, cold evaluation and predeclared fresh calibration. The previous
release's controls are historical evidence, not measurements of the correction.
Main remains under the user-confirmed maintenance hold; no production activation
or main DB migration follows this local work.
