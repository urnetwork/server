# PEERS2 — network peers v2: dirty-counter + poll architecture

Replaces the pubsub event delivery of PEERS.md (v1) after the 2026-07-15
outage. The registry data model, protocol frames, API, and SDK surface are
UNCHANGED — this redesign replaces only the change-notification transport.
Incident record and first-principles throughput accounting: FOLLOWUP.md
"Network peers pubsub". Monitoring signals referenced here: monitor/SIGNALS.md.

## 1. Design statement

Per network, writers bump a dirty counter on every visible change. Every
reader polls the counter at its own rate and performs a full read + local
diff only when the counter moved. No subscriptions, no delivery, no
accumulated state anywhere: the publisher and reader are fully rate-decoupled.

Key discovery that shrinks the diff: v1 already maintains this counter —
`publishNetworkPeerEvent` INCRs `{np_<networkId>}eid` before SPublish, and
readers already know how to full-read + resync (`NetworkPeerListener` does it
on subscribe and on event-id gaps). v2 = keep the INCR, delete the SPublish
and all subscription machinery, poll the counter.

## 2. Constraints from the outage (what v2 must never do)

| Learning (FOLLOWUP) | v2 answer |
|---|---|
| O(clients) dedicated pubsub conns melted the cluster | zero standing connections; each poll borrows a pool conn for ~1ms |
| publish fanout = churn × network size deliveries through one shard node | no delivery at all; change cost is one INCR; read cost is demand-driven |
| stalled consumers stopped reading sockets → server output buffers → maxmemory | nothing is pushed, so a slow reader costs the server nothing |
| listener death was permanent (panic in run goroutine) | poll loop is error-tolerant by construction: any error → log, backoff, next tick; never cancels the resident, never exits |
| registration on the nomination hot path hung connects | unchanged from the fix: registration at announce (flag-gated), heartbeat maintains, teardown removes (residentId-guarded, no-op if never registered) |
| global fixed keys become single-slot hot spots (standing rule) | the counter is per-network (`{np_<networkId>}eid`) — distributed by design |
| feature had no kill switch | `EnableNetworkPeers` stays, gating v2 exactly as it gates v1 today |

## 3. Architecture

### 3.1 Writer side (delta from v1: -SPublish, +prune bump)

All mutations already flow through peer_model.go with per-network TxPipelines.
- `AddNetworkPeer` / re-add: keep the eid INCR (drop SPublish). Heartbeat
  REFRESH (ttl extension, no membership/metadata change) must NOT bump —
  v1 already distinguishes refresh (no publish) from re-add (publish).
- `RemoveNetworkPeer`: keep residentId-guarded no-op semantics; bump on
  actual removal only.
- `pruneNetworkPeers` (expired → disconnect markers): BUMP when it prunes
  ≥ 1 entry (v1 published Removed markers here; v2 bumps instead). Prune runs
  from the surviving residents' heartbeats and from reads — a network with
  one surviving device still observes its dead peers (its own heartbeat
  prunes them); a network with zero residents has no pollers to notify and
  API reads compute expiry live, so nothing is missed.
- `AddNetworkProxyPeer`: proxy clients are invisible in the peer list — no
  bump (DECIDED 2026-07-15). **Standing requirement: if proxy clients ever
  become reader-visible through peer_model (counts in the peers response,
  a proxy list, or EnforceConcurrentClients surfacing counts to clients),
  the proxy add/remove/prune paths MUST start bumping the version counter
  at the same time — a visible-but-unbumped surface silently breaks the
  poll contract.**
- Counter key hygiene: `Expire {np_}eid networkPeerKeyTtl` alongside every
  bump (already the family pattern, 24h refresh-on-touch).

### 3.2 Reader side (replaces NetworkPeerListener's transport)

Per resident with `peerNetworkId != nil && peerCategory == Client`, one poll
loop (goroutine) owned by the resident lifetime:

