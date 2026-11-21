# LBALTOPT2: lb_alt after the first round of fixes, measured again

Status: measured 2026-11-20, 18:30–19:40Z, after LBALTOPT1's first round reached every connect block (LBALTOPT1 §8).
- Read-only throughout:
  - Mimir;
  - two 30 s CPU profiles, a heap snapshot and two allocation snapshots, from connect's loopback pprof listeners (LBALTOPT1 Appendix C);
  - edge counters, read as an unprivileged user;
  - one production h3 probe from an expiring lag.dev guest client.
- Nothing in production was changed. R13, R14's sequence timer, R15 and the dashboards are implemented on branches, neither merged nor deployed (§8).
- Recommendations continue LBALTOPT1's numbering at R13, and findings continue at P9 and C6.
- Times are UTC.

## 1. Summary

**Host CPU per Gbps is 8–14% below nginx's, but connect itself costs 10–29% more per Gbps than on 11-19.**
- **What's still saved:** nginx's 11–13 cores per host are still gone. Non-Go CPU is now 1.7–2.3 cores, against 13.1–15.1 on 11-19, mostly from system and softirq time.
- **What connect gave back:** 4.5–10 cores per host of that saving. Round 1 measured a 12–19% saving at midday. In this evening window it is 8–14% (§3.1).
- LBALTOPT1's within-host test found the front itself costs connect little. So the increase is connect's own work, and the profiles show where it goes.

**Where connect's CPU goes** (g4 on edge-4, 9.5 cores; §3.3):
- **Timer heap walks:** the Go runtime walks whole per-P timer heaps of about 1.1–1.3M timers per process. That's 10–12% of the process, up from round 1's 8.8%.
- **Searching for work:** the scheduler's idle search, including those walks, is 20–24%. The runtime's own count of time spent running Go code is 6–14% above connect's actual CPU, a sign that the 100 Ps per 72-CPU host are waiting for CPUs.
- **Socket syscalls, 19%:**
  - exchange links between connect processes, 7.8%;
  - clients' TLS sockets, 6.6%;
  - Redis, 2.9%.
- **TLS handshakes:** 4.3–6.5% of the process and 17% of its allocation. Half of the handshake CPU is the hybrid post-quantum key exchange. Refused connect upgrades are about 31% of all completed handshakes.
- **Redis client:** 5.4%.
- **Connection announce:** 4.5%.
- **The front's request handling:** 2.2%.

**A quadratic allocation.** 13% of connect's allocation is one process-wide copy-on-write callback list. Every client the server creates registers a network-quality listener on it, and each add or remove clones the whole list (§3.5).

**Correctness and client experience** (§3.7–§3.12):
- **Refusals:** the per-address limits, still nginx's values and keyed by address alone, refuse:
  - 12–20% of front requests, all day;
  - 50–66% of connect's upgrade requests;
  - 62% of completed QUIC handshakes on 443. Every QUIC refusal the new family counter has seen is IPv4.
- **Block imbalance:** g2 and g4 carry 2.7 times the clients and handshakes of g1 and g3, although their weights are equal. api requests are balanced, so the cause is a few source addresses carrying many connect clients, under a split that hashes the whole address.
- **h3 from the app:**
  - End to end through the fronts, two clients pinned to h3 the way the app dials, with direct connections off, delivered every message both ways and real egress traffic (DNS and TCP). That held on a desktop and under the phone's memory policy.
  - A phone's 5 MiB transport budget holds one 3 MiB h3 transport. If the phone's provider also runs on h3, it holds none, and the consumer's h3 transport waits for budget forever. That explains "h3 doesn't route data" (C8).
  - App clients hold about 130 h3 sessions, against 9.8k providers'.
  - About 0.6 h3 logins a second fail token verification, from current app builds. That predates lb_alt.
- **Deploys:** tagging every block at once and letting each switch at its drain's end paced the deploy better than either earlier way. Host CPU peaked at 78.5–83.5%, against 84–86% for round 1's spaced waves and 90–91% for the all-at-once deploy.
- **LBALT.md's `connection_new` note is wrong:** the counter reads 0 only during the two-hour transition window after every connect start (§3.9).

**Recommendations** (§5), ranked:
- R13: stop the server's clients registering network-quality listeners.
- R14: find and remove the timer modifications that set off the heap walks, and shrink the heaps.
- R15: limits that suit shared addresses: a 429 with `Retry-After`, a limit per client identity after auth, and separate QUIC caps for api and connect.
- R16: measure, then fix, the block imbalance.
- R17: a GOMAXPROCS trial.
- R18 and R19: batch the announce writes and trim the Redis client.
- R20: h3 from the app, end to end.

## 2. Method and windows

| Window | What it measures |
|---|---|
| 18:30–18:50Z, 11-20 against 11-19 | Steady state with round 1's code. Every connect block ran round 1's build by 18:24Z. Its predecessors were still draining but used about 0.4 cores each. 11-19 is nginx at the same minutes, with 2–7% more clients and 2–5% less NIC traffic per host. |
| 18:45–19:40Z, 11-20 | A deploy: the family-counter build (`2026.11.20-outerwerld+1104002320`) switching each block as its predecessor's drain ended. The new processes started at 18:54 (beta), 18:58 (g1), 19:05 (g2), 19:15 (g3) and 19:22Z (g4). |
| Profiles, 19:34–19:36Z | 30 s CPU profiles of g4 on edge-4 (9.5 cores) and g2 on edge-3 (7.9 cores). A heap snapshot and two allocation snapshots, 60 s apart, of edge-4's g4. Round 1's 18:25Z profile of g2 on edge-3 is the comparison. |

**The processes were young** (12–29 minutes old), so GC and other costs that depend on heap size read lower than in a day-old process. Busy CPU, traffic and the node series are as in LBALTOPT1 §2. Appendix A lists the queries and profile commands.

## 3. Measurements

### 3.1 Host CPU against traffic

18:30–18:50Z:

| Host | Busy cores, 11-20 / 11-19 | NIC Gbps | Cores per Gbps | Change | connect cores | connect cores per Gbps | Change | Non-Go cores |
|---|---|---|---|---|---|---|---|---|
| edge-0 | 46.6 / 51.8 | 4.36 / 4.18 | 10.68 / 12.40 | −13.9% | 35.7 / 31.2 | 8.18 / 7.47 | +9.5% | 2.3 / 13.4 |
| edge-1 | 49.1 / 52.5 | 3.95 / 3.80 | 12.44 / 13.81 | −9.9% | 39.0 / 31.2 | 9.87 / 8.20 | +20.3% | 1.7 / 13.4 |
| edge-3 | 42.6 / 47.6 | 4.15 / 4.16 | 10.27 / 11.43 | −10.1% | 34.5 / 27.0 | 8.30 / 6.49 | +27.9% | 1.9 / 13.1 |
| edge-4 | 51.3 / 53.3 | 4.99 / 4.77 | 10.29 / 11.17 | −7.9% | 39.6 / 29.3 | 7.94 / 6.14 | +29.3% | 2.0 / 15.1 |

