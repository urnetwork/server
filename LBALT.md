# LBALT: connect fronts the edge interfaces (`lb_alt`)

Status: designed 2026-11-18; implemented on branch `lbalt/integrate` (server a297487df, warp bdacae4), 2026-11-19, and not merged to main or deployed. §11 holds the implementation notes for code review.

Questions D1–D22 in §6 have owner decisions (2026-11-18). Each keeps its options for the record.

The §7 measurements were taken 2026-11-18 (§7.1). The owner confirmed the three values they sized (2026-11-18):
- `cores: 20` on every connect block (D15);
- a 256 MiB body-buffer budget per connect process (D7);
- N = 4 sockets per port in each block (D10).

D6c was revised on 2026-11-18 to Go's default per-process session tickets.

M7's HMARK test passed on edge-4 on 2026-11-18 (§7.1). It shows that HMARK selects buckets; the end-to-end return path is still to be tested (§4.7).

**Review** ([LBALT-REVIEW.md](LBALT-REVIEW.md), 2026-11-18, of this doc at 2c9e30d7f).
- Proceed with the front and the nginx comparison harness (§8 steps 1–2).
- Hold the first public cutover for Gate A (§8 step 4).
- Hold the `alt.*` move for Gate B (§8 step 6).
- Its findings are folded into §3, §4 (new §4.6 and §4.7), the decisions they qualify, and §8–§9. Each is marked "review".
- It raised three questions, D20–D22 (§6 G), which the owner decided on 2026-11-18.

Terms used here:
- **interface**: an lb interface, meaning one public address on an edge host.
- **fronted names**: the hostnames the lb serves on that host today.
- **front**: the lb_alt code inside connect.
- **block**: a warp block of a service.

## 1. Goal

Turn on `lb_alt: true` for connect, remove the alt service, and connect users directly to connect.

With `lb_alt` on an edge interface:

- **Connect blocks own the public ports.** These are TCP 80 and 443, UDP 443 and 4053, and UDP 53 (aliased to 4053). No nginx process runs for that interface.
- **Connect names stay in-process on every protocol.** Connect serves H1 websocket on TCP 443, h3 on UDP 443, and whodis on UDP 4053 and 53 itself.
- **Every other fronted name is forwarded.** Connect terminates TLS (and QUIC for the api), then forwards plaintext HTTP/1.1 over TCP to that service's blocks. It uses the same request contract nginx uses today.
- **The lb unit keeps running for the interface.** It behaves like a transparent interface's unit today: it reconciles the route table, ip rules and public DNAT rules, and deploys no container.

Alt's clients dial the `alt.*` names, which resolve to the proxy hosts today. After the change those names resolve to the edge interfaces, which serve everything alt served. Alt is then removed from fireside and crisp.

## 2. Why

- **Latency.** Today connect's UDP traffic goes through nginx's stream proxy. That adds three things:
  - a userspace copy;
  - a per-source pseudo-session with a 30 s idle timeout;
  - a PROXY v2 header that connect then strips.

  Connect is the most latency-sensitive path, and a DNAT straight to the connect socket stays in the kernel. H1 websocket also loses nginx's TLS hop.

  Measured 2026-11-18 (§7.1): the UDP hop's kernel-level loss is under 0.001%. About 96% of connect's clients are on H1 over TCP, where nginx terminates TLS and copies every byte. Most of the win is therefore on the H1 path, which the single cutover (D1) moves together with UDP.
- **Memory.** alt holds 6.4–6.9 GiB per proxy host (CPUOPT4.md:1274, CPUOPT6.md:1582-1585, 2021). fireside's minimum available memory is 9.0 GB, and the proxies there were OOM-killed on 10-23 and 10-25. Alt does not fit as designed.
- **One fewer service.** Alt duplicated connect, since it was a full connect node, and added the api router on top. The edges already run both.
- **Independent deploys** (review). Forwarding api requests to the existing api block ports keeps api deploys independent of connect, unlike alt's embedded api router.

## 3. Today

### 3.1 Edge lb (nginx)

**Hosts and networking**
- There are 10 lb interfaces on the four services hosts: edge-0 ×3, edge-1 ×3, edge-3 ×2, edge-4 ×2 (`../vault/main/services.yml` v26, `lb.interfaces`). Each runs its own nginx lb block.
- Every container uses host networking (`--host_networking=yes`). Each lb binds its interface docker network's bridge address, and the public address is DNATed to it. Replies leave through the interface's route table, picked by `ip rule from <interface docker subnet>` (`../warp/warpctl/run.go:26-45`).
- Each services host runs five connect blocks (beta 1, g1 24, g2 25, g3 25, g4 25). Each has `cores: 16`, which sets GOMAXPROCS 16, and binds the `warpservices` bridge address.
- The ten interfaces have ten public IPv4 addresses and eight public IPv6 addresses. Edge-0 `eno3np2` and edge-1 `eno4` have IPv4 only (services.yml, `lb.interfaces`; review).

**TLS and upstreams**
- TLS ends only in `http{}` server blocks. Each hostname proxies HTTP/1.1 to `service-block-<svc>`.
- The upstream servers are `warpservices:<block external port>`, weighted by block weight. The external port is stable; warpctl DNATs it to the block's current container.
- Balancing is `least_conn`, or `hash $remote_addr consistent` for stateful services (mcp). That is nginx's weighted Ketama ring.

**Request headers** (`addProxyRequestHeaders`, `../warp/warpctl/config.go:2504-2522`)
- `X-UR-Forwarded-For` is overwritten with `addr:port`, IPv6 bracketed (config.go:1390-1395).
- `X-Forwarded-For` and `X-Forwarded-Source-Port` are cleared.
- `Host` is set to nginx's normalized `$host`, which carries no port. `Early-Data` is set from `$ssl_early_data`, which is always empty, because nginx sets `ssl_early_data off` (config.go:1336).
- `X-UR-Request-Buffering` is `off` on streamed routes and cleared elsewhere.
- Websocket services (connect, gossip, grafana) get `Upgrade` and `Connection: upgrade`, 60 s read and send timeouts, and no buffering (config.go:2526-2539).

**Retries**
- `proxy_next_upstream error timeout http_503 non_idempotent; proxy_next_upstream_tries 2` (config.go:1358-1359).
- Retries work because request bodies are buffered: `proxy_request_buffering on`, `client_max_body_size 16m`, 64 KiB in memory and the rest on disk.
- Corrected by the review: a draining api never answers 503 (api/run.go:144, :251; http_drain.go:212-226).
  - On SIGTERM it marks its status as draining.
  - For 10 s it keeps accepting and serving, with `Connection: close` on HTTP/1 responses.
  - Then a 60 s `Shutdown` closes the listener.

  The ordinary retry case is therefore a stale pooled connection or a refused dial.
- 502 is deliberately not retried: a retried application 502 once ran a password reset twice (../warp/warpctl/config_retry_test.go). `non_idempotent` with `timeout` can still resend a slow POST to a sibling block.

**Limits**
- Per client address: `limit_req` 450 r/min with burst 150 nodelay, and `limit_conn` 50. Both answer 429.
- Per-route zones replace the shared zone for api `/connect/control` (12000/min, burst 6000) and for the grafana and loki query and push paths. `exclude_subnets` are exempt.
- Timeouts: header and body 15 s (60 s on `/sn/attempt-artifact`), keepalive 1 m, proxy connect/read/send 30 s, websocket and streamed routes 60 s.

**Responses**
- nginx adds CORS (`cors_origins_from`), HSTS, X-Frame-Options, nosniff and XSS headers.
- `redirect_aliases` return 301s.
- `/status` is denied on service names.
- TCP 80 returns 301 to https.
- The lb names `<env>-lb.<domain>` serve status and the hidden-prefix per-block routes.
- ALPN on TCP 443 is nginx's default: h2 and http/1.1.

**UDP**
- Config: `udp_stream_port_services {443: connect, 4053: connect}` and `udp_forward_ports {53: 4053}`.
- Each stream server sets `proxy_protocol v2; proxy_timeout 30s; proxy_requests 0`, with weighted `least_conn` across the connect blocks (config.go:2734-2845).
- There are no TCP stream ports.
- Because UDP 443 is a connect stream, nginx serves no h3 and sends no Alt-Svc (`hasUdp443Stream`, config.go:990).
- **4053 is private on nginx interfaces** (review).
  - warpctl deliberately keeps the target of `53 → 4053` private (`publicPortServiceTargets`, ../warp/warpctl/run.go:3210).
  - The VyOS generator leaves 4053 out for plain lb interfaces (vyos.go:346), and opens it for transparent alt interfaces. It generates rules only for interfaces with a `router:` entry: edge-3, edge-4, fireside and crisp. Edge-0 and edge-1 have none, so their router rules are kept outside the generator.
  - An alt host publishes both 53 and 4053.
  - Shipped clients (since v2026.9.13) try 4053 as a second whodis port when their alt URL names no port (../connect/net_alt.go:173). So a working socket and a successful UDP 53 probe can coexist with a broken client fallback.

**Certificates and health**
- `findLatestTls` (config.go) takes the highest semver under `vault/{env,all}/tls/<ver>/<name>/`, then `star.<parent>`, then the parent.
- Certificates are baked into the lb image, so rotating one means a rebuild and redeploy.
- Health checking: none (`max_fails=0`). Gating is warpctl's cutover after `/status`.

### 3.2 Transparent interfaces and host-pinned public ports

- With `lb.interfaces.<host>.<iface>.transparent: true` (fireside, crisp), the lb unit deploys nothing. It reconciles routing every 30 s.
- A block can own public UDP ports through `external_udp_ports` and `external_udp_forward_ports`. warpctl DNATs the interface address and public port to the block's allocated port, and leaves the TCP side of the same port number free for an lb (run.go:3057-3110, `externalUdpPortServiceTargets`).
- That support has three limits that matter here:
  - it requires a transparent interface (config.go:2997-3006 panics otherwise);
  - it is UDP only, with no TCP aliases;
  - each (host, port) has a single owner, which relies on alt having one block (`../warp/services/load.go:277-284`).
- Deploy sequence:
  - the new container takes a free pool port;
  - warpctl polls `/status`, switches the DNAT and cleans dead UDP conntrack entries;
  - the old container drains for up to 60 m while conntrack keeps its flows.

### 3.3 Alt

Design: `../connect/EXTENDER.md` §L.

- **Listeners.** QUIC on UDP 443, and whodis (decode53, `ur.xyz.`) on 4053 with 53 aliased to 4053. Both address families; one block per proxy host.
- **Dispatch by SNI** (alt/alt.go):
  - connect names go to `HandleQuicConn`;
  - api names go to an in-process http3 api router;
  - anything else is closed with 0x1000.

  `*.x` names match exactly one label (alt/hosts.go).
- **Limits (L5)** come from `default_rate_limit` and the api's `route_rate_limits` (alt/rate_limit.go):
  - the api front gets a token bucket and a per-address concurrency cap;
  - connect gets a per-address connection cap, refused with 0x1001.
- **Headers.** Forwarding headers come from `router.SetIngressHeaders` (alt/ingress.go). No PROXY protocol.
- **Shutdown** (review). Alt closes HTTP/3 with a bounded close (alt/alt.go:510-519). A graceful shutdown started while the QUIC transport was being destroyed once produced an exit burst of at least 700k goroutines, one GOAWAY writer per pass.
- **Readiness** (review). Alt records one readiness boolean per transport and address (alt/alt.go:116-128).

### 3.4 Connect listeners

- **H1 websocket** is served over HTTP on `WARP_HOST_IPV4` only.
- **QUIC** h3 (443) and h3dns (4053) bind IPv4 only, with `reusePort := false` (connect/transport.go:2357-2365). They wrap the socket in the PROXY reader when `EnableProxyProtocol` is set, which is the default.
- **The PROXY reader trusts the first header from each new source address**, with no peer check (connect/transport_pp.go).
- **H1 takes the client address from `X-UR-Forwarded-For`** with no trusted-peer check, and logs at error level on a repeated or malformed value (session/client_session.go:134-180). Safety relies on block ports being unreachable from clients.
- **QUIC TLS** comes from tls.go and is gated by `tls.yml allowed_hosts`, which lists only ur.network and bringyour.com names (tls.go:58-72). There is no reload.
- **Readiness.** `/status` returns `X-UR-Connect-Listeners-Ready: 1` and `X-UR-Connect-UDP-Listeners` (connect/connect_router.go:150-170). An lb with UDP streams polls every connect block for these before its cutover.
- **Carriers** (review).
  - The H1 path also supports the `urnetwork-framer/1` upgrade, H1+ (connect/transport.go:1554).
  - On UDP, connect is raw QUIC and may offer no ALPN, while api requests use `h3` (alt/alt.go:356).
  - The QUIC config alt inherits sets `Allow0RTT: true` (connect/transport.go:858).
- **Private paths** (review). warpctl's candidate status poll uses the private HTTP port (../warp/warpctl/run.go:2315), and the exchange has its own internal sockets (connect/resident.go:1515-1525).
- **Shutdown** (review). Connect sets its status to draining, calls `exchange.Drain()`, and then cancels its serving context (connect/run.go:208-212).

### 3.5 How clients reach alt

- In `../connect/net_alt.go`, a client derives `alt`, `alt-v4` and `alt-v6` under its platform or api url's domain. It presents the connect or api SNI, never an alt name, and dials UDP 443 for h3, then UDP 53 and 4053 for whodis.
- The `alt.*` names are static A and AAAA records of the proxy hosts (`dns_aliases` on the alt service).
- As of 11-02, alt served about 20% of extender activation posts.

## 4. Proposed design

This section assembles the decisions recorded in §6.

### 4.1 What answers each public port

| Public port | Name | Handled by |
|---|---|---|
| TCP 443 | connect names | connect, in-process H1 websocket; TLS terminated in connect |
| TCP 443 | any other fronted name | TLS terminated in connect; HTTP/1.1 to that service's blocks |
| TCP 443 | no SNI, or unknown | the lb name's default server, as nginx's first server block does today |
| TCP 80 | any | 301 to https; lb status routes on lb names, including Route53's health check (D17); denied otherwise |
| UDP 443 (h3) | connect names | connect, in-process |
| UDP 443 (h3) | api names | h3 terminated in connect; HTTP/1.1 to api blocks |
| UDP 443 (h3) | anything else | refused with application error 0x1000, as alt does |
| UDP 4053, UDP 53 → 4053 | as UDP 443 | the same dispatch under decode53, as alt does |

No name gets an Alt-Svc header, so browsers keep using TCP for web, mmm and app.

**Public port list** (review).
- lb_alt interfaces get one explicit public port list: TCP 80 and 443, and UDP 443, 53 and 4053.
- Both the host rules and the VyOS generator consume that list. The generator covers edge-3 and edge-4. Edge-0 and edge-1 have no `router:` entry (§3.1), so their 4053 rules are set and tested outside it.
- Unlike on an nginx interface (§3.1), 4053 is published at the host and at the router.
- Tests run a whodis exchange on both 53 and 4053 and check the public reply address and port, in each address family. nginx interfaces keep their legacy test that the forward target stays private.

### 4.2 Forwarding

- **Request contract.** The forwarder copies nginx's (§3.1):
  - it overwrites `X-UR-Forwarded-For` from the accepted socket;
  - it removes client-sent `X-Forwarded-For`, `X-Forwarded-Source-Port`, `X-UR-Request-Buffering` and `Early-Data`. The shared header helper leaves `Early-Data` in place today (router/ingress.go:93-110), so the front applies the rewrite once, with it removed (review). No server code reads `Early-Data` today, so removing it is defense in depth;
  - it sets `Host` to the normalized authority with no port, as nginx's `$host` does. A Go proxy that copies `Request.Host` can keep a port (review);
  - it handles websocket upgrade and streamed bodies per service.
- **Routing after TLS** (review; D9).
  - SNI picks the certificate. The request's authority picks the route: `Host` on h1, and each stream's own authority on h2 and h3.
  - Names match with nginx's server-name semantics. An nginx wildcard can match several labels, while alt's matcher accepts one (alt/hosts.go:118-120).
  - The pinned nginx image settles routing and default-host behavior in the parity harness (§8 step 2).
- **Upstreams** are the same stable `warpservices:<block external port>` targets nginx uses, with:
  - the same block weights;
  - `least_conn`, or, for stateful services, nginx's weighted Ketama ring on the client address. The ring is reproduced exactly, so a mixed rollout doesn't remap clients (review);
  - retries as D7 specifies: at most two attempts, on distinct eligible blocks, before any response reaches the client;
  - keepalive pools per upstream block.
- **Limits and response headers** match nginx (§3.1). The token bucket and per-address caps come from alt/rate_limit.go.
- **No stream forwarding (decided, D4).** The front forwards only HTTP. There is no raw TCP or UDP forwarding and no PROXY protocol; connect's own UDP ports are served in-process.

### 4.3 Networking (warpctl)

- **The lb unit of an lb_alt interface is routing-only**, like a transparent unit: route table, ip rules, MASQUERADE, and the public DNAT split for TCP and UDP. No nginx runs for that interface (D1). Unlike a transparent interface, an lb_alt interface stays in DNS and in the edge inventory (§4.6).
- **Each connect block binds every lb interface network's bridge address on its host**, IPv4 and IPv6, at its allocated ports. Replies leave with that network's source address, and the existing ip rules route them (D11).
- **Public flows are split across the host's connect blocks by a hash of the source address**, weighted by block weight.
  - Each block owns a chain that holds only the DNAT to its current port. The block's own deploy switches that chain.
  - Conntrack keeps existing flows on the draining container, as it does today (D10).
- **SO_REUSEPORT is used only within a block.** Each block opens several sockets per address and port, for both UDP and TCP, for receive parallelism. Blocks never share a reuseport group (D10).
- **A block's DNAT switch waits for readiness of the same generation.** Every public listener on every interface address must be up first, with all of its sockets counted (D13, §4.7).

### 4.4 Security requirements

These are not open questions; each must hold before connect is exposed directly.

1. **The PROXY reader is off on every interface-bound socket.** It trusts the first header from any new source, so leaving it on would let any client claim any address.
   - The trusted listeners on the `warpservices` address keep it. nginx streams to them with PROXY v2 for as long as any interface on the host is on nginx, and again after a rollback (§4.6).
   - Corrected by the review: they stay for the blocks' whole lifetime, not only until a cutover's old flows drain.
   - The public front binds only its declared interface addresses. The host and router rules keep client traffic off the trusted ports.
2. **Client-sent ingress headers are removed on interface-bound listeners.** This covers `X-UR-Forwarded-For`, `X-UR-Request-Buffering`, `X-Forwarded-For`, `X-Forwarded-Source-Port` and `Early-Data`.
   - They are removed before in-process handling and before forwarding. Connect's own H1 path trusts `X-UR-Forwarded-For` and logs malformed values at error level.
   - Public HTTP ingress headers are derived from the accepted peer.
   - The shared helper leaves `Early-Data` in place today, which the front fixes (§4.2).
3. **`/status` stays unreachable on public service names.** nginx denies it today and alt returns 404.
4. **Alt's ingress hardening applies to connect's public listeners.** That means:
   - one handshake admission limit, shared across all of a block's sockets;
   - the 0x1000 and 0x1001 refusals;
   - the fix behind alt's rollback floor at 1088438700 (older builds die to a small unauthenticated UDP burst).
     - That fix is connect `6203a4e8` (../connect/EXTENDER-ACTIVATION.md:390). Before it, a lost listener ended the alt process.
     - The fix keeps the listener up: packet translation survives a datagram it can't write (transport_pt_server_failure_test.go).
     - The extender's carriers likewise each recover on their own.
     - D13 and D22 carry the requirement into the front: losing a socket must not end the process.
   - early data refused in the public QUIC config. The config alt inherits sets `Allow0RTT: true` (§3.4), so D9's "early data off" must be set explicitly there (review).
5. **Connect loads the private key of every fronted name.** Connect already mounts the vault read-only, so it can already read those keys; what's new is that the process holds them in memory.

### 4.5 Code layout

Decided (owner, 2026-11-18): a new package, `server/nginx`, holds the nginx compatibility code. It is the part the parity harness (§8 step 2) checks against the lb image. It contains:
- the route table model, and the loader for the file warpctl renders (D5);
- the TLS setup for fronted names: certificate lookup by nginx's `findLatestTls` rule (D6a), with Go's default per-process session tickets (D6c);
- the HTTP/1.1 forwarder with nginx's request contract (§4.2): header rewrites, websocket upgrade, streamed bodies, keepalive pools, `least_conn` and the consistent hash, one retry over buffered bodies under the byte budget (D7), with each body past its first 64 KiB in a temp file (D19);
- the limits: `limit_req` and `limit_conn` semantics and route zones, moved from alt/rate_limit.go (D8);
- response behavior: CORS, HSTS and the other added headers, redirect aliases, the `/status` deny, port 80, the default server, and the lb routes (D9).

