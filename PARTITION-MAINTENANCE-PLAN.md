# Partitioning and maintenance plan: ongoing bloat

Status: PLAN with the owner's final decisions (2026-10-24); implementation in
progress. Written 2026-10-23 during the edge-2 disk-full outage, then revised
after a code review of each table and a design review
(`PARTITION-MAINTENANCE-PLAN-REVIEW.md`), whose findings are folded in
throughout. Each section marks its **Decided** choice; "Final decisions" below
lists them together.

Companion to `CONTRACT-PARTITION.md`, which covers the six contract tables
(built, not cut over). This plan covers the rest of the database, plus the
`db_maintenance` changes that keep bloat from coming back.

**Owner constraint:** no `pg_repack` or similar external tool. Online table
compaction waits for the built-in support in PostgreSQL 19, which main will
upgrade to soon. Until then, a table shrinks only in a maintenance window
(`VACUUM FULL`, as on 2026-10-23). Index rebuilds stay online with core
`REINDEX CONCURRENTLY`, as the rotation does today.

Sizes are decimal GB, as SIGNALS.md §5.14 measures them.

## Final decisions (2026-10-24)

| Item | Decision |
|---|---|
| A4 | Online compaction is deferred until PostgreSQL 19, at least a month away. The bloat measurement and its signal are built now. |
| A5 | Autovacuum pacing, workers, memory and the A1 per-table settings are coded in `playbook-dbs.yml` behind their own tag, and applied only when the owner runs it. |
| A6 | `transaction_timeout` of 15 minutes for the application role, after the long-reader inventory, plus the horizon signal. |
| C2 | `provider_egress_health_history`: daily partitions, 7 days. |
| C3 | `finished_task`: built. Daily partitions kept 8 days, dropped behind the owner fence. |
| C4 | `provider_work_session_event`: weekly partitions, dropped after 13 weeks. |
| C5 | `contract_close_report_evidence`: weekly `report_id` partitions dropped once they expire, with the built row deletes kept for the rest. |
| C6 | `st_provider_usage_archive`: weekly partitions by contract week, 1-year retention. |
| D | Retired hosted identities and their provide keys are hard-deleted after 1 day. Key history of a client that is gone is deleted 91 days after it's gone. The mint rate is a product question outside this plan. |
| Contract-dependent work | A7's reuse of the contract checks and retiring `legacy_settlement_intent` go ahead now: the contract-partition review fixes are merged (`09cd98565`). |
| `search_projection` | Left as is. |
| `search_provider_stats` | Change guard only; the open hour keeps its one-minute rollup. |

## Summary

The 2026-10-23 VACUUM FULL separated main's tables into three kinds. Each
needs a different fix.

1. **Churn tables** hold a small live set that is rewritten constantly: the
   task queue, journals, probe and score tables, and the connection table.
   - Almost all of their size was dead. Vacuum fell behind.
   - Their time-ordered indexes kept pages that vacuum cannot give back.
   - The REINDEX rotation dropped them once they bloated.
   - Partitioning does not fix this. `db_maintenance` (Part A) and the write
     paths that create needless row versions (Part B) do.
2. **Append-only history that nothing deletes** is live, not bloated. Examples:
   - `provider_egress_health_history`, which has no retention;
   - `provider_work_session_event`, which is guarded against deletes;
   - `contract_close_report_evidence`, whose deletes start in January.

   Partitioning makes their retention a partition drop (Part C).
3. **Identity churn.** `network_client`, `provide_key` and
   `st_client_key_history` hold about 1.1 TB, mostly live. They grow with
   hosted window identities and need retention, not partitioning (Part D).

## Evidence (2026-10-23)

With the edges down, VACUUM FULL reclaimed 266 GB across 299 tables by 21:41Z.
The five largest tables wait for the storage expansion: `transfer_escrow`,
`transfer_contract`, `contract_close`, `st_client_key_history` and
`network_client`.

Sizes are heap, toast and indexes, before and after the rewrite. "Reclaimed"
is the difference between the two: dead rows, free space and index bloat
together.

| Table | Before | After | Reclaimed |
|---|---|---|---|
| `pending_task` | 87.0 GB | 0.6 GB | 99% |
| `finished_task` | 44.5 GB | 3.4 GB | 92% |
| `legacy_settlement_intent` | 33.6 GB | 0.0 GB | 100% |
| `network_client_connection` | 21.7 GB | 2.6 GB | 88% |
| `search_projection` | 17.1 GB | 9.2 GB | 46% |
| `device` | 12.4 GB | 1.6 GB | 87% |
| `provider_url_probe_run` | 7.4 GB | 5.4 GB | 27% |
| `search_provider_stats` | 7.3 GB | 4.3 GB | 41% |
| `network_client_location` | 7.2 GB | 2.1 GB | 71% |
| `transfer_balance` | 6.0 GB | 3.3 GB | 45% |
| `transfer_debit_journal` | 2.1 GB | 0.0 GB | 99% |
| `client_connection_reliability_score` | 1.6 GB | 0.1 GB | 94% |
| `provider_egress_probe_cycle` | 1.6 GB | 0.2 GB | 87% |
| `user_auth_attempt` | 1.0 GB | 0.0 GB | 100% |
| `audit_device_event` | 33.0 GB | 31.0 GB | 6% |
| `provide_key` | 225.6 GB | 194.7 GB | 14% |
| `provider_egress_health_history` | 134.1 GB | 130.4 GB | 3% |

Lifecycles, measured on the compacted tables:

- **`finished_task`:**
  - about 3.1M rows a day, kept about 29 hours;
  - creation to finish: p50 7 minutes, p99 11.5 hours, maximum 96 days.
    Run-once tasks keep their `task_id` when rescheduled.
- **`pending_task`:**
  - 870k rows after the 9-hour outage, almost all due;
  - the oldest task was created 2026-08-16.
- **`network_client_connection`:** about 3.2M connections a day, deleted 8
  hours after disconnect.

Settings on main (PostgreSQL 18):

- **Autovacuum:**
  - `autovacuum_max_workers = 6` (changed by reload) and
    `autovacuum_worker_slots = 16` (changed by restart).
  - Global `autovacuum_vacuum_cost_delay = 0`, so workers are unthrottled
    unless a table sets its own delay.
