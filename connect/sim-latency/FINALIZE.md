# Apex integration finalization plan

Status date: 2026-08-14

This plan takes `sim-latency` from a working local benchmark to a launch-ready
Apex competition evaluation system. It is the execution plan for the
`ur_latency` design in [`../../../apex/PLAN.md`](../../../apex/PLAN.md).

The simulator itself is substantially built. The remaining work is to freeze
the Apex scoring contract, close correctness and anti-gaming gaps, qualify the
harness on official hardware, build the scoring/evaluation service, and pass
the Macrocosmos staging handoff.

## Current state

Already implemented and not to be rebuilt:

- Seeded `providers.yml` generation and reproducible site/client/fleet load.
- In-process API, exchange hosts, reliability pipeline, fake site, and
  shardable provider fleet.
- Warm-client pool, reliability prewarm, latency/speed tests, impairment,
  churn, provider flow caps, and reset support.
- Per-request CSV plus `run.json`, `analyze`, `baseline`, and `compare`.
- FindProviders2 protobuf samples, local segment storage, MinIO upload/export,
  and sample diff tooling.
- The negative-WaitGroup teardown fix described in `EVALUATION2.md`.
- Passing package tests as of the status date:

  ```text
  go test -count=1 ./connect/sim-latency
  go test -count=1 ./stats
  ```

Existing evaluation data is directional only. It was collected on a
workstation and a modified build, not on the official Apex hardware, and does
not satisfy the Apex calibration requirements.

## Finalization principles

1. `sim-latency` is immutable competition infrastructure. Submitted patches
   must never be allowed to modify it, the scorer, accounting, stats export,
   migrations, SDK simulation helpers, or build/dependency metadata.
2. Every value affecting a score must be pinned and recorded in an evaluation
   manifest: base commit, patch hash, API image digest, providers file hash,
   run flags, hardware identity, kernel/tuning, and scorer version.
3. Scoring fails closed. Missing artifacts, incomplete responses, missing
   accounting, missing samples, a panic, an OOM, or an unclean teardown must
   produce a typed evaluation error rather than a placeable score.
4. Identical patch bytes in one round must return one cached evaluation result.
   Re-submission must not provide another draw from machine noise.
5. A 1% takeover threshold is not acceptable until measured noise supports it.
   Calibration, not schedule, decides the run scale, duration, and replicate
   count.

## Phase 0: freeze the competition contract

Owner: UR engineering with Macrocosmos confirmation.

- [ ] Resolve the scoring mismatch between the Apex design and the current
  simulator:

  - The Apex design specifies one lower-is-better scalar: p95 `total_ms`, with
    failed or incomplete requests charged at the request-timeout ceiling.
  - The current simulator verdict uses `ttfb_p95_ms` and
    `throughput_p50_bytes_per_s` as primary metrics and computes timing metrics
    over successful requests only.

  Unless explicitly changed in a signed-off scoring specification, implement
  the existing Apex design. Retain the current metrics as diagnostics and
  anti-regression gates; do not silently substitute the `compare` verdict for
  Apex's scalar score.

- [ ] Write `APEX-SCORE-SPEC.md` and version it as `score_schema: 1`. It must
  define, without implementation-dependent language:

  - measured-window inclusion rules;
  - success, failure, incomplete-body, and short-body semantics;
  - the request-timeout ceiling used for failed observations;
  - the exact quantile convention;
  - G1-G6 formulas and inclusive/exclusive thresholds;
  - treatment of missing/empty inputs and NaN/Inf;
  - baseline aggregation and candidate replicate aggregation;
  - normalizer formula and clamping;
  - all diagnostics returned to Apex and when they become visible;
  - typed submission errors versus infrastructure errors.

- [ ] Confirm whether a crawl canceled at the two-minute crawl deadline creates
  only rows for requests actually attempted or also synthetic failure rows for
  undiscovered descendants. Keep the volume gate consistent with that choice.
- [ ] Confirm the initial takeover threshold and whether scoring uses one run,
  a median of runs, or a paired candidate/base design. Treat this as provisional
  until Phase 4 calibration passes.

Exit criteria:

- `APEX-SCORE-SPEC.md` is reviewed by UR engineering.
- Macrocosmos confirms that the scalar, timeout, submit/poll flow, hidden-seed
  commitment, and evaluation duration fit the platform contract.
- The spec has no unresolved scoring semantics.

## Phase 1: close simulator correctness gaps

### 1.1 Incomplete HTTP bodies

`ClientDriver.fetch` currently ignores `ReadBytes` and `io.Copy` errors and
records the HTTP response status. A truncated response can therefore appear as
a successful `200` row.

- [ ] Validate the leading JSON line before recording the row.
- [ ] Compare the received body length with the page's declared `size` and the
  response `Content-Length` when present.
- [ ] Treat any header read error, body read error, invalid page header, or byte
  mismatch as an incomplete request under the score specification.
- [ ] Keep the existing CSV columns stable unless the score specification
  requires an explicit `expected_bytes` or `complete` column. If a column is
  added, bump the artifact schema and keep legacy readers working.
- [ ] Apply the same completeness check to warm-up requests so a partial body
  cannot establish a supposedly healthy warm client.
- [ ] Add tests for a complete response, truncated header, truncated body,
  incorrect content length, read error, non-200 response, and cancellation.

### 1.2 Evaluation lifecycle

- [ ] Ensure TERM cancels ramp, prewarm, settle, warm-client construction, and
  the measured window promptly; the runner may still use TERM followed by KILL
  as a final containment boundary.
- [ ] Join fleet subprocesses and all stats/client drain goroutines before
  reporting a successful evaluation.
- [ ] Make lingering child processes, a missing `run.json`, or a failed stats
  flush a typed incomplete-evaluation result.
- [ ] Add an evaluation/run identifier to logs and artifacts so DB accounting,
  stats samples, CSV, stderr, and resource reports cannot be mixed across jobs.

### 1.3 Artifact completeness

- [ ] Extend the run manifest or sidecar with every scorer input that is not
  derivable from the CSV: request timeout, score schema, scorer version,
  expected run flags, stats root, completion state, and resource-report path.
- [ ] Record the exact build revision and reject modified or unpinned builds in
  official mode.
- [ ] Add a machine-readable final marker only after CSV flush, sidecar write,
  stats close, accounting snapshot, and child teardown have completed.

Exit criteria:

- All new unit and fault-injection tests pass.
- A deliberately truncated response cannot be scored as successful.
- An interrupted run cannot produce a placeable artifact set.

## Phase 2: implement the official scorer

Prefer a Go `sim-latency score` subcommand so it reuses the benchmark's parsers
and can be shipped to miners as the exact official scorer. Keep evaluation
orchestration outside the scorer.

- [ ] Add a command that accepts the run CSV/sidecar, stderr, baseline manifest,
  DB accounting snapshot, FindProviders2 sample directory, and resource report.
- [ ] Emit one versioned JSON document containing:

  ```json
  {
    "score_schema": 1,
    "raw_score": 0,
    "placeable": false,
    "gates": {},
    "diagnostics": {},
    "eval_error": null
  }
  ```

- [ ] Implement the raw score exactly as frozen in `APEX-SCORE-SPEC.md`.
- [ ] Implement and test every gate:

  - **G1 success:** success rate is at least 97% and no more than one percentage
    point below the same-round baseline.
  - **G2 volume:** measured-window request count and received bytes are each
    within the specified band of the baseline.
  - **G3 path integrity:** immutable server-side provider-egress/accounting bytes
    cover at least 95% of client-received bytes.
  - **G4 matchmaking:** samples exist, candidate pools are non-empty, and p95
    FindProviders2 `load_millis` is within the allowed baseline multiplier.
  - **G5 stability:** the run completed with no panic, restart, fatal recovery,
    missing service, or unclean drain.
  - **G6 resources:** the cgroup/resource report shows no OOM, hard kill, limit
    escape, or missing measurement.

- [ ] Fail closed when any mandatory input is absent or malformed.
- [ ] Keep scorer output deterministic for identical artifact bytes.
- [ ] Add golden tests for baseline, genuine improvement, regression, all six
  individual gate failures, multiple simultaneous failures, empty CSV, legacy
  log noise, malformed samples, NaN/Inf, and boundary values.
