# LBALT implementation contract

This is the working contract for implementing [LBALT.md](LBALT.md), with the review folded in from [LBALT-REVIEW.md](LBALT-REVIEW.md). Parallel agents implement it. Every agent reads this file, the parts of LBALT.md its unit names, and `../connect/CODESTYLE.md`, which is the style guide for all code here.

The contracts in §2 are fixed. An agent that finds one unworkable stops and reports. It doesn't change a contract on its own branch, because other branches code against it.

## 1. Rules for every unit

### Style and tests
- Follow `../connect/CODESTYLE.md` exactly. In particular:
  - receivers are named `self`;
  - acronyms are ordinary words (`Http`, `Tls`, `Ipv4`, `Id`, `Quic`, `Sni`);
  - names follow usage + type (`blockWeights`, `interfaceNameLimits`);
  - every tunable is a field of a settings struct with a `Default...Settings()` constructor. No package-level tunable constants, and no inline duration or count literals in logic. Protocol and format values stay named constants;
  - doc comments sit at the top of each file, type and function, and don't repeat the name;
  - locks are `stateLock`, held in an immediately invoked closure, with `...WithLock` naming;
  - every loop takes a context and rate-limits itself;
  - verbose logs are guarded with `Enabled()`;
  - structs are built with explicit field names;
  - binary sizes are products (`64 * 1024`);
  - randomness comes only from `math/rand/v2`, or `crypto/rand` for secrets.
- Tests are top-level `TestXxx`, with no `t.Run` for ordinary cases; tables are fine.
- Test data is synthetic:
  - `.example` names;
  - documentation addresses `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`, plus loopback for sockets;
  - generated certificates;
  - no production hostnames, prefixes, IPs or secrets.
- Every behavior gets a test that fails without it. Use barriers or hooks, not sleeps, as the primary proof of ordering.
- Keep diagnostics free of client addresses and of the lb hidden prefix value: no logs, metric labels or error bodies containing them.

### Builds (the disk is nearly full)
- Run `export GOFLAGS=-trimpath` before every go command, so the shared build cache is reused across worktree paths.
- Build and test only the packages you touch and their direct dependents, for example `go test -count=1 ./nginx/...`. Never run `go test ./...` at the server root, or the full xops suite.
- Check free space with `df -h /Users/brien`. If it is under 25 GB, stop and report instead of building.
- Don't run Docker or Linux network tests. The harness units in §3 do that later.

### Git
- Work only in your own worktree, listed in §3. Commit there on your branch, in the repo's commit style.
- Commit messages carry no AI attribution of any kind (see CODESTYLE "Commits").
- Never push, merge, rebase onto main, or touch another worktree or the main checkouts.
- Don't edit files another unit owns (§3). If you need a change there, describe it in your final report.

## 2. Contracts

### 2.1 Route file (schema version 1)

warpctl renders one JSON file per connect generation, when it starts the container (D5). Connect reads it at start. The file carries resolved values only: server and connect never apply services.yml defaults.

The revision is the lowercase hex SHA-256 of the exact file bytes.

```json
{
  "schema_version": 1,
  "env": "main",
  "host": "edge-a.example",
  "generation": "<container name of this generation>",
  "domains": ["example.com", "example.net"],
  "upstream": {"host": "warpservices", "ipv4": "192.0.2.1"},
  "lb": {
    "names": ["main-lb.example.com", "main-lb.example.net"],
    "route_prefixes": ["/hidden-example"]
  },
  "no_access_log": ["quiet.example.com"],
  "interfaces": [
    {
      "name": "eth1",
      "block": "edge-a.example-eth1",
      "lb_alt": true,
      "ipv4": "198.51.100.1",
      "ipv6": "2001:db8:1::1",
      "rate_limit": {"requests_per_minute": 450, "burst": 150, "net_connections": 50, "exclude_subnets": ["10.0.0.0/8"]}
    }
  ],
  "listen": {"http_port": 7080, "https_port": 7443, "quic_port": 7443, "whodis_port": 7453},
  "front": {"connect_service": "connect", "api_service": "api", "whodis_tlds": ["ur.xyz."]},
  "services": [
    {
      "name": "api",
      "exposed": true,
      "lb_exposed": true,
      "status": "standard",
      "names": ["main-api.example.com", "api.example.com"],
      "redirect_aliases": [{"name": "old-api.example.com", "target": "api.example.com"}],
      "cors_origins": ["https://app.example.com"],
      "websocket": false,
      "streamable": false,
      "streamable_paths": [],
      "stateful": false,
      "route_prefixes": [""],
      "route_rate_limits": [{"path": "/connect/control", "requests_per_minute": 12000, "burst": 6000}],
      "route_request_bodies": [{"path": "/sn/attempt-artifact", "client_body_buffer_size": 65536, "client_body_timeout_seconds": 60}],
      "no_success_access_log": ["/connect/control"],
      "keepalive": {"connections": 1024, "requests": 8192, "time_seconds": 900, "timeout_seconds": 60},
      "blocks": [{"name": "g1", "weight": 24, "port": 7123}]
    }
  ]
}
```

