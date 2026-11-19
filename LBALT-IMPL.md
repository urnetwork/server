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

lb_alt is enabled for a host when the lb_alt service is connect (validation allows only connect), declared `true` or `false`, connect is placed on the host, and the host has a non-transparent lb interface (final review). Neither the default's value nor any override counts, so no switch, of an interface or of the default, changes connect's units. Declaring lb_alt is the step that redeploys connect, and removing the declaration is the only other one; LBALT.md §11.3 (d) gives the canary order this needs.

**Connect units** on such a host get `--lbaltnets="<iface>:<docker_network>;..."`. It lists every non-transparent lb interface on the host, sorted. It depends on neither the overrides nor the default's value, so a switch never changes the unit (final review). They also get `--lbalthost=<host>`, the host's key in services.yml, which the route render reads. warpctl never guesses the key from the machine's hostname.

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
  - The rules are written in the form `iptables -S` lists back: `--hmark-src-prefix 32` (IPv4) or `128` (IPv6) in place of `--hmark-tuple src`, which hashes the same. A rule is then deleted by its listed form.
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
- **Replacement.** No jump ever points at a partial chain. The last working targets stay until the replacement is complete.
  - A block chain is replaced in place by one `iptables-restore --noflush` transaction per family (flush and refill), and not at all when it already matches.
  - The split chain is built complete under `WARP-<ENV>-LBT-<IFACE>`, and the entry jumps switch to it before the old chain goes.
- **Reconcile** (lb unit, every 30 s).
  - It moves only orphaned bucket ranges, to ready blocks by weight. A block is orphaned when its chain is missing, or its target has no listener.
  - It gives a range back when its block returns.
  - If every block is down, the split chain sends nothing anywhere, so the Route53 health check fails.
