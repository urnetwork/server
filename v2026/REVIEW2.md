# REVIEW2 — delta review vs origin/main: server, connect, sdk, proxy, warp (2026-07-19)

Read-only review of everything not on origin/main across the five repos — local commits plus the
uncommitted/untracked working tree (~25k changed lines implementing CONNECTDRAIN2, PEERSSTREAMS2,
RELIABILITY2, APIDRAIN1, PROXYDRAIN1, TASKDRAIN1, the connect/sdk perf commits, the stats/monitor/
sim-latency tooling, and the warpctl config refactor). No code was modified.

Method: 11 parallel area reviews covering the full delta, each headline finding independently
re-verified against the code before inclusion (every CONFIRMED item was traced to the quoted
lines; PLAUSIBLE items state what remains unproven). `go vet ./...` is clean on all five repos.

Severity: P0 = can destroy prod data / outage-class. P1 = real defect on a realistic path, or a
shipped feature that does not work. P2 = edge-case defect, deploy-window hazard, or contract
divergence. P3 = hardening, doc drift, cosmetics.

A note on deploy state: evidence in the tree says parts of this delta have already run in prod —
`redis.go` documents a 2026-07-18 connect-fleet crash-loop fixed by the current `stateLock`,
RELIABILITY2 §4 records the resharding rollout as completed 2026-07-18, and warpctl's drain flock
cites the 2026-07-18 g1+g4 incident. Deploy-order cautions below therefore apply to *re-rolls and
rollbacks* as much as first deploys.

---

## 1. Top findings, ranked

| # | Sev | Where | One-liner |
|---|-----|-------|-----------|
| 1 | P0 | connect/sim-latency | No local-env guard; with `WARP_ENV=main` in the shell it migrates/provisions/prewarm-overwrites **prod** pg+redis |
| 2 | P1 | model/network_client_model.go | `IsProFresh` (pg on a 2nd conn + redis SET with 60s retry) runs **inside** the `AuthNetworkClient` txs — recreates the idle-in-tx/redis-coupling incident pattern on the client-auth path |
| 3 | P1 | connect ip.go (orphan RST) | Use-after-free: pooled packet returned, then the `IpPath` handed to the receive callback still aliases it — data race + garbage IPs into egress policy on providers |
| 4 | P1 | proxy drain (server) | wg endpoint handoff is consumed (once, at new-container boot) before it is produced (old-container drain end) — P3a never engages in a real deploy |
| 5 | P1 | proxy prewarm + window identity | Same reused window clientId active in both containers during deploy overlap; old side's eviction deletes the platform client the new side is using (mechanism confirmed on both halves; in-grace timing unproven) |
| 6 | P1 | sdk js | `js/src/types.ts` still declares the old wasm API — consumers compile clean and throw at runtime |
| 7 | P1 | sdk gomobile/cgo | Removed/renamed exported surface (contract-details VC split, `ContractClientRow` gone, 4 C exports deleted) — apple/android/windows need lockstep updates at next rebuild |
| 8 | P1 | monitor | `pgConnectRateProbe` full-scans `network_client_connection` (no `connect_time`-leading index) every 60s on the prod primary — fix before running monitor against prod |
| 9 | P2 | model/stream_model.go | `joinStream`/`AddToStream` Eval pass `time.Duration` → go-redis marshals **nanoseconds** → `EXPIRE` ≈ 913,000-year TTL; stream keys never expire (pre-existing, re-entrenched) |
| 10 | P2 | redis key events | go-redis PubSub silently auto-reconnects, so the spec'd "conn killed → resync" never fires; blip freshness degrades to the 5-min poll and the resubscribe metric is blind |
| 11 | P2 | connect drain | Split-state clients (transport here, resident elsewhere) get neither excuse nor migrate — the motivating double-bounce still invalidates their reliability blocks |
| 12 | P2 | reliability resharding | Old-taskworker rollup + new-connect writers loses whole blocks; ordering documented but unenforced — relevant to rollback |
| 13 | P2 | connect ip defaults | NAT `GlobalLimit` caps + 60s UDP idle now apply to unbudgeted egress providers: at cap, every new flow LRU-cancels an established one and pays an O(n log n) scan under the buffer mutex |
| 14 | P2 | FindProviders2 stats | Sample construction (full-pool sort before the 2000 cap + up to ~4k HMACs/call at fraction 1.0) is inline in the hottest api route — inert until the stats salt is provisioned, expensive the day it is |

Everything else is P2/P3 detail below.

---

## 2. Cross-cutting verifications that came back clean

- **Wire protocol**: regenerated `*.pb.go` changes exactly one tag — the new `migrate_time` field
  (header churn is protoc 7.35.0→7.35.1; protoc-gen-go unchanged). No existing field
  numbers/types touched. `TransferResidentMigrate=28` + `ResidentMigrate` are purely additive.
- **Old clients vs the migrate frame** (the compat linchpin): traced end-to-end on
  `origin/main` of both client repos — migrate arrives as an inner Pack frame, is acked at the
  sequence layer, and every registered receive callback type-switches and silently drops unknown
  frame types (StreamManager, PeerManager, ContractManager, webrtc signal receiver,
  RemoteUserNat client/provider, multiClientChannel; the `panic(err)` sites in ip.go are inside
  other case arms). Proto3 open enums decode value 28 fine. **Old clients are safe; no version
  gate needed; no retransmit storm.**
- **ProvideMode mask rule**: the delta *removed* the last ordinal usages — `max(provideMode,
  self.provideMode)` at both egress sites and the numeric-min tracking in
  `recordSourceProvideMode` — and zero ordinal comparisons/min/max remain across connect + server
  + sdk. The replacement is a strict narrowing that **closes a security hole**: old code promoted
  `(Network, None)` / `(None, Network)` pairs to full same-network (LAN-reaching) egress; new
  code requires both sides Network (`TestEgressRelationship` pins the table). No legitimate
  relationship loses access (sdk always passes explicit modes).