Connect keeps:
- the listeners: per-interface, dual-stack, SO_REUSEPORT within the block (D10, D11);
- the dispatch by SNI, which sends connect names to in-process handling and other names to `server/nginx`; the QUIC and whodis dispatch moves from alt/alt.go, and the name matching from alt/hosts.go;
- drain and readiness (D12, D13).

alt/ingress.go's header handling folds into the `server/nginx` request contract.

The api router is not embedded. Api h3 requests are forwarded to api blocks through `server/nginx`, so the front carries none of alt's api memory.

After §8 step 8, the alt service, its binary and its config are deleted.

### 4.6 Handing an interface between nginx and connect (review)

- D1 moves new TCP and UDP flows together, while conntrack keeps delivering existing flows to their original owner.
- Meanwhile nginx on a neighbouring interface keeps creating new plaintext H1 and PROXY-v2 UDP flows to the same five connect blocks. It does so for as long as any interface on the host is on nginx, and again after any rollback (D18).
- So:
  - Compatible connect blocks start on generation-specific ports while nginx still owns ingress.
  - The trusted services-bridge listeners stay, for every nginx dependency, for the blocks' whole lifetime: H1, and UDP with the PROXY reader.
  - The private status port (the readiness poll and the hidden per-block routes) and the exchange's own internal sockets also stay for their whole lifetime. Removing any of them when the first interface drains would break the neighbouring interface and a later rollback.
- The public front:
  - binds only its declared interface addresses;
  - turns PROXY decoding off on public UDP;
  - derives public HTTP ingress headers from the accepted peer.

  The host and router rules must keep client traffic off the trusted ports.
- Switching an interface:
  1. probe the new owner's private status (§4.7);
  2. switch new flows;
  3. keep the old owner alive through its drain.

  Rollback runs the same steps in reverse.
- **lb_alt interfaces stay in DNS and in the edge (mover) inventory.** The existing `transparent` flag takes an interface out of the weighted lb set (../warp/warpctl/dns.go:202, services_lb_inventory.go:88), so lb_alt cannot reuse it. The lb unit is routing-only like a transparent unit, but the interface keeps its DNS weight and its health check.

### 4.7 Generations, readiness and chains (review)

- **Ownership.**
  - Today's port allocator checks one bridge address per family, plus the wildcard and loopback addresses. It records occupancy by port number alone, lumping TCP, UDP and both families together (../warp/warpctl/run.go:1701-1771). UDP conntrack cleanup uses that occupancy and the selected bridge's reply address (run.go:1906-1960).
  - With several bridges and SO_REUSEPORT, a successful bind no longer proves that a generation owns a port. A stale port, or a cleanup that runs early, can send a repeat dial to a dead target or interrupt a live drain.
  - So the allocator records (bridge, protocol, port) per generation, and cleanup touches only the tuples of a retired generation.
- **Readiness.** warpctl probes the candidate on its private allocated port and requires:
  - the generation;
  - the route-file version or digest;
  - an explicit ready state. Today warpctl's status check fails only on an `error` prefix (../warp/warpctl/docker.go:679), so a block reporting `draining` (router/warp_handlers.go:92) passes it. Draining must never count as ready;
  - the full expected listener set: four sockets for each required address, protocol and port.

  Alt records one readiness boolean per transport and address (alt/alt.go:116-128), which would collapse four sockets into one entry, so the front registers each socket on its own. A socket lost after promotion behaves as D22 decides.
- **Chains.**
  - Complete replacement chains are built before an entry jump switches.
  - The design names the writer of each chain: the lb unit writes the HMARK rule and the interface's split chain, and each block writes its own DNAT chain.
  - The hash seed and range ordering are stable.
  - The design says how a restarted worker recovers from an interruption between the IPv4 and IPv6 families.
  - The last working targets stay in place until the replacement is complete.
  - On a loss, only the missing ranges move, to ready blocks by weight.
  - If every target is down, the interface fails its health check, so Route53 withdraws it.
- **Return path.** M7 showed that HMARK selects buckets, using loopback flows and one IPv6 source. The end-to-end return path is tested in a two-interface network namespace (§9): replies leave with the original public source, and routing marks on unrelated traffic are untouched.

## 5. Packet paths

```
today, UDP    client -> public ip:443 -DNAT-> nginx (stream, PROXY v2) -> connect block
today, TCP    client -> public ip:443 -DNAT-> nginx (TLS, HTTP/1.1)  -> connect | api | web | ...

lb_alt, UDP   client -> public ip:443 -DNAT, hash split-> connect block (QUIC)
                                                           `- api SNI -> HTTP/1.1 -> api block
lb_alt, TCP   client -> public ip:443 -DNAT, hash split-> connect block (TLS)
                                                           `- other SNI -> HTTP/1.1 -> service block
```

## 6. Design questions

### A. Scope and config

**D1. Phasing.**
- (a) One cutover: connect takes TCP and UDP together, and nginx stops.
- (b) Two phases:
  - Phase 1: connect owns the public UDP ports and forwards api h3 to api blocks. The `alt.*` names move to the edges and alt is removed. nginx keeps TCP 80 and 443.
  - Phase 2: connect takes TCP and nginx stops.
- Recommendation: (b).
  - Phase 1 delivers the UDP latency win and alt's removal. It forwards only one service (api), whose h3 traffic alt serves today, and it needs no new certificates, because api and connect names are already in tls.go's allowlist.
  - Phase 2 carries the whole nginx-parity surface: certificates for seven domains, CORS and HSTS, redirects, limits, retries, body buffering, websocket and streamed routes, and the lb routes. It can be shadow-tested against nginx before it takes traffic.
  - Splitting one address between an lb on TCP and a block on UDP already exists on transparent interfaces.
- **Decision (owner, 2026-11-18):** (a), one cutover. On each lb_alt interface, connect takes TCP and UDP together and nginx stops. The nginx-parity work (§8 step 2) has to pass before any traffic moves.
- Amended by the review: the first public cutover also waits for Gate A (§8 step 4). In particular, a client must be able to use both whodis ports, and keep an established session through a deploy and through a rollback.

**D2. The setting.**
- (a) `services.connect.lb_alt: true` only, turning on every edge interface at once.
- (b) The service flag plus a per-interface override, `lb.interfaces.<host>.<iface>.lb_alt`, for the rollout.
- Recommendation: (b). The cutover happens per public address, so the canary is one interface. Validation refuses:
  - more than one service with lb_alt;
  - lb_alt on a transparent interface;
  - a host with lb_alt interfaces and no blocks of that service;
  - on an lb_alt interface, any TCP stream mapping, or a UDP stream mapping to a service other than connect (D4).
- **Decision (owner, 2026-11-18):** (b). `services.connect.lb_alt: true` is the end state, and `lb.interfaces.<host>.<iface>.lb_alt` overrides it per interface for the canary and for single-interface rollback. The validation above applies.
- Amended by the review:
  - Tests cover all three override cases: no interface override; `true` under a disabled service default; and `false` under an enabled default, which is the rollback switch.
  - An lb_alt interface stays in DNS and in the edge inventory, unlike a transparent one (§4.6).

**D3. Names and protocols served.**
- The proposal is the §4.1 table: every fronted name on TCP, h3 and whodis for connect and api names only, and no Alt-Svc.
- Alternative: h3 for every name, with Alt-Svc. That moves browser traffic for web, mmm and app onto QUIC through connect.
- Recommendation: §4.1 as written. It matches alt on UDP and nginx on TCP.
- **Decision (owner, 2026-11-18):** §4.1 as written. h3 and whodis serve connect and api names only, everything else on QUIC is refused with 0x1000, and no name gets an Alt-Svc header.
- Amended by the review: on h3, each forwarded request's authority must also be one of the api names that D3 admits. A connection whose SNI is an api name cannot carry a request for another service.

### B. Front and forwarding

**D4. Forwarding protocol, and stream forwarding.**
- Context:
  - With TLS terminated in connect, forwarding is an HTTP/1.1 reverse proxy carrying nginx's header contract, because that is what every upstream parses (`session.ResolveClientAddress`).
  - Stream forwarding (raw TCP or UDP, with PROXY protocol) is used only by connect. Main's lb maps exactly two stream ports, UDP 443 and 4053, both to connect. There are no TCP stream mappings. canary, community and local declare no stream ports at all.
  - Grafana declares TCP and UDP stream ports for its Loki/Mimir ring, but only so warp assigns them per-deploy ports. Its config states that the ring does not go through the lb, which has no mapping for those ports.
- **Decision (owner, 2026-11-18):** the connect lb has no stream forwarding: no raw TCP, no raw UDP, and no PROXY protocol.
  - Connect's UDP 443 and 4053 are served in-process (§4.1).
  - Under lb_alt, validation refuses any TCP stream mapping, and any UDP stream mapping to a service other than connect.

**D5. Where the route table comes from.**
- Connect needs, per host:
  - the fronted names and each name's service;
  - each service's blocks, with external ports and weights;
  - the stateful, websocket and streamable flags;
  - limits and route zones;
  - redirect aliases, CORS origins and the lb names.
- (a) warpctl renders a route file when it starts a connect block, from the same inputs and functions as the nginx config (`NginxConfig.services()`, block infos, port blocks), and mounts it. Connect reads it at start.
- (b) Connect reads services.yml from the vault itself, as alt/hosts.go does for names. Port blocks are computed by warpctl, so connect would have to reimplement the port allocation.
- Recommendation: (a).
  - It gives parity with nginx by construction, and ports are still computed in one place.
  - A route change takes effect on the next connect deploy, just as it takes effect on the next lb image today.
  - Hot reload (SIGHUP, plus a `warpctl` command that rewrites the file) can follow once the table is stable.
- **Decision (owner, 2026-11-18):** (a). warpctl renders the route file when it starts a connect block, from the inputs and functions that build the nginx config, and mounts it. Connect reads it at start. Route changes apply on the next connect deploy. Hot reload is deferred.

**D6. Certificates and session tickets.**
- **Lookup.** Port nginx's `findLatestTls` rule into tls.go, and serve exactly the names in the route table, in place of the `allowed_hosts` gate under lb_alt.
  - Recommendation: yes. The current gate refuses the ur.io, ur.xyz, vpn.dev, lag.dev and ww.dev names.
- **Rotation.**
  - (a) Redeploy, as with nginx today.
  - (b) Rescan the vault every few minutes and swap certificates through `GetCertificate`.
  - Recommendation: (b). A connect redeploy drains each block for up to 60 m, which is far slower than an lb redeploy.
- **Session tickets.** Flows are split across five blocks per host and DNS spreads clients across hosts, so with per-process ticket keys most resumptions become full handshakes.
  - Recommendation: derive ticket keys from a vault secret for each rotation epoch, and install current and previous with `SetSessionTicketKeys`. Every block and host shares them, for both TCP and QUIC. This needs a new vault secret.
- **Decision (owner, 2026-11-18):**
  - Lookup: port nginx's `findLatestTls` rule into the new `server/nginx` package (§4.5). Under lb_alt, serve exactly the route-table names, in place of tls.go's `allowed_hosts` gate.
  - Rotation: (a), redeploy, as with nginx today. Certificates load once at start, and a new certificate takes effect on the next connect deploy. Rotations need lead time for connect's rolling deploys.
  - Session tickets, first decided as shared keys from a new vault secret: superseded below.
- **Session tickets, revised (owner, 2026-11-18):** Go's default per-process ticket keys. There is no vault secret and no `SetSessionTicketKeys`.
  - Each connect process generates random keys in memory, rotates them every 24 h, and accepts them for 7 days. TCP and QUIC both use them.
  - The first recommendation rested on two premises that were wrong.
    - It assumed flows spread across blocks. D10's source-address hash keeps a returning address on one block, so per-process keys resume whenever a client returns to the same host.
    - It didn't check today's nginx, which has `ssl_session_tickets off`. Its only resumption is a 10 m, 10 MB session cache in each interface's nginx (../warp/warpctl/config.go:1292-1294). Per-process keys already do better than that.
  - With early data off (D9), a TLS 1.3 or QUIC resumption takes one round trip, the same as a full handshake. Resumption saves the certificate and signature CPU, under a core per host at peak (§7.1, M2), and only TLS 1.2 clients save a round trip.
  - A static shared secret would add a lasting exposure. If it leaked, recorded TLS 1.2 sessions that received tickets could be decrypted.
- **OCSP stapling.** nginx sets `ssl_stapling on`, but no certificate in `vault/all/tls/2026.10.16` carries an OCSP responder URL: they are Let's Encrypt certificates. Stapling does nothing today, and the front needs none.
- Amended by the review (certificates and config revisions):
  - The route file is immutable and schema-versioned. A block loads it, with complete and matching certificate and key pairs, before it reports ready.
  - Upstream targets named by an older route file stay until the last block using that file has drained.
  - Validation checks SAN coverage for aliases, the parent fallback and wildcard names.
  - Private status reports the config revision and the earliest certificate expiry.
  - Rotation and rollback are rehearsed before the 2027-01-15 expiry.
    - Today the lb image holds only the generated config, whose certificate paths are pinned to the version found at image build (../warp/lb/Dockerfile:107; ../warp/warpctl/config.go:955).
    - Both the certificates and the keys come from the read-only vault mount (../warp/warpctl/run.go:2106-2113).
  - D6c: Go's automatic ticket keys belong to the listener's `tls.Config`. A config returned by `GetConfigForClient` uses the original's keys unless it sets its own (crypto/tls common.go), and the vendored quic-go uses the listener's config without cloning it.
    - So the ticket scope is one long-lived config shared by every socket of the block. A ticket from one block or host still can't resume on another.
    - Tests verify resumption across sockets within one block.

**D7. Retries and body buffering.**
- nginx buffers every body that isn't streamed (16 MiB cap, 64 KiB in memory, the rest on disk). That lets it retry a 503 or a connect failure on another block, including for non-idempotent requests.
- (a) Same semantics, with bodies buffered in memory up to 16 MiB under a process-wide byte budget. Over the budget, answer 503 before reading the body.
- (b) Spill to a temp file past 64 KiB, as nginx does.
- (c) No body buffering. Retry only when the upstream connection failed before the request was sent.
- Recommendation: (a), with the budget sized from M2. Option (c) breaks the api drain contract, because a draining api block answers 503 and relies on the retry.
- **Decision (owner, 2026-11-18):** (a). Bodies are buffered in memory up to 16 MiB each, under a process-wide byte budget sized from M2. Over the budget, connect answers 503 before reading the body. There is one retry on connect error, timeout or 503 (non-idempotent included), on another block. Streamed routes pass through unbuffered.
  - Sized from §7.1, confirmed (owner, 2026-11-18): a 256 MiB budget per connect process, room for 16 concurrent 16 MiB bodies. The measured load is a mean body of 74 bytes and at most 4.3k api requests in flight fleet-wide, which needs under 1 MiB. Grafana pushes and the validators' artifact uploads fit too (§7.1, M2).
  - Amended by D19 (owner, 2026-11-18): only the first 64 KiB of each body is held in memory under the budget, and the rest goes to a temp file, as nginx does.
- Corrected by the review: the recommendation's reason for rejecting (c) was wrong. A draining api never answers 503. It serves for 10 s with `Connection: close`, then closes its listener (§3.1), so the ordinary retry case is a stale pooled connection or a refused dial. The decision stands, with this specification:
  - At most two upstream attempts, on distinct eligible blocks, and only before any response has reached the client.
  - Retried: a connect error, a timeout and an upstream 503. Not retried: 500, 502 and 504, or a partial response. A retried 502 once ran a password reset twice.
  - A block-specific request reports the named block's own result, and is never retried elsewhere.
  - A streamed body becomes ineligible for retry once forwarding begins.
  - Go's transport can retry on its own, so tests count attempts at a recording upstream (§9).
  - A timeout after a POST reached the first block can still run it twice. nginx does the same today under `non_idempotent`, and that stays accepted.

**D8. Limits.**
- nginx's per-address limits are kept per interface process. With the source-address hash split (D10), an address always lands on one block per interface, so per-block buckets reproduce per-interface limits exactly. (Corrected below: one block serves several interfaces.)
- Recommendation:
  - Use alt/rate_limit.go semantics: a burst+1 nodelay token bucket, per-address concurrency, and 429.
  - Take the numbers and route zones from services.yml, and apply them to forwarded routes and to connect's own H1.
  - QUIC connect keeps alt's per-address connection cap.
  - Keep nginx's timeouts and the 16 MiB body cap.
- **Decision (owner, 2026-11-18):** as recommended. alt/rate_limit.go semantics, with nginx's numbers and route zones from services.yml. The limits apply to forwarded routes and to connect's own H1; QUIC connect keeps alt's per-address connection cap. nginx's timeouts and body cap carry over.
- Corrected by the review: a block serves two or three interfaces.
  - A process-wide bucket per address would let independent interfaces use up each other's quota, which nginx's per-interface zones never allow: there is one nginx config per lb block, with zones keyed on `$binary_remote_addr` (../warp/warpctl/config.go:1168-1172, 1718-1744). Alt keys its state by address alone (alt/rate_limit.go:163). So:
  - The shared request and concurrency zones are keyed by ingress interface and normalized client IP.
  - Route buckets are scoped to the service and the matched route within that interface.
  - The lb status exemptions, `exclude_subnets` and per-stream accounting on h2 and h3 carry over.
  - An upgraded connection holds its concurrency slot until the tunnel closes.
  - During a deploy overlap, the draining generation counts its own sessions, so a client may briefly have up to twice its limit. The same happens today when a new lb container starts with fresh zones.

**D9. Default server, ALPN, early data.**
- **No or unknown SNI on TCP.** nginx answers with the lb name's certificate and the lb routes. Recommendation: do the same. On QUIC, refuse, as alt does.
- **ALPN.** Recommendation:
  - h2 and http/1.1 for forwarded names, as nginx does;
  - http/1.1 only for connect names, whose websocket transport is h1.
- **Early data** stays off.
- **Decision (owner, 2026-11-18):** as recommended.
  - No or unknown SNI on TCP gets the lb name's certificate and routes; on QUIC it is refused with 0x1000.
  - ALPN is h2 and http/1.1 for forwarded names, and http/1.1 only for connect names.
  - Early data stays off.
- Amended by the review:
  - Routing after TLS follows §4.2. SNI picks the certificate and the authority picks the route, with nginx's server-name semantics, settled against the pinned nginx image.
  - Connect names keep both H1 carriers: the websocket and the `urnetwork-framer/1` upgrade (H1+).
  - Raw-QUIC connect may offer no ALPN, while api requests use `h3`. As in alt, dispatch picks the front by SNI (alt/alt.go:399-405). ALPN keeps alt's handling, which offers the intersection with `[h3]` (alt.go:356-388), so connect without ALPN still gets through.
  - "Early data off" means the public QUIC config sets `Allow0RTT: false` and TLS refuses early data. Waiting for the handshake to complete before dispatch is a separate control. A resumed session must report `Used0RTT == false` (§9).
  - Real client code exercises WebSocket, H1+, raw QUIC and API h3 before the first cutover (Gate A).

### C. Networking and deploy

**D10. Splitting public flows across a host's five connect blocks.**
- (a) iptables hash split:
  - Per interface, the lb unit owns a mangle rule that hashes the source address into 100 buckets (HMARK).
  - It also owns a nat chain that sends each bucket range to a block's chain by weight (beta 1, g1–g4 24/25/25/25).
  - Each block owns its own chain, which contains only the DNAT to its current port.
  - A deploy switches only that block's chain, and conntrack keeps existing flows on the draining container.
- (b) Random split (`statistic --mode random`). It is simpler, but has costs:
  - one address's flows land on different blocks, so per-address limits multiply by up to five;
  - ticket and stateful affinity weaken;
  - a UDP flow whose conntrack entry expires can come back on a different block.
- (c) One SO_REUSEPORT group across the blocks on each address and port. TCP is fine, but UDP is not:
  - the kernel maps each 4-tuple to a socket by taking a modulo over the group's socket count;
  - every deploy changes the count, which moves most QUIC flows on the host to a process that doesn't know their connection IDs;
  - the only fix is an eBPF reuseport program that steers by connection ID, which needs CAP_BPF and a quic-go connection ID generator;
  - weights would become socket counts.
- (d) A single front block that forwards connect traffic to the others. This brings back the hop.
- Recommendation: (a). It needs two things:
  - HMARK on both the legacy IPv4 iptables and the nft-backed IPv6 iptables (M7);
  - mark bits that don't collide with the routing fwmarks.
- **SO_REUSEPORT within a block.**
  - Each block opens N sockets per (address, port), with one quic-go transport per UDP socket and one accept loop per TCP listener.
  - The group is fixed for the life of the process, so the mapping is stable.
  - A client whose NAT rebinds can land on another socket of the same block and lose its connection. Today it lands on another nginx pseudo-session, possibly on another block, so this is no worse.
  - Recommendation: yes, with N picked from M3; start at 4.
