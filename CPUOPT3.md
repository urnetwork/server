# CPUOPT3 — fleet CPU after CPUOPT2 (2026-10-31)

CPUOPT2 (2026-10-29) found the exchange keepalive, timer, forward-tail, proxy page-cache and kernel costs. Its changes reached main on 10-30 and 10-31: connect with keepalive and full deadlines restored, proxy, api, alt and taskworker. This document measures the fleet again on 10-31 at the Saturday evening peak, checks what CPUOPT2's changes did, and ranks what is left.

Five read-only investigations, one per CPU domain, wrote the five parts:

| Part | Domain | Hosts |
|---|---|---|
| A | Services edges and connect | edge-0, edge-1, edge-3, edge-4 |
| B | Proxy hosts and the proxy service | crisp, fireside |
| C | api, alt and taskworker | the services edges |
| D | PostgreSQL | edge-2 |
| E | The Redis cluster | edge-5, edge-6 |

Evidence came from Mimir, short perf and bpftrace captures (data files deleted on the hosts), `pg_stat_statements`, Redis INFO and SLOWLOG, and code read at the deployed revisions (server a8461e236, connect 89b15920). No production state was changed.

What has been built from the plans, and where each change stands, is under "Design plans", in "Changes made so far".

Section numbers inside a part are local to that part: A §6.1 is Part A's section 6.1. Raw outputs and scripts are cited as `<scratch>/…`. They stayed in the analysis workspace and are not committed.

**Owner rule (binding):** "We can't sacrifice correctness of performance timings for the cpu savings." Nothing proposed here loosens a timeout, deadline, keepalive, probe, detection or revocation bound. Each part lists what it rejected under that rule.

## Summary

### The fleet

Busy CPU by host, as of 2026-10-31T18:24Z:

| Host | Cores | CPUOPT2 window 10-22..29, mean / peak busy % | Last 3 days, mean / peak busy % | Mean busy cores, last 3 days |
|---|---|---|---|---|
| edge-0 | 96 | 41.0 / 84.0 | 37.2 / 54.5 | 35.7 |
| edge-1 | 72 | 57.9 / 86.1 | 56.6 / 75.6 | 40.7 |
| edge-2 | 96 | 39.1 / 92.9 | 47.0 / 96.6 | 45.1 |
| edge-3 | 72 | 70.0 / 98.1 | 53.9 / 76.5 | 38.8 |
| edge-4 | 72 | 65.7 / 96.2 | 54.8 / 77.3 | 39.5 |
| edge-5 | 96 | 21.4 / 58.1 | 16.5 / 29.4 | 15.8 |
| edge-6 | 128 | 14.7 / 23.3 | 15.6 / 18.8 | 20.0 |
| crisp | 24 | 45.3 / 99.9 | 43.0 / 99.9 | 10.3 |
| fireside | 24 | 45.3 / 99.9 | 46.1 / 99.9 | 11.1 |

The fleet averages about 257 busy cores.
- **Busiest services edges:** edge-3 and edge-4 peaked at 96–98% in CPUOPT2's window, and at about 77% over the last 3 days.
- **edge-2:** its peak rose from 92.9% to 96.6%, driven by long snapshots rather than steady load (Part D §2.7).

### Where the CPU goes at the evening peak

- **Services edges (A):** 176–178 of 312 logical CPUs busy, serving 197–210k clients.
  - **By process:** connect 56–59% of busy CPU, nginx about 15%, api about 10%, taskworker about 7%, logging and observability 4–5%. Kernel time is about 40% of busy.
  - **Inside connect:** exchange socket trees 27%, H1 transports 15.5%, Go timer-heap scans 7.2–8% (down from 10–13%), Redis client 6.6–9.6%, the liveness-lease writer 4%.
- **PostgreSQL (D):** about 41 cores at measurement.
  - **By process:** backends 29.1 cores (71%), PgBouncer 5.6 (73% of it kernel time), and the nginx stream hop in front of PgBouncer 4.1 (80% kernel).
  - **Inside the backends:** about 47% of CPU is per-statement overhead (planning, executor start, protocol) rather than execution.
  - **The largest swings are long snapshots:**
    - the 10-29 pg_dump took edge-2 from 37 to 92 cores, about 455 extra core-hours;
    - each of the 10-30 census runs added 15–25 cores for up to 3 hours.
- **Redis (E):** process CPU averages 26.3 cores, peaking at 34.
  - A request costs about 200 µs all-in. About half of that is kernel time, spent handing the request between I/O and main threads.
  - Scripts are 70% of command time.
  - One O(N) Lua loop (`count_clients`) stalls a whole node for 340–452 ms at a time.
- **Proxy hosts (B):** 12–13.5 cores at peak across both hosts, 0.5–0.6 cores per process.
  - **CPU split:** keeping windows up about 37%, creating and retiring window clients about 19%, scheduler 13–17%.
  - **No leak:** CPU, goroutines and memory track platform transports, one per window client.
- **api, alt and taskworker (C):** api averages 13.0 cores (17.4 at peak), taskworker 11.5 (13.2), alt 0.45.
  - **api:** 83% of requests are `POST /connect/control`, and only about a fifth of those do real work. JWT handling is about 19% of api CPU.
  - **taskworker:** the provider egress probe uses at least 55% of its CPU, and 17% goes to TLS chain verification.

### What CPUOPT2's changes did

- **Services edges (A §3):**
  - Busy CPU per 1k clients fell 18%, and connect cores per 1k clients 16%.
  - Per client, exchange connection ends fell 51%, goroutines 40% and connect heap 31%.
  - Even with keepalive restored, keepalive probes fell from 170k/s to 0.7–1.0k/s per host.
  - The restored full deadlines cost about 1.5% of connect CPU.
  - The connect release and the kernel reboots both landed on 10-30, so their effects can't be separated.
- **Redis (E §5):** the proxy page cache cut Redis output bytes 50%, SMISMEMBER time 51% and proxy round trips 55%. Redis CPU fell 11% day over day.
- **PostgreSQL (D):**
  - The PgBouncer queue and the idle-in-transaction pile-up went away as the services edges gained headroom. Idle-in-transaction averaged 33 on 10-31, against 566 on 10-22. No client has waited in PgBouncer since 10-27.
  - Commits fell about 30% from 10-28, but no single change explains it.
- **Not shipped:**
  - **api GOGC=400:** its predicted 12–15% came from a sample that caught a GC cycle, and it is now worth about 1%.
  - **The JWT verification cache:** its real cost (about 19%) is larger than the predicted 11%.
  - **The staged kernel options:** on edge-2 and on edge-5/6, not yet rebooted.
  - **Moving edge-6 into the services tier:** Redis NUMA pinning has to come first.

### Corrections to CPUOPT2

- **Redis and pgx in connect:** CPUOPT2 put each under 0.25% of connect. That was wrong: its grep anchored each pattern at the start of the symbol, and these symbols begin with `github.com/`. Counted inclusively, the Redis client is 6.6–9.6% of connect (A §2.3) and 12.6% of an api process (E §5).
- **Proxy "per process" figures:** the brief's numbers were per block, because `proxy_canary.py` sums both hosts. Per process, see B §2.2.
- **Taskworker:** it is no longer "database-bound, not a target". The egress probe's TLS is a target (C §2.6).

## Ranked opportunities across the fleet

Savings are in cores at the evening peak unless noted. They overlap: the PeerAudit, JWT and TLS-chain items each appear in two parts and are listed once here.

| # | Change | Where | Est. saving | Risk |
|---|---|---|---|---|
| 1 | Deliver in-process when the transport's resident is in the same process | A §6.1 | 12–15 cores (7–8% of services busy CPU) | medium |
| 2 | Check strict capacity before building a window-client candidate, and let a drained client hand over its slot | B #1 | 10–19% of proxy CPU (1.2–2.6 cores), plus about 265/s mints, TLS dials and contract pairs taken off PostgreSQL, Redis and the edges | low |
| 3 | `PeerAudit`: answer it before authentication, and stop clients sending it | C #1, B #4 | 1.4–1.7 api cores, 0.3–0.4 proxy cores, 5.2k fewer Redis commands/s | very low to low |
| 4 | Cache verified JWT claims per token | C #2, B #6 | 2.2–2.9 api cores, about 0.2 proxy cores | low–medium (auth path) |
| 5 | Cache verified certificate chains in connect's TLS client | C #3, B #3 | 2.3–2.5 taskworker cores, 0.5–0.7 proxy cores, plus every app and provider | low (crypto code, needs tests) |
| 6 | Re-arm the pgx slow-write and quic-go connection timers without resetting a heaped timer. Firing times stay the same | A §6.2 | 3–6 cores | low (pgx), low–medium (quic-go) |
| 7 | Remove the nginx stream hop in front of PgBouncer | D #2 | 4–5 edge-2 cores | medium (DB entry path) |
| 8 | Index the liveness-lease writer per shard, with a dirty list | A §6.3 | about 3 cores | low |
| 9 | Stop the key-event merge-buffer overflows that force a full resync, and batch the close-request reads | A §6.4 | 2–3 cores, plus Redis load | low–medium |
| 10 | Read contract holes and events without the preflight PING | E §4.1 | about 5% of Redis CPU (about 1.3 cores), and 13–16k fewer round trips/s | low |
| 11 | Switch the hot scripts from EVAL to EVALSHA | E §4.3 | 2–2.5% of Redis CPU, and 40–50% of its inbound bytes | low |
| 12 | Connect PgBouncer to PostgreSQL over a Unix socket | D #3 | 1–2 cores (unmeasured) | low |
| 13 | Drop the `reap_pending` and `stream_id` indexes on transfer_contract | D #7 | 0.2–0.5 cores, and 95 GB less to read per vacuum cycle | low |
| 14 | Keep long snapshots off the primary: a physical backup instead of pg_dump, the census off-peak, the retention cascade in short transactions | D #1 | avoids spikes of 15–55 cores (455 core-hours on 10-29) | low–medium; owner decision |
| 15 | Reboot into the staged kernel options: edge-2 (`retbleed=stuff iommu=pt`), edge-5/6 (`iommu=pt`), plus Redis NUMA pinning | D #4, E §4.4 | 1.5–3 edge-2 cores; IOMMU is 3.3–3.7% of Redis samples | medium (restart windows; Redis has no replicas) |
| 16 | Make provider-intent `count_clients` cheap and still exact | E §4.2 | removes 340–452 ms whole-node stalls (latency more than CPU) | low–medium |
| 17 | Stop the rejection creep at its source: connect's refusal replays grow with connect uptime | A §6.5, C §2.3 | 0 early in a connect generation, rising to 6–8 api cores late in one | api/SDK work |
| 18 | Idle tier for proxy devices, or a 20-minute hold | B #2 | 20–40% of proxy CPU *(inferred)* | medium; product call |
| 19 | Terminate H1 in connect, with no nginx hop | A §6.6 | 8–12 cores | high |
| 20 | Multiplexed links for the remaining cross-process exchange, after #1 | A §6.7 | 5–8 cores | high |

Items 2–13 need no product or posture decision. #7 does touch the database entry path, so it needs a change window.
- **Together:** roughly 23–33 cores, about 9–13% of the fleet's average busy CPU.
- **With #1 added:** roughly 35–48 cores.

These totals overlap and are approximate. Each part has the evidence, the code locations, the correctness argument and a validation plan per item.

### Themes

1. **One change, several tiers.** PeerAudit (#3), the JWT claims cache (#4) and the certificate-chain cache (#5) are each one library or server change, and each pays in api, proxy and taskworker at once.
2. **Hops cost kernel time.** The nginx hops in front of H1 (#19) and PgBouncer (#7) are 8–12 and 4–5 cores, mostly in the kernel.
3. **Long snapshots are now the biggest swing on the primary (D §2.7).** The dump, the census and the retention cascade each hold the vacuum horizon. That inflates buffer reads, bloats hot tables and adds to edge-2's disk growth.
4. **Staged kernel options are still waiting for reboots,** on edge-2 and on edge-5/6.
5. **Library timer resets drive connect's timer-heap scans (A §4).** pgx accounts for 59–64% of the triggers, quic-go 24–28% and go-redis 10–11%.
6. **Proxies build window clients only to throw them away (B §3.5).** Mints and retirements both run at about 265/s, so a client lives 6–15 minutes against its 60-minute lifetime.
7. **The rejection creep is api load set by connect (A §5, C §2.3).** Refusal replays follow connect's uptime sawtooth.

## Decisions for the owner

1. **Backups (D #1).** The Sun/Thu logical pg_dump holds the vacuum horizon for about 20 h.
   - On 10-29 it took edge-2 from 37 to 92 cores and was stopped by the CPU rule with about 5 h left.
   - Its staged generations also sit on edge-2's root filesystem.
   - On 10-31 the owner chose to keep the logical schedule. This is new evidence for that choice: a physical backup holds no horizon.
2. **Proxy idle devices (B #2):** an idle tier, or a shorter hold. The trade-off is resume latency.
3. **Hosted quality window 5 → 3 (B #10):** 10–15% of proxy CPU, against fewer racing exits and less failover depth.
4. **Restart windows** for the staged kernel options on edge-2, and on edge-5/6 with Redis NUMA pinning (D #4, E §4.4). Redis has no replicas, so each node restart has real risk.
5. **Redis experiments (E §4.9):**
   - an io-threads 1 A/B (not advised yet: it halves incident headroom);
   - the SRSO security posture;
   - edge-5's co-location with MinIO.
6. **GOMAXPROCS A/Bs on one block:** proxy 4 → 3 (B #8), api 8 → 4 (C #9).
7. **transfer_contract freezing (D #8):** shorter vacuum cycles and a lower per-table `freeze_min_age`. Raising `autovacuum_freeze_max_age` is not recommended by default.

## Validating a change

Each part gives a validation plan per item. They share four rules.

- **Normalize by the work.**

  | Domain | Normalize per |
  |---|---|
  | Services | 1k clients |
  | Proxy | transport and device |
  | api and Redis | request |
  | PostgreSQL | statement |

- **Compare like with like:** the same hours across days, over at least three days. Where a change can be staged, A/B it on one block (beta or g1) against the other.
- **Hold the timing bounds as acceptance criteria,** not just the CPU. These include forward and liveness detection, revocation, probe timing, and the proxy's session interval maxima.
- **Measure CPU from Mimir and perf,** not from a single short profile. CPUOPT2's GOGC prediction came from one sample that caught a GC cycle.

## Design plans (added 2026-10-31)

The owner asked for a design for each headline item before implementation, with the decisions that need an answer first. Each plan covers:
- the change and its code locations;
- why no timing bound moves;
- compatibility and rollout;
- failing-first tests and production validation;
- savings and effort;
- numbered questions for the owner, each with a recommended answer.

The questions are asked one at a time, and nothing is implemented before they are answered.

| Plan | Item | Est. saving | Effort | Status |
|---|---|---|---|---|
| D1 | In-process delivery when the transport's resident is in the same process (A §6.1) | 12–15 cores | 8–12 engineering days, plus a 6-day staged A/B | approved; implementing |
| D2 | Stop building window clients the strict hard max refuses; a drained client hands over its slot (B #1) | 1.2–2.5 proxy cores, plus 265–383/s fewer mints, retirements and contracts | about 8 engineering days and two proxy releases, then app releases | approved; implementing |
| D3 | PeerAudit: answer cheaply, stop sending (C #1, B #4) | 1.4–1.7 api cores, 0.3–0.4 proxy cores | about 1.5–2 days server and monitor, 1 day connect | approved; implementing |
| D4 | (a) A verified JWT claims cache; (b) a verified certificate-chain cache (C #2, C #3, B #3, B #6) | 2.2–2.9 api cores, 2.3–2.5 taskworker cores, 0.7–0.9 proxy cores | about 3 + 3 engineering days | deferred |
| D5 | Keep long snapshots off the primary (D #1) | avoids 15–55-core spikes, about 700 core-hours per full dump | a standby host (hardware), plus small interim changes | minimal fix approved (Part 2 bounds) |

### Owner decisions (2026-10-31)

| Plan | Decision |
|---|---|
| D1 | Implement. Q1, the kill switch: both a code default and the runtime field in the Redis move-control hash. The other questions use the recommended answers, which the owner can still override. Q5 is moot: the quick wins aren't scheduled, so D1 ships on its own staged A/B. |
| D2 | Implement with the recommended answers (the owner can still override): a counters-only release first, then the capacity check and the slot handover. |
| D3 | Implement with the recommended answers (the owner can still override): answer audit-only packs before authentication, retire PeerAudit, drop ReportAbuse but count it for two weeks, fix the monitor signal now. |
| D4 | Deferred. The owner was concerned about storage. Both caches are in-process memory, not Redis (the JWT cache is capped at 64 MB per process, the chain cache at 256 entries), and the plan stays here for later. |
| D5 | The minimal fix only: `RuntimeMaxSec=30h` on the dump unit, plus an abort watcher that stops the dump and publishes nothing above a transfer_contract freeze age of 1.0B or below 1.5 TB free on edge-2's root. No format change. The standby is not scheduled. Separately, the owner chose to delete edge-2's dump generations once planetoid has verified its copy. |

### Changes made so far (status 2026-11-01 05:00Z)

What has been built from these plans, and where each change stands.
- **Nothing here is deployed yet.** Each change goes through an independent review, then a staged rollout.
- **Server releases are hotfixes for now.** Server changes are kept cherry-pickable onto the running release revision (a8461e236 plus its hotfixes), because origin/main needs migration 903, which production applies separately.

| Plan | Branch and head | State |
|---|---|---|
| D1 | server `feat/cpuopt3-d1-inprocess-delivery` @ 37b2782c4 | Built, ships dark; independent review in progress |
| D2 | connect, sdk and server `feat/cpuopt3-d2-window-handover` | Being built: counters-only step first |
| D3 | server `feat/cpuopt3-d3-peeraudit` @ dafc7d60e; connect `feat/cpuopt3-d3-peeraudit` @ c034364a | Built; independent review in progress |
| D4 | none | Deferred by the owner |
| D5 | xops `feat/edge2-release-verified-dumps` @ 0e44e1d, more commits to come | Bounds and delete-after-verify built; the parallel format and planetoid retention are being added; not applied |

#### D1: in-process delivery

**Commits:**
- 4366f8156: model;
- 87bdcdd88: connect;
- 445e940d6: dashboard and docs;
- 37b2782c4: the public throughput counts in-process bytes.

They cherry-pick cleanly onto a8461e236 as 51ee9f3d2, 1fcd93787, 1a0a2371d and 9310d499c, with no migration.

**What changed:**
- `connect/resident_local_transport.go` (new): when a transport's resident is in its own process, the transport attaches to it in memory instead of dialing it over loopback TCP.
  - Two pumps per link replace about 9 goroutines and two sockets.
  - Each pump keeps the socket writer's contract: FIFO batches, framer limits over the whole batch, zero-length messages as pings, activity per delivered batch, and every undelivered buffer returned exactly once.
- Each batch gets 15 s from its start. The timer exists only while a delivery blocks, and a stalled batch detaches into the normal reconnect.
- `connect/resident.go`:
  - one branch in `ResidentTransport.Run`;
  - `CancelIfIdle` counts an attached link as activity;
  - an `op` label on `exchange_io_*`.
- `model/connect_move_control.go`: the `localTransport` field.

**Kill switch (owner, D1 Q1: both):**
- **The code default** `LocalTransportDefault` is off.
- **The runtime field:** `HSET {connect_move}control localTransport beta|g1|all|off`, which each process re-reads every 10 s.
- **Turning it off:** new attaches go to TCP, and live links detach back to TCP at 500/s per process.

**Metrics:**
- `exchange_local_frames_total`, `exchange_local_bytes_total`, `exchange_local_attachments`, `exchange_local_detaches_total{reason}`, `exchange_local_blocked_seconds`, `exchange_local_transport_enabled`;
- the drop boundaries `local_transport_send` and `local_transport_receive`.

Exchange totals are now io plus local, with no double counting.

**Public dashboard (owner decision):** the live exchange throughput panel and the live throughput header stat count socket plus in-process bytes, so the published number stays continuous through the rollout.

**Evidence:**
- 31 tests, 5× under -race, plus DB end-to-end tests through the gate containers;
- 26 faithful mutants caught, among them a per-delivery timer re-arm, a deadline started before the batch, a leaked or double-returned buffer, unpaced or missing detach, and writers that modify shared buffers;
- **Benchmark:** the hop costs 1.4 µs per frame in process against 4.0 µs over loopback (−63%), with 1 allocation instead of 2.

**Rollout:**
1. Deploy dark.
2. Enable beta for 1 day, then g1 for 3 days against the other blocks, then all.

#### D2: window-client handover

Being built in two separable steps:
1. A counters-only release (`connect.WindowClientStats` through the sdk to a proxy collector) for the 24 h baseline.
2. The capacity check before candidate construction, and the atomic slot handover for drained clients.

#### D3: PeerAudit

**Commits:**
- e1b5b142b: monitor partitions;
- 87bf51944: monitor denominators;
- dafc7d60e: the discard;
- connect c034364a.

The server commits cherry-pick cleanly onto a8461e236.

**What changed:**
- **`controller/connect_control_discard.go` (new):** a pack of at most 2,048 base64 characters, made only of PeerAudit or abuse-report frames, gets an empty reply before authentication.
  - The route gates (lag-guest, intended-network) have already run by then.
  - Mixed or malformed packs take the normal path unchanged.
  - Counter: `urnetwork_connect_control_discarded_requests_total{abuse="claimed|none"}`, with no logging.
  - api and alt share the router path. The proxy's localclient runs its lifecycle checks, then the discard, before contract admission and authentication.
- **Monitor §2.19e (control route pressure) works again.** It had been blind since 10-22, because its parser rejected the new `resident_move_result` label: the old parser marks 20 of 20 api slots unobservable, the fixed one 0 of 20. It also subtracts discarded requests from its denominators.
- **Abuse reports no longer spend a provider's control budget.** A DB regression test goes from 429 to 200.
- **connect:** `SendPeerAudits`, default off, stops all three senders. The off path costs 4.8 ns per call against 84.9 ns.

**Evidence:**
- failing-first runs on the base;
- 15 mutants caught;
- 60 s of fuzzing, 136k executions;
- every new test, DB-backed ones included, passes under -race.

**Rollout:**
1. The monitor commits.
2. The api on one block for 24 h, then the fleet and alt.
3. The proxy.
4. Merge connect and bump the server release lock.
5. The sdk, then apps on their normal schedule.

The server change alone removes about 85% of the api cost.

#### D5: backups (the minimal fix, plus the owner's later choices)

- **c920b07, delete after verify:**
  - Planetoid writes a verified marker to edge-2 only after a full copy matches the dump's sha256.
  - An hourly unit on edge-2 then deletes that generation. It can write only `final/` and `verified/`, with no network and no capabilities.
- **0e44e1d, dump bounds:**
  - `TimeoutStartSec=30h` (`RuntimeMaxSec` has no effect on a oneshot unit).
  - A guard checks before the dump and every 5 min. It refuses to start, or aborts, when transfer_contract's `age(relfrozenxid)` (partition-aware) exceeds 1.0B, when edge-2's root has under 1.5 TB free, or after three failed reads in a row.
  - It publishes nothing, removes the stage, and exits 1.
- **Being added:**
  - `pg_dump -Fd -j4` from the 11-05 dump, about a 4.5 h snapshot hold instead of 18–20 h. The output is tarred and encrypted into one artifact, so planetoid's pull and the release step stay the same.
  - Planetoid retention:
    - remove the stale 294 GB partial now;
    - remove the two legacy xz monthlies only after a newer verified generation exists;
    - keep latest 2 / week 1 / month 1.
- **Tests:** 19 release tests and 12 guard tests, failing first; `systemd-analyze verify` on systemd 255.
- **Apply:** edge-2 is applied only when no dump is running. Planetoid can be applied before the 11-01 generation's pull.

**The 11-01 00:00Z dump**, still single-stream, runs under the owner's 10-29 stop rule: CPU at or above 97% on two consecutive 10-minute readings, or more than 50 PgBouncer clients waiting on two. From 04:07 to 04:48Z it read:

| Measure | Range |
|---|---|
| CPU | 90.7–96.1% |
| PgBouncer waiting | 0–89 |
| Debit-flush lag | 4,610–6,901 s |

#### Related changes from Part D's findings

The owner approved an edge-2 disk reclaim plan:
- REINDEX CONCURRENTLY of the bloated partial indexes and of network_client's;
- partitioning provider_egress_health_history;
- VACUUM FULL of the queue tables in no-dump windows;
- client_reliability back to 21-day retention, with the subsidy-payment pin removed;
- a 91-day contract row reaper.

All of these are being built, with a runbook to follow.

#### Contract horizon reaper: a runtime horizon down to 35 days

The reaper merged on 11-01 (7334b3a17) at the owner's 91 days, and `horizonDays` followed the same day with a 45-day floor (2bff1b9a5). The owner then set the horizon to **35 days, in force by about 11-08, with guards**: the disk model shows 45 days cannot keep edge-2 above 1.5 TB free, and edge-2 has no free bays. Branch `feat/contract-reaper-horizon-35d` lowers the floor and adds the guards. The control stays unset until an independent review; the owner sets `horizonDays 35` about 11-08.

**The field:** `HSET {contract_horizon}control horizonDays <days>`.
- Whole days from 35 to 91. A value below 35 counts as 35, one above 91 as 91.
- Absent, empty or unparsable (`35.5`, `35d`), it is the code's 91 days. That is the other fields' rule: a value that does not parse narrows as far as it can.
- It is read before every batch, with the other fields, and a new value applies from the next batch. Raised, it stops deleting inside the new horizon from that batch on. Nothing already deleted comes back.
- It never turns the reaper on or off; `mode` does. A dry run at 35 days is `mode dry_run` with `horizonDays 35`.

**Reaper-only, except key history while the mode is on.** The floor is `ContractHorizonFloor` in `model/contract_horizon_reaper.go`, and the code setting floors there too. `server.ContractRetention` stays 91 days, so nothing else moves:
- the partition drop, whose `contractWeekBlocker` keeps only payment holds (it needs the reaper's keep rules before any cutover; that is separate work);
- close evidence retention;
- the sn-reconcile holders floor;
- the monitor's partition retention signal.

**Key history follows `horizonDays` while `mode` is `on` (owner, 2026-11-01).** `remove_gone_st_client_key_history` then permanently deletes the signed key history of clients gone longer than the horizon, never under the guard's 30-day floor (migration 904). With `mode dry_run`, `mode off`, no control, or an unreadable one, key history keeps 91 days, so a dry run at any horizon deletes no key history. The deletes have their own brake, `{client_key_history_retention}control` (`mode` on/off, `maxClientsPerSecond`, default 150 a second), read before every batch. Its `horizonDays`, when set, takes the place of the reaper's horizon for key history, whatever the reaper's mode, down to the guard's floor; it is read once a run.

**Readers of the deleted rows, and how far back each reads:**

| Reader | What it reads | How far back | At 35 days |
|---|---|---|---|
| Operator block points (`ComputeOperatorBlockPoints`: the daily task, `bringyourctl account-points backfill-blocks`) | unpaid sweeps with a sweep time in the block's window, joined to credit-bearing contracts closed after the cutoff | until the block is awarded, and at most until the window start is `ClientReliabilityRetention` (21 days) old, when the award fails closed. By sweep time, which is the settlement: a contract closed long before can settle in a recent block | kept by the new keep rule |
| Transfer audit rollup (`RollupTransferAuditEvents`: the periodic task, `stats rollup-transfer`, `stats backfill`, `stats migrate-202608`) | transfer_contract joined to contract_close by close time, one UTC day at a time, replacing the day's row | task: 3 days; CLI defaults: 7 days (`rollup-transfer`), 21 days (`backfill`, `migrate-202608`); an explicit `--start` reaches any day | days the reaper can reach keep their rows (the new guard) |
| Payout planner | unpaid sweeps of contracts minted before the cutoff, sweeps of safely canceled payments, staged sweeps | up to 300 days | kept by `unpaid_legacy`, `payment_incomplete`, `plan_staged` |
| sn-reconcile (A2 holders, census) | sweeps of pending and safely canceled payments; legacy payments | per payment | kept as payment holds or incomplete payments. The reaper deletes a contract with its sweeps, so A2's rule that a sweep above its floor has its contract still holds |
| Subnet epoch reads (`GetStEpochProviderUsage`, the v2 ledger, close originals) | live contracts and the usage archive, by close time; close evidence | each epoch's window | unchanged in result: every deleted credit-bearing contract is archived in the same statement, and evidence keeps its 91 days. The read's duplicate probe once hashed the whole archive, which the reaper grows; see below |
| Provider stats (`/stats/provider-last-n`, `/stats/providers-overview-last-n`, the legacy `-last-90` routes) and provider payout stats | contracts joined to closes; sweeps by sweep time | up to 366 days | undercount the days older than 35 |
| `/transfer/stats` | unpaid sweeps | all time | `unpaid_bytes_provided` falls as post-cutoff sweeps pass 35 days |

The disk analysis put block points at 37 days: a 7-day block plus reliability retention stretched to 30 days. That stretch was the subsidy-epoch frontier, which the owner retired on 10-31, so an award now fails closed once its window is 21 days old. Its real exposure is late settlement: a dispute resolved, or a legacy settlement committed, weeks after the close puts an unpaid sweep in a recent block, and no horizon covers that. Before this change, a 91-day reaper could delete such a contract too.

**The block points keep rule (`block_points`).** A contract stays while any unpaid sweep of it has a sweep time inside the window of a block an award can still read (`OperatorBlockPointsPendingWindows`):
- The candidate blocks run from the cutoff's block through the one after the open block (a sweep can commit there while a run holds the list). Each has traffic past the cutoff and a reliability window inside retention, by the predicate the award checks (`operatorBlockReliabilityRetained`).
- A block with an award (its `payment_report` row) drops out.
- The windows are the award's own, clamped to the cutoff.
- The list is read once a run. An award committed since only keeps more.
- A block's retention is judged at the start of the oldest award that can still be reading, `OperatorBlockPointsMaxTime` (12 hours) and an hour before the run. The award checks retention again on the clock after its reads, so one that outlives the block's retention fails closed.
- A contract leaves once its block is awarded, or once the block's reliability is past retention for every award that can still be running. That is at most 21 days and 13 hours after the block opened.
- It is a new reason on `urnetwork_contract_horizon_protected_contracts_total`, one more series, last in the classification order but for `payment_review`.

**The rollup guard (`TransferAuditRollupFirstDay`).** A rollup never writes a day before the first whole day 33 days back: the 35-day floor less a 2-day margin, since one day's read can take most of a day at Main's churn while the reaper's horizon advances. The row of an earlier day was written while all of its contracts were present, and a re-roll would replace it with a sum of what the reaper left.
- The CLI path skips those days, and logs that it kept their rows.
- A periodic chain clamped this way counts in `urnetwork_transfer_audit_rollup_clamped_total` and logs at V(1); any is a chain that fell weeks behind.
- A periodic chain that fell behind resumes at that day.
- Neither default window reaches it.
- No reaper keep rule can serve this reader, since an explicit `--start` can name any day.
- The first roll of every day happens within about three days, long before 35.

**Every other protection holds at any horizon.** Each check that keeps a contract reads its rows, not its age:
- open or disputed contracts, and unsettled escrows;
- payment holds, and incomplete, missing or recent payments;
- unpaid sweeps of contracts minted before the earning cutoff (the stricter sweep rule), and sweeps staged in a pending plan;
- rows another transaction holds;
- sweeps paid by a payment that completed under attribution review (`payment_review`). `completePaymentInTx` queues no retention stamping for such a payment, so the payment completion path keeps its contracts until the straggler horizon, and so does the reaper. The rule is the reaper's own: `ContractPaymentRetentionHoldSql`, which the straggler pass shares, is unchanged.

The DB tests check each one at 35 days, with the cutoff inside 91 days as production's is. The settings comment once cited a planner retention-risk age tied to 91 days. No such age exists: the planner's is `StragglerContractExpiration` minus 7 days (293 days), and it guards the straggler path.

**What users see at 35 days:**
- **Provider stats:** the 90-day views lose the days older than 35, days 36 to 90, by the contracts deleted there. Those are traffic after the earning cutoff and contracts without a sweep, with their contract and client counts. Windows past 91 days already undercount.
- **`/transfer/stats`:** `unpaid_bytes_provided` falls as unpaid sweeps of contracts minted after the cutoff pass 35 days. With the cutoff about 10-06, that starts about 11-10. The subnet pool pays those, never USDC. Paid bytes are unchanged.
- **Stats re-rolls:** `stats backfill` and `stats rollup-transfer` no longer rewrite days more than 33 days old; those days keep the rows they have.
- **Presentation is an owner call:** clamp the stats windows to the horizon, serve older days from the daily audit rollup, or stop counting subnet-pool sweeps as unpaid.

**Close evidence** keeps its 91 days. Below 91, a deleted contract's evidence outlives it until then; `RemoveExpiredCloseEvidence` takes evidence whose contract is gone.

**Reporting:**
- `urnetwork_contract_horizon_horizon_seconds` (no labels) is the horizon the last control read set.
- `urnetwork_contract_horizon_horizon_timestamp_seconds` and the lag gauge follow it.
- The run log line carries `horizon_days=`.
- `block_points` and `payment_review` count the contracts those rules keep.
- **Monitor:** `contract-horizon-reaper` (SIGNALS.md §2.4r) warns when the chain stops running, when runs fail or cannot read the control without progress, when the pass falls behind while on, and when block points keep contracts. `usage-archive-hash-budget` (§2.4s) warns while twice the largest archive week passes half of what the usage read's hash holds in one batch.

**The epoch usage read's archive join (fixed 11-01).** The live arm of `stEpochProviderUsageSql` flags a contract that is also archived by joining `st_provider_usage_archive` on `contract_id`. Unbounded, that join hashed the whole archive in every read: the v1 close, the v2 epoch step and the recovery census.
- On Main, for epoch 3's window, the hash built on all 27 partitions, 20.35M rows, against 3.66 GB of hash memory (work_mem 234 MB times hash_mem_multiplier 16), which holds about 60M.
- The reaper archives every credit-bearing contract it deletes, about 75M a day by the review's estimate, and the archive went from about 20M to 23M rows in an hour on 11-01. Past about 60M rows the hash spills in batches toward the 150 GB `temp_file_limit`, and an epoch step that fails misses its root, as epoch 3's did.
- The join now takes only the archive rows that closed in the window, so each partition reads its close-time index range. It loses no duplicate of a live row: an archived row is an exact copy of its contract's close time, a terminal close time never changes, and a NULL close time on either side stops the read.
- The owner paused the reaper (mode off, 11-01 22:38Z) until a taskworker with the fix runs.

### D1. Deliver in-process when the transport's resident is in the same connect process

Code is cited at server main `3ae2c76f7` and connect `89b15920`, the version main pins. The running connect service is 1085953670: server `37ec4788b` plus connect `ea79e1f1`. Its `ResidentTransport` and exchange paths have the same structure.

**Problem and evidence**

Part A §2.5 and §6.1:
- **Same-process transports:** on edge-3, 50,334 of 116,592 outbound exchange connections dial a listener owned by the dialing process. That was matched exactly through the DNAT map. The host has 55.4k transport outbound ends, so about 91% of transports are a TCP loop back into their own process. If the other hosts match, that is about 200k loops fleet-wide.
- **Forwards are almost never local:** 11.2k connections go to another process on the same host and 55.1k cross hosts.
- **Cost:**
  - The exchange socket trees are 27% of connect CPU: `ExchangeConnection.Run` 13.2% and `handleExchangeConnection` 13.75%, with their syscalls and inline kernel work.
  - Local delivery (`process_backlog`) is 3.0% of edge-3 host samples for connect alone.
  - A relayed packet crosses two transport hops and one forward hop, so transports carry two thirds or more of exchange frames *(inferred)*.
- **Per local transport today:**
  - 2 sockets, 1 conntrack entry, about 9 goroutines;
  - 6 heaped timers: a read deadline, a write deadline and a pinger at each end;
  - 2 extra 96 KiB channel rings and 2 × 64 KiB read buffers.
  - Each frame costs a writev and a read syscall, two deadline installs, two framing copies, a pool get, a netpoll wake and three channel hand-offs.
- **The cause:** `ResidentTransport.Run` always dials (connect/resident.go:3692), even when the resident record names this process's resident. `isLocalResident` (connect/resident_move.go:335) is used only for move takeover.

**Proposed design**

*Where it plugs in.* `ResidentTransport.Run`'s loop (resident.go:3672–3711) resolves the record, runs `takeOverMovedResident`, then dials. D1 adds one branch before the dial:

```go
if link := self.exchange.attachLocalTransport(self.ctx, headerCopy); link != nil {
	self.runLocal(link) // returns after detach; the loop continues exactly as after handle()
} else {
	// unchanged: NewExchangeConnection + handle
}
```

Everything after it stays: the reconnect pacing (`ExchangeReconnectAfterErrorTimeout`), the nomination that replaces the old resident id (:3745), and the client-limit kick.

*Attach.* `(*Exchange).attachLocalTransport(ctx, header)` goes in a new connect/resident_local_transport.go. It returns nil, and the caller dials as today, unless all of these hold:
1. The code ceiling `ExchangeSettings.EnableLocalTransport` is on, and the runtime control allows this block (see the kill switch).
2. `matchResidentGeneration(clientId, header.ResidentId)` (resident.go:1444, under `exchange.stateLock`) returns `Current`.
   - `Missing` and `Changed` go to TCP. The inbound side then waits (up to 30 s, polling 15 s) or refuses, exactly as today.
3. `!resident.IsDone()`. Drain refuses only missing residents (:1531), so a present local resident is accepted, as inbound does.

Then attach does what the accept path does after its header, in the same order:
- `registerExchangeConnection(clientId)` (:1954).
- `resident.addTransportWithEdge(header.transferCarrierProperties(), header.transportEdge())` (:5119). This publishes the same send and receive routes, with the same carrier properties, receive reliability, transport edge for MOVE, and `move.notifyTransportClosed` on close.
- `resident.localAttachments.Add(1)`.
- A link context: `context.WithCancel(transport ctx)`, plus `context.AfterFunc(resident.ctx, link.cancel)`. This replaces the accept path's goroutine that waits on resident or handle done and cancels. It propagates cancellation only; it is not an I/O watchdog.

*Two pumps,* each a goroutine owned by the link. They replace the 9 goroutines and the socket.

| TCP loop today | Client pump: `RT.send` → `routeReceive` | Resident pump: `routeSend` → `RT.receive` |
|---|---|---|
| Writer gathers up to `ExchangeWriteBatchCount`/`ByteCount` without blocking | same gather from `RT.send` | same gather from `routeSend` (inbound writer, :1628–1647) |
| `writeBatchByteCount`: MaxMessageLen and uint16 checks fail the write and close the connection | same checks before delivery; on failure, return the batch to the pool and detach with reason `max_message_len` (V(1) log plus counter) | same |
| Writer installs now+`WriteTimeout` (15 s) per writev (`exchangeDeadline.refresh`, :2666) | batch deadline now+15 s at batch start. Try a non-blocking send first; if it would block, select on `{send, timer, ctx}` with a timer created for the remaining time. Expiry detaches with reason `write_timeout` | same, into `RT.receive` |
| A zero-length frame is a ping: the reader drops it and calls `resident.UpdateActivity` | zero-length message: return it and update activity | same |
| Reader and writer call `resident.UpdateActivity` per message or batch | per delivered batch | per delivered batch |
| Read deadline 30 s, pings every 10 s, keepalive 11 s/1 s×4 | not needed: the peer is this process, and its death is a context edge (below) | same |

- Ordering is FIFO: one producer per pump, FIFO channels, and in-order batch delivery.
- The timer is created only when a delivery would block, as `sendPooledMessage` already does (:3322). It is never a `Reset` of a heaped timer, so D1 adds no heap-scan trigger (Part A §4).

*Detach* mirrors `closeExchangeTransportQueues` (:3443), in this order:
1. `closeTransport()`: route removal joins every resident writer still admitted to the old snapshot (connect transfer_route_manager.go:1180–1212).
2. Cancel the link.
3. Join both pumps.
4. `returnReadyPooledMessages(routeSend)` and `returnReadyPooledMessages(routeReceive)`.
5. `localAttachments.Add(-1)`, `unregisterExchangeConnection`, and the gauge.

`RT.send` and `RT.receive` are not drained. They outlive the link, as they outlive an `ExchangeConnection` today, and `Run`'s defer still closes `RT.receive` after the join.

*Resident activity.* An idle TCP loop refreshes activity at least every 10 s with pings, so an attached transport never lets the resident reach `ExchangeResidentTtl` (300 s). Instead of keeping a ping timer per link, `CancelIfIdle` (:5271) refreshes `lastActivityNanos` and returns false while `localAttachments > 0`. That says exactly "attached ⇒ active", without a timer. TCP-attached transports keep today's ping-driven rule unchanged.

*Accounting and metrics:*
- **Unchanged:**
  - transfer and contract accounting: the resident's `connect.Client` sees the same frames on the same routes with the same properties;
  - the client-facing byte meters (`announce.SendMessage`/`ReceiveMessage`), H1/H3 batching and the speed-test counts;
  - every resident payload ledger (the transport exchange path updates none).
- **New metrics,** so that exchange totals stay exact as io + local:
  - `urnetwork_connect_exchange_local_frames_total` and `…_bytes_total`, with the same `{direction,kind}` labels as `exchange_io_*`;
  - `exchange_local_attachments` (gauge);
  - `exchange_local_detaches_total{reason}`: resident_done, transport_done, write_timeout, max_message_len, disabled;
  - `exchange_local_blocked_seconds`, observed only when a delivery blocks.
- **New `receive_queue_dropped` boundaries** (resident.go:104–111): `local_transport_send` and `local_transport_receive`.
- **Dark release only:** add an `op` label to `exchange_io_*`, to measure the transport share of frames before enabling.

*Kill switch.* Add `LocalTransport` to the `{connect_move}control` hash (model/connect_move_control.go:126). It narrows the code ceiling and never widens it, like `TransportDelay`. Values are `off`, `all`, or a block list, so one block can be A/B'd without a restart. Each exchange reads it every refresh interval (the pattern of connect/transport_delay_control.go). When it narrows, two things happen:
- new attachments take TCP;
- live links detach with reason `disabled`, paced at `LocalTransportDisableDetachRate` (a setting; for example 500/s per process). Each one then redials over TCP through the normal loop.

*What stays the same:* forwards, the inbound accept path, MOVE takeover, drain, nominations, every H1/H3 deadline and ping, and the exchange header and wire protocol.

*Alternatives considered:*
- **Register `RT.send` and `RT.receive` directly as the resident's routes,** with no pumps: one fewer hand-off per frame. It loses the framer checks and the independent 15 s stall owner for the resident→client direction, so not now. Revisit only with a proof that no check is lost.
- **AF_UNIX or socketpair:** keeps the syscalls, timers and goroutines.
- **Merging `ResidentTransport` into the `Resident`:** a larger refactor with the same savings.
- **Multiplexing first (§6.7):** higher risk, and it still loops through the kernel for these connections.

**Correctness under the owner rule**

| Bound on today's local TCP loop | Local path | Why it holds, and how it is measured |
|---|---|---|
| Exchange write deadline: 15 s per batch write, a stall tears down → reconnect → nominate replacement | batch deadline 15 s per delivered batch in each pump, with the same teardown and the same loop | Same budget per batch (a healthy batch gets the full 15 s) and the same stalled-batch bound. `detaches{write_timeout}`, `local_blocked_seconds` |
| Per-message 15 s admission into `ExchangeConnection.send` (drop and continue) | that queue no longer exists; the pump blocks only on the route queue, bounded by the same 15 s | No producer blocks longer than today. A stall still ends at 15 s without progress |
| Read deadline 30 s, pings ≤10 s, TCP keepalive | not applicable: the peer is in this process. Resident or transport end is a context edge, so the link detaches at once | Detection drops from ≤30 s to immediate, never later. `detaches{resident_done,transport_done}` and detach latency |
| Resident idle TTL 300 s | unchanged value; "attached ⇒ active" replaces the 10 s pings | Exact equivalence for healthy links; a wedged link detaches in 15 s. Covered by a test |
| Handshake header deadlines (30 s), resident wait (30 s, polling 15 s) | no handshake: the generation is checked under `exchange.stateLock`; non-current generations use TCP | Unchanged where it can wait; immediate where it cannot |
| MaxMessageLen and uint16 frame limit | same check, same teardown | `detaches{max_message_len}` |
| Reconnect pacing 1 s, the client limit, H1/H3 deadlines and pings, forward 3 min idle, forward exchange bounds, revocation | untouched | — |

- **Ordering:** FIFO per direction within a link, as on a TCP connection.
- **Across reattachment:** frames in `RT.send` go to the next link in order. Frames in the route queues are returned, as today when a connection closes. With two transports per client the resident chooses routes, as today, and the transfer layer orders and retransmits end-to-end.
- **Backpressure:**
  - The H1 reader still blocks on `RT.send` for reliable lanes; H3 datagram lanes still refuse at once (`sendReceivedMessage`, :3791).
  - A slow resident fills its route queue, then the pump blocks, then the 15 s bound applies.
  - A slow client fills `RT.receive`, then the resident pump blocks (15 s bound); the H1 15 s write deadline still owns the client socket.
  - Buffering before a stall shrinks: one 4096-slot ring and the socket buffers are gone. The per-batch budget is unchanged.
- **Buffers:** ownership moves on a successful channel send, unchanged.
  - The client pump owns each batch from dequeue until delivery. On drop, detach or cancel, `sendPooledMessage`-style code returns every undelivered buffer exactly once.
  - Read-only shares from the resident's writers now reach the H1/H3 writers instead of fresh socket copies. They were checked not to mutate inputs: `FramedMessageConn.WriteMessages` copies into its own storage; the H3 fragmenter copies into scratch; gorilla copies. A test pins this.
  - The local path removes 2 copies and 1 pool get/return per frame.
- **Races:**
  - **Replacement between the generation check and `addTransportWithEdge`:** the old resident's context is done, so `AfterFunc` detaches at once. The loop resolves again and attaches to the new generation. This is the accept path's existing race and handling.
  - **Detach while a resident writer is blocked on a full `routeSend`:** `closeTransport` waits for that writer's own 15 s write timeout, exactly as `closeExchangeTransportQueues` does today.
  - **Kill switch during attach:** the attach either completes and is detached by the pacer, or never starts.
  - **Exchange shutdown:** the link context derives from the transport, and the transport from the handler, so `WaitForIdle` semantics are unchanged.
- **MOVE:**
  - A takeover into this process makes the record local, and the next iteration attaches locally.
  - A move away closes the local resident (registration refresh or replacement), so the link detaches and the loop dials the remote resident over TCP.

**Compatibility and rollout**

- **What changes:**
  - **Server connect only:** resident.go, the new resident_local_transport.go, model/connect_move_control.go, metrics, the connect.json dashboard, and SIGNALS §8.15 (exchange owners = TCP + local).
  - **alt** links the same code with the ceiling off.
  - No sdk, app, proxy or wire change.
- **Mixed versions:** none matters. The decision is local to one process. Old and new processes reach each other's residents over the unchanged TCP accept path.
- **Deploy order:**
  1. A dark release from main with the ceiling off and the `op` label added. Measure the transport frame share and keep a baseline for 2 days.
  2. Set the control to `beta` for 1 day.
  3. Set it to `g1` (g1 on all four hosts) for 3 matched days against g2–g4.
  4. Set it to `all`.
- **Rollback:** set the control to `off`; links detach, paced, to TCP within minutes. Or deploy the prior release.
- **The release:** the next connect release from main carries about 400 files of unrelated changes since `37ec4788b`. Shipping dark keeps the D1 A/B clean.

**Tests**

All failing-first, in connect/resident_local_transport_test.go. Clocks are synctest virtual time, and orderings use explicit barriers.

| Test | What it asserts | Mutation it must catch |
|---|---|---|
| `TestLocalTransportDialsNoExchangeSocket` | a local resident's transport opens no socket (dial hook), and frames flow both ways in order | removing the branch |
| `TestLocalTransportBatchDeadlineStallsDetach` | a batch blocked 15 s detaches at exactly 15 s; one that completes at 14.999 s does not | per-message or per-send re-arm (loosens); reusing an earlier deadline (shortens) |
| `TestLocalTransportResidentPumpDeadline` | the same, for `RT.receive` with the H1 writer stalled | as above |
| `TestLocalTransportMaxMessageLenTearsDown` | an oversize message detaches and is returned; the next link carries the rest | dropping the check |
| `TestLocalTransportZeroLengthIsPing` | dropped and counted as activity in both directions | forwarding it |
| `TestLocalAttachmentKeepsResidentActive` | idle and attached past 300 s: resident alive; detached: retired at TTL | removing the attached rule |
| `TestLocalTransportPoolOwnershipUnderTeardown` | detach at each barrier (mid-gather, blocked send, resident close) leaves pool outstanding at zero and violations at zero | a double return or a leak on any path |
| `TestLocalTransportReadOnlyShareUnmodified` | shares written by the resident are byte-identical after the H1, H1+ and H3 writers | an in-place writer |
| `TestLocalTransportGenerationReplacementRace` | replacement inside the barrier between check and add: prompt detach, attach to the new generation, nothing delivered to a done resident | a missing `AfterFunc` edge |
| `TestLocalTransportKillSwitchPacedDetach` | narrowing switches new links to TCP and detaches live ones at the set rate; frames already in `RT.send` survive | unpaced or no detach |
| `TestLocalTransportMoveAwayRedialsTcp` and `TestLocalTransportDrainConvergence` | composed through the real handler and exchange | — |
| `TestLocalAndTcpPathsDeliverIdenticalSequences` | one deterministic traffic script through both paths gives equal resident-observed frame order, bytes, contract counters and drop counters | any semantic drift |

- **Benchmark:** `BenchmarkTransportFrameLocalVsTcpLoop` on real loopback, measuring CPU µs per frame and allocations per frame.
- **Production acceptance** (g1 against g2–g4, matched hours over 3 days):
  - `exchange_active_connections{op="transport"}` on g1 −85–95%;
  - connect cores per 1k clients on g1 at least 10% lower;
  - no rise in `h1_sessions_closed_total{reason}`, nominations per client, or `receive_queue_dropped`;
  - `detaches{write_timeout}` per 1k transports no higher than control-block transport redials;
  - a rerun of `bpf_adjust.sh` shows smaller heaps.

**Savings and effort**

- **Base:** 10-31 18:27Z: connect 103.5 cores, 0.95M frames/s crossing exchange sockets, exchange trees at 27% of connect, about 60% of frames local.
- **Estimate:** about 27 µs saved per local frame, less about 1.5 µs for the two hand-offs. That is **12–15 cores**, 7–8% of services busy CPU.
- **Also:**
  - about −1.4M goroutines (−10%);
  - about −75k timers per g-process (−15% heap, so −15% of `adjust`);
  - about 4 GB of heap per g-process;
  - −400k sockets and −200k conntrack entries fleet-wide.
- **Effort:** 4–6 engineering days of code and 4–6 of tests, then a 6-day staged A/B.
- **Releases:** one connect release, dark, then control flips.

**Risks**

- **Edge-case drift from the TCP loop:** oversize, zero-length and teardown ordering. Mitigated by the parity table and the identical-sequence test.
- **Lifecycle races:** generation, MOVE, drain, kill switch. Mitigated by barrier tests through the real accept and handler code.
- **Pool ownership:** read-only shares reach new consumers.
- **Less buffering before a stall:** the same per-batch bound fires on less queued data.
- **The 60% frame share is inferred:** the dark release's `op` label measures it before enabling.

**Follow-up: §6.7 multiplexed links**

D1 separates a link's semantics from its carrier: the pumps, batching, checks, batch deadline, activity, drop boundaries and path-labeled metrics. A multiplexed link per process pair would keep the pumps per stream, and replace "no carrier" with one shared socket. That means:
- stream ids in the frames;
- per-stream credit standing in for the route queues, so the per-batch deadline becomes a credit-stall bound per stream;
- one pinger, read deadline, write deadline and keepalive per link instead of 6 timers per connection.

The local path is that design's "link to self". D1's io+local accounting also measures what multiplexing has left: forwards and cross-process transports, about 40% of frames.

**Questions for the owner**

1. **Kill-switch form.**
   - (a) A code ceiling only (a change needs a block restart and a reconnect wave).
   - (b) A runtime `{connect_move}control` field `localTransport` (off, all, or a block list).
   - (c) Both.

   *Recommend (c).* It changes whether the dark release can be A/B'd per block without restarts.
2. **What "off" does to live links.**
   - (a) New attachments only.
   - (b) Also detach live links to TCP, paced, for example at 500/s per process.

   *Recommend (b),* because it makes rollback real. It changes the pacer setting and the pacer test.
3. **Rollout appetite.**
   - (a) Dark, then beta 1 day, then g1 for 3 days, then all.
   - (b) Dark, then g1 for 3 days, then all.
   - (c) All blocks at once behind the switch.

   *Recommend (a).* It sets the release and soak dates.
4. **Scope.**
   - (a) Transports only.
   - (b) Transports and local forwards in the same release.

   *Recommend (a):* forwards are about 0% same-process today. (b) adds forward-side lifecycle tests for little gain.
5. **Order against the quick wins** (the pgx and quic-go timer fix, the liveness writer, the key-event resync).
   - (a) D1 after they ship.
   - (b) In parallel.

   *Recommend (a):* each A/B then measures one change. It decides which connect release carries D1.

### D2. Stop building window clients the strict hard max will refuse, and let a drained client hand over its slot

Code references are connect 89b15920, as deployed in proxy 1086377170. These files are unchanged at connect main c62b060e. The sdk is at fe265dfc and the server at a8461e236.

**Problem and evidence**

From CPUOPT3 Part B §3.5 and §4 #1:
- **Churn:** hosted window clients are minted at 269/s fleet-wide and retired at 265/s. That is 13.5 per process per second, against about 5.1k carriers per process. By 19:35Z on 10-31, mints were 383/s, about 44 per device-hour.
- **Cost:** building and retiring window clients is about 19% of proxy CPU:

  | Part | Share of proxy CPU |
  |---|---|
  | TLS dial (x509 verify 5.3% of it) | 6.3% |
  | contracts and Provide | about 4.5% |
  | FindProviders2 | 2.9% |
  | construction and teardown | 2.4% |
  | mint | 1.4% |

  Every candidate also costs pg a network_client allocation and retirement, Redis an escrow reservation, and a services edge a TLS handshake and a resident.

**The mechanism.** Every low-memory profile runs strict windows: hosted proxy devices, iOS and Android (sdk/mobile_memory_policy.go:535-536). Hosted windows are quality min = max = hard max = 5, speed fixed at 1, with no standing reserve. The loop goes like this:
1. A client passes its jittered lifetime (60 min × 0.75–1.0, ip_remote_multi_client.go:11969). The lifetime clock starts at the first received packet, and a probe answer counts (20553-20617, 20796).
2. Resize only warns it, "client drain" (13565-13606). The warned client leaves `clients`, so `len(clients) < targetWindowSize`.
3. That resets the discovery backoff (ip_remote_multi_client_discovery_retry.go:87-99), and expansion runs (14052).
4. The enumerator runs FindProviders2 and mints an identity (13115-13144, 13255-13271). `expand` then builds the client: Provide, platform transport with TLS, key registration, a contract and the evaluation ping (14600-14700).
5. Only then does `strictWindowAdmissionAllowed` refuse the candidate, because the drained client still holds its slot (14326-14341, 15174-15187). The candidate is cancelled and retired.
6. Collapse runs only above the hard max (14093), so the drained client is never removed. The pass is "unproductive", the backoff runs 10 s → 2 min (discovery_retry.go:42-77), and the cycle repeats as long as the window lives.

**The code contradicts its own contract.** `StrictWindowSizeHardMax` says a new identity is declined "when a flow-carrying warned client cannot yet be collapsed" (593-602). The code also declines when the warned client is flowless and collapsible.

**Beyond CPU, rotation never completes in strict windows.** A drained exit stays warned, and warned exits are excluded from new flows (orderedClients, 15581-15633). A discovery window whose exits have all drained offers no exit to new flows. On a hosted device that clears `MinSatisfied` (15516-15538), which can make `ProxyDevice.Active()` false and trigger a device recreation. This is *inferred* from code: no metric shows it today, and hosted logging is off.

**Proposed design**

There are three parts, plus the under-filled windows.

**0. Counters first** (connect, sdk, server/proxy)
- **Type:** a new `connect.WindowClientStats`, atomic counters over fixed enums. The precedent is `connect.H1PlusStats` and `server.NewH1PlusCollector` (server/proxy/server.go:38, 171).
- **Wiring:**
  - `MultiClientSettings.WindowClientStats *WindowClientStats`; nil means off, so apps pay nothing.
  - `sdk.DeviceLocalSettings.WindowClientStats`, copied into the multi-client settings at sdk/device_local.go:5136-5141.
  - The proxy sets one process-wide instance in `newProxyDeviceLocalSettings` (server/proxy/proxy_device.go:1380) and registers a collector.
- **Series** (labels are fixed vocabularies, never identities):
  - `urnetwork_proxy_window_candidates_total{window, outcome}`. Outcomes: `admitted`, `admitted_handover`, `declined_strict_cap`, `declined_replacement`, `not_admitted_surplus`, `failed_setup`, `failed_ping`, `failed_contract`, `canceled`.
  - `urnetwork_proxy_window_expand_passes_total{window, state}`. States:
    - `below_min`;
    - `below_target`;
    - `strict_hole`: fewer kept clients than target, with installed clients at the hard max;
    - `skipped_strict_full`: new;
    - `deferred`.
  - `urnetwork_proxy_window_warnings_total{window, cause}`, counting transitions into the warned state: drain, silent, unhealthy, starved, capacity, quarantine, family_swap.
  - `urnetwork_proxy_window_removals_total{window, cause}`: verdict, unhealthy, transport_down, collapse, handover, family_swap, closed.
  - A scrape-time gauge `urnetwork_proxy_windows{state}`, built from the existing per-DeviceLocal iteration (memory_metrics.go:396). States: `selectable_full`, `strict_hole`, `below_min`, `no_selectable`.
- **Baseline:** the release carrying the counters runs 24 h with the behavior flags off. The split it measures becomes the baseline and the acceptance criteria:
  - `declined_strict_cap` against the `failed_*` outcomes;
  - `strict_hole` windows against `below_min` windows;
  - mints per device-hour.

**1. Capacity check before construction**
- **One predicate,** `strictAdmissionCapacityWithLock(windowSize) int`. It returns `hardMax − len(self.clients) − reserved`, or unbounded when `!StrictWindowSizeHardMax || hardMax <= 0`. These are the same inputs as `strictWindowAdmissionAllowed`, under the same window `stateLock`. `reserved` is the at most one handover reservation of part 2.
- **Where it runs:**
  1. **Resize,** before the deferral and `expand` (14043-14052): `neededCount = min(target − len(clients), capacity + handoverEligible)`. At zero, the pass skips expansion and counts `skipped_strict_full`. It must not call `noteDiscoveryExpandUnproductive`: nothing was attempted, so the backoff state is untouched.
  2. **The enumerator,** before `enumerateDestinations` and before each mint (13115-13144, 13255): it lists and mints nothing while capacity is zero and no handover is reserved. This is the clause the demand path already has (ip_remote_multi_client_discovery_demand.go:190); the main enumerator lacks it.
  3. **Admission** keeps `strictWindowAdmissionAllowed` exactly as it is, as the authoritative gate. A same-identity replacement (fixed destinations) bypasses the pre-check, just as `replacing` bypasses the gate.
- **Why this is exact:** the pre-check suppresses only candidates the gate would refuse at the moment of planning. A slot can free between planning and admission. Every path that frees one already wakes resize: `removeClient` notifies (13393-13400), and so does the `client.Done` watcher (14400-14407). The next pass then starts at once and builds the candidate. So no admission is lost, only re-planned within microseconds. Non-strict windows (desktop) take the unbounded branch and are byte-for-byte the old code.

**2. Slot handover ("collapse at admission")**
- **Who is eligible:** in resize, after classification, a warned client is eligible when the existing collapse test passes for it. That test (13802-13817) is `StatsWindowGraceperiod ≤ duration && weight ≤ 0 && collapseRemovalAllowed(client, duration)`: flowless, or past the collapse deadline of 2 lifetimes (11950, 15211-15221). Candidates are ordered as collapse orders them (family, weight, duration).
- **Reservation:** when capacity is zero, the pass reserves one victim per window, `strictHandover{victim, owner}` under `stateLock`. The pass's `expand` defer releases it.
- **The swap:** in `admitCandidate`, if the gate refuses and this pass holds the reservation, one `stateLock` section does the following:
  1. checks the victim is still installed and still warned;
  2. deletes it;
  3. installs the candidate and notifies.

  The installed count stays at the hard max throughout. After unlocking, the victim takes the ordinary removal path: Close, the Removed monitor event, and retirement by the generator. If the victim was removed meanwhile, the slot is free and the plain gate admits.
- **A drained client with flows** is not eligible before the collapse deadline, so no candidate is built for it. It keeps serving. Its movable flows were already re-pinned when the drain started (`markDrainMigrateOnce`, 13598-13606). Once it is flowless and its 30 s weight window is empty, it becomes eligible and hands over.
- **What stays the same:**
  - warn causes and their thresholds;
  - selection's exclusion of warned clients;
  - the hard max on installed clients;
  - evaluation before install;
  - family-swap and fixed-destination paths;
  - every non-strict window.
- **Transient graphs:** while a window sits at its ceiling, at most one candidate is in evaluation. Today a pass builds up to `multiClientExpandStepMax` = 4 candidates and discards them all.

**3. Under-filled windows**
- **The pattern:** windows below target whose candidates fail evaluation also build, then discard.
- **What constrains a fix:**
  - an identity a provider has seen never follows the window to another provider (ip_remote_multi_client_identity_reuse.go, header);
  - identities no provider saw are already parked and reused;
  - the 10 s → 2 min backoff already paces passes.
- **What D2 does:** it does not change their pacing. It counts them (`failed_*` by pass state `below_min` and `below_target`), so the baseline sizes them. A follow-up design comes only if they dominate (owner question 4).

**Alternatives considered**
- **Admit one over the ceiling, as non-strict windows do:** breaks the mobile memory envelope and the "never exceed the hard max" requirement.
- **Remove the drained client first, then build** (the strict family-swap path, 13966-13975): the window sits below its minimum for the whole evaluation, 1–30 s.
- **Turn strict mode off for hosted devices:** rotation works again through collapse, but with a transient overshoot, and phones are not fixed.
- **Pre-check only:** stops the waste but leaves rotation broken and the selectable exits decaying.

**Correctness under the owner rule**

Every bound on the path:

| Bound | Value and code | After D2 |
|---|---|---|
| Client lifetime | 60 min × [0.75, 1.0), 11969 | Unchanged. The drain still starts at `removeTime`; the handover now completes instead of never. |
| Collapse deadline | 2 × lifetime, 11950 | Same predicate, now also applied at the ceiling (owner question 1) |
| Stats grace and weight window | 30 s | Unchanged; reused as is |
| Resize cadence | `WindowResizeTimeout` 15 s | Unchanged |
| Discovery backoff | 10 s → 2 min (323-324) | Unchanged; skipped passes neither advance nor reset it |
| Expansion deadlines | acquisition 15 s, setup 30 s, evaluation ping 30 s (14540-14545) | Unchanged for every candidate built; evaluation still precedes install |
| Health and probing | blackhole 30 s / 20 s, poll 1.25 s, idle backstop 10 s; cping 10 s rest on the 10 s grid and 30 s ack wait; unhealthy 15 s and 60 s; prober cadence; `ProbeSilenceWarnStreak` | Code untouched |
| Verdict-removal budget | 2 per 30 s | Untouched; a handover is a collapse, not a verdict |
| Platform transport timers | ping, 30 s read, 10 s write | Untouched |
| Refill after a removal | notify, then immediate pass | Unchanged |

Failure modes:
- **A victim gains a flow between eligibility and the swap.** Warned clients take no new flows, so the window is tiny. `replacementAllowed` (15238-15249) already accepts the same read-then-install race.
- **A candidate fails evaluation.** The reservation is released at the end of the pass, and the next pass may reserve again on the ordinary backoff.
- **The window closes mid-pass.** Existing cancellation applies, and the reservation dies with the pass.
- **Concurrent passes.** Resize is one goroutine per window. The reservation has a single owner and is checked under `stateLock`.
- **Stats.** A nil `WindowClientStats` is a no-op.

**Compatibility and rollout**
- **What changes:**
  - connect: window logic and the stats type;
  - sdk: one settings field, plumbed through;
  - server/proxy: the stats instance, the collector, and a proxy.yml kill switch `window_strict_handover` that turns off both the pre-check and the handover;
  - apps: they inherit the change with their next release.
- **No wire or protocol change.** The platform sees fewer mints, Provide frames and contracts. Mixed versions are per device: an old app keeps looping until it updates, and nothing on the server depends on the new behavior.
- **Order:**
  1. Proxy release N: counters only, behavior flags compiled off. Run 24 h on all blocks for the baseline.
  2. Release N+1: flags on, g1 first, A/B against g2–g10 for 24 h, then the rest.
  3. iOS and Android carry it in the next app release after a week on the proxy.

  Desktop is unaffected, because its windows are non-strict.
- **Rollback:** the kill switch with a block restart, or redeploy N.

**Tests**

Failing-first, composed windows, a fake generator, and deterministic barriers (no sleeps):

| Test | Asserts | Fails on old code because | Mutation it catches |
|---|---|---|---|
| `TestStrictWindowLifetimeDrainHandsOverSlot` | 5 installed, 1 drained and flowless. Exactly one candidate is built and admitted by handover. The installed count is 5 at every hook point, and the drained client is retired. | a candidate is built every pass, refused, and the drained client stays | dropping the swap branch; swapping before evaluation, which dips the count to 4 |
| `TestStrictWindowFullDoesNotMint` | 1 drained client carrying a flow. Over N passes: zero `NextDestinations`, zero mints, zero transports. The flow is still served. | mints every pass | turning the pre-check off |
| `TestStrictWindowDrainedFlowsHandOverAfterFlowsEnd` | no handover while flows are live; one handover after the last flow ends and the grace and weight windows pass | — | ignoring flows in eligibility |
| `TestStrictWindowCollapseDeadlineEscape` | past 2 lifetimes, the policy chosen in question 1 | — | — |
| `TestStrictWindowNeverBelowMinimumDuringHandover` | with a barrier inside admit, a concurrent selection snapshot always sees 5 installed and at least 4 selectable | — | — |
| `TestStrictAdmissionPrecheckEquivalence` | table over installed count, hard max, eligibility, replacing, and strict on or off: pre-check capacity > 0 exactly when the gate admits or a reservation exists | — | an off-by-one in the predicate |
| `TestStrictWindowFreedSlotRefillsImmediately` | a victim removed by a blackhole verdict between plan and admission: the next pass starts on notify and admits without waiting 15 s | — | — |
| `TestEnumeratorSkipsListingWhenStrictFull` | — | — | — |
| `TestNonStrictWindowTraceUnchanged` | a golden candidate and admission trace for desktop settings | — | — |
| `TestWindowClientStatsOutcomes` | each outcome counts exactly once; nil stats are safe | — | — |

Existing suites (`ip_remote_multi_client*_test.go`, including the Android loop noted in `window_min_test.go`) must stay green under `-race`.

**Production validation** (g1 against g2–g10, same hours):

| Measure | Acceptance |
|---|---|
| `declined_strict_cap` | → 0 |
| hosted mints per device-hour | from 31–44 at the peak to about 4 (rotation) plus the measured under-filled failures |
| `admitted_handover` | about installed ÷ 0.875 h |
| `strict_hole` windows in steady state | → 0 |
| `below_min`, `no_selectable`, `connect_dial_error` per admission, `connect_timeout` | not higher |
| perf: x509 and `AuthNetworkClient` shares | fall |
| proxy-originated `create_contract` and `provide` | fall |
| mcores per transport | fall in proportion to the strict share of the baseline |

**Savings and effort**
- **Base:** proxy at the 10-31 evening peak, 12–13.5 cores, with about 19% in the lifecycle (Part B §2.4).
- **Expected:** 1.2–2.5 cores. The upper end applies if the baseline shows strict declines dominate.
- **Platform:** 265–383 mints and retirements/s, about 1.25× that in contract opens and closes, and as many edge TLS handshakes and residents.
- **Phones:** battery, and the api's mint and contract load.
- **Effort:**
  - counters 2 days;
  - pre-check 1.5 days;
  - handover 2.5 days;
  - tests and review 2 days.

  That is about 8 engineering days in all.
- **Releases:** two proxy releases (counters, then behavior), then the next iOS and Android releases.

**Risks**
- Rotation starts working, so long-lived flows meet the collapse deadline in strict windows for the first time (question 1).
- If the baseline shows under-filled failures dominate, the CPU saving is closer to 1 core. The functional fix stands either way.
- Something may implicitly depend on drained exits staying put. Sticky windows never drain (`lifetimeDrainDue`, ip_remote_multi_client_sticky.go:52), and the golden non-strict trace guards desktop.

**Questions for the owner**
1. **A drained exit in a strict window that still carries flows at 2 lifetimes (90–120 min):** what happens?
   - (a) Hand over and cut those flows, exactly as non-strict collapse does today. **Recommended.**
   - (b) Never cut; that slot waits for its flows to end.
   - (c) A longer strict-only deadline.

   Changes: the eligibility predicate and its test.
2. **Which warned clients may hand over?**
   - (a) Only lifetime drains.
   - (b) Every warned cause whose intent is backfill (drain, probe silence, quarantine, starvation, unhealthy-warned), under the same collapse test. This matches non-strict collapse. **Recommended.**

   Changes: the eligibility filter and two more tests.
3. **Rollout:** how cautious?
   - (a) A counters-only release first, for a 24 h baseline. **Recommended.**
   - (b) Counters and behavior together on g1, with g2–g10 as the baseline. One release, a weaker baseline.

   Changes: one or two proxy releases.
4. **Under-filled windows:** if the baseline shows they dominate, may an identity that one provider evaluated be kept for that same provider's next attempt within 5 minutes?
   - (a) Yes, same provider only, with the existing park bounds. It adds no cross-provider linkability. **Recommended for a follow-up design.**
   - (b) No; keep retiring after any provider contact.

   Changes: scopes the follow-up only; nothing in D2.
5. **Apps:** should D2 ship to them?
   - (a) The next iOS and Android release after a week on the proxy. **Recommended.**
   - (b) Proxy only for now.

   Changes: the release plan.

### D3. `PeerAudit`: answer it without the auth path, and stop sending it

**Problem and evidence** (CPUOPT3 C §2.3, §2.4, §3 #1; B §3.6, §4 #4)

- `SequencePeerAudit.Complete` (connect transfer.go:18781–18811) sends a lone `PeerAudit` frame through `ClientOob().SendControl` and ignores the answer (`func(resultFrames, err) {}`, :18806–18809).
  - No server package names `PeerAudit`. `ConnectControlFrames` ends in `default: "Cannot handle oob control message: %T"` (controller/connect_controller.go:552).
- **Rates now** (Mimir, 30 min to 10-31 21:00Z). CPUOPT3's 18:20–19:25Z figures are in brackets.
  - api 2,646/s [2,601–2,731].
  - proxy 2,162/s [2,255].
  - alt 24.5/s [25.4; 87% of alt's control requests].
  - taskworker 0.02/s; the connect resident 0.
- **The rate is not stable over months.** It follows live peer relationships (below), not user bytes. No version label exists to tie the steps to releases.
  - api daily means: 0.2–0.6k/s from 09-11 to 10-12, 1.2k/s from 10-13, 1.8k/s from 10-21, and 2.3–2.4k/s since 10-26.
  - The proxy went from about 30/s to 1.5–2.3k/s over the same weeks.
- **Cost of each audit at the api** (about 0.6–0.75 ms of CPU, C §2.4):
  - the lag-guest claims decode, then ES256 verification and claims checks;
  - the per-client GCRA `EVALSHA` (controller/connect_control_limit.go:108);
  - a refusal-replay decode, which returns false;
  - the revocation-record `HMGET` (session/revocation_record.go:331), or the SQL state query on a miss;
  - a session last-used sample, at most once a minute per session per process;
  - a second pack decode;
  - the error path (`strings.ToLower`, connect_controller.go:195–223), and a JSON reply.
- **Cost of each audit at the proxy**, in process (localclient/authority.go:244):
  - P-256 signature and claims checks;
  - `ValidateByJwtState` (a record `HMGET`; 11% miss to SQL, which is 381/s at the proxy);
  - the dispatch error, and a `convert` JSON round trip.
  - In total 2.5–3% of proxy CPU, plus 0.35% on the client side (B §3.6). Audits are 65% of the proxy's 3.3k/s control record checks.

**History** (`git log -S/-G` in connect and server):
- **2023-07-12, connect 26d51ab3:** added `PeerAudit`. The proto comment reads "the acl will block contracts between two parties with bad audits of each other". The receive path carried FIXMEs ("submit peer audit", "if peer audit has any bad marks, downgrade the connection"). Neither the ACL nor the downgrade was ever built.
- **2023-10-29, connect e6b36428:** added `Client.ReportAbuse`, an audit with `abuse=true`, for the provider egress policy's Incident verdict.
- **Until 2024-04:** audits went in-band to `ControlId`. The resident controller had cases only for Provide, CreateContract and CloseContract and no default, so it dropped them silently (server 0ad013fde^:connect/resident_controller.go).
- **2024-04-02:** connect a63ae0a7 moved `Complete` to `ClientOob().SendControl`. The same day, server 0ad013fde started out-of-band control with CreateContract only and the "Cannot handle" default. Every audit since then has been an API request that fails.
- **Nothing has ever consumed one:**
  - No server code ever handled `PeerAudit` or `ReportAbuse`. The only source hit is a perfvar test that widens `MaxPeerAuditDuration`; the other `-S` hits are committed binaries.
  - Provider reliability (`client_reliability`), contract disputes and provider exclusions are all derived on the server and never read audits.
  - No document in server or connect plans a consumer. `protocol/audit.proto` (2024-06) is a separate, unbuilt design for provider egress records.

**Who sends it, and when**
- **Senders:** every connect `Client` built with an `ApiOutOfBandControl`: app and sdk window clients over HTTP to the api or alt (ip_remote_multi_client_api.go:1326), sdk providers over HTTP to the api (sdk device_local_provider.go:227), and proxy hosted window clients in process. The exchange resident uses `NoContractClientOob` (server connect/resident.go:4221) and sends nothing.
- **Triggers:**
  1. **Receive sequences.** Each receive sequence (one per peer and stream, transfer.go:15828) holds one audit, created on its first update. The audit completes on:
     - the first update at least `MaxPeerAuditDuration` (60 s, :1093) after it began;
     - a verified stream change (`SetStreamId`, :18758, called at :17561);
     - sequence exit (:15816).
     The proxy's measured rate is about one audit per window client every 45–60 s (about 100k window clients). That fits one completion per live window client per minute, plus exits. So the rate is about live receive sequences divided by 60 s.
  2. **Malformed frames.** `Client.run` (:4430) sends one immediately for each malformed inbound transfer frame.
  3. **Abuse reports.** `ReportAbuse` (:3254) fires for each packet or fragment group that gets an Incident verdict at a provider (ip.go:10209, :10307). Incident covers non-public destinations, and BitTorrent signatures by default (`ReportBittorrentIncident: true`, ip_security_dmca.go:163). There is no dedupe and no rate limit.
- **Shape:** always one frame per request, because `sendControl` packs exactly the frames it is given (transfer_oob_control.go:180).
  - The `PeerAudit` message is about 45–60 bytes: peer_id (16 B), stream_id (16 B), duration, abuse and 9 counters.
  - That becomes about 80 base64 characters and a ~90-byte JSON body. The bearer JWT dominates the ~1 KB of headers.
- **Finding: abuse reports spend the provider's own budget.** Each report counts against the provider's per-client control budget (6,000/min, burst 3,000; model/connect_control_limit.go:69).
  - A remote user who pushes about 100 blocked packets/s through a provider gets that provider's real `CreateContract` and `Provide` requests refused with 429.
  - This needs a modified client: stock clients drop these packets locally.

**Proposed design**

Server: one change that covers api, alt and proxy.
1. **`controller.DiscardConnectControl(args *ConnectControlArgs) (*ConnectControlResult, bool)`**, in a new file `controller/connect_control_discard.go`.
   - **Inputs it reads:** only the request body. It reads no credential, Redis, database or clock.
   - **Conditions for a discard:**
     - `len(args.Pack) ≤ 2048` (connect sends about 80);
     - a plain base64 decode, then `proto.Unmarshal` into `protocol.Pack`;
     - at least one frame;
     - every frame is `MessageType_TransferPeerAudit` and its bytes decode as `protocol.PeerAudit`. The loop stops at the first frame that is not an audit.
   - **Anything else** returns false and takes today's path, untouched. This follows the `createsContractsOnly` precedent (localclient/authority_contract_admission.go:122).
   - **The answer** is `{"pack":"","error":null}`, exactly what an authenticated no-op returns (`connectControlResult(nil, nil)`). It is the same for every caller and grants nothing.
2. **api and alt.** `router.WrapWithInputRequireClientUnlessAnswered` (router/handler_utils.go:418) gains a first argument, `discard func(T) (R, bool)`.
   - The wrapper has one caller, api/handlers/connect_handlers.go:22. alt serves the same router (alt/run.go:183).
   - The discard runs after the JSON decode and before `advanceControlHttpPhase(controlHttpAuthenticate)` (:429).
   - The route's gates still run first: the intended-network gate, then the lag-guest gate (api/route_authz.go:361–376).
   - Under Q1 (b), the same check instead runs first in `PrepareConnectControl` (connect_control_limit.go:137), before `checkConnectControlLimit`, and answers with `answer.discarded`. The `BeforeStateAnswer` contract (session/client_session.go:212–219) gains one clause: "or acknowledge, empty, a pack the server discards whole".
3. **proxy.** `Authority.ConnectControl` (localclient/authority.go:244) calls the same check after its `args == nil` test and the lifecycle checks, so a closed authority still returns `context.Canceled`.
   - The check runs before contract admission and before `authenticate` (:100). Under (b) it runs after `parseClaimsPreflight` (:125).
   - It returns `&connect.ConnectControlResult{}` directly, with no `convert` round trip.
4. **Dispatch no-op.** Add `case *protocol.PeerAudit:` next to `ControlPing` (connect_controller.go:540).
   - It covers mixed or in-band packs, and audit packs too large for the discard.
   - It also stops a stray audit's joined error from making the client drop its sibling replies (connect transfer_oob_control.go:211).
   - The replay gate (:517) stays ahead of it, so an audit inside a replay is still refused.
5. **Counter.** `urnetwork_connect_control_discarded_requests_total{abuse="claimed|none"}`, with both cells pre-materialized. It counts the requests answered by (1); every connect version sends one frame per request.
   - The `abuse` label shows whether any frame sets `abuse=true`. It is spoofable and serves as telemetry only, to size Q4.
   - Nothing is logged at any level: the counter is lossless, and peer ids are identifiers.
   - **No `peer_audit` frame-metric label** (CPUOPT3 C #1 suggested one). Monitor §2.19e marks a process invalid on any unknown frame label (monitor/signal_control_route_pressure.go:162–172).
   - **That parser already fails today.** `resident_move_result` was added on 10-22 (65778f4d1), and all 20 api processes now export it, so every api slot reads as unobservable. The monitor's list (:39) gains `resident_move_result`.
6. **Monitor §2.19e denominators.** Subtract `increase(discarded_requests_total[5m])` from `count` and `requests` (:236–240). The mean and canceled share then cover only requests that do work, so fast discards cannot dilute the 5 s / 20% and 10 s / 50% thresholds.

**What stays the same:**
- Every pack that is not audit-only takes today's path, check for check.
- The refusal ring, the escrow, contract and transfer accounting, the GCRA parameters, the revocation-record TTLs and the session deadlines are unchanged. The discard path reads none of them.

Client (connect):
- **`ClientSettings.SendPeerAudits bool`** (transfer.go:1578), default false.
  - `NewSequencePeerAudit` (:18746) records the setting. When it is false, `Update` (:18766), `Complete` (:18781) and `SetStreamId` return before `time.Now()` and before building the frame.
  - `ReportAbuse` and `Client.run`'s `updatePeerAudit` both go through `NewSequencePeerAudit`, so all three senders stop.
- **Local policy verdicts are untouched.** Blocked packets are still dropped and counted (`recordProviderBlock`). Only the unread report goes away.
- **sdk:** a connect bump, with no code change. Proxy and taskworker pick it up through the server release lock; apps and providers through their next sdk.
- **Spec note** in connect api/bringyour.yml, `/connect/control`: an audit-only pack is answered empty and stored by no one (under (a), without reading the credential).

**Alternatives considered:** the dispatch no-op alone (B #4's server variant) keeps the verification and both Redis round trips, so it saves only about 0.1 of the 0.6–0.75 ms. The client change alone leaves old apps and providers on the full path for months. Batching audits still has no consumer. nginx cannot read the protobuf body. The message type must stay so old frames decode. Deleting the client accounting now (about 40 call sites and 12 test files) waits for Q3 and Q4.

**Correctness under the owner rule**

Checks on each path:

| Path | Checks kept | Checks skipped |
|---|---|---|
| api/alt audit-only, (a) | per-address limit (nginx `route_rate_limits`; alt/rate_limit.go), intended-network gate, lag-guest gate, 16 MiB body cap, then 2 KiB plus the shape check | signature and claims, session checks, GCRA, revocation record, dispatch |
| api/alt audit-only, (b) | as (a), plus ES256 and claims, session horizon, session marker (untagged credentials), remembered refusals | GCRA, revocation record, dispatch |
| api/alt, any other pack | all, unchanged | none |
| proxy audit-only | lifecycle checks and the shape check; (b) adds `parseClaimsPreflight` | revocation record, dispatch; (a) also skips the signature |
| proxy, any other pack | all, unchanged | none |

Timing and detection bounds:
- **`AuthTimeout` (30 s, session/auth_deadline.go:15) and the intended-network check's 30 s:** every authenticating request keeps them. The discard does no I/O on at most 2 KiB, so it needs no deadline.
- **GCRA, 6,000/min with burst 3,000:** the parameters and the per-client ceiling on state-touching requests are unchanged. Only audit-only requests stop counting against them (Q2).
- **Revocation-record freshness and TTLs:** unchanged.
  - Every request from a revoked credential that does something is still refused.
  - Its audit gets the same empty 200 that recorded refusals already give such a client (connect_control_refusal_replay.go:11–13).
- **Refusal ring (60 s and 1 h):** unchanged and not consulted.
- **Proxy:** the per-request `RequestTimeout` (connect transfer_oob_control.go:127) and the contract admission bound are unchanged.
- **Connect timers:**
  - `MaxPeerAuditDuration` is a reporting period, not a bound.
  - Receive `IdleTimeout` and `GapTimeout`, `WriteTimeout` (15 s), ack timers, cping cadence and blackhole detection read nothing from audits.
  - The backend-degraded gate (3 failures in 2 min, transport.go:416–421) counts only auth and `CreateContract` results (transfer_contract_manager.go:1905, 1931).
- **Monitor §2.19e:** the thresholds are unchanged; the denominators exclude discards.
- No new timers, no AfterFunc, no watchdogs.

Failure modes:
- **A shape-check bug that answered a frame that acts** would be a security bug. The guards are:
  - an exact type check on every frame;
  - a full decode;
  - the size cap;
  - a fuzz invariant (see Tests).
- **A malformed body** falls through to today's path.
- **No races:** the check is a pure function with an atomic counter.
- **An unauthenticated flood** of audit packs costs about 0.1 ms per request, behind the per-address limits.
  - That is less than anyone can already cost the route per request: a well-formed token with a bad signature forces a full ES256 verification.
  - The answer is constant, smaller than the request, and has no token-dependent timing. So it is neither an oracle nor an amplifier.

**Compatibility and rollout**
- **Mixed versions:**
  - Every pairing of server and client versions works; there is no protocol change.
  - Old clients (connect from 2024-04 on) keep sending until they update, and the server path stays at about 0.1 ms per request indefinitely.
  - New clients send nothing to old servers.
  - Old clients get an empty 200 where they used to get a 200 with an error, and in place of some 401s, 429s and 503s. The callback discards it, and logout and refresh are driven by transport authorization (`AuthorizationClosed`), not by out-of-band results.
- **Flags:**
  - Server: none. The check is a pure function, so canary and redeploy are the rollback.
  - Connect: `SendPeerAudits`, default off, so a future consumer can turn sending back on.
- **Order:**
  1. The monitor label fix, now and independently.
  2. api on one block for 24 h as an A/B, then the fleet and alt.
  3. The proxy release.
  4. Merge connect, then bump the server release lock (proxy, taskworker).
  5. The sdk bump, then app and provider releases on their normal schedule.
- **Rollback:** redeploy the previous api, alt or proxy, or flip the connect default back. Nothing is persisted.

**Tests**

All are written to fail first; the mutation each must catch is in brackets.

Server:
- `TestDiscardConnectControlAnswersAuditOnlyPacks`: packs of one and three audits are answered empty; the counter splits by abuse; the answer equals `connectControlResult(nil, nil)`. [always return false]
- `TestDiscardConnectControlRefusesEveryOtherShape`: every other `MessageType` value, audit plus CreateContract, audit plus CloseContract, an empty pack, bad base64, bad protobuf, an audit with undecodable bytes, and a pack over 2 KiB all return false. [check only frames[0]; drop the cap; accept on type without decoding]
- `FuzzDiscardConnectControlNeverAnswersAnAction`: if it answers, every frame is a decodable `PeerAudit`; no input panics.
- `TestConnectControlRouteDiscardsAuditWithoutCredential` (httptest): no token, and a garbage token, each get `{"pack":"","error":null}`; the JWT parse, the `connectControlLimitAdmit` seam and the record read are never called; the same request with a CreateContract frame gets 401. [move the discard after auth] Under (b): a garbage token gets 401, and admission and the record are still not called.
- `TestConnectControlRouteDiscardKeepsLagGuestGate`: a lag-guest token still gets 403. [answer before the gate]
- `TestConnectControlDiscardSpendsNoClientBudget`: GCRA state is unchanged after 100 discards. [discard after the limit]
- `TestConnectControlFramesTreatsPeerAuditAsNoOp`: in an [audit, close] pack the close applies, there is no error, and the `unhandled_message` counter does not move. [remove the case] `TestReplayStillRefusesUnpreparedPeerAudit`. [case above the replay gate]
- `TestAuthorityDiscardsAuditBeforeAuthenticate`: an expired token gets an empty result; a closed authority still gets `context.Canceled`. [discard after authenticate]
- `TestControlRouteMessagesCoverControllerLabels`: the monitor's list includes every controller label. It fails today on `resident_move_result`.
- `TestControlRoutePressureSubtractsDiscards`: 25% discards plus real requests at 6 s with 25% canceled still raise WARN. [drop the subtraction]

Connect:
- `TestSequencePeerAuditSendsNothingByDefault` (synctest, with a recording out-of-band control): a 60 s rollover, a stream change and an exit send nothing. [ignore the setting]
- `TestReportAbuseSendsNothingByDefault` and `TestClientRunMalformedFrameSendsNoAuditByDefault`.
- `TestSequencePeerAuditSendsWhenEnabled`: exactly 3 single-frame packs, with the old field values.
- `BenchmarkSequencePeerAuditUpdate`, with the setting on and off.
- The existing receive-timing tests are the regression gate.

Production (A/B by block, the same hours over at least 3 days). Acceptance:
- `control_frame_failures_total{cause="unhandled_message"}` below 1/s, and `discarded_requests_total` within ±5% of the old `unhandled_message` rate.
- api cores down 1.3–1.7 at the peak (0.7–0.9 under (b)).
- `redis_pool_hits_total` down 5.3k/s at the api and 2.2k/s at the proxy.
- `jwt_state_record_checks_total{operation="control"}` down 2.6k/s at the api and 2.2k/s at the proxy.
- `client_limit_total{outcome="limited"}` does not rise.
- For the remaining requests, the authenticate and controller phase means do not rise, and the contract and rejection rates are unchanged.
- §2.19e evaluates every slot.

Expected drops that are not regressions: auth attempts (down 2.6k/s), the `active_authenticated_networks` gauge, and 401s from stale-token audits. After the client changes ship, the proxy's discards fall to about 0 with its release; at the api, discards decay as apps and providers update (check weekly).

**Savings and effort**

| Item | Saving | Base |
|---|---|---|
| api, (a) | 1.3–1.7 cores; keeping the lag-guest gate leaves about 0.1 ms per request (≈0.26 cores) | 2,646 requests/s × 0.6–0.75 ms; api 13.0 cores mean, 17.4 at the peak (C §2.1) |
| api, (b) | 0.7–0.9 cores. If the JWT claims cache (CPUOPT3 #4) ships, (a) and (b) differ by less than 0.1 | same |
| alt | about 0.02 cores | 25 requests/s |
| proxy | 0.25–0.3 cores from the server side, plus 0.05–0.1 after the connect change | 2,162 requests/s; 12–13.5 proxy cores |
| Redis | about 7.5k fewer round trips/s, ≈0.7 cores | api 2 × 2,646, plus proxy about 2,200; about 0.09 cores per 1k round trips/s (E #1, #5) |

- Also removed: the SQL fallbacks for credentials whose only checks are audits. Up to about 250/s at the proxy are driven by audits.
- On every app and provider: one HTTPS request per live peer per minute.
- **Effort:** server 1.5–2 days (including the monitor), connect 1 day, plus an sdk bump.
- **Releases:** monitor, api/alt, proxy, the connect pin, then sdk and apps.

**Risks**
1. **Posture under (a):** one body shape on `/connect/control` is answered without a credential.
2. **Session last-used** is presentation-only (session_activity.go:1: it "never grants or extends authority"). It will lag for sessions whose only api traffic is audits. The active-networks gauge on the internal api dashboard falls.
3. **No abuse signal after the clients update.** It was discarded unread all along. `abuse=claimed` gives a sample of about 2 weeks before clients update.
4. **Residual cost from old clients:** about 0.26 cores now, falling with adoption.

**Questions for the owner**
1. **Where to answer audit-only packs (auth posture).**
   - (a) Before authentication, after the route gates: 1.3–1.7 api cores.
   - (b) After the signature, claims and session checks, before the per-client limit: 0.7–0.9 cores, or within 0.1 of (a) once the JWT claims cache ships.
   - (c) After the per-client limit: about 0.4 cores. Audits still spend the budget and an `EVALSHA`.
   - **Recommended: (a).** It reads no credential and returns a constant. It costs less than a bad-signature request anyone can already send, and every frame that acts keeps the full path. Choose (b) if "every 200 on `/connect/control` follows a verified signature" is worth about 0.6 cores (about 0.1 with the claims cache).
   - **What changes:** where the hook sits (the wrapper or `PrepareConnectControl`), the proxy's placement, the spec note, and the savings row.
2. **The per-client budget.** Under (a) or (b), audit-only requests stop spending the 6,000/min budget (burst 3,000).
   - (a) Keep 6,000 and 3,000.
   - (b) Lower the rate by the audit share (about 4,500/min). That keeps a typical client's allowance for real requests where it is today, and tightens clients that send no audits.
   - (c) Keep counting audits. This forces Q1 (c).
   - **Recommended: (a).** The per-client ceiling on state-touching requests is unchanged, and it closes the path by which blocked packets get a provider's own contract requests refused.
   - **What changes:** `connect_control_limit.yml`, or Q1's placement.
3. **Keep PeerAudit as a feature?**
   - (a) Retire it: stop sending, discard forever, and delete the client accounting one release later.
   - (b) Build a consumer: sampled, batched audits for peer scoring.
   - (c) Leave the accounting behind `SendPeerAudits` until a scoring design asks for it.
   - **Recommended: (a).** Reliability, disputes and exclusions already come from server-side evidence, and the 2023 "ACL on bad audits" was never specified.
   - **What changes:** whether a connect cleanup task is scheduled.
4. **ReportAbuse.**
   - (a) Drop it along with PeerAudit. The provider is already protected by the local drop.
   - (b) A minimal real feature:
     - a new `AbuseReport` control frame carrying the source client, the verdict's reason class, a count and a window;
     - on the provider, deduplicated per source and reason, at most once per 5 min, in a bounded map;
     - sent over the authenticated path;
     - accepted by the server only from a reporter that holds a recent contract with that source;
     - stored as a daily count per client, with no automatic action, plus an ops query.
   - (c) Option (b) plus enforcement (route exclusion or account action), which needs a policy and legal decision.
   - **Recommended: (a) now.** Read `abuse=claimed` for 2 weeks before clients update, then revisit (b) if the rate is material.
   - **What changes:** (b) adds about 2–3 days across connect and server.
5. **Rollout appetite.**
   - (a) The monitor fix now. Then api on one block for 24 h, then the fleet, alt and proxy. The connect change ships with the next scheduled connect pin and app releases.
   - (b) As (a), with a dedicated sdk and app release.
   - (c) Hold the server change until the clients ship.
   - **Recommended: (a).** The server change alone removes about 85% of the api cost; the client change mainly saves device requests.
   - **What changes:** the schedule only.

### D4. Cache verified results: JWT claims (a) and TLS certificate chains (b)

Each cache remembers the result of one expensive, deterministic check: an ECDSA signature over
exact token bytes, or a certificate chain over exact DER bytes. Every check that depends on time,
state or a name still runs on every use. I read server a8461e236 (deployed) and main 3ae2c76f7,
and connect 89b15920 (deployed) and main c62b060e. The files involved are the same at both
revisions.

#### D4(a). A cache of verified JWT claims

**Problem and evidence**
- **api (CPUOPT3 Part C §2.4).** JWT handling is about 19% of api CPU, or 225–260 µs per
  authenticated request. It is the ES256 verify and claims decode in `parseByJwtSigned`
  (session/by_jwt.go:420–449), a second unverified decode in the lag-guest gate
  (router/lag_guest_credential.go:61 → by_jwt.go:763), and P-256 assembly samples whose stacks
  are cut off. Part C #2 puts the saving at 2.2–2.9 api cores at the 10-31 peak.
- **proxy (Part B #6).** `localclient.Authority.parseClaimsPreflight`
  (localclient/authority.go:125–153) verifies the same window-client token again on every control
  call; that is 2.2% of proxy CPU.
- **Tokens repeat.** A busy api process sees 1.6–1.9k distinct networks per 5 minutes against
  about 330k requests (`urnetwork_session_active_authenticated_networks`).
- **Many tokens are expired.** On 10-31, 1,463 of about 10k api parses/s carried an expired client
  token, accepted only because `auth.yml` `reject_expired` is false; 57/s carried no exp
  (`urnetwork_auth_jwt_legacy_accepts_total`).

**Proposed design**
- **Where.** One cache per keyring, a new field `byJwtKeyring.verified`
  (session/by_jwt_keyring.go), consulted only inside `parseByJwtSigned`. Every verifier already
  goes through that function: api authentication (session/client_session.go:263), the three router
  gates, connect's transport handshakes (connect/transport.go:1692, 2757), localclient in the
  proxy, and the auth controllers. Tests that build their own keyrings stay isolated.
- **Key.** SHA-256 of the whole token string as presented (header, payload and signature). Never
  a prefix or a single field, so tokens that differ in one signature byte get different keys.
- **What is stored.** The verified, decoded claims before `fixByJwt`, as a deep copy. Each hit
  returns a fresh deep copy from `(*ByJwt).clone()`, which copies the four `*server.Id` fields,
  `Roles`, `Audience` and the three `*NumericDate`. No caller can change the cached object,
  including mint paths that copy the presented `CreateTime` into derived tokens.
- **Successes only.** A malformed token, a bad signature, a refused method or a kid miss is never
  stored; a retry verifies again, as today.
- **Lifetime.** The base TTL is verify time plus a cap; I propose 10 minutes (Q2).
  - With `reject_expired` on, an entry also ends at exp plus the 30 s leeway, and a token already
    past that is not stored.
  - With `reject_missing_expiration` on, a token without exp is not stored.
  - While `reject_expired` is off, an expired token is kept for the cap only (Q1). Its exp is
    still evaluated on every request, and the gate still decides.
- **Key rotation.** Keys are only ever added while a process runs; a key the vault drops keeps
  verifying until restart (by_jwt_keyring.go:16–17). A reread that gains keys therefore leaves
  every entry valid, and must not flush, or anyone sending unknown-kid tokens could force a flush
  every 10 s. Each entry records the keyring's removal generation; any future code that drops a
  held key bumps it, which invalidates every entry at once.
- **Size.** A sharded LRU: 16 shards, each with its own mutex (taken inside the method with
  `defer Unlock()`). The default is 65,536 entries per process, about 1 KB each, so at most 64 MB.
  Expired entries are dropped on read and the oldest evicted on insert.
- **Single-flight.** A miss joins any in-flight verification of the same key
  (`singleflight.DoChan`; x/sync is already an indirect dependency). Each waiter selects on its own
  ctx, so its deadline stays its own (AuthTimeout 30 s, session/auth_deadline.go:15). A failed
  flight goes back to the waiters already joined, and nothing is stored.
- **The second decode.** `requestLagGuestCredential`, `requestClientCredential`
  (router/client_credential.go:88) and `requestCredentialNetworkId`
  (session/intended_network.go:156) each decode the token unverified today.
  - Each first calls `session.verifiedClaimsCached(token)`, a lookup that never verifies.
  - On a hit it reads `LagGuest`, `ClientId` or `NetworkId` from the verified claims and skips the
    unverified decode. These are the same bytes through the same decoder, so the value is the
    same.
  - On a miss it decodes unverified, as today. A guest token is still verified through
    `ParseByJwtForAudience` (now a hit) before it is refused.
- **Kill switch:** `auth.yml` `jwt_verified_cache_entries`; 0 disables. It is read once per
  process, like the gates.
- **Metrics:** `urnetwork_auth_jwt_verified_cache_total{result=hit|miss|shared|store|evict|expired}`
  and an entries gauge.

**What still runs on a hit.** `ParseByJwtForAudience` (by_jwt.go:466–539) is unchanged:
- the audience argument check and both gate reads;
- `validateSessionHorizon`;
- exp presence (against `reject_missing_expiration`) and exp passed (against `reject_expired`,
  with the legacy-accept counter);
- nbf and iat, with leeway;
- the identity claims: issuer, audience, subject equal to the user, jti and CreateTime;
- `fixByJwt`.

After that, every caller's state check runs as before: `ValidateByJwtState`/`ForParent`, or
`validateByJwtStateBeforeQuery`, the answer, then `validateByJwtStateAfterBeforeQuery`. That
covers the remembered refusals, the session marker, and the Redis revocation-record HMGET or its
SQL fallback. Skipped on a hit: base64 and JSON decoding, the kid lookup, method pinning and the
signature. These are fixed properties of the same bytes under the same held keys.

**Alternatives considered:**
- Cache the whole `ParseByJwtForAudience` result: no; the time and gate checks must run per
  request.
- Key by `jti` or a prefix: no; the key must bind the signature.
- A shared Redis cache: no; a Redis round trip costs about as much CPU as the verify.
- Cache failures: no; a newly read key version can make a failed token valid within 10 s.

**Correctness under the owner rule**
- **Revocation.** The state check runs on every request with the same round trip, and none of it
  is cached. A removed or rotated client is refused on its next request, exactly as today. The
  replay path's existing `BeforeStateAnswer` contract is unchanged.
- **Expiry, nbf, iat and session horizon** are evaluated per request from the claims against
  `NowUtc()`. An expired token is refused (gate on) at the same instant, cached or not. The cap
  never extends acceptance.
- **Gates** are read per request. The tests' atomic overrides keep working, because no outcome is
  cached.
- **Key rotation.** New key versions sign new tokens, which get new keys; old tokens verify while
  their key is held, as today.
- **Key compromise.** Today the key must be removed from the vault and every verifier restarted,
  since keys are never dropped while running. A restart also empties the cache, so the response is
  unchanged. Any later in-process removal invalidates every entry through the removal generation
  (Q3).
- **Races.** A waiter cancelled mid-flight returns its ctx error. A failed flight stores nothing.
  LRU operations hold their shard lock, and stored claims are immutable.
- **Timeouts.** None is added or changed. Single-flight waits are bounded by each caller's ctx.

**Compatibility and rollout**
- Server only (session and router). The api, alt, connect, taskworker and proxy binaries pick it
  up in their next release. There is no wire, client or schema change, and each process is
  independent, so mixed versions are fine.
- **Order:** the api g1 block first with the kill switch ready, then the other api blocks, then
  alt, connect and the proxy.
- **Rollback:** `jwt_verified_cache_entries: 0` plus a restart, or the previous release.

**Tests** (failing first; each catches the mutation named)
1. `TestByJwtVerifiedCacheSecondParseSkipsSignature`: two parses of one token run one
   verification, counted through a seam. Catches the cache not being consulted.
2. `TestByJwtVerifiedCacheKeyIsWholeToken`: a token with the same header and payload but one
   signature byte changed is verified and rejected. Catches keying by prefix or by claims.
3. `TestByJwtVerifiedCacheNeverStoresFailures`: a bad-signature token fails twice, then verifies
   once its key is read. Catches negative caching.
4. `TestByJwtVerifiedCacheExpiryPerRequest`: cached, clock past exp: refused with `reject_expired`
   on; accepted and counted on every hit with it off. Catches caching the claims decision.
5. `TestByJwtVerifiedCacheStateCheckEveryRequest`: authenticate, deactivate the client, then
   authenticate again from the cache: refused with `no_active_row`. Catches caching the state
   verdict.
6. `TestByJwtCloneSharesNothing`: walks every field of `ByJwt` by reflection; changing a returned
   copy leaves the next hit unchanged. Catches shallow copies, including of fields added later.
7. `TestByJwtVerifiedCacheRereadDoesNotFlush` and `…RemovalGenerationFlushes`.
8. `TestByJwtVerifiedCacheSingleFlight`: 32 concurrent cold parses run one verification; a waiter
   with a cancelled ctx returns at once.
9. `TestByJwtVerifiedCacheBoundedAndTtl` and `TestLagGuestGateReadsCachedClaims` (no unverified
   decode on a hit; the same decisions for guest and non-guest tokens on hits and misses).

- **Benchmark:** a hit against a miss in session/by_jwt_bench_test.go; expect about 2 µs against
  about 150 µs.
- **Production checks:**
  - P-256 plus `ParseWithClaims` under 2% of api CPU, and a hit ratio above 90%;
  - `urnetwork_auth_jwt_rejections_total` and the legacy-accept counter unchanged by cause;
  - api cores per 1k requests down at least 12% on g1 against g2–g4.

**Savings and effort**
- api: 2.2–2.9 cores at the 10-31 peak, against a 17.5-core base; 1.9–2.5 cores if expired tokens
  are not cached (Q1 option A).
- proxy 1.5–2% of its CPU; connect and alt small.
- About 3 engineering days; one server release (api, alt, connect, proxy).

**Risks**
- A clone bug could leak claims between requests (test 6).
- A wrong bound could use memory; the design caps it at 64 MB.
- A gate's cache lookup must never become a verification path (test 9).

#### D4(b). A verified-chain cache in connect's TLS client

**Problem and evidence**
- **taskworker (CPUOPT3 Part C §2.6).** x509 chain verification takes 2.59 cores, 16.9% of
  taskworker CPU, almost all on the egress probe's control-plane dials (`clientTlsHandshaker`
  → uTLS `verifyServerCertificate` → `x509.(*Certificate).Verify`). Each tunnel builds a fresh
  `ClientStrategy` (providertunnel/tunnel.go:522), so its session caches start empty. The probe
  makes about 4.4 TCP dials per attempt, at 61.5 attempts/s.
- **proxy (Part B #3).** `Verify` is 5.3% of a proxy process (P-384 4.6%, RSA 0.5%), about one
  full verification per platform dial; crisp makes 170 such dials/s.
- **Why it costs so much.** The platform chain is leaf ← YE1 ← Root YE ← ISRG Root X2, plus an X1
  cross-sign. Go builds about four P-384 ECDSA verifications and one RSA verification per
  handshake, and its P-384 is generic code: 1.52 ms per verify on an M1 Max, against 0.11 ms for
  P-256.

**Proposed design**
- **Where.** connect net_tls_hello.go: `newChromeTlsConfig` (:129) and the Chrome-hello branch of
  `clientTlsHandshaker.handshake` (:275). The Chrome hello is the default for API dials
  (net_http.go:378–380), platform and WebSocket dials (net_resilient.go:95–98), and the extender
  and VLESS inner dials. These run in every connect client: the taskworker probe, proxy hosted
  devices, apps and providers.
- **Which configs.** A dial is eligible only if `InsecureSkipVerify` is false,
  `VerifyPeerCertificate` and `VerifyConnection` are nil, FIPS-only mode is off, and `RootCAs` is a
  pool connect built and registered.
  - Registration happens in `sharedDefaultPinnedCertPool` (net_tls.go:89) and in the
    extra-roots clone in `defaultRootCertPool` (:128). Each records an identity: SHA-256 of the
    embedded pin PEM plus any extra-roots file bytes.
  - Not eligible, so unchanged: system roots; the extender and VLESS outer configs (they set
    `InsecureSkipVerify` and have their own verifiers); peer encryption (transfer_encrypt.go);
    caller configs in sdk `socket.go`; the Go-hello path; H3/QUIC (Q5).
- **Verification on an eligible dial.** The uTLS config gets `InsecureSkipVerify = true` and a
  `VerifyConnection` method on a per-handshake `chainVerification` struct. The method:
  - computes the key: SHA-256 of "v1", the root identity, the length and bytes of `ServerName`,
    and the length and DER of each presented certificate;
  - on a valid hit, one whose window covers now, runs `leaf.VerifyHostname(ServerName)` and
    accepts;
  - otherwise runs exactly the verification uTLS would (`Roots`, `CurrentTime` from the config
    clock, `DNSName = ServerName`, the presented certificates after the leaf as intermediates);
  - on failure returns `&tls.CertificateVerificationError{UnverifiedCertificates, Err}`, the type
    the handshaker and its callers classify today, and on success stores the entry.
- **Verified chains stay visible.** The chosen chains are kept on the struct and attached to
  `chromeTlsConn`, whose `ConnectionState()` (:181) returns them as `VerifiedChains`. Callers see
  the same state as today.
- **Entry, hit rule and expiry.** An entry holds the validity window of the first verified chain,
  leaf through root (latest NotBefore to earliest NotAfter), the verified chains and the insert
  time. A hit needs NotBefore ≤ now ≤ NotAfter, Go's own comparison, on the config's clock. An
  entry expires at the earlier of the earliest NotAfter and insert time plus a 1 h cap. The cache
  holds 256 entries in an LRU under one mutex.
- **No single-flight.** A miss costs bounded CPU and no I/O; duplicate verifications happen only
  in the first milliseconds after a cold start.
- **On a mismatch.** A different byte in any certificate, a different server name, a different
  root identity, or a window that no longer covers now is a miss: a full verification, with
  today's errors. Errors are never cached, and any internal failure falls back to full
  verification. The only ways to accept are a valid entry plus a hostname check, or a successful
  `Verify`.
- **Still checked on every handshake, including hits:** key possession (the TLS 1.3
  CertificateVerify signature with the leaf key, or in TLS 1.2 the ServerKeyExchange signature or
  RSA key transport); the Finished MACs; the negotiated version, cipher suite and ALPN; the
  hostname; the time window. Resumed sessions call `VerifyConnection` too (uTLS
  handshake_client_tls13.go:769–772), so resumption gets the same checks.
- **Kill switch:** `connect.SetTlsVerifiedChainCacheEnabled(false)`, plus the env
  `URNETWORK_CONNECT_TLS_CHAIN_CACHE=off`, read once.
- **Stats:** `connect.TlsVerifiedChainCacheStats()` reports hit, miss, store and evict counts;
  server services export them as CounterFuncs.

**Chain cache or session resumption.** I recommend the chain cache.
- Resumption skips verification only when the next dial reaches the nginx that issued the ticket.
  The platform name fronts 4 edges × 2 lb blocks, which do not share ticket keys.
- connect deliberately keeps one ticket cache per dial path (net_tls.go:35–55), so tickets never
  link a user's egress paths. The probe's per-tunnel strategies leave even that cache empty.
- Making resumption work needs per-edge ticket keying or shared nginx ticket keys. Either adds a
  dependency on server state and, for apps, a privacy trade-off.
- The chain cache needs only the client. It is deterministic, invisible on the wire (the same
  hello, the same tickets), and works across edges and strategies.
- Per-edge ticket keying (Part B #3 option 2) stays an optional later step for the proxy, where
  it would also save handshakes on nginx.
- Serving a shorter chain from nginx would cut every client's cost. That is a TLS-operations
  choice that trades away compatibility with clients lacking X2.

**Probe correctness**
- **The probe's exit checks are out of scope.** Target fetches through the provider dial with
  `tls.Client(raw, PinnedTlsConfigForHost(...))` (providertunnel/tunnel.go:1252–1257,
  pinning.go:199): stock crypto/tls, system roots, and an SPKI pin check on `VerifiedChains`.
  - That config has `RootCAs` nil and a `VerifyPeerCertificate`, so it is ineligible on two counts,
    and it is not a connect handshaker at all.
  - `isTlsAuthenticationFailure` (egresshealth.go:1230–1250) sees the same errors.
    `tls_authentication_failed`, pin mismatches and every other outcome come out exactly as today.
- **The cached path is the probe's control plane**: taskworker to the api and connect hosts
  directly, not through the provider. Interception there still fails on a hit:
  1. The interceptor's own chain (self-signed, private or corporate CA): different bytes, a miss,
     `UnknownAuthorityError`.
  2. Another public CA's certificate for our name: a miss, then no chain to the pinned X1 or X2.
     The pin set is Let's Encrypt only, stricter than system roots.
  3. The genuine chain replayed without its private key: a hit, but key possession fails and the
     handshake aborts. That is today's outcome too, since the genuine chain verifies today.
  4. A genuine chain for another name: the name is part of the key, so a miss and a
     `HostnameError`. On a hit the hostname is checked anyway.
  5. An expired or not-yet-valid chain (a stale certificate, clock skew): the window check makes it
     a miss, and full verification returns `CertificateInvalidError`.
  6. A changed root set (the extra-roots env): a different identity, so a miss.
  7. A mis-issued Let's Encrypt certificate with its key: accepted today and with the cache. The
     cache does not change trust, and Go does not check revocation either way.
- **Extender and VLESS inner TLS behave the same way**, so a third-party relay cannot use the cache
  to intercept.

**Correctness under the owner rule**
- No timeout, deadline, keepalive or probe bound changes. The cache runs inside the handshake,
  under its existing deadline, and only cuts CPU work.
- Verification fails closed by construction (the mismatch rules above).
- No probe outcome changes (Probe correctness above).

**Compatibility and rollout**
- connect only. Server services take it by pinning the new connect: the taskworker for the probe,
  the proxy for hosted devices, and api and connect for their few outbound dials. Apps and
  providers get it in the next SDK release (Q4).
- There is no wire or server change, so mixed versions are independent.
- **Order:** the taskworker g1 block first, compared with g2; then one proxy block; then the rest.
  The SDK goes last.
- **Rollback:** the env switch, or the previous connect.

**Tests** (in connect, failing first; the test PKI uses a P-384 intermediate shaped like YE1)
1. `TestChromeTlsChainCacheSecondDialSkipsVerify`: two dials run one `Verify`, counted through a
   seam. Catches the cache not being used.
2. `TestChromeTlsChainCacheImpostorWithGenuineChain`: a server presenting the cached chain with a
   different private key fails on a hit, over TLS 1.3 and TLS 1.2 ECDHE. Catches any hit path
   that skips key possession.
3. `TestChromeTlsChainCacheRejectsUntrusted`: self-signed and wrong-CA chains fail with
   `*tls.CertificateVerificationError`, both cold and after a valid chain is cached. Catches a
   branch that accepts without checking.
4. `TestChromeTlsChainCacheHostnameAlwaysChecked`: a forced hit with a mismatched name fails.
5. `TestChromeTlsChainCacheExpiresOnHit`: with the config clock past NotAfter, or before
   NotBefore, the dial misses and fails with `CertificateInvalidError`; the same on a resumed
   session.
6. `TestChromeTlsChainCacheRootIdentity`, `…IneligibleConfigs` (extender, custom verifiers, system
   roots and the Go hello unchanged), `…VerifiedChainsExposed`, and `…BoundedAndTtl`.
7. In server, `TestProviderTunnelTargetTlsNotCached`: two target handshakes through the tunnel's
   `DialTLSContext` both verify. The existing egresshealth TLS-failure tests pass unchanged.

- **Benchmark:** handshake CPU with a chain shaped like the YE chain, hit against miss.
- **Production checks:**
  - taskworker `crypto/x509` share under 1%;
  - probe `urnetwork_egress_probe_attempts_total{result}` and the TLS-authentication-failure
    counter unchanged between the A/B blocks;
  - proxy `Verify` share under 0.5%, and no change in carrier handshake failures.

**Savings and effort**
- taskworker: 2.3–2.5 cores, against the 15.3-core fleet capture (about 12 cores on the 1 h
  average).
- proxy: 0.5–0.6 cores (Part B #3). Apps and providers: battery and dial latency.
- About 3 engineering days: one connect commit, then taskworker and proxy releases; the SDK
  later.

**Risks**
- Once `InsecureSkipVerify` is set, verification depends entirely on our callback, so a bug could
  accept anything. The single fail-closed structure and tests 2–3 guard against it.
- A uTLS upgrade could change the callback order; the tests pin it.

#### Questions for the owner
1. **Cache expired client tokens that are still tolerated while `reject_expired` is off?** That
   is 1,463/s, about 14% of api parses.
   - A. No: the TTL never goes past exp. The api saving drops to 1.9–2.5 cores.
   - **B (recommended).** Yes, for the cap only while the gate is off, and bounded by exp once it
     is on. Expiry is still checked on every request, so nothing accepted changes.
   - C. Flip `reject_expired` first. That is a product decision with user impact, outside D4.

   What changes: the TTL rule in the cache's insert.
2. **How long may a verified JWT signature be reused?** A. 5 min. **B (recommended). 10 min.**
   C. 30 min. What changes: the working set (memory), and how long a cached result can outlive an
   in-process key removal (Q3). No per-request bound moves.
3. **Respond to a JWT key compromise without a restart?**
   - **A (recommended).** Keep today's response: remove the key from the vault and restart the
     verifiers, which also empties both caches.
   - B. Add a `revoked_kids` list in jwt.yml, read by a periodic keyring reread (for example every
     60 s), which drops the key and flushes the cache. About +1 day, reviewed separately.

   What changes: whether anything bumps the removal generation now.
4. **When do apps and providers get the chain cache?**
   - **A (recommended).** On by default in the SDK release after the server services have run it
     for a week.
   - B. Opt-in for apps until a longer soak.
   - C. Server services only.

   What changes: the default of the enable flag in the SDK.
5. **Include H3/QUIC dials in the first version?**
   - **A (recommended).** Later. They verify through crypto/tls inside quic-go, where
     `VerifiedChains` cannot be re-attached, and the measured CPU is on H1 dials.
   - B. Now, with a quic-go/simnet test; about +1 day.

   What changes: which configs are eligible.

### D5. Keep long snapshots off the primary

**Problem and evidence** (CPUOPT3 Part D §2.7, §2.8, §2.10, #1, #8; read-only checks on 10-31)

- **A long snapshot inflates every OLTP read.** While a transaction holds the
  xmin horizon, dead tuples in the churn tables (transfer_debit_journal,
  pending_task, provider_egress_probe_cycle) cannot be removed.
  - **10-29 pg_dump:** buffer hits rose from 3.4M to 33M/s with commits flat,
    and edge-2 went from 37 to 92 cores. The excess was 455 core-h in 13.2 h,
    and the run was stopped about 5 h short under the owner's CPU rule.
  - **10-30 census:** two attempts of up to 3 h cost about 100 core-h.
- **The cost grows with pin length.** Excess core-h from the 10-29 curve:

  | Pin | 2 h | 4 h | 6 h | 8 h | 13 h | 18 h |
  |---|---|---|---|---|---|---|
  | Excess core-h | 20 | 65 | 120 | 190 | 420 | ~700 (extrapolated) |

  The pin also bloats: the database grew 227 GB in those 13.2 h against
  about 163 GB normally.
- **Wraparound.** transfer_contract sets the cluster's freeze age: 329M now,
  and 450M on 10-30 04:21Z.
  - While a snapshot is pinned, each completing vacuum can reset the age only
    to (hours pinned × 18–28M xids/h) + 50M.
  - That fits 10-29: 197M at 12:51Z against 191M predicted, and 450M on
    10-30 04:21Z against 455M.
- **The next pin is about 5 h away.**
  - `bringyour-backup.timer` is active with `Sun,Thu 00:00` and
    `Persistent=true`; its next run is Sun 11-01 00:00Z.
  - The owner decided on 10-31 to keep this schedule and the timer.
  - Each run stages 1.04–1.46 TB on edge-2's root (5.73 TB free). Under
    owner decision 2, a generation is deleted once planetoid verifies it.
- **Other long holders** (pg_stat_statements maximum runtime since 10-23):
  - **Retention cascade:** 41 calls, mean 447 s, maximum 929 s, about every
    4.7 h (model/subscription_model.go:6376; 50,000-row batches set at :6111).
  - **Payout attribution check:** 1,578 s at most.
  - **Operator tools:** for example, the 10-27 sn-reconcile run left a 2 h
    orphan query.
- **No 15-minute bound applies to application transactions.**
  - The application role `bringyour` is a superuser, and the A6 migration
    (db_transaction_timeout.go) skips superusers.
  - So `pg_db_role_setting` holds no transaction_timeout, and
    `statement_timeout` is 0.

**Proposed design**

A consistent logical dump needs one MVCC snapshot for its whole read, so on
the instance it reads it always pins the horizon. Removing the pin means
reading another instance (Part 1). Everything else only shortens the pin
(Part 2), and since the cost is superlinear, a 4× shorter pin costs about 10×
less.

**Options**, costed for edge-2 per dump (DB 9.2 TB, heap about 4.4 TB, WAL
41 MB/s average and 68 MB/s at peak):

| Option | Pin | edge-2 CPU | edge-2 IO and network | Disk | Needs |
|---|---|---|---|---|---|
| Today: plain dump on the primary | 18–20 h | ~600–700 core-h, plus ~4 cores of pipeline | heap read at ~66 MB/s | 1.0–1.5 TB staged on edge-2, plus ~0.08 TB bloat | nothing new |
| (b) `-Fd -j4` on the primary (one synchronized snapshot) | ~4.5 h (transfer_contract's 835 GB heap is read by one worker) | ~65–80 core-h | +265 MB/s; sda is about 50% busy now | same | a format change |
| (a1) fresh base backup, restored and dumped elsewhere | 0 | ~13 core-h | 9.2 TB read per cycle (12.8 h at 200 MB/s) | at least 12 TB on the restore host | a host with space; 25–30 h per cycle |
| (a2) streaming hot standby, dump there | 0 | ~0.2–0.3 cores continuously (walsender) | WAL only: 0.33–0.55 Gb/s on 10 GbE | standby: 9.2 TB, plus 2 TB WAL buffer, plus 1.5 TB staging | hardware |

**Hosts with space (measured 10-31): none can take a 9.2 TB copy.**
- **edge-5:** MinIO's 8 × 3.58 TB drives, each held free by MinIO and
  base-backup reserves; no single filesystem big enough; load average 100+.
- **edge-6:** 128 cores and 345 GB of free RAM, but only 1.3 TB of disk.
- **snow:** a 3.1 TB pool, already running subtensor and the validator.
- **planetoid:** 14 GB RAM, 2.5 TB on USB, remote over VPN.

So (a) needs hardware; (a2) is preferred over (a1).

**Part 1: a streaming standby, with the logical dump moved to it** (xops and
provisioning; no application code)

1. **Standby host** (new xops group `pg-standby`, `playbook-pg-standby.yml`
   modeled on playbook-pg-basebackup.yml's guards):
   - PostgreSQL 18.6;
   - `hot_standby = on`, `hot_standby_feedback = off` (the primary's horizon
     is never held), `max_standby_streaming_delay` left at 30 s;
   - `primary_conninfo` as a new REPLICATION role `standby` over hostssl on
     the LAN, `primary_slot_name = standby_1`;
   - sized at about 20 TB usable SSD (the primary's 15.1 TB, plus WAL buffer
     and two generations), 256 GB+ RAM, 32+ cores, 10 GbE.
2. **On the primary** (owner-applied):
   - one `hostssl replication` line in pg_hba.conf;
   - the role;
   - `max_slot_wal_keep_size` 32 → 256 GB. Today's 32 GB is only 8–13 min of
     WAL; 256 GB is 1–1.7 h of receive outage before a re-seed.
     `pgbackup-slot-guard.sh` is extended to `standby_*` slots and still drops
     them below 200 GB free (`pg_basebackup_guard_min_free_bytes`).
3. **Seed:** `pg_basebackup -X stream -R -S standby_1 --max-rate=200M`, once.
   That is 12.8 h of +200 MB/s reads at about 1 core, with no snapshot,
   started off-peak (about 06:00Z).
4. **Dump job on the standby:** the same `bringyour-backup.timer`
   (`Sun,Thu 00:00`, Persistent), with a wrapper around bringyour-backup.sh.
   - **Pre-checks:** `pg_stat_wal_receiver.status = 'streaming'`, replay lag
     under 5 min, and free disk at least the expected WAL buffer plus 25%.
   - `pg_wal_replay_pause()`, then wait for
     `pg_get_wal_replay_pause_state() = 'paused'`.
   - The unchanged pipeline (bringyour-backup.sh:60: pg_dump → zstd → md5 →
     gpg → final/).
   - `pg_wal_replay_resume()` in an EXIT trap and in `ExecStopPost=`. A
     reboot resumes replay by itself.
   - **Bounds:** `RuntimeMaxSec=16h` on the unit (kill, resume, publish
     nothing), and an abort when free disk drops below 1 TB.
   - **During the pause** the standby buffers 0.9–2.0 TB of WAL for a 6–8 h
     plain dump, or 0.3–0.7 TB for 2–3 h with `-j`. The walreceiver keeps
     acknowledging flushes, so the primary's slot keeps advancing.
   - **Catch-up** takes about 2.5–3.5 h at an assumed 150 MB/s replay
     *(inferred)*.
5. **Planetoid pulls from the standby host**
   (host_files/planetoid/backup/remote-backup-archive.sh). The other agent's
   verify-then-delete applies there unchanged.
6. **Cut-over:** edge-2's job is retired after two verified standby
   generations and one test restore, if Q1 allows.

**Unchanged:** the schedule, format, gpg passphrase (vault backup.yml),
planetoid rotation and edge-5's physical base backups.

**Variant (a3):** the standby on ZFS.
- Each dump takes a snapshot, starts a throw-away instance on a clone on
  another port, dumps it and destroys it.
- The standby never pauses, so it stays current for failover and reads.
- The clone needs room for 1–3 TB of rewrites *(inferred)*.

**Part 2: until a standby exists** (xops; the timer stays)
- **A runtime bound** of `RuntimeMaxSec=30h` on `bringyour-backup.service`,
  which has none today. A Thursday dump is then dead by Fri 06:00Z, before
  the 18:25Z epoch close.
- **An abort watcher** (Q4) that stops pg_dump and publishes nothing when:
  - transfer_contract's `age(relfrozenxid)` exceeds 1.0B;
  - edge-2's root has less than 1.5 TB free;
  - optionally, edge-2 has been busy above an owner-set level for 30 min.
- **Optionally `-Fd -j4`** (Q3):
  - the pin falls from 18–20 h to about 4.5 h;
  - CPU from about 700 to about 70 core-h;
  - freeze-age growth from +0.35–0.56B to about +0.1B.

**Variants that do not avoid the pin:**
- a time-boxed abort (if it fires, there is no backup);
- per-table snapshots (inconsistent across tables; transfer_contract alone
  pins for about 3.5 h);
- `--exclude-table-data` (a product call, and it only shortens the pin);
- "incremental" logical (logical decoding needs `wal_level=logical`, a
  restart, plus an initial copy; PostgreSQL 17+ incremental backups are
  physical);
- an LVM snapshot on edge-2 (`ubuntu-vg` has 0 GB free, checked 10-31, and
  copy-on-write would amplify every primary write).

**Part 3: the SN census.** It runs at the epoch boundary, which chain time
fixes (11-06, about 18:25Z). Its root is due about 4 h later (block 9450028,
about 22:25Z). So it cannot move off-peak; edge-2 runs 42–44 cores then.

- **Keep the deployed bitmap read** (6a523f17d, 7720f0deb): 20–65 min,
  about 10–20 core-h and about 300 GB read. Its 2 h step MaxTime and its
  REPEATABLE READ snapshot are unchanged.
- **No other long reader in the window:** Part 2's 30 h dump bound, and no
  heavy maintenance from 18:00Z to 23:00Z on close days, including the
  Phase C sweep on 11-06.
- **Later (Q5): read the standby behind a freshness gate.**
  - Take `pg_current_wal_lsn()` on the primary after the boundary, then wait
    within the same MaxTime until the standby's `pg_last_wal_replay_lsn()`
    reaches it. Dumps never overlap closes.
  - This needs a new direct `ReplicaMaintenanceDb` pool. `ReplicaDb`
    (db.go:703) still uses the primary pool.

**Part 4: the retention cascade in short transactions** (server; ships on
its own)

- **Why each batch runs long:**
  - `RemoveCompletedContracts` passes `maxRowCount := 50000`
    (subscription_model.go:6111) to every pass.
  - Each `removeDueContractBatches` batch (:6272) is one maintenance
    transaction: lock, classify, the cascade at :6376, then the contract
    delete. It runs 447 s on average and 929 s at most.
  - That overruns the 5-min `reaperRunBudget` (:59), which is checked only
    between batches.
  - The batch holds an xid throughout, and a running xid holds the horizon in
    READ COMMITTED too. So splitting statements inside the transaction would
    not help.
- **Change:** unexported, test-settable vars `reaperDeleteBatchRows = 2000`
  (an estimated ~18 s per transaction) and `reaperAssignBatchRows = 10000`
  (for `assignCompletedContractReapTimeBatches`, :6171: 21–47 s today,
  about 4–9 s after). :6111 passes these instead of 50k.
- **Unchanged:** the per-batch protocol (FOR UPDATE, protected rows set back
  to NULL, cascade, delete), the 5-min budget (now enforced within about
  20 s), the 30-min cadence and MaxTime, `force_custom_plan` and
  `OptNoRetry`.
- **Throughput** is about the same: roughly 32k contracts per 5-min run,
  against 50k per 7–15 min.

**Part 5: monitoring**
- **Horizon:**
  - confirm that signal §2.4l (monitor/signal_transaction_horizon.go; warns
    at 15 min, pages at 1 h) is enrolled;
  - add a postgres_exporter series for the oldest backend_xmin and xid age by
    backend type.
- **Wraparound:** `pg_database_wraparound_age_datfrozenxid_seconds` (an xid
  age): warn at 600M, page at 1.0B. The failsafe at 1.6B leaves 21–30 h of
  margin.
- **Pin signature:** blks_hit/s or tup_returned/s more than 3× the same hour
  the previous day, sustained for 30 min.
- **Standby:** receiver status, replay lag in bytes and seconds, the
  primary's `pg_replication_slots.wal_status` and `safe_wal_size`, the
  standby's pg_wal size and free disk, pause state, and dump duration and
  result.
- **Retention:** a per-batch duration histogram (bounded series, V(1) logs
  only).

**Rejected:**
- **(a1):** a 9.2 TB read of edge-2 per dump, on the same hardware as (a2).
- **Logical replication:** needs a restart, and DDL is not replicated.
- **`max_standby_streaming_delay = -1`:** it would drop a bound. The
  explicit pause plus `RuntimeMaxSec` covers it.
- **A chunked census read:** it would change the snapshot semantics of
  payout roots.

**Correctness under the owner rule**

- **Unchanged bounds:** `wal_sender_timeout` and `wal_receiver_timeout`
  (60 s), `idle_in_transaction_session_timeout` (60 s),
  `max_standby_streaming_delay` (30 s), the census step's MaxTime and root
  deadline, `reaperRunBudget` and the retention MaxTime, PgBouncer's
  timeouts, and `vacuum_failsafe_age`.
- **New bounds, all tighter:** the dump's `RuntimeMaxSec` (16 h standby,
  30 h primary), the abort thresholds, and retention transactions of about
  20 s instead of up to 929 s.
- **`max_slot_wal_keep_size`** is a disk cap, not a timing bound, and the
  200 GB free-space guard stays.
- **Failure modes:**
  - **Dump crashes while replay is paused:** the trap or `ExecStopPost`
    resumes replay; a reboot resumes it too.
  - **Standby disk runs short:** the job aborts and resumes replay.
  - **Standby out of contact past the slot cap:** the slot is invalidated,
    the pre-check refuses to dump stale data, and the standby is re-seeded
    (with an alert).
  - **A failed retention batch** rolls back alone, as today.
- **Dump consistency:** a dump from the standby is consistent as of its
  replay point, at most 5 min old. That is the same guarantee as a dump
  started on the primary at that moment.

**Compatibility and rollout**

- **Components:** xops (standby playbook, pg_hba, slot guard, wrapper,
  planetoid pull, `RuntimeMaxSec`, watcher) and server (retention vars;
  later, the replica session). No connect, sdk, app or proxy changes.
- **Mixed versions:** old and new taskworkers each run whole batches, and the
  RunOnce key serializes runs.
- **Order:**
  1. Part 4 in the next taskworker release; reverting one var rolls it back.
  2. Part 2's bound and watcher, before Sun 11-01 if the owner wants it.
  3. Hardware, then seed, then a week watching lag and slots, then standby
     dumps (edge-2's timer moves per Q1), then retire edge-2's job.
  4. Optionally, the census on the standby.
- **Rollback:** re-enable edge-2's timer, kept installed until retirement.

**Tests**

- **Server, failing first:**
  - `TestRemoveDueContractBatchesBoundsRowsPerTransaction`: 5,000 due rows
    with `reaperDeleteBatchRows = 2000` give 3 transactions of at most 2,000
    each, counted around MaintenanceTx. Catches passing the shared 50k back.
  - `TestRemoveCompletedContractsUsesSeparateAssignAndDeleteBatches`: at most
    10k rows per assign transaction and 2k per delete transaction. Catches a
    single shared constant.
  - `TestRemoveDueContractBatchesBudgetStopsWithinOneBatch`: with an injected
    budget, the run stops after the first batch that crosses it.
  - Equivalence: the protected, cleared and deleted sets match the
    single-batch run on the same fixture.
  - A benchmark of per-batch time.
- **xops pytest** (as in tests/test_postgresql_config.py):
  - the wrapper pauses before pg_dump and resumes in the trap and in
    `ExecStopPost`;
  - it refuses when lag exceeds 5 min or the receiver is not streaming;
  - `RuntimeMaxSec` is set on both units;
  - the slot guard covers `standby_*`.
- **Production:** during a standby dump, edge-2's busy CPU and blks_hit/s
  stay within ±10% of the same hours a week earlier, no pg_dump backend
  appears on edge-2, and the freeze-age sawtooth is unchanged. Replay lag is
  under 5 min within 4 h of the dump. One restore test a month. Retention
  batch p99 under 30 s.

**Savings and effort** (base: edge-2 at 37–41 cores, 10-29 to 10-31)

- **Dump on the standby:**
  - removes about 600–700 core-h per complete dump, about 7–8 cores averaged
    over a week of two dumps, plus the +50-core peaks;
  - removes +0.35–0.56B of freeze age per dump;
  - moves 1.0–1.5 TB of staging and about 0.08 TB of bloat per dump off
    edge-2. On the disk projection, root reaches 0 on 11-21 rather than
    11-15 for B1.
- **Interim `-j4`:** about 90% of that, at once.
- **Retention:** small CPU, and no more 7–15 min pins every ~4.7 h.
- **Effort:**
  - Part 4: 1 day;
  - Part 2: 0.5–1 day;
  - Part 1: hardware lead time, plus 4–6 days, plus a 13 h seed and a week of
    soak;
  - census on the standby: 2 days.
- **Releases:** taskworker for Part 4, and xops plays.

**Risks**
- Until the standby exists, each Sun/Thu dump can cost up to 700 core-h
  unless Part 2 is adopted. The 11-01 run starts in about 5 h.
- A receive outage of more than about 1.7 h forces a 13 h re-seed.
- With (a2), the standby is up to 16 h stale twice a week; (a3) avoids that.
- A second full copy of the data needs edge-2's encryption-at-rest posture
  (LUKS).
- The 91-day contract backlog (the disk design) needs a longer retention duty
  cycle. Short transactions are what make that safe.

**Questions for the owner**

1. **Where do Sun/Thu logical dumps run once a standby exists?**
   - Options: (a) move the timer to the standby; (b) the standby, with
     edge-2 as an automatic fallback; (c) keep edge-2.
   - Recommended: **(a)**, with a manual fallback to edge-2.
   - Changes: whether edge-2's job is retired.
2. **Standby hardware and mode.**
   - Hardware options: (a) 6 × 7.68 TB SSDs in edge-6, beside Redis; (b) one
     of edge-7/8, moved off the services plan; (c) a dedicated host matching
     edge-2, which can also take over on failover.
   - Mode options: pause replay during dumps (a2), or ZFS clones (a3).
   - Recommended: **(c) with (a3).** Today there is no replica at all.
   - Changes: Part 1's sizing, the clone step and the slot cap.
3. **Dump format.**
   - Options: (a) keep the plain single `.sql.zst.gpg`; (b) directory format
     with `-j` (pg_restore -j and per-table restore; planetoid scripts
     change).
   - Recommended: **(b), adopted now.** On the primary it cuts the interim
     pin from 18–20 h to about 4.5 h.
   - Changes: the wrapper, planetoid verification and the restore runbook.
4. **When must the on-primary dump stop** (until the standby exists)?
   - Options: (a) never; (b) on a CPU level for 30 min, freeze age 1.0B or
     root under 1.5 TB; (c) age and disk only.
   - Recommended: **(c) plus `RuntimeMaxSec=30h`.** For (b), give the CPU
     threshold.
   - Changes: the Part 2 watcher.
5. **Census source after the standby.**
   - Options: (a) the primary, about 10–20 core-h a week; (b) the standby
     behind the replay-LSN gate.
   - Recommended: **(a)** for 2–3 clean epochs, then (b).
   - Changes: Part 3 and the replica session.
6. **The application role is a superuser,** so A6's 15-minute bound is off.
   - Options: (a) leave it; (b) make `bringyour` non-superuser; (c) a separate
     non-superuser role for the pooled application.
   - Recommended: **(c).** It is a security-posture change with its own
     review.
   - Changes: the role setup, and A6 taking effect.

## Part A — Services edges and connect

Domain: by-us-fmt-5-edge-0/1/3/4 (312 logical CPUs) and the connect service.
Everything here was measured read-only on 10-31 between 18:25Z and 19:35Z, or
read from Mimir. Code is cited at server origin/main `acc078f5d` unless it says
"running", which means the deployed connect source (server `37ec4788b`, connect
library `ea79e1f1`).

### Summary

**Where the CPU goes now** (busy 176–178 of 312 logical CPUs at the
measurement; that was also the 10-31 daily peak):
- **By process:** connect is 56–59% of busy CPU, nginx about 15%, api about
  10%, taskworker about 7%, and logging and observability 4–5%.
- **Kernel time:** about 40% of busy time (system plus softirq).
- **Inside connect,** one g-block process:
  - Go timer heap scans, `(*timers).adjust`: 7.2–8.0% self (was 10–13%).
  - Socket syscalls and the kernel work done inline: 30% inclusive.
  - Redis client calls: 6.6–9.6%. CPUOPT2 reported under 0.25%, but its
    pattern match missed go-redis symbols (§7).
  - The liveness-lease writer: 4.0% (was 2.8%).
  - Exchange socket trees: 27%. H1 client transports: 15.5%.

**What the shipped changes did** (10-29 20:00Z against 10-31 18:27Z):
- Busy CPU per 1k clients −18% (1.02 → 0.84).
- Connect cores per 1k clients −16% (0.586 → 0.492).
- Exchange connection ends per client −51% (9.4 → 4.6). Goroutines per client
  −40% (108 → 66). Connect heap −31% (889 → 612 GB).
- Exchange keepalive probes went from 170k/s to 0.7–1.0k/s per host, even with
  keepalive restored.

**What restoring keepalive and full deadlines cost:**
- About 1.5% of connect CPU, roughly 1.5 cores fleet-wide.
  `exchangeDeadline.refresh` alone is 1.6% inclusive.
- Same-hour comparison with the lazy release: +1.9% connect cores per 1k
  clients and +3.2% per exchange frame. That is within day-to-day noise.
- They add no timer-heap scans: deadline pushes move timers *later*, which Go
  1.27 does not rescan for.

**Two new findings:**
- **What drives the remaining heap scans.** bpftrace shows 223–259 full scans/s
  per process, each over about 32k timers per P. That is all of `adjust`, and
  none are forced by zombies. The scans are triggered by three libraries
  re-arming short timers that are still in the heap, not by our deadlines:
  - pgx's 15 ms slow-write timer, reset on every query: 59–64% of short
    triggers;
  - quic-go's connection timer, reset on every loop: 24–28%;
  - go-redis socket deadlines taken from short contexts: 10–11%.
- **About 91% of transport exchange connections loop back into the same
  process.** On edge-3, 43% of all outbound exchange connections dial their own
  process. If the other hosts match edge-3, that is about 200k TCP loops
  fleet-wide, carrying an estimated 60% of exchange frames.

**Contract-rejection growth is api CPU, not connect CPU:**
- Control HTTP grows from 3.3k to 10.4k requests/s over one connect generation,
  and costs about 1.1 api cores per 1k requests/s.
- After CPU is fitted to clients and frames, connect's residual drifts
  +0.02 cores/h with uptime, which is no growth.
- The service-runaway pages are structural. Every g-process runs at 760k–990k
  goroutines, against the probe's 500k hard ceiling, right from restart.

**Ranked opportunities.** Savings are services-edge cores at today's peak (177
busy). They overlap, so do not add them up.

| # | Change | Est. saving | Effort | Risk |
|---|---|---|---|---|
| 1 | In-process delivery when a transport's resident is in the same process (§6.1) | 12–15 cores (7–8%), −400k sockets, −200k conntrack entries, −2M goroutines | 2–4 weeks | medium |
| 2 | Stop short-timer re-arms from triggering heap scans: the pgx slow-write timer and the quic-go connection timer (§6.2) | 3–6 cores (2–3.5%) | days | low |
| 3 | Index the liveness-lease writer per shard, with a dirty list (§6.3) | ~3 cores (1.7%) | days | low |
| 4 | Key-event merge overflow: stop resyncing every close listener about once a minute per process; batch the takes (§6.4) | 2–3 cores (1–2%), plus Redis load | days | low–medium |
| 5 | Stop the `/connect/control` rejection creep at its source (§6.5, api/SDK) | 0 → 6–8 cores over a generation (≤4%) | 1–2 weeks | low–medium |
| 6 | H1 terminates in connect, with no nginx hop (§6.6) | 8–12 cores (5–7%) | 1–2 months | high |
| 7 | Multiplexed links per process pair for the remaining cross-process exchange (§6.7) | 5–8 cores (3–4.5%) after #1 | 1–2 months | high |
| 8 | Size channel rings on demand; cap forward-ingress shards (§6.8) | ~1 core, plus 5–8 GB heap per process | days | low |
| 9 | Move Mimir/Loki/Grafana off the services edges; use docker `local` logging (§6.9) | 2–4 cores (1–2%) | days | low |
| 10 | Small items: hex `encodeUuid`, AppArmor, GOMAXPROCS and THP experiments (§6.10) | ≤1–2 cores each | hours–days | low |

### 1. Method

- **When and at what load:**
  - 10-31, 18:25–19:35Z, a Saturday.
  - Services edges at 176–178 busy logical CPUs (56–57%), 197–210k
    connected clients.
  - Connect 97–104 cores, on generation 1085953670, which started 02:01–02:44Z
    and was 16–17 h old.
- **Periods compared** (from process start times and host uptimes):

  | Period | Window | What ran |
  |---|---|---|
  | P0 | before 10-30 02:26Z | what CPUOPT2 profiled (10-29 19:57Z): 1 s/1 s×15 exchange keepalive, 15 min forward tail, eager deadlines and ping resets |
  | P1 | 10-30 03:52Z → 10-31 02:01Z | connect 1085173010: keepalive off, 3 min forward tail, lazy deadlines, ping pacing, timer reuse. Plus `retbleed=stuff iommu=pt` (rolling reboots 10-30 03:16–03:52Z) |
  | P2 | 10-31 02:44Z → now | 1085953670 = P1 + exchange keepalive at 11 s idle, 1 s × 4, + full native per-operation deadlines |

  - The CPUOPT2 status block gives only times. Host uptimes (39 h at 18:40Z on
    10-31) and the forward drop at 10-30 03:00Z place the 1085173010 rollout
    and the kernel reboots on 10-30.
  - The kernel change is confounded with the P1 release.
- **Mimir:** CPUOPT2's window (10-22 → 10-29 20Z), the last 7 days, hourly
  series from 10-28, and fixed instants. There are no exporters for non-Go
  processes, so host-only processes come from `top`.
- **Host census, all four edges** (`host_census.sh`), counts only:
  - `top` (5 s) summed by command;
  - per-CPU `/proc/stat` deltas;
  - `nstat` over 10 s;
  - an `ss -tnpo` census by owning process, peer locality and TCP timer;
  - conntrack, sockstat, kernel command line, IOMMU domains.
- **Exact exchange locality on edge-3** (`census_dnat.sh`): outbound connect
  sockets to local peers, matched through the iptables DNAT map
  (external → internal port) to the owning listener PID.
- **perf on edge-3,** the CPUOPT2 reference host:
  - one connect g-block process (g4), three captures of 40 s at 199 Hz with
    frame-pointer call graphs: 54–63k samples at 6.9–7.3 cores;
  - the whole host, 15 s at 49 Hz with call graphs.
  - Data was summarized on the host and deleted. Classification was memoized
    per symbol (`perf_conn2.sh`, `perf_conn3.sh`, `perf_host.sh`).
- **bpftrace, entry-only uprobes** (no uretprobes, which are unsafe on Go), on
  edge-3 g4 and edge-1 g2:
  - `runtime.(*timers).adjust` for 5 s: heap length, zombies,
    `minWhenModified`;
  - `runtime.(*timer).modify` for 1 s: delta to the new `when`, whether the
    timer was heaped and lowered its P's `minWhenModified`, and sampled stacks.
  - Struct offsets were read from Go 1.27.3 `runtime/time.go`, the toolchain
    of the running binary.
  - Afterwards: no uprobes registered, no leftover files or processes on any
    edge.
- **Caveats:**
  - P1 and P2 are compared day over day (Friday against Saturday).
  - The 40 s captures caught no GC cycle; GC runs every 2–2.5 min per process.
  - All process profiles are of the hottest g-block. `edge-1 g2` was used only
    for the uprobe counts.

### 2. Where the CPU goes now

#### 2.1 Hosts (CPUOPT2 §2.1)

Busy as % of capacity:

| Host | CPUs | 10-26 02:25Z | 10-29 20:00Z (P0) | 10-30 18:00Z (P1) | 10-31 18:27Z (P2) | Kernel share of busy, P2 |
|---|---|---|---|---|---|---|
| edge-0 | 96 | 74.8% | 47.4% | 32.4% | 42.6% | 39.6% |
| edge-1 | 72 | 82.5% | 71.5% | 54.0% | 65.3% | 41.4% |
| edge-3 | 72 | 85.7% | 68.6% | 65.3% | 62.7% | 39.3% |
| edge-4 | 72 | 84.9% | 67.0% | 57.2% | 61.8% | 40.0% |
| **All** | 312 | 81.4% (254.0) | 62.4% (194.7) | 50.7% (158.2) | 56.9% (177.5) | 40.1% |

- **Mode split now** (% of capacity): user 34.0, system 14.5, softirq 8.3,
  irq 0.
  - 10-26 peak: 44.6 / 22.4 / 14.4, with kernel 45% of busy.
  - 10-29 20Z: 36.9 / 14.1 / 11.4, with kernel 41% of busy.
- **Daily peaks,** busy logical CPUs from 5-min samples:

  | 10-22 | 10-23 | 10-24 | 10-25 | 10-26 | 10-27 | 10-28 | 10-29 | 10-30 | 10-31 |
  |---|---|---|---|---|---|---|---|---|---|
  | 211 | 213 | 193 | 267 | 254 | 240 | 226 | 207 | 174 | 178 (to 18:30Z) |

  Peak-hour connect: 10-25/26 182–188 cores, 10-27/29 115–119, 10-30/31
  99–103.
- **Window averages, busy CPUs:**

  | Window | Busy | User | System | Softirq |
  |---|---|---|---|---|
  | CPUOPT2 window | 157.7 | 91.4 | 39.5 | 26.7 |
  | Last 7 days | 164.7 | 97.2 | 40.3 | 27.2 |
  | P1 | 151.0 | 90.1 | 36.5 | 24.4 |
  | P2 so far | 140.5 | 84.3 | 35.6 | 20.5 |

- **Per-CPU spread over 10 s,** busy median / p90:
  - edge-0 54/63%, edge-1 72/77%, edge-3 68/75%, edge-4 80/84%.
  - Softirq median 2.7–8.7%, p90 12.6–17.6%. CPUOPT2 on edge-3: median 5%,
    p90 20%.
- **edge-0 still runs cool:** 42.6% busy on 96 CPUs. It holds 57.9k clients,
  against 45–57k on the 72-CPU hosts, but CPUOPT2 §3.11's capacity weighting is
  not applied.

#### 2.2 By process (CPUOPT2 §2.2)

`top`, 5 s, 18:41–18:44Z, in cores:

| Host | connect | nginx | api | taskworker | logging and observability |
|---|---|---|---|---|---|
| edge-0 | 26.5 | 5.5 | 2.3 | 1.7 | 1.8: loki 0.7, dockerd 0.4, fluent-bit 0.4, journald 0.3 |
| edge-1 | 26.0 | 6.9 | 3.6 | 4.0 | 3.1: loki 0.7, fluent-bit 0.6, dockerd 0.5, journald 0.5, mimir 0.2, grafana 0.2, containerd-shim 0.2, rsyslog 0.2 |
| edge-3 | 21.3 | 6.9 | 5.9 | 1.6 | 1.9: dockerd 0.7, loki 0.5, journald 0.3, containerd-shim 0.2, rsyslog 0.2 |
| edge-4 | 27.9 | 7.3 | 6.0 | 5.3 | 2.1: dockerd 0.6, loki 0.5, journald 0.3, fluent-bit 0.3, containerd-shim 0.2, rsyslog 0.2 |
| **Sum** | **101.7** | **26.6** | **17.8** | **12.6** | **~8.9** |

edge-3 whole-host perf at 19:04:45Z, as a share of non-idle samples:

| Process | Now | CPUOPT2 |
|---|---|---|
| connect | 55.9% | 53.6% |
| nginx | 17.8% | 15.5% |
| api | 14.4% | 11.2% |
| taskworker | 5.4% | 8.9% |
| dockerd + containerd-shim + docker CLI | 2.9% | 2.1% |
| journald + rsyslog | 1.0% | 1.0% |
| loki + mimir + fluent-bit | 1.3% | 3.9% |

- 12.2% of non-idle samples are IRQ or softirq work on otherwise idle CPUs.
- Mimir process CPU puts connect at 59% of busy (103.5 of 176.1 cores).

#### 2.3 Inside connect (CPUOPT2 §2.3)

edge-3 g4 at 6.9–7.3 cores, captures 2 and 3. All shares are of connect
samples.

| Category | Now | CPUOPT2 | Notes |
|---|---|---|---|
| Timer heap: `(*timers).check` incl. / `adjust` self / `siftDown` | 10.5–11.3 / 7.2–8.0 / 0.86% | 15.3–16.1 / 10–13 / 1.23% | `adjust` via `findRunnable` 3.9%, via `stealWork` 3.1–3.7%. §4 |
| Scheduler: `findRunnable` incl. (with timers), `stealWork`, `netpoll` | 24.5–26.2%, 8.1–9.0%, 3.3–3.6% | 28.7%, 8.3%, 2.9% | idle Ps hunting |
| `selectgo` incl., `lock2`+`unlock2` self | 9.5–10.0%, 2.9% | 8.2–8.6%, 2.4% | top selects: `sendPooledReceive` 2.1, H1 writer 1.2, exchange writer 1.0 |
| Syscalls (`Syscall6` incl., with inline kernel work) | 30.5% | 23–29% | `conn.Write` 15.0, `conn.Read` 10.8 |
| Kernel leaf, all | 44% | — | net 16.2, sched/futex 7.8, entry/exit+mitigation 5.8, netfilter 2.7, mm 2.2, epoll 1.3, AppArmor 0.9, other 7.2 (perf's own 1.4) |
| Exchange socket trees (`ExchangeConnection.Run` + `handleExchangeConnection`) | 13.2 + 13.75% | 25–30% | |
| H1 transports (`ConnectHandler.Connect`) | 15.5% | ~20% | write batch 5.3, pooled read 3.7 |
| Redis client (go-redis, incl.) | 6.6–9.6% | "<0.25%" (missed, §7) | callers in §2.3.1 |
| PostgreSQL (pgx, incl.) | 2.1% | "<0.25%" (missed) | contract close and settlement, `GetProvideKeyChanges` |
| `model.*` incl. | 8.6–11.4% | — | |
| Liveness-lease writer | 3.95% | 2.81% | `write.func2` 1.8 self, `written.func1` 1.3 self |
| `ConnectionAnnounce.run` | 1.7% | — | |
| `AuthorizationLease.runChecks` | 2.3% | — | |
| `residentController` (control frames) | 2.6–3.0% | 2.8% | |
| Connect library (`github.com/urnetwork/connect.*`) | 35% incl. | — | mostly the framer under syscalls |
| Per-op exchange deadlines (`exchangeDeadline.refresh` incl.) | 1.6% | — | new in P2 (§3.3) |
| All deadline installs (`runtime_pollSetDeadline` incl.) | 1.05% | — | exchange 0.64, Redis 0.10, H1 0.25 |
| GC | no cycle in the windows | 4.5% (150 s run) | est. ~3% now: heap −31%, goroutines −40% |
| conntrack (`__nf_conntrack_find_get` self) | 1.1% | 1.5% | |
| AppArmor (`aa_inet_msg_perm`) | 0.5% | 0.4% | |
| `call_depth_return_thunk` (retbleed=stuff) | 1.6% | — | price of stuffing |
| quic-go (H3) | 0.3% | — | but 24–28% of timer-scan triggers (§4) |

##### 2.3.1 Redis callers

From capture 3, as a share of connect CPU:

| Caller | Share |
|---|---|
| `TakeClientCloseRequests` (client-close listener) | 2.53% |
| `ReadContractHoleLease` (packet-path allowance) | 1.22% |
| `session.init.func5.1` | 0.86% |
| `PublishConnectionAuthority` | 0.71% |
| `GetResidentForClient` | 0.63% |
| `GetProvideModes` | 0.36% |
| `RecordClientReliabilityStatsRange` | 0.34% |
| `GetStreamHops` | 0.28% |
| egress verify read and update | 0.34% |
| `AuthorizeStream` | 0.14% |
| unattributed | 1.1% |

##### 2.3.2 Timer-arming callers

This is the edge_perf6 pattern: samples with an arming frame, attributed to the
first application caller (capture 2, % of connect samples).

| Arming kind | Total | Callers |
|---|---|---|
| Deadline installs (`runtime_pollSetDeadline`) | 1.05% | `exchangeDeadline.refresh` 0.64; Redis hook 0.10; H1 `writeConnectH1UserReadyBatch` 0.09, `readConnectH1PooledWithDeadline` 0.06, `Connect.func13.7.2` 0.05; `WebSocketWriteBatchConn.Set*Deadline` 0.08 |
| `(*timer).modify` | 0.57% | `exchangeDeadline.refresh` 0.23; `ForwardSequence.Run` 0.05; H1 writer ping Reset 0.04; `resetOrCreateTimer` 0.04; H1 deadlines 0.06; Redis 0.02; `ReadContractHoleLease` 0.01 |
| `time.(*Timer).Reset/Stop` | 0.27% | `ForwardSequence.Run` 0.06, H1 writer 0.05, `resetOrCreateTimer` 0.04, pgx 0.02, `exchangePinger.arm` 0.01 |
| Channel-timer block/unblock | 0.70% | exchange outbound writer 0.17, H1 writer 0.11, inbound transport writer 0.11, `MultiRouteSelector.readWithCarrier` 0.07, `ForwardSequence` 0.06 |
| `context.WithTimeout/Deadline` | 0.22% | `ReadContractHoleLease` 0.07, `AuthorizationLease.runChecks` 0.05, `RedisAuth` 0.03 |
| `(*timer).maybeAdd` | 0.19% | spread |

- CPUOPT2's biggest armers have mostly gone. Counting timer frames in
  captures of similar size, `runMeasurementPublisher` fell from 197 samples to
  about 26 (0.04%), and keyEvent resyncs and notifications from 183 to about 17
  (0.03%).
- The arming work itself is small, about 2.5%. Timer cost is in the scans that
  short re-arms trigger (§4).

#### 2.4 Work and its cost (CPUOPT2 §2.4)

| | 10-26 02:25Z (1 s pings) | P0 10-29 20:00Z | P1 10-30 18:00Z | P2 10-31 18:27Z |
|---|---|---|---|---|
| Connected clients | 201.8k | 191.0k | 181.0k | 210.3k |
| Busy CPUs / connect cores | 254.0 / 189.2 | 194.7 / 111.9 | 158.2 / 88.2 | 176.1 / 103.5 |
| Busy CPUs per 1k clients | 1.26 | 1.02 | 0.87 | 0.84 |
| Connect cores per 1k clients | 0.94 | 0.59 | 0.49 | 0.49 |
| Exchange frames/s, both directions | 3.57M | 1.76M (pings 287k) | 1.62M (97k) | 1.94M (121k) |
| Connect µs per exchange frame | 53 | 63 | 55 | 53 |
| Exchange ends (forward / transport) | 1.40M (0.99/0.41) | 1.79M (1.40/0.39) | 0.79M (0.42/0.37) | 0.96M (0.54/0.43) |
| Ends per client | 6.9 | 9.4 | 4.4 | 4.6 |
| New exchange connections/s | — | ~1.2k | ~1.2k | ~1.5k |
| Goroutines (per client) | 15.9M (79) | 20.6M (108) | 12.0M (66) | 13.8M (66) |
| Heap / stacks / RSS (GB) | 681 / 127 / 950 | 889 / 163 / 1211 | 559 / 102 / 798 | 612 / 112 / 858 |
| Softirq+irq CPUs per M frames/s | 12.6 | 20.1 | 14.2 | 13.6 |
| NIC rx / tx pkts/s | 2.17M / 2.42M | 1.66M / 2.10M | 1.29M / 1.35M | 1.44M / 1.50M |

- New exchange connections/s is handshake frames ÷ 4. The mean exchange
  connection now lives about 5.5 min.
- **Same-hour P1 against P2,** averaged over 13–18Z: connect cores per 1k
  clients 0.480 → 0.489 (+1.9%), µs per frame 52.2 → 53.9 (+3.2%).
- **Load model per generation,** hourly least squares:
  - P2: connect = 0.18 × clients(k) + 0.037 × frames(k/s) − 5.8;
  - P1: 0.23 × clients(k) + 0.042 × frames(k/s) − 20;
  - residual RMS about 3 cores.

#### 2.5 Kernel and host (CPUOPT2 §2.6)

Per host over 10 s at 18:41–18:44Z:

| | edge-0 | edge-1 | edge-3 | edge-4 |
|---|---|---|---|---|
| TcpExtTCPKeepAlive/s | 950 | 680 | 815 | 800 |
| TcpOutSegs/s | 691k | 588k | 677k | 764k |
| TcpInSegs/s | 636k | 543k | 637k | 696k |
| TcpExtTCPPureAcks/s | 68k | 61k | 75k | 88k |
| TcpRetransSegs/s | 1.3k | 1.0k | 1.9k | 2.0k |
| Active / passive opens/s | 775 / 1.2k | 527 / 1.2k | 522 / 2.2k | 697 / 1.8k |
| EstabResets/s | 931 | 750 | 715 | 824 |
| Context switches/s | 809k | 748k | 941k | 819k |
| Interrupts/s | 803k | 737k | 831k | 882k |
| conntrack entries | 393k | 338k | 428k | 429k |
| TCP inuse / timewait | 474k / 23k | 390k / 22k | 469k / 39k | 485k / 36k |

- **edge-3 against CPUOPT2:**
  - keepalive probes 170k/s → 0.8k/s;
  - TcpOutSegs 865k → 677k/s, TcpInSegs 741k → 637k/s, with more clients;
  - pure ACKs 166k → 75k/s;
  - conntrack 591k → 428k.
- **Kernel options:** `retbleed=stuff iommu=pt` are live on all four.
  - `retbleed` reports "Mitigation: Stuffing" on edge-0/3/4 and "Not affected"
    on edge-1.
  - IOMMU groups are all `identity`. `intel_iommu_map_pages` is gone from the
    profiles.
  - `rmem_max` is 1 MiB, and `busy_poll` is 0.
- **Whole-host kernel paths on edge-3,** as a share of non-idle samples (all /
  connect / nginx):

  | Path | All | connect | nginx |
  |---|---|---|---|
  | `handle_softirqs` | 14.6 | 5.8 | 3.0 |
  | `tcp_sendmsg` | 12.5 | 7.6 | 3.8 |
  | `net_rx_action` | 13.0 | — | — |
  | `i40e_napi_poll` | 7.9 | — | — |
  | `process_backlog` (local delivery) | 4.4 | 3.0 | 1.2 |
  | `nf_hook_slow` | 4.8 | 2.4 | 1.3 |
  | `nf_conntrack_in` | 2.3 | — | — |
  | `do_epoll_wait` | 3.9 | 1.3 | 1.7 |
  | futex | 3.5 | 2.1 | — |
  | `nanosleep` (Go spinning) | 1.8 | 1.0 | — |

  - Kernel self by group: TCP/IP/socket 14.4, entry/exit+mitigation 12.2
    (stuffing thunk 2.4), scheduler/futex/timers 9.3, netfilter/conntrack 3.4,
    mm/slab 3.3, NIC/NAPI 2.2, epoll 1.4, AppArmor 0.8.
  - perf's leaf-based kernel share is 58% of non-idle. That overstates the 40%
    from Mimir, because IRQ entries and IRQ work on idle CPUs count as kernel
    samples.
- **Docker logging** is still `json-file` on all four.
- **Exchange locality on edge-3,** exact by DNAT:
  - Outbound connect connections to exchange listeners: same process 50,334
    (43.2%), same host other process 11,201 (9.6%), other edges 55,057
    (47.2%).
  - The host has 55.4k transport and 64.9k forward outbound ends, so about
    91% of transports are same-process.
  - Inbound on connect listener ports: 114k from this host (about 61.5k
    exchange and about 52.8k nginx H1 relays) and 43.7k from other edges.

#### 2.6 Runtime shape per connect process, and the service-runaway pages

Each g-block process at 16–17 h of age (beta processes are small):

| Measure | Range |
|---|---|
| CPU | 5.7–7.4 cores |
| Goroutines | 760k–990k |
| RSS | 48–66 GB |
| Heap in use / stacks | 34–50 / 6.2–8.6 GB |
| Allocation | 127–200 MB/s |
| GC | 0.4–0.5 cycles/min |
| Clients / residents | 11–14.6k / 22–30k |
| Resident callback workers | 79–151k |
| Forward workers | 13–22k |

- **Every g-process trips the probe twice** (`monitor/signal_service_load.go`):
  goroutines ≥ 500k is a hard ceiling, and RSS ≥ 16 GiB with CPU ≥ 4 cores is
  the other. At restart, 10.3M goroutines over 16 g-processes is still about
  640k each.
- **Per-uptime state:**
  - Resident callback workers grow from 0.70M to 1.65M fleet-wide over a
    generation. Forward-ingress shard goroutines start lazily and never stop,
    up to 16 per resident.
  - Goroutines grow from 10.3M to 12.5–13.8M.
  - None of this shows in CPU: the residual slope is −0.07 cores/h in P1 and
    +0.02 in P2.

### 3. Did the shipped changes show up? (CPUOPT2 §3.1–3.7)

#### 3.1 Exchange keepalive (§3.1)

- **Probes:** 170k/s → 0.7–1.0k/s per host, under the restored 11 s idle,
  1 s × 4 policy.
- **Segments and packets:** pure ACKs about halved, NIC tx packets −29% (P0 →
  P2), softirq per exchange frame −32% (20.1 → 13.6 µs·CPU per frame).
- **Confounding:** part of the softirq drop is `iommu=pt` and fewer idle
  forwards, which changed the same night.
- **What restoring keepalive costs:** at about 800 probes/s per host, the
  probes are negligible.

#### 3.2 Timer hygiene (§3.2)

- **Timer share:** `timers.check` is 10.5–11.3% (was 15–16%), and `adjust` is
  7.2–8.0% self (was 10–13%). The predicted 3–5% did not materialize:
  - The lazy deadlines were reverted for correctness.
  - Ping pacing and timer reuse shipped.
  - Most of the drop came from smaller heaps: 60% fewer forward connections.
- **The open question from §3.2 is now answered:** what triggers a scan every
  ~50–60 ms per P (§4).

#### 3.3 Restored full deadlines

- **Direct cost:** 1.6% of connect CPU (`exchangeDeadline.refresh` incl.;
  `runtime_pollSetDeadline` from exchange 0.64%). Under P1's lazy re-arm it was
  an estimated ~0.15%.
- **Heap-scan cost:** none. Every install moves a timer to now+15 s or now+30 s,
  and Go 1.27 `modify` only schedules a full rescan when a heaped timer moves
  *earlier* than its P's pending minimum.
- **In the modify census,** installs at 15 s and 30 s (exchange and H1
  deadlines) are about 80% of heaped-timer modifies: the 8–32 s buckets.
  Exchange deadlines are about 40% of modify CPU. None of them fall in the
  short buckets that trigger scans.
- **Net:** about 1.5 cores fleet-wide (0.8% of busy). The Mimir same-hour delta
  (+2–3%) agrees within noise.

#### 3.4 Forward tail of 3 min (§3.3 step 1)

- Forward ends 1.40M → 0.54M (−62%).
- Forward workers and idle watchers 758k → 276k.
- Goroutines per client −40%, heap −31%.
- Forward lookups 7.5k → 2.9k/s.
- Redials did not explode: about 1.5k new exchange connections/s, a mean life
  of about 5.5 min, and handshake frames 0.3% of all frames.

#### 3.5 Not shipped, and has grown

- **Liveness-lease writer (§3.5):** 2.8% → 4.0% of connect CPU.
- **In-process delivery (§3.3 step 2):** CPUOPT2's prerequisite measurement is
  now done (§2.5). About 91% of transports are same-process.

#### 3.6 Kernel (§3.7)

- `iommu=pt` is confirmed: identity domains, no map/unmap in samples.
- System time per exchange frame fell about 7% (25 → 23 µs·CPU), against a
  predicted 3–8% of busy CPU for `retbleed=stuff`.
- Stuffing itself now costs 1.6% of connect samples and 2.4% of host samples.
- The effect could not be isolated, because it changed with P1.

### 4. What drives the remaining timer-heap cost (new)

#### 4.1 Measured (bpftrace)

| Process | Window | `adjust` calls | Full scans | Forced (zombie) scans | Heap per P | Timers visited |
|---|---|---|---|---|---|---|
| edge-3 g4 | 5 s | 100k | 1,293 (259/s) | 0 | 16–64k, mean 32k | 8.3M/s |
| edge-1 g2 | 5 s | 81k | 1,117 (223/s) | 0 | 16–32k | 6.6M/s |

- Zombies stayed under 10% of every heap, so no scan was forced by zombies.
- Heaps of about 32k timers on each of 16 Ps mean about 500k timers per
  process.
- `adjust` self time is 0.50 cores per process: about 60 ns per timer visited,
  a cache miss on each scattered timer.

1-s modify census (`runtime.(*timer).modify`, entry-only uprobe):

| Process | Modifies/s | On heaped timers | Lowered a P's `minWhenModified` |
|---|---|---|---|
| edge-3 g4 | 293k | 254k | 3,256 |
| edge-1 g2 | 244k | 207k | 2,951 |

- Lowering modifies by delta: about 900/s under 250 ms (most at 8–32 ms),
  about 800/s at 0.25–1 s, and the rest at 1–32 s.
- Sampled stacks of the short-delta lowering modifies:

  | Caller | edge-3 | edge-1 |
  |---|---|---|
  | pgx `(*PgConn).flushWithPotentialWriteReadDeadlock`, from `ExecStatement`/`Exec`/`Pipeline.Sync` | 59% | 64% |
  | quic-go `(*Conn).maybeResetTimer` | 28% | 24% |
  | go-redis `pool.(*Conn).WithWriter`/`WithReader` | 11% | 10% |
  | connect library `SendSequence.Run` | 2.5% | 2% |

#### 4.2 Why these trigger scans

In Go 1.27, `(*timer).modify` (runtime/time.go:549–640) marks a heaped timer
`timerModified`. If the new `when` is below the P's `minWhenModified`, it lowers
that minimum. When the minimum comes due, `(*timers).adjust` (time.go:834)
walks the entire heap and rebuilds it.

A *new* timer goes in with an O(log n) insert and never sets
`minWhenModified`. A stopped timer becomes a zombie and leaves in O(log n) when
it reaches the top. So the trigger is specifically a timer that was **reset
while still heaped** (for example, stopped less than its period ago) to a
**near** time.

- **pgx v5.10.0** (`pgconn/pgconn.go:2107–2137`) calls
  `slowWriteTimer.Reset(15 * time.Millisecond)` before every flush and `Stop()`
  after it. Any query within 15 ms of the previous one on the same connection
  resets a heaped zombie to 15 ms ahead, so that P scans 32k timers 15 ms
  later.
  - Each connect process makes at least ~600 such re-arms/s. The queries come
    from contract close and settlement, `GetProvideKeyChanges` and
    `dbWithPool`.
- **quic-go v0.61.0** (`connection.go:861–903`) calls
  `c.timer.Reset(monotime.Until(deadline))` unconditionally on every run-loop
  iteration (line 902). The deadline is usually the ACK alarm or the pacing
  deadline, milliseconds ahead. The timer is the loop's select timer, so it is
  heaped as a zombie whenever a packet woke the loop.
- **go-redis v9.23.0** sets the socket deadline from `min(now+timeout,
  ctx.Deadline())` on every command (`internal/pool/conn.go:1163–1230,
  1382`). Callers with 50–250 ms contexts produce near-term deadline modifies.

#### 4.3 The heap itself

About 500k timers per process. The estimated mix is:

| Owner | Timers | Share |
|---|---|---|
| Exchange: read and write netpoll deadlines and a ping timer per end | ~187k (62k ends × 3) | 37% |
| H1 transports | ~40k | 8% |
| Resident connect clients and listeners | the rest | ~55% |

So the cost is about (scan triggers per P) × (heap per P). §6.1 shrinks the
heap and §6.2 removes the triggers.

### 5. Contract rejections and connect uptime

- **The growth is on api's `/connect/control`:**

  | Generation age | Control requests/s | Rejections/s |
  |---|---|---|
  | 2 h (P2) | 3.3k | 0.85k |
  | 16 h (P2) | 10.4k | 6.9k |
  | end of P1, 23 h | 11.8k | 8.9k |

  - Rejections are inactive_destination 75%, missing_companion_origin 17%,
    provide_secret_unavailable 8%.
  - The refusal replay cache serves about 80–85% of them.
  - They reset at each connect restart (10-30 03Z, 10-31 02Z), when clients
    reconnect and rebuild their windows.
- **The CPU is in api:** api = 1.12 × control requests (k/s) + 0.021 ×
  clients(k) + 1.4 cores (P2 fit). Late in a generation that is about 6–7
  extra api cores, plus nginx TLS and HTTP for about 7k extra requests/s.
- **No connect CPU is tied to uptime:** the residual slope is +0.02 cores/h.
- **State that does accumulate with uptime:**
  - resident callback workers, +0.95M goroutines over 15 h;
  - stack memory.

  Neither costs measurable CPU. GC stack scanning grows with goroutines, but GC
  is about 3%.
- **The service-runaway pages are not an uptime effect** (§2.6).

### 6. Ranked opportunities

#### 6.1 In-process delivery when the resident is local (transport first)

**Evidence:**
- On edge-3, 50.3k of 116.6k outbound exchange connections dial their own
  process (exact by DNAT). That is about 91% of the host's 55.4k transports.
  Fleet-wide, about 200k TCP loops, or 400k sockets.
- A relayed packet crosses two transport hops and one forward hop, so
  transports carry two thirds or more of exchange frames. About 60% of exchange
  frames therefore go over in-process loops.
- The exchange socket trees are 27% of connect CPU, with their syscalls and
  inline kernel work. `process_backlog`, the local TCP delivery, is 3.0% of
  host samples for connect alone.
- `ResidentTransport.Run` always dials, even when the resident is in this
  process (connect/resident.go:3559, dial at :3692). `isLocalResident`
  (connect/resident_move.go:335) is used only for move takeover.

**Estimated saving:**
- 60% of 27% is 16% of connect CPU. Less one channel hand-off per frame (about
  1–2 µs; about 0.6–1.1 cores), that is **12–15 cores, 7–8% of busy.**
- Also: about 200k conntrack entries, about 2M goroutines (−15%), about 75k
  timers per process (−15% heap, so −15% of `adjust`), and 4.8 GB of 96 KiB
  rings per process.

**Change:**
- In `ResidentTransport.Run`, when `isLocalResident(clientId,
  resident.ResidentId)` holds, attach `self.send` and `self.receive` directly as
  a transport of the local `Resident`. Reuse the inbound path's attach:
  `runTransport` in `handleExchangeConnection` (resident.go:1607) over
  `Resident.addTransportWithEdge` (:5119).
- Skip `NewExchangeConnection`. Detach exactly where a socket close would:
  resident cancel, move takeover or drain.
- Then do the same for `ResidentForward` (resident.go:3855, dial :3971) into
  `AddForward` (:5182).

**Correctness under the owner rule:**
- No remote timeout changes. The socket deadlines, pings and keepalive exist to
  detect a dead remote process or path. In-process, the only failures are
  process death (both ends die together) and resident cancellation, which the
  attached transport observes immediately through the resident's context.
  Detection becomes immediate, never later.
- Keep the 15 s write-backpressure bound: the in-process queue admission keeps
  `sendMessage`'s timer at `WriteTimeout`.
- Keep the resident's activity refresh: count each frame, and refresh at least
  every `ExchangePingTimeout` while attached, as exchange pings do now.
- MOVE and drain must break the attachment exactly where they now close the
  socket.

**Risk:** medium. Pooled-buffer ownership across the attach, generation
checks, drain counts, and MOVE takeover ordering.

**Validate:**
- One g-block A/B: `exchange_active_connections{op="transport"}` should drop
  about 90% on that block.
- Compare cores per 1k clients and µs per frame against the peers.
- perf shares of syscalls, `process_backlog` and `adjust`.
- `receive_queue_dropped` should not change.
- Run the exchange drain, move and e2e suites.

#### 6.2 Stop short-timer re-arms from triggering heap scans

**Evidence:** §4.

**Estimated saving:**
- `adjust` + `siftDown` + `check` self is about 8.4% of connect CPU.
- Fixing pgx alone removes about 60% of short triggers; scans drop roughly
  40–50%.
- Fixing pgx and quic-go leaves go-redis's 50–250 ms triggers, about 6/s per
  P. Scans fall to about 4–6 per P per second, against 14–16 now: −60–70%.
- **3–6 cores, 2–3.5% of busy.** Compounds with §6.1's smaller heap.

**Change** (keep every timer's firing time; change only how it is armed):
- **pgx:** in `enterPotentialWriteReadDeadlock`, arm a fresh
  `time.AfterFunc(15ms, …)` per flush instead of `Reset` on a stopped timer,
  and `Stop` that instance in `exit…`. The `bgReaderStarted` handshake stays
  per instance. A fresh timer is an O(log n) insert, and its zombie leaves at
  the top. Carry it as a `replace` to a small fork and offer it upstream.
- **quic-go:** in `maybeResetTimer`:
  - skip the `Reset` when the deadline is unchanged;
  - when the deadline moves earlier, replace the timer instead of resetting a
    heaped one;
  - when it moves later, leave the earlier timer armed. The loop already
    recomputes on every wake, so an early wake is harmless.
- **go-redis:** no exact library-side fix. These are netpoll deadlines from
  the callers' own bounds (`ReadContractHoleLease`, session checks), and those
  bounds stay. Their 10–11% share falls only as Redis call volume falls (§6.4),
  and it is the residual.
- **Our own short resets:** connect `SendSequence`'s timers. connect main
  already has "skip an unchanged Reset" (CPUOPT2 §9.4), and it ships with the
  next connect build.

**Correctness:** no timeout, alarm or detection time changes. The pgx 15 ms
detector still fires at 15 ms. quic-go's timer never fires later than its
deadline; at worst it wakes early once and recomputes. Redis bounds are
untouched.

**Risk:** low for pgx, which is a tiny patch with its own tests. Low–medium
for quic-go, which is a fork patch.

**Validate:**
- Rerun `bpf_adjust.sh`: full scans/s should fall from about 250 to about
  20–50 per process.
- perf `adjust` self should fall below 2%.
- Rerun the modify census: no pgx stacks in the <250 ms bucket.

#### 6.3 Liveness-lease writer: per-shard index and a dirty list (CPUOPT2 §3.5)

**Evidence:**
- 3.95% of connect CPU (was 2.81%).
- `write.func2` 1.8% self, `written.func1` 1.3% self, map iteration 0.44%.
- Every wake (`begin`, and `afterRead` state changes) runs `write`
  (transport_announce_liveness_lease.go:314). That loops over all of the
  process's connections twice, and `written` (:388) loops over all of them
  again for each shard written. A full pass does that for 32 shards.

**Change:**
- Keep a per-client count of providing connections, updated in `afterRead`
  and `end`.
- Keep a dirty set, and per-shard indexes from client to connections.
- `write(all=false)` touches only dirty connections. `written` touches only
  that shard's clients. `oldest` (:255) keeps a min-structure or is computed in
  the full pass.

**Saving:** about 3% of connect CPU, about 3 cores (1.7% of busy).

**Correctness:** the same writes, vouch timestamps, withholding and releases.
Only bookkeeping changes.

**Risk:** low.

**Validate:** perf share below 0.5%; `liveness_lease_writes_total` outcomes
unchanged.

#### 6.4 Key-event merge overflow and the close-listener resync

**Evidence:**
- `urnetwork_key_event_resubscribes_total` per process tracks
  `urnetwork_redis_key_event_merge_drops_total` almost exactly (now 0.82 against
  0.85 per minute).
- Both were 0.01–0.04/min until the client-close feature arrived (10-28
  10–16Z). Since then they are 0.2–1.4/min, rising with client-close activity.
- Each resubscribe runs `resyncAll` and `resyncCloseListeners`
  (connect/key_event_subscriber.go:434, :187). That kicks every resident's
  close listener, about 22–30k per g-process, each doing one Redis `SPOP`
  (`TakeClientCloseRequests`, model/client_close_model.go:165).
- `TakeClientCloseRequests` is the largest Redis caller: 2.5% of connect CPU.
- The overflow is the shared merge channel of `MessageBufferSize` 1024
  (redis.go:706, :760). On overflow the generation ends (:835).
- Every connect process receives every client-close keyspace event: sadd,
  spop, del and pexpire.

**Change:**
- **Absorb bursts without dropping.** Raise the merge buffer to the measured
  burst; 64k entries is about 2–3 MB. Or drain per node into per-node
  buffers. Keep "drop → resync" for true overflow only.
- **Make the close resync cheap and exact.** Pipeline the takes per Redis slot
  instead of one round trip per listener, and kick only the listeners whose
  `SPOP` returned members.
- **Optionally, narrow the events.** Only sadd matters to listeners
  (`dispatch` ignores spop and del); move the take bookkeeping off the
  keyspace channel.

**Saving:**
- About 2–3% of connect CPU (2–3 cores; 1–2% of busy).
- Redis server work for about 6k `SPOP`/s.
- The extra `resyncAll` waves after each drop.

**Correctness:** an event that is not dropped needs no resync. A pipelined take
is the same per-client `SPOP`. A genuine overflow still triggers the full
resync.

**Risk:** low–medium. Memory bound and tests for the drop path.

**Validate:**
- `merge_drops_total` and `resubscribes_total` should go to about 0.
- `TakeClientCloseRequests` perf share below 0.3%.
- `client_close_sequences_sent_total` unchanged.

#### 6.5 The `/connect/control` rejection creep (api/SDK; tied to connect uptime)

**Evidence:** §5. 0 → 6–8 api and nginx cores over a generation, reset by each
connect restart. inactive_destination is 75% of the rejections.

**Change** (api and SDK domain):
- When a cached refusal replays for (client, destination), have the server
  tell the client to drop that destination. `CloseSequences` and the refusal
  details already exist.
- Or have the resident push the refusal over the control channel, so the
  client stops retrying the stale exit.

**Correctness:** no bound is involved; the change only removes retries the
platform already refuses.

**Validate:** the control rate tracks clients through a generation, instead of
tripling.

#### 6.6 H1 without nginx (CPUOPT2 §3.6)

**Evidence:**
- nginx is 26.6 cores (15% of busy) and 17.8% of edge-3 samples, 60% of them
  kernel.
- It holds 49–72k client TLS sockets and 45–57k upstream relays per host, which
  is about one per H1 client.
- On the connect side, the relay read/write costs 3–5% of connect:
  `httpUpgradeConn.Read` 1.4, the batch conn read 0.9 and flush 1.2.
- Each relay is 2 extra sockets and an extra conntrack entry.

**Change:** as in CPUOPT2 §3.6. Connect terminates H1/H1+ TLS on dedicated
addresses (warp DNAT or a SO_REUSEPORT group), and rate limits move into
connect.

**Saving:** 8–12 cores net (5–7%). The upstream hop and part of nginx's event
loop disappear; TLS moves into Go at roughly the same cost.

**Risk:** high: ports, rate limits, and deploy port rotation.

#### 6.7 Multiplexed links per process pair, after §6.1 (CPUOPT2 §3.3 step 4)

**Evidence:** after §6.1, about 40% of today's exchange frames remain on
cross-process links:
- forward connections, about 280k per direction;
- cross-process transports.

Each remaining socket carries about 2 frames/s, so nearly one frame per
syscall. Its 3 timers and about 10 goroutines are still per-connection.

**Change:** as in CPUOPT2 §3.3 step 4:
- one or a few links per process pair, carrying stream ids, with per-stream
  credit;
- coalesced `writev` with a short linger;
- one ping and one deadline pair per link.

**Saving:** 5–8 cores (3–4.5%). That is about half of the remaining exchange
cost, plus about 1.7M timers and 2.8M goroutines fleet-wide.

**Correctness:** each link keeps the exchange read, write and ping bounds. Each
stream needs its own write-timeout bound for backpressure, through credit.

**Risk:** high: a protocol version, mixed-version rollout, head-of-line
blocking, and blast radius.

#### 6.8 Rings and shards on demand (CPUOPT2 §3.4)

**Evidence:**
- `defaultExchangeBufferSize` 4096 (resident.go:395) pre-sizes 96 KiB rings
  for every exchange end: :2928, :3553–3554, :3849, :4758, :5128–5129, :5203.
- About 1.5k new connections/s means 0.3–0.6 GB/s of ring allocation and
  zeroing. `makechan` is 0.48% and `memclrNoHeapPointers` 0.73% of connect.
- Rings are an estimated 5–8 GB of each process's heap.
- Forward-ingress shards (`defaultResidentForwardQueueShardCount` 16, :408)
  never stop, which is the per-uptime callback-worker growth.

**Change:** start rings small and grow them to the same caps (keep the hard
count and byte bounds). Cap shards at 2–4.

**Saving:** about 1 core and 5–8 GB heap per process. The shard cap bounds the
per-uptime goroutine growth: residents average about 4 shard goroutines after
16 h, up to 16 each. A cap of 2 saves about 0.6M goroutines fleet-wide.

**Correctness:** the same queue limits, so the same drop and backpressure
behavior.

#### 6.9 Logging and observability (CPUOPT2 §3.10)

**Evidence:** 7–9 cores across the four hosts (4–5% of busy):
- dockerd, containerd-shim and the docker CLI: 2.9% of edge-3 samples;
- loki, fluent-bit and mimir: 1.3%;
- journald and rsyslog: 1.0%;
- Mimir, Loki and Grafana listeners on every services edge;
- `json-file` logging on all four.

**Change:** as in CPUOPT2 §3.10. Move Mimir, Loki and Grafana to edge-6, and
switch to the docker `local` driver or journald-only.

**Saving:** 2–4 cores.

#### 6.10 Small items

- **`connect.encodeUuid` through `fmt`:** 0.45% of connect, counting its
  `fmt` and malloc time. It builds Redis key strings. Switch to hex encoding in
  the connect library, as CPUOPT2 §8.6 #4 proposed for the proxy.
- **Skip the read-deadline install when the next frame is already buffered.**
  A `ReadMessage` with no physical read has no I/O to bound, and the next
  physical read installs its own full deadline. Exact; about 0.1–0.2% of
  connect.
- **AppArmor unconfined for connect:** `aa_inet_msg_perm` is 0.5% of connect.
  This is a security decision.
- **Conntrack:** §6.1 removes about 50k entries per host. Same-host
  cross-process exchange could dial the internal port over loopback with
  NOTRACK and no DNAT.
- **Goroutine and channel hand-offs (CPUOPT2 §3.4):**
  - `selectgo` is 10% inclusive. The top sites are `sendPooledReceive` 2.1,
    the H1 writer 1.2, the exchange writer 1.0, `sendPooledMessage` 0.8,
    `readWithCarrier` 0.6 and the inbound transport writer 0.5.
  - §6.1 removes 2–3 hand-offs per in-process frame.
  - The rest of §3.4 is still worth about 1–2% of connect after §6.1:
    `context.AfterFunc` for the Done watchers, the writer inside
    `ExchangeConnection.Run`, and the forward consumer merged into its reader.
- **GOMAXPROCS experiment:** 5 × 16 = 80 Ps on 72 CPUs, with `findRunnable`
  at 24.5% and `stealWork` at 8% of connect. Try 12 on one block. But fewer
  Ps make each heap larger, which raises the cost per scan. Measure `adjust`,
  and do this after §6.2.
- **THP for connect heaps, and NUMA pinning:** still unmeasured, one host each
  (CPUOPT2 §3.7.5).
- **The runaway probe:** its absolute 500k-goroutine ceiling flags healthy
  connect processes at about 37 goroutines per resident. A per-resident band
  would separate growth from cost. This is the monitor domain, not CPU.

### 7. CPUOPT2 predictions: what did and didn't materialize

| CPUOPT2 item | Predicted | Observed |
|---|---|---|
| §3.1 keepalive | 2–5% of busy | Probes −99.5%, softirq per frame −33%, NIC tx −29%. A real but confounded share of the −18% busy per client; kept after restoration at 11 s idle |
| §3.2 timers | timers 15% → 3–5% | `check` 10.5–11.3%, `adjust` 7.2–8%. Lazy deadlines were reverted; the remaining scans are third-party short re-arms (§4) |
| §3.2 open question: why scans every ~50 ms | — | Answered: pgx 15 ms, quic-go and go-redis short re-arms of heaped timers; 223–259 scans/s per process over ~32k-timer heaps |
| §3.3 step 1, forward tail | 2–4%, −0.2M goroutines and GBs per process | Forward ends −62%, goroutines per client −40%, heap −31%. Redials modest at ~1.5k/s |
| §3.3 step 2, in-process delivery | 5–10%, "measure the local fraction first" | Measured: ~91% of transports same-process (43% of outbound exchange). Estimate raised to 7–8% of busy |
| §3.5 liveness writer | ~1% | Not shipped; cost grew 2.8% → 4.0% of connect |
| §3.7 `retbleed=stuff`, `iommu=pt` | 3–8% + 1–3% | IOMMU passthrough confirmed. System time per frame −7%; the stuffing thunk itself is 1.6–2.4% of samples. Not separable from P1 |
| §2.3 "TLS, protobuf, Redis, pgx <0.25% each" | — | Wrong for Redis and pgx. CPUOPT2's category grep (`svc_perf.sh`, `edge_perf2`) anchored each pattern at the symbol start (`\] redis/go-redis`), and go-redis and pgx symbols begin with `github.com/`. They are 6.6–9.6% and 2.1% of connect now; the growth since 10-29 can't be told |
| §2.4 per-client cost rising with the control creep | — | The creep is api CPU (§5). Connect per-client cost is flat over a generation |

### Raw outputs

All files are in
<scratch>/cpuopt3/svc/:

- **Mimir:** `hosts.txt`, `work3.txt`, `series.txt`, `starts.txt`,
  `rej.txt`, `frames3.txt`, `procs3.txt`, `regress.txt`, `svc7d.txt`.
- **Host census:** `census_all_2.txt`, `census_dnat_edge3.txt`.
- **perf:** `perf_conn_edge3_1.txt`, `perf_conn_edge3_2.txt`,
  `perf_conn_edge3_3.txt`, `perf_host_edge3_1.txt`.
- **bpftrace:** `bpf_adjust_edge3.txt`, `bpf_modify_edge3_2.txt`,
  `bpf_both_edge1.txt`.
- **Scripts:** `host_census.sh`, `census_dnat.sh`, `perf_conn2.sh`,
  `perf_conn3.sh`, `perf_host.sh`, `bpf_adjust.sh`, `bpf_modify.sh`, and
  `py/*.py`.

## Part B — Proxy hosts and the proxy service

CPUOPT3 section, 2026-10-31. Release under study: proxy
`2026.10.31-outerwerld-1086377170` (server a8461e236, connect 89b15920, sdk
fe265dfc, proxy 1575ad2, userwireguard 70ecd6e; source pinned in scratchpad
ws-rel31), 10 blocks on each host, deployed 09:33–09:56Z. Everything below is
read-only measurement plus code reading at those exact revisions.

### Summary

- **Correction to the brief's per-process figures.** The canary script
  (`proxy_canary.py`) sums each block over both hosts, so every row is two
  processes. Per process, the old release at 13 h (09:44Z) was 153–165k
  goroutines, 4.0–4.8 GB heap_inuse, 0.55–0.63 cores and 1.25–1.34k devices
  (not 313–326k, 8–9 GB, 1.15–1.22 cores and 2.6k). The g1 canary at 10 min
  was 0.46–0.49 cores per process.
- **Nothing leaks.** CPU, goroutines, stacks and heap follow the number of
  platform transports (window clients) the process holds, within each release
  and regardless of uptime. After controlling for transports and devices, the
  old release drifted +0.45 mcores/h and −66 goroutines/h over 13 h. The
  "growth" was the daily device waves: transports per device swing from 1.5
  to 4.0 within a day, and the marginal device brings about 6 transports (a
  full hosted window set: quality 5 plus speed 1).
- **Cost model now (per process, current release, r² 0.88–0.996):** about
  0.06 mcores, 18 goroutines and 83 KB of stack per platform transport, plus
  0.17 mcores, 52 goroutines and 260 KB of stack per device. At the evening
  peak a process holds about 1.5k devices and 5.1k transports: 0.5–0.6
  cores, 165–180k goroutines, 4.4 GB heap_inuse, 1.25 GB of stacks. That is
  0.11–0.12 mcores per transport, against 0.39 in the 10-29 saturation and
  0.13 right after the 10-30 release.
- **Where it goes.** About 4% of process CPU moves user bytes (the tun loop,
  WireGuard, gVisor, the HTTP proxy server). About 37% keeps live windows up:
  H1 carrier I/O 12%, send and receive sequences 15%, `Client.run` 4%,
  channel loops 3–4%. About 19% creates and retires window clients: TLS
  dials 6% (x509 chain verification alone 5.3%), contract and provide control
  5%, hosted mints 1.4%, FindProviders2 2.9%. Rejected `PeerAudit` calls take
  3%. The Go runtime takes the rest: scheduler 13–17% (38k context
  switches/s per process), GC mark 6–9%, allocation 4–5%, timer heaps about
  6%.
- **Goroutines:** the code has about 49 permanent goroutines per idle device
  and about 18 per window client, matching the fit. 18 of the 49 are a
  per-device copy of the contract-origin notifier that api, connect and
  taskworker keep once per process.
- **Idle devices cost as much as active ones.** 71% of live devices moved no
  user traffic in the last 2 minutes, yet they hold as many transports as
  active ones (regression coefficient for active devices ≈ 0). Most of the
  proxy's CPU keeps idle devices' windows warm for the 45-minute hold.
- **Most window-client creations are probably thrown away.** Hosted mints
  run at 269/s fleet (13.5/s per process; 6–31 per device-hour) and
  retirements at 265/s, against about 5.1k transports per process. Each
  creation is a mint, a Provide frame, a TLS dial, an evaluation ping and
  contracts.
  - Hosted windows run with a strict hard max, and a lifetime-drained
    client only warns and keeps its slot. Every resize pass that sees the
    "hole" builds a full replacement, which `strictWindowAdmissionAllowed`
    then discards (connect ip_remote_multi_client.go:13565-13606,
    14326-14341, 15174-15187). That settles at one candidate per 1–2 minutes
    per stuck window, with no end.
  - Under-filled windows whose expansions admit nothing build and discard
    clients on the same backoff.
  - Hosted logging is off and no metric counts declines, so the split
    between the two needs a counter first.
- **71% of the proxy's control calls are rejected.** `SequencePeerAudit`
  sends `PeerAudit` frames to the control destination, and no server handler
  exists: 2,255/s fleet end as "Cannot handle oob control message" after a
  full JWT verification. The api sees another 2,731/s.
- **Memory.** At every daily peak, RSS sits at exactly 95% of GOMEMLIMIT
  (6.04 GiB of 6500 MiB, the scavenger's target). GC is still paced by
  GOGC (1.0–1.4 cycles/min), not by the limit, but the limit starts to
  govern at about +20–30% transports. The 10 GiB cgroup has never been
  approached (peak 6.19 GiB, no memory events). fireside has 18 GiB
  available at peak; crisp has about 50 GiB.
- **Bursts.** Every platform ping and channel ping is wall-clock aligned. At
  each 10 s boundary all ten processes on a host jump past one core at once:
  20 of 24 cores for about 100 ms, against 7–8 cores on average. Together
  with 395k context switches/s per host, that is why PSI cpu-some reads
  16–18% at 31% busy.

**Ranked opportunities** (proxy CPU at the evening peak is 12–13.5 cores
fleet-wide; savings overlap, so do not add them):

| # | Change | Proxy CPU saved | Risk |
|---|---|---|---|
| 1 | Stop building candidates that the strict hard max will refuse; let a drained flowless client hand over its slot | 10–19% (1.2–2.5 cores), plus about 265 mints, retirements, TLS handshakes and contract pairs per second off pg, Redis and the edges | low; rotation starts working as designed |
| 2 | Idle devices: an idle tier (one client per device after 10 min without user traffic), or a 20-minute hold | idle tier 20–30%; 20-min hold 30–40% before re-creation costs | resume latency; product call |
| 3 | Cache verified certificate chains (or make TLS resumption work per edge) for platform dials | 4–5% (0.5–0.6 cores), plus edge nginx handshakes | low |
| 4 | Stop sending `PeerAudit` to the control (or accept it as a no-op) | 2.5–3% (0.3 cores), plus 2.7k req/s at the api | very low |
| 5 | One `ContractOriginNotifications` per proxy process, as api, connect and taskworker have | −27k goroutines and ~0.2 GB stacks per process; 1–2% CPU | low |
| 6 | Cache verified hosted JWT claims in `localclient.Authority` | 1.5–2% | low |
| 7 | Wire `PingJitter` for hosted transports and jitter channel pings | 1–3%, PSI 16% → low | low |
| 8 | GOMAXPROCS 4 → 3 on one block (after #7) | 2–5% (A/B) | burst latency |
| 9 | Memory: keep 6500 MiB on fireside; 8 GiB on crisp; GOGC 200 on crisp only | 3–4% of crisp's proxy CPU | crisp RAM |
| 10 | Hosted quality window 5 → 3 (CPUOPT2 §8.6 #5, not done) | 10–15% (overlaps #2) | redundancy |

Details, evidence and validation are in §4.

### 1. Method

**Load at the time.** Evening peak of 10-31: 30.1k live devices fleet-wide
(1.5k per process), about 100k platform transports (5.1k per process), 9k
devices with user traffic in the last 2 minutes, host busy 28–33%, proxy
5.9 cores per host, alt 0.25. User payload was small: 4.2 MB/s of HTTP and
SOCKS sessions and 2.9 MB/s of WireGuard fleet-wide, 172 HTTP admissions/s.

**Mimir** (5-minute steps, 10-30 12:00Z to 10-31 18:50Z; three releases):
- per process (20 series, keyed host/block): CPU, heap_inuse, heap_alloc,
  next_gc, stacks, RSS, goroutines, GC cycles, allocation and malloc rates,
  fds, threads;
- per process from the proxy's own gauges: devices live and active,
  platform transports, sessions, WireGuard sessions and peers;
- host CPU modes, memory and swap, and daily peaks back to 10-22 (CPUOPT2's
  window);
- control-frame, contract, JWT-state, FindProviders2, page-cache and
  Redis-pool counters for service=proxy.

Least-squares fits of each per-process resource against devices and
transports, per release, skip each process's first hour.

**perf** (read-only, data deleted on the host, symbol aggregates only):

| Capture | When | Process | Event, rate, length | Samples | Purpose |
|---|---|---|---|---|---|
| A | 18:33Z | crisp g5 | cycles, 199 Hz, 45 s | 6,772 | categories, roots, allocation and timer callers |
| B | 18:37Z | crisp, host-wide | /proc at 100 ms, 30 s | — | wall-clock phase of host and process CPU |
| C | 18:40Z | fireside g5 | cpu-clock, 199 Hz, 50 s, CLOCK_REALTIME | 6,367 | which work runs in the bursts; host phase over 60 s |
| D | 18:52Z | crisp g8 | cycles, 199 Hz, 50 s | 7,684 | callee breakdown of the main subtrees; /proc/pid/io |

The hybrid Core Ultra 9 285HX PMU refuses `-k` with hardware cycles, so C
uses the cpu-clock software event. `perf stat` (10 s) gave context switches,
migrations and faults for A's process. `nstat` (10 s), `ss` state counts and
cgroup v2 memory files were read on crisp; cgroup files on fireside.

**Code** was read at the release revisions (ws-rel31). Two read-only code
studies enumerated the per-device goroutines and the window-client removal
paths (§3.1, §3.5).

Scripts and raw outputs: scratchpad `cpuopt3/proxy/` (collect.py,
regress.py, resid.py, hosts.py, daily.py, scripts/perf3.sh–perf5.sh,
scripts/net.sh, scripts/recon.sh, and the `*_perf*.txt` outputs).

### 2. Where the CPU goes now

#### 2.1 Hosts

| Host | CPUs | RAM total / available at peak | Busy at peak (30-min) | PSI cpu-some | Proxy cores | softirq + irq | Swap used |
|---|---|---|---|---|---|---|---|
| crisp | 24 (8P + 16E, no SMT) | 125 GiB / 50 GiB | 28–31% | 16% | 5.9 | 5.0% (1.2 cores) | 0 |
| fireside | 24 (8P + 16E, no SMT) | 94 GiB / 18 GiB | 28–31% | 17% | 5.9 | 4.9% (1.2 cores) | 0.07 GiB |

- Busy follows the transport count. 30-minute means at the 10-31 peaks
  were 28–31%, and 35% during the 10:00Z rollover while old and new
  processes overlapped. At 19:12Z, with 106.8k transports and 13.5 proxy
  cores, it was 36.7%, inside the brief's 35–41%.
- Daily mean busy: 36.7% on 10-22, 45–78% on 10-26 to 10-30, 24.6% on 10-31
  so far. Mean proxy CPU fell from 19–35 cores/day (10-26 to 10-29) to 8.7.
- crisp, 10 s: 170 TCP active opens/s, 102k segments/s in, 111k out, 1.3k
  keepalive probes/s, 395k context switches/s, 105k NET_RX softirqs/s,
  52.5k established sockets to port 443 (the platform transports), 10k
  TIME-WAIT. `rtl8127_rx_interrupt` is 2–3.4% of a proxy process's samples:
  the single-queue Realtek NIC (CPUOPT2 §2.6) still interrupts whichever
  core it lands on.

#### 2.2 Per process since the deploy

Hourly means over the 20 processes (current release from 10:00Z; the
previous release before it):

| UTC | Release | Uptime h | Cores | Devices live / active | Transports | per device | Goroutines k | Stacks GB | heap_inuse GB | next_gc GB | RSS GB | GC/min | mcores per transport |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-30 21:00 | 1085916410 | 0.4 | 0.21 | 783 / 243 | 1,352 | 1.73 | 66 | 0.48 | 1.99 | 2.30 | 2.76 | 0.62 | 0.155 |
| 10-31 01:00 | 1085916410 | 4.4 | 0.53 | 1,559 / 541 | 5,108 | 3.28 | 172 | 1.23 | 4.41 | 4.28 | 6.45 | 0.76 | 0.103 |
| 10-31 05:00 | 1085916410 | 8.4 | 0.27 | 1,046 / 244 | 2,108 | 2.02 | 90 | 0.81 | 3.35 | 3.32 | 4.91 | 0.49 | 0.127 |
| 10-31 09:00 | 1085916410 | 12.4 | 0.59 | 1,247 / 545 | 4,722 | 3.79 | 158 | 1.24 | 4.16 | 4.47 | 6.14 | 0.92 | 0.124 |
| 10-31 11:00 | 1086377170 | 1.2 | 0.55 | 1,534 / 452 | 5,073 | 3.31 | 171 | 1.23 | 4.36 | 4.22 | 6.43 | 1.13 | 0.108 |
| 10-31 13:00 | 1086377170 | 3.2 | 0.30 | 1,094 / 241 | 2,439 | 2.23 | 99 | 0.94 | 3.60 | 3.53 | 5.36 | 0.73 | 0.122 |
| 10-31 16:00 | 1086377170 | 6.2 | 0.55 | 1,483 / 336 | 4,919 | 3.32 | 163 | 1.21 | 4.50 | 4.51 | 6.47 | 1.27 | 0.111 |
| 10-31 18:00 | 1086377170 | 8.2 | 0.50 | 1,408 / 320 | 4,385 | 3.11 | 154 | 1.17 | 4.28 | 4.40 | 6.40 | 1.03 | 0.115 |

- **No uptime drift.** Fitting cores, goroutines, stacks and heap on
  transports and devices, then the residual on uptime: the previous release
  drifted +0.45 mcores/h, −66 goroutines/h, +6.6 MB stacks/h over 13 h
  (heap residuals swing ±0.3 GB with the daily load, not monotonically). The
  current release's residual bins over 9 h are −23, −9, −17, +34, +23
  mcores, which is time of day, not a trend.
- **What grew on the old release** between 0.4 h and 12.4 h: devices +59%,
  transports +249%, cores +179%. Transports per device rose from 1.73 to
  3.79. The same swing happens every day at the same hours (peaks near 01Z,
  09–11Z and 15–17Z), on every release.
- **Previous vs current release** at similar load: about 0.11 mcores per
  transport now against 0.12 then (01:00Z and 16:00Z rows), so 5–10% cheaper
  per transport, not a step change. The 10-minute canary looked 20% cheaper
  per block mostly because it held 7% fewer transports on fresh processes:
  0.101–0.103 mcores per transport against 0.114–0.127 on the old blocks at
  that instant.
- **Sessions are not the driver.** HTTP sessions active per process are
  0–14 and ingress connections about 90–125. Session bytes are 0.2 MB/s
  per process.

#### 2.3 What scales: per transport and per device

Fits for the current release (1,800 five-minute samples, 11:00–18:50Z):

| Resource | Intercept | Per device | Per transport | r² | Transports only: per transport (r²) |
|---|---|---|---|---|---|
| CPU | −5 mcores | 0.165 mcores | 0.060 mcores | 0.88 | 0.087 mcores + 109 (0.88) |
| Goroutines | −0.4k | 52 | 18 | 0.996 | 26.5 + 35k (0.993) |
| Stacks | 441 MB | 0.26 MB | 0.083 MB | 0.89 | 0.125 MB + 619 MB (0.89) |
| next_gc | 2.52 GB | 0.59 MB | 0.22 MB | 0.50 | 0.31 MB |
| Allocation | 10.5 MB/s | — | 9.9 kB/s | 0.72 | 8.1 kB/s |
| Mallocs | 86k/s | — | 57/s | 0.73 | 57/s |
| fds | 1.1k | 0.4 | 0.97 | 0.997 | 1.04 + 1.38k |

- **Transports per device** fit `−3,959 + 5.94 × devices` (r² 0.97): the
  marginal device at a peak brings about six transports. Hosted devices run
  the sdk's low-memory window policy, not connect's defaults: quality
  min = max = hard max 5, speed fixed at 1, no standing reserve, and a strict
  hard max (sdk/mobile_memory_policy.go:503-559, asserted by
  device_local_memory_profile_test.go:159-168). An Auto-profile device holds
  all six even when idle. Off-peak devices average 1.5–2; a device with a
  fixed profile and no window size forms one client per window
  (sdk/device_local.go:8474-8476).
- **Active devices add nothing.** With devices active as a second variable,
  its coefficient is −0.07 mcores (CPU) and +0.06 transports (transports per
  device). An idle device costs what an active one costs.
- **A transport is a whole connect.Client.** Each window client owns its own
  `PlatformTransport` (ip_remote_multi_client_api.go:1491-1535), one TLS
  socket (1.04 fds), its sequences, contracts and channel loops. The 57
  mallocs/s and 8 kB/s per transport happen at zero user traffic.

**Growth per added device** at today's mix (devices as the only variable,
current release): +0.52 mcores (r² 0.87), +159 goroutines (0.98), +0.75 MB
of stacks (0.88), +2.0 MB heap_inuse (0.61), +1.9 MB of GC goal, +48 kB/s
allocated, +336 mallocs/s, +6.2 fds and +5.9 carriers. The previous release
had the same shape: +0.47 mcores, +139 goroutines, +0.88 MB stacks per
device.

By load quartile (current release): the busiest quarter of samples ran 5.1k
transports at 0.105 mcores each, the quietest quarter 2.6k at 0.124. So
there is a fixed part of roughly 0.1 cores per process, and the rest is
linear in transports.

#### 2.4 Inside one process

Capture D (crisp g8, 0.77 cores), by goroutine root, the outermost frame
below the `HandleError` wrappers. A and C agree within about 2 points on the
main rows; GC mark varies more because a 45–50 s capture holds zero or one
GC cycle.

| Root | Share | What it is (children) |
|---|---|---|
| `runtime.mcall` | 13.3% | goroutine park → `schedule` → `findRunnable` 12.4%: `stealWork` 3.5, `timers.check` 3.4, `netpoll` 2.2, `stopm` 2.0 |
| `PlatformTransport.runH1.func4.1.1` | 12.0% | H1 carrier workers: reader 6.8 (`ReadH1PooledMessage` 5.2, TLS record decrypt, read syscalls), writer 5.0 |
| `ApiOutOfBandControl.SendControlWithCtx.func1.1` | 9.0% | in-process control: controller 5.2 (`CreateContract` 3.0, `Provide` 1.1, `CloseContract` 0.7), `authenticate` 3.3–3.7 (JWT parse and P-256 verify 2.0, state check 1.5) |
| `SendBuffer.createSendSequence` | 7.7% | `SendSequence.Run`: `estimateSendWindow` 1.8, `Run.func16` 1.8, `receiveAckFeedbackAt` 1.2 |
| `ReceiveBuffer.Pack` + `ReceiveSequence.Run` | 7.6% | `flushDeliver` 3.7 |
| `PlatformTransport.startModeRunner` | 6.3% | dials: uTLS handshake 6.2, of which `x509.Certificate.Verify` 5.3 (P-384 field arithmetic 4.6) |
| GC background mark | 5.6% | 5.6–9.1% across A, C, D; assists 0.25%; `scanstack` + `markroot` 1.5–2.3% |
| `NewClientWithTag.func1` | 4.1% | `Client.run`: `ReceiveBuffer.Pack` 1.3, `MultiRouteSelector.readWithCarrier` 1.2 |
| `windowGeneratorCall` (two shapes) | 4.6% | FindProviders2 2.9; new window clients (`AuthNetworkClient` mint 1.4) |
| `runtime.sysmon` | 3.2% | |
| `newMultiClientChannel` | 2.7% | `detectBlackhole` 2.7 (`windowStatsInto` 0.7), `clientReceive` 2.5 |
| go-redis goroutines | 2.6% | pub/sub channel and pipeline workers; all go-redis frames 7.0% |
| `ProxyDevice.Run`, WireGuard peers, gVisor timers, HTTP server | 3.8% | the user data plane |
| `newIpAssoc`, `newMultiClientWindow`, window identity | 3.4% | |
| `RemoveClientWithArgs`, `parallelEvalWithRouteHedge`, `morestack` | 3.4% | window-client teardown, dial races, new goroutines' stack growth |

Other inclusive shares (D): `mallocgc` 5.0%, timers (`timers.check`) 6.2%,
`timers.adjust` 4.8%, futex 3.0%, pgx 2.7%, `crypto/ecdsa.Verify` 6.2%
(certificate chains 5.3, JWTs 1.2). The kernel is 19% of A's samples:
syscalls, TCP, and the NIC interrupt. `perf stat` on A's process: 38.4k
context switches/s and 460 migrations/s at 0.55 CPUs, 180 page faults/s.
`/proc/pid/io` on D: 14.4k read and 4.7k write syscalls/s, 4.0 MB/s read and
2.5 MB/s written.

Grouped:

| Group | Share of process CPU |
|---|---|
| User data plane (tun loop, WireGuard, gVisor, HTTP proxy server) | ~4% |
| Keeping live windows up (carrier I/O, sequences, `Client.run`, channel loops, IpAssoc) | ~37% |
| Creating and retiring window clients (dials, mints, contracts, provide, FindProviders2, teardown) | ~19% |
| `PeerAudit` control calls rejected by the controller (§3.6) | ~3% |
| Go scheduler and sysmon | ~16% |
| GC mark (allocation is inside the rows above) | 6–9% |
| Redis and pg client work outside the rows above | ~3% |

#### 2.5 Memory against GOMEMLIMIT and the container limit

- **Settings:** GOMEMLIMIT 6500 MiB (6.82 GB), GOGC 100, GOMAXPROCS 4,
  docker `memory_limit` 10 GiB (cgroup v2).
- **At every daily peak RSS sits at 6.02–6.06 GiB** on all 20 processes:
  exactly 95% of GOMEMLIMIT, the background scavenger's goal. The cgroup
  reads the same (memory.current 6.0 GiB, peak 6.10–6.19 GiB, anon 5.96–5.99
  GiB). memory.events shows no high, max or oom events on either host.
- **Composition at the peak:** heap_inuse 4.0–4.9 GB, stacks 1.15–1.36 GB,
  runtime metadata about 0.35 GB. Go's sys is 6.7–6.9 GB, at the limit.
- **GC is still paced by GOGC.** next_gc ≈ 2 × live + stacks, which puts the
  live heap near 1.6 GB at the peak; the limit-derived goal is about 5.0 GB,
  just above the GOGC goal of about 4.5 GB. GC runs 0.6 cycles/min off-peak
  and 1.0–1.4 at the peak; stop-the-world time is 13 µs/s. The scavenger
  does not show in the samples, and page faults are 180/s.
- **Headroom:** with today's slopes (live about 0.16 MB and stacks 0.125
  MB per transport), the limit starts to set the goal at about +20–30%
  transports per process, and GC CPU roughly triples at +50% (cycles every
  ~30 s over a 4.3 GB root set). A continuous-GC regime like 10-29's needs
  about +100%. The 10 GiB cgroup leaves 3.9 GiB above GOMEMLIMIT and is not
  a constraint.
- **Host RAM is the binding resource on fireside:** 18 GiB available at the
  peak with ten 6 GiB processes. Raising GOMEMLIMIT by 1 GiB per process
  would take 10 GiB of it, and a full-overlap restart (run-edges.sh restarts
  every unit at once) needs another ~6 GiB per block. crisp has about 50 GiB
  available.

#### 2.6 Wall-clock bursts

Host-wide CPU in 100 ms bins by phase within each 10-second wall-clock
cycle (B on crisp over 30 s; C on fireside over 60 s):

| Phase | crisp host cores | fireside host cores | Proxies above 1 core |
|---|---|---|---|
| 0.1 s | 20.3 | 20.1 | 10 of 10 |
| 0.2–0.5 s | 11–14 | 10–13 | 3–10 |
| 5.6–6.3 s | 12–14 | 9–12 | 1–6 |
| rest | 5–9 | 5–9 | 0–1 |

- Per-host 100 ms busy: mean 7.7–8.2 cores, p90 11.4–12.5, p99 17.3–17.4,
  max 21.7–22.9 of 24. These spikes, and the wake-up latency of 395k context
  switches/s per host, are what PSI cpu-some 16–18% at 31% average busy
  measures.
- **What runs in the spike** (C, samples at 0.0–0.6 s against the rest):
  `SendSequence.Run` 15.2% (6.3%), writer batches 5.5% (3.2%), uTLS record
  writes 5.7% (2.6%), `selectgo` 9.5% (4.4%), `estimateSendWindow` 4.2%
  (1.3%), `authenticate` 7.3% (3.2%). That is the sending side of platform
  pings and channel pings, plus the control calls they trigger.
- **The cause** is in the code: platform pings use `pingPacer` with offset 0
  on a 5 s epoch, so every idle carrier pings on the same 10 s grid
  (connect transport.go:2947-2953, wakeup_schedule.go:39-48). Channel pings
  rest `resetWakeupTimer(pingTimer, 10 s, 10 s)`, which lands every channel
  on a 10 s boundary, each one every ~20 s (ip_remote_multi_client.go:19890,
  wakeup_schedule.go:23-35). `PingJitter` exists (transport.go:562,
  1775) but nothing in sdk or server sets it, so the proxy runs at offset 0.

### 3. What drives per-device CPU and memory

#### 3.1 Goroutines per device and per window client

The fit says 52 per device plus 18 per carrier (r² 0.996). A code study at
the release revision found about 49 permanent goroutines per idle device and
about 18 per window client (18–26 with short-lived ones). At 3.4 carriers per
device that is about 110 per device, and about 160 for a full Auto device
(5 + 1 carriers). At about 7.4 KB of stack each (1.25 GB over 170k), that
is about 0.8 MB of stack per average device, and GC scans every one of
them each cycle (`scanstack` + `markroot` 1.5–2.3%).

**Per window client (about 18, permanent unless noted)**, connect paths:

| Goroutines | Started at | Wakes when idle |
|---|---|---|
| `Client.run` receive loop, loopback | transfer.go:2376, 4490 | each inbound frame |
| contract-stats epoch, contract expiry, network-quality worker | transfer_contract_manager.go:702; util.go:254 | 1 s epoch after a byte-count change, about 3 wakes per cping; expiry only while queues are non-empty |
| SendSequence to the provider (idle exit 300 s) | transfer.go:6180 | each packet and ack; kept alive by cpings |
| ReceiveSequence and its ack worker (idle exit 120 s) | transfer.go:14800, 15836 | each echo, plus a 10 ms ack-compress wait; kept alive by the provider's echoes |
| `PlatformTransport.run`, `runH1`, and per connection: watch, inactive drain, writer, reader | transport.go:1865, 2235, 2753 (2767, 2932, 2943, 3239) | writer's ping timer, about every 10 s on the 5 s grid; reader on the server's pings; 30 s read deadline |
| `detectBlackhole`, cping loop, extender-IP watch, client-done waiter | ip_remote_multi_client.go:16568, 16574, 14394, 14400 | blackhole parks with a 10 s backstop (1.25 s while evidence is pending); cping as below |

Not started for hosted window clients: the control ping
(`ControlPingTimeout` 0), the provide ping, encryption sessions (off unless
post-quantum is required), and WebRTC.

**Per device (about 49):**

| Goroutines | Count | Wakes when idle |
|---|---|---|
| `ContractOriginNotifications`: 16 buckets + 2 publishers (localclient/authority.go:85 → model/contract_origin_notification.go:152, 156) | 18 | parked until a companion-origin watch starts a bucket; a started bucket then holds a Redis socket and wakes about once a second (1 s receive timeout, then PING) for the device's life |
| gVisor TCP processors, one per GOMAXPROCS (gvisor tcp/protocol.go:626) | 4 | none |
| multi-client: removal, flow reaper, event epoch, heartbeat, prober | 5 | event epoch 1 s after a change; heartbeat 60 s; prober scans at least every 60 s |
| per window (×2): enumerate, resize, send-stall watchdog | 6 | resize every 15 s |
| monitor callbacks, contract-status workers, identity state | 6 | event-driven |
| sdk: token manager, extender watch, `runContractStatsFlush`, hosted RPC accept | 4 | flush 1 s until settled; token at most every 5 min |
| `ProxyDevice.Run`, idle watcher (`time.After` 1 min), hosted `ClientStrategy`, LocalUserNat, IpAssoc, DMCA scan | 6 | tun reads; 1 min; IpAssoc 1 s only while dirty |

**Wakes.** An idle device wakes about 4.3 times/s, so a process with 1.5k
devices wakes about 6.5k times/s:
- the cping chain, about 13 goroutine wakes per round trip, is 2.2/s;
- contract-stats epochs 0.8/s;
- H1 keepalive 0.8/s;
- the blackhole backstop 0.34/s;
- resize 0.13/s.

These wakes drive most of `findRunnable`/`stopm` (12%) and the 38k context
switches/s per process.

**Cadences, corrected.**
- The cping rest is rounded up to the next 10 s wall-clock boundary after
  the ack arrives (`resetWakeupTimer(pingTimer, 10 s, 10 s)` →
  `WakeupTime`, wakeup_schedule.go:23-35), so each channel pings every
  ~20 s, always on a 10 s boundary. About 255 cpings/s per process, half the
  channels on each boundary.
- Platform pings are on the 5 s grid with offset 0.

Both explain the spikes in §2.6.

**Per-device goroutine budget.**
- An Auto device costs about 160 goroutines today.
- One shared `ContractOriginNotifications` (§4 #5) takes 18 off every
  device.
- An idle tier (§4 #2) takes about 18 off for each carrier it closes.
- Neither touches a detection bound.

#### 3.2 Timers

- `timers.check` is 6.2–6.3% inclusive in all three profiles, and
  `timers.adjust` 3.7–4.8%. On 10-30 (CPUOPT2 §9.1) it was 2.6% `adjust`
  self and about 5% for all timer functions, so the timer share did not
  fall with this release. Its size follows the number of live carriers and
  channels, not traffic.
- Timer-arming frames by first application caller (A): `detectBlackhole`
  (`maybeAdd`, `Reset`, `modify`), `SendControlWithCtx` (a
  `context.WithTimeout` and a `WithDeadlineCause` per control call), go-redis
  pub/sub health checks, the sdk's 1 s `runContractStatsFlush` per device,
  `ResilientTlsConn.SetReadDeadline` per carrier read, the receive-sequence
  ack worker, `RedisAuth` deadlines, `sequenceDeadlineTimer.armBy`,
  `IpAssoc.run`, `ContractManager.runContractStats`, `TakeContract` and
  `expireQueuedContracts`, and `pingPacer.arm`.
- The CPUOPT-KEEPALIVE restorations (native per-operation deadlines, prober
  and multi-client wakes) are in this build. They are correctness, so the
  remaining levers are fewer carriers and channels, and the de-alignment in
  §4 #7, not looser deadlines.

#### 3.3 WireGuard

No longer a driver. userwireguard is 0.8–1.5% of samples. Started peers
average 18 per process (`wg_device_peers`) against 41k configured, so the
on-demand peers removed the registration-driven goroutines, queues and
timers that CPUOPT2 §8.2 measured at 82.6k goroutines and 1.5 GB per
process. 343 WireGuard sessions were active fleet-wide, moving 2.9 MB/s.
What remains is the 15 s `PeerStatuses` sample over the configured table
(server/proxy/wg_sessions.go:122-138, server.go:238) and the 5 s runtime
metrics loop (server.go:952-964).

#### 3.4 FindProviders2 in process

- 2.5–2.9% of process CPU, down from 17.8% on 10-29. gob decoding is 0.7%.
- 85.8 calls/s fleet-wide (4.3/s per process). The decoded score-page cache
  hits 91.7% (471 hits/s, 43 misses/s) and holds 67 MB per process.
- Wall time per call: hard exclusions 22 ms, primary load 22 ms, match
  recording 4.8 ms; mostly Redis waits.
- **The new city fallback adds no reads here.**
  `urnetwork_findproviders2_location_fallback_total` is 0/s for every level
  at service=proxy. Hosted windows ask best-available or country targets,
  and those have no level above them (network_client_location_fallback.go,
  package comment). A climb happens only for an empty city or region
  target.
- The call rate follows window-client creation (discovery for each
  candidate), so it falls with §4 #1.

#### 3.5 Contract and transfer loops, and window-client churn

**The steady loops.** Every window client keeps, at zero user traffic:
- a platform ping each way every 10 s (`pingPacer`, 5 s epoch);
- a channel ping (cping) to its provider about every 20 s: a 10 s rest after
  the ack, rounded up to the next 10 s boundary
  (`multiClientChannel.ping`, ip_remote_multi_client.go:19799-19900). It is
  a real transfer through a contract, the exchange and the provider, with
  acks on both sides;
- `detectBlackhole`, parked when idle behind a 10 s backstop (1.25 s while
  evidence is pending), and the client's contract-stats and sequence timers;
- one `PeerAudit` per 60 s of receiving (§3.6).

That is about 255 cpings/s per process (about 13 goroutine wakes each) and
roughly 0.4–0.6 carrier frames/s per transport. Those loops, not user bytes,
are the 37% "keeping windows up" group: `SendSequence.Run` 7–8%, the
receive side 6–8%, carrier I/O 12%. The cadences are detection bounds (a
dead idle client is found within rest + ack wait). So under the owner rule
the levers are fewer window clients and cheaper mechanics, not slower
pings.

**Contracts.** At service=proxy, fleet-wide:
- 334 contracts/s opened;
- `create_contract` 335/s (27.7 ms mean residence), `provide` 265/s
  (19.7 ms) and `close_contract` 260/s (11.2 ms), plus 43/s
  "already closed";
- 1,049 Redis contract-reservation operations/s;
- 278 pg acquires/s per process.

In process CPU: `CreateContract` 3.0%, `Provide` 1.1%, `CloseContract`
0.7%. Contract opens track window-client creation (1.25 per creation), not
user traffic.

**Churn: most creations are probably discarded candidates.**
- Hosted mints are 269/s and retirements 265/s fleet-wide (JWT record
  checks with operation=retire; the 4.6/s state-query counter counts only
  record-cache misses). That is 6–31 creations per device-hour: 6 off-peak,
  about 30 at the peaks. A transport therefore "lives" 6–15 minutes against
  a 60-minute `MaxClientLifetime`.
- Hosted windows use `StrictWindowSizeHardMax` with no standing reserve
  (sdk/mobile_memory_policy.go:535-536).
- When a client passes its lifetime (0.75–1.0 × 60 min after its first
  event), resize only warns it: "client drain" (connect
  ip_remote_multi_client.go:13565-13606). The warning makes the size math
  see a hole and expand.
- Expansion runs discovery, mints an identity, registers Provide, dials a
  TLS transport, sends an evaluation ping and opens contracts. Then
  `strictWindowAdmissionAllowed` refuses the candidate, because the drained
  client still holds its slot: "The candidate was fully constructed but
  never made visible to selection" (14326-14341, 15174-15187). The candidate
  is cancelled and retired.
- A flowless drained client is never collapsed, because collapse acts only
  over the hard max (13769-13836). So the window stays one-short forever and
  repeats at the discovery-expand backoff of 10 s rising to 1–2 minutes:
  30–60 discarded candidates per stuck window-hour.
- Probe-silence and brief-unhealthy warnings enter the same loop. Probe
  answers count as receives with no counted sends, so an answered probe pass
  can read as receive-only, which is unhealthy, for up to ~30 s
  (20429, 20574, 20793-20796).
- **Under-filled windows do the same.** A window below its minimum whose
  expansions admit nothing retries on the same discovery backoff, 10 s
  rising to 2 minutes (ip_remote_multi_client.go:323-324). Each retry builds
  a complete client before it learns that the candidate is unusable.
- **The numbers fit.** 13.5 creations/s per process is 30–60 per hour from
  800–1,600 looping windows out of about 3,000 (two windows per device). The
  rate climbs at the peaks, when more long-lived active devices hold windows
  older than their lifetime.
- **Unproven part:** hosted devices run with `DisableLogging`, so the
  `expand_decline` events are never written, and no metric counts them. The
  first step of §4 #1 is a counter by decline reason and removal cause.
- **Cost:** the lifecycle group is about 19% of proxy CPU (TLS dial 6.3%,
  contracts and Provide about 4.5%, the authenticate share of those calls,
  mint 1.4%, FindProviders2 2.9%, client construction and teardown 2.4%, and
  `morestack`). Beyond the proxy, every candidate costs pg a network_client
  allocation and its retirement, Redis an escrow reservation, and a services edge a
  TLS handshake, a resident and a forward.

#### 3.6 PeerAudit: 71% of control calls are rejected

- `SequencePeerAudit.Complete` (connect transfer.go:18781-18811) sends a
  `protocol.PeerAudit` frame to the control destination through
  `ClientOob().SendControl`, with a callback that ignores the result. A
  receive sequence completes one audit every `MaxPeerAuditDuration` (60 s,
  transfer.go:1093) while it receives, and one when it exits
  (transfer.go:15816). `Client.ReportAbuse` (transfer.go:3254) uses the same
  path.
- The server has no handler for it. `ConnectControlFrames` falls to
  `default: "Cannot handle oob control message: %T"`
  (server controller/connect_controller.go:552); no server package names
  `PeerAudit`.
- Measured: `urnetwork_connect_control_frame_failures_total{message="other",
  cause="unhandled_message"}` is 2,255/s at service=proxy (71% of its 3,163
  control frames/s) and 2,731/s at service=api. It was 1.0–2.5k/s on every
  day back to 10-25.
- Each one costs, in the proxy: a protobuf marshal, a goroutine with a
  `context.WithTimeout`, `localclient.Authority.ConnectControl`, a full
  `authenticate` (JWT parse with a P-256 verify, plus the live state check;
  `jwt_state_record_checks{operation="control"}` 2.9k/s), a frame decode,
  the error formatting and classification (`fmt.Errorf`,
  `strings.ToLower(err.Error())`), three labelled metric updates, and a JSON
  round trip of the result (`localclient.convert`).
- About 65% of `authenticate` calls are these, so they are about 2.5–3% of
  proxy CPU (about 0.3 cores fleet-wide), plus the client side
  (`SequencePeerAudit` 0.35%).
- **Functional note:** abuse reports (`ReportAbuse`) go nowhere either.
  That is a product gap to decide on, separate from the CPU fix.

### 4. Ranked opportunities

Savings are shares of proxy CPU at the evening peak (12–13.5 cores
fleet-wide) unless stated. They overlap: #2 and #10 remove standing window
clients, and #1 removes discarded ones, so each shrinks the others. Do not
add them.

#### #1 Stop building window clients the strict hard max will refuse

- **Evidence:** §3.5. 269 mints/s and 265 retirements/s fleet-wide; 6–31
  creations per device-hour; the lifecycle work is about 19% of proxy CPU.
  The code path that builds and then discards candidates while a drained or
  warned client holds its slot is verified at the release revision.
- **Change** (connect ip_remote_multi_client.go):
  1. Count `expand_decline` by reason and window-client removals by cause in
     a bounded metric the proxy exports. Hosted logging is off, so this is
     the only way to see it.
  2. Check strict capacity before constructing. Make the
     `strictWindowAdmissionAllowed` test (15174-15187) before discovery,
     mint, Provide, dial and contracts, not after (14326-14341). Then an
     expansion that cannot be admitted costs nothing.
  3. Make the drain hand-off work under the strict ceiling: let a drained
     client with no flows give up its slot when its replacement is ready,
     or admit one replacement above the ceiling until the drained client's
     flows finish. The code already admits a same-identity replacement at
     the ceiling.
  4. If the counter shows under-filled windows dominate, make a failed
     expansion cheaper before making it rarer. The code already parks
     never-connected identities for up to 5 minutes; extend that to the
     transport, so a retry does not mint and dial again.

  Step 2 alone stops the strict-mode waste. Step 3 makes lifetime rotation,
  which is a no-op under strict mode today, do what `MaxClientLifetime`
  intends.
- **Correctness:** no timeout, deadline, ping or detection bound changes.
  Step 2 removes work whose result is always discarded. Step 3 restores the
  intended rotation; keep the strict ceiling's memory bound by counting the
  overlap client against the device's carrier budget, as make-before-break
  migration already does.
- **Saving:** 10–19% of proxy CPU (1.2–2.5 cores at the peak), depending on
  how much of the churn the counter attributes to this loop. It also takes
  about 265 mints, retirements, TLS handshakes and contract open/close pairs
  per second off pg, Redis and the services edges.
- **Risk:** low for step 2; step 3 needs the composed window tests (strict
  window at hard max with a drained flowless client; a drained client with
  live flows).
- **Validate:** hosted mints per device-hour fall from 6–31 to the rotation
  rate, at most about 4 (3.4 carriers rotating every 45–60 minutes);
  `create_contract` and `provide` frames fall in step; the x509 and
  `AuthNetworkClient` shares in perf fall; carriers per device stay the
  same.

#### #2 Idle devices: an idle tier, or a shorter hold

- **Evidence:** §2.3. 71% of live devices had no user traffic in the last 2
  minutes; active devices add neither CPU nor transports in the fits. Each
  live device holds its full window set (up to 5 + 1 carriers, each with
  cpings, platform pings, sequences and blackhole loops) for 45 minutes
  after its last user packet (server/proxy/proxy_device.go:1064, the reaper
  loop at 702-711 and `CancelIfIdle` at 2096-2108).
- **Options:**
  1. **Idle tier:** after N minutes without user traffic (N ≥ 10, to avoid
     flapping), switch the device to a one-client window with the runtime
     `DeviceLocal.SetPerformanceProfile` (sdk/device_local.go:2573), and
     restore the provisioned profile on the first user packet (a flag set
     in `UpdateActivity`, applied off the packet path). Window identities
     persist (PROXYDRAIN1.md §3.5), but per the code study identities are
     reused only from the generator's startup snapshot and short parks, so a
     restore mints its new clients.
  2. **Shorter hold:** 45 → 20 minutes.
- **Saving** (from the fits plus a simple arrival model with a 16-minute
  mean active span; *inferred*):
  - idle tier after 10 minutes: about −39% carriers and −20–30% proxy CPU;
  - idle tier after 5 minutes: −24–35%;
  - 20-minute hold: about −41% devices and −30–40% proxy CPU, before the
    cost of re-creating devices that come back between 20 and 45 minutes.
- **Correctness:** these are retention policies, not detection bounds. No
  ping, deadline or blackhole cadence changes for a device that keeps its
  windows. The cost is resume latency: the first request after idle waits
  for discovery, mints and contracts (several seconds) instead of finding
  six warm exits. That is a product decision.
- **Risk:** medium. Flapping between tiers would add churn, so do #1 first
  and measure the restore cost.
- **Validate:** carriers per device by idle age; time to first byte after
  resume; mints per resume.

#### #3 Cache verified certificate chains for platform dials

- **Evidence:** `x509.Certificate.Verify` is 5.3% of process CPU (D), under
  `clientTlsHandshaker.handshake` → uTLS `readServerCertificate` →
  `verifyServerCertificate`. P-384 field arithmetic is 4.6% and RSA 0.5%.
  The rest of the handshake (key exchange, the server's CertificateVerify,
  finished) is under 0.3%. crisp makes 170 outbound TCP connections/s for
  13.5 new window clients/s per process, and the verify cost (about 2.5–3 ms
  each at 0.77 cores) matches about one full verification per dial.
  Resumption is not helping: connect already keeps a uTLS session cache per
  dial path (net_tls_hello.go:186-248), but every edge is dialed with the
  same platform server name, so a ticket from one edge's nginx is offered to
  the next edge, which cannot use it unless the edges share ticket keys
  *(inferred)*.
- **Change, either:**
  1. a process-wide cache of verified chains in connect's TLS config: key
     the SHA-256 of the presented chain DER plus the roots generation; on a
     hit, re-check the leaf's validity window and the server name, skip path
     building and the chain signatures; or
  2. key the uTLS session cache by (server name, dialed edge address) in
     `chromeSessionCache` (net_tls_hello.go:194-248), so each edge's ticket
     is resumed by later dials to that edge.

  Option 1 is client-only and deterministic.
- **Correctness:** both keep server authentication. Chain validity is a
  pure function of (chain bytes, roots, time), and the cache re-checks time
  and name on every use. The per-handshake CertificateVerify signature,
  which proves possession of the leaf key, is still verified each time.
  TLS 1.3 resumption is the standard mechanism and still runs (EC)DHE. No
  timeout or detection bound changes.
- **Saving:** 4–5% (0.5–0.6 cores). It also takes full handshakes off the
  services edges' nginx with option 2.
- **Risk:** low. A stale cached chain past NotAfter is rejected by the
  per-use time check.
- **Validate:** `x509.(*Certificate).Verify` share in perf below 0.5%; cache
  hit rate; no change in carrier handshake failures.

#### #4 Stop sending PeerAudit to the control

- **Evidence:** §3.6. 2,255 rejected frames/s at the proxy and 2,731/s at
  the api; the controller has no handler.
- **Change:** in connect, do not send from `SequencePeerAudit.Complete`
  (transfer.go:18806) unless a setting enables it (default off), or batch
  audits. Alternatively, the server's `ConnectControlFrames` accepts
  `*protocol.PeerAudit` as a no-op next to `ControlPing`
  (connect_controller.go:540). That removes the error path but still
  authenticates each call. The client-side change removes all of it.
- **Correctness:** today the frame changes nothing on the server, and the
  client ignores the result. The audit data is lost either way until a
  handler exists.
- **Saving:** 2.5–3% of proxy CPU (0.3 cores). It also removes about 2.7k
  control requests/s from the api, whose authentication CPUOPT2 §2.5
  measured at 11–21% of api CPU.
- **Risk:** very low. Client releases reach apps slowly; the server-side
  no-op helps immediately.
- **Validate:** `urnetwork_connect_control_frame_failures_total{cause=
  "unhandled_message"}` → 0; `jwt_state_record_checks{operation="control"}`
  falls by about 75%.

#### #5 One ContractOriginNotifications per proxy process

- **Evidence:** every hosted device's `localclient.Authority` builds its own
  `model.NewContractOriginNotifications` (localclient/authority.go:85). Each
  one starts 16 bucket goroutines and 2 publishers
  (model/contract_origin_notification.go:23, 80, 149-157), and each bucket
  may hold its own Redis shard subscription. api (api/run.go:300), connect
  (connect/resident.go:900) and taskworker build one per process.
- **Change:** the ProxyDeviceManager owns one, passed through
  `AuthoritySettings` as `MintAdmission` and `ContractAdmission` already
  are; or start a bucket goroutine only when its bucket is first needed.
- **Correctness:** the same registry semantics as api and connect; waiters
  are keyed by contract origin, not by owner. Close order moves to the
  manager.
- **Saving:** 18 of the 52 per-device goroutines: about 27k goroutines and
  about 0.2 GB of stacks per process (about 16% of each), less GC stack
  scanning, and at most 16 subscriptions per process instead of up to 16 per
  device. CPU 1–2%; the memory matters more (§2.5 headroom).
- **Risk:** low. The bounds (`MaxWaiters` 8192, publish queue 1024) become
  process-wide; size them from the peak of concurrent contract creations.
- **Validate:** goroutines per device in the fit drop from 52 to about 34.

#### #6 Cache verified hosted JWT claims

- **Evidence:** `authenticate` is 3.3–3.7%: `ParseByJwtForAudience` 2.2%
  (P-256 verify 1.2%) and the live state check 1.5%. A window client
  presents the same token on every control call for its whole life.
- **Change:** in `localclient.Authority.parseClaimsPreflight`
  (authority.go:125-153), memoize the parsed and signature-verified claims
  by token string in a small LRU keyed with the keyring generation. Keep the
  lifetime checks (`validateSessionHorizon`, expiry) and
  `ValidateByJwtState(ForParent)` on every call.
- **Correctness:** signature verification of identical bytes under the same
  keys is deterministic. Expiry and revocation stay per call.
- **Saving:** about 1.5–2% (more if #4 does not ship).
- **Risk:** low; flush on key rotation.

#### #7 Spread the pings

- **Evidence:** §2.6. All ten processes on a host pass one core at the same
  100 ms, 20 of 24 cores busy, every 10 s; PSI cpu-some 16–18% at 31% busy.
- **Change:** set `PlatformTransportSettings.PingJitter = PingTimeout` for
  hosted devices. It exists in connect (transport.go:554-562, 1775) but is
  unwired: add an sdk `DeviceLocalSettings` field and set it in
  `newProxyDeviceLocalSettings` (server/proxy/proxy_device.go:1380). Give
  the channel pings the same per-channel offset
  (`resetWakeupTimer` → `wakeupTimeWithOffset`, ip_remote_multi_client.go:19890).
- **Correctness:** each carrier and channel keeps its cadence (an offset
  below one period, drawn once), so ping intervals, read deadlines and
  dead-peer bounds are unchanged. Phones keep wall-clock alignment.
- **Saving:** 1–3% (less scheduler contention and spinning in the spike);
  the main gain is PSI and tail latency for user traffic during the spike.
- **Risk:** low.
- **Validate:** B's phase table flat (p99 host cores near p50); PSI
  cpu-some below 5%.

#### #8 GOMAXPROCS 4 → 3 on one block

- **Evidence:** the scheduler is the largest single runtime cost:
  `runtime.mcall` root 13.3%, `findRunnable` 12.4% (`stealWork` 3.5%,
  `stopm` 2.0%), futex 3.0%, 38k context switches/s per process. Ten
  processes at 4 Ps are 40 Ps on 24 cores (16 of them E-cores). A process
  averages 0.5–0.77 cores but bursts to 4.0 in the ping spike (p99 of 100 ms
  bins).
- **Change:** `cores: 3` for one block on each host, after #7 has flattened
  the spike.
- **Correctness:** no timing bound changes. Burst work queues behind 3 Ps
  instead of 4.
- **Saving:** 2–5% if idle-P hunting drops as expected; measure, don't
  assume.
- **Risk:** burst latency. Compare `urnetwork_proxy_session_interval_max_seconds`
  and the WireGuard return backpressure histogram on the A/B block.

#### #9 Memory limits and GOGC

- **Evidence:** §2.5. RSS is pinned at 95% of GOMEMLIMIT at every peak; GC
  is still GOGC-paced; the limit governs from about +20–30% transports.
- **Recommendations:**
  - Keep GOMEMLIMIT 6500 MiB on fireside (18 GiB available at the peak).
  - On crisp (50 GiB available), raise GOMEMLIMIT to 8 GiB under the
    unchanged 10 GiB cgroup. It is CPU-neutral today and buys headroom
    against the next wave.
  - GOGC 200 on crisp only would halve GC cycles: about −3–4% of crisp's
    proxy CPU for about +1.5 GB per process. Do it after #5 and #1 shrink the
    stacks.
  - Apply on fireside one block at a time (full-overlap restarts need about
    6 GiB more per block).
- **Correctness:** none of these change any timing bound.
- **Risk:** RAM; a cgroup kill redeploys the block.

#### #10 A smaller hosted quality window

- **Evidence:** a full hosted device holds quality 5 plus speed 1 carriers,
  and the daily waves are exactly such devices (marginal 5.94 carriers per
  device). CPUOPT2 §8.6 #5 proposed capping hosted windows; it was not done.
- **Change:** hosted-only override in `newProxyDeviceLocalSettings`
  (server/proxy/proxy_device.go:1380) or the sdk policy
  (sdk/mobile_memory_policy.go:503-559): quality 5 → 3.
- **Correctness:** no timing bound changes. It is a redundancy and
  throughput trade-off for wave devices.
- **Saving:** about −23% carriers and 10–15% proxy CPU at today's mix
  (*inferred*; it overlaps #2).
- **Risk:** fewer racing exits per cold CONNECT, and less failover depth.

#### Smaller items seen in the profiles

- `localclient.convert` JSON-encodes and decodes every control and
  FindProviders2 result (0.2–0.4%; authority.go:177-185). Pass the typed
  result.
- go-redis `PubSub.Channel` goroutines are 1.6% of roots, each with a 3 s
  health ping. Separately, each started contract-origin bucket holds a shard
  subscription that PINGs about once a second (§3.1). Count both per
  process; #5 bounds the second at 16.
- Each process keeps about 1,008 Redis pool connections, 1,007 idle: 20k
  connections from the proxies to the cluster, for 525 commands/s per
  process.
- `morestack`/`copystack` at 1.5% is new goroutines growing their stacks;
  it falls with churn (#1).
- `rtl8127_rx_interrupt` at 2–3.4% of samples: RPS on the single-queue
  Realtek NIC (CPUOPT2 §8.6 #10) is still open.
- Dial errors (32–39/s per host, the brief's 31–40) are now real upstream
  failures: 41% of HTTP admissions end in `connect_dial_error`. With bounded
  CONNECT they are cheap, but with `MultiRaceClientCount` 0 each cold CONNECT
  races every exit in a quality window. Worth a quality look, not a CPU
  one.

### 5. What CPUOPT2 predicted, and what happened

| CPUOPT2 item | Prediction | Observed |
|---|---|---|
| §8.6 #1 on-demand WireGuard peers | −1.0 GB heap, −0.45 GB stacks, −80k goroutines per process; GC from about 1.3 to 0.3 cores per process | Materialized. 189k → 84k goroutines and 4.9 → 2.6 GB heap at about 1k devices (10-30 14:00Z); 18 started peers per process; GC from 14.6 to 0.6–1.3 cycles/min |
| #2 memory limit up | about −1 core per process | Superseded by #1. Now GOMEMLIMIT 6500 MiB and a 10 GiB cgroup; RSS at the 95% plateau each peak |
| #3 bounded CONNECT | ends the 29k/s storm; 5–15% CPU | Materialized. Dial errors 29k/s → 71/s fleet; ingress connections about 1,100 → 90–125 per process |
| #4 FindProviders2 page cache | −10–14 points | Materialized. 17.8% → 2.5–2.9%; 91.7% hits; 67 MB per process (predicted 64 MB) |
| #5 cap hosted windows (quality 3, speed 0) | prevents a 2.7× rise in per-device heartbeats, channel pings and FindProviders2 as windows refill from 2.25 to 6 | Not done, and the rise materialized: transports per device swing 1.5–4.0 daily, and marginal devices bring 6 |
| #6 idle hold 90 → 30–45 min, DoH keepalives not activity | −0.5–1 GB per process, about −25% background work | Done (45 min, user traffic only, CPUOPT2 timeline 10-30) |
| #7 per-device loops | −14k timers/s, −34 goroutines per device, 3–6% | Partly. Allocation-free loops shipped; parking of prober and multi-client wakes was reverted for correctness (CPUOPT-KEEPALIVE). Timer share is 6% (5% on 10-30) |
| #9 GOMAXPROCS 8 → 3–4 | fewer GC workers | Done (4); the scheduler is now the biggest runtime cost (§4 #8) |
| §9.3 connect timer changes | most of the remaining ~5% timer CPU | Not materialized. The release carries connect 89b15920 with the correctness restorations; timers are 6.2% |
| §3.9 proxy CPU per device | −30–50% | Exceeded per unit: 1.55 → 0.37–0.40 mcores per device and 0.39 → 0.11 mcores per transport (10-29 saturation vs 10-31 peaks). Absolute proxy CPU at the peak is 11–13.5 cores, against 46 on 10-29 |

### 6. Validating

- **Use per-process numbers.** `proxy_canary.py` sums both hosts per block.
  Compare blocks on mcores per transport, goroutines per transport and per
  device, mints/s per 1k transports, and GC cycles/min, not on raw cores:
  the daily waves move transports by 3×.
- **perf per change** on one A/B block (perf5.sh style): the shares of
  `x509.Verify` (#3), `authenticate` and `SendControlWithCtx` (#4, #6),
  `findRunnable` and `stopm` (#8), and the 100 ms phase table (#7).
- **Counters:** control frames by message and outcome, hosted mints,
  contracts opened, `urnetwork_proxy_platform_transports_used`, PSI
  cpu-some, RSS against 95% of GOMEMLIMIT at the next peak.
- **User-facing guards for #1, #2, #8, #10:** session interval max,
  `connect_timeout` and `connect_dial_error` per admission, WireGuard return
  backpressure, and time to the first byte after an idle device resumes.

## Part C — api, alt and taskworker

Read-only measurement of Main on 10-31, 18:20–19:25Z, against the releases
deployed that morning: api `1086374610`, alt `1086375600`, taskworker
`1086372490`, all from server main `a8461e236`. Code references are to that
commit. Connect-library references are to connect origin/main `c62b060e`;
the deployed binaries embed the connect tree they were built with, so those
line numbers are approximate. *(inferred)* marks a claim reasoned from code or
arithmetic rather than measured.

### Summary

**Size of the domain.** On 10-31 api averaged 13.0 cores (17.4 at the
evening peak) and taskworker 11.5 (13.2). Together that is about 17% of
services-edge busy CPU (140 cores mean, 173 peak). alt is 0.4–0.5 cores on
crisp and fireside.

**api.** Load is now about 12.5k req/s at peak, and 83% of it is `POST
/connect/control`. Only about a fifth of those control requests do real work:
- 53% are refusal replays, answered after JWT verification and before the
  state query (5.5k/s now; 7.3k/s just before that morning's connect
  deploy). They follow the connect uptime sawtooth.
- 25% (2.6k/s, steady for 10 days) carry only a `PeerAudit` frame. The api
  has never handled `PeerAudit` (no reference in controller, model or api),
  and the client ignores the answer. Each one still pays for a P-256 JWT
  verification, two Redis round trips and the error path.
- About 20% go through `CreateContract`, plus 2% close or provide.

Per request, the costs are dominated by plumbing:
- JWT handling is about 19% of api CPU: the signature verify, a second
  unverified claims decode in the lag-guest gate, and P-256 assembly samples
  whose stacks are cut off.
- Redis admission and the revocation-record check are 8–9%.
- The scheduler is 13%.
- GC is about 5%, time-averaged. CPUOPT2's 20% figure came from a short
  sample that happened to include a GC cycle.

**taskworker.** The provider egress probe now uses at least 55% of
taskworker CPU, and x509 chain verification alone takes 17%. Each probe
tunnel builds a fresh `connect.ClientStrategy` and makes about four new TLS
connections. Each one verifies the Let's Encrypt YE chain, which costs about
four P-384 ECDSA verifications at 6–10 ms of CPU per handshake. Ordinary
tasks are about 7% of taskworker CPU. `CloseExpiredContracts` costs about
nothing on taskworker (0.00–0.08% of samples); its cost is in the database.

**alt.** Its time splits into roughly equal quarters:
- quic-go, about 26%;
- Redis pub/sub reception, about 26%. Origin notifications are 7–8% of
  that, and go-redis `Channel()` readers about 16%;
- scheduler and GC, about 25%;
- the api itself, 6–7%. 87% of alt's `/connect/control` requests are
  `PeerAudit`.

**Ranked opportunities.** Savings are at the 10-31 evening load. They overlap
where noted, so do not add them up.

| # | Change | Service | Est. saving | Risk | Effort |
|---|---|---|---|---|---|
| 1 | Answer `PeerAudit`-only control requests without the auth, limit and state path; stop the client sending them | api, alt | 1.4–1.7 cores (8–10% of api) answered before auth, or 0.7–0.9 after the signature check; −5.2k Redis commands/s | low | S |
| 2 | Cache verified JWT claims per token; the lag-guest and intended-network gates read the cache | api | 2.2–2.9 cores at peak (13–17% of api); overlaps #1 by about 0.6 | low–medium | S–M |
| 3 | Process-wide verified-chain cache in connect's TLS handshaker | taskworker (and every connect client) | 2.3–2.5 cores (15–17% of taskworker) | low | S–M |
| 4 | Replace the origin-notification broadcast with a version check when a missing-origin refusal is replayed | api, alt | about 1.0 core, plus about 27k Redis pub/sub deliveries/s | medium | M |
| 5 | One control-plane `ClientStrategy` per probe process (keep-alive and TLS resumption) | taskworker | about 0.6 cores, plus fewer server-side handshakes; most of #3 again if #3 is not done | medium (probe isolation review) | S–M |
| 6 | `encodeUuid` and `parseUuid` without fmt or string concatenation (connect `id.go`) | all | about 0.25 cores api, 0.1 taskworker, more in connect *(inferred)* | very low | XS |
| 7 | Stop compiling a regexp on every `updateFunctionName` call | taskworker | 0.1–0.15 cores | none | XS |
| 8 | Pre-bound Prometheus children for constant labels on hot paths | api | about 0.2 cores | low | S |
| 9 | api GOMAXPROCS 8 → 4, A/B on one block | api | 2–5% of api *(inferred)* | low–medium | XS |
| 10 | Pointer-free in-memory search indexes, which are likely most of the 3 GB live heap; GOGC alone is worth only about 1% | api | 0.4–0.6 cores and about 50 GB RSS fleet-wide *(inferred, needs a heap profile)* | medium | M |
| 11 | Merge `CreateContract`'s pre-transaction reads into one round trip | api | 0.3–0.4 cores plus latency *(inferred)* | low–medium | M |
| 12 | Fewer api processes; fixed overhead is 0.13–0.22 cores per process | api | 0.1–0.15 cores per process removed | low | topology |
| 13 | Larger claim batches for `ApplyLegacyProviderTotals` (about 620/s now) | taskworker | 0.3–0.5 cores while the backlog lasts *(inferred)* | low–medium | M |

**Priorities.** #1, #2, #3, #6 and #7 are the clean wins. Together they take
about a quarter of the domain (about 6–7 cores) with no timing or detection
bound involved. The largest variable load on the api is upstream: the
contract-refusal sawtooth in connect drives 5–7k replays/s. See §5.

### 1. Method

**Window and load.** Measured 10-31, 18:20–19:25Z, the evening peak:
- api at 12.5k req/s (the 10-31 maximum was 13.7k) and 17.6 cores;
- services-edge busy about 170 of 312 logical CPUs;
- taskworker at 12.2 cores (1 h average), 15.3 during the fleet capture;
- 61.5 probe attempts/s.

**Mimir** (read-only, through the tunnel):
- `process_cpu_seconds_total` by service, host and process, on a 30-minute
  grid from 10-21 to 10-31 (CPUOPT2's window was 10-22 to 10-29);
- Go runtime gauges;
- `urnetwork_http_requests_total` and `urnetwork_http_request_duration_seconds_{sum,count}`
  by route. Duration buckets are not ingested, so there is no per-route p99;
- the control-path counters `urnetwork_connect_control_*`, `urnetwork_session_*`,
  `urnetwork_jwt_state_*`, `urnetwork_redis_contract_reservation_total`,
  `urnetwork_contract_origin_notifications_total` and
  `urnetwork_connect_companion_origin_*`;
- task metrics: `urnetwork_taskworker_execution_*` and queue depth;
- pool counters: `urnetwork_pg_pool_acquires_total` and
  `urnetwork_redis_pool_hits_total`.

**perf.** Frame-pointer call graphs; each data file was deleted on the host,
and only symbol aggregates were printed.

| Target | Host | Length, rate | Samples | Cores during |
|---|---|---|---|---|
| one api g-block process | edge-3 | 30 s, 99 Hz | 5,177 | 1.36 |
| all 5 api processes | edge-0 | 60 s, 49 Hz | 10,643 | 2.77 (beta 0.17) |
| one api g-block process, callee breakdown | edge-4 | 40 s, 99 Hz | 7,822 | about 2.0 |
| one api g-block process, corrected categories | edge-3 | 40 s, 99 Hz | 7,975 | 1.74 |
| all taskworker processes | edge-1, edge-3 | 60 s, 49 Hz | 9,246; 8,409 | 3.16; 2.64 |
| taskworker, TLS focus | edge-4 | 60 s, 49 Hz | 10,892 | about 2.7 |
| taskworker, owner buckets | edge-0, edge-1 | 60 s, 49 Hz | 9,442; 8,499 | 3.20; 2.92 |
| taskworker fleet, owner buckets, simultaneous | edge-0/1/3/4 | 60 s, 49 Hz | about 36k | 15.3 |
| alt | crisp; fireside | 60 s, 99 Hz | 2,097; 2,394 | 0.30; about 0.4 |

**Other evidence:**
- `perf stat -e syscalls:sys_enter_connect` on edge-4's taskworkers for 10 s.
- `pg_stat_statements`: two snapshots 63 s apart at about 19:01Z, read-only.
- Loki line and byte rates per service over 5 minutes.
- A local P-256 and P-384 ECDSA verify benchmark on an M1 Max with go1.27.3.
- Catalog row estimates for the search tables.

**Production impact.** One `ss -tnp` connection census on edge-4 ran far
longer than intended; `-p` walks every process's file descriptors. I stopped
it, confirmed nothing was left running, and used `perf stat` instead.
Everything else was a short capture or a single catalog or metrics query.

### 2. Where the CPU goes

#### 2.1 Service totals over 10 days (cores; Mimir, 30-minute grid)

| Day | api mean / max | taskworker mean / max | alt mean / max | api krps mean / max | api cores per krps | edge busy mean / max |
|---|---|---|---|---|---|---|
| 10-22 | 11.1 / 12.8 | 9.8 / 12.6 | 0.3 / 0.7 | 4.7 / 5.7 | 2.37 | 199 / 209 |
| 10-23 | 9.9 / 21.0 | 5.2 / 10.3 | 0.2 / 0.3 | 4.0 / 6.2 | 2.48 | 137 / 209 |
| 10-24 | 5.8 / 9.7 | 8.2 / 11.7 | 0.4 / 0.6 | 2.8 / 4.8 | 2.07 | 114 / 185 |
| 10-25 | 12.1 / 16.6 | 7.8 / 14.7 | 0.3 / 0.5 | 4.0 / 5.6 | 3.03 | 168 / 247 |
| 10-26 | 13.1 / 16.7 | 11.0 / 14.8 | 0.4 / 0.5 | 6.0 / 7.2 | 2.20 | 211 / 252 |
| 10-27 | 14.5 / 19.4 | 12.2 / 13.5 | 0.5 / 0.6 | 8.6 / 12.0 | 1.70 | 167 / 228 |
| 10-28 | 10.5 / 14.5 | 11.4 / 12.9 | 0.4 / 0.6 | 5.3 / 7.1 | 1.99 | 149 / 203 |
| 10-29 | 14.4 / 19.0 | 10.6 / 14.5 | 0.5 / 0.7 | 8.0 / 10.9 | 1.80 | 164 / 205 |
| 10-30 | 14.0 / 18.3 | 11.2 / 13.3 | 0.5 / 0.7 | 9.8 / 12.8 | 1.43 | 141 / 169 |
| 10-31 (to 18:25Z) | 13.0 / 17.4 | 11.5 / 13.2 | 0.4 / 0.5 | 9.2 / 13.7 | 1.41 | 140 / 173 |

10-23 and 10-24 include the edge-2 disk-full outage.

**Against CPUOPT2's window** (10-22 to 10-29, outage days excluded):
- api averaged 12.6 cores; it is now 13–14. Requests rose about 56%
  (6.1k → 9.5k/s daily mean).
- Cost per 1k requests fell from about 2.2 to 1.4 cores. That is the mix,
  not a code change: the cheap replays went from about 25% to 53% of control
  traffic. No api CPU change shipped between 10-29 and 10-31; see §4.
- taskworker averaged 10.5 cores and is now 11.2–11.5.

#### 2.2 api processes

There are 20 api processes: beta and g1–g4 on each services edge.
GOMAXPROCS is 8, GOGC 100, and GOMEMLIMIT is unset. Measured over 30 minutes
at 18:35Z:

| Host (CPU) | g1–g4 cores each | req/s each | cores per krps | beta |
|---|---|---|---|---|
| edge-0 (Platinum 8168, 2.7 GHz) | 0.59–0.63 | 392–410 | 1.49–1.54 | 0.17 cores at 38 req/s |
| edge-1 (E5-2697 v4 Broadwell, 2.3 GHz) | 0.80–0.83 | 366–384 | 2.13–2.17 | 0.24 at 38 |
| edge-3 (Gold 6154, 3.0 GHz) | 1.27–1.31 | 1,076–1,128 | 1.15–1.18 | 0.29 at 119 |
| edge-4 (Gold 6154, 3.0 GHz) | 1.37–1.43 | 1,123–1,176 | 1.21–1.22 | 0.32 at 136 |
| **Total** | **17.56 cores** | **12,567 req/s** | **1.40** | |

**Fixed cost per process.** A fit across the g-blocks gives about 0.2 cores
per process at zero traffic plus about 0.96 ms per request. The beta
processes agree, at 0.13–0.2 cores of background. Twenty processes therefore
carry about 2.5–4 cores of fixed cost:
- the origin-notification subscription (§2.4);
- GC of a roughly 3 GB live heap (§2.4);
- `runPosts`, the stats pusher, and idle-P scheduling.

**Imbalance.** edge-3 and edge-4 processes serve about 2.8× the requests of
edge-0 and edge-1 processes.

**edge-1's cores are slower.** edge-1 is Broadwell at 2.3 GHz, and an api
request there costs about 1.8× what it costs on edge-3 or edge-4.

**Memory.** Heap in use is 3.7–5.7 GB and next GC about 6.2 GB, so the live
heap is about 3.1 GB per process. GC runs 0.47–0.98 times a minute.
Allocation is 18–47 MB/s on g-blocks and 2.5–6.6 MB/s on beta.

#### 2.3 api request mix and 5xx

**Top routes now** (30-minute rates):

| Route | req/s | Share | Mean ms | Notes |
|---|---|---|---|---|
| `POST /connect/control` | 10,388 | 83.1% | 10.5 | 8,908/s at 15.5 ms on 10-29 20:00Z |
| `GET /hello` | 1,460 | 11.7% | 0.2 | |
| `POST /network/auth-client` | 166 | 1.3% | 9.4 | 62/s are 4xx |
| `POST /network/remove-client` | 94 | 0.7% | 14.2 | about 60/s are probe identities *(inferred)* |
| `POST /network/provider-egress-attempt` | 61 | 0.5% | 6.1 | from probes |
| `POST /network/provider-egress-health` | 61 | 0.5% | 6.8 | from probes |
| `GET /network/provider-egress-due` | 56 | 0.4% | 26.4 | from probes |
| `GET /network/reliability` | 44 | 0.4% | 10.4 | 40/s 200, 2.6–2.8/s 503 |
| `POST /network/find-providers2` | 21 | 0.2% | 53.5 | about 23 ms CPU per request *(from the profile)* |

**What `/connect/control` carries.** The frame counters break it down
(30-minute rates):
- **`create_contract`, 7,555 frames/s:**
  - 5,541/s are refusal replays: 4,776 `inactive_destination` and 766
    `missing_companion_origin`;
  - about 2.0k/s reach `CreateContract`, and 1,051/s of those go through
    the companion-origin wait.
- **2,601/s `message="other", cause="unhandled_message"`:** this is the
  `PeerAudit` frame, as follows.
  - The connect client sends one through the out-of-band API control on
    every receive-sequence exit, stream change or audit period
    (`SequencePeerAudit.Complete`, connect transfer.go:18781–18810).
  - It ignores the result (`func(resultFrames, err) {}`).
  - The api dispatch has no `PeerAudit` case (controller/connect_controller.go:520–553).
  - The rate holds at 2.0–2.7k/s across all 10 days, independent of the
    replay sawtooth.
- 120 close and 79 provide frames/s.
- 41/s fail authentication (401).

**Time per phase.**
- `authenticate`: 3.4 ms mean, 35 s/s.
- `controller`: 7.1 ms mean, 73 s/s.
- Contract stages: `companion_origin` is 47.7 s/s. That is the escrow
  transaction inside the wait loop, not idle waiting.

**5xx (1 h).** None of these is a CPU matter.
- `/network/reliability`: 2.58/s 503 (by design) plus 0.17/s canceled.
- `/connect/control`: 0.48/s 503, canceled during auth (dependency deadline
  or drain).
- `/transfer/stats`: 0.31/s 503.
- `/key/{id}`: 0.22/s 500, canceled.
- Every other route is at or below 0.15/s.

**429s.**
- `/auth/login-with-password`: 14.1/s, which is 100% of that route; its
  rate limit is refusing everything sent to it.
- `/connect/control`: 5.5/s from the per-client limit.
- `/auth/network-create`: 1.35/s.

#### 2.4 Inside an api process

Shares of process CPU. The main source is the 40 s edge-3 capture at 1.74
cores; the edge-4 callee breakdown and the edge-0 all-process capture
confirm it within ±1.5 points.

| Component | Share | Notes |
|---|---|---|
| JWT: `ParseByJwtForAudience` (ES256 verify plus claims decode) | 10.5–11.7% | session/by_jwt.go:420, 466 |
| JWT: lag-guest gate's unverified claims decode | 4.4–5.0% | router/lag_guest_credential.go:61 → session/by_jwt.go:763 |
| P-256 assembly samples with no app frame (cut-off stacks; JWT verify) | about 4.5% | `p256MulInternal` and `p256SqrInternal` as leaves |
| **JWT total** | **about 19%** | about 225–260 µs per authenticated request |
| Per-client control admission (Redis EVALSHA) | 5.8–5.9% | controller/connect_control_limit.go:103; model/connect_control_limit.go:231 |
| Revocation-record check (Redis GET) | 2.5–2.9% | session/revocation_record.go:331 |
| `CreateContract` | 11.2–13.5% | See the breakdown below |
| `FindProviders2` | 1.8–2.0% | |
| Origin-notification subscription | 3.0% (busy process) to 9.3% (light) | about 0.045 cores per process |
| `runPosts` | 1.0–1.3% | |
| GC mark, time-averaged | 5.1% | 0–8.3% per process; 3–4 core-seconds per cycle |
| `mallocgc` | 6–8% | |
| Scheduler (`findRunnable`) | 13–14% | futex 7.3%, netpoll 3.5%, `stealWork` 2.4% |
| encoding/json (v1, v2, jsontext) | 7.1% | Mostly inside JWT claims decoding |
| go-redis | 12–15% | Includes its syscalls |
| pgx | 6–7% | |
| prometheus client | 2.3–2.6% | |
| fmt | 1.5–1.9% | `connect.encodeUuid` and Sprintf key builders |
| protobuf | 1.2–1.5% | |
| glog | about 0% | |

**Inside `CreateContract`** (edge-4, % of process CPU):
- `nextContract` 6.4%:
  - `waitForCompanionOrigin` 4.6%, which runs the escrow transaction;
  - `CreateTransferEscrow` 0.8%;
  - `AddCompanionContractToStream` 0.6%.
- `GetClientContractMetadata` 2.0%.
- Provide relationship 0.9%.
- `GetProvideSecretKey` 0.5%.

**Inside `authenticate`:**
- `ParseByJwtForAudience` 11.7%;
- `PrepareConnectControl` 7.3%: admission 5.9%, refusal preparation and
  frame decode 1.0%;
- the revocation record 2.5%.

**Estimated CPU per request class.** Bottom-up from the profile, checked
against a regression of api cores on class rates over 2–4 days (R² 0.95–0.97).
The regression pins replays robustly; the other classes are collinear in it.

| Class | Rate | CPU per request |
|---|---|---|
| Refusal replay | 5.5–7.3k/s | about 0.45 ms (regression: 0.40–0.57) |
| `PeerAudit` | 2.6k/s | about 0.6–0.75 ms: replay path plus state check plus error path |
| Real create | about 2.0k/s | about 1.7–2.5 ms |
| `find-providers2` | 21/s | about 23 ms |
| `/hello` | 1.46k/s | about 0.1 ms |

#### 2.5 Redis and database round trips (api)

**Redis.** Pool hits are 25.9k/s, about 2.1 per api request:
- admission: one EVALSHA on every control request (10.7k/s, including
  replays and `PeerAudit`);
- revocation record: one GET per non-replay request (4.4k/s);
- creation path: `MGET` for the provide secret, reservation scripts (about
  1.1k ops/s), stream adds and origin publishes (758/s).

Separately, every api process receives each published origin edge (§3, #4):
24.5k deliveries/s to api and 2.4k/s to alt.

**PostgreSQL.**
- api pool acquires are 10.8k/s: 0.86 per api request, and about 2.4 per
  non-replay control request.
- Fleet-wide `pg_stat_statements`: 120k statements/s and 9.7 execution
  seconds/s, across all services.
- A committed creation is about 10 statements in one transaction. The begin
  and `lock_timeout` already share one round trip (db.go:1022–1037). There
  are 4–6 separate acquisitions before the transaction.
- Top api-attributable statements, each about 1.3–1.4k/s and 0.02–0.45 ms:
  the `transfer_contract` and `transfer_escrow` inserts, the
  allocation/prober CTE, the balance and network-client checks, and the
  `contract_extender` insert.

#### 2.6 taskworker

There are 8 processes (g1 and g2 on each edge), at 1.33–1.93 cores each
(edge-1 is highest) and GOMAXPROCS 4.

**Fleet owner breakdown.** All 8 processes, 60 s simultaneous, 15.3 cores.
Each sample counts once, under the first rule it matches.

| Owner | Cores | Share |
|---|---|---|
| Probe: platform and API TLS, x509 chain verification | 2.59 | 16.9% |
| Probe: other TLS (uTLS, ECDHE, key generation) | 0.58 | 3.8% |
| Probe: in-process control (`providerEgressControl` → `ConnectControlFrames`) | 1.04 | 6.8% |
| Probe: `qualityprobe`, `providertunnel`, `egresshealth` | 1.46 | 9.6% |
| Probe: connect client stack (transfer sequences, Tun, websocket) | 2.76 | 18.1% |
| **Probe subtotal** | **8.43** | **55%** |
| Tasks: `FlushTransferDebits` 0.21, `ApplyLegacyProviderTotals` 0.24, `UpdateClientScores` 0.17, `CloseStrandedGrantContracts` 0.11, `UpdateClientLocations` 0.08, Rollup* 0.05, other 0.18 | 1.04 | 6.8% |
| Task framework (claim, finalize) | 0.55 | 3.6% |
| GC | 0.72 | 4.7% |
| Scheduler | 1.08 | 7.0% |
| Other app frames, runtime and kernel | 3.40 | 22% |

**Probe arithmetic.**
- There are 61.5 attempts/s fleet-wide, about 137 ms of taskworker CPU each
  *(inferred)*.
- edge-4's taskworkers made 67 `connect()` calls/s, about 4.4 TCP dials per
  attempt.
- Each tunnel builds a new `connect.ClientStrategy`
  (qualityprobe/providertunnel/tunnel.go:522), so its TLS session caches and
  HTTP pools start empty. Each tunnel also mints and retires its own client
  identity (`ReuseFixedDestinationIdentities = false`, tunnel.go:536).
- The served chain is leaf ← YE1 ← Root YE ← ISRG Root X2, plus an X2
  cross-sign by X1. Against the pinned X1 and X2 roots, Go's chain builder
  verifies about four P-384 signatures and one RSA signature.
- Go's P-384 is generic fiat code: 1.52 ms per verify on an M1 Max, against
  0.11 ms for P-256. That puts each handshake at about 6–10 ms of CPU
  *(inferred)*, which matches 2.6 cores ÷ about 270 dials/s.

**Tasks by wall time** (24 h; mostly database time):

| Task | Wall s/s | Executions/s | Mean |
|---|---|---|---|
| `ProviderEgressProbe` | 7.7 | 0.02 | 478 s per pass |
| `FlushTransferDebits` | 2.87 | 3.9 | 0.74 s |
| `CloseStrandedGrantContracts` | 0.89 | | 60 s |
| `CloseContractDeadlines` | 0.89 | | 60 s |
| `CloseExpiredContracts` | 0.83 | 0.03 | 31 s |
| `UpdateClientScores` | 0.77 | | 163 s |
| `ApplyLegacyProviderTotals` | 0.58 | 618 | 0.9 ms |
| `ClosePayerDueContracts` | 0.46 | | |
| `RefreshVerifyProxyEgress` | 0.43 | | |

**`ApplyLegacyProviderTotals`** went from about 80/s before 10-30 to
650–880/s since, draining a backlog. It is 98% of executions, and each one
costs about 1.3 ms of CPU in total (about 0.8 cores):
- framework claim and finalize: about 0.9 ms;
- task body: about 0.4 ms.

**Close tasks.** `CloseExpiredContracts` was 0.00% of samples in the fleet
capture and 0.08% on edge-4. All four close tasks together were 0.1 cores on
edge-1 in one window. Their cost is database time; another agent covers the
96M backlog.

**Database.** The largest single taskworker statement by execution time is
the chunked `transfer_close_byte_sum` walk of the daily rollup and backfill
(model/transfer_close_byte_sum.go): 16 calls/s at 47 ms, 0.75 s/s.

#### 2.7 alt

There are two processes, on crisp and fireside, at about 0.45 cores
together. alt serves 78 req/s, 58% of them `/hello` and 37%
`/connect/control` (29/s, of which 25.4/s are `PeerAudit`). It also runs an
embedded exchange over H3 and whodis. Shares from the fireside capture, with
crisp agreeing:
- **quic-go:** 26%.
- **go-redis:** 26%:
  - `PubSub.Channel` readers (`initAllChan`), about 16%. These serve
    key-event and other `Channel()` subscriptions;
  - origin-notification `RunRedisShardSubscription`, 6.5%;
  - key-event subscriber processing, 4%.
- **Scheduler:** 16–19%.
- **GC:** 4–7%.
- **TLS:** 5%.
- **api handlers:** 6–7%.

#### 2.8 Logging

From Loki over 5 minutes:
- api: 12 lines/s (1.6 KB/s);
- alt: 2.5 lines/s;
- taskworker: 617 lines/s (238 KB/s), nearly all from the probe's embedded
  connect clients at the default level:
  - `[multi]receive no race and no client` is about 35%;
  - `sequence_exit`, `contract set` and `[rel]` events make up most of the
    rest.

In-process glog cost is at most 0.24%. Logging is not a CPU lever in this
domain beyond a small fluent-bit and Loki saving from quieting probe clients
(under 0.1 core, *(inferred)*).

### 3. Ranked opportunities

#### #1 Answer `PeerAudit` without the auth, limit and state path; stop sending it

**Evidence.** §2.3:
- 2.6k/s of control requests (25%) carry a lone `PeerAudit` frame. The
  sender is connect transfer.go:18806, a single-frame pack.
- The api has no handler. Each request pays for:
  - the JSON body decode and the lag-guest decode;
  - ES256 verification;
  - the Redis admission EVALSHA;
  - the Redis revocation GET;
  - frame decode;
  - the error path, which `strings.ToLower`s the error
    (controller/connect_controller.go:195–223);
  - a protobuf and JSON reply.
- On alt it is 87% of control requests.

**Saving.**
- At about 0.6–0.75 ms each: 1.6–1.95 cores of api CPU.
- Answering before authentication saves about 85%, or 1.4–1.7 cores.
- Answering after the signature check, inside the existing
  `BeforeStateAnswer`, saves 0.7–0.9 cores.
- Redis: −5.2k commands/s.
- If #2 lands first, the before-auth answer gains about 0.6 cores less.

**Change:**
- In `PrepareConnectControl` (controller/connect_control_limit.go:137),
  before `checkConnectControlLimit`: answer at once when every frame's
  message type is `TransferPeerAudit`. `PrepareConnectControlRefusals`
  already decodes the frames (connect_control_refusal_replay.go:72).
- Or decode the pack in the handler before authentication, for the larger
  saving.
- Add a `case *protocol.PeerAudit:` no-op in the dispatch
  (connect_controller.go:520–553), and a `peer_audit` label in
  `controlFrameMessageLabel` (:168), so mixed packs skip the error path.
- In connect, stop sending `PeerAudit` through `ClientOob()`
  (transfer.go:18781–18810), or put it behind a setting. Old SDKs will keep
  sending until they age out, so the server side is needed anyway.

**Correctness.** The server has never acted on `PeerAudit`; there is no
reference in controller, model or api history. The client discards the
answer. The reply grants nothing, which fits the `BeforeStateAnswer`
contract (session/client_session.go:210–217). No timeout, deadline or
detection bound is involved.

**Risk.** Low. One accounting change: if the answer comes before admission,
these requests no longer spend the per-client control budget.

**Validate:**
- `urnetwork_connect_control_frames_total{message="other",outcome="error"}`
  falls to about 0, replaced by `peer_audit`;
- api cores per krps;
- `urnetwork_redis_pool_hits_total{job="api"}`;
- `urnetwork_connect_control_client_limit_total`.

#### #2 Cache verified JWT claims

**Evidence.**
- About 19% of api CPU, roughly 225–260 µs per authenticated request:
  - the ES256 verify (`parseByJwtSigned`, session/by_jwt.go:420);
  - its claims decode;
  - a second, unverified decode in the lag-guest gate (router/lag_guest_credential.go:50–66);
  - a third, only when the intended-network header is present (session/intended_network.go:187).
- Nothing caches verification.

**Token reuse is high.** A busy api process sees only 1.6–1.9k distinct
authenticated networks per 5 minutes, against about 330k requests
(`urnetwork_session_active_authenticated_networks{window="5m"}`). Old SDKs
retry the same route every 5 s. A per-process cache retained for at least 10
minutes should hit at least 95% *(inferred)*.

**Saving.** 2.2–2.9 cores at peak (13–17% of api), and 1.7–2.2 at the daily
mean.

**Memory.** 100–200k entries × about 1.5 KB is 150–300 MB per process, out
of 400–700 GB free per host.

**Change:**
- A bounded LRU in `parseByJwtSigned`:
  - key: the exact token string, or its SHA-256, plus the keyring
    generation;
  - value: the immutable decoded claims, copied per call;
  - TTL: min(token exp, 15 min);
  - cache successes only, and flush on keyring reread.
- `ParseByJwtForAudience` (:466–540) keeps every per-request check: exp,
  nbf, iat with leeway, `validateSessionHorizon`, the gates, identity claims
  and `fixByJwt`.
- `requestLagGuestCredential` and `byJwtNetworkIdUnverified` read the cache
  first and fall back to their unverified decode on a miss.

**Correctness.** A signature check is a pure function of the token bytes and
the key set. The cache key binds both. Expiry, session horizon,
revocation-record and state checks still run on every request, so revocation
and expiry timing are unchanged. No timing bound moves.

**Risk.** Low–medium, because this is the auth path:
- return copies, never a shared struct;
- bound the memory;
- test key rotation, expiry while cached, and foreign or kid-less tokens
  (existing tests: session/by_jwt_keyring_test.go and by_jwt_test.go).

**Validate:**
- perf share of `p256*` and `ParseWithClaims` drops below 2%;
- a cache hit-ratio counter;
- `urnetwork_auth_jwt_rejections_total` unchanged;
- api cores per krps;
- `session/by_jwt_bench_test.go`.

#### #3 Verified-chain cache in connect's TLS handshaker (probe TLS)

**Evidence.** §2.6: 2.59 cores (16.9% of taskworker) in x509 chain building
and checking. The edge-4 focus capture puts 13 of 16 points of handshake time
under `crypto/x509.(*Certificate).Verify`, and 11.4 under P-384 ECDSA. The
clients are uTLS Chrome-hello handshakes
(connect net_tls_hello.go:275, `clientTlsHandshaker.handshake`) on the
control-plane dials of probe tunnels.

**Saving.** 2.3–2.5 cores of taskworker. Every connect client also gains
(apps and providers spend 6–10 ms per new connection *(inferred)*).

**Change (connect):**
- Use a process-wide cache in `newClientTlsConfig` (net_tls.go:40) or the
  handshaker.
- Set `InsecureSkipVerify` together with a `VerifyConnection` that:
  - looks up SHA-256 of the raw chain, plus `ServerName`, plus the root-pool
    generation;
  - on a miss, runs `x509.Verify` with the same `RootCAs`, intermediates and
    `DNSName`;
  - on success, stores the entry until the chain's earliest `NotAfter`,
    capped at about 1 h.
- A server-only alternative is #5.

**Correctness.** The trust decision is the same for the same bytes, name and
roots. TLS 1.3 still verifies `CertificateVerify` (proof of the leaf key) on
every handshake, because `InsecureSkipVerify` skips only chain building; uTLS
keeps crypto/tls's logic, which needs a test. Expiry is checked against the
cached `NotAfter`. Revocation is not checked by Go today. No timeout changes.

**Risk.** Low, but this is crypto code. Tests needed:
- a different SNI;
- an expired chain;
- one changed byte;
- a changed root pool;
- a hello retry.

**Validate:**
- taskworker perf share of `crypto/x509` falls below 1%;
- taskworker cores;
- `urnetwork_egress_probe_attempts_total{result}` and probe health results
  unchanged.

#### #4 Origin notifications: replace the broadcast invalidation

**Evidence.**
- 1.22k origin edges are published per second. Every api process (20) and
  alt process (2) subscribes to all 16 buckets and receives each one: 24.5k/s
  to api and 2.4k/s to alt.
- Each delivery costs about 35–40 µs, mostly go-redis `PubSub.ReceiveTimeout`
  (a deadline re-arm, a read syscall and a RESP parse per message), then the
  hex decode and counters. See model/contract_origin_notification.go:310–390
  and redis_shard_subscription.go:174–270.
- That is about 0.045 cores per process: about 0.9 cores on api and 0.1 on
  alt.
- The wake path the broadcast exists for is almost unused:
  - event wakes 0.3/s;
  - fallback wakes 97.7/s;
  - 1.05k waits/s
  (`urnetwork_connect_companion_origin_wait_wakes_total`, `…lookups_total`).
- Its remaining job is to remove missing-origin refusals from the
  per-process route-rejection ring early (model/contract_route_rejection.go:1–33).
  That ring serves 766 replays/s.

**Saving.** About 1.0 core, plus about 27k Redis pub/sub deliveries/s on
edge-5 and edge-6.

**Change:**
- (a) Cheap first step: bind the counters once, decode hex into a
  `[32]byte` without allocating, and fold the per-message receive re-arm into
  the existing silence and ping cadence. Perhaps 20–30% *(inferred)*.
- (b) The structural change:
  - the committer increments a per-pair origin version key instead of, or
    alongside, publishing;
  - a missing-origin replay first reads that key (one GET, about 0.8k/s) and
    drops the refusal if the version moved;
  - waits keep their existing PostgreSQL polls, unchanged;
  - with that, the bucket subscriptions can close.

**Correctness.**
- The companion-origin bounds are untouched: 100 ms first retry, 500 ms
  fallback, 3 s deadline (controller/connect_companion_origin.go:16–18).
- Refusal freshness becomes exact at replay time. Today an edge that this
  process never receives leaves a refusal for up to 60 s.
- The fence logic in `contractRouteRejections.put` and its removal, along
  with the shared lookup state, must be re-derived with deterministic
  ordering tests.

**Risk.** Medium.

**Validate:**
- `runBucket` CPU falls to 0;
- `urnetwork_connect_control_refusal_replays_total{cause}`;
- a test that a refused route is served once its origin commits;
- `companion_origin` stage time unchanged.

#### #5 One control-plane `ClientStrategy` per probe process

**Evidence.** §2.6:
- About 4.4 dials per tunnel, all fresh: there is no HTTP keep-alive across
  tunnels and no TLS resumption. The new strategy is created at
  providertunnel/tunnel.go:522.
- Beyond x509 (#3), handshakes cost 0.58 cores on taskworker. They also cost
  a server-side handshake in nginx and an extra round trip for each API call.

**Saving.** About 0.6 cores on taskworker, plus nginx handshake CPU, plus
API latency. If #3 is not done, this also takes most of #3.

**Change.**
- Share the strategy, or at least its HTTP transport and session caches,
  across a taskworker's tunnels for the API and platform dials.
- Platform WebSockets and identities stay per tunnel.

**Correctness.** The per-dial-path session-cache separation in net_tls.go:35–55
exists to avoid linking a user's egress paths. That concern does not apply
to the operator's own probe. Measurement semantics need a review: setup
timings would lose the CPU-bound verification time, which is arguably better
fidelity.

**Risk.** Medium, because this is a probe design review.

**Validate:** taskworker `connect()` rate, probe results, setup observations.

#### #6 `encodeUuid` and `parseUuid` without fmt (connect `id.go`)

**Evidence.**
- `encodeUuid` (connect id.go:169) is
  `fmt.Sprintf("%x-%x-%x-%x-%x", five slices)`.
- `parseUuid` (:149) concatenates four strings and calls `hex.DecodeString`.
- Every `server.Id.String()`, Redis key and JSON id goes through them. Key
  builders add another Sprintf, for example model/connect_control_limit.go:205.
- Measured: fmt is 1.5–1.9% of api (`encodeUuid` alone 0.9% of fmt plus
  0.46% of malloc) and 1.1–1.5% of taskworker.

**Saving.** About 0.25 cores api and 0.1 taskworker. More in connect and the
proxy, which were not measured here.

**Change.** A fixed `[36]byte` hex encoder with one allocation, and a
decoder that allocates nothing.

**Correctness.** A pure function. Table tests against the current output
(lowercase hex, dashes), including the 32-character input.

**Risk.** Very low.

#### #7 `task.updateFunctionName` compiles a regexp on every call

**Evidence.** task/task.go:1287–1290, and the twin at :1284, call
`regexp.MustCompile("/v\\d+")` on every call. It runs at claim admission,
claim group, run cohort, run (twice) and finish, about 6 times per task at
about 620 tasks/s. That is 0.8–1.0% of taskworker in busy captures and 4.5%
in a quiet one.

**Saving.** 0.1–0.15 cores.

**Change.** A package-level compiled regexp plus a `sync.Map` memo; the
names are a small fixed set.

**Risk.** None.

#### #8 Pre-bound Prometheus children on hot paths

**Evidence.** The Prometheus client is 2.3–2.6% of api. Hot-path counters
call `WithLabelValues` with constant labels on every request: the limit
decisions (connect_control_limit.go:104–131), HTTP request, duration and
bytes vectors, control phase and frame metrics, and auth attempts.

**Saving.** 1–1.5% of api, about 0.2 cores.

**Change.** Resolve the fixed-label children once (curry them); keep the
dynamic labels.

**Risk.** Low.

#### #9 api GOMAXPROCS 8 → 4 (A/B)

**Evidence.**
- Five processes × 8 Ps is 40 Ps per host, carrying 2.5–7 cores per host.
- The scheduler is 13–14% of api CPU (futex 7%, netpoll 3.5%,
  `stealWork` 2.4%).
- The busiest api process peaks at about 2 cores.

**Saving.** 2–5% of api *(inferred)*.

**Change.** GOMAXPROCS 4 on one block (g1) on all hosts. Compare cores per
krps and the control phase means against g2–g4.

**Correctness.** This is not a timeout or detection bound. Check that queuing
does not rise, using the phase means and in-flight gauges, and revert if it
does.

#### #10 api GC and the live heap

**Evidence.**
- GC is 5.1% time-averaged, about 0.8 cores fleet-wide: 0.22 cycles/s × 3–4
  core-seconds per cycle.
- Busy processes collect on allocation every 61–71 s. Quiet ones run on Go's
  2-minute forced GC.
- So GOGC 100 → 200 or 400 only moves busy processes to the 2-minute floor,
  about 0.2 cores (1%).
- The live heap is about 3.1 GB per process. At startup the api warms
  in-memory search indexes (api/run.go:79–80):
  - network names (model/network_model.go:44);
  - locations and location groups (model/network_client_location_search_model.go:34–46).

  Their projections are maps of maps (search/search_local.go:46–66). The
  source holds about 7.8M `search_value` rows (catalog estimate, about 80%
  location-prefix) and 52.8M projections. That index is likely most of the
  heap *(inferred; no pprof endpoint exists)*.

**Saving.**
- A pointer-free index, or serving these rare searches
  (`/auth/network-check`, location lookups) from fewer processes, would cut
  per-cycle mark work: 0.4–0.6 cores *(inferred)*.
- About 2–3 GB of RSS per process.

**Validate first.** Take a heap profile of a production-sized index in a
canary or test.

#### #11 Fewer database round trips per real create

**Evidence.** §2.5:
- 4–6 pool acquisitions before the escrow transaction:
  - `GetClientContractMetadata`;
  - the provide relationship;
  - the provide-secret fallback;
  - `CompanionOriginExists`;
  - the active pair networks.
- pgx is 6–7% of api CPU.

**Saving.** Merging the reads into one statement or one pgx batch saves 2–3
round trips per create: about 0.3–0.4 cores *(inferred)*, plus latency and
less pool pressure.

**Risk.** Low–medium: query-plan review for the merged statement.

#### #12 Fixed overhead per api process

**Evidence.** §2.2: about 0.13–0.22 cores per process at zero traffic. The 4
beta processes take about 3% of requests and cost about 1 core.

**Change.**
- #4 and #10 shrink the overhead.
- Beyond that, run beta on one or two hosts only, or fewer blocks per host:
  0.1–0.15 cores per process removed.
- Weight edge-1 for its slower cores.

This is an owner topology decision.

#### #13 `ApplyLegacyProviderTotals` per-task overhead

**Evidence.** §2.6:
- 618–880 tasks/s at about 1.3 ms of CPU each, about 0.8 cores.
- The `pending_task` churn on the primary is `INSERT` 655/s and `DELETE`
  1.6k/s.

**Change.** Larger claim batches per transaction. A batch target exists
(model/subscription_provider_totals_batch.go).

**Saving.** 0.3–0.5 cores while the backlog drains *(inferred)*.

#### Considered and not recommended

- **Caching the revocation-record check** (session/revocation_record.go:331).
  Immediate revocation is correctness.
- **Shortening companion-origin polls, or waiting on notifications alone.**
  The owner rule forbids it.
- **Skipping per-client admission for replays.** It saves about 0.1 ms per
  replay, but changes rate-limit semantics. Raise it with the owner only
  after #2.
- **Reusing probe identities across tunnels.** The probe deliberately
  retires each identity at teardown (tunnel.go:532–536). Its cost is part of
  the probe's 137 ms per attempt, and this is the owner's measurement-design
  decision.
- **Pipelining admission with the revocation-record GET.** The keys hash to
  different slots (`{connect_control_limit_<client>}` against
  network-and-lineage shards), so it would need a key migration.

### 4. CPUOPT2 predictions: what did and didn't materialize

| CPUOPT2 (§2.5, §3.8) | 10-31 | Verdict |
|---|---|---|
| api GC about 20%; GOGC=400 with GOMEMLIMIT saves 12–15% of api | GOGC still 100, GOMEMLIMIT unset. GC 5.1% averaged over 5 processes × 60 s (0–8.3% per process). The 2-minute forced GC floors the cycle rate | Not shipped. The 20% came from one 20 s single-process sample that caught a cycle. GOGC alone is now worth about 1%; the live heap (#10) is the lever |
| JWT verification 11%; cache saves about 10% | `ParseByJwtForAudience` 10.5–11.7%, plus 4.4–5% for the lag-guest decode, plus about 4.5% of cut-off P-256 stacks: about 19% | Not shipped; larger than estimated (#2) |
| JSON 6.6% | 7.1% across encoding/json v2 and jsontext, mostly JWT claims | Unchanged; #2 removes most of it |
| `/connect/control` handler about 37% inclusive; creep 4.9k → 9.0k/s; refusals tripled; resets on connect restart; cached refusal per (client, destination) proposed | `ConnectControl` 42–44% inclusive; 10.4k/s. The replay (shipped 10-12, before CPUOPT2) now answers 5.5k/s at about 0.45 ms. The sawtooth persists: 7.3k/s just before the 10-31 02:00Z connect deploy, 1.1k/s after it, and 5.6–5.9k/s again by 18–19Z | The cache exists and works; the creep's root cause is upstream (§5) |
| taskworker "database-bound… not a target" (`FlushTransferDebits` 22.6%) | The provider egress probe is ≥55% of taskworker CPU, x509 17%; `FlushTransferDebits` 1.4% | No longer true; taskworker is a CPU target (#3, #5, #7) |
| api cost per request | 1.8–2.4 cores per krps then, 1.41 now | Improved by mix (more cheap replays), not by code; total api CPU is flat to up with traffic |
| api heap 4.7 GB in use | 3.7–5.7 GB in use, live about 3.1 GB | Unchanged |

### 5. Cross-domain notes

These are for the connect, database and topology owners.

- **The contract-refusal sawtooth in connect** sets most of api's variable
  load:
  - replays climb from about 1k/s after a connect restart to 5–7k/s;
  - even after #2 a replay costs about 0.2 ms, so the peak replay load is
    worth about 1.5 cores of api, plus Redis admission;
  - fixing the cause in connect, or a retry hint new SDKs honor, would remove
    most of it.
- **The connect announce loop** issues `SELECT COUNT(*) … FROM
  provide_key_change` 7.0k/s (model/network_client_model.go:3270). It is the
  most frequent statement on the primary.
- **go-redis `PubSub.Channel` readers** take about 16% of alt CPU, through
  the key-event and other `Channel()` subscriptions. Check the same
  subscriptions in the 20 connect processes.
- **The x509 cost of the Let's Encrypt YE chain** (about four P-384 verifies)
  hits every connect client on every new connection. #3 benefits apps and
  providers too. A shorter served chain would also cut it, but that is a
  TLS-ops decision with compatibility trade-offs.
- **Capacity weighting.** edge-1 is a Broadwell E5-2697 v4 at 2.3 GHz, and
  api costs 2.17 cores per krps there against 1.16–1.22 on edge-3 and edge-4.
  Separately, api requests are skewed about 2.8× toward edge-3 and edge-4
  processes.

### Appendix: scripts and raw outputs

In scratchpad cpuopt3/work/:
- perf scripts: `perf_svc3.sh`, `perf_svc4.sh`, `perf_multi3.sh`,
  `perf_tw3.sh`, `perf_tw4.sh`, `perf_tw5.sh`, `perf_focus3.sh`,
  `perf_callee3.sh`, `connect_rate.sh`;
- Mimir scripts: `svc_cpu.py`, `routes.py`, `control.py`, `tasks.py`,
  `regress.py`, `labels.py`;
- outputs: `api_*.txt`, `tw_*.txt`, `alt_*.txt`.

In cpuopt3/pg/:
- `pg_stat_statements` snapshots (`s1.txt`, `s2.txt`).

The ECDSA benchmark is in cpuopt3/bench_ecdsa/.

## Part D — PostgreSQL on edge-2

This is research, not a change. Every number comes from read-only measurement
of Main's primary, by-us-fmt-5-edge-2, on 2026-10-31 between 18:26Z and 18:55Z,
plus Mimir history. Nothing in production was changed. I printed no
identifiers, addresses or literals. Code references are at server origin/main
964c489c0 and xops main. *(inferred)* marks a claim reasoned from code or
arithmetic rather than measured.

### Summary

**Where edge-2's CPU goes.** edge-2 has 96 logical CPUs (2× Xeon Platinum
8168, HT). During the window it was 39–41 cores busy. A 20 s `/proc` census
attributed 41.3 cores:

| Consumer | Cores | Share | Notes |
|---|---|---|---|
| PostgreSQL backends | 29.1 | 71% | 24.7 user, 4.4 sys |
| PgBouncer (32 processes) | 5.6 | 14% | 73% kernel time |
| nginx stream proxy (port 6432) | 4.1 | 10% | 80% kernel time |
| PostgreSQL io workers, autovacuum, checkpointer, walwriter | 1.7 | 4% | |
| dm-crypt kworkers and the rest | ~0.8 | 2% | |

**Inside PostgreSQL, nearly half the CPU is per-statement overhead, not query
execution.** LBR call stacks put the executor at about 31–45% of PostgreSQL CPU.
Planning takes 11.8%, executor start 9.7% and the protocol and main loop 20%.
The load is about 104k statements/s (99% of them under 1 ms) in 37k
transactions/s, so per-round-trip costs dominate. Those costs fall on PgBouncer,
nginx and the PostgreSQL protocol path alike.

**The biggest swings come from long snapshots, not from any query.** While a
long transaction holds the cluster xmin horizon, dead tuples in the high-churn
tables cannot be cleaned up. Every OLTP read then walks them.
- **The 10-29 `pg_dump`:** buffer hits rose 10× (3.4 → 33M/s) and edge-2 went
  from 37 to 92 busy cores. That is +34 cores on average, about 455
  core-hours, before the owner's CPU rule stopped it.
- **The 10-30 SN census attempts:** two runs of up to 3 h each did the same,
  at +15–25 cores.
- **Recovery:** in both cases CPU returned to baseline within an hour of the
  snapshot closing.

**Ranked opportunities.** Savings are in edge-2 cores at today's load; 1 core
is about 2.5% of edge-2's busy CPU. They overlap only slightly.

| # | Change | Est. saving | Effort | Risk |
|---|---|---|---|---|
| 1 | No long snapshots on the primary: physical backups (the pg-basebackup play exists) instead of `pg_dump`; the census as one short bitmap read at off-peak; shorter retention-cascade transactions | Avoids +15–55 cores while a snapshot is open; a resumed twice-weekly dump averages 3–7 cores a week | days | low–medium |
| 2 | Remove the nginx hop to PgBouncer (client-side spread over ports 6433–6464, or PgBouncer `so_reuseport`) | 4–5 cores (10–12%) | days | medium |
| 3 | PgBouncer → PostgreSQL over a Unix socket instead of loopback TCP | 1–2 cores *(unmeasured)* | hours | low |
| 4 | Reboot edge-2 into the staged `retbleed=stuff iommu=pt` (IBRS is live now) | 1.5–3 cores *(unmeasured)* | maintenance window | medium |
| 5 | Fewer round trips per transaction (ownership PID checks, advisory-lock statements, unlocks) | 1–2 cores | 1–2 weeks | low–medium |
| 6 | Measure planning with `track_planning`; stop plan-invalidation storms from ANALYZE every ~16 s on tiny churn tables | 0.3–1.5 cores | days | low |
| 7 | transfer_contract index cuts: drop the 82 GB `reap_pending` index (its query cannot match a row before 2027-03-28) and the 13 GB `stream_id` index | 0.2–0.5 cores; −95 GB per vacuum cycle | days | low |
| 8 | Break transfer_contract's perpetual anti-wraparound vacuum loop (shorter cycles, lower per-table freeze_min_age; partitioning later) | ≤0.5 core CPU; −30–50% of 229 MB/s of autovacuum reads; freeze-age peaks from 330–450M to about 200M | days | medium |
| 9 | `jit = off` (or a much higher `jit_above_cost`) | 0.06–0.24 cores | hours | low |
| 10 | Compute provider-census inputs once fleet-wide, not per caller | 0.3–0.6 cores | 1–2 weeks | low–medium |
| 11 | GetTransferStats unpaid bytes as an incremental total | 0.1–0.15 cores; −8% of buffer hits | ~1 week | low |
| 12 | transfer_debit_journal dead-entry walk (475k index entries/s for 1.3k live rows) | 0.1–0.3 cores; much more during snapshot pins | days | low–medium |

**Connection churn and idle-in-transaction are not problems now:**
- 0.8 new backends/s, 3.1 PgBouncer client logins/s, about 0 server logins/s.
- Idle-in-transaction averaged 33 on 10-31, down from 566 on 10-22. 85% of
  those sessions are under 10 ms old (normal pipelining).
- PgBouncer has had no waiting clients since 10-27.

**Risk outside CPU:**
- transfer_contract's relfrozenxid age is 329M and peaked at 450M on 10-30.
  It is the only table above 200M.
- At 7.8k xids/s, about 45 h without a completed transfer_contract vacuum
  would reach `vacuum_failsafe_age` (1.6B).

### 1. Method

- **Load at the time (18:31–18:35Z):**
  - edge-2: 39–41 cores busy; 36.4k commits/s and 104k statements/s.
  - transfer_contract: 1.37k inserts/s and 1.76k updates/s.
  - Contract closes 1.67k/s, of which 134/s are force-closes.
  - xid consumption 7.8k/s; WAL 68 MB/s.
- **Mimir:**
  - edge-2 CPU modes for 10 days (10-22 to 10-31), compared with CPUOPT2's
    window (10-22 to 10-29).
  - PostgreSQL exporter: pg_stat_database rates, activity by state,
    wraparound age.
  - PgBouncer exporter (32 pools); disk throughput, IOPS and utilization.
- **pg_stat_statements:**
  - Last reset at 2026-10-23 20:08:05Z, at crash recovery after the disk-full
    outage. The window is 190.4 h; everything "since reset" is normalized per
    hour.
  - A 219 s delta window, 18:31:01–18:34:40Z, gives the current state.
  - The since-reset window includes the 10-24 restore, the 10-25 incident,
    the 10-29 dump and the 10-30 census. So its averages for queue-table
    statements are inflated by snapshot pins (§2.7).
  - Query text is pg_stat_statements' normalized text with any remaining
    literal stripped on the server, truncated.
- **pg_stat_activity:** 120 samples at 0.25 s intervals (18:35:19–18:35:52Z),
  grouped by backend type, state, wait event, client class (local or remote,
  never an address), query_id and idle-in-transaction age.
- **Host CPU:**
  - CPU per process category from `/proc` over 20 s.
  - `perf record -a -F 99` for 30 s, flat. PostgreSQL is a PGDG jammy build
    (gcc 11), so call stacks came from LBR rather than frame pointers.
  - Two LBR call-stack captures, 15 s and 12 s at 49–99 Hz. The second
    classified PostgreSQL samples by execution phase.
  - Data files were written to /var/tmp and deleted; only symbol aggregates
    were printed.
- **Catalog and statistics views:**
  - pg_stat_user_tables (PG 18 adds total_autovacuum_time) and
    pg_stat_user_indexes, with sizes.
  - pg_stat_io, pg_stat_checkpointer, pg_stat_wal, pg_stat_progress_vacuum.
  - A 60 s rate snapshot of every index and table counter.
  - PgBouncer `SHOW STATS` on all 32 admin consoles, 21.6 s apart.
- **EXPLAIN (no ANALYZE):**
  - the force-close page;
  - the straggler reap stamp, with and without its partial index;
  - the payer open-sum;
  - the weekly census's whole-window read, under its own plan settings.

Every SQL session was `BEGIN READ ONLY` (or `default_transaction_read_only`)
with `statement_timeout` of 15–30 s. Nothing counted a large table. The only
query on a large table was one `min(create_time)` index probe.

### 2. Where the CPU goes

#### 2.1 Host, 10 days

Busy means every non-idle mode, including iowait (1.4–2.7 cores), as in
CPUOPT2.

| Day | user | sys | softirq | iowait | busy mean | busy p95 | busy max | What happened |
|---|---|---|---|---|---|---|---|---|
| 10-22 | 24.6 | 9.2 | 3.8 | 1.4 | 38.9 | 52.3 | 61.1 | |
| 10-23 | 14.3 | 6.9 | 2.5 | 1.0 | 24.6 | 41.7 | 53.2 | PG down from 11:12Z (disk full) |
| 10-24 | 33.5 | 12.4 | 6.4 | 1.0 | 53.3 | 85.5 | 88.3 | restore, cold cache, dump |
| 10-25 | 22.1 | 8.2 | 3.0 | 1.4 | 34.6 | 72.7 | 77.9 | PgBouncer queue incident |
| 10-26 | 20.0 | 8.9 | 3.7 | 1.1 | 33.7 | 40.2 | 42.9 | |
| 10-27 | 23.8 | 8.5 | 3.6 | 1.6 | 37.5 | 53.2 | 70.3 | |
| 10-28 | 22.7 | 8.0 | 3.3 | 2.1 | 36.1 | 47.5 | 56.5 | |
| 10-29 | 41.3 | 9.2 | 4.1 | 1.4 | 56.0 | 90.0 | 92.2 | `pg_dump` 04:31–17:45Z |
| 10-30 | 25.4 | 7.8 | 3.5 | 2.0 | 38.7 | 51.1 | 56.8 | census attempts from 18:30Z |
| 10-31 (to 18:26Z) | 23.0 | 7.5 | 3.0 | 2.7 | 36.2 | 45.8 | 59.3 | |

| Window | busy mean | p50 | p95 | max |
|---|---|---|---|---|
| CPUOPT2's, 10-22 to 10-29 | 36.3 | 35.7 | 68.7 | 88.3 |
| Last 7 days | 40.2 | 36.9 | 73.4 | 92.2 (10-29, the dump) |
| Last 24 h | 38.7 | 38.3 | 54.1 | 59.3 |

**Against CPUOPT2's window:**
- Median busy CPU is roughly flat (35.7 → 37–38 cores), while the workload
  mix changed:
  - row updates more than doubled (3.8k → 8.7k/s);
  - inserts rose 11.6k → 12.8k/s and deletes 3.0k → 5.2k/s;
  - commits fell by a third (42–46k/s on 10-22..10-27 → 29.6–32.7k/s on
    10-30/31).
- The tail is entirely long-snapshot episodes (§2.7).
- Disk reads rose from 129 MB/s (10-22) to 351 MB/s (10-31), and writes from
  81 to 145 MB/s. sda is about 50% utilized, with 7.6k read and 6.9k write
  IOPS. Most of the reads are autovacuum (§2.8).

#### 2.2 By process

From `/proc`, 20 s at about 18:38Z:

| Category | Cores | user | sys | Processes |
|---|---|---|---|---|
| PostgreSQL client backends | 29.13 | 24.73 | 4.40 | 830 |
| … via PgBouncer (loopback) | ~23 | | | ~640 |
| … direct (taskworker maintenance pool and others, port 5432) | ~6 | | | ~156 |
| PgBouncer | 5.61 | 1.54 | 4.07 | 32 |
| nginx stream (6432 → 6433–6464) | 4.05 | 0.83 | 3.22 | 33 |
| PostgreSQL io workers (`io_method=worker`, 24) | 1.08 | 0.12 | 0.95 | 24 |
| autovacuum worker (one, on transfer_contract) | 0.39 | 0.38 | 0.01 | 1 |
| checkpointer and walwriter | 0.23 | | | 2 |
| dm-crypt kworkers (LUKS under all of PG's IO) | ~0.5 | | | many |

- **perf (30 s, 99 Hz)** splits busy samples into PostgreSQL 75%, PgBouncer
  14%, nginx 10% and the rest 1%, matching `/proc`.
- **Per round trip:** at 104.4k statements/s, PgBouncer costs about 54 µs of
  CPU per statement and nginx about 39 µs, mostly in the kernel. Their top
  symbols are syscall entry and exit, TCP send and ACK processing,
  `nft_do_chain`, memcg accounting and IOMMU DMA mapping (`pfn_to_dma_pte`,
  `__domain_mapping`). Retbleed is mitigated by IBRS: the kernel options are
  staged but not rebooted on edge-2.

#### 2.3 Inside PostgreSQL

**LBR call stacks, 12 s.** Of 21,414 PostgreSQL samples, classified by the
first recognizable frame:

| Phase | Share | ~Cores |
|---|---|---|
| executor run | 31.0% | 9.3 |
| unclassified (mostly deep executor stacks past LBR's 32-frame depth *(inferred)*) | 18.5% | 5.5 |
| planning (`pg_plan_queries`, `BuildCachedPlan`, `subquery_planner`) | 11.8% | 3.5 |
| executor start (`ExecutorStart`, `ExecInitNode`, `PortalStart`) | 9.7% | 2.9 |
| main loop, other (`PostgresMain` without a deeper match) | 8.6% | 2.6 |
| protocol receive and wait (`secure_read`, `pq_*`) | 6.9% | 2.1 |
| protocol send (`ReadyForQuery`, flush, `secure_write`) | 4.7% | 1.4 |
| executor end | 2.7% | 0.8 |
| io worker | 2.4% | 0.7 |
| parse, analyze, rewrite | 1.2% | 0.4 |
| vacuum | 0.9% | 0.3 |
| utility, transaction start/commit, checkpointer, pgstat flush, parallel | 1.4% | 0.4 |

- **Per-statement overhead** (planning, executor start and end, parse,
  protocol, main loop, transaction control) is about 47% of PostgreSQL CPU,
  roughly 14 cores.
- **The sampled `pg_stat_activity` agrees:** only about 13 backends are on CPU
  in the executor at any instant (11.3 client, 1.15 parallel, 0.7
  autovacuum). pg_stat_statements' exec time is ExecutorRun plus Finish only,
  and adds up to 15.2 backends busy in the window.
- **Prepared statements already work through PgBouncer:**
  - 93.7k Binds/s, against 232 client Parses/s and only 55 server
    re-prepares/s (PgBouncer 1.26.1, `max_prepared_statements = 1024`).
  - So the 3.5 cores of planning do not come from PgBouncer re-preparing.
    They come from custom plans and cache invalidations (§3 #6).

**Top flat symbols (percent of all host samples):**
- `hash_search_with_hash_value` 2.25% (buffer-mapping, lock and cache lookups);
- `heap_hot_search_buffer` 1.73%;
- `_bt_compare` 1.18%;
- `heap_page_prune_opt` 0.89%;
- `AllocSetAlloc` 0.89%;
- `expression_tree_walker_impl` 0.47%;
- `copyObjectImpl` 0.37%;
- `GetSnapshotData` 0.35%;
- `ExecEvalHashedScalarArrayOp` 0.34%;
- `HeapTupleSatisfiesVisibility` 0.33%;
- `XidInMVCCSnapshot` 0.29%;
- liblz4 0.45% (WAL full-page-image compression);
- JIT-compiled code 0.36%.

`heap_hot_search_buffer`, `heap_page_prune_opt` and the visibility functions
are the cost of index scans landing on dead tuples (§2.7, #12).

#### 2.4 Workload shape

- **PgBouncer `SHOW STATS`,** 32 instances, 21.6 s at about 18:46Z:
  - 37.2k transactions/s and 104.4k queries/s;
  - 16.1 MB/s in and 14.1 MB/s out;
  - average query 264 µs, average transaction 1.99 ms, wait about 0.
- **2.8 statements per transaction.** Transaction-control and lock statements
  are 19.4k/s, 18.7% of all statements:
  - `begin` 7.8k/s (it already carries `set local lock_timeout`, db.go:1037);
  - `SELECT pg_backend_pid()` 1.8k/s;
  - the task-claim session statement 1.7k/s;
  - advisory-lock statements about 4.6k/s;
  - unlocks 1.0k/s; `set_config` 0.8k/s.
- Since the reset they averaged about 30k/s, 36% of statements. The per-transaction
  `SET LOCAL lock_timeout` statement (4.37B calls since the reset) is gone in
  the current window. Folding it into BEGIN has already cut one round trip per
  transaction.
- **Window exec time by table family** (each statement goes to its first
  matching family):

  | Family | Exec time | Calls |
  |---|---|---|
  | transfer_contract | 35.9% | 17.4k/s |
  | network_client, provide_key, device | 19.7% | 18.7k/s |
  | transfer_escrow and _sweep | 9.4% | 9.1k/s |
  | contract_close* | 5.9% | |
  | provider_egress and probes | 4.7% | |
  | pending_task and finished_task | 4.7% | |
  | reliability and location | 4.4% | |
  | transfer_debit_journal | 3.6% | |
  | transfer_balance | 3.1% | |
  | transaction control | 1.3% | 18.4% of calls |
- **By statement latency:**
  - statements averaging under 1 ms are 98–99% of calls and 55–60% of exec
    time, and they carry nearly all of the overhead outside exec time;
  - statements averaging 1 s or more are 11–15% of exec time and 55% of
    shared-block reads.

#### 2.5 pg_stat_statements

**Totals since the reset:**
- 2,372 exec-hours over 190.4 h, 12.5 backends busy on average.
- 57.5B calls (83.9k/s).
- Shared blocks: 2.78T hits and 8.2B reads.
- Temp blocks written: 105M (0.86 TB).
- WAL attributed to statements: 20.9 TB. JIT: 45.1 h.
- Local (temp-table) blocks: 2.4B, negligible.

**Top 20 by total exec time since the reset, per hour.** The current-window
mean is in brackets where it differs a lot.

| # | Statement (normalized, truncated) | %time | calls/h | mean ms | rows/call | hit/call | read/call | WAL MB/h |
|---|---|---|---|---|---|---|---|---|
| 1 | `WITH slot_candidates AS MATERIALIZED (… FROM provider_egress_probe_cycle … next_attempt_at<=$1 ORDER BY …` | 6.22 | 141,521 | 19.7 [3.0] | 1.3 | 13,054 [1,072] | 0.9 | 746 |
| 2 | `WITH allocation AS MATERIALIZED (SELECT EXISTS (… prober_identity …), (SELECT count(*) … transfer_balance …` | 3.86 | 2,739,953 | 0.63 | 8.0 | 227 | 0 | 2 |
| 3 | `WITH RECURSIVE ancestors AS (SELECT client_id, source_client_id, network_id, active … FROM network_client …` | 3.53 | 25,082,036 [5.2M] | 0.06 | 1.0 | 18 | 0 | 56 |
| 4 | `SELECT COALESCE((SELECT GREATEST(…create_time…) FROM transfer_debit_journal WHERE shard=$1 AND applied=$2 ORDER BY create_time LIMIT …` | 3.35 | 22,041 | 68.1 [6.1] | 1.0 | 46,818 [4,950] | 0 | 4 |
| 5 | `SELECT active FROM network_client WHERE client_id = $1 AND network_id = $2 AND (…) FOR UPDATE` | 2.90 | 858,669 | 1.52 | 1.0 | 6 | 0 | 304 |
| 6 | `SELECT client_id, network_id, active FROM network_client WHERE client_id = ANY($1) ORDER BY client_id FOR SHARE` | 2.36 | 504,330 | 2.10 | 2.0 | 13 | 0 | 273 |
| 7 | `WITH creation_clock AS MATERIALIZED (…) INSERT INTO transfer_contract(…)` (variant A) | 2.26 | 1,911,247 | 0.53 | 1.0 | 56 | 0.43 | 8,278 |
| 8 | `SELECT proxy_client_change.change_id, … FROM proxy_client_change INNER JOIN proxy_client …` | 2.21 | 26,752 | 37.0 [1.3] | 200 | 980 | 11.8 | 1 |
| 9 | `UPDATE transfer_contract SET outcome = $2, close_time = clock_timestamp() …, provider_usage = $3 WHERE contract_id = $1 AND outcome IS NULL RETURNING …` | 2.17 | 1,708,283 | 0.57 | 1.0 | 57 | 0.17 | 6,164 |
| 10 | `SELECT e.balance_id, … COALESCE((SELECT sum(debit_byte_count) FROM transfer_debit_journal j WH…` | 2.10 | 1,068,905 | 0.88 | 0.8 | 355 [171] | 0 | 2 |
| 11 | `SELECT EXISTS(… transfer_contract WHERE contract_id=$1), EXISTS(… transfer_escrow …), EXISTS(… JOIN transfer_balance …` | 1.55 | 1,240,771 | 0.56 | 1.0 | 168 | 0 | 11 |
| 12 | `UPDATE transfer_contract SET outcome = $2, close_time = …, dispute = $6, usage_unverified = $7, expiration_time = LEAST(…) …` | 1.48 | 1,068,905 | 0.62 | 1.0 | 54 | 0.96 | 7,107 |
| 13 | `SELECT client_id, MAX(measured_at), MIN(measured_at), SUM(ok_count), SUM(total_count) FROM (… provider_egress_health_history WHERE measured_at > $1 …` | 1.29 | 415 | 1,393 | 32,036 | 591,530 | 66 | 0 |
| 14 | `SELECT balance_id FROM transfer_debit_journal WHERE shard=$1 AND balance_id>$2 ORDER BY balance_id LIMIT $3` | 1.24 | 284,179 | 1.97 [0.14] | 1.0 | 1,238 [70] | 0 | 2 |
| 15 | `UPDATE network_client_connection SET connected = $3, disconnect_time = $2 WHERE connection_id = $1` | 1.22 | 858,815 | 0.64 | 1.0 | 51 | 0.15 | 2,624 |
| 16 | `WITH payment AS MATERIALIZED (SELECT payment_id, block_rollup_cursor FROM account_payment WHERE NOT block_rollup_complete AND EXISTS (… account_point …` | 1.17 | 329 | 1,596 | 1.0 | 1,821,157 | 220 | 5 |
| 17 | `SELECT selected_escrow.balance_id, selected_escrow.balance_byte_count, … FROM unnest(ARRAY[$1::uuid]) AS requested_contract(contract_id) CROSS JOIN LATERAL (… transfer_escrow …` | 1.16 | 1,608,258 | 0.32 | 1.0 | 31 | 0 | 3 |
| 18 | `SELECT escrow.balance_id, … GREATEST($2,EXTRACT(… clock_timestamp() … contract.create_time))*$5) …, (SELECT debit_byte_count FROM transfer_debit_journal …` | 1.14 | 812,089 | 0.63 | 1.0 | 18 | 0 | 3 |
| 19 | `INSERT INTO contract_close_report_evidence (…) VALUES (…)` | 1.14 | 2,414,607 | 0.21 | 1.0 | 15 | 0.01 | 3,036 |
| 20 | `INSERT INTO network_ping_hour_tally (…) SELECT * FROM unnest(…)` | 1.14 | 2,602 | 196 | 1.5 | 19 | 0 | 1 |

Notes on the table above:
- Row 18 spent 0.61 ms of JIT per 0.63 ms call (499 s of JIT per hour) since
  the reset; §3 #9.
- Row 16 is ~1.8M buffer hits per call at 329 calls/h; worth a look by its owner.

**Top 15 by calls, current window (18:31–18:35Z):**

| calls/s | mean ms | Statement |
|---|---|---|
| 7,834 | 0.011 | `begin isolation level read committed read write not deferrable` (plus its `set local lock_timeout`) |
| 7,016 | 0.018 | `SELECT COUNT(*) AS changed_count FROM provide_key_change WHERE client_id = $1 AND $2 <= change_time` (autocommit; August's F5 fix holds) |
| 2,998 | 0.012 | `SELECT client_id,network_id FROM contract_extender WHERE contract_id=$1` |
| 2,495 | 0.085 | pair network lookup (`SELECT a.network_id, a.active, … b.network_id … prober.network_id …`) |
| 2,046 | 0.026 | `SELECT party, used_transfer_byte_count, checkpoint, … FROM contract_close WHERE contract_id = $1` |
| 2,046 | 0.145 | `WITH prior AS MATERIALIZED (SELECT … FROM contract_close WHERE contract_id=$1 AND party=$2) …` (close-report upsert) |
| 2,002 | 0.084 | `SELECT source_id,destination_id,outcome,dispute FROM transfer_contract WHERE contract_id=$1 FOR UPDATE` |
| 1,761 | 0.002 | `SELECT pg_backend_pid()` |
| 1,741 | 0.011 | `WITH task_claim_session AS MATERIALIZED (SELECT current_setting(…), pg_backend_pid(), … pg_try_advisory_lock(…)` |
| 1,578 | 0.006 | `SELECT pg_advisory_xact_lock(hashtextextended($1, $2))` |
| 1,572 | 0.033 | `SELECT source_network_id, … FROM transfer_contract WHERE contract_id=$1 FOR UPDATE` |
| 1,571 | 0.022 | `DELETE FROM pending_task WHERE ctid=ANY(ARRAY(SELECT ctid FROM pending_task WHERE run_once_key=ANY($1::text[]) …` |
| 1,561 | 0.063 | `SELECT provide_mode, secret_key FROM provide_key WHERE client_id = $1` |
| 1,448 | 0.074 | the recursive ancestors walk (row 3 above; 6,967/s averaged since the reset) |
| 1,410 | 0.134 | `INSERT INTO transfer_debit_journal (contract_id,balance_id,debit_byte_count,shard) VALUES(…)` |

**Top 10 by exec time, current window:**

| Share | Statement | Rate and cost |
|---|---|---|
| 7.5% | transfer_contract INSERT (A) | 871/s, 1.30 ms |
| 5.6% | close UPDATE | 1,170/s, 0.73 ms |
| 5.4% | close-byte chunk walk (`WITH chunk AS MATERIALIZED (SELECT close_time, contract_id FROM ((SELECT … FROM transfer_contract WHERE outcome IS NOT NULL AND (close_time, contract_id) > ($1,$2) …`) | 10.9/s, 75 ms, 11.8k hits and 312 reads per call: the daily transfer rollup or the block-9 clock backfill (model/transfer_close_byte_sum.go), bounded by design |
| 4.5% | `INSERT INTO transfer_escrow` | 1,179/s |
| 2.8% | payment-driven contract retention (`WITH payment AS MATERIALIZED (… account_payment WHERE contract_retention_pending AND completed …`) | 47 s per call; the largest WAL writer since the reset at 10.9 GB/h |
| 2.4% | allocation | |
| 2.3% | `INSERT INTO pending_task` | 606/s |
| 2.2% | transfer_contract INSERT (B) | |
| 2.1% | contract FOR UPDATE with an evidence NOT EXISTS | |
| 2.0% | network_client FOR KEY SHARE | 1,198/s |

**Other columns:**
- **Temp:** the largest temp writers are rare analytics, for example one
  statement wrote 12.8M temp blocks (105 GB) per call. Temp IO is 2.2–2.4 MB/s
  on 10-30/31.
- **Ad hoc queries:** two investigation queries from other sessions ran during
  the window (a regex scan of pending_task run_once_key, 2.8 s; a LIKE scan,
  9.8 s with 2.7M reads). Each cost about 0.3 cores while it ran.

#### 2.6 Backends, waits, idle-in-transaction, PgBouncer

**Sampled `pg_stat_activity`** (120 samples, 18:35Z), average counts:

| Backends | Count |
|---|---|
| idle | 775 (603 via PgBouncer, 147 direct, 24 Grafana) |
| idle in transaction | 52.1 |
| active | 25.1 |
| io workers | 24 |
| parallel workers active | 4.2 |
| autovacuum workers | 1.2 |

**Active client backends by wait:**
- CPU (no wait event) 11.3;
- `Client/ClientRead` 12.9: extended-protocol sequences waiting on the
  client, not CPU;
- `IPC/BtreePage` 3.2: parallel btree scan coordination, mostly the network
  client listing query;
- `IO/DataFileRead` 2.4;
- `Timeout/VacuumDelay` 0.5;
- locks (`transactionid`) 0.07; `LWLock` waits about 0.06.

**Idle-in-transaction age:** 44.4 sessions under 10 ms, 6.1 under 100 ms, 1.7
under 1 s, none over 1 s. `idle_in_transaction_session_timeout` is 60 s.

**Daily means (Mimir):**

| Day | active backends (active_time) | idle in tx | pg backends | PgBouncer clients active | PgBouncer waiting (mean / max) | PgBouncer servers (mean / max) |
|---|---|---|---|---|---|---|
| 10-22 | 18.9 | 566 | 862 | n/a | n/a | n/a |
| 10-24 | 20.7 | 249 | 874 | 7,790 | 132 / 1,047 | 632 / 640 |
| 10-25 | 193.5 | 219 | 815 | 9,083 | 4,106 / 17,439 | 615 / 640 |
| 10-26 | 16.3 | 341 | 763 | 8,168 | 644 / 3,320 | 568 / 687 |
| 10-27 | 14.1 | 90 | 623 | 1,470 | 0 / 0 | 463 / 629 |
| 10-29 | 38.2 | 143 | 734 | 1,410 | 0 / 0 | 578 / 724 |
| 10-30 | 16.0 | 66 | 662 | 1,360 | 0 / 0 | 508 / 690 |
| 10-31 | 13.5 | 33 | 651 | 1,328 | 0 / 0 | 496 / 671 |

- **On 10-31:** PostgreSQL backends ranged 475–866 and PgBouncer server
  connections 300–680. Max wait and starvation were 0. Server logins were
  about 0/s, so there is no pool churn.
- **Connection churn:** 0.8 new backends/s from pg_stat_database sessions.
  Parallel-worker forks average 2.3/s (1.6M since the reset), which is
  negligible.

#### 2.7 Long snapshots: the dominant swing

**10-29, the `pg_dump` (`bringyour-backup.sh`).** It held one snapshot from
04:31Z until the owner's CPU rule stopped it at 17:45Z.

| Hour (Z) | busy | user | blks_hit M/s | tup_returned M/s | active backends | commits k/s | oldest tx (h) |
|---|---|---|---|---|---|---|---|
| 03:00 | 38.8 | 22.8 | 3.6 | 3.2 | 11.9 | 45.9 | 0.2 |
| 05:00 | 44.9 | 29.4 | 9.4 | 14.0 | 18.0 | 39.3 | 2.2 |
| 07:00 | 58.0 | 44.7 | 23.2 | 35.0 | 33.6 | 28.1 | 4.2 |
| 11:00 | 78.8 | 64.5 | 30.8 | 44.9 | 53.9 | 30.8 | 6.5 |
| 15:00 | 86.7 | 71.0 | 32.2 | 45.3 | 76.6 | 35.1 | 10.5 |
| 17:00 | 91.6 | 72.6 | 30.2 | 41.3 | 113.0 | 41.6 | 12.5 |
| 19:00 | 45.6 | 26.3 | 4.0 | 4.1 | 16.2 | 46.2 | 3.2 (autovacuum) |
| 20:00 | 39.0 | 23.3 | 3.7 | 3.4 | 12.8 | 44.1 | |

- **The work did not grow:** commits were flat or lower. The cost per
  statement grew as dead versions piled up behind the pinned horizon. The
  dump pipeline itself is at most about 4 cores (zstd -3 on 2 threads,
  bringyour-backup.sh:35-36).
- **Cost:** an excess of about 455 core-hours in 13.2 h, with about 5 h of
  the dump still to go.

**10-30 evening, the census.** Buffer hits rose from 4.8M/s (18:30Z) to
13.2M/s, then 18.1, 21.0, 23.2 and 24.9M/s. They fell to 12.5M/s at 22:00Z,
climbed again to 24.7M/s, and dropped to 3.7M/s at 01:00Z. Busy CPU went
33 → 59 cores. Commit 6a523f17d records that epoch 3's census statement "ran
past the close's three hours twice on 2026-10-30". The two ramps and the dip
between them fit two snapshots of about 3 h each. They are the same
signature as the dump.

**Which statements inflate.** Since-reset means against today's window:
- debit-journal age gauge: 46.8k → 4.95k hits/call;
- debit-journal shard cursor: 1,238 → 70;
- probe-cycle slot candidates: 13.0k → 1.07k;
- escrow-minus-debits: 355 → 171.

These are the tables with constant inserts, updates and deletes:
- transfer_debit_journal: 9.6k live rows in a 7.2 GB heap;
- provider_egress_probe_cycle: 0% HOT, 207 updates/s;
- pending_task.

#### 2.8 Autovacuum, freezing, checkpoints and WAL

**Autovacuum time since the reset,** PG 18's `total_autovacuum_time`
(190.4 h of wall clock):

| Table | Heap / indexes | autovacuum h (runs) | autoanalyze h (runs) | Notes |
|---|---|---|---|---|
| transfer_contract | 835 GB / 1,591 GB (18 indexes) | 166.0 (43) | 0.07 (38) | about 3.9 h per run, back to back; per-table cost_delay 2 ms |
| transfer_escrow | 468 / 871 GB (4) | 40.6 (48) | 0.30 (639) | 0 HOT of 463M updates |
| network_client | 95 / 396 GB (12) | 16.5 (2,292) | 0.01 (52) | 0 HOT of 215M updates |
| pending_task | 28 / 12 GB | 13.2 (44,105) | 15.9 (43,916) | thresholds 50/50; 2.7M rows, 97% `ApplyLegacyProviderTotals` |
| transfer_escrow_sweep | 157 / 188 GB | 11.6 (39) | 0.13 (260) | |
| contract_close | 413 / 275 GB | 10.2 (22) | 0.41 (903) | |
| provide_key | 96 / 104 GB | 5.4 (1,732) | 0.01 (46) | |
| transfer_debit_journal | 7.2 GB / 474 MB | 4.3 (46,496) | 1.2 (43,605) | 9.6k live rows, 258k dead |
| network_client_connection | 6.1 GB / 1 GB | 3.6 (3,525) | 0.04 (267) | |

**IO and CPU:**
- pg_stat_io, autovacuum workers in the vacuum context, since the reset:
  156.7 TB read (229 MB/s on average) and 11.3 TB written. Client backends
  read 56.3 TB; the checkpointer wrote 37.2 TB.
- About 60–65% of autovacuum's reads are transfer_contract cycles: each reads
  the heap plus all 18 indexes, about 2.4 TB. Most of the rest is
  transfer_escrow *(inferred from sizes × runs)*.
- autovacuum's CPU is small: one worker was 0.39 cores in `/proc`, 0.68 on CPU
  and 0.53 in VacuumDelay in the samples. Its cost is IO, dm-crypt and cache
  pollution.
- The run in progress at 18:32Z had been going 5 h 7 min. It was vacuuming
  indexes (14 of 18 done) with 38.1M dead item ids.

**Anti-wraparound recurrence:**
- datfrozenxid age was 329M at 18:30Z. Only transfer_contract is above 200M;
  the next oldest relation is at 199M.
- 8 days of 30-min samples show a sawtooth: 30 drops, one at each completed
  transfer_contract vacuum, 4–10 h apart. Floors after a drop were
  193–236M. Peaks over the last 3 days were 450M (10-30 04:21Z), 379M, 375M,
  334M and 326M.
- Between drops the age grows at the xid burn rate: median 4.9k xids/s, p90
  7.2k, 7.8k in the window.
- **The mechanism:**
  - An aggressive run freezes up to its starting OldestXmin minus
    `vacuum_freeze_min_age` (50M).
  - So the age it leaves behind is 50M plus the xids consumed during the
    run: 6–8 h at 18–28M/h is 160–280M.
  - That floor sits at or above `autovacuum_freeze_max_age` (200M), so the
    next forced run starts at once. transfer_contract is in a permanent
    anti-wraparound loop, and every run is aggressive.
  - The same burn forces aggressive runs on every other large table about
    every 7–11 h. transfer_escrow's 48 runs are about 1.7× the roughly 28
    that its dead-tuple and insert thresholds alone would trigger
    *(inferred)*.
- "195M → 230M in about 8 h" was one segment of this sawtooth. The rise is
  usually 18–28M/h.

**Checkpoints and WAL:**
- 2,190 timed and 13 requested checkpoints since the reset, one per
  `checkpoint_timeout` of 300 s. Each spreads about 264 s of writes. The
  checkpointer writes 54 MB/s on average.
- WAL averages 40.8 MB/s since the reset (28.0 TB); it was 68 MB/s in the
  window.
- Full-page images: 8.8k/s on average, 13.1k/s in the window. A regression
  over pg_stat_statements gives about 104 bytes per WAL record and about
  3.8 KB per lz4-compressed FPI. So FPIs are about 73% of WAL bytes.
- `wal_buffers_full` occurred 13.75M times (20/s).
- WAL CPU (lz4, CRC, walwriter, dm-crypt) is about 0.5–0.8 cores
  *(inferred)*; its IO cost is larger.

#### 2.9 transfer_contract WHERE open

**The open set:**
- About 79M rows: pg_stats gives open=true at 2.75% of 2.886B; attstattarget
  is 300 and the July planner trap is not armed. The other agent counts about
  96M expired.
- The force-closer runs at 134/s (both sides 112/s, source accepts 13/s,
  destination accepts 9/s), out of 1.67k closes/s.

**Partial indexes over the open set:**

| Index | Size | Entries |
|---|---|---|
| `transfer_contract_open_partial_create_time` | 9.9 GB | 79.2M |
| `transfer_contract_outcome_null` | 18 GB | 79.2M |
| `transfer_contract_unresolved_source_pair_create_time` | 77 GB | 79.2M |
| `transfer_contract_unresolved_destination_pair_create_time` | 76 GB | 79.2M |
| `transfer_contract_unresolved_payer_transfer_byte_count` | 36 GB | 53.3M |
| **Total** | **217 GB** | |

Densely packed, these entries need about 25–30 GB *(inferred)*. The
difference is pages left from the open-set peak (about 174M on 10-27) and
random-key fragmentation. Every transfer_contract vacuum cycle scans all
217 GB.

**Statements filtering on the open set, since the reset:**

| Statement | calls/h | mean | Notes |
|---|---|---|---|
| prober contract classification | 1.13M | 0.23 ms | |
| unresolved-pair EXISTS | 1.68M | 0.06 ms | |
| unresolved union | 69k | 1.7 ms | |
| force-close page | 3,018 | 22 ms | 10.2k hits per call |
| force-close page, second variant | 783 | 14.7 ms | |
| prober-shard scoped contracts | 402 | 428 ms | 18.5k hits, 2.0k reads |
| payer open-sum | 44k | 3.5 ms | 22.6 s/h of JIT |
| expired-by-payer | 46k in total | 101 ms | 0.26 cores on CPU in the samples |
| monitor open-count probes | 3/h | 0.55–0.58 s | |
| TABLESAMPLE censuses | 1 call each | 3–6 s | |

- All open-set statements together cost about 0.3–0.4 cores.
- The force-closer's own selection is 2.5 pages/s × 16.5 ms, about 0.04 cores.

**Plans:**
- **E1, the force-close page.** An index scan on
  `transfer_contract_open_partial_create_time` (index condition on the
  create_time range, row-comparison filter), an incremental sort with
  create_time presorted, LIMIT 1000, and LATERAL index-only probes of
  `contract_close_pkey`. Estimated cost 1,138. It is cheap.
- **E4, the payer open-sum.** An index-only scan on
  `transfer_contract_unresolved_payer_transfer_byte_count`. Each call costs
  in proportion to the payer's open contracts, so a big payer's stale opens
  are walked on every call.
- **What the stale open set costs:**
  - about 190 GB of excess index pages in every vacuum cycle, about 1 TB/day
    of reads;
  - payer-scoped walks;
  - the force-close updates themselves, each a non-HOT update touching about
    9 indexes. Closing is required work.

#### 2.10 The SN census on 11-06

**What runs.** The whole-window read (model/st_provider_usage.go:76-97,
470-498, commit 6a523f17d):
- one REPEATABLE READ read-only transaction on a maintenance session, without
  the 15-minute transaction bound;
- `enable_indexscan = off`, `enable_seqscan = off`, no parallel workers and
  a forced custom plan;
- the caller's 2-hour step bound still applies.

**EXPLAIN** for [10-30, 11-06) plans a Bitmap Heap Scan on transfer_contract
fed by `transfer_contract_closed_usage`, estimating 185M rows. It hash-joins
the 13.4M-row archive. The undated probe uses the empty
`transfer_contract_usage_missing_time` index. The plan JIT-compiles 400
functions with inlining and optimization, a few seconds of CPU.

**Expected cost:**
- **The read:** 185–400M rows at the commit's measured 125–165K rows/s on
  cold pages is 20–50 min. That is one backend at about 1 core, plus 4 decode
  workers in taskworker.
- **IO:** about 35% of 105M heap pages (about 300 GB) plus about 10 GB of
  index, an extra 100–250 MB/s while it runs.
- **The snapshot pin:** 20–50 min. On the 10-30 curve, buffer hits roughly
  triple within 30 min, which means about +10 cores by the end of the read and
  about 5–10 core-hours of extra CPU on edge-2 in all, recovered within about
  30 min. The two 3-hour attempts on 10-30 cost about 100 core-hours.

### 3. Ranked opportunities

#### #1 Keep long snapshots off the primary

**Evidence:** §2.7. The dump cost about 455 core-hours in 13.2 h (+34 cores
mean, +55 peak). The 10-30 census attempts cost about 100 core-hours. Both
recovered within an hour of the snapshot closing. A resumed Sun/Thu dump would
average 3–7 cores over each week *(inferred: about 600 core-hours per complete
run, two runs a week)*.

**Changes:**
- **Backups.**
  - Use the physical base backup that already exists:
    xops/main/ansible/playbook-pg-basebackup.yml, pulled by edge-5 at
    `pg_basebackup_max_rate`. It compresses and encrypts on edge-5 and, in its
    own words, "holds no snapshot and no lock" on the primary.
  - Retire the `pg_dump` path: host_files/by-us-fmt-5-edge-2/backup/bringyour-backup.sh:60
    and the `db-backup` block in playbook-dbs.yml:1823.
  - The play's timer flag is `pg_basebackup_timer_enabled`
    (playbook-pg-basebackup.yml:303).
- **Census.**
  - Keep the bitmap read (6a523f17d) and schedule the epoch read for the
    lowest-traffic hours after the close.
  - The structural fix is a streaming replica with
    `hot_standby_feedback = off`. `server.ReplicaDb` already exists for this.
- **Retention cascade.** The contract delete cascade (`WITH deleted_report AS
  (DELETE FROM contract_close_report …`) averages 410 s per call, with a
  maximum of 899 s, about every 5 h. Bound its batches so that each
  transaction lasts seconds.

**Correctness:**
- No timing bound changes: the census keeps its 2-hour step bound and its
  REPEATABLE READ consistency.
- A physical backup is a consistent restore point by construction (WAL
  streamed with `-X stream`).
- Restore must be rehearsed (postgres-backup/restore.md in xops).

**Risk:** low–medium. The backup format changes, and edge-5's drives are
shared with MinIO; the play reserves headroom for MinIO.

**Validate:**
- Run `pg-basebackup.service` by hand off-peak.
- `pg_stat_activity_max_tx_duration` should show no long application or
  backup transaction.
- `pg_stat_database` blks_hit/s and edge-2 busy CPU should stay within ±10%
  of baseline. Compare with the 10-29 hourly table.
- On 11-06, watch blks_hit/s during the census: expect at most about 3×,
  recovering within 30 min.

#### #2 Remove the nginx hop to PgBouncer

**Evidence:** §2.2. nginx costs 4.05 cores, 80% of it kernel time, and adds
one loopback TCP hop each way to every one of 104k statements/s. Its
softirq share is about 0.5–1 core more *(inferred)*. Under load on 10-26 the
hop added 3.5 ms (7.4 ms through PgBouncer against 10.9 ms through nginx, from
the pgbench timing in that day's notes).

**Changes.** Either option keeps PgBouncer's settings unchanged.
- **(a) Spread connections on the client side.** A pgx `DialFunc` in db.go,
  next to `configurePgPoolLiveness` at about db.go:320, picks one of the
  configured PgBouncer ports 6433–6464 per new connection. pgx sends cancel
  requests to the address the connection dialed, so they reach the right
  instance without peering.
- **(b) PgBouncer `so_reuseport = 1` on 6432** (pgbouncer.ini.j2:356, with
  `listen_port` at :59), with `[peers]` (:421) over the per-instance Unix
  socket directories, and nginx.conf.j2:30 retired.

Option (a) can be rolled out block by block.

**Correctness:**
- nginx's only timing bound is `proxy_timeout 300m` (nginx.conf.j2:33), an
  idle close. Keep it by setting PgBouncer `client_idle_timeout = 18000`
  (pgbouncer.ini.j2:321).
- The pools already close idle connections at 60 min and recycle them at
  8 h, and `query_wait_timeout` and server timeouts are unchanged.
- nginx's `least_conn` balancing becomes random placement over 32 instances
  of about 44 connections each.
- nginx also skips a failed instance for 30 s (`max_fails=3
  fail_timeout=30s`). Under option (a), the dialer must reproduce that: on a
  refused or timed-out dial, try another port within the same
  `connect_timeout`.
- `proxy_connect_timeout` becomes moot, because clients connect to
  PgBouncer directly under their own `connect_timeout`.

**Risk:** medium. This is the DB entry path, and moving 6432 resets client
connections; pgx reconnects.

**Validate:**
- Canary one api block on option (a).
- Measure PgBouncer CPU per query (SHOW STATS per instance with `/proc` per
  PID), nginx CPU, edge-2 softirq, and the client-side query latency
  histogram.
- Expect nginx CPU to scale down with the moved share, and lower latency.

#### #3 PgBouncer → PostgreSQL over a Unix socket

**Evidence:** §2.2–2.3:
- PgBouncer's 4.07 cores of sys time, about half of it on server
  connections;
- backends' 4.4 sys cores (`secure_write` → `tcp_sendmsg`, about 4.7% of
  PostgreSQL CPU is protocol send);
- loopback softirq.

Unix sockets skip TCP, the IP layer and the netfilter hooks.

**Change:**
- pgbouncer.ini.j2:18:
  `bringyour = host=/var/run/postgresql port=5432 dbname=bringyour user=bringyour`.
- Add `local bringyour bringyour scram-sha-256` to pg_hba.conf ahead of
  `local all all peer`; today local connections use peer auth only. Reload
  only.

**Correctness:** no bound is involved. `server_connect_timeout` and the login
retry are unchanged.

**Risk:** low, and per-instance reversible.

**Validate:**
- Change one of the 32 instances.
- Compare its CPU per query and its server connections' backend sys time
  against the other 31 for an hour.
- Expected 1–2 cores fleet-wide *(unmeasured)*.

#### #4 Reboot edge-2 into the staged kernel options

**Evidence:**
- `/sys/devices/system/cpu/vulnerabilities/retbleed` reads "Mitigation:
  IBRS".
- The inventory stages `kernel_cmdline_options: retbleed=stuff iommu=pt`
  for edge-2 (inventory.yml:71).
- edge-2 runs about 11 cores in the kernel (sys 7.5, softirq 3), mostly on
  syscall-heavy PgBouncer, nginx and backend sockets. IOMMU mapping frames
  appear in their samples.

**Savings:** 1.5–3 cores *(unmeasured; CPUOPT2 §3.7 estimated 3–8% for the
services edges)*. #2 and #3 shrink this item.

**Change:** a planned PostgreSQL restart with the reboot.

**Correctness:** no timing bound is involved. `retbleed=stuff` keeps the
mitigation by a cheaper mechanism.

**Risk:** medium, because of the primary's downtime. It needs an owner window.

**Validate:** sys and softirq per commit, and PgBouncer and nginx CPU per
query, against this report's 10-31 baseline.

#### #5 Fewer round trips per transaction

**Evidence:** §2.4. 19.4k/s of transaction-control and lock statements. Each
round trip costs about 93 µs in PgBouncer and nginx plus about 30–50 µs of
PostgreSQL protocol work *(inferred from §2.2–2.3)*. Examples:
- `SELECT pg_backend_pid()` at 1.8k/s, issued after admission
  (db_owned_transaction.go:195 and :359), although the admission statement
  `SELECT pg_backend_pid(), pg_try_advisory_xact_lock(…) FROM unnest(…)`
  already returns the PID;
- `pg_advisory_unlock` and `pg_advisory_unlock_all` at 1.0k/s;
- standalone advisory-lock statements at about 4.6k/s that could ride on the
  transaction's first statement.

**Change:**
- Return and validate the backend PID in the same statement that takes the
  ownership locks.
- Coalesce unlocks.
- Where a transaction's first statement is known, send it with BEGIN, as
  db.go:1037 already does for `lock_timeout`.

**Correctness:** the same validations and locks, in fewer messages. The
ordering inside one statement must keep the PID check before the business
callback.

**Savings:** about 10k statements/s fewer ≈ 1–2 cores, split across PgBouncer,
nginx and PostgreSQL.

**Risk:** low–medium: ownership and fencing code.

**Validate:** PgBouncer `total_query_count / total_xact_count` (2.8 now)
together with edge-2 CPU per transaction.

#### #6 Measure and cut planning

**Evidence:**
- Planning is 11.8% of PostgreSQL CPU (about 3.5 cores) despite prepared
  statements, with 55 server re-prepares/s.
- Two sources are likely:
  - custom plans for parameter-sensitive statements;
  - cache invalidations. Every vacuum or analyze of a table updates pg_class
    in place and sends a relcache invalidation, which drops every backend's
    cached plans on that table.
- pending_task and transfer_debit_journal are vacuumed and analyzed about
  every 16 s each. That is about 88–90k events per table since the reset;
  pending_task's thresholds are 50 rows (db_migrations.go:5678-5683).
- About 830 backends then re-plan their task and debit statements
  *(inferred)*.

**Changes:**
- First measure: `pg_stat_statements.track_planning = on` for about 15 min
  off-peak (superuser-settable, no restart; it adds some contention on hot
  entries, hence the short window). Rank statements by plans/calls and plan
  time.
- Then raise those tiny tables' `autovacuum_analyze_threshold` (for example
  50 → 20k on pending_task), keeping their vacuum cadence.
- For statements that stay on custom plans, use per-statement plan settings
  (as the census does with `SET LOCAL plan_cache_mode`) or restructure the
  query.

**Correctness:** analyze cadence is planner-statistics freshness, not a
timeout or detection bound. A queue table's value distribution barely changes
between analyzes.

**Risk:** low. Check that the poll stays on `pending_task_function_poll_order`
after the change; a July incident came from that plan.

**Validate:** the LBR planning share (11.8% now) and pg_stat_statements plan
counts.

#### #7 transfer_contract index cuts

**Evidence:**
- **`transfer_contract_reap_pending_create_time`:**
  - 82 GB with 2.66B entries; 283 scans since the reset, returning 0 tuples.
  - Its only reader is the straggler stamp (model/subscription_model.go:6455-6475),
    with a 300-day bound (subscription_model.go:47).
  - The oldest contract was created 2026-06-01, 152 days ago, so no row can
    match before 2027-03-28.
  - EXPLAIN E3 shows the full `transfer_contract_create_time` index serving
    the same range at cost 2.41.
- **`transfer_contract_stream_id`:** 13 GB with 546M entries, 0 scans in 8
  days. Its reader is the weekly orphan sweep's participant probe
  (model/contract_stream_binding.go:6 note).
- **`transfer_contract_open_source_id`:** 316 GB, keyed `(open, source_id,
  destination_id, contract_id)`, with 386M scans. Worth checking whether a
  `WHERE open` partial index, or the unresolved-pair indexes, serve its
  readers.

**Change:**
- Point the straggler scan at `create_time` without the partial predicate
  (E3), then `DROP INDEX CONCURRENTLY` through a migration. The index was
  created at db_migrations.go:4155.
- Probe participants from `contract_participant` and drop `stream_id`.

**Savings:**
- 95 GB less per vacuum cycle, about 4%, about 0.5 TB/day of reads.
- One less index insertion on about 1.5k closes/s; fewer FPIs.
- Smaller `ExecOpenIndices` and planner index lists for transfer_contract,
  which takes 36% of exec time (§2.4).
- 0.2–0.5 cores *(inferred)*.

**Correctness:** no bound is involved. The straggler query keeps the same
predicate and 300-day rule.

**Risk:** low. A drop needs no heap scan, unlike REINDEX.

**Validate:** autovacuum duration per cycle on transfer_contract, pg_stat_io
autovacuum reads per day, the WAL FPI rate, and the transfer_contract
INSERT/UPDATE means.

Note: REINDEX CONCURRENTLY of the bloated open-set partials (217 GB, §2.9)
would read the 835 GB heap twice per index. A partial index cannot use PG's
safe-index-build path, so each build would also pin the horizon for its heap
scans, which brings §2.7's CPU cost. Defer it until the stale open set has
drained or partitioning lands.

#### #8 Break transfer_contract's anti-wraparound loop

**Evidence:** §2.8:
- 166 of 190 h spent vacuuming one table;
- floors at or above `autovacuum_freeze_max_age`;
- peaks up to 450M;
- the worker spends about 44% of its sampled time in VacuumDelay, from the
  per-table `autovacuum_vacuum_cost_delay = 2` (db_migrations.go:4375-4376).

**Changes**, through migrations, the only place autovacuum is configured:
- Make cycles shorter: #7's index cuts, then a moderate unthrottle, for
  example `autovacuum_vacuum_cost_limit` 200 → 400–800 at the same 2 ms
  delay.
- Lower the per-table `autovacuum_freeze_min_age` to about 10–20M, so each
  run leaves the age at its duration's burn plus 10–20M rather than plus 50M.
- With the floor below 200M, raise the dead-tuple threshold (5M → about
  25M, matching transfer_escrow), so cycles stop running back to back.
- The structural fix is contract partitioning (CONTRACT-PARTITION.md, merged
  a8050a286, not cut over). Old weekly partitions freeze once and are never
  scanned again.

**Savings:**
- CPU is small (autovacuum is under 1 core). IO: −30–50% of the 229 MB/s of
  autovacuum reads *(inferred)*.
- Freeze-age peaks fall toward about 200M, which restores the wraparound
  margin.

**Correctness:** these are maintenance thresholds, not timing bounds.
`vacuum_failsafe_age` stays at 1.6B.

**Risk:** medium. In July, three unthrottled vacuums evicted the buffer cache
(the autovacuum-tuning.conf note, xops host_files/…/autovacuum-tuning.conf).
Raise the limit stepwise and watch `BufferMapping` waits. The 256 MB vacuum
ring helps. Lower freeze_min_age re-freezes pages that are later re-dirtied.

**Validate:**
- the freeze-age sawtooth (floor and peak);
- transfer_contract `total_autovacuum_time` per day;
- pg_stat_io autovacuum reads per day;
- sda utilization;
- BufferMapping in the sampled waits.

**Owner decision, not recommended by default:**
- Raising the global `autovacuum_freeze_max_age` (200M → 400–500M, restart
  required) would halve the forced runs on transfer_escrow and the other
  large tables.
- But it removes margin before the failsafe, and it does not help
  transfer_contract, whose runs are triggered anyway.

#### #9 JIT off for OLTP

**Evidence:**
- 45.1 h of JIT since the reset (0.24 cores on average); 0.06 cores in the
  window.
- The worst cases are misestimated OLTP statements:
  - escrow and debit lookup: 0.61 ms of JIT per 0.63 ms call;
  - payer open-sum: 22.6 s/h of JIT;
  - GetTransferStats: 136 s/h.

**Change:** `jit = off` in the edge-2 postgresql.conf (xops host_files,
currently default on; reload only), or `jit_above_cost` at 10M or more.

**Correctness:** no bound is involved. The rare analytics lose JIT speedups,
which are a small share of their runtime.

**Risk:** low.

**Validate:** the pg_stat_statements jit_* deltas should go to 0 and the
latency tail of the affected statements should drop.

#### #10 Compute provider-census inputs once

**Evidence:**
- The health-window aggregate (`SELECT client_id, MAX(measured_at), … FROM
  provider_egress_health_history …`) runs 415 times/h at 1.39 s and 592k hits
  per call (0.16 cores). It is called from several places:
  - model/network_client_location_model.go:4111 and :5644;
  - taskworker/work/provider_egress_probe_metrics.go:837;
  - the query itself is at model/provider_egress_health_model.go:505.
- Its siblings are large reads repeated per caller:
  - the cohort and eligibility queries, 7.5–22.5 s per call with 10–12M
    hits;
  - the location-reliability and city, region and country listing;
  - the client listings, 1.6–2.5 s.
- Together these are about 0.6–0.8 backends busy *(inferred from
  pg_stat_statements)*.

**Change:**
- One task computes each window's snapshot and publishes it; every reader
  takes the published copy.
- Keep the exact evidence window the code comments require
  (network_client_location_model.go:5640-5643).

**Savings:** 0.3–0.6 cores.

**Correctness:** the same window and the same population. Freshness is bounded
by the existing window, not lengthened.

**Risk:** low–medium.

**Validate:** calls/h of those query ids.

#### #11 GetTransferStats unpaid bytes

**Evidence:** about 0.5 calls/s in the window, 199 ms and 511k hits per call
(8.5% of all statement buffer hits). Since the reset: 1,224 calls/h at 349 ms,
with 136 s/h of JIT (model/account_payment_model.go:1148-1177). The heaviest
networks hold millions of unpaid sweeps.

**Change:** keep a per-network unpaid total, updated in the same transaction
as sweep insert and payment, and read it in O(1). Do not lengthen the
existing 60 s cache.

**Savings:** 0.1–0.15 cores, plus IO.

**Risk:** low, with an equivalence test against the SUM.

**Validate:** this query id's hits per second.

#### #12 transfer_debit_journal dead-entry walk

**Evidence:**
- `transfer_debit_journal_pkey` returned 475k index entries/s in the 60 s
  snapshot, but only 1.3k live rows (370:1).
- The table has 9.6k live rows in 7.2 GB of heap and 258k dead.
- It drives `heap_hot_search_buffer` and `heap_page_prune_opt` (§2.3), and
  it is the first family to inflate when a snapshot pins the horizon (§2.7).
- `applied` is deliberately unindexed so that marking a row applied stays
  HOT (model/subscription_transfer_debit.go:42-45). Keep that.

**Change:**
- Shrink the heap once: VACUUM FULL in an owner maintenance window, a short
  ACCESS EXCLUSIVE lock on a small live set, or PG19 REPACK when it arrives.
  pg_repack is excluded by owner rule.
- Have the flush delete applied rows in the key order its readers scan.

**Savings:** 0.1–0.3 cores now; much more during pins.

**Risk:** low–medium.

**Validate:** the idx_tup_read : idx_tup_fetch ratio and n_dead_tup.

#### Not proposed (owner rule or poor trade)

- **Raising `checkpoint_timeout`.** FPIs are about 73% of WAL bytes, so a
  longer interval would cut WAL and checkpoint IO sharply. But it is a timeout
  and lengthens crash recovery. Fewer random-key index inserts (#7, #8) reduce
  FPIs instead.
- **Lengthening cache lifetimes**, such as GetTransferStats' 60 s cache, or
  loosening the census's 2-hour step bound, the 15-minute transaction bound
  or `idle_in_transaction_session_timeout`.
- **Lowering parallelism globally.** Parallel btree scans in the client
  listings show more coordination waits (BtreePage 3.2 + 2.6) than CPU (1.15
  cores). That is a per-query index question, not a global setting.
- **Shrinking PgBouncer pools.** There is no churn and no starvation. Idle
  server connections cost memory and a little `GetSnapshotData` (0.35%), not
  enough to justify the risk.

### 4. CPUOPT2 predictions vs what happened on edge-2

CPUOPT2 measured no PostgreSQL baseline, so the comparison is indirect.

- **Materialized: services-edge CPU saturation drove PostgreSQL
  idle-in-transaction and the PgBouncer queue.**
  - CPUOPT2 and the 10-26 notes made this claim. As the services edges
    gained headroom, idle-in-transaction fell from a 566 mean (10-22) to 33
    (10-31).
  - PgBouncer waiting went from a 4,106 mean on 10-25 to 0 since 10-27, and
    clients from 8–9k to 1.3k.
  - The queue disappeared on 10-27, before most CPUOPT2 deploys. The 10-26
    nginx `reuseport` fix and other changes also contributed.
- **Partly: database transaction volume fell.**
  - The 10-26 note put JWT state checks at about 17k transactions/s (about
    40% of PgBouncer). Commits fell from 41–46k/s (10-22..10-27) to
    30–33k/s (10-30/31).
  - The recursive client-lineage check fell from 6,967/s (since-reset mean)
    to 1,448/s.
  - DB data cannot attribute the drop to a specific change. It began on
    10-28.
- **Unchanged: "taskworker is database-bound (the debit flush transactions)".**
  Debit-journal statements are 3.6% of exec time, and the dead-entry walk
  (#12) remains.
- **Not yet: the kernel change.** `retbleed=stuff iommu=pt` is live on the
  services edges but only staged on edge-2. IBRS is still in effect (#4).
- **New since CPUOPT2:** long snapshots are now the largest CPU risk on the
  primary (§2.7). One table's vacuum, transfer_contract's, sets the cluster's
  wraparound age (§2.8).

### 5. Files

Scripts and raw aggregates are in
<scratch>/cpuopt3/:
- scripts/: the pg.sh runner, host probe, perf and LBR classifier, PgBouncer
  stats;
- sql/: each read-only battery;
- work/: pg_stat_statements snapshots, ranks, deltas, the activity samples,
  the perf output and the Mimir scripts.

All query text in work/ is pg_stat_statements' normalized form with literals
stripped.

## Part E — The Redis cluster

This section is read-only research. All numbers come from Main on 2026-10-31,
from Mimir, `/proc` and short perf samples on the Redis hosts and on one
connect and one api process, plus a code read of server origin/main
964c489c0. Nothing in production was changed. Keys, values, script
arguments, client addresses and identifiers were never printed. *(inferred)*
marks a claim reasoned from code rather than measured.

### Summary

- **The cluster costs about 26 cores, not 14.** Main threads average 13.9
  cores (48 nodes), and the `io_thd_1` I/O threads cost about as much again.
  Process CPU averages 26.3 cores and peaks at 34 (10-28 18:00Z to 10-31
  18:00Z). Redis is about 95% of edge-6's busy CPU.
- **Requests drive the cost, not commands.** About half of Redis CPU is
  kernel time, and the I/O thread is about 47% of it. Outside command
  execution, each request (read event) costs about 110 µs of CPU on edge-6
  and 73 µs on edge-5, with 1.4–1.8 thread sleep/wake cycles. All-in, Redis
  spends about 200 µs of CPU per request at 120–180k requests/s. So the
  biggest lever is fewer round trips.
- **Commands:** scripts are about 70% of command time: EVAL is 4.5 cores at
  62k/s, and EVALSHA is 1.5 cores at 16k/s. EVAL sends the full script text
  each time, about 50–75 MB/s of it, and Redis SHA1-hashes every body (about
  1.5% of samples).
- **The 0.89 spike on 10-31 is one O(N) Lua loop.** `count_clients` in the
  provider-intent scripts scans a large network's connected members, at
  about 8 µs per member, on one slot on node 6416.
  - The slowlog holds 46 such calls in 8 minutes (18:36–18:44Z, with no
    deploy running), averaging 340 ms and up to 452 ms. That is one stall
    about every 11 s, and each blocks every key on that node.
  - Reconnect waves multiply it: the connect restarts at 02:07–02:36Z, and a
    provider wave at 08:47Z with no deploy.
- **The cheapest win is a PING per call.** Contract-hole reads (13.1k/s, on
  the packet path) and contract-hole events (4.0k/s) go through
  `RedisWithDeadline` without `OptRedisNoPreflight`. Each one sends a PING to
  an arbitrary node first. That accounts for nearly all of the 14–18k PINGs/s,
  about 11% of all requests.
- **Host costs on edge-5/6:**
  - IOMMU map/unmap is 3.3–3.7% of samples (`iommu=pt` is staged, not yet
    rebooted).
  - The SRSO return thunk is 1.6% of samples on edge-6 and 7.2% on edge-5.
  - On edge-6 (8 NUMA nodes), NUMA balancing migrates 36.8k pages/s, about
    96% of them Redis's. The median Redis process holds only 44% of its pages
    on its majority node.

**Ranked opportunities.** These are shares of Redis CPU (26–28 cores). They
overlap, so do not add them up.

| # | Change | Saving | Risk | Effort |
|---|---|---|---|---|
| 1 | No preflight PING on contract-hole reads, events and readiness (§4.1) | −13–16k round trips/s; ≈1.2–1.6 cores (≈5%); a round trip off each cold packet-path read | low | hours |
| 2 | Provider-intent `count_clients`: an exact batched scan, then an exempt index (§4.2) | ends the 100–450 ms single-node stalls; 6416's wave peak 0.89 → ≈0.3–0.4 | low–medium | days |
| 3 | EVAL → EVALSHA for the hot scripts (§4.3) | ≈0.5–0.7 cores (2–2.5%); 55–75 MB/s less inbound, 40–50% of bytes | low | 1–2 days |
| 4 | Host: pin each redis-server to one NUMA node with bound memory; activate the staged `iommu=pt` (§4.4) | IOMMU ≈3.5%; NUMA ≥1.5–2% direct plus locality (measure on one node) | medium: restarts with no replicas | days, via xops |
| 5 | One round trip instead of two: GETEX in resident polls; prune inside the peer-heartbeat MULTI (§4.5) | −6–7k round trips/s; ≈0.6 cores | low | hours |
| 6 | Turn on `cluster-slot-stats-enabled` (§4.6) | finds the hot slot in one command, without MONITOR | low | hours, via xops |
| 7 | `{provider_hard_exclusions}` single slot on 6415: a generation-checked local snapshot (§4.7) | 6415 −57–115 ms/s, 6–11% of its main thread | medium | days |
| 8 | Backstop TTLs armed once, not on every write (§4.8) | ≈0.1–0.2 cores | low | hours |
| — | Decisions and experiments (§4.9): io-threads 1 A/B, SRSO posture, edge-5 co-location | see §4.9 | — | — |

### 1. Method

- **Mimir, 10-21 to 10-31 18:25Z** (redis_exporter scrapes every ~90 s):
  - main-thread and process CPU per node;
  - commandstats calls and µs per command;
  - reads processed, bytes, clients, connections, memory, expiry, persistence;
  - app counters for contract holes, reservations, provider intent, pools and
    leases.
  - Windows:
    - CPUOPT2's window (10-22 to 10-29). It includes the 10-23 edge-2 outage,
      so its averages read low.
    - The last 7 days, the last 3 days and the last 24 h.
    - The 8 h after the 10-31 api, alt, taskworker and proxy deploys
      (10:00–18:00Z).
- **On the Redis hosts, 10-31 18:30–19:10Z, at evening load:** 690–750k
  commands/s, 147–179k requests/s, about 93k clients. Everything ran through
  `ansible -m script`; aggregates were made on the host.
  - Per-thread CPU and context switches from `/proc`, over a measured 15–23 s.
  - perf, 30 s at 99 Hz with call graphs, on node 6383 (edge-6, median load,
    2,538 samples) and node 6420 (edge-5, 2,662 samples). Data files were
    deleted on the host.
  - For all 48 nodes: `INFO everything`, `SLOWLOG GET 128` (command names
    only, and EVAL bodies reduced to a SHA1 fingerprint of their first 64
    bytes), `LATENCY LATEST`, and `CLIENT LIST` aggregated by source host
    name.
  - NUMA, cgroup, swap and vmstat counters.
- **Client side, edge-3, 10-31 ~19:00Z:** perf, 30 s at 99 Hz, on one connect
  g-block process (4.6 cores, 21,413 samples) and one api process (1.1 cores,
  6,933 samples). Samples under go-redis frames were attributed to the first
  urnetwork caller.
- **Script mapping:** a Go tool fingerprinted every Lua literal in the server
  tree, matched the slowlog fingerprints, and gave script sizes.
- **Not available:** `cluster-slot-stats-enabled` and `latency-tracking` are
  both off, so there are no per-slot CPU or per-command latency histograms.
  Hot slots were inferred from per-node command mix plus code.

### 2. Where the CPU goes

#### 2.1 Totals and trend

| Window | Main threads (cores) | Process (cores) | Commands/s | Requests/s |
|---|---|---|---|---|
| 10-22, 24 h (before CPUOPT2 changes) | 11.4 | 21.6 | 472k | — |
| CPUOPT2 window 10-22..10-29, avg (peak) | 12.0 (18.7) | 24.0 (42.1) | 628k | — |
| 10-28 18:00 to 10-29 18:00 | 13.6 | 26.0 | 570k | 131k |
| Last 3 days, avg (peak) | 13.9 (16.6) | 26.3 (34.1) | 608k | 120–150k |
| 10-31 10:00–18:00 (after deploys) | 15.0 | 28.2 | 693k | ≈147k (1 h at 18:00) |

- **Hosts:**
  - edge-6 averages 15–17% busy (about 20 of 128 logical CPUs, peak 23–30),
    and Redis is about 95% of that.
  - edge-5 averages 16–19% busy. Redis is 7.5–8.2 cores of it; MinIO is
    about 7.8.
- **Per command:** Redis process CPU per command fell from 45.7 µs (10-22) to
  40.7 µs (10-31 8 h), while command volume grew 47%.
- **The 42-core peak:** this and the per-node 0.95–0.98 peaks in CPUOPT2's
  window are the 10-24 to 10-28 escrow and grant incidents (§2.2). Since
  10-28 02:00Z no node has exceeded 0.63 at the 2-minute resolution. At irate
  resolution (one ~90 s scrape) only 6416 has, at 0.89 (§2.6).

#### 2.2 Per node

The 7-day p99 of main thread per node is a median of 0.40 (0.36 over the last
3 days). Nodes with a 7-day p99 at or above 0.77:

| Node | Host | Avg | p99 | Max (5 m) | What made it hot |
|---|---|---|---|---|---|
| 6396 | edge-6 | 0.41 | 0.96 | 0.99 | grant-exhaustion storms 10-25/26/27; now verify-egress HSCAN 495/s |
| 6408 | edge-6 | 0.38 | 0.96 | 0.98 | grant storms 10-25..10-28 |
| 6394 | edge-6 | 0.35 | 0.89 | 0.97 | grant storms 10-26..10-28 |
| 6421 | edge-5 | 0.28 | 0.86 | 0.93 | 10-24 single-slot escrow outage |
| 6387 | edge-6 | 0.32 | 0.83 | 0.89 | 10-24 escrow outage |
| 6419 | edge-5 | 0.29 | 0.77 | 0.92 | 10-24 escrow outage |

- **Over the last 24 h the cluster is even.**
  - Median node: main 0.29, 12.3k commands/s, 2.8–3.6k requests/s.
  - Busiest: 6404 at 0.375, 6392 at 0.368.
- **Hot slots now, inferred from per-node outliers plus code:**

| Node | Excess over the median node | Key family *(inferred from code)* |
|---|---|---|
| 6416 | 25.1k commands/s vs 12.3k; ZSCORE 5.8k/s vs 93; HGET 3.1k/s vs 122; ZADD, EXPIRE, MULTI/EXEC, ZREMRANGEBYSCORE 2–4×; 4.8k clients vs 1.8k | `{np_<network>}` of one large provider network: the provider-intent scans (§2.6) and provider-install heartbeats |
| 6415 | SMISMEMBER 96/s at 625 µs (60 ms/s) | `{provider_hard_exclusions}` (model/egress_index.go:1005) |
| 6396, 6424 | HSCAN 495/s each at 20 µs | verify-egress forward-lease hashes of the two proxy hosts' addresses (bounded since 10-25) |
| 6420, 6392, 6408 | TYPE 600–700/s vs 200 (4 per reservation call) | busy `{escrow_<balance>}` balances |
| 6404, 6392, 6399 | SPUBLISH 100–157/s | contract-origin notification shard channels |

#### 2.3 Threads, kernel and the cost per request

- **Thread split:** every node's I/O thread uses about as much CPU as its
  main thread.
  - On edge-6, 32 main threads use 11.5 cores and 32 I/O threads 11.3.
  - Main threads are about 72% user. I/O threads are about 69% system time.
  - bio and jemalloc threads use about 0.
- **Per request:** a regression across 48 nodes and nine 3-hour windows
  (10-29 18:00 to 10-31 18:00) gives the cost per read event outside command
  execution:

| Host | I/O thread | Main thread outside commands | Total |
|---|---|---|---|
| edge-6 (EPYC 7601, Zen 1, 8 NUMA nodes) | 83.5 µs (R² 0.95) | 28 µs | ≈111 µs |
| edge-5 (EPYC 7352, Zen 2, 2 NUMA nodes) | 47.0 µs (R² 0.97) | 25.5 µs | ≈73 µs |

  All-in process CPU per read event has stayed at 190–223 µs every hour since
  10-28. Only 55–56% of main-thread event-loop time is inside commands
  (`eventloop_duration_cmd_sum ÷ eventloop_duration_sum`).
- **Wakeups,** measured over 15–23 s:
  - edge-6: I/O threads sleep 104.6k times/s and main threads 57.4k/s, for
    114.8k requests/s. That is 1.41 sleep/wake cycles per request.
  - edge-5: 77.0k and 38.0k per second for 64.0k requests/s, or 1.8 per
    request.
  - At about 3–4k requests/s per node, the two threads hand each request back
    and forth through eventfd, and mostly sleep in between.
- **perf, as a share of process samples:**

| | 6383 (edge-6) | 6420 (edge-5) |
|---|---|---|
| Syscalls (`do_syscall_64`) | 46% (write 20%, epoll_wait 16%, read 5%) | 49% (write 21%, epoll_wait 17%, read 6%) |
| Softirq inline (`net_rx_action`) | 13% | 7% |
| Scheduler (`schedule`) | 12% | 8% |
| Scripts (`evalGenericCommand`) | 9% | 19% |
| SHA1 of EVAL bodies (`sha1hex`) | 1.6% | 1.6% |
| IOMMU (`iommu_dma_map/unmap_page`) | 3.3% | 3.7% |
| SRSO return thunks | 1.6% | 7.2% |
| NUMA faults, migration, TLB, scan | ≈1.5% | ≈2.1% (page faults 3.2%) |
| `getClientMemoryUsage` (client eviction is on) | 0.8% | 1.1% |
| perf's own overhead | ≈3% | ≈1% |

#### 2.4 Command time

Rates over 10-31 10:00–18:00Z, 699k calls/s. Commands run inside scripts are
counted twice: once in their own row and once inside EVAL.

| Command | Calls/s | µs/call | Cores |
|---|---|---|---|
| EVAL | 62.2k | 72 | 4.48 |
| EVALSHA | 16.4k | 91 | 1.49 |
| SET | 44.3k | 17.7 (13–21 on every node) | 0.78 |
| EXEC | 8.8k | 43 | 0.38 |
| GET | 81.1k | 4.7 | 0.38 |
| PEXPIRE / PEXPIREAT / EXPIRE | 39.6k / 19.1k / 12.5k | 6.6 / 8.1 / 6.8 | 0.50 |
| HMGET | 14.5k | 17.5 | 0.25 |
| HSET / HINCRBY / ZADD | 23.6k / 29.8k / 22.0k | 7.7 / 5.4 / 6.8 | 0.49 |
| HDEL | 3.3k | 38 | 0.12 |
| ZMSCORE | 2.2k | 45 | 0.10 |
| SMISMEMBER | 133 | 637 | 0.085 |
| PING | 15.8k | 0.7 | 0.01, but a whole round trip each |
| TIME / PTTL / TYPE (inside scripts) | 49.3k / 43.0k / 15.2k | 0.5–0.8 | 0.08 |

- **Duplicate-free command time** is about 8.3 cores (55% of 15). Scripts are
  about 6 of those.
- **Script cache:** 70–79 scripts per node, 0 evicted, no functions. EVAL
  bodies are stable constants, so EVAL never recompiles. It only rehashes and
  re-sends the body.

#### 2.5 Kernel and host

- **edge-6, NUMA** (20 s at ~18:45Z):
  - 165k NUMA hint faults/s, of which 61k are local, and 36.8k page
    migrations/s (≈144 MB/s copied).
  - Redis threads have migrated 22.0 billion pages in 7.8 days of uptime.
  - The median process holds 44% of its pages on its majority node (minimum
    25%). The 32 main threads were spread over both sockets.
- **edge-5:**
  - 63k hint faults/s, 11.3k migrations/s, and a median of 75% of pages on
    the majority node.
  - Swap is full (7 of 7 GB). There were 65 swap-ins/s during the sample, and
    Redis VmSwap totals 128 MB.
  - `compact_stall` is 1.08M since boot.
  - Load is 77–113 on 96 CPUs, with MinIO about 7.8 cores on average.
  - edge-6 has no swap use and 0 compaction stalls.
- **No CPU quota:** the `redis-cluster@` units have no CPU quota (0
  throttled periods). THP is `madvise`, which Redis does not request.
- **Mitigations and IOMMU:**
  - `iommu=pt` is staged in the inventory, but the IOMMU still runs in
    translated mode (`DMA-FQ`).
  - retbleed is "untrained return thunk", and SRSO is "Safe RET".

#### 2.6 Slowlog and latency events

- **Threshold:** `slowlog-log-slower-than` is 10 ms. The last 128 entries per
  node were 6,144 entries, from 14:38 to 18:44Z.
- **The O(N) script:** `providerIntentCountLua` appears 46 times, all on 6416,
  averaging 340 ms and up to 452 ms. All 46 fall within 8.2 minutes
  (18:35:51–18:44:03Z), because 6416's last 128 entries span only 8.3
  minutes. That is ≈1.9 s of blocking per minute, about 3% of the node's main
  thread, in 340 ms pieces. Its inner ZRANGEBYSCORE appears 31 times on its
  own, up to 35 ms.
- **Other scripts,** by entries: `connectControlLimitScript` (342),
  `clientScoreExportSetScript` (303), `redisContractReservationScript` (291),
  `providerLivenessLeaseShardScript` (250, on 19 nodes), `contractHoleReadScript`
  (248), `connectionAuthorityPublishLua` (223), `contractHoleEventScript` (190),
  and the verify-egress lease scripts (531 together).
  - Most of these entries top out at 90–115 ms, whatever the script. That is
    consistent with the host stalls below rather than their own cost; the
    cause was not isolated.
- **edge-5 nodes stall far more:**
  - Their 128 entries span a median 0.3 h, against 2.9 h on edge-6.
  - Simple commands (GET, SET, HMGET…) are 94 of 128 entries per node.
  - That is about 26× more simple-command stalls of 10 ms or more per node per
    hour than on edge-6.
  - LATENCY LATEST on all 16 edge-5 nodes shows `fast-command` up to 733 ms,
    `command` up to 914 ms, `expire-del` up to 914 ms and `expire-cycle` up to
    530 ms.
  - On edge-6, `command` events appear on 3 nodes and `fast-command` on 2.
  - BGSAVE fork events appear on all 48 nodes. The latest median is 104 ms
    on edge-5 and 144 ms on edge-6; the maximum is 481 ms.
- **The 10-31 spike on 6416:**
  - Each connect block restart (02:07, 02:17, 02:26, 02:36Z) was followed by
    a burst on 6416, to main threads of 0.40, 0.46, 0.68 and 0.89.
  - At the peak (02:41:30): ZSCORE ran at 178k/s and HGET at 90k/s, both
    inside EVAL. EVAL averaged 624 µs per call, against 78 µs normally. New
    connections ran at 414/s, and clients rose from 2.6k to 5.8k.
  - At 08:47Z it reached 0.69 with no deploy. Provider connections fell
    90.2k → 85.1k → 89.6k, and intent slot decisions rose to 4/s.

#### 2.7 Clients and churn

- **Connections:** 93.2k in all.
  - By source: edge-0 21%; edge-1, edge-3 and edge-4 19% each; crisp 11%;
    fireside 11%.
  - 2.3k are pub/sub or shard subscribers.
  - There are 1.6–1.8k per node, except 6416 with 4.8k.
  - 29% have been idle for 60 s or more, and 5.9% are younger than 60 s.
  - All use go-redis 9.23.0.
- **New connections:** 88/s over 24 h, 117/s over the last hour, and about 2/s
  per node. 6416 has 7.8/s (24 h). Its deadline pools time out behind the
  long scripts and redial *(inferred)*.
  - Setup costs a handful of commands. Churn is under 0.05 core in all.
- **Pools (client side):** connect 52.8k, api 21.8k, proxy 20.3k, taskworker
  15.1k, mcp 4.8k, alt 1.3k, gossip 1.0k.
- **Round trips (pool gets):** connect about 70–90k/s, api about 19–24k/s,
  proxy 5–8k/s, taskworker 5k/s.

#### 2.8 Memory and persistence

- **Memory:** 60.8 GB used and 69.6 GB RSS. Per node: 1.15–1.56 GB used, a
  peak of 3.3 GB, against a 12 GiB maxmemory. No evictions.
- **Keys:** 366.7M (7.5–9.0M per node), 69% with no TTL.
  - 98% are strings of 63 bytes or less: 213M of 32–63 B, 110M of 2–3 B, 28M
    of 1 B.
  - There are 3.3M hashes and 2.2M zsets.
- **BGSAVE:** about 25 per node per day (`save 2250 100000`), 33–53 s each.
  Children use 0.55 cores; forks block for 60–160 ms, and up to 481 ms.
  - The no-TTL string families are what grow fork and save cost. This is not
    a CPU priority. It belongs to the TTL audit of per-client families.

### 3. Hot commands and scripts mapped to code

| Command or script | Rate | Code (origin/main) | Caller and cadence |
|---|---|---|---|
| EVAL `contractHoleReadScript` (1.3 KB), plus a PING | 13.1k/s | model/subscription_contract_hole.go:56; read at :104-117 through `RedisWithDeadline` with preflight | connect/resident_contract_manager.go:57 → resident_contract_allowance.go:30. Each active pair is re-read at half of the 5 s `ContractManagerCheckTimeout` (connect/resident.go:679). The largest single source, about 26% of connect's go-redis samples |
| EVAL `contractHoleEventScript` (2.0 KB), plus a PING | 4.0k/s (create 1.2k, remove 2.9k) | subscription_contract_hole.go:185, :273-277 | contract lifecycle in connect, taskworker, api and proxy |
| EVAL `redisContractReservationScript` (5.8 KB) | ≈3.8k/s (TYPE ÷ 4) | model/subscription_redis_admission.go:191, :320; flush pipelines at subscription_transfer_debit.go:300 | admission in api, proxy, taskworker and connect, plus the debit flush (512 per pipeline). No preflight |
| EVAL `redisReservationRecoveryPageScript` (1.4 KB) | 1.1k/s | model/subscription_redis_recovery.go:116, :163 | once per published contract |
| EVALSHA `connectControlLimitScript` | 10.6k/s | model/connect_control_limit.go:182, :242 | one per api `/connect/control`; 34% of the api's go-redis samples. Already EVALSHA, no preflight |
| EVAL `providerIntentApplyLua` / `CountClientsLua` (`count_clients`, O(N)) | rare, 100–450 ms each | model/provider_intent_model.go:382-414, :421, :551, :824 | `connectProviderIntentAt` (:687-705) re-decides the slot "on each connection" of a failed-intent client (connect/provider_intent.go:219), plus the taskworker check (:881) |
| GET then a separate EXPIRE (resident record) | ≈5.3k polls/s × 2 round trips | model/network_client_resident_model.go:49-60 | per resident every `ExchangeResidentTtl/4` = 75 s (connect/resident.go:1245, :1269), plus 2.7–4.5k/s forward lookups |
| MULTI ZADD EXPIRE EXEC, then ZREMRANGEBYSCORE | heartbeats; ≈400/s on 6416 | model/peer_model.go:1162-1184 (proxy), :1334-1364 (provider installs) | resident heartbeat (connect/resident.go:1302, :1308) |
| MULTI HINCRBY… EXPIRE EXEC | per provider sync | model/network_client_reliability_model.go:350-366 | connect/transport_announce.go:837 |
| HMGET revocation record | per auth check | session/revocation_record.go:302-307 (`RedisAuth`) | api auth (5.35k attempts/s) and connect authorization |
| SMISMEMBER `{provider_hard_exclusions}` | 96/s × 625 µs, one slot | model/egress_index.go:1005, :1113 | FindProviders2 in api and proxy |
| EVALSHA/EVAL verify-egress lease scripts, HSCAN step | ≈500/s on each proxy-host address node | model/verify_egress_lease_model.go:46, :103; verify_egress_refresh_model.go:182-200 | connect egress feed and refresh |
| EVALSHA `providerLivenessLeaseShardScript` | ≈25–30 scripts/s × ≈300 members *(inferred from 7.6k lease writes/s)* | model/provider_liveness_lease.go:161 | connect writer, 32 shards every 25 s. Already batched |
| SPUBLISH | ≈1k/s | model/contract_origin_notification.go *(inferred)* | api `runBucket` |
| EVAL `clientScoreExportSetScript` and `clientScoreTargetManifestWriteScript` (singleton JSON merge, 10–59 ms) | export bursts | model/network_client_location_model.go:605, :651; network_client_score_target_manifest.go:203 | taskworker client-score export every 30–65 min |

- **Client-side cost:** inclusive go-redis stacks are 7.5% of the connect
  process's samples and 12.6% of the api process's. So each round trip
  removed also saves client CPU.
- **Top connect callers,** as shares of the 1,166 go-redis samples with an
  application caller:
  - `ReadContractHoleLease` 26%;
  - `GetResidentForClient` 13%;
  - verify-egress 12%;
  - session paths 12%;
  - `GetProvideModes` 9%;
  - `RecordClientReliabilityStatsRange` 9%.

### 4. Opportunities

#### 4.1 Contract-hole reads and events without the preflight PING

- **Evidence:**
  - PING runs at 13.6–18.0k/s (24 h and 1 h).
  - Contract-hole operations through `RedisWithDeadline` without
    `OptRedisNoPreflight` run at 17.1k/s: 13.1k reads and 4.0k events.
  - redis.go:625-626 pings an arbitrary node before the callback on the
    deadline pools.
  - Admission, recovery, the grant probe, the liveness lease and the debit
    flush already skip it. The skip's own comment (redis.go:586-589) says the
    PING "reaches an arbitrary node … and costs a round trip and a pool turn
    per call".
- **Saving:**
  - 13–16k requests/s at ≈98 µs each (2/3 land on edge-6 nodes at ≈111 µs,
    1/3 on edge-5 at ≈73 µs). That is ≈1.2–1.6 Redis cores (≈5%) and about 11%
    of requests.
  - On the connect side, the PING is about half of `ReadContractHoleLease`'s
    1.4% of the process (≈0.5% of connect CPU) *(inferred)*.
  - A cold packet-path allowance read loses a full round trip.
- **Change:**
  - Pass `server.OptRedisNoPreflight()` at model/subscription_contract_hole.go:109,
    :276, :371 and :407, and at subscription_contract_hole_readiness.go:110,
    :122 and :133.
  - Optionally add `OptRedisKeepLateReplies()` for the reads, which run under
    `WithoutPostgres` (resident_contract_manager.go:48). Then a slow node does
    not cost a redial.
  - Consider making no-preflight the default of `RedisWithDeadline`. The
    other preflight callers are low-rate.
- **Correctness:**
  - The PING guards nothing. It reaches an arbitrary node, the pool never
    replays the command, and the 1 s `contractHoleRedisTimeout` still bounds
    the whole callback.
  - A connection error now surfaces from the EVAL instead of the PING, with
    the same `read_error` and refusal outcome.
  - The EVAL gets the whole budget instead of sharing it with the PING. No
    timing bound moves.
- **Risk:** low. This is the same change already shipped for admission
  (subscription_redis_admission.go:319-334).
- **Validate:**
  - `redis_commands_total{cmd="ping"}` drops by about 13–16k/s.
  - `redis_total_reads_processed` drops.
  - `urnetwork_contract_hole_operations_total{outcome="read_error"}` and the
    connect allowance outcomes stay flat.

#### 4.2 Provider-intent `count_clients`: O(N) on one slot

- **Evidence:**
  - The script iterates every live member of the network's connected and
    provider zsets. Per member it makes two ZSCORE calls and one HGET, plus a
    `string.match` (provider_intent_model.go:382-414).
  - That is about 8 µs per member: at 02:41:30, 0.69 s/s of EVAL time for 89k
    members/s.
  - The slowlog averages 340 ms per call, implying about 40–50k members
    *(inferred)*.
  - It runs on every connection of a failed-intent client
    (provider_intent_model.go:699-705), so every reconnect wave multiplies it.
    It is the source of the 0.89 and 0.69 peaks, the extra clients on 6416,
    and its redials.
  - Enforcement is still in shadow (`mode="shadow"`). The work buys the record
    status and the shadow metric, but it must stay exact for the day
    enforcement turns on.
- **Change, in steps; each keeps the single atomic script:**
  1. **Batch the lookups:** one ZMSCORE on the present zset, one ZMSCORE on
     the expire zset, and one HMGET over chunks of up to 1,000 members, with
     `unpack` staying under Lua's 8,000-slot C stack. The reads and the count
     are identical. Estimate ≈2.5 µs per member (≈3×), so 340 ms → ≈110 ms.
  2. **Add an exempt index,** a zset `{np_<id>}pi_exempt` scored by record
     expiry and holding only pending or qualified records.
     - The apply script writes it in the same call as its HSET and ZADD, and
       the prune removes from it.
     - The membership test becomes two ZMSCOREs, with no record strings
       entering Lua. Estimate ≈1.5 µs per member (≈5×).
     - It needs a backfill, or a per-network "built" marker with the record
       path as fallback, before readers switch.
  3. **Only if the network keeps growing:** an index of members that are both
     connected and exempt, scored by the minimum of the three expiries. Then
     the count is a ZCOUNT, O(log N).
     - It must be maintained by every writer of the connected, provider,
       present and exempt sets, including the heartbeats at peer_model.go:1334
       and the refresh scripts.
     - Prove it with a property test that compares it with the scan.
- **Correctness:** steps 1 and 2 compute the same normal and exempt counts in
  one script, under the same `{np_}` hash tag. The client-limit decision is
  unchanged. Step 3 adds an invariant that tests must hold.
- **Saving:**
  - Average CPU is small: ≈0.03 core in steady evening operation (one 340 ms
    call every ~11 s), and up to ≈0.7 core on 6416 during a wave.
  - The point is per-node peak and head-of-line blocking. During a wave,
    6416's count work drops from ≈0.7 to ≈0.2 s/s (step 1) or ≈0.13 s/s
    (step 2).
  - Per-call blocking drops from 340–450 ms to ≈70–110 ms. Step 3 removes it.
- **Risk:** low–medium. It is a script rewrite with fixtures for expired
  members, missing records, `skip_member` and duplicate registries.
- **Validate:**
  - `providerIntentCountLua` slowlog entries and duration.
  - ZSCORE and HGET rates on 6416.
  - 6416's main-thread irate and new connections at the next connect
    rollout.

#### 4.3 EVAL → EVALSHA for the hot scripts

- **Evidence:**
  - EVAL runs at 62k/s and EVALSHA at 16k/s. Redis computes SHA1 over every
    EVAL body (`SHA1Transform` is 1.4–1.5% of samples on both hosts) and
    allocates the body string each time.
  - Known body traffic at 13.1k + 4.0k + 3.8k + 1.1k calls/s comes to ≥48
    MB/s: reservation 22 MB/s, contract-hole read 17, contract-hole event 7.8,
    recovery 1.6.
  - With peer, session and provider-intent scripts it is likely 55–75 MB/s of
    the 120–150 MB/s inbound.
  - The debit flush sends 512 copies of the 5.8 KB script per pipeline, which
    is the laptop-flush problem in the escrow notes.
- **Change:**
  - Use `redis.NewScript(...).Run` (EVALSHA with a NOSCRIPT fallback) for
    `contractHoleReadScript`, `contractHoleEventScript`,
    `redisContractReservationScript`, `redisReservationRecoveryPageScript`, the
    peer refresh and add scripts, the session scripts, connection authority and
    provider intent.
  - Pipelines (the debit flush at subscription_transfer_debit.go:300, the
    client-score export at network_client_location_model.go:651) use
    `EvalSha` and re-run only the NOSCRIPT failures with `Eval`. That is the
    pattern at verify_egress_refresh_model.go:182-200.
- **Correctness:** the same script text runs. NOSCRIPT appears only after a
  node restart or flush, and then costs one extra round trip for that script
  on that node. A pipeline must treat NOSCRIPT as "retry with EVAL", never as
  a reservation result.
- **Saving:** ≈0.4 cores of SHA1 plus body allocation and parse, ≈0.5–0.7
  cores (2–2.5%). Also 40–50% of inbound bytes and the matching client
  marshalling.
- **Risk:** low. 70–79 scripts per node, with eviction far below Redis 8's
  EVAL-cache limit.
- **Validate:** EVALSHA up and EVAL down in commandstats;
  `redis_net_input_bytes_total` −40–50%; `SHA1Transform` gone from perf.

#### 4.4 Host: NUMA pinning, plus the staged `iommu=pt`

- **Evidence:** see §2.3 and §2.5.
  - Direct NUMA cost: `do_numa_page`, migration, TLB shootdowns and scanning
    are about 1.5% of samples (edge-6) and 2.1% (edge-5).
  - The larger effect is unmeasured: remote memory and cross-CCX wakeups for
    a request that bounces between two threads 1.4–1.8 times.
  - IOMMU is 3.3–3.7%.
- **Change, all via xops (run-*.sh, playbook drop-in):**
  - Give each `redis-cluster@N` unit `CPUAffinity=` for one NUMA node, with
    the main and I/O threads sharing a CCX, plus `NUMAPolicy=bind` and
    `NUMAMask=`.
  - edge-6 has 8 nodes, so 4 Redis processes per node (8 busy threads of 16).
  - Optionally keep NIC IRQs on the same node.
  - With every big process pinned, `kernel.numa_balancing=0` on edge-6 stops
    the scanning as well.
  - Activate the already-staged `iommu=pt` at the next reboot window.
- **Correctness:** no timing or bound changes; placement only.
- **Saving:** IOMMU ≈3.5% (≈0.9 cores). NUMA is ≥1.5–2% direct plus locality.
  Measure on one node first; ≥5% on edge-6 would justify the rollout.
- **Risk:** medium.
  - There are no replicas. A unit restart makes that node's slots unavailable
    for its RDB reload, and a host reboot makes a third or two thirds of the
    slots unavailable.
  - The one-node test can be done live, with `taskset -a -p` plus
    `migratepages`, before any restart.
  - This is also the prerequisite for CPUOPT2 §3.11 (edge-6 joining the
    services tier): Redis needs fixed cores first.
- **Validate:**
  - On the test node against its peers: µs per read event, NUMA migrations
    (`/proc/<pid>/sched`), and the main-thread share at equal requests.

#### 4.5 One round trip instead of two

- **Resident poll:**
  - `GetResidentForClient` with `ttl > 0` sends GET and then EXPIRE as two
    sequential round trips (network_client_resident_model.go:49-60). That is
    ≈5.3k polls/s → ≈10.6k round trips/s.
  - `GETEX key EX 300` does it in one.
  - The only difference: an undecodable record also has its TTL extended.
    `NominateResident` replaces such a record anyway, through `SetIfEqual`
    at :94.
  - The poll cadence (75 s against the 300 s TTL) is a detection bound and
    stays.
- **Provider and proxy heartbeats:** the prune (ZREMRANGEBYSCORE) runs as a
  separate round trip after the MULTI (peer_model.go:1182, :1354-1362). Queue
  it inside the same TxPipeline instead. Same key, same slot, same commands;
  now atomic.
- **Saving:** −6–7k round trips/s fleet-wide, ≈0.6 Redis cores, plus the
  matching connect CPU. 6416 sheds about 400 round trips/s.
- **Risk:** low.
- **Validate:** the EXPIRE rate drops by ≈5k/s and requests per second drop.
  Resident-loss metrics are unchanged.

#### 4.6 Turn on per-slot statistics

`cluster-slot-stats-enabled` is `no`, so the hot slots in §2.2 had to be
inferred.

- **Change:** set it to `yes`, via xops in redis.conf.j2. CONFIG SET can apply
  it live, but nothing runs CONFIG REWRITE, so it must also go into the
  template.
- **What it gives:** `CLUSTER SLOT-STATS ORDERBY cpu-usec LIMIT 10` then names
  the hot slot in an incident, without MONITOR.
- **Cost:** per-command accounting, expected well under 1%. Measure it on one
  node first.
- **Correctness:** none affected.

#### 4.7 `{provider_hard_exclusions}` on one slot

- **Evidence:**
  - Each FindProviders2 call runs SMISMEMBER over 2N+2 members of one set
    (egress_index.go:1113). That is 96 calls/s at 625 µs, 60 ms/s on 6415 alone
    (57 over the last 23 h, 115 before the proxy page cache).
- **Change:** a process-local copy of the published snapshot, keyed by its
  publication generation (the ready and cohort markers). Read the generation
  once per call, or once per short interval, and reload the copy only when it
  changes.
- **Correctness:** an exclusion is answered from the same atomic publication.
  The mixed-version rules (complete and cohort markers) must apply unchanged
  to the cached copy. Design this before coding.
- **Saving:** most of 6415's 6–11%, plus FindProviders2 latency.
- **Risk:** medium.

#### 4.8 Backstop TTLs re-armed on every write

- **Evidence:**
  - PEXPIRE, PEXPIREAT and EXPIRE run at 71k/s, ≈0.5 cores of command time.
  - With `notify-keyspace-events g$sxK` and 5 patterns per node, each emits
    an `expire` event. The connect subscriber discards those
    (connect/key_event_subscriber.go:303).
  - Examples:
    - the reliability-stats EXPIRE on every write (network_client_reliability_model.go:364);
    - the peer scripts' EXPIRE after each INCR;
    - the reservation script's 4 PEXPIREs per call.
- **Change:** where a TTL is only a backstop, arm it once per key generation:
  `EXPIRE … NX`, or once per block per process.
- **Correctness:** this applies only to backstops. The escrow script's
  "never shorten beneath younger tokens" margin stays as it is.
- **Saving:** ≈0.1–0.2 cores.
- **Risk:** low.

#### 4.9 Decisions and experiments

- **io-threads 1 (experiment only):**
  - The I/O thread costs ≈11–13 cores. At 3–4k requests/s per node, its
    eventfd handoffs add wakeups without parallelism *(inferred)*.
  - An A/B on one node per host would show whether total node CPU falls
    (estimate −15–30%).
  - The main thread would take on the syscalls (estimate +40–60%). That
    roughly halves the incident headroom that saved the hot nodes on
    10-24..10-28, when both threads ran at about 100%.
  - Not before §4.2 and §4.7, and it needs a node restart.
- **SRSO on edge-5/6:** "Safe RET" is 1.6% (edge-6) and 7.2% (edge-5) of
  samples. These are single-tenant hosts, so this is a security-posture call
  for the owner, like CPUOPT2 §3.7. It needs a reboot.
- **edge-5 co-location (latency, not CPU):**
  - The 16 nodes beside MinIO see 26× the stalls of 10 ms or more, with swap
    full and compaction stalls.
  - Options:
    - `MemorySwapMax=0` for the Redis units;
    - keeping MinIO off Redis's NUMA node (cpusets);
    - moving the 16 nodes to edge-6, which has 345 GB free. That puts the
      whole keyspace on one host, with no replicas either way.

### 5. CPUOPT2 predictions, checked against Redis

- **§8.6 #4, the FindProviders2 page cache and singleflight (proxy
  1085611320, 10-30):** materialized in Redis too. Comparing the 23 h before
  with the 23 h after:
  - Redis output bytes −50% (114.5 → 56.9 MB/s);
  - SMISMEMBER time on `{provider_hard_exclusions}` −51%;
  - proxy Redis round trips −55% (11.9k → 5.4k/s);
  - FindProviders2 liveness candidates on proxies −61%;
  - Redis process CPU −11% and main threads −7%. Other changes overlap these
    windows.
- **§3.3 #1, the forward idle tail cut to 3 minutes (connect, 10-29):**
  resident forward lookups fell from 6.0k/s (24 h to 10-29 18:00) to 2.7k/s
  (24 h to 10-31 18:00). The 10-28 removed-resident flood fix lands in the
  same span, so the cut cannot be separated out.
- **§3.11 and §7.2, edge-6 into the services tier with Redis isolated by
  cpusets:** not done.
  - Redis now takes ≈20 of edge-6's 128 logical CPUs on average, and 22–29
    at peak.
  - It floats over all 8 NUMA nodes.
  - §4.4's pinning is both the NUMA fix and the prerequisite.
  - With 4 nodes per NUMA node (8 threads of 16), about 64 logical CPUs would
    be left for services, matching CPUOPT2's 60–80.
- **§2.3, which put Redis under 0.25% of connect CPU (self time):** inclusive,
  go-redis stacks are 7.5% of a connect process and 12.6% of an api process.
  Round trips cost client CPU as well.
- **Kernel options:** `retbleed=stuff iommu=pt` is live on the services edges.
  Only `iommu=pt` is staged for edge-5/6, and it is not rebooted, so IOMMU
  remains 3.3–3.7% of Redis samples there. `retbleed=stuff` does not apply to
  these AMD hosts; their return-thunk cost is SRSO (§4.9).
- **CPUOPT2's edge-6 figures:** 13–28% busy and 366 GB free. Now: 15–17%
  average, peak 23–30 cores, 345 GB available. edge-5 has gone from 16–21% to
  16–19%.

### 6. Considered and not proposed (owner rule)

- **Raising the 5 s `ContractManagerCheckTimeout`** (connect/resident.go:679)
  to cut contract-hole reads. It is the revocation-visibility bound. Cut the
  PING instead (§4.1).
- **Lengthening the resident poll (75 s) or the provider-liveness cadence
  (25 s).** Both are detection bounds. Make each poll cheaper instead (§4.5).
- **A per-process token bucket for `/connect/control`.** It would multiply a
  client's allowed rate by the api process count. The shared EVALSHA stays.
- **Caching revocation reads.** It would delay revocation. The single HMGET
  per check stays.
- **Merging the reservation recovery page with its "published"
  acknowledgement.** That saves only ≈1.1k round trips/s, and it is
  fence-sensitive money code. Not worth the risk now.
- **Skipping `count_clients` while enforcement is in shadow.** That would
  change the evidence for the rollout. Make it exact and cheap (§4.2).

Scratch data and scripts are in
<scratch>/cpuopt3/redis/:
host scripts, aggregated census (`census.json`, no identifiers), perf
summaries and script fingerprints.