- **Per-table settings:**
  - On 2026-07-13, nine tables went back to the database default (v494–v502).
  - Settings were then re-added where measured:
    - `pending_task`, with a fixed threshold of 50. At default settings its
      poll query took 39% of all database time.
    - `device`.
    - 2 ms pacing for `contract_close`, `transfer_escrow`,
      `transfer_escrow_sweep` and `transfer_contract`. Three concurrent
      unthrottled vacuums of multi-hundred-GB tables had evicted the buffer
      cache under live traffic, with 130+ backends in BufferMapping waits.
  - Every other large table vacuums unthrottled.
- **`vacuum_truncate = off`** server-wide. A plain vacuum makes space reusable
  inside a table but never returns it to the filesystem.
- **Timeouts:**
  - `statement_timeout = 0` server-wide; each caller sets its own.
  - `idle_in_transaction_session_timeout = 60s`, from `postgresql.conf`.
    `migrationSetIdleInTransactionTimeout`'s database-level 5-minute setting
    is not present on main, and the file's value is stricter anyway.
  - `transaction_timeout = 0`.
- **`db_maintenance`** runs daily:
  - a REINDEX rotation that rebuilds each table once per 8 runs;
  - opt-in rebuilds of seven named contract partial indexes when bloated
    (`priorityReindexIndexes`);
  - one explicit `VACUUM (ANALYZE, SKIP_LOCKED) pending_task`;
  - a final database-wide `ANALYZE`.

  Nothing else vacuums on a schedule.
- **Dead-tuple warning (SIGNALS.md §2.4):** it fires only above 10M dead
  rows, and treats an old `backend_xmin` horizon as context, not as a trigger
  of its own.
- **Extensions:** `pgstattuple` is installed.

## Why the churn tables bloated

1. **Vacuum fell behind, or could not remove rows.**
   - Six autovacuum workers serve a database of several TB. Passes over the
     largest tables hold a worker for hours, so small tables wait.
   - A dead row is removable only after every older transaction ends. Idle
     transactions stop at 60 s, but neither statements nor transactions have
     a server-wide bound. Any long reader holds back cleanup in every table:
     - multi-minute usage and stats reads;
     - the contract orphan sweep, which never completed;
     - an operator's VALIDATE, REINDEX or dump.
2. **Time-ordered indexes keep pages vacuum cannot free.**
   - A btree page is freed only once every entry on it is dead. New keys land
     at the right edge, so a page holding one long-lived row is never reused.
   - Vacuum cannot shrink these indexes. Only a rebuild can.
   - Examples:
     - `pending_task`'s poll indexes are keyed by `available_block`, a time.
       On 2026-10-21 it had 94 GB of indexes for about 200 MB of rows
       (SIGNALS.md).
     - `finished_task_run_end_time`.
     - The connection table's time and flag indexes.
     - `transfer_debit_journal`'s age index.
3. **The rotation drops a table once it bloats.**
   - The REINDEX rotation skips tables over 16 GB
     (`DefaultDbMaintenanceMaxReindexTableBytes`).
   - It measures them with `dbMaintenanceTableBytesQuery`: heap, toast and
     indexes as they are on disk, bloat included.
   - So `pending_task` (87 GB), `finished_task` (44.5 GB) and
     `network_client_connection` (21.7 GB) left the rotation exactly when they
     needed a rebuild.
   - `dbMaintenanceSkipReindexTables` also lists `network_client_connection`
     permanently, as partition-managed per STORAGE.md. It is a plain table.
4. **Write paths make row versions they don't need.** Part B lists them:
   - updates that touch an indexed column and so cannot be HOT;
   - upserts with no change guard;
   - 32 task chains that re-arm every 2 s when idle.

A queue does not benefit from partitioning: its partitions still need vacuum,
and a partition can be dropped only once every row in it is gone.

## Part A: `db_maintenance`

### A1. Vacuum the churn tables every 5 minutes

Add a run-once task, `vacuum_hot_tables`, so only one run is active at a time.
Schedule it in `taskworker/work/maintenance_work.go` and implement it beside
`DbMaintenance` in `db_maintenance.go`.

- **Each run:** for every table on the list, `VACUUM (SKIP_LOCKED) <table>` on
  an autocommit maintenance connection. Once an hour, also ANALYZE.
  - SKIP_LOCKED keeps it from queueing behind, or cancelling, an autovacuum.
    See the 2026-10-13 note at `dbMaintenanceVacuumPendingTaskStatement`.
  - Each statement gets a 2-minute timeout, and the run a 4-minute budget.
  - A run that stops early resumes at the next table, so every table gets
    its turn.
  - Session settings changed for the run are reset before the connection goes
    back to the pool.
- **The list, from the evidence:**
  - `pending_task`, `finished_task`
  - `network_client_connection`, `network_client_location`, `network_client_handler`
  - `transfer_debit_journal`, `transfer_balance`
  - `client_connection_reliability_score`
  - `provider_egress_probe_cycle`, `provider_url_probe_run`
  - `device`, `search_provider_stats`
- **Output:**
  - Per table, the dead rows removed and the dead rows not yet removable,
    from the `VACUUM (VERBOSE)` summary through a notice handler.
    `pg_stat_progress_vacuum` does not report the second number.
  - Each table's last successful vacuum. A table skipped or timed out for an
    hour raises a warning: a vacuum that started is not one that finished.
  - Rows not yet removable mean the horizon (A6) is the problem. The task
    then logs the oldest holder instead of vacuuming harder.
- **Retire the daily step:** remove the `pending_task` vacuum from
  `DbMaintenance`. This task supersedes it.
- **Tests:**
  - a locked table followed by an eligible one: the skip must not stall the
    run;
  - a statement that times out on every run: the warning fires, and later
    tables still get their turn;
  - a pinned horizon: rows are reported as not yet removable, and the holder
    is logged.

An explicit vacuum does not need an autovacuum worker, so a pass over a large
table can no longer starve these tables. Compact, they are each under 10 GB,
so a pass over all of them takes seconds. A plain vacuum stops the heap from
growing, but does not shrink a time-ordered index (A2).

### A2. Rebuild a churn table's bloated indexes

The priority rebuild already does the right thing for seven contract indexes:

- it measures each index against its estimated compact size
  (`dbMaintenanceEstimateBtreeBytes`);
- it runs `REINDEX INDEX CONCURRENTLY` once an index is at least twice that
  size (`dbMaintenancePriorityIndex.bloated`).

It is off unless `db_maintenance.yml` sets `priority_reindex_enabled`.

