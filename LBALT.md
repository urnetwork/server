# LBALT: connect fronts the edge interfaces (`lb_alt`)

Status: design, 2026-11-18. Nothing is implemented.

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