- [ ] Add a CLI-level parity test comparing the locally shipped scorer with the
  scorer invoked by the evaluation worker.

Exit criteria:

- The scorer has a stable JSON schema and golden fixtures.
- A baseline run normalizes to the agreed nonzero display score.
- No invalid or incomplete run returns `placeable: true`.

## Phase 3: qualify scale and freeze the official run

All qualification must run on the exact official hardware: two identical
circa-2017 Xeon machines, 12 cores, 128 GB ECC RAM, Ubuntu 24.04, with fixed
microcode, kernel, SMT/NUMA placement, turbo state, and governor.

### 3.1 Find the egress-establishment frontier

- [ ] Sweep provider count, warm-client pool size, arrival rate, multi-client
  window size, exchange host count, and fleet shard count.
- [ ] Run every promising point with impairment enabled and `--no-impair`.
- [ ] Measure warm-client establishment, empty-market events, request failures,
  auth/contract timeouts, CPU, RSS, socket pressure, Postgres/Redis latency, and
  teardown health.
- [ ] Fix the window-auth capacity issue if a small, production-valid change
  removes it. Otherwise select a round scale safely below the measured frontier.
- [ ] Do not assume the proposed 15,000-provider/600-client round scale or the
  100,000-provider final scale is viable until measured.

Frontier exit gate:

- 20 consecutive clean baseline evaluations at the selected round scale.
- 100% of the expected warm-client pool established, or a separately frozen
  threshold justified by the scoring contract.
- No empty market or establishment anomaly.
- No `ENOBUFS` during the measured window.
- Baseline steady-state CPU at or below approximately 65% of the 12-core budget.
- Stable Postgres, Redis, socket, memory, and teardown behavior.

### 3.2 Freeze `official-run.sh`

- [ ] Add `official-run.sh` and a runbook that pin:

  - base/scorer revisions and clean-build enforcement;
  - provider/client scale and all sim flags;
  - cgroup CPU/memory/PID limits;
  - CPU governor, turbo, NUMA, SMT, and affinity;
  - file descriptor, ephemeral port, and socket settings;
  - template-DB restore and Redis reset procedure;
  - per-job site/stats directories and ports;
  - build/test commands and offline dependency behavior;
  - TERM/KILL deadlines and cleanup checks;
  - artifact paths and checksums.

- [ ] Have the script produce an immutable evaluation manifest and a complete
  artifact bundle: patch, CSV, stderr, sidecar, scorer JSON, accounting snapshot,
  stats samples, resource report, and hashes.
- [ ] Publish all non-secret run-spec fields through the scoring API `/info`
  endpoint and the miner README.

Exit criteria:

- A fresh machine built from the runbook reproduces a valid baseline bundle.
- Re-running the same bundle through the scorer produces byte-identical JSON.

## Phase 4: pass the noise and separability launch gate

The directional workstation result for `total_p95_ms` has approximately 5%
run-to-run CV and a one-run minimum detectable change around 12.5%. The planned
Apex gate is at most 0.25% run noise for a 1% takeover margin. The existing data
therefore does not support single-run 1% takeovers.

- [ ] On the production box, run at least 20 clean repetitions of one seed to
  measure `sigma_run` under the final scorer.
- [ ] Run the baseline once on at least 20 independent CSPRNG seeds to measure
  round-to-round spread and identify pathological mixtures.
- [ ] Create three pinned reference submissions:

  - no-op baseline patch;
  - deliberately worse patch;
  - plausibly better production-valid patch.

- [ ] Verify the references rank in the intended order on at least 19 of 20
  seeds.
- [ ] Confirm baseline CPU headroom and wall time at the chosen scale.
- [ ] If `sigma_run` exceeds one quarter of the takeover margin, test in order:

  1. improved host quiescing and state isolation;
  2. longer measured windows;
  3. interleaved or paired baseline/candidate runs;
  4. median-of-R candidate evaluations;
  5. a more stable scalar consistent with the success statement;
  6. a larger takeover threshold negotiated with Macrocosmos.

- [ ] Select the cheapest design that actually passes; do not declare R=1 or
  R=3 in advance.
