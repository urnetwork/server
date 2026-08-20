# sim-latency

A local simulation of the egress-provider stack for an algorithmic competition
to improve end-to-end latency. It stands up a full urnetwork environment on the
`local` env — api, exchange/connect, the reliability→score pipeline, a fleet of
simulated providers with realistic network conditions, and a fake origin site —
then drives client load through the tunnel and prints per-request performance as
CSV. The code under competition is the egress-provider algorithm stack:
`FindProviders2` (matchmaking), the client (`RemoteUserNatMultiClient`), and
`LocalUserNat` (provider egress).

Everything runs against the real server code paths: providers connect over real
websockets, get geolocated, latency/speed-tested, reliability-scored, and
selected by the real `FindProviders2`. Improving the algorithms improves the
measured numbers.

## What you optimize

- **`model.FindProviders2`** and the score pipeline it reads
  (`UpdateClientScores`, the reliability rollup) — which providers get selected,
  how they are weighted, sampled, and banded.
- **`connect.RemoteUserNatMultiClient`** (the client) — window management,
  provider selection among discovered destinations, retries.
- **`connect.LocalUserNat` / `RemoteUserNatProvider`** — the provider egress
  data path.

You do not edit sim-latency itself to compete — you edit the stack. sim-latency
is the fixed, reproducible measuring instrument.

## Metrics and the official Apex score

A run is scored on the CSV rows inside the **measured window** (after ramp +
settle; the boundary is logged to stderr as `MEASURE WINDOW: [start, end]` and
recorded in the run.json side-car).

The official Apex scalar is defined in
[`APEX-SCORE-SPEC.md`](APEX-SCORE-SPEC.md): p95 `total_ms`, lower better, with
every failure or incomplete body charged at the frozen request-timeout ceiling.
It is produced by `sim-latency score` and guarded by G1-G6. The local
`analyze`/`baseline`/`compare` commands retain the following richer diagnostic
metric set; their multi-metric verdict is not the Apex score:

- **primary: `ttfb_p95_ms`** (tail time-to-first-byte, lower is better) and
  **`throughput_p50_bytes_per_s`** (median large-transfer throughput, higher
  is better; transfers >= 1 MiB only — the site's download tier (2–6 MiB
  pages), sized so transfer time dominates ttfb and `bytes/total` honestly
  measures sustained bandwidth rather than connection-setup luck)
- **guards: `fail_rate` and `throughput_p05_bytes_per_s`** — non-inferiority
  gates. Timing/throughput metrics count successes only, so the failure rate
  stops a build winning latency by dropping hard requests; the struggling-
  tail throughput stops a build raising the median by starving the slowest
  flows (and at ~2% A/A CV it detects small degradations)
- secondary (reported, not gating): `ttfb_p50_ms`, `total_p50_ms`,
  `total_p95_ms`, `throughput_p05_bytes_per_s` (the struggling tail),
  `throughput_p95_bytes_per_s` (the fast tail — reported only: per-flow
  throughput is ceilinged by the tunnel data path far below provider lane
  rates, so the fast tail is a run-level regime lottery that measures at
  21–47% A/A CV across measured revisions, undecidable as a verdict),
  `goodput_bytes_per_s`

A standard run is a fixed `providers.yml` (which locks the fleet and all seeds),
a fixed settle and duration, and the same server build except for the code under
test. Because the fleet, site tree, client arrivals, and impairments are all
seeded from `providers.yml`, two runs of the same file replay the identical
workload — whether an observed difference is *real* is decided statistically
(see "Comparing runs" below).

## System requirements

