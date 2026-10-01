# Code Delta Review: Correctness, Performance, Backward Compatibility

**Scope:** `server` (3 commits ahead of `origin/main` + large dirty tree), `connect` (1 commit + dirty), `sdk` (1 commit + dirty), standalone `proxy` (working tree only).  
**Stance:** analysis only — no modifications made.  
**Date:** 2026-07-19

---

## Executive summary

This is a **coordinated multi-repo rollout** around three themes:

1. **Graceful drain / deploy continuity** (connect residents, HTTP/API, proxy socks/http/wg, taskworker)
2. **Redis memory & throughput** (peers/stream-hops key events, reliability sharding, stream companion joins)
3. **Client/SDK performance & UX** (DNS/SNI naming, multi-client window health, contract stats, provide control, JS retries)

Overall design quality is high: kill switches, metrics, extensive tests, and design docs (`CONNECTDRAIN2`, `PEERSSTREAMS2`, `PROXYDRAIN1`, `TASKDRAIN1`, `APIDRAIN1`, `RELIABILITY2`). The main risks are **operational prerequisites**, a few **hot-path / subscriber pressure** issues, and **behavior defaults that change production traffic** if deployed without the listed gates.

---

## What changed (by repo)

| Repo | vs origin | Themes |
|------|-----------|--------|
| **server** | +3 commits + large uncommitted set | Key-event peers/hops, connect drain+excuse, reliability pack/shard, stream companion join, monitor suite, HTTP/proxy/task drains, readiness, stats/blob, WG handoff models |
| **connect** | +1 commit + dirty | UpgradeMux SNI/HTTPS DNS, block actions, multi-client health/identity, stream reset reconcile, contract stats, `ResidentMigrate` proto |
| **sdk** | +1 commit + dirty | Device provide control, contract UI, grid reconcile, migrate transport, multi-client identity store hook, JS fetch retry |
| **proxy** (standalone) | dirty only | `drainState`, socks/http drain, WG `SeedEndpoints` / `InitiateHandshake` |

Module wiring is local (`replace => ../connect|proxy|sdk`), so these trees must ship **together**.

### Server commits ahead of origin

1. `916fcb79` — peers+stream hops: fix redis usage to lower memory throughput  
2. `6b76ede3` — stream: fixes  
3. `0d614959` — connect: drain improvements  

### Uncommitted server themes (selected)

- HTTP drain (`http_drain.go`), API readiness, taskworker drain  
- Proxy drain coordinator, activity prewarm, WG handoff, window identity  
- Stats package + MinIO blob abstraction  
- IP / router / monitor probe extensions  

---

## Critical findings

### 1. Key-event delivery defaults ON — silent 5‑minute peer/hop staleness if Redis notify is wrong

**Where:** `server/connect/resident.go` (`KeyEventDelivery.Enabled: true`, corrective poll **5m**), `redis.go` `SubscribeKeyEvents`, `Testing_*` uses `"Kg$sx"`.

**Issue:** With notify misconfigured or incomplete on any master, delivery silently degrades to the corrective poll. That is a large freshness regression vs PEERS2’s ~5s poll.

**Docs drift:** `PEERSSTREAMS2.md` still mentions `"Kghxz"` in one place while code/status require **`Kg$sx`** (`$` for string SETs on peer member keys, `s` for hop sets).

**Deploy gate:** confirm `CONFIG GET notify-keyspace-events` on **every** master before connect canaries; monitor probe for keyevent config drift is present but does not block deploy.

### 2. DNS local fallback can win over tunnel (privacy / wrong answers)

**Where:** `connect/ip_mux_upgrade.go` — default `LocalFallbackTimeout: 5s`, first successful reply wins.

**Issue:** Early A/AAAA after tunnel bring-up can resolve **off-tunnel** (and get cached for TTL). Split-horizon / captive portal risk. SVCB/HTTPS path is tunnel-only → inconsistent.

**Note:** Intentional for startup UX; still critical for a privacy product default on consumer devices. Hosted/proxy devices should keep mux/fallback off (as elsewhere intended).

### 3. Migration ordering: `connection_excused_new_count`

**Where:** `server/db_migrations.go` + reliability rollup/insert writers.