Extend it to every index of the A1 tables:

- Measure on the hot-vacuum task's hourly pass. Rebuild indexes that are at
  least 2× their compact estimate and have at least 64 MB to reclaim, so tiny
  indexes are not rebuilt every hour.
- **On by default for this list.** A compact churn table's rebuild takes
  seconds. The large contract indexes keep their opt-in.
- **Same safeguards as the rotation:**
  - wait for the table's lock without queueing (`dbMaintenanceAwaitTableUnlocked`);
  - defer under load or long transactions (`dbMaintenanceDeferReason`);
  - drop `_ccnew` debris after an interrupted rebuild.

### A3. Fix the rotation's size bound and skip list

- **Bound:** measure what a rebuild reads and writes:
  - the heap and toast it scans;
  - the estimated compact size of the indexes it builds.

  Not the current index sizes, which the rebuild throws away. A table then
  stays eligible while it is bloated.
- **Skip list:** remove `network_client_connection` from
  `dbMaintenanceSkipReindexTables`. It is 2.6 GB compact, and its indexes are
  on the A2 list as well.
  - Keep the partitioned families (`client_reliability`, the contract tables).
  - Keep `network_client` and `network_client_location_reliability` until
    Parts B and D shrink them.
- **Comments:** the skip list's comment and the one in `db_migrations.go`
  still plan "a one-time pg_repack". Replace both with the owner's rule: a
  maintenance-window VACUUM FULL until PostgreSQL 19, then its built-in online
  compaction (A4).

### A4. Measure bloat now; compact online after PostgreSQL 19

- **Measure weekly, under a time budget:**
  - **Heap:** `pgstattuple_approx` for tables under 50 GB. It skips
    all-visible pages but reads every other page, so a heavily updated table
    still costs a scan. Resume next week where the budget ran out, and record
    the share scanned.
  - **Indexes and TOAST:** measured separately. `pgstattuple_approx` covers
    only the relation it is given. Indexes use the btree estimate the
    priority rebuild already uses.
  - **Tables over 50 GB:** the statistics estimate the 2026-10-23 driver used
    (live rows × average width, against heap size).
  - Subtract the free space a fillfactor leaves on purpose.
  - Keep the raw measurements, so thresholds can be checked against them.
- **Monitor:**
  - Add a bloat signal that scales with table size: reclaimable share, or
    dead rows against live rows.
  - The §2.4 warning starts at 10M dead rows. A queue of a few hundred live
    rows stays under that while its indexes grow to 94 GB.
- **Until PostgreSQL 19:**
  - A1–A3 and Part B stop new bloat.
  - Existing heap bloat is reused in place, or removed in a maintenance window.
  - Nothing external (owner decision).
- **After the upgrade, `REPACK (CONCURRENTLY)`.** The PostgreSQL 19
  documentation sets the requirements for running it on main:
  - **Disk:** room for a copy of the table and all its indexes, plus every
    change made during the run, which it stores on disk separately. Add the
    WAL of the copy, beyond the disk reserve.
  - **Horizon:** it counts as one long transaction for its whole run, so it
    holds back cleanup in every other table: the horizon A6 bounds. Watch its
    `backend_xid`.
  - **Visibility:** it is not MVCC-safe. Once it commits, a transaction whose
    snapshot predates the run sees the table as empty. `server.Tx` defaults to
    REPEATABLE READ, so any application transaction that outlives a run and
    first reads the table after the swap is exposed.
  - **Locks:** SHARE UPDATE EXCLUSIVE for most of the run, so no vacuum or
    concurrent index build on that table meanwhile. Then ACCESS EXCLUSIVE for
    the swap. Concurrent DDL can make it fail.
  - **Eligibility:** not a partitioned parent (only its leaves), and not a
    table without a primary key or an index-based replica identity. It runs
    outside a transaction block.
  - **Replication:** it captures changes with logical decoding through a
    replication slot (`max_repack_replication_slots`). Confirm what it needs
    from `wal_level` at the upgrade.

  Pin the release's behavior at the upgrade. Then define eligibility,
  headroom, lock and runtime bounds, cancellation and retry, and horizon
  monitoring while it runs.

  Start by reporting candidates for an operator. Because of the visibility
  hazard, run it with the application drained, or only on a table that no
  long REPEATABLE READ transaction reads. Automate only after that: one table
  at a time, under the rotation's load deferrals, with its own budget apart
  from routine vacuum.
  - **Decided (2026-10-24): deferred.** PostgreSQL 19 is at least a month
    away, so compaction and any automation of it wait for the upgrade. The
    measurement and the bloat signal above are built now.
- **Hand-off:** a table over 50 GB with persistent bloat goes to Part C or D,
  not to repeated compaction.

### A5. Autovacuum pacing, then capacity

More unthrottled workers would repeat the July buffer-cache eviction. So
change the pacing first, then add workers:

1. **Pacing:**
   - Set the global `autovacuum_vacuum_cost_delay` back to 2 ms, the
     PostgreSQL default.
   - Raise `autovacuum_vacuum_cost_limit` so the shared budget still keeps up.
   - Give the A1 tables `autovacuum_vacuum_cost_delay = 0` per table, as
     `pending_task` and `device` have.

   Every large table then vacuums paced by default, including those that set
   nothing today: `network_client`, `st_client_key_history`, `provide_key`,
   `provider_work_session_event` and `contract_close_report_evidence`.

   After the change, watch anti-wraparound progress on the large tables:
   `age(relfrozenxid)` against `autovacuum_freeze_max_age` (200M).
2. **Workers:** raise `autovacuum_max_workers` from 6 to 10–12. This is a
   reload, since `autovacuum_worker_slots` is already 16.
3. **Thresholds:** give the A1 tables fixed thresholds:
   - `autovacuum_vacuum_scale_factor = 0` and
     `autovacuum_vacuum_insert_scale_factor = 0`;
   - base thresholds near their hourly churn.
4. **Memory:** set `autovacuum_work_mem` to 1 GB. Today each worker inherits
   `maintenance_work_mem`, 2 GB.

A setting change alone doesn't show it worked. Judge the result by measured
pass durations, dead-row growth, wraparound margin, and whether the A1 tables
keep finishing their vacuums.

Check each table's trigger arithmetic too. A table qualifies once its dead
rows pass min(`autovacuum_vacuum_max_threshold`, base threshold + scale factor
× rows). At scale 0.2, the 100M cap applies to every table over 500M rows, so
raising a large table's base threshold alone changes nothing.

