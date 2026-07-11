# PERFNOTES2 — pg_stat_statements audit and fixes (2026-07-10)

Source: `SLOW1.csv`, a `pg_stat_statements` capture ordered by `avg_time_ms`
(`100 <= calls`). The window spans roughly 173 days (~mid-January 2026 →
2026-07-10): 361 distinct statements, ~33k hours of total execution time, an
average of ~7.9 busy cores. Percentages below are of total DB execution time
over that window.

Caveat: index fixes shipped mid-window (`39f54e2a` 2026-03-03 contract-index
reshuffle, `e9abbe2c` 2026-06-13 dispute index, `35f14f2e` 2026-05-14 verify
code retention), so the lifetime averages blend pre- and post-fix behavior.
After deploying this round, run `SELECT pg_stat_statements_reset()` and
re-audit in ~2 weeks.

## Top offenders and fixes

### 1. Announce-path reliability stats — ~30% (fixed: redis rollup)

`client_reliability` took 36.2B single-row upserts (2.4k/sec sustained), each
in its own repeatable-read transaction, from the connection announce loop
(`connect/transport_announce.go`, every `ReliabilityBlockDuration/2` per
connected provider). Together with the per-sync `provide_key_change` COUNT
(30B calls) and `provide_mode` reads (37B calls, already redis-first), this
path was the bulk of 74B `BEGIN`s (~5.4k tx/sec).

Fix — mirror the `RecordProviderSearchMatches`/`RollupSearchProviderStats`
pattern:

- `model.RecordClientReliabilityStatsRange` (hot path): increments counters in
  a per-block redis hash `client_reliability_stats.<block>`, field
  `<hash hex>:<network_id>:<client_id>:<counter idx>`, plus a pending-blocks
  SET. Redis-only, best-effort (errors are swallowed with a log so a stats
  hiccup cannot kill the reporting connection). All commands per block target
  one key → one slot → cluster-safe pipelines.
- `RollupClientReliabilityStats` (serial task, reschedules every ~1min,
  `task.RunOnce("rollup_client_reliability_stats")`): drains closed blocks into
  `client_reliability` as chunked `unnest`-array bulk upserts.
- Block finality rule: the recorder refuses blocks older than
  `currentBlock - 1` (drops with a log line instead of corrupting); the drain
  only touches blocks with `currentBlock >= block + 2`. A drained block can
  therefore never change again, counts are written as absolutes
  (`DO UPDATE SET c = EXCLUDED.c`), and a re-drain after a crash is
  idempotent.
- High-water mark `client_reliability_rollup.max_drained_block` advances to
  `currentBlock - 2` every run (even idle). All four score computations shift
  their block windows back by `reliabilityRollupBlockShift` so windows only
  span fully drained blocks. Shifting (not truncating) preserves window width,
  so the `SUM/(max-min)` weight normalization keeps its scale. No high-water
  row → shift 0 (pre-rollup behavior; also how pg-direct test fixtures work).
- `AddClientReliabilityStats*` remains as the pg-direct fixture/backfill path.

Net effect: ~2.4k statements/sec + tx overhead collapse to a few bulk inserts
per minute; `client_reliability` becomes write-once per (block, client), which
also keeps its visibility map clean for the index-only score scans below.

### 2. `pending_task` poll — 12.35% (fixed: matching-direction index)

`SELECT ... WHERE available_block <= $1 ORDER BY available_block,
run_priority DESC, run_max_time_seconds DESC LIMIT n FOR UPDATE SKIP LOCKED`
(71.6ms × 204M calls ≈ one full core). The old index
`(available_block, run_priority, task_id)` cannot produce a mixed-direction
order, so every poll fetched and sorted the entire ready backlog.

Fix: `pending_task_poll_order (available_block, run_priority DESC,
run_max_time_seconds DESC)` — the poll streams in index order and stops at
LIMIT; dropped `pending_task_available_block`; set aggressive per-table
autovacuum on `pending_task` (`scale_factor 0.01`, `cost_delay 0`) since queue
tables live and die by dead-tuple density.

### 3. Orphan/retention sweeps — ~16% (fixed: cascade at source + daily safety net)

The retention tasks ran full-table anti-joins continuously (task Post
reschedules ~1min after completion): `provide_key` (3.5–5.5min avg,
unbounded), `device` (2min, unbounded), `transfer_contract`/
`transfer_escrow_sweep`/`contract_close`/`transfer_escrow` (44s–2.6min each,
`DISTINCT ... ORDER BY contract_id LIMIT` forced materializing every eligible
row before the limit).