```
lastVersion := -1   // unknown → first tick always full-reads (reset sync)
every tick (PollInterval + jitter ±20%):
    v, err := GetNetworkPeerEventId(networkId)     // 1 GET on the network slot
    if err: log V(1), backoff (2x up to 4×PollInterval), continue
    if v == lastVersion && tick % FullReadEvery != 0: continue
    eventId, peers, err := GetNetworkPeers(networkId)   // existing 4-cmd pipeline
    if err: log V(1), backoff, continue           // stale list, nothing worse
    diff vs local accumulator → emit NetworkPeersReset/Update control frames
    lastVersion = eventId
```
- `!=` comparison, not `>`: a flushed/expired counter restarts at 1; any
  mismatch (including regression) triggers a full read. Missing key reads as
  0 — also a mismatch. Self-healing against every redis data-loss mode.
- `FullReadEvery` (default: every 10th tick) is a cheap hygiene re-read of
  the snapshot; it still delivers ONLY on a version change (same `!=` guard as
  the normal poll), so a static registry never re-delivers — the "no change →
  no event" contract holds (clients react to peer events, and a periodic
  redundant Reset would be wasted wire traffic + spurious client churn). The
  `!=` comparison catches every realistic flush/expiry/eviction: a
  flush-and-rebuild is NOT atomic in production (heartbeats repopulate over
  seconds), so a poll observes the intermediate empty state (missing counter
  reads as 0, below the synced value) and the version diverges. (An earlier
  design force-delivered on the insurance tick to also cover a counter that
  resets to the *exact* value already synced between two polls — but that
  requires a sub-poll-interval atomic flush+rebuild that does not occur in
  production, and the cost was a redundant Reset per client every
  FullReadEvery ticks. Reverted; the flush tests now use the realistic
  non-atomic timing.)
- The accumulator/diff and the frame batching (Reset + Update, 50 peers per
  frame) are v1 code reused as-is; only the event source changes from
  pubsub messages to poll diffs. Client protocol (MessageType 26/27) and SDK
  are untouched.
- Exit condition: resident Done only. No error exits.

### 3.3 What gets deleted

- `server.Subscribe` usage in the peer path; `NetworkPeerListener`'s
  SSubscribe/channel plumbing (the struct shell can remain as the poll loop
  host); `publishNetworkPeerEvent`'s SPublish (function becomes bumpVersion).
- The pubsub shard channel `{np_}events` ceases to exist. Nothing else reads it.

### 3.4 Enforcement hook (from the outage direction)

Over-limit / concurrent-client checks read the connected zset AT ANNOUNCE
(kill the connection there), never at nomination. `EnforceConcurrentClients`
rollout must use this hook. (The announce already owns a cancel; one ZCard.)

## 4. Load math (the check v1 never got)

Let N = connected top-level clients with residents, T = poll interval.

- Version polls: N/T GETs/s, spread across per-network slots.
  N=60k, T=15s → 4,000 GETs/s fleet-wide ≈ 125/s/node — noise (nodes serve
  50k+ ops/s; each GET is O(1) on a tiny key).
- Full reads: only on change + the 1/10 insurance tick.
  Change-driven: churn of C networks/s × pollers-per-network P (devices of
  that network) full reads within one interval — bounded by C×P, and P ≤ 100
  (LimitTopLevelClientIdsPerNetwork). Worst observed churn storms (~100s of
  connects/s) → low thousands of 4-cmd pipelines/s fleet-wide, demand-driven
  and spread across slots. Insurance ticks: N/(10T) ≈ 400/s.
- Connections: zero standing. Pool floors already sized (min 8/node).
- Memory: zero accumulated state server-side; reader state = one int + the
  local accumulator per resident (already existed in v1).

Compare v1 failure math (FOLLOWUP): 50k standing conns + churn×size
deliveries + tens of GB of output buffers. v2's worst case under total redis
failure: peers lists go stale. That is the entire blast radius.

## 5. Freshness semantics

- Worst-case staleness for connect/disconnect visibility ≈ T + jitter
  (+ ExchangeResidentTtl for silent deaths, unchanged from v1 — expiry-based
  disconnect detection was already ~300s worst case per the PEERS.md gotcha).
- Default T = 15s (setting `NetworkPeersPollInterval`). The peers list is a
  UI surface; v1's instant delivery was luxury, not requirement. `/network/peers`
  API remains a live read (fresh on demand, e.g. pull-to-refresh).
