# MONITOR.md — monitor service design

A service that runs in a loop and executes automated analysis against a
production environment to detect issues affecting product reliability. It
**detects**; it does not diagnose root causes and it does not fix. Its output
is an issue "ticket": a detailed textual summary of what was observed vs the
baseline that was expected, pinpointed with real table names, key names,
index names, service names, host names, program names, and observed values.
Tickets are handed to a separate specialized system that diagnoses and
iterates on the production environment until the issue is fixed.

The signal catalog — WHAT to measure, HOW, healthy/broken bands — lives in
SIGNALS.md (this directory). This document is the architecture of the program
that encodes those signals as automated probes.

Related: FOLLOWUP.md (open items ledger), SIGNALS.md §7 (the alert emission
spec this service implements).

---

## 1. Principles

1. **Detect, don't diagnose.** A ticket says *that* something is wrong and
   *exactly what was observed*; the causal chain is the downstream system's
   job. The monitor still runs the cheap evidence-collection steps from the
   SIGNALS.md playbooks (§5) when a signal trips, because evidence gathered
   at detection time (a 30s idx_scan delta, the error text of a failing
   task) is often gone by diagnosis time.
2. **Deterministic probes.** Every signal is coded Go: a query/command, a
   band, an evaluator. No LLM in the detection loop — reproducible, cheap at
   60s cadence, and its own load on prod is bounded and predictable.
   (An LLM enrichment stage over collected evidence is a possible later
   addition, outside the loop.)
3. **First, do no harm.** The monitor observes an environment that is
   sometimes melting down. Every pg session sets `statement_timeout` and
   `default_transaction_read_only = on`; every ssh command has a hard
   timeout; per-resource concurrency is capped (max 1 in-flight diagnostic
   battery per host); a probe that times out is *recorded as an observation*
   (often the strongest signal — e.g. a hung redis PING), never retried hot.
   The monitor never mutates the monitored systems.
