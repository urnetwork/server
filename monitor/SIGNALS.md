# SIGNALS.md — the signal catalog for the monitor service

What the monitor looks for to determine something is wrong. Each signal here
is being encoded as an automated probe in the monitor service — architecture
and probe mapping in MONITOR.md (this directory). History: this file began as
server/MONITOR.md, the distilled incident-diagnosis runbook.

Distilled from the 2026-07-15 incident day (redis cluster instability + pg
coupling + the network-peers pubsub outage) and the preceding two weeks of
database performance work. Every signal here was actually used to diagnose or
verify; every threshold comes from observed healthy/broken values on main.
Updated 2026-07-17 after the transfer_contract planner-stats CPU wall — that
incident was diagnosed and recovered with this doc (new: 2.3 landmine variant,
2.6 open-set canary, 5.8 playbook, query_wait_timeout class). Updated again
2026-07-17 evening after the stale-provider-selection incident (grey dots
while every aggregate was healthy): new 2.7 new-connection rate, 2.8
selection freshness, 3.7 reliability-pipeline load, 5.9 grey-dots playbook,
5.3 full-restart aftermath, task-overdue + selection-stale + connects-rate
alert rows, and the §8 deployment-state section. Updated 2026-07-18 after
the connect crash-loop outage (bad overnight build, concurrent map writes in
ForEachMaster callbacks → fleet-wide Exited(2) churn → lb 502 → zero new
connections): new 5.10 crash-loop playbook incl. the post-churn contract
trough, and the §7 ticket-identity discipline (stable target, varying frame)
learned from 61 zombie log-class tickets.

Intended consumer: a monitoring service with read access to pg (primary),
redis (cluster, all nodes individually), and service logs. Each signal below
specifies: WHAT to measure, HOW (query/command), HEALTHY vs BROKEN bands, and
the ACTION line the alert should carry. Section 6 explains how we separated
real issues from noise; section 7 is the alert emission spec.

Related docs: FOLLOWUP.md (open items ledger), redis conf overrides in
xops .../redis/redis.conf.j2, grafana redis-cluster dashboard + alert rules.

---

## 1. Tier-0 vitals (always-on, 60s cadence)

The five numbers that, together, tell you in one glance whether main is fine.
During the incident we polled exactly these in a 65s loop.

### 1.1 Contract creation rate — THE user-facing throughput proxy
```sql
SELECT count(*) FROM transfer_contract
WHERE create_time >= date_trunc('minute', now()) - interval '1 minute'
  AND create_time < date_trunc('minute', now());
```
- HEALTHY: 8,000–13,000/min (daily cycle; compare against trailing-hour median,
  not a constant).
- BROKEN: < 50% of trailing median for 3 consecutive minutes = brownout;
  < 1,000/min = outage (observed during CLUSTERDOWN: 150–700/min).
- SHAPE MATTERS: cliff (one minute 40k→1k) = systemic event (cluster state,
  deploy); sag (slow decline) = partial failure (one sick node, growing
  backlog); ramp = recovery in progress (do not re-alert during ramp).
- Action line: "check 1.2 canaries + redis cluster_state; correlate with
  deploys/restarts in the last 10 min".

### 1.2 Task canaries — the cheapest end-to-end redis probes
`UpdateClientLocations` runs every ~30s, writes redis across many slots,
completes in 3–15s. It is the perfect canary: if redis is sick ANYWHERE on the
write path, it errors within a minute.
```sql
-- completions (locations) in the last 3 minutes: healthy 12–25, broken 0
SELECT count(*) FROM finished_task
WHERE function_name LIKE '%UpdateClientLocations%'
  AND run_end_time > now() - interval '3 minutes';

-- error state of all redis-heavy recurring tasks + THE ERROR TEXT
SELECT split_part(function_name,'.',3) AS task,
       reschedule_error_count,
       left(coalesce(reschedule_error,''), 120) AS last_error
FROM pending_task
WHERE reschedule_error_count > 0;
```
- The error TEXT is diagnostic gold: `CLUSTERDOWN` vs `OOM command not
  allowed` vs `dial tcp <ip>:<port>: i/o timeout` vs `connection refused`
  name the failure mode AND the sick node (see section 4 taxonomy).
- GOTCHA — exponential backoff parks tasks: after N errors, run_at can be an
  hour out. A "quiet" failing task is indistinguishable from a healthy one
  unless you check `run_at` vs `now()`. The monitor must report parked tasks
  (error_count > 0 AND run_at > now() + 5min) — during incidents we manually
  pulled them forward:
```sql
UPDATE pending_task SET run_at = now()
WHERE function_name LIKE '%UpdateClient%'
  AND release_time < now() AND run_at > now() + interval '60 seconds';
```
- GOTCHA — long-running vs stuck: a live run shows claim_time refreshing
  every ~10s (the keepalive bumps claim_time+release_time). Frozen claim +
  future release_time = pre-lease-fix binary or killed worker. claim age >
  2× the task's historical duration (finished_task history) = investigate.

### 1.3 pg idle-in-transaction count — the redis-latency mirror
```sql
SELECT count(*) FILTER (WHERE state = 'idle in transaction') AS idle_in_tx,
       count(*) FILTER (WHERE state = 'active') AS active,
       max(now() - xact_start) FILTER (WHERE state = 'idle in transaction') AS oldest
FROM pg_stat_activity WHERE backend_type = 'client backend';
```
- HEALTHY: idle_in_tx < 30, active < 20, oldest < 1 min.
- BROKEN: idle_in_tx > 100 = redis latency is leaking into pg through
  tx-scoped redis calls (observed 563 during brownouts, 2 when healthy —
  it is a live graph of redis health seen from pg). oldest > 30 min = leaked
  transaction pinning the vacuum xmin horizon (kills autovacuum silently).
- BROKEN: active > 100 with wait_event '-' (on-CPU) = a query-plan CPU wall,
  not load (360–390 seen 2026-07-17 vs ~6 healthy; idle-in-tx elevated too but
  redis was healthy — check 1.4 to disambiguate). pgbouncer kills queued
  clients with query_wait_timeout while direct 5432 connects fine → 5.8.
- KEY INSIGHT: pgadmin-style "connection utilization" is NOT query load.
  Real active backends were ~6 even during the worst incidents. Always split
  by state before concluding anything about load.

