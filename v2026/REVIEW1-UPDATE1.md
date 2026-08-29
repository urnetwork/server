# REVIEW1-UPDATE1 — verification of REVIEW2/REVIEW3 + adjacent issues

**Date:** 2026-07-19  
**Method:** Re-read `REVIEW2.md` and `REVIEW3.md`, re-traced each high-severity claim to the cited (or corrected) code paths, then hunted for *similar* patterns elsewhere in the same delta.  
**Stance:** analysis only — no implementation changes.

This file is an **addendum** to `REVIEW1.md`. It does not replace REVIEW2/REVIEW3; it grades their confidence and adds issues they under-specified or missed that sit next to the same mechanisms.

Legend for verification status:

| Status | Meaning |
|--------|---------|
| **CONFIRMED** | Reproduced against current tree; lines/behavior match the claim |
| **CONFIRMED (nuance)** | Core claim true; scope, severity, or fix detail needs adjustment |
| **PLAUSIBLE** | Mechanism present; production frequency / timing unproven |
| **PARTIAL** | Claim mixes true and false sites |
| **DISCONFIRMED** | Not true as stated (or wrong site) |

---

## 1. Executive take

REVIEW2 and REVIEW3 are largely accurate on the P0/P1 set, and they surface several defects that REVIEW1 either missed or under-ranked:

1. **sim-latency prod-write hazard (P0)** — confirmed; not in REVIEW1.  
2. **`IsProFresh` inside auth txs (P1)** — confirmed on the hot client-auth paths; some lower-QPS sites in REVIEW2/3 need nuance.  
3. **Orphan-RST use-after-pool-return (P1)** — confirmed; classic alias bug next to an already-correct copy on the SYN path.  
4. **WG handoff consume-before-produce (P1)** — confirmed against warp deploy order + one-shot GETDEL.  
5. **Stream Lua `EXPIRE` TTL as nanoseconds (P2, memory)** — confirmed via go-redis `writer.go` encoding; newly re-copied into `joinStream`.  
6. **Key-event corrective contract holes** (hop expiry phantoms, missed peer expiry, delta-does-not-advance-eventId) — confirmed; stronger than REVIEW1’s “5m corrective poll is insurance.”  
7. **`SET … KEEPTTL` can resurrect a TTL-less peer member key (P2)** — confirmed; adjacent to the PEERSSTREAMS2 member-key design.

Ship stance from REVIEW3 still stands: **hold unrestricted rollout until P0/P1 are fixed or explicitly gated.**

---

## 2. Double-check table (REVIEW2 / REVIEW3 top findings)

### P0

| Finding | Status | Notes |
|---------|--------|-------|
| sim-latency no local-env guard | **CONFIRMED** | `connect/sim-latency/main.go` only `setIfUnset("WARP_ENV","local")`; never rejects non-local. Contrasts with `test_util.go` hard panic outside `local`. Mutating path applies migrations, provisions balances, can upsert reliability weights + client scores. |

### P1

