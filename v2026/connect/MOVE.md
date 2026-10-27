# MOVE — server-directed connection placement

Status: **design complete; decisions settled 2026-10-22 (§9, §10).
Implementation in progress:** the protocol is on connect main (09d446ee), and
the settings live in `connect/move_settings.go`. The one input still pending is
edge-7/8's core counts and interfaces (Q2), which v27 needs but the design does
not.

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
provider re-resolved. Q1 found why. Route53 answers do follow the weights, but
86% of H1 dial attempts still land on edge-3/4. The three largest networks hold
73% of connections and sit 66–82% on edge-3/4. Placement follows how a few
large fleets resolve and dial, not our weights. DNS also influences only new
connections, and providers stay connected for days.

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
- **Latency or geography-aware placement.** All edges are in one facility
  today. If locality matters later, it lives in the server's choice of target
  IP. The client only ever learns an IP (Q8).
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
    ResidentMoveKindDrain = 0;
    ResidentMoveKindAdvisory = 1;
    ResidentMoveKindCompulsory = 2;
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
    ResidentMoveOutcomeDeclined = 0;
    ResidentMoveOutcomeFailed = 1;
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
- Client-to-control frames are handled in `controller.ConnectControlFrames`,
  wherever the frame lands:
  - a connect resident (`connect/resident_controller.go`);
  - api `POST /connect/control`;
  - the proxy's in-process control (`localclient`);
  - taskworker egress control.

  So validation cannot use resident memory.
- A report is accepted only when its `move_id` matches the client's
  `move_intent_<clientId>` in Redis (§5.3). The intent outlives the client's 60 s
  connect timeout. Anything else is dropped and counted.
- An accepted report increments bounded counters (§5.9), deletes the intent (no
  arrival is coming), and, for a failure, feeds the target breaker (§5.5).
- Client-driven, so it is never logged at the default level; detail goes to
  V(1).
- Old servers never see it, because the server ships first (§7). That includes
  the proxy, whose in-process control path must ship in the same server
  release.

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
- (mobile, D11) the app is backgrounded, or the active network is metered
  (`MoveDeclineBackgroundOrMetered`).

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
- Mechanism (Q7): today a live resident learns of replacement only through the
  75 s refresh. The process-wide key-event subscriber (`KeyEventDelivery`, on
  since 2026-07-18) serves network peers and stream hops. Subscribing it to every
  `ncr_` key would deliver over 1k events a second to every process, from
  refresh SETs plus new registrations.
- Instead, A watches only the moves it started, since it wrote their intents.
  While a move is outstanding, A re-reads that client's `ncr_` every
  `MoveIntentPollInterval` (1 s), and again whenever one of the resident's
  transports closes. It retires the resident as soon as `ncr_` names another
  resident.
- This is bounded: balance moves leave at most about 360 outstanding
  fleet-wide (120/min × the 3 min intent TTL), so about 360 GETs a second in
  total. The poll stops when the intent expires.
- Drains do not poll. The draining process closes everything by the deadline,
  and a taken-over resident there only idles until then.
- The 75 s refresh stays as the backstop.
- A direct "resident replaced" exchange message from B would remove the poll.
  It is left as a later optimization, because it needs a new internal frame and
  connection path.
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

All of these have code defaults; there are no globals. Server settings live in
`connect/move_settings.go` and are embedded in `ExchangeSettings`:
`ExchangeMoveSettings` holds the move primitives, and `ExchangeMoverSettings`
holds the mover. The code switch is the ceiling. The mover's mode is off until
an operator sets the Redis control, so a deploy alone changes no placement.