### 1.4 redis cluster state + per-node liveness
```bash
redis-cli -p 6379 CLUSTER INFO | grep -E 'cluster_state|slots_fail|known_nodes'
# per-node PING with a hard timeout — a wedged node hangs, it does not error
for p in $(seq 6380 6411); do timeout 2 redis-cli -p $p PING >/dev/null || echo "$p DEAD/WEDGED"; done
```
- HEALTHY: cluster_state:ok, slots_fail 0, known_nodes == expected (32 as of
  the 2026-07-17 phantom purge: 32 slot-holding masters, 0 replicas; the entry
  port 6379 lands on a member — myself shows 6396 — so it is not a distinct
  member; phantoms from restarts/dead replicas inflate this — see 3.6), every
  PING < 100ms.
- BROKEN: any PING timeout = event-loop wedge (the #1 recurring failure —
  process alive, kernel accepting into backlog, event loop starved);
  cluster_state:fail = at least one slot uncovered.
- With cluster-require-full-coverage now `no`, a single dead shard degrades
  1/32 of keys WITHOUT flipping cluster_state on other nodes — the monitor
  MUST check per-node liveness, not just cluster_state.

### 1.5 log error-class rates (per service, per minute) — ALWAYS-ON TAIL
The monitor tails the logs of ALL services AT ALL TIMES, classifying lines
against the section 4 signatures. This is a standing collector, not a
sampled probe: `warpctl logs <env> <service> -f` streams a service's logs
across every block/host from one command (also `--query=`, `--since=`,
`--limit=` for non-tail pulls — the escalation batteries use `--since` to
pull the window around an incident). One tailer per service; a tailer that
dies or goes silent while the service is running is itself a visibility
signal.
Count lines per minute matching each class in section 4. Healthy ≈ 0 for all
classes. Alert on rate, include the class name and one sample line. Error
VOLUME is retry amplification, not incident size — 100k identical lines can
be one sick node × fleet retry loops. The monitor should report class + rate
+ distinct target (ip:port) set, never raw volume as severity. A NEW
signature appearing at rate (an unseen error shape / panic frame) is a
signal even when no known class matches — report it as class `novel` with
the sample line.

---

## 2. pg signal catalog (beyond tier-0)

### 2.1 Active query sampling (what is the load, really)
Repeated snapshots beat any single view. 12 samples × 2s apart, aggregate:
```sql
SELECT state, coalesce(wait_event_type,'-')||':'||coalesce(wait_event,'-'),
       left(regexp_replace(coalesce(query,''),'\s+',' ','g'), 110)
FROM pg_stat_activity
WHERE backend_type='client backend' AND pid <> pg_backend_pid();
```
Aggregate by query shape; the top ACTIVE shapes at sample time are the true
load. On healthy main the persistent actives are exactly two background
workers (client_reliability_running INSERT drain + contract_close keyset
sweep) + the pending_task poll. Anything else persistently active = new.

### 2.2 Wait events on active queries
`LWLock:WALWrite` clusters = WAL pressure (check checkpoint cadence,
max_wal_size — a forced checkpoint every < 5 min melted main earlier this
month). `IPC:MessageQueueReceive` = parallel workers. `Client:ClientRead` on
active = server waiting on client mid-protocol.

### 2.3 Planner-flip detection
The catastrophic mode: stale statistics flip a hot query to a bad plan
(observed: pair-lookup 58ms → 6.1s scanning all open contracts). Signals:
one query's mean time step-changes ×10–100 in pg_stat_statements without a
deploy. Remediation was ANALYZE + a structural index. Monitor: track
per-query mean_exec_time deltas hour-over-hour for the top-20 by calls.

2026-07-17 variant — the rare-value stats landmine (incident playbook: 5.8).
Stats can be systematically wrong, not just stale: a value rarer than
~1/sample (30k rows at default_statistics_target=100) can vanish from the
ANALYZE sample entirely. pg_stats then records n_distinct=1 with MCV
{other}@1.0 and the planner treats `col = rare_value` as ~0 rows — walking
the rare-value range of ANY index looks free (cost ≈ 2). transfer_contract.open
at steady state is ~30k true of 530M rows (~6e-5): every ANALYZE wrote {f}@1.0,
so both hot pair lookups (queryids -8886165072987082751 pair/earliest-origin,
-9081667096631174736 open-pair LEFT JOIN contract_close) ran an O(open-set)
plan — Index Cond `open = true` only on
transfer_contract_open_payer_network_id_transfer_byte_count, pair columns as
Filter. Latent at 30k open (the inflated lifetime means, 160/694ms); at 700k
open it was 7–18s/call × ~350 stacked backends × ~988k buffer hits (~7.5G)
each = 96 cores pegged.
- Tells (all observed): pg_stats n_distinct=1 on a known-two-valued column;
  reltuples=0 on every open-partial index; idx_scan=0 on the purpose-built
  pair indexes while one wrong index takes all scans (30s snapshot delta);
  current mean ≫ lifetime mean from a 60s pg_stat_statements counter delta
  (18.5s vs 694ms).
- Remediation 2026-07-17: ANALYZE transfer_contract (89s on 530M rows) → MCV
  {f,t} {0.9985,0.0015} → plan flipped to transfer_contract_pair_open_create_time
  (4,588ms/988,593 buffers → 0.68ms/9 buffers); active backends 387→2 within a
  minute.
- DURABLE FIX still pending as of 2026-07-17: `ALTER TABLE transfer_contract
  ALTER COLUMN open SET STATISTICS 10000;` then ANALYZE (3M-row sample sees a
  6e-5 value reliably) — without it the next steady-state ANALYZE re-arms the
  mine.

### 2.4 Vacuum health
```sql
SELECT relname, n_dead_tup, last_autovacuum FROM pg_stat_user_tables
ORDER BY n_dead_tup DESC LIMIT 10;
```
n_dead_tup > 10M on a hot table, or oldest idle-in-tx > 30 min (pins xmin
fleet-wide) → warn. (Autovacuum thresholds are hand-tuned per giant table;
default scale factors never fire on 600M-row tables.)

### 2.5 Task-system meta-health
- finished_task per-function duration percentiles vs history (regression
  detector — e.g. scores export 12.5min normal; 37min during recovery).
- Duplicate concurrent executions (pre-lease-fix signature): same function
  claimed while a previous run is mid-flight — claim_time churn every ~30-60s
  with error count frozen.
- pending_task rows with reschedule_error_count ≥ 20 = something failed for
  hours (observed 23-28 during the day). Every such row is an incident.

