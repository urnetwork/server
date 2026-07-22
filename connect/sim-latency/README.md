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

## The metric

A run is scored on the CSV rows inside the **measured window** (after ramp +
settle; the boundary is logged to stderr as `MEASURE WINDOW: [start, end]` and
recorded in the run.json side-car). The metric set (computed by
`sim-latency analyze`, decided by `sim-latency compare`):

- **primary: `ttfb_p95_ms`** (tail time-to-first-byte, lower is better) and
  **`throughput_p95_bytes_per_s`** (p95 of large-transfer throughput, higher
  is better; transfers >= 256 KiB only, so it measures bandwidth rather than
  latency)
- **guard: `fail_rate`** — timing/throughput metrics count successes only, so
  the failure rate is a non-inferiority gate: a build cannot win latency by
  dropping hard requests
- secondary (reported, not gating): `ttfb_p50_ms`, `total_p50_ms`,
  `total_p95_ms`, `throughput_p05_bytes_per_s` (the struggling tail),
  `throughput_p50_bytes_per_s`, `goodput_bytes_per_s`

A standard run is a fixed `providers.yml` (which locks the fleet and all seeds),
a fixed settle and duration, and the same server build except for the code under
test. Because the fleet, site tree, client arrivals, and impairments are all
seeded from `providers.yml`, two runs of the same file replay the identical
workload — whether an observed difference is *real* is decided statistically
(see "Comparing runs" below).

## System requirements

The official scale — **~100,000 providers, ~1,000 clients/min** — targets a
big-memory Linux box:

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
market to a stable state: the fleet connects, providers are latency/speed
tested, reliability scores are established, and the selectable set is exported.
Only after warm-up do clients start, so every measured request runs against a
settled market.

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
| prewarm | establish reliability scores for the connected fleet (instant) | `--prewarm 13h` |
| tests | latency + synthetic speed test each provider | `--test-timeout 3s`, `--announce-timeout 2s` |
| settle | let the pipeline propagate scores → selectable set | `--settle 1m`, `--pipeline-interval 10s` |

`--prewarm` writes the final reliability scores directly for every connected
provider (weight 1.0), rather than replaying ~8.4h of history, so it is
effectively instant regardless of the window value; `--prewarm 0` restores the
true cold start. The synthetic speed test defaults to 60s in production; the sim
drops it to `--test-timeout` (3s) because the score gate requires a completed
speed test, so this bounds how soon a fresh provider can be selected. A shorter
`--pipeline-interval` propagates provider state (new tests, churn) into the
selectable set faster.

For the fastest possible warm-up on a small fleet:
`--ramp 10s --settle 20s --pipeline-interval 5s`.

Notes:

- Churny providers (short uptimes in the mixture) still fall out of the market
  live over the run as their reliability degrades — the intended selection
  behavior is preserved; prewarm only sets the initial established condition.
- The measured window is logged as `MEASURE WINDOW: [start, end]`; only rows in
  it count.

## CSV output

One header line, then one row per request (stdout only):

```
t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s
```

- `t_start_ms` — request start (unix ms); compare against the `MEASURE WINDOW`.
- `client` — the client id (raw in local env, so it joins to `providers.yml`).
- `path`, `depth` — the crawled suburl and its depth in the loading tree.
- `status` — HTTP status; `0` means the request never completed (no provider /
  timeout).
- `bytes`, `ttfb_ms`, `total_ms`, `bytes_per_s` — size, time to first byte,
  total time, throughput.

Alongside the CSV, `run` writes a **run.json side-car** (`--meta`, default
`run.json` — name it per run, e.g. `--meta a.run.json` next to `a.csv` so the
tools find it by convention). It records everything the statistical tooling
needs beyond the rows: the measure window, providers.yml sha256 + seed, build
revision, host, flags, warm-pool establishment count, and the per-metric
summaries. `sim-latency analyze --run x.csv` recomputes the summaries from
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
(`ttfb_p95_ms`, `throughput_p95_bytes_per_s`) is significantly better after
Holm correction, and no guard metric (primaries + `fail_rate`) is
significantly worse** at raw alpha. Other outcomes: `b_better`, `mixed`
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

## providers.yml

The locked ground truth. `init` writes the mixture definition plus one concrete
entry per provider. Edit the `providers.mixture` weights/ranges and the `site`,
`clients`, and `subnets` sections, then re-run `init` to re-sample the fleet
(the entry list under `fleet:`). Key sections:

- `region` — the fake country (`zz` / "Sim"); every provider and client lives
  here, matched from the testing subnets by the server's `ip_overrides` hook.
- `subnets` — provider and client testing subnets (RFC 2544 `198.18.0.0/15` for
  providers). Each provider/client presents a unique address from these.
- `site` — the loading tree: `mean_depth` (K), `branching`, body size range.
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
metrics. The default mixture caps hosting providers around 64–256 concurrent
flows, residential 8–32, mobile 4–12.

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
regime over the run. The model is intentionally cheap (inline, no per-connection
goroutine) so 100k connections are affordable — it trades emulation fidelity for
scale, which is the right call for comparing algorithms.

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
 ├─ provision providers + client pool (bulk pg inserts, minted jwts)
 ├─ services: N exchange hosts + connect handlers + api + reliability pipeline loop
 ├─ fake site (deterministic loading tree)
 ├─ fleet: providers connect (ramp), impaired + churning (in-process or sharded)
 └─ client driver: warm pool of SimClients (built during warm-up), then Poisson
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
rows, all `independent_reliability_weight = 1.0`) and the exported
FindProviders2 samples (`pool_count > 0`). Causes: `--prewarm 0` (cold start
needs ~8.4h uptime); a too-short `--settle` (the pipeline hasn't re-exported the
redis samples yet — the market needs one `--pipeline-interval` after prewarm);
or providers not geolocated to `zz` (below).

**Providers must complete a speed test to be selectable.** The score gate
excludes a provider that has no speed test (it scores at the cutoff). The
synthetic speed test fires `--test-timeout` after connect (3s default, 60s in
production). If you raise `--test-timeout` above the warm-up budget, providers
won't be selectable in time. Confirm with the `network_client_speed` row count.

**Egress establishment under the single in-process exchange is the current
scaling frontier.** `FindProviders2` selection is solid (it returns full
candidate pools), but the client→provider→site data path does not yet establish
reliably when many clients come up at once: each client opens a *window* of
2–6 provider connections (`WindowSizeMin/Max`), each needing a Public-mode
contract, so a pool of N clients hits the in-process exchange with ~6N
simultaneous connection+contract setups and the window-client auths time out.
Verified with `--no-impair`: the impairment is **not** the cause (a clean
baseline fails the same way), so this is per-connection harness capacity, not
the algorithms under test. Levers: fewer clients (`clients.pool_size` in
providers.yml), more exchange hosts (`--hosts`), or a smaller multi-client
window. `--no-impair` runs a clean impairment-free baseline.

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
measured latency tracks the impairment's one-way value, not a full RTT. Use
`--no-impair` for an impairment-free baseline.

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