- **Interpretation to confirm.** The original note said "so_reuseport socket listeners". This proposal reads it as sockets within a block, with DNAT between blocks. Confirm, or say if (c) was meant. Resolved: the owner chose (a), below.
- With (a), a fixed 1% of source addresses always lands on beta.
- **Decision (owner, 2026-11-18):** (a).
  - The lb unit hashes the source address into 100 HMARK buckets and DNATs bucket ranges by block weight to per-block chains. Each block's deploy switches only its own chain, and conntrack keeps old flows on the draining container.
  - Within a block, N SO_REUSEPORT sockets per (address, port), starting at 4. Blocks never share a reuseport group.
  - Sized from §7.1:
    - N = 4, confirmed (owner, 2026-11-18). Connect's single listen socket per block took 55% of today's UDP socket drops, and a block's QUIC load is a few thousand datagrams/s.
    - HMARK prerequisites are installed on all four edges (nf_tables backend for both families).
    - HMARK marks must sit outside the full-value routing fwmarks `0x65`–`0x6a`, for example from offset `0x10000`.
    - A test rule on a canary edge, through xops, comes before warpctl relies on it. Done: it passed on edge-4 on 2026-11-18 (§7.1, M7).

**D11. Per-interface binding and IPv6.**
- (a) Each connect block binds the bridge address of every lb interface network on its host.
  - Host networking allows this, and the allocated ports are reused on each address.
  - Replies leave with the interface network's source address, and the existing `ip rule from <subnet>` picks the route table.
  - No new mangle rules are needed.
- (b) DNAT every interface to the `warpservices` address, and route replies by CONNMARK and fwmark on the ingress interface.
- (c) One connect unit per interface, as transparent hosts run `<svc>-<iface>` units. That means two to three times the connect processes, with residents split among them.
- Recommendation: (a).
  - Connect's listeners take a list of addresses instead of `WARP_HOST_IPV4`.
  - QUIC and H1 also bind IPv6. They bind IPv4 only today, while nginx carries the IPv6 clients.
- **Decision (owner, 2026-11-18):** use the binding the lb container and transparent services use today, extended to every interface in one process.
  - Each connect block stays one process (5 per host), and binds each lb interface network's bridge address, IPv4 and IPv6, exactly as the lb container binds its own network's address. In warpctl, that container gets `--dockernet` and `WARP_HOST_IPV4/6` from its interface network.
  - warpctl passes the block every lb interface network on its host instead of one, and each interface's DNAT targets its own address. Replies leave with that network's source address, and the existing `ip rule from <subnet>` route tables carry them.
  - There are no per-interface `<svc>-<iface>` units for connect, and no `--fwmark`: connect's own outbound traffic keeps the default route table.

**D12. Draining forwarded traffic.**
- An old block keeps conntrack-pinned flows for up to 60 m. Besides its connect sessions, those now include:
  - client keepalive connections for api, web, mmm and the other services;
  - forwarded websockets (gossip, grafana).
- Recommendation:
  - At drain start, close idle client keepalive connections.
  - After each in-flight request, send `Connection: close`, h2 GOAWAY or h3 GOAWAY, so clients re-dial and land on the new block.
  - Let in-flight requests finish.
  - Keep forwarded websockets until they close or the drain timeout ends, matching nginx's 60 m `worker_shutdown_timeout`.
- **Decision (owner, 2026-11-18):** as recommended.
  - At drain start, close idle client keepalive connections. After each in-flight request, send `Connection: close`, h2 GOAWAY or h3 GOAWAY.
  - In-flight requests finish.
  - Forwarded websockets stay until they close or the 60 m drain timeout ends.
- Amended by the review: on HTTP/3 the order matters.
  - Today connect sets its status to draining, calls `exchange.Drain()`, and then cancels its serving context, and alt's teardown closes its QUIC transport. A graceful HTTP/3 shutdown during that teardown once produced an exit burst of at least 700k goroutines, which is why alt bounds its close (§3.3).
  - So after the DNAT switch, the old generation starts draining its HTTP front while its transports can still send:
    - it stops admitting new HTTP work;
    - it closes idle H1;
    - it sends an h2 or h3 GOAWAY once per connection.

    Accepted requests finish alongside the resident drain.
  - Hijacked H1 connections, and both sides of forwarded websockets, are tracked through the deadline.
  - A forced transport close has a bounded completion path.
  - Tests: alt's shutdown regressions, plus an in-flight h3 response, a new stream during drain and an established websocket (§9).

**D13. Readiness and holes in the split.**
- Recommendation:
  - Extend connect's `/status` capability headers to name every TCP and UDP listener on each address.
  - warpctl switches a block's chain only when every public listener on every interface address is up.
  - A bucket whose block has no live target falls back to the other blocks.
- Two cases create such a bucket: the block was removed from the config, or warpctl's 30 s reconcile pruned a dead target. The fallback can be implemented two ways:
  - the lb unit reassigns ranges on its reconcile;
  - a catch-all at the end of the split chain.
- A block that crashes keeps its DNAT until systemd restarts it. Its buckets' new flows fail in the meantime (D16).
- **Decision (owner, 2026-11-18):** as recommended.
  - Connect's `/status` names every TCP and UDP listener per address, and warpctl switches a block's chain only when all of them are up.
  - On its 30 s reconcile, the lb unit gives an orphaned bucket range to the other live blocks, and returns it when the block is live again.
- Amended by the review:
  - Readiness and DNAT refer to the same generation. The candidate is probed on its private port for:
    - its generation;
    - its route-file digest;
    - an explicit ready state;
    - its full socket set (§4.7).
  - Losing a socket never ends the process, as connect `6203a4e8` made it for alt. A lost carrier is rebuilt on its own, as the extender rebuilds a lost UDP carrier (§4.4).
    - The extender's listener-loss test (`TestExtenderRebuildsALostUdpCarrier`) is adapted to the front: sibling carriers keep serving.
    - Readiness reports the missing socket until it recovers.
  - A UDP socket lost after promotion is D22.
  - Reassignment moves only the missing ranges, to ready blocks by weight. If every block behind an interface is down, the interface fails its health check (§4.7).

### D. Alt removal

**D14. Where `alt.*` resolves, and the order of removal.**
- (a) Every edge lb_alt interface address, per address family, with the interfaces' DNS weights. Shipped clients need no change, since they present the connect or api SNI and dial UDP 443, 53 and 4053.
- (b) A subset of the edge interfaces.
- (c) Keep the proxy-host addresses, and DNAT them at the router to the edges.
- Recommendation: (a). The cost: circumvention clients lose the address diversity alt gave them, because `alt.*` and the connect names now share addresses. If that diversity matters, (c) keeps it without alt's memory.
- **Names.** The alt names must be DNS-only aliases of connect, with no certificate and no server name. Connect's `expose_aliases` also generates TLS and server names, so they can't go there.
- **Order:**
  1. The edges serve api over h3, and the canary passes.
  2. Move the DNS records and wait past the TTL.
  3. Watch alt's connections and api requests fall to near zero.
  4. Stop alt, then remove the service, its external UDP ports, and the router rules for UDP 443, 53 and 4053 on fireside and crisp.
- **Decision (owner, 2026-11-18):** (a).
  - `alt.*` becomes a DNS-only alias of connect on every edge lb_alt interface, per address family, with the interfaces' DNS weights. Shipped clients need no change.
  - The separate proxy-host addresses are given up.
  - Removal follows the order above.
- Amended by the review: the DNS handover, which Gate B checks (§8 step 6).
  - Today the generator emits host-pinned alt records and rejects conflicting definitions of the same name (../warp/warpctl/dns.go:263, 373-427). Config load refuses `dns_aliases` on an exposed service (../warp/services/routers.go:692).
  - So each alt record has exactly one DNS owner during the handover.
  - The complete record sets are compared before and after, and DNS-only names stay out of TLS and HTTP route generation.
  - Rollback after the move is D20: there is no fallback, because nobody uses alt.

### E. Operations

**D15. CPU and memory on the connect path.**
- TLS handshakes and request proxying for every fronted name now share the 16 Go threads with connect's latency path. nginx's CPU, 24 workers per interface, goes away.
- A reconnect storm, with every client re-dialing after a fleet restart, would land on the connect processes.
- Recommendation:
  1. Measure first (M1–M3).
  2. Cap concurrent handshakes and forwarded requests per service.
  3. Raise connect's `cores` by what nginx used.
  4. Use the beta block, at 1% of addresses, to show the impact on connect latency before the g blocks take traffic.
- **Decision (owner, 2026-11-18):** raise connect's `cores` by nginx's measured CPU (M1). No new per-service handshake or request caps; the per-address limits of D8 still apply.
  - Sized from §7.1, confirmed (owner, 2026-11-18): `cores: 20` on every connect block, up from 16.
    - nginx's work is about 12–15 cores per host at peak. Spread over the four g blocks, that adds 3–4 cores to each.
    - The busiest block peaked at 10.5 cores, so it would reach about 14–15 of 20.
    - That makes 100 Go threads per host, up from 80.
  - Revisit after the beta canary measures Go TLS's cost against nginx's.
  - Caveat: the hosts peak at 80–88% busy. Moving nginx's work into connect is at best CPU-neutral for the host, so this doesn't add host headroom.
  - Amended by the review: `cores: 20` raises GOMAXPROCS on hosts that are already that busy. Before the cutover expands past the canary interface, observe a busy period and a complete deploy, drain and rollback, and check CPU and scheduler delay against §8 step 7's thresholds.

**D16. Blast radius.**
- A connect regression or crash now takes down its share of every fronted service on that host, not only connect.
- Every connect release also redeploys the front for api, web, mmm and the rest.
- Recommendation: accept this, and use beta as the canary for front changes too. Don't add a separate front process, since that is the hop this project removes.
- **Decision (owner, 2026-11-18):** accepted. The front stays in connect, and beta (a fixed 1% of client addresses) is the canary for front changes too.
- Amended by the review: beta is the canary for later front releases, but the first interface switch sends new traffic to all five blocks at once.
  - Qualify beta with pinned test traffic before that switch.
  - Then compare the switched interface with an nginx control interface over the same interval (§8 steps 4–5).
  - Measure each block's actual traffic share and skew. One hash bucket can hold a large NAT.

**D17. Observability.**
- Open question: does anything use nginx's access logs today, such as dashboards, monitor probes or abuse investigation?
  - Answered (2026-11-18): nothing automated. nginx writes its lines to stdout, and Fluent Bit ships them from the journal to Loki with `service`, `block` and `host` labels. The monitor's log tailer skips the `lb` service (monitor/config.go:1354), and no dashboard queries lb lines. They are used ad hoc, as in the 2026-11-15 count behind `no_success_access_log`.
- Recommendation:
  - Export bounded counters and latency histograms by service, protocol, status class and upstream block.
  - Write no per-request logs at the default level; offer an optional sampled access log at V(1).
  - Keep the lb names' status routes, which the monitor and warpctl may rely on.
- **Decision (owner, 2026-11-18):** as recommended.
  - Bounded counters and latency histograms by service, protocol, status class and upstream block.
  - No per-request logs at the default level, and an optional sampled access log at V(1).
  - The lb names' status routes are kept. Route53 depends on them: it health-checks every interface address, IPv4 and IPv6, with a GET of the lb status route on TCP 80 and the `<env>-lb.<domain>` Host (../warp/warpctl/dns_route53.go:113-138, dns.go:582-588). That answer decides each interface's DNS membership.
- Amended by the review:
  - Route53 also counts a 3xx as healthy, so an accidental HTTPS redirect on port 80 would look healthy. The port-80 status route must answer 200 with the expected status JSON, and tests check exactly that.
  - Probe individual blocks as well as the public address. One probe source lands in one hash bucket, so it only ever sees one block.
  - The monitor today rejects a target with service `connect` and front `api`, in both validation and transport code, and its probe ends at the handshake (monitor/public_udp.go:148, public_udp_transport.go:177). Update both, and exercise an api request such as `/hello` over the new path.

**D18. Rollback.**
- Recommendation:
  - Setting `lb_alt: false` on an interface moves it back to nginx, using the same cutover as a deploy: a DNAT switch, with conntrack keeping old flows.
  - Keep building the lb image every release, with its tests green, until the cutover has soaked.
  - After that, nginx config generation stays for environments without lb_alt.
- **Decision (owner, 2026-11-18):** as recommended. Setting `lb_alt: false` on an interface moves it back to nginx with the deploy-style cutover. The lb image keeps being built every release, with its tests green, until the cutover has soaked on all ten interfaces.
- Amended by the review: D14 and D18 share one rollback procedure.
  - A restored nginx interface answers its TCP 80 health check while its UDP goes to nginx's connect-only stream.
  - So after the `alt.*` move, a client that cached that address loses API over QUIC and whodis there. D20 accepts that, because nobody uses alt.
  - Rollback follows §4.6's order in reverse.
  - Keep a tested nginx image with usable certificates through the rollback window.

### F. Found while checking the measurements

**D19. How much of the body-buffer budget one client address may hold.**
- Found 2026-11-18, while checking the D7 budget against services.yml.
- Under D7, bodies up to 16 MiB are held in memory under a 256 MiB budget per process. Under D8, one address may have 50 requests in flight (`net_connections: 50`; under h2, each stream counts).
- So one address can claim 50 × 16 MiB = 800 MiB, more than its block's whole budget.
  - It needs to send very little: a body that trickles in stays open as long as each read arrives within `client_body_timeout` (15 s; 60 s on `/sn/attempt-artifact`).
  - Once the budget is full, the block answers 503 to every request whose body needs buffering. Through the source-address hash, a g block serves about a quarter of an interface's clients.
- nginx has no such shared pool. It keeps 64 KiB of each body in memory (`client_body_buffer_size`) and writes the rest to a temp file, so the same client holds 3.2 MiB of memory.
- (a) nginx's split: the first 64 KiB of each body in memory under the 256 MiB budget, and the rest in a temp file in the container. A retry re-reads the file.
  - Only bodies over 64 KiB spill: the artifact uploads (at most 0.56/s, §7.1) and little else.
  - The budget then covers 4,096 concurrent bodies per process, and filling it takes 80 addresses at their full concurrency.
- (b) Memory only, reserved as bytes arrive, with a per-address cap such as 32 MiB (two full bodies). A handful of addresses still fills a block's budget, and IPv6 addresses are cheap.
- (c) Keep D7 as decided, and accept the exposure.
- Recommendation: (a). It matches nginx's memory profile exactly, and the measured load almost never writes to disk.
- **Decision (owner, 2026-11-18):** (a), nginx's split.
  - Each body keeps its first 64 KiB in memory under the 256 MiB process budget. The rest goes to a temp file in the container, which is removed when the request ends.
  - A retry re-reads the body from memory and the file. Over the budget, connect answers 503 before reading the body, as D7 says.
  - Amended by the review: the temp files need their own caps (D21).

### G. Raised by the review (LBALT-REVIEW.md, 2026-11-18)

All three were decided by the owner on 2026-11-18.

**D20. Rolling an interface back after `alt.*` moves.**
- After D14, clients cache edge addresses for `alt.*`. Suppose an interface then rolls back to nginx (D18).
  - Its TCP 80 health check passes, but its UDP goes to nginx's connect-only stream.
  - A client that cached that address loses API over plain QUIC and over whodis there. DNS withdrawal helps only once the client looks up another address.
  - nginx interfaces don't publish UDP 4053 today (§4.1), so the client's second whodis port fails too.
- (a) A PROXY-aware private backend.
  - Connect's trusted UDP listeners, which nginx streams to with PROXY v2, also dispatch API h3 by SNI, as the public front does.
  - The nginx path publishes UDP 4053.
  - Rollback then keeps API over QUIC and both whodis ports.
- (b) No interface rollback once `alt.*` has moved. Withdraw the interface from DNS instead, and accept the gap for clients with a cached address.
- (c) Keep `alt.*` on the proxy hosts until every interface has soaked, with alt deployable until then.
- Recommendation: (a).
  - The dispatch is the front's own code on a second listener.
  - It makes D18's single-interface rollback safe after D14.
  - Gate B (§8) rehearses rollback whichever option is chosen.
- **Decision (owner, 2026-11-18):** no fallback. Nobody uses alt today, so there is nothing to protect on the alt path:
  - no PROXY-aware private backend;
  - no forwarding of the old alt (proxy-host) addresses;
  - no keeping alt answering for clients that cached them.

  An interface that rolls back after the move loses API over QUIC and whodis until DNS withdraws it. Gate B keeps only its DNS-correctness checks (§8 step 6).
  - Alt's measured load (M5) is probe traffic, and clients default to H1 (owner, 2026-11-18).

**D21. Caps on D19's temp-file spill.**
- With no cap, 4,096 in-memory 64 KiB prefixes can come with up to 63.75 GiB of temp data per process, at the 16 MiB request cap.
- Measured 2026-11-18: the Docker root is on the root ext4 filesystem of each services edge, with 1,352 GiB (edge-1) to 3,058 GiB (edge-4) free. The measured need is under 70 MB fleet-wide (M2, artifact uploads).
- Proposal: 4 GiB and 1,024 files per connect process.
  - With two generations of five blocks overlapping in a deploy, that is at most 40 GiB per host, under 3% of the smallest edge's free space.
  - One address can claim at most 800 MiB (50 requests × 16 MiB), so filling one block's spill takes six addresses.
- Specified by the review, whatever the numbers:
  - A body of unknown length reserves before its first read.
  - A body that crosses 16 MiB is answered with 413 at that point.
  - A local capacity refusal is a 503 counted apart from upstream 503s, and only an upstream 503 can be retried.
- **Decision (owner, 2026-11-18):** as proposed. The caps are 4 GiB and 1,024 files per connect process, with the review's rules above.

**D22. A UDP socket lost after promotion.**
- D10 gives each block N = 4 SO_REUSEPORT UDP sockets per (address, port), each with its own quic-go transport.
- The kernel maps each 4-tuple to a socket by a hash scaled by the group size. When a socket exits, or comes back, the size changes, so most flows move to a transport that doesn't know their connection IDs.
- The front must survive a lost socket and rebuild it (§4.4, D13). In a reuseport group, a rebuild changes the group size again.
- (a) Keep one transport per socket.
  - On loss, re-create the socket at once, and accept one reconnect wave for that block's QUIC clients.
  - The endpoint reports not ready until the socket is back.
- (b) Feed an endpoint's N sockets into one quic-go transport through a fan-in packet conn. A flow that moves between sockets then still finds its connection, which also makes NAT rebinding within a block harmless.
  - Cost: quic-go's per-socket features, GSO and ECN through the OOB conn, need care in the wrapper.
- (c) Steer by connection ID with an eBPF reuseport program. That is out of scope (§10).
- Recommendation: (a) for the cutover, with (b) as a follow-up if the canary shows rebinding or socket-loss resets.
  - Losing a socket after promotion is rare.
  - QUIC carries about 4% of connect's clients (M4), and a reconnect wave touches only one block's share of them.
- **Decision (owner, 2026-11-18):** as recommended.
  - A lost socket is re-created at once, and that block's QUIC clients reconnect once.
  - The endpoint reports not ready until the socket is back.
  - (b) is the follow-up if the canary shows rebinding or socket-loss resets.

## 7. Measurements before deciding

- **M1. nginx CPU and RSS per lb interface on each edge.** This is the budget connect inherits (D15).
- **M2. Request and TLS handshake rates per service per interface, and the body-size distribution on buffered routes.** These size the forwarder and the buffer budget (D7).
- **M3. Connect CPU headroom per block, and current QUIC and H1 latency.** This is the baseline for the win and for regressions, and it picks N sockets (D10).
- **M4. UDP loss and reordering through nginx today.** Read `RcvbufErrors` and `InErrors` on the nginx and connect sockets, plus the stream session counts. The latency claim rests on this, and it is the number the cutover should move.
- **M5. Alt traffic that moves to the edges.** QUIC connections, the api h3 request rate, activation posts, and the share of `alt.*` DNS queries (D14).
- **M6. Edge memory headroom per host**, with alt's clients added.
- **M7. HMARK support in the edges' iptables** (legacy for IPv4, nft-backed for IPv6), and the fwmark bits in use (D10).

### 7.1 Results (2026-11-18, ~20:20Z)

Sources:
- Mimir: 7-day windows.
- A read-only probe on edge-0/1/3/4, as an unprivileged user: cgroup v2 stat files, `/proc/net/{udp,udp6,tcp,tcp6,snmp}`, and the deployed unit files.
- The lb nginx containers and the connect blocks were all about 30.5 h old, so the per-socket cumulative counters cover the same 30.5 h on every host.

Host CPU was 54–79% busy at sample time. The 7-day peak is 80–88%.

**M1. nginx per host.** CPU is a 60 s sample. Memory is the cgroup's `memory.current`.

| Host | Interfaces | nginx cores (now) | nginx memory |
|---|---|---|---|
| edge-0 | 3 | 9.4 (eno4np3 8.0, the other two 0.6–0.7) | 24.2 GiB |
| edge-1 | 3 | 10.3 (eno2np1 8.9, the other two 0.6–0.9) | 21.7 GiB |
| edge-3 | 2 | 9.1 | 18.1 GiB |
| edge-4 | 2 | 10.0 | 16.9 GiB |

