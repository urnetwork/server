# MOVE — server-directed connection placement

Status: **design for review (2026-10-22). Nothing here is implemented.** §9 lists
the decisions to settle before implementation, each with a recommendation; §10
lists the open questions. Work through both before any code lands.

The platform tells a connected client "reconnect to this exact edge IP", and the
client opens the new transport (same hostname, SNI and certificate) before it
closes the old one. An **advisory** move is guidance the client may decline. A
**compulsory** move also says the server will close the old connection after
`MoveCompulsoryTimeout` (default 60 s). A mover inside every connect process
uses a Redis model of load per host to move idle clients slowly, at a bounded
rate, from overloaded hosts to less-loaded ones.

This extends the drain machinery in [CONNECTDRAIN2.md](../CONNECTDRAIN2.md): its
`ResidentMigrate` frame, client make-before-break and excuse markers.

Naming follows connect/CODESTYLE.md. In code and comments the feature is
"move" (`ResidentMove...`, `MoveCompulsoryTimeout`), never all caps. "MOVE" is
only this document's name.

## 1. Problem

**DNS does not place connections where we ask.** route53 weighted A/AAAA records
per edge interface, TTL 60 s, are the only placement mechanism. On 2026-10-19
(vault bdcb025) the per-host weights changed from 120/120/200/200 to
170/170/100/100 (edge-0/1/3/4), cutting edge-3+4 from 62.5% to 37% of the
weight. Their share of providers went **up**:

| `urnetwork_connect_provider_connections` by host | edge-0 | edge-1 | edge-3 | edge-4 | edge-3+4 |
|---|---|---|---|---|---|
| 10-17, before the change | 21.1% | 19.6% | 43.1% | 16.2% | 59.3% |
| 10-19 | 15.6% | 16.7% | 35.4% | 32.2% | 67.7% |
| 10-21 | 16.7% | 12.9% | 35.5% | 34.8% | 70.3% |
| 10-22 13:30Z | 12.6% | 17.9% | 23.7% | 45.8% | 69.6% |
| 10-22 19:30Z | 16.1% | 17.5% | 32.7% | 33.6% | 66.4% |

That held even though every connect block was drained twice on 10-22, so every
provider re-resolved. The likely causes are resolver caching, route53 health
checks, and clients always trying the same first address (sorted per family,
connect `net_dial_race.go:103-135`). We cannot verify any of them from here (Q1).
Whatever the cause, DNS influences only new connections, and providers stay
connected for days.

**Imbalance costs the whole platform, not just one host.** Connect uses about
1.4–1.9 cores per 1k providers. At the 10-22 peak it used 151 cores, while edge-3
and edge-4 ran at 88–93% CPU (load around 190–220 on 72 cores) and edge-0 sat
at 48% (96 cores). A saturated host stalls goroutines between the statements
of open Postgres transactions. Those sessions then sit "idle in transaction"
holding PgBouncer server connections, and PgBouncer queued 2–5k clients for
everyone.

**New hosts start empty.** edge-7 and edge-8 are being brought up. With DNS as
the only lever, they fill only as clients happen to reconnect.

What we want: the platform decides placement continuously, at a controlled
pace, without disturbing anyone with active traffic, and in a way that works
for drains and host evacuation too.

## 2. What exists today

Paths: `S` = server repo, `C` = connect client repo, `SDK` = sdk repo.

### 2.1 Residents: a new transport forwards, it does not take over

This is the crux of the design.

**Placement:**
- A client's resident (its routing, contract and stream state, and most of its
  CPU) lives where Redis `ncr_<clientId>` says. The gob value holds the
  instance, resident id, resident host/service/block and internal ports
  (`S/model/network_client_resident_model.go:19-33`).
- The TTL is 300 s, refreshed every 75 s (`S/connect/resident.go:1120-1163`).

**What a new transport on host B does when the resident is on host A:**
- It **forwards**: it dials A's internal exchange port over the LAN and becomes
  one more route of A's resident (`resident.go:3288-3301`, `2489-2503`).
- B takes over only after that B→A connection ends. It waits
  `ExchangeReconnectAfterErrorTimeout` (1 s), then calls
  `nominateLocalResident(residentIdToReplace=old)` (`resident.go:3336-3372`).
- `NominateResident` replaces the stored resident only when the instance
  differs or the stored resident id is the one named to replace
  (`network_client_resident_model.go:71-113`).

**Consequence:** a client that opens a transport to B and then closes its
transport to A leaves its resident, and the CPU, on A. B adds an exchange hop.
The resident moves only when A closes it or the B→A socket breaks. **A move
without an explicit resident handoff does not move load** (§5.3). Today's drain
migration has the same flaw: migrated clients forward back into the draining
resident until the straggler sweep closes it.

### 2.2 `ResidentMigrate` and client make-before-break

- **Message:** type 28, `TransferResidentMigrate` (`C/protocol/frame.proto:63-68`).
  Its only field is `migrate_time` (`C/protocol/transfer.proto:800-805`).
- **Server:** sent from the resident's internal client (ControlId) by
  `Exchange.sendResidentMigrate` (`S/connect/resident.go:1840-1857`) during
  `Drain()`.
- **Client:** acts only when the frame comes from the control source.
  - `ApiMultiClientGenerator.MigrateClientTransport`
    (`C/ip_remote_multi_client_api.go:1402-1580`) and the sdk's
    `deviceLocalProvider` (`SDK/device_local_provider.go:595-772`) build a
    replacement transport with the same URL.
  - They check the memory budget (`CanMakeBeforeBreakFrom`,
    `C/transport.go:1312-1350`) and wait up to 60 s for the new transport to
    connect.
  - Then they swap and close the old one, keeping the old one on timeout.
  - The replacement re-resolves the same name. Nothing chooses a host.
  - In the sdk an H3→H3 migration may break before it makes
    (`SDK/device_local_provider.go:717-729`).