| Setting | Default | Meaning |
|---|---|---|
| `EnableMove` | true | ceiling for every move behavior; the Redis control can only narrow it |
| `EnableDrainMoves` | true | drains send compulsory same-host moves with intents (D13); the Redis control can turn it off |
| `MoveDefaultMode` | off | the mover's mode while the Redis control is absent: off, dry_run or on |
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
| `MoveEmergencyTicks` | 3 | consecutive ticks at the threshold before shedding |
| `EnableArrivalMoves` | false | arrival-time moves (D9), enabled after idle moves prove out |
| `MoveIdleTimeout` | 10m | payload-idle requirement |
| `MoveMinResidentAge` | 10m | skip fresh residents |
| `MoveClientCooldown` | 6h | per-client minimum time between balance moves |
| `MoveArrivalTimeout` | 3m | intent TTL |
| `MoveIntentPollInterval` | 1s | source re-reads `ncr_` for its outstanding moves |
| `MoveCompulsoryTimeout` | 60s | deadline after `migrate_time` |
| `MovePreferDuration` | 6h | stickiness sent to the client |
| `MoveDbWaitPauseThreshold` | 50ms | pause when pool acquire wait exceeds this |
| `MoveInventoryRefreshInterval` | 5m | reload `services.yml` |
| `MoveTargetBreakerRatio` | 0.2 | failures ÷ sends to one target IP that opens its breaker |
| `MoveTargetBreakerMinSends` | 20 | minimum sends in the window before the breaker can open |
| `MoveTargetBreakerTimeout` | 10m | how long an open breaker skips that IP |

Client settings go in the connect repo, in `PlatformTransportSettings` or
`ApiMultiClientGeneratorSettings`, plus the sdk overlay. Per CODESTYLE, legacy
constants move into settings when touched:

| Setting | Default | Meaning |
|---|---|---|
| `EnableMove` | true | honor targeted moves |
| `MoveTargetFailHold` | 1h | do not retry a failed target |
| `MoveClientMinInterval` | 30m | client-side floor between advisory moves |
| `MoveDeclineBackgroundOrMetered` | true | sdk on mobile: decline advisory moves while backgrounded or metered |
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
| Two residents serve one client during handoff | Compare-and-set nomination plus retirement at A within about 1 s (§5.3 poll). The residentId guard already protects stale cleanups. |
| Make-before-break overlap trips the client limit | The concurrent-client limit counts a client once by client id (as CONNECTDRAIN2 established for migration), so the overlap does not. |
| Result report arrives at a process other than the source | Validated against the Redis intent, not process memory, so any process that runs `ConnectControlFrames` handles it. |
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
- old-field-only frames behave exactly as today;
- a result report is sent for each decline and failure reason;
- the mobile background or metered decline (sdk).

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
- kill switch and dry run;
- the source's `ncr_` poll retires a replaced resident within the interval and
  stops when the intent expires;
- result reports through each control path (resident, api `/connect/control`,
  proxy `localclient`), accepted only with a matching intent;
- the target breaker opens at the ratio and minimum sample, per IP.

**Load:** a move storm against the arrival cap, and a DB-guard pause.

## 9. Decisions (settled 2026-10-22)

Each decision below keeps the options and reasoning that led to it. Every one
went as recommended, with the refinements noted.

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
- **Decided:** an open contract with no recent payload counts as idle.

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
- **Decided:** memory only, so a restart re-resolves through DNS.

**D11. Advisory decline rules.** The §5.2 list.
- **Decided:** mobile clients also decline advisory moves while backgrounded or
  on a metered network. Compulsory moves still apply.

**D12. Compulsory semantics.** The deadline is per client: `close_time =
migrate_time + MoveCompulsoryTimeout` (60 s). Triggers spread `migrate_time`
over a window. Enforcement is a close with reason "moved" (4006). Legacy
clients get the legacy migrate plus the close.
- **Decided:** one `MoveCompulsoryTimeout` (60 s) for every trigger. Triggers
  that cover many clients spread their send times instead.

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
- **Decided:** hosted windows are included. They are short-lived and often
  active, so most of their moves will come from arrival-time moves (D9).

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
     edge-4 1.95), which show heavy dial racing on the hot hosts.
2. **Capacity:** decided by D3 and D17: host cores × an optional per-host
   `move_weight`. **Still pending from the owner:** edge-7/8's core counts and
   interfaces, for the v27 inventory entry.
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
7. **Prompt retirement at A:** answered (§5.3). It is not wired today. The
   source polls `ncr_` for its own outstanding moves (1 s, bounded), with the
   75 s refresh as the backstop. A direct exchange message is a later
   optimization.
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
  - the source's `ncr_` poll for its outstanding moves, and retirement of a
    replaced resident;
  - per-resident capability (`resident_move_version`);
  - draining gates for inbound exchange connections.