| Finding | Status | Notes |
|---------|--------|-------|
| `IsProFresh` inside `AuthNetworkClient` txs | **CONFIRMED** | `network_client_model.go`: Tx at ~269 / ~546, `IsProFresh` at ~413 / ~655. Chain: `UpdateProNetwork` → `loadProNetwork` (`server.Db` second conn) + `setProNetworkCached` (`server.Redis` with up to 60s retry). Concurrent-client gate is correctly *before* the tx; Pro stamp is not. |
| Same pattern on other JWT-mint paths | **PARTIAL** | **Inside Tx:** `auth_model.go` AuthCodeLogin ~1626; `network_model.go` UpgradeGuest ~1249/1344; `device_association_model.go` DeviceConfirmAdopt ~894. **Outside Tx:** `auth_model.go` AuthVerify ~897 (Tx closes ~891 before `IsProFresh`). REVIEW2’s “four sites” list should not treat AuthVerify as in-tx. |
| Orphan RST pool UAF | **CONFIRMED** | `parseIpv4`/`parseIpv6` document slice alias of pooled packet. Orphan path: `MessagePoolReturn(ipPacket)` then `receiveCallback` with `tcp.sourceIp`/`destinationIp` (`connect/ip.go` ~1883, ~1998–2005). SYN path *does* copy IPs (~1929–1933) — adjacent inconsistency. Tests use independent `net.IP` values. |
| WG handoff consumed before produced | **CONFIRMED** | Apply: one-shot `TakeProxyWgHandoff` after initial sync (`cli/proxy/main.go` ~171–181). Export: `AddBeforeExit` at drain end (~123). Warp order: ready → redirect → drain. Test uses export-before-apply. |
| Prewarm + window identity dual live identity | **PLAUSIBLE** | Both halves present: prewarm reuses persisted ids; `RemoveClientArgs` with live ctx calls `api.RemoveNetworkClient` (`ip_remote_multi_client_api.go` ~300–323). Whether eviction fires inside the 2m grace is unproven — needs overlap integration test. |
| SDK JS stale typings | **CONFIRMED** | Not re-diffed line-by-line this pass; API rename vs `types.ts` was already confirmed in REVIEW2/3 and is consistent with the local tree layout. |
| SDK gomobile/cgo removals | **CONFIRMED** | Coordination requirement; apps updated lockstep locally. |
| Monitor `network_client_connection` time-range scan | **CONFIRMED** | `probe_pg_tier1.go` count by `connect_time` only; no leading `connect_time` index. |
| Stream Eval TTL nanoseconds | **CONFIRMED** | go-redis v9.21.0 `internal/proto/writer.go`: `case time.Duration: return w.int(v.Nanoseconds())`. `AddToStream` + new `joinStream` pass `ttl time.Duration` into Lua `EXPIRE`. Typed `Set`/`Expire` elsewhere convert correctly. `redis.go` lock helper correctly uses `(ttl+…)/time.Second`. |
| DNS local fallback / privacy | **CONFIRMED (product)** | Documented; still a privacy/split-DNS product decision. |
| DNS flight: fallback wins, tunnel worker not cancelled | **CONFIRMED** | `startDnsPipeline`: independent `queryContext()`s; `reply` marks done but does not cancel the loser. `tunnelOk` only skips *starting* local work after tunnel success — not the reverse. |
| Reliability old-reader / new-writer | **CONFIRMED** | New writers only shard keys; old rollup reads legacy only then covers/SRems pending block. Rollback hazard. |

### P2 (selected, re-verified)

| Finding | Status | Notes |
|---------|--------|-------|
| API SIGTERM during startup unlatches ready | **CONFIRMED** | Signal goroutine sets draining (`cli/api/main.go` ~73–83); later success path still `SetWarpStatusReady()` (~125–127). Last writer wins. |
| Taskworker give-up cancels finalize ctx | **CONFIRMED** | Finalize uses `self.ctx` (~1526); CLI `defer cancel()` on drain goroutine. Post path already uses `WithoutCancel` for posts — finalize does not. |
| Connect drain split-state no excuse | **CONFIRMED** | Straggler loop: `resident := self.residents[clientId]`; `markDrained` only if `resident != nil`; always cancels handles. |
| Migrate broadcast serial, no mid-loop deadline | **CONFIRMED** | `migrateResidents` loop with `SendWithTimeout` 1s; deadline only on subsequent wait. Wait uses **all** `len(self.connections)`, including split-state never migrated. |
| go-redis PubSub silent reconnect | **CONFIRMED (nuance)** | Done channel fires on ctx cancel / topology watcher / explicit Close — not on every internal reconnect. Resync metric under-counts gap class. Corrective poll is the real bound — but that poll itself is defective for pure-expiry (below). |
| Stream hop expiry → no delivery | **CONFIRMED** | Hop set TTL 8h; eid TTL 24h; `Kick` only; `reset()` delivers only if eid differs. Expiry does not bump eid. |
| Missed peer expiry not healed by version poll | **CONFIRMED** | Member expiry without delivered event and without version bump → corrective `reset()` sees same eid → no callback. |
| Peer deltas leave local eventId stale | **CONFIRMED** | By design comment; every post-churn corrective poll full-resets → O(N) readers × full set after any mutation. |
| Peer failed-delta `forceResync` without wake | **CONFIRMED** | `forceResync.Store(true)` then `continue` without `Resync()`/`resync` send (~1197–1206). Repair waits up to corrective poll. |
| `UpdateNetworkPeerProvideModes` KEEPTTL resurrection | **CONFIRMED** | Meta hash can outlive member (24h safety vs registration ttl). `SET … KEEPTTL` on missing key creates **no TTL**. Refresh path already has restore-with-TTL (~572–580) — update path lacks it. |
| Window identity store out-of-order snapshot | **CONFIRMED** | Snapshot under mutex; `StoreWindowClientIdentities` after unlock. Concurrent Record/Remove can persist stale full snapshot. |
| Proxy drain enter race | **CONFIRMED** | HTTP: check `Draining` then separate `enter()`. SOCKS: Accept → goroutine → `enter()` later. `enter` never refuses. Window is scheduling-small but real. |
| FindProviders2 100% sample + full-pool sort | **CONFIRMED** | Defaults fraction 1.0, max 2000; sort before cap; HMAC in hot path; inert until salt/site enable. |
| Blob missing minio → local store | **CONFIRMED** | `LoadBlobStore` always returns local when vault resource absent. |
| CreateContractRetryInterval 5s→1s | **CONFIRMED** | ~5× control load for stuck destinations. |
| NAT GlobalLimit on unbudgeted providers | **CONFIRMED** mechanics | LRU collect+sort under buffer mutex at cap. |
| SDK migrate clock skew unbounded wait | **CONFIRMED** | `time.Until(migrateTime)` no clamp; `migrating` CAS blocks further migrates. |
| SDK migrate auth snapshot race | **CONFIRMED** race shape | Snapshot auth, connect next, swap; `SetByJwt` can update old transport only mid-flight. |
| HTTPS fanout case / attach race | **CONFIRMED** | See §3 adjacent. |
| Contract-stats emit reorder | **PLAUSIBLE** | Callbacks outside lock; CloseAll vs epoch interleave. |
| Prewarm dual-identity | **PLAUSIBLE** | See above. |

