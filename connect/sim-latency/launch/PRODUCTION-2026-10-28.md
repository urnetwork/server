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

## Completed exploratory calibration

An isolated nine-replicate preparation run started October 24 using the exact
image above, 1,800 providers, 200 clients, 80 arrivals/minute, quality window 2,
four fleet shards, and impairment enabled. Each replicate uses 60-second ramp,
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
inspection guard this distinction. Full real-container verification is still
in progress; this is not yet a successful build or qualification receipt.