The historical [eval-48](#the-eval-48-evaluation-environment) environment is a
local/directional workstation profile, not the Apex production environment.
The Apex host/runner contract is in [`OFFICIAL-RUN.md`](OFFICIAL-RUN.md): two
identical 12-core, 128 GB Ubuntu 24.04 machines, with scale and replicate count
frozen only after production-box calibration. The full-scale configuration —
**~100,000 providers, ~1,000 clients/min** — remains available for stress
runs and targets a big-memory Linux box:

- **Linux** (the ephemeral-port and fd limits below are Linux-tuned). macOS
  works for small dev runs but cannot source enough connections for full scale.
- **RAM**: budget ~50–70 GB for 100k providers plus the exchange residents.
  Shard the fleet across machines (or processes) to spread it.
- **File descriptors**: `ulimit -n` in the millions (`1048576`+). Each provider
  and client holds sockets.
- **Ephemeral ports**: the fleet opens >64k connections, so the exchange listens
  on several ws ports (`--hosts`) and providers spread across them. Raise
  `net.ipv4.ip_local_port_range` and `net.ipv4.tcp_tw_reuse=1`.
- **Postgres + redis**: the local stack (`server/local/run-local.sh`) must be
  running. At 100k providers the reliability pipeline writes ~100k rows/minute;
  give postgres headroom.

For development, start at `--count 2000 --hosts 2` on a laptop and scale up.

## The eval-48 evaluation environment

The historical local environment is **eval-48**: a reduced configuration
sized so the whole stack — the run process, fleet shards, and the local
postgres + redis — fits a **48 GB** machine with several-fold headroom. The
sizing is deliberate: an environment near its machine's memory or CPU limits
measures its own scheduling and paging noise, which fattens the between-run
variance and raises the smallest improvement a submission can prove. eval-48
trades scale realism for a stable instrument.

The environment is defined — and pinned — by `eval-48.sh`. The current
revision is **eval-48d** (2026-08-16). It keeps the eval-48c scale and mixture,
but freezes deterministic warm-client ordering and exchange-host assignment,
plus a bounded retry of only identities that miss their first establishment
attempt. This removes transient `199/200` aborts and scheduler-dependent pool
order without changing measured request semantics. eval-48c itself kept the
eval-48b scale and mixture, which spread fast-tier load across more lanes but
did not make `throughput_p95` suitable as a verdict: eval-48d still measured
that tail at 21.33% run CV (eval-48a measured 47%; see the noise-floor
section):

- **fleet**: 2,000 providers, mixture v2, **seed 48** — residential 45%,
  mobile 25%, and a continuous fast upper band holding 30% of the
  population: business-fiber 15% (50–250 Mbps, caps 24–64) bridging into a
  narrowed hosting tier 15% (200–600 Mbps, caps tightened to 32–96 so tail
  load spreads across many lanes)
- **site**: two-tier page bodies — web tier 4–512 KiB, download tier 2–6 MiB
  on 25% of pages (the throughput sample; for seed 48 the tree has 37 pages,
  7 of them download-tier, 33 MB full-crawl weight)
- **clients**: 200-identity warm pool, 80 mean arrivals/min (rebalanced for
  the heavier crawls)
- **run shape**: 30 m measured window, `--fleet-shards 4`, warm-up defaults
  (ramp 1 m, prewarm 13 h, settle 1 m, hosts 4, pipeline-interval 10 s),
  followed by a 70 s inter-run service/connection quiescence period
- every measured run starts from `--reset`
- the wrapper pins the local service identity to `sim/sim`; inherited
  `test.sh` labels are deliberately overridden because they select a different
  stats namespace
- **canonical fleet file**: `eval-48g/providers-eval48d.yml`, regenerated
  bit-identically from the seed; sha256
  `549ec41c033f344d6e0a6b1de82b404bb63d5a8dfb5861b6c4b6d55886cdace4`.
  `compare` refuses to compare runs whose config sha differs, so every
  machine evaluates the identical workload. The revision was advanced because
  the historical eval-48b pin could not be regenerated from committed source;
  neither eval-48b nor pre-fix eval-48c artifacts may be mixed with eval-48d
  measurements, even though eval-48c and eval-48d share this fixture hash.

```
./eval-48.sh init             # generate + sha-verify the canonical providers file
./eval-48.sh run > my.csv     # one standard evaluation run (~36 min wall)
./eval-48.sh campaign 13      # target >=20 sequential A/A replicates → eval-48g/runs/
./eval-48.sh baseline         # noise floor + convergence from the campaign runs
./eval-48.sh summary          # authenticated Apex-score/resource summary (k>=20)
# Or, after campaign returns, finish every remaining local gate unattended:
./finalize-local-baseline.sh 20 2
```

`campaign` is the long-form of `baseline --replicates`: the same sequential
independent `run --reset` replicates, but time-bounded and failure-tolerant
(one failed replicate is logged and skipped; a wedged replicate is killed by
a 55 m watchdog; a frozen G5 log signature terminates the run before it can
write a completion marker; three consecutive failures abort as an environment
outage),
with the stack's memory sampled to `eval-48g/campaign-rss.csv` by
`sample-rss.sh`. `sample-host-resources.sh` adds low-overhead CPU, available
memory, swap, load, and TCP-socket telemetry for a local directional campaign.
On hosts where PostgreSQL and Redis are native processes rather than the fixed
Docker containers expected by the legacy RSS sampler,
`sample-service-resources.sh` records process counts and explicitly labeled
summed process RSS. PostgreSQL summed RSS double-counts shared pages and is a
trend diagnostic, not unique-memory or cgroup accounting; missing early-run
coverage is reported rather than interpreting Docker zeros as measurements.
After aggregation, `summarize-baseline.py` independently authenticates every
CSV/sidecar/marker chain, recomputes the failure-ceiling Apex raw score and
baseline convergence arrays, records every excluded failed-closed attempt,
measures drift, and writes `eval-48g/baseline-summary.json`, a human-readable
Markdown summary, and `eval-48g/baseline-stability.svg`.

These local files measure noise; they are not the signed
`sim-latency-score-baseline` manifest consumed by `score`. That manifest also
fixes an accepted takeover margin and odd replicate policy and may only be
issued by the trusted control plane after official-hardware, independent-seed,
and reference-separability calibration succeeds.

`finalize-local-baseline.sh` tops up an underfilled campaign in two-hour chunks,
regenerates and audits the baseline, collects two clean runs outside the
baseline, requires their A/A verdict to be `indistinguishable`, and then invokes
`verify-local-baseline.sh`. The refreshed JSON and Markdown summaries embed the
held-out verdict and fingerprint its complete comparison artifact. The verifier
refuses to run while a simulator is
active, replays the baseline/summary twice byte-for-byte, checks hashes and
machine-readable invariants, and runs the package, race, vet, and compile gates.
Its unattended log belongs at `eval-48g/postcampaign-verification.log`.

**An evaluation run requires an idle machine.** This is a hard requirement,
not hygiene: machine on AC with the lid open
(`caffeinate -i ./eval-48.sh campaign 13`), `server/local/run-local.sh` up
for the whole campaign, and nothing else heavy on the box. **Pause Time
Machine during campaigns** (`tmutil stopbackup`; better, exclude the Docker
VM disk image with `sudo tmutil addexclusion` — its constant churn makes
backups run for hours), or backup + Spotlight disk contention starves
postgres dials exactly when the 2,000-provider fleet connects
("postgres unreachable under the connected fleet"). External CPU
pressure does not add mild noise — it breaks runs in obvious ways (measured
2026-07-30 under a Spotlight/Mail indexing storm: fail_rate blowing out
2–3×, a mid-run market collapse to 100% failures, and a warm-up wedged for
2h — while quiet-box runs sat in a tight band). Treat any run whose
fail_rate or row count departs the baseline band as an investigation signal,
not an automatic exclusion. An authenticated completion remains in the
baseline unless a predeclared, artifact-backed infrastructure rule proves the
run invalid; the summary reports robust outlier candidates without removing
them. Warm-up failures and interrupted/incomplete runs fail closed and are
inventoried separately because they never produced placeable artifacts.

Measured eval-48d footprint (`sille`, 24 logical CPUs, 2026-08-16): simulator
peak RSS **14.97 GiB**, mean simulator CPU **10.23 logical-core equivalents**,
and peak established sockets **26,051**. PostgreSQL summed-process RSS peaked
at 7.90 GiB, but that observational number double-counts shared pages; Redis
peaked at 129.9 MiB. All 20 runs had complete host and native-service coverage,
at least 107.37 GiB host memory available, and zero swap. A standard run is
about 36 minutes wall (~6 minutes setup plus a 30-minute window) and produces
roughly 83–85 k measured requests with all 200 clients established.

### Measured local noise floor (2026-08-16, k=20, revision eval-48d)

The post-fix campaign completed **20/20 consecutive authenticated attempts**
with zero exclusions. The complete report is
[`eval-48g/baseline-summary.md`](eval-48g/baseline-summary.md); machine-readable
evidence is in `baseline-summary.json`, `baseline.json`, and
`baseline-stability.svg`.

| metric | role | mean +/- sd | cv | min delta, 1 run/side | 3 runs/side |
|---|---|---|---:|---:|---:|
| Apex raw score | official scalar | 23,823.31 +/- 696.30 ms | 2.923% | quarter-margin floor 11.691% | not policy-qualified |
| `ttfb_p95_ms` | primary diagnostic | 1,025.40 +/- 47.82 ms | 4.663% | 116.93 ms | 67.51 ms |
| `throughput_p50_bytes_per_s` | primary diagnostic | 290.75 +/- 8.20 kB/s | 2.819% | 20.04 kB/s | 11.57 kB/s |
| `throughput_p05_bytes_per_s` | guard | 84.99 +/- 2.35 kB/s | 2.765% | 5.75 kB/s | 3.32 kB/s |
| `fail_rate` | guard | 1.154% +/- 0.114 pp | 9.901% | — | — |
| `total_p95_ms` (successful rows) | diagnostic | 20,902.62 +/- 435.72 ms | 2.085% | — | — |
| `goodput_bytes_per_s` | diagnostic | 41.151 +/- 0.299 MB/s | 0.727% | — | — |
| `throughput_p95_bytes_per_s` | reported only | 11.258 +/- 2.401 MB/s | **21.330%** | — | — |

No scored metric had |drift t| >= 2. Robust triage flagged TTFB-p95 values in
`r004` and `r011`; both remain included because no predeclared infrastructure
rule invalidated them. The three primary diagnostic SD sequences meet the local
last-five <=10% span heuristic, while the Apex raw-score sequence does not.
Some between-run SDs are also below within-run block SE, so the generated report
warns that the noise floor may be understated.

A single-run 1% takeover is therefore unsupported. Deterministic median-of-R
estimates remain above the required 0.25% CV at R=1,3,5,7 (2.863%, 2.020%,
1.671%, and 1.431%). Two separately collected held-out runs returned
`indistinguishable` at alpha 0.05; see
[`eval-48g/heldout-aa-compare.json`](eval-48g/heldout-aa-compare.json). The final
verifier replayed the baseline and summary twice byte-identically and passed
artifact invariants, package tests, race tests, vet, and the repository compile
gate. This remains a local/directional modified-build result, not an official
signed Apex baseline.

### Historical preliminary noise floor (2026-07-31, k=4, eval-48b)

**Superseded by eval-48d.** This table is retained only to explain earlier
design choices. It no longer describes `eval-48g/baseline.json`; the k=4 SD
estimate carried about +/-41% sampling uncertainty and df=3 made its thresholds
conservative.

| metric | role | mean ± sd | cv | min Δ, 1 run/side | 3 runs/side |
|---|---|---|---|---|---|
| `ttfb_p95_ms` | primary | 1670 ± 82 ms | 4.9% | 273 ms (~16%) | 158 ms (~9%) |
| `throughput_p50_bytes_per_s` | primary | 137.4 k ± 9.9 k | 7.2% | 32.9 k (~24%) | 19.0 k (~14%) |
| `throughput_p05_bytes_per_s` | guard | 48.6 k ± 1.1 k | 2.2% | 3.5 k (~7%) | 2.0 k (~4%) |
| `fail_rate` | guard | 0.22% ± 0.02 pp | 9.9% | 0.074 pp | 0.043 pp |

(`throughput_p95` reports at 26.5% CV — the reason it is no longer a
primary.) The per-run series and thresholds are plotted in the
"eval-48b baseline stability" artifact.

### Measured noise floor (2026-07-30, k=12 clean replicates, revision eval-48a)

**Superseded first by eval-48b and now by eval-48d** — the mixture, site tier,
arrival rate, and throughput gate changed, so this floor (archived as
`eval-48g/baseline-eval48a.json`, runs in `eval-48g/runs-eval48a/`) applies
only to eval-48a-sha runs. From 12 clean A/A replicates spanning 8 h (2
contended runs excluded; no significant drift in any metric across the span):

| metric | mean ± sd | cv | min Δ, 1 run/side | 3 runs/side |
|---|---|---|---|---|
| `ttfb_p95_ms` (primary) | 2329 ± 114 ms | 4.9% | 289 ms (~12%) | 167 ms (~7%) |
| `throughput_p50_bytes_per_s` | 142.5 k ± 5.7 k | 4.0% | 14.5 k (~10%) | ~6% |
| `throughput_p95_bytes_per_s` (primary) | 1.41 M ± 0.66 M | **47%** | 1.68 M (~120%) | ~70% |
| `fail_rate` (guard) | 3.9% ± 0.5 pp | 14% | 1.4 pp | 0.8 pp |

`ttfb_p95` is a strong primary in eval-48. `throughput_p95` is effectively
undecidable: its per-request distribution is bimodal (a bulk mode plus a
thin ~2–7 % fast mode from high-bandwidth hosting lanes whose share
crystallizes per run during warm-up), and the p95 sits exactly on the
boundary between the modes, so it inherits the run-to-run fast-share
lottery. The stable candidates for a bandwidth verdict are
`throughput_p50` or a mid-tail/trimmed statistic; changing the primary set
is a rules decision recorded in `compare.go`/`metricDefs`.

## Prerequisites

1. The local backing stores are up:

   ```
   cd server/local && ./run-local.sh        # postgres + redis on the local aliases
   ```

2. `WARP_HOME` (or `WARP_VAULT_HOME`) points at the repo `vault/` so the tool can
   read the local pg/redis/jwt config. sim-latency sets the other env defaults
   (`WARP_ENV=local`, hostnames) itself; `run-local.sh` here does the same.

## Quick start

```
cd server/connect/sim-latency
go build -o sim-latency .

# 1. generate the locked fleet (edit providers.yml afterwards to tune the mixture)
./sim-latency init --count 2000 --clients 200 --rate 200 --seed 1 --out providers.yml

# 2. run: brings up the environment, ramps the fleet, settles, then measures
./sim-latency run --reset --providers providers.yml --meta results.run.json > results.csv
#    (per-request CSV on stdout; all logs on stderr; --reset clears prior
#     runs' reliability state; the run.json side-car carries the window +
#     metric summaries)

# 3. summarize / compare (see "Comparing runs" below)
./sim-latency analyze --run results.csv
```

Or use the convenience wrapper that sets the local env:

```
./run-local.sh run --providers providers.yml > results.csv
```

## The warm-up period

Before the measured window, the run goes through a **warm-up** that brings the
market to a stable state: the fleet connects, fixture-derived latency/speed
evidence and mature reliability scores are established, the selectable set is
exported, and the complete client pool proves every parallel HTTP lane.
Providers carry their base network impairment during this setup but remain
connected and in their base regime. Seeded churn and degraded-regime schedules
start at the authenticated measurement boundary. Variable client-pool
construction time therefore cannot phase-shift an otherwise identical
workload. Only after the complete pool has been revalidated do measured arrivals
start, so every request runs against a settled market with warm reusable lanes.

Why warm-up is needed: `FindProviders2` gates on the real reliability weights,
and the binding one is the **12-hour lookback ≥ 0.7** — a weight is
`valid_blocks / full_window`, so from a cold start reaching it takes roughly
**8.4 hours of uptime**, the same conservative onboarding a real new provider
faces. The competition measures selection and egress among an *established*
fleet, not onboarding, so the warm-up short-circuits this.

The warm-up phases and the knobs that make it **as fast as possible**:

| Phase | What it does | Knob (default) |
|---|---|---|
| ramp | stagger provider connects | `--ramp 1m` |
| prewarm | establish fixture performance evidence and mature reliability scores (instant) | `--prewarm 13h` |
| settle | let the pipeline propagate scores → selectable set | `--settle 1m`, `--pipeline-interval 10s` |
| client pool | establish and revalidate every quality exit and parallel HTTP lane | `--client-warmup-timeout 20m` |

`--prewarm` writes the fixture's round-trip latency and bandwidth evidence onto
each active connection, then writes the final reliability scores for every
connected provider using its seeded uptime duty cycle, rather than replaying
~8.4h of history. Attaching the evidence to the active connection makes later
pipeline snapshots preserve it; tests belong to one transport, so a replacement
transport can otherwise make an established provider look untested.
`--prewarm 0` restores
the true cold start and live connection-test behavior. A shorter
`--pipeline-interval` propagates provider state (new tests, churn) into the
selectable set faster.

The current selection pipeline also requires fresh provider egress-health and
egress-location evidence. Production obtains that evidence from an external
prober; the self-contained simulator has no such process, so provisioning seeds
one deterministic passing `sim` probe and a direct observation in the fake
country. This is an initial condition only: the modeled latency, bandwidth,
loss, capacity, and churn still govern the measured data path.

For the fastest possible warm-up on a small fleet:
`--ramp 10s --settle 20s --pipeline-interval 5s`.

Notes:

- Churny providers carry their mature uptime duty cycle in every reliability
  lookback and still fall out of the market while disconnected. Prewarmed mode
  keeps those mature weights fixed; `--prewarm 0` exercises the live reliability
  history pipeline instead.
- The measured window is logged as `MEASURE WINDOW: [start, end]`; only rows in
  it count.
- Reaching the end stops new crawl arrivals but does not cancel in-flight
  crawls that started inside the window. They drain up to their own request
  deadline; an external TERM/INT still cancels the evaluation immediately.

## CSV output

One header line, then one row per request (stdout only):

```
t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s
```

- `t_start_ms` — request start (unix ms); compare against the `MEASURE WINDOW`.
- `client` — the client id (raw in local env, so it joins to `providers.yml`).
- `path`, `depth` — the crawled suburl and its depth in the loading tree.
- `status` — HTTP status; `0` means the request did not complete (no provider,
  timeout, malformed page header, read/close error, or a byte/Content-Length
  mismatch). A 200 is emitted only after the declared fake-site body has been
  received exactly.
- `bytes`, `ttfb_ms`, `total_ms`, `bytes_per_s` — size, time to first byte,
  total time, throughput.

Alongside the CSV, `run` writes a **run.json side-car** (`--meta`, default
`run.json` — name it per run, e.g. `--meta a.run.json` next to `a.csv` so the
tools find it by convention). It records everything the statistical tooling
needs beyond the rows: the measure window, providers.yml sha256 + seed, build
revision, evaluation id, request-timeout ceiling, stats root, scorer/schema
identity, CSV SHA-256/byte identity, completion state, host, flags, warm-pool
establishment count, and the per-metric summaries. Schema 2 proves that
complete-body validation was active when its complete sidecar is authenticated
by the final marker; a hand-created or incomplete schema-2 file is not
scoreable.
After the client/CSV drain, fleet-child reap, service shutdown, and durable
stats close all succeed, `run` writes `<meta>.complete.json`; interrupted or
unclean runs retain an incomplete sidecar and never receive that marker.
`sim-latency analyze --run x.csv` recomputes the summaries from
the rows (`--window <startMs>,<endMs>` substitutes for a missing side-car).

Historical note: the sdk used to redirect stderr onto stdout at init (mobile
logging convention), so older results files have log lines interleaved with
CSV rows. That is fixed (the redirect is now mobile-only), and the CSV reader
skips such lines in legacy files.

## Comparing runs (is A really better than B?)

Two runs of the same providers.yml replay the identical seeded workload, so
any difference between runs of *unchanged* code is pure environment noise:
goroutine scheduling, backing-store timing, warm-pool composition. Requests
within a run are heavily autocorrelated (shared market state, 60s regime
dwells, the shared warm pool), so per-request t-tests wildly overstate
certainty — the honest unit of replication is the **run**. The tooling
therefore measures the noise floor from baseline (A/A) replicates and tests
observed A-vs-B differences against it (this is what baseline.json is for —
it encapsulates the variance used to measure significance):

```
# 1. measure the baseline once per (config, duration, machine): k >= 5
#    replicate runs of UNCHANGED code, each from a clean reset. One command:
./sim-latency baseline --replicates 5 --providers providers.yml --out baseline.json
#    (runs 5 sequential `run --reset` replicates into baseline-runs/, then
#     computes; or compute from runs you already have:
#     ./sim-latency baseline --runs aa1.csv,aa2.csv,aa3.csv,aa4.csv,aa5.csv)

# 2. measure the candidate build (same providers.yml, same flags)
./sim-latency run --reset --providers providers.yml --meta b.run.json > b.csv

# 3. decide
./sim-latency compare --a b.csv --b baseline-runs/baseline-1.csv --baseline baseline.json --p 0.05
./sim-latency compare ... --json        # machine-readable verdict
```

baseline.json encapsulates, per metric, the between-run mean/sd/cv **and the
convergence diagnostics**: `sd_by_replicates` (the sd estimated from the
first 2..k replicates — has the estimate stabilized, or do you need more?),
`sd_rel_error` (~1/sqrt(2(k−1)), the sampling uncertainty of the sd itself —
±35% at k=5), and `min_detectable_delta_by_runs_per_side` (the smallest
significant delta for a comparison with m = 1..n runs per side, shrinking as
sqrt(1/m)). The command prints both convergence tables for the primary
metrics.

The test: for each metric, `t = (S_A − S_B) / (sd_env · sqrt(1/m_A + 1/m_B))`
with `k − 1` degrees of freedom — "is the difference large relative to the
difference between two runs of identical code?". With >= 2 runs per side a
Welch t on the run-level values is also computed and the more conservative
p-value wins. Multiple runs per side (`--a b1.csv,b2.csv`) shrink the
detectable delta by `sqrt(m)`.

The verdict at threshold `--p`: **A beats B iff some primary metric
(`ttfb_p95_ms`, `throughput_p50_bytes_per_s`) is significantly better after
Holm correction, and no guard metric (primaries + `fail_rate` +
`throughput_p05_bytes_per_s`) is significantly worse** at raw alpha. Other outcomes: `b_better`, `mixed`
(significant effects in both directions), `indistinguishable` (nothing
exceeds the noise floor — the printed *min detectable delta* shows what the
test could even see, so under-powered is never confused with no-difference).

Caveats the tooling enforces or warns about:

- `compare` **errors** when A and B ran different providers.yml (different
  workloads are not comparable), and **warns** when baseline.json was
  measured on a different config, duration, or host, or when the warm client
  pools differ by more than 5% (load per client confounds).
- Without `--baseline` it falls back to within-run block-bootstrap SEs (60s
  blocks), which cannot see between-run noise — flagged `optimistic`.
- The baseline variance is only valid for runs launched from the same clean
  state: use `--reset` (or `sim-latency reset`) before every measured run,
  otherwise reliability history accumulates across runs and the market
  itself drifts (see the gotchas below).

## Official scoring

`score` is file-only and accepts one comma-separated path per candidate
replicate for the CSV, stderr, immutable accounting snapshot, FindProviders2
sample corpus, cgroup resource report, and completion marker, plus the signed
same-round baseline manifest:

```bash
./sim-latency score \
  --run r1.csv,r2.csv,r3.csv \
  --stderr r1.stderr,r2.stderr,r3.stderr \
  --baseline round-baseline.json \
  --accounting r1.accounting.json,r2.accounting.json,r3.accounting.json \
  --samples r1.stats-root,r2.stats-root,r3.stats-root \
  --resource-report r1.resources.json,r2.resources.json,r3.resources.json \
  --marker r1.complete.json,r2.complete.json,r3.complete.json \
  --out score.json
```

The signed manifest determines the odd replicate count, exact run flags,
providers SHA-256, timeout, takeover margin, and baseline replicate
diagnostics. Candidate and baseline values aggregate by median. The output is
deterministic `score_schema: 1` JSON with the raw/normalized score, all G1-G6
results, diagnostics, and a typed submission or infrastructure error. Missing,
malformed, legacy-schema, mixed-id, incomplete, OOM, panic, or unclean artifacts
fail closed. See [`OFFICIAL-RUN.md`](OFFICIAL-RUN.md) for containment and bundle
construction.

The trusted evaluator creates that manifest from its same-round baseline bundle
with the same parsers used for candidates:

```bash
./sim-latency score-baseline \
  --run b1.csv,b2.csv,b3.csv \
  --stderr b1.stderr,b2.stderr,b3.stderr \
  --accounting b1.accounting.json,b2.accounting.json,b3.accounting.json \
  --samples b1.stats-root,b2.stats-root,b3.stats-root \
  --resource-report b1.resources.json,b2.resources.json,b3.resources.json \
  --marker b1.complete.json,b2.complete.json,b3.complete.json \
  --round-id "$ROUND_ID" --takeover-margin 0.05 \
  --out baseline.json
```

The command requires an odd positive replicate count, exact workload-contract
agreement, distinct evaluation ids, and clean G1/G3-G6 evidence for every
replicate. The evaluator authenticates and signs the resulting file; miners do
not create the trusted baseline.

## providers.yml

The locked ground truth. `init` writes the mixture definition plus one concrete
entry per provider. Edit the `providers.mixture` weights/ranges and the `site`,
`clients`, and `subnets` sections, then re-run `init` to re-sample the fleet
(the entry list under `fleet:`). Key sections:

- `region` — the fake country (`zz` / "Sim"); every provider and client lives
  here, matched from the testing subnets by the server's `ip_overrides` hook.
- `subnets` — provider and client testing subnets (RFC 2544 `198.18.0.0/15` for
  providers). Each provider/client presents a unique address from these.
- `site` — the loading tree: `mean_depth` (K), `branching`, and the two body
  size tiers (web `min/max_body_bytes`; download `large_fraction` +
  `large_min/max_body_bytes`, the pages the throughput metrics sample).
- `clients` — `pool_size`, `mean_per_minute` (M), balance, connections per crawl.
- `providers` — `count`, network grouping, and the `mixture`.
- `fleet` — the generated per-provider entries (ip, ids, user type, sampled
  latency/bandwidth/loss/churn, dynamics seed). Exported FindProviders2 samples
  join to these by client id.

Each `mixture` component is a weighted mode of the population with ranges for
latency, jitter, bandwidth, loss, **max_connections** (the concurrent
connection cap, below), uptime/downtime (churn), and a degraded-regime
fraction. A provider is assigned a component by weight, then its parameters are
sampled from the ranges.

### max_connections (provider ulimit)

Each provider carries a sampled `max_connections`: the maximum simultaneous
tunneled flows its egress NAT serves (`0` = unlimited, which is also the
behavior for older providers.yml files that predate the field — re-run `init`
to sample caps). It is enforced by the real `LocalUserNat` flow limits
(`TcpBufferSettings.GlobalLimit` / `UdpBufferSettings.GlobalLimit`, plumbed
through `sdk.SimProviderConfig.MaxConcurrentFlows`): a flow over the cap is
admitted and the **idle-most established flow is lru-evicted**, which the
victim sees as a reset/timeout. Idle keep-alive connections cull first (soft
degradation), then active transfers fail — the NAT-table-realistic shape of
capacity exhaustion.

Like bandwidth, the cap is **hidden ground truth**: clients discover capacity
only through failures and latency, so a strategy that routes all traffic to
the single best provider saturates it and pays for it in the measured
metrics. The default mixture caps hosting providers around 32–96 concurrent
flows, business 24–64, residential 8–32, mobile 4–12.

**Environment rule:** the flow-limit machinery in `connect` (the
`LocalUserNat` buffer limits and their lru eviction) and the caps sampled in
`providers.yml` are part of the fixed measurement environment, exactly like
the impairment model — competition changes must not weaken or bypass them,
even though `LocalUserNat` is otherwise open for optimization. (The client
side is free to *react* however it likes — e.g. the multi-client's
`WindowSizeSettings.Ulimit` source-count warning is a legitimate lever.)

## Scaling the fleet across processes

By default the fleet runs in-process. For scale, shard it into subprocesses:

```
./sim-latency run --providers providers.yml --fleet-shards 8
```

spawns 8 `sim-latency fleet` subprocesses, each carrying 1/8 of the providers,
connecting to this run's services. To add providers from another machine, run
`sim-latency fleet` standalone against the run's api/ws urls (see
`sim-latency fleet --help`).

