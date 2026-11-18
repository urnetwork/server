# LBALT: connect fronts the edge interfaces (`lb_alt`)

Status: design, 2026-11-18. Nothing is implemented.

Questions D1–D19 in §6 have owner decisions (2026-11-18). Each keeps its options for the record.

The §7 measurements were taken 2026-11-18 (§7.1). The owner confirmed the three values they sized (2026-11-18):
- `cores: 20` on every connect block (D15);
- a 256 MiB body-buffer budget per connect process (D7);
- N = 4 sockets per port in each block (D10).

No design question is open. D6c was revised on 2026-11-18 to Go's default per-process session tickets.

The last check, M7's HMARK test, passed on edge-4 on 2026-11-18 (§7.1). Nothing remains before implementation.

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

## 3. Today

### 3.1 Edge lb (nginx)

**Hosts and networking**
- There are 10 lb interfaces on the four services hosts: edge-0 ×3, edge-1 ×3, edge-3 ×2, edge-4 ×2 (`../vault/main/services.yml` v26, `lb.interfaces`). Each runs its own nginx lb block.
- Every container uses host networking (`--host_networking=yes`). Each lb binds its interface docker network's bridge address, and the public address is DNATed to it. Replies leave through the interface's route table, picked by `ip rule from <interface docker subnet>` (`../warp/warpctl/run.go:26-45`).
- Each services host runs five connect blocks (beta 1, g1 24, g2 25, g3 25, g4 25). Each has `cores: 16`, which sets GOMAXPROCS 16, and binds the `warpservices` bridge address.

**TLS and upstreams**
- TLS ends only in `http{}` server blocks. Each hostname proxies HTTP/1.1 to `service-block-<svc>`.
- The upstream servers are `warpservices:<block external port>`, weighted by block weight. The external port is stable; warpctl DNATs it to the block's current container.
- Balancing is `least_conn`, or `hash $remote_addr consistent` for stateful services (mcp).

**Request headers** (`addProxyRequestHeaders`, `../warp/warpctl/config.go:2504-2522`)
- `X-UR-Forwarded-For` is overwritten with `addr:port`, IPv6 bracketed (config.go:1390-1395).
- `X-Forwarded-For` and `X-Forwarded-Source-Port` are cleared.
- `Host` and `Early-Data` are set.
- `X-UR-Request-Buffering` is `off` on streamed routes and cleared elsewhere.
- Websocket services (connect, gossip, grafana) get `Upgrade` and `Connection: upgrade`, 60 s read and send timeouts, and no buffering (config.go:2526-2539).

**Retries**
- `proxy_next_upstream error timeout http_503 non_idempotent; proxy_next_upstream_tries 2` (config.go:1358-1359).
- Retries work because request bodies are buffered: `proxy_request_buffering on`, `client_max_body_size 16m`, 64 KiB in memory and the rest on disk.
- The api drain's readiness latch depends on the 503 retry.

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

### 3.4 Connect listeners

- **H1 websocket** is served over HTTP on `WARP_HOST_IPV4` only.
- **QUIC** h3 (443) and h3dns (4053) bind IPv4 only, with `reusePort := false` (connect/transport.go:2357-2365). They wrap the socket in the PROXY reader when `EnableProxyProtocol` is set, which is the default.
- **The PROXY reader trusts the first header from each new source address**, with no peer check (connect/transport_pp.go).
- **H1 takes the client address from `X-UR-Forwarded-For`** with no trusted-peer check, and logs at error level on a repeated or malformed value (session/client_session.go:134-180). Safety relies on block ports being unreachable from clients.
- **QUIC TLS** comes from tls.go and is gated by `tls.yml allowed_hosts`, which lists only ur.network and bringyour.com names (tls.go:58-72). There is no reload.
- **Readiness.** `/status` returns `X-UR-Connect-Listeners-Ready: 1` and `X-UR-Connect-UDP-Listeners` (connect/connect_router.go:150-170). An lb with UDP streams polls every connect block for these before its cutover.

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

### 4.2 Forwarding

- **Request contract.** The forwarder copies nginx's (§3.1):
  - it overwrites `X-UR-Forwarded-For` from the accepted socket;
  - it removes client-sent `X-Forwarded-For`, `X-Forwarded-Source-Port`, `X-UR-Request-Buffering` and `Early-Data`;
  - it preserves `Host`;
  - it handles websocket upgrade and streamed bodies per service.
- **Upstreams** are the same stable `warpservices:<block external port>` targets nginx uses, with:
  - the same block weights;
  - `least_conn`, or a consistent hash on client address for stateful services;
  - one retry on connect error, timeout or 503, using buffered bodies;
  - keepalive pools per upstream block.
