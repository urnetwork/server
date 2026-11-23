# CPUOPT7-ORIGIN-DELIVERY: contract-origin edges only to their waiters (design 2026-11-23, approved and built)

CPUOPT7 §10.5 ranked this item second (X1). On 2026-11-23 at about 14:15Z the owner chose "Top 5 minus #2": items 1
and 3–5 go ahead, and this one waits for a design review. This document is that design.

The design (§1–§5) was written before anything was built, and nothing in production was changed to write it; §Status
below records the owner's choices and what was built. The design's code is server main at f09d3edd6. The
measurements are Mimir reads from 14:20–14:40Z on 11-23: 10-minute and 24-hour rates, and 7-day increases for the
failure counters (Appendix).

**Owner rule (binding, as in CPUOPT4–8):** "We can't sacrifice correctness of performance timings for the cpu
savings." The companion-origin wait keeps its bounds (100 ms first retry, 500 ms fallback, 3 s deadline) and its event
wake. The refusal ring keeps its fence. §4.2 argues both.

## Status (11-23, built)

The owner approved X1 "as designed" on 11-23 at about 14:55Z: option E, and the proposed answer to every question of §5.
It was built in four steps, each on its own branch cut from server main:

| Step | Branch | Merge on main | State |
|---|---|---|---|
| §1.7, the watch outlives the record | `fix/cpuopt7-x1-watch-until-recorded-1123` | 2a3984171 | merged; ships with the next deploy |
| Stage 0, the edge counters | `perf/cpuopt7-x1-stage0-counters-1123` | 5c4091fe8 | merged; ships with the next deploy |
| Build A, both paths | `perf/cpuopt7-x1-build-a-1123` | 878cf20b8 | merged; needs its own roll (§4.4) |
| Build B, the pair channels only | `perf/cpuopt7-x1-build-b-1123` | not merged (tip e5e8d744f) | waits for its gate (below) |

**The owner's choices (§5), as recorded:**
1. **E**, a channel per pair, subscribed after a miss.
2. **The Stream-fallback miss stays exact,** at one extra read: an unfenced miss subscribes, waits up to 100 ms for the
   acknowledgement, and re-reads once; only that fenced read stores the refusal. Without the acknowledgement in time the
   refusal is answered and not stored.
3. **Non-terminal destination refusals subscribe at `put`.**
4. **The cap is 8,192 held pairs per owner.** Over it a missing-origin refusal is answered and not stored. The doc
   proposed nothing for the other holders; the build follows its bounded-state reasoning: a destination refusal that would
   need a hold over the cap is not stored either (it is re-read, as before the ring existed), and a wait that cannot hold
   polls, as a declined watch does. `pair_cap` counts each.
5. **Two builds, no flag.**
6. **Stage 0's counters ride the next routine deploy. B1 (idle buckets close) ships inside Build A.**
7. **§1.7 is fixed now, on its own.**
8. **The `registration` wake label is added**, and the same source on `urnetwork_connect_companion_origin_lookups_total`.

**Where the build refines §4.1:**
- **A fenced read holds its pair.** `Update` takes a hold when it reports a fenced read, so the pair stays subscribed from
  the arm until the watch closes, after the refusal has taken its own hold. Without it, another holder could release the
  pair between a Stream fallback's fenced read and its `put`, and an edge in that gap would be lost: §4.2's case 4, which
  covered only the wait's own hold, now covers every fenced read.
- **The fenced-only rule for missing-origin refusals is the controller's.** `CreateContract` is the only production
  caller; it records a missing-origin verdict only from a fenced read and counts any other as `unstored`.
  `RecordRouteRejection` keeps its signature, and every ring refusal holds its pair whoever stores it.
- **A pair's missing hint ends with its last watch.** A pair kept by holds (a refusal, an unsubscribe not yet answered)
  outlives its watches now, so the last watch's close invalidates the hint: the next watch starts afresh, as before.
- **An unrequested acknowledgement of the bucket channel on the live socket replaces the socket.** What its pair channels
  carry is then unknown; a fresh socket resubscribes every held pair and each acknowledgement resyncs its pair.
- **One manager goroutine per bucket** opens the socket, writes the subscriptions, keeps the acknowledgement deadlines and
  the idle timer, and runs the held refusals' expiry pass (at most once a second per owner). A reader goroutine runs only
  while the bucket's socket is open. An idle owner keeps the 18 goroutines it had (16 bucket goroutines, 2 publishers).
- **Settings.** `MaxHeldPairs` 8,192, `RegistrationRecheckTimeout` 100 ms, `IdleBucketTimeout` 60 s,
  `HeldPairExpiryInterval` 1 s, `SubscribeBatchSize` 256. The sockets' detection bounds (2 s acknowledgement, 2 s
  incoming, 1 s receive, 1 s reconnect) and the 1 s publish timeout moved from constants into
  `ContractOriginNotificationSettings`, unchanged (CODESTYLE: tunables live in settings). The wait's own bounds (100 ms,
  500 ms, 3 s) stay the controller's named constants: they are the owner's fixed rule, not tunables.
- **`registration` replaces a wake, it adds none.** A continued wait's first wake is now `registration` instead of
  `fallback` (both run the read at 100 ms), so per request `fallback` falls by about the `registration` rate and
  `fallback + registration` is unchanged. A Stream-fallback re-check counts one `registration` wake and lookup.
- **A slow acknowledgement** (after the 100 ms read) shifts the 500 ms polls by up to 100 ms, as an event wake does
  today. While acknowledgements take under 100 ms, the reads are exactly today's (test 3 pins it for every ordering).