## Network impairment model

Only providers are impaired (clients are unimpaired in v1). Each provider's
platform websocket is wrapped with a bandwidth token bucket, one-way latency +
jitter, and a loss model (an occasional retransmit-sized stall). Because the
connect service measures latency (ws ping RTT) and speed (timed transfer) over
that same connection, the server-side scores reflect the impairment with no fake
reporting, and the rate limiter's backpressure produces realistic queuing under
load. A fleet control loop modulates each provider between a base and a degraded
regime during the measured run. Its seeded churn and regime schedules are
anchored to the measurement boundary, not fleet construction, while base
impairment remains active during warm-up. The model is intentionally cheap
(inline, no per-connection goroutine) so 100k connections are affordable — it
trades emulation fidelity for scale, which is the right call for comparing
algorithms.

## FindProviders2 stats samples

Every `FindProviders2` call exports one anonymized sample (`server/stats`): the
loaded candidate pool in scaled-weight order (with reliability, tier, and score)
and the chosen client ids. This traces exactly how sampling, sorting, and
selection behaved, so you can see why a provider was or wasn't picked. In the
local sim the ids are raw, so samples join directly to `providers.yml`.

Samples are written under the run's site directory
(`<site-home>/stats/local/<instance>/findproviders2/*.pb.zst`) as zstd-compressed
length-delimited protobuf (`sample.proto`). In the main environment the same
stream is shipped to object storage.