- **CROSSSLOT audit**: every new/modified redis pipeline batches a single key or keys under one
  hash tag (`{np_<networkId>}` peers incl. member keys, `{contractId}`, stream-key tags,
  reliability single-shard writes; blocks-set SAdd and drain DELs deliberately separate). No
  violations in the entire delta. New proxy models are single-key ops only, hash-tagged anyway.
- **DB migrations**: two tail-appended, additive, no positional collisions:
  `transfer_contract.open SET STATISTICS 10000` (the durable planner-landmine fix — its own
  comment says apply manually on prod ahead of the next nightly ANALYZE) and
  `client_reliability.connection_excused_new_count DEFAULT 0` (metadata-only).
  `client_reliability_valid` SQL untouched — excused counts are non-invalidating by construction.
- **pgx fixed-array Scan gotcha**: no instances anywhere in the delta.
- **warpctl `/status` contract, both directions**: new statuses "ok" / "error not ready: …" /
  "draining"; old regex `^(?i)error\s` and new `^(?i)error(\s|:)` both match the not-ready form;
  "draining" deliberately matches neither (deploy poll must not treat operator drains as errors);
  old binaries' constant "ok" still passes new warpctl. Pinned by tests on both repos.
- **warpctl compat**: services.yml structs/tags verbatim-identical after the -777-line config.go
  refactor; `service run` flag-for-flag identical vs deployed xops systemd units; the removed
  commands (`watch`, `run-local`, `service drain`, `service up|down`) were already dead no-ops on
  origin/main with zero references in xops. SIGTERM/stop timeouts unchanged (KillTimeout 15s,
  DrainTimeout 60min, `docker stop -t 3600`) — the server-side bounded drains fit comfortably.
- **WgTun cross-repo rule**: interface unchanged; the new surface lives on WgProxy; the
  `recordingWgTun` mock needs no change and has none.
- **sdk RPC wire (net/rpc + gob)**: compatible both directions; dropped `ContractDetailsRpc`
  fields gob-skip/zero-fill gracefully; no method renamed.
- **JWT/auth**: no new/removed claims, no validation change → no mass-logout risk. Old long-lived
  tokens keep verifying.
- **Money accounting**: server settlement does not consume the reworked `ContractStatsEvent`
  (sdk-UI-only); and the `SendNoContract` mid-sequence drop **fixes a real pre-existing
  crash/over-credit bug** (old code retained the contract and either panicked "Bad accounting" on
  the eventual ack or silently over-credited). Correct in the new code — but untested (§9).
- `go vet ./...`: clean on server, connect, sdk, proxy, warp.

---

## 3. P0

### 3.1 sim-latency has no environment guard and writes with full authority — CONFIRMED
`connect/sim-latency/main.go:164-170` only does `setIfUnset("WARP_ENV", "local")`; nothing checks
the resolved env, unlike `test_util.go:192-195` which hard-panics outside `local`. With
`WARP_ENV=main` exported — exactly the documented monitor workflow (`WARP_ENV=main monitor`) —
`sim-latency run` against prod would: apply local-HEAD migrations (`run.go:53`), bulk-insert ~100k
fake networks/clients with `AddBasicTransferBalance(...1 TiB...)` (`provision.go:131-138`),
`--prewarm` upsert `reliability_weight = 1.0` for **every connected+valid provider in the fleet**
(`provision.go:230-257`), and overwrite the prod redis `{cs_}` selection cache via
`UpdateClientScores`. Fix shape (post-review): the same three-line env panic test_util uses, at
the top of `runRun`/`runInit`/`runFleet`.

---

## 4. P1

### 4.1 `IsProFresh` inside the `AuthNetworkClient` transactions — CONFIRMED
`model/network_client_model.go:413` and `:655` call `IsProFresh` inside `server.Tx` (txs opened at
:269/:546). `IsProFresh` → `loadProNetwork` acquires a **second** pool connection while the tx conn
is held, then `setProNetworkCached` → `server.Redis`, whose client Pings and retries up to
`endRetryTimeout = 60s` before panicking (`redis.go:317-334`). A redis stall pins every client
create/re-auth tx open for up to 60s → idle-in-tx pileup → pg pool exhaustion → api-wide outage;
this is the same coupling class as the known escrow-in-tx incident, newly introduced on the auth
path. It also makes client auth hard-fail on redis-down when pg alone could answer, and doubles
per-request conn demand under pool pressure. The same function's concurrent-client gate is
deliberately placed *before* the tx with a comment explaining why — the fix is to hoist `isPro`
the same way in both branches (nothing in either tx affects `transfer_balance`). Same pattern at
four low-QPS sites: `network_model.go:1249,1344` (UpgradeGuest), `auth_model.go:1626`
(AuthCodeLogin), `device_association_model.go:894` (DeviceConfirmAdopt) — same hoist.

### 4.2 Orphan-RST path reads pool-returned packet memory — CONFIRMED
`parseIpv4`/`parseIpv6` return IP slices that alias the pooled packet (stated in their own
comments, `connect/ip.go:762/780`), and `parseTcpPacket` stores them into
`tcp.sourceIp`/`tcp.destinationIp`. The new orphan-RST branch returns the packet to the pool
inside `initSequence` (`MessagePoolReturn(ipPacket)`, ip.go:1883) and *then* builds
`&IpPath{SourceIp: tcp.sourceIp, DestinationIp: tcp.destinationIp}` for the receive callback
(ip.go:1998-2005). Any concurrent `MessagePoolGet` can take and overwrite the buffer while the
callback (`RemoteUserNatProvider.Receive` → `InspectEgress`/`RefreshEgress`) reads the IPs: a Go
data race; under load the RST's egress-policy check runs on garbage (RST silently dropped, or
garbage flow entries in the dmca table). The RST bytes themselves are built before the return and
are safe; no cross-flow corruption — hence P1 not P0. The tests miss it because they construct
`parsedTcp` with independent IPs (`ip_orphan_rst_test.go:40-51`). Fix shape: copy the 4/16-byte
IPs when building the IpPath (as `ParseIpPathWithPayload` does, ip.go:3907-3914) or defer the pool
return until after the callback.

