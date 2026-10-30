# CPUOPT2 — edge CPU efficiency (2026-10-29)

**Question (owner, 2026-10-29):** the edges run close to their CPU limit while
using less than half their memory. How do we cut and rebalance CPU so the same
hardware can carry about twice today's usage?

This is research, not a change. Every number comes from read-only measurement
on Main on 2026-10-29: Mimir, short low-rate `perf` samples, kernel and socket
counters. Nothing in production was changed for it. Code references were read
at server main e5e6d4c71 plus the connect and warp repos. *(inferred)* marks a
claim reasoned from code rather than measured. §8 adds the proxy hosts in
depth (2026-10-30). §9 applies the timer lessons to the connect library
(2026-10-30).

**Status (2026-10-30 ~15:20Z; ledger T0228–T0235):**
- **Deployed:**
  - connect `2026.10.29-outerwerld-1085173010` (02:26–03:05Z): keepalive off,
    the 3 min forward idle timeout, and the §3.2 timer changes (lazy
    deadlines, pings paced from the last write, reused timers). Forwards fell
    from 672k to 191k and exchange connections from 1.61M to 0.71M.
  - `retbleed=stuff iommu=pt` on the four services edges, after rolling
    reboots (03:16–03:52Z).
  - proxy `2026.10.30-outerwerld-1085611320` (12:45–13:08Z), shipping §8.6
    #1, #3, #4, #6 and #7. Proxy host CPU fell from about 30% to about 16%,
    dial errors from 6–10k/s to about 15/s per host, and heap from 4.9 to
    2.4 GB per process.
  - proxy limits (14:21Z): GOMEMLIMIT 6500MiB again, a 10GiB `memory_limit`,
    and 4 cores (§8.6 #2, #9).
- **Open:** §9's connect-library changes (none started).

## Summary

**Where the services-edge CPU goes** (edge-0, edge-1, edge-3, edge-4; 312
logical CPUs):

- **By process:** connect is about 54% of busy CPU and 75% of process CPU at
  peak. nginx (the lb) is about 15%, api 11%, taskworker 9%, and logging and
  observability 7%.
- **Kernel time:** about 45% of busy time is system plus softirq, mostly
  syscalls, TCP and loopback.
- **Inside connect, most CPU goes to plumbing rather than the actual work:**
  - Go timer heaps: 15–16%. `runtime.(*timers).adjust` alone is 10–13% self
    time.
  - Scheduler, channel and lock churn: about 23%.
  - Socket syscalls: about 23%, nearly one exchange frame per syscall.
  - GC: only about 4.5%.
- **Frames, not bytes, drive connect CPU.** The 10-26 change from 1 s to 10 s
  exchange pings (606dd8c42) cut connect CPU per client from 0.82–0.97 to
  0.38–0.46 cores per 1k clients.

**The structural cause** is that the exchange gives every (client, resident,
op) pair its own TCP connection, even when both ends are in one process:

- Fleet-wide that is about 885k connections (1.77M ends) for about 192k
  clients.
- Each connect process holds about 1M goroutines and about 450k timers
  *(estimate)*.
- One connection carries about 0.86 data frames/s, so per-connection overhead
  dominates: timers, goroutines, keepalives, deadlines and one syscall per
  frame.

**The memory headroom helps less than hoped for connect,** whose GC is only
4.5%. It helps a lot for api (GC about 20%) and the proxy (GC about 68%). The
big wins come from removing per-connection overhead and from idle capacity:
edge-6 has 128 logical CPUs at 13–28% busy and 366 GB free.

**Ranked plan.** Savings are shares of services-edge busy CPU. They overlap,
so do not add them up.

| # | Change | Effort | Risk | Est. saving |
|---|---|---|---|---|
| 1 | Exchange TCP keepalive: 1 s idle → 60 s, or off (§3.1) | hours | low | 2–5% |
| 2 | api GOGC=400 with a GOMEMLIMIT (§3.8) | hours | low | ~1.5% |
| 3 | Move Mimir/Loki/Grafana off the services edges; trim log volume (§3.10) | days | low | 1–3% |
| 4 | Kernel: `retbleed=stuff` instead of IBRS, `iommu=pt`, by rolling reboot (§3.7) | days | medium | 3–10%, measure on one host |
| 5 | Timer hygiene in connect (§3.2) | 1–2 weeks | medium | 4–7% |
| 6 | Forward idle tail 15 min → 2–3 min; pings stop extending resident activity (§3.3) | days | low–medium | 2–4% |
| 7 | api: JWT verification cache; stop the `/connect/control` retry creep (§3.8) | 1–2 weeks | low–medium | 2–5% |
| 8 | Fewer goroutines and smaller channel rings per exchange end; index the liveness-lease writer (§3.4, §3.5) | 1–2 weeks | medium | 2–4%, plus 10–15 GB heap per process back |
| 9 | In-process delivery when the transport's or forward's resident is local (§3.3) | 2–4 weeks | medium | 5–10% |
| 10 | Multiplexed exchange links per process pair (§3.3) | 1–2 months | high | 8–15% on top of 9 |
| 11 | H1 without nginx: connect terminates TLS (§3.6) | 1–2 months | high | 4–8% |
| 12 | Add edge-6, and edge-7/8 when they arrive, to the services tier (§3.11) | weeks | medium | +20–25% capacity per host |
| P | Proxy hosts (§8): lazy WireGuard peers, bounded CONNECT retries, a process-wide FindProviders2 page cache; memory limit up meanwhile | days–weeks | low–medium | proxy CPU −50–70% at the 10-29 load |

**Path to 2×** (§5). The post-fix peak is about 225 of 312 logical CPUs
(10-27 21:30Z). Carrying twice that at no more than 80% busy needs:

- about a 45% cut in CPU per unit of work on today's hosts;
- about 33% with edge-6 added;
- about 8% with edge-6 plus edge-7/8.

Items 1–8 together plausibly give 15–30%, and items 9–11 another 15–25%. So 2×
on today's hardware needs the structural work. With edge-6 added, the near-term
items plus in-process delivery are likely enough.

## 1. Method

- **Mimir, 7 days (10-22 to 10-29):** node-exporter CPU modes, process CPU by
  service and host, Go runtime gauges, connect exchange and contract counters.
- **perf on edge-3** (2× Xeon Gold 6154, 72 logical CPUs) at evening load:
  about 192k clients, with the host 64–74% busy.
  - One connect g-block process, five times: 20–150 s at 9–199 Hz with
    frame-pointer call graphs, including one pass that attributes timer-arming
    frames to their first application caller.
  - One api process, one taskworker process, and the whole host twice.
  - Each sample was short and low-rate, and its data file was deleted on the
    host.
- **perf on fireside:** one proxy process.
- **Kernel and host:** `nstat` over 10 s, an `ss` census (counts only, no
  addresses), docker host config (network mode and CPU limits only), NIC
  queues and coalescing, IOMMU, transparent huge pages (THP), NUMA and
  mitigations.
- **Two read-only code studies,** one on timers and goroutines and one on the
  exchange and ingress, cited by file:line below.

## 2. Where the CPU goes

### 2.1 Hosts

| Host | Role | Logical CPUs | RAM free/total (GB) | Busy 10-26 02:30Z | Busy 10-29 20:16Z |
|---|---|---|---|---|---|
| edge-0 | services | 96 (2× Xeon Platinum 8168, HT) | 649/1082 | 75% | 51% |
| edge-1 | services | 72 (HT) | 421/812 | 82% | 76% |
| edge-3 | services | 72 (2× Xeon Gold 6154, HT) | 708/1082 | 85% | 74% |
| edge-4 | services | 72 (HT) | 644/1014 | 85% | 72% |
| edge-5 | MinIO, Redis | 96 (2× EPYC 7352) | 79/270 | 16% | 21% |
| edge-6 | Redis | 128 (2× EPYC 7601) | 366/507 | 13% | 28% |
| crisp | proxy, alt | 24 (Core Ultra 9 285HX) | 60/134 | 36%; 99.9% on 10-29 11–15Z | 50% |
| fireside | proxy, alt | 24 (Core Ultra 9 285HX) | 27/101 | 36%; 99.9% on 10-29 11–15Z | 63% |

- **Hyperthreads:** the services-edge "cores" are hyperthreads, so 72 is 36
  physical cores. Logical busy % understates saturation under load; at 80–85%
  logical the physical cores are close to full.
- **Daily services-edge peaks,** in busy logical CPUs: 10-22 208, 10-23 211,
  10-24 192, 10-25 253, 10-26 253, 10-27 225, 10-28 211, 10-29 204. The
  10-25/26 highs predate the ping fix.
- **edge-0 runs cooler than its share:**
  - It has 33% more logical CPUs, but gets the same share of clients as a
    72-CPU host (42–54k each at the 10-26 peak).
  - Its threads are about 10% slower (Platinum 8168 at 2.7 GHz against Gold
    6154 at 3.0 GHz).
  - Connect cost 1.06 cores per 1k clients there against 0.82 on edge-3/4.

### 2.2 By process (services edges)

At the 10-26 02:30Z peak, summed over the four hosts, busy was 253 cores:
connect 190, api 12, taskworker 10, gossip 2, softirq + irq 45. The CPU split
at peak was user 43–46%, system 20–24% and softirq 12–15% of capacity.

edge-3, whole-host perf at 19:57Z, as a share of busy CPU:

| Process | Share |
|---|---|
| connect (5 processes: beta, g1–g4) | 54% |
| nginx (2 lb blocks) | 15% |
| api (5) | 11% |
| taskworker (2) | 9% |
| fluent-bit, loki, dockerd, journald, docker CLI | 7% |
| other | 4% |

### 2.3 Inside connect

These shares are from one g-block process at 6.8–8.3 cores. They agree across
the five runs.

| Category | Share of connect CPU | Notes |
|---|---|---|
| Go timer heaps (`timers.check` → `adjust`, `siftDown`) | 15–16% | `adjust` 10–13% self, reached from `findRunnable` 7.5% and `stealWork` 4.0% |
| Scheduler outside timers (`stealWork`, futex, `notesleep`, `usleep`, netpoll) | ~13% | idle Ps hunting for work |
| `selectgo`, channel ops, runtime locks | ~10% | |
| Socket syscalls | ~23% | write ~14%, read ~11%; mostly exchange, about one frame per write |
| Exchange code paths (inclusive; overlaps syscalls) | 25–30% | writer, reader, buffers, accept workers |
| Client transports (H1/websocket) | ~20% | |
| Connect library sequences and `Client.run` | ~7% | |
| `providerLivenessLeaseWriter` | ~2.8% | O(connections) loops under a lock |
| GC (mark + assist) | ~4.5% | allocation 1.7% |
| conntrack lookup | ~1.5% | `__nf_conntrack_find_get` |
| AppArmor socket checks | ~0.4% | `aa_inet_msg_perm` |
| TLS, protobuf, Redis, pgx | <0.25% each | TLS terminates in nginx |

### 2.4 Work and its cost

| | 10-26 02:30Z (1 s pings) | 10-29 ~20:00Z (10 s pings) |
|---|---|---|
| Connected clients | 203k | 192k |
| Exchange connection ends | 1.45M | 1.77M (forward 1.38M, transport 0.39M) |
| Connect goroutines | 16.1M | 20.3M (1.0M per process) |
| Exchange frames/s, both directions | 3.57M (77% pings) | 1.80M (16% pings) |
| Exchange bytes/s | 349 MB | 989 MB |
| Connect cores | 190 | 112 |

- **Idle connections:** an exchange connection carries about 0.86 data frames/s,
  so most are idle at any moment.
- **The ping change shows the cost model.** Halving the frames halved connect
  CPU while bytes tripled.
- **Per-client cost is rising again.** It was 0.38–0.46 cores per 1k clients on
  10-27 and 0.60 at 10-29 18:00Z. Two drivers:
  - the `/connect/control` creep: control HTTP went from 4.9k to 9.0k/s, and
    contract refusals from 1.6k to 5.3k/s in a day;
  - more exchange data per client, as proxy-hosted devices doubled.

### 2.5 api, proxy and taskworker

- **api** (one process at about 2.6 cores):
  - GC about 20% (background mark 12.7%, malloc 6.9%).
  - P-256 JWT verification 11%: every request re-verifies its token.
  - JSON 6.6%.
  - The `/connect/control` handler about 37% inclusive (`CreateContract` 11%,
    Redis 12%), with session authentication about 21%, partly overlapping.
- **proxy** (fireside, one process at about 3.7 cores):
  - GC about 68% (background mark 50%, assists 19%), because the heap is
    pinned against GOMEMLIMIT (6.8 GB).
  - Ten processes at GOMAXPROCS 8 give 80 Ps on 24 cores. CPU pressure
    (PSI cpu-some) is 21% on crisp and 27% on fireside, and both are paging.
- **taskworker:** database-bound (the debit flush transactions). Not a target.

### 2.6 Kernel and host

- **Exchange keepalive:** edge-3 sends 170k TCP keepalive probes/s and receives
  166k pure ACKs/s. That is about 20% of all TCP segments each way (865k/s out,
  741k/s in).
- **Mitigations** on the Skylake-SP services edges:
  - retbleed IBRS;
  - PTI (PCID is present);
  - MDS/TAA/MMIO buffer clearing on every kernel exit.

  The syscall entry and exit stubs alone are about 3.4% of connect samples.
- **IOMMU** is in translated mode on all edges: `intel_iommu_map_pages` and
  `pfn_to_dma_pte` show in kernel samples.
- **conntrack** tracks every flow: 590k entries on edge-3, no NOTRACK. warp's
  DNAT needs it.
- **Memory layout:** THP is in `madvise` mode with 0 huge pages in use. There
  are two NUMA nodes, and automatic NUMA balancing is on.
- **Containers:** host networking, with no CPU limits or cpusets.
- **NICs (edge-3):** three i40e 10 GbE with 72 queues and adaptive coalescing.
  Softirq is uneven across CPUs: median 5%, p90 20%.
- **crisp and fireside:**
  - The second NIC is a Realtek r8127 with one RX queue and RPS off.
  - UDP receive-buffer overflows are 4.3M and 3.7M.
  - `net.core.rmem_max` is 1 MiB (also on the edges).

## 3. Findings and proposals

### 3.1 Exchange TCP keepalive fires every second

- **Evidence:**
  - `connect/resident.go:2749-2754` sets `Idle: 1s, Interval: 1s, Count: 15`
    on every outbound exchange socket.
  - This is on top of the 10 s application ping (`ExchangePingTimeout`,
    resident.go:610) and the 30 s read deadline.
  - edge-3 sees 170k probes/s out and 166k pure ACKs/s in. Fleet-wide that is
    about 0.68M probes/s and as many ACKs, plus a kernel keepalive timer
    firing per idle socket per second.
- **Change:** set Idle 60 s, Interval 10 s and Count 3, or turn keepalive off.
  Pings and the read deadline already catch dead peers.
- **Saving:** 2–5%, from softirq, kernel timers and loopback/NIC packets.
- **Risk:** low.
- **Validate:**
  - `nstat -az TcpExtTCPKeepAlive` should go to about 0.
  - softirq share;
  - packets/s on lo and the NICs.

### 3.2 Go timer heaps are the largest single cost in connect

**Evidence:**
- `runtime.(*timers).adjust` is 10–13% self time in every connect profile.
- In go1.27.3, `adjust` walks and rebuilds a P's whole timer heap in two
  cases: when the earliest modified timer comes due, or when zombies exceed a
  quarter of the heap (runtime/time.go). `check` runs it from every
  `findRunnable`, and `stealWork` runs it on other Ps' heaps.
- About 0.9 cores of `adjust` per process means each P rescans roughly every
  50 ms.

**Timer population** (code study, about 450k heaped timers per process,
roughly 28k per P):

- **Exchange sockets, about 60%:**
  - two netpoll deadline timers re-armed on every frame (resident.go:2514,
    2559, 2607);
  - a ping timer per writer (2974/2990, 1653/1668).
  - Netpoll deadlines are never deduplicated, so every `Set*Deadline` is a
    modify even when the deadline did not move. Each modify that lowers the
    heap's next modified time also calls `wakeNetPoller`.
- **Other sources:**
  - The announce measurement publisher re-creates `time.After(5s)` on every
    loop of every transport (transport_announce.go:1215).
  - Authorization lease checks use short `context.WithDeadline`s
    (session/authorization_lease.go).
  - Key-event subscriber resyncs.
  - `RedisAuth` contexts.
  - go-redis per-command deadlines.
  - The connect library's sequence timers (`resetOrCreateTimer`).
  - Abandoned 12–24 h announce timers (transport_announce.go:695-696) that stay
    as zombies.

**Sampled timer-arming sites** on edge-3 (40 s at 199 Hz): samples whose stack
holds an arming frame, by the first application caller. quic-go did not appear.

| First caller | Samples |
|---|---|
| `ConnectionAnnounce.runMeasurementPublisher` | 197 |
| `AuthorizationLease.runChecks` | 177 |
| exchange buffer and connection deadlines and ping resets | 233 |
| `keyEventSubscriber` resync and notifications | 183 |
| `RedisAuth` | 72 |
| go-redis pool | 84 |
| `ReadContractHoleLease` | 26 |
| connect library sequences | 49 |
| H1 deadlines | 30 |

**What is not settled:** the code study could not pin the trigger for scans
every ~50 ms. Our own re-arms are at least 1 s ahead and explain only about 5%
of the scan rate. Selects that wake on data leave their timers as zombies, and
resets turn those back into modifies, which pushes scans.

- **To pin it down:** count `runtime.(*timers).initHeap` per P with a short
  uprobe.
- **It doesn't change the fix:** the scan cost scales with the timer count,
  and those timers are ours.

**Changes:**
1. **One liveness sweeper per process** instead of per-connection deadlines and
   ping timers.
   - Keep atomics for last read, last write and write start.
   - A 1 s ticker closes sockets idle for 30 s or stuck writing for 15 s, and
     queues pings for writers idle for 10 s.
   - Removes about 265k timers and about 135k modifies/s per process.
   - Files: resident.go:1600-1800, 2463-2616, 2920-3005; transport.go:395,
     1393, 2082, 2139-2204.
2. **Until then, refresh deadlines lazily:** re-arm only when less than two
   thirds of the timeout remains.
3. **Reuse timers** in the measurement publisher, the lease checks and the
   key-event loops, and stop abandoning long timers
   (transport_announce_liveness_lease.go:280 and the listener loops).

**Saving:** timers fall from about 15% to 3–5% of connect, and there are fewer
poller wakeups. That is 4–7% of services-edge busy CPU.

**Risk:** medium. Detection becomes up to 1 s coarser, and the ping-deadline
tests need rework.

### 3.3 One TCP connection per exchange pair

**Evidence (code):**
- `ResidentTransport` and `ResidentForward` always dial a new
  `ExchangeConnection` (resident.go:3563, 3842), even when the resident is in
  the same process. `isLocalResident` (resident_move.go:335) is used only for
  move takeover.
- A relayed packet can cross three exchange hops: transport-in, forward,
  transport-out.
- The writer drains only its own queue, up to 256 frames or 256 KiB
  (resident.go:586-587). A socket carrying 0.86 frames/s therefore writes one
  frame per syscall. TCP_NODELAY is on and nothing corks.
- Per frame: about 1 write, about 2 reads (one EAGAIN, *inferred*), 2 deadline
  re-arms, about 3 goroutine wakeups, and the ACKs.

**Placement:**
- The first carrier nominates its own process as the resident by Redis
  compare-and-set (model/network_client_resident_model.go).
- Later carriers on other processes forward to it.
- The host comes from DNS or EdgePlacement, and nginx `least_conn` picks the
  block.

**Forwards:**
- There is one per (source resident, destination client). It lives 15 min
  after its last payload (`ForwardIdleTimeout`, resident.go:600).
- Forward pings refresh the destination resident's activity, so residents
  outlive their clients.
- Handshakes run at about 2.4k/s, which is about 1.2k new connections/s with a
  mean life of about 12 min, mostly idle tail *(inferred)*.
- A client averages about 1 transport and 3.6 forwards. Quality windows hold
  6 providers even when idle, and identity churn adds idle tails.

**Changes, in order:**
1. **Shorter forward tail.** Set `ForwardIdleTimeout` to 2–3 min and stop
   counting pings as resident activity (resident.go:1806-1816).
   - Saving 2–4%, plus about 0.2M goroutines and gigabytes of heap per process.
   - Risk: a redial (Redis read plus handshake) after an idle gap.
2. **In-process delivery.** When the target resident is in the same process,
   attach channels directly instead of dialing: wire the `ResidentTransport`
   channels in as the resident's routes, and push local forwards into
   `AddForward`'s queue (resident.go:4990-5115).
   - Each local hop sheds 2 sockets, 1 conntrack entry and about 7
     goroutines, and per frame 2 syscalls and 3 timer operations.
   - Saving 5–10%, depending on the local fraction. Measure that first, by
     transport locality from `ss -tnp` process IDs.
   - Medium complexity: pooled-buffer ownership, drain counts and generation
     checks.
3. **The resident follows its transport.** Generalize MOVE takeover with a
   cooldown so most transports end up local (resident_move.go:665-693).
   - A further 3–6%.
   - MOVE is merged but off (move_settings.go).
   - Risks: rebuilding the resident, and flapping between H1 on the edge and
     H3 on alt.
4. **Multiplexed links per process pair.** The design:
   - frames tagged with a stream id;
   - one writer coalescing many residents' frames into `writev` with a
     100–200 µs linger;
   - one reader dispatching to resident queues;
   - per-stream credit, so a slow destination cannot block the others;
   - one ping per link.

   This removes about 99% of exchange sockets, conntrack entries, pings,
   keepalives, deadline timers and per-end goroutines.
   - Saving 8–15% on top of step 2.
   - High complexity: version negotiation through `ExchangeHeader.Version`,
     mixed-version rollout, head-of-line blocking, and the blast radius when a
     link fails.
5. **Then fewer, larger connect processes per host** (5 → 2). More traffic
   becomes local, and there are fewer idle Ps (today 5 × GOMAXPROCS 16 = 80
   Ps on 72 logical CPUs).
   - Saving 1–3%, only after step 2 or step 4.
   - Risks: GC on 100 GB+ heaps, longer drains, and twice the memory during a
     full-overlap rollout.

A cheaper stopgap before step 2 is AF_UNIX sockets for same-host links. They
work under host networking. Saving 2–4% before step 2, about 1% after.

### 3.4 Goroutines, hand-offs and channel rings

**Evidence (code):**
- Goroutines per exchange end: 6 forward-out, 5 forward-in, 6 transport-out,
  4 transport-in, about 477k per process.
- Each client transport adds about 8–13, and each resident about 25–45.
  Forward-ingress shards alone can be up to 16 per resident (resident.go:408).
- A forwarded frame passes through about 11 goroutines. About a third of the
  goroutines only wait on a context.
- Every exchange end pre-sizes `make(chan []byte, ExchangeBufferSize)` rings
  (resident.go:2801-2802, 3424-3425, 3720, 4629, 4999-5000). At 4096 slots
  that is 96 KiB per channel, about 15 GB per process of mostly empty pointer
  slots that GC must scan *(estimate)*.

**Changes:**
- Use `context.AfterFunc` for the Done→cancel watchers.
- Run the writer inside `ExchangeConnection.Run`, and the reader on the
  accept worker.
- Merge the forward consumer into its reader.
- Fold the idle watchers into the sweeper (§3.2).
- Cap forward-ingress shards at 2–4.
- Size rings small and grow them on demand.

**Saving:** 250–300k fewer goroutines (about 2 GB of stacks) and 2 fewer
hand-offs per frame. That is 1–3% of CPU, plus 10–15 GB of heap per process
back.

**Risk:** medium, from teardown ordering.

### 3.5 The provider liveness-lease writer

- **Evidence:** `write()` loops over every connection twice per pass, and
  `written()` once per shard (32 shards), under the lock
  (transport_announce_liveness_lease.go:290-444). It is about 2.8% of connect
  CPU.
- **Change:** keep per-shard sets and a dirty list.
- **Saving:** about 1%. Low risk.

### 3.6 nginx in the H1 path

**Evidence:**
- nginx is about 15% of busy CPU on edge-3.
- H1 and h1plus terminate TLS in nginx's L7 `http` block
  (warp/warpctl/config.go:2047). nginx relays the upgrade, with buffering off,
  to connect's internal port 80 over a second local socket.
- H3 and whodis pass through nginx `stream` with PROXY v2
  (config.go:2496-2517).
- libcrypto is about 13% of nginx's samples; the rest is kernel TCP.
- An H1 client whose resident is local costs about 5 host sockets, 3 conntrack
  entries and 2 relay hops *(inferred)*.

**Change:**
- Let connect terminate H1 TLS itself; it already loads TLS for QUIC. This
  needs either a TCP variant of warp's external interface DNAT or a
  SO_REUSEPORT/sk_lookup group.
- H3 could skip nginx `stream` the way alt does.

**Saving:** 4–8% net, after the TLS work moves into connect.

**Risks:**
- Port 443 is shared with api, so connect needs dedicated addresses.
- nginx's rate limits are lost.
- Ports rotate on every deploy.

Effort is high.

### 3.7 Kernel and host settings

1. **`retbleed=stuff` instead of IBRS** on the Skylake-SP services edges. The
   kernel has `CONFIG_CALL_DEPTH_TRACKING=y`.
   - IBRS on Skylake is among the costliest mitigations for syscall-heavy
     work. Call-depth stuffing keeps the protection at lower cost.
   - Estimate 3–8% *(unmeasured)*. Needs a reboot per host.
   - `mitigations=off` would save more, but that is a security decision and
     not recommended here.
2. **`iommu=pt`.** Per-packet DMA mapping shows in kernel samples.
   Passthrough for host drivers would save 1–3% *(unmeasured)*. Needs a
   reboot.
3. **NOTRACK for the exchange:** not possible today, because it depends on
   warp's DNAT. It becomes moot after §3.3 step 4.
4. **AppArmor:** docker's default profile adds `aa_inet_msg_perm` to every
   socket message (0.4% of connect). Running connect unconfined would remove
   it. Small.
5. **THP and NUMA,** both memory-for-CPU experiments for one host:
   - Connect's 60 GB heaps run on 4 KiB pages across 2 NUMA nodes with
     automatic balancing on.
   - Try THP `always` with `defrag=defer`, watching RSS.
   - Try pinning connect blocks to a NUMA node with `cpuset-cpus` plus
     `cpuset-mems`. warp would need per-block cpusets.
   - A few % each for large Go heaps is typical; unmeasured here.
6. **Proxy hosts:**
   - Enable RPS on the single-queue Realtek NIC.
   - Raise `net.core.rmem_max` and the proxy's UDP socket buffers.

### 3.8 api

- **GC** is about 20% of api CPU, at GOGC=100 on a 4.7 GB heap.
  - Set GOGC=400 with GOMEMLIMIT around 16 GiB per process.
  - That saves 12–15% of api CPU for about +15 GB per process (+75 GB per
    host).
- **JWT verification** is about 11%. Every request re-verifies the same client
  token with P-256 ECDSA.
  - Cache verified tokens: a bounded LRU keyed by a hash of the token, with
    entries expiring with the token.
  - Session state checks still run.
  - Saves about 10% of api CPU.
- **The `/connect/control` creep:**
  - API traffic doubled in a day (10.9k against 5.6k req/s).
  - Contract refusals tripled (5.3k/s against 1.6k/s), mostly
    `inactive_destination` from old clients retrying stale exits.
  - It resets on a connect restart.
  - The handler and its authentication are about half of api CPU, and the
    creep also costs connect and the database.
  - A server-side retry hint, or a cached refusal per (client, destination),
    would remove most of it.
- **Together:** 2–5% of services-edge busy CPU.

### 3.9 Proxy hosts (crisp and fireside)

§8 supersedes this subsection with a fuller measurement and code study.

**Evidence:**
- GC is about 68% of proxy CPU, because the heap is pinned against GOMEMLIMIT.
  GOMEMLIMIT is 90% of the warp block memoryLimit (warp/warpctl/run.go:2189).
- 80 Ps run on 24 cores, and CPU pressure is 21–27%.
- Live devices doubled on 10-29 (16k → 29k).
- Idle devices are held for 90 min (`ProxyDeviceIdleTimeout`, c2040f152).

**Changes:**
- Raise the memoryLimit where RAM allows: crisp has 60 GB free, fireside only
  27 GB.
- Set GOMAXPROCS to 3–4 per proxy process.
- Shorten the idle hold, for example from 90 to 30 min, to cut live heap.
- Spec the four planned proxy servers at 128 GB RAM or more, with server NICs
  that have multi-queue RSS.

**Saving:** proxy CPU per device −30–50%.

### 3.10 Logging and observability on the services edges

Mimir, Loki and Grafana run on services edges (edge-0, edge-3), along with
fluent-bit, dockerd's log handling, journald and rsyslog. Together they are
about 7% of edge-3's busy CPU.

- Move Mimir, Loki and Grafana to edge-6, which is idle and has 366 GB free.
- Cut container log volume: drop docker's json-file logging for journald or
  `local`, and log fewer connect lines.
- **Saving:** 1–3%.

### 3.11 Placement and spare capacity

- **Weight hosts by capacity.** edge-0's placement and lb weights should be
  about 1.2× a 72-CPU host: 33% more threads, each about 10% slower. This
  lowers the hottest host by 3–4 points at peak. Small.
- **edge-6 can host services.**
  - It has 128 logical CPUs at 13–28% busy and 366 GB free.
  - Pin Redis to its own cores and IRQs with cpusets, and give connect and
    api the rest.
  - That adds 60–80 usable logical CPUs, +20–25% services capacity.
  - Risk: Redis latency. Validate against Redis latency metrics.
- **edge-5 is memory-bound** (79 of 270 GB free). It could take at most one
  small block; not a priority.
- **edge-7 and edge-8** have been in provisioning by the owner since 10-22 and
  are not in the inventory yet. Each 72-CPU host adds about 23% services
  capacity.

## 4. Trading memory for CPU

| Lever | Memory cost | CPU effect |
|---|---|---|
| api GOGC 100 → 400, GOMEMLIMIT ~16 GiB | about +15 GB per process | api −12–15% |
| Proxy memory limit up, idle hold down | +3–5 GB per process on crisp | proxy −30–50% per device |
| JWT verification cache | under 1 GB | api about −10% |
| Connect GOGC 100 → 200, GOMEMLIMIT ~110 GiB | about +40 GB per process | connect about −2% (GC is only 4.5%) |
| THP for connect heaps | some RSS | a few % (experiment) |
| NUMA pinning | none | a few % (experiment) |
| Fewer, larger connect processes | neutral | +1–3%, after in-process delivery |
| Smaller channel rings | −10–15 GB per process | less GC mark work; pays for the rows above |

For connect, memory is not what the CPU is waiting on. Its cost is
per-connection plumbing. Spend the headroom on api and proxy GC, on caches and
on process consolidation. Get connect's heap back by shrinking the rings. Keep
headroom for full-overlap rollouts, which double a block's memory while it
swaps.

## 5. Path to 2×

**Baseline:** about 225 busy logical CPUs at the post-fix peak (10-27 21:30Z).
The 10-26 peak of 253 had 1 s pings.

The requirement is 2 × 225 × (1 − s) ≤ 0.8 × capacity, where s is the cut in
CPU per unit of work:

| Capacity (logical CPUs) | s needed |
|---|---|
| Today, 312 | ≥ 45% |
| + edge-6, about 376 usable | ≥ 33% |
| + edge-7/8, 456 | ≥ 19% |
| + edge-6 + edge-7/8, 520 | ≥ 8% |

- **Near-term items** 1–8 in the summary plan are estimated at 15–30%
  together.
- **Structural items** 9–11 add another 15–25%.
- **Order:**
  1. Ship 1–4 within a week.
  2. Then 5–8.
  3. Measure.
  4. Start in-process delivery (9) once transport locality is measured.
  5. Decide on 10 and 11 from the results.
  6. Bring edge-6 in alongside.
- **Plan on hyperthreads:** on the HT hosts, plan for no more than 75% logical
  busy at peak. Track µs per frame and cores per 1k clients, not busy %.

## 6. Validating each change

- **Connect A/B per block.** g1–g4 carry near-equal load (block weights
  24/25/25/25). Run a change on one g-block and compare it with the others on
  the same hosts:
  - connect cores per 1k clients;
  - µs per exchange frame (process CPU ÷ exchange frames);
  - goroutines and heap.
- **perf per process:** the shares of `timers.adjust`, `stealWork`,
  `selectgo`, `syscall.Syscall` and GC.
- **Host:**
  - `nstat` counters TcpExtTCPKeepAlive, TcpOutSegs and TcpInSegs;
  - softirq share and per-CPU spread;
  - packets/s on lo;
  - CPU pressure (PSI).
- **Kernel changes:** one host at a time, in the same time-of-day window.
  Compare µs per frame and system % against the peers.
- **api:** GC share and CPU per request.
- **proxy:** GC cycles/s and CPU per live device.

## 7. Decisions for the owner

1. The kernel command line on the services edges (`retbleed=stuff`,
   `iommu=pt`): rolling reboots and a security-posture call.
2. Moving Mimir, Loki and Grafana to edge-6, and whether edge-6 joins the
   services tier with Redis isolated by cpusets.
3. A connect release with the keepalive, forward-tail and timer changes. A
   connect rollout causes a reconnect wave.
4. The structural program: in-process delivery, then multiplexed links (a
   protocol change), and generalizing MOVE takeover.
5. Bypassing nginx for H1: dedicated addresses, with rate limiting done
   elsewhere.
6. Proxy settings (memoryLimit, GOMAXPROCS, idle hold) and the spec for the
   new proxy hosts.
7. Proxy code fixes from §8.6, in order: lazy WireGuard peers, bounded
   CONNECT retries, the FindProviders2 page cache. Whether to raise the
   proxy memoryLimit before they land. The restart reconnects each host's
   hosted devices; on 10-29 the owner chose to wait the saturation out.

## 8. Proxy hosts in depth (added 2026-10-30)

Measured during the second saturation of 10-29: from 21:10Z both hosts sat at
99.9% CPU, with load 80 on 24 cores. Sources:
- perf on a crisp proxy process;
- Mimir and Loki;
- a code study of server/proxy, ../proxy, connect, sdk, userwireguard and
  gvisor.

File references are relative to the urnetwork root.

### 8.1 What the CPU is doing

- **The hosts are CPU-bound in Go, not the kernel:** user 92%, system 3%,
  softirq 5%.
- **One process at 2.35 cores:**
  - GC is about 57%: background mark 27% and assists 30%. `mallocgc` is 34%
    inclusive.
  - The connect library is 58% inclusive. Inside it, in-process
    FindProviders2 is 18%: Redis 15%, and gob decoding of client-score pages
    12%.
  - HTTPS CONNECT handling 6%, H1 transport 5%, blackhole detection 4%,
    gVisor 4%.
- **Useful work is small:** 881 active proxy sessions moved 0.66 MB/s
  fleet-wide while 46 cores ran.
- **Day over day:**
  - devices +40% (20.3k → 28.4k);
  - proxy CPU 3.4× (13 → 46 cores);
  - allocation per process 3× (0.05 → 0.14 GB/s);
  - GC 9× more often (every 27 s → every 3 s).

### 8.2 Why: the processes live above their memory limit

Per process: heap in use 5.56 GB + stacks 1.26 GB + runtime 0.17 GB ≈ 6.99 GB.
GOMEMLIMIT is 6.82 GB, 90% of the warp memoryLimit (warp/warpctl/run.go:2189).
The next-GC target (4.46 GB) sits below the live heap, so GC never stops, and
only Go's 50% GC CPU limiter caps it. Cutting allocation barely helps until
memory drops below the limit.

| Owner, per process | Count | Heap | Stacks | Goroutines |
|---|---|---|---|---|
| WireGuard peers: every configured one, started on registration | ~41.3k | ~1.1 GB | ~0.45 GB | ~82.6k |
| Hosted devices | ~1.43k | ~2.4 MB each | ~0.4 MB each | ~73–98 each |
| Stuck HTTP CONNECTs | ~1.9k | ~60 MB | — | ~4k |

**WireGuard peers follow signups, not use:**
- The proxy registers every WG-configured client of its block
  (server/proxy/server.go:886-941, 1021-1057).
- userwireguard starts each peer at registration (device/uapi.go:412-413).
- Each started peer holds 2 goroutines, queues of 1024/1024/128 slots, two
  128-slot batch slices and 5 timers: about 26 KiB of heap and 11 KiB of
  stack.
- That is 824k peers across 20 processes. MaxPeers is 65,536 per device
  (device/constants.go:39).

**Idle devices keep everything:**
- 12.7k of the 28.4k live devices were idle.
- Each keeps its full state for 90 min (`ProxyDeviceIdleTimeout`,
  server/proxy/proxy_device.go:868; 90 min since 6df7a6fab).
- DoH keepalive pings count as activity, so the real hold is about 105 min.

### 8.3 A CONNECT retry storm that has run for days

- **The numbers:** 38.3k HTTP CONNECTs are in flight against 881 active
  sessions, and `connect_dial_error` runs at 29k/s fleet-wide.
- **The shape:** both grow linearly, by about 2k stuck CONNECTs and about 1k
  errors/s per hour. They reset only when the proxies restart, as at 10-29
  05:30Z, and have climbed again since.
- **The cause:** ../proxy/http.go:380-398 retries a failed CONNECT dial about
  every second until the client hangs up.
  - It retries every error class: unauthorized, unknown device, NXDOMAIN,
    policy reset.
  - It never answers the client.
  - Device creation has no per-id cooldown (server/proxy/proxy_device.go:375-464);
    only WireGuard has one.
- **The saturation's "panic" pages:** these are Redis deadline errors that
  `HandleError` logs with a stack. In 400 sampled lines, 272 came from
  `getProviderHardExclusions`, 90 from `RecordProviderSearchMatches` and 36
  from loading client scores. All are inside the in-process FindProviders2.

### 8.4 Allocation sources

By first application caller of `mallocgc`, sampled for 15 s at 99 Hz:
- **encoding/gob score decoding** (`decodeMap`, `decString`, `decAlloc`): the
  largest source.
- **Multi-client stats snapshots:** `windowStatsWithCoalesce`,
  `packetStatsCounters.snapshot`, `unorderedClients`.
- **Per-tick timers.** Every hosted device runs 1 s loops that each create a
  new timer per tick: contract stats, the sdk contract-stats flush,
  `IpAssoc`, event epoch, the scheduler-pause detector and the send-stall
  watch. Blackhole detection adds one every 1.25 s. That is about 14k new
  timers/s per process with zero traffic.
- **Per-frame and per-packet work:**
  - a 4-byte heap header per H1 frame;
  - `flushDeliver` clones;
  - IP path parses per packet;
  - a WireGuard endpoint per datagram.
- **Per CONNECT attempt:** `strings.SplitN` of the SNI, amplified by the retry
  loop.

### 8.5 FindProviders2 inside the proxy

**Cadence:**
- Each hosted window refreshes its exits on a 45–60 min rotation, idle
  devices included.
- Errors retry at 1→15 s even when the window is full.
- Under timeouts that reaches about 190 calls/s per process *(inferred)*.

**Cost per call:**
- Each call loads max(10 × count, 1000) score rows, with count ≥ 20, though
  windows ask for 1–4 candidates
  (server/model/network_client_location_model.go:6252-6260, 6350-6355).
- Each 200-row page is copied, SHA-256 checked and decoded by a new
  `gob.Decoder` (server/model/network_client_score_cursor.go).

**The cache opportunity:** pages are content-addressed, and every hosted
session asks for location 0.0.0.0:0. A process-wide cache of decoded pages
would almost always hit and could never be stale. None exists; the "5 s score
cache" is test-only.

### 8.6 Ranked proxy changes

| # | Change | Expected | Risk | Effort |
|---|---|---|---|---|
| 1 | Lazy WireGuard peers: start queues and goroutines on the first handshake or server-initiated send, stop on key expiry; 256-slot queues | −1.0 GB heap, −0.45 GB stacks, −80k goroutines per process. On its own this takes the process out of the memory-limit regime (GC ≈1.3 → ≈0.3 cores per process) | start/stop races; a handoff initiation must start the peer | M: userwireguard/device |
| 2 | Raise the proxy memoryLimit now, and revert after #1: crisp +4 GB per process, fireside +1.5 GB (its ceiling) | about −1 core per process, −10 to −12 cores per host | fireside RAM; the restart reconnects hosted devices | XS: warp config |
| 3 | Bound CONNECT: answer at once on definitive failures (407/403/502); wait with `WaitForReady` instead of polling; back off 1→8 s within about 30 s, then 504; add a per-id creation cooldown | ends the 29k/s storm: about 5–15% CPU, 4k goroutines and 60 MB per process, plus the error-log flood | clients waiting on cold windows get a 504 | S–M: ../proxy/http.go, server/proxy/server.go, connect/tun.go |
| 4 | FindProviders2: a process-wide LRU of decoded score pages keyed by slot, field, generation and checksum; hex `encodeUuid`; a hosted error-retry floor plus singleflight | −10–14 points of proxy CPU; most gob allocation gone | about 64 MB per process | M: server/model/network_client_score_* |
| 5 | Cap hosted windows, for example quality 3 and speed 0 | prevents a 2.7× rise in per-device heartbeats, cpings and FP2 as windows refill from 2.25 to 6 clients | throughput per device | S: sdk policy |
| 6 | Idle hold 90 → 30–45 min (identity TTL ≥ hold), don't count DoH keepalives as activity, or hibernate idle DeviceLocals | −0.5–1 GB per process, about −25% background work | re-mint churn on reopen | S/M |
| 7 | Per-device loops: reuse timers, skip ticks with no pending work, `context.AfterFunc` for parked waiters; turn off the pause detector, heartbeat and outcome watch for hosted devices | −14k timers/s, about −34 goroutines per device, 3–6% CPU | low | M: connect |
| 8 | Small fixes: `strings.Cut` for the SNI; borrowed IP parse in WG; reuse WG endpoints and frame-header scratch; skip gonet write deadlines; cache the uTLS spec; sample WG sessions without an `IpcGet` of 41k peers every 15 s; cache TLS keys by cert path | a few % | low | XS–S each |
| 9 | GOMAXPROCS 8 → 3–4 per process (80 Ps on 24 cores today) | fewer continuous GC workers (about 20 → 8 cores per host), 5 fewer gVisor goroutines per device | burst latency | XS |
| 10 | Host: SO_RCVBUF/SO_RCVBUFFORCE 4–8 MiB on the WireGuard socket (none is set today), and RPS on the single-queue Realtek NIC | fewer of the millions of UDP drops | low | XS: userwireguard/conn, xops |
| — | The message pool cap (8 GiB, server/cli/proxy/main.go:26, 39-45) sits above GOMEMLIMIT and its free list is never trimmed | only 10 MB is retained today; cap it at about 512 MiB as a guard | low | XS |

**Order:**
- #2 is the immediate relief.
- #1, #3 and #4 are the structural fixes. They make per-device cost scale with
  use rather than signups and stop the storm.
- With them, the four planned proxy hosts add real headroom instead of
  absorbing GC.

Estimates are inferred from the measurements above. Measure each one per
block: GC share, CPU per live device, and the `connect_dial_error` rate.

## 9. The connect library (added 2026-10-30)

**Question (owner, 2026-10-30):** do the timer lessons from §3.2 also apply to
the connect repo?

They do, and mostly in the same shapes:
- deadlines re-armed on every frame;
- a ping timer reset on every write;
- loops that wake on a schedule with nothing to do.

The library runs in three places, so each fix lands differently:
- **Proxy hosted devices:** about 1k per proxy process, 20 processes. Each
  device has a platform transport, windows and channels. This is the only
  population measured here.
- **Providers:** desktop, CLI and mobile devices that carry other users'
  flows. Timer work there scales with flows and packets.
- **Apps:** phones, where wakeups cost battery more than they cost CPU.

### 9.1 Method and measurements

- **perf on crisp** (10-30 14:45Z): one proxy process on release
  `1085611320`, 30 s at 99 Hz, 4,148 samples.
  - Runtime timer functions are about 5% of samples: `(*timers).adjust` 2.6%
    self, `siftDown` 0.8%, `stealWork` 0.7%.
  - For comparison, `adjust` was 10–13% in the connect profiles of §3.2.
  - Timer-arming frames, by first application caller:
    - `RedisAuth` deadline contexts;
    - `runEventEpoch` and `detectBlackhole` (`maybeAdd`/`modify`);
    - the ack worker `ReceiveSequence.Run.func3`;
    - `ResilientTlsConn.SetWriteDeadline`;
    - `ContractManager.runContractStats` and `expireQueuedContracts`;
    - `ApiOutOfBandControl.SendControlWithCtx`;
    - `ClientStrategy.parallelEvalWithRouteHedge`.
- **Code study:** five read-only audits of connect main `caad1de6` (Go
  1.27.3), cited by file:line below; paths are in the connect repo:
  - the platform transport;
  - transfer sequences and contracts;
  - multi-client loops after 3ad45203;
  - the provider side;
  - the other transports.
- **Not measured:** providers and phones. Their rates below are *(inferred)*
  from code and default settings.

**A Go 1.27 detail that shapes every fix:**
- A channel timer sits in a P's heap only while a goroutine blocks on it.
  Leaving a `select` turns it into a zombie, so each pass of a loop pays a
  `maybeAdd`, and each `Reset` or `Set*Deadline` pays a `modify`. Modifies
  are what make `adjust` rebuild the heap (§3.2).
- A lazy deadline removes the per-packet `modify`. A timer channel left in a
  per-packet `select` still pays block, unblock and zombie accounting.
- A `time.AfterFunc` that feeds a 1-buffered wake channel, or a shared
  sweeper, removes that cost too.

### 9.2 Findings

**1. Deadlines re-armed on every frame (lesson: lazy re-arm).**
- **Platform transport, H1:**
  - The read deadline is re-armed on every inbound message, server pings
    included (transport.go:3214 → net_websocket_batch.go:227 →
    net_resilient.go:635).
  - The write deadline is sent again by gorilla on every frame, even
    mid-batch (net_websocket_batch.go:235 → net_resilient.go:639, the
    profiled `ResilientTlsConn.SetWriteDeadline`).
  - The writer stores a new value per write at transport.go:2926, 2987,
    2994, 3111 and 3167.
  - Nothing is deduplicated.
- **Platform transport, H1+ and H3:** a write deadline per batch
  (transport_h1plus.go:69); H3 stream deadlines per batch or message
  (transport.go:4076, 4186, 4398).
- **Provider data path:**
  - TCP deadlines per read and per write batch (ip.go:5923, 4965);
  - UDP and ICMP deadlines per datagram (ip.go:3519, 3709, 3807;
    ip_icmp.go:796, 834).
- **Extender server relay:** a read and a write deadline per 4 KiB chunk, in
  both directions (extender/extender.go:1893, 1900, 1933, 1939;
  extender_camouflage.go:492, 518). This is a server-side hot path on
  operator extender hosts.
- **p2p legacy SCTP lane:** per message, a write deadline
  (transport_p2p.go:1472; transport_p2p_webrtc.go:4688), a zero read deadline
  (4498), and a `NewTimer` plus `Stop` (4467).

**2. A ping timer reset on every write (lesson: pace from the last write).**
- `resetWakeupTimer(pingTimer, …)` runs on every writer wake that writes:
  transport.go:2953, 3097, 3118, 3133, 3141, 3159, 3165 and 3174; H3 4193 and
  4209.
- **Cost:** a `time.Now` and a `Reset` of a heaped channel timer, once per
  batch. That is once per message when traffic is sparse.

**3. Loops that wake with nothing to do (lesson: reuse, then park).**
- 3ad45203 (§8.6 #7) removed the allocations but kept the periodic wakes. At
  about 1k devices per proxy process:
  - `detectBlackhole`: per channel, about 7 per device, at 0.8 Hz. About
    5.6k wakes/s, each reading under the parent lock
    (ip_remote_multi_client.go:18869, 19207).
  - `watchSendStalls`: per window, 1 Hz, about 2k/s (12082–12084).
  - `runEventEpoch`: per device, 1 Hz, about 1k/s (2902, 2926).
  - `runProber`: per device, 0.2 Hz (ip_remote_multi_client_prober.go:640–642).
- All four can park on an edge, as `runContractStats` and `IpAssoc` do now:
  - a blackhole verdict needs a send in the 30 s window;
  - a stall needs `sendNackCount > 0`;
  - an epoch needs a counter change.
- **Transfer sequences, per packet in steady state:**
  - **Ack worker** (`ReceiveSequence.Run.func3`, transfer.go:16066): arms
    and fires per 10 ms `AckCompressTimeout` on each active sequence, plus
    1 ms quiet-head re-arms. That is 100–1000 timer operations/s per hot
    sequence.
  - **Client read loop:** a timer per inbound frame
    (transfer.go:4543 → transfer_route_manager.go:3405, 3465, 3546), and a
    wake every 30 s when idle.
  - **Idle and resend timers:** `SendSequence.Run` (transfer.go:9943),
    `ReceiveSequence.Run` (16320) and `ForwardSequence.Run` (18357) Reset
    them on every pass.
  - **`expireQueuedContracts`:** a new `time.After` every 60 s per contract
    manager, forever, and a scan under the manager mutex
    (transfer_contract_manager.go:825).
- **Provider loops:**
  - **UDP poll shards** wait 100 ms forever, 10 Hz per shard (2–4 shards
    × 2 families), even with no flows (ip_udp_socket_poller_epoll.go:64,
    _kqueue.go:65). On a phone that is 20–80 wakes/s while providing
    nothing *(inferred)*.
  - **Stats epoch:** `runPacketStats` creates a new `time.After` each second
    (ip.go:8814).
  - **TCP retransmit:** the timer is re-armed with a goroutine wake on every
    advancing ACK (ip_tcp_return_recovery.go:135, 197→277).
  - **Idle timers:** Reset and Stop on every send-loop wake (ip.go:6689–6696,
    3841; ip_icmp.go:825).
  - **Ack compress:** fires 50 ms after every ack (ip.go:6178–6211).
  - **Last activity:** `UpdateLastActivityTime` takes a mutex and calls
    `time.Now` per packet (ip.go:11253).

**4. Keepalive (lesson: off where app pings exist).**
- **Done:** off for platform-transport dials, including when an extender or
  VLESS carries the platform transport (connect 01030ccd; net.go:288–292).
- **Still on:**
  - **Provider upstream TCP flows:** they inherit
    `DefaultConnectSettings` (5 s idle, 5 s interval, count 1; net.go:94–99),
    because `ProviderKeepAliveConfig` is nil (ip.go:5530). Every idle flow
    probes every 5 s, which keeps a phone's radio up.
  - **Extender feed subscription:** dialed on an unmarked context
    (net_extender_network.go:1176).
  - **Extender server accepted connections:** Go's 15 s default
    (extender/extender.go:377–380).

**5. Ping cadence and alignment (new; not in §3).**
- **Idle cadence:** an idle client pings every 10 s, not 5.
  `WakeupTime` rounds now+5 s up to the next 5 s boundary.
- **Alignment:** the boundaries are wall-clock, so pings from every client
  arrive together.
  - **H1:** 140k idle clients are about 14k pings/s at the exchange,
    arriving in bursts of about 70k at each boundary *(inferred)*.
  - **H3:** quic-go adds its own PING 5 s after the last received packet
    (`KeepAlivePeriod` = PingTimeout, transport.go:1238), so the app ping and
    the QUIC ping alternate. That is about 2 ack-eliciting packets per 10 s
    where one would do.
- **Per-channel pings:** wall-clock aligned the same way.
- **Trade-off:** alignment saves radio wakeups on phones. On hosts (the
  proxy, the exchange) it only makes bursts.
- **Other keepalives:** the p2p ICE keepalive is 2 s per peer connection
  (transport_p2p_webrtc.go:875). The extender QUIC carrier inherits a 5 s
  keepalive on top of the inner transport's ping.

### 9.3 Ranked connect changes

| # | Change | Who | Expected | Risk | Effort |
|---|---|---|---|---|---|
| 1 | Platform transport: lazy deadlines, re-armed only when less than T remains (read to now+40 s, write to now+15 s); the batch conn skips a write deadline `Equal` to the last one forwarded; ping paced from a recorded last-write time | proxy, providers, apps; every carrier at once | per-frame 2–3 timer operations become at most one per 5–10 s per connection; probably most of the proxy's remaining ~5% timer CPU *(inferred)* | a websocket write timeout is terminal, so each write must start with at least T left; the read cutoff moves to 30–40 s after the last frame, safe while server pings stay at most ~10 s apart | S–M: transport.go, net_websocket_batch.go, net_resilient.go |
| 2 | Park the per-device loops: `detectBlackhole` on the send edge, `watchSendStalls` on the nack edge, `runEventEpoch` on a change flag; the prober sleeps until its next due time | proxy, apps | about −8.6k wakes/s per proxy process; an idle phone goes from ~10 to ~1 wakes/s *(inferred)* | a fallback timer (~10 s) keeps transport-loss retirement at most 70 s (60 s today) | S–M: ip_remote_multi_client*.go |
| 3 | Transfer: the ack worker waits for its compress deadline and skips unchanged Resets (or a shared ack-flush wheel); lazy idle and resend deadlines in the Send/Receive/Forward `Run` loops; no per-frame timeout in the client read loop; park `expireQueuedContracts` while empty | providers, proxy, apps | per-packet timer work gone from steady state | resend, ack and idle deadlines must never fire late; an early fire costs one recompute | M: transfer.go, transfer_route_manager.go, transfer_contract_manager.go |
| 4 | Provider: poll shards wait without a timeout (wake via `context.AfterFunc`); park the stats epoch; upstream keepalive off, or idle ≥ the flow idle timeout; signal the retransmit timer only when its deadline moves earlier; lazy I/O deadlines; arm idle timers once; ack at once when ≥50 ms since the last ack; atomic last-activity; one hashed wheel per buffer for idle timeouts (slot ≤ T/8, about 33 checks/s at 10k flows) | providers, especially mobile | battery: removes the 20–80/s idle poller wakes and the 5 s keepalive per flow; CPU: per-packet timer work | a close must never be early (re-check `IdleCondition`) and at most one slot late | M–L: ip.go, ip_udp_socket_poller_*.go, ip_tcp_return_recovery.go |
| 5 | Ping cadence: QUIC `KeepAlivePeriod` = 2× PingTimeout; a ping-jitter setting for hosts (the proxy), keeping wall-clock alignment on phones; measure the exchange's ping bursts | exchange, proxy, H3 apps | halves idle H3 packets; smooths ~70k-ping bursts at the exchange *(inferred)* | dead-peer detection on H3 goes to at most ~30 s (~25 s today) | S |
| 6 | Extender and p2p: lazy re-arm per direction in the extender server relay; `ListenConfig{KeepAlive: -1}` for the extender server; mark the feed subscription dial; on the legacy SCTP lane create the timer only when the conn isn't ready and skip unchanged deadlines; `KeepAlivePeriod` 0 on the extender QUIC carrier when a platform transport owns it; reuse the flight-wait timer (net_extender_quic_send.go:268) | operator extenders, censored-region users, p2p | small per user; the relay is per byte on extender hosts | `TestP2pLegacyPhysicalWriteTimeoutStillBoundsBlockedCarrier` asserts exactly T and needs updating | S each |
| 7 | Small: race completion via `time.AfterFunc` + `sync.Once` (ip_remote_multi_client.go:9791); reuse timers in resize, send retry and discovery; `context.AfterFunc` for net_http's merge goroutines (net_http.go:1508, 1821); one timer per call in `TakeContract` and in Pack's establishment waits | all | minor | low | XS each |

**Order:**
- #1 first. It is one change in one place, every carrier inherits it, and it
  targets the largest sampled proxy site.
- #2 and #3 next, for the proxy and for phones.
- #4 matters most for providers on phones and needs its own tests.

**Validate:**
- Re-profile a proxy process before and after each change: the
  `timers.adjust` share and the callers of `pollSetDeadline`, `modify` and
  `maybeAdd`.
- For #2 and #4, count goroutine wakes per second per process.
- Re-run the connect suite. The perf floors (`TestWindowPathSdk*`) are
  load-sensitive, so run them alone.
- Measure provider and phone battery on devices before shipping #4 and #5.
