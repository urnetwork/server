# CPUOPT6 — fleet CPU after CPUOPT5 (2026-11-15)

CPUOPT5 (2026-11-13) ranked the fleet's CPU costs after CPUOPT4. The owner answered
its nine decisions the same day, and the services that carry those answers are now
deployed on Main. On 11-15 the owner asked: "Now that the latest services are deployed
to main that incorporate the findings of server/CPUOPT5.md, perform another analysis
of opportunities for CPU efficiency improvement using evidence from main and write
results to server/CPUOPT6.md".

This document measures the fleet again, checks what CPUOPT5's changes did against its
predictions, and ranks what is left or new. As in CPUOPT5, read-only investigations
wrote five parts, one per CPU domain. They measured Main on 11-15 between 10:00 and
11:20Z.

| Part | Domain | Hosts |
|---|---|---|
| A | Services edges and connect | edge-0, edge-1, edge-3, edge-4 |
| B | Proxy hosts and the proxy service | crisp, fireside |
| C | api, alt and taskworker | the services edges, and alt on crisp and fireside |
| D | PostgreSQL and its PgBouncer layer | edge-2 |
| E | The Redis cluster, and edge-5's co-tenants | edge-5, edge-6 |

**Builds during the measurements** (Mimir `urnetwork_build_info`, about 11:20Z):

| Service | Build | Instances | Server revision |
|---|---|---|---|
| api | `2026.11.15-outerwerld+1099362010` | 20 | 11d5a8e78, rolled about 09:57Z |
| alt | `2026.11.15-outerwerld+1099363220` | 2 | 11d5a8e78 |
| taskworker | `2026.11.15-outerwerld+1099357100` | 8 | 11d5a8e78, about 09:49Z |
| connect | `2026.11.14-outerwerld+1098366310` | 20 | aa7142599 (D47), rolled 11-14 07:08–07:38Z |
| mcp | `2026.11.14-outerwerld+1098368160` | 20 | aa7142599 (D47) |
| gossip | `2026.11.13-outerwerld+1098358910` | 4 | aa7142599 (D47) |
| proxy | `2026.11.14-outerwerld+1098770070` | 20 | f43488da1 (D49p), every process since 11-14 18:38:47Z |

- **Config:** `2026.11.14-outerwerld+1099044420` (config a5f2c85, "crypto_pay.yml:
  turn on TAO, Bitcoin, ETH and native SOL"), published to the hosts at 01:11Z on
  11-15. **Every service restarted then, one block at a time about 20 s apart, at the
  same builds.** This is the 01:11–01:14Z restart that every part saw.
- **Main's migration head:** 977, applied at 09:41:34Z.
- **The api, alt and taskworker rolled during the morning,** from D50 (server
  6c0a7ffb6: api `+1098824240`, alt `+1098822500`, taskworker `+1098815950`) to
  11d5a8e78 at 09:48–09:57Z. connect, mcp, gossip and the proxy did not roll. The diff
  (the gift's Gamma rail, competition tooling, the points leaderboard, contract close
  reports, the proxy-trim tool) touches no hot path measured here (C, D, E). Part C's
  Mimir windows end at 09:45Z on D50; every perf capture is of the new builds.
- **Release names used below:**
  - D46: the verified-chain cache (D4(b)) for the taskworker, 11-13 22:47–23:01Z, and
    the JWT claims cache (D4(a)) for the api and alt, from 399a011b1, 11-14
    00:20–00:31Z;
  - D47: connect, mcp and gossip from aa7142599, and PgBouncer stage 1 on api beta
    from 11-14 07:58Z;
  - D47p: the proxy's lazy device open (4daaba07f), 11-14 10:44–13:18Z;
  - D49p: the proxy's 10-minute close of devices that carried no traffic (f43488da1),
    g6 at 17:57Z and every block by 18:38:47Z on 11-14;
  - D50: the api, alt and taskworker from 6c0a7ffb6, 11-14 19:14–20:22Z.

**Evidence:**
- Mimir, 11-12 00Z to 11-15 about 11Z (from 11-06 for Part C's fits and 11-07 for
  Part D's history);
- perf captures of 30 s or less (20 s for nginx, 15 s for MinIO's cgroup), one host at
  a time, only while the SN25 validator read finalized or applied with at least 15
  minutes to its next boundary; data files deleted on the hosts, no perf process left;
- `perf stat` counting (no sampling) of two Redis processes, 30 s each;
- host censuses from `/proc`; Redis INFO, SLOWLOG, LATENCY and CLUSTER SLOT-STATS
  deltas, and one 10 s keyspace-event sample on one node, counted by family only;
  PgBouncer SHOW STATS, POOLS, LISTS and SERVERS;
- 20 `pg_stat_statements` snapshots and catalog reads through `support db query`
  (read-only, one statement at a time, 30 s ceiling; no counter was reset);
- log line counts by class on edge-1, and the taskworker's lines for one task through
  Loki, ids masked;
- code read at the deployed revisions.

Root access followed the owner's 11-15 allowance (`xops/bin/by-pass` piped into
`sudo -S`; the password was never printed or stored). No production state was
changed, and the permission system refused nothing. Two capture incidents were
cleaned up (A §2). Part C's two script files are still in `/tmp` on edge-0, edge-1 and
edge-4, and should be removed.

**When it was measured:**
- **Live captures:** 11-15, a Sunday, 10:00–11:20Z, with 276–291k connected clients
  and 40–46k live hosted devices. The sweeps' pulses had merged from about 09:20Z, so
  no quiet trough fell in a validator-idle window: every live capture is at sweep load.
  The quiet baselines are CPUOPT5's captures of the same processes and Mimir.
- **Matched windows from Mimir:** CPUOPT5's 11-12 18–22Z evening and its 3-day mean to
  11-13 04Z, against the 11-14 18–22Z evening and 11-15 02–10Z.
- **connect was 9–10 hours old** during the captures, because the config release
  restarted it.

Section numbers inside a part are local to it: A §3.4 is Part A's §3.4, and A #1 is
Part A's first ranked opportunity. Every part has the same eight sections: Summary,
Method, Where the CPU goes now, What CPUOPT5's items did, New costs, Ranked
opportunities, Considered and rejected, and Noticed, not CPU. CPUOPT5's sections are
cited as "CPUOPT5 A §5.1", and the ranks of this document's fleet table as "fleet #1".
Raw outputs and scripts are cited as `<scratch>/…` and stay in the analysis workspace.
A g-process is a connect process in blocks g1–g4.

**Owner rule (binding):** "We can't sacrifice correctness of performance timings for
the cpu savings." Nothing proposed here loosens a timeout, deadline, keepalive, probe,
detection or revocation bound. Each part lists what it rejected under that rule. Owner
decisions are respected: no new off-by-default settings, the quality probe's UDP gate
stays, one contract size, the proxy client limits, and no user-agent blocks, ASN caps
or new-account proxy limits (11-14). Proposals that touch code follow
connect/CODESTYLE.md: tunables are settings fields with one default, every fix carries
a deterministic regression test, and migrations are additive and apply before the
code that needs them.

## Summary

### What changed since CPUOPT5

**The fleet rose from about 213 to about 331 busy cores** (the sum of non-idle modes:
CPUOPT5's 3-day mean to 11-13 04Z against 11-15 02–10Z). On CPUOPT5's "1 − idle"
basis that is 239 → about 357, and the bot era since 11-14 15:20Z reads the same
(about 359). *(The fleet totals are a Mimir check by the writer, made to put the
parts' host figures on one basis; the parts' own figures below sum to about the same.)*

| Host group | CPUOPT5 | Now | Part |
|---|---|---|---|
| Services edges (312 CPUs) | 140.3 at the 11-12 evening | 199.2 on 11-15 02–10Z (troughs 185.4, peaks 210.4); 210.7 at the 11-14 evening | A §3.1 |
| Proxy hosts (48 CPUs) | 7.0 on 11-13 00–12Z | 40.7 since D49p (between sweeps 20.2, in sweeps 47.4) | B §3.1 |
| edge-2 (96 CPUs) | 29.5 (3 days) | 42.6 since 11-14 14Z; 42.8 on 11-15 02–09Z | D §3.1 |
| edge-5 (96 CPUs) | Redis 6.63 (3 days) | 20.8 busy at 10:25–11:00Z: Redis 12.75, MinIO 5.7–5.9 | E §3.1, §3.6 |
| edge-6 (128 CPUs) | Redis 13.8 (3 days) | about 29 busy (writer's Mimir check; Part E's 10 s reading: 21%): Redis 25.7 | E §3.1, §3.5 |
| **Fleet** | **213 (239 on "1 − idle")** | **about 331 (about 357)** | |

(The parts' host figures are all close to the non-idle-modes basis. CPUOPT5's "1 − idle"
basis reads up to about 5 cores higher per host.)

**Nearly all of the rise is load, and two bounded resources it filled:**
- **The hosted-proxy bot sweeps.** They began on 11-13 about 20Z and have run without
  a break since a step at 11-14 13:50Z: a sweep about every 38 minutes (median; D §3.1),
  with sweep phases 77% of the time since 11-14 15:20Z (C §3.2; Part B counts the proxy
  hosts at 44 or more of 48 cores busy in 68% of 5-minute samples since D49p).
  - Live hosted devices 13.6–13.7k → 41–48k;
  - connect's clients 147–150k → 278–291k, and exchange connections 224–276k →
    533–712k;
  - commits 23.9k → 41.6k/s, and contracts about 670 → 1,300–1,440 a second (A §3.1,
    B §3.1, D §1).
- **Two bounded resources tipped over:**
  - the proxy's Go memory limit: the collector went from 0.6 to 12 cycles a minute, 58–60%
    of a proxy process (B §3.2);
  - connect's 1,024-message key-event merge: every g-process has resubscribed every 7–9 s
    since 11-14 16Z, the key-event resubscribe storm (A §5.1, E §5.1).
- **No release added a large cost.** The storm is a latent defect in code that did not
  change (`redis.go` not since 10-28; A §4, E §2), tipped over by the population. The largest release-related costs
  are the weekly award's re-read after a taskworker restart (D §3.3) and D49p's doubled
  deactivation checks, 0.49–0.68 backends (D §5).

**The owner's 11-13 answers went live:** the extender shadow retired and sliced; both D4
caches; the performance governor on both Redis hosts and `numa_balancing=0` on edge-6;
per-slot statistics; three of O2's five partial rebuilds; PgBouncer stages 0 and 1; the
stuck-provider signal on every api block; the GOMAXPROCS A/B (inconclusive, ended by the
owner). On 11-14 the owner added the proxy's lazy open (D47p) and its 10-minute close of
devices that carried no traffic (D49p).

**Per unit of work, the services cost what they did, or less:**

| Domain | CPUOPT5 | Now | Why |
|---|---|---|---|
| connect, at matched work | — | 117.7 cores observed against 125 by CPUOPT5's model for 11-15's work | no dearer (A §3.3) |
| connect, per exchange frame | 36.1 µs (11-12 evening) | 43.5 µs (11-14 evening); 49.3 µs (11-15 02–10Z) | the storm's resyncs, and the GC and timers of a population with 2.9× the exchange connections and 17% more frames (A §3.1, §3.3) |
| connect, per 1,000 exchange connections | 0.10 cores | 0.04–0.07 | the sweep connections are mostly idle (A §3.3) |
| nginx, per 1k H1+ writes/s | 0.22 cores | 0.19 | one TLS record per write (A §3.2, §3.5) |
| api, per 1k requests/s | 1.09–1.30 (11-07..11-11) | 1.2–1.8 | three connect restarts in 26 h kept cheap replays down; real creates rose 50–90% (C §3.1) |
| proxy, per window client | 0.11 mcores (11-13's first sweeps) | 0.31 in sweeps, 0.14 between | GC at the memory limit (B §3.1) |
| PostgreSQL, per 1k commits/s | 1.24 cores | 1.02 | the floor spread over more commits; per query PgBouncer 64 µs (56), nginx 45 µs (42), backends 266 µs (257) (D §1, §3.2) |
| PostgreSQL, execution per commit | 0.324 ms (0.275 pin-free) | 0.309 ms (0.258 without the award's reads) | (D §3.3) |
| Redis, all-in per request | 178 µs | 111–117 µs | the performance governor, and three times the load per node (E §3.1–3.2) |

### Where the CPU goes now

The fleet between sweeps and at a sweep's top. Each part classified sweeps its own way
(A by quarters of exchange connections, B by the proxy hosts' busy CPUs, C by proxy
cores); Part C's phases are in brackets where they differ.

| Host group | Consumer | Quiet | Sweep | Source |
|---|---|---|---|---|
| Services edges | host busy | 185.4 | 210.4 | A §3.1: the lowest and highest quarters of exchange connections, 11-15 02–10Z |
| | connect | 109.8 (C: 108.8) | 123.3 (C: 119.9) | A §3.1, C §3.2 |
| | nginx (lb) | — | 44.5 | A §3.2, census at the sweep peak |
| | api | 15.3 (C: 15.4) | 18.6 (C: 17.3) | A §3.1, C §3.2 |
| | taskworker | 12.7 (C: 12.9) | 13.3 (C: 13.1) | A §3.1, C §3.2 |
| | logging and observability | — | 13.6 | A §3.2 |
| | mcp and gossip | — | 0.8 | A §3.2 |
| Proxy hosts | host busy (of 48) | 20.2 | 47.4 | B §3.1: under 30, and 44 or more, of 48 busy since D49p |
| | proxy | 17.1 (C: 20.3) | 44.9 (C: 43.9) | B §3.1, C §3.2 |
| | alt | 0.76 | 0.91 | C §3.2 |
| edge-2 | host busy | 36.2 | 42.2 | C §3.2; D §3.1: 42.6 mean, +3–9 at a clean sweep's peak |
| | backends via PgBouncer, and direct | — | 29.7 and 3.1 | D §3.2, census at 10:17Z, during the award's read |
| | PgBouncer, and the nginx stream hop | — | 7.1 and 5.1 | D §3.2 |
| Redis hosts | Redis, 48 nodes | 38 | 38 | E §3.4: sweep against quiet 1.003× in cores; 38.1 on 11-15 06–10Z |
| | network softirq, edge-5 and edge-6 | 4.3 and 6.6 | the same | E §3.1 |
| | MinIO (edge-5) | 5.7–5.9 | 5.7–5.9 | E §3.6 (snapshots; it follows its own daily PUT cycle) |
| **Fleet** | busy, roughly | **about 290** | **about 350** | the groups' figures summed, with edge-5 (20.8) and edge-6 (about 29) at their means |

- **connect (A):** 110–123 cores, from 73.5–77.4.
  - Inside a g-process: H1 transports 18–19%, the scheduler 16–24%, exchange socket
    trees 12–16%, go-redis 12–16%, and resync-driven work 8.6–15.2% (1.5–2.0% before).
  - The timer heap's `adjust` is back at 2.2–4.0% (0.95–1.15%), and the GC takes about
    4 cores across connect, at the 2-minute floor on 37–63 GB heaps.
  - A stable subscriber (a beta process) spends 34.6% receiving the key-event stream.
- **nginx (A):** 44.5 cores, from 26.7. It relays each 0.8–0.9 KB H1+ write as one TLS
  record: 66–67% kernel, TLS record work 33–38%, handshakes under 1.5%.
- **The proxy (B):** 44.9 cores in sweeps against 4.9 on 11-13's quiet morning. GC is
  58–60% of a process (marking 21–26%, assists 26–31%); a device opened costs about
  2.6 CPU-seconds, 1.5 of them GC.
- **api (C):** 16–24 cores, 1.49 ms per average request: `/connect/control` 30%
  (`CreateContract` 16%, authentication 9%), the HTTP server and gates 13%, the
  scheduler 21–27%, GC about 9%. JWT fell from 14.6% to about 5–6% with D4(a).
- **taskworker (C):** 12–14 cores. x509 fell from 25.6% to 6.5–8.0% of a probe process
  with D4(b); what is left is Go's stock TLS. The score export's gob encoding is 1.08
  cores; the unique-exit sweep is cut at its 10-minute bound every run.
- **alt (C):** 0.83–0.89 cores at the same 62–67 req/s as before, because its hosts are
  saturated in sweeps (5.2 → 12–14 ms of CPU a request).
- **PostgreSQL (D):** backends 29.7 + 3.1 cores; the two hops (PgBouncer and the nginx
  stream) 12.2 cores, 23% of busy. Pool pressure is idle-in-transaction time, 252–289 s/s
  against 15–17 s/s active. The contract lifecycle is 42% of execution.
- **Redis (E):** 36.0–38.1 cores at 311–343k requests/s, 17.4 of the cores the storm's.
  edge-6's NIC interrupts pile softirq and the Redis I/O threads onto NUMA node 2 (CPU
  pressure 34.6%, against 3.6% on edge-5).
- **MinIO on edge-5 (E):** 5–7 cores, 88% kernel; 73% of its sampled time is `mkdir` in
  one flat directory of 2.6–2.85M extents per drive.

### What CPUOPT5's changes did

Of CPUOPT5's 21 ranked items, four shipped and met or beat their predictions (#1, #5,
#6, #20); one is deployed and waits for the next backup run (#15); two are half done (#2,
#8) and one three-fifths (#10); one ran and was inconclusive (#7); the owner did not
choose three (#3, #11, #13); and nine are not done.

| # | CPUOPT5 item | Predicted | Status | Measured | Verdict |
|---|---|---|---|---|---|
| 1 | End the extender shadow loop | 3.5–6.5 (1.5 taskworker, 2–5 edge-2) | Shipped: epoch 4's live shadow retired (3cddf5af7, b4374ac08; taskworker 11-13 09:22–09:36Z); hour-long slices, counted timeouts, the stale-bound raise (95306eb79, deployed in f9589fc0e 15:37–15:51Z) | No 30-minute pin since 11-13 08:00Z; `StLedgerV2UsageStep` 0 in flight since 11-13 10Z. The taskworker's share can't be separated from the probe's swing (C §4.1, D §4) | **Met** |
| 2 | Stuck-provider signal, then recycle, then bake in | about 3.5–4 averaged, up to 9 at a sawtooth's top *(inferred)* | Half done: signal on g1 11-13 18:11Z and every api block 11-14 01:09Z; recycle off | In 24 h: 22,009 flags, 414,098 still-stuck findings; replays still climb 0.1 → 7.2k/s per connect generation (C §4.1) | Not yet taken |
| 3 | Terminate H1 in connect | 10–15 *(inferred)* | Owner 11-13: "not now" | nginx 26.7 → 44.5 cores (A §3.2) | New evidence (A O1) |
| 4 | Logging and observability off the edges | 3–5 | Not done | 11.9 → 13.6 cores (A §3.2) | Still 3–5 |
| 5 | D4: (a) JWT claims cache, (b) verified-chain cache | 1.8–2.4 api; 2.5–2.9 taskworker and proxy | Shipped both (D46; the proxy from D47p) | (a) JWT 14.6% → 5–5.8% of an api process, 1.5–1.7 cores, 74% of verifications from the cache. (b) taskworker −3 cores, 99.997% hits, x509 25.6% → 6.5–8.0% of a probe process; the proxy's hits about 100% (C §4.1, B §4) | **Met** |
| 6 | Performance clock policy on the Redis hosts | 1.0–1.7 on edge-5; 1.8–2.5 on edge-6 *(inferred)* | Live 11-13 ~16:56Z on both | edge-5 −20% CPU per request at the switch; marginal cost −28 to −30% (edge-6 −22 to −24%); worth 3.2–3.6 cores on edge-5 and 1.7–4 on edge-6 at today's load (E §4) | **Beat the prediction** |
| 7 | GOMAXPROCS A/Bs: connect, api | 2–4; 0.5–1 *(inferred)* | connect g4 at 10 on 11-13 ~20:00–21:30Z, inconclusive, ended early by the owner; the api half never ran | g-processes now reach 8.9 cores, the revert line for a cap of 10 (A §4, §7) | Not run at this load |
| 8 | Remove the nginx stream hop | 2.0–2.5 | Stage 0 11-13 20:08Z; stage 1 (api beta) since 11-14 07:58Z, its 24 h watch passed 11-15 09:00Z | The hop is now 5.07 cores, at 45 µs a query (D §4) | Half built; worth 4.5–5.5 now |
| 9 | No preflight PING by default | 1.0–1.7 Redis | Not done | PING 14.4k → 77.6k/s (E §4) | Larger: fleet #5 |
| 10 | Rebuild transfer_contract's bloated indexes (O2) | 1–1.5 *(inferred)* | Three of five partials rebuilt 11-13/14 | Indexes 1,637 → 1,477 GB; two partials left, 18 GB and 9.9 GB (D §4) | Three-fifths |
| 11 | Reboot edge-2 into `retbleed=stuff iommu=pt` | 1–2.5 *(unmeasured)* | Not chosen (11-13) | `intel_idle_ibrs` 6.7% of samples (D §3.2) | — |
| 12 | One reliability-stats write per shard per round | 0.5–1 connect, plus 0.4 Redis | Not done | 0.1–0.5% of connect; HINCRBY 39k/s, MULTI/EXEC 11.6k/s (A §4) | Still valid |
| 13 | PgBouncer over a Unix socket | 0.7–1.5 *(unmeasured)* | Not chosen (11-13) | 685 loopback servers (D §4) | — |
| 14 | Fewer round trips | 0.5–1 edge-2; 0.25–0.45 Redis | Not done. BEGIN and its lock bound were already one message | Transaction control 29.2% of statements (D §4) | Larger: D #3, fleet #16 |
| 15 | Move edge-6's restore off Redis's CPUs | 6–7 edge-6 cores while it replays; the stalls | The 8K clone is in xops for the next run; the ZFS taskq parameters wait for a reboot | The restore finished (replay ~11-13 21Z, run 11-15 05:45Z); slow entries 16–37k/h → 100–330/h (E §4) | Stalls gone with the restore; check at the next run |
| 16 | One control-plane strategy per probe process | about 1 | Not done | The remaining x509 is probably the per-tunnel DoH clients, not the control-plane clients (C §3.4) | Replaced by C #4 |
| 17 | Lag-guest claim without the claims decode | 0.5–0.6 | Not done, and not void with D4(a) | 2.6–2.7% of an api process (C §4.1) | C #5 |
| 18 | EVAL → EVALSHA | 0.25–0.4 | Not done | EVAL 53k → 110k/s; inbound 108 → 220 MB/s (E §4) | Now 0.6–1.0 |
| 19 | Small database items | 0.85–1.2 together | Not done | Descendant check 0.49–0.68 backends; SN25 amounts 1.41 s a call (0.31 backends); JIT 0.18–0.26 cores (D §4) | Grew |
| 20 | `kernel.numa_balancing=0` on edge-6 | 0.15–0.25 | Live 11-13 11:36Z | Page migrations 18.1k/s → 0; CPU not separable (E §4) | Done |
| 21 | Multiplexed links | 3–5 | Not done | 1.0–1.2M io frames/s still cross sockets (A §4) | Unchanged |

Also from the owner's 11-13 answers: per-slot statistics are live on all 48 nodes (+1.4%
on the test node, E §4); `iommu=pt` on edge-5 is staged for a reboot; the conntrack
bypass was not chosen.

### New costs

1. **The key-event resubscribe storm** (A §5.1, E §5.1). The largest new cost on record,
   and a timing defect too.
   - **How it works.** Every revision read of a network's sessions re-extends the TTL of
     `{ns_<network>}eid`, so every read emits a keyspace `expire` event to all 20 connect
     processes: about 27k events/s each, 87% of them those `expire` events. A g-process
     holding 11–18k clients cannot drain its 1,024-message merge. Each overflow ends the
     subscription on all 48 masters, and after 5 s the process resubscribes and resyncs
     everything, which makes more reads and more events.
   - **Since when:** the bot wave started it on 11-13 about 20Z (a burst at 22Z), and it
     has run without a break since 11-14 15–16Z.
   - **What it costs:** about 7–10 connect cores *(inferred)*, 15–17 Redis cores (Part E's
     fit: 17.4 of 38.0, R² 0.96, with a storm-off window as control), and 4.5–6 softirq
     cores on the Redis hosts. It runs at troughs and peaks alike.
2. **The sweeps' memory in the proxy** (B §3.2). Each process sits at its 6.82 GB memory
   limit with 266k goroutines; GC is about 27 of the 45 sweep cores.
3. **The sweeps' load elsewhere:** edge-2 +13 cores sustained (D §1); connect's population
   costs, the timer heap and the GC (A §3.4); the api about 1.5 cores averaged (C §3.2);
   the taskworker 1.5–2, waiting on full pools and on a unique-exit sweep that grew from
   0.12 s to 66 s a run (C §3.4).
4. **The weekly operator-block award re-reads the week** whenever the taskworker restarts
   while a block is due: about 300 statements of 4.6–13 s, 0.81 backends and 4 parallel
   workers for 42 minutes, then the reliability phase at about 1 backend (D §3.3).
5. **D49p's closes doubled deactivations,** with the sweeps: descendant checks at 358–371/s,
   0.49–0.68 backends (D §5).
6. **Hourly bursts:** the devices series, 0.2 cores averaged, and the contract_close
   orphan sweep, 0.12 (D §5).
7. **MinIO's flat evidence directory on edge-5:** 5–7 cores, most of it the kernel
   walking one directory's extents and spinning on its lock (E §3.6).
8. **Small or none:** Gamma (under 0.001 backends), the points leaderboard (7 runs in
   24 h, 110–197 s each), Stripe dunning (5 runs, 67 s), D50t's streak skip, the account
   epochs, the 8327 trim (one run), D42's capability sets (latency only) and the 11-15
   builds (C §5, D §5, E §5.2).

### Corrections

**To the analysts' briefs** (Parts C and D):
- **The 15–16k against 8–9k api req/s was the replay sawtooth,** 6–7k replays/s 12–18
  hours into a connect generation; it fell at the 01:11Z restart. A sweep adds about
  0.9k api req/s (C §3.2).
- **The points leaderboard rebuild ran 7 times in 24 hours,** every 1–4 hours, not hourly
  (C §3.4).
- **The "~190 ms" direct-connection cost** was a short peak (3–5 minutes; Part D) or a new
  connection's wait for a PgBouncer server at the 22-server cap (Part C), not the direct
  path. New connections cost the same on both paths (C §4.2, D §3.6).
- **The 01:11–01:14Z restart was the config release** above.

**Between the parts, settled here:**
- **The stuck-provider signal reached every api block at 11-14 01:09:21Z**, by the stage
  log (stage 1, g1, at 11-13 18:10:59Z), as Parts A and C have it. Parts D and E had
  09:30Z.
- **connect subscribes on all 48 Redis masters** (E; CPUOPT5: 48 masters, no replicas).
  Part A had 32, after a stale code comment at `redis.go:765`.
- **11-15 was a Sunday** (Part E had Saturday).
- **The control-route sawtooth was 9–10 hours old during the captures,** because the
  config release restarted connect (A, C). Part E had it 27 hours without a reset.
- **The shadow fix:** merge 95306eb79 (D) reached the taskworker in revision f9589fc0e at
  11-13 15:37–15:51Z (C).
- **PgBouncer stage 2's value:** Part C's 0.7 cores uses CPUOPT5's 2.2-core nginx hop.
  Part D measures the hop at 5.07 cores now, so the api's 29–31% of it is about 1.5.
- **Redis "requests":** Part A's 0.57M → 1.37M/s are commands, which count those inside
  scripts; Part E's requests are network requests, 115k → 343k/s. Both are kept, labeled.

**Not settled:**
- **The storm fix's subscriber details.** Part A raises `MessageBufferSize` once the stream
  is quiet and cancels an older close-listener wave when a new one starts. Part E rejects
  both: a larger buffer only delays an overflow, and a cancelled wave leaves late
  listeners unkicked while waves come every ~8 s (A #1, E §7; Decision 1).
- **The storm's softirq figure may overlap its Redis figure.** On edge-5, Part E's process
  figures plus its softirq exceed the host's busy cores by about 2, so part of the
  softirq is probably inside the processes' time (E §3.1).
- **What a hosted window client costs connect.** Part B prices one at CPUOPT5's 0.082 cores
  per 1k clients; Part A's sweep-era fit implies more (A O2), so B #1's connect saving is
  probably low.
- **Whether the storm reaches edge-2.** Part E suggested the lease rechecks may read
  PostgreSQL. Part D found edge-2's step at 11-14 13:50Z, with the sweeps, before the
  storm ran without a break, and did not measure the rechecks' share. connect's pool
  churn (D #4) rose in the windows where both ran.
- **A sweep's cost on the services edges.** Part A's peak-against-trough quarters give +25
  busy CPUs (connect +13.5, api +3.3); Part C's sweep-against-quiet phases give connect
  +11.1 and api +1.95. Different classifications; both are kept.
- **The Redis cost of a replay.** Part C prices the recycle's Redis saving at 0.45–0.55
  cores with CPUOPT5's 178 µs a request; Part E measures 111–117 µs now and puts the
  sawtooth at 0.5–1.0 cores (C #1, E §6).

**To CPUOPT5:**
- The probe's remaining stock-TLS x509 is probably the per-tunnel DoH clients, not the
  control-plane clients that CPUOPT5 C #7 named (C §3.4).
- BEGIN and its lock bound were already one message; CPUOPT5 #6's remaining round trip is
  the separate `SELECT pg_backend_pid()` (D §4).
- CPUOPT5's 480 nginx workers on edge-4 included the web, app and mmm containers' idle
  workers; this round counts only the lb containers' (A §3.5).
- The lag-guest decode is not void with D4(a): the gate decodes every token the cache
  does not hold (C #5).
- Redis's own counters leave out network softirq on the Redis hosts, 23–32 µs per request
  on top (E §3.1; see the caveat above).

### Noticed, not CPU

1. **The storm is a timing defect** (A §5.1, E §5.1).
   - The busy connect processes receive only about 20% of client-removed events.
   - Removals, peers, stream hops, session events and close requests wait for a resync
     wave or the 5-minute corrective poll.
   - Session checks answering "unavailable" rose from 8–20/s to 700–1,430/s.
   - **A fix is being built** on server branch `fix/connect-key-event-storm-1115`, with
     regression tests and no timing bound changed. It is not merged; the owner decides the
     merge and the deploy (Decision 1).
2. **The proxy hosts' ephemeral ports** (B §8). Each holds about 31.5k connections
   (31.2–31.6k) to edge-0's 65.49.70.71:443 against a ~41k-port range (20000–60999).
   `connect()` fails with `EADDRNOTAVAIL` at about 1.3× today's load. The port search
   already costs 0.5–1.6% of a proxy process.
3. **Users degrade during sweeps** (B §3.1): proxy-test setup goes from 2.1–2.3 s to
   5.4–8.8 s (3-hour medians), page load from 7.5–9 s to 11–16 s, and user traffic through
   the proxy from 19 to 11.6 MB/s.
4. **The unique-exit sweep** (`SweepProxyExits`) has hit its 10-minute limit on every run
   since about 10Z and restarts from the oldest assignment, so the newest are never swept
   (C #9).
5. **MinIO's evidence directory on edge-5 reaches XFS's 32 GiB directory limit** around
   12-10 to 12-27; `mkdir` then fails, and evidence PUTs with it (E §3.6, #4).
6. **The weekly operator-block award re-reads the week** whenever the taskworker restarts
   while a block is due, so a release roll can put about an hour of heavy reads in the
   evening (D §3.3, #11).
7. **edge-5's corrected memory errors reached 206** (EDAC mc1; 196 at 00:45Z), with 790
   hardware-error kernel lines since 00:00Z and none uncorrected. It holds 16 primaries
   without replicas (E §3.6).
8. **The validator-idle gate opens too early after a boundary:** progress.json still shows
   the previous epoch finalized just after it. Also require 10 minutes past the boundary,
   and no `POST /sn/attempt-artifact` at the api for 3 minutes (C §2).
9. **Mimir's newest instant sample reads low** until it is complete: Part B's apparent dips
   at 10:32–10:47Z were that (B §2).

Each part's §8 has the rest: TCP resets on the edges, nginx on one lb container per host,
connect's full JWT cache, quality windows below target, `no_active_row` refusals,
find-providers2's wait, node 6411's client evictions and zero HOT updates on three hot
tables.

## Ranked opportunities across the fleet

Cores are at today's load. "Sweep / quiet" is given where a part measured both; the
sweeps ran 68–77% of the time since 11-14 (B, C). Items overlap where noted; do not add
them all.

| # | Change | Part | Cores saved: sweep / quiet | Evidence | Risk | Owner rule |
|---|---|---|---|---|---|---|
| 1 | **Stop the key-event resubscribe storm:** extend session TTLs only when short and give the hint its own revision-only read; drop `expire` before the bounded merge; a subscriber that keeps up; cheaper close-listener resyncs; counters | A #1, E #1 | **26.5–33 at all hours:** Redis 15–17, connect 7–10, softirq 4.5–6 | Redis measured (fit, R² 0.96, and a storm-off window); connect and softirq inferred | low (stream, filter); medium (close-listener resync) | No bound moves; events arrive instead of waiting for a resync wave, so detection gets faster. Being built: `fix/connect-key-event-storm-1115` |
| 2 | Trim a held hosted device's windows to one exit after the idle hold | B #1 | proxy 8–15 + connect 2.5–4.5 / 3–5 (quiet day 0.2–0.5) | inferred (the memory curve; regressions) | medium; owner (it reverses 11-05's "nothing is disconnected") | The 45- and 10-minute holds, pings and timeouts unchanged; the trim waits out the sequence idle timeouts |
| 3 | One `ContractOriginNotifications` per proxy process | B #2 | 4–8 / 1–2 (quiet day about 0.1) | inferred (the memory curve) | low–medium | Route-refusal timeouts (60 s, 6 h) kept |
| 4 | Proxy memory limit 6500 → 8000 MiB on crisp (a per-host value; or alt off the proxy hosts) | B #3 | 6–11 / 1–2, crisp only | inferred (the memory curve) | low–medium; owner (ops; needs a warp change) | A sizing value per host; the 10 GiB cgroup cap stays |
| 5 | No preflight PING by default | A #3, E #2 | now 9.9–13.1 (connect 3–4.5, Redis 5.1–6.1, softirq 1.8–2.5); **2.9–4.1 after #1** | measured rates and per-request costs | low | Each command keeps its own deadline |
| 6 | PgBouncer direct path, stages 2–4, which remove the nginx stream hop | D #1, C #7 | 4.5–5.5 (stage 2 alone: about 1.5 at today's hop; Part C's 0.7 used CPUOPT5's) | measured hop cost per query | medium; owner windows; the backlog restart first | Both paths kept; no timeout moves |
| 7 | Terminate H1 in connect | A O1 | 13–20 | inferred | high; owner said "not now" on 11-13 | No timing change; new evidence only |
| 8 | No access line for successful control and hello calls, sequence-exit logs at V(1), then Loki, Mimir and Grafana off the edges | A #2 | 3–5 | measured line counts and pipeline cost; saving estimated | low; owner (Loki loses the successful-call lines) | No bound; no new setting |
| 9 | Stuck-provider recycle: stage 3, then bake in | C #1, E §6 | api 0.8–1.9 averaged (2–4.5 at a sawtooth's top); Redis 0.45–0.55 (C) or 0.5–1.0 (E); plus nginx | measured replay rates and costs | medium; built, staged | No bound moves; the keys go at bake-in |
| 10 | Shard MinIO's flat evidence directory | E #4 | 3–4.5 on edge-5 | inferred | medium; owner and SN25; before December | Evidence immutable; retention unchanged |
| 11 | Spread edge-6's NIC interrupts, or turn on RFS | E #3 | 1.7–4.1 on edge-6 | inferred | low–medium; owner (host posture) | A host setting in the playbook |
| 12 | Contract stats only for listeners (sdk) | B #4 | 2–3 / about 1 | measured share (3.7–4.8% with assists) | low | No bound changes |
| 13 | Fewer per-frame allocations on the window clients' paths (connect) | B #5 | 1.5–3 / 0.5–1 | measured share (about 6%) | low | No behavior change |
| 14 | connect's timer heap: measure re-arms, remove short ones, one schedule for lease rechecks | A #4 | 1.5–3 | inferred | medium | Same deadlines and cadences |
| 15 | The provide-key change count from a pessimistic Redis marker | D #2 | 1.2–1.6 (0.9–1.2 after #6) | inferred | medium | Never under-reports; the marker's TTL has one default |
| 16 | Lazy BEGIN: BEGIN and its lock bound with the first statement | D #3 | 1.0–1.4 (0.7–1.0 after #6); about 40 fewer servers held | inferred | medium | The same lock bound |
| 17 | A typed codec for the client-score export | C #3 | 1.1–1.6 | measured (phase counter, perf) | medium | Additive key; readers first |
| 18 | GOMAXPROCS 8 → 4 for the api, 72 h on one block | C #2 | 1–2 | inferred | low–medium; approved 11-13, never ran | One per-block value, with auto-revert |
| 19 | One session check per network event per process, if rechecks stay high after #1 | A #5 | 1–3 | inferred | medium (the revocation path) | The 60 s recheck and 90 s lease stay |
| 20 | GETEX resident heartbeat; one reliability flush per shard; prune inside the MULTI | A #6, E #7 | connect 1–2 + Redis 0.3–0.6 | measured shares; Redis inferred | low | The 75 s heartbeat and 300 s TTL stay |
| 21 | Keep connect's PostgreSQL connections through its own stops | D #4 | 0.7–1.5 on edge-2 | inferred | medium; Part A to confirm the source, which #1 may remove | No stop bound lengthens |
| 22 | Verified chains on the probe's stock-TLS dials | C #4 | about 0.9 | inferred | low–medium; owner's probe review | No probe timing or wire change |
| 23 | EVAL → EVALSHA | E #5 | 0.6–1.0 (0.3–0.6 after #1) | inferred | low | The same scripts and deadlines |
| 24 | Small database items: an active-device partial index, JIT off where it compiles every call, the SN25 amounts index, O2's last partials and then the full indexes, a repack of the queue tables, the health aggregate, fewer claim probes, the award from its rollup | D #5–#11 | about 2.1–3.4 together | mostly inferred | low–medium; windows for the index work; the award is an owner and author call | Additive migrations; no bound moves |
| 25 | Small api and taskworker items: a lag-guest pre-filter, legacy-accepted tokens in the cache (overlaps #9), an index for the fuzzy searches, the unique-exit sweep's cursor, client keys decoded once | C #5, #6, #8, #9, small | about 1.15–1.6 | mixed | low | Nothing accepted that is not today; the 10-minute bound stays |
| 26 | Multiplexed links | A #7 | 3–5 | inferred (CPUOPT4) | high | — |
| 27 | Small connect and proxy items | A #8, B #6 | 0.5–1, plus 0.4–0.6 in sweeps | measured shares | low | — |
| 28 | Publish the full-rewrite sets only on change | E #6 | ≈0.02; removes 115–210 ms whole-node blocks | measured | low | Cadence and TTLs unchanged |

**Sized, not proposed:** window clients held by idle hosted devices, 15–25 connect cores
*(inferred)*, a product decision while the owner keeps the 45-minute hold (A O2; #2 is
the smaller step). The window-client replacements cost 2–4 cores in sweeps; investigate
before any change (B #7).

**Totals:**
- **The storm and what follows it** (#1, #5 after #1, #23 after #1): about 30–38 cores at
  all hours, about a tenth of the fleet. #1 is the largest item and the only one that is
  also a timing defect.
- **The proxy's memory items** (#2, #3, #4, #12, #13): Part B's combined figure for all
  five is 20–28 of the 45 sweep proxy cores, plus 2.5–4.5 connect cores. They share one
  prize, about 27 GC cores in sweeps, so their individual figures do not add.
- **The database path** (#6, #15, #16, #21, #24): about 9–12.5 cores with #15 and #16
  priced after #6 (Part D gives 10–13 before that adjustment).
- **The api and taskworker** (#9, #17, #18, #22, #25): Part C's 5–8.5 cores (api 3.0–5.8
  averaged, taskworker 1.9–2.1, Redis 0.45–0.55), more at a sawtooth's top.
- **Services-edge items besides the storm** (#8, #14, #19, #20's connect part, A #8):
  about 7–14 cores, #19 only if rechecks stay high after #1.
- **Host and co-tenant posture** (#10, #11): 4.7–8.6 cores on edge-5 and edge-6.
- **High-risk structural items** (#7, #26): 16–25 cores, with design work.

### Themes

1. **Load, not code, made this round's costs.** No release since CPUOPT5 added a large
   cost. The bot sweeps doubled connect's population and ran the proxy hosts at their
   limit, and at matched work every service costs what it did or less. Every item here
   cuts the bots' cost without refusing anyone, as the owner asked on 11-14.
2. **Bounded resources fail as cliffs, not slopes.** The 1,024-message key-event merge and
   the proxy's 6.82 GB memory limit cost nothing until the population filled them; then
   each cost about 27 cores or more. Others are close: the 22-server PgBouncer pools (13.5
   of 32 at the cap in sweeps), the proxy hosts' ephemeral ports (77% of the range toward
   edge-0), MinIO's 32 GiB directory wall in December, and the edges at 74–79% busy at
   5-minute peaks. Watch the headroom of each, not only the CPU.
3. **The largest CPU item is also a timing defect.** The storm delays removals, peers,
   session events and close requests to the next resync or the 5-minute poll, and the
   unique-exit sweep no longer finishes a pass. Under the owner rule these come first:
   CPU and correctness point the same way.
4. **Work that produces nothing is still the cheapest to remove:** a TTL refresh that
   sends an event to 20 processes on every read (A §5.1), a PING before every no-retry
   command (77.6k/s, E #2), a provide-key count that is almost always 0 (9.3k/s, D #2), a
   BEGIN round trip before the first statement (10.3k/s, D #3), a week re-read after a
   restart (D §3.3), and a sweep that re-walks the same prefix every 10 minutes (C #9).
5. **In the proxy, memory is the CPU.** Near the limit each 0.35 GB of relief halves the
   collection rate, so the memory items are worth more than their direct CPU. Freed CPU
   first comes back as user traffic and faster setups, because the proxy is starved in
   sweeps (B §6).
6. **Host posture paid, and has more to give.** The performance governor cut Redis's cost
   per request 22–30%, more than predicted, and `numa_balancing=0` ended edge-6's page
   migrations. Now edge-6's NIC interrupts crowd NUMA node 2 (E #3), and edge-5's MinIO
   spends 73% of its sampled time in `mkdir` on one directory (E #4).

## Decisions for the owner

**Already decided, not re-proposed** (with what the new evidence changes):
- **H1 termination: "not now"** (11-13). Part A brings new evidence: nginx +67% to 44.5
  cores, following connect's H1+ writes, and 13–20 cores *(inferred)* for the change.
  Decision 5 asks only when to revisit.
- **PgBouncer `default_pool_size`: "not yet"** (11-14). Part D modeled it: no CPU saving,
  because pool occupancy is idle-in-transaction time, and 23 already breaks the
  connection budget (1,030 > 1,021 slots), so a raise needs a PostgreSQL restart for
  `max_connections` or a smaller direct budget. The evidence supports keeping it.
- **No user-agent blocks, ASN caps or new-account proxy limits** (11-14). Every item here
  cuts the bots' cost without refusing anyone.
- **The stuck-provider recycle, staged** (11-13): stage 3 is next (Decision 4).
- **GOMAXPROCS A/Bs** (11-13): the api half never ran (Decision 9). connect stays at 16:
  its busiest processes reach 8.9 cores, the revert line for a cap of 10 (A §7).
- **The direct PgBouncer path, with both options kept** (11-13): stages 2–4 are next
  (Decision 3).
- **One contract size, the UDP gate, the proxy client limits:** no item changes them.
- **Not chosen on 11-13:** PgBouncer over a Unix socket and the edge-2 reboot; their value
  grew with the load (685 loopback servers, 18.2 kernel cores; D §7). The conntrack bypass
  was declined for the Redis hosts; Part A sizes the same mechanism on the edges at roughly
  5–8 cores and does not re-propose it (A §7).

**New decisions:**

1. **The key-event storm fix (fleet #1): merge and roll connect.**
   - **Options:** (a) merge `fix/connect-key-event-storm-1115` once its regression tests
     pass on the merged tree, and roll connect in a validator-idle window; (b) hold, and
     keep paying about 26.5–33 cores and the delayed removals.
   - **Evidence:** A §5.1, E §5.1: resubscribes every 7–9 s; removed-flag delivery 56–76%
     against a 160% baseline; "unavailable" session checks 700–1,430/s; 17.4 Redis cores
     by fit.
   - **Cost:** a connect release, which restarts every connect process (providers
     reconnect, the replay sawtooth restarts). No new setting.
   - **For the branch, two points on which the parts disagree:** Part A cancels an older
     close-listener wave when a new one starts and raises `MessageBufferSize`; Part E
     rejects both (E §7). Both agree that quieting the stream comes first and that a
     buffer change alone does nothing.
   - **Recommendation:** (a). Keep every close-listener wave and make each take cheap
     (pipelined SPOPs, no PING), which reads the owner rule the conservative way. Keep the
     buffer at 1,024 unless merge drops persist once the stream is quiet and `expire` is
     filtered before the merge. Judge it by the acceptance list in "Validating a change".
2. **The proxy's memory (fleet #2–#4, #12, #13).**
   - **Options:**
     - (a) ship the code items B #2, #4 and #5 with the next proxy release;
     - (b) crisp's memory limit 6500 → 8000 MiB, which needs a per-host override in warp
       (fireside stays at 6500: 9.0 GB minimum available and OOM kills on 10-23 and
       10-25), or instead move alt off the proxy hosts (6.4–6.6 GB each);
     - (c) B #1, trimming a held device's windows to one exit after the idle hold.
   - **Evidence:** GC 58–60% of a proxy process in sweeps; the cliff at about 4.9 GB of
     heap and stacks; users degrade in sweeps (B §3.1–3.2).
   - **Cost:** (b) a warp change and 15 GB of crisp's memory; (c) a returning user rides one
     exit for the seconds a refill takes, and it reverses 11-05's "nothing is
     disconnected".
   - **Recommendation:** (a) now; (b) once warp has the override; (c) yes. It is the
     largest proxy lever, and it touches only devices idle for 10 minutes or more.
3. **PgBouncer: stages 2–4 of the direct path (fleet #6).**
   - **Options:** stage 2 (every api block, with pool destroys counted by reason), stage 3
     (connect), stage 4 (the rest), each in its window and reverted by a merge before the
     next nightly if a trigger fires.
   - **Evidence:** stage 1 ran clean for 24 hours; new connections cost the same on both
     paths; the nginx hop is 5.07 cores, 79% kernel (C §4.2, D §3.2).
   - **Cost:** first the one-at-a-time `./run-dbs.sh --pgbouncer-restart`, so the
     instances listen with the configured backlog of 65535 (still 128).
   - **Recommendation:** the restart, then stage 2 this week, then stages 3 and 4. Keep the
     pool size (above).
4. **The stuck-provider recycle (fleet #9): stage 3.**
   - **Options:** the recycle on beta connect, then g1, then all; both switches become code
     defaults after a week on every block.
   - **Evidence:** the signal ran 24 hours on every api block: 22,009 clients flagged,
     414,098 still-stuck findings, replays 0.1 → 7.2k/s per connect generation (C §4.1).
   - **Cost:** medium risk; a recycled provider reconnects with its drain excuse written
     first.
   - **Recommendation:** proceed as staged, outside the validator's busy minutes. Compare
     by block, because the storm fix's connect roll restarts the sawtooth.
5. **H1 termination in connect (fleet #7): when to revisit.**
   - **Options:** keep "not now"; or start the design, one block on dedicated addresses
     first.
   - **Evidence:** nginx 26.7 → 44.5 cores at 0.19–0.22 cores per 1k H1+ writes/s; the
     72-CPU edges at 74–79% busy at 5-minute peaks (A §3.2; A §6, O1).
   - **Cost:** high risk: ports, rate limits and the deploy port rotation move into connect.
   - **Recommendation:** keep "not now" until fleet #1 and the proxy items land, then
     measure nginx per H1+ write and the edges' headroom again. Fleet #2 alone would
     release 32–54k window clients and their H1 transports.
6. **Access logs and observability placement (fleet #8).**
   - **Options:** (a) no access line for 1xx/2xx answers on the control and hello routes,
     every 4xx/5xx kept; (b) connect's sequence-exit line at V(1); (c) Loki, Mimir and
     Grafana off the edges.
   - **Evidence:** edge-1's journal takes 3.6k lines/s, 2.7k of them access lines; logging
     and observability cost 13.6 cores (A §3.2).
   - **Cost:** successful control calls leave no line in Loki.
   - **Recommendation:** (a) and (b) now; (c) when a host is chosen.
7. **MinIO's evidence directory (fleet #10), with SN25, before December.**
   - **Options:** new content keys under two hex levels with a dual read, the flat
     directory read-only from then on; or leave it.
   - **Evidence:** the directory reaches XFS's 32 GiB limit between 12-10 and 12-27, and
     `mkdir` then fails; 73% of MinIO's sampled CPU is `mkdir` in it (E §3.6).
   - **Cost:** a layout change on SN25's evidence path.
   - **Recommendation:** approve the new layout and ship it before December.
8. **edge-6's NIC interrupts (fleet #11) and edge-5's hardware.**
   - **Options:** spread the i40e IRQs across NUMA nodes, or turn on RFS, through the Redis
     play in a validator-idle window, reverting if µs per request rises 5%.
   - **Evidence:** 16 CPUs at 96–97% busy with 38–48% softirq; 30 of 32 Redis I/O threads
     on them; CPU pressure 34.6% (E §3.5).
   - **Cost:** low–medium risk on the network path, reversible; no Redis or application
     bound changes (E #3).
   - **Also:** edge-5's 206 corrected memory errors. `iommu=pt` and the ZFS taskq
     parameters wait for planned reboots; decide replicas and the hardware ticket first.
   - **Recommendation:** try one setting on edge-6 now, measure it, then template it.
9. **GOMAXPROCS for the api (fleet #18): run the approved A/B.**
   - **Options:** api g4 at 4 cores for 72 hours through sweep phases, with `gmp_apply.sh`'s
     auto-revert watcher; or drop it.
   - **Evidence:** the scheduler is 21.9–26.9% of an api process (C #2).
   - **Cost:** low–medium risk; a runtime knob, reverted on any rise in the control
     route's phase latencies or the in-flight gauges (C #2).
   - **Recommendation:** run it; connect stays at 16.
10. **Database windows and the award (fleet #24).**
    - **Options:** O2's last two partials (approved), then the full indexes one at a time;
      a `pg_repack` of transfer_debit_journal and pending_task; the active-device and SN25
      amounts indexes as additive migrations; and whether the award reads the hourly
      rollup instead of the week (D #11).
    - **Evidence:** D §3.3, §3.4, §4.
    - **Cost:** each index build holds a snapshot (915–1,400 s in the O2 windows); the
      award's figures may differ from the rollup's by late settlements.
    - **Recommendation:** schedule the partials and the repack; decide D #11 with its
      author. Until then, time taskworker rolls so that the award's read, which starts 15
      minutes after a taskworker start while a block is due, does not land in the evening.
11. **The proxy hosts' ephemeral ports (not CPU).**
    - **Options:** spread the H1 transports by placement weight, add a second source
      address, or use IPv6 transports. Fleet #2 lowers the count.
    - **Evidence:** 31.2–31.6k connections per host to one edge address against a ~41k-port
      range; failure at about 1.3× today's load (B §8).
    - **Cost:** not sized in Part B; each option changes where the window clients' H1
      transports connect.
    - **Recommendation:** choose one before the next step in load.

## Validating a change

CPUOPT5's rules hold:
- **Normalize by the work:** connect per exchange frame and per exchange connection (the
  sweep connections are mostly idle); the proxy per window client and per GC cycle; the
  api per request, with replays apart; Redis per request, with the hosts' softirq
  alongside; PostgreSQL per commit and per PgBouncer query.
- **Compare like with like:** the same hours over at least three days, and A/B on one
  block where a change can be staged.
- **Hold the timing bounds as acceptance criteria,** not just the CPU.
- **Measure from Mimir and perf together,** not one short profile.

New this round:
- **Separate sweep phases from quiet phases.** Sweeps fill 68–77% of the time, and a
  window's mix of the two moves every figure (C §3.2, B §3.1).
- **Mind the connect generation's age.** A config release restarts every service too, as
  at 01:11Z on 11-15.
- **Check edge-2 for the award's read** (`ComputeOperatorBlockPoints`, starting 15 minutes
  after a taskworker start while a block is due) and for long application snapshots
  before comparing hours.
- **Gate captures and rolls at least 10 minutes past a validator boundary.**
- **Use complete Mimir samples;** the newest instant sample reads low.

| Fleet # | Measure | Target |
|---|---|---|
| 1 | Resubscribes and merge drops | at or below 0.3/s fleet-wide; drops near 0 |
| 1 | Removed-flag delivery | back near 160% |
| 1 | SPOP, PING, inventory reads | under 6k/s, under 20k/s, under 6k/s |
| 1 | connect pool misses; "unavailable" session checks | under 50/s; under 20/s |
| 1 | Redis cores at today's clients; beta connect processes; g-process cores per frame | about 21–24; near 0.6 cores each; down |
| 1 | Timing | recheck latency percentiles unchanged or better |
| 2–4, 12, 13 | Proxy GC cycles a minute and heap plus stacks per process, in sweeps | from 12.1 toward about 1, under about 4.9 GB |
| 2–4, 12, 13 | Proxy cores per window client; proxy-test setup p50 and user MB/s in sweeps | 0.31 mcores toward 0.11; 6.8 s toward 2.2 s; 11.6 toward 19 MB/s |
| 3 (crisp) | OOM kills; crisp's available memory | none; at least 26 GB at the daily minimum |
| 5 | Redis PING rate; `server.redisWithClient` in connect's profile | down by the removed sites' rates; near 0 |
| 6 | nginx CPU per query; PgBouncer CPU per query | falls with the moved acquires; unchanged (64 µs) |
| 6 | Each block's `initial_ping` and request latency; the `empty` share of acquires | at or below its own baseline; at or below g1–g4's before the stage |
| 8 | Journal lines per edge; logging and observability cores | about a quarter of today's 3.6k/s; below 13.6 |
| 9 | `urnetwork_connect_stuck_provider_recycles_total{outcome="recycled"}`; replay slope per connect hour; still-stuck decisions | above 0 on the enabled block; below the other blocks'; falling |
| 9 | Provider reliability and excused drains | unchanged |
| 10 | MinIO cores; `RenameData` and `CreateFile` latency; the flat directory's size | below 5–7; below 37 and 31 ms; no longer growing |
| 11 | edge-6 CPU pressure "some"; I/O-thread involuntary switches; µs per request | under 10%; under 1k/s; lower (revert above +5%) |

## Part A — Services edges and connect

Domain: by-us-fmt-5-edge-0/1/3/4 (312 logical CPUs) and the connect service.
Connect runs `2026.11.14-outerwerld+1098366310` (D47: server aa7142599, connect
library ed68beb), rolled 11-14 07:08–07:38Z, beside mcp `1098368160` and gossip
`1098358910` from the same server revision. All 20 connect processes were
recreated in place, same build, at 11-15 01:11–01:13Z, when the config release
reached the hosts (§8). Measured read-only on
11-15 between 10:14Z and 11:08Z, and from Mimir back to 11-12 12Z. Scratch:
`<scratch>/A/`.

### 1. Summary

- **The services edges went from 140–158 to 185–211 busy CPUs** (means; pre-sweep
  11-12 evening and 11-13 day against the sweep era, 11-14 evening and 11-15
  morning). The 72-CPU edges now run 65–73% busy on average and 74–79% at
  5-minute peaks (46–60% and 53–68% before).
  - Connect 73.5–77.4 → 110–123 cores. nginx 26.7 → 44.5 (census). Logging and
    observability 11.9 → 13.6. The api and taskworker are flat.
- **The work behind it is the hosted-proxy sweeps.**
  - Clients went from 147–150k to 278–291k, exchange connections from 224–276k to
    533–712k, connect goroutines from 7–8M to 13–15M, and frames from 2.0 to
    2.2–2.7M/s.
  - There is no quiet hour in the sweep era. Since about 09:20Z on 11-15 the
    pulses have merged, and exchange connections stay at 690–730k.
  - A pulse at its top costs about 25 busy CPUs (§3.1).
- **At matched work, connect costs what it did.** CPUOPT5's load model predicts
  125 cores for 11-15's work; 118 were observed. Per client, connect still holds
  53 goroutines and 2.5 MB of heap.
- **The largest removable new cost is the key-event resubscribe storm** (§5.1;
  Part E §5.1 measures the same storm from the Redis side).
  - **How the stream fills up:** the session scripts extend `{ns_<network>}eid` on
    every revision read. Each extension is a keyspace "expire" event that every
    connect process receives and then discards.
  - **What overflows:** a g-process has one dispatch goroutine and a 1,024-message
    merge buffer. That buffer overflows, and every overflow costs 5 s unsubscribed
    plus a full resync.
  - **How often:** since 11-14 16Z every g-process has resubscribed every 7–9 s,
    against every 1–4 minutes before.
  - **What is lost:** most of the g-processes' key events never arrive.
  - **What the resyncs cost:**
    - continuous resync waves take 8.6–15.2% of a g-process (1.5–2.0% before);
    - Redis commands rose from 0.57M to 1.37M/s, and Redis CPU from 22 to 38 cores;
    - the stable beta processes pay 1.2–1.4 cores each just to receive the stream.
- **Other costs that grew with the population** (both CPUOPT5 captures and these
  were of processes 9–12 h old):
  - the timer heap: `adjust` self time is back up to 2.2–4.0%;
  - the GC: one 32 core-second cycle every ~127 s per g-process, about 4 cores in
    all.
- **None of CPUOPT5's Part A items shipped** (§4). H1 termination was declined
  ("not now"). The GOMAXPROCS A/B was inconclusive, and connect stayed at 16. The
  rest were not done.
- **Ranked opportunities** (services-edge cores at sweep load; they overlap):

| # | Change | Est. saving | Risk |
|---|---|---|---|
| 1 | Stop the key-event storm: session keys extended only when short, then a subscriber that keeps up (drain during startup, batch, larger buffer, one close-listener wave at a time; Part E disputes the last two, see #1) | 7–10 connect (6–12) *(inferred)*, plus most of 0.8M Redis commands/s (E #1: 15–17 Redis cores) | low–medium |
| 2 | No access-log line for successful control and hello calls, sequence-exit logs at V(1), then Loki/Mimir/Grafana off the edges (CPUOPT5 #4) | 3–5 | low |
| 3 | No preflight PING on connect's deadline and no-retry Redis calls (E #2; CPUOPT5 E §5.1) | 3–4.5 now; 0.5–1 after #1 | low |
| 4 | Timer heap: measure re-arms by site, remove short re-arms, fewer armed timers per idle client | 1.5–3 *(inferred)* | medium |
| 5 | One session check per network event per process (after #1, if rechecks stay ≫ the timer baseline) | 1–3 *(inferred)* | medium |
| 6 | GETEX heartbeat and one reliability flush per shard per round (CPUOPT5 A §5.4–5.5; with E #7) | 1–2, plus Redis | low |
| 7 | Multiplexed links (CPUOPT4 #21) | 3–5 | high |
| 8 | Small items (`encodeUuid`, message pool, pacing evidence) | 0.5–1 | low |
| O1 | Owner: H1 termination in connect. New evidence: nginx +67% | 13–20 *(inferred)* | high |
| O2 | Owner (product, Part B): window clients held by idle hosted devices | 15–25 connect *(inferred)*, plus nginx | product |

### 2. Method

- **Load and builds.**
  - **Load during the captures:** 11-15 10:14–11:03Z, a Sunday. 276–291k clients,
    700–738k exchange connections, connect 125–130 cores, the edges 215–227 busy
    CPUs (2-minute Mimir).
  - **No trough:** the pulses had merged since ~09:20Z, so no trough fell in a
    validator-idle window.
  - **The quiet baseline:** CPUOPT5's captures of the same processes (edge-0 g2 and
    edge-3 g4, 11-13 04:20–04:41Z, 149k clients, build a00dfeb9a) and the pre-sweep
    Mimir windows.
  - **What changed in between:** connect's code moved little from a00dfeb9a to
    aa7142599 (§4).
- **Root access.**
  - Per the owner's 11-15 allowance, `xops/bin/by-pass <host> by` was piped into
    `sudo -k -S -p ""` over ssh. The password was never printed, logged or stored.
  - Scripts travel base64-encoded in the ssh command (`<scratch>/A/bin/rrun.sh`).
  - Each capture script deletes its data and checks for perf processes.
- **Validator gate.** `<scratch>/A/bin/vgate.sh`, a copy of the roll scripts'
  `validator_ok`. A capture runs only when all three hold:
  - the intent for the current epoch is finalized or applied;
  - no measurement is in the journal;
  - at least 900 s remain to the boundary.
- **Captures:**

| Time (Z) | Capture | Result | Validator at the gate |
|---|---|---|---|
| 10:14–10:16 | census, all four edges, `/proc` over 10 s (`census_e*_10*.txt`) | sweep peak | not required |
| 10:18:23 | perf, connect edge-0 g2, 30 s at 199 Hz, call graphs | 8.08 cores, 52.7k samples | 10:18:16 intent 26354 finalized, ~1,236 s |
| 10:20:2x | perf, edge-4 lb nginx workers, 20 s at 99 Hz | 48 workers, 13.46 cores, 32.1k samples | 10:20:18 finalized, ~1,116 s |
| 10:48:18 | perf, connect edge-3 g4, 30 s at 199 Hz, timed to include one GC cycle (from `go_memstats_last_gc_time_seconds`) | 8.86 cores, 55.3k samples | 10:48:10 intent 26355 finalized, ~3,768 s |
| 10:51:05 | perf, connect edge-1 g3, 30 s at 199 Hz | 7.54 cores, 46.6k samples | 10:50:58 finalized, ~3,600 s |
| 10:52:4x | perf, edge-0 lb nginx workers, 20 s at 99 Hz | 72 workers, 11.12 cores, 29.7k samples | 10:52:37 finalized, ~3,492 s |
| 11:02:53 | perf, connect edge-0 beta, 30 s at 199 Hz | 2.00 cores, 16.7k samples | 11:02:46 finalized, ~2,892 s |
| 10:5x–11:0x | edge-1: logging drivers, journal volume, access-log and connect-log classes (counts only) | §3.2 | not required |

- **Nothing was refused.** Captures paused 10:24–10:45Z, when the validator was
  busy around the 10:39Z boundary, and resumed when intent 26355 finalized.
- **Two incidents, both cleaned up:**
  - **A concurrent capture on edge-0.** Part C's taskworker capture (`/tmp/cpuopt6c2.*`,
    `perf record -p 1751959`) was running there during Part A's 10:52Z nginx capture,
    so the host had two captures for part of those 20 s. It was gone by 11:07Z and was
    not touched. Part C's script files `/tmp/cpuopt6c_perf_one` and `_two` remain on
    edge-0, edge-1 and edge-4, to be removed.
  - **A heavy journal read on edge-1.** A 10-minute read of the whole journal ran
    for about 4 minutes before it was stopped. The orphaned remote
    `journalctl` was killed by its exact command line at ~11:00Z, and the read was
    replaced with an indexed per-container one.
  - **The 11:07Z check:** no perf process and none of Part A's scratch on any edge.
- **Mimir:** 11-12 12Z – 11-15 10:30Z (`windows6.py`, `hourly.py`, `fine.py`,
  `fit6.py`, `resync_trend.txt`, `removal_delivery.txt`, `redis_cmd_compare.txt`).
- **Code** was read at server aa7142599 and connect ed68beb (the D47 build inputs).
  CPUOPT5's connect was read at a00dfeb9a and fc949b78.
- **Not captured:** a sweep trough (none came), and bpftrace. Timer modifications
  were drilled from the perf stacks instead.

### 3. Where the CPU goes now

#### 3.1 Hosts, matched windows

Means of 2-minute samples (`windows6.txt`). "Other" is busy minus every Go job's
`process_cpu_seconds_total`: nginx, logging and observability, and kernel work
outside processes.

| Window (connect build) | Clients | Live hosted devices | Busy CPUs | Kernel % | Connect | api | taskworker | Other | H1 starts/s | H1+ writes/s | Frames/s (local share) | Exchange conns | Goroutines | Heap in use | µs per frame |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 11-12 18–22Z (a00dfeb9a, CPUOPT5's evening) | 150k | 13.7k | 140.3 | 41.3 | 73.5 | 13.9 | 13.8 | 38.6 | 484 | 157k | 2.04M (56%) | 224k | 7.1M | 323 GB | 36.1 |
| 11-13 08–17Z (a00dfeb9a) | 147k | 13.7k | 158.2 | 40.8 | 77.4 | 20.8 | 13.4 | 46.1 | 475 | 152k | 1.96M (52%) | 276k | 8.1M | 392 GB | 39.5 |
| 11-14 18–22Z (D47) | 241k | 55.7k | 210.7 | 41.6 | 116.3 | 21.7 | 13.2 | 58.9 | 706 | 199k | 2.67M (54%) | 626k | 13.6M | 650 GB | 43.5 |
| 11-15 02–10Z (D47, restarted 01:11Z) | 285k | 41.3k | 199.2 | 41.1 | 117.7 | 17.0 | 13.1 | 51.0 | 629 | 192k | 2.39M (56%) | 648k | 14.5M | 706 GB | 49.3 |
| … its troughs (lowest quarter of exchange conns) | 278k | 36.5k | 185.4 | 40.9 | 109.8 | 15.3 | 12.7 | 47.1 | 536 | 183k | 2.18M (57%) | 533k | 13.0M | 646 GB | 50.4 |
| … its peaks (highest quarter) | 287k | 42.4k | 210.4 | 41.4 | 123.3 | 18.6 | 13.3 | 54.6 | 668 | 203k | 2.62M (55%) | 712k | 15.3M | 746 GB | 47.1 |

Per host (`host_busy.txt`): the 72-CPU edges averaged 46–60% busy before the
sweeps, with 5-minute peaks of 53–68%. They now average 65–73% and peak at 74–79%.
edge-0 (96 CPUs) runs 52–57% (60–66% at peaks).

- **The sweeps.**
  - On 11-15 a pulse came every ~40 minutes until 09:20Z (06:38, 07:18, 07:56,
    08:38, 09:18Z). Each brought +20–30k clients in 4–6 minutes and took exchange
    connections from 500k to 720k and goroutines from 12.7M to 15.6M.
  - Since 09:20Z the load has not come back down.
  - Between pulses, clients stayed at 265–280k.
- **A pulse at its top costs about 25 busy CPUs:** connect +13.5, other +7.5, api
  +3.3 (peak quarter against trough quarter). Most of that is traffic: frames
  rose 0.44M/s, which is 16 cores at the pre-sweep 36 µs per frame.
- **The sweep era added about 50 busy CPUs at all hours:** connect +32 to +46,
  other +5 to +16.
- **Per connected client the footprint is unchanged:** 53 goroutines (56 on 11-13),
  2.5 MB of heap in use (2.7 MB), and 8.5 KB of stack per goroutine.
  - **Totals across 20 processes:** 15.3M goroutines and 134 GB of stacks.
  - **Per g-process:** heaps of 37–63 GB (18–24 GB in CPUOPT5) and allocation of
    212 MB/s (58–77).
  - **GC still runs at the 2-minute floor,** every 127–129 s, with heap goals of
    ~74 GB.

#### 3.2 By process (census at the sweep peak, 11-15 10:14–10:16Z)

| Host | connect | nginx | api | taskworker | mcp + gossip | logging and observability | Host busy (10 s) |
|---|---|---|---|---|---|---|---|
| edge-0 | 36.0 (beta 0.7, g 8.7–8.9) | 10.9 | 3.2 | 3.8 | 0.45 | 2.9 (Grafana stack 1.1, dockerd/containerd 0.75, journald/rsyslog 0.6, fluent-bit 0.5) | 61.1 (64%) |
| edge-1 | 32.0 (beta 1.8, g 6.7–8.0) | 10.3 | 3.7 | 4.4 | 0.04 | 4.2 (Grafana stack 2.5, Loki alone 1.9) | 57.9 (80%) |
| edge-3 | 27.4 (beta 1.4, g 5.7–7.0) | 11.0 | 5.4 | 3.9 | 0.04 | 3.3 | 55.1 (77%) |
| edge-4 | 29.4 (beta 1.7, g 6.6–7.5) | 12.3 | 6.3 | 1.8 | 0.26 | 3.2 | 57.3 (80%) |
| **Sum** | **124.8** | **44.5** | **18.6** | **13.8** | **0.8** | **13.6** | **231.4** |
| CPUOPT5, 11-13 04:18Z (149k clients) | 72.6 | 26.7 | 16.3 | 13.2 | — | 11.9 | 145.1 |

- **nginx +67% (26.7 → 44.5 cores)** while connect's H1+ writes doubled (119k →
  236k/s) and sessions rose from 148k to 279k. That is 0.22 → 0.19 cores per
  1k writes/s: nginx follows the writes (§3.5).
- **Logging and observability +14%.**
  - Every service container logs to journald (`LogConfig.Type`).
  - **edge-1's journal takes 3.6k lines/s** (`logging_e1.txt`, `accesslog_e1.txt`,
    `connectlog_e1.txt`):
    - 2.7k/s is nginx access lines. In the busiest lb container, 64% are 2xx
      `POST /connect/...` (api control) and 18% `GET /hello`.
    - 0.93k/s is connect, all INFO `transfer_sequence_exit.go:67` lines (connect
      library).
  - `no_access_log` (vault/main/services.yml) lists only lag.dev.
- **mcp (0.02 cores per process) and gossip (0.025):** nothing to take.
- **Kernel share unchanged at 41%.** The host-wide softirq is 10.6–13.8% of all
  CPU time (`census_*`).

#### 3.3 The connect load model

Least squares of connect cores on the work (`fit6.py`):

| Span | Points | Model | rms | r² |
|---|---|---|---|---|
| CPUOPT5's generation, 11-12 18:30Z – 11-13 18Z | 283 × 5 min | 5.7 + 0.101 per k exchange conns + 0.0197 per k frames/s + 0.011 per H1 start/s | 0.98 | 0.990 |
| D47, 11-14 08:30Z – 11-15 01Z | 199 × 5 min | 34.4 + 0.066 + 0.0122 + 0.011 | 4.2 | 0.905 |
| D47 restarted, 11-15 02–10Z | 246 × 2 min | 54.2 + 0.039 + 0.0151 + 0.003 | 1.7 | 0.929 |
| The same three, by goroutines | | −13.6 / 21.1 / 33.8 + 5.2 / 4.7 / 4.0 cores per million goroutines + 0.025 / 0.011 / 0.011 per k frames/s | 1.2 / 3.1 / 1.6 | 0.98 / 0.95 / 0.94 |

- **At matched work connect is no dearer.** CPUOPT5's model gives 125 cores for
  11-15's mean work (648k exchange connections, 2.39M frames/s, 629 H1 starts/s);
  117.7 were observed. The growth is the work: 2.9× the exchange connections and
  17% more frames.
- **The marginal cost per exchange connection fell, and the intercept rose.**
  - Per 1,000 exchange connections: 0.10 → 0.04–0.07 cores. The sweep connections
    are mostly idle.
  - The intercept: 6 → 34–54 cores. Part of today's cost does not move with the
    pulses:
    - the resubscribe storm runs at the same rate at troughs and at peaks (§5.1);
    - the GC cycles at the 2-minute floor, on heaps proportional to the population;
    - the timer heap grows with the population (§3.4).

#### 3.4 Inside connect

Shares of one process's samples and the cores they represent
(`cmp_all.txt`; files `perf_conn_*.txt`). The CPUOPT5 columns are the pre-sweep
baseline: the same processes at 149k clients.

| Category | CPUOPT5 e0 g2 (5.31 c) | CPUOPT5 e3 g4 (4.27 c) | e0 g2 (8.08 c) | e3 g4 (8.86 c, a GC cycle) | e1 g3 (7.54 c) | e0 beta (2.00 c) |
|---|---|---|---|---|---|---|
| `findRunnable` incl / `stealWork` incl | 22.1 / 8.4 | 18.6 / 6.1 | 23.7 / 7.8 | 16.0 / 5.1 | 18.7 / 6.0 | 32.9 / 7.0 |
| Timer heap: `check` incl / `adjust` self / sift self | 2.9 / 0.95 / 0.33 | 3.1 / 1.15 / 0.35 | 6.9 / 3.96 / 0.66 | 4.5 / 2.21 / 0.52 | 5.4 / 2.93 / 0.53 | 1.3 / 0.16 / 0.05 |
| H1 transports (`ConnectHandler.Connect`) | 22.6 | 16.9 | 18.6 | 18.7 | 18.4 | 2.8 |
| … write batch / pooled read | — | — | 6.2 / 4.5 | 6.1 / 4.2 | 7.2 / 5.3 | 0.9 / 0.7 |
| Exchange socket trees | 17.3 | 9.8 | 16.0 | 12.2 | 14.6 | 3.4 |
| D1 local pumps | 2.4 | 1.9 | 2.0 | 2.0 | 2.0 | 0.2 |
| go-redis | 10.2 | 9.2 | 12.3 | 12.7 | 16.4 | 32.4 |
| … of it the preflight PINGs (`server.redisWithClient`) | — | — | 2.5 | 3.5 | 4.1 | 0.2 |
| **Resync-driven work** (by goroutine start: close listeners, lease checks, session groups, hop reconciles, resync waves) | **~1.5** | **~2.0** | **8.6** | **11.1** (12.6 without the GC) | **15.2** | 0.4 |
| … close listeners (`TakeClientCloseRequests`) | 0.17 | 0.16 | 3.0 | 6.0 | 7.0 | 0 |
| … authorization-lease checks | 0.74 | 1.15 | 2.7 | 3.5 | 3.9 | 0.2 |
| … session notification groups | 0.22 | 0.28 | 1.5 | 1.2 | 3.7 | 0.1 |
| go-redis pub/sub readers + node drains + dispatch | — | — | ~0.1 | ~0.2 | ~0.2 | **34.6** |
| `ConnectionAnnounce.run` | 3.7 | 4.8 | 4.3 | 3.2 | 3.2 | 1.0 |
| `ReadContractHoleLease` / `PublishConnectionAuthority` | 1.7 / 0.5 | 1.1 / 0.5 | 2.6 / 1.2 | 2.2 / 1.5 | 2.5 / 1.8 | 0.5 / 0.1 |
| `GetResidentForClient` | 3.7 | 1.6 | 1.5 | 1.2 | 1.3 | 0.3 |
| GC (mark, assists, sweep) | 0 | 16.7 | 0 | 11.9 | 0 | 5.1 |
| Kernel leaf, all | 43.2 | 38.9 | 42.2 | 36.2 | 42.5 | 57.0 |
| `tcp_sendmsg` / softirq / `nf_hook_slow` / AppArmor (k) | 13.1 / 8.7 / 4.5 / 1.9 | 8.7 / 10.0 / 3.6 / 1.4 | 12.6 / 10.7 / 4.4 / 2.1 | 11.7 / 9.7 / 3.8 / 1.9 | 14.3 / 10.4 / 5.2 / 2.5 | 1.9 / 13.2 / 3.8 / 2.2 |

- **Resync-driven work**
  - **Size:** 8.6–15.2% of the three g-processes, against 1.5–2.0% before, which
    is 0.7–1.1 cores per g-process.
  - **The close-listener takes** are its largest part, at 3.0–7.0%. Each is a
    preflight PING plus an SPOP on the no-retry pool. About 50k takes run per
    second fleet-wide, and only ~190/s find anything
    (`urnetwork_client_close_sequences_sent_total`).
  - **The lease checks** are 2.7–3.9% (0.7–1.2%). The session groups' revision
    reads are 1.2–3.7%: `session.NetworkSessionRevision` under
    `runSessionNotifications`.
- **The pub/sub readers**
  - **A stable subscription is expensive.** The beta process (~1,100 residents)
    spends 34.6% in its keyspace subscription:
    - go-redis's pub/sub readers (`(*channel).initAllChan.func1`) 28.1%;
    - their health check 2.8%;
    - the node drains 3.8%;
    - the dispatch loop 2.8%.
    Add its share of the scheduler (33%) and of receive-side kernel work, and
    receiving the stream costs a stable subscriber ~1.2–1.4 cores *(inferred)*. Beta processes
    ran 0.55 cores on 11-12/13 and 1.6 now.
  - **The g-processes barely receive the stream.** Their readers are at or below
    0.01%: their subscription is down nearly all the time (§5.1).
- **The timer heap: `adjust` self time is back to 2.2–4.0%** (0.95–1.15%).
  - **No single call site dominates.** The re-arm drill (`timer.modify` and
    `Timer.Reset/Stop` by first application frame) spreads it over:
    - poll deadlines: exchange writes (`exchangeDeadline.refresh`), H1 reads, and
      go-redis and pgx connection deadlines;
    - the client library's `sequenceDeadlineTimer.armBy`;
    - the H1 handler's timer (`ConnectHandler.Connect.func14.7`);
    - the lease checks' contexts;
    - the resync waves' per-kick `time.After`.
  - **Why it grew with the population:** Go 1.27's `adjust` still walks the whole
    per-P heap whenever the earliest modified timer is due, or when zombies exceed
    a quarter of the heap (`/usr/local/go/src/runtime/time.go:906`, `:1003`). The
    cost is the timer population times the re-arm rate, and both doubled.
- **The GC** (one cycle caught on edge-3 g4) took 11.9% of the window:
  - `gcDrain` 10.9%;
  - green-tea span scanning (`tryDeferToSpanScan`) 5.1% leaf.
  That is ≈32 core-seconds per cycle. At a 127 s cadence it averages ≈0.25 cores
  per g-process, ≈4 cores across connect (3–4%), and it is proportional to the
  heap.
- **The data plane holds its shares,** in proportion to the traffic: H1 18–19%,
  exchange trees 12–16%, D1 2%, `ConnectionAnnounce` 3–4%.
- **The scheduler** takes 16–24% of a g-process (18.6–22.1% before).

#### 3.5 nginx

| Where | CPUOPT5 edge-4 (9.83 cores) | edge-4 (13.46 cores, 48 lb workers) | edge-0 (11.12 cores, 72 lb workers) |
|---|---|---|---|
| Kernel (leaf) | 55.9% | 66.1% | 67.4% |
| libcrypto / libssl (any frame) | 39.1% / 46.4% | 36.9% / 43.3% | 34.4% / 42.8% |
| TLS record read and write (incl) | 29.5% | 32.8% | 37.6% |
| Handshakes: ECDHE + signatures | 0.3% + 1.6% | 0.2% + 1.0% | 0.06% + 0.4% |
| `tcp_sendmsg` / epoll / softirq | 18.6% / 12.3% / 14.4% | 23.9% / 12.3% / 21.1% | 25.2% / 13.4% / 19.5% |
| loopback transmit / conntrack (incl) | — | 12.4% / 8.3% | 13.6% / 8.7% |
| nginx's own code + libc (leaf) | 10.6% + 11.6% | 9.4% + 6.9% | 9.0% + 7.5% |

- **nginx relays each of connect's H1+ writes as one TLS record.**
  - The writes average 0.8–0.9 KB (219–236k/s carrying 206–220 MB/s;
    `urnetwork_connect_h1plus_*`).
  - **The cost is per record, not per byte.** OpenSSL 3's per-call bookkeeping
    leads the libcrypto leaves: `BIO_ctrl`, `ERR_clear_error`,
    `EVP_CIPHER_CTX_get_iv_length`. AES-GCM itself is 0.7%.
- **Handshakes are negligible** (under 1.5%), and session tickets stay off.
- **The upstream to connect is loopback**, with host networking for every
  container.
- **The worker counts differ:** CPUOPT5 counted 480 workers, including the web,
  app and mmm containers' idle nginx workers; this round counts only the lb
  containers'.

### 4. What CPUOPT5's items did

None of Part A's items shipped. Connect's code moved little between a00dfeb9a and
aa7142599: randomness imports, an H3 test fixture and mover tweaks.
`redis.go` (unchanged since 10-28), the resident model and the reliability
model's hot path are unchanged. The client library moved (window and DNS changes
for the proxy's hosted windows), but not on connect's paths.

| CPUOPT5 item (fleet #) | Status at D47 | Measured now | Verdict |
|---|---|---|---|
| A §5.1 Terminate H1 in connect (#3) | Not done. Owner 11-13: "Not now"; not to be re-proposed without new evidence | nginx 26.7 → 44.5 cores (148k → 279k H1 sessions); it follows connect's H1+ writes, 0.22 → 0.19 cores per 1k writes/s | New evidence; owner item O1 |
| A §5.2 Logging and observability off the edges (#4) | Not done: no xops change since 11-12; Loki, Mimir and Grafana still run in `main-grafana-g1` on every edge | 11.9 → 13.6 cores. nginx access lines for successful control calls are about half of the edges' journal lines | Still worth 3–5 (#2) |
| A §5.3 GOMAXPROCS 16 → 10 on one g-block (#7; predicted 2–4) | Run on g4 11-13 ~20:00–21:30Z and ended early by the owner: inconclusive (g4 lost 60–75% of its providers at the restarts; edge-0/1 lower, edge-3 +30%). Kept at 16 | The g-processes run 5.7–8.9 cores now (8.9 on edge-0), at the A/B's revert line (90% of 10) | Not at this load (§7) |
| A §5.4 One reliability-stats write per shard per round (#12; predicted 0.5–1) | Not done: `RecordClientReliabilityStatsRange` (model/network_client_reliability_model.go:318) runs one MULTI/EXEC per connection per round | 0.1–0.5% of connect; Redis HINCRBY 39k/s, MULTI/EXEC 11.6k/s (35k and 8.5k in CPUOPT5) | Still valid, mostly Redis (#6) |
| A §5.5 GETEX for the resident heartbeat (predicted ≤1) | Not done: `GetResidentForClient` is still GET then EXPIRE (model/network_client_resident_model.go:49) | 1.2–1.5% of connect (3.7% in CPUOPT5's capture of the same process) | Still valid, smaller (#6) |
| A §5.6 Multiplexed links (#21; predicted 3–5) | Not done | 1.0–1.2M io frames/s still cross sockets; exchange trees 12–16% | Unchanged; high risk |
| A §5.7 Small items | Not done | `encodeUuid` 0.8–1.0%; `messagePool.take`/`release` 0.9–1.2% self; `measureEvidenceWithLock` 0.25–0.5% self | Unchanged (#8) |
| Fleet #2 stuck-provider signal, then recycle | Signal on for every api block since 11-14 01:09Z (stage 2); recycle off on every connect block, stage 3 not started (`sp_stages.log`) | No stuck-provider frames in connect's profiles | The api side is Part C's |
| E §5.1 No preflight PING (#9) | Not done | Redis PING 14.8k → 77.6k/s, mostly the close-listener takes; the PINGs are 2.5–4.1% of a g-process | Larger now (#3) |

### 5. New costs

No connect release added a cost: connect's code moved little from a00dfeb9a to
aa7142599 (§4). The new costs come with the sweeps' population (§3.1):
- the key-event resubscribe storm (§5.1), the largest;
- the timer heap and the GC, which grew with the population (§3.4).

#### 5.1 The key-event resubscribe storm (new; with Part E §5.1)

**How the subscription works**
- Every connect process keeps one keyspace-notification subscription on all 48
  Redis masters (`connect/key_event_subscriber.go`, `redis.go:714`; the code comment
  at `redis.go:765` still says 32).
- Node readers feed one merge channel of `MessageBufferSize` = 1,024
  (`DefaultRedisKeyEventSubscriptionSettings`).
- **No consumer during startup:** the merge is not drained at all until all 48
  masters have acknowledged the subscription.

**What happens when the merge is full**
- The node reader counts a drop (`urnetwork_redis_key_event_merge_drops_total`)
  and ends the epoch.
- The subscriber waits `ResubscribeTimeout` (5 s) and subscribes again.
- It then resyncs every registration:
  - **`resyncAll`:** one recheck per session listener, one peer read per network,
    and one reconcile per hop listener. These are trickled over 30 s and canceled
    by the next epoch's wave.
  - **`resyncCloseListeners`:** a kick to every resident's close listener over the
    same 30 s. **This wave is not canceled by the next one.**
  - **Each session group's revision read is nudged at once.** That read prunes the
    network's sessions, returns every live one, and `shared()` extends four keys
    (`session/session_model.go:87`).

**What feeds the loop**
- `extend()` refreshes a key whenever its TTL is short of the target. The
  inventory read's target, `now+86400000`, moves forward every millisecond, so
  every revision read PEXPIREATs `{ns_<network>}eid`.
- A PEXPIREAT on that key is a keyspace "expire" event. It matches the session
  pattern `{ns_*}eid` on all 20 processes, and `dispatch` then discards it
  (`key_event_subscriber.go:353`: "a ttl refresh (heartbeat), not a visible
  change").
- **The result is a closed loop:** each g-process's resyncs nudge its revision
  reads; those emit events to every process; the events overflow the g-processes'
  merges; and each overflow starts another resync.

**The storm since 11-14 16Z** (`resync_trend.txt`, `removal_delivery.txt`)

| Hours | Clients | Resubscribes/s (fleet) | Merge drops/s | SPOP/s | PING/s | EXISTS/s | PTTL/s | Redis commands/s | Redis process cores | Removed-flag events delivered* |
|---|---|---|---|---|---|---|---|---|---|---|
| 11-12 13Z – 11-13 19Z | 141–155k | 0.1–0.3 | 0.1–0.3 | 2.2–5.6k | 11–20k | 6.6–10k | 24–38k | 0.50–0.63M | 19.5–23.8 | 150–184% |
| 11-13 22Z (first sweep wave) | 291k | 1.6 | 2.1 | 36k | 55k | 30k | 85k | 1.04M | 32.6 | 106% |
| 11-14 01–13Z | 170–210k | 0.3–0.6 | 0.3–0.7 | 5.2–12k | 18–29k | 10–17k | 35–56k | 0.59–0.74M | 19.4–25.2 | 163–192% |
| 11-14 16Z – 11-15 10Z | 244–291k | 1.7–2.2 | 2.6–3.3 | 44–55k | 69–81k | 40–59k | 114–152k | 1.22–1.44M | 35.6–39.2 | 56–76% |

\* `urnetwork_key_events_dispatched_client_removed_total` over (remove-client
requests × 20 processes). Other paths set the flag too, so full delivery reads
about 160%, as it did before the sweeps.

- **Every g-process resubscribes every 7–9 seconds,** at troughs and peaks alike:
  1,649–2,024 times in the four hours to 10Z.
  - **The beta processes are stable,** with about 1,000 residents each: 16–40
    resubscribes in those four hours (edge-1's beta 402).
  - **Before the sweeps** the g-processes resubscribed every 1–4 minutes (64–297
    in 11-13 08–12Z).
  - **Failed starts:** in 30 minutes edge-1 g3 logged 96 failed starts (56 "redis
    key-event subscription ended", 40 "dial … operation was canceled";
    `keslog_e1.txt`).
- **The g-processes are subscribed only a small part of the time.** Their pub/sub
  readers barely register in perf (§3.4).
  - **Fleet delivery:** removed-flag events are delivered at 56–76% against the
    160% baseline, and the stable betas deliver theirs in full.
  - **What that leaves for the g-processes:** an estimated ~20% of the removed-flag
    events reach them.
  - **What replaces delivery:** peers, stream hops, session events and close
    requests wait for the resync waves and the 5-minute corrective poll.
- **Redis carries 0.8M more commands/s** (0.57M → 1.37M/s; 4-hour means at 11-13
  12Z and 11-15 10Z; `redis_cmd_compare.txt`).
  - **By command:** PTTL +113k, PEXPIREAT +83k, ZREMRANGEBYSCORE +75k, PING +63k,
    GET +62k, EVAL +56k, ZREVRANGE +51k, SPOP +49k, EXISTS +46k,
    ZRANGEBYSCORE +43k.
  - **Most of it is per-request overhead:** Redis process CPU rose 22.4 → 38.0
    cores, while command time rose only 8.4 → 9.3.
  - **What the extra requests are:**
    - the SPOP+PING pairs are the close-listener takes;
    - the PTTL, PEXPIREAT, TIME, EXISTS and ZREMRANGEBYSCORE are the session
      scripts' `extend`, `revision` and `prune`;
    - the ZRANGEBYSCORE/ZADD/HSET/ZREVRANGE are the connection-authority publish
      after each lease recheck.
  - Part E accounts for it on the Redis side (E §3.1, §5.1): 17.4 cores and about
    194k requests/s by its fit, plus 4.5–6 softirq cores. This section is the cause.

### 6. Ranked opportunities

#### #1 Stop the key-event storm

**The change.** Four steps, in this order. Each is its own commit with its own
deterministic test.
1. **Quiet the stream.** In `sessionLuaCommon`, extend a shared key to the target
   plus a margin, and only when its TTL falls below the target.
   - **The guarantee is unchanged:** a key never outlives less than it does today.
   - **The volume falls:** the PEXPIREAT on `{ns_*}eid`, and its keyspace event to
     all 20 processes, happens once per margin instead of once per read.
   - **The margin** is a script constant with one value.
2. **A subscriber that keeps up.**
   - Drain the merge while the startup waits for its 48 acknowledgements.
   - Dispatch in batches.
   - Raise `MessageBufferSize` (a settings field with one default) above the
     stream's burst over the acknowledgement window.
   - Keep the dispatch loop off the exchange's lock: queue the removed-flag
     handling to a worker, as close kicks already are.
3. **One close-listener wave at a time.** `resyncCloseListeners` cancels the
   previous wave, as `resyncAll` does. The take drops its preflight PING (#3).
   *(Part E rejects this step and the larger buffer in step 2: cancelling a wave
   leaves late listeners unkicked while waves come every ~8 s, and a larger buffer
   only delays an overflow (E §7). Both parts agree that step 1 comes first. See the
   fleet Decision 1.)*
4. **Count, in the same release:** messages by pattern class and event, and the
   dispatch time, so that the effect is measured rather than inferred.

**Evidence:** §5.1 and §3.4.

**Saving:**
- **In connect: 7–10 cores** (range 6–12) at sweep load *(inferred)*.
  - The resync-driven excess, ~0.5–0.9 cores per g-process above what the doubled
    population would cost at CPUOPT5's shares.
  - About 1 core per beta process as the stream shrinks.
- **Step 1 must land first.** A stable subscription on today's stream would cost
  each g-process what it costs a beta (~1.2 cores). Stabilizing alone could be a
  net loss.
- **In Redis:** most of the 0.8M commands/s the storm added. At Redis's overall
  ~28 µs per command that is roughly 10–15 Redis cores *(inferred)*. Part E's fit
  puts the storm at 17.4 Redis cores (E §3.1).

**Risk: low–medium.**
- Step 1 changes when a TTL is refreshed, not how long keys live.
- Steps 2–3 change buffering and wave scheduling, not what is delivered.

**Owner rules.** No timing bound moves:
- The resubscribe backoff, the resync spread, the 5-minute corrective poll and the
  lease intervals are unchanged.
- Events arrive instead of waiting for a wave, so detection gets faster.
- Per CODESTYLE.md, the buffer is a settings field with one default, and there is
  no new switch.

**Tests (deterministic):**
- **(a) Lua:** N inventory reads inside the margin issue one PEXPIREAT, and no
  read leaves the TTL below the target.
- **(b) Startup burst:** an in-memory node feed bursts more than the old buffer
  during the acknowledgement wait, and the epoch survives.
- **(c) Back-to-back resyncs** kick each close listener once per surviving wave.
- **(d) Non-blocking dispatch:** a removed flag dispatches while the exchange lock
  is held.

**Acceptance:**
- Resubscribes at or below 0.3/s fleet, and merge drops near 0.
- Removed-flag delivery back near 160%.
- SPOP under 6k/s and PING under 20k/s.
- Beta processes back near 0.6 cores, and g-process cores per frame down.

#### #2 Fewer log lines, then logging off the edges (CPUOPT5 #4)

**The change.**
- **(a) Access log:** warpctl's generated nginx config writes no access line for
  1xx/2xx answers on the api's control and hello routes, and keeps every 4xx/5xx
  line. `$warp_loggable` already does this per host for lag.dev; a route-and-status
  map is the same mechanism and a changed default, not a new setting.
- **(b) Connect log:** the `transfer_sequence_exit` INFO line goes to V(1) in the
  connect library.
- **(c) Placement:** move Loki, Mimir and Grafana off the edges, as CPUOPT5
  proposed.

**Evidence:**
- Logging and observability: 13.6 cores.
- edge-1's journal takes 3.6k lines/s: 2.7k/s from the lb (64% of the busiest
  container's lines are 2xx `POST /connect/...`, 18% `GET /hello`), and 0.93k/s of
  connect sequence exits.
- The journald pipeline (dockerd/containerd, journald/rsyslog, fluent-bit) costs
  1.7–1.9 cores per host, plus the Grafana stack's 1.1–2.5.

**Saving:**
- (a) and (b) remove ~75% of the edges' journal lines: 2–3 cores.
- (c) adds 1–2: **3–5** together.

**Risk: low.** The trade-off: successful control calls stop leaving lines in Loki.
That is the owner's call.

**Owner rules:** no timing bound; no new off-by-default setting.

**Tests:**
- A warpctl golden-config test: a 2xx control request maps to `$warp_loggable` 0,
  and a 4xx to 1.
- A glog-level test for the sequence-exit line.

#### #3 No preflight PING on connect's deadline and no-retry calls (E #2; CPUOPT5 E §5.1)

**The change.**
- Pass `OptRedisNoPreflight()` at `ReadContractHoleLease`
  (model/subscription_contract_hole.go:109) and at the other `RedisWithDeadline`
  sites without it (`git grep 'RedisWithDeadline('` lists them).
- Let `RedisDoOnce` take options, so that `TakeClientCloseRequests` stops
  preflighting.
- The option's own comment: the PING "guards no command's outcome".

**Evidence:** `server.redisWithClient` is 2.5–4.1% of the g-processes; Redis PING
went from 14.8k to 77.6k/s.

**Saving:** 3–4.5 cores now; 0.5–1 after #1 shrinks the takes. Plus the Redis
PINGs (E #2).

**Risk: low.**

**Owner rules:** each command keeps its own deadline.

**Test:** a counting fake: one call sends one command.

#### #4 Timer heap

**The change.**
1. **Measure:** a sampled counter of timer re-arms by call site and new-deadline
   distance, in connect and in the forked libraries, as CPUOPT4 did before the pgx
   and quic-go forks.
2. **Remove short re-arms** where a deadline can stay in place (CPUOPT4's pattern:
   arm once, check on fire).
3. **Fewer armed timers per idle client.** Each of the ~290k authorization leases
   holds a 60 s ticker and a horizon timer. One process-wide schedule of due
   rechecks gives the same cadence with no per-lease runtime timers.

**Evidence:** §3.4: `adjust` self 2.2–4.0%, sift 0.5–0.7%, and `check` 4.5–6.9%
inclusive, spread over many re-arm sites. Go 1.27 still scans the whole per-P heap.

**Saving:** 1.5–3 cores *(inferred)*.

**Risk: medium.**

**Owner rules:** the same deadlines and cadences. Acceptance requires recheck
latency percentiles unchanged.

**Tests:**
- Extend `resident_deadline_rearm_test.go` with re-arm counters.
- A fake-clock lease-schedule test: every lease rechecks within 60 s ± jitter.

#### #5 One session check per network event per process (after #1)

**The change.**
- After #1, measure the event-driven rechecks.
- If they still exceed the timer baseline by more than ~3×, read the network's
  revocation state once per event per process, and recheck at once only the
  leases that the event can affect.
- Unaffected leases keep their 60 s timer.

**Evidence:**
- About 65k connection-authority publishes/s (ZREVRANGE), against the ~4.8k/s that
  290k leases' 60 s timers need. That was already 5.6× before the sweeps.
- `kickSession` rechecks every lease of the network on every process.
- `AuthorizationLease.runChecks` is 2.7–3.9% of a g-process, and
  `PublishConnectionAuthority` 1.2–1.8%.

**Saving:** 1–3 cores *(inferred)*, plus Redis EVALs.

**Risk: medium:** this is the revocation path.

**Owner rules:** affected leases still recheck at once. The 60 s recheck and the
90 s lease stay.

**Test:** with a fake session store:
- revoking one session in a network of N leases rechecks the affected leases at
  once and no others;
- a network-wide revocation rechecks all of them.

#### #6 GETEX heartbeat; one reliability flush per shard per round (CPUOPT5 A §5.4–5.5; with E #7)

Unchanged in substance.
- `GetResidentForClient` is 1.2–1.5% and `RecordClientReliabilityStatsRange`
  0.1–0.5%.
- Redis: HINCRBY 39k/s, MULTI/EXEC 11.6k/s.
- **Saving:** 1–2 connect cores, plus Redis. **Risk: low.**
- **Owner rules:** the 75 s heartbeat, the 300 s TTL and the 1-minute blocks stay.
- **Tests:** as CPUOPT5. The per-block sums equal today's against a shadow sum.

#### #7 Multiplexed links (CPUOPT4 #21)

Unchanged.
- **Evidence:** 1.0–1.2M io frames/s still cross sockets, and the exchange trees
  are 12–16%.
- **Saving:** 3–5. **Risk: high:** protocol version, mixed-version rollout,
  head-of-line blocking.

#### #8 Small items

- `encodeUuid` through fmt: 0.8–1.0%.
- `messagePool.take` and `release`: 0.9–1.2% self.
- `windowPacingService.measureEvidenceWithLock`: 0.25–0.5% self.
- Together 0.5–1 core, low risk. Each change comes with a benchmark and an
  equivalence test.

#### Owner items

**O1. H1 termination in connect (CPUOPT5 #3; "Not now" on 11-13).** The new
evidence, for the owner to weigh:
- nginx went from 26.7 to 44.5 cores (+67%).
- It tracks connect's H1+ writes, at 0.19–0.22 cores per 1k writes/s.
- The 72-CPU edges reach 74–79% busy at 5-minute peaks.

By CPUOPT5's method the saving would be **13–20 cores** *(inferred)*:
55–65% of nginx is relay work on the upstream side, less Go's record work at
~30% of nginx's equivalent. The risk is high, as before: ports, rate limits and
the deploy port rotation move into connect.

**O2. Window clients held by idle hosted devices (product decision; Part B's
proxy).**
- **Now:** 151k proxy window clients in 85k windows, for 45.7k live hosted devices
  of which 13.7k are active. On 11-13 at 12Z there were 14.5k windows for 13.8k
  devices.
- **If only active devices held windows,** connect would hold about 100k fewer
  clients.
- **Saving: roughly 15–25 connect cores** plus part of nginx *(inferred)*. That
  uses the sweep era's 0.04–0.07 cores per 1,000 exchange connections at ~3.3 per
  hosted client, plus the population's share of the GC, timer and scheduler costs.
- **The trade:** the first request after idle would be slower. The owner keeps the
  45-minute idle hold, so this is not proposed, only sized.
- **Overlap:** B #1 trims a held device's windows to one exit instead, and prices
  the released clients at CPUOPT5's lower per-client rate (B #1).

### 7. Considered and rejected

As CPUOPT5 A §6 and CPUOPT4 A §7, unchanged:
- no longer exchange, H1 or resident deadlines;
- no slower lease or announce cadences;
- no coarser go-redis deadlines;
- no lazy deadline re-arm;
- no relaxed service-runaway ceiling.

Also rejected this round:

- **Changing the key-event bounds.** The 5 s `ResubscribeTimeout`, the 30 s
  `ResyncSpreadTimeout` and the 5-minute corrective poll bound how stale a lost
  event can leave a listener. A resync after every gap is required, because events
  published while unsubscribed are gone. #1 stops the gaps instead.
- **A longer authorization lease.** It stays at a 60 s recheck and a 90 s lease
  (session/authorization_lease.go:24–25): these are revocation bounds.
- **GOMAXPROCS below 16 at this load.**
  - The busiest g-processes reach 8.9 cores, so a cap of 10 would run at the A/B's
    revert line.
  - Re-run it only when the sweeps end, and account for the provider
    redistribution that the 11-13 A/B lacked.
- **GOGC above 100.** The GC runs at the runtime's 2-minute floor (cadence
  127–129 s; heap goals ~74 GB against 37–59 GB live), so GOGC would change nothing.
- **The conntrack bypass on the edges** (NOTRACK for loopback and exchange
  traffic).
  - `nf_hook_slow` is 3.8–5.2% of connect and 8.3–8.7% of nginx inclusive, roughly
    5–8 cores.
  - The owner declined the same mechanism for the Redis hosts on 11-13, so it is
    not re-proposed.
- **AppArmor unconfined for the lb and connect containers.**
  - AppArmor is 1.9–2.5% of connect inclusive, and `aa_inet_msg_perm` 0.9–1.1% of
    nginx's leaves: about 3 cores.
  - It is a security posture change, listed for awareness only.
- **Coalescing connect's H1 writes with a delay.** Writes average 0.9 KB and each
  becomes one TLS record in nginx. A delay would cut nginx and kernel work, but it
  adds latency to every message, so the timing rule rules it out.
- **A larger `proxy_buffer_size`, or kTLS in nginx.** With 0.9 KB writes the buffer
  is never the limit, and kTLS moves the per-record work into the kernel rather
  than removing it.
- **Stabilizing the subscription without quieting the stream first.** It would put
  each g-process at a beta's ~1.2-core stream cost (#1 step order).
- **Shorter hosted-device holds, or an earlier D49p close.** These are product
  decisions: the owner kept the 45-minute idle hold, and D49p's 10 minutes is
  decided.

### 8. Noticed, not CPU

- **Key events are lost most of the time** on every g-process since 11-14 16Z
  (§5.1).
  - Removed flags, peer and stream-hop changes, session events and close requests
    wait for a resync wave or the 5-minute poll.
  - That is a timing regression in its own right, and #1 fixes it.
- **All 20 connect containers were recreated in place at 11-15 01:11–01:13Z.**
  - Same build. On edge-0 it took one block every ~22 s: beta, g4, g1, g2, g3.
  - It was the config release `2026.11.14-outerwerld+1099044420` (config a5f2c85,
    crypto_pay.yml), published to the hosts at 01:11Z: every service restarted, one
    block at a time, at the same builds.
  - Every 11-15 number here comes from processes 9–10 hours old.
- **Headroom.**
  - edge-1, edge-3 and edge-4 reach 74–79% busy at 5-minute peaks.
  - In the 10 s census, the per-CPU busy p90 was 87–88% and the maximum 88–93%.
- **nginx load sits on one lb container per host on edge-0 and edge-1.**
  - edge-0: 9.6 of 10.9 cores on eno4np3. edge-1: 8.8 of 10.3 on eno2np1.
  - edge-3 and edge-4 split theirs over two containers.
  - Each container has 25 workers, none near saturation.
- **TCP:** each host showed 0.7–1.3k established resets/s, ~0.6k TCP timeouts/s
  and ~0.9% retransmitted segments in the census. Not investigated.
- **Leftover files.** Part C's script files `/tmp/cpuopt6c_perf_*` remain on edge-0,
  edge-1 and edge-4 (§2). They should be removed.

## Part B — Proxy hosts and the proxy service

Domain: crisp and fireside (24 CPUs each, Intel Core Ultra 9 285HX) and the 20 proxy
processes (10 per host), with alt and WireGuard on the same hosts. Measured read-only
from Mimir, 11-12 00Z to 11-15 10:30Z, two perf captures (§2) and a `/proc` census of
both hosts. The proxy runs `2026.11.14-outerwerld+1098770070` (D49p: server f43488da1,
connect fc15cc5912ea, sdk d9ad8a26c71b) on all 20 processes since 18:38:47Z on 11-14;
every process restarted at the same version 01:11–01:14Z on 11-15, with the config
release (§8). Raw outputs and
scripts are in `<scratch>/B/`.

### 1. Summary

- **The proxy is the fleet's surge again, and this time the cause is memory, not a
  loop.** In the 127 five-minute samples since D49p in which the two hosts together
  were at 44 or more of 48 cores busy, the proxy used 44.9 cores and the hosts 47.4.
  Between sweeps it used 17.1 cores; on 11-13's quiet morning, 4.9.
- **Each process sits at its Go memory limit.** At sweep load a process holds about
  2,400 live devices, 7,350 window clients and 266k goroutines: 4.8 GB of heap in use
  and 1.9 GB of goroutine stacks. Its runtime memory is 6.75 GB against the 6.82 GB
  `GOMEMLIMIT` (6500 MiB), so the collector runs 12 times a minute, against 0.6 on 11-13.
  In the two sweep captures, one per host, GC took 58–60% of the process. On crisp:
  background marking 26%, mutator assists 26%, write-barrier flushes 5%, sweeping and
  the rest 3%.
- **There is a cliff.** With heap allocated plus stacks under about 4.9 GB per
  process, GC ran 0.6–1.2 times a minute, as on the evening of 11-13, when the first
  sweeps already held 84k window clients at 9.4 proxy cores. On average each further
  0.35 GB doubles GC's frequency, up to 12 a minute at 6.0–6.3 GB. The same window
  clients cost 0.11 mcores each in that evening's sweeps and 0.31 now.
- **Per device opened, the proxy spends about 2.6 CPU-seconds at sweep load:**
  - GC, 1.5;
  - the device's window clients, held for its whole life, 0.56;
  - about 10 window builds, 0.18 (find-providers, client mint, contract, dial);
  - the scheduler, 0.12;
  - the device's own tun, gVisor stack and DoH upkeep, 0.07;
  - constructing the device, about 0.01.

  WireGuard is none of it: the bots use SOCKS and HTTP, and 14–17 WireGuard sessions
  were live fleet-wide. The mean device lives about 40–55 minutes.
- **CPUOPT5's Part B items:**
  - The verified-chain cache (D4(b)) shipped with D47p and hits about 100%.
  - The JWT claims cache (D4(a)) shipped too. Its misses are new tokens, about two per
    window build, not capacity.
  - One origin-notification set per process (CPUOPT4 B #4) is not done. It is now 18 of
    each device's 51 own goroutines.
  - The `failed_ping` share of window candidates fell from 70% to about 13%.
  - The owner's D47p (lazy open) and D49p (10-minute close for devices that carried no
    traffic) each cut live devices at their rollout. Sweeps that send a little traffic
    through every device now take the 45-minute hold anyway (§4).
- **The opportunities all cut memory held per device or allocation per frame, without
  refusing anyone** (§6):
  1. Trim a held device's windows to one exit.
  2. One origin-notification set per process.
  3. A per-host memory limit on crisp.
  4. Contract stats only for listeners.
  5. Fewer per-frame allocations.

  Together they are worth roughly 20–28 of the 45 sweep cores, most of them by taking
  GC off its limit, plus 2.5–4.5 connect cores. They overlap; don't add them.

### 2. Method

- **Mimir** (`127.0.0.1:3100`), 5-minute rates, 11-12 00Z to 11-15 10:30Z:
  - per-process `process_cpu_seconds_total` and the Go runtime series;
  - `urnetwork_proxy_*` devices, windows, candidates, closes, sessions and WireGuard;
  - `urnetwork_findproviders2_*`, contract, admission and cache counters;
  - `node_cpu_seconds_total` and memory for both hosts;
  - the planetoid proxy-test phases.

  The D49p period is split by host load: sweep is the two hosts together at 44 or more
  of 48 cores busy (127 samples); between is under 30 of 48 (39 samples).
- **Device opens** are estimated as the change in live devices plus the idle closes
  (`urnetwork_proxy_devices_closed_idle_total{use}`) and removal closes. No counter
  counts opens.
- **Capture 1, sweep:** crisp g5 (pid 2500025), `perf record -F 99 -g -p`, 30 s from
  10:11:51Z on 11-15.
  - 6,900 samples, or 2.30 cores; the 3-minute rate was 2.15.
  - The host was 23.8 of 24 busy.
  - The validator read intent 26354 finalized, with 1,620 s to the next boundary
    (checked 10:11:50Z).
- **Capture 2, meant for between sweeps, taken in a sweep:** fireside g5
  (pid 2470127), same settings, 30 s from 11:19:58Z.
  - 7,319 samples, or 2.46 cores; the host was 23.0 of 24 busy.
  - The validator read intent 26355 finalized, with 1,860 s to the boundary.
  - No dip came while the validator was idle. Both hosts stayed at 23–24 of 24 busy
    from 09:18Z until the capture, by live `/proc/stat` sampling of fireside every 30 s
    from 10:48Z.
  - The dips that instant Mimir reads seemed to show at 10:32–10:47Z were the newest,
    still-incomplete sample.
  - So the second capture is a sweep capture on the other host. For quiet hours,
    CPUOPT5's 11-13 capture stands (§3.3).
- **Captures, common to both:**
  - Stacks were folded on the host and copied back.
  - Data files deleted, confirmed with `ls`; no perf process left.
  - Root through `by-pass <host> by | sudo -S`, never printed.
- **Census:** both hosts at 10:23–10:24Z in a sweep: `/proc` (meminfo, sockstat,
  softirqs, interrupts, cpufreq), `ss` connection counts by peer, and `vmstat`.
- **Code** read at the D49p build's inputs: server f43488da1, connect fc15cc5912ea,
  sdk d9ad8a26c71b. Proxy limits were read from vault `main/services.yml`.
- Nothing was changed in production. The permission system refused nothing.

### 3. Where the CPU goes now

#### 3.1 Work and cost by window

Fleet, both hosts, 5-minute data (`<scratch>/B/ds5m.tsv`, `dsmem5m.tsv`, `dsadm5m.tsv`).
Builds are admitted window candidates; heap, stacks, goroutines, GC and allocation are
per process.

| Window (build) | Busy of 48 | Proxy cores | Live devices | Active | Window clients | Goroutines | Heap / stacks GB | GC/min | Alloc MB/s | Builds/s | FP2/s | Contracts/s | Mint refusals/s | User MB/s | Proxy-test setup p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 11-13 00–12 quiet (89be8b3b8) | 7.0 | 4.9 | 13.6k | 5.4k | 13.6k | 46k | 1.98 / 0.34 | 0.61 | 10 | 12 | 4 | 49 | 0.1 | 19.0 | 1.8 s |
| 11-13 18–24, first sweeps (89be8b3b8, open at creation) | 12.5 | 9.4 | 27.4k | 6.9k | 84.3k | 148k | 4.03 / 1.16 | 0.82 | 22 | 56 | 56 | 150 | 1.9 | 18.8 | 2.1 s |
| 11-14 13:20–18:30, D47p (lazy open) | 29.1 | 26.1 | 39.6k | 7.1k | 130k | 225k | 4.33 / 1.70 | 6.1 | 36 | 117 | 78 | 242 | 68 | 20.0 | 3.2 s |
| 11-14 18:40 to 11-15 10:10, D49p, all | 40.7 | 38.0 | 43.9k | 11.8k | 144k | 251k | 4.66 / 1.85 | 10.1 | 50 | 161 | 173 | 300 | 343 | 13.4 | 5.8 s |
| D49p, sweep samples | 47.4 | 44.9 | 48.4k | 12.5k | 147k | 266k | 4.82 / 1.94 | 12.1 | 55 | 181 | 208 | 319 | 477 | 11.6 | 6.8 s |
| D49p, between samples | 20.2 | 17.1 | 32.0k | 9.9k | 126k | 203k | 4.21 / 1.58 | 3.8 | 36 | 104 | 90 | 240 | 58 | 17.9 | 3.6 s |

| Window | mcores / live device | mcores / window client | CPU-s / MB of user traffic |
|---|---|---|---|
| 11-13 quiet | 0.36 | 0.36 | 0.26 |
| 11-13 first sweeps | 0.34 | 0.11 | 0.50 |
| D47p | 0.66 | 0.20 | 1.30 |
| D49p sweep | 0.93 | 0.31 | 3.85 |
| D49p between | 0.53 | 0.14 | 0.96 |

- **The sweeps are near-continuous, not hourly blips.** Since D49p went live the two
  hosts together were at 44 or more of 48 cores busy in 68% of five-minute samples.
  They ran saturated from 19:40Z to 01:10Z, from 02:40Z to 06:20Z and most of
  08:40–10:30Z, with dips of 10–20 minutes between.
  - Live devices swung between 17k and 85k; active devices (traffic in 10 minutes)
    between 6k and 26k.
  - From 20:00Z on 11-14 the bots began to send traffic through their devices: active
    devices rose from 6–7k to 10–12.5k. Since 22Z, 55–67% of idle closes each hour are
    `used` (45-minute hold) rather than `unused` (10-minute).
- **Users pay for it.** User traffic through the proxy fell from 19 MB/s to 11.6 MB/s in
  sweeps, and the planetoid proxy-test medians, in 3-hour means, rose:
  - setup, from 2.1–2.3 s to 5.4–8.8 s;
  - page load, from 7.5–9 s to 11–16 s;
  - connect, from 1.4 s to 2.1 s.
- **The cost is per window client, and the price per client tripled with memory.** The
  first sweeps of 11-13 already held 84k window clients and cost 9.4 cores, 0.11 mcores
  per client. The same kind of client costs 0.31 mcores in today's sweeps. What
  changed is memory per process (heap in use plus stacks):
  - 5.2 GB then against 6.8 GB now;
  - GC 0.8 against 12.1 times a minute.

  User-space time is 89% of crisp's busy time in sweeps (user 21.1 cores, system 1.2,
  softirq 1.5).
- **Load passed on to other parts.** The proxy's PostgreSQL pool acquires went from
  913/s to 5,315/s in sweeps and its Redis pool hits from 1.1k/s to 9.0k/s (Parts D and
  E). Each window client is also one connect client on the services edges (Part A):
  connect held 281k clients at 10:45Z on 11-15, against 149k on 11-13's morning, and
  155k of them were hosted window clients.

#### 3.2 Runtime shape and the memory cliff

| Per process | 11-13 quiet | D49p sweep |
|---|---|---|
| Cores | 0.25 | 2.25 |
| Live devices / window clients | 680 / 680 | 2,420 / 7,350 |
| Goroutines | 46k | 266k |
| Heap in use / stacks | 1.98 / 0.34 GB | 4.82 / 1.94 GB |
| Runtime memory (sys − released) / `GOMEMLIMIT` | — | 6.75 / 6.82 GB |
| GC cycles a minute | 0.61 | 12.1 |
| Allocation | 10 MB/s, 118k objects/s | 55 MB/s, 561k objects/s |
| RSS | 2.69 GB | 6.43 GB |

- **The limit:** `GOMEMLIMIT` is 6500 MiB, with a 10 GiB cgroup cap per container
  (vault `main/services.yml`); GOGC is 100 and GOMAXPROCS 4.
  - The cap kills the container, since cgroup v2 ignores warpctl's
    `--oom-kill-disable`.
  - fireside OOM-killed proxies on 10-23 and 10-25, which is why the limit came back
    down from 7000.
- **Where the cliff sits.** Measured by heap allocated plus stacks per process, the
  collector ran (Go 1.27.4):

  | Heap allocated + stacks | GC cycles a minute | When |
  |---|---|---|
  | 4.5 GB | 0.6 | 11-14 06Z |
  | 4.9 GB | 1.2 | 11-13 20Z |
  | 5.3 GB | 5.6 | 11-14 14Z |
  | 6.0–6.3 GB | 11.9–12.3 | 11-15 04Z and 11-14 20Z |

  Near the limit, each 0.35 GB of memory roughly doubles the collection rate.
- **Memory per device**, by multiple regression over 4,640 process samples since 11-14
  15Z (goroutines r² 0.98, stacks 0.80):
  - goroutines: 51 per live device plus 18.5 per window client;
  - stacks: 220 KB per device plus 116 KB per window client;
  - heap: about 560 KB per device plus about 100 KB per window client (r² 0.65, so a
    looser estimate).

  A device with today's 3.35 window clients holds about 1.5 MB, half its own and half
  its windows.
- **Hosts:**
  - crisp has 134 GB, 51.5 GB available at 10:23Z and 41.9 GB at its 24-hour minimum.
  - fireside has 100.6 GB, 19.3 GB available and 9.0 GB at its minimum.
  - The ten proxies take 63 GB of RSS on each host and alt 6.4–6.6 GB more.

#### 3.3 Inside one process

The sweep capture, crisp g5, 30 s at 99 Hz from 10:11:51Z on 11-15, 6,900 samples
(2.30 cores) (`<scratch>/B/prof/sweep-crisp-g5.folded.gz`). The process held 2,118
devices, 7,099 window clients and 249k goroutines, with 4.62 GB of heap and 1.84 GB of
stacks, and collected 11.2 times a minute.

| Component | Share |
|---|---|
| **GC, total** | **60.2%** |
| background mark workers | 25.9% |
| mutator assists, inside the allocating goroutines | 26.3% |
| write-barrier buffer flushes | 5.2% |
| sweeping, scavenging and the rest | 2.8% |
| goroutine stack scanning (`scanstack`, within the above) | 10.2% |
| `runtime.tryDeferToSpanScan` (self; the Go 1.27 span-scan marking loop) | 23.0% |
| **Scheduler and other runtime goroutines** | **4.5%** |
| **Mutator, net of assists** | **35.3%** |
| window clients' send and receive sequences | 9.9% |
| window clients' run loops (channel, client, IP-association goroutines) | 6.0% |
| platform H1 transports (framing; utls record I/O 4.4% with its assists) | 4.5% |
| contracts and control (`ConnectControl`, `ContractManager`) | 3.3% |
| window builds: dial and TLS | 2.7% |
| window builds: find-providers (in-process `model.FindProviders2`) | 2.5% |
| device: tun, gVisor, DoH, `ProxyDevice.Run` | 2.8% |
| Redis client, SOCKS/HTTP ingress, sdk and other | 3.6% |
| Kernel (leaf frames, included in the rows above) | 11.5% |

**Findings from the capture:**
- **In-process control (`localclient.Authority`) is 8.7% with its assists:**
  - `FindProviders2`, 4.2%;
  - `ConnectControl`, 3.4%, mostly contract creation;
  - client mint (`AuthNetworkClient`), 0.6%;
  - `RemoveNetworkClient`, 0.35%.
- **Crypto is small:**
  - net/http's own TLS handshakes (`persistConn.addTLS`), 1.7%, of which x509
    `buildChains` is 0.91%. This path isn't covered by the verified-chain cache.
  - utls handshakes of the platform transports, 0.5%. The chain cache covers these.
  - ECDSA verification, 0.84%; JWT parsing, 0.25%.
- **The largest allocation sites, with their assists:**
  - the sdk's contract stats and details (`deviceContractTracker`, `encodeUuid`,
    `fromConnect`), 3.65%;
  - connect's `Framer.ReadHeader`, 2.9%: a 4-byte header that escapes to the heap on
    every frame read;
  - connect's `emitContractStats`, 2.65%;
  - utls `UConn.Write`, 1.6%;
  - `sendingChannelCount`, 0.8%;
  - `ownIpPacketFlow`, 0.7%.

  Sync pools are emptied at every collection, so at 12 cycles a minute pooled buffers
  are allocated again, which feeds the loop.
- **The kernel:** about 6% of the process's samples are NIC softirqs taken in its
  context. `inet_hash_connect` (the ephemeral port search) is 0.54% and conntrack 0.6%.
- **Against CPUOPT5's quiet capture** (crisp g5, 11-13 04:50Z, 0.21 cores): there, GC
  was 0, the scheduler 19.9%, the kernel 22.5% and connect's window machinery 69%. The
  sweep's mutator mix is the same machinery multiplied by device count.

#### 3.4 The second capture: fireside, also in a sweep

fireside g5, 30 s from 11:19:58Z, 7,319 samples (2.46 cores), validator idle
(`<scratch>/B/prof/sweep-fireside-g5.folded.gz`). The process held 1,913 devices,
7,881 window clients and 253k goroutines, with 4.54 GB of heap and 1.85 GB of stacks,
and collected 11.3 times a minute. The fleet ran 43.5 proxy cores. No between-sweeps
window opened while the validator was idle (§2).

| Share of the process | crisp g5, 10:12Z | fireside g5, 11:20Z |
|---|---|---|
| GC, total | 60.2% | 58.2% |
| background marking / assists / write-barrier flushes | 25.9 / 26.3 / 5.2% | 21.1 / 30.7 / 4.4% |
| `scanstack` / `tryDeferToSpanScan` (self) | 10.2 / 23.0% | 10.0 / 23.6% |
| Scheduler and other runtime goroutines | 4.5% | 4.6% |
| sdk contract stats tracker, with assists | 3.65% | 4.84% |
| `Framer.ReadHeader`, with assists | 2.9% | 3.2% |
| `localclient.Authority` / of which find-providers | 8.7 / 4.2% | 6.5 / 2.2% |
| Device construction (`NewPlatformDeviceLocal`) | 1.9% | 2.3% |
| utls (record I/O and handshakes) | 4.9% | 5.7% |
| net/http TLS (`addTLS`) / x509 / ECDSA | 1.7 / 1.07 / 1.26% | 1.64 / 0.94 / 1.16% |
| `inet_hash_connect` (ephemeral port search) | 0.54% | 1.61% |
| Kernel leaf frames | 11.5% | 12.7% |
| WireGuard | 0 | 0 |

- **The two hosts agree:** about 58–60% GC with a tenth of the process scanning
  goroutine stacks, and the same allocation sites at the top.
- **fireside spends three times crisp's share in the `connect()` port search** (1.6%).
  It holds 31.2k connections to edge-0's 65.49.70.71:443 against a 41k-port range (§8).

#### 3.5 Per device opened

At the sweep capture the fleet closed 18.1 devices a second against a flat live count,
so it opened about 0.9 per process per second. With 2,118 live devices, the mean life
is about 39 minutes. Over the D49p period, opens were 10–15 per second fleet-wide
against 40–45k live, a mean life of 45–55 minutes.

| Per device opened (crisp g5 at sweep load) | CPU-s | How |
|---|---|---|
| GC | 1.54 | 60.2% of 2.30 cores ÷ 0.9 opens/s |
| Window clients' upkeep (3.35 clients × 0.071 mcores × 2,350 s) | 0.56 | sequences, client loops, H1 transports, contract renewals |
| Window builds (about 9.6 per open × 19 ms) | 0.18 | find-providers, mint, contract, TLS dial, evaluation |
| Scheduler | 0.12 | |
| Device's own tun, gVisor and DoH upkeep (0.030 mcores × 2,350 s) | 0.07 | |
| Redis client, ingress and other | 0.09 | |
| Constructing the device (`NewProxyDevice`, `NewPlatformDeviceLocal`) | about 0.01 | JWT load and mint, local authority, DeviceLocal, gVisor tun |
| WireGuard keys and handshakes | 0 | no WireGuard frames in the capture; 14–17 sessions fleet-wide |
| **Total** | **about 2.6** | 2.30 cores ÷ 0.9 opens/s |

- **The same capture by the categories asked for**, as shares of the process with
  their assists (they overlap):
  - GC, 60.2%;
  - contract creation and control (`ConnectControl`, `ContractManager`), 7.2%;
  - find-providers, 5.0%;
  - client mint and removal, 0.95%;
  - window-client setup (`newMultiClientChannel`, `NewClientWithTag`), included in
    builds and loops;
  - crypto, 3.5% (TLS handshakes 2.2%, ECDSA verification 0.84%, JWT parsing 0.25%,
    key exchange 0.36%), plus utls record encryption inside the H1 transports' 4.4%;
  - kernel, 11.5% of samples, spread over the rows above: NIC softirqs in the
    process's context about 6%, `connect()` port search 0.54%, conntrack 0.6%,
    `tcp_sendmsg` 1.3%;
  - WireGuard, 0.
- **A window client costs 0.071 mcores of mutator time.** That matches the 0.07 mcores
  of 11-13's first sweeps, when GC wasn't limit-paced. A build costs 19 ms of mutator
  time; CPUOPT4 put a candidate at 15–35 ms.
- **The ~10 builds per open are not just the first fill.** Window clients are removed
  and replaced at 130–200 a second, against 10–12 on 11-13:
  - `unhealthy`, 60–98/s (against 1.6–2.1);
  - `verdict`, 44–57/s (7–8);
  - `closed`, 23–46/s (2);
  - handover, 4–6/s.

  A window client now lasts about 14 minutes on average, against about 21 on 11-13.
  CPU starvation isn't the cause: the per-client `unhealthy` rate had already
  quadrupled by 11-14 12Z, when the hosts were 28% busy. See #7.
- **Mint refusals:** 140–1,150 a second fleet-wide in sweeps, against 0.1 on 11-13. The
  two-registration-per-process bound (`defaultHostedMintConcurrency`) refuses at once,
  so a refusal is cheap. It is also what kept window clients per process at 3.5–8k
  while live devices per process reached 4.2k at 21:00Z on 11-14.

#### 3.6 Hosts (census 10:23–10:24Z, in a sweep)

- **CPU:** Intel Core Ultra 9 285HX, 24 cores without SMT. intel_pstate HWP
  `powersave` held cores at 4.4–4.7 GHz under load, with boost on, so there is no clock
  problem of edge-5's kind.
  - Load average 41 on 24 cores; `vmstat` shows 62–89% user, 8–11% system, 109–186k
    context switches a second.
- **Sockets:** about 99k established TCP connections on each host.
  - 31.6k (crisp) and 31.2k (fireside) go to edge-0's 65.49.70.71:443, and 21.4k and
    21.3k to edge-1's 65.49.70.70:443. These are the window clients' H1 transports.
  - The ephemeral range is 20000–60999, 41k ports per source and destination pair, so
    the busiest pair uses 77% of it (§8).
  - Each host also holds 16.5k Redis connections, 11.0k to edge-6 and 5.5k to edge-5,
    about 1.65k per proxy process.
- **NICs:** edge traffic leaves on eno1np0 (ice, 24 queues, interrupts spread). The
  private LAN (Redis and the rest) is on eno3 (r8127): 75 MB/s of receive on crisp,
  taken by 2 of its 32 queues, on CPUs 10 and 18. ksoftirqd/10 averaged 3.4% of a core.
- **Co-tenants:** alt (0.67 cores between sweeps and 0.90 in sweeps across both hosts),
  and Loki (0.2–0.3 of a core) and Mimir (0.13) on each host, together 3.3–3.4 GB of
  RSS per host.

### 4. What CPUOPT5's items did

| CPUOPT5 item (Part B, and fleet items that touch the proxy) | Status | Measured |
|---|---|---|
| B §4.1 Fewer proxy processes | Not done | Not a lever now: the memory is per device, and fewer processes would hold the same total. |
| B §4.2 One ContractOriginNotifications per process (CPUOPT4 B #4) | Not done | Each hosted device's `localclient.Authority` still builds its own (`model.NewContractOriginNotifications`). That is 16 bucket and 2 publisher goroutines, 18 of the device's 51 own goroutines, plus a 1,024-entry queue (32 KB). A non-terminal route refusal adds a 16,384-slot ring (about 0.8 MB), but those are rare (about 0.17 refusals stored a second fleet-wide, most of them terminal). At 2,118 devices that is 38k goroutines (15% of the process) and about 0.15–0.35 GB. Worth 0.1–0.2 cores on 11-12; now #2. |
| B §4.3 Track the `failed_ping` candidates with the FP2 S2 labels | Done in D47p: window metrics (dd73d29a1); hosted windows ask for every provider with ipv4 and any protocol, with no limited window (9dae31a7f) | `failed_ping` 27–28/s → 14–27/s while builds rose 15×: from 70% of candidates to about 13% in the last hour. The quality windows average 3.06 clients against their target of 5; 17.7k of 40.8k are below their minimum. |
| Fleet #5 D4(b) verified-chain cache | Shipped: connect's cache, counters exported by server 28d57da44; in the proxy from D47p | 280–470 hits/s, about 0.01 misses/s. The utls handshakes of the platform transports cost 0.5% of a process. net/http's own TLS path is not covered: x509 `buildChains` is still 0.91% (#6). |
| Fleet #5 D4(a) JWT claims cache | Shipped (970dccd8c; in the proxy from D47p) | 830–1,250 hits/s and 310–385 misses/s fleet-wide. The misses are about two per window build: new tokens, plus re-verification after the 10-minute maximum age. The cache sits at its 64Ki bound with 230 evictions/s only because ended entries leave by LRU eviction; a process's live tokens (about 9k) fit with room to spare. JWT parsing is 0.25% of a process. |
| CPUOPT4 B #2 GOMEMLIMIT 6500 → 7000 MiB | Not changed (6500 MiB) | Moot on 11-12 (heap 1.9 GB, GC at the 2-minute floor). Binding now: runtime memory 6.75 of 6.82 GB, and GC is 60% of a process in sweeps (#3). |
| Fleet #7 GOMAXPROCS A/Bs (per-block `cores`, warp fc8912f2f) | Mechanism available; the proxy stays at 4 | Not useful now: the hosts are saturated, the work doesn't shrink with fewer Ps, and the gVisor processors are 4 goroutines per device (§7). |
| PgBouncer direct path | The proxy stays on the single port (stage 4 not reached) | Acquires 913 → 5,315/s in sweeps (Part D; D #1). |
| Owner 11-13 b: lazy device open, D47p | Shipped: server 4daaba07f, 10:44–13:18Z on 11-14 | At the roll, live devices 30.9k → 17.7k and proxy find-providers 161k/h → 98k/h: devices of unused new proxies stopped opening. Sweeps that sign in to every proxy then opened them anyway: 40–50k live by 15–17Z. |
| Owner 11-14: 10-minute close of devices that carried no traffic, D49p | Shipped: server f43488da1, g6 17:57Z, all 18:38:47Z | Live devices 52.0k → 16.8k twenty minutes after the batches, find-providers −80%. Since 01:11Z, 134k `unused` and 197k `used` idle closes: the 4.1 unused closes a second each save 35 minutes, about 8.7k live devices (17%) below what the 45-minute hold would hold. From 20Z the bots carried traffic in most devices, which earns the 45-minute hold, so the sweeps came back larger (§3.1). |

### 5. New costs

The proxy's new cost is the sweeps' memory, not a release:
- **GC at the memory limit:** 0.6 → 12 collections a minute per process, 58–60% of
  a process in the sweep captures, about 27 cores across the fleet in sweeps (§3.2,
  §3.3, §6).
- **Window clients held for the whole of a device's life,** and replaced at 130–200 a
  second against 10–12 on 11-13 (§3.5).
- **D47p and D49p cut live devices at their rollouts** (§4); sweeps that send a
  little traffic through every device now earn the 45-minute hold anyway.

### 6. Ranked opportunities

These are fleet cores at sweep load (D49p sweep samples, 44.9 proxy cores) and between
sweeps (17.1). A quiet day like 11-13 gains little from any of them. The ranking
weighs saving against risk: #2 sits ahead of #3, although #3's range is higher,
because #2 needs no ops change, helps both hosts and carries no memory risk.

The reason all five help is the cliff in §3.2. A process needs about 1.2 GB less heap
and stacks at sweep load to get back under 4.9 GB, where GC ran about once a minute.
- **The first 0.35 GB of relief halves the collection rate; the next 0.35 GB halves it
  again.**
- **That is why memory items are worth more than their direct CPU.** The ~27 GC cores
  in sweeps are the prize. On top of it, released window clients save 0.071 mcores of
  mutator time each.
- **Some freed CPU will become service, not idle time.** The proxy is CPU-starved in
  sweeps, so freed CPU first shows up as user traffic, faster proxy-test setups and
  faster window fills.
- **The estimates overlap; don't add them.**

#### #1 Trim a held hosted device's windows to one exit

Owner decision; sdk and connect, with the proxy's settings.

- **Change:** a hosted device that has carried no user traffic for the idle refill
  hold (10 minutes) already refills nothing. Its held windows keep every exit, with the
  exits' pings, H1 transports and contract retries, until the 45-minute close
  (connect `ip_remote_multi_client_idle_refill.go`: "nothing is disconnected").
  - Under the change, the device keeps one exit per window, its best, and releases the
    rest.
  - It does this after the longer of the idle refill hold and the device's sequence
    idle timeouts. In the hosted low-memory profile those are 3 minutes for TCP and 5
    for others, so the trim comes at 10 minutes, and no inner flow can still be pinned
    to a released exit.
  - The device's next user traffic refills the windows at once, by the wake path the
    hold already uses. The device itself, its 45-minute hold for used devices, D49p's
    10 minutes for unused ones, and every ping, timeout and detection bound stay as
    they are.
  - The trim time derives from those two existing values, so it adds no setting.
- **Saving:** sweep 8–15 proxy cores, plus 2.5–4.5 connect cores; between sweeps 3–5;
  quiet 0.2–0.5. Mutator alone is 2.3–3.9 cores (32–54k released clients × 0.071
  mcores), plus the memory: 0.35–0.59 GB per process at 116 KB of stack and about
  100 KB of heap per client.
- **Evidence:**
  - In sweeps 12.5k of 48.4k live devices carried traffic in the last 10 minutes.
  - The other 36k hold an estimated 68–90k of the 147k window clients; no metric splits
    them. The low end is a held window decaying at the measured removal rate (about
    0.0008 a second per client) over its 45 minutes, which averages 1.9 clients. The
    high end is what is left after active devices at 4.6 each.
  - Keeping one client per idle device releases 32–54k (22–37%).
  - Most held devices never come back: `used` closes, meaning 45 minutes idle after use,
    are about 60% of idle closes.
  - Each released client is also one connect client fewer on the services edges (Part A:
    0.082 cores per 1k clients), and fewer pings and contracts on Redis and PostgreSQL.
    0.082 is CPUOPT5's marginal per-client term. Part A's sweep-era fit prices a hosted
    client higher (0.04–0.07 cores per 1,000 exchange connections at about 3.3 per
    hosted client, plus its share of the GC, timer and scheduler costs; A §6, O2), so
    the connect figure is probably low.
- **Risk: medium.**
  - A user who returns to a held device rides one exit for the seconds the refill takes.
    The quality window is one exit smaller only until then; today a held window that
    has lost exits behaves the same way.
  - The release must not cut a flow: the trim waits out the TCP sequence idle timeout.
- **Proving test:** a deterministic connect test on a fixture window of five clients:
  - with no user traffic for the trim time, the window keeps exactly one client and
    sends no release before the time;
  - a user packet then refills it to five through the existing wake;
  - with a TCP sequence idle timeout set longer than the hold, the trim waits for the
    longer of the two, and the sequence's exit is never released.

  It must fail on fc15cc5912ea, where the held window keeps five.
- **Owner rule:** no timeout, keepalive, probe, detection or revocation bound changes;
  one contract size, the UDP gate and the client limits are untouched. It does reverse
  the 11-05 lever's "nothing is disconnected", hence the owner's decision.

#### #2 One ContractOriginNotifications per proxy process

CPUOPT4 B #4; server `localclient`, `model`.

- **Change:** the proxy's device manager builds one `ContractOriginNotifications` and
  hands it to every device's `localclient.Authority` through `AuthoritySettings`, as it
  already does for the mint and contract admissions.
  - Watches are already keyed by the (source, destination) pair hash.
  - A device's close releases only its own watches and refusals.
  - The api and connect keep theirs.
- **Saving:** sweep 4–8 cores; between 1–2; quiet about 0.1.
  - Per process at 2,118 devices: 38k goroutines (15%) and 0.15–0.35 GB. Per device
    that is 18 parked goroutines of 2–8 KB of stack, 32 KB of queue and the bucket
    maps.
  - Stack scanning is 10.2% of the process.
  - On the §3.2 curve this cuts the collection rate by a quarter to a half. The range
    is discounted for the starved mutator taking back part of the freed CPU.
- **Risk: low–medium.**
  - The shared instance's waiter bound (8,192) and queue must be sized for the process,
    not for one device.
  - A device's route refusals become visible to the process's other devices, which is
    correct, because refusals are keyed by route, not by device.
- **Proving test:** construct N authorities under one manager and assert:
  - the goroutine count does not grow with N;
  - one authority's `Close` leaves another's watch delivering an origin edge;
  - a refusal recorded through one authority replays for an identical request through
    another.

  The first assertion fails on f43488da1.
- **Owner rule:** no bound changes; the route-refusal timeouts (60 s, 6 h) are kept.

#### #3 Proxy memory limit on crisp: 6500 → 8000 MiB

Ops; owner. A per-host value in warp/services.yml.

- **Change:** raise `GOMEMLIMIT` for crisp's 10 proxy processes only. The 10 GiB cgroup
  cap stays as the runaway guard. fireside stays at 6500 MiB: its 24-hour minimum
  available memory is 9.0 GB, and it OOM-killed proxies on 10-23 and 10-25.
  - warp sets `env_vars` per service today, so a per-host override needs a warp change,
    like the per-block `cores` override.
  - The other route to headroom is taking alt off these hosts: 6.4–6.6 GB each, with
    4.0 GB of live heap (Part C).
- **Saving:** sweep 6–11 cores, all on crisp; between 1–2; quiet 0.
  - +1.5 GB of headroom per process is about four halvings of crisp's collection rate on
    the §3.2 curve. crisp's half of the GC prize is about 13.5 cores.
- **Risk: low–medium.** crisp keeps at least 26 GB available at its 24-hour minimum
  after +15 GB. A doubling of live devices would need the cap.
- **Proving test:** a warpctl unit test that a per-host override renders the expected
  `GOMEMLIMIT` for that host's containers and the service default for the others.
- **Owner rule:** a sizing value in config with one value per host, not a code knob;
  no timing bound changes.

#### #4 Build contract stats and details only for listeners

sdk.

- **Change:** `DeviceLocal` builds `ContractStats` and `ContractDetailsList` every
  `ContractStatsEpoch` (1 s) while bytes move, and dispatches them to listeners.
  - Hosted devices have none unless a DeviceRemote attaches: the server's proxy package
    registers no contract-stats listener, and only view controllers and remotes add them.
  - Under the change the tracker keeps its state, the getters compute on demand, and
    the epoch flush builds the lists only when a listener is registered.
  - Optionally, connect's `emitContractStats` events can be coalesced for a client
    whose consumer reads only totals.
- **Saving:** sweep 2–3 cores; between about 1; quiet about 0.1. The tracker path is
  3.7% (crisp) and 4.8% (fireside) of the process with its assists. Those assists are
  10–13% of all assist time, so it also carries about a tenth of the background
  marking.
- **Risk: low.**
- **Proving test:**
  - a DeviceLocal with no listener builds no details list in an epoch (a counting hook);
  - attaching a listener delivers the current details at the next epoch;
  - `GetEgressContractDetails` returns the same values either way.

  The first assertion fails on d9ad8a26c71b.
- **Owner rule:** app logic stays in the sdk; no bound changes.

#### #5 Fewer per-frame allocations on the window clients' hot paths

connect.

- **Change:**
  - `Framer.ReadHeader` reads into a header buffer owned by the framer, not a `[4]byte`
    that escapes through `io.Reader` on every frame.
  - The H1 write path stops allocating per write: utls `UConn.Write`'s small object,
    `sendingChannelCount`'s closure, `ownIpPacketFlow`'s scratch.
- **Saving:** sweep 1.5–3 cores; between 0.5–1; quiet under 0.1. These sites are about
  6% of the process with their assists in the sweep capture.
- **Risk: low** (a single reader owns each framer).
- **Proving test:** `testing.AllocsPerRun` on `ReadHeader` over a bytes reader is 0, and
  on one framed write is no more than the payload copy. Both fail on fc15cc5912ea.
- **Owner rule:** no behaviour change.

#### #6 Small crypto items

- **(a) net/http's own TLS dials through the verified-chain cache.** Today
  `persistConn.addTLS` verifies chains itself: x509 `buildChains` is 0.91% of the
  process. A `VerifyConnection` that consults connect's process cache would cover it.
  - Saving: sweep 0.4–0.6 cores; quiet about 0.02.
  - Proving test: the second handshake to the same chain does no `buildChains` call.
- **(b) The JWT claims cache holds ended entries.** It is at its 64Ki bound because
  entries past their end time leave only by LRU eviction (230 a second).
  - Dropping ended entries when a shard is touched would return about 20–40 MB per
    process. That is small, but it is memory in the regime that needs it.
  - A larger bound would not cut the misses, which are new tokens.
  - Proving test: an entry past its end time is gone from the shard after the next
    store, and `entryCount` falls with it.

#### #7 Investigate the window-client replacements before any change to them

- **The rates:** `unhealthy` removals 60–98/s against 1.6–2.1 on 11-13, and `verdict`
  44–57/s against 7–8.
- **What they cost:** each replacement is a full build, 19 ms of mutator time plus its
  mint, contract and dial, at 100–150 a second, or 2–4 cores in sweeps.
- **The cause is the bots' traffic, not CPU:** the per-client rate had already
  quadrupled at 28% host busy.
- **What to check:** whether the verdicts blame exits for destinations that fail
  everywhere. Use the FP2 S2 window labels and the window warning causes. A false
  verdict is a correctness defect in its own right.
- **No detection bound may be loosened to cut this.**

### 7. Considered and rejected

- **Bot-facing limits.** UA blocks, ASN caps, no hosted proxies for new accounts, and
  lower proxy client limits or create rates: the owner declined these on 11-14, or the
  owner's 11-05 limits stand.
- **Timing changes, under the owner rule.** Rejected:
  - a shorter 45-minute hold for used devices;
  - a shorter 10-minute unused hold;
  - slower pings (`CPingRestTimeout`), a longer `BlackholePollInterval`, or a longer
    expansion backoff;
  - dropping the DoH keepalive pings (every 30 s while a resolver connection is pooled).
- **A smaller hosted quality window from the first open.** It would change every real
  user's first minutes. #1 trims only devices nobody is using.
- **A byte or flow threshold for "carried traffic"** that would hold a device that saw
  one tiny request for 10 minutes instead of 45. It would shorten the hold for light
  real users too, so it is a product decision; not proposed.
- **Raising `defaultHostedMintConcurrency` (2).** It would fill windows faster and add
  memory; today it is the brake that held window clients at 3.5–8k per process in the
  21Z sweep.
- **GOGC, either way.** At the memory limit GOGC doesn't pace the collector. Lower is
  worse; higher does nothing.
- **GOMAXPROCS 4 → 2 or 3.** The hosts are saturated, the work doesn't shrink, and 2 Ps
  would save only 2 gVisor processor goroutines per device (about 1.6% of goroutines).
- **Fewer, larger proxy processes.** Total memory per device is unchanged.
- **`GODEBUG=adaptivestackstart=0` as on alt.** Stacks shrink at every collection
  (every 5 s in sweeps), so steady-state stacks are already near their use. Not
  proposed without a one-block A/B.
- **A process-wide find-providers answer cache shared by devices.** Selection would
  concentrate devices on the same providers. Find-providers is 4.2% of the process,
  1.8 points of it assists that #1–#5 already reduce.
- **TLS session resumption shared across hosted window clients.** Utls handshakes are
  0.5% of the process, and the per-dial-path ticket isolation is deliberate (connect
  `net_tls.go`).
- **Trying Go 1.27's pre-span-scan GC (`GOEXPERIMENT`).** `tryDeferToSpanScan` is 23% of
  the capture, but that is where marking happens now. There is no evidence the old
  collector is cheaper for this heap. A one-block A/B only, and only if #1–#3 leave GC
  high.

### 8. Noticed, not CPU

- **Ephemeral ports toward edge-0 and edge-1.** Each proxy host holds 31.2–31.6k
  connections to 65.49.70.71:443 and 21.3–21.4k to 65.49.70.70:443, against a
  41k-port range (20000–60999) per source and destination pair.
  - At about 1.3× today's window clients on that edge, `connect()` will fail with
    `EADDRNOTAVAIL`.
  - The kernel's port search already costs 0.5% (crisp) and 1.6% (fireside) of a proxy
    process in the captures.
  - Spreading by placement weight, a second source address or IPv6 transports would
    move the cliff; #1 lowers the count.
- **Users degrade in sweeps.** User traffic falls from 19 to 11.6 MB/s; proxy-test setup
  rises from 2.1–2.3 to 5.4–8.8 s and page load from 7.5–9 to 11–16 s (3-hour medians).
- **The config release restarted every proxy.** All 20 proxy processes restarted
  01:11–01:14Z on 11-15 at the same version 1098770070, when the config release
  `2026.11.14-outerwerld+1099044420` reached the hosts, and live devices dropped from
  39k to 24k. Each restart rebuilds every window. alt runs the 11-15 build 1099363220.
- **alt on the proxy hosts** holds 4.0 GB of live heap and 6.45 GB of runtime memory
  per host, with 13k goroutines: memory the proxies need on fireside (Part C).
- **Redis connections.** Each proxy host holds 16.5k Redis connections, about 1.65k per
  process (Part E).
- **Co-tenants.** Loki and Mimir (with warp-grafana) run on both proxy hosts: 0.3–0.5
  of a core and 3.3–3.4 GB of RSS per host.
- **The private LAN's receive** (eno3, r8127) lands on 2 of 32 queues, CPUs 10 and 18.
  Small.
- **The quality windows run below target:** 3.06 of 5 on average, 17.7k below their
  minimum. Mint admission, not provider supply, is the usual limit in sweeps (477
  refusals a second).

## Part C — api, alt and taskworker

Read-only measurement of Main on 2026-11-15 (a Sunday), 10:00–11:15Z, with Mimir from 11-12 00Z
(11-06 for the fits). Raw outputs and scripts are in `<scratch>/C/`.

**Builds (from `urnetwork_build_info` and `urnetwork_source_info`):**

| Service | Build measured in Mimir | Server revision | Running |
|---|---|---|---|
| api | D50a `2026.11.14-outerwerld+1098824240` | 6c0a7ffb6 | 11-14 19:53–20:22Z → 11-15 09:55–09:57Z |
| alt | `2026.11.14-outerwerld+1098822500` | 6c0a7ffb6 | 11-14 19:41Z → 11-15 09:57Z |
| taskworker | D50t `+1098815950` | 6c0a7ffb6 | 11-14 19:14–19:27Z → 11-15 09:48Z |

Two things the brief did not have:
- **The 11-15 release rolled all three at 09:48–09:57Z** to server 11d5a8e78 (taskworker
  `2026.11.15-outerwerld+1099357100`, api `+1099362010`, alt `+1099363220`). Its diff from
  6c0a7ffb6 is the gift's Gamma rail, competition tooling, 132 lines of the points leaderboard,
  9 lines of contract close reports and the proxy-trim tool: nothing on a path measured here. The
  perf captures (10:18Z on) are of the new build; the Mimir windows end at 09:45Z.
- **Every service restarted at 11-15 01:11–01:14Z**, on every host, with no build change:
  connect, proxy, gossip, mcp, alt, taskworker and all 20 api processes. That was the config
  release `2026.11.14-outerwerld+1099044420` (config a5f2c85), published to the hosts at
  01:11Z. It started a new connect generation, so the replay sawtooth began again from about
  0.1k/s (§3.1).

**Which CPUOPT5 Part C items are in which build** (git ancestry of the deployed revisions):
- **D4(a) JWT claims cache** (5437d9614, merge 970dccd8c): api and alt from 399a011b1, rolled
  11-14 00:20–00:31Z (D46). connect, proxy and the taskworker verify through it too.
- **D4(b) verified-chain cache** (connect `net_tls_chain_cache.go`; counts exported by 28d57da44):
  taskworker from 399a011b1, rolled 11-13 22:47–23:01Z; the proxy from aa7142599 (11-14).
- **The extender shadow loop:** b4374ac08 (only epochs 6 and 7 shadow) on the taskworker 11-13
  09:22–09:36Z; hour-long slices, counted timeouts and `RaiseRunOnceMaxTimeInTx` (merge
  95306eb79, deployed in f9589fc0e) 15:37–15:51Z.
- **The stuck-provider signal** is a Redis control, not a build: on for g1 at 11-13 18:11Z and
  every api block at 11-14 01:09Z (`sp_stages.log`). The recycle is still off on every connect block.
- **GOMAXPROCS 8 → 4 for the api** never ran: the 11-13 A/B chain stopped before api g4, and the
  connect g4 half was reverted about 21:25Z (owner, 11-13).
- **PgBouncer direct path, stage 1:** api beta from aa7142599, 11-14 07:58Z (§4.2).

### 1. Summary

- **api: 16–24 cores, 1.2–1.8 cores per krps** (21.7 on 11-14's evening; on 11-15's morning 17.3
  in sweep phases, 15.4 between them). It is no smaller than CPUOPT5's 13–18.5: D4(a) took about
  1.5–1.7 cores off, but real contract creates rose 50–90% against the same hours of 11-12
  (1.3–2.5k/s now), audit-only packs 40%, and the sweeps add 2 cores while they run.
- **Where an api process spends it** (perf B, 1.49 ms per request): `/connect/control` 30% (its
  `CreateContract` 16%, authentication 9%), the HTTP server and the router's gates 13%, the
  scheduler 21–27%, GC about 9%, find-providers2 4–5% at 15–29 ms per request (two thirds of it
  decoding gob score pages), auth-client 2.7% at 3.9 ms, and one location search 5% (about 2.5
  core-seconds a query).
- **The sweeps** (proxy-hot phases three quarters of the time since 11-14 15:20Z) add 0.9k api req/s,
  all cheap ones (create frames, the hosted devices' audit-only packs, `/hello`), +1.95 api
  cores, +18–37% on every route's latency (PgBouncer pools at the cap 3 → 13.5 of 32), +0.3
  taskworker, +0.15 alt. The brief's 15–16k against 8–9k req/s is the replay sawtooth, not the
  sweeps.
- **The replay sawtooth is still the api's largest removable cost:** 0.1 → 7.2k replays/s per
  connect generation, 3.3k/s averaged since D4(a). The stuck-provider signal has now run 24 hours
  on every api block (22,009 clients flagged, 414,098 "still stuck" findings); the recycle that
  would act on it is still off (#1).
- **CPUOPT5's items:** D4(a) cut JWT from 14.6% to about 5–6% of an api process, with 74% of
  verifications answered from the cache; the 977/s it may not hold are expired or exp-less tokens
  accepted by the auth migration gates, and they ride on the replays (r = 0.989). D4(b) took about
  3 taskworker cores off (99.997% hits; x509 25.6% → 6.5–8% of a probe process). The shadow loop
  is gone. GOMAXPROCS 8 → 4 for the api never ran; the lag-guest decode is not void (2.6–2.7%).
- **taskworker: 12–14 cores.** 9.8–10.9 just after the chain cache, back to 13.0–13.6 since the
  sweeps: `FlushTransferDebits` waits on the full pools, and the unique-exit sweep grew from 0.12 s
  to 66 s a run as live assignments went from 1.8k to 7.9k; since about 10Z every run is cut at
  its 10-minute bound and starts again from the oldest assignment (#9). The score export's gob
  encoding is 1.08 cores by its own counter.
- **alt: 0.83–0.89 cores, from 0.34,** at the same 62–67 req/s: its hosts saturate in sweep phases
  (the proxy at 41–45 of crisp's and fireside's 48 CPUs, 23.9 of 24 busy), and its CPU per
  request went from 5.2 to 12–14 ms. Nothing to change in alt; the proxy is Part B's.
- **PgBouncer stage 1 (api beta direct) ran clean for its 24 hours.** New connections cost what
  they cost through nginx (initial ping median 4.1 ms against 4.4–5.5); the latency ratio moved
  only with the sweeps, and beta's mix confounds it. Extend it to every api block after the
  one-at-a-time PgBouncer restart (backlog 65535), counting pool destroys by reason (§4.2): about
  0.7 edge-2 cores at CPUOPT5's 2.2-core hop, about 1.5 at Part D's measured 5.07 (D #1). Warm
  pools would not help: the sweep-minute peaks (150–163 ms) are the wait
  for a PgBouncer server at the cap, which a warm client connection waits for too.

| # | Change | Service | Est. saving | Risk |
|---|---|---|---|---|
| 1 | Recycle stuck providers: stage 3, then bake in (CPUOPT5 C #1) | api, Redis, nginx | 0.8–1.9 api averaged, 2–4.5 at the top of a sawtooth; 0.45–0.55 Redis | medium; built |
| 2 | GOMAXPROCS 8 → 4 for the api, 72 h on one block (CPUOPT5 C #5) | api | 1–2 *(inferred)* | low–medium |
| 3 | A typed codec for the client-score export (gob out) | taskworker, api (alt, proxy) | 1.1–1.6 | medium |
| 4 | Remember verified chains on the probe's stock-TLS dials (most likely its per-tunnel DoH clients) | taskworker | about 0.9 *(inferred)* | low–medium; owner's probe review |
| 5 | The lag-guest gate without a claims decode (CPUOPT5 C #6) | api, alt | 0.35–0.4 | low |
| 6 | Remember tokens the legacy auth gates accept | api | 0.2–0.45 (overlaps #1) | low |
| 7 | PgBouncer direct path, stage 2 for the api, destroys counted by reason | edge-2 | about 0.7 (about 1.5 at D's 5.07-core hop) | medium |
| 8 | An index for the fuzzy location and network-name searches | api | 0.3–0.45 *(inferred)* | low–medium |
| 9 | The unique-exit sweep stops inside its 10-minute bound and resumes from a cursor | taskworker | about 0.2, and passes that finish | low |
| — | Small: decode each client's secp256k1 key once | api, taskworker | about 0.1 | low |

**Totals:** #1–#6, #8 and #9 come to about 5–8.5 cores (api 3.0–5.8 averaged, taskworker 1.9–2.1,
Redis 0.45–0.55), more at the top of a sawtooth; #7 adds about 0.7 edge-2 cores (about 1.5 at
Part D's figures). #1 and #6 overlap.

### 2. Method

- **Windows.** 5-minute means over the evenings 18–22Z of 11-10..11-14, and three windows of
  11-14/15 around the 01:11Z restart; hourly and 1-minute series from 11-12 00Z. The fits of api
  cores on request classes use CPUOPT5's method (hours with every api process at least 40 minutes
  old) over 11-06..11-13 (CPUOPT5's period) and 11-14 00:40Z..11-15 09:50Z (after D4(a)).
- **Sweeps.** A sweep phase is a minute with the proxy service above 33 cores, a quiet phase one
  below 27; the proxy alternates between about 17–24 and 41–45 cores since 11-14 15:20Z, and the
  PgBouncer pools at their 22-server cap move with it. Sweep against quiet compares the interior
  minutes (3 minutes in, 2 before the end) of every phase between 11-15 01:56Z and 09:47Z, one api
  generation: 326 sweep minutes, 80 quiet (`<scratch>/C/sweep_diff.txt`).
- **perf** (30 s at 99 Hz with call graphs, one process, aggregated on the host, data deleted, no
  perf process left; CPUOPT5's `perf_one.sh`, and `perf_two.sh`, which adds attribution
  to the outermost `api/handlers.*` frame or taskworker task and the callers of chosen leaf
  families). The validator gate at each: intent finalized or applied, at least 15 minutes to the
  next boundary, and in the second window also at least 10 minutes past the boundary with no
  `POST /sn/attempt-artifact` at the api for 3 minutes (the post-boundary burst). Right after a boundary progress.json still reads the previous epoch's
  "finalized", so the CPUOPT5 check alone opens the gate too early.
  - window 1 (epoch 26354, boundary 10:38:52Z):
    - **A:** api g2 on edge-4, 10:18:46Z, 1.41 cores, 5,755 samples (`perf_api_edge4_g2_sweep.txt`);
    - taskworker g2 on edge-4, 10:20:10Z, 0.23 cores, no probe pass (`perf_tw_edge4_g2.txt`);
    - taskworker g1 on edge-1, 10:21:09Z, 2.79 cores, 8,646 samples (`perf_tw_edge1_g1.txt`);
  - window 2 (epoch 26355 finalized 10:45:56Z, uploads over by 10:48Z):
    - **B:** api g2 on edge-4, 10:50:06Z, 1.59 cores, 6,070 samples (`perf2_api_edge4_g2_a.txt`);
    - taskworker g2 on edge-0, 10:53:11Z, 2.10 cores, 6,982 samples (`perf2_tw_edge0_g2.txt`);
    - taskworker g1 on edge-0, 10:56:21Z, 1.90 cores, 6,078 samples (`perf2_tw_edge0_g1.txt`).
  - **Skipped:** alt. The script refuses a host at or above 90% busy: crisp read 95–99% busy
    (Mimir, 1 minute) when its capture was due, and fireside 98% and 99% in the script's own 5 s
    samples at 10:55:57Z and 11:14:04Z (load 35–41 on 24 CPUs), both in a sweep phase. The proxy
    stayed in that phase from 08:38Z past 11:15Z, so there is no quiet-phase api capture either.
  - The 10:20Z capture overlapped Part A's 20 s nginx capture on edge-4
    (`/tmp/cpuopt6n.*`), and the 10:53Z capture on edge-0 overlapped Part A's nginx capture
    there (A §2); at 99 Hz each costs well under 1% of a core. The script files
    `/tmp/cpuopt6c_perf_one` and `_two` remain on edge-0, edge-1 and edge-4 (A §2).
- **Per route.** CPU per request by route is the perf share of the route's handler frame
  (outermost `api/handlers.*`) times the process's cores, over the process's own route rates in
  the capture minute; the scheduler, GC, kernel and HTTP-server work outside any handler is
  reported separately. Request rates and latency come from `urnetwork_http_requests_total` and
  `urnetwork_http_request_duration_seconds`.
- **Caches.** `urnetwork_auth_jwt_verified_cache_*`, `urnetwork_auth_jwt_legacy_accepts_total`,
  `urnetwork_tls_verified_chain_cache_*`; the stuck-provider counters; the pool and PgBouncer
  gauges; `urnetwork_update_client_scores_phase_*`.
- **Container logs:** the taskworker's lines for one task, `SweepProxyExits`, read through Loki
  with ids masked (`<scratch>/C/loki_upe_sweep.txt`).
- **Code** read at 6c0a7ffb6 (and its diff to 11d5a8e78).
- **Changed nothing:** no counter reset, no deploy, no setting changed. The stage 1 one-shot
  (`d47_stage1_watch_once.sh`) was run once, read-only.
- **Settings now:** GOMAXPROCS api 8, alt 4, taskworker 4 (`go_sched_gomaxprocs_threads`); GOGC 100
  everywhere; no memory limit on the api or taskworker.

### 3. Where the CPU goes now

#### 3.1 api and alt by window

`<scratch>/C/api_windows6.txt` (5-minute means):

| Window | api cores | k req/s | control k/s | /hello k/s | create frames k/s | replays k/s | discards k/s | cores per krps | JWT cache hits | alt cores | alt req/s | taskworker |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 11-10 18–22Z | 18.53 | 16.65 | 14.41 | 1.56 | 11.82 | 9.36 | 2.30 | 1.11 | — | 0.32 | 72 | 13.34 |
| 11-11 18–22Z | 17.20 | 13.21 | 11.33 | 1.16 | 8.75 | 6.88 | 2.30 | 1.30 | — | 0.36 | 77 | 12.52 |
| 11-12 18–22Z | 13.93 | 7.90 | 5.51 | 1.65 | 2.84 | 1.62 | 2.36 | 1.76 | — | 0.34 | 65 | 13.80 |
| 11-13 18–22Z | 23.99 | 20.35 | 17.37 | 2.20 | 14.21 | 11.66 | 2.80 | 1.18 | — | 0.49 | 64 | 13.87 |
| 11-14 18–22Z | 21.71 | 15.17 | 12.29 | 2.09 | 8.58 | 6.23 | 3.37 | 1.43 | 77% | 0.81 | 67 | 13.24 |
| 11-14 22Z–01:10Z | 20.22 | 15.49 | 12.85 | 2.00 | 9.31 | 7.04 | 3.31 | 1.31 | 77% | 0.87 | 62 | 13.43 |
| 11-15 03–06Z | 16.35 | 9.30 | 6.64 | 2.03 | 3.02 | 1.25 | 3.37 | 1.76 | 70% | 0.89 | 59 | 12.87 |
| 11-15 06–09:45Z | 17.99 | 10.91 | 7.99 | 2.18 | 4.34 | 2.42 | 3.36 | 1.65 | 74% | 0.88 | 67 | 13.31 |

- **The api runs 16–24 cores, 1.2–1.8 per krps.** Cores per krps fall as replays rise, because a
  replay is cheaper than the average request. 11-13's evening is the largest because its connect
  generation was 26–29 hours old (11.7k replays/s); 11-14's (D4(a) on, sweeps on) is 21.7.
- **The replay sawtooth is unchanged in shape.** Replays fall to about 0.1k/s within 20 minutes of
  a connect restart and climb about 0.35–0.45k/s an hour: 7.24k/s in the 11-15 00Z hour, 18 hours
  into the 11-14 07:10Z generation. They averaged 3.28k/s from 11-14 00:40Z to 11-15 09:45Z (max
  7.35k/s), against 5.87k/s (max 15.4k/s) over CPUOPT5's 11-06..11-13: connect restarted three
  times in 26 hours (11-13 23:15Z, 11-14 07:10Z, 11-15 01:11Z).
- **What grew:** "real" creates (create frames that are not replays) by 50–90% against the same
  hours of 11-12 (`hourly_api.txt`: 04Z 0.87 → 1.64k/s, 06Z 1.03 → 1.93k/s, 09Z 1.30 → 1.98k/s;
  the evening 1.22k/s on 11-12, 2.35k/s on 11-14), and discards (audit-only packs answered before
  authentication) from 2.3–2.4k/s to 3.2–3.5k/s. Both rise in sweep phases (§3.2). `/hello` rose
  from 1.6 to 2.0–2.3k/s.
- **The fits** (`api_fit6.txt`): CPUOPT5's period gives 0.49 cores per 1k replays/s (CPUOPT5:
  0.475); after D4(a), 0.26–0.38 (1-hour and 20-minute steps). The other terms are collinear (real
  creates, discards and the other routes rise together through the day and in sweeps) and swing
  with the window, so per-route costs below come from perf, not from the fits.
- **alt: 0.83–0.89 cores at 62–67 req/s**, up from 0.34: §3.5.

#### 3.2 Sweep phases against quiet phases (11-15 01:56–09:47Z)

| | Quiet | Sweep | Change |
|---|---|---|---|
| proxy cores | 20.3 | 43.9 | +23.6 |
| connect cores | 108.8 | 119.9 | +11.1 |
| api cores | 15.39 | 17.34 | **+1.95** |
| api req/s | 9,163 | 10,086 | +922 (control +779: creates +392, discards +387, replays +33; `/hello` +161; every other route flat or lower) |
| api cores per krps | 1.68 | 1.72 | +2.4% |
| route latency | control 13.8 ms, auth-client 19.6, remove-client 32.8, find-providers2 62.4 | 16.3, 25.0, 45.0, 82.1 | +18% to +37% |
| PgBouncer pools at the 22-server cap (of 32) | 3.0 | 13.5 | +10.5 |
| PgBouncer servers | 599 | 665 | +66 |
| api pool acquires/s | 9.8k | 11.7k | +1.9k |
| edge-2 busy cores | 36.2 | 42.2 | +6.0 |
| alt cores | 0.76 | 0.91 | +0.15 |
| taskworker cores | 12.86 | 13.14 | +0.28 |

- **The sweeps reach the api through the hosted devices' control traffic**, not through sign-ups:
  create frames and the devices' audit-only packs, plus `/hello`. `auth-client`,
  `network-create` and the login routes do not move between phases. Sweep phases have filled 867
  of the 1,133 minutes from 11-14 15:20Z to 11-15 10:13Z (77%), so the sweeps cost the api about
  1.5 cores averaged, the taskworker about 0.2, alt 0.1.
- **They are not the 15–16k against 8–9k req/s of the brief.** The api's 15–16k req/s on the
  evening of 11-14 was the replay sawtooth (6–7k replays/s, 12–18 hours into a generation), and
  it fell to 7–9k at the 01:11Z restart, not on a sweep's schedule. A sweep adds about 0.9k req/s.
- **Two network-create bursts did cost the api:** 11-14 19:34–19:53Z (25/s for 20 minutes, max 37/s)
  ran the api from about 22 to 28–29.6 cores, about 0.23 core-seconds per sign-up (argon2,
  CPUOPT5 C §5 keeps its parameters); 20:13–21:17Z ran 11/s.

#### 3.3 Inside an api process, by route

Two captures of the same process, edge-4 g2, both in sweep phases (the proxy ran hot from 08:38Z
past 11:00Z, so no quiet-phase capture was possible in the validator's windows):
- **A**, 10:18:46Z: 1.41 cores; control 775/s (discards 314, create frames 434 with 253 replays),
  `/hello` 185/s, the egress-probe routes 48/s, auth-client 13/s, remove-client 10/s,
  find-providers2 2.5/s; JWT misses 125/s, 79/s of them legacy accepts.
- **B**, 10:50:06Z, with per-handler attribution (`perf2_api_edge4_g2_a.txt`): 1.59 cores, 1,069
  req/s: control 834/s (discards 321, create frames 490 with 309 replays, so 181 real creates;
  closes 11, provides 8), `/hello` 196/s, auth-client 11/s, remove-client 9/s, find-providers2
  2.0/s; JWT misses 132/s, 88/s legacy accepts; a GC cycle fell inside it.

**By owner (B):**

| Owner | Share of the process | In-handler CPU per request |
|---|---|---|
| `/connect/control` | 30.4% | 0.58 ms per request; 0.94 ms per authenticated pack (discards cost 0.25% in all) |
| — `CreateContract` (the companion escrow transaction 7.3%, contract admission in Redis 8.2%) | 15.6% | 1.37 ms per real create |
| — authentication (`AuthUnlessAnswered`) | 9.2% | |
| — prepare / per-client admission (`AdmitConnectControlAt`) / refusal replay | 4.7% / 3.6% / 1.2% | |
| the HTTP server and the router's gates, outside any handler (lag-guest gate 2.6%) | 13.0% | 0.19 ms per request |
| find-providers2 (decoding gob score pages 2.9%) | 3.7% | **29 ms** (15 ms in A); 23 ms of it gob |
| a find-provider-locations search (`SearchLocal.aroundIdsRawN`: edit distance over every location name, 8 goroutines) | 5.3% | one query in 30 s: about 2.5 core-seconds |
| auth-client (JWT signing, the client row) | 2.7% | 3.9 ms |
| `/hello` | 1.5% | 0.12 ms |
| remove-client / subscription balance / reliability / key | 0.6% / 0.5% / 0.5% / 0.4% | 1.1 / 3.7 / 1.9 / 1.7 ms |
| origin-notification subscription (background) | 3.8% | |
| `runPosts`, Redis pipelines, other background | about 4% | |
| scheduler and runtime | 21.2% | |
| GC (background marking) | 9.4% | |

**By component (A, B):**

| Component | A | B | CPUOPT5 (edge-4 g2, 1.43 cores) |
|---|---|---|---|
| JWT: verification / unverified decodes / P-256 outside both | 2.3% / 2.8% / 0.7% | 2.3% / 2.4% / 0.8% | 7.3% / 4.1% / 3.2% |
| `CreateContract` | 18.7% | 15.6% | 9.6% |
| go-redis / pgx / encoding/json | 14.9% / 12.7% / 4.8% | 13.3% / 8.7% / 4.3% | 10.8% / 6.9% / 7.1% |
| scheduler (`findRunnable`, `sysmon`) | 26.9% (17.3%, 6.8%) | 21.9% (13.7%, 6.1%) | 20.7% (12.9%, 5.3%) |
| GC mark / `mallocgc` | 0% (no cycle) / 7.9% | 9.6% / 8.4% | 8.1% / 8.9% |
| network softirq on the process's threads | 15.1% | 16.8% | 8.3% |
| secp256k1 point decompression of client keys (`decodeStClientKeyHistoryRecord` in `GetClientContractMetadata`) and SN25 evidence signatures (`startifact`) | — | 0.5% / 0.4% | — |

- **The average request costs 1.49 ms of api CPU (B), 0.19 ms of it the HTTP server and gates.**
  Handlers take 41% of the process, the search 5%, the server and gates 13%, background work 8%,
  and the runtime (scheduler and GC) 31%.
- **Per route, fleet-wide** (the in-handler costs times the fleet's rates over the last hour):
  `/connect/control` about 9.1k/s at 0.58 ms, 5.3 cores in-handler (real creates 1.8–2.5k/s at
  1.37 ms, 2.5–3.4 of them); find-providers2 28/s at 15–29 ms, 0.4–0.8; auth-client 209/s at 3.9
  ms, 0.8; `/hello` 2.4k/s at 0.12 ms, 0.3; remove-client 122/s at 1.1 ms, 0.13;
  find-provider-locations 0.15/s at about 2.5 core-seconds, about 0.4 *(one query)*.
- **The sweeps' routes are the cheap ones:** a sweep adds 0.9k req/s, mostly create frames and the
  hosted devices' audit-only packs (§3.2).
- **auth-client is the costliest steady route per request after find-providers2** (3.9 ms: a
  client row and a signed token). It is not the egress probe's: auth-client held 195–211/s while
  the taskworker restarted at 09:48–09:51Z and probe attempts fell to 26/s.

#### 3.4 taskworker

`<scratch>/C/tw6.txt`, `tw_tasks.py`:

| Day | Cores | Probe attempts/s | All-in ms per attempt | Note |
|---|---|---|---|---|
| 11-09 | 12.03 | 76.4 | 158 | |
| 11-10 | 12.82 | 76.9 | 167 | |
| 11-11 | 12.45 | 73.4 | 170 | |
| 11-12 | 12.74 | 69.2 | 184 | shadow loop from 20:30Z |
| 11-13 | 13.77 | 68.9 | 200 | shadow loop to 09:30Z; chain cache from 23:00Z |
| 11-14 | 11.90 | 66.4 | 179 | sweeps from 15:20Z |
| 11-15 (to 10Z) | 13.00 | 65.6 | 198 | |

- **The chain cache took about 3 cores off.** 11-13 19–23Z ran 12.8–14.3 cores at 207–229 ms per
  attempt; 11-14 00–05Z, the first hours on 399a011b1, 9.8–10.9 cores at 155–180 ms.
- **Then the sweeps added 1.5–2 cores:** 11.3–11.9 cores on 11-14 06–14Z, 13.0–13.6 from 16Z. The
  wall time of `FlushTransferDebits` went from 3.2–3.9 s/s to 7.3–9.8 s/s (each run 0.5 → 1.4 s,
  waiting on the full PgBouncer pools), and `SweepProxyExits` from 14–25 s an hour to 1,530–2,440 s
  an hour as live unique-exit assignments grew from 1.8k to 7.9k (11-15 00–08Z): a run took 0.12 s
  on 11-14 06Z, 23 s at 08Z and 66 s at 09Z on 11-15, and since about 10Z every run is cut at its
  10-minute bound (#9).
- **The `UpdateClientScores` export's gob encoding is 1.08 cores** on its own phase counter
  (`urnetwork_update_client_scores_phase_duration_seconds_total{phase="gob_encode"}`, 6 hours to
  10Z: 1.075 s/s, 731 encodes/s, 18.7 MB/s), CPU-bound reflection.

**Inside taskworker processes** (three captures; every taskworker process runs probes, and the
other tasks land where their slots are):

| Component | edge-1 g1, 10:21Z, 2.79 cores, 8.8 attempts/s | edge-0 g2, 10:53Z, 2.10 cores, 14.9/s | edge-0 g1, 10:56Z, 1.90 cores, 17.8/s | CPUOPT5 (probe process) |
|---|---|---|---|---|
| `UpdateClientScores` export (its gob encoding) | 21.9% (20.6%) | 29.3% (13.2%) | — | — |
| egress probe: connect client stack / probe frames | 51.7% / 15.3% | 52.9% / 11.3% | 30.7% / 15.4% | 60.5% / 21.5% |
| TLS client handshakes / x509 `Verify` (P-384) | 15.4% / 8.0% (7.3%) | 12.4% / 6.5% (5.8%) | 13.8% / 7.0% (6.3%) | 36.9% / 25.6% (22.8%) |
| — of which under `net/http.(*persistConn).addTLS` (Go's stock TLS) | all of it | all of it | all of it | 9.9% / 9.4% |
| `CloseExpiredContracts`' force close (settlement transactions 6.9%) | — | — | 13.9% | — |
| `SweepProxyExits` → `FindProviders2` per replacement draw | 6.1% | — | — | — |
| `UpdateClientLocations` / `RollupClientReliabilityStats` / `FlushTransferDebits` | 3.7% / — / 1.0% | — / — / 2.2% | 2.6% / 3.9% / 2.5% | — |
| in-process control (`providerEgressControl.ConnectControl`) | 5.5% | 3.8% | 5.4% | 7.6% |
| GC (all marking) / `mallocgc` | 14.2% / 9.7% | 19.4% / 7.4% | 9.2% / 8.6% | 6.1% |
| scheduler | 4.6% | 7.6% | 15.5% | 9.8% |

- **The chain cache did its job:** the Chrome-hello dials' x509 is gone from every capture. What
  remains, 6.5–8.0% of each probe process (about **0.9 taskworker cores**, 7% of 13), is Go's own
  TLS stack: `persistConn.addTLS`, a net/http transport that dials with `DialContext` and
  handshakes itself, which the cache does not cover by design.
- **Which transport is not in the stacks** (net/http handshakes in a goroutine of its own).
  CPUOPT5 C #7 read these dials as the per-tunnel control-plane clients, but connect's
  `ClientStrategy` transport dials through its own `DialTLSContext` (connect net_http.go:2983),
  which never reaches `addTLS`, and so does the probe's target client (providertunnel
  tunnel.go:1358). The stock transports left are the per-tunnel DoH clients (connect
  net_http_doh.go:516: through the tunnel, one DoH cache and session cache per tunnel, so one full
  handshake per tunnel) and the streaming-POST transport (net_http.go:3663). Two captures show
  7.5–9 ms of x509 per probe attempt, about one full chain verification per attempt, which fits
  the DoH clients *(inferred)*.
- **The export's gob encoding** is 13–21% of the process that runs it, and its allocations feed
  the GC (19.4% and 14.2% in the two exporting captures, 9.2% in the one without).
- **Contract expiry** (`continueContractExpiry` → `settleContractWithOptions`) took 13.9% of one
  process: expired contracts settled one transaction each, more of them since the sweeps' hosted
  devices open more contracts. Financial work; nothing proposed.

#### 3.5 alt

`<scratch>/C/alt_tbl.txt`:

| Window | alt cores | req/s | CPU ms per request | mean latency ms | crisp busy (of 24) | fireside busy | proxy cores |
|---|---|---|---|---|---|---|---|
| 11-12 | 0.34 | 66 | 5.2 | 13.6 | 3.7 | 3.7 | 5.2 |
| 11-13 00–15Z | 0.34 | 62 | 5.5 | 14.0 | 3.6 | 3.6 | 5.0 |
| 11-14 00–15:20Z | 0.45 | 62 | 7.3 | 13.5 | 5.9 | 6.2 | 9.2 |
| 11-14 15:20Z–01:10Z | 0.83 | 67 | 12.3 | 17.7 | 20.3 | 20.4 | 38.0 |
| 11-15 02–09:45Z | 0.87 | 62 | 14.2 | 15.1 | 20.4 | 20.4 | 38.1 |

- **alt's CPU per request has nearly tripled with no change in its work:** the same routes
  (`/hello` 30/s, control 29/s, the rest under 1/s each), the same 24–31k goroutines across its
  two processes, and the same 4.5–5 GB heap per process. What changed is its hosts: the sweeps run the proxy at 41–45 cores
  on crisp and fireside, 48 CPUs between them, and both read 23.9 busy cores of 24 in the
  sweep hours (11-14 22Z–11-15 00Z, 11-15 04–06Z). alt shares those CPUs.
- **Nothing to change in alt.** The fix is the proxy's (Part B); alt's latency has held
  (15–18 ms against 13–14 ms).
- **No alt capture:** both hosts were at or above the capture script's 90% busy refusal whenever
  the validator was idle (§2), which is itself the finding.

### 4. What CPUOPT5's items did

#### 4.1 Part C's items

| CPUOPT5 item | Status | Measured | Against the prediction |
|---|---|---|---|
| C #1 stuck-provider signal, then recycle, then bake in | **Half done.** Signal on: g1 11-13 18:11Z, every api block 11-14 01:09Z (`sp_stages.log`); its 24 hours with the recycle off ended 11-15 01:09Z. Recycle off on every connect block. | In the 24 hours to 11-15 10Z the api processes decided 541,409 times: **22,009 flags set**, 102,897 cooldowns, **414,098 still_stuck** (flagged over `StillStuckAge` ago and still retrying, since nothing recycles them), 2,287 rate_limited, 89 errors, 0 dropped. connect: 0 recycled (5,140 disabled, 142,742 not held). Replays still climb 0.1 → 7.2k/s per generation. | Saving not yet taken (#1) |
| C #2 extender shadow loop | **Shipped.** Epoch 4's live shadow retired (b4374ac08, 11-13 09:30Z); slices, counted timeouts and the stale-bound raise (merge 95306eb79, in f9589fc0e at 15:45Z) | `StLedgerV2UsageStep` 0 in flight since 11-13 10Z and no error since. The hourly totals cannot separate the loop's 1.5 averaged cores from the probe's own swing (12.2–14.7 cores in its last hours, 13.0–14.3 in the twelve after); CPUOPT5's perf put it at 1.8 cores while it ran, and it no longer runs. edge-2's side is Part D's. | Met |
| C #3 D4(b) verified-chain cache | **Shipped.** taskworker 11-13 22:47–23:01Z; proxy 11-14 | Hits 99.997% (taskworker 296/s against 0.010/s misses; proxy 357/s against 0.008/s; 8 and 40 entries). **taskworker −3 cores:** 12.8–14.3 cores at 207–229 ms per attempt (11-13 19–23Z) → 9.8–10.9 at 155–180 ms (11-14 00–05Z). x509 in a probe process 25.6% → 6.5–8.0%: what is left is Go's stock TLS (`persistConn.addTLS`), which the cache does not cover by design (#4) | Met (2.2–2.6 predicted for the taskworker; proxy is Part B's) |
| C #4 D4(a) verified JWT claims cache | **Shipped.** api and alt 11-14 00:20–00:31Z | api: 74% of verifications answered from the cache (4.5k/s hits, 1.6k/s misses); JWT **14.6% → about 5–5.8%** of an api process (verification 2.3%, unverified decodes 2.4–2.8%, P-256 outside them 0.7–0.8%), about **1.5–1.7 cores** at 17–18 api cores; the replay term of the fit 0.49 → 0.26–0.38 cores per 1k/s. Never cached: **977/s legacy accepts** (929/s expired client tokens, 48/s with no expiration), which follow the replays (r = 0.989, 0.22 per replay) | About met (1.8–2.4 predicted at 11-07..11-11's evening replay levels) |
| C #5 GOMAXPROCS 8 → 4 for the api | **Not run** (the 11-13 chain never reached api g4) | Scheduler 21.9–26.9% of an api process (`findRunnable` 13.7–17.3%, `sysmon` 6.1–6.8%), against 20.7% | — (#2) |
| C #6 lag-guest claim without the claims decode | **Not done, and not void:** the gate peeks the cache and decodes every token it does not hold | `requestLagGuestCredential` 2.6–2.7% of an api process; `ParseUnverified` 2.4–2.8% in all | — (#5) |
| C #7 one control-plane strategy per probe process | **Not done** | The stock-TLS dials' x509 is the only x509 left (6.5–8.0% of a probe process); they are probably not the control-plane clients, whose transport has its own TLS dialer, but the per-tunnel DoH clients (§3.4) | — (#4) |

#### 4.2 PgBouncer direct path, stage 1 (api beta)

beta's processes have dialed the 32 PgBouncer instances directly since 11-14 07:58Z (16 process
starts on the direct path in 24 hours). The 24-hour watch ended 09:00Z; a last read-only run of
its one-shot at 10:44:46Z (`<scratch>/C/stage1_watch_1045.txt`) was clean: beta 5xx 0.000069 per
request against g1–g4's 0.000063; no PgBouncer clients waiting, no starved pool, all 32 up; no
"each of N instances failed" or "instance on port N out" line.

`<scratch>/C/stage1_2h.txt` (2-hour windows):

| | Before stage 1 (11-13 10Z–11-14 08Z) | Stage 1, before the sweeps (11-14 08–14Z) | Stage 1 with sweeps (11-14 16Z–11-15 10Z) |
|---|---|---|---|
| beta / g1–g4 mean request latency | 1.82–2.85 | 2.42–2.66 | 1.44–1.95 |
| same, `/connect/control` only | 1.89–3.11 | 2.66–2.92 | 1.45–1.97 |
| beta / g1–g4 mean latency, ms | 7.5–16.1 / 3.8–6.0 | 16.5–21.1 / 6.5–8.0 | 16.3–24.6 / 10.2–16.3 |
| pool acquire wait, µs, beta / g1–g4 | 1.5–46 / 1.3–35 | 2.3–10 / 1.7–17 | 12–366 / 36–919 |
| api CPU per krps, beta / g1–g4 | 2.3–5.4 / 1.1–1.5 | 3.6–4.5 / 1.4–1.7 | 2.4–4.7 / 1.3–1.7 |

- **No harm, and no clean gain in latency either.** The ratio fell only once the sweeps began,
  because g1–g4's latency doubled (pools at the cap) while beta's rose by a third. beta carries a
  fifth of a g block's database work (604 acquires/s against 2.8–2.9k/s) and a different mix, so
  the comparison is confounded: beta's processes average 0.5 connections in use (g 2.2–2.5) and
  never reached their cap of 16 (g1–g4 did, which is most of their acquire wait).
- **New connections cost the same on both paths.** The initial ping, per 10 minutes over the
  watch: beta median 4.1 ms (g1–g4 4.4–5.5), peaks 163 ms (11-15 03:40Z) and 153 ms (11-14
  12:10Z), against g1's 153 ms and g4's 101 ms; the peaks are sweep minutes, when a new
  connection waits for a server in a pool at its cap. beta's 2-hour means (10–38 ms) are higher
  only because it opens fewer connections: 4,070 in 24 hours against 13.8–15.5k per g block.
- **No CPU change is visible in beta's processes**: their CPU per krps moves with the mix, as before.
  The CPU gain of the path is on edge-2 (the nginx hop: 2.2 cores, about 42 µs per query, CPUOPT5
  Part D #2): the api is 11.9k of the 38.4k acquires/s fleet-wide (31%), so stage 2 would take
  about 0.7 of those cores, connect (17.1k/s; Part D reads 17.6k) about 1.0. Part D measures the
  hop at 5.07 cores now (D §3.2), so at today's load both shares are about twice that *(the
  api's 29–31% of 5.07 cores is about 1.5)*.
- **The pools churn on both paths:** 145–162 new connections per g process per hour (beta 42),
  with **zero** destroyed for idle time or lifetime (`urnetwork_pg_pool_connections_destroyed_total`
  by reason). The rest are connections closed on an error or an interrupted round trip, which the
  counter does not name (db.go's `beginTx` documents that pgx tears down a connection whose round
  trip is interrupted).

**Recommendation: extend it to every api block (stage 2), after one precondition, with one
change; warming the pools is not the lever.**
1. **First,** the one-at-a-time `./run-dbs.sh --pgbouncer-restart`, so the instances listen with
   the configured backlog of 65535 (they still listen with 128; stage 0's note): 20 api processes
   reconnecting at once must not overflow it.
2. **Why not warm pools:** under transaction pooling a client connection holds no server, so a
   warm connection's first query waits for a server exactly as a new connection's initial ping
   does. The 150–163 ms peaks are that wait (pools at the 22-server cap in sweep minutes), on both
   paths and for every query; only the cap or less work moves it (the owner keeps the cap for
   now, 11-14). The api pools already keep 10.6–12 idle connections a process, and only 0.35%
   (beta) to 0.9% (g) of acquires wait for one (`outcome="empty"`). A warm floor (pgx's
   `MinIdleConns`, `pool_min_idle_conns` in the vendored v5.10.0, which db.go does not set) would
   save those few acquires a TCP connect and a PgBouncer startup, a few ms: worth a db.yml key with
   one default only if stage 2 raises the `empty` share or the initial-ping median.
3. **Count destroys by reason** (closed by the server, by an error, by an interrupted round trip),
   so the churn's source can be found before anyone tunes it. Regression test: a pool whose
   connection is closed under it reports the destroy under its reason, deterministically.
- **No timeout moves:** `conn_max_idle_time` (5 m) stays; raising it would loosen a timeout.
- **Acceptance:** stage 1's triggers per block, plus the `empty` share of acquires and the
  initial-ping median holding at or below g1–g4's figures before the stage.

### 5. New costs

- **The sweeps** (not a release) cost the api about 1.5 cores averaged, the taskworker about
  0.2 and alt 0.1 (§3.2), and they starve alt's hosts (§3.5).
- **The taskworker's waits and the unique-exit sweep:** the sweeps added 1.5–2 taskworker
  cores, `FlushTransferDebits` waiting on the full pools and `SweepProxyExits` growing from
  0.12 s to 66 s a run, now cut at its 10-minute bound every run (§3.4, #9).
- **Two network-create bursts** on 11-14's evening ran the api from about 22 to 28–29.6
  cores (§3.2).
- **New tasks cost little:** the points leaderboard rebuild ran 7 times in 24 hours (every 1–4
  hours in this window, not hourly) at 110–197 s each, mostly database time; the Stripe dunning
  sweep 5 runs, 67 s in all; `StLedgerV2UsageStep` 0 in flight since 11-13 10Z.
- **The 11-15 builds** add nothing on a measured path (intro).

### 6. Ranked opportunities

Cores are at today's load (api 16–24, taskworker 12–14, alt 0.9), averaged unless noted.

#### #1 Recycle stuck providers: stage 3, then bake in (CPUOPT5 C #1, CPUOPT4 #10)

- **Change:** the owner's staged plan, unchanged: the recycle on the beta connect block, then g1,
  then all; after a week on every block, both switches become code defaults and their keys go.
  The signal has now run 24 hours on every api block with the recycle off (§4.1).
- **Evidence:** §3.1 and §4.1. Replays averaged 3.28k/s since D4(a) (max 7.35k/s); 20 minutes
  after a connect restart they are about 0.1k/s and they regrow 0.35–0.45k/s an hour. In 24 hours
  the signal flagged 22,009 clients and found 414,098 times that a flagged client was still
  retrying, because nothing recycles it.
- **Saving:** if the recycle holds replays near a fresh generation's floor, about 2.5–3k/s fewer
  on average. A replay costs 0.3–0.65 ms of api CPU: the post-D4(a) fit's marginal 0.26–0.38,
  and perf B's 0.46 ms in the handler (authentication, admission, prepare, the replay answer) plus
  0.19 ms in the HTTP server and gates. That is **0.8–1.9 api cores averaged and 2–4.5 at the top
  of a sawtooth** (7k/s), plus one admission `EVALSHA` per replay (about 0.45–0.55 Redis cores
  averaged at CPUOPT5 Part E's 178 µs per request, 1.2 at the top; Part E now measures 111–117 µs
  a request and puts the sawtooth at 0.5–1.0 Redis cores averaged, E §6) and its nginx relay (Part A). The
  fewer the connect restarts, the more the recycle saves.
- **Risk:** medium; a recycled provider reconnects with its drain excuse written first. Outside
  the validator's busy minutes, as staged.
- **Proving test:** for the bake-in, a deterministic test that with no Redis control set the api
  reports stuck providers and connect recycles them (the code defaults), and that the retired keys
  are ignored. In production: `urnetwork_connect_stuck_provider_recycles_total{outcome="recycled"}`
  above 0 on the enabled block, its replay slope per connect hour against the other blocks,
  `still_stuck` decisions falling, provider reliability and excused drains unchanged.
- **Owner rules:** no timing bound moves (the thresholds, refusal answers, the refusal ring and
  every deadline stay); the bake-in removes the keys (no hidden settings).

#### #2 GOMAXPROCS 8 → 4 for the api, A/B on one block (CPUOPT5 C #5)

- **Change:** the per-block `cores` override that exists (warp fc8912f2f, services.yml), api g4
  cores 4 for at least 72 hours through sweep phases, with `gmp_apply.sh`'s auto-revert watcher.
  The owner approved it on 11-13 ("Both"); it never ran.
- **Evidence:** the scheduler is 21.9–26.9% of an api process (`findRunnable` 13.7–17.3%,
  `sysmon` 6.1–6.8%; 20.7% in CPUOPT5) at 1.4–1.6 cores per process on 8 Ps.
- **Saving:** if 4 Ps remove a third of the scheduler's work, 7–9% of api CPU: **1–2 cores**
  *(inferred)*.
- **Risk:** low–medium; a runtime knob, reverted on any rise in the control route's phase
  latencies or the in-flight gauges. The sweeps are the stress case.
- **Proving test:** none in code; the A/B compares api g4's cores per krps and route latency
  against g1–g3 hour by hour.
- **Owner rules:** no timeout or deadline changes; one per-block value.

#### #3 A typed codec for the client-score export (gob → binary)

- **Change:** encode the export's score samples (`encodeClientScoreGobValue`,
  model/network_client_score_phase_metrics.go:149) with a hand-written binary codec for
  `ClientScore`, under a versioned key beside the gob one: readers that accept both first (the
  FindProviders2 page readers in api, alt and proxy), then the writer, then gob's removal, as
  `:native_cap_v1` was added.
- **Evidence:** the gob phase is 1.075 s/s, CPU-bound reflection (731 encodes/s, 18.7 MB/s);
  13–21% of the taskworker process that exports, of which `encodeMap` alone is 16% in the edge-1
  capture: `ClientScore` carries five maps (`Scores`, `Tiers`, `ScaledWeights`, `PassesMinimums`,
  and `LookbackClientScores`, whose copies carry their own), and the GC runs 14–19% in the
  exporting captures against 9% without. On the read side, find-providers2 spends 10–23 ms of its
  15–29 ms per request decoding gob pages (perf A and B); the api's page cache answers 52% (88
  hits/s against 82 misses/s) and evicts at its miss rate (64 MiB per process, a constant).
- **Saving:** **1.1–1.6 cores**: 0.8–1.0 taskworker (encoding, and the GC it drives) and
  0.3–0.6 api (28 find-providers2/s at 10–23 ms of decoding), plus alt's and the proxy's
  decoding (Part B).
- **Risk:** medium: a format read on every find-providers2 page miss; dual-read before the switch.
- **Proving test:** a round-trip test over generated samples (every field, modern and other pages,
  capability copies, lookback copies) that the binary decode equals the gob decode, and a test
  that a reader given either format returns the same providers.
- **Owner rules:** additive key, readers before writers; no timing change.
- **A cheaper first step** with an uncertain gain: a larger page-cache bound (a settings field with
  one default) on one block, judged by `urnetwork_client_score_page_cache_lookups_total{result}`
  and find-providers2's CPU; pages are reshuffled on every export (11–21 an hour), which caps
  what any cache can reuse.

#### #4 Remember verified chains on the probe's stock-TLS dials (D4(b)'s remainder)

- **Change, in two steps:**
  1. **Name the transport:** a handshake counter per stock transport (DoH client, streaming POST),
     so the next profile is attributable. Deterministic test: each transport's handshake counts
     once under its own label.
  2. **If it is the per-tunnel DoH clients, as the per-attempt arithmetic says,** let their
     verification consult a process-wide verified-chain cache with D4(b)'s keys and checks: exact
     certificate bytes, server name and root pool; hostname, validity window and key possession
     on every handshake; a miss verifies in full and errors are never remembered. Each tunnel
     keeps its own DoH cache, session cache and admission state, so nothing on the wire changes:
     every handshake stays a full one. The chain cache left the probe's paths through the exit out
     by design, so this needs the owner's probe-design review.
- **Not this:** sharing the DoH session caches or control-plane connections across tunnels
  (CPUOPT5 C #7) would save the handshakes too, but changes what the probe sends through each
  exit (resumption), which tunnel.go's "independently owned DNS cache" rules out.
- **Evidence:** §3.4: x509 6.5–8.0% of every probe process, all under `persistConn.addTLS`
  (P-384 5.8–7.3%); 7.5–9 ms of it per attempt in two captures.
- **Saving:** about **0.9 taskworker cores** *(inferred from three captures)*.
- **Risk:** low–medium: a verification path.
- **Proving test:** D4(b)'s cache tests against the DoH path: a changed byte in any certificate,
  another server name, another root pool, or a clock outside a validity window verifies in full
  and refuses as the stack would; an exact repeat is a hit; a tunnel's DoH handshake is still a
  full handshake (no resumption) with or without a hit.
- **Owner rules:** no probe timing, bound or wire change.

#### #5 The lag-guest gate without a claims decode (CPUOPT5 C #6, not void)

- **Change:** in `byJwtLagGuestUnverified` (session/by_jwt.go:803), after the cache peek, decode
  only the payload segment's base64 and look for the `"lag_guest"` key, which the server's encoder
  writes only for a guest (`json:"lag_guest,omitempty"`); decode the claims only when it is
  there.
- **Evidence:** `requestLagGuestCredential` 2.7% and 2.6% of an api process (A, B), nearly all of
  `ParseUnverified`'s 2.8% and 2.4%: every request with a bearer token passes the gate, discards
  included, and any token the process has not verified in the last 10 minutes is decoded in full.
- **Saving:** about **0.35–0.4 api cores** (85–90% of 2.6% of 16–18 cores).
- **Risk:** low. A token without the key decodes to `LagGuest == false` today; a token that spells
  the key another way can only come from outside the server's encoder, fails verification, and
  is refused by authentication as it is now.
- **Proving test:** for every claims shape the server mints (guest and not, every optional
  claim), the pre-filter's answer equals the full decode's; a forged token with the key in other
  spellings or escapes is still refused on a gated route.
- **Owner rules:** the gate's decision is unchanged for every token the server issues; nothing
  about verification, revocation or the per-address limits changes.

#### #6 Remember tokens the legacy gates accept

- **Change:** `byJwtVerifiedCache.store` refuses tokens past their expiration or without one, so
  the 977/s that the auth migration gates still accept (929 expired, 48 without `exp`) are
  verified in full on every request. Store them too, with the cache's 10-minute bound; every
  lifetime check, gate included, already runs per request after the lookup, so turning
  `reject_expired` on refuses a remembered token on its next request.
- **Evidence:** §4.1; 62% of the cache's misses (977 of 1,567/s). A full verification costs about
  0.26 ms (perf B: `parseByJwtSigned` 2.3% of 1.59 cores over 132 misses/s; P-256 1.3%). A stuck
  provider's token returns to the same process about every 100 s (5 s × 20 processes), so a
  10-minute entry would answer about five of every six.
- **Saving:** **0.2–0.45 api cores** (977/s now, 1.75k/s at the top of a sawtooth). It overlaps
  #1: the expired tokens ride on the replays (r = 0.989), so a working recycle removes most of
  them as well; this item keeps what is left cheap.
- **Risk:** low.
- **Proving test:** with the gate off, an expired token verifies once and is then answered from
  the cache; with the gate turned on between two requests (fixed clock), the second is refused;
  a missing-`exp` token's entry ends at 10 minutes.
- **Owner rules:** nothing is accepted that is not accepted today, and no entry outlives the
  cache's bound or a gate change.

#### #7 PgBouncer direct path, stage 2 for the api (§4.2)

- **Change:** stage 2 of the owner's plan (`pgBouncerDirectBlocks` for every api block), after the
  one-at-a-time PgBouncer restart; with it, pool destroys counted by reason (§4.2).
- **Evidence:** §4.2: 24 hours clean, new connections as cheap as through nginx.
- **Saving:** about **0.7 edge-2 cores** (the api's 31% of the 2.2-core nginx hop) and about 42 µs
  per api query. At Part D's measured 5.07-core hop it is about 1.5 (D #1).
- **Risk:** medium; the backlog restart first; stage 1's triggers per block.
- **Proving test:** `TestPgBouncerDirectRolloutStage` pinned to the stage; a deterministic pool
  test that a connection closed under the pool is counted under its destroy reason.
- **Owner rules:** both paths stay (the code chooses per block); no timeout moves.

#### #8 An index for the location and network-name searches

- **Change:** `SearchLocal.aroundIdsRawN` (search/search_local.go:259) answers a fuzzy query by
  scanning every alias of every value, a histogram check each and an edit distance for the
  survivors, on 8 goroutines. Give it an index that yields the same candidate set (for example
  aliases bucketed by length and by a histogram signature, so only buckets within the distance are
  scanned), with the scan kept as the reference.
- **Evidence:** one find-provider-locations query (a location search at distance 2 over every
  prefix alias of 3 or more characters) fell inside capture B: 5.3% of the process, about 2.5
  core-seconds. The route runs 0.15/s fleet-wide at 362 ms mean on 8 goroutines (up to 2.9
  core-seconds each). network-check runs the same scan over network names at distance 1, 0.35/s at
  46 ms.
- **Saving:** about **0.3–0.45 api cores** *(inferred: one query in a capture and the routes'
  latency)*; lower latency for both routes.
- **Risk:** low–medium: search results must stay identical.
- **Proving test:** a property test over a generated corpus and queries that the indexed search
  returns exactly the scan's results and distances (ties included), at distances 1–3.
- **Owner rules:** no timing or search-semantics change.

#### #9 The unique-exit sweep: stop inside its bound and resume

- **What happens now** (Mimir, and the taskworker's lines for this task in Loki, ids masked:
  `<scratch>/C/loki_upe_sweep.txt`): `SweepProxyExits` walks the live assignments oldest first
  (`loadLiveProxyExitAssignments`, `ORDER BY assigned_at LIMIT SweepBatchSize`) under a 10-minute
  `MaxTime` (taskworker/work/proxy_unique_exit_work.go:21).
  - The last runs that finished (09:35–09:57Z) read the full batch, `live=10000`, and spent
    themselves on held proxies: `held=1022`, `1846`, `1591` per pass (a held proxy's replacement
    draws, each a `FindProviders2` call, found no unique exit), against `replaced=31–43` and
    `failing+264–609`.
  - Since then every run ends `eval error(600.0s) (reschedule) … SweepProxyExits({}) = Timeout`:
    seven in a row from 10:07:28Z to 11:09:47Z, each cutting 750–1,680 assignments ("[upe]sweep
    proxy …: context canceled" / "Done"). A cut run never reaches its Post, and the framework's
    error reschedule starts the next one from the oldest assignment again, so the same prefix is
    re-walked and the newest assignments are not swept.
  - Runs took 0.12 s on 11-14 06Z, 23 s at 11-15 08Z and 66 s at 09Z as live assignments grew
    from 1.8k to 7.9k with the sweeps' hosted proxies.
- **Change** (the shape of CPUOPT5 #2's fix for the shadow step):
  1. the walk stops at its own deadline inside the 10 minutes and returns through the Post, which
     schedules the next run;
  2. a cursor (assigned_at, assignment id), kept like the contract expiry's
     `ContractExpiryCursor`, lets the next run continue where this one stopped, and wrap;
  3. a held proxy is retried only when the unique-exit supply it was held against has changed
     (the sweep already keeps a supply snapshot, `urnetwork_proxy_unique_exit_supply_*`), instead
     of a fresh set of draws every `HeldRetryInterval`.
- **Saving:** about **0.2 taskworker cores** that now loop (6.1% of the 2.79-core process it fell
  into), plus the database and Redis reads of 1–1.8k futile draws a pass; and the sweep finishes
  its passes again.
- **Risk:** low.
- **Proving test:** with a stub clock and a candidate source that takes a fixed time per draw, a
  sweep over more assignments than one run can finish stops before its bound, schedules the next
  run, and the next run starts after the last assignment handled (every assignment visited once
  per cycle); a held proxy is not drawn for again while the supply snapshot is unchanged, and is
  drawn for on the next pass after it changes.
- **Owner rules:** the 10-minute bound stays; no unique-exit rule changes
  (HOSTED-PROXY-UNIQUE-EXIT.md); a held proxy is still retried whenever a unique exit could have
  appeared.

#### Small items

- **Decode each client's secp256k1 key once:** `GetClientContractMetadata` decompresses the
  client's public key from its key-history record (`decodeStClientKeyHistoryRecord`, a square
  root per call) on each contract: 0.5% of an api process and 0.4% of a taskworker one, about
  0.1 cores. A bounded cache keyed by the record bytes, with a test that a changed record decodes
  afresh.
- **api network softirq** (15–17% of an api process's samples): host posture, Part A.

### 7. Considered and rejected

- **Lowering argon2's cost** for the sign-up bursts (11-14 19:34–19:53Z: 25 sign-ups/s took the
  api from about 22 to 28–29.6 cores): a security bound (CPUOPT5 C §5).
- **Turning on `reject_expired`** to end the 929/s expired-token verifications: it is the auth
  migration gate, a decision about old clients, not a CPU change (§8).
- **A longer JWT cache lifetime than 10 minutes:** the misses that remain are tokens the cache may
  not hold (#6) and clients that return to the same process less than every 10 minutes; a longer
  age buys little and holds claims longer.
- **Skipping the lag-guest and intended-network gates for audit-only packs** (3.4k/s): the owner
  kept both for those packs (connect_control_discard.go, owner question 1 answered (a)); #5 makes
  the gate cheaper instead.
- **Raising the api pools' `conn_max_idle_time`** to keep connections: a timeout, and the churn is
  not idle closes (§4.2).
- **GOGC or a memory limit for the api:** one GC cycle per process about every 80 s on a 3 GB live
  heap (0.20–0.27 cycles/s fleet-wide since 11-08, unchanged); more memory for little CPU.
- **Lowering the unique-exit sweep's cadence, raising its 10-minute bound, or retiring it:** the
  owner's design (HOSTED-PROXY-UNIQUE-EXIT.md) and a bound; #9 makes a run stop in time and
  resume instead.
- **Caching the probe's target TLS** (the exit's certificate): it is what the probe judges.
- CPUOPT5 C §5 and CPUOPT4 C §6 stand: no replay answers before verification, no cached
  revocation, no shorter companion-origin waits, no probe-timing changes, no sampled shadow.

### 8. Noticed, not CPU

- **The proxy hosts are saturated in sweep phases.** crisp and fireside read 23.9 busy cores of 24
  in the sweep hours (11-14 22Z–11-15 00Z, 11-15 04–06Z) with the proxy at 41–45 cores. alt shares
  them (§3.5); its latency has held. Part B's.
- **Every service restarted at 11-15 01:11–01:14Z** with no build change, when the config release
  reached the hosts (intro). It reset the connect generation (the replay sawtooth) and the api's
  caches.
- **The brief's "running now" builds were replaced at 09:48–09:57Z** (11d5a8e78).
- **Expired client tokens follow the replays** (929/s, r = 0.989 with replays, 0.22 per replay):
  the stuck providers' old builds present expired tokens, accepted because `reject_expired` is
  off. The recycle (#1) reconnects them but does not renew an old build's token.
- **`no_active_row` refusals run 80–110/s:** tokens of removed clients, each verified in full and
  refused on the state check.
- **find-providers2 at the api** is 86 ms mean over 6 hours (about 40 ms before 11-12 19Z; 105–140
  ms on 11-12's evening, CPUOPT5 C §6): still a wait, not CPU.
- **The unique-exit sweep no longer finishes a pass** (#9): since about 10Z every run is cut at
  its 10-minute bound, so the newest assignments are not swept while the oldest are re-read.
- **The points leaderboard rebuild** ran 7 times in 24 hours (every 1–4 hours in this window, not
  hourly at :11), 110–197 s each.
- **connect's JWT cache is full** (65,536 entries per process; evictions equal stores) and answers
  24% of connect's verifications. Part A did not measure it.
- **The api's threads carry 15% network softirq** (8.3% in CPUOPT5): receive processing that lands
  on the CPUs the api runs on, a host-posture question for Part A.

## Part D — PostgreSQL on edge-2, and its PgBouncer layer

This is research, not a change. Every number comes from read-only measurement of
Main's primary, by-us-fmt-5-edge-2, on Sunday 2026-11-15 between 10:05Z and
11:06Z, plus Mimir history from 11-07. Nothing in production was changed: no
write, DDL, index build, setting change, restart or counter reset. No
identifiers, addresses or literals are printed here. *(inferred)* marks a claim
reasoned from code or arithmetic rather than measured.

**Revisions in the window** (`urnetwork_build_info`, release logs, git):
- **api, taskworker, alt:** a gift-gamma release was rolling during the window.
  - Migration 977 was applied at 09:41:34Z (`migration_audit` 976→977).
  - The builds are taskworker `2026.11.15-outerwerld+1099357100` (processes
    from about 09:49Z) and api `+1099362010` (from about 09:57Z). alt was going
    from `2026.11.14-outerwerld+1098822500` (6c0a7ffb6, D50a) to
    `+1099363220`.
  - The new builds come after 6c0a7ffb6 (D50); server main is at 11d5a8e78
    (merge of feature/gift-gamma at 09:40:28Z).
  - Code is cited at 11d5a8e78; nothing between 6c0a7ffb6 and 11d5a8e78
    changes the SQL ranked below, except the gift-gamma tables (measured at
    under 0.001 backends).
- **connect and mcp:** D47 (`2026.11.14-outerwerld+1098366310`,
  `+1098368160`), from aa7142599.
- **gossip:** `2026.11.13-outerwerld+1098358910`.
- **proxy:** D49p (`+1098770070`), from f43488da1.
- **Restarts before the window:** connect, gossip, mcp, proxy and two alt
  processes all restarted at about 01:12Z, when the config release
  `2026.11.14-outerwerld+1099044420` reached the hosts.
- **xops:** origin/main as checked out on this Mac (playbook-dbs.yml,
  pgbouncer.ini.j2, nginx stream unchanged since CPUOPT5).

### 1. Summary

**edge-2 is back up about 13 cores since 11-14 ~14Z, because the bot sweeps'
hosted proxies doubled its work; per unit of work it costs what it did at
CPUOPT5.** From 11-14 14Z to 11-15 10Z it ran 42.6 busy cores (40.5 without
iowait) at 41.6k commits/s, against 29.5 (26.5) at 23.9k on CPUOPT5's 3-day
basis. Cores per 1k commits/s fell from 1.24 to 1.02 (the floor is spread over
more commits). Busy sits +3.7 to +4.2 cores above CPUOPT5's fit, so the added
work costs a little more per commit than the old marginal rate (0.74–0.79 cores
per 1k commits/s here, 0.54–0.57 before). That extra is mostly the two database
path hops. PgBouncer queries per commit rose from 2.24 to about 2.5, so the
hops grew from 5.1 to 12.2 cores while commits grew 1.74×.
- **Pool acquires:** 22.2k → 41.3k/s in all.
  - proxy 0.7k → 5.3k/s (×7.3);
  - connect 9.3k → 17.6k (×1.9);
  - taskworker 4.0k → 6.6k (×1.65);
  - api 8.2k → 11.9k (×1.45).
- **Contracts** are created and closed at about 1,300–1,440 a second: the
  close UPDATE runs 1,367/s and the escrow insert 1,380/s, against 670–674 at
  CPUOPT5. Live hosted devices: 13.6k → 43.7k.
- **The sweeps** come every 38 minutes (median of 15 cycles since 00:00Z) and
  add about 7k live hosted devices in about 20 minutes. On a clean cycle the
  peak adds 3–10k commits/s and 3–9 cores over the trough. This rides on the
  sustained +13 cores, not instead of it (§3.1).

**Where the CPU goes** (20 s `/proc` census at 10:17:22Z, 53.0 cores busy
without iowait, softirq 5.4, sys 12.8):
- backends through PgBouncer 29.7 cores (665 processes), direct backends 3.1
  (137);
- PgBouncer 7.1 and the nginx stream hop 5.1: the two hops are 12.2 cores, 23%
  of busy (CPUOPT5: 5.1, 16%);
- autovacuum 0.7, dm-crypt 0.7, IO workers 0.6, parallel workers 0.4 (the
  award read below).

perf (30 s at 10:14:30Z) agrees: postgres 76% of non-idle samples, PgBouncer
12.6%, nginx 9.0%. PgBouncer and nginx are 82% and 90% kernel.

Per unit, nothing moved:
- PgBouncer 64 µs per query (CPUOPT5 56), nginx 45 µs (42);
- backends 266 µs per PgBouncer query (257);
- pgss execution, against CPUOPT5's 0.324 ms in its main window and 0.275 ms
  pin-free:
  - 0.309 ms per commit over 10:10–11:05Z;
  - 0.287 ms without the award's reads;
  - 0.258 ms after the award's sweep read, without its reliability read.

**Four findings that are new:**
1. **Connection churn.**
   - connect creates 10.5 pool connections a second, with no idle or lifetime
     destroys counted, so its connections die from errors.
   - PgBouncer replaces 2.1–2.7 server backends a second at constant pool size.
     Of the 16 servers that vanished in a 60 s reading, 15 were younger than
     the 600 s idle timeout. Of 666 server backends, 160 were born in the last
     minute.
   - PostgreSQL logs "incomplete message from client" about 3 times a second.
   - Each new backend re-prepares its statements: PgBouncer's server parses
     are 884/s, 0.9% of binds, against 0.1% at CPUOPT5. Each also re-plans and
     rebuilds its caches (§3.6).
2. **One statement in fourteen, and one PgBouncer query in twelve, is the
   announce loop's provide-key change count:** 9.3k/s at 18 µs of execution
   each, every one a separate round trip through both hops
   (connect/transport_announce.go:755, model/network_client_model.go:3846).
3. **Pool pressure comes from idle-in-transaction time, not from
   execution.**
   - PostgreSQL spends 252–289 s/s idle in transaction against 15–17 s/s
     active.
   - PgBouncer's servers are in a transaction 322 s/s, running a query 42 s/s.
   - The largest single bucket is the gap between a transaction's BEGIN and
     its first statement: 34–46 servers at a time, about 3.9 ms per
     transaction by Little's law.
   - The next are connect's close and report transactions, which last 200–900
     ms with gaps.
4. **The weekly operator-block award re-reads the whole week's sweeps.**
   - That is about 300 statements of 4.6–13 s each, 10:04–10:46Z. The
     taskworker restart at 09:49Z started it 15 minutes later.
   - It held about 1 backend and 4 parallel workers, with 168–210 MB/s of
     parallel-worker reads and 292 ms of JIT per statement.
   - Its reliability phase then held about 1 more backend from 10:41Z.
   - The hourly rollup (D47/D48) has already read the same stretches with the
     same statement.

**CPUOPT5 scorecard** (§4):
- **#1 shipped.** The shadow loop ended 11-13: epoch 4 retired by b4374ac08,
  sliced reads by 95306eb79. No 30-minute pin since 11-13 08:00Z.
- **#2 is half built.**
  - The direct path is live on api beta since 11-14 ~08:00Z, and its 24 h
    watch passed.
  - nginx is now 5.1 cores, not 2.2.
  - Over 30 h with the pools at the cap, a new direct connection's ping
    averaged 16 ms on api beta. connect beta, still on nginx, averaged 27 ms;
    g1–g4 averaged 7–13 ms. So the "190 ms" was a beta-wide peak, not the
    path.
- **#4 is three-fifths done.** Three of O2's five partials are rebuilt;
  `transfer_contract_outcome_null` (18 GB) and `…_open_partial_create_time`
  (9.9 GB) still hold 396k entries.
- **#3 and #5** were not chosen by the owner.
- **#6–#10 are not done.**
  - The descendant check grew to 0.49–0.68 backends.
  - The SN25 amounts read is now 1.41 s a call (0.31 backends): its table grew
    from 449k to 701k rows.
  - JIT is 0.18–0.26 cores, 58–72% of it in one prober-retention statement
    that spends 92–93% of its time compiling.

**New costs** (§5):
- **Hosted proxies:** the bot sweeps (not a release) cost +13 cores.
  Deactivations doubled with the sweeps and D49p's 10-minute close of
  zero-byte devices: descendant checks ran 358–371/s, against 187 at CPUOPT5.
- **Operator-block award re-read:** about 0.8 backends plus 4 parallel
  workers for 42 minutes, then the reliability phase. Averaged over a week it
  is small; it matters because any taskworker restart while a block is due
  starts it.
- **Hourly bursts:**
  - the devices-series count: about 1,000 statements, 713 s an hour (0.2
    cores averaged);
  - the contract_close orphan sweep: about 200 statements, 433 s an hour
    (0.12).
- **Small:**
  - GET /account/epochs' served-bytes read: 318 calls, 0.1 s in 27 minutes;
  - the leaderboard rebuild and D50t's streak skip: did not run in the window;
  - operator_block_points_expired: trivial;
  - Gamma: under 0.001 backends;
  - the 8327 trim: a one-off at 20:23Z on 11-14.

**Ranked opportunities** (cores now, close to the evening peak; §6):

| # | Change | Est. saving | Risk |
|---|---|---|---|
| 1 | Finish the approved direct path, stages 2–4, and so remove the nginx stream hop (CPUOPT5 #2) | 4.5–5.5 | medium; staged, plan exists |
| 2 | Answer the announce loop's provide-key change count from a pessimistic Redis marker in the mirror it already reads | 1.2–1.6 *(inferred)* | medium |
| 3 | Send BEGIN and its lock bound with the transaction's first statement (lazy BEGIN; extends CPUOPT5 #6) | 1.0–1.4 *(inferred)*; frees ~40 servers on average | medium |
| 4 | End connect's connection churn (10.5/s): keep pgx connections across stops it can survive | 0.7–1.5 *(inferred)* | medium (Part A) |
| 5 | Descendant check from a partial index of active clients by device (CPUOPT5 #8) | 0.4–0.6 | medium (index on a hot table) |
| 6 | JIT off where it compiles every call: prober retention window, the award read, the health aggregate (CPUOPT5 #9) | 0.2–0.25 | low |
| 7 | An index for the SN25 amounts read (CPUOPT5 #7) | 0.3 | low |
| 8 | Finish O2's last two partials; then the reaper-thinned full indexes (CPUOPT5 #4); compact the two bloated queue tables in the same kind of window | 0.1–0.2 partials, 0.5–1 full indexes, 0.15–0.3 queue tables *(inferred)* | medium (window) |
| 9 | Health-window aggregate and GetTransferStats (CPUOPT5 #10) | 0.2–0.3 | low–medium |
| 10 | Fewer probes per claimed task, and one statement for owned-session setup | 0.2–0.4 *(inferred)* | low–medium (Part C) |
| 11 | The award reads the rollup it already keeps, not the week again | ~0.01 averaged; 1–2 cores and ~200 MB/s for about an hour a week | medium |

Together about 10–13 cores against edge-2's 42.6-core mean, with overlaps:
- **#2 and #3 overlap #1.** Part of their per-round-trip saving is nginx's. After
  #1 they shrink to about 0.9–1.2 and 0.7–1.0.
- **#4 overlaps #3** only in the reconnects' own round trips.

**Owner items:**
- the windows for #1 (stages 2–4) and #8 (O2 already approved);
- the PgBouncer cap, which is the owner's call (§3.6, modeled: no CPU saving;
  needs a restart or a smaller direct budget);
- #11, the award's semantics.

### 2. Method

**When and load.**
- **Window:** Sunday 11-15, 10:05–11:06Z. edge-2 ran 47–59 cores (including
  iowait) at 42–46k commits/s, above the 42.6-core mean of the bot era.
- **pgss windows:**
  - 1→14 (10:10:44–10:47:12Z, 2,189 s), the detailed ranking: 43.8k commits/s
    and 135.1k statements/s;
  - 1→20 (to 11:05:13Z, 3,269 s), all of it;
  - 14→20 (1,080 s), after the award's sweep read.
  - PgBouncer counted 111.7k queries/s in 41.9k transactions/s at 10:18Z.
- **What else ran:**
  - the operator-block award (§3.3), 10:04–10:46Z, then its reliability phase
    from 10:41Z;
  - transfer_contract's autovacuum, in its index phase throughout (4 of 16
    indexes done by 10:44Z);
  - the gift-gamma roll: api processes restarted 09:57Z, alt processes in the
    window.

**Captures** (raw outputs at the end of this section):
- **pgss:** snapshots every 3 minutes, 10:10:44–11:05Z (snapshots 1–20), no text,
  never reset. Each is one statement, 2–8 s, with pg_stat_database, _wal,
  _io, _checkpointer, _bgwriter, _user_tables, activity counts by state and
  path, transaction ages and vacuum progress.
  - The extension is 1.10 with 4,759 entries, 60 deallocations since its 10-23
    reset and none in the window. `track_planning` is off and `track_utility`
    on.
  - Rows are summed per queryid across users, because the same queryid
    appears once per role.
  - Texts were fetched once, in four statements, for the 917 active queryids,
    plus two pattern reads (feature tables, temporary tables). Strings, UUIDs
    and long numbers were stripped on the server.
- **Catalog and settings:** pg_settings, migration_audit, indexes and sizes of
  the hot tables, transfer_contract's indexes, pg_stat_sys_tables twice
  (catalog churn), backend ages by path, idle-in-transaction states by
  query_id (three samples), and the PostgreSQL log as counts by message kind
  (10 minutes, root, no identifiers).
- **Host:**
  - two 20 s `/proc` censuses at 10:16:42Z and 10:17:22Z, non-root;
  - one system-wide perf capture, 30 s at 99 Hz with call graphs,
    10:14:30–10:15:03Z (262,931 samples);
  - TCP counters (`nstat`).
  - The perf capture ran with the validator idle (intent 26354 finalized, 1,476
    s to its boundary at 10:14:14Z), as root through by-pass and `sudo -S`. Its
    reports ran under `nice 19`; the data file was deleted and no perf process
    was left.
  - PostgreSQL is the PGDG 22.04 build without debug symbols. Several of its
    top entries stay hex addresses, and callers inside PostgreSQL were not
    attributed.
- **PgBouncer:**
  - SHOW STATS on all 32 admin sockets twice, 24.3 s apart, plus SHOW POOLS,
    SHOW LISTS, SHOW CONFIG and the listen backlogs, at 10:18Z;
  - SHOW SERVERS on four instances twice, 60 s apart, at 10:31Z;
  - four instances' logs as counts by kind.
  - All as the postgres OS user, read only, as pgbouncer-pool-exporter does.
- **Mimir:**
  - edge-2 CPU by mode, pg_stat_database, activity, PgBouncer pool series,
    pool acquires and connections created by service, initial_ping by block,
    the api routes, live hosted devices and node netstat;
  - 11-07 to 11-15 at 5-minute steps, the last 30 h at 1-minute steps.

**Load on Main from this part:** about 45 statements through `support db
query`, 20 of them snapshots. Each was read-only with a 30 s cap; most took
2–4 s, the longest 8 s (a snapshot during the perf capture). No snapshot was
held between statements, and no read touched a large table's heap except
through the stats views.

**Not captured:**
- **EXPLAIN** (the tool takes only SELECT and WITH), so plans are inferred.
- **Planning time:** `track_planning` is off, and perf shows planner functions
  without callers.
- **Statements per service:** PgBouncer's peers are all nginx's loopback
  address, except api beta.
- **A clean quiet-against-sweep pgss pair:** live hosted devices stayed flat
  at 40–43k from 10:15Z, so the sweep is costed from Mimir cycles instead.
- **An award-free pgss window:** the award's reliability phase was still
  running at the last snapshot used.

**Refused or skipped:** nothing was refused. Skipped: a second perf capture
(the brief allows one), and per-PID attribution of the log's "incomplete
message" lines (the log prefix has no host).

**Raw outputs.** Under `<scratch>/D/`: scripts in
`scripts/` and `host/`, SQL in `sql/`, outputs in `raw/`. Query text is
pg_stat_statements' normalized form with strings, UUIDs and long numbers
stripped on the server.

- **pgss:**
  - `snap-{1..20}.json`, `snaploop.log`;
  - `pgss-delta-1-4.tsv`, `pgss-delta-1-11.tsv`, `pgss-delta-1-13.tsv`,
    `pgss-delta-1-14.tsv`, `pgss-rank-1-14.txt`, `families-1-14.txt`;
  - final windows: `pgss-delta-{1-20,14-20}.tsv`,
    `pgss-rank-{1-20,14-20}.txt`, `families-{1-20,14-20}.txt`;
  - `texts-1.tsv`, `texts-2.tsv`, `texts-3.tsv`, `texts-4.tsv`,
    `texts-features.tsv`, `texts-temp.tsv`, `texts-all.tsv`;
  - `horizon-1.txt` (xid rate and horizon age).
- **Catalog and settings:**
  - `columns.txt`, `settings-1.txt`, `indexes-nc.txt`, `tables-1.txt`,
    `tc-indexes.txt`;
  - `catalog-1.txt`, `catalog-2.txt`, `backend-ages-1.txt`;
  - `iit-1.txt`, `iit-2.txt`, `iit-3.txt`, `pglog-1.out`, `pglog-2.out`.
- **Host:** `census-1.out`, `census-2.out`, `perf-1.out`, `pgb-1.out`,
  `pgb-2.out`.
- **Mimir:**
  - `m1-30h.json`, `m5-8d.json`, `m1-snapwin.json`, `m-step0920.json`,
    `m-listen-3d.json`, `m-initialping-30h.json`;
  - `timeline-10m-30h.txt`, `timeline-1m-0700-1010.txt`,
    `windows-evening.txt`, `sweep-cycles.txt`, `initial-ping-by-pcap.txt`.
- **Scripts:**
  - `scripts/{mimir,timeline,windows,delta,families}.py`,
    `scripts/{snap,snaploop,final}.sh`, `sql/snap.sql`;
  - `host/{census.py,perf.sh,pgb.sh,pgb2.sh,pglog.sh,pglog2.sh}`.

### 3. Where the CPU goes now

#### 3.1 Host, evenings, per commit

Means of 5-minute rates (`windows-evening.txt`).
- **Residual** is busy minus CPUOPT5's fit (16.4 + 0.54 × commits in k/s);
  **res NI** uses the no-iowait fit (12.6 + 0.57 ×).
- **Acquires** are per second: connect / api / taskworker / proxy.

| Window | Busy | No iowait | Commits k/s | Cores per 1k/s | Residual | res NI | Pools at cap (mean of 5-min) | Acquires | Live hosted devices |
|---|---|---|---|---|---|---|---|---|---|
| 3 d to 11-13 04Z (CPUOPT5 basis) | 29.5 | 26.5 | 23.9 | 1.24 | +0.2 | +0.3 | 4.7 | 9.3k / 8.2k / 4.0k / 0.7k | 13.6k |
| 11-11 18–22Z | 29.9 | 26.7 | 24.5 | 1.22 | +0.3 | +0.1 | 4.4 | 9.1k / 8.8k / 4.1k / 0.7k | 14.0k |
| 11-12 18–22Z | 29.7 | 27.6 | 24.7 | 1.20 | 0.0 | +0.9 | 3.0 | 10.0k / 7.9k / 4.1k / 0.9k | 13.7k |
| 11-13 18–22Z | 35.1 | 33.1 | 34.6 | 1.01 | 0.0 | +0.8 | 3.9 | 13.0k / 12.3k / 5.2k / 2.4k | 26.8k |
| 11-14 00–13:45Z | 30.6 | 28.4 | 29.5 | 1.04 | −1.7 | −1.0 | 2.4 | 12.1k / 8.5k / 4.9k / 2.1k | 23.8k |
| 11-14 18–22Z | 43.7 | 41.5 | 41.6 | 1.05 | +4.9 | +5.2 | 18.8 | 17.4k / 13.2k / 6.3k / 5.2k | 55.7k |
| 11-14 14Z – 11-15 10Z | 42.6 | 40.5 | 41.6 | 1.02 | +3.7 | +4.2 | 13.7 | 17.6k / 11.9k / 6.6k / 5.3k | 43.7k |
| 11-15 02–09Z | 42.8 | 40.6 | 41.8 | 1.02 | +3.8 | +4.1 | 11.5 | 17.8k / 11.1k / 6.8k / 5.6k | 41.3k |

- **The step is 11-14 13:50Z.** In ten minutes commits went from 27k to 40k/s
  and busy from 29 to 40 cores (`timeline-10m-30h.txt`). The wave had begun on
  11-13's evening.
  - D49p (18:38Z) dropped live devices from 59k to 19k.
  - They were back to 42–91k within the hour, and since 22:00Z have ranged
    24–50k.
  - auth-client and remove-client grew only 9–12%: the hosted devices, not new
    network clients, carry the load.
- **Sweep cycles since 00:00Z** (`sweep-cycles.txt`). The cycle is a trough to
  peak of live hosted devices with 7-minute smoothing.
  - 15 cycles, with a median period of 38 minutes and a median rise of 7.3k
    devices.
  - On the clean cycles, busy rose by 3–9 cores and commits by 6–10k/s:

    | Cycle | Busy | Commits/s |
    |---|---|---|
    | 02:18→02:39 | +9.4 | +10.2k |
    | 06:34→06:46 | +4.1 (no iowait) | +6.9k |
    | 07:10→07:25 | +4.3 | +6.5k |
    | 07:43→08:05 | +3.2 | +7.3k |

  - proxy acquires rose 3–4k/s and connect 2–3.5k/s.
  - Peak pools at the cap: 14–31 of 32.
  - Overlapping cycles and other episodes make the rest noisy (median rise 2
    cores over all 15).
  - The 01:12Z restart of connect and proxy (the config release) cost a +28-core
    spike for about 10 minutes (01:03→01:16).
- **Within the hour, busy swings ±7 cores at constant commits.** It follows
  application active time (11–20 s/s) and the heavy periodic statements
  (§3.3), not commits (`m-step0920.txt`). The 10:00–10:40Z level (50–59
  cores) carried the award read.

#### 3.2 By process

`/proc`, 20 s at 10:17:22Z (`census-2.out`; census-1 at 10:16:42Z agrees within
0.4 cores per class), against CPUOPT5's 04:31:43Z census on 11-13:

| Consumer | Cores now (user/sys) | Processes | CPUOPT5 |
|---|---|---|---|
| backends via PgBouncer (loopback) | 29.67 (24.41/5.26) | 665 | 13.49 (350) |
| PgBouncer | 7.11 (1.87/5.24) | 32 | 2.93 |
| nginx stream (6432 → 6433–6464) | 5.07 (1.03/4.04) | 33 | 2.20 |
| backends direct (LAN, 5432; taskworker maintenance pool) | 3.10 (2.55/0.55) | 137 | 5.75 (120; incl. the shadow read) |
| autovacuum workers | 0.73 | 4 | 1.03 |
| dm-crypt kworkers | 0.67 | 51 | 1.35 |
| IO workers | 0.57 (0.96 in census-1) | 24 | 2.13 |
| parallel workers (the award read) | 0.40 | 8 | — |
| walsummarizer, checkpointer, walwriter, bgwriter | 0.34 | 4 | 0.31 |
| kswapd, kcompactd | 0.23 | 2 | — |
| Grafana's sessions | 0.06 | 24 | — |
| **host busy without iowait** | **53.03** (user 34.86, sys 12.77, softirq 5.39; iowait 2.40) | | 31.56 |

**Per unit**, with PgBouncer at 111.7k queries/s and 41.9k transactions/s
(2.67 queries per transaction, `pgb-1.out`):

| Unit | Now | CPUOPT5 |
|---|---|---|
| PgBouncer per query | 63.6 µs | 56 |
| PgBouncer per transaction | 170 µs | 142 |
| nginx per query | 45.4 µs | 42 |
| backends via PgBouncer, per PgBouncer query | 266 µs | 257 |
| bytes per query, in / out | 174 / 183 | 178 / 237 |

- **The two hops are 12.2 cores and 23% of busy.** With part of the 5.4 cores
  of softirq (loopback and LAN TCP), they are a quarter of the host.
- **Backends spend 29.7 cores against about 12–13 backends of pgss
  execution.** The rest is protocol, executor startup, planning, snapshot,
  commit and socket work, which `total_exec_time` does not count. That is why
  per-statement round trips cost far more than their execution (#2, #3).
- **perf, 30 s at 10:14:30Z** (`perf-1.out`):
  - **By process:** postgres 52.6% of samples, PgBouncer 8.7%, nginx 6.2%, idle
    30.9%. Of non-idle samples that is 76.1%, 12.6% and 9.0%.
  - **Kernel share:** postgres 27%, PgBouncer 82%, nginx 90%.
  - **Leading postgres symbols** (% of all samples):
    - `hash_search_with_hash_value` 2.37, mostly from `StartReadBuffer`: the
      buffer mapping table on every buffer hit (3.7–3.8M hits/s);
    - `_bt_compare` 1.13 and `AllocSetAlloc` 1.02;
    - `heap_page_prune_opt` 0.81 and `heap_hot_search_buffer` 0.58: dead
      versions on hot pages;
    - `GetSnapshotData` 0.55 (with about 850 backends);
    - planner and parser functions, `expression_tree_walker_impl` 0.58,
      `copyObjectImpl` 0.52, `get_relation_info` 0.22 and `base_yyparse` 0.16:
      planning is material (§3.6), though unquantified without
      `track_planning`;
    - `libLLVM` 0.85 (JIT) and `ExecParallelScanHashBucket` 0.37 (the award's
      parallel hash join).
  - **Idle:** `intel_idle_ibrs` is 6.7% of samples, the retbleed IBRS
    mitigation on the idle path. The host still boots the bare command line.
- **PgBouncer is healthy but full.**
  - 1,507 client connections and 685 servers: 374 active, 311 idle.
  - 19 of 32 pools are at 22 or more servers; none are waiting at the reading.
    Mimir shows waits at most 55–172 clients over the bot era.
  - 11.0 client logins/s (CPUOPT5: 3.6). The listen backlog is still 128.

#### 3.3 Inside PostgreSQL: pgss

**Window 1→14, 10:10:44–10:47:12Z (2,189 s)** (`pgss-rank-1-14.txt`,
`families-1-14.txt`):
- 135.1k statements/s, 14.17 backends of execution;
- 3.09 statements and 0.324 ms of execution per commit, 105 µs per statement;
- JIT 0.234 cores, shared hits 3.74M/s, reads 37.8k/s (310 MB/s).

CPUOPT5's main window: 70.1k/s, 7.56 backends, 3.00 statements and 0.324 ms
per commit. Without the award's two reads (0.81 + 0.19 backends) the window is
13.2 backends, 0.301 ms per commit.

**Final windows** (`pgss-rank-1-20.txt`, `pgss-rank-14-20.txt`,
`families-{1-20,14-20}.txt`):
- **All of it, 1→20** (10:10:44–11:05:13Z, 3,269 s): 43.8k commits/s, 135.2k
  statements/s (3.08 per commit), 13.55 backends, 0.309 ms per commit, 100 µs
  per statement, JIT 0.216 cores. Without the award's two reads (0.99
  backends): 12.56 backends, 0.287 ms per commit.
- **After the award's sweep read, 14→20** (10:47:12–11:05:13Z, 1,080 s): 43.9k
  commits/s, 135.4k statements/s, 12.28 backends, 0.280 ms per commit, 91 µs
  per statement.
  - JIT 0.178 cores, 72% of it the prober retention window.
  - The reliability read is 0.96 backends (3.6 s and 25.6k disk reads a
    call). Without it: 11.32 backends, 0.258 ms per commit.
  - CPUOPT5's pin-free window: 6.71 backends, 0.275 ms per commit, 93 µs per
    statement.
- **The steady statements are a little cheaper per call than in 1→14:** the
  close UPDATE 0.513 ms, the contract insert 0.466 ms, the descendant check
  1.34 ms (715 hits), the SN25 amounts read 1.34 s (0.37 backends).
- **14→20 by family:**

  | Family | % exec | Backends |
  |---|---|---|
  | network_client etc. | 27.3 | 3.36 |
  | transfer_contract | 19.7 | 2.42 |
  | contract_close | 11.9 | 1.46 |
  | escrow | 10.1 | 1.25 |
  | reliability (the award's phase) | 8.4 | 1.03 |
  | probe | 5.8 | 0.72 |
  | SN25 | 4.4 | 0.54 |
  | hosted proxy | 2.9 | 0.36 |
  | debit | 2.3 | 0.28 |
  | transaction control | 2.1 | 0.26 |

The tables below are window 1→14, the longest window with the award's sweep
read in it.

**By family:**

| Family | % exec | Backends | % calls | CPUOPT5 % exec |
|---|---|---|---|---|
| network_client, provide_key, device, connections | 31.2 | 4.42 | 23.1 | 23.5 |
| transfer_contract (create, close, pair, lookups) | 19.4 | 2.75 | 13.6 | 14.7 |
| contract_close* | 10.3 | 1.45 | 10.7 | 6.8 |
| transfer_escrow and sweeps | 10.0 | 1.42 | 10.9 | 7.7 |
| URL and egress probe | 5.8 | 0.82 | 5.1 | 17.2 |
| operator block points (the award read) | 5.7 | 0.81 | 0.0 | — |
| SN25 ledger, census, extender, wallets | 3.7 | 0.53 | 0.4 | 4.0 |
| hosted proxy (proxy_client, device config, exits) | 3.4 | 0.48 | 0.8 | — |
| debit journal | 2.1 | 0.29 | 1.8 | 9.1 (pinned) |
| transaction control and locks | 1.8 | 0.26 | 29.2 | 1.7 |
| tasks | 1.0 | 0.14 | 1.3 | — |
| reliability, location, other | 4.3 | 0.60 | 3.1 | — |

The contract lifecycle (the transfer_contract, contract_close, escrow and debit
families) is 42% of execution and runs at about 1.3–1.4k contracts/s, about
2× CPUOPT5. The probe family fell from 17.2% to 5.8%: the slot claim is no longer
pinned.

**Leading statements** (window 1→14; code at 11d5a8e78):

| # | Statement | Code | % exec | calls/s | mean ms | hits/call | CPUOPT5 |
|---|---|---|---|---|---|---|---|
| 1 | operator-block sweep read (award) | model/operator_block_points.go:422, :445 (award :954; rollup model/operator_block_traffic.go:270) | 5.70 | 0.13 | 6,409 (292 JIT) | 4.0M, 177k reads | new |
| 2 | contract insert `WITH creation_clock` | model/subscription_model.go:1993, :2980; subscription_redis_admission.go:566 | 5.62 | 1,263 | 0.63 | 55 | 674/s, 0.36 ms |
| 3 | close `UPDATE transfer_contract SET outcome` | model/subscription_model.go:3825 | 5.32 | 1,367 | 0.55 | 53 | 670/s, 0.43 |
| 4 | close-report upsert `WITH prior AS MATERIALIZED` | model/subscription_model.go:3297 | 3.74 | 3,383 | 0.16 | 46 | 1,640/s, 0.21 |
| 5 | descendant check on deactivation | model/network_client_model.go:1734, from `readActiveChildClientIdsInTx` :1692 | 3.45 | 358 | 1.37 | 636 | 187/s, 2.51 |
| 6 | escrow insert | model/subscription_redis_admission.go:564 | 3.08 | 1,380 | 0.32 | 16 | — |
| 7 | `UPDATE network_client_connection SET connected` | model/network_client_model.go:4283 | 2.48 | 388 | 0.91 | 57 | — |
| 8 | devices series (hourly burst) | model/audit_model.go:486 | 2.30 | 0.46 | 704 | 30k | — |
| 9 | SN25 amounts for a network | model/st_ledger_v2_reader_model.go:86 | 2.16 | 0.22 | 1,406 | 23k | 0.17/s, 1,167 |
| 10 | close-report evidence insert | model/contract_close_report.go:397 | 1.89 | 1,359 | 0.20 | 17 | 673/s, 0.19 |
| 11 | connection insert | model/network_client_model.go:4185 | 1.51 | 390 | 0.55 | 48 | — |
| 12 | contract status with prober case | model/subscription_model.go:2435, :2549 | 1.49 | 710 | 0.30 | 57 | — |
| 13 | egress health-window aggregate | model/provider_egress_health_model.go:806 | 1.47 | 0.08 | 2,680 (48 JIT) | 1.6M | 0.1/s, 2,370 |
| 14 | pair network lookup | model/network_client_model.go:2840 | 1.47 | 2,706 | 0.08 | 17 | 1,779/s, 0.06 |
| 15 | `SELECT active … FOR UPDATE` | model/network_client_model.go:1129, :1495 | 1.41 | 289 | 0.69 | 6 | — |
| 16 | contract_close orphan sweep (hourly burst) | contract_close retention | 1.40 | 0.09 | 2,168 | 596k, 7.7k reads | — |
| 17 | URL-probe slot claim | model/provider_url_probe_due.go:357 | 1.38 | 56 | 3.49 | 880 | 66/s, 9.35 (pinned) / 2.30 (pin-free) |
| 18 | client_reliability valid counts (award's reliability phase) | model/network_client_reliability_model.go:1399 | 1.37 | 0.05 | 3,687 | 26k reads | — |
| 19 | debit-journal insert | model/subscription_model.go:4264 | 1.26 | 1,366 | 0.13 | 18 | — |
| 20 | provide-key change count | model/network_client_model.go:3846, from connect/transport_announce.go:755 | 1.17 | 9,331 | 0.018 | 3 | 5.0k/s |
| 21 | prober retention contract window | model/prober_shard_retention.go:1066 | 1.03 | 0.64 | 226 (209 JIT) | 3.2k | 193 ms, 176 JIT |

- **By calls:**
  - `SET LOCAL lock_timeout` 10.4k/s and `begin` 10.2k/s. These are the same
    simple-protocol message (db.go:1073), counted twice.
  - `commit` 10.0k/s.
  - The provide-key change count 9.3k/s.
  - The close-report upsert 3.4k/s, the close-report read 3.2k/s, the pair
    lookup 2.7k/s, the extender lookup 2.5k/s.
- **Transaction control and locks** are 29.2% of statements and 1.8% of
  execution (`pgss-delta-1-13`). Of them:

  | Statement | Rate | CPUOPT5 |
  |---|---|---|
  | `begin` | 10.3k/s | 4.2k/s |
  | `commit` | 10.0k/s | 4.9k/s |
  | `rollback` | 0.3k/s | — |
  | advisory locks | 6.6k/s | 4.1k/s |
  | `pg_backend_pid` | 2.6k/s | 351/s |
  | `set_config` | 0.8k/s | — |
  | `LOCK TABLE` | 0.8k/s | — |
  | the clock check | 0.7k/s | — |

  - **`pg_backend_pid`'s growth is the taskworker's claim loop.** The claim
    session and queue probe runs 1.44k/s (db_task_claim_ownership.go:63, from
    task/task.go:2230), beside `pg_advisory_unlock_all()` at 584/s and the
    bare `SELECT pg_backend_pid()` at 527/s.
  - That is against 76 polls/s (22 claiming) and 30.8 task executions/s:
    about 47 probes per executed task.
- **Periodic heavy statements** (`snap` series):

  | Statement | Pattern | Each | Calls/s | Backends |
  |---|---|---|---|---|
  | devices series | hourly burst, ~1,000 calls in ~6 min | 0.7 s | — | 0.2 averaged |
  | contract_close orphan sweep | hourly burst, ~200 calls | 2.2 s | — | 0.12 averaged |
  | health aggregate | steady | 2.7 s | 0.08 | 0.21 |
  | SN25 amounts | steady | 1.4 s | 0.22 | 0.31 |
  | prober retention window | steady | 0.23 s | 0.64 | 0.15 |
  | probe cohort | steady | 9.6 s | 0.01 | 0.07 |

- **The award** (`ComputeOperatorBlockPoints`, model/operator_block_points.go:931).
  - It reads the block's window past the cutoff in 30-minute chunks, each its
    own statement on the maintenance pool.
  - About 300 statements ran from about 10:04 to 10:46Z: 6.4 s mean, 4.0M hits
    and 177k disk reads (1.4 GB) each, a 4-worker parallel hash join, and 292
    ms of JIT.
  - The chunks from 11-14's afternoon took 9–13 s each.
  - Its reliability phase (`readOperatorBlockReliability`) followed from about
    10:41Z: 3.7 s and 26k disk reads per statement, about 1 backend, still
    running at 10:50Z.
  - Why now: the daily task runs at 12:00Z, and `ScheduleAwardOperatorBlockPoints`
    moves a due run to 15 minutes after any taskworker start
    (controller/operator_block_points_controller.go:43, :61). So the 09:49Z
    roll started it at about 10:04Z.

#### 3.4 Vacuum

- **Workers and CPU:** 3–4 autovacuum workers at a time, 0.7–0.9 cores in the
  censuses; IO workers 0.6–1.0 and dm-crypt 0.7–0.8 follow their reads
  *(inferred)*.
- **transfer_contract runs back to back.** The previous vacuum ended at
  09:08:19Z, and the current one was in its index phase all through the window:
  1 of 16 indexes done at 10:10Z (3,656 s in), 4 of 16 at 10:44Z.
  - It walks 1,477 GB of indexes, down from 1,637 GB at CPUOPT5 thanks to O2's
    three partials, for 2.23B live rows.
  - The big ones are about 2–3× what a fresh build of today's rows would need
    *(inferred: 197 GB pkey, 317 GB create_time and 323 GB open_source_id
    against about 32–40 bytes per entry)*.
- **Autovacuum reads 71–89 MB/s** in vacuum context over 10:10–10:47Z, and
  396 MB/s over 10:47–11:05Z as it reached larger indexes (`pg_stat_io`;
  CPUOPT5's window: 544 MB/s, which had transfer_contract's partials in it).
- **The queue tables are vacuumed and analyzed about every 6 s:** 594 an hour
  each for transfer_debit_journal and pending_task. CPUOPT5 found every 20.9 s,
  with a pin; CPUOPT4 found every 6.0 s.
  - The rates follow the doubled writes, since both tables have
    threshold-only triggers.
  - pending_task is still a 28 GB heap for 112k live rows.
- **Zero HOT updates on three hot tables:** network_client (591M updates since
  the 10-23 reset), transfer_escrow (1.54B) and transfer_escrow_sweep (198M).
  transfer_contract is at 3%.
  - Every update writes every index: 162 GB, 952 GB and 249 GB of indexes
    respectively.
  - That is write amplification in the WAL, in vacuum, and in the buffer
    churn behind `heap_page_prune_opt`.
- **Catalog churn is not a cost now.** pg_class took 8 inserts in 2.5 minutes.
  The 4.2/s average since the reset came from two location tasks' temporary
  tables, about 2M creations each (`catalog-{1,2}.txt`, `texts-temp.tsv`).

#### 3.5 WAL and IO

- **WAL** (window 1→14): 43.9 MB/s, 153k records/s and 8.4k FPI/s; 54.0 MB/s
  and 11.5k FPI/s in 14→20. CPUOPT5's window had 58.6 MB/s and 15.5k FPI/s.
  It is lower or equal at twice the commits *(inferred: smaller partials and
  no vacuum of them in this window, lz4 already on)*.
  - The contract insert and close write 3.9–4.0 MB/s each.
  - Checkpoints every 5 minutes (7 in the window) write 57 MB/s.
  - The WAL summarizer reads 48 MB/s.
- **IO by backend type** (`pg_stat_io`):

  | Backend type | Read MB/s | Evictions/s |
  |---|---|---|
  | client backends | 132 | 16.5k |
  | parallel workers ("background worker", the award read) | 168 | — |
  | autovacuum | 89 | — |

  - Buffer allocations run 43k/s.
  - A compact transfer_contract pkey and create_time index would cut the
    insert path's random leaf reads *(inferred)*.

#### 3.6 Connections and the PgBouncer layer

**Server churn.**
- **Ages** (`backend-ages-1.txt`):
  - Of 666 loopback backends (PgBouncer's servers), 160 started in the last
    minute, 624 in the last 10 and all 666 within 60 minutes.
  - Of 114 direct backends, 9 started in the last minute.
  - pg_stat_database counts 2.8–4.0 new sessions a second, nearly all ended
    as "abandoned" (3.0–3.6/s).
- **SHOW SERVERS on four instances, 60 s apart** (`pgb-2.out`):
  - Pools stayed at 21–22 servers while 16 servers left and 18 arrived.
  - 15 of the 16 that left were younger than 600 s, so neither
    `server_idle_timeout` (600 s) nor `server_lifetime` (3,600 s) closed them.
- **The PostgreSQL log** shows "incomplete message from client" 1,766–1,881
  times in 10 minutes (about 3/s), against 4 "could not receive data" and 62
  user cancels (`pglog-{1,2}.out`).
- **On the client side,** connect creates 10.5 pool connections a second (2.1–2.4
  per g-block). Its destroys counted for idle or lifetime are about 0, so
  pgx closes them after errors (db.go:599, `pgConnectionErrorDiscards`).
  api creates 1.3/s, proxy 0.9/s.

The servers' replacement rate fits clients dropping connections while linked
to a server, mid-transaction or mid-exchange, which PgBouncer must then
discard *(inferred; reasons are not logged with `log_disconnections = 0`)*.

What it costs edge-2 *(inferred)*:
- a fork and a SCRAM login (4,096 iterations);
- cold relcache and catcache;
- the re-prepare of every statement the new server sees. Server parses are
  884/s, about 330 per new server; client parses are 773/s, about 70 per new
  client connection.
- five custom plans and one generic plan per statement before the plan cache
  settles.

That is about 0.2–0.5 s of backend CPU per replaced server, 0.7–1.5 cores in
all, plus 11 PgBouncer logins and nginx accepts a second. The planner symbols
in perf (§3.2) bound it from below; `track_planning` would measure it.

**Pool occupancy is idle time.**
- **Totals:**
  - PgBouncer: servers in a transaction 322 s/s, running a query 41.9 s/s.
    Transactions average 7.69 ms for 1.0 ms of query time.
  - PostgreSQL: 252–289 s/s idle in transaction against 15–17 s/s active.
- **Samples of idle-in-transaction backends by their last statement**
  (`iit-{1,2,3}.txt`, length-biased):
  - after `SET LOCAL lock_timeout`, that is after BEGIN and before the first
    statement: 34–46 backends, about 3.9 ms per transaction by Little's law;
  - after the close-report read, about 14–31 backends;
  - after the evidence insert, 11–19;
  - after the close-report upsert, 8–10;
  - after the ancestors walk, 12–15.
  - Transactions in the close and report path last 200–900 ms with 50–400 ms
    gaps.
- **This is what fills the 22-server pools during sweeps.** Execution averages
  about 1.3 servers per pool (41.9 of 704).

**Raising `default_pool_size` (owner's call; "not yet" on 11-14), modeled.**
- **Budget:** `32 × (22 + 2) + 190 + 40 = 998` of 1,021 non-superuser slots.
  Even 23 gives 1,030 > 1,021.
  - So any raise needs a larger `max_connections`, which means a PostgreSQL
    restart with the 10-13 lessons.
  - Or it needs a smaller direct budget: 190 now, with 114 + 24 sessions
    open.
- **No CPU saving.** It would hold more idle-in-transaction servers. At the
  peaks it adds concurrent backends and contention (`GetSnapshotData` 0.55% of
  samples at 850 backends).
- **The CPU-neutral levers** that shrink occupancy are #3 (about 40 servers
  on average) and shorter close and report transactions in connect (Part A).

**The direct path.**
- Over 30 h, with 21 or more pools at the cap (`initial-ping-by-pcap.txt`),
  a new connection's `initial_ping` averaged:
  - 16.1 ms on api beta (direct);
  - 26.8 ms on connect beta (single port);
  - 8.6–12.8 ms on api g1–g4 and 7.1–7.4 ms on connect g1–g4.
- beta is slower on either path. A new direct connection dials a PgBouncer
  instance (db_pgbouncer_path.go), not PostgreSQL, so it starts no backend.
  The 190 ms was a 3–5 minute peak on every block.
- No listen overflow in 3 days (`m-listen-3d.json`), though the instances
  still have backlog 128; nginx's 6432 has 4,096.

### 4. What CPUOPT5's items did

| # | Item | Status | Measured now | Confidence |
|---|---|---|---|---|
| 1 | End the SN25 shadow loop | Shipped. 3cddf5af7/b4374ac08 retired epoch 4's live shadow (owner, 11-13). 95306eb79 reads epochs 6–7 in hour-long snapshots, counts timed-out attempts, holds a failed chain 8 days and raises stale bounds (`RaiseRunOnceMaxTimeInTx`) | No 30-minute pin since 11-13 08:00Z. The application's longest transaction since: 915–1,400 s only in O2 rebuild windows (11-13 09:30, 18:35, 19:25; 11-14 04:05, 05:55Z), else ≤ 627 s, and ≤ 351 s from 11-14 14Z except one 627 s at 10:00Z today. At 10:55Z the horizon was 6–13 s old: xids at 9.2k/s, oldest application xmin 56k–121k (`horizon-1.txt`). The debit gauge still costs 3.99 ms and 1,988 hits per call (CPUOPT5 pin-free: 0.96 ms, 587). That is bloat, not a pin: transfer_debit_journal is a 7.2 GB heap for 40–57k live rows and reports 650–740k dead rows right after its vacuums *(inferred: vacuum cannot prune pages the constant shard scans pin, and cannot truncate)* | high |
| 2 | Remove the nginx stream hop | Built and staged. Direct path in 71d10dd43/438f2698d; stage 0 (`client_idle_timeout` 18000) applied 11-13 20:08Z; stage 1, api beta, since 11-14 ~08:00Z (aa7142599), 24 h watch passed 11-15 09:00Z | nginx 5.07 cores at 45 µs per query, against 2.20 at 42 µs. The saving doubled with the queries. Stages 2–4 remain | high |
| 3 | Reboot into `retbleed=stuff iommu=pt` | Not chosen (owner 11-13) | Booted 10-24, bare command line; `intel_idle_ibrs` 6.7% of samples; kernel time sys 12.8 + softirq 5.4 cores | high |
| 4 | Rebuild transfer_contract's bloated indexes (O2) | Approved; 3 of 5 partials rebuilt 11-13/14 | The unresolved source and destination pairs (3.0, 2.8 GB) and the payer partial (1.26 GB) are rebuilt. `transfer_contract_outcome_null` 18 GB and `…_open_partial_create_time` 9.9 GB still hold 396k entries; the expired-contract scan on the former reads 6.7k blocks from disk per call (1.45 s). Full indexes 1,477 GB in all | high |
| 5 | PgBouncer to PostgreSQL over a Unix socket | Not chosen (owner 11-13) | All 33 database lines still `host=127.0.0.1`; 685 loopback servers | high |
| 6 | Fewer round trips | Not done; db_owned_transaction.go:195 and :359 still send `SELECT pg_backend_pid()` alone. BEGIN with its lock bound was already one message (db.go:1073) | Transaction control 29.2% of statements (39k/s, CPUOPT5 21.7k/s) | high |
| 7 | An index for the SN25 amounts read | Not done | 0.22 calls/s at 1,406 ms (0.31 backends; CPUOPT5 0.19). st_payout_wallet_resolution grew 449k → 701k rows; 0.29 sequential scans/s, 199k tuples/s | high |
| 8 | A narrower descendant check | Not done | 358–371 calls/s at 1.37–1.84 ms, 636–771 hits (0.49–0.68 backends; CPUOPT5 187/s, 0.47) | high |
| 9 | JIT off | Not done globally; `SET LOCAL jit = off` already in the reaper (model/contract_horizon_reaper.go:1062), the due-contract reaper and the probe fleet read | 0.18–0.26 cores. The prober retention window is 0.13 of it (204–209 of 222–226 ms per call), the award read 0.037 while it runs | high |
| 10 | GetTransferStats and the health aggregate | Not done | Health aggregate 0.08/s at 2.68 s, 1.6M hits (0.21 backends); GetTransferStats did not rank in the window | medium |

Not Part D's items, but relevant here:
- **The PgBouncer instances' `listen_backlog`** is still 128. The one-at-a-time
  restart is planned before stage 2.
- **The stuck-provider signal is on for every api block** (stage 2, 11-14
  01:09Z by the stage log).

### 5. New costs

| Source | Revision, live since | Cost measured | Notes |
|---|---|---|---|
| Bot sweeps of hosted proxies (not a release) | from 11-13 evening; step at 11-14 13:50Z | +13 cores sustained (42.6 against 29.5), +3–9 cores at each sweep's peak, 38-minute cycles | proxy acquires ×7.3, connect ×1.9; contracts 2.1×. The owner declined UA blocks, ASN caps and new-account limits (11-14) |
| D49p zero-byte hosted devices close after 10 minutes | f43488da1, proxy 11-14 18:38Z | live devices 59k → 19k at the roll, back to 24–50k since 22:00Z; deactivations, with the sweeps, doubled: descendant checks 358–371/s (0.49–0.68 backends) | the cheaper fix for the check is #5 |
| Operator-block award re-reads the week (D47/D48 era statement) | award task; the startup schedule (controller/operator_block_points_controller.go:43, :61) | about 300 statements, 10:04–10:46Z: 0.81 backends + 4 parallel workers (0.4 cores), 168–210 MB/s of reads, 292 ms of JIT each; then the reliability phase, about 1 backend from 10:41Z | started by the 09:49Z taskworker roll; any restart while a block is due starts it again until it commits |
| Hourly operator-block traffic rollup (D47/D48) | 53b8acb7c, d58652613 | at most 16 sweep statements (8 hours) a run; 2 an hour once caught up (about 12 s/h) *(inferred from code)* | rides the leaderboard rebuild, which ran 7 times in 24 h (C §3.4) |
| GET /account/epochs bytes per epoch and the open block | d701eeabe, 28a242c70 | 318 calls in 27 minutes, 0.1 s in all | rollup and award rows by key |
| operator_block_points_expired | 870c9b7d1, b6fa45f87 | not measurable | daily |
| Points-leaderboard rebuild (7 runs in 24 h, not hourly; C §3.4) and D50t streak skip | 9ef3f5866, 6c0a7ffb6 | did not run in the window; leaderboard inserts 4,077 s over 39,861 calls since 10-23 | reschedules after 1 minute while its backfill advances (points_leaderboard_controller.go:626) |
| 8327 `trim-proxies --include_pro` | c0d82611d | one run at 20:23Z on 11-14, outside the window | 680 proxies removed |
| Gift-gamma | 11d5a8e78, migration 977 at 09:41:34Z | gamma statements under 0.001 backends | — |
| PgBouncer stage 1 (direct path on api beta) | aa7142599, 11-14 ~08:00Z | 24 h watch passed; new-connection ping in line with connect beta on the single port | §3.6 |
| Devices series, hourly | e1956f130 (10-13) | about 1,000 statements and 713 s an hour (0.2 cores averaged), from parallel chains | it was there at CPUOPT5, below its ranking |

### 6. Ranked opportunities

#### #1 Finish the direct path: stages 2–4, which remove the nginx stream hop (CPUOPT5 #2)

- **Change:** the approved plan's next stages, each a merged change to
  `pgBouncerDirectBlocks` (server/db_pgbouncer_path_rollout.go) in its owner
  window:
  - stage 2: every api block;
  - stage 3: connect;
  - stage 4: taskworker, proxy, alt, gossip and mcp.
  - The one-at-a-time `run-dbs.sh --pgbouncer-restart` for `listen_backlog`
    goes first, as planned on 11-13.
  - The single port stays for tools and tunnels (owner, 11-13: keep both
    options).
- **Evidence:**
  - nginx 5.07 cores (79% kernel), 45 µs per query, and part of 5.39 cores of
    softirq (§3.2).
  - Stage 1 held its 24 hours.
  - Under sweeps, a new direct connection costs what the single port costs on
    the same hosts (§3.6).
- **Saving:** 4.5–5.5 cores at today's load. connect alone carries 43% of the
  acquires, api 29%.
- **Risk:** medium, on the database entry path.
  - The client-side balancer reproduces nginx's `max_fails=3 fail_timeout=30s`.
  - Each stage reverts by a merge before the next nightly.
- **Proving test:**
  - `TestPgBouncerDirectRolloutStage` for each stage, plus the dialer's
    deterministic tests in db_pgbouncer_path_test.go;
  - live: nginx CPU per query falls in proportion to the moved acquires, and
    PgBouncer CPU per query is unchanged;
  - the block's `initial_ping` and request latency against its own baseline.
- **Owner rules:** no timeout moves (`client_idle_timeout = 18000` already
  equals `proxy_timeout 300m`); no new setting (the table is code); the
  windows are the owner's.

#### #2 The provide-key change count from a pessimistic Redis marker

- **Change.** Every connected client's announce loop calls
  `GetProvideKeyChanges` each sync (connect/transport_announce.go:755). It
  sends `SELECT COUNT(*) FROM provide_key_change WHERE client_id = $1 AND $2 <=
  change_time` (model/network_client_model.go:3846) as its own round trip,
  then reads the modes from the Redis provide mirror (`GetProvideModes`,
  :3362).
  - Have the writer of `provide_key_change` set a per-client "last change at"
    marker in the same hash slot as the mirror, *before* its transaction
    commits, so a failed commit leaves a marker that only sends the reader to
    PostgreSQL.
  - The reader MGETs the marker with the modes key it already reads. When the
    marker is present and older than `$2`, the count is 0. When it is absent
    (expired, evicted, Redis error) or newer, the reader runs today's
    statement.
- **Evidence:**
  - 9.3k calls/s, 6.9% of statements, at 0.018 ms of execution (§3.3). Each
    pays a pool acquire, about 64 µs of PgBouncer, 45 µs of nginx, and the
    backend's per-statement protocol and executor work outside
    `total_exec_time` (§3.2).
  - The table holds 1.45M rows and takes about 405 inserts a second, many of
    them from newly minted clients. Spread over about 107k connected
    providers, that is a change every few minutes per client on average, so
    most counts are 0 *(inferred)*.
- **Saving:** 1.2–1.6 cores *(inferred: 130–180 µs per call across backend,
  PgBouncer and nginx)*, 8% of PgBouncer's queries, and connect-side acquires.
  About 0.9–1.2 after #1.
- **Risk:** medium, on correctness of the reliability input
  `provide_changed_count`.
  - The marker must never under-report. Writing it before commit and treating
    absence as unknown keeps it one-sided.
  - The mirror's existing confirm-and-unfill discipline is the model
    (model/network_client_model.go:3342).
- **Proving test (deterministic):** a client whose key changed after `$2` with
  the marker write failing still gets its exact count from PostgreSQL. A
  committed change is never reported as 0. A rolled-back change costs one
  PostgreSQL read and reports the true count. An expired marker falls back.
  The test fails on today's code only for "PostgreSQL not read when the marker
  is older", so add a counter assertion.
- **Owner rules:** the sync period, the stats and every detection bound are
  unchanged; the marker's TTL is a setting field with one default (CODESTYLE),
  and absence is always safe.

#### #3 Lazy BEGIN: BEGIN and its lock bound travel with the transaction's first statement (extends CPUOPT5 #6)

- **Change.**
  - Today `Tx` sends `begin …; set local lock_timeout = N` as one
    simple-protocol round trip (db.go:1058–1074, `beginSqlWithLockTimeout`),
    then runs the callback, which sends its statements.
  - Instead, hold the BEGIN until the callback's first statement, and send both
    in one network write: an extended-protocol pipeline of BEGIN, `set_config`
    and the statement, ending in one Sync. pgx's pipeline does this.
  - A callback that issues nothing then costs nothing.
  - Keep `PgBeginTimeout` on the combined exchange, and the same `lock_timeout`
    value and scope.
- **Evidence:**
  - 10.3k transactions/s each pay one extra round trip through both hops
    (§3.3).
  - The largest idle-in-transaction bucket is between BEGIN and the first
    statement: 34–46 servers at a time, about 3.9 ms per transaction (§3.6).
    Under transaction pooling that bucket would hold no server.
- **Saving:** 1.0–1.4 cores *(inferred: about 110 µs of hops plus 20–40 µs of
  backend protocol per BEGIN)*, about 0.7–1.0 after #1. Also about 40 fewer
  servers in use on average, 6% of the cap, plus one round trip off every
  transaction's lock hold.
- **Risk:** medium, in transaction setup.
  - `now()` becomes the first statement's arrival, closer to the work.
  - REPEATABLE READ snapshots already start at the first statement.
  - Error mapping of a failed BEGIN inside a pipeline needs care, including
    the 08P01 refusal that PgBouncer's `query_wait_timeout` produces.
- **Proving test (deterministic):** with a recorded pgconn transport, a `Tx`
  sends BEGIN, the bound and its first statement in one write, and the lock
  bound is in force for that statement (a 55P03 within the bound on a held
  lock). A callback that sleeps before its first statement holds no PgBouncer
  server: a server-count assertion against a test PgBouncer, or a fake that
  counts assignments. A BEGIN refused by the pooler surfaces as today's error.
- **Owner rules:** no bound moves; the same `PgLockTimeout` and caller
  options.

#### #4 End connect's connection churn

- **Evidence (§3.6):**
  - connect creates 10.5 pool connections a second while its idle and
    lifetime destroys are about 0;
  - PgBouncer replaces 2.1–2.7 servers a second at constant pool size;
  - PostgreSQL logs "incomplete message from client" about 3 times a second;
  - server parses are 884/s, 0.9% of binds (CPUOPT5: 0.1%);
  - client logins are 11/s (CPUOPT5: 3.6).
- **Change:**
  - Find which connect paths stop a context while a pgx exchange or
    transaction is open. The likely ones are the per-client goroutines whose
    clients leave during sweeps *(inferred)*.
  - Where the stop is the caller's own and the database work is safe to
    finish, let it finish on a detached context with the existing bounds
    (`PgStatementStopTimeout`, `PgRollbackTimeout`), so pgx returns the
    connection idle instead of closing it.
  - db.go:590–608 already keeps a connection that pgx reports idle and in
    sync after another component's deadline. This extends that to connect's
    stops.
- **Saving:** 0.7–1.5 cores on edge-2 *(inferred: 0.2–0.5 s of backend CPU
  per replaced server for fork, SCRAM login, cold caches, about 330
  re-prepares and their custom plans)*, plus PgBouncer logins, nginx accepts
  and connect's own reconnects (Part A). It also lowers the server-side
  "abandoned" sessions and the planner share in perf.
- **Risk:** medium. Owner rule: no stop bound may lengthen, and a caller's
  stop must still return promptly. Only the cleanup of an already-sent
  exchange moves to a detached context bounded as today.
- **Proving test:** a deterministic test that stops a connect caller during
  an open transaction and asserts the pool connection is reused (not
  destroyed) and the transaction ended. It must fail on the current code.
  Live: connect's `connections_created` rate, PgBouncer server ages (median
  over 600 s), and server parses against binds.
- **Owner:** whether connect's pool may hold connections through stops; Part
  A should confirm the source first.
- **Cross-part:** connect's creation rate rose only in the windows where both
  the larger sweeps and the key-event storm ran (`windows-evening.txt`; A §5.1).
  The storm's resync waves are cancelled by the next wave every 7–9 s; whether
  that interrupts database work is not measured. Read connect's creation rate
  again after A #1 lands, before building this.

#### #5 Descendant check from a partial index of active clients (CPUOPT5 #8)

- **Change:**
  - An additive migration: `CREATE INDEX CONCURRENTLY network_client_active_device_id
    ON network_client (device_id, client_id) WHERE active`, applied before the
    code.
  - Then `readActiveChildClientIdsInTx` adds `AND network_client.active`
    inside its LATERAL (model/network_client_model.go:1734), keeping `OFFSET
    0`, so the device read can only be the device's active range.
- **Evidence:**
  - 358–371 calls/s at 1.37–1.84 ms and 636–771 hits.
  - A hosted device holds every window it minted. D49p's closes doubled the
    calls.
- **Saving:** 0.4–0.6 cores *(inferred: an active-only range of a few
  entries)*.
- **Write cost:** network_client's updates are already 0% HOT, so each update
  of an active row adds one index insert (about 300 updates/s); the index
  covers active rows only *(inferred: small)*. Measure on the restored copy on
  edge-6 first, as CPUOPT5 asked.
- **Risk:** medium: the table whose unbounded walk browned out Main on 11-06.
- **Proving test:** the same children for a device with many retired windows
  and a few active ones, against the current query (a deterministic model
  test). Hits per call in pgss under 50.
- **Owner rules:** the migration is additive and runs before the code; no
  bound moves.

#### #6 JIT off where it compiles every call (CPUOPT5 #9)

- **Change:** `SET LOCAL jit = off` in the transactions of:
  - the prober retention contract window (model/prober_shard_retention.go:1066);
  - the award's sweep read (model/operator_block_points.go:422, which reads
    outside a transaction, so a statement-level `SET LOCAL` in a short
    transaction, or the pool's own option);
  - the health aggregate (model/provider_egress_health_model.go:806).
  - This is the existing pattern (model/contract_horizon_reaper.go:1062). Or
    raise `jit_above_cost` in postgresql.conf, a reload in an owner window.
- **Evidence:** JIT 0.18–0.26 cores across the windows; the retention window
  spends 204–209 of 222–226 ms compiling (0.13 cores by itself).
- **Saving:** 0.2–0.25 cores while the award runs, 0.15–0.18 otherwise.
- **Risk:** low.
- **Proving test:** pgss `jit_*` deltas near 0 for those queryids; for the
  code form, a model test that the transaction runs with `jit` off.
- **Owner rules:** no new setting.

#### #7 An index for the SN25 amounts read (CPUOPT5 #7)

- **As CPUOPT5 #7:** an additive index on st_payout_wallet_resolution for the
  `DISTINCT ON (epoch, coldkey)` read (model/st_ledger_v2_reader_model.go:28,
  :86).
- **Now:** 1,406 ms per call and 0.31 backends. The table grew 56% in two days
  (701k rows), so the cost grows every epoch.
- **Saving:** about 0.3 cores, rising.
- **Proving test:** the reader's results unchanged (deterministic test) and the
  queryid under 10 ms.

#### #8 Finish O2's last two partials, then the full indexes (CPUOPT5 #4)

- **Change:**
  - `REINDEX CONCURRENTLY` of `transfer_contract_outcome_null` (18 GB) and
    `transfer_contract_open_partial_create_time` (9.9 GB), for 396k entries.
  - Then the reaper-thinned full indexes, one at a time, under O2's existing
    gates.
- **Evidence:**
  - The expired-contract scan reads 6.7k blocks from disk per call.
  - transfer_contract's vacuum index phase runs for hours over 1,477 GB.
- **Saving:** 0.1–0.2 cores for the partials, 0.5–1 for the full rebuilds
  *(inferred)*, plus disk and buffer-cache headroom.
- **Risk:** medium. Each build holds a snapshot: the O2 windows on 11-13/14
  raised the longest transaction to 915–1,400 s.
- **Owner:** the window (O2 approved).

**And the queue tables:** `pg_repack` (1.5.3 is installed) of
transfer_debit_journal and pending_task, one at a time.
- **Evidence:**
  - transfer_debit_journal is a 7.2 GB heap for 40–57k live rows and reports
    650–740k dead rows right after its vacuums, with a horizon only 6–13 s old
    (`horizon-1.txt`). Its oldest-age gauge costs 3.99 ms and 1,988 hits per
    call, against 0.96 ms and 587 pin-free at CPUOPT5. The shard walk and the
    `FOR UPDATE SKIP LOCKED` page read cross the same empty pages.
  - pending_task is 28 GB for 112k live rows.
- **Saving:** 0.15–0.3 cores *(inferred: the debit family is 0.29 backends and
  the tasks family 0.14)*, for as long as the tables stay compact.
- **Proving step:** relation size against live rows, and the gauge's hits per
  call before and after.
- **Risk:** medium. pg_repack takes brief exclusive locks at its start and
  swap, on tables written about 1.4k times a second. Run it outside the
  settlement and validator windows, with the debit stop of CPUOPT4 #3.
- **Owner:** the window. This is maintenance, not a setting, so no timeout
  or bound moves.

#### #9 The health-window aggregate and GetTransferStats (CPUOPT5 #10)

- **Unchanged recommendation.** The aggregate is 0.21 backends (2.68 s,
  1.6M hits, 48 ms JIT per call). Compute it once per export and reuse it.
- **Saving:** 0.2–0.3.

#### #10 Fewer claim probes per task, and one statement for owned-session setup (with Part C)

- **The probes.** The claim loop probes candidates one round trip at a time
  (task/task.go:2230): 1.44k probes/s for 30.8 executions/s, with 584/s
  `pg_advisory_unlock_all()` and 527/s bare `pg_backend_pid()` (§3.3).
  - Fewer candidates per pass, or a per-process backoff when a pass is
    refused, cuts edge-2's share.
  - Also fold `SELECT pg_backend_pid()` into the admission statement that
    already returns it (db_owned_transaction.go:195, :359, :479), as CPUOPT5
    #6 proposed.
- **Saving:** 0.2–0.4 edge-2 cores *(inferred)*, more on the taskworker.
- **Risk:** low–medium (claim fairness, ownership fencing).
- **Proving test:** claim throughput and fairness tests unchanged, and the
  probes per executed task below a fixed bound.

#### #11 The award reads the rollup it keeps

- **Change:** `ComputeOperatorBlockPoints` takes each network's bytes from
  `operator_block_network_traffic` (model/operator_block_traffic.go), which
  the hourly rollup fills with the award's own statement. It then reads only
  what the rollup has not, plus a reconciliation of the last settle stretch.
- **Evidence:** about 300 statements of 4.6–13 s (§3.3), started by any
  taskworker roll while a block is due.
- **Saving:** small averaged (about 0.01 cores). About 1–2 cores, 4 parallel
  workers and 170–210 MB/s for about an hour per block, and the risk that a
  release roll lands it in the evening.
- **Risk:** medium. The award's figures may differ from the rollup's by late
  settlements, so this is an award-semantics decision for the owner and the
  author.

### 7. Considered and rejected

- **Raising `default_pool_size` for CPU** (modeled in §3.6). Occupancy is
  idle-in-transaction time. A raise needs a restart or a smaller direct budget
  and saves no CPU. It is the owner's call for latency.
- **Raising `min_pool_size`** to stop the churn. The pools already sit at
  21–22, and the servers are replaced, not reaped.
- **Lengthening `server_lifetime` or `server_idle_timeout`.** These are
  timeouts, and they are not what closes the servers.
- **Shortening `idle_in_transaction_session_timeout`, `query_wait_timeout` or
  any PgBouncer timeout**, or a statement timeout on the maintenance pool, to
  cut held servers. These are timeouts, and they would fail legitimate work.
  #3 and Part A remove the idle time instead.
- **Raising `checkpoint_timeout`** to cut FPIs (8.4k/s), or
  `autovacuum_freeze_max_age`. A timeout, and lost wraparound margin.
- **Sampling the debit gauge less often.** It feeds the lag alert.
- **Turning off WAL summaries.** They serve the incremental base backups
  (0.12 cores).
- **Turning on `track_planning` for the analysis.** It is a setting change
  with its own contention. It would measure #4's planning share, and is left
  to the owner if wanted.
- **Blocking or limiting the bot sweeps** (UA block, ASN cap, new-account
  proxy limit). The owner declined them on 11-14; the levers above make each
  sweep cheaper instead.
- **PgBouncer over a Unix socket, and the edge-2 reboot** (CPUOPT5 #5, #3).
  The owner did not choose them on 11-13.
  - Their value grew with the load: 685 loopback servers, and kernel time is
    18.2 cores.
  - Not re-proposed. Noted for when the owner revisits them.
- **Temporary-table catalog churn.** It measured negligible now (§3.4).
- **Pausing the reaper, retention or the hourly bursts for CPU.** They are
  small averaged, and retention and disk are owner decisions.

### 8. Noticed, not CPU

- **The weekly operator-block award re-reads the week whenever the taskworker
  restarts while a block is due** (§3.3): the 09:49Z roll started it at about
  10:04Z, and a release roll can put it in the evening (#11).
- **Connections die young** (§3.6): PostgreSQL logs "incomplete message from
  client" about 3 times a second, and 3.0–3.6 sessions a second end as
  "abandoned".
- **Zero HOT updates** on network_client, transfer_escrow and
  transfer_escrow_sweep (§3.4): every update writes every index.
- **Bloated queue tables:** transfer_debit_journal is a 7.2 GB heap for 40–57k
  live rows, and pending_task 28 GB for 112k (§3.4, §4).
- **The PgBouncer instances still listen with backlog 128** (65535 is
  configured); no listen overflow in 3 days (§3.6, §4).
- **The "190 ms" direct-connection cost** in the brief was a 3–5 minute peak on
  every block, not the direct path (§3.6).

## Part E — The Redis cluster, and edge-5's co-tenants

This part is read-only research on Main, 2026-11-15 10:08–11:06Z, with Mimir history from 11-12 to now. Sources:
- Mimir: the redis exporter (48 nodes; it scrapes every 99 s on edge-5 and 67 s on edge-6, so every rate here uses windows of 5 minutes or more), node exporter for edge-5 and edge-6, MinIO's own metrics, and the client-side metrics (`urnetwork_redis_pool_*`, `urnetwork_key_event_*`, `urnetwork_session_*`, contract-hole and control-limit counters).
- redis-cli on edge-5 and edge-6 over ssh as the unprivileged `by` user: `INFO everything` and `LATENCY LATEST` for 10 rounds 5 minutes apart (10:22–11:05Z); `CLUSTER SLOT-STATS` for the same rounds; `CONFIG GET`, `CLUSTER NODES` reduced to slot ranges, `SLOWLOG GET 1024` and `CLIENT LIST`, summarized on the host; `SCARD`/`HLEN`/`TTL` of five fixed code-named keys; one 10-second pattern subscription on one node, counting keyspace events by family.
- Root, through `xops/bin/by-pass` piped into `sudo -k -S -p ""` over ssh (the owner's 11-15 allowance; the password was never printed or stored): two 30 s `perf stat` counts, one 15 s perf sample of MinIO's cgroup, host posture (kernel journal, iptables), and `stat`/`xfs_io -r stat` of MinIO's directories.
- `/proc` and `/sys` on both hosts.
- Code read at the deployed revisions.

Nothing in production was changed. No key name, member, argument, value, client address or client name was printed or kept; client sources are salted pseudonyms, mapped to host names locally. `perf` ran only while the SN25 validator read finalized with ≥15 minutes to its boundary; no perf process or data file was left on either host (checked after each run and at the end), and no scratch file was left in either host's `/tmp`. *(inferred)* marks a claim reasoned from code, timing or command mix rather than measured directly. Raw outputs are under `<E>` = `<scratch>/E/`.

### 1. Summary

- **Redis CPU doubled since CPUOPT5, and a resubscribe storm in connect's key-event subscriber causes nearly all of it.**
  - **Totals:** 36.0–38.1 Redis cores at 311–343k requests/s since 11-14 16Z. CPUOPT5's 3-day mean was 20.4 cores at 115k/s. The snapshots (10:25–11:00Z) give 39.1 cores at 353k/s.
  - **The control window:** on 11-14 00–05Z the storm was off. With 172k connected clients, Redis ran 19.5 cores at 136k requests/s, 0.79 requests per client per second (CPUOPT5: 0.77–0.81). Now it runs 1.20 per client.
  - **The storm's share:** a fit over 106 half-hour windows (11-13 06Z – 11-15 10:30Z, R² 0.96) puts Redis cores at 22.1 + 0.35 per 1k/s of excess SPOP, with no detectable term for connected clients. That attributes **17.4 of today's 38.0 cores and 194k of 343k requests/s to the storm**. Host softirq adds 23–32 µs per request on top, so about 4.5–6 more host cores (§3.1).
- **How the storm works** (§5.1; it starts in connect and lands on Redis; Part A §5.1 has the connect side):
  1. Every connect process pattern-subscribes six keyspace patterns on all 48 masters, so it receives the whole cluster's event stream: about 27k events/s per process. 87% of those are `expire` events on `{ns_<network>}eid`.
  2. The session hint read generates those events itself. `NetworkSessionRevision` runs the full inventory script, which re-extends the TTL of the network's four session keys on every call.
  3. The 16 g-block connect processes hold 11–18k clients each and run at about 7.5 cores. They can't drain their 1,024-message merge. One node's drop ends the whole 48-node subscription; after a 5 s backoff it resubscribes and starts two resync waves: a kick of every close listener (preflight PING + SPOP each) and a hint read of every network group, which makes more `expire` events.
  4. Merge drops run at 3.2/s fleet-wide (0.19/s on 11-12), and each g-block process resubscribes about every 8.3 s.
- **Signatures:**
  - SPOP 3.2k → 53k/s and PING 14k → 78k/s. The close-listener kicks plus 25k/s of contract-hole calls account for every PING.
  - Session inventory reads 1.8–2.6k → 18–27k/s, carrying about 100k PEXPIREAT/s and 143k PTTL/s inside the script.
  - PSUBSCRIBE 9 → 145/s; HELLO 78 → 929/s; CLIENT SETINFO about 1.7k/s.
  - connect pool misses (new dials) 33 → 1,316/s. Pool timeouts are back at 1.2/s (CPUOPT5 measured 0).
  - Session checks answering "unavailable": 8–20 → 700–1,430/s.
- **The sweeps themselves cost Redis almost nothing.** Over 155 five-minute windows from 11-14 22Z to now, sweep windows (live hosted devices +2.6k in 5 min) against quiet ones: requests 0.999×, cores 1.003×. The 10:40–10:50Z sweep in the snapshots was flat too (353.5k/s against 349.8–355.5k). The storm runs between sweeps; the bot wave started it on 11-13 ~20Z, and it has run continuously since 11-14 15Z (§3.4, §5.1).
- **Per request, Redis is 35–38% cheaper:** 111–117 µs all-in now against 178. The performance governor (CPUOPT5 #2, live 11-13 ~16:56Z) cut marginal cost per request by 22–30%. Clocks under the Redis threads are now 2.30 GHz on edge-5 (1.62 before, 78% of samples then below 1.6 GHz) and 2.6 GHz on edge-6. Three times the load per node does the rest. A median edge-5 node now spends 237k cycles per request at 2.12 GHz, against 249k at 1.40 GHz in CPUOPT5 (§3.2).
- **CPUOPT5's other items:**
  - **Host items, all live:** the governor, `numa_balancing=0` on edge-6 (page migrations 18k/s → 0) and per-slot statistics.
  - **Staged for a reboot or the next run:** the ZFS taskq parameters, the 8K clone and `iommu=pt` on edge-5.
  - **The restore stalls are gone because the restore finished.** The WAL replay ended about 11-13 21Z, and edge-6's slow entries fell from 16–37k/h to 100–330/h. The backup run that began 11-10 ended 11-15 05:45Z, and no next run is scheduled.
  - **None of the code items shipped:** preflight PING, EVALSHA, GETEX and in-MULTI prune, exclusions published on change.
- **edge-6 has a softirq hot spot** (§3.5; new).
  - Its live NIC is an i40e on NUMA node 2. Its interrupts land on 16 CPUs (80–87, 21–23), which run 96–97% busy with 38–48% softirq while the host is 21% busy.
  - 30 of the 32 Redis I/O threads were last running on those CPUs.
  - CPU pressure "some" is 34.6% on edge-6, against 3.6% on edge-5. The I/O threads take 7.7k involuntary switches/s against edge-5's 0.4k, and cost 72 µs per request against edge-5's 55.
  - irqbalance is inactive and RFS is off.
- **edge-5's co-tenant, MinIO, burns most of its CPU on one directory** (§3.6).
  - MinIO runs 5–7 cores. In a 15 s cgroup sample, 88% of its time was kernel:
    - about 39% walking a directory's extent list on every directory grow (`xfs_bmap_first_unused` under `xfs_dir2_node_addname`);
    - about 34% spinning on that directory's lock in `mkdir` (`osq_lock`, `rwsem_spin_on_owner` under `filename_create`).
  - The directory is the flat `st/v1/evidence/content/sha256/` (`startifact/evidence.go:147`): 18.54 GB with 2.6–2.85M extents on each of 8 drives, growing 26 MB/h.
  - MinIO's `RenameData` drive calls average 37 ms.
  - At this hour's rate the directory reaches XFS's 32 GiB data limit around 12-10; at 60–75% of that rate, 12-18 to 12-27.
  - XFS buffer growth (7.2 GB/h since the 00:45Z drop_caches) had no measurable effect on Redis: 102–104 µs per request on edge-5 before and after.
  - Redis threads on edge-5 are rarely preempted (223 involuntary switches/s over 16 main threads).
- **The hardware risk is not CPU, but it is live:**
  - EDAC mc1 corrected errors: 196 at 00:45Z, 204 at 10:25Z, 206 at 11:00Z.
  - 790 `[Hardware Error]` kernel lines since 00:00Z, none uncorrected.

**Ranked opportunities.** Cores are at today's load (Redis about 38–39 cores); they overlap, so do not add them all.

| # | Change | Saving | Risk |
|---|---|---|---|
| 1 | Stop the key-event storm: a revision-only session hint read, TTL extension with headroom, `expire` events dropped before the bounded merge, then a cheaper close-listener resync (§5.1, #1; with A #1) | 15–17 Redis cores, plus 4.5–6 softirq cores on edge-5 and edge-6; connect CPU and pool churn too | low (a, b); medium (c) |
| 2 | No preflight PING by default (CPUOPT5 E §5.1, unshipped) | 5.1–6.1 Redis + 1.8–2.5 softirq cores now; 1.8–2.2 + 0.6–0.9 after #1 | low |
| 3 | Spread edge-6's NIC interrupts, or turn on RFS, so softirq and the Redis I/O threads stop piling onto NUMA node 2 (new; host playbook) | 1.7–4.1 edge-6 cores *(inferred)*; ends 34% CPU pressure and the I/O threads' preemption | low–medium; owner |
| 4 | Shard MinIO's flat evidence directory on new writes, with a dual read (edge-5 co-tenant) | 3–4.5 MinIO cores on edge-5 *(inferred)*; PUT latency; avoids the 32 GiB directory wall in December | medium; owner/SN25 |
| 5 | EVAL → EVALSHA, now led by the session inventory and authority-publish scripts (CPUOPT5 E §5.7, unshipped) | 0.6–1.0 cores now; 0.3–0.6 after #1 *(inferred)* | low |
| 6 | Publish the five full-rewrite sets only on change: `{provider_hard_exclusions}` and D42's capability sets (CPUOPT5 E §5.8, widened) | ≈0.02 cores; removes 115–210 ms whole-node blocks on 6415, 6386 and 6399 | low |
| 7 | GETEX in the resident poll, prune inside the MULTI (CPUOPT5 E §5.6, unshipped) | 0.3–0.6 cores *(inferred)* | low |
| — | Stuck-provider recycle stage (owner's staged plan); a local exclusions copy; `iommu=pt` at a planned reboot (§6, owner items) | see §6, owner items | owner |

**Cross-part notes:**
- **Part A, connect:** the storm's connect-side cost. That's 16 g-block processes resubscribing every ~8.3 s, their resync goroutines, about 190k extra Redis round trips/s and 1.3k dials/s. connect CPU is 7.25–7.62 cores per g-block process, against 1.57 on beta, which holds 750 clients per process.
- **Part D, PostgreSQL:** lease rechecks run `ValidateByJwtState`. If its remembered-state path queries PostgreSQL, the storm reaches edge-2 too; look for a step at 11-14 15Z. Part D found edge-2's step at 11-14 13:50Z, with the sweeps, before the storm ran continuously (D §3.1); the rechecks' PostgreSQL share is not measured.
- **Part B, proxy:** the proxy's Redis round trips went from 1.1k to 7.5–8.1k/s with the hosted devices. Its single-pattern client-removed subscriber is stable, at 2.8 KB/s per node.

### 2. Method

- **Windows and load:**
  - **Live captures:** 2026-11-15 10:08–11:06Z, a Sunday mid-morning, with 277–291k connected clients, 40.2–45.7k live hosted devices and one bot sweep (10:40–10:50Z).
  - **Like-with-like comparisons, from Mimir:**
    - CPUOPT5's windows: the 3 days 11-10 04Z – 11-13 04Z, the 11-12 18–22Z evening, and 11-13 00–04Z.
    - "governor, before the wave": 11-13 17:00–19:30Z.
    - "storm off": 11-14 00–05Z.
    - Storm on: the 11-14 18–22Z evening, 11-15 00–04Z, 11-15 06–10Z, and the whole wave from 11-14 16Z to 11-15 10Z.
  - **Normalization:** per request (`total_reads_processed`) and per connected client, as in CPUOPT5. The storm is measured by SPOP above its storm-off level (3.6k/s).
- **Builds** (`urnetwork_build_info`):

  | Job | During the captures | Server revision | Since |
  |---|---|---|---|
  | connect, mcp (D47) | `2026.11.14-outerwerld+1098366310`, mcp `+1098368160` | aa7142599 | 11-14 07:00–07:48Z |
  | gossip (D47) | `2026.11.13-outerwerld+1098358910` | aa7142599 | 11-14 06:32Z |
  | proxy (D49p) | `2026.11.14-outerwerld+1098770070` | f43488da1 | 11-14 17:30–18:38Z |
  | api, alt, taskworker | D50 (`+1098824240`, `+1098822500`, `+1098815950`) until 09:50–10:05Z, then 2026.11.15 builds (taskworker `+1099357100` from 09:50Z, api `+1099362010` and alt `+1099363220` from 10:00Z) | D50: 6c0a7ffb6; the 11-15 builds match main's 11d5a8e78 (gift-gamma, merged 09:40Z) by timing | — |

  - connect's revision comes from the D47 batch: its build number sits between the taskworker's and alt's, both audited as aa7142599.
  - **No hot Redis file differs** across aa7142599, 6c0a7ffb6 and f43488da1: redis.go, connect/key_event_subscriber.go, connect/session_notifications.go, connect/client_close.go, session/, model/client_close_model.go, model/subscription_contract_hole.go, model/network_client_score_capability.go, model/egress_index.go and startifact/evidence.go.
  - 11d5a8e78 adds no Redis call on a hot path, and the snapshot rounds across 10:00Z show no step.
- **Host captures** (scripts in `<E>/scripts/`; `byrun.sh` runs a script as `by`, `rootrun.sh` as root, both copying it to `/tmp` and removing it):
  - **`snap6.sh`, every 5 minutes for 10 rounds (`snaploop.sh`):** CPUOPT5's snap.sh with ports taken from process titles (as `by`, /etc/redis is unreadable), plus reclaim counters, EDAC and MinIO's thread count. With it, **`slotstats.sh`**: `CLUSTER SLOT-STATS SLOTSRANGE 0 16383` per node.
  - **`summ6.py`:** CPUOPT5's summ.py with ports from process titles: config, slot map, slowlog names and fingerprints, latency, and the CLIENT LIST summary with salted source pseudonyms. **`map-sources.sh`** maps the pseudonyms to inventory hosts locally.
  - **`pubsub6.py`, `pubsub-rates.py`:** subscriber connections' ages and output rates.
  - **`psample.sh`:** a 10 s `PSUBSCRIBE` of connect's six patterns on node 6418, counting events by family and event type only.
  - **`setsizes.sh`:** SCARD, HLEN and TTL of five fixed code-named keys, and the key families of five slots.
  - **`host-posture6.sh` (root):** CPUOPT5's posture script plus MCE/EDAC counts, NET_RX spread and NIC IRQ affinity. **`softirq-cpus.sh`:** per-CPU busy and softirq over 10 s, NIC queues, RPS. **`e6-timers.sh`:** edge-6's backup units. **`freq-sample.sh`:** CPUOPT5's clock sampler.
  - **`perfstat.sh` (root):** `perf stat -e cycles,instructions,task-clock`, 30 s, counting only, on the median nodes 6418 (edge-5) and 6386 (edge-6), at 10:14:49–10:15:20Z.
  - **`minio-perf.sh` (root):** `perf record -N -F 99 -a -g -e cpu-clock -G system.slice/minio.service`, 15 s at about 10:17:20Z, MinIO's cgroup only, no build-id cache; data file deleted.
  - **`minio-dirs.sh` (root):** bucket names, then size and data-fork extent count of fixed `st/` code paths, on all 8 drives. Two runs, 10:20:09Z and 11:01:01Z.
  - **The validator gate (`vgate.sh`):** progress.json on snow, the sn-validator journal's intent lines, and the chain head. At 10:10:24Z it read intent 26354 finalized with 1,716 s to the boundary (10:39Z). Both perf runs finished by 10:18Z.
- **Mimir scripts** (`<E>/scripts/`):
  - CPUOPT5's `winstats.py`, `cmdcompare.py`, `pernode.py`, `regress.py`, `hourly.py`, `slowlog-analyze.py`, `tag-slots.py`, `lua-fingerprints.py` and `preflight-callers.py`.
  - New: `series.py`, `stormfit.py` (OLS of each command's rate on connected clients and excess SPOP), `softirq-fit.py`, `sweepclass.py` (5-minute windows classed by the rise in live hosted devices), `loopdelta.py`, `snapdelta6.py` and `slotdelta.py`.
- **Code:** read with `git show` at aa7142599, 6c0a7ffb6 and f43488da1. Lua literals were fingerprinted at all three (127 scripts each), and fixed hash tags mapped to ports from `CLUSTER NODES` (215 tag/slot lines).
- **Not captured:**
  - Redis sampling profiles (counting only, by the rules).
  - Per-pool client metrics: `urnetwork_redis_pool_*` sums all five pools, so the auth pool's misses aren't separable.
  - Which key makes the four high-output slots (§3.3), since key names weren't read.
  - Node exporter has no `pgscan`/`pgsteal`, so reclaim history before today is missing.
  - PostgreSQL effects (Part D).
- **Refused or skipped:** nothing was refused by the permission system.
  - **The MinIO sample inflated MinIO itself.** MinIO's own 15 s CPU counter read 11.0 cores in the sample's interval, against 7.0–7.3 around it: perf's cgroup event switching on a 58k-thread process. The profile's proportions are of sampled time and are treated as approximate. No second sample was taken.
  - **Slot statistics cover only part of command time.** Their `cpu-usec` sums to 1.0 core against INFO's 9.8 cores of command time; scripts appear not to be attributed. They are used for relative ranking only.

**Raw outputs.** All under `<E>`.

- **Scripts (`scripts/`):**
  - Access: `byrun.sh` (as `by`), `rootrun.sh` (root through by-pass and `sudo -k -S`), `vgate.sh` (validator gate), `perfcheck.sh`.
  - Hosts: `snap6.sh`, `snaploop.sh`, `slotstats.sh`, `summ6.py`, `pubsub6.py`, `pubsub-rates.py`, `psample.sh`, `setsizes.sh`, `host-posture6.sh`, `softirq-cpus.sh`, `e6-timers.sh`, `freq-sample.sh`, `perfstat.sh`, `minio-perf.sh`, `minio-dirs.sh`, `minio-where.sh`.
  - Mimir: `series.py`, `hourly.py`, `winstats.py`, `cmdcompare.py`, `pernode.py`, `regress.py`, `stormfit.py`, `softirq-fit.py`, `sweepclass.py`, with `exprs-*.txt` and `windows*.txt`.
  - Local: `snapdelta6.py`, `loopdelta.py`, `slotdelta.py`, `clients-summ.py`, `slowlog-analyze.py`, `tag-slots.py`, `lua-fingerprints.py`, `preflight-callers.py`, `map-sources.sh`.
  - The pseudonym salt (`salt.txt`) stays local.
- **Mimir (`out/`):**
  - totals: `core-windows6.txt`, `core-storm-off-on.txt`, `ts-hourly-1112-1115.txt`;
  - commands: `cmd-compare6.txt`, `cmd-storm-off-on.txt`;
  - the storm: `stormfit.txt`, `ts-storm-30m.tsv`, `ts-session.tsv`;
  - sweeps: `sweep-vs-quiet.txt`, `ts-sweep-1115-0710.tsv`;
  - per request: `regress6.txt`, `softirq-fit.txt`;
  - per node: `pernode-1115-0610.txt`, `pernode-1112E.txt`;
  - hosts: `ts-e6host.txt`, `ts-e5mem.txt`, `ts-minio-6h.txt`.
- **Hosts (`host/`, deltas in `out/`):**
  - snapshot rounds: `host/loop/snap-edge-{5,6}-HHMMSS.txt` and `slot-edge-{5,6}-HHMMSS.txt` (10 rounds, 10:22–11:05Z), with `out/loop-deltas.txt`, `out/delta-1025-1100-edge-{5,6}.txt`, `out/delta-quiet-edge-{5,6}.txt`, `out/slot-1025-1100.txt` and `out/slot-quiet-1025-1040.txt`;
  - `host/summ-edge-{5,6}.txt`, with `out/slowlog-edge-*.txt`, `out/slowlog-edge-6-bycmd.txt` and `out/clients-by-source.txt` (`host/source-map.txt` maps pseudonyms to hosts);
  - `host/pubsub-edge-{5,6}.txt`, `host/psample-edge-5-6418.txt`;
  - `host/perfstat-edge-{5,6}-1.out`, `host/freq-edge-{5,6}.txt`;
  - `host/minio-perf-edge-5.out`, `host/minio-dirs-edge-5.out`, `host/minio-dirs-edge-5-2.out`;
  - `host/posture-edge-{5,6}.txt`, `host/softirq-cpus-edge-{5,6}.txt`.
- **Code (`code/`):** `preflight-callers-{aa7142599,6c0a7ffb6,f43488da1}.txt`, `lua-fp-*.tsv`, `tag-slots-6c0a7ffb6*.txt`.
- **CPUOPT5's evidence reused:** CPUOPT5 Part E's scripts and windows, and its `cpuopt5-hosts2/did-governor.out` (the governor at the switch).

### 3. Where the CPU goes now

#### 3.1 Totals, normalized (`<E>/out/core-windows6.txt`, `core-storm-off-on.txt`, `stormfit.txt`, `softirq-fit.txt`)

Mean (peak) of 5-minute rates.

| Window | Process cores | Main threads | Kernel share | Requests/s | Commands/s | µs/request | Connected clients | Requests/s per client | New conns/s |
|---|---|---|---|---|---|---|---|---|---|
| CPUOPT5 3 d, 11-10 04Z – 11-13 04Z | 20.4 (24.9) | 10.9 | 51% | 114.8k | 515k | 178 | 148.3k | 0.77 | 79 |
| CPUOPT5 11-12 18–22Z | 20.5 (21.9) | 10.8 | 53% | 121.3k | 529k | 169 | 150.0k | 0.81 | 86 |
| Governor, before the wave, 11-13 17:00–19:30Z | 21.9 (25.5) | 11.0 | 53% | 153.3k | 635k | 143 | 158.3k | 0.97 | 125 |
| **Storm off**, 11-14 00–05Z | 19.5 (22.6) | 9.69 | 52% | 136.1k | 594k | 143 | 171.8k | **0.79** | 96 |
| 11-14 18–22Z | 36.0 (39.6) | 14.9 | 55% | 311.5k | 1.24M | 117 | 242.1k | 1.29 | 1,018 |
| 11-15 00–04Z | 36.9 (41.3) | 15.1 | 55% | 329.2k | 1.31M | 113 | 270.5k | 1.22 | 756 |
| **Storm on**, 11-15 06–10Z | 38.1 (39.9) | 15.4 | 55% | 343.4k | 1.38M | 111 | 285.2k | **1.20** | 970 |
| Snapshots 10:25–11:00Z (INFO deltas) | 39.1 | 15.8 | 55% | 352.9k | 1.42M | 110.8 | — | — | 1,376 |

**By host, storm on, 11-15 06–10Z:**
- edge-6: 25.7 cores, 115 µs per request (CPUOPT5 3-day: 13.8 cores, 188 µs).
- edge-5: 12.4 cores, 103 µs (6.63, 162).

**The storm fit** (`stormfit.py`, 106 windows of 30 minutes, 11-13 06Z – 11-15 10:30Z):
- **Redis cores** = 22.12 − 0.0035 per 1k clients + 0.350 per 1k/s of excess SPOP (R² 0.96).
- **Requests/s** = 113.2k + 151 per 1k clients + 3,887 per 1k/s of excess SPOP (R² 0.98).
- **In the storm-on window** (286k clients, excess SPOP 49.8k/s): 17.4 of 38.0 cores and 193.6k of 343.2k requests/s are the storm. Each kick unit costs about 3.9 requests and 350 µs of Redis CPU.
- **Without the storm,** at today's clients, the model gives about 21 cores and about 156k requests/s.
- **Caveat:** clients and storm rose together with the bot wave. But the storm-off window, with 172k clients at 19.5 cores and 0.79 requests per client, is direct evidence that clients alone don't double Redis.

**Softirq is a host cost CPUOPT5 left out:**
- Redis's own counters don't include network softirq. On the Redis hosts it is 4.3 cores (edge-5) plus 6.6 cores (edge-6) in the snapshots.
- Regressed on requests, hourly from 11-13 17Z (`softirq-fit.txt`): 32.1 µs per request on edge-5 and 23.0 µs on edge-6 (R² 0.97–0.98), on top of Redis process CPU at 90.0 and 84.1 µs.
- The storm's ~194k requests/s therefore also cost about 4.5–6 softirq cores.
- *Writer's caveat:* on edge-5 the processes' 12.75 + 5.7–5.9 + 0.3 cores plus 4.3 softirq cores exceed the host's 20.8 busy cores (§3.6) by about 2, so part of the softirq is probably already inside the processes' time, and this softirq figure may overlap the 17.4 Redis cores by up to that share.

#### 3.2 Threads, clocks, cycles (`<E>/out/delta-1025-1100-edge-*.txt`, `regress6.txt`, `<E>/host/perfstat-*.out`, `freq-*.txt`)

Snapshots 10:25–11:00Z (2,100 s):

| Host | I/O threads (user / sys) | Main threads (user / sys) | Requests/s | µs per request, I/O / main / all-in | Involuntary switches/s, main / I/O | Event loop in commands | CPU pressure "some" |
|---|---|---|---|---|---|---|---|
| edge-6 (EPYC 7601, 8 NUMA nodes) | 16.58 (4.21 / 12.37) | 9.75 (7.63 / 2.12) | 230.0k | 72.1 / 42.4 / 114.5 | 114 / 7,670 | 4.87 of 8.81 cores (55%) | **34.6%** |
| edge-5 (EPYC 7352, 2 NUMA nodes) | 6.70 (1.85 / 4.84) | 6.05 (4.00 / 2.05) | 122.9k | 54.5 / 49.2 / 103.8 | 223 / 384 | 2.69 of 5.30 cores (51%) | 3.6% |

- **Per-request marginal cost, by regression** (CPUOPT5's method; I/O thread / whole process, µs per request):

  | Window | edge-5 | edge-6 |
  |---|---|---|
  | CPUOPT5's 11-07 00Z – 11-10 21Z, `schedutil`, before the restore (3 h windows) | 58.1 / 98.1 | 84.1 / 169.9 |
  | Governor, storm off, 11-14 00–06Z (1 h) | 41.9 / 68.7 (−28% / −30%) | 63.9 / 133.0 (−24% / −22%) |
  | Governor, storm on, 11-14 16Z – 11-15 10Z (1 h) | 39.7 / 65.9 | 47.8 / 78.7 |

  On edge-6 the storm-off row also carries `numa_balancing=0` and the end of the restore, so the −22% is not the governor alone.
- **Counted with perf stat, 30 s each:**

  | Node | Requests/s | CPU | Effective clock | IPC | Cycles per request | CPU µs per request | CPUOPT5 |
  |---|---|---|---|---|---|---|---|
  | 6418, edge-5 (median) | 7,006 | 0.785 cores | 2.12 GHz | 0.56 | 237k | 111.7 | 2.25k/s, 1.40 GHz, IPC 0.69, 249k, 178 µs |
  | 6386, edge-6 | 7,199 | 0.836 cores | 2.54 GHz | 0.43 | 294k | 115.9 | 2.0k/s, 2.31 GHz, IPC 0.40, 417k, 180 µs |

  - edge-5's cycles per request are flat. Its time per request fell 37% because the clock rose 51% (`performance`, max 2.3 GHz, boost off).
  - edge-6's cycles per request fell 30% at 3.6× the requests per node.
- **Clock under the Redis threads, 10 samples each:**
  - edge-5: 2,295 MHz for main and I/O threads, 0% below 1.6 GHz. CPUOPT5: 1,615–1,618 MHz, 78% below.
  - edge-6: 2,593 (main) and 2,611 (I/O) MHz. CPUOPT5: 2,355 and 2,613.

#### 3.3 Per node, key families and slots (`<E>/out/pernode-1115-0610.txt`, `slot-1025-1100.txt`, `<E>/host/setsizes`)

- **No node is hot.**
  - Median node: 28.2k commands/s (10.7k in CPUOPT5); median main thread 0.301 cores (0.217).
  - The busiest main threads: 6427 (0.421), **6415 (0.419)**, 6424 (0.412), 6416 (0.392). All edge-5 nodes run slightly busier main threads than edge-6's (0.28–0.36), at lower µs per request.
  - Load is even because the storm's keys are per client and per network (`{ca_<client>}`, `{ns_<network>}`, `{<client>}c_close`) and spread over all slots.
- **New connections are uneven:** edge-6 nodes accept 23–25/s each, edge-5 nodes 12–16/s (snapshots: 1,104/s on edge-6, 272/s on edge-5).
- **Fixed hash tags that stand out:**
  - **Slot 9358, `{provider_hard_exclusions}` on 6415:**
    - 96 mcores and 9.5% of all slot CPU, with 7.1 MB/s inbound.
    - The inbound is SMISMEMBER's argument lists: 339–551 calls/s at 182–219 µs, up 10× from CPUOPT5's 35–48/s, 0.07–0.10 cores.
    - The set holds 112,154 members (TTL 1,796 s). It is rewritten whole every ~14.5 s, and each rewrite is a 115 ms whole-node slow entry (§3.7).
  - **6386 holds `{provider_modern}` and 6399 holds `{provider_post_quantum_positive}`** (D42's capability sets, `model/network_client_score_capability.go:206–207`).
    - 72,419 and 72,546 members, each with a proof hash of the same size (TTL about 5 h).
    - Both are rewritten whole about every 4 minutes (§5.2).
  - **Slot 4467 on 6420:** `verify_eligible_v2`, as in CPUOPT5; 6.8 mcores.
- **High-output slots:** 3175 (6380) 9.7 MB/s, 14906 (6399) 9.4 MB/s, 1945 (6424) 5.7 MB/s, 15094 (6423) 3.9 MB/s; 27% of all output from 4 of 16,384 slots.
  - Their key families are ordinary per-client ones (`{pm_*}rp`, `{escrow_*}net_revision`, `{*}s2_c_eid`, `ckey_*`), and no `{ns_*}z` session set lives there.
  - The hot key is not identified, because key names weren't read. Their CPU is small: 1.1–2.8 mcores.
- **Memory and keys:** used memory 31.4 → 36.5 GB, keys 147M → 187M, Redis connections 66.5k → 103k (peak 246k).
  - Per node, `latest_fork_usec` averages 62 ms and the last BGSAVE took 16.5 s on average.
  - Node 6411 has evicted 367 clients under `maxmemory-clients` since its start.

#### 3.4 Commands, and quiet against sweep (`<E>/out/cmd-compare6.txt`, `cmd-storm-off-on.txt`, `sweep-vs-quiet.txt`, `loop-deltas.txt`)

| Command | CPUOPT5 11-12 18–22Z calls/s (cores) | Storm off 11-14 00–05Z | Storm on 11-15 06–10Z (cores, µs/call) | Storm part (fit) |
|---|---|---|---|---|
| EVAL | 53.2k (4.21) | 65.8k (3.53) | 109.8k (5.63, 51) | 40.8k |
| EVALSHA | 6.4k (0.65) | 5.6k (0.42) | 8.3k (0.50, 60) | — (the control-limit sawtooth) |
| PTTL | 29.8k | 34.9k | 143.3k (0.058, 0.4) | 104.8k |
| PEXPIREAT | 16.1k (0.18) | 19.9k (0.17) | 99.5k (0.53, 5.3) | 80.0k |
| ZREMRANGEBYSCORE | 19.2k | 22.5k | 94.9k (0.085) | 70.5k |
| GET | 66.3k (0.20) | 70.3k | 135.9k (0.15) | 62.4k |
| PING | 14.4k (0.012) | 17.3k | 77.6k (0.033) | 57.8k |
| SPOP | 3.2k | 5.1k | 52.9k (0.071) | 49.7k |
| EXISTS | 7.8k | 9.7k | 54.6k | 46.7k |
| ZREVRANGE / ZRANGEBYSCORE | 13.7k / 9.9k | 16.2k / 11.8k | 65.0k / 52.7k | 45.7k / 40.7k |
| TIME | 38.6k | 47.9k | 73.3k | 28.0k |
| ZADD / HSET / HMGET | 17.8k / 12.0k / 8.0k | 18.5k / 13.5k / 8.8k | 43.1k / 35.5k / 29.6k | 20.8k / 19.1k / 20.6k |
| HELLO / PSUBSCRIBE | 78 / 9 | 88 / 14 | 929 / 145 (CLIENT SETINFO about 1.7k) | — |
| SMISMEMBER | 48 (0.022) | 77 | 339 (0.074, 219 µs) | — |
| **Total** | 531k (7.65 cores of command time) | 592k (6.02) | 1.37M (9.30) | — |

- **Command time rose 1.5×; process CPU rose 1.95×.** The growth is per-request I/O, kernel and softirq work, not command execution.
- **PING sources:** 25.3k/s of contract-hole calls (connect 22.7k, api 1.06k, taskworker 1.07k, proxy 0.51k; `urnetwork_contract_hole_operations_total`) plus 52.9k/s of close-listener kicks (`TakeClientCloseRequests`, one PING and one SPOP each) ≈ 78k = PING 77.6k. In CPUOPT5, contract-hole calls explained 73–76% of PINGs; now 33%.
- **Quiet against sweep** (`sweepclass.py`; a sweep window means live hosted devices rose ≥1,000 in 5 minutes):

  | Window | Sweep windows | Quiet windows | Sweep/quiet: requests, cores, PING, SPOP, new conns, proxy round trips |
  |---|---|---|---|
  | 11-14 22Z – 11-15 10:50Z | 49 (rise 2.6k/5 min) | 21 | 0.999, 1.003, 0.98, 0.96, 0.92, 1.13 |
  | 11-14 16–22Z (bigger sweeps, up to 90k live) | 34 (rise 4.6k/5 min) | 4 | 1.08, 1.09, 1.10, 1.14, 1.38, 1.51 |

  - In the snapshots, the 10:40–10:50Z sweep (41.9k → 45.7k live devices) ran 353.5k requests/s and 38.8–39.3 cores, against 349.8–355.5k and 38.8–39.3 in the quiet intervals around it. Merge drops held at 3.2/s throughout.
  - A sweep's own device opens are tiny next to the storm's steady load. The proxy's round trips do follow the sweeps (+13–51%), but the proxy is 2% of Redis requests.

#### 3.5 edge-6 host posture (`<E>/host/posture-edge-6.txt`, `softirq-cpus-edge-6.txt`, `e6-timers`, `<E>/out/ts-e6host.txt`)

- **Posture now:**
  - `performance` governor with boost on (2.2 GHz base, up to 3.19); `iommu=pt` active; `numa_balancing=0`, persisted in `/etc/sysctl.d/60-bringyour-numa-balancing.conf`.
  - Swappiness 60; Redis swap 0. ZFS ARC at its 253.5 GB cap.
  - conntrack holds 119,981 entries (58k in CPUOPT5).
  - ZFS module parameters are still `zio_taskq_batch_pct=80` and `spl_taskq_thread_priority=1`; the staged ones take effect at the next reboot. edge-6 has been up since 11-03 04:23Z.
- **The restore is over.**
  - `bringyour-pg-backup.service` ran from 11-10 21:15Z to 11-15 05:45:37Z (success). Its timer shows no next run.
  - The WAL replay's write load ended about 11-13 21Z:

    | | Through 11-13 18Z | After |
    |---|---|---|
    | Disk writes | 150–220 MB/s | 11–50 MB/s |
    | System time | 14–19 cores | 6–9 cores |
    | Slow entries | 16–37k/h | 100–330/h (25–38 per node-hour) |

  - A last 116–153 MB/s write phase ran about 11-15 00–04Z (2-hour windows), with slow entries at 318–825/h through 06Z and 16.7 system cores in 04–06Z. The run ended at 05:45Z.
  - ZFS writer threads use 0.02 cores now.
- **NIC interrupt concentration (new).**
  - The live NIC, `ens3f1np1` (i40e, 119 queues), sits on NUMA node 2. Its interrupts are served by CPUs 80–87, and CPUs 21–23 are among the most loaded.
  - Over 10 s, 16 of 128 CPUs ran above 30% softirq, the top ones 96–97% busy with 38–48% softirq. The mean CPU ran 5.1% softirq.
  - NET_RX since boot: the top 8 CPUs handled 65.2% (edge-5: 21.7%).
  - **30 of the 32 Redis I/O threads were last on node 2's CPUs**; the main threads were on nodes 3–7. Each request's handoff between main and I/O thread therefore crosses nodes.
  - irqbalance is inactive, and `net.core.rps_sock_flow_entries` is 0 (RFS off).
  - This is consistent with the 34.6% CPU pressure, the I/O threads' 7.7k involuntary switches/s, and edge-6's 72 µs I/O-thread cost per request against edge-5's 55. The wake-affine pull of the I/O threads toward the softirq CPUs is *(inferred)*.

#### 3.6 edge-5 and its co-tenants (`<E>/out/delta-1025-1100-edge-5.txt`, `ts-e5mem.txt`, `ts-minio-6h.txt`, `<E>/host/minio-perf-edge-5.out`, `minio-dirs-edge-5*.out`, `posture-edge-5.txt`)

- **Host, 10:25–11:00Z:** 20.8 busy cores (user 6.4, system 10.0, softirq 4.3, iowait 0.2).
  - Redis 12.75, MinIO 5.7–5.9, kworkers and the rest about 0.3.
  - CPU pressure "some" 3.6%; 450k context switches/s, about 193k of them Redis's.
- **MinIO:**
  - 5.4–7.3 cores per 5-minute interval in the snapshots; 3.1–6.6 hourly over the last day, following its daily PUT cycle (58–87 PUT/s).
  - 58,338 OS threads (unchanged since 11-14); RSS 34 GB; 6.42 GB in swap.
  - Swap-ins are 0.8/s, and major faults are 0.8/s host-wide.
- **Where MinIO's CPU goes** (15 s sample of its cgroup; proportions of sampled time):
  - 88.2% kernel, 11.6% MinIO user code (the binary is stripped).
  - **The directory grow path, about 39%:** `xfs_iext_get_extent` 20.5%, `xfs_bmap_first_unused` 9.9%, `xfs_iext_next` 6.9% and `xfs_dir2_node_find_freeblk` 2.0%. All of it sits under `xfs_dir2_node_addname` → `xfs_dir2_node_add_datablk` → `xfs_dir2_grow_inode` → `xfs_da_grow_inode_int` → `xfs_bmap_first_unused`, which walks the directory's whole data-fork extent list.
  - **The lock spin, about 34%:** `osq_lock` 18.6% and `rwsem_spin_on_owner` 15.4%, under `do_mkdirat` → `filename_create` → `down_write` → `rwsem_optimistic_spin`. Threads spin on the parent directory's lock while its holder walks.
  - The rest is under 1% per symbol.
- **The directory** (`minio-dirs.sh`):
  - The `blob` bucket's `blob/st/v1/evidence/content/sha256` is 18,540,584,960 bytes, with 2,586,684–2,851,807 data-fork extents on each of the 8 drives. It grew 18.0 MB per drive in 40.9 minutes: 26.4 MB/h, or 0.63 GB/day at this hour.
  - The other flat `st/` paths are small: `st/v2/attempt/*/sha256` has 9–46 extents and `st/v2/ledger/sha256` has 1.
  - Free inodes: 716M per drive, falling 11.6M/day.
  - MinIO's drive-call latency (`minio_system_drive_api_latency_micros`): `storage.RenameData` 37.1 ms and `storage.CreateFile` 30.8 ms, against `ReadXL` 8.8 and `ReadVersion` 8.2.
- **XFS buffers and reclaim:**
  - MemAvailable was 191 GB at 02Z after the 00:45Z drop_caches and 126 GB at 11Z: about 7.2 GB/h refilling, as the memory note says.
  - In the snapshots: `pgscan_kswapd`, `pgsteal_*`, `kswapd_inodesteal` and `allocstall` all 0; `slabs_scanned` 199/s; no kswapd or XFS reclaim thread in the CPU table.
  - edge-5's Redis µs per request was 102–104 before and after the drop. No Redis cost from the buffer cache is measurable today.
- **Competition with Redis:** small now. Redis main threads took 223 involuntary switches/s across 16 threads (14/s each). Softirq is spread evenly (no CPU above 30%). edge-5 is the cheaper Redis host per request.
- **Hardware** (not CPU):
  - EDAC mc1 corrected errors: 204 at 10:25Z, 206 at 11:00Z (196 at 00:45Z). mc0 has 0.
  - 790 `[Hardware Error]` kernel lines since 00:00Z (a count of the CPU 24 lines alone, 120, misses formats), with 0 uncorrected.
  - `iommu=pt` is still staged (DMA-FQ); edge-5 has been up since 10-23.

#### 3.7 Slowlog and latency (`<E>/out/slowlog-edge-5.txt`, `slowlog-edge-6.txt`, `slowlog-edge-6-bycmd.txt`)

All slowlogs hold their full 1,024 entries; the threshold is still 10 ms.

- **edge-6:** 25–38 entries per node-hour, back to the pre-restore rate (CPUOPT5's replay: 346–820).
  - The common entries are scripts at the 10–20 ms floor: the control-limit script 3,452 entries over about 40 h, the liveness-lease shard script 3,211, the client-score export 3,186, the contract-hole read 2,726, `sessionLuaCommon` 1,992, the connection-authority publish 1,761.
  - Two nodes stand out. **6386** logs 472 HSET (mean 112 ms, max 186) and 473 SADD (94 ms); **6399** logs 400 HSET (118 ms) and 399 SADD (97 ms). That's about 14.5 pairs an hour each, at gaps of 164–296 s: the capability-set rewrites (§5.2).
- **edge-5:** only 6415 is active: 249 entries an hour, all `SADD {provider_hard_exclusions}`, mean 115 ms, max 191 ms, gaps of 13–17 s. The other 15 nodes' newest entries are from 11-01..11-03.
- **LATENCY LATEST:** events of 100 ms or more within the last 6 hours on 2–3 nodes per event type (`fast-command`, `command`); fork events on 12 edge-6 nodes within 24 h.

### 4. What CPUOPT5's items did

| CPUOPT5 E item | Status at the deployed revisions | Measured now | Still worth doing? |
|---|---|---|---|
| §5.1 / #9: no preflight PING by default | **Not shipped.** redis.go unchanged since 10-28 (508caeb58): `preflight := true` at :597, PING at :625–626 for no-retry and deadline pools. 41 call sites still preflight (`<E>/code/preflight-callers-*.txt`); new since CPUOPT5: `publishProviderCountryFloor` and `readProviderCountryFloor` (`model/network_client_score_country_floor.go:362`, :379) | PING 14.4k → 77.6k/s: 52.9k from close-listener kicks, 25.3k from contract-hole calls | yes, #2 |
| §5.2 / #6: performance clock policy | **Live** 11-13 ~16:56Z on both hosts (`bringyour-cpu-governor.service`, xops), boost and C-states unchanged | At the switch, matched hour against 1–2 days earlier: edge-5 −19.7 to −20.6% CPU per request; edge-6 −6.0% while the restore ran (`cpuopt5-hosts2/did-governor.out`). Marginal per request: edge-5 −28 to −30%, edge-6 −22 to −24% (§3.2). Clocks under Redis 1.62 → 2.30 GHz on edge-5. perf: 6418 cycles per request 249k → 237k at 1.40 → 2.12 GHz | done. CPUOPT5 predicted 2.8–4.2 cores at 11-13's load. At today's load the measured shares are worth about 3.2–3.6 cores on edge-5 and 1.7 (at the switch) to 4 *(inferred, no restore)* on edge-6 |
| §5.3 / #15: edge-6's restore off Redis's CPUs | 8K clone (`zfs clone -o recordsize=8K`) deployed in xops; live from the next run, but the 11-10 run ended 11-15 05:45Z and no next run is scheduled. ZFS taskq parameters in modprobe.d, live at the next reboot (not yet) | The stalls ended with the replay (§3.5). Slow entries 25–38 per node-hour; ZFS writers 0.02 cores | verify at the next run (4.4 days long) |
| §5.4 / #20: `kernel.numa_balancing=0` on edge-6 | **Live** 11-13 11:36Z, persisted in `/etc/sysctl.d/60-bringyour-numa-balancing.conf` | `numa_pages_migrated` 18.1k/s → 0. Slow entries 34.9k → 12.9k/h at the switch (during the restore). Its CPU (predicted 0.15–0.25) is not separable | done |
| §5.5: the `/connect/control` creep | The stuck-provider signal reads on for every api block since 11-14 01:09Z (stage 2, by the stage log). The recycle is off: `urnetwork_connect_stuck_provider_recycle_enabled` = 0 on every block; about 120k would-recycle decisions per g-block per day; 0 transport closes | The sawtooth continues: admits 1.6k → 14.6k/s per connect cycle (EVALSHA 4.9k → 18k/s); 2.3k–9.5k admits/s on 11-15 | per the owner's staged plan (Part C) |
| §5.6 / #14: GETEX; prune inside the MULTI | **Not shipped**: GET then EXPIRE (`model/network_client_resident_model.go`, `GetResidentForClient`); prunes outside the transaction (`model/peer_model.go:1370`, :1938) | EXPIRE 15.9k/s | yes, small: #7 |
| §5.7 / #18: EVAL → EVALSHA | **Not shipped**: contract-hole `client.Eval` at `model/subscription_contract_hole.go:111`, :277, :372, :408; session scripts through `sessionEval` (`session/session_model.go:142–160`); `session/connection_authority.go:45` | EVAL 53k → 110k/s; inbound 108 → 220 MB/s | yes, #5 |
| §5.8: `{provider_hard_exclusions}` published on change | **Not shipped**: DEL + full SADD + EXPIRE every run (`model/egress_index.go:1181–1183` at 6c0a7ffb6) | 112,154 members every ~14.5 s, 115 ms per rewrite (CPUOPT5: 118 ms every ~12 s). SMISMEMBER 10× | yes, for latency: #6 |
| §5.9: per-slot statistics | **Live** on all 48 nodes, `cpu net` (xops 36c5217) | +1.4% CPU on the test node against a ±1.1% baseline spread. Counts 1.0 of 9.8 command-time cores (scripts apparently not attributed); still finds fixed hot tags (§3.3) | done; diagnostic |
| §5.9: `iommu=pt` on edge-5 | Staged in grub; no reboot (DMA-FQ, 126 groups) | — | at a planned reboot (§6, owner items) |
| §5.9: conntrack bypass | Owner: not chosen (11-13) | edge-6 conntrack 58k → 120k entries with the connection churn | no (owner); the churn goes with #1 |

**Prediction against outcome:**
- The governor beat its prediction per request: −20% on edge-5 at the switch against 15–25% predicted, and −22 to −30% at the margin.
- CPUOPT5's code items scale with request volume, and volume tripled, so each is worth about 2–3× its CPUOPT5 estimate while the storm runs and about 1.3× after it.

**Confounders:**
- The bot wave from 11-13 ~20Z.
- The end of the restore at 11-13 ~21Z.
- `numa_balancing` off at 11-13 11:36Z.
- Releases: D46 (11-13 night), D47 (11-14 06–09Z), D48 and D50 (11-14), D49p (11-14 17:30–18:38Z), the 11-15 builds at 09:50–10:05Z.
- No connect release since 11-14 07:38Z, but the config release restarted every connect process at 01:11–01:13Z on 11-15 (A §8), which reset the control-route sawtooth (C §3.1).

### 5. New costs

#### 5.1 The key-event resubscribe storm (with Part A §5.1)

- **What it is.** connect runs one key-event subscriber per process (`connect/key_event_subscriber.go`).
  - It pattern-subscribes on every master: `{np_*}p:*`, `{ns_*}eid`, `{*}s2_c_hops`, `{*}c_removed`, `{*}c_close`, and `{*}c_recycle` where the recycle ceiling is on (:124–138).
  - Its per-node drains feed one shared merge of `MessageBufferSize` 1,024 (redis.go:698–706, :760). A drain that finds the merge full counts a drop and ends the whole generation (redis.go:820–826).
  - The supervisor (`run`, key_event_subscriber.go:450–521) then:
    1. waits `ResubscribeTimeout` (5 s) and resubscribes on all 48 masters;
    2. calls `resyncAll()` (:257–335): session rechecks, one peer read per network, hop reconciles, spread over `ResyncSpreadTimeout` (30 s) and cancelled by the next wave;
    3. calls `resyncCloseListeners()` (:224–249): a `Kick()` of every close listener over 30 s, **not** cancelled by the next wave.
  - Settings are at connect/resident.go:702–713.
- **What feeds it.** On 6418, connect's patterns deliver 572 events/s for 10 s (10:41:14Z), and 87% are `expire` events on `{ns_<network>}eid`:

  | Event | Rate on 6418 |
  |---|---|
  | session `expire` | 499/s |
  | c_close sadd / expire | 26 / 14 |
  | c_removed set / expire / expired | 8 / 8 / 4.5 |
  | c_close spop / del | 4.4 / 4.4 |
  | peer, hops | ≤1.5 each |

  - The session events name 688 networks in 10 s, and the busiest network's share is 0.5%.
  - Across 48 nodes that's about 27k events/s per subscribing connect process. Each connect subscription connection receives 61–71 KB/s per node, about 3.2 MB/s per process; the proxy's single-pattern subscriptions get 2.6–3.1 KB/s.
  - `dispatch` discards `expire` first thing (key_event_subscriber.go:340–343), but each one has already taken a merge slot.
- **Who makes the `expire` events.**
  - `NetworkSessionRevision` (session/session_model.go:466) only needs the generation and event id, yet runs `readSessionInventory`. That's the full `sessionInventoryLua` (:208–215): `prune()`, `revision()`, a `ZRANGEBYSCORE … WITHSCORES` of every live session of the network, a `ZREVRANGE`, and `shared(max(now+24h, maxretain))`.
  - `shared()` calls `extend()` on four keys (`sessionLuaCommon`, :87–105). Because `now+24h` always moves, every call PTTLs and PEXPIREATs all four keys, so every call emits an `expire` event on `{ns_<net>}eid`.
  - `runSessionNotifications` (connect/session_notifications.go:121–185) makes that read for each network group:
    - on a kick, coalesced to one read per 5 s;
    - on a 60 s ticker;
    - only while the group has hint listeners, which every resident registers (connect/resident.go:4832).
  - `resyncSessionRechecks()` (:104–119) nudges every group on every resubscribe.
  - The other callers of `readSessionInventory` are `GetNetworkSessions` and revocations, both rare. So connect's 18–27k inventory reads/s are hint reads.
- **Who overflows, and the loop.**
  - The g-block processes overflow; beta's don't:

    | Block | Clients per process | connect CPU per process | Resubscribes per process | Inventory reads |
    |---|---|---|---|---|
    | g1–g4 | 11.4–17.9k | 7.25–7.62 cores | 0.11–0.13/s (every ~8.3 s) | 5.5–6.1k/s per block |
    | beta | 750 | 1.57 cores | 0.0003–0.023/s | 89/s |

  - At the census (four edge-6 nodes) only 6 of the 20 connect processes held a subscription, with ages 140–3,359 s: 4 with the recycle pattern and 2 without. The rest were between a drop and their next resubscribe. Which ones were beta's isn't separable from CLIENT LIST.
  - Each resubscribe nudges every group into another hint read, and every hint read emits another `expire` event: a feedback loop.
- **What each resubscribe costs Redis:**
  - 48 new pub/sub connections (HELLO + 2 CLIENT SETINFO + PSUBSCRIBE each) and 48 closes.
  - A kick of every close listener. Each kick runs `TakeClientCloseRequests` (model/client_close_model.go:165–185), a `RedisDoOnce` with its preflight PING plus `SPOPN`. Waves overlap about 3–4 deep, because each lasts 30 s and they come every ~8.3 s.
  - A hint read per network group.
  - Lease rechecks, each a credential check plus `PublishConnectionAuthority` (an EVAL of 493 B: ZRANGEBYSCORE, HDEL loop, ZREMRANGEBYSCORE, ZADD, HSET, ZREVRANGE, 2 PEXPIREAT; session/connection_authority.go:27–47).
- **History** (`<E>/out/ts-storm-30m.tsv`, `ts-session.tsv`):

  | When | Merge drops/s | Resubscribes/s | SPOP/s | Inventory/s | Redis cores |
  |---|---|---|---|---|---|
  | 11-13 12–19Z, before the wave | 0.23–0.35 | 0.22–0.33 | 3.6–5.9k | 1.8–2.2k | 20.5–24.1 |
  | 11-13 22Z, the first burst | 2.1 | 1.7 | 36k | 12k | 32.5 |
  | 11-14 00–06Z | 0.27–0.32 | — | 4.6–5.7k | 2.3–2.6k | 18.7–20.1 |
  | 11-14 15Z onward | 2.0–3.4 | 1.5–2.3 | 33–57k | 15.6–26.6k | 31–39 |

  At 11-14 00–06Z the storm was off even though 170–190k clients were connected.
- **Side effects:**
  - connect pool misses 32.6 → 1,316/s, and Redis new connections 96 → 970–1,104/s. Most new connections are short-lived: CLIENT LIST sees about 250–340/s surviving 10 s.
  - connect pool timeouts 0 → 1.2/s.
  - Session inventory "unavailable" 8–20 → 700–1,430/s, failures within `RedisAuth`'s 2 s budget (redis.go:343–363).
    - `RedisAuth`'s pool has no idle floor and is capped at max(16, `max_connections`) per node; "a raced dial is then used once and closed" (redis.go:203–227).
    - Each recheck for a network hits that network's one slot.
    - So bursts of rechecks for a large network queue on one node's auth pool *(inferred)*.
  - Net in 123 → 220 MB/s; net out 34 → 87 MB/s.
- **Verdict:** 17.4 Redis cores and about 194k requests/s (§3.1), plus 4.5–6 softirq cores. This is the largest Redis cost on record, and it is new. Fix: #1.

#### 5.2 Other items

| Item | Code | Measured | Verdict |
|---|---|---|---|
| D42/FP2 S2: the four capability sets, replaced on every export | `publishProviderCapabilitySets`: per set, one MULTI of DEL + SADD of every member + EXPIRE, and for the two post-quantum sets DEL + HSET of every proof + EXPIRE (model/network_client_score_capability.go:214–265 at 6c0a7ffb6) | `{provider_modern}` (6386) and `{provider_post_quantum_positive}` (6399): 72.4–72.5k members plus equal proof hashes, rewritten every ~4 min; HSET 112–118 ms + SADD 94–97 ms per rewrite, about 14.5 pairs an hour per node; 2/48 of the slots wait up to ≈210 ms each time | latency only (≈0.002 cores); #6 |
| `{provider_hard_exclusions}` reads | FindProviders2's hard-exclusion check, SMISMEMBER on 6415 | 48 → 339–551 calls/s at 182–219 µs (0.07–0.10 cores), the second-busiest main thread (0.419) | small; owner items (§6) |
| Proxy zero-byte idle 10 min (f43488da1, D49p) and the hosted devices | proxy device lifecycle | proxy Redis round trips 1.1k (CPUOPT5) → 7.5–8.1k/s; contract-hole events from the proxy 511/s; not separable from the bot wave | small (2% of requests) |
| Stuck-provider signal (D47, all api blocks) | api control path | api Redis round trips 12.9k → 17.7–24.7k/s; the EVALSHA sawtooth unchanged | small; recycle staged by the owner |
| D4 caches, PQ follow-ups, gift rails, streaks, account epochs, PgBouncer stage 1 | no hot Redis call sites in aa7142599..6c0a7ffb6..11d5a8e78 (diffs of the hot files are empty) | — | none |
| The 11-15 09:50–10:05Z builds (api, alt, taskworker) | 11d5a8e78 by timing: one `RedisWithDeadline` hook install, no hot path | snapshot rounds across 10:00Z flat | none |

### 6. Ranked opportunities

#### #1 Stop the key-event storm (new; with A #1)

- **Change, in order:**
  1. **(a) A revision-only session hint read.** Give `NetworkSessionRevision` (session/session_model.go:466) its own script: `sessionLuaCommon` + `prune(); local generation,eid=revision()`, returning only those two.
     - It drops the whole-membership `ZRANGEBYSCORE … WITHSCORES`, the `ZREVRANGE` and the `shared()` TTL extension from the hint path.
     - `prune()` stays, so expiry still bumps the event id as today.
     - Separately, make `extend()` (:91–94) overshoot: when the key's expiry is before the deadline, set it to `deadline + slack`, otherwise do nothing. Every key still outlives every deadline, and a network's keys are re-extended at most once per `slack`, not on every call.
     - `slack` is a settings field with one default (for example 1 h on a 24 h horizon).
  2. **(b) Drop `expire` before the bounded merge.** Add `IgnoredEvents` to `RedisKeyEventSubscriptionSettings` (redis.go:693–707), default `{"expire"}`, and let each node drain discard those payloads before `out`.
     - connect already discards them (key_event_subscriber.go:340). Check the proxy's client-removed consumer (proxy/proxy_client_removed.go) before relying on the default.
     - A TTL refresh can then never end a subscription epoch.
  3. **(c) A cheaper close-listener resync.** Instead of one `TakeClientCloseRequests` (PING + SPOP round trips) per listener, issue the resync's SPOPs as pipelines per node, without the preflight. Every listener is still taken once per wave.
- **Evidence:** §5.1.
  - Merge drops are 3.2/s, and the g-blocks resubscribe every ~8.3 s.
  - 87% of the subscriber's stream is `expire` on `{ns_*}eid`.
  - Inventory reads track SPOP (inventory ≈ 0.46 × SPOP).
  - The storm-off window (19.5 cores at 172k clients) against storm on (38.1 at 285k), and the fit (17.4 cores; R² 0.96).
- **Saving:**
  - 15–17 Redis cores: from about 38 to about 21–23 at today's clients (the fit's no-storm value plus margin).
  - 4.5–6 host softirq cores on edge-5 and edge-6: 194k requests/s at 23–32 µs.
  - The connect-side CPU of 190k round trips/s, 1.3k dials/s and the resync goroutines (Part A).
  - Any PostgreSQL reads behind the lease rechecks (Part D).
  - At baseline the hint read also gets cheaper: about 7 commands instead of 17, and no member list in the reply *(inferred)*.
- **Effort and risk:**
  - (a) and (b) are small code changes, low risk; they change neither the hint's content nor which events consumers act on.
  - (c) touches the close-delivery path: medium.
- **Proving tests** (deterministic, in the server's test environment, with keyspace notifications on through `server.Testing_EnableKeyspaceNotifications`):
  - **(a):**
    - `NetworkSessionRevision` returns the same (generation, event id) as `readSessionInventory` for the same state.
    - It issues no PEXPIREAT and publishes no `expire` event on `{ns_<net>}eid`, captured with a test pattern subscriber.
    - It returns no members.
    - A second `extend()` within `slack` issues no PEXPIREAT, and the key's expiry never falls below the deadline.
  - **(b):** more than `MessageBufferSize` `expire` notifications, during startup and after, end no generation (`urnetwork_redis_key_event_merge_drops_total` unchanged), and a following `set` is still delivered.
  - **(c):** a resync wave over N listeners on M nodes makes at most ⌈N/batch⌉ round trips per node, and every pending request is taken exactly once.
- **Production check:**
  - merge drops and resubscribes below 0.3/s (the pre-wave level);
  - SPOP below 6k/s; inventory reads below 6k/s;
  - connect pool misses below 50/s; inventory "unavailable" below 20/s;
  - Redis cores about 21–24 at today's clients.
- **Owner rules:**
  - No timeout, deadline, keepalive, probe, detection or revocation bound moves. These all stay as they are: the 60 s lease recheck, `CorrectivePollInterval` 5 min, `ResyncSpreadTimeout` 30 s, `ResubscribeTimeout` 5 s, `MessageBufferSize` 1,024, `RedisAuth` 2 s and the 5 s hint coalescing.
  - Session keys still outlive every deadline; `slack` only lengthens how long idle bookkeeping keys survive.
  - No new off-by-default settings: `slack` and `IgnoredEvents` each have one default, on.
- **Owner decision:** none for (a) and (b). (c) after (a) and (b) are measured.

#### #2 No preflight PING by default (CPUOPT5 E §5.1; unshipped; with A #3)

- **Change:** as CPUOPT5. In `redisWithClient` (redis.go:594–627), the PING becomes opt-in (`OptRedisPreflight()`). First among the 41 sites: `TakeClientCloseRequests` (model/client_close_model.go:167) and the contract-hole reads and events (model/subscription_contract_hole.go:109, :276, :371, :407).
- **Evidence:** PING is 77.6k/s, 22.6% of requests: 52.9k from close kicks and 25.3k from contract-hole calls (§3.4). Each is its own request on a random node.
- **Saving:** at a marginal 66–79 µs of Redis CPU (regression, storm on) plus 23–32 µs of softirq per request:
  - now: 5.1–6.1 Redis cores + 1.8–2.5 softirq;
  - after #1 (about 28k/s left): 1.8–2.2 + 0.6–0.9.
- **Risk:** low. A connection error surfaces from the first real command through the same path; these pools never replay.
- **Proving test:** a deadline-pool call issues no PING by default and one with the opt-in (as CPUOPT5).
- **Owner rules:** each callback keeps its own deadline (`contractHoleRedisTimeout` 1 s and the others); no bound moves.
- **Owner decision:** none.

#### #3 Spread edge-6's NIC interrupts, or turn on RFS (new; host)

- **Change:** a default-on task in the Redis play (xops; host settings live in playbooks). Either:
  - set the i40e queue IRQs' affinity across all NUMA nodes; or
  - enable RFS: `net.core.rps_sock_flow_entries` and each RX queue's `rps_flow_cnt`. Protocol processing then runs where each connection's consuming thread runs, and stops pulling the I/O threads to node 2.

  Measure one, then template it.
- **Evidence (§3.5):**
  - 16 CPUs at 96–97% busy, 38–48% softirq.
  - 30 of 32 I/O threads on those CPUs.
  - CPU pressure 34.6% against edge-5's 3.6%.
  - I/O threads: 7,670 involuntary switches/s against 384 on edge-5; 72.1 against 54.5 µs per request.
  - NET_RX top-8 CPUs: 65.2% against 21.7%.
  - irqbalance inactive; RFS off.
- **Saving *(inferred)*:** 10–25% of edge-6's 16.6 I/O-thread cores, so 1.7–4.1 cores, plus tail latency on all 32 edge-6 nodes. edge-5, where softirq is spread and threads share a node with their NIC queues, is the reference.
- **Risk:** low–medium (network path; reversible).
- **Proving test:**
  - The playbook's check run shows only the affinity or RFS values, with no restarts.
  - A host check script asserts the values after apply.
  - Acceptance on edge-6, at matched requests against the hour before: CPU pressure "some" < 10%, I/O-thread involuntary switches < 1k/s, µs per request lower.
  - Revert if µs per request rises above +5%.
- **Owner rules:** host setting in the playbook, not by hand; no Redis or application bound changes.
- **Owner decision:** yes (host posture), in a validator-idle window.

#### #4 Shard MinIO's flat evidence directory (edge-5 co-tenant)

- **Change:** new content keys under two hex levels.
  - `EvidenceContentKey` (startifact/evidence.go:139–148) writes `st/v1/evidence/content/sha256/<h[0:2]>/<h[2:4]>/<h>.json`.
  - Readers try the new key, then the legacy flat key.
  - The flat directory becomes read-only: lookups in it stay fast (hashed B-tree), and it stops growing.
  - Treat the history keys (`EvidenceHistoryKey`, :176–187) the same way if their run directories grow.
- **Evidence (§3.6):**
  - About 73% of MinIO's sampled CPU is mkdir in this directory: the extent walk 39% and the lock spin 34%.
  - 2.6–2.85M extents per drive, growing.
  - `RenameData` 37 ms, `CreateFile` 31 ms.
  - The XFS 32 GiB data-section wall falls between 12-10 and 12-27 at 0.63 GB/day (this hour) to 60% of that. At the wall mkdir fails, and evidence PUTs with it.
- **Saving *(inferred)*:** 73% of MinIO's 4–6 core daily range, so 3–4.5 cores on edge-5, plus PUT latency for every writer behind MinIO (the SN25 validator reads artifacts there).
- **Risk:** medium; a storage layout change on SN25's evidence path.
- **Proving tests:**
  - key derivation: the new layout for a given hash;
  - dual read: a legacy object is still found, a new object is written only under the new prefix;
  - nothing is ever written to the legacy prefix.
- **Owner rules:** no bound changes. Evidence stays immutable and content-addressed. Retention unchanged (lifecycle is a separate owner/SN25 call).
- **Owner decision:** yes, owner and SN25 (evidence layout and retention). Time it before December.

#### #5 EVAL → EVALSHA (CPUOPT5 E §5.7; unshipped)

- **Change:** as CPUOPT5: `redis.NewScript(...).Run`, with NOSCRIPT retried by body in pipelines. Now first:
  - `sessionInventoryLua` (1,394 B) and the revision script from #1;
  - `connectionAuthorityPublishLua` (493 B);
  - the contract-hole read (1,301 B) and event (1,961 B);
  - the reservation script (5,849 B);
  - `netEscrowSnapshotScript`.
- **Evidence:** inbound is 220 MB/s (123 with the storm off). The known bodies come to about 33 + 27 + 10 + 8 MB/s at today's rates.
- **Saving *(inferred, scaled per byte from CPUOPT5's 0.5–0.8 cores at 55–60 MB/s)*:** 0.6–1.0 cores now; 0.3–0.6 after #1.
- **Risk:** low.
- **Proving test:** a NOSCRIPT reply is retried with the body, never taken as a result (as CPUOPT5).
- **Owner rules:** the same scripts under the same deadlines.
- **Owner decision:** none.

#### #6 Publish the full-rewrite sets only on change (CPUOPT5 E §5.8, now five sets)

- **Change:** for `publishProviderHardExclusions` (model/egress_index.go:1166–1190) and `publishProviderCapabilitySets` (model/network_client_score_capability.go:214–265):
  - keep a digest of each member set (and proof map) beside its key;
  - when it is unchanged, only re-arm the EXPIRE;
  - when it has changed, write a staging key under the same hash tag in chunks and RENAME it into place, so readers still see one atomic publication with its ready member.
- **Evidence (§3.3, §3.7):** whole-node blocks of 115 ms every ~14.5 s on 6415, and of HSET 112–118 + SADD 94–97 ms every ~4 min on 6386 and 6399.
- **Saving:** ≈0.02 cores; the gain is latency on 3/48 of the slots.
- **Risk:** low.
- **Proving test:** an unchanged set is not rewritten; a changed one is replaced atomically, with the ready member present at every read.
- **Owner rules:** cadence and TTLs unchanged; only identical rewrites are skipped.
- **Owner decision:** none.

#### #7 GETEX in the resident poll, prune inside the MULTI (CPUOPT5 E §5.6; unshipped; with A #6)

- **Change:** as CPUOPT5: `GETEX key EX ttl` in `GetResidentForClient`, and the client-peer prunes queued in their transaction (model/peer_model.go:1370, :1938).
- **Saving *(inferred)*:** 0.3–0.6 cores. That's CPUOPT5's 0.25–0.45, scaled by the residents' 1.9× clients, not by the storm.
- **Risk:** low.
- **Proving tests:** the TTL is renewed on a read; the prune runs once per transaction.
- **Owner rules:** the resident poll (75 s against a 300 s TTL) and the heartbeat cadences stay.
- **Owner decision:** none.

#### Owner items and small ones

- **Stuck-provider recycle:** next per the owner's staged plan: the recycle on beta connect, then g1, then all (Part C). It is the fix for the EVALSHA sawtooth (4.9k → 18k/s per connect cycle, 0.5–1.0 Redis cores averaged).
- **A local copy of `{provider_hard_exclusions}`** (CPUOPT5's step 2): SMISMEMBER is up 10× (0.07–0.10 cores, one main thread). Worth it only if FindProviders2 stays at the bot-wave volume.
- **`iommu=pt` on edge-5:** at a planned reboot only. Decide on replicas and the hardware ticket first (16 primaries without replicas on a host logging machine checks).
- **The edge-6 restore:** when the next base backup runs, check edge-6's slow entries and the 8K clone's effect. The ZFS taskq parameters wait for edge-6's next planned reboot.

### 7. Considered and rejected

- **Raising `MessageBufferSize`, `ResubscribeTimeout` or `ResyncSpreadTimeout`** to quiet the storm. A larger buffer only delays an overflow while the g-block dispatchers can't keep up with 27k events/s. Longer backoff or spread loosens delivery and convergence bounds (owner rule). #1 removes the events instead. (Part A raises the buffer once the stream is quiet, A #1 step 2; see the fleet Decision 1.)
- **Cancelling an older close-listener wave when a new one starts,** as `resyncAll` does. With resubscribes every ~8.3 s and a 30 s spread, listeners late in the order would go unkicked: a delivery bound. (Part A proposes it, A #1 step 3; see the fleet Decision 1.)
- **Narrowing `notify-keyspace-events`** (dropping `g`): loses the `del` and `expired` events the peer and hop listeners need. That's a detection bound.
- **Lengthening `AuthorizationRecheckInterval` (60 s) or widening `RedisAuth`'s 2 s budget** to cut the rechecks or the "unavailable" answers: revocation and authentication bounds. Fix the storm.
- **Bot-specific limits** (user-agent blocks, ASN caps, new-account proxy limits): the owner declined these on 11-14. #1 cuts the bots' Redis cost without refusing anyone.
- **The conntrack bypass on edge-6:** declined by the owner on 11-13. Its value is connection churn, which #1 removes.
- **drop_caches on a schedule, or `vfs_cache_pressure`:** no Redis effect measured (102–104 µs per request across the 00:45Z drop). These remain the owner's memory calls, not CPU items.
- **An `io-threads` change or cpusets on edge-6** before #1 and #3: per-node load tripled under the storm, and the I/O-thread placement problem is the NIC's, not the thread count's.
- **Boost on edge-5:** the owner kept boost and C-states unchanged (11-13).
- **Redis `requirepass` or ACLs:** declined by the owner; not proposed.
- **Raising `slowlog-log-slower-than` or `latency-monitor-threshold`:** hides detection (as CPUOPT5).

### 8. Noticed, not CPU

- **edge-5's corrected memory errors reached 206** (EDAC mc1: 196 at 00:45Z, 204 at 10:25Z, 206 at 11:00Z), with 790 `[Hardware Error]` kernel lines since 00:00Z and none uncorrected (§3.6). edge-5 holds 16 primaries without replicas (§6, owner items).
- **MinIO's flat evidence directory** reaches XFS's 32 GiB data limit around 12-10 at this hour's rate, 12-18 to 12-27 at 60–75% of it; at the wall mkdir fails, and evidence PUTs with it (§3.6, #4). Free inodes fall 11.6M a day of 716M per drive.
- **Session checks answering "unavailable"** rose from 8–20 to 700–1,430/s with the storm: failures within `RedisAuth`'s 2 s budget (§5.1).
- **Node 6411 has evicted 367 clients** under `maxmemory-clients` since its start (§3.3).
- **Four slots carry 27% of all output**; their hot key is not identified, because key names were not read (§3.3).
- **edge-6's conntrack** holds 119,981 entries (58k in CPUOPT5) with the connection churn (§3.5).
- **Whole-node blocks:** 115 ms every ~14.5 s on 6415, and 94–118 ms every ~4 minutes on 6386 and 6399, from full rewrites of the sets (§3.7, #6).
