# MONITOR.md — main stabilization learnings → monitoring service spec

Distilled from the 2026-07-15 incident day (redis cluster instability + pg
coupling + the network-peers pubsub outage) and the preceding two weeks of
database performance work. Every signal here was actually used to diagnose or
verify; every threshold comes from observed healthy/broken values on main.

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
- KEY INSIGHT: pgadmin-style "connection utilization" is NOT query load.
  Real active backends were ~6 even during the worst incidents. Always split
  by state before concluding anything about load.

### 1.4 redis cluster state + per-node liveness
```bash
redis-cli -p 6379 CLUSTER INFO | grep -E 'cluster_state|slots_fail|known_nodes'
# per-node PING with a hard timeout — a wedged node hangs, it does not error
for p in $(seq 6380 6411); do timeout 2 redis-cli -p $p PING >/dev/null || echo "$p DEAD/WEDGED"; done
```
- HEALTHY: cluster_state:ok, slots_fail 0, known_nodes == expected (33
  incl. entry; phantoms from restarts inflate this — see 3.6), every PING < 100ms.
- BROKEN: any PING timeout = event-loop wedge (the #1 recurring failure —
  process alive, kernel accepting into backlog, event loop starved);
  cluster_state:fail = at least one slot uncovered.
- With cluster-require-full-coverage now `no`, a single dead shard degrades
  1/32 of keys WITHOUT flipping cluster_state on other nodes — the monitor
  MUST check per-node liveness, not just cluster_state.

### 1.5 log error-class rates (per service, per minute)
Count lines per minute matching each class in section 4. Healthy ≈ 0 for all
classes. Alert on rate, include the class name and one sample line. Error
VOLUME is retry amplification, not incident size — 100k identical lines can
be one sick node × fleet retry loops. The monitor should report class + rate
+ distinct target (ip:port) set, never raw volume as severity.

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
INFO memory → used_memory_dataset, used_memory_clients
```
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
| `CLUSTERDOWN` | Slot coverage lost (node marked fail + no failover, or majority loss). | CLUSTER INFO/NODES; restart dead nodes; transient ≤ node-timeout during elections is expected and retried in-client. |
| `OOM command not allowed when used memory > 'maxmemory'` | Node at maxmemory and volatile-ttl has nothing evictable (no-TTL keys dominate). Writes fail, reads work. | Identify node (3.1); drain no-TTL piles (cleanup script) or raise ceiling temporarily; NEVER a client-side problem. |
| `pubsub ... channel is full for 1m0s (message is dropped)` | IN-PROCESS consumer stall: the app isn't draining go-redis's channel (usually because its goroutine is blocked on another redis call). While blocked, the socket goes unread → server buffers grow (3.2). | Check what the consumers block on; server-side buffer alert 3.2 is the paired signal. |
| `EOF` / `connection reset by peer` | Server closed the conn (COBL kill, maxmemory-clients eviction, restart). | Correlate with server-side events; retried in-client. |
| `LOADING` / `READONLY` | Node restarting (rdb load) / replica mid-failover. Transient; retried in-client. | Only alert if sustained > 2 min. |
| Panic stack traces (`trace.go` "Unexpected error") | The STACK identifies the load-bearing call path (e.g. AddNetworkPeer → NominateLocalResident = connection-killing). | Rate per unique innermost app frame; a new frame appearing at rate = new incident. |

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

---

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

Tier-1 (warn):
| id | source | check | threshold |
|---|---|---|---|
| task-parked | pg | error_count>0 ∧ run_at>now()+5min ∧ lease expired | any |
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

Every alert carries: identity (class+target+frame), rate, one sample, the
matching playbook section (5.x), the ACTION line, and last control-plane
event age. Alerts auto-resolve when the signal returns to its healthy band
for 5 minutes, and emit the resolution (recovery confirmation is part of the
loop, per 6.8).