### Export and diff (competition workflow)

The competition provides an **official** sample dump from main and evaluates a
build against it. Both are exported as a single flat file and compared:

```
# one xz-compressed varint-delimited protobuf file (every protobuf runtime can
# read the framing: a base-128 varint length, then that many bytes of a
# FindProviders2Sample)
bringyourctl stats export-samples-flat --days 7 --out official.pb.xz   # from minio
bringyourctl stats export-samples-flat --from <site>/stats --out eval.pb.xz  # from local segments

# summarize the difference
bringyourctl stats diff-samples --a official.pb.xz --b eval.pb.xz \
    --label-a official --label-b eval          # add --json for machine output
```

The diff reports, for each dump and their delta:

- **matchmaking health** — `pool_count` (candidates seen per call, mean/p95),
  `load_millis` (candidate-load latency, mean/p95), `chosen_count`.
- **selection quality** (the chosen providers, joined back to the candidate pool
  by id) — `selection_lift` (chosen mean scaled-weight ÷ pool mean, how much
  better than uniform the selection is), `chosen_weight`, `chosen_tier` (lower
  better), `chosen_reliability`, `chosen_rel_latency_ms` (lower better),
  `chosen_speed_mbps`, and the `has_speed`/`has_latency` fractions.
- **call shape** — rank-mode split, top caller countries, force-flag fractions.

