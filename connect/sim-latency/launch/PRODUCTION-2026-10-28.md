# Production launch — October 28, 2026, 00:00 UTC

## Current preparation pointer — October 25, 01:30 UTC

The latest source-ledger publication is config main
`949e08c8c5412206e06709e36f8bfbe1e182aef3`, server
`0d58d0d91b069c62bcfb39cd72166c26925b786a`, canonical source lock
`37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`.
All other nine repository pins are unchanged. Both the main runtime checkout
at `d16a6adbc` and the evaluated-source checkout passed three consecutive full
suites with Go 1.27.3. The later host-only preflight correction passed 63 focused
race executions and actual ten-repository remote verification in 9 seconds;
see [SOURCE-PREFLIGHT-2026-10-24.md](SOURCE-PREFLIGHT-2026-10-24.md).

Current main `ec02f4e5b` subsequently passed three consecutive complete
sim-latency suites with actual Go 1.27.3: 446 top-level passes per invocation,
including 445 race tests, disposable local services and verified cleanup.
Exact source, commands, durations and log hashes are recorded in
[CONTROL-VALIDATION-2026-10-25.md](CONTROL-VALIDATION-2026-10-25.md).

Manifest file SHA-256:
`156aee96af328e2b4d4d838f2de2eecb0204650b8400504a0eca773b9838de2f`.
Preparation-descriptor SHA-256:
`a393a426fb57b14df8aa22e55027c690751bbd3dfd8a1261b387bbb040f16743`.
The exact prior ledger is archived at
`config/history/sim-latency-2026-10-24-213002.yml` with SHA-256
`838b132d2c19112e50fefaabfabda81e22127638e8ba281f0c0141ab82d30612`.

A fresh evaluator build completed under mono-root-relative
`temp/ready-snapshot-evaluator-build-20261024.MCorfDyJ`, using the clean main
`9a3b7970a` builder scripts and immutable corrected source tool, with fresh exact
checkouts of all ten frozen repositories in the build context. Its local tag is
`urnetwork/sim-latency-evaluator-base:ready-snapshot-0d58d0d9`, image ID
`sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`.
Actual compiler/recovery/source gates and independent local image lifecycle,
build-isolation and host adversarial-cleanup phases passed. The first fresh
15,000-provider impairment-on point failed after 1,090 seconds at matchmaking
probe 2, followed by a shard-0 TERM timeout. Its complete retained artifacts
are in `baseline/prelaunch-ready-snapshot-frontier-2026-10-25/`. The startup
barrier held and provide-key duplicate errors were absent, but this remains an
incomplete capacity point, not a baseline. A fresh 7,200-provider on point also
failed after 606 seconds at the initial matchmaking probe, with zero measured
rows and clean teardown. Both outcomes are retained. The next 1,800-provider on
point completed in 525 seconds with an authenticated marker, 2,441 measured
rows, two failed rows, no SQL errors and clean teardown. It is one exploratory
completion, not a qualified scale or epoch baseline. The 3,600-provider on point
also completed (568 seconds, 2,482 measured rows, two failed rows, authenticated
marker and clean teardown), with retained SQL serialization/cancellation events.
Neither direct-controller point has a final resource report. Full gate
qualification, cold R9 timing and impairment-off frontier work remain pending.
Further frontier/diagnostic work must preserve the correctness gates.
Full production qualification is not complete. See
[ARTIFACTS-READY-SNAPSHOT-2026-10-25.md](ARTIFACTS-READY-SNAPSHOT-2026-10-25.md).
No new performance result is claimed. Neither
production nor a new staging round is activated. Final scale/margin, fresh
calibration, hidden epoch baseline, genuine provider-backed qualification and
live production readiness remain incomplete; 16.1% is still a provisional floor.

## Historical preparation journal — superseded status entries

The dated entries below preserve the investigation and implementation sequence.
Statements that a component was not yet implemented describe their entry's
time, not the current launch checklist. Current remaining gates are in the
preparation pointer above and its linked exact-artifact records.

### Earlier preparation pointer — October 24, 23:02 UTC

Public admission remains fixed at **2026-10-28 00:00 UTC**. This file also
retains the chronological preparation journal below; its earlier checkpoint
tables and measurements must not be interpreted as the latest qualified release.

The last source-ledger publication remains server
`6261c15454adec5a00680e4c3332f628a0aed1e7` on `sim-latency`, with the other nine
repository pins unchanged. Config main
`cf8b9b584c93137a44a618bd9adf589f8b23749f` publishes canonical source lock
`52b6d63a32ba21cb011d122de80ea8d51d82cb164a165d906223fb07c6b7b694`.
Go **1.27.3** is mandatory. The exact test, archival, compiler and publication
evidence is in [GO-AND-PROVIDE-2026-10-24.md](GO-AND-PROVIDE-2026-10-24.md).

The new evaluator image is built and its independent basic identity checks pass,
but it is not fully qualified. Its exact evaluator/worker identities and retained
qualification attempts are in
[ARTIFACTS-GO1273-2026-10-24.md](ARTIFACTS-GO1273-2026-10-24.md).
Its synthetic-local end-to-end smoke and malicious build-isolation checks have
passed independently. A separate host CPU/72 GiB memory-bomb test passed, with
575 ms management-core cleanup; that result is not image-bound host qualification.
A fresh 15,000-provider impairment-on public-seed frontier point failed at
matchmaking probe 6; it is not a baseline or accepted scale. The resulting
transaction-snapshot and shard-construction corrections have passing focused
regressions and still require full validation, a coherent replacement source
ledger/image and fresh measurements. See
[STARTUP-AND-SNAPSHOT-2026-10-24.md](STARTUP-AND-SNAPSHOT-2026-10-24.md).
The tested source-branch tip containing both fixes is
`0d58d0d91b069c62bcfb39cd72166c26925b786a`; it is not yet the ledger's new
checkpoint or a qualified replacement evaluator image.
No previous checkpoint's measurements are being reused as the new baseline.
Current source publication is
explicitly `prepared_not_activated`; final scale/margin, fresh calibration,
hidden epoch baseline and production runtime activation remain unproven.
The current source ledger is `config/main/sim-latency.yml`; old ledgers and image
identities remain immutable historical evidence. Do not reuse their qualification
or attach their measurements to the new source lock.

