# sim-latency

`sim-latency` is the fixed measuring instrument for the URnetwork latency
competition. It runs the real API, exchange, matchmaking, client, provider,
reliability, and accounting paths against a deterministic provider fleet and
fake origin site. Submissions change the measured product repositories; they do
not change this package or its scorer.

Build and test the host tool from this directory:

```bash
make
./tests.sh
```

`make` writes `build/<goos>/<goarch>/sim-latency`. The package test gate is
Go-only and runs under the race detector against the local PostgreSQL and Redis
services, like the other server packages.
The actual compiler must be **Go 1.27.3**. Select that installed toolchain on
`PATH` and use `GOTOOLCHAIN=local`; build/test launchers reject other versions.

## Current competition contract

The original **2026-10-28 00:00 UTC** opening was missed, not backdated.
The approved replacement production season, R2 epoch 1, opened submission intake
at **2026-10-29 13:06:42.413416 UTC**. Intake remains open while grading and
publication are held for calibration. The reduced public R9/R9 pilot and native
13-source exclusion census are accepted; the fresh hidden-seed 36-run screen
is now running and **not yet accepted**. Follow the
[current checkpoint](launch/current-state.json),
the [live hidden-screen record](launch/HIDDEN-SCREEN-LIVE-2026-10-31.json), and
[`launch/PRODUCTION-2026-10-28.md`](launch/PRODUCTION-2026-10-28.md) and its
exact-artifact records for current evidence. An installed image or a passing
local smoke is not production activation.

The environment is defined by the source-epoch ledger at
`config/main/sim-latency.yml`, the evaluator configuration in
`config/main/competition.yml`, and the authenticated same-round baseline manifest.
The table below separates fixed competition boundaries from historical
calibration; it is not a claim that the new checkpoint has passed calibration.

| Item | Boundary or qualification status |
|---|---|
| Evaluator host | one 128 GiB-class Ubuntu host with 12 isolated CPUs |
| CPU boundary | 10 evaluation CPUs; 2 management/cleanup CPUs |
| Workload | Frozen epoch-one profile: 3,600 providers, 200 clients and 80 arrivals/minute; accepted public pilot, independent-seed screen running |
| Selection | quality window 2; 4 exchange hosts; 4 fleet shards |
| Measurement | 180 seconds with provider impairment enabled |
| Aggregation | one immutable 9-run control per epoch; median of 9 fresh runs per candidate |
| Initial significant-improvement percentage | 26.9% prospectively frozen by the accepted October 26 three-pair reduction; independent-seed acceptance remains pending |
| Admission | six seven-day epochs; unbounded paid submissions at $20 each |
| Dispatch | immediate single-job FIFO; Redis list backed by authoritative PostgreSQL |
| Submission deadline | 3 hours total, including attempts, backoff, score, and cleanup |

The historical qualified checkpoint used 1,800 providers. Do not transfer that
qualification, its measurements or its final report to the new source/image.
The 3,600-provider profile is frozen for R2 epoch one; do not resize it mid-round.
Future checkpoint scale selection requires fresh impairment-on/off frontier,
correctness and resource evidence and the full cold-job three-hour budget. Incomplete points
are retained for diagnosis and never count as control samples.

Every canonical submission patch gets a content-addressed image derived from
the immutable evaluator base. Candidate execution and builds are offline and
default-deny. The job boundary contains the runner and its disposable
PostgreSQL/Redis services. A candidate receives only the direct
`config/local` and `vault/local` leaves, read-only; parent, `all`, `main`, host
site configuration, control-plane secrets, and the Docker socket are never
mounted. A fresh per-job site/stats directory is the only writable simulator
state.

See:

- [`OFFICIAL-RUN.md`](OFFICIAL-RUN.md) for the evaluator and artifact contract;
- [`PLAYBOOK.md`](PLAYBOOK.md) for live deployment and epoch operation;
- [`RUN-MAIN.md`](RUN-MAIN.md) for the six-epoch agent harness and mandatory
  winner honesty review;