- `controller/connect_controller.go`: a `ConnectControlFrames` case for
  `ResidentMoveResult`, validated against the Redis intent.
- `connect/transport.go`:
  - capability parse (H1 header, H3 Auth);
  - draining gate in `connectQuic`;
  - close reason 4006.
- New `connect/move.go` (mover loop, eligibility, target choice) and
  `connect/move_load.go` (load publisher, inventory reload).
- `model/`: move intent (set, get, `GETDEL`), cooldown, `{connect_load}` table,
  `{connect_move}` budget script and control hash, and target breaker
  counters.
- `monitor/SIGNALS.md`, grafana placement panel.
- Release: connect, api, taskworker, the proxy (its in-process control path)
  and alt (its embedded exchange and API) ship the server side together.

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
- The mobile background or metered decline (`MoveDeclineBackgroundOrMetered`),
  wired to the platform's app-state and network-type signals.
- Ship in android, apple, windows, linux and wasm builds.

**Other consumers:** rebuild with the new library: server/proxy hosted windows,
the sn miner and validator, and connectctl.

## 12. Implementation notes (2026-10-23)

What landed differs from the sections above in these places. The sections
above are the design as decided; these notes record what the code does and
why.

**Where it lives:**
- connect `main` 9c9f9f42: the client (`resident_move.go`,
  `transport_move.go`, the dial overrides in `net.go` and
  `transport_family.go`, the window generator's `MoveClientTransport`).
- sdk `main` 99758df1: the provider's moves, the H3 migration fix and the
  mobile decline.
- server: the primitives (`connect/resident_move.go`,
  `connect/transport_moved_close.go`, `connect/edge_inventory.go`,
  `model/resident_move_*.go`, `controller/resident_move_result.go`), the mover
  (`connect/move*.go`, `model/connect_move_*.go`), and the wiring
  (`connect/move_exchange.go`, `connect/run.go`).

**Alt (decided during implementation):**
- H3 and whodis are canonical on alt (connect `EXTENDER.md` L1, L4), and
  every deployed space derives an alt url. A move redirects only the platform
  host's edges: a carrier the alt url sends to alt never dials a move's target
  or a preference. A strict move's replacement parks those carriers, so it
  arrives at the target over H1, and they dial alt again once the move
  concludes.
- A resident on an edge whose client reaches it only through alt is still a
  candidate. Moving it moves the resident's CPU, and the client's alt
  carriers then forward to the new resident.
- Alt names each listener's carrier to the connect handler (plain QUIC is H3,
  whodis is H3 under dns), so those residents are direct carriers only when
  they really are.
- Only the connect service runs the mover and the load publisher. Alt's
  embedded exchange runs neither, and its hosts carry no `-edge-` name, so
  they are never targets.

**Load model and mover:**
- Evacuation: an evacuating host's residents stay in the fleet mean but its
  capacity does not. With the §5.4 mean as written, a balanced fleet has no
  host below the mean, so an evacuation could never find a target.
- The control's tolerance can only widen the band, since a narrower band moves
  more clients.
- Emergency moves go only to capable idle clients. A legacy client re-resolves
  the same name, which §1 shows lands mostly on the hot hosts again.
- Arrival-time moves (D9) are noted at nomination and sent at the next tick,
  first within the tick's share. Arrivals by intent are never moved again; an
  evacuation still moves them.
- Dry run reserves budget, so its would-send counts show real pacing, but
  writes no cooldown, intent or frame.
- New settings: `MoveLoadCleanupTimeout` (10 min) for stale load records and
  `MoveTargetBreakerWindow` (10 min) for the breaker's send window.
- `move_weight` is `versions[0].lb.move_weights.<host>`, beside
  `lb.interfaces`: warp reads every key under `lb.interfaces.<host>` as an
  interface.