Preparation started October 24. The public opening is fixed; preparation and
qualification start immediately. Current upstream development heads replace the
previous plan to wait for a Monday source freeze. Do not admit public submissions
before the opening or reinterpret an existing staging round as production.

The inactive root-owned October 24 host-control installation, exact artifact
identities, test evidence and remaining activation gates are recorded in
[HOST-CONTROL-2026-10-24.md](HOST-CONTROL-2026-10-24.md). It does not change the
frozen evaluated source below or assert live production readiness.

**Frontier correction (October 24, 18:01 UTC):** the frozen executable's
`--no-impair` mode was not propagated to fleet subprocesses. All four-shard
requested-off points below actually had impaired children, so their on/off
interpretations and capacity eligibility are withdrawn. Raw runs remain retained;
the frontier index explicitly records the invalid mode. The impairment-on R9
dataset is unaffected. A fixed, authenticated executable and true off trials are
required before final scale selection; no old sidecar is rewritten to fake them.

## Candidate source checkpoint

Fetched October 24 from the development branches below. These are candidate
identities, not a claim of completed qualification, branch publication, production
activation, or a scoring baseline. Local operator checkouts remain untouched;
builds and evaluations use fresh temporary checkouts.

| Repository | Development branch | Candidate commit |
| --- | --- | --- |
| server | main | 67c87445abe6725b25b3df2a13af7eb99cd4f8ed |
| connect | main | e90d59eb404f973dba6e9d0689ecd94e2718c10e |
| sdk | main | d3824aa98c02e3c16ff7abcfe7a7088c8a470cdb |
| proxy | main | af1d364fe34b807c31936642bff7d3fd849ab2a4 |
| glog | master | 1c4954b017d9e8140a4e7020f914eb27410a41c6 |
| goidenticons | main | 39ccec1b69db3a6f67e7346d6f44863499e6241d |
| userwireguard | master | f9a92f0e5f5577a80d358065c4d39ca21f971a1c |
| sn | main | 6e686aabd53b7c45b154051fd268138988224eba |
| warp | main | 8dfa405ec380a60307952778921f05bba1c59a33 |
| gvisor | go | 57c9e137f7407ad6cd41dcca1ba304a446e09c79 |

Three dependency forks have no `main`: their actual upstream development branches
are listed explicitly instead of inventing a branch or silently retaining an old
staging pin. `operator-proxy` is not in the evaluation graph. The tracked SCTP
module is part of the connect commit, not a separate repository.

## Live capacity sweep — October 24, 12:50 UTC

The isolated four-exchange-host frontier is running serially on sille, with
200 clients, 80 arrivals/minute and impairment on/off at each provider scale.
The staging supervisor is inactive and no build/test load is scheduled during
measurement. The initial 1,800-provider on/off and 7,200-provider on attempts
failed before simulation because the private harness omitted `EVALUATION_JOB_ID`
from the container environment. Their disposable stacks cleaned up; these are
harness failures, not capacity results or baseline samples. The environment
handoff is corrected for subsequent attempts. Repeat those missing points
separately after this batch, retaining the failed logs rather than overwriting
them. Sweep evidence is currently private under
`temp/production-frontier-20261024.63AtMXS5`; archive completed results before
selecting scale. This sweep does not replace nine-run calibration.

The first measured point (7,200 providers, impairment off) warmed all 200
clients and completed measurement, but exited nonzero after 691 seconds:
`unclean_teardown`, fleet shard 0 required KILL after TERM. Its sidecar marks
the run incomplete. Retain its CSV/metadata/logs for shutdown diagnosis; do not
count its latency metrics as a usable baseline or infer a capacity limit until
the lifecycle failure is understood. The serial sweep has advanced to the
15,000-provider impairment-on point. That point also warmed all 200 clients but
failed during measurement with `matchmaking_probe_failed`; its fleet then
drained cleanly without a diagnostic signal. Both points, including exact CSVs,
sidecars, compressed inputs/logs and pre-measurement harness failures, are now
preserved under `baseline/prelaunch-frontier-2026-10-24/` and remain explicitly
ineligible for baseline use. The original batch and waiting recovery controller
were stopped after the point's terminal exit and verified container cleanup.
Resume the missing lower-scale points after the current test window; do not
start larger points or declare capacity from either incomplete attempt.

The lower-scale recovery finished: 1,800 providers passed impairment on and off
in 461 and 435 seconds with clean teardown. The 7,200-provider on simulation
also completed and drained cleanly, but its host wrapper returned 127 after a
live controller-script edit displaced Bash's unread tail. Its measured sidecar
and failing controller outcome are both preserved, not relabeled as a clean
whole-pipeline pass. All five measured attempts are now versioned in the frontier
archive; only the two 1,800-provider trials are usable capacity points, and none
is an official baseline sample.

A separate immutable-controller 7,200-provider impairment-off attempt completed
cleanly at about 17:33 UTC: all 200 clients warmed, all four shards drained,
controller exit 0, no diagnostic signal and no leftover trial containers. Its
complete artifacts are retained in the frontier archive as
`p7200-off-handoff-fix`; the earlier failed run is unchanged. This adds capacity
evidence but neither explains the intermittent teardown failure nor establishes
nine-run stability, a calibrated margin, final scale or a hidden-seed baseline.

The next public-seed nine-run set uses fresh disposable PostgreSQL/Redis per
replicate and the same 1,800-provider/four-exchange-instance workload. It stops
on any failed replicate instead of silently resampling. The root-owned,
read-only `r9-controller-v1` bundle fixes the mutable-controller failure and
checks script/input digests before and after execution. The source-case script
digest is `35f5d0148e25d85a0075a6067400619ef7b4fc2114aa9f2e1b255582642ebc49`;
measurement script digest is
`1cade50cd96f2c7529c89b3afb8a60a47567a564bdd7f3aa84dadf54d464a3cd`.
This remains exploratory calibration, not final scale selection, hidden-seed
commitment, an official scoring baseline or a finalized margin.