- **First switch** (final review). While a family's lb chain has no entry jump and holds a DNAT, without the split's comment, whose target a host socket listens on (nginx's; nginx runs with host networking), the reconcile writes nothing in that family until every block of positive weight is ready there, and returns `errLbAltSplitAwaitsEveryBlock`, so the lb unit keeps nginx. A retired nginx's DNAT stays in the lb chain with nothing listening behind it and holds nothing: a family nginx never served (after a reboot) or no longer serves (one that lost its entry jumps and came back) switches with the ready blocks, orphaned ranges moved (D13).
- **Rollback.** Setting the interface's `lb_alt: false` removes the HMARK rules, the entry jumps and the split chains, after nginx's DNAT is restored. Block chains stay: they belong to the block workers, and do nothing without the split's jumps.

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
// over <vault>/<env>/tls, then <vault>/all/tls, then the flat host layout <vault>/tls. It fails only if a name has
// no complete, readable pair, a key that doesn't match its certificate, or a certificate whose SANs don't cover
// the name. (final review) A leaf outside its validity window loads and serves, as nginx serves it, and is logged
// at error level; EarliestExpiry, and so X-UR-Front-Cert-Expiry, includes it, and NotValidNames lists it.
func LoadCertificateStore(vaultDir string, env string, routeTable *RouteTable) (*CertificateStore, error)
func (self *CertificateStore) EarliestExpiry() time.Time
func (self *CertificateStore) NotValidNames(now time.Time) []string // (final review) the served names whose leaf isn't valid at now

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
func (self *Front) ConfigureServer(httpServer *http.Server) // every HttpSettings-derived field of a public TCP server (§7)
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
  - **QUIC:** at the connection, by SNI.
    - A missing or unknown SNI fails the TLS handshake. alt does the same: its TLS refuses names outside its allowed hosts, since self-signing is off in production.
    - Connect names go to `HandleQuicConn`, with the h3 or h3dns carrier. Api names go to http3 with `Front.H3ApiHandler(iface)`.
    - Any other served name completes the handshake and is closed with 0x1000.
  - **Limits** (final review): a per-address QUIC connection cap, charged to connect and api connections alike once the handshake proves the address, closes with 0x1001. One handshake admission limit is shared by every socket. An h3 api request stream whose HEADERS do not arrive within client_header_timeout is reset with H3_REQUEST_INCOMPLETE, and its field section is read as it arrives (the vendored http3's ReadHeaderTimeout). nginx serves no h3 here.
  - **Early data:** the public QUIC config sets `Allow0RTT: false`.
- whodis on `listen.whodis_port` uses decode53 with `front.whodis_tlds`.
- Supervision: each public socket is supervised on its own.
  - A lost socket is re-created at once (D22). The backoff resets after a healthy run.
  - Readiness reports it missing until it is back.
  - Losing a socket never ends the process.
- Drain order:
  1. `SetDrainingIfReady`;
  2. then together, while the transports are live (final review): `Front.BeginDrain()`; each TCP server's Shutdown, off the drain's goroutine, which turns keepalives off, closes its listeners, closes idle connections and sends one GOAWAY per h2 connection; and one GOAWAY per h3 connection;
  3. alongside, `exchange.Drain()`;
  4. hijacked H1 and forwarded websockets are tracked through the deadline;
  5. a forced close is bounded by ForcedCloseTimeout (final review): it cuts every public TCP socket before closing the servers, so no TLS close_notify can hold it, whether behind a full send buffer or a write wedged under TLS's write lock. nginx's worker_shutdown_timeout close sends none either (ngx_event_openssl.c's quiet shutdown on c->error);
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

Consolidated on 2026-11-19 from the units', round 2's and the final review's parity decisions, as their reviews corrected them, the parity harness's findings, and the list this section held before. The harness (`nginx/parity_test.go`) checks the front against the pinned lb image. A difference passes a run only when an entry of `parityAcceptances` (`nginx/parity_compare_test.go`) names it; each entry's id is in brackets. An item without an id is outside what the harness compares, and was accepted from a unit's or a fixer's decision and checked against the code. LBALT.md §11.4 has each item's code.

The front accepts these differences.

TLS:
- Groups: `X25519MLKEM768`, then nginx's `X25519:prime256v1:secp384r1`. Go's other default groups are left out, and Go ignores the configured order, which equals its own.
- TLS 1.2 suites are ECDHE with AES-GCM. nginx's `EECDH+AESGCM:EDH+AESGCM` also allows finite-field DHE, which Go lacks; with ECDSA certificates, DHE-RSA can't be chosen on nginx either.
- Within those suites Go picks AES-128-GCM, where OpenSSL under `ssl_prefer_server_ciphers` picks AES-256-GCM. crypto/tls ignores a server's order and has no TLS 1.3 suite setting [tls-suite-choice].
- Session tickets follow D6c: Go's per-process keys, for TLS and QUIC. nginx has `ssl_session_tickets off` and a per-interface session cache.
- ALPN is http/1.1 only for connect names, where nginx offers h2 [connect-alpn]. An offer of only http/1.0 or http/0.9 gets no_application_protocol, where nginx selects it; with http/1.1 also offered, the front negotiates http/1.1.
- An SNI ending in a dot fails the TCP handshake with decode_error, because crypto/tls refuses the ClientHello before GetConfigForClient; nginx drops the dot and serves the name. RFC 6066 forbids the dot.
- An SNI over 255 bytes gets the default server's certificate and is routed by Host; OpenSSL refuses it with unrecognized_name [long-sni-default-certificate].
- A certificate whose SANs don't cover its served name fails the block's start; nginx would serve it.
- `findLatestTls` breaks a version tie by the greatest directory name, ignores symlinked version directories, has no flat-file fallback, and searches the host's flat `<vault>/tls` after `<vault>/<env>/tls` and `<vault>/all/tls`.

Request parsing and limits:
- Header limits match nginx's totals (414 for a request line over 8 KiB; 400 for a header line over 8 KiB or a head over 64 KiB) but not its packing. nginx fills whole 8 KiB buffers and refuses some heads from about 33 KiB, when lines are just over 4 KiB each [header-buffer-packing].
- Past `MaxHeaderBytes` (twice `MaxRequestHeadersTotalBytes`, §7) plus Go's 4 KiB of read slack, about 132 KiB, Go's server answers 431 where nginx answers 400, or 414 for a long request line [max-header-bytes-431].
- max_headers (1000) is counted as nginx counts it, except that on h1 the count also takes in two lines Go's server may make up, which the handler can't tell from the client's own: the `Cache-Control: no-cache` that net/http's fixPragmaCacheControl adds beside a `Pragma: no-cache` sent without one, and the Host of an HTTP/1.0 request in absolute form sent without a Host line. A head of exactly 1000 lines with either is a 400 that nginx serves (`TestFrontE2eCountsTheHeaderLinesGoAdds`). Discounting the Cache-Control line would let the common browser-reload head of 1001 lines, with both lines sent, through where nginx answers 400. Past `MaxHeaderValueCount` (2000, §7), Go's server answers 431 where nginx answers 400.
- A request with Content-Length beside `Transfer-Encoding: chunked` is read as chunked (RFC 9112 §6.3) and forwarded re-framed, where nginx answers 400. Each request reaches the upstream once, as its own request [content-length-beside-chunked].
- Go's h1 and h2 servers and the vendored http3 merge repeated identical Content-Length values into one before the handler (net/http's fixLength, the h2 server's newWriterAndRequest, http3's parseHeaders). The front then forwards the request framed by that one length, where nginx answers a second Content-Length 400 on h1 and h2 alike (ngx_http_process_unique_header_line). Nothing is smuggled. Different values are refused before the handler: 400 on h1, as nginx answers, and a stream reset on h2 (PROTOCOL_ERROR) and h3 (H3_MESSAGE_ERROR), where nginx's h2 answers 400 [repeated-identical-content-length].
- Go's h1 server answers a repeated Transfer-Encoding 501, where nginx answers 400, and an Expect other than 100-continue 417, which nginx ignores.
- Go unfolds a header line continued with a leading space (obs-fold), and the front forwards the parts trimmed and joined by a space; nginx answers 400 [obs-fold].
- Go drops Transfer-Encoding from an HTTP/1.0 request and reads it as bodiless. Bytes sent as its body become the connection's next request, which the front forwards as its own, each framed by its length; nginx answers 400 [http10-transfer-encoding].
- The token bucket matches nginx's limit_req decision for decision, except below its 1 ms and milli-request granularity.
- At `LimitSettings.MaxAddressStates` the least recently used idle state is evicted. With every state in flight, a new address gets 429, as limit_conn does with a full zone.
- Each generation keeps its own limit state, so during a deploy overlap a client may briefly get up to twice its limit (D8).

Routing and redirects:
- No slash auto-redirect whose `Location` carries the internal listen port: the front answers that request 404, charged to the limits [slash-redirect].
- Names and redirect targets are lowercase, so a redirect's `Location` may differ from nginx's in letter case.
- On h2 and h3, `Host` is compared with `:authority` byte for byte. The harness compares distinct and equal names only.

Responses and errors:
- No `Server: nginx` header [server-header].
- No nginx HTML error pages: the front's own responses are the status text as text/plain, and the status codes match [own-error-page].
- No duplicate `Content-Type` on status routes. On the port-80 per-block status, the front replaces the upstream's only on 2xx and 3xx [status-content-type].
- The lb status JSON adds `"front":"lbalt"` (§2.6) [lb-status-json, lb-status-head-length]. It has no Last-Modified, ETag or Accept-Ranges [lb-status-static-file], so a Range or conditional request gets 200 [lb-status-range].
- A preflight's 204 has no `Content-Length: 0` [preflight-content-length].
- A 101 connect writes in-process carries none of nginx's `always` headers [in-process-switch-headers].
- `Connection: keep-alive` is left implicit on a persistent HTTP/1.1 response [keepalive-connection-header].
- An HTTP/1.0 request gets an HTTP/1.0 status line [http10-status-line].
- gzip's compressed bytes differ from zlib's; the decoded body and the headers match [gzip-bytes].
- A duplicate single-valued upstream header, such as a second Content-Type, passes; nginx drops it.

Forwarding and bodies:
- No duplicate `Connection` header to websocket upstreams: a single `Connection: upgrade`, with `Upgrade` only when the client sent one [websocket-upstream-connection].
- Connect in-process sees the client's own hop-by-hop headers and writes its own 101 head, where a block behind nginx sees nginx's `keep-alive` and `upgrade`. A client sending Upgrade without `Connection: upgrade` is upgraded behind nginx only [in-process-hop-by-hop].
- The `Trailer` request header is dropped [trailer-request-header].
- Request headers go upstream sorted by name, in Go's canonical case.
- net/http's fixPragmaCacheControl adds `Cache-Control: no-cache` beside a `Pragma: no-cache` sent without one, on h1 requests the front forwards and on upstream responses it relays; nginx passes only Pragma.
- A 101 to a request that asked for no upgrade is a 502, never retried.
- Keepalive pools, balancing state and max_conns are per block per process, not per worker.
- A reused keepalive connection that fails before any request byte is written is redone on a fresh connection to the same block, and isn't an attempt. One that fails after the request was written counts as one of D7's attempts and moves only to another block. nginx gives any error on a cached connection a free try, and with one server redoes the request there, so a single-block service or a per-block lb route answers that race with 502 where nginx retries.
- Bodies are held under D7, D19 and D21's process caps. A body that can't reserve is a local 503 before its first read, counted apart from upstream 503s and never retried.
- An h3 request without a content-length reserves its body's memory prefix only once a read returns bytes (LBALT.md §11.3 (c)). The vendored http3 gives every such request a body of unknown length, a GET's included, and can show a stream that ended with its head only through a read, where nginx's h3 peeks at the stream (ngx_http_v3_process_request_header). So a bodiless h3 GET holds nothing and is served while other bodies fill the budget, as on h1, h2 and through nginx. An h3 body with bytes that finds no room is the local 503 after its first read, not before it. Until the reservation, that read's bytes sit in the store's pooled read buffer (`BodySettings.ReadBufferBytes`), outside the budget.
- On h3, a request without a content-length on a streamed route streams chunked and is not retried, even when its stream ends with no data. On a buffered route, a stream that ends empty only after a wait goes unframed, where nginx sends `Content-Length: 0`. nginx peeks at the stream without waiting; the vendored http3 can't, and a read before the head would hold a client that sends its body only once the response begins.
- Spill files are unlinked as soon as they are created.
- On a streamed route the request head goes upstream at once, and the body's first read decides, as nginx's first pass does. An h1 body of declared length that this read returns whole, as Go's server returns one it holds whole with its head, is kept in memory under the body budget; it goes with its Content-Length and `X-UR-Request-Buffering: off`, and a 503 or failed attempt is retried (D7). A declared empty body is retried on every protocol. h2 and h3 bodies with data, and bodies that wait for 100 Continue, stream, as nginx streams them. Unlike nginx:
  - a chunked body streams chunked even when it came whole with its head, where nginx sends its Content-Length and retries a 503 [streamed-chunked-whole];
  - a declared body Go's server doesn't return in one read streams unkept: with its head, past Go's 4 KiB read buffer less the head, otherwise past `CopyBufferBytes` (32 KiB), where nginx's first pass holds up to client_body_buffer_size and a quarter (80 KiB);
  - a body that arrives whole only after its head may be kept and retried where nginx streams it;
  - a block that answers before the first read returns finds nothing kept;
  - a body with no room in the budget streams, unrefused;
  - on h2, a declared empty body whose stream ends after its HEADERS frame is retried where nginx streams it.
- A service both websocket and streamable has its bodies streamed, where nginx buffers them. No such service exists in main.

Timeouts and connections:
- A request body that stalls past client_body_timeout gets no response, as nginx terminates the request. On h1 the connection closes with nothing written, before any block, or while a streamed body is forwarded, a response being relayed included. On h2 the stream alone is reset and the connection serves on. Unlike nginx, Go's h1 server sends TLS close_notify before closing, where nginx's quiet shutdown sends none; Go's h2 server resets with INTERNAL_ERROR, nginx with PROTOCOL_ERROR; and h3, which nginx doesn't serve here, answers 408 (kept, owner 2026-11-19).
- send_timeout bounds each write as a whole: a handler write or flush, at most 32 KiB, and on h2 each frame written to the socket, through the server's write byte timeout, which closes the connection. nginx re-arms it on any progress, so a client reading slower than about 1 KiB/s is cut by the front and kept by nginx.
- A write with no deadline on connect's public TCP sockets ends once none of it has gone out for send_timeout (§7). A reader blocked behind such a write during a request head, such as the answer to a TLS 1.3 KeyUpdate from a client that stopped reading, is closed up to send_timeout (30 s) after nginx's read timer would close it: about 30 s in rather than at client_header_timeout (15 s). An idle one closes at keepalive_timeout, as in nginx. Accepted by the owner, 2026-11-19.
- Lingering close. nginx discards an unread body after any answer of its own for up to lingering_time (30 s), each read waiting at most lingering_timeout (5 s), then keeps the connection or, for a closing answer, shuts its write side and keeps discarding until EOF or 30 s. Go's h1 server has no lingering_time. It reads up to 256 KiB of the body, bounded by lingering_timeout, before an answer that keeps the connection, and keeps the connection if that was all of it. When the connection closes with the body unread, which the front ensures for every such case (`responseState.finish`), Go half-closes (a FIN on port 80, a TLS close_notify on 443), closes 500 ms later and reads nothing more. So:
  - the answer to a body that stalls comes after lingering_timeout, where nginx answers at once; this holds for the front's own answers, the forwarder's refusals and connect's in-process answers;
  - a client still sending 500 ms after its answer gets a reset where nginx goes on discarding. A client that reads while it writes (Go's client, curl, browsers) has the answer first; one that reads only after writing its whole body loses the answer once its upload outlasts 500 ms plus the socket buffers;
  - on a 403, 404, 405, 429, 503, 301, the preflight's 204 or the lb status 200, nginx discards the body and keeps the connection. Go's server closes it after the answer, with the half-close and wait, when the declared remainder is 256 KiB or more, or when it doesn't arrive within lingering_timeout. A small body that arrives whole is read, and the connection kept, by both;
  - a client expecting 100-continue that sends its body anyway after its own wait (curl after 1 s, Go's client after ExpectContinueTimeout), within lingering_timeout, keeps its connection on nginx, which discards the body; Go's server closes it after the answer, because it closes a connection it answers with an expected body still unread. A client that sends nothing is closed by both, after about lingering_timeout;
  - a forwarded answer that read its body whole and closes the connection while a pipelined request is arriving is closed at once by Go's server, and the client sees a reset after the answer, where nginx lingers. The pipelined request is served by neither;
  - in-process connect: nginx buffers connect's body before proxying, under client_body_timeout, and ends a stalled one with no response. The front gives connect's own answer, with Connection: close, once lingering_timeout cuts the read.
- On h2, the GOAWAY for keepalive_requests and keepalive_time goes out with the last stream's response head, not as that stream opens. With keepalive_timeout 0 it goes out once the connection is idle.
- keepalive_requests counts only requests that reach the front's handlers. On h2 a stream Go's server answers or resets itself is not counted and leaves the connection open.
- A request still in flight at the forced close can get a 502, where nginx closes without a response: the front's cut ends its forward while connect cuts the client's connection (§2.8 Drain order step 5), neither waiting on the other, so the 502 reaches the client only when it is written first.
- An h1 upgrade after the drain begins gets 503, any request the forwarder would tunnel included: an Upgrade header to a websocket service, with or without the Connection token.
- The drain deadline is `DrainTimeout` less `DrainStopReserve`, 58 m, so the forced close fits in warpctl's 60 m stop grace.
- The listen backlog is Go's, not nginx's 65535.

h2 and h3:
- A connect name over h2, reached only by a client that forces h2 past ALPN, is served in-process without an upgrade, as behind nginx.
- h3 has no nginx counterpart. It follows alt and D3, with these changes:
  - an authority other than an api name gets 421;
  - `/status` gets alt's 404, after the limits, and so does a path with no location, whatever the method;
  - nginx's method rule applies, where alt took any token;
  - the idle timeout is `keepalive_timeout` and the header limit 64 KiB, where alt used http3's defaults;
  - each api connection is charged to the per-address QUIC connection cap, and a request stream's HEADERS must arrive within client_header_timeout (§2.8; LBALT.md §11.3 (b));
  - keepalive_requests and keepalive_time don't apply;
  - early data is refused, and a resumed session reports `Used0RTT == false`.
- No name gets Alt-Svc.

Metrics and logs:
- The access log samples one request in `AccessLogSampleInterval`, at V(1), with no query, no client address and no hidden prefix. nginx logged every request.
- A request ended for a body timeout, or because its client went away, counts under status class `none`, and its access log line has status 0, where nginx logs 408 or 499.
- warpctl's lb version polls count `"front":"lbalt"` samples as neither a version nor an error, and don't poll when no lb interface is left on nginx.

Ports and packet paths:
- An lb_alt interface publishes exactly the §2.3 table: udp 4053 at the host and the router, and no udp 80. An lb-level stream port closes when its interface switches.
- HMARK hashes the whole source address, /128 on IPv6.
- With every block of an interface down, no public port answers, TCP 80 included (§2.5).
- With `tcp_fwmark_accept` or `fwmark_reflect` on, the split's replies carry their bucket mark; no rule routes by it.
- During a rollback, until the split is removed, public udp 4053 still reaches connect.
- Conntrack cleanup stays UDP-only.

The front reproduces these:
- `Host` normalization (lowercase, no port, one trailing dot removed), with nginx's host validation, else 400;
- header limits (414 and 400 at 8 KiB);
- nginx's max_headers, 1000 header lines: on h1 every kept line, Host and Transfer-Encoding included; on h2 and h3 every field but the pseudo-headers and Cookie. The 1001st line is a 400, but for the off by one above;
- a second line of Authorization, Proxy-Authorization, Content-Range, Expect, If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since or If-Range answered 400, and so a second Host field on h2 and h3, even one equal to `:authority`;
- on h2, a Content-Length that isn't a decimal length answered 400, as nginx's ngx_http_v2_run_request does; Go's h1 server and the vendored http3 refuse one before the handler;
- an h2 `:authority` and `Host` mismatch answered 400;
- two `Host` headers on h1 answered 400, by Go's server;
- the method rule (one or more of `A`-`Z`, `_` and `-`, else 400), checked first; TRACE and CONNECT answered 405;
- a path with no location answered 405 to a method other than GET, HEAD and POST, and 404 to those, as nginx's static module answers it, on the lb names' TCP 443 servers, the default server, and a service's path outside its prefixes; h3 keeps alt's 404;
- headers with any byte outside `[A-Za-z0-9-]` in their names dropped;
- upstream response headers over 4 KiB answered 502, not retried;
- gzip of `text/html` when the upstream sends none: GET and HEAD alike, statuses 200, 403 and 404, HTTP/1.1, no Via, the type tested as nginx's gzip_types reads the Content-Type (the value before a `;` only when spaces and `charset=` follow it, else the whole value, lowercased and untrimmed: ngx_http_upstream_copy_content_type). A HEAD gets `Content-Encoding: gzip`, a weak ETag, and no Content-Length, Accept-Ranges or Vary, with no body, forwarded and in-process; and Go's servers add no Content-Length of their own to any gzipped response;
- the request contract of LBALT.md §4.2: `X-UR-Forwarded-For` in nginx's `$remote_addr` text, the other ingress headers removed, `Host` set to `$host`, and `X-UR-Request-Buffering: off` on streamed routes. The normalized URI is re-escaped (NGX_ESCAPE_URI) only when the client's path held a `%` (quoted_uri) or a rewrite changed it, the nested streamable location under a prefix; otherwise raw bytes above 0x7f go upstream as received, normalized, connect in-process included;
- connect names reaching connect in-process only through connect's own routes, as a block's private router answers behind nginx: GET /, the websocket and H1+ upgrades included, reaches connect; another path answers 404, an upgrade to it included; another method on / answers 405 with `Allow: GET`; cookies are dropped;
- weighted least_conn, nginx's Ketama ring exactly, and D7's retries as amended; proxy timeouts as per-operation deadlines, the read armed once the request is sent;
- upstream keepalive: keepalive_requests, keepalive_time checked on return, keepalive_timeout, eviction of the least recently used;
- body framing, and 413 before reading a declared length over 16 MiB and as an unknown length crosses it;
- an h3 body that ends before its declared content-length answered 400 ("client prematurely closed stream"), as on h2;
- an h3 request with no content-length whose stream ends with no data sent to a buffered route unframed, as nginx's h3 frames none for a stream ended with its head (ngx_http_v3_process_request_header);
- on a streamed route, the request head sent upstream at once, and client_body_timeout bounding only the wait for the client's next bytes;
- a streamed body that fails on the client's side while its response is relayed ends that response short: a timed-out body terminates it, and a broken or oversized body ends it without its last bytes. The block's side is cut, and the refusal counted by its reason;
- a forwarded request whose client goes away, mid-body or while its block holds the answer, gets no response: h1 aborts and closes, h2 and h3 reset the stream. It is recorded with status class none, as nginx terminates it (c->error; 499);
- CORS, `Vary: Origin` and the preflight, the security headers with nginx's `always` rules, and frame options from the upstream's CSP;
- `/status` denied on service names, on the normalized URI; port 80; redirect aliases; the default server; the hidden lb routes; the lb status exemptions, in any case;
- `Connection: close` on the front's own h1 400, 413, 414, 500 and 501;
- a PROXY header on TCP 80 answered 400, and on TCP 443 failing TLS;
- a served name whose leaf is outside its validity window loaded and served, as nginx serves it (§2.7), and logged at error level;
- nginx's client, keepalive and send timeouts, and 60 s on websocket and streamed routes.

Not differences, though once listed as such:
- two Host headers on h1: both answer 400;
- a key that doesn't match its certificate: both refuse to start;
- the front's own `Date`: nginx hides the upstream's too.

Round 2 (2026-11-19) replaced three earlier lines that misread nginx: "a 408 goes out at once in both" on a body timeout, gzip "never for HEAD", and a streamed body that "always streams, and is never retried once begun". The final review (2026-11-19) replaced four more: a client that went away got "an unseen 502"; a leaf outside its validity window "fails the block's start"; the upstream path was always re-escaped; and a cached connection's failure was described as nginx moving to another block, where nginx's free try covers any error and, with one server, redoes the request there.

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

As of 2026-11-19, `lbalt/integrate` at server a297487df and warp bdacae4. LBALT.md §11 has the implementation notes.

- [x] base-server
- [x] base-warp
- [x] implementation units (merged onto `lbalt/integrate` in both repos; each passes its own tests there)
- [x] integration (§7): s-front, s-e2e, s-mon and s-car in server and w-int in warp, each implemented, reviewed and fixed, then merged with the integrator's decisions on top (LBALT.md §11.3)
  - the shared QUIC stateless reset key (d7f3643a0) is reverted (a29d3cd2d): one key shared by sibling sockets makes each a reset oracle for the others (RFC 9000 §21.11). D22 (b) is the follow-up
- [x] round 2, each unit implemented, reviewed and fixed, and merged (LBALT.md §11.3):
  - `lbalt/r2-forward` (869f0d563): a body that stalls past client_body_timeout ends the request with no response, on h1 and h2; a streamed request's head goes upstream at once, and a body its first read returns whole is kept and retried; a pool waits for a dial in flight when it closes
  - `lbalt/r2-http` (b99fc7b5a): gzip on HEAD as on GET; an h1 answer that ends its connection over an unread body goes through Go's half-close and wait; connect's in-process answers bound Go's read of an unread body
  - `lbalt/r2-monitor` (c6c0712bd): the lb status probe's attempt bound in its transport, its dial under the request, capped by the runner's command timeout
  - the parity entry `streamed-chunked-whole` (85101757a)
- [x] the final adversarial review (LBALT.md §11.1): eight dimensions, 32 findings, 30 confirmed (3 high, 7 medium, 20 low) and 2 refuted; each area fixed, re-reviewed and fixed again, then merged:
  - `lbalt/final-nginx` (9981127c4): no response to a client gone away; h3 body framing, short bodies and the reservation at first bytes; nginx's max_headers and unique headers; 405 for a path with no location; the upstream path escaped only as nginx escapes it; gzip by nginx's content type; a certificate outside its validity window served; the drain refusing every upgrade the forwarder would tunnel; the parity entries `obs-fold`, `http10-transfer-encoding` and `repeated-identical-content-length`
  - `lbalt/final-connect` (9ff29fe87): the forced close cutting the public sockets first, and the drain leaving idle closes to each server's Shutdown; start counting its supervisors before it reads as started; connect names in-process through connect's own routes; api QUIC connections under the per-address cap, their headers bounded by the vendored http3's ReadHeaderTimeout; writes without a deadline bounded by send_timeout; the monitor's public UDP bound and path key, command-timeout seams and `scope-excluded`
  - warp `lbalt/final-warp` (bdacae4): the enabling rule (§2.2), the first switch's wait (§2.5), connect workers reading no public address, IPv6 chains read alone, and load's chain-name, keepalive and service-name refusals (§7)
  - the integrator's a297487df: an h2 Content-Length nginx can't parse is a 400
- [x] package tests on the fix branches, 2026-11-19 (LBALT.md §11.5): nginx and its race run on `lbalt/final-nginx`; connect, monitor, router, the vendored http3 and the DB-backed connect LbAlt suite (377 s) on `lbalt/final-connect`; services and warpctl on `lbalt/final-warp`, whose tree bdacae4 has. Gofmt and vet clean throughout
  - [x] the same on the final merged tree (a297487df, bdacae4), 2026-11-19, with the DB-backed suite (329 s), the parity harness and the netns harness: all pass (LBALT.md §11.5)
- [x] parity harness (server `nginx/parity_*_test.go`, warp `warpctl/lbalt_parity_fixture_test.go` with golden copies of the fixture): 257 cases and no difference that no entry names, in 76 s, on `lbalt/final-nginx` and again on the final merged tree
- [x] network-namespace harness (warp `warpctl/lbalt_netns*_test.go`, 8 edge tests against real iptables, ip6tables, conntrack and ss): PASS on `lbalt/final-warp` in 93 s and on bdacae4 in 100 s, the first switch's wait included; the rollback keeps every block chain, and the harness's copy of the §2.3 table is checked against the workers'
- [x] §4 consolidated, this checklist updated, and LBALT.md §11 written
- [x] the owner's confirmation, 2026-11-19, of the final review's departures (LBALT.md §11.3 (a) to (h), recorded as amendments in its §6) and of the §4 line on the write bound's timing. The KeyUpdate-during-upload gap stays open, h3 keeps its 408, and the monitor's tls-expiry signal is the expiry alert (LBALT.md §11.6)
- [x] the exchange drain bound (server 0b75c1afa) and the five pre-existing warpctl fixes (warp d8a87f0), 2026-11-19 (LBALT.md §11.6)
- [x] the warp test-data scrub (warp 7e53b30) and the production values in warp's code comments (warp fc6ee3a), 2026-11-19 (LBALT.md §11.6)
- [x] D22 (b), one QUIC transport per socket group (§8), built on `feat/lbalt-d22b-quic-fanin-1119` and merged 2026-11-19
- [ ] the code review (LBALT.md §11.7)
- [x] merge, 2026-11-19: origin/main into `lbalt/integrate` (server 30b6935b7), then `lbalt/integrate` into main in both repos (server a3f37e085, warp 6aaace2), verified on main: builds and vets, the unit and race suites, parity (257 cases), the DB suite (23 tests) and the netns harness
- [ ] the owner's config:
  - services.yml: load the edited file with this warpctl first, then declare `services.connect.lb_alt: false` before any override (LBALT.md §11.3 (d)), and connect's `cores: 20` (D15)
  - monitor.yml: the `lbalt_status` and `public_udp` enrollment (LBALT.md §11.6)
  - the 4053 router rules of the two edges with no `router:` entry
- [ ] Gate A (LBALT.md §8 step 4). Nothing is deployed.

## 7. Integration decisions (2026-11-18)

Made when the units were merged, from what their reports raised.

Server:
- **Unexposed services.** `RouteTable.ServedNames()` and the name matcher skip a service with `exposed: false`: its names and its redirect aliases. nginx emits no server block for it, and warpctl requests no certificate for it. Its names reach the default server on TCP and fail TLS on QUIC. The route file still lists it, for the hidden lb routes.
- **Public TCP servers.** `Front.ConfigureServer(httpServer)` sets every `HttpSettings`-derived field: `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes`, the h2 stream limit, `DisableGeneralOptionsHandler`, keepalives off at `keepalive_timeout 0`, and `ConnContext`. Connect sets only `Handler`, `ErrorLog` and `BaseContext`, then calls it. The parity harness builds its servers the same way.
- **keepalive_requests (1000) and keepalive_time (1 h).** Per-connection state from `ConnContext`. Once either is reached, the response carries `Connection: close` on h1 and a GOAWAY on h2, as in the drain.
- **Header bytes.** `MaxHeaderBytes` is twice `MaxRequestHeadersTotalBytes`, so the front itself answers nginx's 400 and 414 inside that band. Go's 431 answers only beyond it (§4).
- **Header lines** (final review). `MaxHeaderValueCount` is twice `MaxRequestHeaderLines`, nginx's max_headers (1000), so the front itself answers nginx's 400 past 1000 lines, and Go's 431 answers only past 2000. Go counts every h1 line and every h2 field, pseudo-headers and cookie crumbs included (§4).
- **close_notify.** A client write that hit `send_timeout` must not hold the handler while TLS tries close_notify for 5 s. Connect's public TCP sockets fail any write after a timed-out write at once.
- **Writes without a deadline** (final review). On connect's public TCP sockets, a write its owner left without a deadline, hijacked or not, fails once none of it has gone out for `send_timeout`. The close_notify latch then fails every later write. These are crypto/tls's own writes on the reading goroutine (the answer to a client's KeyUpdate request, alerts) and net/http's few answers of its own: net/http clears the write deadline after each request, gorilla's and connect's H1+ upgrades clear it after their 101, and the idle, header and auth-frame read deadlines never reach a goroutine blocked in a write. nginx closes such a connection by its read timer (`ngx_ssl_handle_recv`) and bounds every blocked client write by `send_timeout`, an upgraded connection's too (`ngx_http_upstream_process_upgraded`). A write whose owner set a deadline keeps it; a zero deadline on these sockets means `send_timeout`, never forever.
- **Routing to the forwarder.** Routing sets `forwardOptions.host` to the `$host` it routed by.

warp:
- **`whodis_tlds`.** warpctl's constant `ur.xyz.` is the one source. services.yml gets no key, since the tld is out of scope (LBALT.md §10).
- **Connect host.** `--lbalthost` (§2.2). It is required with `--lbaltnets`.
- **Rollback keeps block chains** (§2.5).
- **lb version polls.** `deploy lb` reads services.yml. When no lb interface of the env is left on nginx, it skips the version poll and logs why. Otherwise it polls as before, ignoring `"front":"lbalt"` samples.
- **Validation** (`validateLbAlt`). The §2.3 table moves into services, as the one source for `LbAltPublicPorts`, the host DNAT and VyOS. Load refuses:
  - an lb_alt interface whose public ports fall outside the table;
  - service port 443 among the lb_alt service's http ports;
  - an lb interface name with `-` on a host where lb_alt is enabled, since chain names split on it;
  - a block chain `WARP-<ENV>-LBA-<IFACE>-<BLOCK>` over 28 characters, for any block of connect on any front interface of an lb_alt host, whose promotion would never complete (final review). The worker also checks the names before a generation starts;
  - front interfaces of an lb_alt host that give a service placed there with tcp port 80 and no keepalive of its own different keepalives (final review). They are compared as nginx applies them (`services.Keepalive.Resolve`, the comparison the route render makes), so times spelled apart but read alike (2m and 120s; 900s and the default's 15m) are one keepalive, and every keepalive without connections is none. Keepalives whose times don't resolve are compared as written;
  - a service named `lba` or `lbt`, or starting with `lba-` or `lbt-` (final review), whose block chains would read as an interface's split chain or the split's block chains.

Monitor: the lb status probe is a registered signal, with its classes in SIGNALS.md and its settings read from monitor.yml. A failed `/hello` on the public UDP signal has its own finding.

## 8. D22 (b): one QUIC transport per socket group

Started 2026-11-19 by the owner, ahead of canary evidence (LBALT.md D22, §11.6), on `feat/lbalt-d22b-quic-fanin-1119` from main a3f37e085. A group is the N = 4 SO_REUSEPORT udp sockets of one interface, family and udp port kind (quic or whodis).

### 8.1 The problem

- D10 gives each block N = 4 SO_REUSEPORT udp sockets per (interface address, port), and the front gave each socket its own quic-go transport.
- The kernel delivers each datagram to one socket of the group, by a hash of its 4-tuple scaled by the group's size. When a socket is lost or re-created, the size changes and most of the group's flows move. When a client's NAT rebinds, its flow moves.
- Under D22 (a), a moved flow reaches a transport that doesn't hold its connection IDs. That transport has no reset key, so it stays silent, and the client reconnects after its idle timeout, about 20 s.
- (b) feeds every socket of a group into one transport, so a moved flow still finds its connection.

### 8.2 Options

1. A plain `net.PacketConn` fan-in over the sockets, handed to quic-go as the transport's conn. quic-go wraps a conn that isn't OOB-capable as a basic conn: one datagram per read, no ECN, no DF bit (so no path MTU discovery), no GSO. That loss is the cost D22 names.
2. An OOB-capable fan-in outside quic-go. It would implement `OOBCapablePacketConn`, and the unexported `batchConn` so that quic-go's oob conn reads through the wrapper's `ReadBatch`. But quic-go configures a socket through `SyscallConn().Control` when it wraps it (ECN and packet info reception, DF, the GSO probe), so the wrapper's `Control` would have to reach every socket and replay quic-go's closures on each socket that joins later. It rests on an undocumented interface check and on replayed closures. Rejected.
3. Multi-socket support in the vendored quic-go: one conn type that `Transport` reads as its raw conn, over several sockets, each wrapped by quic-go's own `wrapConn`. **Chosen** for the quic port (§8.3).
4. Per-socket transports that share one connection-ID table. Each keeps its own listen loop, but `packetHandlerMap` and every connection's send conn change: a connection writes through the socket it started on, so it would still end with that socket. Larger and riskier than 3.
5. eBPF steering by connection ID: out of scope (LBALT.md §10, D22 (c)).

Whodis doesn't need 3: its transport reads the decode53 translation, which isn't OOB-capable, so quic-go already treats it as a basic conn. It gets one translation per group over a plain fan-in of the group's sockets (§8.4).

### 8.3 The vendored `ConnGroup`

`third_party/quic-go/urnetwork_conn_group.go`, a new file; no upstream file changes.

- `quic.NewConnGroup(localAddr)` returns the conn of one `Transport`. `ConnGroup.Add(conn)` wraps a socket as `Transport.init` wraps the conn it is given, and starts reading it.
- The `ConnGroupMember` that `Add` returns reports when its socket stops (`Done`, `Err`), and `Close` takes the socket out of the group and closes it. `ConnGroup.Close` closes every socket.
- `ConnGroup` implements quic-go's unexported `rawConn`, so `Transport.init` uses it as it is, and `net.PacketConn`.

**Reads.**
- Each socket has a reader goroutine running quic-go's own read for that socket. For a `*net.UDPConn` that is the oob conn's batched read: recvmmsg, 8 datagrams a call on Linux, with ECN bits and packet info taken from the control messages.
- Each packet goes into one queue of 256 packets. The transport's single listen loop reads the queue, as it read one socket. A full queue makes the readers wait, so a socket's receive buffer absorbs a burst, as when the listen loop is slow to read a lone socket.
- A socket's read failure ends only its reader. The socket leaves the group's write set at once, and its member's `Done` closes with the error. The transport never sees the failure. Temporary errors are retried, as `Transport.listen` retries them.
- `Transport.Close` stops the listen loop with a read deadline in the past, as on a socket. The group implements read deadlines over its queue and stays open; the front closes it after the transport.
- The cost is one channel handoff a datagram, and one listen goroutine for the group where each socket had one. A block's QUIC load is a few thousand datagrams/s (LBALT.md D10, §7.1), and the listen loop's work per datagram (a connection-id lookup and a queue push) is well under a microsecond. §8.7 measures it.

**Writes: which socket sends.**
- Every socket of a group is bound to the same address and port, so any of them sends the same datagram: the same source address and port, hence the same conntrack entry and the same reply mapping through the block's DNAT.
- A write leaves through the live socket picked by a hash of its destination address and port, seeded per group. While the group's sockets stay the same, one flow's datagrams leave through one socket: a qdisc such as fq classifies by socket, and could reorder one flow across sockets. The flows spread over the sockets' send buffers, as the per-socket transports spread them.
- When a socket leaves or joins, some flows change socket, once; at most a reordering.
- A socket closed between the pick and the write (`net.ErrClosed`) is skipped for the next live one. With no live socket, the datagram is dropped as the network drops one, and QUIC's loss recovery sends it again: quic-go ends a connection on any send error but EMSGSIZE. After `ConnGroup.Close`, writes fail with `net.ErrClosed`.
- Every other error passes through unchanged: EMSGSIZE, for path MTU discovery; GSO's EIO, after which quic-go sends that connection's batches one datagram at a time; and the first write's EPERM, which quic-go retries.

**GSO, ECN and packet info.**
- Each socket gets what a lone socket handed to a `Transport` gets: 7 MiB buffers, the DF bit, `IP_RECVTOS` and `IPV6_RECVTCLASS`, packet info only on a wildcard bind (the front binds exact addresses, so none), batched reads, and the GSO probe.
- `QUIC_GO_DISABLE_GSO` and `QUIC_GO_DISABLE_ECN` still apply; each socket reads them when it is wrapped.
- The group's capabilities (DF, GSO, ECN) are those of its first socket. A later socket whose capabilities differ is refused, and its re-creation backs off. A connection reads ECN and DF when it starts and GSO on every send, so the capabilities must not change under it, and every socket a GSO batch can reach must support GSO. A group's sockets are alike, so in practice none is refused.

### 8.4 The front

- Each udp group has a `frontQuicGroup`: its N sockets in the inventory, and its current transport, a `frontQuicRun`. The run holds the group's fan-in (a `ConnGroup` on the quic port; the whodis fan-in and the decode53 translation over it on the whodis port), the transport from `ConnectHandler.NewQuicTransport`, its listener, and the h3 servers of its api connections.
- On the quic port the transport reads the `ConnGroup`, so every socket keeps GSO, ECN, DF and batched reads.
- On the whodis port, `frontWhodisFanIn` reads each socket a whole datagram at a time, up to 64 KiB, so the translation reads what it read from its socket before (its own buffer cuts it, as a socket's read did), and writes through one live socket picked as in §8.3, with the same skip of a socket closed under a write and the same drop with none left. A temporary read error is retried after the sockets' backoff (`SocketRestartInitialDelay` doubling to `SocketRestartMaxDelay`), any other is the socket's loss. One translation per group sits over the fan-in, and the transport over the translation, as one sat over each socket before. The buffer sizes quic-go asks of the translation still reach the sockets, now every socket of the group, including those that join later, and still no DF or ECN. One translation now holds the pump state of every query its group receives, so an answer can use a query's id whichever socket the query reached; with a translation per socket, an answer sent through another socket had none. Its queue is `FrontServerSettings.WhodisFanInQueueLength`, 256 datagrams.
- Dispatch is unchanged, per transport: name classes; the handshake admission limit, through `NewQuicTransport`; the per-address cap (0x1001); 0x1000 for other served names; the h3 header timeout; `Allow0RTT = false`; the carrier by port kind.
- Stateless resets are unchanged. No transport has a `StatelessResetKey`, so none sends a reset, and each transport, now one per group, keeps quic-go's random per-transport token key; no key is shared. A packet of a group's connection reaches it from any socket of the group, and one that reaches another group's transport finds no key there.

**Socket loss and re-creation.**
- Per-socket supervision is unchanged (`superviseSocket`). A udp socket's run is its membership in its group (`frontQuicSocketRun`): `serve` waits until the socket stops, and `close` takes it out of its group and closes it. A re-created socket joins its group's current transport.
- A socket's loss costs no connection: its flows move to the group's other sockets and reach the same transport. On Linux the group's size still changes twice, now harmlessly.
- During the drain a lost socket stays lost, as before, and the group's other sockets carry its flows.

**Transport loss and replacement.**
- Each group's transport has its own supervisor (`superviseQuicTransport`), counted with the sockets'. Its accept loop ends when the front closes. It can also end when the transport fails under it: on the whodis port, when the translation ends, as a panic in one of its workers ends it. On the quic port nothing but the front's own close ends it.
- A transport that ends while the front serves is replaced. A new `frontQuicRun` is opened, at once after a healthy run, else after the sockets' backoff, and installed; then the old run is closed in the close order. Closing its fan-in closes its sockets, which are lost and re-created into the new transport. Its connections end, as a lost socket's did under (a).
- During the drain or a close, a transport that ends is closed, not replaced.

**Readiness** is unchanged: each socket counts on its own, up from its bind and join until its loss.

**Drain and close** keep §2.8's order.
- `beginDrain` gives each group's transport the drain once: the graceful h3 shutdown of its api connections.
- The forced close cancels the serving context. Each transport's accept loop returns, and its supervisor closes it: the listener, the h3 servers (bounded by ForcedCloseTimeout), the transport, the translation, then the fan-in and its sockets. A socket's run ends only once its group's transport has closed, so the h3 servers' CONNECTION_CLOSE frames still leave through live sockets. Every path to a transport's close goes through `frontQuicRun.close`.

### 8.5 Risks

- One listen goroutine per group handles the datagrams of all N sockets, where each socket's transport had one. At a block's load that is far from a core. If it ever binds, the next step is a listen loop per socket inside `Transport`, a larger vendored change.
- One translation per whodis group: its encode and decode run on one pair of goroutines per group, not N.
- `ConnGroup` is new vendored code on the packet path. A socket's removal races writes in flight; a write skips a closed socket. Its tests run under `-race`.
- Replacing a transport runs only after a failure. A test drives it through a hook.
- (b) doesn't help a flow that moves to another block, or one whose block restarts: those still reconnect after their idle timeout. HMARK hashes the source address alone (D10), so a rebinding that changes only the source port stays in its block.
- On Linux (2026-11-19), test binaries cross-compiled for linux/arm64 ran in an unprivileged ubuntu:24.04 container with only loopback: the vendored `TestUrnetwork*` tests, `TestUrnetworkConnGroupSendsAGsoBatchThroughItsSocket` included, and all 84 tests `-run '^TestFront'` selects passed, without `-race`. Linux's reuseport hash moving real flows between a group's sockets is still untested anywhere; the tests move flows themselves.

### 8.6 Tests

darwin delivers a reuseport group's datagrams to its newest socket, where Linux hashes each flow to one, so every test moves a flow deterministically: either a group narrowed to one socket while the connection is made, whose loss leaves only siblings, or sockets bound to distinct ports behind a relay the test steers, which also stands in for a NAT rebinding.

Vendored (`third_party/quic-go`, `GOFLAGS= go test -run '^TestUrnetwork' .`), each failing when its behavior is taken out of `urnetwork_conn_group.go`:
- `TestUrnetworkConnGroupKeepsAConnectionWhoseFlowMovesBetweenSockets`: a connection keeps echoing when its flow moves to another socket, and again after a rebinding changes its source port too;
- `TestUrnetworkConnGroupKeepsAConnectionThroughTheLossOfItsSocket`: a socket whose reads fail reports the error and leaves the write set, and its connection goes on through a sibling;
- `TestUrnetworkConnGroupWritesThroughOneLiveSocketPerDestination`: one socket per destination, another once it is lost, a drop with no error with none left, `net.ErrClosed` once closed;
- `TestUrnetworkConnGroupSkipsASocketClosedUnderAWrite`;
- `TestUrnetworkConnGroupPassesGsoEcnAndPacketInfoToTheSocket` and `TestUrnetworkConnGroupRefusesASocketWithOtherCapabilities`;
- `TestUrnetworkConnGroupReadsEverySocketWithItsOobPath`: each socket read through its oob conn, ECN bits included, with a lone socket's capabilities;
- `TestUrnetworkConnGroupOutlivesTheTransportOverIt`: `Transport.Close` leaves the sockets read on; the group's close ends them;
- `TestUrnetworkConnGroupSendsAGsoBatchThroughItsSocket`, Linux only, where the kernel has GSO; not run on darwin.

Front, no database (`connect/front_quic_group_test.go`, `front_quic_transport_test.go`, `front_whodis_test.go`):
- `TestFrontQuicGroupsFeedOneTransportEach`: one transport per group, reading a `ConnGroup` or a translation, never a socket;
- `TestFrontQuicKeepsAConnectionThroughTheLossOfItsSocket` and `TestFrontQuicKeepsAConnectionWhoseFlowMovesToASiblingSocket`: an h3 api connection keeps answering on the quic and the whodis ports when its socket is lost, when its flow is steered to another socket, and after a rebinding;
- `TestFrontDrainKeepsApiConnectionsThroughTheLossOfTheirSocket`: a request in flight finishes across its socket's loss during the drain, one GOAWAY, and nothing re-created;
- `TestFrontCloseSendsEachApiConnectionsCloseThroughItsSockets`: the forced close's H3_NO_ERROR reaches the client, so the sockets close after the transport;
- `TestFrontQuicGroupSocketsAreReadAsBound`, `TestFrontReplacesALostQuicTransport` (a quic `ConnGroup` closed under its transport, a whodis translation ended) and `TestFrontDrainSendsOneH3GoawayWhenItsTransportIsLost`, which keeps the vendored http3 fix's coverage now that a socket's loss destroys nothing;
- the whodis fan-in's five tests: every socket read and one written per destination, a whole datagram past quic-go's 1452 bytes, buffer sizes for later sockets, a temporary read error waited out, a socket closed under a write skipped.

Database-backed (`connect/lbalt_socket_test.go`):
- `TestLbAltQuicConnectionKeepsItsTransportWhenItsSocketIsLost` replaces the earlier test that a moved connection is never reset: the moved connection's next request is answered on the same connection, where (a) waited out its idle timeout; still no reset, since no transport has a reset key;
- `TestLbAltRebuildsALostUdpSocket` now also requires that the new client, whose flow the re-created socket takes, never connects again.

Changed: the earlier test of one GOAWAY when a socket is lost gave way to the two drain tests above; `TestFrontPublicSocketIgnoresAProxyProtocolHeader` keeps its behavior and leaves the structure to `TestFrontQuicGroupSocketsAreReadAsBound`; `TestFrontDrainIsNotHeldByAWedgedIdleConnection` reads each group's transport.

### 8.7 Performance

`TestConnectPerformanceCarriersLbAlt` (DB-backed, `WARP_TEST_ENV_FAIL_FAST=1`), on the darwin dev machine, 2026-11-19, from test binaries of a3f37e085 (before) and of this branch (after). The first before ran at 14:42; the rest ran back to back from 15:42, after, before, after, before, after, so each later pair saw about the same load. Each invocation reports the best of its three runs per carrier. The machine was shared and heavily loaded throughout.

| Pair | Load average (1 m) | QUIC before | QUIC after | whodis before | whodis after |
|---|---|---|---|---|---|
| 1 | 54 to 34; 60 to 52 | 5,399 msg/s, p50 0.63 ms | 7,810 msg/s, p50 0.61 ms | 18 msg/s, 59.3 ms | 19 msg/s, 53.7 ms |
| 2 | 52 to 63; 63 to 118 | 7,174 msg/s, p50 0.52 ms | 5,788 msg/s, p50 0.61 ms | 18 msg/s, 54.1 ms | 17 msg/s, 54.8 ms |
| 3 | 118 to 193; 193 to 129 | 2,171 msg/s, p50 1.08 ms | 2,174 msg/s, p50 0.84 ms | 16 msg/s, 64.6 ms | 17 msg/s, 58.7 ms |

Best of all: before 7,174 msg/s and p50 0.52 ms; after 7,810 msg/s and p50 0.61 ms. Inside single invocations the QUIC runs spread from 3.1k to 7.8k msg/s and their p50 from 0.52 to 1.40 ms, more than any pair differs, so the channel handoff (§8.3) shows no cost beyond the noise. The perf harness's QUIC case is one client pair on darwin, where every flow reaches the group's newest socket, so it measures the handoff and not the single listen loop under N sockets' load (§8.5).
