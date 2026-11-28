# CPUOPT4 — fleet CPU after CPUOPT3 (2026-11-04)

CPUOPT3 (2026-10-31) ranked the fleet's CPU costs and planned D1–D5. D1, D2 and D3 reached main on 11-01, along with the CPUOPT2 timer work. Every service now runs them:

| Service | Generation | Built from | Running since |
|---|---|---|---|
| connect | 1088460100 | server `1e94b4d78`, connect `b13c6d5a` | 11-02 19:13–19:38Z |
| api, alt | 1089433320 | server `d5cbc2a72`, connect `0dad308f` | 11-03 22:08–22:18Z |
| proxy | 1089433320 | the same | 11-03 22:54–23:26Z |
| taskworker | 1089300440 | server `9e2dcaeed`, connect `49f285f1e` | 11-03 |

- **D1** ships dark and has never been switched on.
- **D4** was deferred by the owner.
- **D5's** minimal fix moved with the dump to edge-6.

This document measures the fleet again on 11-04, checks what the shipped changes did against CPUOPT3's predictions, and ranks what is left.

Five read-only investigations, one per CPU domain, wrote the five parts:

| Part | Domain | Hosts |
|---|---|---|
| A | Services edges and connect | edge-0, edge-1, edge-3, edge-4 |
| B | Proxy hosts and the proxy service | crisp, fireside |
| C | api, alt and taskworker | the services edges, and alt on crisp and fireside |
| D | PostgreSQL | edge-2 |
| E | The Redis cluster | edge-5, edge-6 |

**Evidence:**
- Mimir;
- short perf captures, with the data files deleted on the hosts;
- entry-only bpftrace uprobes on the services edges;
- host censuses;
- `pg_stat_statements` and Redis INFO deltas between two snapshots (no counter was reset);
- Redis SLOWLOG and LATENCY;
- code read at the deployed revisions.

No production state was changed. The permission check refused one read on edge-2, and it was not retried (C §1).

**When it was measured:**
- **Live captures:** 11-04, 10:15–11:35Z, a Wednesday morning, during the conditions under "What is happening now" below.
- **Evening peaks (18–22Z):** 10-31 to 11-03, from Mimir.
- **Release coverage:** 11-03's evening is the first on this connect generation. api, alt and the proxy had not yet run an evening peak on their releases.

Section numbers inside a part are local to that part: A §5.1 is Part A's section 5.1. Raw outputs and scripts are cited as `<scratch>/…`. They stay in the analysis workspace and are not committed.

**Owner rule (binding):** "We can't sacrifice correctness of performance timings for the cpu savings." Nothing proposed here loosens a timeout, deadline, keepalive, probe, detection or revocation bound. Each part lists what it rejected under that rule.

## Summary

### What is happening now (11-04)

Three conditions dominate today's numbers. Each is cheaper to fix than anything else in this document.

1. **Out-of-balance hosted devices (B §3.2).**
   - **What happens:** at 02:45Z, contract refusals for `insufficient_balance` at the proxy stepped from 0 to 2,016/s in ten minutes. A hosted device's windows refill forever. Every expansion builds a whole window client and asks for a contract about 8 times in 30 s. Each ask is refused, and then the client retires and tries again.
   - **Proxy:** 9.3 → 39.9 cores in those ten minutes.
     - Both hosts are at 95–97% busy, with the proxy pinned at 43.5 cores.
     - GC is 47% of process CPU.
     - `create_contract` takes 1.27 s at the proxy, against 29–41 ms normally (B §3.4–3.6).
   - **Connect:** H1+ session starts doubled (about 430 → 900/s), and connect rose 20 cores at 03Z (A §5.4).
   - **PostgreSQL:** the proxy's pool acquires went from 3.0k to 18.5k/s. That is most of edge-2's 17 extra cores of work (D Summary).
   - **Redis:** node 6383's main thread is saturated on one escrow-balance slot, and the proxy sees 280–660 pool timeouts/s (E §2.3).
   - **History:** it has driven every proxy surge since 11-01 05Z, on every release, CPUOPT3's own included. Hourly proxy CPU correlates with these refusals at r = 0.83, and with transports at only 0.30.
2. **A plan flip on the payer reads (D §2.5).**
   - **What changed:** since 07:23Z, `GetOpenTransferByteCount` walks each payer's whole contract history. It is 36% of all statement execution time, about 7–8 edge-2 cores.
   - **Latency:** `GET /subscription/balance` went from about 20 ms to about 590 ms at the api, and to 2.4–3.6 s at alt (C §2.7).
   - **The payer drain:** `ReadPayerDueContracts` runs on the same plan at 6.9 s per call. The drain's page read times out at 20 s and is retried forever, closing nothing (C §2.6).
   - **Why:** the open set drained from 79M entries to 4.41M, but the five open-set partial indexes kept about 230 GB of pages. The planner prices the partial index about 100× too high. The plan has flipped at least four times since 11-01.
3. **Thursday's dump is armed (D §2.7).**
   - **The 11-03 run:** the edge-6 logical dump started at 22:35Z. It took edge-2 from 38 to 84 cores within 45 minutes and cost 144–161 excess core-hours before it was stopped at 02:05Z. The taskworker's oldest overdue task peaked at 887 s.
   - **The 11-05 06:00Z run:** it would pin the horizon for 11–12 h, a length set by st_client_key_history's TOAST. It would cost about 330–420 excess core-hours and leave roughly half of daytime demand unserved *(inferred)*. Its guards would not stop it.

**Also on record: edge-6 was down about 94 minutes on 11-03 (E §2.6).**
- **Timeline:** the host died at 02:50:15Z with no shutdown sequence and booted at 04:23:51Z.
- **Impact:** its 32 Redis nodes were down: two thirds of the slots, without replicas.
- **Before the crash:** three 16 TB archive drives were hot-added at 01:59–02:00Z. At 02:06–02:08Z the controller dropped one of them (`sde`) and re-attached it, logging "Hardware Error / Logical unit failure" on the cache sync.
- **Now:** the `archive` pool is ONLINE with no known data errors, and the reboot activated the staged `iommu=pt` on edge-6.

### The fleet

Busy CPU by host. "Now" is the 3 days to 11-04 10Z at 5-minute steps; "CPUOPT3" is its 3 days to 10-31.

| Host | Cores | CPUOPT3, mean / peak busy % | Now, mean / peak busy % | Mean busy cores, CPUOPT3 → now | Evening 18–22Z mean busy %, 11-01 / 11-02 / 11-03 |
|---|---|---|---|---|---|
| edge-0 | 96 | 37.2 / 54.5 | 51.8 / 63.1 | 35.7 → 49.7 | 52.3 / 54.7 / 52.7 |
| edge-1 | 72 | 56.6 / 75.6 | 68.0 / 79.1 | 40.7 → 49.0 | 67.3 / 71.6 / 72.2 |
| edge-2 | 96 | 47.0 / 96.6 | 55.9 / 95.8 | 45.1 → 53.7 | 47.8 / 59.3 / 50.4 |
| edge-3 | 72 | 53.9 / 76.5 | 65.1 / 79.8 | 38.8 → 46.9 | 70.4 / 69.4 / 66.6 |
| edge-4 | 72 | 54.8 / 77.3 | 68.1 / 81.8 | 39.5 → 49.0 | 73.2 / 71.2 / 70.5 |
| edge-5 | 96 | 16.5 / 29.4 | 28.3 / 72.8 | 15.8 → 27.1 | 37.9 / 30.7 / 22.1 |
| edge-6 | 128 | 15.6 / 18.8 | 21.2 / 89.3 | 20.0 → 27.2 | 20.7 / 21.8 / 22.7 |
| crisp | 24 | 43.0 / 99.9 | 62.6 / 98.8 | 10.3 → 15.0 | 55.7 / 70.6 / 74.9 |
| fireside | 24 | 46.1 / 99.9 | 62.5 / 98.8 | 11.1 → 15.0 | 54.6 / 71.1 / 76.6 |

The fleet averages about 333 busy cores, against about 257 in CPUOPT3. The work grew faster than the CPU:
- **Services edges:** 272k clients at the 11-03 evening peak, against 209k on 10-31 (+30%). Busy CPU per 1k clients fell 19%, and connect cores per 1k clients 17% (A §1).
- **api:** 1.19 cores per 1k requests, against 1.40 (C Summary).
- **PostgreSQL:** 0.98 cores per 1k commits/s at the 11-03 evening peak, against 1.12 on 10-31 (D Summary).
- **Redis:** requests rose 40% on the 3-day mean. Cost per request is unchanged at matched load: 187 µs against 188 (E Summary).
- **edge-5 and edge-6 peaks:**
  - edge-5 peaked 11-01 12–16Z.
  - edge-6 peaked at 11-03 04Z, as its 32 Redis nodes reloaded after the reboot *(inferred)*, and again at 15–16Z *(cause not determined)*.

### Where the CPU goes now

- **Services edges (A):** 187 of 312 logical CPUs busy at the 11-03 evening peak, serving 272k clients.
  - **By process:** connect 111 cores, nginx about 37 (0.13 per 1k clients, unchanged), api 15.6, taskworker 12.1, logging and observability about 13 (Loki query bursts). Kernel time is 37.8% of busy.
  - **Inside connect:**

    | Category | Now | CPUOPT3 |
    |---|---|---|
    | exchange socket trees | 18–22% | 27% |
    | H1 transports | 13–16% | 15.5% |
    | timer-heap `adjust` | 8.8–11.2% | 7.2–8.0% |
    | liveness-lease writer | 5.8–10% | 3.95% |
    | go-redis | 6.7–9.2% | 6.6–9.6% |
    | `ConnectionAnnounce` | 3.8–4.8% | 1.7% |
    | GC | about 3.5–4% | — |
- **Proxy hosts (B):** 43.5 cores today (pinned), against 9.3 before the surge.
  - In the surge, GC is 47% of process CPU.
  - The user data plane (SOCKS, HTTP, gVisor) is 3–4%, about 6 ms per SOCKS admission.
  - Without refusals, cost per transport doubles past about 7k transports per process, as CPUOPT3 B §2.5 predicted. GC is then paced by GOMEMLIMIT.
- **api, alt and taskworker (C):**
  - **api:** 16.8–19.4 cores at the evening peaks, and 18.7 at 10Z today.
    - **`/connect/control`:** 13.8k of 15.8k req/s, and only 17% of it does real work. Refusal replays are 60% (about 4 cores), PeerAudit discards 22% and real creates 16%.
    - **Shares:** JWT handling is 19–21% of api CPU and go-redis 12%.
  - **taskworker:** 12.3 cores mean and 13.7 peak. The egress probe is 85–90% of it, and x509 chain verification alone is 2.8–3.0 cores.
- **PostgreSQL (D):** 62.5 cores this morning (65%), against 45.1 at the 11-03 evening peak.
  - **By process:** backends 63%, PgBouncer 13%, the nginx stream hop 9%. Both hops are 75–80% kernel time.
  - **Overhead:** per-statement overhead is about 54% of PostgreSQL CPU (47% in CPUOPT3), and planning 12.0%.
- **Redis (E):** 31.2 cores on the 3-day mean (peak 43.5), at 172 µs per request all-in.
  - **Split:** kernel time is 51–59%, I/O threads 54%, and scripts 76% of command time.
  - **PING:** 13.5% of all requests (32.3k/s), double its 10-31 rate.

### What CPUOPT3's changes did

- **D1 (in-process delivery):** never switched on.
  - `{connect_move}control` has no fields and the code default is off. Every block shows 0 attachments since the dark release (A §4).
  - The dark release did measure what D1 needs: 72.6% of outbound transports dial their own process, exact on all four edges (A §3.7).
- **D2 (strict window handover):** it did what was predicted for the loop it targeted (B §4.1).
  - In refusal-free hours, mints fell from 34 to 6.7–8.1 per device-hour.
  - `create_contract` fell 52–62%, `provide` 73%, and FindProviders2 calls 38–47%.
  - That is about 15–20% of proxy CPU: 1.9–2.6 cores at 10-31's load, against the predicted 1.2–2.6.
  - The under-filled-window case D2 deferred is now the proxy's largest cost (condition 1 above).
- **D3 (PeerAudit):** it met its prediction per audit.
  - At the 11-02 roll, the api fell 2.3 cores against the traffic mix: 0.55–0.65 ms per audit, or 1.6–1.9 cores at the evening rate, against the predicted 1.4–1.7 (C §3).
  - Redis round trips per api request fell from 1.95 to 1.52 (E §3.1).
  - The proxy's own audits stopped. Providers and apps still send 2.6–3.9k/s until their releases.