A better build raises `selection_lift`, `chosen_reliability`, and
`chosen_speed_mbps`, and lowers `chosen_tier`, `chosen_rel_latency_ms`, and
`load_millis` — i.e. it picks faster, more reliable providers without spending
more matchmaking time.

Also available: `bringyourctl stats export-samples --days 7 --out stats.tgz`
(a self-describing tarball of the raw segments), and the `server/stats` bulk
loaders (`LoadStream`, `LoadSegmentDir`, `ReadFlat`, `LoadStreamTyped`) for
reading a corpus back in code.

## How it fits together

```
sim-latency run
 ├─ apply migrations, create the zz region, write ip_overrides into site settings
 ├─ provision identities, simulated egress evidence, client pool, and minted jwts
 ├─ services: N exchange hosts + connect handlers + api + reliability pipeline loop
 ├─ fake site (deterministic loading tree)
 ├─ fleet: providers connect + settle at base impairment; measurement starts
 │          seeded churn + degraded regimes (in-process or sharded)
 └─ client driver: revalidated warm pool with every crawl lane, then Poisson
                    arrivals → crawl through a pooled client → tun → provider
                    egress → site → per-request CSV (stdout)
```

The tool uses the SDK for both providers (`sdk.SimProvider`) and clients
(`sdk.SimClient`), with simulation overrides added to the SDK/connect: extra
websocket headers (fake forwarded-for addresses), a custom dial hook (impairment),
and disabled egress security (the fake site is on a private address).