- [ ] Write `APEX-CALIBRATION.md` with raw measurements, convergence, selected
  scale/duration/R, minimum detectable delta, reference separability, resource
  headroom, expected cost, and timeout justification.

Exit criteria:

- `sigma_run <= 0.25 * takeover_margin` for the final scalar and aggregation.
- Reference separability passes on at least 19/20 seeds.
- Evaluation wall time fits the agreed platform timeout.
- Macrocosmos accepts the written sizing justification.

## Phase 5: build the secure evaluation service

This work belongs with the Apex service artifacts, but it is required before
the simulator integration is complete.

### 5.1 Infrastructure and job runner

- [ ] Provision two identical, dedicated, monitored evaluation machines.
- [ ] Run one evaluation at a time per box through a FIFO queue.
- [ ] Restore a migrated template database and reset Redis before every run.
- [ ] Enforce cgroups, wall-clock deadlines, process cleanup, disk quotas, and
  artifact retention for the full season.
- [ ] Cache by `(round_id, canonical_patch_hash)` and return cached results for
  identical submissions.
- [ ] Provide box self-check and same-round re-baseline jobs before failover.

### 5.2 Patch pipeline and containment

- [ ] Pin and tag the public server `BASE_SHA` as `apex-season-1`.
- [ ] Produce an exact file allowlist after reviewing the import graph. Keep
  simulator, SDK harness, scorer, stats, accounting, contracts/payment,
  migrations, generated files, build tags, `go.mod`, `go.sum`, vendor, and CI
  outside it.
- [ ] Parse unified diffs structurally; reject path traversal, binary patches,
  symlink/submodule changes, renames outside the allowlist, mode changes, and
  oversized submissions.
- [ ] Re-enforce all checks server-side even if the Apex screener accepted the
  patch.
- [ ] Build offline with a per-job cache and no production credentials.
- [ ] Run `go vet`, sim-latency tests, and the frozen contract-test package set
  before the expensive evaluation.
- [ ] Execute on a default-deny network with only the scoring control-plane path
  permitted. Make the host disposable and rebuildable from its image.
- [ ] Complete the security checklist and a no-secrets audit.

### 5.3 Scoring API

- [ ] Implement authenticated `/healthz`, `/readyz`, `/info`,
  `/generate-round`, `/score`, and `/score/{job}` endpoints.
- [ ] Store round seeds privately, generate `providers.yml`, publish its SHA-256
  commitment, and reveal the seed/file only after the round.
- [ ] Compute the same-round baseline using the calibrated replicate policy.
- [ ] Keep submission faults as typed 4xx responses and infrastructure faults as
  retriable 5xx responses.
- [ ] Return only active-round-safe diagnostics; release detailed artifacts at
  the agreed reveal time.
- [ ] Build and publish a digest-pinned image plus OpenAPI document.

Exit criteria:

- A hostile patch cannot reach secrets, the internet, other jobs, immutable
  score inputs, or the host outside its disposable boundary.
- Queue, cache, failure, retry, cleanup, and failover behavior have integration
  tests.
- The API image and all dependencies are pinned by digest/version.

## Phase 6: produce the Apex competition package

- [ ] Create the no-op baseline and the two calibration reference patches.
- [ ] Create and contract-test:

  - `models.py` with no public seed field;
  - `generator.py` calling `/generate-round`;
  - submit-and-poll `runner.py`;
  - structural `screener.py`;
  - NaN/Inf-safe `normalizer.py`;
  - registry entry proposal.

- [ ] Write the miner README with:

  - success statement and editable surface;
  - exact patch format and allowlist;
  - scoring formula and every gate;
  - official hardware and run spec;
  - local iteration/scoring commands;
  - error-code table;
  - fee, round length, reveal delay, and licensing;
  - season end and grand-final rules;
  - warning that relative improvement, not absolute modern-hardware latency,
    transfers to official evaluation.

- [ ] Adapt and pass the Apex contract-test template:

  - round generation and seed rotation;
  - same patch/round cache identity;
  - malformed patch rejection;
  - valid degenerate patch handling;
  - baseline score greater than zero;
  - API error classification;
  - runner polling and timeout behavior.

- [ ] Run the Apex local round harness end to end and preserve its verbatim
  passing output.