Deterministic FIFO-barrier Go regressions reproduce the live-shell unread-tail
failure and verify the immutable snapshot control. A composed real-Git-tree
regression also confirms that schema-2 preflight already selects a source
generation without inventing a future-round mapping; no production fix was
needed for that suspected issue. These and archive metadata checks passed
three race repetitions (19.080 seconds). All earlier raw `v1` evidence remains
unchanged. No main control-plane writes occurred during the maintenance hold.

The public API still returns `503 source_not_ready`, and sille's configured
MinIO endpoint `172.28.208.177:23900` refuses TCP connections. No main database
write or production activation has been attempted while maintenance confirmation
is pending.

### Calibration and preparation checkpoint — October 24, 15:01 UTC

The original nine-run coordinator is still live. Replicates 1–4 each completed
with exit 0, taking 442, 452, 441 and 436 seconds respectively; replicate 5 is
running. No Go build or test work overlaps these timed measurements. This is
still public-seed exploratory calibration, not the official baseline.

Draft host-preparation changes allow a root-only, protected alternate host
configuration without replacing the active host document. The prospective
profile explicitly cannot assert runtime qualification or inherit another
image's readiness evidence. Deterministic tests cover protected-path ancestry,
marker authentication before evidence reads, root-only argument authorization,
changed hardware/image/boot identity and preservation of failed runtime facts.
These drafts have formatting/shell syntax checks only; execute their Go tests
after timed calibration ends, before committing or installing them.

Read-only live checks still return API `503 source_not_ready` and connection
refused from sille's MinIO endpoint `172.28.208.177:23900`. No main writes or
activation have occurred during the maintenance hold.

## Execution checklist

### Qualifier and four-instance evidence — October 24, 15:42 UTC

All nine fresh-service runs completed cleanly, with no replacements and no
overlapping builds/tests. Their failure-charged raw p95 median is 17,597.57 ms,
sample mean 17,967.77 ms, sample SD 1,753.81 ms and CV 9.76%. Twenty of 22,112
measured requests failed. The historical 16.1% margin would imply an illustrative
14,764.36 ms practical threshold; neither scale nor margin is frozen here.
An equal-variance nine-per-side Welch approximation gives an 8.03% mean-difference
boundary, not an actual candidate significance decision.

The original enclosing coordinator returned 1 only after all nine runs: its
optional descriptive-analysis command omitted an explicit source-config path.
That failure remains retained. Separate offline, read-only analysis in the same
image succeeded with its actual embedded source configuration/lock; no run was
repeated. The failure-charged report SHA-256 is
`9d2c8c4a20650cc97cb742b5415c726c4230462a24db10d818da783068074796`;
the separate success-only descriptive report is
`34b61461c76249b4cf73cb5379f1d64f748c40164cd364bb81bef1ef451fdffb`.

The independent build qualifier now has a real `source-qualify-build` entry point
joining the prescribed low-model test client, actual Docker containment
inspection, terminal/cleanup observations and a root-checked prospective host
fingerprint. The fingerprint explicitly cannot assert future runtime readiness.
The exact-image fenced preparation and API/worker acknowledgments remain gates.

Focused regression tests passed three race repetitions (8.597 seconds; log
`4653507d66623fd813cb3d861483ce618807404856edfe8c8d426cdfab30761d`).
The real-container offline probe passed all 33 correctness tests and replay;
its retained log is
`ecf344957aba22e2a8d5868787054db0d3f7823485a425b2abf74902c8568fb1`.
Provider responses in that probe were mocked, not genuine model provenance.
Three full simulator-suite invocations passed consecutively in 143, 123 and
124 seconds, with owned disposable services cleaned up. Logs:

- `6f323306250d5f291e4067168ead191acbcd5e2e0047ee495154d3a21cc712a3`
- `4e29df7f228e6214ac59dcce656a5a653e55bae1a62ae3631a9cdc7ac9478b01`
- `98a2123d15da4d9b35cca28cd7de667fa570e21380fe746dd20ea84e835eca78`

This closes implementation/test work for the qualifier coordinator, not its
installation, genuine provider access, production host/artifact readiness,
reference calibration or season activation. No main control-plane writes were
performed during the maintenance hold.

The complete four-instance dataset is now versioned at
[`../baseline/prelaunch-four-exchange-2026-10-24/`](../baseline/prelaunch-four-exchange-2026-10-24/README.md),
separate from the earlier one-instance evidence. Its raw CSVs, sidecars, final
markers, compressed full logs, controller snapshots and both analysis outcomes
are retained. New dataset/source/topology/marker/failed-row regression checks
passed three race repetitions (16.415 seconds); full archive verification covers
2,826 files, with every historical `v1` checksum unchanged.

The tested qualifier phase was committed as `777388323`, merged with upstream
main in `01473cd8f` and pushed. The post-merge focused race/lifecycle regressions
passed three repetitions (8.579 seconds). The measured checkpoint remains
`67c87445abe6725b25b3df2a13af7eb99cd4f8ed`, not advancing control main.
Read-only checks at 15:50 UTC still show API `503 source_not_ready` and refused
TCP connections to sille's MinIO endpoint. Production has not been activated.

### Reviewed-image host handoff — October 24, 16:43 UTC

The automatic promotion path now installs the reviewed image's expected host
fingerprint inside the existing fenced qualification operation, after genuine
API acknowledgment and acquisition of the singleton evaluation slot. Previously
the qualifier required the new image pin while the host document still held
the incumbent pin. The new step authenticates the review-indexed reservation,
matching broker/owner receipts, retained tests/containment and same-boot host
profile; it reproduces the actual fingerprint through the pinned alternate-config
checker before changing the active document. Only image/fingerprint pins change.
The old markers do not become readiness for a new image or round: real runtime
qualification, protected artifact retention and worker acknowledgment still run.

The root qualification inventory now needs `reviewed_build_state_directory`,
matching the build broker's private root-owned state. It is required for new
elected-winner-policy deployment, not accepted from submissions. Existing exact
root provisioning and carry-forward remain supported. Shared evidence types keep
producer/consumer wire shapes identical instead of copying private CLI schemas.
Restart tests also cover an interrupted exclusive publication leaving a private
temporary hard link: trusted root records remain readable, while the untrusted
submission-artifact hard-link restriction remains intact.

