# RELIABILITY2 — client reliability stats: redis hot-path optimization plan

STATUS 2026-07-20: §3.1–3.3 IMPLEMENTED, sharded writes UNCONDITIONAL.
The interim `client_reliability_sharded_writes` writer gate was removed
(FIXPLAN1.md decision 2): the 2026-07-18 rollout completed everywhere —
every taskworker runs the shard-aware drain and every fresh environment
stands up both services from the same build, so a gated legacy writer only
created a mis-provisioning hazard (an env without the setting would silently
re-concentrate the fleet's counters on one node). The rollup keeps the §4
legacy-key transition read for counters written by pre-shard builds; rollback
of a taskworker to a pre-shard build remains forbidden (an old rollup cannot
read shard hashes and would orphan them). Tests:
TestClientReliabilityStatsShardedWriter (writes land only in shard keys),
TestClientReliabilityStatsShardedDrain
(multi-shard record → merge-drain → absolute pg rows → keys removed),
TestClientReliabilityStatsLegacyDrain (rolling-deploy mix of legacy ascii +
sharded packed fields in one block, distinct clients),
TestClientReliabilityStatsDeployMixedDrain (the full deploy timeline: a
pre-deploy legacy-only block, then a mid-deploy block where ONE client's
syncs land on both an old-build and a new-build process — overlapping
counter indexes SUM across forms into a single pg row, disjoint indexes
coexist — plus legacy-only and new-only clients, re-drain idempotency, and
removal of every key form of every block),
TestClientReliabilityStatsShardFunction, plus the full pre-existing
Reliability suite — all passing under -race.
§3.4 (per-process coalescing) remains a follow-up per its own sequencing:
land + measure 3.1–3.3 first. §3.5 unchanged (leave cadence alone).

## 1. Why

The reliability stats pipeline is the standing top redis load, measured on
main 2026-07-17/18 (monitor/SIGNALS.md 3.7):

- ~7,500 `HINCRBY`/s + ~1,800 `MULTI/EXEC`/s concentrated on ONE node — the
  node owning the current minute-block's single hash key.
- The rollup's whole-hash `HGETALL` runs 200–290ms per block (the worst
  entries in every slowlog), holding that node's entire event loop — every
  co-located key stalls behind it.
- During the 2026-07-17 redis outage aftermath, score/reliability rebuild
  tasks ground for hours competing with this hot path (the stale-selection
  incident, SIGNALS.md 5.9); shrinking this contention shortens those
  worst-case runs.

The design's bones are right and stay: redis buffering replaced per-sync pg
upserts that were "the single largest statement load on the database", and
the block-finality + high-water-mark machinery gives idempotent drains. The
costs are concentration artifacts.

## 2. Current shape (model/network_client_reliability_model.go)

- Every connected provider's announce loop calls
  `RecordClientReliabilityStatsRange` every ~30s (half a 60s block): one
  `MULTI` of up to 8 `HINCRBY` + `EXPIRE` into ONE hash per minute block,
  `client_reliability_stats.<blockNumber>` (no `{...}` hash tag → one slot
  per block; the hot node rotates each minute).
- Field = `<64-hex addr hash>:<networkId>:<clientId>:<counterIdx>` (~140
  bytes, repeated up to 8x per client per block).
- `RollupClientReliabilityStats` (serial task): `SMEMBERS` the pending-block
  set, per final block one whole-hash `HGETALL`, parse in Go, chunked pg
  upserts into `client_reliability`, `DEL` + `SREM`. A block drains only
  after 2 full blocks elapse (the recorder writes only current + previous
  block), so drains see final data and re-drains are idempotent.

## 3. The optimizations, in order

### 3.1 Shard the block hash across slots (highest value, mechanical)
Key becomes `client_reliability_stats.<block>.<shard>`,
`shard = clientId last byte mod 32`. Effects:
- write load spreads across ~32 slots/nodes instead of one;
- each drain read is 1/32 the size;
- a client's counters stay in one shard, so per-client aggregation at drain
  and the `valid` computation are untouched;
- each writer call still targets exactly one key — the `TxPipelined` batch
  stays cluster-safe.
The pending-blocks set and finality rule carry over unchanged; the drain
loops over the 32 shard keys per block.