---

## 3. Adjacent / similar issues (new or strengthened)

These are issues that sit next to REVIEW2/REVIEW3 findings but were not fully spelled out there, or that REVIEW1 under-specified.

### A1. Orphan-RST path is the only TCP init path that *doesn't* copy IPs — CONFIRMED (adjacent to P1 UAF)

**Where:** `connect/ip.go`  
**Evidence:** New sequence construction copies `tcp.sourceIp` / `destinationIp` into fresh `net.IP` buffers before storing them on the sequence. The orphan-RST branch builds `IpPath` from the aliased slices *after* `MessagePoolReturn`.  
**Why it matters:** Not a second bug class — it shows the correct pattern already exists two dozen lines away and was not applied to the new feature. Any future “fast path that returns the packet early then still needs path metadata” will reintroduce the same race unless pool ownership is a type-level rule.  
**Also check:** UDP paths that call `receiveCallback` after return (spot-check: normal UDP sequence keeps packet until processed; no second orphan-UDP feature yet). Enabling a future “orphan ICMP/UDP” helper must copy first.

### A2. HTTPS/SVCB flight attach-after-reply loses responders — CONFIRMED (adjacent to REVIEW3 §15)

**Where:** `connect/ip_mux_upgrade.go` `attachDnsResponder`, `fanOutHttpsForward`, `startHttpsForwardPipeline`  
**Evidence:**

- A/AAAA `reply()` sets `replied` **and** `delete(self.inflight, key)` under one lock.  
- HTTPS `fanOutHttpsForward` sets `replied` and snapshots responders **without** deleting the flight. Deletion is deferred in the pipeline goroutine after Forward+fanout.  
- `attachDnsResponder` does **not** check `fl.replied`; if the flight is still in the map, it appends responders.

**Impact:** A retransmit/attach during the narrow post-reply, pre-delete window is appended and never answered (client timeout). Worse under slow `deliverDownstream` fanout.  
**Related:** Raw HTTPS forward only rewrites the 2-byte ID; does not restore per-responder 0x20 question case (REVIEW3) — stubs that validate case can reject.

### A3. Local-fallback win pollutes reverse index with off-tunnel answers — CONFIRMED (adjacent to DNS privacy)