- **Old clients** drop unknown frame types silently and ignore unknown fields
  (`C/transfer.go:4549-4557`, `C/ip_remote_multi_client.go:19373`).

### 2.3 Drain

`Drain()` (`S/connect/resident.go:1997-2109`) runs in this order:
1. Closes the admission gate (H1 only).
2. Writes excuse markers (`drain_excuse_<id>` and `drain_excuse_pc_<id>`).
3. Broadcasts `ResidentMigrate` with `migrate_time` jittered over 2 min.
4. Waits up to 2 min for zero connections.
5. Sweeps stragglers one at a time over 60 s.
6. Hard cap of 60 min.

Observed drains take 15–30 min. Two facts in the code explain the long tail:
- H3 and H3-over-DNS accepts have no draining gate.
- Inbound exchange connections register in `connections` with no gate.

The sweep exits only when both maps are empty (`resident.go:2058-2079`).

### 2.4 Dialing an exact IP while keeping the hostname

The building blocks exist:
- **H1:** `InternalDohDomains` with `internalDohResolver.wrapDialContext` passes
  only IP literals to the inner dialer, while TLS ServerName and Host keep the
  name (`C/net_http_internal_doh.go:197-214`, `304-317`).
- **H3:** already dials a `*net.UDPAddr` with a fixed `tlsConfig.ServerName`
  (`C/transport.go:3317-3350`).
- **`AltUrl`** overrides the dial host for H3 and the DNS-carried modes. H1
  ignores it (`C/transport.go:651-658`).
- **Server:** the monitor already probes an exact edge IP with a fixed SNI
  (`S/monitor/signal_edge_ipv6.go:166-170`).

### 2.5 LB and DNS

- **Routing:** each host's public interfaces go to that host's nginx, which
  proxies only to that host's blocks. The upstream is `least_conn` with block
  weights beta 1, g1 24, g2–g4 25 (`warp/warpctl/config.go:1592-1625`).
- **TCP 443:** every edge serves the same names and certificates
  (`connect.bringyour.com`, `*.connect…`, `connect-v4/v6`, the ur.network
  equivalents; `config.go:1970-2033`). So dialing any edge IP with SNI/Host
  `connect.bringyour.com` works.
- **UDP:** 443 and 4053 go through nginx stream with PROXY v2 to connect, which
  terminates QUIC TLS by SNI. Public UDP 53 is DNATed to 4053. All of it is per
  host.
- **DNS:** route53 weighted records per interface, `dns_weight`, TTL 60 s,
  health check every 10 s with threshold 3 (`warp/warpctl/dns.go:182-215`,
  `287-305`).

### 2.6 Edge inventory and self-knowledge

- **Inventory:** vault `services.yml` `versions[0].lb.interfaces.<host>.<iface>`
  carries `ipv4`, `ipv6`, `dns_weight` and `transparent`.
  - The monitor already parses it (`S/monitor/config.go:1468-1503`,
    `activePublicLBFromServices`).
  - Connect already reads the vault for TLS (`S/tls.go:86-92`).
  - IPv6 is not on every interface: edge-0 eno3 and edge-1 eno4 have none.
- **Self-knowledge:** a process knows `WARP_HOST`, `WARP_BLOCK` and
  `WARP_SERVICE`. It does **not** know which public IP a transport arrived on:
  nginx forwards no server-address header, and the H3 PROXY v2 destination is
  discarded after a family check (`S/connect/transport_pp.go:217-237`).

### 2.7 Load and activity signals

- **Per-process gauges:**
  - `connected_clients` (H1 only)
  - `resident_clients`
  - `provider_connections`
  - `exchange_active_connections`
- **Missing:** there is no per-host index of residents in Redis.
- **Activity:**
  - `ResidentForward.lastActivityNanos` counts payload only, with a 15 min idle
    timeout (`resident.go:3606-3653`).
  - `PingTracker` also counts pings.
  - There is no contract last-activity timestamp.

### 2.8 Capabilities and settings

- **No general capability negotiation.**
  - Precedents are per-feature auth fields: `h3_datagram_version` (Auth field 4)
    and `stream_lease_version` (field 9) in `C/protocol/transfer.proto:292-326`.
  - The H1 headers `X-UR-StreamLeaseVersion` and `X-UR-ClientInfo` carry
    `sdk_version` and `app_version`.
  - Nothing tells the server whether a client handles `ResidentMigrate`.
- **Settings:** server tunables live in `ExchangeSettings`
  (`S/connect/resident.go:390-623`). Production runs the code defaults; there is
  no per-environment override. Tuning therefore means a connect deploy, which
  today is about 2 hours of drains.

## 3. Goals and non-goals

Goals:
1. **Band:** keep each host's load (residents per unit of capacity) within a
   band around the fleet mean, continuously.
2. **Idle only:** balance only with idle clients. Never interrupt active traffic
   to balance.
3. **Make-before-break, no penalty:** every capable client moves
   make-before-break, and server-initiated moves cost providers no reliability.
4. **Bounded pace:** a maximum number of moves per minute (fleet, per source
   host, per target host), plus a kill switch that works without a deploy.
5. **Compulsory moves:** a deadline (default 60 s) for drains, host evacuation
   and emergency shedding.
6. **New hosts fill automatically** once they appear in the inventory.
7. **Old clients keep working:** advisory moves are never sent to them, and
   compulsory moves fall back to today's migrate-then-close.

Non-goals:
- **Choosing the process within a host.** nginx `least_conn` across blocks does
  that.
- **Controlling DNS weights automatically.**
- **Latency or geography-aware placement.** All edges are in one facility today;
  keep the inventory shape open for it (Q8).
- **Moving clients that reach the platform through an extender or a
  DNS-carried transport** (§5.2).

## 4. Overview