### 4.3 wg endpoint handoff: consumed before produced — CONFIRMED
Apply side: one shot, at new-container initial sync — `cli/proxy/main.go:171-181` →
`ApplyWgHandoff` → single `TakeProxyWgHandoff` (GETDEL); empty ⇒ "no handoff", return
(`proxy/wg_handoff.go:77-80`). Export side: `drainCoordinator.AddBeforeExit(wg.ExportWgHandoff)`
(`cli/proxy/main.go:123`) — i.e. on the OLD container at drain end, after SIGTERM + grace. warpctl
orders a deploy poll-ready → redirect → drain (warp `run.go:605-663`), so apply always runs
minutes before export: the GETDEL finds nothing, and the exported key sits until its 10-min TTL —
or is consumed by the *next* deploy's boot (stale endpoints; wasted initiations). The P3a
"~200ms re-establish" therefore never engages in production; wg clients keep the pre-P3a blip.
The tests pass because `wg_handoff_test.go:52-86` calls export manually before apply. Fix shape:
poll `TakeProxyWgHandoff` with a budget ≥ flip-delay + drain flock + grace (the existing 5-min
`WgHandoffInitiateTimeout` suggests that intent), or export at SIGTERM instead of drain end.

### 4.4 Prewarm + window-identity reuse: same identity live in two containers — PLAUSIBLE
Default-on `EnablePrewarm` + window identity persistence: the new container prewarms devices for
exactly the proxyIds active on the still-serving old container, and `WaitForReady` forces window
establishment with the *reused* persisted `{clientId, jwt, instanceId}`
(`connect/ip_remote_multi_client_api.go:274-289`) while the old container still egresses with the
same identity. connect's receive-sequence supersede (`transfer.go:3423-3446`) darkens the old
container's egress mid-grace; if the old side's window then evicts the starved client with a live
ctx, `RemoveClientArgs` calls `identityState.Remove` **plus `api.RemoveNetworkClient`**
(`ip_remote_multi_client_api.go:313-323`) — deleting the platform client the new container is
actively using → auth failure → evict/mint churn on both sides, per active client, per deploy.
Both halves of the mechanism are confirmed in code (the connect-side review verified eviction
"removes for real" while only ctx-done teardown preserves identities); what remains unproven is
whether eviction thresholds trip within the 2-min grace and how the resident treats duplicate
(clientId, instanceId) connections. The lazy (non-prewarm) path is safe. Worth an integration
test that models the real deploy overlap before trusting P2/P3b on a busy box.

### 4.5 sdk JS package ships stale typings for a renamed wasm API — CONFIRMED
`js/src/types.ts:157` still declares `openContractDetailsViewController()`, `:264-265` still
declare `getClientContractRows()/getProviderContractRows()` (and the old `ContractClientRow`
shape), but the wasm now exposes only `openClientContractDetailsViewController` /
`openProviderContractDetailsViewController` (`js/device_remote.go:223-228`) and `getContractRows`
/ `setAtTop` / `pendingCount` (`js/view_controllers.go:259-273`). `types.ts` compiles into
`dist/index.d.ts` (package.json `"types"`), so TS consumers pass type-check and throw
`TypeError` at runtime. Also missing (additive): `matchedIps`/`matchedHosts`, max-block-actions
accessors, peer connected count. Related P2: `js/package.json` version is still `0.0.1-beta.6` —
unchanged across a breaking API change.

### 4.6 sdk gomobile + cgo surface removals — CONFIRMED (coordination requirement)
Removed/renamed exports: `OpenContractDetailsViewController()` (split into Client/Provider
variants), `ContractDetailsViewController.GetClientContractRows/GetProviderContractRows` (→
`GetContractRows`/`SetAtTop`/`PendingCount`), types `ContractClientRow(+List)` (→
`ContractPeerRow`/`ContractEntry`(+Lists)), 6 `ContractDetails` Companion*/Replaces fields, and
4 C exports (`urnet_contract_details_view_controller_get_{client,provider}_contract_rows`,
`urnet_device_{local,remote}_open_contract_details_view_controller`). Compiled apps in the field
are unaffected until a framework rebuild; at that point apple/android source and the Windows
client (DLL import table) break. Requires coordinated app updates in the same release train.
`.h`/`.hpp`/`.def`/`exports_gen.go` are mutually consistent.

### 4.7 monitor connect-rate probe full-scans a churned multi-GB table every 60s — CONFIRMED
`monitor/probe_pg_tier1.go:76-80`: `SELECT count(*) FROM network_client_connection WHERE
connect_time >= … < …` every minute. No index leads on `connect_time` (only
`(client_id, connect_time)`, `connected`-leading, `disconnect_time`) → full scan of ~3M+
delete-churned rows on the primary, per minute, bounded only by the 30s statement_timeout. This is
the one monitor query violating MONITOR.md's own "handful of sub-second reads" budget. Needs a
`connect_time` (BRIN or btree) index or a cheaper proxy signal before the monitor runs as a
standing prod probe.

---

## 5. P2

### Server — drain work
- **api: SIGTERM during startup unlatches "draining"** — CONFIRMED. The signal goroutine flips
  draining + ready=0 once and exits; if SIGTERM lands during `ReadinessCheck` (≤15s) or `Warmup()`,
  main then runs `SetWarpStatusReady()`/`readyGauge.Set(1)` (`cli/api/main.go:112-127`;
  last-writer-wins at `warp_handlers.go:88-90`) and serves the whole drain reporting "ok". Worst
  case an operator `docker stop` during warpctl's 120s poll lets the poll read "ok" from a dying
  container and flip DNAT to it. Guard the ready latch on `quitEvent.IsSet()` (or make draining
  sticky).