**Decided (2026-10-24): code it, apply later.** The global pacing, worker count
and memory go into the PostgreSQL config that `playbook-dbs.yml` manages. The
A1 per-table settings go into a task beside it. Both sit behind their own tag,
so nothing changes until the owner runs that tag, as a trial after the storage
expansion with the §2.4 signals watched.

### A6. Bound the transaction horizon

- **Timeout:** set `transaction_timeout` for the application role, for example
  `ALTER ROLE bringyour SET transaction_timeout = '15min'` (PostgreSQL 17+).
  - It applies only to new sessions. The pools keep a connection up to 8
    hours plus 15 minutes of jitter (`db.go`), so recycle them, then check
    `SHOW transaction_timeout` through both the application and maintenance
    pools.
  - It ends the session, not only the statement. Check that callers roll
    back, reconnect and retry correctly.
- **Exemptions per operation, not per pool:**
  - The maintenance pool also carries ordinary application work
    (`MaintenanceTx`, `MaintenanceDb`).
  - So a long maintenance operation raises the timeout for its own session
    only, and gets its own horizon alarm.
- **Before setting it:** inventory the long readers.
  - Callers that set `statement_timeout` are in `task/`, `taskworker/`,
    `stmonitor/`, `bringyourctl/` and the maintenance code.
  - A reader longer than the bound either moves to the replica once one
    exists, or pages its work, as CloseExpiredContracts does.
- **Monitor:** make an old horizon a signal of its own in SIGNALS.md §2.4,
  instead of only context for the dead-tuple warning.
  - Warn at 15 minutes; page at 1 hour.
  - Count every holder of the horizon:
    - sessions (`backend_xmin`, `backend_xid`);
    - replication slots;
    - prepared transactions.

    If slots or prepared transactions are unused on main, confirm that
    instead.
  - Name the holder, ranked as §2.4 already ranks horizon candidates.
- **Decided (2026-10-24): 15 minutes,** set once the inventory confirms that
  every request and task path fits.

### A7. One partition maintenance registry

Partitioned families today and after this plan:

- `client_reliability` (`MaintainClientReliabilityPartitions`);
- `network_ping` and its tallies: daily by `create_time`, GEOMAP §5.7
  (`model/network_ping_model.go`);
- the contract tables (`MaintainContractPartitions`);
- C2, and later C3–C6.

They share one shape: create partitions ahead, analyze each once its range
closes, drop expired partitions once their preconditions pass, and stay out of
the REINDEX rotation.

Move them into one registry beside `db_maintenance`. Each table declares:

- its key, granularity and how far ahead to create partitions;
- its retention, or "never drop";
- its drop precondition: none for logs, the contract checks for contracts;
- an optional archive step before a drop.

The registry keeps each family's own rules:

- late writes into old ranges;
- leaves that keep changing after their range closes. They need analyzing
  again, and may need index rebuilds, as the contract legacy leaf's priority
  indexes do;
- families that are never dropped;
- the contract checks, as completed by the contract-partition review fixes
  (`09cd98565`): rollback under its locks, and the row reaper's payment
  guards in retirement.

It also needs, for every family:

- **Canonical bounds:** daily partitions start at 00:00 UTC and weekly ones at
  Monday 00:00 UTC, as the contract weeks do, so a range never straddles two
  partitions.
- **Parent statistics:** autovacuum never analyzes a partitioned parent, so
  the registry analyzes each parent on a schedule, not only each leaf.
- **Create-ahead on its own cadence,** apart from slow archive or compaction
  work, and catching up after downtime. An outage must not leave an insert
  with no partition.
- **A drop that rechecks its precondition** in the same transaction that
  detaches the partition.
- **Resume state,** so an interrupted pass continues where it stopped.

A new family does not wait for the registry. It ships with its own
maintenance, as C2 would, and moves into the registry later.

The monitor's runway and blocked-retention probes then cover every registered
table, not only contracts.

**Decided (2026-10-24): build it now, including the contract family.** The
contract-partition fixes are merged.

## Part B: write paths

The changes below stop row versions from being written in the first place.
Each one is small, and each needs a test.