- **By mode:**
  - system time fell 3.4–3.6 cores per host, and softirq 1.2–1.8;
  - user time was flat, except on edge-4 (+3.1).
- **Loopback:** 0.28–0.48 Gbps, against 1.07–2.18.
- **Go services other than connect:** 6.3–9.7 cores per host, against 7.2–8.9, so flat.
- **Confounds:** connect's build changed four times between the windows, and `cores: 20` replaced 16 (D15).

### 3.2 connect's runtime

18:30–18:50Z, per host:
- **GC:**
  - 2.9–3.2% of connect's busy CPU (round 1: 3.3–3.9%);
  - dedicated marking 0.82–0.98 cores, assists 0.22–0.32, idle-priority marking 0.07–0.21;
  - 0.08–0.09 cycles a second, twice round 1's rate, because two processes per block were collecting and the new heaps were still growing.
- **Scheduler latency:** p50 4.2–4.9 µs, p99 0.80–0.90 ms, except edge-1 at 2.9 ms.
- **Running time against CPU:** `go_cpu_classes_user_cpu_seconds_total` exceeds connect's `process_cpu_seconds_total`:
  - +9.4% on edge-0, +6.0% on edge-1, +8.3% on edge-3, +14.0% on edge-4;
  - the classes are the runtime's own estimates from the time its Ps spend running, so time a running P's thread waits for a CPU counts as running;
  - on edge-1, 3 and 4, five blocks at GOMAXPROCS 20 put 100 Ps on 72 CPUs (LBALTOPT1 P4).
- **Memory and goroutines:**
  - allocation 738–935 MB/s and 5.4–6.4M allocations a second (round 1: 673–744 MB/s at midday);
  - goroutines 3.1–4.0M, heap in use 193–224 GB, resident 229–279 GiB.

### 3.3 Where connect's CPU goes

**By kind of work**, grouped by each goroutine's entry function. Functions started through `server.HandleError` are attributed to the function passed to it, and stacks that are runtime-only are named by what the runtime was doing.

| Work | g4 on edge-4, 9.5 cores |
|---|---|
| Client transports' read and write workers (`connectHandlerWorkers.start`) | 17.8% |
| Scheduler idle work: timer checks 10.0, other idle search 4.7, netpoll 2.7, stealing 2.0, park and switch 1.6 | 21.0% |
| Residents: forward ingress, forwards, forward workers, callbacks, `Run`, local nomination | 13.0% |
| Exchange links between connect processes (`ExchangeConnection.Run`, `handleExchangeConnection`) | 12.8% |
| The front's HTTP: `net/http.(*conn).serve` (handshakes included) and h2 | 11.8% |
| The residents' client library: `NewClientWithTag`, send and forward sequences | 7.7% |
| Connection announce and measurement publishing | 4.3% |
| QUIC (`quic-go`) | 3.1% |
| Authorization lease checks | 1.0% |

**By function**, cumulative, across the three profiles:

| | Round 1, g2 on edge-3 (8.7 cores) | g2 on edge-3 (7.9) | g4 on edge-4 (9.5) |
|---|---|---|---|
| `runtime.(*timers).check` (of which `adjust`) | 8.8% (7.2%) | 12.3% (10.2%) | 10.3% (8.4%) |
| `runtime.findRunnable` (of which `stealWork`) | 19.0% (7.2%) | 24.0% (8.6%) | 19.7% (6.1%) |
| `runtime.selectgo` | 8.9% | 9.7% | 10.3% |
| `syscall.Syscall` (socket reads and writes) | 16.4% | 19.2% | 19.1% |
| `crypto/tls.(*Conn).serverHandshake` | 4.0% | 4.3% | 6.5% |
| `crypto/tls.(*Conn).Write` / `Read` | 4.5% / 3.7% | 5.1% / 3.8% | 5.2% / 4.3% |
| go-redis | 4.8% | 5.7% | 5.4% |
| pgx | 1.0% | 1.4% | 1.2% |
| `quic-go.(*Conn).run` | 1.0% | 1.3% | 1.7% |
| The front's `routeHttpHandler.ServeHTTP` | – | – | 2.2% |

**Socket syscalls by component**, 20.4% of edge-4's g4:
- **Exchange links (plain TCP), 7.8%:**
  - writes through `writeFramerBatch`, 3.1%;
  - reads as `Framer.ReadHeader` refills the link's `bufio.Reader`, 2.7%;
  - `writev` in `ExchangeBuffer.WriteMessages`, 1.2%;
  - the rest, 0.8%.
- **Clients' TLS sockets, 6.6%:** the front's `frontTrackedConn.Write` 4.2%, TLS reads 2.5%.
- **Redis, 2.9%:** writes 1.5%, reads 0.9%, and go-redis's per-checkout health check (`connCheck`, a peek syscall) 0.5% plus 0.3% in its own code.
- **Others:** Postgres 0.8%, QUIC 1.0%, accept and close 0.3%.

**TLS handshakes**, 5.9% of edge-4's g4 in `serverHandshakeStateTLS13.handshake`:
- the hybrid key exchange (X25519MLKEM768), 3.2%: X25519 1.7%, ML-KEM encapsulation and key parsing 1.4%;
- ECDSA P-256 signing, 0.5%;
- the key schedule's HKDF and HMAC hashing, and writing the certificate, most of the rest.

**Redis and announce:**
- **The Redis client, 5.4%:** half of it socket syscalls. It's reached mostly through `redisTtlWarnHook`, which every command passes.
- **Connection announce, 4.5%:** its per-connection Redis calls, 2.3% (the liveness lease, about 9.7k writes a second fleet-wide), and the measurement publisher's Postgres writes, 1.35%.

### 3.4 The timer heap walks

**How Go 1.27 keeps timers** (`runtime/time.go`, go1.27.4):
- **Modifying a timer:** changing a timer already in a P's heap, by `Timer.Reset` or a socket deadline (`runtime_pollSetDeadline`), only marks it, and lowers the heap's `minWhenModified` to the new deadline when it's earlier. That's whether the deadline moved earlier or later (`modify`, line 584).
- **The walk:** once `minWhenModified` passes, the next `timers.check` on that heap calls `adjust(now, false)` (line 1012). That walks every timer in the heap, loading each one's state atomically, and rebuilds the heap.
- **Deleted timers:** a stopped timer, or a channel timer whose last blocked select woke through another case (`unblockTimerChan`), stays in the heap until a walk removes it. A forced walk (line 1031) runs only when deleted timers pass a quarter of the heap.