### 2.6 Open-contract set size — the close-backlog canary
```sql
SELECT count(*) FROM transfer_contract WHERE open = true;
-- walks only the open partial index; seconds even under load
```
- HEALTHY: ~10–50k (29,981 at steady state after the 2026-07-17 recovery;
  pre-incident hourly residue was ~8k).
- BROKEN: > 150k and rising = closes not keeping up (CloseExpiredContracts
  stalled or timing out; its healthy run is seconds, 20–25 min when broken).
  While the 2.3 landmine plan is live, every pair lookup degrades linearly
  with this number, and the growth is the feedback loop's fuel (slow closes →
  bigger open set → slower pair queries → slower closes). 700k at the
  2026-07-17 peak. Observed drain after the fix: ~440k closed in 8 min, so a
  high reading self-heals fast once close runs are healthy — alert on
  sustained rise, not a spot value during recovery.

### 2.7 New-connection rate — existing-sessions vs new-connects discriminator
```sql
SELECT date_trunc('minute', connect_time), count(*)
FROM network_client_connection
WHERE connect_time >= now() - interval '10 minutes'
GROUP BY 1 ORDER BY 1;
-- compare against the same window 1h ago, not a constant
```
- Contract rate (1.1) proves EXISTING sessions are moving data; this proves
  NEW connections are being established. They fail independently: on
  2026-07-17 evening a user "can't connect" report arrived while both were
  healthy (~6,300–7,400/min, identical hour-over-hour) — which redirected
  the diagnosis to the ping/selection path (5.9) instead of the transport.
- HEALTHY: within ~±20% of the same minute-of-day an hour ago (diurnal).
- BROKEN: sustained < 50% of the hour-ago window = new connects failing
  (auth, lb, or announce path) even if contract rate still looks fine on
  long-lived sessions.

