# Official Apex evaluation runbook

This is the operator contract for [`official-run.sh`](official-run.sh) and
`score_schema: 1`. The script is deliberately fail-closed and has no scale,
duration, replicate-count, kernel, or takeover defaults. Production
calibration is preserved under [`baseline/`](baseline/README.md); the current
source epoch and significant-improvement percentage are frozen in
`config/main/sim-latency.yml`, and the same-round manifest freezes the exact
workload and replicate policy.

Set `APEX_CALIBRATION_ACCEPTED=yes` only after the worker authenticates the
selected epoch, evaluator image, host facts, local-leaf hashes, and promoted
same-round baseline. Macrocosmos acceptance is a separate public-launch action
tracked in [`PLAYBOOK.md`](PLAYBOOK.md); it does not alter score calculation.

## Immutable components

Keep three images/artifact identities distinct:

1. `BASE_SHA` is the public season base, tagged `apex-season-1`.
2. The per-job candidate image is the digest-pinned base plus the structurally
   screened patch, committed as a deterministic ephemeral commit by the fixed
   submission Dockerfile. Its SHA is `APEX_BUILD_SHA`; it has no tracked,
   staged, or untracked changes. The worker pins the canonical diff and the
   `(base image, patch, policy, builder)` image key independently.
3. `APEX_SCORER_BIN` comes only from the pristine base image and is checked
   against `APEX_SCORER_SHA256` before every networkless scoring stage. It is
   never copied from or mounted out of a candidate image.

The server-side patch allowlist MUST exclude this directory, `stats/`, all
accounting code, migrations, generated files, SDK simulation helpers,
`go.mod`, `go.sum`, build tags, vendor/cache data, CI, the runner, and scorer.
The evaluation service structurally parses the unified diff and re-enforces the
allowlist after applying it. A textual path grep is not sufficient.

Build the reviewed season base once, then derive a candidate with dependency
networking disabled and the pre-populated module cache:

```bash
./connect/sim-latency/evaluator/container/build-base.sh --epoch 0 --tag registry/base:season-1
./connect/sim-latency/evaluator/container/build-submission.sh \
  --base-image registry/base@sha256:<digest> \
  --patch /job/canonical.patch --policy /job/policy.json
```

The fixed build runs offline `go vet`, compiles every `connect/...` test
package, builds the simulator with `GOPROXY=off`/`GOSUMDB=off`, and requires
`go version -m` to report the exact candidate VCS revision and
`vcs.modified=false`. Service-backed tests run later with fresh Compose
PostgreSQL/Redis. The official run repeats the binary check.

## Qualified host image

The single authoritative evaluator must be a circa-2017 Xeon host with exactly
12 physical CPUs exposed, at least 128 GB ECC RAM, and Ubuntu 24.04. Ten CPUs are
the evaluation set; two are the management set and are never exposed to an
untrusted build or scored workload. Pin and record:

- BIOS revision and hardware inventory id;
- microcode package/revision and exact kernel release;
- SMT state, NUMA topology and memory policy;
- turbo state and `performance` governor;
- isolated CPU/IRQ/housekeeping placement and job affinity;
- THP, swap, overcommit, and dirty-page settings;
- `nofile`, PID, ephemeral-port, listen-backlog, and socket-buffer settings;
- Postgres, Redis, container runtime, and API image versions/digests.

Store the expected values in the host image and make its self-check compare
actual values byte-for-byte before enabling `APEX_CALIBRATION_ACCEPTED=yes`.
At minimum the runner itself checks kernel release, CPU count, RAM, file
descriptors, cgroup-v2 membership, workload hash, scorer hash, build revision,
and calibration acceptance. The evaluation service checks the remaining image
facts and records their signed self-check report in the season archive.

Install the repository CPU/resource-boundary and IRQ controls with
`competition/install-authoritative-host-controls.sh --install`. Both systemd
units must be enabled, must order before Docker, and must pass again after a
reboot. Install the reviewed `competition/docker-daemon.example.json` bytes as
root-owned `/etc/docker/daemon.json` during a controlled restart. Readiness
authenticates that file and a live candidate-image UID/GID mapping; a claimed
`userns-remap` setting with an identity mapping is not sufficient.