Focused producer/consumer regressions passed three race repetitions. The expanded
controller preparation suite, including actual disposable-DB qualification-slot
tests, passed three race repetitions (24.374 seconds; log
`a89ffc4155ee63c11add6521d18d8fba8ec8259bf67f1eb7b13ab447c4905495`).
An earlier direct invocation correctly failed environment preflight before DB
tests because it omitted the standard local test environment; that log remains
retained. The corrected harness sources `test-env.sh` explicitly.
Three consecutive full simulator-suite invocations then passed in 195, 124 and
124 seconds, with owned fixture cleanup. Logs:

- `1175349797a91059e0f92c7621fe81431114f404d2c83ab75e76d99d452d1ddf`
- `637f07171946d47466e0b6cb8a2b5e48b981e4f209a3d6ecf9451355f12dfa75`
- `3bc9e9d83e39d989b64ad7555a5b1b291f6d88a7fb27a7037126fee02068fb2f`

These are implementation/test results, not a live main-DB preparation proof.
No main writes, active host-document replacement or production activation occurred
during maintenance. The frozen measured checkpoint and nine raw runs are unchanged.

### Restart/review-build validation — October 24, 13:38 UTC

The build broker now durably binds one review to one image/owner and original
deadline, reuses authenticated completed artifacts, refuses unfinished pipeline
reservations, and prevents concurrent controllers with a process-owned lock.
Reviewed Git commits use the authenticated review timestamp and fixed identities
so re-materialization does not change the source lock. Independent test handling
retains the original plan, raw provider streams before validation, execution
observations and final evidence. Unfinished transports pause for reconciliation;
they do not silently sample another judgment. These are control changes, not
changes to the frozen evaluated checkpoint.

Focused race regressions passed three repetitions (19.144 seconds; log SHA-256
`38136da7dbf56dd6382a4955d398100f86295c1eecd2e9160916f5a309b8cfd0`).
The full real-container offline probe passed all 33 prescribed correctness tests
and journal replay with no additional candidate test execution. Its log digest is
`539a4e971e2da03ef986ca7d4e31ee6e2dfd8aafed79b055c7b5c6bb9425b553`;
test-log digest is
`927bc47d32f914dbe5aab028c114dc13cc076907945b38e8cb26231af00a0908`.
Provider streams in that probe were **mocked**, not genuine model provenance.
The temporary image and containers were removed.

Three consecutive full `tests.sh` passes, each including the six-epoch lifecycle
and race suite, completed in 115.213, 110.268 and 109.991 seconds. Retained logs:

- `bdce61da90f2aa3449259fd80b41d2b86d1bd3f5acd285160e56e5ea6e77c878`
- `69b877be9233699929a9df3f28fc84aa6c5cd54d0ca7fdae2737c5bacb42eb17`
- `8c8821df6753399b7eda70e9ea6be8702b63077fd0b4579efdc33eb461fe4917`

The separately owned disposable database/Redis fixtures were cleaned up. Archive
verification passed all 2,763 files; historical `v1` bytes and their checksums
remain unchanged. Real model access, installed qualifier integration, host and
artifact readiness, supported-scale calibration and production activation are
still outstanding; these test passes do not close those launch gates.

- [x] Fetch development heads and inspect operator worktrees without modifying them.
- [x] Compile the candidate graph and pass the simulator harness against its pinned dependencies.
- [x] Audit transitive repository coverage against that graph.
- [x] Publish the preparation source lock and competition branches without
  overwriting historical epoch records or force-pushing an existing branch.
- [ ] Build and qualify the exact production evaluator; retain authenticated
  containment, kill/cleanup, artifact, and host-readiness evidence.
- [ ] Collect the fresh production calibration and freeze its supported threshold;
  retain the configured nine-run shared scoring baseline for each official epoch.
- [ ] Deploy the additive winner-review migration and compatible API/worker/manager
  after publishing their changes. Qualify the real isolated Sol 6.1 max review
  broker and build/preparation integrations; test rejection, replacement,
  no-winner carry-forward, restart recovery, and successful next-round activation.
- [ ] Configure a separate production scope: six weekly admission windows, $20
  admission fee, production significance rules, live settlement, and the adopted
  elected-winner review policy. Preserve staging history and scoped credentials.
- [ ] Bind the first admission window to `2026-10-28T00:00:00Z`; verify restart
  cannot open early. Subsequent windows start after scoring/review/preparation.
- [ ] Verify MinIO capacity/retention/backup, production credential isolation,
  Grafana heartbeat and blocked-progression delivery to support@ur.xyz.
- [ ] Obtain Macrocosmos production adapter/settlement acceptance and publish the
  corrected participant instructions and duplicate-fee policy.
- [ ] Complete production preflight and leave the supervised system ready and
  waiting for the fixed opening.

Run independent preparation concurrently, but serialize scoring on sille and
avoid build/test load during timed measurements. Any repair changing evaluated
source or the evaluator requires a new exact identity and requalification, not
an unrecorded edit to the checkpoint. Failures are work to resolve before the
fixed opening, not permission to omit honesty review or invent readiness proof.

## Initial findings — October 24

- The candidate is staged in a private temporary sibling-repository layout;
  the simulator/manager compilation passed. It uses exact fetched commits, not
  local branch heads, which may lag upstream.
- The resulting local Linux binary SHA-256 is
  `f7f51be5c21ded30725602317924a7c11f34bea3b1a49b47537cb60c4ccc5c10`.
  This is a preparation build, not a published production release identity.
  Worker compilation also passed (SHA-256
  `d322efd144db3cb7e449968ab3305ae7d631e1a407ab57f3dbaa27cb9a0e0e83`).
  Focused lifecycle/source/review tests passed using the exact candidate graph:
  `go test -p 2 -timeout 10m -run '^Test(RunMainCompleteSixEpochLifecycle|WinnerReview|SourceReview|ReviewedSource|StagingSource|SourceGraph)' -count=1 ./connect/sim-latency`.
  This does not claim a full-suite, race-suite or live-integration pass.