### 2.8 Provider-selection freshness — the score-cache staleness canary
`FindProviders2` (the app's provider list) reads ONLY the redis
`{cs_<fm>_<rank>_<callerLoc>_<targetLoc>}` score cache (counts `c_l`/`c_g`,
filters `f_l`/`f_g`, samples `s_l_N`/`s_g_N`), and that cache has exactly ONE
writer: the recurring `UpdateClientScores` task, writing with ttl 18000s (5h).
Two freshness reads:
```sql
-- completion gap: healthy is back-to-back runs, 12–50 min each
SELECT max(run_end_time) FROM finished_task
WHERE function_name LIKE '%UpdateClientScores%';
```
```bash
# key age = 18000 - TTL on any sampled {cs_*} key (run on its owning node)
redis-cli -p <port> TTL "{cs_...}s_l_0"
```
- HEALTHY: last completion < ~1h old; sampled cs_ ttl not far below 18000.
- BROKEN: completion gap > ~90 min = apps are being served a stale provider
  snapshot — every provider that disconnected or changed provide mode since
  is misrepresented (2026-07-17: a 2.5h gap after the redis outage served a
  19:54 snapshot; apps pinged disconnected/stream-only zombies and no dots
  turned green — playbook 5.9).
- CLIFF: if no rebuild completes within 5h of the last, the cs_ keys EXPIRE
  and selection goes from stale to EMPTY — strictly worse. The gap alert
  must fire long before the ttl.
- The task-overdue signal (§7) is the leading indicator: the rebuild grinding
  past 2x its p95 is what precedes the gap.

---

## 3. redis signal catalog

### 3.1 Per-node memory table (the skew detector)
For each master: `INFO memory` → used_memory, maxmemory, pct.
- HEALTHY: all nodes within ~2× of each other (fleet baseline was 3–8G).
- BROKEN: any node > 85% of maxmemory (warn) / > 92% (page); any node > 3×
  the fleet median (skew — either a hot key family or un-drained piles).
- volatile-ttl POLICY IMPLICATION: eviction can only touch TTL'd keys. A node
  full of no-TTL keys at maxmemory rejects ALL writes (`OOM command not
  allowed`) while reads keep working and cluster_state stays ok — invisible
  to naive health checks, devastating to write paths. Monitor writes-error
  class per node (or canary tasks, 1.2) to catch it.

### 3.2 Memory attribution: dataset vs client buffers
```
INFO memory → used_memory_dataset, mem_clients_normal + mem_clients_slaves
```
(field-name correction 2026-07-17: there is no `used_memory_clients` key in
the deployed redis; the client-buffer total is `mem_clients_normal` +
`mem_clients_slaves`)
THE decisive discriminator for "why is this node growing": dataset growth =
keys (find the family, 3.3); client-buffer growth = output buffers, i.e.
pubsub/slow consumers (observed 41–49G RES driven by subscriber buffers
during the peers outage). Alert separately:
- used_memory_clients > 25% of used_memory or > 2G → "client/pubsub buffer
  accumulation; check CLIENT LIST omem + subscriber consumer health".

### 3.3 Keyspace family histogram (what is growing)
```bash
redis-cli -p <port> --scan --count 5000 \
 | sed -E 's/[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}/<id>/g' \
 | sort | uniq -c | sort -rn | head -20
```
Shapes, not bytes (a few huge keys are invisible here — pair with
`--memkeys`). Run LC_ALL=C. This is how the `{cs_}` concentration and the
legacy pile families were identified. The monitor can run this on the
fullest node daily + on any skew alert, and diff family counts week-over-week
(a family growing without bound = missing TTL — the recurring disease).

### 3.4 Node process signals (host-level)
- top: a redis process at >200% CPU = io-threads + lazyfree churn (normal
  under storms, but sustained = investigate); pegged 100% children with VIRT
  matching a parent = BGSAVE fork wave (32 simultaneous forks stall event
  loops — save points were retuned to ~hourly for this).
- VIRT ≫ RES on a redis process = past peak + freed (fragmentation history).
- `dmesg -T | grep -iE 'oom|killed process'` — kernel OOM kills happen when
  Σ maxmemory > physical RAM; the killer takes the fattest process (often a
  serving master). oom-score-adj is now set to sacrifice BGSAVE children
  first, but the alert should still page.

### 3.5 Connection-level signals
- connected_clients per node: baseline ~pool_floor × processes; step change
  +50% in 10 min = reconnect storm or pool misconfig (min_connections is PER
  NODE ×32 — a config of 64 = 2k idle conns per process).
- `CLIENT LIST` sorted by omem: any client > 32mb = a stalled consumer.
- Accept-queue: `ss -lnt` Recv-Q pegged at backlog on a redis port = event
  loop too busy to accept() = wedge in progress (dials time out while the
  process looks alive).
- Client-side (edge hosts): `cannot assign requested address` in logs =
  ephemeral port exhaustion toward one dst (~41k tuples / 60s TIME_WAIT ≈
  680 sustainable dials/sec per destination); drains ~60s after the storm.

### 3.6 Cluster topology hygiene
- `CLUSTER NODES | grep -cE 'noaddr|:0@0'` — phantom entries from restarted
  processes; they break every iterate-the-cluster tool (scripts must filter
  `fail|noaddr|handshake` and `^:`); purge with CLUSTER FORGET on every node.
- Replica count: `CLUSTER NODES | grep -c slave` — 0 means no failover
  exists and any single wedge is a partial outage until manual restart.
- known_nodes drift vs expected = membership event worth a warn.
- 2026-07-17 purge: 5 `slave,noaddr` phantoms (dead old replicas) removed with
  a per-node loop — each node FORGETs the IDs in ITS OWN noaddr list (views
  differ; every FORGET returned OK, "Unknown node" is tolerable). All 32 views
  now known=32/state:ok. Replica count is genuinely 0 (the phantom "slaves"
  were the only slave entries): no failover exists, any single wedge is a
  partial outage until manual restart — standing risk, unchanged.

### 3.7 Reliability/scores pipeline load — busy vs degraded
The reliability pipeline is the standing top redis load: announce-path
`HINCRBY client_reliability_stats.<block>` (observed ~7,500/s on one node)
plus block-drain and score-rebuild `HGETALL`s of those giant hashes (200–290ms
per call in slowlog). Two or three nodes hot at 5–10x fleet ops with this
exact command mix is this pipeline, not an incident — attribute before
alarming:
```bash
# 10s command-rate delta names the load
redis-cli -p <port> INFO commandstats   # snapshot, sleep 10, snapshot, diff calls
redis-cli -p <port> SLOWLOG GET 8       # the key names attribute it
```
- THE busy-vs-degraded discriminator: `redis-cli -p <port> --latency` while
  hot. Observed 2026-07-17: a node at 23,000 ops/s answered 0.31ms avg / 1ms
  max — busy, NOT degraded, and idle-in-tx (1.3) stayed at 0. High ops with
  healthy latency and healthy 1.3 = load to fix structurally, not an
  incident to mitigate.
- When a score/reliability rebuild task grinds (task-overdue, §7), this load
  runs continuously instead of in bursts — the sustained version of this
  signature accompanies a selection-freshness problem (2.8).

---

## 4. Log error-class taxonomy (what each class MEANS)

The single most valuable diagnostic skill from the incident: reading the
error CLASS, not the volume. Classes, causes, and the action each implies:

| Class (grep) | Meaning | Action |
|---|---|---|
| `dial tcp <ip>:<port>: i/o timeout` | Node's accept path starving — process alive but event loop wedged (or SYN drop). | PING that port locally on the redis host: hangs → restart that process; fine → network path. |
| `connect: connection refused` | Port closed: process dead or bound to wrong interface after manual restart. | `ss -lntp` on the host: absent → restart; bound 127.0.0.1-only → restart with correct conf. |
| `connect: cannot assign requested address` | CLIENT-side ephemeral-port exhaustion (redial storm to one dst). | Fix the target node; storm self-drains ≤60s after; do NOT restart the client fleet. |
| `redis: connection pool timeout` | Local pool exhausted for PoolTimeout — backpressure, not the root. Deliberately NOT retried in-client (retry amplifies to livelock). | Find what is slow/stuck consuming the pool (usually a wedged node); check pool_timeouts metric per service. |
| `FATAL: query_wait_timeout` (pgbouncer) | pgbouncer server pool saturated — every server conn busy on slow queries; queued clients are killed at the timeout. A pg-side stall symptom, never a pgbouncer config problem. | Diagnose on direct 5432 (it still connects); check 1.3 active count + db host load → 5.8. |
| `CLUSTERDOWN` | Slot coverage lost (node marked fail + no failover, or majority loss). | CLUSTER INFO/NODES; restart dead nodes; transient ≤ node-timeout during elections is expected and retried in-client. |
| `OOM command not allowed when used memory > 'maxmemory'` | Node at maxmemory and volatile-ttl has nothing evictable (no-TTL keys dominate). Writes fail, reads work. | Identify node (3.1); drain no-TTL piles (cleanup script) or raise ceiling temporarily; NEVER a client-side problem. |
| `pubsub ... channel is full for 1m0s (message is dropped)` | IN-PROCESS consumer stall: the app isn't draining go-redis's channel (usually because its goroutine is blocked on another redis call). While blocked, the socket goes unread → server buffers grow (3.2). | Check what the consumers block on; server-side buffer alert 3.2 is the paired signal. |
| `EOF` / `connection reset by peer` | Server closed the conn (COBL kill, maxmemory-clients eviction, restart). | Correlate with server-side events; retried in-client. |
| `LOADING` / `READONLY` | Node restarting (rdb load) / replica mid-failover. Transient; retried in-client. | Only alert if sustained > 2 min. |
| Panic stack traces (`trace.go` "Unexpected error") | The STACK identifies the load-bearing call path (e.g. AddNetworkPeer → NominateLocalResident = connection-killing). | Rate per unique innermost app frame; a new frame appearing at rate = new incident. |
| `[contract][error] ... Insufficient balance` | Payer network has no usable balance. Runs at a steady background rate (~1,000+/min measured 2026-07-17) from out-of-data free users — presence is NOT an incident. | Watch the RATE: a step-change up = netEscrow drift re-emerging (`bringyourctl contracts reconcile-net-escrow --dry-run`) or a balance-grant regression. |
| `[contract][error] ... Missing origin contract for companion` | A contract request resolved to the companion path (destination usable only as reply traffic — announced stream-only / provide-off / gone) but no reversed origin contract exists. Emitted by the earliest-origin lookup (subscription_model CreateCompanionTransferEscrow). ~90/min background; `companion=false` lines mean NORMAL requests are degrading to this path — the destination's keys are the problem, not the requester. | A sustained step-change = clients being pointed at non-contractable destinations: stale provider selection (2.8, playbook 5.9) or an announce/provide-key regression. Sample failing pairs and check the dest's `{pm_<clientId>}sk_*` keys. |

Volume heuristics: identical lines exploding = one cause × retry loops.
Extract (class, target ip:port, innermost app frame) as the alert identity;
report rate; sample one full line.

---

## 5. Diagnosis playbooks (decision trees actually used)

### 5.1 "Users can't connect" (the composite)
1. Contract rate (1.1): at baseline → partial failure, go 2; collapsed → systemic, go 3.
2. Partial: per-node PING sweep (1.4) → sick node(s) found → wedge playbook 5.2.
   Aggregate-fine-but-individuals-broken is the 1/32 signature: keys are
   deterministic per user, so the same users fail every time while metrics
   look near-normal. Never dismiss a user report because aggregates are fine.
3. Systemic: CLUSTER INFO → CLUSTERDOWN → 5.3. cluster ok → check pg (1.3),
   task canaries (1.2), recent deploys; new panic frame in logs → the stack
   names the broken path.

### 5.2 Node wedge (PING hangs locally)
Signature: dial i/o timeouts fleet-wide for one ip:port; local PING hangs;
top shows the process at high CPU; accept backlog full.
Causes seen: synchronous eviction/expiry on the event loop (fixed:
lazyfree-*), BGSAVE fork stalls (fixed: hourly saves), plain overload.
Action: restart the process (with TimeoutStopSec=600 the shutdown SAVE is
protected). Watch for the restart trap: manual restarts that bind loopback
only (use the unit, not ad-hoc redis-server invocations).

### 5.3 CLUSTERDOWN
`CLUSTER NODES | grep fail` names the dead/unreachable node(s). With no
replicas, slots stay uncovered until the process returns. Check for the mass
failure-detection false positive: load storms delaying cluster bus PONGs
(node-timeout was raised 15s→30s for this). After recovery, expect parked
tasks (1.2 gotcha) — pull them forward.

Full-restart aftermath (learned from the 2026-07-17 20:00 edge-6 reboot,
all 32 masters down ~4 min):
- Nodes come back LOADING their rdb (~28 up / 4 loading observed), and up to
  ~2 nodes can carry transient `fail` flags (1,024 slots_fail) for a minute
  after all answer PING — it self-heals as failure-detection converges; do
  not act on slots_fail during the first minutes.
- The rdb restore is up to a save-interval (~1h) STALE. Per-connection state
  self-heals as clients reconnect (re-announce), but derived/aggregated
  redis state rebuilt by recurring tasks (client scores, reliability) stays
  wrong until those tasks complete a full post-outage run — and those runs
  can GRIND (2.5h observed vs 12–50min normal) because they compete with the
  post-outage churn on the same hot hashes. Watch task-overdue (§7) and
  selection freshness (2.8) for HOURS after the cluster itself reads
  healthy; the user-facing tail (5.9 grey dots) outlived cluster recovery by
  2.5h.

### 5.4 OOM wall (writes failing, reads fine)
Task canaries error with OOM class; find the node ≥ maxmemory (3.1); check
dataset-vs-clients (3.2). Dataset → family histogram (3.3) → if legacy
no-TTL piles: run the cleanup script (idempotent, chunked, gated); if a live
family grows unbounded: it's missing a TTL — code fix. Clients → pubsub
buffer playbook 5.5. Temporary relief: raise that node's maxmemory (live
CONFIG SET + conf file), revert after drain. Remember Σ maxmemory vs RAM.

