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
- BROKEN (high side): sustained > 2.5x baseline = a RECONNECT STORM —
  connections establish then die young, so clients cycle. Confirm with the
  median connection lifetime (it halves during churn):
  ```sql
  SELECT date_trunc('hour', connect_time),
         percentile_cont(0.5) WITHIN GROUP (ORDER BY
           EXTRACT(EPOCH FROM (disconnect_time - connect_time)))
  FROM network_client_connection
  WHERE connected = false AND connect_time >= now() - interval '6 hours'
    AND disconnect_time IS NOT NULL GROUP BY 1 ORDER BY 1;
  ```
  First correlate with deploys AND unit restarts (8.5): 2026-07-19 22:55 an
  ansible restart wave took the baseline 2.5k/min to a 7k plateau for 40
  min, a 15k/min final drain burst, then decay to baseline within ~6 min —
  with contract rate, canary, api error rates all healthy throughout. If NOT
  restart-correlated, a storm means something is killing established
  connections (transport, lb flapping, provide churn).
- MEDIAN-POLLUTION CAUTION: a 40-minute storm drags any trailing-hour median
  up to storm levels, so (a) the storm signal un-trips as the window fills,
  and (b) the RECOVERY back to true baseline then reads as < 50% "collapse"
  (false page observed 2026-07-19 23:42: contracts 4.5k/min vs a 10.9k
  churn-inflated median). Judge recovery against a pre-incident window; the
  probes fall back to the trailing-6h median whenever the hour median is
  >= 1.5x it.

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
per call in slowlog). (Key shape after RELIABILITY2 shipped 2026-07-18:
`client_reliability_stats.<block>.<shard>` — 32-way sharded by client id,
drained with chunked HSCAN. Post-deploy the single-node concentration and
the 200-290ms HGETALL slowlog class should DISAPPEAR; their reappearance =
regression.)

Rollout observations (2026-07-18, the live cutover):
- The legacy unsharded field count is NON-MONOTONIC during a fleet roll: it
  tracks the remaining old-build announce volume per block and bounces
  (273k → 16.7k → 162k observed) as edges drain at different paces. Judge
  cutover progress by which build generations remain in `docker ps` (8.2),
  not by the legacy count falling smoothly.
- The legacy hot spot ROTATES per block (the block number is in the key
  name → a new slot each minute), so "which node is hot" moves even while
  the sharded portion is already spread. Verify the spread with short
  per-node `hincrby` DELTAS (e.g. two commandstats snapshots 10s apart on
  several nodes) — lifetime counters dilute the change and a single-node
  instantaneous read can land on the rotating legacy slot. Observed: top
  sampled node fell 7,014/s → 165/s as the fleet cut over.
- `SCARD client_reliability_stats_blocks` ≈ 3 (current + previous + one
  pending) = the drain is healthy through the transition; a growing set =
  the drain is not keeping up or cannot see the keys (wrong build order).
- Deploy order matters: the drain lives in TASKWORKER, the writers in
  CONNECT. Until the new taskworker lands, sharded counters are invisible
  to the old drain (bounded stats loss via the 15-min ttl backstop). Roll
  taskworker first or concurrently — on 2026-07-18 the new taskworker
  landed ~3 min after new connect took traffic; loss window was minimal. Two or three nodes hot at 5–10x fleet ops with this
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
| `[redis][ttl]` (server-side guard, server/redis_ttl_warn.go) | A redis write carried an effective ttl > 120 days, or a raw Go `time.Duration` command/eval arg — go-redis serializes Durations as int64 NANOSECONDS, so an 8h ttl becomes `EXPIRE <key> 28800000000000` (~913,000 years). The 2026-07-20 signature: ~1.1M immortal `s2_sk_*` stream keys from exactly this in the AddToStream eval; nothing in the system keeps a >120d ttl intentionally. | The warning names the command + key: find the write site, pass seconds/ms ints to evals (never a Duration). Clean already-written keys with `bringyourctl streams expire-leaked-ttls`; per-key check: `TTL <key>` in the trillions = this bug. |
| Panic stack traces (`trace.go` "Unexpected error") | The STACK identifies the load-bearing call path (e.g. AddNetworkPeer → NominateLocalResident = connection-killing). | Rate per unique innermost app frame; a new frame appearing at rate = new incident. |
| `urnetwork_connect_contract_failures_total{cause="insufficient_balance"}` (Mimir; `[contract][error] class=insufficient_balance` is a rate-limited exemplar only) | Payer network has no usable balance. Runs at a steady background rate (~1,000+/min measured 2026-07-17) from out-of-data free users — presence is NOT an incident. | The provisioned Grafana rule watches the lossless 5-minute counter rate; >4,000/min for 5 minutes = netEscrow drift re-emerging (`bringyourctl contracts reconcile-net-escrow --dry-run`) or a balance-grant regression. Do not calculate the rate from sampled logs. |
| `asset amount owned by the wallet is insufficient` / `insufficient token balance ... in wallet` (taskworker, circle payment path) | The payout wallet cannot cover pending payouts (usdc on solana — mint EPjFWdd5...Dt1v in the error text). NOT an api failure: every AdvancePayment retry 400s until the wallet is funded, parking the tasks on backoff (decoded 2026-07-18 from the novel class — the full error text names the wallet id, its balance, and the required amount). | Finance/ops: fund the payout wallet (or pause payouts). Task-side symptoms clear on their own once funded and the backoff run_at arrives. |
| `urnetwork_connect_contract_failures_total{cause="missing_companion_origin"}` (Mimir; `[contract][error] class=missing_companion_origin` is a rate-limited exemplar only) | A contract request resolved to the companion path (destination usable only as reply traffic — announced stream-only / provide-off / gone) but no reversed origin contract exists. Emitted by the earliest-origin lookup (subscription_model CreateCompanionTransferEscrow). ~90/min background; `companion=false` means NORMAL requests are degrading to this path — the destination's keys are the problem, not the requester. | The provisioned Grafana rule watches the lossless 5-minute counter rate; >500/min for 5 minutes means clients are being pointed at non-contractable destinations. Use the sampled log only to obtain a failing pair, then check the destination's `{pm_<clientId>}sk_*` keys. |

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
7. POST-CHURN VARIANT (2026-07-19): a rebuild that OVERLAPS a churn window
   is itself polluted, so ONE completion does not recover — the snapshot
   scores "flash clients" that connected for only 9–30 SECONDS during the
   churn (verified: failing destinations' entire connection lifetime sat
   inside the restart wave). Apps re-fetch after that completion, get the
   zombies, and the missing-origin counter CLIMBS again (observed 900/min -> 5.6k/min
   after the 23:57 completion). The run takes ~45 min, so count on the
   SECOND post-churn completion (started strictly after the churn ended)
   for genuine recovery; verify the failing destinations flip from
   flash-client zombies to live providers. Follow-up idea: the scorer
   should exclude candidates whose current connection is younger than a
   floor or already gone at write time.

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
| tls-key-mitm | logs | 15.2 identity cross-check mismatch class | any |
| e2e-key-coverage | pg | 15.1 coverage vs trailing 24h median (armed ≥ 5%) | < 50% of median, 3 probes |
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
| contract-balance-failure-rate | Mimir/Grafana | `urnetwork_connect_contract_failures_total{cause="insufficient_balance"}` 5-minute rate | > 4,000/min for 5 min |
| missing-origin-rate | Mimir/Grafana | `urnetwork_connect_contract_failures_total{cause="missing_companion_origin"}` 5-minute rate vs its ~90/min background | > 500/min for 5 min |
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

