# CONNECTDRAIN2 — connect drain: excuse the disruption, then eliminate it

How a connect service sheds its clients on deploy/rebalance, why that currently
costs providers reliability score and flashes every device offline to its
peers, and the staged plan to fix both — first by EXCUSING drain-caused
disruption from the penalty paths, then by making the migration itself
gapless so there is nothing to excuse.

Companion to PEERS2.md / PEERSSTREAMS2.md (peer registry) and the reliability
model. This is a NEW subsystem doc; the "2" aligns with the vN generation
naming, there is no CONNECTDRAIN1.

Motivating incident (2026-07-18, observed live on `by-us-fmt-5-edge-4`): a
rolling deploy ran `docker container stop -t 3600` on the g1 AND g4 connect
containers of one host SIMULTANEOUSLY. Both drained blind for 28+ minutes; the
peer registry `eid` for an affected network sat frozen and its two devices
showed each other as offline the whole time. Every provider that reconnected
during the window took a reliability hit for a disruption the operator caused.

## 1. How drain works today

- **Trigger.** `docker stop -t 3600` sends SIGTERM. `cli/connect/main.go:63-70`
  catches SIGTERM/SIGQUIT and calls `Exchange.Drain()`; the `-t 3600` is the
  kernel's SIGKILL grace (1 hour) if the process has not exited by then.
- **Eviction.** `Exchange.Drain()` (`connect/resident.go:1058`) walks the
  residents/connections one at a time, `resident.Close()` + handle-cancel each,
  sleeping `DrainOneTimeout` (200ms, `resident.go:294`) between each, until the
  maps are empty. `DrainAllTimeout` is 60min (`resident.go:296`). There is no
  client-side coordination: a resident is closed, its transport drops, the
  client notices and redials — possibly to another block that is ALSO draining.
- **Reconnect accounting.** On redial the announce loop
  (`connect/transport_announce.go`) records reliability per 60s block. An
  ongoing connection sets `ConnectionEstablishedCount=1` (`:459`); a fresh
  connection sets `ConnectionNewCount=1` (`:468`), which **invalidates the
  block**. A provide-key change in the block sets `ProvideChangedCount=1`,
  which also invalidates. Block validity is decided by the
  `client_reliability_valid(...)` SQL (`network_client_reliability_model.go:141`),
  which forgives `ReliabilityAllowDisconnectCountPerBlock = 1` reconnect per
  block (`:73`). Blocks feed the 5m/60m/12h `ClientLookbacks` and the 7-day
  network window that drive the score and the payout multiplier.
- **Peer registry.** The resident cleanup calls `model.RemoveNetworkPeer`
  (`resident.go:520`, gated `EnableNetworkPeers && peerNetworkId != nil`),
  writing a disconnect marker. So every drain makes every providing device
  flash "disconnected" to all its network peers until it redials and re-adds.

## 2. Why drain hurts (three distinct costs)

1. **Reconnect invalidates the block.** One forgiven reconnect per 60s block
   means a *single* clean drain-reconnect is usually absorbed — but a rolling
   deploy that bounces a client twice within a block (its block drains, it
   lands on a sibling that drains minutes later) spends the forgiveness and
   invalidates. The live incident bounced both of a host's groups at once,
   maximizing this.
2. **Re-announce is a provide change.** The first sync on the new resident
   re-announces provide keys; `ProvideChangedCount` invalidates the block
   independently of the reconnect forgiveness.
3. **The gap itself.** Downtime between eviction and successful redial is
   missing blocks. A 90s drain gap erases ~30% of the 5-minute lookback's
   denominator — pure score loss with no counterpart credit.

Plus the non-score cost: the peer-registry disconnect marker (§1) means the
"You have N other devices online" line and the connectable-peer list drop the
device on every deploy, for the whole drain-plus-redial window.

## 3. Design — excuse first, then eliminate