### 5.5 Pubsub buffer blowup (the peers outage)
Signature: 1–2 nodes' used_memory_clients exploding; fleet logs full of
channel-is-full drops; consumer goroutines blocked on other redis calls.
The loop: churn → publishes → stalled consumers stop reading sockets →
server buffers → maxmemory → OOM → more churn. Break the loop at the
publisher (feature kill switch — EnableNetworkPeers) and bound the blast
(client-output-buffer-limit pubsub, maxmemory-clients). Structural fix =
redesign (FOLLOWUP "network peers pubsub").

### 5.6 idle-in-tx storm (pg pool exhaustion)
1.3 count > 100: it is redis latency inside tx scopes (escrow-in-tx is the
one known site until restructured). Verify with the last-query shapes of the
idle-in-tx backends. Fix the redis side; the pg side recovers instantly
(observed 563 → 2 within minutes of the deploy). Kill zombies > 30 min;
idle_in_transaction_session_timeout is the standing guard.

### 5.7 Task parked / task long-running
Covered in 1.2 gotchas: parked = error_count>0 ∧ run_at far ∧ lease expired →
pull forward once the cause is fixed. Long-running = live lease + claim
heartbeat advancing → let it run; compare against finished_task history
before declaring it stuck.

### 5.8 Query-plan CPU wall (the 2026-07-17 planner-stats landmine)
Signature: db host load ≫ cores (490 on 96 observed), hundreds of client
backends active with no wait event (pure on-CPU), %sys 40–50 from scheduler
churn, disk near idle; pgbouncer kills queued clients with query_wait_timeout
while direct 5432 connects instantly; contract rate SAGS over hours (not a
cliff); CloseExpiredContracts 8s → 20-25min with Timeout errors; idle-in-tx
elevated even though redis is healthy (run 1.4 first to rule redis out).
1. psql direct to 5432 (never wait on the pgbouncer queue). Group actives by
   pg_stat_activity.query_id — if 1–3 shapes own the pile (186+159 of 360
   observed), it is a plan problem, not organic load.
2. Confirm the plan: EXPLAIN (ANALYZE, BUFFERS) one shape with real params —
   the tell is a giant Rows-Removed-by-Filter on the wrong index — plus a 30s
   idx_scan snapshot delta (the intended index sits at 0). Get the CURRENT
   per-call cost from a 60s pg_stat_statements counter delta; lifetime means
   dilute the step-change.
3. pg_stats on the flag column: n_distinct=1 / {v}@1.0 = the 2.3 landmine.
   Fix: ANALYZE <table> (89s on 530M rows). Recovery is immediate — active
   collapses to single digits within a minute as in-flight bad-plan
   executions drain; no restarts needed, plans re-resolve on next execution.
4. Aftermath (1.2 gotchas apply): parked tasks self-recover as backoff run_at
   arrives — only pull forward rows parked > 5 min out with expired leases.
   Watch the open set (2.6) drain once close runs return to seconds
   (~440k/8min observed). Expect a brief above-median flush of queued demand
   (11.8k/min seen) before the rate settles at the daily baseline — a ramp,
   not a re-incident.
5. Durable fix: raise the column's statistics target (2.3) so steady-state
   ANALYZE keeps seeing the rare value; verify the next two ANALYZE passes
   keep both values in pg_stats.

