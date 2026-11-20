# LBALTOPT1: lb_alt in production, measured, and what to improve next

Status: measured 2026-11-20, the day lb_alt reached all 10 non-transparent lb interfaces of edge-0, 1, 3 and 4 (LBALT.md §11.8). Read-only: Mimir, the edges' cgroup and /proc files read as an unprivileged user, and two public TLS probes. Of §6, only parts of R4 and R12 are implemented, not yet deployed (2026-11-20). Times are UTC.

## 1. Summary

**lb_alt saves about 17% of host CPU for the same traffic.**
- **Within-host test:** the cleanest measurement comes from edge-0. One stale controller rolled it back to nginx from 09:08 to 14:08Z (LBALT.md §11.8), with the same connect build and config before and after. Its busy CPU per Gbps of NIC traffic, relative to the three edges that stayed on lb_alt:
  - 1.20× on nginx;
  - 0.97× and 0.99× in its two lb_alt windows.
- **Across the two days:** busy cores per Gbps fell 12–19% on edge-1, 3 and 4. Edge-0, on nginx in both windows, fell 1.5%.
- **Where it comes from:** host CPU outside the Go services fell from 10.5–13.9 cores to about 2. System and softirq time carry most of the saving. nginx's loopback hop, 1.2–1.6 Gbps of loopback traffic per host, is gone.
- **Fleet-wide:** about 35 cores at midday across the four edges.

**The front costs connect very little.** On edge-0, connect's own CPU per Gbps was the same on nginx (8.20) as on lb_alt (7.99–8.37). connect does allocate 15–25% more per Gbps with the front. Without a profile that can't be attributed (§4).

**At the morning peak, host CPU is at or below the day before.**

| | Peak, 11-20 | Peak, 11-19 | Mean, 11-20 | Mean, 11-19 |
|---|---|---|---|---|
| edge-1 | 80.7% | 80.7% | 75.0% | 79.0% |
| edge-3 | 67.3% | 81.0% | 58.6% | 73.7% |
| edge-4 | 76.6% | 80.5% | 70.4% | 76.9% |

Window: 07:30–13:55Z. Edge-4 carried 80k clients, against 62k the day before. Memory isn't a constraint: each host has 450–700 GiB available.

**The issues worth fixing are mostly outside raw CPU** (§5):
- **Refusals:** the per-address limits refuse 14% of front requests. connect's upgrade requests are refused about as often as they are accepted, as they were under nginx.
- **Failed handshakes:** since the `alt.*` move at 06:32Z, most QUIC handshakes on the alt path fail. 443 completes about 20% of them and whodis 1–2%, and the QUIC failures have no reason. TLS handshakes over TCP fail 463 times a second; their reason is counted since the connect hotfix of 15:36–15:42Z (5ba2c9f6f on main), and the failures are clients closing (60%) or timing out (40%), with no version, cipher or ALPN failures (§3.7).
- **Unalerted rollbacks:** edge-0 was rolled back twice with no alert.
- **No way to profile:** nothing can profile the front in production, and api has no latency histogram.

**First recommendations** (§6):
- R1: a loopback-only pprof endpoint, which every later CPU decision needs.
- R2: rollout safety. run-*.sh would refuse a stale controller, and the monitor's `lbalt_status` would be enrolled.
- R3: refusal zones, then limits that know a client rather than only its address.
- R12: the alt path's QUIC failures, a possible cause of the h3 failures users report.
- R4: handshake accounting.
- R5: runtime metrics before any GOGC or GOMAXPROCS change.

## 2. Method and windows

| Window | What it compares |
|---|---|
| 10:00–13:00Z, 11-20 against 11-19 | edge-1, 3 and 4 on lb_alt against the same hours on nginx. edge-0 was on nginx in both, so it is the control. |
| edge-0, three windows on 11-20 | 08:20–09:05Z on lb_alt (after its re-apply's drain), 10:15–14:05Z on nginx (after the rollback's fronts drained), and 15:10–15:35Z on lb_alt (after the second drain, before the connect deploy). Each is compared with edge-1, 3 and 4 in the same window. |
| 07:30–13:55Z, 11-20 against 11-19 | Peak and mean host CPU, after the rollout's nginx drains ended. |
| 15:04–15:24Z, 11-20 | The front's request mix, latency, forwarder, limits and handshakes, with every interface fronted. |

