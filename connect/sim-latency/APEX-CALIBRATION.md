# Apex production calibration

Status: **NOT YET QUALIFIED — launch gate closed**

Score schema: `1`

Required hardware: one authoritative circa-2017 Xeon host, 12 physical CPUs
exposed as 10 evaluation + 2 management, 128 GB ECC RAM, Ubuntu 24.04

The second-host equivalence requirement was retired on 2026-08-17. This does
not waive any host-control, containment, frontier, noise, independent-seed, or
reference-separability requirement below.

No official-hardware measurements are present in this checkout. The
workstation campaigns in `EVALUATION2.md` are directional only and MUST NOT be
copied into a signed round manifest. The corrected local campaign measured
6.639% run CV for the failure-ceiling Apex scalar and a directional
quarter-margin threshold 26.556% below its mean. That does not justify a
single-run 1% takeover, and its independent-seed and reference-patch performance
requirements have not been run.

`official-run.sh` therefore requires `APEX_CALIBRATION_ACCEPTED=yes` and has no
scale, duration, replicate-count, or takeover default. That variable may be set
in a production image only after this document is replaced with the accepted
measurements below and Macrocosmos accepts the resulting evaluation duration.

## Local directional evidence — not qualification

The 2026-08-16 eval-48d campaign is a completed local workflow validation, not
an official calibration. Its authoritative reports are
[`eval-48g/baseline-summary.md`](eval-48g/baseline-summary.md),
[`eval-48g/baseline-summary.json`](eval-48g/baseline-summary.json), and
[`eval-48g/heldout-aa-compare.json`](eval-48g/heldout-aa-compare.json).

- 20/20 consecutive authenticated same-seed 30-minute runs; zero exclusions;
  200/200 warm clients in every run; no scored-metric drift with |t| >= 2.
- Host `sille`, linux/amd64, 24 logical CPUs; locally modified build. This is
  not the required frozen 12-core production boundary.
- Apex raw score 23,823.31 +/- 696.30 ms, CV 2.923%. The quarter-margin rule
  implies a smallest locally supported takeover margin of 11.691%.
- Successful-row total-p95 CV 2.085%, TTFB-p95 CV 4.663%, median-throughput CV
  2.819%, goodput CV 0.727%, and fast-tail-throughput CV 21.330%.
- Raw-score SD did not satisfy the local <=10% last-five-span heuristic. The
  deterministic median-of-R estimates remained above the 0.25% target for
  every tested R: 2.863% (R=1), 2.020% (R=3), 1.671% (R=5), and 1.431% (R=7).
- Two separately collected held-out runs returned `indistinguishable` at
  alpha 0.05. This validates the A/A workflow but is not independent-seed or
  reference-submission separability evidence.
- Peak simulator RSS was 14.97 GiB, mean simulator CPU was 10.23 logical-core
  equivalents, peak established TCP sockets were 26,051, and every run stayed
  at zero swap. Host and native PostgreSQL/Redis telemetry covered all runs.

The baseline warns that between-run SD is below within-run block SE for several
metrics, so its noise floor may be understated. The official campaign must
therefore start from fresh production-box evidence; it must not adopt the local
R values or the 11.691% directional margin as policy.

### Corrected pre-resource-reserve 20-run local campaign

The post-boundary, post-matchmaking-probe, post-double-cleanup binary is pinned
at `4db9cea1be48ec91bc5ef6965912b6b7f221a7a53d50c0d6b794637733a4a4e8`.
Its authoritative local reports are
[`eval-12c/frontier-summary-doublecleanup.md`](eval-12c/frontier-summary-doublecleanup.md)
and
[`eval-12c/frontier-summary-doublecleanup.json`](eval-12c/frontier-summary-doublecleanup.json).
On 2026-08-17 it completed 20/20 authenticated, same-seed, three-minute runs at
1,800 providers, 200 warm clients, and 80 arrivals/minute on one thread from
each physical core of this 24-thread host. All runs passed the provisional
frontier gates with 200/200 warm clients, zero empty pools, cleanup `401`s,
`ENOBUFS`, swap, or failed gates. Six runs used one bounded missing-client
retry.