### 5.9 Providers/peers visible but cannot be pinged (grey dots)
The 2026-07-17 evening composite: app connects, the provider/peer list
arrives, no dot ever turns green — while EVERY aggregate is healthy. The
control plane and selection API work; the per-candidate contract path fails.
1. Confirm the split: contract rate (1.1), NEW-connection rate (2.7), canary
   (1.2), per-node redis (1.4) all healthy → this is not transport. The list
   arriving proves control/API; grey dots mean pings to the listed
   candidates are being refused.
2. Name the refusal: api logs, class `Missing origin contract for companion`
   (§4) — bucket its rate per 30 min from a long-lived api container's
   docker logs to find the step-change and correlate with the deploy/outage
   clock (§8).
3. Classify the failing pairs (pg): resolve source/dest client_ids to
   network_id and source_client_id. Cross-network + destination is a
   derivative client = the PROVIDER ping path; same-network top-level pairs
   = the network-peers panel instead.
4. Check what the destinations really are (redis): `{pm_<clientId>}sk_<n>`
   EXISTS per provide mode + `network_client_connection.connected` in pg.
   Destinations that are stream-only (only sk_stream present) or
   disconnected are not cold-contractable BY DESIGN — the requester was
   handed a zombie candidate. The question becomes: who served it?
5. Selection freshness (2.8): `UpdateClientScores` last completion vs now,
   and cs_ key ttls. A completion gap ≈ the symptom onset = root cause:
   apps are selecting from a pre-gap snapshot. Check task-overdue (§7) for
   the grinding rebuild, and remember the 5h ttl cliff.
6. Recovery is automatic when the rebuild completes (fresh cs_ writes, hot
   reliability nodes fall back to baseline, dots green on next app
   connect). Clients holding pre-rebuild candidate lists keep failing until
   they re-fetch — a decaying tail, not a re-incident.

### 5.10 Service crash-loop from a bad build (the 2026-07-18 connect outage)
Signature: the service's public endpoint returns 502 (lb up, no healthy
backend); the 2.7 new-connection rate collapses toward 0 while contract rate
initially stays HEALTHY (existing sessions ride until their containers die)
then decays; `docker ps -a` on any edge shows Exited(2) churn on ONE build
tag with container ages of seconds-to-minutes; a fatal-error class appears
in the service logs at process cadence.
1. Root cause in one read: `docker logs` of an exited container — a Go
   `fatal error:` / panic stack names the exact function. 2026-07-18: the
   overnight connect build died in ~30–90s on `fatal error: concurrent map
   writes` in `SubscribeKeyEvents` — go-redis `ForEachMaster`/`ForEachShard`
   run their callbacks CONCURRENTLY (one goroutine per node), so any shared
   write in the callback must hold a lock; with 32 masters the race fires on
   nearly every startup.
2. Mitigation = roll back: `warpctl deploy <env> <service> <last-good
   version> --percent=100`. Identify last-good from the deploy clock (§8) —
   the previous tag that held multiple hours of Up time.
3. Fix + prove: patch, then run the service's tests under `-race`
   (./test.sh already does) — this bug class is deterministic under the
   race detector when a test exercises the path, and the subscriber tests
   here did. GATE BUILDS ON ./test.sh: this exact outage was preventable at
   the door, twice over (the tests existed and the deploy went fleet-wide
   without a canary soak).
4. Recovery verification (8.3 discipline): the fixed tag must hold Up well
   PAST the crash window (>3 min here) before trusting it — staggered group
   starts look like churn for the first ~2 min; distinguish "Up Xm aging"
   from Exited-and-replaced. Then 502 clears, and expect the reconnect
   flush: connects/min spikes far ABOVE baseline (28k/min vs ~6k observed)
   and drains — a ramp, not a re-incident.

Aftermath — the post-churn contract trough: a full-fleet connection churn
(every session killed, twice here) drops contract rate to a TROUGH (~20% of
the pre-outage rate observed) even after connects recover, because
reconnected clients must re-select providers and re-establish tunnels before
their traffic resumes. The trough levels off and ramps; check selection
freshness (2.8) is healthy and then WATCH — do not diagnose the trough
itself as a new incident.

## 6. How we decided what was REAL (methodology)

1. **One discriminating measurement before any action.** Every hypothesis got
   a single cheap test that could kill it: dataset-vs-clients memory split;
   local PING vs remote dial; UPDATE 0 rows vs rows returned; control
   experiment on pristine HEAD (is this failure pre-existing?). If a check
   can't distinguish two hypotheses, it's the wrong check.
2. **Error text over error volume.** Class + target + frame identifies the
   incident; volume only measures retry amplification. The monitoring service
   should dedupe by identity and report rate.
3. **Aggregate + individual, always both.** Near-baseline aggregates with
   individual failures = deterministic partial failure (hash-slot semantics).
   This inverted "metrics look fine" twice during the day.
4. **Correlate with control-plane events first.** Deploys, restarts, config
   pushes explained most step-changes (claim_time jumps = worker restart;
   contract cliff at 13:05 = restart shock). The service should ingest a
   deploy/restart event feed and annotate every alert with "last change N
   minutes ago".
5. **Distinguish root, amplifier, and symptom.** Wedged node = root; 5-min
   pool timeouts, per-call PING, retry loops, 2048-conn pool floors =
   amplifiers; pg idle-in-tx, port exhaustion, pubsub drops = symptoms.
   Fixing amplifiers without the root just softens the next incident —
   record both, fix the root first, de-amplify second.
6. **Feedback loops get broken at the coupling, not tuned at the knobs.**
   Every knob we turned during the pubsub loop (COBL, maxmemory bumps,
   timeouts) delayed the collapse; only decoupling (registration off the
   critical path, then the kill switch) ended it. If a system re-degrades
   after each mitigation, look for the loop.
7. **Mitigations must be idempotent and reversible** (pull-forward UPDATEs,
   CONFIG SET, temporary ceilings, chunked+gated cleanup). If a mitigation
   can't be safely re-run or undone, it's a change, not a mitigation — and
   it waits for diagnosis.
8. **Watch the recovery, not just the fix.** Every fix got a watcher with an
   explicit success signal (error count resets to 0, completions resume,
   rate returns to trailing median) and auto-retry of parked work. "Deployed"
   is not "recovered".

## 6b. Issue → actionable item template

Every confirmed mechanism became a FOLLOWUP.md entry with this shape (the
monitoring service should emit alerts in the same shape):