Reconnect waves are excluded: the rollout's drain ends at 07:16–07:22Z, edge-0's drain end at 15:08Z, and the connect hotfix that carries the TLS failure reasons. That hotfix is `2026.11.20-outerwerld+1103888290`, server 997b59696; it reached beta at 15:36:01–15:36:54Z and g1 to g4 at 15:39:58–15:41:51Z. Busy CPU is every mode but idle and iowait, from `node_cpu_seconds_total`. Traffic is NIC receive plus transmit on the `en*` devices. The node and process series come from the stats push and node exporter. Appendix A lists the queries.

Two confounds limit the cross-day comparison:
- connect's build changed overnight: the canary build at 01:54Z, then the metrics hotfix at 05:06Z.
- `cores: 20` replaced 16 (D15).

The edge-0 windows share one build and config, so §1 leads with them.

## 3. Measurements

### 3.1 Host CPU against traffic

10:00–13:00Z:

| Host | Busy cores, 11-20 | 11-19 | NIC Gbps, 11-20 | 11-19 | Cores per Gbps, 11-20 | 11-19 | Change |
|---|---|---|---|---|---|---|---|
| edge-0, on nginx both days | 49.6 | 55.6 | 3.82 | 4.21 | 12.99 | 13.19 | −1.5% |
| edge-1 | 51.6 | 53.5 | 4.50 | 3.80 | 11.45 | 14.09 | −18.7% |
| edge-3 | 40.2 | 49.9 | 3.85 | 3.98 | 10.45 | 12.52 | −16.5% |
| edge-4 | 47.2 | 51.6 | 4.42 | 4.23 | 10.69 | 12.21 | −12.4% |

Edge-0's three windows on 11-20. The ratio column is edge-0's cores per Gbps over the mean of edge-1, 3 and 4 in the same window:

| Window | edge-0 cores/Gbps | Ratio | Non-Go CPU | System + softirq | connect cores/Gbps | Loopback Gbps |
|---|---|---|---|---|---|---|
| lb_alt, 08:20–09:05Z | 11.23 | 0.965 | 2.9 | 14.9 | 8.37 | 0.39 |
| nginx, 10:15–14:05Z | 12.95 | 1.204 | 10.7 | 20.5 | 8.20 | 1.14 |
| lb_alt, 15:10–15:35Z | 10.35 | 0.989 | 2.7 | 16.3 | 7.99 | 0.34 |

### 3.2 Where the CPU goes, by mode and process

10:00–13:00Z, in cores:

| Host | user, 11-20 / 11-19 | system | softirq | Go services' CPU | Non-Go CPU |
|---|---|---|---|---|---|
| edge-0, on nginx both days | 29.3 / 32.1 | 12.0 / 14.0 | 8.3 / 9.5 | 39.0 / 41.9 | 10.5 / 13.7 |
| edge-1 | 30.2 / 29.5 | 12.5 / 14.6 | 8.9 / 9.4 | 49.5 / 40.3 | 2.1 / 13.2 |
| edge-3 | 25.8 / 28.8 | 8.1 / 12.7 | 6.3 / 8.4 | 38.0 / 37.2 | 2.2 / 12.7 |
| edge-4 | 30.2 / 28.7 | 9.6 / 13.8 | 7.5 / 9.1 | 45.1 / 37.7 | 2.1 / 13.9 |

- **Go services:** connect, api, taskworker, gossip and mcp, from `process_cpu_seconds_total`.
- **Non-Go CPU:** busy CPU minus the Go services. That's nginx and its kernel work while it ran, plus dockerd, fluent-bit and node tasks, which are about 2 cores.
- **connect** is 75–85% of busy CPU on every host, at 7.8–9.5 cores per Gbps.
- **On edge-3's connect containers**, read from their cgroups at about 15:20Z with the processes up 9.3 h, system time is 31–34% of each process's CPU.

### 3.3 connect's runtime

