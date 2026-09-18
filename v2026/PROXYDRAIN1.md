# PROXYDRAIN1 — proxy drain: use the grace we already have, then remove the blip

How a proxy service deploy disrupts customer connectivity today, why the
deploy scaffold already does most of the hard work, and the staged plan to
make deploys first graceful (stop killing in-flight traffic) and then nearly
seamless (a sub-5s blip with established inner flows surviving).

Companion to CONNECTDRAIN2.md (the connect-service drain, whose machinery and
lessons this reuses) and the warpctl deploy path in `../warp`. "1" because
this is the first generation of the proxy drain subsystem; the vN naming
aligns with CONNECTDRAIN2 only by family, not by number.

Status: IMPLEMENTED 2026-07-19 (same day as the audit) — P0 readiness +
metrics, P1 graceful drain, P2 activity pre-warm, P3a wg endpoint handoff,
P3b window identity persistence + orphan RST. See §8 for what shipped where
and the measured results. warpctl (§1.1) needed no changes, as designed.

## 1. How a proxy deploy works today

### 1.1 The warpctl scaffold (already make-before-break)

Topology: 2 dedicated hosts (`fireside.bringyour.com`, `crisp.bringyour.com`)
x 10 blocks each (`vault/main/services.yml:388-410`). No nginx in the
datapath — the proxy hosts are `transparent: true` lb blocks, so exposure is
direct iptables DNAT on the host: fixed-forever external ports per
(block, service port) (e.g. g8: 7158→80 status, 7159→8080 socks, 7160→8081
http, 7161→8082 https, 7162→8083 api, 7163→8084 wg udp;
`warp/warpctl/run_test.go:468-480`), internal container ports rotated per
deploy from a 30-port pool. udp replies are SNAT'd back to the public
ip:external-port (`run.go:1466-1596`).

Deploy sequence per block (`warp/warpctl/run.go:582-708`), all 10 blocks
concurrently on a version publish:

1. New container starts on rotated internal ports, health-polled up to 120s
   on `GET /status` (`run.go:1200-1292`).
2. `redirect()` swaps the DNAT rules: NEW flows conntrack to the new
   container immediately; ESTABLISHED flows keep their conntrack entry and
   continue to the old container's still-open sockets (`run.go:1406-1462`).
3. `cleanupStaleConntrack()` flushes udp entries pinned to pool ports with NO
   listener — draining containers still hold their sockets, so their flows
   are preserved (`run.go:869-931`).
4. Old containers drain asynchronously, serialized one-block-per-host by the
   host drain flock (`host_drain_lock.go`, CONNECTDRAIN2 §3.4):
   `docker update --restart=no` + `docker container stop -t 3600`
   (`DrainTimeout = 60min`, `run.go:48,2124-2141`) → SIGTERM
   (`cli/proxy/Dockerfile:19`).
5. After each old container exits, `cleanupStaleConntrack()` runs again: the
   dead container's pinned udp flows are flushed, so the next client packet
   re-evaluates DNAT and lands on the new container (`run.go:703-707`).

So the network layer already provides: overlap, atomic cutover for new
flows, preservation of established flows, an hour of grace, per-host
stagger, and post-exit unpinning. This is exactly the scaffold a graceful
drain needs.

### 1.2 What the proxy binary does with it: nothing

`cli/proxy/main.go:57-65` — SIGTERM/SIGQUIT → `quitEvent` → cancel the root
ctx, with a literal `// FIXME drain`. Everything dies at once:

- Every in-flight SOCKS/HTTP(S) tunnel is closed mid-byte. The `../proxy`
  lib has no graceful mode anywhere: http uses `Close` not `Shutdown` with
  hijacked conns killed via `context.AfterFunc(ctx, conn.Close)`
  (`../proxy/http.go:162-168,269-271`, `relay.go:219-222,262-277`), socks
  likewise (`socks5_server.go:62-65`).