- **taskworker: drain give-up destroys already-earned handbacks** — CONFIRMED. On give-up the
  drain goroutine's `defer cancel()` (`cli/taskworker/main.go:141`) kills the root ctx; each stuck
  batch's finalize tx (`task/task.go:1526`) — the only place `release_time = now` handbacks are
  written — then starts on a canceled ctx, panics `DbContextDoneError`, and is IsDoneError-
  suppressed (silent). Slow-unwind ctx-respecting tasks (>30s `DrainCancelTimeout`: DbMaintenance,
  UpdateClientScores) keep leases up to claim+MaxTime (24h/2h); *successfully finished* batch-mates
  also roll back and re-run. Not a regression vs the old SIGKILL, and the shipped mitigations
  (gave-up log → tailer class, `pg/task-lease-stranded` probe, `bringyourctl task release`) cover
  it — but it contradicts TASKDRAIN1 §2.1's "handed back immediately". Cheap fix: run the finalize
  on `context.WithoutCancel`, or delay the root cancel briefly after give-up.
- **connect: split-state clients get neither excuse nor migrate** — CONFIRMED
  (`connect/resident.go:1275-1300`: sweep looks up `self.residents[clientId]`, marks excuse only
  `if resident != nil`; migrate covers local residents only). Clients whose transport is on the
  draining box but whose resident lives elsewhere (common and long-lived: `ResidentTransport`
  prefers the existing remote resident) are cut with `ConnectionNewCount=1` — a rolling-deploy
  double-bounce still invalidates their blocks, which is the incident CONNECTDRAIN2 set out to fix.
  Write the excuse for connection-only clients too.
- **connect: migrate broadcast is serial with no aggregate deadline** — CONFIRMED
  (`connect/resident.go:1180-1186`): per-resident `SendWithTimeout(…, 1s)`; wedged clients block
  1s each; the deadline clamp lives only in the wait loop after the broadcast. Tens of thousands
  of residents × a few % wedged can eat the whole drain window; SIGKILL becomes the real bound.
  Add an elapsed check in the loop or a bounded parallel fan-out.
- **sdk: client migrate wait unclamped against clock skew** — CONFIRMED
  (`device_local_provider.go` handleControlFrames): `time.Until(migrateTime)` with no upper bound;
  a clock hours behind leaves `migrating=true` for that long, so the CAS drops every later drain's
  migrate frame (falls back to eviction each deploy) until the stale timer fires. Clamp the wait
  to ~2× the migrate window.

### Server — peers/streams/reliability
- **Key events: silent go-redis reconnect ≠ spec'd resync** — CONFIRMED. go-redis v9 PubSub
  auto-reconnects and resubscribes on network errors; `Channel()` only closes at `PubSub.Close`.
  So `SubscribeKeyEvents`'s done fires **only** on ctx cancel or the 60s master-set watcher —
  never on a killed subscription conn. Events lost in the reconnect gap are repaired only by the
  5-min corrective poll (which does work, via version counters), and
  `urnetwork_key_event_resubscribes_total` cannot count the gap class it exists for. PEERSSTREAMS2
  §7's 5-min bound holds; §8 row 2's seconds-scale claim does not. If ms-scale freshness after
  blips matters, detect reconnects (e.g. per-conn ping/epoch) and force resync.
- **Stream hops: key expiry is never delivered** — CONFIRMED. `{clientId}s2_c_hops` (TTL 8h)
  expiring does not bump the 24h eid key; `Kick()` → `GetStreamEventId == synced` → no reset — in
  both event and poll mode (the rewritten `reset()` dropped v2's unconditional insurance
  delivery). A client with zero contract turnover for 8h whose resident stays up keeps phantom
  "open" hops for up to ~16h (v2 healed in ~50s). Same hole for maxmemory eviction (class `e` not
  enabled). Fix shape: on `expired` for the hops key, force a delivery, or bump eid on expiry.
- **Streams: Eval TTL is nanoseconds** — CONFIRMED (`model/stream_model.go:427-443` new,
  `AddToStream` pre-existing): `ttl time.Duration` passed as ARGV; go-redis marshals
  `Nanoseconds()` (`internal/proto/writer.go:181-182`); Lua `EXPIRE key 2.88e13` ⇒ ~913,000-year
  TTL. `{…}s2_sk_sid`/`s2_sk_cs` effectively never expire; crashed/unclosed streams leak keys
  forever, and the deterministic stream key re-attaches later streams to stale contract sets —
  directly counter to the memory-throughput goal of commit 916fcb79. Fix `int(ttl/time.Second)` at
  both Eval sites (typed `Set`/`Expire` calls elsewhere are correct).
- **Reliability resharding deploy order (rollback-relevant)** — CONFIRMED. Old rollup reads only
  the legacy unsharded key, then covers the block and SRems it from the pending set — stranding
  all 32 shard keys to their 15-min TTL (whole blocks lost, not just the new counter). New-rollup +
  old-writer direction is handled and tested. RELIABILITY2/CONNECTDRAIN2 say "taskworker with or
  before connect" and the rollout appears already completed (2026-07-18) — the residual risk is a
  taskworker **rollback** while new connect writers run.
- **peer_model: failed delta read arms forceResync without waking the listener** — CONFIRMED
  (`peer_model.go:1197-1206`): stores the flag but sends no token, so the repair waits for the
  next wake (up to 5 min). One line: call `Resync()`.