| | 11-20 | 11-19 |
|---|---|---|
| GOMAXPROCS | 20 (D15) | 16 |
| GOGC / GOMEMLIMIT | 100 / unset | 100 / unset |
| GC cycles, per host | 0.04 a second: about one per 125 s per block | the same |
| Heap in use, per host | 182–206 GB | 166–217 GB |
| Goroutines, per host | 3.6–4.1M, about 61 per client | 3.4–4.5M, about 62 per client |
| Resident memory, per host | 233–263 GiB | 211–292 GiB |
| Allocation rate, per host, 10:00–13:00Z | 673–744 MB/s | 517–759 MB/s |

- **edge-3's connect containers:**
  - none has a CPU quota (`cpu.max` is `max`), so `cores: 20` sets GOMAXPROCS only, and `nr_throttled` is 0;
  - CPU pressure averages at most 0.38% over 10 s, and memory pressure is 0;
  - resident memory per block is 2 GiB (beta), 44, 44, 75 and 79 GiB.
- **Allocation on edge-0:** it allocated 240.7 and 234.9 MB per Gbps in its lb_alt windows, against 189.8 MB per Gbps on nginx (+23–27%). Allocations ran at 1.29–1.33M per Gbps against 1.15M (+12–16%), or about 0.6M allocations and 190 MB more a second at 15:10–15:35Z.

### 3.4 Requests through the fronts

15:04–15:24Z, fleet-wide, in requests a second:

| Service | h1 | h2 | h3 | 1xx | 2xx | 4xx | No response |
|---|---|---|---|---|---|---|---|
| api | 1,657 | 7,769 | 38 | – | 8,629 | 785 | 51 |
| connect (in-process) | 2,047 | – | – | 926 | 194 | 926 | – |
| gossip | 41 | – | – | 31 | – | 9 | – |
| grafana, mmm, lb status | 24 | 2 | – | – | 26 | – | – |

Response-header time, excluding 1xx and requests with no response:

| Service | p50 | p90 | p99 |
|---|---|---|---|
| api | 0.9 ms | 30 ms | 251 ms |
| web, app, mcp, mmm | 0.5–0.6 ms | 0.9–1.2 ms | 1.8–3.9 ms |
| taskworker | 0.6 ms | 1.7 ms | 7.3 ms |
| connect | 0.6 ms | 5.0 ms | 716 ms (2xx alone: 6.9 s, its long-lived responses) |

api has no latency histogram of its own in Mimir: `urnetwork_http_request_duration_seconds` has had no series since at least 11-19. So api's processing can't be separated from the forwarding, though the 0.9 ms median bounds the forwarding.

### 3.5 The forwarder

15:04–15:24Z:
- **Upstream attempts:**
  - api: 8,741 a second answered, 49 a second ended by the client, 0.012 a second answered 503;
  - every other service: under 32 a second;
  - stale-connection redos: none.
- **Retries:** 0.005 a second after a 503, 0.0017 after a timeout. Forward errors: none.
- **Body refusals, all api:** 9.0 a second incomplete, 0.98 canceled, 0.87 timeout.
- **Tunnels:** 2,365 open to gossip, 12 to grafana.
- **Body budgets, at most over the 9 h after 06:21Z:** 93 KB of the 256 MiB memory budget, 1.9 MB of the 4 GiB spill cap, and 1 of 1,024 spill files.

### 3.6 Limits

15:04–15:24Z: 1,440 refusals a second from the request limit, 168 from the concurrency limit and 25 from the QUIC per-address cap.
- The HTTP refusals are 13.9% of the front's 11,578 requests a second, and nearly every 4xx.
- The connect route refuses about as many upgrade requests as it accepts: 926 against 926.
- Nginx refused the same, about 1,147 a second on edge-3 and edge-4 before the switch (LBALT.md §11.8), so this isn't a regression.
- `urnetwork_front_limit_refusals_total` has only a `limit` label, so which route zone refuses can't be read.

### 3.7 TLS and QUIC

- **Served TLS** (two public names, probed): ECDSA P-256 certificates, TLS 1.3, TLS_AES_128_GCM_SHA256, and one session ticket per connection. A second connection from the same source resumed.
  - The SDK keeps a utls LRU session cache (`connect/net_tls_hello.go`), so clients resume when they return to their block. HMARK keys on the source address, so they do return.
  - D6c's per-process ticket keys mean a block restart invalidates its tickets.