```
 source host A (above the band)        client C                      target host B (below)
 ─────────────────────────────         ────────                      ─────────────────────
 mover tick: pick idle resident C
 reserve budget (Redis script)
 SET move intent {moveId, fromResident=A, target=B}
 SET excuse markers (CONNECTDRAIN2)
 ResidentMigrate{advisory, targets=[B v4, B v6], moveId} ──►  C
                                       dial B's IP, SNI/Host = connect.bringyour.com ──► B
                                                                     auth, then ncr_ = A and intent targets B
                                                                     nominate(replace A's resident), GETDEL intent
 A's resident is replaced: it retires
 C's old transport on A now forwards to B
                                       C closes the old transport once the new one is up
```

A compulsory move works the same way, with `close_time` set. If C still has a
resident on A at `close_time`, A closes C's transports with reason "moved".

## 5. Components

### 5.1 Protocol

Extend `ResidentMigrate` (D1). Old clients ignore the new fields and keep
today's behavior.

```proto
enum ResidentMoveKind {
    // legacy migrate semantics: same name, server-chosen time (drain)
    ResidentMoveDrain = 0;
    ResidentMoveAdvisory = 1;
    ResidentMoveCompulsory = 2;
}

message ResidentMoveTarget {
    // 4 or 16 bytes. at most one target per ip family.
    bytes ip = 1;
}

message ResidentMigrate {
    uint64 migrate_time = 1;
    // ulid. correlates the intent, metrics and client dedupe.
    bytes move_id = 2;
    ResidentMoveKind kind = 3;
    repeated ResidentMoveTarget targets = 4;
    // unix millis. compulsory only: the server closes the old transport at this time.
    uint64 close_time = 5;
    // millis the client prefers the target address for reconnects to this hostname.
    uint64 prefer_duration = 6;
}
```

**Capability (D2):** the H1 header `X-UR-ResidentMoveVersion: 1` and a new H3
`Auth` field `resident_move_version` (the next free number after field 9). The
server keeps the highest version any of the client's transports declared on the
resident. Advisory moves go only to version ≥ 1.

**Close reason for compulsory enforcement:**
- H1 `TransportControlClose` reason 6 "moved", and WebSocket/QUIC close code
  4006 (following 4001 = client limit and 4002–4005 = authorization).
- Old clients read an unknown code as an ordinary close and redial at once,
  since the connection is older than the 5 s reconnect timer.

Ports are not in the message: H1 and H3 use 443, the same as the name. The
message carries no region or locality either (Q8). The client only learns a
target IP, and any locality is the server's choice of IP.

**Result report (Q4, decided):** a new client-to-control frame,
`TransferResidentMoveResult = 35`. The client sends it when it declines a move,
or when the replacement transport fails to connect. A successful move is
already visible as an arrival at the target.

```proto
enum ResidentMoveOutcome {
    ResidentMoveDeclined = 0;
    ResidentMoveFailed = 1;
}

enum ResidentMoveReason {
    ResidentMoveReasonOther = 0;
    ResidentMoveReasonExtenderCarrier = 1;
    ResidentMoveReasonDnsCarrier = 2;
    ResidentMoveReasonNoFamilyTarget = 3;
    ResidentMoveReasonMemoryBudget = 4;
    ResidentMoveReasonRecentMove = 5;
    ResidentMoveReasonTargetFailedRecently = 6;
    ResidentMoveReasonMobileBackgroundOrMetered = 7;
    ResidentMoveReasonMoveInProgress = 8;
    ResidentMoveReasonConnectTimeout = 9;
    ResidentMoveReasonDialError = 10;
    ResidentMoveReasonTlsError = 11;
    ResidentMoveReasonAuthError = 12;
}

message ResidentMoveResult {
    bytes move_id = 1;
    ResidentMoveOutcome outcome = 2;
    ResidentMoveReason reason = 3;
    // which family the failure applies to: 4 or 6, 0 when not family specific
    uint32 ip_family = 4;
    // failures only: millis from the dial start to the failure
    uint64 elapsed = 5;
}
```

**Server handling:**
- The resident's internal client receives the frame, like other
  client-to-control messages.
- It is accepted only when `move_id` matches the move this resident sent to
  this client (the resident keeps the last few move ids). Anything else is
  dropped and counted.
- It increments bounded counters (§5.9). A failure also feeds the target
  breaker (§5.5) and deletes the intent early.
- Client-driven, so it is never logged at the default level; detail goes to
  V(1).
- Old servers never see it: the server ships first (§7).

### 5.2 Client behavior

This applies to `ApiMultiClientGenerator.MigrateClientTransport` (used by
server/proxy hosted windows and device clients) and to the sdk's
`deviceLocalProvider` (providers).

**Accepting a move:**
- Accept it only from the control source (existing check).
- Dedupe by `move_id`, and ignore it while a migrate or move is already in
  progress (existing).

**Advisory: decline (D11) when any of these holds:**
- the transport reaches the platform through an extender, or uses H3Dns or
  H3DnsPump;
- no target matches the transport's IP family (pinned transports refuse
  other-family literals, `C/transport_family.go:269-283`);
- `CanMakeBeforeBreakFrom` refuses;
- a move completed within `MoveClientMinInterval`;
- this target failed within `MoveTargetFailHold`;
- (Q3) the app is backgrounded on a metered network.

**Execute:**
- At `migrate_time`, build the replacement transport with the same URL,
  settings and carrier mode, plus a one-shot dial override: hostname → target
  IP.
  - H1: a resolver override in the dial context, the same shape as
    `wrapDialContext`.
  - H3: dial the target `UDPAddr` with `ServerName` unchanged.
- TLS still verifies the original name, and Host and SNI are unchanged.
- Wait up to `MigrateConnectTimeout` (60 s) for the new transport to
  authenticate, then swap and close the old one. Transfer resends anything not
  acknowledged.
- On failure, keep the old transport and remember the target failure for
  `MoveTargetFailHold`.
- **Reporting (Q4):** every decline (including the ones above) and every failure
  sends one `ResidentMoveResult` to the control id with the reason. The send is
  best effort, with no retries: a lost report only means the server falls back
  to intent expiry.