Field rules:
- **Encoding.** The file is exactly `json.MarshalIndent(table, "", "  ")` followed by `\n`. Struct fields are declared in the order shown above, and empty arrays are `[]`, never `null`.
- **Ordering.** Every array has a defined order, so both repos produce and parse the same bytes:
  - `domains`: primary first, then sorted;
  - `lb.names`: DomainNames order, whose first entry is the TCP default server;
  - `services`: the services the nginx generator fronts on this host (`NginxConfig.services()` rule), sorted by name;
  - `blocks`: `services.ServiceBlockWeights` order: list order, with keys sorted inside each map;
  - `interfaces`: sorted by name;
  - every other list sorted lexicographically and deduplicated: `names`, `route_prefixes`, `streamable_paths`, `no_success_access_log`, `no_access_log`, `exclude_subnets`, `cors_origins` and `whodis_tlds`; and by `name` or `path`, `redirect_aliases`, `route_rate_limits` and `route_request_bodies`.
- **`interfaces`** lists every non-transparent lb interface on the host, whether lb_alt is on or not. Connect binds all of them, so flipping one interface's override never changes connect's unit (§2.2). `lb_alt` is informational for connect.
- **`ipv4` and `ipv6`** are the interface docker network's bridge addresses on this host. An empty string means the family is absent.
- **`rate_limit`** is resolved per interface (nginx's zones are per interface). `requests_per_second` in YAML is converted to `requests_per_minute`, and the value may be fractional.
- **`listen`** holds this generation's internal ports, from WARP_PORTS:
  - `http_port` is service port 80;
  - `https_port` and `quic_port` are both service port 443;
  - `whodis_port` is service port 4053.
- **`names`** in a service are the exact served names, after env aliases, expose_aliases and expose_domains, with nginx's server_name forms:
  - exact `a.b.c`;
  - leading wildcard `*.b.c`, which matches one or more labels, as nginx does.

  Redirect alias names are served too, and need certificates.
- **`blocks[].port`** is the block's external port for service port 80. Upstream targets are `upstream.ipv4:port`. The literal string `upstream.host + ":" + port` (for example `warpservices:7123`) is the key for nginx's consistent-hash ring.
- **`status`** is the raw services.yml value. `"standard"` (the default) means `/status` is denied on service names.
- **`front.whodis_tlds`** replaces connect's hard-coded `DnsTlds`.
- **`redirect_aliases[].target`** is a host. The front answers `301 <request scheme>://<target><request uri>`, as nginx's `return 301 $scheme://<target>$request_uri` does.
- **`cors_origins`** is the resolved list (`cors_origins`, or the `cors_origins_from` chain). It contains `"*"` for wildcard mode. Otherwise the front echoes an exactly matching origin and adds `Vary: Origin`, as nginx does.
- **`route_prefixes`** follows nginx's shape: `"/" + prefix` for each hidden prefix, or `[""]` when there is none. That holds for `lb.route_prefixes` and for each service (top-level `hidden_prefixes`, then the service's own).

Parsing:
- Parsing is strict: unknown fields, a different `schema_version`, or a missing required field all fail. A connect started with an unreadable route file fails its start, and never comes up half-configured.
- The example values in this section are illustrative. The authoritative fixture is the checked-in file below, generated from structs declared in contract order.
- The same file is checked in, byte for byte, at `server/nginx/testdata/route_v1_example.json` and `warp/warpctl/testdata/lbalt_route_v1_example.json`. Both repos test against it: warpctl renders it from a synthetic services.yml fixture, and server parses it.

### 2.2 warpctl to connect: units, env and mount

lb_alt is enabled for a host when the lb_alt service is connect (validation allows only connect) and either:
- the service sets `lb_alt: true`; or
- one of the host's interfaces has the override `lb_alt: true`.