### 8.5 Ansible provisioning restarts every warp unit SIMULTANEOUSLY

A `warpctl deploy` is rolling and graceful (8.2). An ansible provisioning run
is neither: it rewrites the systemd unit files, systemd does `Reloading.`, and
every `warp-main-*` unit on the host restarts — all blocks, all services, and
(since the playbook runs hosts in parallel) the same minute FLEET-WIDE.
Observed 2026-07-19 22:53–22:55: edges 0/1/4 all logged
`Stopping Warpctl main connect g3/g4` within a 40-second window.

- Each unit restart stops the running container (which then drains up to its
  stop timeout) and starts a fresh container of the SAME version — so
  `docker ps` shows same-tag containers with reset `Up` times, NOT a new
  build. Distinguish from a crash loop (5.10): statuses stay `Up`, no
  `Exited`/`Restarting` churn, and journalctl shows systemd
  `Stopping`/`Started` pairs, not container deaths.
- Client effect: every client of every block evicted at once → reconnect
  storm (2.7 high side) with a plateau (drain walkers evicting), a final
  eviction burst (15k/min observed at the 40-min mark), then fast decay to
  baseline. Median connection lifetime halves during the window. Score
  effects follow CONNECTDRAIN2: reconnect + provide-change invalidate
  reliability blocks fleet-wide.
- Diagnosis: `journalctl --since '<window>' | grep -E "systemd\[1\]: (Stopping|Started) Warp|ansible-ansible"`
  on any edge. Ansible module invocations log as `python3.10[...]:
  ansible-ansible.builtin....` — their presence at the inflection minute is
  the confirmation.
- Log access during such windows: `warpctl logs` rides loki via
  main-grafana, which may itself be down/redeploying (it panicked and timed
  out throughout the 2026-07-19 incident). Container stdout is NOT in
  journald (`--log-driver=local`; journalctl -u warp-* has only the warpctl
  supervisor lines) — the fallback is `sudo docker logs --since <t>
  <container>` over ssh.
- Expectation to verify recovery: connect rate back within ~±20% of the
  pre-incident baseline within ~10 min of the final burst, old same-tag
  containers gone by their stop timeout, no residual page-tier tickets
  except known standing ones.

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
- `urnetwork_redis_key_event_merge_drops_total` (`server/redis.go`) — keyspace
  notifications dropped at the per-node/per-process merge (each master's
  PubSub drain goroutine feeds a shared 1024-slot merge channel; a full merge
  drops the message rather than blocking the socket). Every drop TERMINATES
  that subscription epoch → resubscribe + corrective full resync, so nothing
  is silently lost — each drop shows up as a resubscribe+resync cycle above.
  Occasional drops during a mass reconnect are self-healing; a SUSTAINED
  nonzero rate = a key-event burst storm outrunning the merge buffer, forcing
  continuous resubscribe/resync churn — find the write storm generating the
  events before tuning buffer sizes or intervals.
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

## 10. Connect drain (deploy / rebalance)

Signals learned draining `by-us-fmt-5-edge-4` live during a rolling deploy
(2026-07-18). Design + fixes: CONNECTDRAIN2.md.

### 10.1 Is a connect group actually draining?
- **The systemd unit uptime is NOT the signal.** `warp-main-connect-g<N>` is a
  warpctl ORCHESTRATOR; it stays up for weeks while the connect BUILD runs in a
  docker container underneath. On the incident host the unit showed 29h uptime
  while the container was mid-deploy — reading unit uptime as "not deployed yet"
  was wrong. Check the container, not the unit.
- **Drain-in-progress = the orchestrator has a live `docker container stop`
  child.** `pid=$(systemctl show warp-main-connect-g<N> -p MainPID --value)` then
  `ps --ppid $pid -o etime,cmd` shows e.g. `14:39  sudo docker container stop
  -t 3600 <container>`. Presence of that child = that group is draining; its
  `etime` = how long. `-t 3600` = up to a 1-HOUR SIGKILL grace, so a stuck drain
  can hang ~an hour blind.