- `WgProxy.Close` tears down the wg device, all peers, and all noise
  sessions (`../proxy/wg.go:662-677`). Client packets go unanswered.
- Every `ProxyDevice` (embedded `DeviceLocal` + gVisor tun) is canceled;
  contracts and the multi-client egress window state are dropped on the
  floor.
- The process exits within ~30s (the status/api http servers' 30s
  `ShutdownTimeout` is the slowest part) — using none of the 3600s grace.

### 1.3 What a customer experiences

**wireguard** (single udp 5-tuple; packets pass through the device, inner
TCP endpoints live at the customer and the egress provider):

- t0 old container killed: tunnel silent. The pinned conntrack entry keeps
  steering retries at the dead port until the post-exit flush (seconds).
- t+~15s: client detects the dead session (10s keepalive timeout + 5s rekey
  timeout, `userwireguard/device/constants.go:19,23`) and re-initiates every
  5s. The new container restored the full peer set at startup (watch from
  changeId 0 delivers every proxy_client for the host/block,
  `proxy/proxy_client_notification.go:172-199`,
  `model/network_client_proxy_model.go:876-926`), so the handshake succeeds.
- First data packet triggers the tun factory → `OpenProxyDevice` cold start:
  pg config load + jwt + `DeviceLocal` + platform dial + egress window
  establishment (`proxy/proxy_device.go:179-253`); packets drop until the
  window is satisfied.
- Net blip ≈ 15-30s+ when everything works. Herd friction on top: 1024-deep
  handshake queue with silent overflow drop
  (`userwireguard/device/queueconstants_default.go:16`,
  `receive.go:210-220`), mac2 cookie round-trip once under load
  (`device.go:218-228`), 20/s-per-source-ip limiter.
- **And the tunnel coming back is not enough**: every established TCP
  connection inside it is orphaned at the provider (§2.3) and hangs.

**SOCKS/HTTPS**: established tunnels die instantly at SIGTERM (mid-transfer,
no FIN courtesy). New connections already go to the new container (the flip
preceded the stop) and only pay the device cold start.

**device-rpc**: websocket sessions drop; `deviceGeneration` lets the remote
detect the recreate and reconnect (`proxy/proxy_device.go:547-549`).

## 2. Why this is structurally different from connect drain

### 2.1 No sibling absorption — blue-green is the only lane

A proxy client is assigned (host, block) uniformly at random at create and
the assignment is sticky forever; every issued endpoint — socks/http/https
urls, the wg `Endpoint` — bakes in the per-host FQDN and per-block external
port (`model/network_client_proxy_model.go:632-642,656-688,756-771`). There
is no re-fetch API and no migration. Connect's drain model (evict → client
redials to a sibling via the lb) cannot apply: the ONLY server that can
serve a client is the new container of its own block. Fortunately that is
exactly what warpctl's overlap provides.

### 2.2 Identity is stable; the platform tolerates a restart

`ProxyDeviceConfig` persists `{ProxyId, ClientId, InstanceId}`
(`model/network_client_proxy_model.go:349-391`), so a recreated device is the
same client instance to the platform. Note the proxy device never gets the
CONNECTDRAIN2 §3.3 `ResidentMigrate` make-before-break: `NewPlatformDeviceLocal`
forces `AllowProvider = false` (`../sdk/device_local.go:557`), and the migrate
handler lives on the provider client only (`../sdk/device_local_provider.go:106-137`).
Connect-service drains are absorbed instead by multi-client window
redundancy plus the Track A excusal. Out of scope here, but the asymmetry is
worth knowing.

### 2.3 Inner flows COULD survive — one thing severs them

Provider-side egress state is restart-tolerant by construction:

- NAT flows are keyed `{source TransferPath (clientId, streamId), inner ip
  4-tuple}` — no transport handle, no instanceId
  (`../connect/ip.go:639-674,932-948`).
