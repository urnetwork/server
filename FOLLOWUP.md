# FOLLOWUP — 2026-07-15 redis/pg incident day

Deferred work from the 2026-07-15 incident response and resilience audits.
Everything here was deliberately NOT shipped in the incident-day deploy batch,
or is an ops action still pending. Items are ordered by priority within each
section. Context lives in the audit reports (session memory:
`redis-resilience-audit-2026-07`, `pg-idle-in-tx-redis-coupling`) and the
inline comments referenced below.

## 0. Housekeeping (do first)

- [ ] **Commit the tree.** Everything prod runs — incident fixes, key
  distribution, lease fixes, resilience batch, xops config — is uncommitted
  local edits across `server/`, `xops/`, and `warp/`. Multiple deploys have
  shipped from this uncommitted state.

## 1. Ops actions (no code, mostly one-liners)

- [ ] **Run the redis key cleanup** on the redis host if not yet completed:
  `REDISCLI_AUTH=<pw> ./redis-key-cleanup.sh --apply` (no `--fast-relief`).
  Drains the orphaned no-TTL piles (`{escrow}net_*`, `{connect}*`) and TTLs
  legacy `{pm_*` keys. Every OOM wall recurrence today traced to this being
  incomplete. Never run it during a slot rebalance.
- [ ] **`bringyourctl contracts reconcile-net-escrow`** after the cleanup (and
  once more after traffic stabilizes) to converge escrow counters.
- [ ] **Revert the temporary 48gb maxmemory ceilings** on 6398/6410 (and any
  other bumped node) once the cleanup drains them — the 12gb template value
  applies at next restart/ansible run; `CONFIG SET maxmemory 12gb` applies it
  live.
- [ ] **pg: set `idle_in_transaction_session_timeout = 5min`** in the main
  postgres conf (same workflow as the max_wal_size edit). Currently 0 —
  zombie transactions observed >5h old pinning the vacuum xmin horizon.
- [ ] **pg: terminate current idle-in-tx zombies** (>30 min old):
  `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE state = 'idle in transaction' AND xact_start < now() - interval '30 minutes';`
- [ ] **Apply the live redis CONFIG SET block** (require-full-coverage no,
  allow-reads-when-down yes, replica-validity-factor 0, link-sendbuf 1gb,
  maxmemory-clients 1gb, latency-monitor 100, slowlog 1024, timeout 300,
  oom-score-adj yes, lazyfree-user-flush yes) if not yet run — the template
  overrides block only covers restarts. pubsub COBL is already set fleet-wide.
- [ ] **Purge the 5 phantom cluster-node entries** (noaddr ghosts from the
  restarts): `CLUSTER FORGET <id>` per ghost on every live node (loop given in
  session; ghosts show as `:0@0`/`noaddr` in CLUSTER NODES).
- [ ] **Ansible run on redis-clusters** to install redis_exporter + the
  templated fluent-bit conf (32 per-node scrape inputs) and land the conf/unit
  changes (TimeoutStopSec=600, RequiresMountsFor, overrides block).
  First run downloads the exporter tarball from github.
- [ ] **Grafana:** `bringyourctl grafana load-defaults` (loads
  `redis-cluster.json`), rebuild+redeploy the warp grafana front (embeds the
  provisioned alert rules), then set up notification contact points/policies
  routing on `severity=page|warn` in the UI (rules fire into the default
  policy until then). Consider flipping RedisNodeDown to
  `noDataState: Alerting` once the scrape pipeline is proven.
- [ ] **Investigate AdvancePayment failures**: ~15 payments stuck at 28+
  consecutive `Payment create transaction error` retries (predates the redis
  incident window). Payouts affected. Untouched all day.

## 2. Code — first post-deploy round