**Connect units** on such a host get `--lbaltnets="<iface>:<docker_network>;..."`. It lists every non-transparent lb interface on the host, sorted. It doesn't depend on per-interface overrides, so a flip never changes the unit.

**When a connect block starts, the worker:**
1. renders the route file, at `<WARP_HOME>/lbalt/<container name>/route.json` (`/srv/warp/<env>/...` on hosts), mode 0444, never rewritten;
2. bind-mounts it read-only at `/srv/warp/lbalt/route.json`;
3. sets these env vars:
   - `WARP_LBALT_ROUTE_FILE=/srv/warp/lbalt/route.json`
   - `WARP_GENERATION=<container name>`

The directory is removed when the container is removed.

**lb units of lb_alt interfaces** get `--lbalt=true`, never `--transparent`. They run the routing-only loop plus the split reconcile (§2.5). They also get `--lbaltblocks="<block>:<weight>;..."`, in the stable order of `ServiceBlockWeights`.

### 2.3 Public ports of an lb_alt interface

`services.LbAltPublicPorts(version, host, iface)` returns them. The host rules and the VyOS generator both use it.

| Public | Protocol | Connect socket (on the interface bridge address) |
|---|---|---|
| 80 | tcp | `listen.http_port`: the front's port-80 handler |
| 443 | tcp | `listen.https_port`: the TLS front |
| 443 | udp | `listen.quic_port`: QUIC |
| 4053 | udp | `listen.whodis_port`: whodis |
| 53 | udp | `listen.whodis_port` |

- Connect binds every (interface address, protocol, port) above. It uses N sockets each, with SO_REUSEPORT, N = 4, in both families when present.
- The trusted sockets on the warpservices address stay for the block's whole lifetime (§4.6): private H1 on service port 80, the PROXY-reading UDP 443 and 4053, the exchange, and `/status`.
- Public sockets never use the PROXY reader.
- connect's TCP 443 uses the same internal port as service port 443. warpctl allocates it as its own lb type (`"tcp"`), so the nginx http and stream generators ignore it.

### 2.4 Readiness: connect's private /status

The private H1 status (`GET /status` on the warpservices address) adds these headers when the front is configured:

| Header | Value |
|---|---|
| `X-UR-Front-Generation` | `WARP_GENERATION` |
| `X-UR-Front-Route-Revision` | hex SHA-256 of the route file bytes |
| `X-UR-Front-Ready` | `1` only when the status latch is `ok` (not draining), the route file and every certificate are loaded, and every expected socket is up; otherwise `0` |
| `X-UR-Front-Sockets` | `<protocol>/<address>/<port>=<up>/<expected>`, comma-separated and sorted, for example `tcp/198.51.100.1/7443=4/4` |
| `X-UR-Front-Cert-Expiry` | earliest certificate expiry, RFC 3339 UTC |

- Draining is never ready.
- `X-UR-Connect-Listeners-Ready` and `X-UR-Connect-UDP-Listeners` keep their current meaning for the nginx path.

warpctl accepts a candidate generation only if all of these hold:
- HTTP 200, with status exactly `ok`;
- `X-UR-Front-Ready: 1`;
- the generation matches;
- the revision equals the SHA-256 of the file it rendered;
- every expected `(protocol, bridge address, port)` shows `4/4`.

### 2.5 iptables split (lb unit and block workers)

- **Mark.** HMARK on new flows, `--hmark-tuple src --hmark-mod 100 --hmark-offset 0x10000 --hmark-rnd 0x4c42414c`. The seed is a named protocol constant, since it must be stable across restarts.
  - The rule sits in mangle PREROUTING, and in mangle OUTPUT for local dials.
  - It matches `-d <interface public address>` and the public ports of §2.3, per protocol.
  - It uses `-m conntrack --ctstate NEW`, in iptables and in ip6tables.
- **Chains.** They fit the 28-character limit:
  - split chain: `WARP-<ENV>-LBA-<IFACE>`, in nat;
  - per block: `WARP-<ENV>-LBA-<IFACE>-<BLOCK>`;
  - the same names in both families.
- **Split.** The split chain holds one rule per bucket, `-m mark --mark 0x100XX -j <block chain>`.
  - Buckets go to blocks in `ServiceBlockWeights` order, with largest-remainder rounding of weight × 100 / total.
  - A block with weight > 0 gets at least one bucket.