- **The running build = the container image tag**, not the unit. `sudo docker ps
  --format '{{.Image}} {{.Status}}'` (needs root; on these hosts `by` sudo
  password is `by-pass`, feed via `sudo -S`). A drain shows the old container in
  `Removal In Progress`/`Exited` while the new image's container is `Up`.

### 10.2 Drain health signals
- **`urnetwork_connect_drain_residents_remaining`** (gauge, per service;
  CONNECTDRAIN2 §3.5) is the drain ETA WITHOUT ssh-ing to find the stop-child.
  Query it as a RANGE, not a last value: the gauge rides the stats pusher's
  {env, service, block, host} series, and the replacement container overwrites
  that same series with 0 within one ~15s push, so an instant `> 0` check
  misses the whole drain after the fact. Use
  `max_over_time(urnetwork_connect_drain_residents_remaining[15m]) > 0` to
  detect that a drain ran, and the range graph falling to 0 for live progress.
  A value that stays nonzero across consecutive pushes for minutes = a stuck
  drain (the sweep found residents it cannot evict); the hard
  `DrainAllTimeout` bounds it. Replaces reading the stop-child `etime` for
  progress (that recipe is §10.1, still valid when scraping is down).
- **`urnetwork_connect_drain_excuses_written` − `urnetwork_connect_drain_excuses_consumed`**
  (two counters): markers minted at drain/migrate vs redeemed at reconnect. A
  large sustained written-minus-consumed gap = clients that did NOT come back
  (real capacity loss), not a deploy artifact.
- **Both groups of a host draining at once** (`edge4_draining=2` = the g1 AND g4
  stop-children both live) halves the host's serving capacity during the window
  and double-bounces clients — flag it. Track B stagger (CONNECTDRAIN2 §3.4,
  shipped) serializes one group/host at a time via a host-wide flock
  (`warpctl-host-drain.lock` under WARP_HOME), so concurrent stop-children per
  host should be 1; `>1` means the stagger was bypassed (lock-acquire timeout,
  or `WARPCTL_STAGGER_HOST_DRAIN=0`). Watch: count of concurrent `container
  stop` children per host.
- **Drain wall-time** (the stop-child `etime`): the pre-CONNECTDRAIN2 fixed
  200ms/resident walk plus the blind `-t 3600` ceiling produced a 28+ MINUTE
  drain in the incident. Track A replaced the fixed walk with an adaptive pace
  over `DrainStragglerSweepTimeout` and an enforced `DrainAllTimeout`, and the
  admission gate (503 on new connects while draining) stops the refill loop, so
  the process exits promptly. A drain running longer than a few minutes with
  residents remaining is stuck — check `[c]drain in progress (at least N
  remaining)` / `[c]drain deadline with at least N remaining` in the connect
  log (`resident.go`).
- **Peer registry frozen during drain:** the affected network's
  `{np_<net>}eid` stops advancing and its `{np_<net>}connected` set empties while
  the old build drains and clients haven't re-registered on the new one — devices
  see each other as offline for the whole window. Confirm registration resumes
  post-drain: `redis-cli -c -p 6379 zrange '{np_<net>}connected' 0 -1` on the
  redis host repopulates and `eid` advances.

### 10.3 Reliability fallout of a drain (CONNECTDRAIN2 Track A shipped)
- Pre-Track-A, a drain-reconnect wrote `ConnectionNewCount`, which invalidates
  the provider's 60s reliability block (`transport_announce.go`); only ONE
  reconnect/block is forgiven (`ReliabilityAllowDisconnectCountPerBlock=1`), so a
  double-bounce (two groups draining) invalidated. The re-announce also set
  `ProvideChangedCount` (independent invalidation).
- **With Track A**, a drain-caused reconnect consumes a `drain_excuse_<clientId>`
  marker and is recorded as `connection_excused_new_count` (a non-invalidating
  counter — it never enters `client_reliability_valid`), and the mechanical
  provide re-announce is suppressed for the drain window. So a deploy shows as
  **`urnetwork_connect_connection_new{excused="true"}`** instead of a score dip.
  The split is the tell: `excused="true"` spiking on a deploy is benign;
  `excused="false"` spiking is organic churn worth investigating.
- **What to still expect:** the excusal fixes the reconnect/provide-change
  invalidation, NOT the missing-blocks gap (§2.3 of CONNECTDRAIN2) — that is
  removed by Track B make-before-break (migrate → no gap). A residual score dip
  on a deploy that runs Track A but not Track B migration (old SDKs, SIGKILL,
  clients that could not migrate) is the gap, not the reconnect penalty.

### 10.4 Access recipe (this env)
`ssh by@172.28.208.175` (edge-4). Orchestrator child: `ps --ppid <MainPID>`.
Docker (root): `echo by-pass | sudo -S docker ps`. Redis registry lives on
edge-6 `172.28.208.177`: `redis-cli -c -p 6379` (no auth). Readonly only.

## 11. Grafana / loki / mimir observability stack (the 2026-07-19 outage)

The observability PLANE itself: grafana + loki + mimir + alloy behind a Go front
(`warp/grafana/main.go`), ONE bundled service `warp-main-grafana-*` on 6-of-7
lb/host_services hosts (edge-0/1/3/4, crisp, fireside; **edge-5 offline**). The
front is PID1 and supervises the children via `warp.Child` (restart-on-exit).
Access: `by-pass <secmd-key> by` → per-host sudo pw, then
`echo "$PW" | ssh by@<ip> 'sudo -S -p "" bash -s' <<'EOS' … EOS` — the `by` user
is NOT in the docker group, so docker needs sudo. Log driver is now `local`, so
`sudo docker logs <c>` WORKS (was awslogs → cloudwatch). IPs: edge-0=.173,
edge-1=.51, edge-3=.174, edge-4=.175, edge-5=.176 (OFFLINE), crisp=.58,
fireside=.3 (all 172.28.208.x mgmt). Route-net (eno1) IPs live in
`config/main/settings.yml` routes.