- **D4:** deferred, not built. Re-measured, it is now worth (C #2, #3):
  - 2.6–3.4 api cores for the JWT claims cache;
  - 2.2–2.7 taskworker cores, plus 0.5–0.7 proxy, for the chain cache.
- **D5 and Part D (D §3):**
  - #13's index drops shipped.
  - D5's minimal fix shipped, and moved with the dump to edge-6.
  - The contract horizon reaper runs at 35 days, using about 0.9 backends.
  - Two of the five disk-reclaim parts are done.
- **Not landed:**
  - #6 (pgx and quic-go timers) and #8 (the lease writer), both of which grew (A §6);
  - #7 (the nginx stream hop), #9, #10 (the preflight PING), #11 (EVALSHA) and #12;
  - #15 (edge-2's kernel options);
  - #17 (the stuck-provider recycle is built and off), #18, #19 and #20;
  - B #2, #5 and #7–#10.

### Corrections to CPUOPT3

- **What drives proxy CPU.**
  - **CPUOPT3:** CPU, goroutines and memory track platform transports.
  - **Since 11-01 05Z:** hourly proxy CPU tracks `insufficient_balance` refusals (r = 0.83) much more than transports (0.30). CPUOPT3's 10-31 window had no refusals (B §1).
- **D1's locality.**
  - **CPUOPT3:** 91% of transports dial their own process, a figure extrapolated from edge-3.
  - **Now, exact on all four edges:** 72.6% (68% on edge-3). D1 is re-estimated at 10–13 cores, from 12–15 (A §3.7, §5.1).
- **D5's pin.**
  - **CPUOPT3:** `pg_dump -Fd -j4` would hold its snapshot about 4.5 h.
  - **Measured:** st_client_key_history's 715 GB of TOAST holds it 11–12 h (D §3).

## Ranked opportunities across the fleet

Savings are in cores at the evening peak unless noted. They overlap, and an item that appears in several parts is listed once.

| # | Change | Where | Est. saving | Risk |
|---|---|---|---|---|
| 1 | Balance-aware refill: while a hosted device's payer is out of balance (the sdk's existing InsufficientBalance status), send one contract probe per backoff step instead of building window clients | B #1; A §5.4; D Summary; E §2.3 | **During a surge:** 15–30 proxy cores, 14–19 connect, and most of edge-2's 17 extra, plus Redis node 6383 relieved. **In a normal peak:** 0 | low–medium |
| 2 | Turn D1 on, as approved: beta 1 day, g1 3 days, then all | A §5.1 | 10–13 cores | medium; built, with a kill switch |
| 3 | Give the planner the open-set predicate's real selectivity (expression statistics), so the payer open-sum and payer-due reads use their partial index again | D #1; C §2.7, #4 | 7–8 edge-2 cores and about 550 MB/s fewer block reads; `/subscription/balance` back to about 20 ms | low |
| 4 | Hold Thursday's dump; then dump a restored base backup or split st_client_key_history out, with stops on taskworker overdue, debit lag and CPU | D #3 | avoids about 330–420 core-hours per full run (144–161 in 3.5 h on 11-03) | low; owner decision |
| 5 | Liveness-lease writer: a dirty list, plus per-shard and per-client indexes (CPUOPT3 #8) | A §5.2 | 6–9 cores | low |
| 6 | Re-arm pgx's and quic-go's timers without resetting a heaped timer; firing times stay the same (CPUOPT3 #6) | A §5.3 | 6–9 cores (5–8 after #2) | low (pgx), low–medium (quic-go) |
| 7 | Remove the nginx stream hop in front of PgBouncer (CPUOPT3 #7) | D #2 | 5–6.5 edge-2 cores | medium (DB entry path) |
| 8 | D4: (a) a verified JWT claims cache, (b) a verified certificate-chain cache | C #2, #3; B #5 | 2.6–3.4 api cores, 2.2–2.7 taskworker, 0.7–0.9 proxy | low–medium; owner deferred |
| 9 | A higher proxy GOMEMLIMIT (6500 MiB now) | B #2 | 4–9 proxy cores in the GC regime; the knee moves from about 7k to 8–10k transports per process | low; owner (fireside RAM) |
| 10 | Turn on the built stuck-provider signal, then the recycle, staged by block (CPUOPT3 #17) | C #1; A §6.1 | 1–2.8 api cores at recent peaks, 3.7 at today's replay rate, plus 0.2–0.7 Redis *(inferred)* | medium; built |
| 11 | Move logging and observability off the services edges; docker `local` logging | A §5.5 | 3–5 cores | low |
| 12 | No preflight PING by default (CPUOPT3 §4.1, widened to the other call sites) | E §5.1 | 2–3 Redis cores, 25–32k fewer requests/s | low |
| 13 | One ContractOriginNotifications per proxy process; a version check instead of the api's 20-process broadcast | B #4; C #6 | 2–5 proxy cores *(inferred)*, about 0.5 api | medium |
| 14 | FindProviders2 page hits without the Redis read and copy; reuse a verified client-score page while its generation holds | B #3; E §5.4 | 1–3 proxy cores, 0.3–0.4 Redis, and 200–300 MB/s less Redis output | low–medium |
| 15 | Reboot edge-2 into `retbleed=stuff iommu=pt` (CPUOPT3 #15) | D #4 | 2–4 cores *(unmeasured)* | medium (restart window) |
| 16 | PgBouncer to PostgreSQL over a Unix socket (CPUOPT3 #12) | D #5 | 1.5–3 cores *(unmeasured)* | low |
| 17 | Payer drain: a page read that can stop, and a backoff for a payer whose read keeps timing out | C #4 | 1.3–1.5 connection-seconds per second, up to 1–1.3 edge-2 cores *(inferred)*; overlaps #3 | low–medium |
| 18 | Fewer round trips: per transaction at PostgreSQL; GETEX and an in-MULTI prune at Redis | D #6; E §5.5 | 1–2 edge-2 cores; 0.5–0.7 Redis | low–medium |
| 19 | EVAL → EVALSHA for the hot scripts (CPUOPT3 §4.3) | E §5.2 | 0.5–0.8 Redis cores, 55–60 MB/s less inbound | low |
| 20 | Terminate H1 in connect, with no nginx hop (CPUOPT3 #19) | A §5.6 | 8–12 cores | high |
| 21 | Multiplexed links for the remaining cross-process exchange, after #2 (CPUOPT3 #20) | A §5.7 | 5–8 cores | high |

**Smaller items,** each under about 1 core (details in the parts):
- a lag-guest gate that reads one claim (C #5);
- the GOGC and GOMAXPROCS A/Bs (A §5.8, C #7);
- one control-plane strategy per probe process (C #8);
- plan invalidation on the queue tables (D #7);
- rebuilding the drained partial indexes (D #8; mostly vacuum IO and plan stability);
- JIT off (D #9);
- GetTransferStats (D #10);
- `{provider_hard_exclusions}` published on change (E §5.6);
- per-slot statistics (E §5.7);
- small code items (A §5.10, C #9).

**Totals:**
- **Code and configuration items** (#2, #3, #5–#7, #10–#14, #16–#19) need no product decision; #3 and #7 need database change windows. Together they come to roughly 40–60 cores in a normal evening peak, about 12–18% of the fleet's 333 mean busy cores. They overlap and are approximate.
- **Owner decisions:** D4 and a higher GOMEMLIMIT add about 10–16 cores.
- **In a surge like today's,** #1 is worth more than all of these together.
- **#4** avoids 330–420 core-hours per dump.

### Themes

1. **One loop costs the most, in four tiers at once.** Out-of-balance hosted devices refill forever (B §3.2). The proxy pays most, and connect, PostgreSQL and one Redis node pay for the same loop (A §5.4, D §2.1, E §2.3). The fix is one change in the refill path.
2. **The largest steady-state savings are built and switched off.** D1 (10–13 cores) has never been enabled, and the stuck-provider recycle (1–3.7 api cores) is off on every block (A §4, C #1).
3. **Plans on drained partial indexes are unstable.** The open set drained from 79M to 4.4M entries, but the five partial indexes kept about 230 GB of pages, so the planner misprices them after transfer_contract vacuums (D §2.5). The flip shows up as balance latency at the api and alt, and as a futile retry loop in the taskworker (C §2.6, §2.7).
4. **CPUOPT3's connect items grew.** The timer-heap scans (#6) and the liveness-lease writer (#8) are each larger than in CPUOPT3, with 47% more providers per process (A §3.3, §3.4).
5. **Redis cost per request is flat, but the request count is not.** PING preflights are 13.5% of requests, and scripts 76% of command time (E §2).
6. **Long snapshots remain the largest swing on the primary.** The 11-03 edge-6 dump pinned edge-2 about 8× faster than the 10-29 dump did (D §2.7).
7. **Hosts at the ceiling cost latency, not only CPU.** On the proxy hosts, contract creation went from 29–41 ms to 1.27 s, and alt's CPU per unit of work rose about 50% (B §3.6, C §2.7).

## Decisions for the owner

1. **Today's out-of-balance hosted payer (B §3.2).** A top-up, or pausing that payer's hosted devices, ends today's surge. Then approve the balance-aware refill (#1) for implementation.
2. **Thursday 11-05 06:00Z dump (D #3).** Hold it (recommended).
   - Choose the redesign: dump a restored base backup, or split st_client_key_history out, with stops on taskworker overdue, debit lag and CPU.
   - Without a decision by Thursday 04:00Z, the run is disabled, as the handoff plans.
3. **D1's rollout (A §5.1).** It was approved on 10-31 and is built with its kill switch, but has never started. Start beta for 1 day, then g1 for 3 days, then all.
4. **The stuck-provider signal, then the recycle (C #1),** staged by block.
5. **D4 (C #2, #3).** It was deferred over storage. Both caches are in-process memory: the JWT cache is capped at 64 MB per process, the chain cache at 256 entries. Together they are now worth about 5.5–7 cores.
6. **The proxy's GOMEMLIMIT (B #2).** It is set service-wide in vault `services.yml`, and a value for crisp alone needs a warpctl change. It depends on fireside's RAM.
7. **Change windows on the database path:** the payer-read statistics migration (D #1) and the nginx stream hop (D #2).
8. **Host posture:**
   - `kernel.numa_balancing=0` on edge-6, a runtime sysctl (E §5.3);
   - the edge-2 reboot into the staged kernel options (D #4);
   - `iommu=pt` on edge-5;
   - the other Redis items in E §5.8: conntrack bypass, NUMA cpusets, io-threads (not advised) and SRSO.
9. **Proxy capacity (B §3.8):** a third proxy host, if surges recur before #1 ships.
10. **edge-6's 11-03 outage (E §2.6):** was the 94-minute outage during the archive-drive work planned? Redis there has no replicas.

## Validating a change

CPUOPT3's rules still hold:
- **Normalize by the work:** services per 1k clients, the proxy per transport and device, api and Redis per request, PostgreSQL per statement or commit.
- **Compare like with like:** the same hours across days, over at least three days. A/B on one block where a change can be staged.
- **Hold the timing bounds as acceptance criteria,** not just the CPU.
- **Measure CPU from Mimir and perf,** not from a single short profile.

Two more from this round:
- **Exclude surge hours, or normalize by refusals.** While a payer is out of balance, the proxy, connect, edge-2 and one Redis node all carry the refill loop. B §3.3 gives the cost per refusal.
- **Check the payer-read plan before comparing edge-2 hours.** A flipped plan adds about 8 cores (D §2.5).

## Part A — Services edges and connect

Domain: by-us-fmt-5-edge-0/1/3/4 (312 logical CPUs) and the connect service.
Everything here was measured read-only on 11-04 between 10:20Z and 11:35Z (a
Wednesday morning), or read from Mimir. Code is cited at the deployed connect
revision (server `1e94b4d78`, connect library `b13c6d5a`) unless it says
otherwise. Raw outputs are in `<scratch>/a/`.

### 1. Summary

**What runs.** Connect is `2026.11.2-outerwerld+1088460100`: server `1e94b4d78`,
connect library `b13c6d5a`, sdk `826df3eb`, go1.27.3. It rolled 11-02
19:13–19:38Z and was 39 h old at the captures. It was not re-released on 11-03.
It contains D1, D2, D3 and the CPUOPT2 timer merge.

**Matched evenings, 18–22Z** (10-31 is CPUOPT3's P2; 11-03 is this
generation at 22.5–26.5 h):
- Clients +30% (209k → 272k). Exchange frames flat (1.79M/s). H1 session starts
  +26% (626 → 790/s).
- Busy CPUs 178 → 187 (+5%). Busy per 1k clients 0.851 → 0.687 (−19%).
- Connect 102.8 → 111.0 cores. Per 1k clients 0.491 → 0.409 (−17%). Per
  exchange frame 57.5 → 62.2 µs (+8%). The client mix shifted toward clients
  that carry little traffic.
- At a matched workload (clients, frames and session starts), connect is 7–11%
  cheaper than P2 (§3.5).

**D1 has never been on.** The `{connect_move}control` hash has no fields, and
the code default `LocalTransportDefault` is `off`. Since the dark release
(11-02 00:04Z) every block shows 0 attachments, 0 local frames and 0 detaches.
The dark release did measure what D1 needs:
- transports carry 70% of exchange data frames (68% of all frames at the peak);
- 206k transport loops (72.6% of outbound transports) dial their own process,
  exact by socket matching on all four hosts.

D1 is now worth 10–13 cores (CPUOPT3: 12–15).

**Inside connect** (four 40 s captures, 7.1–8.5 cores each, against CPUOPT3):
- timer-heap `adjust` self 8.8–11.2% (was 7.2–8.0%);
- liveness-lease writer 5.8–10.0% (was 3.95%);
- exchange socket trees 18.4–21.7% (was 27%);
- H1 transports 13.2–16.1% (15.5%), go-redis 6.7–9.2% (6.6–9.6%);
- `ConnectionAnnounce.run` 3.8–4.8% (1.7%);
- `TakeClientCloseRequests` 0.09–0.14% (2.53%);
- GC about 3.5–4% averaged over cycles; kernel leaf 34–38% (44%).

**CPUOPT3 items #6, #8, #9, #17, #19 and #20:** none landed, in server origin/main
(to 11-04 08:17Z) or connect origin/main (to 11-03 20:32Z).
- #6 grew: 252–335 full heap scans/s per process (was 223–259). pgx and quic-go
  are 90–93% of the short re-arms.
- #8 doubled: providers per process +47%, plus more session churn.
- #9's main cost went away for another reason, though merge drops rose 6.5×.
- #17's sawtooth persists: `/connect/control` 5.9k/s at the restart, 13.5k/s
  38 h later.

**A condition to account for.** Since 11-04 03Z (the proxy condition, Part B
§3.2), H1+ session starts doubled against the same hours on 11-03 (380–440/s
→ 860–920/s), and connect rose 20 cores at 03Z. Hourly fits price an accepted
session at 29–68 core-ms (41 for P2).

**Around connect:**
- kernel time 37.8% of busy (39.0%);
- nginx about 37 cores this morning, 0.13 cores per 1k clients (unchanged);
- api 15.6 and taskworker 12.1 cores at the 11-03 evening (8.4% and 6.5% of busy);
- logging and observability about 13 cores (Loki query bursts).

**Ranked opportunities.** Savings are services-edge cores at the evening peak
(11-03: 187 busy, connect 111). They overlap; don't add them all.

| # | Change | Est. saving | Risk |
|---|---|---|---|
| 1 | Turn D1 on, as approved: beta 1 day, g1 3 days, then all (§5.1) | 10–13 cores | medium; built, with kill switch |
| 2 | Liveness-lease writer: dirty list, per-shard and per-client indexes (CPUOPT3 #8) (§5.2) | 6–9 | low |
| 3 | Re-arm pgx's slow-write timer and quic-go's loop timer without resetting a heaped timer (CPUOPT3 #6) (§5.3) | 6–9 (5–8 after D1) | low; low–medium for quic-go |
| 4 | Find and stop the 11-04 H1+ churn at its source (Part B) (§5.4) | 0 at 11-03's churn; 14–19 under 11-04's | depends on the cause |
| 5 | Move Loki, Mimir and Grafana off the services edges; docker `local` logging (§5.5) | 3–5 | low |
| 6 | Terminate H1 in connect, without the nginx hop (CPUOPT3 #19) (§5.6) | 8–12 | high |
| 7 | Multiplexed links for the remaining cross-process exchange, after #1 (CPUOPT3 #20) (§5.7) | 5–8 | high |
| 8 | GOGC 100 → 150 on one block (§5.8) | 1–2 | low–medium (memory) |
| 9 | Key-event merge buffer and resync (CPUOPT3 #9) (§5.9) | ≤1, plus Redis and PostgreSQL reads | low–medium |
| 10 | Small items (§5.10) | ≤1–2 each | low |

Items 1–3 together are about 21–30 cores, 11–16% of services busy CPU at the
evening peak. They share 1–2 cores of timer-heap work.

### 2. Method

**When and load.** Captures ran on 11-04, 10:20–11:35Z. The services edges were
at 199–208 busy CPUs (64–67%) with 283–291k clients. That is Wednesday morning
load, but under the 11-04 churn condition it is busier than the 11-03 evening
(187 busy CPUs). Profiles are normalized by the work. Every comparison with
CPUOPT3 uses matched hours from Mimir.

**Connect generations** (from `process_start_time_seconds{job="connect"}`,
`urnetwork_build_info` and `urnetwork_source_info`):

| Generation | Server / connect | Started | Evening 18–22Z on it |
|---|---|---|---|
| 1085953670 (P2) | 37ec4788b / ea79e1f1 | 10-31 02:01–02:36Z, rolling | 10-31, 16–20 h old |
| 1087195620 | ae10ce1ef / — | 11-01 08:13–08:15Z, all blocks at once | 11-01, 10–14 h; a proxy restart at 19:3x–19:5x dropped residents by 157k |
| 1087754130 | 53c9c767a (D1 dark) / — | 11-02 00:04–00:35Z, rolling | 11-02 until 19:13Z |
| 1088460100 (now) | 1e94b4d78 / b13c6d5a | 11-02 19:13–19:38Z, rolling | 11-02 after 19:38Z (fresh); 11-03, 22.5–26.5 h |

- The connect library commit comes from `factury/HANDOFF-1102-2.md`. The binary
  can't confirm it: the server builds with `replace => ../connect`. The server
  revision is confirmed by `urnetwork_source_info` and the container names.
- **The 11-03 evening is the cleanest match for P2's.** Connect and proxy did not
  restart inside it (proxy restarted 06:1x–08:0x and from 22:5x). api, alt and
  taskworker did restart at 18:0x–19:2x. The api release of 11-03 (22:0x–23:2x)
  comes after it.
- **CPUOPT3 merges** (D1 `f7ddb96ef`, D2 `a556dafc7` `863aa0d9f` `c6615cd17`
  `ee22592cc`, D3 `c0c310a1b` `e66f57589`, timer churn `c2cc384a4`) are all
  ancestors of `1e94b4d78`. `53c9c767a` lacks only `c6615cd17`. `ae10ce1ef` has
  none.

**Code and history checked:** server and connect origin/main from 10-29 to the
last local fetch (server 11-04 08:17Z, connect 11-03 20:32Z; `git fetch` was
not possible from this Mac), and the pgx v5.10.0 and quic-go v0.61.0 module
sources. They are pinned unchanged in both go.mod files.

**Captures** (all read-only; data files deleted on the hosts; each script checked
that no perf or bpftrace process and no file was left):
- **Host census, all four edges, 10:35Z:** per-process CPU from `/proc` over 5 s,
  `/proc/stat` over 10 s, TCP counters, sockstat, conntrack.
- **Per-second connect CPU on edge-3** for 40 s, to see GC cycles.
- **perf, one connect process, 40 s at 199 Hz with call graphs,** four captures:
  edge-3 g4 at 10:43:40Z (59.9k samples, 7.12 cores) and 10:45:56Z (65.8k, 7.67,
  with a GC cycle); edge-0 g1 at 11:22:49Z (69.8k, 8.46); edge-0 g2 at 11:29:37Z
  (68.9k, 8.19).
- **perf host-wide on edge-3,** 15 s at 49 Hz, at 11:00:39Z and 11:13:18Z.
- **bpftrace, entry-only uprobes, as CPUOPT3:** `runtime.(*timers).adjust` (5 s)
  and `runtime.(*timer).modify` (1 s), on edge-3 g4 (11:02–11:04Z) and edge-1 g2
  (11:05Z). Offsets are from go1.27.3 `runtime/time.go`, the running toolchain;
  sanity counters confirmed the reads.
- **Exact exchange locality on all four edges** (`socket_census3.py`). An outbound
  socket's local tuple equals the peer tuple of the socket that accepted it, since
  DNAT rewrites only the destination. The match also requires the ports to agree
  through the DNAT map, because Linux reuses an ephemeral port for different
  destinations. Each host's outbound same-process count equals its inbound one
  within 3%; on edge-1 they are equal.
- **Logging processes and Loki's own counters** on edge-0 and edge-4, 20 s.
- **One read of the D1 control,** on edge-5: `HEXISTS`, `HGET`, `HKEYS` of
  `{connect_move}control`.

**Caveats and corrections:**
- **Wednesday morning against evening peaks.** Per-client figures are confounded
  by the client mix, and this morning by the churn condition.
- **Two socket censuses were wrong and are superseded.** The first used
  `ss -tnp` (159 s of one core on edge-3); the second matched without ports.
  Ephemeral-port reuse made both report 98% same-host exchange.
- **Unscraped processes.** GC CPU is not exported, so it is estimated from
  per-second process samples and perf. nginx is stripped (31% `[unknown]`
  leaves), so its relay work can't be split from api proxying. Loki and Mimir
  have no CPU series in Mimir.
- **edge-3 load.** It read 70–88% busy in 5 s windows (load 85–98). No capture
  ran there after 11:13Z.
- **Nothing was refused.** One failure was mine: a Perl error in a probe script
  exited 255, which ansible reports as "unreachable". It was fixed and rerun.

### 3. Where the CPU goes now

#### 3.1 Hosts

Matched evenings, means over 18–22Z (Mimir):

| | 10-31 (P2) | 11-01 | 11-02 | 11-03 |
|---|---|---|---|---|
| Clients (H1 sessions) | 209.4k | 251.5k | 286.5k | 271.6k |
| Busy CPUs (% of 312) | 178.1 (57.1%) | 188.3 | 191.4 | 186.7 (59.8%) |
| Kernel share of busy | 39.0% | 38.0% | 37.7% | 37.8% |
| Busy per 1k clients | 0.851 | 0.749 | 0.668 | 0.687 |
| Connect cores | 102.8 | 112.6 | 118.1 | 111.0 |
| Connect per 1k clients | 0.491 | 0.448 | 0.412 | 0.409 |
| Exchange frames/s, both directions | 1.788M | 1.914M | 1.754M | 1.786M |
| Connect µs per exchange frame | 57.5 | 58.8 | 67.3 | 62.2 |
| H1 sessions accepted/s | 626 | 607 | 704 | 790 |
| api / taskworker cores | 18.5 / 12.3 | 16.6 / 11.8 | 15.3 / 13.2 | 15.6 / 12.1 |

Per host, 10-31 → 11-03 evening:

| Host | CPUs | Busy % | Kernel % of busy | Clients | Busy per 1k | Connect per 1k |
|---|---|---|---|---|---|---|
| edge-0 | 96 | 42.7 → 48.8 | 38.2 → 36.8 | 55.5k → 84.0k | 0.739 → 0.558 | 0.466 → 0.356 |
| edge-1 | 72 | 65.0 → 67.2 | 40.4 → 39.8 | 43.5k → 62.7k | 1.077 → 0.772 | 0.644 → 0.488 |
| edge-3 | 72 | 60.9 → 61.2 | 38.3 → 37.4 | 53.6k → 56.1k | 0.817 → 0.785 | 0.440 → 0.439 |
| edge-4 | 72 | 64.1 → 65.3 | 38.8 → 37.2 | 56.4k → 68.6k | 0.819 → 0.685 | 0.447 → 0.373 |

- **Mornings, same generation, 09–11Z:** 11-03 (14 h old) against 11-04 (38 h old,
  churn condition):
  - clients 263.8k → 286.9k;
  - busy 161.9 → 207.2 CPUs (51.9% → 66.4%);
  - connect 98.9 → 120.0 cores (0.375 → 0.418 per 1k clients; 58.5 → 66.1 µs per
    frame);
  - H1 starts about 435 → 900/s.

  Now edge-0 is 52.9% busy and edge-1, edge-3 and edge-4 are 71–74%.
- **edge-0 still runs cool.** It holds 21–22k clients per g-process against 15–18k
  elsewhere, and is 49–53% busy on 96 CPUs.
- **Per-CPU spread over 10 s:**
  - busy median/p90: edge-0 69/73%, edge-1 82/88%, edge-4 83/86%;
  - softirq median 3.6–9.4%, p90 16.4–20.5%.

#### 3.2 By process

Census 10:35Z, cores (`/proc` over 5 s; connect also as Mimir 10-minute rates,
since a 5 s window can catch a GC cycle):

| Host | connect (Mimir) | nginx | api | taskworker | logging and observability |
|---|---|---|---|---|---|
| edge-0 | 38.3 (33.8) | 7.8 | 2.4 | 1.4 | 6.0; 20 s mean 2.9 (loki 0.8, dockerd 0.5, fluent-bit 0.4, journald 0.4) |
| edge-1 | 33.2 (31.3) | 8.6 | 3.4 | 5.7 | 3.8 |
| edge-3 | 33.9 (28.4) | 10.4 | 5.9 | 5.0 | 3.0 |
| edge-4 | 25.4 (27.7) | 10.6 | 6.8 | 4.3 | 5.8; 20 s mean 3.7 (dockerd 0.8, mimir 0.7, loki 0.6, journald 0.5) |
| **Sum** | **130.9 (121.2)** | **37.4** | **18.4** | **16.4** | **≈13 as means** |

edge-3 host-wide perf, share of non-idle samples (two samples):

| Process group | Now | CPUOPT3 |
|---|---|---|
| connect | 47.9 / 48.7% | 55.9% |
| nginx | 17.5 / 17.1% (54–55% kernel) | 17.8% |
| api | 9.2 / 11.8% | 14.4% |
| taskworker | 6.3 / 6.4% | 5.4% |
| loki, mimir, fluent-bit, grafana | 3.2 / 2.8% | 1.3% |
| dockerd, containerd | 2.5 / 2.4% | 2.9% |
| journald, rsyslog | 1.05 / 0.81% | 1.0% |
| IRQ and softirq on idle CPUs | 10.8 / 8.3% | 12.2% |

- **Share of busy at the 11-03 evening:** connect 59.4% (CPUOPT3: 59%), api 8.4%,
  taskworker 6.5%, other Go jobs 1.1%. The rest, about 46 CPUs, is nginx (about
  31–35), logging and observability (about 11–13) and kernel work outside
  processes.
- **nginx scaled with clients:** 0.130 cores per 1k clients now against 0.127 in
  CPUOPT3.
- **nginx's own split** (`perf_host_edge3_2.txt`): syscall frames 43%,
  `tcp_sendmsg` 17%, epoll 8%, TLS record crypto 4%, TLS handshakes 1%. It holds
  62–86k H1 relays per host, one per H1 session (port 7009 → 7598 on edge-3 g4
  alone holds 15.6k).
- **Loki is bursty:** 0.55–0.81 cores mean, 1.8–2.3 max over 20 s, 3.6 in one 5 s
  window. It serves 8–11 queries/s plus 1–2 `query_range`/s, and ingests
  0.8–1.4 MB/s per host.

#### 3.3 Inside connect

Shares of one g-process's samples. Columns: edge-3 g4 capture 1, capture 2 (with
a GC cycle), edge-0 g1, edge-0 g2.

| Category | e3 g4 #1 | e3 g4 #2 | e0 g1 | e0 g2 | CPUOPT3 (e3 g4) |
|---|---|---|---|---|---|
| Process cores / samples | 7.12 / 59.9k | 7.67 / 65.8k | 8.46 / 69.8k | 8.19 / 68.9k | 6.9–7.3 |
| Timer heap: `check` incl / `adjust` self / sift self | 14.2 / 10.4 / 1.05 | 12.2 / 8.8 / 0.91 | 15.3 / 11.2 / 1.17 | 15.5 / 11.2 / 1.23 | 10.5–11.3 / 7.2–8.0 / 0.86 |
| `findRunnable` incl, `selectgo` incl, `lock2`+`unlock2` self | 24.7, 8.7, 2.3 | 21.5, 8.3, 1.9 | 23.9, 8.0, 2.2 | 24.4, 8.2, 2.3 | 24.5–26.2, 9.5–10.0, 2.9 |
| Syscalls (`Syscall6` incl) | 23.9 | 21.2 | 23.4 | 23.7 | 30.5 |
| Exchange socket trees (`ExchangeConnection.Run` + `handleExchangeConnection`) | 10.6 + 11.1 | 10.1 + 10.7 | 9.3 + 9.1 | 9.9 + 9.4 | 13.2 + 13.75 |
| H1 transports (`ConnectHandler.Connect`) | 13.7 | 13.2 | 14.5 | 16.1 | 15.5 |
| go-redis incl | 7.6 | 6.7 | 9.2 | 7.3 | 6.6–9.6 |
| pgx incl | 2.2 | 1.7 | 2.5 | 2.3 | 2.1 |
| Liveness-lease writer | 7.8 | 5.8 | 9.7 | 10.0 | 3.95 |
| `ConnectionAnnounce.run` | 4.2 | 3.8 | 4.8 | 4.8 | 1.7 |
| `AuthorizationLease.runChecks` / `residentController` | 1.0 / 1.5 | 0.9 / 1.4 | 1.1 / 1.0 | 1.1 / 0.9 | 2.3 / 2.6–3.0 |
| `ReadContractHoleLease` incl | 1.3 | 1.2 | 3.2 | 1.5 | 1.22 (Redis part) |
| `TakeClientCloseRequests` | 0.14 | 0.09 | 0.13 | 0.12 | 2.53 |
| Key-event subscriber, all | 0.43 | 0.38 | 0.54 | 0.38 | — |
| `exchangeDeadline.refresh` incl | 1.22 | 1.22 | 1.05 | 1.11 | 1.6 |
| quic-go (H3) | 1.2 | 1.1 | 1.0 | 0.9 | 0.3 |
| GC (mark worker, assists, sweep) | 1.3 | 9.1 | 0 | 0 | none caught |
| `mallocgc` / `makechan` incl | 1.7 / 0.64 | 2.0 / 0.70 | 1.7 / 0.59 | 1.6 / 0.54 | — / 0.48 |
| Kernel leaf, all | 37.5 | 34.4 | 35.0 | 36.2 | 44 |
| `process_backlog` (local delivery) incl | 4.8 | 4.6 | 4.9 | 5.4 | — |
| conntrack find self / AppArmor / stuffing thunk | 1.08 / 1.9 / 1.57 | 1.01 / 1.6 / 1.31 | 1.16 / 1.9 / 1.50 | 1.11 / 1.8 / 1.65 | 1.1 / 0.5 (`aa_inet_msg_perm`) / 1.6 |
| D1 local transport | 0 | 0 | 0 | 0 | — |

- **Liveness-lease writer.** `written.func1` 2.75–6.79% incl and `write.func2`
  1.9–2.9% self. Both are full iterations of the writer's connection map under its
  lock (§5.2). The writer is the largest single application item, and it is larger
  on edge-0, whose processes hold more connections.
- **`ConnectionAnnounce.run`** (transport_announce.go:421) has four parts:
  - the per-connection sync read `GetProvideKeyChanges` (`run.func9`, :726);
  - the egress lease feed (`run.func5.1`, :564);
  - per-session `ConnectNetworkClientWithIpFamily` (:508);
  - per-session `DisconnectNetworkClient` (:529–558).

  Announce connections per g-process grew from 13.5k to 18.0k.
- **Session setup and teardown frames are about 4–5% of connect** (edge-0
  captures): auth 0.5, authorization lease 0.65, admission and nomination
  0.6–0.8, exchange dial 0.6–0.7, announce profile reads 0.3–0.4, contract close
  and settlement 0.8–0.9, accept and goroutine creation 0.35, plus the client row
  writes in `ConnectionAnnounce` (about 0.8% on edge-3 g4).
- **Not session work:** `refreshResidentRegistration` (1.3%) runs per resident
  every `ExchangeResidentTtl/4` = 75 s (resident.go:1307–1319).
  `newResidentContractManager.func1` (1.5%) is the packet-path `readContract`
  closure (resident_contract_manager.go:56).
- **Top `selectgo` callers:** `sendPooledReceive` 1.7, the H1 writer 0.9, the
  exchange writer 0.7, `sendPooledMessage` 0.6, the inbound transport writer 0.5,
  `SendSequence.Run` 0.5 (CPUOPT3: 2.1, 1.2, 1.0, 0.8, 0.5).

##### 3.3.1 Redis callers

From `perf_conn_edge3_g4_1.txt` and `perf_conn_edge0_g1.txt`, % of connect:

| Caller | e3 g4 | e0 g1 | CPUOPT3 |
|---|---|---|---|
| `ReadContractHoleLease` | 0.52 | 1.49 | 1.22 |
| `GetResidentForClient` | 0.89 | 1.02 | 0.63 |
| `redisWithClient` (callback with no app frame) | 0.64 | 1.23 | — |
| go-redis goroutines with no app caller | 1.95 | 1.78 | 1.1 |
| `GetProvideModes` | 0.51 | 0.51 | 0.36 |
| `session.init` | 0.41 | 0.52 | 0.86 |
| egress verify (update, read, membership, forward) | 0.86 | 1.04 | 0.34 |
| `PublishConnectionAuthority` | 0.33 | 0.34 | 0.71 |
| `RecordClientReliabilityStatsRange` | 0.49 | 0.26 | 0.34 |
| `TakeClientCloseRequests` | 0.06 | 0.06 | 2.53 |

#### 3.4 What drives the timer-heap cost

`bpftrace` (`bpf_timers_edge3_g4.txt`, `bpf_modify2s_edge3_g4.txt`,
`bpf_timers_edge1_g2.txt`):

| Process | `adjust` calls in 5 s | Full scans | Forced (zombie) | Heap per P | Timers visited | CPUOPT3 |
|---|---|---|---|---|---|---|
| edge-3 g4 | 65.8k | 1,673 (335/s) | 0 | 16–64k, mean 32k | 10.7M/s | 259/s, 8.3M/s |
| edge-1 g2 | 66.5k | 1,259 (252/s) | 0 | 16–64k, mean 33k | 8.3M/s | 223/s, 6.6M/s |

- **The heap is about 530k timers per process** (16 Ps). Scans are 16–21 per P
  per second. Zombies are under 1% of every heap.
- **Modify census, 1 s:**
  - edge-3 g4: 200–204k modifies/s, 170–178k on heaped timers, 1.6–1.8k that
    lowered a P's `minWhenModified`, 520–614/s of them under 250 ms;
  - edge-1 g2: 196k, 160k, 2.0k and 729/s.
- **Callers of the lowering modifies** (sampled stacks):

  | Delta | Caller | edge-3 g4 | edge-1 g2 | CPUOPT3 (short only) |
  |---|---|---|---|---|
  | < 250 ms | quic-go `(*Conn).maybeResetTimer` | 278–295/s (48–54%) | 273/s (37%) | 24–28% |
  | < 250 ms | pgx `flushWithPotentialWriteReadDeadlock` (`slowWriteTimer.Reset(15ms)`) | 189–269/s (36–44%) | 411/s (56%) | 59–64% |
  | < 250 ms | go-redis socket deadlines | 50–53/s (8–10%) | 44/s (6%) | 10–11% |
  | < 250 ms | connect `SendSequence` | 0 | 1/s | 2–2.5% |
  | 0.25–2 s | go-redis deadlines from short contexts | 272/s | 434/s | — |
  | 0.25–2 s | H1 writer timer reset (`ConnectHandler.Connect.func14.7`) | 85/s | 63/s | — |
  | 0.25–2 s | connect `sequenceDeadlineTimer.armBy` | 67/s | 53/s | — |

- **pgx and quic-go are 90–93% of the short triggers.** `SendSequence` left the
  short bucket when the connect library's 10-30 timer work shipped in this
  generation. P2 ran connect `ea79e1f1`, from 10-28.
- **Cost:** `adjust` + sift self is 10–12% of connect, about 12–13 cores at the
  evening peak (CPUOPT3: 8.4%, about 8.7 cores).

#### 3.5 Work and its cost

| | P2, 10-31 18–22Z | Now, 11-03 18–22Z |
|---|---|---|
| Exchange ends per client | 4.74 | 3.66 |
| Goroutines per client | 70.1 | 55.9 |
| Connect heap | 679 GB | 730 GB |
| Exchange frames per client per second | 8.5 | 6.6 |
| Mean H1 session life (active ÷ starts) | 334 s | 344 s |
| New exchange connections/s (handshake frames ÷ 4) | ~1.65k | ~1.25k |

**Load models**, hourly least squares per generation (`fit_sessions.txt`,
`fit_crosspredict.txt`):
- P2: connect = 0.158 × clients(k) + 0.0284 × frames(k/s) + 0.041 × H1
  starts(/s) − 6.4; rms 0.88 cores, against 2.94 without the session term.
- 53c9c767a: 0.123, 0.0269, 0.068, −7.4; rms 0.74.
- Now: 0.260, 0.0150, 0.029, −7.6; rms 2.9.
- **Cross-prediction:** P2's model predicts 119.7 cores for the 11-03 evening, and
  110.9 were observed (−7%). The current model predicts 92.3 for the 10-31
  evening, and 102.8 were observed (P2 +11%). Each extrapolates in clients, so
  read this as "about 7–11% cheaper", not exact.
- **Uptime:** an age term adds −0.04 cores/h for this generation over 39 h, so
  there is still no CPU growth with connect uptime (CPUOPT3: +0.02). Heap and
  resident callback workers do grow: 0.96M → 2.18M workers.
- **The 11-04 03Z step** (`churn_hourly.txt`):
  - H1 starts 566 → 904/s, clients +15k, frames +101k/s;
  - connect 90.3 → 109.9 cores.

  With P2's client and frame coefficients that leaves 14.4 cores for +338
  starts/s, about 43 core-ms per start. The jump is all H1+ (499 → 832/s;
  WebSocket steady at 60–70/s). Active H1+ sessions stayed at about 250k, so the
  mean H1+ session fell from about 10 min (11-03 09–10Z) to about 5 min.
- **What a session costs.** The profiles show about 4–5% of connect in setup and
  teardown frames (§3.3), plus the lease writer's wake passes. On edge-0 g1, at
  about 66 starts/s, those frames are about 6 core-ms per start, or about 13 if
  half the writer's work counts as wake passes. The rest of the fitted 30–40 ms is
  likely the lifetime cost of short-lived residents: allowance reads, the 75 s
  registration refresh, announce rounds.

#### 3.6 Kernel and network

Per host now (10 s, 10:35Z), against CPUOPT3's 10-31 evening:

| | edge-0 | edge-1 | edge-3 | edge-4 | CPUOPT3 range |
|---|---|---|---|---|---|
| TCP keepalive probes/s | 668 | 965 | 799 | 889 | 680–950 |
| TcpOutSegs/s | 787k | 649k | 745k | 749k | 588–764k |
| Pure ACKs/s | 83k | 76k | 97k | 97k | 61–88k |
| Retransmits/s | 1.8k | 2.0k | 2.5k | 2.3k | 1.0–2.0k |
| Context switches/s | 716k | 475k | 490k | 781k | 748–941k |
| Interrupts/s | 867k | 752k | 875k | 927k | 737–882k |
| conntrack entries | 484k | 441k | 565k | 594k | 338–429k |
| TCP inuse / timewait | 596k / 25k | 502k / 36k | 536k / 87k | 567k / 88k | 390–485k / 22–39k |

- **edge-3 host-wide kernel paths,** % of non-idle samples (two samples; CPUOPT3
  in brackets): `handle_softirqs` 12.7 (14.6), `net_rx_action` 11.3 (13.0),
  `tcp_sendmsg` 10.2 (12.5), NIC NAPI 7.4 (7.9), `nf_hook_slow` 4.2 (4.8),
  `process_backlog` 3.3 (4.4), `do_epoll_wait` 3.1 (3.9), futex 2.8 (3.5),
  `nf_conntrack_in` 2.2 (2.3), `nanosleep` 1.3 (1.8).
- `retbleed=stuff iommu=pt` are still on edge-3. Uptime is 5 days; the reboots
  were on 10-30.
- **Kernel share:** 37.8% of busy at the 11-03 evening (39.0% on 10-31). Connect's
  kernel leaf share fell from 44% to 34–38% with the smaller exchange share.

#### 3.7 Exchange locality (exact, all four edges)

Outbound exchange connections of connect, from `socket_census3*.txt` (10:58–11:25Z):

| Host | Same process | Other connect on the host | Other hosts | Outbound transports (Mimir) | Same process ÷ transports |
|---|---|---|---|---|---|
| edge-0 | 68.8k | 18.7k | ~64k | 84.1k | 82% |
| edge-1 | 51.8k | 14.0k | ~57k | 61.9k | 84% |
| edge-3 | 42.6k | 18.6k | ~53k | 62.7k | 68% |
| edge-4 | 42.4k | 23.3k | ~63k | 74.5k | 57% |
| **All** | **205.6k** | **74.6k** | **~237k** | **283.2k** | **72.6%** |

- **"Other hosts" includes the "remote other" class.** Other edges number some
  blocks' ports differently, so this host's port set can't name them as exchange.
- **Edge-3 against CPUOPT3:** outbound exchange is now 37% same process, 16% same
  host other process, 47% other hosts. CPUOPT3: 43%, 10%, 47%.
- **Inbound:** each g-process's H1 port holds 15–16k nginx relays, and each of its
  11 exchange ports holds about 1.3k remote, 0.95k same-process and 0.4k
  other-process connections.
- **Frames by `op`** (`frames_by_op.txt`): transports carry 594–599k data
  frames/s per direction against 242–260k for forwards, 70% of data frames. At the
  11-03 19Z peak, transports were 607k of 897k frame crossings/s (67.7%).

#### 3.8 Runtime shape and GC

- **Per g-process:** 6.4–8.8 cores, 15–22k clients, 0.90–1.18M goroutines, heap
  43–74 GB. GOMAXPROCS is 16.
- **GC:** GOGC 100 with no memory limit, 0.48 cycles/min per g-process (also at
  P2), allocation 207 MB/s.
  - The per-second series shows a mark phase of about 8 s at +4–6 cores, about
    35 core-seconds per cycle. Capture 2 caught one cycle at 9.1% of a 40 s
    window, about 28 core-seconds.
  - That is 0.24–0.28 cores per process, 3.5–4% of connect, about 4–4.5 cores at
    the evening peak (CPUOPT3 estimated about 3%).
- **The service-runaway probe still trips** on every g-process: ≥0.9M goroutines
  against its 500k ceiling.

### 4. What CPUOPT3's shipped changes did

- **D1, in-process delivery: shipped dark, never enabled.** The code is in
  `53c9c767a` (from 11-02 00:04Z) and `1e94b4d78` (from 11-02 19:13Z).
  - The code ceiling is on in the service (connect/run.go:64), the default is off
    (resident_local_transport.go:264), and the control has no fields.
  - `exchange_local_transport_enabled` has been 0 on every block for 3 days, and
    attachments, local frames and detaches have all been 0.
  - The dark code costs nothing visible: no samples, and one control read per
    process every 10 s.
  - The predicted 12–15 cores, −1.4M goroutines and −400k sockets could not
    happen. What the dark release measured is in §3.7: the transport frame share
    CPUOPT3 inferred (≥2/3) is confirmed. The same-process share of transports is
    72.6%, not the 91% extrapolated from edge-3, but transports grew, so the loop
    count (206k) matches CPUOPT3's 200k.
  - The plan's next step, beta for 1 day, was not taken. Confidence: high (direct
    counters).
- **D2, window handover, and D3, PeerAudit:** no direct connect effect is expected,
  and none shows.
  - D3 answers 3.1k/s audit-only packs before authentication on api (Part C).
  - D2 acts on proxy window clients (Part B). Connect sees those as H1+ sessions.
    The 11-04 03Z doubling of H1+ starts came after the 11-03 22:5x proxy release
    and with the proxy condition. Part A can't separate the causes.
- **The connect library's 10-30 timer work** (lazy sequence deadline timers,
  timer reuse, unchanged-Reset skips) is in this generation and was not in P2's
  `ea79e1f1`. `SendSequence` left the short re-arm bucket (2–2.5% → 0%). The
  server-side CPUOPT2 timer commits were already in P2's line as cherry-picks.
- **Net, connect at a matched workload: −7 to −11% against P2.** Medium
  confidence: the models extrapolate in clients.
  - **What fell:** exchange trees 27% → 20% of connect, about −12% per frame
    crossing; `TakeClientCloseRequests` −2.4 points (close requests 1,352/s →
    774/s); provider-intent shadow sampling (`4f4581377`); the library timer work.
  - **What grew:** the lease writer (+2–6 points) and the timer heap (+2–3 points).
  - **Confounders:** the client mix shifted (providing connections per g-process
    5.6k → 8.2k; clients +30% with flat frames), and api, alt and taskworker
    restarted inside the 11-03 evening.

### 5. Ranked opportunities

#### 5.1 Turn D1 on, as approved

- **The change.** Follow D1's approved rollout:
  `HSET {connect_move}control localTransport beta` for 1 day, then `g1` for 3
  matched days against g2–g4, then `all`. This is an owner's operation, not a
  code change.
- **Code at the deployed revision.** `connect/resident_local_transport.go`:
  `newExchangeLocalTransport` :309, settings :237–269. `connect/run.go:64` holds
  the ceiling. `model/connect_move_control.go` holds `localTransport`.
- **Evidence:**
  - 206k same-process transport loops, 72.6% of 283k transports (§3.7);
  - transports are 67.7% of frame crossings at the peak;
  - exchange trees are 18.4–21.7% of connect;
  - `process_backlog` is 4.6–5.4%.
- **Saving at the evening peak:**
  - frames on same-process transports are about 49% of 897k crossings/s;
    0.49 × 22 cores of exchange trees = 10.9 cores, less about 0.6 for the two
    hand-offs (1.4 µs per frame in D1's benchmark);
  - −15% of the timer heap (about 77k timers per g-process) gives about 1.5–2
    cores of `adjust`, less if §5.3 lands first;
  - −1.8M goroutines gives about 0.3 cores of GC.
  - **Total 10–13 cores, about 6% of services busy CPU.** Also −412k sockets and
    −206k conntrack entries.
- **Risk:** medium, as assessed in D1: lifecycle races, pool ownership, less
  buffering before a stall. The plan has 31 tests, 26 mutants, a paced kill switch
  and a per-block A/B.
- **Why no timing bound moves:** D1's design table. A batch still gets 15 s and a
  stall detaches at 15 s. Detection of a dead peer becomes immediate, never later.
  Resident TTL, reconnect pacing, H1/H3 deadlines and forwards are untouched.
- **Validation** (D1's plan, adjusted to the measured shares; g1 against g2–g4,
  matched hours, 3 days):
  - `exchange_active_connections{op="transport"}` on g1: −60–80% (not −85–95%,
    since 73% are same-process);
  - connect cores per 1k clients and per frame on g1: −8–10%;
  - no rise in `h1_sessions_closed_total{reason}`, nominations per client or
    `receive_queue_dropped`;
  - `exchange_local_detaches_total{write_timeout}` per 1k transports no higher
    than control-block redials;
  - a rerun of `bpf_timers.sh` shows heaps per P −10–15%.

#### 5.2 Liveness-lease writer: incremental bookkeeping (CPUOPT3 #8, grown)

- **The change.** In `providerLivenessLeaseWriter` keep:
  - a dirty list, appended in `begin` and in `afterRead`'s state changes;
  - per-client connection sets, and a per-client count of providing connections,
    kept in `afterRead` and `end`;
  - per-shard client sets.

  `write(all=false)` then touches only dirty connections and their shards.
  `written` touches only the connections of the clients in that shard write.
  `oldest` uses the same structures or the full pass. The full pass each
  `refreshInterval` plus jitter is unchanged.
- **Code:** `connect/transport_announce_liveness_lease.go`:
  - `write` :314, which rebuilds `providingClientIds` over all connections, then
    walks all connections again;
  - `written` :388, which walks all connections once per shard written;
  - `begin` :182, `afterRead` :202, `end` :245, `oldest` :255, `run` :272.
- **Evidence:**
  - 5.8–10.0% of connect in four captures (3.95% in CPUOPT3). `written.func1`
    2.75–6.79% and `write.func2` 1.9–2.9% are the map loops; map iteration and
    access self is 2.4–3.8%.
  - Connections per g-process are 18.0k (13.5k at P2) and providing ones 8.2k
    (5.6k). The cost grows as connections × wakes. Wakes follow session starts,
    which is why edge-0's larger processes pay 9.7–10%.
  - The same lock serializes every connection's `afterRead`.
- **Saving:** about 8.5 cores at the evening peak (edge-0 at 9.9%, the others at
  about 6.8%), about 85% removable: **6–9 cores** (CPUOPT3: about 3).
- **Risk:** low; only bookkeeping changes.
- **Why no timing bound moves:** the same writes, at the same moments:
  - wakes stay immediate;
  - the full pass keeps its cadence;
  - vouch times stay each shard write's start;
  - withholding and release rules are the same.
- **Validation:**
  - a randomized equivalence test of old and new bookkeeping over
    begin/afterRead/end/write sequences, with the same shard writes and outcomes;
  - perf share below 1%;
  - `liveness_lease_writes_total{outcome}` per providing connection unchanged;
  - lease ages unchanged.

#### 5.3 Re-arm pgx's and quic-go's timers without resetting a heaped timer (CPUOPT3 #6, grown)

- **The change** (as CPUOPT3 §6.2):
  - **pgx:** a fresh `time.AfterFunc(15ms)` per flush instead of `Reset` on a
    stopped timer that is still heaped; `Stop` that instance on exit.
  - **quic-go:** in `maybeResetTimer`, skip an unchanged deadline; replace the
    timer when the deadline moves earlier; leave the earlier timer armed when it
    moves later.
  - Both go in a small fork, carried as `replace` lines in server's go.mod, and
    are offered upstream.
- **Code:**
  - pgx v5.10.0 `pgconn/pgconn.go`: `enterPotentialWriteReadDeadlock` :2107
    (`slowWriteTimer.Reset(15 * time.Millisecond)` :2114),
    `exitPotentialWriteReadDeadlock` :2120, `flushWithPotentialWriteReadDeadlock`
    :2133;
  - quic-go v0.61.0 `connection.go`: `maybeResetTimer` :861, Resets at :884, :895
    and :902.
- **Evidence:**
  - scans 252–335/s per process (+13–29% against CPUOPT3);
  - `adjust` + sift self 10–12% of connect;
  - pgx and quic-go are 90–93% of the sub-250 ms lowering re-arms (§3.4).
- **Saving:** without them, the remaining lowerings are go-redis (mostly
  0.25–2 s), the H1 writer reset and `SendSequence.armBy`. Scans would fall from
  16–21 to about 4–6 per P per second (−65–80%). On about 12–13 cores of
  `adjust`, that is **6–9 cores**, or 5–8 if D1 shrinks the heap first (CPUOPT3:
  3–6).
- **Risk:** low for pgx, low–medium for quic-go (fork patches).
- **Why no timing bound moves:** every timer fires when it fires today. pgx's
  15 ms detector still fires at 15 ms. quic-go's timer never fires later than its
  deadline; at worst it wakes early once and recomputes. Only the arming changes.
- **Validation:**
  - `bpf_timers.sh`: scans per process ≤ 80/s, and no pgx or quic-go stacks below
    250 ms;
  - perf `adjust` self below 3%;
  - pgx's own tests plus a deadlock test (a large write with a slow reader);
  - quic-go's tests plus an ACK-delay and pacing timing test.

#### 5.4 Stop the 11-04 H1+ churn at its source (Part B)

- **The change.** Find why H1+ session starts doubled at 03Z, then remove the
  excess where it is made. The proxy's admission surge and window-client
  lifetime are Part B's.
- **Connect's own share is smaller:**
  - setup and teardown frames are about 4–5%, plus the lease writer's wake passes,
    which §5.2 fixes;
  - the client row writes in `ConnectionAnnounce` (`ConnectNetworkClientWithIpFamily`
    :508, `DisconnectNetworkClient` :529–558) are about 0.8%.
- **Evidence** (§3.5):
  - at the 03Z step, +338 starts/s brought +19.6 connect cores, 14.4 after
    clients and frames, about 43 core-ms per start;
  - the hourly fits give 29–68 core-ms per start;
  - mean H1+ session about 5 min against about 10 at the same hours on 11-03;
  - in the evenings, starts per active session are the same as at P2 (0.29–0.30%
    per second).
- **Saving:** 0 at 11-03's churn. Under 11-04's, about 450 excess starts/s ×
  30–43 core-ms = **14–19 connect cores**, plus nginx TLS handshakes and api auth
  work.
- **Risk:** depends on the cause.
- **Why no timing bound moves:** the lever is fewer needless sessions, not holding
  dead ones. No idle, read or close bound changes.
- **Validation:** H1+ starts per active session back to 11-03's same-hour value,
  and connect cores within the §3.5 model's residual.

#### 5.5 Logging and observability off the services edges (CPUOPT3 §6.9)

- **The change.** Move Loki, Mimir and Grafana to edge-6, as CPUOPT2 §3.10 and
  CPUOPT3 §6.9 proposed. Use the docker `local` driver or journald only.
- **Code:** xops host and service definitions (not read for this part).
- **Evidence:**
  - about 13 cores across the four edges, against about 9 in CPUOPT3;
  - per host: Loki 0.55–0.81 cores mean with query bursts to 2–3.6, dockerd
    0.5–0.8, journald 0.4–0.5, fluent-bit 0.3–0.4, mimir 0.2–0.7, rsyslog
    0.2–0.3, containerd-shim 0.2–0.3;
  - `json-file` logging on all four (CPUOPT3).
- **Saving:** **3–5 cores**, mostly Loki's queries and the docker log path.
- **Risk:** low.
- **Why no timing bound moves:** no service path changes.
- **Validation:** per-host CPU of these processes, and log completeness against
  the same hour.

#### 5.6 Terminate H1 in connect, with no nginx hop (CPUOPT3 #19)

- **Not landed:** every H1/H1+ session is an nginx relay (ext 7009 → 7598 on
  edge-3 g4).
- **Evidence:**
  - nginx about 37 cores this morning, 17–18% of edge-3 samples, 54–55% kernel;
  - 62–86k relays per host, each 2 extra sockets and a conntrack entry;
  - TLS crypto is only 4% of nginx;
  - the relay read and write on connect's side is about 3–4% of connect
    (`httpUpgradeConn.Read` 1.6% plus the batch writes).
- **Saving:** **8–12 cores,** as CPUOPT3. Unchanged per client.
- **Risk:** high: ports, rate limits, deploy port rotation, and TLS moving into Go.
- **Why no timing bound moves:** the same H1 deadlines and pings, in connect.
- **Validation:** one block on dedicated addresses; nginx plus connect cores per
  1k H1 sessions.

#### 5.7 Multiplexed links after D1 (CPUOPT3 #20)

- **Not landed.** After D1, cross-process exchange is about 51% of crossings:
  forwards 32%, cross-process transports 19%. Each remaining socket carries about
  1.2–2.2 frames/s.
- **Saving:** **5–8 cores,** as CPUOPT3.
- **Risk:** high: a protocol version, mixed-version rollout, head-of-line blocking.
- **Why no timing bound moves:** each link keeps the exchange read, write and ping
  bounds, and each stream keeps a write-timeout bound through credit.

#### 5.8 GOGC 100 → 150 on one block

- **The change.** Set GOGC to 150 on one block (connect service settings).
- **Evidence:**
  - GC is 3.5–4% of connect, about 4–4.5 cores at the peak, at 0.48 cycles/min
    per g-process (§3.8);
  - heap in use is 43–74 GB per g-process;
  - edge-1 has 438 GB available and the other hosts 600–700 GB.
- **Saving:** about −33% of cycles, **1–1.5 cores** (about 2 at GOGC 200).
- **Risk:** low–medium. The cost is about +25 GB of heap per g-process at 150.
- **Why no timing bound moves:** GC pacing only. Mark phases get rarer, not longer.
- **Validation:** cycles/min, `/proc` per-second excess per cycle, RSS headroom,
  and p99 of the H1 write batch.

#### 5.9 Key-event merge buffer and resync (CPUOPT3 #9)

- **Not landed.** The merge buffer is still `MessageBufferSize` 1024
  (server/redis.go:706, :760), and a resubscribe still runs `resyncAll` and
  `resyncCloseListeners` (connect/key_event_subscriber.go:257, :224; subscribe
  loop :450).
- **Evidence:**
  - merge drops 7.5–7.8/min per g-process (1.2 at P2), resubscribes 2.1–2.5/min
    (1.1), listener resyncs 10.8–12.9k/min (4.1k);
  - but `TakeClientCloseRequests` fell to 0.06% Redis and 0.09–0.14% incl, with
    close requests down from 1,352/s to 774/s;
  - the fan-out (subscriber, session rechecks, hop and close listeners) is now
    0.8–1.2% of connect.
- **Saving:** **≤1 core,** plus Redis and the PostgreSQL session rechecks each
  resubscribe triggers (Part D).
- **Risk:** low–medium.
- **Why no timing bound moves:** a true overflow still resyncs. A bigger buffer
  only absorbs bursts that today force a resync.

#### 5.10 Small items

- **Allowance and registration reads.** `ReadContractHoleLease` is 1.2–3.2% of
  connect and `refreshResidentRegistration` 1.3% (a Redis read per resident every
  75 s). Part E's contract-hole read without the preflight PING (CPUOPT3 #10) also
  trims connect's client work. No commit for it was found. **0.5–1 core.**
- **Same-host exchange:** dial the internal port directly with NOTRACK.
  conntrack find is 1.0–1.2% self and `nf_hook_slow` 4.2% of the host. Same-host
  exchange is about 70k connections per host. **1–2 cores.**
- **`encodeUuid` through `fmt`:** 0.34–0.40% of connect. Hex encoding is exact.
- **`windowPacingService.measureEvidenceWithLock`:** 0.55% self (connect library;
  present at P2 too). Not studied.
- **Rings on demand** (CPUOPT3 §6.8): not landed. `defaultExchangeBufferSize` is
  4096 (resident.go:432), and `makechan` is 0.54–0.70%. About 1 core plus heap.

### 6. CPUOPT3 predictions checked

| CPUOPT3 item | Predicted | Status | Observed now |
|---|---|---|---|
| #1 / D1, in-process delivery | 12–15 cores; −1.4M goroutines, −400k sockets, −200k conntrack | built, dark, never on | No effect. The transport frame share (≥2/3) is confirmed at 68–70%. Loops 206k (≈200k predicted); 72.6% of transports, not 91%. Re-estimated 10–13 |
| #6, pgx/quic-go timers | 3–6 cores; scans 250 → 20–50 per process | not landed | Scans 252–335/s; `adjust` self 8.8–11.2%; pgx+quic-go 90–93% of short triggers. Re-estimated 6–9 |
| #8, lease writer | ~3 cores | not landed | 5.8–10.0% of connect, about 8.5 cores. Re-estimated 6–9 |
| #9, key-event resync | 2–3 cores | not landed | Drops ×6.5, but close-take cost −95% for other reasons. ≤1 core left |
| #17, rejection creep | 0 → 6–8 api cores per generation | not landed | Control 5.9k → 13.5k/s over 38 h. Replays 0.5k → 8.0k/s; rejections 9.6k/s (inactive_destination 73%, missing_companion_origin 19%, provide_secret_unavailable 5%, insufficient_balance 3%). About 0.7 api cores per 1k req/s (§6.1) |
| #19, H1 without nginx | 8–12 cores | not landed | nginx 0.13 cores per 1k clients, unchanged |
| #20, multiplexed links | 5–8 after #1 | not landed | #1 is not on |
| §5: no connect CPU tied to uptime | +0.02 cores/h | holds | −0.04 cores/h over 39 h; heap and callback workers grow |
| §3.3: full deadlines cost 1.6% | — | holds | `exchangeDeadline.refresh` 1.05–1.22% |
| §6.8 rings, §6.9 logging, §6.10 `encodeUuid` | ~1, 2–4, ~0.45% | not landed | `makechan` 0.54–0.70%; logging about 13 cores; `encodeUuid` 0.34–0.40% |
| GC | "est. ~3%" | — | 3.5–4% averaged over cycles |

#### 6.1 #17 in more detail

- **Over this generation** (`control_hourly.txt`), POST `/connect/control` went
  from 5.9k/s (11-02 20Z, just after the connect restart) to 13.5k/s (11-04 08Z;
  13.9k/s at 10:2xZ). The 11-03 22:xxZ api release did not reset it, so the creep
  follows connect's uptime, as CPUOPT3 found.
- **Its cost** is about 0.7 api cores per 1k req/s (13.4 → 18.7 api cores), plus
  nginx. That is about 5 api cores over this 38 h generation.
- **The source fixes are not live:**
  - the connect library's client-side `1a7843e7` ("stop asking for companion
    contracts an exit keeps refusing", 11-03) is not in any app;
  - the server's stuck-provider recycle (`7e4c66e31` and follow-ups) is built
    but off (`stuck_provider_recycle_enabled` = 0);
  - refusal holds are about 3/s, not a creep fix.

### 7. Considered and rejected under the owner rule

1. **Lengthening pgx's 15 ms slow-write detector, or dropping it.** It changes when
   pgx starts its background reader. §5.3 keeps 15 ms and changes only the arming.
2. **Coarsening go-redis socket deadlines** (rounding to 1 s, or ignoring short
   contexts) to stop lowering `minWhenModified`. It loosens callers' bounds.
3. **Longer exchange read or write deadlines, ping intervals or keepalive** (30 s,
   15 s, 10 s, 11 s / 1 s × 4), to cut timer work.
4. **CPUOPT2's lazy deadline re-arm.** It was reverted for correctness, and is not
   re-proposed.
5. **Delaying or batching liveness-lease wakes,** or a slower full pass. Lease
   writes and provider discovery would lag. §5.2 keeps both cadences.
6. **A slower `ConnectionAnnounce` sync round** (`GetProvideKeyChanges`). Provide-mode
   changes would land later.
7. **Dropping the key-event corrective poll, or widening its resync spread.**
   Convergence would be slower.
8. **Holding H1+ sessions open past read errors or idle bounds** to cut churn.
   Detection and revocation would slow. §5.4 removes the causes instead.
9. **Raising `ExchangeResidentTtl`** to thin out the 75 s registration refresh. It
   loosens the resident liveness bound.
10. **Relaxing the service-runaway probe's 500k-goroutine ceiling.** It is a
    detection bound, and the monitor's domain.

### 8. Raw outputs

All files are in `<scratch>/a/`:

- **Mimir:** `gens_connect.txt`, `gens_other_jobs.txt`, `versions_connect.txt`,
  `evenings_18_22.txt`, `mornings_09_11.txt`, `hourly_connect.txt`,
  `hosts_evening_18_22.txt`, `hosts_morning_09_11.txt`, `client_mix.txt`,
  `per_process_now.txt`, `fit_generations.txt`, `fit_sessions.txt`,
  `fit_crosspredict.txt`, `churn_hourly.txt`, `h1_by_carrier.txt`,
  `h1_evenings.txt`, `frames_by_op.txt`, `ends_by_host_op.txt`,
  `keyevent_rates.txt`, `control_hourly.txt`.
- **D1 control:** `d1_control_field.txt`.
- **Host census:** `census_all.txt`, `proc_series_edge3.txt`, `layout_edge3.txt`,
  `layout2_edge3.txt`, `nat_jumps_edge3.txt`, `logging_probe.txt`.
- **Socket locality:** `socket_census3_edge3.txt`, `socket_census3_edge0_edge4.txt`,
  `socket_census3_edge1.txt`, `socket_census4_edge3_ports.txt`. Superseded, kept
  for the record: `socket_census_edge3.txt` and `socket_census2_edge3.txt`.
- **perf:** `perf_conn_edge3_g4_1.txt`, `perf_conn_edge3_g4_2.txt`,
  `perf_conn_edge0_g1.txt`, `perf_conn_edge0_g2.txt`, `perf_host_edge3.txt`,
  `perf_host_edge3_2.txt`.
- **bpftrace:** `bpf_timers_edge3_g4.txt`, `bpf_modify2s_edge3_g4.txt`,
  `bpf_timers_edge1_g2.txt`.
- **Scripts:** `mq.py`, `gens.py`, `evenings.py`, `hourly.py`, `hosts_evening.py`,
  `fit.py`, `fit3.py`, `fit4.py`, `host_census.py`, `busy.py`, `proc_series.py`,
  `socket_census*.py`, `nat_jumps.sh`, `layout*.sh`, `logging_probe.py`,
  `perf_conn.sh` (with `perf_conn.head`, `perf_sum.py`, `perf_conn.foot`),
  `perf_host.sh` (with `perf_host_sum.py`), `bpf_timers.sh`, `run_script.sh`.

## Part B — Proxy hosts and the proxy service

CPUOPT4 section, 2026-11-04. Read-only measurement of fireside and crisp and their proxy processes, with alt's share of those hosts. Live captures ran at Wednesday-morning load (10:30–10:50Z). Evening peaks come from Mimir. Everything is normalized by the work.

### 1. Summary

- **The saturation is not demand for SOCKS and not a release regression.** It is hosted devices whose payer is out of balance.
  - At 02:45Z on 11-04, contract refusals for `insufficient_balance` at the proxy stepped from 0 to 2,016/s in ten minutes.
  - Proxy CPU went from 9.3 to 39.9 cores in the same ten minutes. SOCKS admissions did not move (56 → 62/s).
  - Live devices then rose from 27.9k to 55.0k by 03:40Z, and windows with no selectable exit from 2.6k to 45.6k by 04:05Z.
  - Since 04Z the refusals have run at 1.8–2.0k/s. Both hosts are at 95–97% busy, and the proxy is pinned at 21.5–22.1 cores per host (43.5 fleet).
- **The same mechanism has driven every proxy surge since 11-01 05Z, on every release.** That includes CPUOPT3's own release a8461e236 (1,909 refusals/s at 11-01 05Z). It was absent from CPUOPT3's 10-31 window (0/s). Over 62 hourly samples since D2 shipped, proxy cores correlate with refusals at r = 0.83 and with transports at only 0.30.
- **How a refusal turns into CPU.** A hosted device's windows refill forever (server/proxy/proxy_device.go:2085–2103). Each expansion builds a whole window client: FindProviders2, a mint, Provide, a TLS dial, and an evaluation ping. The ping's contract wait then asks for a contract about 8 times in 30 s (connect transfer.go:10515–10630). Every ask is refused. The client is retired, and the window tries again.
  - Each refusal costs about 10–15 ms of proxy CPU all-in: 15.0 mcores per refusal/s (r² 0.94) on 5d4b3293a's unsaturated surge, 9.7–15.2 at this morning's onset, 9.9 in the hourly fit.
  - Candidates failing at contract run at 170–330/s, against about 0 in quiet hours.
- **GC turns the extra work into 47% of the CPU.** In perf (crisp g5, 30 s at 99 Hz), GC assists are 31.1% and background mark 16.1%. CPUOPT3 measured 6–9% mark and 0.25% assists.
  - Each process runs 12 GC cycles a minute, against 1.5 on 10-31. It allocates 138 MB/s, against 25–29 MB/s in quiet hours.
  - GC is paced by GOMEMLIMIT (6500 MiB). The live heap is about 3.5 GB against a goal of about 4.2 GB, so each cycle has only about 0.7 GB of headroom.
  - CPUOPT3 §2.5 predicted this regime. Without any refusals it starts at about 7k transports per process: cost per transport goes from 0.11 to 0.25 mcores, and GC from 1.5–2 to 10–12 cycles a minute (11-01 06–09Z, a8461e236).
- **Contention at the ceiling costs latency.**
  - Every proxy process waits 0.53–0.68 s on the run queue per second it runs, and PSI cpu-some is 46–50%.
  - `create_contract` at the proxy takes 1.27 s, against 29–41 ms on 10-31 and in quiet hours, and 5.7 ms at the api.
  - The proxy refuses 185–264 contract creations/s and about 930 mints/s at its admission bounds, and sees 478 Redis pool timeouts/s.
  - alt on the same hosts waits 2.2 s per second it runs. Its mean HTTP latency is 33 ms at 09Z, against 15.5 ms at 02Z.
- **SOCKS is cheap.** The whole user data plane (SOCKS, HTTP, gVisor, the device) is about 3–4% of process CPU, about 6 ms per SOCKS admission. At 118–178 admissions/s that is about 1 core of the 43.5.
- **D2 did what CPUOPT3 predicted for the loop it targeted.**
  - In refusal-free hours, mints fell from 34 to 6.7–8.1 per device-hour.
  - `create_contract` fell 52–62% and `provide` 73%. FindProviders2 calls fell 38–47% and TLS opens per host about 50%.
  - Cost per transport fell 22–37%, with D3 and the load mix included. D2's share is about 15–20%: 1.9–2.6 cores at 10-31's load, against the predicted 1.2–2.6.
  - The under-filled-window case that D2 deferred (owner question 4) is now the proxy's largest cost.
- **D3 removed the proxy's 2.2k/s PeerAudit control calls.** Control-path JWT checks fell about 90% in quiet hours.
- **Top opportunities** (cores at a surge-affected evening peak; they overlap):
  1. A balance-aware refill for hosted windows: 15–30 cores while a payer is out of balance.
  2. A higher proxy GOMEMLIMIT: 4–9 cores in the GC regime, and the knee moves from about 7k to 8–10k transports per process (owner, RAM).
  3. FindProviders2 page hits without the Redis read and copy: 1–3 cores, plus about 300 MB/s of Redis output (inferred).
  4. One ContractOriginNotifications per process: 2–5 cores in the GC regime (inferred).
  5. The deferred D4 caches: 1–2.5 cores.

### 2. Method

**Revisions.** Proxy processes started 11-03 22:54–23:26Z. Every proxy release since CPUOPT3:

| Version | Server | Connect / sdk | Started |
|---|---|---|---|
| 2026.10.31-outerwerld+1086377170 | a8461e236 | 89b15920 / fe265dfc | 10-31 09:33–09:56Z |
| 2026.11.1-outerwerld+1087515290 | f19323066 | not recorded | 11-01 g1 16:52, g2–g3 17:46–17:50, g4–g10 19:31–19:56Z |
| 2026.11.2-outerwerld+1088460100 | 1e94b4d78 | b13c6d5a / 826df3eb | 11-02 19:45–20:18Z |
| 2026.11.2-outerwerld+1088828550 | 5d4b3293a | f5484467e / 360d12ae1 | 11-03 06:10–08:00Z |
| 2026.11.3-outerwerld+1089433320 | d5cbc2a72 | 0dad308f8 / 4b3cddc9d | 11-03 22:54–23:26Z |

- **Sources:** `urnetwork_source_info` and `urnetwork_build_info` in Mimir; `temp/resume-1102/release/build-r2.sh`, `multi-build.out`; factury HANDOFF-1102-2.
- **Shared by all five:** proxy repo 1575ad25c, Go 1.27.3, GOMAXPROCS 4, GOMEMLIMIT 6500 MiB, a 10 GiB container cap, host networking.
- **The 11-01 release already carried D2's behavior.** Its tree sets `DisableStrictWindowHandover: !proxyWindowStrictHandoverFromConfig()` with the default on. Its counters show handovers and strict-full skips from the first hour. The counters-only baseline in D2's plan therefore never ran in production.

**Evening peaks (18–22Z) against the deploys.**
- 10-31: wholly a8461e236.
- 11-01: a8461e236 → f19323066 during 16:52–19:56Z.
- 11-02: f19323066 → 1e94b4d78 during 19:45–20:18Z.
- 11-03: wholly 5d4b3293a. Its surge's tail overlapped the 22:54Z roll; refusals were already down from 686/s at 21:30Z to 59/s at 22:55Z.
- No evening after 10-31 was free of refusals (§3.3).

**Mimir.** Data from 10-30 19Z to 11-04 10:30Z (`<scratch>/b/mimir/`):
- hourly trailing means for host CPU modes, process CPU, Go runtime, proxy gauges, admissions, sessions, events, D2 window counters, control frames, JWT checks, contract rejections, FindProviders2 and the page cache;
- a 5-minute per-host dataset (`ds5m.tsv`), used for the window table (§3.3) and the fits;
- fits that drop rows with fewer than 10 processes per host or a process younger than 1 h, which removes rollout double counting.

**Host captures (read-only).**

| Capture | When | What |
|---|---|---|
| recon | crisp 10:34Z | load, PSI, containers, ps |
| perf | crisp g5 10:36Z | cpu-clock, 99 Hz, 30 s (31.3 s), call graphs, 6,441 samples, process at 2.18 cores, host 96.5% busy. Summarized on the host; the data directory was deleted and no perf process was left. |
| /proc during the perf | crisp g5 | schedstat run and wait, context switches, io |
| recon2 | both hosts 10:49Z | PSI, MemAvailable, 10 s CPU split, per-container schedstat over 10 s, cgroup memory.current/max/events, cpu.max |
| env | crisp | GOMEMLIMIT, GOMAXPROCS and docker limits of one proxy and alt, printed alone |

There was no bpftrace and no second perf: on a host above 90% busy, the limit was one perf capture of at most 30 s at 99 Hz.

**Code** was read at the deployed revisions with `git show` (server d5cbc2a72, connect 0dad308f8, sdk 4b3cddc9d). `git fetch` failed here (no SSH key), so "origin/main" means each shared checkout's last fetch: server 403fa4b39 (11-04 08:17Z), connect 0dad308f, xops a074338.

### 3. Where the CPU goes now

#### 3.1 Hosts

| Host | CPUs | Busy now, 11-04 04–10Z | proxy / alt / softirq cores | Busy 11-03 04–10Z | Busy 10-31 18:25–19:35Z | MemAvailable now | PSI cpu-some now |
|---|---|---|---|---|---|---|---|
| crisp | 24 (8P + 16E) | 95–97% | 21.5–22.1 / 0.39 / 1.6 | ~69% | ~33% | 49.3 GiB | 46–50% |
| fireside | 24 (8P + 16E) | 95–97% | 21.5–22.1 / 0.38 / 1.6 | ~69% | ~33% | 17.8 GiB | 46–49% |

- Load average is 36–38 on 24 CPUs.
- In 10 s at 10:49Z: user 81–82%, system 5–6%, softirq 6%.
- No container has a CPU quota (`cpu.max max`), and no cgroup has memory events.
- Proxy RSS is 5.83–6.05 GiB per process, at 92–95% of GOMEMLIMIT.
- About 2.1–2.4 cores per host go to alt, softirq and the rest. That leaves the proxy at most about 21.7 cores per host, which it is using.

#### 3.2 The saturation: what starts it and what it is

At 5-minute steps, from `<scratch>/b/mimir/5m-1103-2130-1104-0500.tsv`:

| UTC 11-04 | Proxy cores | Busy | Devices | Transports | Refusals/s | Failed-contract candidates/s | Deferred passes/s | Windows below min / no selectable | SOCKS adm/s |
|---|---|---|---|---|---|---|---|---|---|
| 02:40 | 9.3 | 26% | 27.9k | 121k | 0 | 0 | 150 | 30 / 2.6k | 56 |
| 02:45 | 18.4 | 45% | 32.0k | 136k | 938 | 80 | 225 | 1.8k / 7.6k | 56 |
| 02:50 | 39.9 | 89% | 38.7k | 143k | 2,016 | 305 | 769 | 6.8k / 16.5k | 62 |
| 03:00 | 43.2 | 96% | 45.1k | 143k | 1,988 | 310 | 1,965 | 10.0k / 27.9k | 53 |
| 03:40 | 44.8 | 99% | 55.0k | 125k | 1,349 | 112 | 3,800 | 23.1k / 42.5k | 71 |
| 05:00 | 43.4 | 96% | 46.9k | 143k | 2,132 | 230 | 3,200 | 23.4k / 34.0k | 67 |

- **Refusals come first; devices follow for an hour.** The new devices hold few transports: about 2.7 per device fleet-wide now, against 4.3–4.4 before. Their windows sit below minimum, with no selectable exit. Device closes stay at about 1/s, so this is a standing set of devices, not churn.
- **The other surges match.**
  - 11-01 05Z (a8461e236): 1,909 refusals/s, 31.9 cores.
  - 11-02 11Z – 11-03 09Z: 1.2–3.7k/s, 18–42 cores.
  - 11-03 18–23Z: 0.75–1.86k/s, 24–38 cores.
  - Each ends when the refusals stop: 11-03 09–10Z went from 20.9 to 10.8 cores, and the 11-03 evening's tail from 12.6 cores at 22:50Z to 9.2 at 00:15Z.
- **They are payer refusals.** `contract_rejections_total{cause="insufficient_balance"}` is a refusal that found the payer short (controller/connect_controller.go:276–300).
  - At 09Z the proxy saw 1,996/s of them, plus 369/s `deadline`, 18/s `canceled` and 387/s of `contract_failures_total{cause="other"}`.
  - The quiet hours and 10-31 had none of them. Outside rollouts the other causes stayed at or below about 10/s (`inactive_destination`).
  - This part neither identifies nor counts the networks concerned.
- **How one refusal becomes CPU.**
  1. `ProxyDevice.Active()` deliberately ignores window readiness: "the multi-window must keep refilling forever under the same hosted device" (proxy_device.go:2085–2103).
  2. Every opener calls `UpdateActivity` (:449), so a client that keeps connecting keeps the device inside its 45-minute hold.
  3. A below-minimum window re-expands on the discovery backoff, 10 s rising to 2 min (connect ip_remote_multi_client_discovery_retry.go:42–106).
  4. Each expansion (`expand`, ip_remote_multi_client.go:14373) builds a complete client: FindProviders2, a mint, Provide, a TLS dial, and an evaluation ping.
  5. The ping's send sequence then waits for a contract. It asks at 1, 2, 4, 5, 5… s with ±50% jitter, until `CreateContractTimeout` (30 s) runs out (transfer.go:868–880, 10530–10630). That is about 8 refusals per candidate.
  6. The candidate is counted `failed_contract`: "the local contract could not be acquired before any provider contact" (ip_remote_multi_client_window_client_stats.go:496–508). It is retired, and the window tries again.
  7. The sdk already records the device's `InsufficientBalance` contract status (sdk device_local.go:3195–3260), but nothing in the refill reads it.
  - Measured: about 2.4k `create_contract` calls/s for 170–330 failed candidates/s, and FindProviders2 at 381/s (94/s on 10-31).
- **What it costs, and that it is not the release.**

  | Measure | 5d4b3293a (11-03) | d5cbc2a72 (11-04) |
  |---|---|---|
  | Unsaturated surge, excess over the quiet model | 15.0 mcores per refusal/s (r² 0.94) | — |
  | Onset slope (from 9.3 cores at 0/s) | — | 9.7 at 02:45Z, 15.2 at 02:50Z |
  | Quiet-hour mcores per transport | 0.095 | 0.077 |
  | Quiet-hour allocation per process | 29 MB/s | 25 MB/s |

  - The hourly fit since 11-01 21Z is 13.6 cores + 9.9 cores per 1k refusals/s (r = 0.83).
  - Neither the quiet-hour cost nor the cost per refusal is higher on the new release. Its connect changes touch the send path (custody under memory pressure, prepared refusals, TCP half-close). SOCKS session lengths swung as widely before the roll (6.7–106 s means) as after.

#### 3.3 Per unit of work: now, 11-03, 10-31

Fleet, both hosts, from `<scratch>/b/windows-out.tsv`:

| Window | Busy | Proxy cores | Devices | Transports | Refusals/s | SOCKS / HTTP adm/s | User MB/s | GC/min per proc | Alloc MB/s per proc | mcores/device | mcores/transport | ms CPU/admission | CPU-s/MB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-31 18:25–19:35 (CPUOPT3) | 33% | 12.8 | 30.9k | 105k | 0 | 3 / 164 | 7.3 | 1.6 | 51 | 0.42 | 0.122 | 77 | 1.8 |
| 10-31 18–22 eve | 34% | 13.0 | 30.4k | 102k | 0 | 3 / 150 | 7.0 | 1.7 | 55 | 0.43 | 0.127 | 85 | 1.9 |
| 11-01 18–22 eve (mixed) | 55% | 23.2 | 29.3k | 140k | 639 | 35 / 134 | 6.1 | 5.2 | 55 | 0.79 | 0.166 | 137 | 3.8 |
| 11-02 18–22 eve (mixed) | 71% | 31.0 | 36.8k | 160k | 2,121 | 125 / 154 | 7.3 | 6.7 | 82 | 0.84 | 0.194 | 111 | 4.2 |
| 11-03 18–22 eve | 75% | 32.6 | 37.5k | 135k | 1,435 | 83 / 123 | 3.9 | 8.0 | 82 | 0.87 | 0.241 | 158 | 8.4 |
| 11-03 04–10 (same hours) | 69% | 30.1 | 37.4k | 137k | 2,401 | 68 / 147 | 7.1 | 6.6 | 99 | 0.80 | 0.219 | 140 | 4.2 |
| **11-04 04–10 (now, pinned)** | **96%** | **43.5** | **50.6k** | **136k** | **1,923** | **118 / 113** | **2.5** | **12.0** | **138** | **0.86** | **0.321** | **188** | **17.5** |
| quiet 11-03 13:10–17:55 | 29% | 11.3 | 30.5k | 118k | 0 | 51 / 128 | 4.7 | 1.2 | 29 | 0.37 | 0.095 | 63 | 2.4 |
| quiet 11-04 00:15–02:40 | 25% | 9.3 | 27.7k | 121k | 0 | 74 / 123 | 4.3 | 0.9 | 25 | 0.34 | 0.077 | 47 | 2.2 |
| 11-01 06–09, no refusals (a8461e236) | 77% | 34.5 | 36.8k | 146k | 33 | 27 / 136 | 4.8 | 9.8 | 67 | 0.94 | 0.237 | 211 | 7.2 |

- **The "same hour on 11-03" comparison (10Z)** came just after a surge ended (refusals 1,217/s in the 08–09Z hour, 45/s by 09–10Z). The morning before it was a surge like today's: 30.1 cores at 2.4k refusals/s. Today has a third more devices, nearly twice the GC rate and 45% more cores.
- **The per-admission and per-byte costs** are 2.4× and 9.7× 10-31's. That is because user traffic is a small rider on background work: 2.5 MB/s of user payload for 43.5 cores. Neither measure drives the CPU.
- **Admission marginals.** Over 955 unsaturated per-process rows (all D2 releases, r² 0.83), the fit gives 6.2 mcores per SOCKS admission/s and 3.4 per HTTP admission/s. That is about 6 ms and 3 ms of CPU per connection, and about 0.7–1.1 cores for SOCKS at today's rates. The fit's other terms are unstable (a −1.2-core intercept) because GC makes the cost non-linear; only the admission terms are used.
- **SOCKS sessions are not short on average.** Mean SOCKS duration is 20–103 s per hour today, against 5–6 s on 10-31. Concurrent SOCKS sessions run 1.4–6.4k.
- **Dial outcomes.** `dial_refused_no_such_host` rose from about 0.15/s to 2–6/s, and SOCKS dial failures are 15–34% of admissions. Both cost little CPU.
- **HTTP admissions** are 38–64/s against 58–94/s in the same hours of 11-03. That may be a symptom of the slow contract path; it is not proven.

#### 3.4 Inside one process (perf, crisp g5, 2.18 cores, host 96.5% busy)

Inclusive shares of the 6,441 samples (`<scratch>/b/perf-crisp-g5.txt`), against CPUOPT3's capture D (crisp g8, 0.77 cores):

| Group | Now | CPUOPT3 | Main contents now |
|---|---|---|---|
| GC (background mark + assists) | **47.2%** (16.1 + 31.1) | 6–9% (assists 0.25%) | `tryDeferToSpanScan` 21.0% self, `findObject` 4.5%, `scanObject` 3.8%, `scanSpan`, `greyobject`; `scanstack`/`markroot` 10.4–11.3% |
| `mallocgc` (contains much of the assists) | 28.9% | 5.0% | small-scan and tiny slow paths, `growslice` 5.7% |
| FindProviders2 (window discovery) | **12.7%** | 2.9% | score-page reads `readWithClient` 6.5%, gob decode 4.4%, `decodeClientScorePage` 4.6% |
| Window lifecycle: expand/discover/channel setup | 16.6% | ~19% (lifecycle group) | 86% of it under `discoverDestinations` |
| In-process control (`ConnectControl`) | 10.5% | 9.0% | `CreateContract` 7.5%, `Provide` 0.5%, `CloseContract` 0.2%; `authenticate` 2.0%; PeerAudit 0% (was ~3%) |
| Dials | 6.0% (H1 dial path) | 6.3% | TLS handshake 4.9%, `x509.Certificate.Verify` 3.7% (5.3%), mint 0.9% (1.4%) |
| Keeping windows up | ~30% | ~37% | carrier workers `runH1.func4.1.1` 11.2%, receive side 9.0%, `SendSequence` 4.0%, `multiClientChannel` 4.2%, `Client.run` 2.2%, `detectBlackhole` 1.4% |
| Scheduler (`mcall`/`schedule`/`findRunnable`) | 5.1% | 13.3% | saturated: a P rarely idles, so little idle-P hunting |
| Timers | 3.8% | 6.2% | |
| go-redis / pgx | 7.0% / 2.8% | 7.0% / 2.7% | |
| User data plane | ~3–4% | ~4% | proxy ingress 1.4%, gVisor 1.1%, `ProxyDevice` 1.0%, SOCKS 0.3%, net/http 0.3%, userwireguard 0.2% |
| Kernel (dso) | 10.5% | 19% (capture A) | softirq/NAPI 4.2–4.7%, `rtl8127_rx_interrupt` 0.8% (2–3.4%) |

- The rows overlap. GC assists run inside whatever goroutine allocates, so they are charged to FindProviders2, control, carriers and the rest. Work other than GC is about 53% of the process, about 1.15 cores.
- **Per process, against CPUOPT3:** GC about 1.03 cores (about 0.05–0.07), FindProviders2 0.28 (0.022), `CreateContract` 0.16 (0.023), carrier workers 0.24 (0.09). The scheduler is about the same at 0.11 cores.
- **One FindProviders2 call** now costs about 15 ms of CPU, against about 5 ms. The calls run 18.8/s per process, and assists are charged inside them.
- **No lock contention:** `sync.Mutex.lockSlow` is 0.0% and `runtime.lock2` 0.5%.
- **/proc during the capture:**
  - 5.3k time slices a second, with a mean run-queue wait of 255 µs each;
  - 2.1k voluntary and 3.2k involuntary context switches a second (17 threads);
  - 17.9k read and 6.8k write syscalls a second.

#### 3.5 GC and memory: the knee

Per-process means from `<scratch>/b/heap-live.txt` (heap_alloc minimum and maximum over 15 minutes, scraped every 15 s) and `<scratch>/b/gc-per-process.txt`:

| When | Transports/proc | Goroutines/proc | heap_alloc min (≈ live) | next_gc | Headroom per cycle | Stacks | Alloc MB/s | GC/min |
|---|---|---|---|---|---|---|---|---|
| 10-31 19Z | 4.9k | 168k | 2.58 GB | 4.43 GB | 1.85 GB | 1.29 GB | 44 | 1.3 |
| 11-03 15–16Z (quiet) | 5.9k | 186k | 2.89 GB | 4.24 GB | 1.35 GB | 1.41 GB | 32 | 1.3 |
| 11-04 02Z (quiet) | 6.0k | 187k | 2.69 GB | 4.23 GB | 1.54 GB | 1.46 GB | 24 | 0.8 |
| 11-04 06–09Z (pinned) | 6.8k | 255–258k | 3.53–3.55 GB | 4.21–4.23 GB | 0.68 GB | 1.69–1.71 GB | 136–141 | 11.8 |

- **The memory limit sets the GC goal all day.** `next_gc` stays at 4.2 GB, below `heap_inuse` (4.4–4.6 GB). That is 6.82 GB less about 2.6 GB of stacks and runtime overhead.
- The GOGC goal would be live + (live + stacks), 7–9 GB. CPUOPT3 read 10-31 as GOGC-paced; on these minima it was already at the edge.
- **Headroom per cycle shrinks by about 0.5 GB per 1k transports per process.** Once it falls below about 0.8–1 GB, GC jumps.
- The no-refusal bins (`<scratch>/b/gc-regime-bins.txt`) show the knee:
  - 1.4–2.2 GC/min and 0.108–0.123 mcores per transport up to 7k transports per process;
  - 10.5–12.4 GC/min and 0.246–0.255 mcores per transport at 7–9k (240–270k goroutines).
- **Under limit pacing, memory buys CPU.** Each 100 MB less of live heap or stacks per process adds 100 MB of headroom. At today's 0.68 GB that is about 13% fewer cycles, about 6% of process CPU.
- **Host RAM.** crisp has 49 GiB available. fireside has 17.8–18.6 GiB, and services.yml (vault/main/services.yml:816–845) records proxy OOM kills on fireside on 10-23 and 10-25. alt holds 6.4–6.9 GiB per host (GOMEMLIMIT 8192 MiB, 12 GiB cap).

#### 3.6 Contention at the ceiling, and its latency

- **Run queue** (schedstat over 10 s at 10:49Z):
  - each proxy process ran 2.18–2.69 cores and waited 1.21–1.66, a wait/run of 0.53–0.68;
  - that is 13–14.5 cores of waiting per host;
  - alt ran 0.41–0.44 cores and waited 0.90–0.97, a wait/run of 2.2;
  - PSI cpu-some was 46–50% (16–18% at 31% busy in CPUOPT3).
- **Contract latency inside the proxy:**
  - `create_contract` takes 1,118–1,273 ms (29–41 ms on 10-31 and in quiet hours);
  - `close_contract` takes 420–447 ms (11–17 ms);
  - every creation stage inflates 50–100×: grant selection 404 ms (6–17 ms), endpoint lookup 76 ms (1 ms), relationship 158 ms (1.2–1.4 ms).
- **The backend is not the cause.**
  - The api's `create_contract` is 5.7 ms and its grant selection 1 ms at the same hour.
  - alt, on the same hosts, is 40 ms against 29 ms at 02Z.
  - The PostgreSQL transaction stage is 5.7 ms.
  - So the inflation is CPU starvation inside the proxy.
- **Admission bounds hold, and refuse:**
  - `hosted_contract_admission_in_use` averages 161–188 per process against its 256 cap, with 185–264 refusals/s (0 in quiet hours);
  - mint admission refuses 928–959/s (0.7–2.2/s quiet);
  - Redis pool timeouts at the proxy run 478/s (0 quiet), with 22.9k pool gets/s (2.7k quiet);
  - contract `deadline` rejections run 195–370/s.
- **Platform cost of the loop:**
  - about 450 candidate mints/s and 400 retirements/s reach PostgreSQL, against about 60/s in quiet hours (Part D: edge-2 was 73% busy at 10Z);
  - about 2.4–2.6k score-page lookups/s each read a page of about 120 KB, about 300 MB/s fleet (§5 #3, Part E);
  - TCP opens are 425/s per host (85/s quiet);
  - NIC receive is 165–186 MB/s per host (39–53 MB/s quiet), consistent with the page reads.
- **User-facing timings:**
  - HTTP `connect_timeout` is 4–5/s (5–10/s before);
  - no WireGuard return backpressure;
  - session interval maxima measure idle long-lived sessions (hundreds to thousands of seconds), not latency;
  - the session duration buckets have no recent samples (not captured).

#### 3.7 alt on the proxy hosts

alt is about 0.38–0.40 cores per host now: 1.6% of a host. That compares with 0.25–0.27 on 10-31's evening and 0.17–0.19 in the 11-03 afternoon. It serves 74–82 requests/s (40–77 before).
- It is the clearest victim of the ceiling: wait/run 2.2.
- Its mean HTTP latency was 15.5 ms at 02Z (quiet), 15.6 ms at 06Z and 33 ms at 09Z.
- Its control requests take 23–30 ms, against 16–27 ms quiet.
- alt's own CPU is Part C's.

#### 3.8 Capacity and headroom

- **Per host:**
  - 24 cores; the proxy can get about 21.7 of them, and is using them;
  - demand is at least 43.5 cores fleet-wide;
  - the onset slope (15 ms per refusal) plus the 8–16k devices added since 02:50Z put it at roughly 45–55 cores (inferred, not measurable while pinned).
- **Without refusals:**
  - a process stays at 0.11–0.12 mcores per transport up to about 7k transports, and the fleet about 140k transports (about 35k devices at 4 per device), 15–17 proxy cores (about 35–40% busy);
  - past that, cost per transport doubles; on 11-01 06–09Z, 146k transports already took 34.5 cores.
  - With the refusal loop on top, the evening peaks of 11-02 and 11-03 needed 31–33 cores (71–75%), and today needs more than both hosts have.
- **A lower per-connection cost buys little:** the user data plane is 3–4% of CPU.
- **Removing the refusal loop (§5 #1)** would put today's 50.6k devices at about 14–16 proxy cores (about 35–40% busy, inferred). That is the quiet base of 9–11 cores plus about 0.17 mcores for each of the 25k standing devices (CPUOPT3's per-device term) and their few transports.
- **A third proxy host** (24 cores, 10 more blocks) gives 72 cores. Per-process load falls by a third (about 4.5k transports and 1.7k devices), below the knee. Today's state would then need roughly 30–40 proxy cores of 72 (about 45–60% busy, inferred): the refusal loop still costs 5–10 ms per refusal below the knee. With §5 #1 as well, about 25%.
- **GOMEMLIMIT moves the knee** (§5 #2): +0.5 GiB to about 8k transports per process, 8 GiB on crisp to about 10k.

### 4. What CPUOPT3's shipped changes did

#### 4.1 D2: strict window handover (live from 11-01's f19323066; kill-switch plumbing and the app-off path in 1e94b4d78)

Refusal-free windows after D2 against CPUOPT3's window. 10-31 18:25–19:35Z: 30.9k devices, 105k transports. Quiet 11-03 13:10–17:55Z and 11-04 00:15–02:40Z: 30.5k and 27.7k devices, 118k and 121k transports.

| Measure | Predicted | 10-31 | After D2, quiet | Change |
|---|---|---|---|---|
| Hosted retirements (JWT retire checks) | −265/s | 291/s | 57–62/s | −79% |
| Mints per device-hour | 31–44 → about 4 plus under-filled failures | 33.9 | 6.7–8.1 | −76 to −80% |
| `create_contract` | fall in step | 374/s | 141–180/s | −52 to −62% |
| `provide` | fall in step | 296/s | 80/s | −73% |
| FindProviders2 calls | follow creation | 94/s | 50–58/s | −38 to −47% |
| TCP active opens per host | −265 handshakes/s fleet | 170/s (crisp) | about 85/s | about −50% |
| `declined_strict_cap` | → 0 | (no counter) | 0.1–0.2/s | the waste the plan targeted is gone |
| `admitted_handover` | about installed ÷ 0.875 h ≈ 37/s | — | 5–7/s | lower; see below |
| `strict_hole` windows | → 0 | — | 0.9k (11-04 night) to 11.4k (11-03 17Z) | not met |
| Proxy CPU | −10–19%, 1.2–2.6 cores | 0.122 mc/tr, 0.42 mc/dev | 0.077–0.095 mc/tr, 0.34–0.37 mc/dev | −22 to −37% per transport, with D3 and mix; about 15–20% (1.9–2.6 cores) for D2 |
| Proxy Redis gets | — | 9.5k/s | 2.7k/s (11-04 02Z) | −71% (D2 + D3) |

- **Confidence: medium.** There was no counters-only baseline, and the quiet windows are afternoon and night against a Saturday evening, at about 14% more transports per device. The fall in mints, contracts and dials is large and unambiguous. The CPU split between D2 and D3 is inferred.
- **Where rotation goes.** In quiet hours the removals are verdict 21–33/s, unhealthy 18–24/s, handover 5–7/s and closed 3.5–11/s. A client lives about 35 minutes on average, so most leave through detection before their 45–60-minute rotation. That is why handovers are a fifth to a seventh of the plan's estimate. Those removals are detection bounds and stay.
- **What D2 deferred is now the problem.** D2 part 3 left under-filled windows to a follow-up "only if they dominate" (owner question 4). They dominate whenever a payer is out of balance. The cause is not provider-side identity reuse; it is contracts the payer cannot open, which question 4's design would not fix.

#### 4.2 D3: PeerAudit (f19323066)

- Unhandled control frames at the proxy fell from 0.7–4.2k/s (2.2k/s at CPUOPT3's peak) to 0 from 11-01 20Z.
- The proxy's discard counter is also 0. The proxy's connect has `SendPeerAudits` off (c034364a is in every pin since), so the audits are never sent, rather than discarded.
- JWT control checks fell from 3.5k/s (10-31 peak) to 0.33–0.39k/s in quiet hours.
- They rise again in surges (3.0–3.3k/s), all of it contract and provide work.
- CPUOPT3 predicted 0.3–0.4 proxy cores (2.5–3%). The frames are gone. The CPU cannot be separated from D2, which shipped with it.

#### 4.3 Others

- **D1** is in-process delivery in the connect service (Part A). It does not run in the proxy.
- **The timer-churn merge c2cc384a4** is in every release since a8461e236. Timers are 3.8% inclusive now, against 6.2%; per process that is 0.08 cores against 0.05, with three times the work.
- **Not shipped:** D4 (JWT and chain caches, deferred), and CPUOPT3 B #2, #5, #7, #8, #9 and #10 (§6).

### 5. Ranked opportunities

Savings are in cores. "Surge" is a peak with 1.4–2.1k refusals/s, like every evening since 11-01. "Normal" is a refusal-free peak like 10-31's (about 13 cores). Savings overlap: #1 shrinks every other item's surge value, and #2 and #4 both act on GC headroom. Do not add them.

#### #1 A balance-aware refill for hosted windows (new; the follow-up D2 deferred)

- **Change:**
  - While the device's contract status is `InsufficientBalance`, a below-target window builds no candidate: no FindProviders2, mint, Provide, dial or evaluation.
  - Each expansion step on the existing backoff sends one contract request for the device instead, as a balance probe.
  - Success clears the status, as the sdk already does on any successful contract, and the next pass expands at once.
  - Count it as a new pass state `skipped_insufficient_balance` in `urnetwork_proxy_window_expand_passes_total`.
  - Setup, Reliability and NoPermission failures keep today's path.
- **Code:**
  - connect 0dad308f8: `(*multiClientWindow).expand` and the resize planner (ip_remote_multi_client.go:14373 and the pass before it), `noteDiscoveryExpandUnproductive` (ip_remote_multi_client_discovery_retry.go:42–106), `windowCandidateFailureOutcome` (ip_remote_multi_client_window_client_stats.go:496–508);
  - sdk 4b3cddc9d: the status is `netContractStatus.InsufficientBalance` (device_local.go:3195–3260), which needs a getter into `MultiClientSettings`;
  - server d5cbc2a72: the refusal class is in controller/connect_controller.go:276–300.
- **Evidence:** §3.2. The correlation is r = 0.83, at 10–15 ms per refusal. There are about 8 refusals per failed candidate. Failed-contract candidates run at 170–330/s during surges and about 0/s otherwise.
- **Saving:**
  - surge: 15–30 cores (today: 43.5 pinned → about 14–16, inferred);
  - normal: 0;
  - outside the proxy: about 400 mint/retire transactions/s, about 20k Redis gets/s, about 300 MB/s of score-page reads and about 650 TCP opens/s (mostly TLS dials to the edges), during a surge.
- **Risk:** medium. It touches refill logic. A stuck status would leave a device with no exits after a top-up, so recovery must be tested end to end.
- **Timing bounds:**
  - the discovery backoff (10 s → 2 min), evaluation deadlines (15/30/30 s), the contract retry cadence of live sequences (1–5 s, 30 s), health, blackhole and ping cadences: all unchanged;
  - the probe runs once per expansion step, the cadence at which expansions find a refilled balance today, so resume after a top-up is no slower (at most the 2-minute backoff cap, as now);
  - nothing is disconnected (memory `no-auto-disconnect`).
- **Validate:** on one block against the rest, at the same hours:
  - refusals per out-of-balance device fall at least 8×, and `failed_contract` approaches 0;
  - FindProviders2 calls, mints and TCP opens per device return to quiet levels;
  - a top-up test (DB test with a drained balance, then a grant) admits a candidate within one backoff step;
  - `below_min` windows of funded devices, and `connect_dial_error` and `connect_timeout` per admission, do not rise.

#### #2 Raise the proxy's GOMEMLIMIT (CPUOPT3 #9, not done)

- **Change, either:**
  - (a) GOMEMLIMIT 7000 MiB for the service (the CPUOPT2-era value; +0.5 GiB per process, +5 GiB per host); or
  - (b) per-host env in warpctl, then 8 GiB on crisp and +0.5 GiB on fireside.
- **Code:**
  - vault/main/services.yml:816–845 (`cores: 4`, `memory_limit: 10GiB`, `env_vars: GOMEMLIMIT: 6500MiB`);
  - warp/warpctl/run.go:2186–2192 (GOMEMLIMIT defaults to 90% of `memory_limit`; GOMAXPROCS = `cores`);
  - warp/services/services.go:700–735 (`EnvVars` are service-wide; there is no per-host override, so (b) needs a warpctl change).
- **Evidence:** §3.5. Today's headroom is 0.68 GB per cycle. GC is 47% of CPU at 11.8 cycles/min. The knee sits at about 7k transports per process.
- **Saving:**
  - surge: (a) headroom 1.22 GB, about −44% cycles, about 20% of proxy CPU, about 8–9 cores; (b) on crisp, headroom about 2.45 GB, about −71% cycles, about 7 cores on crisp alone (inferred from the cycle model);
  - normal: 1–3% (GC is about 8% there);
  - the main value is capacity: the knee moves to about 8k (a) or 10k (b) transports per process.
- **Risk:** RAM. fireside keeps about 13 GiB available under (a), and a full-overlap restart of a block needs about 6–7 GiB more. The OOM kills of 10-23 and 10-25 were on fireside. The 10 GiB cap is unchanged. **Owner decision.**
- **Timing bounds:** none. It moves GC pacing only, and fewer assists shorten tail latency.
- **Validate:** one block first. GC cycles/min and mcores per transport at equal transports per process, RSS against 95% of the new limit, no `memory.events` high/max, and host MemAvailable at the peak.

#### #3 FindProviders2 page hits without the Redis read and the copy (new)

- **Change:**
  - In `readWithClient`, for native pages, look up the decoded page by the index's `page.nativeChecksum` before reading, and pipeline HMGET only for the misses.
  - In `decodeClientScorePage`, stop copying every score of the page per lookup: copy on write, or a per-request overlay of the fields readers set.
- **Code:**
  - server d5cbc2a72: model/network_client_score_cursor.go:164–252. Every page is read: HMGET of generation and page, then a `[]byte(value)` copy and a sha256 over about 120 KB, and only then the cache.
  - model/network_client_score_page_cache.go:18 (64 MiB bound) and :120–140 (`copies := make([]ClientScore, len(sample))` per lookup).
- **Evidence:**
  - lookups run 2,438–2,635/s fleet in the surge (320–420/s quiet), at 85–88% hits (96% quiet), with evictions equal to misses;
  - decoded pages average about 650–710 KB, so about 120 KB of gob each;
  - FindProviders2 is 12.7% of the profile, the read 6.5% and gob 4.4%;
  - the inferred allocation is 30–45 MB/s per process in the surge (two copies of the page plus the score copies): 22–33% of the 138 MB/s.
- **Saving (inferred):** surge 1–3 cores, through less allocation under limit-paced GC and less read work; normal 0.2–0.4 cores. It also takes about 85% of the score-page bytes off Redis output (Part E) and the hosts' NICs, and does the same for the api's FindProviders2 (Part C).
- **Risk:** low–medium.
  - The result can change in one case: a page replaced after the index read is served index-consistent where today it reads "missing or changed page".
  - That needs review, and a shadow comparison of selections.
- **Timing bounds:** none.
- **Validate:** Redis HMGET bytes from the proxies, NIC receive per FindProviders2 call, alloc MB/s per call, identical selections in shadow mode, and the FindProviders2 stage timings.

#### #4 One ContractOriginNotifications per proxy process (CPUOPT3 #5, not done)

- **Change:** the device manager owns one `model.NewContractOriginNotifications` and passes it through `AuthoritySettings`, as `MintAdmission` and `ContractAdmission` already are. api, connect and taskworker build one per process (api/run.go:303, connect/resident.go:964).
- **Code:** server d5cbc2a72 localclient/authority.go:85; model/contract_origin_notification.go.
- **Evidence:** 18 goroutines per device. At today's 2.5k devices per process, that is about 45k of 255k goroutines and about 0.30 GB of the 1.69 GB of stacks.
- **Saving (inferred):** under limit pacing the 0.30 GB becomes heap headroom (0.68 → 0.98 GB, about −30% cycles), plus 18% less stack scanning. Surge: 2–5 cores. Normal: 1–2%, 0.1–0.3 cores (CPUOPT3's figure).
- **Risk:** low. Size the bounds (`MaxWaiters` 8192, publish queue 1024) for the process.
- **Timing bounds:** none.
- **Validate:** goroutines per device in the fit fall by 18, stacks per process fall, and the contract-origin wait outcomes are unchanged.

#### #5 The D4 caches (deferred by the owner): JWT claims, certificate chains

- **Code:** server localclient/authority.go `parseClaimsPreflight` / `authenticate`; connect net_tls_hello.go:186–248 (session cache per server name) and uTLS `verifyServerCertificate`.
- **Evidence:** `x509.Certificate.Verify` is 3.7% and `authenticate` 2.0% of the profile. Dials run at 425/s per host in surges and about 85/s quiet. Control calls run 3.0–3.3k/s in surges and 0.33–0.39k/s quiet.
- **Saving:** surge 1–2.5 cores; normal 0.2–0.5 cores. Most of the surge value disappears with #1.
- **Risk, bounds and validation:** as in D4. Expiry and revocation stay per call.

#### Smaller and owner items

- **Hosted quality window 5 → 3 (CPUOPT3 #10, owner).** The sdk still gives 5 + 1 at the 32 MiB budget: `mobileQualityWindowSize` 4 scaled by 32/24 (sdk mobile_memory_policy.go:237, 292–299, 544–555). About −23% carriers. 10–15% at normal load, more inside the knee.
- **Idle tier or a shorter hold (CPUOPT3 #2, owner).** `defaultProxyDeviceIdleTimeout` is still 45 min (proxy_device.go:1077). 20–40% at normal load.
- **PingJitter for hosted carriers and channel pings (CPUOPT3 #7).** Not wired: there is no `PingJitter` in server or sdk, and channel pings still call `resetWakeupTimer(pingTimer, restTimeout, restTimeout)` (connect ip_remote_multi_client.go:20190). 1–3% at normal load; nothing while pinned.
- **GOMAXPROCS 4 → 3 on one block (CPUOPT3 #8, owner A/B).** 40 Ps on 24 CPUs now drive 3.2k involuntary switches/s per process. The scheduler share fell to 5.1% because the processes are saturated, so measure this only after #1.
- **`localclient.convert` JSON round trip** (authority.go:177): still there, 0.2–0.4%.
- **RPS on the single-queue Realtek NIC:** not in xops. `rtl8127_rx_interrupt` is 0.8% of samples, and `ksoftirqd/10` is the busiest kernel thread on crisp.

**Owner decisions in this part:**
1. GOMEMLIMIT: (a) or (b), against fireside's RAM.
2. A third proxy host, against #1.
3. The idle tier or hold, and the quality window.
4. Whether the proxy should keep opening and holding hosted devices for a network whose contracts are refused for balance. This is a product posture; #1 changes no user-visible outcome.
5. The GOMAXPROCS A/B.
6. D4's two caches, still deferred.

### 6. CPUOPT3 predictions checked

Checked in git (deployed revisions and origin/main): D2 and D3 merges in the server, connect and sdk pins; `localclient/authority.go:85` (#5); `PingJitter` in server, sdk and connect (#7); services.yml `cores` (#8) and `GOMEMLIMIT` (#9); sdk window sizes (#10); `defaultProxyDeviceIdleTimeout` (#2); claims- and chain-cache code (D4); RPS in xops.

| CPUOPT3 item | Prediction | Observed |
|---|---|---|
| B #1 / D2 | 10–19% (1.2–2.5 cores); about −265/s mints, contracts and dials; mints per device-hour → about 4 | In refusal-free hours: −79% retirements, 6.7–8.1 per device-hour, contracts −52–62%, dials about −50%, about 15–20% CPU. Shipped 11-01 together with its counters, so there was no counters-only baseline. Handovers 5–7/s, not 37/s; strict holes not 0. |
| B #4 / D3 | 2.5–3% (0.3 cores); `unhandled_message` → 0 | → 0 at the proxy (the proxy sends no audits). CPU not separable. |
| B #2 idle tier / hold | 20–40% | Not done; owner. |
| B #3 / D4b chain cache | 4–5% | Deferred. x509 is 3.7% now. |
| B #5 one ContractOriginNotifications | 1–2%, −27k goroutines per process | Not done. Now −45k goroutines per process, and more value under limit-paced GC. |
| B #6 / D4a JWT cache | 1.5–2% | Deferred. `authenticate` is 2.0%. |
| B #7 PingJitter | 1–3%, PSI → low | Not done. PSI is 46–50%, now from saturation rather than bursts. |
| B #8 GOMAXPROCS 4 → 3 | 2–5% | Not done (`cores: 4`). |
| B #9 memory | the limit governs from +20–30% transports; GC triples at +50% | Materialized. The knee is at about 7k transports per process (+37%); past it GC runs 10–12 cycles/min and CPU per transport doubles. GOMEMLIMIT is still 6500 MiB on both hosts. |
| B #10 quality 5 → 3 | 10–15% | Not done (5 + 1). |
| Cost model, 0.11–0.12 mcores per transport at the peak | — | 0.077–0.095 refusal-free after D2 and D3; 0.24–0.32 in surges |
| "Nothing leaks" | CPU tracks transports | Still true without refusals. With refusals, CPU tracks refusals (r = 0.83) and transports only weakly (0.30). |

### 7. Considered and rejected under the owner rule

- **Lengthening the discovery backoff (10 s → 2 min) for below-minimum windows.** It bounds how fast any window refills after a provider loss. #1 skips work that cannot succeed and keeps the cadence.
- **Shortening `CreateContractTimeout` (30 s), or slowing the 1–5 s contract retries of live sequences.** These bound how fast traffic resumes after a refusal clears. Even ending a wait early on InsufficientBalance changes when an in-flight transfer gives up, so it is not proposed. #1 alone removes the candidates that make most of the waits.
- **Shorter evaluation deadlines (15/30/30 s) for candidates:** rejected.
- **Cancelling a device whose windows cannot get contracts.** `Active()` keeps readiness out of the liveness gate on purpose, to avoid the device-recreation loop (proxy_device.go:2085–2091). A shorter hold for such devices is a product decision (owner list, item 4), not an engineering change.
- **Raising `hosted_contract_concurrency` (256) or the mint admission bound.** They protect PostgreSQL and the pool. Raising them would add load and loosen a protective bound. Lowering them is not needed.
- **Slower cpings, platform pings, the blackhole backstop, or fewer verdict and unhealthy removals (45–50/s in quiet hours).** These are detection bounds, kept.
- **GOGC above 100.** Under limit pacing it changes little and adds risk. #2 is the lever.

Not captured:
- **Allocation profiles.** There is no pprof read in the allowed methods, so the #3 and #4 allocation shares are inferred.
- **A second perf capture.** One capture was the limit, so the goroutine-root table failed (my root filter missed `runtime.goexit.abi0`) and only the inclusive categories remain.
- **Per-process network bytes.** The containers use host networking.
- **PSI and context-switch history.** Not in Mimir for these hosts.
- **Session-duration buckets.** No recent samples.
- **The f19323066 connect pin.** Not recorded.
- **`git fetch`.** Refused (no key); last-fetched refs were used.

### 8. Raw outputs

All under `<scratch>/b/` (= `<scratch>/b/`).
- **Scripts:** `mq.py`, `starts.py`, `hourly.py`, `fivemin.py`, `dataset.py`, `regress.py`, `windows.py`, `view.py`; host scripts `scripts/run.sh`, `recon.sh`, `recon2.sh`, `perf-one.sh`, `envlimits.sh`.
- **Releases:** `proxy-starts.txt`, `proxy-revisions.txt`.
- **Mimir:** `mimir/hourly-host.tsv`, `hourly-proxy.tsv`, `hourly-window.tsv`, `hourly-events.tsv`, `hourly-contract-rejections.txt`, `ds5m.tsv`, `5m-1103-2130-1104-0500.tsv`; views `view-core.txt`, `view-control.txt`, `view-cand.txt`, `view-events.txt`, `view-window.txt`.
- **Derived:** `windows-out.tsv`, `regress-out.txt`, `gc-regime-bins.txt`, `heap-live.txt`, `gc-per-process.txt`, `hourly-correlation.txt`, `contract-outcomes.txt`, `control-frame-ms.txt`, `contract-stage-ms.txt`, `contract-ms-by-service.txt`, `fp2-page-cache.txt`, `proxy-backend-load.txt`, `alt-latency.txt`, `session-timings.txt`, `labels-proxy*.txt`, `names-proxy.txt`.
- **Host:** `recon-crisp.txt`, `recon2-crisp.txt`, `recon2-fireside.txt`, `envlimits-crisp.txt`, `perf-crisp-g5.txt` (summary; the perf data was deleted on the host). IPs are masked.

## Part C — api, alt and taskworker

This is a read-only measurement of Main on 11-04, 10:15–11:05Z, with Mimir history back to 10-29. These are the deployed revisions:
- **api and alt:** `2026.11.3-outerwerld+1089433320`, built from server `d5cbc2a72` with connect `0dad308f8` and sdk `4b3cddc9d` (`temp/resume-1102/release/build-r2.sh`).
- **taskworker:** `+1089300440`, built from server `9e2dcaeed` with connect `49f285f1e` (`build-fu.sh`).

Code references are to those commits. *(inferred)* marks a claim reasoned from code or arithmetic rather than measured.

The live captures ran at Wednesday-morning load. The api was at 15.8–16.0k req/s, the services edges at 57–81% busy, and crisp and fireside at 97–98%. CPUOPT3 measured at the Saturday 10-31 evening peak. So comparisons below use Mimir at matched hours, and per-request costs.

### Summary

**Size.**
- **api:** 18.7 cores at 10:00Z on 11-04, at 15.8k req/s (30-minute rates). Its daily means for 10-29 to 11-03 are 13.3–17.1 cores, and its evening peaks (18–22Z) 16.8–19.4 cores. CPUOPT3 had 13.0 mean and 17.4 peak.
- **taskworker:** 12.3 cores mean and 13.7 peak on 11-04 (CPUOPT3: 11.5 and 13.2).
- **alt:** 0.45–0.57 cores until 11-03, and 0.69–0.78 since 04Z on 11-04.

**api: cheaper per request.**
- **Fleet:** 1.19 cores per 1k requests at 10Z on 11-04, against 1.40 in CPUOPT3's window.
- **Per host:** 1.06–1.86 cores per krps, down 4–14% on every host, although edge-3 and edge-4 are busier than on 10-31.
- **Why:** D3, and a mix with more cheap replays.

**`/connect/control`.** It carries 13.8k of 15.8k req/s at 10Z. Only 17% of those requests do real work:
- refusal replays: 60%, at about 0.5 ms each;
- PeerAudit discards: 22%, at about 0.1–0.15 ms;
- real creates: 16%, at about 1.9–2.4 ms;
- close and provide frames: 1%.

**The replay creep follows connect's uptime, not the api's.**
- Connect has not restarted since 11-02 19:38Z. At 38.9 h of uptime, replays reached 8.3k/s, the highest of any generation since 10-29. They cost about 4 api cores and 8.3k Redis admissions/s.
- The api restarts of 11-03 dipped replays only briefly, while each process's refusal ring refilled.
- The stuck-provider recycle that targets this creep is built, deployed on api and connect, and switched off.

**D3 did what it predicted, per request.**
- Across the 11-02 06:00Z fleet roll, api cores fell by 2.3 against the traffic mix, at 3.5–3.9k audits/s. Restarts that changed no hot path moved −0.5 to +0.3 cores.
- That is about 0.55–0.65 ms saved per audit, or 1.6–1.9 cores at the evening audit rate. The prediction was 1.4–1.7 cores at 2.6k/s.
- Clients still send audits: 2.6–3.9k/s at the api with no decline since 11-02, and 35–42/s at alt. At the proxy they stopped with its 11-01/02 release, because its in-process clients run connect with `SendPeerAudits` off. Providers get that change with the next nightly; apps release separately.

**Shares inside an api process (two perf captures):**
- **JWT handling:** 14.5–20.9% of samples, about 19–21% fleet-weighted, or 3.5–3.9 cores at 10Z. CPUOPT3 measured about 19%. The verified parse costs about 0.27 ms per authenticated request. The lag-guest gate's unverified decode of the whole claims set adds 0.09 ms to every bearer request, discards included.
- **go-redis:** 12.1–12.2%.
- **TLS:** 0%. nginx terminates TLS, and Redis and PostgreSQL are plaintext.

**taskworker.**
- **The egress probe is now 85–90% of taskworker CPU:** 11–12 of 13.3 cores at 75 attempts/s. CPUOPT3 measured at least 55% at 61.5/s. The non-probe floor fell from 0.71 to 0.12–0.16 cores per process now that the `ApplyLegacyProviderTotals` backlog has drained.
- **TLS:** 38.7% of a probe-running process. x509 chain verification alone is 28.1%, about 38–40 ms per attempt, or 2.8–3.0 fleet cores (CPUOPT3: 17%, 2.6 cores).

**`ClosePayerDueContractsTask` is a retry loop with no output.**
- Five payers fail the task's page read at exactly its 20 s `ReadTimeout`, about 45 times an hour each. The task target retries every failure within a minute (`WithErrorRetryCap`), forever.
- The successful runs average 9 s and every one logged in four processes' 2-hour logs returned `due=0 closed=0`.
- Together the drain keeps about 1.5 runs in flight, each holding a database connection: about 1.3 connection-seconds per second in reads cancelled at 20 s. It closes nothing.
- **Why, from the code:** the page read walks the payer's whole unresolved-contract index range and checks `expiration_time` in the heap. When few of a payer's open contracts are due, the 4,096-row LIMIT is never reached.

**alt's latency rise is not CPU starvation.** At 08–10Z on 11-04 its mean was 27–33 ms:
- **`/subscription/balance`:** 9–16 ms of it, at 2.4–3.6 s per request. The api's same route went to 0.5–0.9 s at the same time, so this is the database.
- **The audit discards:** 11–15 ms. Their handler residence went from 1–4 ms to 11–13 ms with alt's 11-03 22:18Z release, already at 00Z with the hosts 27% busy. alt reads the body straight off the client connection, while the api sits behind nginx and reads it in 0.06 ms.
- **When the hosts saturated (04–06Z):** the mean did not move (13.9–16.2 ms, against 16.6 ms at 02Z).
- **Run queue:** alt waits 2.2 s per CPU-second, but in 48 µs slices. `/hello` stayed at 0.21 ms, so queueing adds under about 0.4 ms per request.
- **CPU per unit of work:** about 50% higher under the saturation. That is alt's cost, not its latency.

**Ranked opportunities.** Savings are in cores at the evening peak. They overlap where noted, so do not add them up.

| # | Change | Service | Est. saving | Risk | Effort |
|---|---|---|---|---|---|
| 1 | Turn on the built stuck-provider signal, then the recycle, staged by block | api, Redis, connect | 1–2.8 api cores at the evening peaks of 10-31 to 11-03, 3.7 at today's 8.3k replays/s, plus 0.2–0.7 Redis cores *(inferred; the signal-only step measures the stuck share first)* | medium (closes stuck old providers' transports, excused) | none (built) |
| 2 | D4(a), the verified JWT claims cache | api (alt, proxy, connect) | 2.6–3.4 api cores, about 0.2 proxy | low–medium | about 3 days (designed) |
| 3 | D4(b), the verified-chain cache in connect's TLS client | taskworker (proxy, apps) | 2.2–2.7 taskworker cores, plus 0.5–0.7 proxy (B) | low (crypto code) | about 3 days (designed) |
| 4 | Payer drain: an index the page read can stop on; back off a payer whose read keeps timing out | PostgreSQL, taskworker | about 1.3–1.5 connection-seconds per second freed, up to about 1–1.3 edge-2 cores *(inferred)*, plus 150–235 stack-trace logs/h | low–medium (index on a hot table) | S–M |
| 5 | Read the lag-guest claim without decoding the whole claims set | api, alt | 0.6–0.8 api cores *(inferred)*; void if #2 ships | low | XS |
| 6 | Origin notifications: a version check instead of the 20-process broadcast (CPUOPT3 C #4) | api, alt, Redis | about 0.5 api cores, 0.04 alt, about 20k pub/sub deliveries/s | medium | M |
| 7 | GOGC 400 on the busy api blocks; the GOMAXPROCS 8→4 A/B (CPUOPT3 C #9) | api | about 0.4 cores; 0.4–0.9 *(inferred)* | low | XS |
| 8 | Share a control-plane strategy per probe process (CPUOPT3 C #5) | taskworker | about 1 core of handshakes beyond x509 | medium (probe design review) | S–M |
| 9 | `encodeUuid` without fmt, a precompiled `updateFunctionName` regexp, pre-bound Prometheus children (CPUOPT3 C #6–#8) | all | about 0.2–0.25 api, 0.1 taskworker, 0.2 api | very low | XS each |

#1, #4 and #5 are new or newly measurable. #2 and #3 are the deferred D4 plans, and both are now larger than CPUOPT3 estimated. Nothing here loosens a timing bound; the rejected items are in §6.

### 1. Method

**When and load.**
- **Live captures:** 11-04, 10:15–11:05Z, a Wednesday morning.
  - api: 15.8–16.0k req/s and 18.7–19.6 cores.
  - taskworker: 12.9–13.3 cores at 75 probe attempts/s.
  - Hosts: services edges at 57% (edge-0), 78% (edge-1), 79% (edge-3) and 81% (edge-4) busy; crisp and fireside at 97.6%.
- **Mimir:** 10-29 00:00Z to 11-04 10:30Z, on 30-minute and 10-minute grids; evening peaks 18–22Z of 10-31 to 11-03.
- **Excluded from steady state:**
  - restart waves, the first 40 minutes after any api start;
  - the edge-6 dump snapshot (11-03 22:35Z–11-04 02:05Z), which degraded the taskworker;
  - the archive export, from 02:37Z on 11-04.

**Revisions running** (container `WARP_VERSION`, `urnetwork_build_info`, process start times; `<scratch>/c/inv_containers.txt`, `<scratch>/c/deploys.txt`):

| Service | Build | Server | Started |
|---|---|---|---|
| api (20 processes) | +1089433320 | d5cbc2a72 | 11-03 22:08:59–22:16:43Z |
| alt (crisp, fireside) | +1089433320 | d5cbc2a72 | 11-03 22:18:34Z |
| taskworker (8) | +1089300440 | 9e2dcaeed | 11-03 19:24:47–19:28:04Z |
| connect (20) | 2026.11.2 +1088460100 | 1e94b4d78 (per two 11-02 release notes) | 11-02 19:13–19:38Z |

22:50–23:26Z is the proxy's roll. The api and alt rolled about 40 minutes earlier.

**Settings:**
- api: GOMAXPROCS 8; GOGC and GOMEMLIMIT unset.
- alt: GOMAXPROCS 4, GOMEMLIMIT 8 GiB.
- taskworker: GOMAXPROCS 4.
- connect: GOMAXPROCS 16.
- All are on go1.27.3, with no CFS quota (cgroup `cpu.max` is `max`).

**Builds at each evening peak** (`<scratch>/c/mimir_misc.txt`):

| Evening | api | Connect uptime at 20Z | Notes |
|---|---|---|---|
| 10-31 | +1086374610 (a8461e236, before D3) | 17.5 h | CPUOPT3's release |
| 11-01 | +1087064850; beta +1087474430 | 11.9 h | D3 on beta only, from 15:40Z |
| 11-02 | +1088438700 | 0.5 h | D3 fleet-wide since 06:17Z; api restarted 18:50Z; connect restarted 19:13–19:38Z |
| 11-03 | +1089297300 | 24.5 h | D3; api rolled 17:50–19:18Z; taskworker rolled 18:05–19:28Z |

**perf.** Frame-pointer call graphs, one process per capture. Aggregation into symbol and category counts ran on the host, and each data file was deleted there. Every capture ended with "data deleted" and "no perf left" (`<scratch>/c/perf_one.sh`). Shares are of samples. Those include the host's network softirq work that lands on the process's threads (8–14%), which `/proc` does not charge to it.

| Target | Host | Length, rate | Samples | Cores during |
|---|---|---|---|---|
| api g2 (busy) | edge-4 | 40 s, 99 Hz | 8,739 | 1.85 at 1,650 req/s |
| api g2 (light) | edge-0 | 40 s, 99 Hz | 5,105 | 0.90 at 439 req/s |
| alt | crisp (97.6% busy) | 30 s, 99 Hz | 1,561 | 0.41 |
| taskworker g1 (no probe in window) | edge-4 | 60 s, 99 Hz | 1,528 | 0.19 |
| taskworker g2 (probe running, 20.5 attempts/s) | edge-3 | 60 s, 99 Hz | 17,190 | 2.74 |

**Other evidence:**
- Run-queue delay from two `/proc/<pid>/task/*/schedstat` snapshots 20 s apart, plus cgroup `cpu.stat` throttling and PSI, for alt and the proxies on crisp and fireside, and for api and taskworker on edge-4 and edge-0 (`<scratch>/c/sched_delay.sh`).
- Taskworker `docker logs` for the last 2 h on edge-1 and edge-4. Ids were masked and payers counted only by a 6-hex hash (`<scratch>/c/tw_logs_payerdue*.sh`).
- A regression of api cores on request classes; a step test across api restarts; a per-process fit of taskworker cores on probe attempts.

**Not captured, refused, and production notes.**
- **The payer-due read on edge-2:** `pg_stat_activity` samples, its `pg_stat_statements` row and a plain `EXPLAIN (GENERIC_PLAN)` (`<scratch>/c/pg_payerdue.sh`).
  - The first run hung on its first psql call (`show server_version`). psql opened its pager on the pty that ansible allocates, and waited (`wchan=do_wait` on a child `sh`). I stopped my own job, and checked that no process of mine was left on edge-2.
  - The corrected rerun (pager off, stdin from /dev/null) was refused by the permission classifier. I did not retry it. The plan and the database CPU of that read are therefore inferred from code, task metrics and logs (§2.6).
- **Another session's psql on edge-2:** pid 1652842 (`timeout 60 sudo -u postgres psql … SELECT count(*) FROM pg_inherits …`) has been stopped (state T) for about 3.5 days, with a stopped child `sh` (likely the same pager hang) and an open socket. It is not mine and I did not touch it. The owner may want to clear it.
- **Fetching origin:** `git fetch` of the shared server checkout failed (GitHub SSH). I used local refs, so "on origin/main" means the local view, 403fa4b39 (committed 11-04 08:17Z).

### 2. Where the CPU goes now

#### 2.1 Service totals (cores; Mimir, 30-minute grid; `<scratch>/c/daily_peaks.txt`)

| Day | api mean / max | api krps mean / max | api cores per krps | taskworker mean / max | alt mean / max |
|---|---|---|---|---|---|
| 10-29 | 14.41 / 18.91 | 7.96 / 10.88 | 1.81 | 10.62 / 14.39 | 0.54 / 0.68 |
| 10-30 | 14.05 / 18.25 | 9.76 / 12.76 | 1.44 | 11.29 / 12.96 | 0.50 / 0.74 |
| 10-31 | 14.06 / 19.42 | 10.14 / 13.88 | 1.39 | 11.61 / 12.95 | 0.44 / 0.55 |
| 11-01 | 17.05 / 20.60 | 11.42 / 16.02 | 1.49 | 10.55 / 12.28 | 0.57 / 0.74 |
| 11-02 | 14.05 / 18.61 | 9.25 / 12.87 | 1.52 | 12.85 / 15.06 | 0.52 / 0.65 |
| 11-03 | 13.27 / 16.79 | 9.51 / 12.76 | 1.40 | 12.41 / 14.18 | 0.45 / 0.62 |
| 11-04 to 10:30Z | 16.56 / 18.95 | 14.17 / 15.81 | 1.17 | 12.28 / 13.72 | 0.69 / 0.80 |

Evening peaks, 18–22Z (requests per second are 30-minute means):

| Evening | api cores mean / max | krps | cores per krps | replays | audits | real creates | taskworker | alt |
|---|---|---|---|---|---|---|---|---|
| 10-31 (before D3) | 18.33 / 19.42 | 13.25 | 1.384 | 6,118 | 2,636 (full path) | 2,223 | 12.21 / 12.67 | 0.51 |
| 11-01 (D3 on beta) | 16.77 / 18.74 | 10.64 | 1.576 | 3,150 | 3,623 | 1,898 | 11.78 / 12.28 | 0.57 |
| 11-02 (D3; connect restarted in the window) | 15.48 / 18.61 | 9.89 | 1.564 | 2,688 | 3,080 | 2,075 | 13.20 / 15.06 | 0.53 |
| 11-03 (D3) | 15.47 / 16.79 | 12.12 | 1.276 | 5,007 | 3,000 | 1,867 | 12.32 / 14.18 | 0.49 |
| 11-04 10:00Z (morning) | 18.74 | 15.81 | 1.185 | 8,284 | 3,099 | 2,226 | 13.10 | 0.78 |

**The closest like-for-like pair is 10-31 against 11-03:** connect uptimes of 17.5 and 24.5 h, and 13.3 against 12.1k req/s.
- api cores fell from 18.3 to 15.5 (−16%) for 8.5% fewer requests.
- Bottom-up *(inferred)*: about 1.6 cores of that is D3 (3.0k audits/s at about 0.55 ms each). About 0.6 is fewer replays, and about 0.7 fewer real creates.

**This morning is the highest api load in the window.** Traffic and the replay creep are both at their highest. Unless connect restarts, tonight's peak will see about 10k replays/s or more: 8.3k/s at 10Z plus 200–300/s per hour *(inferred from the creep's slope, §2.3)*.

#### 2.2 api per host and per request

Measured over 30 minutes at 10:30Z on 11-04, g-blocks only. CPUOPT3's 10-31 18:35Z figures are in brackets.

| Host (CPU) | Cores | req/s | Cores per krps |
|---|---|---|---|
| edge-0 (Platinum 8168) | 2.55 | 1,787 | 1.43 [1.49–1.54] |
| edge-1 (E5-2697 v4) | 3.31 | 1,783 | 1.86 [2.13–2.17] |
| edge-3 (Gold 6154) | 6.20 | 5,848 | 1.06 [1.15–1.18] |
| edge-4 (Gold 6154) | 6.55 | 6,148 | 1.07 [1.21–1.22] |

**Memory and GC.** Heap in use is 3.7–5.7 GB and next GC 6.2 GB, unchanged. The 8 busy processes (edge-3/4 g-blocks) collect 0.94–0.97 times a minute. The other 12 are at the 2-minute forced-GC floor (0.49–0.52 per minute). Allocation is 34 MB/s per g-process.

**Round trips.** Redis pool hits are 25.2k/s, 1.57 per request; on 10-31 at 19:00Z they were 25.6k/s, 1.98 per request. PostgreSQL acquires are 13.5k/s, 0.85 per request (0.82 then). So D3 took about 6k Redis round trips/s off the api, and the database path is unchanged.

#### 2.3 `/connect/control`: what it carries, what it costs, the sawtooth

**At 10:00Z on 11-04** (30-minute rates; `<scratch>/c/grid_api_30m.tsv`), the route carried 13,814 req/s, 87% of api requests:

| Class | Rate | Share of control | CPU per request | api cores |
|---|---|---|---|---|
| Refusal replay: 7,042 `inactive_destination`, 1,242 `missing_companion_origin` | 8,284/s | 60% | 0.46–0.52 ms (regression, before and after D3; CPUOPT3: 0.40–0.57) | 3.8–4.3 |
| PeerAudit discard: 99.4% `abuse=none`, 100% `bearer=present` | 3,099/s | 22% | 0.1–0.15 ms (profile) | 0.3–0.5 |
| Real create (`create_contract` frames minus replays) | 2,226/s | 16% | 1.9–2.4 ms (profile); the regression gives 3.3–4.3 with GC and scheduler folded in | 4.2–5.3 |
| Close and provide | 172/s | 1.2% | about 1–2 ms *(inferred)* | 0.2–0.3 |

So 17% of control requests do real work, against about 22% on 10-31. They take 23–30% of api CPU. The rest of api CPU is:
- replays, about 22%;
- discards, 2–3%;
- per-process overhead (GC, origin-notification subscriptions, sysmon, `runPosts`) and the other routes (`find-providers2` at 18/s and about 23 ms).

**Per-class costs, bottom-up.** From the edge-4 capture (1,650 req/s), using samples ÷ 99 Hz as CPU (`<scratch>/c/perf_api_edge4_g2.txt`):
- **A real create** spends 1.37 ms in `controller.CreateContract`. That includes the escrow in `waitForCompanionOrigin` (7.0% of samples) and the Redis contract admission (6.9%). Add about 0.27 ms of JWT verification and 0.14 ms of `PrepareConnectControl`.
- **A replay** pays the gate decode, the verification, admission (one `EVALSHA`) and the ring lookup (0.04 ms).
- **A discard** pays the gate decode (0.09 ms), the body read and decode, and `DiscardConnectControlPack` (0.012 ms).

The regression is in `<scratch>/c/regress_api.txt`. It pins only the replay cost robustly; the other classes are collinear with the fixed costs.

**The sawtooth.** Replays per second against connect uptime, per connect generation (`<scratch>/c/sawtooth.txt`):

| Connect generation from | Length | +2 h | +6 h | +12 h | +18 h | +24 h | End | Slope | api cores at the end |
|---|---|---|---|---|---|---|---|---|---|
| 10-29 07:05Z | 19.4 h | 566 | 1,646 | 3,626 | 5,044 | – | 5,153 | 279/s per h | 14.6 |
| 10-30 03:52Z | 22.1 h | 778 | 2,596 | 5,040 | 6,308 | – | 7,596 | 347 | 15.6 |
| 10-31 02:36Z | 29.4 h | 488 | 1,776 | 4,014 | 6,353 | 7,493 | 8,073 | 282 | 20.6 |
| 11-01 08:15Z | 15.8 h | 511 | 1,852 | 3,155 | – | – | 3,857 | 253 | 15.3 |
| 11-02 00:35Z | 18.6 h | 345 | 1,104 | 2,755 | 5,084 | – | 5,084 | 298 | 17.7 |
| 11-02 19:38Z (current) | 38.9 h | 813 | 2,052 | 1,109 | 3,181 | 5,003 | 8,284 | 205 | 18.7 |

**What the 11-03 restarts did.** Since 11-02, the restarts were of api, alt, the proxy and the taskworker. Connect was not restarted.
- The api restarts at 11-03 05:30Z and 08:28Z dropped replays to about 0.5–1.1k/s for an hour or two. The +12 h cell above is one of those dips. That is the refill of each api process's refusal ring, and those requests take the full create path meanwhile.
- The 22:08Z restart showed no dip: 5.2k/s before, 5.8k/s 40–130 minutes after.
- The creep then went on: 6.1k/s at 04Z, 6.8k at 08Z, 8.3k at 10Z.

**What it costs.** At 0.5 ms per replay, the creep is about 3.7 api cores above a fresh generation now. It also adds the same number of Redis admission `EVALSHA`s, about 0.7 Redis cores at Part E's 0.09 cores per 1k round trips/s *(inferred)*. Each replay also passes nginx on a services edge.

**The fix exists and is off.**
- Server 7e4c66e31 and a5fca11a8 (11-01) add a per-api-process tracker of providers stuck retrying terminal refusals, and a connect-side recycle. Both are in d5cbc2a72 and in the connect service's 1e94b4d78.
- The api has the code ceiling on (api/run.go:288–292). But `urnetwork_connect_stuck_provider_signal_enabled` is 0 on every api and alt block, and `…recycle_enabled` is 0 on every connect block. `…tracked_clients` is 0. See #1.

#### 2.4 Inside an api process

Two captures (`<scratch>/c/perf_api_edge4_g2.txt`, `<scratch>/c/perf_api_edge0_g2.txt`). Shares are inclusive, of samples.

| Component | edge-4 g2, busy (1,650 req/s) | edge-0 g2, light (439 req/s) | CPUOPT3 (edge-3) |
|---|---|---|---|
| JWT verify, `ParseByJwtForAudience` (session/by_jwt.go:468, 422) | 10.5% | 7.3% | 10.5–11.7% |
| P-256 samples outside any ECDSA or JWT frame (cut-off stacks) | about 4.5% | 2.6% | about 4.5% |
| Lag-guest unverified decode, `requestLagGuestCredential` → `ByJwtLagGuestUnverified` (router/lag_guest_credential.go:50, by_jwt.go:765) | 5.9% | 4.6% | 4.4–5.0% |
| **JWT total** | **about 20.9%** | **14.5%** | **about 19%** |
| go-redis, including its syscalls | 12.1% | 12.2% | 12–15% |
| Per-client admission, `checkConnectControlLimit` `EVALSHA` | 6.1% | 3.6% | 5.8–5.9% |
| Revocation and state checks | 1.6% | 1.9% | 2.5–2.9% |
| `CreateContract` | 14.2% | 10.3% | 11.2–13.5% |
| Its companion-origin wait and escrow | 7.0% | 4.4% | 4.6% |
| Refusal-replay lookup | 1.6% | 1.2% | – |
| `DiscardConnectControl` itself | 0.16% | 0.14% | – |
| `FindProviders2` | 1.3% | 7.2% | 1.8–2.0% |
| Origin-notification subscription (`runBucket`) | 2.5% | 3.7% | 3.0–9.3% |
| GC mark | 6.9% | 11.6% | 5.1% |
| `mallocgc` | 9.2% | 10.1% | 6–8% |
| Scheduler | 20.5% | 21.0% | 13–14% (`findRunnable` only) |
| Its `findRunnable` | 12.9% | 11.2% | |
| Its `runtime.sysmon` | 5.5% | 7.4% | not separated |
| pgx | 7.1% | 5.8% | 6–7% |
| encoding/json | 8.4% | 7.2% | 7.1% |
| prometheus | 2.7% | 2.3% | 2.3–2.6% |
| fmt / `encodeUuid` and `parseUuid` | 1.1% / 1.1% | 1.4% / 1.1% | 1.5–1.9% |
| TLS and x509 | 0 | 0 | |
| Network softirq on the process's threads (host work) | 13.8% | 8.1% | |

**Fleet-weighted** by host CPU (edge-3/4 are 65% of api cores), JWT is about 19% of samples, about 21% of process CPU, or 3.5–3.9 cores at 10Z. Per authenticated request, verification costs about 0.27 ms on edge-4: 1,309 samples over 40 s against 1,210 verified req/s. The lag-guest decode costs 0.09 ms per bearer request (0.15 on edge-0).

**The discards removed about 3.1k verifications/s, yet the JWT share held.** The replays, which verify, grew, and the gate's decode now runs on more requests than the verifications do.

**The scheduler is about a fifth of api CPU.** That includes `sysmon`, at 5.5–7.4%. This is a syscall-heavy, short-request process: on edge-4 its threads wait 0.56–0.59 s on the run queue per CPU-second, against 0.06 on edge-0 (`<scratch>/c/sched_delay_edges_1.txt`).

#### 2.5 Redis and database round trips (api)

See §2.2. Each replay is one admission `EVALSHA`, and each real create about 2.4 pool acquires and the reservation scripts. Discards touch neither. The origin-notification broadcast now delivers about 0.94k edges/s to each of 22 processes. That is about 18.8k/s to the api and 1.9k/s to alt (alt: `urnetwork_contract_origin_notifications_total`).

#### 2.6 taskworker

**Per process.** Cores against probe attempts over one hour (`<scratch>/c/tw_probe_regression.txt`):

| When | taskworker cores | Attempts/s | Fit: cores per process at no probe | Fit: probe CPU per attempt | edge-1 per attempt |
|---|---|---|---|---|---|
| 10-31 19:25Z (CPUOPT3) | 12.71 | 62.4 | 0.69–0.71 | 106–113 ms | 234 ms |
| 11-03 21:00Z | 10.58 | 63.3 | 0.16–0.42 | 115–138 ms | 230 ms |
| 11-04 10:55Z | 13.31 | 75.2 | 0.12–0.16 | 147–160 ms | 209 ms |

**Over 24 h** ending 10:30Z on 11-04, all-in, there were 73.5 attempts/s at 168 ms of taskworker CPU each. Over the 24 h ending 10-31 19:25Z it was 57.3/s at 197 ms. By host, the 24-h all-in cost is 149–163 ms on edge-0/3/4 and 209 ms on edge-1 (Broadwell).

**The probe is now 85–90% of taskworker CPU:** 11–12 cores. CPUOPT3 measured at least 55%, 7 cores. Two things changed:
- **Attempts rose 21%.**
- **The non-probe floor fell from about 0.7 to 0.12–0.16 cores per process.** `ApplyLegacyProviderTotals` ran at 618–880 executions/s on 10-31 and is gone; `DrainLegacyProviderTotals` runs 514 times an hour. Today's largest tasks by count are `ProviderIntentCheck` (17/s), `RollupProviderEgressTallies` (9.2/s) and `FlushTransferDebits` (4.2/s).

**Inside a probe-running process.** edge-3 g2 at 2.74 cores and 20.5 attempts/s (`<scratch>/c/perf_tw_edge3_g2.txt`):

| Component | Share | Notes |
|---|---|---|
| connect client stack | 66.2% | the probe tunnels' own connect clients |
| TLS, client handshakes | 38.7% | uTLS 27.2%, stock crypto/tls 6.6% |
| x509 `Certificate.Verify` | 28.1% | 23.7% through uTLS `verifyServerCertificate` on Chrome-hello platform and WebSocket dials, which `ClientStrategy.parallelEvalWithRouteHedge` makes per route (29.3%); 4.6% through crypto/tls on HTTP API dials (`persistConn.addTLS`) |
| P-384 (fiat), RSA | 24.2%, 6.3% | `fiat.p384Mul` alone is 11.1% |
| In-process control, `providerEgressControl.ConnectControl` → `ConnectControlFrames` | 9.6% | `CreateContract` 4.5% |
| probe package frames (qualityprobe, providertunnel, egresshealth) | 22.7% | |
| GC, scheduler | 6.5%, 7.3% | 6–7 GC cycles a minute on probe processes |
| task framework, `updateFunctionName` | 4.5%, 0.16% | |

**x509 per attempt** is about 4,849 samples ÷ (99 × 60 s) = 0.82 cores, over 20.5 attempts/s: 38–40 ms. CPUOPT3 had about 42 ms. Fleet-wide that is 2.8–3.0 cores at 75 attempts/s, about 22% of taskworker. All TLS together is about 52–55 ms per attempt, 3.9–4.1 cores. No chain cache exists in connect 49f285f1e or 0dad308f8 (net_tls_hello.go:129, :275). Each tunnel still builds its own control-plane strategy (qualityprobe/providertunnel/tunnel.go:522, unchanged since a8461e236).

**A process without a probe pass.** edge-4 g1, 0.19 cores (`<scratch>/c/perf_tw_edge4_g1.txt`): task framework 35.7%, GC 35.4%, pgx 23.6%, `FlushTransferDebits` 12.6%, the `updateFunctionName` regexp 4.5%. Heaps swing between 1 and 7 GB per process with task activity. They do not grow, so there is no leak signal.

**`ClosePayerDueContractsTask`** (`<scratch>/c/grid_cpdc_1h.tsv`, `<scratch>/c/tw_logs_payerdue2_edge4.txt`, `<scratch>/c/tw_logs_payerdue2_edge1.txt`, `<scratch>/c/tw_logs_payerdue_stack_edge4.txt`):
- **Rates:**
  - 11-04 09Z: 313 runs/h, 235 errors/h, all `cause=deadline`, attribution system.
  - The errors started at 10-31 03Z at 42/h. They were 32–88/h from 11-01 to 11-02 12Z, and 132–185/h from 11-02 15Z to 11-04 06Z.
  - Wall time is 5,395 s/h, so about 1.5 runs are in flight on average.
- **Every error is the page read hitting `ReadTimeout`:** `ReadPayerDueContracts` → `server.Db` → "timeout: context deadline exceeded". Each failed eval lasts 20.0 s (min 20.0, max 20.1). Settings are at model/subscription_payer_due_close.go:85, the read at :115–127.
- **The same five payers fail on both hosts sampled,** at about 45 runs/h each. That is 20 s per run plus a reschedule within a minute: `task.WithErrorRetryCap(…, time.Minute)` at :448, and "A failed read retries the run" at :452.
- **The successful runs take 8.9 s on average** ((5,395 − 235 × 20 s) ÷ 78). Every result line in the 2-hour logs of four of the eight processes is `due=0 closed=0 already_closed=0 busy=0 failed=0 more=false`.
- **Cost:**
  - taskworker CPU is about 0 (no samples in either capture).
  - PostgreSQL: about 1.3 connection-seconds per second in reads cancelled at 20 s, plus 0.2 in reads that find nothing. That is up to about 1–1.3 edge-2 cores if the time is spent executing from shared buffers, rather than queued for a connection or on I/O *(inferred; the edge-2 capture was refused, §1)*.
  - Each failure also logs a full stack trace ("Unexpected error").
- **Why it fails** *(inferred from code; the plan was not captured)*:
  - The read is `… WHERE payer_network_id=$1 AND <unresolved> AND (expiration_time<=$2 OR (expiration_time IS NULL AND create_time<=$3)) LIMIT 4096`, with no order.
  - Its index is `transfer_contract_unresolved_payer_transfer_byte_count (payer_network_id) INCLUDE (transfer_byte_count) WHERE <unresolved>` (db_migrations.go:7246–7257). That index has no `expiration_time`, so every candidate needs a heap visit. When a payer's unresolved contracts are mostly not yet due (live contracts expire within 60 min, `DefaultContractExpiration`), the LIMIT is never reached and the scan visits all of them.
  - The 10-28 comment's 42 ms was measured for a payer whose page filled from a large backlog of due contracts.
  - The `due=0` runs fit this: their payers have only live contracts.
  - A drain is requested when "a balance read whose Pending exceeds what its current grants reserve" passes the 2-minute Redis gate (:391). For these payers the request does no work.

**`FlushTransferDebits` also logs deadline stacks:** 166 in 2 h on edge-4 g1. These are its 15 s page bound. The page keeps its cursor, so no work repeats (model/subscription_transfer_debit.go:106, :186–196). I mention it as a symptom of the database's latency, for Part D.

#### 2.7 alt

**Cost.** In the crisp capture, alt ran 0.41 cores (`<scratch>/c/perf_alt_crisp.txt`):

| Component | Share | CPUOPT3 |
|---|---|---|
| quic-go | 35.5% | 26% |
| TLS 1.3 server handshakes on QUIC (`QUICConn.Start`) | 9.4% | 5% |
| Their hybrid ML-KEM key exchange | 4.1% | |
| go-redis | 19.5% | 26% |
| `PubSub.ReceiveTimeout` | 14.3% | |
| Its `Channel()` readers (`initAllChan`) | 13.1% | about 16% |
| Origin `runBucket` | 4.7% | 6.5% |
| Scheduler | 25.3% | 16–19% |
| Its `sysmon` | 6.0% | |
| api handlers | 3.1% | 6–7% |
| Rate limiter | 3.1% | |
| GC | 0 (no cycle in 30 s) | 4–7% |
| Network softirq | 10.3% | |

The go-redis push-notification peek before each pub/sub read (`processPendingPushNotificationWithReader`, 6.6%) is where the blocking read happens. It is not extra work.

**Traffic changed with alt's releases,** on both hosts and all status 200: 62–95 req/s until 11-02 15:33Z, 36–47 until 11-03 22:18Z, 68–80 since.

**Latency** (`<scratch>/c/alt_latency_decomp.txt`). Each route's contribution is its duration-sum rate over alt's total count rate:

| Time (30-minute windows) | Mean | Control route | `/subscription/balance` | Other | Discard residence | Balance mean | crisp busy | alt cores |
|---|---|---|---|---|---|---|---|---|
| 10-31 20:00Z | 9.3 ms | 6.7 | 0.1 | 2.5 | – (before D3) | 23 ms | 38% | 0.54 |
| 11-03 10:00Z | 20.6 | 12.3 | 0.1 | 8.2 | 2.3 ms | 16 ms | 29% | 0.37 |
| 11-04 02:00Z | 16.6 | 14.8 | 0.0 | 1.8 | 13.1 ms | 22 ms | 26% | 0.49 |
| 11-04 04:00Z | 13.9 | 11.9 | 0.0 | 1.9 | 12.2 ms | 28 ms | 99% | 0.78 |
| 11-04 06:00Z | 16.2 | 12.8 | 0.1 | 3.2 | 11.2 ms | 37 ms | 96% | 0.76 |
| 11-04 08:00Z | 32.8 | 14.1 | 16.0 | 2.6 | 12.8 ms | 3,646 ms | 96% | 0.76 |
| 11-04 10:00Z | 27.4 | 15.0 | 9.0 | 3.4 | 11.6 ms | 2,418 ms | 96% | 0.78 |

- **The jump to 27–33 ms is `/subscription/balance`:** 0.30–0.34 req/s at 2.4–3.6 s each. The api's same route rose from 12–21 ms to 529–856 ms over the same hour, and it reads the payer's unresolved-contract index as the payer drain does (§2.6). This is the database (Part D).
- **The control route's 11–15 ms is mostly discards,** at a handler residence of 10.7–13.1 ms. It was 1.1–4.5 ms before the 11-03 22:18Z release, and is 0.063 ms on the api.
  - The residence includes reading the body (router/control_observation.go). alt reads it straight from the client's connection; the api reads it from nginx's buffer.
  - It was already 10.7–13.1 ms at 00–02Z with crisp 26–27% busy, when the other half of alt's clients came back. So it is client network time, not CPU.
- **Saturation did not move the mean.** From 02Z (26% busy) to 04–06Z (96–99% busy), the mean went from 16.6 ms to 13.9–16.2 ms.

**CPU starvation** (`<scratch>/c/sched_delay_crisp_fireside_1.txt`, 10:30Z):
- **The hosts:** crisp averages 40.5 runnable tasks on 24 CPUs (max 55), fireside 40.9. CPU PSI "some" is 49–52%.
- **alt:** it waits on the run queue 2.15–2.18 s per CPU-second, but in 17–18k slices/s: 48–49 µs of wait per slice, against 22 µs of run.
- **The proxies:** they wait 0.59–0.67 s per CPU-second, in 238–308 µs slices.
- **No throttling:** no container has a CFS quota, and none was throttled.
- **`/hello` on alt** was 0.21 ms in the saturated hours, against 0.13–0.22 ms on 11-02 and 11-03 (`<scratch>/c/mimir_misc.txt`). A request needs a few wakeups, so queueing adds about 0.1–0.4 ms per request *(inferred)*: under 2% of the mean.

**Where saturation does show is alt's CPU per unit of work.** From 02Z to 04Z alt went from 0.49 to 0.78 cores at a flat 68–80 req/s. Origin notifications rose 35–45%, about +0.04 cores at about 40 µs each (CPUOPT3 C #4). The remaining +0.24 cores is the same work taking about 1.5 times as long on a saturated host (SMT siblings and caches) *(inferred)*.

#### 2.8 GOGC and GOMAXPROCS

**api GOGC 400.** It would move the 8 busy processes from about 0.95 collections a minute to the 2-minute floor, about −47% cycles. With GC at 6.9% of a busy process, that is about 0.05 cores each, about 0.4 cores in all (about 2% of api; CPUOPT3: about 1%). The cost is about 9 GB more heap per busy process (next GC at about 15 GB instead of 6.2 GB) *(inferred)*. The light processes gain nothing.

**api GOMAXPROCS 8 → 4.** The scheduler is about 21% of samples, `findRunnable` 11–13% and `sysmon` 5.5–7.4%. Five processes with 8 Ps each share 72 CPUs with connect (5 × 16 Ps) on edge-3 and edge-4, where api threads wait 45 µs per wakeup. The A/B (CPUOPT3 C #9, owner decision 6) is still the way to find out. Expect 2–5% of api *(inferred)*.

**alt and taskworker.** alt has GOMAXPROCS 4 and an 8 GiB memory limit, with no GC in its capture; nothing to change. The taskworker has GOMAXPROCS 4. Its probe processes collect 6–7 times a minute, at 6.5% GC; not a lever on its own.

#### 2.9 For other parts

- **Part B:** proxy discards read 0. The proxy's `unhandled_message` failures, its in-process audits at 1.4–3.4k/s, went to 0 with its 11-01/02 release. That is connect's `SendPeerAudits` default working, not the discard.
- **Part D:**
  - The payer drain's reads (§2.6) and the `/subscription/balance` slowdown at 08–10Z on 11-04 read the same payer index.
  - `FlushTransferDebits` hits its 15 s page bound about 80 times an hour on one process.
- **Part E:** every replay is one Redis admission `EVALSHA`, about 8.3k/s now.
- **Part A:** the connect recycle side of #1 is a connect-service switch. D1's `localTransport` is off on every connect block (`urnetwork_connect_exchange_local_transport_enabled` = 0).

### 3. What CPUOPT3's shipped changes did

**D3, server side (c0c310a1b, e66f57589; in d5cbc2a72 and earlier builds).**
- **Rollout:**
  - 11-01 15:40Z: the beta block (+1087474430), 139–289 discards/s.
  - 11-02 05:59–06:17Z: the whole fleet (+1088438700).
  - 11-02 13:09Z: alt.
- **`unhandled_message`** failures at the api went from 1.9–3.7k/s to 0 at the fleet roll. Discards took their place at 2.6–3.9k/s.
- **Step test.** api cores 90 minutes before against 40–130 minutes after each api restart, adjusted for the mix change at 0.5 ms per replay and 3.9 ms per real create (`<scratch>/c/restart_effect.txt`):

  | api restart | Before: cores, krps, replays/s, audits/s | After | Δ cores | Residual |
  |---|---|---|---|---|
  | 11-02 05:59Z, the D3 fleet roll | 13.10, 7.90, 842, 3,873 | 11.53, 8.32, 1,418, 3,473 | −1.58 | −2.26 |
  | 11-02 12:40Z | 14.97, 10.50, 2,710, 3,630 | 15.58, 10.90, 3,485, 3,199 | +0.61 | −0.50 |
  | 11-02 15:18Z | 15.58, 10.90, 3,485, 3,199 | 17.15, 11.82, 4,505, 3,002 | +1.57 | −0.07 |
  | 11-03 00:50Z | 13.15, 8.15, 1,708, 3,026 | 13.71, 8.80, 2,331, 2,910 | +0.56 | −0.24 |
  | 11-03 05:30Z | 12.67, 7.71, 1,458, 2,714 | 12.37, 7.86, 960, 3,154 | −0.30 | −0.47 |
  | 11-03 08:28Z | 12.31, 7.98, 1,119, 3,106 | 11.32, 8.07, 1,885, 2,832 | −0.98 | +0.29 |
  | 11-03 22:08Z | 15.01, 12.11, 5,181, 3,023 | 13.25, 12.27, 5,789, 3,071 | −1.76 | −0.42 |

  Restarts that changed no hot path leave −0.5 to +0.3 cores. The D3 roll leaves −2.26, or −2.1 at a real-create cost of 2.1 ms.
- **Measured against predicted:**
  - D3 saved about 2.0 ± 0.4 cores at 3.5–3.9k audits/s, about 0.55–0.65 ms per audit. The plan predicted 85% of 0.6–0.75 ms, which is 0.5–0.64 ms per audit, or 1.3–1.7 cores at 2.6k/s.
  - At the evening audit rate of about 3.0k/s, it is worth 1.6–1.9 cores.
  - The remaining cost is 0.1–0.15 ms per discard, 0.3–0.5 cores, mostly the lag-guest gate's decode. The plan expected about 0.26 cores at 2.6k/s.
- **Redis:** api pool hits per request fell from 1.98 to 1.57, about −6k/s at today's discard rate. The plan predicted −5.3k/s at 2.6k/s.
- **Confidence:** medium–high. The confounders are the api restart that carried D3 and the mix change across it. Both are bounded by the other restarts in the table.

**D3, client side (connect c034364a, `SendPeerAudits` default false).**
- It is in the connect pinned by every server build running now (0dad308f8 for api, alt and the proxy; 49f285f1e for the taskworker), and in the sdk tree, which builds against `../connect`.
- **Proxy:** its hosted window clients' audits went to 0 with the proxy's 11-01/02 release.
- **api:** discards show no decline: 3.66k/s at 11-02 08Z, 2.96k at 11-03 08Z, 3.16k at 11-04 08Z. Per the 11-02 release notes, the provider released on 10-27 predates c034364a. Providers get it from the next finished nightly (planned for about 11-05), and apps from their next releases.
- **Abuse reports:** `abuse=claimed` is 18/s (0.6%), all with a bearer. That sizes the 2-week abuse sample the plan asked for (D3 Q4).

**Stuck-provider recycle (7e4c66e31, a5fca11a8).** CPUOPT3 #17 asked for this. It is built, in api d5cbc2a72 and in connect's 1e94b4d78, and off on every block. The creep is unchanged (§2.3).

**D1, D2, and the CPUOPT2 timer-churn merge.** None touches these services' hot paths. D1's switch is off everywhere, so it saved nothing here.

### 4. Ranked opportunities

#### #1 Turn on the stuck-provider signal, then the recycle

**Change.** Use the built controls in the Redis move-control hash; no code.
1. Set `stuckProviderSignal` for one api block, then all. With the recycle off, this only measures. It reports clients, at most 5/s per process, as one `SET NX` flag with a 1 h cooldown. The connect processes count them `disabled`.
2. Read `urnetwork_connect_stuck_provider_tracked_clients` and `…recycle_decisions_total{result}` for 24 h. Size the stuck share of the replays.
3. Set `stuckProviderRecycle` on the beta connect block, then g1, then all.

**Code** (d5cbc2a72):
- controller/connect_control_stuck_provider.go: settings at :190–204 (20 min, 6 answers, 5 min gap, 1 h cooldown, 5/s with a burst of 20).
- api/run.go:288–292.
- model/connect_move_control.go:97–98.
- connect/client_recycle.go (server repo): 4 workers at 5/s, a moved close, a drain excuse first.

**Evidence.**
- 8.3k replays/s at 38.9 h of connect uptime, climbing 205–347/s per hour in every generation (§2.3). 85% of them are `inactive_destination`, the terminal class the signal counts.
- The commit attributes the creep to providers on connect builds before v2026.9.8 that never act on a `CreateContractResult` error.

**Saving** *(inferred)*:
- If the stuck share is most of the growth above a fresh generation's 0.3–0.8k/s, at 0.5 ms per replay:
  - 1–2.8 api cores at the evening peaks of 10-31 to 11-03 (2.7–6.1k replays/s);
  - 1–2 averaged over a 16–30 h generation;
  - 3.7 at today's 8.3k/s.
- Plus the same count of Redis `EVALSHA`s (0.3–0.7 Redis cores) and their nginx and connect handling.
- Step 1 gives the real number before anything is recycled.

**Risk.** Medium. A recycled provider reconnects, as it would on a connect deploy. The design writes the drain excuse, so reliability does not count it. The pacing (5/s per process, the queue bound, the cooldown) limits the burst.

**Why no timing bound moves.**
- The refusal answers, the refusal ring, the per-client limit and every deadline stay as they are.
- The thresholds are the code defaults. §6 rejects lowering them.
- The recycle uses the existing moved close.

**Validate:**
- `…recycles_total{outcome}` mostly `recycled`, with few `still_stuck`.
- Replays per second against connect uptime: the slope falls on the enabled blocks.
- api cores per krps.
- Redis `EVALSHA`s per second.
- Provider reliability and excused-drain counts.
- `urnetwork_connect_control_client_limit_total` unchanged.
- The switches restart nothing. Recycling does move provider connections, though, so enable it with the owner outside the validator's busy minutes.

#### #2 D4(a): cache verified JWT claims

**Change.** As designed in CPUOPT3 D4(a): a sharded LRU keyed by SHA-256 of the whole token, successes only, with single-flight, read in `parseByJwtSigned`. The lag-guest, client-credential and intended-network gates read it first.

**Code:**
- session/by_jwt.go:422 (`parseByJwtSigned`), :468 (`ParseByJwtForAudience`), :765 (`ByJwtLagGuestUnverified`);
- router/lag_guest_credential.go:50;
- session/by_jwt_keyring.go.

**Evidence.** JWT is 19–21% of api, 3.5–3.9 cores at 10Z (§2.4). The discards made the gate's decode a larger share of it.

**Saving.** On hits (≥90% *(inferred)*), the cache removes the verification, its claims decode and the gate's decode: 85–90% of JWT. That is 2.6–3.4 api cores at an evening peak of 16–19 cores, plus about 0.2 proxy (B). It also makes #5 unnecessary.

**Risk.** Low–medium: the auth path. D4's tests 1–9 cover it.

**Why no timing bound moves.** Expiry, the session horizon, revocation and state checks run on every request (D4(a)).

**Validate:**
- P-256 plus `ParseWithClaims` under 2% of api, a hit ratio above 90%;
- `urnetwork_auth_jwt_rejections_total` unchanged by cause;
- api cores per krps down at least 12% on g1 against g2–g4.

#### #3 D4(b): verified-chain cache in connect's TLS client

**Change.** As designed in CPUOPT3 D4(b), in connect net_tls_hello.go (`newChromeTlsConfig` at :129, `clientTlsHandshaker.handshake` at :275). Eligible are pinned-pool configs only. The cache key is the chain bytes, the server name and the root identity. Key possession, hostname and validity are checked on every handshake.

**Evidence** (§2.6):
- x509 is 28.1% of a probe-running taskworker, about 38–40 ms per attempt, 2.8–3.0 fleet cores at 75 attempts/s.
- The verifications come from the Chrome-hello platform and WebSocket dials, which route hedging makes per route, and from the stock-TLS API dials.

**Saving.**
- 2.2–2.7 taskworker cores at an evening peak of 63–75 attempts/s.
- Edge-1's probe attempts gain most: 209 ms all-in against about 150–160 elsewhere.
- Plus 0.5–0.7 proxy cores (B), and apps and providers later.
- The stock-TLS API dials (4.6%) need the same cache in `newClientTlsConfig` (net_tls.go:40) or stay outside it; decide in review.

**Risk.** Low, but this is crypto code. D4(b)'s tests 1–7 cover it.

**Why no timing bound moves.** It runs inside the handshake, under its existing deadline. The probe's target TLS, which measures exits, is ineligible and unchanged (D4(b), "Probe correctness").

**Validate:**
- taskworker `crypto/x509` share under 1%;
- `urnetwork_egress_probe_attempts_total{result}` and `…tls_authentication_failures_total` unchanged between A/B blocks;
- taskworker cores per attempt.

#### #4 Payer drain: a page read that can stop, and a backoff for one that cannot

**Change**, in three parts.
- **(a) The fix.** Give the page read an index it can stop on. Add a partial index with `expiration_time`: `(payer_network_id, expiration_time) WHERE <unresolved>`, plus the legacy `expiration_time IS NULL` rows by `create_time`. Then a page read examines only due rows, at about a page of index work whatever the payer's live count.
- **(b) An interim, the owner's choice.** Back off a payer whose read keeps timing out. Today "a failed read retries the run" within a minute, forever, and the payer's activity requests a drain again every 2 minutes through the gate (:391), so ending the run alone would not cut the reads.
  - After two consecutive read timeouts for one payer, end the request (as at :450–452).
  - Lengthen that payer's gate TTL: double it per further consecutive timeout, from 2 minutes up to a cap of about 30 minutes.
  - Reset on any read that finishes.
  - Count each backoff in a `read_timeout` counter.
- **(c) Check why the requests fire.** Every finished run found nothing due. The balance-read trigger ("Pending exceeds what its current grants reserve") seems to fire for payers whose excess is live contracts *(inferred)*.

**Code** (9e2dcaeed):
- model/subscription_payer_due_close.go: :85 (settings), :115–127 (`payerDueContractsSql`, `ReadPayerDueContracts`), :219 (`ClosePayerDueContracts`), :391 (`requestPayerDueContractClose`), :448 (`WithErrorRetryCap`), :454 (`ClosePayerDueContractsTask`);
- db_migrations.go:7246–7257 (the current index).

**Evidence** (§2.6):
- 235 errors/h, every one at exactly 20 s, from five payers on every sampled host.
- 5,395 s/h of task wall time, and no closes in any sampled result.

**Saving.**
- About 1.3–1.5 PostgreSQL connection-seconds per second, up to about 1–1.3 edge-2 cores if that time is spent executing rather than waiting for a connection *(inferred; edge-2 not measured)*.
  - (a) removes nearly all of it, since each run becomes one short index range scan.
  - (b) alone cuts a stuck payer from about 45 reads an hour to a few.
- Plus 150–235 stack-trace log lines an hour.
- (a) also lets the drain work for these payers when they do have due contracts.

**Risk.** Low–medium.
- (a) is an index on `transfer_contract`, a hot, billion-row table. Weigh its write cost and disk with Part D, and build it with `CREATE INDEX CONCURRENTLY` in an online migration.
- (b) delays the next drain of a payer whose reads keep timing out. Such a payer gets nothing from the drain today.

**Why no timing bound moves.**
- `ReadTimeout` stays at 20 s and `RunBudget` at 1 min. §6 rejects raising the timeout.
- The drain only speeds things up. Every due contract is still retired by the deadline pass and the expiry sweep (the header of subscription_payer_due_close.go), which neither part touches.
- (b) changes only how soon a read that timed out twice in a row runs again. No timeout, deadline, probe, detection or revocation bound depends on it.

**Validate:**
- a DB test with a payer holding 100k live contracts and 10 due ones: the page read returns the 10, within a bound on index tuples, under `EXPLAIN` (plain);
- `urnetwork_taskworker_execution_errors_total{task="model.ClosePayerDueContractsTask"}` under 1/h;
- the five payers' `Pending` against their reservations;
- edge-2 backend time from `pg_stat_statements`, which then completes rather than cancels.

#### #5 Read the lag-guest claim without decoding the whole claims set

**Change.** `ByJwtLagGuestUnverified` (session/by_jwt.go:765) runs golang-jwt's `ParseUnverified` into the whole `ByJwt`: four ids parsed as UUIDs, the create time, the roles. It needs only `lag_guest`. Instead, base64-decode the payload segment and unmarshal it into ``struct{ LagGuest bool `json:"lag_guest"` }``. Return false on any error, as today.

**Code.** session/by_jwt.go:765; router/lag_guest_credential.go:50–66.

**Evidence.** 5.9% (edge-4) and 4.6% (edge-0) of api samples, about 1.0 core. It runs on every bearer request, including the 3.1k/s that are then discarded.

**Saving.** 0.6–0.8 api cores *(inferred; benchmark the two decoders first)*. Void if #2 ships.

**Risk.** Low. The gate refuses only a token that `ParseByJwtForAudience` then verifies as a guest, so the cheap check is only a filter.
- A token whose other claims fail to decode can now pass the filter. It then fails verification and goes to the handler, as today.
- A token claiming `lag_guest` in an escaped form is not one this server mints.

**Why no timing bound moves.** It touches no deadline, no gate decision and no verification.

**Validate:**
- table tests over guest, non-guest, API-key, malformed and escaped tokens, giving the same decision as the current function;
- `TestConnectControlRouteDiscardKeepsLagGuestGate`;
- perf share under 1.5%.

#### #6 Origin notifications: replace the broadcast (CPUOPT3 C #4)

**Change.** As in CPUOPT3 C #4: (a) the cheap step, then (b) a per-pair origin version read on missing-origin replays, so the 16-bucket subscriptions can close.

**Code.** model/contract_origin_notification.go:128, :310 (`runBucket`), and RunRedisShardSubscription; unchanged since a8461e236.

**Evidence.** 2.5–3.7% of api samples and 4.7% of alt's. About 0.94k edges/s go to each of 22 processes, about 20.7k deliveries/s. Missing-origin replays are 1.24k/s.

**Saving.** About 0.5 api cores, 0.04 alt, and the pub/sub fan-out on edge-5 and edge-6.

**Risk.** Medium. The refusal-ring fence logic must be re-derived, with ordering tests.

**Why no timing bound moves.** The companion-origin bounds (100 ms, 500 ms, 3 s) and the waits' PostgreSQL polls stay as they are.

**Validate:** as in CPUOPT3 C #4.

#### #7 GOGC 400 on the busy api blocks; the GOMAXPROCS A/B

**Change.**
- `GOGC=400` on the edge-3 and edge-4 g-blocks only. The others already collect at the 2-minute floor.
- Separately, `GOMAXPROCS=4` on one block on all hosts against the other g-blocks (CPUOPT3 C #9).

**Evidence** (§2.8):
- GC is 6.9% of a busy process at 0.95 cycles a minute.
- The scheduler is about 21% of samples, `sysmon` 5.5–7.4%, with run-queue waits of 45 µs per wakeup on edge-4.

**Saving.** About 0.4 cores, at about 9 GB more heap per busy process. The GOMAXPROCS A/B: 2–5% *(inferred)*.

**Risk.** Low. Revert if the control phase means or in-flight gauges rise.

**Why no timing bound moves.** These are runtime knobs, not deadlines.

**Validate:** api cores per krps by block, `go_gc_duration_seconds_count`, the phase means, and RSS.

#### #8 One control-plane strategy per probe process (CPUOPT3 C #5)

**Change, evidence and caveats** as in CPUOPT3 C #5 (tunnel.go:522). The handshake work beyond x509 is 10.5% of a probe process (38.7% TLS against 28.1% x509), about 1 core. This needs the owner's probe-design review: it changes which dials resume, and setup timings lose some CPU-bound handshake time.

#### #9 Small code items (CPUOPT3 C #6–#8)

- **`encodeUuid` and `parseUuid` without fmt** (connect id.go:169, unchanged): 1.1% of api and 0.8% of a probe process, about 0.2–0.25 api and 0.1 taskworker cores.
- **A precompiled `updateFunctionName` regexp** (task/task.go:1284, :1289, unchanged): 0.16–4.5% of a taskworker process, about 0.05 cores.
- **Pre-bound Prometheus children:** 2.3–2.7% of api, about 0.2 cores.

Each is very low risk. Validate with table tests and benchmarks.

### 5. CPUOPT3 predictions checked

**What I checked in git.** Server a8461e236..d5cbc2a72, a8461e236..9e2dcaeed, and d5cbc2a72..origin/main (local); connect 89b15920..0dad308f8 and ..49f285f1e.
- **No commits:** task/task.go, session/by_jwt_keyring.go, router/lag_guest_credential.go, model/contract_origin_notification.go, model/redis_shard_subscription.go, qualityprobe/providertunnel/tunnel.go, model/subscription_provider_totals_batch.go, search/search_local.go, and connect id.go.
- **D3 only:** controller/connect_control_limit.go, connect_controller.go, connect_control_discard.go, router/handler_utils.go.
- **The stuck-provider recycle:** model/contract_route_rejection.go, controller/connect_control_refusal_replay.go, api/run.go.
- **Docs only:** session/by_jwt.go (one auth-notes comment).
- **No chain cache:** connect net_tls*.go.
- **On local origin/main after d5cbc2a72,** none of these files changed.

| CPUOPT3 | Now | Verdict |
|---|---|---|
| C #1 / D3: 1.4–1.7 api cores, with the answer before authentication | About 2.0 ± 0.4 cores at 3.5–3.9k audits/s (step test); 1.6–1.9 at the evening audit rate; 0.1–0.15 ms left per discard | Met per audit; larger in total, because audits run at 3.0–3.9k/s, not 2.6k |
| D3: Redis −5.3k/s at the api | 1.98 → 1.57 pool hits per request, about −6k/s | Met |
| D3: discards decay as apps and providers update | No decline at the api (2.6–3.9k/s); proxy audits gone | Pending the provider nightly and app releases |
| C #2 / D4(a): JWT 19%, 2.2–2.9 cores | 19–21%, 3.5–3.9 cores at 10Z | Not built (deferred); larger |
| C #3 / D4(b): x509 17%, 2.3–2.5 taskworker cores | About 22% of taskworker, 2.8–3.0 cores at 75 attempts/s | Not built; larger |
| C §2.6: probe at least 55% of taskworker | 85–90% (11–12 cores) | Larger; the non-probe floor fell |
| C #4: origin notifications, about 1.0 core | 2.5–3.7% of api, about 0.5 cores (fewer edges per second) | Not built |
| C #5: shared probe strategy, about 0.6 cores | About 1 core of handshakes beyond x509 | Not built |
| C #6–#8: uuid, regexp, Prometheus | 1.1% / 0.16–4.5% / 2.3–2.7% | Not built |
| C #9: GOMAXPROCS A/B | Still 8 | Not done |
| C #10: live heap about 3.1 GB | 3.7–5.7 GB in use, next GC 6.2 GB | Unchanged |
| C #11: fewer round trips per create | 0.85 PostgreSQL acquires per request (0.82) | Not built |
| C #12: fewer api processes | 20 processes; beta 0.13–0.32 cores each | Unchanged |
| C #13: `ApplyLegacyProviderTotals` batches | The backlog drained | Moot |
| #17: stop the creep at its source | Recycle built and deployed, off; 8.3k replays/s at 38.9 h | Built, not enabled |
| GOGC 400 worth about 1% | About 2% (0.4 cores) | Small either way |
| api 1.41 cores per krps, only a fifth of control requests do real work | 1.19; 17% real | Cheaper per request (D3, mix); real share lower |
| alt 0.4–0.5 cores, 87% of its control requests audits | 0.45–0.57, 0.69–0.78 when saturated; 93% discards | As expected; saturation inflates its CPU, not its latency |

### 6. Considered and rejected

Under the owner rule ("We can't sacrifice correctness of performance timings for the cpu savings"):
- **Raising the payer drain's 20 s `ReadTimeout`** so the slow reads finish. It loosens a timeout and spends more database time. #4 fixes the read instead.
- **Lowering the stuck-provider thresholds** (20 min, 6 answers, 5 min gap) or the 1 h cooldown, to recycle sooner. Those are detection bounds; #1 uses the defaults.
- **Cutting the probe's handshakes by changing its design:** dropping the route hedge, reusing identities across tunnels, or lowering the attempt rate. Those change probe timing and fidelity. #3 cuts the CPU with the same handshakes.
- **Answering replays before JWT verification and admission.** That changes the per-client limit and the authentication semantics.
- **Caching the revocation record.** Immediate revocation is correctness.
- **Shortening the companion-origin waits.**

Not recommended for other reasons:
- **Moving the discard ahead of the lag-guest gate.** The owner chose the discard after the route's gates (D3 Q1). #5 makes the gate cheap instead.
- **Giving alt CPU priority over the proxies on crisp and fireside.** Its latency is not CPU-bound (§2.7). The saturation is Part B's.
- **Shared nginx TLS tickets for the probe.** That is a privacy and ops trade-off (D4(b)).

### 7. Raw outputs

In `<scratch>/c/`:
- **Helpers:**
  - `mq.py` and `grid.py` (Mimir);
  - `rs.sh` (ansible script runner with IP masking);
  - `inv_containers.sh`, `check_tools.sh`;
  - `perf_one.sh` (one-process capture, on-host aggregation, deletion);
  - `sched_delay.sh`;
  - `tw_logs_payerdue.sh`, `tw_logs_payerdue2.sh`, `tw_logs_payerdue_stack.sh`;
  - `pg_payerdue.sh` (its first run hung on psql's pager; the corrected rerun was refused);
  - `deploys.py`, `regress.py`, `mimir_misc.py`.
- **Mimir:**
  - `deploys.txt`, `daily_peaks.txt`;
  - `grid_api_30m.tsv`, `grid_api_10m.tsv`, `grid_alt_30m.tsv`, `grid_cpdc_1h.tsv`;
  - `regress_api.txt`, `restart_effect.txt`, `sawtooth.txt`;
  - `tw_probe_regression.txt`, `alt_latency_decomp.txt`, `mimir_misc.txt`;
  - specs: `spec_api.json`, `spec_api10.json`, `spec_alt.json`, `spec_cpdc.json`.
- **Hosts:**
  - `inv_containers.txt`;
  - `perf_api_edge4_g2.txt`, `perf_api_edge0_g2.txt`, `perf_alt_crisp.txt`, `perf_tw_edge4_g1.txt`, `perf_tw_edge3_g2.txt`;
  - `sched_delay_crisp_fireside_1.txt`, `sched_delay_edges_1.txt`;
  - `tw_logs_payerdue_edge4.txt`, `tw_logs_payerdue2_edge4.txt`, `tw_logs_payerdue2_edge1.txt`, `tw_logs_payerdue_stack_edge4.txt`;
  - `pg_payerdue.txt` (empty).

## Part D — PostgreSQL on edge-2

This is research, not a change. Every number comes from read-only measurement
of Main's primary, by-us-fmt-5-edge-2, on 2026-11-04 between 10:15Z and 10:57Z,
plus Mimir history from 10-30. Nothing in production was changed. No
identifiers, addresses or literals are printed here. Code references are at the
deployed revisions: server d5cbc2a72 (api, alt, proxy) and 9e2dcaeed
(taskworker), and xops origin/main a074338. *(inferred)* marks a claim
reasoned from code or arithmetic rather than measured.

### Summary

**edge-2 is busier because of more work and one bad plan, not a long
snapshot.** It ran 62.5 cores (65%) from 10:00 to 10:55Z on 11-04, with
5-minute peaks of 68.8–70.3 cores (72–73%); at 10Z on 11-03 it ran 42.6 cores
(44%). Commits rose from 32.4k to 53.7k/s, mostly from the proxy, whose
database pool acquires went from 3.0k/s to 18.5k/s at 10Z (21.6k/s by
10:50Z). Since 07:23Z the payer open-sum behind `GET /subscription/balance`
walks each payer's whole contract history: 7–8 cores, 10× the disk reads, and
about 590 ms per request instead of about 20 ms. On a fit of busy cores
against commits (80 clean hours), the extra work explains about 17 cores and
the flip about 8.

**At matched evening peaks, CPU per unit of work fell about 12%** since
CPUOPT3: 1.12 cores per 1k commits/s on 10-31 (43.3 cores at 38.6k/s) against
0.98 on 11-03 (45.1 at 45.9k/s), and buffer hits per commit fell from 132 to
92. The current api, alt and proxy (d5cbc2a72) have not run an evening peak
yet.

**Where the CPU goes now** (10:21:47Z, 55.1 cores busy without iowait):
backends 34.8 cores (63%), PgBouncer 7.1 (13%), the nginx stream hop 5.2 (9%);
at 10:51:42Z (71.1 cores) 46.9, 8.8 and 6.5. Per PgBouncer query PgBouncer
costs 62–74 µs and nginx 45–55 µs, both 75–80% kernel time (CPUOPT3: 54 and
39 µs). Per-statement overhead is about 54% of PostgreSQL CPU (CPUOPT3: 47%),
and planning is 12.0% (11.8%).

**One statement is 36% of all execution time.** `GetOpenTransferByteCount`
runs 24.5 calls/s at 353 ms with 2.7k block reads per call. It walks each
payer's history through a PG 18 skip scan of `(open, payer_network_id,
transfer_byte_count)`, because the partial index built for its exact
predicate is now priced about 100× too high. All five open-set partial
indexes hold 4.41M entries in 10–83 GB each, about 230 GB in total: the open
set drained from 79M on 10-31, and the pages stayed. The plan has flipped at
least four times since 11-01, each within about an hour of a transfer_contract
vacuum finishing. `ReadPayerDueContracts` runs on the same plan, at 6.9 s per
call.

**The 11-03 edge-6 dump pinned edge-2's horizon from 22:35Z to 02:05Z.**
edge-2 went from 38 to 84 cores within 45 minutes (maximum 87.9), buffer hits
from 3.5M to 41.8M/s, and commits fell 20%. It cost 144–161 excess core-hours
in 3.5 h. The taskworker's oldest overdue task peaked at 887 s (above 60 s for
54 minutes) and debit lag at 346 s. CPU recovered within 5 minutes of the
stop, autovacuum IO in 40 more. The ramp was about 8× faster than on 10-29.
**Thursday's run is still armed** (edge-6, 11-05 06:00Z): an 11–12 h pin set
by st_client_key_history's TOAST would cost 330–420 excess core-hours and
leave roughly half of daytime demand unserved while edge-2 sits at its
ceiling *(inferred)*. Its guards would not stop it.

**CPUOPT3 Part D items** (§3): shipped are #13 (both index drops), D5's
minimal fix (moved to edge-6; its 4.5 h pin estimate failed), the contract
horizon reaper (on at 35 days: about 0.9 backends and 7.7 MB/s of WAL), and
two of five disk-reclaim parts. Not done: #7 nginx hop, #12 Unix socket, #15
kernel options (edge-2 still boots without `retbleed=stuff iommu=pt`;
retbleed uses IBRS), #6, #8, #9, #10 and #11.

**Ranked opportunities** (cores at the evening peak; they overlap little):

| # | Change | Est. saving | Risk |
|---|---|---|---|
| 1 | Give the planner the open-set predicate's real selectivity (expression statistics), so the payer open-sum and payer-due read use their partial index again | 7–8 cores, about 550 MB/s fewer block reads, 12 fewer sessions waiting on IO | low |
| 2 | Remove the nginx stream hop to PgBouncer (CPUOPT3 #7) | 5–6.5 cores | medium |
| 3 | Hold Thursday's dump; dump a restored base backup or split st_client_key_history out; stop on taskworker and debit lag | avoids 144–161 core-h per 3.5 h pinned, about 330–420 core-h per full run | low (owner decision) |
| 4 | Reboot edge-2 into `retbleed=stuff iommu=pt` (CPUOPT3 #15) | 2–4 cores *(unmeasured)* | medium |
| 5 | PgBouncer to PostgreSQL over a Unix socket (CPUOPT3 #12) | 1.5–3 cores *(unmeasured)* | low |
| 6 | Fewer round trips per transaction (CPUOPT3 #5) | 1–2 cores | low–medium |
| 7 | Stop plan invalidation every 6 s on pending_task and transfer_debit_journal (CPUOPT3 #6) | 0.3–1.5 cores | low |
| 8 | Rebuild the five drained open-set partial indexes (230 GB → about 1 GB) in a low-traffic window | plan stability; about 230 GB less autovacuum read per 4 h cycle | medium (horizon pin) |
| 9 | JIT off for OLTP (CPUOPT3 #9) | 0.2–0.3 cores | low |
| 10 | GetTransferStats unpaid total and census inputs (CPUOPT3 #10, #11) | 0.4–0.9 cores | low–medium |

### 1. Method

**When and load:** Wednesday 11-04, 10:15–10:57Z: morning load, not the
evening peak, which is compared from Mimir. In the pg_stat_statements window
(10:19:53–10:36:41Z): 142.8k statements/s and 55.7k commits/s; 113.8–119.1k
PgBouncer queries/s in 49.6–51.4k transactions/s; WAL 48 MB/s, FPI 9.4k/s; xid
burn about 8.1k/s *(inferred from the autovacuum worker's xmin age)*.

**Revisions** (process start times in Mimir):

| Service | Revision | Started |
|---|---|---|
| api | d5cbc2a72 | 11-03 22:08–22:16Z |
| alt | d5cbc2a72 | 11-03 22:18Z |
| proxy | d5cbc2a72 | 11-03 22:54–23:26Z |
| taskworker | 9e2dcaeed | 11-03 19:24–19:28Z |
| connect | Part A's domain | 11-02 19:13–19:38Z |

**Host and database:** 2× Xeon Platinum 8168 with HT (96 logical CPUs), kernel
6.8.0-147 up since 10-24 00:47Z. PostgreSQL 18.6 (PGDG jammy), postmaster since
10-24 11:21Z. pg_stat_statements is extension 1.10, max 5,000, with 4,807
entries; 45 deallocations since its reset at 10-23 20:08Z, none in my windows;
`track_planning` off.

**Captures:**
- **pg_stat_statements:** snapshots at 10:19:53, 10:28:13 and 10:36:41Z, with
  no text and never reset. The 500 s and 1,008 s windows agree within 3%; the
  rankings use the 1,008 s one. Texts were fetched only for active queryids,
  with literals, UUIDs and numbers of six or more digits stripped on the
  server. The same snapshots took pg_stat_database, _wal, _io, _user_tables
  and _user_indexes.
- **CPU per process from `/proc`:** 20 s at 10:21:47Z and 10:51:42Z,
  classified on the host; no command line or address printed.
- **PgBouncer:** `SHOW STATS` and `SHOW POOLS` on all 32 admin consoles over
  their Unix sockets, about 24 s apart, at 10:22Z and 10:52Z.
- **perf:** host-wide flat 20 s and LBR call stacks 12 s, both 99 Hz, at
  10:40–10:41Z. Summarized on the host, data deleted, no perf or bpftrace
  process left (checked by exact name).
- **pg_stat_activity:** 80 samples at 0.25 s (10:48Z), grouped by state, wait
  and query_id. **EXPLAIN** without ANALYZE: six plans, placeholder values.
- **Mimir:** edge-2 CPU, pg_stat_database, the activity and PgBouncer
  exporters, pool acquires by service, the balance route, the reaper and the
  taskworker, 10-30 to 11-04 10:55Z.

Every SQL session set `statement_timeout 20s`, `default_transaction_read_only
on` and `lock_timeout 2s`. Nothing counted a large table.

**Not captured:** statements per service (services set no
`application_name`; Mimir's pool acquires stand in); planning per statement
(`track_planning` off, left off); the dump's COPY backends' own CPU (the
overnight session reported about 1.7 cores, not re-measured). The snapshot
windows are 500 s and 1,008 s, not 15 and 30 min, because the harness blocks a
foreground sleep between snapshots.

### 2. Where the CPU goes now

#### 2.1 Host, evenings and 10Z

Busy means every non-idle mode, including iowait, as in CPUOPT3. Means are of
5-minute rates (scratch d/raw/hist-*.txt).

| Window | Busy | Without iowait | sys+softirq | Commits | Cores per 1k commits/s | Fit residual | idle in tx | proxy acquires/s |
|---|---|---|---|---|---|---|---|---|
| 10-31 18–22Z (CPUOPT3) | 43.3 | 40.8 | 13.8 | 38.6k/s | 1.12 | +0.4 | 58 | 6.6k |
| 11-01 18–22Z | 42.9 | 39.9 | 13.3 | 39.5k/s | 1.09 | −0.8 | 69 | 8.5k |
| 11-02 18–22Z | 52.8 | 49.2 | 17.2 | 55.0k/s | 0.96 | −2.9 | 71 | 17.4k |
| 11-03 18–22Z | 45.1 | 42.4 | 15.0 | 45.9k/s | 0.98 | −3.6 | 120 | 13.9k |
| 11-03 10Z | 42.6 | 36.5 | 13.2 | 32.4k/s | 1.31 | +4.5 | 35 | 3.0k |
| 11-04 03–07Z | 53.5 | 50.2 | 18.0 | 52.7k/s | 1.02 | −0.4 | 167 | 15.7–18.5k |
| 11-04 10Z | 62.5 | 57.7 | 21.3 | 53.7k/s | 1.16 | +7.7 | 159 | 18.5k |

**Deploys in those evenings:** 10-31 is CPUOPT3's baseline. 11-01: api from
05:53Z, connect from 08:13Z, taskworker from 15:25Z. 11-02: api rolled
15:27–18:57Z and connect 19:13–19:38Z, both inside the evening. 11-03: api
5bb504212 at 17:50Z, then 9e2dcaeed at 19:10Z; taskworker 9e2dcaeed from
19:24Z.

**The fit.** Over 80 hours (10-31 to 11-04 07Z, without the dump, the census
and the flip hours), busy cores = 12.8 + 0.78 × commits (k/s), R² 0.70,
residual sd 5.2 cores (d/raw/regress-busy-vs-commits.txt). On it, 11-04
03–07Z sits on the line: the step at 03Z is all work, as commits rose from 34k
to 50–56k/s with the proxy's acquires. 11-04 08–10Z sits 8.0 cores above it,
7.4 of them without iowait: the plan flip (§2.5). There is no long snapshot
today: buffer hits stayed at 4.0–4.5M/s and the oldest client xmin was 17 s
old.

**Pool acquires by service** (Mimir, per second, the hour starting at the time
shown; d/raw/acquires-by-service.txt):

| Service | 10-31 19Z | 11-03 10Z | 11-03 19Z | 11-04 03Z | 11-04 10Z |
|---|---|---|---|---|---|
| proxy | 6,943 | 3,028 | 15,788 | 15,650 | 18,527 |
| connect | 14,601 | 14,404 | 16,933 | 17,306 | 18,112 |
| api | 11,172 | 7,905 | 10,896 | 11,558 | 12,249 |
| taskworker (both pools) | 4,210 | 4,843 | 3,612 | 3,700 | 3,855 |
| total | 36,978 | 30,220 | 47,271 | 48,259 | 52,794 |

The proxy's database traffic varies about 7× from hour to hour and is now the
largest single source. At the fit's marginal cost (0.78 cores per 1k
commits/s), its +15.5k acquires/s at 10Z cost edge-2 roughly 10–12 cores
*(inferred: an acquire is not exactly a commit)*. Why it issues them is
Part B's question.

#### 2.2 By process

From `/proc`, 20 s each (d/raw/census-2.out, census-3.out):

| Consumer | 10:21:47Z cores (user/sys) | 10:51:42Z cores | Processes | CPUOPT3 10-31 |
|---|---|---|---|---|
| backends via PgBouncer (loopback) | 32.84 (25.86/6.98) | 43.98 | 663–696 | ~23 |
| backends direct (LAN, port 5432) | 2.00 | 2.87 | 121–126 | ~6 |
| PgBouncer | 7.09 (1.89/5.20) | 8.82 | 32 | 5.61 |
| nginx stream (6432 → 6433–6464) | 5.15 (1.05/4.10) | 6.52 | 33 | 4.05 |
| io workers | 1.09 | 1.64 | 24 | 1.08 |
| parallel workers | 0.04 | 1.49 | 3–8 | |
| dm-crypt kworkers | 1.12 | 1.35 | 41–43 | ~0.5 |
| autovacuum workers | 0.72 | 0.94 | 3–4 | 0.39 |
| kswapd and kcompactd | 0.66 | | 2 | |
| checkpointer and walwriter | 0.30 | | 2 | 0.23 |
| **host busy without iowait** | **55.1** | **71.1** | | 39–41 with iowait |

**Perf (20 s flat)** agrees: of non-idle samples, postgres is 75%, PgBouncer
13% and nginx 9% (d/raw/perf-1.out).

**Normalized:**
- **PgBouncer:** 62 µs per query and 138 µs per transaction at 10:22Z, 74 and
  178 µs at 10:52Z (CPUOPT3: 54 and 151). **nginx:** 45–55 µs per query
  (CPUOPT3: 39). Bytes out per query rose from 135 B to 238–314 B, and most of
  both processes' time is kernel socket work, which scales with packets and
  bytes.
- **Backends through PgBouncer:** 289–369 µs per PgBouncer query, against about
  220 µs in CPUOPT3; about 7 cores of that are the flip. Without the flip,
  host busy per query is up about 15% (about 419 µs against 364 µs)
  *(inferred: the mix moved toward the proxy)*.
- **Per pgss statement:** backends cost 244 µs of CPU; mean execution time is
  167 µs.

#### 2.3 Inside PostgreSQL

**LBR call stacks**, 12 s, 58,377 postgres samples, each classified by its
innermost known frame (d/raw/perf-1.out):

| Phase | Share | CPUOPT3 |
|---|---|---|
| executor run | 27.5% | 31.0% |
| unknown frames (static functions, mostly deep executor stacks *(inferred)*) | 10.9% | 18.5% |
| IO done in the process (`pgaio`, reads) | 6.5% | 2.4% |
| WAL insert, including lz4 FPI | 0.9% | |
| planning | 12.0% | 11.8% |
| executor start | 11.1% | 9.7% |
| protocol receive and wait | 7.3% | 6.9% |
| main loop, other | 4.9% | 8.6% |
| bind and portal | 4.8% | |
| protocol send | 4.7% | 4.7% |
| transaction start and commit | 4.4% | 1.4% (with utility) |
| executor end | 2.3% | 2.7% |
| parse, analyze, rewrite | 1.3% | 1.2% |
| utility, pgstat, auxiliary, vacuum | 1.4% | |

**Overhead.** Per-statement overhead (planning, executor start and end, parse,
bind, protocol, main loop, transaction control) is about 54% of PostgreSQL
CPU, roughly 19–25 cores. Execution and IO are about 46%.

**pg_stat_database agrees:** active time averaged 35.7 backends against 23.9
in pgss execution, so 33% of active time is outside the executor (execution
time includes IO waits; `track_io_timing` is off).

**Prepared statements are not the planning source:** 98–102k Binds/s against
719–840 client Parses/s and 173–227 server re-prepares/s (0.2% of Binds). The
12% of planning comes from custom plans and plan invalidations (§4 #7).

**Top flat symbols** (percent of all samples, idle included):
`hash_search_with_hash_value` 2.74%, `_bt_compare` 1.08%, `AllocSetAlloc`
1.03%, `GetSnapshotData` 0.64%, `expression_tree_walker_impl` 0.63%,
`copyObjectImpl` 0.53%, `_bt_checkpage` 0.50%, `heap_page_prune_opt` 0.46%,
`heap_hot_search_buffer` 0.43%, `CatCacheInvalidate` 0.26%,
`get_relation_info` 0.26%.

#### 2.4 pg_stat_statements: top statements

Window 10:19:53–10:36:41Z (d/raw/pgss-rank-1-3.txt). "Backends" means average
backends in execution: 23.9 in all.

| # | Statement (normalized) | Code (d5cbc2a72) | % exec | calls/s | mean ms | hit/call | read/call |
|---|---|---|---|---|---|---|---|
| 1 | `SELECT COALESCE(SUM(transfer_byte_count),$2) FROM transfer_contract WHERE payer_network_id=$1 AND (CASE WHEN outcome IS NULL THEN dispute=$3 ELSE $4 END)` | model/subscription_model.go:6982 `GetOpenTransferByteCount` ← subscription_grant_model.go:252 ← controller/subscription_controller.go:228 (`GET /subscription/balance`) | 36.22 | 24.5 | 353 | 11,704 | 2,746 |
| 2 | close `UPDATE transfer_contract SET outcome=…, provider_usage=$3 WHERE contract_id=$1 AND outcome IS NULL` | model/subscription_model.go:3711 | 3.28 | 1,009 | 0.78 | 52 | 0.7 |
| 3 | `WITH creation_clock … INSERT INTO transfer_contract` (A) | model/subscription_model.go:1897, subscription_redis_admission.go:560 | 3.03 | 834 | 0.87 | 57 | 1.0 |
| 4 | reaper `WITH deleted AS (DELETE FROM transfer_contract WHERE contract_id=ANY($1))` | model/contract_horizon_reaper.go:1060 (taskworker) | 2.45 | 1.4 | 413 | 35,158 | 4.8 |
| 5 | close-report upsert `WITH prior AS MATERIALIZED (… contract_close …)` | model/subscription_model.go:3187 | 2.33 | 2,487 | 0.22 | 47 | 0.2 |
| 6 | TLS chain lookup `SELECT c.tls_certificate_pem, …` | model/client_contract_metadata.go:35 | 2.05 | 3,775 | 0.13 | 19 | 0.1 |
| 7 | `UPDATE network_client_connection SET connected=…` | model/network_client_model.go:3721 | 1.92 | 563 | 0.82 | 48 | 0.7 |
| 8 | pair network lookup (`a.network_id, … prober.network_id`) | model/network_client_model.go:2281 | 1.79 | 5,295 | 0.08 | 17 | 0.0 |
| 9 | `INSERT INTO network_client_connection` | model/network_client_model.go:3621 | 1.63 | 562 | 0.69 | 44 | 0.3 |
| 10 | health-window aggregate (`… SUM(ok_count), SUM(total_count) …`) | model/provider_egress_health_model.go:505 | 1.48 | 0.3 | 1,050 | 350,156 | 17 |
| 11 | payment-driven contract retention (`WITH payment AS MATERIALIZED … contract_retention_pending`) | model/subscription_model.go:6280 | 1.32 | 0.01 | 31,842 | 2.84M | 105,593 |
| 12 | escrow and balance select (`selected_escrow.balance_byte_count`) | model/subscription_model.go:819 | 1.30 | 1,007 | 0.31 | 10 | 0.0 |
| 14 | GetTransferStats unpaid (`… AS paid_bytes_provided …`) | model/account_payment_model.go:1148 | 1.15 | 0.6 | 498 | 856,546 | 6,205 (77 ms of JIT per call) |
| 26 | payer-due read `SELECT contract_id,create_time,expiration_time … payer_network_id=$1 …` | model/subscription_payer_due_close.go:117 | 0.69 | 0.03 | 6,882 | 1.67M | 240,756 |

**Top by calls:**

| Statement | calls/s | mean |
|---|---|---|
| `SET LOCAL lock_timeout = $1` | 9,850 | 3 µs |
| `commit` | 9,621 | |
| `provide_key_change` COUNT (model/network_client_model.go:3283) | 9,385 | 20 µs |
| `begin … read committed` | 8,212 | |
| `begin … repeatable read` | 1,486 | |
| pair lookup | 5,295 | |
| TLS chain lookup | 3,775 | |

- **The begin pair is one round trip.** `SET LOCAL lock_timeout` rides on the
  BEGIN as a multi-statement message (db.go:1038), and pgss counts the two
  statements separately. This is unchanged since a8461e236.

**By table family** (d/raw/families-1-3.txt):

| Family | % exec | Backends | % calls |
|---|---|---|---|
| payer open-sum (one statement) | 36.2 | 8.65 | 0.02 |
| network_client, provide_key, device, client key | 20.4 | 4.87 | 28.6 |
| transfer_contract, the rest | 13.6 | 3.26 | 10.4 |
| provider egress and probes | 7.6 | 1.82 | 8.7 |
| transfer_escrow and sweeps | 6.0 | 1.43 | 5.0 |
| reaper and retention deletes | 4.8 | 1.16 | 0.0 |
| contract_close* | 3.8 | 0.90 | 5.8 |
| transaction control and locks | 1.3 | 0.31 | 30.3 (43.3k/s) |

**Sampled activity** (80 samples, d/raw/actsample-1.top.txt): on average 51.6
active backends and 163.9 idle in transaction. Active backends by wait:
`ClientRead` 20.5 (extended protocol, waiting on the client), CPU 13.7, IO 13.8
(`DataFileRead` 7.6, `AioIoCompletion` 6.1; CPUOPT3: 2.4), IPC 2.8. The
open-sum alone holds 14.6 sessions: 12.0 waiting on IO and 2.2 on CPU.

**pg_stat_io** (d/raw/tables-io-1-3.txt): client backends read 1,400 MB/s and
evicted 171k buffers/s, of which the open-sum is about 67k block reads/s
(about 550 MB/s). Autovacuum read 139 MB/s, parallel workers 161 MB/s, and the
checkpointer wrote 42 MB/s.

#### 2.5 The open-sum plan flip

**What changed:** mean latency of `GET /subscription/balance` was 10–35 ms per
hour from 10-30 to 11-04 07Z, 580–1,060 ms in 5-minute means after 07:25Z, and
530–620 ms per hour since (d/raw/route-subscription-balance.txt,
flip-0700-0830-5m.txt). Commits did not change.

| 5-minute means | 07:00–07:15Z | 07:30–08:30Z |
|---|---|---|
| busy cores | 44–45 | 58–71 |
| sys | ≈10 | 15–18 |
| disk reads | 58–76 MB/s | 433–994 MB/s |
| average active backends | 15–16 | 32–43 |

Hourly means show three shorter earlier episodes: 11-01 05–09Z (130 ms),
11-03 05Z (599 ms) and 11-03 17Z (145 ms).

**Plans** (EXPLAIN only; d/raw/05-explain.out, 06-explain2.out,
09-explain3.out):
- **The open-sum** runs as an `Index Scan using
  transfer_contract_open_payer_network_id_transfer_byte_count` whose only index
  condition is `payer_network_id = $1`: a PG 18 skip scan over both values of
  the leading `open` column, with the `CASE` as a heap filter (estimated cost
  20,401). Every call visits every contract of the payer, open or closed:
  about 58k index entries per scan (108.4B tuples read over 1.88M scans).
- **The payer-due read has the same plan.** The unresolved-pair EXISTS still
  uses its CASE partial index (cost 5.1), so the predicate proof works and the
  payer queries' choice is cost-based. `open AND payer_network_id = $1` would
  get an index-only scan of the composite index at cost 6.7.
- **Index counters over 380 s:** the composite index gained 30k scans (about
  80/s, about 3 primitive scans per call);
  `transfer_contract_unresolved_payer_transfer_byte_count` gained 12.

**Why the planner prefers the skip scan** *(inferred from the catalog and
PostgreSQL's costing)*. The open-set partial indexes are mostly empty: all
five have reltuples of exactly 4,410,364, the count a vacuum leaves, but are
still 10–83 GB, 2.4–18.7 KB per entry (79.2M entries on 10-31):

| Index | Size | Bytes per entry |
|---|---|---|
| `…_unresolved_source_pair_create_time` | 82.5 GB | 18,709 |
| `…_unresolved_destination_pair_create_time` | 81.3 GB | 18,435 |
| `…_unresolved_payer_transfer_byte_count` | 38.3 GB | 8,685 |
| `…_outcome_null` | 19.3 GB | 4,379 |
| `…_open_partial_create_time` | 10.4 GB | 2,355 |

The planner prices a partial-index path by pages per tuple, so each entry
looks about 100× more expensive than it is. The opaque `CASE` also gets the
default selectivity of 0.5 against a true 0.15% (the generic estimate of
26,073 rows matches 2.86B × 0.5 / 51.8k distinct payers). The two paths then
cost about the same, and a small change in stats flips the plan. This flip
came with a transfer_contract vacuum finishing at about 07:23Z: the freeze age
dropped from 298M to 197M at 07:30Z, and `last_autoanalyze` reads 07:23:45Z.
The 11-03 17Z episode also follows such a drop (17:40Z, 305M to 198M).

**Cost:** on the fit, 7.4 cores without iowait (3.7 of them sys) and 154 MB/s
of disk reads above the line (hourly means, 08–10Z). In pgss, 8.65 backends of
execution, 12 of them waiting on IO at any instant. The rest of the CPU is the
IO path and buffer eviction: in-process reads, dm-crypt, kswapd, and other
statements missing cache.

#### 2.6 Autovacuum, freezing and the reaper

- **Cadence:** pending_task and transfer_debit_journal are each autovacuumed
  and autoanalyzed every 6.0 s (168 times in 1,008 s; about 16 s in CPUOPT3).
  Each pass updates `pg_class`, which invalidates the cached plans on the
  table in all ~800 backends. pending_task still has
  `autovacuum_analyze_threshold=50` at scale 0, so CPUOPT3 #6 was not applied.
  The cluster's freeze age (214–243M) is above `autovacuum_freeze_max_age`
  (200M), which may be why the launcher sends a worker to bringyour on every
  naptime/N tick *(inferred, not verified)*.
- **transfer_contract:** 65 autovacuums totaling 255.8 h since the reset, so
  one worker is on it continuously, about 4 h per run. The freeze-age sawtooth
  is unchanged: floors of 187–204M (at or above `autovacuum_freeze_max_age`)
  and peaks of 298–389M on 11-01..11-04 (453M on 10-30). The anti-wraparound
  loop CPUOPT3 #8 described continues.
- **Dead rows** at 10:36Z: transfer_contract 76.2M (vacuum in progress; 2.84B
  live), transfer_escrow 31.9M, contract_close 13.9M, pending_task 283k
  against 161k live in a 28 GB heap, transfer_debit_journal 377k against 23k
  live in 7.2 GB.
- **IO:** autovacuum's vacuum-context reads total 304 TB since the reset,
  about 465 MB/s on average since CPUOPT3's 156.7 TB against 229 MB/s before
  *(inferred: it assumes no pg_stat_io reset in between)*; 139 MB/s in my
  window.
- **The contract horizon reaper** (d/raw/reaper-metrics.txt, reaper-pgss.txt):
  mode 2 (on) at a 35-day horizon since at least 11-02 12Z, 89.4 days behind
  it, working through the June backlog. It deletes about 1.75k
  transfer_contract, 3.5k contract_close, 1.75k transfer_escrow and 149 sweep
  rows/s, in 2,000-row batches of 0.92 s (0.94 batches/s), and protects
  70.6/s for payment holds and 63.9/s for payment review. On edge-2 it costs
  0.92 backends of execution (3.8%), 75k buffer hits/s and 7.7 MB/s of WAL
  (about 16% of statement WAL); its five-table cascade spends 44 of each 83 ms
  in JIT.
- **Disk:** root free fell from 5.82 TB (10-31) to 5.46 TB, and the database
  grew from 9.12 to 9.48 TB. Deletes free space inside the tables but do not
  shrink files.

#### 2.7 The 11-03/04 dump (edge-6 pg_dump of the primary)

5-minute data (d/raw/dump-1103-5m.txt, dump-1103-cost.txt):

| Measure | Before (21:30–22:35Z) | Plateau (23:20Z–02:05Z) | After the stop |
|---|---|---|---|
| busy cores | 38.2 | 84.3 mean, 87.9 max | 44.7 at 02:10Z |
| buffer hits | 3.5–4.0M/s | 41.8M/s | 3.4M/s at 02:10Z |
| tuples returned | 3.5–5.6M/s | 64.9M/s | 3.8M/s |
| commits | 33.0k/s | 26.3k/s | back above 29k/s by 02:15Z |
| taskworker oldest overdue | 0 | peak 887 s (00:25Z); 377–379 s at 01:36–01:42Z; >60 s for 54 min | 0 at 02:10Z |
| debit lag (oldest) | 5–20 s (one 900 s spike at 21:30Z) | 295 s at 22:53Z | 346 s at 02:07Z, then normal |

**Cost:** 144–161 excess core-hours in 3.50 h, depending on the baseline (the
pre-dump mean, or the fit on actual or pre-dump commits). The catch-up after
the stop (02:05–02:45Z) added 10 core-hours: iowait 8.6 cores and disk reads
980 MB/s while autovacuum cleaned up. The same hours on the three nights
before ran 38.9–52.0 cores.

**The ramp** was +46 cores within 45 minutes of the start, against about 6.5 h
to reach +40 cores on 10-29. Why it is faster was not determined. The
overnight session saw pending_task reach 3.7M dead rows against 144k live,
which points at the queue tables' scans; I did not re-measure that.

### 3. What CPUOPT3's shipped changes did

| Item | Landed | In the deployed revision | Measured against predicted | Confidence and confounders |
|---|---|---|---|---|
| #13 index drops (`reap_pending`, `stream_id`) | f5bff300f, 8c9b0e69d (11-01, `db_index_diet_migrations.go:27-34`); both indexes absent on Main | yes (ancestors of d5cbc2a72 and 9e2dcaeed) | Predicted 0.2–0.5 cores and −95 GB per vacuum cycle. The −95 GB holds: 16 indexes now. CPU is not separable. The transfer_contract vacuum still runs about 4 h back to back, because the heap grew from 835 to 925 GB and the other indexes grew by about the same 95 GB | high that it shipped; low on the CPU effect |
| D5 minimal fix: bounds, verify-then-delete, `-Fd -j4` | xops 0e44e1d, c920b07, b972e66 (be7c270), then moved to edge-6: 6b45921, 405292e, 890207a, b1c9a7c | applied: the edge-6 timer, `TimeoutStartSec=30h`, the 1.0B freeze-age guard, free-space guard; edge-2's timer disabled | Predicted a 4.5 h pin and 65–80 core-h. The first edge-6 run was heading for an 11–12 h pin, set by st_client_key_history's 715 GB of TOAST, which one worker reads. It cost 144–161 core-h in 3.5 h. Staging moved off edge-2's root as intended | high |
| Contract horizon reaper (7334b3a17, 2bff1b9a5) | yes | yes, on (mode 2, 35 days) | Cost about 0.9 backends and 7.7 MB/s of WAL. 2,000-row batches of 0.92 s replace the 447–929 s retention batches that D5 Part 4 described. The open set has drained from 79M to 4.4M, through all closers together | medium on attribution |
| Disk reclaim | provider_egress_health_history partitioned (9 daily partitions, 32 GB); client_reliability daily partitions with 14-day retention | yes | REINDEX of the open-set partials not done: 232 GB for 4.4M entries, and network_client's indexes grew from 396 to 416 GB. VACUUM FULL of pending_task (28 GB) and transfer_debit_journal (7.2 GB) not done | high |
| #7 nginx hop | no (nginx.conf.j2 unchanged) | no | nginx is now 5.2–6.5 cores (predicted 4–5 at 10-31's load) | high |
| #12 Unix socket | no (pgbouncer.ini.j2:18 `host=127.0.0.1 port=5432`; 677 loopback connections) | no | untested | high |
| #15 kernel options | staged only (inventory.yml:74, grub.d) | not booted: `/proc/cmdline` has no options; retbleed IBRS; IOMMU "Translated" (DMA-FQ) | untested | high |
| #6 analyze thresholds, #8 freeze loop, #9 JIT, #10 census inputs, #11 GetTransferStats, #12 debit-journal walk | not landed; server origin/main 403fa4b39 has no change in these files since d5cbc2a72 | no | see §5 | high |
| PgBouncer `listen_backlog 65535` (xops 57df5cc, not CPU) | written and reloaded | listeners still at 128: it needs restarts that the owner has not given a go for | n/a | high |

**D1, D2 and D3 are not database changes.** D2 predicted about 265/s fewer
contract pairs. transfer_contract inserts were 1.0k/s on Wednesday at 10:20Z
against 1.37k/s on Saturday at 18:31Z, despite 50% more commits. Time of day,
active devices and the proxy mix confound it, so I cannot attribute the drop
to D2.

### 4. Ranked opportunities

#### #1 Restore the partial-index plan for the payer reads

- **Change**, in order of preference:
  - **(a) Expression statistics:** a migration adding `CREATE STATISTICS … ON
    (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) FROM
    transfer_contract`. The planner then sees the predicate's true selectivity
    (about 0.15%, not 0.5), and the partial-index path's estimate falls about
    300× (26k to about 85 entries), well below the skip scan's 20k. The
    statistics are collected at the next autoanalyze of transfer_contract,
    between its 4-hour vacuums. The `CASE` isolation from the "false-zero"
    indexes (20a9414e9) stays.
  - **(b) Later, in a window:** rebuild `…_unresolved_payer_transfer_byte_count`
    (38 GB → about 0.2 GB *(inferred)*) to fix its pages per tuple; see #8 for
    the pin.
  - **Not recommended without its author:** rewriting to `open AND
    payer_network_id = $1`. It gets an index-only scan at cost 6.7, but
    reopens the false-zero exposure the `CASE` form was written to avoid.
- **Code:** model/subscription_model.go:6982 `GetOpenTransferByteCount`;
  model/subscription_payer_due_close.go:115-120 `payerDueContractsSql` and
  `ReadPayerDueContracts`, called from controller/subscription_controller.go:236
  (`RequestPayerDueContractClose`) and model/subscription_contract_deadline_scan.go:71;
  the index migrations at db_migrations.go:2751 and :7247.
- **Evidence (§2.5):** 36% of execution time; +7.4 cores without iowait on the
  fit; 12 sessions waiting on IO and about 550 MB/s of block reads; 590 ms per
  request on the route; four flips since 11-01.
- **Saving:** 7–8 cores at today's morning load. The route runs 24–26
  requests/s at the evening peak (11-03 18–19Z), so about the same there while
  the plan stays flipped *(inferred)*; the route returns to about 20 ms.
- **Risk:** low: statistics only, no query, lock or index change. Confirm first
  that PG 18 uses expression statistics for an opaque boolean `CASE` in this
  costing (EXPLAIN on a restored copy, or on Main after the next autoanalyze).
  `CREATE STATISTICS` takes ShareUpdateExclusiveLock, so it waits for the
  running vacuum; it does not block DML.
- **Timing bounds:** none. The same rows are summed; no timeout, deadline or
  cache changes.
- **Validation:** EXPLAIN shows both payer reads on
  `…_unresolved_payer_transfer_byte_count`; the composite index's idx_scan
  stops growing by about 80/s; pgss means of queryid 5208762677119552037 at
  1–2 ms (9.2 ms on average since the reset, before most flips) and of
  2499203249236021781 under 100 ms; the route under 30 ms; blks_read/s and
  disk reads back on the fit, residual near 0. Add a monitor rule on the
  route's latency, or that queryid's mean, to catch the next flip.

#### #2 Remove the nginx stream hop (CPUOPT3 #7)

- **Change:** CPUOPT3's options, unchanged. Either (a) a pgx `DialFunc` that
  spreads new connections over ports 6433–6464 and tries the next port on a
  refused or timed-out dial within `connect_timeout` (db.go:351, beside
  `configurePgPoolLiveness` at :370); or (b) PgBouncer `so_reuseport` on 6432
  with `[peers]` (pgbouncer.ini.j2:59, :366, :431), retiring nginx.conf.j2's
  stream block.
- **Evidence:** nginx uses 5.15–6.52 cores, 80% in the kernel, at 45–55 µs per
  query, for 1,452 client connections.
- **Saving:** 5–6.5 cores at today's load and 4.5–6 at the evening peak, plus
  part of the loopback softirq.
- **Risk:** medium: the database entry path; moving 6432 resets connections.
- **Timing bounds:** `proxy_timeout 300m` is an idle close; keep it as
  PgBouncer `client_idle_timeout = 18000`. Under (a) the dialer reproduces
  `max_fails=3 fail_timeout=30s`. `query_wait_timeout`, the server timeouts
  and `connect_timeout` are unchanged.
- **Validation:** canary one api block on (a); measure PgBouncer CPU per query,
  nginx CPU, edge-2 softirq and client query latency.

#### #3 Stop long pins from the dump

- **Change**, for the owner: hold 11-05 06:00Z (edge-6
  `bringyour-pg-backup.timer`). Then either (a) dump a restored base backup,
  the 11-04 recommendation, or (b) back up st_client_key_history apart from
  the consistent dump, since its 715 GB of TOAST sets the pin. Beside the 1.0B
  freeze-age and free-space guards, add stops on the taskworker's oldest
  overdue task above 60 s sustained, debit lag above 300 s, and an edge-2 busy
  level the owner sets.
- **Code:** xops host_files/by-us-fmt-5-edge-6/db-archive/bringyour-pg-backup.sh:70-91
  (guards), bringyour-pg-backup.service (`TimeoutStartSec=30h`),
  bringyour-pg-backup.timer (`Sun,Thu 06:00 UTC`); playbook-pg-basebackup.yml
  for (a).
- **Evidence (§2.7):** the CPU stop rule (97% twice) cannot fire, because
  edge-2 plateaus at 84–88 cores. The freeze-age guard would not fire either:
  about 25M per hour × 12 h on a 200M floor is about 500M.
- **What another full run would cost** *(inferred)*: a plateau of about 85
  cores within about 30–45 minutes, against a 06–18Z baseline of about 53–56
  cores if today's commits hold, is about 30–35 cores × 11–12 h = 330–420
  core-hours, ending in the evening ramp. The larger cost is lost work: at the
  plateau each 1k commits/s costs about 3.2 cores, so 85 cores serve about 27k
  commits/s against a daytime demand of 50–55k/s, leaving taskworker and debit
  work and api and connect transactions waiting for most of the run.
- **Saving:** 144–161 core-hours per 3.5 h of pin avoided, 330–420 per full
  run; about 4–5 cores on average over a week of two runs.
- **Risk:** low for the hold and the guards; the format is the owner's call.
- **Timing bounds:** none loosened; the guards are new, tighter bounds.
- **Validation:** during any dump, edge-2's residual on the fit, buffer hits
  per second against the same hour the day before, taskworker overdue and
  debit lag.

#### #4 Reboot edge-2 into the staged kernel options (CPUOPT3 #15)

- **Change:** the owner-scheduled reboot that applies inventory.yml:74
  (`retbleed=stuff iommu=pt`, already in grub.d).
- **Evidence:** kernel time is 19–26 cores (sys 14.2–19.1, softirq 5.3–7.0).
  retbleed is mitigated by IBRS, the IOMMU is in translated mode, and syscall
  entry and exit symbols lead PgBouncer's and nginx's profiles.
- **Saving:** 2–4 cores *(unmeasured; scaled from CPUOPT2's 3–8% on the
  services edges)*. #2 and #5 shrink it.
- **Risk:** medium: a primary restart. Follow the 10-13 huge-pages lessons
  (stop the taskworker containers for any outage over about 100 s; expect the
  post-start fork stall), in an owner window outside the validator gate and
  any dump.
- **Timing bounds:** none. `retbleed=stuff` keeps the mitigation.
- **Validation:** sys and softirq per commit, and PgBouncer and nginx CPU per
  query, against §2.2.

#### #5 PgBouncer to PostgreSQL over a Unix socket (CPUOPT3 #12)

- **Change:** pgbouncer.ini.j2:18 becomes `host=/var/run/postgresql`, plus a
  `local bringyour bringyour` md5 or scram line in edge-2's pg_hba.conf.
  Reload, one instance at a time.
- **Evidence:** PgBouncer sys is 5.2–6.6 cores and backend sys 7.0–8.6, over
  677 loopback TCP server connections.
- **Saving:** 1.5–3 cores *(unmeasured)*. **Risk:** low; reversible per
  instance. **Timing bounds:** none (`server_connect_timeout` and
  `server_login_retry` unchanged).
- **Validation:** one instance of 32 against the other 31 for an hour: CPU per
  query and its backends' sys time.

#### #6 Fewer round trips per transaction (CPUOPT3 #5)

- **Evidence:** transaction-control and lock statements are 43.3k/s, 30% of
  statements. `SELECT pg_backend_pid()` runs 1.3k/s after admission
  (db_owned_transaction.go:195, :359), although :479 already returns the PID.
  Advisory locks run about 5k/s, `set_config` 1.1k/s and `LOCK TABLE
  network_client_connection` 1.1k/s. A round trip costs about 107–129 µs in
  PgBouncer and nginx, plus the backend's protocol path.
- **Change:** CPUOPT3's, unchanged. **Saving:** 1–2 cores. **Risk:**
  low–medium (ownership and fencing code). **Timing bounds:** none; the same
  locks and checks in fewer messages.
- **Validation:** PgBouncer queries per transaction (2.2–2.4 now) and edge-2
  CPU per transaction.

#### #7 Stop plan invalidation every 6 s on the queue tables (CPUOPT3 #6)

- **Change:** a migration raising pending_task's `autovacuum_analyze_threshold`
  from 50 to about 20k at scale 0 (db_migrations.go:5678), and giving
  transfer_debit_journal its own analyze threshold (it sets only fillfactor,
  db_write_path_migrations.go:86). Both vacuum thresholds stay.
- **Evidence:** both tables are analyzed every 6.0 s (§2.6); planning is 12.0%
  of PostgreSQL CPU; `CatCacheInvalidate` is 0.26% of host samples.
- **Saving:** 0.3–1.5 cores *(inferred)*. **Risk:** low; check that the poll
  stays on `pending_task_function_poll_order`. **Timing bounds:** none;
  analyze is statistics freshness.
- **Validation:** autoanalyze rate per table, the LBR planning share,
  `CatCacheInvalidate` samples.

#### #8 Rebuild the drained open-set partial indexes

- **Change:** REINDEX CONCURRENTLY, one index at a time, off-peak, under the
  dump's guards; or wait for contract partitioning.
- **Evidence:** 232 GB for 4.41M entries (§2.5). Every transfer_contract vacuum
  reads all of it, and the inflated pages are the root of #1 and a risk for
  the pair queries (the unresolved-pair EXISTS runs 1,675/s).
- **Saving:** CPU under 0.5 core *(inferred)*; about 230 GB less autovacuum
  read per 4 h cycle (about 16 MB/s on average) and about 230 GB of disk.
- **Risk:** medium. A partial index cannot use the safe concurrent build, so
  each 925 GB heap scan pins the horizon; at 11-03's pin cost (+46 cores
  within 45 minutes) only off-peak, one at a time. **Timing bounds:** none.
- **Validation:** relpages against reltuples per index, plan checks for the
  five families, transfer_contract vacuum duration.

#### #9 JIT off for OLTP (CPUOPT3 #9)

- **Evidence:** JIT is 0.26 cores in the window. The reaper's cascade spends
  44 ms of each 83 ms call (0.06 cores), GetTransferStats 77 ms per call, and
  a per-client contract read (queryid −2375764198285970919) 223 ms per call
  (0.11 cores).
- **Change:** `jit = off`, or `jit_above_cost` of 10M or more
  (postgresql.conf:432; reload). **Saving:** 0.2–0.3 cores. **Risk:** low.
  **Timing bounds:** none. **Validation:** the pgss `jit_*` deltas go to
  about 0.

#### #10 GetTransferStats unpaid total and census inputs (CPUOPT3 #10, #11)

- **Evidence:** GetTransferStats unpaid runs 0.6 calls/s at 498 ms and 857k
  hits per call, 14% of all statement buffer hits
  (model/account_payment_model.go:1148). The egress cohort read takes 7.7 s
  per call (model/provider_url_probe_fleet.go:82) and the health-window
  aggregate 1.05 s (model/provider_egress_health_model.go:505). Together about
  0.6–0.9 backends.
- **Change:** CPUOPT3's: an incremental unpaid total, and one computed census
  snapshot per window. **Saving:** 0.4–0.9 cores. **Risk:** low–medium.
  **Timing bounds:** none; caches are not lengthened and windows are the same.
- **Validation:** these queryids' hits per second.

#### Noted, not a Part D change

- **The proxy is now edge-2's largest workload driver,** at 18–21.6k acquires/s
  (§2.1). Each 1k commits/s it removes is worth about 0.8 edge-2 cores
  (Part B).
- **Idle-in-transaction rose from 35 to 160–178 sessions from 03Z on 11-04.**
  It holds PgBouncer server connections (231–236 active of about 690) but
  burns no edge-2 CPU. PgBouncer waiting stayed at about 0 (a 5-minute mean
  of 5.3 at most).
- **Connection churn is negligible:** 11–14 client logins/s and 1.0 new
  backends/s.

### 5. CPUOPT3 predictions checked

| CPUOPT3 claim | Outcome |
|---|---|
| "Connection churn and idle-in-transaction are not problems now" | Churn holds (1.0 new backends/s). Idle-in-transaction rose from 33 to 164 on average since 03Z on 11-04, with the proxy's traffic; no waiting clients |
| Planning 11.8% of PostgreSQL CPU; per-statement overhead about 47% | 12.0% and about 54%: unchanged, nothing shipped |
| #7 nginx hop 4–5 cores | not shipped; now 5.2–6.5 cores at higher load |
| #13 0.2–0.5 cores, −95 GB per vacuum | shipped; −95 GB holds; CPU not separable |
| #1/D5: `-j4` pins about 4.5 h, about 65–80 core-h | wrong for the real data: st_client_key_history's TOAST makes the pin about 11–12 h; 144–161 core-h in the first 3.5 h |
| CPUOPT3 §2.7: CPU recovers within an hour of the snapshot closing | it recovered within 5 minutes; autovacuum IO took 40 more minutes |
| #8 freeze-age peaks to about 200M | not shipped; peaks 298–389M on 11-01..11-04 |
| #9 JIT 0.06–0.24 cores | not shipped; 0.26 cores in the window |
| #11 GetTransferStats 0.1–0.15 cores | not shipped; 0.27 backends of execution now |
| #12 debit-journal walk | not shipped; 23k live rows against 377k dead in a 7.2 GB heap |
| D5 Part 4: retention in short transactions | the horizon reaper's batches take 0.92 s (2,000 rows); the payment-retention read still runs 32 s per call |
| CPUOPT3 §2.10: the 11-06 census about +10 cores and 5–10 core-h | not yet run. It will meet the flipped plan unless #1 lands, and Thursday's dump if not held |
| #15 1.5–3 cores | untested; not rebooted |

### 6. Considered and rejected (owner rule and poor trades)

- **A cache in front of the open-sum or `/subscription/balance`.** It would
  trade the pending bytes' freshness for CPU, and the plan fix makes it
  unnecessary.
- **Lowering `idle_in_transaction_session_timeout`, `query_wait_timeout` or
  PgBouncer's server timeouts** to cut the idle-in-transaction pile-up. These
  are timeouts, and the slow transactions come from CPU-saturated clients.
- **Raising `checkpoint_timeout`.** FPIs are most of WAL, but this is a timeout
  and lengthens crash recovery.
- **Loosening the dump's bound or guards,** or a time-boxed dump that publishes
  nothing. #3 adds tighter bounds only.
- **A role-wide `statement_timeout` or `transaction_timeout` for the
  application.** It would cut legitimate long reads, which have their own
  bounds, such as the census step.
- **Raising `autovacuum_freeze_max_age`.** Not a timing bound, but it removes
  wraparound margin and does not help transfer_contract.
- **Pausing the reaper for CPU.** It costs about 1 core, and retention and disk
  are owner decisions.
- **Lowering parallelism globally, or shrinking PgBouncer pools.** Both are
  poor trades, as in CPUOPT3.

### 7. Raw outputs

Under `<scratch>/d/`: scripts in `scripts/` and
`sql/`, outputs in `raw/`. Query text is pg_stat_statements' normalized form
with literals, UUIDs and long numbers stripped on the server; audit database
names are masked.

- **Settings and catalog:** `01-info.out` (settings), `02-pgsscols.out`,
  `03-tcidx.out` (indexes, partitions, vacuum progress), `07-reloptions.out`,
  `12-reclaim.out`.
- **pg_stat_statements:** `snap-{1,2,3}.tsv` (with database, WAL, IO, table and
  index counters), `pgss-delta-{1-2,1-3,2-3}.*`, `pgss-rank-1-{2,3}.txt`,
  `texts-*.tsv`, `families-1-3.txt`, `reaper-pgss.txt`, `tables-io-1-3.txt`.
- **Plans and activity:** `05-explain.out`, `06-explain2.out`,
  `09-explain3.out`, `actsample-1.*`.
- **Host:** `census-2.out`, `census-3.out` (`census-1.out` has a mis-parse,
  superseded), `pgb-stats-{1,2}.*`, `perf-1.out`, `edge2-host-facts.out`.
- **Mimir:** `hist-5m.json`, `hist-*.txt`, `acquires-by-service.txt`,
  `route-subscription-balance.txt`, `flip-0700-0830-5m.txt`,
  `regress-busy-vs-commits.txt`, `evenings-normalized.txt`,
  `dump-1103-{5m,cost}.txt`, `reaper-metrics.txt`, `freeze-age-daily.txt`,
  `edge2-root-free.txt`, `late-morning-5m.txt`, `process-starts.txt`.

## Part E — The Redis cluster

This part is read-only research on Main, 2026-11-04 10:20–11:00Z. Sources:
- Mimir;
- redis-cli on edge-5 and edge-6 only: `INFO everything`, `CONFIG GET`, `CLUSTER NODES`, `SLOWLOG GET`, `LATENCY LATEST`, `CLIENT LIST` summarized on the host;
- `/proc` and three short perf samples;
- code read at the deployed revisions.

Nothing in production was changed. No key, argument, value, client address or client name was printed or kept. *(inferred)* marks a claim reasoned from code or per-node command mix rather than measured directly. Raw outputs are cited as `<scratch>/e/…` (`<scratch>/e/`).

### Summary

- **Volume rose, cost per request did not move.** On CPUOPT3's own basis, process CPU is up 19%:
  - the 3-day mean went from 26.3 to 31.2 cores (peak 34.1 → 43.5);
  - read events (requests) went from 131k/s to 185k/s (+40%);
  - all-in cost per request fell from 203 to 172 µs.
  - **Evenings, like with like:** 31.7 cores on 10-31 18–22Z against 37.0 on 11-03 18–22Z (+17%), with requests +22% and 187 → 182 µs per request.
  - **At matched load** (≈150–170k requests/s) the cost is unchanged: 187 µs on the 10-31 evening, 188 µs on the 11-03 morning.
  - **Kernel and I/O threads:** about half of Redis CPU is still kernel time (51–59%). I/O threads are 54% of it.
  - **Scripts** are 76% of event-loop command time (70% in CPUOPT3).
- **Now (Wednesday morning, during the proxy incident):** 36.1 cores, 964k commands/s, 239k requests/s, 151 µs per request, in a 15-minute snapshot pair.
  - **Node 6383's main thread is saturated:** 0.87–0.97 cores, 195 event-loop cycles/s against ~2,700 on its peers.
  - **The cause *(inferred)*:** one escrow-balance slot under the proxy's exhausted-payer loop. Since 03Z it runs about 1k/s each of refused reservations, empty probes and proven exhaustion, with recovery bursts to 14k/s. 6383's EVAL rate stepped from 1.0k to 4.6k/s at 03:00Z.
  - **Fallout:** 234 new connections/s on 6383, and proxy pool timeouts of 280–660/s (≈0 before).
- **PING doubled, and it is now the largest cheap win.**
  - **Volume:** 18.5k/s on the 10-31 evening, 32.4k/s on the 11-03 evening, 32.3k/s now (13.5% of all requests).
  - **Sources:** ≈15–18k/s are the contract-hole preflights that CPUOPT3 §4.1 named; they are still in the code. The other 6–20k/s appeared from 11-02 01:00Z, from other preflight call sites.
  - **The fix:** making no-preflight the default removes all of them, about 2–3 Redis cores.
- **edge-6 crashed and rebooted on 11-03.**
  - **The crash:** the journal stops mid-line at 02:50:15Z with no shutdown sequence. The next boot was 04:23:51Z. A SCSI "Hardware Error" sense on `sde` was logged at 02:06:53Z.
  - **The impact:** its 32 nodes, two thirds of the slots, were down about 94 minutes. I found no record of it in the handoffs.
  - **A side effect:** the staged `iommu=pt` is now active on edge-6, and IOMMU symbols are gone from its profiles. edge-5 still runs translated DMA.
- **Latency moved more than CPU.**
  - **edge-5:** NUMA balancing has been off since 11-01 (xops 7679528), and its slowlog rate fell from 170–210 to 0.2–3.6 entries per node-hour.
  - **edge-6** still balances, migrating 41–44k pages/s. It logs 58–130 slow entries per node-hour, almost all 10–20 ms and on every command type.
- **CPUOPT3's shipped changes, in Redis:**
  - **D1** is enabled on no block, so it had no Redis effect.
  - **D3** cut api round trips per request from 1.95 to 1.52 (−22%), about 5.1k/s at equal load (predicted 5.3k/s), ≈0.5 cores.
  - **D2:** proxy contract creates fell from 410 to 207/s, and contract-hole events by 1.4k/s. The proxy's reservation calls rose from 1.3k to 7.4k/s for other reasons (exhaustion loops on 11-03 and 11-04).

**Ranked opportunities** (cores of Redis CPU at the evening peak, 37 cores on 11-03; they overlap, so do not add them all):

| # | Change | Saving | Risk |
|---|---|---|---|
| 1 | No preflight PING by default on the deadline and once pools (§5.1) | 2–3 cores; 25–32k fewer requests/s; one round trip off each packet-path contract-hole read | low |
| 2 | EVAL → EVALSHA for the hot scripts (§5.2) | 0.5–0.8 cores; −55–60 MB/s inbound; 3–5% of a saturated escrow node's main thread | low |
| 3 | `kernel.numa_balancing=0` on edge-6, as on edge-5 (§5.3) | ≈0.4–0.5 cores, plus edge-6's 10–20 ms stalls *(inferred)* | low (runtime sysctl), owner approval |
| 4 | Client-score native pages: reuse a verified page while its slot's generation holds (§5.4) | −200 MB/s Redis output; ≈0.3–0.4 Redis cores, plus proxy CPU on hosts at 96–99% | low–medium |
| 5 | GETEX in the resident poll, and the heartbeat prune inside its MULTI (§5.5, CPUOPT3 §4.5) | ≈0.5–0.7 cores | low |
| 6 | `{provider_hard_exclusions}`: publish only on change, and a generation-checked local copy (§5.6) | removes a ≈150 ms whole-node stall every ~18 s on 6415; up to ≈0.3 cores | low (publish), medium (local copy) |
| 7 | Per-slot statistics on (§5.7, CPUOPT3 §4.6) | names the hot slot in one command; would have named 6383's slot | low |
| — | Owner decisions (§5.8): edge-6 conntrack bypass for Redis LAN traffic; per-NUMA-node cpuset partitions on edge-6 (newidle balancing ≈8.5% of a median node); `iommu=pt` on edge-5 (reboot); io-threads 1 (not advised); SRSO posture | see §5.8 | medium–high |

### 1. Method

- **When and load:**
  - Live captures ran 2026-11-04 10:29–10:58Z, on a Wednesday morning, during the proxy incident: proxies 96–99% busy since about 03Z.
  - This morning's Redis load is higher than any evening this week: 36 cores against 37.0 on the 11-03 evening and 31.7 on the 10-31 evening.
  - Like-with-like comparisons use Mimir for the 18–22Z evenings of 10-31 to 11-03 and for the 08:30–10:20Z mornings, and normalize per request (read event).
- **Deployed revisions** (from `urnetwork_source_info` and `urnetwork_build_info`; `<scratch>/e/out/revtimeline.txt`, `starts.txt`):

  | Job | Version | Server | Connect library | Running since |
  |---|---|---|---|---|
  | api (20) | 2026.11.3-outerwerld+1089433320 | d5cbc2a72 | 0dad308f (build record) | 11-03 22:08–22:16Z |
  | alt (2) | same | d5cbc2a72 | 0dad308f | 11-03 22:18Z |
  | proxy (20) | same | d5cbc2a72 | 0dad308f | 11-03 22:54–23:26Z |
  | taskworker (8) | 2026.11.3-outerwerld+1089300440 | 9e2dcaeed | — | 11-03 19:24–19:28Z |
  | connect (20) | 2026.11.2-outerwerld+1088460100 | 1e94b4d78 | b13c6d5a (HANDOFF-1102-2) | 11-02 19:13–19:38Z |

  - The server builds with `replace github.com/urnetwork/connect => ../connect`, so the connect-library commits come from release records.
  - All CPUOPT3 merges (D1 f7ddb96ef; D2 a556dafc7, 863aa0d9f, c6615cd17, ee22592cc; D3 c0c310a1b, e66f57589) are ancestors of all three server revisions. d3f1508c2 is on origin/main only (`<scratch>/e/out/rev-ancestry.txt`).
- **Which evenings were after which deploy:**
  - **10-31:** before every CPUOPT3 change (api, proxy and taskworker a8461e236; connect 37ec4788b).
  - **11-01:** mixed. Four api processes had D1 and D3 from 15:50Z, and the proxy rolled to f19323066 (D1, D3, ee22592cc) during 17:00–20:10Z.
  - **11-02:** api with D1–D3 from 06:30Z. Connect rolled to 1e94b4d78 during 19:13–19:38Z, and the proxy during 19:50–20:30Z, both inside the window.
  - **11-03:** everything had D1–D3. api and taskworker restarted at 19:20–19:30Z, and the proxy ran its first exhausted-payer episode.
- **Redis host captures** (all through `ansible -m script` and summarized on the host):
  - `snap.sh` three times, A 10:39:44Z, B 10:44:00Z and C 10:54:30Z (885 s A→C). Each run takes `INFO everything` and `LATENCY LATEST` of all 48 nodes, per-thread CPU and context switches of every redis-server, `/proc/stat`, NUMA and swap counters, and per-process CPU.
  - `summ.py` once: `CONFIG GET` of 25 parameters, `CLUSTER NODES` reduced to slot ranges, `SLOWLOG GET 1024`, `LATENCY LATEST` and `CLIENT LIST`.
    - Slowlog entries keep only the command name, duration, time, and for EVAL a sha1 of the body's first 64 bytes plus its length.
    - Client sources are salted-hash pseudonyms, mapped to host names locally (`map-sources.sh`).
  - perf, 30 s at 99 Hz with frame-pointer call graphs, on one process each:
    - 6386 (edge-6, a median node, 2.1k samples);
    - 6383 (edge-6, saturated, 5.0k samples);
    - 6420 (edge-5, CPUOPT3's edge-5 node, 2.2k samples).

    Data files were deleted on the host, and no perf process was left (verified on both hosts). perf's own frequency-adjust overhead was 5.0% of 6386's samples, 1.0% of 6383's and 0.55% of 6420's.
- **Code:** read with `git show` at 1e94b4d78 (connect service), d5cbc2a72 (api, alt, proxy) and 9e2dcaeed (taskworker).
  - EVAL fingerprints were mapped to Lua literals at those revisions (`lua-fingerprints.py`, 123 scripts).
  - Fixed hash tags were mapped to slots and ports (`tag-slots.py`).
  - xops checked at a074338.
- **Not available:**
  - `cluster-slot-stats-enabled` and `latency-tracking` are both `no` on all 48 nodes, so hot slots are inferred from per-node command mix and code.
  - Sprintf-built scripts (the provider-intent family) do not fingerprint; they are identified by node and by the earlier redisburst-1104 work.
  - No per-caller counter exists for the preflight PINGs.

### 2. Where the CPU goes now

#### 2.1 Totals, normalized per request

Process CPU is `used_cpu_sys + used_cpu_user`. "Requests" are `total_reads_processed`. Mean (peak) of 5-minute rates (`<scratch>/e/out/core-*.txt`).

| Window | Process cores | Main threads | Kernel share | Requests/s | Commands/s | µs per request | Clients | New conns/s |
|---|---|---|---|---|---|---|---|---|
| CPUOPT3 3 d, 10-28 18Z – 10-31 18Z | 26.3 (34.1) | 13.9 | 52% | 131k | 609k | 203 | 84k | 106 |
| 3 d to now, 11-01 10Z – 11-04 10Z | 31.2 (43.5) | 15.4 | 52% | 185k | 821k | 172 | 97k | 171 |
| 10-31 18–22Z (before) | 31.7 (34.0) | 16.2 | 52% | 170k | 786k | 187 | 97k | 119 |
| 11-02 18–22Z (deploys in window) | 32.9 (38.9) | 15.7 | 52% | 203k | 896k | 163 | 99k | 153 |
| 11-03 18–22Z (after; proxy exhaustion episode) | 37.0 (40.7) | 18.2 | 59% | 208k | 859k | 182 | 106k | 403 |
| 11-03 08:30–10:20Z (after, quiet) | 28.6 (30.4) | 15.0 | 50% | 152k | 724k | 188 | 102k | 112 |
| 11-04 08:30–10:20Z (now) | 36.2 (38.7) | 16.8 | 51% | 238k | 972k | 153 | 104k | 340 |
| Snapshots A→C, 10:39:44–10:54:30Z | 36.1 | 16.8 | 51% | 239k | 965k | 151 | 94k | 382 |

- **Per request:** cost falls as load per node rises, because fixed loop and wake costs spread over more requests.
  - At matched load the cost is the same as CPUOPT3's: 187 µs at 170k requests/s (10-31 evening) against 188 µs at 152k (11-03 morning).
  - The rise in cores is volume.
- **Commands per request:** 3.9–4.1 now. CPUOPT3's 4.6–4.7.
- **Command time:**
  - Command time from INFO is 11.7 cores (it counts commands inside scripts twice).
  - Event-loop command time is 8.8 cores (`eventloop_duration_cmd_sum`), 59% of event-loop busy time (10.2 + 4.6 cores).
  - Scripts (EVAL 5.22 cores, EVALSHA 1.47) are 76% of that.
- **Hosts (A→C):**
  - edge-6: 25.6 cores busy, of which Redis 25.0 (98%).
  - edge-5: 15.9 cores busy, of which Redis 11.2 and MinIO 3.0–3.2 (MinIO was 7.8 in CPUOPT3).

#### 2.2 Threads, kernel and the cost per request (A→C, `<scratch>/e/out/delta-AC-*.txt`)

| Host | I/O threads (user / sys) | Main threads (user / sys) | Requests/s | µs per request all-in | outside commands | of which I/O thread | Voluntary switches per request |
|---|---|---|---|---|---|---|---|
| edge-6 (EPYC 7601, 8 NUMA nodes) | 13.46 (3.69 / 9.77) | 11.52 (8.13 / 3.39) | 152k | 164 | 124 | 88 | 1.06 |
| edge-5 (EPYC 7352, 2 NUMA nodes) | 5.91 (1.84 / 4.07) | 5.24 (3.92 / 1.32) | 87k | 128 | 97 | 68 | 1.66 |

- **The regression, on CPUOPT3's method:** I/O-thread CPU against requests across nodes and 3-hour windows (`regress.py`, `<scratch>/e/out/regress.txt`).

  | Window | edge-6 µs per request | edge-5 µs per request |
  |---|---|---|
  | 10-29..31 (CPUOPT3's 83.5 / 47.0) | 90.7 (R² 0.96) | 56.5 (R² 0.87) |
  | 11-01 21Z – 11-03 02Z | 73.4 | 46.5 |
  | 11-03 05Z – 11-04 10Z | 80.6–83.2 | 52.5 |

  Window-to-window swings of ±10–15% follow the workload mix and are larger than the IOMMU share. The regression therefore cannot resolve `iommu=pt`; perf can (§2.4).
- **Wakeups:** 1.06 sleep/wake cycles per request on edge-6 (CPUOPT3: 1.41) and 1.66 on edge-5 (1.8). The higher load per node batches more requests per wake.

#### 2.3 Per node and why the hot nodes are hot

Median node: 17.3k commands/s, main thread 0.33. Last hour, 09:30–10:30Z (`<scratch>/e/out/pernode-now.txt`, `nodemix-now.txt`):

| Node | Host | Commands/s | Main | New conns/s | What makes it hot | Evidence |
|---|---|---|---|---|---|---|
| 6383 | edge-6 | 62–70k | 0.73 h-mean; 0.87–0.97 by scrape; 195 loop cycles/s | 86 (h) / 234 (snapshot) | One `{escrow_<balance>}` slot under the proxy's exhausted-payer loop *(inferred)* | EVAL 4.45k/s vs 1.3k median at 109 µs, with the reservation script's inner commands (TYPE 5.1k, EXISTS 4.0k, ZRANGEBYSCORE 3.6k, ZRANGE 2.7k, ZSCORE 2.7k, PEXPIRE 5.7k). EVAL stepped 1.0k → 4.6k/s at 03:00Z, with the proxy's new probe, proof and refusal rates. One balance is one slot by design (`redisContractReservationKeys`, model/subscription_redis_admission.go:175) |
| 6419 | edge-5 | 47–52k | 0.51–0.53 | 4.6–6.2 | The same signature, another busy balance *(inferred)* | EVAL 3.8k, TYPE 3.7k, ZRANGEBYSCORE 2.9k |
| 6416 | edge-5 | 27.6k | 0.34 | 4.5 | `{np_<network>}` of the largest provider network (provider intent and installs) | EVALSHA 1.2k, ZSCORE 3.9k, ZADD 2.5k, TTL 684; hourly 0.5 s walks (§3.2) |
| 6424, 6396 | edge-5, edge-6 | 22.6–24.7k | 0.39–0.44 | 3.5–4.0 | verify-egress forward-lease hashes of the proxy hosts' addresses | HSCAN 1,077/s each at 21 µs (CPUOPT3: 495/s) |
| 6381 | edge-6 | 23.8k | 0.42 | 4.6 | a moderately busy escrow balance *(inferred)* | EVAL 1.7k, TYPE 1.2k, EXISTS 0.8k |
| 6420 | edge-5 | 22.5k | 0.33 | 3.7 | `verify_eligible_v2`, a fixed key in slot 4467 | SREM 3.0k/s, SADD 412/s, EXPIRE 755/s (model/verify_model.go:456, verify_egress_refresh_model.go:268) |
| 6415 | edge-5 | 18.2k | 0.57 | 4.6 | `{provider_hard_exclusions}`, slot 9358 | SMISMEMBER 526/s at 606 µs = 0.32 cores, plus a ≈150 ms SADD republication every ~18 s (§5.6); 39 MB/s of input |
| 6399, 6380 | edge-6 | 17.7–22.1k | 0.36–0.43 | 5.7–7.0 | client-score native pages (FindProviders2) | HMGET 2.0k and 1.5k/s vs 395 median; output 138 and 91 MB/s, most of the cluster's 250 MB/s (§5.4) |

- **Before the surge (01:30–02:30Z),** the top nodes were within 14–23k/s: 6416, 6381, 6427, 6420, 6419, 6424 (`pernode-0130-0230.txt`). That matches the earlier reading on 11-04 of 6420, 6381, 6424 and 6382 at about 20k/s each.
- **The 11-03 evening** had the same pattern on other balances: 6397 at 51k/s with 122 new connections/s, and 6392 with main 0.80 (`pernode-1103E.txt`).
- **The hot balance and the deadline pool:**
  - Calls to a hot balance go through the deadline pool: 1 s socket timeouts, at most 8 connections per node, and PoolTimeout 1 s (redis.go:200–211 at 1e94b4d78).
  - The proxy's pool timeouts rose from ≈0 to 277/s (hour mean), and to 658/s at 08:00Z.
  - The reservation results show 117/s `reserve-pending` and 96/s `release-pending` errors.
  - The grant probe (subscription_redis_grant_probe.go:173) runs without keep-late replies, so each late reply drops its connection. This is consistent with 234 redials/s on 6383 *(inferred)*.

#### 2.4 perf, as a share of process samples (`<scratch>/e/out/perf-*.txt`)

| | 6386 (edge-6, median) | 6383 (edge-6, saturated) | 6420 (edge-5) | CPUOPT3: 6383 / 6420 |
|---|---|---|---|---|
| Threads: I/O / main | 63% / 36% | 50% / 50% | 59% / 41% | — |
| Scripts (`evalGenericCommand`, inclusive) | 14.6% | 38.3% (≈77% of the main thread) | 14.7% | 9% / 19% |
| SHA1 of EVAL bodies (`SHA1Transform`) | 0.43% | 1.62% (≈3% of the main thread) | 1.37% | 1.6% / 1.6% |
| IOMMU map/unmap | none listed | none listed | `__iommu_map` 0.63% self | 3.3% / 3.7% |
| SRSO return thunks | 1.9% | 2.5% | 4.1% | 1.6% / 7.2% |
| Newidle load balancing (`sched_balance_newidle`, inclusive) | 8.5% (`update_sg_lb_stats` 3.9% self) | below 1% | 0.95% | not broken out |
| Netfilter (`nf_hook_slow` inclusive; conntrack lookup self) | <1%; 0.83% | 5.1%; 1.18% + 0.51% | none | — |
| Cluster key checks (`getNodeByQuery`) | 2.3% | 1.3% | 0.73% | — |
| Client-memory accounting (client eviction on) | 1.9% | <0.5% | 0.8% | 0.8% / 1.1% |

- **IOMMU:** the reboot removed it from edge-6. By CPUOPT3's share, that is ≈3.3% of edge-6 Redis, about 0.8 cores.
- **Newidle balancing** is new in this breakdown. A median edge-6 node spends about 8.5% of its CPU in the scheduler's balance on going idle; edge-5 spends about 1%. It follows the 8-node topology and the 1–1.7 sleeps per request. A saturated node never idles, so it pays none.
- **Netfilter:** edge-6 runs docker and ufw, so `nf_conntrack` tracks every Redis connection (82k entries). The cost rises with connection churn: 5.1% on 6383, which redials 234 times a second. edge-5 does not load conntrack (xops minio playbook).

#### 2.5 Commands (A→C, 885 s; `<scratch>/e/out/delta-AC-*.txt`, `cmd-compare.txt`)

| Command | Calls/s | Cores | µs/call | 10-31 18–22Z calls/s (cores) | 11-03 18–22Z calls/s (cores) |
|---|---|---|---|---|---|
| EVAL | 72.0k | 5.22 | 72 | 67.4k (4.74) | 69.4k (4.88) |
| EVALSHA | 19.1k | 1.47 | 77 | 19.1k (1.58) | 14.8k (1.16) |
| SET | 46.3k | 0.75 | 16 | 48.9k (0.81) | 40.7k (0.67) |
| EXEC | 16.5k | 0.73 | 44 | 9.8k (0.40) | 11.7k (0.42) |
| HDEL | 8.2k | 0.38 | 46 | 4.0k (0.15) | 4.8k (0.16) |
| SMISMEMBER (one node) | 475 | 0.32 | 662 | 183 (0.12) | 340 (0.18) |
| GET | 107k | 0.32 | 3.0 | 92.2k (0.40) | 93.9k (0.28) |
| HMGET | 23.8k | 0.28 | 12 | 17.8k (0.28) | 20.5k (0.23) |
| PEXPIRE / PEXPIREAT / EXPIRE | 44.1k / 32.0k / 20.6k | 0.30 / 0.20 / 0.13 | 6–7 | 43.9k / 22.6k / 13.8k | 39.6k / 32.3k / 16.9k |
| HINCRBY / ZADD / HSET | 51.8k / 43.8k / 31.0k | 0.20 / 0.22 / 0.16 | 4–5 | 31.0k / 26.0k / 25.4k | 41.5k / 37.2k / 29.7k |
| PING | 32.3k | 0.02, but a full request each | 0.6 | 18.5k | 32.4k |
| SPUBLISH | 0.9k | 0.04 | 40 | 1.3k (0.04) | 0.96k (0.42 at 442 µs: a transient, 11-03 16–24Z) |

- **Script cache:** 63 scripts on 6383, 0 evicted.
- **EVAL bodies:** they are still sent each call, at least 55–60 MB/s of the 207 MB/s inbound now:
  - reservation 5,849 B × ≈4.4k/s;
  - contract-hole read 1,301 B × 15.3k/s;
  - contract-hole event 1,961 B × 3.2k/s;
  - recovery page 1,416 B × 3.4k/s, in bursts to 14k/s;
  - grant probe 755 B × ≈2.6k/s.

#### 2.6 Host posture (`<scratch>/e/out/posture-*.txt`, `edge6-prev-boot-end.txt`)

| | edge-6 | edge-5 |
|---|---|---|
| Boot | 11-03 04:23:51Z, after an unclean end at 02:50:15Z | 10-23 19:58Z |
| IOMMU | `iommu=pt` active (127 groups identity) | staged in grub, still DMA-FQ (126 groups) |
| NUMA balancing | on: 191–201k hint faults/s, 41–44k page migrations/s (CPUOPT3: 165k and 36.8k) | off since 11-01 (xops 7679528); 0 migrations |
| Unit placement | no `CPUAffinity`, `NUMAPolicy` or `NUMAMask` on any `redis-cluster@` unit | same |
| Swap | 52 MB used of 8 GB | 8.0 of 8.0 GB used; 46 swap-ins/s; 9.7 major faults/s. `MemorySwapMax=0` on all Redis units (xops 0621da2), so new swap-outs are stopped and old pages return as touched |
| Netfilter | docker and ufw active; `nf_conntrack` loaded, 82k entries | not loaded |
| SRSO / retbleed | Safe RET / untrained return thunk, STIBP off | Safe RET / untrained return thunk, STIBP always-on |

#### 2.7 Slowlog and latency events (`<scratch>/e/out/slowlog-*.txt`)

All 48 slowlogs are full (1,024 entries; `slowlog-max-len 1024`, threshold 10 ms).

- **edge-6:** 58–130 entries per node-hour, with a 602/h burst on 6411.
  - 94% are 10–20 ms, and none reached 100 ms.
  - They hit every command type: the liveness-lease shard script (3,416 entries), contract-hole read (2,241), SET (2,119), and GET at 13 ms.
  - Simple commands stalling as long as scripts point to host stalls, not command cost *(inferred)*.
  - `LATENCY LATEST` adds BGSAVE fork stalls on 19 nodes: p50 138 ms, max 324 ms.
- **edge-5:** split at 11-01 20:00Z, when NUMA balancing went off.
  - Before: 170–210 entries per node-hour, p50 ≈70 ms, 15% of them at 100 ms or more.
  - After: 0.2–3.6 per node-hour, except two nodes:
    - **6415:** the exclusions SADD, 934 entries in 4.8 h, mean 152 ms, max 309 ms, plus SMISMEMBER at ≈25 ms;
    - **6416:** the provider-intent EVALSHA walk, mean 436 ms, max 574 ms (§3.2).

### 3. What CPUOPT3's shipped changes and its Part E items did

#### 3.1 D1, D2, D3

| Change | Predicted for Redis | Measured | Confidence and confounders |
|---|---|---|---|
| D1 in-process delivery (f7ddb96ef) | none directly | `urnetwork_connect_exchange_local_transport_enabled` is 0 on beta and g1–g4, and `exchange_local_frames_total` is 0: dark on every block | high. No Redis effect is possible yet |
| D2 window handover | fewer mints and contracts (265–383/s at the platform) | Proxy contract-hole creates: 410/s (10-31 18–22Z) → 207/s (11-03 18–22Z), −50%. Cluster-wide creates 1,295 → 948/s and removes 3,133 → 2,083/s, ≈1.4k fewer events/s; each is a 2 KB EVAL plus a PING, ≈2.8k fewer requests/s, ≈0.25 cores. Proxy reservation operations rose 1.3k → 7.4k/s in the same windows | medium. App versions, D3 and load overlap. The reservation growth is the exhausted-payer loops (`exhaustion-proof`, `reserve-probe`, `recovery pending`), not D2 |
| D3 PeerAudit discard (c0c310a1b, e66f57589) | ≈7.5k fewer round trips/s, ≈0.7 cores; api `redis_pool_hits` −5.3k/s, proxy −2.2k/s | api discards 3.0–3.1k/s. api Redis round trips per api request 1.95 → 1.52 (−22%), ≈5.1k/s fewer at equal request rate. EVALSHA −4.4k/s (19.1k → 14.8k). Proxy discards are 0, and its saving cannot be separated | high for the api share (≈0.5 Redis cores at ≈98 µs per request), unobservable for the proxy |

#### 3.2 CPUOPT3 Part E items, checked on server origin/main and the deployed revisions (and xops a074338)

| Item | Status | Value now |
|---|---|---|
| §4.1 contract-hole reads and events without the preflight PING | Not done. `RedisWithDeadline` without `OptRedisNoPreflight` remains at model/subscription_contract_hole.go:109, :276, :371, :407 and subscription_contract_hole_readiness.go:110, :122, :133 (1e94b4d78, d5cbc2a72, origin/main). 38 call sites still preflight (`<scratch>/e/out/preflight-callers-*.txt`) | PING 32.3k/s. Contract-hole calls are 15.3k reads plus 3.2k events per second. The rest came from 11-02 01:00Z, after connect 53c9c767a and taskworker b98f384b3, and is not attributable per caller (client-close takes, ~700/s, do not track it) |
| §4.2 provider-intent `count_clients` | Done as an exact exempt index: 4f4581377 (sampled shadow decisions), b6ebb4411 (index), 434a578fb (drop a disagreeing index), a7d06ad03 (index scripts by sha), 15b5124ac. In every deployed revision since connect 53c9c767a (11-02 00:10Z) | Counts 0.25/s, from the index. The hourly rebuild still walks the largest network: 47 walks per 24 h, mean 436 ms, max 0.57 s on 6416 |
| d3f1508c2 (755d3fcd0) re-sweep in place | On main only. Needs connect, alt and taskworker | It changes Redis CPU negligibly. Each count of a network over 4,096 members re-sweeps at most 1,024 members (≲7.2k commands, ≈10 ms) instead of 1–7 walks of ≈0.5 s an hour: about the same or slightly less CPU (≈0.001 core). It removes 6416's hourly half-second whole-node stalls |
| §4.3 EVAL → EVALSHA | Done only for the provider-intent index scripts (a7d06ad03). The reservation, contract-hole, recovery, probe, verify-egress, session and connection-authority scripts are still `Eval` | EVAL 72k/s, 5.2 cores; ≥55–60 MB/s of script bodies |
| §4.4 NUMA pinning / `iommu=pt` | No unit pinning (live properties empty, nothing in xops). `iommu=pt` became active on edge-6 by the unplanned 11-03 boot; still staged on edge-5. NUMA balancing off on edge-5 (11-01), on on edge-6 | IOMMU gone from edge-6 profiles; `__iommu_map` 0.63% self on edge-5 |
| §4.5 GETEX, prune inside the MULTI | Not done: model/network_client_resident_model.go:49–60 (GET, then EXPIRE); peer_model.go:1209–1219 (proxy peer: MULTI, then the prune at :1219, defined at :1336); the client-peer paths at :869 and :1029 (prune after the transaction, defined at :1893) | EXPIRE 20.6k/s, ZREMRANGEBYSCORE 34k/s |
| §4.6 per-slot statistics | Not done: `cluster-slot-stats-enabled no` on 48 of 48; not in redis.conf.j2 | — |
| §4.7 `{provider_hard_exclusions}` | Not done | SMISMEMBER 0.12 → 0.32 cores on 6415 (2.6×), with the proxy's FindProviders2 rate |
| §4.8 backstop TTLs once | One `ExpireNX` use added in model/ (none at a8461e236). The reliability-stats EXPIRE (network_client_reliability_model.go:366) is unchanged | PEXPIRE/PEXPIREAT/EXPIRE 97k/s, 0.63 cores |
| io-threads | `io-threads 2` (one I/O thread) on 48 of 48, unchanged | I/O threads 19.4 of 36.1 cores (54%), 70–73% system time |

### 4. CPUOPT3 predictions checked

- **§4.1, "−13–16k requests/s, ≈1.2–1.6 cores":** not shipped. The target doubled to 25–34k/s (§5.1).
- **§4.2, "per-call blocking 340–450 ms → ≈70–110 ms (steps 1–2), step 3 removes it":** the index (step 3's O(log N) count) shipped.
  - Steady-state walks are gone. Only the rebuild-age walks remain, and d3f1508c2 removes those.
  - The slowlog entry rate on 6416 fell from 706 per hour (10-31 evening, NUMA stalls included) to 3.6 per hour.
- **§4.3, "0.5–0.7 cores, 40–50% of inbound bytes":** not shipped.
  - Script bodies are now ≈27–29% of inbound, against CPUOPT3's 40–50%, because inbound grew from other sources: SMISMEMBER arguments alone are 39 MB/s into 6415.
  - The CPU estimate stands and grows with EVAL volume (§5.2).
- **§4.4, "IOMMU ≈3.5%":** confirmed on edge-6 by elimination: no IOMMU symbol remains after `iommu=pt`. The NUMA side was not tested: edge-6 is still unpinned and balancing.
- **§4.9, the edge-5 co-location latency:** largely resolved by NUMA balancing off and `MemorySwapMax=0`, not by moving nodes. edge-5 now stalls far less than edge-6 (§2.7).
- **CPUOPT2 §8.6 #4, the proxy page cache:**
  - It held through 11-02: output 82.5 MB/s on the 3-day basis then.
  - Output is back to 250 MB/s now. The page cache saves decoding, not transfer (§5.4).
- **D3, "Redis −7.5k round trips/s":** api share measured at −5.1k/s (§3.1).

### 5. Ranked opportunities

#### 5.1 No preflight PING by default (CPUOPT3 §4.1, widened)

- **Change:**
  - In redis.go, make `redisWithClient` skip the PING unless a caller asks for it: invert `RedisPreflightOptions`, or pass `OptRedisNoPreflight()` at all 38 sites in `<scratch>/e/out/preflight-callers-d5cbc2a72.txt`.
  - First among them are model/subscription_contract_hole.go:109, :276, :371, :407 and subscription_contract_hole_readiness.go:110, :122, :133.
  - Then `TakeClientCloseRequests` (model/client_close_model.go:167, `RedisDoOnce`), clock, egress tally, resident retirement, onboarding, network health, degradation, metrics cache, handler-cache renew and the others.
- **Code:** redis.go:582–592 (the option), :597–627 (the PING at :626). The pools are the deadline pools (`safeDeadlineClient`, `safeLateReplyClient`) and `safeNoCommandRetryClient`.
- **Evidence:**
  - PING runs at 32.3k/s (A→C), against 18.5k/s on the 10-31 evening.
  - ≈15–18k/s match contract-hole operations one for one, as in CPUOPT3. The excess grew from 0.6k/s to 4–20k/s starting 11-02 01:00Z (`<scratch>/e/out/ts-ping-excess.txt`).
  - Each PING is its own request on a random node. The option's own comment (redis.go:586–589) says it "guards no command's outcome".
  - Admission, recovery, the grant probe, the liveness lease, the debit flush and the D-plan controls already skip it.
- **Saving:** 25–32k fewer requests/s at the evening peak.
  - At 75–98 µs per request (the fixed per-event cost: I/O-thread 52–83 µs by the regression, plus main-thread handling), that is ≈1.9–3.1 Redis cores, 5–8%.
  - Client side: 25–32k fewer pool turns and round trips per second in connect, api and proxy. A cold packet-path contract-hole read also loses one round trip.
- **Risk:** low. A connection error surfaces from the first real command instead of the PING, with the same error path: these pools never replay a command.
- **Why no timing bound moves:** each callback keeps its own context deadline (1 s `contractHoleRedisTimeout` and the others). The PING only shared that budget, so the command gets the whole of it.
- **Validation:**
  - `redis_commands_total{cmd="ping"}` falls by 25–32k/s, and `redis_total_reads_processed` by the same.
  - `urnetwork_contract_hole_operations_total{outcome=~"read_error|event_error"}` and the connect allowance outcomes stay flat.
  - µs per request on the Redis hosts stays within the regression's band.

#### 5.2 EVAL → EVALSHA for the hot scripts (CPUOPT3 §4.3)

- **Change:** `redis.NewScript(...).Run` (EVALSHA with NOSCRIPT fallback) for:
  - `redisContractReservationScript` (model/subscription_redis_admission.go:191, called at :319);
  - `contractHoleReadScript` and `contractHoleEventScript` (model/subscription_contract_hole.go:56, :185);
  - `redisReservationRecoveryPageScript` (model/subscription_redis_recovery.go:116);
  - the grant probe (subscription_redis_grant_probe.go:31, pipelined at :175);
  - the verify-egress lease scripts (verify_egress_lease_model.go:46, :103);
  - `sessionLuaCommon` (session/session_model.go:87);
  - `connectionAuthorityPublishLua` (session/connection_authority.go:27).

  Pipelines (the probe pipeline and the debit flush at subscription_transfer_debit.go) use `EvalSha` and re-run only NOSCRIPT failures with `Eval`, as verify_egress_refresh_model.go already does.
- **Evidence:**
  - EVAL runs at 72k/s, ≥55–60 MB/s of bodies, each SHA1-hashed (perf: 0.43–1.62% of samples).
  - On the saturated 6383, SHA1 is ≈3% of the main thread, and the 5.8 KB reservation body is parsed and allocated on every call.
- **Saving:** ≈0.5–0.8 cores fleet-wide (SHA1, body allocation and parsing, kernel receive). That is 3–5% of a saturated escrow node's main thread, which is where it matters most today.
- **Risk:** low. 63–79 scripts per node, 0 evicted. A NOSCRIPT after a restart or flush costs one round trip once per node and script. A pipeline must treat NOSCRIPT as "retry with EVAL", never as a reservation result.
- **Why no timing bound moves:** the same script runs, under the same deadline.
- **Validation:**
  - EVAL falls and EVALSHA rises by the same count.
  - `redis_net_input_bytes_total` falls by ≈55–60 MB/s.
  - `SHA1Transform` leaves the profiles.
  - 6383-type nodes show lower µs per EVAL.

#### 5.3 `kernel.numa_balancing=0` on edge-6

- **Change:** extend xops 7679528's task (playbook-minio-clusters.yml:417–437, edge-5 only) to every `redis-nodes` host: `sysctl -w kernel.numa_balancing=0`, live, with no restart.
- **Evidence:**
  - edge-6 hint-faults 191–201k pages/s and migrates 41–44k/s (≈170 MB/s copied). CPUOPT3 put the direct cost at ≈1.5% of edge-6 Redis.
  - Placement is already poor: CPUOPT3 found a median 44% of each process's pages on its majority node.
  - On edge-5 the same change cut slow entries by about 99% (§2.7). edge-6 now logs 58–130 slow entries per node-hour across all command types *(inferred to be host stalls)*.
- **Saving:** ≈0.4–0.5 cores, plus the stall tail if the inference holds.
- **Risk:** low. It is a runtime sysctl, reverted by writing 1. Memory stays where first touch put it, as it effectively does now.
- **Why no timing bound moves:** placement only.
- **Validation:**
  - `numa_pages_migrated` goes to 0.
  - Slow entries per node-hour on edge-6 (rate of `redis_slowlog_last_id`) fall.
  - µs per request at equal load does not rise. Compare a week of edge-6 against edge-5, which has both changes.

#### 5.4 Client-score native pages: reuse a verified page while its generation holds

- **Change:**
  - In `clientScoreCursor.readWithClient` (model/network_client_score_cursor.go:165–245), a page whose checksum the process already holds (the page cache, model/network_client_score_page_cache.go) is read as `HGET <slot key> g` only, or once per slot key per pipeline.
  - The full `HMGET <slot key> g <page>` and the sha256 verification run only on a cache miss.
- **Evidence:**
  - Proxies read ≈2.3k pages/s now (1.9k cache hits, 0.39k misses; 1.24k/s on the 11-03 evening).
  - Every read transfers the page, ≈85–95 KB on the wire, and re-hashes it (:226), even on a hit.
  - Nodes 6399 and 6380 emit 138 and 91 MB/s, most of the cluster's 250 MB/s output.
- **Saving:**
  - ≈0.3–0.4 Redis cores (6399 and 6380 run 0.15–0.26 cores above the median node).
  - About 200 MB/s less to receive, plus the sha256, on proxies at 96–99% busy. Part B should size that.
- **Risk:** low–medium. The design must hold that a page's bytes never change within one (slot key, generation). The comment at network_client_score_native.go:396–398 relies on generation and checksum to make a reused slot unavailable. With the generation still read on every use, a reused slot still reads as "missing or changed".
- **Why no timing bound moves:** no cadence or TTL changes; the same snapshot is answered.
- **Validation:**
  - Output bytes on 6399 and 6380, and `urnetwork_client_score_page_cache_lookups_total` against HMGET rate.
  - `client_score_read_errors_total` unchanged.
  - A test that a reused slot with a new generation is refused.

#### 5.5 One round trip instead of two (CPUOPT3 §4.5, unchanged)

- **Change:**
  - `GetResidentForClient` uses `GETEX key EX ttl` (model/network_client_resident_model.go:49–60).
  - `AddNetworkProxyPeer` queues its prune inside its TxPipeline (peer_model.go:1209–1219). So do the client-peer paths that prune after their transaction (:869, :1029).
- **Saving:** −6–7k requests/s, ≈0.5–0.7 cores, plus connect CPU. The resident poll (75 s against 300 s TTL) and the heartbeat cadences stay.
- **Risk:** low. An undecodable resident record also has its TTL extended, and `NominateResident` replaces it, as CPUOPT3 noted.
- **Validation:** EXPIRE falls by ≈5k/s; resident-loss and peer-count metrics unchanged.

#### 5.6 `{provider_hard_exclusions}` on 6415

- **Change, in two steps:**
  1. In `publishProviderHardExclusions` (model/egress_index.go:1044–1066), publish only when the member set changed: compare a digest kept beside the set, and on no change only re-arm its EXPIRE. Otherwise write a staging key under the same hash tag in chunks and RENAME it into place. Readers still see one atomic publication.
  2. Then CPUOPT3 §4.7's generation-checked local copy for `getProviderHardExclusions` (:1100–1130). It is only worth it once (1) makes changes rare, because the publisher, `UpdateClientLocations`, runs again 5 s after each run (taskworker/work/network_client_location_work.go:61–71).
- **Evidence:**
  - The slowlog shows SADD at 152 ms mean (max 309 ms) about every 18 s on 6415. Each one blocks every key on the node.
  - SMISMEMBER is 0.32 cores at 662 µs, with ≈70 KB of arguments per call (39 MB/s of input).
- **Saving:** step 1 removes ≈0.9% of 6415's time in 150 ms blocks; a latency gain, ≈0.01 core. Step 2 saves up to ≈0.3 cores of 6415's main thread, and FindProviders2 latency.
- **Risk:** low for step 1. Medium for step 2: the cohort and ready-marker rules must hold for the cached copy.
- **Why no timing bound moves:** the check cadence and the TTL stay as they are; only identical rewrites are skipped.
- **Validation:** SADD slowlog entries on 6415 go to near zero; SMISMEMBER rate (step 2); the exclusion and coverage outcomes unchanged.

#### 5.7 Per-slot statistics (CPUOPT3 §4.6)

- **Change:** `cluster-slot-stats-enabled yes` in templates/redis/redis.conf.j2. `CONFIG SET` can apply it live, but that is a production write and needs the owner's go.
- **Why now:** today's incident needed a slot name. `CLUSTER SLOT-STATS ORDERBY cpu-usec LIMIT 10` would have named 6383's balance slot without MONITOR.
- **Cost:** per-command accounting, expected well under 1%. Measure it on one node first.

#### 5.8 Decisions and experiments for the owner

- **edge-6 conntrack bypass:**
  - `CT --notrack` rules for Redis node and bus ports from the LAN (raw table), if ufw's rules admit untracked packets on those ports.
  - The saving is ≈1% of typical edge-6 Redis CPU and 5% on a node with redial churn, ≈0.3–0.5 cores.
  - This is a firewall change; medium risk.
- **Per-NUMA-node cpuset partitions on edge-6:**
  - Newidle balancing is ≈8.5% of a median edge-6 node, about 2 cores across the host. NUMA locality is CPUOPT3's other half.
  - `AllowedCPUs=` is a cgroup property that can be set at runtime, but only a partition root shrinks the balance domains.
  - Test on one node first. This is CPUOPT3 §4.4, now with a measured kernel share.
- **`iommu=pt` on edge-5:** needs a reboot of 16 nodes (a third of the slots), with no replicas. The saving is ≈0.6% self, ≈1–3% inclusive, of edge-5 Redis.
- **io-threads 1:** not advised. 6383's main thread is saturated already, and it would also absorb ≈0.84 cores of I/O work.
- **SRSO posture:** the return thunks are 4.1% of edge-5 and 1.9–2.5% of edge-6. A security call; needs reboots.
- **edge-6's crash:** not a CPU item. The 11-03 02:50Z crash took two thirds of the slots out for 94 minutes, with a disk hardware error logged before it. The disk and BMC logs deserve a look, and the no-replica posture deserves a fresh decision.

### 6. Considered and rejected under the owner rule

- **`OptRedisKeepLateReplies` on the grant-probe and exhaustion-proof reads,** to stop 6383's redials. The option lets a call end up to 1 s after its deadline (redis.go:564–578), which loosens the caller's bound. Cut the calls instead: §5.2 here, and the exhausted-payer loop in Part B.
- **Raising the deadline pool's 1 s PoolTimeout, ReadTimeout or per-node cap of 8** to absorb the hot node. Each loosens a bound.
- **Raising `contractHoleRedisTimeout` (1 s) or `ContractManagerCheckTimeout` (5 s),** to cut contract-hole reads. These are revocation-visibility bounds. Remove the PING instead (§5.1).
- **Lengthening the resident poll (75 s), the liveness-lease cadence (25 s) or the `UpdateClientLocations` loop (5 s after each run).** These are detection bounds. §5.5 and §5.6 keep the cadences and make each pass cheaper.
- **Caching revocation reads, or a per-process `/connect/control` token bucket** (as in CPUOPT3). Each would delay revocation or multiply a client's allowed rate.
- **Turning off client eviction (`maxmemory-clients 0`) to save its ≈1–2%.** It is a memory-safety bound on 48 nodes with no replicas.
- **Skipping provider-intent counts while enforcement is in shadow.** That would change the rollout's evidence; the index made counts cheap instead.

### 7. Raw outputs

All under `<scratch>/e/` (`<scratch>/e/`).

- **Scripts (`scripts/`):**
  - Mimir: `mq.py`, `winstats.py`, `multits.py`, `cmdcompare.py`, `pernode.py`, `nodemix.py`, `regress.py`, `starts.py`, `revtimeline.py`.
  - Hosts: `host-posture.sh`, `prev-boot-end.sh`, `snap.sh`, `summ.py`, `perf-node.sh`.
  - Local: `run-script.sh` (masks IPv4), `map-sources.sh` (prints pseudonyms and host names only), `snapdelta.py`, `slowlog-analyze.py`, `lua-fingerprints.py`, `tag-slots.py`, `preflight-callers.py`.
  - Window and expression files.
- **Mimir (`out/`):**
  - totals: `core-evening.txt`, `core-morning.txt`, `core-long.txt`, `ts-2h.txt`;
  - commands: `cmd-compare.txt`;
  - per node: `pernode-now.txt`, `pernode-0130-0230.txt`, `pernode-1103E.txt`, `pernode-1031E.txt`, `nodemix-now.txt`;
  - PING: `ts-ping-1101.txt`, `ts-ping-excess.txt`, `ts-ping-candidates.txt`;
  - slow entries and churn: `ts-slow-newc.txt`;
  - shipped changes: `d3-evening.txt`, `d2-evening.txt`;
  - per-request cost: `iommu-did.txt`, `regress.txt`;
  - revisions: `starts.txt`, `revtimeline.txt`, `rev-ancestry.txt`.
- **Hosts (`out/`):**
  - posture: `posture-edge-6.txt`, `posture-edge-5.txt`, `edge6-prev-boot-end.txt`;
  - snapshots: `snapA-*`, `snapB-*`, `snapC-*` (INFO, threads, processes), with deltas in `delta-AB-*`, `delta-AC-*`;
  - summaries: `summ-edge-6.txt`, `summ-edge-5.txt` (config, slot map, slowlog names and fingerprints, latency, client summaries with pseudonymous sources), mapped by `source-map.txt`; slowlog analysis in `slowlog-edge-6.txt`, `slowlog-edge-5.txt`;
  - perf: `perf-6386.txt`, `perf-6383.txt`, `perf-6420.txt`.
- **Code (`out/`):** `preflight-callers-{37ec4788b,1e94b4d78,d5cbc2a72}.txt`, `lua-fp-{1e94b4d78,d5cbc2a72,9e2dcaeed}.tsv`, `tag-slots-*.txt`.
- **Not captured:** a per-caller attribution of the non-contract-hole PINGs (no counter exists, and a client perf sample would hold too few PING samples to split them); per-slot CPU (slot stats are off).
- **Nothing was refused.**