- **TLS handshake errors:** 463 a second over TCP. At about 15:20Z that was edge-0 78, edge-1 65, edge-3 148 and edge-4 167. Another 1.6 a second fall under `other`.
- **TLS failure reasons**, counted since the 15:42Z hotfix (connect `2026.11.20-outerwerld+1103888290`). At 16:05–16:25Z, 20–40 minutes after the deploy, with the failure rate back at its pre-deploy level, 418 failures a second:
  - `closed`: 252 a second (60%);
  - `timeout`: 166 a second (40%);
  - `certificate` 0.30, `other` 0.21, `not_tls` 0.12;
  - `version`, `cipher`, `alpn` and `client_alert`: none.

  These are clients that hang up or stall mid-handshake, not incompatible clients. Timeouts run 62–71 a second on edge-3 and edge-4 against 14–20 on edge-0 and edge-1, and `closed` runs 45–81 a second per host.
- **QUIC connections, a second:**

  | Port | api | connect | rate_limited | handshake_failed |
  |---|---|---|---|---|
  | quic (443) | 18.3 | 4.5 | 24.7 | 104.4 |
  | whodis (53, 4053) | 1.26 | 0.19 | 0.16 | 105.8 |

- **Sockets:** 1,600 front sockets up, with no restart and no bind failure since the processes started.
- **When the QUIC failures began.** `handshake_failed` was 0.2 a second at 06:30Z, after the 06:15–06:21Z switch. By 07:00Z it was 89 a second on 443 and 89 on whodis together. So it began with the `alt.*` move at 06:32Z, when the SDK's alt h3 and alt whodis dialers (`connect/net_http_alt.go`) started reaching the fronts. It has run since:
  - 443: 68–112 a second failed, against 13–22 completed for api and 2–20 for connect;
  - whodis: 83–105 a second failed, against about 1.5 completed, so 1–2% of whodis handshakes complete.
- **By host, 14:30–15:30Z:**
  - 443 failures run 22–29 a second on each edge;
  - whodis failures run 13–14 a second on edge-0 and edge-1, which have no router rules for 4053, and 37–38 on edge-3 and edge-4.
- **Probes from outside**, over IPv4, pinned to one edge:
  - h3 `/hello` answers 200 with SNI `api`, `api-v4` and `api-v6.bringyour.com`;
  - the QUIC handshake fails with SNI `alt.bringyour.com` or `alt-v4.bringyour.com`, names the fronts don't serve on QUIC. `nginx/tls.go:newTlsConfigs` gives an unknown SNI no certificate on QUIC, as alt did for names it didn't serve, while TCP gives it the default server's certificate;
  - the client has presented the api name rather than the alt one since fc3da5a0 (2026-09-13). Older builds that present an alt name can't complete h3 or whodis through the fronts.
- **connect's own h3 transport recovered:** `urnetwork_connect_h3_datagram_events_total` ran 53–79k events a second before the switch, dipped to about 22k during the rollout, and reached 95k by 16:00Z.

### 3.8 connect sessions

At 15:05–15:25Z:
- 918 sessions accepted a second, 199 `client_canceled` a second, and 263k active;
- mean session 246 s, median 5.4 s.

The same minutes of 11-19 had 773 accepted, 267k active and a 6.0 s median. That window includes edge-0's 15:08Z drain-end reconnects.

Steady state, 11:00–13:00Z on edge-1, 3 and 4:
- sessions closed per active session moved by −10% to +20% per host from 11-19;
- the median session was 3.9 s against 4.9 s;
- `client_canceled` rose 9–28%.

### 3.9 Kernel and NIC

- **Conntrack** at 15:25Z: 315–450k entries per host, against 375–487k at the same time on 11-19. The limit is 2.1M.
- **NIC receive drops:** at most 2 a second (edge-4), unchanged from 11-19.
- **edge-3:**
  - no softnet drops, and 295k time squeezes in total since boot;
  - 176 UDP sockets, 7 of which dropped 2,812 datagrams in all since 06:05Z;
  - `net.core.rmem_max` is 1 MiB;
  - 315k TCP connections established, 12.8k orphaned and 8.2k in TIME_WAIT.

## 4. Where connect's CPU goes, compared with nginx