- **Limits and response headers** match nginx (§3.1). The token bucket and per-address caps come from alt/rate_limit.go.
- **No stream forwarding (decided, D4).** The front forwards only HTTP. There is no raw TCP or UDP forwarding and no PROXY protocol; connect's own UDP ports are served in-process.

### 4.3 Networking (warpctl)

- **The lb unit of an lb_alt interface is routing-only**, like a transparent unit: route table, ip rules, MASQUERADE, and the public DNAT split for TCP and UDP. No nginx runs for that interface (D1).
- **Each connect block binds every lb interface network's bridge address on its host**, IPv4 and IPv6, at its allocated ports. Replies leave with that network's source address, and the existing ip rules route them (D11).
- **Public flows are split across the host's connect blocks by a hash of the source address**, weighted by block weight.
  - Each block owns a chain that holds only the DNAT to its current port. The block's own deploy switches that chain.
  - Conntrack keeps existing flows on the draining container, as it does today (D10).
- **SO_REUSEPORT is used only within a block.** Each block opens several sockets per address and port, for both UDP and TCP, for receive parallelism. Blocks never share a reuseport group (D10).
- **A block's DNAT switch waits for readiness.** Every public listener on every interface address must be up first (D13).

### 4.4 Security requirements

These are not open questions; each must hold before connect is exposed directly.

1. **The PROXY reader is off on every interface-bound socket.** It trusts the first header from any new source, so leaving it on would let any client claim any address.
   - During the cutover, conntrack-pinned flows still arrive through nginx with PROXY headers. They reach the existing listener on the `warpservices` address, which only nginx can reach.
   - That listener keeps the reader on until the old flows drain, then goes away.
2. **Client-sent ingress headers are removed on interface-bound listeners.** This covers `X-UR-Forwarded-For`, `X-UR-Request-Buffering`, `X-Forwarded-For`, `X-Forwarded-Source-Port` and `Early-Data`. They are removed before in-process handling and before forwarding. Connect's own H1 path trusts `X-UR-Forwarded-For` and logs malformed values at error level.
3. **`/status` stays unreachable on public service names.** nginx denies it today and alt returns 404.
4. **Alt's ingress hardening applies to connect's public listeners.** That means:
   - the shared handshake limit;
   - the 0x1000 and 0x1001 refusals;
   - whatever fix set alt's rollback floor at 1088438700 (older builds die to a small unauthenticated UDP burst).
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

After §8 step 5, the alt service, its binary and its config are deleted.

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

**D2. The setting.**
- (a) `services.connect.lb_alt: true` only, turning on every edge interface at once.
- (b) The service flag plus a per-interface override, `lb.interfaces.<host>.<iface>.lb_alt`, for the rollout.
- Recommendation: (b). The cutover happens per public address, so the canary is one interface. Validation refuses:
  - more than one service with lb_alt;
  - lb_alt on a transparent interface;
  - a host with lb_alt interfaces and no blocks of that service;
  - on an lb_alt interface, any TCP stream mapping, or a UDP stream mapping to a service other than connect (D4).
- **Decision (owner, 2026-11-18):** (b). `services.connect.lb_alt: true` is the end state, and `lb.interfaces.<host>.<iface>.lb_alt` overrides it per interface for the canary and for single-interface rollback. The validation above applies.

**D3. Names and protocols served.**
- The proposal is the §4.1 table: every fronted name on TCP, h3 and whodis for connect and api names only, and no Alt-Svc.
- Alternative: h3 for every name, with Alt-Svc. That moves browser traffic for web, mmm and app onto QUIC through connect.
- Recommendation: §4.1 as written. It matches alt on UDP and nginx on TCP.
- **Decision (owner, 2026-11-18):** §4.1 as written. h3 and whodis serve connect and api names only, everything else on QUIC is refused with 0x1000, and no name gets an Alt-Svc header.

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

**D7. Retries and body buffering.**
- nginx buffers every body that isn't streamed (16 MiB cap, 64 KiB in memory, the rest on disk). That lets it retry a 503 or a connect failure on another block, including for non-idempotent requests.
- (a) Same semantics, with bodies buffered in memory up to 16 MiB under a process-wide byte budget. Over the budget, answer 503 before reading the body.
- (b) Spill to a temp file past 64 KiB, as nginx does.
- (c) No body buffering. Retry only when the upstream connection failed before the request was sent.
- Recommendation: (a), with the budget sized from M2. Option (c) breaks the api drain contract, because a draining api block answers 503 and relies on the retry.
- **Decision (owner, 2026-11-18):** (a). Bodies are buffered in memory up to 16 MiB each, under a process-wide byte budget sized from M2. Over the budget, connect answers 503 before reading the body. There is one retry on connect error, timeout or 503 (non-idempotent included), on another block. Streamed routes pass through unbuffered.
  - Sized from §7.1, confirmed (owner, 2026-11-18): a 256 MiB budget per connect process, room for 16 concurrent 16 MiB bodies. The measured load is a mean body of 74 bytes and at most 4.3k api requests in flight fleet-wide, which needs under 1 MiB. Grafana pushes and the validators' artifact uploads fit too (§7.1, M2).
  - Amended by D19 (owner, 2026-11-18): only the first 64 KiB of each body is held in memory under the budget, and the rest goes to a temp file, as nginx does.