**Issue:** Connect/reliability binaries that write the new column **before** the migration will fail PG upserts. Must land migration first.

---

## High findings

### Redis / peers / reliability (server)

| # | Finding | Why it matters |
|---|---------|----------------|
| H1 | `SubscribeKeyEvents` **blocks** on `out <- message` (buf 1024) | Slow demux or storm → client OBUF kill → resubscribe + full `resyncAll` storms |
| H2 | Overlapping `resyncAll` trickle goroutines on resubscribe flaps | Extra full reads under Redis instability |
| H3 | Drain excuse is **one-shot GETDEL** | Double-bounce across two draining groups still invalidates reliability (ops stagger still required) |
| H4 | Server-initiated **nominate/replace** teardown is not excused / still removes peers | Only `Exchange.Drain` marks drained — non-drain rebalances still blip peers + reliability |
| H5 | `EnableNetworkPeers: true` after pubsub outage | Correct valve redesign (recent-auth count, independent of concurrent-client enforce), but re-enables fleet-wide fan-out cost; canary one block |
| H6 | Reliability: packed fields + 32 shards + HSCAN | Strong perf fix; dual-read of legacy + shard keys during deploy is correct; watch rollup wall time |

**Performance wins (server):**

- Peer valve no longer disabled solely by dormant 30‑day top-level clients.
- Key events cut steady-state poll GETs when notify is healthy.
- Reliability hash size/CPU fixed via binary fields + sharding + HSCAN (addresses 200–290ms HGETALL event-loop stalls).
- `transfer_contract.open` statistics target 10000 addresses planner “open=true is empty” catastrophe.

### Drain / deploy (server + proxy + sdk)

| # | Finding | Why it matters |
|---|---------|----------------|
| H7 | Connect drain: migrate + excuse + skip peer remove | Solid; older clients ignore `ResidentMigrate` and fall back to eviction+excuse |
| H8 | Task drain: finish → cancel → `ErrDrained` / no error backoff | Matches TASKDRAIN1; some “Task not run” races can still advance error counts |
| H9 | HTTP drain: keepalive grace + `Connection: close` | Correct; default grace 0 preserves old behavior unless CLIs set it |
| H10 | Proxy drain + WG handoff | Library + server wiring look consistent; **requires** DNAT flip before SIGTERM, export/seed endpoints, initiate handshake after conntrack flush |
| H11 | Provide-change excuse window ~7m, armed at write | Broad: real provide flaps in window also excused |

### Client / SDK

| # | Finding | Why it matters |
|---|---------|----------------|
| H12 | Multi-client `SendPacket` still full-parses + security + block decision | Real packet-rate CPU cost; SNI invalidation can thrash block cache |
| H13 | SNI reassembly is arrival-order, not TCP seq | Missed names on fragmented PQ ClientHellos (enrichment-only, not drop) |
| H14 | JS `fetchWithGetRetry` ignores `Request.method` | `fetch(new Request(..., {method:"POST"}))` can be treated as GET and retried |
| H15 | `CloseAllContractStats` is load-bearing | Teardown order bugs reintroduce sticky-open contracts in UI |
| H16 | `ProvideControlModeAuto` now maps idle → **Network** provide (not None) | Battery/discoverability product change; verify intended for all platforms |

### Protocol / compatibility

- **`TransferResidentMigrate = 28` / `ResidentMigrate`:** additive; unknown types ignored by old clients (documented).
- **StreamReset with hop snapshot:** new clients keep listed streams (P2P survive resident move); old clients cancel-and-reopen (same as prior empty reset).
- **ProvideMode comment:** correctly documents mask semantics — any remaining `<`/`max` over modes elsewhere remains a latent bug class (pre-existing risk, better documented now).
- **Redis keys:** additive (`{np_*}p:*`, drain excuse keys, pair stream markers, reliability shard keys).
- **`/status`:** `draining` must **not** match warpctl error regex; `error not ready` must. Tests exist for this intent.

---

## Medium findings