**nginx's share.** LBALT.md §7.1 put nginx at 9–10 cores and 17–24 GiB per services host. The measured non-Go CPU drop says the same: about 11 cores per host on 11-19 against about 2 on lb_alt, and 10.7 against 2.7–2.9 on edge-0. Those cores came out of system and softirq time, besides nginx's own user time. Two hops are gone:
- nginx's TLS-to-loopback proxying of connect's websockets, which carried 1.2–1.6 Gbps of loopback traffic per host;
- its UDP PROXY path.

**The front inside connect.** On edge-0, connect's CPU per Gbps didn't move between nginx and lb_alt (8.20 against 7.99–8.37). The pieces the front added are each small at today's rates. These are estimates; R1's profile replaces them:
- **TLS record crypto:** AES-128-GCM on 3.3–5.0 Gbps a host is about 0.3–0.6 cores with AES-NI.
- **Handshakes:** about 500 a second per host, counting the accepted sessions, the canceled ones and the failures, at ECDSA P-256 plus X25519. That's about 0.1 core.
- **h2 serving and forwarding:** 2–4k requests a second per host is about 0.1–0.3 cores.
- **Per-write bookkeeping:** `frontTrackedConn.Write` (`connect/front_http.go`) takes one uncontended mutex per write. connect sets its own write deadline on every write (`connect/transport.go`, `connect/resident.go`), so the front's send_timeout bound is skipped. That's about 60k writes a second per host, negligible.

**Allocation.** connect allocates 15–25% more per Gbps with the front, about 0.6M allocations and 190 MB a second on edge-0. A plausible but unconfirmed list of sources:
- each handshake: crypto/tls state, plus net/http's pooled bufio reader and writer, which a hijacked connection keeps;
- h2 per-request state;
- `forwardRequestHead`'s 1 KiB head;
- per-request metric label work.

At GOGC 100 with 36–41 GB heaps per block, GC runs about every 125 s per block. A mark of a ~30 GB live heap at ~1.5 GB/s per core is about 20 core-seconds, so an estimated 0.15 cores per block, 2–4% of connect. The extra allocation therefore costs little CPU today. It is the first thing R1 should confirm.

**The rest of the host.** connect's relay is 75–85% of busy CPU, at about 8 cores per Gbps, and kernel networking is 15–21 cores per host. Further CPU savings of any size are in connect's relay and the kernel's per-packet work, not in the front.

## 5. Findings

**Performance**
- **P1. Host CPU per Gbps is 17% lower.** nginx's ~11 cores per host are gone (§3.1, §3.2).
- **P2. The front is not a CPU problem today.** It is within noise of connect's per-Gbps CPU, but it allocates 15–25% more per Gbps (§4).
- **P3. GC is infrequent and probably cheap,** but unmeasured. There is no GC CPU class in Mimir, and the 36–79 GiB per-block heaps make each cycle large (§3.3).
- **P4. Every edge runs up to 100 connect Ps** (5 blocks at GOMAXPROCS 20) with no CPU quota, plus the other services' Ps. That's more than edge-1's, edge-3's and edge-4's 72 CPUs each, and about edge-0's 96. Pressure is low (§3.3), but at peaks the blocks compete with one another and with the kernel's softirq time. No scheduler latency is measured.
- **P5. Refused requests are 14% of front requests** (§3.6). Each costs a request parse and often a handshake, and refused clients retry.
- **P6. Failed handshakes dominate QUIC:** 210 a second against 24 completed. On TCP, 418–463 handshakes a second fail, all but 0.2% because the client closed (60%) or timed out (40%) mid-handshake. That costs little CPU (§3.7).
- **P7. connect sessions churn:** 920 a second fleet-wide, with a median life of 4–6 s, about as on 11-19 (§3.8). Every new session pays a handshake and connect's session setup. That setup, not TLS, is what the reconnect waves cost: edge-3 and edge-4 reached 89–93% CPU (LBALT.md §11.8).
- **P8. Nearly idle limits:** the body budget and spill caps are nearly unused (§3.5). UDP drops are negligible, though `rmem_max` (1 MiB) is below what quic-go asks for (§3.9).