Two independent tracks. Track A (excusal) is small, ships first, and removes
the *penalty* for drain disruption immediately. Track B (gapless migration)
removes the *disruption*, after which there is nothing left to excuse. Track A
stays as the backstop for the residual (crash, forced SIGKILL, client that
cannot make-before-break).

### 3.1 Track A — mark the disruption at the source, consume it at ingest

**Mark (writer = the drain path only).** `Exchange.Drain()` already touches
every resident individually. As it evicts each one, and anywhere an operator
action replaces a resident (nomination replacement, rebalance), write a
short-TTL redis marker keyed by client:

```
SET drain_excuse_<clientId> <residentId> EX <DrainExcuseTtl>   # ~5 min
```

Operator-scoped by construction: the ONLY writers are the server's own drain /
replace paths. A provider cannot self-excuse — it never sees this key. The TTL
bounds a stale marker to one excuse window; `<residentId>` lets the consumer
ignore a marker from a resident other than the one it is replacing.

**Consume (reader = the announce new-connection branch).** In
`transport_announce.go` where the else-branch sets `ConnectionNewCount=1`
(`:468`), `GETDEL drain_excuse_<clientId>` first. If present:

- record the sync as `ConnectionEstablishedCount=1` (the block stays valid)
  instead of `ConnectionNewCount`, OR add a dedicated
  `ConnectionExcusedNewCount` counter that the `client_reliability_valid` SQL
  treats as non-invalidating (preferred — preserves observability of how many
  reconnects were drain-excused vs organic);
- suppress the `ProvideChangedCount` invalidation for THIS first post-drain
  sync only (the re-announce is mechanical, not a real provide flap).

`GETDEL` = consumed exactly once; a second reconnect in the same window is NOT
excused (real flapping still fails). The validity SQL and the scoring/rollup
stay otherwise untouched.

**The gap (§2.3) under Track A alone:** still present — the excusal fixes the
reconnect/provide-change invalidation, not the missing blocks. Deliberately do
NOT patch the score math to credit excused gaps: the right fix for the gap is
Track B (make it ~zero), and synthetic gap credit is an abuse surface. Until
Track B, the residual gap is bounded and small (one drain per deploy per
client) and the forgiveness already absorbs the block boundary.

### 3.2 Track A — skip the peer disconnect marker on drain

At the resident cleanup (`resident.go:520`), when the teardown cause is drain /
replacement (not a real client disconnect), SKIP `RemoveNetworkPeer`. The
member registration carries its own TTL (PEERSSTREAMS2 §5.2 per-member key at
`ExchangeResidentTtl`), so:

- if the client redials within the TTL (the common case), the new resident's
  re-add refreshes the entry and peers never observed a change — no `del`
  event, no marker, no "device went offline" blip;
- if the client never returns, the TTL lapses and the entry converts to a
  disconnect marker anyway (correct — it really did leave).

