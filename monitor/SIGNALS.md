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
learned from 61 zombie log-class tickets. Updated 2026-08-11 after the grafana
config-panic outage (a `{{ env: }}` value in grafana.yml naming a settings.yml
env_var that never reaches the container → every new container Exited(2)
fleet-wide, and the hosts that had already dropped their old container served
nothing; meanwhile the public dashboard read empty because the shared
datasource row was pinned to another host's rotating mimir port): new 11.10
config-env startup panic, 11.11 shared-datasource port drift, playbook rows
8-9. The same day's cleanup added 11.7c drain starvation — old containers pile
up one per deploy while the deploy itself reports success, because the
host-wide drain lock queued every service behind one holder; that is the
failure 11.7 gets mistaken for, and it is why `warpctl logs` returned empty.
Updated 2026-08-18 after a §11 audit of the live fleet found two failures the
healthy band did not cover, both because the band only watches what the deploy
poll watches: 11.13 (one host's loki query modules stuck in Starting for 16h
while `up=1 front=200 graf=200 restarts=0` — the whole fleet's frontends
dialing its ACTIVE-but-dead scheduler became ~half of every grafana container's
log volume) and 11.14 (the log/metric shipper is a HOST unit, not a container
child, and a failed one is permanent — edge-6 shipped nothing for 2.5 days and
took every `redis_*` series with it). Both now have code fixes; the same pass
corrected the stale minio data path in 11.6, the dead `[alloy]` restart counter
in 11.1/11.2, the service-label count, and the vacuous `query=up` mimir check
in 11.11.
Updated 2026-08-19 after the public connect H3 rollout: the connect build was
ready for QUIC, but the serving LB still emitted legacy PROXY protocol and the
replacement LB initially crash-looped because its newly generated UDP/8053
listener was paired with stale systemd units that did not allocate 8053 or
carry the UDP/53 forward alias. Section 16 records the PPv2, actual-generation,
QUIC-accept, conntrack, and public return-tuple signals that separated those
two failures from the historical UDP reply-SNAT failure. A forced-mode audit
the same night added the DNS/WhoDis split: both DNS envelope codecs worked on
forwarded interfaces, while the matrix exposed interfaces whose upstream
firewall did not deliver UDP/53 and two blocks whose exhausted LB port pools
prevented the new generation and its port-53 alias from becoming live. The
`.82`-through-`.85` upstream UDP/53 delivery fault was fixed on 2026-08-20;
§16.6 retains it as a resolved historical discriminator, not current state.
A follow-up direct-443 audit found a different failure: malformed PROXY
traffic can terminate one connect block's QUIC listener without terminating
the healthy container, leaving nginx to send most new sessions to closed UDP
ports. §16.5 records the socket, `UdpNoPorts`, LB `ECONNREFUSED`, conntrack,
raw-client, and qlog signals that localize that apparent packet loss.
Updated 2026-08-20 with the durable transport remediation: PP datagram
rejections are non-fatal and counted, QUIC listeners are supervised with
dynamic readiness, client PTO/no-response handshakes are counted separately,
and an LB must prove the exact UDP listener set on every Connect block before
activation. The WhoDis private service port moves from 8053 to collision-free
4053 while 8053 remains a rolling compatibility listener. §16.7 records the
second root cause found during that move: Warp retained withdrawn Grafana DNAT
rules at 7178–7182 even though Grafana's live allocation had moved, so the old
rules could steal Connect traffic indefinitely.
Updated later on 2026-08-20 after the rollout audit exposed three lifecycle
blind spots: §11.6 now distinguishes MinIO's healthy LAN path from an omitted
overlay bind; §16.7 defines bounded socket/conntrack cleanup summaries instead
of per-port journal floods; and §17 separates snow's green Compose oneshot from
a stale zero-peer Subtensor node and nginx's unrecovered OpenVPN-address bind
race.
The final Subtensor deployment pass added a third bootstrap discriminator:
historical runtimes can return a JSON-RPC error for `eth_chainId` before the
Frontier runtime API exists, even while the chain identity, peers, and head
progress are healthy. Section 17 now keeps that response visible without
mistaking it for a deployment failure, while still requiring the exact EVM
identity before chain readiness.
A post-deploy P2P audit then found snow listening on 30333 but accepting zero
inbound sessions because the site's upstream NAT did not expose the port.
Section 17 distinguishes that public-path failure from host firewall,
conntrack, DNS, and bootnode failures.
Updated 2026-08-27 after a live api/connect/proxy audit caught four more split
signals that green process checks concealed: a config-generation restart wave
caused a 6x reconnect storm while every public front door still answered; a
lazy `verify.yml` lookup made only `/verify/*` return 500 while `/hello` stayed
200; PgBouncer client-write timeouts appeared while direct-postgres active
load remained low; and crisp accepted public SYNs but sent its SYN-ACKs out the
LAN NIC, so TCP-open probes disagreed with every application handshake. The
same audit identified a payment-completion update whose indexed lookup still
fans out to millions of contract rows per call. Sections 2.7, 2.10-2.11,
4, 8.6-8.7, and 14.5 record the discriminators.

Intended consumer: a monitoring service with read access to pg (primary),
redis (cluster, all nodes individually), and service logs. Each signal below
specifies: WHAT to measure, HOW (query/command), HEALTHY vs BROKEN bands, and
the ACTION line the alert should carry. Section 6 explains how we separated
real issues from noise; section 7 is the alert emission spec.

Automated entries declare a short semantic key on a `Probe:` line. That key
maps to `server/monitor/signal_short_key.go` and
`signal_short_key_test.go`; the Go file keeps a comment linking back to this
section number. Use the same convention for every new automated entry.

Cadence mode starts each probe immediately, but admits at most four concurrent
probe executions. Most probes reach the production boundary through SSH; an
unbounded 29-probe startup wave caused a real SSH connection rejection on
2026-08-29 and a false visibility alert while the same target remained healthy
when sampled alone. Keep this bound when adding probes, including probes whose
cadences collide after startup. Likewise, a probe interrupted by an intentional
monitor shutdown is a lifecycle event, not loss of production visibility, and
must not emit a `monitor/visibility` alert. Cadence mode enforces each alert's
consecutive-tick `Sustain` value and resets that identity after a healthy tick;
`--once` deliberately reports current violations immediately for diagnosis.

Related docs: FOLLOWUP.md (open items ledger), redis conf overrides in
xops .../redis/redis.conf.j2, grafana redis-cluster dashboard + alert rules.

---

## 1. Tier-0 vitals (always-on, 60s cadence)

The five numbers that, together, tell you in one glance whether main is fine.
During the incident we polled exactly these in a 65s loop.

### 1.1 Contract creation rate — THE user-facing throughput proxy
Probe: `contract-rate`

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
Probe: `task-canaries`

`UpdateClientLocations` runs every ~30s, writes redis across many slots,
completes in 3–15s. It is the perfect canary: if redis is sick ANYWHERE on the
write path, it errors within a minute.
```sql
-- completions (locations) in the last 3 minutes: healthy 12–25, broken 0
SELECT count(*) FROM finished_task
WHERE function_name LIKE '%UpdateClientLocations%'
  AND run_end_time > now() - interval '3 minutes';

-- error state of all redis-heavy recurring tasks + THE ERROR TEXT
WITH failures AS (
  SELECT split_part(function_name,'.',3) AS task,
         reschedule_error_count, run_at, claim_time,
         left(coalesce(reschedule_error,''), 160) AS last_error
  FROM pending_task WHERE reschedule_error_count > 0
)
SELECT task, count(*) AS failing_rows,
       count(*) FILTER (WHERE run_at > now()+interval '5 minutes') AS parked,
       count(*) FILTER (WHERE claim_time > now()-interval '2 minutes') AS fresh_claim,
       max(reschedule_error_count) AS max_errors
FROM failures GROUP BY task;
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
- GOTCHA — group before limiting. On 2026-08-29, ten unfunded
  `AdvancePayment` rows filled the monitor's old `ORDER BY error_count LIMIT
  10`, hiding the independently failed `UpdateClientScores` row and its Redis
  `:6402` write timeout. The probe now groups the complete failing set by task
  function and emits one alert identity per family, carrying family/parked/live
  counts and one representative error. Never cap raw rows before this grouping.
- GOTCHA — one task family can still contain several causes. On 2026-08-30,
  `AdvancePayment` had 384 failing rows: 368 wallet-insufficient, ten
  connection-cleanup deadlines from §2.10 retention, five
  `processor-invalid-destination` 400s, and one processor 429. The
  highest-error representative belonged to the wallet class and made a
  single-cause explanation falsely describe the other 16 rows. The probe now
  computes a complete bounded cause breakdown before it selects a
  representative error. When more than one class exists, the family alert says
  it is mixed and builds its action and verification from only the classes
  present in that snapshot; stale guidance for an absent class is itself an
  alerting defect. A sample is evidence, not permission to apply its diagnosis
  to every row. Keep invalid-destination separate from generic processor 400s
  because only that typed, definitive pre-chain result is safe to unpin
  (§5.7).
- GOTCHA — `parked` and `fresh_claim` are independent snapshots, not disjoint
  buckets. During reschedule handoff, a row can already have `run_at` more than
  five minutes in the future while its prior attempt's claim heartbeat remains
  fresh. On 2026-08-30 the one disabled `RefreshVerifyProxyEgress` row therefore
  read parked=1 and fresh_claim=1. Report that overlap explicitly and never add
  the two counts or describe `fresh_claim` as a guaranteed active retry.
- GOTCHA — an undefined PostgreSQL object is an activation-order signal, not a
  generic task retry. Classify SQLSTATE 42703/42P01/42883/42704 as
  `schema-object-missing`, retain the exact object name, and compare the
  running binary requirement, successful `migration_audit` head, and §8.9
  artifact table. A behind head means the migration phase did not precede
  dependent services; a current head with a missing object is
  `migration-schema-drift`. Never create the object by hand or delete the task
  row to hide either condition.
- GOTCHA — long-running vs stuck: a live run shows claim_time refreshing
  every ~10s (the keepalive bumps claim_time+release_time). Frozen claim +
  future release_time = pre-lease-fix binary or killed worker. Current workers
  use a five-minute rolling lease, so a killed worker's claim self-releases
  within five minutes of its final heartbeat. A direct-postgres session lock
  still prevents duplicate execution if only the heartbeat is starved and the
  original worker remains alive. A raw p95 is not sufficient when repeated
  defects have polluted the tail: use `min(2*p95, max(4*p50, 20m))`, with the
  original one-hour fallback when history is absent. First select only the
  worst live row per task family so many payment rows cannot hide a singleton
  maintenance task. Then query and verify the matching task id's authoritative
  `eval active` elapsed time; `run_at` is only the due time, and a task that
  waited in the queue but began less than the guard ago is not long-running.
  The 2026-08-30 reaper recurrence supplied the regression values: p50 42s,
  p95 3,552s, due age 6,283s, and matching heartbeat 6,220s. The old p95-only
  rule waited until 7,104s; the median-tail guard alerts at 1,200s.

### 1.3 pg idle-in-transaction count — the redis-latency mirror
Probe: `pg-state`

```sql
SELECT count(*) FILTER (WHERE state = 'idle in transaction') AS idle_in_tx,
       count(*) FILTER (WHERE state = 'active') AS active,
       max(now() - xact_start) FILTER (WHERE state = 'idle in transaction') AS oldest
FROM pg_stat_activity WHERE backend_type = 'client backend';
```
- HEALTHY: idle_in_tx < 30, active < 20, oldest < 1 min.
- BROKEN: idle_in_tx > 100 requires attribution. Redis latency leaking through
  tx-scoped calls produced 563 during brownouts (2 when healthy), but a bounded
  close cohort can also put many workers briefly between statements. Report
  both the highest-count query shapes and the single continuously oldest
  transaction; they can have different owners. Oldest transaction age > 30
  min = leaked transaction pinning the vacuum xmin horizon (kills autovacuum
  silently).
- BROKEN: active > 100 with wait_event '-' (on-CPU) = a query-plan CPU wall,
  not load (360–390 seen 2026-07-17 vs ~6 healthy; idle-in-tx elevated too but
  redis was healthy — check 1.4 to disambiguate). pgbouncer kills queued
  clients with query_wait_timeout while direct 5432 connects fine → 5.8.
- KEY INSIGHT: pgadmin-style "connection utilization" is NOT query load.
  Real active backends were ~6 even during the worst incidents. Always split
  by state before concluding anything about load.

### 1.4 redis cluster state + per-node liveness
Probe: `redis-cluster`

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
Probe: `log-errors`

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
+ distinct target (ip:port) set, and normally should not use raw volume as
severity. The explicit exception is a class where each line is itself a
mutation exposure: `netescrow-negative` warns on any line and pages at
>=100/minute for one service/site. A NEW
signature appearing at rate (an unseen error shape / panic frame) is a
signal even when no known class matches — report it as class `novel` with
the sample line. Apply the novelty threshold to the most frequent normalized
shape, not the sum of unrelated shapes. Public web endpoints routinely receive
scanner bursts across dozens of nonexistent paths; nginx logs those misses at
error level, but many one-off paths do not constitute one recurring server
failure. Keep the total and distinct-shape counts as diagnostic context.

Implementation regression learned 2026-08-30: the standing tailer, restart
health, and scanner-overflow tests existed, but `Monitor.RunLoop` still ran the
bounded one-minute `logWindowProbe`; no production path instantiated the
tailers. Sequential service pulls therefore undercounted a net-escrow storm
whose manual pull reached the 10,000-line query cap in 14 seconds. `RunLoop`
now replaces only the registered bounded `log-errors` probe with one
`warpctl logs ... --since=1s -f` stream per discovered service and owns their
shutdown. One-shot `Run`/`RunSignal` retain the bounded query. The deterministic
runtime test requires the exact standing command, so tested-but-dormant tailer
code cannot recur.

A live scheduler control immediately found a second boundary in that wiring.
The v71 watcher started its streams at 19:18:56Z, but the generic RunLoop
startup wave drained `log-errors` at 19:19:15Z and labelled that roughly
19-second fragment `/min`; the following drain arrived only 46 seconds later.
The signal was also competing with SSH and database probes for the four remote
execution slots, so a collision could stretch a later rate window. Standing
logs now wait one complete cadence before their first drain and bypass the
remote-command slots—the check only drains in-memory counters. The
deterministic scheduler test fills every remote slot, proves there is no
partial startup callback, then proves the log drain still runs exactly on its
own cadence. This preserves both continuous ingestion and meaningful
per-minute storm thresholds.

The live watcher found a third observability boundary at 22:48Z on
2026-08-30. A bounded `warpctl logs` query reached Grafana/Loki, but the HTTP/2
connection was retired with `GOAWAY` while `query_range` was in flight. The Go
HTTP client returned that transient transport error and `warpctl` panicked,
turning a safe idempotent read into a failed monitor helper and occasionally a
full command-timeout visibility gap. Warp commit `5b65a16` retries
`query_range` transport failures and retryable 429/502/503/504 responses up to
three times with context-aware bounded backoff. The synthetic transport returns
the production GOAWAY on its first request and a valid Loki response on its
second, proving the observation survives without duplicating a mutation (the
request is GET-only). Exhausted retries still fail visibly; they must not be
relabelled as a healthy log window. Standing WebSocket tails retain their
separate reconnect lifecycle.

The replacement watcher exposed the corresponding stream-boundary defect at
22:54Z. `warpctlStream` merged local stderr into the stdout stream containing
remote log lines. When all three retryable Loki requests returned 502,
`warpctl` wrote retry diagnostics and its final `panic: Loki query error` to
stderr; `log-errors` consequently paged taskworker and web as though those
services had panicked 70 times/minute. The stream now classifies stdout only
and leaves stderr on the monitor's own diagnostic channel. The deterministic
test emits an ordinary remote line on stdout and the exact exhausted-502 panic
shape on stderr, then proves the service panic finding stays healthy. An
exhausted query still restarts the tailer and is surfaced by tailer health; it
is neither hidden nor attributed to the observed service.

That Grafana startup outage exposed one more feedback loop. `tailOnce` waited
for the `warpctl` child but discarded its nonzero exit status, so `run`
mistook every exhausted 429/502 query for a clean stream rotation and reset
its retry delay to one second. Eight services each reached 35 restarts in one
minute, adding query load while the observation backend was least able to
serve it. `tailOnce` now returns `cmd.Wait` errors after a successful stdout
scan; only a true exit-zero rotation resets the delay, while failures use the
existing exponential backoff capped at one minute. The synthetic child exits
2 and proves that failure reaches the backoff branch. Tailer restart findings
remain visible, but the monitor no longer amplifies their cause.

---

## 2. pg signal catalog (beyond tier-0)

### 2.1 Active query sampling (what is the load, really)
Probe: `active-queries`

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

Use completed runtime history before calling a known heavy shape stuck. On
2026-08-29 the `network_connection_reliability_score` INSERT had one backend at
877s, but 99 completed calls averaged 640s and its observed maximum was 950s;
that was normal work, not a database incident. The probe now warns for a new
shape after two minutes, a known shape after twice its completed mean, or five
concurrent copies regardless of history. The aggregate active-pileup signal
still independently pages whole-database saturation.

PostgreSQL utility statements are not represented in `pg_stat_statements`.
Daily `DbMaintenance` intentionally runs `REINDEX TABLE/INDEX CONCURRENTLY`
with a two-hour per-object timeout; a 40KiB `web_search_ingest_state` rotation
was observed completing normally after 420s. The active probe treats those
statements as known bounded maintenance until the same two-hour limit, after
which it warns; task canaries report the resulting maintenance error as well.

### 2.2 Wait events on active queries
Probe: `wait-events`

`LWLock:WALWrite` clusters = WAL pressure (check checkpoint cadence,
max_wal_size — a forced checkpoint every < 5 min melted main earlier this
month). `IPC:MessageQueueReceive` = parallel workers. `Client:ClientRead` on
active = server waiting on client mid-protocol.

Count alone is not a stall discriminator for `Client:ClientRead`. At 11:28Z on
2026-08-30, consecutive samples contained five to seven active ClientRead rows,
but the oldest was reported as 0s; direct millisecond sampling showed rotating
PIDs only 2–4ms old on ordinary `BEGIN`, `COMMIT`, and indexed reads. That is
healthy protocol handoff under concurrency, not five persistent waiters. The
probe now ignores a ClientRead cluster until its oldest active command reaches
one minute. Other wait classes retain the five-backend cluster rule, and a
single ClientRead older than one minute still identifies a client/pool path
that must be attributed. The rebuilt watcher validated the negative branch:
a direct production sample again found seven distinct ClientRead PIDs with a
4ms oldest command, while the wait-events probe emitted no alert.

`Lock:virtualxid` usually means concurrent index maintenance is waiting for an
older transaction. Apply the same two-hour `REINDEX ... CONCURRENTLY` grace as
§2.1: at 07:08Z on 2026-08-30, the daily reindex of `pending_task` had waited
493s behind an expected reliability rebuild, and the old generic one-minute
rule emitted a contradictory warning even though §2.1 correctly classified the
reindex as bounded maintenance. The probe now suppresses only that named
maintenance shape below two hours. A non-reindex virtual-XID wait, or a reindex
at/above two hours, still warns and must identify its blocker with
`pg_blocking_pids` before either backend is canceled.

At 08:01Z the same chain crossed the task-overdue band: `DbMaintenance` had a
fresh claim heartbeat 3,683s after `run_at`, while its `pending_task` concurrent
reindex was in `waiting for old snapshots` and PostgreSQL named the active
`client_reliability_running` INSERT as blocker. That is one shared reliability
anchor incident, not an independently stuck maintenance worker. With fewer
than ten recent maintenance completions there is no meaningful seven-day p95;
the task probe uses and now displays its 1,800s fallback instead of rendering a
misleading `p95 0s`. The reliability operation later proved not to be bounded
by ordinary duration: it reached the exact task deadline described below. Let
the configured deadline arbitrate it, roll out §3.7's checkpointed cadence fix,
and verify the reindex advances after each short checkpoint releases its
snapshot.

At 08:29:40Z the shared cause crossed its decisive boundary. Taskworker logged
`eval error(7200.00s)` for `UpdateReliabilities` with
`Interrupted: failed to deallocate cached statement(s): conn closed`; three
seconds later a new claim was active with the identical
`min_time=2026-08-30T05:46:09.315400597Z`. The original blocker disappeared and
the concurrent maintenance advanced from `pending_task` to
`network_create_attempt`, but replacement PID 788299 immediately became the
new `waiting for old snapshots` locker. This is a rollback/retry loop, not
successful release: a single all-lookback transaction throws away completed
work at the deadline and repeats it. The task alert recognizes this exact error
shape only after the matching eval duration and unchanged successor args are
confirmed. Its action is per-lookback transaction checkpoints plus §3.7's
maintenance-aware cadence—not a larger deadline or manual retry.

The replacement did eventually complete at 08:43:45Z in 842s. All four
`client_reliability_running_window` rows advanced durably from max block
29,801,147 to 29,801,310 (last re-anchor 29,801,308), and the next task was
scheduled cleanly for 09:13. The concurrent reindex then left
`waiting for old snapshots` and entered `building index: scanning table` on
`transfer_contract_pair_open_create_time_ccnew`. This validates both sides of
the discriminator: marker movement plus an advancing downstream phase is real
recovery; the first attempt's PID replacement without either was not.

That recovery exposed a second alert-state distinction. At 08:47Z the overall
`DbMaintenance` task was still 6,529s past `run_at`, but its current
`transfer_contract_pair_open_create_time_ccnew` rebuild had no blocker and was
actively in `building index: scanning table` at 10,114,170 / 23,790,366 heap
blocks. A generic "stuck task" action discards the evidence just as badly as
calling a blocked reindex healthy. The task probe now carries relation, index,
phase, query age, wait/blocker fields, and block progress in either state. When
no blocker is present, compare query age with the two-hour per-object limit and
require phase/block movement on consecutive samples; do not cancel or duplicate
a progressing rebuild merely because the serial task's total age includes its
earlier objects and waits.

The progressing case then completed cleanly at 08:50:10Z: DbMaintenance's
total duration was 6,609s, with no task or post error. A new transfer-contract
autovacuum began immediately afterward and was scanning the heap normally.
This validates the alert transition end to end: old-snapshot evidence required
the shared reliability diagnosis; later block progress required observation;
and clean task completion—not cancellation—was the terminal state.

The next nominal reliability cycle supplied a clean recurrence of the cadence
defect. Its markers were only at max block 29,801,310 / last re-anchor
29,801,308, yet the row claimed at 09:13:56Z and by 09:14:02Z was already
executing another full `INSERT INTO client_reliability_running`. At that instant
the transfer-contract autovacuum had been active for more than 20 minutes. No
error retry or missing marker was needed: the deployed 20-minute threshold is
simply shorter than the 30-minute task cadence, so every ordinary cycle
qualifies for a full anchor and starts it beside established maintenance. This
is direct production validation for both the four-hour cadence and the
five-minute optional-anchor maintenance deferral. Bootstrap/backward-window
repairs remain mandatory; this marker state was neither.

That clean recurrence completed at 09:27:17Z in 811.12s. All four markers
advanced together from max block 29,801,310 to 29,801,354 (last re-anchor
29,801,352); until that final commit every marker remained unchanged even as
the backend moved through the lookbacks. The successful result confirms that
the deployed math can finish, but not that its transaction boundary is safe:
an interruption before the one all-lookback commit would still discard all
811s. The checkpointed implementation makes each marker movement durable, and
the maintenance deferral would have selected rolling updates rather than
starting this optional anchor beside the already 20-minute-old autovacuum.

The 09:57 cycle repeated the same clean discriminator. It completed in
701.08s with no task error, but all four markers stayed unchanged for roughly
11 minutes before advancing together from max block 29,801,354 to re-anchor
block 29,801,394; the final rolling update reached 29,801,398 while retaining
29,801,394 as the re-anchor marker. The next task was scheduled normally for
10:39. This proves the deployed computation can recover and finish even while
the concurrent vacuum completes, but also proves every ordinary 30-minute
cycle still selects another full all-lookback anchor. Keep the four-hour
cadence, maintenance deferral, and per-lookback commits; one successful
701-second transaction does not make that rollback unit acceptable.

The 10:39 cycle repeated the deployed anchor under heavier overlap. It ran
948.17s while Payout, close recovery, and the successor transfer-contract
vacuum were active, then completed cleanly at 10:54:51Z. All four final
markers reached max block 29,801,440 with re-anchor block 29,801,435, and the
next cycle was scheduled normally for 11:24:51Z. This is a completed but
needlessly recurring full anchor, not a stuck task: retain the four-hour
source cadence and per-lookback commit boundary, and let the progressing
vacuum use the released horizon.

The scheduled 11:24:53Z cycle repeated the same behavior and completed
cleanly at 11:38:11Z in 798.39s. All four windows again advanced together to
max block 29,801,485 with recompute block 29,801,483, and the deployed
scheduler immediately queued the next cycle for 12:08:11Z. This is another
successful computation but another needless full-anchor transaction beside
close and vacuum recovery. It strengthens the cadence/checkpoint diagnosis;
it does not justify the deployed 30-minute re-anchor behavior.

The 12:08:14Z cycle completed cleanly at 12:26:22Z in 1,088.04s while two
legacy retention fan-outs, close recovery, and the index-vacuum phase
overlapped. Until commit, all four markers remained at max block 29,801,485;
they then advanced together to 29,801,529 with recompute block 29,801,526.
During the anchor, `transfer_contract` dead tuples crossed 10.59M and the
vacuum advanced to 3/13 indexes; the monitor correctly named the reliability
INSERT as the old MVCC horizon and advised letting it finish. Marker movement
plus the authoritative `finished_task` row prove clean release. The longer
duration under shared write load reinforces the four-hour optional-anchor
cadence and per-lookback commits; it is not evidence for a larger deadline or
vacuum cancellation.

The 12:56:25Z cycle extended the same load-sensitive tail. It completed
cleanly at 13:19:01Z in 1,356.13s while the successor vacuum, a close cohort,
Payout, and the tenth net-escrow sequence overlapped. Direct samples at
12:57Z, 13:04Z, and during the run showed all four markers fixed at max block
29,801,529/recompute block 29,801,526; the final commit advanced them together
to max block 29,801,577/recompute block 29,801,575. At completion the
`transfer_contract` vacuum had reached `vacuuming indexes`, so neither worker
was stuck. This is another clean deployed result and another 22-minute
all-lookback rollback unit. Retain the four-hour optional-anchor cadence,
maintenance deferral, and per-lookback commits.

### 2.3 Planner-flip detection
Probe: `planner-flips`

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
Probe: `vacuum-health`

```sql
SELECT relname, n_dead_tup, last_autovacuum FROM pg_stat_user_tables
ORDER BY n_dead_tup DESC LIMIT 10;
```
`n_dead_tup` above 10M on a hot table, or an old `backend_xid`/`backend_xmin`
horizon candidate → warn. If a table declares a larger fixed
`autovacuum_vacuum_threshold`, use that value as the dead-tuple alert floor;
the effective threshold is `max(10M, configured fixed threshold)`. Rank the
oldest combined horizon, then break equal-horizon ties by the oldest
transaction/query start; fresh snapshots inherit an old in-progress xid and
must not arbitrarily displace its real long-running owner. Report PID, both
xids, backend/application name, age, and query, and include
`pg_stat_progress_vacuum`: an active `pg_dump` / `COPY TO` snapshot pins
cleanup just as an idle-in-transaction session does. Autovacuum thresholds are
hand-tuned per giant table because default scale factors never fire on 600M-row
tables.

That configured-threshold rule prevents an intentional cascade victim from
becoming noise. At 06:53Z on 2026-08-30, `transfer_escrow` reported 10.59M dead
tuples with no active vacuum and no old horizon holder, but its fixed vacuum
threshold is 25M (and insert threshold 50M). Vacuum was not due. The former
generic 10M comparison emitted a false warning; the probe now reads each
table's reloptions and keeps the 10M floor for tables without a larger fixed
threshold.

2026-08-30 cross-signal example: `transfer_contract` reached 11.74M dead
tuples while the legacy §2.10 retention statement continued updating about
2.16M rows/call. The sampled oldest MVCC horizon candidates had transactions
only 0–1s old, so no long snapshot pinned cleanup. Autovacuum completed at
05:20Z and reduced the estimate to 7.35M, then a new scan began normally. The
table already had its fixed threshold and 2ms cost delay. This is writer churn
outrunning vacuum, not a reason to lower the threshold or kill ordinary
sub-second client transactions; remove the unbounded retention writer.

The same writer made the required sampling cadence explicit later that hour.
Between 06:13Z and 06:24Z, `transfer_contract` dead tuples rose from 10.35M to
14.23M. Its autovacuum was 3,570s old, had scanned all 23,781,003 heap blocks,
and remained in `vacuuming indexes` while one to two legacy payment-retention
updates overlapped and the aged open-contract cohort resumed rising. An hourly
probe cannot describe that feedback loop in time, so `vacuum-health` samples
every five minutes and carries vacuum age, heap scan/vacuum progress, index
vacuum count, and the oldest combined MVCC horizon candidate. When the heap is fully
scanned, no old horizon exists, and dead tuples still rise, bound the writer;
do not cancel a progressing index vacuum or misdiagnose a sub-second client
transaction as the pin. The estimate peaked at 16.03M, then fell to 705,980
when that autovacuum completed at 06:34:09Z; the retention probe was also clear.
That recovery confirms cleanup was progressing and the bounded writer—not a
vacuum kill or threshold change—is the durable fix.

The 07:23Z sample exposed an attribution trap and a second shared cause. Every
fresh PostgreSQL snapshot inherited the same old `backend_xmin`, so ordering on
that field alone had arbitrarily named a seconds-old contract read. Ranking
`max(age(backend_xid), age(backend_xmin))` and then the oldest transaction/query
start instead selected the 3,200s `UpdateReliabilities` transaction, whose xid
horizon was 6.46M transactions old. It had begun before the active
`transfer_contract` autovacuum, which had scanned the full heap and was in index
vacuum while dead tuples reached 13.10M. The task's pre-fix full re-anchor on
every half-hour cycle therefore also pinned cleanup and the concurrent reindex;
use the four-hour re-anchor fix in §3.7, let a progressing bounded anchor
finish, and verify both task cadence and vacuum recovery. Do not cancel the task
or retune vacuum to hide the common root.

That horizon restricted visibility; it did not make all reclamation
impossible. The long vacuum completed at 07:54:11Z while the reliability
transaction was still active, reducing the estimate from 16.09M to 9.52M dead
tuples, and a successor vacuum began five minutes later. An old horizon can
therefore leave a newer dead cohort behind while still allowing older tuples to
be removed. Verify consecutive samples and downstream maintenance after the
anchor releases instead of interpreting either partial recovery or one active
horizon as an all-or-nothing vacuum state.

The 16:23Z anchor supplied the same control later in the observation window.
At 16:33:59Z its `UpdateReliabilities` INSERT was the oldest useful MVCC
horizon: the transaction was 614s old and the horizon was 700,598 xids old.
While it remained live, the `transfer_contract` dead-row estimate rose from
13.247M to 16.537M, but the already-complete heap scan continued through its
index work. Autovacuum finished at 16:43:06Z, before the anchor itself
completed cleanly at 16:45:44Z. By 16:56Z the estimate was 7.995M and a
successor vacuum had normally begun scanning the 23.99M-block heap. That
sequence again proves a progressing vacuum can reclaim an older cohort despite
the anchor, while the avoidable all-lookback transaction still prolongs the
horizon and writer feedback. Preserve the maintenance deferral and
per-lookback checkpoints; do not cancel either progressing operation.

The successor vacuum supplied the terminal cleanup boundary for that same
writer wave. While the next edge-0 score, reaper, and close tasks remained
active, `transfer_contract` dead tuples rose through 10.8M, 13.75M, 15.51M,
and 18.20M. The vacuum had already scanned all 23,989,241 heap blocks and was
advancing through index cleanup rather than waiting on a lock. It completed at
17:58:48.522408Z without intervention, and the 18:02Z table sample reported
884,223 dead tuples. That is a decisive recovery certificate for the vacuum,
not for the writers: at the same sample the still-slow close path left 513,826
contracts open, including 444,919 older than five minutes and 100,709 older
than 30 minutes. Preserve the bounded score, retention, reaper, and close
fixes; vacuum completion removes accumulated cleanup debt but cannot make an
unbounded or process-starved task checkpoint safe.

The anchor did not commit at its 08:29:40Z deadline. Its retry opened a new old
snapshot within seconds, so a momentary blocker-PID change was not vacuum
recovery. Verification must follow the replacement claim and require durable
running-window marker movement, then observe the concurrent reindex and vacuum
chain advance. The checkpointed implementation makes that observable boundary
one lookback transaction at a time; a later timeout no longer rolls all earlier
markers back.

The next cycle demonstrated the resulting write feedback even though it
completed. By 09:31Z the transfer-contract autovacuum had run for 2,454s,
scanned all 23,793,748 heap blocks, and was still vacuuming indexes while the
dead-row estimate rose to 20.91M. The reliability anchor had released, but one
legacy retention execution was still active at 116s. This transition matters:
after the old horizon disappears, continued growth is evidence of the bounded
but still-deployed high-row writers, not permission to cancel a progressing
vacuum. Roll out both source fixes and require the eventual vacuum completion
plus consecutive sub-10M samples.

At 09:42Z the oldest useful horizon made that remaining writer explicit: a
98s transaction was executing the legacy `UPDATE transfer_contract SET
reap_time = ...` while dead tuples held at 21.66M. The autovacuum itself was
active on `IO:WalSync`, had no blocker, and remained in index cleanup. The
vacuum probe now recognizes this query shape and names the shared
`retention-fanout` cause, the durable pending-queue/cursor-batch fix, and the
payment retry-safety constraint. It must not suggest canceling the healthy
vacuum or tuning around the unbounded writer.

The same live vacuum exposed a monitor compatibility blind spot at 09:47Z.
PostgreSQL reported that it had processed 4 of 13 indexes while the legacy
`index_vacuum_count` remained zero. Reporting only that completed-cycle
counter hid real forward progress. The probe now reads
`index_vacuum_count`, `indexes_processed`, and `indexes_total` through
`to_jsonb`, so an absent version-specific field does not break the query, and
reports both the legacy count and current per-index progress.

That discriminator was validated minutes later without intervention. The
same vacuum advanced from 5/13 to 9/13 processed indexes, completed at
10:03:44Z, and reduced the dead-tuple estimate from 22.68M to 17,612 while
`UpdateReliabilities` was still seven minutes into another full anchor.
Autovacuum count advanced to 101. This was a progressing cleanup worker with a
large removable cohort, not a candidate for cancellation; retain consecutive
post-completion samples because continuing deployed writers can build the next
dead cohort quickly. Ten minutes later the estimate had already returned to
7.19M while the next 100k close cohort was actively draining the open backlog.
No legacy `reap_time` fan-out was active; the sampled writers were ordinary
roughly one-second per-contract `SET outcome, close_time` transactions. When
that query is the selected horizon, the probe now names it as bounded recovery
work rather than an old pin: let the closer and vacuum advance, retain the 25k
task checkpoint, and use the open-contract age buckets to verify drain before
attributing the next dead-row wave to retention.

The successor vacuum exposed a second benign-reader attribution case at
10:36Z. After scanning all 23,980,545 heap blocks it entered index vacuum while
dead tuples reached 14.10M, and the selected horizon was a seconds-old Payout
transaction first building `temp_account_payment` and then reading the
`transfer_escrow_sweep` subsidy time range. That bounded payment planner can
retain an MVCC snapshot, but it is not the legacy multi-million-row
`transfer_contract.reap_time` writer that created the dead-row wave. The probe
now recognizes both payment-plan query shapes and defers task diagnosis to the
Payout canary: let the attempt reach its bounded outcome, roll out the
transaction-local idle-timeout override and plan slices, and retain the global
five-minute timeout for every unrelated session. Live validation at 10:40Z
reported this classification while the vacuum advanced to 1/13 processed
indexes; it no longer advised removing an imaginary write fan-out.

After the old reliability and Payout horizons released, the 11:01Z sample
exposed the complementary false-owner case. The ranked candidate was an
ordinary two-second `search_provider_stats` SELECT carrying an inherited
`backend_xmin`, while the vacuum itself had advanced to 4/13 indexes. Its xid
age did not make that fresh reader the writer or persistent horizon owner.
The probe now treats a sub-minute read-only candidate as negative attribution
evidence, tells the operator not to cancel it, and redirects writer diagnosis
to the retention, close-backlog, and active-query probes. The rebuilt monitor
validated the branch at 11:02Z on another one-second network-client SELECT;
index progress and the sampled reader's normal disappearance, not intervention,
are the required checks.

That successor vacuum completed the proof at 11:33Z without cancellation or
retuning. After advancing through the index pass and heap-vacuum phase, the
dead-tuple estimate fell from about 15.09M to 5.45M; a new autovacuum then
started scanning the 23,980,545-block heap normally. The open set was still
791,095 (724,618 older than five minutes and 390,289 older than 30), one legacy
retention writer was active, and a full close cohort was still running. The
remaining close backlog and fresh writer churn therefore did not retroactively
make the completed vacuum stuck. Keep the bounded writer/close fixes and use
the new vacuum's consecutive progress samples as the next cleanup check.

### 2.5 Task-system meta-health
Probe: `task-health`

- finished_task per-function duration percentiles vs history (regression
  detector — e.g. scores export 12.5min normal; 37min during recovery).
- Duplicate concurrent executions (pre-lease-fix signature): same function
  claimed while a previous run is mid-flight — claim_time churn every ~30-60s
  with error count frozen.
- pending_task rows with reschedule_error_count ≥ 20 = something failed for
  hours (observed 23-28 during the day). Every such row is an incident.

The post-migration 2026-08-31 sample adds a bounded-backlog discriminator.
The first two successful `RemoveCompletedContracts` runs averaged 306.2s
against a 6.3s trailing p95 after migration 595 made the durable retention
cursor available. Current source gives each completed-payment assignment,
straggler assignment, and due-delete phase a five-minute wall-clock budget, so
a normally completed run near 300s is consistent with one phase draining its
budget; it is not by itself a stuck transaction or recurrence of the former
missing-column failure. Keep one recurring reaper, correlate
`contract_retention_pending` and cursor progress with §2.10, and require the
queue plus durations to fall on consecutive 30-minute runs. Do not add
concurrent reapers or raise the task deadline to hide durable backlog.

### 2.6 Open-contract set size — the close-backlog canary
Probe: `open-contracts`

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
- Trend evidence is the immediately preceding five-minute sample. On monitor
  startup, a high one-shot diagnostic says the trend is warming up rather than
  claiming growth. The continuous loop requires three high/rising ticks (ten
  minutes from the first observation); any flat or falling tick resets the
  streak. Do not default `rising` true while a longer baseline warms up. The
  2026-08-30 hardening was prompted by a 212,497 alert whose persisted samples
  later confirmed that this instance really was rising: 90,328, 91,679,
  124,317, 155,901, 182,471, then 212,497 at roughly five-minute intervals.

2026-08-30 close-tail discriminator: the set reached 244,019 (204,756 older
than five minutes, only 11,765 older than 30 minutes) while one
`CloseExpiredContracts` run processed a 52,970-contract cohort for 1,548s.
Immediately afterward, two cohorts of roughly 100,000 each completed in 22–23s
and the open set fell to 63,926 (23,387 older than five minutes). During the
slow tail, PostgreSQL showed only two active backends; 59 `idle in transaction`
sessions were the closer's 92 workers between commands (about 0.1s average
idle, 1.5s oldest transaction), not leaked transactions or a database CPU/lock
wall. Post-close aggregate comparison did not support a special contract mix:
the sampled slow and fast cohorts were each about 91–92% escrow-backed and
24–26% had nonzero used bytes. Instead, the legacy §2.10 payment-retention
statement was concurrently updating millions of `transfer_contract` rows (the
monitor repeatedly saw 72–110s active executions) and cleared immediately
before the two fast close cohorts. Treat that timing as strong evidence that
the close backlog was downstream write/storage contention from the retention
fan-out; deploy and verify the bounded retention fix before rewriting the
closer or raising its concurrency/connection pools.

The follow-on 2026-08-30 sample made the persisted-debt variant explicit. A
new close run logged a full 100,003-contract cohort and remained live for more
than 20 minutes while the open set rose from 299,011 to 323,756. At the first
sample, 250,518 were older than five minutes but only 6,891 were older than 30
minutes; three minutes later those buckets were 277,030 and 34,379. No
retention statement was then active and closer worker transactions were
sub-second. However, `transfer_contract` autovacuum had run for 1,958s, scanned
all 23,781,003 heap blocks, was still vacuuming indexes, and 8.1M dead tuples
remained. This is the same retention root after the fan-out statement clears:
write/vacuum debt continues to suppress close throughput. The probe therefore
includes five- and 30-minute age buckets in every high/rising alert and directs
the operator to correlate the close task, retention signal, and autovacuum
phase before changing closer concurrency. That 100,003-row cohort ultimately
logged `eval error(1800.82s) ... = Timeout`, exactly matching its 1,800-second
task deadline, then retried 18 seconds later. Per-contract commits survived,
but task-level progress did not: the retry had to select and scan again. The
source now caps a task cohort at 25,000 while retaining 92-way internal
parallelism and the 30-minute safety ceiling. A production-sized 100,003-row
backlog therefore checkpoints through five independently acknowledged tasks;
full cohorts schedule their successor immediately. Verify no new exact-1,800s
timeouts and that the aged buckets fall across those checkpoints.

The still-running legacy build reproduced that boundary once more at 06:55Z:
its next 100,000-row attempt logged `eval error(1800.77s) ... = Timeout`.
Automatic retry completed in 20.6s, the next six cohorts completed in 19.8–25s,
and the open set fell from 696,015 to 95,717 by 06:58Z, with zero rows older
than 30 minutes. That recovery proves the timeout was contention-sensitive and
that per-contract commits survived, but it also proves a task-level 100,000-row
checkpoint can still repeat expensive discovery after an exact deadline. Keep
the 25,000-row source checkpoint; do not raise the deadline based on the fast
retries.

The legacy boundary recurred a third time at 08:39:35Z:
`eval error(1801.18s) ... CloseExpiredContracts ... = Timeout`. The open set
had risen to 419,520 (358,485 older than five minutes and 70,126 older than 30
minutes). The identical task id retried 14 seconds later; within the next
minute, fresh successor task ids appeared about every 20–30 seconds, confirming
that per-contract commits survived and full cohorts were again draining. This
is the exact `Timeout` variant the task alert now explains as the deployed 100k
scheduler boundary. It reinforces, rather than changes, the deterministic 25k
checkpoint fix and its verification against consecutive aged-bucket samples.

The next deployed 100k cohort completed rather than timing out, but remained
too close to the same boundary: it ran from 09:33:04Z to 09:57:57Z
(1,493.20s) with `full=true`. Over the surrounding recovery, total open
contracts fell from 419,520 to 367,692 and the older-than-30-minute tail fell
from 70,126 to 1,607. That is durable close progress with only about five
minutes of deadline margin, not evidence that a 100k task checkpoint is safe.
This cohort also overlapped §5.11's sixth long reconcile and its subsequent
21,436 negative counters. The exact 2.02TiB opposite-direction repair proves
the closer exposed stale Redis reservations; it did not create them. Keep the
25k scheduler checkpoint and the page-local additive escrow fix as separate,
complementary remediations.

The following 100k cohort then hit the boundary again. Taskworker logged
`eval error(1801.33s) ... CloseExpiredContracts ... = Timeout` at 10:29:19Z;
the same task id was reclaimed seconds later and emitted a fresh 10s heartbeat
at 10:29:33Z. At the surrounding sample the open set was 436,567
(371,408 older than five minutes and 36,769 older than 30), three legacy
retention writers overlapped, and the successor autovacuum was still scanning
the heap. This is the deployed 100k checkpoint failing under recurring shared
write pressure exactly as diagnosed—not a reason to raise the deadline or
restart PostgreSQL. The deterministic 25k cohort remains the fix.

A later deployed cohort reproduced the full chain while cleanup debt was still
present. Task `01a05242-c226-d02d-1a3a-907f6084a454` reached
`eval error(1801.55s) ... = Timeout` at 11:11:46Z, was reclaimed under the
same id 17 seconds later, and needed another 504.55s to return `full=true`.
The immediate successor then completed another full legacy cohort in 33.13s,
while its successor was again still active after five minutes. Per-contract
commits therefore survived, but neither the quick second cohort nor the
successful retry made the 100k scheduler checkpoint safe. At the surrounding
monitor tick the open set reached 786,024, up 57,931 from the preceding sample;
719,370 were older than five minutes and 390,607 older than 30 minutes. Two
legacy retention writers simultaneously remained active for 114s, while
autovacuum had completed an index pass and entered heap vacuuming. This is
durable cleanup progress coexisting with arrival/write pressure above close
throughput, not a stalled vacuum or justification for a larger task deadline.
Retain the deterministic 25k checkpoint and bounded retention fixes. Even
after that vacuum completed, the 11:40Z monitor tick reached 881,722 open
contracts, up 57,493 from its preceding sample; 814,243 were older than five
minutes and 479,092 older than 30. The still-active 100k cohort was not keeping
pace with arrivals, so cleanup completion alone did not resolve the scheduler
checkpoint deficit. That cohort ultimately reached
`eval error(1800.72s) ... = Timeout` at 11:50:55Z. Its same-id retry returned
`full=true` in 22.57s, and the next two full successor cohorts completed in
21.33s and 21.72s. This is the same durable-per-contract/task-level-rollback
split: fast recovery after the boundary does not make the deployed 100k
checkpoint safe.

The next legacy cohort completed rather than timing out, but repeated the same
near-boundary/fast-successor shape. It ran from 11:52:36Z to 12:13:59Z
(1,283.00s), after the open set had climbed back to 852,348 (788,638 older than
five minutes and 448,648 older than 30). Its next three full cohorts completed
in 21.70s, 25.95s, and 22.00s; by 12:15:51Z the open set had fallen to 584,367
(517,757 older than five minutes and 182,292 older than 30). The 268k drain is
durable progress, while the 21-minute checkpoint remains much too sensitive to
shared write/vacuum pressure. Keep the 25k source cap and require consecutive
aged-bucket decline; do not infer safety from the fast successors.

The deployed boundary then recurred under another retention/vacuum overlap.
Task `01a05298-6315-fa3e-3719-55e95aac9de1` reached
`eval error(1800.69s) ... = Timeout` at 12:45:18Z. Its same-id retry completed
in 27.55s, and four new full successor cohorts completed in 20.01s, 22.11s,
19.96s, and 30.34s. The open set had peaked at 919,950 (854,226 older than five
minutes and 523,677 older than 30), then fell to 494,974 (422,960 and 95,016)
by 12:47:42Z. The concurrent autovacuum completed at 12:44:57Z and reduced the
dead-tuple estimate from 17.15M to 532,715. Both cleanup paths therefore made
durable progress, but the oversized task checkpoint still failed exactly at
its deadline while arrival and legacy retention pressure overlapped. Retain
the 25k checkpoint and bounded retention queue; neither a larger task deadline
nor vacuum cancellation addresses this reproduced boundary.

The following legacy cohort repeated the exact deadline under the successor
vacuum and reliability anchor. Task
`01a052b5-dd90-0999-08a1-de70429d62df` reached
`eval error(1800.86s) ... = Timeout` at 13:17:30Z. Its same-id retry ran from
13:17:36Z to 13:24:14Z and committed successfully in 397.89s; the historical
`Timeout` remains on that finished row, while a new successor task id proves
the recurring chain advanced. This is durable retry progress, not proof that
the deployed 100k checkpoint is safe: the first attempt still spent its whole
deadline beside vacuum/reliability work. Keep the 25k source checkpoint and
follow the new successor plus aged open buckets.

That successor supplied the next exact boundary. Task
`01a052d7-9750-7771-3efd-a76cd0275248` ran from 13:24:18Z until
`eval error(1800.72s) ... = Timeout` at 13:54:19Z while the successor vacuum,
an 18-minute net-escrow reconcile, and a 3,356,615-row legacy payment-retention
update overlapped. Its same-id retry committed in 24.84s. Eight immediately
following full cohorts then committed in 18.89–24.44s through 13:57:52Z, with
new task ids proving the chain advanced. This is the strongest form of the
checkpoint discriminator: the database and per-contract writes were not
wedged, because the exact same remaining work and its successors drained
quickly after the task-level rollback. The 30-minute first attempt is still a
production failure. Retain the 25k source checkpoint and bounded retention
queue; do not convert the fast retry into evidence for a larger deadline.

The following cohort supplied a tighter same-executor coupling to net-escrow
write amplification. Task `01a052f6-5c55-e78b-110d-dad7afffe710` ran from
13:57:54Z to 14:20:41Z (1,367.15s) on
`by-us-fmt-5-edge-3/g2`, container `4cf91fd25a2e`. The same executor was
simultaneously applying a 1,021.01s legacy `ReconcileNetEscrow` pass from
14:03:23Z to 14:20:24Z. The close cohort committed only 16.61s after that
fleet-wide Redis writer stopped; its next three full successors then completed
in 21.35s, 27.30s, and 22.29s. No task timeout was required to expose the
boundary, but executor overlap alone was not the causal discriminator. The
following close task `01a0530c-65aa-153e-19d8-82ad3698cf40` began on the same
container at 14:21:56Z, after the escrow writer had stopped, and still remained
live after 769s. At 14:28:18Z the open set was 435,994 (368,281 older than five
minutes), a legacy payment-retention writer had been active for 106s, and the
`transfer_contract` autovacuum had spent 1,521s scanning 15,383,494 of
23,980,545 heap blocks with about 10.0M dead tuples. It later entered index
vacuuming while the close task remained live. That follow-up falsifies simple
same-process Redis-walker interference and identifies the already-diagnosed
PostgreSQL write/vacuum debt plus the oversized 100k checkpoint as the close
path. Executor identity is still valuable chronology, but is not standalone
causal proof. Keep both source fixes—the 25k close cohort and the
page-local/no-op-skipping escrow reconciler—and do not raise the deadline.

The same close id then reached the exact boundary at 14:52:00Z:
`eval error(1800.83s) ... = Timeout` on
`by-us-fmt-5-edge-3/g2`, container `4cf91fd25a2e`. Its same-id retry moved to
edge-1/g2 and committed in 31.41s. A new full successor
`01a05328-7ab0-20bf-7a56-da7de2a04be3` moved to edge-0/g1 and committed in
21.89s. The following full successor
`01a05328-d675-7e6d-1991-20f454b4e1ce` landed back on edge-3/g2 and exceeded
1,000s while the open set rose from 621,149 to 676,121. That A/B/A executor
sequence proves a container-local contention component in addition to the
global retention/vacuum debt; the same 100k algorithm and live fleet state
were fast on two peers and slow again on the original executor. The slow
container concurrently carried a 34.7GiB-resident taskworker,
`RemoveDisconnectedNetworkClients` beyond 6,200s, and `UpdateClientScores`
beyond 2,500s. Host load was 20 on 72 CPUs, memory had 803GiB free, and the
container had no CPU throttle, memory event, or pressure, so this is
co-resident application work rather than host/cgroup starvation. The exact
share—Go heap scanning, serialized Redis cleanup, or task scheduling—is not
uniquely identified; keep the bounded fixes for all three paths and do not
restart the executor to erase the evidence.

That successor then supplied a same-host generation control. Edge-3/g2 reached
`eval error(1801.06s) ... = Timeout` at 15:23:04Z. The same task id retried on
edge-3/g1 container `786ae804bb97` and committed in 22.450s; its authoritative
`finished_task` row retained the prior `Timeout`. The next full successor
`01a05344-c984-35a7-ff1f-89df7a57b0ea` returned immediately to edge-3/g2.
Moving only between g2 and g1 on the same physical host reproduces the slow/
fast split without changing host memory, network, PostgreSQL, or Redis. The
process-level allocated-heap evidence in §2.12 is therefore the local discriminator,
while the 25k close cohort remains the durable checkpoint fix.

The next independent edge-1/g2 cohort supplied the deadline/reclaim sequence
again. Task `01a0534c-0b80-16c9-d801-6b052913efcc` started at 15:31:31Z,
logged `eval error(1801.47s) ... = Timeout` at 16:01:32Z, and was reclaimed
under the same id seconds later. The retry emitted a fresh 10.02s heartbeat at
16:01:49Z, then completed from 16:01:39Z to 16:19:15Z in 1,056.375s; its
finished row correctly retained `reschedule_error=Timeout` from the first
attempt. Six fresh full successor cohorts then completed in 20.961–24.270s.
The open set was 796,882 and rising near the boundary. A transient 585-session
`idle in transaction` count contracted to 147; sampled transactions were only
1–3 seconds old and dominated by the closer's bounded statements, so this was
worker fan-out between commands, not 585 abandoned sessions. The exact
deadline, durable retry, and immediate fast drain again validate the 25k task
checkpoint. Do not raise the deadline or connection pool based on either the
successful retry or transient count.

The 16:19Z close cycle repeated the production-sized checkpoint failure while
the open-set probe preserved user impact. Consecutive samples rose from
309,353 to 363,016, 418,771, 472,729, and 531,094 open contracts. At 363,016,
301,316 were older than five minutes and none were older than 30 minutes, so
this was a growing fresh cohort rather than ancient residue. After the exact
deadline and same-id retry described in §2.6a, catch-up closes reduced the set
to 244,296 by 16:56Z; 174,897 were older than five minutes and zero were older
than 30. The rise-and-drain pair is deterministic evidence that the close
checkpoint, not an abandoned session set, controlled the backlog. Retain the
25,000-row task checkpoint and require the aged buckets to keep falling after
rollout.

### 2.6a Close-task checkpoint duration — the live backlog precursor
Probe: `close-duration`

The open-set trend is the durable user-impact signal, but it intentionally
needs consecutive five-minute samples. Watch the authoritative taskworker
`eval active(<seconds>s) ... CloseExpiredContracts` heartbeat so an individual
checkpoint that leaves its healthy band is visible before the open backlog has
had ten minutes to mature. `pending_task.run_at` is only the due time and
`claim_time` is a moving lease heartbeat; neither is an execution-start clock.
For completed incidents, read `finished_task.run_end_time-run_start_time`.
Also retain taskworker `eval error(<seconds>s) (reschedule)` attempts: a timeout
never becomes a finished duration because the same pending task id is reclaimed.
Retain the latest overrun for 45 minutes so an immediate fast successor cannot
erase its precursor.

- HEALTHY: full deployed legacy cohorts normally finish in roughly 20–30s.
- WARN: a live or completed checkpoint reaches 120s. Include task id plus the
  live heartbeat's host/generation/container when present.
- DEADLINE: 1,800s is failure even when the per-contract commits survived.
  The retry repeats discovery because the task-level checkpoint did not
  commit; do not treat those durable child writes as task success.

The 14:21:56Z task `01a0530c-65aa-153e-19d8-82ad3698cf40` demonstrated the
lead time: its taskworker heartbeat exceeded 900s before the existing
open-contract probe's sustained trend opened at 536,761 contracts (474,389
older than five minutes and 141,039 older than 30). The live probe therefore
alerts at 120s, while the open-set buckets remain authoritative for impact and
drain. Correlate the duration with §2.10 and vacuum state. Executor identity is
chronology, not causal proof: the same task remained slow after the overlapping
net-escrow writer ended, while a legacy retention writer and
`transfer_contract` vacuum debt were active.

The post-migration 03:24Z and 03:39Z cohorts added a database-versus-executor
discriminator. Each selected exactly 25,000 contracts and took 879s and 871s,
then immediate full successors completed in roughly 5–10s and drained about
175,000 contracts in bursts. During a slow cohort, the closer's sampled
`UPDATE transfer_contract` calls averaged about 0.6ms, PostgreSQL showed no
lock wait, and sessions spent their gaps in `ClientRead`; the application was
not waiting on a database lock or slow statement. The exact edge-3/g2 process
was simultaneously pinned at four cores by the allocation-heavy score export
described in §2.12a. That establishes process-local CPU/allocation contention
as an amplifier for the slow-close cohort. The fast successors while the score
task was still active also show that co-residency alone is not a sufficient
cause, so retain the 25,000-row close checkpoint and its duration alert rather
than increasing the deadline or attributing every long cohort to one sibling.

Implementation convention: SIGNALS.md §2.6a (`close-duration`) maps to
`signal_close_duration.go` and `signal_close_duration_test.go`. The synthetic
lifecycle tests require the newest timestamped heartbeat to supersede an older
one, the same completed task id to suppress its lingering heartbeat, and a
different active successor id to remain visible. They also preserve the exact
1,800.83s rescheduled timeout when a short same-id retry follows it and the only
`finished_task` overrun belongs to an older checkpoint. A completed-retry case
places an even newer successor completion and heartbeat after that retry; the
query must still retain the terminal task id's own row, and executor attribution
must come from that exact id's heartbeat rather than the newest function
heartbeat. When a different successor crosses 120s, its active alert retains
the precursor failure's duration, task id, error, timestamp, and executor
identity rather than letting new activity erase the deadline incident.
An exact completed task id is authoritative for the full 45-minute incident
window; the two-minute completion-age bound applies only to the legacy
unlabelled duration fallback. Live task
`01a05446-9d23-9e06-7cd9-1e3db5d91423` exposed that distinction: the monitor
correctly rendered its 253-second completion, then incorrectly resurrected its
last 251-second heartbeat as active once the completion became 131 seconds old.
The shared lifecycle comparator now checks exact ids before the age fallback,
and the deterministic 131-second case prevents a completed run from
resurfacing.

The fleet log gateway is an observation path, not a prerequisite for close
visibility. If `warpctl logs` fails, the probe concurrently reads a bounded
45-minute `CloseExpiredContracts` window from the configured service hosts'
g1/g2 taskworker journals, with a 12-second per-host deadline and journald-side
filtering. It reads journald JSON and reconstructs the fleet envelope from the
authoritative journal timestamp, hostname, generation tag, and container id;
`-o cat` is unsafe here because it removes the timestamp and lets an older run's
larger elapsed heartbeat masquerade as the newest task. The synthetic 502 case
places an older 837-second run before a current 432-second run and requires the
fallback to retain the latter task id, duration, and host/generation/container
attribution. A partial fleet read remains an observation error unless its
returned lifecycle already proves an incident; it must never silently become a
healthy result.

After the version-594–597 migration completed on 2026-08-31, the generation
that had begun its checkpoint before the edge-0 reboot supplied a clean
schema-versus-algorithm control.
One checkpoint completed in 855s at 01:16:52Z, followed by short scheduler
successors, but the open set still rose from 717,709 to 730,751; the later
sample contained 689,561 contracts older than five minutes and 471,886 older
than 30 minutes. PostgreSQL showed the next block-size-one close task actively
refreshing its lease. The migration removed its schema blocker; it did not
erase the accumulated close and vacuum debt.

After edge-0 rebooted into fleet version `2026.8.30+1033129380`, its live
journal made the current boundary explicit: at 03:06:21Z it logged `found
25000 contracts to close`. The version tag independently contains
`closeExpiredContractsMaxCount = 25_000`, the cursor-backed retention queue,
and their deterministic tests. The 25k cap is therefore deployed, not a
pending remediation. Even so, the preceding checkpoints completed in roughly
760–777 seconds while the open set reached 1,175,304 (1,139,992 older than five
minutes) and a progressing `transfer_contract` autovacuum crossed 10.16M dead
tuples. Retain the cap and bounded queue, let vacuum and scheduled cohorts
drain the inherited debt, and require consecutive aged-bucket decline. Roll
the cap only where a live selection log still exceeds 25,000; do not prescribe
redeploying code that the executing version and journal already prove present.

The adjacent 03:27Z sample confirmed recovery direction without declaring the
backlog cleared. Open contracts fell to 1,021,961, including 982,373 older than
five minutes and 791,335 older than 30 minutes. `transfer_contract` dead tuples
fell from 10.16M to 5.28M; `pg_stat_progress_vacuum` showed a fresh heap scan at
9,885,975 of 24,254,708 blocks rather than a blocked worker. Keep the incident
open until consecutive samples continue that aged-bucket decline and close
durations return below 120 seconds, but do not interrupt the progressing
vacuum or add closer concurrency while this path is converging.

The 05:14:59Z autovacuum completion supplied the stronger post-vacuum control.
It reduced `transfer_contract` dead tuples from 11.56M to 0.49M before the
follow-up analyze and left zero blocking sessions. Nevertheless, open contracts
rose from 1,234,192 at 05:15Z to 1,340,672 at 05:28Z; the latter contained
1,294,787 older than five minutes and 1,074,628 older than 30 minutes. The
retention queue remained bounded at four payments with one cursor, and sampled
close updates were seconds-old or younger. During the same interval edge-3/g1
ran `UpdateClientScores` and `CloseExpiredContracts` together while holding
roughly four CPU cores and allocating 640–660MiB/s. Vacuum was no longer the
active bottleneck in this control: the old score fanout's process-local churn
was starving the co-resident closer while creation continued. Deploy the
target-oriented score fanout/alias cache from §2.12a, then require both worker
rates and aged open buckets to fall. Do not add closer concurrency; exact
co-residency remains an incident-specific causal control, not a claim that all
slow close cohorts share one owner.

Task `01a0537a-bb79-1dfe-a0fb-ae25bb4d3a31` supplied the sharpest executor
control at 16:52Z. Edge-1/g1 container `53ef545dc646` logged
`eval error(1800.88s) (reschedule) ... = Timeout`; the monitor immediately
rendered `phase=failed`, the exact task id, error, timestamp, and executor.
The same id was reclaimed on edge-4/g2 container `3c0a752d4433` and its
authoritative row ran from 16:52:36.353384Z to 16:53:00.255024Z—23.902s—while
retaining `reschedule_error=Timeout`. That 75x same-task duration split proves
the deployed 100k checkpoint is load-sensitive and proves why a fast retry
must not erase its failed precursor. It validates the monitor lifecycle rule
and the 25k source checkpoint; it is not a reason to raise 1,800 seconds.

Its later scheduled successor made the local amplifier visible without another
timeout. Task `01a05397-bb73-ca07-2df7-cd1e483c0e70` landed back on the
edge-1/g1 score-heavy process and ran from 16:54:11.917863Z to
17:17:32.289278Z (1,400.371s). During it the open set resumed rising through
315,750, 395,199, and 452,844. The colocated score export completed at
17:17:13Z, and this close committed 19 seconds later; the colocated reaper
followed 14 seconds after that. This ordering makes allocator pressure a
strong process-local wall-clock amplifier, while the 100k task still owns the
large rollback/discovery unit. Preserve both fixes: stream score export and
checkpoint close at 25k. A successful 1,400-second cohort is still far outside
the 20–30-second band and only 400 seconds from failure.

The next hourly placement repeated the exact A/B boundary on a third
executor. Task `01a053ad-eae5-752f-3e04-95696dff3a2e` shared edge-0/g1 with
the next score export, reached `eval error(1800.90s) ... = Timeout` at
17:48:27.076631Z, and let the open set rise to 567,403. The same id was
reclaimed on edge-3/g2 14 seconds later and its authoritative row ran from
17:48:31.198071Z to 17:48:54.551346Z—23.353s—while retaining
`reschedule_error=Timeout`. That roughly 77x split reproduces the failed
precursor/fast retry lifecycle and local allocator amplification independently
of edge-1. Keep the 25k checkpoint; do not normalize the rising historical p95
or raise the deadline.

Its full successor repeated the boundary on the same score-heavy executor.
Task `01a053ca-bd27-e4b7-5d8e-f5b6c2acd410` reached
`eval error(1801.17s) ... = Timeout` at 18:19:55.787918Z on edge-0/g1
container `4a6bbf31e2d4`; the open set had reached 720,664 and was still
rising. The same id was reclaimed on edge-1/g2 container `06abfbe03c32`, and
its authoritative row ran from 18:20:00.088051Z to 18:20:29.683999Z—29.596s—
while retaining `reschedule_error=Timeout`. The score-heavy edge-0 task did
not finish until 19 seconds after this peer retry, and its colocated legacy
net-escrow pass did not finish until two seconds after the retry. The same
work therefore remained healthy on a peer while the 100k checkpoint exhausted
its deadline on the hot process. This is another pre-rollout reproduction of
both the oversized checkpoint and its process-local amplifier; retain the 25k
checkpoint and bounded sibling paths.

The immediately following same-sized checkpoint repeated that control again.
Task `01a053e8-00d2-cdfd-7c2f-f4843d2f972a` reached
`eval error(1801.43s) ... = Timeout` at 18:51:54.281835Z on the same
edge-0/g1 container. The same id was reclaimed on edge-3/g1, and its
authoritative row ran from 18:51:59.461170Z to 18:52:21.335651Z—21.874s—
while retaining `reschedule_error=Timeout`. This approximately 82x same-task
split is a fourth independent reproduction of the oversized 100k checkpoint
plus the hot-process amplifier. It is pre-rollout evidence: require new
generations to complete recurring 25k checkpoints below 120s before clearing
the incident.

The next old-generation checkpoint supplied the non-timeout side of the same
boundary. Task `01a05406-222b-61a7-a780-cc6244cde015` remained on the
edge-0/g1 hot process and `finished_task` recorded a successful 1,568-second
run. It was still only 232 seconds below the hard deadline and roughly 65x the
healthy 20–30-second band. A successful terminal row therefore does not clear
the latency incident or weaken the 25k checkpoint fix; it shows the amplifier
is continuous near the deadline rather than a binary timeout-only failure.

The last observed old-generation checkpoint repeated the failed/fast-peer
control. Task `01a0541e-82bf-5677-cb49-aa25a23ecdf0` reached
`eval error(1801.41s) ... = Timeout` at 19:51:23.925745Z on edge-1/g2
container `06abfbe03c32`. The same id was reclaimed on edge-3/g1 container
`786ae804bb97`, emitted 10s and 20s heartbeats, and `finished_task` recorded a
rounded 25-second completion ending at 19:51:53Z. The monitor retains the
failed attempt as the incident while rendering `retry_phase=completed`, retry
duration/time, and the exact retry executor. This live validation also exposed
and fixed two evidence-loss bugs: selecting only the latest completion plus
latest overrun lost a retry after later cohorts completed, and using the newest
function heartbeat mislabeled that retry with a later successor's executor.
The probe now selects the exact active and terminal task ids in addition to the
ranked rows and parses executor identity for the terminal id specifically. The
deterministic later-successor test locks both invariants. This approximately
72x same-task split remains pre-rollout evidence for the 25k checkpoint and
process-local load sensitivity; it is not permission to erase the timeout or
raise its deadline.

### 2.7 New-connection rate — existing-sessions vs new-connects discriminator
Probe: `connection-rate`

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
  SELECT date_trunc('hour', disconnect_time),
         percentile_cont(0.5) WITHIN GROUP (ORDER BY
           EXTRACT(EPOCH FROM (disconnect_time - connect_time)))
  FROM network_client_connection
  WHERE disconnect_time >= now() - interval '6 hours'
    AND disconnect_time IS NOT NULL GROUP BY 1 ORDER BY 1;
  ```
  First correlate with deploys AND unit restarts (8.5): 2026-07-19 22:55 an
  ansible restart wave took the baseline 2.5k/min to a 7k plateau for 40
  min, a 15k/min final drain burst, then decay to baseline within ~6 min —
  with contract rate, canary, api error rates all healthy throughout. If NOT
  restart-correlated, a storm means something is killing established
  connections (transport, lb flapping, provide churn).
- LIFETIME-CENSORING CAUTION: compare matched `disconnect_time` windows, not a
  still-open recent `connect_time` cohort. The latter excludes every long-lived
  survivor and biases its median short. At 2026-08-30 06:32Z, the naive recent
  connect cohort read 24.7s versus 44.8s an hour earlier; disconnect-time
  cohorts read 35.7s versus 45.4s, with p90 375.1s versus 376.6s. Together with
  only 1.1–1.5x matching-hour connection rates and 14% higher contract
  creation, that ruled out a new reconnect storm as the primary cause of the
  open backlog and left slow close throughput under vacuum debt as the cause.
- MEDIAN-POLLUTION CAUTION: a 40-minute storm drags any trailing-hour median
  up to storm levels, so (a) the storm signal un-trips as the window fills,
  and (b) the RECOVERY back to true baseline then reads as < 50% "collapse"
  (false page observed 2026-07-19 23:42: contracts 4.5k/min vs a 10.9k
  churn-inflated median). Judge recovery against a pre-incident window; the
  probes fall back to the trailing-6h median whenever the hour median is
  >= 1.5x it.
- CONFIG-GENERATION VARIANT (2026-08-27): compare binary AND config versions.
  During a cross-service config rollout, `warpctl ls versions --sample` showed
  old api/connect binaries reporting the new config generation while same-tag
  containers had fresh `Up` times. New connects rose from 2.4-3.0k/min to
  14.7k/min (6.2x the matching hour-ago minute), the disconnected-session
  median lifetime fell 44.9s -> 14.3s, and 72,564 of 83,444 connections in the
  current 10-minute window had already disconnected. Contract creation rose
  to 33.1k/min and the open set to 292k because churn creates contracts; those
  high-side values were NOT organic demand or proof of health. Use §8.6 to
  distinguish this from a crash loop, then require the rate, lifetime, and open
  set to recover after the last restart.

### 2.8 Provider-selection freshness — the score-cache staleness canary
Probe: `selection-freshness`

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
- 2026-08-29 export-timeout variant: a rebuild ran 3,651s and then one
  cross-slot Redis pipeline failed with `write tcp ... -> ...:6402: i/o
  timeout`. The task-level retry restarted the entire hour-scale export after
  backoff, allowing the completion gap to cross 90 minutes even though the
  cluster was healthy when checked. `UpdateClientScores` used to send every
  location/group `SET` for one caller location as one giant pipeline from each
  of 48 workers. The writer now caps a wire batch at 512 idempotent `SET`s and
  retries only the failed chunk up to three attempts for transient transport,
  failover, LOADING, or READONLY errors. It deliberately does not retry pool
  exhaustion or permanent Redis errors. A completed earlier chunk is never
  replayed by the local retry. The pre-fix production row then recovered on
  its next task-level attempt: it completed at 2026-08-30 02:32:20 UTC in 598s
  and cleared `reschedule_error_count`, confirming a transient transport
  failure rather than a persistently unhealthy 6402 node.

  The 2026-08-30 process-level trace exposed the corresponding working-set
  defect before that first batching change was rolled out. Edge-3/g2 repeatedly
  cycled between 6.46GiB and 31.43GiB of allocated heap while its score task
  ran.
  Merely building a complete immutable operation list and then executing
  512-item slices would still retain every encoded sample for one caller
  location on each of 48 exporters. The writer now produces into the bounded
  buffer: a batch executes synchronously at either 512 commands or 8MiB of
  key/value payload, and its payload references are cleared before encoding
  resumes. An individually oversized value is sent alone. Each provider sample
  is encoded lazily as it is emitted, rather than materializing every sample
  for the current location first. Deterministic regressions produce 10,000 synthetic 1KiB
  values, prove the producer never gets 512 unexecuted values ahead, observe
  19 full batches plus one 272-value tail, exercise the independent byte cap
  including an isolated oversized value, prove flushed slots release their
  payloads, and retain the exact failed-batch retry checks above.

  The edge-0 scheduled reboot at 02:09:48Z supplied the process-exit variant.
  It interrupted task `01a05555-97e8-e794-e009-04721c586db9` after a fresh
  4,096s heartbeat. Normal scheduler reclamation restarted that exact id at
  02:15:01Z; by 02:39 it had advanced another 1,458s while the completion gap
  crossed 98 minutes. This is not a stuck lease: the same-id heartbeat proves
  recovery. It is lost in-process scan progress, because the full-fleet export
  still restarts from its scheduler boundary. `selection-freshness` now
  reads the bounded task lifecycle window (with host-journal fallback), emits
  task id/duration/executor with the gap, and tells the operator not to restart
  or duplicate a live rebuild. Retain the streaming bounded exporter wherever
  version/code evidence proves it present and deploy it only on older
  generations, then require the exact task to finish and two following runs to
  remain fresh; correlate the interrupted predecessor through §2.13 rather
  than treating a later retry as evidence that it completed.

  The rebooted edge-0/g1 attempt supplied the post-rollout discriminator.
  Fleet version `2026.8.30+1033129380` contains
  `runClientScoreExportStream`, its 512-item/8MiB limits, lazy sample encoding,
  cleared batch slots, and deterministic tests. The same-id task restarted at
  02:15:01Z and retained fresh heartbeats through at least 03:06:31Z
  (`active(3089.61s)`). During that run its allocated heap stayed near 3.1GiB
  and RSS near 3.6GiB instead of the pre-fix 28–45GiB peaks, while allocation
  rate could still approach 0.8GiB/s. That validates the bounded live heap and
  proves the exporter is deployed; prescribing its rollout again is stale.
  Wall-clock duration can remain high because the task still loads the full
  location/group maps and repeats caller-location fan-out before one durable
  task boundary. Let the active attempt finish. If two uninterrupted runs
  exceed the 60-minute freshness band despite bounded heap, profile and
  checkpoint those remaining phases rather than restarting the worker or
  redeploying the already-present writer.

  That attempt completed at 03:06:35Z after roughly 3,094 seconds, inside the
  60-minute freshness band. At 03:27Z the completion gap was 1,262 seconds and
  the next scheduled score task carried a four-second-old fresh claim with no
  reschedule error. This closes the interrupted attempt itself and validates
  the bounded exporter under the production fleet. Keep the two-following-run
  freshness verification open; profile the remaining maps/fan-out only if an
  uninterrupted run now exceeds 60 minutes.

  The next uninterrupted run isolated that remaining fan-out before reaching
  the freshness cliff. Task `01a055c8-759e-406e-4061-603f0dc86869` began on
  edge-3/g2 at 03:07:07Z and was still active after 2,875 seconds at 03:55Z.
  The deployed streaming fix kept allocated heap near 3.7GiB and RSS near
  4.1GiB, but the process stayed at its four-core quota and allocated about
  653,329,681 bytes/s (623MiB/s). Its progress denominator was 1,008: two
  ForceMinimum modes times two rank modes times 252 caller locations. For each
  caller, the exporter re-encoded every target even though a caller's blocked
  networks alter only targets that actually contain one of those networks.
  The live database had 252 caller locations and only 2,766 block rows across
  138 of them; 114 callers had no exclusions at all.

  The source fix now partitions work by target and encodes one baseline target
  payload. Its first compatibility pass fans the same immutable bytes out
  under legacy unchanged-caller keys; after publishing a ready marker, later
  passes replace those duplicate values with one-byte baseline aliases. Only a
  caller that genuinely removes a provider receives a separately filtered
  encoding and caller alias. Sharing a sample's shuffled byte payload is safe
  because readers randomize the sample-key order and `FindProviders2` performs
  the final weighted selection. Missing aliases retain legacy-reader behavior,
  and old payloads expire through the ordinary five-hour TTL. The bounded
  512-command/8MiB stream remains in place. Deterministic regressions prove
  equivalent callers share one two-provider encoding, affected callers retain
  their one-provider override, the compatibility pass retains old-reader
  values, and the sparse pass omits duplicate payloads. Deploy this
  target-fanout/alias change after the schema-head migration, then verify a
  complete run with §2.12a and byte recovery with §3.3b rather than treating a
  bounded heap alone as success.

### 2.9 Provider-selection population — the fresh-but-empty cache canary
Probe: `selection-population`

Freshness is necessary but NOT sufficient. `UpdateClientScores` can complete
successfully, refresh every ttl, and publish an empty provider market. Check the
database supply and the exported cache as separate stages:
```sql
SELECT
  (SELECT count(DISTINCT ncc.client_id)
   FROM network_client_connection ncc
   WHERE ncc.connected AND EXISTS (
     SELECT 1 FROM provide_key pk
     WHERE pk.client_id = ncc.client_id AND pk.provide_mode = 3
   )) AS connected_public_clients,
  (SELECT count(DISTINCT nclr.client_id)
   FROM network_client_location_reliability nclr
   WHERE nclr.connected AND nclr.valid AND EXISTS (
     SELECT 1 FROM provide_key pk
     WHERE pk.client_id = nclr.client_id AND pk.provide_mode = 3
   )) AS eligible_public_providers,
  (SELECT count(*) FROM provider_egress_health) AS egress_health_rows,
  (SELECT count(*) FROM provider_egress_health
   WHERE measured_at >= now() - interval '24 hours'
     AND total_count > 0 AND 10 * ok_count >= 9 * total_count
  ) AS fresh_passing_health_rows,
  (SELECT count(*) FROM provider_egress_location) AS egress_location_rows;
```
Then inspect the SAME target/caller pair in the normal and ForceMinimum score
caches. Resolve a country target first; `00000000-0000-0000-0000-000000000000`
is the no-caller-location key:
```sql
SELECT location_id FROM location
WHERE location_type = 'country' AND country_code = 'us';
```
```bash
target=<uuid-from-sql>
caller=00000000-0000-0000-0000-000000000000
normal="{cs_0_q_${caller}_${target}}c_l"
forced="{cs_1_q_${caller}_${target}}c_l"
redis-cli -c -p <entry-port> TTL "$normal"
redis-cli -c -p <entry-port> STRLEN "$normal"
redis-cli -c -p <entry-port> TTL "$forced"
redis-cli -c -p <entry-port> STRLEN "$forced"
```
`c_l` is Go-gob `[]int`: decode it and SUM the entries for the exact exported
provider count. Do not use EXISTS/TTL as a population check — an encoded empty
slice is a present, fresh key. `STRLEN` is a fast discriminator before decoding:
on the 2026-08-17 build an empty slice was 18 bytes; the matching 74,602-provider
ForceMinimum value was 1,142 bytes. Compare the pair rather than making 18 a
permanent encoding contract. Repeat with rank `s` and caller=`target` if the
incident is caller/rank-specific.

- HEALTHY: normal decoded sum is nonzero and tracks the eligible supply band;
  ForceMinimum is normally larger because it bypasses reliability/score gates.
- GATE WIPE: connected/eligible are large, normal sum is 0, ForceMinimum is
  large. Provider connectivity is healthy; a minimum predicate ate the market.
  Split the predicates: reliability lookbacks, score cutoff, then egress health.
- EGRESS-CONFIG DISCRIMINATOR: locate `provider.yml` in the DEPLOYED taskworker
  with `find /srv/warp/config -maxdepth 2 -type f -name provider.yml -print`
  (normally `/srv/warp/config/<version>/provider.yml`) and inspect the newest
  resolver-visible file. Missing or `enable_egress_test: false` means egress
  health/location evidence MUST NOT gate the export. When true, compare the two
  egress table counts above; zero rows means the pipeline is uninitialized, not
  that every connected provider independently failed a test.
- UPSTREAM EMPTY: normal and ForceMinimum both decode to 0. Investigate the
  connected/valid/provide-mode pool and target location before the minimums.

2026-08-17 signature: contracts fell from ~8k/min to tens/min while new connects,
pg, redis, and score-task freshness stayed healthy. Pg still held 100,442
connected Public clients and 88,903 eligible Public providers. Fresh normal
quality+speed caches decoded to 0, ForceMinimum held ~74.6k, and BOTH egress
probe tables held 0 rows. The score writer had activated a fail-closed egress
gate before the probe pipeline was populated. This signal, not 2.8 alone,
localized the outage.

### 2.10 Payment-completion retention fan-out — low concurrency, huge writes
Probe: `retention-fanout`

`CompletePayment` stamps `transfer_contract.reap_time` for every contract in a
payment's `transfer_escrow_sweep`. The lookup is indexed by `payment_id`, so an
indexed plan can still be catastrophically expensive when one payment owns
millions of contracts. Track the exact statement separately from the generic
top-total-time list:
```sql
SELECT queryid, calls,
       round(mean_exec_time::numeric, 1) AS mean_ms,
       round(max_exec_time::numeric, 1) AS max_ms,
       round(rows::numeric / NULLIF(calls, 0), 0) AS rows_per_call,
       shared_blks_hit, shared_blks_read, shared_blks_dirtied,
       shared_blks_written
FROM pg_stat_statements
WHERE queryid = -3312164664690273449;

SELECT count(*) AS active,
       max(clock_timestamp() - query_start) AS oldest
FROM pg_stat_activity
WHERE state = 'active' AND query_id = -3312164664690273449;
```
2026-08-27 signature since the pgss reset: 18 calls, 338,497ms mean,
544,554ms max, 2,209,550 updated rows/call, 1.48B shared hits, and 54.7M
dirtied blocks. One to three copies remained active for tens of seconds to
minutes while the storage device ran in high-utilization write bursts.

2026-08-30 live monitor confirmation: query id `-3312164664690273449` had
reached 36 calls, 326,707ms mean, 544,554ms max, and 2,156,191 rows/call; one
sample saw three active copies with the oldest at 72s and the next saw one at
105s. The current source no longer issues that synchronous statement:
`CompletePayment` sets the durable `contract_retention_pending` queue bit, and
`RemoveCompletedContracts` advances `contract_retention_cursor` in bounded,
committed batches. Seeing the legacy query id after that source change therefore
identifies an older deployed generation; deploy the fixed generation and then
require the exact query id to disappear rather than tuning PostgreSQL around it.
The same live window supplied a cross-signal check: a 1,548s
`CloseExpiredContracts` run accumulated 244k open contracts while this query
remained active, then two comparable 100k close cohorts finished in 22–23s
immediately after the retention executions cleared (§2.6). This is the
retention fan-out delaying unrelated writers, not evidence that the closer
needs a larger pool.
It also pushed `transfer_contract` above the 10M dead-tuple warning while
autovacuum was active; vacuum brought the estimate back below threshold, but
cannot make an unbounded multi-million-row update cheap. Treat §2.4 bloat as a
downstream symptom until the legacy query id disappears.

A later read-only sample separated the payment backlog into causes instead of
letting its largest class hide the retention failures. Of 384 failing
`AdvancePayment` rows, 368 carried the known unfunded-wallet error, while ten
different rows retained `Interrupted: failed to deallocate cached
statement(s): conn closed` after their exact 120-second task boundary. Those
ten rows spanned two payment plans and nine networks; each payment referenced
1,184,684–3,356,615 sweep rows, or 21,875,448 in total. All ten had a processor
record and idempotency key but no receipt/hash or terminal state. One selected
retry supplied direct causal proof: taskworker began it at 13:49:03Z while
PostgreSQL began query id `-3312164664690273449` for its 3,356,615-row payment.
The same backend remained in that exact `UPDATE` from 0.5s through 118.5s,
alternating ordinary execution with WAL/data-file waits and no blocker; at
13:51:03Z taskworker reported `eval error(120.00s)` with the connection-cleanup
text. The cleanup message is cancellation aftermath, not the slow operation.
The probe therefore retains this durable correlation between retries when both
the legacy statement averages at least 100,000 rows/call and an
`AdvancePayment` row carries that deadline signature. The error alone is not
enough to attribute retention.

- WARN: one execution active >30s, or >=2 concurrent executions for two
  samples; also warn between retries when the exact legacy query has at least
  100,000 rows/call and an `AdvancePayment` row retains the 120-second cleanup
  signature. Include rows/call and storage/WAL signals; backend count alone
  understates this load.
- This is not the rare-value planner landmine (§2.3):
  `transfer_escrow_sweep_payment_id` exists and is used to find the fan-out.
  `ANALYZE` and larger PgBouncer pools do not reduce millions of updates.
- Action: make retention stamping bounded/chunked or decouple it from the
  synchronous payment transaction. During an incident, correlate it with
  §2.6 growth and §2.11 pool-path timeouts; do not kill a payment transaction
  without first proving retry/idempotency safety.

The 16:19–16:53 recurrence preserved the same durable writer signature while
close, reliability, and vacuum work overlapped. Query id
`-3312164664690273449` advanced from 39 to 40 calls and from 2,156,465 to
2,162,072 rows/call; lifetime mean/max execution reached 324,803/544,554ms,
with 3.234B shared-buffer hits, 30.93M reads, 115.45M dirtied blocks, and only
164,194 written blocks. One to three copies were transiently active and one
exceeded 410s. Ten durable `AdvancePayment` rows still carried the exact
120-second connection-cleanup deadline signature. This is continued evidence
of an unbounded row writer and delayed cancellation cleanup, not a reason to
expand connection pools; keep the bounded retention implementation and use
the retained task errors only as attribution between active executions.

The next execution brought the lifetime total to 41 calls and then cleared
from `pg_stat_activity`. At 18:02Z the statement averaged 2,170,158 rows and
325,648ms per call, with a 544,554ms maximum, 3,329,316,398 shared-buffer hits,
31,981,964 reads, 118,226,752 dirtied blocks, and only 173,741 written blocks.
All ten cleanup-deadline task rows remained durable. The concurrent
`transfer_contract` vacuum completed and cut its dead-tuple estimate below one
million, but that cleanup result does not make the 2.17M-row payment
transaction bounded. Require the query id and deadline-error cohort to
disappear after the queued cursor path is deployed.

### 2.11 PgBouncer client-write stall — pool path vs postgres load
Probe: `pgbouncer-stalls`

The application-side error
`pgproto3.writeError=write failed: write tcp <app>:<ephemeral>-><pg>:6432: i/o timeout`
is distinct from PgBouncer's `FATAL: query_wait_timeout`. The client could not
write the request to the 6432 frontend before its socket deadline; the query
may never have reached a postgres backend. A healthy direct-5432 snapshot can
therefore coexist with real route failures.

Diagnosis order:
1. Sample `pg_stat_activity` through direct 5432. Low active count/no blockers
   rules out a postgres CPU or lock wall but does NOT clear the pool path.
2. On the db host, remember 6432 is nginx in front of the 32 PgBouncer shards
   (6433-6464). Check `ss` listen/accept queues, nginx errors, every shard unit,
   and `SHOW POOLS`/`SHOW STATS` on the shards rather than the intentionally
   unused default `pgbouncer.service`.
3. Group the timeout logs by route and app host. On 2026-08-27 they clustered
   on `/stats/providers` plus one connect announce while direct pg remained
   lightly active. The provider route compounds the pool pressure with query
   ids `8120731601370473026` (hundreds of thousands of provider ids returned
   per call) and `6264993620546911677` (~3.3s lifetime mean aggregate).
4. A route-specific cluster means bound/cache/page that route's database work;
   a fleet-wide cluster with full shard queues means pool-path saturation.
   Raising the socket timeout only hides either failure and retains scarce
   connections longer.

### 2.12 Taskworker allocated-heap skew — the local executor discriminator
Probe: `worker-memory`

Query the pushed Go runtime metrics through any services host's loopback Mimir
front and compare fresh taskworker processes, keeping host, block, and runtime
instance identity:

```promql
go_memstats_heap_alloc_bytes{env="main",job="taskworker"}
```

- HEALTHY: allocated heap stays below 8GiB or within 4× of the fresh fleet median.
  Require both limits to fail for two consecutive one-minute samples. With
  fewer than three fresh instances, use a conservative 16GiB absolute guard.
- BROKEN: the process exceeds both guards. Include
  `go_memstats_heap_objects`, `process_resident_memory_bytes`, cumulative
  allocations, GC cycles, process age, exact host/block/instance, and
  five-minute CPU-core, allocation-byte, GC-cycle, and GC-pause rates plus
  fleet medians. Join the newest active task IDs/durations from that
  host/block's taskworker heartbeats. Ignore a sample older than 90s so a
  drained generation cannot create a false skew. The rate query is
  best-effort: an unavailable range evaluation must not hide a decisive heap
  outlier, but its absence is recorded in the evidence.
- This is process-local evidence. `HeapAlloc` includes reachable objects and
  objects not yet reclaimed by the next GC; unlike RSS, it excludes retained
  pages with no allocated objects. Host free memory, CPU count, cgroup limits,
  and pressure remain necessary controls, but they do not clear a worker whose
  allocated heap is hundreds of times its peers. Correlate exact
  taskworker `eval active` task IDs on that executor, then compare those task
  families on other workers. Co-residency proves local contention; it does not
  assign every byte to one task.
- Query the loopback Mimir API through service hosts in inventory order rather
  than treating the first host as a unique metrics authority. A transport
  failure falls through to the next gateway, and the gateway that served the
  primary heap query is tried first for its related five-minute rate query.
  Emit `monitor/visibility` only if every service gateway fails. At
  2026-08-30 21:57Z, edge-0's local query timed out after 15s while edge-1,
  edge-3, and edge-4 each returned a valid Mimir API response immediately;
  that is a gateway-path failure, not evidence that the replicated metrics
  backend is unavailable. The synthetic regression forces the first gateway
  to time out, proves the second preserves the heap finding, and proves the
  rate query stays on the successful gateway instead of repeating the known
  timeout.
- Action: bound or stream the implicated working sets; retain task deadlines.
  Do not restart merely to erase the evidence, and do not raise deadlines to
  normalize allocator/GC contention.

On 2026-08-30, edge-3/g2 container `4cf91fd25a2e` supplied the production
shape while it carried the long client reaper, score rebuild, close checkpoint,
and net-escrow pass. Its pushed runtime metrics showed a 29.62GiB allocated
heap, 27.25M allocated objects, a 32.13GiB RSS, and 7.63TiB cumulatively
allocated after about 15 hours. The allocated heap had repeatedly cycled
between 6.46GiB and 31.43GiB; edge-3/g1 was 0.145GiB and the fresh taskworker fleet median was
about 0.14GiB. The host still had more than 800GiB free and the container had
no CPU throttle, OOM event, or pressure. That rules out host/cgroup starvation
and explains the A/B/A task-duration split as local allocator/GC contention.
The oscillation predated the current reaper, so retain the bounded fixes for
every co-resident large path rather than assigning the whole heap to that one
task. Source inspection then found the matching score-export allocator: all 48
exporters encoded and retained a whole caller-location operation list before
the first bounded Redis write. §2.8 now streams and clears each 512-item/8MiB
batch and lazily encodes one provider sample at a time; its deterministic
10,000-value test caps producer lead at 511, a separate regression proves
every flushed backing-array slot releases its payload, and the retry tests preserve
failed-batch-only replay.

The task boundaries supplied a second discriminator. Edge-3's score task
`01a0530b-0e6a-9c14-6694-11a165f3c27b` completed at 15:30:03Z in
4,143.294s, but its allocated heap stayed near 25GiB until a later GC; the
co-resident reaper completed at 15:32:16Z in 7,932.960s in the same scrape
where heap and RSS contracted. That coincidence does not assign those bytes
to the reaper: `HeapAlloc` can retain unreachable objects between collections.
The independent reproduction did assign the allocator. Edge-1/g2 began score
task `01a0534a-c1c9-fb11-37e6-98740046f7eb` at 15:30:34Z: allocated heap rose
from 0.91GiB at 15:30:30Z to 5.55GiB at 15:31:00Z and 8.74GiB at 15:31:30Z,
before its later close and reliability-rollup tasks began, then reached
13.94GiB at 15:32:00Z. That clean start-aligned reproduction validates the
score-export working-set fix independently of the edge-3 task mixture. A
simultaneous negative control separated the reaper: task
`01a0534c-cb89-633c-78b2-417bb4ea3717` ran past 460s on edge-4/g1 while that
process stayed between 0.07GiB and 0.16GiB allocated heap and near 0.50GiB RSS.
The reaper's old serialized Redis path explains its wall-clock tail, but not
the multi-GiB allocation pattern. The probe now performs this correlation in
its alert by joining the outlier's exact host/block to recent taskworker
heartbeats and listing active task IDs plus elapsed times. A timestamped
terminal line supersedes an older active line; successful `eval done` is V(1)
and absent from the production stream, so an otherwise unterminated active
line also expires after 45 seconds (four missed 10–12-second heartbeats).
Synthetic lifecycle coverage proves both paths and rejects a heartbeat more
than 30 seconds in the future, so a completed task cannot remain attached to
later heap samples merely because its last informational line was retained.

The five-minute rates made the independent reproduction quantitative at
15:51Z. Edge-1/g2 consumed 4.01 CPU cores and allocated approximately
910–916MB/s while the score export was active; every other taskworker was at
or below 0.184 cores and 7.84MB/s. Its GC counter advanced at about 0.125/s
(one cycle per eight seconds), while the stop-the-world pause sum grew by only
about 0.000041 seconds per second. This is sustained encoding/allocation work,
not an executor paused by GC or a host starved of CPU. The alert now carries
these rates and their fleet-median ratios so a post-completion heap awaiting a
collection is distinguishable from an actively allocating task.

The reproduction then reached an authoritative terminal boundary. Task
`01a0534a-c1c9-fb11-37e6-98740046f7eb` completed at 16:18:55Z in
2,901.257s; Mimir's 60-minute maximum allocated heap for that exact instance
was 23.24GiB. Heap was still 20.98GiB at 16:18:47Z, then collapsed to 0.28GiB
by 16:19:19Z and 0.20–0.25GiB thereafter. The ordinary two-minute log query
continued returning the last `eval active(2890.84s)` line well after the
finished row existed because successful `eval done` was not ingested. That is
the live production discriminator for the 45-second heartbeat freshness rule:
raw log retention is not task liveness. It also completes the clean score-only
allocator reproduction; the streaming/bounded writer remains a source fix and
must still be verified after rollout by bounded heap and producer lead while
the task runs. Encoding may continue to use CPU and allocate bytes; those
rates alone are not a regression if live heap no longer grows by tens of GiB.

The next scheduled score export reproduced the allocator on a second executor
without a cold-start ambiguity. Task
`01a05377-7f00-72a0-c8a0-6de9c75d6147` began on edge-1/g1 container
`53ef545dc646` at 16:19:26Z. Its heap repeatedly sawtoothed from roughly
9.39GiB to 27.72GiB while five-minute allocation stayed near 798–850MiB/s and
CPU near 4.00 cores. GC pause accumulation was only
0.000024–0.000034 seconds/second, so the worker was doing sustained encoding
and allocation rather than stopping in GC. At 16:56Z its claim and
10–12-second heartbeats were still fresh beyond 2,200s, alongside a long
client reaper. This independent recurrence strengthens the streaming
512-command/8MiB, lazy-encode, cleared-slot fix.

The terminal boundary closed that proof. The task completed at
17:17:13.039655Z in 3,466.538s. Mimir's 70-minute maximum for the exact
instance was 30,436,930,688 bytes (28.35GiB); its heap was still 20.78GB at
17:17:10Z and had collapsed to 252.9MB by 17:17:25Z. The colocated close and
reaper then completed 19 and 33 seconds after score, respectively. At
17:17:44Z the next hourly score task
`01a053ac-ddd5-a88a-f7f3-88e1f63d11d6` began on edge-0/g1 and reached
19.33GiB allocated heap after only 135s, later allocating roughly
760–915MiB/s. Its new close and reaper siblings immediately developed the same
slow tail while edge-1/g1 stayed near 0.16GiB. That start/stop/cross-host
sequence identifies the deployed score exporter as the portable allocator and
local amplifier; it does not erase the independent need to bound the sibling
paths. By 1,324s the edge-0 process reached 44.01GiB allocated heap and about
49.7GiB RSS while still allocating roughly 910MiB/s. The host retained 843GB
free/890GB available with no swap use; the cgroup had no memory limit, zero
high/max/OOM events, zero memory pressure, and zero CPU throttling. This is
application allocation, not host or container starvation.

That pre-rollout task completed at 18:20:49.147041Z in 3,784.989s. Mimir's
90-minute maximum for the exact instance was 48,189,121,640 bytes
(44.88GiB), while five-minute allocation remained near 0.9GiB/s through the
tail. The terminal duration and instance-local peak are the baseline for the
streaming exporter: after rollout, a successful task alone is insufficient;
live heap must remain bounded throughout the export while all 1,008 locations
complete.

The 2026-08-31 05:37Z watch exposed a correlation-path regression after the
streaming exporter rollout. Edge-3/g1 held 9.61GiB of allocated heap, 91.0×
the 0.11GiB fleet median, while consuming about four cores and allocating
642MiB/s. The sibling §2.12a probe used the bounded host-journal fallback and
attached fresh `UpdateClientScores` and `CloseExpiredContracts` heartbeats,
but this heap alert called `warpctl logs` directly, waited for its full
60-second command timeout, and then omitted `active_tasks` even though its
evidence promised that join. The heap measurement remained valid; the missing
task attribution was a monitor transport bug. `worker-memory` now uses the
shared 12-second gateway boundary plus bounded taskworker-journal fallback,
records `active_log_source`, and retains partial-read degradation separately
from a complete fallback. When the exact executor carries `UpdateClientScores`,
the alert names the already-proven caller-fanout allocator and the pending
target-oriented/alias fix; a co-resident close task adds the process-budget
impact and close/backlog verification. A deterministic synthetic regression
forces the gateway failure, normalizes a timestamped journal heartbeat, and
requires both the exact task id and `host-journal-fallback` source in Markdown.
The first v114 live result attached four fresh task IDs, including the
4,357-second score export and 747-second close checkpoint, but also labeled
`crisp` and `fireside` as partial-read failures. Direct checks showed zero
running taskworker units on both hosts and an exact `journalctl --grep` status
of 1, while an active edge returned status 0. Status 1 is journald's normal
no-match result, not a transport failure. The bounded command now converts
only that status to an empty successful host observation and preserves every
other nonzero status as an error. A mixed active/empty-host synthetic test
locks the shell-status guard and requires the active host's normalized task to
survive without false degradation.

### 2.12a Taskworker CPU/allocation churn — the bounded-heap blind spot
Probe: `worker-churn`

Query paired one-minute process rates through a services host's loopback
Mimir front, retaining exact host, block, and runtime instance identity:

```promql
label_replace(
  rate(process_cpu_seconds_total{env="main",job="taskworker"}[1m]),
  "monitor_rate", "cpu", "job", ".*"
)
or
label_replace(
  rate(go_memstats_alloc_bytes_total{env="main",job="taskworker"}[1m]),
  "monitor_rate", "alloc", "job", ".*"
)
```

- HEALTHY: a worker is below 3.8 CPU cores, below 256MiB/s allocation, or
  within 8× of either fresh fleet median.
- BROKEN: the same fresh process exceeds all four guards for two consecutive
  one-minute probes. Ignore samples older than 90 seconds. CPU saturation by
  itself can be useful work and a short allocation burst is not a leak; the
  absolute-plus-fleet conjunction identifies exceptional object churn.
- Join recent `eval active` heartbeats on exact host/block and include task id,
  duration, runtime instance, fleet sample count, both rates, medians, and
  ratios. A missing heartbeat prevents task attribution but does not clear the
  process-rate finding. A large quiescent heap belongs to §2.12; this signal
  catches sustained encoding/allocation even after streaming bounds live heap.
- ACTION: remove repeated encoding, copying, or materialization in the exact
  active task family. Preserve bounded writers and task deadlines. Do not
  raise the CPU quota or restart the worker merely to make the evidence vanish.

The production discriminator appeared on edge-3/g2 after the bounded score
writer was deployed. From about 03:07Z through at least 04:19Z on 2026-08-31,
that process repeatedly consumed about four cores and allocated 623–640MiB/s,
while peers used roughly 0.005–0.11 cores and at most 13.4MB/s. The live
one-minute alert at 04:19Z measured 3.996 cores and 639.51MiB/s, 363.6× and
6,421.9× its fleet medians. Heap stayed near 3.7GiB and RSS near 4.1GiB, so
§2.12 correctly no longer fired even though the process was still doing
pathological work. The exact task beginning at the rate transition was
`UpdateClientScores` id `01a055c8-759e-406e-4061-603f0dc86869`; it remained
active beyond 4,322 seconds. PostgreSQL statements sampled during colocated
close work were fast and had no lock waits, separating database blocking from
application CPU starvation.

Source and live-data inspection identified the multiplier. The old loop was
two ForceMinimum modes × two rank modes × 252 caller locations, and for every
caller it gob-encoded every location and location-group target. A blocked
network changes only a target containing a provider from that network. There
were 2,766 block rows across 138 caller locations, while 114 callers had no
blocks; most caller/target pairs therefore repeated identical encoding. The
root fix partitions by target and encodes its baseline once. The rolling
compatibility pass refreshes legacy unchanged-caller payloads; subsequent
passes store only one-byte baseline aliases. Only callers whose blocked network
intersects that target get independently filtered encodings and caller aliases.
It keeps the 512-command/8MiB streaming budget, treats missing aliases as legacy
payloads, and preserves final provider-selection semantics while §3.3b verifies
the duplicate bytes expire.

The synthetic source regression supplies four fresh workers, makes one consume
4.003 cores and 650MiB/s, and proves the structured alert identifies its exact
executor and active score task with both fleet ratios. Negative cases prove a
CPU-only worker, an allocation-only worker, and a stale former generation do
not alert. The model regression independently proves equivalent callers share
one encoding while a genuinely affected caller gets a filtered encoding.

The first live probe run also exposed a chronology failure in degraded task
attribution. Loki's 502 path delayed `warpctl logs` for more than a minute
before the host-journal fallback returned. The probe had captured its clock
before that wait, so current journal heartbeats appeared more than 30 seconds
in the future and the alert omitted the still-active score task. Task-lifecycle
reads now bound the fleet gateway to 12 seconds and compare fallback lines with
a clock refreshed after collection; §2.12 uses the same corrected chronology.
A deterministic delayed-collection test advances the clock by 65 seconds and
proves the fresh score heartbeat and target-specific remediation survive, and
a separate context-aware source proves gateway timeout falls through to the
bounded journal read.

Implementation convention: SIGNALS.md §2.12a (`worker-churn`) maps to
`signal_worker_churn.go` and `signal_worker_churn_test.go`.

### 2.13 Maintenance reboot collision — process exit is not task completion
Probe: `reboot-collision`

An orderly host reboot can still interrupt scheduler-level work. For 20 minutes
after each services-host boot, read the previous boot's final journal boundary,
the `by-restart.service` evidence, and bounded g1/g2 taskworker lifecycle logs.
Treat a task as colliding only when its newest `eval active` heartbeat was
within 45 seconds of shutdown, had already reached 120 seconds, and no newer
terminal line exists. Cross-check its exact task id against `finished_task`
through the previous-boot boundary so a task that completed between its last
informational heartbeat and shutdown is not mislabeled.

- HEALTHY: no task above 120 seconds is still active at a boot boundary, or its
  exact attempt reached `finished_task` before shutdown.
- WARN: one or more fresh non-terminal tasks above 120 seconds cross the boot
  boundary. Include boot/end timestamps, reboot source, task name/id/duration,
  and host/generation/container.
- ACTION: deploy bounded/checkpointed task implementations and coordinate a
  taskworker drain with future maintenance windows. Do not disable the fleet
  reboot policy ad hoc, delete pending task rows, or raise deadlines to hide
  the collision. Verify scheduler reclamation and the affected backlog.

Production supplied the discriminator on edge-0 at 2026-08-31 02:09:48Z.
`by-restart.timer` started its scheduled reboot after the configured Monday
01:00Z window plus randomized delay. At the previous-boot boundary,
`CloseExpiredContracts` task `01a0558c-3405-5671-1f3d-7429f4dd08f7` still had
a fresh 541-second heartbeat and `UpdateClientScores` task
`01a05555-97e8-e794-e009-04721c586db9` had run for roughly 59 minutes. The host
shut down cleanly, came back on the same old binary, and neither attempt had a
terminal line. That is a maintenance collision, not an OOM or crash. The
existing 25,000-contract checkpoint and streaming score exporter remain the
root fixes; a later retry is recovery, not evidence that the interrupted
attempt completed.

Implementation convention: SIGNALS.md §2.13 (`reboot-collision`) maps to
`signal_reboot_collision.go` and `signal_reboot_collision_test.go`. The
synthetic scheduled-reboot case includes two long active heartbeats and makes
one exact id complete before the previous-boot boundary; only the genuinely
interrupted task may alert.

---

## 3. redis signal catalog

### 3.1 Per-node memory table (the skew detector)
Probe: `redis-memory`

For each master: `INFO memory` → used_memory, maxmemory, pct.
- HEALTHY: all nodes within ~2× of each other (fleet baseline was 3–8G).
- BROKEN: any node > 85% of maxmemory (warn) / > 92% (page); any node > 3×
  the fleet median (skew — either a hot key family or un-drained piles).
- volatile-ttl POLICY IMPLICATION: eviction can only touch TTL'd keys. A node
  full of no-TTL keys at maxmemory rejects ALL writes (`OOM command not
  allowed`) while reads keep working and cluster_state stays ok — invisible
  to naive health checks, devastating to write paths. Monitor writes-error
  class per node (or canary tasks, 1.2) to catch it.
- 2026-08-29 discriminator: node 6406 held 10.60G of dataset at 86% of its
  12.88G ceiling while client buffers were negligible. Slot key counts were
  uniform; a bounded memory sample attributed its extra bytes to expiring
  `{cs_1_q_*}s_l_N` provider-selection samples (typical sampled TTL ~14,000s),
  not a no-TTL leak or stale out-of-slot keys. Trend it through the five-hour
  score-cache cycle; the first sustained high-memory tick now attaches the
  dataset/client/accept-queue battery using `mem_clients_normal` plus
  `mem_clients_slaves`.

### 3.2 Memory attribution: dataset vs client buffers
Probe: `redis-buffers`

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
Probe: `key-families`

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

### 3.3a Impossible TTL residue (writer fixed, stale keys remain)
Probe: `ttl-leaks`

`[redis][ttl]` catches a new suspect write, but it becomes quiet after the
writer is fixed even though old keys retain their original expiry. Read each
node's `INFO keyspace` `avg_ttl` as the persistent-state counterpart. Annual
net-escrow balances legitimately approach 395 days, so the probe uses a
conservative two-year average threshold.

On 2026-08-29/30, 30 of 32 nodes had an impossible fleet-wide average TTL;
the four nodes already at 85–89% memory each held roughly 6.45–6.57M keys,
4.82–4.94M with expiry, and reported average TTLs from 5.09e14 to 9.08e14ms.
The probe emits one fleet incident rather than one markdown alert per affected
port. A bounded Redis-side, binary-safe sample on the worst-average node found
99 over-limit keys among 4,914 examined: 47 legacy `s_sk_cs` contract sets and
52 legacy `s_sk_sid` stream-id keys, with zero current-generation or unknown
suspects. The maximum PTTL was 28,799,985,310,940,036ms: the exact
pre-2026-07-20 fingerprint where an 8h Go `time.Duration` was serialized as
nanoseconds and Lua `EXPIRE` consumed that integer as seconds (about 913,000
years). Fragmentation was only 1.02 and client buffers below 1MiB, proving the
high-memory class was dataset residue. The probe repeats this bounded
classification through `EVAL_RO` on the node with the largest average, so a
future unrelated TTL family receives attribution rather than stream-specific
cleanup advice.

By 2026-08-31T00:46Z the residue had exhausted the former headroom: seven
nodes were at 94–100% of their 12GiB maxmemory, with port 6406 at 12.00GiB and
`volatile-ttl`. A 12-second read-only counter sample on 6406 observed 5,429
evictions and 533 ordinary expirations while `total_error_replies` and every
error class stayed flat; `current_eviction_exceeded_time` was zero at both
ends. Writes were therefore still surviving through roughly 452 evictions/s,
not currently returning OOM, but this is emergency churn rather than healthy
headroom. The memory alert now carries policy, total/expiring/non-expiring key
counts, average TTL, cumulative evictions, current exceeded time, and OOM/error
counters. When its average TTL is impossible, it points to the independently
attributed TTL cleanup with explicit maintenance authority; it does not
recommend a blind maxmemory increase or arbitrary key deletion.

The 2026-08-31T01:06Z host-capacity battery ruled out a safe ceiling increase.
The focused samples show the deficit being consumed. At 02:31Z Redis held
355.8GiB used / 366.8GiB RSS; by 02:46Z that had risen to 362.2GiB used /
372.3GiB RSS beneath 384GiB of aggregate maxmemory. The fleet had 184.6M keys
and 22 of 32 nodes above 92%. The 472.2GiB host had 16.3GiB available while
Redis could still consume 21.8GiB before reaching its per-process ceilings—a
definite 5.6GiB physical capacity deficit even before additional RSS overhead,
the kernel, or other processes. Raising maxmemory would exchange controlled
per-node eviction for host swapping or OOM risk. A subsequent 12-second sample
on four 100%-class nodes kept total error replies and
`current_eviction_exceeded_time` flat, but ports 6388, 6397, and 6406 evicted
about 38, 104, and 138 keys/second. Writes are still surviving through
emergency churn; that is not healthy headroom.

The monitor emits `redis-host-capacity` when a critical node coincides with
this aggregate deficit. The required capacity is remaining aggregate
`maxmemory` growth plus an operational reserve equal to the larger of 8GiB or
2% of physical RAM. Including the reserve is important at the limiting case:
remaining growth reaches zero when every node reaches its ceiling, but a host
with no RSS/kernel/service reserve is still in danger and the page must not
disappear. Immediate capacity headroom requires more physical memory, Redis
masters on additional hosts, or an immediately smaller retained footprint.
The independently attributed, binary-safe stream cleanup
(`bringyourctl streams expire-leaked-ttls`) still requires explicit
maintenance authority and is complementary: it clamps keys to an 8-hour TTL
and begins a bounded drain, but cannot free all of that capacity immediately.

The first corrected live sample at 2026-08-31T03:04Z caught the exact limiting
case. Redis held 368.5GiB used / 377.6GiB RSS, 27 nodes were above 92%, and the
host had 15.8GiB available. The old comparison would have cleared because its
15.5GiB remaining configured growth was just below `MemAvailable`; including
the 9.4GiB reserve correctly retained a 9.2GiB capacity-deficit page.

A byte-sized TTL sample at 2026-08-31T04:27Z separated correctness from
capacity: 98 impossible-TTL keys among 4,975 examined occupied only 12,694
bytes in total, with the largest key at 185 bytes. The duration-as-seconds
residue is real and should be repaired, but it cannot explain roughly 372GiB
of Redis data. The probe now returns aggregate `MEMORY USAGE SAMPLES 1` for
suspect keys and explicitly sends capacity attribution to §3.3b; an
impossible average TTL alone no longer labels the residue as the memory root
cause.

- Do not inspect binary stream keys through shell variables; embedded bytes
  can truncate or corrupt family attribution. The existing
  `ExpireLeakedStreamKeys` scanner is binary-safe and pipelines PTTL on each
  shard. Its first implementation scanned only the newer `*s2_sk_*` names and
  therefore missed the production residue, which predates that namespace and
  uses `*s_sk_*`. The corrected scanner covers both generations and validates
  the exact suffix before changing a key.
- Classify every new `redis-ttl-suspect` line by command and redacted key
  family. Require zero new stream-family warnings before cleanup; a warning on
  another family is an independent writer defect, not proof that stream writes
  resumed. Running `bringyourctl streams expire-leaked-ttls` changes production
  TTLs and therefore requires explicit maintenance authority. It clamps only
  legacy/current stream-id and stream-contract keys beyond 8h, allowing active
  streams to refresh and orphaned residue to expire.
- 2026-08-30 net-escrow variant: API emitted repeated `EXPIREAT` warnings for
  one `{escrow_<balance-id>}net` key with about 3.139B seconds (99.5 years)
  remaining. PostgreSQL confirmed a legitimate 10TiB balance ending
  2126-01-24 with 1,240 active escrows. The durable balance is valid; the bug
  is copying its century-long end time into a derived Redis mirror that
  reconciliation can recreate. Current source caps creation at the earlier of
  `end_time + 30d` and `now + 90d`; every other mirror write already uses the
  90-day fallback. The `redis-netescrow-ttl` log class now reports this
  mechanism and explicitly separates it from legacy stream cleanup. Its
  deterministic real-Redis test creates a 100-year balance and requires the
  mirror TTL to remain in the 89–90 day rolling band. The corrected invocation
  passed three race-enabled real-Redis runs; a rebuilt standing monitor then
  classified the continuing production line as `redis-netescrow-ttl`, rendered
  the key as `{escrow_<id>}net`, and included the cap plus the durable-balance
  non-action in its Markdown. The first complete fixed-cadence production
  stream later measured 20/min on old API g4, followed by 12/min and 15/min;
  this is a rollout target, not evidence to shorten the durable balance.
- Verify TTL repair with a binary-safe sample, `avg_ttl` below two years, and
  no new TTL warnings. Verify dataset-memory recovery independently through
  §3.3b; raising maxmemory repairs neither defect.

### 3.3b Sampled byte-family attribution
Probe: `redis-bytes`

Key counts do not attribute memory: a small family of large serialized values
can dominate millions of tiny keys. Every 15 minutes, select the Redis node
closest to its configured maxmemory and run a bounded `EVAL_RO` that scans at
most 1,000 keys. Classify binary keys inside Redis as score, provide-mode,
client-key, connect, stream, or other; sum `MEMORY USAGE SAMPLES 1`; return
only aggregate family counts and bytes. Never pass raw keys through a shell
variable. Alert when the node is at least 85% full and score data owns at
least 50% and 128KiB of sampled bytes.

The 2026-08-31T04:27Z sample on port 6380 measured 1,001 keys. Only 73 were
`{cs_}` score keys, but they occupied 1,318,490 sampled bytes (about 93.6%);
508 `{pm_}` keys occupied 48,456 bytes, 240 `ckey_` keys 24,480 bytes, 54
connect keys 7,774 bytes, 123 stream keys 10,442 bytes, and the remaining
keys 324 bytes. This identifies the capacity root independently of the tiny
impossible-TTL residue.

The score writer historically materialized counts, filter, and gob provider
samples under every `(caller location, target)` key. Production had roughly
252 caller locations, but only 138 callers had any excluded networks and 114
had none; for most caller-target pairs the payload was byte-identical. The
alias-aware schema writes one full zero-caller baseline per target, a one-byte
baseline alias for an unchanged caller, and a full override plus caller alias
only when the caller's exclusions intersect a network present in that target.
Readers treat a missing alias as the legacy caller payload and fall back to it
if an aliased baseline is absent.

Rolling compatibility is deliberate. The first successful alias-aware export
still refreshes all legacy caller payloads and publishes a durable ready marker
only after every bounded write completes. Later exports omit unchanged
duplicates. Every legacy key receives its normal five-hour TTL when written;
the task's 120-minute deadline leaves at least three hours after marker
publication even for the earliest key. Legacy blobs then expire naturally
without a production delete.
Verify after one complete export plus five hours: the sampled score byte share
falls below 50% or every node falls below 85%, provider selections remain
equivalent, excluded callers still select overrides, and OOM/error counters
stay flat. Do not delete cache keys or increase maxmemory to hide amplification.

### 3.4 Node process signals (host-level)
Probe: `redis-process`

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
Probe: `redis-connections`

- connected_clients per node: baseline ~pool_floor × processes; step change
  +50% in 10 min = reconnect storm or pool misconfig (min_connections is PER
  NODE ×32 — a config of 64 = 2k idle conns per process).
- On a sustained per-node outlier, the probe runs one bounded `CLIENT LIST`
  battery and aggregates on the Redis host by source, flags, last command, and
  client library. This distinguishes one fixed-slot key touched fleet-wide
  from a reconnect storm without transferring raw client rows.
- 2026-08-29 fixed-slot variant: node 6382 reached 2,535 clients against a
  fleet median near 231 (6394 briefly reached 1,271). The new battery found
  the dominant 6382 cohorts ending in `EXPIRE` and `SADD`—788 `EXPIRE`
  connections from edge-4 alone, with idle ages near 288s—rather than newly
  connected clients spread uniformly. The reliability recorder wrote every
  block number to the single `client_reliability_stats_blocks` discovery set;
  one touch by each process creates that process's per-node `MinIdleConns`
  pool, so merely coalescing writes once per process per minute still leaves
  the fixed node with the fleet's pools. Current writers no longer touch a
  discovery set. `RollupClientReliabilityStats` derives candidates after its
  durable pg high-water mark, bounded to the hashes' 15-minute TTL, and does
  not mark an absent/expired block covered. Roll this compatibility change out
  taskworker-first (new rollup), then api/connect (marker-free writers); after
  all processes restart, require connection counts to return near the fleet
  shape and the `SADD`/`EXPIRE` cohorts to disappear.
- The ratio is not itself attribution. At 07:26Z on 2026-08-30, a one-shot
  sample also placed nodes 6388, 6401, and 6406 above 3x the temporarily low
  fleet median, but their dominant cohorts ended in ordinary
  `PING`/`GET`/`EXEC`, not `SADD`. The former probe attached the fixed-slot
  rollout action to every outlier. Current alerts choose that action only when
  the bounded battery resolves the owner of
  `client_reliability_stats_blocks` and finds a `SADD` cohort on that owner;
  other shapes require command-rate/hot-slot, cohort-age/source, node-latency,
  pool-timeout, accept-queue, and output-memory discrimination first.
  The simultaneous 32-node `reliability-pipeline` latency sample had no active
  alert, and a repeat connection sample retained only 6382 with its explicit
  `SADD`/`EXPIRE` fingerprint; that validated the conditional diagnosis.
- A larger 08:15Z retry wave validated the discriminator with simultaneous
  positives and controls. Port 6382 had 2,002 clients versus a 212 median,
  owned marker slot 9508, and retained `SADD` cohorts, so it received the
  marker-free-writer action. Ports 6389, 6402, 6407, and 6409 were also above
  3x median, but each explicitly reported owner 6382 and ordinary
  `PING`/`GET`/`EXEC`-dominated cohorts; all four received the generic hot-node
  investigation instead. A stray `EXPIRE` or high ratio is not ownership
  proof.
- At 16:44Z port 6382 independently repeated the owned-marker shape: 700
  connected clients versus a 218 fleet median (3.2x), later 904 versus 298.
  Marker slot 9508 resolved to that port, and the bounded battery was dominated
  by persistent `EXPIRE` cohorts—293 clients from `192.168.51.181`, 84 from
  `.40`, 58 from `.95`, and 38 from `.180`—with smaller `SADD`/`PING`
  cohorts. That is the fixed-slot pool fingerprint, not Redis latency or a
  reconnect storm. The marker-free writer/high-water rollup remains the
  source fix; existing pools contract only as old service processes restart
  normally.
- `CLIENT LIST` sorted by omem: any client > 32mb = a stalled consumer.
- Accept-queue: `ss -lnt` Recv-Q pegged at backlog on a redis port = event
  loop too busy to accept() = wedge in progress (dials time out while the
  process looks alive).
- Client-side (edge hosts): `cannot assign requested address` in logs =
  ephemeral port exhaustion toward one dst (~41k tuples / 60s TIME_WAIT ≈
  680 sustainable dials/sec per destination); drains ~60s after the storm.

### 3.6 Cluster topology hygiene
Probe: `redis-topology`

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
Probe: `reliability-pipeline`

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
  This is one cluster-wide value: the set occupies one hash slot and
  `redis-cli -c` redirects every entry port to its owner. Collect it once and
  target one cluster alert. The 2026-08-30 monitor audit caught the old probe
  attaching the same value of 7 to 13 node identities, manufacturing 13
  alerts while every independently sampled node latency was 0.3ms. Node
  latency remains per-process evidence and alerts only for the affected port.
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
- 2026-08-30 re-anchor cadence and checkpoint root cause:
  `client_reliability` held an estimated 3.16B rows. The code's 20-minute
  threshold forced a full scan on every 30-minute task cycle, contradicting the
  documented four-hour cadence. More importantly, all lookbacks shared one
  transaction. At 08:29:40Z the task hit its exact 7,200-second deadline and
  rolled the whole attempt back; by 08:29:43Z the same task id and `min_time`
  were active again. `pg_stat_statements` showed the full-anchor statement's
  long-tail maximum at 12,392.06s (34 calls, 482.53s mean), so changing cadence
  alone would merely make the same unbounded failure less frequent.
- Durable fix: keep the four-hour anchor cadence and the equivalence-proven
  add-entering/subtract-leaving path between anchors, but checkpoint each
  lookback in its own `READ COMMITTED` maintenance transaction before either
  score writer runs. A timeout on a later lookback preserves earlier aggregate
  rows and markers, so the retry rolls those windows instead of rescanning
  them. If a VACUUM or concurrent index build is already present in PostgreSQL's
  progress views for at least five minutes, defer only the optional cadence
  anchor and roll this cycle; reconsider it on the next half-hour run. Missing
  state and backward-window recovery still re-anchor immediately because they
  have no correct delta path. Do not raise the task deadline.
- Deterministic proof: the model test injects a failure immediately after the
  first lookback transaction commits, verifies that its marker survives while
  the second is absent, then resumes and commits the remaining lookback. The
  cadence table separately proves that established maintenance defers a due
  periodic anchor but never suppresses bootstrap or backward-window repair.
  Production verification is: most cycles remain below p95; a quiet anchor
  commits markers one lookback at a time; an interrupted retry retains them;
  the task error clears; and downstream REINDEX/VACUUM progress rather than
  acquiring an immediate replacement blocker.
- Observed legacy recovery: the same-argument retry completed at 08:43:45Z in
  842s, advanced every marker by 163 blocks, and released REINDEX into an active
  transfer-contract index scan. The fast retry does not invalidate the fix: the
  preceding 7,200-second attempt performed no durable marker work, while the
  statement history already contains a 12,392-second tail. Checkpoints remove
  that all-or-nothing exposure without changing the score result.
- A later deployed run, task `01a05333-245b-2c2e-4e8e-1ad790efe454`, ran on
  edge-4/g2 from 15:34:17Z to 15:53:29Z (1,152.120s). Its exact PostgreSQL
  statement remained active with no wait event while the task heartbeat
  advanced, then the task completed normally. Concurrent
  `transfer_contract` autovacuum continued scanning its 23.98M-block heap.
  That terminal state rules out an orphaned/canceled backend in this sample;
  it does not make the all-lookback transaction safe. Retain the four-hour
  anchor cadence, per-lookback checkpoints, and maintenance-aware optional
  deferral, then verify their marker-by-marker behavior after rollout.
- The following task `01a05360-3544-70b6-58ec-f4327c74c449` ran on
  edge-3/g2 container `4cf91fd25a2e` from 16:23:30.019754Z to
  16:45:44.722565Z (1,334.703s) with uninterrupted heartbeats and no task
  error. During it, the anchor INSERT held the oldest useful MVCC horizon and
  the transfer-contract vacuum completed just before the task. This is another
  successful deployed all-lookback result, not evidence that its rollback
  unit is safe. The race-enabled rolling-equivalence, durable-checkpoint, and
  cadence tests cover the source behavior; production verification still
  requires a rolled cycle and a quiet four-hour anchor boundary after rollout.

---

## 4. Log error-class taxonomy (what each class MEANS)

The single most valuable diagnostic skill from the incident: reading the
error CLASS, not the volume. Classes, causes, and the action each implies:

| Class (grep) | Meaning | Action |
|---|---|---|
| `dial tcp <ip>:<port>: i/o timeout` | Node's accept path starving — process alive but event loop wedged (or SYN drop). | PING that port locally on the redis host: hangs → restart that process; fine → network path. |
| `connect: connection refused` | Port closed: process dead or bound to wrong interface after manual restart. | `ss -lntp` on the host: absent → restart; bound 127.0.0.1-only → restart with correct conf. |
| `connect: cannot assign requested address` | CLIENT-side ephemeral-port exhaustion (redial storm to one dst). | Fix the target node; storm self-drains ≤60s after; do NOT restart the client fleet. |
| `Proxy protocol header must be UDP` (legacy log) or `urnetwork_connect_pp_dropped_packets_total{reason="transport_family"}` | The UDP backend received a PROXY header whose address family is not UDP, observed when legacy `proxy_protocol on`/PPv1 traffic overlaps PPv2. Pre-hardening Connect returned this error to quic-go and could kill the shared listener; current Connect drops and counts only that datagram. | Inspect the ACTUAL LB generations and require `proxy_protocol v2`. Page if the PP-drop rate is sustained or `h3_listener_up` falls; a legacy error followed by a missing socket means a pre-fix image is still serving. See §16.2/§16.5. |
| `proxy protocol header required but not found` (legacy log) or `urnetwork_connect_pp_dropped_packets_total{reason="missing_header"}` | A previously unseen LB source tuple sent a headerless datagram: a bypassing/old LB path or broken UDP pseudo-session/header behavior. Current Connect drops it inside `PpPacketConn`; it does not escape as a listener-fatal `ReadFrom` error. | Correlate the exact Connect/LB rollout boundary and remove the incompatible sender. Verify listener gauges and sockets instead of inferring death from one rejected datagram. Do not diagnose JWT/auth or SNAT first. See §16.2/§16.5. |
| `[c]h3 listener <transport>/<port> failed stage=<stage> ... restart in ...` | A supervised QUIC listener exited or failed bind/transform/ListenEarly/accept. `/status` is 503 and `h3_listener_up=0` until the retry succeeds. | Use `stage`, `h3_listener_failures_total`, and `h3_listener_restarts_total`; fix persistent bind/allocation or transform errors. Warp must not activate an LB while any required listener is down. |
| `recv()` / `sendmsg() failed (111: Connection refused) while proxying ... upstream` on an nginx UDP stream | Host DNAT selected a connect allocation with no UDP listener. QUIC Initials reached nginx, but the kernel returned ICMP port-unreachable before connect could answer. | Map the logged logical upstream through the current host DNAT to its connect block/allocation; verify `WARP_PORTS`, `ss -lunp`, that block's last H3 listener-exit line, and `UdpNoPorts`. See §16.5. |
| `[c]h3 handshake no response mode=... sent_packets=N pto=M ...` | The client emitted QUIC packets but received zero packets before handshake failure. This is the Initial-blackhole signal that `PacketLost` misses; PTOs are counted separately. | Pin the resolved public tuple, then split public DNAT/conntrack, LB backend socket, and return path with §16.5/§16.6. Do not interpret `q_lost=0` as health. |
| `[c]h3 connect err = ... context deadline exceeded` | No valid QUIC response reached the client. This is below auth but otherwise ambiguous: direct UDP/443, DNS-envelope creation, public UDP/53 delivery, LB/PP/DNS decode, and the return tuple can all cause it. | Record the mode and resolved IP. For direct H3, split public ingress from backend-listener loss with §16.5. For `h3dns`/`h3dnspump`, prove raw envelopes were sent and compare the exact port-53 DNAT delta: zero is upstream delivery, no rule is LB activation, and a rise without a 4053 (or migration 8053) accept is LB/PP/decode (§16.6). |
| `panic: Missing host port for service port <port>` (LB startup) | The image's nginx config contains a logical listener absent from runtime `WARP_PORTS`; usually services.yml/image generation is newer than the systemd unit's baked `--portblocks`/`--forwardports`. The desired LB version may look deployed while the new container is Exited(2) and the old LB keeps serving. | Compare `systemctl cat`, container `WARP_PORTS`, and baked nginx listeners; regenerate and deploy units (§11.8), then require the new LB to stay `Up` before evaluating its behavior. |
| `failed to create TTRPC connection: unsupported protocol: \b\x03\x12Yunix` in `docker.service` (a pre-fix Warp build reports only `Start container failed: exit status 125`) | A partial Docker/containerd package upgrade left a pre-2.3 containerd daemon running while the on-disk 2.3 shim is used for each new container. The old daemon interprets the shim's protobuf bootstrap result as a socket address, so no new container can start even though every Warp systemd unit remains `active (running)`. | Compare `ctr version` client/server and check whether `/proc/<containerd-pid>/exe` is `(deleted)`. Stop retrying/restarting Warp units; restart/reboot the container runtime one host at a time, then require client/server versions to match and a replacement container to reach `Up`. See §8.5a. |
| `systemd-networkd-wait-online.service: Timeout occurred while waiting for network connectivity` after an edge reboot | At least one configured link never reached online. On the 2026-08-28 recovery, unused no-carrier NICs remained `configuring` while every serving interface was already `routable`; this failed the boot wait unit but did not imply a traffic outage. | Use `networkctl list`, source-specific `ip route get`, and public probes. The authoritative edge netplans mark known non-serving links `optional: true`; recurrence after those netplans take effect means a new required-link failure or config drift. Do not restart working networkd during recovery. |
| `snapd.apparmor.service` failed with parser errors under `snap.lxd.*` while `snapd.service` is active | Installed LXD snap profiles are incompatible with the host AppArmor parser. This is independent of Docker/Warp unless the host intentionally runs production workloads in LXD. | LXD is deliberately absent from main edges; `run-edges.sh` purges it while preserving Snapd and Canonical Livepatch. Confirm `snap list lxd` is absent and both `snapd.service` and `snapd.apparmor.service` are active. A reinstalled LXD snap is configuration drift. |
| `invalid alert rule: interval (<duration>) should be non-zero and divided exactly by scheduler interval: 10` | A file-provisioned Grafana alert group uses an evaluation interval outside Grafana 13's 10-second scheduler grid. Grafana provisioning fails, the child exits and restarts, `/status` never becomes ready, and Warp keeps the old generation serving. | Fix the rule interval to a positive multiple of 10 seconds and run `go test ./grafana` in Warp; `TestProvisionedAlertIntervalsMatchGrafanaScheduler` validates every embedded alert file. Do not restart Warp or remove the old healthy container—the same invalid image will continue failing. See §11.16. |
| `redis: connection pool timeout` | Local pool exhausted for PoolTimeout — backpressure, not the root. Deliberately NOT retried in-client (retry amplifies to livelock). | Find what is slow/stuck consuming the pool (usually a wedged node); check pool_timeouts metric per service. |
| `FATAL: query_wait_timeout` (pgbouncer) | pgbouncer server pool saturated — every server conn busy on slow queries; queued clients are killed at the timeout. A pg-side stall symptom, never a pgbouncer config problem. | Diagnose on direct 5432 (it still connects); check 1.3 active count + db host load → 5.8. |
| `pgproto3.writeError=write failed: write tcp ...->...:6432: i/o timeout` | The app could not write a request into the nginx/PgBouncer frontend before its socket deadline. Unlike `query_wait_timeout`, it may occur before postgres sees a query; direct-pg active load can stay low. | Split the 6432 nginx frontend, its 32 PgBouncer shard queues/listeners, and direct 5432 with §2.11. Group by route; do not merely increase the timeout. |
| `[plugin.notRegistered] plugin not registered` in `ngalert.scheduler` | Grafana has the provisioned datasource row but cannot load that datasource's plugin. `/api/health`, Mimir `/ready`, and the UI can all stay green while every affected dashboard query and alert rule fails. | Query through Grafana's `/api/ds/query`, then inspect `/var/lib/grafana/plugins`. For Grafana 13.2, bake the signed standalone Prometheus plugin into the image as in §11.15; do not recreate a datasource row that already exists. |
| `CLUSTERDOWN` | Slot coverage lost (node marked fail + no failover, or majority loss). | CLUSTER INFO/NODES; restart dead nodes; transient ≤ node-timeout during elections is expected and retried in-client. |
| `OOM command not allowed when used memory > 'maxmemory'` | Node at maxmemory and volatile-ttl has nothing evictable (no-TTL keys dominate). Writes fail, reads work. | Identify node (3.1); drain no-TTL piles (cleanup script) or raise ceiling temporarily; NEVER a client-side problem. |
| `pubsub ... channel is full for 1m0s (message is dropped)` | IN-PROCESS consumer stall: the app isn't draining go-redis's channel (usually because its goroutine is blocked on another redis call). While blocked, the socket goes unread → server buffers grow (3.2). | Check what the consumers block on; server-side buffer alert 3.2 is the paired signal. |
| `EOF` / `connection reset by peer` | Server closed the conn (COBL kill, maxmemory-clients eviction, restart). | Correlate with server-side events; retried in-client. |
| `LOADING` / `READONLY` | Node restarting (rdb load) / replica mid-failover. Transient; retried in-client. | Only alert if sustained > 2 min. |
| `[redis][ttl]` (server-side guard, server/redis_ttl_warn.go) | A redis write carried an effective ttl beyond its family limit, or a raw Go `time.Duration` command/eval arg. Raw Durations serialize as int64 NANOSECONDS, so an 8h ttl can become `EXPIRE <key> 28800000000000` (~913,000 years); alternatively, a correct `EXPIREAT` can expose an unbounded durable deadline. The 2026-07-20 signature was ~1.1M immortal legacy `s_sk_*` stream keys. | The warning names the command + redacted key family. For raw Duration, pass seconds/ms ints and clean the affected family. For a long `EXPIREAT`, preserve authoritative data and bound only the Redis mirror horizon; see §5.11. |
| Panic stack traces (`trace.go` "Unexpected error") | The STACK identifies the load-bearing call path (e.g. AddNetworkPeer → NominateLocalResident = connection-killing). | Rate per unique innermost app frame; a new frame appearing at rate = new incident. |
| `dohRouteForConn.func1` with `runtime error: invalid memory address or nil pointer dereference` | HTTP/2 reused or retired a live connection wrapper whose `LocalAddr()` or `RemoteAddr()` was nil. The optional route-observation callback dereferenced that endpoint, so `HandleError` recovered the resolver goroutine but the in-flight DNS result was lost; the proxy process and public listener remain healthy while a request can time out. This is not provider unresponsiveness. | Any occurrence identifies a pre-fix Connect module. Current code treats nil and typed-nil endpoints as absent diagnostic metadata and preserves the DoH response. Deploy the fixed proxy generation, then require zero new occurrences while sustained HTTP/SOCKS/WireGuard acceptance runs. See §14.6. |
| `urnetwork_connect_contract_failures_total{cause="insufficient_balance"}` (Mimir; `[contract][error] class=insufficient_balance` is a rate-limited exemplar only) | Payer network has no usable balance. Runs at a steady background rate (~1,000+/min measured 2026-07-17) from out-of-data free users — presence is NOT an incident. | The provisioned Grafana rule watches the lossless 5-minute counter rate; >4,000/min for 5 minutes = netEscrow drift re-emerging (`bringyourctl contracts reconcile-net-escrow --dry-run`) or a balance-grant regression. Do not calculate the rate from sampled logs. |
| `asset amount owned by the wallet is insufficient` / `insufficient token balance ... in wallet` (taskworker, circle payment path) | The payout wallet cannot cover pending payouts (usdc on solana — mint EPjFWdd5...Dt1v in the error text). NOT an api failure: every AdvancePayment retry 400s until the wallet is funded, parking the tasks on backoff (decoded 2026-07-18 from the novel class — the full error text names the wallet id, its balance, and the required amount). | Finance/ops: fund the payout wallet (or pause payouts). Task-side symptoms clear on their own once funded and the backoff run_at arrives. |
| `Invalid destination address.` / Circle code `155219` (taskworker, Circle payment path) | The destination is invalid for its declared chain and Circle rejected it before creating a transfer. On 2026-08-30 all five rows were 44-character Solana base58 keys stored on active `MATIC` wallets; the chain-blind validator always parsed base58 regardless of `chain`. Their stable submit keys then pinned each payment to the bad wallet even after a payout-wallet correction. | Correct the payout wallet and deploy chain-specific SOL/MATIC validation. Clear the pinned attempt only after the typed Circle 400/code confirms this definitive pre-chain rejection, allowing `UpdatePaymentWallet` to select the correction. Preserve the key for transport failures, 429s, and every ambiguous submit; never delete payment/sweep rows. See §5.7. |
| `urnetwork_connect_contract_failures_total{cause="missing_companion_origin"}` (Mimir; `[contract][error] class=missing_companion_origin` is a rate-limited exemplar only) | A contract request resolved to the companion path (destination usable only as reply traffic — announced stream-only / provide-off / gone) but no reversed origin contract exists. Emitted by the earliest-origin lookup (subscription_model CreateCompanionTransferEscrow). ~90/min background; `companion=false` means NORMAL requests are degrading to this path — the destination's keys are the problem, not the requester. | The provisioned Grafana rule watches the lossless 5-minute counter rate; >500/min for 5 minutes means clients are being pointed at non-contractable destinations. Use the sampled log only to obtain a failing pair, then check the destination's `{pm_<clientId>}sk_*` keys. |
| `Resource not found in vault (<resource>.yml)` in a route panic | A lazily resolved resource is absent from the deployed vault generation. The process and `/hello` can stay green indefinitely; only the first request to the dependent route fails. On 2026-08-29, `/verify/keys` and `/verify/stats` returned 500 while `/hello` remained 200 because the unreleased subnet was disabled and its deliberately absent `verify.yml` was nevertheless loaded by unconditionally exposed handlers. | First branch on feature state. If disabled, fail closed with a stable 503 before parsing or vault access; do not fabricate a signing secret merely to stop the panic. If enabled, the missing resource is a deployment blocker: provision it through the supported secret mechanism and probe the affected route on every active generation (§8.7). |
| `[session]X-UR-Forwarded-For ... was not one ip:port value` or legacy `X-UR-Forwarded-For from untrusted peer` | Source attribution fell back to the ingress peer, collapsing users onto one address for signup/login limits and `/my-ip-info`. The legacy line proves a pre-standardization binary is still active. | Verify Warp overwrites one bracket-safe `ip:port` value, backend ports are not publicly reachable, and every active api/connect generation accepts the UR header. Probe both address families as in §8.8; do not add a proxy CIDR. |
| `[netescrow]negative counter after <site>` | A Redis reservation mirror had fewer bytes than PostgreSQL durably released. Besides a lost create/double release, a long legacy reconcile can overwrite live mirror traffic (§5.11); even the fixed page-local reconciler retains a small PostgreSQL-commit/Redis-post ordering window. Old binaries leave the negative value until reconciliation. Current release Lua emits `clamped_to=0` after atomically deleting it while retaining the negative diagnostic result. Any occurrence remains a defect. | Correlate the first burst with `ReconcileNetEscrow` duration and aggregate drift. Roll out both the page-local additive reconciler and atomic release clamp; after rollout verify any residual line says `clamped_to=0` and its key is absent. Alert artifacts retain only `site`; balance/contract ids are redacted. |

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
For a 1.3 count > 100, first separate the count owner from the age owner.
Redis latency inside tx scopes remains one known cause: the same grouped query
shape stays continuously idle and the pool recovers instantly when Redis does
(563 → 2 observed). Backlog close workers instead appear as many
per-contract query shapes with sub-second continuous idle ages; do not
mass-terminate them. The battery always reports the single continuously oldest
transaction even when its one-row query shape falls below the top six groups.
Follow a subsidy-window query to the Payout canary and its transaction-local
idle-timeout fix. Kill only proven zombies > 30 min;
`idle_in_transaction_session_timeout` is the standing guard.

The 10:43Z recovery sample demonstrated why this split matters. There were 121
idle-in-transaction clients and the oldest transaction was 532s old, but the
high-count closer shapes were only 0–1s continuously idle. One Payout planner
was the age owner: its subsidy-window transaction was 277s continuously idle
and disappeared as it crossed the global 300s cutoff. The prior battery listed
only shapes ordered by count, omitted that singleton, and therefore suggested
generic Redis leakage. The probe now emits an explicit oldest PID/query line,
labels summary age as transaction age rather than continuous idle duration,
and explains the bounded-closer/Payout split.

### 5.7 Task parked / task long-running
Covered in 1.2 gotchas: parked = error_count>0 ∧ run_at far ∧ lease expired →
pull forward once the cause is fixed. Long-running = live lease + claim
heartbeat advancing → let it run; compare against finished_task history
before declaring it stuck. The grouped task alert includes the representative
row's `sample_max_time_s`; for a non-`Drained:` context cancellation, compare
that value with the taskworker `eval error` duration before deciding whether
the task-specific deadline is undersized.

Two 2026-08-29 task-family variants were formerly hidden behind the raw-row
limit:

- `AdvancePayment`, five `400 Bad Request` rows with Circle
  `Invalid destination address.`: a complete read-only breakdown found two
  active external wallets across two networks (three payment plans), all
  declared `MATIC` but carrying 44-character Solana base58 public keys. Both
  wallets were still selected for payout; the rows had retried 373–1,059
  times. `WalletValidateAddress` ignored the declared chain and always called
  the Solana base58 parser, so these addresses passed registration before
  Circle rejected them as Polygon destinations. The payment path creates a
  stable idempotency key before submission, and `UpdatePaymentWallet` correctly
  refuses to change a wallet once that key exists; without distinguishing this
  definitive rejection, a user correction cannot reach the parked payment.
  Validate SOL with the Solana parser and MATIC with a nonzero, `0x`-prefixed
  EVM address; reject Ethereum in this SOL/MATIC payout-wallet endpoint (TAO
  remains a non-payout identity). After typed HTTP 400/code 155219, Circle has
  created no transfer, so clear only that pre-chain attempt and let the next
  retry select the corrected payout wallet. An arbitrary string match is not
  enough: malformed responses, generic 400s, 429s, and transport errors retain
  their stable key because submission may be ambiguous. Deterministic tests
  cover both cross-chain address directions, zero/unknown destinations, typed
  and wrapped error classification, corrected-wallet selection with a fresh
  key, and key reuse after an ambiguous error.
- `Payout`, `ERROR: no empty local buffer available (SQLSTATE 53000)`: the
  2026-08-31 UTC production row reached 153 failures on PostgreSQL 18.4 while
  `effective_io_concurrency=200` and `temp_buffers=8MB`. Its stack ended in
  `PaymentPlanner.finalizePayments` while scanning the planner's temporary
  tables. This exactly matches PostgreSQL 18's read-stream lookahead defect:
  high effective I/O concurrency can pin every local buffer for a temporary
  relation. The [upstream bug report](https://www.postgresql.org/message-id/CAFMO8-rYPSJbXsDdWDzDdpNi-fQ%2B6bKvgbXwE%2BR=sGko4epq0Q@mail.gmail.com)
  identifies the temporary-buffer failure, and PostgreSQL's
  [18.6 release announcement](https://www.postgresql.org/about/news/postgresql-186-1711-1615-1519-1424-and-19-beta-3-released-3365/)
  includes the server fix.
  Upgrade PostgreSQL to 18.6 or newer for the root fix. Until then, the
  payment-plan transaction applies `SET LOCAL effective_io_concurrency = 32`;
  this contains the affected read stream without reducing the production-wide
  value for unrelated queries. Do not raise `temp_buffers`, delete the task
  row, or blindly lower the global concurrency setting. Verify 32 inside the
  Payout transaction, the original value on a fresh session after commit, and
  completion of the same pending row. Remove the containment after the server
  upgrade only with a production-shaped temporary-table regression. The
  deterministic regression records both exact `SET LOCAL` statements and
  verifies that both transaction settings return to their session baselines
  after commit.
- `Payout`, `pgconn.connLockError=conn closed`, repeatedly after roughly
  16 minutes: production's database-level
  `idle_in_transaction_session_timeout=5min` closed the outer payment-plan
  transaction while a deliberately separate, long reliability maintenance
  transaction ran. The bounded payout task now applies `SET LOCAL
  idle_in_transaction_session_timeout=0` to that transaction only. Its task
  `MaxTime` and four-day plan slices remain the bounds; the global five-minute
  guard still protects every unrelated session. Verify one payout slice
  commits, the same pending row's error clears, and a fresh DB session still
  reports the configured five-minute setting.
  A live 2026-08-30 retry reproduced every boundary: Payout began at 07:58:25Z;
  its outer transaction reached 726s total and 260s continuously idle on the
  subsidy-overlap query, then disappeared after crossing the 300s database
  cutoff while a separate `network_connection_reliability_score` INSERT kept
  running. The task finally failed at 08:17:36Z after 1,150.90s with
  `pgconn.connLockError=conn closed`, raising the same row to 143 failures.
  This is affirmative validation for the transaction-local override, not a
  reason to disable the global guard.
  The next retry reproduced the separation even more precisely. It began at
  09:17:37Z; outer PID 1554457 entered `idle in transaction` on the
  subsidy-overlap query at approximately 09:24:33Z and disappeared as that
  uninterrupted interval crossed 300s near 09:29:33Z. The task claim kept
  heartbeating while its separate computation finished, then taskworker logged
  `eval error(1011.80s)` at 09:34:28Z and advanced the same row from 143 to 144
  failures. The roughly five-minute delay between connection death and error
  observation explains why task duration alone cannot identify the timeout;
  follow the outer backend state and the nested work together.
  A third retry repeated the boundary under the later recovery load. Payout
  began at 10:34:31Z; its outer transaction became continuously idle on the
  same subsidy-window query near 10:42:41Z, was still present at 298s idle,
  and disappeared before the next 12-second sample after crossing 300s. The
  task claim continued for almost eight more minutes, then logged
  `eval error(1264.30s)` with the same `pgconn.connLockError=conn closed` at
  10:55:35Z and advanced the row to 145 failures. The next retry remained on
  the normal hourly backoff at 11:55:35Z. Repeatedly observing the database
  guard fire long before task error presentation is direct validation for
  changing only the outer transaction's local setting.
  That fourth retry began at 11:55:36Z. Outer PID 1846780 spent roughly seven
  minutes actively building the subsidy window, then became idle in transaction
  on the `subsidy_payment` overlap query near 12:03:03Z. It was still present at
  291 seconds of uninterrupted idle and absent at the 12:08:10Z sample, while
  taskworker continued heartbeating beyond 753 seconds. Total transaction age
  had already exceeded 700 seconds, so the disappearance again follows the
  continuous-idle guard, not a total-runtime limit. This fourth independent
  boundary reproduction keeps the same scoped remediation and supplies a
  post-vacuum-load control. Taskworker ultimately logged
  `eval error(1068.46s)` with the same `pgconn.connLockError=conn closed` at
  12:13:25Z and advanced the row to 146 failures, completing the expected
  database-cutoff-to-late-task-error chain.
  The fifth retry began at 13:13:26Z. During the attempt, a direct PostgreSQL
  sample found its outer subsidy-window transaction 533s old and continuously
  idle for 89s; a later sample found no matching outer backend while the task
  heartbeat continued through 1,307s. Taskworker then logged
  `eval error(1312.95s)` with the same `pgconn.connLockError=conn closed` at
  13:35:19Z and advanced the row to 147 failures. This is the same
  database-connection-loss chain under the longest observed tail so far, not a
  reason to raise the task's 21,600s deadline.
  Two more deployed attempts reproduced the identical terminal class. The
  sixth ran on edge-3/g1 until `eval error(1346.46s)` at 14:57:48Z and raised
  the row to 148 failures. The seventh ran on edge-3/g2 from approximately
  15:57:49Z until `eval error(1278.54s)` at 16:19:09Z and raised it to 149;
  both stacks ended in `createPaymentPlan` with
  `pgconn.connLockError=conn closed`. The hourly recurrence across taskworker
  generations rules out a one-process connection accident and continues to
  validate the transaction-local timeout override. Do not pull the parked row
  forward or disable the global five-minute policy; verify the same slice
  commits only after the scoped source fix is rolled out.
  The eighth retry ran independently on edge-4/g2 and ended at 17:35:44Z with
  `eval error(994.66s)` and the same `*pgconn.connLockError=conn closed`,
  advancing the row to 150 failures. Its fresh claim then expired and the row
  returned to the normal hourly backoff. This independent executor recurrence
  preserves the same classification and remediation; it is not score-worker
  contention or a reason to disable the database-wide guard.
  The deterministic regression first records the session baseline, installs a
  25ms transaction-local timeout, applies the payment-plan override, idles for
  100ms, and requires a successful commit; it then requires the same connection
  to report its original baseline again. This proves survival and scope without
  weakening the standing database policy. It passed three race-enabled runs
  against real local PostgreSQL after the fourth production reproduction.
- `RefreshVerifyProxyEgress`, `Interrupted: Done` / `Interrupted: context
  canceled`, 940 retries observed while
  `st.yml enabled:false`: this is disabled work, not a Redis incident. All four
  verification recurring-task schedulers now stop when `StEnabled()` is false,
  their task functions return before dependency access, their Post hooks cannot
  perpetuate a chain, and taskworker startup removes surviving RunOnce rows
  from older generations. The function guard closes the claim/config race in
  which cleanup sees no row because a worker already owns it. Do not pull this
  row forward; deploy the feature-state gate and require the family to
  disappear. The 940th retry supplied the exact deadline discriminator: its
  live heartbeat advanced from 10.01s through 899.27s and `eval error` landed
  at 900.05s, matching `run_max_time_seconds=900`. Increasing that deadline
  would only let disabled work run longer. The same surviving disabled row
  reproduced the boundary at 12:42:32Z: its heartbeat reached 890.39s, then
  taskworker logged `eval error(900.00s) ... = Interrupted: context canceled`
  and advanced the row from 942 to 943 failures with its next run parked an
  hour later. The monitor simultaneously reported the fresh-claim/parked
  handoff overlap, so this is another exact stale-chain reproduction rather
  than verification traffic or a short deadline. The task-canary probe now carries
  the canonical verification feature state in `SignalSettings`: in a disabled
  environment it reports the stale ungated RunOnce chain and startup reap,
  while an enabled environment retains the generic deadline diagnosis.
- `ExportStats`, `Interrupted: context canceled` at **120.1–120.3s** followed
  by a successful 60–80s retry: this is the generic two-minute task deadline,
  not a deploy drain. Confirm in taskworker logs that `eval error` lands at the
  row's `run_max_time_seconds` and that there is no contemporaneous
  `[taskworker]drain canceling` line. This task performs four 90-day aggregate
  passes, whose normal primary-load variance can cross two minutes. Its
  scheduler now sets a bounded ten-minute `MaxTime` (still well below the
  hourly cadence), preventing the canceled pass from being recomputed from the
  beginning. After rollout, require `run_max_time_seconds=600`, one completion
  per hour, and no new non-`Drained:` context-canceled retries. Do not enlarge
  the global default: short tasks should retain the tighter bound. The deployed
  boundary recurred at 10:47:34Z: taskworker logged `eval error(120.04s)` with
  no drain line, then the same task id retried on another worker and completed
  in 68.125s. Its next hourly row retained the old 120-second configuration.
  That exact-error/fast-retry pair is another production validation of the
  task-specific 600-second source fix, not a reason to change global task
  cancellation semantics. The next scheduled attempt at 11:48:50Z completed
  normally in 64.49s under the same deployed limit. That healthy sample
  confirms the task is not deterministically slow, but does not erase the
  observed load-sensitive 120-second cancellation; retain the task-specific
  ceiling and verify it after rollout across multiple hourly cycles.
- `RemoveDisconnectedNetworkClients`, completed in **6,530.6s** (108.8m)
  versus a 7-day
  p95 of 2,754s (four-hour task ceiling): distinguish PostgreSQL backlog from
  the post-delete Redis tail before changing indexes. For the 2026-08-30
  03:30Z run, all five capped eligibility probes using that invocation's fixed
  thresholds were zero (old connections, idle top-levels, inactive reap,
  connected-child bump, and disconnected-child reap), and no reaper backend
  was active in `pg_stat_activity`, while the task heartbeat advanced every
  ~10s. PostgreSQL stats since the six-day reset showed the real scale—about
  12.5M inactive and 2.37M child-client deletes. Its immediate successor still
  took 424.9s, the expected catch-up tail after rows accumulated during the
  108.8-minute run. The old post-delete path then
  re-entered `server.Redis` separately for every reaped client (a PING plus
  public-key deletion, another PING plus HGETALL/conditional forward deletes/
  reverse deletion), and executed a separate provide-key pipeline per client.
  Current code processes 1,000-client chunks through idempotent, cluster-aware
  plain pipelines. It completes conditional forward compare-deletes before
  discarding reverse evidence, so a partial cluster error remains retryable
  and an address already reassigned to another client is never clobbered.
  Verify a large run returns toward its historical seconds/minutes band, the
  `task-overdue` alert clears, and the Redis regression still removes target
  public/reverse/eligible state while preserving a reassigned forward owner.
  The defect recurred at 13:20Z in task
  `01a052d2-9c33-c78e-1e37-66e411e45c1e` on edge-3/g2 container
  `4cf91fd25a2e`: its heartbeat passed 6,220s with a fresh claim and no task
  error. Its 7-day distribution had become p50 42s, p95 3,552s, and max
  6,644s, so the old `>2*p95` monitor rule treated another hour-scale tail as
  normal. The taskworker was 34.7GiB RSS versus 0.46GiB for its g1 sibling and
  shared the executor with the slow close and net-escrow passes. The host had
  ample CPU and memory and the cgroup reported zero throttling, OOM, or
  pressure. The task-canary probe now applies the median-tail cap from §1.2,
  carries the exact task id plus heartbeat executor identity, and includes the
  bounded Redis cleanup diagnosis. Its two synthetic regressions repeat the
  6,283s/42s/3,552s/6,220s shape and also prove that a 600s new attempt whose
  due time is 6,283s old is suppressed rather than misreported. The next
  production run supplied an independent memory control: task
  `01a0534c-cb89-633c-78b2-417bb4ea3717` ran on edge-4/g1 from 15:35:01Z to
  15:51:28Z and completed normally in 987.628s. Its process stayed near
  0.61–0.71GiB RSS (and was only 0.07–0.16GiB allocated heap during the
  earlier samples), while another executor's score export cycled through
  10–30GiB. Keep the bounded Redis latency fix, but do not attribute the score
  allocator's heap to this reaper.
  A subsequent reaper `01a05379-a1b0-d902-e4bd-4aef15756092` began on
  edge-1/g1 at 16:25:04Z and remained live beyond 1,870s with fresh
  heartbeats. Its seven-day p50/p95 were 51s/3,718s, so the generic
  median-tail cap correctly alerted at 1,200s instead of normalizing the
  recurrence to twice the inflated p95. It shared the score-heavy process,
  but the independent low-heap control above still separates reaper latency
  from score allocation. Follow this row to its authoritative terminal state;
  the bounded 1,000-client, compare-delete cleanup remains the root fix.
  The row completed cleanly at 17:17:46.333688Z in 3,162.175s—33 seconds
  after its colocated score export ended and heap collapsed. Its successor
  then landed beside the next score export on edge-0/g1 and again crossed
  several minutes with fresh heartbeats. The paired low-heap 987.628s control,
  score-boundary acceleration, and recurring high wall time support both
  conclusions: old per-client Redis round trips are intrinsically slow, and
  score allocation amplifies them locally. Keep the idempotent 1,000-client
  pipelines and compare-delete safety; do not replace them with scheduler
  affinity or a larger deadline.

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
5. Selection freshness AND population (2.8, 2.9): check
   `UpdateClientScores` last completion, cs_ key ttls, and the decoded normal
   vs ForceMinimum provider counts. A completion gap ≈ symptom onset means a
   stale snapshot. A fresh normal count of 0 with a large ForceMinimum count
   means a gate wipe — do not declare selection healthy merely because the
   task completed and refreshed ttl. Check task-overdue (§7) for a grinding
   rebuild, and deployed `provider.yml` plus egress-table population for a
   fresh-but-empty one.
6. Recovery is automatic when the rebuild completes (fresh cs_ writes, hot
   reliability nodes fall back to baseline, dots green on next app
   connect). Clients holding pre-rebuild candidate lists keep failing until
   they re-fetch — a decaying tail, not a re-incident.
   If the pending row still carries a prior error but its claim heartbeat is
   live, a recovery run is in progress; watch its `[nclm]export client
   location[N/total]` index increase. Do not release or duplicate that live
   task. A transport timeout late in the export is addressed by the bounded,
   failed-chunk-only retry described in §2.8; require a finished_task row and a
   refreshed sample-key ttl before declaring recovery.
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

### 5.11 Net-escrow reconcile clobbers live reservations

Probe: `netescrow`

Signature: `[netescrow]negative counter after settle` rises immediately during
or after `ReconcileNetEscrow`; its completion log reports a large
`under-reserved` correction, while Redis and PostgreSQL are otherwise healthy.
This differs from an isolated lost create/double release: many unrelated
balances flip together at reconcile cadence.

On 2026-08-29/30, normal pre-fix runs completed in roughly 15–55s, but several
took 1,021–1,502s. One 1.8M-balance run reported 5.79TiB under-reserved; a later
1,182s run reported 597.3GiB, and its 17s successor corrected roughly the same
amount in the opposite direction (594.29GiB over-reserved). That alternating
drift proves reconcile-created corruption rather than independent lost writes.
The next observed long run completed in 990s with 976.53GiB over-reserved; its
10s successor flipped to 975.37GiB under-reserved. Two more short runs alternated
165.84GiB under then 160.84GiB over before the fifth converged to
16.72GiB/13.04GiB. This repeated long-run/short-repair sequence is the exact
stale-snapshot clobber and convergence signature.
The negative-counter class peaked at 964/min (more than 10,000 lines were
observed in one 29s burst). The old
algorithm took one fleet-wide PostgreSQL reservation snapshot, then walked all
balances and used Redis `SET`. By the end of a long run the snapshot was many
minutes stale, so `SET` erased reservations/decrements that had happened since
the snapshot. Subsequent settlements drove the overwritten counters negative.

A second production recurrence at 06:48Z on 2026-08-30 sharpened the temporal
test. Five preceding runs finished in 16–23s; the next remained live past 724s
without a matching long PostgreSQL statement. While it was live, a close-task
cohort settled contracts and emitted 732 negative-counter lines/min, falling to
107/min after the burst. The run eventually finished in 1,107s with 5.38TiB
under-reserved and 483.73GiB over-reserved. Its immediate 22s successor then
reported 5.38TiB over-reserved and 478.22GiB under-reserved, after which the
negative-counter stream reached zero. This matched-value, opposite-direction
repair is stronger evidence than timing alone. The close site revealed the
corruption, but did not create the shared stale state: correlate the long
reconcile precursor before blaming thousands of independent settlements.

A third recurrence at 07:17Z supplied the full convergence sequence. The run
lasted 1,492s over 897,486 balances and ended with 546.45GiB over-reserved plus
451.05GiB under-reserved; taskworker's negative-counter class reached
7,960/min in its immediate aftermath. A 22s successor reversed the dominant
directions to 442.58GiB over and 531.10GiB under, and the following 20s pass
converged to 26.52GiB over and 37.70GiB under. This long-run, short reversal,
short convergence sequence again identifies stale absolute writes rather than
independent balance defects. All three emitting services then recorded zero
new negative-counter lines for the next full five-minute reconciliation
interval.

A fourth sequence made both the reversal and the live-writer timing explicit.
The 08:30Z pass ran 1,306s over 897,616 balances and ended with 495.31GiB over
plus 2.68TiB under. Its 21s successor reversed those dominant values to
2.68TiB over plus 488.13GiB under, and all three emitting services reached zero
negative lines by 08:59:53Z. The following 09:02Z pass ran 1,211.90s over
897,659 balances and ended with 569.42GiB over plus 632.26GiB under. While that
old fleet snapshot was still being applied, four closer
cohorts completed in 24s/full, 34s/full, 7s/full, and 2s/not-full. Taskworker
emitted 11,625 negative-counter lines from 09:16:26.279Z through
09:17:28.940Z—almost exactly the closer interval. Two later close cohorts
finished in 16s and 4s while the same reconcile was still live; their
09:22:30–09:22:33Z settlement interval exposed another 5,221 negative writes.
Across those two live-run bursts, taskworker emitted 16,846 lines (18,248 when
the preceding 1,402-line decay minute is included). PostgreSQL had no long
reconcile statement, the owning taskworker was consuming CPU, and every Redis
master was serving operations with zero blocked clients. This rules out a
wedged dependency: the pre-fix walk was spending its time issuing a read and
an unconditional `SET`/`DEL` for roughly 898k balances. A ten-second
`INFO commandstats` delta on node 6406 while the pass was live recorded 5,961
GETs, 261 SETs, and 2,065 DELs, with no blocked client. The close cohorts exposed
stale values but did not create them; the corruption source was the
many-minutes-old absolute snapshot being written concurrently.

The first repair pass began at 09:27:55Z and completed in only 16.39s, proving
the dependencies and page count were healthy. It still found 1,629 drifted
networks (621.2GiB over and 561.59GiB under), and the intervening 14s/full close
cohort exposed another 8,289 negative counters from 09:27:01Z through
09:28:11Z. Thus the fourth sequence produced 26,537 taskworker negative lines
across its decay/live-reveal/repair windows. A short successor does not by
itself prove convergence; follow scheduled passes until the opposite-direction
aggregate contracts and all emitting services stay at zero for a full interval.
The following scheduled pass supplied that terminal check: it completed in
17.85s, reduced the aggregate to 29.43GiB over plus 34.57GiB under across 790
networks, and taskworker emitted no negative counters for the full six minutes
after 09:28:11Z. This is recovery from the corruption, not evidence the old
writer is safe; a later long absolute pass can recreate the same sequence.

A sixth long recurrence proved that warning immediately. The 09:38:34Z pass
ran 1,005.43s over 905,318 balances and recreated a strongly one-directional
drift across 1,687 networks: 2.02TiB over-reserved versus only 65.88GiB
under-reserved. Settlements then exposed 21,436 taskworker negative counters
from 09:58:37Z through 10:00:39Z. The scheduled repair completed in 17.52s and
reversed the same dominant quantity—66.97GiB over and 2.02TiB under across
1,707 networks—with the last negative line arriving immediately before that
completion. Thus a short pass that had converged to tens of GiB did not make
the deployed algorithm safe: its next unconditional full-fleet `SET`/`DEL`
walk recreated TiB-scale corruption. Skipping an already-correct mirror is
therefore part of the root fix, not merely a performance optimization; retain
the page-local additive race regression and the no-op-write regression. The
next scheduled pass completed in 16.33s and contracted the aggregate to
31.66GiB over plus 34.41GiB under across 819 networks; no negative counters
appeared for the full five-minute interval after repair. That supplies the
terminal convergence check for this sequence without making the next legacy
full-fleet pass safe.

A seventh recurrence showed that even the first corrective successor can
become another stale writer. The 10:11:00Z pass ran 1,095.56s over 905,372
balances and ended with 2.17TiB over-reserved versus 85.94GiB under-reserved
across 1,760 networks. Four API and five connect settlements exposed negative
counters from 10:29:06Z through 10:30:50Z. Its scheduled successor then ran
405.88s—not the normal 15–20s repair—and reversed the dominant drift to
393.04GiB over plus 1.79TiB under across 2,018 networks. That second stale
walk produced 41 more negatives (35 taskworker, five API, one connect) from
10:41:07Z through 10:43:44Z. A 22.02s pass only partially contracted the
aggregate to 184.81GiB over plus 865.12GiB under; short duration alone was not
a terminal recovery check. The following 15.80s pass finally reached
32.49GiB over plus 50.49GiB under across 852 networks, and all three emitting
services were at zero for the full interval after the last negative. This
chain reinforces both halves of the fix: page-local observations bound
staleness, while skipping already-correct mirrors avoids enough fleet writes
to keep a corrective pass from becoming the next incident. The monitor now
retains the latest completed precursor's duration and age in an active
successor alert, so two consecutive overruns are represented as one explicit
lifecycle chain rather than the second silently replacing the first.
The next scheduled pass remained in the healthy band at 36.47GiB over plus
34.15GiB under, with no negative counters, confirming the terminal state.

An eighth recurrence immediately demonstrated why one healthy pass was not a
rollout substitute. The 11:02:05Z run lasted 1,071.15s over 897,891 balances
and ended with 2.15TiB over-reserved versus 98.3GiB under-reserved across 1,905
networks. Seven API and four connect settlements exposed negative counters
from 11:18:48.831Z through 11:21:22.337Z; taskworker emitted none in that
window. Its first corrective successor completed in about 22.5s and reversed
the dominant quantity to 106.82GiB over plus 2.15TiB under across 1,949
networks. That short reversal proves the same stale absolute-write mechanism,
but is not terminal convergence: require the following scheduled aggregate to
contract toward the tens-of-GiB band and all three emitting services to remain
at zero for a full interval. The following scheduled pass supplied that check:
it completed in about 21s, reduced drift to 42.07GiB over plus 47.33GiB under
across 926 networks, and taskworker, API, and connect each remained at zero
negative counters for the full interval. The incident recovered without making
the next legacy full-fleet pass safe.

A ninth recurrence closed a duration-only monitoring blind spot. A pass ending
at 11:52:10Z finished in about 19 seconds but corrected 1004.36GiB
under-reserved versus only 15.14GiB over-reserved. Its roughly 20-second
successor ending at 11:57:32Z moved the same quantity back: 1006.26GiB
over-reserved versus 14.09GiB under-reserved. Taskworker, API, and Connect each
remained at zero negative-counter lines because no sampled settlement happened
to decrement the overwritten mirrors before the corrective pass. The matched
inverse still proves the fleet-wide absolute snapshot clobber; neither a
sub-120-second duration nor a quiet negative-counter stream is a recovery
certificate. The `netescrow` probe now also retains 15 minutes of aggregate
logs and emits `netescrow-large-drift` when either direction reaches 256GiB.
It labels adjacent quantities within 20% in opposite directions as a matched
reversal, while leaving a one-direction event for durable-reservation tracing
instead of overclaiming its cause. The deterministic synthetic pair reproduces
the exact short under-to-over flip; a tens-of-GiB short pass remains healthy.
The following two scheduled passes supplied the terminal check: they contracted
to 38.51GiB over/45.15GiB under and then 36.93GiB over/41.65GiB under, while
taskworker, API, and Connect each remained at zero negative counters throughout
the full intervals.

A tenth sequence showed that the initial 512GiB aggregate threshold was too
coarse. A 20.73s pass ending at 12:45:35Z corrected 380.77GiB under-reserved
versus 29.05GiB over-reserved across 1,464 networks. That is more than eight
times the preceding 24-hour median pass maximum of 45.15GiB, but the first
aggregate probe would have called it healthy. Its successor then ran from
12:50:37Z to 13:08:31Z (1,073.80s) and corrected 2.70TiB over-reserved versus
81.85GiB under-reserved across 2,257 networks. The rebuilt monitor reported the
short 380.77GiB event with `matched_reversal=false`, then retained the live and
completed overrun and the 2.70TiB aggregate independently. The threshold is
therefore 256GiB: still over five times the healthy median band, but low enough
to preserve the short precursor instead of relying on its successor to become
catastrophic. The exact 380.77GiB synthetic case and the ordinary 45GiB control
both run under race detection. The negative aftermath began during the final
minute of the apply: the first sampled Connect error was at 13:07:58Z and API
at 13:08:13Z, before the aggregate log at 13:08:31Z. By 13:10:24Z the exact
eight-minute query found six API and seven Connect negatives, while taskworker
remained at zero; the monitor's first one-minute sample reported five and three.
An immediate post-completion query returned no lines because the remote log
results had not arrived yet, so completion-time silence is not a safe negative
control. Keep this sequence open until the corrective aggregate contracts and
all three emitters remain quiet for a full interval.

The first scheduled successor confirmed the inverse but did not converge. Task
`01a052c9-2ecc-a48f-5f1a-4f79b0c831a9` ran from 13:13:33Z to
13:13:55Z in 21.85s and corrected 115.91GiB over-reserved plus 2.37TiB
under-reserved across 2,047 networks. The monitor matched its dominant
under-reserved quantity to the preceding 2.70TiB over-reserved correction and
reported `reversal_direction=over-to-under`. A short inverse is causal evidence
for the stale absolute write, not a recovery certificate: 2.37TiB remains far
outside the 256GiB band. Follow the next scheduled aggregate, and start the
full three-emitter quiet interval only after a genuinely contracting pass.

The next two scheduled passes supplied that terminal certificate. Task
`01a052ce-207b-164e-e724-2df892d11bcd` completed at 13:19:25Z in 28.42s and
contracted to 66.33GiB over/42.61GiB under across 994 networks. Task
`01a052d3-2948-c5cd-bb58-752bed8f963d` completed at 13:24:45Z in 17.93s
and remained in band at 37.03GiB over/65.06GiB under across 1,012 networks.
Taskworker, API, and Connect each stayed at zero negative lines throughout the
full interval between them, including the post-pass ingestion allowance. The
tenth sequence therefore recovered without intervention, but the deployed
full-fleet apply remains unsafe.

That recovery exposed a trailing-window alert bug. Once the 13:08:31Z
2.70TiB precursor aged out of the 15-minute log query, the still-retained
13:13:55Z 2.37TiB inverse was incorrectly rendered as
`matched_reversal=false`, contradicting the same monitor's earlier proven
match. The probe now reports `matched_reversal=unknown_window_boundary` when
a large correction is the oldest retained aggregate and later passes follow
it. A bounded window may lose the predecessor; it must not rewrite retained
incident history into a one-direction claim. The deterministic regression
uses the exact inverse plus both healthy successors and rejects `false`.

An eleventh sequence then proved why executor identity belongs in the monitor.
Task `01a052d8-0c5e-3471-5ac8-2ec7ef3aeca4` ran from 13:29:48Z to
13:47:40Z (1,071.29s), ending with 2.30TiB over-reserved and 148.44GiB
under-reserved across 2,107 networks. Six API settlements after completion
exposed exact -1MiB counters before the 18.42s successor reversed the dominant
quantity to 148.18GiB over and 2.30TiB under. A second 18.46s pass contracted
to 49.40GiB over/37.01GiB under, and all three emitters remained quiet for the
full following interval.

The next scheduled pass showed that convergence and fast successors still do
not make the deployed writer safe. The monitor followed task
`01a052f6-cbde-7a3b-ce45-cb6e8554c036` on
`by-us-fmt-5-edge-3/g2`, container `4cf91fd25a2e`, from 14:03:23Z to
14:20:24Z (1,021.01s). It rewrote 898,352 balances and recreated 2.23TiB
over-reserved versus 118.03GiB under-reserved across 2,060 networks. No
PostgreSQL transaction remained active during the long tail and all three
negative emitters were initially zero; dominant over-reservation can deny new
contracts without producing a negative decrement, so silence is not a recovery
certificate. `warpctl ls versions main taskworker --sample` showed the same
`2026.8.28+1031763440` binary/config version in all 20 g1 and all 20 g2
samples. The earlier 18-second passes were therefore runtime variance, not a
partial fixed rollout. The `netescrow` alert now carries source
task id plus host/generation/container for active heartbeats, and source
host/generation/container for aggregate pairs. That
identity lets operators connect a long writer to sibling work and prevents one
fast executor from erasing it.

The scheduled successors supplied the correction sequence. Task
`01a0530a-ff8e-5d3b-de01-16d864868d75` ran on
`by-us-fmt-5-edge-3/g1`, container `786ae804bb97`, from 14:25:26Z to
14:25:47Z (20.92s) and reversed the dominant quantity to 122.19GiB over and
2.24TiB under across 2,083 networks. One exact -1MiB taskworker settle went
negative at 14:25:22Z—five minutes after the stale writer completed and four
seconds before the correction began—so it is aftermath of the stale mirror,
not damage caused by the corrective pass. Task
`01a0530f-eaf7-3701-855e-2a8cf6c2d32c` then ran on
`by-us-fmt-5-edge-1/g2`, container `06abfbe03c32`, from 14:30:48Z to
14:31:10Z (21.46s) and contracted to 40.69GiB over/46.04GiB under across 921
networks. By 14:33:59Z taskworker, API, and Connect all reported zero negative
lines in the trailing eight minutes. Task
`01a05314-d7d0-473e-9e5d-6140352a6b5c` supplied the terminal certificate on
`by-us-fmt-5-edge-4/g2`, container `3c0a752d4433`: it ran from 14:36:11Z to
14:36:29Z (18.00s), remained in band at 38.77GiB over/43.52GiB under across
847 networks, and all three emitters were still zero after the ingestion
allowance. The sequence therefore recovered without intervention, while the
recurring deployed fleet writer remains unsafe until the page-local additive
fix is rolled out.

The next sequence recreated the incident and supplied an executor control.
Task `01a05319-b85b-2d49-4564-7eb70075486c` ran on edge-3/g2 container
`4cf91fd25a2e` from 14:41:32Z to 14:59:35Z (1,082.77s). It rewrote 898,443
balances and reported 1.26TiB over-reserved plus 1.17TiB under-reserved across
2,237 networks; API and Connect each emitted negative-counter lines after the
apply. Its scheduled successor `01a0532e-dff2-7a25-199e-cb9e6e935865` moved
to edge-1/g1 and completed in 23.26s, immediately reversing the quantities to
1.18TiB over and 1.26TiB under across 2,290 networks. That fast inverse is both
the stale-write repair signature and an A/B executor control: fleet Redis and
PostgreSQL could complete the same deployed algorithm in seconds while the
edge-3 taskworker was co-resident with the long reaper, score rebuild, and
close checkpoint. The next scheduled pass
`01a05333-d6b7-6573-ec1d-0fac2c382c9f` moved again, to edge-0/g1, and ran from
15:10:02Z to 15:10:23Z (20.453s). It contracted the residual to 38.73GiB over
and 48.03GiB under across 873 networks. By 15:12:10Z taskworker, API, and
Connect all reported zero negative-counter lines in the trailing eight minutes,
and the next two samples remained quiet. That contraction plus the full
three-emitter ingestion window is the terminal recovery certificate for this
sequence; it does not make the recurring deployed fleet writer safe before the
page-local additive fix is rolled out.

The following recurrence reproduced the same causal inversion and exposed the
last cross-store race that remains after page-local reconciliation. Task
`01a0534c-7dce-2fa4-d40b-6e28adcc03c3` ran on edge-1/g2 from 15:37:00Z to
15:54:51Z (1,071.751s), rewrote 898,581 balances, and reported 2.25TiB
over-reserved versus 129.43GiB under-reserved across 2,035 networks. Four API
and five Connect releases then exposed exact -1MiB negative counters; the
first was at 15:53:29Z while the stale apply was still live, and taskworker
emitted none. Its successor
`01a05361-7b03-7f6e-7ab9-4d52336aceb8` ran for 24.248s and reversed the
dominant quantity to 131.74GiB over/2.25TiB under across 2,050 networks. The
next scheduled pass `01a05366-735f-cd75-31f1-7ee0f1586af8` completed in
20.751s, contracted to 46.26GiB over/49.31GiB under across 945 networks, and
taskworker, API, and Connect were all at zero negative lines after the full
following interval and ingestion allowance. This is another terminal recovery
certificate for the legacy incident, not evidence that its full-fleet writer
is safe. Two further scheduled passes stayed in band at 52.59GiB/45.38GiB and
45.04GiB/55.28GiB over/under, respectively, while every emitter remained
quiet; the convergence was durable across executor changes.

The next recurrence completed the same long-run/reversal/convergence chain at
16:50Z. Task `01a05375-52b2-59c3-8a72-36646de9060d` ran on edge-1/g1 from
16:21:36.597190Z to 16:39:30.633361Z (1,074.036s), rewriting 898,952 balances
and reporting 1.11TiB over-reserved plus 1.81TiB under-reserved across 2,331
networks. Eleven settlements then exposed negative mirrors: seven API and four
Connect lines from 16:35:31.032Z through 16:43:16.640Z; taskworker emitted
none. The first three occurred while the stale apply was live, and the last
three Connect results on one balance reached -62.98MiB, -191MiB, and
-319MiB. Corrective task `01a0538a-5d49-7938-0805-24eed3a0e8de` moved to
edge-3/g1 and completed in 17.020s, reversing the dominant quantities to
1.81TiB over and 1.11TiB under across 2,317 networks. The next scheduled pass
`01a0538f-3b89-ea12-7f55-39e0ae148398` moved to edge-0/g2 and completed in
20.841s, contracting drift to 46.12GiB over/36.47GiB under across 834
networks. API, Connect, and taskworker then each remained at zero negative
lines through 16:58:40Z, more than eight minutes after that pass and beyond the
log-ingestion allowance. This is the exact stale absolute-write inversion
followed by normal convergence; retain the page-local additive/no-op-skipping
writer and atomic release clamp. Alert grouping now preserves
`frame=site=settle` while redacting balance and contract ids, so separate
mutation sites cannot collapse into one opaque target.

The immediately following sequence proved that duration alone cannot clear
the legacy writer. Four scheduled passes from 16:55Z through 17:11Z completed
in 15.997–23.394s and stayed within 28.7–51.33GiB in either direction. Task
`01a053a7-c9ff-805e-78b8-9df21c8bf561` then ran on edge-1/g1 from
17:16:43.785282Z to 17:17:29.346654Z (45.561s) and abruptly corrected
59.78GiB over plus 536.69GiB under across 1,570 networks. Its successor
`01a053ad-1d8a-fe42-fdce-f6eef954abf6` moved to edge-3/g2, completed in
18.761s, and reversed the same dominant quantity to 525.76GiB over plus
62.9GiB under across 1,571 networks. The monitor rendered
`matched_reversal=true reversal_direction=under-to-over` and retained both
executor identities. No settlement happened to expose a negative counter.
The next scheduled task `01a053b2-0002-dfba-240e-b302d2ef7429` completed in
22.133s and contracted the aggregate to 50.5GiB over/30.4GiB under across 837
networks. This short-pass inversion is the exact synthetic aggregate
regression: skipping already-correct mirrors is required even when the full
fleet walk stays below 120 seconds. Another scheduled pass
`01a053b6-f72f-beb4-2a87-683c53a3fe89` then completed in 17.653s and stayed in
band at 31.29GiB over/52.27GiB under across 858 networks. API, Connect, and
taskworker each remained at zero negative lines through 17:41:58Z, more than
eight minutes after that pass and beyond the ingestion allowance. This is the
terminal recovery certificate; it does not make a later legacy absolute walk
safe.

One more short legacy sequence reproduced the same matched reversal before
the rollout boundary. Task `01a053c5-a2b7-053b-5583-e8ea35e63aee` completed
in 19.708s at 17:49:37Z with 19.37GiB over-reserved and 604.48GiB
under-reserved across 1,574 networks. Its successor
`01a053ca-8796-54e3-24ea-02bf8923b555` moved to edge-1/g1 and completed in
23.294s at 17:55:01Z, reversing the quantities to 619.08GiB over and
16.87GiB under across 1,615 networks. The following pass
`01a053cf-7ae3-752e-6e14-dba61deb77bc` completed in 16.821s at 18:00:19Z and
contracted to 29.53GiB over/51.05GiB under across 903 networks. API, Connect,
and taskworker remained at zero negative lines through 18:07Z. This is a
second short-duration proof that a quick full-fleet write can still clobber a
newer mirror; no-op skipping is part of the root fix, not only an optimization
for long passes.

The very next legacy pass escalated on the score-heavy edge-0/g1 executor.
Task `01a053d4-54d7-89b7-4910-8f757791cb0e` ran from
18:05:21.913100Z to 18:20:32.094955Z—910.182s—and rewrote 900,039 balances.
Its terminal aggregate corrected 1.58TiB over-reserved plus 553.29GiB
under-reserved across 2,076 networks. Two API and two Connect settlements
then exposed exact -1MiB counters from 18:20:49Z through 18:21:17Z;
taskworker emitted none in the first post-pass sample. This is pre-rollout
stale-snapshot damage, not merely scheduling latency. Recovery remains open
until a scheduled inverse and a subsequent in-band pass complete, followed by
a full quiet interval across all three emitters.

Instead of supplying that recovery, two consecutive scheduled passes landed
on the same old edge-0/g1 container and amplified the damage. Task
`01a053e6-d5e1-a07e-ca22-a9fe98395e34` ran from 18:25:35.186328Z to
18:44:36.869386Z (1,141.683s), rewrote 900,295 balances, and corrected
2.05TiB over-reserved plus 777.02GiB under-reserved across 2,469 networks.
Six exact -1MiB settlement negatives appeared across API and Connect from
18:44:15Z through 18:45:53Z, beginning while the apply was still live.

Its successor `01a053fc-e925-686d-b08a-fa82b2ee3864` ran from
18:49:41.687654Z to 19:08:43.856564Z (1,142.169s) on the same process. Its
terminal aggregate rewrote 900,556 balances and corrected 586.11GiB over plus
8.53TiB under across 2,562 networks. This was not a matched inverse: the
dominant under-reserved error grew far beyond the preceding quantities. A
redacted full-interval pull counted 160 API and 100 Connect negative
settlements, and the taskworker pull hit its 10,000-line cap within 14 seconds;
individual results approached -10GiB and no old-generation line reported
`clamped_to=0`. This is fleet-scale mirror corruption from repeated legacy
absolute writes, not independent lost creates. The log signal now pages at
100 negatives/minute for one service/site and uses standing streams so the
burst cannot fall between sequential snapshots. Recovery remains open until
the append-only schema gate passes, new taskworker generations run the
page-local additive/no-op-skipping reconciler, a scheduled aggregate returns
to the tens-of-GiB band, and all three emitters remain quiet for a full
following interval.

A later pre-deploy sequence supplied that terminal control while also
reproducing a short-duration clobber. Task
`01a05417-ef96-3dc9-4448-e5f849e7f296` ran for about 108 seconds on old
edge-0/g1 and ended at 19:21:00.946518Z with 185.27GiB over plus 267.55GiB
under across 1,596 networks. Its approximately 23-second successor
`01a0541e-3516-546c-9cb5-8c0c8c1cfa88` moved to old edge-1/g1 and ended at
19:26:25.921346Z with 268.32GiB over plus 186.55GiB under across 1,620
networks. The monitor preserved both executor identities and rendered the
near-exact 267.55GiB-under to 268.32GiB-over flip as
`matched_reversal=true reversal_direction=under-to-over`. Task
`01a05423-2a85-ac78-d438-085f46362b21` then moved to old edge-0/g2, completed
in about 21 seconds at 19:31:48.963077Z, and contracted to 42.95GiB over plus
47.14GiB under across 868 networks. The overlapping v71/v72 standing streams
recorded zero negative-counter lines across the reversal and contraction.
This is a complete legacy clobber/reversal/convergence chain; it clears the
individual sequence without making the old absolute writer safe.
One further old-generation pass on edge-3/g1 completed at
19:37:08.885440Z and remained in band at 36.24GiB over plus 46.47GiB under
across 842 networks, while every negative-counter stream remained quiet.

The next old-generation pass recreated a full stale-write sequence on the hot
edge-1/g2 container `06abfbe03c32`. Task
`01a0542c-fa1a-6ed7-5e90-19b29c5f76a8` ran from about 19:42:13Z to
20:04:07.468260Z (an authoritative rounded 1,314s), rewriting 901,144
balances and correcting 438.07GiB over-reserved plus 5.54TiB under-reserved
across 2,238 networks. The redacted raw taskworker pull peaked at 3,181
negative settlements in minute 20:05; the standing monitor windows retained
289, 207, 133, and 81/min during the decay rather than losing the burst at a
snapshot boundary. Its same-executor successor
`01a05445-aef3-991e-3ccd-e3557c314bf6` was not a fast recovery: it ran 402s
and ended at 20:15:53.180148Z with 6.17TiB over plus 386.85GiB under across
2,405 networks. The monitor correctly rendered the 5.54TiB-under to
6.17TiB-over pair as `matched_reversal=true`, and its next full stream window
paged on another 1,280 taskworker negatives/min. This is a causal inverse, not
convergence. The next task `01a05450-7164-89a5-becd-0ac85f54b68d` moved to
old edge-4/g2 and completed in about 19 seconds at 20:21:12.648396Z, providing
a peer-executor control and contracting the aggregate to 137.45GiB over plus
838.12GiB under across 1,529 networks. A following g1 task completed its
aggregate at 20:26:35.870259Z with 36.77GiB over plus 369.70GiB under across
1,290 drifted balances. That is continuing contraction, but its under-reserved
total remains above the 256GiB alert band and taskworker negatives recurred
during the rollout; keep the sequence open until a later scheduled aggregate
returns to the tens-of-GiB band and taskworker, API, and Connect remain at zero
for a full following interval.

The completed service-version rollout did not supply that recovery. With the
database still at 590 and therefore missing the version-594 balance/contract
index, task `01a0546b-c769-9420-2055-cd95e18fab76` timed out on edge-3/g1 at
exactly 1,800 seconds, retried on edge-1/g1, and reached a 1,799-second live
heartbeat before its task row advanced from one to two `context-canceled`
errors and immediately showed another fresh claim. During the second attempt,
the standing taskworker stream rose from 75 to 394 and then 270 negative
settlements per minute. Samples lacked `clamped_to=0`, directly proving that
the deployed release still used the old non-atomic release path; this is not a
regression in the current Lua fix. Migration lag made each old full-fleet pass
miss its deadline, while automatic rescheduling repeatedly exposed legacy
stale-write damage. Require migrations first, then a service build containing
the page-local additive reconciler and atomic release before interpreting a
later scheduled pass as post-fix verification. The exact 1,800-second
`context-canceled` boundary is containment, not evidence that this task needs
a larger `MaxTime`; the task-canary alert routes this family to §5.11 and §8.9
and explicitly preserves the deadline.

The third attempt on edge-4/g1 reached the same boundary. Its last retained
heartbeat was 1,777s at 22:22:17Z; by the 22:23:13Z task-canary sample the
row's `reschedule_error_count` had advanced from two to three with the same
`context canceled` error and already carried a fresh successor claim. The
standing taskworker stream recorded 8/min and then 4/min negative counters
across that boundary. A narrow task-id Loki lookup timed out while awaiting
headers, so the successor executor was not inferred from missing log results;
the PostgreSQL row is authoritative for the third failure and fresh retry.

The negative aftermath also revealed an irreducible ordering window: a
PostgreSQL settlement commits before its Redis mirror post. Even a bounded
additive reconciler can observe that committed settlement and correct the
still-reserved mirror to zero before the delayed release arrives. The release
would then decrement a missing key below zero. Current source routes both
normal settlement and quarantine release through one Lua command: it performs
`DECRBY`, deletes a zero or negative result atomically, and returns the original
negative value for a `clamped_to=0` diagnostic; a positive result preserves a
shorter precise TTL and caps only a missing/legacy-long TTL at 90 days. This
defense does not replace the page-local fix—the production TiB-scale matched
reversals still prove the old absolute writer—but it prevents the residual
commit/post race from leaving available balance overstated until another pass.

Diagnosis and recovery:

1. Read `finished_task` duration for `ReconcileNetEscrow` and the matching
   `[sm]reconcile net escrow` aggregate log. A long run plus a large
   under-reserved correction immediately preceding a fleet-wide burst proves
   this variant; do not attribute thousands of balances to independent lost
   creates.
2. Do not repeatedly apply the pre-fix reconciler as mitigation—it can recreate
   the drift it is meant to repair. A dry run is observational; any task pause
   or manual apply is an operations mutation and requires explicit authority.
3. The `netescrow` probe warns when taskworker's authoritative `eval active`
   heartbeat or a completed duration reaches 120s, and retains a completed
   precursor for 45 minutes; a quick corrective successor must not hide the
   clobber. A completed PostgreSQL task row supersedes the same run's lingering
   two-minute `eval active` log line; otherwise a just-finished overrun is
   falsely described as still active. If the successor itself crosses the
   limit, retain the latest completed precursor duration and age alongside the
   live heartbeat. Retain the heartbeat's host/generation/container as well:
   recurring tasks can land on different fleet members, and a fast member does
   not prove the deployed algorithm fixed. Do not calculate live duration from
   `pending_task.run_at`: it is the due time, while `claim_time` is a moving
   heartbeat. Current code reads PostgreSQL
   reservations immediately before each bounded balance page and applies
   `INCRBY(delta)` in Lua, deleting an exact zero and otherwise restoring the
   fallback TTL. It emits no Redis write at all when the observed mirror already
   equals PostgreSQL; the pre-fix loop still issued a `SET` or `DEL` for every
   one of roughly 898k balances, so even a logically no-op pass created a full
   fleet write storm. A concurrent mirror change between Redis `GET` and
   correction is therefore preserved; the reconciliation snapshot window is
   bounded to one fresh page rather than the whole fleet scan. A separate
   PostgreSQL-commit/Redis-post window still exists, so settlement and
   quarantine release use the atomic decrement-and-clamp Lua command rather
   than a bare `DECRBY`.
4. The deterministic regression changes the mirror between observation and
   correction and requires the additive result to retain that write. A second
   regression gives an already-correct mirror a 30-minute TTL and requires the
   apply pass to preserve it, proving the no-op balance did not receive the
   old 90-day rewrite. A third starts at 100 bytes, releases 40, and requires
   the positive 60-byte value plus the original short TTL; it then releases
   100, requires the diagnostic return value `-40`, and proves the key is
   absent in the same command. The page query requires the online
   `transfer_escrow(balance_id, contract_id)` index and must keep requested
   balances as the outer side of a lateral range lookup; the index's presence
   alone does not prevent PostgreSQL from choosing a whole-table scan for a
   large scalar-array predicate. Apply the index migration before rolling
   taskworker and retain the lateral optimization boundary. After rollout,
   require recurring runs to return
   to their short band, aggregate drift to converge, `netescrow-negative` to
   reach zero, and the lossless insufficient-balance counter to remain below
   its incident band. Monitor alert samples retain the non-sensitive `site` but
   redact balance/contract ids. The log alert also carries the full causal
   discriminator: negative lines are mutation-site aftermath, their rate is not
   an overwritten-byte count, and recovery requires a sub-120-second scheduled
   pass below the 256GiB aggregate threshold plus a full quiet interval across
   taskworker, API, and Connect after allowing for log-ingestion delay.

The first migration-complete pass supplied the post-fix discriminator on
2026-08-31. Schema head 597 included the version-594
`transfer_escrow(balance_id, contract_id)` artifact, and running fleet version
`2026.8.30+1033129380` contains the page-local additive reconciler plus atomic
release clamp. Its 295-second run ended at 03:00:14Z after scanning 903,527
balances, but corrected only 4.96GiB over-reserved and 4.04GiB under-reserved
across 131 networks. That is a freshness overrun and possible catch-up/storage
cost, not the old TiB-scale matched-reversal signature; duration alone must not
label the executing algorithm pre-fix.

The following 20-minute bounded host-journal read found 27 negative settlement
diagnostics, all on edge-3 taskworker g2 and all carrying `clamped_to=0`; the
per-minute counts were 5, 5, 1, 14, and 2, with zero on the other service
hosts. This proves the current atomic containment deleted each bad mirror while
retaining evidence of the smaller PostgreSQL-commit/Redis-post ordering window.
It is still a warning until a full interval is quiet, but it is not a reason to
redeploy fixes that the schema, version tag, aggregate, and clamp field already
prove present. Persistent residual rate requires tracing the commit/post
ordering; a missing clamp or renewed >=256GiB reversal reopens the old-writer
or regression branch.

Repeated post-migration 177–449-second passes exposed a separate read-path cost
without reopening the integrity incident. `pg_stat_statements` initially
attributed 2,128 calls and 16,758,829.6ms total execution time to the bounded
open-reservation join: 7,875.4ms/call lifetime mean and 74,031.3ms maximum.
The balance-id keyset page, by contrast, averaged 4.601ms across 968,502 calls.
A bounded 1,000-ID production diagnostic reached the 30-second read-only
statement timeout and was confirmed absent afterward. That diagnostic used a
parallel bitmap heap plan and showed that merely shrinking a page was not a
demonstrated fix, but it did not reproduce the deployed 10,000-ID plan.

The exact 10,000-ID read-only `EXPLAIN` on 2026-08-31 supplied the missing
discriminator. At 1,081,742,992 estimated `transfer_escrow` rows (153.5GiB heap,
257.8GiB including indexes), PostgreSQL 18.4 rejected the published 50.4GiB
`transfer_escrow(balance_id, contract_id)` index and selected a four-worker
`Parallel Seq Scan` for every reservation page, estimating 22,390,004 matching
rows per worker. The 5,000-ID control still selected the balance index, while
10,000 crossed into the sequential plan. A subsequent 210-second reconcile
made 91 reservation calls, exactly the roughly 898k active-balance page count;
the scalar-array query therefore asked PostgreSQL to walk the billion-row
history about 91 times in one task. Index presence was not the access-path
guarantee the first remediation assumed.

The no-migration root fix keeps the 10,000-balance freshness/correction page but
expresses requested balances as `unnest($1::uuid[])` on the outer side of a
`CROSS JOIN LATERAL`. An `OFFSET 0` optimization boundary prevents the planner
from flattening it back into the scalar-array join. The same production
`EXPLAIN` then has a 10,000-row function scan feeding a nested loop and one
`transfer_escrow_balance_contract` range scan per requested balance; because
each active balance is visited once, a reconcile can no longer multiply a
whole-table scan by its page count. PostgreSQL's estimate remains pessimistic
because historical escrow ownership and the mostly-empty active-balance set
are not correlated in single-column statistics, but the physical scan bound is
now structural. The query returns the same grouped bytes and keeps the fresh
page-local additive correction window.

`netescrow` classifies legacy 10,000-ID `ANY` calls separately from bounded
lateral calls while retaining combined lifetime/adjacent timing. A deterministic
query-shape regression requires `unnest`, `CROSS JOIN LATERAL`, the per-balance
equality, and `OFFSET 0`, and forbids restoring `balance_id = ANY`; synthetic
signal coverage prevents an already-deployed lateral plan from being
misdiagnosed as the legacy scan. Verification is new bounded-lateral delta
calls with zero new legacy-`ANY` calls, reservation-page adjacent mean below one
second, full passes below 120 seconds, small aggregates, and a full quiet
negative-counter interval. A covering index remains a separate option only if
the bounded-lateral profile is still slow in an isolated production-shaped
benchmark; it is not required for this root fix and no migration was added.

An independent live-writer variant appeared during the same observation
window: API emitted 15–18 `[redis][ttl]` lines/minute for `EXPIREAT` on
`{escrow_<id>}net`, with roughly 36,306 days remaining. PostgreSQL showed this
was not nanoseconds or a malformed timestamp: 574 active paid/Pro balances were
intentionally created with 36,501-day lifetimes, totaling roughly 6.31PB of
remaining data. The old mirror writer copied `balance.end_time + 30d` directly
into Redis, so a hot lifetime balance was refreshed into 2126 by every API
generation. Do not shorten or delete those durable balances. The Redis mirror
now uses the earlier of `end_time + 30d` and a rolling 90-day fallback. Early
mirror expiry is already a supported state: the recurring reconciler compares
the missing zero with PostgreSQL reservations and recreates the counter. The
deterministic regression creates a 100-year balance through the real contract
path and requires its Redis TTL to remain within 90 days. Alert samples retain
`expireat` and `{escrow_<id>}net` while redacting the balance id.

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
| selection-empty | pg+redis | 2.9 eligible Public providers vs decoded normal score-cache count | eligible > 1,000 AND exported == 0 for 2 probes | ForceMinimum count; egress-test config; health/location row counts; last score completion |
| node-mem-critical | redis | used/maxmemory | > 92% for 2 min | dataset vs clients split; top families |
| oom-writes | pg+logs | OOM class in task errors or logs | any sustained 2 min | node attribution from error text |
| active-pileup | pg | 1.3 active client backends | > 100 for 2 min | top query_ids by count; wait-event split; db host load |

Tier-1 (warn):
| id | source | check | threshold |
|---|---|---|---|
| task-parked | pg | all `error_count>0` rows grouped by task family before reporting; payload separates parked, live-retrying, and total rows | any family (never limit raw rows first) |
| task-overdue | pg+task logs | one worst row/task family with live claim; due age over `min(2*p95,max(4*p50,20m))`, then matching `eval active` confirms actual elapsed time | any (median cap prevents repeated long failures from polluting p95; exact task/executor identity retained) |
| task-duration-regression | pg | run duration vs 7-day p95 per function | > 2× |
| idle-in-tx | pg | 1.3 count / oldest | > 100 / > 30 min |
| node-mem-high | redis | used/maxmemory | > 85% for 5 min |
| mem-skew | redis | max/median used across nodes | > 3× |
| ttl-leaks | redis | 3.3a `INFO keyspace` average TTL | > 2 years; longest intentional family exception is 395 days |
| score-byte-dominance | redis | 3.3b bounded `MEMORY USAGE SAMPLES 1` family share on fullest node | node ≥85% AND score ≥50% / ≥128KiB sampled bytes |
| client-buffers | redis | used_memory_clients | > 25% of used or > 2G |
| clients-spike | redis | connected_clients own-node step and fleet shape; trip battery groups `CLIENT LIST` cohorts | +50% in 10 min or >3× fleet median for 2 probes |
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
| close-duration-overrun | task logs+pg | 2.6a live heartbeat or completed CloseExpiredContracts duration | >= 120s; retain completed precursor 45 min |
| reboot-task-collision | host journal+pg | 2.13 fresh non-terminal task heartbeat at previous-boot boundary | >= 120s during a boot in the last 20 min |
| stats-landmine | pg | pg_stats n_distinct=1 on transfer_contract.open, or any open-partial index reltuples=0 after analyze | daily check |
| connects-rate | pg | 2.7 new-connection rate vs same window 1h ago | < 50% sustained 5 min |
| connects-storm | pg+deploy | 2.7 new-connection rate and disconnected lifetime vs pre-event window | > 2.5x for 3 min; payload includes binary/config generations and same-tag restart times |
| retention-fanout | pg | 2.10 active query id `-3312164664690273449`, plus durable `AdvancePayment` deadline correlation | one execution > 30s or >= 2 concurrent for 2 probes; between retries, exact query >= 100k rows/call plus retained 120s cleanup signature |
| grafana-plugin-unregistered | logs | 11.15 `[plugin.notRegistered]` scheduler/query failures | any |
| pgbouncer-write-stall | logs+host | 2.11 app write timeout to `:6432` | any route/host cluster sustained 2 min |
| worker-memory-skew | mimir | 2.12 fresh taskworker allocated heap by host/block/instance | >= 8GiB and >= 4× fleet median for 2 probes; sparse-fleet fallback >= 16GiB |
| worker-cpu-allocation-churn | mimir+task logs | 2.12a paired one-minute taskworker CPU/allocation rates by host/block/instance | >= 3.8 cores and >= 256MiB/s and both >= 8× fleet medians for 2 probes |
| selection-stale | pg | 2.8 UpdateClientScores completion gap | > 90 min (page at > 3h — ttl cliff at 5h) |
| contract-balance-failure-rate | Mimir/Grafana | `urnetwork_connect_contract_failures_total{cause="insufficient_balance"}` 5-minute rate | > 4,000/min for 5 min |
| missing-origin-rate | Mimir/Grafana | `urnetwork_connect_contract_failures_total{cause="missing_companion_origin"}` 5-minute rate vs its ~90/min background | > 500/min for 5 min |
| keyevent-config-drift | redis | 9.1 notify-keyspace-events class SET per node | any node divergent from the fleet (all-off = healthy dark state) |
| pubsub-conn-shape | redis | 9.1 CLIENT LIST TYPE pubsub count per node | warn > 300; page > 1,000 (O(clients) = the v1 outage shape) |
| required-vault-resource | logs+route | 8.7 `Resource not found in vault` plus dependent-route probe | any active generation; payload includes resource, route, config generation |
| source-attribution | synthetic+logs | §8.8 dual-stack `/my-ip-info` family/source check plus UR-header resolver warnings | any mismatch for 2 probes, or any legacy untrusted-peer line after rollout |
| migration-schema-drift / migration-behind | pg | §8.9 successful `migration_audit` head cross-checked against every published schema artifact | page when any artifact at or below the recorded head is absent; warn while the database head trails this source tree |
| netescrow-reconcile-overrun | task logs+pg | 5.11 live heartbeat or completed ReconcileNetEscrow duration | >= 120s; retain completed precursor 45 min |
| netescrow-large-drift | task logs | 5.11 reconcile aggregate over/under-reserved correction | either direction >= 256GiB in the last 15 min; payload labels an adjacent opposite-direction quantity within 20% as a matched reversal |
| netescrow-negative | standing logs | `[netescrow]negative counter after` | any warns; >=100/min/service/site pages; payload includes site (never raw balance/contract ids) |
| proxy-public-handshake | synthetic+host | 14.5 protocol handshake vs internal readiness | any host/block with internal 200 but public SOCKS/HTTP/HTTPS handshake failure for 2 probes |
| policy-route-drift | host | 14.5 networkd/LB start clocks plus Warp table/rules | networkd newer than the transparent LB and any owned public route or source/fwmark rule missing |
| edge-auto-upgrades | host | 14.5 APT periodic config and unit masks | any edge with APT periodic enable nonzero or an apt-daily timer/service not masked |

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

### 8.5a Controlled APT left Docker/containerd split across versions

`policy_rc_d: 101` deliberately suppresses package-triggered service restarts.
That is unsafe if a controlled APT pass upgrades `containerd.io` but the
playbook returns without a rolling runtime restart. On 2026-08-28 edge-0,
edge-1, and edge-4 had the 2.3.3 binaries installed while their live
containerd process was still 2.2.6; `readlink /proc/<pid>/exe` ended in
`(deleted)`. Every subsequent deploy failed before application startup:

```
docker run ...                                            # exit 125
docker inspect <created-container> --format '{{.State.Error}}'
# failed to create TTRPC connection: unsupported protocol: \b\x03\x12Yunix
ctr version                                               # Client 2.3.3, Server 2.2.6
```

This is a containerd bootstrap-protocol mismatch, not a Warp poll, port,
drain, image, or application failure. The 2.3 shim writes a protobuf bootstrap
result that the still-running 2.2 daemon cannot parse. Existing containers
remain `Up`, so public traffic stays on the old generation while every
`warp-main-*` worker retries. The unit's `ActiveState=active`, `NRestarts=0`
is therefore expected and misleading. Read the Docker daemon journal for the
real error. Pre-fix Warp builds retain only exit 125; the corrected `outAndLog`
adds a bounded, escaped stderr excerpt to the same journal record.

Recovery is a controlled rolling restart/reboot of the container runtime, one
host at a time. Restarting Warp units alone cannot repair the daemon/shim
pairing and increases retry churn. Before advancing to the next host require:

1. Docker and containerd client/server versions match;
2. neither daemon executable is `(deleted)`;
3. one newly created container reaches `Up` and its Warp journal records
   `Deploy success`;
4. the old service generation drains normally.

Observed recovery on 2026-08-28: edge-4, edge-0, and edge-1 took 432s, 578s,
and 488s respectively to reboot and return to Ansible. After each host returned,
Docker was 29.7.2/29.7.2, containerd was 2.3.3/2.3.3, all five API blocks on
`2026.8.28-outerwerld+1031122680` returned 200 from their host allocation, and
the post-boot daemon/Warp journals contained no recurrence. Keep the rolling
reboot timeout above these slow physical boot times. A final fleet probe found
matching runtime pairs and zero failed Warp units on every reachable edge;
edge-5 was already known offline and was not part of the recovery. Strict
public probes also made five IPv6 requests to `api-v6.bringyour.com/my-ip-info`
and received the exact bound source address every time.

The edge APT playbook now snapshots all Docker/containerd/runc packages around
the controlled upgrade, probes daemon liveness, executable paths, and
client/server versions, and performs a throttled reboot when either package
state changes or the probe detects an inconsistent runtime. It then refuses to
continue unless the strict probe passes and Warp launches a main-environment
container. A `/var/run/reboot-required` check alone is not sufficient: this
userspace package transition does not have to create that file.

### 8.6 Config-generation restart wave — binary version alone is incomplete

Warp reports two independently useful identities:
```bash
warpctl ls versions main api beta g1 g2 g3 g4 --sample
warpctl ls versions main connect beta g1 g2 g3 g4 --sample
```
Read both the `<service> versions` and `config versions` sections. A block can
serve the old binary with the new config generation, and same-image containers
can be recreated while a new binary canary is also present. The 2026-08-27
signature was:

- api beta/g1 served `2026.8.27+1030474020`; api g2-g4 and all sampled connect
  blocks still served `2026.8.26+1029569170`;
- every sampled block reported config `2026.8.27+1030474020`;
- `docker ps` showed fresh same-old-image api/connect containers across hosts,
  plus new-version beta/g1 canaries and clean `Exited (0)` drain ancestors;
- new connects and contract creation jumped with those creation times (§2.7),
  while `/hello` and the unauthenticated connect handshake remained available.

This is neither proof the new binary is fully deployed nor a crash loop. Emit
one control-plane event keyed by `(service, binary generation, config
generation)` and annotate every rate change until all three converge:

1. active public samples report the intended binary AND config generation;
2. old/same-tag drain containers disappear on every reachable host;
3. the reconnect rate/lifetime and open-contract set return to their
   pre-rollout bands.

An unreachable known host (for example edge-5 during this audit) belongs in
the coverage field. Exclude it from denominators only when the operator has
explicitly declared it offline; never silently turn an SSH failure into a
healthy sample.

### 8.7 Lazy required-vault resource — green startup, route-specific 500

`Resolver.RequireSimpleResource` is evaluated lazily by some controllers. A
missing resource therefore does not necessarily crash startup or fail
`/hello`; the first request to its route panics inside the router and returns
500. On 2026-08-29 main omitted `verify.yml` by design because `st.yml` had the
unreleased subnet disabled, yet unconditional `/verify/*` handlers still
reached the required-resource loaders: `/verify/keys` and `/verify/stats`
returned 500 while `/hello` remained 200.

- Log signal: `Resource not found in vault (<resource>.yml)`, grouped by
  resource + route + config generation. One occurrence is a deterministic
  configuration defect, not an intermittent application panic. The monitor
  preserves those three fields as the alert frame instead of folding two
  broken routes into one service-wide count, and the rendered Markdown carries
  the disabled-versus-enabled mechanism, action, and five-minute verification.
- Feature-state discriminator: an intentionally disabled optional subsystem
  must stop at its HTTP boundary. The `/verify`, `/verify/keys`,
  `/verify/stats`, and `/verify/proofs` handlers now return 503 with
  `Retry-After` before body/query parsing, vault access, or database work when
  `StEnabled()` is false. That is healthy fail-closed behavior. Do not generate
  or commit Ed25519 seeds or an egress hash key merely to make a disabled
  route return 200.
- Apply the same gate to background work. Main also had a
  `RefreshVerifyProxyEgress` RunOnce row that reached 940 failures
  (`Interrupted: Done` and an exact 900.05s `Interrupted: context canceled`)
  even though verification was disabled. Verification task seeding and
  Post rescheduling now require `StEnabled()`, and InitTasks deletes stale
  Sweep/Rollup/Retention/Egress rows when disabled. Otherwise an old pending
  row survives every deploy and keeps retrying even after route handlers are
  fixed.
  A later independent retry on edge-0/g2 reached its exact 900.00-second
  boundary at 17:42:41.335251Z with `Interrupted: Done`, increasing the same
  row from 946 to 947 failures before its fresh claim expired and it parked
  again. That recurrence confirms a durable ungated RunOnce chain rather than
  a transient route request; retain the startup reap and all execution/Post
  guards, and do not raise the task deadline.
- Enabled-subsystem discriminator: if `StEnabled()` is true, absent or invalid
  `verify.yml` remains a hard release failure. Provision its signing and hash
  material through the supported vault secret mechanism, validate that its
  profile/policy hash matches `st.yml`, and retain historical public key IDs.
- Deployment gate: enumerate every resource declared required by newly added
  routes, confirm it exists in the mounted vault generation, and probe at
  least one dependent route. A liveness-only canary cannot cover this class.
- Mixed generations matter: sample enough times to reach every active binary
  generation. A route may be 404 on an old binary and 500 on a new binary;
  neither result proves the intended route is healthy.
- Recovery requires zero new missing-resource lines for five minutes on every
  active generation. Require 2xx for an enabled subsystem; require the
  documented 503 response for one intentionally disabled. Restarting an old
  process without either gating the route or deploying the enabled resource
  reproduces the failure.

### 8.8 Source attribution — liveness can be green while every client is the ingress
Probe: `source-attribution`

Probe `api-v4.bringyour.com/my-ip-info` over IPv4 and
`api-v6.bringyour.com/my-ip-info` over IPv6 from a runner whose public source
addresses are known. Require all three properties independently:

1. both responses are 2xx;
2. each returned `info.ip` has the same address family as the connection;
3. each returned address equals that runner's public source address, not an
   API or Warp address.

On 2026-08-27 both endpoints were live but returned `65.49.70.82`, the Warp
peer. API logs showed the supplied `X-UR-Forwarded-For` being rejected because
the ingress was outside an undeployed trusted-CIDR setting. The application
therefore collapsed every user behind Warp onto one address; IPv6 also became
`null` on ur.io because its IPv6 request returned an IPv4 address.

The fleet contract is now singular: Warp overwrites
`X-UR-Forwarded-For: <bracket-safe-ip>:<source-port>`, strips
`X-Forwarded-For` and `X-Forwarded-Source-Port`, and API/Connect ignore those
alternate headers. There is no trusted-proxy environment setting. This makes
backend isolation a security invariant: allocations must remain unreachable
from public networks, because a direct caller that can reach one could supply
the UR header. Check public port exposure whenever this contract changes.

Healthy recovery is the correct known address from both family-specific
endpoints on every active generation, no new malformed-value resolver lines,
and no legacy untrusted-peer lines. `/hello` alone proves none of this.

### 8.9 Append-only migration coherence — a numeric head can hide skipped schema
Probe: `migrations`

Migration versions are a published production protocol. Once a migration has
run anywhere, its slice position and version must never move: corrections and
new work are appended after the published sequence. A successful numeric head
alone is not sufficient evidence because a source edit can insert migrations
before that head. The runner will then skip the newly inserted work and can
replay old non-idempotent migrations under later numbers.

On 2026-08-30 production recorded successful version 590, with the first three
competition artifacts present. Four escrow/retention migrations had been
inserted before those published entries in source, shifting the competition
sequence to 592–596. A rollout from that ordering would have skipped the new
schema through 590 and then attempted to recreate existing competition tables.
The root fix restored the published positions and appended the new work.

The 2026-08-30 rollout also exposed a separate activation-order defect. While
production's successful head remained 590, every sampled taskworker g1 block
was already running binary `2026.8.30+1033129380`, whose source requires 597;
g2 still ran the older binary with the new config generation. The shared
startup readiness check proved only PostgreSQL connectivity (`SELECT 1`) and
Redis PING, so a schema-dependent binary could become ready and claim work
before its migrations. Startup readiness now reads the successful
`migration_audit` head and rejects a binary when the database is below
`server.MigrationCount()`. A database at or above the binary's requirement is
accepted deliberately, preserving rollback to an older binary after
append-only migrations. This gate prevents future activation-order failures;
it does not make an already active mixed-generation rollout safe, so retain
the independent monitor alert until the current head and all artifacts reach
597.

A follow-up entry-point audit found that the first gate was not yet universal.
Taskworker and Connect used the shared migration-aware check, but API retained
a private `SELECT 1` check and started competition-metric database queries,
stats upload, and its OAuth reaper before that check; MCP warmed and served
without invoking startup readiness at all. API now delegates to the shared
check and starts those background components only after it passes. MCP now
gates warmup through the same latched check and preserves a not-ready error
across SIGTERM. Deterministic lifecycle tests prove failed or canceled
readiness cannot activate API background work or MCP warmup. Audit every new
schema-dependent service entry point against this same activation boundary;
adding a healthy `/status` route alone is not a migration gate.

At the rollout's promised 21:15Z completion boundary, all sampled taskworker,
API, Connect, and LB blocks reported binary `2026.8.30+1033129380`, but the
successful database head still reported 590. This was no longer merely a
mixed-generation interval: the complete sampled fleet had activated seven
migrations ahead of PostgreSQL. The long current-code net-escrow run supplied
one direct consequence. Its page-local query requires version 594's
`transfer_escrow_balance_contract` index; without that artifact, each bounded
page can rescan the large escrow history. The run reached its exact
1,800-second deadline at 21:20:46Z, was rescheduled with `context canceled`,
and the same task ID was already 337 seconds into its retry on another edge by
21:26:38Z. `RemoveCompletedContracts` independently began failing on the
missing version-595 `account_payment.contract_retention_cursor` column, with
seven reschedule errors observed by 21:26:22Z. These are direct schema-lag
failures, not stale rollout telemetry. A successful binary/config rollout
therefore does not satisfy the release gate. Require the migration phase and
artifact probe to finish before accepting service-version convergence.

The controlled migration then completed normally: audit versions 594 and 595
committed at 00:52:40Z, 596 at 00:52:58Z, and 597 at 00:57:22Z; a read-only
00:57:48Z check required head 597, every success bit, and every required
artifact before declaring the schema coherent. Recovery distinguished schema
repair from deployment repair. `RemoveCompletedContracts` cleared its missing-
column error and completed at 01:13:06Z, although its first backlog pass took
308s. Schema-object-missing `AdvancePayment` rows drained from 18 to zero under
normal retry by 01:51:59Z; the remaining 683 rows were only wallet-insufficient
(673), processor-invalid-destination (6), and processor-rate-limit (4), proving
those external outcomes were independent of migration coherence. The new
escrow index reduced successive `ReconcileNetEscrow` runs from 1,136s to 138s
and 137s, but the old absolute-write generation stretched back to 313s beside
close load. Thus head 597 is a
necessary release gate, not the application root fix: deploy the page-local
additive reconciler, bounded close/retention paths, and startup gates, then
verify their task durations and error cohorts rather than editing task rows.

This is the version-to-artifact contract checked by the probe:

| Version | Required published artifact |
|---:|---|
| 588 | `competition_round` |
| 589 | `competition_job_immutable_guard()` |
| 590 | `competition_round.providers_sha256` |
| 591 | `competition_round.epoch_number` |
| 592 | `competition_job.api_image_digest` |
| 593 | `competition_candidate_review` |
| 594 | `transfer_escrow_balance_contract` |
| 595 | `account_payment.contract_retention_cursor` and `contract_retention_pending` |
| 596 | `account_payment_contract_retention_pending` |
| 597 | `transfer_escrow_sweep_payment_contract` |

Page immediately as `migration-schema-drift` when the successful audit head is
at or above an artifact's version but that artifact is absent. Warn as
`migration-behind` while the audit head is below this source tree's required
head. The deployment gate is strict: run migrations from the exact service
commit, require version 597 and all ten artifact checks, and only then activate
dependent APIs or taskworkers. Never edit `migration_audit` or create objects
by hand merely to silence the probe; repair the append-only migration stream
and let its normal runner advance the database.

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
Probe: `redis-keyevents`

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

The observability PLANE itself: grafana + loki + mimir behind a Go front
(`warp/grafana/main.go`), ONE bundled service `warp-main-grafana-*` on 6-of-7
lb/host_services hosts (edge-0/1/3/4, crisp, fireside; **edge-5 offline**). The
front is PID1 and supervises the children via `warp.Child` (restart-on-exit,
plus restart-on-unready for loki and mimir since 2026-08-18, 11.13).
**alloy is NOT a container child** — host-managed fluent-bit replaced it, and
the container runs only warp-grafana + loki + grafana + mimir (`docker exec $c
ps -eo pid,comm`). The shipper is therefore a HOST unit on every warp host,
including the ones with no grafana bundle (11.14).
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
systemctl is-active fluent-bit                                                # shipper (HOST unit, 11.14)
```
HEALTHY BAND: `up=1 front=200 loki=200 mimir=200 graf=200 svc>0 restarts=0/0
shipper=active`. **Probe the CHILD `/ready` ports, not only the front proxy**
(11.3 sends you through the front for the bind paradox — do both). `/ready` is
the only field that carries child MODULE state, and 11.13 is invisible without
it: a loki whose query modules never started answers `/loki/...` 200 through
the front for `status/buildinfo` while every actual query hangs.
`svc` = distinct `service` labels loki knows = proof of BOTH log ingest and
cross-host read (label fixed 2026-07-19: it is `service`, not `warp_service`;
healthy main knows 10 — api app config-updater connect grafana lb mcp proxy
taskworker web. The labels api defaults to a recent window: pass explicit
start/end before reading an empty result as "nothing was ever ingested"). Any
field off names a class below.

### 11.2 up-count & child restarts — overlap vs crash-loop
- `up` = running grafana containers. **`up>1` = redeploy overlap** (old container
  draining) — normal for a few minutes, STUCK for hours = the poll deadlock (11.7).
- Child restarts from the front's supervisor:
  `docker logs --tail 200 $c | grep -c '\[loki\]exited'` (also `[mimir]`).
  **There is no `[alloy]` child** — grepping it always returns 0 and proves
  nothing; the shipper is a host unit (11.14). Since 2026-08-18 a second line
  also restarts a child: `[loki]unhealthy for <d>. Restarting.` (11.13) —
  a climbing count there means loki/mimir keep failing `/ready`, not that they
  keep exiting.
  Stable = **0**; nonzero-and-climbing = crash-loop. **CRITICAL: front + grafana
  read 200 while loki/mimir crash-loop** — the front supervises the children, so
  the container still passes its readiness poll and a broken build still
  "deploys". Always check restarts, never trust "container Up" alone. This has
  a quieter sibling: the child process is up, never exits, and only SOME of its
  modules are dead — 11.13. `/status` gates on child readiness since 2026-08-18,
  which closes both for new deploys, but a container that latched ready before
  going bad still reads `front=200`.

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
**`/mnt/data/minio`**, on edge-6 172.28.208.177). Read the data dir from the
host, never from memory — it is `MINIO_VOLUMES` in `/etc/default/minio`
(`systemctl cat minio` shows `ExecStart=... $MINIO_VOLUMES`). This doc said
`/data/minio` until 2026-08-18; that path does not exist, so every recipe below
returned empty at it and read as "storage is empty" — the exact false alarm
this section exists to prevent. A `du` that prints NOTHING (rather than `0`)
is the tell that you have the wrong path, not an empty bucket.
```
minio_data=$(grep ^MINIO_VOLUMES= /etc/default/minio | cut -d= -f2)     # do not hardcode
du -sh $minio_data/loki $minio_data/mimir                              # growth = writing
find $minio_data/loki -name xl.meta -printf '%T+\n' | sort | tail -1   # fresh mtime = live
ls $minio_data/loki/fake | wc -l                                       # loki CHUNK dirs (fake=anon tenant)
find $minio_data/mimir -name meta.json | wc -l                         # mimir FINALIZED blocks
```
loki flushes on `chunk_idle_period`/shutdown; **mimir uploads only after a ~2h
TSDB block boundary** — an empty mimir bucket right after a healthy start is
EXPECTED, not a fault (populates on the next boundary). An empty/stale loki bucket
while loki is crash-looping = writes stopped (11.2/11.4), not a storage bug.

The S3 API must be reachable on both edge-6 addresses: LAN
`192.168.51.193:23900` and management overlay `172.28.208.177:23900`. Do not
infer this from `minio.service=active`; an address-specific bind can leave one
path healthy and refuse the other. Check the configured bind, listening socket,
and both HTTP health paths together:
```
systemctl show minio -p ActiveState -p SubState -p NRestarts -p ExecMainStatus
grep -E '^(MINIO_OPTS|MINIO_VOLUMES)=' /etc/default/minio
ss -lntp | grep ':23900'
curl -fsS --max-time 5 http://192.168.51.193:23900/minio/health/live
curl -fsS --max-time 5 http://172.28.208.177:23900/minio/health/live
```
Desired `MINIO_OPTS` binds the S3 API as `--address :23900` and keeps the
administrative console loopback-only. The Ansible deployment must fail unless
both health requests return 200. On 2026-08-20 the process was healthy but used
`--address 192.168.51.193:23900`; LAN health returned 200 while the overlay
connection was refused. That is a bind/config split, not a route, firewall, or
data-store failure. Re-run `xops/main/ansible/run-minio.sh` after changing the
vault environment; a source-only vault change does not restart the service.

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

### 11.7c Drain STARVATION — containers pile up while the poll passes (2026-08-11)
A second, more common way old containers survive, and it looks nothing like
11.7: the deploy fully succeeds. The poll passes, the DNAT is repointed at the
new front, `Found overlapping containers (...)` is logged with the full list —
and then nothing. Containers accumulate one per deploy (observed 5 deep on
edge-3; `up=5`, `lokis=2`, `mimirs=2`).
```
journalctl -u warp-main-<service>-<block> --since -2h | grep -E 'Found overlapping|Draining'
#   Found overlapping containers (...) a, b, c   <- present, repeatedly
#   Draining N overlapping container(s)          <- NEVER printed
lsof /srv/warp/<env>/warpctl-host-drain*.lock    # W = the holder, plain u = queued
```
- TELL: `Found overlapping` with no matching `Draining` line and no
  `docker container stop` in the journal. The drain goroutine is blocked
  *before* its first log line, which is the host-drain-lock acquire.
- ROOT: the stagger lock was ONE file per host shared by every service. The wait
  bound is `DrainTimeout + 5m` = **65m**, and a connect drain legitimately holds
  it for up to `DrainTimeout` waiting on client connections, so grafana's drain
  queued behind it and was still queued when the next deploy replaced it. Seven
  warpctl workers were stacked on one lock file. Fixed 2026-08-11: the lock is
  scoped per env+service (`warpctl-host-drain-<env>-<service>.lock`), which
  preserves the original intent — the capacity the stagger protects is per
  service, so only the groups of one service need to take turns.
- **`systemctl restart` does NOT clear a pileup — it adds to it.** The unit stop
  deliberately leaves containers running ("restarting warpctl should not
  interrupt running services"), so a restart just deploys one more. Measured
  twice: `up 2→3`, then `3→4`. To clear one by hand, read the DNAT front target
  (`iptables -t nat -S WARP-MAIN-<SERVICE>-<BLOCK> | grep 'dport 7176'`), keep
  the container whose `WARP_PORTS` `80:` matches it — that is the one the lb is
  routed to, so there is no gap — and `docker stop -t 150` the rest.
- WHY IT MATTERS beyond waste: every container runs its own alloy AND its own
  loki, alloy pushes to the shared reuseport `local_port`, so log data scatters
  across N loki instances per host while the ring exposes ONE entry per host
  (11.7b). Queries reach a single instance and `warpctl logs <env> <service>`
  returns EMPTY. Metrics survive it; logs do not.

### 11.8 systemd unit port/forward-alias baking (WARP_PORTS staleness)
`warpctl service run` reads ports from `--portblocks` and aliases from
`--forwardports` BAKED into the unit at `create-units` time, NOT live
services.yml. Symptoms: grafana front panics `ring port 6490 must be declared
... Missing host port for 6490`, or LB panics `Missing host port for service
port 8053` while converting the historical nginx config that first added
UDP/8053. During the v21 migration the equivalent mismatch names 4053 and the
stale unit lacks `--forwardports=udp:53:4053`, so even a listener that started
would not receive the public alias.
```
systemctl cat warp-main-lb-eno2np1.service | grep -E 'portblocks|forwardports'
docker inspect $c --format '{{range .Config.Env}}{{println .}}{{end}}' | grep WARP_PORTS  # what the container got
```
FIX: `warpctl service create-units main` → commit the regenerated `xops` units →
redeploy. Editing services.yml alone does nothing until the units are regenerated.

A correct, restarted unit is still not proof that a forward alias is live.
`warpctl` installs the interface-scoped public DNAT in `redirect()` only after
it assigns one free host port for every LB service and deploys the replacement
container. If any 30-port pool is exhausted by accumulated old LB containers,
the worker loops `netstat -tuln` / `Found occupied port`, the old generation
keeps serving, and there is no UDP/53 rule despite the running process showing
`--forwardports=udp:53:4053`. Require all three: correct process args, an Up
replacement with `WARP_PORTS` including 4053 (and 8053 during migration), and
the exact live DNAT rule.

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
8. every host's NEW container `Exited (2)` with a `missing env var` panic and no
   old container left on some of them → config-env startup panic (11.10). Read
   the panic before assuming 11.7: the poll loop looks identical.
9. panels empty while `/api/health`, the ui, and login are all 200 → shared
   datasource row port drift (11.11): compare the provisioned datasource url to
   the port mimir is actually listening on for that host.
10. the lb intermittently 504s / a fraction of requests hang while the rest are
    fast, and ONE host also shows memberlist `i/o timeout` to every peer → that
    host's conntrack table is full (11.12): `sysctl net.netfilter.nf_conntrack_count`
    vs `_max`. Check this BEFORE anything grafana-specific — the container is a
    victim, not the cause, and every other symptom here is downstream of it.
11. log panels hang / Explore times out on SOME sessions while metrics render
    everywhere, and every host logs `error sending requests to scheduler ...
    SHUTTING_DOWN addr=<peer>:6490` → one host's loki query modules are stuck
    Starting (11.13). The `addr=` names it. `up/front/graf/restarts` all read
    healthy — go to the child `/ready` and `/services`, not the front proxy.
12. a whole dashboard is blank (not just some panels), or `count by (host)` on
    any metric is short a host → that host's fluent-bit is dead (11.14).
    Check `systemctl is-active fluent-bit` on the hosts with NO grafana bundle
    too (edge-6, edge-2) — they appear in no §11.1 reading.
13. Grafana UI/health and Mimir are green, the datasource row exists, but
    `/api/ds/query` and every scheduler rule fail with
    `[plugin.notRegistered]` → the datasource implementation is absent from
    the image (11.15). Inspect the plugin directory; recreating the row does
    not install the plugin.

### 11.10 grafana.yml `{{ env: }}` → fleet-wide startup panic (2026-08-11)
A config value may thread `{{ env:KEY }}`, but the KEYs live in settings.yml
`env_vars`, which **warpctl never puts in the container environment**.
```
docker logs $(docker ps -a --filter name=grafana -q | head -1) 2>&1 | head -20
#   panic: missing env var BRINGYOUR_MINIO_HOSTNAME for grafana.yml value "{{ env:BRINGYOUR_MINIO_HOSTNAME }}"
#   main.resolveMinioEndpoint -> renderLokiConfig -> main.main
docker ps -a --filter name=grafana --format '{{.Image}} | {{.Status}}'
#   ...2026.8.10-1016091940 | Exited (2) 1m ago   <- new build, crash looping
#   ...2026.7.20-997576310  | Up 10 hours         <- old build, still serving
docker inspect $c --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c BRINGYOUR_  # 0 on EVERY service
```
- ROOT: `--envvar=` is emitted only for services.yml `env_vars`
  (`warpctl/config.go` ~2368); no unit carries a `BRINGYOUR_*` one
  (`grep -rl 'envvar=BRINGYOUR' xops/main/ansible/files/systemd/` = 0). The
  server binary sees these values only because `server/env.go` init() replays
  settings `env_vars` through `os.Setenv`; the grafana front has no such step,
  so `os.Getenv` was always empty. Fixed 2026-08-11: `interpolateEnv` resolves
  from `hostSettings.EnvVars` first and the process env second — the postgres
  and redis hostnames in `renderGrafanaConfig` already read settings directly,
  and only the minio path had drifted onto `os.Getenv`.
- TELL vs 11.7: on the stuck hosts `docker ps -a` shows **no lingering old
  container** — a plain crash loop, not the poll deadlock. warpctl still loops
  `Poll http://<gw>:14488/status` forever, which reads exactly like 11.7.
- AMPLIFIER: hosts that still had their previous container kept serving; hosts
  whose old container was already gone served NOTHING → lb 502 there. The fleet
  splits into healthy / no-data / 502 simultaneously, so a single external
  probe reports whichever host it happened to land on.
- TEST GAP that let it ship: `main_test.go` set the very same var with
  `t.Setenv`, so the suite passed. The regression test now resolves purely from
  `HostSettings.EnvVars` with nothing in the process environment.

### 11.11 Shared datasource row → per-host port drift ("no data", everything healthy)
Grafana state is ONE shared env postgres and file provisioning upserts
datasources by `uid`, so every host writes `uid: warp-mimir` with **its own**
WARP_PORTS mimir port and the host that starts last pins its port fleet-wide.
```
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"intervalMs":900000,"maxDataPoints":50,"timeRange":{"from":"now-24h","to":"now"}}' \
  https://<env>-grafana.<domain>/api/public/dashboards/<accessToken>/panels/<id>/query
#   {"results":{"A":{"error":"Post \"http://127.0.0.1:14578/prometheus/api/v1/query_range\":
#    connect: connection refused","errorSource":"downstream","status":502,"frames":[]}}}
ss -tln | awk '{print $4}' | grep -oE '[0-9]+$' | sort -un | awk '$1>=14578 && $1<=14607'  # real mimir port
docker exec $c grep -A1 warp-mimir /run/warp-grafana/provisioning/datasources/loki.yml     # what THIS host wrote
```
- DECISIVE READ: a host whose own provisioning file says 14579 dials 14578 →
  the shared DB row overrode the local file. That is the whole bug in one line.
- The public dashboard renders it as empty panels with no error, while
  `/api/health`, the ui, and login all return 200 and mimir itself is fine. Do not
  read "no data" as an ingest or retention problem before checking this.
- **`query=up` is NOT a mimir data check.** There is no `up` series anywhere in
  mimir — nothing in the fluent-bit `prometheus_remote_write` path synthesizes
  one (that is a prometheus SCRAPER artifact, and there is no prometheus here),
  so it answers 200 with an empty result whether mimir holds data or not. Reads
  that actually discriminate:
```
curl -sG 127.0.0.1:3100/prometheus/api/v1/query --data-urlencode 'query=count({__name__!=""})'
#   healthy main: ~25000 series
curl -sG 127.0.0.1:3100/prometheus/api/v1/query --data-urlencode 'query=count by (host)(urnetwork_build_info)'
#   healthy main: 6 hosts (the grafana bundle hosts). node_load1 adds edge-2 = 7.
#   a host MISSING here has a dead shipper (11.14), not a mimir problem
```
- Fixed 2026-08-11: the datasources address the front's stable `local_port`
  (3100) and the **loopback binding of that port** serves `/prometheus/` and
  `/loki/` reads. That port is identical on every host and every deploy, so the
  shared row is correct fleet-wide. The lan binding stays push-only — those
  read routes are unauthenticated (same trust as the loopback child listeners
  the datasources dialed before), and the lan binding is reachable from every
  routed host.

### 11.12 conntrack table full after a reboot → host-wide packet loss (2026-08-12)
Presents as grafana "down": the lb returns intermittent 504 and ~1 in 6 requests
hangs past 12s while the other five answer in 0.26s. It is NOT a grafana bug —
one host is dropping packets for **every** new connection, and grafana is just
the loudest victim.
```
sysctl net.netfilter.nf_conntrack_count net.netfilter.nf_conntrack_max   # count == max
dmesg -T | grep 'table full'          # nf_conntrack: table full, dropping packet
grep conntrack /etc/sysctl.conf       # says 1048576 — but the RUNNING value is 262144
uptime -s                             # the split falls exactly on boot time
```
- DECISIVE READ: **`/etc/sysctl.conf` says 1048576 while the running value is
  262144.** The config is right and un-applied. 262144 = the module's own default
  (65536 buckets x 4), so that exact number means "nobody ever set this".
- MECHANISM: `net.netfilter.*` keys only exist once `nf_conntrack` is loaded. At
  boot nothing loads it until docker configures nat, which is *after*
  systemd-sysctl has already applied sysctl.conf — so the key silently no-ops
  (systemd-sysctl still exits `0/SUCCESS`; there is no error to find) and the
  module later comes up at its default. **Every reboot reverted it until the
  next ansible run.** The ansible path always worked because the playbooks
  modprobe first, which is why the fleet looked fine for months.
- Compare across hosts before blaming the app — the correlation is the proof:
  the four hosts booted before 2026-08-11 02:35 ran 1048576; the two booted
  after (fireside 22:41, edge-3 02:14) ran 262144. Only edge-3 carried enough
  traffic to actually fill it (peer edge-1 idles at 180k).
- Symptom set is wide and misleading: loki/mimir memberlist gossip fails
  `dial tcp <peer>:6492/6493: i/o timeout` to every peer, mimir queries return
  `fetched_series_count=0`, ping drops 50-100%, and even ssh to the box drops
  commands. `i/o timeout` (not `connection refused`) is the tell — packets are
  dropped, not rejected. Partial rather than total loss = slots briefly freeing.
- Do NOT chase the NIC. Check `ethtool`/`ip -s link` once to clear it and move
  on: on edge-3 the link was 10Gb/s full duplex with 0 carrier and 0 TX errors,
  and `rx_missed 10816 / 2.2B` packets is noise. ARP was clean too (every peer
  agreed on the MAC, no duplicate IP), and INPUT policy was ACCEPT with no rules.
- IMMEDIATE FIX (safe, instant, no restart — just realizes the host's own
  declared config): `sudo sysctl -w net.netfilter.nf_conntrack_max=1048576`
- PERMANENT FIX 2026-08-12: the playbooks now write
  `/etc/modules-load.d/nf-conntrack.conf`, and `systemd-sysctl.service` is
  ordered `After=systemd-modules-load.service`, so sysctl.conf applies on every
  boot. Applied to playbook-{edges,dbs,redis-clusters,subtensor}.yml — all four
  had the identical defect.

### 11.13 loki query modules stuck Starting while the ring says ACTIVE (2026-08-17)
The quiet sibling of 11.2. The child process is UP and never exits, so the
supervisor never restarts it; the deploy poll passes; every §11.1 field except
`loki`/`svc` reads healthy — and the host answers no log query at all. edge-4
ran this way for 16h40m.
```
curl -s 127.0.0.1:$(getp 3101)/services | sed 's/<[^>]*>/ /g'
#   querier => Starting   query-scheduler-ring => Starting        <- the 4 stuck
#   query-scheduler => Starting   query-frontend => Starting
#   ingester/distributor/store/ring => Running                    <- ingest is FINE
curl -s 127.0.0.1:$(getp 3101)/ready
#   503 Some services are not Running: Running: 12 Starting: 4
```
- **DECISIVE READ** — grafana's own datasource proxy, the exact path a panel
  takes. Metrics answer, logs hang to timeout:
```
ap=$(docker exec $c sed -n 's/^admin_password = """\(.*\)"""/\1/p' /run/warp-grafana/grafana.ini)
curl -s -m 25 -o /dev/null -w 'http=%{http_code} time=%{time_total}\n' -u "admin:$ap" \
  "http://127.0.0.1:$(getp 3000)/api/datasources/proxy/uid/warp-loki/loki/api/v1/labels"
#   broken host: http=000 time=25.001      healthy host: http=200 time=0.071
```
- **INGEST IS UNAFFECTED**, which is why nothing else looks wrong: the broken
  host's own logs are queryable *from every other host*. Only its querier is dead.
- **FLEET-WIDE BLAST RADIUS.** The stuck host's scheduler stays `ACTIVE` in the
  ring, so all n−1 query-frontends dial a scheduler that is not running. This
  became ~50% of every grafana container's log volume fleet-wide:
```
docker logs --since 5m $c | grep -c 'error sending requests to scheduler'
#   328 of 655 total lines on edge-0; ~66/min/host; ~95k lines/host/day
#   err="unexpected status received for init: SHUTTING_DOWN" addr=<stuck host>:6490
```
  Read the `addr=` — it names the stuck host, and it is the fastest way to find
  it from any healthy host.
- **THE STUCK POINT** (proven 2026-08-18 by reproducing it on a restart, which
  put the startup logs back inside docker's retention). Loki's scheduler ring
  manager waits for its OWN instance to pass through `JOINING`, and that wait
  never ends:
```
docker logs $c 2>&1 | grep -aE 'ring=scheduler|ringmanager'
#   basic_lifecycler.go:322 msg="instance found in the ring" instance=<host>
#     ring=scheduler state=ACTIVE registered_at="<a PREVIOUS instance's time>"
#   ringmanager.go:186 msg="waiting until scheduler is JOINING in the ring"
#   <nothing ever follows>
# healthy start, for contrast — the two lines that must appear, ~3s apart:
#   ringmanager.go:190 msg="scheduler is JOINING in the ring"
#   ringmanager.go:203 msg="scheduler is ACTIVE in the ring"
```
- **WHAT DOES *NOT* CAUSE IT — do not repeat this mistake.** Finding the entry
  already `ACTIVE` at startup is NOT sufficient, however plausible it looks.
  Both the wedged start and the healthy start that followed it read
  `state=ACTIVE`; the healthy one transitioned to JOINING 3.6s later anyway.
  Any theory has to explain that pair. The same data also kills the
  overlap-length theory: edge-3 deployed with a 4m04s gap between its old and
  new container and was fine, while edge-4's 2m24s gap wedged.
```
                       wedged 07:33            healthy 07:45
  state at startup     ACTIVE                  ACTIVE          <- identical
  registered_at        2026-08-17 14:59:56     2026-08-18 07:43:53
  entry was held by    a WEDGED predecessor    a HEALTHY predecessor
```
- **BEST-SUPPORTED EXPLANATION (not proven): the wedge is CONTAGIOUS.** The two
  containers of one host share a ring entry (instance id = short hostname,
  11.7b). A wedged predecessor keeps heartbeating that entry as ACTIVE while
  its own ring module never completes, so the successor's transition to JOINING
  never sticks; a healthy predecessor does not fight it. This fits every
  observation above, and it explains why one bad instance survived 16h across a
  redeploy. **What wedged the FIRST instance (edge-4, ~2026-08-17 14:59:56) is
  still unknown** — those logs had already rotated. Do not present the
  contagion as the origin.
- **"Forget" alone does NOT release it.** When the entry goes missing under a
  running instance, `basic_lifecycler.go:480` ("instance is missing in the ring
  … registering the instance with an updated registration timestamp")
  re-registers it while KEEPING ACTIVE. Observed: the entry was re-created with
  a fresh timestamp and the wait still never ended.
- **FIX (manual) — the entry must be ABSENT at registration time:**
```
docker stop -t 150 <container>     # loki unregisters: "ring lifecycler is shutting down ring=scheduler"
# confirm from a PEER that the entry is gone before restarting:
curl -s 127.0.0.1:<peer loki port>/scheduler/ring | grep -oE 'value="[a-z0-9-]+"'
systemctl restart warp-main-grafana-<block>
```
  A plain `systemctl restart` on its own is NOT sufficient — it recreates the
  overlap that causes this. The stop briefly leaves the host serving nothing
  (~1 min); the lb has the other five. Do NOT do this on a host with a pileup
  (11.7c says a restart ADDS to it).
- FIXED IN CODE 2026-08-18, two halves in `warp/grafana/main.go`:
  **(1)** `/status` no longer answers ok the moment the front binds — a
  readiness latch holds the deploy poll until loki, mimir and grafana each
  answer their own endpoint once, and reports `error not ready (loki: 503 …)`
  until then, which is what `WarpStatusResponse.IsError` (`^(?i)error(\s|:)`)
  actually fails a poll on. A container that comes up like this no longer
  installs itself; warpctl keeps the old, working one. TRADEOFF, and it is the
  11.10 amplifier: the poll budget is `NewContainerPollTimeout` = 120s, and on
  poll-fail warpctl kills the NEW container (11.7) — so on a host whose old
  container is already gone, a child that cannot start now leaves that host
  serving nothing instead of serving 503s. The failing poll names the child in
  the journal, which is the point: it is loud instead of silent. **(2)** `warp.Child`
  gained an optional `HealthCheck`/`UnhealthyTimeout` (`warp/supervise.go`);
  loki and mimir are restarted after 10 min continuously failing `/ready`, so
  this self-heals instead of sitting for 16h. Grafana is deliberately left
  exit-only — its health tracks the shared postgres, and restarting it does not
  fix what it is reporting. **(2) is the half that actually breaks this
  particular trap**, and it does so for the reason the manual fix works: it
  SIGTERMs loki IN PLACE, so loki unregisters cleanly (verified: the entry left
  a peer's ring in under a second) and the supervisor restarts it with no
  competing instance holding the entry — the one sequence a container-level
  redeploy can never produce. (1) only stops a wedged container from installing
  itself over a working one.
- **THE TWO HALVES ARE NOT INDEPENDENT — (1) WITHOUT (2) WOULD MAKE A HOST
  UNDEPLOYABLE.** If the OLD container is the wedged one, the new container
  catches the wedge, fails its readiness poll, and warpctl kills it at the 120s
  budget (11.7) while the wedged old one keeps serving. Every retry does the
  same. Recovery then depends entirely on the OLD container's watchdog firing
  at 10 min and clearing its own loki. Ship them together, and if (2) is ever
  disabled, disable (1) with it.
- **NEITHER FIX PREVENTS THE FIRST WEDGE**, because its cause is still unknown.
  What they change is the duration: a permanent, silent, host-wide loss of log
  query becomes a ~10 min self-healing blip with `[loki]unhealthy for …
  Restarting.` in the journal naming it.
- The 10-min timeout is deliberately well above the unready window a rolling
  fleet deploy opens: loki and mimir both gate `/ready` on their rings, which go
  unhealthy while peers cycle. Do not shorten it without checking that.

### 11.14 fluent-bit is a HOST unit, and a failed one is permanent (2026-08-15)
alloy is gone; **host-managed fluent-bit ships every log line and every metric**,
on every warp host — including edge-6 (redis + minio) and edge-2 (postgres),
which run no grafana bundle at all and so appear in NO §11.1 reading. §11 had no
signal for it until 2026-08-18, which is why edge-6 shipped nothing for 2d13h
without anything noticing.
```
systemctl is-active fluent-bit                                   # per host, ALL warp hosts
systemctl show fluent-bit -p NRestarts -p ActiveEnterTimestamp -p Result
#   failed / NRestarts=5 / status=255/EXCEPTION
journalctl -u fluent-bit --since '<the failure minute>' | grep -iE '\[error\]'
#   [input collector] COLLECT_TIME registration failed
#   [input] error starting collector #0: prometheus_scrape.29..34
#   [sched] cannot do timeout_create()          <- out of fds, NOT a config error
#   [output:prometheus_remote_write.34] could not create thread scheduler
```
- **DECISIVE READ: the SOFT fd limit, which `systemctl show` does not lead with.**
  `LimitNOFILE` prints the HARD limit (524288, reassuringly large);
  `LimitNOFILESoft` was still the systemd default **1024**, and the soft one is
  what binds. fluent-bit allocates an fd per collector timer and per output
  worker AT STARTUP, so the budget scales with INPUT/OUTPUT pairs, not traffic —
  edge-6 renders one `prometheus_scrape` + one `prometheus_remote_write` per
  redis instance (32 of them) and crossed 1024.
```
systemctl show fluent-bit -p LimitNOFILE -p LimitNOFILESoft      # read BOTH
grep -cE '^ *name +prometheus_scrape' /etc/fluent-bit/fluent-bit.conf
```
- **AND IT STAYS DEAD.** systemd gives up after 5 starts in 10s
  (`Start request repeated too quickly`) and nothing retries a failed shipper.
  The trigger was an ansible `playbook-redis-clusters` run restarting the unit
  mid-flight (§8.5) — visible in the same journal minute as
  `ansible-...file .../etc/redis/redis-N.conf`.
- WHAT GOES MISSING, and where it surfaces: **zero `redis_*` series in mimir**
  → `grafana/dashboards/redis-cluster.json` renders blank and
  `warp/grafana/alerting/redis-cluster.yml` (incl. the `redis_up` /
  `redis_cluster_state` rules) cannot fire; no node metrics for that host
  (`count by (host)(node_load1)` is short one); no logs from it in loki. A
  dashboard that is *entirely* empty points here; a dashboard with *some* empty
  panels is usually just undeployed metrics.
- FIXED 2026-08-18 in `xops/main/ansible/files/fluent-bit/override.conf` (the
  shared drop-in, so all four playbooks get it): `LimitNOFILE=65536` (a bare
  value sets soft AND hard) and `StartLimitIntervalSec=0` + `RestartSec=10`, so
  a failed shipper retries forever instead of needing a human `reset-failed`.
  `xops/main/ansible/tests/test_fluent_bit_shipper.py` asserts the limit leads
  `redis_count`, so the next time that number grows the test fails first.

### 11.15 Grafana 13.2 datasource row without its Prometheus plugin (2026-08-29)

Grafana 13.2 extracted the formerly core Prometheus datasource into a
standalone native plugin. Main retained its provisioned `warp-mimir` datasource
row and every front/backend health endpoint stayed green, but Grafana could not
execute the datasource. The alert scheduler retried each rule on every Grafana
host, producing roughly 220–250 error-shaped lines/minute:
```
error="the result-set has errors that can be retried: [plugin.notRegistered] plugin not registered"
```
The decisive check is a query through Grafana itself, not a direct Mimir read:
```
# authenticated against a live Grafana child/front
POST /api/ds/query   {"queries":[{"datasource":{"uid":"warp-mimir"},"expr":"vector(1)"}],...}
# broken: HTTP 500 / plugin.notRegistered
find /var/lib/grafana/plugins -maxdepth 2 -type f | grep '/prometheus/'
# broken image: no standalone Prometheus plugin files
```
- A datasource database row proves only configuration. Direct Mimir success
  proves only storage/query health. Neither proves Grafana can instantiate the
  datasource implementation.
- Runtime plugin preinstallation is deliberately disabled so readiness does
  not depend on internet access. The image fix in `warp/grafana/Dockerfile`
  bakes Prometheus plugin 13.1.7 for both amd64 and arm64 with catalog-published
  SHA-256 checksums; `grafana/prometheus_plugin_test.go` holds that offline
  invariant.
- Verify after rollout with `vector(1)` through `/api/ds/query`, zero new
  `grafana-plugin-unregistered` lines, and successful evaluation of one
  provisioned rule. Do not silence scheduler errors or recreate the existing
  datasource as remediation.

### 11.16 Alert interval outside Grafana's scheduler grid (2026-08-30)

A new Grafana container can remain `Up` while its supervised Grafana child
restarts forever and Warp correctly refuses to activate it. The definitive
child error is:
```
invalid alert rule: interval (15s) should be non-zero and divided exactly by scheduler interval: 10
```
On the `2026.8.30+1033129380` rollout, the competition alert group used `15s`.
Grafana 13 schedules provisioned rules on a 10-second grid, rejected the file
during provisioning, and shut down every dependent module. The parent
`warp-grafana` process and Docker container stayed running, while `/status`
reported the Grafana child connection refused until the 120-second deployment
attempt failed. An old generation continued serving only on hosts where one
still existed.

Inspect both layers; `docker ps` alone is a false green:
```bash
docker logs <new-grafana-container> 2>&1 | grep 'invalid alert rule'
journalctl --utc -u warp-main-grafana-<interface>-g1 \
  | grep -E 'Poll result|Deploy fail'
```
The fix changes the group to `20s`, preserving a short heartbeat cadence while
aligning with the scheduler. Warp's
`TestProvisionedAlertIntervalsMatchGrafanaScheduler` parses every embedded
`grafana/alerting/*.yml` file and requires every group interval to be a positive
multiple of 10 seconds. Run `go test ./grafana` before building the image. A
restart cannot repair the current artifact; publish a corrected Grafana image
and require the new container's `/status` to become ready before draining the
old generation.

### 11.17 Exact-edge Grafana ingress and rotating-DNS blindness

Probe: `grafana-ingress`

Pin `https://<env>-grafana.<domain>/api/health` to every enabled edge IPv6
address from the active `services.yml` version. HTTP 200 is the only healthy
response. Transport refusal/timeout remains owned by `edge-ipv6` (§18.1); this
signal starts after TLS reaches the edge and therefore identifies a
service-specific response without duplicating the interface ticket.

The `2026.8.30+1033129380` rollout exposed why a single ordinary DNS request is
not enough. Edge-0 and edge-1 returned 200 while both edge-4 interfaces returned
502. Edge-4 had no old Grafana generation, the new container stayed `Up` while
its child repeatedly rejected the 15-second alert interval, its front `/status`
returned 503, and `WARP-MAIN-GRAFANA-G1` had no port-7183 DNAT target because
Warp correctly refused to activate the unready generation. The edge-4 LB then
received connection refused at `172.18.0.1:7183` and returned 502. DNS rotation
made `warpctl logs` alternate between success and three exhausted 502 retries,
so a global visibility alert alone did not name the broken edge.

The 2026-08-31T01:24Z live root-cause battery closed the remaining manual gap.
Edge-4's Warp unit was active, but its `/status` continuously reported the
Grafana child connection refused; the bounded child journal contained the
exact rejected `interval (15s)` / scheduler `10` error followed by child
restart. `grafana-ingress` now runs that unprivileged systemd/journal battery
once per failed host and shares the result across its interface alerts. When
the signature is present, Markdown records
`root_cause=alert-interval-scheduler-grid`, the rejected and scheduler
intervals, the parent-versus-child lifecycle, and the scheduler-grid test. A
synthetic host with two 502 interfaces requires one battery call and two fully
attributed alerts. A 502 still proves IPv6 reached the LB and must not be
relabelled as an interface-routing failure.

At 03:29Z the fleet also exposed a two-generation recovery trap. Edge-0 and
edge-4 still returned exact-address 502s because the new generation rejected
the 15-second rule, while the older serving edge-1 generation emitted
17–54 `plugin.notRegistered` rule-evaluation lines/minute because it lacked
Grafana 13's standalone Prometheus plugin. A direct Mimir read or green
Grafana health endpoint cannot clear that older generation. The replacement
image must contain both fixes: a pinned, checksum-verified Prometheus plugin
for each architecture and provisioned intervals on the 10-second scheduler
grid. Run both Warp Grafana regressions, then verify `vector(1)` through
Grafana `/api/ds/query` and exact-edge health before draining either failure
mode's predecessor.

For an exact-edge 502/503/504, compare old/new Grafana containers, each front
`/status`, the service-alias rule/socket, and the child provisioning log. Do not
add a DNAT target for an unready process and do not restart the same invalid
image. Correct the alert interval, pass the scheduler-grid test, publish a new
Grafana image, and require three pinned HTTP 200 responses on every edge plus a
bounded `warpctl logs` query across multiple DNS rotations.

---

## 12. Taskworker drain (deploy) — TASKDRAIN1

The taskworker plane has no client connections; its "clients" are the chain
cadences (one contract-close scheduler task with an internal worker pool,
handler reap 60s, reliability rollup 1min, client scores 30s). Deploys are
make-before-break over the shared pg queue, so a HEALTHY deploy pauses
nothing. These signals catch the unhealthy paths.

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
  (and EVERY claim of that container) remains leased for at most five minutes
  after its final heartbeat — find them with 12.3 and normally let them
  self-release. Use `bringyourctl task release` only for verified-dead workers
  when immediate recovery matters. Also broken: no outcome line within
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
Probe: `stuck-leases`

Alert id `pg/task-lease-stranded` (`signal_stuck_leases.go`, 60s cadence):
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
  duration history, 2.5), whose claim_time keeps advancing, and whose worker
  is alive.
- BROKEN: claim_time frozen > 2m shortly AFTER a taskworker kill/crash = a
  temporarily stranded claim. Current binaries cap release_time at five
  minutes after the last heartbeat regardless of run_max_time_seconds, so the
  chain self-recovers in at most five minutes instead of 30min–24h. A
  direct-postgres session advisory lock remains held while a worker is alive,
  so a starved heartbeat alone cannot create a duplicate; PostgreSQL drops the
  lock with the dead worker's connection. A lease remaining far beyond five
  minutes identifies a pre-fix claim/binary and still needs manual handling.
- ACTION: normally observe automatic expiry. If immediate re-claim is needed,
  verify the claiming worker is dead (deploy log / container list), then run
  `bringyourctl task release <task_id>` and/or `bringyourctl task kick
  <run_once_key>` (pull run_at to now). Releasing a RUNNING task re-opens the
  duplicate-execution window — verify first.

### 12.4 Post-deploy convergence
Probe: `task-convergence`

Alert ids: `pg/task-due-lag` (oldest due-and-unclaimed > 180s sustained = the
plane stopped claiming) and `pg/task-target-missing` (`Target not found`
past 100 retries = beyond any overlap, a missing registration) — both in
`signal_task_convergence.go`, 60s cadence.
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

### 13.5 Password-auth failure classification

- A missing account and a wrong password are ordinary authentication failures.
  `/auth/login-with-password` must return the same generic `Invalid user or
  password.` result for both; neither case may become HTTP 500. A distinct
  missing-account response also creates an account-enumeration oracle.
- Acceptance first inspects whether its configured phone or email identity is
  stale. A clean identity therefore deliberately exercises the missing-account
  branch before signup. `inspect stale ... /auth/login-with-password returned
  HTTP 500` is an API failure, not bad acceptance data and not evidence that the
  fixture needs to be pre-created.
- When this signal appears, query API logs for `login-with-password` over the
  exact acceptance interval and compare route 5xx counts with the client
  artifact. The deterministic boundary regression is
  `TestAuthLoginWithPasswordUnknownUserIsGenericFailure`; retain the acceptance
  parser's legacy-500 case only for rolling deployments where an old API block
  may still answer during the upgrade. That old handler's wire response is
  `text/plain` with body `User does not exist.`, not the API's usual nested JSON
  error. Preserve the short plain-text detail, but tolerate only that exact
  missing-account message; `TestPhoneLifecycleRejectsUnrelatedPlainTextServerError`
  guards against turning arbitrary 500s into a clean-fixture result.

### 13.6 Cross-platform acceptance contract failures

- `Network name must contain only lowercase letters, numbers, and dashes`
  immediately after an otherwise valid signup is an acceptance identity
  generator failure when the generated name contains `_`. URL-safe base64 is
  not a safe network-name alphabet: `_` is valid base64url and invalid here.
  Current fixtures encode their random suffix as lowercase hex; the all-`ff`
  deterministic case guards the exact pre-fix underscore output. Do not debug
  PostgreSQL uniqueness or email verification before inspecting the submitted
  name.
- `rpc_pin_required` from the Linux or Windows service after all account cases
  pass means the acceptance controller and the privileged daemon disagree on
  the `start_tunnel` contract. The request must carry one generated server PEM,
  its pinned client certificate, the loopback listen address, and a CSPRNG
  `rpc_session_id`; the remote client must be configured with the matching
  material before the request is sent. Current Linux and Windows controllers
  build this shared payload through one helper. Recreating only the service or
  retrying provider selection cannot repair missing request fields.
- `socket cgroupv2 level ...: No such file or directory` can mean two different
  things. First verify `/sys/fs/cgroup/<reported-path>` exists. If it does not,
  the process is in the wrong/private cgroup namespace or the path is stale. If
  it does exist, inspect `/proc/config.gz`; Docker Desktop's LinuxKit kernel can
  report `# CONFIG_NFT_SOCKET is not set`, so nft rejects every socket-cgroup
  expression even though cgroup v2 itself is healthy. Run
  `urnetworkd --selftest-egress` to distinguish that from a missing cgroup-BPF
  marker. A proven marker permits the explicit floorless mark-only fallback;
  kill-switch floors and helper-DNS reconnect permits still refuse rather than
  silently weakening their cgroup dependency. The Docker acceptance container
  therefore uses the host cgroup namespace and its disposable privileged
  profile; `NET_ADMIN` alone cannot load and attach the BPF program. It also
  moves only `urnetworkd` into a dedicated child cgroup before exec. Leaving
  the test agent beside the daemon in the container-wide cgroup marks the
  agent's sockets too, making its public-egress probe bypass the tunnel and
  turning a green result into a harness false positive.

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

### 14.5 Public proxy protocol and return-path proof
Probe: `proxy-path`


Proxy health has five layers; none substitutes for the next:

1. **Current allocation readiness:** resolve the running container's current
   `WARP_PORTS` and request `/status` through the host/LAN address. Never cache
   the allocation across a rollout: crisp g1 moved status `12688 -> 12689`
   and SOCKS `12718 -> 12719` while this audit was running. A stale direct
   probe produces a false failure.
2. **Full public TCP handshake:** SYN, SYN-ACK, and final ACK must all cross the
   public interface. `nc -z`/a listening socket is weaker than a packet-level
   proof and says nothing about DNAT or the return route.
3. **Protocol negotiation:** SOCKS greeting should select username/password
   (`05 02`); HTTP and HTTPS-proxy requests with deliberately bad credentials
   must reach a prompt authentication rejection instead of timing out. This
   proves the correct process, not egress.
4. **Authenticated egress:** use a valid proxy-client fixture and fetch an
   external target through SOCKS, HTTP CONNECT, and TLS-to-proxy HTTPS. An
   invalid login fixture or a test-account signup blocked on verification
   makes the probe `BLOCKED`; it does not make the data plane PASS or FAIL.
5. **WireGuard traffic:** require a valid peer handshake and tunneled bytes in
   both directions. A UDP socket or `nc -u` result cannot prove WireGuard.

The proxy service intentionally has no normal public 443 status endpoint;
`warpctl ls versions main proxy --sample` can therefore return a uniform 404.
That is a probe-method mismatch, not evidence that all blocks are down. Use the
host-side deploy worker's readiness result or resolve each block's live
allocation and query it directly.

**Public-identity drift signature (fireside, 2026-08-28):** a proxy host may be
healthy at its current address while a future deploy is configured from a
stale address. Compare all five sources before changing routing: the live
public-interface address, public DNS, the XOps netplan, the active
`vault/main/services.yml` LB entry, and the upstream router route/whitelist.
Fireside's live interface, DNS, netplan, and router agreed on
`65.49.70.92` / `2001:470:99:5960:3a05:25ff:fe32:e5ab`, while three active
Vault version sections still named `.90` / `...:e5ac`. Crisp consistently used
`65.49.70.94` / `2001:470:99:5940:3a05:25ff:fe37:292a`. Do not classify an
arbitrary occurrence of `.90` as a host mapping: XOps router snapshots also
use it as a DHCP-pool lower bound.

Prove the routed identity against every block, on both address families. For
SOCKS, a greeting is enough to prove DNAT and protocol ownership without a
credential:
```bash
ip -4 -o addr show dev <public-interface> scope global
ip -6 -o addr show dev <public-interface> scope global
dig +short A <proxy-host>; dig +short AAAA <proxy-host>
printf '\005\001\002' | timeout 4 nc -4 -w 2 <proxy-host> <socks-port> | xxd -p
printf '\005\001\002' | timeout 4 nc -6 -w 2 <proxy-host> <socks-port> | xxd -p
```
Expect `0502` for every block. On 2026-08-28, all ten fireside and all ten
crisp blocks returned `0502` over both IPv4 and IPv6, and every HTTP proxy port
accepted TCP. That proves the router currently reaches `.92` / `...:e5ab`; it
does not excuse stale deploy metadata.

**Asymmetric-return signature (crisp, 2026-08-27):** every current container's
internal `/status` returned 200 and g1's direct SOCKS listener returned `05 02`,
but public SOCKS/HTTP/HTTPS handshakes timed out. A capture showed public SYNs
arriving on `eno1np0`, SYN-ACKs leaving on the LAN NIC `eno3`, repeated SYN-ACK
retransmits, no final ACK, and therefore no packet reaching the DNAT listener:
```bash
tcpdump -nn -tttt -i any \
  '(tcp port <public-port> or tcp port <current-allocation>)'
ip -4 route get <client-v4> from <public-v4>
ip -6 route get <client-v6> from <public-v6>
ip -4 rule show
ip -6 rule show
```
The host had no source-policy rules and the DHCP LAN default route had metric
50 versus 100 for the public default. `ip route get ... from 65.49.70.94`
selected the LAN gateway; IPv6 also selected the LAN route/source. The repair
belongs in the persistent Warp routing-table reconciler: traffic sourced from
each public address must use that address's public gateway/interface, while
management/LAN traffic keeps its LAN route. Do not lower the public main-table
metric as a workaround; that would move management traffic too.

The discriminator for the 2026-08-27 failure was the service clock. Crisp's
transparent LB had been active since 00:35, while `systemd-networkd` restarted
at 06:45. The LB startup journal proved Warp had installed the public subnet
routes and inspected existing rules. After the networkd restart, table 100
retained only Docker-interface routes; its public-interface subnet/default
routes and all policy rules were gone. Check this explicitly:
```bash
systemctl show -p ExecMainStartTimestamp systemd-networkd.service \
  warp-main-lb-<public-interface>.service
ip -4 route show table <rttable>; ip -6 route show table <rttable>
ip -4 rule show; ip -6 rule show
```
If networkd is newer than a still-running transparent LB and that table is
partial, classify it as routing-state drift, not a proxy/container failure.
On Crisp, `apt-daily-upgrade.service` invoked post-upgrade service restarts at
06:45 on August 27 and again at 06:19 on August 28; systemd reexecuted and
networkd restarted both times. Neither run upgraded systemd itself: the first
included OpenSSL and the second included PAM/Perl updates. This is therefore a
potential daily production risk from ordinary unattended library/security
updates, not a reboot-only corner case. Edge provisioning sets
`APT::Periodic::Enable "0"` and masks `apt-daily*` plus
`unattended-upgrades.service`; check both configuration and unit masks, because
either layer can drift. Apply OS/security updates only in a controlled
maintenance window with proxy return-path verification afterward.
`warpctl service run ... lb ... --transparent=true` must periodically restore
its owned routes/rules, use replay-safe `route replace`, and copy each real
main-table gateway (especially an IPv6 link-local RA gateway) instead of
assuming subnet `::1`. Verify both families with source-aware `route get`,
packet captures, and application handshakes after applying it; an internal 200
alone cannot close the incident.

**Late-RA boot signature (fireside, 2026-08-28):**
`network-online.target` can start the transparent LB after its required static
IPv4 address exists but before Router Advertisement assigns the optional public
IPv6 address. The startup log then names the public interface only as `ipv4=...`
while the Docker bridge already has `ipv6=...`; later `ip -6 addr` shows the
public address, but `ip -6 rule` has no Warp rules and `ip -6 route show table
<rttable>` says the FIB table does not exist. A forced-IPv4 authenticated proxy
request passes while the normal dual-stack hostname stalls. This is not a
container, provider-window, or router-whitelist failure. Current Warp retries
live interface discovery while the optional routing-table IPv6 family is
missing, then installs its source/fwmark rules and the real RA default gateway.
For a pre-fix binary, restarting only the transparent LB controller after the
address appears restores the table without restarting proxy containers.

**Dead-first DNAT after a partial LB cutover (2026-08-30):** An immediate TCP
reset from `api-v6` while the LB unit and its sole container are Up can be a
host-rule failure, not nginx, routing, or IPv6 address discovery. Capture all
three views before changing anything:
```bash
sudo ip6tables -t nat -L WARP-MAIN-LB-<IFACE> -n --line-numbers
sudo ip6tables -t nat -S WARP-MAIN-LB-<IFACE>
sudo docker inspect <running-lb> --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep '^WARP_PORTS='
```
The decisive signature is two interface-scoped rules for the same public
protocol/port: the FIRST target is a pool port with no live socket, and a later
target is the port in the current container's `WARP_PORTS`. Packet capture then
shows an inbound SYN followed immediately by the host's RST; a source-aware
`ip -6 route get` can still be correct. On edge-1 eno2, public TCP/443 first
targeted dead port 7232 while the live LB owned 7231; edge-0 eno4 had the same
7659-before-7658 split.

Root cause: `warpctl deploy()` inserted the candidate's DNAT first, then a
fallible post-cutover container-discovery step ran before the deployment
success flag was set. If that later step failed, deferred rollback stopped the
candidate but did not restore DNAT, permanently leaving the dead rule first.
Polling considered the old desired-version container healthy, so it never
repaired the chain. Correct Warp behavior treats the start of a validated,
non-transactional redirect as the irreversible commit boundary, keeps that
candidate on later redirect/housekeeping errors, and on controller startup
removes only current-pool DNAT targets that have no live socket. It repeats
that socket-authoritative check at a bounded cadence while multiple containers
of the current version overlap, because the listener can close after startup.
Docker state is insufficient ownership evidence: during
`docker stop -t 3600`, nginx closes its listeners at the start of the graceful
wait while Docker can continue to report the container as running for an hour.
Reuse the same bounded socket inventory as port allocation and stale-conntrack
cleanup. With an old binary, delete only the fully inspected dead-target rules
or restore service with a corrected Warp controller; a generic route or
container restart is not a diagnosis.

The post-warpctl check later on 2026-08-30 reproduced that graceful-stop
variant on edge-1/eno2. Vault and the live interface both named
`2001:470:99:56:e643:4bff:fec3:8446`; its source rule, policy default, gateway
ping, and bound public egress all passed. Nevertheless every public TCP probe
was refused. Port 7231 had live IPv4/IPv6 TCP and UDP sockets, port 7232 had
none, and container `5e7c412aa0cd` remained underneath
`docker container stop -t 3600`. Verify the socket-authoritative fix by seeing
the dead 7232 rules removed in both families and three consecutive pinned IPv6
HTTPS requests return 200. Do not group the simultaneous edge-3/fireside
timeouts with this RST signature: those need inbound/upstream-path evidence
even though their Vault addresses and source-policy routes also match.

The completed `2026.8.30+1033129380` rollout reproduced the post-startup timing
on edge-0/eno4. The first same-version container took port 7659 at 20:27Z. A
warpctl restart at 20:36Z found 7659 occupied and launched the same target on
7658; at 21:03Z its inherited one-hour stop began draining the 7659 container.
Only 7658 retained live TCP/UDP sockets, while the pinned public IPv6 request
to the exact Vault/interface address refused in roughly 60ms on three
consecutive attempts. At 21:15Z, three pinned attempts on every reachable
configured IPv6 interface produced four stable HTTP 200 interfaces
(edge-0/eno2, edge-1/eno3, edge-4/eno3, edge-4/eno4), three immediate-refusal
interfaces (edge-0/eno4, edge-1/eno2, crisp), and three timeout interfaces
(both edge-3 interfaces and fireside). Edge-5 was operator-declared offline
and is excluded from that denominator. Every failed reachable host's Vault
address exactly matched its live interface; do not repair this incident by
changing `services.yml`.

The later socket inventory found the remaining controller race. Warpctl did
perform the bounded socket-authoritative scan while Docker reported two
same-version containers, but nginx could close the old listener immediately
after that scan and Docker could remove the old container before the next
30-second scan. The following poll saw only one container and skipped
reconciliation, leaving the dead first DNAT rule indefinitely. On edge-0/eno2,
edge-0/eno4, and edge-1/eno2 this left dead targets 7232, 7659, and 7232 ahead
of the live 7231, 7658, and 7231 targets respectively. Preserve the bounded
overlap scans, and also run one final socket-authoritative prune on the
duplicate-to-single transition. The deterministic regression closes the old
listener and removes the old Docker container one second after a clean overlap
scan; it requires the dead IPv4 and IPv6 rules to be removed while the live
rules remain.

**Upstream IPv6 ACL identity drift (edge-3, 2026-08-30):** a timeout has a
different discriminator from the dead-first reset. Both edge-3 links were up
at 10 Gb/s, the live interface addresses exactly matched active v21 Vault
(`...:5880:...:e380` and `...:5860:...:e381`), both exact-source outbound
requests returned those same addresses, both gateways answered, live nginx
sockets existed, and the exact host DNAT rules had zero inbound packets.
External ICMPv6 reached both addresses while new TCP/22, TCP/80, and TCP/443
timed out. Healthy edge-4 on the same EdgeRouter answered TCP/80 and TCP/443
while intentionally filtering TCP/22. That protocol split localizes the drop
to the upstream ingress ACL rather than Vault, NDP, routing, nginx, or host
netfilter.

The EdgeRouter Infinity configuration supplied the exact cause. Its active
`WANv6_IN` policy has default action `drop` and permits ICMPv6 in rule 50, but
edge-3's TCP/UDP allow rules still name the former NIC identities ending
`e382` and `e383`. The deployed host and active Vault version now end `e380`
and `e381`; edge-4's equivalent ACL destinations match its live addresses and
pass. Repair only the destination address in rules 30, 31, 41, and 42 from
`e382` to `e380`, and rules 33 and 34 from `e383` to `e381`; do not broaden the
default-drop policy or change ports/actions. Persist the router transaction,
then require three consecutive pinned HTTP/1.1 IPv6 200 responses on each
edge-3 interface and confirm the host DNAT counters advance. Keep the active
Vault version as the authority, update any stale inventory copy, and compare
router permit destinations with the live interface during every address or
NIC migration. Edge-5 remains operator-declared offline and excluded.

### 14.6 Hosted DeviceLocal carrier-budget saturation

**False post-deploy verification against a stale proxy artifact:** do not use
an operator rollout statement, an `Up` container, or the configured desired
version as proof that the return-path fixes are running. Before interpreting a
post-deploy acceptance failure as a regression, inspect the running containers
and the exact image on both proxy hosts:
```bash
sudo docker ps --no-trunc | grep main-proxy
sudo docker inspect $(sudo docker ps --filter name=main-proxy -q) \
  | jq -r '.[] | [.Name,.Created,.Config.Image,.Image] | @tsv'
sudo docker images --digests bringyour/main-proxy
sudo docker image inspect <image-id> | jq -r '.[0].Created'
sudo journalctl --utc -u 'warp-main-proxy-*' --since=-10m \
  | grep -E 'Latest version|Polled latest versions'
```
Compare the tag and image creation time with the release containing the fix,
not merely with the container start time; a reboot can restart an old image.
If every controller repeatedly polls the old version as `Latest`, the rollout
is not stalled on those hosts: the corrected artifact is not selected as
latest (and may not have been published). Do not restart healthy old
containers to conceal that control-plane state; publish/select a distinct
corrected version first.
On 2026-08-30, the sustained suite selected Crisp and reproduced isolated plus
overlapping return stalls, but every Crisp and Fireside block still ran image
`2026.8.28-1031763440`, created at `2026-08-29T05:53:13Z`. That predates the
finite-TUN handoff, lossless WireGuard return queue, and destination-keyed
cross-protocol demultiplexing fixes. The image still had all three pre-fix
behaviors: finite TUN tails waited for a later cadence packet, a full WireGuard
queue dropped returns, and every HTTP/SOCKS dial detached WireGuard receive.
Those old behaviors exactly explain a green 9/9 window with isolated SOCKS or
WireGuard stalls and overlapping three-protocol stalls. If the expected image
does not appear in `docker images` on either host, the new rollout never
reached the proxy fleet; deploy a distinct new version before using another
acceptance run to judge the fixes.
An unchanged rerun is an especially useful discriminator: all three isolated
campaigns can pass 60/60 on this old image, while the overlapping campaign
then loses WireGuard and SOCKS returns with the hosted device still connected
and its exit window ready. Isolated green results therefore do not disprove
the stale-artifact diagnosis; require the simultaneous three-protocol soak.

`providers-unresponsive` is not sufficient evidence that providers failed.
The main proxy failure on 2026-08-28 had healthy public ingress, healthy proxy
RPC/API access, H1 correctly pinned, and fill retries still running. The
deployed process instead showed one process-global platform-transport budget
shared by every hosted DeviceLocal: 16 MiB total, 8 MiB used, all 16/16 carrier
slots acquired, and 146 H1 reservations (about 76.5 MiB) pending. A fresh
standalone DeviceLocal worked because it did not arrive behind that saturated
process queue. `WindowStatus.Failed` remained a presentation latch; it did not
stop enumeration or retry.

Current proxy builds give each hosted DeviceLocal one private carrier budget
derived from `config/main/proxy.yml: device_memory_budget` (24 MiB in main).
Devices share one manager-lifetime NetworkSpace/client strategy, but not
mutable JWT/refresh sessions or memory admission. Use these aggregate,
identity-free metrics:

- `urnetwork_proxy_device_memory_target_bytes`: should equal approximately
  `urnetwork_proxy_devices_live * 24 MiB` in main.
- `urnetwork_proxy_platform_transports_max`: should scale as
  `urnetwork_proxy_devices_live * 16`; a flat 16 with multiple live devices is
  a pre-fix/shared-budget deployment.
- `urnetwork_proxy_platform_transports_used` and
  `urnetwork_proxy_platform_transport_used_bytes`: acquired carriers.
- `urnetwork_proxy_platform_transports_pending_h1` and
  `urnetwork_proxy_platform_transports_pending_h1_bytes`: the direct starvation
  signature. A short nonzero value can be normal during an overlapping
  replacement; sustained pending H1 together with a locally full budget and an
  unsatisfied window is not.
- `urnetwork_proxy_device_memory_tracked_used_bytes` is live budget-accounted
  use, not RSS; allocator/runtime and bounded NAT endpoint memory remain outside
  that tracked sum.

For a reported proxy failure, correlate pending H1 with the window status and
retry logs. If pending remains zero while the window repeatedly enumerates and
evaluates candidates, investigate provider reachability/auth. If pending is
sustained, compare `transports_used` with `transports_max` and verify both the
24 MiB config and the per-device SDK build before blaming the provider fleet.

**Hosted-device recreation loop:** repeated
`[pd][<same-proxy-id>]window identity restore` lines without a proxy deploy are
proof that one logical proxy is being torn down and reconstructed. They are not
ordinary window retries. The idle timeout is 90 minutes, so repetitions minutes
or seconds apart under active traffic are also not idle reaping:
```bash
warpctl logs main proxy --query='window identity restore' --since=30m --limit=2000
```
The 2026-08-28 pre-fix selector treated a live DeviceLocal's transient
`MinSatisfied=false` as terminal after it had once been ready. The next
HTTP/SOCKS/WireGuard lookup canceled the whole DeviceLocal, aborting the very
multi-window retry intended to refill it, and restored the same identities into
a new instance. Correct behavior retains the same hosted device while its
lifecycle is live, regardless of readiness; only proxy-context cancellation,
DeviceLocal lifecycle completion, or the real idle reaper may replace it.
Correlate this signature with carrier saturation: a shared full carrier budget
makes the replacement less likely to fill and turns selection churn into an
amplifier. A `Failed` window presentation status is never a replacement reason.

**DoH route-observation panic:**
`dohRouteForConn.func1` in an `Unexpected error` stack means HTTP/2 supplied a
connection wrapper with a nil local or remote address. Route metadata is only
diagnostic, but the pre-fix callback dereferenced the missing endpoint and lost
that DNS result. `HandleError` keeps the process up, so container readiness and
public handshakes stay green while one proxied request can time out. Current
Connect returns no route metadata for nil or typed-nil endpoints and continues
processing the DNS response. Count this stack independently from window stalls;
after deploying the fix, require zero new occurrences throughout the sustained
acceptance window.

**Cross-protocol return-path theft:** a hosted proxy client can legitimately
use HTTP CONNECT, SOCKS, and WireGuard at the same time. A pre-fix proxy device
had one process-global return mode: attaching WireGuard redirected every
DeviceLocal return packet to the WireGuard receive channel, while a later Tun
dial detached that channel. The decisive live signature is an overlapping
campaign where HTTP and SOCKS never complete readiness, WireGuard works for a
while and then stalls, and its inner-packet trace records foreign-destination
TCP resets with no payload for the WireGuard address. All paths can pass when
run alone, so sequential acceptance is not sufficient. This is not provider
window failure or carrier-budget saturation when readiness remains one and
pending H1 remains zero. Current code attaches the WireGuard receive channel to
its assigned client IP and demultiplexes each returned IPv4/IPv6 packet by
destination; every other packet remains on the private HTTP/SOCKS Tun.
`DialContext` must never toggle that attachment. Require the acceptance suite's
concurrent three-protocol campaign and zero foreign-address packets in its
WireGuard trace. The deterministic boundary regression is
`TestProxyDeviceTunDialDoesNotStealWireGuardReturn`: it attaches WireGuard,
starts a Tun dial on the same device, then injects a returned WireGuard packet
through the production callback path and requires delivery to the peer channel.

**WireGuard return-queue loss:** before the lossless handoff fix, a full shared
WireGuard receive queue made a hosted DeviceLocal silently discard the returned
inner packet. Provider NAT has already consumed the upstream TCP bytes at this
boundary and does not retransmit the discarded device-side segment. One brief
queue-full event can therefore appear as TCP/TLS success followed by a permanent
single-flow sequence hole: outbound inner retransmits continue, inbound inner
traffic stops, and the next fresh connection works. Current code waits for the
fixed queue's capacity or lifecycle cancellation, after flushing every Tun
return in the batch so HTTP/SOCKS are not held behind that wait. Watch
`urnetwork_proxy_wireguard_return_backpressure_total` and the
`urnetwork_proxy_wireguard_return_backpressure_seconds` histogram. A rising
count is bounded flow control, not loss; sustained or long waits identify a
WireGuard reader that is not draining and require correlating the process
lifecycle rather than increasing an unbounded queue.

**Finite TUN return-burst stall:** HTTP CONNECT and SOCKS can lose a completed
origin response even with no active WireGuard attachment. The decisive
2026-08-29 reproduction was an isolated SOCKS request beginning at
06:09:38.867Z: the client completed TLS and timed out awaiting headers, while
the LB owning the selected `.85` address logged the same
`urnetwork-proxy-acceptance/1` request at 06:09:40.672Z as HTTP 200 with a 1 ms
upstream response. The hosted window stayed satisfied (9/9), proxy readiness
was 1, pending H1 was zero, and the subsequent isolated plus overlapping
WireGuard campaigns passed. That proves the response vanished after the origin
and isolates the failing branch to the hosted device's private TUN, rather than
the origin, rate limiter, carrier budget, or WireGuard queue.

The pre-fix Connect TUN forced gVisor's documented `LockUser`/`UnlockUser` TCP
processor handoff only every 16 injected return packets. A short TLS/HTTP
response can end before that cadence; if its final segment was queued while the
endpoint was syscall-owned, no later packet existed to repair the wakeup. The
provider NAT had already consumed the upstream bytes, so ordinary TCP
retransmission could not recreate the device-side segment. Current TUN code
finishes the handoff at the end of every finite `WriteBatch` and after every
single-packet `Write`, while retaining the 16-packet mid-batch bound for large
bursts. When diagnosing this signature, join the acceptance request's exact UTC
start and resolved destination from its device timeline to the LB access log;
an LB 200 before the client timeout is affirmative return-path-loss evidence,
not merely a healthy control request.

**HTTP transient-dial amplification:** one sustained HTTP CONNECT request may
time out while the same temporary device subsequently passes all SOCKS and
WireGuard requests. On its exact block, the distinguishing counter edge is one
new `ConnectDialErrors` followed by `ConnectClientsGone`, with no DoH panic,
identity restore, or pending-H1 increase. `ProxyConnectTimeout` is the interval
between retries, not a total timeout. A legacy 30-minute value turns one
recoverable TUN dial error into a client-visible terminal timeout; current
proxy/server defaults pace retries at one second and continue until the client
leaves. A clean immediate rerun does not make the legacy interval safe.

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
Probe: `key-publication`

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

---

## 16. Connect H3 / QUIC over public UDP — H3TRANSPORT1

Project terminology: connect "H3" is the custom connect carrier over QUIC on
UDP/443. It is not an HTTP/3 endpoint. `curl --http3`, HTTP `Alt-Svc`, and an
HTTP response are therefore not valid health probes. A valid probe must run a
QUIC handshake with SNI `connect.bringyour.com`; an authenticated connect
client is the strongest end-to-end probe.

The packet path has five independently observable boundaries:
```
client -> public interface IP:443/udp -> host DNAT -> nginx UDP listener
       -> PROXY v2 + connect backend -> PpPacketConn -> quic-go/auth
reply  <- reverse conntrack NAT       <- nginx         <- connect
```
Do not jump from "UDP arrived" to an SNAT conclusion. On a pre-hardening
Connect image a PP parse failure could close the backend before it emitted any
QUIC packet; current images drop/count that datagram and keep the shared socket
alive. In either case an `[UNREPLIED]` conntrack row says only that no reply was
seen and says nothing yet about the return-NAT implementation.

### 16.1 Generation and listener contract — desired is not live

The LB image contains generated nginx listeners, while the host-network port
mapping and public aliases come from arguments baked into systemd units. All
three views must agree:
```
# publish-side intent (useful, but insufficient)
warpctl ls versions main lb
warpctl ls versions main connect

# edge-side truth
systemctl cat warp-main-lb-<interface>.service
sudo docker ps -a --filter name=main-lb --format '{{.Names}}|{{.Image}}|{{.Status}}'
sudo docker inspect <lb-container> \
  --format '{{range .Config.Env}}{{println .}}{{end}}' | grep '^WARP_PORTS='
```

During the 4053 migration, the LB unit must carry logical UDP/4053 and the
compatibility UDP/8053 allocation plus `--forwardports="udp:53:4053"`;
Connect `WARP_PORTS` must contain 443, 4053, and 8053. The old 8053 path remains
only so the prior LB generation can drain. After no old LB can select 8053, a
later services version removes 8053 from both sides. Exact allocated host ports
vary by interface and generation; monitor service-port presence, not a fixed
internal number.

- BROKEN: new LB containers are `Exited (2)` with `Missing host port for
  service port 4053` or `8053`, while an old LB remains `Up`. The new tag is
  published but its forwarding behavior is not serving.
- HEALTHY: the intended LB container stays `Up`, its status endpoint passes,
  and its `WARP_PORTS` covers every `listen` port in the baked nginx config.
- ACTION: compare §11.8's unit args to the container env; regenerate/deploy
  systemd units before changing connect or nginx.

An ansible unit rollout restarts the observability units too (§8.5), so
`warpctl logs` may return 502 during precisely this window. Fall back to
`sudo docker logs --since <boundary> <actual-container>` on the edge. Keep the
rollout boundary in UTC so pre-fix PP errors are not counted against the new
generation.

### 16.2 UDP PROXY v2 provenance and parse signals

The live LB—not the repository—must prove that it can emit PPv2 to a UDP
upstream:
```
sudo docker exec <lb-container> nginx -V
sudo docker exec <lb-container> nginx -T 2>&1 \
  | grep -E 'listen (443|4053|8053)|proxy_protocol|proxy_timeout|proxy_requests'
```

Current healthy provenance/config:

- nginx build string contains pinned commit
  `11d11b5f0d3d8ace5215e1a77918e9dc219ce7db` (the first upstream build used
  here with UDP-upstream PPv2 support);
- UDP listeners use `udp reuseport`, `proxy_protocol v2`,
  `proxy_timeout 30s`, and `proxy_requests 0`;
- connect has `EnableProxyProtocol=true`, so its `PpPacketConn` requires a
  header on a proxy address's first datagram.

Read actual connect container stdout from the same post-rollout window:
```
sudo docker logs --since <boundary> <connect-container> 2>&1 \
  | grep -Ei 'h3|proxy protocol|quic'
```

- `urnetwork_connect_pp_dropped_packets_total{reason="transport_family"}` =
  wrong PP transport family, historically logged as `Proxy protocol header
  must be UDP`. Failure is before QUIC and auth.
- `...{reason="missing_header"}` = a headerless first datagram from an
  old/bypassing LB path or broken UDP pseudo-session. Other closed reasons are
  `malformed_header`, `proxy_address_family`, and `address_family`.
- Current `PpPacketConn.ReadFrom` loops until it receives a valid datagram or
  the underlying socket itself fails. A malformed/headerless/wrong-family
  datagram is dropped and counted inside the wrapper and is never returned to
  quic-go. Any legacy PP error escaping through an H3 accept loop identifies a
  pre-hardening Connect generation.
- Every enabled listener is supervised with bounded exponential backoff.
  Bind, transform, `ListenEarly`, accept, and panic exits increment
  `urnetwork_connect_h3_listener_failures_total{transport,stage}`; restarts
  increment `urnetwork_connect_h3_listener_restarts_total{transport,port}`.
  `urnetwork_connect_h3_listener_up{transport,port}` is 1 only while the
  listener is accepting. `/status` returns 503 while any enabled listener is
  down.
- `[c]h3 accept connection <backend-listen-address>` with no PP error = QUIC
  completed far enough for quic-go's listener to accept. Repeated accepts
  immediately after the new boundary are the behavior signal that PPv2 is
  actually live.
- `[c]h3 connection exited ... err = <auth/frame error>` is above transport;
  PP, QUIC handshake, and return routing already worked. A clean exit is an
  ordinary client close.

HEALTHY is not merely zero PP errors (there might be zero attempts). Require
at least one post-boundary H3 accept from a real or synthetic QUIC client, and
require the socket for every enabled allocation to remain bound. Derive the
port from each current container rather than hard-coding a generation's port:
```
sudo docker inspect <connect-container> \
  --format '{{range .Config.Env}}{{println .}}{{end}}' | grep '^WARP_PORTS='
sudo ss -lunpH | grep -E '172[.]18[.]0[.]1:<allocated-443-4053-or-8053>\b'
```
During migration, a five-block edge expects five direct-443, five DNS/4053,
and five compatibility DNS/8053 sockets. A missing socket with an `Up`
container is BROKEN. A ready status also carries
`X-UR-Connect-Listeners-Ready: 1` and a sorted
`X-UR-Connect-UDP-Listeners: 443,4053,8053`; Warp requires the exact ports,
so a stale 443/8053 unit cannot authorize a 53→4053 LB activation.

### 16.3 Authenticated H3 carrier traffic — handshake vs actual use

Connect exports closed-label counters that advance only after authenticated
H3 DATAGRAM capability negotiation. They distinguish "QUIC can handshake"
from "the app is routing messages on H3":
```promql
sum by (host, block, event) (
  increase(urnetwork_connect_h3_datagram_events_total[5m])
)
```

The positive-path events are `received_message`, `sent_message`,
`received_fragment`, `sent_fragment`, `stream_received_message`, and
`stream_sent_message`. Stream events are not an H1 fallback: hybrid H3 keeps a
reliable QUIC stream lane for messages above the DATAGRAM threshold. Error
events (`send_error`, `malformed_fragment`, `checksum_failure`,
`reassembly_timeout`, `reassembly_limit`, queue oversize/wait) should remain
zero or at their established near-zero baseline.

Use `increase`, not a raw fleet sum: every process has a random `instance`
label, and old/new connect generations push concurrently during a drain. A raw
total retains stale old instances and resets per process. Interpret with a
known traffic stimulus:

- H3 accepts + rising received/sent events = auth, negotiation, and routed app
  traffic are all using H3. This is the strongest server-side success signal.
- H3 accepts + flat events while a test client sends known routed messages =
  handshake/auth only; inspect client Auto-mode election and route selection.
- Flat events with no known traffic is inconclusive, not broken.
- Rising error events identify the post-auth DATAGRAM/reassembly layer; they
  are not PP or SNAT failures.

### 16.4 Public ingress and return-SNAT invariant

Resolve immediately before probing: Route53 health rotation can change both
the IP and interface during a rollout. Record the tuple and pin the probe to it
rather than attributing counters from a prior DNS answer.
```
dig +short A connect.bringyour.com
dig +short AAAA connect.bringyour.com

# On the host/interface owning the selected address. NAT counters reset when
# warpctl recreates its chains, so compare deltas around one probe.
sudo iptables-save -t nat -c | grep -E -- '--dport 443|dpt:443'
sudo ip6tables-save -t nat -c | grep -E -- '--dport 443|dpt:443'
sudo conntrack -L -f ipv4 -p udp --dport 443
sudo conntrack -L -f ipv6 -p udp --dport 443
```

For a probe to public `P:443`, the successful invariant is:

1. the selected public-interface DNAT counter increments;
2. connect emits a post-boundary H3 accept (and, for the full probe, auth
   succeeds);
3. the matching conntrack entry becomes `[ASSURED]`, not only `[UNREPLIED]`;
4. a client-side packet observer sees every server datagram sourced from the
   exact selected `P:443` tuple.

Conntrack displays the pre-reverse-NAT reply tuple (for example an internal LB
address and allocated port); that is normal. Its original tuple retains
`dst=P dport=443`, and the established NAT mapping rewrites the reply back to
`src=P sport=443`. Pair `[ASSURED]` with a successful QUIC handshake for the
practical proof: packets traveled both directions and quic-go accepted them as
belonging to the connection addressed to `P:443`. A client-side source-tuple
capture remains the strongest regression probe.

- Mostly `[UNREPLIED]` plus PP errors: fix PP/listener generation first; SNAT
  has not been exercised.
- H3 accept plus `[ASSURED]`: ingress, backend PPv2, QUIC response, and reverse
  NAT work. If the app still reports H1, inspect client mode election/auth
  above transport.
- Backend sends but the client observes a different source IP or port (or QUIC
  times out while a separate reply flow appears): UDP return-SNAT regression.
  Inspect the interface-scoped POSTROUTING rules and whether another LB block's
  cleanup removed this block's SNAT state; do not paper over it in the client.

### 16.5 Direct UDP/443 loss — localize before calling it QUIC loss

An H3 timeout can be a real lossy path, but it can also be deterministic
blackholing inside the edge. Separate these cases in this order:

1. pin one public `P:443` target and observe the raw client socket;
2. prove the first flow reached that interface with its DNAT delta and exact
   conntrack row;
3. map nginx's current logical upstreams through host DNAT to every connect
   block's allocated port;
4. require an actual UDP socket at every allocated port, then correlate nginx
   `ECONNREFUSED`, kernel `UdpNoPorts`, and per-block H3 accepts;
5. only on a bidirectional, accepted flow interpret qlog loss/drop events and
   authenticated H3 DATAGRAM counters as transport quality.

Useful edge-side split:
```
sudo nstat -az UdpNoPorts UdpInErrors UdpRcvbufErrors
sudo ss -lunpH
sudo iptables-save -t nat -c \
  | grep -E -- '--dport <logical-upstream>|dpt:<logical-upstream>'
sudo docker logs --since <boundary> <lb-container> 2>&1 \
  | grep -E 'recv\(\)|sendmsg\(\).*111: Connection refused'
sudo docker logs --since <boundary> <connect-container> 2>&1 \
  | grep -E '\[c\]h3 (accept connection|connection exited)'
```

Interpretation:

- A public DNAT counter is a **new-flow** signal, not a packet counter. After
  conntrack creates the mapping, later datagrams bypass the nat table. A +1
  delta proves the first Initial reached the host rule; it does not prove all
  retransmissions arrived.
- Exact `[UNREPLIED]` conntrack plus a DNAT delta and zero raw client reads
  proves ingress to the host but no response through that mapping. If the
  selected backend port has no socket and nginx logs `ECONNREFUSED`, the loss
  boundary is nginx-to-connect. Return SNAT has not been exercised.
- `UdpNoPorts` rising during the probe is the kernel signature for datagrams
  delivered to closed UDP allocations. Pair it with the current host DNAT
  mapping; a fleet-wide raw value alone includes unrelated UDP traffic.
- `UdpRcvbufErrors`/`UdpInErrors`, NIC missed/error counters, qdisc drops, and
  conntrack occupancy are the competing host-capacity signals. Flat values
  while `UdpNoPorts` and LB `ECONNREFUSED` rise rule out receive-buffer,
  physical-link, and conntrack pressure.
- A successful handshake with every raw reply sourced from exact `P:443`
  proves reverse conntrack NAT/SNAT for that flow. Do not infer a return-SNAT
  regression from another flow that never got a backend response.

Client qlog needs one important qualification. `LostPacketCount` counts
quic-go `PacketLost` events, not every missing response. Initial PTO probes can
be sent repeatedly without a loss declaration before the handshake deadline.
Therefore `q_lost=0`, many raw writes, zero raw reads, and a handshake timeout
is a blackhole, not a clean path. Use qlog loss percentage to characterize an
already established flow; use handshake duration, PTO-shaped raw write count,
and raw reads to characterize startup. Healthy startup is a tight latency
cluster with an immediate raw response, not merely zero declared loss. Current
client stats expose `ProbeTimeoutCount`, `HandshakeAttemptCount`,
`HandshakeSuccessCount`, `HandshakeFailureCount`, and
`HandshakeSentWithoutResponseCount`; the last counter requires sent>0 and
received=0 for the individual attempt. `DialEarly` 0-RTT readiness is not
counted as success: the attempt remains open until QUIC handshake completion.

The backend-listener health gate is:

- every current connect block's `WARP_PORTS` 443 allocation has one bound UDP
  socket;
- `urnetwork_connect_h3_listener_up{transport="h3",port="443"}=1` for every
  block, with no restart/failure growth during the probe;
- PP-drop counters are flat or explainable and never coincide with a missing
  listener (§16.2);
- nginx emits no UDP-upstream `ECONNREFUSED`, and `UdpNoPorts` is flat under a
  pinned probe;
- every weighted public target completes repeated handshakes without a PTO
  staircase; and
- a paced established-flow probe has zero/near-baseline qlog loss/drop and all
  replies use the selected public tuple.

2026-08-20 incident baseline: 250 paced 1,000-byte DATAGRAM frames at 8 ms were
lossless after connection establishment on `.41`, `.62`, `.70`, `.71`,
`.82`, `.83`, `.84`, and `.85`: qlog reported no lost or dropped packets and
the raw socket saw replies only from the pinned public tuple. This ruled out a
broad QUIC, MTU, checksum, or return-SNAT problem. `.40` and `.42` gave no raw
reply at all and were a separate stale-LB/activation failure despite remaining
low-weight DNS answers.

Repeated handshakes exposed the high-weight split. Edge-4 `.82` and `.83`
passed 20/20 with maxima of 90 ms and 74 ms. Edge-3 `.84` passed only 14/20;
8 successes took at least 1 s and the maximum was 3.071 s. `.85` passed 19/20;
12 successes took at least 1 s and the maximum was 6.271 s. Failures still
incremented the exact public DNAT rule and left an exact `[UNREPLIED]` row,
with 12 raw Initial/PTO writes and zero reads.

The edge-3 fanout explained the distribution. Nginx assigned direct H3 with
weights beta/g1/g2/g3/g4 = `1/24/25/25/25`, but only beta and g4 had their
allocated UDP/443 sockets. Host DNAT counters showed traffic continuing to
g1-g3, nginx logged UDP-upstream `Connection refused`, and `UdpNoPorts` rose
from 63,841 to 66,922 during the investigation while `UdpRcvbufErrors` stayed
at 26. Thus 74% of first backend choices hit a closed port. ICMP
port-unreachable closed those nginx UDP pseudo-sessions; later QUIC PTOs could
create a new pseudo-session and randomly reach beta/g4, producing the observed
~0.7/1.5/3.1/6.3-second staircase instead of uniform random packet loss.

The missing sockets were a listener-lifecycle failure. New g1, g2, and g3
started at `03:16:30Z`, `03:16:35Z`, and `03:16:51Z`; each then logged exactly
one listener exit (`header required but not found`, `header must be UDP`, and
`header required but not found`) at `03:16:52Z`, `03:16:51Z`, and `03:17:06Z`.
The old PP-incompatible LB generation continued draining while the new LBs
started at `03:16:51Z` and `03:16:59Z`, so incompatible UDP pseudo-sessions
overlapped the new connect listeners. G4 started at `03:17:11Z`, survived, and
continued accepting. All five DNS/8053 sockets remained bound, which is why
direct H3 could be broken independently of WhoDis. The immediate recovery is
to recreate the missing connect listeners after the incompatible LB traffic
is gone; the durable requirements are a non-fatal malformed-datagram path or
a supervised listener restart, logged bind failures, and readiness/monitoring
that fails when any enabled allocation disappears.

### 16.6 WhoDis DNS carriers — public UDP/53 to private UDP/4053

`H3Dns` and `H3DnsPump` are QUIC wrapped in DNS TXT request/response
envelopes, not DNS resolvers and not HTTP/3. The client keeps SNI
`connect.bringyour.com`, but the destination lookup differs:

- `h3dns` resolves `connect.bringyour.com:<DnsPort>`;
- `h3dnspump` resolves the explicit `whodis.bringyour.com:<DnsPort>` discovery
  endpoint and continuously supplies requests against which replies can be
  paired. The packet envelope still uses the configured canonical `ur.xyz.`
  TLD; the destination hostname is deliberately independent of that codec
  value.

Both names are weighted and their eligible address sets can overlap. Never
infer a fixed address pool from the mode or from one resolver's cached answer.
An ordinary HTTP status health check can keep an interface in Route53 while
its UDP/53 path is broken. Enumerate/pin the returned address immediately
before the transport probe; the fleet gate is **every eligible weighted target
passes**, not "one target worked."

The intended IPv4 path is:
```
client DNS envelope -> public P:53/udp -> interface-scoped warp DNAT
                    -> LB logical UDP/4053 -> nginx PPv2
                    -> connect UDP/4053 -> PP decode -> DNS decode -> QUIC
reply              <- reverse conntrack NAT                    <-
```

Policy invariants:

- UDP/53 is an IPv4-only forward alias to service port 4053. Absence of an
  IPv6 port-53 alias is intentional until product policy changes.
- UDP/4053 must stay private: there must be no direct public 4053 rule.
- UDP/8053 is a private compatibility listener only during the rolling
  migration. It must have no direct public rule and must be removed after old
  LBs have drained.
- TCP/53 is unrelated and must not be created by this mapping.
- The server transform order is PPv2 first, DNS `decode53` second, then QUIC.

Inspect one selected interface around one forced-mode attempt:
```
dig +short A connect.bringyour.com
dig +short A whodis.bringyour.com

sudo iptables-save -t nat -c \
  | grep -E -- '--dport (53|4053|8053)|dpt:(53|4053|8053)'
sudo ip6tables-save -t nat -c \
  | grep -E -- '--dport (53|4053|8053)|dpt:(53|4053|8053)'
sudo conntrack -L -f ipv4 -p udp --dport 53
sudo docker logs --since <boundary> <connect-container> 2>&1 \
  | grep -E '\[c\]h3 accept connection|proxy protocol'
```

A plain `dig` is only an ingress-counter stimulus. `decode53` deliberately
does not answer ordinary non-transport questions, so a DNS timeout is expected
and cannot test WhoDis. The positive probe must encode real QUIC packets in
each mode. It should also observe the raw socket beneath packet translation so
it can prove requests were emitted and all replies came from the exact
selected `P:53` tuple.

For a successful forced probe to `P:53`, require all of:

1. raw DNS envelopes were sent to `P:53`;
2. the exact IPv4 DNAT counter increments;
3. connect logs an H3 accept on its **4053 allocation**, not the direct-443
   allocation (an 8053 accept is expected only from a draining old LB);
4. the flow is `[ASSURED]` in conntrack;
5. every client-observed DNS response is sourced from `P:53`;
6. for the full health probe, auth and a routed connect message succeed.

The boundary split is unusually sharp:

- Raw requests sent, but DNAT stays at zero: traffic did not reach that host
  rule. Capture on the public NIC and inspect raw/nft/filter rules. No ingress
  capture plus no earlier host drop is an upstream firewall/routing failure;
  changing connect or nginx cannot fix it.
- The unit/process contains UDP/4053 and `--forwardports`, but the interface
  has no port-53 rule: the LB activation never reached `redirect()`. Inspect
  the actual LB generation and `WARP_PORTS`. A worker repeatedly scanning
  occupied ports while an old LB owns every slot is port-pool/drain starvation,
  not a firewall rule-generation bug (§11.7c, §11.8).
- DNAT rises, but there is no H3 accept on a 4053 allocation: inspect the
  running nginx UDP/4053 listener, PPv2 errors, backend selection, and DNS
  decode. This is ingress/LB/decode, not client auth.
- H3 accepts and conntrack is `[ASSURED]`, but the raw client sees another
  source address/port: return-SNAT regression.
- Handshake and tuple proof pass, but authenticated traffic does not: move up
  to auth, mode election, and application routing (§16.3).

Historical 2026-08-19/20 8053 baseline: forced `h3dns` and `h3dnspump`
handshakes to a known
forwarded target (`65.19.157.62:53`) completed in about 0.4s, produced 21 DNS
responses from that exact public tuple, logged accepts on the connect 8053
allocation, and left `[ASSURED]` conntrack rows. Pump-mode probes also passed
on `.41`, `.70`, and `.71`. This proves both codecs, PPv2/DNS decode, QUIC,
and return-SNAT before the private-port move. The post-migration acceptance
gate is the same matrix with accepts on 4053 for every eligible target.

The same matrix exposed two independent partial failures. `.42` and `.40`
had correct restarted unit arguments but no live port-53 DNAT: their LB
workers were still scanning fully occupied old port pools, so no new LB had
deployed and `redirect()` had not run. More importantly, all four directly
addressed targets `.82` through `.85` had exact host DNAT rules and healthy
new LBs, but forced clients sent 120 envelopes per target with no response and
every DNAT counter stayed zero. UFW was inactive, no earlier host raw/filter
drop existed, and an edge-4 capture saw no external probe packets. The fault
was upstream UDP/53 delivery. At that time Route53 continued returning those
high-weight addresses because their ordinary status health checks passed, so
the production hostnames were probabilistically broken even though selected
legacy targets worked. The `.82`-through-`.85` upstream delivery fault was
fixed on 2026-08-20. Retain this paragraph only as the historical signature;
it is not current failure state. Forced `h3dns` and `h3dnspump` probes pinned
to every eligible target remain the post-fix acceptance gate.

A client-side preflight has its own sharp signature. If forced
`h3dnspump` reports zero QUIC connections, zero handshake attempts, and a
resolver error for `zone-ur-xyz-.bringyour.com`, no packet reached UDP/53.
The canonical codec TLD (`ur.xyz.`) was incorrectly copied into a DNS label
with its trailing dot and the `connect` label was incorrectly removed from
the platform hostname. The fixed client no longer derives infrastructure
names from codec data: it resolves the explicit `whodis.bringyour.com` alias
while retaining `connect.bringyour.com` for QUIC SNI/auth. Keep this separate
from the server-side zero-DNAT signal: both have zero edge counters, but only
this case has a local name-resolution error and zero raw writes.

The `whodis.bringyour.com` discovery record was deployed on 2026-08-20 at
07:25:35Z as an IPv4-only Route53 weighted alias (`main-lb`, weight 100) to
`main-lb.bringyour.com`, with target-health evaluation enabled (change
`C0194887P6U8F3TB0CUC`). Route53 reached `INSYNC`, and all four authoritative
servers returned an eligible edge IPv4 address. The absence of an AAAA record
is deliberate because public UDP/53 is IPv4-only. Immediately after creating
a previously nonexistent name, recursive resolvers can retain the old negative
answer until its cache expires; distinguish that propagation window from a bad
record by querying an authoritative server directly. Do not release a client
that requires this name until its deployment resolvers return an A answer.

### 16.7 2026-08-20 root causes, allocation move, and rollout gate

The high apparent UDP/443 loss was not a broad lossy link and was not the
historical reply-SNAT bug. A malformed/headerless/wrong-family PROXY datagram
escaped `PpPacketConn.ReadFrom` after the discard budget, quic-go terminated
the block's shared listener, and the healthy HTTP container stayed `Up`.
Nginx continued selecting the closed allocation, producing `ECONNREFUSED`,
`UdpNoPorts`, `[UNREPLIED]` flows, and PTO staircases. Recycling edge-3 g1–g3
restored the sockets after the incompatible LB generation drained. The
durable correction is the non-fatal PP path plus supervised/dynamically-ready
listeners in §16.2.

WhoDis then exposed a separate Warp allocation-lifecycle bug. Grafana's
running processes and current units already used external 7183–7190, but its
iptables chain still held withdrawn DNAT rules at 7176–7182. `redirect()` only
reconciled ports present in the new allocation; it never deleted an external
or internal port that disappeared entirely. Those stale same-chain rules
overlapped Connect's 8053 logical allocations at 7178–7182 and stole packets
before Connect. This was not a PP socket defect and was not UDP return SNAT.

The 2026-08-20 emergency recovery removed only Grafana's stale unscoped UDP
DNAT rules for external ports 7178–7182 on reachable edge-0, edge-1, edge-3,
and edge-4. It did not touch TCP, Grafana's withdrawn 7176/7177 rules, Connect
DNAT, units, containers, or listeners. Post-change every repaired host had
zero matching Grafana UDP rules, all five Connect 8053 DNAT rules, and all
five Connect 8053 sockets. Edge-5 (`172.28.208.176`) was unreachable over SSH
and was not mutated. This is an operational hotfix, not the durable allocator
repair below.

Post-hotfix forced `h3dns` and fixed-client `h3dnspump` QUIC handshakes passed
on `65.49.70.82` through `.85` in roughly 0.23–0.28 seconds. The exact public
UDP/53 DNAT counters rose and matching conntrack entries were `[ASSURED]`, so
the repaired path exercised ingress, DNS decode, PPv2, QUIC reply, and reverse
NAT rather than merely proving that an iptables rule existed.

Warp now:

- removes unscoped DNAT/REDIRECT rules absent from the block's complete current
  external+internal allocation, while preserving draining-generation ports and
  interface-scoped public aliases;
- refuses activation if any desired external or internal port is still owned
  by another `WARP-<ENV>-*` chain; and
- before an LB with UDP aliases activates, polls every Connect block directly,
  requires HTTP success, `X-UR-Connect-Listeners-Ready: 1`, and an exact
  `X-UR-Connect-UDP-Listeners` set containing all required service ports.

Services version v21 allocates the new path without reusing a live number:

- LB service UDP/4053: a host/interface-specific external allocation (for
  example 7191–7192 on edge-3; never hard-code this fleet-wide);
- Connect UDP/4053: external 7193–7197, internal 15058–15207;
- compatibility Connect UDP/8053 remains external 7178–7182, internal
  14578–14727 until old LBs drain; and
- public IPv4 UDP/53 forwards to logical 4053, never directly to those host
  allocation numbers.

Rollout order is correctness-sensitive:

1. install the new Warp binary and generated v21 units;
2. reconcile/redeploy Grafana first so its withdrawn 7176–7182 rules disappear;
3. deploy Connect and require every block status to report 443,4053,8053;
4. deploy/redeploy the LBs, which changes public UDP/53 to logical 4053 and is
   blocked by the per-port Connect readiness gate; and
5. after every pre-v21 LB has drained, create a later services version without
   8053 and remove the compatibility listeners.

Do not deploy the LB first: it would send port 53 to a service port absent from
old Connect units. Do not deploy Connect before the Grafana reconciliation
while 8053 compatibility is present: the cross-chain ownership guard will
correctly refuse the collision rather than silently overlap it.

Pre-Connect-image checkpoint at 2026-08-20 09:21Z: the published/running
Connect generation was still `2026.8.19+1023689220`, so the four reachable
five-block edges correctly exposed TCP/80, UDP/443, and compatibility UDP/8053
on all 20 current blocks, but exposed UDP/4053 on 0/20 and returned HTTP 200
without either listener-readiness header. Do not classify that exact shape as
a listener regression before the new Connect image is published. The v21 LB
candidates had 443/4053/8053 plus the 53-to-4053 alias, saw the missing
readiness header, refused activation, and left the old public DNAT generation
serving. This is the intended rollout gate.

At the same checkpoint, pinned QUIC handshakes for direct H3, `h3dns`, and
`h3dnspump` all passed on eight public targets (`.62`, `.71`, `.70`, `.41`,
`.82`–`.85`) with replies from the exact requested public source port. All
three modes timed out with zero received datagrams on `.40` and `.42`, even
though every block's HTTPS status path and the HTTP-only Route53 health checks
passed there. Those two interfaces were still serving port-pool-starved old LB
generations and had no current UDP/53 alias; HTTP target health therefore must
not be used as UDP eligibility. Edge-5 `.91` failed both
HTTP and all three QUIC modes and Route53 correctly marked it unhealthy. Until
`.40` and `.42` are recovered or withdrawn, main is partially available—not
fleet healthy—for H3 and both WhoDis carriers. The successful DNS probes still used
the legacy 8053 backend; repeat the full pinned matrix after the new Connect
image makes 4053 ready and the gated LB generation activates.

The `.40`/`.42` pool exhaustion was a Warp lifecycle leak, not ordinary QUIC
packet loss. Each affected LB block had all 30 internal-port generations still
`Up`; exactly one newest old container owned every target referenced by the
live DNAT chain and the other 29 owned distinct, unreferenced port tuples. A
failed deployment previously launched its candidate stop in a goroutine, and
successful deployment launched old-generation drains the same way. A worker
process exit could kill either cleanup before Docker stopped the container;
the next worker then allocated another tuple until no complete tuple remained.
`assignDeployPorts` could only log occupied ports and wait forever.

The durable Warp behavior is restart-safe: a failed candidate is stopped
synchronously before `deploy()` returns, and a host-networked worker startup
inspects same-block containers plus its live DNAT chain. It preserves every
container that owns any active target, resumes graceful drain for only the
unreferenced containers, and refuses cleanup if inspection is malformed, the
chain has no target, or no running container owns a target. This recovery must
run before allocation so a full pool can free a complete tuple without an
operator deleting containers or guessing which generation is serving.

The manual `.40`/`.42` recovery exposed two more lifecycle defects. First,
`assignDeployPorts()` waited inside `deploy()` while retaining the version that
was current when the pool became full. Once an operator freed a tuple, that old
call resumed immediately and started the captured 2026-08-19 image even though
2026-08-20 was then desired. Allocation is now a single attempt: an occupied
pool returns to the outer watcher, which repolls image and config versions
before every retry.

Second, both replacement workers crashed during the IPv6 half of `redirect()`.
These interfaces have a dual-stack Warp Docker bridge but no IPv6 public
routing-table interface. The UDP SNAT branch checked the aggregate
`self.routingTable`, then dereferenced the nil family-specific
`networkConfig.routingTable` after DNAT was partly changed. The worker restart
created another candidate and repeated the partial transition. SNAT is now
entered only when the current address family has a routing-table interface; a
regression test retains private IPv6 DNAT while proving that family gets no
public SNAT.

At the 2026-08-20 09:53Z recovery checkpoint, the old 30-container pools and
all crash-loop candidates were removed. `.40` retained only current LB
`0f347fd2033c`; `.42` retained only current LB `219fc9d9833b` after deleting one
stale, lower-priority TCP/443 DNAT to `beaea8ee28cb` and gracefully stopping
that duplicate. No live rule referenced a removed tuple. The two affected
warpctl units remain intentionally stopped until the corrected Warp binary is
deployed; the retained Docker LBs continue serving. Pinned client handshakes
then passed on both `65.19.157.40` and `.42`: direct H3 in 74–105 ms,
`h3dns` in 275–287 ms, and `h3dnspump` in 292–301 ms. H1 also reached both in
about 0.25 seconds and returned the expected unauthenticated HTTP 403. Do not
confuse these targets with the same suffixes in the `65.49.70.64/27` range.

At the 2026-08-20 post-deployment verification checkpoint, edge-0, edge-1,
edge-3, and edge-4 all ran the same new warpctl binary (SHA-256
`8890e7146b6165c0a11a342b2a9c8b790a1591924a3515e5f65a9b1a916d97b3`).
Every main Connect and LB unit was active with zero systemd restarts, and the
deployment window contained no panic, deploy-failure, reconcile-failure,
pool-occupied, or listener-readiness-failure log. The `.40` and `.42` workers
successfully restarted under this binary.

All 20 current Connect blocks returned HTTP 200 with
`X-UR-Connect-Listeners-Ready: 1`, advertised exactly UDP 443, 4053, and 8053,
and owned each corresponding allocated socket. Every current LB owned its
allocated 443/4053/8053 sockets, and every reachable interface had exactly one
public UDP/53 DNAT to its latest LB's logical 4053 target. Forced probes against
all ten reachable public IPv4 interface addresses passed H1, direct H3,
`h3dns`, and `h3dnspump`; the raw QUIC probes both transmitted and received and
verified the exact selected public reply address and port. Connect accepts on
the 4053 allocation proved that the DNS carriers used the new path rather than
compatibility 8053.

No Connect or LB block had more than the current generation plus one old
graceful-drain generation, and some old Connect generations had already
exited. This bounded two-generation shape is expected during deployment and is
not the old orphan leak; treat a third running generation or unbounded growth
as the regression signal.

Two fleet exceptions remain. Edge-5 (`172.28.208.176`, public `.91`) was
unreachable from management and from every other reachable edge; H1 and all
three QUIC modes timed out, and Route53 health checks correctly excluded it.
Only three of nine configured public IPv6 interfaces passed forced H1 and H3:
edge-0 eno4, edge-1 eno3, and edge-4 eno4. Route53 health checks excluded the
other six and authoritative AAAA answers contained only those three healthy
addresses. Thus the eligible serving set is healthy, but the complete physical
edge/interface inventory is not.

The rollout also exposed a policy error independent of QUIC health: current LB
chains directly publish both TCP and UDP service port 8053 on every reachable
interface. UDP/4053 remains private and public UDP/53 correctly aliases 4053,
but compatibility 8053 is required to remain private too. Warp's
`publicPortServiceTargets()` currently suppresses only the present forward
target, so changing `udp:53` from 8053 to 4053 made 8053 public again. A direct
public 8053 rule is therefore a failed policy check even when transport probes
pass.

At the 2026-08-20 17:16Z follow-up, all ten reachable IPv4 interfaces again
passed direct H3, `h3dns`, and `h3dnspump` with exact return tuples. All 20
Connect status endpoints still returned HTTP 200, listener-ready 1, and the
exact UDP listener set 443,4053,8053. The four reachable hosts still ran the
same warpctl hash, all 30 current Connect/LB units had zero systemd restarts,
and the recent lifecycle-error search was empty. Edge-5 and `.91` remained
unreachable and had 0/16 successful Route53 health-check observations.

The old generation drains were bounded but not complete. Process inspection
found 4 LB masters across edge-0's 3 blocks, 6 across edge-1's 3, 4 across
edge-3's 2, and 4 across edge-4's 2. Connect process counts were respectively
5, 5, 6, and 7 for five blocks per host. This is still at most one predecessor
per block and is shrinking from the earlier two-generation shape. Each host
had an active `docker container stop -t 3600` owned by the correct warpctl
worker (edge-4 had one LB and one Connect stop); remaining blocks wait behind
the per-service host drain lock. That is the expected staggered one-hour drain,
not an orphan or port-pool leak.

IPv6 availability narrowed from three interfaces to two: edge-0 eno4
(`2001:470:99:57:e643:4bff:fe23:a343`) began refusing H1 and timing out on H3.
Its Route53 health check also changed to 0/16 successes, so authoritative AAAA
selection excluded it. Edge-1 eno3 and edge-4 eno4 remained 16/16 healthy and
passed the pinned H1/H3 probes. The eligible serving set therefore remained
healthy despite the additional physical-interface failure.

The 8053 policy fix now carries the immediately previous services version's
forward target into generated LB units as `--privateports`. Runtime public-rule
selection suppresses both current forward targets and those rolling private
ports across TCP, UDP, IPv4, and IPv6. Regression coverage verifies the v20
53-to-8053 to v21 53-to-4053 unit rendering and starts from stale public TCP
and UDP rules for both 4053 and 8053, requiring their deletion while preserving
only public UDP/53 to private 4053. The full Warp suite and the focused race
suite pass. This source fix is not deployed yet: current units have no
`--privateports`, and host journals show the old warpctl inserting public 8053
DNAT. Direct public-8053 QUIC probes time out only because the upstream firewall
blocks that port; regenerate the units and deploy the rebuilt warpctl to remove
the host rules themselves.

That deployment warning is historical as of the 2026-08-20 20:01Z checkpoint.
Edge-0, edge-1, edge-3, and edge-4 now run the same rebuilt warpctl (SHA-256
`ac84483944b72572ada672bf2fcdb1287344385c5d2af58a0241e0e46ba2829b`).
All ten enabled LB units carry both `--forwardports=udp:53:4053` and
`--privateports=8053`, and every unit recorded an explicit successful deployment
of LB version `2026.8.20-outerwerld+1024531200`. Surviving reconciliation records
show the rebuilt worker deleting old public TCP/UDP 8053 DNAT while preserving
public UDP/53 to private 4053. Thus the source, generated-unit, and deployed-LB
sides of the 8053 privacy fix are all live.

All 102 enabled `warp-main-*` units on those four hosts were active/running
(26, 26, 25, and 25 respectively), with no enabled-but-inactive unit, nonzero
systemd restart count, or nonzero main exit status. All 88 HTTP-capable
non-LB blocks (API, app, Connect, Grafana, MCP, taskworker, and web) returned
HTTP 200 with `status: ok`. All 20 Connect block status endpoints additionally
returned `X-UR-Connect-Listeners-Ready: 1` and exactly
`X-UR-Connect-UDP-Listeners: 443,4053,8053`. A fresh pinned wire matrix passed
H1, direct H3, `h3dns`, and `h3dnspump` on all ten reachable IPv4 interfaces.
Every QUIC carrier both sent and received and the observed wire return tuple was
the exact selected public IPv4 with source UDP/443 or UDP/53 as appropriate.
Edge-5 and `65.49.70.91` remained unreachable over management, H1, and all three
QUIC carriers.

Route53 agreed with the wire result: each of the ten reachable A checks had
16/16 successful observations and edge-5 had 0/16. IPv6 improved to five
healthy direct H1/H3 interfaces, each also 16/16 in Route53: edge-0 eno4,
edge-1 eno2 and eno3, and edge-4 eno3 and eno4. Edge-0 eno2, both edge-3
interfaces, and edge-5 remained 0/16. Do not require the DNS carrier on an IPv6
tuple: `whodis.bringyour.com` is deliberately an A-only health-evaluated alias
of `main-lb.bringyour.com`, and Warp's current product policy deliberately omits
the public 53-to-4053 forward alias on IPv6. Direct H1/H3 are the IPv6 gates.

The rollout was serving but had not yet converged to one LB generation. Two LB
image versions arrived within minutes of each other, so edge-1, edge-3, and
edge-4 temporarily had the current, intermediate, and pre-rollout nginx master
on each interface; edge-0 had one or two masters per interface. The workers had
active, correctly parented `docker container stop -t 3600` drains and the counts
were shrinking (Connect was already at exactly five processes on edge-0/1 and
five current plus two predecessors on edge-3/4). This bounded three-generation
shape is explained by two back-to-back successful deployments, not by an
unowned orphan. A third generation without two deployment-success records, or
one that remains after the serialized one-hour drains have completed, is still
the orphan-regression signal.

Finally, every LB activation triggered journald rate limiting: between 81,710
and 112,998 worker messages per LB unit were suppressed. The flood is dominated
by per-port stale-conntrack cleanup after the large `netstat` snapshot. During
such a rollout, an empty grep for deploy or firewall errors is not evidence;
require the explicit final `Deploy success`, active unit and restart state,
Connect listener readiness, Route53 observations, and the pinned wire matrix.
The corrected warpctl captures the netstat snapshot without logging it, scans
conntrack once per address family for the Docker-network reply source, and
deletes only returned reply ports that are in the configured pool but have no
live/draining listener. It emits one `Socket discovery ... scanned=...
occupied_pool_ports=... duration=...` record and one `Conntrack cleanup ...
family=... scanned=... candidate_ports=... stale_ports=... deleted_flows=...
errors=... duration=...` record per family. Any raw socket/flow rows, per-pool-
port delete output, or new journald suppression after that warpctl is deployed
is a regression; the source correction is not live until warpctl is rebuilt and
rolled out.

### 16.8 Incident-shaped playbook

1. Resolve A/AAAA; map the selected public address to one edge/interface.
2. Record desired connect/LB versions, then read actual containers and status.
3. Compare the LB unit's baked `--portblocks`/`--forwardports`, container
   `WARP_PORTS`, and nginx `listen` ports. An Exited(2) replacement means the
   old generation still defines behavior.
4. Prove nginx provenance and `proxy_protocol v2` from the running container.
5. Derive every connect block's 443/4053/8053 migration allocation from current
   `WARP_PORTS` and require each expected UDP socket in `ss`. Also require the
   readiness headers to list those exact ports. An `Up` container is not this
   check.
6. Start one real H3 attempt pinned to the recorded public tuple. For WhoDis,
   force each of `h3dns` and `h3dnspump`; a plain DNS query is insufficient.
7. Read direct connect logs and PP/listener counters from the same UTC window.
   Current PP rejects are per-datagram drops; if a legacy PP error escaped on
   the listener's accept line, use the listener gauge/socket and later restart
   log to determine whether that pre-fix allocation exited.
8. Compare public DNAT counter deltas and the exact conntrack row. Require
   `[ASSURED]`; capture the client-observed source tuple when testing SNAT.
   A zero UDP/53 delta after confirmed raw sends is an ingress-firewall split,
   while a missing rule despite correct args is an LB-activation split.
   For direct H3, `[UNREPLIED]` plus LB `ECONNREFUSED` and rising `UdpNoPorts`
   is a closed connect allocation (§16.5).
9. Only after H3 accept + bidirectional tuple proof, move upward to auth,
   DATAGRAM negotiation, client transport availability/election, and routed
   traffic.

2026-08-19 baseline: the old serving nginx 1.30.4 config used
`proxy_protocol on` and connect logged both PP classes above. The PPv2-capable
LB replacement first exited with `Missing host port for service port 8053`
because `xops/main/ansible/run-edges.sh` had deployed stale systemd units.
After regenerated units supplied UDP/8053 and `udp:53:8053`, the pinned nginx
1.31.4 LB stayed up, connect logged repeated H3 accepts, and public IPv4
UDP/443 flows reached `[ASSURED]`. The new edge-3 g4 counters then rose by
thousands of received/sent H3 DATAGRAM messages with zero carrier errors,
proving routed app traffic rather than handshake-only success. That sequence
is the known-good recovery shape for direct H3. The DNS-carrier matrix and
failure split are recorded in §16.6.

## 17. Subtensor RPC gateway (snow)

Snow provides the testfinney archive node on loopback `127.0.0.1:9945` and an
nginx HTTP/WebSocket gateway on overlay `172.28.208.185:9944`. Treat the node,
gateway, and overlay lifecycle as separate layers. In particular,
`subtensor.service` is a `Type=oneshot` Docker Compose launcher, so
`active (exited)` proves only that Compose accepted the start; it does not prove
that the container is current, peered, syncing, or reachable through nginx.

### 17.1 Listener and deployment identity

Run these together on snow:
```
systemctl show subtensor nginx openvpn@by-pre \
  -p Id -p ActiveState -p SubState -p Result -p NRestarts -p ExecMainStatus
ip -brief address show tun0
ss -lntp | grep -E ':(9944|9945|30333)\b'
docker ps --filter name=subtensor --format '{{.Image}} {{.Status}}'
grep -E '^\s*image:|--(chain|sync|pruning|database|bootnodes)' \
  /etc/subtensor/docker-compose.yml
test -s /etc/subtensor/preflight.json && cat /etc/subtensor/preflight.json
journalctl -u nginx -b --no-pager | tail -100
```
Required shape: `tun0` owns `172.28.208.185`; nginx listens on that exact
address at 9944; the node listens on loopback 9945 and P2P 30333; the container
uses the pinned RaoFoundation digest; and `preflight.json` records the expected
testfinney identity. During archive bootstrap, `ready=false`, `isSyncing=true`,
and a historical runtime version are expected. Before the historical head
reaches Frontier, `evm_chain_id` can be unavailable and
`evm_chain_id_error.message` can say that
`EthereumRuntimeRPCApi_chain_id` is not found; this is bootstrap state only
when peers and heads are progressing. Application cutover requires
`ready=true`, `isSyncing=false`, runtime specification 447, transaction version
1, EVM chain ID `0x3b1`, and an available `eth_getLogs`. A missing preflight or
a mutable `v3.x` image is an undeployed/stale node even when the oneshot unit
is green.

P2P listening is not P2P exposure. From an independent internet host, probe
snow's current WAN IPv4 (do not use snow itself; NAT hairpin behavior is not a
public-path proof):
```
# on snow: record WAN and the LAN target
curl -4 -fsS --max-time 5 https://api.ipify.org
ip -4 route get 1.1.1.1
ss -lntp | grep ':30333\b'

# on an independent host
nc -vz -w 5 <snow-wan-ip> 30333
```
Healthy is a completed external TCP handshake plus a rising
`substrate_sub_libp2p_incoming_connections_total`. A timeout while the local
listener exists, UFW is inactive, and conntrack has ample capacity localizes
the fault to upstream NAT/firewall. Forward WAN TCP/30333 to snow's LAN
TCP/30333 and reserve the LAN address; snow currently receives its LAN address
by DHCP, so an unreserved forward can silently drift after a lease change.

### 17.2 Node progress and gateway path

Query the backing node first, then the same RPC through the gateway:
```
rpc='{"jsonrpc":"2.0","id":1,"method":"system_health","params":[]}'
curl -fsS --max-time 5 -H 'content-type: application/json' -d "$rpc" \
  http://127.0.0.1:9945
curl -fsS --max-time 5 http://172.28.208.185:9944/healthz
curl -fsS --max-time 5 -H 'content-type: application/json' -d "$rpc" \
  http://172.28.208.185:9944
```
Also sample `chain_getHeader` and `chain_getFinalizedHead` twice across a useful
interval. Deployment-healthy bootstrap means peers are greater than zero, the
best/finalized head advances, and the direct and proxied identity agree; it is
valid for `isSyncing` to remain true while millions of archive blocks download.
Chain-ready additionally requires `isSyncing=false`, preflight `ready=true`,
and runtime specification 447. An RPC response alone is insufficient: a
zero-peer node can serve a permanently stale local chain.

`isSyncing=false` is especially unsafe by itself. With no peers, Subtensor can
set `system_syncState.highestBlock` equal to its own stale `currentBlock` and
therefore report false even though the public chain is millions of blocks
ahead. Require all of: peers > 0, two advancing head samples, the expected
current runtime, and a comparison against the official RPC head before
declaring convergence. For the P2P layer, distinguish TCP reachability from a
retained peer session:
```
getent ahosts bootnode.test.finney.opentensor.ai
nc -vz -w 5 bootnode.test.finney.opentensor.ai 30333
docker exec subtensor getent ahostsv4 bootnode.test.finney.opentensor.ai
docker exec subtensor timeout 3 bash -c \
  'exec 3<>/dev/tcp/bootnode.test.finney.opentensor.ai/30333'
docker logs --since 10m subtensor | grep -E 'Running (litep2p|libp2p)|peers|Idle|Syncing'
```
A successful host `nc` does not prove the container path. If container DNS
takes longer than the three-second gate while a direct-IP container TCP test
succeeds, fix its resolver inputs before investigating P2P negotiation.

Litep2p discovery can briefly lose the sole retained outbound peer, but repeated
loss is not a healthy steady state. On 2026-08-20 the node imported at 630--685
blocks/s with two peers, spent about 130 seconds at zero peers with a frozen
head, then rediscovered outbound peers without a process restart and resumed.
The cycle repeated. Metrics showed 155 discovered peers, zero inbound
connections, and only outbound opened sessions; an independent edge timed out
to snow's WAN TCP/30333. Treat any zero-peer/frozen-head sample as degraded;
alert if it persists for three minutes, and page if it persists for five.
Recovery requires both public inbound reachability and more than one good RPC
sample. The observed temporary post-recovery proof was 12/12 five-second
samples with two peers and `isSyncing=true`, while the head advanced from
701,762 to 733,678. Never turn a zero-peer `isSyncing=false` sample into a ready
signal.

### 17.3 2026-08-20 incident signature

Two independent faults were present:

1. Nginx tried to bind `172.28.208.185:9944` during boot before
   `openvpn@by-pre.service` had installed that address on `tun0`. It exited with
   `bind() ... failed (99: Cannot assign requested address)`. The stock unit had
   `Restart=no`, so port 9944 stayed closed even after the overlay appeared;
   `nginx -t` then passed, which is the discriminator for this boot race.
2. The node was still the old `ghcr.io/opentensor/subtensor:v3.2.7` deployment,
   runtime specification 212 with RocksDB/pruning 256. It had zero peers and a
   best head stuck at 3,424,064 despite the target already exceeding 7.8 million
   at its prior start. The desired pinned runtime-447 archive deployment and
   `/etc/subtensor/preflight.json` had never reached the host.

The first pinned-image deployment then exposed a playbook bug: it asserted
`isSyncing=false` and exact runtime 447 before installing nginx. At that point
the new node was healthy bootstrap state—one peer, advancing from block 50,296
toward 7,826,287, and reporting historical runtime 135 at that historical
head—but the assertion aborted the play and left port 9944 closed. The corrected
gate requires peer, identity, and head progress during bootstrap; writes
`preflight.json` with `ready=false`; installs and probes nginx; and enforces
runtime 447 only after synchronization converges. The nginx drop-in orders it
after `openvpn@by-pre.service`, waits for the exact overlay address, and restarts
after transient bind failures.

A second run exposed the bootnode-path variant. The node reached block 316,785,
then restarted with zero peers; for minutes its head did not move and
`system_syncState` reported starting/current/highest all equal to 316,785 while
the official target remained above 7.8 million. Both litep2p and libp2p showed
the same result, ruling out the backend. Host DNS and TCP/30333 succeeded, but
inside the container hostname resolution took 8.05 seconds and a five-second
hostname TCP probe emitted no SYN; the same direct-IP container connection
completed in 80ms with correct bridge forwarding, MASQUERADE, and return SYN-ACK.
Snow's resolved config incorrectly preferred `192.168.51.1`, a resolver from a
different site, ahead of its working `192.168.1.1` DHCP resolver. The corrected
host config and Compose service pin reachable IPv4 DNS, and deployment now
requires DNS plus bootnode TCP from inside the container within three seconds.
The gateway include also runs immediately after local RPC liveness and before
peer/progress assertions; even a real P2P failure therefore leaves an
operational, restart-supervised 9944 endpoint while preflight remains not ready.

A later run reached historical runtime 156, where `eth_getLogs` returned an
empty result successfully but `eth_chainId` returned JSON-RPC error `-32603`
because `EthereumRuntimeRPCApi_chain_id` did not yet exist. The playbook
incorrectly dereferenced `.json.result` unconditionally and failed despite
healthy archive progress. Bootstrap now asserts the stable chain, genesis,
finality, runtime name, peer, and progress signals; it classifies and persists
the optional historical EVM response. Exact runtime/transaction version, EVM
chain ID, and log interface remain mandatory convergence gates. The corrected
deployment completed 82 tasks with zero failures.

That passing deployment still did not prove the public P2P path. Snow had the
Docker proxy listening on `0.0.0.0:30333`, UFW inactive, and conntrack at only
858 of 1,048,576 entries, but an independent edge could not connect to
`173.25.160.143:30333`. Prometheus simultaneously reported
`substrate_sub_libp2p_incoming_connections_total 0`; peer sessions were all
outbound and periodically fell to zero. This is the upstream router/NAT
signature. The required repair is WAN TCP/30333 forwarded to snow's current
LAN `192.168.1.161:30333`, with that DHCP address reserved, followed by the
independent-host handshake and rising inbound-connection counter.

Gateway recovery and archive bootstrap deployment are complete after deploying
`run-subtensor.sh`: the pinned image/chain identity, temporary peers, advancing
heads, `/healthz`, and JSON-RPC on overlay 9944 are proven. P2P remains degraded
until the upstream TCP/30333 forward is installed and externally verified;
chain cutover additionally waits for preflight `ready=true` and runtime 447.
Reboot snow once as a deployment gate: nginx must wait until
`172.28.208.185` exists or restart on the transient bind failure.
`network-online.target` alone does not guarantee that the later OpenVPN address
is present.

## 18. Edge IPv6 ingress — EDGEIPV61

### 18.1 Exact-address HTTPS and upstream identity

Probe: `edge-ipv6`

Take public edge targets only from the first (active) `services.yml` version.
Ignore historical versions and transparent proxy interfaces. For every enabled
edge interface with an IPv6 address, pin `api-v6.<domain>` HTTP/1.1 HTTPS to that
exact address so DNS health selection cannot hide a failed interface. Healthy
means HTTP 200; recovery requires three consecutive pinned 200 responses.

Compare every target with the exact global IPv6 address on the configured live
interface and the matching `warp-main-lb-<interface>.service`. Classify a
missing active address as identity drift: reconcile active Vault, persistent
host networking, DNS, and the upstream router permit destination before
changing routes or containers. Disabled hosts in `monitor.yml` are deliberately
excluded; this is how an operator-declared offline edge such as edge-5 stays out
of both the health denominator and remote diagnostics.

A curl exit 7 in less than one second is the immediate-reset signature. Inspect
DNAT rules in order and compare every pool target with live listening sockets.
During a duplicate-to-single rolling transition, an old first rule can point at
a listener that closed after the overlap scan while shadowing a later live
rule. Remove only the proven dead target and deploy Warp's final
socket-authoritative reconciliation on that transition.

A timeout is different. If the host owns the exact address, serves HTTP 200 when
the same SNI request is pinned locally to it, and a request bound to that source
address proves IPv6 egress, local identity, DNAT, TLS, service readiness, and the
return path are intact. If the external pinned SYN also does not increment the
host's exact DNAT rule, the failure is upstream default-drop/ACL. Compare the
router permit destination with active `services.yml`, preserving the existing
ports, actions, ICMPv6 permit, and default drop. On 2026-08-30 edge-3 owned active
addresses ending `e380` and `e381`, but its WANv6 permit rules still named
historical destinations ending `e382` and `e383`; that identity mismatch allowed
ICMPv6 while silently dropping new TCP/80 and TCP/443 ingress.

For any other timeout, capture the pinned SYN, check NDP, policy routing, and
exact DNAT counters, then change only the first layer where packets disappear.
A connected request with a non-200 response is instead a TLS/SNI, LB generation,
or application-readiness fault. Verification for every repair is three pinned
HTTP/1.1 IPv6 200 responses per configured address plus advancing counters at
the repaired layer.
