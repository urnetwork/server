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

## Current competition contract

The final environment is defined by the source-epoch ledger at
`config/main/sim-latency.yml`, the evaluator configuration in
`config/competition.yml`, and the authenticated same-round baseline manifest.
The initial calibrated values are:

| Item | Final value |
|---|---|
| Evaluator host | one 128 GiB-class Ubuntu host with 12 isolated CPUs |
| CPU boundary | 10 evaluation CPUs; 2 management/cleanup CPUs |
| Workload | 1,800 providers; 200 client identities; 80 arrivals/minute |
| Selection | quality window 2; 4 exchange hosts; 4 fleet shards |
| Measurement | 180 seconds with provider impairment enabled |
| Aggregation | median of 9 independently reset runs |
| Initial significant-improvement percentage | 16.1% |
| Admission | six seven-day epochs; unbounded paid submissions at $20 each |
| Dispatch | immediate single-job FIFO; Redis list backed by authoritative PostgreSQL |
| Submission deadline | 3 hours total, including attempts, backoff, score, and cleanup |

The 1,800-provider point is the competition target. Larger configurations are
useful only for development stress tests and are not an alternative production
scale.

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
- [`launch/APEX-OPEN-QUESTIONS.md`](launch/APEX-OPEN-QUESTIONS.md) for the shareable partner
  decision and acceptance checklist;
- [`playbook.yml`](playbook.yml) for machine-readable launch status;
- [`baseline/README.md`](baseline/README.md) for preserved calibration evidence;
- [`baseline/final-baseline.html`](baseline/final-baseline.html) for the visual
  baseline report;
- [`../../competition/README.md`](../../competition/README.md) for the API and
  worker implementation; and
- [`../../../sn/api/competition.yml`](../../../sn/api/competition.yml) for the
  public competition API.

Superseded plans, workstation campaigns, preliminary reports, and their helper
programs are retained only under [`old/`](old/README.md).

## Score and statistical significance

For each replicate, the raw score is p95 `total_ms` over requests that start in
the measured window. Failures and incomplete bodies are charged at the frozen
request-timeout ceiling. Lower is better. The candidate and same-round
baseline each contribute nine independently reset replicate scores, aggregated
by median. The displayed normalized score is
`100 * baseline_median / candidate_median`, clamped to 1–200.

A score is placeable only when all G1–G6 correctness, volume, path-integrity,
matchmaking, stability, and resource gates pass. A placeable submission is
takeover-eligible only when all of these are also true:

1. its median raw score is at or below the current source epoch's improvement
   threshold relative to the same-round baseline;
2. a one-sided Welch test over the run-level raw scores has `p <= 0.05`; and
3. the winning variance supports a next-epoch threshold in `(0%, 50%]`.

Every successful score records the baseline and candidate means, sample
variances, observed improvement, current margin, minimum statistically
significant improvement, required improvement, Welch statistic/degrees of
freedom/p-value, and the recommended next-epoch margin. This is part of the
immutable evaluation result, not a post-hoc leaderboard calculation.

Epoch 1 begins from source epoch 0's calibrated 16.1% requirement. Statistical
eligibility places a submission into the ranked honesty-review queue; it does
not by itself make the submission a winner. When an epoch has an approved
winner, its authenticated `score.json` sets the next source epoch's
percentage to the scorer's recommendation, which never weakens the incumbent
margin. If no submission is statistically significant, or every significant
candidate is rejected as dishonest, the epoch has no winner and both the
source commits and percentage carry forward unchanged.

## Epoch source identity and promotion

Only the repositories that can affect the measured product are frozen by
source epoch: `connect`, `sdk`, `server`, and `proxy`, all on branch
`sim-latency`. The trusted API and worker continue on `main`; they record their
runtime image digests per evaluation but their source commits are not scoring
inputs.

Round N evaluates source epoch N-1. Before a measured command touches services,
the tool verifies the disposable evaluation checkouts against the authenticated
source lock and ledger. Operator worktrees are not evaluation inputs and need
not be on any particular branch. A manual check can target an explicitly
prepared disposable repository root:

```bash
./build/$(go env GOOS)/$(go env GOARCH)/sim-latency \
  source-check --epoch 0 --repos-root /tmp/evaluation-source --json
```

After round N closes and every evaluation is terminal, the worker exits with
any significant results still embargoed. The trusted agent harness enumerates
the exact ranked candidate into a fresh mode-0700 directory:

```bash
./run-main.sh epoch-review --epoch N next

# After inspecting candidate.json, score.json, and canonical.patch, append one
# immutable decision. Rejection returns/materializes the next ranked candidate.
./run-main.sh epoch-review --epoch N reject \
  --job-id CANDIDATE_JOB_ID --reviewer HONESTY_AGENT_ID \
  --reason 'score-path tampering detected' --evidence honesty-report.json

# Approval atomically finalizes this exact job as the winner.
./run-main.sh epoch-review --epoch N approve \
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
./run-main.sh promote --epoch N \
  --winner /restricted/winner-N --winner-job-id WINNING_JOB_ID

# No significant candidate, or all significant candidates rejected.
./run-main.sh promote --epoch N --no-winner
```

Promotion creates one temporary root, clones `connect`, `sdk`, `server`, and
`proxy` into it, checks out each `sim-latency` branch at the prior epoch, applies
and commits the winner there, pushes measured branches first, and activates the
new config ledger commit last. Before staging, promotion queries the finalized
round and requires the exact approved job, canonical patch SHA-256, and score
significance record; unevaluated repository patches are rejected. `--dry-run`
performs the same validation without publishing. The next evaluator image and
round baseline are built only after the new source epoch is active.

## Competition lifecycle

Submissions are admitted throughout each seven-day window and evaluated as
soon as possible in exact FIFO order. Closing an epoch stops admission but does
not stop accepted work: the worker continues until the entire backlog is
terminal, then seals the deterministic significant-candidate ranking and exits.
Scores, failures, hidden seed, workload, and leaderboard remain embargoed while
the agent harness reviews the candidates one by one. Approval of the first
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

`run-main.sh` is a trusted operator/development wrapper for a local checkout
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

The trusted evaluator builds a same-round manifest from the exact nine baseline
artifact sets, then supplies that signed manifest and nine candidate artifact
sets to `score`:

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
- `FindProviders2` stats must cover at least the frozen fraction of the measured
  window and join to the exact workload identities.
- Candidate networking is internal-only; scoring runs with no network.
- Resource limits are enforced on the whole Compose job, not just the simulator
  PID. CPU and memory bombs are expected inputs and cleanup remains on the two
  management CPUs.
- The versioned [`baseline/`](baseline/README.md) snapshot is immutable. Add a
  new version for later evidence; never edit `baseline/v1` or place credentials
  or an unrevealed seed there.