- Fleet total: 38.9 cores now. Scaled by connect's peak-to-now ratio (1.15–1.5×), about 12–15 cores per host at peak.
- An idle interface's nginx holds 5.7–6.0 GiB. That is consistent with per-worker connection arrays sized by `worker_connections`. The cutover frees all of it.

**M2. Request and TLS rates.**
- **api:** 9.1k req/s now, 21.1k req/s 7-day peak (hourly). Mean request body 74 bytes. The heaviest route by bytes is `POST /connect/control` at 0.47 MB/s fleet-wide. Peak in-flight requests: 4,247 fleet-wide.
- **connect:** H1 upgrades at 1.0k req/s now, 1.2k req/s peak. alt: 91 req/s. mcp: about 0.
- **TCP accepts per host:** 1.2–1.6k/s now, 2.2–4.1k/s 7-day peak. This is an upper bound on nginx's TLS handshakes, since upstream accepts count too.
- **Certificates:** per-name ECDSA P-256, so handshake CPU stays well under a core per host at peak.
- **Connections:** nginx holds 78–108k established TCP connections per host on 443. Of those, 56–84k are connect H1 websockets (connect's port-80 upstream connections).
- **Other buffered bodies (checked after the D7 sizing).**
  - nginx proxies grafana, a websocket service, with response buffering off but request buffering on, so Loki and Mimir pushes are buffered too. Only the off-LAN hosts (the VPN server and snow) push through the public grafana name. LAN hosts push straight to `main-grafana.local:3100`, which bypasses the lb.
  - The validators' `POST /sn/attempt-artifact` uploads (up to about 4.2 MB) peak at 0.56/s fleet-wide (7-day max of 5 m rates), with under 0.3 in flight at the api. Even at 30 s per upload, that is about 70 MB across the 20 connect processes.
  - The 256 MiB budget holds for both.

**M3. Connect CPU.**
- Per host: 24–31 cores now, 35–45 cores 7-day peak.
- The busiest single block peaked at 8.5–10.5 cores of its 16.

**M4. UDP through nginx today.**
- **Socket drops.** Cumulative over 30.5 h on the four hosts, 83.8k datagrams were dropped: about 0.76/s against roughly 100k UDP datagrams/s in. That is under 0.001%.
  - 55% of those drops were at connect's single listen socket per block.
  - 45% were on nginx's listen sockets, concentrated on the two busiest interfaces (edge-1 eno2np1, edge-0 eno4np3).
- **Other drops.** PROXY-reader drops (`urnetwork_connect_pp_dropped_packets_total`): 38 in 7 days, all `missing_header`. UDP `NoPorts`: about 0/s.
- **QUIC versus H1.** QUIC is a small share of connect's clients: about 9.3k nginx UDP pseudo-sessions fleet-wide, against 267k connected clients (the gauge counts H1 carriers). That puts about 96% of connect's clients on H1 over TCP, where nginx terminates TLS and copies every byte.
- **No end-to-end metric.** Connect exports no QUIC loss or RTT metric, so the latency win can only be measured end to end. The beta block is the place to measure it.
- **Not measured** (review): application latency, and the exact client mix by carrier.
  - The perf harness (connect/connect_perf_test.go) dials a plaintext loopback URL in H1 mode.
  - Before the switch, it pins WebSocket and H1+ separately, adds QUIC and whodis cases, and records an nginx baseline (§8 step 4).

**M5. Alt.**
- Load: 91 req/s, mostly `/hello` (42/s) and `/connect/control` (37/s); 154 KB/s of h3 datagrams in; 0.3 cores per proxy host.
- Memory: 5.7–6.0 GiB RSS per proxy host.
- Moving this load to the edges is negligible.
- That load is probe traffic, and clients default to H1 (owner, 2026-11-18).

**M6. Edge memory.** The 7-day minimum of available memory is 418–645 GiB per host. Memory is not a constraint.

**M7. iptables and HMARK.**
- iptables is v1.8.10 with the nf_tables backend for both IPv4 and IPv6 on all four edges.
- `libxt_HMARK.so` and `xt_HMARK.ko` are installed. The module is not loaded; it loads on first use.
- Every lb interface has a full-value `fwmark 0x65`–`0x6a` rule (`lookup warp101`–`warp106`).
- **Verified on edge-4, 2026-11-18 ~22:55Z: PASS.** The run was `run-edges.sh --tags lbalt_hmark_test --limit by-us-fmt-5-edge-4`, at xops 4806305.
  - The test runs HMARK and a DNAT split by bucket on loopback flows only, to ports it holds. It removes every rule it adds: 16 rules and chains removed, none left.
  - The kernel is 6.8.0-147-generic. `xt_HMARK` loaded on first use, and `nft_compat` was already loaded.
  - IPv4: 800 flows from 200 sources, with none lost and none undiverted; 88 of the 100 buckets were hit.
    - Each source's TCP and UDP flows shared one bucket and one port. So `--hmark-tuple src` hashes the source address alone, with no protocol or ports.
    - PREROUTING and OUTPUT put every flow in the same bucket.
  - IPv6: all flows from the one source landed in one bucket. With the source port added to the hash, 200 flows hit 88 buckets.
  - No iptables rule on edge-4 sets or matches a mark. Its route rules (fwmark `0x66` and `0x67`, full mask) select none of the test marks.
  - An earlier dry run, in a container on kernel 7.0.14, gave an identical IPv4 split (376 of 800 flows to port A, 88 buckets hit). That fits a hash that depends only on the seed and the address, not on the kernel.
  - Scope (review): this used loopback flows and one IPv6 source. It shows that HMARK selects buckets, not the end-to-end return path, which the two-interface network-namespace harness tests (§4.7, §9).

**Certificate expiry.** Every certificate in `vault/all/tls/2026.10.16` expires 2027-01-15. Under D6b, the next set must land and connect blocks must redeploy before then.

## 8. Rollout

One cutover per interface (D1).

**0. Measure.** Decisions are recorded in §6. Measurements M1–M7 were taken 2026-11-18 (§7.1), and the owner confirmed the values they sized. The HMARK test passed on edge-4 (M7), so warpctl can rely on HMARK for bucket selection in step 3. The return path is tested by the network-namespace harness (§4.7).

**1. The front: `server/nginx` and connect (§4.5).** Starting from alt's code:
- TLS for every fronted name;
- h3 and whodis dispatch for connect and api names;
- HTTP/1.1 forwarding with the nginx request contract;
- limits, retries and buffering;
- redirects and response headers;
- the lb routes and port 80;
- per-interface, dual-stack listeners;
- the PROXY reader off on interface-bound sockets;
- extended readiness;
- the H1+ upgrade beside the websocket on connect names, and a public QUIC config that refuses early data (D9);
- an HTTP drain that starts while the transports can still send (D12).

**2. Parity harness, before any traffic.** Send the same requests to the lb image and to the front, both generated from the same services config. Diff the status, the response headers, and the headers the upstream receives. From the review:
- Generate the route file and the nginx config from one fixture, and run the pinned lb image beside the front.
- Use recording upstreams for requests with side effects. Compare the URI and query, the bytes in both directions, the headers and the attempt count.
- Cover:
  - the hidden per-block routes and redirects;
  - CORS preflight and errors, `Vary: Origin`, and CSP-dependent frame options;
  - streaming;
  - the TCP service-name `/status` denial, and alt's API h3 `/status` 404.
- Routing after TLS (D9):
  - missing SNI with a known Host;
  - a known SNI with a different known authority;
  - two authorities on one h2 connection;
  - exact versus wildcard names.

  Assert the selected certificate, the upstream, the forwarded Host and access to status routes.
- Upstream choice:
  - Hold requests open to check weighted `least_conn` under known load.
  - For mcp, compare the chosen backend with nginx's weighted Ketama ring on fixed IPv4 and IPv6 client addresses. A different consistent hash can be stable on its own and still remap clients during a mixed rollout.
- Timeouts: an upload that keeps progressing past the idle timeout, and a stalled one on the artifact route's 60 s override.

**3. warpctl: lb_alt mode.**
- routing-only lb units that keep their interface in DNS and in the edge inventory (§4.6);
- one public port list for lb_alt interfaces, consumed by both the host rules and the VyOS generator, with UDP 4053 published (§4.1);
- the hash split for TCP and UDP, and block-owned chains, built complete before an entry jump switches (§4.7);
- generation-owned ports, and readiness on the candidate's private status port (§4.7);
- multi-block validation.

**4. Gate A: before the first public cutover** (review). Each item has tests in §9.
1. UDP 4053 is published on the host and at the router. A whodis exchange succeeds on both 53 and 4053 in each address family, with the right public reply address and port (§4.1).
2. A host with one interface in each mode keeps an established connection through each direction of the switch, while fresh sessions succeed on both interfaces (§4.6).
3. Readiness and DNAT refer to the same generation, and the two-interface network-namespace harness passes (§4.7).
4. The HTTP/3 drain ordering passes alt's shutdown regressions and the new in-flight cases (D12).
5. Request routing after TLS matches nginx. Real client code passes on WebSocket, H1+, raw QUIC and API h3 (D9).
6. Retries, body storage and limits pass at their scopes (D7, D8, D19, D21).
7. Beta is qualified with pinned test traffic before the switch (D16), and an nginx-path baseline exists for every step 7 metric.

**5. Canary.** Before the first cutover, connect's `cores` goes to 20 on every block (D15).
- As implemented, `services.connect.lb_alt: false` is declared, and every connect block deployed, before the canary override: §11.3 (d) gives the order.
- Cut over one low-weight dual-stack interface first, TCP and UDP together. Edge-0 `eno3np2` and edge-1 `eno4` have IPv4 only, so the first switch avoids them.
- Compare that interface with an nginx control interface over the same interval.
- Then switch the rest one interface at a time. Each next interface waits until step 7's thresholds have held through a busy period and a complete deploy, drain and rollback.

**6. Gate B: before moving `alt.*` DNS** (review; D14, D18; narrowed by D20).
- Each alt record has exactly one DNS owner during the handover.
- Compare the complete record sets before and after:
  - the implicit `<env>-alt` names and the bare aliases;
  - the A-only `-v4` and AAAA-only `-v6` names;
  - weights and health membership.
- DNS-only names stay out of TLS and HTTP route generation.
- Keep a tested nginx image with usable certificates for the rollback window (D18).
- Dropped by D20, because nobody uses alt:
  - the rollback rehearsal with clients pinned to the old proxy address and to the new edge address;
  - keeping alt answering for clients that cached the proxy addresses;
  - tracking extender activations by carrier and family;
  - keeping alt and its router rules through a post-DNS soak.

**7. Promotion evidence** (review). Set numerical promotion and rollback thresholds from the step 4 baselines, for:
- H1 and H1+ upgrade success, and p99 application RTT;
- QUIC and whodis success and RTT;
- forwarded 5xx and retries;
- CPU, scheduler delay, socket drops and spill usage;
- each block's actual traffic share and skew, especially behind large NATs.

Include a reconnect wave with full and resumed handshakes.

**8. Alt.** Move `alt.*` after Gate B, then remove alt in D14's order. Nobody uses alt, so it gets no post-DNS soak (D20).

**9. Soak.** Then stop building the lb image for main.

## 9. Tests (outline)

Every path above gets a test that fails without the change:

- dispatch per name and protocol, including the refusals;
- the forwarding contract against nginx's (§8 step 2), including that spoofed headers are removed;
- a PROXY header sent by a client on an interface socket is not honored;
- the 503 retry with buffered bodies, and the refusal over budget;
- one address cannot hold more than its share of the body budget (D19);
- Route53's health check (TCP 80, the `<env>-lb` Host, IPv4 and IPv6) gets the same answer as from nginx;
- limits per address and per route;
- a drain closes keepalive connections and keeps websockets;
- the warpctl iptables split, block cutover, bucket fallback, ownership validation and rollback, in the style of `../warp/warpctl/run_test.go`;
- the connect perf harness (connect/connect_perf_test.go), direct and through nginx, for M4.

From the review:
- **Port contract.**
  - TCP 80 and 443 and UDP 443, 53 and 4053 work through the generated host and router rules.
  - A whodis exchange works on both 53 and 4053, with the public reply address and port checked.
  - The family matrix comes from the inventory: ten public IPv4 addresses and eight IPv6.
  - The legacy test that the forward target stays private still holds on nginx interfaces.
- **Handover.**
  - A host with one interface in each mode.
  - Established connections held through each direction of the switch.
  - D2's three override cases: no override; `true` under a disabled service default; `false` under an enabled default, which is the rollback switch.
- **Generations and chains**, in a Linux network namespace with two interfaces:
  - a block promotion concurrent with an lb reconcile;
  - block loss and return;
  - stale UDP tuples;
  - an interrupted rollback.

  Check traffic both ways, the original public source on replies, and the routing marks on unrelated traffic.
- **Readiness.** Four sockets are counted per (address, protocol, port). `draining` is never ready. A socket lost after promotion behaves as D22 says.
- **Drain.**
  - Alt's shutdown regressions (alt/alt_close_test.go).
  - An in-flight h3 response, a new stream during drain, and an established websocket. The old response completes, a fresh dial reaches the new generation, and the deadline releases the old sockets.
- **Carriers.**
  - WebSocket, H1+, raw QUIC and API h3 with real client code: authentication, a bidirectional exchange, whodis on both ports, and the existing refusals.
  - A resumed QUIC session reports `Used0RTT == false`.
  - One handshake admission limit across all sockets.
  - The extender's listener-loss test (`TestExtenderRebuildsALostUdpCarrier`, ../connect/extender/extender_carrier_resilience_test.go), adapted to the front: sibling carriers keep serving, and readiness shows the missing socket until it recovers.
  - Alt's survival of a datagram it can't write (transport_pt_server_failure_test.go) carries over.
- **Retries.**
  - A buffered POST across an api drain.
  - A 503 replayed byte for byte.
  - Application 500, 502 and 504 are not retried, and neither is a partial response.
  - Attempts are counted at a recording upstream, including any automatic retry by Go's transport.
  - A block-specific status request reports that block.
- **Body storage.**
  - The 64 KiB and 16 MiB boundaries, and 413 as a body crosses 16 MiB.
  - Unknown-length bodies.
  - Cancellation during spill or replay, and failed writes under a small injected quota.
  - Every exit releases bytes, descriptors and files, and a local 503 is told apart from an upstream 503.
- **Limits.**
  - One source across two interfaces, two services, two reuseport sockets and a deploy overlap.
  - The lb status exemptions and excluded subnets.
  - Per-stream accounting on h2 and h3, and upgrade concurrency held until the tunnel closes.
- **TLS.**
  - Resumption across sockets within one block (D6c).
  - SAN coverage for aliases, parent fallback and wildcards.
  - The route file and every certificate are loaded before readiness.
- **Monitor.**
  - Port 80 returns 200 and the expected status JSON, not a 3xx.
  - Individual blocks are probed as well as the public address.
  - A target with service `connect` and front `api` is accepted, and its probe completes `/hello`.
- **Performance.** The perf harness pins WebSocket and H1+ separately, adds QUIC and whodis cases, and has an nginx baseline.

## 10. Not in scope

- QUIC connection-ID steering, and connection migration across blocks.
- Changes to the whodis encoding or its tld.
- The proxy service on fireside and crisp.
- Environments other than main, where lb_alt stays off.

## 11. Implementation notes (for code review)

Written 2026-11-19, at server a297487df and warp bdacae4 on `lbalt/integrate`, after round 2 and the final adversarial review. The units built to [LBALT-IMPL.md](LBALT-IMPL.md), the implementation contract, whose §4 (accepted nginx differences) and §6 (status) are replaced together with this section; the final review's corrections to its §2.2, §2.5, §2.7, §2.8 and §7 are marked "(final review)" there.

Server paths are from the server root and warp paths from `../warp/`; in a warp item a bare file name is in `../warp/warpctl/`. Code is named `path:Symbol`, a method `Type.Method`, and a name without a path is in the file named before it, or its package. Line numbers are left out, since they move.

### 11.1 Where things are

- `lbalt/integrate` holds everything. Server: `/Users/brien/urnetwork-wt/lbalt/integrate/server` at a297487df, merge base 16e4846aa; origin/main has moved since, so main is merged in before `lbalt/integrate` goes to main. Warp: `/Users/brien/urnetwork-wt/lbalt/integrate/warp` at bdacae4, merge base 9782f32.
- `lbalt/base` (server 82456174a, warp ee9d340) holds the contract, the route file types and parser, the settings, the API stubs and the shared fixture.
- Each unit has a branch `lbalt/<unit>` and a worktree under `/Users/brien/urnetwork-wt/lbalt/<unit>/`. Server: `s-tls`, `s-forward`, `s-http`, `s-connect`, `s-monitor`; then `int-front`, `int-e2e`, `int-monitor`, `int-carriers`, `harness-parity`; then round 2's `r2-forward`, `r2-http`, `r2-monitor`, from 4285c3007, merged as 869f0d563, b99fc7b5a and c6c0712bd; then the final review's `final-nginx` and `final-connect`, from 85101757a, merged as 9981127c4 and 9ff29fe87. Warp: `w-route`, `w-split`, `w-units`, `w-edge`, then `int-warp`, `harness-netns`, `harness-parity`, then `final-warp`, merged as bdacae4.
- The integrator's own commits: server 8af8f868d, 6505ed506, 6efe5fbad, 0e9138ed1, 2ff1790c2, 4285c3007, 85101757a and a297487df; warp 7b71ef2, 295cd2c, cd6963a and 4cb3677. The units' reports stay outside the repos; their decisions are in LBALT-IMPL §5 and §7 and here.
- Size against the merge bases: server 138 files, +57,253 −195 lines (production Go +13.4k, tests +39.2k), 187 commits besides merges, 474 new test functions; warp 49 files, +26,604 −311 (production Go +6.6k, tests +16.0k), 70 commits, 199 new test functions.
- Builds use `GOFLAGS=-trimpath` and the shared `GOCACHE=$WARP_HOME/.gocache`, which has filled the disk: check `df -h` first, and trim it with `find "$GOCACHE" -mindepth 2 -maxdepth 2 -type f -mmin +720 \( -name '*-a' -o -name '*-d' \) -delete`. Never `go test ./...` at the server root; run `GOOS=linux go vet ./warpctl/` after every warp merge, since warp's netns files are linux-only.
- Commit through `git -c core.hooksPath=.githooks commit` (no AI attribution); its gofmt hook re-adds staged Go files from the working tree, so a commit split through the index picks up unstaged edits.

**The final review** (2026-11-19) read the merged code in eight dimensions: lifecycle, contract, forwarder, tls-quic, security, split, warpctl and monitor. Verifiers confirmed 30 of its 32 findings (3 high, 7 medium, 20 low) and refuted 2 (§11.6). Fixers worked in three areas; a second pass fixed what a re-review found (5 low in nginx, 1 medium in connect, 2 low in warp):
- nginx, 14 findings, `lbalt/final-nginx` (15 + 7 commits), merged as 9981127c4: a client gone away, h3 bodies, header counts and repeats, 405 for a path with no location, the upstream path's escaping, gzip types, lapsed certificates, the drain's upgrade test, three parity entries;
- connect and monitor, 8 findings and round 2's six monitor items, `lbalt/final-connect` (19 + 4), merged as 9ff29fe87: the forced close and the drain, connect's routes, the api QUIC cap and header bound, the write bound, the public UDP bound and path key, the probes' seams, `scope-excluded`;
- warp, 8 findings, `lbalt/final-warp` (6 + 3), merged as bdacae4: the enabling rule, the first switch, connect workers' public addresses, IPv6 chains, load's name and keepalive checks;
- then the integrator's a297487df: an h2 Content-Length nginx can't parse is a 400.

Not taken:
- an h3 peek before a streamed route's head: the vendored http3 can't peek without waiting (§11.4);
- redoing a written request on the same block: under D7 the block may have run it;
- a raw-head scanner for obs-fold and HTTP/1.0 Transfer-Encoding: both are accepted as parity entries;
- preferring a valid older certificate: the front serves warpctl's pick, as nginx does;
- http/1.0 in the ALPN lists: the owner's choice, documented and pinned;
- unrecognized_name for an unknown QUIC name: alt answers internal_error;
- replaying a close or drain called during start: no caller makes one, so the doc is narrowed;
- exempting hijacked connections from the write bound: nginx bounds them too, and a websocket stayed wedged;
- any override as the lb_alt host rule: a default flip, rollback included, would still redeploy connect.

### 11.2 Code map

Each heading names the decisions and sections it implements. D15 and D16 have no code (services.yml's `cores: 20`, and the rollout), nor has D20 (connect's trusted listeners are unchanged).

**Route file (D5; IMPL §2.1).**
- `nginx/route.go`: `LoadRouteTable` (revision: the bytes' sha256), `ParseRouteTable` (strict: `checkRouteShape` refuses unknown, recased and null keys before `RouteTable.validate`), `RouteTable.ServedNames` (no unexposed services); `connect/front_load.go:loadFront` requires the file's generation to be `WARP_GENERATION` and its env `WARP_ENV`.
- warp: `../warp/warpctl/lbalt_route_schema.go:LbAltRouteTable` mirrors it, with the HMARK constants; `lbalt_route.go:renderLbAltRouteTable` renders it through the nginx generator's helpers and `../warp/services/services.go:Keepalive.Resolve` (with `parseNginxTimeSeconds`, moved there), `validateLbAltRouteTable` checks it, and `writeLbAltRouteFile` writes it, 0444, in a new 0700 directory. `lbalt_worker.go:RunWorker.prepareLbAltWorkerGeneration` checks the block's chain names, writes the file, and returns its read-only mount, `WARP_LBALT_ROUTE_FILE` and `WARP_GENERATION`.
- The fixture `nginx/testdata/route_v1_example.json` is byte-identical to warp's, rendered from `../warp/warpctl/testdata/lbalt_route_services.yml`.

**Units and flags (D2, D11; IMPL §2.2).** `../warp/services/lbalt.go:IsLbAltHost` (§11.3 (d)); `config.go:SystemdUnits.generateForHost` adds `lbalt_worker.go:SystemdUnits.lbAltServiceUnitArgs` (`--lbaltnets` with every non-transparent lb interface, `--lbalthost`) and `SystemdUnits.lbAltLbUnitArgs` (`--lbalt=true`, `--lbaltblocks`); `parseLbAltWorkerFlags` panics on either connect flag alone; `parseLbAltWorkerInterfaces` reads only docker networks, so `lbAltWorkerInterface` and its `splitInterface` hold no public address. `config.go:portBlocksOf` gives connect's tcp 443 the lb type `"tcp"` on its block, so no port moves and the nginx generators ignore it; `getBlockInfos` keeps lb_alt interfaces as lb blocks, whose nginx configs stay baked for a rollback.

**Public ports (§4.1; IMPL §2.3).** `../warp/services/lbalt.go:LbAltPublicPortTable` is the one table (tcp 80 and 443, udp 443 and 4053, udp 53 to 4053's socket), read through `LbAltPublicPorts`. `lbalt_edge.go:lbAltCheckPublicPorts` refuses a port no front socket serves, for the split plan and VyOS. `run.go:publicPortServiceTargets` stays nginx's map.

**Split (D1, D10, D13, §4.7; IMPL §2.5).** `lbalt_split.go`, whose file doc describes the scheme; base's entry points are `reconcileLbAltSplit`, `removeLbAltSplit`, `writeLbAltBlockChains` and `pollLbAltReadiness`.
- `lbAltSplitWriter.lockHost` (the host lock, from the listing to the last write) and `apply` (one `iptables-restore -w --noflush` per table and family); the chains of `newLbAltChainNames`; `lbAltInterfacePlan.hmarkRules` (PREROUTING and OUTPUT, `--ctstate NEW`, `--hmark-src-prefix 32|128`) and `readyBlocks` (the block's chain, its first DNAT to the family's bridge, a listener there).
- `assignLbAltBuckets` moves only orphaned buckets, by largest remainder, from `services.LbAltBucketBlocks`; `lbAltSplitWriter.reconcileFamily` holds a first switch (`nginxServes`, by `lbalt_ports.go:listenSockets.holds`; `errLbAltSplitAwaitsEveryBlock`); `lbAltSplitWriter.convergeSplit` acts on four listed states, with the tcp and udp jumps together (`entryRules`, D1).
- `writeBlockChains` flushes and refills each block chain in one transaction per family, or writes nothing; `removeFamily` takes out the entry jumps and split chains, then HMARK, and keeps block chains; a family with no ready block converges, then returns `errLbAltSplitNoReadyBlock`. `lbAltChainOwnedBy` and `RunWorker.lbAltChainTargetPorts` give the ownership guard and the allocator the block's chains, the latter reading IPv6 whenever `RunWorker.bindsLbAltInterfaces`.

**Ports and conntrack (§4.7).** `lbalt_ports.go:readListenSockets` (`ss -Htuln`, else `netstat -tulnW`; an unreadable row fails it); `run.go:RunWorker.findOccupiedPortsAndSockets`; `assignDeployPorts`, which also skips `lbalt_worker.go:RunWorker.lbAltWorkerChainPorts`; `run.go:RunWorker.validateRedirectPortOwnership`, which skips the worker's own chains; UDP conntrack cleanup of ports no socket holds, `RunWorker.cleanupStaleConntrack` and `lbalt_ports.go:RunWorker.cleanupStaleLbAltBridgeConntrack`.

**Readiness (D13; IMPL §2.4).** `connect/front_status.go:frontServer.writeStatusHeaders` writes the `X-UR-Front-*` headers, and `connect/run.go:runWithDependencies` loads the front and binds every public socket before the private listener first answers. warp's `pollLbAltReadiness` accepts a candidate by `acceptLbAltStatus` (200, `ok`, Ready 1, its generation and revision, every socket `4/4`) and gives up at once by `refuseLbAltReadiness`.

**Deploy and handover (D1, D13, D18; §4.6).**
- `run.go:RunWorker.deployContainerOverlap`: refuse a routing-only lb unit; the status polls; ownership validation; `cutover.commit()`; `lbalt_worker.go:RunWorker.assignLbAltWorkerChains`; then one promote step, `writeLbAltWorkerBlockChains`, `redirect` and `removeLbAltWorkerSplitAfterCutover`, whose failure on a block that binds lb interfaces leaves a pending promotion (`deploymentRetirementError`).
- `run.go:RunWorker.Run`: the routing-only loop (`lbalt_worker.go:RunWorker.isRoutingOnlyLb`) runs `reconcileLbAltWorkerInterface` at its start and after each 30 s routing reconcile, which writes the split record (`<WARP_HOME>/lbalt-split/<env>/<iface>`), reconciles, and on nil retires the interface's nginx. The version loop calls `retryLbAltWorkerSplitRemovalIfDue` and `refreshLbAltWorkerChainsIfDue`; on rollback an nginx lb unit removes a recorded split after nginx's redirect.

**DNS and the router (D14; §4.1).** `dns.go:dnsDerivation.domain` derives the lb_alt service's `dns_aliases` (an alias of `<env>-lb` when every member is lb_alt, else a weighted set over the lb_alt members, nothing while none is); `dns_route53.go:route53Provider.reconcile` reports stale records as notes; `../warp/services/routers.go:validateDnsAliases` allows one owner per name; `vyos.go:VyosGenerator.publicRules` opens exactly the lb_alt ports, 4053 last.

**Validation (D2, D4).** `../warp/services/load.go:validateLbAlt` refuses a second declaring service or one that isn't connect, an override on a transparent interface, an lb_alt interface whose blocks are missing or unsplittable, the lb_alt service's tcp stream ports and mappings, another claim of its udp ports, a public port outside the table, and a service named `lba` or `lbt`, alone or before a `-`; on an lb_alt host, a `-` in an lb interface name, a block chain over `lbAltChainNameLimit`, and front interfaces whose keepalives differ by `Keepalive.Resolve` (`ResolvedKeepalive`). `validateLbAltTlsServicePort` guards 443 across versions. Fields: `../warp/services/services.go:ServiceConfig.LbAlt`, `LbBlock.LbAlt`.

**Lb version polls (IMPL §2.6).** `docker.go:sampleStatusVersions` counts a `"front":"lbalt"` sample as neither a version nor an error; `pollLbServiceStatusUntil` skips the poll when `services.NginxLbInterfaces` is empty.

**Front: TLS (D6, D9).** `nginx/tls.go:LoadCertificateStore` refuses a missing or unreadable pair, a key that doesn't match and a SAN miss (`checkCovers`), and logs a lapsed leaf (`certificateValidAt`), which `CertificateStore.NotValidNames` lists. `newTlsConfigs` builds every config once: for TCP one per served name and a default, for QUIC two per name, with and without h3; `nginx/front.go:Front.TlsConfig` and `Front.QuicTlsConfig` hand the same pointers to every `tls.NewListener` and `ListenEarly`.

**Front: names and routing (D3, D9; §4.2).** `nginx/name.go:normalizeHost` ports ngx_http_validate_host; `nameMatcher.match` takes an exact name, then the longest leading wildcard, over one or more labels. `nginx/route_http_request.go:readRouteRequest` applies the method rule, 414 and 400, header names, max_headers (`HttpSettings.MaxRequestHeaderLines`), `routeRequestUniqueHeaders`, an h2 Content-Length's syntax, the h2 and h3 Host, and 405 for TRACE and CONNECT. `nginx/route_http.go`: `routeHttpHandler.serve` (redirects first, the default server, port 80, api authorities only on h3), `serveLocation` in nginx's phase order (405 or 404 without a location), `serveProxy` (its drain test, `nginx/forward.go:forwardUpgradeRequested`), `serveInProcess` (`boundUnreadBody` on h1; RawPath from the upstream target), `addLbRoutes`. `nginx/route_http_uri.go:routeUri.upstreamTarget` escapes only a `routeUri.quoted` path or one `routeHttpLocation.upstream` rewrote.

**Front: forwarder (D4, D7; §4.2).**
- `nginx/forward.go`: the request contract (`forwardRequestHead`, `forwardedForValue`, `forwardSkippedRequestHeaders`, `forwardResponseHeaders`).
- `nginx/upstream.go:upstreamGroup.serveHttp` picks a body's path: a 413 before any read for a declared length over the limit; on a streamed route a `forwardStreamedBody`, kept for an h1 body of declared length not expecting 100-continue, and `Content-Length: 0` for a declared empty one; else a stored body, on h3 with `absentWhenEmpty`.
- `upstreamGroup.forward` runs the attempts (`forwardAttempts.retryAllowed`, `retry503Allowed`, `next`); `relay` labels the block that answered and, when its copy fails, joins the request's write, counts a streamed body's refusal, then aborts; `refuse` answers a body refusal or, for a timeout on h1 and h2 or a client gone away, ends the request with no response, as `forward`'s `upstreamFailureClient` branch does.
- `nginx/upstream_conn.go`: `upstreamPool.dial` and its `pendingDials`, which `upstreamPool.close` waits for; `upstreamGroup.exchange`, whose write goroutine cuts the block's connection when the client's streamed body failed; `upstreamExchange.writeRequest`, the head flushed before the body; `stopBody`.
- `nginx/upstream_balance.go` (least_conn, round robin, nginx's Ketama ring); `nginx/forward_tunnel.go:upstreamGroup.tunnel`; the `urnetwork_lbalt_*` metrics in `nginx/upstream_metrics.go`, `urnetwork_lbalt_forward_body_refusals_total` by reason among them (`forwardBodyRefusalsCounter`, its help corrected).

**Front: bodies (D7, D19, D21).** `nginx/body.go:bodyStore.open` reserves the memory prefix before the first read, or at the first bytes under `bodyReadOptions.absentWhenEmpty` (§11.3 (c)); `reserveSpillFile` and `reserveSpillBytes` hold the spill caps; `createTempSpillFile` unlinks each file at creation; `storedBody.release` runs once and waits for a spill file being created. `bodyStore.read` and `nginx/body_stream.go:forwardStreamedBody.Read` refuse a declared body that ends short (`bodyRefusalIncomplete`), and the latter bounds each read and keeps a whole first read (`kept`; §11.7 item 4). `nginx/body_settings.go:BodySettings`: 16 MiB a body, a 64 KiB prefix, a 256 MiB budget, 4 GiB and 1,024 spill files, 15 s between reads.

**Front: limits (D8).** `nginx/limit.go:frontLimits.acquireRequest` charges the bucket, then the concurrency cap, and releases once; `acquireQuicConnection`; `addrStateWithLock` evicts the least recently used idle state at the cap; `limitInterface`, an interface's zones and `exclude_subnets`. `nginx/response.go:responseState.holdSlot` keeps the slot across a hijack.

**Front: responses (§4.2).** `nginx/response.go:responseState`: `writeHeader` adds the location's headers, `Connection: close` while draining and at the keepalive limits, and on h1 `endsConnection`; `armWriteDeadline` applies send_timeout per write; `start` wraps every h1 body in `responseRequestBody` (the 100 Continue's deadline, `requestBodyUnread`); `finish` marks an h1 response that ends its connection over an unread body, for Go's half-close; `gzips` decides a HEAD as a GET, by nginx's content type (`contentTypeMediaType`), and for a HEAD `startGzip` nils the Content-Length and starts no writer. `nginx/response_headers.go:responseHeaders.apply` follows add_header and `always`.

**Front: public servers (IMPL §7).** `nginx/front_server.go:Front.ConfigureServer` sets every HttpSettings-derived field, `MaxHeaderValueCount` and the h2 limits included; `Front.lastRequestOnConnection` applies keepalive_requests and keepalive_time; `connect/front_http.go:frontServer.newHttpServer` sets Handler, ErrorLog and BaseContext, then calls it.

**Front: drain and close (D12).**
- `nginx/front.go:Front.BeginDrain` arms the DrainTimeout cut; `Front.Close` is idempotent, answers new requests 503 and is bounded by ForcedCloseTimeout; `Front.cut` runs every close on its own goroutine.
- `connect/front_drain.go:drainConnect`: latch, front and exchange drains, wait, forced close, cancel. `frontServer.beginDrain` (its context from `context.WithoutCancel`) leaves each TCP server's part to its `Shutdown`, off its goroutine; `frontServer.close` cuts every public TCP socket (`frontServer.closeTcpConns`) before the `http.Server.Close` loop and reports only steps not done; `connect/front.go:frontServer.start` counts every supervisor before the front reads as started (`FrontServerSettings.startedObserverForTest`).
- h3: `connect/front_quic.go:frontQuicRun.beginDrain`; `addApiServer` refuses api connections while draining; `close` closes the api servers before the transport. The vendored fix: `third_party/quic-go/http3/server.go:Server.handleConn`.
- TCP: `connect/front_http.go:frontTrackedConn`: `Write`'s latch and its bound by `sendTimeout`, `SetWriteDeadline` and `SetDeadline` for the owner's deadline (§11.3 (g)), and `CloseWrite` for net/http's half-close.

**Front: metrics and access log (D17).** `nginx/route_http_metrics.go:frontMetrics`: `urnetwork_front_requests_total`, `urnetwork_front_response_header_seconds` and `urnetwork_front_limit_refusals_total` by service, protocol, status class and block, and `urnetwork_front_certificate_expiry_timestamp_seconds` (`frontMetrics.certificateExpiry`), unset until `NewFront`; `Front.redact` (hidden prefixes as `<prefix>`); `glogFrontAccessLog`, one request in `AccessLogSampleInterval` at V(1). `connect/front_metrics.go`: the `urnetwork_connect_front_*` series; `connect/front_http.go:scrubFrontHttpPanic` keeps client addresses out of the servers' logs.

**Connect: sockets (D10, D11, D22).** `connect/front.go:newFrontSockets` lists every interface, family and port kind, `SocketsPerPort` times; `frontServer.start` binds them all or fails, and sets `Allow0RTT = false`. `connect/front_settings.go:listenFront` and `listenFrontPacket` bind the exact bridge address with `server.SoReusePort`. `connect/front_socket.go:frontServer.superviseSocket` re-creates a lost socket at once, then after 100 ms doubling to 5 s (reset after 1 m healthy), never once draining. `connect/front.go:frontServer.startWithRouter` serves connect names in-process through `startWithConnectRoutes` and `connect/connect_router.go:newConnectRoutes`, which `run.go`'s private router mounts too (§11.3 (f)).

**Connect: QUIC dispatch (D3, D9; §4.4).** `connect/front_quic.go:frontServer.openQuicSocket` reads the socket directly, with no PROXY reader; whodis goes through the connect module's `connect.NewPacketTranslation`, and the transport comes from `ConnectHandler.NewQuicTransport`, with the shared handshake limit. `frontQuicRun.dispatch`, by server name: `serveConnect` and `serveApi` charge the per-address cap after the handshake (0x1001), `serveApi` with an `http3.Server` per connection from `frontServer.newApiServer`; `refuseUnknownName` closes another served name with 0x1000. The header phase: `third_party/quic-go/http3/server.go:Server.ReadHeaderTimeout`, which `third_party/quic-go/http3/server_conn.go:RawServerConn.handleRequestStream` applies as `RawServerConn.readHeaderTimeout`, and `readHeaderBlock`.

**Ingress headers (§4.4.2).** `nginx/forward.go:forwardSkippedRequestHeaders`; in-process, `routeHttpHandler.serveInProcess` calls `router.SetIngressHeaders` and deletes Early-Data, which `router/ingress.go` now clears for every caller.

**Monitor (D17).** `monitor/lbalt_status.go:lbAltStatusRequests` expands every enrolled interface and family to the lb route and every block's route. `monitor/lbalt_status_transport.go:observeLbAltStatus` bounds each attempt by AttemptTimeout, dials under the request's context, and ends an abandoned TLS handshake with its deferred `CloseIdleConnections`; `observeLbAltStatusWithin` and `monitor/public_udp_transport.go:observePublicUdpWithin` cap attempts by the command timeout, behind the seams `monitor/conn.go:runner.lbAltStatusDial` and `runner.publicUdpListen`. `observePublicUdp` requests /hello for connect/api; `monitor/public_udp.go:DefaultPublicUdpProbeSettings` allows 64 rows, and `publicUdpRequests` keys a path by its port. `lbAltStatusEvaluate` and `monitor/signal_public_udp.go:publicUdpFindings` report `scope-excluded` (`hostScopeOnlyError`), and the latter holds `public-udp-application` while the handshake fails.

**Docs that changed:** LBALT.md, LBALT-IMPL.md, `nginx/doc.go`, monitor/SIGNALS.md §16.9 and §16.10, third_party/quic-go/URNETWORK.md; warp's README.md and warpctl/VYOS.md.

### 11.3 Departures from the design

Each with its reason. A departure that also makes the front differ from nginx is described in §11.4 and only named here.

**Final review, for the owner to confirm**; the decision text of §1 to §10 is left as decided.
- (a) **QUIC with a missing or unknown SNI.** §3.3, the §4.1 row "UDP 443 (h3) | anything else", D3 and D9 refuse it with 0x1000. The code fails its TLS handshake (CRYPTO_ERROR 0x150, internal_error), as alt does and LBALT-IMPL §2.8 says, since the QUIC config has no certificate for it and 0x1000 needs a completed handshake; only a served name other than connect and api gets 0x1000.
- (b) **D8 extended to h3 api connections** (LBALT-IMPL §2.8). D8 keeps alt's per-address QUIC cap for connect. An api connection is now charged to it too after its handshake (`net_connections`, 0x1001), idle ones included, and a request stream's HEADERS must arrive within client_header_timeout (H3_REQUEST_INCOMPLETE). Before, one address could hold any number of api connections, each with up to 100 header-less streams and about 7 MiB.
- (c) **h3 bodies reserve at their first bytes** (08ed6a81b). D21 reserves "a body of unknown length ... before its first read", and D7 and D19 answer 503 before reading the body. The vendored http3 gives every h3 request without a content-length, GETs included, a body of unknown length, so reserving first refused bodiless h3 GETs under a full budget, which h1, h2 and nginx's h3 serve. Such a body now reserves once a read returns bytes, and its 503 comes after that read.
- (d) **The lb_alt enabling rule** (LBALT-IMPL §2.2; warp 81b9bb9). A host is lb_alt when connect declares lb_alt, `true` or `false`, is placed there, and has a non-transparent lb interface; no override or default value counts. Under the old rule D2's canary, `true` over a declared-off default, redeployed the host's connect blocks one by one while the split already ran, so the first ready took every bucket and nginx was retired behind it; the rollback coupled the same way. Now declaring lb_alt redeploys connect once, and later switches change only lb units. §8 step 5's canary order becomes:
  1. declare `services.connect.lb_alt: false` and let every connect block deploy;
  2. set the canary interface's override `true`;
  3. roll back with the override `false`;
  4. remove the declaration only after every interface is back on nginx and its split is removed, since that edit still changes connect's units.
- (e) **The first switch waits for every block** (LBALT-IMPL §2.5; warp 2dc67af, d7ee30b). D16's amendment sends a first switch's flows to all five blocks at once, but the split took whichever were ready. Now a family whose nginx still listens behind its DNAT switches only once every block of positive weight is ready, nginx keeping new flows meanwhile; a retired nginx's DNAT holds nothing. This also covers declaring lb_alt and switching in one edit. Load also refuses (LBALT-IMPL §7) block chain names over 28 characters, whose promotion would never complete; front interfaces whose resolved keepalives differ for a service with tcp port 80 and none of its own, whose render would fail; and services named `lba` or `lbt`, alone or before a `-`.
- (f) **Connect names reach connect through its own routes** (de51d9975). §4.1 and LBALT-IMPL §2.7 serve connect names in-process, and the units passed any method and path to `ConnectRouter.Connect`. Now only GET / reaches connect, the websocket and H1+ upgrades included; another path is 404, another method on / 405 with `Allow: GET`, and cookies are dropped, before connect counts or rate-limits anything, as a block's private router answers behind nginx.
- (g) **Writes without a deadline end at send_timeout** (LBALT-IMPL §7; b036731cf, d522f11f4). The contract failed only writes after one that timed out. Now, on connect's public TCP sockets, a write its owner left without a deadline, hijacked or not, fails once none of it has gone out for send_timeout, as nginx bounds a blocked client write; an owner's deadline keeps it. Before, a TLS 1.3 client that sent KeyUpdate requests and stopped reading held such a connection, and every drain of its block, for good, and an unauthenticated websocket its D8 slot.
- (h) **A certificate outside its validity window serves** (LBALT-IMPL §2.7; c003371e6). D6 asks for complete, matching pairs with SAN coverage; the units also refused a leaf not valid now, which would stop every connect block of an lb_alt host at its next start over one name. Neither nginx nor warpctl reads a NotAfter, so the leaf now serves, logged at error level, and `X-UR-Front-Cert-Expiry` and `urnetwork_front_certificate_expiry_timestamp_seconds` carry the earliest NotAfter, lapsed or not. Nothing alerts on them yet (§11.6).

**LBALT-IMPL §5 and §7** are built as written, in the code §11.2 names and in `nginx/route_http.go:routeServiceStatusStandard`, `nginx/upstream_balance.go` with `services.LbAltBucketBlocks` (weight 0 gets no requests and no bucket), `services.LbAltService`, `services.LbAltBucketCount`, `newNameMatcher`, `routeHttpHandler.serveProxy` (`forwardOptions.host`), `lbAltDefaultWhodisTld` and `monitor/signal_lbalt_status.go:NewLbAltStatusSignal` (`lbalt-status-inventory`, `lbalt-status-path`, `lbalt-status-observation`). They differ from the text in:
- MaxHeaderBytes and MaxHeaderValueCount come from settings, `ServerHeaderBytesMultiple` and `ServerHeaderLinesMultiple` (2 each), since CODESTYLE allows no package-level tunable.
- 443 is refused as the lb_alt service's http or tcp stream port in every version once any version declares lb_alt: a port block keeps its lb type across versions, so a per-version check loaded files that then panicked in `portBlocksOf`.
- `-` is refused in every lb interface name of an lb_alt host, transparent ones too, since the chain names split on it.

A count-only check found every env's services.yml loads, before the final review's load refusals. Only the lb_alt service's own `tcp_stream_ports` are refused (§5), so an lb-level stream port closes when its interface switches (§11.4).

**At the integration merges.**
- The shared QUIC stateless reset key (d7f3643a0) was reverted (a29d3cd2d), since sibling sockets sharing one key are reset oracles for each other (RFC 9000 §21.11): a flow moved by a socket loss or a NAT rebinding reconnects after its idle timeout, about 20 s (D22 (a)), and D22 (b), one transport per group behind a fan-in, is the follow-up.
- A stored body's release waits for its releaser and for a spill file still being created, so `Front.Close` never reports done with one open; a forwarded request counts under the block that answered.
- Accepted into LBALT-IMPL §4 (2ff1790c2, 85101757a): the implicit `Connection: keep-alive`, the HTTP/1.0 status line, Go's AES-128-GCM choice, the default certificate for an SNI over 255 bytes, Content-Length beside chunked, the client's hop-by-hop headers in-process, and a streamed chunked body sent chunked.
- Go's h1 server refuses two Host headers with 400, as nginx does (4285c3007); the harness servers come from `Front.ConfigureServer` (6efe5fbad); the forced close reports only the steps not done (0e9138ed1); the netns harness requires a rollback to keep every block chain (cd6963a); the parity fixture's render is pinned to golden copies (4cb3677).

**Round 2** replaced unit behavior that misread nginx (what still differs is in §11.4): a stalled body ends the request with no response (h3 keeps the 408), and a streamed body failing mid-relay cuts the block's side and aborts the response; a streamed route sends the head at once and lets the body's first read decide what is kept, so D7's "ineligible for retry once forwarding begins" holds for the bodies nginx itself streams; gzip decides HEAD as GET; an h1 answer that ends its connection over an unread body takes Go's half-close and 500 ms wait (`responseState.finish`), and in-process answers bound Go's read of the body; a pool waits for a dial in flight; and `observeLbAltStatus` dials under the request's context, which carries the attempt bound.

**Found while building.**
- 0x1000 and the 0x1001 cap wait for the handshake, so the code reaches the client (RFC 9000 §10.2.3) and a spoofed source takes no victim's slots; alt closed at once.
- Each api QUIC connection has its own `http3.Server`, so a stuck handler holds only its own connection's close.
- The vendored http3 ends a connection's loop on an accept failure once a graceful GOAWAY is sent, rather than start a GOAWAY writer per pass (alt's 700k-goroutine burst); third_party/quic-go/URNETWORK.md records it and the header phase.
- The drain deadline is 58 m, DrainTimeout less DrainStopReserve: warpctl stops with `docker stop -t 3600`, and SIGKILL must not land before the forced close and the final stats flush.
- The forwarder has its own HTTP/1.1 client over `net/http.ReadResponse`: `http.Transport` may resend an idempotent request, a third attempt, and has no per-operation deadlines.
- Port ownership reads live state where §4.7 asks the allocator to record it: a pool port is taken while a socket holds it or the block's lb_alt chains, read from iptables, target it; the trusted UDP listeners hold a generation's ports while its public sockets are re-created.
- `ss -Htuln` replaces `netstat -tuln`, which truncates long IPv6 addresses.
- HMARK is written `--hmark-src-prefix 32|128`, as `iptables -S` lists it: libxt_HMARK stores `--hmark-tuple src` as a 16-byte mask, so an IPv4 rule added with it can't be deleted by its listed form.
- Block chains are replaced in place; only the split chain is rebuilt under the LBT name, since repointing it is the lb unit's alone.
- The split record lets an nginx lb unit remove a split only where one was made.
- Connect workers rewrite their block chains for the committed generation every 30 s, on every bound interface, so a later switch needs no connect deploy.
- A failure after `cutover.commit()` leaves a pending promotion, not an untracked candidate per retry.
- An lb_alt lb unit gets `--transparent=false`, which docopt requires beside `--rttable`.
- The route render refuses a keepalive time that isn't whole seconds and, after load now, interfaces that give a service different keepalives; it renders no env aliases, since the lb image is built without `--envalias`.
- SAN coverage is checked when a connect generation loads its certificates, so a miss shows as a candidate that never starts.
- Only service connect with front api requests /hello; alt's api front stays handshake-only.
- Public UDP sockets are raw `*net.UDPConn`, so quic-go uses GSO, ECN and OOB, which the trusted listeners' PROXY wrapper turned off; `QUIC_GO_DISABLE_GSO` and `QUIC_GO_DISABLE_ECN` are the escape hatches.

### 11.4 Accepted nginx differences

One list, from LBALT-IMPL §4, the units', round 2's and the final review's parity decisions as their reviews corrected them, and the parity harness's entries, checked against the code at a297487df. An id in brackets is the `nginx/parity_compare_test.go:parityAcceptances` entry that accepts it in a harness run; an item without one is outside what the harness compares. LBALT-IMPL §4 holds the same list, with what the front reproduces.

**TLS**
- Groups: X25519MLKEM768, then nginx's X25519, P-256 and P-384. TLS 1.2 suites: ECDHE with AES-GCM, where nginx also allows finite-field DHE, which Go lacks and nginx can't choose with ECDSA certificates.
- Within those, Go picks AES-128-GCM where OpenSSL under `ssl_prefer_server_ciphers` picks AES-256-GCM; crypto/tls ignores a server's order and has no TLS 1.3 suite setting [tls-suite-choice].
- Session tickets: Go's per-process keys, for TLS and QUIC, shared by every socket of a block, where nginx runs `ssl_session_tickets off` with a per-interface session cache (D6c).
- ALPN: connect names get http/1.1 only, where nginx offers h2 [connect-alpn]. An offer of only http/1.0 or http/0.9 gets no_application_protocol, where nginx selects it.
- An SNI ending in a dot fails the handshake with decode_error, since crypto/tls refuses the ClientHello before GetConfigForClient, where nginx drops the dot; RFC 6066 forbids it.
- An SNI over 255 bytes gets the default server's certificate and is routed by Host, where OpenSSL refuses the handshake [long-sni-default-certificate]. On QUIC it is an unknown name.
- A served name its certificate's SANs don't cover (a wildcard needs its own `*.x` SAN) fails the block's start; nginx would serve it.
- Against warpctl's lookup: a version tie goes to the greatest directory name, symlinked version directories are ignored, the flat-file fallback is not ported, and the host's flat `<vault>/tls` is searched last.

**Request parsing and limits**
- Header limits match nginx's totals (414 for a request line over 8 KiB, 400 for a header line over 8 KiB or a head over 64 KiB) but not its packing: nginx packs whole lines into eight 8 KiB buffers, so from about 33 KiB, with lines just over 4 KiB, it refuses some heads the front takes [header-buffer-packing].
- Past MaxHeaderBytes (2 × 64 KiB) and Go's 4 KiB read slack, about 132 KiB, Go's server answers 431, where nginx answers 400, or 414 for a long request line [max-header-bytes-431].
- max_headers (1,000) is counted as nginx counts it, but on h1 the count also takes in two lines Go's server may make up: the `Cache-Control: no-cache` net/http's fixPragmaCacheControl adds beside a lone `Pragma: no-cache`, and the Host of an HTTP/1.0 absolute-form request sent without one. A head of exactly 1,000 lines with either is a 400 nginx serves; discounting would pass the 1,001-line browser-reload head, which sends both. Past 2,000 lines or h2 fields (MaxHeaderValueCount), Go answers 431 where nginx answers 400.
- Content-Length beside `Transfer-Encoding: chunked`: Go reads the body as chunked (RFC 9112 §6.3) and the front forwards it re-framed, where nginx answers 400; each request reaches the upstream once, so nothing is smuggled [content-length-beside-chunked].
- Go's h1 and h2 servers and the vendored http3 merge repeated identical Content-Length values before the handler, and the front forwards the request framed by that length, where nginx answers a second Content-Length 400 on h1 and h2; nothing is smuggled [repeated-identical-content-length]. Different values: 400 on h1, as in nginx, and a stream reset on h2 (PROTOCOL_ERROR) and h3 (H3_MESSAGE_ERROR), where nginx's h2 answers 400.
- Go's h1 server answers a repeated Transfer-Encoding 501, where nginx answers 400, and an Expect other than 100-continue 417, which nginx ignores.
- An obs-folded header line is unfolded by Go's server, and the front forwards its parts trimmed and joined by a space, where nginx answers 400 [obs-fold].
- An HTTP/1.0 request's Transfer-Encoding is dropped by Go's server, which reads it as bodiless; bytes sent as its body become the connection's next request, forwarded as its own, each framed by its length, where nginx answers 400 [http10-transfer-encoding].
- The limit bucket matches limit_req decision for decision, except below nginx's 1 ms and milli-request granularity (`TestLimitBucketDecidesAsNginxLimitReq`).
- At `LimitSettings.MaxAddressStates` the least recently used idle state is evicted, as nginx frees a full zone's oldest nodes; with every state in flight, a new address gets 429, as limit_conn does.
- In a deploy overlap each generation keeps its own limit state, so a client may briefly get twice its limit (D8).

**Routing and redirects**
- No slash auto-redirect: a prefix location asked for without its slash gets 404, charged to the limits, where nginx sends an uncharged 301 whose Location carries the internal listen port [slash-redirect].
- Names and redirect targets are lowercase in the route file, so a redirect's Location can differ from nginx's in case.
- On h2 and h3, a Host that differs from `:authority`, byte for byte, is a 400; the harness compares distinct and equal names only.

**Responses and errors**
- No `Server` header [server-header].
- The front's own answers are the status text as text/plain, where nginx writes HTML pages; the status codes match [own-error-page].
- Status routes send one `Content-Type: application/json`, where nginx sends two; on the port-80 per-block status the front replaces the upstream's only on 2xx and 3xx [status-content-type].
- The lb status JSON adds `"front":"lbalt"` [lb-status-json, lb-status-head-length]. It has no Last-Modified, ETag or Accept-Ranges [lb-status-static-file], so a Range or conditional request gets 200 [lb-status-range].
- A CORS preflight's 204 has no `Content-Length: 0` [preflight-content-length].
- A 101 connect writes in-process carries none of nginx's `always` headers [in-process-switch-headers].
- A persistent HTTP/1.1 response leaves `Connection: keep-alive` implicit [keepalive-connection-header].
- An HTTP/1.0 request gets an HTTP/1.0 status line, where nginx writes HTTP/1.1 [http10-status-line].
- gzip's bytes differ, Go's flate against zlib; the decoded body and the headers match [gzip-bytes].
- A second value of a single-valued upstream header passes, where nginx drops it.

**Forwarding and bodies**
- A websocket service gets one `Connection: upgrade` upstream, with `Upgrade` only when the client sent one, where nginx sends `keep-alive` and `upgrade` [websocket-upstream-connection].
- Connect in-process sees the client's own hop-by-hop headers and writes its own 101 head, where behind nginx it sees nginx's `keep-alive` and `upgrade`; an Upgrade without `Connection: upgrade` is upgraded behind nginx only [in-process-hop-by-hop].
- The `Trailer` request header is dropped, where nginx passes it [trailer-request-header].
- Client headers go upstream sorted by name, in Go's canonical case.
- net/http's fixPragmaCacheControl adds `Cache-Control: no-cache` beside a `Pragma: no-cache` sent without one, on h1 requests the front forwards and on upstream responses it relays; nginx passes only Pragma.
- A 101 to a request that asked for no upgrade is a 502, never retried, where nginx relays it.
- Keepalive pools, balancing state and max_conns are per block per process, where nginx's are per worker; the harness runs nginx with one worker so least_conn compares.
- A reused keepalive connection that fails before any request byte is written is redone on a fresh connection to the same block, as no attempt; after the write it is one of D7's attempts and moves only to another block, so a single-block service or a per-block lb route answers that race 502, where nginx gives any error on a cached connection a free try and, with one server, redoes the request there.
- Bodies are stored under process caps nginx lacks (D7, D19, D21): a body that can't reserve is a local 503 before its first read, counted apart from upstream 503s and never retried. Spill files are unlinked at creation, where nginx keeps named temp files to the request's end.
- An h3 request without a content-length reserves at its first bytes (§11.3 (c)); until then that read's bytes sit in the pooled read buffer (`BodySettings.ReadBufferBytes`), outside the budget.
- On h3, a request without a content-length streams chunked on a streamed route and isn't retried, even when its stream ends with no data; on a buffered route, a stream that ends empty only after a wait goes unframed, where nginx sends `Content-Length: 0`. nginx peeks at the stream without waiting; the vendored http3 can't, and reading before the head would hold a client that sends its body only once the response begins.
- Streamed routes: the request head goes upstream at once, and the body's first read decides, as nginx's first pass does. An h1 body of declared length that one read returns whole, as Go's server returns one it holds whole with its head, is kept in memory under the body budget, sent with its Content-Length and `X-UR-Request-Buffering: off`, and retried under D7. A declared empty body is retried on every protocol. h2 and h3 bodies with data, and bodies expecting 100-continue, stream unkept, as nginx streams them. Unlike nginx:
  - a chunked body streams chunked though it came whole with its head, where nginx sends its length and retries a 503 [streamed-chunked-whole];
  - a declared body one read doesn't return whole streams unkept: past Go's 4 KiB server buffer less the head when it came with its head, else past `CopyBufferBytes` (32 KiB), where nginx's first pass holds up to 1.25 × client_body_buffer_size (80 KiB);
  - a body that arrives whole only after its head may be kept and retried where nginx streams it;
  - a block that answers before the first read returns finds nothing kept, and a body with no room in the budget streams, unrefused;
  - on h2, a declared empty body whose stream ends after its HEADERS frame is retried, where nginx streams it.
- A service both websocket and streamable has its bodies streamed, where nginx buffers them; no such service is in main.

**Timeouts and connections**
- A body that stalls past client_body_timeout gets no response, as nginx terminates the request: on h1 the connection closes with nothing written, before any block or while a streamed body is forwarded, a relayed response included; on h2 only the stream is reset. Unlike nginx, Go's h1 server sends TLS close_notify first, where nginx's quiet shutdown sends none; the h2 reset is INTERNAL_ERROR, nginx's PROTOCOL_ERROR, since Go's server takes no reset code from a handler; h3, which nginx doesn't serve here, answers 408.
- send_timeout bounds each write as a whole, a handler write or flush of at most 32 KiB and on h2 each frame (through the write byte timeout, which closes the connection), where nginx re-arms it on any progress, so a client reading slower than about 1 KiB/s is cut by the front and kept by nginx.
- Lingering close. nginx discards an unread body after any answer of its own for up to lingering_time (30 s), each read waiting at most lingering_timeout (5 s), then keeps the connection or, for a closing answer, shuts its write side and discards until EOF or 30 s. Go's h1 server has no lingering_time: it reads up to 256 KiB of the body within lingering_timeout, before an answer that keeps the connection and after one that closes it, and when the connection then closes with the body unread it half-closes (a FIN on port 80, close_notify on 443) and closes 500 ms later, as the front ensures in every such case. So:
  - the answer to a stalled body comes after lingering_timeout, where nginx answers at once, for the front's own answers, the forwarder's refusals and connect's in-process answers alike;
  - a client still sending 500 ms after its answer gets a reset where nginx goes on discarding: one that reads while it writes (Go's client, curl, browsers) has the answer first, one that reads only after writing its whole body loses it once the upload outlasts 500 ms and the socket buffers;
  - after an answer that keeps the connection (403, 404, 405, 429, 503, 301, the preflight's 204, the lb status 200), nginx keeps it however large the body; Go closes it when 256 KiB or more is left or the rest doesn't arrive within lingering_timeout;
  - a client expecting 100-continue that sends its body anyway after its own wait (curl after 1 s, Go's client after ExpectContinueTimeout), within lingering_timeout, keeps its connection on nginx, where Go closes it after the answer, as it closes one it answers with an expected body unread; one that sends nothing is closed by both after about lingering_timeout;
  - a forwarded answer that read its body whole and closes the connection while a pipelined request is arriving is closed at once by Go, so the client sees a reset after the answer, where nginx lingers; neither serves that request;
  - in-process connect: nginx buffers connect's body under client_body_timeout and ends a stalled one with no response, where the front gives connect's own answer, with Connection: close, once lingering_timeout cuts the read.
- keepalive_requests and keepalive_time on h2: Go sends the GOAWAY with the last stream's response head, nginx as that stream opens. With keepalive_timeout 0, Go's GOAWAY comes once the connection is idle, nginx's at the first stream; production uses 1 m.
- keepalive_requests counts only requests that reach the front's handlers. On h1 those Go answers itself (431, 400, 501, 417) close the connection anyway; on h2 a stream Go answers or resets itself isn't counted and leaves the connection open.
- A request in flight at the forced close can get a 502, where nginx closes with no response: the front's cut ends its forward while connect cuts the client's connection (`frontServer.closeTcpConns`, each QUIC run's close), neither waiting on the other, so the 502 reaches the client only when it is written first.
- An h1 upgrade after the drain begins gets 503, any request the forwarder would tunnel included; nginx closes idle connections at SIGQUIT and never sees one.
- The drain deadline is 58 m, against nginx's 60 m worker_shutdown_timeout (§11.3).
- The listen backlog is Go's (somaxconn), where nginx used `backlog=65535`.

**h2 and h3**
- A connect name over h2, reached only by a client that forces h2 past ALPN, is served in-process with a writer that can't hijack, so connect answers as behind nginx, which forwards h2 without upgrade fields.
- h3 has no nginx counterpart here. It follows alt, and D3 and D9, except: a non-api authority gets 421; `/status`, and a path with no location whatever the method, get alt's 404, after the limits; nginx's method rule applies; the idle timeout is keepalive_timeout and heads are limited to 64 KiB, where alt used none and 1 MiB; connections count against the per-address cap and a stream's HEADERS against client_header_timeout (§11.3 (b)); a body short of its content-length is a 400; keepalive_requests and keepalive_time don't apply; a resumed session reports `Used0RTT == false`.
- No response carries Alt-Svc (D3).

**Metrics and logs**
- The access log writes one request in 100 at V(1), with no query, no client address and no hidden prefix, where nginx logged every request (D17).
- A request ended for a body timeout, or because its client went away, counts under status class `none` in `urnetwork_front_requests_total`, and its access log line has status 0, where nginx logs 408 or 499.
- warpctl's lb version polls count `"front":"lbalt"` samples as neither a version nor an error, and don't poll when no lb interface is left on nginx.

**Ports and packet paths** (warpctl's)
- An lb_alt interface publishes exactly the IMPL §2.3 table: udp 4053 at the host and the router, and no udp 80, where nginx publishes every lb port and mapped stream port, udp 80 included, and keeps 4053 private. An lb-level stream port closes when its interface switches, and VyOS's later rules move up one stride.
- HMARK hashes the whole source address, /128 on IPv6, so an IPv6 client's rotating addresses can land on different blocks.
- With every block of an interface down no public port answers, TCP 80 included, so Route53 withdraws the interface; an nginx interface whose upstreams are down still answers its own status route.
- With `tcp_fwmark_accept` or `fwmark_reflect` on, the split's TCP replies and the kernel's resets carry their bucket mark; no fwmark rule selects one, so they leave by their interface's table.
- During a rollback, between nginx's redirect and the split's removal, public udp 4053 still reaches connect's whodis; an interrupted removal leaves that in one family until the 30 s retry.
- Conntrack cleanup stays UDP-only; a TCP flow to a dead target gets a reset and redials.

**Not differences**, though reported as such: two Host headers on h1 (both 400); a key that doesn't match its certificate (both refuse it); the front's own `Date` (nginx hides the upstream's too). The final review matched nginx on a client gone away (no response, where the front sent an unseen 502), a lapsed leaf (served), an h3 body short of its length (400), and a path with no location (405 to a method other than GET, HEAD and POST).

### 11.5 Test map

**Results on the final merged tree** (server a297487df, warp bdacae4), 2026-11-19, all passing, with the commands below:
- server: gofmt and both vets clean; `./nginx/` 29.7 s, its race run 50.7 s; connect `-race -run '^TestFront'` (71 tests) 15.2 s, and the five other non-DB front tests 1.0 s; monitor 7.3 s, its race run 8.6 s; router `Ingress` 0.9 s; the vendored quic-go commands, about 1 s each.
- server, DB-backed `-run "LbAlt"`: 21 top-level tests, 328.9 s.
- parity: "257 cases; 0 differences no entry names", 76.6 s. Differences accepted, by entry: connect-alpn 2, content-length-beside-chunked 90, gzip-bytes 6, header-buffer-packing 38, http10-status-line 3, http10-transfer-encoding 60, in-process-hop-by-hop 18, in-process-switch-headers 4, keepalive-connection-header 2, lb-status-head-length 4, lb-status-json 60, lb-status-range 5, lb-status-static-file 68, long-sni-default-certificate 34, max-header-bytes-431 5, obs-fold 30, own-error-page 606, preflight-content-length 17, repeated-identical-content-length 58, server-header 517, slash-redirect 12, status-content-type 43, streamed-chunked-whole 2, tls-suite-choice 3, trailer-request-header 1, websocket-upstream-connection 13.
- warp: gofmt and both vets clean; warpctl 107.1 s, services 0.8 s.
- netns: PASS, the eight edge tests, 99.6 s.
- perf, the lb_alt path only (`TestConnectPerformanceCarriersLbAlt`, in the DB-backed run; the best of three runs, on a darwin dev machine at a load average near 38): websocket 181,733 msg/s, echo p50 0.37 ms; H1+ 158,201 msg/s, 0.42 ms; QUIC 8,741 msg/s, 0.56 ms; whodis 19 msg/s, 56.7 ms. `lbalt/s-car`'s run gave 155k, 179k, 8.2k and 18 msg/s, within the spread between runs (websocket's three: 182k, 154k, 91k). The direct and nginx paths weren't rerun.

**Results on the fix branches**, from the fixers' reports; the two server branches touch disjoint files and never ran each other's packages, and a297487df ran on neither.
- `lbalt/final-nginx` at 7c492d5a9: gofmt and vet clean; `go test ./nginx/` 29.4 s, the race run below 49.7 s; parity at 0844460ac, "257 cases; 0 differences no entry names", 76.2 s (later commits comments only).
- `lbalt/final-connect` at b1a1fce2d: connect `-race -run '^TestFront'` 15.6 s, monitor 8.2 s, router 0.8 s, the DB-backed LbAlt suite 376.8 s; vendored `./http3/...` ok at ceac2ee69, unchanged since.
- `lbalt/final-warp` at 3216b90, whose tree bdacae4 has: both vets clean, warpctl 104.1 s, services 0.5 s, netns PASS in 93.2 s.

**Commands.** From each repo's root, with `export GOFLAGS=-trimpath GOCACHE=$WARP_HOME/.gocache` unless a block says otherwise.

server, no database or Docker:
```
gofmt -l nginx connect monitor router
go vet ./nginx/ ./connect/ ./monitor/ ./router/ && GOOS=linux go vet ./nginx/ ./connect/ ./monitor/ ./router/
go test -count=1 ./nginx/
go test -count=2 -race -run 'Forward|Upstream|Body|E2e|Route|Response|Gzip|FrontServer|Tls|Limit|Name' ./nginx/
go test -count=1 -race -run '^TestFront' ./connect/                                                   # 71 tests
go test -count=1 -run '^Test(Front|RunStartsTheFront|LbAltWithoutTheRouteFileChangesNothing|LbAltRouteFile|LbAltStartServesTheRealFrontFromTheEnv)' ./connect/   # 76 non-DB front tests
go test -count=1 -run 'PublicUdp|LbAlt|LbStatus|SettingsFreshness|RegisteredSignals|EveryAlertClassIsClassified|TestEveryFixedProductionAlertClassIsCataloged$|ClearHold' ./monitor/
go test -count=1 -race -run 'LbAlt|LbStatus|PublicUdp' ./monitor/
go test -count=1 -run 'Ingress' ./router/
```
`go test ./monitor/` as a whole fails 239 tests on environment preconditions, as the base does, none of them lb_alt's, and `go test ./router/` as a whole needs the portable fixture env. Stress the upstream pool tests in batches: one `-count=1500` run exhausts macOS's ephemeral ports in TIME_WAIT, which shows as the front's own 502s.

server, database-backed (connect's `lbalt_*_test.go` and the perf harness, in `server.DefaultTestEnv`): the local PostgreSQL and Redis stack (`local/run-local.sh`), `test-env.sh` sourced from bash (zsh fails its authority check), and no `-trimpath` (it can't find the fixture vault):
```
bash -c 'cd server && export WARP_HOME=<urnetwork home> GOCACHE=$WARP_HOME/.gocache && source ./test-env.sh && cd connect && go vet . && go test -count=1 -timeout 30m -run "LbAlt" .'
#   320 to 380 s, TestLbAltRebuildsALostUdpSocket ~90 s of it (D22's idle-timeout reconnects)
bash -c '<as above> && cd connect && WARP_TEST_ENV_FAIL_FAST=1 go test -count=1 -timeout 40m -v -run "TestConnectPerformance(Carriers|NoContract)" .'
#   carriers ~3.2 min, no-contract ~40 s
```
`-run "LbAlt"` also selects the four non-DB route-file tests and `TestConnectPerformanceCarriersLbAlt`; `TestConnectPerformanceCarriersNginx` needs warp/lb's `make nginx_local` build (`NGINX_UDP_PROXY_V2_BINARY` or `../warp/lb/build/nginx-local`) and skips without it.

vendored quic-go, from its own module and without `-trimpath`, under which six upstream http3 tests fail:
```
cd third_party/quic-go && GOFLAGS= go test -count=1 -run '^TestUrnetwork' . ./http3/
GOFLAGS= go test -count=1 ./http3/...                    # the upstream suite
```

parity harness (server, Docker):
```
NGINX_PARITY=1 go test -count=1 -v -timeout 30m -run '^TestNginxParity$' ./nginx
```
It skips, saying why, without `NGINX_PARITY=1`, `docker` or the pinned lb image of `nginx/parity_test.go:defaultParityHarnessSettings` as linux/arm64 (never pulled), and stops under 25 GB free for `GOCACHE` or 4 GiB in the Docker VM. It runs `TestNginxParityContainer` in one container (`--network none --cap-add NET_ADMIN`, IPv6, `NGINX_PARITY_CONTAINER=1`, `WARP_ENV=test`): nginx through the image's lb binary, the front behind `ConfigureServer`'s servers, recording upstreams. `go test -count=1 -run 'TestParity|TestParseParity' ./nginx/` checks the accepted list without Docker; warp's `LBALT_PARITY_FIXTURE_OUT=<server>/nginx/testdata/parity go test -count=1 -run '^TestLbAltParityFixture$' ./warpctl` regenerates the fixture, or checks it against `testdata/lbalt_parity/`.

warp, no Docker:
```
gofmt -l warpctl services
go vet ./services/ ./warpctl/ && GOOS=linux go vet ./services/ ./warpctl/
go test -count=1 ./services/ ./warpctl/       # warpctl ~105 to 145 s
go test -count=1 -run 'LbAlt|PortBlocksOf|NginxConfigServices|Vyos|Dns|Route53|CleanupStaleConntrack|FindOccupiedPorts|ValidateRedirectPortOwnership|ActiveRedirectInternalPorts|PruneUnownedRedirectRules' ./warpctl/   # ~20 s
go test -race -count=1 -run 'LbAltWiring|LbAltWorker|LbAltLbVersionPoll|CleanupStaleConntrack|FindOccupiedPorts|LbAltRoute|LbAltSplit' ./warpctl/                                                         # ~15 s
```

netns harness (warp, Docker):
```
cd warpctl && WARPCTL_LBALT_NETNS=1 go test -count=1 -timeout 60m -run '^TestLbAltNetns$' -v .
```
It skips, saying why, without `WARPCTL_LBALT_NETNS=1` or Docker. It needs a VM kernel with xt_HMARK and conntrack (Docker Desktop's has both), about 200 MB and free TEST-NET subnets, so one run at a time; it builds its image once from `testdata/lbalt_netns` (`--pull=false`, ubuntu:24.04, iptables 1.8.10 on nf_tables, as on the edges) and gives its containers only CAP_NET_ADMIN and CAP_NET_RAW. `WARPCTL_LBALT_NETNS_BINARY` takes a prebuilt test binary, `WARPCTL_LBALT_NETNS_RUN` a pattern of the eight `TestLbAltNetnsEdge*` tests.

**By LBALT.md §9 item.** Server files unless marked warp; DB marks database-backed tests, netns the edge tests of `../warp/warpctl/lbalt_netns_linux_test.go`, parity the harness's cases.
- **Dispatch and refusals:** `nginx/route_http_test.go`, `nginx/front_e2e_routing_test.go`, `nginx/tls_config_test.go`, `connect/front_quic_test.go`; DB `connect/lbalt_refusal_test.go`; parity `tls/*`.
- **The forwarding contract and spoofed headers:** `TestForwardRequestContract`, `nginx/front_e2e_contract_test.go`, `router/ingress_test.go`; DB `TestLbAltH1IngressHeadersComeFromTheAcceptedPeer`; parity `ingress/*`, `uri/*`.
- **A PROXY header not honored:** `TestFrontPublicSocketIgnoresAProxyProtocolHeader`; DB `TestLbAltPublicSocketsDoNotHonorAProxyHeader`.
- **The 503 retry and the budget's refusal:** `TestRetry503ReplaysBodyByteForByte`, `TestForwardBodyBudgetIsLocal503`; parity `retry/503-*`. One address's share (D19) is bounded by the prefix and the per-address cap (`TestBodyStoreMemoryPrefixBoundary`, `TestLimitConcurrencyCapAfterTheBucket`), but no test fills one address's cap and reads the budget.
- **Route53's check:** parity `route53/*`; `TestFrontE2eLbStatusAndPort80`; netns `TestLbAltNetnsEdgeMovesOnlyOrphanedBuckets`, where tcp 80 refuses with every block down.
- **Limits:** `nginx/limit_test.go`, `nginx/route_http_limit_test.go`, `nginx/front_e2e_limit_test.go`; parity `limit/*`. No test sends one source over two reuseport sockets; the deploy overlap is accepted (D8).
- **Drain:** `connect/front_drain_test.go`, `TestFrontE2eDrainClosesConnectionsAndKeepsWebsockets`, quic-go's `TestUrnetworkGracefulShutdown*`; DB `TestLbAltDrainFinishesInFlightWorkAndHoldsWebsocketsToTheDeadline`; netns `TestLbAltNetnsEdgeKeepsEstablishedFlowsThroughAPromotion`.
- **The split, cutover, fallback, ownership and rollback:** warp `lbalt_split_test.go` (an in-memory iptables that applies restores atomically and refuses what the kernel refuses), `lbalt_worker*_test.go`, `lbalt_ports*_test.go`, `lbalt_wiring_test.go`; every netns test.
- **Perf (M4):** DB `TestConnectPerformanceCarriersDirect`, `TestConnectPerformanceCarriersLbAlt` and `TestConnectPerformanceCarriersNginx`, each with websocket, H1+, QUIC and whodis cases, and `TestConnectPerformanceNoContract`.
- **Port contract:** warp `services/lbalt_test.go`, `lbalt_edge_test.go`, `lbalt_edge_vyos_test.go`; netns `TestLbAltNetnsEdgeSplitsSourcesByWeight` and `TestLbAltNetnsEdgeSplitsAnIpv4OnlyInterface`; whodis on 53 and 4053, DB `TestLbAltUdpRepliesComeFromTheAddressAndPortTheClientDialed`; nginx interfaces' private forward target, warp `TestPublicPortServiceTargetsKeepsPreviousAliasTargetPrivate`.
- **Handover:** netns `TestLbAltNetnsEdgeRollsBackToNginx` (both modes on one host, flows kept both ways, an interrupted reconcile and removal, block chains kept, the first switch's wait); warp `lbalt_worker_units_test.go`, `TestLbAltWiringRoutingOnlyLoopRetiresNginxOnceTheSplitIsReady`, `TestLbAltWiringRollbackRemovesTheSplitOnceNginxDnatIsBack`; D2's three cases in `services/lbalt_test.go`.
- **Generations and chains:** the netns tests, warp `TestLbAltSplitWritersTakeTurnsOnTheHostLock` and `TestCleanupStaleConntrackDeletesOnlyRetiredGenerationsOnLbAltBridges`, and `TestLbAltNetnsHmarkModelKeepsTheKernelsBuckets`.
- **Readiness:** `connect/front_status_test.go`, `connect/front_socket_test.go`; warp `TestLbAltReadinessAcceptsOnlyAnExactlyReadyGeneration`, `TestPollLbAltReadinessRefusesADrainingGenerationAtOnce`, `TestLbAltWiringDeployWritesNoChainForAnUnreadyCandidate`.
- **Carriers** (DB): `connect/lbalt_carrier_test.go`, `connect/lbalt_session_test.go`, `connect/lbalt_socket_test.go`.
- **Retries:** `nginx/upstream_retry_test.go`, `TestFrontE2eHiddenBlockRoutesReportTheirBlock`; parity `retry/*`, `balance/ketama-retry-walk*`.
- **Body storage:** `nginx/body_test.go`, `nginx/front_e2e_close_test.go`; parity `limits/body-*`.
- **TLS:** `TestTlsConfigResumesSessionsAcrossSocketsSharingTheBase`, `TestQuicTlsConfigResumesSessionsAcrossSocketsSharingTheBase`, DB `TestLbAltSessionsResumeAcrossTheBlocksSockets`; SAN coverage, `nginx/tls_store_test.go`; loaded before readiness, `connect/front_load_test.go`.
- **Monitor:** `monitor/lbalt_status_transport_test.go`, `TestPublicUdpConnectApiFrontCompletesHelloOverHttp3`, `TestPublicUdpApplicationIncidentClearsOnlyOnACompletedHello`.

**Round 2's mechanisms.** The body timeout: `TestFrontE2eBodyTimeoutEndsTheRequestWithoutAResponse`, the `TestForwardStreamedBodyTimeout*` tests, `TestForwardBodyTimeoutResetsTheH2Stream`, `TestForwardStreamedBodyFailureCutsTheRelayedResponse`, `TestStreamedBodyTimeoutBeforeAnInterruptIsTheClients`, h3's `TestNewFrontH3BoundsEachBodyReadByTheClientBodyTimeout`, parity `timeout/*`. The first read: the `TestForwardStreamed*` tests, `TestForwardKeptBodyOutlivesTheExchangeItLeaves`, `nginx/body_stream_test.go`, parity `retry/503-streamed-post-sent-whole`, `retry/503-streamed-post-in-flight`, `balance/ketama-retry-walk-post`, `lb/by-service-mcp-streams`. The dial in flight: `TestUpstreamPoolCloseWaitsForADialInFlight` and its siblings, `TestFrontE2eCloseNeverReportsADialStillOpen`. gzip on HEAD: `TestRouteHttpGzipsAHeadOverEveryProtocol`, `TestFrontE2eGzipsAHeadAsAGet`, parity `gzip/html-head`. The half-close: `nginx/front_e2e_linger_test.go`, `TestFrontHttpHalfClosesAConnectionItEndsOverAnUnreadBody`, `TestRouteHttpConnectInProcessBoundsTheReadOfAnUnreadBody`. The lb status bound: `TestLbAltStatusTransportBoundsTheAttemptByItsSettings`, `TestLbAltStatusNativeAttemptIsBoundedWithinTheCommandTimeout`.

**The final review's tests**, 49 new test functions in server and 15 in warp, two of them replacing removed ones, each failing without its fix under the fixers' and re-reviewers' overlay mutations.
- `nginx/front_e2e_client_gone_test.go`: `TestFrontE2eClientGoneMidBodyGetsNoResponse`, `TestFrontE2eClientResetMidBodyIsRecordedWithNoStatus`, `TestFrontE2eClientGoneWhileTheBlockHoldsTheAnswerIsRecordedWithNoStatus`.
- `nginx/front_e2e_h3_test.go`, on a real http3 server: `TestFrontE2eH3BodyEndingShortOfItsLengthIs400`, `TestFrontE2eH3BodilessRequestHasNoFraming`, `TestFrontE2eH3BodilessRequestNeedsNoBodyBudget`; `TestBodyStoreRefusesABodyEndingShortOfItsLength`, `TestBodyStoreAbsentWhenEmptyReservesAtItsFirstBytes`, `TestBodyStoreAbsentWhenEmptyUnderAFullBudget`, `TestStreamedBodyEndingShortOfItsLengthFails`.
- `nginx/route_http_test.go`: `TestRouteHttpUnmatchedPathAnswersByMethod`, `TestRouteHttpEscapesTheUpstreamPathOnlyAsNginx`, `TestRouteHttpCountsHeaderLinesAsNginxMaxHeaders`, `TestRouteHttpRefusesARepeatedHeaderNginxKeepsOnce`, `TestRouteHttpRefusesAnH2ContentLengthNginxCantParse` (a297487df). `nginx/route_http_uri_test.go`: `TestParseRouteUriRecordsAPercentInThePath`, `TestRouteUriUpstreamTargetEscapesAsNginx`.
- End to end: `TestFrontE2eServesNginxMaxHeadersLines`, `TestFrontE2eCountsTheHeaderLinesGoAdds` (the accepted off by one), `TestFrontE2eForwardsTwoIdenticalContentLengthsAsOne`, `TestFrontE2eDrainRefusesAnUpgradeWithoutTheConnectionToken`.
- `nginx/tls_store_test.go`: `TestLoadCertificateStoreServesALeafNotValidNow`, `TestCertificateStoreNotValidNamesFollowsTheValidityWindow`, `TestNewFrontReportsTheEarliestCertificateExpiry`, `TestFrontMetricsExportNoCertificateExpiryWithoutAFront`; `TestTlsConfigRefusesAnSniEndingInADot`; `TestStaleKeepaliveFailureAfterTheWriteIsAnAttempt`, a pin of the single block's 502.
- Extended: `TestRouteHttpGzipsTextHtml`, `TestTlsConfigRefusesAnAlpnOfferWithNoOverlap`, `TestFrontConfigureServerSetsTheHttpSettingsFields`, `TestParityAcceptancesMatchOnlyTheirDifferences`; the corpus gained 22 cases, to 257, such as `lb/root-put`, `uri/raw-utf8-*`, `gzip/html-with-*`, `limits/header-lines-*`, `limits/two-*`, `limits/obs-fold` and `limits/http10-transfer-encoding`.
- `connect/front_drain_test.go`: `TestFrontForcedCloseIsNotHeldByAWedgedConnection`, `TestFrontDrainIsNotHeldByAWedgedIdleConnection`, `TestFrontCloseRacingStartWaitsForEverySupervisor`, `TestFrontDrainEndsDespiteAnApiStreamWhoseHeadersNeverArrive`.
- `connect/front_http_nginx_test.go`: `TestFrontConnectNamesReachConnectOnlyThroughItsRoutes`; the write bound's `TestFrontTrackedConnBoundsAWriteItsOwnerLeftWithoutADeadline`, `TestFrontTrackedConnRestartsTheBoundOfAWriteThatMakesProgress`, `TestFrontTrackedConnKeepsAnOwnerDeadlineSetDuringABoundedWrite`, and end to end `TestFrontHttpsClosesAnIdleConnectionWhoseTlsWriteStallsOnAClientThatStoppedReading`, `TestFrontClosesAnUnauthenticatedWebsocketWhoseTlsWriteStallsOnAClientThatStoppedReading`, `TestFrontPublicTcpWritesWithoutADeadlineAreBoundedBySendTimeout` (443 and 80, hijacked or not).
- `connect/front_quic_test.go`: `TestFrontQuicFailsTheHandshakeOfAnUnknownServerName` (the real config, quic and whodis), `TestFrontQuicConnectionCapRefusesApiConnectionsWithRateLimited`, `TestFrontH3ApiResetsAStreamWhoseHeadersNeverArrive`, and `TestFrontQuicRefusesOtherNamesWithUnknownServerName`, now on served names. DB: `TestLbAltConnectNamesReachConnectOnlyThroughItsRoutes`. Vendored: `TestUrnetworkReadHeaderTimeoutResetsAStreamWithoutItsHeaders`, `TestUrnetworkReadHeaderTimeoutLeavesTheBodyToTheHandler`, `TestUrnetworkReadHeaderBlockGrowsWithTheBytesThatArrive`.
- monitor: `TestLbAltStatusStalledTlsHandshakeIsClosedWithItsAttempt`, `TestPublicUdpNativeAttemptIsBoundedWithinTheCommandTimeout`, `TestPublicUdpWhodisRowsOnBothPortsAreDistinctPaths`, `TestPublicUdpLbAltMatrixFitsTheDefaultBound`; `TestLbAltStatusNativeAttemptIsBoundedWithinTheCommandTimeout` now through `runner.lbAltStatus`, `TestLbAltStatusDeadlineDuringTheExchangeIsATimeout` failing in 5 s, not hanging, and three scope tests on `scope-excluded`.
- warp `services/`: `TestParseNginxTimeSecondsReadsNginxTimes` (moved there), `TestKeepaliveResolveReadsTheKeepaliveAsNginxApplies`, `TestIsLbAltHostIgnoresTheSwitches`, `TestLoadServicesConfigAcceptsLbAltNamesWithinTheChainRules`, `TestLoadServicesConfigAcceptsLbAltKeepalivesTheRouteFileCanHold`, `TestLoadServicesConfigComparesLbAltKeepalivesAsNginxAppliesThem`.
- warpctl: `TestLbAltDeclaringChangesOnlyConnectUnits`, `TestLbAltWorkerParsesItsInterfacesWithoutTheirPublicAddresses`, `TestLbAltSplitFirstSwitchWaitsForEveryBlock`, `TestLbAltSplitFirstSwitchWaitsInEachFamily`, `TestLbAltSplitReturningFamilySwitchesPastARetiredNginxsDnat`, `TestLbAltWorkerChainPortsReadABlockChainInIpv6Alone`, `TestLbAltChainTargetPortsSkipIpv6WithoutABoundInterface`, `TestLbAltWorkerDeployRefusesABlockChainNameBeforeItsStart`, `TestLbAltRouteLoadsAndRendersKeepalivesSpelledApart`; rewritten or extended, `TestLbAltInterfaceFlipChangesOnlyItsLbUnit` (seven switches), `TestLoadServicesConfigRefusesInvalidLbAlt` and `TestLbAltWiringRoutingOnlyLoopRetiresNginxOnceTheSplitIsReady`.

**By Gate A check (§8 step 4).** Items 1 to 6 are covered by the tests above (item 3 also by warp `TestLbAltWiringDeployPollsReadinessThenWritesTheBlockChains`), the netns PASS and the parity run. Not automated: a whodis exchange through the router in each family, and the 4053 rules of the two edges with no `router:` entry (item 1); real sessions through a switch and a rollback, left to the canary (item 2); item 7's beta qualification and nginx-path baselines.

**Not automated anywhere:** D6's rotation and rollback rehearsal before 2027-01-15; D15's CPU and scheduler delay; D16's per-block share in production; Gate B's record-set comparison (warp's `TestDnsLbAlt*` cover the derivation, `warpctl dns plan` the move); SO_REUSEPORT's hash on Linux, since the carrier tests ran on darwin.

### 11.6 Known gaps and open items

Each is open, checked against the code at a297487df and bdacae4.

**Reruns.** One `lbalt/final-nginx` race run of seven failed with its output uncaptured; six reruns, 15 runs of the new tests and the final merged tree's race run passed. If it recurs, keep the output.

**Left by the final review.**
- Nothing alerts on certificate expiry: `X-UR-Front-Cert-Expiry` has no reader (warp only names it) and no monitor signal reads `urnetwork_front_certificate_expiry_timestamp_seconds`, while a lapsed leaf only logs (§11.3 (h)). warpctl's readiness poll or the monitor should warn of the earliest expiry more than a full connect rolling deploy ahead; the private status could also count `CertificateStore.NotValidNames`.
- A KeyUpdate during an upload (low): `nginx/response.go:responseRequestBody.Read` arms the client write deadline at an h1 body's first read, for Go's 100 Continue, until the response's first write. Once an upload outlasts send_timeout, a write crypto/tls makes during the rest of the read fails at once, and the response with it, where nginx waits under client_body_timeout. Such a write is the answer to a client's TLS 1.3 KeyUpdate request, which mainstream clients don't send unprompted. Clearing the deadline after the first read races a response write on full-duplex streamed routes, whose body the forwarder's request writer reads, so the fix needs a design.
- The write bound's timing: a connection whose reader blocks in such a write closes when the write's bound passes, if its read deadline passed first, up to send_timeout (30 s) after nginx's read timer, so a KeyUpdate wedge during a request head closes about 30 s after it began, not at client_header_timeout (15 s); an idle wedge closes at keepalive_timeout, as in nginx. Bounding the write by the earlier of the two needs `frontTrackedConn` to track read deadlines. Proposed for LBALT-IMPL §4, not accepted.
- nginx's worker_connections, and its reuse of idle keepalive connections under pressure, have no counterpart: the read timeouts bound a public TCP connection, as in nginx, but no global cap does.
- The in-process connect router is a second `router.Router`: one more RouterStats report a minute per connect process, unflushed at the drain's end. It has no GET /status route, since the front's deny answers first for connect's standard status; a non-standard connect would get 404 where nginx forwards /status.
- Two different h2 Content-Length values get a stream reset where nginx answers 400 (§11.4), with no corpus case, which would need an acceptance entry.
- The first switch keys on nginx's listener, so one that falls while nginx's sockets are gone, as in an nginx container restart, goes ahead with the ready blocks. A block that never becomes ready, such as a failing beta deploy, holds a still-nginx interface on nginx, logged every 30 s: fix the block or set its weight to 0.
- Load compares keepalives whose times don't resolve as written, so two different unparsable ones read as different keepalives, which the render and `nginx -t` refuse anyway. It checks only `-` among the characters of interface and block names; the worker refuses any other name `newLbAltChainNames` rejects (it allows letters, digits, `_` and `.`) before a generation starts.
- Refuted as decided behavior, and kept as residuals:
  - An api QUIC client that re-dials from the same UDP socket during a drain reaches the draining block, where conntrack pins its tuple, and is refused until the forced close. The verifier: D12 refuses new HTTP work as decided, h3 api carries only probes (D20), and the monitor and the SDK's alt dialer use a fresh socket per attempt; serving such connections would change D12. Revisit if a socket-reusing client carries h3 api.
  - With interfaces on both nginx and lb_alt, `deploy lb`'s version poll still samples `<env>-lb` through DNS, so rounds reaching only connect fronts count nothing and the poll can time out though nginx converged. The verifier: LBALT-IMPL §7 decides it, and it is harmless, since `deploy lb` promotes before it polls and `--timeout 0` skips the poll.

**Left from round 2.**
- h3 still answers 408 on a body timeout (`TestNewFrontH3BoundsEachBodyReadByTheClientBodyTimeout`); ending it as on h1 and h2, as nginx's own h3 module would, means dropping the `r.ProtoMajor != 3` condition in `upstreamGroup.refuse` and that test's 408: the owner's call.
- An aborted request is logged and counted without nginx's 408 or 499 (§11.4); a writer method that records a status without writing it, called before the abort, would fix it.
- No parity case sends a head alone, or a whole body longer than one read, so the first read's residuals have unit tests only.
- The linger tests compare the server's wait with `e2eRstAvoidanceDelay`, a copy of Go's unexported 500 ms.

**Open design items.**
- D22 (b): a QUIC flow moved to a sibling socket reconnects only after its idle timeout, about 20 s.
- `drainConnect` still waits for `exchange.Drain`, whose DrainAllTimeout (60 m) equals the stop grace, so an exchange drain run to its deadline lets SIGKILL land before the forced close; this predates lb_alt.
- D6's amendment that upstream targets named by an older route file stay until its last block drains has no code of its own: the targets are the blocks' stable external ports.
- The 30 s chain refresh needs the committed generation in memory (`lbAltState.chainServicePorts`), so a worker restarted since its last deploy heals no chain until it deploys again.
- A lost QUIC socket is re-created only once its run has closed, which waits up to ForcedCloseTimeout (30 s) for its api connections' h3 servers.

**Not run against real iptables**, only the in-memory host: `convergeSplit`'s fallback rewrite; an HMARK rewrite after a public address or port change; a family's return past a retired nginx's DNAT; the worker loop itself, which `lbalt_wiring_test.go` drives; `pollLbAltReadiness`, which is HTTP only.

**Monitor enrollment**, the owner's, through the settings workflow; nothing is armed until then.
- monitor.yml has no `lbalt_status` and no `public_udp` block. `lbalt_status` needs `enabled`, `expected_targets`, a target per lb interface (label, inventory host, interface, families), `block_service: connect` and `expected_blocks`, in step with the block count; 10 interfaces on 4 hosts with 5 blocks fit its bounds.
- `public_udp` needs rows for connect and api on quic 443 and whodis on 53 and 4053, one alias serving both now that a path includes its port; the 60-row matrix fits `DefaultPublicUdpProbeSettings().MaxTargetCount` (64).
- The watcher is promoted with the change and until then reports `settings-generation-stale`. In `public_udp`, a host paused with `disabled: true` refuses the whole matrix; in both probes, an address a paused or excluded host shares reports `scope-excluded`.
- The per-block probe reaches each block's status through whichever block the monitor's source hashes to, so a block's own port-80 answer is seen only when that bucket maps to it; its 5 s attempt is looser than Route53's 4 s connect and 2 s response.
- Older than lb_alt: a disabled enrollment counts as a complete run, so open incidents clear after two runs with it off.

**Pre-existing issues, left for the owner.**
- Production-like warp test data outside the lb_alt files: `warpctl/dns_test.go`'s `dnsFixtureWithDns` holds the production lb_hidden_prefixes value (on origin/main since 4c47dc4), `warpctl/testdata/services-vyos.yml`'s interface ids reach tests, goldens and VYOS.md, and `services/services_test.go`, `warpctl/run.go` and its tests hold production-looking names and addresses.
- `config.go:portBlocksOf` panics on a port that is http in one version and stream in another, which load accepts (only the lb_alt service's 443 is guarded); the allocator can give a candidate ports a dead committed generation's nginx pool chain still targets; and a redirect failure after `cutover.commit()` on a block that binds no lb interface leaves the committed one unowned.
- The startup orphan drain reads `routingTable.ipv6` on a goroutine while Run's routing reconcile can set it, and `run.go`'s `parseRoutingTable` panics on an lb interface with more than one global IPv6 address.

**Smaller items.**
- The perf baseline's nginx is warp/lb's `nginx_local` build, `--with-stream` only, so its H1 hop is plaintext.
- least_conn after held requests complete is not compared: the front lowers its count after writing to the client, so the next pick races the decrement.
- Cloudflare (`warpctl/dns_cloudflare.go`) gets no stale-record report; the `alt.*` names are on the route53 primary domain.
- os.File.Close doesn't wait for a write in flight, so a release racing a spill write holds a descriptor until that write returns.
- Once connect declares lb_alt, a connect generation on a host where it is placed with a front interface never starts if the api isn't fronted there, since the render fails; load doesn't check that.

**Owner actions not taken.**
- Confirm the final review's departures (§11.3 (a) to (h)), and decide on the proposed §4 line for the write bound's timing.
- Before declaring lb_alt, load the edited services.yml with this warpctl: every host where connect is placed with a front interface must pass the `-`, chain-length and keepalive rules, and should front the api. Then declare `services.connect.lb_alt: false` first (§11.3 (d)).
- services.yml's lb_alt declarations, connect's `cores: 20` (D15) and the `alt.*` `dns_aliases` move (Gate B); monitor.yml's enrollment; the 4053 router rules on the two edges with no `router:` entry. Nothing is merged or deployed.

### 11.7 Review focus

Where a reviewer's time is best spent, and why; "(final review)" marks the final review's additions.

1. **Drain, readiness and close concurrency** (`connect/front_drain.go`, `connect/front_quic.go`, `connect/front_socket.go`, `connect/front_status.go`, `nginx/front.go`).
   - The drain context comes from `context.WithoutCancel`, so a graceful h3 shutdown can't end on its own while sockets are torn down; check that nothing waits on it after `close`, which cancels it last.
   - h3 servers close before their transport by structure, not by a lock: check that every path to a transport's close goes through `frontQuicRun.close`.
   - `superviseSocket` checks `socketsStopped` before the re-bind and again before installing: check the windows where a drain begins during a re-creation.
   - `Front.cut` closes everything concurrently, a hijack after the cut is closed at once, and `Front.Close` is bounded while handlers may still run; and the vendored http3 change's one-pass guard.
   - (final review) The forced close: cancel, `closeTcpConns`, then the `http.Server.Close` loop, which closes connections under the server's lock, close_notify first; a drain's `Shutdown` holds that lock closing idle connections until the cut. Check that nothing before the cut blocks, and that `trackTcpConn` refuses a socket accepted once closing.
   - (final review) `start` counts every supervisor before publishing `started`, and `beginDrain` and `close` do nothing before it: check that callers act only after start returns, as `run.go` does.
2. **iptables order in the cutover and the rollback** (`lbalt_split.go`, `lbalt_worker.go`, `run.go`). Every step is one transaction; the order between them is the design.
   - `convergeSplit`'s four states, and that an interruption between any two transactions, or between the families, converges on the next run.
   - HMARK goes in before the split, and the entry jumps sit at position 1, ahead of nginx's DNAT; on rollback nginx's redirect goes ahead of the split before `removeLbAltSplit`, leaving the 4053 window.
   - nginx is retired only after a reconcile returns nil, with a ready block in every family; its DNAT stays behind the jumps, pointing at closed ports, so an all-down interface fails its health check.
   - (final review) The first switch: `reconcileFamily` decides before any write, HMARK included, from the nat listing and `nginxServes`, on the snapshot `readyBlocks` reads; weight-0 blocks aren't awaited, and each family decides alone. `IsLbAltHost` reads only the declaration, the placement and a front interface: check that unit args, validation and the render agree.
   - The deploy order and the pending promotion; the ownership guard and the allocator with `lbAltChainOwnedBy` inside them.
3. **Limit and body accounting** (`nginx/limit.go`, `nginx/body.go`, `nginx/body_stream.go`, `nginx/response.go`). Every exit must give back exactly what it took.
   - The limit slot is taken before the handler, released in `routeHttpExchange.end` even on a panic, and held across a hijack until the hijacked conn closes; check the release-once guards, and `bodyStore.close` while requests still run.
   - (final review) The h3 reservation at first bytes: under `absentWhenEmpty`, `bodyStore.read` opens the body only when a read returns bytes, so its deferred release must allow a nil body, and a nil body with no refusal goes unframed; a declared length ignores the flag.
   - A kept streamed body reserves its length under the same budget, and `forwardStreamedBody.release`, deferred in `serveHttp`, returns it once every attempt's write has ended, the abort paths included.
4. **The streamed body's first read** (`nginx/body_stream.go`, `nginx/upstream.go`, `nginx/upstream_conn.go`): one body read by successive attempts' writers while the exchange may interrupt it from another goroutine.
   - The keep: a store only for an h1 body of declared length without 100-continue, kept only when the first read returns it whole; `kept` read atomically by the retry rules; `next` handing a kept body on as a stored one. Check that a writer has ended before the next attempt reads the body.
   - The timeout: armed before each read and cleared after one that returns data; a timed-out read is the client's failure unless the interrupt ended it, by whether the read's own deadline had passed. On h1 Go's server cancels the request inside the timed-out read, which is what makes the exchange interrupt the body.
   - `stopBody` leaves a whole body uninterrupted: a past deadline after the body's end would end Go's h1 background read and cancel the request, the kept body's retry with it.
   - `relay` joins the request's write before counting a failure, and the exchange's write goroutine cuts the block's connection when the client's body failed, ending the wait for the response head or the relay's next read.
5. **Ending a request or a connection** (`upstreamGroup.refuse`, `upstreamGroup.forward`, `responseState.finish`, `routeHttpHandler.serveInProcess`, `frontTrackedConn`).
   - The abort: `panic(http.ErrAbortHandler)` on a body timeout on h1 and h2 and, since the final review, for a client gone away, in `refuse` and in `forward`'s `upstreamFailureClient` branch, with no recover before Go's server; the slot released by the exchange's deferred end; `finish` not run, so no empty 200 and no half-close mark.
   - The half-close mark: `endsConnection`, set with the head from `Connection: close`, `r.Close` or keepalives off; `responseRequestBody`'s atomic read-whole flag; `http.MaxBytesReader` over a byte of the front's own, which trips requestTooLarge only on the server's own writer; no mark for a body read whole; `CloseWrite` on port 80, hidden on a hijacked connection.
   - The in-process read bound: `boundUnreadBody` after connect's handler returns on h1 with the body unread and no hijack, the limit slot already let go.
6. **TLS config lifetime and session tickets** (`newTlsConfigs`, `frontServer.start`, `connect/front_http.go`, `connect/front_quic.go`). D6c holds only while one base config of each kind is shared and never cloned: no `ServeTLS`, `http.Server.TLSConfig` nil (net/http writes into it), no ticket keys or `Clone` on the per-name configs, and one pointer to every `ListenEarly`, which quic-go under go1.27 doesn't clone. Also `Allow0RTT = false`.
7. **The name matcher and the authority rules** (`nginx/name.go`, `nginx/route_http_request.go`, `routeHttpHandler.serve`): `normalizeHost`, state by state, against ngx_http_validate_host; the longest wildcard across several labels, unlike alt, and the bound in `wildcardBases`; the h2 and h3 Host check, and `$host` for HTTP/1.0 without a Host; redirects before the limits; QUIC dispatch by SNI alone. (final review) The max_headers count over the head as Go rewrote it (on h1 `r.Host`, `r.TransferEncoding` and every value, on h2 and h3 every field but Cookie); `routeRequestUniqueHeaders` against nginx's table, given what Go merges or refuses first; the h2 Content-Length parse; `routeUri.quoted`, set where the parser decodes a `%`.
8. **The forwarder's own HTTP/1.1 client** (`nginx/upstream_conn.go`). Review fixed four defects in it: an exchange cut before its write reported (`awaitWrite`), a cancel hook firing after the response ended (`stopCancelHook`), a stale read deadline failing a pooled connection's MSG_PEEK check (`put` clears it), and the read timeout armed before the request was sent. Check the deadlines' lifecycle, that every physical attempt is counted, and round 2's dial in flight: registered in `pendingDials` under the pool's lock, refused on a closed pool, under a context the request's end or the group's close cancels (`context.AfterFunc`), and waited for by `upstreamPool.close`.
9. **send_timeout** (`nginx/response.go`, `nginx/front_server.go`, `frontTrackedConn`). The deadline is armed and cleared around each write, armed for the server's final writes and on the first body read for the 100 Continue; then the h2 write byte timeout and the write latch. A forwarder may not type-assert `r.Body`, on h1 always the front's `responseRequestBody`. (final review) `frontTrackedConn.Write` bounds a write under `stateLock` only while `ownerWriteDeadline` is zero, counts concurrent bounded writes (`boundWriteCount`), restores the owner's deadline when the last returns and re-arms after progress; a zero `SetWriteDeadline` mid-write keeps the bound, and `SetDeadline`, which net/http's hijack calls, records the write deadline too. Check it against the websocket and H1+ upgrades, which clear the write deadline after their 101.
10. **The api QUIC cap and the h3 header timeout** (final review; `frontQuicRun.serveApi`, `third_party/quic-go/http3/server_conn.go`). `serveApi` takes a slot only after `AwaitQuicHandshake` and releases it after the deferred close. The header deadline is set before the stream's first frame, unknown frames included, and cleared once the request is parsed, before the handler bounds its own body reads; a stream past it takes the existing H3_REQUEST_INCOMPLETE path. `readHeaderBlock` reads with `io.ReadAll` over an `io.LimitReader`, so memory follows the bytes that arrive.
11. **Smaller points:** `ss` parsing of device-bound rows (an unreadable row fails the listing, and `assignDeployPorts` panics, by design); occupancy relying on the trusted listeners holding a generation's ports while a public socket is re-created; `ConnectHandler.releaseQuicWindow` for connections connect didn't serve; the route file's 0444 in a 0700 directory, which keeps the hidden prefix from other local users; the clear hold of `public-udp-application`; the public UDP sockets' GSO and ECN path.