### connect repo — perf commit and defaults
- **NAT flow caps now bind on unbudgeted egress providers** — PLAUSIBLE (policy), CONFIRMED
  (mechanics). `DefaultTcpBufferSettings.GlobalLimit = MemoryScaledCount(512, 64)`, UDP
  `2048`, UDP idle 300s→60s (`ip.go:126-137`); providers never set a memory budget, so busy
  egress hosts get hard caps that were previously unlimited: at cap, each new SYN LRU-cancels the
  idle-most *established* flow (RstAck to the client — long-polls/ssh reset), and plain-UDP
  protocols needing >60s NAT bindings break (QUIC survives). Compounding: the over-cap path runs
  `slices.Collect` over all sequences + per-sequence `LastActivityTime()` locks + sort **under the
  buffer-wide mutex** (`ip.go:1056-1074, 4125-4155`) on *every* flow creation while at cap (each
  DNS query is a new UDP flow) — a real dispatch stall on providers. Consider a provider-profile
  default (higher caps / budget-gated caps) and an O(1) eviction structure (heap/LRU list).
- **`CreateContractRetryInterval` 5s→1s multiplies platform CreateContract load ~5×** for stuck
  sequences (`transfer.go:127-136`; retry loop at :2533-2563): an offline/revoked destination now
  drives ~30 CreateContract control messages per 30s window per stalled sequence, each hitting
  the platform's contract-creation path. The same-network peer-connect latency win is real;
  consider fast-then-backoff.
- **Direct-to-peer traffic now rides the p2p stream conn exclusively** — CONFIRMED behavior
  (`transport_p2p.go:377-386`: `MatchesSend` also matches the bare peerId; gateway weight 0,
  pinned by test). A wedged-but-ICE-connected SCTP conn stalls that peer's traffic up to the 15s
  WriteTimeout before falling back (was: platform path always available); `Downgrade(source)`
  still matches only the streamId, so a downgrade derived from the direct path can't shed the p2p
  transport. Stall/latency cliff, not loss (sequences resend).
- **Contract-stats open/close reorder** — PLAUSIBLE (`transfer_contract_stats.go:143-183`):
  callbacks run outside `contractStatsLock`; `CloseAllContractStats` (teardown) can interleave
  with an epoch-tick pass so a stale `Open=true` is delivered *after* the final close — the exact
  UI linger this change was built to fix, now only in a goroutine-preemption-wide window.
  Serialize emit passes (emit mutex across callback invocation).
- **StreamReset reconcile: two client generations behave differently** — CONFIRMED, wire-legal
  both ways. New clients keep relisted streams (sequences + live p2p conns survive a resident
  migration); old clients cancel-all-then-reopen (fresh webrtc negotiation through the new
  resident). Server-side hop accounting and the migrate window must keep tolerating both.
- **SNI/ECH + type-65 behavior**: (a) ECH ClientHellos carry a cleartext *outer* server_name;
  `ip_sni.go` records it as the destination's name (contradicting its own header comment) —
  block-action reporting/affinity pollution as ECH deploys, no blocking bypass — PLAUSIBLE.
  (b) `handleDns` now claims SVCB/HTTPS (64/65) unconditionally, but with `EnableRemoteDoh=false`
  or a >1232B response the forward silently sends nothing (no SERVFAIL/TC) — type-65 queries that
  previously passed through to the upstream now time out; stubs racing A/AAAA survive, resolvers
  waiting on HTTPS RR see added latency — CONFIRMED (encoded in the updated blocker test).

### warp
- **All 11 redis alert rules have `noDataState: OK`** — CONFIRMED
  (`grafana/alerting/redis-cluster.yml`). Whole-host/exporter/fluent-bit death ⇒ series vanish ⇒
  NoData ⇒ OK ⇒ no page; `RedisNodeDown` can only fire for "exporter up, redis down". Add
  `noDataState: Alerting` (or an absent()/heartbeat companion rule). Metric names/labels
  themselves verified correct against the dashboards and exporter pipeline.
- **loki/mimir ring-port fields dropped from grafana.yml structs** — PLAUSIBLE (vault contents
  not verifiable from the repo): non-strict yaml means a vault file still setting
  `gossip_port`/`grpc_port` is silently ignored; ports are now constants 6490-6493 requiring
  services.yml declarations (missing ⇒ clean panic, fails safe).