### 11.1 Fleet health one-shot — the composite line
One line per host tells the whole story. Service ports are NOT the bind ports —
read the container's `WARP_PORTS` for the per-deploy internal port, then probe:
```
c=$(docker ps --filter name=grafana --filter status=running -q | head -1)
wp=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $c | grep ^WARP_PORTS= | cut -d= -f2)
getp(){ echo "$wp" | tr , '\n' | awk -F: -v s=$1 '$1==s{print $2}'; }   # service port -> internal
gw=$(ip -4 addr show warpservices | grep -oE '172\.[0-9]+\.0\.1' | head -1)
curl -so/dev/null -w '%{http_code}' http://$gw:$(getp 80)/status              # front (warpctl's OWN poll target)
curl ... 127.0.0.1:$(getp 3101)/ready ; :$(getp 3201)/ready ; :$(getp 3000)/api/health  # loki/mimir/grafana
curl -sG 127.0.0.1:3100/loki/api/v1/label/service/values | grep -oE '"[^"]+"' | wc -l  # svc
```
HEALTHY BAND: `up=1 front=200 loki=200 mimir=200 graf=200 svc>0 restarts=0/0/0`.
`svc` = distinct `service` labels loki knows = proof of BOTH log ingest and
cross-host read (label fixed 2026-07-19: it is `service`, not `warp_service`;
healthy main knows 9 — api app connect grafana lb mcp proxy taskworker web.
The labels api defaults to a recent window: pass explicit start/end before
reading an empty result as "nothing was ever ingested"). Any field off names
a class below.

### 11.2 up-count & child restarts — overlap vs crash-loop
- `up` = running grafana containers. **`up>1` = redeploy overlap** (old container
  draining) — normal for a few minutes, STUCK for hours = the poll deadlock (11.7).
- Child restarts from the front's supervisor:
  `docker logs --tail 200 $c | grep -c '\[loki\]exited'` (also `[mimir]`, `[alloy]`).
  Stable = **0**; nonzero-and-climbing = crash-loop. **CRITICAL: front + grafana
  read 200 while loki/mimir crash-loop** — the front supervises the children, so
  the container still passes its readiness poll and a broken build still
  "deploys". Always check restarts, never trust "container Up" alone.