- **Block chain.** It holds only the DNAT rules from §2.3, to `<interface bridge address>:<listen port>`, per protocol and family. The block's own worker writes it, after readiness (§2.4).
- **Entry.** The lb chain `WARP-<ENV>-LB-<IFACE>` jumps the interface's public ports to the split chain, in place of the nginx DNAT.
- **Replacement.** A replacement chain is built complete, under a temporary name, before any jump points at it. The last working targets stay until then.
- **Reconcile** (lb unit, every 30 s).
  - It moves only orphaned bucket ranges, to ready blocks by weight. A block is orphaned when its chain is missing, or its target has no listener.
  - It gives a range back when its block returns.
  - If every block is down, the split chain sends nothing anywhere, so the Route53 health check fails.
- **Rollback.** Setting the interface's `lb_alt: false` removes the HMARK rules, the entry jumps and the split chains, after nginx's DNAT is restored.

### 2.6 lb status from the front

- On lb names, `/<prefix>/status` answers 200, `Content-Type: application/json`, with body `{"version":"<connect WARP_VERSION>","status":"ok","front":"lbalt"}` and a newline. It answers on port 80 and on 443.
- warpctl's lb version polls (`deploy lb`, `ls versions`) ignore samples whose JSON has `"front":"lbalt"`.
- Route53's check needs that 200 on port 80, with no redirect.

### 2.7 `server/nginx` Go API

Package `github.com/urnetwork/server/nginx`. It must not import `server/connect`, `server/alt` or `server/api`.

```go
const RouteSchemaVersion = 1

type RouteTable struct { /* fields of §2.1, json tags exactly as shown */ }
func ParseRouteTable(raw []byte) (*RouteTable, error)
func LoadRouteTable(path string) (routeTable *RouteTable, revision string, err error)

type CertificateStore struct{ /* ... */ }
// Loads certificate+key pairs for every name the route table serves, using findLatestTls (warpctl config.go:910-988)
// over <vault>/<env>/tls, then <vault>/all/tls, then the flat host layout <vault>/tls. It fails if any name has
// no complete pair, or if a certificate's SANs don't cover the name.
func LoadCertificateStore(vaultDir string, env string, routeTable *RouteTable) (*CertificateStore, error)
func (self *CertificateStore) EarliestExpiry() time.Time

type NameClass int
const (
    NameClassUnknown NameClass = iota
    NameClassLb
    NameClassConnect
    NameClassApi       // the api service: forwarded, and admitted on h3/whodis
    NameClassForwarded // every other fronted service
    NameClassRedirect
)

type FrontSettings struct { /* every tunable of D7, D8, D12, D19, D21 and nginx's timeouts */ }
func DefaultFrontSettings() *FrontSettings

type Front struct{ /* ... */ }
// connectHandler serves connect authorities in-process (websocket and H1+ upgrade). The Front applies the ingress
// rewrite and D8 limits around it.
func NewFront(ctx context.Context, routeTable *RouteTable, revision string, certificateStore *CertificateStore, connectHandler http.Handler, version string, settings *FrontSettings) (*Front, error)
func (self *Front) ClassifyName(name string) NameClass
func (self *Front) TlsConfig() *tls.Config      // one long-lived base config for every TCP 443 socket; GetConfigForClient by SNI
func (self *Front) QuicTlsConfig() *tls.Config  // one long-lived base config for every QUIC socket (alt's ALPN rules)
func (self *Front) HttpsHandler(interfaceName string) http.Handler // TCP 443: routes by authority
func (self *Front) HttpHandler(interfaceName string) http.Handler  // TCP 80: lb status routes, 301s
func (self *Front) H3ApiHandler(interfaceName string) http.Handler // h3: only api authorities
func (self *Front) AcquireQuicConnection(interfaceName string, addr netip.Addr) (release func(), ok bool) // alt's per-address QUIC cap (0x1001)
func (self *Front) BeginDrain()                 // stop admitting new HTTP work; responses carry Connection: close
func (self *Front) Close() error                // release pools and spill files
```

- The handlers carry everything per request: the ingress rewrite, D8 limits, D7/D19/D21 body handling, retries, responses and routing.
- Handlers expect `r.RemoteAddr` to be the accepted peer, and `r.TLS` to be set on TLS sockets.
- One `Front` serves every interface. Limit state is keyed by (interface name, normalized client IP).

### 2.8 Connect integration (`server/connect`)