- Eviction is lazy only: udp 60s source-idle, tcp 300s idle/read; no
  purge-on-disconnect exists (`ip.go:84,110,1549-1562,2832-2848`).
- Return traffic is addressed by logical clientId and re-resolved to fresh
  routes at write time (`ip.go:3304-3318`, `transfer.go:3183-3201`).
- A restarted sender's fresh sequenceId supersedes the old receive sequence
  (`transfer.go:3422-3446`); the reverse direction fast-forwards on head
  (`transfer.go:4138-4149`); contracts renegotiate without touching NAT
  state (`transfer.go:2434-2563,4386-4443`).

The single breaker: the proxy device egresses through
`ApiMultiClientGenerator`, which mints EPHEMERAL platform-derived clientIds
with `InstanceId: NewId()` per window entry, removed on teardown
(`../connect/ip_remote_multi_client_api.go:195-235`). A restart mints new
identities → every provider NAT flow orphans. Worse, orphaned non-SYN
packets are dropped SILENTLY (no RST, `ip.go:1839-1846`), so customer apps
hang to their own timeouts instead of reconnecting.

## 3. Design — graceful first, then seamless

Track ordering mirrors CONNECTDRAIN2: stop the bleeding with server-local
changes, then shrink the blip, then eliminate it. All tunables live in
`ProxySettings` / new settings structs — no globals.

### 3.1 P0 — readiness + drain visibility (tiny, server-only)

- `/status` returns 503 until the initial full peer sync has been applied to
  the wg device (today `/status` is a constant "ok" —
  `router/warp_handlers.go:69-91` — so the DNAT flip can theoretically beat
  peer install; the startup no-callback delivery race can add a 5s retry
  cycle, `proxy_client_notification.go:202-209`). warpctl's existing 120s
  health poll then gates the flip for free.
- After SIGTERM, `/status` reports draining (observability only; nothing
  routes on it).
- Metrics (mirror CONNECTDRAIN2 §3.5): `urnetwork_proxy_ready`,
  `urnetwork_proxy_drain_active_remaining`, `urnetwork_proxy_devices_live`,
  `urnetwork_proxy_prewarmed_devices`, `urnetwork_proxy_wg_peers`.
  Deploy-facing signals runbook: monitor/SIGNALS.md §14.

### 3.2 P1 — implement `Drain()` (the FIXME; biggest single win)

On SIGTERM, instead of canceling the root ctx:

1. Flip draining; `/status` reflects it. Stop ACCEPTING new socks/http
   connections (post-flip, new flows go to the new container anyway; the
   gate is belt-and-suspenders for pinned-source oddities and local use).
2. Keep established socks/https tunnels relaying and keep the wg device
   serving pinned flows for a bounded `DrainGraceTimeout` (default ~2-5min,
   see §4 for why not the full hour).
3. As the deadline approaches, close remaining tunnels paced (connect's
   straggler-sweep shape, `connect/resident.go:1245-1316`).
4. On each device teardown, close its contracts cleanly (today they are
   dropped and linger to expiry — the netEscrow-drift adjacent cost).
5. EXIT THE INSTANT the drain is empty (CONNECTDRAIN2 §3.4's 28-minute
   lesson: never idle toward the `docker stop -t` ceiling — the host drain
   flock serializes blocks, so idle time multiplies rollout time x10/host).