- [ ] **Network peers pubsub — DISABLED 2026-07-15; first-principles redesign
  required before re-enable.** `EnableNetworkPeers=false` (default) in
  `ExchangeSettings` and `ConnectionAnnounceSettings` gates: announce-time
  registration, the resident heartbeat refresh/re-add (including proxy-peer
  counting), the teardown publish, and the per-resident
  `NetworkPeerListener`. `/network/peers` and SDK peer updates are dark;
  registry keys self-expire (24h meta ttl, 300s connected scores).

  **The incident record (why it melted — keep this):**
  - As built: one dedicated SSubscribe TCP connection PER CONNECTED
    TOP-LEVEL CLIENT (per-resident listener) + a 60s poll each running
    `GetNetworkPeers` (HGetAll + 2×ZRangeByScore + GET against one
    `{np_<networkId>}` slot); every device connect/disconnect published an
    event fanned to every subscribed device of that network.
  - Throughput accounting at fleet scale (~50k+ concurrent): ~50k extra
    pubsub conns (~1.6k/node); ~800-1500 poll pipelines/s onto single
    slots; publish volume = churn_rate × avg_network_device_count
    deliveries/s through the shard-owner node's event loop.
  - The lethal coupling: go-redis pubsub delivers into a 100-slot
    in-process channel with a 60s send timeout — when app consumers stall
    (any redis slowness), the reader goroutine blocks up to 60s per message
    and STOPS READING THE SOCKET, so the server buffers deliveries
    per-connection. Each conn stays under the 32mb pubsub COBL soft limit,
    but thousands of stalled conns × MBs = tens of GB on the shard-owner
    nodes (observed 41G and 49G RES) → maxmemory → OOM-rejected writes →
    more stalls and connection churn → more publishes. Self-amplifying, and
    it coupled to the DB incident: the same redis stalls held pg
    transactions open via the escrow-in-tx call (double whammy).
  - Fix attempts, in order, and why each was insufficient: (1) HandleError
    wrap at nomination — stopped connection kills, not volume; (2)
    settle-delay registration in resident — reinvented the announce's
    existing gate (reverted); (3) announce-time registration — right layer
    for add-event churn gating, but listener conns/polls and
    established-connection fanout remained; (4) kill switch (shipped).

  **Redesign directions (write the design doc off-incident, then pick):**
  1. Poll-only + epoch counter: no subscriptions; listeners poll
     `GET {np_}eid` (one cheap GET, jittered interval), full read only on
     change. Removes all subscriber conns and fanout. Simplest; likely
     right.
  2. Process-level fan-in: ONE subscription per pod, in-process dispatch to
     residents (conns collapse O(clients)→O(pods)); requires channel-naming
     redesign since shard channels are per-network.
  3. Off-redis event plane: carry peer events over the existing
     exchange/control-plane fabric; redis remains registry-of-record only.
  Client-side hardening regardless: pubsub chanSendTimeout 60s → ~1s (drop
  fast, never block the socket reader), larger channel, listener
  restart-with-backoff. Re-enable checklist: `maxmemory-clients` verified
  on all 32 nodes, redesign load-tested with a churn×fanout model, redis
  dashboard/alerts live, `EnforceConcurrentClients` counting reads kept off
  the nomination path (count at announce).

- [ ] **Escrow redis read out of the pg transaction** (the big one).
  `createTransferEscrowInTx` (`model/subscription_model.go:~960-1008`) queries
  `transfer_balance` and then does the netEscrow pipelined GET *inside* the
  caller's pg tx — pg connection dwell = redis latency × ~200 contracts/s;
  this is what turned redis brownouts into pg pool exhaustion (563 idle-in-tx
  observed vs 6 active). Design: read the active balance list (plain conn) and
  the netEscrow counters BEFORE opening the tx; pass the clamped balances into
  the tx which keeps only the SQL writes. Watch for: races between the
  pre-read and the tx (balances can change — same tolerance as today's
  fail-open read), the caller chain (`CreateTransferEscrow`,
  `CreateCompanionTransferEscrow`, `CreateContract`), and the escrow test
  suite (`TestEscrow`, companion checkpoint test ~77s, reconcile drift test).
- [ ] **Resident poll: distinguish redis error from "no resident".**
  `GetResidentForClient(WithInstance)` (`model/network_client_resident_model.go:50/82`)
  swallows redis errors and returns nil; `connect/resident.go:519+509` treats
  nil as "replaced" → `resident.Cancel()`. One wedged node holding `ncr_*`
  keys cancels ~1/32 of residents per 75s tick. Fix: error-distinct return +
  N-strike grace before cancel; same change de-flaps the reconnect loops at
  `resident.go:1635/:1818` (error currently reads as absent → spurious
  re-nomination churn).
- [ ] **Rate limiter fail-open decision** (needs explicit product/security
  sign-off): `connect/transport_rate_limit.go:136` runs pre-websocket-upgrade
  and fails CLOSED — a redis outage means zero new connections platform-wide.
  Proposal: fail open on redis *error* (allow + log + metric), keep closed on
  a genuine over-limit verdict; cap this call's retry window to ~1s.
- [ ] **FeedVerifyEgress must not tear down provider connections.**
  `connect/transport_announce.go:263/:271` pass `self.cancel` as the error
  handler while `model/verify_model.go` Raises on redis errors — every 2-min
  refresh during a brownout disconnects providers. Fix: self-wrap inside the
  model (pattern: `RecordClientReliabilityStatsRange`,
  `network_client_reliability_model.go:258`) and drop the cancel handler.