**Stickiness (D10):** after a successful move, the client tries the target first
for reconnects to that hostname and family for `prefer_duration`. It keeps this
in memory only. Any dial failure falls back to DNS, and a newer move replaces
it. Without stickiness, the next natural reconnect re-resolves DNS and can undo
the move.

**Compulsory:**
- The client cannot decline. Paths that cannot use a target (extender, DNS
  carriers, no target in the family) do a same-name migrate instead.
- If the replacement is not up by `close_time`, the server closes the old
  transport and the normal reconnect loop runs, using the preferred target if
  one is set.

**Clients with several transports** (provider v4, v6 and standby): move every
platform transport whose family has a target.

**Fix to include:** the sdk's H3→H3 break-before-make path (§2.2).

### 5.3 Resident handoff at the target (D5)

**Intent:**
- When A sends a move, it writes `move_intent_<clientId>` = {`moveId`,
  `kind`, `fromResidentId`, `fromExchangeId`, `targetHost`}.
- The TTL is `MoveArrivalTimeout`: the `migrate_time` jitter plus the 60 s
  connect timeout plus a margin, about 3 min.
- It is a single key, so the cluster slot rules impose nothing.

**At B,** when `ResidentTransport` finds `ncr_` pointing elsewhere and before
dialing a forward:
1. Read the intent. It matches when:
   - **balance moves:** `targetHost` equals this process's `WARP_HOST` and
     `fromResidentId` equals `ncr_`'s resident id;
   - **drain moves:** any exchange other than `fromExchangeId`.
2. Nominate the local resident with `residentIdToReplace = fromResidentId`,
   through the existing compare-and-set path.
3. `GETDEL` the intent and count the arrival.
4. With no matching intent, behave exactly as today and forward.

**At A:**
- The replaced resident must retire promptly, not after the 75 s refresh poll.
  Until it does, two residents serve one client.
- Mechanism (Q7, answered): today a live resident learns of replacement only
  through the 75 s refresh. The process-wide key-event subscriber
  (`KeyEventDelivery`, on since 2026-07-18) serves network peers and stream
  hops. Subscribing it to every `ncr_` key would deliver over 1k events a second
  to every process, from refresh SETs plus new registrations. So B sends A a
  direct "resident replaced" message over the exchange as it takes over. B
  already has A's host and ports from `ncr_`. The 75 s refresh stays as the
  backstop.
- After retirement, the client's old transport on A re-reads `ncr_` and forwards
  to B until the client closes it.

**Continuity:** this is the same as drain migration. A new resident generation
starts, the `StreamReset` reconcile keeps relisted streams, and Transfer
resends. Balance moves pick idle clients, so this costs almost nothing.

**Side effect:** with drain moves carrying intents, a migrated client takes over
at the sibling as soon as it arrives, instead of forwarding back into the
draining resident (§2.1). That is useful before any balancing ships (§7,
phase 0).

### 5.4 Load model in Redis

**Publishing:**
- Every `MoveLoadPublishInterval` (15 s), each connect process writes one field
  of the hash `{connect_load}`.
- Field: `<host>/<block>/<exchangeId>`.
- Value: {`time`, `residents`, `providerResidents`, `transports`,
  `processCpu`, `hostCpuBusy`, `hostCores`, `draining`}.
- Host CPU comes from `/proc/stat`, which is host-wide inside the container.
  Cores come from `runtime.NumCPU()`.

**Computing load (D3):**
- Host load is the sum over the host's fresh, non-draining fields.
- Capacity is `hostCores` × an optional per-host `move_weight` from the
  inventory (default 1).
- Normalized load = residents / capacity. The fleet mean = Σ residents /
  Σ capacity over hosts with fresh records.
- The resident is the unit because it is where per-client CPU lives. Transports
  are published for visibility.

**Reading:**
- A record older than `MoveLoadStaleTimeout` (60 s) is ignored. A host with no
  fresh record is neither source nor target.
- The hash tag keeps the whole table in one slot, so one `HGETALL` per tick
  reads the fleet: about 30 fields today, 3 reads/s.
- A cleanup in the mover deletes fields stale for over 10 min. HEXPIRE needs
  Redis 7.4 (Q6).

**Budget:**
- A Lua script over `{connect_move}` keys holds per-minute window counters:
  fleet, per source host and per target host.
- It checks and increments atomically (all keys share the tag) and returns
  "allowed" or the cap that stopped the move.
- Windows use Redis `TIME` in the script.

**Runtime control (D14):**
- The hash `{connect_move}control` holds {`mode`: off | dry_run | on,
  `maxPerMinute`, `tolerance`, `evacuateHosts`}.
- It overrides the code defaults and is read every tick, so ops can stop or
  slow moves at once without a connect deploy.
- Code defaults apply when the key is absent.

**Cooldown:** `move_cooldown_<clientId>` with a TTL of `MoveClientCooldown`.

### 5.5 Mover (D6, D7, D8)

**Each tick:** every non-draining connect process does this every
`MoveTickInterval` (10 s):
1. Read the control hash and the load table, then compute host loads and the
   fleet mean.
2. **Source test:** own host load > mean × (1 + `tolerance`), or the host is in
   `evacuateHosts`. Optionally also require host CPU ≥ `MoveSourceMinCpu`.
3. **Own share:** the host's excess × (this process's residents ÷ the host's
   residents), capped by `MoveMaxPerTick`.
4. **Candidates (D7):** own residents that meet all of these:
   - capable;
   - resident for ≥ `MoveMinResidentAge` (10 min);
   - every forward payload-idle for ≥ `MoveIdleTimeout` (10 min);
   - no open streams;
   - no cooldown key;
   - on a direct carrier (not H3-over-DNS).
5. **Target:** a host with load < mean and CPU < `MoveTargetMaxCpu`, chosen with
   probability proportional to its deficit. On that host, pick one interface
   per family the client uses, weighted by `dns_weight`, and skip transparent
   interfaces.
