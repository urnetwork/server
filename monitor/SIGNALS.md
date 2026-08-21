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
  future release_time = pre-lease-fix binary or killed worker. Current workers
  use a five-minute rolling lease, so a killed worker's claim self-releases
  within five minutes of its final heartbeat. A direct-postgres session lock
  still prevents duplicate execution if only the heartbeat is starved and the
  original worker remains alive. claim age > 2× the task's historical duration
  (finished_task history) = investigate.

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

### 2.9 Provider-selection population — the fresh-but-empty cache canary
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
| `Proxy protocol header must be UDP` (legacy log) or `urnetwork_connect_pp_dropped_packets_total{reason="transport_family"}` | The UDP backend received a PROXY header whose address family is not UDP, observed when legacy `proxy_protocol on`/PPv1 traffic overlaps PPv2. Pre-hardening Connect returned this error to quic-go and could kill the shared listener; current Connect drops and counts only that datagram. | Inspect the ACTUAL LB generations and require `proxy_protocol v2`. Page if the PP-drop rate is sustained or `h3_listener_up` falls; a legacy error followed by a missing socket means a pre-fix image is still serving. See §16.2/§16.5. |
| `proxy protocol header required but not found` (legacy log) or `urnetwork_connect_pp_dropped_packets_total{reason="missing_header"}` | A previously unseen LB source tuple sent a headerless datagram: a bypassing/old LB path or broken UDP pseudo-session/header behavior. Current Connect drops it inside `PpPacketConn`; it does not escape as a listener-fatal `ReadFrom` error. | Correlate the exact Connect/LB rollout boundary and remove the incompatible sender. Verify listener gauges and sockets instead of inferring death from one rejected datagram. Do not diagnose JWT/auth or SNAT first. See §16.2/§16.5. |
| `[c]h3 listener <transport>/<port> failed stage=<stage> ... restart in ...` | A supervised QUIC listener exited or failed bind/transform/ListenEarly/accept. `/status` is 503 and `h3_listener_up=0` until the retry succeeds. | Use `stage`, `h3_listener_failures_total`, and `h3_listener_restarts_total`; fix persistent bind/allocation or transform errors. Warp must not activate an LB while any required listener is down. |
| `recv()` / `sendmsg() failed (111: Connection refused) while proxying ... upstream` on an nginx UDP stream | Host DNAT selected a connect allocation with no UDP listener. QUIC Initials reached nginx, but the kernel returned ICMP port-unreachable before connect could answer. | Map the logged logical upstream through the current host DNAT to its connect block/allocation; verify `WARP_PORTS`, `ss -lunp`, that block's last H3 listener-exit line, and `UdpNoPorts`. See §16.5. |
| `[c]h3 handshake no response mode=... sent_packets=N pto=M ...` | The client emitted QUIC packets but received zero packets before handshake failure. This is the Initial-blackhole signal that `PacketLost` misses; PTOs are counted separately. | Pin the resolved public tuple, then split public DNAT/conntrack, LB backend socket, and return path with §16.5/§16.6. Do not interpret `q_lost=0` as health. |
| `[c]h3 connect err = ... context deadline exceeded` | No valid QUIC response reached the client. This is below auth but otherwise ambiguous: direct UDP/443, DNS-envelope creation, public UDP/53 delivery, LB/PP/DNS decode, and the return tuple can all cause it. | Record the mode and resolved IP. For direct H3, split public ingress from backend-listener loss with §16.5. For `h3dns`/`h3dnspump`, prove raw envelopes were sent and compare the exact port-53 DNAT delta: zero is upstream delivery, no rule is LB activation, and a rise without a 4053 (or migration 8053) accept is LB/PP/decode (§16.6). |
| `panic: Missing host port for service port <port>` (LB startup) | The image's nginx config contains a logical listener absent from runtime `WARP_PORTS`; usually services.yml/image generation is newer than the systemd unit's baked `--portblocks`/`--forwardports`. The desired LB version may look deployed while the new container is Exited(2) and the old LB keeps serving. | Compare `systemctl cat`, container `WARP_PORTS`, and baked nginx listeners; regenerate and deploy units (§11.8), then require the new LB to stay `Up` before evaluating its behavior. |
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
| selection-empty | pg+redis | 2.9 eligible Public providers vs decoded normal score-cache count | eligible > 1,000 AND exported == 0 for 2 probes | ForceMinimum count; egress-test config; health/location row counts; last score completion |
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