- [ ] **Listener restart-with-backoff.** `NetworkPeerListener`
  (`model/peer_model.go:957`, spawned `connect/resident.go:2121`) and
  `StreamHopListener` (`model/stream_model.go`, spawned `resident.go:2085`)
  die permanently if a redis panic escapes their run goroutine — the client
  stays connected but never receives peer/stream updates again. Wrap run in a
  restart loop with backoff + jitter; resync-on-restart already exists.
- [ ] **Retire the per-call PING in `server.Redis`** (`redis.go:248`) and
  enable `ContextTimeoutEnabled`. The keyless PING routes to a random node —
  during a single-node wedge ~1/32 of ALL redis calls fail at the gate
  regardless of key location, and it's +1 RTT on every call. Requires
  rethinking the retry loop (the PING is its connectivity probe); consider a
  background health prober + per-call typed-error retries instead.
- [ ] **Escrow mirror divergence hardening**: post-commit netEscrow
  IncrBy/DecrBy are fire-and-forget (`subscription_model.go:1115/2066/2085`,
  `releaseNetEscrowForContract:646` ignores Pipelined err) — lost decrements
  recreate "Insufficient balance" drift. Funnel failed escrow posts into a
  durable retry task. Also `GetActiveTransferBalances:315` silently skips the
  netEscrow clamp on redis error (over-reports available balance) — surface
  it.
- [ ] **Verify subsystem hot keys**: `verify_eligible`/`verify_reap`/
  `verify_stat_clients` are fixed-name single-slot keys carrying the whole
  /verify api; `SampleVerifyNextHop` (`verify_model.go:561`) does SCard + a
  sequential per-exclude SIsMember loop per request → use SMISMEMBER (one RT)
  now; sharding/local-cache of the eligible set is a design task.
- [ ] **Missing pg fallbacks**: `loadInitialClientLocations`
  (`network_client_location_model.go:1773`) errors on a lost redis key with no
  fallback (provider-locations api broken until the export task rewrites it);
  `GetStEpochSummaryCache` (`st_model.go:376`) Raises instead of recomputing
  from pg.
- [ ] **Attribute the remaining idle-in-tx shapes**: ~250 transactions idle
  right after `BEGIN` and ~158 after the `provide_key_change` COUNT
  (`network_client_model.go:1596`) — find the holders (suspect nested-tx /
  slow pre-first-query work) and apply the same read-before-tx treatment.
- [ ] Minor: `isRedisConnectionError` string-matching → typed error
  predicates (`net.Error`, `errors.Is`); `MigrateProvideMode` unbounded
  per-client goroutine fan-out (`network_client_model.go:1296`).

- [ ] **API spec drift backlog**: `TestSpecConformance` logs ~30 informational
  drift entries — impl fields absent from `connect/api/bringyour.yml`
  (wallet_auth.wallet_nonce, stats lookback args, proxy_config initial device
  state/location fields, error.upgrade_required, balance pro flag, device
  association fields, ...). Not failures (spec ⊆ impl holds), but the public
  spec lags the implementation. Sweep bringyour.yml against the drift log.
  (The one hard mismatch — spec-only `providers[].provide_mode`, never
  implemented — was removed from the spec 2026-07-15.)

## 3. Redis topology — second host + replicas (committed direction)

Groundwork already in the tree: `cluster-announce-ip {{ redis_lan_ip }}` in
the template + inventory var; playbook rescue path defanged (no more bare
add-node; rebalance opt-in via `WARP_REDIS_REBALANCE=1`, never seeds empty
masters); repl-backlog/replica-COBL/repl-timeout pre-tuned for sync storms.

Runbook:
1. Live-set `cluster-announce-ip 192.168.51.193` on all 32 nodes (safe now —
   gossip-propagated; the Go client's port-rewrite dialer ignores announced
   hosts). Verify CLUSTER NODES converges to LAN addresses.
2. Pick the donor host (edge-5 suggested), add to the `redis-clusters` group
   with its own `redis_lan_ip`, deploy with `WARP_SKIP_REDIS_INIT=1`.
3. Open 6380-6411 AND 16380-16411 (cluster bus) both ways on the LAN.
4. Join each new instance as a replica of a named master:
   `redis-cli --cluster add-node <edge5>:PORT 192.168.51.193:6380
   --cluster-slave --cluster-master-id <id>` — in batches of 4-8 to avoid 32
   concurrent sync forks on edge-6.
5. **Retire the port-rewrite dialer** in `server/redis.go:97-105` (with
   announce-ip set, stock go-redis routing works). This is the one code change
   the migration requires — until then, cross-host routing cannot work.
6. Optional hardening: `CLUSTER FAILOVER` half the replicas to split masters
   16/16 across hosts; run the same nginx seed LB on the second host and put
   both seeds in the client `Addrs`/DNS so bootstrap survives edge-6 loss.