```
SYMPTOM   what a human/user observes (with the tier-0 number that moved)
MECHANISM the causal chain, one sentence per link
EVIDENCE  the discriminating measurement + file:line for code causes
SEVERITY  user-facing impact chain position (connect > tasks > freshness)
ACTION    the exact next command or code change, and who owns it
VERIFY    the signal that proves it fixed (and its healthy band)
```

## 7. Alert emission spec (for the future service)

Tier-0 (page):
| id | source | check | threshold | payload extras |
|---|---|---|---|---|
| contracts-collapse | pg | 1.1 vs trailing-hour median | <50% for 3 min | last deploy age; canary states |
| canary-dead | pg | 1.2 locations completions/3min | == 0 | last_error text of all failing tasks |
| node-unreachable | redis | 1.4 per-node timeout PING | any, 2 probes | ip:port; ss backlog if host access |
| cluster-state | redis | cluster_state / slots_fail | != ok / > 0 for 60s | failing node list |
| node-mem-critical | redis | used/maxmemory | > 92% for 2 min | dataset vs clients split; top families |
| oom-writes | pg+logs | OOM class in task errors or logs | any sustained 2 min | node attribution from error text |
| active-pileup | pg | 1.3 active client backends | > 100 for 2 min | top query_ids by count; wait-event split; db host load |

Tier-1 (warn):
| id | source | check | threshold |
|---|---|---|---|
| task-parked | pg | error_count>0 ∧ run_at>now()+5min ∧ lease expired | any |
| task-overdue | pg | claim keepalive live ∧ run_at > 10min past ∧ overdue > 2× function's 7-day p95 | any (the 2026-07-17 UpdateClientScores 2.5h grind froze provider selection: stale {cs_} scores → apps offered dead providers → pings refused) |
| task-duration-regression | pg | run duration vs 7-day p95 per function | > 2× |
| idle-in-tx | pg | 1.3 count / oldest | > 100 / > 30 min |
| node-mem-high | redis | used/maxmemory | > 85% for 5 min |
| mem-skew | redis | max/median used across nodes | > 3× |
| client-buffers | redis | used_memory_clients | > 25% of used or > 2G |
| clients-spike | redis | connected_clients step | +50% in 10 min |
| pubsub-drops | logs | channel-is-full rate | > 10/min/service |
| port-exhaustion | logs | cannot-assign rate | any burst > 100/min |
| new-panic-frame | logs | unseen innermost app frame at rate | > 5/min |
| phantom-nodes | redis | noaddr/:0 entries in CLUSTER NODES | > 0 for 1h |
| zombie-tx | pg | idle-in-tx xact age | > 30 min |
| dead-tuples | pg | n_dead_tup hot tables | > 10M |
| replica-cover | redis | CLUSTER NODES slave count | < expected |
| open-set-size | pg | 2.6 open-contract count | > 150k sustained 10 min |
| stats-landmine | pg | pg_stats n_distinct=1 on transfer_contract.open, or any open-partial index reltuples=0 after analyze | daily check |
| connects-rate | pg | 2.7 new-connection rate vs same window 1h ago | < 50% sustained 5 min |
| selection-stale | pg | 2.8 UpdateClientScores completion gap | > 90 min (page at > 3h — ttl cliff at 5h) |
| missing-origin-rate | logs | §4 companion-origin class rate vs its ~90/min background | sustained > 3x background |
| keyevent-config-drift | redis | 9.1 notify-keyspace-events class SET per node | any node divergent from the fleet (all-off = healthy dark state) |
| pubsub-conn-shape | redis | 9.1 CLIENT LIST TYPE pubsub count per node | warn > 300; page > 1,000 (O(clients) = the v1 outage shape) |

Every alert carries: identity (class+target+frame), rate, one sample, the
matching playbook section (5.x), the ACTION line, and last control-plane
event age. Alerts auto-resolve when the signal returns to its healthy band
for 5 minutes, and emit the resolution (recovery confirmation is part of the
loop, per 6.8).

Identity discipline (learned 2026-07-18): the identity's `target` must be
the STABLE thing the healthy signal is emitted for (the service, the host),
and per-incident attribution that varies between observations (an ip:port
from a log line, a task name) belongs in `frame`. Healthy resolution matches
(probe, class, target) ignoring frame; an identity keyed on a varying value
opens tickets that no later healthy finding can ever match — observed as 61
zombie dial-io-timeout tickets accumulated across one outage.

---

## 8. Deployment state — the source of truth for "what changed"

§6.4 says correlate every step-change with a control-plane event first. The
authoritative source of "what is running" is NOT git/code — it is the
containers actually running on the hosts. Read them directly (learned the
hard way 2026-07-17: a git-based "the fix isn't deployed" conclusion was
wrong — the build packaged the working tree).

### 8.1 Read deploy state from the edges, not from git
Tools available on the monitor machine:
- `by-ip <host>` resolves host→ip; `by-pass <host> by` returns the sudo
  password (docker needs sudo as `by`; feed it via `sudo -S -p ''` over ssh
  **stdin**, never argv).
- `warpctl logs <env> <service> [<blocks>...] [--query=] [--since=]
  [--limit=] [-f]` streams a service's logs fleet-wide — the transport for
  the always-on log tail (1.5) and for pulling incident windows
  (`--since=<duration>`) in escalation batteries. Also `warpctl ls versions
  <env> [<service>]` reads the version registry the edges poll — the
  publish-side half of the deploy clock (8.2 reads the edge side).
Then:
```bash
sudo -S -p '' docker ps --format '{{.Names}}\t{{.Image}}\t{{.RunningFor}}\t{{.Status}}'
```
- Image = `bringyour/main-connect:<date>-outerwerld-<buildnum>`; the numeric
  `buildnum` is monotonic (higher = newer). Container name =
  `main-connect-<group>-<ver>-<instance>` (groups beta, g1..g4). The build
  number + container start time IS the deploy clock — annotate every ticket's
  CONTEXT with it.
- **git HEAD is NOT the deployed version.** Builds can package the working
  tree (uncommitted edits included) or lag HEAD. Never infer "the fix is/isn't
  live" from `git status`/`git log` — inspect the running image, and confirm
  with the behavior signal (8.2).

### 8.2 The graceful-handoff drain — multiple build generations run at once
A connect deploy does NOT replace the old containers atomically. New-build
containers start and take NEW connections; the OLD-build containers keep
serving EXISTING connections and drain over time. Observed 2026-07-17: two
builds up simultaneously on every edge — `995097210` (~2h, the pre-fix build)
alongside `995148990` (~13m, the fixed build), 4–5 process groups each.

