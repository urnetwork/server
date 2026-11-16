# OPEN-SOURCE-ID-RETIRE — dropping transfer_contract_open_source_id for good (design, 11-16)

The owner, 11-16 about 15:15Z: "redesign the queries that use it if possible, drop it for good."
This is the design and the rollout. It is not implemented yet: it adds a table, a trigger on
transfer_contract and a sweep, so it needs its own implementation slot and the owner's go on the two
decisions in §5.

## 1. What still needs the index

`transfer_contract_open_source_id (open, source_id, destination_id, contract_id)`: 323 GiB on 11-16,
about 1,200 scans a second. Every reader of open contracts by pair or endpoint already uses the
unresolved pair partials (the CASE predicate). Two families read its **closed** rows (`open = false`),
and nothing else serves them:

1. **Lingering origins.** A closed origin keeps anchoring companions until its deadline
   (`CompanionOriginCloseLinger`, `model/subscription_contract_expiration.go`). The reads are
   `open = false AND $3 <= close_time AND contract_id >= <unexpired floor> AND unexpired AND
   source_id = $1 AND destination_id = $2`, at:
   - `model/subscription_model.go:2305` (`companionOriginExistsSql`, about 940 calls/s);
   - `model/subscription_model.go:2449, 2483` (the plain origin selection and its prober maximum);
   - `model/subscription_model.go:2567, 2613` (the chained origin selection and its prober maximum);
   - `model/subscription_zero_escrow.go:123, 181` (the zero-escrow origins).

   The callers are the companion paths in `controller/connect_controller.go`, served by api, alt,
   proxy and taskworker.
2. **Prober shard retention.** The contract window (`model/prober_shard_retention.go:1076`) reads
   `open = false AND source_id = client` for each client of a dropped pass.

The nearest other index, `transfer_contract_destination_id_close_time`, would read a destination's
whole recent fan-in: the top destination takes 0.38% of contracts, about 18,800 an hour.
`TestCompanionClosedOriginReadsNeedTheClosedPairIndex` pins this.

## 2. Lingering origins: a trigger-kept side table

```sql
CREATE TABLE contract_origin_linger (
    source_id uuid NOT NULL,
    destination_id uuid NOT NULL,
    contract_id uuid NOT NULL,
    -- COALESCE(expiration_time, create_time + 60 min) when the row was written; the sweep's key
    prune_time timestamp NOT NULL,
    PRIMARY KEY (source_id, destination_id, contract_id)
);
CREATE INDEX contract_origin_linger_prune_time ON contract_origin_linger (prune_time);
```

**The trigger.** It is on transfer_contract: `AFTER INSERT OR UPDATE OF outcome, dispute, close_time,
expiration_time`, with a WHEN clause so other updates never enter plpgsql.
- It upserts a row when the row is closed (`open = false`), its `close_time` is set, and it closed
  before its deadline (`COALESCE(expiration_time, create_time + 60 min) > close_time`).
- Only such a row can ever satisfy the closed arm: that arm needs an unexpired deadline at a time
  after the close.
- It covers every path that closes a contract:
  - the settlements at `subscription_model.go:3825, 3838, 5004`;
  - the dispute paths at `:4433` and `subscription_contract_expiry_continuation.go:193`;
  - `subscription_close_owner.go:93`;
  - any path added later.
- At about 1,300 closes a second the table holds about 3–4M rows, under 1 GiB.

**The reads.** Each closed arm reads the side table, then the contract by its primary key, and keeps
every original predicate, so the side table is only an access path:

```sql
FROM (SELECT contract_id FROM contract_origin_linger
      WHERE source_id = $1 AND destination_id = $2 AND prune_time > statement_timestamp() AT TIME ZONE 'UTC'
      OFFSET 0) AS linger
JOIN transfer_contract c ON c.contract_id = linger.contract_id
WHERE c.open = false AND $3 <= c.close_time AND c.contract_id >= <unexpired floor> AND <unexpired> ...
```