**Correctness and operations**
- **C1. Rollbacks without an alert.** A stale controller's run-lb-unit.sh rolled edge-0 back twice (LBALT.md §11.8). Nothing alerted, because monitor.yml's `lbalt_status` isn't enrolled (LBALT.md §11.6), and the second rollback was found 5 hours later by reading metrics.
- **C2. Most alt-path QUIC handshakes fail.** They have done so since the `alt.*` move at 06:32Z (§3.7): 443 completes about 20% of handshakes and whodis 1–2%. The failures carry no reason: `frontQuicRun.serveConnect` and `serveApi` count `handshake_failed` without the error from `AwaitQuicHandshake`. So an alt-named SNI (no certificate on QUIC), a timeout, a TLS alert and a version mismatch all look the same. The TLS failures over TCP do carry a reason since the 15:42Z hotfix (§3.7). This may be the h3 failure users report.
- **C3. A misfiled body failure.** A client gone mid-body counts as `incomplete` (400) when its read fails with a reset: `nginx/body.go:bodyReadRefusal` treats only a canceled context or a timeout differently. The client never sees the 400, so `canceled` undercounts clients that went away. This is metric accuracy only.
- **C4. A connect deploy is now a front deploy.** It restarts every public listener on the host, api's h2 connections included, so every connect deploy is a reconnect wave for all clients, not only connect's. D12's drain softens it, and the waves' cost is P7's. The 15:36–15:42Z hotfix shows the cost:
  - fleet clients fell from 261.4k at 15:30Z to 231.9k at 15:46Z and recovered by about 15:56Z;
  - one-minute busy CPU peaked at 90.2% on edge-4 (15:49Z, against 74.2% before), 85.9% on edge-1, 81.5% on edge-3 and 71.7% on edge-0;
  - refusals peaked at 3,130 a second (three-minute rate, 15:44Z) and stayed 25–30% above their pre-deploy rate through 16:24Z.
- **C5. Observability gaps:**
  - no production profiling;
  - no api latency histogram;
  - no count of completed handshakes, resumed or full;
  - no route zone on refusals;
  - no upstream dial count, so keepalive reuse can't be read;
  - no GC CPU or scheduler latency.

## 6. Recommendations

Ranked by impact over effort, with risk.

