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
Updated 2026-08-31 after live web logs showed repeated exact-path ENOENTs for
Android and Apple association metadata. The files existed in the tracked Astro
public tree and passed the SEO gate in `dist`, but `mv dist/*` silently omitted
the root `.well-known` directory while staging the deployable tree. Section 19
adds exact-edge semantic probes and records the dotfile-safe staging fix.
Updated again after the standing log-tail handover exposed impossible negative
Loki active-tail gauges. Section 11.19 adds the `loki-tailers` direct-child
probe, separates invalid accounting from actual live-tail loss, and records
Loki 3.7.3's non-idempotent double-close root cause.
Updated 2026-09-01 after the public stats dashboard exposed matching multi-hour
holes in live throughput, connected devices, provider counts, and network
counts. Section 11.20 adds the `mimir-continuity` raw-range control and records
the lost ephemeral TSDB head root cause plus the clean-shutdown flush fix.
The same production pass traced modified, incompletely attributable service
executables back to the exact Warpctl copies that built and launched them.
Section 8.13 adds the `release-builder` exact-binary identity probe.
Operator policy subsequently confirmed that local repository checkouts,
including deliberate uncommitted changes, are the deployment source; Warp
commit `8797d48` intentionally removed those release gates. Section 8.13 now
retains exact executable identity and observation-loss coverage without treating
`modified=true` or absent withdrawn guard strings as a fault.
The direct runtime follow-up adds §11.21 (`mimir-shutdown`): all six enabled
Grafana blocks still rendered shutdown flushing false, proving the source fix
had not reached production even though it existed in Warp.
The database follow-up adds §1.3a (`pg-capacity`) and a typed
`pg-client-capacity` log class. It separates PostgreSQL slot exhaustion from
generic panic amplification and records the validated legacy-reindex, WAL
stall, client-deadline, and PgBouncer replacement chain.

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
cadences collide after startup.

The 2026-08-31 recurrence proved that the top-level bound alone is not enough.
Probe-local batteries can fan out to four or eight hosts, so four admitted
signals briefly opened at least ten SSH handshakes to the same database host.
Edge-2's stock OpenSSH `MaxStartups` began throttling at connection `#10`,
dropped 16 connections over 2m01s, and produced four unrelated
`monitor/visibility` alerts even though PostgreSQL and the next standalone
canary read were healthy. The sshd journal attributed every dropped connection
to the monitor host. Every real SSH execution now also passes through one
shared two-command semaphore keyed by destination address. The semaphore lives
in the monitor runtime, not a fresh probe runner, so internal fan-out and
cadence collisions share the same budget while different hosts remain
parallel. The deterministic transport test fills the budget through separate
probe environments, proves the next same-host command cannot invoke SSH,
proves its canceled wait releases no slot, and proves another host still runs.

The first capped production startup exposed the independent host boundary.
The monitor admitted only three sessions in the trip second, but sshd was
already holding seven unauthenticated sessions on its public-facing address,
some 112 seconds old, and reported `7 of 10-100 startups`. Two public source
addresses owned those long `[accepted]`/`[net]` children. The resulting
one-second throttle dropped one overlay monitor connection. Source-side
containment therefore uses two commands per host; the operational root fix is
to bound pre-auth lifetime and per-source occupancy while reserving a larger
global admission pool in the shared Ansible ssh hardening. Restrict public SSH
at the network boundary where operational access permits. Do not merely raise
`MaxStartups`, and do not attribute this signature to PostgreSQL.
Client-side key-exchange resets are emitted as structured class
`ssh-admission-reset`, grouped at the SSH host rather than mislabeled as the
database/service a command never reached. The alert treats MaxStartups as a
journal-confirmed discriminator, not an inference from a generic TCP reset.

The exact two-command monitor remained contained through a longer live control
on 2026-08-31. From 14:40Z through 15:16Z, edge-2's readable ssh journal held
1,985 lines and 680 accepted-key events but zero `MaxStartups`, throttle, or
drop events. This verifies the source limiter under repeated full cadences; it
does not prove the host hardening is deployed. The login could not run
`sshd -T` without privilege, and its readable ssh config contained none of the
new `LoginGraceTime`, `PerSourceMaxStartups`, or `MaxStartups` directives.
Keep the shared xops hardening deployment as a separate verification gate.

Likewise, a probe interrupted by an intentional monitor shutdown is a
lifecycle event, not loss of production visibility, and must not emit a
`monitor/visibility` alert. Cadence mode enforces each alert's consecutive-tick
`Sustain` value and resets that identity after a healthy tick; `--once`
deliberately reports current violations immediately for diagnosis.

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
completes in 3–15s. It is an end-to-end canary: if Redis is sick anywhere on
the write path, it errors within a minute, but a zero-completion window is not
proof that Redis is the failed layer. PostgreSQL admission exhaustion or a
database-wide wait pileup can prevent the scheduler/worker from reaching Redis
at all, and a taskworker lifecycle gap can do the same. On zero completions,
preserve bounded per-minute completion history, pending claim/lease state,
direct PostgreSQL capacity, active reindex progress, and Redis cluster state;
attribute the failed layer before mutating it.
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
- Never emit a durable task identifier from a stored error, PostgreSQL row, or
  taskworker heartbeat. It can identify a durable payment/task row. A probe may
  retain the exact identifier in memory only long enough to correlate lifecycle
  records; structured alerts emit `attempt_correlated=true`, task family,
  duration, lifecycle timestamps, executor identity, source, and the bounded
  error class/target instead. A mutation that truly needs the identifier must
  obtain it through the protected operator lookup and must not copy it into an
  alert, transcript, test failure, commit, or agent response.
- The 2026-09-03 `UpdateReliabilities` alert exposed this gap. Task-canary,
  close-duration, selection-freshness, netescrow, reboot-collision,
  stuck-leases, worker-memory, and worker-churn now keep identifiers inside
  correlation only. Their deterministic failure cases require the rendered
  Markdown to omit the synthetic identifiers. Historical opaque identifiers in
  this catalog are represented as `<redacted-id>`.
- The 2026-09-03 `BackfillClock` failure is a distinct exact-deadline
  workload bug, not evidence that its ten-minute `MaxTime` is too small. The
  task reached 600.00s, rescheduled, and repeated. A direct read-only snapshot
  found one pending row and one live lease; the five matching PostgreSQL
  backends were one query leader plus its four parallel workers, so RunOnce
  was working. The old candidate scanned an estimated 2.18 billion
  `contract_close` rows, filtered the destination half, probed
  `transfer_contract` by primary key, and then ran the same full retained-
  history aggregate a second time. Its plan cost was about 60 million;
  `pg_stat_statements` had recorded a 635s maximum. Meanwhile the existing
  `transfer-rollup:v1` feed contained exactly one provenance-marked row for
  every complete UTC day from block 9 through 2026-09-01, and its bounded
  three-day rollup tasks had completed in at most 462s. Current source sums
  only the contiguous, unique completed-day prefix and scans the raw tail once
  from the first missing or duplicate day, marked `clock_unrolled_tail` for
  direct attribution. Redis's monotonic max preserves successful live
  increments that race the snapshot. Deploy that Taskworker path, retain the
  600s containment, and require the same pending row to complete; do not add a
  broad billion-row index, restart PostgreSQL/Redis, or increase the deadline.
- The 2026-09-01 recurrence supplied the non-Redis discriminator. Completions
  were normal through 19:16Z, fell to one at 19:17Z, and were zero for four
  minutes. At 19:17:47Z, while the legacy maintenance worker was rebuilding
  `contract_close`, direct PostgreSQL showed 1,023 client backends against a
  1,021 ordinary-role ceiling, 995 active, and 803 active loopback sessions
  waiting on transaction IDs, `BufferContent`, `WALInsert`, and `WALWrite`.
  In 19:18Z PostgreSQL completed 852 logged statements: 835 exceeded ten
  seconds, 602 exceeded thirty seconds, 163 clients were already gone, and
  164 statements were canceled. The canary recovered with five completions in
  19:22Z and six in 19:23Z without a Redis repair after the large rebuild
  attempt ended. This proves an end-to-end scheduler/database gap, not Redis
  failure. The root fix remains the large/high-churn maintenance exclusion and
  lease-owner repair in current Taskworker; do not restart Redis or manually
  schedule a duplicate canary to mask it.
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
- GOTCHA — wallet insufficiency is an operational liquidity boundary, but its
  retry shape can still contain a software amplification. Consecutive task
  errors have a one-hour nominal cap. The old scheduler added only 0–2 seconds
  of jitter at that cap, so rows created by one outage retained their narrow
  wave forever. On 2026-08-31, 824 failing `AdvancePayment` rows would have
  averaged 13.7 retries/minute if dispersed; instead a live wave reached
  79/minute (69 rows also had a fresh claim heartbeat), and the processor
  returned one 429 amid 817 wallet-insufficient rows. Ordinary 400
  wallet-insufficient responses continued immediately afterward, separating
  the bounded burst from a fleet-wide processor outage. The pre-fix cohort
  recurred later the same day: wallet-insufficient output reached 86/minute at
  15:40Z, Circle returned a second 429 at 15:40:52Z, and the next durable
  snapshot changed from 817 wallet-insufficient plus one rate-limit row to 816
  plus two while the family total remained 824. The wave continued: five
  distinct wallet-insufficient attempts landed at 15:46:23Z before another
  request received 429 in that same second; four attempts at 15:46:33Z then
  preceded a fourth 429. The durable snapshot shifted to 815 plus three before
  the fourth result was ingested and stayed there afterward: the cause
  breakdown is each row's current last error, not a cumulative event counter,
  so one row changing class can hide another 429. The immediate
  `payment-processor-rate-limit` log class is the event signal; use the durable
  breakdown only for the current retry population. Both taskworker groups were
  still entirely on `2026.8.31-outerwerld+1033655820`; its embedded source
  revision `1d8f01e5` predates the proportional-jitter fix. This exact temporal
  and row-class conversion is evidence of cohort amplification, not a claim
  that every 429 has that cause. The scheduler now jitters saturated retries
  across 30–90 minutes with the same one-hour mean; its deterministic 824-row
  synthetic cohort covers all 60 minute buckets with at most 14 rows in one
  bucket. Deploy taskworker commit `70b0d269` or later and verify the narrow
  hourly wave disappears and no new rate-limit row is created during the next
  recurrence. This containment cannot create liquidity:
  finance/ops must still fund the exact network/token wallet or pause payouts.
  Never accelerate retries, delete pending rows, or rotate idempotency keys to
  silence this class.
  One logical Circle rejection normally produces two diagnostic lines: the
  provider-boundary client record and the task evaluator record. The standing
  alert now reports both raw `diagnostic_lines` and
  `processor_rate_limit_events`, counting only exact-replay-deduplicated task
  evaluator records for the latter. A lone provider line still alerts
  fail-safe, but it renders a zero canonical count instead of being silently
  divided by two. This matters during the live stale-build drain, where a
  4/min line rate represented two logical 429 events and the durable current
  row class advanced independently.
- The standing tail derives a separate `payout-retry-microburst` finding from
  that wallet class. It counts only the canonical `[task.go:<line>]` evaluator
  record (one per logical attempt), groups the embedded source timestamp by
  second, and de-duplicates exact records across the current and immediately
  previous drain window. Circle-client plus evaluator diagnostic pairs
  therefore count as one attempt, and a `warpctl logs --since=1s` reconnect at
  a minute boundary cannot manufacture a second burst. WARN at a peak of four
  attempts in one source second: four was the smallest live cohort immediately
  followed by a 429 on 2026-08-31. This is an empirical incident discriminator,
  not a claim about Circle's account-specific quota. Keep its remediation
  class distinct:
  `payout-retry-microburst` is a **software deployment** alert closed by
  converging stale taskworkers to `70b0d269` or later and observing a complete
  90-minute drain window below four attempts/second; `payout-wallet-insufficient`
  is an **operations/finance** alert that no software release can close. A
  remaining 429 after every block is current requires measurement across all
  Circle clients against the account's authoritative quota, not another blind
  redeploy.

  The standing implementation then caught the next recurrence live at
  16:30–16:38Z. A bounded independent pull found 164 canonical
  wallet-insufficient task attempts across all eight active taskworker
  host/generation allocations. The peak was five attempts in
  `2026-08-31T16:31:47Z`; the one logical 429 occurred in that same second and
  appeared as the expected Circle-client plus task-evaluator diagnostic pair.
  The standing windows independently alerted at four, five, four, and four
  attempts/second. Both taskworker generations were still uniformly on
  `2026.8.31-outerwerld+1033655820`, so this is direct post-probe validation of
  the old-jitter diagnosis, not inferred recurrence from durable row counts.
  The live alert also found an evidence bug: its stored sample came from the
  first canonical attempt in the minute (16:31:33), not the actual five-event
  peak second (16:31:47). The tailer now updates both `peak_source_second` and
  its redacted sample whenever a later second exceeds the prior peak; a
  deterministic sparse-first/peak-later regression prevents the alert from
  presenting unrelated evidence again.

  The next old-build recurrence exposed a second timestamp-evidence defect.
  As the parked subset fell from 801 to 691, the independent canonical audit
  found 39 attempts in three minutes and a four-attempt peak at
  `2026-08-31T18:31:16Z`; the standing monitor independently opened
  `payout-retry-microburst` with 28 canonical attempts from 56 diagnostic
  lines. Its peak field rendered `2026-08-31T13:31:16` with no offset because
  the parser stripped `-05:00` before storing the grouping key. Interpreting
  that wall clock as UTC would shift correlation with Circle, PostgreSQL, and
  kernel evidence by five hours. Burst timestamps now parse the complete
  RFC3339 offset, normalize the instant to a whole UTC second, and render the
  trailing `Z`; the redacted sample retains the original envelope. A
  deterministic non-UTC-offset regression requires
  `13:31:16-05:00` to render and group as `18:31:16Z`.

  Post-roll validation on the clean `5c91a3c4` monitor captured the same live
  wave without an inference step. Three consecutive windows rendered peaks of
  five attempts at `18:36:58Z`, four at `18:38:48Z`, and four at
  `18:40:08Z`; each peak retained its original `-05:00` sample envelope. No
  `payment-processor-rate-limit` event appeared through that last window. This
  proves the UTC evidence fix is active while independently reaffirming that
  the still-deployed pre-`70b0d269` taskworker preserves narrow retry cohorts.

  The same recurrence then crossed the processor boundary. Four canonical
  wallet failures landed at `18:45:38Z`; at `18:45:39Z`, two more wallet
  failures and two canonical 429s landed together. Five wallet failures at
  `18:46:41Z` were followed at `18:46:42Z` by two wallet failures and a third
  canonical 429. The standing monitor independently reported the first pair as
  two logical events from four diagnostic lines, then the third as one event
  from two lines. At `18:46:57Z`, the durable family snapshot changed from
  `wallet-insufficient=817,processor-invalid-destination=6,processor-rate-limit=1`
  to `814,6,4` while the total stayed 824. Exactly three rows changing class
  matches the three canonical events and rules out a growing payment backlog
  or a duplicate-log illusion. A fourth canonical 429 followed at
  `18:48:04.608126Z`, and a fifth followed five wallet-insufficient attempts
  at `18:53:30Z` before landing at `18:53:30.571865Z`. The durable breakdown
  remained `814,6,4`: it is the current last error of each row, not a
  cumulative event counter, so a row returning to wallet-insufficient can
  conceal a different row's new 429. The stale cohort caused real provider
  throttling even though several earlier four/five-per-second peaks did not;
  deploy `70b0d269` rather than treating the empirical burst threshold as a
  deterministic provider quota.

  The unchanged deployment produced another independently bounded recurrence
  from `20:29:56Z` through `20:35:31Z`. Across the eight active
  host/generation allocations, 143 exact-replay-deduplicated task-evaluator
  records represented wallet-insufficient attempts. Seven source seconds held
  at least four attempts; the peak was five at `20:31:57Z`, and one canonical
  processor 429 landed in that same second. The standing monitor independently
  rendered that exact five-attempt UTC peak, one logical rate-limit event from
  its two diagnostic lines, and the surrounding wallet retry rate. The durable
  family stayed at 824 rows while its latest-error breakdown became 810
  wallet-insufficient, eight processor-rate-limit, and six invalid-destination
  rows. This is further direct evidence that
  `2026.8.31-outerwerld+1033655820` still preserves the pre-fix retry wave; it
  does not justify a different retry algorithm or treating the 429 as a broad
  processor outage. Deploy a Taskworker artifact containing `70b0d269`, then
  use the complete 90-minute observation gate already specified above.

  The UTC-rollover recurrence supplied the direct post-provenance control. At
  `00:31:39Z`, the standing monitor reported 28 exact-replay-deduplicated task
  attempts from 56 diagnostic lines. Six wallet-insufficient attempts landed
  at `00:30:43Z`, and one canonical processor 429—two diagnostic lines—landed
  in that exact second. The independently bounded query produced the same
  source second and logical event count. Because the deployed image provenance
  is `1d8f01e5`, this joins the runtime burst to the known pre-`70b0d269`
  scheduler rather than inferring source age from its version string. It also
  validates the monitor's peak-sample and canonical-event accounting through a
  fresh live recurrence. Deploy the existing jitter fix; do not add a second
  retry algorithm or confuse dispersion with the separate wallet-liquidity
  operation.

  The unchanged artifact then produced a broader causal control through
  `00:50Z`. A bounded three-hour query found 15 canonical 429 evaluator events
  across 12 exact source seconds and 12 task rows. Every one of those seconds
  also held at least five canonical wallet-insufficient attempts; one held
  six. In the latest event, five wallet results completed from
  `00:49:53.240866Z` through `00:49:53.327394Z`, followed by the 429 evaluator
  result at `00:49:53.339836Z`. The durable family remained exactly 824 rows
  while its latest-error breakdown moved to 815 wallet-insufficient, six
  invalid-destination, and three rate-limit rows. This proves another provider
  throttle was amplification of the existing backlog rather than backlog
  growth, while also proving the durable class count is not a cumulative 429
  counter. The tailer now reports a privacy-safe source-second join directly:
  canonical rate-limit seconds, how many crossed the four-attempt cohort
  threshold, total/peak coincident wallet attempts, and the canonical logical
  event count. It retains burst-second counts across the bounded drain overlap
  so late reconciliation cannot sever the correlation; deterministic tests
  require an exact-second cross-drain join, reject the adjacent second, expire
  old state, and keep diagnostic replay at zero logical events. This stronger
  evidence still does not establish Circle's account quota or justify a shared
  limiter before `70b0d269` has completed its 90-minute deployment control.

  At `03:22Z` on 2026-09-01, the standing invalid-destination alert exposed a
  monitor provenance bug: every responding block sampled
  `2026.8.31+1034210530`, and the investigation initially treated that mutable
  WARP/config generation as proof that source `a52392db` was running. Exact
  artifact validation later disproved that inference. Six directly observable
  blocks on enabled edges 0, 1, and 3 executed image content identity
  `sha256:042255119828a004024a4dc5e57d97373a8bf399aca6074ca98804dec2b3156a`;
  edge-4's two blocks had no retained startup digest and unprivileged Docker
  inspection was unavailable, so they remain provenance-unknown rather than
  being counted as proven. The matching amd64 manifest's extracted executable
  reported base revision `078d6c11` with `vcs.modified=true`, while its BuildKit
  attestation described Docker context `a52392db`. The prebuilt binary was made
  from a dirty earlier tree and the Docker context was published after HEAD
  advanced. This is a release-provenance race, not evidence that config version
  maps to source.

  Direct binary discriminators nevertheless prove that the six-block artifact
  contains the proportional-jitter fix: it exports the
  `task.errorRescheduleDelay` symbol and the post-`70b0d269` direct `run_at=$3`
  / `$5` error-count SQL rather than the old SQL `power()` backoff. The one
  four-attempt source second at `06:14:26Z` produced no canonical processor 429
  and the microburst finding cleared on the next complete cadences. That is not
  a reason to redeploy the same Taskworker; retain the full observation gate
  and investigate a repeated current-artifact wave or a coincident 429. The
  taxonomy now makes no current-version claim and delegates software action to
  the independently triggered `payout-retry-microburst` finding. Its
  deterministic test rejects historical version/source assertions and a direct
  jitter-deploy instruction. The operational wallet correction remains
  unchanged.

  The required post-jitter control then failed at `07:12:48Z` on 2026-09-01.
  Five canonical wallet-insufficient transfer results completed in that source
  second and a sixth transfer request received Circle 429; three results came
  from edge-0/g1, one from edge-1/g1, and one from edge-4/g1, while the 429
  came from edge-1/g2. Four of the five successful rejections therefore came
  from blocks whose exact executable had already been proven to contain the
  proportional-jitter implementation; edge-4's unknown artifact cannot explain
  away the recurrence. At `07:12:49Z`, three more edge-1/g2 wallet rejections
  and another canonical 429 followed. This rules out stale retry jitter as the
  remaining root cause: independent 30–90-minute random choices lower average
  synchronization but cannot enforce a fleet-wide instantaneous ceiling.
  Circle's current [Wallets API rate-limit
  documentation](https://developers.circle.com/api-reference/wallets/rate-limits)
  specifies five default POST requests/second and a 429 when exceeded, matching
  the observed five completed rejections plus sixth refusal.

  Current-main server commit `14928f69` (the patch-identical replay of former
  commit `eb7e79b6`) is the root fix. Immediately before the developer
  transfer POST, every process contends on one Redis-time rolling sorted set;
  an atomic Lua decision admits at most three submits in any rolling second,
  leaving two requests/second of documented headroom. A unique member makes a
  lost-response command replay idempotent. Redis or context failure is fail
  closed before HTTP, and the durable payment idempotency key remains unchanged.
  A deterministic synthetic test launches eight concurrent fleet callers at
  one timestamp, requires exactly three admissions/five deferrals, replays an
  admitted member without consuming a slot, and reopens capacity only after the
  rolling second expires. §2.14 owns deployment and runtime verification; the
  existing source-second log join remains the provider-outcome control.
  Follow-up current-main commit `66525afc` converts the server Redis wrapper's
  connection panic path into that same measured fail-closed error, so it is the
  minimum deployable source for complete §2.14 telemetry.

  The first remediation added a fail-closed Warp release gate, but operator
  policy later established that intentional local checkout state must remain
  deployable; current Warp commit `8797d48` removes that gate. Generic service
  telemetry still exports the Go base revision, Boolean modified bit, and
  Warp-injected immutable digest so future probes can join the running process
  without extracting images. A modified bit is descriptive rather than a
  failure; preserve the participating local diff when exact replay matters.
  None of this requires redeploying the current Taskworker solely to re-prove
  the already-present jitter behavior.

  The first `03:50Z` run of the replacement watcher found the same defect in
  the independent `task-canaries` mixed-family guidance: its current
  `AdvancePayment` alert still asserted that the old version/source was the
  production Taskworker. The durable task rows do not contain runtime artifact
  ancestry, so this PostgreSQL probe cannot make that claim. It now requires an
  explicit per-block ancestry check for typed-reset `b8af229f`, deploys only to
  older blocks, and treats persistence on already-current blocks as invalid
  configured-wallet selection. Synthetic mixed-family tests reject the old
  version, source, and generic `production taskworker` claim. Historical
  incident paragraphs retain their dated artifacts as evidence, not runtime
  instructions.
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
  maintenance task. Then use its identifier internally to correlate the
  authoritative `eval active` elapsed time; emit only the task family, duration,
  executor, source, and `heartbeat_attempt_correlated=true`. `run_at` is only
  the due time, and a task that waited in the queue but began less than the
  guard ago is not long-running.
  The 2026-08-30 reaper recurrence supplied the regression values: p50 42s,
  p95 3,552s, due age 6,283s, and matching heartbeat 6,220s. The old p95-only
  rule waited until 7,104s; the median-tail guard alerts at 1,200s.
- GOTCHA — an overdue `UpdateReliabilities` duration does not identify its SQL
  phase. A full anchor, rolling enter, rolling leave, and post-leave cleanup
  have different causes and closure gates. When this family crosses the
  duration guard, run one bounded read-only diagnostic over
  `pg_stat_activity`, `client_reliability_rollup`,
  `client_reliability_running_window`, `pg_catalog`, and
  `pg_stat_all_indexes`. Report the recognized phase, SQL elapsed time and
  wait class, active/transaction-blocked statement counts, current-format
  marker counts, classification-guard state, and the maximum distance from
  the current drained target to `last_recompute_block`, including its lookback
  index, plus old/covering index state and each index family's last-scan age.
  The current drained target is `max_drained_block + 1`, the same `newMax`
  used by the writer. Do not substitute the prior committed window head:
  while a slow checkpoint runs that head is stale and can hide a now-due
  periodic correction. Map `client_addr` internally to the
  configured host and emit only that host name; an address with no unique
  inventory match becomes `unmapped-service-client`, never a raw IP. Never
  emit query text, PID, task identifier, or customer fields. If this
  diagnostic is unavailable or samples between statements, say the cause is
  unknown: do not fall back to the historical repeated-full-anchor claim.
  The 2026-09-03 incident proved why both discriminators are required. The
  deployed worker already contained the four-hour cadence and per-lookback
  checkpoints. Its first overrun was directly observed in `rolling-leave`; all
  four markers were current-format and their committed heads were only 54–55
  blocks past re-anchor. At `04:11:13.779Z` that attempt reached exactly
  7,200 seconds and emitted the connection-cleanup signature; a same-task,
  same-arguments successor began about 14 seconds later. That terminal error
  does not convert the observed rolling predecessor into a full anchor or
  prove that earlier checkpoint transactions rolled back. The successor later
  entered `full-anchor-insert` legitimately: lookback 1000's prior committed
  head was only 54 blocks beyond its last recompute, but the current drained
  target was block 29,806,818, making the actual decision distance 251 blocks;
  the other three lookbacks were eight blocks away, and all four version/token
  markers plus the database guard were current. The correction therefore
  reports target distance and lookback 1000 rather than prescribing the
  already-deployed cadence fix. See §5.7 and §8.10 for the legacy-index and
  reboot-overlap root cause. Server commit `fcb4de54` adds a transaction-local
  two-hour PostgreSQL statement timeout to every checkpoint, matching the
  existing task ceiling so a hard worker loss cannot leave a server-side
  statement unbounded. This is a Taskworker software containment, not a
  replacement for index finalization or a reason to disturb current work.

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
- The 2026-08-31 05:55Z migration watch supplied the young-count
  discriminator. A sustained sample reached 181 idle-in-transaction clients,
  but the oldest transaction and every grouped shape had only 0–1 seconds of
  continuous idle time. The trip-time battery was dominated by brief
  repeatable-read `BEGIN`, close-contract selection, and provide-key update
  shapes; the next direct `pg-state` read was healthy. Migration coherence,
  active-query, wait-event, and PostgreSQL-state companions stayed clean.
  This was bounded application concurrency while the close backlog drained,
  not a migration lock, Redis-stalled transaction, or xmin-pinning leak. Keep
  the count warning for capacity attribution, but require the explicit
  continuous-idle age before terminating any session or assigning a leak.

### 1.3a PostgreSQL client-slot capacity and rejected logins
Probe: `pg-capacity`

Read this through direct port 5432, never through the nginx/PgBouncer frontend.
The probe derives the ordinary-role ceiling from the deployed server settings,
counts every `client backend`, and exports only the ten largest groups by
application, role, client address, state, and wait event. It does not export
query text or customer identifiers.

```sql
SELECT name, setting, unit
FROM pg_settings
WHERE name IN ('max_connections',
               'superuser_reserved_connections',
               'reserved_connections');

SELECT application_name, usename, client_addr, state, count(*)
FROM pg_stat_activity
WHERE backend_type = 'client backend'
GROUP BY application_name, usename, client_addr, state
ORDER BY count(*) DESC
LIMIT 10;
```

- `normal_role_ceiling = max_connections -
  superuser_reserved_connections - reserved_connections`. Every existing
  client backend consumes the admission threshold, including a privileged or
  direct-maintenance owner and the observation session itself.
- HEALTHY: more than 25% normal-role headroom remains.
- WARN: at least 75% of that ceiling is occupied for two consecutive 30-second
  samples. Capacity is not query load: split active, idle, and
  idle-in-transaction owners before assigning a cause.
- PAGE: at least 90% is occupied, 64 or fewer normal-role slots remain, or the
  direct observation itself receives `FATAL: sorry, too many clients already`.
  A direct rejection has no numeric snapshot, but it is affirmative server
  capacity evidence rather than a generic monitor visibility failure.
- PgBouncer `server_login_retry` caches a failed server login. One rejected
  application request can then produce an `Unexpected error` record, a router
  recovery record, and goroutine-shaped JSON. Class `pg-client-capacity` takes
  precedence over generic `panic`; raw line volume is diagnostic amplification,
  not a count of unique rejected PostgreSQL sessions.
- This is distinct from `query_wait_timeout`, where a PgBouncer shard already
  owns all of its server connections and kills a queued client, and from a
  client write timeout on port 6432, where the request may not reach the pool.
- ROOT-CAUSE ORDER: first split active, young idle-in-transaction, idle, and
  starting owners. Correlate active and young transaction cohorts with direct
  wait events and completed statement or `COMMIT` latency in PostgreSQL logs.
  A request that reaches its deadline during a stalled transaction can make
  PgBouncer discard the uncertain server session and open a replacement while
  the old backend unwinds. Compare PgBouncer connection logs, or `SHOW POOLS`
  where administrative access exists, across all active shards and with direct
  maintenance sessions. The current deployment has 32 independent PgBouncer
  processes; a per-process `default_pool_size` is not a fleet-wide cap. Remove
  the upstream database stall, transaction leak, or continuously retaining
  owner before changing that envelope. Do not infer a leak from a later idle
  recovery cohort; do not first raise `max_connections`, restart PostgreSQL or
  PgBouncer, or mass-terminate sessions. `work_mem` is per operation and is
  large on this host, so adding slots without a memory budget can turn an
  admission incident into host-wide memory pressure.
- FIX CLASS: software/configuration when a leaking caller, retry loop, or
  aggregate pool ceiling owns the slots; operational when direct maintenance
  overlaps serving demand. More hardware may be part of a deliberately larger
  database concurrency budget, but it does not replace owner attribution and
  bounded pools.
- VERIFY: for ten minutes through the workload that triggered the alert,
  direct 5432 stays observable, ordinary-role headroom remains above 25%,
  completed `COMMIT` latency and WAL waits return to their ordinary band, and
  neither `pg-client-capacity` nor `query_wait_timeout` recurs.

The 2026-09-01 production control established the current causal chain rather
than merely the admission symptom. During the repeatedly lease-recovered
legacy `DbMaintenance` reindex, PostgreSQL completed large groups of unrelated
commands and `COMMIT`s together after 60–66 seconds. Direct samples showed
hundreds of active localhost PgBouncer-owned backends waiting in
`WALSync`, `WALInsert`, `WALWrite`, and `BufferContent`; PostgreSQL then logged
client loss as the application deadlines expired. PgBouncer shard 1 alone
opened 537 replacement server connections during minute `17:25Z`. A direct
snapshot during the stall reached 824 of the 1,021 ordinary-role slots (80.7%,
746 active); after the stall it briefly changed to 517 young
idle-in-transaction sessions and then drained. PostgreSQL never restarted and
checkpoint sync itself remained below 0.4 seconds. This proves WAL/storage
stall followed by deadline-driven replacement overlap, not an idle-retention
leak or a PostgreSQL restart. For this recurrence, wait until
`pg_stat_progress_create_index` is empty and deploy a clean Taskworker with
current-main commits `908a8b2c` and `d8392c83`; pool tuning is not the root
fix. Those commits have the same stable patch IDs as the former `7676014f`
and `abfd976b` hashes from before main was rewritten.

### 1.3b PgBouncer idle-backend retention
Probe: `pool-retention`

This is the reserve-shape companion to §1.3a. Read it through direct 5432 and
count loopback client backends by state; do not assume that every loopback
backend belongs to PgBouncer until a privileged socket census or `SHOW POOLS`
proves the owner.

```sql
SELECT count(*) FILTER (WHERE client_addr <<= inet '127.0.0.0/8'
                         AND state = 'idle') AS loopback_idle,
       count(*) FILTER (WHERE client_addr <<= inet '127.0.0.0/8'
                         AND state = 'idle'
                         AND state_change <= now() - interval '600 seconds')
         AS loopback_idle_aged,
       max(now() - state_change) FILTER (
         WHERE client_addr <<= inet '127.0.0.0/8' AND state = 'idle'
       ) AS oldest_loopback_idle
FROM pg_stat_activity
WHERE backend_type = 'client backend';
```

- HEALTHY: idle loopback backends consume less than 50% of the ordinary-role
  ceiling, excess pool connections drain toward their configured warm minimum
  after 600 idle seconds, and §1.3a retains more than 25% total headroom.
- WARN: idle loopback backends consume at least 50% of the ordinary-role
  ceiling for ten consecutive 30-second samples. Report local idle, active,
  idle-in-transaction, continuously idle for at least 600 seconds, and the
  oldest continuous idle age. A zero aged count distinguishes a young
  post-peak or recurring-demand cohort from proved long-idle retention; it
  does not restore the admission reserve.
- MECHANISM: `default_pool_size` is per PgBouncer process and user/database
  pair, not a database-wide cap. Every idle server connection still owns a
  PostgreSQL slot. A zero `server_idle_timeout` can retain a peak until
  `server_lifetime`; a nonzero timeout should contract each continuously
  unused server connection toward the `min_pool_size` warm floor. A young
  cohort that has not crossed the configured interval proves a temporary
  reserve squeeze, not disabled draining. A cohort that survives beyond the
  interval requires both owner attribution and the selected live settings
  before classifying the drain as disabled, ineffective, or mismatched.
- ROOT-CAUSE ORDER: first read the selected settings from every live shard and
  take one root socket census. If the timeout is zero, apply the isolated
  Xops commit `31ae1e7` only after protected database maintenance is empty and
  with explicit operational authorization. The supported consolidated runner
  is `xops/main/ansible/run-dbs.sh --pgbouncer-only`; there is no separate
  `run-pgbouncer.sh`. It sets the documented 600-second PgBouncer default,
  reloads only changed pooler units, and fails if any PID changes across
  reload. If 600 is already effective and the cohort is younger than 600
  seconds, make no change and observe one complete timeout interval. If the
  reserve remains consumed after that interval, use `SHOW POOLS`/`SHOW STATS`
  and wait metrics to decide whether the per-shard pool size is justified.
  Do not restart PostgreSQL/PgBouncer, terminate sessions, or raise
  `max_connections` to silence this signal.
- FIX CLASS: software/configuration for disabled draining or an oversized
  aggregate pool envelope; operational for sequencing the reload outside
  protected maintenance. If observed demand genuinely requires the retained
  concurrency, adding database memory/CPU hardware may be necessary before
  raising the total connection budget. Software cannot manufacture that host
  capacity, and hardware does not replace bounded pools or owner attribution.
- VERIFY: every live shard file reports `server_idle_timeout=600`; every
  PgBouncer PID is unchanged across the isolated reload; after more than 600
  seconds outside a demand peak, excess connections contract toward the warm
  floor; §1.3a headroom stays above 25% for ten minutes; and neither
  `query_wait_timeout` nor a rejected server login recurs.

The 2026-09-01 production control supplied exact causal evidence. Edge-2 had
32 running PgBouncer units. All 32 live files selected
`default_pool_size=20`, `min_pool_size=8`, `reserve_pool_size=4`,
`server_lifetime=3600`, and `server_idle_timeout=0`. One privileged socket
census found 16–20 established PostgreSQL connections per shard and 608 total;
the direct PostgreSQL view independently showed roughly 600 idle loopback
backends while only single-digit queries were active. The disabled timeout
therefore permits a 640-connection retained ceiling instead of contraction
toward the 256-connection warm floor. This is a real post-incident reserve
defect, but it did not initiate the earlier capacity burst: §1.3a proved that
legacy reindex WAL/storage stalls and deadline-driven replacement overlap did.
Both root fixes are required at their own boundaries.

The 2026-09-02 edge-3 scheduled reboot supplied the post-fix timeout control.
Immediately before the reboot PostgreSQL had 285 idle loopback backends. When
all service blocks returned, the 32 PgBouncer shards refilled together and the
count reached 636 at `01:26:48Z`; the oldest member was only 25 seconds idle and
none had crossed 600 seconds. The isolated
`run-dbs.sh --pgbouncer-only --check` then proved all 32 live files and process
identities current with `changed=0`. At `01:37:22Z`, after one configured
timeout interval, idle loopback backends had fallen to 481 (47.1% of the
ordinary-role ceiling) without any reload, restart, or session termination.
That clears the reserve threshold and validates `server_idle_timeout=600` as
the active drain mechanism. Continue the ten-minute headroom control and let
the cohort contract; do not redeploy merely because the expected post-reboot
warmup briefly crossed the warning band.

A second 2026-09-02 production control reproduced the same boundary without a
host reboot. At `05:55:23Z`, PostgreSQL reported 589 loopback clients, 577 of
them idle, 684 total client backends, and a 574-second oldest idle age. Thirty-
two seconds later, as that cohort reached its configured expiry, loopback
clients fell to 366, idle fell to 358, and total client backends fell to 461;
the following sample's oldest idle age reset to 462 seconds. A direct systemd
census afterward found all 32 PgBouncer units active, every `NRestarts=0`, and
every process start still dated 2026-08-09. The drop was timeout contraction,
not a PgBouncer restart, reload, or forced session termination. This second
control rejects the probe's former unconditional “with idle draining disabled”
diagnosis for a young cohort while retaining the warning as an early reserve-
pressure detector. The ten-minute §1.3a headroom gate remains the full closure
criterion.

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
>=100/minute for one service/site. A `netescrow-mirror-write` precursor also
warns on any line because PostgreSQL has already committed while the Redis
mirror mutation has an uncertain result. A NEW
signature appearing at rate (an unseen error shape / panic frame) is a
signal even when no known class matches — report it as class `novel` with
the sample line. Apply the novelty threshold to the most frequent normalized
shape, not the sum of unrelated shapes. Public web endpoints routinely receive
scanner bursts across dozens of nonexistent paths; nginx logs those misses at
error level, but many one-off paths do not constitute one recurring server
failure. Keep the total and distinct-shape counts as diagnostic context.

Connect's former TLS-loader fallback is a dedicated page class, not a generic
panic. Exact line `[c]Could not initialize tls config. Disabling transport. =`
means a legacy process replaced the failed identity with an empty TLS
configuration and could bind its UDP sockets while rejecting every QUIC
ClientHello. Class `connect-tls-disabled` alerts on the first line. Repair the
identity source and deploy server `64366fb5` or a descendant: its checked
constructor cancels startup before starting listener goroutines. A bound UDP
socket or green process unit is not a valid control; require a real handshake
on each enabled carrier. This is software/configuration correctness, not a
Proxy hardware-capacity alert. A bounded seven-day main search on 2026-09-02
found no matching line in Connect or Proxy, so this newly discovered failure
mode did not explain that day's provider degradation without new affirmative
evidence.

The 2026-09-01 `17:09:30Z` window supplied the PostgreSQL-capacity
classification control. Connect's four blocks emitted 16,140 cached
`too many clients already (server_login_retry)` records, 123 paired route
records, 16,218 `Unexpected error` records, and 16,341 goroutine-shaped
records in the bounded production pull; the standing monitor reported
16,342/min as generic panic, while API contributed 11/min. Those counts are
overlapping renderings, not independent failures. The literal cached-login
error now maps first to `pg-client-capacity`; §1.3a supplies the authoritative
direct ceiling and owner snapshot. The line count alone cannot assign a cause,
but the direct and database-log controls did: the repeated excluded-table
reindex produced WAL/storage waits and 60–66-second `COMMIT`s; deadline-expired
clients were lost while PgBouncer opened replacements and the old backends
unwound. PostgreSQL did not restart. The later young idle-in-transaction and
idle cohorts were recovery turnover, not proof that idle retention initiated
the exhaustion. The operational gate is to let current index progress finish,
then deploy the current-main Taskworker fixes `908a8b2c` and `d8392c83`; do
not tune the 32 pool shards as the first root-cause action.

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

The stderr boundary still needs narrow self-health parsing. Warpctl reconnects
an interrupted WebSocket internally, so an explicit `Tail read error ... no
route to host ... Reconnecting` does not exit the child or increment the
tailer's restart counter. Class `tailer-ipv6-route-loss` now reconstructs that
diagnostic across arbitrary stderr write chunks, groups independent service
tails by exact active edge address, and sends only the structured route event
through the alert path. Every other stderr line remains operator-only. A clean
next cadence emits the same address identity healthy, while §18.1 owns the
direct interface, public path, router, and same-second IPv6 controls. On the
macOS monitor host, a route event also triggers one bounded local `configd`
query for a same-window default-router lifetime reaching zero,
autoconfiguration detach, and IPv6 restoration. This proves local expiration
but does not say whether an explicit zero-lifetime advertisement arrived or
refresh advertisements were missed or late. Affirmative local evidence changes
the action owner to the monitor's first hop; an unavailable platform log leaves
the generic edge/upstream differential intact rather than hiding the event.

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

The 06:57Z watcher then exposed a service-discovery framing defect. `warpctl
ls services` logs `Found repo names ...` and follows it with a human-readable
service table; the monitor parsed everything after the marker in its combined
stdout/stderr buffer. The table was therefore appended to the final repository
name, and a transient nonzero exit could append warpctl's own JSON panic as
well. The resulting multiline string became a bogus Grafana tailer target and
eventually a malformed `tailer-silent` alert. Discovery now honors the command
exit status, isolates exactly the repository log line, and falls back to the
core service set rather than trusting partial output. Synthetic tests retain
both the normal production-shaped table and the exact partial-output panic
boundary, so local helper diagnostics cannot become remote service identity.

The rebuilt watcher made the dependency problem measurable: `warpctl ls
services` fetches tag histories for every Docker repository before printing
the service table. A slow Docker Hub response consumed the monitor's complete
60-second command budget; the safe core fallback then started no Grafana
stream, preserving the very visibility gap discovery was meant to prevent.
Production service inventory now comes directly from the `services` mapping
in the first (active) `services.yml` version already loaded by the monitor.
It starts deterministically without registry I/O and includes all eight active
services. Warpctl discovery remains only a compatibility fallback for custom
callers that omit the inventory. Synthetic coverage proves historical service
versions are ignored and that a configured inventory performs no registry
command at all.

The 15:15Z Redis writer validation exposed a different self-observation loop.
The bounded application-service searches correctly returned zero
`[redis][ttl]` result lines, but Loki and Mimir log each query at info level in
the `grafana` service. Their `engine.go:274` and `roundtrip.go:412`
`msg=\"executing query\"` metadata repeated the searched signature inside the
quoted query. Completed queries also emit `metrics.go:285` lines carrying the
same literal plus `query_hash` and `status`. The standing Grafana tailer first
reported six and 20
`redis-ttl-suspect` lines/minute even though Grafana cannot be the application
Redis writer. A first filter for the two start-query callers passed its unit
test, but an exact live replay still produced two/minute from the completion
shape; that failed control is why all three shapes are retained. This can
affect any class whose literal appears in a query, not only TTL warnings.
Classification now ignores only info-level Grafana query metadata with those
proven caller/field combinations, after updating tailer liveness. A real
Grafana warning and the same text from another service remain classifiable.
The deterministic regression feeds all three production shapes (including a
`panic:` query), proves they cannot create known or novel alerts, and proves
the exclusion does not hide real log lines.

The exact v137 live replay at 15:27:43Z exercised the completed filter. The
same bounded API search generated two start-query and two completed-query
Grafana lines containing `[redis][ttl]`; after a full standing-tailer drain
window, no Grafana TTL finding appeared. Real Redis residue and unrelated task
and proxy findings remained visible in that same window.

The 18:53Z payout recurrence then exposed a standing-stream completeness gap
that process health could not detect. An authoritative bounded query found the
fifth canonical Circle 429 at `18:53:30.571865Z` by the following minute, but
the standing taskworker stream never emitted either that event or its matching
five-attempt wallet peak through repeated drains. The same stream consumed a
newer `18:55:06Z` peak, and its `warpctl` child had remained alive without a
restart since 13:35:48 local time. The process was healthy; its contents were
incomplete. That missing event alone does not distinguish the late-timestamp
cursor mechanism below from the internal tail-backend EOF mechanism found
later. Both can leave a connected external WebSocket with incomplete contents.
Warpctl first runs `Search`, then `LiveTail` initializes a new Loki `/tail`
cursor at `time.Now()` and advances it to each emitted source timestamp. Loki
accepts an entry ingested later with an older source timestamp, but that record
is now behind the connected cursor and need not appear in the WebSocket stream.
A reconnect from the last source timestamp has the same blind spot, so this
remains an independent completeness risk after the transport fix.

The standing collector now keeps the low-latency WebSocket and independently
runs a bounded two-minute `warpctl logs` reconciliation every 45 seconds,
with a 20,000-line cap. Only timestamp-framed remote records enter the
classifier, so local retry diagnostics remain monitor evidence rather than
service errors. Exact fingerprints for alert-relevant lines are retained for
four minutes, making stream/query overlap and successive queries idempotent
without retaining ordinary log volume. The first query remembers pre-start
history but counts only source records at or after collector start, preventing
a two-minute startup replay from becoming a one-minute rate. Reconciliation
does not refresh WebSocket liveness. A failed or stale query, a cap boundary
that cannot advance, or a partition still full after eight pages raises the
separate `tailer-reconcile` visibility class; the live findings already in
memory are still drained. Deterministic synthetic tests reproduce a stream
that sees two of four same-second wallet attempts and misses a canonical 429,
recover the absent records from the bounded query exactly once, exclude
pre-start history, preserve a live finding on query failure, de-duplicate an
inclusive page boundary, reject a non-advancing boundary, and stop an
advancing hot partition after eight pages.

The `20:39:59Z` Proxy synchronization on 2026-08-31 supplied the first live
cap control. One block's full WireGuard synchronization emitted 12,866 peer
installation records and 3,338 no-configuration refusal diagnostics; its
terminal summary covered 14,110 candidate clients and six removals. The next
aggregate two-minute reconciliation reached exactly 20,000 lines even though
the live tail remained connected. Repeating that same absolute source-time
window for each active Proxy block left every individual partition below the
cap. The tailer now takes its block inventory from the first (active)
`services.yml` version: it uses the cheap aggregate query normally, and only
on saturation retries the identical window once per block. Block labels make
those batches disjoint. A cap-sized block continues from its inclusive final
source timestamp; exact fingerprints remove the repeated boundary. A boundary
that does not advance, or an eighth page that remains full, is still a
visibility failure. Do not shorten the late-ingestion overlap or raise Loki's
global limit to hide it. Synthetic tests prove block partitioning uses one
shared initial timestamp and that continuation is both complete and bounded.
This event is query-volume evidence, not 20,000 distinct Proxy failures; alert
artifacts retain aggregates rather than peer keys or client addresses.

The `21:09Z` full synchronization supplied the stricter live control after
v155 was the sole watcher. Proxy block g1 alone filled its first page: 19,995
of 20,000 returned records were default-info `peer installed` lines. In the
same minute, Grafana emitted 19,470 lines, including 18,165 Loki
`caller=tailer.go:<line> msg="tailer dropped streams is reset"` records, and
its single g1 block also filled a page. Loki 3.7.3 emits that message only
after a blocked tailer's bounded stream queue has dropped entries and its
dropped-stream metadata list fills and resets. The still-connected WebSocket
was therefore affirmatively incomplete; this was not a query-cap false
positive. Server Proxy now keeps the aggregate sync summary and peer gauge at
default verbosity but moves exact client address/key installation details to
`V(1)`. The standing classifier reports any reset as
`loki-tail-dropped-streams`, and reconciliation continues a cap-sized block
from its inclusive boundary for at most eight pages. Do not raise Loki queues
or suppress its reset line: remove the high-cardinality producer, retain the
bounded recovery path, and require both signals to stay healthy through the
next full peer sync.

The replacement v156 watcher supplied the end-to-end monitor control during
the next full synchronization at `21:39Z`. It was the sole watcher with all
eight service tails attached. A separate bounded two-minute query returned
10,000 Proxy records, including 8,447 per-peer installation lines, while the
matching Grafana query found 1,584 dropped-stream resets. The standing monitor
classified the reset burst at 4,219/min and completed its partitioned,
inclusive continuation without a `tailer-reconcile` finding. This validates
the loss classifier and recovery path; it does not validate the producer fix,
because the deployed Proxy image still predates the verbosity gate. That gate
must be verified on a later full synchronization after a Proxy deployment.

The v157 control then separated two independent Loki loss boundaries that must
not share attribution. In Loki 3.7.3, `pkg/ingester/tailer.go` retains ten
`DroppedStreams` descriptors and emits the reset above when that list fills.
The then-deployed querier, like current upstream, had
`pushTailResponseFromIngester` forward only `resp.Stream` and discard
`resp.DroppedStreams`. Consequently the reset was affirmative
ingester-to-querier loss, but the Grafana log was only its observation point:
it could not identify which external service selector lost records, and no
Warpctl client change could reconstruct the field after that hop.
At `21:56Z`, Loki emitted 1,634 resets in a bounded two-minute query while the
sole v157 watcher's eight fixed Warpctl tails reported zero direct
`dropped_entries`, proving these were different layers rather than a failed
client patch. The monitor retains the internal reset as
`loki-tail-dropped-streams` and recovers all service windows through bounded
reconciliation without claiming Grafana was the affected application tail.
Its structured symptom says `observation service grafana` and
`affected live-tail selector is unknown`; the target remains Grafana only as
the concrete log-emitting component used for ticket identity.

The next full synchronization at `22:09Z` supplied the saturated control with
the corrected v158 classifier as the sole watcher. Of the first 10,000 Proxy
records, 9,628 were per-peer installation lines; of the first 10,000 Grafana
records, 7,407 were ingester reset logs. v158 classified 14,282/min internal
`loki-tail-dropped-streams`, but all eight fixed Warpctl tails returned zero
direct `loki-tail-dropped-entries`. Its block-partitioned inclusive
reconciliation drained the saturated window without `tailer-reconcile` or a
tail restart. This proves the external consumer and downstream response queue
kept up while loss occurred earlier at the ingester-to-querier boundary. It
also leaves both producer/transport gates open: the deployed Proxy still emits
one default-info line per peer, and the deployed Grafana still predates Warp
`1e95aef`.

The v163 watcher supplied an independent recurrence at `23:53Z`, more than
three minutes after its predecessor had exited and with no collector overlap.
Fireside Proxy g10 emitted 9,891 default-info `sync peer installed` lines in
about 111 milliseconds; three ordinary add lines brought the fleet's
wall-minute peer-install count to 9,894. A matching exact Grafana query found
5,470 dropped-stream resets in that wall minute, versus 119 and 226 in the
adjacent minutes, while the standing rolling window reported 5,535/min. The
discrete producer/reset shape rules out watcher handover as the spike source
and independently confirms the high-cardinality full-sync amplification.
The nonzero adjacent-minute reset baseline remains a separate reason to deploy
the Grafana transport fix. Deploy server Proxy `e055c98c` or later and Grafana
with Warp `1e95aef` or later; verify the next full sync has one aggregate
summary per reconciling instance and zero dropped-stream resets rather than
treating either fix as a substitute for the other.

The `00:09Z` recurrence exposed a material undercount in an unpartitioned
control query. The service-wide Proxy read reached its 20,000-record cap and
initially appeared to contain 19,995 sync-detail lines. Repeating the identical
absolute window by block, then checking the source journals, produced the
complete retained count: 39,378 default-info `sync peer installed` lines over
roughly 13 seconds. Fireside emitted 19,800 and Crisp emitted 19,578 across
g1, g4, and g5. Grafana emitted 6,961 ingester reset lines in the matching wall
minute, versus 181 in the prior minute; the standing rolling window reported
7,105/min. The previous g1/g4 reconcile starts were one configured
30-minute interval earlier, proving that near-synchronous process starts had
aligned normal periodic full syncs. This was neither a deployment nor watcher
handover and did not show 39,378 failed peer installations. It was the
already-identified O(peers) default logging defect multiplying a correctness
reconcile into a live-tail producer burst.

The next g1 interval repeated at `00:39Z` and supplied a second complete
control with a different generation cohort. Block-partitioned range reads
returned exactly 38,513 installation lines over about eight seconds: 22,400
from g1, 6,326 from g2, and 9,787 from g3. Direct host journals independently
returned the same 38,513 total, split 19,623 on Fireside and 18,890 on Crisp.
Grafana emitted 10,937 ingester reset lines in the matching wall minute versus
162 in the preceding minute. g1's recurrence exactly one configured interval
after the `00:09Z` wave, while g2 and g3 joined this rotation, confirms aligned
periodic reconciliation rather than a deployment or monitor lifecycle event.
The exact journal/range-query agreement also proves persisted records were
recoverable through the partitioned reconciliation even though the live tail
lost streams; it does not validate the still-undeployed producer verbosity
fix.

The `02:09Z` rotation supplied a third independent pre-fix production control.
Block-partitioned reads found 39,583 peer-detail lines across g1, g3, and g9
(`19,809 + 9,800 + 9,974`); every line came from `server.go:764`, the
unconditional pre-`e055c98c` callsite. Six direct Grafana journals returned
exactly the range-query reset total of 9,058 over `02:08:30Z-02:11:00Z`, split
by wall minute as `137 + 7,523 + 1,398`. The producer wave began at
`02:09:40Z`, while sole watcher v165 did not start until `02:11:41Z`, excluding
watcher promotion. This immutable source boundary proves production still
needed Proxy `e055c98c` at that time; a version label or capped service-wide
aggregate alone was not sufficient rollout evidence.

Never report a service-wide cap as this event's total. Use the configured block
inventory, one shared absolute window, and bounded inclusive continuation;
direct journals are the independent control when attribution itself is in
question. Do not jitter or disable full reconciliation to suppress its output.
First inspect artifact ancestry. A Proxy artifact predating `e055c98c` needs
the verbosity gate so each reconciling instance retains one aggregate summary
and its peer gauge while per-peer details require `V(1)`; a Grafana artifact
predating Warp `1e95aef` independently needs the ring transport fix. Do not
redeploy already-current blocks from this historical evidence.

The first post-fix control on `2026.8.31+1034210530` changed the active diagnosis.
Every responding Grafana and Proxy block contained the ring-deadline and
per-peer-log fixes, and the current edge-4 Grafana container had started at
`02:29Z`, after that artifact boundary. A fresh one-minute Grafana window still
contained 1,762 records: 344 Mimir query-frontend statistics lines, 344 Mimir
evaluator statistics lines, 252 Loki table-manager lines, 238
`tailer dropped streams is reset` lines, and 175 Grafana Prometheus-plugin
completion lines. Roughly 170 short alert-rule queries per minute therefore
made the observation plane its own default-info producer after the Proxy burst
was removed. Direct five-minute journals independently found 280 reset lines on
Crisp while Fireside emitted none because it was no longer an active Loki ring
member, not because it was a healthy control. That was a real prerequisite at
the time, but it is historical rather than a standing action.

Controlled netplan activation cleared that prerequisite at `04:12:44Z` on
Fireside and `04:14:34Z` on Crisp. Both hosts then retained their configured LAN
identity, local scheduler/database paths, fresh cross-host metrics, and ring
membership. A direct immutable-container window from `04:15:08Z` through
`04:29:30Z` across edge-0, edge-1, edge-3, edge-4, Fireside, and Crisp contained
35,909 Grafana records. Of those, 11,583 were dropped-stream resets, 5,511 were
Mimir query-frontend statistics, another 5,511 were evaluator statistics, 4,596
were Loki `caller=table_manager.go:195 msg="get or create table"` records, and
182 were exact unquoted backend EOFs. Those EOFs split 37 on edge-0 and 145 on
edge-4; 48 quoted cancellation records
from deliberate watcher retirement were counted separately and remain excluded
from the class. All six active nodes were healthy, so missing LAN identity is no
longer the current reset or EOF prerequisite.

The exact transition supplied the causal discriminator. Loki 3.7.3 gives each
ingester tail a 100-stream processing queue and a five-stream send queue. The
first dropped stream sets `blockedAt`; when another stream arrives more than 15
seconds later, the ingester closes that server-side tail. `Ingester.Tail`
returns nil, the querier's `Recv` observes EOF and removes the client, and its
five-second connection ticker reconnects. The first edge-0 EOF arrived about 17
seconds after the replacement watcher's tail children started, during the same
reset wave. Current `loki_querier_tail_active` state then showed all eight
external tails on edge-0 and zero on the other five nodes, proving that the
emitting host followed the selected querier and was not the failed backend
identity. This makes the present off-grid EOF a downstream blocked-tail symptom,
while process exit or ring loss remain alternate ways to produce the same text.

Warp commit `42168fe` disables Mimir `frontend.query_stats_enabled` in the
generated Grafana config. That option controls the query-frontend `query stats`
record; at this point it was expected, but not yet proven, to cover evaluator
records too. It retains query execution, errors, metrics, and alert cadence and
was a controlled producer reduction rather than proof that all remaining volume
was harmless or that it was the sole cause. The deployed Loki log also had an
`addr` variable in scope but omitted it from the EOF record. Warp commit
`bca37cf` adds that backend address and a pinned-upstream regression, while the
monitor accepts both rolling formats and frames new alerts by backend. The
combined deployment was therefore a discriminator: measure each exact producer
afterward, require every bounded reconciliation partition to drain, and retain
the reset and EOF records as correctness evidence.

A direct post-deployment audit at 12:39Z on 2026-09-01 proved that boundary was
still absent from the then-current `2026.8.31+1034210530` generation. Warpctl's
deployed-status sample found the same generation on all 20 Grafana endpoints.
On each of the six enabled ring hosts (edge-0, edge-1, edge-3, edge-4,
Fireside, and Crisp), the running container's generated `mimir.yml` omitted
`query_stats_enabled: false`, and `loki --version` reported
`3.7.3-urnetwork.1` at revision `82cdcdc0+tail-close-once`, not the
`urnetwork.2` / `+tail-backend-addr` build produced by `bca37cf`. The same live
monitor cadence observed 556 dropped-stream resets/minute and ten backend
EOFs/minute with an empty backend frame. This is immutable runtime evidence
that another Grafana build/deploy is required; the later-looking version label
does not satisfy either commit gate.

The later `2026.9.1-outerwerld+1035004200` deployment supplied that missing
discriminator. All 20 endpoints reported the generation. Extracting the
running parent binary from one container without writing the host proved clean
embedded Warp revision `71731e4`; both `42168fe` and `bca37cf` were ancestors.
A clean, sole-watcher window from `22:44:05Z` through `22:49:22Z` contained zero
query-frontend `query stats`, proving the first option worked, but still
contained 2,825 `caller=evaluator.go:94 msg="evaluation stats"` records. Mimir
3.1.1 emits that streaming-engine record unconditionally at info level, outside
the query-frontend option. The same window contained 1,084 Loki
`get or create table` info records, 942 Mimir bucket-index warnings, 1,229
caller-attributed dropped-stream resets, and 22 exact backend EOFs. The EOFs
named every active backend: edge-3 6, Fireside 5, edge-1 4, edge-4 3, Crisp 2,
and edge-0 2. This rules out stale provenance and a single failed node while
showing that `42168fe` removed only half of the originally paired Mimir stream.

Warp commit `13fcd05` supplies the bounded producer-side fix. It renders Mimir
`server.log_level: warn`, stopping the unconditional evaluator info records at
their source while retaining warnings, errors, query metrics, and alert
evaluation. It changes the single-tenant store-gateway bucket-index refresh
from the jittered 15-minute default to one minute, so a new shared generation
cannot remain locally undiscovered for the observed 882 seconds. The same
checksum-pinned Loki 3.7.3 source patch moves only the per-query `get or create
table` line from info to debug; reset and EOF evidence remains at its original
level. Deterministic rendered-config tests, focused/race/full Warp tests, vet,
an exact patch dry-run, and both affected patched-upstream Loki package tests
pass. Warp commit `5927527` is the deployable descendant: it preserves all of
`13fcd05` and additionally fixes the attribution defect in the checksum-pinned
Loki source. Its querier retains up to the existing 1,000-descriptor response
bound, converts each ingester `DroppedStream` to the existing HTTP
`dropped_entries` shape, and forwards it on the same service-specific
WebSocket. It does not enlarge the ingester's 100/5 queues, the querier's
ten-response queue, or any tail limit. A deterministic patched-upstream test
injects an ingester drop descriptor and requires its timestamp and labels in
the exact downstream response; ten race repetitions, the full upstream tail
package, focused vet, the complete Warp suite, and an exact patch dry-run pass.
Current official Loki main inspected on 2026-09-01 still discards this field.

Deploy a Grafana image containing `5927527`; this is neither a queue increase
nor an alert suppression. Closure requires zero query-frontend/evaluator and
table-lookup info records, bucket-index version convergence below two minutes,
healthy rules/metrics/warnings/errors, complete bounded reconciliation, and
zero raw reset, service-attributed dropped-entry, plus EOF classes for ten
minutes. Any residual dropped-entry summary names the affected service tail;
pair it with same-window resets to identify the ingester stage. Any residual
EOF remains framed by backend and must be diagnosed on that exact node.

The querier also has its own ten-response channel to the WebSocket. If that
later queue fills, Loki attaches up to 1,000 `dropped_entries` descriptors to a
successful HTTP tail response. After Warp `5927527`, the earlier ingester
descriptors use this same response field, so a non-empty response proves loss
for the named service but does not by itself distinguish the two stages. A
same-window raw reset identifies the ingester path. Warpctl decoded the API
field but `LiveTail` ignored it and printed only `streams`. Warp commit
`26089b2` fixes this client defect by emitting one local
`[warpctl][loki-tail-dropped-entries] service=<service> count=<n>` summary for
each non-empty response. It deliberately omits labels and timestamps, which
may be sensitive or high-cardinality. The monitor maps the summary to the
separate `loki-tail-dropped-entries` class; the owning standing tail supplies
exact service attribution for either source. Deterministic Warpctl tests
exercise the same response-processing function as `LiveTail`, require the
summary, and prove no label or timestamp reaches output. A server synthetic
test requires that direct summary to open the downstream class against the
affected service.

An independent audit of Grafana's own low-rate errors then found a second,
concrete transport gap. With the v151 watcher and all eight external tails
stable, Loki repeatedly emitted
`caller=tail.go:230 component=tail-querier ... msg="Error receiving response
from grpc tail client" err=EOF`. Initial steady samples were 2–14/minute.
Individual backend failures recurred on the proxy's exact idle grid: edge-1
at `19:15:35.929Z` and `19:16:35.928Z`, edge-0 at `19:16:57.348Z` and
`19:17:57.348Z`, another pair at `19:17:14.848Z` and `19:18:15.947Z`, and
`19:20:51.247Z` to `19:21:51.847Z`. The 59–61-second cadence is not ordinary
Loki query jitter.

The root was Warp's Grafana ring TCP proxy. `warp/grafana/main.go` applied
`ringIdleTimeout = 60s` through `SetReadDeadline(now+60s)` before every TCP
read. Loki and Mimir use long-lived HTTP/2/gRPC connections that can validly
carry no application bytes for longer than a minute, so the front closed a
healthy backend stream and Loki surfaced EOF. This was not ring admission:
the live Loki/Mimir established-session counts were only 74/24 on edge-0,
61/24 on edge-1, 61/24 on edge-3, and 62/24 on edge-4, all well below
`maxRingTcpSessions=256` even with both connection directions counted.

Warp commit `1e95aef` removes the TCP application read deadline, enables TCP
keepalive with a 30-second period on both accepted and backend sockets, retains
the bounded write deadline, and keeps the independent UDP idle timeout. The
deterministic `TestCopyRingTcpDoesNotExpireIdleGrpcStream` proves a valid idle
stream receives no read deadline and still forwards its next payload;
`TestEnableRingTcpKeepAliveDetectsDeadIdlePeers` proves keepalive is enabled.
Focused race tests, the complete `grafana` package, and `go vet ./grafana`
pass. The later `2026.8.31+1034210530` Grafana deployment contains this commit.

The standing classifier reports only the unquoted internal EOF form as
`loki-tail-backend-eof` at 5/minute. It deliberately excludes quoted gRPC
`Canceled ... context canceled` lines: explicitly retiring the old v150
watchers at `19:17:37Z` produced a large expected cancellation burst without
proving a backend fault. The current fixed-image window above proves the same
EOF text is not cause-specific: its off-grid instances co-occurred with the
15-second blocked-tailer reset path after all active ring nodes were healthy.
Because the emitting host follows the externally selected querier, a pre-
`bca37cf` line cannot identify its failed backend and must retain an empty frame.
After `bca37cf`, the exact `addr=<backend>` becomes the stable alert frame; the
synthetic regression requires that format while preserving legacy detection and
the cancellation negative control. Verify the running artifacts contain `1e95aef` and `bca37cf`, and
deploy them only to older Grafana blocks. For the current fleet, deploy one
Grafana image containing `5927527` (and therefore `13fcd05` plus the earlier
fixes). Do not claim the producer or attribution changes themselves fix loss,
rebuild from the historical deadline prose, raise Loki tail-request limits,
raise the fixed queues, or raise the ring-session cap. Closure requires
query-frontend/evaluator and table-lookup info records at zero with rules,
query metrics, warnings, and errors healthy, every active ring member owning
its LAN identity and heartbeat,
and ten minutes of stable external tails with zero reset, service-attributed
dropped-entry, and EOF classes. A residual framed EOF is investigated on that
exact backend. Bounded two-minute
reconciliation remains required because late source timestamps and the
Search-to-tail handoff are independent completeness boundaries.

The first production run of that classifier supplied its end-to-end control.
Monitor v152 was built from clean server commit `45d832aa` (binary SHA-256
`72f9b929691d43f27b8ed43603a14573972eb914f945be53b8409662b142a1a4`),
started all eight configured standing tails, and completed a healthy bounded
reconciliation. Its first full log cadence at `19:36:13Z` rendered
`loki-tail-backend-eof` at 6/minute with the commit-specific action above.
The old v151 watcher was then terminated normally; its client-cancellation
burst did not enter the EOF class, and v152 retained the same eight tail
children as the sole watcher. This proves the detector against the live
failure without treating watcher retirement as a backend incident.

The first live reconciled watcher rejected the initial five-minute window
rather than silently trusting it: both proxy and Grafana reached the
20,000-line cap. A simultaneous volume matrix measured proxy at 771 and
Grafana at 2,974 lines over one minute, 978 and 6,801 over two minutes, but
17,073 and 16,808 over three minutes before both capped over five. The sharp
boundary was real burst history, so raising the cap would trade visibility for
observation-side memory. The production window is therefore two minutes every
45 seconds: it covers more than two reconciliations and the observed
sub-minute late-ingestion event while retaining measured cap headroom. A
future two-minute cap still fails visibly; it is never shortened silently.
The clean `c843c192` watcher completed a full cadence with all eight streams
stable and every two-minute query below the cap; the succeeding `c1ab149b`
watcher repeated that result after the corrected net-escrow alert text was
loaded. Neither emitted `tailer-reconcile`, `tailer-silent`,
`tailer-restarting`, or `cannot-observe`.

The 22:23Z Connect audit found a previously unclassified recovery defect. A
bounded two-hour query returned 131 canonical
`http: response.WriteHeader on hijacked connection` lines across several
blocks, each paired in the standard library with a rejected body write. The
same window contained zero `[h]unhandled error from route` lines. Router
recovery always logs that marker for an unexpected panic before attempting its
500 response, so its complete absence isolates the other branch: Connect's
`GET /` route had handed the H1 socket to Gorilla, a later expected
`server.IsDoneError` panic was deliberately suppressed, but control then fell
through to `http.Error` after net/http no longer owned the connection.

Router recovery now returns immediately for that expected cancellation branch
while retaining error accounting, the stack record, and the ordinary 500 for
unexpected pre-hijack panics. The deterministic reproduction crosses a fake
Hijack boundary, raises the same Done panic, and failed on the old code with
two post-hijack writes including status 500; it now requires zero. A companion
test preserves the unexpected-panic response. The `http-hijack-write` log
class counts only the canonical WriteHeader line so one recovery does not look
like two incidents. Deploy this server revision to Connect, then require ten
minutes of ordinary H1 teardown with zero class lines. Do not silence
net/http's logger globally: any post-fix warning accompanied by an `[h]`
record is a real route panic with a separate root cause.

Monitor v160 supplied the live detector control. It was built from server
commit `02f4d29a` (binary SHA-256
`1e7a5c234072ce8aea16a4b0cd4708aacdbbe00542403645c4ad5737a6b91950`),
ran as the sole watcher with all eight standing service tails and healthy
bounded reconciliation, and classified the first natural post-start Connect
warning at `22:38:58Z` as `http-hijack-write` at one/minute. The same alert
retained the mechanism, action, and verification contract above. Its generic
200-byte evidence sample cut the final source at `router.(*Rou`, however,
which would conceal whether a future line came from the known Router recovery
or another post-hijack writer. The class-specific sample now preserves the
complete `http: response.WriteHeader ... from <source>` suffix, with a
deterministic markdown assertion for
`router.(*Router).ServeHTTP.func1.1 (router.go:104)`.

The `2026.8.31-outerwerld+1033803620` Connect rollout then supplied the
deployment boundary. At `23:02Z`, `warpctl ls versions --sample` reported that
build on all 20 blocks in each of beta and g1-g4 (100 total). Fresh bounded
30-minute Connect queries returned zero `[redis][ttl]` and zero
`redis-ttl-suspect`, independently confirming the stream-TTL writer fix in
that image. The same fleet still emitted eight canonical
`http-hijack-write` lines in the latest ten minutes, including four between
`23:00:50Z` and `23:01:39Z`, all from the known Router recovery frame. Commit
`02f4d29a` was made after this image was built, so the result does not regress
the TTL fix: it proves that one newer Connect image is still required for the
separate post-Hijack recovery fix. Re-deploying `1033803620` cannot clear that
class.

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

One exact Payout shape is not allowed to become normal through that history
rule. A 2026-08-31 retry spent several minutes computing
`MIN(transfer_contract.create_time), MAX(transfer_contract.close_time)` from
the complete `transfer_escrow_sweep` history with no wait event. The same
transaction had already materialized `temp_account_payment`, containing the
exact unpaid/safely-canceled set and the bounded plan's close-time cutoff. The
later historical scan therefore selected the wrong, broader epoch as well as
making each bounded payout proportional to the lifetime sweep table. The
probe recognizes the complete MIN/MAX/FROM shape and warns after two minutes
even when its current duration is below twice the polluted completed mean.
The source fix joins `transfer_contract` to `temp_account_payment`; do not add
an index for the redundant scan or cancel a live bounded attempt. Verify zero
new legacy executions in both `pg_stat_activity` and `pg_stat_statements`, then
require the same Payout row to commit and clear its error.

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

At 06:03Z on 2026-08-31, a read-only one-shot observation found one
`IO:DataFileRead` waiter at 71s. It cleared before the immediate attribution
query, leaving only sub-second protocol handoffs and a one-second B-tree page
wait; there was no DDL lock or persistent I/O cluster. This also exposed an
alert-detail defect: the probe selected a representative SQL sample but did
not put it into the structured alert, and its rendered baseline named only the
five-backend branch even though the query deliberately also selects one
waiter older than a minute. The alert now carries the sample query, both
thresholds, and `DataFileRead`-specific attribution guidance. The query also
selects the PID, query ID, application, client address, and sample from the
same oldest waiter, retaining an attribution snapshot when a transient command
finishes before the follow-up query. A one-shot aged singleton remains
evidence to validate, while the standing monitor's
two-observation gate distinguishes recurrence before opening a ticket.

At 06:05Z the next one-shot found a different singleton,
`Client:ClientWrite` at 96s. It too completed before the immediate PID query,
so it was not the earlier data-file read persisting under another label and
did not establish a migration stall. This class means PostgreSQL cannot send
more result bytes until its client resumes reading. The structured alert now
directs attribution to the exact SQL/client result-consumption path and
requires recurrence before changing PostgreSQL or canceling the backend.

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

### 2.2a Incomplete concurrent-index debris — retry residue vs a live build
Probe: `reindex-debris`

`REINDEX ... CONCURRENTLY` temporarily creates invalid indexes with PostgreSQL's
`_ccnew`, `_ccnewN`, `_ccold`, or `_ccoldN` suffixes. One belonging to a live
`pg_stat_progress_create_index` row is expected transient state. The healthy
steady-state count is zero after that operation exits. An inactive suffixed
index is durable retry debris: it consumes storage, and any artifact with
`pg_index.indisready=true` is also maintained by later writes even though a
query plan cannot use it.

The probe reads only catalogs. It maps both each public table and that table's
`reltoastrelid`, counts invalid suffixed indexes and their bytes, reports how
many are write-ready, and excludes an exact index named by
`pg_stat_progress_create_index.index_relid`. A table-wide concurrent rebuild
can have non-exact `_ccnew`/`_ccold` siblings that include both its current
transient work and older debris; those candidates are neither called inactive
nor hidden. The alert reports confirmed inactive bytes as a lower bound and
active-table candidate bytes separately as unclassified and explicitly not
reclaimed. If only active-table candidates exist for two probes, class
`reindex-debris-obscured` preserves that visibility boundary without directing
cleanup. From the same catalog snapshot, each active owner carries its bounded
`pg_stat_progress_create_index` relation/index, command, phase, query age,
wait, blocker count, and block/tuple/locker/partition done/total counters.
Compare phase and counters across samples: movement is forward progress, while
one flat point is not enough to call a stall. Phase-specific zero totals remain
literal PostgreSQL evidence rather than an inferred percentage. Including
TOAST is required: a full-table reindex also rebuilds the
associated TOAST index, while the old cleanup query looked only at indexes
whose direct parent name matched the public table. The alert is therefore not
an invitation to drop a temporary index under a progressing build. All
affected tables share one cleanup and deployment boundary, so the probe emits
one aggregate alert with bounded samples rather than repeating identical
guidance for every table.

The 2026-09-01 incident established the causal chain. At 11:15Z, Connect login
receives against PgBouncer timed out across many blocks while PgBouncer's
one-minute average transaction and wait times rose from milliseconds to
hundreds of milliseconds and, on one shifted sample, seconds. PostgreSQL had
not restarted, PgBouncer's 32 real instances remained active, the host had no
contemporaneous kernel OOM, and API/taskworker did not report the same login
signature. Direct PostgreSQL sampling instead found:

- `REINDEX TABLE CONCURRENTLY transfer_escrow` active on the roughly
  1.08-billion-row, high-churn table (about 153.5 GiB heap and 257.8 GiB with
  indexes), waiting on `IO:DataFileExtend`;
- simultaneous `LWLock:WALInsert` and `LWLock:WALWrite` clusters on ordinary
  inserts; and
- PostgreSQL warnings that the same attempt was skipping dozens of invalid
  public and TOAST `_ccnew` indexes, with numbered suffixes from earlier
  attempts.

The first direct run of the new catalog probe at 11:44:43Z validated both
branches. It excluded active `transfer_escrow`, while finding 315 inactive
artifacts across 35 other public-table owners: 301 were write-ready and the
total occupied 6,714,458,112 bytes (6.25 GiB). `contract_close` owned
6,678,208,512 bytes across 13 not-ready artifacts; `pending_task` owned the
largest write-ready byte set at 31,645,696 bytes across 21 artifacts. The
remaining high counts were predominantly 8 KiB TOAST remnants. This separates
material disk debt, write-maintained overhead, and catalog-count noise without
weakening the zero-artifact healthy invariant.

The protected `transfer_escrow` rebuild exited at 11:54:17Z after 2,035.18
seconds. The first post-operation probe at 11:57:39Z then exposed what the live
relation exclusion had intentionally hidden: 330 inactive artifacts across 36
owners, 304 write-ready, totaling 180,814,946,304 bytes (168.40 GiB).
`transfer_escrow` alone owned 15 artifacts totaling 174,100,488,192 bytes.
A second read-only sample at 12:09:51Z found 317 artifacts across 35 owners,
304 write-ready, totaling 174,179,827,712 bytes (162.22 GiB): the 13 not-ready
`contract_close` artifacts had disappeared while all 15 `transfer_escrow`
artifacts remained. The changing count is compatible with the deployed task's
cleanup phase advancing; it is not closure while any inactive artifact
remains. The PostgreSQL filesystem still had 2,529,818,935,296 bytes free at
66% use, so this was material storage and write amplification rather than an
immediate disk-exhaustion emergency.

A later monitor run exposed a reporting defect in that relation-wide
exclusion. The headline moved from 169.19 GiB at 12:37:15Z to 6.92 GiB at
12:42:17Z, returned to 169.63 GiB at 12:47:18Z, then moved from 162.76 GiB at
12:49:17Z to 7.60 GiB at 12:59:25Z while index progress started and stopped on
one large table. No authorized cleanup occurred, and the candidate count moved
with the same table boundary. The old query had removed every invalid sibling
whenever any index operation shared its parent relation, so active work made
roughly 155–162 GiB disappear from the alert without reclaiming it. The new
lower-bound/active-candidate split prevents that false closure. Synthetic
regressions pin the combined headline, the active-only obscured class, exact
progress exclusion, bounded progress fields, malformed counts, missing or
impossible progress detail, and the rule that a confirmed-byte dip paired with
active candidate bytes is not cleanup.

The first production run of the corrected probe at 13:31:41Z resolved the
apparent cleanup: 320 confirmed inactive artifacts across 35 tables occupied
at least 162.90 GiB, while 18 additional invalid candidates totaling 7.56 GiB
shared one table with active index work. The active operation had moved away
from the previously masked large owner, so the confirmed and unclassified
buckets swapped scale while their combined storage remained in the same
roughly 170 GiB band. This validates both the lower-bound wording and the rule
that neither bucket may be interpreted as reclaimed until progress exits and
the catalog reaches zero.

The owner split also proves the maintenance root fix is not deployed. At that
same corrected sample, `transfer_escrow` was fully inactive with 18 artifacts,
3 write-ready, totaling 174,875,197,440 bytes. The earlier post-operation
sample had 15 artifacts totaling 174,100,488,192 bytes: three more siblings
and 774,709,248 additional bytes were created. Meanwhile `contract_close`
owned the 18 active-table candidates, all not-ready, totaling 8,122,728,448
bytes. This is continuing retry creation, not merely a new presentation of the
old catalog. Deploy Taskworker from an intentional local server checkout
containing current-main commit `908a8b2c` before another maintenance cycle; focused
normal and race tests for the transfer exclusion and
cleanup-before/rebuild/cleanup-after state machine
pass. Do not conflate that software deployment with authorization to delete
the existing relations while `contract_close` progress remains active.

The same legacy policy recurred under a later owner boundary. Task
`<redacted-id>`, epoch 422, started old-format
`maintenance reindex[6/22] transfer_escrow` on edge-3/g1 at 16:33:01Z. It did
not emit a matching completion line; its last heartbeat was at 16:44:27Z and
the maintenance call unwound at 16:44:57Z while other tasks on that process
also became stranded. At 16:47:23Z the corrected catalog probe found 355
confirmed inactive artifacts totaling 231.07 GiB. `transfer_escrow` now owned
24 artifacts and 234,002,931,712 bytes, one more failed-attempt artifact than
the preceding sample. The same task was lease-recovered by edge-3/g2 at
16:49:19Z, replayed the old 22-table rotation, and launched old-format
`maintenance reindex[16/22] contract_close` at 16:50:38Z. A focused catalog
read then kept the 24 `transfer_escrow` artifacts in the confirmed bucket and
framed 29 `contract_close` candidates totaling 14,070,284,288 bytes behind the
active relation. This is direct launch and retry evidence, not proof that the
active rebuild completed or that either byte bucket was reclaimed. Do not
roll Taskworker across this operation merely to land the fix; first establish
its current PostgreSQL progress and make any cancel-versus-finish decision as
an explicit authorized database operation.

That retry then reproduced the heartbeat/cancellation half of the chain. Its
last heartbeat was 16:53:09Z. The active `contract_close` call stopped without
a completion line at 16:53:39Z; in the same millisecond, the canceled context
printed start lines for all remaining tables and then cleanup lines for all 22
tables, with no per-object completion. In particular, the 16:53:39Z
`transfer_escrow` start line did **not** correspond to a new PostgreSQL backend:
the instantaneous unwind and a 16:57:42Z progress-free catalog sample are the
negative control for interpreting the standing class. That catalog sample
instead found 356 confirmed inactive artifacts totaling 249,039,659,008 bytes
(231.94 GiB). `contract_close` had risen from 29 active candidates to 30
inactive artifacts totaling 14,936,702,976 bytes, while `transfer_escrow`
remained at 24 artifacts totaling 234,066,706,432 bytes. The second interrupted
`contract_close` build therefore created another durable artifact, and the old
end-of-rotation cleanup did not remove it. This independently validates both
source fixes: adjacent cleanup/exclusion (former `7676014f`) and preserving
ownership through a pooled timestamp-refresh stall (former `abfd976b`). After
main was rewritten, stable `git patch-id --stable` output proves those exact
patches are now commits `908a8b2c` and `d8392c83`, respectively. Operational
ancestry gates use the current-main hashes; the former hashes remain only as
historical incident identifiers.

The lease recovered the same task a third time on edge-0/g1 at 16:58:04Z.
The unchanged legacy policy again selected 22 of 171 tables, including four
daily reliability partitions, `contract_close`, and `transfer_escrow`; its
random order reached `contract_close` at 16:58:09Z after only two tables. Its
last heartbeat was 17:00:35Z and the call unwound 30 seconds later. At
17:03:38Z, the catalog contained 357 inactive artifacts totaling
250,213,687,296 bytes (233.03 GiB): `contract_close` had risen to 31 artifacts
and 16,077,586,432 bytes. A fourth recovery began on edge-0/g2 at 17:05:27Z,
selected two excluded daily partitions and then `contract_close`, and produced
the first live `db-maintenance-legacy-reindex` alerts at 17:06:18Z with all
three exact table frames. Its last heartbeat was 17:08:33Z, the canceled loop
reached its log-only `transfer_escrow` entry 30 seconds later, and the
17:09:48Z catalog contained 358 inactive artifacts totaling 250,231,382,016
bytes; `contract_close` had risen again to 32 artifacts. Each interrupted
backend added one durable artifact. This five-minute recovery is an ongoing
artifact-creation loop, not a historical debris inventory. It also supplied
the production fixture for matching excluded daily partition names in
addition to the seven exact-table exclusions.

Later samples supplied the first same-snapshot progress controls for this
loop. At 18:21:40Z, `transfer_escrow_unsettled_balance_contract_ccnew10` was
in `index validation: scanning table`, with no wait or blockers and
10,190,966 of 20,116,380 blocks complete (50.7%). At 18:24:45Z that exact
operation had exited and all 30 `transfer_escrow` artifacts, totaling
291,331,448,832 bytes, were confirmed inactive; the same legacy cycle had
immediately moved to `contract_close_pkey_ccnew33`, which was in
`building index: scanning table`, with no wait or blockers and 27,621,896 of
29,871,309 blocks complete (92.5%). At 18:27:15Z progress was empty, but the
catalog held 366 confirmed inactive artifacts across 36 owners, totaling
307,542,106,112 bytes (286.42 GiB): `transfer_escrow` owned 30 and
`contract_close` owned 34. Legacy start lines for two excluded daily
reliability partitions followed at 18:26:21Z. Thus phase/counter movement and
the empty final progress sample prove this retry advanced through its bounded
operations; the larger inactive inventory proves that completion was not
cleanup. One table leaving progress never authorizes an interruption while
the next table is active. The rollout gate opens only on an empty progress
sample, and deployment still separately requires readable exact Warpctl identity
under section 8.13 plus running-artifact verification under section 8.12.

The next lease recovery demonstrated the new progress evidence in the
authoritative watcher. At 18:30:54Z, the legacy task selected
`contract_close` first and PostgreSQL was building
`contract_close_pkey_ccnew34`: 4,510,923 of 29,871,309 blocks were complete,
the wait was `IO:BuffileWrite`, and there were no blockers. At 18:31:55Z the
same exact index had advanced to 15,985,477 blocks with no wait or blockers.
Progress was empty at 18:33:58Z, but `contract_close` then owned 35 confirmed
artifacts totaling 17,110,269,952 bytes, one more than before the recovery;
the fleet total was 367 artifacts / 308,522,303,488 bytes (287.33 GiB). This
is the positive production control for the phase/counter fields and another
direct reproduction of the legacy artifact-creation loop.

The 19:41Z recovery then tied the loop to its user-visible capacity cost a
second time. `contract_close_pkey_ccnew36` began at 19:41:17Z. Within 76
seconds direct PostgreSQL frames moved from 710 total / 5 active clients to
806 total / 162 active and then 891 total / 666 active. The bounded database
log window recorded 51 rejected logins, 682 statements over 30 seconds, 37
over 60 seconds, 271 client-loss records, and 289 cancellations; the
`UpdateClientLocations` canary fell from 6-8 completions/minute to 3 and then
2. PostgreSQL canceled the reindex at 19:45:22Z after its maintenance client
lost the pooled heartbeat path, and the catalog rose to 369 invalid artifacts
/ 289.02 GiB, including a new 37th `contract_close` artifact. The row still
had a 24-hour task maximum and no reschedule error, but its last successful
claim refresh expired at 19:49:23Z. Another worker reclaimed the same row at
19:49:34Z and started `contract_close_pkey_ccnew37` immediately. This
same-window sequence proves both the database-capacity mechanism and the
five-minute lease-retry mechanism without inferring either from a later idle
snapshot.

Standing class `db-maintenance-legacy-reindex` matches only the old-format
start line for an exact table or daily reliability partition excluded by
current policy, groups it by table, and pages on the first occurrence. It
deliberately excludes the later `reindex took` completion line, ordinary
old-format tables, and the fixed
`maintenance table[...] <step> <table>` state-machine format. Old code writes
the start line before it opens the maintenance connection, so this proves
legacy selection and call-path entry but not that PostgreSQL began the
statement. The page gives that legacy attempt an immediate operational gate
while `pg_stat_progress_create_index` and `reindex-debris` remain the sources
of truth for an active backend and for confirmed inactive/active-table
candidate artifacts.

The pool timeout was consequently a downstream queue symptom. The daily
maintenance scheduler had two independent defects: `transfer_escrow` was not
excluded from the two-hour full-table policy despite its documented one-time
`pg_repack` strategy, and incomplete-index cleanup ran only after the entire
table rotation. A timed-out rebuild or later task cancellation could strand
its artifacts; the next attempt then created another numbered sibling before
cleanup was ever reached. This is not evidence for a PostgreSQL/PgBouncer
restart, a larger service pool, or a PostgreSQL 18.6 correctness workaround.

Taskworker lifecycle evidence separately showed one durable `DbMaintenance`
task and epoch moving through sequential lease-recovery attempts separated by
roughly five minutes. No attempts overlapped, so the existing session advisory
guard did its job; adding another singleton lock would not fix this incident.
The oversized database operation stalled ordinary work long enough to lose an
evaluation, lease recovery retried the same epoch, and each retry encountered
or created numbered debris under the old cleanup ordering.

The later `contract_close` phase isolated why those otherwise-exclusive
evaluations were lost. Task `<redacted-id>` completed a
2,023.98-second `transfer_escrow` rebuild on edge-1 at 13:30:05Z, started
`contract_close` at 13:30:18Z, and emitted its last ten-second heartbeat at
13:33:06Z without any terminal result. The same task then emitted its first
heartbeat on edge-4 at 13:38:08Z and on edge-0 at 13:45:59Z, again without a
terminal result from either prior owner. Direct PostgreSQL progress showed a
new `REINDEX TABLE CONCURRENTLY contract_close` during each ownership window.
The roughly five-minute handoffs matched the timestamp lease; the absence of
overlap matched the session advisory lock.

The production boundary was `task/task.go`'s heartbeat. It first pinged the
direct PostgreSQL session that carries the advisory ownership lock, then wrote
`claim_time`/`release_time` through the ordinary PgBouncer transaction path.
When the reindex-induced pool stall made that second write hit its 30-second
bound, `server.Tx` raised the error. The panic escaped `EvalTasks`, its deferred
guard release closed the healthy ownership session and canceled the maintenance
context, and the still-pending row became eligible after its last successful
five-minute timestamp. The timestamp is not the ownership authority: an
expired timestamp cannot pass `pg_try_advisory_lock` while the direct session
is alive. Current taskworkers therefore treat a pooled timestamp-refresh
failure as visible, nonfatal recovery-metadata loss after a successful direct
ping; loss of the direct session remains fatal. Metric
`urnetwork_task_timestamp_lease_refresh_errors_total` and the paired log retain
the degraded heartbeat signal. A barrier-driven regression injects the same
pooled panic, proves the live task does not terminate or reschedule, then lets
it finish normally; the existing owner-session test proves an expired timestamp
still cannot create a second owner.

The adjacent table policy was also incomplete. At 13:48Z `contract_close` was
355,384,926,208 bytes (2.10 billion estimated live rows, only 173,427 dead), and
`transfer_escrow_sweep` was 107,269,292,032 bytes (271 million live rows, only
six dead). Full-table rebuilds of those large, autovacuum-healthy tables add
hundreds of gigabytes of scan/write work without addressing measured churn.
They now join `transfer_contract` and `transfer_escrow` in the recurring
whole-table exclusion; tuned autovacuum and explicitly scheduled one-time
`pg_repack`/targeted maintenance remain their supported strategies. The
synthetic policy test covers all four large tables and proves an ordinary table
remains in rotation.

The software closure spans `db_maintenance.go` and `task/task.go`: exclude the
four large contract/escrow tables from full-table reindex, retain supported
targeted and one-time operational strategies, execute cleanup-before → rebuild
→ cleanup-after for each selected table or priority index, and keep live work
owned when only its pooled timestamp refresh fails. A failed prerequisite
cleanup prevents the rebuild; a failed rebuild still reaches its immediate
post-cleanup. The cleanup query schema-qualifies identifiers, includes TOAST,
and refuses to touch a relation represented by live progress. Synthetic
regressions enforce the ordering, failure branches, complete large-table
exclusion, heartbeat ownership boundary, and this probe's active-build
distinction.

Do not interrupt a protected in-progress rebuild merely to apply the source
fix. Compare every active Taskworker artifact with current-main commits
`908a8b2c` and `d8392c83`; deploy only blocks that predate either fix so future
daily maintenance uses the new policy. When every block already contains both,
do not redeploy Taskworker merely because debris remains. Existing debris is a
separate operational database mutation: after the protected operation finishes,
obtain explicit maintenance authorization and run the supported cleanup-only
full cycle:

```sh
bringyourctl db maintenance all --cleanup
```

Do not wildcard-drop `_ccnew` indexes by hand. Closure requires zero inactive
suffixed artifacts, no later full-table `transfer_escrow` progress row, and one
complete post-deploy maintenance cycle with no recurring DataFileExtend/WAL
cluster or Connect login-timeout wave.

The 2026-09-02 deployed-artifact control reached that software boundary. All
eight current Taskworker processes started at 15:01Z and exported modified base
`2d6f27c`; repository ancestry proves that base contains both `908a8b2c` and
`d8392c83`. The same-snapshot catalog had no active table owner but retained at
least 344 inactive artifacts across 27 owners, 277 write-ready, totaling about
347.9 GiB. Redeploying the same fix cannot delete that residue. The remaining
closure is the explicitly authorized cleanup-only cycle followed by a
zero-artifact catalog and a later maintenance cycle that creates no new debris.

The 2026-09-03 `location_group` rebuild supplied a different old-snapshot
control. It had exactly one blocker: an active, non-waiting
`client_reliability_running` transaction holding a snapshot. The shared phase
classifier identified `rolling-leave`, with a 6,172-second transaction and a
3,180-second current statement, while the sibling UpdateReliabilities
diagnostic proved current versioned window markers, a ready covering family,
and a still-eligible legacy non-covering family. This directly falsifies the
older task-canary wording that called every reliability blocker a re-anchor and
unconditionally prescribed the already-deployed four-hour cadence/checkpoint
fix. The maintenance observation now exports only blocker count, structural
phase, state, bounded ages/waits, and snapshot ownership; it does not render
backend PIDs or SQL text. Preserve both operations. Deploy server `fcb4de54`
only to Taskworkers that lack its transaction-local hard-loss timeout, then,
after the protected work ends and explicit DBA authorization is granted, use
`bringyourctl model upgrade-client-reliability-index` to remove the legacy
family. Recovery requires the reindex to advance after snapshot release, not a
restart or a repeated rebuild.

The same control supplied the bounded terminal sequence. When the rolling
blocker reached the exact 7,200-second deadline, `location_group` released and
DbMaintenance immediately advanced to `provide_key`, where its first direct
sample had scanned 2,804,136 of 2,997,067 blocks. A later sample reached
2,997,383 of 2,997,587 blocks before waiting for the successor's legitimate
lookback-1000 `full-anchor-insert`. This is progress plus a new bounded
snapshot dependency, not a reason to restart or repeat either operation. A
maintenance alert may classify the structural blocker phase, but it must use
the sibling target/marker diagnostic to explain why a full anchor exists and
must never infer the predecessor's phase from a cleanup error.

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

At 06:16Z, one `ExportStats` run completed in 187s versus its 87.6s trailing
p95. The exact finished interval overlapped an 870-second
`CloseExpiredContracts` checkpoint for all 187 seconds and a 317-second
legacy-ANY `ReconcileNetEscrow` run for 121 seconds; ordinary adjacent exports
completed in 71–88s, with one earlier 201s shared-load outlier. ExportStats
performs four read-heavy 90-day aggregates through ReplicaDb, which still
resolves to the primary. The overlap makes the already-proven close/NetEscrow
work the leading load owner, but it does not prove a SQL wait edge. The alert
now retains a bounded finished-task overlap snapshot, preserves the hourly
cadence and ten-minute export bound, and directs remediation to the dedicated
owner signals. Require consecutive exports to return toward baseline after
those taskworker fixes; do not rewrite or disable a successful public-stats
export from one correlated duration.

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
minutes). The same durable attempt retried 14 seconds later; within the next
minute, fresh successor attempts appeared about every 20–30 seconds, confirming
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
the same durable attempt was reclaimed seconds later and emitted a fresh 10s heartbeat
at 10:29:33Z. At the surrounding sample the open set was 436,567
(371,408 older than five minutes and 36,769 older than 30), three legacy
retention writers overlapped, and the successor autovacuum was still scanning
the heap. This is the deployed 100k checkpoint failing under recurring shared
write pressure exactly as diagnosed—not a reason to raise the deadline or
restart PostgreSQL. The deterministic 25k cohort remains the fix.

A later deployed cohort reproduced the full chain while cleanup debt was still
present. Task `<redacted-id>` reached
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
Task `<redacted-id>` reached
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
`<redacted-id>` reached
`eval error(1800.86s) ... = Timeout` at 13:17:30Z. Its same-id retry ran from
13:17:36Z to 13:24:14Z and committed successfully in 397.89s; the historical
`Timeout` remains on that finished row, while a distinct successor attempt proves
the recurring chain advanced. This is durable retry progress, not proof that
the deployed 100k checkpoint is safe: the first attempt still spent its whole
deadline beside vacuum/reliability work. Keep the 25k source checkpoint and
follow the new successor plus aged open buckets.

That successor supplied the next exact boundary. Task
`<redacted-id>` ran from 13:24:18Z until
`eval error(1800.72s) ... = Timeout` at 13:54:19Z while the successor vacuum,
an 18-minute net-escrow reconcile, and a 3,356,615-row legacy payment-retention
update overlapped. Its same-id retry committed in 24.84s. Eight immediately
following full cohorts then committed in 18.89–24.44s through 13:57:52Z, with
new successor attempts proving the chain advanced. This is the strongest form of the
checkpoint discriminator: the database and per-contract writes were not
wedged, because the exact same remaining work and its successors drained
quickly after the task-level rollback. The 30-minute first attempt is still a
production failure. Retain the 25k source checkpoint and bounded retention
queue; do not convert the fast retry into evidence for a larger deadline.

The following cohort supplied a tighter same-executor coupling to net-escrow
write amplification. Task `<redacted-id>` ran from
13:57:54Z to 14:20:41Z (1,367.15s) on
`by-us-fmt-5-edge-3/g2`, container `4cf91fd25a2e`. The same executor was
simultaneously applying a 1,021.01s legacy `ReconcileNetEscrow` pass from
14:03:23Z to 14:20:24Z. The close cohort committed only 16.61s after that
fleet-wide Redis writer stopped; its next three full successors then completed
in 21.35s, 27.30s, and 22.29s. No task timeout was required to expose the
boundary, but executor overlap alone was not the causal discriminator. The
following close task `<redacted-id>` began on the same
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
`<redacted-id>` moved to edge-0/g1 and committed in
21.89s. The following full successor
`<redacted-id>` landed back on edge-3/g2 and exceeded
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
`eval error(1801.06s) ... = Timeout` at 15:23:04Z. The same attempt retried on
edge-3/g1 container `786ae804bb97` and committed in 22.450s; its authoritative
`finished_task` row retained the prior `Timeout`. The next full successor
`<redacted-id>` returned immediately to edge-3/g2.
Moving only between g2 and g1 on the same physical host reproduces the slow/
fast split without changing host memory, network, PostgreSQL, or Redis. The
process-level allocated-heap evidence in §2.12 is therefore the local discriminator,
while the 25k close cohort remains the durable checkpoint fix.

The next independent edge-1/g2 cohort supplied the deadline/reclaim sequence
again. Task `<redacted-id>` started at 15:31:31Z,
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
never becomes a finished duration because the same pending attempt is reclaimed.
Retain the latest overrun for 45 minutes so an immediate fast successor cannot
erase its precursor.

- HEALTHY: full deployed legacy cohorts normally finish in roughly 20–30s.
- WARN: a live or completed checkpoint reaches 120s. Internally correlate its
  identifier, then include `attempt_correlated=true` plus the live heartbeat's
  host/generation/container when present; never render the identifier.
- DEADLINE: 1,800s is failure even when the per-contract commits survived.
  The retry repeats discovery because the task-level checkpoint did not
  commit; do not treat those durable child writes as task success.

The 14:21:56Z task `<redacted-id>` demonstrated the
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
one, the same completed attempt to suppress its lingering heartbeat, and a
different active successor to remain visible. They also preserve the exact
1,800.83s rescheduled timeout when a short same-id retry follows it and the only
`finished_task` overrun belongs to an older checkpoint. A completed-retry case
places an even newer successor completion and heartbeat after that retry; the
query must still retain the terminal attempt's own row, and executor attribution
must come from that exact attempt's heartbeat rather than the newest function
heartbeat. When a different successor crosses 120s, its active alert retains
the precursor failure's duration, correlation marker, error, timestamp, and executor
identity rather than letting new activity erase the deadline incident.
An exact internally correlated completed attempt is authoritative for the full 45-minute incident
window; the two-minute completion-age bound applies only to the legacy
unlabelled duration fallback. Live task
`<redacted-id>` exposed that distinction: the monitor
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
fallback to retain the latter attempt's duration and host/generation/container
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

Task `<redacted-id>` supplied the sharpest executor
control at 16:52Z. Edge-1/g1 container `53ef545dc646` logged
`eval error(1800.88s) (reschedule) ... = Timeout`; the monitor immediately
rendered `phase=failed`, an attempt-correlation marker, error, timestamp, and executor.
The same id was reclaimed on edge-4/g2 container `3c0a752d4433` and its
authoritative row ran from 16:52:36.353384Z to 16:53:00.255024Z—23.902s—while
retaining `reschedule_error=Timeout`. That 75x same-task duration split proves
the deployed 100k checkpoint is load-sensitive and proves why a fast retry
must not erase its failed precursor. It validates the monitor lifecycle rule
and the 25k source checkpoint; it is not a reason to raise 1,800 seconds.

Its later scheduled successor made the local amplifier visible without another
timeout. Task `<redacted-id>` landed back on the
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
executor. Task `<redacted-id>` shared edge-0/g1 with
the next score export, reached `eval error(1800.90s) ... = Timeout` at
17:48:27.076631Z, and let the open set rise to 567,403. The same id was
reclaimed on edge-3/g2 14 seconds later and its authoritative row ran from
17:48:31.198071Z to 17:48:54.551346Z—23.353s—while retaining
`reschedule_error=Timeout`. That roughly 77x split reproduces the failed
precursor/fast retry lifecycle and local allocator amplification independently
of edge-1. Keep the 25k checkpoint; do not normalize the rising historical p95
or raise the deadline.

Its full successor repeated the boundary on the same score-heavy executor.
Task `<redacted-id>` reached
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
Task `<redacted-id>` reached
`eval error(1801.43s) ... = Timeout` at 18:51:54.281835Z on the same
edge-0/g1 container. The same id was reclaimed on edge-3/g1, and its
authoritative row ran from 18:51:59.461170Z to 18:52:21.335651Z—21.874s—
while retaining `reschedule_error=Timeout`. This approximately 82x same-task
split is a fourth independent reproduction of the oversized 100k checkpoint
plus the hot-process amplifier. It is pre-rollout evidence: require new
generations to complete recurring 25k checkpoints below 120s before clearing
the incident.

The next old-generation checkpoint supplied the non-timeout side of the same
boundary. Task `<redacted-id>` remained on the
edge-0/g1 hot process and `finished_task` recorded a successful 1,568-second
run. It was still only 232 seconds below the hard deadline and roughly 65x the
healthy 20–30-second band. A successful terminal row therefore does not clear
the latency incident or weaken the 25k checkpoint fix; it shows the amplifier
is continuous near the deadline rather than a binary timeout-only failure.

The last observed old-generation checkpoint repeated the failed/fast-peer
control. Task `<redacted-id>` reached
`eval error(1801.41s) ... = Timeout` at 19:51:23.925745Z on edge-1/g2
container `06abfbe03c32`. The same id was reclaimed on edge-3/g1 container
`786ae804bb97`, emitted 10s and 20s heartbeats, and `finished_task` recorded a
rounded 25-second completion ending at 19:51:53Z. The monitor retains the
failed attempt as the incident while rendering `retry_phase=completed`, retry
duration/time, and the exact retry executor. This live validation also exposed
and fixed two evidence-loss bugs: selecting only the latest completion plus
latest overrun lost a retry after later cohorts completed, and using the newest
function heartbeat mislabeled that retry with a later successor's executor.
The probe now selects the exact active and terminal identifiers internally in
addition to the ranked rows and parses executor identity for the terminal
attempt specifically. The rendered alert omits both identifiers. The
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
  >= 1.5x it. A multi-hour incident can contaminate that control too. On
  2026-09-02 the live rate remained ~13.4k/min against a pre-incident
  ~4.3k/min, but the one-hour and six-hour medians had adapted to 13.8k and
  12.5k. The connection-rate probe now falls through again to a trailing-24h
  median when the selected shorter median is >= 1.5x it. The daily anchor must
  contain at least 120 samples spanning 12 actual hours, so duplicate watcher
  samples cannot manufacture a long baseline. A deterministic six-hour
  contamination fixture preserves the high-side alert and its §2.15 causal frame.
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
- RELIABILITY-WINDOW FEEDBACK VARIANT (2026-09-02): a high rate is not always a
  restart. The probe now samples a mature two-minute `connect_time` cohort
  three to five minutes behind now, retains its still-connected survivors, and
  compares child count, distinct parent/source clients, networks, and
  disconnected p50/p90 with the same-width cohort three hours earlier. It also
  joins the durable §2.15 classification version and complete 12-hour score
  distribution. The incident held roughly the same parent/network population
  while child creation per parent multiplied, p50 fell from 70.3s to 8.8s,
  and the provider score gate admitted 0 of 100,737 rows. A later live sample
  admitted only one extreme outlier of 101,225 while apps still failed to find
  usable providers. Fewer than one passing row per 1,000 scored rows is therefore
  the conservative effective-empty boundary; one outlier must not erase the
  causal frame. Frame
  `reliability-window-churn` therefore identifies a provider-window feedback
  loop; do not call it a fleet restart or organic account growth. Apply and
  verify the §2.15 root fix, then require destination diversity and two mature
  cohorts to recover. Raw client/network ids never leave PostgreSQL.

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
  It interrupted task `<redacted-id>` after a fresh
  4,096s heartbeat. Normal scheduler reclamation restarted that exact id at
  02:15:01Z; by 02:39 it had advanced another 1,458s while the completion gap
  crossed 98 minutes. This is not a stuck lease: the same-id heartbeat proves
  recovery. It is lost in-process scan progress, because the full-fleet export
  still restarts from its scheduler boundary. `selection-freshness` now
  reads the bounded task lifecycle window (with host-journal fallback), emits
  an attempt-correlation marker, duration, and executor with the gap, and tells the operator not to restart
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
  the freshness cliff. Task `<redacted-id>` began on
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
successfully, refresh every ttl, and publish either an empty provider market or
a large market made from the wrong lifecycle class. Check the database supply,
the writer-generation marker, and the exported cache as separate stages:
```sql
-- Arm the hard-integrity fields only when this returns true. The Go probe
-- executes the population query without either direct column reference while
-- the append-only migration is pending.
SELECT EXISTS (
  SELECT 1 FROM pg_attribute
  WHERE attrelid = 'provider_egress_health'::regclass
    AND attname = 'tls_authentication_failure'
    AND NOT attisdropped
) AS tls_integrity_armed;

-- Once armed:
WITH supply AS MATERIALIZED (
  SELECT nc.active, nc.source_client_id
  FROM network_client_location_reliability nclr
  JOIN network_client nc USING (client_id)
  WHERE nclr.connected AND nclr.valid
    AND EXISTS (
      SELECT 1 FROM provide_key pk
      WHERE pk.client_id = nclr.client_id
        AND pk.provide_mode IN (1, 3) -- Network or Public
    )
)
SELECT
  (SELECT count(*) FROM supply) AS raw_score_candidates,
  (SELECT count(*) FROM supply
   WHERE active AND source_client_id IS NULL) AS eligible_score_candidates,
  (SELECT count(*) FROM supply
   WHERE source_client_id IS NOT NULL) AS derived_candidates,
  (SELECT count(*) FROM supply
   WHERE NOT active AND source_client_id IS NULL) AS inactive_top_level_candidates,
  (SELECT count(*) FROM provider_egress_health) AS egress_health_rows,
  (SELECT count(*) FROM provider_egress_health peh
   WHERE measured_at >= now() - interval '24 hours'
     AND total_count > 0 AND 10 * ok_count >= 9 * total_count
     AND NOT peh.tls_authentication_failure
  ) AS fresh_passing_health_rows,
  (SELECT count(*) FROM provider_egress_location) AS egress_location_rows,
  (SELECT count(*) FROM provider_egress_health peh
   WHERE peh.tls_authentication_failure
  ) AS tls_authentication_failures;
```
The raw count is diagnostic only. The score writer must join `network_client`
and admit only `active = true AND source_client_id IS NULL`. A derived window
identity is a consumer-side child of a durable source client, not independent
provider capacity. Feeding those short-lived identities back into destination
selection makes replacement churn look like supply and can amplify the churn.
An inactive top-level client is likewise not live supply even when a legacy
location row remains connected.

After a completely successful filtered export, the current writer publishes
`client_score_provider_eligibility_v1_ready=1` in Redis. Absence of that marker
while derived or inactive candidates exist is `provider-supply-ineligible`.
Never set it manually: it is a completion boundary, not a feature flag. Raw
historical rows may remain after repair, so their existence is harmless once
all Taskworkers have converged and a current writer has published the marker.
During a partial rollout, an old writer can still replace score caches after a
new writer sets the marker; deployment provenance must therefore be healthy as
well.

Then inspect the SAME target/caller pair in the normal and ForceMinimum score
caches. Resolve a country target first; `<redacted-id>`
is the no-caller-location key:
```sql
SELECT location_id FROM location
WHERE location_type = 'country' AND country_code = 'us';
```
```bash
target=<uuid-from-sql>
caller=<redacted-id>
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

- HEALTHY: normal decoded sum is nonzero and tracks the eligible active
  top-level supply band;
  ForceMinimum is normally larger because it bypasses reliability/score gates.
- INELIGIBLE SUPPLY: the ready marker is absent and the raw pool contains
  derived or inactive clients. Deploy the filtered writer and let the existing
  serialized score task finish. Do not delete client, location, provide-key, or
  cache state manually.
- GATE WIPE: connected/eligible are large, normal sum is 0, ForceMinimum is
  large. Provider connectivity is healthy; a minimum predicate ate the market.
  Split the predicates: reliability lookbacks, score cutoff, then egress health.
- TLS-INTEGRITY DISCRIMINATOR: `fresh_passing_health_rows` excludes a provider
  whose otherwise-passing ratio contains a hard certificate-authentication
  failure. `tls_authentication_failures` is aggregate-only evidence; never put
  provider IDs in the alert. The Go probe first checks `pg_attribute`; before
  the append-only column exists it omits both direct column references, so the
  current database remains observable without serializing every wide health row
  through JSON. Until `tls_integrity_armed=true`, §2.10 owns the pending
  migration and this count is unarmed rather than proof of zero failures. Once
  armed, PostgreSQL can use the partial boolean index, a positive row remains
  excluded even after its 24-hour score sample ages out, and only a later clean
  authenticated run may replace and clear it. Do not weaken the hard gate or
  average the failure into the 90% score. API must persist the bit and the
  Taskworker/operator-proxy probe must classify, submit, and enforce it; apply
  the migration before deploying either schema-dependent artifact. This is a
  software integrity boundary plus provider/network remediation, not a
  hardware-capacity alert.
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

2026-09-02 signature: the live raw Network/Public score pool held 390,110
connected/valid candidates, but only 90,298 were active top-level clients.
297,776 were derived identities and another 2,036 were inactive top-level
clients. The same incident contained 64,194 mature open connection rows with no
handler (§2.16). The source fix joins the durable lifecycle in every location
and location-group score query; the completion marker makes that semantic
change observable. This is a **software root cause**. Deploy every Taskworker
from a descendant of Server commit `b7599962` that contains the marker, allow
one post-convergence `UpdateClientScores` run to complete, and require the marker,
bounded cache inspection, destination diversity, and child-churn recovery. It
does not require hardware and must not be repaired with manual data deletion.

Implementation convention: SIGNALS.md §2.9 (`selection-population`) maps to
`signal_selection_population.go` and
`signal_selection_population_test.go`. Synthetic tests cover a fresh empty
normal cache, a legacy export with derived/inactive supply, and harmless raw
residue after a completed filtered export.

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
  fleet medians. Internally join the newest active attempt identifiers and
  durations from that host/block's taskworker heartbeats, but render only task
  families and elapsed times. Ignore a sample older than 90s so a
  drained generation cannot create a false skew. The rate query is
  best-effort: an unavailable range evaluation must not hide a decisive heap
  outlier, but its absence is recorded in the evidence.
- This is process-local evidence. `HeapAlloc` includes reachable objects and
  objects not yet reclaimed by the next GC; unlike RSS, it excludes retained
  pages with no allocated objects. Host free memory, CPU count, cgroup limits,
  and pressure remain necessary controls, but they do not clear a worker whose
  allocated heap is hundreds of times its peers. Internally correlate exact
  taskworker `eval active` attempts on that executor, then compare those task
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
`<redacted-id>` completed at 15:30:03Z in
4,143.294s, but its allocated heap stayed near 25GiB until a later GC; the
co-resident reaper completed at 15:32:16Z in 7,932.960s in the same scrape
where heap and RSS contracted. That coincidence does not assign those bytes
to the reaper: `HeapAlloc` can retain unreachable objects between collections.
The independent reproduction did assign the allocator. Edge-1/g2 began score
task `<redacted-id>` at 15:30:34Z: allocated heap rose
from 0.91GiB at 15:30:30Z to 5.55GiB at 15:31:00Z and 8.74GiB at 15:31:30Z,
before its later close and reliability-rollup tasks began, then reached
13.94GiB at 15:32:00Z. That clean start-aligned reproduction validates the
score-export working-set fix independently of the edge-3 task mixture. A
simultaneous negative control separated the reaper: task
`<redacted-id>` ran past 460s on edge-4/g1 while that
process stayed between 0.07GiB and 0.16GiB allocated heap and near 0.50GiB RSS.
The reaper's old serialized Redis path explains its wall-clock tail, but not
the multi-GiB allocation pattern. The probe now performs this correlation in
its alert by joining the outlier's exact host/block to recent taskworker
heartbeats and listing active task families plus elapsed times. A timestamped
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
`<redacted-id>` completed at 16:18:55Z in
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
`<redacted-id>` began on edge-1/g1 container
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
`<redacted-id>` began on edge-0/g1 and reached
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
the alert names the already-proven caller-fanout allocator and uses the durable
alias marker to distinguish a pending target-oriented fix from residual
post-deploy allocation; a co-resident close task adds the process-budget impact
and close/backlog verification. A deterministic synthetic regression
forces the gateway failure, normalizes a timestamped journal heartbeat, and
requires the exact identifier to remain absent while `host-journal-fallback`
appears in Markdown. The first v114 live result attached four active task
families and elapsed times, including the
4,357-second score export and 747-second close checkpoint, but also labeled
`crisp` and `fireside` as partial-read failures. Direct checks showed zero
running taskworker units on both hosts and an exact `journalctl --grep` status
of 1, while an active edge returned status 0. Status 1 is journald's normal
no-match result, not a transport failure. The bounded command now converts
only that status to an empty successful host observation and preserves every
other nonzero status as an error. A mixed active/empty-host synthetic test
locks the shell-status guard and requires the active host's normalized task to
survive without false degradation. The v115 live result at 05:49Z then
attached the 4,668-second score export and its 185-second successor close
checkpoint from `host-journal-fallback` with no degraded-read evidence,
validating the production no-match path.

The 06:12Z recurrence exposed a different partial-record boundary. Both worker
alerts attached the exact edge-3/g1 score, close, net-escrow, and reliability
tasks, but marked edge-1 degraded because one JSON journal record had no
`MESSAGE`. The raw record was the 1,120-second Payout terminal error: its host,
container, tag, and timestamp were intact, while `journalctl -o json` rendered
the oversized message as null unless `--all` was requested. Enabling `--all`
would remove the fallback's byte bound when stack traces are large. Journal
normalization now skips and reports only the unavailable record, retains every
other identity-checked row from that host, and never passes partial raw JSON to
the lifecycle parsers. A synthetic mixed valid/null journal requires the
active score row and host-journal source to survive while the returned partial
error names the unavailable message.

The alias rollout supplied the next production boundary. The writer published
`client_score_alias_v1_ready=1` at 08:33:49Z on 2026-08-31. On the formerly
fullest Redis node, used memory then fell from 12.35GB (95.8%) to 6.13GB
(47.6%) before the five-hour compatibility TTL ended; sampled score values
fell from 91.3% to 3.0% of bytes. Immediately after the final legacy expiry at
13:33:49Z, the marker was still present, the fullest node was 5.66GB (44.0%),
and the deployed API and Connect readers emitted no selection-empty, Redis
timeout, exchange-failure, panic, or fatal evidence. This closes the
caller-fanout storage/read-path fix without a production delete.

The sparse writer also changed the executor shape without eliminating every
large transient. A post-marker score pass finished in approximately 417
seconds rather than the pre-fix 3,785-second baseline. Its exact executor
peaked at 9.31GiB allocated heap, fell to 4.30GiB as the pass ended, and then
to 2.87GiB at the next collection. A cold successor on another executor
peaked at 4.05GiB in its first three minutes. A repeated threshold crossing is
still actionable, but it is no longer evidence that target-oriented fanout is
missing: live maps and encodings from the current pass can overlap objects
from a prior pass awaiting collection. `worker-memory` now reads the durable
marker when it attributes an outlier to `UpdateClientScores`. Marker absent
keeps the deployment action; marker present says not to redeploy, retains the
heap alert, and requires a terminal/post-collection observation before
profiling and bounding the remaining provider-map or encoding concurrency. A
synthetic marker-ready outlier locks that distinction and rejects the stale
pre-deploy action.

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
- Join recent `eval active` heartbeats on exact host/block. Use identifiers only
  for internal correlation; include task family, duration, runtime instance,
  fleet sample count, both rates, medians, and
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
`UpdateClientScores` id `<redacted-id>`; it remained
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

The next exact terminal boundary tied the churn to close latency without a
restart or database wait. Score task
`<redacted-id>` ran from 04:32:00Z to 05:52:41Z for
4,841 seconds. Its same-process close task
`<redacted-id>` ran from 05:46:38Z to 05:52:44Z for
365 seconds and ended only three seconds after the score export. The score
log had printed `export client location[1008/1008]` at 05:49:12Z, but source
inspection shows that atomic counter advances before a caller export begins;
it counts started units and is not a completion boundary. The scheduler
created the next score task about 39 seconds after the finished row, and at
05:54Z the same executor was again near four cores and 644MiB/s with a new
close task beside it. This is nearly continuous caller-fanout pressure, not a
quiescent post-completion heap. Require the target-oriented/alias deployment
to break both the per-run duration and the repeated close overlap; do not use
the start counter, a brief task-row handoff, or the absence of a restart as a
recovery claim.

The successor made that overlap repeatable rather than coincidental. Score
task `<redacted-id>` began at approximately
05:53:14Z on the same edge-3/g1 process and was still active beyond 2,040s.
While it ran continuously, close task
`<redacted-id>` completed its 25,000-contract
checkpoint in 920s (05:53:58Z–06:09:18Z), its immediate successor
`<redacted-id>` completed in 870s
(06:09:29Z–06:23:59Z), and third task
`<redacted-id>` completed in 889s
(06:24:11Z–06:39:00Z). A fourth close began about 12 seconds later on the same
process. During the third overlap the exact worker consumed 4.003 cores and
allocated 651MiB/s; PostgreSQL migration, wait-event, active-query, and
transaction-age probes were clean. Three sequential 14–15-minute close
checkpoints under one uninterrupted score export strengthen the shared
process-budget cause. Keep the 25,000 cap and deploy the score fanout fix;
additional close workers would add contention without removing the allocator.

Implementation convention: SIGNALS.md §2.12a (`worker-churn`) maps to
`signal_worker_churn.go` and `signal_worker_churn_test.go`.

### 2.13 Maintenance reboot collision — process exit is not task completion
Probe: `reboot-collision`

An orderly host reboot can still interrupt scheduler-level work. For 20 minutes
after each services-host boot, read the previous boot's final journal boundary,
the `by-restart.service` evidence, and bounded g1/g2 taskworker lifecycle logs.
Treat a task as colliding only when its newest `eval active` heartbeat was
within 45 seconds of shutdown, had already reached 120 seconds, and no newer
terminal line exists. Internally cross-check its exact identifier against
`finished_task` through the previous-boot boundary so a task that completed between its last
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
`CloseExpiredContracts` task `<redacted-id>` still had
a fresh 541-second heartbeat and `UpdateClientScores` task
`<redacted-id>` had run for roughly 59 minutes. The host
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

### 2.14 Circle transfer admission — fleet ceiling vs random retry dispersion
Probe: `circle-admission`

Circle's [Wallets API rate-limit
documentation](https://developers.circle.com/api-reference/wallets/rate-limits)
currently specifies 20 GET requests/second, five default POST requests/second,
selected POST exceptions at ten/second, and HTTP 429 after a limit is exceeded.
`POST /v1/w3s/developer/transactions/transfer` is not listed as an exception.
The 2026-09-01 post-jitter control in §1.2 matched that default boundary: five
wallet rejection responses and a sixth 429 landed in one source second even
though four of the five responses came from executables already proven to
contain proportional retry jitter. Random scheduling is useful load
dispersion, but it is not admission control.

Current-main server commit `14928f69` (the patch-identical replay of former
commit `eb7e79b6`) puts one fail-closed, fleet-wide gate immediately
before the transfer POST. Redis server time eliminates host-clock skew. One
atomic sorted-set script admits no more than three unique transfer calls in a
rolling second, leaving two requests/second of headroom for other callers. A
stable per-call member makes Redis command replay idempotent. A waiter retains
the payment's durable Circle idempotency key, and a Redis/context error returns
before HTTP rather than guessing that an ambiguous financial submit is safe.
Current-main descendant `66525afc` also converts the Redis wrapper's
pre-command connection panic into the same error/counter/log path; use that
descendant as the minimum observable deployment baseline. Stable patch IDs
prove it is patch-identical to the former `b8718420` hash after main was
rewritten.

The Taskworker exports these process metrics:

- `urnetwork_circle_transfer_admissions_total`
- `urnetwork_circle_transfer_deferrals_total`
- `urnetwork_circle_transfer_admission_errors_total`
- `urnetwork_circle_transfer_admission_wait_seconds_count`
- `urnetwork_circle_transfer_admission_wait_seconds_sum`

The probe selects the newest actual-scrape-fresh process for each host/block,
so an old draining generation cannot supply a replacement's missing collector.
It evaluates five-minute counter increases per exact process.

- HEALTHY: every newest Taskworker exposes all five families; admission errors
  are zero; fleet and per-process mean completed wait are at most five seconds.
  Deferrals may be non-zero—they prove the gate prevented an unsafe burst.
- WARN `circle-transfer-admission-unobservable`: a newest process lacks any
  family. Deploy a clean Taskworker artifact containing `66525afc` only to the
  missing blocks, using §8.12 source/digest provenance rather than a mutable
  config version.
- WARN `circle-transfer-admission-error`: the gate failed closed. Correlate the
  exact window with taskworker drain state, Redis liveness/latency, and the
  privacy-safe admission failure line. Never bypass the gate or manually replay
  a payment.
- WARN `circle-transfer-admission-pressure`: fleet or one process averages more
  than five seconds of completed admission wait across two probes. Keep the
  safety ceiling. If `payout-wallet-insufficient` is active, this is partly an
  **operations/finance** boundary: fund or pause that wallet because software
  cannot create liquidity. For legitimate sustained payout growth, obtain the
  account's authoritative Circle quota before changing code or thresholds.
- VERIFY: §8.12 proves every newest Taskworker contains `66525afc`; all metric
  families are present for two scrapes; admission errors and Circle 429s stay
  zero; canonical wallet attempts stay below four/second; and payment
  idempotency keys remain stable for one full 90-minute retry window.

The 2026-09-02 post-deployment control closed the software branch. All eight
newest fresh Taskworker processes ran version
`2026.9.1-outerwerld+1034926970` from one clean source revision
`fe3fa8eea625a3935ec7fe6569ee83b8a2578143` and immutable image digest. Git
ancestry proves that revision contains typed-reset `b8af229f`, proportional
jitter `70b0d269`, and the complete `66525afc` admission baseline. The
dedicated probe found all five collectors healthy. Across the full 90-minute
control, 1,244 exact task-evaluator wallet-insufficient attempts occupied
1,037 source seconds, peaked at exactly three attempts in one second, and
produced zero admission-failed lines and zero Circle 429s. That is positive
evidence that the fleet ceiling works under the live backlog. Continued
wallet-insufficient or invalid-destination rows after this boundary are the
separate finance and account-configuration actions in §1.2; do not prescribe
another Taskworker deployment for them.

Implementation convention: SIGNALS.md §2.14 (`circle-admission`) maps to
`signal_circle_admission.go` and `signal_circle_admission_test.go`. Synthetic
tests cover a healthy newest generation, a replacement missing two metric
families, fail-closed errors, excessive per-process wait hidden by a lower
fleet mean, and invalid counter data. The product-level Redis synthetic covers
the eight-caller atomic ceiling and replay semantics.

### 2.15 Provider reliability running-sum integrity — immutable degraded blocks
Probe: `reliability-drift`

Provider-selection freshness and a nonempty cache are not sufficient. A
completed `UpdateReliabilities` can publish internally inconsistent weights,
and a completed `UpdateClientScores` can then refresh a catastrophically narrow
but nonempty market. Check all four durable boundaries:

1. `client_reliability_running_window.degraded_classification_version` is 1
   and its current-writer token is nonempty for every lookback;
2. the database classification guard trigger is present and enabled, so a
   legacy writer atomically revokes version-1 trust;
3. the running numerator and score denominator use the same immutable
   per-block degraded classification; and
4. a nonzero established-client population remains at or above the 12-hour
   `independent_reliability_weight >= 0.70` gate.

Version 0 used one median over whichever moving score window asked the
question. That made “is block B degraded?” depend on the later window endpoint.
A full re-anchor could omit B from `client_reliability_running`; hours later,
after sustained lower fleet volume became the new median, the score writer
would count B in its denominator without restoring B to the already-materialized
numerator. Four-hour re-anchors bounded the corruption but did not make rolling
correct.

Version 1 classifies each block against its own 60-block trailing neighborhood,
including itself. Because the reliability rollup records blocks sequentially,
that answer is immutable. Full recompute, entering-block addition,
leaving-block subtraction, and score normalization therefore use the same set.

Migration 602 adds a durable version column with default 0. That column alone
is not a safe mixed-rollout boundary: after a current writer sets version 1, a
legacy writer's old UPSERT can replace the bounds and running sums without
mentioning the new column, accidentally preserving version 1. Migration 603
therefore adds an opaque write token and a database trigger. Every current
writer rotates the token in the same UPSERT as its bounds and version. A legacy
writer leaves the token unchanged, so the trigger atomically resets that row to
version 0. The next current Taskworker must re-anchor it before version 1 can be
trusted again. This also makes an accidental future rollback fail safe. Apply
both migrations through head 603 before deploying the new Taskworkers, converge
the whole Taskworker fleet, and never update the version or token manually.

2026-09-02 production root-cause evidence:

- `UpdateReliabilities` completed at 09:39:27Z. The first score export that
  could read it started at 09:43:07Z; the connection surge began around
  09:44Z. No API, Connect, Proxy, or Taskworker restart occurred at onset.
- The 08:16Z version-0 12-hour re-anchor classified 307 of 721 blocks degraded
  using a whole-window median of 85,955 valid clients. At 10:41Z the moving
  median had adapted to 76,945 and classified only two of the same-width
  current blocks degraded.
- A deterministic sample of 20 clients from the modal score cohort had 716–721
  valid raw rows in the current window, but every materialized running sum was
  exactly 414. This directly proves numerator loss; it is not an inference from
  traffic volume.
- All 100,737 scored clients failed the 12-hour 0.70 gate. The maximum weight
  was 0.6662 and the median was 0.5758. Only 1,037 connected/valid providers
  passed both the reliability and normal quality predicates, while the writer
  could see hundreds of thousands before minimums.
- A mature pre-surge two-minute contract cohort used 8,804 destinations and
  its source connections had p50 70.3s. A matched surge cohort used only 4,393
  destinations despite 2.7x more contracts; p50 connection lifetime fell to
  8.8s and only 108 of 25,998 source connections survived the observation.
  The same roughly 1,100 parent/source clients minted many more window children,
  ruling out new-account growth. Broad geography and healthy auth traffic rule
  out one customer, edge, or login failure.

Alert frames:

- `moving-median-v0`: the durable version is absent/zero. Apply the migration,
  deploy the current Server Taskworker, and let the existing serialized
  `UpdateReliabilities` attempt perform its one-time checkpointed re-anchor.
- `legacy-writer-reset`: the trigger and a prior current-writer token are
  present, but the row is back at version 0. This directly proves that a writer
  which could not rotate the token updated the row during a mixed or rolled-back
  fleet. Finish Taskworker convergence before allowing the existing task to
  re-anchor; do not repair the marker manually.
- `unguarded-version`: a row claims version 1 without both a nonempty write
  token and the enabled guard trigger. The marker cannot establish writer
  provenance. Apply migration 603 and let a current writer re-anchor it.
- `gate-collapse`: version 1 is present but fewer than one in 1,000 scored rows
  pass the 12-hour gate, with the token and trigger present. An isolated
  extreme outlier is not meaningful provider diversity. Treat this as genuine
  reliability-input failure until the raw block, coverage, and fleet-event
  evidence proves otherwise; do not weaken 0.70.

Verify every Taskworker has converged, the guard trigger is enabled, all four
running-window rows reach version 1 with nonempty current-writer tokens, the
re-anchor finishes, and a subsequent `UpdateClientScores` publication restores
a nonzero 12-hour passing population. Then require destination diversity,
child-creation rate, and matched mature connection lifetime to return to their
trailing bands for two probes. Do not delete score rows, edit Redis blobs,
schedule a duplicate task, or restart clients to manufacture recovery.

This is a **software root cause**, not a hardware-capacity alert. Proxy memory
and active-client ceilings remain the separate hardware/operations boundary in
§16.8 and §16.9.

Implementation convention: SIGNALS.md §2.15 (`reliability-drift`) maps to
`signal_reliability_drift.go` and `signal_reliability_drift_test.go`.
Synthetic tests cover the exact version-0 corruption signature, a healthy
guarded version-1 population, a legacy-writer reset, an unguarded marker, and a
post-migration genuine gate collapse. Model tests reproduce an identical-bounds
legacy UPSERT during a mixed rollout, hold one sharp block's classification
stable across a later/longer window, and prove rolling equals full recompute
across a deterministic median flip.

### 2.16 Open connection handler ownership — durable rows need a live owner
Probe: `connection-orphans`

`network_client_connection` is durable history; `network_client_handler` is an
ephemeral lease owned by a live Connect handler. An open connection may briefly
precede or outlive its handler across normal task cadence, but every connection
older than two handler-heartbeat intervals must still join an existing handler.
Aggregate the current open set without exporting identifiers:

```sql
WITH connection_state AS MATERIALIZED (
  SELECT c.connect_time, h.handler_id IS NULL AS orphan
  FROM network_client_connection c
  LEFT JOIN network_client_handler h USING (handler_id)
  WHERE c.connected = true
)
SELECT
  count(*) AS connected_count,
  count(*) FILTER (WHERE orphan) AS orphan_count,
  count(*) FILTER (
    WHERE orphan AND connect_time < now() - interval '2 minutes'
  ) AS mature_orphan_count,
  extract(epoch FROM now() - min(connect_time) FILTER (WHERE orphan))::bigint
    AS oldest_orphan_age_seconds
FROM connection_state;
```

- HEALTHY: mature orphan count is zero. A small younger set can be a normal
  cleanup race and must not page.
- MISSING HANDLER: mature orphans remain on two probes. These rows are not live
  clients even though `connected = true`; they inflate connection and provider
  supply, preserve stale locations, and obscure the real fleet ceiling.

The legacy cleanup first selected expired handler IDs and then closed only
connections carrying one of those IDs. That works while the handler row still
exists. A process loss, earlier handler deletion, or insertion race can remove
the ephemeral row first, leaving its durable connection permanently invisible
to every later cleanup. There is intentionally no foreign key between the two
lifecycle tiers, so this is possible without a database integrity error.

2026-09-02 production evidence: 150,544 rows claimed to be connected; 64,194
had no handler, every orphan was older than two minutes, and the oldest was
38,493,728 seconds (about 445.5 days) old. Only 20 handler rows existed and none
had a stale heartbeat. This is direct state evidence, not an inference from
traffic counters.

This is a **software root cause**. Server commit `b7599962` changes the singleton
Taskworker cleanup into two ordered operations: delete expired handlers, then
sweep every open connection whose handler is absent. The location-reliability
writer also requires a live handler and keeps disconnected fallback rows
disconnected. Deploy every Taskworker from that commit or a descendant, then
let the existing singleton tasks repair state naturally. Do not update
connection rows, delete handlers, clear score caches, or schedule duplicate
tasks manually.

Verification requires all active Taskworkers to have the fixed ancestry, two
consecutive probes with zero mature orphans, a later location-reliability pass
that removes former orphan supply, the §2.9 provider-eligibility completion
marker, and recovery in open-set size, destination diversity, and child churn.
This fix needs no hardware. It does not change the separate proxy active-client
ceiling or the hardware/operations capacity guidance in §16.8 and §16.9.

Implementation convention: SIGNALS.md §2.16 (`connection-orphans`) maps to
`signal_connection_orphans.go` and
`signal_connection_orphans_test.go`. Synthetic tests cover the 445.5-day
legacy leak and a harmless sub-two-minute cleanup race. Model regressions cover
orphan sweeping, handler-qualified location reliability, and disconnected
fallback semantics.

### 2.17 Missing companion-origin contract rate — return path cannot anchor
Probe: `missing-origin`

`urnetwork_connect_contract_failures_total{cause="missing_companion_origin"}`
is the lossless API-side boundary for a contract request that entered companion
settlement but could not find a usable reverse origin after the bounded
server-side race wait. The probe queries only requests whose original wire bit
was `companion=false`:

```promql
sum(rate(urnetwork_connect_contract_failures_total{
  env="main",cause="missing_companion_origin",companion="false"
}[5m])) * 60
```

- HEALTHY: the `companion=false` partition stays at or below 500/min for two
  complete five-minute windows. A six-hour control on 2026-09-02 held mostly
  128–201/min from 10:20Z through 14:50Z, with one short 271/min sample.
- FALLBACK FROM NORMAL: `companion="false"` exceeds 500/min. This label is the
  original request bit, not a trustworthy role label. A non-companion request
  can be converted by `resolveNonCompanionProvideMode` to Stream/companion
  fallback when the destination does not advertise the relationship mode.
  Provider discovery can produce such a destination, but a provider return
  path or same-network peer can also create this request shape. Do not call the
  destination a selected provider from this label alone.
- CONTEXT ONLY: `companion="true"` has a materially higher, workload-dependent
  background (about 2,800–3,100/min during the 2026-09-02 incident). The shared
  Grafana 500/min rule is not calibrated for that partition, so this probe does
  not page on it. Establish a separate healthy band before adding that signal.
- UNKNOWN: the aggregate `companion=false` series is absent, stale, duplicated,
  negative, NaN, or malformed. Counter-vector label families are
  traffic-created, so an empty Mimir response cannot be interpreted as zero.

Require §2.8 score freshness, §2.9 active top-level eligibility, §2.15 guarded
reliability, and §2.16 zero mature orphans. Decode bounded current score-cache
samples and verify normal entries are active, contractable Public providers,
but treat that as a control rather than proof about every failing request. Then
compare the onset with score publications, service rollout/drain boundaries,
connection churn, successful contract creation, and the client-window lifetime.
Windows selected before a repaired publication must age out naturally. If the
rate survives the last known legacy cohort, add or use bounded metric dimensions
for source lifecycle, destination lifecycle, relationship, and resolution path;
those categories can distinguish selection from return traffic without raw
client pairs. Do not log identifiers, edit Redis blobs, weaken provider gates,
restart clients, or increase the companion wait merely to hide the rate.

2026-09-02 production evidence showed why this must be a first-class monitor
signal rather than only a dashboard rule. After the eligibility export, the
normal US cache held 32,390 providers (ForceMinimum 51,109), its sampled TTLs
were near the five-hour maximum, and a decoded normal sample contained 200/200
Public providers passing the quality minimum. PostgreSQL had 75,742 eligible
active top-level location rows, zero mature connection orphans, and 73,769 of
107,524 scored clients passing the 12-hour 0.70 gate. API and Connect then
converged on their current artifacts, the last legacy Connect drain ended, and
legacy 4,096-byte framer rejects fell to zero in a three-minute window.
The `companion=false` rate stepped from 175/min at 14:50Z to 461/min at 14:55Z
and 1,232/min at 15:00Z. That boundary coincided with the legacy
UpdateClientScores run that ended at 14:55:06Z; the eligibility-filtered
Taskworker did not start until 15:01Z. Current API, Connect, Proxy, and
Taskworker generations subsequently converged, and repeated filtered score
runs completed. At 16:17Z, a 20-bucket Redis sample contained 3,978/3,978
active, reliable, connected top-level Public clients and zero Stream-only
clients, yet the rate was still about 958/min while PostgreSQL recorded about
9,753 successful contracts/min over the same five-minute wall-clock interval.
The current cache therefore disproves simple ongoing contamination in that
sample, but cannot identify the failing endpoint role. The last known legacy
list can remain inside a client window for 45–60 minutes (and some draining
flow-carrying channels have a two-lifetime hard bound); continue observation,
then require the bounded lifecycle dimensions above if the rate persists.

The two-lifetime explanation was falsified after its hard boundary. The
`companion=false` rate remained 1,383/min over five minutes at 17:42Z and
2,113/min over one minute at 17:43Z. A read-only contract/lifecycle cohort then
found the continuing root cause: in each five-minute bucket, roughly 2,500
successful non-companion contracts were still being created to derived
destination identities that were already inactive before both the repaired
15:37Z score publication and the new contract's own create time (2,633 in the
17:30 bucket; 2,473 in 17:35). `GetProvideRelationship` treated equal network
ids as Network without enforcing lifecycle, stale provide-mode keys continued
to advertise the dead destination, and `newContract` used a network lookup that
accepted inactive rows. While Network remained advertised this created a
successful no-escrow contract to an identity that could not receive it; after
that mode disappeared but Stream remained, the same stale destination fell
back to companion settlement and surfaced as missing-origin. This is ongoing
authorization of dead routes, not old clients merely aging out.

A second identifier-free aggregate at `2026-09-02T21:12Z` isolated the live
shape after all repaired score-cache controls were healthy. Over 15 minutes,
8,705 successful non-companion contracts targeted 135 derived destinations
that had already been inactive before contract creation. Every one was
same-network, originated from one of 14 active top-level clients, and retained
both Network and Stream provide keys; none was cross-network or a Public
provider selection. Median destination deactivation preceded the new contract
by 641,254 seconds. This rules out current Public score-cache contamination
and Proxy capacity for that cohort: the active failure is a stale
same-network return path accepted by the pre-guard API. §2.20 now measures the
zero-success invariant directly instead of relying on the failed-request rate
to imply it.

The durable software fix has two halves. The API rejects an inactive or missing
destination before provide-mode selection and repeats active-only endpoint
checks at the contract write boundary. Those destination-lifecycle and bounded
missing-origin failures use the additive wire result
`ContractError_Reliability`; balance, policy, setup, trust, and malformed
contract results remain distinct. Connect binds every ContractManager callback
to the exact multi-client channel that emitted it. A matching Reliability
result immediately excludes that channel from new-flow selection, records a
terminal channel error, and wakes the existing resize path, which performs the
normal removal, eligible-flow migration, and window refill. The destination
key must match the channel tail, so a result cannot poison a neighboring exit.
Older clients safely ignore the new action and retain their existing timeout
behavior.

Verification requires the API fix first and a Connect-bearing client build for
the window reaction. After API convergence, successful contracts to already
inactive destinations must fall to zero and the missing-origin rate must return
to its calibrated band. After Connect rollout, a synthetic or observed
Reliability result must remove only its emitting exit and refill that window;
InsufficientBalance and every non-Reliability result must leave window health
unchanged. Do not substitute a longer contract timeout, provider-capacity
hardware, or manual cache deletion for either invariant.

This is a software/provider-lifecycle or bounded operational-aging alert, not a
hardware-capacity alert. More Proxy hosts can raise the active-client ceiling
but cannot repair an unbootstrappable selected destination.

Implementation convention: SIGNALS.md §2.17 (`missing-origin`) maps to
`signal_missing_origin.go` and `signal_missing_origin_test.go`. Synthetic tests
cover the high-rate frame, healthy boundary, absent and duplicate-series
visibility, stale samples, invalid rates, query scoping, and detailed Markdown
rendering without identifiers.

### 2.18 Stale contract destination rejection — dead routes must not authorize
Probe: `stale-destination`

The API-side active-lifecycle guard exports the bounded counter cause
`urnetwork_connect_contract_failures_total{cause="inactive_destination"}`.
Unlike §2.17, this is not an inferred role or fallback: the contract boundary
itself found the requested destination missing or inactive and refused to
create the route. Query both original companion partitions over five minutes:

```promql
sum by (companion) (rate(urnetwork_connect_contract_failures_total{
  env="main",cause="inactive_destination"
}[5m])) * 60
```

The current API initializes both `companion=false` and `companion=true` label
children to zero. This is part of the observation contract: two fresh zeroes
mean the guard saw no rejection, while an absent, duplicate, malformed, stale,
or time-skewed partition is UNKNOWN and must not be converted to zero.

- HEALTHY: both partitions are present and the fleet total is at or below
  50/min for two complete five-minute windows. Successful contracts whose
  destination was already inactive at create time remain exactly zero.
- LIFECYCLE REJECTION: the total exceeds 50/min. The guard is preventing a
  correctness violation, but the selection/window path is still offering dead
  identities often enough to affect users. Preserve the `companion` split as
  bounded context; it is the original request bit and does not establish an
  endpoint role.
- UNKNOWN: either initialized partition is absent, duplicated, stale, negative,
  NaN, infinite, labeled outside the fixed Boolean vocabulary, or evaluated at
  a different timestamp. During rollout, absence means the API generation or
  metrics path cannot expose the guard yet. A schema-valid Mimir vector missing
  either fixed partition is class `stale-destination-instrumentation`, not
  generic connection loss: compare every API artifact/start with `c8dfe570`,
  allow a complete five-minute rate window after the last process converges,
  and only then investigate CounterVec initialization, scrape freshness,
  remote-write acceptance, and label retention. Never convert the missing rate
  to zero.

The durable repair is ordered. First deploy every API instance from server
commit `c8dfe570` or a descendant so an inactive destination cannot pass mode
selection or the final active-only write check and receives the additive
`ContractError_Reliability` result. Then rebuild affected Connect-bearing
clients from Connect commit `5b33c91` or a descendant. That client binds the
status to the exact emitting channel, excludes it from new-flow selection,
records a terminal route error, and wakes the normal resize/refill path. An old
client remains wire-compatible but can keep retrying its stale exit, so an API
rollout alone protects contract correctness without necessarily removing the
retry load.

The 2026-09-02 main API, Connect, Proxy, and Taskworker artifacts were built at
14:56–15:12Z from modified base `2d6f27c`, while the two repair commits were
created at 18:05Z. Those artifacts therefore predate this repair. Before calling
the incident fixed, prove exact running API and affected client artifacts carry
the commits, let two full rate windows elapse, and require the inactive-success
cohort to remain zero. If rejection remains high after the deployed client
window lifetime, use §2.8, §2.9, §2.15, §2.16, and bounded lifecycle/relationship
cohorts to locate the stale producer. Do not delete Redis provide keys, weaken
lifecycle checks, lengthen contract timeouts, or restart clients merely to
clear the graph.

The first live §2.18 monitor run at 18:51Z reached Mimir successfully but
returned neither initialized partition. The contemporaneous process-start
range was 15:19–15:21Z for all 20 API processes, independently confirming the
pre-fix deployment boundary. Generic “restore access” guidance was therefore a
monitor attribution defect: the observation transport was healthy, while the
running API generation could not yet publish the new invariant. The dedicated
instrumentation class preserves that distinction and will automatically move
to a concrete rate result after the corrected fleet and its rate warmup exist.

This is a software lifecycle-correctness signal, not a Proxy hardware-capacity
signal. More Proxy hosts raise the active-client ceiling but do not make an
inactive destination contractible.

Implementation convention: SIGNALS.md §2.18 (`stale-destination`) maps to
`signal_stale_destination.go` and `signal_stale_destination_test.go`. Synthetic
tests cover the high-rate frame, explicit-zero boundary, missing/duplicate and
unknown partitions, stale/invalid/skewed samples, query scoping, rollout-aware
missing-instrumentation guidance, and detailed identifier-free Markdown.

### 2.19 Provider egress probe coverage — every durable shard must advance
Probe: `egress-coverage`

The provider-egress pipeline now runs as recurring `pending_task` shards rather
than as host-owned edge services. Generic task health (§1.2/§8.9) can detect a
row that is parked, overdue, or failing, but cannot prove that every hash slice
exists. A healthy shard can also keep a fleet-wide maximum timestamp fresh
while another slice is absent or frozen. Observe the durable geometry first:

```sql
SELECT run_once_key, args_json, run_max_time_seconds
FROM pending_task
WHERE function_name =
  'github.com/urnetwork/server/taskworker/work.ProviderEgressProbe'
ORDER BY run_once_key;
```

Parse the argument JSON inside the monitor without returning it in an alert.
Every row carries `shard_index`, `shard_count`, `idle_delay_seconds`,
`max_time_seconds`, and the bounded full/blackhole batch settings. Require:

- exactly `shard_count` rows with one common settings snapshot;
- indexes covering every integer in `[0, shard_count)` exactly once;
- `run_once_key=["provider_egress_probe",shard_index]` for each row;
- positive limits, concurrency, timeouts, idle delay, and max time, with each
  concurrency at or below its batch limit; and
- `pending_task.run_max_time_seconds` equal to the argument snapshot.

Malformed JSON, endpoint values, task IDs, credentials, and client IDs are
never copied into the alert. The probe reports only bounded structural reasons
such as `missing_shard_1` or `row_2_mixed_settings`. Zero rows is not zero due
work: it is `egress-probe-unarmed`. The same rollout alert remains open until
the append-only `provider_egress_health.tls_authentication_failure` field
exists, because the new full-probe ingestion path cannot satisfy its integrity
contract without that schema.

After geometry is valid, assign each eligible active, top-level, connected,
valid Public provider to the exact normalized PostgreSQL partition used by the
due APIs:

```sql
((hashtext(client_id::text) % shard_count) + shard_count) % shard_count
```

For each shard, aggregate without exporting identifiers:

- full due: no egress location, or location older than 84 hours, AND no probe
  attempt in six hours;
- blackhole due: no check, or a check older than 90 minutes;
- newest full activity: the newest location, attempt, or health timestamp in
  that same shard;
- newest blackhole activity: the newest blackhole check in that shard; and
- current coverage: locations inside seven days and blackhole checks inside
  three hours.

The due ages are the application contract: full refresh begins at half the
seven-day location lifetime, failed attempts back off for six hours, and the
cheap blackhole sweep becomes due at half its three-hour maximum age. Do not
invent a percentage floor while a large first sweep is catching up. Instead,
when `due > 0`, require the corresponding shard-local newest timestamp to be no
older than its durable `max_time + idle_delay` plus one five-minute monitor
cadence. Old evidence is healthy when the exact due count is zero.

- `egress-probe-shards` (PAGE): missing, duplicate, malformed, or
  mixed-generation durable geometry. Let the normal bootstrap/post path
  converge it; never clone, delete, or rewrite pending rows by hand.
- `egress-full-stalled` (PAGE after two samples): a shard has full-probe due
  candidates but no location/attempt/health progress inside the derived bound.
- `egress-blackhole-stalled` (PAGE after two samples): a shard has blackhole
  due candidates but no check progress inside the same derived bound.
- `egress-probe-unarmed` (WARN after two samples): the required schema or all
  durable tasks are absent. When the schema is absent, apply migrations before
  deploying a Taskworker artifact from an intentional server checkout
  containing commit `49b51eeb` or later. When the schema is already armed, name
  that Taskworker rollout as the immediate next boundary instead of asking the
  operator to repeat the completed migration. In either case, let normal task
  initialization schedule the rows; never create or repair shard rows by hand.

The 2026-09-03 main incident is a dated rollout control, not a permanent
version assertion: the TLS-integrity field was present but zero durable probe
rows existed. All eight fresh Taskworkers reported the same modified base
revision `2d6f27c2`, which predates `49b51eeb`; the base has neither the probe
implementation nor its `InitTasks` scheduler, and a bounded three-hour log
window contained no provider-egress task line. Because `modified=true` does not
describe the participating checkout diff, the revision alone was not treated
as proof. Zero initialized rows after every current process had completed
startup supplied the behavioral discriminator. The immediate closure boundary
was therefore a Taskworker artifact containing `49b51eeb`, not another schema
migration.

Correlate a stalled frame with its bounded `ProviderEgressProbe` Taskworker
logs and generic task error. Repair the concrete authentication, API,
task-claim, or tunnel execution fault; do not delete provider evidence just to
move the timestamp. This is a **software execution / operational rollout**
alert class. It cannot be fixed by adding Proxy hardware, and it does not imply
that the independent Proxy active-client ceiling is adequate.

Implementation convention: SIGNALS.md §2.19 (`egress-coverage`) maps to
`signal_egress_coverage.go` and `signal_egress_coverage_test.go`. Synthetic
tests cover a fully unarmed rollout, the schema-armed/tasks-absent deployment
boundary, a missing shard, shard-local full/blackhole stalls hidden by a
healthy sibling, healthy empty due queues, malformed-secret redaction,
normalized signed hashing, and ambiguous aggregate rejection.

### 2.20 Successful contracts to inactive destinations — stale route acceptance
Probe: `stale-contracts`

The rejection counter in §2.18 measures attempts stopped by the lifecycle
guard. This probe independently measures the more severe opposite outcome: a
row was successfully created after its destination had already become
inactive. It reads only the most recent five-minute PostgreSQL cohort:

```sql
SELECT count(*)
FROM transfer_contract tc
JOIN network_client destination ON destination.client_id = tc.destination_id
WHERE tc.create_time >= now() - interval '5 minutes'
  AND tc.companion_contract_id IS NULL
  AND NOT destination.active
  AND destination.deactivate_time IS NOT NULL
  AND destination.deactivate_time <= tc.create_time;
```

The `deactivate_time <= create_time` boundary is essential. Looking only at a
destination's current inactive bit would falsely include a healthy contract
whose destination disconnected after creation. The probe also exports only
aggregate same/cross-network, derived/top-level, active-top-level source,
distinct endpoint counts, and median/p95 deactivation lead time. For the
same-network subset it exports distinct network, source-device, destination-
parent, and destination-device counts; these distinguish one concentrated
relationship/window boundary from a common failure distributed across several
networks. For the cross-network subset it additionally exports inactive top-
level destination, derived source, active source-parent, distinct destination,
distinct parent, and distinct device counts. Those bounded counts distinguish
one retained client window from a fleet-wide producer without returning
identifiers or contract content.

- HEALTHY: zero matching successful contracts on two consecutive five-minute
  cohorts, alongside both observable §2.18 rejection partitions.
- `stale-contract-success` (PAGE immediately): one or more matching rows. A
  successful row is affirmative contract-correctness failure, not a noisy
  retry or inferred client error. Same-network derived-destination dominance
  identifies the stale return-path class seen on 2026-09-02. Its bounded
  network and parent/device cardinalities distinguish one retained relationship
  from a systemic multi-window lifecycle failure. Cross-network rows to
  inactive top-level destinations from derived sources with active parents can
  be a retained Public route. Concentration into one destination and one
  parent/device, paired with bursts from fresh derived sources, is a client-
  window discriminator; corroborate a bounded current score-cache sample before
  ruling the cache in or out.
- UNKNOWN: the aggregate is absent, malformed, negative, internally
  contradictory, or its median exceeds its p95. Preserve that as observation
  failure rather than coercing it to zero.

Use §8.12 to prove every API artifact contains server commit `c8dfe570`, after
satisfying the selected artifact's append-only migration prerequisite. The API
must reject the stale destination before mode selection and repeat the
active-only check at the write boundary. A Connect-bearing client containing
`5b33c91` separately consumes the Reliability result and retires only the
emitting route, reducing repeated attempts; client behavior cannot substitute
for the server-side zero-success invariant. Do not delete contract history,
inactive clients, or provide keys to manufacture a healthy result.

This is a **software lifecycle-correctness** alert. It is not fixed by adding
Proxy hardware, widening a timeout, weakening provider eligibility, or
clearing Redis state. The 2026-09-02 production control was the 8,705-row
same-network cohort described in §2.17; after deployment, require this probe to
stay at zero for two complete windows while §2.18 becomes observable and the
missing-origin rate returns to band.

A `2026-09-03T05:24Z` identifier-free 15-minute cohort separated a smaller
cross-network shape from the dominant same-network incident. All 180 rows went
from 60 derived source identities belonging to one active parent, network, and
device to one inactive top-level destination. Every source was created only
15--17 seconds before its contracts and made exactly three within zero to two
seconds; the rolling five-minute view therefore shows 20 new derived sources
and 60 contracts. The destination had been inactive for about 18.5 hours,
retained a Public provide key, had no Stream key, and had no connected row. A
bounded read-only sample of 1,600 current score blobs across all 32 Redis nodes
found that destination zero times. That sample is not an
exhaustive proof of absence, but the one-parent/one-destination concentration
and per-source bursts strongly identify one retained client route churning new
derived identities rather than fleet-wide score-cache contamination. The
pre-guard API turns those stale attempts into successful contracts, so deploy
the `c8dfe570` lifecycle guard first; a Connect-bearing client containing
`5b33c91` then consumes the Reliability result and retires exactly that route.
Do not delete the durable Public key or Redis data to hide the cohort.

A `2026-09-03T09:51:50Z` identifier-free five-minute cohort resolved the
dominant same-network attribution. All 2,536 successful stale contracts came
from 11 active top-level source identities on 11 source devices across three
networks. They targeted 117 inactive derived identities belonging to five
destination parents/devices; median and p95 inactivity lead times were about
7.96 and 8.07 days. This is not one retained Public route or one poisoned
device: several same-network relationship windows are retaining old derived
return paths. The common causal mechanism remains the pre-guard API accepting
those routes as successes, which withholds the distinct Reliability result the
fixed clients need to retire them. Preserve the ordered API-then-client repair;
do not turn the aggregate cardinalities into customer identifiers or clear
durable history.

Implementation convention: SIGNALS.md §2.20 (`stale-contracts`) maps to
`signal_stale_contracts.go` and `signal_stale_contracts_test.go`. Synthetic
tests cover the exact inactive-before-create failure, a healthy zero cohort,
concentrated same-network and retained-Public-route boundaries, malformed and
contradictory aggregates, query scoping, privacy boundaries, and detailed
Markdown rendering.

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
 | sed -E 's/[0-9A-Fa-f]{8}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{12}/<id>/g' \
 | sort | uniq -c | sort -rn | head -20
```
Shapes, not bytes (a few huge keys are invisible here — pair with
`--memkeys`). Run LC_ALL=C. This is how the `{cs_}` concentration and the
legacy pile families were identified. The monitor can run this on the
fullest node daily + on any skew alert, and diff family counts week-over-week
(a family growing without bound = missing TTL — the recurring disease).

The shell normalization is not itself a safe persistence boundary. A key from
an unknown schema can contain non-UTF-8 bytes, printable identifiers, or a
shape longer than an alert field. Before logging, baselining, or alerting, the
probe retains only bounded printable shapes from a compile-time allowlist of
known schemas whose dynamic fields are explicit placeholders. A new schema is
redacted until its normalized static form is reviewed. The probe aggregates
every other label into
`redacted-binary-family`, `redacted-unnormalized-family`, or
`redacted-oversize-family`; protected diagnosis can inspect that class without
copying a raw key into an alert. In v189 at 2026-09-01T15:14Z, the former path
logged one malformed binary fragment because unmatched `SCAN` output flowed
straight into the histogram. That observation proves the monitor leak, not
that the underlying Redis key is corrupt. Synthetic coverage pins binary
aggregation and verifies that neither the raw fragment nor replacement runes
reach Markdown.

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

The post-deployment writer gate was still clean at 2026-08-31T15:15Z. Bounded
two-hour Grafana log queries across API, taskworker, Connect, proxy, MCP, app,
and web returned zero application `[redis][ttl]` result lines. Those searches
did make Grafana log their query literals; the pre-fix standing classifier
mistook that metadata for six and then 20 warnings/minute (§1.5), but the
samples were `executing query`, not Redis writes. After separating query echo,
the evidence proves the sampled legacy stream keys are stale residue rather
than a currently repeating writer defect. It satisfies the read-only
precondition for cleanup, but not the authority gate: changing production TTLs
still requires an explicit maintenance decision.
The gate repeated cleanly at 19:17Z after API
`2026.8.31-outerwerld+1033800390` and Connect
`2026.8.31-outerwerld+1033803620` had fully converged: independent bounded
30-minute pulls returned zero `[redis][ttl]` lines from API, Connect, and
taskworker. The persistent 30-of-32-node alert is therefore old metadata, not
a reason to deploy those services again.

The 20:28Z post-deploy probe made the residue boundary semantic rather than
log-only. Its binary-safe sample on port 6393 found 106 impossible-TTL keys:
53 legacy stream-contract names, 53 legacy stream-id names, zero current
stream names, and zero other families. A separate bounded 30-minute Connect
query returned zero `redis-ttl-suspect` and zero `[redis][ttl]` lines. Connect
`1033803620` is therefore verified fixed for this defect; only the explicitly
authorized legacy-key cleanup can resolve the remaining TTL alert.

The independent 22:17Z control remained clean after another two hours at the
fully converged Connect version. The then-worst node moved to port 6395, where
the binary-safe sample found 128 suspects: 64 legacy stream-contract names and
64 legacy stream-id names, again with zero current-generation and zero unknown
families. Bounded two-hour Connect queries returned zero `[redis][ttl]`,
`redis-ttl-suspect`, panic, or fatal lines. A changing worst-node port or legacy
residue count is therefore not evidence of a resumed writer; a new writer
requires a current-family sample or a post-deploy TTL warning.

A pre-maintenance cleanup audit on 2026-09-01 found a second cleanup-side
correctness defect before any authorized production run. The scanner used
go-redis's typed `PTTL` result, whose reply path multiplies Redis's raw integer
milliseconds by `time.Millisecond`. Go `time.Duration` spans only roughly 290
years, while the observed legacy TTLs are roughly 913,000 years, so that
conversion necessarily wraps before the scanner compares it with eight hours.
The exact wrapped value depends on the raw TTL; an observed value may happen to
remain above eight hours, but it is not a trustworthy ordering. A deterministic
local Redis fixture sets `PTTL=18,446,744,073,710ms` (roughly 584 years), proves
the typed result wraps below one millisecond and the old comparison would skip
the key, then proves the corrected cleanup clamps the raw value to eight hours.
Current-main server commit `d8e34003` pipelines generic `PTTL` commands and
reads the signed 64-bit millisecond result directly. Stable patch IDs prove it
is patch-identical to the former `d9b2e291` hash after main was rewritten. No
production TTL was changed during this audit. Any authorized cleanup artifact
must be attributable to a local checkout containing the current-main commit;
an older typed-duration build is unsafe even if it contains the legacy suffix
fix. Record any participating diff.

- Do not inspect binary stream keys through shell variables; embedded bytes
  can truncate or corrupt family attribution. The existing
  `ExpireLeakedStreamKeys` scanner is binary-safe and pipelines raw-integer
  `PTTL` on each shard. Its first implementation scanned only the newer
  `*s2_sk_*` names and therefore missed the production residue, which predates
  that namespace and uses `*s_sk_*`; its next revision used a typed duration
  that could overflow. The corrected scanner covers both generations,
  validates the exact suffix before changing a key, and retains Redis's int64
  milliseconds through the threshold comparison.
- Classify every new `redis-ttl-suspect` line by command and redacted key
  family. Require zero new stream-family warnings before cleanup; a warning on
  another family is an independent writer defect, not proof that stream writes
  resumed. Running `bringyourctl streams expire-leaked-ttls` changes production
  TTLs and therefore requires explicit maintenance authority. First prove the
  binary is a clean `d8e34003` descendant and does not use go-redis's typed
  duration result for `PTTL`. It clamps only legacy/current stream-id and
  stream-contract keys beyond 8h, allowing active streams to refresh and
  orphaned residue to expire.
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
- Verify TTL repair with a binary-safe raw-integer `PTTL` sample, `avg_ttl`
  below two years, and no new TTL warnings. Verify dataset-memory recovery
  independently through §3.3b; raising maxmemory repairs neither defect.

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

Production published `client_score_alias_v1_ready` at 08:33:49Z on
2026-08-31 after the first complete compatibility export. A 08:46:35Z bounded
sample still found score keys owning 91.3% of sampled bytes on a 98.3%-full
node. That sample is only 13 minutes into the documented five-hour drain: it
does not mean the alias-aware software is absent or should be redeployed. The
probe now reads the durable ready marker. When present, the alert reports
`alias_schema_ready=true`, directs operators to use the taskworker ready log as
the expiry clock, and forbids redeploying, deleting legacy keys, or raising
maxmemory merely to accelerate the expected drain. For this publication, the
first terminal memory sample is due after 13:33:49Z.

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
  client library. The same trip-time battery records blocked clients, local
  latency, accept receive queue versus listener backlog, Redis client memory,
  and maximum client output memory. For marker-free reliability traffic it also
  resolves a bounded minute-block ownership history sized from the EXPIRE
  cohort's observed idle age, using one pipelined `redis-cli` session. This
  distinguishes one
  fixed-slot key touched fleet-wide from a reconnect storm without transferring
  raw client rows, preserves a rotating shard collision after its one-minute
  keys have moved, and prevents a correctly attributed workload collision from
  being mistaken for harmless socket shape when it is already exerting
  measurable pressure.
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
- A 2026-09-01 marker-free control identified a different, rotating shape.
  At 10:45:47Z port 6394 had 1,051 clients against a 290 median (3.6x), then
  1,220/348 and 1,040/309 on consecutive ticks. It did not own the retired
  discovery marker. A same-incident host socket snapshot attributed 777
  connections to edge-4's four Connect blocks (178, 257, 185, and 157; the
  second block was at its configured 256-connection per-node ceiling), while
  `CLIENT LIST` was dominated by 472 long-lived edge-4 connections ending in
  `EXPIRE`. Port 6394 owned two current-minute sharded reliability hashes
  (`client_reliability_stats.29804343.1` and `.5`) and no previous-minute
  shard. The 32 keys hash independently across 32 masters, so random
  balls-into-bins collisions concentrate reliability command load on some
  masters and can expand their lazy pools. There were no Connect pool-timeout
  logs and no Redis latency, output-buffer, accept-queue, or memory alert.
  This is not the fixed-marker regression and does not justify killing
  clients, lowering pool limits from the ratio alone, or rolling back the
  marker-free writer. The trip battery now resolves current and previous
  shard ownership and emits the collision diagnosis only with an `EXPIRE`
  cohort. Let ownership rotate and idle pools age out while the health
  controls remain green; if the collision causes measured pressure, use a
  rolling-compatible wider fanout or deliberate slot placement, with a
  dual-schema rollup during transition.
- The 16:18Z recurrence strengthened that boundary. Port 6394 reached 1,276
  clients against a 244 median while it owned three current-minute and two
  previous-minute shards; a direct 16:21Z control then found 1,472 clients
  while ownership rose to five current plus two previous shards. The node had
  zero blocked clients, 0.279ms average local latency, zero accept receive
  queue, about 3.7MB of normal-client memory, and zero client output-buffer
  bytes. By 16:25Z ownership had rotated to zero current and zero previous
  shards while the node still held 1,486 clients, proving a two-block lookup
  can lose the causal collision before its lazy pools contract. The fleet
  ratio is therefore a real randomized load/pool shape but not current Redis
  impairment. The reusable trip battery now carries those controls and a
  bounded history derived from EXPIRE idle age; a future collision with any
  control in its alert band escalates to active pressure and the compatible
  wider-fanout/placement repair instead of waiting only for idle pools.
- The promoted v194 watcher supplied the full sustained-frame control. Its
  trip battery at 16:33:51Z counted 2,244 CLIENT LIST rows and retained a
  two-shard collision four minute blocks earlier; at the 16:38:53Z sustained
  sample, port 6394 had already contracted to 1,574 connected clients against
  a 384-node median. The controls captured at the trip remained healthy:
  zero blocked clients, 0.297ms local latency, an empty accept queue against a
  65,535 backlog, about 5.8MB of normal-client memory, and zero output-buffer
  bytes. Those two client totals are intentionally different time frames, not
  inconsistent Redis counters. Alerts therefore label shard ownership,
  CLIENT LIST cohorts, and pressure controls as trip-time evidence while the
  symptom and ratio remain the later sustained sample. This live contraction
  continues to support observation rather than a client kill, pool-limit
  change, or marker-writer rollback.
- A 2026-09-03 outlier established a separate Connect live-delta
  amplification fingerprint. Port 6393 held 954 clients against a 281-node
  median (3.4x); 375 long-lived clients from edge-3 and 294 from edge-4 ended
  in `HGET`. A simultaneous 15-second commandstats delta measured 301.13
  HGET/s on 6393 while the next busiest Redis node was only 9.4/s. The node
  still had zero blocked clients, 0.487ms local latency, an empty accept queue,
  about 2.7MB of client memory, and no output-buffer memory, so this was
  workload and lazy-pool amplification rather than a wedged Redis process.
  The only production HGET path for that peer metadata is the network-peer
  listener. The process-wide key-event subscriber delivered one event to
  every resident listener, and each listener independently read the same hash
  member. The software fix constructs one lazy immutable delta per event and
  shares one pipelined peer-metadata plus event-version read across the
  process's listeners; construction remains nonblocking in the subscriber.
  Deploy the Connect artifact containing that fix. Verify on consecutive
  samples that both HGET/s and the HGET-ended cohort collapse, port 6393
  returns near the fleet median, key-event delivery continues, listener resets
  do not spike, and Redis latency/pool-timeout controls stay healthy. The
  probe's trip battery now measures a two-second HGET commandstats delta and
  counts HGET-ended clients, so this fingerprint no longer receives the
  generic hot-slot diagnosis.
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
  rolled the whole attempt back; by 08:29:43Z the same attempt and `min_time`
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
- A later deployed run, task `<redacted-id>`, ran on
  edge-4/g2 from 15:34:17Z to 15:53:29Z (1,152.120s). Its exact PostgreSQL
  statement remained active with no wait event while the task heartbeat
  advanced, then the task completed normally. Concurrent
  `transfer_contract` autovacuum continued scanning its 23.98M-block heap.
  That terminal state rules out an orphaned/canceled backend in this sample;
  it does not make the all-lookback transaction safe. Retain the four-hour
  anchor cadence, per-lookback checkpoints, and maintenance-aware optional
  deferral, then verify their marker-by-marker behavior after rollout.
- The following task `<redacted-id>` ran on
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
| `[c]Could not initialize tls config. Disabling transport. = ...` (`connect-tls-disabled`) | A legacy Connect-bearing process failed to load its transport identity, substituted an empty TLS configuration, and could still bind UDP while rejecting every QUIC ClientHello below authentication. | Inspect and repair the active TLS certificate/key resource without logging key material, then deploy server `64366fb5` or later so the checked constructor fails startup before any listener goroutine. Require listener readiness plus a real QUIC handshake on every enabled carrier; do not restart the same artifact or treat a bound socket as recovery. |
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
| `server login has been failing, cached error: sorry, too many clients already (server_login_retry)` (`pg-client-capacity`) | PostgreSQL refused a PgBouncer server login at its connection ceiling and PgBouncer cached the result. A timed-out transaction can leave its old backend unwinding while PgBouncer opens a replacement. A single failed request can also be rendered as `Unexpected error`, route recovery, and goroutine-shaped JSON, so the class takes precedence over generic panic and raw log volume is not unique-failure count. The 2026-09-01 control tied the burst to legacy reindex WAL/storage stalls and 60–66-second `COMMIT`s, not idle retention. | Run direct §1.3a immediately; split active, young idle-in-transaction, idle, and starting owners and correlate PostgreSQL waits/commit latency with PgBouncer connection logs or `SHOW POOLS` where permitted. For the matching legacy-reindex chain, wait for index progress to empty and deploy current-main Taskworker fixes `908a8b2c` and `d8392c83`. Do not first tune pools, raise `max_connections`, restart the database/pools, or mass-terminate sessions; preserve the deployed `work_mem` memory-risk context. |
| `pgproto3.writeError=write failed: write tcp ...->...:6432: i/o timeout` | The app could not write a request into the nginx/PgBouncer frontend before its socket deadline. Unlike `query_wait_timeout`, it may occur before postgres sees a query; direct-pg active load can stay low. | Split the 6432 nginx frontend, its 32 PgBouncer shard queues/listeners, and direct 5432 with §2.11. Group by route; do not merely increase the timeout. |
| `[db]maintenance reindex[<i>/<n>] <excluded-table>` (`db-maintenance-legacy-reindex`) | A Taskworker selected a large or high-churn table that current policy excludes and entered the legacy full-table concurrent-reindex path. Because old code logs before acquiring its maintenance connection, the line proves selection/attempt, not that PostgreSQL began or completed the statement. Interruption plus lease recovery can strand `_ccnew`/`_ccold` artifacts and repeat the rebuild before old end-of-rotation cleanup. | Inspect `pg_stat_progress_create_index`, `reindex-debris`, and the exact DbMaintenance owner before changing Taskworker. Do not let a rollout implicitly cancel active work. After progress is empty, satisfy §8.13 and deploy a clean Taskworker containing current-main commits `908a8b2c` plus `d8392c83`; clean debris separately with explicit maintenance authorization and the supported cleanup-only command. Never wildcard-drop artifacts. See §2.2a. |
| `[plugin.notRegistered] plugin not registered` in `ngalert.scheduler` or `/api/ds/query` | One request named a plugin type that the serving Grafana process could not resolve. A missing native datasource is one cause; a stale dashboard/browser payload or another unsupported request type can emit the same generic line while the required datasource plugins work. | Retain the request path/referer and query both `warp-mimir` and `warp-loki` through that exact generation's `/api/ds/query`. A failed control is the image/plugin branch in §11.15; if both controls succeed and both plugin processes remain present, repair the request/dashboard client instead. Do not infer an image omission or recreate a datasource from the generic log alone. |
| `caller=tail.go:<line> component=tail-querier ... msg="Error receiving response from grpc tail client" [addr=<backend>] err=EOF` | Loki's external WebSocket tail can remain connected while an internal gRPC tail backend is lost, omitting that backend's live entries. The historical exact 59–61-second recurrence was Warp's 60-second ring TCP application read deadline. The current off-grid wave followed Loki's 15-second blocked-ingester-tail close path after all six active ring nodes were healthy. The clean post-`42168fe` window still had 2,825 unconditional evaluator records, 1,084 table lookups, 942 bucket warnings, 1,229 resets, and 22 EOFs spread across every backend. The emitting Grafana host follows the selected querier and is not backend attribution. A quoted `Canceled ... context canceled` during deliberate client retirement is a separate lifecycle. | Verify the running Grafana artifacts contain Warp `1e95aef` and `bca37cf`; deploy them only to older Grafana blocks. For the current fleet, deploy Grafana with Warp `5927527`, which includes `13fcd05`'s producer reduction and service attribution without suppressing reset/EOF or Mimir warning/error evidence. Require query-frontend/evaluator and table-lookup info records zero, bucket-index convergence below two minutes, rules/metrics healthy, and EOF/reset/service-drop classes zero for ten minutes with stable tails and bounded reconciliation. Investigate a residual `addr` frame on that node. Do not raise tail/queue/ring limits or restart the same image. See §1.5. |
| `caller=tailer.go:<line> msg="tailer dropped streams is reset"` | An ingester-side live-tail queue overflowed before its internal gRPC send. Upstream and the current deployed Loki 3.7.3 discard the accompanying `resp.DroppedStreams` in the querier; continued traffic after 15 seconds closes the backend tail and produces the paired EOF. Grafana is the observation point, not affected-service attribution. Historical waves paired with pre-fix Proxy logs. The current clean window proves `42168fe` removed query-frontend records but left the unconditional evaluator stream and other routine producers; resets and attributed EOFs continued across all six backends. | Verify the running Grafana and Proxy artifacts contain Warp `1e95aef` and server `e055c98c`, deploying only where absent. For already-current Grafana blocks, deploy Warp `5927527`; it includes `13fcd05` and forwards bounded ingester descriptors into the existing service-specific `dropped_entries` response without raising a queue. Reconcile every service window and require evaluator/table-lookup info zero, bucket-index convergence below two minutes, alert rules/metrics/warnings/errors healthy, and all three loss classes zero for ten minutes. Any residual EOF must carry a backend frame. Do not raise queues, suppress the reset, or claim Grafana was the affected selector. See §1.5. |
| `[warpctl][loki-tail-dropped-entries] service=<service> count=<n>` | Loki returned a non-empty API `dropped_entries` list for the named standing tail. On Warp `5927527`, this can contain bounded descriptors forwarded from the earlier ingester loss stage; the existing later querier-to-WebSocket overflow uses the same field. This is exact service attribution but requires a same-window raw reset to distinguish the ingester path. | Deploy Grafana with Warp `5927527`, run Warpctl with Warp `26089b2` or later, retain bounded reconciliation for the named service, and remove the correlated producer/consumer stall. Never print the dropped labels or timestamps or raise either queue. Require zero `loki-tail-dropped-entries`, raw resets, and backend EOFs for ten minutes through the triggering load. See §1.5. |
| `caller=bucket.go:<line> ... diff=-<seconds> msg="bucket index version (updated_at) is older than requested"` | Mimir 3.1 logs whenever a store-gateway's local bucket index is older than the querier's requested version. The live fleet's exact `diff=-873`/`-882` was one normal generation of independent 15-minute default phase skew, not a query failure, but it repeated on every query and contributed material self-log volume. Warp `13fcd05` changes the single-tenant gateway refresh to one minute. `mimir-bucket-index-lag` retains a conservative >=1,800-second threshold across rolling/older generations. | Verify the running Grafana artifact contains `13fcd05`. If absent, deploy it normally; if present, use `mimir-index` (§11.18) to check the framed gateway's last successful sync and tenant coverage plus the shared compactor index age. Require gateway convergence below two minutes. Restore sync/object-store/ring health if stale. Do not suppress every warning, increase `max_stale_period`, or upgrade solely to hide 3.1 log noise. |
| `http: response.WriteHeader on hijacked connection ... router.(*Router).ServeHTTP` | Router recovery attempted an HTTP 500 after the Connect handler transferred its H1 socket to Gorilla. In the 2026-08-31 control, 131 canonical warnings with zero `[h]unhandled` records proved the expected-Done branch fell through to `http.Error`; the rejected response is teardown log amplification, not proof of a failed active transport. | Deploy the router fix that returns immediately for `server.IsDoneError`, then require zero `http-hijack-write` lines for ten minutes of normal H1 teardown. Do not suppress net/http logging globally. A warning paired with `[h]unhandled error from route` instead requires fixing that unexpected route panic. See §1.5. |
| `CLUSTERDOWN` | Slot coverage lost (node marked fail + no failover, or majority loss). | CLUSTER INFO/NODES; restart dead nodes; transient ≤ node-timeout during elections is expected and retried in-client. |
| `OOM command not allowed when used memory > 'maxmemory'` | Node at maxmemory and volatile-ttl has nothing evictable (no-TTL keys dominate). Writes fail, reads work. | Identify node (3.1); drain no-TTL piles (cleanup script) or raise ceiling temporarily; NEVER a client-side problem. |
| `pubsub ... channel is full for 1m0s (message is dropped)` | IN-PROCESS consumer stall: the app isn't draining go-redis's channel (usually because its goroutine is blocked on another redis call). While blocked, the socket goes unread → server buffers grow (3.2). | Check what the consumers block on; server-side buffer alert 3.2 is the paired signal. |
| `connection reset by peer` or WebSocket `close 1006 ... unexpected EOF` (`conn-reset`) | The receiver observed an abrupt transport termination, not an orderly close. `connection reset by peer` is peer-relative, and WebSocket 1006 means no close frame arrived; neither identifies which endpoint or network hop caused it. The peer can be a device or service depending on connection direction, while a NAT, load balancer, firewall, or other intermediary can also discard state or inject a reset. | Preserve the emitting host/block/protocol/process generation, identify connection direction, and correlate the exact minute with peer lifecycle, restarts, LB/NAT/conntrack, carrier, OOM/cgroup, socket, and service-specific resource controls. Do not call it a server close, restart a healthy process, increase buffers, or claim memory pressure from this line alone. Require the rate below 50/min for ten minutes under equivalent traffic and the independently identified causal control healthy. See §13. |
| `LOADING` / `READONLY` | Node restarting (rdb load) / replica mid-failover. Transient; retried in-client. | Only alert if sustained > 2 min. |
| `[redis][ttl]` (server-side guard, server/redis_ttl_warn.go) | A redis write carried an effective ttl beyond its family limit, or a raw Go `time.Duration` command/eval arg. Raw Durations serialize as int64 NANOSECONDS, so an 8h ttl can become `EXPIRE <key> 28800000000000` (~913,000 years); alternatively, a correct `EXPIREAT` can expose an unbounded durable deadline. The 2026-07-20 signature was ~1.1M immortal legacy `s_sk_*` stream keys. | The warning names the command + redacted key family. For raw Duration, pass seconds/ms ints and clean the affected family. For a long `EXPIREAT`, preserve authoritative data and bound only the Redis mirror horizon; see §5.11. |
| Panic stack traces (`trace.go` "Unexpected error") | The STACK identifies the load-bearing call path (e.g. AddNetworkPeer → NominateLocalResident = connection-killing). | Rate per unique innermost app frame; a new frame appearing at rate = new incident. |
| `dohRouteForConn.func1` with `runtime error: invalid memory address or nil pointer dereference` | HTTP/2 reused or retired a live connection wrapper whose `LocalAddr()` or `RemoteAddr()` was nil. The optional route-observation callback dereferenced that endpoint, so `HandleError` recovered the resolver goroutine but the in-flight DNS result was lost; the proxy process and public listener remain healthy while a request can time out. This is not provider unresponsiveness. | Any occurrence identifies a pre-fix Connect module. Current code treats nil and typed-nil endpoints as absent diagnostic metadata and preserves the DoH response. Deploy the fixed proxy generation, then require zero new occurrences while sustained HTTP/SOCKS/WireGuard acceptance runs. See §14.6. |
| `urnetwork_connect_contract_failures_total{cause="insufficient_balance"}` (Mimir; `[contract][error] class=insufficient_balance` is a rate-limited exemplar only) | Payer network has no usable balance. Runs at a steady background rate (~1,000+/min measured 2026-07-17) from out-of-data free users — presence is NOT an incident. | The provisioned Grafana rule watches the lossless 5-minute counter rate; >4,000/min for 5 minutes = netEscrow drift re-emerging (`bringyourctl contracts reconcile-net-escrow --dry-run`) or a balance-grant regression. Do not calculate the rate from sampled logs. |
| `asset amount owned by the wallet is insufficient` / `insufficient token balance ... in wallet` (taskworker, Circle payment path) | The payout wallet cannot cover pending payouts (USDC on Solana — mint EPjFWdd5...Dt1v in the protected source log). Each affected `AdvancePayment` remains pending on a one-hour-mean consecutive-error backoff, so N parked rows produce roughly N canonical attempts/hour on average. One attempt normally emits both a Circle-client and task-evaluator diagnostic; the alert therefore reports `wallet_insufficient_events` separately from raw line rate. Proportional 30–90-minute jitter disperses cohorts but cannot impose an instantaneous fleet ceiling; current-main `14928f69` (the patch-identical replay of former `eb7e79b6`) separately gates transfer POSTs at three per rolling second. Alert artifacts redact wallet/entity ids. | **Finance/ops action required:** fund the exact network/token wallet from protected logs or pause payouts with the supported operational control. Deploy a clean `66525afc` Taskworker only where §8.12/§2.14 proves it absent; another software deploy cannot create liquidity. Allow 90 minutes plus ingestion delay for natural convergence; never delete/manual-replay task rows, rotate payment idempotency keys, or accelerate retries. |
| `payout-retry-microburst` (derived standing-tail finding; not a literal log line) | At least four exact-replay-deduplicated task evaluator attempts landed in one embedded source second. The post-jitter 2026-09-01 control proved independent random delays still reached five responses plus a sixth 429; four/second is therefore both the empirical precursor and the invariant below the new three/rolling-second gate. | **Software deployment action:** use §8.12 and §2.14 to deploy a clean Taskworker containing `66525afc` only where absent. Preserve backoff and idempotency keys. Verify all admission collectors, zero gate errors, a full 90-minute window below four attempts/second, and no new processor-rate-limit event. Funding or pausing the wallet remains separate finance/ops work. |
| `Bad status: 429 Too Many Requests ... API rate limit error` (Circle payment path) | The processor identity crossed a short-window request limit. One attempt normally produces both a Circle-client and task-evaluator line, so log-line rate is not unique submits. At `07:12:48Z` on 2026-09-01, an already-jittered artifact still produced five wallet rejection responses plus a sixth 429, proving random retry dispersion was not a hard ceiling. Circle documents five default POST requests/second. | Preserve the existing idempotency key and normal backoff; never manually replay or pull rows forward. Deploy a clean Taskworker containing `66525afc` only where §8.12/§2.14 proves the shared Redis-time three/second gate and complete failure telemetry absent. Then require zero gate errors and zero 429s for 90 minutes. If a fully converged gate still sees 429, correlate all Circle request sources and obtain the account's authoritative quota before tuning it. |
| `[circlec][transfer-admission] failed closed` (Taskworker) | Redis admission failed or the task context ended while waiting, so the gate returned before the Circle POST. A deploy drain can cancel one waiter; repetition outside a drain points to Redis health or admission pressure. | Keep the gate fail closed. Correlate §2.14 errors/waits with Taskworker drain state and Redis health; never manually replay, pull the task forward, or loosen the ceiling. Verify zero admission errors and Circle 429s for two five-minute windows with stable idempotency keys. |
| `payout-invalid-destination` — `Invalid destination address.` / Circle code `155219` (taskworker, Circle payment path) | The destination is invalid for its declared chain and Circle rejected it before creating a transfer. The pre-fix chain-blind validator admitted 44-character Solana base58 keys stored as active `MATIC` wallets. Current validation blocks that shape and the taskworker releases only this definitive pre-chain attempt, but six existing payments continued exactly once/hour because the configured payout wallets were still unchanged. | **Account-owner/operations action required:** correct the payout wallet through the supported account API. The current taskworker already releases the typed failed attempt so `UpdatePaymentWallet` can select the correction; another service deploy cannot invent or authorize replacement wallet data. Preserve keys for transport failures, 429s, and ambiguous submits; never edit/delete payment, task, or sweep rows. Verify the next natural retry uses the corrected chain-compatible wallet and the durable/logical counts clear within 90 minutes. See §5.7. |
| `urnetwork_connect_contract_failures_total{cause="missing_companion_origin"}` (Mimir; `[contract][error] class=missing_companion_origin` is V(1) detail only) | A contract request resolved to the companion path but no reversed origin contract exists. Emitted by `CreateCompanionTransferEscrow`. `companion=false` is only the original wire bit: `resolveNonCompanionProvideMode` converted it to Stream fallback, but the request may be selection, provider-return, or same-network traffic. | §2.17 watches only `companion=false` against its calibrated five-minute band. Require the selection controls, then use bounded relationship and endpoint-lifecycle dimensions if it persists; never infer roles from the Boolean or print raw pairs. The higher `companion=true` band needs separate calibration. |
| `Resource not found in vault (<resource>.yml)` in a route panic | A lazily resolved resource is absent from the deployed vault generation. The process and `/hello` can stay green indefinitely; only the first request to the dependent route fails. On 2026-08-29, `/verify/keys` and `/verify/stats` returned 500 while `/hello` remained 200 because the unreleased subnet was disabled and its deliberately absent `verify.yml` was nevertheless loaded by unconditionally exposed handlers. | First branch on feature state. If disabled, fail closed with a stable 503 before parsing or vault access; do not fabricate a signing secret merely to stop the panic. If enabled, the missing resource is a deployment blocker: provision it through the supported secret mechanism and probe the affected route on every active generation (§8.7). |
| `[session]X-UR-Forwarded-For ... was not one ip:port value` or legacy `X-UR-Forwarded-For from untrusted peer` | Source attribution fell back to the ingress peer, collapsing users onto one address for signup/login limits and `/my-ip-info`. The legacy line proves a pre-standardization binary is still active. | Verify Warp overwrites one bracket-safe `ip:port` value, backend ports are not publicly reachable, and every active api/connect generation accepts the UR header. Probe both address families as in §8.8; do not add a proxy CIDR. |
| Client UI/API callback `Timeout.` with no matching route in the exact LB/API interval | The request did not reach the public edge. In the 2026-09-01 Android acceptance failure, ordinary emulator reachability and concurrent API traffic were healthy, but a stale previously-successful Connect dialer received the complete request deadline and hid the healthy route; cold dialers were also incorrectly classified as prior successes. | Correlate the exact UTC action interval against the exact method/path, not the broader auth prefix. If absent, keep diagnosis client-side: inspect `[net]http serial`/`[net]http parallel` route selection and the embedded Connect revision. Require the bounded preferred-route scheduler and cold-route parallel discovery regression tests; do not restart API, increase the UI wait, or add an app-level retry. |
| `[netescrow]negative counter after <site>` | A Redis reservation mirror had fewer bytes than PostgreSQL durably released. Besides a lost create or replayed release, a legacy absolute reconcile can overwrite live mirror traffic (§5.11). The current page-local additive path still has two cross-store windows: a slow PostgreSQL page snapshot can become stale before its later Redis GET, and a committed settlement can precede its Redis post. Old binaries leave the negative value until reconciliation. Current release Lua emits `clamped_to=0` after atomically deleting the nonpositive result while retaining its diagnostic value. A later legitimate reservation or reconciliation can recreate a positive key. Any occurrence remains a defect. | Correlate the first burst with the exact `ReconcileNetEscrow` executor, immutable source, duration, reservation statement profile, aggregate drift, and Redis mutation errors. Retain the page-local additive reconciler and atomic release clamp; deploy single-attempt checked mirror mutations, migration 601, the unsettled-partial query, and the non-current-open pass. After rollout verify any residual line says `clamped_to=0`; a later key is either absent with no new reservations or exactly equals the current PostgreSQL open-reservation sum. Key presence alone does not disprove the clamp. Pages stay below one second and no matched reversal recurs. Alert artifacts retain only `site`; balance/contract ids are redacted. |

At `2026-09-03T09:56Z`–`09:59Z`, the authoritative watcher measured
Fireside Proxy `device_rpc_transport` WebSocket close-1006 unexpected-EOF
waves at 87, 158, 100, and 90 lines/minute. Focused §13.1, §13.3, §13.4,
and §13.5 runs in the same interval reported no proxy-memory, pool, runtime,
or cache alert. Those controls reject Proxy resource pressure as the
demonstrated cause of this wave; they do not establish normal device churn as
the alternative. A redacted ten-minute cohort contained exactly 1,176
attach/EOF/detach triplets from one logical hosted device. Of 1,175 measured
lifetimes, 1,075 were under one second, 100 were one to four seconds, and none
reached five seconds; there were zero write, keepalive, receive-budget,
receive-queue, version-rejection, instance-rejection, or `SyncReverse`
markers. Proxy accepts `/device-rpc`, while the remote `DeviceRemote` initiates
the WebSocket. Its exact 500-millisecond cadence matches the SDK's failed-sync
retry pacing, localizing the wave to one early remote-to-local sync lifecycle.
The deployed August 31 Web SDK and September 2 Proxy build both predated RPC
wire version 3, so their advertised releases do not support a v2/v3 mismatch;
a cached or independently packaged remote still requires direct artifact
evidence. The current server logs cannot split request decode, response decode,
client cancellation, or another pre-`SyncReverse` failure. Capture a bounded
stage/result on both endpoints before assigning that last cause. The old
taxonomy statement that every such line meant "server closed the conn" was a
monitor defect.

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

For `BackfillClock`, an exact 600s cancellation has its own discriminator.
One task row plus one lease and one client-backend leader means RunOnce is
healthy even though PostgreSQL exposes four additional parallel-worker rows.
An unmarked active aggregate is the legacy full-retained-history candidate;
`clock_unrolled_tail` is the bounded replacement. The replacement must first
consume an unbroken sequence of exactly one `transfer-rollup:v1` row per UTC
day, stop at the first absent or duplicate day, and then query raw settled
contracts only from that boundary. Verify the marked tail start and a clean
task completion below 600s. A larger deadline, duplicate task, service or
database restart, and a new broad index do not repair the repeated scan.

For `UpdateReliabilities`, duration is only the trigger for the direct phase
diagnostic from §1.2. A recognized `rolling-*` phase falsifies the old
repeating-full-anchor explanation for that attempt. A recognized
`full-anchor-*` phase still needs its marker reason: missing state,
classification version/token repair, backward bounds, or the quiet four-hour
boundary are mandatory/current behaviors, while artifact ancestry is required
before claiming the obsolete 20-minute cadence is deployed. Evaluate that
four-hour boundary against the current rollup target, not the last committed
window head; report the responsible lookback and classification-guard state.
A cached-statement cleanup error proves an exact connection/deadline boundary,
not the interrupted SQL phase or rollback scope. On 2026-09-03 the
task was running `rolling-leave` for more than 2,600s even though its historical
rolling-leave maximum was about 30s. PostgreSQL had selected an old
non-covering partition child and a scheduled edge reboot then disconnected the
worker without stopping the server-side UPDATE; the reclaimed attempt was
transaction-ID blocked behind the surviving transaction. Preserve the bounded
work, finish §8.10's supported index finalization only after the protected
measurement and explicit DBA authorization, and coordinate future maintenance
reboots with a task drain. Deploy Taskworker from Server commit `fcb4de54` or
later to add the transaction-local two-hour PostgreSQL timeout for future hard
worker losses; it does not accelerate or cancel this incident's existing
backend. A redeploy solely for the already-present cadence/checkpoint changes,
larger MaxTime, query cancel, or database restart is not the repair for this
variant.

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

  Post-deployment validation on 2026-08-31 separated code convergence from
  configuration convergence. All taskworker blocks reported
  `2026.8.31-outerwerld+1033655820`. Its embedded source revision `1d8f01e5`
  contains typed-reset commit `b8af229f0` but predates proportional-jitter
  commit `70b0d269`; this ancestry distinction must come from the artifact,
  not the current checkout. The same six invalid-destination payments retried
  once in the 12:34–12:51Z hour and again in the 13:34–13:51Z hour. That exact
  spacing matches the one-hour consecutive-error cap. Clearing the typed
  attempt does not invent a replacement payout wallet: without an account-owner/operator
  correction, `UpdatePaymentWallet` selects the same invalid configured wallet
  and Circle rejects it again. Correct the wallet through the supported account
  API and let the next retry converge. Do not manually clear payment rows,
  processor keys, or sweeps; persistence after the reset release is an
  operational configuration alert, not evidence that faster retries are
  needed.

  A later bounded canonical-log audit through 16:51:58Z made that conclusion
  stronger. It found 58 task-evaluator invalid-destination attempts for the
  same six logical payments across both taskworker blocks (22 on g1 and 36 on
  g2), including exactly six attempts in every UTC hour from 11 through 16.
  The deployed revision contains the typed pre-chain attempt release, so the
  unchanged six-payment cardinality after each hourly retry proves the current
  payout-wallet selection is still returning the invalid configuration. The
  `payout-invalid-destination` log class now counts exact-replay-deduplicated
  task-evaluator lines separately from the duplicate Circle-client diagnostic,
  redacts entity ids, and warns as an operational wallet-correction alert on
  the first current occurrence.

  A subsequent bounded audit through 17:52:00Z extended the discriminator by
  another complete recurrence. The two-hour source window contained 24
  diagnostic lines but only 12 exact-replay-deduplicated task-evaluator
  attempts: exactly six in 16:35–16:51Z and the same six payment IDs again in
  17:35–17:52Z. Five attempts ran on g1 and seven on g2, ruling out one stale
  taskworker generation as the owner. Combined with the embedded-revision
  ancestry above, this is continued invalid wallet selection after the safe
  typed reset, not evidence that the reset code is missing. At that point it
  did not weaken the separate requirement to deploy `70b0d269` for retry
  dispersion; the later current-main `66525afc` baseline contains that jitter,
  the fleet admission gate, and complete fail-closed telemetry required by the
  post-jitter control.

  An independent artifact/runtime control at the UTC day boundary removed the
  remaining provenance assumption. The published OCI/SLSA build provenance for
  taskworker `2026.8.31-outerwerld+1033655820` names source revision
  `1d8f01e5` for both amd64 and arm64 and a build completion at 08:43Z. Git
  ancestry proves that revision contains typed-reset `b8af229f0` and excludes
  retry-dispersion `70b0d269`. A bounded three-hour taskworker query then found
  36 diagnostic lines but only 18 exact-replay-deduplicated evaluator attempts:
  the same six logical payments at the same minute in each of the 21Z, 22Z,
  and 23Z hours. The deployed reset is therefore active and repeatedly exposes
  the still-invalid selected wallet, while the hour-locked cadence separately
  proves the old jitter. The incident therefore required both correcting the
  wallet through the supported account API and deploying retry dispersion;
  current software convergence uses `66525afc`, which includes the independently
  required fleet admission gate and its failure telemetry. Neither software
  fix can invent or authorize valid payout-wallet data.

  The 2026-09-02 control then proved that baseline live on all eight newest
  Taskworker processes at source `fe3fa8ee`. The same 90-minute interval had
  zero Circle 429s, zero admission failures, and a maximum of three canonical
  wallet attempts per source second. Six invalid-destination rows still
  remained and emitted a new exact task-evaluator event from that generation.
  This is configuration persistence after both software fixes: the typed reset
  safely releases each definitive attempt, but the unchanged account wallet is
  selected again. Correct it through the supported account API; a new service
  version cannot close this class.
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
  A later live retry supplied the deployment discriminator. Exact task
  `<redacted-id>` began on edge-1/g2 at
  05:51:51Z, kept authoritative ten-second heartbeats through planner work,
  and failed at 06:10:31Z after 1,120.91s with the same SQLSTATE 53000 in
  `PaymentPlanner.finalizePayments`, advancing the row from 157 to 158
  failures. PostgreSQL still reported 18.4, while every sampled g1/g2 worker
  reported `2026.8.30+1033129380`; that tag predates the transaction-local
  containment. The row then returned to its normal backoff about 59 minutes
  ahead. This is a pre-fix recurrence, not evidence against the containment:
  do not pull the row forward, and require a post-rollout retry to observe 32
  inside the transaction and reach a terminal success.
  The next live attempt exposed an independent planner defect before it
  reached `finalizePayments`: its active backend spent several minutes with no
  wait event on the subsidy-range MIN/MAX over `transfer_escrow_sweep`. That
  query rescanned paid historical rows even though `temp_account_payment`
  already held the exact selected and close-time-bounded slice. The planner now
  derives the range from that temporary relation, removing both the semantic
  overreach and the lifetime-table scan. A deterministic query-shape
  regression forbids `transfer_escrow_sweep`, a second close-time predicate,
  or an extra bound argument in that stage. This fix ships in `taskworker`
  alongside the transaction-local PostgreSQL containment; verify both
  independently on the same retry.
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
  no drain line, then the same attempt retried on another worker and completed
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
  `<redacted-id>` on edge-3/g2 container
  `4cf91fd25a2e`: its heartbeat passed 6,220s with a fresh claim and no task
  error. Its 7-day distribution had become p50 42s, p95 3,552s, and max
  6,644s, so the old `>2*p95` monitor rule treated another hour-scale tail as
  normal. The taskworker was 34.7GiB RSS versus 0.46GiB for its g1 sibling and
  shared the executor with the slow close and net-escrow passes. The host had
  ample CPU and memory and the cgroup reported zero throttling, OOM, or
  pressure. The task-canary probe now applies the median-tail cap from §1.2,
  carries an internal exact-attempt correlation plus heartbeat executor identity, and includes the
  bounded Redis cleanup diagnosis. Its two synthetic regressions repeat the
  6,283s/42s/3,552s/6,220s shape and also prove that a 600s new attempt whose
  due time is 6,283s old is suppressed rather than misreported. The next
  production run supplied an independent memory control: task
  `<redacted-id>` ran on edge-4/g1 from 15:35:01Z to
  15:51:28Z and completed normally in 987.628s. Its process stayed near
  0.61–0.71GiB RSS (and was only 0.07–0.16GiB allocated heap during the
  earlier samples), while another executor's score export cycled through
  10–30GiB. Keep the bounded Redis latency fix, but do not attribute the score
  allocator's heap to this reaper.
  A subsequent reaper `<redacted-id>` began on
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
2. Name the refusal with the lossless API counter in §2.17. Per-pair API detail
   is V(1) only and may contain customer identifiers, so it is neither the
   default detector nor alert evidence. Use a bounded Mimir range to find the
   step-change and correlate it with the deploy/publication clock (§8).
3. Classify the path with bounded source/destination lifecycle, relationship,
   and resolution dimensions. `companion=false` names the original request
   bit, not the endpoint roles: provider discovery, provider return traffic,
   and same-network peer traffic can all create non-companion requests. Never
   export or paste raw client pairs to perform this classification.
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
`<redacted-id>` ran from 13:13:33Z to
13:13:55Z in 21.85s and corrected 115.91GiB over-reserved plus 2.37TiB
under-reserved across 2,047 networks. The monitor matched its dominant
under-reserved quantity to the preceding 2.70TiB over-reserved correction and
reported `reversal_direction=over-to-under`. A short inverse is causal evidence
for the stale absolute write, not a recovery certificate: 2.37TiB remains far
outside the 256GiB band. Follow the next scheduled aggregate, and start the
full three-emitter quiet interval only after a genuinely contracting pass.

The next two scheduled passes supplied that terminal certificate. Task
`<redacted-id>` completed at 13:19:25Z in 28.42s and
contracted to 66.33GiB over/42.61GiB under across 994 networks. Task
`<redacted-id>` completed at 13:24:45Z in 17.93s
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
Task `<redacted-id>` ran from 13:29:48Z to
13:47:40Z (1,071.29s), ending with 2.30TiB over-reserved and 148.44GiB
under-reserved across 2,107 networks. Six API settlements after completion
exposed exact -1MiB counters before the 18.42s successor reversed the dominant
quantity to 148.18GiB over and 2.30TiB under. A second 18.46s pass contracted
to 49.40GiB over/37.01GiB under, and all three emitters remained quiet for the
full following interval.

The next scheduled pass showed that convergence and fast successors still do
not make the deployed writer safe. The monitor followed task
`<redacted-id>` on
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
an attempt-correlation marker plus host/generation/container for active heartbeats, and source
host/generation/container for aggregate pairs. That
identity lets operators connect a long writer to sibling work and prevents one
fast executor from erasing it.

The scheduled successors supplied the correction sequence. Task
`<redacted-id>` ran on
`by-us-fmt-5-edge-3/g1`, container `786ae804bb97`, from 14:25:26Z to
14:25:47Z (20.92s) and reversed the dominant quantity to 122.19GiB over and
2.24TiB under across 2,083 networks. One exact -1MiB taskworker settle went
negative at 14:25:22Z—five minutes after the stale writer completed and four
seconds before the correction began—so it is aftermath of the stale mirror,
not damage caused by the corrective pass. Task
`<redacted-id>` then ran on
`by-us-fmt-5-edge-1/g2`, container `06abfbe03c32`, from 14:30:48Z to
14:31:10Z (21.46s) and contracted to 40.69GiB over/46.04GiB under across 921
networks. By 14:33:59Z taskworker, API, and Connect all reported zero negative
lines in the trailing eight minutes. Task
`<redacted-id>` supplied the terminal certificate on
`by-us-fmt-5-edge-4/g2`, container `3c0a752d4433`: it ran from 14:36:11Z to
14:36:29Z (18.00s), remained in band at 38.77GiB over/43.52GiB under across
847 networks, and all three emitters were still zero after the ingestion
allowance. The sequence therefore recovered without intervention, while the
recurring deployed fleet writer remains unsafe until the page-local additive
fix is rolled out.

The next sequence recreated the incident and supplied an executor control.
Task `<redacted-id>` ran on edge-3/g2 container
`4cf91fd25a2e` from 14:41:32Z to 14:59:35Z (1,082.77s). It rewrote 898,443
balances and reported 1.26TiB over-reserved plus 1.17TiB under-reserved across
2,237 networks; API and Connect each emitted negative-counter lines after the
apply. Its scheduled successor `<redacted-id>` moved
to edge-1/g1 and completed in 23.26s, immediately reversing the quantities to
1.18TiB over and 1.26TiB under across 2,290 networks. That fast inverse is both
the stale-write repair signature and an A/B executor control: fleet Redis and
PostgreSQL could complete the same deployed algorithm in seconds while the
edge-3 taskworker was co-resident with the long reaper, score rebuild, and
close checkpoint. The next scheduled pass
`<redacted-id>` moved again, to edge-0/g1, and ran from
15:10:02Z to 15:10:23Z (20.453s). It contracted the residual to 38.73GiB over
and 48.03GiB under across 873 networks. By 15:12:10Z taskworker, API, and
Connect all reported zero negative-counter lines in the trailing eight minutes,
and the next two samples remained quiet. That contraction plus the full
three-emitter ingestion window is the terminal recovery certificate for this
sequence; it does not make the recurring deployed fleet writer safe before the
page-local additive fix is rolled out.

The following recurrence reproduced the same causal inversion and exposed the
last cross-store race that remains after page-local reconciliation. Task
`<redacted-id>` ran on edge-1/g2 from 15:37:00Z to
15:54:51Z (1,071.751s), rewrote 898,581 balances, and reported 2.25TiB
over-reserved versus 129.43GiB under-reserved across 2,035 networks. Four API
and five Connect releases then exposed exact -1MiB negative counters; the
first was at 15:53:29Z while the stale apply was still live, and taskworker
emitted none. Its successor
`<redacted-id>` ran for 24.248s and reversed the
dominant quantity to 131.74GiB over/2.25TiB under across 2,050 networks. The
next scheduled pass `<redacted-id>` completed in
20.751s, contracted to 46.26GiB over/49.31GiB under across 945 networks, and
taskworker, API, and Connect were all at zero negative lines after the full
following interval and ingestion allowance. This is another terminal recovery
certificate for the legacy incident, not evidence that its full-fleet writer
is safe. Two further scheduled passes stayed in band at 52.59GiB/45.38GiB and
45.04GiB/55.28GiB over/under, respectively, while every emitter remained
quiet; the convergence was durable across executor changes.

The next recurrence completed the same long-run/reversal/convergence chain at
16:50Z. Task `<redacted-id>` ran on edge-1/g1 from
16:21:36.597190Z to 16:39:30.633361Z (1,074.036s), rewriting 898,952 balances
and reporting 1.11TiB over-reserved plus 1.81TiB under-reserved across 2,331
networks. Eleven settlements then exposed negative mirrors: seven API and four
Connect lines from 16:35:31.032Z through 16:43:16.640Z; taskworker emitted
none. The first three occurred while the stale apply was live, and the last
three Connect results on one balance reached -62.98MiB, -191MiB, and
-319MiB. Corrective task `<redacted-id>` moved to
edge-3/g1 and completed in 17.020s, reversing the dominant quantities to
1.81TiB over and 1.11TiB under across 2,317 networks. The next scheduled pass
`<redacted-id>` moved to edge-0/g2 and completed in
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
`<redacted-id>` then ran on edge-1/g1 from
17:16:43.785282Z to 17:17:29.346654Z (45.561s) and abruptly corrected
59.78GiB over plus 536.69GiB under across 1,570 networks. Its successor
`<redacted-id>` moved to edge-3/g2, completed in
18.761s, and reversed the same dominant quantity to 525.76GiB over plus
62.9GiB under across 1,571 networks. The monitor rendered
`matched_reversal=true reversal_direction=under-to-over` and retained both
executor identities. No settlement happened to expose a negative counter.
The next scheduled task `<redacted-id>` completed in
22.133s and contracted the aggregate to 50.5GiB over/30.4GiB under across 837
networks. This short-pass inversion is the exact synthetic aggregate
regression: skipping already-correct mirrors is required even when the full
fleet walk stays below 120 seconds. Another scheduled pass
`<redacted-id>` then completed in 17.653s and stayed in
band at 31.29GiB over/52.27GiB under across 858 networks. API, Connect, and
taskworker each remained at zero negative lines through 17:41:58Z, more than
eight minutes after that pass and beyond the ingestion allowance. This is the
terminal recovery certificate; it does not make a later legacy absolute walk
safe.

One more short legacy sequence reproduced the same matched reversal before
the rollout boundary. Task `<redacted-id>` completed
in 19.708s at 17:49:37Z with 19.37GiB over-reserved and 604.48GiB
under-reserved across 1,574 networks. Its successor
`<redacted-id>` moved to edge-1/g1 and completed in
23.294s at 17:55:01Z, reversing the quantities to 619.08GiB over and
16.87GiB under across 1,615 networks. The following pass
`<redacted-id>` completed in 16.821s at 18:00:19Z and
contracted to 29.53GiB over/51.05GiB under across 903 networks. API, Connect,
and taskworker remained at zero negative lines through 18:07Z. This is a
second short-duration proof that a quick full-fleet write can still clobber a
newer mirror; no-op skipping is part of the root fix, not only an optimization
for long passes.

The very next legacy pass escalated on the score-heavy edge-0/g1 executor.
Task `<redacted-id>` ran from
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
`<redacted-id>` ran from 18:25:35.186328Z to
18:44:36.869386Z (1,141.683s), rewrote 900,295 balances, and corrected
2.05TiB over-reserved plus 777.02GiB under-reserved across 2,469 networks.
Six exact -1MiB settlement negatives appeared across API and Connect from
18:44:15Z through 18:45:53Z, beginning while the apply was still live.

Its successor `<redacted-id>` ran from
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
`<redacted-id>` ran for about 108 seconds on old
edge-0/g1 and ended at 19:21:00.946518Z with 185.27GiB over plus 267.55GiB
under across 1,596 networks. Its approximately 23-second successor
`<redacted-id>` moved to old edge-1/g1 and ended at
19:26:25.921346Z with 268.32GiB over plus 186.55GiB under across 1,620
networks. The monitor preserved both executor identities and rendered the
near-exact 267.55GiB-under to 268.32GiB-over flip as
`matched_reversal=true reversal_direction=under-to-over`. Task
`<redacted-id>` then moved to old edge-0/g2, completed
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
`<redacted-id>` ran from about 19:42:13Z to
20:04:07.468260Z (an authoritative rounded 1,314s), rewriting 901,144
balances and correcting 438.07GiB over-reserved plus 5.54TiB under-reserved
across 2,238 networks. The redacted raw taskworker pull peaked at 3,181
negative settlements in minute 20:05; the standing monitor windows retained
289, 207, 133, and 81/min during the decay rather than losing the burst at a
snapshot boundary. Its same-executor successor
`<redacted-id>` was not a fast recovery: it ran 402s
and ended at 20:15:53.180148Z with 6.17TiB over plus 386.85GiB under across
2,405 networks. The monitor correctly rendered the 5.54TiB-under to
6.17TiB-over pair as `matched_reversal=true`, and its next full stream window
paged on another 1,280 taskworker negatives/min. This is a causal inverse, not
convergence. The next task `<redacted-id>` moved to
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
index, task `<redacted-id>` timed out on edge-3/g1 at
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
across that boundary. A narrow attempt-correlation Loki lookup timed out while awaiting
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
defense does not replace the page-local fix. The pre-rollout TiB-scale matched
reversals paired with exact legacy executor evidence prove the old absolute
writer; a matched inverse alone is not sufficient version attribution. The
clamp prevents either the legacy aftermath or a current cross-store race from
leaving available balance overstated until another pass.

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

A later single settlement diagnostic at 16:30:52Z supplied a small but
important alert-artifact regression. The bounded protected-log pull proved the
line carried `clamped_to=0`, but the monitor's generic 200-byte left truncation
ended immediately after the negative result and hid that suffix. The rendered
alert therefore told the operator to retain the clamp discriminator while
withholding it. Net-escrow samples now preserve an explicit trailing
`clamped_to=<value>` after ID redaction; a genuinely legacy line renders
`clamp_marker=absent`, so absence cannot be confused with truncation.

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
negative-counter interval.

Continued monitoring proved that the covering index is required as a second
access-path fix. At 08:16Z on 2026-08-31, a current bounded-lateral run was
still active after 951 seconds. Nine adjacent reservation-page calls averaged
7,216.1ms each, while the matching balance keyset calls averaged only 19.3ms.
The legacy `ANY` plan was absent. Production's `transfer_escrow` relation held
roughly 1.087 billion historical rows, but only about 1.85 million (0.17%) had
`settled=false`. All 1,726,789 rows joined to an `outcome IS NULL` contract were
unsettled, and zero open-contract escrow rows were marked settled. This matches
the write ordering: `claimContractOutcomeInTx` commits the non-NULL outcome
before the best-effort post can set escrow `settled=true`. Therefore
`outcome IS NULL => settled=false`, while the reverse is deliberately false
when a post is missed.

Migration 601 creates the online partial covering index
`transfer_escrow_unsettled_balance_contract(balance_id, contract_id) INCLUDE
(balance_byte_count) WHERE settled=false`. The reservation query adds
`transfer_escrow.settled=false` inside the lateral `OFFSET 0` boundary but
retains `transfer_contract.outcome IS NULL` as the authoritative reservation
test. The predicate reduces each balance range from complete history to the
small unsettled set; the included byte count removes the historical heap fetch
from the aggregation. Never replace the outcome join with `settled=false`
alone: a missed settled post can leave a closed row false. The `netescrow`
probe distinguishes old bounded-lateral calls from the new
unsettled-partial calls. Apply migration 601 before activating the matching
taskworker binary, then require new unsettled-partial delta calls, a page mean
below one second, complete runs below 120 seconds, small drift, and a full quiet
negative-counter interval.

The next production sequence exposed why matched reversals must retain exact
executor and statement-shape attribution. Current taskworker
`2026.8.31-outerwerld+1033599540`, task
`<redacted-id>`, ran for 1,282 seconds on
edge-0/g1 container `40f27d3cd53f`. At 08:21:42Z it reported 906,751
balances, 1,060 drifted networks, 158.60GiB over-reserved, and 686.26GiB
under-reserved. The same current container emitted many atomic
`clamped_to=0` settlement diagnostics during the run. Its scheduled successor,
task `<redacted-id>`, completed on the same executor in
165 seconds and at 08:29:30Z reported 906,827 balances, 544 drifted networks,
703.87GiB over-reserved, and 20.85GiB under-reserved. The roughly 686--704GiB
under-to-over inverse is a reconciliation-created stale write, but it cannot
honestly be attributed to the retired fleet-wide absolute writer: both halves
came from the current page-local additive executor.

The remaining race is between the PostgreSQL page snapshot and the later Redis
GET. A PostgreSQL statement fixes snapshot `P` before executing its reservation
query. If live creates `C` and settlements `S` commit and update Redis while a
multi-second page is still running, the later Redis GET sees `P+C-S`. The
additive correction computes `P-(P+C-S)=S-C` and moves the mirror back toward
stale `P`; the next pass sees current durable `P+C-S` and reverses nearly the
same quantity. Lua `INCRBY` correctly preserves writes after the Redis GET, but
cannot identify a write already visible at GET as newer than the earlier
PostgreSQL snapshot. The deterministic slow-page regression pins this exact
under-to-over signature.

Migration 601 and the unsettled-partial query are the immediate root-cause fix
for the measured amplifier: they reduce roughly 7.2-second historical range
walks to the small active set and therefore shrink the snapshot-to-GET window.
They do not make PostgreSQL and Redis atomic. After deployment, require
unsettled-partial pages below one second, scheduled passes below 120 seconds,
aggregates in the ordinary tens-of-GiB band, and a full quiet three-emitter
interval. If matched reversals persist on fast unsettled-partial pages, the
remaining correctness fix is durable per-balance fencing/versioning shared by
live mirror posts and reconciliation; do not mask it with manual reruns.

Deployment `2026.8.31-outerwerld+1033655820` initially appeared to supply the
access-path proof. Its first unequivocally post-rollout task,
`<redacted-id>`, ran on edge-3/g1 container
`3e5e8253a54e` and completed in about 15 seconds at 08:48:13Z. It scanned
907,015 balances but corrected only 499.65MiB over-reserved and 10.19GiB
under-reserved across 165 networks. The adjacent `pg_stat_statements` sample
recorded 91 new unsettled-partial calls at 61.7ms/page, versus the measured
7,216.1ms historical bounded-lateral mean that triggered migration 601.

Immutable artifact provenance later invalidated the executor attribution,
however. Both published architectures name server source revision `1d8f01e5`;
Git ancestry and the exact source object prove that revision has the historical
bounded-lateral query and excludes `93cbec80`, which added
`transfer_escrow.settled=false`. `pg_stat_statements` is database-wide and does
not carry container identity, so adjacent unsettled-partial calls can come from
another binary or a diagnostic and cannot override immutable artifact source.
The fast duration proves only that this particular historical pass completed
quickly on the then-hot/small working set. Require both provenance containing
`93cbec80` and new post-start statement calls before declaring the matching
taskworker query active; rollout percentage, duration, and database-global
counters alone are insufficient.

A later residual line at `19:04:04.427997Z` supplied the key-lifecycle
discriminator. The matching scheduled task ran from `19:03:53.330931Z` to
`19:04:07.730924Z` (14.400 seconds) and reported only 583.86MiB over-reserved
plus 628.14MiB under-reserved across 53 networks. Exactly one redacted
taskworker settlement emitted a -1MiB result with `clamped_to=0`; API and
Connect emitted none in the same 20-minute window. A later read found the
balance's Redis key present, but this was not a failed clamp: Redis held
1,233,055,744 bytes, exactly equal to the PostgreSQL sum across 64 current open
reservations, and every one of those reservations was created after
`19:04:41Z`, later than the negative line. The atomic script deleted the
nonpositive result at mutation time; subsequent legitimate traffic recreated
the mirror. Verification must therefore compare a present key with current
durable reservations. Requiring it to remain absent would misdiagnose healthy
recreation as containment failure. The single fast-pass line remains evidence
of the irreducible commit/post ordering window and keeps the quiet-interval
observation open; it is not a reason to redeploy the already-present access
path or clamp fixes.

A second independent control at `23:58:27.918855Z` reproduced the contained
clamp shape. One taskworker settlement reported `clamped_to=0`; API and Connect
reported zero negative-counter lines in the same 30-minute window. The
matching pass completed in roughly 12 seconds and corrected only 196.8MiB
over-reserved plus 1.15GiB under-reserved across 22 networks. Its successor
completed in roughly 19 seconds and corrected 1.53GiB over-reserved plus
196.8MiB under-reserved across 36 networks. Across that successor, the
database-wide `pg_stat_statements` counters added 183 unsettled-partial
reservation calls, zero legacy `ANY` calls, and about 26.0ms of reservation
query time per page. Those counters prove the fast query executed somewhere;
the later immutable provenance check proves they do **not** identify the
taskworker artifact that produced the aggregate. The apparent balance-count
jump from roughly 915,000 to 1.82 million was the UTC month boundary, not a
duplicate walk: 906,710 durable balances became active since the UTC day
boundary. The next scheduled pass reported after roughly 20 seconds with
1,821,933 balances, 31 drifted networks, 501.24MiB over-reserved, and 475.62MiB
under-reserved; taskworker, API, and Connect all remained free of negative
counter lines for that following interval. This verifies atomic containment
and a temporary quiet interval, not the provenance-excluded access path.

The same UTC boundary then exposed a separate, deterministic lifecycle gap at
`01:06:16Z`. Taskworker emitted 52 settlement diagnostics over 10.21 seconds;
every line carried `clamped_to=0`, API and Connect emitted none, and the 52
negative results totaled 586,862,592 bytes. A bounded read-only reconstruction
used those redacted-in-alert contract/balance pairs only inside the diagnostic
query. It found 425 contracts on 20 balances, all 20 balances active by byte
count but with the same `2026-09-01 01:00:00Z` end time. The contracts reserved
6,937,052,501 bytes, were all created after the preceding reconcile completed
at `00:57:40Z` (latest creation `00:59:59.674999Z`), and closed from
`01:06:15.298149Z` through `01:06:26.333917Z`. The first 373 releases consumed
6,216,738,133 bytes without going negative; the remaining 52 released
720,314,368 bytes. If every Redis release applied exactly once, reversing those
results gives a wave-start Redis total of 6,350,189,909 bytes, exactly
586,862,592 bytes below PostgreSQL, with all 20 balances short.

This kills the slow-page snapshot hypothesis for this cohort: no affected
contract existed during the preceding page reads. It also rules out a second
durable settlement claim, Redis eviction, Redis/node restart, cluster-role
failover, and Connect/API process restart. It does **not** rule out replay of a
Redis release: go-redis explicitly retries a failed pipeline as a whole, so a
connection lost after Redis applied a command but before its response reached
the client can execute the mutation twice. The old reservation and release
paths discarded pipeline errors, leaving both a missed/partial create increment
and a replayed release observationally possible. Therefore the reverse total
above is a conditional reconstruction, not proof of the wave-start counter.
The exact network transient and which of those two mechanisms occurred cannot
be reconstructed after the error was discarded, and must not be invented.

Current source uses a separate client with both go-redis command retries and
the server callback retry disabled for every non-idempotent reservation,
release, and additive reconcile mutation. It checks the pipeline result and
emits `netescrow-mirror-write` before raising an error; the mutation is never
blindly replayed. Read-only pipelines retain normal retries. PostgreSQL remains
the source of truth and a later independently recomputed reconciliation repairs
a missing or partially applied mutation.

A bounded production check on 2026-09-01 supplied the pre-rollout control.
Taskworker emitted exactly one 1MiB negative settlement at `10:45:58Z`, with
`clamped_to=0`; API and Connect emitted none in the same ten-minute window.
The adjacent scheduled aggregates remained fast and small compared with the
old TiB incident, but the `10:30Z` pass corrected 781.89MiB over plus 11.83GiB
under and the `10:35Z` successor corrected 11.81GiB over plus 783.89MiB under
before the next two passes converged below 1GiB in each direction. This is a
matched, sub-alert-threshold reversal: the atomic clamp contained the result,
but it does not identify a lost reservation increment versus a replayed
release. The live log's `subscription_model.go:760` call site and config
generation match tagged source `a52392db`; immutable source metrics were still
absent, so that is a source fingerprint rather than §8.12 proof. That source
predates the single-attempt checked-mutation fix in `39f662d2`. Deploy a clean
Taskworker descendant containing `66525afc` (which includes `39f662d2`) and
verify through a full expiry/close interval; do not manually reconcile or
replay the mutation to erase this control.

The reason any create-side shortfall survived until release is independently
deterministic.
`ReconcileNetEscrow` selected only balances satisfying
`start_time <= now < end_time`, while `CloseExpiredContracts` intentionally
waits at least five minutes after contract creation/expiry. The `01:02:55Z`
pass therefore excluded all 20 ended balances even though all 425 PostgreSQL
reservations were still open, and the delayed close exposed the full shortfall.
The root fix adds a second keyset pass over non-current balances that still own
`settled=false` escrow joined to authoritative `outcome IS NULL`. Production
read-only `EXPLAIN ANALYZE` used
`transfer_escrow_unsettled_balance_contract`, returned no current stragglers,
and completed in about 2.09 seconds without a history-table sequential scan.
The pass is proportional to the unsettled/open set, works for close
backlogs longer than any fixed grace window, and is applied by both fleet and
targeted network reconciliation.

`TestNetEscrowReconcileRepairsExpiredBalanceWithOpenEscrow` creates a live
contract, crosses only its balance end boundary, deletes the Redis mirror, and
requires both fleet and targeted reconciliation to restore the exact durable
reservation while ordinary active-balance selection returns zero rows. Keep
the single-attempt client configuration test and atomic clamp test as separate
guards. Verification requires a
taskworker artifact whose immutable source contains migration-601 query
`93cbec80` plus the non-current-open pass, observed statement calls for both
paths after that process start, one sub-120-second run, and zero negative lines
through the next balance-expiry/close boundary. Do not manually replay an
`INCRBY`: a pipeline error can follow partial application and blind replay can
double-reserve.

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
| clients-spike | redis | connected_clients own-node step and fleet shape; trip battery groups `CLIENT LIST` cohorts and samples HGET command rate | +50% in 10 min or >3× fleet median for 2 probes |
| pubsub-drops | logs | channel-is-full rate | > 10/min/service |
| connect-tls-disabled | logs | exact legacy Connect TLS-loader fallback that binds transport with no usable identity | any |
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
| grafana-plugin-unregistered | logs + Grafana `/api/ds/query` | 11.15 `[plugin.notRegistered]` scheduler/query failures | any |
| grafana-datasource-query | Grafana `/api/ds/query` | 11.15 authenticated `warp-mimir` or `warp-loki` control does not return a successful result after plugin loading | 2 consecutive probes |
| loki-tail-backend-eof | logs | §1.5 exact internal tail-querier `err=EOF` (client `context canceled` excluded) | >= 5/min/service |
| loki-tail-dropped-streams | logs | §1.5 exact ingester dropped-stream reset; raw observation service is not selector attribution | any |
| loki-tail-dropped-entries | logs | §1.5 exact privacy-safe, service-attributed Warpctl summary from either bounded HTTP tail loss stage | any |
| tailer-ipv6-route-loss | standing-tail stderr + monitor local IPv6 state | §18.1 exact `no route to host` reconnect, with same-window local default-router lifetime expiry and IPv6 loss as an affirmative monitor-first-hop discriminator | any |
| mimir-bucket-index-lag | logs | §11.18 store-gateway local/requested bucket-index difference; one-generation phase skew excluded | magnitude >= 1,800s, any line |
| mimir-index | host Mimir metrics | §11.18 per-process gateway sync/tenant coverage plus fleet compactor index freshness | gateway sync > 30m, discovered != synced, or writer index > 35m; 2 probes |
| mimir-ingestion-gap | raw Mimir range | §11.20 always-emitted build-info continuity across the public dashboard window | >= 3 missing 5-minute evaluations inside two present samples; any bounded gap |
| mimir-shutdown-flush-disabled / mimir-shutdown-child-missing | host Mimir config | §11.21 exact-process clean-shutdown flush setting, remotely reduced to one non-secret Boolean | any `flush_blocks_on_shutdown=false`; immediate; child absent for 2 probes |
| loki-tailers | host Loki metrics | §11.19 exact-process active-tail and active-stream accounting | either gauge missing, non-finite, or negative; any process |
| http-hijack-write | logs | §1.5 canonical net/http WriteHeader-after-Hijack recovery line | any |
| web-association-files | synthetic HTTPS | §19.1 Android assetlinks + Apple association documents pinned to every enabled edge and semantically decoded | any exact HTTP/contract failure; edge transport remains §18.1 |
| web-email-assets | synthetic HTTPS | §19.2 every image embedded by the transactional-email layout through the public website URL and pinned to each enabled edge under the same Host | any non-200, non-image, or empty response; exact edge transport remains §18.1 |
| pgbouncer-write-stall | logs+host | 2.11 app write timeout to `:6432` | any route/host cluster sustained 2 min |
| worker-memory-skew | mimir | 2.12 fresh taskworker allocated heap by host/block/instance | >= 8GiB and >= 4× fleet median for 2 probes; sparse-fleet fallback >= 16GiB |
| worker-cpu-allocation-churn | mimir+task logs | 2.12a paired one-minute taskworker CPU/allocation rates by host/block/instance | >= 3.8 cores and >= 256MiB/s and both >= 8× fleet medians for 2 probes |
| selection-stale | pg | 2.8 UpdateClientScores completion gap | > 90 min (page at > 3h — ttl cliff at 5h) |
| contract-balance-failure-rate | Mimir/Grafana | `urnetwork_connect_contract_failures_total{cause="insufficient_balance"}` 5-minute rate | > 4,000/min for 5 min |
| missing-origin-rate | Mimir/Grafana | `urnetwork_connect_contract_failures_total{cause="missing_companion_origin",companion="false"}` 5-minute rate vs its 128–201/min control band | > 500/min for 5 min; `companion=true` is not covered |
| keyevent-config-drift | redis | 9.1 notify-keyspace-events class SET per node | any node divergent from the fleet (all-off = healthy dark state) |
| pubsub-conn-shape | redis | 9.1 CLIENT LIST TYPE pubsub count per node | warn > 300; page > 1,000 (O(clients) = the v1 outage shape) |
| required-vault-resource | logs+route | 8.7 `Resource not found in vault` plus dependent-route probe | any active generation; payload includes resource, route, config generation |
| source-attribution | synthetic+logs | §8.8 dual-stack `/my-ip-info` family/source check plus UR-header resolver warnings | any mismatch for 2 probes, or any legacy untrusted-peer line after rollout |
| migration-schema-drift / migration-behind | pg | §8.9 successful `migration_audit` head cross-checked against every published schema artifact | page when any artifact at or below the recorded head is absent; warn while the database head trails this source tree |
| reliability-index-drift | pg catalog | §8.10 exact `client_reliability` parent/partition covering-index shape | warn while the old index remains, the desired index is absent/mis-shaped/invalid, or any partition child is absent/invalid |
| warpctl-provenance-invalid | local + managed-host executables | §8.13 exact Warpctl local-checkout base revision plus Boolean modified identity | missing/malformed revision or modified label; `modified=true` is valid; immediate |
| netescrow-reconcile-overrun | task logs+pg | 5.11 live heartbeat or completed ReconcileNetEscrow duration | >= 120s; retain completed precursor 45 min |
| netescrow-large-drift | task logs | 5.11 reconcile aggregate over/under-reserved correction | either direction >= 256GiB in the last 15 min; payload labels an adjacent opposite-direction quantity within 20% as a matched reversal |
| netescrow-negative | standing logs | `[netescrow]negative counter after` | any warns; >=100/min/service/site pages; payload includes site (never raw balance/contract ids) |
| netescrow-mirror-write | standing logs | `[netescrow]mirror write failed after` | any warns; never blindly replay the non-idempotent mutation |
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

### 8.1a Transparent services have no load-balancer status route

A service assigned only to transparent load-balancer interfaces cannot be
sampled through `https://<env>-lb/.../by/b/<service>/<block>/status`. Those
routes exist only on non-transparent load balancers. Proxy is in this class:
on 2026-09-01, the old `warpctl ls versions main proxy --sample` reached
edge-1 over IPv6 and received HTTP 404 for every block. A completed TLS/HTTP
exchange proved that IPv6 routing worked; the 404 meant the application route
did not exist. Do not classify that response as an edge-IPv6 failure.

Warpctl must fail closed at this observability boundary. `ls versions
--sample` now reports that status sampling is unavailable without issuing the
false requests. `deploy --only-older` refuses to deploy when a transparent,
non-standard, unexposed, errored, or empty sample prevents it from proving the
running version of every selected block. Never substitute the registry's
desired version for a live observation: that recreates the blind deployment
the option is meant to avoid.

For transparent services, verify convergence directly on each assigned host
from the current `services.yml` inventory: first require the remote hostname
to match, then read the running container image/digest, embedded source
revision, process start, and drain ancestors. A legacy address table or a
normal edge's 404 is neither host assignment nor runtime provenance.

The 2026-09-01 post-fix audit found the source closure ready but the tools
stale. Warp `217392e6` contains `2e13328`; its deterministic transparent
sampling tests pass, and a freshly built warpctl returned the explicit
unavailable-status result in 0.37 seconds without polling. The workstation
binary was last built on 2026-08-30 and lacked the fix string. The resident
warpctl binaries on edge-0, edge-1, edge-3, edge-4, Fireside, and Crisp all
shared one older digest and also lacked it. While edge-5 remains
operator-declared offline, run from an intentional local Warp checkout
containing `2e13328`:

```sh
xops/main/ansible/run-edges.sh --limit 'edges:!by-us-fmt-5-edge-5'
```

Rebuild the workstation warpctl separately. The playbook deployment does not
replace the developer binary under `warp/warpctl/build/darwin/arm64`. Verify
both binaries contain the transparent sampling boundary before using
`--sample` or `--only-older`; do not infer that an earlier `run-edges.sh`
execution included a later commit.

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
and the same attempt was already 337 seconds into its retry on another edge by
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
| 598 | idempotent repair of `transfer_escrow_balance_contract` |
| 599 | `migration_catalog` |
| 600 | `migration_catalog` identities cover indices 0–599 |
| 601 | `transfer_escrow_unsettled_balance_contract` |
| 602 | `client_reliability_running_window.degraded_classification_version` |
| 603 | `client_reliability_running_window.degraded_classification_write_token` plus its write-reset function and trigger |
| 604 | `provider_egress_health.tls_authentication_failure` plus its failed-client index |
| 605 | `st_fleet_binding_signature` plus its network-time index |
| 606 | `st_epoch_notification` |
| 607 | `network.points_leaderboard_public` |
| 608 | `network.emoji_tag` |
| 609 | `network_points_leaderboard_snapshot` |
| 610 | `network_points_leaderboard` |
| 611 | `network_points_leaderboard_pos_points` |
| 612 | `network_points_leaderboard_pos_blocks` |
| 613 | `network_points_leaderboard_pos_streak` |

Page immediately as `migration-schema-drift` when the successful audit head is
at or above an artifact's version but that artifact is absent. Warn as
`migration-behind` while the audit head is below `server.MigrationCount()` for
this source tree; never duplicate that count as a monitor constant. The
deployment gate is strict: run migrations from the exact service commit,
require the current head and all version-gated artifact checks, and only then
activate dependent APIs or taskworkers. Never edit `migration_audit` or create
objects by hand merely to silence the probe; repair the append-only migration
stream and let its normal runner advance the database.

At `2026-09-03T06:09Z`, production remained coherently at version 606 when
server commit `c42a4a4e` appended the all-time points-leaderboard schema through
613. No API or Taskworker artifact containing that commit had been activated,
so this was a valid pre-deployment gate rather than a live missing-object
outage. Versions 607 and 608 add the opt-in and emoji columns; 609 and 610 add
the snapshot and ranked-row tables; 611--613 add the three keyset-paging
indexes. Apply the exact append-only stream before deploying either the API
endpoints or the Taskworker snapshot builder. The probe must then verify every
published object, not infer coherence from numeric head 613 alone; each object
has a synthetic missing-at-head regression, and a version-606 fixture proves
future objects do not create false schema drift before migration.

The 2026-08-31 taskworker rollout proved why the monitor must derive that head
from code. Image `2026.8.31-outerwerld+1033599540` pulled and started on both
generations of every active edge, but each candidate correctly remained
unready with `database migration head 597 is below binary-required head 600`.
Warp retained the serving predecessor and retried the candidate; this was not
an image-pull, systemd, or host-specific stall. The monitor still carried a
hard-coded required head of 597 and therefore missed the exact release gate
the application enforced. The probe now calls `server.MigrationCount()`,
checks the version-599 catalog table and version-600 complete identity range,
and has a synthetic regression where 597/600 must alert. Apply versions
598–600 from that exact binary commit, then require both taskworker generations
to become ready before testing their task fixes.

### 8.10 Client-reliability covering-index drift after partition cutover
Probe: `reliability-index`

The partitioned `client_reliability` table has a physical optimization step
that is intentionally separate from ordinary schema migrations. Production's
partition cutover created
`client_reliability_valid_block_number_client_address_hash` before the score
query's covering payload was added. Changing the builder to
`INCLUDE (network_id, client_id)` did not change the already-created index:
`CREATE INDEX IF NOT EXISTS` checks identity, not definition, and cannot
reshape a parent index or its partition children.

The live 2026-08-31 signature was 29 identical `[crp]secondary index drift`
warnings in a bounded 30-minute taskworker window. They moved among every
active g1/g2 block because the recurring partition-maintenance task moves
between workers; they are repeated observations of one PostgreSQL catalog
state, not 29 independent defects. The old shape remains usable, so this is a
performance warning rather than data loss or a reason to stop taskworkers.
Without the INCLUDE payload, wide reliability score scans must fetch
`network_id` and `client_id` from the heap instead of remaining index-only.

The first live catalog probe then refined that warning at 16:42:34Z:
`client_reliability_valid_bnch_net_client` already had the exact covering
shape, PostgreSQL marked it valid, and all 34 table partitions had one valid
attached child. Only the old parent remained. That is finalization-only drift,
not evidence that the expensive partition builds still need to run. In this
state the supported command skips every child and attempts only its final old
partitioned-index drop under the existing 15-second lock timeout and bounded
retry policy. It still needs an operational window because the metadata drop
takes a lock, but it does not rescan or sort the 34 large partitions.

The 2026-09-03 reliability overrun made that unfinished finalization causal,
not merely theoretical. Both running Taskworkers on the executing host exposed
Server source `2d6f27c237d7c00d225ea45dab229dad12188e3d`; ancestry checks proved
that revision already contains the four-hour re-anchor, per-lookback
checkpoint, classification-v1, and mixed-rollout guard changes. The four
durable running-window rows all had version 1 plus guarded write tokens and
their committed heads were only 54–55 one-minute blocks beyond their last
re-anchor. Direct
`pg_stat_activity` instead showed an active `rolling-leave` UPDATE from edge-4
for more than 2,600 seconds and a reclaimed `rolling-enter` attempt from edge-1
waiting on its transaction ID. Historical statement stats put the rolling-leave
maximum near 30 seconds, separating this run from ordinary variance and from a
full anchor.

A bounded `EXPLAIN` without `ANALYZE` supplied the access-path discriminator:
for the same 30-block leaving slice PostgreSQL chose
`client_reliability_p20260903_valid_block_number_client_add_idx1`, the old
non-covering child, for an estimated 2,142,377 rows. The ready covering family
had all 34 valid attached children, but the old parent and children were still
eligible; live index statistics showed the old family used seconds earlier
while the covering family's last use was much older. Fetching `network_id` and
`client_id` from the heap explains the rolling regression. During that query,
edge-4 entered its scheduled reboot. The worker disappeared, but PostgreSQL
could not observe the dead client while executing and retained the UPDATE and
its transaction; the newly claimed edge-1 attempt therefore waited rather than
making independent progress.

This incident has three distinct closure boundaries. The software containment
is Server commit `fcb4de54`: each reliability checkpoint installs a
transaction-local two-hour PostgreSQL statement timeout matching the existing
task ceiling, so a server eventually cancels and rolls back an orphan even if
an abrupt worker-host loss removes the client context. Deploy it in Taskworker;
it cannot change the already-running incident backend, and unrelated pooled
sessions retain their configured timeout. The operational database fix is
still the existing idempotent
`bringyourctl model upgrade-client-reliability-index`, run only after the
protected measurement ends and explicit DBA authorization permits its final
metadata lock; no application deployment can remove the old parent. Future
scheduled host maintenance must drain task ownership before reboot so a
server-side statement is not orphaned behind a reclaimed lease. Recovery
requires every Taskworker artifact to contain `fcb4de54`, its checkpoint-local
timeout to leave unrelated sessions unchanged, the old parent to be absent,
the covering parent and all children to remain valid, a representative bounded
rolling plan to choose the covering child without the legacy heap-fetch path,
the old backend to reach a bounded terminal outcome, the blocked successor to
proceed, and later rolling cycles to return to their historical band. Do not
redeploy only for the already-present four-hour/checkpoint change, rebuild the
34 valid children, raise the task deadline, cancel a progressing statement, or
restart PostgreSQL to make the alert disappear.

The focused probe reads only `pg_catalog`; it never scans
`client_reliability` rows. Require all of these as one physical contract:

- `client_reliability` is partitioned;
- the old parent index is absent;
- `client_reliability_valid_bnch_net_client` exists, is valid, and has exact
  keys `(valid, block_number, client_address_hash)` with
  `INCLUDE (network_id, client_id)`;
- the desired parent has one valid attached child index for every table
  partition.

This alert requires an operational database-maintenance action that cannot be
completed by deploying application software. The safe implementation already
exists as `bringyourctl model upgrade-client-reliability-index`: it creates an
instant parent shell, builds each partition with `CREATE INDEX CONCURRENTLY`,
attaches the children, and drops the old parent only after PostgreSQL marks the
replacement valid. It is idempotent and resumable, but each child build still
scans and sorts a large partition and can perturb I/O, replication lag, and
free-space consumption. Do not start it while another protected measurement
must remain undisturbed. Wait for explicit maintenance authorization, choose
bounded parallelism, and observe those database resources throughout.

Do not run `CREATE INDEX` directly on the partitioned parent, drop the old
index before its valid replacement is complete, edit catalog state, or
deploy/restart taskworkers merely to silence the warning. If an interrupted
run leaves an invalid child or incomplete parent, rerun the supported command;
completed partitions are skipped. If the desired-name index exists with a
different definition, stop for DBA inspection before dropping anything.

Recovery requires the command's final-state check and the independent probe
to agree: the desired parent has the exact shape, its attached-child count
equals the table-partition count, every index is valid, the old parent is
absent, and no new `[crp]secondary index drift` warning appears for five
minutes after log-ingestion delay.

### 8.11 Fleet rollout serialization and worker freshness
Probe: `rollout-guard`

The host rollout lock is executed by every long-running Warp service worker,
not by proxy alone. Installing a corrected `/usr/local/sbin/warpctl` does not
change code already mapped by API, Connect, taskworker, proxy, or another
running worker. Probe every enabled host with the `services` role and require
both the executable capability and the worker lifecycle to agree:

- `rollout_guard=full-overlap`: the installed binary contains the host-lock
  timeout path introduced by Warp commit `7e2075c`; the lease begins before
  candidate start, remains held through synchronous old-container drain, and
  refuses a replacement when acquisition times out.
- `rollout_guard=drain-only`: the binary contains the legacy
  `Draining %d overlapping container(s) (staggered=%t)` path. That lease covers
  only drain, so independent workers can start almost a complete duplicate
  fleet before any drain serializes. WARN `rollout-guard-stale`.
- `rollout_guard=disabled`: at least one managed unit sets
  `WARPCTL_STAGGER_HOST_DRAIN=0`. WARN `rollout-guard-disabled` even if the
  executable contains the fix, and name every disabling unit.
- A missing or unrecognized executable is `missing` or `unknown`. WARN
  `rollout-guard-unverified`; do not discover its behavior by launching a
  production rollout.
- For every running managed unit, compare its systemd
  `ExecMainStartTimestamp` with `/usr/local/sbin/warpctl`'s inode-change time.
  WARN `rollout-guard-workers-stale` when a worker started before the installed
  binary changed or either timestamp cannot be verified. A new on-disk binary
  with old resident workers is not a deployed fix.

This signal intentionally owns rollout-guard classification for the whole
services fleet. Proxy memory §14.7 still records the guard in incident evidence
because it changes the safe OOM/UDP response, but it does not emit duplicate
guard alerts. Hosts without any enabled or running environment-scoped Warp
unit are not managed by this signal; availability and inventory probes own the
absence. Disabled inventory entries are never contacted.

**Live deployment audit (2026-08-31):** edge-0, edge-1, edge-3, edge-4, Crisp,
and Fireside were the six enabled managed-services hosts. Every installed
binary exposed only `drain-only`. Edge-5 was explicitly disabled/offline and
was not contacted. Edge-6 had no managed Warp service units, while edge-2 and
Snow did not carry the managed-services role, so none belongs in this probe's
target set. Repository HEAD `a85a277` contains root fix `7e2075c`; its focused
race-enabled host-lock/config-validation tests and full `go test ./...` passed,
but that source result is not evidence that any installed worker runs it.

A config-only Proxy control on 2026-09-01 proved that “service release” includes
both identities from §8.6. The service image stayed on
`2026.8.31-outerwerld+1033797570` while config advanced to
`2026.8.31+1034210530`; every legacy worker nevertheless entered the same
candidate-start path. The host lock must therefore cover candidate creation for
a service-image change, a config-generation change, or both. An unchanged
application image is not evidence that rollout memory is unchanged.

This is a software deployment and operational-restart gate, not a hardware
capacity alert. Deploy validated Warp commit `a85a277` or later, remove every
disabling override, and restart every running Warp service worker. Adding RAM
does not close any of these guard classes. Conversely, serialization cannot
create RAM, CPU, host slots, or proxy active-client capacity; §14.7 hardware
alerts remain open when a serialized old/candidate pair or the steady client
load does not fit.

Verify without creating the unsafe condition: every managed services host
reports `full-overlap`, zero stale workers, and zero unverifiable workers. Then
use one controlled ordinary service replacement to show overlap remains within
the configured host bound. Do not validate by launching a full proxy-fleet
rollout. Implementation convention: SIGNALS.md §8.11 (`rollout-guard`) maps to
`signal_rollout_guard.go` and `signal_rollout_guard_test.go`. Synthetic cases
cover legacy, disabled, missing, unknown, healthy full-overlap, an on-disk fix
with stale/unverifiable workers, partial host failure, a managed host with no
units, and exclusion of non-services hosts.

### 8.12 Fleet service artifact provenance
Probe: `provenance`

A desired version, a healthy route, and a source checkout do not prove which
executable a running process contains. Query Mimir once per minute for the
newest actual-scrape-fresh `api`, `connect`, `competitionworker`, `proxy`, and
`taskworker` identity in each `(job, host, block)`. Join these four families on
exact `env`, `job`, `host`, `block`, and `instance` labels:

- `process_resident_memory_bytes` is the live-process denominator;
- `process_start_time_seconds` selects the newest overlap generation;
- `urnetwork_build_info.version` preserves the mutable config annotation; and
- `urnetwork_source_info` carries the full Go VCS `revision`, `modified` bit,
  and exact OCI `image_digest` that Warp inspected and executed.

Filter every family independently with its source timestamp no older than 90
seconds before joining. An instant Prometheus result can otherwise give a
stopped lookback series the current query timestamp. A fresh RSS identity that
omits process start remains an explicit unknown: dropping it would let the
generation-selection prerequisite hide itself. When a new long-running Go
service adopts `StartStatsPusher`, add its job to this probe's explicit service
set; exporters and short commands are not deployment denominators.

WARN `service-provenance-unobservable` when the newest identity lacks process
start, build info, or source info. WARN `service-provenance-invalid` when the
source family has anything other than a full 40- or 64-hex Git object ID,
Boolean `modified=true` or `modified=false`, and `sha256:` plus 64 lowercase
hex characters. The modified bit records intentional local checkout state; it
is not itself invalid.
WARN `service-provenance-conflict` when one valid immutable digest maps to more
than one `(revision, modified)` tuple across fresh identities. The conflict is
stronger than version skew: one content digest cannot legitimately describe
two executables. Stop promotion and preserve the raw series until direct
container and extracted-binary inspection identifies collector-label,
platform-manifest, or runtime-injection drift.

Do not substitute `WARP_VERSION`: it can change in a config-only rollout. Do
not substitute an image tag, desired registry entry, current git checkout, or
BuildKit context attestation either. `urnetwork_source_info` comes from the
running executable's `debug.ReadBuildInfo`; `WARP_IMAGE_DIGEST` is injected
only after Warp pulls and inspects the exact image it passes to `docker run`.
Both halves are required, and the reported digest must independently match the
running container and the executable extracted from that digest.

The production discriminator was a Taskworker release on 2026-09-01. Six
directly observable blocks on enabled edges 0/1/3 executed config digest
`sha256:042255119828a004024a4dc5e57d97373a8bf399aca6074ca98804dec2b3156a`.
Its extracted binary reported base revision
`078d6c1117bd8537a47b1933301e546cf500cf90` with `modified=true`, while the
image's SLSA Docker context identified `a52392db`. Direct symbols and SQL
proved the intended retry-jitter change was present, but neither revision
alone described the binary. Exact replay of a modified build therefore also
requires preserving its local diff. Edge-4's two blocks remained provenance-unknown
because their digest could not be inspected without authorized Docker access;
they were not silently counted observable. This exposed a shared-worktree
attribution boundary: the Linux binary was compiled from one modified state
and copied into an image whose later build context described another state.

**Live pre-gauge audit (2026-09-01 07:05 UTC):** the new signal selected 68
newest fresh processes through one services gateway: 20 API, 20 Connect, 20
Proxy, and eight Taskworker identities. All 68 had fresh RSS, process start,
and build info, and all 68 lacked `urnetwork_source_info`; no current
Competitionworker entered the denominator. This is a clean legacy-instrumentation
boundary, not evidence that any binary is malformed. Deploy each of those
four service artifacts from a local checkout containing the source-gauge commit
before attempting to classify its revision/digest. The probe contacted no
disabled edge and does not need direct access to edge-5 for this metric join.

**Live post-gauge audit (2026-09-01 23:19 UTC):** ordinary Proxy and
Taskworker deployments reduced the missing set from 68 to 40. All 20 newest
Proxy and eight Taskworker identities now had complete source/digest families;
the remaining set was exactly all 20 API and all 20 Connect identities across
beta and g1-g4 on enabled edges 0/1/3/4. Direct Warp status reported code
version `2026.8.31+1034210530` for every API and Connect block while their
config annotation had advanced to `2026.9.1-outerwerld+1035001930`. The code
version maps to server `a52392db`, which predates source-gauge commit
`236bf0ce`; the newer config cannot add a metric owned by the executable. Build
and deploy API and Connect from an intentional local checkout containing
`236bf0ce`. Do not
redeploy the already-complete Proxy or Taskworker solely for this class.

Warp commit `217392e` temporarily enforced a clean-tree release gate. Operator
policy subsequently confirmed that local repository checkouts, including
deliberate uncommitted changes, are the deployment source, and current Warp
commit `8797d48` intentionally removed that gate. The durable monitor contract
is therefore identity, not cleanliness: require a full base revision, a
Boolean modified bit, and the executed image digest; preserve the local diff
when `modified=true`; and alert only on missing, malformed, or conflicting
identity. Hardware cannot repair those identity failures, and a redeploy is
not justified solely by the mutable config version when direct behavior already
proves a fix present.

Recovery requires two consecutive complete identity scrapes, followed by direct
inspection of the exact running container digest and the binary extracted from
that digest. SIGNALS.md §8.12 (`provenance`) maps to
`signal_provenance.go` and `signal_provenance_test.go`; synthetic cases pin a
mixed-service fleet, draining-generation suppression, missing and stale source
families, a fresh RSS identity without process start, accepted modified source,
malformed provenance, and a conflicting digest.

### 8.13 Warpctl local-checkout executable identity

Probe: `release-builder`

Section 8.12 detects an unverifiable artifact after it is running. The release
control must also identify the executable capable of creating the next one.
Every five minutes, inspect the exact `warpctl` resolved on the monitor/release
host and `/usr/local/sbin/warpctl` on each enabled managed-services host. Scan
the Go build settings directly from that executable with bounded binary-safe
`grep` patterns; do not depend on optional binutils packages or infer them from
a checkout, desired tag, install timestamp, or a different Warpctl copy.
Disabled inventory hosts are never contacted.

The local repositories are intentionally the authoritative deployment source,
including deliberate uncommitted changes. This control neither requires nor
downloads a published or cached Warpctl. Emit `warpctl-provenance-invalid` only
when the exact executable lacks one full 40- or 64-hex Git base revision or its
`vcs.modified` field is not the Boolean string `true` or `false`. A missing or
unreadable executable remains `cannot-observe`. `modified=true` is valid
identity context and must not produce an alert.

The modified bit does not name the participating diff. When exact replay or
incident attribution matters, preserve the owning local checkout and record
its diff alongside the artifact digest. A desired version, install timestamp,
checkout HEAD viewed later, or Docker/BuildKit context attestation is not a
substitute for the exact executable's base/modified tuple. Local and managed
copies can legitimately identify different checkout generations between
operator runs, so this probe does not invent a fleet-equality requirement.

The 2026-09-01 production discriminator joined all three boundaries. Running
Taskworker image digest
`sha256:042255119828a004024a4dc5e57d97373a8bf399aca6074ca98804dec2b3156a`
and Proxy image digest
`sha256:1ec9a150c148f0aa32fafc636241c5e7561a645546c31f4a0dab54ee4c35683c`
both contained executables with base revision `078d6c11` and
`vcs.modified=true`, even though their mutable version was
`2026.8.31+1034210530`. The revision was not available in the current server
repository. Edge-0's exact installed Warpctl was instead a clean
`42168fe8` executable, but it predated and lacked every `217392e` release
guard. The workstation Warpctl resolved by the monitor was older still and
reported `vcs.modified=true`. Thus neither the tag, the later image context,
nor a different clean launcher proved the service executable's source. Under
the current policy, the valid modified tuple was not itself broken; failure to
retain its diff was the remaining exact-replay limitation.

Warp `217392e` temporarily introduced three fail-closed clean-tree guards.
Operator policy later affirmed local checkout deployment and current Warp
`8797d48` intentionally removed those guards. The former
`warpctl-release-guard-missing` class is withdrawn; a new watcher emits a
healthy compatibility tombstone so persisted state from an older monitor can
resolve rather than linger.

This establishes an ownership boundary, not a ban on correctness fixes. The
monitor agent should repair correctness defects within the existing product
architecture, but a provenance finding does not authorize it to change how
builds work or redesign deployment. Clean-tree requirements, stable-HEAD build
gates, rejection of `modified=true`, and checkout-versus-binary admission gates
are architecture decisions requiring explicit operator direction. Exact
identity remains useful read-only evidence; the intentional local checkout and
its diff remain valid build inputs.

This is software identity and release-operations visibility, never a hardware
alert and never a prohibition on local changes. If identity is malformed,
rebuild the workstation copy through the current local
`warp/warpctl/Makefile`, or rerun `xops/main/ansible/run-edges.sh` to build and
install managed-host copies from the current local Warp checkout. Do not
substitute a published/cached Warpctl or discard a desired diff merely to clear
the warning. The playbook can restart resident Warp workers, so retain §8.11's
separate mutation and worker-freshness gates. Verification requires every exact
path to report one full revision and either Boolean modified value; §8.12 then
matches each running service's independently emitted tuple and image digest.

SIGNALS.md §8.13 (`release-builder`) maps to
`signal_release_builder.go` and `signal_release_builder_test.go`. Synthetic
coverage pins a parseable fleet, an accepted modified local builder with no
guard strings, malformed identity, partial host observation loss,
services-role scoping, and parsing of the actual executable-string shape.

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
loki flushes on `chunk_idle_period`/shutdown. Mimir normally uploads a complete
TSDB block at roughly a two-hour boundary, so an empty Mimir bucket immediately
after the first healthy start can be expected. That does **not** make a partial
head disposable. Warp intentionally does not mount the Grafana bundle's data
directory: old and new deploy generations overlap and must not write the same
WAL/TSDB. Mimir's default `blocks_storage.tsdb.flush_blocks_on_shutdown: false`
assumes the incomplete head will be reused from persistent disk after restart;
container removal instead erases it here. Every generated Mimir config must set
`flush_blocks_on_shutdown: true`. The Grafana parent gives its Mimir child a
120-second stop allowance, inside Warpctl's normal 3,600-second container drain,
so clean shutdown can upload the partial head to this MinIO bucket. Do not
repair this by sharing one TSDB mount between overlapping containers. See §11.20.
An empty/stale Loki bucket while Loki is crash-looping = writes stopped
(11.2/11.4), not a storage bug.

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
14. some unrelated metric panels have identical historical holes, while data
    exists on both sides of each hole → raw Mimir continuity loss (11.20).
    Query the always-emitted build-info control directly through Mimir and
    correlate gap endpoints with Mimir starts. Do not zero-fill, span-null, or
    rewrite the throughput query: those treatments turn missing observations
    into false traffic measurements.

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

### 11.15 Grafana 13 datasource rows without native plugins (2026-08-29, 2026-09-01, 2026-09-02)

Probe: `grafana-datasources`

Grafana 13 packages the formerly core Prometheus and Loki datasource
implementations as standalone native plugins. A provisioned datasource row and
an installed app plugin do not supply either implementation.

The 2026-08-29 failure retained the provisioned `warp-mimir` row and green
front/backend health, but omitted the Prometheus plugin. The alert scheduler
retried each rule on every Grafana host, producing roughly 220–250
error-shaped lines/minute:
```
error="the result-set has errors that can be retried: [plugin.notRegistered] plugin not registered"
```

The 2026-09-01 Loki failure had the same shape. Direct Loki and `warpctl logs`
returned fresh `web_page_view` events, and `/api/datasources/uid/warp-loki`
returned the expected provisioned row. Grafana's installed-plugin list
contained the Logs Drilldown app and the Prometheus datasource, but no native
`loki` datasource plugin. The Web Analytics query through `/api/ds/query`
returned HTTP 404 `plugin.notRegistered`; Logs Drilldown displayed "no Loki
datasource configured" and emitted a URL with an empty `var-ds=`. Its
provisioned `jsonData.dataSource: warp-loki` was already correct: Logs
Drilldown is an app frontend and only considers a datasource usable after
Grafana registers type `loki`.

On 2026-09-02, one browser-driven `/api/ds/query` on the current edge-1
container returned HTTP 404 `plugin.notRegistered`, while that exact container
still ran both native plugin processes and authenticated `warp-mimir` and
`warp-loki` controls succeeded on every serving exact edge. The live
`urnetwork-connect` dashboard also referenced only `prometheus/warp-mimir`.
This is the discriminator the generic log class had been missing: the line
proves a request-level registry lookup failure, not which plugin was requested
or that the image omitted a required plugin. Preserve its bounded request path
and referer. The `grafana-datasources` probe owns the packaging diagnosis; when
both controls are healthy, inspect stale browser/dashboard payload state or a
different unsupported request type instead of rebuilding the image or
recreating a datasource.

The decisive check is an authenticated query through Grafana itself, not a
direct storage read:
```
# controls authenticated against a live Grafana child/front
POST /api/ds/query  warp-mimir / prometheus / vector(1)
POST /api/ds/query  warp-loki / loki / sum(count_over_time({service="web"}[1m]))
# broken: HTTP 404/500 or an embedded result error containing plugin.notRegistered
find /var/lib/grafana/plugins -maxdepth 2 -type f \
  | grep -E '/(prometheus|loki)/'
```
- A datasource database row proves only configuration. Direct Mimir success
  or fresh direct Loki events prove only storage/query health. Neither proves
  Grafana can instantiate the datasource implementation.
- Runtime plugin preinstallation is deliberately disabled so readiness does
  not depend on internet access. The image fix in `warp/grafana/Dockerfile`
  bakes Prometheus plugin 13.1.7 for both amd64 and arm64 with catalog-published
  SHA-256 checksums and Loki datasource plugin 13.1.0 with its per-architecture
  catalog checksums. `grafana/prometheus_plugin_test.go` and
  `grafana/loki_datasource_plugin_test.go` hold those offline invariants.
- Warp deployment readiness now submits both bounded controls through
  Grafana's local `/api/ds/query` boundary. A new artifact cannot take over
  merely because `/api/health`, Loki, and Mimir are green while a datasource
  plugin is absent.
- The Grafana front redirects only the exact Logs Drilldown GET route when it
  carries an explicit empty `var-ds=`, replacing that stale override with
  `var-ds=warp-loki` while preserving every other query variable. URLs with a
  valid datasource, and URLs that omit the variable so app provisioning can
  choose the default, pass through unchanged.
- The standing `grafana-datasources` probe submits the two controls separately
  through the public Grafana boundary every minute. It reads the existing
  Grafana admin credential from `vault/<env>/grafana.yml`, sends it only as
  in-memory Basic Auth, bounds the response, and raises
  `grafana-plugin-unregistered` immediately when Grafana names that failure.
- Verify every active exact-edge generation with successful results for both
  controls, Web Analytics view panels populated from `web_page_view`, Logs
  Drilldown selecting `var-ds=warp-loki`, successful evaluation of one
  provisioned rule, and zero new `grafana-plugin-unregistered` lines after
  ingestion delay. Do not silence query errors, recreate either existing
  datasource, or restart the same image as remediation.

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

### 11.17a Host-local Grafana LAN and ring health

Probe: `grafana-node`

Public exact-edge health and a successful `warpctl logs` query can select a
healthy Grafana replica. They do not prove that every host in the active
`services.yml host_services` placement still owns the LAN identity advertised
by its Grafana/Loki/Mimir children. Derive the Grafana host role from that
active placement rather than duplicating it in `monitor.yml`, then check each
host independently once per minute:

- the exact LAN IPv4 from `config/<env>/settings.yml routes` is present on a
  global-scope interface;
- `networkctl` reports zero failed links, so an ad-hoc or retained address
  cannot masquerade as durable network ownership;
- an interface-scoped `warp-<env>-grafana-*-g1.service` is active;
- the Mimir scheduler accepts TCP on the exact LAN identity and port 6490;
- the PostgreSQL LAN endpoint accepts TCP when a primary is configured; and
- loopback Mimir answers the data-independent `vector(1)` with HTTP 200 in
  under four seconds.

PAGE `grafana-lan-identity` when the configured address is absent. A socket can
remain bound with non-local/freebind semantics and the Warp unit can remain
active, so neither listener output nor a prior `Deploy success` clears this
class. WARN `grafana-networkd-link` when the address exists but networkd still
reports a failed link; this prevents a temporary `ip address add` from closing
the durable configuration gate. PAGE focused `grafana-node-unit`, `grafana-ring-local`,
`grafana-database-path`, or `grafana-node-query` for the later boundaries only
when the preceding identity checks pass. `vector(1)` reads no customer series;
its timeout is a query-control-plane failure, not an expensive workload query.

The `2026.8.31+1034210530` Grafana verification exposed the missing-node case
on 2026-09-01. Crisp answered `vector(1)` in about 1.6ms. Fireside accepted TCP
on loopback 3100 but did not return even that query within six seconds. Its
current Grafana, Loki, and Mimir children were all running and had started at
02:29Z; the Warp unit recorded `Deploy success`, so another image rollout was
not a discriminating action. Direct host state instead showed that Fireside's
`eno3` had only IPv6: configured LAN IPv4 `192.168.51.196/24` and its connected
LAN route were absent, while Mimir still listened on the missing address.
Current logs repeatedly timed out dialing its own scheduler at that identity,
and Grafana alert evaluation timed out reaching the LAN PostgreSQL endpoint.

The same missing identity blocked the in-progress Proxy rollout rather than
identifying a second Proxy-image defect. Crisp completed all ten blocks on
`2026.8.31+1034210530`, while Fireside completed only g1, g2, and g10. Each
remaining Fireside candidate timed out reaching PostgreSQL/PgBouncer and Redis
over the missing LAN route, exited before its status check, and left the
previous serving process in place. Restore and verify the host-network path
before retrying those seven blocks; forcing the same candidates or rebuilding
the already-successful image cannot make their dependencies reachable.

The retained journal pins the causal sequence to the earlier unsafe Proxy
overlap rather than this Grafana image. During the second global-pressure
window, `systemd-journald` flushed caches at 12:47:01Z, 54 seconds before
`systemd-networkd` timed out setting the NDisc address at 12:47:55Z. Journald
reported pressure again two seconds later and repeatedly through 12:53:24Z,
when the kernel's global OOM killed a roughly 5.5GiB Proxy. Networkd never
restored the DHCP LAN address; the live link remained `routable (failed)` with
only IPv6. This exact pressure-before/NDisc/pressure-after/OOM ordering, not the
mere presence of events somewhere in a 72-hour count, ties the secondary
host-network failure to the §14.7 memory-capacity incident and explains why
Fireside metrics disappeared from Mimir while direct Proxy processes continued
running.

The probe therefore carries the last NDisc epoch, nearest pressure epochs
before and after it, and first following OOM epoch in addition to counts. It
labels pressure causal only when a pressure event precedes the NDisc failure by
at most ten minutes and either another pressure event follows within ten minutes
or an OOM follows within fifteen minutes. Otherwise the 72-hour events remain
context and the networkd failure is diagnosed independently. A deterministic
six-hour/20-hour negative control rejects the former count-only false
attribution.

There are two required fixes. First, deploy Warp's serialized candidate-start
guard (§8.11/§14.7) so a Proxy image or config-generation rollout cannot create
the global-memory precursor. Second, restore Fireside's exact LAN address only
after checking that no other MAC owns it, then deploy the xops netplan change
that pins stable service-host LAN identities statically instead of depending on
a renewable DHCP lease. This second step is an **operational host-network
change**; redeploying Grafana cannot add the address. Do not restart Grafana as
the first action. The scoped xops entry point is `run-edges.sh --limit <host>
--tags netplan_config`, one host at a time, first Fireside and then Crisp. That
tag copies `/etc/netplan/0-by.yaml`; it does not activate netplan, so use the
approved controlled activation or reboot procedure and verify each host before
advancing. After address recovery, require local scheduler and database TCP
success, three fast `vector(1)` responses on both Grafana hosts, fresh metrics
from both host labels, and no address loss through a controlled Proxy rollout.

The controlled activation completed on 2026-09-01, Fireside first at 04:12:44Z
and Crisp only after Fireside passed at 04:14:34Z. A live ARP check found no
owner for Fireside's `.196`; before Crisp activation, Fireside resolved `.198`
to Crisp's exact `eno3` MAC. Both hosts then held their configured address with
infinite lifetime, a connected LAN route, `routable (configured)` state, and
zero failed networkd links. Scheduler and PostgreSQL TCP passed, as did three
local `vector(1)` queries per host (all under 7ms). Each local Mimir frontend
returned fresh `urnetwork_build_info` for both `fireside` and `crisp`. The eight
standing Loki tails made one reconnect wave as the ring paths reconfigured and
then remained connected; the authoritative monitor emitted no further
`grafana-node` alert through multiple one-minute cadences. This clears the
host-network incident, while the separate Proxy memory/headroom and controlled
rollout verification gates remain open.

SIGNALS.md §11.17a (`grafana-node`) maps to `signal_grafana_node.go` and
`signal_grafana_node_test.go`. Synthetic cases preserve the causally bracketed
OOM/networkd/LAN-loss signature, unrelated pressure-event negative control,
inactive unit, missing local scheduler, missing database path, trivial-query
timeout, a healthy node, and exclusion of non-Grafana hosts.

### 11.18 Mimir bucket-index and store-gateway freshness

Probe: `mimir-index`

Grafana's parent, front proxy, and `/api/health` can all stay green while one
bundled Mimir child has stopped discovering blocks. Query success through a
sibling also cannot prove that every store-gateway owns a current view. This
probe enumerates loopback listeners on every active services host, identifies
Mimir through `/api/v1/status/buildinfo`, and reads the exact child's
`/metrics`; it does not trust a parent PID or a rotating public route.

The relevant Mimir loops are deliberately independent. Before Warp `13fcd05`,
store-gateways used Mimir's default 15-minute block-view refresh with 20%
jitter. That commit renders a one-minute refresh for this single-tenant fleet;
the compactor cleanup loop still writes the per-tenant bucket index every 15
minutes with 10% jitter. A querier
passes its most recently discovered index `updated_at` to a store-gateway in
gRPC metadata, and Mimir 3.1.1 defers the comparison until that RPC returns.
Therefore `ours < requested` is not itself a failed query. One process can
legitimately be exactly one generation behind another while both loops are
healthy.

The metric probe applies Mimir's operational freshness bands rather than
alerting on any difference:

- `cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds` must
  be no more than 30 minutes old on every established child.
- `cortex_bucket_stores_tenants_discovered` must equal
  `cortex_bucket_stores_tenants_synced` on every ready child.
- the fleet maximum of
  `cortex_bucket_index_last_successful_update_timestamp_seconds` for every
  privacy-safe tenant identity must be no more than 35 minutes old. A newly
  started child gets the matching startup grace, but an established fleet
  with discovered tenants and no writer metric is broken.
- a timestamp more than 30 seconds in the future is clock skew, not
  freshness. Missing/unreadable child metrics are explicit observation
  failures rather than zero values.

The conservative 30-minute gateway band remains compatible with rolling older
generations; on `13fcd05`, direct verification is stricter and requires every
gateway below two minutes. The 35-minute writer band covers two nominal
15-minute compactor updates plus the documented buffer. Both remain below the
querier's default one-hour `max_stale_period`, after which Mimir fails a query
rather than silently using an arbitrarily stale index. Do not increase that
period to turn a stopped writer into a green dashboard.

The 2026-08-31 production audit separated normal phase skew from a real
incident. Grafana embeds Mimir 3.1.1. A bounded 30-minute query contained 410
actual `bucket.go` warnings (plus four harmless Grafana query-echo records),
and every actual warning had exactly `diff=-873`. Counts by observation host
were edge-0 7, edge-1 187, edge-3 63, edge-4 56, Crisp 47, and Fireside 54.
Every `ours`/`requested` pair was exactly one successive index generation, and
the generations themselves advanced every 873 seconds.

An independent direct-metrics control at `2026-08-31T22:55:26Z` excluded the
known-offline edge-5 and checked all six active Grafana hosts. Every child
reported Mimir 3.1.1, one discovered and one synced tenant, zero compactor
cleanup failures, and a last successful gateway sync less than 14 minutes
old. The current compactor owner's bucket index was about 803 seconds old.
Gateway Series RPCs were succeeding across the fleet; a single cumulative
historical `Unknown` on edge-1 among 9,138 successful calls had no matching
current error. Separate two-hour queries contained zero `err-mimir-*` and
zero Grafana `status_code=5` records. The `-873` warning is consequently
healthy cache phase skew, not a stopped compactor or failed query.

The implementation control exercised the embedded remote shell, not only its
Go parser: a production-shaped fixture with unlabelled gateway gauges failed
the draft `{label=...}`-only filter and now guards both labelled and
unlabelled Prometheus forms. A focused production CLI run at `23:07:54Z`
queried all six active services hosts, skipped operator-disabled edge-5, and
returned no Mimir alert. Monitor v162 (binary SHA-256
`515dc4182be11a088415ac6bf3f85593495b6a261ac751140181f7a075814855`)
became the sole standing watcher at `23:10:56Z` after its first full cadence
retained all service tails and emitted no `mimir-*`, `cannot-observe`,
`tailer-silent`, `tailer-restarting`, or `tailer-reconcile` finding. The
predecessor watcher then exited cleanly. Observation loss also has a dedicated
synthetic invariant: it cannot invent or resolve the shared writer aggregate
when the possible compactor owner is unreadable.

The standing `mimir-bucket-index-lag` log class deliberately ignores that
historical one-generation control. It alerts only when `diff <= -1800`, retains
the exact host and generation as its frame, and preserves `ours`, `requested`,
and `diff` in evidence. A warning at that distance means two nominal writer
generations under the old default, or many missed one-minute syncs on
`13fcd05`, have separated the views. Correlate it with
`mimir-store-gateway-stale`, `mimir-store-gateway-tenants`,
`mimir-bucket-index-stale`, and `err-mimir-store-consistency-check-failed`
before assigning cause.

Mimir 3.2 replaces this 3.1 warning with a version-difference histogram; every
3.1 patch release retains the warning. A feature upgrade may improve
observability later, but it is not remediation for a healthy `-873` line.
For a real alert, inspect only the framed replica first: object-store reads,
ring ownership, bucket synchronization, and tenant loading. For a stale shared
writer, find the current compactor owner and its cleanup/object-store errors.
Never restart all replicas together, suppress all bucket warnings, or raise
the staleness tolerance. On `13fcd05`, verification requires ten minutes with
every gateway below two minutes, the writer below 35 minutes, complete tenant
coverage, and zero multi-generation warning or consistency error. Retain the
30-minute alert band during a rolling transition; it is not the post-deploy
performance target.

### 11.19 Loki live-tail accounting integrity

Probe: `loki-tailers`

The live-tail data path has three different boundaries: the external
WebSocket, each querier's fan-out to the ingesters, and the querier's own
accounting. A connected external `warpctl logs -f` process proves only the
first. The `loki-tailers` probe enumerates loopback listeners on every active
services host, identifies exact Loki children from their Prometheus metric
family, and records `process_start_time_seconds`, version,
`loki_querier_tail_active`, and `loki_querier_tail_active_streams`. Process
start keeps two valid rollout generations separate.

Both active gauges are cardinalities. They must be present, finite, and
non-negative. A negative value is an instrumentation invariant violation; it
does not mean there are negative real tails and does not itself prove that a
log entry was lost. Once either gauge is invalid, it cannot be summed across
the fleet to prove how many collectors or selectors are active. Use
`tailer-silent`/`tailer-restarting` for the external stream and
`loki-tail-backend-eof`, `loki-tail-dropped-streams`, and
`loki-tail-dropped-entries` for affirmative data-path loss.

The 2026-08-31 `23:18Z` production read found the exact impossible shape after
a standing-monitor handover. The six enabled Grafana hosts exported active
tail/active-stream pairs of `8/8`, `-16/-16`, `0/0`, `-16/-20`, `0/0`, and
`0/0`; edge-5 remained operator-disabled and was not contacted. The fleet sums
were therefore `-24/-28`, so they could not prove whether the 81-second
two-monitor overlap doubled Loki fan-out. The positive `8/8` process did
independently match one standing tail for each of the monitor's eight service
selectors after the predecessor exited.

The sole v163 watcher then supplied a clean one-hour lifecycle control. It
started at `23:50:39Z` with exactly eight service tails and no collector
overlap. Edge-0 first became invalid at `00:51:27Z`, immediately after
`tail_max_duration`, on the same Loki process (`process_start=1788162008`):
`tails_active=-8 streams_active=-9`. Eight real tails decremented twice explain
the exact active-tail value: `+8 - 16 = -8`. After v163 exited, the sole v164
watcher attached its eight service tails to that unchanged process; by
`01:00:12Z` the pair was `0/-1`, an exact `+8/+8` transition. New live tails
therefore masked eight prior over-decrements in the first gauge while the
sibling gauge still exposed the underlying residue. This rules out a scrape
parser, process restart, or fleet-sum artifact and shows why zero in one gauge
cannot repair or validate the other.

The root cause is deterministic in Loki 3.7.3 (`82cdcdc0`). `newTailer`
increments both accounting families for a new tail. The loop calls
`Tailer.close()` when the configured one-hour `tail_max_duration` expires or
when all ingester clients have closed. `TailHandler` also unconditionally
defers `tailer.close()`. The close function has no once/compare-and-swap guard
and decrements on every invocation, so the internal close followed by the HTTP
defer subtracts the same lifecycle twice. The official Loki `main` source at
`95d8e7f5` on the same date retained the same non-idempotent path; upgrading to
an unverified newer binary is not a fix.

This is software-owned. Warp commit `ba01c98` builds the checksum-pinned Loki
3.7.3 source in the Grafana image and makes the metric decrement and iterator
close idempotent with `sync.Once`; its image build runs an upstream-package
regression that closes one two-stream tail twice and requires both gauges to
finish at exactly zero. Deploy a Grafana image containing `ba01c98` or later
(and therefore the earlier Loki idle-tail transport fix `1e95aef`). Preserve
the one-hour lifecycle limit and HTTP cleanup. Do not clamp negative
exposition, lengthen `tail_max_duration`, or restart a healthy process merely
to reset a gauge; those actions erase evidence while leaving the next
double-close intact. After every Grafana block converges, verify the gauges
remain non-negative through two one-hour rotations and that the separate
dropped-stream/EOF signals remain clear.

### 11.20 Mimir historical sample continuity across restarts

Probe: `mimir-continuity`

The public `live throughput` panel is a fleet-wide rate over
`urnetwork_connect_exchange_io_bytes_total`; the query and panel are valid.
On 2026-09-01 its gaps were not specific to that counter. Direct public and
loopback Mimir range queries found the same missing intervals in independent
Connect resident-client metrics and taskworker provider/network gauges. In the
representative `00:00Z` through `04:30Z` range, raw Mimir returned the selected
dashboard series at `00:00Z`, `04:20Z`, and `04:30Z` only.
The always-emitted build-info control resolved ten bounded gaps in the trailing
seven days, including a short island from `02:30Z` through `02:40Z`; the gap
endpoints aligned with replacement Mimir fleet starts. Repeated restart loops
extended some holes. This rules out zero traffic, counter reset alone, Grafana
rendering, a single producer, and the Grafana datasource plugin.

The root cause is the boundary between local ingestion and durable object
storage. Each service pushes its default Prometheus registry every 15 seconds.
Mimir ingesters retain recent samples in their local TSDB head/WAL until a
complete block is uploaded to MinIO. The generated config used
`/var/lib/mimir/tsdb`, the active Warp service definitions deliberately set
the Grafana data mount false, and Mimir's `flush_blocks_on_shutdown` default is
false. That default is safe only when the incomplete TSDB survives and is
reused after restart. Here, removal of the old container removed the unshipped
head. A rolling fleet restart eventually removed every replica that still held
the same interval, making the historical samples permanently unavailable.

The `mimir-continuity` probe directly queries Mimir, not Grafana, for
`sum(count_over_time(urnetwork_build_info[2m]))` at five-minute steps over the
same trailing seven-day window. Build-info is emitted regardless of traffic,
so a bounded absence between present evaluations means raw observation loss.
Leading and trailing absence is ignored because it can describe a new
environment or normal ingestion delay; one or two isolated missing evaluations
are tolerated. Three or more missing evaluations inside two present samples
emit `mimir-ingestion-gap`, preserving all gap ranges and the serving gateway.

The causal defect is software-owned, but a historical continuity alert does not
unconditionally request another Grafana deployment. Pair it with the §11.21
exact-process signal. Any current child rendering false requires a Grafana/Warp
image from an intentional local checkout containing `7176ccd`. When every current child
already renders `blocks_storage.tsdb.flush_blocks_on_shutdown: true`, the old
hole is expected to remain visible and is not evidence that the fix is absent.
Preserve the setting through the next ordinary rollout and use that clean
shutdown as the discriminator. The Grafana parent gives Mimir a 120-second
child stop allowance inside Warpctl's normal 3,600-second container drain. Keep
the TSDB directory ephemeral: a persistent directory shared by overlapping
old/new containers can introduce concurrent WAL/TSDB writers and corruption.
Do not zero-fill or span-null the dashboard, restart the same artifact, or
reinterpret a hole as zero throughput. Existing gaps cannot be recovered from
Mimir and will age out of the dashboard range.

The first deployment of this fix still terminates old Mimir children whose
rendered config lacks the flush. If preservation of their current partial head
is required, an operator must explicitly flush those old ingesters before
rollout; this is a production mutation and the monitor never performs it.
Verification requires the setting in every active block followed by a
controlled and then full Grafana rollout with no new bounded build-info gap.

### 11.21 Mimir shutdown durability configuration

Probe: `mimir-shutdown`

The historical range signal in §11.20 proves that data was lost, but source
code, a desired image tag, and an earlier gap do not prove what each current
Mimir child will do at its next shutdown. This probe enumerates loopback TCP
listeners on every enabled `services` host and requests each local `/config`
endpoint. Rendered Mimir configuration can contain object-store credentials,
so the remote command selects exactly one Boolean
`flush_blocks_on_shutdown` field and emits only the port and that Boolean. The
full response never leaves the host. A bounded four-host fan-out and the shared
per-host SSH limiter preserve the monitor's admission limits.

HEALTHY: every enabled services host exposes at least one Mimir child and every
observed child renders `flush_blocks_on_shutdown: true`. During overlap, both
old and new generations must be checked independently. BROKEN: any exact child
renders false (`mimir-shutdown-flush-disabled`, immediate). A host with no
matching child emits `mimir-shutdown-child-missing` after two probes. An SSH or
parse failure remains `cannot-observe`; it must not clear an existing fleet
alert. A confirmed false sibling is still reported when another host is
unobservable.

On 2026-09-01, a direct read of the sole active Grafana block on every enabled
services host returned false: edge-0 `:14819`, edge-1 `:14819`, edge-3
`:14818`, edge-4 `:14819`, fireside `:14818`, and crisp `:14818`. Edge-5 was
offline and disabled in the active inventory, so it was not contacted. This
proves the production gap was not fixed at that observation time. Warp commit
`7176ccd` renders the required true value and has a deterministic config test,
but source presence is not deployment evidence.

The later `2026.9.1-outerwerld+1035004200` deployment changed that current-state
gate. At `23:17Z`, a direct privacy-reduced read found exactly one Mimir child
on each enabled services host and `flush_blocks_on_shutdown=true` on all six:
edge-0 `:14819`, edge-1 `:14819`, edge-3 `:14818`, edge-4 `:14819`, fireside
`:14818`, and crisp `:14819`. The running parent had already been extracted as
clean Warp `71731e4`, a descendant of `7176ccd`. Edge-5 remained disabled and
was not contacted. This closes the current configuration/deployment gate; it
cannot restore the ten historical §11.20 gaps. The next ordinary Grafana
replacement, including a deployment of later Warp `13fcd05`, must preserve the
true setting and produce no new bounded build-info gap through the following
block-upload window.

This alert is software-owned. First use §8.13 to record the exact local-checkout
Warpctl identity, then build and deploy Grafana from an intentional local Warp
checkout containing `7176ccd`. Keep each generation's TSDB private and ephemeral; never
shared-mount a WAL/TSDB directory into overlapping containers. Retain the
120-second Mimir child stop allowance and the outer 3,600-second Docker drain.
The generated systemd unit's separate 60-second timeout applies to the Warpctl
controller; controller shutdown deliberately leaves service containers running,
so that value does not truncate a normal container drain and must not be
misdiagnosed as the gap cause. The first rollout begins by terminating old
children whose setting is false; preserving their current partial heads
requires an explicit operator-controlled flush before replacement.

Verification has two layers. First, require the exact loopback value to be true
on every active block. Then perform a controlled Grafana replacement and the
full rollout, and require §11.20 to produce no new bounded build-info gap
through the next restart and block-upload window. Historical holes are
unrecoverable from Mimir and clear only when they age out of the seven-day
dashboard range; their continued presence is not evidence that a successfully
verified rollout regressed.

### 11.22 Planetoid backup archive completeness and freshness

Probe: `backup-archives`

Planetoid is the offsite recovery boundary for four independently completed
archives: `pg`, `redis`, `github-urnetwork`, and `github-urfoundation`. Their
writers atomically replace final artifacts and publish two Prometheus textfile
families: `urnetwork_backup_archive_latest_timestamp_seconds` carries the
completed file timestamp plus generation, while
`urnetwork_backup_archive_in_progress` carries one Boolean per archive. Fluent
Bit reads those files and remote-writes them with `env`, `host`, and `job`
labels. Grafana is only a renderer; a blank panel is not enough to distinguish
no completed backup from a missing collector.

Code recovery points use the same sortable UTC naming convention as database
recovery points: `main-code-urnetwork-YYYY-MM-DD-HH-MM-SS.tar.xz` and
`main-code-urfoundation-YYYY-MM-DD-HH-MM-SS.tar.xz`. PostgreSQL, Redis, and
each code organization follow the same retention contract: four newest
complete generations under `latest/<type>`, the newest promotion candidate
under `staging/<type>`, four generations under `week/<type>` selected after a
seven-day window, and four under `month/<type>` selected after a 30-day window.
Retention copies are hard links so the same recovery point consumes space once.
For code, `<type>` is `code` and the organization remains in the filename. The
first updated writer run atomically migrates existing flat dated code tarballs
from `code/` into `latest/code/`; it still recognizes a legacy fixed-name
tarball as the latest generation until a dated run succeeds.

Redis's exported `generation` is intentionally the recovery-point identifier
`main-redis-YYYY-MM-DD-HH-MM-SS`, not a literal filename. The complete stored
generation is a pair named `<generation>.tar.gpg` and
`<generation>.tar.gpg.sha256`; omitting `.tar.gpg` from the Grafana label does
not mean the encrypted archive is an unpacked directory.

Planetoid also publishes physical allocation telemetry for the mounted archive
volume. `urnetwork_backup_archive_storage_bytes{archive="pg|redis|code"}` is
the total used by each class, with hard-linked retention copies for all three
classes deduplicated and the persistent Git mirror cache included in `code`.
`urnetwork_backup_archive_volume_size_bytes` and
`urnetwork_backup_archive_volume_free_bytes` are the filesystem total and free
bytes, and `urnetwork_backup_archive_storage_timestamp_seconds` proves when the
bounded scan completed. The archive Grafana dashboard renders the three class
totals beside free space; validate those values against one direct `du`/`df`
sample on Planetoid before treating the panel as capacity evidence.

The `backup-archives` probe queries raw Mimir through a reachable loopback
Grafana service gateway for every monitor-inventory host with the `backup`
role. It also reads `github-backup-archive.service` state and MainPID plus the
effective `remote-backup-archive.service` active state, substate, MainPID,
result, exit status, restart policy, restart delay, and its four non-secret
PostgreSQL/Redis source endpoint values directly on that backup host. From the
same effective unit it reads `BRINGYOUR_BACKUP_MOUNT`, then uses `mountpoint`
and direct `findmnt` source, filesystem type, and options to classify that exact
destination as missing, read-only, read-write, or unknown. In particular,
ext4's `emergency_ro` overrides a simultaneous top-level `rw` mount flag. The
probe also queries the producer-owned
`urnetwork_backup_archive_heartbeat_timestamp_seconds` values alongside the
progress gauges. It expects exactly the four archive names above.
Samples older than 90 seconds are observation loss even when their archive
timestamp value is old. The newest valid generation is selected during the
short Mimir staleness overlap after a label change. Metric values must be
finite, generations must be non-empty, archive and heartbeat timestamps may
not be more than five minutes in the future, and each in-progress gauge must be
uniquely present and equal to zero or one. The query and direct discriminator
carry no credentials, repository names, private key paths, or backup contents.

HEALTHY: all four in-progress samples are scrape-fresh; all four archives have
at least one complete generation; and each newest completion is no more than
five days old. While the GitHub unit is active, its MainPID is nonzero, both
producer heartbeat values are no more than 90 seconds old, and exactly one
GitHub organization gauge is one; while it is inactive or failed both are zero.
The data-pull oneshot has effective `Restart=on-failure` and
`RestartUSec=30min`, so a late encrypted-disk mount can recover without making
successful pulls repeat. The effective data-pull unit matches the two
dedicated direct SSH endpoints and ports recorded in monitor inventory. These
bulk paths deliberately bypass the `172.28.*` management VPN; the monitor
itself may still use that VPN to inspect Planetoid. An executing data pull has
a nonzero MainPID. Otherwise its last invocation has `Result=success` and
`ExecMainStatus=0`; `ActiveState=activating` with `SubState=auto-restart` and a
zero MainPID is a failed attempt waiting for its retry, not active progress.
The configured archive path is a real mountpoint whose direct options contain
`rw` and contain neither `ro` nor `emergency_ro`. Serial queue and active-phase
attribution are valid only while this volume contract is healthy.
BROKEN:

- `backup-archive-metrics-missing` after two one-minute probes means the
  producer, textfile collector, authenticated remote-write path, or Mimir
  ingestion is absent. A fresh `node_uname_info` control does not clear this
  class because it comes from a different collector.
- `backup-archive-metrics-invalid` is immediate for an ambiguous progress
  series, a value outside `{0,1}`, an empty generation, or an impossible
  timestamp. Do not coerce these values in Grafana.
- `backup-archive-missing` after two probes means no atomic completed
  generation is observable through the latest-timestamp metric. It can mean no
  artifact exists, or that a pre-fix writer overwrote its off-volume metric
  while the archive was unavailable and discarded the last-known row. Resolve
  any volume alert and inspect the root-owned latest tier before choosing
  between those states. `in_progress=1` makes a first run operationally
  pending; it does not create a recovery point. Never infer physical absence
  from metric absence or manufacture a timestamp.
- `backup-archive-stale` is immediate once the newest real completion is more
  than five days old. A current scrape of an old value proves the telemetry
  path while reporting an expired recovery-point objective. When
  `in_progress=1`, preserve the single active writer and compare increasing
  receive bytes plus source backlog with sustained direct-transfer throughput.
  A transfer is not stalled merely because its atomic final timestamp has not moved. When
  a stale `pg` or `redis` gauge is zero but the direct data-pull unit is active
  and its sibling gauge is one, frame it as `queued-behind=<sibling>`: the
  script serializes those two sources, so zero describes the next phase rather
  than an idle scheduler. Never start a second catch-up run for that phase.
  This queue interpretation is forbidden when the direct archive volume is
  missing, read-only, or unknown; an old `in_progress=1` and live PID do not
  prove that rsync retains a writable destination.
- `backup-archive-volume-unavailable` is immediate when the unit's configured
  archive path is not a mountpoint, its options contain `ro` or
  `emergency_ro`, or its writable state cannot be established. Stop treating
  phase gauges as progress. With explicit operator authority, stop both writer
  units before touching storage, identify the current partition by stable LUKS
  UUID rather than mutable `/dev/sdX`, inspect USB/UAS, block-I/O, and SMART
  evidence, repair or replace the proven physical fault, close only a stale
  unmounted mapper, unlock the current volume, run `e2fsck` offline, and mount
  normally. Never live-remount an aborted ext4 journal read-write or let a
  writer target the bare directory beneath a missing mount.
- `backup-archive-progress-stale` after two probes means direct systemd state
  and the two exported GitHub phase gauges disagree, the active unit has no
  MainPID, or its producer heartbeat value is absent or more than 90 seconds
  old. Fluent Bit gives every reread a fresh scrape timestamp, so raw-Mimir
  sample freshness alone cannot disprove source-file staleness; the heartbeat
  must be checked as a metric value.
- `backup-archive-retry-disabled` is immediate when the effective data-pull
  unit is not configured with the bounded on-failure retry above. This is a
  software policy failure even when its precipitating missing mount needs an
  operator or hardware repair.
- `backup-archive-run-failed` is immediate when the last data-pull invocation
  failed or is waiting in `auto-restart` with no MainPID. Preserve its rsync
  partial and the single-writer boundary, inspect the bounded journal and both
  direct endpoints, and observe the scheduled systemd-owned retry. A retry
  policy is recovery machinery; it does not make the failed attempt healthy.
- `backup-archive-source-route` is immediate when either effective source
  target or port differs from its exact monitor-inventory direct SSH endpoint,
  or when either source uses `172.28.*`. This is a configuration failure even
  when the management VPN is reachable; bulk PG/Redis payloads must not use it.

The 2026-09-01 blank-dashboard incident had two distinct layers. Planetoid's
ordinary `node_uname_info` arrived through the new VPN Grafana publisher, both
root-owned `.prom` files were readable by the `fluent-bit` user, and a bounded
stdout-only Fluent Bit 5.1.1 run parsed and emitted all six current archive
series. The long-running unit emitted none. An exact synthetic discriminator
preserving the classic-config value's outer quotes enabled only the middle
`uname` collector from `"cpu,uname,textfile"`: Fluent Bit retained the quote
bytes and interpreted the boundary names as `"cpu` and `textfile"`. Removing
the wrapping quotes enabled `textfile` immediately. Xops commit `19f8123`
contains the unquoted config and a boundary-token regression. After
`run-planetoid.sh` applied it, the process started at `2026-09-01T22:29:55Z`;
both Fireside and Crisp then returned four current progress series and two
completed-generation series directly from Mimir. This fixed observation and
dashboard input, not the underlying backup age.

The newly visible state showed real operational failures. The PostgreSQL and
Redis values still named August 20 generations, outside the five-day band.
Kernel history established the first cause: on August 21 the Thunderbolt/PCIe
dock carrying the archive device disconnected (`pciehp Link Down`, USB and
`enp65s0` removal, followed by ext4 journal I/O errors). The archive device and
udisks mount then remained unavailable across the August 25 through September
1 daily attempts. The September 1 04:00 run waited the full 900 seconds for
`/run/media/by/archive1`, then exited. Its effective properties were `ActiveState=failed`,
`Result=exit-code`, `ExecMainStatus=1`, `Restart=no`, and `RestartUSec=100ms`.
The disk appeared later, but a persistent daily timer does not revisit an
already-consumed trigger, so the failed oneshot remained failed. Xops commit
`2311114` adds `Restart=on-failure` with a 30-minute delay. It retries only a
failure; successful pulls still stop. Deploy that unit with
`main/ansible/run-planetoid.sh` after the active GitHub writer safely finishes,
and authorize any immediate catch-up separately. The timer remains scheduled
for 04:00, while that session mount is not guaranteed at boot. The first GitHub job began at
`2026-09-01T21:30:55Z`; it mirrored all 47 urnetwork repositories (about 69.9
GB) and remained in its atomic compression phase with no final organization
tarball at the observation boundary. Preserve that job rather than restarting
it merely to make the panel change.

A 2026-09-02 preflight initially misclassified the dedicated public SSH
forwards as stale configuration and Xops commit `fbd291a` moved both bulk pulls
onto `172.28.*:22`. That was incorrect: Planetoid is offsite, and the intended
data paths are the direct forwards at `65.49.70.73:8022` for PostgreSQL and
`:8023` for Redis. The management VPN is for control traffic and must not carry
the archive payload. The monitor inventory, service template, and archive
script now encode and validate that direct-path contract, including an explicit
rejection of `172.28.*` sources. Installing the corrected unit changes only
future processes; an already-running transfer retains the environment it
started with and must not be interrupted without operator authorization.

Read-only direct source listings expose complete August 27 and August 30
PostgreSQL and Redis generations while the mounted archive still exposes
August 20, so only a successfully completed transfer plus artifact/manifest
validation can close freshness. The first September 2 attempt had already
started through the erroneous VPN configuration. It received about 9.18 GB
before OpenVPN went down at 13:38Z; rsync failed at 13:56Z, four bounded retries
could not reach either VPN source, and the unit resumed only after tunnel
recovery. That sequence proves the VPN route itself can prevent publication of
an otherwise healthy source generation. It does not authorize an unscheduled
or duplicate catch-up transfer.

An additional metrics-writer race can falsely show an active GitHub job as
idle: the standalone `--refresh-metrics` process initializes both shell-local
in-progress values to zero. An unconditional Ansible refresh invoked alongside
the oneshot therefore overwrote its live `1`. The durable playbook contract is
to read `github-backup-archive.service` state and skip the standalone refresh
while it is active, activating, or reloading. The running writer remains the
sole owner; its exit path publishes zero. This is a visibility fix and must not
be treated as completing either tarball.

The guard prevents another writer from clobbering an active phase, but it
cannot repair a file that was already overwritten. After the guarded
`run-planetoid.sh` on 2026-09-01, direct state still showed the original
activating PID `156738` with live `tar` and two-thread `xz` children, while
`backup-archive-code.prom` retained mtime `2026-09-01T22:29:50Z` and both
organization gauges at zero. Mimir kept returning those zeros with fresh
scrape timestamps. Xops commit `2733b0b` fixes that causal gap: the sole owning
shell atomically republishes its current phase and a producer-owned timestamp
every 30 seconds, then cancels the heartbeat helper before publishing the
transition or final zeros. Carrying freshness as a metric value avoids requiring
the unprivileged monitor SSH identity to traverse Fluent Bit's root-owned
textfile directory. A running pre-fix shell cannot inherit newly installed
script behavior. Preserve the healthy active compression, install the fixed
script for the next generation only when deployed provenance predates that
commit, and never restart the current job, rerun an already-current playbook,
or hand-edit the metric merely to change the panel.

Ownership is deliberately split. The quote bug, refresh race, atomic writers,
bounded failed-pull retry, writer-side read-write preflight, and monitor
detector are software/configuration work. Making the archive disk available
before unattended timers and
authorizing a catch-up run are operator work. A persistent mount may require an
approved fstab/systemd-mount design; replacement media and additional free
capacity are hardware work. Software cannot attach an absent disk or create
archive capacity. Never fabricate a timestamp, rename a partial artifact,
refresh a stale value, or raise the five-day threshold to clear these classes.

The September 2 VPN run measured only about 0.95 MiB/s against roughly 705 GiB
of complete PostgreSQL source data. That is evidence about the prohibited VPN
path, not a valid capacity benchmark for the dedicated direct forwards. First
deploy the direct-endpoint unit for the next process and measure that path. If
source backlog divided by sustained direct throughput still exceeds five days,
preserve partial rsync state and have operations provision a faster offsite
path or approved offline seed. WAN bandwidth, attached media, and physical
archive capacity remain **network/operations or hardware closures**, not
software-only fixes.

The same run also establishes the serial queue contract. Direct systemd state
remained `activating` while the fresh PostgreSQL in-progress gauge was one and
the Redis gauge was zero. Redis was not an idle or independently failed job: it
was queued behind PostgreSQL inside the same authorized writer and could not
start until that transfer and rotation completed. The stale Redis alert must
therefore preserve the PostgreSQL phase and require Redis to transition to one
without a second unit generation. Generic “start a catch-up run” guidance at
this boundary is a monitor defect because it risks concurrent writers.

The operator-authorized 2026-09-02 route cutover preserved that single-writer
boundary. The VPN-backed process with PID 262449 was stopped, its rsync partial
state was retained, and `run-planetoid.sh` installed the direct endpoint unit.
The explicitly restarted process had PID 278173 and resumed the same PostgreSQL
phase through `65.49.70.73:8022`; no archive payload socket to either
`172.28.208.182:22` or `172.28.208.177:22` remained. Over one 15-second direct
socket sample, received bytes rose from 4,824,292,818 to 5,017,877,410, about
12.3 MiB/s compared with the earlier 0.95 MiB/s VPN sample. This proves the
route and resumability fix, but a short throughput sample is not a completed
recovery point: freshness still closes only after the unit succeeds, validates
the artifact and manifest, transitions through Redis without a second writer,
and publishes current completed generations.

A second separated 15-second sample held about 12.5 MiB/s. The direct source
then contained 757,154,670,694 PostgreSQL bytes across the August 27 and August
30 generations, while Planetoid's resumable partial held 30,542,826,944 bytes;
the queued Redis source held another 90,962,003,055 bytes. Holding the measured
rate would put the remaining PostgreSQL transfer near 15.4 hours and both
phases near 17.3 hours before validation/rotation overhead, well inside the
five-day objective. This is a capacity projection, not recovery proof. Continue
measuring the same socket and PID; only a material sustained-rate drop that
moves the projection outside the objective reopens the network/hardware branch.

At `2026-09-02T20:33:16Z`, that direct PostgreSQL session was reset after rsync
had received 90,338,645,957 bytes. The unit then tried the separate Redis
forward and it was reset immediately before an authenticated session reached
edge-6. Neither source host nor either SSH daemon restarted, the archive
remained mounted with about 5.4 TiB free, and both direct ports again returned
SSH banners during the investigation. This localizes the interruption to the
shared public-forward path, but does not yet distinguish router restart,
conntrack eviction, or another upstream reset. Rsync retained the partial and
systemd correctly scheduled its 30-minute retry. Direct state during that wait
was `ActiveState=activating`, `SubState=auto-restart`, `MainPID=0`,
`Result=exit-code`, and `ExecMainStatus=1`; the prior probe emitted only stale
generation alerts because it treated `activating` as sufficient evidence of a
live writer. The `backup-archive-run-failed` class and MainPID-gated queue
attribution make that failed-attempt boundary explicit. Do not add a second
writer or abandon the direct path: observe the scheduled retry, preserve the
partial, and escalate the shared router/network boundary if progress cannot be
sustained.

The source-side SSH journals make that boundary stronger. Edge-2 recorded the
Planetoid public address authenticating the PostgreSQL rsync at
`18:36:15Z`, but recorded no orderly disconnect or PAM session close for that
sshd process when Planetoid received the reset almost two hours later. Edge-6
recorded no non-monitor authentication for the Redis attempt that was reset two
seconds later. In contrast, both hosts continued accepting management sessions
through the same interval and retained their original boot generations. Thus
the backup client did not independently terminate both transfers and neither
source sshd rejected them: state disappeared before the first packet of the
Redis SSH session reached edge-6, on infrastructure shared by public ports 8022
and 8023. Inspect the public router's lifecycle and conntrack evidence at that
timestamp before changing rsync or either source host. Absence of router logs
still leaves router restart, state eviction, and an upstream reset as distinct
possibilities; do not claim one from client-side `Connection reset` text alone.

The scheduled recovery then exercised the intended policy without operator
intervention. At `2026-09-02T21:03:18Z`, exactly 30 minutes after the failed
unit entered backoff, systemd started one new main process (PID `292560`) and
one PostgreSQL rsync/SSH chain to the direct `65.49.70.73:8022` endpoint; the
restart counter was one and no duplicate writer existed. From
`21:08:58Z` to `21:09:13Z`, that socket's `bytes_received` rose from
21,686,350 to 203,844,282, about 11.6 MiB/s (97 Mbit/s), while the connection
remained established. This clears the failed-attempt class and proves both the
bounded retry and partial-resume route can make progress. It does not close
archive freshness or the unresolved shared-forward reset cause. Keep the same
PID under observation through PostgreSQL validation/rotation and the serial
transition to direct Redis port 8023; another reset must retain the partial and
reopen the router, conntrack, or upstream-network discriminator rather than
falling back to the VPN.

A later retry supplied the independent network control that the first reset
lacked. Planetoid's NetworkManager state had fallen from `CONNECTED_GLOBAL` to
`CONNECTED_SITE` at `2026-09-03T00:28:10Z`. Beginning at `00:35:04Z`, its
OpenVPN client received repeated `EHOSTUNREACH` results while contacting the
unrelated public VPN endpoint `50.18.105.84:443`. At `00:43:44Z`, the direct
PostgreSQL session to `65.49.70.73:8022` timed out after 4h10m and about 103.3
GB received; three seconds later the first direct Redis connection to the same
public address on port 8023 failed with `No route to host`. There was no kernel
carrier or physical-interface transition. The VPN reauthenticated at
`00:44:10Z`, NetworkManager returned to `CONNECTED_GLOBAL` at `00:46:57Z`, and
the systemd-owned retry resumed PostgreSQL at `01:13:48Z`. Because an unrelated
public target and both backup ports failed inside the same site-only interval,
this occurrence is a Planetoid router or upstream-Internet outage, not a
PostgreSQL, Redis, SSH daemon, or Fremont-forward failure. It does not identify
whether the offsite router or its WAN provider failed. That final distinction
is operational: inspect the router/WAN evidence for the interval. The software
closure remains preserving the partial and bounded retry; do not route bulk
archives back through the VPN or restart the healthy resumed writer.

The `2026-09-03T04:09:23Z` recurrence refined that unresolved boundary. The
direct PostgreSQL client was reset after receiving 82,888,740,773 bytes. Its
source sshd had authenticated that session at `01:13:48Z` and recorded no
orderly close. Two seconds after the client failure, the Redis flow reached and
authenticated to the separate edge-6 sshd, then vanished while Planetoid
reported another reset; that source also recorded no orderly close. Both source
hosts kept their boot generation and ssh service, Planetoid had no local
carrier or NetworkManager connectivity transition, and its unrelated VPN
endpoint emitted no concurrent reachability error. The two direct sessions
arrived through different Planetoid public egress identities. That proves the
offsite side can present more than one source-NAT identity, whether through
multi-WAN policy or an upstream carrier-grade NAT pool, and rules out rsync,
authentication, either source daemon, and one individually failed source host.
It does not by itself distinguish Planetoid gateway/upstream state from the
Fremont public-forward edge, which are the remaining shared boundaries. Obtain
bounded lifecycle, WAN-selection, and conntrack evidence from both sides
before assigning or changing either one.

Systemd again supplied the safe recovery control. At `04:39:27Z`, exactly 30
minutes later, one new writer resumed PostgreSQL through the configured direct
port and `enp65s0`, using the same public egress identity as the preceding
PostgreSQL attempt. The process and socket remained stable while receive bytes
advanced by 199,659,676 in 15 seconds (about 106.5 Mbit/s). Preserve that
writer and its partial. If router evidence proves destination flows are being
moved or evicted, the operational repair is a stable policy route/NAT mapping
and adequate conntrack capacity for the direct endpoint; it is not a return to
the management VPN, a duplicate rsync, or a larger software retry loop.

A bounded `2026-09-03T05:00Z` TCP path control refined the offsite side without
assigning a cause that the evidence cannot prove. Five probes to each direct
port traversed the same first-hop UDM SE and the same carrier-private/ECMP path
before reaching the Fremont endpoint, with no final-hop loss in that healthy
window. Combined with the different public source identities observed by the
two source ssh daemons, this makes carrier-grade or other upstream multi-egress
NAT state a concrete candidate; it does not prove that the UDM is doing
per-port multi-WAN routing, nor does a healthy later trace explain the earlier
reset. The UDM's current unauthenticated system status reported Internet
available and carried a last-change timestamp near the independently observed
`00:28Z` site-only transition, but exposed no history for the `04:09Z`
recurrence. Obtain UDM WAN-event/config evidence and carrier NAT/session
evidence before choosing between those remaining owners. If the carrier path
cannot retain active multi-hour TCP state, the non-software closure is stable
public/no-CGNAT egress or another approved direct WAN path; retain rsync's
bounded retry and partial-resume safety, and do not move bulk data onto the
management VPN.

The next recurrence at `2026-09-03T05:30:07Z` further narrowed that candidate
without turning correlation into a router verdict. The systemd-owned
PostgreSQL retry ran for 50m40s and received 34,223,902,479 bytes before the
client saw another reset. The separate Redis source authenticated the serial
follow-up one second later, then its client reset after only 622,804 bytes;
neither source sshd recorded an orderly close, and both source ssh services
retained their generations. The PostgreSQL source observed the same public
egress identity used by the preceding and subsequent PostgreSQL retries, while
Redis used the other identity. That is stable per-endpoint selection, not
evidence that one PostgreSQL flow changed egress mid-session.

Planetoid did record a burst of NetworkManager messages reselecting the same
wired interface for IPv6 routing and DNS around both reset boundaries: the
first burst bracketed the `04:09:23Z` reset and the next message followed the
`05:30:07Z` reset by three seconds. There was no link-carrier loss, and the UDM
continued reporting Internet available with its last whole-site state change
near the earlier `00:28Z` outage. An isolated identical reselection at
`01:17:04Z` did not interrupt the then-active PostgreSQL transfer, so the
NetworkManager line itself is a negative control and cannot be named as the
IPv4 reset cause. Repeated bursts bracketing failures instead support a
narrower router/WAN/NAT/router-advertisement lifecycle event that can preserve
coarse Internet availability while invalidating long-lived state. The
remaining proof requires UDM WAN-selection/event and conntrack evidence plus
carrier NAT/session evidence; do not change NetworkManager, rsync, or source
hosts from this correlation alone.

Authoritative ARIN registration supplies one more negative control for that
recurrence. The public egress seen by the PostgreSQL source belongs to Mediacom,
while the egress seen by the immediate Redis sibling belongs to T-Mobile. The
sessions also terminated against independent source hosts. A single carrier
failure and a single source daemon therefore cannot be the common cause of the
two resets within three seconds. The remaining shared stateful boundaries are
Planetoid's UDM/conntrack path and the Fremont public-forward gateway; the RIR
result does not choose between them. Preserve the exact timestamps and obtain
paired WAN-event/config/conntrack evidence from both gateways. Do not infer that
T-Mobile CGNAT, Mediacom, NetworkManager, or either source host is individually
at fault merely because one path traverses it.

Systemd started exactly one fourth retry at `06:00:11Z`. Its PostgreSQL source
again observed the same per-endpoint public egress, and a direct 15-second
sample retained one stable PID, one `enp65s0` socket, and another 17,773,660
received bytes. Preserve that writer and partial. If UDM or carrier evidence
confirms that the direct path cannot preserve an active multi-hour mapping,
the operational closure remains a pinned stable public/no-CGNAT WAN path with
adequate conntrack lifetime/capacity, never the management VPN.

The `2026-09-03T13:18:01Z` storage failure superseded the apparent healthy
serial-transfer interpretation. During the Redis phase, the GlyphTech BB Plus
U.2 external enclosure on `usb 10-2` accumulated UAS command timeouts. Two
device-reset attempts completed without restoring readiness; the SCSI layer
offlined the device, reads and writes to the underlying disk returned I/O
errors, JBD2 aborted the encrypted ext4 journal, and ext4 reported
`Remounting filesystem read-only`. The live mount temporarily exposed both
`rw` and `emergency_ro`, so checking only the ordinary mount flag would have
been a false negative. Rsync then reported filesystem error 30 while the unit
and phase metric remained active.

At `14:16:05Z` the enclosure disconnected, removing the mount. Thirteen seconds
later the same 7 TB partition reappeared on a different USB controller and
mutable block name as `/dev/sdc1`; its LUKS UUID still matched the archive, but
it was locked while the stale mapper still named the vanished `sda1`. This is
not an intentional read-only mount and cannot be recovered by
`mount -o remount,rw`. A September 1 control already contained another UAS
reset and ten read I/O errors for this enclosure, so today is a recurrent
storage/transport fault rather than a one-off rsync or network failure. The
remaining hardware discriminator is SMART/media evidence versus cable, port,
or enclosure stability; the current bridge does not expose SMART through
UDisks. Recover offline in the order prescribed by
`backup-archive-volume-unavailable`, then require three one-minute read-write
observations, a bounded write/read/delete check, 30 minutes without another
USB/UAS, block-I/O, journal, or remount event, and one validated atomic backup
generation. The writer scripts must reject `ro` and `emergency_ro` before any
future phase, but that software guard cannot repair the external SSD path.
The host then rebooted and, at `14:25:18Z`, ext4 replayed the journal and
mounted the same filesystem read-write with no subsequent runtime error count.
That proves present mount availability, not an offline full-filesystem check or
transport durability. Both archive units were inactive afterward. Their
pre-fix failure exit had refreshed the off-volume data metric while the mount
was absent, so a focused raw-Mimir read at `14:33Z` contained fresh zero phase
gauges but no PostgreSQL or Redis latest row. The monitor must report that as
unknown completion visibility rather than claiming the physical artifacts are
gone. The fixed data and code writers preserve strictly validated last-known
latest rows while resetting phases during volume loss; when the volume is
healthy, the existing bounded refresh reconstructs current rows from disk.

Diagnosis order is: query both raw Mimir gateways; read the exact `.prom` files
as the Fluent Bit identity and compare their mtime with the direct unit state;
reproduce the textfile input with a bounded stdout-only process; inspect the
deployed unquoted metrics list; then read the owning systemd unit, mountpoint,
direct mount options, free space, and bounded kernel/unit journal. On the next
fixed long-running GitHub phase,
require two consecutive raw Mimir samples with exactly one organization gauge
at one and both producer heartbeat values no more than 90 seconds old, followed
by final zeros and no heartbeat helper after exit. Verify archive recovery only
after the real unit exits successfully, the completed artifact and manifest
validate on mounted media, and two consecutive direct Mimir reads show the same
new generation inside the five-day band. The Grafana dashboard must agree with
those raw inputs, but it is never the proof source.

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
SELECT split_part(function_name,'.',3) AS task,
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
  verify the claiming worker is dead (deploy log / container list), obtain the
  exact claim through the protected operator lookup, then use the supported
  release/kick commands without copying that identifier into an alert or
  transcript. Releasing a RUNNING task re-opens the duplicate-execution window
  — verify first.

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

**Window-identity overlap despite a gated Prewarm (2026-09-03):** gating only
the post-handoff `Prewarm` call is insufficient. The initial proxy-client sync
independently runs notification warmup for recently created clients, and a
first customer request can independently lazy-open the same DeviceLocal. In
the retained g6 failure, both the old and candidate containers logged warmup
for the temporary acceptance client at `07:24:48Z`; the candidate immediately
logged `window identity restore: 2 identities`, the old drain did not begin
until `07:24:52.050Z`, and a SOCKS request entering the candidate at
`07:24:53.163Z` then stalled on its inner connect. The candidate therefore ran
the old process's exact Connect client identities concurrently. This makes
return ownership nondeterministic and lets old-window eviction remove an
identity the candidate still uses.

Correlate both container IDs for the block; a process-wide query without the
container boundary hides the duplicate:
```bash
warpctl logs main proxy <block> --since='<candidate-start>' --query='warmup' --utc
warpctl logs main proxy <block> --since='<candidate-start>' --query='window identity restore' --utc
warpctl logs main proxy <block> --since='<candidate-start>' --query='[proxy]drain' --utc
```
The replacement now holds identity restoration itself, so every open path is
covered. Early notification/lazy opens remain available but mint fresh IDs and
buffer their newest snapshot per proxy ID. The drain-complete handoff flushes
those snapshots and releases restoration before post-drain prewarm. During a
rollout expect `window identity restore held until drain completion`; any
candidate `window identity restore: N identities` before the old container's
handoff export is a regression. The deterministic boundaries are
`TestWindowIdentityRestoreGateProtectsLazyOpen`,
`TestWindowIdentityRestoreGateJoinsConcurrentReleaseWrite`,
`TestWindowIdentityRestoreGateKeepsNewestDeviceGeneration`, and
`TestProxyDeployOverlapPrewarmGate`.

**Host-memory collapse while drains look correctly staggered:** the rollout
lease must cover candidate start through old-container drain. Serializing only
the drain is too late: every independent block worker can first start and
prewarm its memory-heavy candidate, nearly doubling the resident service fleet,
then queue behind one correctly serialized old drain. The old generation can
remain resident for roughly `blocks * DrainGraceTimeout`, so this is sustained
memory pressure rather than a harmless startup spike.

The 2026-08-31 fireside proxy rollout supplied the decisive signature. The
100.64 GB host had about 37.1 GB available and 7.99 GB swap free before the
rollout. Nine candidates started beside ten old containers within one minute;
each proxy process reported roughly 3.6--6.1 GB RSS. Available memory fell to
1.95 GB, swap free fell to 106 MB, node metrics stopped shipping, SSH became
unresponsive, and candidate IPv4 WireGuard sockets accumulated UDP drops while
the host scheduler thrashed. Crisp had 134.47 GB total RAM and retained about
37.6 GB available during the same overlap, explaining the host-specific split.
Once fireside recovered scheduling time, its candidate passed isolated and
overlapping HTTP/SOCKS/WireGuard traffic with draining socket queues and no new
WireGuard admission or receiver-failure counters; that separates rollout host
pressure from the shared WireGuard-reader defect in §14.6.

Current Warp takes the per-env, per-service host rollout lock before allocating
or starting the candidate, drains the old container synchronously while holding
it, waits for conntrack/LB settlement, and only then admits the next block. A
replacement that cannot obtain the lease must defer instead of falling back to
an unsafe unleased start. Startup reconciliation may still drain orphaned old
containers because doing so reduces memory and starts no replacement. The
deterministic boundaries are
`TestHostDrainLockCoversReplacementOverlap` and
`TestHostDrainLockTimeoutRefusesReplacement` in
`warpctl/host_drain_lock_test.go`.

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

Use a purpose-built network-validation endpoint for the sustained authenticated
egress campaign. Do not use `ur.io/ip` or a normally rate-limited API route as
the high-cadence transport oracle. On 2026-08-31, three `ur.io/ip` repetitions
produced sub-second TLS EOFs across HTTP, SOCKS, and WireGuard: the WireGuard
inner trace had a valid SYN-ACK followed by a valid FIN and no TLS response
payload. Direct traffic from the test host passed 60/60, while three equivalent
proxy repetitions against
`https://connectivitycheck.gstatic.com/generate_204` passed every isolated and
overlapping campaign (558 requests total) despite ordinary exit loss,
quarantine resets, and dial reraces. That separates target/egress-IP treatment
from a proxy transport outage. `proxy/test-main.sh` uses the validation endpoint
by default; `UR_ACCEPT_PROXY_TARGET_URL` remains available for an explicit
site-specific investigation. A site-specific EOF is still a real reachability
result for that site, but it must not be generalized into block-wide proxy loss
when the validation control passes.

**Redis maintenance collision (2026-09-03):** a public proxy request can reach
the listener and then remain at `tunnel_connected` without `GotConn` when the
Connect control plane loses Redis while a newly hosted DeviceLocal is forming
or repairing its provider window. Do not classify that shape as a proxy-host
return-path defect until the Redis boundary is excluded. In the retained
incident, all 32 `redis-cluster@N` masters received `Stopping` at
`06:57:04Z`; the hosted-device tracker recorded `exit_loss=+1` at
`06:57:04.552Z`, and the failing HTTP request began at `06:57:07.264Z`.
Connect, API, and both proxy hosts subsequently emitted connection refusals to
the individual Redis ports while the RDBs saved and reloaded. The proxy block
had no drain, DoH panic, or repeated identity restore in that interval. The
same sustained campaign passed outside the maintenance boundary.

Use all three clocks; a log error that appears after the request deadline can
still name a restart which began before the request:
```bash
# Redis host: simultaneous stop is the decisive full-cluster-outage signal.
journalctl --utc --since '<request-start - 15s>' --until '<request-end + 30s>' \
  -u 'redis-cluster@*.service'
for i in $(seq 1 32); do
  systemctl show "redis-cluster@$i.service" \
    -p Id -p ExecMainStartTimestamp -p ActiveEnterTimestamp -p NRestarts
done

# Fleet consumers: require the same Redis target/port interval, not just a
# generic timeout near the acceptance run.
warpctl logs main connect --query='connection refused' --since='<boundary>' --utc
warpctl logs main api     --query='connection refused' --since='<boundary>' --utc
warpctl logs main proxy   --query='connection refused' --since='<boundary>' --utc
```
The current cluster has one master per shard and no replicas. Reloads must
therefore restart one instance at a time and wait for that member to report
`cluster_state:ok`; restarting all instances together is an avoidable complete
control-plane outage. XOps removes the former `--all-at-once` reload path and
has a deployment-contract regression which rejects its return. Wait for every
node PING and cluster state to recover, then repeat the complete sustained and
overlapping proxy campaign. A post-maintenance pass explains this exact
failure; it does not waive investigation of a failure outside that boundary.

The proxy service intentionally has no normal public 443 status endpoint;
`warpctl ls versions main proxy --sample` can therefore return a uniform 404.
That is a probe-method mismatch, not evidence that all blocks are down. Use the
host-side deploy worker's readiness result or resolve each block's live
allocation and query it directly.

The exact-address control was repeated at 20:39Z on 2026-08-31 after a sample
again returned uniform Proxy 404s through an edge IPv6 address. On edge-1,
Vault's configured `eno2` IPv6 exactly matched the global address on the live
interface. Through both that IPv6 address and the corresponding routed public
IPv4 address, the same LB returned HTTP 200 for the hidden API `g1` status
route and HTTP 404 for the Proxy `g1` status route. Address-family reachability,
TLS, and the LB status mechanism were therefore healthy; only the unsupported
Proxy route was absent. Do not change edge IPv6 or router state in response to
this uniform Proxy-only 404 signature.

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
lifecycle rather than increasing an unbounded queue. A pre-fix metric wrapper
passed `time.Since(start)` directly as a deferred argument, so Go evaluated it
at defer registration and every histogram sample was near zero regardless of
the real wait. Current code evaluates elapsed time inside the deferred closure;
`TestObserveElapsedSecondsUsesCompletionTime` pins a synthetic 3.75-second
completion and prevents the observability defect from returning.

**Shared WireGuard socket-reader head-of-line stall:** this is below the hosted
DeviceLocal return queue above. One proxy block can keep HTTP/SOCKS and its
container/control plane healthy while WireGuard intermittently or permanently
loses every peer. On the block's WireGuard UDP socket, `ss -u -a -n -m -p`
shows `Recv-Q` persistently near `rb` and the `skmem d` drop count rising. A
short syscall trace sees no `recvmmsg`/`recvmsg` on that socket while a healthy
block's equivalent fd is actively reading. During the 2026-08-31 fireside g3
reproduction, the IPv4 queue grew from 191232 to 214272 bytes in 12 seconds,
socket drops rose 12603 -> 12650, and HTTP/SOCKS continued; later WireGuard
requests showed outbound retransmits while their last inbound inner packet
aged past 16 seconds.

The old userspace WireGuard receive routine grouped datagrams by peer, then
performed a blocking send into each peer's 1024-entry inbound queue. One hosted
DeviceLocal whose bounded send path was slow could therefore park the single
shared IPv4 receiver and starve every unrelated peer in the block. An
unexpected socket receive error had a second indistinguishable failure mode:
the routine returned but left the device/container alive. Current code refuses
an encrypted batch when only that peer's queue is full or its lifecycle lock is
busy, allowing WireGuard/inner-protocol retransmission without cross-peer
head-of-line blocking; an unexpected receiver exit closes the device for owner
recovery. Watch:

- `urnetwork_proxy_wg_inbound_peer_queue_drop_packets`: cumulative packets
  refused at the isolated peer boundary because its queue was full or its
  lifecycle lock was busy. A rise names an overloaded/transitioning peer path,
  not a block-wide outage; it must no longer coincide with a filling kernel
  socket queue.
- `urnetwork_proxy_wg_inbound_decryption_queue_drop_packets`: cumulative
  packets refused because the device-global crypto queue was full. This is CPU
  saturation at a shared boundary; the socket reader must continue draining
  even while this counter rises.
- `urnetwork_proxy_wg_receive_routine_failures`: unexpected socket receiver
  exits. Any nonzero value should be followed by device/process recovery, not
  an indefinitely Up block with a dead UDP reader.

Do not restart the block before collecting the socket queue/drop delta and a
healthy-block syscall comparison; a restart empties the only decisive boundary
evidence. After deploying the isolation fix, require sustained concurrent
WireGuard/HTTP/SOCKS acceptance, a draining kernel `Recv-Q`, and zero receiver
failures. Per-peer admission drops may occur under an individual client's
backpressure, but unrelated WireGuard peers must continue.

**H1 encrypted-handshake envelope rejection:** a hosted device can remain
connected while its provider window drains below target with
`stall=platform-unreachable`. Before treating that state as provider ingress
loss, query Connect and the selected Proxy block for
`[framer][reject] ... messageLen=... > MaxMessageLen=4096`. The standing
`logs/framer-message-too-large` class covers this signal explicitly because
the transport emits the rejection at info severity. The 2026-09-01 main
reproduction rejected 4,232- and 4,187-byte reliable H1 messages during the
same sustained campaign that retired two exits carrying 16 flows apiece; the
next SOCKS request completed its public proxy connection and origin TLS
handshake, then lost the response and ended with EOF. Direct origin controls,
public listener checks, host packet-discard counters, and the WireGuard socket
drop counter remained clean. That combination localizes the loss above the
public proxy listener and below the provider flow, at the reliable Connect H1
carrier rather than the origin, rate limiter, policy route, TUN handoff, or
WireGuard queue.

The active post-quantum TLS profile produces a 4,946-4,950-byte fully wrapped
handshake carrier. The legacy 4 KiB minimum was based on component estimates
and rejected the integrated message. Transfer then retried the same immutable
oversized Pack, so the H1 route could not recover and a multi-window refill
kept trying against an impossible admission boundary. Current Connect defaults
set `ClientSettings.MinimumMessageLenLimit`, the platform H1 WebSocket read
limit, every default framer, and the resident exchange/handler cap to the same
8 KiB pooled class. Ordinary H1 data groups retain their 4 KiB target, and the
per-device carrier budget remains the admission bound; this is not an
unbounded-buffer fix.

The v190 standing cadence at 2026-09-01T15:27Z observed a fresh Connect write
rejection at `messageLen=4408`, `MaxMessageLen=4096`, and
`maxFrameLen=4100`. That exact runtime cap directly proves the compatible pair
is absent even while §8.12 source metadata is unavailable: current-main Connect
commit `096414ac` makes the shared minimum 8 KiB, and current-main server commit
`c1403f16` propagates it through the resident H1 path. Stable patch IDs prove
they are patch-identical to the former `7e0fcba` and `53780b3e` hashes after
both histories were rewritten. After §8.13 can read the exact Warpctl identity,
deploy Connect and Proxy artifacts from intentional local checkouts containing
the current-main commit pair; deploying only one endpoint leaves the other able
to reject the same carrier. Record participating diffs because a mutable
version label is not proof of either commit.

Deploy both ends of the H1 path before judging the change: the Connect
resident must admit/forward the carrier and the H1-only hosted DeviceLocal must
accept it. Require zero new 4 KiB framer rejections, a window at target without
`platform-unreachable`, no busy exit retirement caused by that stall, and three
complete sustained HTTP/SOCKS/WireGuard overlap passes. The deterministic
regressions are `TestMinimumMessageLenLimitFitsWorstCaseHandshake` and
`TestH1MaximumLogicalGroupEncryptedPackFitsMinimumMessageLimit` in Connect,
plus `TestResidentAdmitsMinimumMessageLenLimit` in the server Connect package;
the latter sends the full declared minimum through the production resident
framing path and would fail at the legacy cap.

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

**Rejected TLS-chain attribution:** an acceptance error ending only in
`x509: certificate signed by unknown authority` does not distinguish a broken
origin certificate chain from a proxy exit that intercepted or redirected the
request. The normal TLS verifier has already parsed the peer certificates when
`httptrace.TLSHandshakeDone` reports the rejection. Preserve a bounded public
identity for at most four certificates: sanitized subject common name,
sanitized issuer common name, the first six bytes of SHA-256 over the DER, the
total peer-certificate count, and the verified-chain count. Do not retain PEM,
certificate contents, private request headers, or disable verification to get
a response. Compare the leaf identity and fingerprint with a direct-origin
control and the exact selected exit. A matching bad origin chain is external;
a different issuer/fingerprint only through the proxy isolates the exit or
route. Go can report an empty `tls.ConnectionState` on a failed handshake even
though `crypto/x509` already parsed the rejected leaf and retained it in
`x509.UnknownAuthorityError`. In that exact case, fall back to the error's
certificate and emit only the same sanitized subject/issuer and truncated DER
fingerprint, framed as `peer_certs=unavailable`, `verified_chains=0`, and
`rejected_leaf=...`; do not treat the absent connection-state slice as proof
that the peer sent no certificate. `TestHTTPSRequestTraceRetainsRejectedPeerCertificateChain`
supplies a synthetic rejected two-certificate state, while
`TestHTTPSRequestTraceFallsBackToRejectedUnknownAuthorityLeaf` reproduces the
empty-state error and requires the bounded leaf identity to survive without
certificate bytes.

**WireGuard encrypted-UDP versus inner-TUN boundary:** an acceptance request
whose inner packet trace ends at outbound TCP retransmits still leaves two
materially different failure regions: the client may receive no encrypted UDP
return at all, or encrypted WireGuard packets may arrive but fail to produce
the expected inner packet. Aggregate transport-data counts alone do not resolve
that boundary: an established peer sends empty authenticated transport packets
as 32-byte keepalives, so a later inbound count can rise without carrying any
return payload. The acceptance client's WireGuard bind therefore records both
aggregate outer send-attempt, successful-send, receive, byte, error,
last-activity, and public message-type counts (handshake initiation/response,
cookie, and transport data), plus a fixed 16-event ring of direction, relative
time, envelope type, and encrypted datagram size. It deliberately retains no
endpoint, key, payload, packet contents, or customer identity. Snapshot these
counters and recent sizes with the inner trace for the same request:

- a local send error or attempted/successful-send mismatch is below the public
  path and must be repaired before blaming the server;
- successful outer transport sends with no outer receive localize the silence
  before client-side decryption/TUN delivery, but still require server socket,
  LB/DNAT, provider/origin, and return-path evidence to distinguish which side
  produced no reply;
- only inbound 32-byte transport events after inner receive stopped are empty
  keepalives, not proof that the missing response reached the client;
- larger outer transport-data receive packets with no corresponding inner
  receive packet prove encrypted payload ingress reached the client and move
  the failed request into WireGuard peer/decryption/AllowedIP processing when
  the exact interval and healthy control rule out unrelated traffic; and
- corresponding inner receive packets move the loss above decryption into the
  inner TCP/TUN/application path.

Message-type counts are envelope evidence, not a handshake-success assertion:
an established request may carry only transport-data packets, and a per-request
snapshot can begin after the most recent handshake. Do not infer a server
root cause from a zero count alone, expose endpoints or keys, disable
authentication, or present this instrumentation as a fix. Join it to the exact
request time, selected block, server/LB evidence, and a healthy control.
`TestWireGuardTransportFailureReportsOuterUDPBoundary` reproduces the tracked
send, receive, message-type, and wrapped-error evidence.
`TestWireGuardOuterTraceDistinguishesKeepaliveFromReturnDataAndStaysBounded`
pins the 32-byte keepalive discriminator, larger encrypted-data event, privacy
boundary, and fixed ring capacity. This client-side boundary does not require
or substitute for a Proxy service deployment.

### 14.7 Proxy host rollout memory and UDP starvation
Probe: `proxy-memory`

Proxy readiness is not host-capacity proof. Every proxy block normally owns a
large resident Go process, and the old process remains live while its candidate
restores clients and drains. Treat a deploy as an explicit host-memory budget:

- PAGE immediately when the kernel reports a recent global OOM that killed
  `bringyour-proxy`.
- PAGE while process count exceeds running proxy block units and
  `MemAvailable` is below the largest current proxy RSS plus operational
  reserve. Another candidate cannot start safely at that boundary.
- Join the fleet-wide rollout state from §8.11. Proxy-memory retains that state
  in OOM, overlap, headroom, and UDP evidence so its action is safe, while
  `rollout-guard` owns the alert and checks every managed service host rather
  than only hosts currently running proxy processes.
- WARN for the all-block parallel-capacity boundary when `MemAvailable` is
  below the current fleet's aggregate RSS plus the larger of 8 GiB or 5% of
  physical RAM. With a full-overlap guard this remains a hardware/operational
  warning rather than an incident prediction: serialized replacement can be
  safe even though a second complete fleet cannot fit.
- WARN when adjacent same-boot samples show at least 100 new
  `Udp.RcvbufErrors` per minute. PAGE immediately at 10,000/minute or when at
  least 1% of delivered-plus-dropped UDP datagrams in the interval were lost
  to full receive buffers. Record `InDatagrams` alongside the error counter so
  the ratio is explicit. The first sample, boot-id change, counter regression,
  and a sub-30-second manual rerun are warmups rather than zero-length rate
  windows.
- Record proxy process count, aggregate/largest RSS, block-unit count, swap,
  cgroup `memory.max` coverage, and the adjacent UDP counters. Both UDP
  counters are cumulative since boot: only a same-boot delta proves live loss;
  one old nonzero value is historical evidence, not a current incident.

Release-validation boundary learned while preparing the full-overlap guard on
2026-08-31: a focused host-lock test is not sufficient evidence that Warp is
ready to deploy. The repository-local `nginx_local` fixture intentionally
contains the patched UDP upstream PROXY-v2 stream path without requiring local
OpenSSL/PCRE development packages; it omits HTTP SSL and explicitly disables
the rewrite and gzip modules. `TestNginxConfigValidation` used to select the
first executable by path, so installing that narrow fixture made the full Warp
suite fail with `unknown directive "ssl_protocols"` even when a full system
NGINX was available. Warp commit `a85a277` inspects `nginx -V`, requires the
exact version/modules used by the generated production config, rejects an
explicit incapable override, and falls through incapable default candidates.
Its synthetic regression supplies the narrow build flags followed by a full
build and proves the latter is selected. Before publishing the guard, require
both the focused/race-enabled complete-overlap tests and `go test ./...`; this
is a release-test capability defect, not evidence that the generated
production config or a running LB lacks SSL.

**Non-software remediation requirements by alert class:** a software release
may prevent recurrence, but these classes must not be closed until the required
operator action or physical-capacity change has been completed and verified.

- `proxy-host-oom` requires an immediate operational stop or reduction of new
  candidates on the affected host. Host-aware rollout serialization prevents
  accidental old/candidate fleet overlap, but that code fix is insufficient if
  a steady fleet plus one serialized candidate and reserve still cannot fit.
  **Operator action required:** stop or serialize the live rollout and preserve
  incident evidence. **Hardware required when serialized capacity still does
  not fit:** add RAM/proxy-host capacity before restoring that load or
  increasing rollout concurrency.
- `proxy-rollout-overlap` requires the operator to pause the rollout until old
  processes drain, then resume at a host-safe concurrency. The deploy lock and
  memory preflight make that operating constraint durable. **Operator action
  required:** pause/drain the unsafe live overlap. **Hardware required when one
  replacement pair does not fit:** add capacity when even one candidate cannot
  coexist with its old process plus reserve, or when parallel replacement is an
  operational requirement.
- `proxy-rollout-headroom` is an operational capacity warning, not by itself a
  steady-state leak. **Operator action required:** constrain deployment to the
  concurrency the existing host can hold. **Hardware required for greater
  concurrency:** add RAM/hosts if a full-fleet parallel rollout must remain
  supported. Do not close this class merely because an idle snapshot is green.
- `rollout-guard-stale`, `rollout-guard-disabled`,
  `rollout-guard-unverified`, and `rollout-guard-workers-stale` are the
  fleet-wide §8.11 software/operational gates, not hardware alerts. **Operator
  action required:** install validated Warp commit `a85a277` (containing root
  fix `7e2075c`) or later, remove any disabling
  `WARPCTL_STAGGER_HOST_DRAIN=0`, restart every running Warp service worker,
  and verify worker start times before any service release. Adding RAM does not
  close a guard alert; a server application release alone does not restart the
  resident Warp workers. Do not use a full proxy rollout as the discovery test.
- `proxy-udp-receive-drops` proves live host-kernel loss but is host-wide, not
  socket attribution. **Operator action required:** stop launching candidates,
  preserve the exact interval, and map the loss to process overlap, memory,
  CPU scheduling, and `ss -u -m -p` socket queues. **Software required when a
  steady-capacity receive loop is slow:** fix the owning reader or its bounded
  buffering. **Hardware required when a safe steady fleet is already at its
  CPU/RAM/client ceiling:** add proxy instances and capable hosts. Enlarging a
  buffer can absorb a burst but cannot create CPU or RAM; restarting WireGuard
  or reinstalling peers cannot recover a datagram already discarded below
  authentication.

There is also a separate steady-state service ceiling: each proxy process can
serve only a bounded number of active clients (including the WireGuard peer
table's hard per-process maximum). Memory optimization can lower the cost per
client, but it does not by itself raise that per-proxy ceiling. When projected
or observed active clients approach the aggregate ceiling, the capacity fix is
to add proxy instances and the hardware/hosts to carry them, while preserving
memory reserve and failure headroom. A rollout-serialization fix prevents a
transient duplicate fleet from exhausting RAM; it must not be represented as
increasing the fleet's total supported-client capacity.

Software cannot create RAM, host slots, or additional per-process client slots.
Therefore any alert whose observed steady client demand reaches the aggregate
proxy ceiling is a **hardware-capacity alert**: it requires additional proxy
instances on additional capable hardware (or an explicit operational load
reduction), even when all known memory-efficiency and rollout-control code fixes
have shipped.

**Fireside global-OOM signature (2026-08-31):** at
07:17:09.432994Z journald entered memory pressure. At 07:17:16.675993Z the
kernel killed PID 2515221 (`bringyour-proxy`) with 5,014,876 KiB anonymous RSS.
The OOM task table contained 19 distinct proxy processes against ten running
block units: nine old processes at roughly 4.6–5.0 GiB RSS and ten candidates
already growing toward the same size. Free swap was 0 of 8,388,604 KiB on a
roughly 94 GiB host. This is direct evidence that all-block old/candidate
overlap exhausted the host; it is not a WireGuard handshake, peer-install, or
single-process leak diagnosis.

A post-incident control made the capacity mismatch reproducible without
another failure. Fireside's ten steady proxies used 52,842,940 KiB RSS in
aggregate (roughly 50.4 GiB), while `MemAvailable` was 36,805,604 KiB. A
second equivalent fleet plus the 8 GiB reserve therefore had a roughly
23.3 GiB deficit. Crisp's ten proxies used a similar 53,323,752 KiB, but its
68,950,196 KiB available memory could hold that estimate plus reserve. Every
sampled proxy Docker cgroup on both hosts had `memory.max=max` and
`memory.high=max`; cgroups did not contain the overlap. Fireside had 122,012
cumulative UDP receive-buffer errors versus 25 on Crisp, and neither counter
advanced during a later five-second idle control. The large Fireside delta is
consistent with receive starvation during the OOM window, while the exact
kernel kill and 19/10 process overlap remain the causal evidence.

A later sample found Fireside's boot counter at 177,762 and Crisp's at 34, but
another paired ten-second control advanced neither counter. This demonstrates
why magnitude alone is not a live signal: an unknown-length cumulative change
cannot identify its interval, while an adjacent same-boot delta can. The probe
therefore retains process-local prior counters keyed by host and boot id,
normalizes the delta by elapsed time, and reports the whole-host drop ratio
without claiming which UDP socket owned it.

The immediate root-cause fix is host-aware bounded deployment concurrency.
Warp commit `7e2075c` moves the existing service-scoped host lease before
candidate start, holds it through synchronous old-container drain, and refuses
the replacement on lock timeout. Its deterministic tests preserve both the
complete-overlap exclusion and timeout refusal. On a host with Fireside's
capacity, that permits one block candidate to restore and drain before the next
worker enters; do not launch all ten candidates at once. Longer-term
alternatives are reducing the roughly 5 GiB steady RSS per process or adding
enough physical RAM for the desired parallelism. A cgroup ceiling below
measured steady RSS merely kills a proxy earlier; adding swap or enlarging UDP
buffers does not create safe rollout capacity. Do not restart WireGuard or
reinstall peers for this signature.

**Deployment audit (2026-08-31):** the Warp binary installed at
`/usr/local/sbin/warpctl` on the sampled proxy hosts was built before commit
`7e2075c`. It contained the legacy
`Draining %d overlapping container(s) (staggered=%t)` signature and lacked the
new `host rollout lock not acquired within` signature; Fireside's binary mtime
was 07:11:52Z, while the root fix was committed at 13:00:33Z. Other enabled
hosts either carried the same legacy binary or could not expose a recognizable
guard. Therefore the earlier Warp rollout did not deploy this root fix. Deploy
`7e2075c` or later and restart every Warp service worker before starting any
proxy image or config-generation rollout; then require the fleet-wide §8.11 probe to report
`full-overlap` with fresh workers on each managed services host.

**Config-only overlap control (2026-09-01):** at `02:28:29Z`, Proxy's service
image remained `2026.8.31-outerwerld+1033797570` while the independently polled
config generation advanced to `2026.8.31+1034210530`. The still-legacy workers
started candidates across all ten blocks on both hosts. The monitor sampled
Crisp at 18 processes using 83.47 GiB RSS with 33.41 GiB available, and
Fireside at 14 processes using 60.46 GiB RSS with 25.25 GiB available. The
fleet metric subsequently observed 30 live processes before drains completed.
By `02:33:58Z` both hosts had returned to ten processes; no new kernel OOM or
`UdpRcvbufErrors` delta occurred, swap remained available, and memory reserve
recovered. That recovery is a near-miss control, not proof the old guard is
safe: a config-only generation traverses the same replacement path and must be
serialized before candidate start.

Verify a complete image or config-generation rollout, not an idle snapshot:
proxy process count must stay at or below running blocks plus configured host
concurrency; `MemAvailable` must remain above reserve; swap must not exhaust;
kernel OOM and incident-window `UdpRcvbufErrors` deltas must remain zero; and
the simultaneous WireGuard plus HTTP/SOCKS acceptance request must complete.
Implementation convention:
SIGNALS.md §14.7 (`proxy-memory`) maps to `signal_proxy_memory.go` and
`signal_proxy_memory_test.go`. Synthetic cases preserve the 19-process/ten-unit
OOM, live unsafe overlap, config-generation overlap, steady Fireside headroom
deficit, Crisp-sized healthy headroom, guard-aware incident actions, live UDP receive drops,
first-sample/reboot/small-delta warmups, and a configured non-proxy host that
must be skipped. Canonical legacy/disabled/unverified guard cases belong to
§8.11.

### 14.7a Proxy message-pool capacity and ownership visibility
Probe: `proxy-pool`

The process RSS signal cannot distinguish live application objects from free
buffers retained for reuse unless proxy exports the connect library's pool
state. Filter `process_resident_memory_bytes{job="proxy"}` by its actual scrape
timestamp, select the newest `process_start_time_seconds` for each host/block,
and join that identity to these gauges on exact host, block, and runtime
instance:

- `urnetwork_message_pool_capacity_bytes`
- `urnetwork_message_pool_retained_bytes`
- `urnetwork_message_pool_packet_retained_bytes`
- `urnetwork_message_pool_large_object_retained_bytes`
- `urnetwork_message_pool_outstanding`

WARN `proxy-message-pool-unobservable` when a newest fresh process identity
lacks any of the five pool gauges. This is affirmative instrumentation drift
rather than a zero pool: before the 2026-08-31 correction, the collector lived in the
`controller` package, which proxy does not import. The ordinary process metric
was present on all 20 live proxies while every `urnetwork_message_pool_*`
series was absent for `job="proxy"`; the same counters were visible on API,
Connect, and taskworker. Do not diagnose a pool leak from that absence. Deploy
the root server collector first and preserve host RSS/OOM evidence meanwhile.

The 2026-09-01 config-only rollout exposed a freshness bug in the first probe
implementation. Direct host process tables had already returned to ten proxies
on Crisp and ten on Fireside, while Mimir's instant query still returned 34
recent old/candidate identities. Prometheus lookback selected stopped-series
samples but the instant API rendered the evaluation timestamp, so subtracting
the JSON sample timestamp from local `now` falsely classified every returned
identity as live. The query now filters on
`timestamp(process_resident_memory_bytes) >= time() - 90` and then retains only
the newest process start per host/block. This produces the deployment
denominator; it is deliberately not an overlap counter. Use direct host
processes from §14.7 for live old/candidate concurrency and OOM risk.

WARN `proxy-message-pool-capacity` when a newest fresh identity permits more
than 8 GiB plus 16 KiB of class-size rounding. The proxy's old source comment said “8gib
message pool,” but called `ResizeMessagePools(Gib(8))`. That API's historical
one-argument form gives the full argument to the packet classes and again to
**each** large-object class. With today's 4 KiB and 8 KiB large classes, the
logical process-wide free-list ceiling was therefore about 24 GiB, not 8 GiB.
The corrected call assigns one third of one 8 GiB total to packet classes and
passes the remaining two thirds as the combined large-object budget. This
bounds only free-list retention; in-use messages remain live and allocate on a
pool miss, so it does not trade correctness or liveness for the lower ceiling.

WARN `proxy-message-pool-metrics-invalid` when retained bytes exceed capacity
or packet plus large-object retained bytes do not exactly reconstruct total
retained bytes. Those fields come from one allocation-free aggregate snapshot;
an invariant failure is collector/library or label drift, not evidence that a
pool physically owns impossible memory. Counters and gauges have different
meanings: `taken_total-returned_total`/`outstanding` describe live root
ownership, while `retained_bytes` describes returned buffers held for reuse.
Capacity is merely their configured retention ceiling. Establish adjacent
rates before calling rising ownership a leak.

This software correction can lower steady and rollout memory pressure, but it
does not add RAM or increase the hard active-client slots available per proxy.
If steady demand approaches the aggregate client ceiling, the resolution is
additional proxy instances on capable hardware (or an explicit operational
load reduction). If a serialized old/candidate pair still cannot fit with
reserve after the lower retention cap, additional RAM/hosts are likewise
required. Neither alert may be closed by claiming the capacity limit itself
created more customer capacity.

Deployment gate: roll a proxy build containing both the root collector and the
two-argument total-budget helper under the §14.7 full-overlap guard. Require a
fresh complete gauge set for the newest identity in every host/block, capacity
no greater than 8 GiB plus rounding, retained no greater than capacity, and
matching packet/large subtotals. Then follow RSS, `MemAvailable`, kernel OOM,
and adjacent UDP drop deltas through the serialized rollout; the gauges
supplement rather than replace those host boundaries. SIGNALS.md §14.7a (`proxy-pool`) maps to
`signal_proxy_pool.go` and `signal_proxy_pool_test.go`; synthetic cases pin the
controller-only blind spot, newest-generation selection during a rollout, the
legacy ~24 GiB cap, a fixed healthy cap, stale series, and internally
inconsistent snapshots.

### 14.7b Proxy runtime live-set attribution
Probe: `proxy-runtime`

The host-capacity and message-pool probes answer different questions from the
Go process itself. Query Mimir for the newest actual-scrape-fresh proxy
generation in every `(host, block)` and join these fourteen required memory
attribution metrics on exact `env`, `host`, `block`, and `instance` labels:

- `process_resident_memory_bytes`
- `process_start_time_seconds`
- `urnetwork_build_info` (including its mutable `version`/config-generation
  label)
- `go_memstats_heap_alloc_bytes`
- `go_memstats_heap_objects`
- `go_goroutines`
- `urnetwork_proxy_wg_peers`
- `urnetwork_proxy_devices_live`
- `urnetwork_proxy_device_memory_tracked_used_bytes`
- `urnetwork_message_pool_retained_bytes`
- `go_memstats_next_gc_bytes`
- `go_gc_gogc_percent`
- `go_memstats_stack_inuse_bytes`
- `go_memstats_last_gc_time_seconds`

Join a fifteenth capability family without making it part of the memory-owner
sum: `urnetwork_proxy_lifecycle_join_enabled`. WARN
`proxy-lifecycle-join-unverified` immediately when the newest fresh process
omits it or reports anything other than exactly 1. Missing means the running
artifact does not prove that its external owner waits for the manager, shared
`NetworkSpace`, DeviceLocal, provider, RPC, and other SDK children; it does not
mean those owners consume zero bytes. Keeping this separate from
`proxy-runtime-unobservable` lets the live-set discriminator continue to
evaluate a legacy process whose fourteen memory metrics are complete.

Filter each family with its own source timestamp no older than 90 seconds
before selecting the newest process start. Prometheus's instant-query timestamp
is the evaluation time even for a stopped series returned through lookback, so
the JSON timestamp alone cannot identify a live process. WARN
`proxy-runtime-unobservable` when a newest fresh identity lacks any member of
the joined set; a missing owner gauge is unknown memory attribution, not zero.
The query also selects `urnetwork_source_info` as optional alert context when
it is present. Fleet-wide missing, dirty, malformed, and conflicting source or
image identity is independently owned by §8.12 (`provenance`), not by this
memory signal. That separation keeps a legacy process without the new source
gauge fully eligible for the high-live-set alert and prevents source rollout
from masquerading as missing memory ownership. Do not substitute
`urnetwork_build_info.version`: `WARP_VERSION` can advance with a config
generation and is not immutable source or artifact provenance.

WARN `proxy-runtime-live-set` for two one-minute probes when all of the
following hold on a newest identity:

- `HeapAlloc` is at least 3 GiB;
- the allocated object count is at least 20 million; and
- at least 2 GiB remains after conservatively subtracting returned message-pool
  buffers, tracked DeviceLocal ownership, and a 48 KiB allowance for every
  registered WireGuard peer.

The peer allowance deliberately rounds above the measured RSS slope and is
subtracted from `HeapAlloc`, even though it includes peer stacks and other RSS
that is not heap. The residual test therefore errs toward not alerting. It is
an attribution guard, not a claim that the residual is one leak or that every
allocated object is reachable: `HeapAlloc` includes objects that have not yet
been reclaimed by the next GC, and owner gauges intentionally do not
reconstruct the whole Go heap.

Use `NextGC`, `GOGC`, stack-in-use roots, and last-GC time to distinguish a
reachable floor from a transient wave of allocations waiting for the next
collection. Go's collector derives its next heap goal from the live heap and
GC roots marked by the previous cycle; the exact inverse also includes global
roots not exported here, so report the values rather than pretending they are
an exact heap profile. With the fleet's `GOGC=100`, a roughly 7.2 GB next-GC
goal after recent collections is incompatible with a tiny live heap plus only
momentary garbage.

**Production-shaped control (2026-09-01):** the previous Proxy capacity harness
registered peers on a down WireGuard device. It measured 23.9 KiB RSS per peer
and 38 process goroutines at 20,000 peers, while production's up device starts a
sequential sender and receiver for every peer. The corrected isolated ramp
measured exactly two durable goroutines per peer and 36.7 KiB marginal RSS per
peer: 2,000 peers used 104.6 MiB RSS/4,041 goroutines and 20,000 used 749.2 MiB
RSS/40,041 goroutines. Extending the control through endpoint seeding and the
server-initiated deployment-handoff handshake raised the slope to 44.6 KiB per
peer; 20,000 handshaking peers used 904.2 MiB RSS and 557.5 MiB heap. Applying
the server's 8 GiB logical Connect message-pool ceiling to an up device used
757.8 MiB RSS and 544.1 MiB heap at 20,000 peers, proving the empty logical
capacity remains lazy. The alert therefore rounds the peer allowance to 48
KiB. At the live 12,500–13,000-peer scale this is less than about 0.6 GiB RSS,
so peer startup, handoff state, and empty pool capacity cannot explain a
3.6–3.9 GiB allocated heap or roughly 5 GiB RSS by itself. Do not shrink
per-peer queues, remove ordering routines, or lazy-start peers merely because
the old harness omitted them; packet ordering, shutdown, handshake recovery,
and slow-peer isolation need their own proof.

One smaller startup retention path was concrete: the durable `WgClient.Tun`
closure captured the entire decoded `model.ProxyClient`, keeping its proxy
URLs, auth token, and complete WireGuard config strings reachable for every
peer even though activation needs only `ProxyId`. The factory now captures
only the immutable 16-byte proxy ID and manager. A deterministic regression
reassigns the caller's ID after construction and proves the peer factory owns
the original value. This trims peer-owned startup state, but its bounded
per-client payload is not presented as the multi-gigabyte root cause.

The same production sample found all 20 current Proxy processes above the
software attribution band: roughly 3.60–3.94 GiB `HeapAlloc`, 27.5–30.5 million
objects, 4.89–5.24 GiB RSS, 12,524–13,008 registered peers, and 19–40 hosted
devices. Returned message-pool buffers were only about 0.5–5.4 MiB per sampled
process and aggregate DeviceLocal tracked use was about 28–40 MiB. Every process
reported `GOGC=100`, a roughly 7.21–7.38 GB next-GC goal, 66–85 MiB of
stack-in-use roots, and a recent completed collection while the allocated heap
remained in the same band. This establishes a GC-accounted reachable floor,
not mostly garbage that merely awaits the next ordinary collection. Thirty-minute
heap/object deltas moved both up and down across the fleet, establishing a large
floor but not monotonic growth; an unbounded cache remains a candidate only
after its entry count or a heap allocation stack attributes the retained floor.

The startup edge narrows the current owner further. Under Proxy config
generation `2026.8.31+1034210530`, Fireside g1 started at Unix time
1788236178; its first fresh sample 22 seconds later already had 12,857 peers,
zero live DeviceLocals, zero tracked DeviceLocal bytes, 3,885,930,648 heap
bytes, and 29,736,708 heap objects. Later ordinary collections repeatedly
returned to roughly 3.6 GiB and 27.5 million objects while peer count moved by
only a few. That version label does **not** identify the executable: the
locally cached exact Proxy image tag
`2026.8.31-outerwerld-1033797570` contained clean Go base revision
`2c3b552020160433d3854805f9b9a491859e1bf9`, while every live metric carried
the later mutable config generation. The dominant current floor is therefore
constructed at the initial peer-sync/process-start boundary, not by hours of
caller-lock cache accumulation or by hosted DeviceLocals, but its software
ancestry remains unknown until the process emits the source/digest family.
The bounded caller-lock root fix in §14.7c remains required, but it must not be
credited with closing this larger floor until a deployed entry gauge and
old/candidate heap comparison prove that effect.

The matching source path is the old unscoped process warmup. Importing `model`
registers the network-name, location-prefix, and location-group
`SearchLocal` constructors with `server.OnWarmup`. Proxy then called the global
`server.Warmup()` even though no Proxy request path queries those API search
features. Each `SearchLocal` reads its full `search_value` realm and expands
values through `GenerateAliases` into nested length/value maps whose
`aliasHisto` values each own a `map[rune]int`. That is exactly the shape needed
to create tens of millions of small reachable objects immediately at startup,
and it is independent of hosted devices, caller-lock traffic, and empty message
pool capacity.

The root fix makes warmup an explicit feature selection rather than a
process-wide switch. `server/warmup.go` is the single registry of valid targets:
`ip-database`, `network-name-search`, `location-search`, `country-locations`,
and `location-directory`. Each `OnWarmup` registration names one target, and
each service passes an explicit target list:

- API opts into all five current targets because it serves the complete
  controller/model feature surface. Its list is written explicitly rather
  than inheriting future targets automatically.
- Connect opts into only `ip-database`, which its connection latency and
  verification paths use.
- Proxy opts into an empty list. It does not query geolocation or API search
  features; all underlying `sync.Once` values remain available for lazy
  initialization if a future Proxy call path intentionally starts using one.

This target contract prevents a newly registered warmup feature from silently
becoming resident in every binary that imports its package. Synthetic tests
prove that warming one target does not run another target, that a late
registration for an already-warmed target runs immediately, and that the API,
Connect, and Proxy target lists remain their declared sets. The Proxy-specific
regression therefore tests the service selection, not a brittle source-string
absence.

A separate teardown audit found another software-owned memory-overlap path.
Cancellation was not lifecycle completion: `ProxyDeviceManager.Close` could
return to its process owner without joining an already admitted device
construction, installed device run/idle workers, or the manager-owned shared
`NetworkSpace`. The SDK's device graph also requested cancellation without a
single join boundary for its owned API refresh, provider migration, RPC
accept/callback/session, multi-client generator, memory sampler, and security
monitor workers. A retired device or process generation could therefore keep
transports and its reachable ownership graph alive while its replacement was
already allocating. This can amplify device-churn and serialized-rollout RSS
overlap and produce a long close tail. It cannot explain the legacy process's
3.89 GiB heap only 22 seconds after start with zero hosted devices, so do not
credit it with the eager-warmup floor.

Current-main server commit `a4a8b502` closes manager admission before waiting,
joins every admitted constructor and device worker, closes the shared
`NetworkSpace` only after its borrowers drain, makes the Proxy CLI wait at its
external ownership boundary, and closes the acceptance tracker's independently
owned space. SDK commit `e05ec46` supplies the matching device, provider, RPC,
remote, generator, sampler, monitor, and owned-API joins while rejecting late
work after close. Current-main server commit `04c40524` (the patch-identical
replay of former commit `54f461fe`) adds the identity-free capability gauge
and replaces a remote-handler scheduling assumption in the manager test
with an exact local `NetworkSpace.Close` barrier. Barrier-driven synthetic
tests prove that manager shutdown cannot overtake the owned `NetworkSpace`, an
admitted open drains without being
published, a late open is rejected, and each SDK owner waits for its exact
blocked child. These are teardown/overlap fixes, not extra RAM or new active
client slots.

**Production verification (2026-09-02):** the current 20 Fireside/Crisp Proxy
identities all reported clean server revision
`fe3fa8eea625a3935ec7fe6569ee83b8a2578143`, `modified=false`, and one image
digest. That revision descends from `a11ae7b1` but predates `a4a8b502`. With
12,714–13,201 WireGuard peers and 34–66 hosted devices per process, fresh raw
Mimir samples measured 487,169,696–691,516,768 bytes of `HeapAlloc`,
845,558–2,227,123 heap objects, 541,851,648–781,660,160 bytes of RSS, and a
922,075,906–1,162,316,170-byte next-GC goal. This is a material collapse from
the legacy 3.6–3.9 GiB heap, 27.5–30.5-million-object, roughly 5 GiB RSS band
and closes the eager global-warmup root cause on the deployed artifact. The
absence of the lifecycle capability on that older clean revision keeps the
teardown/overlap rollout open; a healthy steady heap is not proof that close
joins retired ownership.

A later `09:24Z` host and process control showed that reduction holding rather
than merely catching every process just after a collection. Current per-process
RSS was 0.58--0.96 GiB and `HeapAlloc` was 0.45--0.72 GiB with roughly
12,864--13,375 peers. Fireside's ten proxies used 6.80 GiB RSS in aggregate
with 79.1 GiB `MemAvailable`; Crisp's used 7.13 GiB with 109.7 GiB available.
Both hosts retained all 8 GiB of swap and had no proxy OOM in the preceding
hour. Across an adjacent sample, Fireside accepted 1,077 additional UDP
datagrams and Crisp 3,954 while neither host added an `Udp.RcvbufErrors` event.
This independently verifies the scoped-warmup memory result and current UDP
health, but it still does not close the missing lifecycle-join capability or
prove a future old/candidate drain safe.

The scoped warmup path is the first root-cause fix to verify. If a deployed
Proxy build that selects no targets still retains the §14.7b floor, capture an
aggregate heap allocation profile or add identity-free owner gauges on that
exact generation, then separate WireGuard peer/client state, full-sync
payloads, the shared `NetworkSpace`, each hosted DeviceLocal's structural
state, and ordinary active HTTP/SOCKS flows. Preserve the existing memory-owner
metrics as negative controls. Do not force a production GC, restart the process
to erase the evidence, raise `GOGC`, lower a cgroup below the measured live set,
or call the floor a leak from time correlation alone.

This alert is software-owned for attribution and memory reduction, but its
capacity closure is separate. If the optimized steady fleet or a serialized
old/candidate pair still cannot fit with the §14.7 reserve, additional RAM or
proxy hosts are required. If active clients approach the aggregate hard peer
or device ceiling, additional proxy instances on capable hardware (or an
explicit operational load reduction) are required even after the live set is
smaller.

Deploy the Proxy service artifact from intentional local server and SDK
checkouts containing `04c40524` (and therefore `a4a8b502` and `a11ae7b1`) and
`e05ec46`. It must contain targeted warmup, the value-only
WireGuard TUN factory, the bounded caller-lock cache, the new owner gauges, the
generic source/digest gauge, the lifecycle capability gauge, and the complete
manager/device/RPC ownership joins. No xops deployment is needed for these
process-memory corrections. Require §8.13 to record the exact local-checkout
Warpctl identity and §8.12 to join both
`urnetwork_build_info` and `urnetwork_source_info` to the exact newest process
identity and independently match the running container; the release ledger
must separately record each checkout base plus any participating local diff,
especially the SDK dependency because the server's Go VCS stamp alone does not
prove that checkout. With those identities recorded, the first post-sync sample
and repeated post-GC floor must be
materially lower than the legacy 3.6–3.9 GiB
heap/27.5–30.5-million-object band. Require the conservative residual below 2
GiB for 15 minutes across multiple GC cycles, improved RSS and host
`MemAvailable`, and simultaneous WireGuard plus HTTP/SOCKS acceptance with no
new OOM or adjacent UDP receive drops. During a controlled drain, require the
old manager/device/RPC ownership graph to join before process exit and before
the stop timeout, with no late device publication or continuing API refresh;
compare old/candidate RSS overlap rather than using process disappearance as
the only close proof. Require `urnetwork_proxy_lifecycle_join_enabled=1` on
every newest identity for two consecutive scrapes. If only the closure/cache
reductions are visible while
the large startup floor remains, provenance-check the target list before
profiling the next owner. SIGNALS.md §14.7b (`proxy-runtime`) maps
to `signal_proxy_runtime.go` and `signal_proxy_runtime_test.go`; synthetic cases
pin a complete high live set, optional source context that cannot hide high
memory, conservative owner accounting, and newest-generation selection during
overlap.

### 14.7c Proxy caller-lock cache boundedness
Probe: `proxy-cache`

`ProxyDeviceManager.ValidCaller` runs on every accepted Proxy connection and
memoizes the caller-IP lock for the presented proxy ID. The historical
`lockCache map[server.Id]proxyLockEntry` attached a 30-second expiry to each
value, but expiry only caused a reload when that same ID returned. It never
deleted a stale key. A long-lived process could therefore retain every
distinct valid, formerly-valid, or unknown signed proxy ID it had observed,
including its `LockSubnets` slice, until process exit. The value TTL bounded
configuration staleness; it did not bound memory.

The software root fix is a hard-capacity TTL/LRU cache. It copies only the live
prefix slice, removes a key when that key is looked up at or after expiry,
amortizes one bounded scan per TTL to release cold expired keys under traffic,
preserves the first still-fresh result when concurrent miss loaders race, and
evicts the least-recently-used key before cardinality can exceed 16,384. The
capacity is intentionally explicit: changing it is a memory-budget decision,
not a way to silence cache pressure. Deterministic tests insert 16,641 unique
IDs and prove a final size of exactly 16,384 with 257 evictions; separate cases
pin exact expiry, cold-key sweeping, hot-key LRU ordering, prefix ownership,
and concurrent-loader winner semantics. Current-main server commit
`a11ae7b1` owns this cache, its identity-free metrics, targeted service
warmup, and the value-only WireGuard TUN factory; production ancestry and
rollout gates use that exact commit rather than the mutable config version.

Each Proxy process must export these identity-free metrics:

- `urnetwork_proxy_lock_cache_entries`
- `urnetwork_proxy_lock_cache_capacity`
- `urnetwork_proxy_lock_cache_hits_total`
- `urnetwork_proxy_lock_cache_misses_total`
- `urnetwork_proxy_lock_cache_expirations_total`
- `urnetwork_proxy_lock_cache_evictions_total`

Join them with `process_resident_memory_bytes` and
`process_start_time_seconds` on exact `env`, `host`, `block`, and `instance`
labels. Filter every family by its own source timestamp no older than 90
seconds before choosing the newest generation for each `(host, block)`. This
prevents an old bounded candidate and a current legacy process from being
combined into false proof.

WARN `proxy-lock-cache-unobservable` immediately when a newest fresh identity
lacks any cache gauge or activity counter. A legacy image without the gauges
is unknown and must not be interpreted as an empty cache. WARN
`proxy-lock-cache-bound` immediately when capacity is zero, capacity is above
16,384, or entries exceed the published capacity. Stop promotion on this
contract failure: do not raise the capacity or restart away retained entries.

WARN `proxy-lock-cache-pressure` after five one-minute probes when entries are
at least 90% of capacity. The hard bound still protects the heap, but sustained
occupancy near the ceiling means distinct IDs are arriving faster than
the amortized TTL sweep removes them. Use the hit, miss, expiration, and
eviction deltas with authenticated acceptance and rejected-credential logs to
distinguish a legitimate hot set from invalid-token churn. Operational
rate-limiting or source blocking may be required for abusive traffic; do not
expose proxy IDs in metrics or alert files. If a legitimate 30-second hot set
is larger, measure per-entry heap cost and storage-load latency before changing
the explicit capacity.

This closes one software-owned unbounded retention path, but it is not evidence
that the old map owns the entire §14.7b multi-gigabyte live set. Compare cache
entries and activity with `HeapAlloc`, heap objects, and the conservative
live-set residual after deployment. Continue attributing WireGuard
peer/client state, full-sync payloads, shared `NetworkSpace`, DeviceLocal
structure, and ordinary active flows until the residual closes. A bounded
cache also cannot create RAM or raise the hard active-client ceiling: if the
optimized steady process or serialized old/candidate pair cannot fit with the
§14.7 reserve, additional capable Proxy hardware is required; if abusive ID
churn is the source, an operational traffic control is required.

Verify the root fix on the exact Proxy artifact: every current identity exports
all eight joined metrics for two consecutive scrapes, capacity is positive and
no greater than 16,384, entries never exceed capacity through more than 16,384
distinct synthetic IDs and real acceptance traffic, and caller-IP locks still
accept and reject correctly. Observe at least 15 minutes across multiple GC
cycles: cache occupancy and miss/eviction rates settle, §14.7b heap residual
and host reserve improve or identify the remaining owner, and there is no new
OOM, UDP receive loss, or storage latency. SIGNALS.md §14.7c (`proxy-cache`)
maps to `signal_proxy_cache.go` and `signal_proxy_cache_test.go`; synthetic
cases pin legacy-image blindness, invalid capacity, sustained near-capacity
pressure, and newest-generation selection during rollout.

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

### 17.1 Listener and deployment identity — SUBTENSOR1

Probe: `subtensor`

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
`ready=true`, `isSyncing=false`, runtime specification 452, transaction version
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
and runtime specification 452. An RPC response alone is insufficient: a
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
   at its prior start. The desired pinned v447-binary archive deployment and
   `/etc/subtensor/preflight.json` had never reached the host.

The first pinned-image deployment then exposed a playbook bug: it asserted
`isSyncing=false` and the then-current exact runtime 447 before installing nginx. At that point
the new node was healthy bootstrap state—one peer, advancing from block 50,296
toward 7,826,287, and reporting historical runtime 135 at that historical
head—but the assertion aborted the play and left port 9944 closed. The corrected
gate requires peer, identity, and head progress during bootstrap; writes
`preflight.json` with `ready=false`; installs and probes nginx; and enforces
the current runtime only after synchronization converges. The nginx drop-in orders it
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
heads, `/healthz`, and JSON-RPC on overlay 9944 are proven. Chain cutover waits
for live convergence at runtime 452; a historical `preflight.json` is only a
deployment-time observation and must not be treated as current status.
Reboot snow once as a deployment gate: nginx must wait until
`172.28.208.185` exists or restart on the transient bind failure.
`network-online.target` alone does not guarantee that the later OpenVPN address
is present.

### 17.4 2026-09-01 exporter validation and warp-fallback root cause

The Grafana dashboard's millions-behind values were validated against both
source layers. Direct JSON-RPC on snow and the matching Mimir series agreed on
archive/lightnode best heads, sync targets, and peer counts; Mimir samples were
about seven seconds old and the `job=subtensor` and
`job=subtensor-lightnode` labels were distinct. This rules out a stale scrape,
label collision, unit-conversion error, or Grafana export bug. At the audit
boundary the archive was about 1.56 million blocks behind and the lightnode
about 1.62 million behind, while each advanced 24 blocks in a 12-second sample
with 9 and 7 peers respectively. One-hour Mimir slopes estimated archive net
catch-up near 1.93 blocks/s (roughly 9.4 days) and lightnode net catch-up near
3.13 blocks/s (roughly 6.0 days), if those rates hold. The dashboard therefore
must display explicit sync-target lag and sample age; raw best-block height
without those labels is easy to misread.

The lightnode lag is not an acceptable warp bootstrap. Its startup log records
`Can't use warp sync mode with a partially synced database`, followed by
`Warp sync failed. Continuing with full sync.` The process command still says
`--sync=warp`, so inspecting argv alone produces a false healthy signal. The
durable `/data/subtensor-lightnode` path already held a partially synchronized
database and forced the full-sync fallback. The archive is intentionally
`--sync=full`; its lag is ordinary bootstrap while peers and progress remain
healthy.

The root-cause repair is operational and storage-aware: deploy the lightnode
against a new empty, generation-specific base path, preserve the old path for
rollback/evidence, and gate deployment on both the startup fallback log and a
near-reference head. Do not wipe or reuse the failed partial path. This cannot
be closed by deploying server application code alone; it requires the xops
Subtensor playbook and a controlled node restart. A future hardware/storage
alert likewise needs capacity work when disk, memory, or sustained import
throughput—not software correctness—is the measured bottleneck.

The 2026-09-01 public-path retest closes the earlier P2P-forward incident:
independent Fireside TCP probes reached snow's WAN ports 30333 and 30334, and
both nodes exposed large, rising incoming-connection counters. Continue to
alert on zero peers or frozen heads because public TCP reachability alone does
not prove a retained peer session.

A fresh direct control at 18:53:14Z separated the two bootstrap paths again.
The archive was at block 6,382,733 of 7,912,210 with seven peers and had reduced
its lag by about 301 blocks in the preceding two minutes. The lightnode was at
6,349,148 with nine peers but remained 1,563,062 blocks behind. A privileged,
read-only inspection then proved the lightnode container had started at
2026-08-29T02:42:05Z with the pinned image and `--sync=warp`, but its live
`/data` mount was still `/data/subtensor-lightnode`. The current xops config
already selected the fresh `/data/subtensor-lightnode-warp-v2` path. The
millions-behind value therefore represents the still-running failed v1
generation; the prepared v2 repair had not been deployed. This is direct
runtime evidence and does not rely on argv or a stale preflight file.

The ordinary `run-subtensor.sh` path also owns netplan, packages, nginx,
Fluent Bit, and the archive container, so it is not an acceptable way to
change only this lightnode while the archive's multi-day bootstrap must remain
uninterrupted. Xops commit `0b1373b` adds
`main/ansible/run-subtensor-lightnode.sh`: it verifies the exact remote host,
refuses a nonempty inactive generation twice before activation, preserves the
old database, runs Compose for only `subtensor-lightnode`, fails immediately on
the warp-to-full startup discriminator, and asserts the archive container ID
and start time remain unchanged through the near-head/runtime/gateway gates.
That isolated runner was the prerequisite for the authorized v2 control below;
its full readiness result remained pending until the new container's own
verification window completed.

The authorized isolated v2 rollout supplied a second, distinct failure
discriminator. It created the previously unused
`/data/subtensor-lightnode-warp-v2`, recreated only `subtensor-lightnode`, and
retained the archive identity. The v447 node resolved the bootnode, reached
three peers, and entered `Warping, Downloading finality proofs` without either
warp-to-full fallback line, but its best and finalized heads remained at
genesis throughout all 360 five-second readiness attempts. The gate failed with
HTTP 200 and block `0x0`, rather than mistaking a reachable RPC for progress. A
new empty path therefore removes the v1 database fallback but does not make
v447's testnet warp valid.

The pinned v447 source predates two upstream testnet GRANDPA repairs:
`add2b31a19ccf650ad50d79e8ba2668e6494f56f` corrects the checkpoint transition
and `0876234316a3b9107ce1eb0781b04ae55f5df89e` supplies the historical signing
sets. Both are ancestors of RaoFoundation tag v448. The official v448 OCI
provenance resolves its AMD64 image to source commit
`e18ca67f1a00b35c7d5986888d1cc388da8c095f`; pin the multi-platform index
digest `sha256:a1ac7792b5279cdad701eec15742296f91d4be83e256a29fe57cffd500fa8f13`.
The controlled repair is v448 on a new empty
`/data/subtensor-lightnode-warp-v3`, still through the isolated runner. Preserve
both failed generations and require the same archive-identity, startup,
near-head, runtime, EVM, peer, and gateway gates before declaring recovery.

The `subtensor` probe now reads the live container's configured image, sole
`/data` mount, start time, and bounded startup-log discriminators. Class
`subtensor-deployment-drift` reports an image or generation mismatch;
`subtensor-warp-fallback` remains specific to a rejected cold database without
a retained starting block;
`subtensor-warp-checkpoint` identifies a peer-connected finality-proof download
stuck at genesis without fallback; and `subtensor-warp-bootstrap` preserves the
generic lag case when neither root cause is yet proven. These classes require
operational deployment/storage action and cannot be closed by server
application code alone.

The 2026-09-01 v3 rollout exposed a separate lifecycle class. The empty
`/data/subtensor-lightnode-warp-v3` generation was created at 21:12Z and had
acquired 1.4 GiB of state. At 23:06Z the full `run-subtensor.sh` path
unconditionally restarted `subtensor.service`, recreating both the archive and
lightnode. The new lightnode started from block 6,413,262 and logged both the
partially-synced-database and full-sync-fallback discriminators. It continued
to advance with nonzero peers. This proves that a host configuration deployment
interrupted a progressed warp generation; it does not prove that the original
empty-generation warp failed, and resetting to v4 solely to erase the log would
discard useful progress.

Class `subtensor-warp-resume` is therefore distinct from cold
`subtensor-warp-fallback`. A nonzero `system_syncState.startingBlock` opens the
resume class immediately; an explicit startup fallback line corroborates that
classification but is not required. The process-start block is the stronger
generation discriminator because a bounded log query can lose an early line,
while the generic zero-start cold-bootstrap case retains its 15-cadence noise
guard. Preserve an advancing resumed generation and measure its lag slope.
Xops' full-host playbook must
reconcile only the archive, start rather than unconditionally restart the
aggregate unit, require an existing lightnode image and `/data` path to match,
and prove its container ID is unchanged. Intentional lightnode replacement
remains exclusively owned by `run-subtensor-lightnode.sh`. Choose a new empty
generation only if the current import stops or a newer independently verified
checkpoint materially improves recovery. Faster sustained catch-up may still
require storage/CPU capacity; neither the monitor nor a playbook can create that
hardware capacity.

The 2026-09-02 freeze supplied a narrower cause than peer scarcity or slow
hardware. Both pinned containers start through `/entrypoint.sh` as root, which
recursively assigns `/data` to the `subtensor` account and then executes the
node as UID/GID 10001. The full-host playbook subsequently reconciled each
active bind-mount root to `root:root 0750` at 01:44Z. Direct process and inode
inspection then proved UID 10001 could neither traverse nor write either
mounted `/data`. The archive temporarily continued through already-open
handles, but at 02:58:54Z the lightnode's RocksDB background worker reopened a
path, received `IO Error: Permission denied`, failed block import, and froze at
6,447,926. Its repeated one-to-three peer sessions, changing public target,
0.0-bps import rate, low CPU, 1.7-TiB free space, and the simultaneously
advancing archive rule out disk exhaustion, CPU saturation, and simple peer
absence as the causal boundary.

Xops must own both bind roots as the pinned runtime UID/GID, not as root, and
must run an exact in-container `test -w /data` after Compose reconciliation.
The permission repair must preserve both container identities; restoring an
inode owner does not require replacing either process. The applied repair was
idempotent and both exact write gates passed without changing either 23:06:56Z
container start time, but two post-repair samples left the archive fixed at
6,447,933 and the lightnode fixed at 6,447,926. That proves both existing
RocksDB processes latched the earlier background error even after inode access
was restored. Preserve both generations and obtain explicit authorization for
service-scoped, same-generation restarts, one node at a time with head progress
proved between them. Do not erase either database or select a new generation
merely to clear EACCES.

Class `subtensor-data-permission` is immediate. The restricted helper reports
the live process UID/GID, bind-source UID/GID/mode, whether that exact account
has write plus traverse permission, and a Boolean from the bounded 5,000-line
tail beginning at that exact process's `StartedAt` boundary for the RocksDB
permission signature. Docker retains a container ID and its older log stream
across an in-place restart, so container-wide history would incorrectly blame
the recovered process for its predecessor's EACCES. The process scope remains
intentional and has no rolling wall-clock cutoff: a fatal background error must
not age out after 30 minutes while the same process remains alive and frozen. HEALTHY
requires permission observation to be present, `data_runtime_writable=true`,
and either no retained permission signature or head advancement by that exact
process. Recovery requires advancing best heads across two post-repair samples;
container replacement or restart changes the process evidence boundary and
must retain the same data path. Missing permission facts are observation loss,
not a healthy default.
The alert action branches on the live permission boundary. A non-writable path
requires the idempotent Xops ownership repair with both identities preserved.
A writable path plus a retained signature and frozen head means provisioning is
already complete: do not rerun the full-host playbook or choose a new database
generation. Obtain explicit authorization for one service-scoped,
same-generation restart at a time, archive first, and prove progress plus the
other node's unchanged identity before continuing.

That authorization was exercised on 2026-09-02 without changing either
generation. The archive restarted at 18:37:56Z with the same container ID,
pinned image, `/data/subtensor` mount, and peer identity; it logged no new
permission/database error and advanced from block 6,447,933 to 6,448,540. Only
after that proof, the lightnode restarted at 18:39:33Z with the same container
ID, image, `/data/subtensor-lightnode-warp-v3` mount, and peer identity while
the archive start time remained unchanged. It logged no new permission/database
error and advanced from 6,447,926 to 6,448,157. Both nodes had nonzero peers
and advanced again across the final 15-second sample. This closes the latched
EACCES freeze, not their bootstrap lag: the progressed lightnode necessarily
reports the existing `subtensor-warp-resume` discriminator and both nodes must
continue converging on their retained data.

The 2026-09-03 post-restart control exposed why the nonzero start block must
stand on its own. The exact v3 process reported `startingBlock=6,447,926`, the
same retained block at which its predecessor had frozen, then advanced to
6,518,461 with 17 peers. The installed helper nevertheless returned
`startup_fallback=false`, causing the old Go evaluator to emit the generic
15-cadence cold-bootstrap class. Its Docker query used `--tail 10000` across
the first 30 minutes, which selects the last lines of that window and can
discard an early startup discriminator on a verbose node. The helper now reads
the beginning of a five-minute startup window without a tail cap, while the Go
probe independently treats the nonzero process-start block as an immediate
resume. This is a monitor/Xops observability repair only; it does not authorize
restarting or replacing the advancing v3 database.

### 17.5 Long-window catch-up convergence

Probe: `subtensor-convergence`

The 15-second progress check in §17.1 answers whether a node is moving. It does
not answer whether the node is gaining on the live chain quickly enough to
become usable. Measure each exact configured `host`/container `job` pair over a
one-hour Mimir window:

```promql
lag = max by (host, job) (substrate_block_height{status="sync_target"})
    - max by (host, job) (substrate_block_height{status="best"})

net_rate = max by (host, job) (deriv(substrate_block_height{status="best"}[1h]))
         - max by (host, job) (deriv(substrate_block_height{status="sync_target"}[1h]))

import_rate = sum by (host, job) (
  rate(substrate_block_verification_and_import_time_count[1h])
)
seconds_per_imported_block = sum by (host, job) (
  rate(substrate_block_verification_and_import_time_sum[1h])
) / import_rate
```

Also read current `substrate_sync_queued_blocks`, the target-head derivative,
the best-head raw-sample count, and raw-sample age. Select exact inventory
hosts and jobs with anchored escaped matchers; do not accept an unconfigured
series, merge the archive and lightnode, or infer health from a dashboard.
Query through an active services host's loopback Mimir listener. A successful
instant response is observable only when every configured node supplies the
complete measure tuple, at least 200 samples in the one-hour range, and a raw
best-head sample no more than 90 seconds old. Missing, partial, stale,
non-finite, or inconsistent values are observation loss, not zero lag.
Validate configured host/job pairs in stable lexical order and name missing
measures rather than emitting only a bit mask. Check current-sample freshness,
then window sample count, before interpreting derivatives: a short or stale
range that crosses a scrape/restart boundary can produce a negative target
slope, but that slope is not chain convergence evidence.

- READY: lag is at most 128 blocks for a full node or the configured
  `warp_max_lag` for a warp node. No catch-up alert is needed inside that band.
- HEALTHY BOOTSTRAP: a node outside its readiness band has positive net
  catch-up and `lag / net_rate <= 14 days`.
- `subtensor-slow-convergence`: positive net catch-up implies an ETA above 14
  days for three consecutive one-minute cadences.
- `subtensor-nonconverging`: the target head grows at least as fast as the
  local best head, also sustained for three cadences. A rising best height is
  not recovery when lag is flat or growing.
- Recovery requires the same generation to enter its readiness band, or two
  consecutive complete one-hour windows with a positive net rate and an ETA
  no greater than 14 days. A restart resets the evidence boundary and cannot
  be counted as recovery by itself.

`import_rate * seconds_per_imported_block` estimates the fraction of one
block-import worker's wall time spent verifying/importing. When that value is
at least 80% while at least 128 blocks remain queued, the immediate mechanism
is a busy serial historical-import stage. Confirm against the exact process
cgroup's `cpu.stat`, `memory.events`, and `io.stat`, plus bounded host `vmstat`.
If there is no CPU quota/throttling, OOM, sustained I/O wait, or host-wide CPU
pressure, more peers and spare host cores do not accelerate that serial stage.
Do not restart a progressing generation, enlarge a timeout, or replace the
archive merely because its ETA is long.

The 2026-09-03 Grafana restart supplied the discriminator for that validation
order. During Mimir history warmup, the archive had 143 one-hour samples and a
260-second-old last source sample. Its truncated target series produced a
spurious `target_rate=-17.904045` and therefore a nominal
`net_rate=18.450862`; the old parser reported only “inconsistent one-hour
measures” before checking the stale source. On the next evaluation the current
archive gauges aged out entirely and the tuple retained only mask 94, missing
`lag`, `queued_blocks`, and `sample_age`, while the lightnode still published a
fresh sample. In parallel, direct SSH/RPC observation of snow over the
management overlay timed out from both the monitor workstation and an enabled
edge. Treat this as metrics/overlay observation loss and preserve the node
generation; it does not prove an 18-block/s catch-up burst or a node restart.

This class may require an operational or hardware fix that software alone
cannot supply. The available closures are a measured node import optimization,
faster single-core/storage hardware, acceptance of the measured wait, or an
isolated candidate that proves the exact configured chain specification has a
materially newer trusted checkpoint. Preserve the other node's identity while
testing a candidate. A newer runtime or image number is not checkpoint proof:
RaoFoundation v452 added a newer finney GRANDPA checkpoint, but its v452
`raw_spec_testfinney.json` still contains no `grandpaWarpSyncCheckpoint`, so
that finney-only change does not accelerate Snow's testfinney bootstrap.

The 2026-09-03 06:35Z control demonstrated the missing signal. Both retained
post-permission-repair generations had healthy peers and moving heads. The
archive was 1,404,141 blocks behind and gained about 0.466 blocks/s; the
lightnode was 1,398,810 behind and gained about 0.462 blocks/s, implying about
35 days to convergence. Each held 2,112 queued blocks. Their one-hour import
rates were about 0.54 blocks/s at about 1.83 seconds per imported block, or
roughly 99.6% of one import worker's wall time. Direct cgroup and host controls
showed no CPU quota/throttling, no OOM, ample available memory, negligible swap
and I/O wait, and mostly idle host CPUs. Matching throughput on two databases
at the same historical height rules out peer scarcity and a node-specific
stuck process; it localizes the current ceiling to serial historical block
verification/import. Keep the existing generations running while this alert
records their real convergence horizon.

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

`edge-ipv6-policy-route` is the return-path variant. After a timeout, run an
exact `ip -6 route get` from the configured public source to a fixed external
IPv6 address. If the interface owns the address, local SNI/TLS returns 200, and
the public gateway answers, but the lookup selects a different device or
source, the host's LB policy rule/table is absent. The reply follows the
lower-metric management default, so an external SYN can arrive while its
SYN-ACK leaves asymmetrically. Inspect the exact `ip -6 rule` and `warp<N>`
table; do not change the public address, router ACL, or application container.
Deploy Warp `8924493`'s bounded policy-table reconciliation for
non-transparent LBs and,
with operator authorization, replace/restart only the affected LB controller
so it runs that code. Recovery requires route lookup to select the configured
interface and source, source-bound external egress, and three exact-address
HTTP/1.1 200 responses.

The 2026-09-03 production reproduction began when all four public links on
edge-3 and edge-4 lost carrier between 06:46:43Z and 06:47:14Z, then regained
it between 07:00:41Z and 07:01:37Z. The first probe correctly observed the
interfaces down. After carrier returned, edge-3 retained both IPv4/IPv6 source
rules and per-interface defaults and recovered externally. Edge-4 owned both
exact Vault addresses, both gateways answered sub-millisecond ICMPv6, and both
local SNI probes returned 200, but it had no IPv4 or IPv6 policy rules and its
public-source route lookups selected the management interface/source; both
external TLS probes timed out. Its two non-transparent LB workers remained in
the same active generation throughout. Warpctl periodically reconciled policy
routing only for transparent LBs; ordinary non-transparent workers initialized
it once and then only polled versions. A link/network-manager cycle could
therefore remove their foreign routes/rules without a repair path. The durable
software fix, Warp `8924493`, replays the existing idempotent reconciliation
every 30 seconds inside ordinary LB polling; a restart by itself is only a
temporary repair.

For any other timeout, capture the pinned SYN, check NDP, policy routing, and
exact DNAT counters, then change only the first layer where packets disappear.
A connected request with a non-200 response is instead a TLS/SNI, LB generation,
or application-readiness fault. Verification for every repair is three pinned
HTTP/1.1 IPv6 200 responses per configured address plus advancing counters at
the repaired layer.

The standing `log-errors` collector also emits derived class
`tailer-ipv6-route-loss` when warpctl reports `Tail read error ... no route to
host ... Reconnecting` on its own stderr. Warpctl reconnects internally, so
its process does not exit and the ordinary tailer restart counter cannot see
this interruption. Keep that stderr isolated from remote service stdout: it is
a monitor-path finding, never a panic or novel error in the requested service.
Aggregate independent service tails by exact destination address; several
services failing together prove a shared route event even when it recovers
between five-minute `edge-ipv6` samples. On any event, immediately run this
section's monitor-local/default-router check and retain both another configured
edge and an unrelated provider IPv6 prefix as same-second controls. If the
monitor's IPv6 state remained stable, continue through the
identity/public/self-egress battery. Two different prefixes behind one site
router are not independent. A later HTTP 200 proves recovery, not that the
earlier route loss did not occur.

The production discriminator occurred on 2026-09-01. Seven of eight standing
tails simultaneously lost edge-4 `eno3` at 11:52:32Z, another seven did so at
11:53:44Z, and five independently running old/new watcher tails observed the
same destination at 11:59:49Z. Both watcher parents and their tail processes
stayed alive. A focused battery minutes later found the active Vault address
on live `eno3`, its LB unit active, and three pinned HTTP/1.1 requests returning
200 in 0.25–0.27 seconds. The edge journal contained no link, address, or LB
transition in the event window. The upstream router then held the exact target
MAC as REACHABLE; its eth7 neighbor table contained 15 entries, below the
1,024/4,096/8,192 garbage-collection thresholds, with zero table-full events.
Failed neighbors for unrelated addresses and cumulative interface drops are
background pressure worth monitoring, but these controls do not prove neighbor
cache exhaustion. At that point the remaining boundary was a sub-minute
monitor-side or upstream route/neighbor event; no edge or router mutation was
justified.

The next high-frequency control caught a broader recurrence at
12:34:28Z–12:34:40Z. Pinned HTTPS to both edge-4 LANs and an edge-3 LAN moved
together from response/connect timeouts to immediate connect failures, then
recovered. The datacenter router's contemporaneous capture showed successful
edge-4 neighbor solicitation/advertisement immediately before and after the
interval and no destination-unreachable response sent back to the monitor.
This rules out either edge-4 interface, LB, or LAN neighbor as the shared
cause and confines the event to the monitor-to-datacenter path, the router WAN,
or its upstream. Because those three targets still share one site router, that
sample alone could not distinguish a monitor-wide IPv6 outage from withdrawal
of the site's routed space.

The monitor-local control closes that boundary. At 12:34:34.419Z (07:34:34.419
local), macOS `configd` recorded `RTADV en0: router lifetime became zero`; it
immediately published a network state without IPv6 and then recorded two
autoconfigured-address detach/deprecate transitions. An IPv6-bearing network
state returned at 12:34:40.895Z, matching the external timeout → immediate
no-route → recovery sequence. There was no contemporaneous Wi-Fi
deauthentication, disassociation, roam, link-down, link-up, or power-off event.
Together with the healthy datacenter NDP capture and the simultaneous failure
of multiple routed prefixes, this proves that recurrence was the monitor host
losing its local IPv6 default router when its stored lifetime expired—not a
production edge address, interface, LB, LAN neighbor, or service failure.

That `configd` message does **not** prove that the first-hop router transmitted
an explicit Router Advertisement with lifetime zero. The same local expiry can
occur when ordinary refresh advertisements are missed or arrive too late. The
binary has a distinct `ignoring RA (lifetime zero)` diagnostic, which was not
present in the bounded event records. A timestamped ICMPv6 type 134 capture is
therefore required to distinguish an intentional or erroneous withdrawal from
RA refresh or local-link loss before selecting the router change.

The earlier events have the identical precursor. Local default-router lifetime
expirations at 11:52:24.621Z, 11:53:35.584Z, and 11:59:40.537Z each removed
`en0` IPv6 before the standing tails reported route loss at 11:52:32Z,
11:53:44Z, and 11:59:49Z, respectively; IPv6 returned after 7.1, 16.9, and 7.4
seconds. Thus all four observed route-loss waves belong to the monitor's first
hop. The collector queries and correlates 15 seconds on either side of the
transport timestamp so the proven 7–9-second diagnostic lag cannot hide the
cause.

The software signal now queries only that narrow recent local record after a
tail route event and attaches the affirmative discriminator without copying
general system-log contents. When present, its alert says not to mutate the
named edge and instead directs the operator to correlate the local first-hop
router's uptime, WAN/failover state, RA daemon, and local-link health, then
capture timestamped ICMPv6 type 134 traffic on the monitor interface during
recurrence. Repair the RA source if the capture shows an explicit withdrawal;
repair RA cadence or delivery if refreshes are absent or late. Verify at least
30 minutes with the stored router lifetime refreshing before expiry, an
unrelated IPv6 control, and every configured edge. This is an
operational/network-appliance repair; deploying API, Connect, Proxy, Warpctl,
or another edge service cannot fix it.

### 18.2 Public TLS certificate expiry and alias coverage

Probe: `tls-expiry`

Take `manager.<domain>` only from the active `services.yml` exposed aliases;
alternate environments that do not expose it gracefully noop. Take every
non-transparent public LB interface and both of its configured address families
from the first active services version, joined to the enabled monitor host
inventory. Present manager SNI directly to each exact address. This avoids both
ordinary DNS selection and Route 53 health selection hiding a stale LB
generation. Operator-disabled hosts remain absent from the denominator.

The bounded connector completes only a TLS handshake and sends no application
bytes. It deliberately captures the peer chain before verification so an
expired or mismatched leaf remains diagnosable, then applies ordinary system
roots. Evidence is limited to public certificate metadata, exact configured
endpoint, SAN count/coverage, and SHA-256 fingerprint; never emit private key
material or the Vault resource contents.

Healthy means every exact endpoint:

- returns a parseable peer leaf;
- is inside its `NotBefore`/`NotAfter` interval;
- covers the configured manager hostname;
- passes system-root chain verification; and
- retains more than 21 days before expiry.

`tls-certificate-expired`, `tls-certificate-not-yet-valid`, hostname mismatch,
and untrusted-chain findings page on the first cadence because normal clients
already reject them. `tls-certificate-expiring` warns when 21 days or less
remain. A transport or empty-chain result is
`tls-certificate-unobservable` after two cadences: correlate it with §18.1
routing/ingress evidence and do not call the certificate healthy from a
sibling. Recovery requires three consecutive five-minute exact-address
observations with hostname coverage, system trust, and more than 21 days
remaining after the final LB handoff.

The 2026-09-03 manager incident is the defining synthetic reproduction. The
alias entered active `services.yml` on 2026-08-21, after the 2026-08-12 SAN
certificate had been issued; that certificate did not cover
`manager.bringyour.com` and no exact manager asset existed. Warp's established
selection order tried the exact host, then selected the newest available
`star.bringyour.com` asset. That wildcard inventory stopped at version
`2024.3.19`, whose DigiCert leaf expired on 2025-05-17. Both A and AAAA
therefore completed TCP and returned HTTP 200 only when verification was
bypassed, while every ordinary TLS client rejected the endpoint. Correct Route
53 aliases did not mitigate the invalid leaf.

Vault TLS version `2026.9.2`, issued at 05:05Z on 2026-09-03, contains an exact
manager asset, covers `manager.bringyour.com`, and is valid through 2026-12-02.
It was promoted and copied to the edges before the 07:19Z LB-controller
handoff. The replacement edge-0 container completed its handoff and its
controller resumed ordinary 30-second route reconciliation, yet five repeated
manager handshakes still returned the same expired wildcard while five API
handshakes to the identical address returned the valid 2026-08-12 API leaf.
The replacement's emitted Nginx config was decisive: only the manager server
block named
`/srv/warp/vault/tls/2024.3.19/star.bringyour.com/star.bringyour.com.pem`.

The reason is an important deployment boundary: `warp/lb/Makefile` runs
`warpctl lb create-config` while building the LB image, and the Dockerfile
copies that generated Nginx tree into the image. A later Vault sync changes
mounted certificate material but cannot rewrite those baked paths. The running
LB generation was built on 2026-08-31, before the exact manager asset existed;
restarting its controller via `run-edges.sh` therefore reproduced the stale
selection in a fresh container. Generating the same block from the current
checkout and promoted Vault selects
`tls/2026.9.2/manager.bringyour.com/manager.bringyour.com.pem`. The operational
closure is a new LB service image built after promotion, followed by its
controlled deployment and exact IPv4/IPv6 verification.

This class has an operational/deployment closure that monitor software cannot
perform. When an exposed hostname is absent from the newest promoted SAN set,
an authorized operator must run `warpctl certs issue <env>`, review and promote
the generated `all/tls.pending` version, and sync Vault through the existing
edge workflow. Then build and deploy the LB service image so its baked config
selects that promoted asset. `run-edges.sh` alone refreshes the mounted Vault
and controller but does not regenerate an already-built image's Nginx config.
Let the controlled LB drain finish and inspect the replacement's selected path
before declaring recovery. Do not bypass client verification, change DNS,
restart unrelated services, or turn certificate validation into a new
build-admission architecture.

## 19. Web platform association metadata

### 19.1 Android App Links and Apple association files

Probe: `association-files`

The product clients declare `ur.io` as a platform-verified web origin. Android's
release manifest uses an `android:autoVerify="true"` HTTPS intent filter for
that host, so `/.well-known/assetlinks.json` is a functional ownership contract,
not crawler traffic. The site also carries
`/.well-known/apple-app-site-association` for app links and web credentials.
Ordinary homepage, nginx, and web-process health can all remain green while
both contracts return 404.

For every enabled exact edge IPv6 address, retain `ur.io` TLS SNI and request
both paths directly so DNS or CDN health selection cannot hide one stale web
generation. Healthy requires HTTP 200 with a JSON media type and semantic
decoding: assetlinks must authorize package `com.bringyour.network` for
`delegate_permission/common.handle_all_urls` with at least one well-formed
SHA-256 signing fingerprint; the Apple document must authorize
`6BGU69Q742.network.ur` for both app links and web credentials. Aggregate
application failures into one `web-association-files` alert. Curl exit 7/28 on
an exact edge remains owned by §18.1 so one transport incident does not open a
second static-file ticket.

2026-08-31 root cause: the authoritative files were tracked under
`mmm/ur.io/astro/public/.well-known` and present in `astro/dist/.well-known`.
The SEO build gate therefore passed. Both `build-main` and `build-canary` then
ran `mv dist/* build/<environment>/`; POSIX shell `*` excludes dot-prefixed
root entries, leaving `.well-known` in `dist` while the web image copied only
the staged tree. Production consequently returned an nginx ENOENT/404 for
both exact files on an otherwise healthy site.

The first live run of the focused probe at 2026-08-31 19:51:55Z completed all
16 application checks (four enabled edges × two configured IPv6 interfaces ×
two files). Every check connected to its exact address and returned HTTP 404
`text/html`; there were no transport-only exclusions. The disabled/offline
edge-5 was absent by construction. This rules out DNS rotation, CDN selection,
one-edge skew, and IPv6 ingress as the cause of the missing documents.

The standing watcher was promoted at 2026-08-31 19:53:43Z to binary v153,
built from server commit `13158a8e` (SHA-256
`a8fda17ecd18c1e8d62ce7b1bd04dc18a2b9a422de4a16dc5b9bf6074b578958`).
It started all eight configured Loki tails, emitted the same 16/16 association
finding on its first pass, and completed its first one-minute log drain with
all tails still alive. That drain also caught the independently current
Grafana backend-EOF and taskworker payout classes, proving the new static-site
probe did not disrupt standing-log coverage. Only after this gate did v152
receive SIGTERM; v153 remained the sole watcher.

The software fix is mmm/ur.io commit `72190198`: a reusable staging script
enumerates every `dist` directory entry, including dot directories, refuses to
merge into stale output, and is exercised by a deterministic synthetic test
containing both platform files. Build and deploy the web service from that
commit or later. Do not edit live containers, fabricate nginx fallback JSON,
or treat the requests as scanner noise.

Verification requires both paths to return HTTP 200 `application/json` and
pass semantic validation on every enabled exact edge and through the canonical
CDN hostname. Then require zero new nginx ENOENT lines for those exact paths
for ten minutes. A web deployment is required; server, API, Connect, or
taskworker deployments cannot change these static image contents.

### 19.2 Transactional email assets

Probe: `email-assets`

The shared email layout (`controller/email_templates/_layout.html`) embeds two
absolute `https://ur.io/images/emails/...` wordmark images in every
transactional email: the black-on-paper default, and the white one clients swap
in under `prefers-color-scheme: dark`. They ship inside the product site's
bundle (`mmm/ur.io/react/public/images/emails`, mirrored into the astro build by
`sync-public`), so they are live product dependencies of the website deployment.
Keep the probe's dependency list equal to the distinct URLs in
`controller/email_templates`:

- `ur-wordmark-black-bg-320.png`
- `ur-wordmark-white-320.png`

Healthy means every path returns HTTP 200, an `image/*` media type, and a
non-empty body at both relevant layers. First request the recipient-facing
`ur.io` URL through ordinary DNS. Then retain the `ur.io` TLS SNI and Host while
pinning the same paths to every enabled edge IPv6 address, the way §19.3 pins the
association files. The second check detects one stale site generation and
distinguishes an edge fault from DNS selection, TLS, or a cache in front of the
edges.
Curl exit 7/28 on an exact edge remains owned by §18.1; failure of the public
path remains user-facing and is always measured. An affirmative HTTP or
content failure is class `web-email-assets` on the first cadence. A request
that ends before any HTTP response is instead class
`web-email-assets-transport`: retain its selected address and curl exit as
evidence, but require two consecutive five-minute cadences before alerting.
One transport miss cannot establish missing bytes, edge bundle drift, or a
cached error and must not prescribe invalidation. A one-shot diagnostic still
returns the first sample.

Retired contract history: before 2026-09-01 the templates referenced six
`https://bringyour.com/res/emails/...` assets through a CloudFront
`main-web.bringyour.com` origin. A fleet-wide 54/54 HTTP 404 incident was caused
by that origin Host falling into nginx's empty default root and was repaired by
Web commit `2b410faa`. A later one-cadence TLS exit 35 was disproved by direct
IPv4/IPv6 controls. Those six paths, the `main-web` Host, and their 54-check
denominator are historical and must not be used as current alert guidance.

At 2026-09-02 05:21:58Z, the first run of the current contract completed 18
checks: two public requests plus two images pinned to both IPv6 interfaces on
each of four enabled edges. All 18 connected and returned the same 24,535-byte
`text/html` HTTP 404 response. This rules out edge IPv6 transport, DNS rotation,
one stale edge, and the deliberately disabled edge-5. A direct
`warpctl ls versions main web --sample` then showed both Web blocks (`beta` and
`g1`) entirely on `2026.8.31+1034210530`, which predates the image addition.
The same direct sample showed API on `2026.8.31+1034210530` and Taskworker on
`2026.9.1-outerwerld+1034926970`; both were built before server commit
`ec6e3b92` (the patch-identical current-main replay of former commit
`7c852d56`) introduced the new template URLs. At this observation boundary the
404 is therefore a predeployment ordering failure, not evidence that those
new templates have already reached recipients. It becomes a user-facing
broken-email regression if an email-sending artifact containing `ec6e3b92`
rolls out before the Web dependency is healthy.

Mmm commit `b4b229c5c` adds both PNGs to
`ur.io/react/public/images/emails` and the Astro public tree. Their SHA-256
values match across both trees, and the synthetic `sync-public` test proves the
whole images directory is mirrored. At 2026-09-02 21:53Z, current local
`astro/dist` contained the same two hashes while staged `astro/build/main`
still lacked both paths; the independent `sync-public` and `stage-build`
synthetic tests both passed. The causal boundary is therefore an un-staged,
undeployed Web build, not missing source bytes, a Cloudflare-only cache, nginx
Host handling, or live-edge routing.

Build and deploy only the Web service from an intentional local Mmm checkout
containing `b4b229c5c`, using the exact local-checkout Warpctl observed by
§8.13. Require `sync-public`
to place both files in staged output before image publication. The Web build
also consumes the local SDK WASM, so record its checkout base and any
participating diff. At the 2026-09-02 incident observation the local SDK
worktree held unrelated uncommitted device/network changes; those changes are
not a monitor fault, but the operator must decide whether they belong in the
Web artifact and preserve the exact diff if they do. Keep API and Taskworker
artifacts containing current-main server `ec6e3b92` or later behind this Web
gate when they have not deployed yet. If those senders are already active, do
not roll them back or redeploy them merely to clear an asset alert; treat the
404s as a current broken-email dependency and prioritize the Web rollout.
Connect, database, Grafana, and Xops deployments cannot repair this boundary.
Do not copy files into live containers.

At 2026-09-02 21:48Z, a fresh direct Mimir query showed every API group and
both Taskworker groups running server base `2d6f27c` with
`source_modified=true`. Git ancestry proves `2d6f27c` descends from
`ec6e3b92`, while both Web groups still run
`2026.8.31+1034210530`. The release-order gate has therefore already been
crossed in production: newly rendered shared-layout emails can reference the
18/18 missing image paths. The root-cause action remains the Mmm Web rollout
containing `b4b229c5c`, not a sender rollback.

This alert can cross from software into operations: any exact-edge semantic
failure requires the Web image/config repair and full Web rollout. If every
exact edge is healthy while only the public URL returns an HTTP error,
ownership moves to DNS/TLS or a public cache, and a path-scoped invalidation is
allowed only after that response proves the boundary. A transport-only failure
instead needs IPv4/IPv6, resolver, TLS, selected-address, and exact-edge
controls; it does not justify cache mutation. This signal does not require
hardware.

Verification starts only after both `beta` and `g1` run the new Web artifact.
Every cataloged public path and every enabled exact-edge path must return HTTP
200 `image/*` with nonzero bytes, a deliberately missing image must remain 404,
and bounded Web logs must contain zero new exact-path 404/ENOENT records for ten
minutes.

## 20. Mobile store crash reports

Mobile-store reporting is an optional external observation plane. Each
provider has a dedicated least-privilege Vault resource. If that resource does
not exist, its probe is intentionally unarmed: it validates nothing, performs
no OAuth/JWT work, opens no network connection, and returns no alert. This is
the credential-bootstrap state, not proof of zero crashes. Once the resource
exists, incomplete credentials, authentication/authorization failure, API
errors, malformed or oversized results, unsafe pagination, checksum failure,
or an unreadable cursor are visibility failures. Authentication and role
failures use `provider-authentication`, exhausted provider HTTP/transport
failures use `provider-api`, and rejected response contracts use
`provider-data-invalid`; their `SignalID` remains `monitor/visibility` and the
monitor run remains non-zero. Do not silently disable a configured provider to
clear one of those failures.

Both probes persist versioned atomic cursors under
`StateDir/provider-reports/`, retain an overlapping lookback, and commit state
only after every response needed by that run has validated. The overlap is
what captures late store processing and corrections; the cursor prevents the
same provider record from opening a duplicate alert. Transport errors never
include provider response bodies, OAuth assertions, bearer tokens, or Apple
signed download URLs. Bounded provider evidence removes email addresses,
tokens, opaque identifiers, URL query strings, controls, and Markdown fence
characters before an `Alert` can render it.

### 20.1 Google Play crash issues and vitals visibility

Probe: `play-crashes`

Read the Play Developer Reporting API v1beta1 for the package named by the
existing `vault/<env>/google.yml` `webhook.package_name` value. The dedicated
optional `vault/<env>/google-play-reporting.json` is an ordinary Google service
account credential with at least these fields:

```json
{
  "client_email": "...",
  "private_key": "-----BEGIN PRIVATE KEY-----\n...",
  "private_key_id": "...",
  "token_uri": "https://oauth2.googleapis.com/token"
}
```

Enable the Play Developer Reporting API, grant this service account the app's
`View app information (read-only)` permission in Play Console, and use only
the `https://www.googleapis.com/auth/playdeveloperreporting` OAuth scope. It
does not need release, financial, user, or write permissions. The probe obtains
short-lived OAuth tokens and never copies the service-account document into an
alert or its cursor.

Every 30 minutes, query DAILY `crashRate`, `userPerceivedCrashRate`, and
`distinctUsers` through the provider's advertised freshness boundary, then
search crash-only ErrorIssues across a rolling 48-hour UTC interval ending at
the most recent whole UTC hour, as required by the provider's error-search
contract. The next cadence overlaps the partial hour rather than sending an
invalid minute/second boundary. Paginate both sources with exact original
parameters. A new issue/version group, a
later `lastErrorReportTime`, or a larger report count at the same report hour
is class `play-crash-issue`. The alert carries the issue cause, likely method,
latest version code and OS API, 48-hour report/user counts, current aggregate
metric context, and at most one provider-sanitized sample report. Evidence is
bounded again locally. The per-run sample-download cap is 50 advancing groups;
`play-crash-overflow` makes any excess explicit while cursors still prevent a
later duplicate flood. A lower replacement count or moved report hour for the
exact same whole-hour query window, or a changed root-cause fingerprint without
a forward occurrence, is the one-shot class `play-crash-correction`, not a
fabricated new crash. The cursor also records the query boundary: a count or
last-report hour that falls only after the moving 48-hour window advances is
ordinary aging, not a provider correction, and updates quietly.

The aggregate decimal rates are retained as provider values and context; do
not invent a percentage scale or page threshold that the response itself does
not establish. The issue/version advance is the actionable crash signal. Use
Play Console's current core-vital threshold independently when prioritizing a
release. If DAILY freshness is absent, older than 72 hours, or the matching
metric query contains no row, emit `play-crash-data-unobservable` or
`play-crash-data-stale`. An empty issue list is quiet only when an explicit
metric row (including explicit zero values) establishes visibility. Empty
metadata or rows never means zero crashes.

This alert normally needs a software root-cause fix in the Android client, an
embedded SDK, or a server contract used by that client. A device/OS-specific
cluster can instead require an operational release exclusion or an upstream
vendor repair; neither is fixed by deploying the monitor. Verify a software
fix by shipping the owning Android artifact or server dependency, confirming
the named issue/version stops advancing throughout the 48-hour overlap, and
keeping provider freshness current. Credentials and API enablement require
only Vault/Play Console operations and a monitor restart or watcher promotion;
they do not require an API, Connect, Proxy, Grafana, or database deployment.

### 20.2 Apple App Crashes aggregate and corrections

Probe: `apple-crashes`

Read App Store Connect Analytics for the numeric app ID already stored at
`vault/<env>/apple.yml` `app_store_notifications.app_apple_id`. Keep the team
reporting key separate from Sign in with Apple and App Store Server API keys in
optional `vault/<env>/apple-reporting.yml`:

```yaml
issuer_id: "..."
key_id: "..."
private_key: |
  -----BEGIN PRIVATE KEY-----
  ...
  -----END PRIVATE KEY-----
```

An App Store Connect Admin must create one `ONGOING` Analytics Report request
for the app once. The monitor key itself should have the `Sales and Reports`
role, which can list and download generated reports without managing the
request. The probe signs a fresh ES256 JWT for API reads (`kid`, `iss`,
`aud=appstoreconnect-v1`, 15-minute lifetime). It deliberately uses a separate
unauthenticated HTTP client for each short-lived segment URL, so the App Store
Connect bearer token is never forwarded to Apple's storage host.

Every six hours, find the active ongoing request, the report whose name begins
with `App Crashes`, and every DAILY instance in the seven-day processing
overlap. Treat the paginated segment catalog as stable IDs, then read each
segment's details immediately before downloading it; Apple's signed URLs are
valid for only five minutes and must not age while later catalog pages are
collected. Cap compressed and expanded bytes, verify Apple's declared size and
MD5 checksum, expand gzip, and parse the tab-delimited schema by header name.
Reject foreign App Apple Identifiers, negative/non-numeric counts, missing
headers, duplicate resources or page tokens, mismatched detail resources, and
pagination that leaves the App Store Connect API origin.

Apple processing instances are replacement sets, not deltas. For each event
`Date`, rows from a newer `processingDate` replace the older partition; never
merge or sum the same Date across instances. This matters because a daily
instance can contain multiple event dates, late arrivals, and rare historical
corrections. Compare the final replacement partition with the committed one
and alert per Date/app-version group. A first observation or increase is
`apple-crash-group`; any changed or disappearing group in a newer replacement
is `apple-crash-correction`. The alert includes total crashes and a bounded
top-device/platform breakdown. `Unique Devices` is labeled as a row sum because
a device can appear in more than one dimension row and must not be presented
as a globally distinct count. The monitor processes at most 60 chronological
instances per run and emits `apple-crash-backlog` until a longer outage is
drained.

The App Crashes report contains aggregates, not crash stacks. It covers only
users who opted to share diagnostics, is complete within five days, and omits
privacy groups below Apple's reporting threshold. Therefore an absent active
request, stopped-due-to-inactivity request, missing App Crashes report, empty
instance list, an instance with no observable rows, or a newest processing date
older than 72 hours is never zero crashes. Emit the explicit classes
`apple-crash-request-missing`, `apple-crash-request-stopped`,
`apple-crash-report-missing`, `apple-crash-data-unobservable`,
`apple-crash-privacy-suppressed`, or `apple-crash-data-stale`, as applicable.
The privacy-suppressed class remains active from the cursor until a later
non-empty instance supersedes it.

Aggregate integration requires only the credential, the one-time ongoing
request, and the monitor code. Fixing an aggregate crash usually requires an
iOS app, SDK, or server-contract deployment; an OS/device boundary may instead
need an operational rollout exclusion or Apple/vendor resolution. Detailed,
symbolicated stacks are not available from this report. Adding that evidence
requires a separately consented client crash/MetricKit collection path and its
server ingestion deployment; do not claim a stack-level root cause from these
counts. Verify by shipping the owning fix, observing replacement rather than
summation for late instances, confirming the affected version stops gaining
crashes through the five-day completeness window, and retaining a current
processing date.

## 21. Management VPN session inventory

The management OpenVPN plane is a control and observation path. Losing it can
make unrelated host probes fail together while their application processes
continue running. It is not a bulk-data path: Planetoid must continue pulling
PostgreSQL and Redis archives through the dedicated public SSH forwards in
§11.22 even when the VPN is unhealthy.

### 21.1 Server-side client-session coverage

Probe: `vpn-sessions`

Configure exactly one enabled monitor host with role `vpn-server` and every
required client with role `vpn-client`. A VPN server can override the ordinary
service-host SSH user and identity paths in `monitor.yml`; relative identity
paths resolve under `WARP_HOME`, so no key material enters the inventory. The
probe connects only to the server. It reads
`openvpn-server@server.service`, the mtime and `CLIENT_LIST` records in the
server-owned status file, a bounded server-originated ICMP check of each
configured overlay address, and a bounded two-hour reduction of inactivity
timeouts. Status rows are joined to clients by the exact configured virtual
IPv4 address, not by a possibly different certificate common name. The ICMP
check is part of this inventory's established host contract: every enabled
client currently answers it from the VPN server.

The status and journal are reduced through one source-equality map inside the
VPN server. This lets a current-but-unreachable session and a recently timed
out session retain their shared-site attribution while the site flaps between
control-plane states. The output contains only transient equality-group
numbers and configured host names in each group; public addresses, source
ports, certificates, and unrelated VPN identities never leave the host. A
missing client with no recent comparable timeout stays isolated-or-unknown
rather than being assigned to a site from adjacency or memory. Site-class
attribution also requires at least one reachable configured client as a
central-path control.

HEALTHY: the VPN server is `active/running`; its status file is no more than 90
seconds old (the live configuration uses OpenVPN's 60-second default status
interval, plus 30 seconds of scheduler/write tolerance); every enabled
`vpn-client` virtual address appears in the fresh snapshot; and every address
answers from the server across the overlay. A `CLIENT_LIST` row alone is not
healthy forwarding. Require two consecutive one-minute cadences after recovery
and ten minutes of observable dependent host probes before closure.

BROKEN:

- `vpn-server-unhealthy` is an immediate page when the configured server unit
  is not active/running. Check the exact process, EC2 instance/system checks,
  UDP/443 listener, journal, and network boundary before assigning every
  client independently.
- `vpn-status-stale` after two cadences means the status writer is more than 90
  seconds old or in the future. An old row is UNKNOWN, not evidence that its
  client remains connected.
- `vpn-client-session-loss` after two cadences is a warning for one absent
  virtual address whose source is isolated or cannot be compared. Ownership
  can be the client process, host, WAN, route, or NAT path.
- `vpn-site-session-loss` after two cadences is a page when a missing client
  and at least one other missing or current-but-unreachable configured client
  map to the same current/recent public source. With the central server and an
  unrelated reachable client healthy, this localizes the common boundary to
  that offsite LAN, router/conntrack/NAT, WAN, or site-side OpenVPN path. It
  does not prove which device failed.
- `vpn-client-data-path-loss` after two cadences warns when a current session
  exists but its exact overlay address does not answer the server-originated
  check. This separates control-session presence from host/tunnel forwarding.
- `vpn-site-data-path-loss` after two cadences pages when a current-but-
  unreachable client and at least one other current-but-unreachable or
  recently timed-out configured client map to the same source while another
  configured client remains reachable. This healthy control rules out a
  server-wide tunnel failure and the monitor workstation, but the equality
  still does not distinguish the site client processes, router state, NAT/WAN,
  or hosts.

For every site-class alert, compare a path that deliberately bypasses OpenVPN.
If Planetoid's active direct public SSH transfer continues increasing receive
bytes, keep the fault at the UDP/VPN boundary. If that direct TCP flow also
disappears, the failure is broader than OpenVPN and belongs to the shared site
router/WAN path. Preserve one resumable backup writer and advancing Subtensor
database generations; never restart the databases, start a duplicate transfer,
or move archive payloads onto the management VPN to make this signal green.

**2026-09-03 incident:** the central `by-us-west-1-vpn-0` process had been
active since June 20 with zero systemd restarts, both EC2 health checks were
green, its status file remained fresh, and 19 other clients were still active.
Snow, Planetoid, Sille, Goofe, and Widget all used one equal public source.
Goofe, Widget, Snow, and Sille established new sessions within two seconds at
07:52:24--07:52:26Z, then the server aged out Widget at 08:03:59Z, Planetoid at
08:06:29Z, Goofe at 08:07:59Z, Sille at 08:08:03Z, and Snow at 08:14:48Z for
the exact 120-second ping-restart timeout. Snow and Planetoid were absent from
the fresh server snapshot while enabled Fremont, Fireside, and Crisp controls
remained reachable. The long-running direct PostgreSQL backup socket that had
still advanced at 07:56Z was also absent by 08:18Z. This rules out a central
VPN restart, application-only Subtensor failure, monitor-machine tunnel loss,
and VPN-only packet loss. The established root boundary is the common offsite
site/router/WAN path; exact router WAN-event and conntrack evidence is still
required before naming the failed device or mechanism.

At 08:49Z Snow and Planetoid both reappeared in the fresh `CLIENT_LIST`, but
the VPN server still could not reach either overlay address and workstation
SSH to both timed out. The same server-originated check reached the other eight
enabled clients. Session presence was therefore a false recovery boundary:
the common offsite data path remained blackholed while its control sessions
were current. This observation added the two data-path classes above; closure
starts only after forwarding and dependent probes recover, not when a row
reappears.

The recovery did not hold: the server recorded new exact ping-restart
timeouts for Snow at 08:54:10Z and Planetoid at 08:54:31Z, and both rows were
absent again in the 08:55:53Z focused probe. The transition from absent, to
session-present-but-unreachable, to absent again is an active flap of the same
site path. It is not a completed recovery window and must not clear the
router/WAN investigation.

A bounded Planetoid capture at 09:18:53Z caught the next brief return. The host
had retained the same August 15 boot, OpenVPN PID, and zero-restart generation;
its Ethernet carrier was up and its route still selected the LAN gateway. The
unchanged client had nevertheless logged repeated UDP `EHOSTUNREACH` failures
from 08:04Z onward, while NetworkManager moved from global connectivity to
site-only at 08:47:52Z, briefly back to global at 09:06:14Z, and back to
site-only at 09:11:50Z. OpenVPN completed again at 09:18:29Z, but the server
could not reach either Planetoid or Snow at 09:23:35Z while reaching the other
eight configured controls. Subsequent workstation SSH to both timed out. This
rules out client process replacement and a persistent missing host route; the
remaining causal boundary is the shared site gateway/router/WAN path. A DHCP
lease renewal occurred during the outage but is correlation, not evidence that
DHCP caused it. Exact router WAN-event, gateway/conntrack, or carrier evidence
is still required before naming the failed device or mechanism.

The historical client template named
`by-us-fmt-0-edge-0.bringyour.com`, which Route 53 authoritatively returns as
NXDOMAIN, while active clients reach the current
`by-us-west-1-vpn-0.bringyour.com` endpoint. XOps `9421fe3` corrects the
generator and adds an exact single-remote regression. Existing clients run a
root-only `/etc/openvpn/by-pre.conf`; the ordinary monitor identity can prove
the unit and config path but cannot read that installed endpoint. Therefore
installed-client convergence still needs an authorized configuration audit and
controlled update. Do not restart a healthy client merely to test it. This is
a separate cold-start recovery hazard, not sufficient evidence for the live
multi-path site outage above.

This alert is operational/network-owned. Software can improve the redacted
detector and reconnect diagnostics, but it cannot restore site power, WAN
service, router state, carrier NAT, or physical links. A hardware replacement,
router configuration repair, or carrier operation may be required. Verify
recovery from the server-side session inventory, the dedicated direct-path
control, and dependent host probes rather than from one successful ping.