A row that later reopens, for example a dispute resolved back to open, fails `c.open = false` and is
read by the open arm.

**The sweep.** A taskworker task deletes rows past `prune_time`:
- in batches on `contract_origin_linger_prune_time`, every minute;
- batch size and interval come from its settings object (CODESTYLE);
- the rate is about one delete per close.

**The DDL needs an operator step.** CREATE TRIGGER takes SHARE ROW EXCLUSIVE on transfer_contract,
which queues writers behind it and waits for any vacuum.
- The operator creates it by hand in a vacuum gap with `lock_timeout = '500ms'`, retrying, never behind
  an anti-wraparound pass (the stepd_after_vac.sh pattern).
- The migration then records it: a catalog read once the trigger exists, as the registration_hash
  record does.
- The table, its indexes and the function are additive and go in the migration.

## 3. Prober retention: two options

- **A, the window over the generation's contract ids.**
  - Walk the primary key over the dropped generation's day (contract ids from 15 min before the day to
    15 min after), keeping contracts whose payer is one of the generation's pass networks.
  - This needs no new table. It reads each contract of the day once, about 1,350 rows a second at
    today's load. That is about as much as the horizon reaper's scan, and the heap reads are mostly
    cold.
  - It replaces both arms, origins and companions, because every prober contract's payer is its pass's
    network (`validateProberShardPayerInTx`).
- **B, a registry written at creation.**
  - `prober_shard_contract (network_id, contract_id)`, inserted in the prober's contract creation
    transaction, about 134 a second, and deleted with the contracts.
  - The retention reads by network. About 93M rows for eight days, about 9 GiB.
  - Precise and cheap to read; one more write on the prober's creation path.

## 4. Rollout (each production step with the owner's go)

1. **Migration:** the table, its indexes and the trigger function.
   - The operator creates the trigger by hand (§2), then db migrate records it.
   - From that moment every closing contract fills the side table.
2. **Wait at least 75 minutes,** the longest lingering window plus the id margin. After that the side
   table holds every lingering origin, with no backfill.
3. **Roll the readers:** api, alt, proxy and taskworker. The taskworker roll also brings the sweep and
   the prober retention rewrite.
4. **Check that nothing reads the index any more.** Its `idx_scan` should grow only by planner
   activity for a day that includes a prober generation drop. Run plan proofs on a seeded copy with
   the index dropped.
5. **The owner drops it by hand,** with `DROP INDEX CONCURRENTLY` (stepd_after_vac.sh: 30 s lock
   waits, never queued behind a vacuum). That frees about 323 GiB.
   - Every transfer_contract vacuum pass then reads 28% less index: 1,137 to 814 GiB.
   - Every contract writes two index entries fewer.
6. **Merge the record migration** for the drop, at the next free number.

## 5. Decisions and tests

**Owner decisions:**
- Option A or B for the prober retention (§3).
- Accept a trigger on transfer_contract and the operator-run CREATE TRIGGER (§2).

**Tests**, under connect/CODESTYLE.md:
- **The trigger:**
  - an early close writes the side row; a late close, an expired close and a reopened dispute leave
    the reads exact;
  - each update path in §2 is covered;
  - an update that closes nothing never enters the function.
- **Root cause** (the 323 GiB index served only closed-pair reads): with
  `transfer_contract_open_source_id` dropped, the companion origin reads still find a lingering
  origin, and still refuse an expired or late-closed one.
- **Plan proofs on seeded fixtures,** without the index and with sequential scans off:
  - each closed arm reads the side table's primary key and transfer_contract_pkey;
  - the prober retention reads only a primary-key window (option A) or the registry (option B).
- **The sweep:** bounded batches, the prune time honoured, settings-driven.
- **The monitor:** the table's and the trigger's catalog contracts; the drop record's contract.
- **The merge set:** ./grafana covers any new metric, and the alert classes are classified.