Fixes:

- `RemoveCompletedContracts`: each removal is now one CTE statement that
  deletes a bounded candidate batch and cascades
  `contract_close`/`transfer_escrow`/`transfer_escrow_sweep` for those
  contract ids atomically. Candidates have no DISTINCT and no ORDER BY (any
  batch will do; duplicate matches in `DELETE ... USING` are harmless), and
  use `NOT EXISTS` — so the scan stops at LIMIT. The paid-contracts driver
  flipped from `account_payment` (completed payments accumulate forever) to
  `transfer_escrow_sweep` (deleted as processed, so the scan is bounded by the
  live sweep set). New `transfer_balance (end_time)` index for the balance
  delete.
- `RemoveDisconnectedNetworkClients`: the network_client deletes RETURN
  `client_id, device_id`; cascades (`provide_key` + redis mirrors,
  `proxy_device_config` + redis + `proxy_client`/`proxy_client_change`,
  `client_tls_certificate`, `device` with a "no other client references it"
  recheck) are targeted chunked `ANY(uuid[])` deletes on exactly the reaped
  ids. The connection + location/latency/speed cleanup is one bounded CTE loop
  (50k per batch) instead of a full delete plus three anti-join sweeps.
- The full anti-join sweeps became **daily** safety-net tasks
  (`sweep_orphan_contract_data`, `sweep_orphan_network_client_data`), reshaped
  to bounded streaming batches, still clearing redis mirrors. They only catch
  orphans from older releases or non-reap deletion paths.
- `RemoveProxyDeviceConfig` now cascades its `proxy_client`/
  `proxy_client_change` rows itself (previously relied on the frequent orphan
  sweep). Change rows only propagate adds/updates (`GetProxyClientsSince`
  INNER JOINs `proxy_client`), so deleting them with the config is safe.

### 4. Reliability window-score rebuild — ~9% (fixed: pre-aggregated join + upsert)

`UpdateNetworkReliabilityWindowScoresInTx` recomputed the 7-day window from
scratch each cycle (2,225s avg — 37 minutes) after a full-table DELETE
(2min avg, plus permanent bloat). The cost driver was
`COUNT(*) OVER (PARTITION BY block_number, client_address_hash)` over the
joined 7-day rowset — an external sort of hundreds of millions of rows.

Fixes (applied to `UpdateClientReliabilityScores`,
`UpdateNetworkReliabilityScoresInTx`, `UpdateNetworkReliabilityWindowScoresInTx`,
and the bucket query in `UpdateNetworkReliabilityWindow`):

- The window function is replaced by a pre-aggregated `valid_counts` subquery
  (`GROUP BY block_number, client_address_hash` over
  `WHERE valid = true AND block range`) joined back on
  `(block_number, client_address_hash)`. The count streams off the
  `(valid, block_number, client_address_hash)` index in order and merge-joins
  back — no full-range sort.
- Refresh is upsert + stale-row delete
  (`DELETE ... WHERE max_block_number != $current`; per-lookback for the
  client scores) instead of delete-all + reinsert, ending the bloat cycle on
  `client_connection_reliability_score`, `network_connection_reliability_score`
  and `network_connection_reliability_window_score`.

Semantic note (deliberate, locked by
`TestClientReliabilityScoreSharedIpNormalization`): `valid_client_count` — the
shared-IP dilution denominator — now counts by `client_reliability.valid`
alone (stats-valid), without the location-reliability join. This matches the
bucket window aggregation and the expected math in
`TestAddClientReliabilityStats`; the numerator still requires a valid
location, so location-invalid clients earn nothing but still dilute their IP.

### 5. `user_auth_verify` mark-used — 1.64%, 4.5s avg (fixed: bounded update)

`AuthVerifyCreateCode` ran `UPDATE user_auth_verify SET used = true WHERE
user_id = $1` on every code send — rewriting the user's entire code history
(already-used rows included) and serializing concurrent sends. Fixed with
`AND used = false`. Retention (`RemoveExpiredVerifyCodes`, effective 4h
window) already landed 2026-05-14, so the lifetime average is mostly
pre-cleanup history; the delete now raises on error instead of failing
silently.