1. **NetworkPeerListener absolute poll** can be delayed under a steady drip of ready deltas (select fairness); hop listener’s `nextPollTime` is slightly better but advances before error backoff updates.
2. **Peer deltas leave local `eventId` stale** → corrective poll often full-resets after any churn (by design insurance; fleet cost to watch).
3. **Per-member peer keys** add Redis memory/write amp (expected for keyspace stream).
4. **Stream companion pair marker** is best-effort/stale-tolerant; good design, but pair key is separate slot (extra round trips).
5. **UpgradeMux claims HTTPS/SVCB** and returns empty for blocked names — closes hint bypass; some OS resolvers may hang longer on empty HTTPS.
6. **Reverse TTL raised to 1h** — good for long-TTL client DNS caches; more memory until LRU shed.
7. **Identity store restore** (proxy process restart): reuses client id/jwt/instance against same destination — correct for NAT resume; JWT expiry / provider rejection paths need careful fallthrough (mint on failure — verify).
8. **Window monitor identity cache** + RPC rebind on monitor change — fixes frozen remote grids.
9. **Provider local NAT flow caps** scaled by memory — important protect against remote abuse.
10. **Stats + MinIO blob** (uncommitted): hot path uses non-blocking Append; disabled without site dir/salt — safe, but new ops surface.
11. **Proxy `/status` 503 while draining** vs connect/api `"draining"` string — intentional; dashboards must not treat all non-200 as outage.
12. **Host name multi-part PSL incompleteness** — policy correctness for block wildcards.

---

## Low findings

- `DrainMigrateWindow == 0` panics on `rand.Int63n`.
- Proxy drain wait goroutine not `HandleError`-wrapped.
- Taskworker re-enters `Run` every 1s while draining (no-ops after guard).
- Connect status server may not set keepalive drain grace.
- StreamReset canceled sequences linger in maps briefly under churn.
- Logging volume on stream resets / multi health at V(1).

---

## Performance deep dive

### Wins (likely large in production)

| Area | Effect |
|------|--------|
| Reliability HGETALL → HSCAN + 32 shards + binary field keys | Removes multi‑hundred‑ms Redis event-loop stalls; lower memory |
| Peer valve = recent auth (14d), not all active top-level | Stops false-disable; still blocks true mega-networks from O(n²) shard fan-out |
| Key events (when healthy) | Replaces continuous 5s full-set polls with event-driven deltas + 5m insurance |
| Stream companion join via pair marker | Avoids contract scan on common non-stream pairs |
| StreamReset keep-listed | Avoids P2P teardown/rebuild on resident migration |
| Contract stats CloseAll | Prevents unbounded “open” UI/accounting after peer drop |
| Multi-client keep-unhealthy duration + grid reconcile | Stops dead providers pinning window slots |
| UpgradeMux peekClaim | Pass-through bulk skips full parse; DNS coalesce / inflight caps |
| Proxy drain vs hard kill | Keeps in-flight tunnels; WG handoff shortens dead-session wait |
| Task drain release claim | Deploy no longer holds healthy chains at claim max |

### Regression risks

| Risk | Condition | Impact |
|------|-----------|--------|
| Peer/hop 5m staleness | Key notify off/wrong | Feature latency regression |
| Resubscribe stampede | Cluster flaps / blocked pubsub | Full-read storms |
| Multi-client + SNI + block path | High pps egress | CPU/alloc on every packet |
| DNS off-tunnel | Startup window | Privacy + sticky wrong answers |
| Peers re-enabled fleet-wide | Large recent-auth networks near limit | Redis/CPU at valve edge |
| Reliability dual-key read | Rolling deploy only | Temporary extra HSCAN work |

---

## Correctness highlights (looks solid)