Interim option (before the second host): restore 1 same-host replica per shard
— doesn't survive host loss but restores 30s automatic failover for the
wedge/crash class that caused every CLUSTERDOWN today.

## 4. Design/durability decisions (parked, need owners)

- **ckey_ posture accepted 2026-07-15**: redis is source of truth for client
  public keys, no pg backing; loss ⇒ re-nomination heals. Revisit only if
  replica coverage doesn't materialize.
- **`{account_balance_*}` payout accounting lives in redis with no TTL** on a
  cache-posture cluster (hourly RDB, no replicas yet): checkpoint-into-pg
  remains the standing FIXME — the only redis data whose loss is real money.
- **Matchmaking gate redesign (0.95-on-5min reliability gate)** — committed
  follow-up once production is stable (user 2026-07-15: "I want to fix this
  after we unbreak production").

  **The problem**: `FindProviders2` includes/excludes a provider on a single
  scalar — lookback_index=0 reliability weight >= 0.95 over a 5-minute block.
  One threshold is doing two jobs: detecting "this provider went bad" AND
  absorbing "our measurement went bad". Any small dip excludes the provider,
  whether the provider actually degraded or the measurement pipeline hiccuped
  (missed rollup drain, redis buffer loss during a brownout, late block).
  2026-07-15's redis instability would have dented reliability data
  fleet-wide all day — the gate can silently empty the provider pool during
  exactly the incidents when stability matters most (this is what happened in
  the 2026-07 reliability incident: most providers excluded).

  **Current mitigation and its limits**: degraded-block excusal via a
  60-block (~5h) LOCAL median (`reliabilityDegradedMedianBlockCount = 60`,
  `model/network_client_reliability_model.go` — see the KNOWN LIMITATION
  comment there). It excuses SYSTEM-WIDE degraded blocks by comparing block
  totals to the recent median. Documented failure modes at both ends: the
  60-block memory cannot excuse degradation lasting longer than its window,
  and the 24h-anchor variant (tried 2026-07-15, reverted same day) froze ALL
  scores when recovering traffic sat below 95% of the daily median. The
  median mechanism also cannot distinguish per-provider measurement loss
  from per-provider degradation — it only sees aggregate volume.

  **The redesign (two-signal event detector)**: separate the two questions
  with independent evidence rather than one threshold:
  - provider-local signal: did THIS provider's own observed
    traffic/verify/latency results degrade?
  - fleet/cohort signal: did everyone (or the provider's cohort: same
    country/ASN/block) dip together? Correlated dip = measurement event →
    excuse; isolated dip = provider event → exclude.
  Design considerations: hysteresis on the gate (exclusion requires N
  consecutive bad blocks, re-inclusion cheaper than exclusion); missing-data
  as an explicit third state (absent block != zero-weight block); gate
  behavior during declared measurement incidents (e.g. reliability rollup
  drain lag > threshold → freeze exclusions, keep inclusions); and an
  operator kill switch to force the gate open like today's
  `EnableNetworkPeers` lesson. Write the design doc first; validate against
  replayed data from the 2026-07 incident window (the frozen-scores and
  mass-exclusion episodes are both in `network_client_score` history).
- **/stats to the replica** once the replica DB is up (routes currently
  503'd).
- **`server.Subscribe` uses sharded pubsub** (SSubscribe): each channel pins
  to its slot's node — a wedged node stalls all subscribers of its channels.
  Acceptable with the resilience work; revisit if pubsub criticality grows.

## 5. Verification follow-ups

- [ ] After cleanup + deploy: confirm exports cycle clean (locations ~2/min,
  scores ~1/13min), contract rate at baseline, zero OOM/pool-timeout errors in
  connect logs, `urnetwork_redis_pool_timeouts_total` flat in grafana.
- [ ] After ansible + grafana deploy: redis-cluster dashboard populates for
  all 32 nodes (`node` label 6380-6411), alerts visible in the "urnetwork
  alerts" folder.
- [x] Pre-existing test failures — ALL FIXED 2026-07-15: `TestWalletLoginNonceSingleUse`
  + `TestWalletLoginNonceMustBindMessage` (rewritten against the structured
  wallet_auth_challenge flow; the legacy freetext-nonce format they signed no
  longer parses), `TestSampleVerifyNextHop` (D26 set the default
  EligibilityInterval to 0 = token bucket off; test now forces the bucket on
  to keep covering §5.3 burst accounting), `TestStream` (was an accidental
  32k-key/131k-contract load test that ground for 20+ min then failed
  fixed-1s event-propagation windows; rescaled to 1024 keys with
  poll-cycle-covering windows, passes -race in ~95s).