The `RemoveNetworkPeer` residentId guard (`model/peer_model.go:565`) already
protects the inverse race (a replacement resident registered first ⇒ the old
cleanup's remove no-ops), so skipping is strictly safer, never staler than the
TTL. Thread the cause into the cleanup: `resident.Close(cause)` /
`drainCancel` vs a genuine transport loss.

### 3.3 Track B — make-before-break migration (removes the gap)

The disruption exists because eviction happens BEFORE the client has a
replacement. Invert it:

1. At drain start, before evicting anyone, send each affected client a control
   frame: "this resident is migrating; establish a new transport and redial by
   T" with per-client jitter across the drain window.
2. The client SDK's `RouteManager` already multiplexes transports — it
   establishes the replacement transport to a sibling group/host while the old
   one still carries traffic, then the platform nominates the new resident.
3. Only then does `Drain()` evict the old resident, whose traffic has already
   moved. Streams and contracts survive the handoff; there is no gap, no
   reconnect-invalidation, no peer marker, nothing to excuse.

`Drain()` becomes: broadcast-migrate → jittered redial window
(≤ some `DrainMigrateWindow`, replacing the blind per-resident 200ms walk) →
evict stragglers who did not migrate (these fall back to Track A excusal). The
`serviceTransitionTime` machinery (`transport.go:157,361`) — which already
delays announce stabilization by `2*DrainAllTimeout` after a transition — is
the natural place to hang the new-vs-old service coordination.

### 3.4 Track B — stagger and pace (reduces blast radius)

- **Stagger groups within a host.** The incident drained g1 and g4 of one host
  at once. Warpctl should drain one group per host at a time (or a bounded
  fraction), so local capacity persists and migrating clients can land on a
  sibling group on the SAME host (cheapest move).
- **Adaptive pacing + prompt exit.** Replace the fixed 200ms/resident with a
  time-budget over the live resident count, so a big block drains within a
  target window instead of `count * 200ms`. Ensure the connect process EXITS
  the instant `Drain()` returns, so `docker stop` returns immediately rather
  than idling toward its `-t 3600` ceiling (the observed 28-minute blind wait).

### 3.5 Observability

`Drain()` already logs remaining-resident count every 100 evictions
(`resident.go:1087`). Export as metrics + a monitor/SIGNALS.md entry:

- `urnetwork_drain_residents_remaining` (gauge, per service) — drain ETA
  without ssh-ing to find the `docker stop` child, which is what today's
  incident required.
- `urnetwork_drain_excuses_written_total` / `_consumed_total` — excuse markers
  minted vs redeemed; a large written-minus-consumed gap = clients not
  returning (real capacity loss, not a deploy artifact).
- `urnetwork_connection_new_total{excused="true|false"}` — organic vs
  drain-excused reconnects, so a deploy's reliability impact is visible as
  excused (benign) rather than showing up as a score dip.

## 4. Correctness / abuse review

| Concern | Answer |
|---|---|
| Provider self-excuses to hide flapping | Impossible: `drain_excuse_*` is written ONLY by the server drain/replace path; the client never sees or sets it. `GETDEL` = one use; TTL-bounded; residentId-scoped. |
| Excuse marker outlives the drain and excuses an organic reconnect | ≤ `DrainExcuseTtl` (~5min) and consumed-once. A genuine reconnect >5min later is not excused. Tune the TTL to the drain window. |
| Skipping RemoveNetworkPeer strands a peer that really left | No: the per-member key TTL (`ExchangeResidentTtl`) converts a non-returning entry to a marker; skipping is never staler than the TTL, and the residentId guard already handles the replacement race. |
| Make-before-break doubles a client's residents mid-migration | Bounded to the migrate window; the nomination replaces atomically (residentId guard). Concurrent-client limit counts the client once (same clientId). |
| Track B partial rollout | Track A is the backstop for every client that does NOT migrate (old SDKs, crashes, SIGKILL) — excusal runs regardless. |

## 5. Rollout

1. **Track A first** (server-only, no SDK change, immediate score relief):
   excuse markers in `Drain()` + replacement paths; `GETDEL` consume in the
   announce new-conn branch with `ConnectionExcusedNewCount`; skip
   `RemoveNetworkPeer` on drain teardown; the three metrics. Behind a settings
   flag (`ExchangeSettings`, no globals). Ship, watch
   `connection_new{excused}` split on the next deploy.
2. **Observability** ships with (1) so the next deploy is measured.
3. **Track B stagger + pacing** in warpctl (ops) — no code-path risk to the
   connect data plane; drain one group/host at a time, adaptive pace, prompt
   exit.
4. **Track B make-before-break** (server control frame + SDK RouteManager
   coordination) last — the largest change, gated so an old SDK falls back to
   Track A cleanly.

## 6. Decisions (RESOLVED — implemented 2026-07-19)

1. **`ConnectionExcusedNewCount` (new counter).** Chosen. It is recorded
   *instead of* `ConnectionNewCount`, so it never enters
   `client_reliability_valid` — **the validity SQL is unchanged (still 6-arg),
   no arity bump.** New pg column `connection_excused_new_count` (nullable
   default-0 ALTER, no rewrite) + redis counter index 8 (append-only; an older
   rollup drops the unknown index with a log, fail-safe). **Deploy the
   taskworker (rollup) with or before connect.**
2. **`DrainExcuseTtl = 5min`, fixed** (not derived). `DrainAllTimeout` is 60min
   in prod, so the doc's derive-and-clamp always hit the 10min cap; a plain 5min
   setting matches the intent and the consume lag (redial + announce + first
   sync ≈ 2–3min). A companion `drain_excuse_pc_<clientId>` marker
   (`DrainProvideChangeExcuseTtl = 2 blocks` past the excuse ttl) suppresses the
   mechanical provide re-announce for **every** announce of the client — needed
   because a make-before-break overlap has the old and new connections syncing
   concurrently, and either may carry the re-announce.
3. **Track B make-before-break: implemented now.** `ResidentMigrate` control
   frame (proto msg type 28) + SDK `deviceLocalProvider` make-before-break +
   server migrate broadcast in `Drain()`. Old SDKs ignore the unknown frame and
   fall back to Track A. The `StreamReset` is now a **reconcile** (relisted
   streams keep their sequences/p2p transports; unlisted cancel) so streams
   survive the handoff — split into two independently-compatible halves (client
   reset handler + resident reset-from-hops), each degrades to the old
   cancel-all behavior against an old counterpart.
4. **Admission gate + hard deadline added** (beyond the original doc): a
   draining exchange 503s new connections and declines nominations
   (`EnableDrainCoordination`), so evicted clients land on a sibling via the lb
   instead of bouncing; `Drain()` enforces `DrainAllTimeout` and paces the
   straggler sweep over `DrainStragglerSweepTimeout`. Without the gate the drain
   never converged (refill loop).
5. All new tunables live in `ExchangeSettings` (`EnableDrainExcuse`,
   `EnableDrainCoordination`, `DrainExcuseTtl`, `DrainMigrateWindow`,
   `DrainMigrateSendTimeout`, `DrainStragglerSweepTimeout`) — no globals. warpctl
   stagger is a host-wide flock gated by `WARPCTL_STAGGER_HOST_DRAIN` (default
   on).

## 7. Implementation inventory (files)

- `connect/resident.go`: `Exchange.Drain()` (`:1058`) writes excuse markers +
  drives migrate/stagger/pacing; resident cleanup (`:505-527`) skips
  `RemoveNetworkPeer` on drain cause; `resident.Close(cause)` threads the
  cause; `ExchangeSettings` (`:206`) gains the drain/excuse tunables.
- `connect/transport_announce.go`: new-conn branch (`:468`) `GETDEL`-consumes
  the excuse marker → `ConnectionExcusedNewCount` + suppress the one
  post-drain `ProvideChangedCount`.
- `model/network_client_reliability_model.go`: `ClientReliabilityStats` gains
  `ConnectionExcusedNewCount`; `client_reliability_valid` SQL treats it as
  non-invalidating (a migration bumps `client_reliability_valid` arity — new
  migration).
- `model/peer_model.go`: no change (the TTL + residentId guard already do the
  right thing when the drain path skips the remove).
- NEW excuse-marker helpers in `model/` (`SetDrainExcuse`, `TakeDrainExcuse`
  = GETDEL) keyed `drain_excuse_<clientId>`, TTL'd.
- `cli/connect/main.go`: unchanged (SIGTERM → Drain stays the entry).
- Track B: server migrate control frame (protocol) + SDK `RouteManager`
  make-before-break; warpctl stagger/pacing (ops repo).
- Monitor: `monitor/SIGNALS.md` drain section + the three metrics.