- If `WARP_LBALT_ROUTE_FILE` is set, connect loads the route file and every certificate before readiness. It then builds the `Front`, and binds every public socket (§2.3) before the private `/status` first answers.
- Name dispatch:
  - **TCP 443:** TLS through `Front.TlsConfig()`; HTTP/1.1 and h2 through `Front.HttpsHandler(iface)`.
  - **QUIC:** at the connection, by SNI. Connect names go to `HandleQuicConn`, with the h3 or h3dns carrier. Api names go to http3 with `Front.H3ApiHandler(iface)`. Anything else is closed with 0x1000.
  - **Limits:** a per-address QUIC connection cap closes with 0x1001. One handshake admission limit is shared by every socket.
  - **Early data:** the public QUIC config sets `Allow0RTT: false`.
- whodis on `listen.whodis_port` uses decode53 with `front.whodis_tlds`.
- Supervision: each public socket is supervised on its own.
  - A lost socket is re-created at once (D22). The backoff resets after a healthy run.
  - Readiness reports it missing until it is back.
  - Losing a socket never ends the process.
- Drain order:
  1. `SetDrainingIfReady`;
  2. then together: `Front.BeginDrain()`, server-level keepalives off, and a GOAWAY once per h2 or h3 connection while the transports are live;
  3. alongside, `exchange.Drain()`;
  4. hijacked H1 and forwarded websockets are tracked through the deadline;
  5. a forced close is bounded;
  6. `cancel()` comes last.

## 3. Work units and file ownership

Worktree roots are `/Users/brien/urnetwork-wt/lbalt/<unit>/`. A server unit works in `<root>/server`, whose root has sibling symlinks so `../connect` and the rest resolve. A warp unit works in `<root>/warp`. Every unit's branch is `lbalt/<unit>`, started from `lbalt/base`.