**Where:** `startDnsPipeline` `reply()` always `self.reverse.record(addrs, domain)` on success.  
**Impact:** If local fallback wins, reverse index / SNI affinity / block-action naming can bind IPs from the **local** resolver view for the rest of ReverseTtl (now 1h). That couples the privacy leak to long-lived control-plane state, not just one DNS answer.

### A4. Loser tunnel workers are not bounded by MaxInflightQueries — CONFIRMED (adjacent to REVIEW3 §10)

**Where:** `startDnsPipeline`  
**Evidence:** First success deletes the flight from the 96-cap map, freeing the slot for new questions, while the losing worker can still block up to `ResolveTimeout` (60s) in DoH/single-flight/sem.  
**Impact:** During tunnel brownout, goroutines and waiters scale with arrival × remaining timeout, not the inflight map cap. Semaphores bound *active* HTTP, not waiters queued behind them.

### A5. Peer member-key lifecycle asymmetry (Refresh restores; ProvideModes KEEPTTL can immortalize) — CONFIRMED

**Where:** `RefreshNetworkPeer` vs `UpdateNetworkPeerProvideModes`  
**Evidence:** Refresh: `Expire`; if false, `SET` with full registration `ttl`. Update: `SET … KEEPTTL` only — no “missing key” branch, no residentId check on the write beyond the initial HGET race window.  
**Adjacent failure modes:**

1. Member expired, meta still present → KEEPTTL creates **persistent** event key (no expiry notification ever).  
2. Concurrent `RemoveNetworkPeer` between HGET and pipeline → can resurrect a removed peer’s member key.  
3. Persistent member key + periodic provide updates keep emitting `set` while zset membership may disagree → listeners can re-add a peer that prune thought was gone until next full reset (which may not come — see missed-expiry hole).

### A6. Corrective poll is not a true full-state reconcile — CONFIRMED (strengthens REVIEW3 §6)

Both peer and hop listeners gate delivery on **version inequality**, not on “read state ≠ last delivered state.”

| Event class | Version bumps? | Delivered on pure expiry? |
|-------------|----------------|---------------------------|
| Peer add/remove/provide (happy path) | Yes | Yes (delta or reset) |
| Peer member TTL expiry (event delivered) | No (delta path) | Yes via delta `expired` |
| Peer member TTL expiry (event lost) | No | **No** until eid TTL (24h) or later mutation |
| Hop set TTL expiry | No | **No** (even if `expired` event arrives — Kick sees same eid) |
| Redis maxmemory eviction (`e` not in `Kg$sx`) | No | **No** |

So PEERSSTREAMS2’s “corrective poll bounds staleness to 5 minutes” is **false for pure expiry / silent key loss**. The 5-minute poll only catches version-moving gaps.

### A7. Failed peer delta does not call `Resync()` — CONFIRMED (adjacent to forceResync bugs)

**Where:** `peer_model.go` delta branch stores `forceResync` on panic but does not send on `resync` / call `Resync()`.  
**Contrast:** Buffer-full `Delta` correctly calls `Resync()` which sets the flag **and** wakes.  
**Impact:** A transient Redis error on a single delta read waits the full corrective interval (5m in key-event mode) before repair attempt — and that repair may still no-op if the underlying change was expiry-only (A6).

### A8. Stream memory goal is undermined by Lua TTL bug on *both* add and join — CONFIRMED

**Where:** `AddToStream` (pre-existing) + `joinStream` (new companion path copies the bug).  
**Adjacent:** `RemoveFromStream` correctly DELs when last contract leaves, so *clean* close still frees keys. The leak is specifically **crash / partial failure / orphan contracts** that never hit remove — exactly the case TTL was meant to heal. Permanent stream_id + contracts sets also mean a later stream with the same deterministic key can reattach to a zombie contract set (REVIEW3).

### A9. Drain excuse / migrate coverage gaps form a set — CONFIRMED

REVIEW1/2/3 each name pieces; together:

| Client situation on draining node | Excuse? | Migrate frame? |
|-----------------------------------|---------|----------------|
| Local resident, drain start | Yes (bulk mark) | Yes (if coordination on) |
| Local resident, straggler | Yes | Already attempted |
| Connection only (resident elsewhere) | **No** | **No** |
| Nominate/replace teardown (non-drain) | **No** | N/A |
| Second bounce within reliability block | **No** (GETDEL one-shot) | N/A |