| Table | What writes the dead versions | Change |
|---|---|---|
| `pending_task` | A running task's heartbeat, every 10 s, rewrites `release_time` (`task/task.go:refreshTaskTimestampLeases`). That moves the generated `available_block` column, which is in both poll indexes, so every heartbeat is non-HOT: about 720 versions for a 2-hour task. | Write `claim_time` every heartbeat. Extend `release_time` only when less than half the lease remains: the claim's session advisory lock guards live ownership, and `release_time` only bounds crash recovery. Fillfactor 50–70. |
| `pending_task`, `finished_task` | 16 `flush_transfer_debits_*` and 16 `flush_legacy_settlements_*` chains re-arm every 2 s even when they find nothing. That is at least 16 completions a second, about 1.4M `finished_task` rows a day, each also a `pending_task` insert, claim and delete. The legacy queue is empty. | Back off an idle chain from 2 s to 30 s, and have the enqueue path kick its shard's chain (`KickTasks`) so latency stays low when work arrives. Stop the legacy chains when that table retires. |
| `network_client_connection` | Disconnect flips `connected`, which is in five indexes: a non-HOT update writing six index entries. About 97% of rows are closed and waiting out the 8 hours, and all of them sit in the indexes the open-row reads use. | Partial indexes for open rows: `(client_id, extender_id) WHERE connected`, `(client_address_hash) WHERE connected`, keep `(handler_id) WHERE connected`, and add `(disconnect_time) WHERE NOT connected` for the reaper. The hot indexes then hold about 90k entries instead of 3.3M. Check that each reader's predicate matches. |
| `network_client_location_reliability` (132 GB) | `UpdateClientLocationReliabilitiesInTx` upserts every connected client every 30 minutes with no change guard. It sets the indexed `update_block_number`, so every rewrite is non-HOT. The retention delete is one unbatched statement every 15 minutes. | Add an `IS DISTINCT FROM` guard. Refresh `update_block_number` only when it is about a day old. Fillfactor 85. Batch the delete. Measure how many rows are hosted child identities (`source_client_id IS NOT NULL`), which most readers exclude anyway. |
| `search_provider_stats` | `RollupSearchProviderStats` re-upserts every row of the open hour every minute: about 60 versions per row. A closed hour rolls up once, and its Redis bucket is then deleted. | Add a change guard, so an unchanged row is not rewritten. Fillfactor 70. The open hour keeps its one-minute rollup: a 10-minute cadence would delay current-hour stats, and isn't adopted. Do not regenerate the table: a closed hour's Redis source no longer exists. |
| `device` | The re-auth branch of `authNetworkClientWithParentState` rewrites `device_spec` on every re-auth, changed or not. | Change guard. Fillfactor 90. |
| `client_connection_reliability_score` | Each reliability pass upserts every client × lookback, then deletes stale rows. The upserts are non-HOT because `max_block_number` is indexed. | Drop that index, or narrow it to `(lookback_index)`. Skip unchanged rows. Fillfactor 50. |
| `transfer_debit_journal` | Insert, then `applied = true`, then delete seconds later. The update is non-HOT because `applied` is in the age index. | Re-key the age index to `(shard, create_time)`. Fillfactor 70. |
| `provider_egress_probe_cycle` | One state row per provider, updated across 8 indexes. The slot and non-slot index pairs duplicate each other, and the priority-mode indexes are nearly empty. | Index diet; fixed thresholds (A5). |
| `provider_url_probe_run` | Insert, two non-HOT updates, delete at 7 days. Readers look back at most 24 hours. | Cut retention to 2 days. |
| `network_client_handler` | A heartbeat every 5 s per handler, non-HOT only because `heartbeat_time` is indexed, on a table of a few dozen rows. | Drop the index. |
| `transfer_balance` | In-place updates on the admission path. | Fillfactor 85–90. |
| `user_auth_attempt` | Nothing writes or reads it any more; rate limiting moved to Redis. | Drop the table, and retire `RemoveExpiredAuthAttempts` through `removedTaskTargets`. |
| `legacy_settlement_intent` | A drained queue: Redis admission now marks escrows `redis_reserved`. The 33.6 GB was a past backlog plus non-HOT retries. | Once all 16 shards are empty, drop the table, its outcome trigger on `transfer_contract`, and its FK, along with the weekly contract drop's check on it (`db_contract_partition_maintenance.go`). Built now: the contract-partition fixes are merged. |
| `search_projection` | The search fallback's inverted index. Each value's projections are replaced, and its update record is published, in one transaction (`search/search.go:AddRawInTx`). Since f071c331c an unchanged value is skipped, so most of the 46% is older than that. The table is read only until the in-memory search sync completes. | Leave it (decided 2026-10-24). Dropping it together with the fallback stays possible later. Do not rebuild and swap it: a write between the copy and the swap would be lost. |

A new fillfactor applies only to pages written after the change. Existing pages
take it at the next compaction: a VACUUM FULL window, or PostgreSQL 19's online
compaction.

Implemented (2026-10-24), the `legacy_settlement_intent` row and its 16
`flush_legacy_settlements_*` chains: the queue is retired in two steps. This
release removes every reader and writer and drops the queue's guards; a later
release drops the empty table.
- No writer remains. The only INSERT was `queueLegacySettlementInTx`, reached
  from the foreground settlement for a contract whose escrows were not all
  `redis_reserved`, and from expired-dispute settlement. Both are gone. In SQL,
  the outcome guard only reads the table, and its two row triggers
  (`legacy_settlement_intent_assign_payer`,
  `legacy_settlement_intent_resolve_close_owner`) only read other tables and
  fill the row being written; nothing else writes it. Every public admission
  has marked its escrow `redis_reserved` since the policy switched on
  2026-10-03 01:16:08Z (`75b409734` made it unconditional), and the contracts
  admitted before it expired by about 04:00Z on 2026-10-22. `model`'s
  `TestModelSourcesDoNotReferenceRetiredLegacySettlementQueue` keeps every
  model source free of the table.
- Settlement: an open contract with a legacy reservation settles inline in its
  own close, through the path the queue's worker ran: the debit, outcome,
  exact earnings, escrow metadata, and the durable mirror and provider-total
  owners commit together (`settleEscrowWithOptionsInTx` with
  `inlineFinancial`). Public close, expiry, expired disputes, expiry repair and
  `SettleEscrow` use it. The deadline close already settled inline; it no
  longer reads or deletes an intent.
- Removed: the queue, its payer, source and cohort owners, the chains and
  their scheduling, ownership keys and kicks, `bringyourctl contracts
  drain-legacy`, and the monitor's `legacy-settlements` probe with its two
  classes. `removedTaskTargets` reaps `work.FlushLegacySettlements`,
  `model.ApplyLegacyPayerSettlements` and `model.ApplyLegacySourceSettlements`.
  The close pages, the deadline owner batch, expiry, the reaper, expiry repair
  (its `legacy_intent_present` status and field) and the weekly contract drop
  (`contractWeekBlocker`) no longer read the table.
- Migration, appended last (`legacySettlementIntentRetirementSql`,
  `db_legacy_settlement_intent.go`; its version is found by identity,
  `LegacySettlementIntentRetirementMigrationVersion`): with `lock_timeout` 5s
  it locks `transfer_contract`, then `legacy_settlement_intent`, in ACCESS
  EXCLUSIVE mode, raises if any intent remains, then drops the trigger
  `transfer_contract_legacy_settlement_owner`, the function
  `guard_legacy_settlement_intent_outcome()` and the key
  `legacy_settlement_intent_contract_id_fkey`. The outcome guard runs inside
  writers that already hold `transfer_contract`, so locking in that order
  cannot deadlock with them, and the check after both locks sees every
  committed intent. A refusal or lock timeout leaves it unapplied; rerun
  `db migrate`.
- Monitor: the 763 contract ends at the retirement's version, and a new
  contract at that version requires the table's published shape plus the
  guard, function and key gone (also on every contract partition). Both read
  the version from the migration's position. SIGNALS.md §2.5b records the
  retirement.
- Before deploying (owner, read-only on main):
  `SELECT count(*) FROM legacy_settlement_intent` must be 0, or the migration
  refuses. Also check, with the same expectation, that no open contract still
  holds a legacy reservation:
  `SELECT count(*) FROM transfer_contract c WHERE c.outcome IS NULL AND EXISTS (SELECT 1 FROM transfer_escrow e WHERE e.contract_id = c.contract_id AND NOT e.redis_reserved)`.
  One would settle correctly under this release, but a previous-release binary
  can still queue an intent for it during the rollout. The migration takes
  ACCESS EXCLUSIVE on `transfer_contract` for a moment: apply it when quiet.