### 6. `GetTransferStats` — 1.93%, 254ms × 9M calls (fixed: index + cache)

The `/transfer/stats` endpoint aggregated three tables per call, uncached, and
`account_payment` had no `(network_id, completed)` index. Added
`account_payment_network_id_completed (network_id, completed) INCLUDE
(payout_byte_count)` and wrapped the handler in `router.CacheWithAuth`
(60s TTL) — the totals only move on sweeps and payouts.

### 7. Open-contract close poll — 2.35%, 5.3s × 528k calls (fixed: partial index)

`ForceCloseOpenContractIds` polls `WHERE open AND create_time <= $1 ORDER BY
create_time LIMIT n`; after the 03-03 reshuffle only
`(create_time, open, contract_id)` remained, which walks every old *closed*
contract. Added the partial index
`transfer_contract_open_partial_create_time ON transfer_contract (create_time)
WHERE open` — tiny (open contracts only), perfectly ordered, and unlike the
dropped `(open, create_time)` full index it cannot be chosen for queries that
don't filter on `open`.

### 8. `transfer_contract` index consolidation (fixed: 7 → 5 full-width secondaries)

Every index on `transfer_contract` is maintained on each insert (1.75B in the
window) and again on every close — the generated `open` column flips on close,
so closes are never HOT updates and touch every index. A full query inventory
(every `FROM/JOIN/UPDATE/DELETE transfer_contract` in the repo) mapped each
predicate shape to an index:

| query shape | call sites | index |
|---|---|---|
| `contract_id = $1` | closes, settles, joins, cascades | PK |
| `open + source_id + destination_id [...]` | escrow create/dedupe, per-client force close | `transfer_contract_open_source_id` |
| `open + create_time` (close poll) | `ForceCloseOpenContractIds` | `transfer_contract_open_partial_create_time` (partial, new) |
| `dispute AND outcome IS NULL + create_time` | expired-dispute scan | **was** full-width `dispute_create_time` |
| `create_time + open = false` (retention) | `RemoveCompletedContracts` | `transfer_contract_create_time` |
| `destination_id + close_time` / `+ create_time` | /stats/providers* (7 sites) | the two destination indexes |
| `payer_network_id + open` (SUM) | `GetOpenTransferByteCount` — the *only* payer query | **both** payer indexes (redundant pair) |

Consolidations taken:

- Dropped `transfer_contract_payer_network_id (payer_network_id, open,
  contract_id)` — the open-bytes SUM is served index-only by
  `(open, payer_network_id, transfer_byte_count)`, and nothing else filters by
  payer.
- Replaced full-width `transfer_contract_dispute_create_time (dispute,
  outcome, create_time)` — an entry per contract to serve a scan over the tiny
  undecided-dispute set — with the partial
  `transfer_contract_dispute_partial_create_time (create_time) WHERE (dispute
  AND outcome IS NULL)`, which only carries entries while a dispute is
  undecided. Pre-create CONCURRENTLY out of band like the other large-table
  indexes.

Kept deliberately:

- The `(destination_id, close_time)` / `(destination_id, create_time)` pair:
  merging them would require bounding `close_time - create_time` to rewrite
  the close_time-window stats queries with a create_time floor, but disputed
  contracts can settle long after creation, so a floor would silently drop
  them from provider stats.
- `transfer_contract_create_time (create_time, open, contract_id)`: sole user
  is now the retention scan, which it serves index-only.

Index ledger: before this round transfer_contract had 7 full-width secondary
indexes (+ PK); the two drops above take that to 5 (+ PK), alongside the two
partials — the open-poll partial from §7 and the dispute partial, which carry
entries only while a contract is open / while a dispute is undecided. Per-row
write cost: an insert previously maintained 7 secondary entries + PK; now 5 +
PK + the open partial. A close (non-HOT, so it re-inserts entries in every
index that keeps one) previously touched all 8; now 6 — the open-partial entry
is not re-created when `open` flips false, and the dispute partial is empty
for normal contracts.

### Already fixed mid-window (no action)

The dispute scan (2.34%, 25.7s avg) is covered by
`transfer_contract_dispute_create_time` (2026-06-13); the open-bytes SUM by
payer (1.9%) by `transfer_contract_open_payer_network_id_transfer_byte_count`.
Their averages are dominated by pre-fix history.

## Bug found by the new tests