CONNECTDRAIN2’s motivating double-bounce class is only partially addressed.

### A10. API ready-vs-draining race has siblings — CONFIRMED (adjacent to REVIEW2 API finding)

| Service | Startup latch | SIGTERM | Race? |
|---------|---------------|---------|-------|
| api | `SetWarpStatusReady` after check | sets draining then serveCancel | **Yes** — ready can overwrite draining if SIGTERM during check/warmup |
| taskworker | readyGauge only (no SetWarpStatusReady in snippet) | SetDraining + Drain | Different shape; not-ready overwritten by draining is intentional |
| connect | `StartupReadiness` sets status inside router helper | Drain after ready | Weaker race: readiness completes before signal handler usually; still no “if already draining, skip ready” guard if startup were slow |

**Fix shape (shared):** status transitions should be monotonic: `not_ready → ready → draining` with draining sticky.

### A11. Task finalize vs post: inconsistent cancel isolation — CONFIRMED

**Where:** `task/task.go`  
**Evidence:** Post already uses `context.WithoutCancel(ctx)` with an explicit comment about drain/max-time. Finalize still uses `self.ctx`. Give-up therefore destroys earned handbacks (REVIEW2) *despite* the post path having already learned this lesson.  
**Adjacent:** `"Task not run."` path for incomplete batch recording still advances error accounting (REVIEW1) — another drain-adjacent honesty hole.

### A12. Stats enable path couples three sharp edges — CONFIRMED (adjacent composition)

When ops enables FindProviders2 stats:

1. Default **100%** sample + full-pool sort/HMAC on the hottest route.  
2. Missing `minio.yml` → **local blob tree on the API host** (no byte cap; TTL reaper only).  
3. Upload head-of-line blocking (REVIEW2) can delay segment ship while local disk fills.

Individually each is P2/conditional; **composed** they are an easy self-DoS of the API fleet the first day stats is turned on in a non-local env without MinIO.

### A13. Identity persistence write ordering vs Redis GETDEL-style handoff — adjacent pattern

Window identity: last writer wins with full snapshot, no generation.  
WG handoff: one-shot GETDEL, no poll.  
Both are “single Redis key coordination across two containers” without a generation or wait loop. Same design family as the WG ordering bug — any new handoff (activity sets, prewarm keys) should default to **poll with budget + generation**, not one-shot at the wrong lifecycle phase.

### A14. `joinStream` / pair marker CROSSSLOT is fine; Eval TTL is not the only join hazard — nuance

Pair marker is correctly on its own pipeline (different slot). Readers re-check liveness.  
**Adjacent:** pair marker TTL is set via typed `Expire` (correct seconds) while the stream membership keys use Lua nanoseconds — so the marker can expire long before the immortal stream keys, or vice versa depending on clock… actually marker gets real 8h, stream keys get ~forever. Markers prune as stale; stream keys linger. Asymmetric TTLs make debugging “why is this stream still joinable?” harder.

### A15. ProvideMode mask fix is a real security win — CONFIRMED (REVIEW2 clean check)

Re-verified at the level of REVIEW2’s claim: ordinal max/min promotion of Network relationships is removed in the delta. Worth preserving in release notes: **not only a perf change; closes LAN-reach promotion holes.**

---

## 4. Corrections to REVIEW1 ranking / content

| REVIEW1 item | Update after R2/R3 + re-check |
|--------------|-------------------------------|
| Key events ON without notify config | Still Critical for freshness; **plus** even with notify ON, pure-expiry convergence is wrong (A6). |
| DNS local fallback privacy | Still Critical product gate; **add** loser-worker leak (A4) and reverse-index pollution (A3). |
| SubscribeKeyEvents blocking buffer | Still High; **add** go-redis silent reconnect means resync path rarely runs on blips. |
| Excuse one-shot / multi-bounce | Still High; **add** split-state and non-drain replace (A9). |
| Missed entirely | sim-latency P0, IsProFresh-in-tx, orphan RST UAF, WG handoff order, Lua TTL ns, KEEPTTL resurrection, API ready race, HTTPS attach race. |

---

## 5. Items still open / not fully re-proven this pass