## Edge cases and gotchas

**Reliability history is per-database, so reuse the same DB carefully.** The
reliability tables accumulate across runs. Running twice against the same
database without a reset mixes the runs' blocks and depresses provider weights
(coveredBlockCount spans both runs while each provider was only up part of the
time), which can empty the market. For a clean measurement pass `--reset` to
`run` (or run `sim-latency reset` standalone): it truncates the reliability/
connection tables and flushes redis. (`--prewarm` re-establishes scores each
run, but stale rows still skew the reliability window — comparisons and A/A
replicates should always start from a reset.)

**Empty market / all `status=0`.** The market is empty — no provider passed the
`FindProviders2` gates. With the default `--prewarm` this should not happen;
check the stderr `prewarm complete; running pipeline` line appeared. Diagnose by
looking at `client_connection_reliability_score` (should be `providers × 3`
rows for connected, valid providers, with each weight equal to that fixture
provider's seeded uptime duty cycle),
`network_client_location_reliability` (fixture providers should have both test
flags and nonzero performance values), and the exported FindProviders2 samples
(`pool_count > 0`). Causes: `--prewarm 0` (cold start needs ~8.4h uptime and
completed live tests); a too-short `--settle` (the pipeline hasn't re-exported
the redis samples yet —
the market needs one `--pipeline-interval` after prewarm); provider provide
modes not yet acknowledged; missing/failing
`provider_egress_health`; missing/stale `provider_egress_location`; or providers
not geolocated to `zz` (below).

**Providers need speed evidence to be selectable.** The score gate excludes a
provider that has none (it scores at the cutoff). Prewarmed runs derive this
evidence from the deterministic fixture; `--prewarm 0` instead depends on the
live synthetic/passive test. If that test timeout exceeds the warm-up budget,
providers
won't be selectable in time. Confirm with the `network_client_speed` row count.

**Egress establishment fails closed before measurement.** Each client opens a
quality window of provider connections, each needing a Public-mode contract.
The warm pool establishes only four client identities at once and must prove
every configured exit and HTTP lane before arrivals start. If it remains
incomplete, inspect the FindProviders2 corpus first: an empty candidate pool is
a scoring/prewarm failure, while a nonempty pool with auth timeouts is an
exchange-capacity boundary. `--no-impair` distinguishes transport impairment
from the rest of the stack.

**`ip_overrides` must be present before the first connection geolocates.** `run`
writes the site `settings.yml` before starting services, so this holds; if you
point services at a `--site-home` you cannot write, or set `WARP_SITE_HOME`
elsewhere, providers won't map to `zz` and won't be selectable. Note the env var
is `WARP_SITE_HOME` (the container mount var `WARP_SITE` is warpctl-only).

**Migrations run on `run`.** `run` applies db migrations (idempotent). A
`region country location not created` panic means the local stack is
unreachable — start `server/local/run-local.sh`.

**Impairment is a model, not an emulator.** Latency is charged once per write
burst (not per frame — per-frame collapses throughput); packet loss is modeled
as an occasional retransmit-sized stall (a reliable ws stream cannot drop
bytes); and because `expected_latency_ms` is a server-side FIXME (always 0),
"relative latency" equals absolute latency — fine in a single-region sim. The
prewarmed ranking seed uses the connection test's round-trip convention (twice
the fixture's one-way latency); request-path impairment still applies on each
read/write direction. Use `--no-impair` for an impairment-free baseline.

**Reproducibility.** A given `providers.yml` fully locks a run (fleet, ids,
impairments, site tree, client arrivals, all seeded). `init` is also
reproducible from `--seed` alone — the same seed regenerates an identical file
(ids included), so the canonical competition config can be distributed as a seed
rather than a large file.

**Scale limits (macOS vs Linux).** macOS cannot source enough connections for a
large `--count`; use Linux, raise `ulimit -n` to the millions, widen
`net.ipv4.ip_local_port_range`, and spread the fleet across `--hosts` ws ports
(and `--fleet-shards` processes). The connect service holds one resident per
provider, so 100k providers is a big-memory box.

**Stats are inert unless enabled.** The FindProviders2 sample stream writes only
when the site dir exists and (non-local env) a vault `stats.yml` salt is set;
otherwise it is a silent no-op — by design, so it is safe on the hot path and in
un-provisioned environments.