| Area | Assessment |
|------|------------|
| Excuse GETDEL one-shot + `ConnectionExcusedNewCount` outside `client_reliability_valid` | Correct; tests cover SQL/rollup |
| Skip `RemoveNetworkPeer` when `resident.drained` + TTL fallback | Correct race tradeoff |
| Provide-change dual path (local suppress + redis PC key) for MBB | Thoughtful; broader than original design |
| Keyspace flavor `K` + patterns + payload-as-event | Consistent with `Kg$sx` |
| Peer write order (meta HSET + member SET in TxPipeline) before events | Safe for delta HGET |
| Hop pipeline `INCR` then `SADD`/`SREM` | Kick sees bumped counter |
| Global ignore of `expire` (heartbeat) for peers **and** hops | Avoids TTL-refresh noise |
| Delta buffer full → Resync | No silent loss |
| Listener panic containment per tick/delta | Addresses 2026-07-15 permanent death |
| `SubscribeKeyEvents` ForEachMaster map lock | Addresses 2026-07-18 concurrent map crash |
| Task drain: finish → cancel → release claim with `ErrDrained` | Matches TASKDRAIN1 |
| API http drain: Connection:close grace then Shutdown | Matches APIDRAIN1 |
| Proxy readiness gate before DNAT flip + grace for socks/http | Matches PROXYDRAIN1 |
| Startup readiness latch (no exit on fail) | Correct deploy-revert behavior |
| StreamReset reconcile (keep listed streams) | Right fix for resident-migration P2P survival |
| DNS inflight coalescing + reverse map caps | Addresses real mobile memory issues |
| Blocker empty HTTPS answers | Closes ipv4hint/ipv6hint bypass |
| Proxy drainState and blackhole/relay cancel-by-close | Production-grade |
| SOCKS associate oversize drop-not-truncate | Correct UDP semantics |

---

## Backward compatibility matrix

| Surface | Compatible? | Notes |
|---------|-------------|-------|
| Wire protocol new message types | Yes | Old clients ignore migrate; StreamReset behavior differs by generation (both acceptable) |
| Redis key formats | Yes | Additive; rollup reads legacy reliability fields/keys |
| PG schema | Migration required | `connection_excused_new_count`; `open` STATISTICS |
| `/status` JSON | Extended | `draining` / `error not ready` semantics matter for warpctl |
| API HTTP defaults | Yes if grace=0 | CLIs that set keepalive grace change LB pool behavior |
| Peers feature | Behavior change | Was off; now on with new valve |
| Provide auto idle | Behavior change | Network provide when disconnected |
| Hosted `AllowDirect` | Harder off | HostedSafe + `NeverAllowDirect` |
| SDK/connect module APIs | Additive | Identity store optional; contract `Stream`/`HasStream` client-local |

---

## Cross-repo contract checklist

| Contract | Risk |
|----------|------|
| **StreamReset semantics** | New clients keep listed streams; old clients still full-teardown. Server should always send a full desired set. |
| **session_role / session_companion** | Optional; mixed versions OK if trial-decrypt retained. |
| **ContractStats.Stream / HasStream** | Client-only; no server proto change. |
| **UpgradeMux defaults** | Client SDK devices leak DNS on startup; **server proxy devices** should continue to use **nil mux** / no fallback (as comments state). |
| **SOCKS ASSOCIATE `network=="udp"` dial** | Still requires server `ConnectDialWithRequest` UDP → tun.Dial. If missing, associate is privacy-broken. |
| **Drain + WG handoff** | Server must orchestrate Drain → WaitIdle → cancel and PeerStatuses/SeedEndpoints across instances. Library alone is insufficient. |
| **UDP buffer sizing on server proxy_device** | Server should set appropriate UDP buffers on per-client tuns; client keeps large buffers. Mismatch → OOM at scale. |
| **Module versions** | Local replace directives couple `server` ↔ `connect` ↔ `sdk` ↔ `proxy`; ship together. |

---

## Cross-repo deploy checklist (ordered)

1. **PG migrations** (reliability column + optional early `SET STATISTICS` on `transfer_contract.open`).
2. **Redis:** `notify-keyspace-events "Kg$sx"` on all masters; run drift probe green.
3. **Canary connect block** with peers + key events + drain; watch:
   - peer listener resets, key-event resubscribes/resyncs
   - `drain_excuses_written` vs consumed
   - reliability excused vs organic `connection_new`
   - shard CPU / redis latency
4. **Ship connect + sdk** with matching proto (message 28); apps get migrate MBB; old apps still get excuse path.
5. **Ship proxy lib + server proxy** together for drain/WG handoff/activity prewarm/window identity.
6. **API / taskworker** readiness latch + HTTP/task drains; never exit on readiness fail (deploy reverts while old container serves).
7. **Multi-group host deploys:** still stagger externally — one-shot excuse does not fix double bounce.

---

## Solid / well-executed areas (not just risks)