- Deviations:
  - The table and its two row triggers stay one release. Migrations apply
    before the new binaries serve (startup readiness requires the head), and
    the previous release still reads the table on expiry, the deadline close,
    the close pages and the reaper. Drop the table, with
    `assign_legacy_settlement_intent_payer()` and
    `assign_legacy_settlement_close_owner()`, once every job (api, connect,
    taskworker, alt, proxy, monitor) runs this release; that change also
    retires the monitor's payer and close-owner contracts and the tests that
    insert a leftover intent row.
  - A legacy reservation's close now meets its payer's grant owner directly:
    a busy owner refuses the close (`errTransferBalanceOwnershipBusy`) for the
    client or expiry to retry, and an accounting refusal at expiry takes the
    same no-payout quarantine as a Redis contract, where the queue retried
    both with backoff. No such contract is expected to remain.
  - Tests of the queue machinery itself were deleted; tests of settlement,
    expiry, repair, mirrors and provider totals were converted to the inline
    owner.

## Part C: partitioning

A candidate is a table whose retention can drop a whole time range, and whose
hot reads carry that range. The partition key must be in every unique index.
The key must also be the table's retention timestamp. An id from
`server.NewId()` leads with the millisecond it was minted, which differs from
the row's own time when a backfill or a delayed write mints a new id.

### C1. Contract tables

Built: see `CONTRACT-PARTITION.md`. Weekly partitions by `contract_id`, with
13-week retention. Cut over after the storage expansion. Once the tables are
partitioned, `maintain_contract_partitions` starts retiring weeks by itself
(`db_contract_partition_maintenance.go:78`).

### C2. `provider_egress_health_history`: partition now

- **Size:** 130 GB after VACUUM FULL (134 GB before), so it is live, not
  bloated.
- **Writes:** one `INSERT ... ON CONFLICT (run_id) DO NOTHING` per probe result
  (`model/provider_egress_health_model.go:SetProviderEgressHealth`, the only
  writer), about 60 wide jsonb rows per provider a day.
  - When the insert inserts nothing, the result is not published to health or
    cycle progress again. The conflict is the deduplication.
  - A delayed result keeps its original `measured_at`.
- **Retention:** none. No code ever deletes from it.
- **Reads:** every reader bounds `measured_at` to 8 hours or less:
  - `providerEgressHealthWindowSql`;
  - the 4-hour windows in `provider_url_probe_due.go`;
  - `provider_intent_model.go`;
  - `provider_egress_destination_model.go`.

  One diagnostic has no lower bound.
- **No FKs or triggers.**
- **Key:** daily `RANGE (measured_at)`, with primary key and conflict target
  `(run_id, measured_at)`. A replay repeats both, since a delayed result keeps
  its `measured_at`.
  - `run_id` cannot be the key. A submission without a run id gets one derived
    from a SHA-256 of the client and the measurement time, which has no time
    order. Production submitters send their own.
- **Replays with a new time:** the composite key catches a replay only if it
  repeats `measured_at`. Before the cutover, confirm that every submitter's
  retry does. If any can resend a run id with a new time, check `run_id`
  across the retained partitions under a per-run advisory lock before
  inserting.
- **Ingest:** today it rejects only future timestamps
  (`controller/provider_egress_ingest_controller.go:SubmitProviderEgressHealth`).
  Add a lower bound of about a day, because without a DEFAULT partition an
  older timestamp would fail the insert.
- **Retention after:** must stay longer than the ingest lower bound. A replay
  is then either refused at ingest, or meets its row still present, so it can
  never credit cycle progress twice.
- **Cutover:**
  1. Create the partitioned table and copy the last 1–2 days. Older rows are
     not carried over; only the one diagnostic reads past 8 hours.
  2. In one transaction:
     - lock the old table against writes (`LOCK ... IN EXCLUSIVE MODE`, with
       a short `lock_timeout` and retry);
     - copy the rows inserted since step 1;
     - swap the names.

     Ingest waits on the lock for that moment, then writes to the new table.
  3. Verify, then drop the old table, which returns about 125 GB.

  Until the drop, renaming back is the rollback, after copying back the rows
  written since the swap.
- **Monitor:** `monitor/migration_fp2.go` pins this table's exact columns and
  indexes. Update that catalog contract for the partitioned shape in the same
  change, as the contract work did for its tables.
- **Tests:**
  - a result with no run id, before and after the cutover;
  - a delayed result;
  - a replay at the retention boundary, which must not credit cycle progress
    twice;
  - writes during the swap.
- **Decided (2026-10-24): 7 days.** Daily partitions, each dropped once its
  upper bound is 7 days past.

### C3. `finished_task`: daily partitions kept 8 days (decided 2026-10-24)

- **Writes:** insert-only at completion. The row moves from `pending_task` in
  one fenced statement (`DELETE ... RETURNING` in `task/task_run_once.go`;
  `taskCompletionOwnerSql`, `taskCompletionBatchSql`). Its rare updates are
  HOT-eligible.
- **Two retention classes** (`task/task_cleanup.go`):
  - 24 hours by default;
  - 7 days for a failed Post that has not completed (`post_error` set,
    `post_completed` false), which RunPost may still retry.

  `TaskCleanup` admits each row's finished owner before deleting it, even
  past 7 days.
- **What a drop must not break:**
  - **Post retries.** RunPost treats a missing row as a permanent no-op
    (`task/task.go`). Dropping a failed Post before its 7 days would silently
    end its retries.
  - **Owner admission.** A drop bypasses the owner admission, so it needs an
    equivalent fence against a Post retry running at that moment.
  - **Task identity.** `task_id` is the primary key today, and readers key on
    it alone: the `GetFinishedTasks` map, the Post acknowledgement and owner
    validation.
    - A partitioned table can enforce only `(task_id, run_end_time)`. That
      allows two rows for one task, even in the same partition.
    - The fenced move inserts once per completion. The design must prove that
      for every writer, the cutover and every retry, or keep identity in a
      separate relation. A unique `task_id` index on each leaf does not stop a
      duplicate in another leaf.
- **Key:** `run_end_time`, not `task_id`. A task's id is minted when it is
  scheduled, and one task finished 96 days later.