**Measured:**
- **Where the cost is:** all of the walk cost in all three profiles is at line 1012, and the forced walk at line 1031 never appears. The ordinary walks remove deleted timers often enough that forced walks don't happen.
- **The walk itself:** `adjust` is 8.4% of edge-4's g4. 5.6% of that is the per-timer atomic load (`internal/runtime/atomic.(*Uint8).Load` under `adjust`), and 0.6% rebuilds heaps.
- **Who triggers it:** walks happen when idle Ps look for work, through `findRunnable` (60%) and `stealWork` (40%), which checks other Ps' heaps.

**What's in the heaps.** The heap snapshot holds about 1.1M live timers made by `time.NewTimer`, plus `time.AfterFunc` and context timers. That's about 45 timers a client, before counting socket deadlines. By the code that made them:

| Owner | Live timers |
|---|---|
| `ConnectionAnnounce.run` | 147k |
| quic-go `(*Conn).resetTimer`: a stop and a new timer at each deadline change, so the replaced timers wait in the heaps until a walk removes them | 95k |
| `ConnectionAnnounce.runMeasurementPublisher` | 86k |
| `handleExchangeConnection` | 81k |
| `AuthorizationLease.runTimer` | 81k |
| `Resident.runIdleWatcher` | 78k |
| `ResidentForward.runIdleWatcher` | 73k |
| `ResidentForward.runWithResidentLookup` | 66k |
| connect's `resetOrCreateTimer` | 47k |
| The h1 writer's ping timer | 45k |
| `newExchangePinger` | 23k |
| Created through `time.newTimer`: the front's h2 `SetWriteDeadline` per request, `context.WithDeadline`, and connect's `sequenceDeadlineTimer.armBy`, 42k each; pgx's per-query `enterPotentialWriteReadDeadlock`, 19k | 145k |

**Who modifies them.** CPU in `runtime.(*timer).modify` is a proxy for how often each caller modifies:
- **Socket deadlines, 49%:**
  - `exchangeDeadline.refresh` sets the exchange link's read deadline on every message. Its equality check compares nanosecond times, so it never skips one;
  - the front's `frontTrackedConn.SetWriteDeadline`, for every write to a client;
  - go-redis's per-command deadlines;
  - the h1 reader's per-read deadline.
- **`Timer.Reset`, 26%:**
  - the h1 writer's ping timer, reset each time the session goes idle, at least `MinPingTimeout` ahead;
  - the measurement publisher;
  - connect's sequence deadline timers;
  - the exchange.
- **New timers, 25%:** these enter the heap directly and don't move `minWhenModified`.

**What the profile can't say.** At about 50–100 ns a timer, 0.72 cores of walking over heaps of about 55k timers a P means each P walks several times a second. A walk is due only when a modified deadline arrives, so some modification keeps a deadline only about 100–250 ms ahead. The candidates are the timers above with short horizons: sequence deadlines, the h2 per-request deadlines, and any Reset under a second. The CPU profile can't attribute horizons; R14 measures them.

### 3.5 Allocation

edge-4's g4 allocated 26.9 GB in the 60 s between snapshots, 448 MB/s:
- **The network-quality listener list, 3.5 GB (13%):** `connect.AddNetworkQualityChangeListener` (`connect/util.go:184`) registers each client in one process-wide `CallbackList[func()]`. Its `Add` and `Remove` clone the whole list and a parallel id list (`connect/util.go:309`). In the server, every resident's client registers, so each client created or closed copies two slices as long as the process's resident count: `Add` 2.44 GB, `Remove` 1.08 GB. Only `NetworkQualityChanged` reads the list, which is a phone's signal, so the server never reads it. Its CPU is small (`memmove` 0.15%), but its garbage is 13% of the GC's work and grows with the square of residents per process.
- **TLS handshakes, 4.7 GB (17.4%):** the cryptobyte builders, SHA-256 and HMAC state, ML-KEM keys and handshake records. This is round 1's open question: the extra allocation per Gbps with the front is mostly handshakes. The front's request handling adds another 2.0 GB (7.6%).
- **Per-transport and per-forward setup, about 6.7 GB (25%):** `newResidentTransport` 1.33 GB, `newSendSequenceWithLogicalLane` 1.31, `addTransportWithEdge` 1.23, `NewExchangeConnection` 0.85, `addForwardWithReceive` 0.74, `NewResidentForward` 0.73 and `newWindowPacingService` 0.51. Sessions churn, with a median life of 4.4 s, so each new session pays this.
- **The h1 writer's batches, 1.6 GB (6%):** `writeConnectH1UserReadyBatch` allocates a batch per write.
- **Readers:** `bufio.NewReaderSize`, 0.67 GB.

### 3.6 Requests, forwarding and latency

18:30–18:50Z, fleet-wide:
- **Requests,** about 11.1k a second:
  - api: h1 1,272, h2 7,227, h3 38;
  - connect: 2,452;
  - gossip: 44.
- **Statuses:**
  - connect: 910 `1xx` (accepted upgrades), 1,380 `4xx`, 162 `2xx`;
  - api: 7,832 `2xx`, 658 `4xx`, 47 with no response;
  - `5xx`: 0.11 a second for api and 0.12 for connect.
- **api's hottest routes:** `/connect/control` 5,151 a second, `/hello` 1,935, `/network/auth-client` 184, `/network/remove-client` 130, and the provider egress attempt and health routes 73 each.
- **Forwarding overhead** (R10): api's own handling against the front's header time:
  - mean 14.6 against 16.1 ms;
  - p50 0.80 against 0.89 ms;
  - p99 180 against 227 ms.
- **The forwarder:**
  - api's upstream connections are reused 99.4% of the time: 7,913 reused, 47.9 dialed and 1.45 failed dials a second;
  - no forward errors;
  - retries 0.10 a second after a 503;
  - 2,715 tunnels.
- **Body refusals:** `incomplete` 9.3, `canceled` 0.84, `timeout` 0.89 a second. R9 didn't move them (round 1: 9.0 and 0.98), so the `incomplete` refusals are bodies that really end early, not resets that were misfiled.

### 3.7 Limits and refusals

- **The limits:** `default_rate_limit` in services.yml applies to every lb interface: 450 requests a minute, a burst of 150, and 50 QUIC connections per address per interface, outside `exclude_subnets`. They are nginx's values.
  - api's `/connect/control` has a zone of its own: 12,000 a minute with a burst of 6,000 per address. That's a backstop, since api limits that route per verified client id.
  - grafana's and Loki's query and push paths have zones too.
  - Everything else, connect's upgrades included, shares the interface's default zone.
  - A refusal is a 429 with no `Retry-After` (`nginx/route_http.go:respondError`).
