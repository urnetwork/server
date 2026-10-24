# Production launch — October 28, 2026, 00:00 UTC

Preparation started October 24. The public opening is fixed; preparation and
qualification start immediately. Current upstream development heads replace the
previous plan to wait for a Monday source freeze. Do not admit public submissions
before the opening or reinterpret an existing staging round as production.

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

## Execution checklist

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