### 3.2 Drain with chunked HSCAN, not HGETALL
A drained block is final (no writers), so `HSCAN` (COUNT ~5000) is a
consistent read, and no single command holds the owning node's event loop
for the whole hash. Kills the 200–290ms slowlog class outright. The drain is
a background task — the extra round trips are free.

### 3.3 Compact the field names
Packed binary fields: `<addr hash 32B><networkId 16B><clientId 16B><idx 1B>`
= 65 bytes vs ~140 ascii — roughly halves hash memory, network transfer, and
parse cost. The drain parser accepts BOTH forms during transition (binary =
fixed length 65; legacy = 4-part `:` split), producing the same row keys
downstream so the pg upsert is untouched.

### 3.4 Per-process coalescing (structural follow-up, separate change)
Each connect/api process accumulates its clients' counters in memory and
flushes every few seconds as bulk pipelines grouped by shard — turning ~7.5k
tiny MULTIs/s fleet-wide into tens of bulk writes/s. This is the big redis
CPU + connection-pool win on the announce path (the same path whose
goroutine coupling produces the pg idle-in-tx mirror, SIGNALS.md 1.3).
Trade-off: a process crash loses up to one flush interval of stats —
acceptable for best-effort stats (the recorder already swallows redis errors
by design) — and it adds an aggregator with shutdown-flush, so it is a real
piece of engineering vs the mechanical 3.1–3.3. Do it after 3.1–3.3 land and
are measured.

### 3.5 Leave the cadence alone
Halving the 30s report rate looks tempting but the half-block cadence is
what guarantees every block sees each client's report despite jitter, and
the stale-block drop rule ("recorder > 1 block behind drops the sync") gets
riskier at 60s. Revisit only if 3.1–3.4 prove insufficient.

## 4. Rollout safety (rolling deploy, two writer generations live)

During a rolling deploy, old-build writers fill the legacy unsharded key.
The shard-aware drain reads, per block, that legacy key PLUS all 32 shard
keys, merges them before the upsert, and deletes all forms. This supports
old-writer/new-reader overlap and mixed field encodings.

The reverse overlap is not safe: an old taskworker reads only the legacy
hash, can mark a block drained, and can orphan shard data. The 2026-07-18
rollout followed exactly this ordering (shard-aware taskworkers first, then
sharded writers); with the rollout complete the interim writer gate was
removed and sharded writes are now unconditional (FIXPLAN1.md decision 2).
What remains operative:

1. Rollback of any taskworker to a pre-shard build is FORBIDDEN — an old
   rollup cannot read shard hashes and silently orphans them to the TTL.
2. The legacy drain read stays until the rollback horizon has passed and no
   pre-shard writer can exist; it also covers any residual legacy-keyed
   counters.

The
15-minute Redis TTL is not a safety mechanism: an old reader can remove the
shared pending marker and make shard data undiscoverable before that TTL.
If unsafe mixed generations ever ran, audit every affected block rather than
assuming the loss was bounded.

The 2026-07-18 canary demonstrated the expected performance shape once shards
were active (top sampled-node HINCRBY rate fell from 7,014/s to 165/s), but it
does not replace the ordered gate protocol above.

## 5. Verification

- SIGNALS.md 3.7 before/after: per-node `HINCRBY` rate spreads (no node ≫
  fleet), `zrangebyscore`/`hgetall` slowlog entries for
  `client_reliability_stats.*` disappear, `--latency` on former hot nodes
  unchanged-or-better.
- `TestRecordClientReliabilityStatsRollup` (exercises record → drain → pg
  end to end), `TestAddClientReliabilityStats*`, drain-gap/coverage tests —
  all under `-race` (./test.sh).
- Monitor task-overdue: rollup + score task durations should shorten under
  contention.

## 6. Validated implementation notes (from the backed-out draft)

- `server.RedisClient = redis.UniversalClient` — `HScan` is available.
- `server.RequireIdFromBytes` exists for decoding packed fields.
- `clientReliabilityStatsKey` has exactly two call sites (writer + drain);
  nothing else in model/ or monitor/ constructs the key name.
- The monitor doc/probes reference the key SHAPE in SIGNALS.md 3.7 — update
  the family text there when the sharded names ship (the histogram
  normalizes trailing numbers, so `.<block>.<shard>` groups fine as-is).