Requires a graceful mode in `../proxy` (cross-repo; `cd ../proxy && go vet`
per the WgTun precedent): stop-accept + per-connection deadline drain for
http/socks, and a WgProxy "keep serving, no new state" mode. The drain wait
itself is socks/http-idle-bounded, with the grace as its CEILING, not its
duration. wg needs no eviction step — pinned clients CANNOT leave until the
process exits and warpctl flushes conntrack — so wg rides along for as long
as the socks/http drain keeps the process alive, and a wg-only block exits
soon after SIGTERM. With §3.4's handoff that prompt exit is BENEFICIAL: it
advances the drain-end export (the replacement's drain-complete beacon, §8)
and the conntrack flush, and the replacement re-establishes the wg sessions
from its side in ~1 RTT.

Effect: deploys stop killing mid-flight transfers; wg customers are served
for as long as the process stays up and blip only at the end, where
warpctl's flush hands them to the (already warm, §3.3) new container.

### 3.3 P2 — activity-aware pre-warm (removes the cold start + herd)

Today's warmup only covers clients CREATED within `WarmupTimeout` (30min)
(`cli/proxy/main.go:132-145`) — a client active for months gets a cold
device on its first post-deploy packet, concurrently with every other active
client (per-id single-flight only, no global bound).

- Track the active set: batched flush of `lastActivityNanos` per proxyId to
  a per-(host, block) redis key every ~1min (or derive from wg
  last-handshake via `IpcGet`).
- The new container — already running while the old one drains — pre-opens
  devices for proxyIds active within `PrewarmActivityWindow` (~10min) with
  bounded concurrency + jitter, waiting for egress windows
  (`WaitForReady`). Optionally fold "prewarm complete OR budget elapsed"
  into readiness.
- Retire the create-time special case once this lands.

By handover, active clients hit warm devices: first packet egresses
immediately.

### 3.4 P3a — wg fast re-establishment (cuts ~15s to ~1 RTT)

The 15s is client-side dead-session detection. WireGuard is symmetric: with
a known endpoint the SERVER can initiate. The pieces all exist:

- Old container exports learned per-peer endpoints + last-handshake recency
  at drain end (`IpcGet` already serializes endpoints,
  `userwireguard/device/uapi.go:56-160`) → short-TTL redis keyed
  (host, block).
- New container applies endpoints on `AddClients` (uapi `IpcSet2` accepts
  peer endpoints today; `../proxy/wg.go` AddClients just doesn't plumb them)
  and proactively initiates handshakes for recently-active peers, retrying
  for ~grace+60s. Initiations sent BEFORE the conntrack flush are harmlessly
  blackholed (the client's reply re-DNATs to the old container); the first
  attempt after the flush converges in ~1 RTT. The client's own NAT mapping
  stays open thanks to `PersistentKeepalive = 25`
  (`model/network_client_proxy_model.go:766`).

No client changes; degrades exactly to today's client-driven re-handshake.

### 3.5 P3b — window identity persistence (inner flows survive)

Persist the multi-client window identity per proxyId so the recreated
device REUSES its window clients instead of minting:

- On window change, write the window-client args (platform clientIds /
  instanceIds / auth) + provider destinations to a short-TTL redis key per
  proxyId.
- The recreated device's generator consumes them: re-authenticate the SAME
  window clientIds against the SAME providers first, top up from the api
  only for shortfall.
- TTL ~5min: comfortably beats the provider tcp 300s eviction; udp 60s is
  the tight timer and is why P3a's fast re-establishment matters.
- In connect, add fail-fast for orphans: a bounded/rate-limited RST for
  non-SYN packets with no matching sequence (`ip.go:1839-1846`), so whenever
  continuity DOES fail the customer's app reconnects instead of hanging.

With identity stable, everything else already works (§2.3): established TCP
inside a wg tunnel survives the deploy with a sub-5s stall.

socks/https tunnels terminate TCP in-container; P1's grace is their ceiling
(the same deal nginx-style draining gives every other service).

### 3.6 Explicitly deferred

- Split ingress plane (stable wg/tcp termination process + frequently
  deployed device plane over a local handoff) and cross-container fd
  handoff: not worth the complexity if P0-P3 land.
- Config re-fetch / host-migration API: the adjacent gap (a removed HOST
  strands every issued config forever — no re-fetch, no reassignment). Worth
  doing independently of drain; it is also what would let ops drain a whole
  host, not just a container.

## 4. Correctness / hazard review

| Concern | Answer |
|---|---|
| Two containers hold the same clientId+instanceId during overlap (resident nomination + sequence churn) | Bounded by `DrainGraceTimeout` (minutes, not the hour). wg is naturally single-homed — ONE udp 5-tuple per client, atomically pinned-to-old or flipped-to-new, never split. The split case is a socks/https customer opening new tunnels (new container) while old tunnels drain (old container); that churn exists today at full severity (hard kill); a bounded grace strictly improves it. With P3b in play the overlap is NOT acceptable for the forced pre-warm set (REVIEW2-UPDATE1 §4.4: the new side would force-establish windows with the same persisted identities the old side still egresses with, and the old side's live-ctx eviction can api-remove the identity the new side is using), so the pre-warm is gated on the old instance's drain-complete beacon (§7.2 amendment, §8): the pre-warm set never dual-uses. Residual dual-use is only clients that actively send to the new container during the grace (lazy opens, deliberately ungated) — their old-side flows are darkening anyway, and it is bounded by the grace. |
| Serving pinned wg during drain delays client migration | Yes, by design: pinned clients cannot migrate until conntrack flush, which warpctl runs only after exit. Grace trades continuity-now for blip-later; with P3a the end-of-grace blip is ~1 RTT, so long grace is cheap for customers, bounded for rollout time by prompt exit. |
| Endpoint handoff (P3a) leaks or spoofs peer addresses | Redis key is server-written, server-read, short-TTL, per (host, block); endpoints are already observable to the server. A wrong/stale endpoint costs one wasted initiation; the client's own retry path is unaffected. |
| Server-initiated handshakes before the flush confuse the client | The reply is DNAT'd to the old container and dropped; wireguard initiations are idempotent and rate-limited client-side; whichever side completes first wins. |
| Window identity reuse (P3b) lets a stale window pin a bad provider | TTL-bounded (~5min) and top-up-from-api covers shortfall; the multi-client ranking machinery still replaces bad entries post-restore exactly as it does live. |
| Readiness gating wedges a deploy (peer sync never completes) | The 120s health-poll ceiling already fails the deploy and reverts (`KillWorker`, `run.go:594-599`) — same failure mode as any unhealthy container, now with a truthful signal instead of a false "ok". |
| Orphan RST (P3b) becomes an injection surface | RST only for {known source clientId, no matching sequence}, rate-limited; an attacker who can forge that source path can already do worse. |
| A live multi-client rebuild (e.g. location change) restores stale identities (P3b) | Bounded and self-healing: the old window's live-ctx evictions remove its identities from the store and the api as the rebuild tears it down; a racing reuse of an api-removed identity fails auth, is evicted, and the window mints fresh. The 10min ttl bounds any residue. |

## 5. Rollout

1. **P0 readiness + metrics** — server-only, no behavior change to traffic;
   ships first so the next deploy is measured.
2. **P1 Drain()** — server + `../proxy` lib graceful mode. Flag-gated in
   `ProxySettings` (`EnableDrain`, `DrainGraceTimeout`,
   `DrainStragglerSweepTimeout`). Verify: rollout time per host stays
   bounded (prompt exit), tunnel-kill count at deploy → ~0.
3. **P2 pre-warm** — server + redis activity set (`PrewarmActivityWindow`,
   `PrewarmConcurrency`). Verify: post-deploy first-packet egress latency
   collapses; no db/platform herd.
4. **P3a wg fast re-establishment** — `../proxy` + `userwireguard` endpoint
   plumbing + handoff. Verify: blip P50 from ~15-30s to ~1-2s.
5. **P3b window persistence** — `../connect`/sdk generator hooks + server
   storage; the largest change, last, independently flagged. Verify: an
   iperf/download through a wg tunnel survives a deploy without TCP reset.

## 6. Implementation inventory (files)

- `cli/proxy/main.go`: replace the FIXME with the drain sequence; wire
  readiness into the `/status` route; prompt exit.
- `proxy/server.go` / `proxy/proxy_device.go`: drain-aware accept gates;
  device teardown closes contracts; activity flush for the pre-warm set;
  pre-warm loop on startup.
- `proxy/proxy_client_notification.go`: expose "initial full sync applied"
  for readiness.
- `router/warp_handlers.go`: readiness-aware status (or a proxy-local
  status route).
- `../proxy` (cross-repo, vet both): http/socks graceful mode; WgProxy
  drain mode; AddClients endpoint plumbing; endpoint export helper.
- `../userwireguard`: none expected (uapi already supports endpoints both
  directions); verify only.
- `../connect`: `ApiMultiClientGenerator` persistence hooks (P3b); orphan
  RST (P3b).
- `../warp`: no changes required — the scaffold (overlap, flush, stagger,
  grace) is already correct. Optional: raise the 120s health poll budget if
  pre-warm folds into readiness.
- Tests: proxy drain e2e mirroring `connect/exchange_drain_e2e_test.go`;
  extend `../proxy/wg_restart_test.go` for endpoint-handoff; deploy
  dashboard = time from old-exit to Nth peer re-handshake.

## 7. Decisions (RESOLVED — implemented 2026-07-19)

1. `DrainGraceTimeout = 2min` (fast rollouts; raise with deploy data).
2. Readiness waits for the initial peer sync ONLY; the pre-warm and the wg
   handoff apply race the old container's drain grace after readiness flips.
   AMENDED 2026-07-20 (FIXPLAN1 decision 3): readiness is still sync-only —
   the flip stays fast — but the pre-warm no longer races the grace. It is
   sequenced after the wg handoff outcome: the old instance's
   generation-tagged export lands at drain END (empty peer set = completion
   marker), so it doubles as a drain-complete beacon that ungates the
   pre-warm; poll-budget expiry is the crashed-old fallback. Accepted trade:
   the pre-warmed set turns warm at old-drain-end rather than at flip, and
   dual-use of persisted window identities during the grace is eliminated
   for the pre-warm set. Customer-driven lazy opens stay ungated.
3. P3b storage: per-proxyId key (`{pwi_<proxyId>}`, 10min ttl,
   self-reaping), full-snapshot writes.
4. Activity source: device-level `lastActivityNanos` (covers every mode)
   for the pre-warm set; wg last-handshake (via `PeerStatuses`) for the
   endpoint handoff export filter. Both are used, each where it is
   authoritative.

## 8. Implementation inventory (as shipped, 2026-07-19)

All flag-gated via `ProxySettings` (`EnableDrain`, `EnablePrewarm`,
`EnableWgHandoff`, per-device `DisableWindowIdentityPersistence`) and
`TcpBufferSettings.EnableOrphanRst`; defaults on.

**P0+P1 — readiness + graceful drain.**
- `github.com/urnetwork/proxy` (lib): `drain.go` `drainState` (zero-value
  usable, embedded by value); `HttpProxy`/`SocksProxy` gain
  `Drain`/`ActiveCount`/`WaitIdle`; during a drain, listeners close, new
  requests get 503 `Connection: close`, in-flight tunnels keep relaying and
  `ListenAndServe` holds its runCtx (returning would kill them) until the
  caller cancels — the hard teardown. Drain/cancel listener closes are
  clean nil exits (previously ErrServerClosed reached a panic).
- server `proxy/readiness.go`: `/status` 503s until the initial
  proxy-client sync is applied (`proxyClientNotification.InitialSyncDone`,
  which requires first successful read AND delivery per watch), 503
  "draining" after SIGTERM; `urnetwork_proxy_ready` gauge. warpctl's
  existing 120s health poll gates the DNAT flip on it, unchanged.
- server `proxy/drain.go`: `DrainCoordinator` — flip readiness, drain the
  socks/http targets, wait idle or `DrainGraceTimeout` (1s-paced
  `urnetwork_proxy_drain_active_remaining` gauge + progress logs), run
  before-exit hooks (bounded by `DrainBeforeExitTimeout`), return. No
  straggler pacing on purpose: evictions here push no redials onto this
  process (the flip already moved new flows), so there is no refill loop.
  `cli/proxy/main.go` replaces the `FIXME drain`: SIGTERM → `Drain` →
  cancel → exit 0 immediately (never idles toward `docker stop -t`). The
  wg server is NOT a drain target: pinned clients cannot migrate until the
  process exits and warpctl flushes conntrack, so it serves for as long as
  the process is up. The drain wait is therefore socks/http-idle-bounded
  with the grace as its ceiling — NOT deadline-bounded: a wg-only block
  exits soon after SIGTERM, which is beneficial since the prompt exit
  advances the drain-end export (the replacement's drain-complete beacon,
  below) and the conntrack flush, and P3a re-establishes the sessions in
  ~1 RTT.
- Contract close-out on teardown needed no new work: the contract manager
  already final-flushes on client close.

**P2 — activity + pre-warm.**
- model `network_client_proxy_activity_model.go`: per-(host, block) redis
  zset (`{pca_<host>_<block>}`), self-reaping (1h retention / 2h ttl);
  `TouchProxyClientActivity` / `GetActiveProxyClients`.
- server `proxy/activity.go`: `StartActivityFlusher` (1min pace, from
  `ProxyDeviceManager.ActiveProxyIds`; also `urnetwork_proxy_devices_live`);
  `Prewarm` opens devices for clients active within
  `PrewarmActivityWindow` with `PrewarmConcurrency`-bounded workers +
  jitter, `WaitForReady` each, bounded by `PrewarmTimeout`
  (`urnetwork_proxy_prewarmed_devices`). GATED on the old instance's drain
  (2026-07-20, FIXPLAN1 decision 3 / REVIEW2-UPDATE1 §4.4): after readiness
  flips, `cli/proxy/main.go` runs `ApplyWgHandoff` — which blocks until the
  old instance's drain-end export appears or `WgHandoffPollTimeout` expires
  (the crashed-old fallback) — and only then `Prewarm`, in the same
  goroutine. The forced window establishment with reused persisted
  identities therefore starts only after the old side stopped using them
  (its live-ctx eviction could otherwise api-remove the identity the new
  side is using). Trade: the pre-warm set turns warm at old-drain-end, not
  at flip. Readiness is not gated; customer-driven lazy opens are not
  gated.

**P3a — wg endpoint handoff.**
- `github.com/urnetwork/proxy` wg: `WgClient.Endpoint` seed,
  `PeerStatuses()` (learned endpoint + last handshake via device IpcGet),
  `SeedEndpoints` (UpdateOnly, never creates peers), `InitiateHandshake`
  (device retry timers take over; internally rate limited).
- model `network_client_proxy_wg_handoff_model.go`: `{pwh_<host>_<block>}`
  keys — the replacement's generation request (`BeginProxyWgHandoff`,
  published at boot) and the generation-tagged export
  (`SetProxyWgHandoffForGeneration`), consumed exactly once (getdel). The
  ttl is passed by the caller from `ProxySettings.WgHandoffRequestTtl`
  (default 20min; the model only guards <= 0): the request must survive
  until the old instance reads it at drain end, i.e. a whole serialized
  multi-block host drain.
- server `proxy/wg_handoff.go`: `ExportWgHandoff` (drain before-exit hook;
  filters to peers with a handshake inside `WgHandoffActivityWindow`; an
  empty peer set is exported as the completion marker), `ApplyWgHandoff`
  (after initial sync: BLOCKS polling for the generation-tagged export on
  `WgHandoffPollInterval` up to `WgHandoffPollTimeout` — default 15min,
  sized for blocks × grace, and doubling as the pre-warm gate budget —
  then seeds endpoints and drives initiations in the BACKGROUND on
  `WgHandoffInitiatePace` until each peer completes a NEW handshake or
  `WgHandoffInitiateTimeout`, a budget that starts at export-seen, not at
  boot, so a long host drain cannot eat it (REVIEW2-UPDATE1 §5.6);
  pre-flush initiations are harmlessly blackholed). A poll-budget expiry
  that races the export GETDEL is absorbed as the clean no-export return
  (`takeHandoffExport`), not an "Unexpected error".
- Measured: lib restart recovery 500ms (vs ~15-20s client-driven);
  server-level harness 200ms.

**P3b — window identity persistence + orphan RST.**
- connect `ip_remote_multi_client_identity.go`: `WindowClientIdentity`,
  `MultiClientIdentityStore` (full-snapshot Store / once Load),
  `MultiClientGeneratorWithDestination` (optional generator extension), and
  the per-destination restore QUEUE (a destination can carry several window
  clients; each is restorable once).
- connect `ip_remote_multi_client_api.go`: `SetIdentityStore`;
  `NewClientArgsForDestination` reuses a restored identity (same client id
  + jwt + instance id) or mints-and-records; `NextDestinations` dials
  restored destinations first; `RemoveClientArgs` distinguishes window
  eviction (ctx live → remove from store AND the api) from shutdown
  teardown (generator ctx done → keep both, so the replacement can reuse).
  `DeviceLocal.Close` cancels its ctx before closing the multi client, so
  every shutdown path sees ctx-done.
- connect `ip.go`: orphan RST — a non-SYN packet matching no sequence gets
  an RFC 793 reset back to the source (built outside the buffer lock;
  never resets a reset; `OrphanRstPerSecond` valve, default 256/s), so a
  flow that could NOT be resumed fails fast instead of hanging.
- sdk: `DeviceLocalSettings.MultiClientIdentityStore` threaded to the api
  generator.
- server: model `network_client_proxy_window_model.go` (`{pwi_<proxyId>}`,
  10min ttl) + `proxy/window_identity.go` adapter (errors swallowed —
  persistence is best-effort and must never break the window), wired per
  device in `NewProxyDevice`.

**Tests.**
- lib: `drain_test.go` (state machine, http tunnel-survives-drain +
  kept-alive 503, socks relay-survives-drain, serve-after-drain),
  `wg_handoff_test.go` (endpoint export → replacement seeds + initiates →
  recovery in 500ms, reverse path).
- server proxy: `drain_coordinator_test.go` (idle completion, grace
  deadline, disabled mode, readiness handler),
  `proxy_client_notification_test.go` (initial sync: empty host; delivery
  gating), `drain_e2e_test.go` (full-harness: held socks tunnel carries
  real https across the drain, new conns refused, WaitIdle),
  `activity_test.go` (ActiveProxyIds; full-harness pre-warm on a fresh
  manager), `wg_handoff_test.go` (full-harness: apply waits for the
  delayed generation-tagged export, then server-initiated
  re-establishment in ~200ms, then traffic; poll-expiry race returns
  clean no-export), `deploy_overlap_test.go` (full-harness two-instance
  deploy: the new instance holds at the beacon gate — no devices, no
  identity restore — while the old instance's device stays live, ungates
  on the old side's real drain-end export, pre-warms and reuses the
  persisted identity; plus the crashed-old poll-budget fallback),
  `window_identity_test.go`
  (full-harness: shutdown keeps the snapshot; recreated window reuses a
  persisted identity against the same provider, then serves).
- connect: `ip_remote_multi_client_identity_test.go` (restore queue,
  consume-once, snapshot mirroring, nil store),
  `ip_orphan_rst_test.go` (rst form for ack/no-ack orphans, never-reset-a-
  reset, rate limit, disabled).
- model: activity set, wg handoff store (consume-once, generation
  isolation, caller-ttl actually applied), window identity store tests.
