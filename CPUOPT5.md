# CPUOPT5 — fleet CPU after CPUOPT4 (2026-11-13)

CPUOPT4 (2026-11-04) ranked the fleet's CPU costs. The owner approved its refill,
statistics migration, dump redesign, lease and timer items, the proxy levers and
the hosted-proxy reap, and all of them went live between 11-04 and 11-06. Since then
every service has released several times:

| Service | Build | Server revision | Running since |
|---|---|---|---|
| api | D42 `2026.11.12-outerwerld+1097289240` during the captures; D43 `+1097411970` from about 05:10Z | a151cee8d; 978dcba02 | D42 11-13 02:30–04:10Z; D43 04:50–05:10Z |
| alt | D43 `+1097411970` | 978dcba02 | 11-13 about 04:30Z |
| taskworker | D43 `+1097411970` | 978dcba02 | 11-13 03:50–04:30Z |
| connect, gossip | `2026.11.12-outerwerld+1096979900` | a00dfeb9a | 11-12 17:10Z |
| proxy | `2026.11.11-outerwerld+1096461840` | 89be8b3b8 | 11-12 04:50Z |
| mcp | `2026.11.11-outerwerld+1096212990` | 87bd6ad33 | 11-11 |

**A correction to the release notes, from git ancestry:** D42 (a151cee8d) already
carries FP2 S2's selection and export (merge 3dcc7b138: capability facets and the
`:native_cap_v1` snapshot), S1 step 1 (migration 966), the Solana receiver sweep,
the uncredited-payment metrics and the store notification flow. D43 (978dcba02)
adds S1 step 2 (post-quantum URL turns, 86553321d) and the proxy's window metrics
(4fdf026c0). The proxy, connect, gossip and mcp run neither.

This document measures the fleet again, checks what CPUOPT4's changes did against
its predictions, and ranks what is left or new. As in CPUOPT4, read-only
investigations wrote five parts, one per CPU domain:

| Part | Domain | Hosts |
|---|---|---|
| A | Services edges and connect | edge-0, edge-1, edge-3, edge-4 |
| B | Proxy hosts and the proxy service | crisp, fireside |
| C | api, alt and taskworker | the services edges, and alt on crisp and fireside |
| D | PostgreSQL | edge-2 |
| E | The Redis cluster | edge-5, edge-6 |

**Evidence:**
- Mimir, 10-28 to 11-13 05Z;
- perf captures of 30 s or less (20 s for nginx), one host at a time, only while the
  SN25 validator read finalized or applied with at least 15 minutes to its next
  boundary; data files deleted on the hosts, no perf process left;
- `perf stat` counting (no sampling) of two Redis processes, 30 s each;
- host censuses from `/proc`; Redis INFO, SLOWLOG and LATENCY deltas; PgBouncer
  `SHOW STATS`;
- `pg_stat_statements` snapshot deltas and catalog reads through `support db query`
  (read-only, one statement at a time, 30 s ceiling; no counter was reset);
- taskworker container logs for one task, ids and keys masked;
- code read at the deployed revisions.

No production state was changed. Nothing was refused.

**When it was measured:**
- **Live captures:** 11-13, 04:18–05:10Z, a Friday early morning, about 149k clients.
  The work is flatter over the day than it was in CPUOPT4: edge-2's commits at
  04Z are 23.4k/s against 24.5–25.8k/s at the evening peaks.
- **Evening peaks (18–22Z):** 11-06 to 11-12, from Mimir. Each evening's builds are
  named where they matter: connect released eight times between 11-04 and 11-12,
  the api several times a day.
- **D43:** live on the taskworker and alt during the captures. The api rolled to D43
  between about 04:50 and 05:10Z, after its capture, so no api number here is from
  D43, and no evening has run on it. D43's code beyond D42 touches the probe (S1
  step 2), retention, USDC close-out and the competition routes, none of the api's
  hot paths, so the api figures should carry over.

Section numbers inside a part are local to it: A §5.1 is Part A's §5.1. Raw outputs
and scripts are cited as `<scratch>/…` and stay in the analysis workspace.

**Owner rule (binding):** "We can't sacrifice correctness of performance timings for
the cpu savings." Nothing proposed here loosens a timeout, deadline, keepalive,
probe, detection or revocation bound. Each part lists what it rejected under that
rule. Owner decisions are respected: no new off-by-default settings, the quality
probe's UDP gate stays, one contract size, the proxy client limits. Proposals that
touch code follow connect/CODESTYLE.md: tunables are settings fields with one
default, every fix carries a deterministic regression test, and migrations are
additive and apply before the code that needs them.

## Summary

### What changed

**The fleet fell from about 333 to 239 busy cores** (3-day means, CPUOPT4's
"1 − idle" basis; 309 → 213 summing the non-idle modes). At the evening peak it ran
191–265 cores on 11-06..11-12 (246 on 11-12), against 296–354 on 11-01..11-04.

| Host | Cores | CPUOPT4 3 d to 11-04 10Z: mean (busy %) / peak % | 3 d to 11-13 04Z: mean (busy %) / peak % | Evening 18–22Z, 11-03 → 11-06..11-12 |
|---|---|---|---|---|
| edge-0 | 96 | 49.7 (51.8%) / 63.1% | 35.9 (37.4%) / 49.5% | 50.6 → 28.5–42.7 |
| edge-1 | 72 | 49.0 (68.0%) / 79.1% | 37.6 (52.3%) / 71.0% | 52.0 → 31.6–46.4 |
| edge-2 | 96 | 53.6 (55.8%) / 95.8% | 31.6 (32.9%) / 47.7% | 48.4 → 28.8–34.9 |
| edge-3 | 72 | 46.9 (65.1%) / 79.8% | 34.6 (48.0%) / 63.1% | 47.9 → 31.9–44.4 |
| edge-4 | 72 | 49.0 (68.0%) / 81.8% | 38.2 (53.1%) / 75.7% | 50.8 → 33.4–45.0 |
| edge-5 | 96 | 27.2 (28.3%) / 72.8% | 14.8 (15.4%) / 37.5% | 21.2 → 13.4–16.8 |
| edge-6 | 128 | 27.1 (21.2%) / 89.3% | 38.8 (30.3%) / 61.4% | 29.1 → 16.8–34.9 |
| crisp | 24 | 15.0 (62.6%) / 99.2% | 3.8 (15.8%) / 30.2% | 18.0 → 2.8–4.2 |
| fireside | 24 | 15.0 (62.5%) / 99.3% | 3.8 (15.9%) / 23.1% | 18.4 → 2.8–4.2 |
| **Fleet** | | **332** | **239** | **337 → 191–265** |

(`<scratch>/fleet/fleet2-1113.txt`. edge-6 rose because it now restores the base
backup that replaced the dump; E §2.5.)

**Most of the fall is work that left, and most of the work that left was one
service's:**
- **The hosted proxies shrank.** The out-of-balance refill loop ended (CPUOPT4 #1
  shipped 11-04; the payer side stopped refusing 11-05 about 12Z), the proxy client
  limits and the 11-05 trims cut live hosted devices from 37–51k to 7–15k, and the
  idle-refill hold cut transports per device from 3.6 to about 1 (B §1).
- **That took about half of connect's clients with it** (271k → 150k at the evening
  peak; exchange connections 994k → 224k), most of the proxy's database traffic
  (proxy pool acquires 15.8–19.1k/s → 0.6–0.9k/s) and a third of Redis's requests
  (185k → 115k/s) (A §3.1, D §2.1, E §2.1).

**What the services do per unit of work:**

| Domain | CPUOPT4 | Now | Why |
|---|---|---|---|
| connect, per exchange frame | 62.1 µs (11-03 evening) | 36–43 µs (11-06..11-12) | lease writer and timer fixes; D1; fewer clients |
| connect, marginal per 1k clients | 0.290 cores | 0.082 cores | the same (A §3.3) |
| api, per 1k requests/s | 1.27 cores (11-03) | 1.09–1.30 (11-07..11-11) | unchanged mix: replays (C §2.1) |
| proxy, per device / per MB of user traffic | 0.87 mcores / 8.4 CPU-s | 0.29–0.37 mcores / 0.2–0.3 CPU-s | the refill loop is gone (B §2.1) |
| PostgreSQL, marginal per 1k commits/s | 0.78 cores | 0.54–0.57 cores | no flipped plan; less eviction (D §2.1) |
| PostgreSQL, execution per statement | 167 µs | 93 µs without a pin (108 µs during the epoch read) | the same (D §2.3) |
| Redis, per request | 172–182 µs | 169–191 µs | unchanged per request and per client (E §2.1) |

### Where the CPU goes now

