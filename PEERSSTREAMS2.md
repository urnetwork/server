# PEERSSTREAMS2 — network peers + stream hops v3: redis key events + corrective poll

Successor to PEERS2.md (dirty-counter + poll, v2), which replaced the PEERS.md
pubsub delivery (v1) after the 2026-07-15 outage. The registry data model is
extended (peers only — see §5.2), but the protocol frames, API, and SDK
surface remain UNCHANGED: this is again a change to the change-notification
transport, plus the minimal schema change that makes deltas real.
Incident record: FOLLOWUP.md "Network peers pubsub". Monitoring:
monitor/SIGNALS.md.

## 1. Design statement

Writers keep writing keys the way they do today. Readers subscribe to the
key events redis already emits natively (`notify-keyspace-events`) to learn
about set modifications (add, remove, expire) as they happen, and full-read
only (a) once at sync, (b) on any subscription (re)establishment, and (c) on
a LONG corrective poll (5 minutes) that repairs dropped events via the
existing version counter. There is no application-managed publish channel —
the delta transport is the redis server itself announcing its own writes.

The v2 poll stays in the codebase as the corrective loop and as the
kill-switch fallback: events off → v2 behavior, exactly.

## 2. Why: the v2 steady-state read burden

v2 eliminated standing connections but pays for it in reads. Per resident
(N = connected top-level clients, T = poll interval):

- one counter GET per tick: N/T GETs/s fleet-wide, forever, churn or not.
- one unconditional insurance full read every `FullReadEvery` ticks:
  N/(10T) full 4-cmd pipeline reads/s of the ENTIRE per-network set —
  **every client re-pulls its complete peer set every ~10×T even when the
  registry is static**. At N=60k, T=15s: 4,000 GETs/s + 400 full set
  reads/s of steady-state noise, all moving full set payloads through node
  output buffers. This is the raw-throughput / memory-bandwidth burden this
  redesign removes.
- change-driven full reads on churn (correct, but each churn event costs
  pollers-per-network × full set reads even for a single-member change).

v3 target: steady state ≈ zero reads besides the 5-minute corrective poll;
churn cost ≈ one small delta read per affected reader; standing connections
O(processes), not O(clients).

## 3. Constraints (outage learnings re-answered, plus v2 learnings)

| Learning | v3 answer |
|---|---|
| O(clients) dedicated pubsub conns melted the cluster (v1) | subscriptions are per PROCESS per owning node, demuxed in-process to residents: O(pods × cluster nodes) standing conns fleet-wide (~10²), not O(clients) (~10⁴·⁵) |
| publish fanout = churn × network size through one shard node (v1) | no application publish at all; redis emits one keyevent per actual key write; subscriber count per node = process count, not resident count |
| stalled consumers → server output buffers → maxmemory (v1) | the shared subscriber is a dedicated drain goroutine that only flips in-process dirty flags — it never blocks on downstream work. If it still stalls, redis kills that ONE connection (client-output-buffer-limit pubsub), we reconnect and resync; bounded, self-healing |
| listener death was permanent (v1); per-resident poll goroutines can still die silently (observed 2026-07-17, fixed by per-tick containment) | the per-resident goroutine disappears entirely; the per-process subscriber is one well-supervised loop with reconnect + forced resync; the corrective poll (v2 loop, error-tolerant per tick) still runs per resident as the backstop |
| events are fire-and-forget; drops are invisible | the 5-minute corrective poll compares the version counter (`!=` semantics from v2 §3.2, unchanged) and full-reads on mismatch — dropped events are repaired within ≤ 5min + jitter |
| global fixed keys become single-slot hot spots | unchanged: everything stays under the per-network `{np_<networkId>}` / per-client `{<clientId>}` hash tags |
| feature had no kill switch (v1) | new flag `EnableKeyEventDelivery` (name TBD) gates events; OFF → pure v2 poll at v2 intervals. `EnableNetworkPeers` continues to gate the feature entirely |

## 4. Event source: redis keyspace notifications

- Config prerequisite: `notify-keyspace-events` enabled on every data node.
  Scope narrowly — `E` (keyevent) + `g` (generic: DEL/EXPIRE), `h` (hash),
  `x` (expired), and `z`/`s` only if the aggregate-key variants (§5.3) are
  used. Keyevent flavor (`__keyevent@<db>__:<event>` → payload = key name)
  is the useful one: the key name carries the identity.