- SDK unchanged: its 1s local epoch watcher runs against DeviceLocal state.

## 6. Rollout plan (this release)

1. Implement behind the existing `EnableNetworkPeers` flag (both settings
   structs). Default stays false.
2. Tests: rewire peer lifecycle tests (registration announce-driven + poll
   delivery — the v1 tests asserting pubsub delivery become poll-tick
   assertions with a short PollInterval); keep TestNetworkPeerEventGapReset
   semantics as counter-mismatch resync; churn test asserting NO redis
   connection growth (connected_clients flat) and bounded full-read counts.
3. Canary: enable on one connect block; watch monitor/SIGNALS.md signals — per-node
   connected_clients (flat), used_memory_clients (flat), cmd rate (+small),
   pubsub-drop log class (zero by construction), contract rate (unaffected).
4. Fleet enable. Re-enable checklist from FOLLOWUP applies (maxmemory-clients
   verified, dashboard live).
5. **StreamHopListener converts in the SAME release** (DECIDED 2026-07-15):
   identical pattern, identical reuse — the counter already exists
   (`{<clientId>}s2_c_eid`, INCR'd by AddToStream/RemoveFromStream, TTL'd 24h
   refresh-on-touch as of 2026-07-15), `GetStreamEventId` is the poll read,
   `GetStreamHops` is the full read with the accumulator diff, and the
   SPublish in the add/remove pipelines + the SSubscribe transport get
   deleted. The `!=` mismatch semantics absorb the counter's 24h idle expiry
   (expired counter → 0 → mismatch → resync from the hops set, which is
   itself 8h-TTL'd — an idle client resyncs to empty, correctly).

## 7. Failure-mode review

| Failure | v2 behavior |
|---|---|
| redis node wedge/down holding a network's slot | that network's polls error → backoff → stale list; no goroutine death, no connection impact |
| counter flushed / OOM-evicted (it is TTL'd, evictable) | version mismatch → full read → resync; if the whole registry flushed, heartbeats repopulate within ExchangeResidentTtl |
| CLUSTERDOWN window | all polls error-backoff; recovery is automatic on next tick; nothing accumulated, nothing to drain |
| reader stuck (slow client, blocked goroutine) | its own list goes stale; zero server-side cost (the property that saves us) |
| writer bump lost (code bug) | 1/10 insurance full-read bounds staleness to 10×T |
| thundering herd after recovery | jitter ±20% + change-driven reads spread naturally; no reset storm because each reader resyncs from its own poll, not a shared event |

## 8. Decisions (2026-07-15) + remaining open question

1. **Proxy bumps: NO** — with the standing requirement in §3.1 (any future
   reader-visible proxy surface must add the bump).
2. **Per-pod poll dedupe: SKIP** for v1 (revisit only at ~10× fleet growth).
3. **StreamHopListener: SAME RELEASE** — see §6.5.
4. **REMAINING — poll interval defaults** (runtime-tunable settings, so a
   low-stakes call): proposed `NetworkPeersPollInterval = 15s` (UI-surface
   staleness) and `StreamHopsPollInterval = 5s` (stream lifecycle is more
   operational; 60k clients / 5s ≈ 375 GETs/s/node, still noise). Both
   ±20% jitter. Confirm or adjust; changing later is a settings edit.

## 9. Implementation inventory (files)

- `model/peer_model.go`: publishNetworkPeerEvent → bumpNetworkPeerVersion
  (INCR + Expire, no SPublish); prune bump; NetworkPeerListener → poll loop
  (keep accumulator/diff/reset internals); delete subscribe plumbing.
- `connect/resident.go`: listener construction unchanged apart from settings
  (poll interval); heartbeat/teardown already correct from the outage fixes.
- `connect/transport_announce.go`: registration already in place (flag-gated);
  add the over-limit ZCard + cancel hook when EnforceConcurrentClients lands.
- `connect/resident.go` ExchangeSettings + `transport_announce.go` settings:
  `NetworkPeersPollInterval`, `NetworkPeersFullReadEvery`.
- Tests: `model/peer_model_test.go`, `server/connect/peer_discovery_test.go`
  (integration), churn/no-connection-growth test (new).