| Unit | Repo | Owns | Scope |
|---|---|---|---|
| base-server | server | `nginx/route.go`, `nginx/settings.go`, `nginx/front.go` (API skeleton only), `nginx/doc.go`, `nginx/testdata/route_v1_example.json` | route table parse/load/revision with full tests; settings with defaults; compiling API stubs |
| base-warp | warp | `services/lbalt.go`, `services/lbalt_test.go`, the `LbAlt` fields in `services/services.go`, `validateLbAlt` in `services/load.go`, `warpctl/lbalt_route_schema.go` | D2/D4 validation and accessors; Go types of §2.1 for the renderer |
| s-tls | server | `nginx/tls*.go`, `nginx/name*.go` | CertificateStore (findLatestTls, the flat host layout, SAN validation, expiry); name matching with nginx semantics; TlsConfig and QuicTlsConfig (ALPN per class, curves and TLS 1.2 suites pinned to nginx's, early data off, one long-lived config) |
| s-forward | server | `nginx/forward*.go`, `nginx/upstream*.go`, `nginx/body*.go` | upstream pools; least_conn; nginx's weighted Ketama, exact; D7 retries; request contract; websocket relay; streamed bodies; D19/D21 body storage |
| s-http | server | `nginx/limit*.go`, `nginx/response*.go`, `nginx/route_http*.go`, `nginx/front.go` (implementation) | D8 limits; responses (CORS, security headers, Vary); routing by authority, redirects, `/status` deny, port 80, the default server, hidden lb routes, lb status JSON; the Front assembly and BeginDrain |
| s-connect | server | `connect/front*.go`, and edits to `connect/run.go`, `connect/connect_router.go`, `connect/transport.go` (only what §2.8 needs) | public listeners, dispatch, readiness headers, drain order, D22 |
| s-monitor | server | `monitor/public_udp*.go`, `monitor/*lbalt*`, `router/ingress.go` | the monitor accepts connect/api and completes `/hello`; the port-80 status check (200 and JSON, no 3xx); per-block probes; `Early-Data` removed in the ingress helper |
| w-route | warp | `warpctl/lbalt_route.go`, `warpctl/lbalt_route_test.go`, `warpctl/testdata/lbalt_route_v1_example.json` | the renderer of §2.1 from the nginx generator's inputs; golden test against the shared example |
| w-split | warp | `warpctl/lbalt_split.go`, `warpctl/lbalt_split_test.go`, `warpctl/lbalt_ports.go` | §2.5 chains, HMARK, reconcile and rollback; generation-owned ports (`ss -Htuln`, per bridge/protocol/port); conntrack per retired generation; `pollLbAltReadiness` per §2.4 |
| w-units | warp | edits in `warpctl/config.go` (units, port blocks), `warpctl/main.go` (flags), `warpctl/run.go` (worker fields, routing-only loop calling w-split's reconcile, startContainer render/mount/env, deploy overlap calling w-split's readiness and chain writes) | §2.2; lb version polls ignore `"front":"lbalt"` (docker.go) |
| w-edge | warp | edits in `warpctl/vyos.go`, `warpctl/dns.go`, `warpctl/dns_route53.go`, `publicPortServiceTargets` in `warpctl/run.go`, `services/routers.go` (dns_aliases ownership) | the public port list in host DNAT and VyOS (4053 on lb_alt); lb_alt interfaces stay in DNS; `alt.*` as a DNS-only alias of connect with one owner per name (D14) |

Ownership is by file. Units that touch the same file (only `warpctl/run.go`) must change disjoint functions. They add new code in their own files, and keep their edits in shared files minimal.

## 4. Parity decisions

The comparison harness checks the front against the pinned lb image, and accepts these differences:
- No `Server: nginx` header, and no nginx HTML error pages. The status codes match.
- No slash auto-redirect whose `Location` carries the internal listen port.
- No duplicate `Content-Type` on status routes, and no duplicate `Connection` header to websocket upstreams.
- TLS curves:
  - `X25519MLKEM768` is offered before nginx's `X25519:prime256v1:secp384r1`;
  - TLS 1.2 suites are limited to ECDHE with AES-GCM, as nginx's `EECDH+AESGCM` is;
  - session tickets follow D6c.

The front reproduces these:
- `Host` normalization (lowercase, no port);
- header limits (414 and 400 at 8 KiB);
- an h2 `:authority` and `Host` mismatch answered 400;
- TRACE answered 405;
- headers with underscores dropped;
- upstream response headers over 4 KiB answered 502;
- gzip of `text/html` when the upstream sends none.

A unit that finds another difference adds it to this list in its report, with its choice.

## 5. Decisions made while building the base (2026-11-18)

Route table (server `nginx/route.go`, which w-route must satisfy):
- Parsing requires every field under its exact key, and refuses `null` and unknown or recased keys.
- Names are lowercase. A served name belongs to exactly one place: an lb name, one service's `names`, or one redirect alias. nginx's generator keeps a redirect alias among its service's hosts, so w-route moves it out of `names`, as the fixture shows.
- Addresses are in canonical `netip` text, so `X-UR-Front-Sockets` matches what warpctl rendered. No interface address may equal `upstream.ipv4`.
- These lists are non-empty: `domains`, `lb.names`, `lb.route_prefixes`, `interfaces`, each service's `route_prefixes`, and `front.whodis_tlds`.
- `status` is the resolved value, so `"standard"` is written out.
- `route_request_bodies` values are positive. `route_rate_limits` rates are above 0, because nginx refuses a zero rate.
- Block ports are unique across services. `http_port` differs from `https_port`, `quic_port` from `whodis_port`, and `connect_service` from `api_service`.
- Identities use only `[A-Za-z0-9._-]`.
- A block of weight 0 gets no new requests. nginx refuses `weight=0`, and the split gives it no bucket.

Services (warp `services/lbalt.go`):
- `LbAltService` names the service that declares lb_alt, `true` or `false`, so D2's "`true` under a disabled service default" works.
- `LbAltBucketCount = 100` is the one source of the HMARK modulus.
- Validation also refuses an effective lb_alt interface whose lb_alt service blocks can't be split into buckets.
- Only the lb_alt service's own `tcp_stream_ports` are refused. Lb-level stream ports name no service and are not refused.

Front API (§2.7): `AcquireQuicConnection` was added for alt's per-address QUIC cap. s-http implements it.

Cross-unit warp functions:
- They are stubbed in base with fixed signatures, in files their owners replace.
- `warpctl/lbalt_route.go` (w-route): `renderLbAltRouteTable`, `writeLbAltRouteFile`, `removeLbAltRouteFile`.
- `warpctl/lbalt_split.go` (w-split): `reconcileLbAltSplit`, `removeLbAltSplit`, `writeLbAltBlockChains`, `pollLbAltReadiness`.
- w-units calls them. Their owners may add helpers, but not change these signatures.

## 6. Status

- [x] base-server
- [x] base-warp
- [ ] implementation units
- [ ] parity harness (server, opt-in Docker test against the pinned lb image)
- [ ] network-namespace harness (warp, opt-in Linux container test with two interfaces)
- [ ] integration and review