**Tests (§4.3).** All are deterministic: `testing/synctest` and barriers, with an in-memory shard broker
(`internal/redisshardtest`) that answers subscribes with Redis 8's notices and counts. Root cause: test 2 failed before the
§1.7 fix (the stale refusal was stored) and passes after it. Test 1 checks what today's code cannot do: owners that
waited once in a bucket receive nothing after it idles, and the holder receives only its pair's edges on the pair
channel; with the idle close or the per-pair delivery removed it fails. Each safeguard (the per-pair resync, the idle
close, the count check, the release of refusal holds, the fenced read's hold, the overdue acknowledgement) was removed
in turn, and a test failed each time. Test 14 ran against a disposable three-node Redis 8.10.3 cluster inside a
container with no network, so nothing reached a shared or production Redis.
BenchmarkContractOriginDelivery, 40 owners on one broker: an edge on the bucket channel costs 31.6 µs of wall time for
its 40 deliveries, one on a held pair's channel 0.73 µs for its one.

**Build A in a mixed fleet, and its rollback.** Every A publisher sends the bucket channel and the pair channel in one
pipeline to one node, and every A owner keeps the bucket channel subscribed while any watch or held pair is in the
bucket (B1 closes only idle buckets). So:
- a pre-A owner receives every A publisher's bucket-channel edge, as today;
- an A owner receives every pre-A publisher's edge on its bucket channel, and its fenced reads stay fenced by it, as
  today's are: the pair is watched or held, so the edge bumps its version and removes its refusals; its pair channel
  simply hears nothing from that publisher;
- A with A carries both, and the shadow counters compare them.
Nothing persists outside a process: no keys, no TTLs; a subscription ends with its socket. Rolling back from A to pre-A
is therefore one more mixed fleet of the same two kinds, safe at any point while no B process runs. Pinned by
TestContractOriginMixedVersions and the publisher kinds of TestContractOriginSubscribeAfterMissOrderings.

**Every process that runs origin delivery or the origin wait** (checked 11-23):
- **api**: one owner per process (api/run.go), bound to `/connect/control`; publishes and waits.
- **alt**: mounts the api router, so the same owner and paths.
- **proxy**: one owner per device manager, shared with its hosted local authorities (localclient); publishes and waits.
- **connect**: one owner per exchange (connect/resident.go), for the in-band resident controller, which runs
  `CreateContract`; it can publish and wait.
- **taskworker**: one owner per provider-egress pass; publishes and waits.
- **mcp**: none. It builds no owner and creates no contract.

**Build B may merge only when all of these hold:**
1. Build A (878cf20b8 or later) has run for at least 24 hours on every api, alt, proxy, connect and taskworker process, and
   no process of those services runs a pre-A build anywhere: canaries, hotfix images and the builder's own deploys
   included.
2. Over those 24 hours `v1_for_acknowledged_pair` equals `v2_received` (summed per service), `subscribe_failed` and
   `pair_cap` are near 0, and the §4.4 wait, outcome and timing signals are flat against the days before Build A.
3. The deploy version guard states that once a B build runs, no pre-A build of api, alt, proxy, connect or taskworker
   may be deployed (rollback goes to A only).
Connect's leg of Build A is a connect roll in its own lane, and connect goes last in A's order (§4.4).

**Expected savings, per step** (24-hour means of §1.6; warm means every proxy bucket started):
- §1.7 and Stage 0: none; correctness and counters.
- Build A: the proxy's deliveries go, because its buckets close when idle: proxy 0.15–0.34 cores (0.31–0.69 warm), Redis
  0.35–0.56 (0.71–1.12 warm), less the second SPUBLISH, about +0.1–0.15 Redis cores: **about 0.4–0.8 cores, up to 1.6
  warm.** The api still receives every bucket-channel edge.
- Build B: the api's deliveries go and the second SPUBLISH with them: api 1.16–1.95 cores, Redis 0.68–1.07, plus
  0.1–0.15: **about 1.9–3.2 cores more.** A and B together: 2.3–3.9 cores (2.5–4.3 warm), less under 0.05 of new work.

**What each deploy needs:**
- **§1.7 and Stage 0:** nothing of their own; they ride whichever routine deploy comes next, in any order, because
  nothing on the wire changes. Check `matched_waiter`, `matched_refusal` and `unmatched` appear, with `unmatched` at
  99.9% or more of `received` (else revisit before Build A rolls), and the refusal stored and hit rates flat.
- **Build A:** a roll of api, then alt if it runs, then proxy, then taskworker, then connect (server-0c's lane), under the
  usual gates. The order is for observation, not correctness: any order is safe, connect first included (above). The
  gates: the validator-idle gate, and the proxy's canary block and 30-minute hold. No migration. Watch, canary
  against control: the §4.4 wait signals per request (`event`, `deadline`, and `fallback + registration`); the companion
  origin outcomes and the `companion_origin` stage seconds per completion; the refusal stored and hit rates;
  `urnetwork_redis_shard_subscriptions_total` and the owner's `reconnected` and `subscription_failed` (no rise);
  `subscribed_pair`, `pair_acknowledged`, `pair_released` (about 50 holds a second fleet-wide), `subscribe_failed` and
  `pair_cap` (near 0), `urnetwork_contract_origin_held_pairs` (about 150 per api process); the proxy's `received` falling
  to its own waits' and `bucket_closed` rising; Redis `spublish` doubled, `ssubscribe`/`sunsubscribe` at about 100–150 a
  second, `redis_pubsubshard_channels` at the anchors plus the held pairs. Then the 24-hour soak; read the shadow pair
  only once every publisher runs A.
- **Build B:** only after its gate. Proxy first; one api block for at least 2 hours against the other blocks on the same
  hosts; the rest of the api and alt; taskworker; connect. The §4.4 table's signals; stop and roll back to A (never
  further) on any of its stop conditions.

## Summary

- **Today.** Every committed contract publishes its (source, destination) pair as an origin edge on one of 16 sharded
  channels. A process subscribes to a bucket the first time it waits in it and never leaves, so each edge reaches 30–40
  processes.
  - **Fan-out:** 1.65k publishes a second became 50.0k deliveries a second over the last 24 hours, and 65.4k a second
    at 14:30Z (api 34.6k, proxy 30.7k).
  - **Use:** they serve about 1.3 event wakes a second and the refusal ring's early removals, at most about 50 a
    second. About 0.1% of deliveries find anything to do.
- **Cost:** Redis 1.0–1.6 cores, api 1.1–2.0 and proxy 0.15–0.35 (0.7 once every proxy bucket is warm), so **2.3–3.9
  cores** at the 24-hour mean (§1.6). §10's 2.5–3 sits inside that range.
- **Recommendation (§4): a channel per pair, subscribed only after a miss.**
  - The committer publishes to the pair's own sharded channel, which carries the bucket's hash tag. It lives on the
    same slot, node and socket as today's bucket channel.
  - A process subscribes to a pair's channel only while it holds the pair past a miss: a wait whose read found no
    origin, or a stored refusal. Redis delivers an edge only to those processes. A publish nobody subscribed to costs
    one lookup.
  - Companion waits that their first read decides (99% of them) add no Redis round trip.
  - A wait that misses subscribes in the background. Its existing 100 ms pacing holds the next read to the time the
    fallback runs it today, so no read moves.
  - A missing-origin refusal is stored only from a read armed after its pair's subscription was acknowledged. So the
    ring's fence keeps today's exactness.
- **Rollout (§4.4):**
  1. Stage 0: counters only.
  2. Build A: publish both, subscribe both, idle buckets close, and a shadow counter proves the new path.
  3. Build B: the new path only, once every process runs A.

  Savings arrive in two parts. Build A takes the proxy's share, because idle buckets close. Build B takes the rest.
- **Open questions (§5):** per-pair channels or a registry with process inboxes; one extra read on a Stream-fallback
  miss to keep its refusal exact; destination refusals; the held-pair cap; the two-build rollout; shipping the counters
  and the idle-bucket close sooner; fixing the gap in §1.7 now.

## 1. Today

### 1.1 Who publishes

Five creation paths call `notifyCommittedContractOrigin` (model/contract_origin_notification.go) after their
transaction commits:
- model/subscription_model.go:
  - `createTransferEscrow` (source paid);
  - `createCompanionTransferEscrow` (companion);
  - `CreateContractNoEscrowWithExpiration` (no escrow);
- model/subscription_zero_escrow.go: `CreateZeroEscrowContract` and `CreateZeroEscrowCompanionContract`.

How a publish goes out:
1. `notifyCommittedContractOrigin` removes this process's own ring refusals for the pair
   (`contractRouteRejections.invalidate`).
2. It offers the pair's digest to the owner's queue (1,024 deep) without blocking.
3. Two `runPublisher` workers per owner SPUBLISH it (`publishContractOrigin`: a 1 s timeout, no retry).
4. A full queue (`queue_full`) or a failed publish (`publish_failed`) drops the edge, and the waits' polls cover it. In
   the last 7 days that happened 16.8k and 6.5k times, out of about a billion publishes.

The pair and its channel:
- **The pair** is `contractOriginPairHash(source, destination)`: SHA-256 over a domain tag and both ids, so ids never
  travel in plaintext.
- **The channel** is `contract-origin:v1:{bb}` (`contractOriginChannel`), where bb is the digest's first byte mod 16.
  That gives 16 hash tags and 16 slots, on 10 of the 48 Redis primaries; one primary carries three buckets.

Publishes on the 24-hour mean: api 1,078 a second, proxy 444, taskworker 127. connect and mcp publish none.

### 1.2 Who subscribes

Each lifecycle owner builds one `ContractOriginNotifications`:
- **api:** one per process. api/run.go `newRouterWithStuckProviderSignals` builds it, and
  api/handlers/connect_handlers.go `ConnectControlWithOwners` binds it to `/connect/control`. alt mounts the same
  router when it runs; it reported until earlier this week.
- **proxy:** one per device manager (proxy/proxy_device.go `NewProxyDeviceManager`), shared with its local authorities
  (localclient/authority.go `newAuthority`).
- **taskworker:** one per provider-egress pass (taskworker/work/provider_egress_control.go
  `runWithProviderEgressControl`).
- **connect:** one per exchange (connect/resident.go `newExchange`), for the in-band resident controller.

The constructor starts 16 `runBucket` goroutines and the publishers. A bucket opens no socket until a `Watch` lands in
it. From then on, `runBucket` runs `server.RunRedisShardSubscription` (redis_shard_subscription.go) on the bucket
channel for the owner's lifetime. Nothing ever closes a started bucket.

### 1.3 Why every process receives every edge

The channel names a bucket, not a pair or a process, so Redis delivers each edge to every socket subscribed to its
bucket. Each owner subscribes to a bucket the first time any request waits there, and stays.
- **api:** a process waits about 40 times a second, so all 16 buckets start in its first second.
- **proxy:** it waits about 0.09 times a second across 20 processes, so its buckets start over hours:
  - 17.1k deliveries a second on the 24-hour mean;
  - 30.7k at 14:30Z, with 14–16 of 16 buckets started per process;
  - 34.6k once every bucket is warm.
- **connect:** receives none. **taskworker:** 4 a second on the 24-hour mean.

### 1.4 What a receiver does with an edge

**Reading it.** `RunRedisShardSubscription` reads one event per go-redis `PubSub.ReceiveTimeout` call. Each call
re-arms the socket's read deadline, and a message usually costs a read syscall and a goroutine wake.

**Handling it.** `runBucket`'s handler checks the channel and the 64-hex payload, decodes it, counts `received`, and
calls `notifyBucket(index, &key)`. That does two things, in this order:
1. **The waiters.** It looks the pair up under the owner's lock. For a watched pair, it invalidates the shared missing
   hint (`contractOriginLookupState.invalidate`, model/contract_origin_lookup.go) and bumps the pair's version, which
   wakes every armed waiter.
2. **The refusals.** Then it removes the ring's refusals for the pair, in both request shapes
   (`contractRouteRejections.invalidate`).

   The order is the fence's: a verdict fenced on that version channel is either refused at `put` or removed.

**Resync.** An acknowledgement that starts or restarts a bucket's subscription runs `notifyBucket(index, nil)`. Every
pair in the bucket wakes and every ring refusal in the bucket goes, because edges published while no subscription
existed are lost. Bucket resubscriptions in the last 7 days: api 7.5k, proxy 4.4k.

The three consumers:
- **The companion wait.** controller/connect_companion_origin.go `waitForCompanionOrigin`, called from
  controller/connect_controller.go `newContract`.
  - A companion request that beats its origin reads PostgreSQL, then waits for an edge or a poll. The first retry is
    100 ms after the first read, then every 500 ms, to a 3 s deadline.
  - An edge brings a read forward, but never closer than 100 ms to the previous one. The watch is armed immediately
    before each read.
- **The Stream-fallback single lookup** (`CreateContract`). A non-companion request that falls back to a Stream
  companion reads once (`companionOriginExists`). The watch it armed before that read fences the missing-origin refusal
  it records.
- **The refusal ring** (model/contract_route_rejection.go).
  - **What it replays:** for 60 s, at most 16,384 entries a process: missing origins, missing destination rows and
    write-boundary destination refusals. Terminal (inactive-row) refusals live in their own generations and ignore
    edges.
  - **Edges:** an edge for the reverse pair removes a ring refusal early.
  - **Before the state query:** `PrepareConnectControlRefusals` (controller/connect_control_refusal_replay.go) answers
    retries from the ring before the client state query.

### 1.5 Measured (Mimir, 11-23)

| Quantity (api unless noted) | 24-hour mean | 10 minutes to about 14:30Z |
|---|---|---|
| origins published a second (api / proxy / taskworker) | 1,650 (1,078 / 444 / 127) | 1,732 (1,162 / 438 / 131) |
| deliveries a second to the 20 api processes | 33.0k | 34.6k |
| deliveries a second to the 20 proxy processes | 17.1k | 30.7k |
| deliveries per publish | 30.3 | 37.8 |
| companion requests a second (`…lookups_per_request` count) | 812 | 777 |
| … decided by their first read (at most one lookup) | 803 | 767 |
| … still waiting after it (two or more lookups) | 8.8 | 10.1 |
| wait wakes a second: event / fallback / deadline / cancel | 1.31 / 45.9 / 7.30 / 0.39 | 1.48 / 54.1 / 8.31 / 0.70 |
| refusals stored a second: missing origin / inactive destination | 37.1 / 287 | 39.0 / 301 |
| refusal replays (ring hits) a second: missing origin / inactive destination | 62.6 / 451 | 59.9 / 566 |
| proxy companion lookups a second (all 20 processes) | 0.088 | 0.041 |

- **Most inactive-destination refusals are terminal** and never touch the ring. By destination lifecycle (30 minutes,
  replays included):
  - terminal: inactive derived 770 a second, inactive top 13;
  - non-terminal: a missing row 9.7, the write boundary 1.3.
- **Stream-fallback requests:** about 57 a second. About 27 find their origin and about 30 miss. Derived from the gap
  between initial lookups (839 a second) and the histogram count (812), and from missing-origin refusals stored by the
  wait's deadline (7.3 a second).
- **What a delivery finds:** a waiter about 1.3 times a second, and a ring refusal at most ~50 times a second. Those
  are the missing-origin and non-terminal destination refusals stored; no counter separates the edges that removed
  one. That is about 0.1% of 50k deliveries. Stage 0 (§4.4) counts it exactly.

### 1.6 What it costs

Per delivery:
- **api: 35–59 µs.** §10.3.3 found the subscription at 4.5–4.9% of two api processes running 2.07 cores, for about
  1.72k deliveries a second each. That is 0.093–0.101 cores, or 54–59 µs a delivery. CPUOPT3 C #4 measured 35–40 µs.
  - Every api process receives the same deliveries, whatever its load (0.47–1.67 cores a process at 13:30–14:30Z).
    So the cost is per process, not a share.
- **Redis: 20.5–32.5 µs (8–20 + 12.5).**
  - On Redis's threads, §10.3.7's fit gives 14–20 µs.
  - Comparing the primaries that carry buckets with those that don't (edge-6, 30 minutes, at matched command rates)
    gives 8–15 µs.
  - Softirq on edge-5/6 adds 12.5 µs (§10.3.7).
- **proxy: 9–20 µs.** §10.3.2's 0.5–0.6% of a 2.3-core process with most buckets warm is about 9 µs a delivery as
  sampled. Its GC and wakeups land in other rows, hence the upper bound.

At the 24-hour mean:

| Where | Deliveries a second | Cost each | Cores |
|---|---|---|---|
| Redis (edge-5, edge-6) | 50.0k | 20.5–32.5 µs | 1.03–1.63 |
| api (20 processes) | 33.0k | 35–59 µs | 1.16–1.95 |
| proxy (20 processes) | 17.1k (34.6k warm) | 9–20 µs | 0.15–0.34 (0.31–0.69 warm) |
| **Total** | | | **2.3–3.9** (2.5–4.3 with the proxy warm) |

The receiving hosts' own softirq per delivery is not counted. Neither are the read-deadline re-arms. Each delivery
re-arms one, so each api and proxy process makes about 1.7k runtime timer modifications a second, the work C1 targets
in connect.

### 1.7 A gap found while reading

The companion wait's final missing-origin verdict can be stored stale for up to 60 s.

**The code.** The wait's watch closes when `newContract` returns (`defer originWatch.Close()`, in the companion branch).
`CreateContract` records the refusal only after `nextContract` returns, with the channel the last read armed
(`contractOriginFence`, controller/connect_controller.go).

**The interleaving.** If that watch was the pair's only one, `Close` deletes the pair from its bucket. Then:
1. an edge arrives in that window and finds no pair, so it closes no fence;
2. `notifyBucket`'s ring removal finds nothing, because the refusal isn't stored yet;
3. `put` stores the refusal, though the origin has already committed. It is replayed for up to 60 s.

**Size.** The window is one metric update long, so the effect today is tiny, and only on the process that stored it.
The design closes the window by moving the watch's close after the record, with the refusal taking its own hold first
(§4.1). It needs its own root-cause test (§4.3, test 2), and it can ship on its own (§5, question 7).

**Fixed** in 2a3984171 (§Status): `newContract` hands the watch to `CreateContract`, which closes it after the record.
TestCreateContractCompanionRefusalKeepsItsWatchUntilRecorded failed before the change and passes after it.

## 2. What any change must keep

1. **PostgreSQL stays authoritative.** A notification is a hint, so any Redis failure costs only the optimization. The
   polls remain.
2. **The wait's read schedule.** The first read; the next at +100 ms; then every 500 ms; a final read at 3 s. An edge
   brings a read forward, but never within 100 ms of the previous one. For any commit time, and the same Redis
   latency, no read may come later than today.
3. **The fence.** A missing-origin refusal is stored only if every origin that commits after its read's snapshot either
   closes the fence before `put`, or removes the entry after it.
4. **Resync.** Whenever a process may have missed edges for a pair it holds, that pair's waiters wake and its ring
   refusals go.
5. **Mixed versions.** Every roll and every rollback runs old and new processes side by side.
6. **Bounds.** State per owner stays bounded (`MaxWaiters` 8,192, a 16,384-entry ring), with no process-global
   singleton (CODESTYLE: budgets are owned by instances).
7. **Detection bounds.** The subscription sockets keep theirs (2 s acknowledgement, 2 s incoming evidence, 30 s
   re-assertion). CODESTYLE says detection bounds are correctness and are not traded for CPU.

## 3. Options

### 3.1 Option E: a channel per pair (recommended)

**Mechanism.**
- **Publish.** The committer publishes to `contract-origin:v2:{bb}:<64-hex pair>` instead of the bucket channel. The
  hash tag is the bucket's, so the channel lives on the same slot, node and socket as today's bucket.
- **Subscribe.** A process subscribes to a pair's channel only while it holds the pair past a miss:
  - a wait whose read missed;
  - a Stream-fallback re-check;
  - a stored non-terminal ring refusal.

  It subscribes on the bucket's existing socket (one SSUBSCRIBE takes many channels of one slot), and unsubscribes when
  the last holder lets go.
- **Deliver.** Redis delivers an edge only to the sockets subscribed to its pair, which are the processes waiting on it
  or holding its refusal. A publish nobody subscribed to costs Redis one dictionary lookup and replies 0.
- **The bucket socket** keeps an anchor channel `contract-origin:v2:{bb}` that nobody publishes to. The anchor serves
  `RunRedisShardSubscription`'s liveness rules: the acknowledgement, the pings, the 30 s re-assertion and slot-move
  detection. The socket closes after a minute with no held pair, and opens again with the next one.

**Saving.** Every delivery but the matched ones (about 0.1%) goes:
- Redis 1.0–1.6 cores, api 1.1–2.0, proxy 0.15–0.35 (0.7 warm): **2.3–3.9 cores** at the 24-hour mean.

New costs, together under 0.05 cores:
- about 100–150 SSUBSCRIBE and SUNSUBSCRIBE a second fleet-wide, and as many acknowledgements: ~50 holds a second,
  from about 10 continued waits, about 30 Stream-fallback misses and about 10 destination refusals;
- pings on the api's 320 bucket sockets, which now go quiet: about 320 a second, against about 45 PINGs a second
  across all of Redis today;
- about 30 more PostgreSQL reads a second (§4.1).

During Build A, the second SPUBLISH rides the same pipeline to the same node: about +0.1–0.15 Redis cores until Build B.

**Failure modes:**
- **A missed wake at subscribe time.** A waiter's read can miss a commit whose edge was published before its
  subscription existed. The acknowledgement wakes every waiter that armed before it, through a per-pair acknowledged
  channel, for a paced re-read after the acknowledgement. §4.2 shows no read moves later than today.
- **Socket loss.** A slot move, a node restart, a silent or blocked socket, or an error reply replaces the socket.
  Every held pair resubscribes on the new socket, and each pair's acknowledgement resyncs that pair: its waiters wake
  and its refusals go. This is today's bucket resync, narrowed to the pairs held.
- **A subscription lost without notice.** Every acknowledgement carries the socket's shard-channel count. A count that
  differs from the channels the reader has seen acknowledged replaces the socket.
- **Cluster slots.** All of a bucket's pair channels share its slot. A migration ends every one of them with a
  sunsubscribe, and the reader replaces the socket and reloads the slot map, as it does for the bucket today.
  Multi-channel SSUBSCRIBE never crosses slots.
- **A process restart or crash.** The socket closes and Redis drops its subscriptions, with nothing to expire. If a
  host dies without a FIN, Redis drops them at its TCP keepalive. Until then, a matched edge is written to a dead
  socket, which costs nothing anyone waits on.
- **Backpressure.**
  - **The publisher:** its queue and drop rules are unchanged.
  - **Subscription writes:** a per-bucket writer goroutine batches them, so no request writes to a socket. Only the
    Stream-fallback re-check waits for an acknowledgement, bounded by 100 ms.
  - **Output buffers:** pushes per socket fall from about 108 a second to nearly none. A stalled process (GC, or the
    saturated proxy hosts) therefore stops accumulating Redis output buffer. There were no pubsub buffer disconnects in
    7 days.
- **Leaks.** A hold that is never released leaves a subscription until its socket goes. The owner keeps a gauge of
  held pairs, and Redis exports `redis_pubsubshard_channels` per primary (75 today), so a leak shows. A cap bounds
  each owner.
- **Redis down.** Nothing subscribes, and the waits poll as they do when today's bucket subscription is down. No
  missing-origin refusal is stored without a fenced read. Today those are stored, and only the next subscription's
  resync clears them, so the new rule is the more conservative one. The Redis contract admission fails then anyway.

**Migration and rollout.** Two builds after the counters (§4.4):
- **Build A** publishes both v1 and v2 in one pipeline, and subscribes both.
- **Build B** drops v1, only once no pre-A process runs anywhere.

### 3.2 Option A: a registry of waiting processes, publishing to their inboxes

This is §10.5 X1's sketch, with E's subscribe-after-a-miss.

**Mechanism.**
- **Inboxes.** Each owner draws a random 128-bit id and, per bucket, subscribes to its own inbox
  `contract-origin:v2:{bb}:p:<owner id>`. Today's reader runs unchanged; only the channel name differs.
- **The registry.** A holder registers by HSET on `contract-origin:v2:{bb}:r:<pair>`, with its owner id as the field.
  Each field gets its own TTL (HPEXPIRE: 10 s for a wait, 75 s for a ring refusal). Hash-field expiry is there:
  Main runs Redis 8.10.2, and the vendored go-redis has `HPExpire`. The holder runs HDEL on release.
- **The committer** reads the pair's registry (HKEYS) after its commit and SPUBLISHes to each registered inbox. The
  inboxes share the slot, so that is one pipeline. Or a script does both.
- **Resync.** Every inbox acknowledgement re-registers the bucket's held pairs before it resyncs. A node that restarts
  loses both the keys and the socket.

**Saving.** About E's, less the registry's own work:
- HKEYS replaces SPUBLISH one for one;
- about 150 registry commands a second;
- a script, if used, adds the 64–67 µs script premium of §10.3.7, about 0.1 cores.

**Failure modes beyond E's:**
- **TTL.** A crashed owner's registrations live out their TTL. That is harmless: its inbox has no subscriber. But a
  holder that outlives its field's TTL loses edges silently, so the TTLs must exceed every hold. The ring holds for
  60 s.
- **Registration and inbox fail separately.** A registration can succeed while the inbox socket is down; the inbox
  acknowledgement's resync covers it.
- **The publisher reads before it publishes.** A matched edge takes two round trips, or a script.

**Against E.**
- *For A:* `RunRedisShardSubscription` stays untouched, and A follows connect's extender WebRTC offer wakes
  (connect/extender_webrtc_offer_wakes.go), where a registration names the process's own channel.
- *For E:* no keys, no TTLs, no owner ids. The publish stays one command. Redis's own subscription table is the
  registry, and a closed socket cleans it up.

### 3.3 Option B1: idle buckets close

**Mechanism.** A bucket with no watch and no held pair for a minute closes its subscription, and the next `Watch`
opens it again, as the lazy start does today. A cold start's acknowledgement resyncs the bucket, so the first
missing-origin refusal after it is not stored, as today.

**Saving.**
- **The proxy** waits 0.09 times a second across 20 processes, so its buckets would be closed almost always. That
  removes its 17.1–34.6k deliveries a second: Redis 0.35–1.12 cores and proxy 0.15–0.69, so **0.5–0.9 at the 24-hour
  mean and up to 1.8 warm**.
- **The api** saves nothing: its buckets never go idle.

**Failure modes and rollout.** A closed bucket's destination refusals see no edges and live out their 60 s, inside the
ring's documented bound. The alternative is to hold the bucket open, at the cost of the first refusal stored after a
cold start. There is no wire change, so it is one build in any order. E includes it, in Build A.

### 3.4 Option R: a cheaper receive, broadcast kept

**Mechanism (CPUOPT3 C #4(a)).**
- bind the counter children once, not `WithLabelValues` per message;
- decode the payload into a fixed `[32]byte`;
- skip the ring's lock when the ring is empty.

**Saving.** 20–30% of the receivers' cost *(inferred, CPUOPT3)*: about **0.26–0.69 cores** across the api and the
proxy, and none in Redis. The per-message wake and read syscall, which dominate, stay.

**Risk and rollout.** Low; one build. It only makes sense if E and A are declined.

### 3.5 Not proposed

- **C. Version keys and polls, no push** (CPUOPT3 C #4(b), CPUOPT4 #6).
  - **Mechanism:** the committer bumps a per-pair key instead of publishing, a ring replay checks the key, and waits
    poll PostgreSQL only.
  - **Saving:** the same as E. Ring freshness even becomes exact at replay time.
  - **Why not:** about 1.3 event-woken waits a second would wait for their next 500 ms poll instead of about 1 ms, up
    to 400 ms later each. That breaks the owner rule.
  - **Polling the key at 20 ms** keeps most of the wake for about 1.5k GETs a second. Wakes still come up to 20 ms
    later, so the rule still fails.
- **D. Batching publishes.**
  - **Why not:** a bucket carries about 108 publishes a second, so a 5 ms window holds about half an edge. Only
    windows near 100 ms batch tenfold, and those delay wakes.
  - Batching across buckets would cross slots and nodes.
- **A relay per host.** One subscriber per host fanning out locally would cut Redis's deliveries about sixfold. It adds
  a new component and IPC on every host, and leaves every process's receive cost in place.

### 3.6 Side by side

| Option | Saving at the 24-hour mean (cores) | Wire change | Wait timing and fence | Risk |
|---|---|---|---|---|
| **E. A channel per pair** | **2.3–3.9** (Redis 1.0–1.6, api 1.1–2.0, proxy 0.15–0.35) | two builds | unchanged (§4.2) | medium |
| A. Registry and inboxes | about 2.2–3.8 | two builds | unchanged | medium |
| B1. Idle buckets close | 0.5–0.9 (up to 1.8 with the proxy warm) | none | unchanged | low |
| R. Cheaper receive | 0.26–0.69 | none | unchanged | low |
| C. Version keys and polls | as E | yes | loses the event wake | not proposed |
| D. Batching | about 0 at windows that keep the wake | yes | delays wakes | not proposed |

## 4. Recommendation: Option E, in two builds after a counter

### 4.1 The design

**State per owner** (model/contract_origin_notification.go):
- **A pair entry** gains a hold count, a subscription state, an acknowledged channel and the socket generation it was
  acknowledged on. The states are none, requested, acknowledged and releasing.
- **Holders:**
  - a wait after its first missed read;
  - the Stream-fallback re-check;
  - each non-terminal ring refusal, in either shape.

  A watch that hasn't missed holds nothing in Redis, as today.
- **`Watch.Update()`** returns the version channel and a fenced flag. The flag says whether the pair's subscription was
  acknowledged when the channel was armed.

**Subscribing:**
- **The first holder** requests the subscription. The bucket's writer, one goroutine per bucket parked when idle,
  sends a single SSUBSCRIBE for every requested pair and a single SUNSUBSCRIBE for every released one. It sends at most
  256 channels a command.
- **A pair's first acknowledgement** invalidates its missing hint and closes its acknowledged channel. It bumps no
  version and removes no refusal, so the version channel keeps meaning an origin edge or a resync.
- **A lost socket** returns every held pair to requested, with a fresh acknowledged channel. The resubscription's
  acknowledgement then does what today's bucket acknowledgement does, for that pair only. It invalidates the hint, bumps
  the version, removes the pair's refusals and closes the acknowledged channel.
- **A wait that misses** calls `watch.Missed()`. If the read was fenced, that returns nil. Otherwise it returns the
  pair's acknowledged channel, already closed if another holder's acknowledgement came first.
- **The wait selects on it as a new wake source, `registration`.** Its 100 ms pacing then holds the re-read to when
  the fallback would have run it: the same time as today, but after the acknowledgement. `event` keeps meaning an edge.
  (TestCompanionOriginWaitDoesNotLoseEventDuringLookup pins today's 100 ms.)

**The Stream-fallback single lookup** (`CreateContract`):
- A miss whose read was fenced is recorded as today.
- A miss whose read was not fenced:
  1. requests the subscription and waits up to 100 ms for the acknowledgement;
  2. re-arms and reads once more;
  3. records the refusal only from that fenced read, or creates the contract if the origin is there now.

  If no acknowledgement comes in time, it answers the refusal and stores nothing, as today when the owner declines a
  watch.
- **Cost:** one more single-statement read and about a millisecond, on about 30 refusals a second. §5 question 2
  offers cheaper alternatives.

**The companion wait's final verdict.** Its last read was armed after the acknowledgement, so the read is fenced. The
watch now closes after `CreateContract` records the refusal: `contractOriginFence` carries the watch, and the refusal
takes its own hold before the watch lets go. That closes §1.7.

**Ring refusals** (model/contract_route_rejection.go):
- **Holds.** `put` of a non-terminal refusal takes a hold of its pair. `popWithLock`, an expiry in `get`,
  `invalidate`, `invalidateBucket` and `close` release it. The releases run outside the ring's lock, whose rule is no
  I/O under the lock.
- **Expiry.** An expiry pass runs at most once a second, so an idle owner releases expired refusals too.
- **Missing-origin refusals** are stored only with a fenced channel.
- **Destination refusals** keep their nil fence and subscribe at `put`. Edges published before that subscription
  applies don't reach them, just as edges during their read don't today (§4.2, case 5).
- **The cap.** An owner holds at most 8,192 pairs, against about 150 an api process needs today. Over the cap, a
  missing-origin refusal is answered and not stored.

**Sockets** (redis_shard_subscription.go):
- **Two optional handler hooks:**
  - the live socket after each subscription starts, so the writer sends only on the current socket;
  - acknowledgements for channels other than the anchor: pair acknowledgements, its own unsubscribes and unrequested
    sunsubscribes.

  nil keeps today's behavior for the other users (proxy/proxy_client_notification.go,
  connect/extender_webrtc_offer_wakes.go). The socket interface gains `SUnsubscribe`, which go-redis's `PubSub` has.
  go-redis allows subscribes while the reader is blocked ("Don't hold the lock to allow subscriptions and pings").
- **On a new socket,** the anchor's acknowledgement is followed by one SSUBSCRIBE of every held pair. Each
  acknowledgement then resyncs its pair.
- **Replacement rules:**
  - a pair acknowledgement missing past the acknowledgement timeout (2 s), as for the anchor;
  - an unrequested sunsubscribe or a channel-count mismatch;
  - a MOVED or other error reply, as today.

**Publish:**
- **Build A** sends `SPUBLISH contract-origin:v1:{bb} <hex>` and `SPUBLISH contract-origin:v2:{bb}:<hex> ""` in one
  pipeline: same slot, one round trip.
- **Build B** sends only the second.
- The queue, the workers and the drop rules are unchanged.

**Idle buckets (B1).** A bucket socket with no held pair and no watch for a minute closes, and opens again with the
next.

**Settings** (CODESTYLE: tunables live in settings): `MaxHeldPairs` 8,192; `RegistrationRecheckTimeout` 100 ms;
`IdleBucketTimeout` 60 s; `HeldPairExpiryInterval` 1 s; `SubscribeBatchSize` 256.

### 4.2 Why no wake and no fence is lost

**Events,** for one pair P and one process X:
- **arm:** a waiter takes P's version channel, immediately before a read with snapshot s.
- **c:** an origin for P commits, in any process.
- **p:** its edge executes on P's slot owner. It always follows c, because the committer enqueues after its commit
  returns.
- **a:** X's subscription to P applies on the slot owner.
- **a′:** X reads its acknowledgement. Redis answers in order, so any push published after a reaches X after a′.

**Facts:**
- **F1.** Redis delivers p to X exactly when X's subscription exists at p.
- **F2.** A hold is released only after its holder no longer needs P: the wait has ended, or the refusal is gone.
  Until then, the subscription ends only with a lost socket. After a lost socket, the next socket resubscribes P, and
  its acknowledgement a2′ wakes P's waiters and removes P's refusals.
- **F3.** The acknowledgement a′ invalidates P's missing hint and closes P's acknowledged channel. Every waiter whose
  read was armed before a′ then re-reads after a′, paced.

**Cases:**
1. **A waiter armed after a′ (fenced).** Then a < a′ < arm < s < c < p. If the socket lives to p, the edge is
   delivered (F1) and the waiter wakes as today. If not, a2′ follows p and wakes it (F2), as today's bucket resync
   does for the same socket loss.
2. **A waiter armed before a′, which missed.** It re-reads after a′ (F3).
   - If c < a, that re-read sees the origin.
   - If c > a, then p > c > a, so the edge is delivered, as in case 1.

   Today, with the bucket already subscribed, the same waiter wakes at the edge and re-reads at the later of the edge
   and 100 ms after its last read began. a′ comes about one Redis round trip after the miss. The earliest re-read is
   100 ms after the missed read began. So both schedules are the same whenever the read and the acknowledgement
   together take under 100 ms.

   A slower acknowledgement is bounded by the polls. Today's publish needs the same Redis to be fast.
3. **The fence.** A missing-origin refusal is recorded only from a read armed after a′, while P is held. Any origin
   committing after that read has p > c > s > arm > a′ > a, so its edge reaches X while P is held, or a2′ follows it
   (F2).

   Either way, X bumps P's version, closing the fence, before it removes P's refusals: the order of `notifyBucket`
   today. So `put` either sees the fence closed or runs before the removal.
4. **No hold gap.** The refusal takes its hold before the waiter's watch releases its own, so F2 covers the whole span
   from arm to the refusal's removal (§1.7).
5. **Destination refusals** are unfenced today. A commit whose edge publishes during the verdict's read (before `put`)
   leaves the refusal for its 60 s. In E, the same holds for edges published before the refusal's subscription
   applies (before a): a window about one Redis round trip wider. Both sit inside the ring's documented bound: "an
   origin edge this owner never receives … refuses an identical request for at most the timeout."
6. **Mixed versions.**
   - **Build A:** every A publisher sends v1 and v2 in one pipeline to one node, and every A owner subscribes v1 (the
     anchor) and v2 (after a miss). Either path alone suffices, and pre-A processes still get v1.
   - **Build B:** v1 goes only once no pre-A process runs. Every publisher then sends v2, and every owner, A or B,
     subscribes v2 after a miss.

### 4.3 Tests

These use the synthetic subscription harness of model/contract_origin_notification_test.go, extended into a small
broker that keeps per-channel subscribers and per-socket channel counts, under `testing/synctest`, with barriers rather
than sleeps.

**Root cause** (each fails on today's code):
1. `TestContractOriginEdgeReachesOnlyHoldingOwners`. Three owners on one broker; one misses on P. 1,000 publishes for
   P and other pairs reach that owner only for P, and the other two receive nothing. Today every owner receives every
   publish in its started buckets.
2. `TestCreateContractCompanionRefusalKeepsItsWatchUntilRecorded`. An edge is injected between `newContract`'s return
   and the refusal's record. Today the stale refusal is stored (§1.7). After the change it is refused or removed.

**Orderings and races:**

3. `TestContractOriginSubscribeAfterMissOrderings`. Every order of the read snapshot, a, a′, c and p, with the
   acknowledgement before and after the 100 ms read.
   - The read schedule equals the same harness's v1 schedule when a′ comes before the 100 ms read.
   - Nothing passes the 3 s bound in any order.
   - No read starts sooner than 100 ms after the previous one started.
4. `TestContractOriginSlowAcknowledgementWakesForReread`. An acknowledgement after the 100 ms read wakes the wait
   (`registration`), and the next read finds a commit that landed before it.
5. `TestContractOriginSubscriptionFailureFallsBackToPolls`. With every SSUBSCRIBE failing, the reads run at 0, 100 and
   600 ms and so on to the final read, and no missing-origin refusal is stored.
6. `TestContractOriginSocketReplacementResubscribesHeldPairs`. Lose the socket while pairs are held:
   - the new socket resubscribes all held pairs in one command;
   - each acknowledgement wakes its pair and removes its refusals;
   - unheld pairs are untouched.
7. `TestContractOriginUnrequestedSunsubscribeReplacesSocket`, plus a channel-count mismatch, an error reply and a
   missing pair acknowledgement past 2 s: each replaces the socket.
8. `TestContractRouteRejectionFenceArmedAfterAcknowledgement`. Every order of verdict read, commit, publish and `put`
   never leaves a stale stored refusal. Extends `TestContractRouteRejectionClosedFenceRefusesStaleVerdict`.
9. `TestContractRouteRejectionHoldsBalance`. Over 10,000 seeded random operations (put, get, expiry, eviction, edge,
   resync, close), held pairs always equal the live non-terminal refusals plus waits past a miss, and SSUBSCRIBE and
   SUNSUBSCRIBE balance.
10. `TestCreateContractStreamFallbackRechecksAfterAcknowledgement`:
    - an unfenced miss subscribes, re-reads and records only from the fenced read;
    - an origin committed between the two reads lets the contract proceed;
    - a timed-out acknowledgement answers without storing.
11. `TestContractOriginMixedVersions`. Old (v1 only), A and B publishers against old, A and B owners, in every
    allowed pair. Every wait is woken and every refusal removed. The disallowed pair, an old owner against a B
    publisher, shows the regression the roll rule prevents.
12. `TestContractOriginShadowCountsMatch`. Dual publishes to one socket: v1 edges for acknowledged pairs equal v2 edges
    received.
13. `TestContractOriginIdleBucketClosesAndReopens`. After the idle timeout the socket closes; the next hold reopens it,
    and its acknowledgement resyncs.
14. **Cluster.** Extend `TestRedisShardSubscriptionFollowsSlotMigration` (redis_shard_subscription_cluster_test.go) to
    held pair channels on a local cluster: migrate the slot, then check resubscription and that no wake is lost.

**Unchanged and passing:**
- every `TestCompanionOriginWait*`, `TestContractRouteRejection*`, `TestContractOriginNotifications*` and
  `TestRedisShardSubscription*` test (the last with nil hooks);
- `TestContractOriginPublicationAllCreatorsAreCommitted`;
- the refusal replay tests.

**Benchmark.** `BenchmarkContractOriginDelivery`: CPU per publish with 40 owners, v1 against v2, with one owner
holding.

### 4.4 Rollout and canary

All api and proxy rolls run under the usual gates: the validator-idle gate, and the proxy's canary block and
30-minute hold. Each build lists its own order below.

**Stage 0: counters, with no behavior change.** `notifyBucket` counts `matched_waiter`, `matched_refusal` (the ring's
`invalidate` returns whether it removed anything) and `unmatched`. It can ride any routine deploy. Expect `unmatched`
at 99.9% or more. If it is not, revisit this design before building it.

**Build A: both paths.**
- dual publish; v1 bucket subscriptions; v2 pair subscriptions after a miss; holds for refusals;
- idle buckets close (B1);
- the watch's close after the record (§1.7);
- the shadow counters `v1_for_acknowledged_pair` and `v2_received`.

Roll it to every process, in this order: api, alt if it runs, proxy, taskworker, connect. It takes the proxy's
deliveries at once, because idle buckets close. Then soak it for 24 hours. The shadow counters are read only once
every publisher runs A, because a pre-A publisher sends v1 alone.

**Build B: v2 only.** It starts only when no pre-A process runs anywhere. The order:
1. the proxy first: it waits least and holds half the deliveries;
2. one api block for at least 2 hours, against the other blocks on the same hosts;
3. the rest of the api, then taskworker and connect.

**Rollback:**
- B to A at any time: A subscribes and publishes both.
- A to pre-A only while no B process runs anywhere.
- Once B starts, pre-A builds of api, alt, proxy, taskworker and connect are off the table, which the deploy version
  guard should state.

Signals, canary against control:

| Signal | Metric | Expect |
|---|---|---|
| deliveries | `urnetwork_contract_origin_notifications_total{event="received"}` per process | Build B: about 1.7k a second → matched only, a few a second |
| holds | new `subscribed_pair`, `pair_acknowledged`, `pair_released`, `subscribe_failed`, `pair_cap` events; a held-pair gauge | about 50 holds a second fleet-wide; about 150 held per api process; failures and cap near 0 |
| correctness, waits | `urnetwork_connect_companion_origin_wait_wakes_total{source}`, `…lookups_total{source}` and `…lookups_per_request`, per request | `event` and `deadline` per request unchanged; `fallback + registration` per request unchanged (`registration` replaces a continued wait's first `fallback`); `registration` ≈ continued waits plus Stream-fallback misses |
| correctness, outcomes | `urnetwork_contract_creation_companion_origin_outcomes_total{outcome}`, `urnetwork_contract_creation_completed_total` | `origin_ready` and `missing_origin` shares unchanged |
| timing | `urnetwork_contract_creation_completed_stage_seconds_total{stage="companion_origin"}` per completion | unchanged; Build B ±5% against control |
| refusals | `urnetwork_connect_contract_route_rejection_cache_total{cause,result}`, `urnetwork_connect_control_refusal_replays_total{cause}` | missing-origin stored and hit rates per request unchanged within the Stream-fallback re-check's effect; `unstored` near 0 |
| sockets | `urnetwork_redis_shard_subscriptions_total{event}`; contract-origin `reconnected` and `subscription_failed` | no rise; the proxy's sockets mostly closed |
| shadow (Build A) | `v1_for_acknowledged_pair` against `v2_received` | equal over 24 hours, once every publisher runs A |
| cost | `process_cpu_seconds_total` | api −0.06–0.10 cores a process; proxy: more work per core |
| cost, Redis | `redis_commands_total{cmd=~"spublish\|ssubscribe\|sunsubscribe\|ping"}`, `redis_pubsubshard_channels`, the bucket primaries' CPU, `node_cpu_seconds_total{mode="softirq"}` on edge-5/6 | SPUBLISH flat (doubled in Build A); shard channels ≈ held pairs; bucket primaries and softirq down by 1.0–1.6 cores in all, the proxy's part at Build A and the rest at Build B |

**Stop and roll back to A** if any of these hold:
- event wakes per request fall more than noise against control;
- deadline wakes or missing-origin outcomes per request rise;
- the shadow counters diverge;
- subscribe failures persist.

## 5. Open questions for the owner

Answered on 11-23: the proposal to each, as §Status records.

1. **E or A?** E is per-pair channels: Redis's own subscription table is the registry and no keys expire. A is a
   registry with process inboxes: today's reader is untouched, at the cost of keys, TTLs and a read before each
   publish. Both save about the same. E is recommended.
2. **The Stream-fallback miss.** To keep its stored refusal exact, an unfenced miss takes one more read and about a
   millisecond, on about 30 refusals a second. The alternatives:
   - store only from a process's second sighting of the route: no extra read, slightly fewer replays;
   - store unfenced: up to 60 s stale, as destination refusals are.
3. **Destination refusals.** Should non-terminal ones (about 10 a second, replays included) subscribe at `put`, as
   proposed? Edges before that subscription are missed, about as today. The alternative is no subscription, which lets
   each one live its full 60 s.
4. **The cap.** Is 8,192 held pairs per owner right, against about 150 an api process holds today? And should a
   missing-origin refusal over the cap be answered and not stored?
5. **Rollout.** Two builds with no flag, as proposed (no hidden settings), with a 24-hour Build A soak? Or one build
   with a Redis control that turns v1 off?
6. **Sooner steps.** Ship Stage 0's counters on the next routine deploy? And ship B1 (idle buckets close: 0.5–0.9 cores
   now, up to 1.8 with the proxy warm, no wire change) ahead of E, or only inside Build A?
7. **The gap in §1.7.** Fix it now on its own (small, with test 2), or with Build A?
8. **A wake label.** Add the new wake source `registration` to `urnetwork_connect_companion_origin_wait_wakes_total`?

## Appendix: the queries

All are Mimir instant queries (`/prometheus/api/v1/query`), read-only, 11-23 14:20–14:40Z.

- **Publishes and deliveries:**
  - `sum by (job, event) (rate(urnetwork_contract_origin_notifications_total{event=~"published|received|enqueued"}[10m]))`,
    and the same over `[24h]`;
  - per proxy process: `sum by (instance) (rate(…{job="proxy",event="received"}[10m]))`.
- **Waits:** `sum by (source) (rate(urnetwork_connect_companion_origin_wait_wakes_total{job="api"}[24h]))`,
  `…lookups_total`, `…lookups_per_request_bucket` and `_count`.
- **Refusals:** `sum by (cause, result) (rate(urnetwork_connect_contract_route_rejection_cache_total[24h]))`, and
  `sum by (destination_lifecycle) (rate(urnetwork_connect_inactive_destination_details_total{job="api"}[30m]))`.
- **Failures:** `increase(urnetwork_contract_origin_notifications_total{event=~"queue_full|publish_failed|reconnected|subscription_failed"}[7d])`,
  and `increase(redis_client_output_buffer_limit_disconnections_total[7d])`.
- **Redis:**
  - `rate(redis_commands_total{cmd=~"spublish|ssubscribe|ping"}[30m])` by node;
  - per-node `redis_cpu_sys_seconds_total` + `redis_cpu_user_seconds_total` and `redis_commands_processed_total` on
    edge-6;
  - `redis_instance_info` (version, roles) and `redis_pubsubshard_channels`.
- **CPU:** `sum by (host, block) (rate(process_cpu_seconds_total{job="api"}[1h]))`, and `[24h]` by job.