- Concurrent map fix in `SubscribeKeyEvents` ForEachMaster (known 2026‑07‑18 crash).
- Listener panic containment (2026‑07‑15 silent death).
- Excuse never enters `client_reliability_valid` (column is metadata only).
- Drain skip of peer disconnect marker + TTL fallback (no offline blip for returning clients).
- Task `enterRun` WaitGroup race fix during drain.
- Proxy drain does not cancel in-flight tunnels when listeners close.
- Window identity persistence design is clear and opt-in.
- Strong test investment: exchange drain e2e, keyevent tests, reliability shard tests, proxy drain/wg, SDK provide/RPC, multi grid leak tests.

---

## Suggested priority before ship

1. **Ops gates:** Redis `Kg$sx` + PG migration (Critical).
2. **Key-event subscriber hardening:** non-blocking merge + single-flight resync (High H1/H2).
3. **Product call on DNS local fallback** for consumer defaults (Critical #2).
4. **Fix JS Request method retry** (High H14).
5. **Canary peers re-enable** and multi-bounce deploy policy (H5, H3).
6. **Profile multi-client egress** with mux+SNI under load (H12).
7. Decide **provide auto → Network** is intended product-wide (H16).

---

## Delta size context

- **server committed:** ~9.9k LOC across 59 files (3 commits).
- **server uncommitted:** ~2k LOC modified + 62 untracked (drains, proxy handoff, stats/blob, sim-latency, readiness).
- **connect committed:** ~5.4k LOC; **sdk committed:** ~4.1k LOC; **proxy:** drain/WG surface.

---

## Additional detail from deep dives

### Key-event transport (server)

- One `keyEventSubscriber` per exchange demuxes Redis keyspace notifications to registered peer/hop listeners.
- Per-member peer keys `{np_<networkId>}p:<clientId>` emit `set`/`del`/`expired`; heartbeats use `EXPIRE` and listeners ignore `expire`.
- Hop sets use dirty-kick + full read (no per-member restructure).
- Resubscribe forces trickled `resyncAll` over `ResyncSpreadTimeout` (default 30s).
- Prerequisite and rollout notes live in `PEERSSTREAMS2.md` and `monitor/SIGNALS.md`.

### Connect drain (server)

- `EnableDrainExcuse` / `EnableDrainCoordination` default true.
- Drain sequence: mark draining (refuse new nominate/connect), write excuses, jittered `ResidentMigrate` over migrate window, wait for voluntary leave, adaptive straggler sweep.
- Metrics: `drain_residents_remaining`, `drain_excuses_written` / consumed.
- Design: `CONNECTDRAIN2.md`.

### Reliability (server)

- Packed binary redis field prefix (32B hash + 16B network + 16B client + 1B counter index).
- 32 shards by last byte of client id; legacy unsharded key still read during rollup.
- New counter index 8: `connection_excused_new_count` — never passed into `client_reliability_valid`.
- Design: `RELIABILITY2.md`.

### Task drain (server, uncommitted)

- `DrainFinishTimeout` (60s) then `DrainCancelTimeout` (30s).
- `ErrDrained` skips error-count advance and backoff; claim released for immediate re-run.
- `ErrTargetNotFound` clamps backoff exponent for deploy version skew.
- `enterRun` prevents WaitGroup Add/Wait race when main re-enters `Run` during drain.

### Proxy (standalone + server)

- `drainState`: close listeners, track active, `WaitIdle`; HTTP refuses new work on keepalive with 503.
- WG: `PeerStatuses`, `SeedEndpoints`, `InitiateHandshake` for post-deploy RTT recovery.
- Server proxy: activity flush, prewarm, drain coordinator, window identity store for multi-client NAT resume.

### Client multi-client / SDK

- Keep-unhealthy duration forces remove of rank-kept but chronically bad clients.
- Replaced client cancel no longer emits `ProviderStateRemoved` that would kill the new live grid dot.
- `CloseAllContractStats` / `CloseContractStats` on channel teardown fix sticky-open UI contracts.
- SDK: provide control mode order in RPC Sync fixed; auto idle → Network; migrate platform transport make-before-break; multi-client identity store optional hook.

---

*End of review. No code modifications were made as part of this analysis.*