- **18:30–18:50Z:** 1,756 refusals a second from the request limit, 157 from the interface's concurrency limit, 28.6 from `/connect/control`'s concurrency zone, and 36.6 from the QUIC cap.
- **Over the day:**
  - the request limit refused 12.4–19.7% of front requests between 08:00 and 18:30Z, the most at 18:30Z;
  - it refused 50–66% of connect's upgrade requests;
  - by host, it refused 450–1,030 a second on edge-3 and edge-4, against 150–310 on edge-0 and edge-1. Edge-0 refused none while it was back on nginx (11:00–14:00Z), when nginx's refusals weren't counted.
- **QUIC:**
  - of 59 completed handshakes a second on 443, 36.4 (62%) were then refused by the per-address cap;
  - since the family counter's deploy, every QUIC cap refusal has been IPv4 (15.6 a second at 19:10–19:20Z, none on IPv6);
  - api's h3 connections and connect's are charged to the same cap (`nginx/front.go:AcquireQuicConnection`).
- **How the client retries:** its h1 dial retries a refused upgrade with its `ReconnectTimeout`, 5 s, and backs off. An address carrying many clients therefore keeps its upgrade rate high, while a single client behind it waits.

### 3.8 TLS and QUIC

**TLS over TCP, 18:30–18:50Z:**
- **Completed:** 4,419 handshakes a second, all TLS 1.3. 1,594 were resumed (36%).
  - h2 (api): 880 full and 642 resumed;
  - `http/1.1` (connect and gossip upgrades): 1,516 full and 952 resumed;
  - no ALPN: 429 full and none resumed.
- **Failures:**
  - `closed` 282 and `timeout` 172 a second;
  - the timeouts are 73 and 57 a second on edge-3 and edge-4, against 25 and 17 on edge-0 and edge-1;
  - `certificate` 0.26, `other` 0.25 and `not_tls` 0.11.

**QUIC, 18:30–18:50Z:**
- **443:**
  - completed 59 a second: v2 full 54.1, v1 2.0, resumed 2.9;
  - after the handshake: api 17.7, connect 4.8 and the cap 36.4;
  - failed: `idle_timeout` 93.1, `handshake_timeout` 0.12, `peer_closed` 0.05.
- **whodis:**
  - completed 1.3 a second;
  - after the handshake: api 0.98, connect 0.15 and the cap 0.17;
  - failed: `idle_timeout` 112.9.

**By address family** (`urnetwork_connect_front_quic_connections_by_family_total`, four blocks, 19:10–19:20Z):

| | Completed and served | Failed | Refused by the cap |
|---|---|---|---|
| 443, IPv4 | 17.1 a second (22%) | 45.9 (58%) | 15.6 (20%) |
| 443, IPv6 | 4.0 (22%) | 14.4 (78%) | 0 |
| whodis, IPv4 | 1.0 (1.4%) | 65.7 | 0.1 |
| whodis, IPv6 | no traffic | | |

Failure shares are about the same on every host: 53–64% on IPv4 443 and 74–82% on IPv6. So neither a host nor a family is broken.

### 3.9 Sessions and block balance

**h1 sessions, 18:30–18:50Z:**
- 907 accepted a second, 165 `client_canceled`, 5.5 `auth_frame`, 3.4 `upgrade_error` and 3.1 `state_refused`;
- 256.7k active, with a median life of 4.4 s (round 1: 5.4 s).

**Blocks.** services.yml weights connect's blocks equally: beta 1, g1 24, g2, g3 and g4 25 each.

| | g1 | g2 | g3 | g4 |
|---|---|---|---|---|
| Clients | 34.0k | 88.9k | 34.6k | 98.3k |
| Completed TLS handshakes a second | 555 | 1,637 | 767 | 1,445 |
| Accepted h1 sessions a second | 85 | 376 | 158 | 286 |
| api requests a second | 2,487 | 2,085 | 2,101 | 1,805 |
| connect cores, fleet | 30.9 | 41.6 | 29.3 | 42.9 |

- **The split hashes the full source address** (`warpctl/lbalt_split.go`, `--hmark-src-prefix 32/128`). api's even request rates show it spreads addresses evenly. connect's clients aren't spread evenly, so a few addresses must carry many connect clients each. Provider farms fit that: 105k of the 254k h1 sessions declare provide intent.
- **The same addresses would explain the refusals:** connect upgrades are refused more often than accepted, and handshake timeouts and refusals concentrate on edge-3 and edge-4.

**`connection_new` isn't broken.** LBALT.md §11.8 says it has read 0 since 05:06Z. In fact it ran at 9–14 a second through 07:30–15:00Z and at 18:00Z, and reads 0 only for two hours after each connect start. During that window, `serviceTransitionTime` (start plus 2 × `DrainAllTimeout`, 60 minutes) treats every new connection as established (`connect/transport.go:1103`). Today's five connect deploys kept it at 0 for much of the day.

### 3.10 A deploy that switches each block at its drain's end

**How it ran.** The family-counter build's tags were set for all blocks at 18:58Z. Each block switched by itself once its predecessor's drain ended: beta 18:54, g1 18:58, g2 19:05, g3 19:15 and g4 19:22Z. Beta's tag was set at 18:47Z.

**Effect:**
- **CPU:** one-minute busy CPU peaked at 83.0% on edge-1 (19:26Z), 78.5% on edge-3 (19:26Z) and 83.5% on edge-4 (18:55Z). Edge-0's peak, 69.8% at 18:47Z, was before any switch. Medians were 54–76%.
- **Clients:**
  - g2 fell from 88.2k to 80.1k (19:09Z), g4 from 100.1k to 90.1k (19:27Z), g1 by 2.3k and g3 by 3.0k;
  - the fleet fell from 258.6k to 245.8k at worst (−5%) and stood at 263.9k by 19:40Z.
- **Errors:** front `5xx` peaked at 0.87 a second.
- **Refusals:** they peaked at 3,961 a second, twice their median of 1,987, as reconnecting clients ran into the per-address limit.

**Compared with earlier deploys:** round 1's spaced waves peaked at 84–86% and the all-at-once deploy at 90–91% (LBALTOPT1 §8).

### 3.11 Kernel and NIC