- A process keys its load record by its inventory host name
  (`Exchange.MoveHost`), resolved after the inventory's first load, so the
  table joins the inventory whether `WARP_HOST` carries the domain or not.
- `providerResidents` counts residents with a transport that declared provide
  intent. The edge carries the declaration across exchange connections.
- Payload idleness (D7) is read from the resident's forwards: any forwarded
  frame is activity. An idle client has no forwards, so it reads as idle.
- DB guard: the mean acquire wait of the main pool between ticks. A pool with
  every connection checked out and no acquire completing reads as having
  waited the whole interval.

**Primitives:**
- The intent also stores the targets: a failure report names only an IP
  family, so the intent is where the failed IP is found.
- The intent lives `MoveArrivalTimeout` past the move's `migrate_time`, not
  past its write, so a drain client jittered late in the window still finds
  it.
- Drain moves do not feed the target breaker: they point clients at the
  draining host's own addresses, and counting them would open every host's
  breakers on each deploy.
- A drain's deadlines are capped at the drain's own, and a drain move replaces
  any balance move still outstanding on the resident.
- The drain's switch is the Redis control's `drainMoves`; a drain that cannot
  read the control keeps the code switches, since its clients must still
  leave.
- Probes (capprobe, qualityprobe, the egress probe) open their tunnels through
  `qualityprobe/providertunnel`, which turns moves off, so no probe declares
  the capability (D15). Evacuations and drains still reach them as the legacy
  migrate.

**Client:**
- The sdk H3→H3 make-before-break fix is not possible as stated: when the
  memory budget is blocked, connect forbids holding two full H3 working sets.
  Instead a migration the platform directs never breaks: the old carrier
  stays, the replacement closes, and the platform closes the old carrier at
  its own deadline. An advisory move in that state is declined with
  `MemoryBudget`.
- Only a failure in the carrier the old transport used holds the target or
  fails the move, so a target is not blamed for a carrier blocked on the
  client's own network.
- One report per move, sent when the move concludes. A compulsory move refused
  for the memory budget sends none, so the intent stays for the reconnect.
- The stickiness fallback to the name happens on the next dial attempt.
- VLESS is a relay like an extender: strict dials skip it, and a move it
  carries is declined with `ExtenderCarrier`.
- The sdk's `SetAppBackgrounded` and `SetNetworkMetered` exist on
  `DeviceLocal`. Until the android and apple apps call them, no device
  declines for that reason.

## 13. Placement of platform-hosted clients (2026-10-25)

**Problem.** On 2026-10-25 edge-3 and edge-4 ran at 92–96% CPU with 157k and
124k residents. edge-0 held 48k at 49% CPU on 96 cores, and edge-1 held 44k at
72%. Contract creation took 1.4–2.5 s per reply as saturated connect processes
held Postgres transactions open. Counting the lb's established connections by
source showed that the two hosted proxy hosts alone held 33.3k of edge-3's
92.6k connections and 28.1k of edge-4's 80.3k, against about 7k each on edge-0
and edge-1. The proxies resolve the platform name through DoH, route53 answers
one weighted record per query, and a large resolver caches that answer for the
60 s TTL. So each proxy host's windows land on whichever edge the cache held,
and the identity churn (about 226 new window identities a second) refills the
same edges. The mover could move these clients afterwards, but a client that
runs on the platform can read the load model itself and be placed right the
first time.

**Design (owner, 2026-10-25).** A client the platform hosts consults the load
model and dials the least-loaded edge. The proxy's hosted windows do, and so do
the provider quality and capability probes.