`UpdateClientLocationReliabilitiesInTx` never detected multi-IP clients: pgx
`Scan` into a `*[]byte` **replaces** the slice rather than filling the
`[32]byte` backing array it was created from, so every row keyed the zero
hash and `client_address_hash_count` was always 1 — the hash-count half of the
location-validity check (`network_client_location_reliability.valid`) never
fired in production. Fixed by scanning into a plain `[]byte` and `copy`ing
into the array. Behavior change: clients connected from multiple /29s are now
actually marked location-invalid, which is the designed (stricter) scoring
behavior.

## Tests

New/updated (all passing, plus the pre-existing exact-math regression
`TestAddClientReliabilityStats` which validates every rewritten score query):

- `TestRecordClientReliabilityStatsRollup` — accumulate, two-block finality,
  idempotent re-drain, bucket cleanup, high-water mark, stale-write clamp.
- `TestRecordClientReliabilityStatsScores` — record → drain → clamped score
  windows end to end.
- `TestClientReliabilityScoreSharedIpNormalization` — locks the stats-valid
  denominator semantics; found the pgx scan bug.
- `TestReliabilityScoreStaleRowsRemoved` — upsert + stale-delete refresh across
  all three score tables.
- `TestRemoveDisconnectedCascadesReapedClients` — targeted cascades:
  provide_key + redis, tls certificate, proxy config chain, device incl.
  shared-device survival, and the connection's location/latency/speed rows.
- `TestSweepOrphanConnectionAndClientData` — safety-net sweep of orphan
  location/latency/speed, tls, and device rows, live parents untouched;
  `TestSweepOrphanClearsProvideRedis`, `TestSweepOrphanClearsProxyConfigRedis`,
  `TestSweepOrphanReapsProxyClients` (moved to the safety-net sweep).
- `TestProxyDeviceConfigCacheAndFallback` — extended: explicit
  RemoveProxyDeviceConfig also cascades the proxy_client rows.
- `TestRemoveCompletedContractsCascades` — full paid-payment lifecycle, one
  pass leaves no orphans, open contracts survive, balances removed once
  end_time passes retention; `TestSweepOrphanContractData` — orphan drain with
  batch looping.
- Index-affected pre-existing queries re-validated against the new index set:
  `TestForceCloseDisputedContract` (dispute scan on the partial index),
  `TestGetOpenTransferByteCount` (payer SUM after the redundant index drop),
  `TestEscrow`/`TestSettleContractCheckpointPlusClose`/
  `TestGetOpenContractIdsWithPartialCloseCheckpointPlusClose`
  (open_source_id shapes + the force-close poll on the open partial).
- `TestAuthVerifyCodeInvalidation` — only the latest code is live, invalidated
  codes don't verify, retention reaps aged rows.
- Integration: `TestTask`, api spec conformance, `TestProxyContractChurnLoad`,
  `TestConnectNoNack` (full stack over the redis announce path).

## Deploy notes

- Pre-create `transfer_contract_open_partial_create_time` and
  `transfer_contract_dispute_partial_create_time` with
  `CREATE INDEX CONCURRENTLY` out of band; the migrations are
  `IF NOT EXISTS` no-ops once they exist (same pattern as the provider-stats
  indexes). The other new indexes are on small/bounded tables and run inline.
  The `DROP INDEX` migrations (`pending_task_available_block`,
  `transfer_contract_dispute_create_time`,
  `transfer_contract_payer_network_id`) take only a brief lock.
- Deploy the taskworker first (schedules `rollup_client_reliability_stats`,
  `sweep_orphan_*`), then connect/api hosts. During cutover, blocks written by
  both paths are overwritten by the drain with redis-only counts (~2 blocks of
  slight undercount).
- Redis: ~2–3 live block buckets at a time (very roughly 50–150MB transient at
  current provider counts; ~5–20k ops/sec against the current block's hash
  key). Buckets carry a 15-minute TTL backstop.
- `SELECT pg_stat_statements_reset()` after deploy to re-baseline.

## Remaining candidates (not done)

- The scheduled whole-database `ANALYZE` (7min avg × 205) → targeted per-table
  analyze after big deletes.
- `client_id → network_id` lookups (17B + 7B calls, ~1.4%): immutable per
  client — cacheable in-process or derivable from the JWT.
- Time-partitioning the `transfer_contract` family by `create_time` would turn
  retention into `DROP PARTITION` and keep the hot tables small.