- **Chosen: keep daily partitions 8 days,** dropping each 7 days after its
  upper bound, behind the owner fence. That is about 14 GB at today's volume
  and about 7 GB after B2. Lookups by `task_id` probe up to 9 partitions.
  - Not chosen: moving a failed Post's row to a side table when it fails, and
    dropping history at 24 hours.
- **Identity:** the fenced move from `pending_task` stays the only writer. A
  test pins that a completion inserts exactly one row, and the cutover copies
  nothing (the old table attaches as is), so no backfill can duplicate a task.
- **Cutover:** attach the current table as a legacy partition behind a
  validated `CHECK (run_end_time < C)`, as the contract cutover does. It then
  ages out as one partition.
- **Tests:**
  - with a fixed clock, a 25-hour-old and a 6-day-old failed Post that still
    retry successfully;
  - a drop racing a Post retry, and a 7-day-old row whose finished owner is
    held while the drop runs: the drop waits or defers;
  - one task id with two end times, both in one partition and across two;
  - an interrupted cutover overlapping a live completion, and a completion
    repeated after an unacknowledged commit: one durable row and one Post
    effect;
  - the existing `task/task_queue_retention_test.go` cases, against the
    partitioned schema.

B1, B2 and A1 alone would keep the table small. The owner chose to partition it
as well (2026-10-24), so its retention is a drop that doesn't depend on vacuum.

### C4. `provider_work_session_event` (136 GB): weekly partitions, 13 weeks (decided 2026-10-24)

- **Writes:** the connection trigger (`provider_work_session_append`) appends
  an admit and a retire event for every connection of every client, provider
  or not.
- **All live:** guard triggers refuse UPDATE, DELETE and TRUNCATE.
- **Signer not deployed:** the signer these events exist for has not shipped
  (migration 824; `providerWorkSignSessionEventsInTx`).
- **Not chosen:** stopping events for non-provider clients while no signer
  runs.
- **Design:**
  - weekly `RANGE (observed_at)`, with primary key
    `(client_id, sequence, observed_at)`;
  - rework the FK from `provider_work_session_receipt`;
  - give the retire lookup a lower bound from the connection id's timestamp;
  - cut over without copying: the current table attaches as a legacy
    partition behind a validated `CHECK (observed_at < C)`;
  - drop only behind an explicit policy check, since a drop bypasses the
    guards: only the retention task drops, only weeks whose upper bound is 13
    weeks past, and only under an opt-in setting it alone sets (as migration
    857 does for close evidence).
- **Decided (2026-10-24): 13 weeks,** matching contract retention, so a
  session's events never outlive the contracts they describe.

### C5. `contract_close_report_evidence` (170 GB): weekly partitions plus the row deletes (decided 2026-10-24)

Deletes start about 91 days after the oldest evidence rows, around 2027-01-05.

- **Built, and kept:** migration 857 and `RemoveExpiredCloseEvidence` (every 15
  minutes) delete rows accepted more than 91 days ago whose contracts are
  resolved.
  - At 1.3 GB an hour (SIGNALS.md §2.4f), 91 days is about 2.8 TB. Row deletes
    alone would mean tens of millions of guarded deletes a day.