- **Ring port renumber partitions the loki/mimir gossip ring during rollout** — CONFIRMED,
  transient by design (old 2394x/2309x vs new 649x can't gossip). WAL-flush/RF3/autoforget
  mitigate; roll all grafana hosts promptly rather than leaving it half-deployed.
- Drain gauges push on the same {env,service,block,host} series the replacement container
  overwrites within 15s — alert on `max_over_time`/`increase`, not last-value (SIGNALS.md §13.1
  currently says "nonzero"). Applies to `drain_cut_connections`, `draining`, `ready`.

### sdk (mixed-version rpc)
- **`ProvideControlModeNetwork` sent to an old DeviceLocal host maps to "no providing"** —
  CONFIRMED (old switch default → `ProvideModeNone`) while the remote's offline-derived getters
  report Network/enabled — UI and reality disagree; `ConnectLocation.NetworkPeer` is dropped by
  old gob ⇒ peer connect degrades to Public egress. No crash either way; iOS app+extension ship
  together so skew is rare — relevant if rpc ever spans separately-updated processes.
- **fetch_retry latent guard bypass** — CONFIRMED: `Request`-object input bypasses the GET-only
  check (only `init?.method` is read); abort during the backoff sleep isn't observed (≤1s extra);
  discarded 502/503 response bodies never cancelled. Current two call sites are true GETs.

### Tooling (monitor/stats), pre-deploy quality
- Escalation batteries run on **every** failing tick, not once per trip (`probe_pg.go:51-63`,
  `probe_redis_tier1.go:119,158`) — extra psql/CLIENT LIST load lands each 60s exactly when the
  target is sickest.
- Log tailer: leaked pipe read-fd per restart (~1/s if warpctl exits immediately) and a >1MB line
  wedges the tailer forever (`ErrTooLong` → child blocks on full pipe → `cmd.Wait()` never
  returns); `lastLineTime`/`restartCount` are written but never read, so a dead tailer is
  undetectable (`tailer.go:159-169`, `conn.go:297-314`).
- logs/novel tickets can never open when the per-minute top-shape varies: frame is part of ticket
  identity and `sustain: 2` never accumulates (`tailer.go:263-271`, `ticket.go:109-137`).
- `stats/export.go:54-58` swallows gzip/tar `Close` errors — a truncated competition tarball
  reports success (flat.go does this right).
- Uploader sweep head-of-line: first failing segment blocks all later segments every 30s until
  the 8GiB cap eventually drops it (`stats/upload.go:115-159`) — PLAUSIBLE.
- sim-latency crawl leaks one goroutine per timed-out crawl (workers exit without draining
  `jobs`; `pending.Wait()` never returns — `client.go:264-317`); measurement-window noise only.

### Server — stats/FindProviders2 (inert until enabled)
- **Sample construction inline in the hot path** — CONFIRMED (cost), contingent (impact). At
  default fraction 1.0: full-pool `SortFunc` *before* the 2000-candidate truncation with two map
  lookups per comparison, then up to ~2000 `Candidate` allocs + ~4000 HMAC-SHA256 per
  FindProviders2 call (`model/find_providers_stats.go:57-155`); only the disk write is behind the
  drop-gate. Armed the day ops provisions the stats salt (`stats.Enable` is unconditional in api
  main). Recommend before enabling: default fraction ≪1 in settings, truncate-before-sort
  (partial selection), hoist weights out of the comparator, and byte-bound (not count-bound) the
  4096-sample queue (~1GB worst case today).

### Product-behavior notes (deliberate, release-note them)
- 90d → 30d top-level client idle expiration (`network_client_model.go:1857`): one-time ~6.4M-row
  deactivation wave at deploy and a hard-delete wave 30 days later; the reap's redis cleanup is 2
  sequential RTTs per id (`:2104-2109`) — watch the wave, consider chunking/pipelining if it lands
  in one invocation. Users idle 30-90 days now must re-login (previously silently resumed).
- sdk: control mode `auto` while idle now keeps a live provider (`ProvideModeNetwork`) for peer
  discoverability — idle devices hold provider NAT state and can carry same-network peer traffic.
- connect: `ip_assoc` clustering much stricter (`PacketAssociationTime` 5s→1s,
  `MinMeanAssociation` 0.5→0.9); reverse-index TTL 5min→1h with carry-across-rebuilds;
  `BlockAction.Ips/Hosts` are now **disjoint** from the new `MatchedIps/MatchedHosts` — consumers
  that want totals must union (previously Ips/Hosts were the full sets).

---

## 6. P3 (terse)

**Server drain**: panic exit path skips final stats flush (api main.go:164-174); readiness latch
is one-shot — restart-in-place with a boot-time dep blip serves unwarmed + "error not ready"
forever (deliberate; consider bounded background re-check); `api/readiness.go` ≡
`router.startupReadinessCheck` byte-identical twins (+ stale comment in startup_readiness_test);
taskworker main discards the `StartStatsPusher` flush func so `drain_seconds`/`drain_canceled`
~never export (api main does it right — one line); errors racing phase-2 cancel are mislabeled
`Drained` (flat retry, self-correcting); `TaskWorker.Close()` cancels drainCtx (test-only);
`ReleaseTask` has no claim identity — an alive claimant re-steals within ~10s (documented operator
footgun; a worker-id column would help); SIGTERM on a not-ready taskworker overwrites "error not
ready" with "draining"; TASKDRAIN1 §7.4 mischaracterizes the pre-change post-sever behavior
(doc-only).

**Proxy drain**: before-exit hooks not panic-isolated (a redis error in export aborts the drained
flag → exit 1 vs 0); `WaitForReady` returns true on budget-expiry/dead-device so the prewarm gauge
overcounts; `HttpDrainCutError` from the /status server panics main after a successful drain
(cosmetic exit code); `ApplyWgHandoff` pending-set never empties for unseeded peers (pins the
5-min loop, full-device IpcGet per 5s — unreachable today given §4.3); microsecond accept-vs-enter
TOCTOU in socks/http (equals today's behavior for the raced conn); drain.go's wg comment says
deadline-bounded but the grace is socks/http-idle-bounded — a wg-only block exits seconds after
SIGTERM (defensible, but contradicts PROXYDRAIN1 §3.2/§8).

**Connect drain**: late-jittered clients get ~zero establish margin before the sweep (excused
anyway — blip not score); `rand.Int63n` panics if `DrainMigrateWindow <= 0` (config-error only);
excuse consume ignores the stored residentId (doc §3.1 claims scoping; any reconnect within the
5-min TTL is excused — writer is server-only so not abusable); provide-flap detection blind
~7min/drained client (per §6.2 decision, fleet-wide per deploy); **SIGNALS.md:963 + CONNECTDRAIN2
§3.5 name the gauge `urnetwork_drain_residents_remaining` but it registers as
`urnetwork_connect_drain_residents_remaining`** — dashboards/alerts built from the doc match
nothing; sdk replacement transport can keep a pre-refresh jwt ≤60s if `SetByJwt` races a
migration (negligible).

**Peers/keyevents**: kill-switch mode is not "pure v2" (insurance force-delivery removed in both
modes — deliberate, tests updated; spec overpromises); `UpdateNetworkPeerProvideModes` with
`KeepTTL` on a missing member key mints a permanent key (narrow trigger, memory-only); mixed-fleet
deploy window: peers of old-build residents get no events (poll-bounded ~5min, transient);
topology watcher treats transient `ForEachMaster` errors as topology change (full
resubscribe+resync storm, self-limiting) and `ReloadState` is lazy (~2×60s new-master detection);
subscriber `remove()` closures not idempotent (latent); member keys store a full gob payload
nothing reads (~2× per-peer memory — a 1-byte sentinel suffices).

**connect repo**: StreamReset open-loop aborts on the first malformed StreamOpen, stranding later
listed streams (pre-existing shape, needs a server-side bug to trigger); loopback-only: receive
supersede closes both send+receive stats entries for one contract id; p2p receive deadline-continue
never checks ctx (teardown chain via `pc.Close()` is prompt — hygiene); `CloseAllContractStats`
runs listener callbacks inline on the teardown goroutine (current sdk listeners non-blocking);
windowIdentityState snapshots can persist out of order under concurrent mutations (self-healing:
one wasted auth attempt after restart); DoH-forwarded type-65 replies keep the lowercased question
(dns-0x20 stubs reject — fails safe to A/AAAA); fixed-ClientId specs bypass FindProviders
validation (deliberate; bounded ping-timeout churn per expand round); stale `provide_mode <`
idiom in a `subscription_model.go:1447` comment (comment-only).

**Auth/models**: rebuilt client tokens reset `CreateTime`, breaking the root-lineage convention if
create-time expiry ever returns (matches pre-existing RefreshToken behavior); stale "90 days"
comment at the reap task; `stats.Append` takes a process-global mutex per sample.

**Monitor/stats/blob**: ticket auto-resolve is tick-based (daily probes resolve in ~5 days);
selection-freshness `LIKE '%…%'` probe degrades to a full walk exactly during the outage it
detects (bounded by 24h retention + statement_timeout); MONITOR.md promises controls that don't
exist (per-host semaphore; redis.yml creds; §5 lists unimplemented probes); `blob.go` keys ending
`.partial` invisible to local List; **minio.yml absent on a prod api host silently falls back to
the local blob root (unbounded growth outside the 8GiB stats cap)** — log/refuse in non-local
envs; ctl export paths use `context.Background()` with no timeout; crashed `.partial` segments
(~64MB/stream) never cleaned; monitor has **zero tests**.

**warp**: `resolveVaultPath` falls back to vault-root services.yml on a typo'd env (loses the old
typo guard); `certs issue` now panics on registrars other than route53/cloudflare (narrowing);
`grafana/grafana` compiled binary untracked and not gitignored; file-provisioned alert rules are
never deleted without a `deleteRules` section; `HostsForService` nil-`Lb` panic edge; ring
reuseport proxy can EADDRINUSE-retry-log forever against an lb stream listener (PLAUSIBLE).

**sdk**: `sim_device.go` compiles into production builds (required by sim-latency imports; no
backdoor — unbindable constructors, absent from cgo/js exports; framework-size noise; next cgo gen
should land Sim symbols in the skip list); ConnectGrid reconcile can eject a provider added in the
pull→reclaim window (self-heals ≤30s, cosmetic flicker); contract-details run total seeds with a
contract's lifetime bytes when the view opens mid-transfer (design edge).

---

## 7. Performance assessment

**Improvements that check out**
- api request path: drain adds one atomic load per request; router_stats refactor is
  logic-identical extraction; the only new lock is `httpConnTracker`'s global mutex on ConnState
  transitions (~2 uncontended lock+map ops per keepalive request; connect hijacks upgrade conns so
  per-message cost is zero). The alloc-diet properties (trie, bounded stats) are intact.
- reliability writer: same-key `TxPipelined` HINCRBYs, packed 65-byte fields (~54% smaller),
  HSCAN COUNT 5000 drain replaces the 200-290ms HGETALL event-loop hold; RELIABILITY2 records the
  measured win (hot node 7,014 → 165 HINCRBY/s). Drain correctness under HSCAN duplicates holds by
  construction (absolute counters, map overwrite, clamped finality).
- peers/hops v3: steady-state read burden drops to ~1 GET/listener/5min + event-driven deltas;
  backpressure degrades to poll-bounded staleness at every layer; standing conns = pods × masters.
  Accepted trade: `Kg$sx` makes every SET/DEL/EXPIRE on every node run pattern matches for
  ~2 patterns × pods (heartbeat EXPIREs generate events every pod discards at dispatch).
- connect drain sweep pacing: 10k residents in ~60s vs the old ~2000s serial walk (the unbounded
  phase left is the broadcast, §5).
- taskworker: claim path byte-identical, poll index still exactly matched; new per-task overhead
  is O(1) atomics.
- proxy: no per-packet additions anywhere; redis is 3 single-key cmds/min + one SET per window
  change.
- connect p2p: the ready-header serialization fixes a real short-buffer race (verified against
  pion sources); `MaxPeerConnectionCount` bounds pion stacks with sound slot recovery; message
  pool discipline in the p2p paths is exactly-once with no aliasing found.
- sdk: contract-details rewrite (4 feeds→2, 1/s recompute throttle, O(n²) pairing deleted),
  `cachedWindowMonitor` (kills per-call alloc + resubscribe churn), memory-scaled NAT LRU caps for
  phone providers, 200-cap block-action retention.
- announce hot loop: +1 GETDEL on the first provide-enabled sync only, outside stateLock.
- SNI capture: per-443-packet cost is two value-type conversions + a 3-byte check; capture memory
  bounded (128×8KB, 10s TTL); no packet-buffer retention.

**Watch items**: provider NAT caps + over-cap O(n)-under-mutex eviction (§5 — the one perf
*regression* candidate on busy providers); CreateContract 1s retry amplification (§5);
FindProviders2 sampling when enabled (§5); the 30d-idle deactivate/delete waves (§5); reliability
rollup now 33 HSCANs+DELs per block vs 1 HGETALL (background task, deliberate);
`NetworkPeersEnabled` scans dormant clients per 5-min cache miss (bounded, accepted); monitor
probe load is fine *except* the connect-rate scan (§4.7) and per-tick batteries (§5); while any
partial ClientHello reassembly is pending, every TCP/443 packet takes the global sniffer mutex
(brief hold, regular under post-quantum hellos).

**Known hotspot not addressed** (by design, still open): `MultiRouteSelector` reflect.Select —
`transfer_route_manager.go` unchanged in this delta.

**Pre-existing, not this delta**: `router_stats.go:197` `min(...)` looks like it should be
`max(...)` (log-only bucket span); the stream Eval-TTL bug predates the delta (§5) but is
re-entrenched by it; `TcpSequence.send`/`UdpSequence.send` failure paths never pool-return the
packet — now hit more often because eviction/60s-idle makes sends-to-closed-sequences more
frequent (GC pressure, not a leak).

---

## 8. Backward-compatibility summary

Verified safe: wire protocol (additive; old clients drop migrate frames safely — traced, not
assumed); DB migrations (additive, tail-appended; validity SQL untouched); redis keys (new proxy
drain keys TTL-bounded and unknown to old builds — clean rollback; reliability legacy+shard
read-both; peers v2 counters retained as the corrective backstop); warpctl ↔ services `/status`
in all four old/new pairings; nginx retry narrowing (`error timeout http_502 http_503
non_idempotent`, tries=2 — 500/504 dropped, `non_idempotent` retained deliberately per APIDRAIN1
§6.2, with the >30s-POST timeout re-execution a documented accepted residual); services.yml
parsing; sdk rpc gob both directions; JWTs (no claim/validation changes); StreamReset reconcile
(both client generations wire-legal); GET retry (connect `HttpGetWithStrategyRaw` 502/503 ×1
jittered + js fetch_retry — POSTs untouched, pinned by tests).

Behavioral narrowings to know about: `egressRelationship` closes the None-promotion hole (strict
narrowing, no legitimate loss — §2); type-65 DNS claimed unconditionally (silent drop in
remote-DoH-off configs, §5); `BlockAction.Ips/Hosts` now disjoint from `MatchedIps/Hosts`
(consumers union for totals); provider NAT caps (§5); `certs issue` registrar narrowing (§6).

Requires coordination: sdk surface (apple/android/windows/js lockstep + npm version bump,
§4.5-4.6); taskworker-vs-connect ordering for reliability (rollback direction, §5); grafana
ring-port rollout (all hosts promptly, §5); `transfer_contract` STATISTICS migration applied
manually on prod ahead of the next nightly ANALYZE (its own comment says so); Target-not-found
backoff clamp only takes effect next release (no new task types ship this deploy — zero exposure
now).

Ops config dependencies: `notify-keyspace-events "Kg$sx"` must be persisted on every cluster
node — the live-set script and edge-6's redis.conf.j2 (last-wins section) agree, but
**edge-2's persisted redis.conf still has `notify-keyspace-events ""`** with no override; a
restart of a node with the stale conf silently reverts to poll-bounded (5-min) freshness (the
monitor's keyevents probe watches for exactly this drift once deployed). MinIO 7-day retention is
an ops-side ILM rule — nothing in code sets it, and a missing minio.yml silently falls back to
local disk.

---

## 9. Test-coverage gaps worth closing (top of the list)

1. **`SendNoContract` mid-sequence drop** — the most money-relevant change in the delta (fixes a
   real over-credit/panic bug) has zero coverage: neither the unacked==0 immediate-close nor the
   park-then-close-on-ack case.
2. A `-race` orphan-RST test with a real pooled packet + concurrent pool traffic (the §4.2 UAF is
   structurally invisible to the existing tests); also an IPv6 RST construction case.
3. Real deploy-order tests for proxy handoff/prewarm (§4.3/§4.4 live exactly in the untested
   ordering; every existing test orchestrates the halves in the harness-friendly order).
4. api: an e2e over the main-wiring SIGTERM sequence (the §5 latch inversion lives there);
   taskworker: the give-up path (root-cancel vs finalize).
5. Key events: conn-level drop with generation ON (the silent-reconnect divergence); hops-key
   expiry with a live eid (would fail today); TTL assertions on stream keys (would have caught the
   ns bug); `SubscribeKeyEvents` has zero direct tests and its cluster path never runs in the
   single-node test env.
6. sdk: a types.ts ↔ wasm-bindings parity check (exactly the class of §4.5); the real
   `migratePlatformTransport` success path (the server e2e re-implements the client side; the two
   copies can drift).
7. connect drain: the forward-only-connection sweep path (§5 split-state); a clock-skewed
   `migrate_time` client test.
8. connect ip: type-65 client-visible behavior with remote DoH off; an ECH outer-SNI case;
   GlobalLimit under concurrent SYN load.
9. monitor: zero tests overall — ticket lifecycle, parsers, and tailer are pure-logic and would
   have caught three of the §5/§6 items.

---

## 10. What holds up well

The drain architecture end-to-end (admission gates before model work, bounded waits, truthful
/status in all four version pairings, prompt exits); the §2.3 WaitGroup fix and the single-writer
handback invariant in task; the http drain machinery (h1-gated Connection:close, exactly-once
Shutdown, context split that keeps stats alive through the drain); the per-member peer key design
(same-slot atomic writes, EXPIRE-only heartbeats, consistent event classes end-to-end); HSCAN
drain correctness by construction; the excuse lifecycle (server-only writer, GETDEL-once, TTL,
fail-punitive); old-client migrate safety; the ProvideMode cleanup (closed a real LAN-reach hole);
the `SendNoContract` accounting fix; the p2p ready-header race fix; the grid-dot leak fixes traced
end-to-end on both the connect and sdk sides (slot-guarded removal, replace-without-terminal,
done-watchdogs, unhealthy-eviction — plus a real loopback rpc leak repro); the SNI parser's
robustness posture (bounds-checked, bounded reassembly, charset-validated, no packet retention)
and the closed type-65 hint gap for blocked names; monitor's read-only discipline (session-level
`default_transaction_read_only=on`, stdin-fed password, grep-verified zero write commands); the
warpctl config refactor (mechanical, field-identical, well-tested) and the new per-host drain
flock; stats' fsync→rename segment discipline and fail-safe anonymization.