- **Services edges (A, C):** 140 busy CPUs at the 11-12 evening peak (186 on 11-03).
  - connect 74 cores (111); nginx about 27–34 (37 on 11-04's morning census); api 14
    (16); taskworker 14 (12); logging and observability about 12 (13).
  - **Inside connect** (two 30 s captures): H1 transports 17–23%, the scheduler 19–22%,
    exchange socket trees 10–17%, go-redis 9–10%, `ConnectionAnnounce` 4–5%, GC about
    3–4% averaged, the D1 local pumps 2–2.4%. The timer heap (1.3–1.5%) and the
    lease writer (0.07–0.16%) are gone from the top.
  - **nginx:** TLS record crypto about 30–40%, kernel socket work about 45%,
    handshakes about 2% (A §3.5).
  - **api:** `/connect/control` is 70% of requests; refusal replays are about 80% of
    its create frames a day into a connect generation (C §2.1). JWT is about 15% of an
    api process, go-redis 11%, the scheduler 21%.
  - **taskworker:** the egress probe is most of it; TLS is 37% of a probe process and
    x509 26%. The §8.6 extender shadow step takes about 1.8 cores while it runs,
    which is most of the time since 11-12 20:30Z (C §2.4).
- **PostgreSQL (D):** 29.7–32.8 busy cores at the evening peaks (42.9–52.8 at
  CPUOPT4's). At 04:31Z: backends 19.2 cores, PgBouncer 2.9, the nginx stream hop
  2.2, IO workers 2.1, dm-crypt 1.35, autovacuum 1.0. No statement is above 8.2% of
  execution time. Long snapshots from application reads are the largest swing
  (D §2.5).
- **Redis (E):** 20.4 cores on the 3-day mean (31.2), at 178 µs per request; kernel
  52%, scripts 63% of command time; no hot node (the busiest main thread is 0.27 of
  a core). edge-6's backup restore stalls two thirds of the slots every few seconds
  (E §2.5–2.6), and edge-5's Redis threads run at 1.4–1.6 GHz on a 2.3 GHz part
  (E §2.7, §5.2).
- **Proxy hosts (B):** the proxy 2.6–5.1 cores at the evening peaks (32.6 on 11-03),
  hosts 10–16% busy. A process runs 0.23 cores; TLS dials for window candidates are
  7% of it.

### What CPUOPT4's changes did

Of CPUOPT4's 21 ranked items, six shipped (#1–#6), one became unnecessary (#17), two
are moot at today's load (#9, #14), one is still deferred by the owner (#8, D4), and
eleven are not done.

| # | CPUOPT4 item | Status | Measured |
|---|---|---|---|
| 1 | Balance-aware refill | Shipped: connect cf0f3120, proxy 3c9f0fdc7, rolled 11-04 16:06–17:05Z | Proxy 40 → 17–25 cores while refusals still ran at 1.2–1.3k/s; Redis node 6383's saturation and the proxy's pool timeouts gone (E §3). Refusals 0 since 11-05 12Z, ended on the payer side. **Met** |
| 2 | D1 in-process delivery on | Owner flips 11-04: `localTransport all` 14:32Z, `localForward all` about 17:49Z, every block at once | 56% of frames now in process; exchange connections −23% at the switch. CPU **−3.6% per frame**, against the predicted 9–11% (10–13 cores). **Shipped; under-delivered on CPU** (A §4) |
| 3 | Expression statistics for the payer reads | Migration 926 applied 11-04 17:47:05Z | No flip in 8 days while the open set drained 13.45M → 115k entries; open-sum 0.15 ms (353 flipped); `/subscription/balance` 8–24 ms every hour. **Met** (D §3) |
| 4 | Hold the dump; dump a copy instead | Held 11-05; base-backup design on edge-6 (xops 4870279, ab04803, 14f77b1, 9ed4c05, e42581b, c328547) | No dump pin on edge-2 since 11-04; a copy costs edge-2 about 1–1.4 cores while it streams. The restore's ZFS writes now stall edge-6's Redis (E §5.3). **Met for edge-2** |
| 5 | Liveness-lease writer index | 101d59909, connect 7f0e0ad89, 11-04 16:22–17:20Z | 5.8–10.0% of connect → 0.07–0.16%. **Met** |
| 6 | pgx and quic-go timer re-arms | ade766d85, 3348b66ca, same release | timer-heap `adjust` self 8.8–11.2% → 0.95–1.15%. **Met** |
| 7 | nginx stream hop to PgBouncer | Not done | 2.2 cores now (5.2–6.5) (D #2) |
| 8 | D4 caches (JWT claims, chains) | Deferred by the owner | JWT about 15% of an api process; x509 26% of a probe process (C #3, #4) |
| 9 | Proxy GOMEMLIMIT | Not changed | Moot: heap 1.9 GB, GC at the 2-minute floor (B §2.2) |
| 10 | Stuck-provider signal, then recycle | Built, off on every block | Replays still climb 0.5k → 9–14.5k/s per connect generation (C #1) |
| 11 | Logging and observability off the services edges | Not done | About 12 cores (A §5.2) |
| 12 | No preflight PING by default | Not done | PING 13–14k/s (32k): 1.0–1.7 Redis cores now (E §5.1) |
| 13 | One ContractOriginNotifications per proxy process | Not done | 0.1–0.2 cores now (B §3) |
| 14 | FindProviders2 page hits without the Redis read | Not done | Moot: proxy page lookups 1,242 → 16/s (E §3) |
| 15 | Reboot edge-2 into the staged kernel options | Not done | 1–2.5 cores *(unmeasured)* (D #3) |
| 16 | PgBouncer over a Unix socket | Not done | 0.7–1.5 cores *(unmeasured)* (D #5) |
| 17 | Payer drain page read | Not done; **not needed**: #3 fixed the read | `ClosePayerDueContractsTask` errors 156–221/h → 0 from 11-05 (C §3, D §3) |
| 18 | Fewer round trips (PostgreSQL; GETEX and in-MULTI prune) | Not done | 0.5–1 edge-2 cores, 0.25–0.45 Redis (D #6, E §5.6) |
| 19 | EVAL → EVALSHA | Not done | 0.25–0.4 Redis cores (E §5.7) |
| 20 | Terminate H1 in connect | Not done | nginx is now 19–23% of services-edge busy (A §5.1) |
| 21 | Multiplexed links | Not done | 3–5 cores (A §5.6) |

The owner's 11-05 levers shipped too: the idle-refill hold and active-only WireGuard
restore (server c65ed59fa; proxy from 11-06 15:30Z: restored WireGuard peers 823k →
33–45k), the 45-minute idle hold kept, and the hosted-proxy 2-day hard delete
(91f14f258; taskworker from 11-06 20:20Z). CPUOPT4's smaller items (the lag-guest
decode, GOGC and GOMAXPROCS A/Bs, one probe control plane, queue-table analyze
thresholds, partial-index rebuilds, JIT, GetTransferStats, exclusions publish on
change, slot statistics, the small code items) are all not done.

### New costs from recent releases

1. **The §8.6 extender shadow step loops, and pins the primary each time** (C §2.4,
   D §2.5). `StLedgerV2UsageStep` (extender_shadow, epoch 4) reads the epoch's 7 days
   of contracts in one REPEATABLE READ snapshot on the maintenance pool. It is cut at
   1,800 s and claimed again; 13 reads since 11-12 20:30Z, none finished, nothing
   recorded.
   - **Why it never ends:** the pending row was scheduled by d1ea65129 with the
     30-minute bound. D42's 2-hour `ShadowMaxTime` never reached it: the sync
     schedules `IfAbsent` and the framework's error reschedule keeps the row's bound,
     never runs the Post that counts attempts, and has no error limit.
   - **What each read costs:** about 1.8 taskworker cores, mostly JSON decoding of
     contract usage snapshots, and +5 to +14 edge-2 cores by its end: the snapshot's
     pin tax on the debit journal and the probe-slot claims (four statements cost
     0.22 backends without a pin and 1.74 twenty minutes into one; all statements
     6.71 against 8.68).
   - **Averaged:** about 1.5 taskworker and 2–5 edge-2 cores, without end until it is
     fixed.
2. **edge-6's base-backup restore** (CPUOPT4 #4's design) runs beside 32 Redis nodes:
   6–7 cores of ZFS and PostgreSQL work during the replay, 30–53 cores of system
   time during the 18-hour copy, and 10–470 ms stalls on two thirds of the slots,
   every 5 s at the ZFS transaction-group interval (E §2.5–2.6). No client errors.
3. **FP2 S2's health-window aggregate** costs 0.21 edge-2 backends (2.37 s per call,
   21% of it JIT); the SN25 amounts read 0.19 (1.17 s per call, 0.013 ms on 11-04);
   the descendant check on every deactivation, the fix for the 11-06 walk, 0.47
   (D §4).
4. **Small or none:** D42's `:native_cap_v1` (+1.1M Redis keys, +0.15–0.25 GB, no
   readers yet, CPU below detection; no change in the export task's run time), S1's
   history writes (0.011 backends), the post-quantum turns (ML-KEM 1.1% of a probe
   process), the h3 and IP-family legs (quic-go 0.5% of a probe process), the Solana
   receiver sweep, the uncredited-payment metrics, the store notification flow, the
   feature-use marks and the client-limit counter.

### Corrections to CPUOPT4

- **D1's CPU value.** CPUOPT4 estimated 10–13 cores from the exchange trees' share and
  the same-process locality. Measured across the switch, in one connect generation, it
  saved 3.6% per frame. The sockets, goroutines and conntrack entries fell as
  predicted; the CPU did not (A §4).
- **The payer drain** needed no change of its own: the statistics fix (#3) put its read
  back on the partial index (CPUOPT4 C #4).
- **The PING excess** that CPUOPT4 left unattributed is not go-redis's pub/sub health
  checks (at most 105 commands/s): contract-hole calls are 73–76% of PINGs, the other
  32 preflighting sites the rest (E §2.4).

### Noticed, not CPU

- **FindProviders2 at the api** went from about 40 ms to 105–140 ms mean at 19:10–19:40Z
  on 11-12, with no api deploy: its `select` stage went from 0.33 ms to about 40 ms on
  average, because about 0.04 requests/s now run past 10 s and are canceled; 99.9% of
  selects still take 5 ms or less. Selection is 0.07% of api CPU, so this is a wait.
  Cause not determined (C §6).
- **The Sunday payout pin is retired** by D42 (7475aa85f): it cost 94–183 edge-2
  core-hours on each of the last two Sundays (D §2.5). Check 11-15 10–14Z.

## Ranked opportunities across the fleet

Savings are cores at the evening peak, which is now close to the daily mean, unless
noted. Items overlap where noted; do not add them all.

| # | Change | Where | Est. saving | Risk |
|---|---|---|---|---|
| 1 | End the extender shadow loop: raise the stale row to the bound the code declares (or retire epoch 4's shadow), count timed-out attempts, then read the epoch in short snapshots | C #2, D #1 | 3.5–6.5 now (1.5 taskworker, 2–5 edge-2), more while a read runs | low (a, b); medium (slicing) |
| 2 | Stuck-provider signal, then recycle, then bake them in (CPUOPT4 #10) | C #1, E §5.5 | about 3.5–4 averaged (api 2.5, Redis 0.8–1.4, nginx part), up to about 9 at the top of a sawtooth *(inferred)* | medium; built |
| 3 | Terminate H1 in connect, with no nginx relay (CPUOPT4 #20) | A §5.1 | 10–15 *(inferred)* | high |
| 4 | Logging and observability off the services edges (CPUOPT4 #11) | A §5.2 | 3–5 | low |
| 5 | D4: (b) verified-chain cache, (a) JWT claims cache (CPUOPT4 #8) | C #3, #4 | 2.5–2.9 taskworker and proxy; 1.8–2.4 api | low–medium; owner deferred |
| 6 | A performance clock policy on the Redis hosts | E §5.2 | 1.0–1.7 on edge-5 now; 1.8–2.5 on edge-6 whenever no restore runs *(inferred)* | low; owner (power) |
| 7 | GOMAXPROCS A/Bs: connect 16 → 8, api 8 → 4 | A §5.3, C #5 | 2–4 and 0.5–1 *(inferred)* | low–medium |
| 8 | Remove the nginx stream hop to PgBouncer (CPUOPT4 #7) | D #2 | 2.0–2.5 | medium |
| 9 | No preflight PING by default (CPUOPT4 #12) | E §5.1 | 1.0–1.7 Redis, plus connect round trips | low |
| 10 | Rebuild transfer_contract's bloated indexes, the drained partials first (O2) | D #4 | 1–1.5 *(inferred)*, plus disk | medium (one snapshot per build) |
| 11 | Reboot edge-2 into `retbleed=stuff iommu=pt` (CPUOPT4 #15) | D #3 | 1–2.5 *(unmeasured)* | medium (restart) |
| 12 | One reliability-stats write per shard per process per round | A §5.4 | 0.5–1 connect, plus about 0.4 Redis | low |
| 13 | PgBouncer over a Unix socket (CPUOPT4 #16) | D #5 | 0.7–1.5 *(unmeasured)* | low |
| 14 | Fewer round trips: PostgreSQL per transaction; GETEX and the in-MULTI prune (CPUOPT4 #18) | D #6, E §5.6 | 0.5–1 edge-2; 0.25–0.45 Redis | low–medium |
| 15 | Move edge-6's restore off Redis's CPUs: clone at 8K records, ZFS writers at normal priority at the next reboot, or another host | E §5.3 | 0 Redis; 6–7 edge-6 cores while it replays; removes the stalls | low–medium; owner |
| 16 | One control-plane strategy per probe process (CPUOPT4 C #8) | C #7 | about 1 | medium |
| 17 | Lag-guest claim without the claims decode (void with D4(a)) | C #6 | 0.5–0.6 | low |
| 18 | EVAL → EVALSHA, the net-escrow refresh added (CPUOPT4 #19) | E §5.7 | 0.25–0.4 | low |
| 19 | Small database items: an index for the SN25 amounts read; a narrower descendant check; JIT off; GetTransferStats and the health aggregate | D #7–#10 | 0.85–1.2 together | low–medium |
| 20 | `kernel.numa_balancing=0` on edge-6 (CPUOPT4 E §5.3) | E §5.4 | 0.15–0.25, plus a stall source | low; owner |
| 21 | Multiplexed links for the remaining cross-process exchange (CPUOPT4 #21) | A §5.6 | 3–5 | high |

**Latency only:** `{provider_hard_exclusions}` published on change removes a 118 ms
whole-node block every 12 s on one Redis node (E §5.8).

**Totals:**
- **The code and configuration items that need no product decision** (#1, #2, #4, #7,
  #9, #12, #14, #16–#19) come to roughly 18–28 cores, 7–12% of the fleet's 239-core
  mean. #1 alone is the cheapest large item, and it costs cores every hour now.
- **Database-path items needing windows** (#8, #10, #11, #13): about 5–8 cores.
- **Owner decisions** (#5 D4, #6 clocks, #15 restore placement, #20): about 5.5–10
  cores, 6–7 more edge-6 cores while a restore replays, and the end of edge-6's
  Redis stalls.
- **High-risk structural items** (#3, #21): 13–20 cores, with design work.

### Themes

1. **Loops that produce nothing are still the cheapest wins.** CPUOPT4's were the
   out-of-balance refill and the payer drain; both are gone. This round's is the
   extender shadow step, and it repeats CPUOPT3's other lesson: a long snapshot on
   the primary costs far more than the read that holds it.
2. **The replay sawtooth is now the api's largest removable cost,** and it also loads
   Redis and nginx. The fix is built and has never been switched on.
3. **The services edges are now mostly I/O plumbing.** With the lease writer and the
   timer heap gone, connect's top items are its H1 relay and scheduler, and nginx is
   a fifth of these hosts. The remaining large saving is structural (#3).
4. **Host posture matters more on the Redis hosts than any Redis code item:** edge-5
   runs its Redis threads at 1.4–1.6 GHz, and edge-6 shares its CPUs with ZFS writers
   at nice −19.
5. **Work moved, so per-client figures mislead.** Clients halved, but connect's frames
   and the api's control requests did not. Normalize by frames, requests and commits.

## Decisions for the owner

1. **The extender shadow loop (#1), now.** Either retire epoch 4's §8.6.7 shadow
   report, or approve the fix that raises the pending row to its 2-hour bound and
   counts timed-out attempts. Until then each read pins the primary for 30 minutes,
   about every 1–2 hours.
2. **The stuck-provider signal, then the recycle (#2),** staged by block, outside the
   validator's busy minutes.
3. **D4 (#5).** Still deferred over storage; both caches are in-process memory. Now
   worth about 4.3–5.3 cores together.
4. **The Redis hosts' clock policy (#6):** power and heat against 1–4 cores and lower
   command latency. Confirm first with a day on one host.
5. **edge-6's restore (#15):** clone at 8K records at the next run; ZFS write-thread
   priority at the next planned reboot (32 Redis nodes without replicas); or another
   host for the restore.
6. **Database-path windows:** the stream hop (#8), the Unix socket (#13), the edge-2
   reboot (#11), and the index rebuilds (#10, planned as O2).
7. **Host posture:** `kernel.numa_balancing=0` on edge-6 (#20); `iommu=pt` on edge-5;
   per-slot statistics; the conntrack bypass (E §5.9).
8. **The H1 termination design (#3):** whether to start it, one block on dedicated
   addresses first.
9. **GOMAXPROCS A/Bs (#7)** on one block each.

## Validating a change

CPUOPT3's and CPUOPT4's rules hold:
- **Normalize by the work:** connect per exchange frame (not per client, now that the
  client mix moved), the api and Redis per request, PostgreSQL per commit and per
  statement, the proxy per device and per MB.
- **Compare like with like:** the same hours over at least three days, and A/B on one
  block where a change can be staged.
- **Hold the timing bounds as acceptance criteria,** not just the CPU.
- **Measure from Mimir and perf together,** not one short profile.

Two more from this round:
- **Check for long application snapshots before comparing edge-2 hours:**
  `pg_stat_activity_max_tx_duration` for the application user, and the taskworker's
  in-flight `StLedgerV2UsageStep` (D §2.5).
- **Mind the connect generation's age** when comparing api or Redis evenings: the
  replay sawtooth adds up to 7 api cores and 2.7 Redis cores at its top.

## Part A — Services edges and connect

Domain: by-us-fmt-5-edge-0/1/3/4 (312 logical CPUs) and the connect service.
Measured read-only on 11-13 between 04:18Z and 04:53Z, and from Mimir back to
10-31. Connect runs `2026.11.12-outerwerld+1096979900` (server a00dfeb9a) since
11-12 17:10Z. Raw outputs are in `<scratch>/a/` and `<scratch>/prof/`.

### 1. Summary

- **Services edges at the evening peak: 186 → 140 busy CPUs** (11-03 against
  11-12, 18–22Z, sum of non-idle modes). Connect fell from 111 to 74 cores, and
  the other Go jobs stayed flat (api 15.6 → 13.9, taskworker 12.1 → 13.8).
  Everything that is not a Go process (nginx, logging and observability,
  unattributed kernel work) fell only from 46 to 39 cores.
- **The work changed shape.** Clients fell from 271k to 150k, exchange
  connections from 994k to 224k and H1 session starts from 781 to 483/s, mostly
  the proxy's hosted window clients (Part B). Exchange frames rose from 1.79M to
  2.03M/s, and 56% of them (1.14M/s) now move in process (D1).
- **Per unit of work connect is much cheaper.** 62 → 36–43 µs per exchange
  frame, and the hourly model's per-client term fell from 0.29 to 0.08 cores per
  1k clients (§3.3).
- **CPUOPT4's three connect items all shipped on 11-04, and the profiles confirm
  two of them removed what they targeted:**
  - the liveness-lease writer index (101d59909): 5.8–10.0% of connect → 0.07–0.16%;
  - the pgx and quic-go timer re-arms (ade766d85, 3348b66ca): timer-heap `adjust`
    self 8.8–11.2% → 0.95–1.15%, `timers.check` inclusive 12–15% → 2.9–3.1%;
  - D1 in-process delivery (owner flips: `localTransport all` 14:32Z, `localForward
    all` about 17:49Z on 11-04) moved 56% of frames off sockets, but its CPU effect
    was small: −3.6% per frame across the switch, against CPUOPT4's predicted
    9–11% (§4).
- **What connect spends now** (two 30 s captures, 4.3 and 5.3 cores):
  - H1 transports 17–23% (`writeConnectH1UserReadyBatch` 6–8%, the pooled read 4–7%);
  - the scheduler 19–22% (`findRunnable` inclusive; `stealWork` 6–8%);
  - exchange socket trees 10–17%, D1's local pumps 2–2.4%;
  - go-redis 9–10%, led by `GetResidentForClient` (1.6–3.7%);
  - `ConnectionAnnounce` 3.7–4.8%; GC about 3–4% averaged (a 16.7% cycle was
    caught in one capture; heaps are 18–24 GB, collected at the 2-minute floor).
- **nginx is now the second-largest consumer on these hosts:** 6.1–9.8 cores per
  edge, about 27–34 cores in all, against connect's 74. Of nginx: TLS record
  crypto about 30–40%, kernel socket work about 45%, handshakes about 2% (§3.5).
- **Ranked opportunities** (services-edge cores at the evening peak; they overlap):

| # | Change | Est. saving | Risk |
|---|---|---|---|
| 1 | Terminate H1 in connect, with no nginx relay (CPUOPT3 #19 / CPUOPT4 #20) | 10–15 | high |
| 2 | Logging and observability off the services edges (CPUOPT4 #11) | 3–5 | low |
| 3 | GOMAXPROCS 16 → 8 for connect, A/B on one block | 2–4 *(inferred)* | low–medium |
| 4 | One reliability-stats write per shard per process per round, not per connection | 0.5–1 connect, plus Redis (Part E) | low |
| 5 | `GetResidentForClient`: GETEX for the heartbeat (E §5.6), and find why its share tripled | up to 1 *(inferred)* | low |
| 6 | Multiplexed links for the remaining cross-process exchange (CPUOPT4 #21) | 3–5 | high |

### 2. Method

- **Load and builds.** Captures ran 11-13 04:18–04:53Z, early-morning load:
  148.7k clients, 11.7k api req/s, the services edges 34.9–37.4 busy CPUs each.
  Connect 1096979900 (a00dfeb9a) was 11–12 h old. Comparisons with CPUOPT4 use
  Mimir evenings (18–22Z) and the hourly model, normalized by the work.
- **Releases.** Connect released eight times between 11-04 and 11-12
  (`<scratch>/fleet/gens.txt`), so every evening after 11-04 is on a connect
  generation 0.5–48 h old. Connect has carried the lease writer and the timer
  forks since 7f0e0ad89 (11-04 16:22–17:20Z).
- **Captures** (all read-only; data files deleted on the hosts; no perf process
  left, checked by each script):
  - host census on all four edges, 04:18Z (`/proc` over 5–10 s; `<scratch>/a/census_0440.txt`);
  - perf, one connect process, 30 s at 199 Hz with call graphs: edge-3 g4 at
    04:20:45Z (30.3k samples, 4.27 cores), edge-0 g2 at 04:41:03Z (34.8k samples,
    5.31 cores) (`<scratch>/prof/perf_conn_*.txt`);
  - perf of all nginx workers on edge-4, 20 s at 99 Hz, 04:51Z (480 workers, 9.83
    cores; `<scratch>/prof/perf_nginx_edge4.txt`);
  - each capture was taken only while the SN25 validator read finalized or
    applied with at least 15 minutes to the next boundary
    (`<scratch>/prof/validator_idle.sh`); one was deferred for 20 minutes for it.
- **Not captured:** no bpftrace this round (CPUOPT4 used it for the timer census;
  the perf shares are enough to score #6). No evening captures: the evening
  peak is 14 hours after this round's window.

### 3. Where the CPU goes now

#### 3.1 Hosts, matched evenings

Means over 18–22Z (Mimir; `<scratch>/a/connect_eve.txt`):

| Evening (connect build) | Clients | Busy CPUs | Kernel % | Connect | api | taskworker | H1 starts/s | Exchange conns | Frames/s: io transport / io forward / local | µs per frame | Connect per 1k clients |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-31 (37ec4788b, CPUOPT3) | 209k | 177.8 | 39.0 | 102.6 | 18.5 | 12.3 | 623 | 990k | 1.79M (not split) | 57.6 | 0.491 |
| 11-03 (1e94b4d78, CPUOPT4) | 271k | 186.3 | 37.8 | 110.7 | 15.6 | 12.1 | 781 | 994k | 1.20M / 0.58M / 0 | 62.1 | 0.408 |
| 11-06 (5e6d6d65b, rolled 17:30–18:20Z) | 150k | 105.0 | 40.8 | 52.3 | 9.3 | 10.5 | 346 | 191k | 0.18M / 0.41M / 0.74M | 39.5 | 0.348 |
| 11-08 (5e6d6d65b; 562ead790 from 20:40Z) | 147k | 131.4 | 40.4 | 62.0 | 15.1 | 12.6 | 416 | 215k | 0.27M / 0.49M / 0.85M | 38.7 | 0.421 |
| 11-10 (4a7aecd9b) | 155k | 146.1 | 40.5 | 69.1 | 18.5 | 13.3 | 412 | 240k | 0.33M / 0.53M / 0.86M | 40.3 | 0.446 |
| 11-11 (079dc1d3f; 87bd6ad33 from 21:50Z) | 142k | 155.2 | 42.3 | 81.9 | 17.2 | 12.5 | 466 | 395k | 0.68M / 0.59M / 0.65M | 42.6 | 0.576 |
| 11-12 (a00dfeb9a) | 150k | 140.1 | 41.3 | 73.5 | 13.9 | 13.8 | 483 | 224k | 0.26M / 0.64M / 1.14M | 36.1 | 0.490 |

Busy here sums the non-idle modes. On CPUOPT4's "1 − idle" basis the four edges
read 3.5–4 CPUs higher each (the summary's fleet table).

- **Connect grows with frames now, not clients.** From 11-06 to 11-12 clients
  were flat (139–155k) while frames rose 1.33M → 2.03M/s and connect 52 → 74 cores.
- **The local share varies by generation:** 49–56% of frames on most evenings,
  34% on 11-11, when the 07:00Z and 21:50Z releases left residents spread over
  other processes. Exchange connections follow it (395k against 191–240k).
- **Per g-process:** 3.3–4.9 cores at the evening peak (6.75 on 11-03), 0.40–0.51M
  goroutines (0.94M), heap in use 17.5–23.8 GB (45 GB), allocation 58–77 MB/s
  (158 MB/s). GC runs 0.49–0.52 times a minute, the runtime's 2-minute floor, on
  every evening before and after.

#### 3.2 By process (census, 11-13 04:18Z)

| Host | connect | nginx | api | taskworker | logging and observability | Host busy (10 s) |
|---|---|---|---|---|---|---|
| edge-0 | 18.8 | 6.1 | 3.3 | 1.3 | 2.9 (loki 1.4, dockerd 0.5, mimir 0.3) | 36.5 |
| edge-1 | 18.5 | 6.3 | 2.9 | 5.7 | 2.6 (loki 0.8, dockerd 0.4, fluent-bit 0.4) | 36.3 |
| edge-3 | 15.8 | 6.8 | 5.5 | 6.0 | 3.9 (loki 2.2, dockerd 0.6) | 37.4 |
| edge-4 | 19.5 | 7.5 | 4.6 | 0.2 | 2.5 (dockerd 0.6, loki 0.5, mimir 0.4) | 34.9 |
| **Sum** | **72.6** | **26.7** | **16.3** | **13.2** | **11.9** | **145.1** |

- nginx held about the same per host while clients halved (37.4 cores for 287k
  clients on 11-04 at 10:35Z; 26.7 for 149k now): its cost follows bytes and TLS
  records more than sessions.
- Logging and observability is unchanged at about 12 cores (CPUOPT4: about 13).
- The evening split from Mimir agrees: busy minus the Go processes is 38.5 cores at
  the 11-12 evening (45.8 on 11-03), which is nginx, logging and kernel work
  outside processes.

#### 3.3 The connect load model

Hourly least squares, hours with every connect process older than 1 h
(`<scratch>/a/fit_post.txt`, `fit_pre.txt`):

| Generation span | Clients (k) | Frames (k/s) | H1 starts (/s) | Intercept | rms |
|---|---|---|---|---|---|
| CPUOPT4's, 11-02 20Z – 11-04 14Z (42 h) | 0.290 | 0.0145 | 0.026 | −12.7 | 2.9 |
| Since, 11-06 – 11-13 04Z (161 h) | 0.082 | 0.0263 | 0.034 | −4.6 | 2.3 |

- The per-client term fell 72%: the lease writer and the timer heap both scaled
  with connections held.
- The frame term rose; the models are fitted on very different client counts and
  the two terms are collinear, so read the pair, not either term. Cross-predicted,
  the new model gives 91 cores for the 11-03 evening's work (111 observed), about
  −18% at matched work *(extrapolated in clients)*.

#### 3.4 Inside connect

Shares of one process's samples (`<scratch>/prof/perf_conn_edge3_g4.txt`,
`perf_conn_edge0_g2.txt`), against CPUOPT4's four captures:

| Category | e3 g4 (4.27 cores) | e0 g2 (5.31 cores) | CPUOPT4 (7.1–8.5 cores) |
|---|---|---|---|
| Timer heap: `check` incl / `adjust` self / sift self | 3.1 / 1.15 / 0.35 | 2.9 / 0.95 / 0.33 | 12.2–15.5 / 8.8–11.2 / 0.9–1.2 |
| Liveness-lease writer | 0.16 | 0.07 | 5.8–10.0 |
| `findRunnable` incl / `stealWork` incl | 18.6 / 6.1 | 22.1 / 8.4 | 21.5–24.7 / — |
| `lock2`+`unlock2` self | 2.2 | 3.5 | 1.9–2.3 |
| H1 transports (`ConnectHandler.Connect`) | 16.9 | 22.6 | 13.2–16.1 |
| Exchange socket trees | 9.8 | 17.3 | 18.4–21.7 |
| D1 local transport and forward pumps | 1.9 | 2.4 | 0 (off) |
| go-redis | 9.2 | 10.2 | 6.7–9.2 |
| pgx | 1.9 | 2.0 | 1.7–2.5 |
| `ConnectionAnnounce.run` | 4.8 | 3.7 | 3.8–4.8 |
| `GetResidentForClient` | 1.6 | 3.7 | 0.9–1.0 (Redis part) |
| `ReadContractHoleLease` | 1.1 | 1.7 | 1.2–3.2 |
| GC (mark, assists, sweep) | 16.7 (a cycle caught) | 0 | 0–9.1 |
| Kernel leaf, all | 38.9 | 43.2 | 34.4–37.5 |
| `tcp_sendmsg` / softirq / `nf_hook_slow` (k) | 8.7 / 10.0 / 3.6 | 13.1 / 8.7 / 4.5 | — |

- **The H1 path is now the largest single tree** (17–23%), and the kernel's share
  of connect rose to 39–43% as the user-space overheads shrank.
- **go-redis callers** (edge-0 g2): `GetResidentForClient` 3.06%, go-redis's own
  goroutines 1.74% (pub/sub readers and health checks), `ReadContractHoleLease`
  0.72%, `AuthorizeStream` 0.70%, `RecordClientReliabilityStatsRange` 0.41–0.83%,
  `GetProvideModes` 0.39–0.71%, the verify-egress lease family about 0.8%.
- **The scheduler** is 19–22% inclusive at 4–5 cores per process with
  GOMAXPROCS 16: idle Ps spin in `stealWork` and `netpoll` between bursts (§5.3).

#### 3.5 nginx

One 20 s capture of all 480 nginx workers on edge-4 (9.83 cores, host 59% busy):

| Where | Share of nginx samples |
|---|---|
| Kernel (leaf) | 55.9% (syscall entry inclusive 46.8%, `tcp_sendmsg` 18.6%, softirq 14.4%, epoll 12.3%) |
| libcrypto / libssl (any frame) | 39.1% / 46.4% |
| TLS record read and write (`SSL_read`/`SSL_write` inclusive) | 29.5% |
| Handshakes: ECDHE and signatures | 0.3% + 1.6% |
| nginx's own code, libc | 10.6% + 11.6% (leaf) |

nginx is stripped, so its own frames are unattributed; libssl and libcrypto have
their exported symbols only. Handshakes are a small part: nginx's cost is
relaying records, twice per byte (client side TLS, upstream side plaintext).

### 4. What CPUOPT4's connect items did

| CPUOPT4 item | Shipped | Measured | Verdict |
|---|---|---|---|
| #2 D1 on (approved rollout beta 1 day, g1 3 days, then all) | Owner flips 11-04: `localTransport all` 14:32Z, `localForward all` ~17:49Z, every block at once (HANDOFF-1103-2) | Same generation, 12:30–14:25Z against 14:45–16:15Z: transport io frames 1.16M → 0.70M/s, local 0.52M/s, exchange connections 980k → 751k, connect 117.5 → 118.3 cores at +4% frames: **−3.6% per frame**. Now 1.14M/s local of 2.03M/s, exchange trees 10–17% (18–22%) | Shipped; CPU effect well under the predicted 10–13 cores (9–11%). Sockets and goroutines fell as predicted |
| #5 liveness-lease writer index | 101d59909, connect 7f0e0ad89, 11-04 16:22–17:20Z | 5.8–10.0% → 0.07–0.16% of connect | Met (predicted 6–9 cores; worth about 6–8 cores at 11-03's scale) |
| #6 pgx and quic-go timer re-arm | ade766d85, 3348b66ca (forks in third_party/pgx and third_party/quic-go, by go.mod `replace`), same release | `adjust` self 8.8–11.2% → 0.95–1.15%; `check` incl 12–15% → 2.9–3.1% | Met (predicted 6–9 cores; worth about 8–10 at 11-03's scale) |
| A §5.4, the 11-04 H1+ churn | The balance-aware refill and the hosted-device reductions (Part B) | H1 starts 781–900/s → 346–483/s | Gone |
| #11 logging off the services edges | Not done (no xops change since 11-04) | About 12 cores | Still worth 3–5 |
| #20 terminate H1 in connect | Not done | nginx 27–34 cores now | Larger share; §5.1 |
| #21 multiplexed links | Not done | Cross-process exchange 0.26M + 0.64M io frames/s at the 11-12 evening | §5.6 |
| A §5.8 GOGC 150 | Not done | GC runs at the 2-minute floor on 18–24 GB heaps: GOGC would change nothing | Dropped |
| A §5.9 key-event merge buffer and resync | Not done | Key-event subscriber 0.5–0.6% of connect | Dropped (under 0.5 core) |
| A §5.10 `encodeUuid` through fmt | Not done | 0.42–0.48% of connect | Small, still valid |

Net: connect at the evening peak went from 111 to 52–82 cores (74 on 11-12).
Most of the fall is work that left with the proxy's hosted windows; the rest is the
per-client cost the lease and timer fixes removed (§3.3).

### 5. Ranked opportunities

#### 5.1 Terminate H1 in connect, with no nginx relay (CPUOPT4 #20)

- **The change.** As CPUOPT3 §6.6: connect listens for H1 and H1+ with TLS itself
  (Go crypto/tls with the same certificates and session settings), one block on
  dedicated addresses first.
- **Evidence:** nginx is 26.7 cores (census) to about 34 (edge-4's 9.8 × 4) on the
  four edges, 19–23% of their busy CPU. About 95% of nginx is record relay work (§3.5),
  done once on the client socket and again on the upstream socket; connect also
  pays its side of the upstream socket (`readConnectH1PooledWithDeadline` 4–7%,
  half of `tcp_sendmsg`).
- **Saving:** the upstream half of nginx's kernel work and its user-space relay
  (about 55–65% of nginx) less Go's TLS record work in connect (about 30% of
  nginx's equivalent): **10–15 cores** *(inferred)*, larger than CPUOPT4's 8–12
  because nginx is now a larger share.
- **Risk:** high. Ports, the lb's rate limits and deploy port rotation move into
  connect; TLS moves into Go.
- **Why no timing bound moves:** the same H1 deadlines, pings and keepalive, in
  connect instead of a relay.
- **Validation:** one block on dedicated addresses; nginx plus connect cores per
  1k H1 sessions and per MB; H1 handshake and session-close reasons unchanged.

#### 5.2 Logging and observability off the services edges (CPUOPT4 #11)

Unchanged from CPUOPT4: about 12 cores across the four edges (Loki 0.5–2.2 per
host in bursts, dockerd 0.4–0.6, journald, fluent-bit, Mimir). Move Loki, Mimir
and Grafana to edge-6 or edge-5 once those hosts' own work allows (Part E), and
switch the docker log driver to `local`. **3–5 cores.** Low risk. No timing bound.

#### 5.3 GOMAXPROCS 16 → 8 for connect, on one block

- **The change.** `GOMAXPROCS=8` for the connect service on one block (warpctl
  service settings), A/B against the other g-blocks for 3 days.
- **Evidence:** a g-process runs 3.3–4.9 cores at the evening peak with 16 Ps;
  `findRunnable` is 18.6–22.1% inclusive and `stealWork` 6–8%, `lock2`/`unlock2`
  2.2–3.5% self. Five connect processes share each host with the api and the
  taskworker.
- **Saving:** a third to a half of the spinning, **2–4 cores** *(inferred)*.
- **Risk:** low–medium. It caps a process at 8 cores; the 11-03 evening needed 6.75
  per process, and the GC's background workers take a quarter of the Ps during a
  mark. Revert on any rise in H1 write-batch latency or receive-queue drops.
- **Why no timing bound moves:** a runtime knob; acceptance requires the H1 and
  exchange deadlines' outcomes to be unchanged.
- **Validation:** cores per frame on the block against the others, `findRunnable`
  share in perf, p99 of the H1 write batch, `h1_sessions_closed_total{reason}`.

#### 5.4 One reliability-stats write per shard per process per round

- **The change.** `ConnectionAnnounce` writes each connection's counters every
  sync round (`SyncConnectionTimeout` = 30 s; connect/transport_announce.go:99,
  :865) through `RecordClientReliabilityStatsRange`
  (model/network_client_reliability_model.go:315), one MULTI/EXEC per connection
  with up to eight `HINCRBY`s and an `EXPIRE` on one of 32 shard keys. Gather the
  rounds in the process and flush one pipeline per shard key per round instead,
  as the liveness-lease writer does; one `EXPIRE` per key.
- **Evidence:** about 150k connections give about 5k transactions/s fleet-wide, most
  of Redis's 8.5k EXEC/s at the 11-12 evening, with 35k HINCRBY/s and 13.5k
  EXPIRE/s (E §2.4); 0.41–0.83% of connect in go-redis.
- **Saving:** 0.5–1 connect core and Redis request overhead (Part E).
- **Risk:** low: best-effort counters already; a flush stays inside the block the
  rollup has not drained (the rollup waits two full blocks). Per CODESTYLE.md, the
  flush interval is a field of the writer's settings with one default, and the
  change carries a deterministic test that the per-block sums equal today's.
- **Why no timing bound moves:** the counters land in the same 1-minute blocks; the
  sync round's read and its cadence are unchanged.
- **Validation:** `RollupClientReliabilityStats` totals per block unchanged against
  a shadow sum; Redis EXEC rate.

#### 5.5 Fewer resident lookups

- `GetResidentForClient` (model/network_client_resident_model.go:51) is a GET plus
  an optional EXPIRE. It is 1.6–3.7% of connect now, against about 1% in CPUOPT4.
  Its callers: the resident heartbeat (`refreshResidentRegistration`,
  connect/resident.go:1411, every `ExchangeResidentTtl/4` = 75 s, with the EXPIRE),
  the announce's peer registration and its `registerWhenResident` poll
  (connect/transport_announce.go:664, :941), and the replacement checks
  (connect/resident.go:1403, connect/resident_move.go:562).
- **The change:** GETEX for the heartbeat, as Part E §5.6 proposes (one round trip
  instead of two, the same TTL). Then split the rest of the calls by caller with a
  counter before changing anything: in one capture only 0.67 of its 3.06 points
  came through the heartbeat.
- **Saving:** part of Part E §5.6, and up to about 1 connect core *(inferred)*.
  Low risk. No cadence moves; the 75 s poll and the 300 s TTL stay.

#### 5.6 Multiplexed links for the remaining cross-process exchange (CPUOPT4 #21)

0.26M transport and 0.64M forward io frames/s still cross sockets at the 11-12
evening (0.68M + 0.59M on 11-11). Exchange socket trees are 10–17% of connect.
**3–5 cores**, high risk (protocol version, mixed-version rollout, head-of-line
blocking), as CPUOPT4 said. Lower in rank than §5.1.

#### 5.7 Small items

- `encodeUuid` through fmt: 0.42–0.48% of connect (CPUOPT4 A §5.10).
- `windowPacingService.measureEvidenceWithLock`: 0.52–0.59% self.
- `messagePool.take`: 0.49–0.56% self.

### 6. Considered and rejected

As CPUOPT4 A §7, unchanged: no longer exchange, H1 or resident deadlines, no
slower lease or announce cadences, no coarser go-redis deadlines, no lazy
deadline re-arm, no relaxed service-runaway ceiling. Also:
- **GOGC above 100 for connect:** GC is at the 2-minute floor; it would buy nothing.

## Part B — Proxy hosts and the proxy service

Domain: crisp and fireside (24 CPUs each) and the 20 proxy processes (10 per host).
Measured read-only from Mimir, 10-31 to 11-13 04:00Z, plus one perf capture
(§2.3). The proxy runs `2026.11.11-outerwerld+1096461840` (server 89be8b3b8) since
11-12 04:50Z. Raw outputs are in `<scratch>/b/`.

### 1. Summary

- **The proxy went from the fleet's surge to a rounding error.** It used 32.6 cores
  at the 11-03 evening peak and 43.5 pinned on 11-04. At the evening peaks of
  11-06 to 11-12 it used 2.6–5.1 cores, and both hosts sit at 10–16% busy.
- **Three things did it, in this order:**
  1. **The out-of-balance loop ended** (B §3.2 of CPUOPT4). The balance-aware refill
     shipped 11-04 16:06–17:05Z and cut the proxy from about 40 to 17–25 cores
     while refusals still ran at 1.2–1.3k/s. The payer side ended the refusals at
     11-05 about 12Z; they have been 0 since.
  2. **Far fewer hosted devices.** The proxy client limits (one network 05:27Z, all
     06:45Z on 11-05), the 11-05 trims (123k idle proxies on one network; 218k on
     2,362 free networks), the proxy restarts of 11-05 06:30–09:10Z and the reap
     took live devices from 37–51k (93k in the 11-05 01–08Z create flood) to 7–15k.
  3. **The idle-refill hold and active-only WireGuard restore** (server c65ed59fa,
     live on every block from 11-06 15:30Z): transports per device fell from 3.6
     to about 1.0, and restored WireGuard peers from 823k to 33–45k.
- **Per unit of work:** CPU per MB of user traffic fell from 8.4 CPU-seconds
  (11-03 evening) to 0.2–0.3, with 4.5× the user bytes (17–18 MB/s against 3.9).
  CPU per device is 0.29–0.37 mcores, the same as CPUOPT4's quiet hours: the
  remaining cost is the fixed per-process and per-device floor.
- **What is left is small.** A process now runs 0.23 cores, 44k goroutines and a
  1.9 GB heap, and collects 0.58 times a minute (the 2-minute floor). The
  CPUOPT4 proxy items that were GC-driven (GOMEMLIMIT, one origin-notification
  set per process, FindProviders2 page reuse) are worth under about 1 core each
  now (§4).

### 2. Where the CPU goes now

#### 2.1 Work and cost by window

Fleet, both hosts, 5-minute data (`<scratch>/b/ds5m.tsv`, `windows-out.txt`):

| Window (build) | Busy | Proxy cores | Devices | Transports | Refusals/s | SOCKS / HTTP adm/s | User MB/s | GC/min/proc | Alloc MB/s/proc | mcores/device | mcores/transport | CPU-s/MB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-31 18–22 (a8461e236, CPUOPT3) | 34% | 13.0 | 30.4k | 102k | 0 | 3 / 150 | 7.0 | 1.7 | 55 | 0.43 | 0.127 | 1.9 |
| 11-03 18–22 (5d4b3293a, CPUOPT4) | 75% | 32.6 | 37.5k | 135k | 1,435 | 83 / 123 | 3.9 | 8.0 | 82 | 0.87 | 0.241 | 8.4 |
| 11-04 04–10 (d5cbc2a72, pinned) | 96% | 43.5 | 50.6k | 136k | 1,923 | 118 / 113 | 2.5 | 12.0 | 138 | 0.86 | 0.321 | 17.5 |
| 11-04 18–22 (3c9f0fdc7, balance hold) | 55% | 22.8 | 51.5k | 140k | 1,267 | 435 / 140 | 5.0 | 5.2 | 62 | 0.44 | 0.163 | 4.6 |
| 11-05 00–06 (3c9f0fdc7, create flood) | 95% | 43.2 | 85.5k | 92k | 670 | 406 / 271 | 2.4 | 14.0 | 100 | 0.51 | 0.470 | 18.1 |
| 11-06 18–22 (f0b2e9ef6, idle hold) | 10% | 2.6 | 7.4k | 21.6k | 0 | 228 / 124 | 3.9 | 0.56 | 7 | 0.35 | 0.119 | 0.7 |
| 11-09 18–22 (562ead790 / 924dfc81a) | 12% | 4.0 | 13.4k | 13.0k | 0 | 249 / 235 | 15.3 | 0.58 | 8 | 0.30 | 0.308 | 0.3 |
| 11-11 18–22 (079dc1d3f) | 16% | 5.1 | 14.0k | 14.3k | 0 | 312 / 281 | 18.0 | 0.63 | 10 | 0.37 | 0.357 | 0.3 |
| 11-12 18–22 (89be8b3b8) | 14% | 4.7 | 13.7k | 11.4k | 0 | 252 / 268 | 17.4 | 0.60 | 10 | 0.34 | 0.412 | 0.3 |
| 11-13 00–04 (89be8b3b8) | 14% | 4.6 | 13.7k | 13.1k | 0 | 248 / 260 | 15.9 | 0.59 | 10 | 0.33 | 0.349 | 0.3 |

- **The 11-05 00–06Z row is a different event:** a hosted-proxy create flood took
  live devices to 93k (one network created about 2,000 proxies every 5 minutes;
  another held 123k). The owner's client limit (05:27Z for one network, 06:45Z for
  all), the trims and the proxy restarts of 06:30–09:10Z ended it; the proxy was at
  5 cores by 11Z.
- **Per transport the cost looks higher now (0.31–0.41 mcores)** only because
  transports fell 90%: a process holds about 550–700 transports, and the per-process
  floor (timers, Redis polls, GC, the window loops of idle-held devices) dominates.
  Per device and per user byte, the proxy is at or below its best CPUOPT3/4 windows.
- **Platform load from the proxy is gone with it.** FindProviders2 calls 381/s
  (11-04) → 3.3/s, score-page lookups 2.4k/s → 15/s, proxy Redis pool timeouts
  478/s → 0, `create_contract` 2.4k/s → 52/s. Contract rejections are 3.1/s
  `inactive_destination` and 0.26/s `provide_mode_unavailable`, no
  `insufficient_balance`.

#### 2.2 Runtime shape

| Per process | 11-04 08Z (pinned) | 11-12 20Z |
|---|---|---|
| Cores | 2.18 | 0.23 |
| Goroutines | 255k | 44k |
| Heap in use / next GC | 4.60 / 4.16 GB (limit-paced) | 1.91 / 2.01 GB |
| GC cycles a minute | 12.1 | 0.58 |
| Allocation | 135 MB/s | 9.4 MB/s |
| RSS | 6.39 GB | 2.58 GB |

Host memory available: crisp 91.7 GB, fireside 58.3 GB (49 and 17.8 GB on 11-04).

#### 2.3 Inside one process

One capture, crisp g5, 30 s at 99 Hz, 04:50:53Z, 0.21 cores, 1,068 samples
(`<scratch>/prof/perf_proxy_crisp_g5.txt`; host 16% busy):

| Component | Share |
|---|---|
| connect library (window clients, sequences, carriers) | 69.3% |
| `SendSequence.Run` / receive sequences | 14.0% / 7.3% |
| Platform carriers (`PlatformTransport.runH1`) | 9.7% |
| Window-client H1 dials with route hedging (TLS 15.6% in all; x509 P-384 verify 6.8%) | 7.2% |
| `ProxyDevice.Run`; gVisor; SOCKS | 5.6%; 6.7%; 1.4% |
| Control (`CreateContract` 1.4%) | 3.0% |
| Scheduler (`findRunnable` 10.3%, `sysmon` 7.3%) | 19.9% |
| GC | 0 (no cycle in the window) |
| Kernel | 22.5% |

The window loops and their dials are what is left. The dials' x509 work (6.8%,
about 0.3 cores fleet-wide) is D4(b)'s, Part C #3.

#### 2.4 Window churn that remains

Window candidates run about 40/s fleet-wide: 11.7/s admitted and 28/s
`failed_ping` (70%), with `failed_contract` at 0.03/s. A failed candidate costs a
full build (FindProviders2, mint, provide, a TLS dial and the evaluation ping), as
CPUOPT4 B §3.2 described, but at 28/s that is about 1 core or less *(inferred, at
CPUOPT4's 15–35 ms per candidate)*. The cause is the providers the proxies draw,
not the proxy: the quality probe's h3 and IP-family gate (11-11/12) narrowed the
Quality pool, and windows now draw more from Speed and Online. It is a quality
question before it is a CPU one.

### 3. What CPUOPT4's proxy items did

| CPUOPT4 item | Status | Measured |
|---|---|---|
| B #1 balance-aware refill | Shipped: connect cf0f3120, proxy 3c9f0fdc7, rolled 11-04 16:06–17:05Z (owner) | Proxy 40 → 17–25 cores within the hour, with refusals still 1.2–1.3k/s; proxy Redis pool timeouts 1,233/s → 1/s (HANDOFF-1103-2). `held_insufficient_balance` ran 549 passes/s at 11-05 00Z, then 0 once refusals stopped. Predicted 15–30 surge cores; measured about 15–23 in the hours it could act. **Met.** |
| B #2 GOMEMLIMIT 6500 → 7000 MiB | Not changed | Moot now: the heap is 1.9 GB of a 6.5 GB limit and GC is at the 2-minute floor. |
| B #3 FindProviders2 page hits without the Redis read and copy | Not done | Moot for the proxy (3.3 calls/s). The api side is still worth checking (Part C). |
| B #4 one ContractOriginNotifications per process | Not done | Goroutines are 44k per process; 18 per device × about 700 devices ≈ 12.6k. Worth about 0.1–0.2 cores now *(inferred)*. |
| B #5 D4 caches | Deferred | `x509` and JWT work scale with dials and control calls, both down 90%+. Under 0.2 proxy cores now *(inferred)*. |
| Owner levers (11-05): idle-refill hold, active-only WireGuard restore | Shipped: server c65ed59fa, connect 206a4769 / sdk 5517f035; proxy f0b2e9ef6 from 11-06 15:30Z (4812cbe46 on g1–g9 from 02:00Z, rolled back 07:46–08:19Z for the walk incident) | WireGuard peers restored 823k → 44k on 11-07 (33k now); transports per device 2.7–3.6 → 0.8–1.1. |
| Hosted-proxy 2-day hard delete | Shipped: server 91f14f258, taskworker 1dd5829c9 from 11-06 20:20Z | Removes dead rows the proxies used to restore. Its database cost is Part D's. |
| GOMAXPROCS 4 → 3 A/B | Not done | Not worth running at 0.23 cores per process. |

### 4. Opportunities

Nothing here is worth more than about 1 core at today's load, so none is ranked
in the fleet list. In order:

1. **Fewer proxy processes, as an ops choice, not a CPU fix.** 20 processes carry
   0.23 cores each. Half as many blocks would save the per-process floor (about
   0.1 core each, *inferred*) but halve the restart blast radius only if the
   blocks stay small. Not recommended for CPU alone.
2. **One ContractOriginNotifications per process (CPUOPT4 B #4).** Still the right
   design; worth 0.1–0.2 cores now. Do it with Part C's origin-notification change.
3. **The `failed_ping` candidates.** Track them with the FP2 S2 capability labels
   once the proxy's window metrics ship (D43, 4fdf026c0): if the failures are
   UDP-incapable or IPv4-only providers drawn into windows that need both, the
   FP2 protocol selectors should cut them without any proxy change.

Rejected under the owner rule, as in CPUOPT4: a longer discovery backoff, a
shorter `CreateContractTimeout`, slower pings or detection. A shorter idle hold
than the owner's 45 minutes would be a product decision, and is not needed for CPU
now.

## Part C — api, alt and taskworker

Read-only measurement of Main on 11-13 (04:40–05:00Z) with Mimir history back to
10-29. Builds: api D42 `2026.11.12-outerwerld+1097289240` (server a151cee8d) since
02:30–04:10Z, its D43 roll starting about 04:50Z; alt D42 from 02:00Z and D43
`+1097411970` (server 978dcba02) by about 04:30Z; taskworker D43 since 03:50–04:30Z. Raw outputs are in `<scratch>/c/` and `<scratch>/prof/`.

### Summary

- **api: 13.1–18.5 cores at the evening peaks of 11-07 to 11-12,** at 1.09–1.30
  cores per 1k req/s (1.27 on 11-03, 1.38 on 10-31). 11-12's 1.76 is a day of four
  api releases. It is unchanged in size because its main load, `/connect/control`,
  is unchanged.
- **The refusal-replay sawtooth is the api's largest removable cost, and it is
  still switched off.** Replays climb from 0.5–1k/s after each connect restart to
  9–14.5k/s a day or two later, at 0.48 ms of api CPU each (hourly fit, 129 clean
  hours). Over 11-06 to 11-13 they averaged 5.9k/s: **2.8 api cores on average, up
  to 7 at the top of a sawtooth**, plus the same count of Redis `EVALSHA`s and nginx
  relays. The stuck-provider signal and recycle (CPUOPT4 #10) are built and 0 on
  every block.
- **JWT handling is about 15% of an api process** (7.3% verification, 3.2% P-256
  outside it, 4.1% the lag-guest gate's decode), down from 21% because replays
  were lower at capture time (5.6k/s against 8.3k/s). The D4(a) claims cache is still deferred.
- **taskworker: 11.3–12.8 cores a day (11-05..11-12), 14.3 on 11-13 00–04Z.**
  - The egress probe is still most of it. In a probe process, TLS is 37% and x509
    chain verification 26% (P-384 ECDSA 23%), as in CPUOPT4 (39% and 28%): about
    36 ms of x509 per attempt, 2.6 fleet cores at 71 attempts/s.
  - The probe's newer legs cost little CPU: quic-go 0.5%, ML-KEM 1.1%, X25519 2.0%
    of a probe process. CPU per attempt inside a probe process is 126 ms (134 in
    CPUOPT4). All-in taskworker CPU per attempt rose from 151–157 ms (11-05..11-09)
    to 167–169 (11-10/11), 185 (11-12) and 201 (11-13): non-probe work grew.
  - **New: the §8.6 extender shadow step loops.** `StLedgerV2UsageStep`
    (extender_shadow, epoch 4) has run almost continuously since 11-12 20:30Z and
    times out at 1,800 s each attempt: 13 attempts by 04:48Z, none finished. While it runs it is 45% of its process, about 1.8 cores, nearly all
    JSON decoding of contract usage snapshots. About **1.5 taskworker cores
    averaged, producing nothing** (§2.4).
- **The payer drain loop (CPUOPT4 C #4) is gone,** without the change C #4
  proposed: migration 926's statistics (D #1, applied 11-04 about 18Z) put the
  page read back on its partial index. `ClosePayerDueContractsTask` errors went
  from 156–221 an hour to 0 from 11-05, and its wall time from about 5,000 s/h
  to under 3 s/h.
- **alt:** 0.30–0.36 cores at 50–96 req/s, down from 0.69–0.78 under the 11-04
  saturation. Nothing to change.

| # | Change | Service | Est. saving | Risk |
|---|---|---|---|---|
| 1 | Stuck-provider signal, then recycle (CPUOPT4 #10), then bake it in | api, Redis, nginx | 2.8 api cores averaged (up to 7), plus 0.3–0.6 Redis and part of nginx | medium; built |
| 2 | Stop the extender shadow's futile loop; make the usage read cheap enough to finish | taskworker, PostgreSQL | about 1.5 taskworker and 2–5 edge-2 cores now (Part D #1) | low |
| 3 | D4(b) verified-chain cache in connect's TLS client | taskworker, proxy | 2.2–2.6 taskworker, 0.3 proxy | low–medium; owner deferred D4 |
| 4 | D4(a) verified JWT claims cache | api, alt | 1.8–2.4 api | low–medium; owner deferred D4 |
| 5 | GOMAXPROCS 8 → 4 for the api, A/B on one block (CPUOPT4 C #7) | api | 0.5–1 *(inferred)* | low |
| 6 | Lag-guest claim read without decoding the claims set (CPUOPT4 C #5) | api, alt | 0.5–0.6; void with #4 | low |
| 7 | One control-plane strategy per probe process (CPUOPT4 C #8) | taskworker | about 1 | medium |

### 1. Method

- **Windows.** Evenings 18–22Z of 10-31, 11-03 and 11-06 to 11-12, from Mimir; hourly
  fits over 11-06 to 11-13 using only hours with every api process older than
  100 minutes (`<scratch>/c/api_fit.txt`). The api released 4–6 times on 11-11 and
  11-12; those evenings mix builds.
- **perf** (30 s at 99 Hz, call graphs, one process, aggregated on the host, data
  deleted; validator idle at each):
  - api g2 on edge-4, 04:42Z, 1.43 cores, 5,563 samples (`<scratch>/prof/perf_api_edge4_g2.txt`);
  - taskworker g1 on edge-3, 04:43Z, 0.28 cores (no probe pass; `perf_tw_edge3_g1.txt`);
  - taskworker g2 on edge-3, 04:43:54Z, 3.98 cores, during an extender shadow run (`perf_tw_edge3_g2.txt`);
  - taskworker g1 on edge-4, 04:52:48Z, 2.39 cores at about 18.9 probe attempts/s (`perf_tw_edge4_g1.txt`).
- **Logs:** taskworker container logs on edge-3 for the shadow step (ids and keys
  masked; `<scratch>/c/tw_shadow_*.sh`).
- **Settings now:** GOMAXPROCS api 8, alt 4, proxy 4, connect 16, mcp 8, gossip 96
  (`go_sched_gomaxprocs_threads`).
- **Not captured:** an evening api capture; alt (0.3 cores, nothing to find).

### 2. Where the CPU goes now

#### 2.1 api and alt at the evening peaks

`<scratch>/c/api_windows.txt` (5-minute means over 18–22Z):

| Evening | api cores | k req/s | control k/s | create frames k/s | replays k/s | discards k/s | cores per krps | alt cores | alt req/s | taskworker |
|---|---|---|---|---|---|---|---|---|---|---|
| 10-31 | 18.50 | 13.36 | 11.32 | 8.46 | 6.20 | 0 | 1.38 | 0.51 | 72 | 12.26 |
| 11-03 | 15.57 | 12.22 | 10.21 | 6.94 | 5.05 | 3.03 | 1.27 | 0.50 | 47 | 12.09 |
| 11-07 | 13.07 | 12.03 | 9.87 | 7.55 | 6.16 | 2.04 | 1.09 | 0.35 | 75 | 11.76 |
| 11-08 | 15.07 | 13.56 | 11.70 | 9.24 | 7.47 | 2.21 | 1.11 | 0.36 | 96 | 12.64 |
| 11-09 | 17.53 | 15.47 | 13.53 | 10.94 | 8.95 | 2.31 | 1.13 | 0.31 | 61 | 12.83 |
| 11-10 | 18.53 | 16.65 | 14.41 | 11.82 | 9.36 | 2.30 | 1.11 | 0.32 | 72 | 13.34 |
| 11-11 | 17.20 | 13.21 | 11.33 | 8.75 | 6.88 | 2.30 | 1.30 | 0.36 | 77 | 12.52 |
| 11-12 | 13.93 | 7.90 | 5.51 | 2.84 | 1.62 | 2.36 | 1.76 | 0.34 | 65 | 13.80 |

- **Replays are about 80% of create frames** whenever connect is a day old (73% on
  11-03), the same sawtooth CPUOPT3 and CPUOPT4 measured (`inactive_destination`
  79–89%, `missing_companion_origin` the rest). The 11-12 evening is low because connect
  restarted at 17:10Z.
- **Discards still run 2.0–2.4k/s.** PeerAudit senders have not all updated.
- **The fit** (`api_fit.txt`): 0.475 cores per 1k replays/s (CPUOPT4: 0.46–0.52),
  rms 0.63 cores over 129 hours. The other classes' terms are collinear with the
  intercept and are not used.

#### 2.2 Inside an api process

edge-4 g2, 1.43 cores at 1.1k req/s, about 5.6k replays/s fleet-wide at the time:

| Component | Share | CPUOPT4 (edge-4 g2, 1.85 cores) |
|---|---|---|
| JWT: verification / P-256 outside it / lag-guest decode | 7.3% / 3.2% / 4.1% (≈14.6%) | 10.5% / 4.5% / 5.9% (≈20.9%) |
| `CreateContract` (with companion-origin wait 3.7%, contract admission Redis 4.3%) | 9.6% | 14.2% |
| Per-client admission `EVALSHA` | 5.1% | 6.1% |
| Origin-notification subscription | 1.6% | 2.5% |
| FindProviders2 (its select stage 0.07%) | 1.7% | 1.3% |
| go-redis / pgx / encoding/json | 10.8% / 6.9% / 7.1% | 12.1% / 7.1% / 8.4% |
| Scheduler (`findRunnable` 12.9%, `sysmon` 5.3%) | 20.7% | 20.5% |
| GC mark / `mallocgc` | 8.1% / 8.9% | 6.9% / 9.2% |
| Password hashing (argon2, login) | 2.8% self | — |
| Network softirq on the process's threads | 8.3% | 13.8% |

#### 2.3 taskworker

Daily (`<scratch>/c/tw_daily.txt`):

| Day | Cores | Probe attempts/s | All-in ms per attempt |
|---|---|---|---|
| 11-03 | 12.41 | 74.0 | 168 |
| 11-05 | 11.69 | 75.5 | 155 |
| 11-07 | 11.60 | 76.7 | 151 |
| 11-09 | 12.04 | 76.3 | 158 |
| 11-10 | 12.84 | 76.9 | 167 |
| 11-11 | 12.39 | 73.3 | 169 |
| 11-12 | 12.83 | 69.2 | 185 |
| 11-13 (00–04Z) | 14.31 | 71.0 | 201 |

**A probe process** (edge-4 g1, 2.39 cores, about 18.9 attempts/s):

| Component | Share | CPUOPT4 (edge-3 g2, 2.74 cores, 20.5/s) |
|---|---|---|
| connect client stack | 60.5% | 66.2% |
| TLS client handshakes (uTLS 24.1%) | 36.9% | 38.7% |
| x509 `Certificate.Verify` (P-384 ECDSA 22.8%, RSA 4.6%) | 25.6% | 28.1% |
| H1 platform dials with route hedging (`parallelEvalWithRouteHedge`) | 21.3% | 29.3% |
| Stock-TLS API dials (`persistConn.addTLS`; their x509 9.4%) | 9.9% | x509 on these dials 4.6% |
| In-process control (`providerEgressControl.ConnectControl`) | 7.6% | 9.6% |
| probe package frames | 21.5% | 22.7% |
| quic-go (HTTP/3 legs) / ML-KEM / X25519 | 0.5% / 1.1% / 2.0% | — |
| GC / scheduler | 6.1% / 9.8% | 6.5% / 7.3% |

- **In-process cost per attempt** is 126 ms (2.39 cores ÷ 18.9/s), against 134 ms in
  CPUOPT4. The h3 legs (0.56 UDP legs per TCP leg now, `urnetwork_url_probe_transport_outcomes_total`)
  and the IP-family legs cost little CPU: the probe's cost is its tunnels' control-plane
  TLS, which every attempt pays once.
- **x509 per attempt:** 2,027 samples ÷ (99 Hz × 30 s) ÷ 18.9/s ≈ 36 ms (CPUOPT4: 38–40).
- **The all-in rise (151 → 201 ms per attempt) is non-probe work:** the extender
  shadow (§2.4) from 11-12 20:30Z, plus smaller new tasks. Task wall time by function
  over the 6 hours to 04:40Z (`<scratch>/c/` query in the method): the probe 7.3 s/s,
  `FlushTransferDebits` 2.6, `UpdateClientScores` 0.73, `RemoveProberShardGenerations`
  0.72, `RemoveContractsPastHorizon` 0.64, `UpdateClientLocations` 0.57,
  `CloseExpiredContracts` 0.35, `CapabilityProbe` 0.25 (off by config from 11-13
  03:21Z). The histogram records a run when it ends, so attempts cut by a timeout
  or a restart, like the extender shadow's (§2.4), are missing from it.
- **The FP2 S2 export** (`:native_cap_v1` beside `:native_v1`, in D42): no visible change
  in `UpdateClientScores`, which ran 15–20 times an hour at 135–250 s before and
  after the D42 roll. Its Redis side is Part E's.

#### 2.4 The extender shadow loop (new)

- **What runs:** `work.StLedgerV2UsageStep` with kind `extender_shadow`, for epoch 4
  (`controller.stLedgerV2RunExtenderShadow`, controller/st_ledger_v2_extender_shadow.go:101).
  It builds the §8.6 shadow report: it "reads the epoch's contracts as a root's build
  does" through `model.GetStEpochAmendedUsageAtEpoch` (model/st_extender_pool.go:239),
  on `server.MaintenanceDb`.
- **What it costs:** 45.2% of a 3.98-core process in the 04:43:54Z capture: 44.1% in
  `checkStEpochUsageRow` → `decodeContractUsageSnapshot` (model/st_provider_usage.go:134,
  model/contract_usage.go:129), of which `encoding/json` decoding is 36% and GC the rest.
  About 1.8 cores while it runs.
- **How often:** `urnetwork_taskworker_execution_inflight{task="work.StLedgerV2UsageStep"}`
  reads 1 at 30 of 34 quarter-hours from 11-12 20:30Z to 11-13 04:45Z (the gaps are
  the D42 and D43 rolls). Every attempt has ended at 1,800 s with `Timeout`: the
  pending row counted 13 by 04:49Z (Part D §2.5), the latest ending 04:48:42Z with
  "failed epoch 4 shadow rows: context canceled". None has finished.
- **Why it never ends** (Part D §2.5 read the pending row): the row was scheduled
  by d1ea65129 with the usage steps' 30-minute `MaxTime`. D42 gave shadow steps a
  2-hour bound (3939500bb; taskworker/work/st_ledger_v2_usage_work.go:42–45, applied
  by `maxTimeOf` at :50), but the sync schedules with `ScheduleTaskInTxIfAbsent`
  (:75), which leaves an existing row alone. A timed-out attempt then goes through
  the framework's error reschedule (task/task_completion.go:77–118): the same row,
  the same bound, a backoff that doubles to about an hour, and no error limit. It
  never reaches the step's Post (:121–140), so `Attempt` stays 1 and `MaxAttempts`
  (100) never applies. The row read `reschedule_error_count` 13 at 04:49Z.
  `task.RaiseRunOnceMaxTimeInTx` (task/task.go:609) exists for exactly this; only
  the deposit step uses it (taskworker/work/st_work.go:432).
- **Cost:** about 1.5 taskworker cores averaged since 11-12 20:30Z, and on edge-2 a
  30-minute REPEATABLE READ snapshot each time, which costs more than the read:
  +5 to +14 edge-2 cores by the end of each read (Part D §2.5).

### 3. What CPUOPT4's items did

| CPUOPT4 item | Status | Measured |
|---|---|---|
| C #1 / fleet #10 stuck-provider signal, then recycle | Built, off: `urnetwork_connect_stuck_provider_signal_enabled` and `…recycle_enabled` are 0 on every api, alt and connect block | Replays 0.5k → 9–14.5k/s per connect generation; 5.9k/s averaged, 2.8 api cores |
| C #2 / #8 D4(a) JWT claims cache | Deferred; session/by_jwt.go changed only for the client-limit claim (359c02259) | JWT ≈14.6% of an api process now |
| C #3 / #8 D4(b) chain cache | Deferred; connect net_tls_hello.go and net_tls.go unchanged since 11-04 | x509 25.6% of a probe process, about 2.6 taskworker cores |
| C #4 payer drain page read | Not done as proposed; fixed by D #1 (migration 926) | Errors 156–221/h → 0 from 11-05; wall 5,000 s/h → under 3 s/h |
| C #5 lag-guest decode | Not done (router/lag_guest_credential.go unchanged) | 4.1% of an api process |
| C #6 origin notifications | Not done (model/contract_origin_notification.go unchanged) | 1.6% of an api process |
| C #7 GOGC 400, GOMAXPROCS A/B | Not done | GC 8.1%, scheduler 20.7% |
| C #8 one control-plane strategy per probe process | Not done (qualityprobe/providertunnel/tunnel.go changed for S1 step 2 only) | Stock-TLS API dials 9.9% of a probe process |
| C #9 small items | Not done | `updateFunctionName` regexp 0.05–1.8% |

### 4. Ranked opportunities

#### #1 Turn on the stuck-provider signal, then the recycle, then bake them in

- **Change:** CPUOPT4 C #1's three steps, unchanged: the signal on one api block, then
  all, for 24 hours with the recycle off (measurement only); then the recycle on the
  beta connect block, g1, all. Once both have run on every block for a week, make them
  the code defaults and remove the keys (the owner's 10-15 rule against hidden
  settings).
- **Code:** controller/connect_control_stuck_provider.go:190–206 at a151cee8d (20 min,
  6 answers, 5 min gap, 1 h cooldown, 5/s with a burst of 20; the runtime default
  off), api/run.go (the code ceiling on), model/connect_move_control.go (the
  `stuckProviderSignal` and `stuckProviderRecycle` fields), connect/client_recycle.go.
- **Evidence:** §2.1; 0.475 cores per 1k replays/s; 5.9k/s averaged over 11-06..11-13,
  9.4k/s at the 11-10 evening, 14.5k/s at the top on 11-06 15Z.
- **Saving:** if most of the climb above a fresh generation's 0.5–1k/s is stuck
  providers (step 1 measures it), **about 2.5 api cores averaged and up to 6.5 at the
  top of a sawtooth**, plus the same number of Redis `EVALSHA`s (0.3–0.6 Redis cores
  at Part E's per-request cost) and their nginx relays.
- **Risk:** medium; a recycled provider reconnects, with the drain excuse written
  first. The validator is not involved, but enable outside its busy minutes.
- **Why no timing bound moves:** refusal answers, the refusal ring and every deadline
  stay; the thresholds are the code defaults.
- **Validation:** CPUOPT4 C #1's list: `…recycles_total{outcome}`, the replay slope per
  connect hour on enabled blocks, api cores per krps, provider reliability and excused
  drains.

#### #2 Stop the extender shadow's futile loop (with Part D #1)

- **Change, in order** (Part D #1 has the database side):
  1. **End the loop:** raise the pending row to the bound the code declares, with
     `task.RaiseRunOnceMaxTimeInTx` beside the sync's `ScheduleTaskInTxIfAbsent`
     (taskworker/work/st_ledger_v2_usage_work.go:75), as the deposit step does; or
     the owner retires epoch 4's shadow report. Check the other `IfAbsent` chains
     whose `MaxTime` changed for the same stale-row defect.
  2. **Count a timed-out attempt** toward the chain's `MaxAttempts`, so a chain that
     cannot finish stops with a metric and a log line instead of re-reading the
     epoch at every backoff.
  3. **Make the read cheaper on both sides:** read the window in short slices
     (Part D #1(c)), and decode each contract usage snapshot once per epoch with a
     typed decoder instead of `encoding/json` reflection
     (model/contract_usage.go:129); the live root build reads the same statement
     from epoch 8 (controller/st_ledger_v2_rows.go:50), so it gains too.
- **Evidence:** §2.4; Part D §2.5.
- **Saving:** about 1.5 taskworker cores now, and 2–5 edge-2 cores averaged (Part
  D). A shadow that finishes costs one read per epoch.
- **Risk:** low for 1 and 2; low–medium for 3, which touches the settlement
  statement's read path: the decoded rows must stay byte-identical inputs to
  `ledger.Verify`, checked by the offline replay.
- **Why no timing bound moves:** the shadow commits nothing; the 2-hour bound in 1
  is the one the owner already merged.
- **Validation:** `urnetwork_taskworker_execution_errors_total{task="work.StLedgerV2UsageStep"}`
  at 0; the pending row's `reschedule_error_count` stops rising; one
  `st_ledger_v2_extender_shadow` row per epoch if the report is kept; taskworker
  CPU per probe attempt back near 155 ms.

#### #3 D4(b) verified-chain cache (deferred by the owner)

- Unchanged design (CPUOPT3 D4(b)): cache a verified chain by its bytes, server name
  and root, for pinned-pool configs only; key possession, hostname and validity are
  checked on every handshake.
- **Evidence:** x509 25.6% of a probe process, 36 ms per attempt; the platform chain's
  P-384 ECDSA verify is 22.8% (Go's P-384 has no assembly path).
- **Saving:** 2.2–2.6 taskworker cores at 70–75 attempts/s, 0.3 proxy, and every app
  and provider's dials.
- **Risk, bounds and validation:** as CPUOPT4 C #3. The probe's target TLS (the exit's)
  is ineligible and unchanged.

#### #4 D4(a) verified JWT claims cache (deferred by the owner)

- **Evidence:** JWT ≈14.6% of an api process at 5.6k replays/s; more when replays are
  high, since every replay verifies.
- **Saving:** 85–90% of it, **1.8–2.4 api cores** at the evening peaks of 11-07..11-11;
  it also makes #6 unnecessary. #1 shrinks it by removing replays.
- **Risk, bounds and validation:** as CPUOPT4 C #2.

#### #5 GOMAXPROCS 8 → 4 for the api, on one block

The scheduler is 20.7% of an api process (`findRunnable` 12.9%, `sysmon` 5.3%) at
0.7–1.5 cores per process. **0.5–1 core** *(inferred)*. A runtime knob; revert on any
rise in control-route phase latencies or in-flight gauges.

#### #6 Lag-guest claim without the whole claims decode

As CPUOPT4 C #5: 4.1% of an api process, **0.5–0.6 cores**; void if #4 ships.

#### #7 One control-plane strategy per probe process

As CPUOPT4 C #8. The stock-TLS API dials are 9.9% of a probe process now, and
their x509 9.4% (4.6% in CPUOPT4): each tunnel's control-plane client dials and
verifies on its own. About **1 core**.
Needs the owner's probe-design review: it changes which dials resume.

### 5. Considered and rejected

- **Lowering argon2's cost parameters** (model/auth_model_identity.go:84, 2.8% of one
  api process during logins): a security bound.
- **Shortening the extender shadow's work by sampling contracts:** the report must
  map one to one onto `ledger.Verify`'s input.
- **Raising the payer drain's or the shadow's bounds to make slow reads finish:** the
  shadow's own 2-hour bound is already in the code; #2 asks for it to be honored, not
  raised.
- CPUOPT4 C §6's list stands: no replay answers before verification, no cached
  revocation, no shorter companion-origin waits, no probe-timing changes.

### 6. Noticed, not CPU

- **FindProviders2 latency at the api rose at 19:10–19:40Z on 11-12** and has stayed:
  the route's mean went from about 40 ms to 105–140 ms because its `select` stage
  went from 0.33 ms to about 40 ms on average. 99.9% of selects still take 5 ms or
  less; about 0.04/s run past 10 s, and `selection_outcomes{outcome="canceled"}` rose
  from 0.002/s to 0.04/s at the same time. The api spends 0.07% of its CPU in
  selection, so this is a wait, not CPU. No api deploy happened at 19Z; the cause is
  not determined.

## Part D — PostgreSQL on edge-2

This is research, not a change. Every number comes from read-only measurement
of Main's primary, by-us-fmt-5-edge-2, on 2026-11-13 between 04:18Z and 05:10Z,
plus Mimir history from 10-30. Nothing in production was changed. No
identifiers, addresses or literals are printed here. Code references are at the
deployed revisions: server a151cee8d (D42: api, alt), 978dcba02 (D43:
taskworker, alt from 04:00Z), a00dfeb9a (connect) and 89be8b3b8 (proxy), and
xops origin/main c1836e5. *(inferred)* marks a claim reasoned from code or
arithmetic rather than measured.

### Summary

**edge-2 runs about 20 fewer cores than at CPUOPT4, mostly because it has half
the work and no flipped plan.** Its 3-day mean fell from 50.2 to 29.5 busy
cores (11-01..11-04 against 11-10..11-13, summing the non-idle modes; 53.6 → 31.6
on the summary table's "1 − idle" basis). At the evening peak
(18–22Z) it ran 29.7–32.8 cores on 11-09..11-12, against 42.9–52.8 on
10-31..11-04:

- **Less work.** Evening commits fell from 38.6–55.0k/s to 24.5–25.5k/s. Pool
  acquires fell from 37–54k/s to 21–23k/s: the proxy from 6.9–19.1k/s to
  0.6–0.9k/s, connect from 14.6–18.2k to 8.9–10.0k, the api from 10.9–12.6k
  to 7.8–9.4k. The taskworker is about flat at 3.4–4.2k/s.
- **No flipped plan.** Migration 926 (CPUOPT4 #1, expression statistics) was
  applied 11-04 17:47:05Z. `GetOpenTransferByteCount` now runs 21.2 calls/s at
  0.15 ms (353 ms when flipped). `GET /subscription/balance` has stayed at 8–24
  ms in every hour since 11-04 19Z, except the 11-06 brownout, while the open
  set drained from 13.45M to 115k entries, the regime that flipped it four
  times before. The payer drain has failed no run since 11-05 00Z (3,681
  failures on 11-04).
- **Cheaper per unit.** At the same commits, hourly busy sits 0.8–4.3 cores
  below CPUOPT4's line (11-07..11-12). PgBouncer costs 56 µs per query (62–74)
  and nginx 42 µs (45–55). Cores per 1k commits/s at the evening peak rose to
  1.20–1.28 (0.96–1.12) only because a floor of about 12–16 cores is spread
  over half the commits; the marginal cost fell from 0.78 to 0.54–0.57.

**Where the CPU goes now** (20 s `/proc` census at 04:31:43Z; 38.3 cores busy,
31.6 without iowait): backends 19.2 cores (13.5 through PgBouncer, 5.8 direct,
including one long SN25 read), PgBouncer 2.9, nginx 2.2, IO workers 2.1,
dm-crypt 1.35, autovacuum 1.0. The two hops are 5.1 cores, 16% of busy without
iowait (CPUOPT4: 12.2 cores, 22%). In pg_stat_statements (pgss), 70.1k
statements/s use 7.56 backends of execution (CPUOPT4: 142.8k/s, 23.9). No
statement is above 8.2% of execution time; the payer open-sum, 36% in CPUOPT4,
is 0.05%.

**Long snapshots are still the largest swing on the primary, but they now come
from application reads, not the dump.** No logical dump has run against
edge-2 since 11-04 02:05Z, and the base-backup copies to edge-6 cost about 1
core while they stream. Instead:

- **Sunday 11-08 10:01–13:59Z:** the weekly payout plan held one transaction for
  about 4 h (its `CREATE TEMPORARY TABLE temp_account_payment` runs 519 s with
  88M block reads). edge-2 rose 45.7 cores, about 182 core-hours. 11-01 cost
  94–118. D42 made the payout run a no-op (7475aa85f), so the 11-15 10:00Z run
  should cost nothing.
- **Since 11-12 20:30Z, and still running:** the SN25 extender shadow step
  (d1ea65129, §8.6.7) reads epoch 4's 7 days of contracts in one REPEATABLE
  READ snapshot, is cut at 30 minutes, and is claimed again. 12 reads to
  03:40Z lifted edge-2 by 5–10 cores each (27–49 core-hours); a 13th, at
  04:18–04:48Z under D43, by 14. Nothing was recorded. The pending row still
  has a 1800 s bound after 13 timeouts with its attempt at 1, so D42's 2-hour
  bound never reached it and the loop has no end (§2.5).
- **The pin tax**, measured in this session: 20 minutes into that snapshot, four
  hot statements cost 1.74 backends, against 0.22 in a pin-free window after
  it, and transfer_debit_journal held 2.14M dead rows for 1,353 live.

**CPUOPT4 scorecard:** #1 shipped and holding; #3 shipped as the base-backup
design; C #4 not shipped but no longer needed. #2, #4, #5, #6, #7, #8, #9 and
#10 are not done (§3).

**New costs** (§4): the shadow-step loop above, about 2–5 cores on average for
as long as it runs *(inferred: 3–7 core-hours per read, one read every 1–2 h)*;
FP2 S2's health-window aggregate (2.37 s and 892k buffer hits per call, 21% of
it JIT); the SN25 amounts-for-network read (1.17 s per call, 0.013 ms on 11-04);
the descendant check on every client deactivation (0.47 backends). S1's history
writes cost 0.011 backends; the Solana sweep, the uncredited-payment metric and
the store notification flow under 0.001 together.

**Ranked opportunities** (cores at the evening peak, which is now close to the
daily mean; they overlap little):

| # | Change | Est. saving | Risk |
|---|---|---|---|
| 1 | End the SN25 shadow loop and keep epoch reads from pinning the primary: give the pending row the bound its code declares (or retire epoch 4's shadow), count timeouts as attempts, then slice the read or use the edge-6 copy | 2–5 cores on average from now *(inferred)*; +5 to +14 cores on average while a read runs, measured | low (a), medium (slicing) |
| 2 | Remove the nginx stream hop (CPUOPT4 #2) | 2.0–2.5 | medium |
| 3 | Reboot into `retbleed=stuff iommu=pt` (CPUOPT4 #4) | 1–2.5 *(unmeasured)* | medium (restart) |
| 4 | Rebuild transfer_contract's bloated indexes, the five drained partials first (CPUOPT4 #8, the disk runway's O2) | 1–1.5 in the vacuum IO path *(inferred)* | medium (horizon per build) |
| 5 | PgBouncer to PostgreSQL over a Unix socket (CPUOPT4 #5) | 0.7–1.5 *(unmeasured)* | low |
| 6 | Fewer round trips (CPUOPT4 #6) | 0.5–1 | low–medium |
| 7 | An index for the SN25 amounts read | 0.2 | low |
| 8 | A narrower descendant check on deactivation | 0.3–0.4 | medium |
| 9 | JIT off for OLTP (CPUOPT4 #9) | 0.15–0.2 | low |
| 10 | GetTransferStats and the health-window aggregate (CPUOPT4 #10) | 0.2–0.4 | low–medium |

Together roughly 6–10 cores, 20–35% of edge-2's 29.5-core mean; #2, #3 and #5
overlap in kernel time. #1's retirement option is an SN25 decision; #2, #3 and
#4 need owner windows on the database path (#4's is planned as O2).

### 1. Method

**When and load.** Friday 11-13, 04:18–05:10Z. Since the proxy loop ended,
edge-2's load is nearly flat over the day: 23.4k commits/s in the pgss window
against 24.5–25.8k/s at 18–22Z on 11-09..11-12, so this window stands for the
evening load better than CPUOPT4's morning did. In the main pgss window
(04:19:33–04:38:07Z, 1,115 s): 70.1k statements/s and 23.4k commits/s;
PgBouncer 52.4k queries/s in 20.6k transactions/s; WAL 58.6 MB/s with 15.5k
FPI/s.

**Builds in the window** (`urnetwork_build_info`): api 20 on D42
(1097289240, a151cee8d); alt 2 on D42 and 2 on D43; taskworker 8 on D43
(1097411970, 978dcba02) while the roll continued; connect 20 and gossip 4 on
1096979900 (a00dfeb9a); proxy 20 on 1096461840 (89be8b3b8). alt was all on D43
by 04:30Z; the api began its D43 roll between 04:50 and 05:00Z. Migrations 967–969
(server main 1ed3b4b44, not in D43) were applied at 04:07Z.

**Host and database.** As CPUOPT4: 96 logical CPUs, kernel 6.8.0-147 booted
10-24 00:47Z, PostgreSQL 18.6, postmaster since 10-24 11:21Z. pgss is
extension 1.10, max 5,000, 4,751–4,784 entries, 57 deallocations since its
10-23 reset and none in my windows; `track_planning` off, `track_utility` on.

**Captures** (`<scratch>/d/`):
- **pgss:** snapshots at 04:19:33, 04:22:32, 04:28:58, 04:38:07, 04:54:56 and
  05:10:14Z, no text, never reset, each in one statement with pg_stat_database,
  _wal, _io, _user_tables, _user_indexes and transfer_contract's index pages.
  The ranking uses 1→4, which overlaps the epoch read of §2.5; 1→3 and 3→4
  show its drift, and 5→6 (918 s, after the read was cancelled) is pin-free. Texts were fetched once for the 1,286
  active queryids, with strings, UUIDs and long numbers stripped on the server.
- **Catalog and settings:** the statistics object, migration_audit, reloptions,
  replication slots, sizes, and long transactions in pg_stat_activity by
  query_id (one text, stripped).
- **Host** (ansible, read-only): facts, a 20 s `/proc` census at 04:31:43Z, and
  PgBouncer `SHOW STATS`/`SHOW POOLS` on all 32 admin sockets, 23.8 s apart at
  04:32Z.
- **Mimir:** edge-2 CPU, pg_stat_database, activity by application and user,
  PgBouncer, pool acquires by service, the balance route, the reaper, taskworker
  executions, 10-30 to 11-13 05Z.

Every query went through `support db query`: one statement at a time, READ
ONLY, 30 s timeout, 0.8–5.9 s each including the tool's start. Two reads of
pending_task by function name (`shadowcheck-1.json`, `shadowtask-1.json`) took
4.2 and 5.9 s, probably scanning its 27.8 GiB heap; they were not repeated.
Nothing else touched a large table. Nothing was refused.

**Not captured:** EXPLAIN (the tool accepts only SELECT and WITH), so plans are
inferred from pgss means and index counters; planning time (`track_planning`
off); statements per service (services set no `application_name`; acquires
stand in); an evening pgss window; perf (not allowed in this part). The epoch
read (§2.5) never completes, so pgss has no row for it.

### 2. Where the CPU goes now

#### 2.1 Host, evenings, per commit

Means of 5-minute rates (`<scratch>/d/raw/hist-evening.txt`). "Residual" is busy
minus CPUOPT4's fit (12.8 + 0.78 × commits in k/s).

| Window | Busy | Without iowait | Commits | Cores per 1k commits/s | Residual | Proxy acquires/s |
|---|---|---|---|---|---|---|
| 10-31 18–22Z (CPUOPT3) | 43.3 | 40.7 | 38.6k | 1.12 | +0.4 | 6.9k |
| 11-03 18–22Z (CPUOPT4) | 45.1 | 42.4 | 45.9k | 0.98 | −3.6 | 15.8k |
| 11-06 18–22Z | 32.2 | 28.5 | 22.2k | 1.45 | +2.1 | 0.6k |
| 11-07 18–22Z | 26.9 | 23.8 | 20.7k | 1.30 | −2.1 | 0.4k |
| 11-08 18–22Z | 28.6 | 25.6 | 23.4k | 1.22 | −2.5 | 0.5k |
| 11-09 18–22Z | 30.5 | 26.8 | 24.5k | 1.25 | −1.4 | 0.6k |
| 11-10 18–22Z | 32.8 | 28.0 | 25.5k | 1.28 | +0.1 | 0.6k |
| 11-11 18–22Z | 29.9 | 26.7 | 24.5k | 1.22 | −2.0 | 0.7k |
| 11-12 18–22Z | 29.7 | 27.6 | 24.7k | 1.20 | −2.4 | 0.9k |

- **11-06's evening** includes the 18–20Z episode of §2.5; the others are clean.
- **Builds at 20Z** (`urnetwork_build_info`): 11-09 connect, proxy and
  taskworker 1093617580, api 1094529300; 11-10 connect and taskworker
  1094619150, proxy 1094558780, api 1095399110; 11-11 connect and proxy
  1095717130, taskworker 1096116030, api 1096241800; 11-12 connect, api and
  taskworker 1096979900, proxy 1096461840. 11-12's evening is
  before D42, and the shadow loop began at 20:35Z inside it.
- **Peaks:** the 5-minute maximum in those evenings was 37.9–41.7 cores,
  against 52.8–69.1 on 11-01..11-04.
- **The fit now.** Over 136 clean hours (11-07 00Z to 11-12 20Z, without the
  11-08 pin), busy = 16.4 + 0.54 × commits (k/s), R² 0.23, residual sd 2.1
  cores; without iowait, 12.6 + 0.57 × commits, R² 0.51, sd 1.2
  (`regress-busy-vs-commits.txt`). Commits only ranged over 18–27k/s, so the
  slope is loose. The daily mean residual against CPUOPT4's line was −1.5,
  −1.4, −0.8, −0.9 and −4.3 cores on 11-07, 11-09, 11-10, 11-11 and 11-12.
- **What a commit costs:** marginally less than at CPUOPT4. The cost per commit
  rose only because the floor stayed while the commits halved.

**Pool acquires by service** (per second, the hour from the time shown;
`acquires-by-service.txt`):

| Service | 10-31 19Z | 11-03 19Z | 11-04 10Z | 11-09 19Z | 11-12 19Z | 11-13 03Z |
|---|---|---|---|---|---|---|
| connect | 14,601 | 16,933 | 18,231 | 9,458 | 10,044 | 8,911 |
| api | 11,172 | 10,896 | 12,643 | 9,437 | 8,038 | 7,832 |
| taskworker | 4,209 | 3,612 | 3,792 | 3,727 | 3,640 | 3,408 |
| proxy | 6,943 | 15,788 | 19,140 | 585 | 864 | 939 |
| total | 36,978 | 47,271 | 53,863 | 23,262 | 22,648 | 21,142 |

The taskworker's figures include 440/s on its maintenance pool, direct to port
5432. connect is again the largest driver. The proxy no longer drives edge-2.

#### 2.2 By process

`/proc`, 20 s at 04:31:43Z (`census-1.out`), against CPUOPT4's 10:21:47Z census:

| Consumer | Cores now (user/sys) | Processes | CPUOPT4 |
|---|---|---|---|
| backends via PgBouncer (loopback) | 13.49 (11.71/1.78) | 350 | 32.84 |
| backends direct (LAN, 5432) | 5.75 (5.27/0.49) | 120 | 2.00 |
| PgBouncer | 2.93 (0.85/2.07) | 32 | 7.09 |
| nginx stream (6432 → 6433–6464) | 2.20 (0.46/1.74) | 33 | 5.15 |
| IO workers | 2.13 | 24 | 1.09 |
| dm-crypt kworkers | 1.35 | 23 | 1.12 |
| autovacuum workers | 1.03 | 3 | 0.72 |
| checkpointer, walwriter, walsummarizer | 0.31 | 3 | 0.30 |
| **host busy without iowait** | **31.56** (iowait 6.73, softirq 2.21) | | 55.1 |

- **Direct backends grew** from 2.0 to 5.75 cores. They include the
  taskworker's maintenance pool (400 connections), which carried the SN25 epoch
  read (§2.5) during the census.
- **Per unit** (PgBouncer at 04:32Z: 52.4k queries/s, 20.6k transactions/s,
  2.54 queries per transaction):
  - PgBouncer: 56 µs per query and 142 µs per transaction (CPUOPT4: 62–74 and
    138–178; CPUOPT3: 54 and 151);
  - nginx: 42 µs per query (CPUOPT4: 45–55; CPUOPT3: 39);
  - bytes per query: 178 in and 237 out (CPUOPT4: 238–314 out);
  - backends through PgBouncer: 257 µs per PgBouncer query (CPUOPT4: 289–369,
    about 7 cores of it the flip; CPUOPT3: about 220).
- **Backends' CPU exceeds their active time:** 19.2 cores against 11.1–13.2
  s/s of `active_time`. The rest is protocol and transaction-control work
  between statements, which `active_time` does not count *(inferred)*. That is
  where round trips cost (§5 #6).
- **PgBouncer is healthy:** no waiting clients, `cl_active` 1,108, `sv_active`
  35, `sv_idle` 213, 3.6 logins/s, re-prepares at 0.1% of binds.

#### 2.3 Inside PostgreSQL: pgss

Window 1→4 (`pgss-rank-1-4.txt`): 70.1k statements/s, 7.56 backends of
execution, 3.00 statements per commit and 0.324 ms of execution per commit.
CPUOPT4: 142.8k/s, 23.9 backends, 2.56 and 0.43 ms. Mean execution per
statement fell from 167 µs to 108 µs. The pin-free window 5→6
(`pgss-rank-5-6.txt`) is the steady state: 72.3k statements/s, 6.71 backends,
2.96 statements and 0.275 ms per commit (93 µs per statement). It has the same
leading statements, except that rows 1, 6 and 7 below shrink to 0.17 backends
together, and the reaper's cascade delete (model/contract_horizon_reaper.go:1060)
leads at 354 ms per batch, 0.52 backends (24 ms per batch in 1→4).

| # | Statement | Code | % exec | calls/s | mean ms | hits/call | CPUOPT4 (11-04 10Z) |
|---|---|---|---|---|---|---|---|
| 1 | URL-probe slot claim `WITH slot_candidates …` | model/provider_url_probe_due.go:348 | 8.21 | 66.4 | 9.35 | 6,468 | 68.0/s, 3.98 ms, 1,027 hits |
| 2 | active descendants on deactivation `… CROSS JOIN LATERAL (… network_client.device_id = parent.device_id …)` | model/network_client_model.go:1732, from :1546 (`deactivateLockedNetworkClientsInTx`, :1525) | 6.19 | 186.7 | 2.51 | 2,021 | not present (rewritten after 11-06) |
| 3 | close-report upsert `WITH prior AS MATERIALIZED` | model/subscription_model.go:3290 | 4.48 | 1,640 | 0.21 | 50 | 2,488/s, 0.22 ms |
| 4 | close `UPDATE transfer_contract SET outcome = …` | model/subscription_model.go:3811 | 3.83 | 670 | 0.43 | 52 | 1,009/s, 0.78 ms |
| 5 | payment-driven contract retention | model/subscription_model.go:6278 | 3.65 | 1 call | 14,638 | 5.08M | 31,842 ms |
| 6 | debit-journal oldest age (gauge) | model/subscription_transfer_debit.go:45, :115 | 3.41 | 8.9 | 29.0 | 19,908 | 8.2/s, 3.5 ms, 1,818 hits |
| 7 | debit-journal shard walk | model/subscription_transfer_debit.go:139 | 3.17 | 172.7 | 1.39 | 775 | 149/s, 0.10 ms, 28 hits |
| 8 | egress health-window aggregate | model/provider_egress_health_model.go:806 | 2.81 | 0.1 | 2,370 (504 JIT) | 892k | 0.3/s, 1,050 ms, 350k hits |
| 9 | SN25 amounts for a network | model/st_ledger_v2_reader_model.go:86 (from controller/st_ledger_v2_readers.go:100) | 2.55 | 0.2 | 1,167 | 14,979 | 0.1/s, 0.013 ms |
| 10 | GetTransferStats unpaid | model/account_payment_model.go:1148 | 2.24 | 0.4 | 396 | 1.63M | 0.6/s, 498 ms, 857k hits |
| 11 | contract insert `WITH creation_clock …` (two forms) | model/subscription_model.go:1992 and :2980, subscription_redis_admission.go:560 | 3.20 | 674 | 0.36 | 53 | form A: 834/s, 0.87 ms |
| 12 | close-report evidence insert | model/contract_close_report.go:392 | 1.66 | 673 | 0.19 | 17 | 992/s, 0.24 ms |
| 13 | pair network lookup | model/network_client_model.go:2841 | 1.48 | 1,779 | 0.06 | 17 | 5,297/s, 0.08 ms |
| — | payer open-sum `GetOpenTransferByteCount` | model/subscription_model.go:7004 | 0.05 | 21.2 | 0.15 | 49 | 24.5/s, 353 ms, 11,704 hits |

- **By calls:** `SET LOCAL lock_timeout` 5.1k/s (riding on the BEGIN),
  `provide_key_change` COUNT 5.0k/s, `commit` 4.9k/s, `begin … read committed`
  4.2k/s, the pair lookup 1.8k/s. Transaction control and locks are 21.7k/s,
  31% of statements.
- **Rates fell with the work, and the steady statements got cheaper per call.**
  The close UPDATE went from 0.78 to 0.43 ms and the escrow settle from 0.55
  to 0.41 ms, now that nothing evicts their pages (client-backend reads: 61
  MB/s against 1,400 MB/s).
- **Rows 1, 6 and 7 are inflated by the epoch read's snapshot** (§2.5); row 5
  is one call.

**By family** (`families-1-4.txt`; the health aggregate counted with the probe):

| Family | % exec | Backends | % calls |
|---|---|---|---|
| network_client, provide_key, device, connections | 23.5 | 1.77 | 21.1 |
| URL and egress probe (slots, cycles, health, security) | 17.2 | 1.30 | 7.9 |
| transfer_contract (create, close, pair, lookups) | 14.7 | 1.11 | 12.1 |
| debit journal (FlushTransferDebits) | 9.1 | 0.69 | 2.0 |
| transfer_escrow and sweeps | 7.7 | 0.58 | 6.6 |
| contract_close* | 6.8 | 0.52 | 7.7 |
| reaper, retention and payment retention | 5.7 | 0.43 | 0.0 |
| SN25 ledger, census, extender, wallet resolution | 4.0 | 0.30 | 3.1 |
| transaction control and locks | 1.7 | 0.13 | 30.9 |
| payer open-sum | 0.05 | 0.00 | 0.03 |

**IO** (pg_stat_io, window 1→4): autovacuum read 544 MB/s in vacuum context
(CPUOPT4's window: 139 MB/s), client backends 61 MB/s, the checkpointer wrote
56 MB/s, and the WAL summarizer reads 47 MB/s of WAL (new since 11-05).
transfer_contract's vacuum was in its index phase (4 of 16 indexes done, 85
minutes in). The IO workers' 2.1 cores and dm-crypt's 1.35 cores follow these
bytes *(inferred)*.

#### 2.4 Statistics, settings and catalog now

- **The statistics object** `transfer_contract_unresolved_predicate` exists (kind
  `e`), and its data was collected: the expression's MCV is {f, t} at
  {0.99994, 0.0000556}, about 115k of 2.07B rows. The last autoanalyze ran at
  02:55Z, with 172 since the reset, plus 2 manual analyzes (the last at 11-12
  07:24Z).
- **transfer_contract:** 2.07B live rows (2.93B on 11-04) in a 929 GiB heap with
  1,637 GiB of indexes. The five open-set partials hold 216 GiB for 114,987
  entries (§3, #8). Vacuums run back to back, every 2.6 h on average. The
  table's freeze age was 196M and the database's 198M, so the anti-wraparound
  sawtooth continues around `autovacuum_freeze_max_age`.
- **Queue tables:** pending_task is a 27.8 GiB heap for 115k live rows and 581k
  dead; transfer_debit_journal is 7.0 GiB for 1,353 live rows and 2.14M dead
  (both during the epoch read's snapshot). Each is autovacuumed and
  autoanalyzed every 20.9 s (27 times in 565 s; CPUOPT4: every 6.0 s).
- **Replication:** no slots, no walsender, `archive_mode` off,
  `hot_standby_feedback` off. Nothing retains WAL or holds a horizon for
  edge-6 now.
- **Disk:** 9.80 TB of database (9.48 on 11-04), 4.7 TB free on root.

#### 2.5 Episodes since CPUOPT4: long snapshots and one regression

Excess core-hours on the new fit and against the hours before
(`episodes-cost.txt`, `hot-hours.txt`). Long transactions are from
`pg_stat_activity_max_tx_duration` by user, which separates the application
from autovacuum.

| Episode | Length | Busy mean | Excess core-h | What held it |
|---|---|---|---|---|
| 11-01 (Sun) 10–14Z, before CPUOPT4 | 4.0 h | 71.6 | 94–118 | the weekly payout plan |
| 11-06 02–09Z | 6.8 h | 65.6 | 249–253 | the proxy's descendant walk (proxy release 1091174670), many concurrent heavy queries, not a snapshot |
| 11-06 18–20Z | 2.0 h | 37.4 | 16–18 | one application transaction of about 1,900 s |
| 11-08 (Sun) 10:01–13:59Z | 4.0 h | 73.6 (5-min max about 83) | 182–183 | the weekly payout plan, one transaction over 13,425 s |
| 11-12 20:35Z to 11-13 03:40Z, and 04:18–04:48Z | 7.1 h + 0.5 h | 32.5; 39.7 in the 13th read | 27–36 on the fit, 49 over the 25.5-core trough between reads; about 7 more for the 13th | the SN25 extender shadow read, 30-minute cycles |
| 11-10 21:15Z to 11-11 15Z, base-backup copy | 17.8 h | 30.7 | 24–29 (13 on the same hours a day earlier) | none; pg_basebackup at about 159 MB/s |

- **The weekly payout plan.** The Sunday 10:00Z run's
  `CREATE TEMPORARY TABLE temp_account_payment` (model/account_payment_model_plan.go:668)
  has run 5 times since the reset, 519 s each, with 57.5M hits and 88M block
  reads per call. On 11-08, buffer hits rose to 39.8M/s and tuples returned to
  65.6M/s, as during the 11-03 dump. The handoff called its 2–3 h snapshot
  routine. D42's 7475aa85f makes `work.Payout` a no-op, so the queued 11-15
  10:00Z row plans nothing. That avoids about 100–180 core-hours a week, or
  0.6–1.1 cores on the weekly mean.
- **The SN25 shadow loop.** The application's longest transaction rose 300 s
  every 5 minutes from 20:30Z, peaked at 1,729–1,795 s, dropped, and climbed
  again: 12 cycles until 03:40Z. The gaps grew from 1–5 minutes to 20–30
  minutes. `work.StLedgerV2UsageStep` was in flight at every sample from 21:20Z,
  and `urnetwork_st_extender_pool_epoch` has no `shadow` phase. During the
  loop, buffer hits ran 7.8M/s against 3.7M/s, and tuples returned 9.2M/s
  against 4.3M/s.
- **The 13th cycle, in this session.** One direct backend ran query_id
  4027406781210946646, which is `stEpochAmendedUsageSql`
  (model/st_extender_pool.go:190, called at :325-327 through
  `server.MaintenanceDb`, `WithoutTransactionTimeout` and a REPEATABLE READ,
  read-only transaction). It started at 04:18:43Z and was on CPU with no wait
  event at 470, 1,310, 1,421 and 1,502 s, its xmin 2.2M → 6.8M transactions
  old. By 04:48:57Z it was gone: no pgss row (it never completed),
  st_ledger_v2_extender_shadow empty.
- **What one read costs edge-2** (`shadow-cycles.txt`, `cycle13-5m.txt`). Busy
  climbs from about 26 cores to 35–44 by the end of each read and falls back
  within 5 minutes of the cancel. Over a 30-minute read that is +5 to +10 cores
  overnight and +14 this morning, when iowait rose from 2.7 to 9 cores. The
  read's own backend is about 1 core; most of the rest is the pin tax below.
  Its own counters land at the cancel: tuples fetched jump by 3.6M/s for that
  5-minute step, about 1.1B tuples per read *(inferred)*. pgss cannot show the
  statement's rows or blocks per call, because a cancelled statement is never
  recorded.
- **Why it does not stop** (the pending row at 04:49Z, `shadowtask-1.json`):
  `kind` extender_shadow, `epoch` 4, `attempt` 1, `run_max_time_seconds` 1800,
  `reschedule_error` Timeout, `reschedule_error_count` 13, next `run_at`
  05:20:50Z.
  - The row was scheduled by d1ea65129 with the 30-minute `MaxTime`. D42's
    `ShadowMaxTime` of 2 h (3939500bb; taskworker/work/st_ledger_v2_usage_work.go:42-45
    at 978dcba02) applies only to rows the sync or the Post schedules, and the
    sync's `ScheduleTaskInTxIfAbsent` (:75) leaves an existing row alone.
  - A timed-out attempt goes through the task framework's error reschedule
    (task/task_completion.go:77-118). It keeps the same row with its bound and
    arguments, backs off `RescheduleTimeout × 2^n` capped at about an hour
    (task/task.go:200-213), and has no error limit. It never reaches the step's
    Post, so `Attempt` stays 1 and `MaxAttempts` (100) never applies.
  - `task.RaiseRunOnceMaxTimeInTx` (task/task.go:609) exists for exactly this
    and is used only by the deposit step (taskworker/work/st_work.go:432).
  - A 7-day epoch's whole-window read takes 40–55 minutes at the code's own
    measured 125–165k rows/s on about 400M contracts (model/st_provider_usage.go:88)
    *(inferred)*, so no 30-minute attempt can finish.
  - The backoff is now at its cap, 0.5–1.5 h after each failure, so the loop
    reads for 30 minutes every 1–2 h *(inferred)*: about 2–5 cores on
    average, without end.
- **The pin tax.** Per-call costs of four statements, with the snapshot's age
  (`pintax.txt`):

  | Statement | No pin (04:55–05:10Z) | Snapshot 1–11 min old (04:19–04:29Z) | 11–20 min old (04:29–04:38Z) | CPUOPT4 (11-04) |
  |---|---|---|---|---|
  | debit oldest age, subscription_transfer_debit.go:45 | 0.96 ms, 587 hits | 15.5 ms, 9,074 | 43.6 ms, 31,739 | 3.5 ms, 1,818 |
  | debit shard walk, :139 | 0.045 ms, 16 hits | 0.62 ms, 317 | 2.21 ms, 1,258 | 0.10 ms, 28 |
  | debit page `FOR UPDATE SKIP LOCKED` | 0.29 ms, 102 hits | 0.45 ms, 196 | 0.75 ms, 384 | 0.27 ms, 102 |
  | URL-probe slot claim, provider_url_probe_due.go:348 | 2.30 ms, 824 hits | 5.92 ms, 3,771 | 12.8 ms, 9,208 | 3.98 ms, 1,027 |

  At today's call rates these four cost 0.22 backends without a pin, 0.72 in
  the first window and 1.74 in the second. All statements together: 6.71
  backends of execution at 24.4k commits/s without a pin, against 8.68 at 23.3k
  twenty minutes into one; buffer hits 3.7M/s against 8.1M/s, tuples returned
  4.5M/s against 10.6M/s. transfer_debit_journal turns over about 2,000 row
  versions a second, so its dead rows grow by about 120k a minute of pin
  *(inferred)*.

### 3. What CPUOPT4's changes did

| Item | Shipped | Applied on Main | Measured against predicted | Confidence and confounders |
|---|---|---|---|---|
| D #1 expression statistics on the unresolved predicate | 8a76e30a2 (11-04), migration 926, in every deployed revision | yes: migration_audit 925→926 at 11-04 17:47:05Z; object and expression data present (§2.4) | Predicted 7–8 cores and the route back to about 20 ms. The route has been 8–24 ms in every hour since 11-04 19Z, except the 11-06 brownout. The open-sum runs 0.15 ms with 49 hits per call, on `…_unresolved_payer_transfer_byte_count` (21.3 scans/s for 21.2 calls/s). The composite index gets 1.7 scans/s (about 80/s flipped). The open set drained from 13.45M to 115k entries without a flip. | high. The flip episodes ended before the migration (14:11Z vacuum); the proof is the 8 days since, in the regime that flipped it before |
| D #2 nginx stream hop | no | no: the `stream` block on 6432 (`reuseport backlog=4096`, `proxy_timeout 300m`, `max_fails=3 fail_timeout=30s`) is unchanged; 1,115 client connections | Now 2.20 cores at 42 µs per query (predicted 5–6.5 at 11-04's load) | high |
| D #3 dump redesign | yes, as base backups: xops fe23fd2 (hold, 11-04), 4870279 (edge-6 dumps a copy of the primary, never the primary), ab04803 (fast checkpoint), 14f77b1 (WAL summaries on edge-2, 11-05), 9ed4c05 and 51599fc (incremental copies against a kept base), e42581b (full copy 11-10), c328547 (restore waits for replay) | yes: edge-2's `bringyour-backup.timer` last ran 11-01; `pgbackup-slot-guard.timer` runs every 30 s; no slot held now | No dump pin since 11-04 02:05Z. A copy costs about 1–1.4 cores while it streams (11-10/11: 17.8 h, 15–24 core-h above the same hours a day earlier) and 700–900 MB/s of disk reads. The predicted 330–420 core-h per full dump is avoided. Other long snapshots remain (§2.5) | high for the dump; the payout and shadow pins are separate items |
| D #4 reboot into `retbleed=stuff iommu=pt` | staged only (grub.d) | no: booted 10-24 00:47Z, bare cmdline, retbleed IBRS, IOMMU DMA-FQ | untested | high |
| D #5 PgBouncer to PostgreSQL over a Unix socket | no | no: all 33 database lines `host=127.0.0.1 port=5432`; 352 loopback server connections | untested | high |
| D #6 fewer round trips | no: db_owned_transaction.go unchanged since CPUOPT4 (:195 and :359 still send `SELECT pg_backend_pid()` alone) | — | Transaction control and locks are 21.7k/s, 31% of statements: `pg_backend_pid()` 351/s, advisory locks 4.1k/s, `set_config` 378/s, `LOCK TABLE` 367/s, the clock check 733/s | high |
| D #7 analyze thresholds on the queue tables | no | no: pending_task still has `autovacuum_analyze_threshold=50` at scale 0; transfer_debit_journal has none | Each table is now analyzed every 20.9 s, not 6.0 s, because writes fell; the invalidation cost fell about 3.5× by itself | high |
| D #8 rebuild the drained partial indexes | no | no: 216 GiB for 114,987 entries (230 GB for 4.41M on 11-04). The handoff plans REINDEX CONCURRENTLY after 11-14 (O2), and pg_repack 1.5.3 is installed | Plan stability is now carried by #1. The vacuum still reads the 216 GiB every cycle | high |
| D #9 JIT off | no | no: `jit` on, `jit_above_cost` 100,000 | JIT is 0.18 cores (0.26 on 11-04). The per-client contract read spends 176 of each 193 ms in JIT, the health aggregate 504 of 2,370 ms | high |
| D #10 GetTransferStats and census inputs | no: account_payment_model.go unchanged | — | GetTransferStats unpaid runs 0.4/s at 396 ms, 0.16 backends, with 1.63M hits per call (857k on 11-04) | high |
| C #4 payer drain: a page read that can stop, and a backoff | no: model/subscription_payer_due_close.go unchanged since 10-29 | — | Not needed now: `ClosePayerDueContractsTask` failed 3,681 times on 11-04 and in no run since 11-05 00Z (2,172–3,121 successes a day), and runs about 5 s a day (97,316 s in the 24 h to 11-04 10Z). The payer-due read did not run in my window. Still worth doing only as a guard if a payer plan flips again | high |

**Not a Part D item, also unapplied:** PgBouncer's `listen_backlog 65535`. Its 64
listeners still have a backlog of 128, because the change needs restarts.

### 4. New costs from recent releases

| Source | Revision, live since | Cost measured | Notes |
|---|---|---|---|
| SN25 extender shadow step (§8.6.7), epoch 4 | d1ea65129, taskworker 11-12 20:30Z; D42's 2-h bound (3939500bb) never reached the pending row | +5 to +10 cores on average per 30-minute read overnight (12 reads, 27–49 core-h), +14 in the 04:18–04:48Z read; 13 timeouts, nothing recorded; next read 05:20:50Z | §2.5. Ongoing: about 2–5 cores on average *(inferred)*. From epoch 8 the live root build reads the same statement through `stLedgerV2EpochRows` (controller/st_ledger_v2_rows.go:50) |
| §8.6 extender census and extender tables | ed692a6ec, d1ea65129 (11-12) | all 80 statements naming extender tables: 2,155 calls/s, 0.12 backends | mostly `contract_extender` lookups (1,283/s) and inserts (676/s) at contract creation, which predate §8.6 (2.7B lookups since 10-23); the census table holds 1 row and the network_extender tables under 60k rows each. The census's real cost is the shadow read above |
| FP2 S2 capability counts in the health-window aggregate | 3dcc7b138, in D42 | 0.1 calls/s at 2,370 ms, 892k hits and 504 ms of JIT per call: 0.21 backends | `providerEgressHealthCountsSql` (model/provider_egress_health_model.go:806) adds eleven capability counts and two post-quantum maxima, each a `FILTER` aggregate, over an 8-hour window of about 2M rows. On 11-04: 1,050 ms and 350k hits at 0.3/s |
| SN25 amounts for a network | the v2 reader, before D42 | 0.17 calls/s at 1,167 ms: 0.19 backends; 0.013 ms on 11-04 | `DISTINCT ON (epoch, coldkey)` over st_payout_wallet_resolution (449k rows; 0.22 sequential scans/s, 99k tuples/s) for an amount table of 90 rows (model/st_ledger_v2_reader_model.go:28, :86) |
| descendant check on every deactivation | 6f64361a7, the bounded walk after the 11-06 incident | 186.7 calls/s at 2.51 ms, 2,021 hits: 0.47 backends | One call per `UPDATE network_client SET active` (186/s). It reads every client of the parent's device since the parent was minted (network_client_device_id: 482k tuples/s). The unbounded walk it replaced browned out Main on 11-06 (249–253 core-h) |
| FP2 S1 steps 1 and 2 (history columns; PQ on turns with no outcome in 4 h) | 2f3ebac8b (D42; migration 966 at 00:14:47Z), 86553321d (D43) | history inserts 71.6/s at 0.15 ms (0.011 backends); st_client_key_history inserts 43.3/s (0.05 backends) | provider_egress_health_history holds 6.0–6.7M rows and 8.9–10.8 GiB a day on 11-06..11-12, with no change from S1. st_client_key_history is flat at 273.0M rows (272.4M on 11-03), 978 GiB in all, under its retention |
| quality probe h3 and IP family (11-11/12) | 87bd6ad33 and later | slot claims 66/s (68/s on 11-04); URL-security inserts 268/s (0.06 backends) | The claim's cost per call depends on dead rows in provider_egress_probe_cycle: 3,771 → 9,208 hits as the epoch snapshot aged |
| hosted-proxy 2-day hard delete | 91f14f258 (taskworker since 11-05) | `RemoveRetiredHostedNetworkClients` 251 runs a day at 5.35 s (0.016 in flight); all proxy_client statements 0.076 backends | proxy_client holds 129k rows |
| contract horizon reaper | on since 11-02 | 0.79 tasks in flight; batches of 0.27 s (0.85 on 11-04) | Caught up: 90.3 days behind on 11-04, 2.0 days now; 444 transfer_contract deletes/s (1,789). It now tracks creation |
| payment-driven retention | before CPUOPT4 | 14.6 s per call, 5.08M hits (31.8 s on 11-04) | one call in 1,115 s |
| close-evidence and key-history retention | db_contract_close_evidence_retention.go:632; `RemoveGoneStClientKeyHistory` | 0.019 and 0.019 backends (549 ms per evidence scan, 102 ms per key-history delete) | the reaper's cascade deletes add 0.05 backends |
| Solana receiver sweep, uncredited-payment metric, store notification flow | 48404ff74, d9c549fad, e5b0f5f60 (D42) | under 0.001 backends; the metric 0.11 ms and the notification insert 0.05 ms per call | migration 965 at 00:14:45Z; the cursor table holds 2 rows, store_notification_flow 2 |
| crypto address sweeps | 1ed3b4b44 on server main, not in D43 | none | migrations 967–969 applied at 04:07Z; the tables are empty and nothing deployed writes them |
| WAL summaries for incremental copies | xops 14f77b1 (11-05) | walsummarizer 0.11 cores; 47 MB/s of WAL read | kept on for the backups |
| weekly payout run retired | 7475aa85f (D42) | a saving: 94–183 core-h on each of the last two Sundays | §2.5 |

### 5. Ranked opportunities

#### #1 End the SN25 shadow loop, and keep epoch reads from pinning the primary

- **Change**, in order:
  - **(a) End the loop now (owner's choice of two).** Either the owner retires
    epoch 4's shadow report, if §8.6.7 no longer needs it, and the chain's
    pending row is cleared. Or the sync raises an existing row to the bound the
    code declares: `task.RaiseRunOnceMaxTimeInTx` beside
    `ScheduleTaskInTxIfAbsent` at taskworker/work/st_ledger_v2_usage_work.go:75,
    as the deposit step does at taskworker/work/st_work.go:432 ("a chain an
    earlier binary left pending lacks" this binary's run time). The second
    applies the 2-h bound D42 already merged. CODESTYLE.md asks for exactly
    that: a stale, earlier deadline must not reject a healthy operation before
    its full budget. It does let one read hold its snapshot up to 2 h, so (c)
    matters more after it. Check the other `...IfAbsent` chains whose
    `MaxTime` changed for the same stale-row defect.
  - **(b) Count a timed-out attempt.** Today the framework's error reschedule
    bypasses the Post, which is the only place attempts are counted. A step
    that times out should count toward the chain's `MaxAttempts`, so the chain
    stops with a metric and a log line rather than re-reading the whole epoch
    at every backoff (CODESTYLE.md: stop retrying definitive failures for
    unchanged inputs). Any new limit is a field of
    `StLedgerV2UsageStepSettings`. Retries after other errors stay as they are.
  - **(c) Shorten the snapshot.** Read the 7-day window in close-time slices,
    each in its own short REPEATABLE READ transaction, with the progress
    checkpointed so a failed slice does not restart the read (CODESTYLE.md:
    checkpoint resumable progress). A terminal close time never changes and
    each row has one, so the slices together see each row once *(inferred)*.
    This changes the statement's one-snapshot reasoning, so its author must
    review it. Alternatively, read edge-6's restored copy once it has replayed
    past the epoch's close.
- **Code** (978dcba02): taskworker/work/st_ledger_v2_usage_work.go:42-45 (100
  attempts, 15 s, 30 minutes, 2 h), :75 (the sync) and :138 (the Post);
  task/task_completion.go:77-118 (the error reschedule), task/task.go:200-213
  (its backoff) and :609 (`RaiseRunOnceMaxTimeInTx`);
  controller/st_ledger_v2_extender_shadow.go:172-174;
  model/st_extender_pool.go:239 and :325-327 (one REPEATABLE READ snapshot on
  the maintenance pool without a transaction timeout); controller/st_ledger_v2_rows.go:50
  (the live root build, from epoch 8).
- **Evidence (§2.5):** 13 timed-out reads of 30 minutes, nothing recorded; the
  pending row's bound is still 1800 s with the attempt at 1. Each read lifts
  edge-2 from about 26 cores to 35–44 by its end: +5 to +10 cores on average
  per read overnight (2.7–5.1 core-h each, 27–49 core-h for 12), +14 in this
  morning's read (about 7 core-h). Four hot statements cost 0.22 backends
  without a pin and 1.74 twenty minutes into one; all statements, 6.71 against
  8.68 backends.
- **Saving:** (a) saves about 2–5 cores on average from now, for as long as the
  loop would otherwise run *(inferred: 3–7 core-h per read, one read every 1–2
  h)*. A read allowed to finish in 40–55 minutes costs perhaps 10–15 core-h,
  once *(inferred: the pin tax keeps growing with the snapshot's age)*. (c)
  removes most of that and of any later epoch read's pin tax, including the
  weekly root builds from epoch 8.
- **Effort and risk:** (a) and (b) are small and low risk. Each needs a
  deterministic regression test: a pending row scheduled with the old bound is
  raised at the next sync; a timed-out attempt advances `Attempt` and the chain
  ends at the limit. (c) is medium: it touches the settlement statement's
  determinism and needs its author and a replay check
  (controller/st_ledger_v2_extender_replay.go:97).
- **Timing bounds:** retiring the shadow or counting timeouts moves no bound.
  Raising the row to 2 h applies a bound the owner already merged (3939500bb)
  and touches no user-facing timing. (c) moves no deadline.
- **Verify:** `pg_stat_activity_max_tx_duration` for the application user stays
  under about 5 minutes outside the weekly reads; the debit oldest-age query
  stays near 3.5 ms; the pending usage-step row's `reschedule_error_count`
  stops rising; a `shadow` phase in `urnetwork_st_extender_pool_epoch` if the
  report is kept.
- **Owner:** whether epoch 4's shadow report is still wanted (an SN25 decision).
  For (c), the SN25 statement's author, and the owner if the edge-6 copy is
  used.

#### #2 Remove the nginx stream hop (CPUOPT4 #2)

- **Change, risk and bounds as CPUOPT4 #2:** a pgx `DialFunc` over ports
  6433–6464 that reproduces `max_fails=3 fail_timeout=30s`, or PgBouncer
  `so_reuseport` with `[peers]`; keep `proxy_timeout 300m` as
  `client_idle_timeout = 18000`. Medium risk: the database entry path, and
  moving 6432 resets connections. No timing bound moves.
- **Evidence and saving now:** nginx 2.20 cores, 79% kernel, 42 µs per query
  for 1,115 client connections. That is 2.0–2.5 cores plus part of the loopback
  softirq (CPUOPT4: 5–6.5 at twice the load). Verify on one api block:
  PgBouncer and nginx CPU per query, softirq, client query latency.

#### #3 Reboot edge-2 into the staged kernel options (CPUOPT4 #4)

- **Change:** the owner-scheduled reboot that applies `retbleed=stuff iommu=pt`
  (already in grub.d). **Evidence:** kernel time is about 8–10 cores (sys
  6.5–8.0, softirq about 2.2); retbleed uses IBRS and the IOMMU is in DMA-FQ;
  both hops are 70–80% kernel time. **Saving:** 1–2.5 cores *(unmeasured;
  CPUOPT2's 3–8% scaled to today's kernel time)*.
- **Risk:** medium, a primary restart. Follow the 10-13 lessons (stop the
  taskworker containers for an outage over about 100 s; expect the post-start
  fork stall), outside the validator gate and any base-backup copy. **Timing
  bounds:** none; `retbleed=stuff` keeps the mitigation. **Verify:** sys and
  softirq per commit, hop CPU per query, against §2.2.

#### #4 Rebuild transfer_contract's bloated indexes, the drained partials first (CPUOPT4 #8; O2)

- **Change:** REINDEX CONCURRENTLY, one index at a time and off-peak, under the
  copy schedule. Start with the five open-set partials, 216 GiB for 115k entries
  that should fit in well under 1 GiB *(inferred)*. Then the full indexes the
  reaper thinned (1,637 GiB of indexes for 2.07B rows that were sized for 2.93B),
  as the disk-runway plan's O2 proposes after 11-14.
- **Evidence (§2.3, §2.4):** autovacuum reads 544 MB/s in its index phase.
  transfer_contract vacuums run back to back about every 2.6 h. Each pass reads
  every index, so bytes removed come off every cycle.
- **Saving:** 1–1.5 cores in the IO workers, dm-crypt and the vacuum's own CPU
  *(inferred: about a third fewer index bytes per cycle, if O2's estimate of
  about 2 TiB reclaimable holds)*; plus disk, and a planner that no longer
  depends on #1's statistics for the partials.
- **Risk:** medium. Each rebuild scans the 929 GiB heap and holds a snapshot for
  its length. That is the long-snapshot cost of §2.5, so only one at a time,
  outside the evening and the Sunday reads, with the debit and overdue stops of
  CPUOPT4 #3.
- **Timing bounds:** none.
- **Verify:** relpages against reltuples per index; transfer_contract vacuum
  length and vacuum-context read MB/s; IO-worker and dm-crypt CPU.
- **Owner:** the window (already planned as O2).

#### #5 PgBouncer to PostgreSQL over a Unix socket (CPUOPT4 #5)

- **Change:** `host=/var/run/postgresql` in pgbouncer.ini.j2 and a `local
  bringyour bringyour` scram line in edge-2's pg_hba.conf, one instance at a
  time. **Evidence:** PgBouncer sys 2.07 cores and backend sys 2.27 over 352
  loopback server connections. **Saving:** 0.7–1.5 cores *(unmeasured)*.
  **Risk:** low, reversible per instance. **Timing bounds:** none
  (`server_connect_timeout`, `server_login_retry` unchanged). **Verify:** one
  instance against the other 31 for an hour, CPU per query and its backends'
  sys time.

#### #6 Fewer round trips (CPUOPT4 #6)

- **Change:** as CPUOPT4: take the backend PID from the admission statement that
  already returns it (db_owned_transaction.go:479) instead of `SELECT
  pg_backend_pid()` at :195 and :359; fold the clock check and `set_config`
  into the statements they guard. **Evidence:** transaction control and locks
  are 21.7k/s, 31% of statements; a round trip costs about 98 µs in the two
  hops plus backend protocol work outside `active_time` (§2.2). **Saving:**
  0.5–1 core. **Risk:** low–medium (ownership and fencing code). **Timing
  bounds:** none; the same locks and checks in fewer messages. **Verify:**
  PgBouncer queries per transaction (2.54 now), CPU per transaction.

#### #7 An index for the SN25 amounts read

- **Change:** an index on st_payout_wallet_resolution `(deployment_key, no_id,
  epoch, coldkey, client_id) INCLUDE (network_id)`, so the `DISTINCT ON` reads
  only the requested epochs in order; or join from the 90 amount rows'
  coldkeys first. **Code:** model/st_ledger_v2_reader_model.go:28 and :79-86,
  from controller/st_ledger_v2_readers.go:100. **Evidence:** 1,167 ms per call
  at 0.17 calls/s, a full sequential scan of 449k rows each, growing an epoch
  at a time. **Saving:** about 0.2 cores now. **Risk:** low (an additive
  CONCURRENTLY build on a 0.2 GiB table, before the code deploy).
  **Timing bounds:** none. **Verify:** that queryid under 10 ms;
  st_payout_wallet_resolution's `seq_scan` stops growing.

#### #8 A narrower descendant check on deactivation

- **Change:** serve `readActiveChildClientIdsInTx` from a partial index of
  active clients by device, for example `network_client (device_id, client_id)
  WHERE active`. **Code:** model/network_client_model.go:1689 and :1732, from
  :1546. **Evidence:** 186.7 calls/s at 2.51 ms and 2,021 hits;
  network_client_device_id reads 482k tuples/s. **Saving:** 0.3–0.4 cores
  *(inferred)*. **Risk:** medium: another index on a 427M-row, 233 GiB table
  whose `active` flips about 200 times a second, on the walk that browned out
  Main on 11-06; measure its write cost on a restored copy first. **Timing
  bounds:** none; the same descendants are deactivated in the same
  transaction. **Verify:** hits per call; the index's write amplification.

#### #9 JIT off for OLTP (CPUOPT4 #9)

- **Change:** `jit = off`, or `jit_above_cost` of 10M or more (postgresql.conf;
  reload). **Evidence:** 0.18 cores of JIT; the per-client contract read spends
  176 of each 193 ms in JIT, the health aggregate 504 of 2,370 ms,
  GetTransferStats 22 ms per call. **Saving:** 0.15–0.2 cores. **Risk:** low.
  **Timing bounds:** none. **Verify:** pgss `jit_*` deltas near 0.

#### #10 GetTransferStats and the health-window aggregate (CPUOPT4 #10)

- **Change:** CPUOPT3's incremental unpaid total, and one computed
  health-window aggregate per export reused by its readers. **Evidence:**
  GetTransferStats unpaid 0.16 backends at 1.63M hits per call
  (model/account_payment_model.go:1148); the aggregate 0.21 backends
  (model/provider_egress_health_model.go:806). **Saving:** 0.2–0.4 cores.
  **Risk:** low–medium. **Timing bounds:** none; the window and the export
  cadence stay the same. **Verify:** those queryids' hits per second.

#### Noted, not a separate change

- **The Sunday payout pin is retired** by D42 (7475aa85f). Confirm on 11-15
  10:00–14:00Z that edge-2 stays on the fit.
- **Analyze cadence on the queue tables (CPUOPT4 #7)** is now every 21 s; it is
  worth doing only with #4's migration window, for 0.1–0.5 cores *(inferred)*.
- **The payer drain's guard (CPUOPT4 C #4)** is not worth building at today's
  load while #1 holds.

### 6. Considered and rejected

- **Sampling the debit-lag gauge less often** (it runs twice per flush page, 8.9
  times a second, and suffers most under a pin). It is the input to the debit
  lag alert, so a slower sample delays detection. Rejected under the owner rule;
  #1 removes its cost instead.
- **A transaction or statement timeout on the maintenance pool** to cut long
  snapshots. It would cut legitimate settlement reads that have their own
  bounds. Rejected; #1 changes the retry and the snapshot instead.
- **Shortening the usage steps' task bounds, or the 2-h shadow bound.** These are
  timeouts; a shorter one only makes reads fail more often. Rejected.
- **Running the epoch read on a parallel plan to finish sooner.** The code turns
  parallelism off on purpose to keep row order deterministic
  (model/st_provider_usage.go:99-104). Rejected.
- **Raising `checkpoint_timeout`** to cut FPIs (15.5k/s), and **raising
  `autovacuum_freeze_max_age`**, as in CPUOPT4: a timeout, and lost wraparound
  margin.
- **Lowering `idle_in_transaction_session_timeout`, `query_wait_timeout` or
  PgBouncer's server timeouts.** Timeouts; idle-in-transaction is 25 sessions now
  and costs no CPU.
- **Pausing the reaper or retention for CPU.** They now cost under 1 backend, and
  retention and disk are owner decisions.
- **Turning off WAL summaries.** They serve the incremental base backups that
  replaced the dump, for 0.11 cores.

### 7. Raw outputs

Under `<scratch>/d/`: scripts in `scripts/` and `host/`, SQL in `sql/`, outputs in
`raw/`. Query text is pg_stat_statements' normalized form with strings, UUIDs
and long numbers stripped on the server.

- **pgss:** `snap-{1..6}.json`, `pgss-delta-{1-2,1-3,1-4,3-4,5-6}.tsv`,
  `pgss-rank-{1-2,1-3,1-4,3-4,5-6}.txt`, `texts-1.tsv`, `texts-all.tsv`,
  `families-{1-4,5-6}.txt`, `pintax.txt`, `newfeat-1.{json,tsv}` (cumulative,
  feature tables).
- **Catalog and the shadow step:** `stx-1.json` (statistics object,
  migration_audit, vacuum), `settings-1.{json,txt}`, `sizes-1.{json,txt}`,
  `longtx-{1..6}.json`, `acttext-1.json` (the one stripped text),
  `shadowcheck-1.json`, `shadowtask-1.json`, `shadowrun-*.json`, `fncheck.json`.
- **Host:** `edge2-host-facts.out`, `census-1.out`, `pgb-stats-1.{out,rates}`.
- **Mimir:** `hist-5m.json`, `hist-evening.txt`, `hist-daily.txt`,
  `hist-hourly-1112.txt`, `hot-hours.txt`, `episodes-cost.txt`,
  `regress-busy-vs-commits.txt`, `acquires-by-service.txt`,
  `route-subscription-balance.txt`, `reaper-metrics.txt`, `shadow-cycles.txt`,
  `cycle13-5m.txt`.

## Part E — The Redis cluster

This part is read-only research on Main, 2026-11-13 04:18–05:05Z. Sources:
- Mimir: the redis exporter (48 nodes), node exporter for edge-5 and edge-6, and client-side metrics;
- redis-cli on edge-5 and edge-6 only, through ansible: `INFO everything` and `LATENCY LATEST` (three snapshots), `CONFIG GET`, `CLUSTER NODES` reduced to slot ranges, `SLOWLOG GET 1024`, `CLIENT LIST` summarized on the host;
- `/proc`, `/sys`, `systemctl show`, `zfs get` and `zpool iostat` on the two hosts;
- code read at the deployed revisions.

Nothing in production was changed. No key, argument, value, client address or client name was printed or kept; client sources are salted pseudonyms and were not mapped. No perf sampling was run; two 30 s `perf stat` counts were taken afterwards for §5.2 (§2.7). *(inferred)* marks a claim reasoned from code, timing or per-node command mix rather than measured directly. Raw outputs are cited as `<scratch>/e/…`.

### Summary

- **Redis CPU fell by a third, because the clients did.**
  - The 3-day mean went from 31.2 to 20.4 cores (peak 43.5 → 24.9). Requests went from 185k to 115k/s. All-in cost is 178 µs per request, against 172.
  - **Evening peaks, 18–22Z:** 19.3–23.0 cores on 11-06..11-12, against 37.0 on 11-03.
  - **Per connected client nothing changed:** 0.78–0.81 Redis requests/s per client in every window, against 0.77–0.81 in CPUOPT4's. Connected clients fell from 271k (11-03 evening) to 150k (11-12 evening).
  - **Per request, on the same host posture, nothing changed either.** The marginal I/O-thread cost per request is 58.1 µs on edge-5 and 84.1 µs on edge-6 (11-07..11-10), against 52.5–56.5 and 80.6–90.7 in CPUOPT4.
- **No node is hot.** The busiest main thread uses 0.27 of a core, against 0.87–0.97 on 6383 in CPUOPT4.
  - The escrow hot node is gone: proxy reservation operations fell from 8.6k to 0.14k/s, and proxy pool timeouts from 109/s to 0. 6383 is a median node now (9.8k commands/s).
  - Memory halved, from 60.0 to 31.4 GB.
- **edge-6's backup restore stalls two thirds of the slots every few seconds.**
  - Since the base backup copy started at 11-10 21:15Z, edge-6 logs 20–130k slow entries an hour, against about 750 before. Every command type is affected, TIME and PTTL included.
  - In a 75-minute window, 11% of seconds had 8 or more of the 32 nodes stalled at once. The most common gap between those seconds is exactly 5 s, ZFS's transaction-group interval.
  - **The cause:** 206–210 ZFS write threads at nice −19/−20 preempt Redis. edge-6 main threads take 2,017 involuntary context switches/s, against 75 on edge-5.
  - **Host cost:** about 6–7 cores of ZFS and PostgreSQL work during the replay. While the base backup was copied, edge-6 ran 40–62 busy cores, 30–53 of them system time.
  - No client saw a Redis error: pool timeouts are 0 on every job.
- **Redis runs on slow clocks.**
  - **edge-5:** the CPUs under Redis threads average 1.62 GHz, and 78% of samples are below 1.6 GHz, at or near the 1.5 GHz floor (`schedutil`, boost off, 2.3 GHz max).
  - **edge-6:** they run at 2.36–2.61 GHz, but only because the restore keeps cores busy. When the restore began, edge-6's cost per request fell 11–15% while edge-5's fell 1–3%.
- **CPUOPT4's Redis items: none of the code items shipped**, and `kernel.numa_balancing` is still 1 on edge-6.
  - CPUOPT4 #1, the balance-aware refill hold, removed 6383's saturation.
  - CPUOPT4 §5.4's target shrank 75×: proxy page-cache lookups fell from 1,242/s to 16/s.
- **New costs are small.**
  - **D42's `:native_cap_v1`:** +1.1M keys, +0.15–0.25 GB and +0.8k expired keys/s. Its CPU change is below detection, and it has no readers yet (0.006 select requests/s).
  - **The `/connect/control` limit:** about 70% of EVALSHA. It climbs from 1.8k to 12–14k calls/s over each connect cycle: the control-route creep, an open alert.

**Ranked opportunities** (Redis cores at the evening peak, about 20.5–23 now; they overlap, so do not add them all):

| # | Change | Saving | Risk |
|---|---|---|---|
| 1 | No preflight PING by default (§5.1; CPUOPT4 #12, not shipped) | 1.0–1.7 cores (0.9–1.6 daily mean); 13–15k fewer requests/s | low |
| 2 | A performance clock policy on the Redis hosts (§5.2; new) | 1.0–1.7 cores on edge-5 now; 1.8–2.5 on edge-6 whenever no restore runs *(inferred)* | low; owner (power) |
| 3 | Take edge-6's restore off Redis's CPUs: another host, or clone at 8K records with ZFS writers at normal priority (§5.3; new) | 0 Redis cores; 6–7 edge-6 cores while it replays; removes host-wide stalls of 10–470 ms | low–medium; owner (xops) |
| 4 | `kernel.numa_balancing=0` on edge-6 (§5.4; CPUOPT4 §5.3, not applied) | 0.15–0.25 cores, plus a stall source | low; owner |
| 5 | Find the `/connect/control` creep (§5.5; cross-part, with C) | 1.7–2.7 cores at the top of a 1.5-day cycle, 0.8–1.4 averaged *(inferred)* | investigation |
| 6 | GETEX in the resident poll, prune inside the MULTI (§5.6; CPUOPT4 #18) | 0.25–0.45 cores | low |
| 7 | EVAL → EVALSHA for the hot scripts, adding the net-escrow refresh (§5.7; CPUOPT4 #19) | 0.25–0.4 cores; 25–31 MB/s less inbound | low |
| 8 | `{provider_hard_exclusions}` published only on change (§5.8; CPUOPT4 §5.6) | ≈0.01 cores; removes a 118 ms whole-node block on 6415 every ~12 s | low |
| — | Per-slot statistics; conntrack bypass; `iommu=pt` on edge-5; cpusets; an io-threads 1 trial; SRSO (§5.9) | see §5.9 | owner |

### 1. Method

- **When and load:**
  - Live captures ran 2026-11-13 04:24–05:05Z, a Friday early morning, with 146–150k connected clients and Redis at 19.7–20.9 cores.
  - Like-with-like comparisons use Mimir for the 18–22Z evenings of 11-06..11-12, for 11-13 00–04Z, and for the 3 days 11-10 04Z – 11-13 04Z. CPUOPT4's windows were recomputed with the same scripts.
  - Costs are normalized per request (`total_reads_processed`) and per connected client (`urnetwork_connect_connected_clients`).
- **Builds in the windows** (`urnetwork_build_info`; `<scratch>/fleet/gens.txt`):

  | Job | In the snapshots (04:24–04:33Z) | Evening 11-12 18–22Z |
  |---|---|---|
  | connect (20) | 1096979900, a00dfeb9a (since 11-12 17:10Z) | same, from 17:30Z |
  | api (20) | D42 1097289240, a151cee8d | 1096979900 a00dfeb9a, then 1097121380 d1ea65129 from 21:10Z |
  | taskworker (8) | D43 1097411970, 978dcba02 (8 of 8 since 04:10Z) | 1096979900, then 1097121380 from 20:30Z |
  | alt (2) | D42 → D43, rolling | as the api |
  | proxy (20) | 1096461840, 89be8b3b8 | same |

  - **D42's capability snapshot** (43dd4ec30 in merge 3dcc7b138) is in a151cee8d and 978dcba02. It is not in d1ea65129 or dcb6f6c20, so the first writer started at 11-13 01:30Z.
  - **D43** finished on taskworker at 04:10Z and on alt during the snapshots (done by 04:47Z). The api was still on D42 at 05:00Z. No D43 evening exists yet.
- **Redis host captures** (all through `ansible -m script`, summarized on the host):
  - **`snap.sh`, three times:** A 04:24:54Z, B 04:27:11Z and C 04:33:00Z; A→C is 486 s. Each run takes INFO and LATENCY of all 48 nodes, per-thread CPU and context switches, `/proc/stat`, vmstat, PSI, meminfo, ZFS ARC, and per-process CPU and RSS. PostgreSQL processes are named by title only.
  - **`summ.py`:** CONFIG GET, the slot map, SLOWLOG (command name, duration, time, and for EVAL a fingerprint), LATENCY, and a CLIENT LIST summary.
  - **`pubsub-clients.py`:** the subscriber connections' command rates (counts only).
  - **`host-posture.sh`:** kernel, IOMMU, sysctl, cpufreq, cpuidle, swap, netfilter, ZFS and units.
  - **`freq-sample.sh`:** the clock of each Redis thread's last CPU, 10 samples.
  - **`zfs-detail.sh`:** dataset properties, the copy's data directory, and `zpool iostat`.
- **Code:** read with `git show` at a00dfeb9a (connect), a151cee8d (api, alt), 978dcba02 (taskworker) and 89be8b3b8 (proxy).
  - Lua literals were fingerprinted at all four (`lua-fingerprints.py`, 125–127 scripts).
  - Fixed hash tags were mapped to ports from `CLUSTER NODES`.
  - xops was read at c1836e5.
- **Not captured:**
  - per-slot CPU (slot statistics are off);
  - a per-caller split of the ~3–4k/s PINGs that contract-hole calls do not explain (no counter exists);
  - edge-6's clock frequency before the restore (no cpufreq series in Mimir);
  - which process burned edge-6's 30–53 cores of system time during the base backup copy (no census was taken then);
  - a breakdown of ZFS write-thread CPU (it needs a kernel profile).
- **Nothing was refused.**
- **Two notes from Part A's profiles, checked:**
  - **go-redis PubSub health-check PINGs:** measured at ≤105 commands/s over all 2,453 subscriber connections (only 61 last ran PING), so they are not the missing PING source (§2.4).
  - **D1:** on since 11-04 (§3).

### 2. Where the CPU goes now

#### 2.1 Totals, normalized per request and per client

Process CPU is `used_cpu_sys + used_cpu_user`; requests are `total_reads_processed`. Mean (peak) of 5-minute rates (`<scratch>/e/out/core-evening.txt`, `core-long.txt`, `norm-clients.txt`).

| Window | Process cores | Main threads | Kernel share | Requests/s | Commands/s | µs per request | Requests/s per 1k clients | New conns/s |
|---|---|---|---|---|---|---|---|---|
| CPUOPT4 3 d, 11-01 10Z – 11-04 10Z | 31.3 (43.5) | 15.4 | 52% | 185k | 821k | 171 | — | 176 |
| CPUOPT4 11-03 18–22Z | 37.0 (40.7) | 18.2 | 59% | 208k | 859k | 182 | 765 | 403 |
| 3 d, 11-10 04Z – 11-13 04Z | 20.4 (24.9) | 10.9 | 51% | 115k | 515k | 178 | — | 79 |
| 11-10 18–22Z (before the restore) | 23.0 (24.9) | 12.3 | 49% | 121k | 538k | 191 | 778 | 77 |
| 11-12 18–22Z (replay) | 20.5 (21.9) | 10.8 | 53% | 121k | 529k | 169 | 809 | 86 |
| 11-13 00–04Z | 19.7 (21.7) | 10.4 | 52% | 114k | 499k | 173 | 777 | 77 |
| Snapshots A→C, 04:24:54–04:33:00Z | 20.9 | 11.1 | 52% | 120k | 518k | 175 | — | 78 |

- **The fall is volume.** Requests per connected client are flat, and Redis cores per 1k clients are 0.135–0.152 in every window.
  - Connected clients fell from 271k to 150k at the evening peak (Parts A and B explain why).
  - Connect's Redis round trips fell from 141k to 84k/s. The proxy's fell from 15.4k to 1.1k/s, and the api's from 18.7k to 12.9k/s.
- **Evenings 11-06..11-12:** 20.8, 19.3, 21.1, 22.7, 23.0, 21.2 and 20.5 cores, at 93k–121k requests/s and 169–214 µs per request.
  - Cost per request falls as load per node rises, as CPUOPT4 found.
  - On edge-6 it also fell when the restore began (§2.7).
- **Per request, on the same posture** (`regress.py`, 3-hour windows; `<scratch>/e/out/regress-pre.txt`, `regress-replay.txt`):

  | Window | edge-5 I/O-thread µs per request (R²) | edge-6 I/O-thread µs per request (R²) |
  |---|---|---|
  | CPUOPT4, 10-29..31 and 11-03..04 | 52.5–56.5 | 80.6–90.7 |
  | 11-07 00Z – 11-10 21Z (before the restore) | 58.1 (0.94) | 84.1 (0.96) |
  | 11-11 18Z – 11-13 04Z (replay) | 52.4 (0.94) | 66.0 (0.68) |

  The marginal cost is unchanged until the restore. Both hosts drop in the replay window, edge-6 more, but releases also changed in it, so the regression alone cannot separate the clock effect.

#### 2.2 Threads, kernel, wakeups (A→C, 486 s; `<scratch>/e/out/delta-AC-*.txt`)

| Host | I/O threads (user / sys) | Main threads (user / sys) | Requests/s | µs per request all-in | outside commands | Voluntary switches per request | Involuntary switches/s (main / I/O) |
|---|---|---|---|---|---|---|---|
| edge-6 (EPYC 7601, 8 NUMA nodes) | 6.38 (1.92 / 4.46) | 7.74 (4.89 / 2.86) | 76.7k | 184 | 132 | 1.84 | 2,017 / 1,661 |
| edge-5 (EPYC 7352, 2 NUMA nodes) | 3.46 (0.94 / 2.51) | 3.34 (2.24 / 1.09) | 42.9k | 159 | 124 | 2.18 | 75 / 94 |
| CPUOPT4 edge-6 (11-04) | 13.46 | 11.52 | 152k | 164 | 124 | 1.06 | 465 / 5,088 |
| CPUOPT4 edge-5 (11-04) | 5.91 | 5.24 | 87k | 128 | 97 | 1.66 | 116 / 264 |

- **Kernel time** is 52% of Redis CPU, and I/O threads are 47% (CPUOPT4: 51–59% and 54%).
- **The event loop** is busy 10.0 cores, 54% of it in commands (CPUOPT4: 59%).
- **Scripts** (EVAL 3.68 and EVALSHA 0.96 cores) are 63% of command time (CPUOPT4: 76%). INFO counts commands inside scripts twice, so the share is approximate.
- **Per-request cost and switches** are higher than in CPUOPT4's snapshot because each node does half the work: fewer requests per wake.
- **edge-6's main threads are preempted 4.3× as often as on 11-04, at half the load.** That is the restore's ZFS threads (§2.5).

#### 2.3 Per node and why the top nodes are what they are

Median node: 10.1k commands/s, main thread 0.217 cores. 11-13 00–04Z (`<scratch>/e/out/pernode-1113-0004.txt`, `pernode-1112E.txt`, `nodemix-1113.txt`):

| Node | Host | Commands/s | Main | Why | Evidence |
|---|---|---|---|---|---|
| 6416 | edge-5 | 19.3k (20.0k at 18–22Z) | 0.246 | `{np_<network>}` of the largest provider network (provider-intent index), as in CPUOPT4 | EVALSHA 935, ZSCORE 3,672, ZADD 1,907, TTL 602 per s |
| 6427 | edge-5 | 14.4k | 0.256 (the busiest) | a second large provider network's `{np_<network>}` *(inferred)* | the same signature: EVALSHA 467, ZSCORE 1,963, ZADD 625 per s |
| 6420 | edge-5 | 11.4k, with the most requests (4.1k/s) | 0.209 | `verify_eligible_v2`, slot 4467 | SREM 1,228/s and SADD 374/s |
| 6415 | edge-5 | 10.3k | 0.213 | `{provider_hard_exclusions}`, slot 9358 | SMISMEMBER 35/s at 484 µs; SADD 15/s at 625 µs mean, with a 118 ms republication every ~12 s (§2.6) |
| 6399 | edge-6 | 9.9k | 0.244 | the client-score native page node | output 2.7–3.0 MB/s, against 138 MB/s in CPUOPT4 |
| 6383 | edge-6 | 9.8k | 0.234 | CPUOPT4's saturated escrow node, now median | 62–70k commands/s and main 0.87–0.97 on 11-04 |

- No node reaches 1.3× the median main thread. The top nodes are the key families CPUOPT4 named, at a fraction of their 11-04 rates.
- **Slot spread of the new keys:** the client-score native snapshots, D42's included, hash-tag on the (caller location, target) pair (`{cs_<fm>_<mode>_<caller location>_<target>}`, model/network_client_location_model.go:5393 at 978dcba02). They spread over all nodes and make no hot node.

#### 2.4 Commands (11-12 18–22Z against CPUOPT4's 11-03 evening; `<scratch>/e/out/cmd-compare.txt`)

| Command | Calls/s (cores) now | 11-03 18–22Z | µs/call now |
|---|---|---|---|
| EVAL | 53.2k (4.21) | 69.4k (4.88) | 79 |
| EVALSHA | 6.4k (0.65); sawtooth 5–17k (§4) | 14.8k (1.16) | 102 |
| SET | 32.7k (0.69) | 40.7k (0.67) | 21 |
| EXEC | 8.5k (0.31) | 11.7k (0.42) | 37 |
| GET | 66.3k (0.20) | 93.9k (0.28) | 3.0 |
| PEXPIRE / PEXPIREAT / EXPIRE | 18.8k / 16.1k / 13.5k (0.43) | 39.6k / 32.3k / 16.9k (0.57) | 7.5–11.1 |
| HINCRBY / ZADD / HSET | 35.4k / 17.8k / 12.0k (0.34) | 41.5k / 37.2k / 29.7k (0.51) | 4.2–6.5 |
| HMGET | 8.0k (0.10) | 20.5k (0.23) | 12 |
| SMISMEMBER (6415) | 48 (0.022) | 340 (0.18) | 454 |
| SPOP | 3.2k (0.035) | 14.8k (0.045) | 11 |
| PING | 14.4k (0.012), each a full request | 32.4k | 0.8 |

- **Per-call time is higher on edge-6 than on edge-5 for the same commands,** 1.4–1.6× in A→C: EVAL 83 against 56 µs, SET 21.6 against 14.3, GET 4.4 against 2.4.
  - edge-6's Redis threads currently run at a higher clock than edge-5's (§2.7), so this is the older cores (Zen 1 against Zen 2), remote NUMA memory with balancing on, and preemption *(inferred)*.
  - Redis command durations are wall time, so on edge-6 they include preemption.
- **PING sources:**
  - Contract-hole calls (`urnetwork_contract_hole_operations_total`) are 10.5k of the 14.4k PINGs/s on the 11-12 evening (73%), and 10.1k of 13.3k/s on the 3-day mean (76%).
  - Subscriber health checks add at most 105 commands/s: 2,453 subscriber connections, 61 of them last ran PING (`<scratch>/e/out/pubsub-*.txt`).
  - The remaining ~3–4k/s come from the other 32 preflighting call sites.
- **EVAL bodies with known rates** sum to at least 25–31 MB/s of the ~105 MB/s inbound:
  - contract-hole read 1,301 B × 6.6–8.2k/s;
  - net-escrow refresh 1,238 B × 5.8k/s;
  - contract-hole event 1,961 B × 2.2k/s;
  - reservation 5,849 B × ~0.7–1.3k/s;
  - recovery page 1,416 B × ~0.6k/s.

#### 2.5 Host posture and co-tenants (`<scratch>/e/out/posture-*.txt`, `delta-AC-*.txt`, `ts-host-edge*.txt`, `zfs-*.txt`)

| | edge-6 | edge-5 |
|---|---|---|
| Boot | 11-03 04:23Z; no reboot since | 10-23 19:58Z |
| IOMMU | `iommu=pt` active (127 identity groups) | staged in grub; still DMA-FQ (126 groups) |
| NUMA balancing | **on**: 76k hint faults/s, 18.1k page migrations/s (CPUOPT4: 191–201k and 41–44k) | off (`/etc/sysctl.conf`) |
| Clock | `acpi-cpufreq` `schedutil`, boost on, 1.2–2.2 GHz plus boost | `schedutil`, **boost off**, 1.5–2.3 GHz |
| Memory | 472 GiB (507 GB); available 150 GiB. ZFS ARC 255 GB, at its cap of 253.5 GB (half of RAM). The PostgreSQL copy has 34 GB of shared memory; Redis RSS is 26.5 GB | 252 GiB; available 89 GiB. MinIO RSS 29–33 GB; Redis RSS 13.5 GB |
| Swap | 0.44 of 8 GB; Redis 0 | 7.4 of 8 GB, of which MinIO holds 7.49 GB and Redis 0.01; 2 swap-ins/s |
| PSI, A→C | cpu some 11.4%, io full 4.0%, memory 0 | cpu some 2.1%, io full 0.3%, memory 0.03% |
| Co-tenants, A→C | ZFS write threads 6.4 cores (`z_wr_*` 4.70, `dp_*` 1.03, `z_rd_*` 0.34, kworker 0.33); PostgreSQL copy 0.35 (startup 0.29) | MinIO 3.68 cores |
| Netfilter | `nf_conntrack` loaded, 58k entries; no NOTRACK rules | not loaded |
| SRSO / retbleed | Safe RET; untrained return thunk, STIBP disabled | Safe RET; STIBP always-on |
| Units | no `CPUAffinity` or `NUMAPolicy`; `MemorySwapMax=0` | same |

**edge-6's restore, by phase** (node exporter, hourly; the restore is `bringyour-pg-copy`, a PostgreSQL 18 instance replaying WAL):

| Phase | Busy cores | System | Redis | Disk writes | MemAvailable | edge-6 slow entries/h |
|---|---|---|---|---|---|---|
| Before, 11-10 18–21Z | 16.7–18.2 | 5.9–6.1 | 16.2–16.9 | 5–13 MB/s | 401–425 GB | 700–840 |
| Base backup copy, 11-10 21:15Z – 11-11 ~17:30Z | 40–62 | 30–53 | 11.7–14.1 | 153–289 MB/s | 202–292 GB | 57k–132k |
| WAL replay, 11-11 18Z – now | 24–31 | 15–21 | 12.3–15.1 | 160–240 MB/s | 159–173 GB | 19k–57k |

- **The copy's data directory** is the clone `archive/bringyour/pg-run` of `pg-base@run-2026-11-10-21-15-00`: aes-256-gcm, lz4, on two mirrors. 644 GB has been written to the clone so far.
  - The clone inherits recordsize 1M from `archive/bringyour`. The clone command sets none (xops c1836e5, main/ansible/host_files/by-us-fmt-5-edge-6/db-archive/bringyour-pg-backup.sh:796).
  - So files the replay creates get 1 MB records, while the base's files keep its 8 KB records (playbook-db-archive.yml:58). The clone compresses 1.78× against the base's 1.46×, which suggests many large-record writes *(inferred)*.
  - `zpool iostat` shows 41 MB/s of reads (956 ops/s) beside 63 MB/s of writes. The reads average ~43 KB, so 1 MB read-modify-write is not the main cost now.
  - Why ~6 cores of write threads go to 60–200 MB/s is not determined; a kernel profile of the `z_wr_iss` threads would split compression, AES-GCM, checksums and indirect-block rewrites.
- **ZFS runs 206–210 `z_wr_iss` threads** (`zio_taskq_batch_pct=80`) at nice −19 and −20 (`spl_taskq_thread_priority=1`). The PostgreSQL processes run at nice 10, Redis at 0.
- **During the copy, Redis CPU fell 2.5–4 cores while requests were steady**; per request it fell 12–15% (§2.7). The 30–53 cores of system time then are unattributed.
- **edge-5 is healthy for Redis.**
  - MinIO's swapped pages are cold: 2 swap-ins/s, and PSI memory is 0.03%.
  - The 11-11 swap fix holds: swappiness is 10.
  - MinIO's daily memory swing moves MemAvailable between 58 and 119 GB without touching Redis.

#### 2.6 Slowlog and latency (`<scratch>/e/out/slowlog-*.txt`, `slow-cluster-edge-6.txt`, `ts-timeouts.txt`)

All 48 slowlogs are full (1,024 entries; threshold 10 ms, unchanged).

- **edge-6, from the 1,024 latest entries per node:**
  - **Rate:** 346–820 entries per node-hour; p50 14–15 ms, max 470 ms.
  - **Buckets:** 23.5k entries at 10–20 ms, 7.6k at 20–50, 1.3k at 50–100, 315 at 100–200 and 27 at 200–500 ms.
  - **Commands:** every type, with trivial commands as slow as scripts: TIME 19.9 ms mean, PTTL 19.8, GET 20.3, SET 19.6, against the client-score export script's 18.9.
  - **Host-wide stalls:** in the common 4,499 s window, 38.5% of seconds hold a slow entry. 11.0% of seconds (496) have 8 or more of 32 nodes stalled together, and they hold 83% of the entries. The most common gap between such seconds is 5 s (137 times), then 1, 6, 4 and 10 s: ZFS's `zfs_txg_timeout=5`.
- **edge-5:** only 6415 logs entries now, 379 per hour (p50 105 ms, 555 of 1,024 at 100 ms or more).
  - The `{provider_hard_exclusions}` SADD is 794 entries in 2.7 h at a mean of 118 ms (max 210 ms). That is ≈1% of 6415's time with the whole node blocked, as in CPUOPT4.
  - The other 15 nodes' newest entries are from before NUMA balancing went off on 11-01.
- **LATENCY LATEST, fork events:** latest 149 ms mean (max 569) on edge-6 and 104 ms (max 182) on edge-5. `latest_fork_usec` is 58–61 ms mean on ~620–660 MB nodes, half CPUOPT4's node size.
- **Client impact so far: none in errors.**
  - Redis pool timeouts are 0 on connect, api, proxy and taskworker in every 3-hour window from 11-10 12Z.
  - Contract-hole read and event errors stay at 0.01–0.25/s with no step.
  - The `/connect/control` limit's "unavailable" admits (Redis slower than its 250 ms timeout, model/connect_control_limit.go:168) rose from ≈0.5 to 1–1.5/s during the copy, out of 7–14k/s.
  - The stalls add tail latency on two thirds of the slots.

#### 2.7 Clock frequency (`<scratch>/e/out/freq-*.txt`, `posture-*.txt`, `core-evening.txt`)

| | edge-5 | edge-6 |
|---|---|---|
| CPUs under Redis main threads, 10 samples | 1,615 MHz mean; 78% of samples < 1.6 GHz | 2,355 MHz; 20% < 1.6 GHz |
| CPUs under Redis I/O threads | 1,618 MHz; 78% < 1.6 GHz | 2,613 MHz; 3% < 1.6 GHz |
| All CPUs | 1,656 MHz | 1,829 MHz |
| Policy | `schedutil`, floor 1.5 GHz, max 2.3 GHz, boost off | `schedutil`, floor 1.2 GHz, max 2.2 GHz, boost on (to 2.7–3.2 GHz) |

- `schedutil` sets a clock from each CPU's own load. A Redis thread uses 0.2–0.27 of a core, so on an otherwise idle core it runs near the floor *(inferred)*.
- **The natural experiment is edge-6's restore.** Between the 11-10 and 11-11/12 evenings (121k requests/s in each):
  - edge-6's all-in cost per request fell from 206 to 183 and 176 µs (−11 to −15%);
  - edge-5's fell from 162 to 160 and 157 µs (−1 to −3%).
  - The hourly series steps in the hour the copy began (cluster 195 → 171 µs; `<scratch>/e/out/ts-hourly.txt`). When the copy ended and the replay's lighter load followed, it moved back part way (≈175 µs).
  - Busier neighbours keep edge-6's cores at higher clocks and out of deep idle *(inferred)*.
- **Counted for §5.2** (`perf stat -e cycles,instructions,task-clock`, 30 s, counting only, 05:01–05:03Z; `<scratch>/prof/perf_stat_redis_*.txt`):

  | Node | Requests in 30 s | CPU | Effective clock (cycles ÷ task-clock) | IPC | Cycles per request | CPU µs per request |
  |---|---|---|---|---|---|---|
  | 6418, edge-5 (median) | 67.5k | 0.40 cores | 1.40 GHz | 0.69 | 249k | 178 |
  | 6386, edge-6 (median) | 60.7k | 0.36 cores | 2.31 GHz | 0.40 | 417k | 180 |

  edge-5's median node runs at 1.40 GHz while it runs, 61% of its 2.3 GHz maximum. edge-6's runs 65% faster but retires 42% fewer instructions per cycle (older cores, remote memory with balancing on, preemption), so both spend about the same CPU time per request. edge-6's clocks before the restore are still not measured.

### 3. What CPUOPT4's changes did

| CPUOPT4 item | Status at the deployed revisions | Measured now, against CPUOPT4 | Still worth doing? |
|---|---|---|---|
| E §5.1 / #12: no preflight PING by default | **Not shipped.** redis.go has no commits since d5cbc2a72; `preflight := true` at redis.go:597 and the PING at :625–626 (a00dfeb9a, 978dcba02). 39 call sites still preflight, against 38: the new one is `TakeExtenderWebRtcOffers` (model/extender_webrtc_relay_model.go:195) | PING 32.3k/s (11-04) → 13.3k/s (3-day mean), 14.4k/s (11-12 evening); fewer contract-hole calls (16.7k → 10.5k/s) | yes, §5.1: 1.0–1.7 cores |
| E §5.2 / #19: EVAL → EVALSHA | **Not shipped.** Contract-hole, reservation, recovery, session and connection-authority scripts still use `Eval` (e.g. model/subscription_contract_hole.go:111, :277; model/subscription_redis_admission.go:320) | EVAL 72.0k → 50.4k/s; known bodies ≥25–31 MB/s | yes, smaller: §5.7 |
| E §5.3: `kernel.numa_balancing=0` on edge-6 | **Not applied.** The sysctl is 1. The xops task is still edge-5 only (playbook-minio-clusters.yml:429; also playbook-dbs.yml:150 for the DB host) | migrations 41–44k → 18.1k/s, hint faults 191–201k → 76k/s (half the memory) | yes, §5.4: 0.15–0.25 cores |
| E §5.4 / #14: reuse a verified client-score page | **Not shipped.** The cursor and page cache are unchanged since d5cbc2a72 | proxy page-cache lookups 1,242 → 16/s; Redis output 136 → 32 MB/s (3-day); 6399's output 138 → 2.7–3.0 MB/s | **not at today's load**; revisit if proxy FindProviders2 (3.6/s now, 189/s on 11-03) returns |
| E §5.5 / #18: GETEX in the resident poll, prune inside the MULTI | **Not shipped.** GET then EXPIRE at model/network_client_resident_model.go:49–60; prunes outside the transaction at model/peer_model.go:869, :1029, :1251 (a00dfeb9a) | EXPIRE 20.6k → 12.7k/s, ZREMRANGEBYSCORE 34k → 16.1k/s | yes, §5.6: 0.25–0.45 cores |
| E §5.6: `{provider_hard_exclusions}` publish on change | **Not shipped.** DEL + full SADD + EXPIRE in one MULTI every run (model/egress_index.go:1121, :1136–1139 at 978dcba02) | SADD stall 152 → 118 ms mean, now every ~12 s; SMISMEMBER 340 → 35–48/s (0.02 cores) | step 1 for latency (§5.8); step 2 (local copy) no longer worth it |
| E §5.7: per-slot statistics | **Not done.** `cluster-slot-stats-enabled no` on 48 of 48; not in templates/redis/redis.conf.j2 | — | diagnostic (§5.9) |
| E §5.8: owner items | conntrack bypass not done (58k entries on edge-6); cpusets not done; `iommu=pt` still staged on edge-5; `io-threads 2` unchanged (redis.conf.j2:1334); SRSO unchanged | edge-6 new connections 308 → 57/s, so the conntrack saving shrank to ≈0.1 core *(inferred)* | §5.9 |
| #1: balance-aware refill hold (proxy 3c9f0fdc7, 11-04 16:06–17:05Z) | **Shipped**; proxy `insufficient_balance` refusals 0 since 11-05 12Z (Part B) | escrow hot node gone: proxy reservation operations 8.6k → 0.14k/s (exhaustion-proof, reserve-probe, recovery ≈0); 6383 at 9.8k commands/s and main 0.234 (from 62–70k and 0.87–0.97); proxy pool timeouts 109 → 0/s; new connections 382 → 78/s | done; predicted "6383 relieved", confirmed |
| #2: D1 in-process delivery | **On** since 11-04 (transport 14:32Z, forward ~17:49Z; Part A). `urnetwork_connect_exchange_local_transport_enabled` = 1 on beta and g1–g4 | No direct Redis effect predicted; the resident lookup is still made *(inferred)*. Not separable: the refill hold and the lease and timer roll landed 16:06–17:20Z the same day | — |
| #5: liveness-lease writer (101d59909) | **Shipped** in connect 7f0e0ad89 (11-04 16:30Z) | Redis lease writes stayed ~9–10k/s through the roll (`<scratch>/e/out/ts-1104.txt`): a connect-side CPU fix, as intended; 5.6k/s now with fewer clients | — |
| The no-replica posture after the 11-03 crash | unchanged: 48 masters, no replicas | edge-6 up since 11-03 04:23Z | decision still open (not CPU) |

- **Prediction against outcome:** CPUOPT4 predicted 2–3 cores for the PING change at 25–32k PINGs/s. At today's 13–14k/s it is worth 1.0–1.7 (§5.1). The other code items scale the same way with today's lower volume.
- **Confounders:** client count (−45%), the end of the out-of-balance loop, and frequent releases (connect every 1–2 days; each connect release resets the control-route sawtooth, §4). The restore confounds edge-6's per-request cost from 11-10 21:15Z.

### 4. New costs from recent releases

| Item | Code | Measured | Verdict |
|---|---|---|---|
| D42: the `:native_cap_v1` snapshot beside `:native_v1`, with capability facets | merge 3dcc7b138 (43dd4ec30); published like today's snapshot, alias included (model/network_client_score_native.go:40–47 at 978dcba02; key at model/network_client_score_capability.go:68) | When the first 4 D42 taskworkers started (01:30Z), keys rose 153.3M → 154.4M at 01:31–01:33Z and memory +0.15–0.25 GB. Against the same hours on 11-12 (difference in differences, `<scratch>/e/out/d42-*.txt`): SET +3.1k/s against +4.5k/s baseline, command cores +0.40 against +0.38, process cores +0.56 against +0.65, so no detectable write or CPU change; expired keys +1.33k against +0.51k/s (≈+0.8k/s); output unchanged | small: +0.7% keys, +0.6% memory. A daily +0.43 GB step at 01:00Z (also on 11-12) is not D42 |
| D42: S2 selector readers in FindProviders2 | api D42 since 02:30–02:40Z | select requests 0.006/s; HMGET and output unchanged (+1.3% and +0.4%, within baseline) | no reader cost yet. When apps send selector mode, capability pages are read like today's pages; E §5.4's reuse then applies to both |
| D42: post-quantum readiness flag | model/provider_egress_post_quantum_ready.go (2 Redis calls) | — | negligible *(inferred)* |
| D42/D43: Solana receiver sweep, uncredited-payment metrics, store notifications, S1 step 2, proxy window metrics | no Redis calls in d1ea65129..978dcba02 outside the client-score and post-quantum files | — | none |
| The `/connect/control` limit (0f981becf, owner 10-27) | one EVALSHA per request: TIME, GET, SET PX (model/connect_control_limit.go:182, run at :243 under a 250 ms timeout) | 7.2k admits/s at 04:40Z. EVALSHA minus admits is flat at 3.0–3.5k/s from 11-10 to 11-13, so the whole EVALSHA sawtooth is this limit: 1.8k/s after a connect release, 12–14k/s after 1–1.5 days. api Redis round trips climb with it, 9k → 24k/s (`<scratch>/e/out/ts-sawtooth.txt`) | not new, and the script is lean. Its volume follows the control-route creep (§5.5) |
| Net-escrow mirror refresh (taskworker) | `netEscrowSnapshotScript`, `Eval` (model/subscription_net_escrow.go:49, :108) | 5.8k/s steady since at least 11-01; 1,238 B bodies (7.2 MB/s) | not new; missing from CPUOPT4's EVALSHA list (§5.7) |
| Same-network escrow (8a4aa3ec0) | in a00dfeb9a, a151cee8d, 89be8b3b8 | reservation operations are below 11-03's (api 1.65k/s, taskworker 0.47k/s, proxy 0.14k/s) | no visible growth |
| Proxy feature-use marks (7d73b3e61) | at most one mark per proxy, feature and hour | 1.85/s (socks 1.84, wireguard 0.01) | negligible |
| Hosted-proxy client limit `cl-<network>-<minute>` | one counter per hosted-proxy create | creates are capped at 5–20/min per network | negligible *(inferred)* |
| §8.6 extenders, gossip, WebRTC relay | `TakeExtenderWebRtcOffers` adds a preflighting site; extender probe diagnostics skip preflight | relay offers 0/s; extender probe results 0.54/s; gossip Redis 0.005/s; 1,712 online extenders | negligible |
| Quality probe h3 + IP family (11-11/12) | taskworker egress probe | taskworker Redis round trips 4.3k/s (11-12) against 4.6k (11-03) | no growth |
| Other growth since 11-04 | — | MGET 5.9k → 9.0–9.5k/s; ZREVRANGEBYSCORE 99 → 594–649/s; both under 0.03 cores | not attributed; negligible |
| edge-6's backup restore (not a release; a new co-tenant) | xops `bringyour-pg-backup.sh` | §2.5–2.6: 6–7 host cores now, 30–53 cores of system time during the copy, host-wide Redis stalls | §5.3 |

### 5. Ranked opportunities

#### 5.1 No preflight PING by default (CPUOPT4 #12, E §5.1; unchanged in substance)

- **Change:**
  - In `redisWithClient` (redis.go:594–627 at a00dfeb9a), skip the PING unless a caller asks for it: rename the option to an opt-in `OptRedisPreflight()`, defaulting to no PING.
  - First among the 39 sites in `<scratch>/e/out/preflight-callers-a00dfeb9a.txt` are the contract-hole reads and events: model/subscription_contract_hole.go:109, :276, :371, :407 and subscription_contract_hole_readiness.go:110, :122, :133.
  - Then `TakeClientCloseRequests`, clock, egress tally, resident retirement, onboarding, network health, degradation, metrics cache, handler-cache renew and `TakeExtenderWebRtcOffers`.
- **Evidence:**
  - PING runs at 13.3k/s on the 3-day mean (11.6% of requests) and 14.4k/s on the 11-12 evening.
  - Contract-hole calls explain 73–76% of it; subscriber health checks explain at most 105/s (§2.4).
  - Each PING is its own request on a random node. The comment at redis.go:620–623 already says that a separate PING "doubles a one-command round trip and cannot guarantee the next command succeeds".
- **Saving:** 13–15k fewer requests/s. At 70–120 µs per request (I/O-thread 52–84 µs plus main thread outside commands 16–37 µs, from the regressions), that is:
  - 1.0–1.7 cores at the evening peak and 0.9–1.6 on the daily mean, 5–8% of Redis;
  - client side, one round trip off each contract-hole read on the packet path, and 13–15k fewer pool turns/s, mostly in connect.
- **Effort and risk:** low. An option rename at 39 sites. A connection error now surfaces from the first real command, through the same error path; these pools never replay a command.
- **Why no timing bound moves:** each callback keeps its own deadline (`contractHoleRedisTimeout` 1 s and the others). The PING only shared that budget.
- **Verification:**
  - **Metrics:** `redis_commands_total{cmd="ping"}` falls by 13–15k/s, to near 0, and `redis_total_reads_processed` by the same.
  - **Errors:** contract-hole `read_error`/`event_error` and connect allowance outcomes stay flat.
  - **Tests:** a deterministic test that a deadline-pool call issues no PING by default and one with the opt-in.
- **Owner decision:** none.

#### 5.2 A performance clock policy on the Redis hosts (new)

- **Change:** on edge-5 and edge-6, set `scaling_governor=performance`, or raise `scaling_min_freq` to the maximum, as a default-on xops task beside the NUMA sysctl task. On edge-5, consider boost as a second step. C-states stay as they are.
- **Evidence:**
  - edge-5's Redis threads run at 1.62 GHz, and 78% of samples are below 1.6 GHz on a 2.3 GHz part with a 1.5 GHz floor and boost off.
  - When edge-6's neighbours loaded its cores, its cost per request fell 11–15% at equal requests, while edge-5's fell 1–3% (§2.7).
- **Counted:** a median edge-5 node runs at an effective 1.40 GHz and 249k cycles per request (§2.7). At edge-5's 2.3 GHz maximum the same cycles would take about 108 µs instead of 178, if the work scales with the clock; memory stalls scale less, so the 15–25% below is the conservative end *(inferred)*.
- **Root cause:** host policy, not code. `schedutil` clocks a core by its own load, and each host runs 32–64 lightly loaded Redis threads (0.2–0.27 of a core each) on otherwise idle cores *(inferred)*.
- **Saving *(inferred)*:**
  - edge-5: 15–25% of 6.6–7.0 cores, that is 1.0–1.7 cores, now.
  - edge-6: 11–15% of its pre-restore 16.0–16.6 evening cores, that is 1.8–2.5 cores, whenever no restore runs. During a restore its clocks are already high.
  - Command latency falls with it.
- **Effort and risk:** low. A runtime sysfs write, reverted by writing `schedutil`. The cost is power and heat on two hosts.
- **Why no timing bound moves:** no timeout or cadence changes; work only runs faster.
- **Verification:**
  1. The 30 s `perf stat -e cycles,instructions,task-clock` of one median Redis process per host (§2.7), repeated after the change: cycles per request should stay flat while time per request falls.
  2. Then one host for a day, compared on µs per request at matched requests with the other host as control.
- **Owner decision:** yes, the power trade.

#### 5.3 Take edge-6's restore off Redis's CPUs (new)

- **Change, cheapest first:**
  1. **Clone at the base's record size:** `zfs clone -o recordsize=8K` in bringyour-pg-backup.sh:796, so files the replay creates get the base's 8 KB records (the design rationale at playbook-db-archive.yml:300–312).
  2. **At edge-6's next planned reboot, ZFS write threads at normal priority** (`spl_taskq_thread_priority=0`), and fewer of them (`zio_taskq_batch_pct`, for example 25 instead of 80). Both are module parameters.
  3. **Or run the restore and dump on a host without Redis.**
- **Evidence (§2.5–2.6):**
  - Host-wide stalls on a 5 s cadence: 11% of seconds with 8 or more of 32 nodes stalled.
  - Slow entries 25–170× the pre-restore rate.
  - edge-6 main threads take 2,017 involuntary switches/s, against 75 on edge-5.
  - 206–210 `z_wr_iss` threads at nice −19/−20 burn 4.7 cores, plus 1.0 in `dp_*` and 0.3 in `z_rd_*`, for 60–200 MB/s of replay writes. During the base backup copy, edge-6 ran 30–53 cores of system time for 18 h.
- **Root cause:** co-location and ZFS write-thread priority; the clone inherits `recordsize=1M` (bringyour-pg-backup.sh:796).
- **Saving:**
  - **Redis cores:** none. Redis may cost more per request on an idle edge-6 (§5.2).
  - **Host:** 6–7 edge-6 cores while it replays, and 30–53 during a copy (the cause of that is not determined).
  - **Latency:** the stalls are removed: 10–470 ms on two thirds of the slots, about every 5 s.
- **Effort and risk:** low for step 1 (applies at the next run), medium for step 2 (a reboot of 32 nodes without replicas, so do it in a planned window), and higher for step 3 (a host).
- **Why no timing bound moves:** no product bound changes; Redis keeps its timeouts, and only the stalls go.
- **Verification:**
  - edge-6 slow entries per node-hour back near the pre-restore 20–30.
  - The share of seconds with 8 or more nodes stalled near 0.
  - Main-thread involuntary switches near edge-5's.
  - The restore's duration unchanged or shorter.
- **Owner decision:** yes. The restore is part of the backup redesign (CPUOPT4 #4) and repeats with every run. Step 2 needs a reboot window.

#### 5.4 `kernel.numa_balancing=0` on edge-6 (CPUOPT4 E §5.3; not applied)

- **Change:** apply xops's task (playbook-minio-clusters.yml:429, edge-5 only) to every `redis-nodes` host: `sysctl -w kernel.numa_balancing=0`, live.
- **Evidence:**
  - edge-6 still hint-faults 76k pages/s and migrates 18.1k/s, about 74 MB/s copied.
  - The faults and migrations run in Redis's own threads, and the PostgreSQL copy's 34 GB of shared memory is balanced as well.
  - edge-5 has had the setting since 11-01, and its nodes log no slow entries except 6415.
- **Saving:** 0.15–0.25 cores, CPUOPT4's 0.4–0.5 scaled by migrations, plus one source of stalls *(inferred)*.
- **Effort and risk:** low. A runtime sysctl, reverted by writing 1.
- **Why no timing bound moves:** memory placement only.
- **Verification:** `numa_pages_migrated` at 0; µs per request at equal load not higher; edge-6 slow entries lower (separate this from §5.3 by doing it first, or on a day without a restore).
- **Owner decision:** yes, as in CPUOPT4.

#### 5.5 Find the `/connect/control` creep (cross-part, with Part C)

- **Change:** none in Redis. The per-client limit stays as decided (owner, 10-27).
  - The admitted control rate climbs from 1.8k to 12–14k/s over a connect cycle and resets at each connect release: the open alert `control-create-contract-uptime-creep`.
  - Finding why clients' `create_contract` frames climb with connection age would remove the excess requests at the source.
- **Evidence:** the EVALSHA sawtooth is entirely the limit script (§4). The api's Redis round trips climb in step, from 9k to 24k/s.
- **Saving *(inferred)*:**
  - At 140–225 µs per limit call (a 70–120 µs request plus a 72–106 µs EVALSHA), removing 10–12k calls/s, plus ~3k/s of other api round trips, saves 1.7–2.7 Redis cores at the top of a 1.5-day cycle.
  - Averaged over a cycle, 0.8–1.4 cores. It depends on how long connect runs between releases.
  - api CPU is Part C's.
- **Effort and risk:** an investigation; the fix lives in connect or the sdk.
- **Why no timing bound moves:** the limit and its timeout stay the same; only the excess requests go.
- **Verification:** the admitted rate flat across a connect cycle, with providers, clients and contract outcomes normal.
- **Owner decision:** none to investigate.

#### 5.6 One round trip instead of two (CPUOPT4 #18, E §5.5; unchanged)

- **Change:**
  - `GetResidentForClient` uses `GETEX key EX ttl` (model/network_client_resident_model.go:49–60 at a00dfeb9a).
  - `AddNetworkProxyPeer` and the client-peer paths queue their prune inside their transaction (model/peer_model.go:1251, :869, :1029).
- **Saving:** CPUOPT4's 6–7k requests/s scaled by connect's Redis round trips (×0.53–0.59), that is 3.2–4.1k requests/s, or 0.25–0.45 cores, plus connect CPU.
- **Effort and risk:** low. An undecodable resident record also gets its TTL extended, and `NominateResident` replaces it.
- **Why no timing bound moves:** the resident poll (75 s against a 300 s TTL) and the heartbeat cadences stay.
- **Verification:** EXPIRE falls by ≈2–3k/s; resident-loss and peer-count metrics unchanged; tests that the TTL is renewed on a read and that the prune runs once per transaction.
- **Owner decision:** none.

#### 5.7 EVAL → EVALSHA for the hot scripts (CPUOPT4 #19, E §5.2; one script added)

- **Change:**
  - Use `redis.NewScript(...).Run` (EVALSHA with a NOSCRIPT fallback) for CPUOPT4's list:
    - reservation, model/subscription_redis_admission.go:191, :320;
    - contract-hole read and event, model/subscription_contract_hole.go:56, :185;
    - recovery page, model/subscription_redis_recovery.go:116;
    - grant probe;
    - verify-egress leases;
    - `sessionLuaCommon`;
    - `connectionAuthorityPublishLua`.
  - Add the taskworker's net-escrow refresh: `netEscrowSnapshotScript`, model/subscription_net_escrow.go:49, :108 at 978dcba02.
  - Pipelines re-run only NOSCRIPT failures with `Eval`, as verify_egress_refresh_model.go already does.
- **Saving:** CPUOPT4's 0.5–0.8 cores scaled by body bytes (≥25–31 MB/s now, against ≥55–60), that is 0.25–0.4 cores, and 25–31 MB/s less inbound.
- **Effort and risk:** low. A NOSCRIPT after a restart costs one extra round trip per node and script, once. A pipeline must treat NOSCRIPT as "retry with EVAL", never as a result.
- **Why no timing bound moves:** the same script runs under the same deadline.
- **Verification:** EVAL falls and EVALSHA rises by the same count; `redis_net_input_bytes_total` falls by 25–31 MB/s; a test that a NOSCRIPT reply is retried with the body.
- **Owner decision:** none.

#### 5.8 `{provider_hard_exclusions}`: publish only on change (CPUOPT4 E §5.6, step 1)

- **Change:**
  - In `publishProviderHardExclusions` (model/egress_index.go:1121–1143 at 978dcba02), keep a digest of the member set beside the key.
  - When it is unchanged, only re-arm the EXPIRE. When it has changed, write a staging key under the same hash tag in chunks and RENAME it into place, so readers still see one atomic publication.
- **Evidence:** 794 SADD slow entries in 2.7 h at a mean of 118 ms, about one every 12 s, each blocking the whole of 6415. That is ≈1% of the node's time, and 1/48 of the slots wait up to ~120 ms.
- **Saving:** ≈0.01 cores; the gain is latency.
- **Effort and risk:** low.
- **Why no timing bound moves:** the publisher's cadence (5 s after each run) and the TTL stay; only identical rewrites are skipped.
- **Verification:** 6415's SADD slow entries near 0; exclusion and coverage outcomes unchanged; a test that an unchanged set is not rewritten and a changed one replaces atomically.
- **Owner decision:** none. Step 2, the local copy, is no longer worth it: SMISMEMBER is 35–48/s.

#### 5.9 Diagnostics and owner experiments

- **Per-slot statistics:** `cluster-slot-stats-enabled yes` in templates/redis/redis.conf.j2. `CONFIG SET` is a production write and needs the owner's go. It names a hot slot without MONITOR; its cost is expected under 1%; measure it on one node first.
- **conntrack bypass on edge-6:** its value fell with connection churn (57 new connections/s on edge-6, against 308). ≈0.1 cores *(inferred)*; a firewall change, so medium risk.
- **`iommu=pt` on edge-5:** still staged; it needs a reboot of 16 nodes without replicas. ≈0.6% of edge-5 Redis by CPUOPT4's profile, so do it only at a planned reboot.
- **Per-NUMA-node cpusets on edge-6:** CPUOPT4 measured newidle balancing at ≈8.5% of a median node. Re-measure it with a profile under today's load before deciding.
- **An io-threads 1 trial:**
  - CPUOPT4 advised against it because 6383's main thread was saturated. Today the busiest node's main and I/O threads together use ≈0.5 of a core.
  - One thread per node would drop a cross-thread handoff and about one wake per request (1.8–2.2 voluntary switches per request now). Worth perhaps ≤10% of Redis *(inferred)*.
  - It needs a node restart without replicas, and a surge like 11-04's would saturate a node sooner. Trial it on one node at a planned restart only, with a revert threshold of main thread above 0.6 cores.
- **SRSO:** unchanged; a security call.

### 6. Considered and rejected

- **Raising `slowlog-log-slower-than` or `latency-monitor-threshold`** to quiet edge-6: hides the detection instead of fixing the stall (owner rule).
- **Raising `connectControlLimitRedisTimeout` (250 ms)** so stalls stop admitting without a decision: loosens the api's wait bound. Fix the stalls (§5.3).
- **Caching the `/connect/control` decision per process, or skipping it for providers:** multiplies a client's allowed rate against the owner's per-client authoritative limit (10-27).
- **Lengthening the contract-hole, resident (75 s), liveness-lease (25 s) or `UpdateClientLocations` (5 s after each run) cadences:** detection and revocation bounds, as in CPUOPT4.
- **`OptRedisKeepLateReplies` or larger pool timeouts and per-node caps:** each lets a call end after its deadline (CPUOPT4 §6).
- **Raising `zfs_txg_timeout`** to make the stalls rarer: bigger bursts, and a backup-durability trade. A poor trade.
- **`idle=poll` or disabling deep C-states:** a large power cost for a gain the clock policy (§5.2) should mostly deliver. Not before §5.2 is measured.
- **Turning off client eviction (`maxmemory-clients`):** a memory-safety bound on 48 nodes without replicas, as in CPUOPT4.
- **Client-score page reuse (CPUOPT4 §5.4) now:** not rejected under the rule, but deferred. It saves ≈0 at 16 page lookups/s.

### 7. Raw outputs

All under `<scratch>/e/`.

- **Scripts (`scripts/`):**
  - Mimir: `winstats.py`, `hourly.py`, `beforeafter.py`, `cmdcompare.py`, `pernode.py`, `nodemix.py`, `regress.py`, with their expression and window files (`exprs-*.txt`, `windows-*.txt`).
  - Hosts: `snap.sh`, `summ.py`, `pubsub-clients.py`, `host-posture.sh`, `freq-sample.sh`, `zfs-detail.sh`.
  - Local: `run-script.sh` (masks IPv4), `snapdelta.py`, `slowlog-analyze.py`, `slow-cluster.py`, `lua-fingerprints.py`, `preflight-callers.py`.
  - The pseudonym salt (`salt.txt`) stays local.
- **Mimir (`out/`):**
  - totals: `core-evening.txt`, `core-long.txt`, `norm-clients.txt`, `ts-hourly.txt`;
  - commands: `cmd-compare.txt`;
  - per node: `pernode-1112E.txt`, `pernode-1113-0004.txt`, `nodemix-1113.txt`;
  - hosts: `ts-host-edge6.txt`, `ts-host-edge5.txt`;
  - D42: `ts-d42.txt`, `d42-before-after.txt`, `d42-baseline-prevday.txt`;
  - the 11-04 deploys: `ts-1104.txt`;
  - the control-limit sawtooth: `ts-sawtooth.txt`;
  - net escrow: `ts-netescrow.txt`;
  - client errors: `ts-timeouts.txt`;
  - regressions: `regress-pre.txt`, `regress-replay.txt`.
- **Hosts (`out/`):**
  - snapshots `snapA-*`, `snapB-*`, `snapC-*`, with deltas `delta-AB-edge-6.txt` and `delta-AC-*.txt`;
  - `summ-edge-*.txt` (config, slot map, slowlog names and fingerprints, latency, client summaries with pseudonymous sources);
  - `slowlog-edge-*.txt`, `slow-cluster-edge-6.txt`, `pubsub-edge-*.txt`, `posture-edge-*.txt`, `freq-edge-*.txt`, `zfs-edge-6.txt`, `zfs-run-origin.txt`.
- **Code (`out/`):** `preflight-callers-{a00dfeb9a,a151cee8d,978dcba02,89be8b3b8}.txt`, `lua-fp-*.tsv`, `metric-names-redis.txt`.
- **Profiles worth taking next (not taken this round):**
  - (`perf stat` of one median Redis process per host was taken, §2.7);
  - a kernel profile of the `z_wr_iss` threads on edge-6 during replay (§5.3);
  - one median edge-6 Redis node profile, to compare newidle balancing with CPUOPT4's 6386 (§5.9).