### 11.3 The bind paradox — `ss` shows LISTEN but connect is REFUSED
The single most misleading signal here. `ss -tlnp` shows a service `LISTEN` on
`127.0.0.1:<port>` (or the docker-gw ip), yet `curl`/`/dev/tcp` to it is
**refused** — even from inside the same netns. Mechanism: a listener bound to a
SPECIFIC ip (loopback or the gateway) is refused during a container overlap,
while `0.0.0.0`-bound sockets (front `:3100`, the ring grpc, and post-fix
loki/mimir/grafana http) accept fine. Discriminators:
```
timeout 2 bash -c 'exec 3<>/dev/tcp/127.0.0.1/'$(getp 3101)   # raw connect, bypasses curl
# CONTROL: a throwaway 127.0.0.1 listener DOES accept -> host loopback is fine; it's the service bind
```
So **do NOT trust `ss` "LISTEN" as healthy**, and **probe loki through the front
proxy** (`127.0.0.1:3100/loki/...` → 200 vs 502), not the backend port. FIX for
every component: bind `0.0.0.0` (loki/mimir `server.http_listen_address`, grafana
`http_addr`, the front's main `server` in `serve()`). The exact kernel reason for
the specific-ip refuse during overlap is unexplained; `0.0.0.0` is the empirical
cure (bridges are DOWN, gw ips route via lo — suspected but unproven).

### 11.4 Config parse failure — flag names ≠ yaml keys
loki/mimir crash-loop (11.2 restarts climbing) with, in the logs:
```
docker logs --tail 300 $c | grep -iE 'not found in type|error parsing|unmarshal'
#   field instance_addr not found in type frontend.CombinedFrontendConfig
#   field ring not found in type scheduler.Config
```
The flag name is NOT the yaml key: `-frontend.instance-addr` → yaml
`frontend.address`; loki's scheduler block is `scheduler_ring`, mimir's is `ring`.
**Verify keys against the binary BEFORE building:**
```
docker exec $c sh -c 'printf "target: all\nfrontend:\n  address: 1.2.3.4\n  port: 6490\n" >/tmp/t.yml; /usr/local/sbin/loki -config.file=/tmp/t.yml 2>&1 | grep "not found"'
```
Empty = keys OK. This break passes `front=200/graf=200` (11.2), so it looks "deployed".

### 11.5 Ring formation & cross-host reads (svc=0, frontend healthcheck)
`svc=0` on a host that is otherwise 200 = its querier can't read the cluster. Tells:
```
docker logs --tail 500 $c | grep -iE 'removing frontend failing healthcheck|unexpected status received for init|reached_nodes'
#   removing frontend failing healthcheck addr=192.168.51.196:14609 ... DeadlineExceeded
#   re-joined memberlist cluster reached_nodes=6   <- gossip is fine; it's the query grpc
```
`14609` is the per-deploy INTERNAL grpc port. Internal ports are LOCAL-ONLY
(reachable via loopback/own-lan/interface-gw, but firewalled/timeout cross-host —
the `dport 14609→gw:14609` DNAT is OUTPUT-only, ingress pkts=0). The
ingester/distributor rings advertise the EXTERNAL front-proxied port (loki 6490 /
mimir 6491) and work; the query-frontend + query-scheduler DEFAULTED to the
internal port and broke cross-host reads on the no-LB hosts (crisp/fireside, which
have no local logs). Discriminator: `conn <peer> 6490` OK vs `conn <peer> 14609`
timeout. FIX: pin frontend+scheduler to the external port (loki `frontend.port` +
`query_scheduler.scheduler_ring.instance_port`=6490; mimir `.../ring.instance_port`=6491).

### 11.6 minio object-store persistence (edge-6)
loki chunks + mimir blocks land in minio (`192.168.51.193:23900`, data
`/data/minio`, on edge-6 172.28.208.177). Signals:
```
du -sh /data/minio/loki /data/minio/mimir                              # growth = writing
find /data/minio/loki -name xl.meta -printf '%T+\n' | sort | tail -1   # fresh mtime = live
ls /data/minio/loki/fake | wc -l                                       # loki CHUNK dirs (fake=anon tenant)
find /data/minio/mimir -name meta.json | wc -l                         # mimir FINALIZED blocks
```
loki flushes on `chunk_idle_period`/shutdown; **mimir uploads only after a ~2h
TSDB block boundary** — an empty mimir bucket right after a healthy start is
EXPECTED, not a fault (populates on the next boundary). An empty/stale loki bucket
while loki is crash-looping = writes stopped (11.2/11.4), not a storage bug.

### 11.7 Redeploy poll DEADLOCK (structural, warpctl)
On a host with a lingering old container, the new one never converges: the journal
loops the poll, the old container is Up for HOURS, new containers `Exited(0)` churn:
```
journalctl -u warp-main-grafana-g1 -n 40 | grep -oE 'Poll http.*|Found overlapping.*'
#   Poll http://172.18.0.1:14488/status   (repeats forever)
docker ps -a --filter name=grafana --format '{{.Image}} {{.Status}}'
#   ...996260400  Up 3 hours          <- old, never drained
#   ...996359920  Exited (0) 1m ago   <- new, killed by poll timeout
```
Root (`warpctl/run.go` `deploy()`): the DNAT `redirect`, `cleanupStaleConntrack`,
and draining the OLD container ALL run only AFTER a passing poll; on poll-fail a
`defer` KILLS THE NEW container and leaves the old. So a poll that can't pass while
an old container lingers is self-perpetuating. `cleanupStaleConntrack` can't break
it — UDP-ONLY (`run.go:~925`, built for wireguard keepalives) AND post-poll. The
grafana trigger was 11.3 (front main server bound the specific gw ip; fixed by
`0.0.0.0`). BREAK IT: reboot the host (clears containers + conntrack), or
`docker stop` the old container + `conntrack -D -d <gw>`. LATENT for any service
until the deploy loop drains-old on prolonged poll-fail instead of only after a pass.

### 11.7b Memberlist island (join_members rendered empty) — the 2026-07-19 night tail
After breaking the 11.7 deadlock on edge-0 (stale containers stopped, unit
relaunched solo), its loki/mimir came up as a ONE-NODE gossip cluster: lb
queries flipped from hang to fast `500: too many unhealthy instances in the
ring`, then to answering with edge-0's data invisible; edge-0's own `/ring`
showed a single ACTIVE member (itself) while the other 5 hosts' shared ring
was healthy without it.
- ROOT: `ringJoinMembers` (warp/grafana/main.go) looked up services.yml ring
  hosts (FQDN keys, `by-us-fmt-5-edge-0.bringyour.com`) in settings.yml routes
  (SHORT keys, `by-us-fmt-5-edge-0`) — every lookup missed and EVERY host
  rendered `join_members: []`. The fleet mesh only ever formed because a
  rolling deploy's OLD containers (already meshed) gossip-dial the new
  instance's advertised port and bridge it in — a host restarted ALONE has no
  inbound dial and stays an island forever. Fixed 2026-07-19 (build 997082420):
  fqdn→short-name resolution + fall back to ALL routed hosts when the seed
  list resolves empty (dead seeds are tolerated; an empty list is the only
  fatal render).
- TELLS: rendered `join_members: []` in `/run/warp-grafana/loki.yml` (docker
  exec + sed the memberlist block — THE decisive read); single-member `/ring`
  on the island vs n−1 members elsewhere; lb queries 500 "too many unhealthy
  instances" only when landing on the island. RED HERRING: grafana-server's
  `msg="no peer discovery configured" service=cluster` line is its alerting-HA
  cluster, not loki/mimir memberlist.
- RING HYGIENE: instance id = short hostname, so old+new containers of one
  host SHARE a ring entry (an overlap never dirties the ring), but SIGKILLing
  loki (docker stop -t shorter than its shutdown flush) skips unregister —
  use a generous stop timeout. Leftover UNHEALTHY entries: forget via the loki
  http `/ring` page (internal port from WARP_PORTS, host-loopback, no root).

### 11.8 systemd unit port baking (WARP_PORTS staleness)
`warpctl service run` reads ports from `--portblocks` BAKED into the unit at
`create-units` time, NOT live services.yml. Symptom: front panics
`ring port 6490 must be declared ... Missing host port for 6490` because WARP_PORTS
still has the old ports.
```
grep -o 'portblocks=[^ ]*' /etc/systemd/system/warp-main-grafana-g1.service  # baked ports
docker inspect $c --format '{{range .Config.Env}}{{println .}}{{end}}' | grep WARP_PORTS  # what the container got
```
FIX: `warpctl service create-units main` → commit the regenerated `xops` units →
redeploy. Editing services.yml alone does nothing until the units are regenerated.

### 11.9 Playbook: grafana/loki/mimir "no data / 502 / stuck deploy"
1. `up>1` for hours or `Poll …` looping in the journal → deadlock (11.7): clear stale container + reboot.
2. loki/mimir restarts climbing → config parse (11.4): read `not found in type`, fix the yaml key, re-verify on the binary.
3. front=200 but backend connect refused while `ss` shows LISTEN → bind paradox (11.3): probe via the front proxy; real fix is `0.0.0.0`.
4. `svc=0` on some hosts + `removing frontend failing healthcheck addr=:14609` → cross-host frontend/scheduler on the internal port (11.5): advertise the external ring port.
5. minio empty → mimir-pre-2h-boundary (expected) vs loki-crash-looping (11.2)?
6. front panics `Missing host port` → stale baked units (11.8): regenerate.
7. lb queries hang or 500 `too many unhealthy instances in the ring`, or one
   host's data missing → memberlist island (11.7b): check `/ring` member count
   per host + rendered `join_members`; a redeploy re-bridges, the code fix
   (build ≥997082420) prevents it.

---

## 12. Taskworker drain (deploy) — TASKDRAIN1

The taskworker plane has no client connections; its "clients" are the chain
cadences (contract close ×8, handler reap 60s, reliability rollup 1min,
client scores 30s). Deploys are make-before-break over the shared pg queue,
so a HEALTHY deploy pauses nothing. These signals catch the unhealthy paths.

### 12.1 Drain outcome (log classes, service=taskworker)
PROBE: `logs/taskworker-drain-gave-up` (tailer class; only the gave-up line is
a finding — the other outcomes are healthy-by-design).
The drain logs one start line and exactly one outcome line per SIGTERM:
```
[taskworker]drain start with N in flight
[taskworker]drain finished cleanly in Xs                      # phase 1 (common)
[taskworker]drain canceling N in-flight tasks after Xs         # phase 2 entered
[taskworker]drain finished after cancel in Xs (N canceled and rescheduled)
[taskworker]drain gave up after Xs with N tasks still running  # phase 3 (bad)
```
- HEALTHY: "finished cleanly"; "finished after cancel" with small N is fine
  (the canceled tasks were rescheduled with claims released — re-run within
  seconds elsewhere; their reschedule_error starts with `Drained:` and does
  NOT advance reschedule_error_count).
- BROKEN: "drain gave up" = a ctx-ignoring task rode to SIGKILL. Its claim
  (and EVERY claim of that container) is now leased until claim +
  max(30s, run_max_time_seconds) — find them with 12.3 and decide whether to
  `bringyourctl task release`. Also broken: no outcome line within
  DrainFinishTimeout+DrainCancelTimeout+30s of the start line (process hung
  outside task work).
- Metrics mirror: `urnetwork_taskworker_drain_inflight`, `_drain_seconds`,
  `_drain_canceled` (push-based; the series goes stale when the process
  exits, so the log lines are the durable record).

### 12.2 Readiness gate (deploy-time)
`/status` latches at startup: one-shot pg SELECT 1 + redis PING before any
task is claimed. Status `error not ready: ...` → the warpctl poll fails
(`^(?i)error(\s|:)`) for the full 120s → deploy reverts, old containers keep
the plane running. Status `draining` after SIGTERM is informational (NOT an
error — deliberate, so fleet status sampling doesn't count drains).
- Signal: a deploy that reverts with `error not ready: redis ...` = the new
  build cannot reach a dependency — fix the build/config, do NOT force.
- GOTCHA: readiness is start-time-latched by design. A runtime redis outage
  does not flip /status; that is 1.2's job (task canaries).

### 12.3 Stuck leases (post-SIGKILL / crash)
PROBE: `pg/task-lease-stranded` (probe_taskworker_drain.go, 60s cadence):
claim with a future release_time whose keepalive (claim_time refresh every
~10s while running) has been silent > 2 minutes = claiming worker gone.
```sql
-- claims held with a future release: normal while a task RUNS; suspect when
-- the claiming container is gone (correlate with deploys/restarts)
SELECT split_part(function_name,'.',3) AS task, task_id,
       claim_time, release_time,
       round(extract(epoch from (release_time - now()))) AS lease_remaining_s,
       run_max_time_seconds
FROM pending_task
WHERE now() < release_time
ORDER BY release_time DESC;
```
- HEALTHY: rows whose task genuinely runs long (compare finished_task
  duration history, 2.5) and whose worker is alive.
- BROKEN: lease_remaining_s ≈ run_max_time_seconds shortly AFTER a
  taskworker kill/crash = stranded claim; the chain is paused until release.
  DbMaintenance strands for up to 24h (skips a nightly window),
  UpdateClientScores/UpdateReliabilities up to 2h (selection freshness, 2.8),
  a CloseExpiredContracts slice 30min (close backlog, 2.6).
- ACTION: verify the claiming worker is dead (deploy log / container list),
  then `bringyourctl task release <task_id>` (immediate re-claim per run_at)
  and/or `bringyourctl task kick <run_once_key>` (pull run_at to now).
  Releasing a RUNNING task re-opens the duplicate-execution window — verify
  first.

### 12.4 Post-deploy convergence
PROBES: `pg/task-due-lag` (oldest due-and-unclaimed > 180s sustained = the
plane stopped claiming) and `pg/task-target-missing` (`Target not found`
past 100 retries = beyond any overlap, a missing registration) — both in
probe_taskworker_drain.go, 60s cadence.
Within ~1min of a taskworker deploy completing:
- oldest-due lag returns to ~0:
```sql
SELECT round(extract(epoch from (now() - min(run_at)))) AS oldest_due_s
FROM pending_task
WHERE available_block <= extract(epoch from now()) AND run_at <= now();
```
  (transient spikes while both build generations overlap are normal; a lag
  that GROWS after the old containers exited = workers not claiming — check
  12.2 and 1.2.)
- `Drained:` reschedules from the drain complete their re-runs (the rows
  disappear or complete; reschedule_error_count stayed 0).
- `Target not found` reschedule errors are overlap noise and retry on a flat
  ~16s cadence; they must clear once the fleet is on one build generation.
  PERSISTING target-not-found on one build = a task type shipped without its
  target registration — a code bug, page it (it no longer hides behind the
  1h backoff).

## 13. Api drain (deploy) — APIDRAIN1

The api drains via the shared http drain sequence (`server/http_drain.go`):
SIGTERM → /status latches "draining" → 10s keepalive retire grace (every
http/1 response stamped `Connection: close`, so nginx retires its pooled
conns cleanly) → `Shutdown` with a 60s ceiling → exit. Metrics are
service-neutral `urnetwork_http_server_*` gauges keyed by the stats pusher's
{env, service, block, host} grouping; any service adopting
`HttpServerOptions.KeepaliveDrainTimeout` emits the same series.

### 13.1 The one page-worthy signal
- `max_over_time(urnetwork_http_server_drain_cut_connections[15m]) > 0`
  (service="api"): a drain hit the 60s ceiling with connections still open —
  those were HARD CUT at exit (client-visible truncation; a cut
  sent-but-unanswered POST may be replayed by nginx's non_idempotent retry =
  possible double execution). The RANGE query is required: the gauge reaches
  the series in the dying container's exit flush, and the replacement
  container overwrites the same {env, service, block, host} series with 0
  within one ~15s push — an instant `> 0` check misses the event.
  Must be 0 forever: the ceiling (60s) exceeds the max request lifetime
  (ReadTimeout 15s + WriteTimeout 30s), so a nonzero means a handler is
  wedged past its write deadline or the timeouts were misconfigured.
  Log line: `[http]drain deadline after <dur>: N connection(s) cut`.

### 13.2 Drain-window observability
- `urnetwork_http_server_draining` 1 during the drain sequence;
  `drain_seconds` = last drain duration (expect ~10s grace + seconds);
  `drain_inflight` = requests mid-handler at SIGTERM (context for cut>0).
- The stats pusher and the router stats reporter run through the drain (the
  process ctx outlives the serve ctx) and both FLUSH at exit — the drain
  window's requests appear in the final `[host][api][block]` route lines
  instead of vanishing with the process.
- /status returns the latched json status "draining" (deliberately NOT an
  error: the deploy poll never targets the draining container, and fleet
  status sampling must not count an operator drain as a service error).

### 13.3 Deploy-window client impact (expected: none)
- nginx retry classes are narrowed to `error timeout http_502 http_503
  non_idempotent` (warp config.go): a draining/flipping upstream is ridden
  over; http_500/http_504 no longer re-execute POSTs on a sibling.
- Go clients additionally retry GETs once (jittered) on a surfaced 502/503
  (`connect` ClientStrategy `GetRetry*` settings); the JS SDK retries its
  GETs likewise. POSTs are never replayed by clients.
- BROKEN: deploy-window 5xx spikes at the lb for service=api, or client
  reports of failed POSTs during deploys → check 13.1 first, then whether
  both retry tries landed on draining blocks (host drain flock should make
  that impossible — one block per host drains at a time).

### 13.4 Readiness latch (P0)
- `urnetwork_api_ready` 1 after the startup latch passed (one-shot pg
  `SELECT 1` + redis `PING`, `api/readiness.go`), 0 on a failed check and
  from drain start. A failed check latches /status to
  `error not ready: <check>: ...` — the deploy poll reads it, times out,
  and reverts to the old container. The not-ready container does NOT exit
  (no restart flap; a restart-in-place, where the DNAT already targets it,
  is served best-effort with warmup skipped).
- BROKEN: a deploy that keeps reverting with `error not ready: redis ...`
  = the new build/config cannot reach a dependency the old build can —
  diagnose the dependency (vault drift, network, auth), not the poll.

## 14. Proxy drain (deploy) — PROXYDRAIN1

The proxy hosts (fireside/crisp, 10 blocks each) are transparent-lb: direct
DNAT, no nginx, and `warpctl deploy main proxy` is fire-and-forget at the
CLI (no lb status polling) — the HOST-side run worker's 120s /status poll is
the only gate before the DNAT flip. Clients are pinned to their (host,
block) forever, so there is no sibling absorption: the replacement container
of the SAME block is the only thing that can serve them. All metrics below
are pushed by the standard stats pusher {env, service=proxy, block, host}.

### 14.1 Readiness gate (deploy-time)
- `urnetwork_proxy_ready` 1 once the initial proxy-client sync has been
  APPLIED (every watched host/block stream completed its first successful
  read AND delivery — the wg peer table is restored), 0 before and again
  from drain start. Unlike api/taskworker (a pg/redis latch), proxy
  readiness is the peer restore itself: /status 503
  `not ready: initial proxy client sync in progress` until then, so the
  DNAT flip can no longer beat the peer install (the pre-PROXYDRAIN1 race:
  a wg handshake arriving before its peer was silently dropped).
- Log tell: `[proxy]initial proxy client sync applied; ready`. A deploy
  that reverts without that line = the sync cannot complete (pg/redis
  unreachable from the new build, or the delivery keeps failing —
  `[proxy]proxy clients callback err=... (will retry)`).

### 14.2 Drain window (the old container, SIGTERM → exit)
- `urnetwork_proxy_drain_active_remaining` (1s cadence): in-flight
  socks/http connections still relaying. Falling to 0 → the process exits
  IMMEDIATELY (exit 0), so `docker stop` returns right away. A plateau
  until the 2min `DrainGraceTimeout` = long-lived tunnels riding the grace
  (expected; cut at the deadline). Logs: `[proxy]drain start (N active,
  grace 2m0s)` → `[proxy]drain in progress (N active)` every 10s →
  `[proxy]drain complete in Xs` | `[proxy]drain deadline with N active`.
- The wg ingress serves through the WHOLE drain on purpose (not a drain
  target): its conntrack-pinned clients cannot migrate until this process
  exits and warpctl flushes their entries (run.go cleanupStaleConntrack,
  §10.1 stop-child recipe applies here too). New tcp conns already go to
  the replacement (the flip preceded SIGTERM); only established flows are
  in play.
- `[wg]handoff export: N peers` right before exit = the endpoint handoff
  was written (peers with a handshake in the last 10min). `no recently
  active peers` on a busy block = wrong: check PeerStatuses/last-handshake
  plumbing.

### 14.3 Post-flip convergence (the replacement container)
- wg re-establishment is SERVER-initiated: `[wg]handoff apply: N/M
  endpoints seeded` then per-peer `[wg]handoff re-established <ip> in Xms`
  and `all peers re-established in Xs`. Expect sub-second-to-seconds after
  the old container exits — initiations sent before the conntrack flush
  blackhole harmlessly and retry (5s pace, 5min budget). `initiate budget
  ended with N peers pending` = clients genuinely gone OR the flush never
  ran (verify the drained container actually exited and warpctl's
  post-drain cleanupStaleConntrack fired).
- `urnetwork_proxy_prewarmed_devices` + `[proxy]prewarm: N/M devices
  ready`: devices for clients active in the last 10min are warmed before
  their first packet arrives. N far below M = providers unreachable from
  the new build (egress window never satisfied) — the lazy open path still
  covers misses, at cold-start cost.
- `urnetwork_proxy_devices_live` (1min cadence) should recover toward the
  pre-deploy level as actives return; `urnetwork_proxy_wg_peers` should
  match the block's client set right after the initial sync. A wg_peers
  collapse post-deploy = restore problem — read the `[wg]sync clients:
  applied/removed (...)` drop-reason counts (key mismatch / bad auth /
  not entitled).

### 14.4 Inner-flow continuity (window identity reuse)
- `[pd][<proxyId>]window identity restore: N identities` on the
  replacement = the recreated device reuses its window client ids against
  the same providers, so provider-side NAT flows (udp 60s idle, tcp 300s)
  resume. Absence for a recently-active device = the 10min store ttl
  lapsed (restart gap too long) or persistence is disabled
  (`DisableWindowIdentityPersistence`).
- Flows that can NOT resume (identity not restored, or the flow landed on
  a different window entry) now fail FAST: the provider answers orphaned
  mid-stream packets with a RST (`TcpBufferSettings.EnableOrphanRst`,
  256/s valve) instead of letting the app hang to its own timeout. A
  deploy-window burst of client-side connection resets that immediately
  reconnect is this mechanism working; sustained resets outside deploys
  are worth investigating (source flow state being lost somewhere).

---

## 15. E2E encryption (post-quantum) signals — E2EPQ1

Context: clients can enable per-peer post-quantum e2e sessions (the "Post
Quantum Encryption" toggle; opportunistic — a peer without support falls back
to plaintext at that layer), and providers always enable the responder side.
A provider running the e2e-enabled build publishes its TLS cert commitment on
connect (oob `EncryptedKey` → `client_tls_certificate`, one row per client_id,
`set_time` refreshed on publication; validated in
`controller.SetEncryptedKey`). The platform cannot see inside sessions (by
design). What it CAN see: key publications (pg), the unauthenticated
`/key/<client_id>` cross-check api, and the client-side `[tls]`/`[key]` log
lines of the connect stacks the server itself hosts — the proxy service's
devices are the tailer's vantage point for §15.2/15.3.

### 15.1 Key-publication coverage — the provider e2e rollout/health proxy
```sql
-- coverage among recently-connected clients (probe pg/e2e-key-publication)
SELECT count(DISTINCT ncc.client_id) AS active,
       count(DISTINCT ctc.client_id) AS covered
FROM network_client_connection ncc
LEFT JOIN client_tls_certificate ctc ON ctc.client_id = ncc.client_id
WHERE ncc.connect_time >= now() - interval '1 hour';

-- publication freshness (upserts in the last hour)
SELECT count(*) FROM client_tls_certificate
WHERE set_time >= now() - interval '1 hour';
```
- HEALTHY: coverage ratchets up with the fleet rollout, then holds (diurnal
  wobble fine). Publications track the connect rate of updated providers.
- BROKEN: coverage < 50% of its own trailing 24h median, sustained 3 probes
  (the probe arms only once the median reaches 5%, so pre-rollout zeros are
  quiet): providers stopped publishing — EncryptedKey oob regression, a
  `tls-cert-publish-invalid` spike (bad client build), or a fleet rollback.
- Action: correlate with deploys (§8) and `tls-cert-publish-invalid`; run
  the freshness query; if fresh publications are healthy but coverage fell,
  the active-client mix changed (old builds reconnecting) rather than the
  publish path breaking.

### 15.2 Identity-key cross-check mismatch — the MITM early-warning (page)
Log class `tls-key-mitm`: `CONTRACT vs FETCHED peer client public key
MISMATCH ...`. A session's contract-delivered peer key disagreed with the
`/key/<client_id>` api (the out-of-band cross-check in the connect stack;
today log-only — the contract key is still trusted).
- HEALTHY: 0, always.
- BROKEN: any line. Either the platform serves inconsistent keys between the
  contract path and the key row (data bug) or something is substituting keys
  (MITM attempt). Both demand immediate investigation.
- Action: for the named peer id, compare the contract-attached key against
  the stored client key / `client_tls_certificate` rows and recent
  `set_time` churn; check §8 for a deploy that could have split the two
  paths; treat as security-relevant until proven a data race.

### 15.3 Session anomaly classes (warn)
- `tls-key-rotate-refused` — `peer client public key mismatch with prior
  commitment`: a peer presented a different identity key mid-session;
  refused by design. Occasional lines = reinstalls racing old sessions;
  a sustained rate = client identity bug or key churn upstream.
- `tls-cert-publish-invalid` — `Invalid PEM in certificate chain` /
  `Invalid X.509 certificate in chain` from `SetEncryptedKey`: publications
  failing validation. A rate = a client build shipping malformed chains (or
  probing of the oob path); the error text carries the chain index.

Rollout note: hosted proxy providers begin publishing (and answering
handshakes) once the server vendors the provider-side-enabled sdk; app
providers as their updates land. Until then 15.1 sits at ~0 (probe unarmed)
and 15.2/15.3 are silent.