The corrected directional baseline mean is 27,175.78 ms with sample SD
1,804.19 ms, CV 6.639%, median 26,884.88 ms, and range
23,903.49–30,590.22 ms. Its paired quarter-margin significant-better line is
19,959.03 ms, 26.556% below the mean. The last-five SD-prefix relative span is
7.260%, passing the local 10% convergence heuristic, although the sample-SD
relative standard-error estimate remains 16.222%. This local baseline and its
paired threshold are capacity-planning evidence only; neither value may enter
an official round manifest. It used all 12 physical cores. The final evaluator
now reserves 2 cores and at least 24 GiB for management, so a replacement
10-core campaign is mandatory before official calibration.

### First reserved-boundary protocol pair

On 2026-08-17, development base `sha256:22547cd4…7b16a296` completed one
full-scale baseline/candidate protocol pair with 10 evaluation cores and the
worker on the 2-core management set. The single baseline raw score was
27,174.12 ms and its paired provisional significant-better threshold was
19,957.81 ms; the comment-only candidate scored 27,051.04 ms. Both established
200/200 clients, passed G1-G6, stayed within limits, and authenticated all 54
retained files and 15 security booleans. One pair cannot estimate run noise or
replace the required 20-run same-seed campaign; it only clears the local
reserved-boundary protocol gate.

The separate production-pressure gate simultaneously saturated the exact ten
evaluation CPUs and drove a no-swap memory bomb to the actual 72 GiB runner
ceiling. It exited 137 with `OOMKilled=true`; cleanup from the management CPU
set removed every labeled object in 1,019 ms and left zero residuals. The
authoritative host must reproduce this result before its containment marker is
accepted.

## Frozen host identity

| Field | Qualified value |
|---|---|
| hardware inventory ids | NOT SET |
| BIOS / microcode | NOT SET |
| Ubuntu image digest | NOT SET |
| kernel release | NOT SET |
| CPU model / stepping | NOT SET |
| SMT / NUMA / affinity / 10+2 CPU split | NOT SET |
| turbo / governor | NOT SET |
| memory / swap / THP / >=24 GiB management reserve | NOT SET |
| sysctl and limits profile hash | NOT SET |
| Postgres / Redis versions | NOT SET |
| API image digest | NOT SET |
| base/scorer revisions and scorer SHA-256 | NOT SET |

## Frontier sweep

Run each promising point with impairment enabled and `--no-impair`. Preserve
the providers file, full run flags, CSV, schema-2 sidecar/marker, stderr,
accounting, samples, cgroup report, host self-check, and bundle hashes.

Sweep at least:

| Dimension | Tested values | Selected |
|---|---|---|
| providers | 1,800 (unprivileged prequalification) | NOT SET |
| warm clients | 200 (unprivileged prequalification) | NOT SET |
| arrivals/minute | 80 (unprivileged prequalification) | NOT SET |
| multi-client window | 2, 4, adaptive (unprivileged prequalification) | NOT SET |
| exchange hosts | 4 (unprivileged prequalification) | NOT SET |
| fleet shards | 4 (unprivileged prequalification) | NOT SET |
| measured duration | 3 m and 5 m (unprivileged prequalification) | NOT SET |

Prequalification evidence is generated by `eval-frontier-12c.sh`. The pre-fix
and first post-probe points exposed measurement-boundary and derived-client
teardown-order bugs and remain archived diagnostic history. The corrected
20-run campaign above has replaced them for local planning, but no dimension is
selected for production. At 2026-08-17 19:56 UTC, enabled root-owned systemd
units enforce exactly 12 physical CPUs with SMT/turbo off, `performance`,
`vm.overcommit_memory=1`, a live 10+2 split, and all 49 movable device IRQs on
management CPUs `20,22`. A reboot proof, Docker user-namespace activation, the
firewall, and a controlled Docker restart remain open; Docker still has its
pre-control 24-CPU view. The historical measurements predate these controls and
the direct-local-mount boundary, so the official impairment-on/off frontier
must still be rerun after the remaining host and source gates close.

