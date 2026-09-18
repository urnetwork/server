# Sim-latency season agent runbook

`run-main.sh` is the fail-closed agent harness for the six-epoch competition.
It drives the continuously deployed main API and one-shot worker while keeping
the complete evaluator source graph—`server`, `connect`, `sdk`, `proxy`,
`glog`, `goidenticons`, `userwireguard`, `sn`, `operator-proxy`, and `warp`—isolated
in temporary clones. Every repository is pinned per epoch on its `sim-latency` branch.
Neither evaluation nor promotion changes the operator's product checkouts.

## Agent model roles

Use Terra (`gpt-5.6-terra`) with medium reasoning for all test execution,
including preflight, submission validation, post-promotion smoke tests, and
reruns. A test failure or suspected flake must be handed to Astra (`gpt-6-astra`)
with max reasoning to diagnose the root cause, implement the correction, and add
a deterministic regression test. Terra medium then reruns the affected test and
required suite; do not accept an Astra-run test as the independent completion
result.

Astra with max reasoning owns code evaluation, each winning-submission honesty
and safety code review, the approve/reject decision, every winner promotion,
and all source or config merge/push operations. It must review the exact
materialized patch and score evidence before invoking this harness. Terra must
not approve candidates, merge branches, or push source/config refs. These roles
apply to all six epochs and must survive an agent handoff.

## Before starting

The main environment must contain the reviewed `competition.yml` config and
vault resources, PostgreSQL/Redis migrations, MinIO retention and replication,
and the Grafana alert route. Port-forward PostgreSQL and Redis to localhost when
the worker is run from this host. Set an exact control-plane image identity in
`WARP_IMAGE_DIGEST`; this is recorded per evaluation but is not a scoring input.

Every epoch checkpoint in `config/main/sim-latency.yml` must list exactly these
ten repositories: `server`, `connect`, `sdk`, `proxy`, `glog`,
`goidenticons`, `userwireguard`, `sn`, `operator-proxy`, and `warp`. Each commit
must be reachable from that repository's remote `sim-latency` branch, and the active epoch commit must
be the branch head before promotion begins. Never fill a missing dependency
from a local checkout, its default branch, or the evaluator builder's current
`HEAD`; those sources are deliberately excluded from the evaluation identity.
Before staging, each repository must also expose a `sim-latency-staging` branch
whose head exactly equals its epoch-zero commit. The harness verifies all ten
remote aliases before it creates, evaluates, or advances a staging epoch.
Staging branches never receive a winner or advance automatically; they isolate
the name used for pre-production coordination while retaining the exact frozen
baseline source content and evaluator protocol. A reviewed evaluator repair is
a separate release operation at a drained staging-round boundary, not a
winner promotion or a change to a round already accepting/evaluating work.

The current server module graph requires `operator-proxy` and `warp` in addition
to the earlier eight repositories. Current tools reject incomplete checkpoints
and source locks; they never infer the two missing commits. Prepare a reviewed
complete checkpoint, matching remote aliases, and a new evaluator image at the
drained release boundary. Preserve old eight-repository images, locks, baseline
artifacts, and round evidence as historical records; do not rewrite them to
claim the new source identity.

Create a mode-0600 file containing the operator bearer token, then export:

```sh
export SIM_LATENCY_OPERATOR_TOKEN_FILE=/secure/path/operator.token
export SIM_LATENCY_REVIEWER_ID=agent-reviewer-id
export SIM_LATENCY_STATE_DIR=/var/lib/urnetwork/sim-latency
export WARP_IMAGE_DIGEST=sha256:REPLACE_WITH_THE_RUNNING_WORKER_DIGEST
```

For epoch 1, optionally set `SIM_LATENCY_FIRST_OPENS_AT` to an RFC3339 instant.
Every round is exactly seven days and uses an end-exclusive admission window.
Later rounds open after the previous epoch drains, is reviewed, and is promoted.
The frozen default `SIM_LATENCY_PREPARATION_SECONDS=57600` reserves the 16-hour
same-round host-readiness interval; changing it requires a reviewed
season-policy change before launch. This readiness rebaseline is not the
scoring control. The worker establishes the epoch's one immutable nine-run
`baseline.json` before it builds the first accepted submission.

Run launch preflight and retain its passing JSON before the first epoch. Then:

```sh
cd /home/by/urnetwork/server/connect/sim-latency
./run-main.sh staging
./run-main.sh advance-staging
```

The staging era can contain any number of sequential epochs, beginning at
epoch zero. `staging` creates a 48-hour round by default, or returns the current
scheduled/open/grading round. Share its `round_id` with the Apex integration.
Fee-free staging patches traverse the real FIFO, isolation, evaluation,
scoring, embargo, and authenticated polling paths. Every staging epoch uses
frozen source epoch zero and automatically finalizes with no winner once it
closes and the worker drains all accepted jobs. The default leaderboard remains
production-only; its `include_staging=true` view publishes the finalized
staging epoch with `staging: true` and a null winner for adapter conformance.
It creates no honesty-review, promotion, or production-winner state.

For the polling proof, record both compatibility `state` and additive
`evaluation_status`. Before publication, `state` remains `completed` for both
terminal outcomes, while `evaluation_status` distinguishes `completed` scoring
from terminal `failed` work. A failed response may add only the message-free
reviewed `evaluation_failure` tuple (`kind`, `code`, `retriable:false`); score,
gates, diagnostics, full `eval_error`, and readiness remain embargoed. Absence
of that tuple for an unknown code is valid. A rolling-deployment response with
no `evaluation_status` leaves legacy `completed` outcome-neutral. Neither the
new failure signal nor staging cancellation publishes an epoch early; capture
the finalized polling/leaderboard reconciliation separately.