6. **Commit the move:** reserve the budget, then write the intent, excuse
   markers and cooldown, then send `ResidentMigrate{kind=advisory}` with
   `migrate_time` jittered over the tick.

**Arrival-time moves (D9):** a client that has just connected to a source host
could be pointed at a target right away. That is the cheapest moment, before
there is state. It uses the same budget, minus the idle and age tests. Arrivals
that carry an intent are never re-moved.

**Guards:**
- **DB guard:** skip the tick when this process's Postgres pool acquire wait
  exceeds `MoveDbWaitPauseThreshold`, because every arrival writes to the
  database.
- **Target breaker:** failure reports are counted per target interface IP in a
  Redis window. When failures over sends to that IP exceed
  `MoveTargetBreakerRatio` (0.2, at least 20 sends), stop targeting the IP for
  `MoveTargetBreakerTimeout` (10 min). Counting by IP rather than host matters
  because one bad interface or router path must not idle a whole host.
- **Dry run:** do everything except the intent and the send, and count
  would-be moves by source and target.

### 5.6 Compulsory moves (D12, D13)

**Semantics:** `kind=compulsory` with `close_time = migrate_time +
MoveCompulsoryTimeout` (60 s). The deadline is per client, measured from that
client's own `migrate_time`, so a trigger that covers many clients spreads its
sends over a window. 20k clients must not all close at once.

**Who receives what:** capable clients get targets. Legacy clients get
`ResidentMigrate` with only `migrate_time`, the behavior they already have.
Both are closed at the deadline if their resident is still local.

**Triggers:**
- **Drain:** every local resident gets a compulsory move. By default the target
  is the same host (D13), whose new container accepts nominations.
- **Evacuation:** a host listed in `evacuateHosts` makes its processes move all
  residents, paced by the budget.
- **Emergency:** while host CPU ≥ `MoveEmergencyCpu` for N ticks, the mover may
  use compulsory moves on idle clients.

**Prerequisite for fast drains:** gate H3 and DNS accepts, and inbound exchange
connections, on draining (§2.3). With intents and those gates, a drain takes
about the send window + 60 s + the sweep, instead of 15–30 min.

### 5.7 Reliability and peers

- Every move writes the CONNECTDRAIN2 excuse markers (`drain_excuse_<id>` and
  `drain_excuse_pc_<id>`). The arrival's first sync then records
  `ConnectionExcusedNewCount` and suppresses the mechanical provide re-announce.
- The handoff teardown on A skips `RemoveNetworkPeer` (cause "move"), exactly
  like a drain.

### 5.8 Settings

All of these have code defaults; there are no globals. Server settings go in a
new `ExchangeMoveSettings` embedded in `ExchangeSettings`.

| Setting | Default | Meaning |
|---|---|---|
| `EnableMove` | false | master switch; the Redis control can only narrow it |
| `MoveLoadPublishInterval` | 15s | load record cadence |
| `MoveLoadStaleTimeout` | 60s | ignore older records |
| `MoveTickInterval` | 10s | mover cadence |
| `MoveMaxPerMinute` | 120 | fleet budget |
| `MoveMaxPerMinutePerSourceHost` | 60 | source cap |
| `MoveMaxPerMinutePerTargetHost` | 40 | arrival cap (protects a new host) |
| `MoveMaxPerTick` | 10 | per process |
| `MoveBalanceTolerance` | 0.10 | band around the fleet mean |
| `MoveSourceMinCpu` | 0 (off) | optional extra source condition |
| `MoveTargetMaxCpu` | 0.75 | never move onto a busier host |
| `MoveEmergencyCpu` | 0.95 | emergency shedding threshold |
| `MoveIdleTimeout` | 10m | payload-idle requirement |
| `MoveMinResidentAge` | 10m | skip fresh residents |
| `MoveClientCooldown` | 6h | per-client minimum time between balance moves |
| `MoveArrivalTimeout` | 3m | intent TTL |
| `MoveCompulsoryTimeout` | 60s | deadline after `migrate_time` |
| `MovePreferDuration` | 6h | stickiness sent to the client |
| `MoveDbWaitPauseThreshold` | 50ms | pause when pool acquire wait exceeds this |
| `MoveInventoryRefreshInterval` | 5m | reload `services.yml` |
| `MoveTargetBreakerRatio` | 0.2 | failures ÷ sends to one target IP that opens its breaker (minimum 20 sends) |
| `MoveTargetBreakerTimeout` | 10m | how long an open breaker skips that IP |

Client settings go in the connect repo, in `PlatformTransportSettings` or
`ApiMultiClientGeneratorSettings`, plus the sdk overlay. Per CODESTYLE, legacy
constants move into settings when touched:

| Setting | Default | Meaning |
|---|---|---|
| `EnableMove` | true | honor targeted moves |
| `MoveTargetFailHold` | 1h | do not retry a failed target |
| `MoveClientMinInterval` | 30m | client-side floor between advisory moves |
| `MigrateConnectTimeout` | 60s | existing; reused |

### 5.9 Observability

**Gauges, per process:**
- host normalized load and the fleet mean;
- host excess;
- mover mode;
- capable residents (to track rollout adoption).

**Counters:**
- `connect_moves_sent_total{kind,trigger,source_host,target_host}`
- `connect_moves_arrived_total{kind,target_host}`: handoff by intent at the
  target
- `connect_moves_expired_total{kind}`: no arrival within the intent TTL, which
  covers declines and failures
- `connect_moves_compulsory_closed_total`
- `connect_moves_declined_total{reason}` and
  `connect_moves_failed_total{reason,target_host}`, from result reports
- `connect_move_results_dropped_total`: reports with an unknown or foreign move
  id
- `connect_move_target_breaker_open`: a gauge per target IP, labeled by host and
  interface
- `connect_move_budget_refused_total{cap}`
- `connect_moves_would_send_total`: dry run