- **Design: weekly `RANGE (report_id)`, dropped when a week expires.**
  - `report_id` is in the primary key and time-ordered: the reporting client
    mints it with `NewId()` (connect's `transfer_contract_manager.go`). So
    `ON CONFLICT (client_id, report_id)` stays enforceable.
  - **Drop rule:** a week drops once its upper bound is 91 days past and every
    row in it meets the 857 rule (accepted more than 91 days ago, contract
    resolved or gone). The check runs again in the transaction that detaches
    the week. A week holding a straggler stays. Its qualifying rows are
    row-deleted as today, and it drops once the rest qualify.
  - **Guard:** a drop bypasses the 857 row guard, so only the retention task
    drops, under the same opt-in setting, and only weeks the check passed.
  - **Clock skew:** clients mint `report_id` with their own clocks. A catch-all
    (DEFAULT) partition takes ids outside the created weeks, so no report is
    ever refused. It is never dropped; the row deletes empty it. Before a week
    is created, any of that week's rows in the catch-all move into it, because
    PostgreSQL refuses to create a partition whose rows sit in DEFAULT.
  - **Cutover without copying:** the current table attaches as a legacy
    partition behind a validated `CHECK (report_id < floor(C))`. Rows already
    past that bound, from clients running ahead, move into the catch-all
    first. The legacy partition is never dropped automatically; the row
    deletes empty it over time.
  - **Census:** the ST epoch census (`model/st_provider_close_originals.go`)
    bounds `report_id` using the contract's times, so it prunes to the weeks
    it needs.
  - **Tests:**
    - a report minted ahead of the server clock lands in the catch-all, and
      later in its week;
    - a week with one unresolved contract isn't dropped, and its other rows
      are still row-deleted;
    - the census prunes;
    - the cutover runs with inserts arriving.

### C6. Later, on size triggers or decisions

- **`audit_device_event`** (31 GB):
  - About 1.4M rows a day, deleted hourly at 180 days by `event_time`. It
    should level off near 75 GB.
  - Only if it passes about 100 GB, switch to monthly `RANGE (event_time)`.
    Its primary key already leads with `event_time`; `UNIQUE (event_id)`
    would go.
  - Partition by `event_time`, not `event_id`: backfills mint fresh ids for
    historical event times (`model/audit_provider_sweep_model.go`).
  - A backfill or rollup can write an `event_time` older than the oldest
    partition. Without a DEFAULT partition the insert fails, so refuse or skip
    such rows explicitly.
  - Tests: a historical event with a fresh id; a daily rollup rerun across a
    partition boundary, contributing once; pruning in the stats queries.
  - Name the event tables one by one:
    - The other event feeds are small.
    - `audit_provider_state` and `audit_device_state` are mutable state, not
      event feeds.
    - `audit_account_payment` keeps its unique event id, which is the Circle
      idempotency key.
- **`search_provider_stats`:** daily `RANGE (period_start)` would match its
  30-day retention, but buys little at 4.3 GB once Part B fixes its upsert.
- **`st_provider_usage_archive`** (1.5 GB): one-year retention (decided
  2026-10-24). It grows about 20 GB a week once contract retention archives
  every retired week.
  - **Partitions:** weekly by contract week, on the contract tables' bounds
    (Monday 00:00 UTC, through `contract_id_floor()`). Archive writes land in
    weeks long past, so weeks exist from the oldest contract (2026-06-01)
    onward, created ahead of the contract weeks being retired.
  - **Retention:** a week drops once its upper bound is 52 weeks past. A drop
    bypasses the append-only guard, so only the retention registry drops it.
  - **Cutover:** not a plain copy. The insert guard requires the matching live
    `transfer_contract` row (`db_provider_usage_archive.go`), so an
    already-archived row can't pass it again once its contract is gone. The
    table is small, so one migration copies it into the partitioned table
    with the guard bypassed for that copy only. It then checks the copy
    exactly (row count, and per-week sums of usage) and swaps the names under
    a write lock.
  - **Tests:**
    - an archived row whose contract is gone;
    - a terminal contract archived during the migration;
    - proof fields and usage totals compared before and after;
    - the migration restarted midway;
    - a week dropped at 52 weeks and not before.

### C7. Not partitioned

| Table | Why not | Instead |
|---|---|---|
| `pending_task` | `UNIQUE (run_once_key)` cannot include a partition key, and every run-once producer depends on `ON CONFLICT (run_once_key)`. Rows can live indefinitely (backoff, far-future run times), so no range empties. Keyed on `available_block`, rows would move between partitions on every claim and heartbeat. | A1, A2, B |
| `network_client_connection` | Rows go 8 hours after disconnect, but provider connections last for days, so a connect-time range does not empty. The hot reads have no time bound. | B. If that is not enough, move closed rows to an append-only history partitioned hourly by `disconnect_time`. |
| `st_client_key_history` | A `client_id` range is a cohort of clients, not an age: an old, active client appends a new generation today, so its range never empties. A time key would make every `client_id` lookup probe every partition. | D |
| `network_client`, `provide_key` | Mostly live. Retention is per row, by state. `network_id` reads and the reap scans would probe every partition, and a drop would bypass the client-delete triggers and cascades. | D |
| `network_client_location_reliability` | One mutable row per client. No reader has a time bound. | B |
| `network_client_location` | A 1:1 child of the connection table. | It follows that table's lifecycle. |
| `device`, `transfer_balance`, `transfer_debit_journal`, `client_connection_reliability_score`, `provider_egress_probe_cycle`, `provider_url_probe_run`, `network_client_handler` | Entities, queues or derived tables with no droppable time range. | A1, B |
| `legacy_settlement_intent`, `user_auth_attempt` | Being retired. | B |

## Part D: identity churn needs retention

`network_client` (455 GB), `provide_key` (195 GB) and `st_client_key_history`
(495 GB) hold about 1.1 TB, mostly live.

Hosted proxy windows mint one child client per candidate exit
(`authNetworkClientWithParentState`). That was 57 a second at baseline and
240–760 a second on 10-21 and 10-22 (SIGNALS.md §14.8). Each child adds:

- a `network_client` row and its provide keys;
- a key registration of about 4.7 KB on disk. `st_client_key_history` grew
  4.3 GB an hour on 10-22, the largest share of the database's 15.1 GB an
  hour (SIGNALS.md §2.4f);
- connections and contracts.

Changes:

- **Hard-delete retired hosted identities sooner.**
  - Today a retire is a soft delete (`RemoveNetworkClient`), and the hard
    delete comes 30 days later (`NetworkClientReapAfterDeactivate`).
  - Hard-delete them 1 day after retirement, well past the 75-minute contract
    linger, after checking the close and settlement readers. Top-level
    clients keep their current policy.
- **Delete provide keys at deactivation.** An inactive client cannot be a
  contract destination.
- **Make `SetProvide` write only the difference.** It deletes and re-inserts
  every key on every Provide message, changed or not.
- **Give `st_client_key_history` retention, for clients that are gone.**
  - Its immutability trigger needs a guarded opt-in, as migration 857 gives
    close evidence.
  - Original-close verification reads this history
    (`model/contract_close_original.go`). So the horizon must outlast every
    close report that can still arrive for the client's contracts.
  - Delete the heads first. `st_client_key_head` references history, and a
    retained head marks a complete archived history
    (`db_client_key_policy_history.go`), so a client's history goes only after
    all of its heads.
- **Decided (2026-10-24):**
  - retired hosted identities and their provide keys: hard-deleted 1 day after
    retirement;
  - key history of a client that is gone: deleted 91 days after it's gone,
    aligned with contract retention, so it outlasts every close report;
  - bringing the mint rate down at the source is a protocol and product
    change outside this plan.

## Cutover requirements

Every table swap or partition cutover in this plan needs:

- a write fence or change capture, and an exact-copy check under it;
- bounded lock waits (`lock_timeout` and retry), never an unbounded queue;
- restart behavior for each step;
- a task drain, where one is used, that covers running completions and Post
  retries, not only new claims;
- a rollback point that keeps writes made after the swap;
- tests with fixed clocks and barriers: concurrent writers, a crash before and
  after the swap, and the first partition boundary.

## Decisions

All decided on 2026-10-24; see "Final decisions" at the top. No open owner
questions remain. Two confirmations happen during implementation:

- **C2:** confirm that every egress submitter's retry repeats its
  `measured_at`. If one doesn't, add the per-run `run_id` check.
- **A4:** at the PostgreSQL 19 upgrade, confirm REPACK's `wal_level` needs and
  its lock behavior.

## Order of work

1. **Edge-2 maintenance:** VACUUM FULL of everything that fit (done). The five
   largest tables wait for the storage expansion (in progress) and the
   contract cutover.
2. **Phase 1, stop the queue bloat:** A1, A2, A3, and Part B's first two rows
   (`pending_task` heartbeats, idle chains).
3. **Phase 2, the rest of Part B,** including retiring `user_auth_attempt`
   and `legacy_settlement_intent`.
4. **Phase 3, the horizon and measurement:** A6 (inventory, timeout, horizon
   signal) and the A4 bloat measurement and signal. A5 is coded behind its
   playbook tag, not applied.
5. **Phase 4, the registry:** A7, with C2 as its first new family.
6. **Phase 5:** C3 and C4.
7. **Phase 6:** C5 and C6.
8. **Phase 7:** Part D.
9. **Contract cutover** (`CONTRACT-PARTITION.md`), when the owner schedules it.
10. **After the PostgreSQL 19 upgrade:** A4 compaction, reported first.

Every change ships with tests (owner rule). A change with a migration merges
to main before any database test applies it.