18:30–18:50Z:
- **Conntrack:** 273–369k entries per host, against 364–487k on 11-19.
- **Listen queues:** no listen overflows. Listen drops were 0.03–0.6 a second, against 0.06–0.3.
- **NIC drops:**
  - on the lb interfaces (`eno*`), at most 2.3 a second all afternoon;
  - one 10-minute burst at 18:40Z on `enp94s0f1np1`, which isn't an lb interface: 27 a second on edge-3 and 36 on edge-4. It isn't lb_alt's.

### 3.12 h3 from the app

**What the server shows:**
- **Live h3 sessions:** 9,782 with declared provide intent, and 130 without, against 9,064 and 155 on 11-17. So app clients almost never hold an h3 session.
- **The data path works:** provider h3 sessions carry about 10k datagrams a second in and 35k out, as three days ago.
- **Refused logins:** h3 logins refused for their credential run at 0.6 a second, from builds 2026.11.16, 11.17 and 11.19. All are counted under `signature`, which also covers an empty or malformed token, and the detail is logged only at V(1). They ran on alt at 0.4–1.0 a second since at least 11-16, and moved to connect when alt retired, so they predate lb_alt.

**The probe** (one expiring guest from `/lag/session`, connect main e9c6f29d, from a Mac with IPv4 only):
- h1, h3 dialed as providers do (`connect.*`), and h3 dialed as the app does (`alt.*` and `alt-v4.*`, with and without the delay header) all completed their handshakes through the fronts.
- Each was then refused with close code 4005, h3 as QUIC application error 0xfa5 ("authorization refused"). A lag.dev guest's token serves only its lag.dev routes, so routing couldn't be tested.
- IPv6 had no route from the Mac.

**End to end, 20:09–20:15Z.** A throwaway network signed by a fresh wallet key, and two clients, A and B, each with one transport pinned to h3. Both dial as the app does: `alt.bringyour.com` with the delay header. Direct peer-to-peer was off: plain sends, and `AllowDirect(false)` for egress. The test then deleted the network.

| Run | h3 registered | Messages A→B / B→A, 64 B, 1,200 B and 8,000 B | Egress through B |
|---|---|---|---|
| Desktop | A 1.3 s, B at once | 150/150 each way; p50 312 / 490 ms with all 150 sent at once | DNS 60/60, p50 204 ms; TCP SYN to 1.1.1.1:443, 8/8 SYN-ACK, p50 166 ms |
| The phone's 20 MiB memory policy, one budget per client | A 1.1 s, B at once | 150/150 each way; p50 264 / 319 ms | DNS 60/60, p50 174 ms; SYN-ACK 8/8, p50 184 ms |
| The same, both transports on one budget | A never: waiting for budget for 60 s | – | – |

Both h3 lanes carried data: 150–200 datagrams and about 100 stream messages per transport. So h3 through the fronts routes data and egress traffic, including under the phone's policy.

**The phone's h3 budget.** The SDK sizes a phone's transports from its memory target (`sdk/device_local.go:89`, 20 MiB by default):
- the transport budget is 5.0 MiB;
- `ApplyMobilePlatformTransportMemoryPolicy` reserves 3.0 MiB for each h3 transport and 0.63 MiB for each h1 one;
- the multi client builds one transport per window client from that one budget (`connect/ip_remote_multi_client_api.go:1898`).

The consumer h3 transports that fit, by `publicH3WindowSizeLimitWithLock`'s own arithmetic (`sdk/device_local_h3_window_policy.go`):

| The phone's provider | Consumer h3 transports that fit |
|---|---|
| Not providing | 1 |
| Providing over h1 (three control paths) | 1 |
| Providing over h3 (three control paths, 9 MiB) | 0 |

At 0 the SDK still asks for a window of 1, which, as its comment says, "stays unready until a real charged carrier and provider can be established". So on a phone whose provider also runs on h3, the consumer's only h3 transport waits for budget forever, and h3 never routes data. That's the third run above. With the provider on h1, or not providing, the consumer gets a single h3 path. The iOS app keeps client and provider transport settings apart (`TransportSettingsStore`), so which case a user lands in depends on both choices. Desktops apply no memory policy.

## 4. Findings

**Performance**
- **P9. connect costs 10–29% more CPU per Gbps than on 11-19,** giving back 40–80% of nginx's saving. Host CPU per Gbps is 8–14% lower than on nginx (§3.1).
- **P10. The runtime walks its timer heaps often:** 10–12% of connect. Each walk visits about 55k timers a P, and the walks are set off by modified timers coming due, not by deleted ones (§3.4).
- **P11. The scheduler's search for work is 20–24% of connect,** and Ps wait for CPUs on hosts with 100 Ps for 72 CPUs (§3.2, §3.3). D15 raised GOMAXPROCS from 16 to 20 for the front.
- **P12. Exchange links cost as much in syscalls as clients' sockets** (§3.3).
- **P13. TLS handshakes are 4–6.5% of connect and 17% of its allocation.** About 31% of them serve a request that's then refused (§3.3, §3.5, §3.7).
- **P14. A copy-on-write listener list makes 13% of connect's allocation,** and grows with the square of residents (§3.5).
- **P15. Per-connection Redis and Postgres writes:** announce costs 4.5% and the Redis client 5.4% (§3.3).
- **P16. Deploys are paced well by the drain-end switch** (§3.10).

**Correctness and client experience**
- **C6. Shared IPv4 addresses are refused most of the time:**
  - half to two thirds of connect upgrades;
  - 62% of completed QUIC handshakes on 443;
  - 12–20% of all front requests.
  
  The limits are nginx's, keyed by address alone. A 429 carries no `Retry-After`, and api h3 and connect h3 share the QUIC cap (§3.7). Users behind CGNAT, mobile networks most of all, get transports that keep being refused.
- **C7. The connect blocks are imbalanced 2.7 to 1** at equal weights (§3.9). A deploy of g2 or g4 disconnects about 90–100k clients, and their processes need more of their GOMAXPROCS.
- **C8. A phone's transport budget can leave its h3 transport waiting forever** (§3.12). h3 through the fronts routes messages and egress end to end, on a desktop and under the phone's memory policy. But a phone's 5 MiB transport budget holds one 3 MiB h3 transport. With the phone's provider also on h3, it holds none, and the consumer's single h3 transport never leaves its budget wait. That's the "h3 doesn't route data" report. App clients hold almost no h3 sessions.
- **C9. The h3 signature refusals have no visible cause:** an empty token, a malformed one and a foreign signature all count as `signature` (§3.12).
- **C10. LBALT.md §11.8 misreads `connection_new`** (§3.9).
- **C11. R9 changed nothing measurable** (§3.6).

## 5. Recommendations

Ranked by impact over effort, with risk.