4. **Baseline over threshold.** Static bands from SIGNALS.md are the floor,
   but the strongest detections are deviations from *learned normal* ("a ton
   of table-scan queries when normally we don't have that"). The monitor
   keeps its own local history to compute trailing medians and same-hour
   bands. Its state store must be independent of everything it monitors.
5. **Measure directly from the source of truth.** Deployment state comes
   from the warp logs (journalctl) and docker container status on the edge
   hosts themselves — not from a side-channel feed or a dashboard. The same
   rule everywhere: pg state from pg_stat_* on the primary, redis state from
   the nodes' own INFO/CLUSTER NODES, host state from the host. Derived or
   aggregated sources (grafana, exported metrics) are never a probe input.
6. **Identity, not volume.** Dedup key = (signal id, class, target, frame)
   per SIGNALS.md §6. Rate is reported; volume is never severity. One ticket
   per identity, updated in place, auto-resolved when the signal returns to
   its healthy band for 5 minutes (§7).
7. **Aggregate + individual, always both** (§6.3). Per-node redis probes,
   not just cluster_state; per-query-id pg breakdowns, not just counts.

## 2. Topology and access

The monitor is deployed with special permissions: it can ssh into any host
in the environment as the `monitor` user and run commands — psql on the db
host, redis-cli on the redis hosts, top/ss/dmesg/journalctl/docker anywhere.
ssh authentication is delegated to `~/.ssh/config` (host → IdentityFile),
assumed set up on whatever runs the monitor; no key material lives in
`vault/<env>/monitor.yml` — it carries only the login user, host roles,
overlay IPs, and redis ports.

**ssh-exec is the universal transport.** Every command in SIGNALS.md is
written as a run-on-the-host command, and that is exactly how the monitor
executes them: `ssh monitor@<host> '<command>'` with stdin for payloads
(SQL batteries; secrets on stdin line 1, never argv). This has three
consequences:

- The monitor works from anywhere that can reach the hosts' ssh: the LAN
  (deployed) or the VPN overlay (local development on a workstation, where
  LAN IPs like 192.168.51.x are NOT routable but 172.28.208.x is). Address
  selection is a config knob, not an architecture change.
- No pg/redis client libraries needed for the remote side; psql/redis-cli
  on the hosts are the client. Output is parsed (psql `-A -F'|' -t` for
  machine-readable rows; `INFO`/`CLUSTER NODES` are line protocols).
- Direct TCP connectors (pgx to 5432, go-redis to nodes) are a later
  in-LAN optimization behind the same connector interface, not a
  requirement.

**Host tools available on the monitor machine** (assume present): `by-ip
<host>` resolves an inventory host name to its address; `by-pass <host>
by` returns the sudo password for that host; and `warpctl` — in particular
`warpctl logs <env> <service> [<blocks>...] [--query=] [--since=]
[--limit=] [-f]`, which streams a service's logs fleet-wide from one
command (the transport for the always-on log tail, §3.7) and `warpctl ls
versions <env>` (the publish side of the deploy clock). Two consequences: (1) the
monitor can resolve addresses via `by-ip` instead of storing IPs, so
`monitor.yml` need only carry host names + roles (the overlay/LAN IPs become
a fallback); (2) commands that need root — `docker ps` for deploy state
(SIGNALS.md §8), `dmesg`, some `ss`/`journalctl` — run under sudo with the
password fed via `sudo -S -p ''` over ssh **stdin**, never argv:
`by-pass <host> by | ssh <host> "sudo -S -p '' docker ps ..."`. The control-
plane clock (§9) reads deploy state this way — the running container image
tags on the edges, not git.

The monitor has full vault + config access (the standard WARP_HOME
resolvers) and reads every shared fact from its source of truth rather than
duplicating it: pg credentials from `vault/<env>/pg.yml`, redis credentials
from `vault/<env>/redis.yml`, host LAN IPs from `config/<env>/settings.yml`
routes. `vault/<env>/monitor.yml` (§7) carries only what exists nowhere
else: the monitor ssh identity, host roles, overlay IPs (local dev), and
redis port layout. The pg path has one special rule from
the 2026-07-17 incident: diagnose on **direct 5432**, never through
pgbouncer 6432 — but *also* probe 6432 cheaply, because "6432 queues/dies
while 5432 connects instantly" is itself a documented discriminator
(SIGNALS.md §4 query_wait_timeout).

## 3. Architecture

```
                 ┌────────────────────────────────────────────┐
                 │ scheduler (per-probe cadence, jitter,      │
                 │ per-host concurrency caps, hard timeouts)  │
                 └───────────────┬────────────────────────────┘
                                 │ runs
   ┌─────────────────────────────▼─────────────────────────────┐
   │ probes (one per SIGNALS.md signal)                        │
   │   tier-0: 60s   tier-1: 5m–1h   daily: stats-landmine etc │
   └───────┬───────────────────────────────────────────────────┘
           │ via                       on trip │
   ┌───────▼────────────┐      ┌───────────────▼───────────────┐
   │ connectors         │      │ escalation batteries          │
   │  sshExec (universal│◄─────┤ (playbook §5 evidence steps,  │
   │  transport)        │      │  run once per trip to enrich) │
   │  pgQuery, redisCmd │      └───────────────┬───────────────┘
   └───────┬────────────┘                      │
           │ observations                      │ evidence
   ┌───────▼──────────────────────────────────▼────────────────┐
   │ evaluator: static bands + learned baselines, hysteresis   │
   │ (N consecutive), auto-resolve (healthy 5 min)             │
   └───────────────────────────┬───────────────────────────────┘
                               │ findings
   ┌───────────────────────────▼───────────────────────────────┐
   │ ticket manager: dedupe by identity, open/update/resolve   │
   │ lifecycle, renders SIGNALS.md §6b shape                   │
   └───────────────────────────┬───────────────────────────────┘
                               │ emits
   ┌───────────────────────────▼───────────────────────────────┐
   │ emitters: console (now) │ webhook (future) │ gh PR (future)│
   └───────────────────────────────────────────────────────────┘
   baseline store: local disk (history ring per metric) — independent
   of pg/redis so it works precisely when they don't
```

### 3.1 Probes

One probe per SIGNALS.md signal, registered with id, tier, cadence, and the
alert-spec row it implements (§7 tables are the authoritative probe list).

```go
type Probe interface {
    Id() string                  // "pg/active-pileup" — matches §7 id
    Tier() Tier                  // TierPage | TierWarn
    Cadence() time.Duration      // 60s tier-0; 5m–1h tier-1; 24h daily
    Check(ctx context.Context, env *Env) ([]Observation, error)
}
```

An `Observation` is a named metric sample plus structured evidence
(query text, node ip:port, error class, sample log line). `Check` returning
an error is itself an observation about reachability (see §3.6) — probes
must distinguish "the check ran and the value is X" from "the check could
not run".

Probes are cheap by construction: tier-0 is five queries/commands per 60s
tick (SIGNALS.md §1 — contract rate, canaries, idle-in-tx/active split,
cluster state + per-node PING, log-class rates). Expensive collection (the
keyspace family histogram §3.3, EXPLAIN with real params) happens only in
escalation batteries or on daily cadence.

### 3.2 Escalation batteries

When a probe trips, the matching playbook's *evidence steps* run once,
automatically, before the ticket is emitted — detection stays cheap at
steady state, tickets arrive pre-loaded with the measurements a diagnostician
would run first (and which are perishable). Mapping:

| trip | battery (from SIGNALS.md) |
|---|---|
| active-pileup | group actives by query_id; 60s pg_stat_statements counter delta for top ids; 30s idx_scan delta on implicated tables; pg_stats n_distinct on known flag columns (5.8 steps 1–3) |
| canary-dead / task-parked | reschedule_error text of every failing task; claim_time freshness vs release_time (1.2) |
| node-unreachable / cluster-state | CLUSTER NODES parse (fail/noaddr list); ss -lnt Recv-Q on the node's port; top snapshot of the process (5.2 signature set) |
| node-mem-* | INFO memory dataset-vs-clients split; CLIENT LIST top omem; on skew: family histogram on that node (3.1–3.3) |
| contracts-collapse | shape classification (cliff/sag/ramp per 1.1); canary states; open-set count (2.6); last control-plane event age |
| oom-writes | node attribution from error text; that node's INFO memory (5.4) |
| new-panic-frame / log classes | rate + distinct target set + one full sample line (§4) |

Batteries are bounded: one battery run per identity per cooldown window, and
they respect the same read-only/timeout budget as probes.

### 3.3 Baselines

Two layers, evaluated together:

- **Static bands** from SIGNALS.md — always active, the floor. Encoded next
  to each probe (healthy band, broken threshold, sustain duration).
- **Learned baselines** — a local time-series of every probe metric
  (60s-resolution ring, ~14 days). Supports: trailing-hour median (contract
  rate is a daily cycle — compare to trailing median, not a constant, per
  1.1); 7-day same-hour band; per-function finished_task duration p95
  (task-duration-regression); week-over-week redis family counts;
  step-change detection (connected_clients +50%/10min).

Persistence: flat append-only files per metric under a state dir (local
disk), loaded on start. Missing/short history degrades gracefully to static
bands only. Nothing about baselines touches pg or redis.

### 3.4 Evaluator

- **Hysteresis:** each threshold carries its sustain requirement from §7
  ("for 2 min" = N consecutive failing ticks at the probe's cadence). No
  single-tick pages.
- **Shape awareness** where SIGNALS.md demands it: contract-rate cliff vs
  sag vs ramp (1.1) — a ramp during recovery suppresses re-alerts; open-set
  spot values during a drain don't re-page (2.6: alert on sustained rise).
- **Auto-resolve:** healthy band held for 5 minutes → the ticket resolves
  and the resolution is emitted (recovery confirmation is part of the loop,
  §6.8).
- **Flap control:** re-open within a cooldown reuses the ticket identity
  with an incremented flap count rather than emitting a fresh page.

### 3.5 Tickets

Identity: `(probe id, class, target, frame)` — e.g.
`(logs/dial-timeout, dial-io-timeout, 192.168.51.193:6389, -)`. One open
ticket per identity. Lifecycle events: OPEN, UPDATE (observed values moved
materially or the battery finished), RESOLVE.

Rendered shape = SIGNALS.md §6b, with baseline made explicit:

```
TICKET pg/active-pileup OPEN 2026-07-17T12:41:03Z tier=page env=main
SYMPTOM   pg (by-us-fmt-5-edge-2) active client backends = 387 sustained 4m
          (threshold > 100 for 2m)
BASELINE  active 2–6 at this hour over trailing 7 days; lifetime means of the
          implicated queries 0.7ms / 1.2ms
OBSERVED  top query_ids: -8886165072987082751 = 186 backends,
          -9081667096631174736 = 159; 92% wait_event '-' (on-CPU); host load
          490 on 96 cores; pgbouncer 6432 killing queued clients with
          query_wait_timeout while direct 5432 connects in 40ms; current
          per-call means (60s counter delta): 4,588ms / 3,548ms
EVIDENCE  transfer_contract idx_scan 30s delta:
          transfer_contract_pair_open_create_time = 0,
          transfer_contract_open_payer_network_id_transfer_byte_count = 916;
          pg_stats transfer_contract.open n_distinct = 1 (MCV {f}@1.0);
          open-contract count 699,045 rising
CONTEXT   last control-plane event: none in 6h (no deploy/restart)
PLAYBOOK  SIGNALS.md 5.8
```

Every ticket carries: identity, tier, environment, the observed values with
real names, the baseline it violates and where that baseline came from
(static band vs learned), perishable evidence from the battery, last
control-plane event age (§6.4), and the SIGNALS.md playbook pointer. No
remediation instructions beyond the playbook reference — fixing is the
downstream system's specialization.

### 3.6 Emitters and self-health

```go
type Emitter interface {
    Emit(ctx context.Context, event TicketEvent) error // OPEN|UPDATE|RESOLVE
}
```

- **console** (now): human-readable render to stdout + one-line JSON to
  stderr (machine-parseable from day one, so the future consumers cost
  nothing to add).
- **webhook** (future): POST TicketEvent JSON to the diagnosing system's
  intake, with a local spool for when it's down.
- **github pr** (future): open a PR/issue per ticket for human-visible
  handoff.

Self-health: the monitor distinguishes three states per target — *healthy
observation*, *broken observation* (ticket about the target), and *cannot
observe* (ssh unreachable / command timeout → a `monitor/visibility` ticket
naming what is now unmonitored, because blindness during an incident is
itself urgent). It also heartbeats (periodic "all probes ran" line) so a
silent monitor is detectable.

### 3.7 Log tailers — the always-on collectors

Logs are a first-class signal (SIGNALS.md 1.5): the monitor tails ALL
services AT ALL TIMES looking for error signatures. Unlike cadence probes,
a tailer is a standing collector: one long-running `warpctl logs <env>
<service> -f` per service, each line classified against the SIGNALS.md §4
taxonomy as it arrives. Per minute, each tailer folds its counts into
findings — (class, target ip:port, innermost frame) identity, rate, one
sample line — through the same evaluator/ticket path as every other probe.
Unmatched error-shaped lines at rate are reported as class `novel` (new
panic frames and unseen failure modes are exactly what a fixed taxonomy
misses). Tailer self-health: a tailer that exits or goes silent while its
service is running restarts with backoff and raises `monitor/visibility`
if it cannot stay up. Escalation batteries pull incident windows
non-interactively with `--since=<duration>` instead of tailing.

## 4. Scheduler and load budget

- Per-probe tickers with jitter; a probe never overlaps itself.
- Per-host semaphore (1 concurrent battery, small cap for probes) so the
  monitor cannot pile onto a struggling host.
- Global kill: SIGTERM drains in-flight commands (same quitEvent pattern as
  the other services).
- Budget at steady state (all tier-0 + tier-1): a handful of
  sub-second read-only queries and PINGs per minute per host — deliberately
  smaller than the 65s hand-polling loop used during the incidents.

## 5. What runs when (initial probe schedule)

| cadence | probes (SIGNALS.md ref) |
|---|---|
| 60s | contract rate 1.1; canary completions + failing tasks 1.2; idle-in-tx/active split 1.3; cluster_state + per-node PING 1.4 |
| 5m | open-set count 2.6; per-node INFO memory 3.1/3.2; connected_clients 3.5; parked tasks 1.2; pgbouncer 6432 reachability; control-plane clock (journalctl warp logs + docker container status per host — feeds every ticket's CONTEXT line) |
| continuous | log tailers §3.7: one `warpctl logs <service> -f` per service, §4 classification per line, per-minute rate findings |
| 15m | pg_stat_statements top-20 mean drift 2.3 |
| 1h | vacuum health 2.4; task duration percentiles 2.5; phantom/replica topology 3.6; zombie-tx 1.3 |
| 24h | stats-landmine check (pg_stats n_distinct on transfer_contract.open + open-partial reltuples) §7; keyspace family histogram on fullest node 3.3; dmesg OOM scan 3.4 |

The log tailers are always-on (a standing `warpctl logs -f` per service),
not scheduled — §3.7. The 1.2 task reschedule_error text remains the
cheapest first read on most log classes and stays as its own probe.

## 6. Package layout

One flat `package main` at `server/monitor` (connect/CODESTYLE.md package
layering: a package must never import its own subpackages; shared code that
several files need is just a file in the package, grouped by filename
prefix — the `bringyourctl` precedent):

```
server/monitor/
  MONITOR.md            this design
  SIGNALS.md            the signal catalog (what "wrong" looks like)
  main.go               docopt usage, probe registry, scheduler loop, --once
  config.go             monitor.yml + pg.yml + settings.yml routes assembly
  conn.go               ssh-exec transport; pg/redis/shell/warpctl runners
  baseline.go           local metric history (trailing medians, retention)
  probe.go              probe interface, finding, tiers, small helpers
  probe_pg.go           tier-0 pg: state split 1.3, contract rate 1.1
  probe_pg_tier1.go     open-set 2.6, pgbouncer, vacuum 2.4, stats-landmine
  probe_tasks.go        canary + parked 1.2, duration regression 2.5
  probe_redis.go        tier-0 redis: cluster state + per-node ping 1.4
  probe_redis_tier1.go  per-node memory/skew/buffers/conns 3.1-3.5, topology 3.6
  battery.go            escalation batteries (5.8 plan wall, 5.2/5.4 node)
  tailer.go             always-on log tailers + §4 classifier + novel class
  ticket.go             identity, lifecycle, hysteresis, §6b rendering
  emit_console.go       console emitter (webhook/github pr deferred)
```

Follows the repo's conventions: built automatically by the existing
Dockerfile `./...` build, docopt CLI, `server.RequireEnv()`/`WARP_ENV` for
environment, `server.Vault.RequireSimpleResource("monitor.yml")` for
credentials.

## 7. Configuration

`vault/<env>/monitor.yml` (vault/main/monitor.yml created 2026-07-17 with
the full inventory; abbreviated shape):

```yaml
ssh:
  user: monitor       # deployed login user (in-lan)
  dev_user: by        # login user for local dev over the overlay
address_mode: overlay # lan (deployed) | overlay (local dev)
hosts:                # only monitor-specific facts; lan ips come
  - name: by-us-fmt-5-edge-2   # from config settings.yml routes by name
    overlay_ip: 172.28.208.182
    roles: [pg-primary]
  - name: by-us-fmt-5-edge-6
    overlay_ip: 172.28.208.177
    roles: [redis-cluster, minio]
    redis: {entry_port: 6379, node_ports: [6380, 6411]}
  # ... service hosts with roles [services], snow [subtensor]
pg:
  port: 5432          # direct; 6432 probed separately as pgbouncer
```

ssh keys are not stored here — `~/.ssh/config` supplies the IdentityFile per
host (assumed set up). Everything else shared is read from its source of
truth, never duplicated here (§2): pg credentials from `vault/<env>/pg.yml`
(password passed on stdin line 1 of each battery, never argv), redis
credentials from `vault/<env>/redis.yml`, LAN routes from
`config/<env>/settings.yml`.

## 8. Development plan

Phase 0 (now): local run from the workstation against main —
`WARP_ENV=main address_mode=overlay`, console emitter. This is the
develop/test loop: every probe is verified against the live environment the
same way the SIGNALS.md queries were verified during incidents.

Run it locally:

```
WARP_HOME=/Users/brien/urnetwork WARP_ENV=main WARP_VERSION=0.0.0 \
  go run ./monitor --once        # one pass, print what would fire
WARP_HOME=... WARP_ENV=main ... go run ./monitor   # the 60s loop
```

1. **Skeleton + tier-0 — DONE (2026-07-17).** main loop + `--once`, sshExec
   connector (conn/), four tier-0 probes (contract rate 1.1, pg state split
   1.3, task canary + parked 1.2, redis cluster + per-node PING 1.4), static
   bands, ticket lifecycle with hysteresis + auto-resolve (ticket/), console
   emitter with §6b rendering + JSON on stderr (emit/). Verified against main:
   correctly opened idle-in-tx (638, the live network-peers whale storm) and
   task-parked (10 AdvancePayment/Circle failures), stayed quiet on healthy
   contract rate / canary / redis cluster. Two bugs found and fixed in the
   first live run: psql `SET` command tags polluting parsed rows (moved to
   PGOPTIONS at connection); `--once` needs `Immediate` mode to surface
   findings without waiting for the multi-tick sustain. (Log-class probe 1.5
   deferred to phase 6.)
2. **Ticket lifecycle — DONE in phase 1** (identity/dedupe/hysteresis/
   auto-resolve; JSON event line on stderr). Additional alerting channels
   (webhook, github pr, persistence/spool) deliberately DEFERRED — console
   only for now (user decision 2026-07-17).
3. **Baselines — DONE (2026-07-17).** Local history store (baseline.go:
   append-only per-metric files under ~/.urnetwork-monitor/<env>/baseline,
   14d retention, compaction); trailing-hour median brownout band live on
   1.1 (guarded: needs 30 samples and median >= 2000); open-set rising
   check 2.6; duration-regression from finished_task history (no local
   store needed); redis medians recorded for future step-change checks.
4. **Escalation batteries — DONE (2026-07-17).** 5.8 plan wall (15s
   pg_stat_statements + idx_scan snapshot deltas diffed in Go — temp
   tables are unavailable read-only — plus the 2.3 pg_stats landmine
   check) wired into active-pileup; 5.2/5.4 redis node battery (memory
   attribution, top omem clients, accept queue) wired into
   node-unreachable / mem-critical / client-buffers; logWindowBattery
   (`warpctl logs --since`) available for incident windows.
5. **Tier-1 + daily probes — DONE (2026-07-17).** Per-node memory table +
   skew + client buffers + connected spike (one ssh round for all 32
   nodes); topology phantoms/replicas; open-set; pgbouncer reachability;
   vacuum dead-tuples; daily stats-landmine. First live pass immediately
   surfaced real findings: transfer_escrow_sweep 23.8M and contract_close
   19.7M dead tuples, node 6400 connected_clients 8.8x fleet median.
   Field-name catch: the deployed redis has no `used_memory_clients` —
   client buffers = mem_clients_normal + mem_clients_slaves (SIGNALS.md
   3.2 corrected). Family histogram 3.3 DONE same day: daily sampled scan
   (300k keys, 200s budget via sshTimeout) on the fullest node, ids
   normalized to shapes, per-family counts recorded as baseline metrics;
   alert = family > 3x its trailing 7-day median and > 20k in sample;
   the full histogram logs daily as the diagnostician's inventory.
   Tailer calibration from the first live pass: two contract-error classes
   promoted out of `novel` with measured background rates
   (insufficient-balance ~1,000+/min, missing-origin-contract ~90/min —
   SIGNALS.md §4 rows added); grafana panic-loop found (real: stale
   CloudWatch datasource, log group missing).
6. **Log tailers — DONE (2026-07-17)** (§3.7): standing `warpctl logs -f`
   per service (service list from `warpctl ls services`), §4 taxonomy
   classifier + novel-class detection, per-minute drain through a probe
   into the same ticket path; restart-with-backoff. Loop mode only
   (--no-tail to disable); not exercised by --once.
7. **Deployment**: provision `monitor` user + vault/main/monitor.yml,
   deploy as a warp service in-LAN; webhook emitter for the diagnosing
   system.

## 9. Decisions and open questions

Decided (2026-07-17):

- **Ticket sink**: console emitter now (stdout render + JSON event line on
  stderr); webhook and github-PR emitters later behind the same interface.
- **Control-plane events** (§6.4): read from the source of truth on the
  edges — the warp logs via journalctl and docker container status
  (`docker ps` state/created-at, `docker events` window) over ssh. A
  control-plane probe maintains the per-host "last deploy/restart" clock
  that every ticket's CONTEXT line reads from. No side-channel feed.
- **Grafana split**: the monitor identifies issues — events/alerts that
  need investigation, delivered as tickets. Grafana collects metrics to
  support decisions: it is the data source for the *fixer/debugger* working
  a ticket, not an issue-identification path. Grafana is for fixing issues,
  not identifying them; the monitor never consumes grafana/prometheus as a
  probe input (principle 5).
- **Runtime**: developed and run locally first (address_mode: overlay,
  ssh_override with the existing workstation access) against main; in-LAN
  deployment as a warp service comes after the probe set stabilizes.
- **vault/main/monitor.yml**: created 2026-07-17 with the §7 shape — ssh
  login user (`monitor`, `dev_user: by` for overlay) and the full 10-host
  inventory with roles. No key material: `~/.ssh/config` supplies the
  IdentityFile per host (assumed set up). The `monitor` user does NOT exist
  on hosts yet — provisioning is an ops task.
- **Vault + config access**: the monitor runs with the standard WARP_HOME
  resolvers and reads shared values (pg/redis credentials, LAN routes)
  directly from vault/<env> and config/<env> — monitor.yml never duplicates
  a fact that has a source of truth elsewhere.

Open:

- **monitor pg role**: probes currently need pg_stat_* and catalog reads;
  a dedicated read-only role (pg_monitor grant) would be cleaner than the
  bringyour app user. Ops task, not blocking.
- **Multi-env**: design is env-parameterized via WARP_ENV; only main is in
  scope until the service is stable.
- **LLM enrichment stage** (post-detection ticket narration, novel-pattern
  sweeps): explicitly out of the loop for now; reconsider after webhook
  handoff exists.