- **Client (connect `platform_placement.go`):** `PlatformTransportSettings.
  Placement` is a `PlatformPlacement`: `PlatformPlacementTargets(host)` answers
  at most one address per family, and `PlatformPlacementFailed(host, ips)`
  reports a failed dial.
  - **Precedence:** with no move target or move preference, every H1 and plain
    H3 dial attempt asks the placement. The answer stands in for the platform
    host exactly like a move target: the literal is dialed while TLS SNI and
    the HTTP host keep the name. A move outranks a placement.
  - **Without moves:** placement works with or without `EnableMove`, so
    probes, which keep moves off (D15), are placed as they connect.
  - **Stickiness:** the client's `ResidentMoveTracker` keeps the answer per
    family for `PlacementPreferDuration` (6h), apart from move preferences, so
    reconnects return to the edge that holds the resident.
  - **Failure:** a failed dial drops the kept answer, holds those addresses on
    the client for `PlacementFailHold` (10 min) and reports them, so the next
    attempt asks again. With every address offered held, the name resolves.
  - **Alt:** alt-redirected carriers are never placed, as with moves.
- **Server (`server/edgeplacement`):** an `EdgePlacement` reads `{connect_move}control`
  and the `{connect_load}` table every 10 s, and the `services.yml` inventory
  every 5 min. Its first read happens in the constructor.
  - **Fleet:** the fleet is computed as the mover computes it: fresh,
    non-draining records; capacity = cores × `move_weight`; the mean over the
    capacity of hosts that are not evacuating.
  - **Choice:** an arrival goes to a host whose load is below the band edge,
    mean × 1.1, in proportion to its headroom in residents. Hosts at ≥ 0.8 CPU
    (or with CPU unknown) yield while another host with headroom is below
    0.8. With no headroom anywhere, capacity decides.
  - **Address:** one address per family is weighted by `dns_weight`.
  - **No answer:** it answers only for `connect.<services domain>`. It gives
    none before the first read, once its snapshot is older than 2 min, or
    while the control sets `placement` to off.
  - **Holds:** an address whose dial failed is held process-wide for 2 min.
  - **Metrics:** `urnetwork_edge_placement_{placements,misses,dial_failures,model_reads}_total`
    and `urnetwork_edge_placement_hosts`.
- **Control:** `{connect_move}control` gains `placement` (true/false, on/off).
  It can turn placement off fleet-wide at once and never on past the code
  switch (`EnablePlacement`, default on). The mover ignores it.
- **Wiring:**
  - The proxy main builds one `EdgePlacement` per process.
    `ProxyDeviceManagerSettings` hands it to every device, and
    `DeviceLocalSettings.PlatformPlacement` passes it to each window's
    transport settings. A platform device has no provider, so only windows are
    placed. The proxy forces IPv4 for platform traffic, so its windows dial the
    placed IPv4 address.
  - Each taskworker capability and URL probe pass builds its own
    `EdgePlacement` for its tunnels (`providertunnel.Config.PlatformPlacement`)
    and closes it when the pass ends.

**Tests.**
- **connect `platform_placement_test.go`:** placed dial keeps the hostname;
  stickiness; placement without moves; failure → report, hold, name; a move
  preference outranks placement; pinned family; alt never redirected; tracker
  separation.
- **sdk `device_local_placement_test.go`:** window settings carry the device's
  placement.
- **server:**
  - `edgeplacement/*_test.go`: fleet as the mover reads it; headroom
    proportionality; balanced fleet by capacity; CPU ceiling; held addresses;
    DNS weights; platform-host match; freshness and aging; control off and
    code switch; failed read keeps the snapshot; evacuation; process-wide
    hold; inventory reload interval.
  - `model/connect_move_control_test.go`: placement narrowing.
  - `proxy/proxy_device_placement_test.go` and `cli/proxy/main_test.go`: the
    process placement reaches every device and window.
  - `qualityprobe/providertunnel`: probe windows carry the placement with
    moves off.
  - `taskworker/work/probe_edge_placement_test.go`: every probe pass's
    tunnel config is placed.

**Rollout.** Deploy proxy and taskworker; connect needs nothing new. The kill
switch is `HSET {connect_move}control placement off`. Measure:
- per-host `urnetwork_connect_resident_clients` and the lb's connections from
  the proxy hosts;
- `urnetwork_edge_placement_placements_total` by `edge_host`;
- misses by reason;
- edge CPU;
- contract reply latency.

The proxies' windows rotate every few minutes, so placement should move most
of their 61k edge-3/4 connections within tens of minutes, without moves.