- [`launch/ONBOARDING.md`](launch/ONBOARDING.md) for submitter integration;
- [`launch/INCIDENT-RESPONSE.md`](launch/INCIDENT-RESPONSE.md) for operational response;
- [`launch/APEX-OPEN-QUESTIONS.md`](launch/APEX-OPEN-QUESTIONS.md) for the shareable partner
  decision and acceptance checklist;
- [`playbook.yml`](playbook.yml) for machine-readable launch status;
- [`baseline/README.md`](baseline/README.md) for preserved calibration evidence;
- [`baseline/final-baseline.html`](baseline/final-baseline.html) for the visual
  **historical** baseline report (not the new production checkpoint);
- [`../../controller/competition_controller.go`](../../controller/competition_controller.go)
  and [`../../model/competition_model.go`](../../model/competition_model.go)
  for the API/worker implementation; and
- [`sn/api/competition.yml`](https://github.com/urfoundation/sn/blob/main/api/competition.yml) for the
  public competition API.

Superseded plans, workstation campaigns, preliminary reports, and their helper
programs are retained only under [`old/`](old/README.md).

## Macrocosmos integration: v2 contract

The public OpenAPI snapshot is
[`sn/api/competition.yml`](https://github.com/urfoundation/sn/blob/main/api/competition.yml). Select the API
contract from `GET /competition/info`, not from an environment name or a zero
takeover margin. The selected `active_round` (production) or `staging_round`
advertises its immutable `decision_mode` and `required_api_capability`. V2
requires `decision_mode: provisional_reward_v2` and support for the exact
advertised capability: `competition.reward.v2` (provisioned filesystem runtime)
or `competition.reward.v2.runtime-blob.v1` (authenticated retained runtime
manifest). Both mode/capability fields absent identify the legacy v1 contract; an
unsupported or incomplete explicit contract must not silently fall back to v1.

| Purpose | V2 path and behavior |
|---|---|
| Discover a round | `GET /competition/info`; retain the selected round's `round_id` and frozen contract |
| Screen before a production fee | `POST /competition/v2/score/preflight`; checks the canonical patch and known repeat-reward exclusion, but does not admit or reserve a job |
| Submit | `POST /competition/v2/score`; submitter bearer token and the usual `round_id`/canonical `patch` body; `202` returns `job_id`, `status_url`, and the frozen contract |
| Poll an owned job | `GET /competition/v2/score/{jobId}`; persist and follow the accepted `status_url` exactly, with the owning submitter credential |
| Reconcile public results | `GET /competition/v2/rounds/{roundId}/result`; `GET /competition/v2/rounds/{roundId}/decisions` returns the same complete result and ordered history |
| Reconcile settlement events | `GET /competition/v2/events?after_cursor=...`; separately scoped settlement credential, durable cursor, at-least-once delivery; acknowledge exact events with `POST /competition/v2/receipts` |

For an explicitly v2 round, `POST /competition/score` and the legacy
`GET /competition/score/{jobId}` return `409 unsupported_decision_mode` for an
otherwise valid authorized request. That conflict is expected, not a reason to
retry the legacy endpoint. Conversely, v2 score URLs reject a v1 round. Legacy
operator credentials do not acquire v2 submitter or review/settlement authority.

Closing admission does not stop accepted work. Publication requires admission
close, complete authoritative drain, an immutable evaluation seal, and reveal:
`seal.publication_at` is the later of sealing time and the frozen `reveal_at`.
Before publication, terminal success/failure polling remains `state: completed`
but the owning production submitter credential can immediately read a completed
private `score`: raw/normalized values, reviewed numeric/bool G1–G6 reasons,
significance and safe numeric summaries. Full evaluator errors, arbitrary
diagnostic strings, logs, seeds, artifact paths and review decisions remain
sealed; `evaluation_status` and message-free `evaluation_failure` expose only
reviewed lifecycle/failure signals. Staging outcomes keep their existing
publication gate. This additive owner feedback does not rewrite any current
round's frozen evaluator, scoring policy, source/OpenAPI evidence or public
decision contract, and does not publish, elect, approve or pay a winner.
The public round-result/history endpoints return `409 round_not_published` until
that boundary, then return a `no-store` projection. A provisional reward can be
published without a completed honesty review.

UR privacy is credential-owner-wide, not individual-miner-wide. One
Macrocosmos/Apex submitter credential serves multiple miners and can read every
job durably associated with that credential. Apex must authenticate the miner,
authorize its own miner-to-submission mapping and return private feedback only
through that mapping. Never expose the shared credential or treat a guessed
`job_id` as authorization. Polling responses and errors are `Cache-Control:
no-store`; do not copy private feedback into a public result cache or leaderboard.

Identical canonical patches may bind several distinct Apex `submission_id`s to
one cached `job_id`. Preserve each miner's admission identity and fee receipt;
associate them only when the returned round, canonical patch, status URL,
staging flag and decision contract all match. The reference adapter fans out
the same validated job observation to those existing admissions, with owner
feedback stored as `private_score`, not public `score` or `published`. It never
creates an association for an unadmitted miner. Multiple admissions to one job
do not create more evaluator draws or duplicate reward entitlements; public
winner/reward reconciliation keeps the existing job/round/revision identity.

The reference adapter automatically refreshes pre-upgrade completed admissions
and probes ambiguous completed cache hits without asserting scoring success.
Its per-job durable backoff (one minute by default) alternates with normal FIFO
polling and stops once feedback or terminal failure is known. Update Apex
consumers before the API rollout: old consumers may reject private production
`score` responses, and old adapter binaries cannot read new private feedback or
refresh fields. Do not roll an old binary back onto the new adapter state file.

Production R2 selects `elected_winner_snow_union_v2`: publish the provisional
winner, automatically review it with qualified Qwen (`qwen3.8-27b`) and Gemma
(`gemma-4-26b-a4b`) on Snow, retain the union of their findings, and require both
to approve. Revoke/re-elect on substantiated rejection, then independently build,
qualify and promote that approved winner before opening the next epoch.
Only an exhausted eligible ranking carries the incumbent forward. Missing or
expired review and failed source qualification block and alert support; they
never silently approve, promote another candidate, or fall back. The sixth
epoch is reviewed before the supervisor exits. Historical Sol, GPT-5.4 mini and
three-model Snow records retain their original identities; Muse is not a member
of the R2 two-model policy. Provider qualification remains required.
See [RUN-MAIN.md](RUN-MAIN.md#adopted-automatic-closing-policy-2026-10-23)
for activation, the private model-broker protocol, scoped authority, and
independent build/readiness requirements.

V2 rounds do not populate `GET /competition/leaderboard`, including when
`include_staging=true`. That endpoint retains legacy finalized v1 winners and
their `honesty_review` projection. For v2, use the versioned round result/history
and, with the appropriate credential, the reward event feed. Discovery does not
turn `winner_job_id` into a v2 winner field: reconcile
`provisional_reward_winner`, `reward_revision`, and `decisions` instead.

The result's `decisions` array (Go `Decisions`) is ordered by contiguous
one-based `revision`. Revision 1 is `elect` or `no_winner`; later revisions
append `previous_decision_sha256`, `superseded_revision`, and the previous
recipient when present. Scores, original completion timestamps, sealed ranks,
and eligibility evidence never change. A review reselects from that original
ranking after disqualifications/restorations; it appends a reward revision only
if the recipient changes. Approving the same recipient or deferring only its
source does not create another reward entitlement.

| `decisions[].action` | Meaning |
|---|---|
| `elect` | Initial provisional recipient selected; `selected_job_id` is non-null |
| `replace` | A later revision selects a recipient, including restoration after a no-winner revision |
| `no_winner` | No eligible recipient remains; `selected_job_id` is null |
| `invalidated` | An evaluation-integrity incident terminally invalidates the sealed evaluation; recipient and settlement anchor are null, with no later reward revision permitted |

The complete, case-sensitive `decisions[].reason_code` set is:

| Reason | Meaning |
|---|---|
| `first_eligible` | First eligible, non-disqualified entry in the original ranking |
| `no_submissions` | Empty terminal inventory |
| `no_gate_passing_candidate` | No qualifying gate-passing/placeable candidate |
| `no_significant_improvement` | A placeable, non-gate-failing entry exists, but no eligible improvement remains |
| `no_new_reward_entitlement` | No eligible entry remains and the sealed inventory contains a prospectively excluded exact repeat identity |
| `all_eligible_disqualified` | Originally eligible entries exist, but all are disqualified |
| `EVALUATION_INTEGRITY_INCIDENT` | The reason for `invalidated`; it is not candidate dishonesty |

`first_eligible` accompanies `elect`/`replace`; the five other lowercase reasons
accompany `no_winner`. Exhausted eligible candidates take precedence over a
repeat exclusion, then over insufficient improvement or failing gates. These
decision reasons are distinct from `seal.entries[].ineligibility_reason` and
the uppercase review `reason_codes`. Unknown future actions, reasons, or states
must fail loudly: quarantine them and alert for explicit reconciliation, never
default to no winner, approval, or settlement.

| Result field | Complete value set and scope |
|---|---|
| `integrity_review` | `not_reviewed`, `approved`, `disqualified`, `not_applicable`; only reviews of the current provisional recipient count, restoration projects as `approved`, and no recipient means `not_applicable` |
| `source_approval` | `pending`, `approved`, `rejected`, `deferred`, `not_applicable`; belongs to the current recipient, not the separately selected incumbent in `source_decision`; reward restoration alone does not restore rejected source approval |
| `settlement_status` | `pending`, `received`, `applied`, `confirmed`, `rejected`; summarizes receipts for the current reward revision only, with precedence `confirmed` > `applied` > `rejected` > `received` > `pending` |

These states are independent: a provisional recipient is not an integrity
approval, source activation, or payment. `received` acknowledges delivery only;
`applied`/`confirmed` are externally asserted accounting claims. The
`effective_settlement_anchor` is the selected job's original authenticated
`completed_at`, not a transfer time or a burn formula. Historical receipts
remain visible after replacement but do not settle the new revision. A receipt
or invalidation does not establish that a prior transfer can be reversed.

Explicit v2 staging is fee-free, freezes a zero takeover margin, elects the best
gate-passing nine-replicate entry without a significance requirement, and marks
the seal/result/decisions `staging: true` with `settlement_mode: shadow`. It does
not automatically approve source or move funds. Staging receipts permit only
`received` or `rejected`, with no accounting application or chain transaction;
therefore staging `settlement_status` is limited to `pending`, `received`, or
`rejected`. The manual legacy review and staging flows below do not apply to
explicit v2 rounds; v2 review/source authority uses the separately scoped
automation contract described in [`RUN-MAIN.md`](RUN-MAIN.md).

### Explicit intake-preserving calibration maintenance

The [October 30 owner approval](launch/CALIBRATION-MAINTENANCE-INTAKE-OWNER-APPROVAL-2026-10-30.json)
adds a distinct, bounded intake permission for the current R2 epoch-one round.
Its [prospective hold request](launch/CALIBRATION-MAINTENANCE-INTAKE-HOLD-REQUEST-2026-10-30.json)
expects evaluation revision 3 and appends an **active** revision 4 with reason
`CALIBRATION_MAINTENANCE`. These staged files are not evidence that the code is
deployed, that revision 4 exists, or that calibration has run or passed.

Only that exact scope, frozen source/policy, evidence digest, request digest and
idempotency key can use this permission. Admission must remain unheld, and the
original reward-delivery hold must remain active and unchanged. The default
per-service limit is ten hours from the original database-created hold timestamp,
allowing roughly nine hours of serial calibration plus verification. Future
timestamps, expiry, release/rehold, another revision, incidents or source drift
refuse the permission; replay does not renew it. A maintenance reason on any
other hold or round cannot waive readiness.

After the running job terminates and the FIFO slot and measurement lock are
free, the real worker can be stopped for approved serial calibration. No fake
heartbeat or host-check fact is created: `GET /competition/readyz` may honestly
return 503 with the real host checks false, while `/competition/info` reports
`runtime_status.intake_ready: true`, `grading_ready: false` and
`publication_held: true`, and authenticated v2 submissions queue normally.
Existing separately approved private completed-score feedback is unchanged;
public results, rankings and winner remain held.

All non-host readiness checks, live protected archive checks and authentic
runtime-envelope/frozen-policy validation remain mandatory. Both durable enqueue
passes reread latest holds, activated source and the real clock on the existing
transaction. Epoch admission still uses the original receipt time, and queued,
unstarted jobs do not start evaluation clocks. Restoring grading requires genuine
host readiness and a separately authorized `RECOVERY_APPROVED` release; this mode
does not accept calibration or release rewards.

## Score and statistical significance

For each replicate, the raw score is p95 `total_ms` over requests that start in
the measured window. Failures and incomplete bodies are charged at the frozen
request-timeout ceiling. Lower is better. Each epoch has exactly one control
sample of nine independently reset runs. The evaluator measures that control
before it builds or runs the first submission, freezes the exact
`baseline.json` bytes and SHA-256 in the append-only
`competition_round_baseline` row, and gives every later evaluation those same
authenticated bytes. Each candidate contributes nine separately reset runs.

The competition ranks candidates by their absolute median raw score ascending,
then submission time and job id. The displayed normalized score remains
`100 * baseline_median / candidate_median`, clamped to 1–200, but it is only a
presentation of the shared control and is not a ranking input. After the first
control is frozen, an ordinary submission runs only the nine candidate
replicates rather than another nine control replicates.

Historical epochs without an immutable shared control retain their original
per-job baseline samples and normalized-score-first ranking. The new policy
does not rewrite an already-published leaderboard.

A score is placeable only when all G1–G6 correctness, volume, path-integrity,
matchmaking, stability, and resource gates pass. A placeable submission is
takeover-eligible only when all of these are also true:

1. its median raw score is at or below the current source epoch's improvement
   threshold relative to the same-round baseline;
2. a one-sided Welch test over the run-level raw scores has `p <= 0.05`; and
3. the winning variance supports a next-epoch threshold in `(0%, 50%]`.

Every successful score records the exact baseline SHA-256, baseline and
candidate means, sample variances, observed improvement, current margin,
minimum statistically significant improvement, required improvement, Welch
statistic/degrees of freedom/p-value, and the recommended next-epoch margin.
The one-sided Welch test is recalculated for each candidate using the shared
nine-run control and that candidate's nine independent runs; reusing the
control does not turn requests within a run into independent observations.
This is part of the immutable evaluation result, not a post-hoc leaderboard
calculation.

The production source epoch 0 margin is **26.9%**, frozen by the
[accepted original-three reduction](launch/CALIBRATION-THREE-A-FREEZE-2026-10-26.md).
Intake-first activation is not calibration acceptance: the pending fresh public
pilot, independent reference screen and live qualification must pass before
grading/publication holds can be released. Historical 16.1% values remain
attached to their original rounds, not silently substituted for this margin.
Under legacy v1, production statistical eligibility places a submission into
the ranked honesty-review queue; it does not by itself make the submission a
winner. V2 instead publishes the provisional reward and independent approval
states described above.
When an epoch has an approved
winner, its authenticated `score.json` sets the next source epoch's
percentage to the scorer's recommendation, which never weakens the incumbent
margin. If no submission is statistically significant, or every significant
candidate is rejected as dishonest, the epoch has no winner and both the
source commits and percentage carry forward unchanged.

### Current and next-epoch G2 work-volume policy

Production R2 epoch 1 retains its frozen rule: the median completed-request
count and median received bytes must each be within **95–105%**, inclusive, of
the shared epoch baseline. A genuine speedup receives no exception to the
current upper bounds. Its existing jobs, evaluator and results are not rejudged.

The owner approved a prospective change starting in epoch 2: both medians must
remain **at least 95%** of baseline, inclusive, with **no upper bounds**.
This is `g2_volume_policy: minimum-work/1` and scorer `sim-latency-score/2`;
absence of that policy identifies the historical two-sided rule. More completed
work alone will no longer fail G2. All other gates, nine-run replication,
latency ranking, practical margin, Welch significance and three-hour submission
budget remain unchanged. Faster raw latency is not permission to fake traffic,
skip accounting or bypass correctness.

The policy change is not active in epoch 1. It needs a separately authenticated
protected-instrument transition, qualified evaluator image and fresh shared
baseline for the successor source. Miners should follow the exact policy and
image advertised for their accepted round, not infer an upgrade from this
announcement or reuse another round's control sample. See the
[owner approval](launch/PRIVATE-FEEDBACK-NEXT-G2-APPROVAL-2026-10-30.json)
and [current checkpoint](launch/current-state.json) for rollout status.

## Epoch source identity and promotion

The source graph applies to both contracts. The manual `epoch-review` and
`promote` sequence below is the legacy v1 workflow; explicit v2 uses separately
recorded review, source intent, readiness, and activation authority.

The complete local-module graph that can affect the measured product is frozen
by source epoch: `server`, `connect`, `sdk`, `proxy`, `glog`, `goidenticons`,
`userwireguard`, `sn`, `warp`, and `gvisor`, all on branch
`sim-latency`. The first four are the product roots; the remaining six are required local
dependencies and evaluator build inputs. The trusted API and worker
continue on `main`; they record their runtime image digests per evaluation but
their source commits are not scoring inputs.

Round N evaluates source epoch N-1. Before a measured command touches services,
the tool verifies the disposable evaluation checkouts against the authenticated
source lock and ledger. Operator worktrees are not evaluation inputs and need
not be on any particular branch. A manual check can target an explicitly
prepared disposable repository root:

```bash
./build/$(go env GOOS)/$(go env GOARCH)/sim-latency \
  source-check --epoch 0 --repos-root /tmp/evaluation-source --json
```

After production round N closes and every evaluation is terminal, the worker
exits with any significant results still embargoed. The trusted agent harness enumerates
the exact ranked candidate into a fresh mode-0700 directory:

```bash
./run-local-main.sh epoch-review --epoch N next

# After inspecting candidate.json, score.json, and canonical.patch, append one
# immutable decision. Rejection returns/materializes the next ranked candidate.
./run-local-main.sh epoch-review --epoch N reject \
  --job-id CANDIDATE_JOB_ID --reviewer HONESTY_AGENT_ID \
  --reason 'score-path tampering detected' --evidence honesty-report.json

# Approval atomically finalizes this exact job as the winner.
./run-local-main.sh epoch-review --epoch N approve \
  --job-id CANDIDATE_JOB_ID --reviewer HONESTY_AGENT_ID \
  --reason 'honesty checks passed' --evidence honesty-report.json
```

The database permits decisions only for the current highest-ranked unresolved
candidate. Rejected reviews are append-only. Rejecting the final candidate
atomically finalizes the epoch with no winner. The harness removes each private
temporary directory after recording its evidence.

Only after review finalizes the epoch does the external control loop run one of:

```bash
# Approved significant winner. Use the exact canonical.patch and score.json
# materialized for the approved job.
./run-local-main.sh promote --epoch N \
  --winner /restricted/winner-N --winner-job-id WINNING_JOB_ID

# No significant candidate, or all significant candidates rejected.
./run-local-main.sh promote --epoch N --no-winner
```

Promotion creates one temporary root and clones all ten locked repositories
into it. It checks out each `sim-latency` branch at the prior epoch, applies and
commits the winner to the evaluated server surface, verifies every dependency
remains unchanged, pushes changed source branches first, and activates the new
config ledger commit last. Before staging, promotion queries the finalized round
and requires the exact approved job, canonical patch SHA-256, and score
significance record; unevaluated repository patches are rejected. `--dry-run`
performs the same validation without publishing. The next evaluator image and
round baseline are built only after the new source epoch is active.

## Competition lifecycle

This section describes legacy v1 finalization and staging. Explicit v2 uses the
seal, reveal, provisional reward, and independent review states above.

Before production epoch 1, operators can run a staging era containing any
number of sequential staging epochs numbered from zero. Each staging epoch
uses the same authenticated submission, immutable MinIO retention, canonical
patch cache, Redis FIFO, isolated evaluator, scoring, embargo, and poll-result
publication paths as production. It is fee-free, always evaluates against the
frozen source epoch 0, and finalizes automatically after admission closes and
all accepted work drains. The highest-ranked placeable candidate passing every
gate becomes the named staging winner, regardless of statistical significance
or takeover margin. If none qualifies, `winner_job_id` is null. The default
production leaderboard
excludes staging; `GET /competition/leaderboard?include_staging=true` exposes
the same winner identity in the finalized staging round and its winning entry.
Staging has no review pause before finalization: its winner starts
`honesty_review: not_reviewed`. After explicit manual review, the append-only
`staging-review` command can mark that exact finalized winner `approved` so
Apex can exercise its approved-winner path. Staging never promotes source or
creates a production winner. Newly opened staging rounds freeze a zero
takeover margin; historical rounds retain their original policy. The
[epoch-5 release record](launch/STAGING-5-RELEASE.md) describes its historical
opening policy.

With the operator-token environment configured as described in `RUN-MAIN.md`,
`./run-main.sh staging` creates or returns the current staging epoch and
`./run-main.sh advance-staging` closes admission, evaluates every accepted FIFO
submission through finalization and reveal, then creates the next open epoch.
Its source preflight requires all ten remote `sim-latency-staging` branches
to exactly alias the frozen epoch-zero commits. `staging-worker` remains the
natural-window variant. `staging --replace-current` is an explicit reset that
refuses to run while an evaluation is active. Committing production epoch 1
atomically ends the staging era and cancels queued staging work while retaining
its audit evidence; transition refuses to race a running staging evaluation.

Submissions are admitted throughout each seven-day window and evaluated as
soon as possible in exact FIFO order. Closing an epoch stops admission but does
not stop accepted work: the worker continues until the entire backlog is
terminal, then seals the deterministic significant-candidate ranking and exits.
Public scores, full failures, hidden seed, workload, and leaderboard remain
embargoed while the agent harness reviews the candidates one by one; authorized
production owners may already inspect the private feedback described above.
Approval of the first
honest candidate—or rejection of the last candidate—atomically finalizes and
publishes the epoch. The external control loop can then promote or carry forward
the source and explicitly create the next epoch.

Duplicate canonical patches share one cache identity. Infrastructure failures
may retry within the original three-hour budget; build, structural, resource,
and other submission failures are terminal. The accepted submission count has
no configured maximum.

## Local development

Start the ordinary local backing services, then use the wrapper that supplies
the local environment:

```bash
cd server/local
./run-local.sh

cd ../connect/sim-latency
./run-local.sh init \
  --count 2000 --clients 200 --rate 80 --quality-window 2 \
  --seed 1 --out providers.yml
./run-local.sh run \
  --epoch 0 --reset --providers providers.yml \
  --meta results.run.json > results.csv
./run-local.sh analyze --run results.csv
```

Measured `run`, `fleet`, and fresh `baseline` commands require a configured
source epoch. Use a clean epoch checkout or an authenticated evaluator source
lock; do not bypass the identity check for convenience.

For `run`, `fleet`, `reset` and a measuring `baseline`, the wrapper first
sources `server/local/local-env.sh`, which attests the running launcher's
endpoints (`WARP_TEST_ENV_ATTESTED_ADDRS`); without the launcher it exits
before starting the simulator. Every `WARP_ENV=local` process, this one
included even when only its own defaults set `WARP_ENV=local`, refuses every
PostgreSQL and Redis dial outside that attestation, so a local service name
that falls through to `127.0.0.1`, where a production data tunnel can listen,
fails closed. `reset` and `run --reset` truncate tables with `CASCADE` and run
`FLUSHALL`, so before either sends anything they also require every pool
endpoint to be attested and not loopback outside the private portable root
(`server.RequireAttestedLocalEndpoints`). The evaluator container attests its
own Compose `postgres` and `redis` services in its entrypoint. See
`server/local/README.md`.

`run-local-main.sh` is a trusted operator/development wrapper for a local checkout
using port-forwarded main PostgreSQL and Redis endpoints. It is not the
candidate-container entrypoint and does not replace the official evaluator.

## Run lifecycle and artifacts

Warm-up connects the fleet, pre-establishes deterministic performance and
reliability evidence, propagates selection state, and proves every client exit
and HTTP lane. Seeded churn and impairment schedules begin at the authenticated
measurement boundary so variable pool construction cannot shift the workload.

CSV output contains one row per request:

```text
t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s
```

`status=0` means the request did not complete correctly. A 200 is emitted only
after the declared body is received exactly. The adjacent `run.json` records
the source epoch, workload digest, measurement window, request ceiling,
simulator identity, flags, stats root, CSV digest, and aggregate diagnostics.
Only a clean drain, child reap, service shutdown, durable stats close, and
accounting flush produce the final completion marker.

Official evaluation additionally retains immutable accounting, complete cgroup
resource evidence, stderr, FindProviders2 samples, canonical patch, image and
runtime digests, score, and an authenticated final manifest. Missing, mixed,
legacy, malformed, OOM-killed, hard-killed, escaped, or incomplete artifacts
fail closed.

## File-only analysis and scoring

`analyze`, `compare`, `score-baseline`, and `score` do not start services.
Local `compare` reports rich TTFB, throughput, failure, and goodput diagnostics;
it is not the official winner decision. Requests within one run are correlated,
so comparisons use independently reset runs as the statistical unit.

The trusted evaluator builds the round manifest once from the exact nine
control artifact sets. The worker stores its exact bytes and digest before it
can distribute that same manifest with each set of nine candidate artifacts to
`score`:

```bash
sim-latency score-baseline \
  --run "$BASELINE_RUNS" --stderr "$BASELINE_STDERR" \
  --accounting "$BASELINE_ACCOUNTING" --samples "$BASELINE_SAMPLES" \
  --resource-report "$BASELINE_RESOURCES" --marker "$BASELINE_MARKERS" \
  --round-id "$ROUND_ID" --takeover-margin "$TAKEOVER_MARGIN" \
  --out round-baseline.json

sim-latency score \
  --run "$CANDIDATE_RUNS" --stderr "$CANDIDATE_STDERR" \
  --baseline round-baseline.json \
  --accounting "$CANDIDATE_ACCOUNTING" --samples "$CANDIDATE_SAMPLES" \
  --resource-report "$CANDIDATE_RESOURCES" --marker "$CANDIDATE_MARKERS" \
  --out score.json
```

Re-scoring the same authenticated bundle is byte-deterministic.

## Reproducibility and safety

- A `providers.yml` fixes the complete fleet, site tree, client arrivals,
  impairments, and random schedules. The hidden live workload is generated from
  a fresh encrypted CSPRNG epoch seed and revealed only after finalization.
- Every replicate starts from fresh PostgreSQL and Redis state. Reusing
  reliability history makes runs dependent and invalidates the variance model.
- The worker reruns the complete host self-check after every job. Any change to
  the qualified hardware, kernel/microcode, CPU policy, evaluator image, local
  leaves, workload, or scorer identity stops the queue; it never silently
  redraws the epoch control to accommodate drift.
- `FindProviders2` stats must cover at least the frozen fraction of the measured
  window and join to the exact workload identities.
- Candidate networking is internal-only; scoring runs with no network.
- Resource limits are enforced on the whole Compose job, not just the simulator
  PID. CPU and memory bombs are expected inputs and cleanup remains on the two
  management CPUs.
- The versioned [`baseline/`](baseline/README.md) snapshot is immutable. Add a
  new version for later evidence; never edit `baseline/v1` or place credentials
  or an unrevealed seed there.