| # | Recommendation | Impact | Effort | Risk | How to measure |
|---|---|---|---|---|---|
| R13 | **Stop the server's clients registering network-quality listeners.** A client created inside the server, for example a resident's, skips `AddNetworkQualityChangeListener` (`connect/transfer_network_quality.go:97`) through a client setting. Separately, give `CallbackList` an O(1) `Add` and `Remove`: a map, with the read snapshot rebuilt only when it's stale. | 13% of connect's allocation, about 58 MB/s per process, and the quadratic growth removed | S | L | `slices.Clone`'s share in an allocation diff; GC's share of connect CPU. |
| R14 | **The timer heap walks.** (a) **Measure first:** count `Timer.Reset` and `SetDeadline` calls in the server's hot loops by call site and by horizon bucket (under 100 ms, 1 s, 10 s, more), as a bounded counter in one canary build, or replay the timer population of §3.4 in a benchmark. (b) **Remove the short-horizon modifications** that set the walk rate. (c) **Shrink the heaps:** fold a connection's announce, lease, idle-watch and ping timers into one per-process sweep; refresh socket deadlines only when less than half the timeout is left, since `exchangeDeadline.refresh`'s equality check never skips; drop the front's per-request h2 deadlines for a per-connection one. | Up to 10–12% of connect, 3–4 cores per host | M–L | M: timeout behavior | `timers.check`'s cumulative share; cores per Gbps (§3.1). |
| R15 | **Limits for shared addresses** (R3's decision). (a) Answer 429 with `Retry-After`, and have the SDK's dialers honor it. (b) After auth, charge authenticated requests to the client or network as well as to the address, as api already does for `/connect/control`, and give the address limit IPv4 headroom. (c) Count api h3 and connect h3 against separate QUIC caps, so an address's api calls can't refuse its transport. (d) Add a cardinality-safe concentration measure: distinct addresses refused a minute, and the most-refused address's share, as counts only. | High: C6, and about 31% of handshakes | M | M: abuse controls | Refused share of connect upgrades and of QUIC handshakes; `client_canceled` per active session. |
| R16 | **Block balance.** Count, per block, the addresses with more than 10, 100 and 1,000 active connections, as a bucketed gauge. If a few addresses carry the imbalance, decide on hashing connect's TCP by source port as well, or capping one address's share of a block. Weigh that against D-level reasons for keeping an address on one block: TLS resumption and resident locality. | Medium: C7, deploy size and per-block headroom | S to measure, M to change | M | Clients per block; the bucketed gauge. |
| R17 | **A GOMAXPROCS trial:** `cores: 16` on connect for one off-peak window (warpctl's `cores` is per service). Do it after R16, since the busiest blocks reach 10–12 cores per host now. | Medium: part of P11's 20–24% | S | M: g2 and g4 at peak | Cores per Gbps; scheduler p99; running time against CPU (§3.2). |
| R18 | **Batch announce writes:** one Redis pipeline per round for a process's liveness leases, and batched measurement rows to Postgres. | Up to about 3% of connect | M | L–M | The announce share in a profile; Redis commands a second. |
| R19 | **Redis client:** measure whether the pinned go-redis can skip `connCheck` for a connection used within the last second; if not, carry a small patch. `redisTtlWarnHook` wraps every command, so check that its per-command work stays constant. | 0.8–1% of connect | S | L | `checkSyscallConn` in a profile. |
| R20 | **h3 on phones** (C8). (a) Size a phone's h3 reservation so the provider's control paths and at least two consumer transports fit in the budget: smaller h3 receive windows, or count the pinned family control paths at their real use. (b) When the budget can't admit a transport's h3, fall back to h1 for that transport rather than waiting. (c) Split the `signature` rejection into malformed, empty, unknown key and bad signature, as a bounded label. (d) Decide whether whodis should carry api calls at all, since 73 a second try it and 1.4% complete. | High: client connectivity | S–M | M: phone memory | App h3 sessions (§3.12); §3.12's end-to-end runs under the phone's policy, with the provider on h3. |
| R21 | **Exchange links:** measure frames per syscall on the links from `recordExchangeIO` against the write and read syscall rate. Then coalesce small writes, and measure what share of forwards cross processes, to see whether resident locality would pay. | Medium: P12's 7.8% | M | L | Frames per syscall; exchange syscall share. |
| R22 | **Handshake cost:** R15 removes most refused handshakes. Keep X25519MLKEM768; its ML-KEM half is about 1.4% of connect (§6). Raise resumption above 36% by sharing ticket keys among a host's blocks, which revisits D6c, and identify the 429 full handshakes a second that offer no ALPN and never resume. | Medium | M | M: ticket keys | Resumed share; `serverHandshake`'s share. |
| R23 | **Allocation trims:** pool the h1 writer's batch (6% of allocation), and size per-transport buffers lazily, since sessions last a median 4.4 s (25%). | Low–medium: GC | S–M | L | Allocation diff (§3.5). |
| R24 | **Operations:** make the drain-end switch (§3.10) the documented way to deploy connect, and correct LBALT.md §11.8's `connection_new` note (C10). | Low | S | L | – |

## 6. Not recommended now

- **Dropping X25519MLKEM768 for X25519:** it would save about 1.4% of connect and give up post-quantum protection for every client.
- **GOGC or GOMEMLIMIT changes:** GC is 3% of connect, though R13 should be measured on day-old processes first.
- **Kernel or NIC tuning:** nothing measured is lost on the lb interfaces (§3.11).

## 7. Open questions

- Which modified timers come due soonest and so set the walk rate? (R14)
- Who are the addresses behind the refusals and the block imbalance: provider farms, CGNAT or abuse? (R15, R16)
- How should a phone's h3 budget be split between its provider's control paths and its consumer window: smaller windows, or an h1 fallback? (R20)
- What are the 429 full TLS handshakes a second with no ALPN that never resume?
- Do day-old processes spend more on GC than these 12–29-minute-old ones? Take a profile at tomorrow's peak.
- Why do edge-3 and edge-4 see 2–4 times the refusals and 3–5 times the handshake timeouts of edge-0 and edge-1? Their interfaces' DNS weights, or the address mix?
- What was the 18:40Z burst of drops on `enp94s0f1np1`?

## 8. Implemented, 2026-11-20, not merged

**R13**, connect `483e5850` and server `0e8302295`:
- `ClientSettings.SkipNetworkQualityListener` keeps a client out of the process-wide listener list;
- the exchange sets it for residents' clients.

**R14**, connect `9b3f316a`:
- `sequenceDeadlineTimer.armBy` stops a pending timer and arms a new one for an earlier deadline, rather than moving it. Every deadline fires when it did.
- In `BenchmarkSequenceDeadlineTimerEarlierArmAmongManyTimers`, with 400k timers spread over the Ps, the heap walk falls from 32.8% to 2.4% of the benchmark's CPU, and CPU per op falls 12%.

**What R14 doesn't do:**
- **Batching socket-deadline refreshes:** dropped. It shortens a healthy operation's budget, which the owner's 2026-10-30 rule and `TestExchangeDeadlinesPreserveEachOperationBudget` forbid.
- **Folding per-connection timers into one sweep:** still open. That's the heap-shrinking half.

**R15**, server `e42fd11bb` and connect `8de486b0`:
- (a) the front's 429 carries `Retry-After`;
- (b) the client waits that long before redialing, and backs h3 off 5–30 s after a QUIC cap close;
- (c) api h3 and connect h3 count against separate QUIC caps, with `name_class` on `urnetwork_front_limit_refusals_total`;
- (d) an identity limit for verified tokens. It stays off until warpctl renders its route-table fields:
  - proposed `identity_requests_per_minute: 120`, `identity_burst: 60`, `identity_address_factor: 20`;
  - each identity is also held to `net_connections` in flight;
  - turned on, the front's token checks add to `urnetwork_auth_jwt_rejections_total{service="connect"}`, and a legacy token without a network id costs a Redis read on admission.

**Dashboards**, server `60b5f4fec`: `lbalt.json` and two panels make `TestInternalDashboardsCoverEveryApplicationMetric` pass.

**Verified with:**
- the client library's sequence, buffer, deadline and quality tests;
- server connect's end-to-end, resident, announce and client-close tests on the local stack (21 minutes);
- `./nginx`;
- `./grafana` with promtool;
- R15's own suites.

### 8.1 R14's second half: logical timers on a sweep

Server branch `feat/connect-timer-sweep-1120`, not merged or deployed. It shrinks the heaps the walks visit (§3.4) by taking a connection's logical timers out of them. Socket deadlines and heartbeats stay as they are.

**The sweep** (`internal/timersweep`):
- **One per process.** `newExchange` makes it. The handler gives it to every announcement and authorization lease, and the exchange's residents and forwards use it.
- **Sharded:** one hashed timing wheel per P, GOMAXPROCS shards, each with its own lock. New timers take the shards in turn. A wheel has 1024 slots of one tick, so a rotation is 102.4 s, and a deadline further out waits in its slot through the earlier passes.
- **Fixed tick, 100 ms.** A timer fires at the first tick boundary at or after its deadline: never early, and at most 100 ms late, before the scheduling delay every timer has. A timer re-armed when it fires drifts by at most a tick a period. A ticker keeps its cadence from its start, as a `time.Ticker` does.
- **Driven by one runtime timer per shard,** re-armed only after it fires. A timer that's in no heap when it's re-armed is simply added, so the drivers never set off a walk. A shard's driver runs only while the shard holds an armed timer, so an idle sweep has no goroutine and no runtime timer, and needs no close.
- **A fire never blocks the sweep.** It's a non-blocking send on the owner's one-slot channel, made under the shard's lock, and the owner's goroutine does the work. A fire the owner hasn't received absorbs later ones. The sweep never calls owner code or logging.
- **Cancellation and teardown:** each owner stops its timer as it returns. Arming, re-arming and stopping each unlink or link one entry, O(1), and a stopped timer leaves no entry behind. As with a Go 1.23 timer, `Reset` and `Stop` discard a fire not yet received, so no stale fire arrives after either returns.
- **Memory:** an 80-byte timer and its 112-byte channel, 192 bytes in two allocations, plus each shard's 8 KiB of slots. A runtime timer with its channel takes 248 bytes in three.
- **Without a sweep,** an owner built directly, as tests build them, gives each timer a sweep of its own. Its tick is the process's for a wait of 1.6 s or more, and a sixteenth of a shorter wait, so a test that compresses a 30 s round to 50 ms keeps the sweep's proportions. Inside a synctest bubble that sweep's driver stays in the bubble.

**Converted.** Each fires at its deadline or at most 100 ms after it, never before:

| Timer | Deadline | Why a 100 ms delay is harmless |
|---|---|---|
| Announce window, two halves | 2.5 s each | Registration comes at most 200 ms later. The pending marker has 5 s of slack beyond the window. |
| Announce sync round | 30 s, retries up to 120 s | A round's stats cover the time since the last round ended, so a late round loses nothing, and a 30.1 s round still starts inside the previous 60 s block, which the rollup hasn't drained. |
| Announce test schedule | 12–24 h, random | Against a random 12–24 h. |
| Announce verify egress refresh | 2 min | The lease lives 10 min. |
| Passive speed sample | 5 s window | A window runs 5.0–5.1 s, and the sample divides by its measured length. |
| Resident idle watch | 300 s from the last activity | An idle resident lives at most 100 ms longer. Activity still moves the deadline, and retirement never comes before it. |
| Resident registration refresh | 75 s after the last refresh | The registration's ttl is 300 s. |
| Resident stream grant republish | every 30 s from the resident's start | A grant's lease is 90 s. |
| Forward idle watch | 3 min from the last payload | An idle forward lives at most 100 ms longer, never ending before its deadline. |
| Lease horizon | the credential's horizon | The horizon already carries the 30 s clock leeway past the token's expiry, and a recheck also retires a credential past it. |
| Lease recheck ticker | every 60 s from the lease's start | A lease's deadline is 90 s after its last check starts. |

**Left as they are:**
- **Socket deadlines,** native, by the 2026-10-30 rule (CPUOPT-KEEPALIVE.md).
- **Heartbeats:** the h1 writer's ping, reset per idle transition; the exchange writer's pinger (`newExchangePinger`); and a forward's ping worker (`handleExchangeConnection`, the 81k in §3.4). Each promises the peer a frame at least every ping interval, and the exchange's TCP keepalive starts probing 1 s after a missed ping. A tick late would break that promise. Arming a tick early would keep it, at about 1% more pings, but that's a change to a heartbeat and isn't made here.
- **Per-message bounds:** a forward's write timer (`runWithResidentLookup`) bounds one send into a full queue, and it's in a heap only while such a send waits.
- **The client library's timers** (`resetOrCreateTimer`, `sequenceDeadlineTimer`) and quic-go's.
- **Retries during an outage:** the measurement write retry and the lease's unanswered-recheck retry, which run only after a failure.
- **`registerWhenResident`,** whose first poll is 100 ms: a tick would double it, and it lives at most 30 s.
- **A resident's stream hop and network peer listeners** (`model`), one of each per resident: jittered polls, every 10 min in production as the backstop to key events. They're the next candidates, but they're in `model`, outside this change.

**Measured** on an Apple M1 Max, 10 cores, shared with other sessions' builds and tests (load averages 110–260). The figures compare the two sides of the change; they don't predict production.

- **Runtime timers a simulated connection holds** (`TestConnectionTimerCensus`, 64 connections on the local stack, counted 8 s in). A connection is a resident nominated on the production path, one forward from it, an announcement with verify refreshes on, and a lease with a credential horizon:

  | | main `68c19400d` | This branch |
  |---|---|---|
  | The converted owners' runtime timers | 10.00 | 0 |
  | Their sweep timers | 0 | 10.00 |
  | Every runtime timer the connections made | 13.61 | 3.27 |

  Before, the announcement held 4 (rounds, test schedule, verify refresh, passive samples), the lease 2, the resident 3 and the forward 1. What's left is the resident's client's send sequence deadline and route write timer, 2.0 a connection, the stream hop listener's poll, 1.0, and the sweep's 10 drivers, 0.16 a connection here.
- **One heap walk** (`BenchmarkTimerHeapWalkCost`): one P, its heap at §3.4's 55,000 timers before and 31,786 after, the table's 42% taken out, with random deadlines so the walk misses cache as a server's does. Each op moves a pending timer ahead of the rest, so the P walks its heap.
  - CPU a walk: 367–412 µs before and 131–179 µs after, over six runs of 3,000 walks each; 389 and 143 µs over 100,000.
  - `runtime.(*timers).check` is 78–95% of the benchmark's CPU on both sides. Over 3,000 walks it fell from 0.97–1.16 s to 0.35–0.49 s.
  - That's about 7 ns a timer before and 5 ns after: the smaller heap stays in cache better, so the CPU falls further than the timer count, by 60–64%. If production's walks scale between the two, `timers.check`'s 10–12% of connect would fall by 42–64% at the same walk rate, before R14's first half lowers the rate.
- **The sweep's own cost:**
  - arming and stopping a timer, 31–63 ns an op on 10 Ps in parallel, without allocating, about twice a runtime timer's 15–21 ns;
  - a fire and its receipt, the owner's wake included, 1.0–1.5 µs of CPU, against 0.83–0.96 µs for a runtime channel timer (4,096 owners, 1 ms tick);
  - the drivers' ticks over 550,000 armed timers, 15–26 µs a shard tick, or 2.7–4.7 ns a timer a second: about 2 ms a second for a process's 600,000;
  - memory, 192 bytes and 2 allocations a timer with its channel, against 248 bytes and 3 for a runtime timer.

**Tested:**
- `internal/timersweep`, under the race detector: synctest tests that a timer fires at the first tick at or after its deadline, never early and less than a tick late, through `Reset` and `Stop` (none leaves a stale fire, even at the deadline's own instant), wheel rotations and a deadline past the wheel's range; that a ticker keeps its cadence; that an owner that never receives doesn't hold up the sweep; and that a shard's driver runs only while it holds a timer. In real time, a driver held for three rotations fires every due timer once, and owners on every shard arm, reset and stop at once.
- **Each converted owner,** in a synctest bubble, with its deadline between two ticks: it fires at its deadline or at most a tick after it, never before, and its timer leaves the sweep when the owner ends. That's the resident's and the forward's idle watches, the registration refresh, the passive samples, and the lease's horizon and recheck cadence. The stream grant republish is checked for its one ticker on the sweep, whose cadence the package's tests cover. The announcement's window, rounds, test schedule and verify refreshes run in `run()`, which needs the database and Redis, so they're tested in real time on the local stack, each fire against its own arm.
- **The wiring:** the exchange makes the sweep, the handler gives it to its announcements, and h1 and h3 transports' leases wait on it.
- **The existing tests,** with the timer counts of the announce round and passive sample tests now asserting no runtime timers.
- **Suites, on the local stack:** `internal/timersweep` with `-race`; the whole `session` package (25 minutes); and 522 of server connect's tests, every announce, resident, forward, client-close, exchange, move, peer, session, stream, transport-auth and timer test (29 minutes). The connect run first found a test fixture's resident without an exchange, whose stream grant ticker now gets a sweep of its own, and two announce tests that compress a 30 s round to 50–100 ms, which a timer's own sweep now scales its tick for.

## Appendix A. Queries and profile commands

**Queries.** Instant queries over the window, at its end, against Mimir. LBALTOPT1 Appendix A has the CPU, traffic and runtime queries. New here:
- **Connect per Gbps:** `sum by (host) (rate(process_cpu_seconds_total{service="connect"}[W]))`, over the NIC Gbps.
- **Running time against CPU:** `sum by (host) (rate(go_cpu_classes_user_cpu_seconds_total{service="connect"}[W]))`, against the above.
- **Handshakes:** `sum by (version, resumed, alpn) (rate(urnetwork_connect_front_tls_handshakes_total[W]))`; `sum by (port, version, resumed) (rate(urnetwork_connect_front_quic_handshakes_total[W]))`; `sum by (port, family, result) (rate(urnetwork_connect_front_quic_connections_by_family_total[W]))`.
- **Refusals:** `sum by (limit, zone) (rate(urnetwork_front_limit_refusals_total[W]))`. Their share is that over `sum(rate(urnetwork_front_requests_total[W]))`. The refused share of connect upgrades is `status_class="4xx"` over `1xx|4xx`.
- **api's own latency:** `histogram_quantile(q, sum by (le) (rate(urnetwork_http_request_duration_seconds_bucket{service="api"}[W])))`, against the front's header time.
- **Blocks:** `sum by (block) (…)` of clients, TLS handshakes, accepted h1 sessions, api requests and connect CPU.
- **h3 sessions:** `sum by (transport, intent) (urnetwork_connect_provide_intent_connections)`.
- **Credential refusals:** `sum by (carrier, app_version) (rate(urnetwork_connect_credential_refusals_total[W]))` and `sum by (cause) (rate(urnetwork_auth_jwt_rejections_total{service="connect"}[W]))`.

**Profiles** (LBALTOPT1 Appendix C for taking them):
- **Shares of key functions:** `go tool pprof -top -cum <profile>`.
- **Callers:** `go tool pprof -peek '<function>$' <profile>`.
- **Call sites:** `-lines` with `-peek`. That's how the timer walks were placed at `runtime/time.go:1012`.
- **Kinds of work:** `go tool pprof -traces <profile>`, grouped by each stack's deepest frame outside the runtime, skipping `server.HandleError`.
- **Timer owners:** `go tool pprof -sample_index=inuse_objects -peek '^time.NewTimer$|^time.newTimer$|^time.AfterFunc$' <heap>`.
- **Allocation over a window:** `go tool pprof -sample_index=alloc_space -base <first allocs> -top <second allocs>`, and `-peek` for callers.