**`monitor/SIGNALS.md` entries:**
- host imbalance: max ÷ min normalized load;
- the move success ratio, arrived ÷ sent;
- compulsory closes per drain;
- the mover mode, so "mover on" is visible.

**Dashboards:** a placement panel by host, with providers, residents, CPU and
moves in and out.

### 5.10 Security and abuse

- Only the platform issues moves (the existing control-source check). Clients
  cannot request, steer or fake them, because the load counts are server-side.
- Targets come only from the vault inventory. The client also checks for a
  global unicast address, a matching family, and fixed ports.
- TLS verifies the original hostname, so a wrong or hostile IP cannot
  impersonate the platform. The worst case is a failed dial. An advisory move
  then keeps the old transport, and a compulsory one falls back to the same
  name.
- A per-client cooldown and the budget bound how much any bug can churn.
- **Result reports are client input:**
  - A report counts only for a move id the server sent to that resident.
  - Its reason is a closed enum.
  - Counters are bounded by label set, and nothing is logged at the default
    level.
  - The breaker opens per IP with a minimum sample, so one client cannot open
    it.

## 6. Correctness review

| Concern | Answer |
|---|---|
| Two residents serve one client during handoff | Compare-and-set nomination plus prompt retirement at A (§5.3, Q7). The residentId guard already protects stale cleanups. |
| Ping-pong between hosts | Hysteresis band, targets strictly below the mean, per-client cooldown, intent arrivals never re-moved. |
| Herd onto a new or empty host | Per-target arrival cap and target CPU guard. Arrivals are spread by `migrate_time` jitter. |
| A move lands during DB saturation | DB guard per process, the Redis kill switch, and a low default budget. Moves also reduce saturation by fixing CPU. |
| Client declines silently | The intent expires (counted). The cooldown stops retrying that client. |
| Intent consumed by an unrelated reconnect to B | Harmless: that is the desired placement. |
| Unrelated reconnect lands on A again | No intent at A. The cooldown stops an immediate re-move, and stickiness makes this rare. |
| Old clients | No advisory moves. Compulsory moves fall back to the legacy migrate plus a close at the deadline. |
| Extender or DNS-carried clients | Declined client-side, and excluded server-side when the carrier is visible (H3-over-DNS). |
| Resident lost mid-move (A crashes) | Normal recovery: the client's transport to B nominates, because `ncr_` lapses or the forward breaks. |
| Budget counters and clock skew | Budget windows use Redis `TIME`. Intents and cooldowns are TTLs. No process clock decides safety. |

## 7. Rollout

0. **Server-only fixes that help now** (no client change):
   - drain gates for H3/DNS accepts and inbound exchange connections;
   - intent-directed handoff for drain migration (§5.3);
   - compulsory close reason 4006.
1. **Load model and imbalance metrics** (no moves). This confirms the signal and
   the capacity normalization.
2. **Protocol and client library** (connect repo), then the sdk, apps,
   server/proxy and the sn miner, with the capability header and field. Watch
   the capable-residents gauge.
3. **Mover in dry run.** Compare would-be moves with the imbalance.
4. **Advisory idle moves on:** 60/min, then 120/min, with edge-3/4 as sources.
   Watch arrival ratio, CPU and PgBouncer.
5. **Compulsory moves** for drains and evacuation.
6. **edge-7/8 join:** inventory version v27. The mover fills them under the
   arrival cap.

## 8. Testing

Follows CODESTYLE: deterministic top-level tests, RFC 5737/3849 documentation
IPs and `.example` names. Every changed path gets a test, failing first where
feasible.

**connect repo:**
- the H1 and H3 dial override keeps SNI, Host and certificate verification
  (local TLS and QUIC servers on documentation IPs);
- make-before-break swap;
- every decline path;
- compulsory fallback to the same name;
- stickiness TTL and failure fallback;
- target-family handling for pinned transports;
- dedupe by `move_id`;
- old-field-only frames behave exactly as today.

**server:**
- the mover's selection is pure and table-driven over a fake load table: band,
  targets, budget split, eligibility;
- the budget script under concurrency;
- intent handoff in the split-exchange topology: the resident moves to B,
  relisted streams survive, A retires promptly, and no two residents act
  together;
- intent expiry;
- compulsory close at the deadline;
- drain with intents converging without the long tail;
- excuse consumption on arrival;
- kill switch and dry run.

**Load:** a move storm against the arrival cap, and a DB-guard pause.

## 9. Decisions to settle

| # | Decision | Recommendation | Status |
|---|---|---|---|
| D1 | Message shape | Extend `ResidentMigrate` | **decided 2026-10-22: extend `ResidentMigrate`** |
| D2 | Capability signal | Explicit `resident_move_version` (header + Auth field) | **decided 2026-10-22: explicit `resident_move_version`** |
| D3 | Load measure | Residents per capacity (cores × weight), CPU as a guard | **decided 2026-10-22: as recommended** |
| D4 | Target granularity | Host; server picks one interface IP per family | **decided 2026-10-22: host, one interface IP per family** |
| D5 | Making the resident move | Intent-directed takeover at the target | **decided 2026-10-22: intent-directed takeover at the target** |
| D6 | Who decides | Every process for its own residents, with a shared Redis budget | **decided 2026-10-22: per-process mover, shared Redis budget** |
| D7 | "Inactive" | Payload-idle 10 min, no streams, resident ≥ 10 min, cooldown 6 h | **decided 2026-10-22: as recommended; an open contract with no payload counts as idle** |
| D8 | Pace and band | 120/min fleet, 60 source, 40 target, ±10% band | **decided 2026-10-22: 120/min default (60/min at first enable), 60 source, 40 target, ±10%** |
| D9 | Arrival-time moves | Yes, after idle moves prove out | **decided 2026-10-22: yes, after idle moves prove out, sharing the budget** |
| D10 | Stickiness | Prefer the target for 6 h, in memory, fall back to DNS | **decided 2026-10-22: 6 h, in memory, DNS fallback** |
| D11 | Advisory decline rules | §5.2 list; mobile rule open (Q3) | **decided 2026-10-22: §5.2 list; mobile declines advisory moves while backgrounded or metered** |
| D12 | Compulsory semantics | Per-client deadline 60 s after `migrate_time`, close 4006 | **decided 2026-10-22: one `MoveCompulsoryTimeout` (60 s) for every trigger** |
| D13 | Drains | Compulsory moves, same host by default | **decided 2026-10-22: same host** |
| D14 | Runtime control | Redis control hash over code defaults | **decided 2026-10-22: Redis control hash, narrows only** |
| D15 | Which clients | All capable clients except probes | **decided 2026-10-22: all capable clients except server probes (hosted windows included)** |
| D16 | Excusal | Every move writes the drain excuse markers | **decided 2026-10-22: every move writes the excuse markers** |
| D17 | Inventory | `services.yml` lb interfaces, reloaded; optional `move_weight` | **decided 2026-10-22: `services.yml` lb interfaces, 5 min reload, optional `move_weight`** |