For each point report warm-client establishment, empty markets, attempted
requests/bytes, failures and incomplete bodies, auth/contract timeouts,
FindProviders2 pool/load distributions, accounting coverage, CPU/RSS, socket
pressure/`ENOBUFS`, Postgres/Redis latency, and teardown/resource health.

The selected frontier point needs 20 consecutive clean baseline evaluations,
the frozen warm-pool threshold (100% unless separately justified), no empty
market or measured-window `ENOBUFS`, baseline CPU at or below approximately 65%
of the 10-core evaluation budget, and stable database/cache/socket/memory/teardown behavior.

## Noise campaign

At the selected frontier point:

1. Before drawing a reference-campaign seed, commit all intended changes in
   `server`, `connect`, `sdk`, and `sn`; fetch and merge every `origin/main`;
   rerun affected verification; push; require every source-lock worktree to be
   clean; and record the complete pushed source lock. Fail closed if seed
   generation and evaluation are not bound to that identity.
2. Run at least 20 clean repetitions of one hidden seed. Report every
   per-replicate raw score and the convergence of the median for each candidate
   replicate count considered.
3. Run the baseline on at least 20 independent CSPRNG seeds. Report
   round-to-round spread and every quarantined seed with a predeclared reason.
4. Run the pinned no-op, deliberately worse, and plausibly better reference
   patches. They must rank in order on at least 19/20 seeds.
5. Report CPU headroom and end-to-end submit/poll wall time, including reset,
   build/test, all baseline/candidate replicates, scoring, and bundle hashing.

The three provisional development-base inputs now exist in
[`competition/references`](../../competition/references/README.md). Their
manifest authenticates each patch, deterministic candidate commit, image
digest/key, and simulator digest. All three pass the local offline Docker build
and cache-reuse path. The better patch also has a deterministic regression for
the unordered transfer-pair key required by its bulk lookup. This is build and
semantic-contract evidence only: no reference performance run has occurred,
the frozen season base does not exist, and `official_separability` remains
`not_run`.

For each aggregation candidate record:

| Replicates | Duration | `sigma_run` | takeover margin | `sigma / margin` | MDD | wall time / cost | accepted |
|---:|---:|---:|---:|---:|---:|---:|---|
| NOT RUN | NOT RUN | NOT RUN | NOT SET | NOT RUN | NOT RUN | NOT RUN | no |

The accepted design MUST satisfy

```text
sigma_run <= 0.25 * takeover_margin
```

and reference separability on at least 19/20 seeds. Test remedies in this
order when it fails: host quiescing/isolation, longer windows, interleaved or
paired baseline/candidate runs, median-of-R, a more stable success-consistent
scalar, then a larger negotiated margin. Do not choose R or the margin from
schedule pressure.

## Final signed selection

| Field | Accepted value |
|---|---|
| provider/client/arrival scale | NOT SET |
| ramp/prewarm/settle/duration | NOT SET |
| hosts/fleet shards/window | NOT SET |
| request-timeout ceiling and justification | NOT SET |
| baseline replicates | NOT SET |
| candidate replicates (odd) | NOT SET |
| aggregation | type-7 median (contract-fixed) |
| takeover margin | NOT SET |
| raw-score run noise | NOT MEASURED |
| minimum detectable delta | NOT MEASURED |
| reference separability | NOT RUN |
| CPU/RSS/socket/DB/cache headroom | NOT MEASURED |
| evaluation wall time and expected cost | NOT MEASURED |
| UR reviewer / date | NOT SIGNED |
| Macrocosmos acceptance / date | NOT ACCEPTED |

Until every `NOT SET`, `NOT RUN`, and `NOT ACCEPTED` entry in the final
selection is replaced with linked raw evidence, the integration is locally
implemented but not launch-ready.