- The initial production protocol still required the retired `operator-proxy`.
  The October 24 retirement update removes it from all active production and
  staging source requirements, evaluator/build scripts, promotion, remote
  checks, and runbooks. An additive migration updates both database source
  guards without changing the existing winner-review requirements. Historical
  migration bodies and measured evidence remain untouched; obsolete locks do
  not qualify a new release. No fake placeholder or staging-mode bypass is used.
  Source/model tests and focused race tests passed; real disposable-database
  tests accepted both ten-repository modes and rejected obsolete/unknown pins.
  The updated complete `sim-latency/tests.sh` harness passed, including the race
  suite (114.045 seconds). Its retained log SHA-256 is
  `225388e19256b0b20edca94a414e26b82fff4609a2be7688668031b732252aaa`.
  Repeated disposable-database checks also passed with cross-mode rejection
  still enforced. Live main migration/deployment remains pending maintenance.
- Public API HTTPS, the configured main PostgreSQL connection, and the host's
  configured MinIO endpoint refused connections during the initial checks.
  The existing staging supervisor reports `lease_unavailable` from PostgreSQL.
  Redis connectivity succeeded. These observations require live infrastructure
  verification after maintenance; the operator confirmed main is down for
  maintenance. They are not evidence that the deployed API code is defective.
- The staging supervisor runs the older installed release, not the newly
  published elected-winner review integration. No staging policy, production
  pointer, main database, live credential, or admission window was changed.
- The initial complete direct-Go run caught archive metadata drift: the earlier
  Go 1.27.3 toolchain update changed the archive's minimal module boundary but
  not its manifest checksum. The checksum was repaired and the existing
  deterministic metadata test passed; no `v1` measurement evidence changed.
  Integration tests in that direct run correctly refused a missing local-env
  attestation. The guarded suite is being rerun with separately owned disposable
  PostgreSQL/Redis services and synthetic resources, not main or shared local
  stores. The ordinary local harness also refused stale launcher ownership;
  that refusal was not bypassed by deleting its lock or resetting shared stores.

## Published preparation checkpoint