**D1. Message shape.** Extend `ResidentMigrate`, or add a new frame type
`TransferResidentMove`?
- *Extend:* the client already has one make-before-break path keyed on this
  frame, and old clients degrade to today's same-name migrate. That is right for
  compulsory moves and harmless for drains. The cost: the server must not send
  advisory moves to old clients, or they would reconnect via DNS for no gain.
- *New type:* old clients drop it silently, so compulsory moves to them would
  depend only on the deadline close, a gap. It also creates a second client
  path.
- **Recommend extending.** Drain becomes one kind of move.

**D2. Capability signal.** How does the server know a client honors targets?
- *Explicit version:* `X-UR-ResidentMoveVersion` on H1 and
  `resident_move_version` on the H3 Auth. Unambiguous and independent of
  release channels and forks.
- *`sdk_version` compare:* brittle across channels, forks and server-side
  clients.
- *None:* sends advisory moves that old clients turn into DNS reconnects.
- **Recommend explicit.**

**D3. What "loaded" means.** You suggested connections per host.
- *Raw count:* ignores that edge-0 has 96 cores and the others 72 (edge-7/8
  unknown).
- *Residents per capacity:* stable and predictable. Since CPU per provider is
  about the same everywhere (1.4–1.9 cores per 1k), it tracks CPU closely.
- *Host CPU:* is what actually hurts, but it is noisy, includes api, taskworker
  and kernel work, and lags as a control signal.
- **Recommend residents per capacity** (cores × optional `move_weight`) as the
  objective, with CPU as a guard: never move onto a host above
  `MoveTargetMaxCpu`, plus the emergency threshold.

**D4. Target granularity.** Host, interface or process?
- Processes are not addressable from outside: nginx picks the block.
- Interfaces differ in IPv6 availability and router path.
- **Recommend:** the mover picks a host, then one interface per family,
  weighted by `dns_weight`. The message carries at most one IPv4 and one IPv6
  address.

**D5. Making the resident move** (§2.1, §5.3). Without this, moves only add a
hop.
- *Intent-directed takeover at the target:* precise, immediate, and also fixes
  drain migration.
- *Source releases after arrival:* uses the existing break-then-nominate path,
  with a 1 s gap and a forced break.
- *Newest transport always takes over:* a general semantics change that would
  ping-pong residents between a client's own transports on different hosts.
- **Recommend intent-directed takeover.**

**D6. Who decides.**
- *Every process for its own residents:* no leader machinery, and activity
  knowledge stays local, with a shared Redis budget.
- *A central planner* (a Redis-lease leader or a taskworker task): global
  optimality, at the cost of a new leader and coordination path.
- **Recommend the per-process approach.** Slow convergence is intended, and the
  band, cooldown and budget prevent oscillation.

**D7. "Inactive."**
- **Recommend:** payload-idle on every forward for 10 min, no open streams,
  resident age ≥ 10 min, not moved in 6 h, a direct carrier, and capable.
- Providers with an open contract but no payload count as idle: a handoff costs
  them nothing visible.
- Open: also skip clients with any open contract?

**D8. Pace and band.**
- **Recommend:** 120 moves/min fleet-wide (about 0.6% of the providers on a hot
  host per minute), at most 60/min from one source host and 40/min into one
  target, and a ±10% band.
- Convergence: about 10k moves takes about 85 min.
- For scale, a drain today reconnects hundreds per second.
- Start at 60/min in phase 4.

**D9. Arrival-time moves.**
- Pointing a client that has just landed on a hot host at a cooler one is the
  cheapest move, because there is no state yet. It doubles that client's
  connect cost once, and stickiness then avoids repeats.
- **Recommend:** yes, but only after idle moves prove out, and sharing the
  budget.

**D10. Stickiness.** Without it, natural reconnects undo moves through DNS.
- **Recommend:** prefer the target for `prefer_duration` (6 h, server-supplied),
  in memory only, falling back to DNS on any dial failure.
- Open: should it survive an app restart?

**D11. Advisory decline rules.** The §5.2 list.
- Open: should mobile clients decline while backgrounded or on a metered
  network (Q3)?

**D12. Compulsory semantics.** The deadline is per client: `close_time =
migrate_time + MoveCompulsoryTimeout` (60 s). Triggers spread `migrate_time`
over a window. Enforcement is a close with reason "moved" (4006). Legacy
clients get the legacy migrate plus the close.
- Open: should N differ per trigger (drain, evacuation, emergency) (Q9)?

**D13. Drains.** Compulsory moves targeting the same host (the new container),
or use drains to rebalance?
- **Recommend the same host** by default, so deploys stay predictable and
  rebalancing stays the mover's job.
- Ship the drain gates either way.