**D8. Limits.**
- nginx's per-address limits are kept per interface process. With the source-address hash split (D10), an address always lands on one block per interface, so per-block buckets reproduce per-interface limits exactly.
- Recommendation:
  - Use alt/rate_limit.go semantics: a burst+1 nodelay token bucket, per-address concurrency, and 429.
  - Take the numbers and route zones from services.yml, and apply them to forwarded routes and to connect's own H1.
  - QUIC connect keeps alt's per-address connection cap.
  - Keep nginx's timeouts and the 16 MiB body cap.
- **Decision (owner, 2026-11-18):** as recommended. alt/rate_limit.go semantics, with nginx's numbers and route zones from services.yml. The limits apply to forwarded routes and to connect's own H1; QUIC connect keeps alt's per-address connection cap. nginx's timeouts and body cap carry over.

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

**D16. Blast radius.**
- A connect regression or crash now takes down its share of every fronted service on that host, not only connect.
- Every connect release also redeploys the front for api, web, mmm and the rest.
- Recommendation: accept this, and use beta as the canary for front changes too. Don't add a separate front process, since that is the hop this project removes.
- **Decision (owner, 2026-11-18):** accepted. The front stays in connect, and beta (a fixed 1% of client addresses) is the canary for front changes too.

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

**D18. Rollback.**
- Recommendation:
  - Setting `lb_alt: false` on an interface moves it back to nginx, using the same cutover as a deploy: a DNAT switch, with conntrack keeping old flows.
  - Keep building the lb image every release, with its tests green, until the cutover has soaked.
  - After that, nginx config generation stays for environments without lb_alt.
- **Decision (owner, 2026-11-18):** as recommended. Setting `lb_alt: false` on an interface moves it back to nginx with the deploy-style cutover. The lb image keeps being built every release, with its tests green, until the cutover has soaked on all ten interfaces.

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

**M5. Alt.**
- Load: 91 req/s, mostly `/hello` (42/s) and `/connect/control` (37/s); 154 KB/s of h3 datagrams in; 0.3 cores per proxy host.
- Memory: 5.7–6.0 GiB RSS per proxy host.
- Moving this load to the edges is negligible.

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

**Certificate expiry.** Every certificate in `vault/all/tls/2026.10.16` expires 2027-01-15. Under D6b, the next set must land and connect blocks must redeploy before then.

## 8. Rollout

One cutover per interface (D1).

**0. Measure.** Decisions are recorded in §6. Measurements M1–M7 were taken 2026-11-18 (§7.1), and the owner confirmed the values they sized. The HMARK test passed on edge-4 (M7), so warpctl can rely on HMARK in step 3.

**1. The front: `server/nginx` and connect (§4.5).** Starting from alt's code:
- TLS for every fronted name;
- h3 and whodis dispatch for connect and api names;
- HTTP/1.1 forwarding with the nginx request contract;
- limits, retries and buffering;
- redirects and response headers;
- the lb routes and port 80;
- per-interface, dual-stack listeners;
- the PROXY reader off on interface-bound sockets;
- extended readiness.

**2. Parity harness, before any traffic.** Send the same requests to the lb image and to the front, both generated from the same services config. Diff the status, the response headers, and the headers the upstream receives.

**3. warpctl: lb_alt mode.**
- routing-only lb units;
- the hash split for TCP and UDP, and block-owned chains;
- multi-block validation;
- readiness gating.

**4. Canary.** Before the first cutover, connect's `cores` goes to 20 on every block (D15). Then cut over one interface with a low DNS weight, TCP and UDP together, then all ten.

**5. Alt.** Move `alt.*` (D14), then remove alt.

**6. Soak.** Then stop building the lb image for main.

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

## 10. Not in scope

- QUIC connection-ID steering, and connection migration across blocks.
- Changes to the whodis encoding or its tld.
- The proxy service on fireside and crisp.
- Environments other than main, where lb_alt stays off.