Recommended file-descriptor target is 1,048,576. Socket/sysctl values are not
specified here because the frontier campaign must freeze the exact values; do
not copy workstation tuning into production without measuring it.

## Per-job containment

Run exactly one evaluation at a time on the host through the FIFO worker. Before
each replicate:

1. Create a unique Compose project below a root-owned per-job cgroup parent.
   Reject reuse of an artifact directory or project identity.
2. Start fresh tmpfs PostgreSQL/Redis containers from pinned digests. Run the
   trusted schema initializer against PostgreSQL and authenticate its migration
   hash before admitting a baseline or candidate runner.
3. Verify the dedicated Redis generation is empty. Never attach a shared or
   production backing service to the project.
4. Allocate the fixed API/exchange ports. One-job-at-a-time is what makes the
   currently fixed exchange port range safe.
5. Start the authenticated candidate image as UID/GID 65532 with a read-only
   root; bind only the policy-hashed host `config/local` and `vault/local`
   leaves plus authenticated input read-only, and a fresh site/stats/artifact
   directory read-write. Never bind either parent or any all/main/site tree.
6. Apply the calibrated CPU, memory, PID, IO, affinity, and wall-clock limits.
   The runner is capped at 72 GiB, PostgreSQL at 16 GiB, and Redis at 8 GiB;
   host capacity must leave at least 24 GiB outside those active ceilings. The
   worker and cleanup path stay on the two management CPUs while all untrusted
   execution stays on the ten evaluation CPUs.
   The run bridge is internal-only with no published ports; only the ephemeral
   data services are reachable. The later pristine scorer has
   `network_mode: none`. No production credentials enter either namespace.
7. Launch `official-run.sh run` as the Compose runner command. TERM is
   delivered at the wall deadline; KILL is the final containment boundary after
   `APEX_KILL_AFTER`. Any required KILL is non-placeable.
8. After all child processes are reaped, snapshot accounting and cgroup data as
   described below. Then invoke the scorer only when every replicate in the
   signed round manifest exists.

The common parent includes runner, PostgreSQL, Redis, provider shards, and all
container/helper processes. Docker owns the child cgroups; the worker verifies
the live hierarchy and aggregates every child counter. Sampling only the
runner PID or trusting rendered Compose YAML is invalid.

Before host qualification, run
`connect/sim-latency/evaluator/container/test-resource-bomb-cleanup.sh`.
It must observe a CPU bomb saturating every evaluation core, an OOM-killed
no-swap memory bomb, management-affined cleanup within 10 seconds, and zero
residual labeled containers or networks.

## Required environment

The worker supplies these values; none comes from a submission:

```text
APEX_CALIBRATION_ACCEPTED=yes
APEX_BASE_SHA=<40 hex>
APEX_BUILD_SHA=<40 hex deterministic candidate commit>
APEX_SIM_BIN=<absolute candidate binary path>
APEX_SIM_SHA256=<64 hex candidate binary digest>
APEX_SCORER_BIN=<absolute read-only scorer path>
APEX_SCORER_SHA256=<64 hex>
APEX_PROVIDERS_FILE=<absolute hidden per-round providers.yml path>
APEX_PROVIDERS_SHA256=<published commitment, 64 hex>
APEX_PATCH_FILE=<absolute canonical patch path>
APEX_PATCH_SHA256=<canonical patch, 64 hex>
APEX_ARTIFACT_ROOT=<absolute per-season artifact root>
APEX_EVALUATION_ID=<unique [A-Za-z0-9._-]+ id>
APEX_EPOCH=<measured source epoch 0..6>
APEX_API_IMAGE_DIGEST=sha256:<64 hex>
APEX_HARDWARE_ID=<inventory id>
APEX_HOST_QUALIFICATION_SHA256=<64 hex live host-fact digest>
APEX_KERNEL_RELEASE=<uname -r>
APEX_MICROCODE_REVISION=<live unique /proc/cpuinfo value>
APEX_CPU_COUNT=10

APEX_DURATION=<calibrated duration>
APEX_REQUEST_TIMEOUT=<frozen ceiling>
APEX_RAMP=<calibrated ramp>
APEX_PREWARM=<frozen reliability lookback>
APEX_SETTLE=<calibrated settle>
APEX_CLIENT_WARMUP_TIMEOUT=<calibrated complete-pool deadline>
APEX_FLEET_SHARDS=<calibrated shard count>
APEX_HOSTS=<calibrated exchange host count>
APEX_SITE_LISTEN=<frozen fake-site host:port>
APEX_API_PORT=<frozen API port>
APEX_PIPELINE_INTERVAL=<frozen cadence>
APEX_TEST_TIMEOUT=<frozen speed-test delay>
APEX_ANNOUNCE_TIMEOUT=<frozen announce delay>
APEX_WALL_TIMEOUT=<timeout duration accepted by GNU timeout>
APEX_KILL_AFTER=<TERM grace duration accepted by GNU timeout>
APEX_NO_IMPAIR=no
```

The providers file is generated from the hidden CSPRNG round seed by the
control plane. Only its SHA-256 commitment is public during the round. The seed
and file are revealed only after admission has closed, all accepted work is
terminal, and post-honesty-review epoch finalization commits. `APEX_TAKEOVER_MARGIN` must exactly
equal the selected source epoch's `significant_improvement_percent / 100`;
`official-run.sh baseline` verifies that binding before it creates a manifest.

## Run and external snapshots

Inside the prepared runner container:

```bash
./official-run.sh preflight
job_dir="$(./official-run.sh run)"
```

`run` creates, without overwriting an old job:

```text
<artifact-root>/<evaluation-id>/
  results.csv
  stderr.log
  run.json
  run.complete.json
  site/stats/...
```

It records the future paths of `accounting.json` and `resources.json` in
`run.json`. `run.complete.json` proves only that the simulator joined and
flushed its own lifecycle. It does not make a score placeable by itself.

The trusted worker then writes `accounting.json` from immutable server-side
provider-egress accounting:

```json
{
  "schema": 1,
  "kind": "sim-latency-accounting",
  "evaluation_id": "job-id",
  "complete": true,
  "measure_start_ms": 123,
  "measure_end_ms": 456,
  "provider_egress_bytes": 123
}
```

The snapshot transaction is taken only after terminal contract/accounting
flush succeeds. It must cover exactly the evaluation id and measured job, not
a host-wide delta shared with another process. A failed transaction or missing
terminal accounting writes no `complete: true` report.

The worker also writes `resources.json` from the enclosing cgroup's final
`cpu.stat`, `memory.current`, `memory.peak`, `memory.events`, `pids.events`,
process exit/wait status, and containment audit:

```json
{
  "schema": 1,
  "kind": "sim-latency-resource-report",
  "evaluation_id": "job-id",
  "cgroup_id": "worker.slice/job.scope",
  "measurement_start_ms": 123,
  "measurement_end_ms": 456,
  "complete": true,
  "exit_code": 0,
  "oom_killed": false,
  "hard_killed": false,
  "limit_escape": false,
  "measurement_missing": false,
  "cpu_seconds": 123.5,
  "peak_rss_bytes": 456
}
```

`oom_killed` is true when `memory.events` increments `oom` or `oom_kill`.
`hard_killed` is true when the TERM grace expires or any process is reaped from
SIGKILL. `limit_escape` is true if the process-tree audit finds a descendant
outside the job cgroup or a cgroup limit changed during execution.
`measurement_missing` is true for any missing counter, PID audit, exit status,
or begin/end identity. Never infer false from an absent file.

## Aggregate and score

The trusted evaluator first runs the frozen odd number of independently reset
same-round baseline jobs, then supplies their comma-separated artifacts to the
pinned builder:

```text
APEX_ROUND_ID
APEX_TAKEOVER_MARGIN
APEX_BASELINE_RUNS
APEX_BASELINE_STDERR
APEX_BASELINE_ACCOUNTING
APEX_BASELINE_SAMPLES
APEX_BASELINE_RESOURCES
APEX_BASELINE_MARKERS
APEX_BASELINE_MANIFEST
```

```bash
./official-run.sh baseline
```

The builder validates every artifact with the same production parsers used for
candidates and fails unless all baseline runs have distinct evaluation ids,
the exact same workload contract, and clean success, path-integrity,
matchmaking, stability, and resource evidence. The control plane then signs
and authenticates `APEX_BASELINE_MANIFEST` and publishes its digest.

That manifest fixes the candidate replicate count. Run exactly that many
independently reset candidate jobs. The worker supplies their comma-separated
artifact paths in replicate order:

```text
APEX_BASELINE_SHA256
APEX_CANDIDATE_RUNS
APEX_CANDIDATE_STDERR
APEX_CANDIDATE_ACCOUNTING
APEX_CANDIDATE_SAMPLES
APEX_CANDIDATE_RESOURCES
APEX_CANDIDATE_MARKERS
APEX_SCORE_OUTPUT
```

Each `APEX_CANDIDATE_SAMPLES` entry is the exact `stats_root` recorded in its
replicate sidecar (the per-instance root containing `findproviders2/`), not the
parent `site/stats` directory or an unrelated export.

The worker verifies the baseline manifest's detached control-plane signature
before setting `APEX_BASELINE_SHA256`; the runner re-hashes the file before
both scoring and bundle finalization.

Then run:

```bash
./official-run.sh score
```

The scorer is file-only and returns a typed JSON result even for non-placeable
or invalid submissions. Missing/malformed artifacts are infrastructure errors;
the worker may retry those under the same cached patch identity without
granting a new candidate noise draw. Every successful score also records the
baseline and candidate run-level sample variances, observed improvement,
one-sided Welch p-value, current minimum, and supported next-epoch threshold.
A takeover requires the raw-score margin, `p <= 0.05`, G1–G6, and a supported
next-epoch recommendation.

## Immutable bundle and checksums

After scoring, run `official-run.sh finalize` for each job. It verifies all
mandatory files, records base/build/patch/API/workload/scorer/hardware/kernel
identity, frozen flags, and SHA-256 hashes for CSV, stderr, run sidecar, run
marker, accounting, resources, samples, and score. It writes:

```text
evaluation-manifest.json
evaluation.complete.json
```

`evaluation.complete.json` is the final machine-readable marker and is written
last. The worker publishes/caches a result only when this marker authenticates
the manifest and the manifest hashes every retained artifact. Re-scoring the
same bundle with the pinned scorer must produce byte-identical JSON.

Retain the canonical patch, all replicates, baseline manifest, self-check,
template-DB/migration identity, evaluator image digest, control-plane request
id, and score for the full season.

## Qualification and launch evidence

The accepted speed-to-launch calibration is immutable under
[`baseline/v1`](baseline/README.md). It selected p1800 after the impairment
on/off frontier, retained twelve uncensored same-seed A/A pairs, selected
median-of-nine with an initial 16.1% margin, and passed the authorized
independent reference screen on four of five seeds. That 4/5 screen is an
explicit compromise and is not confidence-equivalent to the original 19/20
design.

The same evidence records complete warm pools, bounded CPU and memory,
PostgreSQL/Redis/socket behavior, teardown, hostile CPU/memory-bomb cleanup,
production-staging API/worker execution, and deterministic re-scoring. Verify
the dataset with `baseline/verify.sh`; do not substitute ignored host run
directories for the versioned evidence.

Each source epoch still requires an authenticated evaluator image, source
ledger match, fresh hidden workload, promoted R=9 same-round baseline, and
passing readiness. Macrocosmos adapter/staging/registry acceptance and signed
public handoff artifacts remain external launch gates and must not be
represented by a locally fabricated checkbox or report.