- [ ] Spend a focused adversarial probe day on tunnel bypass, partial-body/tail
  dropping, load starvation, sample suppression, matchmaking-cost hiding,
  accounting manipulation, impairment evasion, clock/window manipulation,
  cache-key confusion, and artifact-path injection. Turn every successful probe
  into a test plus a gate or allowlist restriction.

Exit criteria:

- Every required file in the Apex handoff manifest exists and is pinned.
- Contract tests, local harness, and adversarial probes are green.
- The miner can reproduce scorer output from a local artifact bundle.

## Phase 7: handoff, staging, and launch

- [ ] Resolve with Macrocosmos:

  - long-running submit/poll evaluations and timeout limits;
  - partner-hosted execution of miner Go patches;
  - hidden seed plus public commitment/reveal;
  - post-hoc invalidation of fraudulent leaders and emissions;
  - final fee, incentive weight, and reveal delay.

- [ ] Assign the on-call rota and new-leader review owner for the full season.
- [ ] Fill every section of `../../../apex/skills/apex-competition-builder/HANDOFF.md`,
  including image digest, OpenAPI, pins, sizing evidence, security answers,
  contact, and auth handover.
- [ ] Run a staging round through the real path:

  ```text
  generate -> submit -> screen -> queue -> patch -> build -> test -> reset
           -> simulate -> score -> normalize -> cache -> reveal
  ```

- [ ] Fix review findings and repeat staging until the baseline and a valid
  candidate complete without manual intervention.
- [ ] Enable alerts for queue depth, baseline drift, wall time, gate failures,
  box/resource health, Postgres/Redis health, artifact storage, and API errors.
- [ ] Publish the miner README, announcement, season dates, and grand-final
  rules before accepting submissions.

Launch gate:

- [ ] Phase 0 score contract signed off.
- [ ] Partial/incomplete responses cannot score as successes.
- [ ] Official scorer and all G1-G6 tests pass.
- [ ] Twenty consecutive frontier runs pass at round scale.
- [ ] Noise and reference-separability gates pass.
- [ ] Adversarial probe findings are closed.
- [ ] Scoring API, runner, cache, containment, monitoring, and failover are live.
- [ ] Apex contract tests and local harness pass.
- [ ] Full staging round passes from submission through reveal.
- [ ] Handoff is accepted and on-call ownership is active.

## Grand-final readiness

The following may complete after initial launch, but the fallback and rules must
be published before launch:

- [ ] Qualify the two-box topology: services/clients on box 1 and standalone
  fleet shards on box 2.
- [ ] Establish the maximum clean full-scale configuration. Target 100,000
  providers and 1,000 arrivals/minute; if unsustainable, publish the measured
  maximum rather than claiming the target.
- [ ] Re-run a same-scale baseline and verify every finalist against it.
- [ ] Automate five held-out seeds with the calibrated replicate policy,
  engineering tests, artifact publication, and deterministic final ranking.

## Required artifacts

The integration is complete only when these artifacts exist:

| Artifact | Expected location/owner |
|---|---|
| `APEX-SCORE-SPEC.md` | this directory |
| `official-run.sh` and runbook | this directory |
| official scorer + golden tests | this package |
| `APEX-CALIBRATION.md` | this directory |
| pinned base tag and exact patch allowlist | server/Apex release config |
| baseline and good/bad reference patches | Apex package |
| scoring API image and OpenAPI | Apex service |
| `models.py`, `generator.py`, `runner.py` | Apex handoff package |
| `screener.py`, `normalizer.py`, registry entry | Apex handoff package |
| miner README | public Apex competition package |
| contract-test and local-harness output | Apex handoff package |
| completed `HANDOFF.md` | Apex handoff package |
| staging-round artifact bundle | season archive |

## Definition of done

The Apex integration is finalized when a miner can create an allowlisted patch
against the pinned public base, reproduce the official scoring method locally,
submit through Apex, and receive a cached, statistically defensible score from
an isolated UR evaluation box; invalid or adversarial runs fail closed; a fresh
round rotates and commits to hidden workload data; the complete staging path has
passed; and monitoring, failover, review, and handoff ownership are operating.