Server `67c87445abe6725b25b3df2a13af7eb99cd4f8ed` contains the retirement and
test-boundary fixes. All ten remote `sim-latency` branches now identify the table
above, using ordinary fast-forwards (gvisor's branch was newly created).
Config `c32a2e5dc13724a25fa2fc6121b3b531d4c9140a` publishes the new schema-2
production manifest at `config/main/sim-latency.yml`; its canonical source lock
is `f8954162b28894aa96c7bd75b2aadb32e5724fa1852f32b0e6db7eb12f5645f5`.
The prior manifest was moved byte-for-byte into config's history directory.
The active staging configuration and all live source pointers remain unchanged.

Three consecutive complete `tests.sh` passes used isolated owned stores and
synthetic resources: 114.045, 101.615, and 98.768 seconds for the race phase.
Each included a separate passing six-epoch lifecycle invocation. The final
race log contains 326 passing top-level tests; its SHA-256 is
`2edfbd774ef03a82c35d9c856c588e25b6073cbdc9d4559b792d0435b36603ed`.
The second race log SHA-256 is
`591c7ab2f3b209d04dbb3f669658ebff4efdbce4d93dded8ab8028fce5cbb0a9`.

The exact production evaluator image build has completed (local checks below). Production qualification,
fresh calibration, real review/build broker qualification, live deployment,
settlement acceptance, and scheduled opening remain separate unchecked work.
The manifest's 16.1% historical floor is explicitly provisional until fresh
calibration; publishing it is not an assertion of a newly measured baseline or
permission to bootstrap/open production.

## Fixed-opening control integration

The control-plane follow-up adds `bootstrap-source --opens-at=2026-10-28T00:00:00Z`
using the source intent's existing immutable `opens_at` field. The supervisor
preserves it across restart rather than substituting the current clock. Once
qualified, the round can be advertised as `scheduled` before the opening;
admission becomes open at the start-inclusive boundary without an operator
having to start a process at midnight. Later epochs without a pin retain their
normal post-review/readiness opening.

An additive migration independently binds exact pinned openings in the round
insert guard, retaining the full window, runtime, winner policy, source-mode,
qualification, and lease requirements. It does not reinstall the obsolete
repository-count guard. Deterministic parser/recovery and supervisor tests pass
under race detection; real disposable-store tests reject both early and late
Go/SQL writers and accept the scheduled control in production and staging.
Deployment/migration and the actual pinned production bootstrap remain pending;
this control-only change does not alter the frozen evaluated source checkpoint.

## Exact-image local checks — October 24

The production evaluator built from the checkpoint above has immutable local
image ID `sha256:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c`.
Its preparation tag is
`urnetwork/sim-latency-evaluator-base:production-20261028-67c87445a`;
the tag is not an immutable distribution record. The image record SHA-256 is
`afa94b0e770347f4cc177f4eceb47db6fbb4e9aa714d8453bf0dd23e45fbfa9a`.

Verified locally against that image:

- Container smoke: offline candidate build, isolated PostgreSQL/Redis,
  migrations, a real 32-provider/8-client simulation, accounting, and pristine
  baseline/scoring completed. The retained log SHA-256 is
  `a93295e38f05b2756202b065f4746787cd2dd04f359bdc5aa8c8ec3218d16db8`.
  This reduced smoke workload is not production-scale calibration.
- Malicious build-initializer isolation: checks executed unprivileged, the check
  stage was discarded, and all six protected paths remained immutable.
- Production-memory resource-bomb cleanup: all ten evaluation CPUs were loaded;
  the 72-GiB memory limit produced a confirmed OOM kill. Cleanup took 588 ms,
  leaving zero job containers and networks. Evidence SHA-256 is
  `8057f235c7f2df106d0bbd295ed798e233b7b85d58098f462805491ba3d858a7`,
  bound to boot `f0e880cb-c124-4ed6-9edb-43669191d84e`.
- Read-only host-control verification found 12 physical/logical CPUs, SMT off,
  performance governor, turbo disabled, ten evaluation cores, and two reserved
  management cores.

Private preparation logs and records are retained under
`/home/by/urnetwork/temp/production-20261024.bUpBTuEw`; the resource-bomb
record also has a root-owned copy under
`/var/lib/urnetwork/competition-production-20261028/qualification/`.
These are host-local preparation records, not a claim of authenticated MinIO
retention or a published evaluator archive. The official qualification checklist
remains unchecked until the live lease, artifact storage, API/worker readiness,
and full calibration evidence are verified. Main API access still refused
connections on the subsequent October 24 check during confirmed maintenance.

## Completed exploratory calibration

An isolated nine-replicate preparation run started October 24 using the exact
image above, 1,800 providers, 200 clients, 80 arrivals/minute, quality window 2,
four fleet shards, one exchange host, and impairment enabled. Each replicate uses 60-second ramp,
13-hour synthetic reliability prewarm, 60-second settle, 180-second measured
window, 120-second request timeout, and up to 20 minutes for client warmup.
It runs with the production container ceilings on the ten evaluation cores.
The services and local-leaf configuration are disposable synthetic fixtures;
main/shared local stores are not inputs. Logs and artifacts are under the
private preparation workspace's `calibration/` directory.

All nine completed and their disposable services were removed. The Git-versioned
copy is [`../baseline/prelaunch-2026-10-24/README.md`](../baseline/prelaunch-2026-10-24/README.md),
published in server `4e59c98d9`. It preserves every run, including the slower
seventh run: 21,849 measured requests, 34 failures, 18,206.59-ms median run-level
p95, and 12.00% between-run p95 CV. The dataset explains both the hypothetical
16.1% margin line and the variance-dependent significance fringe. The archive
verifier and repeated race-tested metadata checks passed; historical `v1` data
was not modified. This evidence does not complete the fresh official
qualification/reference checks or finalize the initial margin.

This is non-official noise-floor exploration, not an authenticated round
control or a replacement for live qualification. No API/worker image identity
is invented to make it appear official. Each real round still needs a fresh
hidden-seed nine-run control bound to its frozen policy. The preparation margin
must not be labeled freshly calibrated until all replicates and the resulting
statistical evidence have been checked and retained.

The preserved run metadata records `flags.hosts = "1"`, while the configured
competition policy uses four exchange hosts. The archive index/readme now make
that difference explicit. A four-host impairment on/off frontier is required
before choosing production scale; the one-host samples must not be silently
relabelled as exact-workload production evidence.

The next capacity frontier uses the exact prepared image and source, four
exchange hosts, 200 clients, 80 arrivals/minute, quality window 2 and four fleet
shards, at 1,800 / 7,200 / 15,000 / 48,000 / 100,000 providers with impairment
on and off. Points execute serially in fresh disposable stacks, with existing
memory/CPU ceilings, a 40-minute per-point bound and verified project cleanup.
This is public-seed capacity/duration evidence, not an official control sample
or a significance estimate. Preserve failed points and do not build/test during
timed measurements; same-seed and reference calibration follow scale selection.

During confirmed main maintenance, the old staging supervisor had no running
evaluation and repeatedly failed to acquire its database lease. It was stopped
locally to prevent automatic staging work from overlapping these measurements
when main returns. No accepted job or main database state was modified. Inspect
the authoritative staging backlog after maintenance before resuming/replacing
that supervisor; do not discard accepted work as part of production activation.

## Review broker preparation

Server control-plane commit `0bb6f10f98e7239c0460cbfee7c3deda08ada83d`
implements `review-broker`, a tool-free Sol 6.1 max Responses API client, and
durably preserves completed unresolved responses before validation. A retry
cannot keep requesting new judgments until approval appears. Identity/policy
changes fail closed; transport failures without completed output may retry
within the original durable deadline. This does not change the frozen evaluated
server commit or replace its evaluator image.

Focused race tests passed, including three repeated invocations. The repeated
test log SHA-256 is
`e1f1c613de54a6c9b32298e525a240aa353bf126a41ab68e2da3e377570c8af1`.
The clean static control binary SHA-256 is
`e0a02ad59f37b937bb0f5c141e1933a7937f8a14c6bed9802080889acdc6ceb0`;
its scratch image ID is
`sha256:250d108319778e2500712317eee0c5eeefa0c4adc1d6bab41eee9ffdcebcb214`.
A network-disabled, read-only, non-root startup probe correctly refused the
missing dedicated model key. That probe's log SHA-256 is
`84d90d83ab65ae5c08c397658df468dd008594e527345432f8e550d00f394191`.

The model key is not provisioned, the broker is not enabled in the supervisor,
and no live provider review or full containment qualification is claimed.
Follow [`../evaluator/review/README.md`](../evaluator/review/README.md) to
provision and qualify the actual installed boundary. Independent build-broker
qualification remains separate. Three consecutive complete `tests.sh` passes
then completed against fresh, owned PostgreSQL/Redis fixtures, each including
the six-epoch lifecycle and race suite (102.524, 101.954, and 98.024 seconds for
the race phases). Logs are retained under
`/home/by/urnetwork/temp/production-broker-fullsuite.Adq6PfXg` with SHA-256:

- `tests-1.log`: `2ec16516d76fe2f4656e9151060e789de25cc7fbb65dd46756cd599b7befda1b`;
- `tests-2.log`: `0fd9606e285f0b3566c2b6f13fc5183dfa68b7501051252932f87656f54707c1`;
- `tests-3.log`: `2e26ec63dcdf9df56c4dc01011e78a912fa509a1f0424d36ef253abd84d85734`.

Owned test services were removed successfully. These builds/tests ran during an
intentional pause between calibration replicates 3 and 4, not concurrently with
timed scoring. The current replicate finished normally before tests started;
calibration orchestration resumed at 09:21 UTC after all three passes.

## Reviewed-source build handoff and staging eligibility

Control commit `b21ff9e66` adds the authenticated prospective image manifest to
the build request. It preserves the active intent's exact history, appends only
the separately reviewed source generation, and deliberately creates no future
round, activation, or readiness claim. The later publication path uses the same
generation constructor. This removes a missing input for the real build broker;
it is not an implementation or qualification receipt for that broker.

The same audit found a legacy production significance check in staging source
promotion. It could refuse an honestly reviewed best-safe staging winner merely
because its diagnostic improvement was zero/negative or nonsignificant. The
staging-only correction preserves finite/valid statistics and zero effective
margin without imposing production eligibility; production retains its positive
improvement, significance and margin requirements.

The new regression failed with the old guard, then focused source/promotion
tests passed three times under race detection with the fix. Retained log hashes:

- failing old guard: `73831297aa659ee3d6a0b3df7a0ff1ab9f34cd01822a067e40e0a4bb6a34361b`;
- repeated passing fixed path: `261e037c696c151b7b57f6da319c0f997946d166ca797794674c2d97d0e5eb7f`.

Those tests ran between calibration replicates 5 and 6, not during measurement.
The future staging checkpoint/image must include this protected-runner fix
before testing nonsignificant source promotion; do not hot-swap a frozen round.
The prepared production checkpoint and image remain unchanged. The real isolated
build/test/qualification broker and its deployment remain launch work.

The next build components are in server `7e1b97af7`: fresh authenticated Git
contexts exclude ignored files and credential-bearing origins, and fixed Docker
check/compile phases have separate caches, no network, no writable host mounts,
resource/output limits, and ownership-checked cleanup after cancellation or lost
creation acknowledgment. Mocked-daemon and real local-Git tests passed three
times under race detection. This is component evidence only: full broker wiring,
real-container adversarial qualification, independent test-model provenance,
image assembly, and live host/runtime readiness remain incomplete.

The opt-in trusted-base Docker integration test subsequently caught a real
mount-policy defect: Docker's tmpfs default made `/work` non-executable, so Go
could compile tests but could not launch their binaries. The private `/work`
tmpfs now explicitly permits execution; source and metadata host mounts remain
read-only, `/tmp` and binary-export tmpfs remain non-executable, and core dumps
are disabled. Fixed-argument regression coverage and actual Docker mount
inspection guard this distinction. The corrected trusted-base real-container
probe passed: check 22.55 seconds, compile 24.28 seconds, clean exact source
revision, and no residual owned containers. Its 126,549,552-byte binary has
SHA-256 `dd682292eba9d38f0fecd7b48ecdd1a96f942fe44fdbc44927bff472226d89ab`,
identical to the frozen evaluator simulator. Probe log SHA-256:
`f331c72ede942e52819bbdff8ee987bf61abbad536865d991900920ed6307c16`.
Fixed-boundary regressions passed three consecutive race-enabled runs. These
prove the trusted-base component path, not adversarial winner qualification,
independent model-owned test execution, or complete live broker integration.

Artifact authentication now also inspects the binary's Go build metadata as
data on the host: exact simulator module/package, exact clean approved Git
revision, Linux/amd64 executable mode and disabled cgo are mandatory. A clean
container exit returning text or unrelated bytes fails and still triggers
authenticated cleanup. A second actual Docker probe passed the stricter
artifact check, reproducing the identical frozen binary; log SHA-256:
`a13a37271124a174c40bbca929fe36af658b4cdb8dd796c735800a50d089255d`.
The package `tests.sh` wrapper currently refuses to start because its shared
local launcher lock has no readable owner. That preflight is not bypassed or
counted as passing. A direct race-enabled package Go suite also failed its six
database integration preflights because `WARP_ENV` was not set to `local`.
The supported release-gate helper subsequently supplied disposable services,
exact daemon-assigned endpoints and private synthetic resources without touching
the legacy shared containers. This restored the real `tests.sh` path and exposed
two provider-selection tests whose unspecified caller IP required an absent
GeoIP database. Both now use the portable documentation-subnet fixture through
a shared test-only constructor, with a deterministic address regression. No
production geography behavior changed, and no independent model-run test
provenance is claimed.

The supported `connect/sim-latency/tests.sh` then passed three consecutive
complete invocations on the code committed as `529be0f76`. Every invocation ran
the six-epoch lifecycle first and then the full race-enabled package suite.
Package durations were 104.097, 99.708 and 99.570 seconds. The release-gate
helper removed both exact owned service containers; no containers with that
owner remained. Preserved log SHA-256 values, in pass order:

- `ea3e1471a57f6c438f8db53fc22fc4dbe37f49f7a5e030ca20152c5f14f22125`
- `5f8020bb9a547bc6b240c0fde7a893ef17b06d0f59a8b2d8a7168c18c7969848`
- `b7a5ad71c9fdf275748c7dc9dc9bc909097e1fe40c494b0d571acbd369bd21f1`

These are shell-run Go tests, not a claim that the required independent low-
reasoning model executed the tests. Live main API, MinIO, reviewer credentials,
and complete reviewed-winner broker qualification remain separate launch gates.

Reviewed-image assembly now has a generated, tested copy-only Dockerfile plan:
its base alias binds the exact incumbent image, and binary/manifest/lock bytes
bind the approved generation. It contains no `RUN`, `ONBUILD`, `ADD`, external
frontend or extra execution stage, and copies no config/vault inputs. This is
an assembly-plan component, not a built reviewed-winner image or completed
broker CLI; execution, installed qualification and live publication remain open.

The next assembly components materialize only allowlisted inputs into Docker's
context and execute a copy-only image build with pull/network disabled. The
incumbent tag's actual image ID is checked, inherited `ONBUILD` hooks are
rejected, logs/inspection output are bounded, and the returned image must bind
the private build owner and evaluator-base kind. Alias substitution, inherited
execution, ownership loss, wrong image kind and log overflow are exercised by
deterministic fake-daemon tests, passing three race-enabled runs. This is not
yet a real assembly/qualification receipt or a wired, deployable build broker.
The control smoke after merging unrelated main maintenance changes also passed;
the evaluated source checkpoint remains unchanged.

The opt-in actual Docker copy-only assembly probe subsequently passed once,
then three consecutive race-enabled runs (10.00, 9.56 and 9.55 seconds). This
probe reuses the exact frozen binary without inventing a source approval or
future generation. All four temporary probe images were removed successfully;
the official evaluator image and source branches were untouched. Bounded build
logs and result identities remain in private preparation artifacts. The
three-run log SHA-256 is
`0d9b7fab3de85c548b848acfa45dc6805c07bda21a6216c04f22db8c27339c32`.
This establishes the real Docker execution boundary, not an approved-winner
end-to-end build, independent test-model receipt, or installed host readiness.

Offline build coordination now connects private authenticated source cloning,
incumbent module-metadata export, independent metadata seeds, isolated checking,
compilation, binary authentication and copy-only image assembly. Metadata export
runs only trusted `tar` in the constrained incumbent container; a bounded Go
reader rejects traversal, links, special files, duplicate entries and budget
overflow before writing regular seed files. Deterministic fake-daemon pipeline
tests force invalid metadata and failed checks, proving no later phase starts
and cleanup completes. Metadata/sandbox/pipeline regressions passed three
race-enabled runs. This integrated coordinator still needs its real full-path
probe, deployable broker entry point, independent model-owned testing and host
qualification before it may return a promotion-authorizing receipt.

The real full-path probe exposed a partial-repository failure: a full Git clone
requested unavailable historical objects from the frozen gvisor checkout.
Build cloning now initializes an empty private repository and fetches only the
exact approved commit at depth one, with no tags, inherited template or object
alternates; private `FETCH_HEAD` transport metadata is removed. A deterministic
fixture removes the approved tree's parent commit, proves full cloning fails,
then proves the exact-commit path succeeds. Repository-specific errors now
identify the failing stage without publishing private Git diagnostics.

The corrected actual offline pipeline then passed in 190.15 seconds including
private synthetic-change preparation (153.76 seconds inside the coordinator).
All ten repositories, metadata export/validation, check, compile, authenticated
new-commit binary and image assembly completed. Its temporary image was removed;
no live review, branch publication or qualification was performed. Probe log
SHA-256: `61fc946da65058e13b08c37da3ac523d610cd6567d0eff666e20eae77c3d191f`.

`source-build-broker` now provides the supervisor-facing entry point, with a
digest-pinned private settings file and independently pinned qualification
executable. It checks qualifier availability before expensive work, rechecks
the program before execution, propagates termination through contained cleanup,
and accepts only complete evidence for the exact built image and owner. Broker,
Git-fetch, sandbox and supervisor regressions passed three race-enabled runs.
The actual independent low-model/host qualifier still must be implemented,
provisioned and qualified; no stub receipt is permitted for launch.

The clean Linux/amd64, cgo-disabled control binary from server
`f01855276c6d1d0d04289d1d435c2b011648929a` built with
`vcs.modified=false`; SHA-256:
`6de33048b7a29204e1962279dd39c87ee202c1bc6802199651922301b711f1fa`.
Its actual root-run broker CLI rejected a digest-pinned settings file naming a
missing independent qualifier before reading a build request or creating a
container. Exit status was 1 and stdout was empty, not a counterfeit receipt.
The binary is a preparation artifact, not an installed/qualified release.

At 2026-10-24 11:31 UTC main API and PostgreSQL were reachable again, but
authenticated `/competition/readyz` returned 503 with
`frozen_source_activation: false`; public `/competition/info` returned
`source_not_ready`. Sille could not connect to the actual configured MinIO
authority `172.28.208.177:23900`. The earlier port-9000 probe did not test that
configured authority. Maintenance completion has been requested from the
operator; control-plane writes remain paused and the old staging supervisor
remains intentionally stopped. The dedicated review API key is still absent.

The independent Sol 6.1 low test client is implemented with one forced
argument-free fixed-suite tool, tool-disabled completion, exact image/suite/log
binding, provider IDs and usage, and observed exit/test-count checks. It shares
bounded pinned-model SSE parsing with the max review client while preserving
opaque reasoning items only for stateless continuation. Provider-request and
test-execution deadlines are distinct. Tests reject tool escape/repetition,
changed identity, zero-test or unlogged success, failed-test blessing,
ambiguous/case-aliased judgments, redirects and canceled work; three consecutive
race-enabled client/review regression runs passed. This is mocked provider
evidence only. The real fixed-suite executor, durable qualifier journal,
containment/host evidence, dedicated-key access and live qualification remain
required before the broker may authorize promotion.

The fixed offline correctness executor now runs 33 mandatory named tests across
three packages in a separate contained phase. It authenticates exact Go test
run/pass events, rejects missing/skipped/repeated/unfinished tests and incomplete
packages, preserves stdout/stderr and binds the exact retained log bytes to the
image/suite result. Package accounting is linear in the bounded event stream.
Executor/parser/model-boundary regressions passed three race-enabled runs.
The actual full-pipeline probe then passed all 33 named correctness tests in
the new contained phase. Total test duration was 220.82 seconds, including
synthetic-change preparation; offline coordination plus correctness took
184.39 seconds. Its test-log bundle SHA-256 is
`23af7252dc2a79088a21c6b595f528fbbeaf33594f88de434e524969a4250613`,
and full probe log SHA-256 is
`4d1cf40b200038df3618c29813669d9541bdefeea33d0380d1847a69328190ee`.
The temporary image and all owned containers were removed successfully. This
is direct Go execution against synthetic source-review fixtures; no live
model, complete database-suite or physical-host qualification is claimed here.

The correctness executor now has a durable execution journal. Exact plan identity
is reserved and directory-synced before dispatch; observations publish atomically
without overwrite, and replay rechecks retained logs instead of calling Docker.
Completed failures cannot be resampled. Concurrent callers, unknown/interrupted
owners, changed plans, corrupt records and missing/modified/redirected evidence
are deterministic regression cases. Journal/correctness tests passed three
race-enabled runs. The real full-path probe then passed all 33 mandatory tests
and verified immutable journal replay with zero additional Docker calls.
Full probe duration was 219.73 seconds; log SHA-256:
`56b75ca2317908bb88b417fee4355c7cacbd33dd778c2b131dab6ce6e84807a8`.
Owned image/container cleanup passed. Provider-response journaling and the
complete independent-model/host qualifier remain unfinished launch work.