Staging uses the same append-only one-baseline-per-round score bundle as
production, but it does not require the separately promoted host-readiness
identity used as a production launch gate. Production retains that exact-round
requirement. A staging worker may bootstrap a newly pinned evaluator from the
complete prior root-owned containment record when only the frozen
qualification/image identity is stale. Every new attempt must still pass all
current-image containment, isolation, cleanup, and artifact-integrity gates;
this exception never makes the host production-eligible.

### Evaluator repair between staging rounds

Test the exact frozen source, not only the operator's `main` checkout. The
[epoch-4 G5 investigation](launch/STAGING-4-G5-INCIDENT.md) found that a source
branch omitted a database fix already present in the qualified baseline and
on `main`. Pulling the API/worker cannot repair such an evaluator.

For the approved epoch-5 repair, Astra max owns the source pull/merge and release
review; Terra medium owns independent regression and image validation. Require
the relevant tests to exist and pass in the image build, review all ten
pinned repositories, and retain the new source-lock and image digests. Do not
change active source aliases, configuration, or installed evaluator commands
while epoch 4 is open or draining. After it finalizes, verify the replacement
source/image deployment before creating epoch 5; do not use `advance-staging`
to create that next round against the old pin. Preserve every historical
round's policy and outcomes, and require a fresh production rebaseline before
public launch.

Before qualifying the repaired evaluator, Terra medium must independently
exercise the deterministic regressions for process-group cancellation,
retained-evidence memory budgeting, PostgreSQL/Redis failure detection, terminal
candidate exits, typed cancellation classification, and durable failed-attempt
archival. Astra max reviews both the fixes and their red/green evidence. Keep
the existing G5/G6 thresholds and replicate counts; these are correctness and
containment repairs, not statistical relaxations. Recreate the containment
record for the new evidence-memory boundary instead of reusing the prior host
attestation.

`advance-staging` is the ordinary staging handoff. It atomically closes new
admission without rewriting the published schedule or canceling accepted work.
Before closing, it runs a root-owned evaluator/database preflight; a failure
therefore leaves the round open and retryable. It then runs the worker until
every FIFO submission is terminal, finalizes and reveals the round with no
winner, and creates the next open staging epoch. The command returns only after
the next `round_id` is ready to share. `staging-worker`
remains available when the original admission window should run to its natural
end. Set `SIM_LATENCY_STAGING_WINDOW_SECONDS` to 60 through 604800 seconds when
a different test window is needed. `staging --replace-current` explicitly
cancels a legacy or unwanted current staging epoch and its queued jobs before
creating the next one; it fails closed if any job is running. Starting
production epoch 1 ends the staging era and atomically cancels queued staging
work; it refuses to race an evaluation that is still running.

After the staging admission and polling proof is captured, start the season:

```sh
cd /home/by/urnetwork/server/connect/sim-latency
./run-main.sh run
```

Starting early is safe. The worker begins its 15-second heartbeat immediately,
but the database claim boundary admits no evaluation before `opens_at`. Jobs
submitted before `opens_at` or at/after `closes_at` are terminally discarded.
After close, the worker drains every accepted FIFO job (each bounded to three
hours) and exits. Results remain embargoed until all jobs are terminal and the
review process finalizes the epoch.

## Mandatory candidate review

Exit status 20 means the harness has materialized the next ranked significant
candidate in a private mode-0700 temporary directory. Inspect `candidate.json`,
`score.json`, and `canonical.patch`, and review the patched code for all of the
following:

- no changes to `connect/sim-latency` or any trusted scoring/evaluator input;
- no fabricated measurements, disabled checks, special-cased workload data,
  hidden-seed inference, input gaming, or benchmark detection;
- no filesystem, network, namespace, Docker-socket, host, or secret escape;
- no credential access or exfiltration path;
- no unsafe behavior, persistence, denial of service, or unrelated product
  change; and
- a plausible causal connection between the allowed Go change and the measured
  improvement.

Write a JSON object containing the evidence inspected and an explicit boolean
finding for every item. Keep it mode 0600. If honest and safe:

```sh
./run-main.sh approve --epoch N --job-id ID \
  --evidence /secure/path/review.json \
  --reason 'honest allowed-path optimization; no scoring or sandbox tampering'
```

If any check fails:

```sh
./run-main.sh reject --epoch N --job-id ID \
  --evidence /secure/path/review.json \
  --reason 'specific dishonest or unsafe behavior found'
```

Rejection is append-only and advances to the next ranked significant candidate
without materializing a second temporary directory. The harness pauses again
with status 20; run `./run-main.sh candidate --epoch N` to materialize that
candidate before reviewing it. If candidates are exhausted, it records
a no-winner transition and carries the exact incumbent commits and significance
threshold into the next epoch. Approval authenticates the reviewed score and
patch, clones all ten locked repositories into a new temporary directory,
checks out the frozen `sim-latency` branch heads, applies the winner to the
evaluated server surface, verifies every dependency and the protected runner
tree are unchanged, pushes changed source branches, and pushes the config ledger
last. It then starts the next epoch. Temporary candidate and promotion
directories are deleted after use.

After epoch 6 is reviewed, `run-main.sh` exits zero. A canceled round, missing
source commit, source/API epoch mismatch, failed worker, incomplete drain,
invalid review evidence, failed push, or unavailable dependency exits nonzero
without advancing the source ledger.