**D14. Runtime control.** Settings are code-only and a connect deploy is about
2 hours of drains.
- **Recommend** a Redis control hash (`mode`, `maxPerMinute`, `tolerance`,
  `evacuateHosts`) that can narrow but never widen beyond `EnableMove` in code.

**D15. Which clients.**
- **Recommend** every capable client: providers, device clients and
  server/proxy hosted windows (they use the same library).
- Exclude server probes (capprobe, qualityprobe, egress probe), whose
  measurements a move would disturb.
- Open: should hosted windows be included (Q5)? They are short-lived and often
  active.

**D16. Excusal.**
- **Recommend:** every move writes the CONNECTDRAIN2 markers and skips the peer
  disconnect marker. Server-initiated placement must never cost a provider
  reliability or payout.

**D17. Inventory.**
- **Recommend:** `versions[0].lb.interfaces` from the vault `services.yml`
  (as the monitor reads it), reloaded every 5 min, so v27 with edge-7/8 is
  picked up without a deploy.
- Add an optional per-host `move_weight` for capacity differences beyond core
  count.

## 10. Open questions

1. **Why doesn't placement follow DNS weights?** (§1.) Investigated 2026-10-22,
   read-only:
   - **Authoritative answers follow the weights.** Over 200 queries per name
     against the route53 name servers, IPv4 answers for `connect`,
     `connect-v4` and `main-lb` went about 34% edge-0, 31% edge-1, 15% edge-3 and
     21% edge-4. The weights give 31.5 / 31.5 / 18.5 / 18.5.
   - **Defect: edge-3 never appears in AAAA answers**, although both its
     interfaces have IPv6 in the inventory. A route53 health check or record is
     wrong. That's a separate fix.
   - **Dialers mostly ignore those answers.** H1 handshake attempts, 08–14Z
     (accepted + client cancelled): edge-0 5.2k/min, edge-1 5.2k, edge-3 29k,
     edge-4 34k. So 86% go to edge-3/4. The median H1 session lasts about 7 s
     on every host. New provider connections (`urnetwork_connect_connection_new`)
     land 61% on edge-3/4.
   - **A few fleets dominate.** The three largest networks hold 73% of connected
     connections (77.6k, 34.7k and 22.9k) and sit 82%, 72% and 66% on edge-3/4.
     Smaller networks spread about evenly over the four hosts.
   - **Conclusion:** placement is decided by how a few large fleets resolve and
     dial (shared resolvers, address caching, or dial races that one router
     path wins), not by our weights. Moves fix this per client; DNS weights
     cannot.
   - Worth a separate look: those fleets' dial paths, and the client-cancelled
     to accepted handshake ratios (edge-0 1.03, edge-1 1.10, edge-3 1.50,
     edge-4 1.95), which show heavy dial racing on the hot hosts. Route53 health checks
   failing on edge-0/1 interfaces would remove them from answers. Client or
   resolver caching, or address ordering, could pin clients. Worth checking
   health-check history independently: moves correct placement, but a flapping
   health check is its own problem.
2. **Capacity:** cores (96/72), or explicit per-host weights? What are edge-7/8's
   core counts and interfaces?
3. **Mobile:** answered by D11: mobile clients decline advisory moves while
   backgrounded or on a metered network.
4. **Declines:** decided 2026-10-22: clients report declines and failures with
   a reason (`ResidentMoveResult`, §5.1), which also feeds the target breaker
   (§5.5).
5. **Hosted proxy windows:** answered by D15: included.
6. **Redis version on main:** probably 8.x. xops's cluster-reload test fixture
   models `redis_version:8.10.2`, so HEXPIRE (7.4+) is likely available. Confirm
   with `INFO server` before relying on it. The design doesn't depend on it:
   the mover's cleanup handles stale fields.
7. **Prompt retirement at A:** answered (§5.3). It is not wired today, so add a
   direct "resident replaced" exchange message from B to A, with the 75 s refresh
   as the backstop.
8. **More facilities later:** decided 2026-10-22: no region or locality field.
   The region comes from the target IP the server picks, and the client only
   needs that IP. If locality matters later, it lives in the server's target
   choice over the inventory.
9. **Compulsory timeout per trigger:** answered by D12: one 60 s setting.

## 11. Implementation inventory

**server:**
- `connect/resident.go`:
  - `ExchangeSettings` gains `ExchangeMoveSettings`;
  - `Drain()` sends compulsory moves and writes intents;
  - `ResidentTransport` takeover by intent (around 3288-3372);
  - prompt retirement of a replaced resident;
  - draining gates for inbound exchange connections.
- `connect/transport.go`:
  - capability parse (H1 header, H3 Auth);
  - draining gate in `connectQuic`;
  - close reason 4006.
- New `connect/move.go` (mover loop, eligibility, target choice) and
  `connect/move_load.go` (load publisher, inventory reload).
- `model/`: move intent (set, get, `GETDEL`), cooldown, `{connect_load}` table,
  `{connect_move}` budget script and control hash.
- `monitor/SIGNALS.md`, grafana placement panel.

**connect** (client library):
- `protocol/transfer.proto`: `ResidentMigrate` fields, `ResidentMoveKind`,
  `ResidentMoveTarget`, `ResidentMoveResult` (with its outcome and reason
  enums), Auth `resident_move_version`; `frame.proto`
  `TransferResidentMoveResult = 35`; `frame.go` `ToFrame`/`FromFrame` cases;
  regenerate.
- Transport dial override (H1 resolver override, H3 target `UDPAddr`) in
  `transport.go` and `transport_family.go`; stickiness state.
- `ip_remote_multi_client_api.go`: `MigrateClientTransport` honors kind, targets
  and deadline; settings.
- H1 header `X-UR-ResidentMoveVersion`.

**sdk:**
- `device_local_provider.go`: same handling for providers; fix the H3→H3
  break-before-make path.
- Ship in android, apple, windows, linux and wasm builds.

**Other consumers:** rebuild with the new library: server/proxy hosted windows,
the sn miner and validator, and connectctl.