Consequence: a change to per-connection behavior (e.g. the peer whale gate,
`model.NetworkPeersEnabled`) only takes effect for a given connection once
that connection lands on a NEW-build container. So the effect **lags the
deploy** by roll time + old-container drain + client reconnect + any server
TTL (the peer connected-zset drains over `ExchangeResidentTtl`=300s after the
last old resident stops refreshing).

Detection rule: a fix is not "live" until the OLD build tag **disappears**
from `docker ps` on the edges — "new containers are up" ≠ "old behavior is
gone." Track the old build-tag count falling to 0, not just the new tag
appearing.

### 8.3 Confirm "live" with the behavior signal, not just the tag
The decisive proof a deploy took effect is the target signal moving, keyed to
the deploy clock. The signals to watch, in the order they clear (network-peers
whale, 2026-07-17):
- **Registrations stopped** = the per-network event counter `{np_<id>}eid`
  FREEZES (a frozen write-counter is the cleanest "the gate is live" signal).
  CAUTION: `eid` churn is bursty — a ~4s spot sample can catch a no-increment
  window and read as frozen when it is not (this misled the 2026-07-17 watch).
  Judge it over ≥30–60s (the whale kept climbing ~13/s while the old build was
  still up), and only trust "frozen" once the OLD build tag is gone (8.2).
- **The registry drains** = `{np_<id>}connected` ZCARD falls over
  `ExchangeResidentTtl` (~300s) after registrations stop. Beware the
  confounder: the connected count also moves with diurnal connect/disconnect,
  so a partial drop while the old build is still up is NOT proof the gate is
  live — pin it to the eid freeze + old-tag disappearance.
- **The shard unloads** = 6410 `zrangebyscore`/`hgetall` usec_per_call and
  instantaneous_ops fall back to fleet baseline.
- **The symptom clears** = pg idle-in-tx falls back under 30 (the redis-latency
  mirror, 1.3), lagging the shard unload.
Read these in order; the earlier signals confirm the fix is live before the
downstream symptom has finished recovering. Do not call stabilization from any
one signal or a short sample — require the old build gone AND the write-counter
frozen AND the shard unloaded.

### 8.4 Deploy is an annotation, not an alert
The monitor reads this clock only to annotate tickets ("last deploy N min ago;
builds running X, Y; old-tag drain in progress"). A signal that step-changes
within ~10 min of a build-tag change is deploy-correlated — correlate before
diagnosing, and do not re-alert during a post-deploy recovery ramp (1.1).

## 9. Key-event delivery (PEERSSTREAMS2)

Signals for the redis keyspace-notification transport for peers + stream hops
(PEERSSTREAMS2.md), meaningful once `KeyEventDelivery.Enabled` is on:

- `urnetwork_key_events_dispatched_{peers,hops}_total` — should track registry
  churn (connects/disconnects/provide changes; hop opens/closes). Flat during
  churn = events not arriving (notify-keyspace-events off on a node, or the
  subscriber is down).
- `urnetwork_key_event_resubscribes_total` — subscription (re)establishments.
  One per process start; anything sustained = conn deaths or topology flapping.
- `urnetwork_key_event_resyncs_total` — listener resyncs; spikes with
  resubscribes and registrations, otherwise quiet.
- `urnetwork_network_peer_listener_resets_total` — full-read deliveries. In
  poll mode this is normal change delivery. In key-event mode it should be
  ≈ registrations + resyncs; a SUSTAINED rate above that means the corrective
  poll is repairing dropped events — find the drop before raising the
  corrective interval.
- redis-side cross-check: per-node keyspace publish rate vs write rate of the
  enabled classes; and the standing pubsub connection count, which must stay
  O(processes × nodes), never O(clients) (the v1 outage shape).

### 9.1 Redis-side keyspace-event diagnostics

Concrete probes for "are keyspace events actually being generated and
consumed", ordered from config to delivery. Run against any entry node with
REDISCLI_AUTH set; per-node loops use the CLUSTER NODES enumeration from
xops/redis-set-notify-keyspace-events.sh.

- **Config is live on EVERY node** = `CONFIG GET notify-keyspace-events`
  per node returns the "Kg$sx" class set. CAUTION: redis normalizes the flag
  string (order and K/E expansion can differ) — compare as a SET of classes,
  not string-equal. One node missing classes = silent no-events for that
  node's slots only, which reads as "some networks update, others don't"
  (slot-striped staleness, the telltale). Drift source: a node
  restarted/provisioned outside the templated conf — rerun
  redis-set-notify-keyspace-events.sh (idempotent).
- **Generation is live** = `PSUBSCRIBE '__keyspace@0__:{np_*}eid'` on a node
  prints `incrby` on peer churn (works on the PRE-deploy build too — version
  bumps are string-class); post-deploy, `'__keyspace@0__:{np_*}p:*'` prints
  `set`/`del`/`expired` per peer transition. A quiet probe during visible
  churn = generation off on that node (check config, above) — remember
  events are emitted ONLY by the node that owns the key's slot, so probe the
  slot owner (`CLUSTER KEYSLOT <key>` + `CLUSTER SLOTS`), not a random node.
- **Subscribers are present and bounded** = redis_exporter
  `redis_pubsub_patterns` per node ≈ 2 × connect processes (each process
  psubscribes the peers + hops patterns on every master); cross-check
  `CLIENT LIST TYPE pubsub | wc -l`. Zero = the exchange subscriber is not
  running (old build, or `KeyEventDelivery.Enabled=false`); a value scaling
  with CLIENT count instead of process count is the v1 outage shape — treat
  as an incident, not a tuning item.
- **Generation cost** = per-node CPU vs pre-rollout baseline. Enabling the
  classes adds publish + pattern-match work on EVERY write in those classes
  cluster-wide (accepted, PEERSSTREAMS2.md §10.2) — a step change at
  rollout is expected; a step change at any OTHER time means a new write
  workload landed in the enabled classes.
- **Delivery is being used** (app side, ties back to §9) = the §9 dispatched
  counters move with churn while `urnetwork_network_peer_listener_resets_total`
  stays ≈ registrations + resyncs. Resets climbing with a healthy redis side
  = drops between redis and the app (output-buffer kills → check
  `client-output-buffer-limit pubsub` overruns in the redis log, and §9
  resubscribes).

Reading discipline: slot-striped staleness → per-node config; global
staleness with healthy config → subscriber presence; healthy both with
climbing resets → delivery drops. Do not raise the corrective poll interval
while any of these is unexplained.