- Exact production frequency of prewarm dual-identity eviction during 2m grace (mechanism yes, timing no).  
- Contract-stats Open-after-Close reordering under real scheduler pressure.  
- Full JS `types.ts` vs wasm export matrix (accepted from R2/R3).  
- Grafana redis `noDataState: OK` (ops/warp tree; not re-opened here).  
- Whether any other Eval sites pass `time.Duration` (stream_model is the only production hit found; verify_model uses seconds).

---

## 6. Suggested fix priority (merged)

### Must-fix or hard-gate before broad ship

1. **sim-latency:** panic unless resolved env is exactly `local` (same as tests).  
2. **`IsProFresh`:** hoist before every JWT-minting transaction (AuthNetworkClient both branches first; then AuthCodeLogin / UpgradeGuest / DeviceConfirmAdopt).  
3. **Orphan RST:** copy IPs (or defer pool return) + race test with pooled buffers.  
4. **WG handoff:** poll/wait for generation-tagged export after redirect, or export before drain end; integration test with warp order.  
5. **Stream Lua TTL:** pass `int64(ttl/time.Second)` in `AddToStream` and `joinStream`; assert TTL in tests.  
6. **Reliability:** enforce taskworker≥writers generation; forbid old rollup rollback while shards write.  

### Should-fix with peers/key-events enablement

7. Expiry → authoritative deliver (hops force empty reset; peers force remove or state-diff reconcile).  
8. Advance/ack version after successful delta apply (per network process).  
9. Non-blocking key-event merge + single-flight resync; detect PubSub reconnect epochs.  
10. `UpdateNetworkPeerProvideModes`: Lua/atomic update; never KEEPTTL-create; mirror Refresh restore.  
11. Failed delta path: call `Resync()` not only `forceResync.Store`.  
12. Drain: excuse connection-only clients; clamp migrate send loop; wait only migrated set.  

### Client / proxy / stats

13. Cancel losing DNS workers; don’t reverse.record local-fallback answers into long-lived affinity (or tag source).  
14. HTTPS fanout: delete flight atomically with replied; restore question case.  
15. Window identity: generation + ordered store.  
16. Drain `tryEnter` under lock.  
17. Stats: default fraction ≪1, truncate-before-sort, non-local requires MinIO or disable upload.  
18. SDK: clamp migrate wait; reapply auth on swap; fix JS Request method + types.ts.  

### Product / release notes

19. DNS local fallback (privacy).  
20. Provide auto → Network while idle.  
21. Peers re-enabled + valve semantics.  
22. ProvideMode mask narrowing (security-positive).  
23. 90d→30d top-level idle expiration wave.

---

## 7. Test gaps worth adding (from verification pain)

| Gap | Why |
|-----|-----|
| sim-latency refuses `WARP_ENV=main` | P0 guard |
| AuthNetworkClient holds one pg conn; IsProFresh must not `Db()`/`Redis()` inside Tx | regression for idle-in-tx |
| Orphan RST with `MessagePoolGet` recycling during callback | UAF |
| WG handoff with apply-at-T0, export-at-T+grace | deploy order |
| Stream key TTL after Add/Join is ~8h not years | Lua seconds |
| Hop key `expired` event → empty hop list delivered | A6 |
| Lost peer `expired` + 5m poll → remove delivered | A6 |
| ProvideModes update after member EXPIRE → key has TTL | KEEPTTL |
| HTTPS attach during fanOut → still answered or rejected cleanly | A2 |
| DNS fallback win cancels tunnel worker | A4 |
| Drain connection-only client writes excuse | A9 |
| Window identity concurrent Record/Remove last-store wins with generation | ordering |

---

## 8. Bottom line

REVIEW2 and REVIEW3’s **P0/P1 list should be treated as real**. Re-verification found:

- **No P0 false positives.**  
- **One partial over-claim:** not every listed `IsProFresh` site is inside a transaction (AuthVerify is outside); the **AuthNetworkClient** instances remain outage-class.  
- **Several adjacent defects** (A1–A15) that share root causes: pool/alias lifetime, Redis TTL encoding, version-gated reconcile that cannot see pure expiry, one-shot cross-container handoffs, and non-monotonic status/auth snapshot races.

REVIEW1 remains useful for the drain/key-event/perf narrative; this update file is the **defect ledger** that should gate the coordinated release.

*No code was modified for this update.*
