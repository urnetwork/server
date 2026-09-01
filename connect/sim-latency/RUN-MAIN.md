# Sim-latency season agent runbook

`run-main.sh` is the fail-closed agent harness for the six-epoch competition.
It drives the continuously deployed main API and one-shot worker while keeping
the measured `connect`, `sdk`, `server`, and `proxy` source isolated in
temporary clones. Neither evaluation nor promotion changes the operator's
product checkouts.

## Before starting

The main environment must contain the reviewed `competition.yml` config and
vault resources, PostgreSQL/Redis migrations, MinIO retention and replication,
and the Grafana alert route. Port-forward PostgreSQL and Redis to localhost when
the worker is run from this host. Set an exact control-plane image identity in
`WARP_IMAGE_DIGEST`; this is recorded per evaluation but is not a scoring input.

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
same-round baseline interval; changing it requires a reviewed season-policy
change before launch.

Run launch preflight and retain its passing JSON before the first epoch. Then:

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
patch, clones all four repositories into a new temporary directory, checks out
the frozen `sim-latency` branch heads, applies the winner, verifies the protected
runner tree is unchanged, pushes the product branches, and pushes the config
ledger last. It then starts the next epoch. Temporary candidate and promotion
directories are deleted after use.

After epoch 6 is reviewed, `run-main.sh` exits zero. A canceled round, missing
source commit, source/API epoch mismatch, failed worker, incomplete drain,
invalid review evidence, failed push, or unavailable dependency exits nonzero
without advancing the source ledger.