| # | Recommendation | Impact | Effort | Risk | How to measure |
|---|---|---|---|---|---|
| R1 | An opt-in pprof listener on loopback per connect process, off by default, enabled by a setting and bound to 127.0.0.1 on a block-specific port; or a signal that writes a CPU and heap profile to a file in the container. | High: every CPU decision below needs it | S | L: loopback only, no new public surface | A 30 s CPU profile and a heap profile from one block at peak; attribute §4's allocation increase and connect's 8 cores per Gbps. |
| R2 | Rollout safety. run-lb-unit.sh, run-warp-units.sh and run-edges.sh refuse to install when the controller's warp or vault checkout is behind origin's main, or when a rendered lb unit drops lb_alt flags the installed one has. Enroll `lbalt_status` (LBALT.md §11.6) so an interface leaving lb_alt alerts in minutes. | High: correctness | S–M | L | Re-run §11.8's stray case in a test: the stale render is refused. In production, alert latency for an interface leaving lb_alt. |
| R3 | Refusal zones, then limits. Add a bounded `zone` label (the route zone, or the interface's default) to `urnetwork_front_limit_refusals_total` (`nginx/route_http.go:serveLocation`, `nginx/limit.go:frontLimits.acquireRequest`). Then decide whether connect upgrades from shared addresses deserve a higher or identity-keyed limit, applied after auth, where the client is known. | Medium to high: 14% of requests and client experience | M | M: abuse controls | Refusals by zone; the refused share of connect upgrades; `client_canceled` and session churn per active session. |
| R4 | Handshake accounting. Count completed TLS handshakes by full or resumed (`tls.ConnectionState.DidResume`), version and ALPN, with a duration histogram. Give QUIC `handshake_failed` a bounded reason from `AwaitQuicHandshake`'s error: idle timeout, handshake timeout, crypto alert, version, canceled. The TLS failure reasons went out at 15:42Z (§3.7). **Implemented, 2026-11-20, not deployed, without the histogram:** `urnetwork_connect_front_tls_handshakes_total` by `version`, `resumed` and `alpn`, counted once net/http's handshake returns, not in VerifyConnection, which a TLS 1.3 server runs before the client's Finished; `urnetwork_connect_front_quic_handshakes_total` by `port`, `version` (v1, v2) and `resumed`; and a `reason` on `urnetwork_connect_front_quic_connections_total` (idle_timeout, handshake_timeout, certificate, no_alpn, unknown_name, version, peer_closed, canceled, other; `-` for the other results) (LBALT.md §11.2). | Medium | S | L | Full against resumed handshakes a second per host; the reasons of the 463 TLS and 210 QUIC failures a second. |
| R5 | Runtime metrics. Export runtime/metrics `/cpu/classes/gc/total:cpu-seconds`, `/cpu/classes/total:cpu-seconds` and `/sched/latencies:seconds` from connect. Only then tune: raise GOGC to 200 with a GOMEMLIMIT backstop if GC exceeds about 5% of connect's CPU (the hosts have 450–700 GiB available), and consider GOMAXPROCS 16 on the 72-CPU edges (edge-1, 3 and 4; edge-0 has 96) if scheduler p99 rises at peak. | Medium | S | L | GC share of connect CPU at peak; scheduler latency p99 by block. |
| R6 | Per-request trims, once R1 confirms them. `routeHttpStatusClass` formats its label with fmt.Sprintf on every request; use constants. `Front.observe` resolves two metric children per request; cache them per service, protocol, status class and block. Pool `forwardRequestHead`'s head buffer. | Low | S | L | Allocations per front request, from R1's heap profile and `go_memstats_mallocs_total`. |
| R7 | Reconnect waves. Measure a connect deploy's wave with R4's counters, and space block restarts (warpctl's `--percent` rounds) by the measured CPU. Separately, decide whether the front should outlive connect's deploys (C4): a design question, not a fix. | Medium | M–L | M | Host CPU peak and handshakes a second during a deploy; the client count dip. |
| R8 | `net.core.rmem_max` and `wmem_max` at 7.5 MiB through xops, so quic-go gets its desired UDP buffers. | Low | S | L | UDP socket drops per host (§3.9). |
| R9 | Count a reset mid-body as `canceled`, not `incomplete` (C3). | Low | S | L | The two reasons' rates. |
| R10 | Restore an api latency histogram with bounded route labels, so forwarding overhead is the front's header time minus api's own. Done 2026-11-20: api's `urnetwork_http_request_duration_seconds` is a 10-bound histogram again; its mean was always available from the summary's `_sum` and `_count` (api's own handling 11.2 ms against the front's 13.6 ms header time at 16:50Z). | Medium: observability | S | L | Front against api p99 per route. |
| R11 | Add an upstream pool counter of dials and reuses per service (`nginx/upstream.go`). | Low | S | L | Reuse ratio; dials a second. |
| R12 | The alt path's QUIC: before anything else in R4, give `handshake_failed` its reason and count failures by SNI class (api, connect, alt, unknown, none). If the failing clients present alt names, serve the `alt.*` names on QUIC as api names, with a certificate and api routing, as alt did. Otherwise follow the reason that dominates. **The counting is implemented, 2026-11-20, not deployed:** the reasons (R4), and `urnetwork_front_quic_client_hellos_total`, every QUIC ClientHello by `port`, `name_class` (connect, api, lb, forwarded, redirect; alt, unknown, none) and `h3_offered`. An alt, unknown or missing name fails before quic-go accepts the connection, so §3.7's `handshake_failed` rates are all served names', and alt-named clients' failures were never counted: the ClientHello counts decide the `alt.*` question. | High: client connectivity | S–M | L | whodis and 443 handshake success rates against the 1–2% and 20% of §3.7. |

Not recommended now:
- **Shrinking the body budget or spill caps:** reserving is accounting, and nothing is preallocated.
- **Shared ticket keys across blocks:** D6c, and a full handshake costs little at these rates (§4).
- **Kernel tuning beyond R8:** there is no measured loss.

## 7. Open questions

- What makes up the 15–25% extra allocation per Gbps? (R1)
- Why do most QUIC handshakes on the alt path fail since the 06:32Z `alt.*` move: alt-named SNIs from older builds, or something in the whodis translation or the QUIC parameters? (R12)
- Why do edge-3 and edge-4 see 3–5 times the TLS handshake timeouts of edge-0 and edge-1 (62–71 a second against 14–20): a different client mix on their addresses, or scanners?
- Who is refused: real users behind shared addresses, or abusive sources? (R3)
- Is a connect deploy's reconnect wave acceptable at peak now that it carries api's connections too? (R7)
- Are GC or scheduling a measurable share of connect's CPU at peak? (R5)

## Appendix A. Queries

All are instant queries over the window's range at its end time, against Mimir's Prometheus API.

- **Busy cores:** `sum by (host) (rate(node_cpu_seconds_total{host=~"by-us-fmt-5-edge-[0134]", mode!~"idle|iowait"}[W]))`
- **Cores by mode:** the same, `sum by (host, mode)`.
- **Go services' CPU:** `sum by (host, service) (rate(process_cpu_seconds_total{host=~"by-us-fmt-5-edge-[0134]"}[W]))`
- **NIC Gbps:** `sum by (host) (rate(node_network_receive_bytes_total{device=~"en.*"}[W]) + rate(node_network_transmit_bytes_total{device=~"en.*"}[W])) * 8 / 1e9`
- **Loopback Gbps:** `node_network_receive_bytes_total{device="lo"}`, as above.
- **connect runtime:**
  - `go_memstats_alloc_bytes_total`, `go_memstats_mallocs_total`, `go_gc_duration_seconds_count` (rates);
  - `go_memstats_heap_inuse_bytes`, `go_goroutines`, `process_resident_memory_bytes` (averages);
  - `go_gc_gogc_percent`, `go_gc_gomemlimit_bytes`, `go_sched_gomaxprocs_threads`;
  - all with `{service="connect"}`.
- **Request mix:** `sum by (upstream_service, protocol) (rate(urnetwork_front_requests_total[20m]))`, and `by (upstream_service, status_class)`.
- **Header-time percentile:** `histogram_quantile(q, sum by (upstream_service, le) (rate(urnetwork_front_response_header_seconds_bucket{status_class!~"1xx|none"}[20m])))`
- **Forwarder:** `urnetwork_lbalt_upstream_attempts_total` by `upstream_service, result`; `urnetwork_lbalt_upstream_retries_total` by `reason`; `urnetwork_lbalt_forward_errors_total`; `urnetwork_lbalt_forward_body_refusals_total` by `reason` (rates). Also `urnetwork_lbalt_forward_tunnels`, and `max_over_time(max(urnetwork_lbalt_body_{memory,spill}_bytes)[9h:1m])`, with `urnetwork_lbalt_body_spill_files` likewise.
- **Limits, QUIC and TLS:**
  - `sum by (limit) (rate(urnetwork_front_limit_refusals_total[20m]))`;
  - `sum by (port, result) (rate(urnetwork_connect_front_quic_connections_total[20m]))`;
  - `sum by (port, kind) (rate(urnetwork_connect_front_http_server_errors_total[20m]))`;
  - `urnetwork_connect_front_sockets_up`, `_socket_restarts_total` and `_socket_bind_failures_total`.
- **Sessions:** `urnetwork_connect_h1_handshake_results_total` by `result`, `urnetwork_connect_h1_sessions_closed_total`, `urnetwork_connect_h1_sessions_active`, and `urnetwork_connect_h1_session_duration_seconds` (sum over count; `histogram_quantile(0.5, …)`).
- **Kernel:** `node_nf_conntrack_entries`, `node_nf_conntrack_entries_limit`, `node_network_receive_drop_total{device=~"en.*"}`.

## Appendix B. Edge reads

Run as an unprivileged user, with no sudo:
- for each `bringyour-connect` process, the cgroup v2 directory named in `/proc/<pid>/cgroup`: `cpu.max`, `cpu.stat`, `cpu.pressure`, `memory.current`, `memory.stat`, `memory.pressure`;
- `/proc/net/softnet_stat`, summed over CPUs;
- the drops column of `/proc/net/udp` and `/proc/net/udp6`;
- `ss -s`;
- `sysctl` for `net.core.rmem_max`, `wmem_max`, `netdev_max_backlog`, and the conntrack timeouts.