- Cluster locality: events are emitted ONLY by the node holding the key's
  slot. The subscriber must connect to the owning node per slot range and
  follow topology: on any slot-map change or MOVED observation →
  resubscribe on the new owner and force a resync of the affected
  registrations (events during the gap are gone forever).
- Delivery is at-most-once to connected subscribers. Every gap class
  (reconnect, resharding, output-buffer kill, config drift) has the same
  answer: forced full resync on (re)subscribe + the corrective poll.
- `expired` events fire when redis NOTICES expiry (lazy + active cycle),
  which can lag the logical TTL by seconds (typically <10s under the
  default active-expiry cycle). Acceptable: disconnect visibility was
  already TTL-bounded; the corrective poll bounds the worst case.

## 5. Architecture

### 5.1 Reader: per-process shared subscriber + demux

One `keyEventSubscriber` per exchange process:

```
registry:  networkId -> set of in-process listeners (peers)
           clientId  -> set of in-process listeners (stream hops)
conns:     one SUBSCRIBE conn per cluster node that owns ≥1 watched slot
           (subscribe to the scoped __keyevent__ channels)
on event(key):
    parse hash tag -> registration key -> mark listeners dirty (non-blocking)
on conn (re)establish / topology change:
    mark ALL registrations routed through that node dirty with
    resync-required + jittered spread (avoid a thundering re-read)
```

Dirty listeners wake their own goroutine-free work: the resident's existing
delivery path performs the (delta or full) read and emits frames. The
subscriber NEVER reads registry data and NEVER blocks: it only routes key
names to dirty flags. Slow residents cost themselves staleness, nobody else.

Registration lifetime = resident lifetime (same gates as today:
`EnableNetworkPeers && peerNetworkId != nil && peerCategory == Client`).

### 5.2 Peers: per-member keys make events carry the delta

A set-key event names the KEY, not the member — on today's aggregate zsets
an event only says "network N changed", forcing a full read per event. To
get true deltas, each peer becomes its own key:

- `{np_<networkId>}p:<clientId>` — hash: profile fields (device name/spec,
  principal, roles, provide modes), `EXPIRE` = registration TTL (today's
  `ExchangeResidentTtl`), refreshed by the heartbeat (EXPIRE only — no
  event fires for a pure TTL refresh, which is exactly the v2 "refresh must
  not bump" rule for free).
- add / re-add / provide-mode change = `HSET` + `EXPIRE` → `hset` event →
  reader reads that ONE small key and emits an Update frame for that peer.
- clean disconnect = `DEL` → `del` event → reader emits the disconnect
  marker (DisconnectTime = now, window-tracked reader-side as today in the
  client PeerManager).
- silent death = key TTL expiry → **native `expired` event** — redis itself
  announces the disconnect. This subsumes the prune machinery's
  notification role (prune stays for the server-side marker/window state
  that late joiners and the API read).
- The index zset (connected members) and the disconnected zset REMAIN,
  maintained exactly as today — they serve the corrective poll, first-sync,
  the `/network/peers` API, and enforcement ZCards. The version counter
  (`{np_}eid`) REMAINS and keeps being bumped as in v2 — it is the
  corrective poll's cheap comparison. Writer pipelines gain one HSET+EXPIRE
  and one DEL per transition; all inside the existing per-network
  TxPipelines, same slot.

### 5.3 Stream hops: aggregate-key events, no schema change

Stream hop sets are per-client and tiny (a stream path is a few hops), so
the per-member restructure buys nothing: subscribe to events on the
existing hops set key (`z`/`s` class as applicable). Any event on
`{<clientId>}s2_c…` → dirty → full read of that client's hops (small) +
accumulator diff, exactly the v2 read path. Counter `{<clientId>}s2_c_eid`
and its 24h/8h TTL semantics unchanged; corrective poll interval also 5min
(from `StreamHopsPollInterval`).

### 5.4 Corrective poll (the v2 loop, retimed)

Per resident, unchanged v2 semantics with `!=` counter comparison and
per-tick error containment, at `PollInterval = 5min ± 20%` when events are
enabled (v2 default intervals when disabled). `FullReadEvery`'s
unconditional insurance read is REMOVED in event mode — the corrective poll
itself is the insurance; delivery still only happens on a version change or
an event-driven delta.

### 5.5 What gets deleted / kept

- Deleted (event mode): the 15s/5s per-resident counter polling cadence;
  the every-10th-tick unconditional full read; nothing else — v2 code paths
  stay as the corrective/fallback loop.
- Kept: version counters + bumps, index/disconnected sets, prune, the
  accumulator/diff + Reset/Update frame batching (50/frame), protocol
  MessageTypes 26/27, SDK untouched.

## 6. Load math

N = 60k clients, C = churn events/s, P ≤ 100 peers/network.

|  | v2 (15s poll) | v3 (events + 5min poll) |
|---|---|---|
| standing conns | 0 | O(pods × nodes) ≈ 10² fleet-wide |
| steady-state counter GETs | N/T = 4,000/s | 0 |
| steady-state full set reads | N/(10T) = 400/s | 0 |
| corrective full reads | — (folded above) | N/300s = 200/s fleet-wide, jittered (halves v2's insurance alone; tunable upward) |
| churn cost | C × P full set reads | C × (readers of that network) single-key HGETALLs (peers) / small set reads (hops) |
| event emission cost | — | one pubsub publish per actual key write on the owning node, fanout = subscribed processes (~pods), not residents |
| memory bandwidth | full set payloads × every client × every ~10T | delta-sized payloads on churn; full sets only on 5min ticks and resyncs |

Optional (enabled by the demux registry, decide at rollout): share ONE
accumulator per (process × network) across co-located residents so the
corrective full read and delta reads are deduped per pod. v2 skipped
per-pod dedupe; the shared subscriber makes it nearly free. Not required
for the throughput win.

## 7. Freshness semantics

- Connect / provide-change / clean disconnect visibility: event latency
  (≈ms) + reader read — effectively live, better than v2's ≤T.
- Silent death: TTL + active-expiry notice lag (seconds) — better than v2
  (which also waited for a poll tick after prune).
- Dropped/missed events: repaired ≤ 5min + jitter (corrective poll), or
  immediately on any resubscribe resync.
- `/network/peers` API remains a live read. SDK's 1s local epoch watcher
  unchanged.

## 8. Failure-mode review

| Failure | v3 behavior |
|---|---|
| `notify-keyspace-events` off / wrong classes on a node (config drift) | silent no-events from that node; corrective poll bounds staleness to 5min; monitor signal: per-node keyevent rate vs write rate (new, monitor/SIGNALS.md) |
| subscriber conn killed (output-buffer limit, node restart) | reconnect + forced resync (jittered) of registrations routed via that node; events in the gap repaired by the resync |
| resharding / slot migration | topology watch → resubscribe on new owner → resync affected registrations; MOVED on the corrective poll also triggers it |
| redis node down | events stop AND polls error-backoff for that slot's networks → stale lists only; no goroutine death; auto-recovery |
| event storm (mass reconnect after an outage) | fanout is per-process; dirty flags coalesce (a listener already dirty absorbs further events); reads are per-reader jittered |
| counter flushed / evicted | unchanged v2 `!=` resync semantics on the corrective poll |
| subscriber process bug marks nothing dirty | corrective poll is a full independent path — v2 behavior at 5min cadence |
| kill switch | `EnableKeyEventDelivery=false` → pure v2 at v2 intervals, no schema rollback needed (per-member keys are additive) |

## 9. Rollout plan

1. Ops prerequisite: enable `notify-keyspace-events` (scoped classes) on
   all data nodes; verify with a canary SUBSCRIBE before any code ships.
   Measure baseline event volume (it fires for ALL matching writes on the
   node, not just ours — scoping classes narrowly matters).
2. Writer schema change ships first, dark: per-member peer keys written
   alongside the existing sets (additive, flag-independent). Verify key
   population + TTL behavior in prod before any reader depends on them.
3. Reader ships behind `EnableKeyEventDelivery` (default false → v2).
   Canary one connect block: watch standing conn count (flat ≈ +nodes),
   per-node keyevent publish rate, corrective-poll mismatch rate (should be
   ~0 — every mismatch is a dropped event to explain), staleness signals,
   and the v1-outage signals (used_memory_clients flat).
4. Fleet enable; raise corrective interval only after mismatch rate proves
   clean. Stream hops converts in the same release (same subscriber, §5.3).
5. Cleanup release: remove v2's `FullReadEvery` insurance path in event
   mode (keep the loop).

## 10. Decisions (RESOLVED 2026-07-17, Brien)

1. **Keyspace flavor + PSUBSCRIBE patterns: YES** — `K` (per-key channels),
   patterns scoped to our key prefixes so filtering happens server-side.
   Config: `notify-keyspace-events "Kghxz"`.
2. **Redis CPU increase: accepted**, with the standing requirement that
   events are NEVER delivered to any client that did not explicitly
   subscribe. This holds at both layers: redis pub/sub only delivers to
   connections that subscribed matching patterns (the config flag only
   enables event GENERATION), and the connect protocol only emits peer/hop
   frames to residents that registered a listener (the existing
   client-category gate) — no broadcast paths anywhere.
3. **Per-member peer keys (§5.2): YES** — ship the writer dark first.
4. **Subscriber placement: the shared subscriber LIVES IN THE EXCHANGE** —
   one `keyEventSubscriber` owned per `Exchange` instance
   (`connect/resident.go`); residents register through the exchange. Not a
   package-level singleton. Per-pod shared accumulators: deferred (the
   registry is structured to allow it later).
5. **Settings: ALL settings live in a settings object** (nested under
   `ExchangeSettings`, e.g. `KeyEventDeliverySettings`) — no global
   constants, no package-level tunables. Flag `EnableKeyEventDelivery`
   defaults false (pure v2). Corrective poll intervals 5m±20% when events
   are on. Ops shape: a new dated last-wins section in `redis.conf.j2` +
   `redis-set-notify-keyspace-events.sh` mirroring
   `redis-set-pubsub-cobl.sh` (live CONFIG SET now, template keeps
   restarts consistent; pubsub COBL already 512mb/128mb/60).

## 11. Implementation inventory — STATUS: IMPLEMENTED 2026-07-17

All items are in place. **Flag default ON since 2026-07-18** — deploy
ordering: `notify-keyspace-events "Kg$sx"` must be live on every data node
BEFORE this build reaches the fleet (with generation off, delivery silently
degrades to the minutes-scale corrective poll). The shared test env enables
notifications on the test redis; the poll-mode e2e pins the flag off to keep
covering v2.
Tests: member-key lifecycle + listener delta/resync units
(`model/peer_model_keyevent_test.go`), events-only e2e
(`TestExchangePeerDiscoveryKeyEvents`, corrective poll pushed to 10m so every
delivery must ride the notifications), drop-repair e2e
(`TestExchangePeerDiscoveryKeyEventDropRepair`, generation silenced via
CONFIG SET -> corrective poll repairs -> re-enable restores live delivery),
poll-mode e2e unchanged. Signals: monitor/SIGNALS.md §9.
NOT unit-tested: cluster resharding resync — the test env runs a single node;
the path (topology watch -> done -> resubscribe -> resyncAll) is the same one
conn-loss takes, and the canary watches
`urnetwork_key_event_resubscribes_total`. Revisit if a multi-node test env
lands.

- `model/peer_model.go`: per-member key writes in Add/Remove/Update/prune
  pipelines (additive); reader delta path (single-key HGETALL → one-peer
  Update); corrective poll = existing NetworkPeerListener loop with new
  interval; keep counter bumps.
- `model/stream_model.go` (hops): no schema change; dirty-driven read path.
- NEW `server/key_event_subscriber.go` (or `connect/`): per-process
  subscriber, node-conn management, topology watch, registration/demux
  API, resync marking. Owns NO business logic.
- `connect/resident.go`: listener registration moves from
  spawn-poll-goroutine to subscribe+corrective-poll; settings.
- Monitoring: per-node keyevent rate, corrective mismatch counter,
  subscriber reconnect/resync counters (monitor/SIGNALS.md).
- Tests: event-driven delivery (add/remove/expire → frames) with real
  keyspace notifications against the test redis; drop-repair test (kill
  subscriber conn, assert 5min-poll repair with short interval); resharding
  resync unit (registry-level); existing peer_discovery e2e must pass in
  BOTH modes (flag on/off).
