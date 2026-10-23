# Partitioning and maintenance plan: ongoing bloat

Status: PLAN, written 2026-10-23 during the edge-2 disk-full outage. It was
revised the same day after a code review of each table and a design review of
the first draft. Nothing here is implemented. **Decision** marks an item that
needs the owner.

Companion to `CONTRACT-PARTITION.md`, which covers the six contract tables
(built, not cut over). This plan covers the rest of the database, plus the
`db_maintenance` changes that keep bloat from coming back.

**Owner constraint:** no `pg_repack` or similar external tool. Online table
compaction waits for the built-in support in PostgreSQL 19, which main will
upgrade to soon. Until then, a table shrinks only in a maintenance window
(`VACUUM FULL`, as on 2026-10-23). Index rebuilds stay online with core
`REINDEX CONCURRENTLY`, as the rotation does today.

Sizes are decimal GB, as SIGNALS.md §5.14 measures them.

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
  - **Indexes:** the btree estimate the priority rebuild already uses.
    `pgstattuple_approx` covers only the heap.
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
- **After the upgrade, `REPACK (CONCURRENTLY)`.** Its documented costs set the
  requirements for any automation:
  - Room for the table, all its indexes and the WAL of the copy, beyond the
    disk reserve.
  - It holds a snapshot for its whole run. That holds back cleanup in every
    other table: the horizon A6 bounds.
  - A final exclusive lock, and some tables it cannot process.

  Pin the release's behavior at the upgrade. Then define eligibility,
  headroom, lock and runtime bounds, cancellation and retry, and horizon
  monitoring while it runs.

  Start by reporting candidates for an operator. Automate only after that:
  one table at a time, under the rotation's load deferrals, with its own
  budget apart from routine vacuum.
  - **Decision:** whether it ever runs on its own.
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

**Decision:** the cost budget and worker count. Set them in a trial after the
storage expansion, with the §2.4 signals watched.

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
- **Decision:** the bound. 15 minutes is the proposal, once the inventory
  confirms every request and task path fits.

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
- leaves that keep changing after their range closes, which need analyzing
  again;
- families that are never dropped;
- the contract checks, unchanged.

A new family does not wait for the registry. It ships with its own
maintenance, as C2 would, and moves into the registry later.

The monitor's runway and blocked-retention probes then cover every registered
table, not only contracts.

## Part B: write paths

The changes below stop row versions from being written in the first place.
Each one is small, and each needs a test.

| Table | What writes the dead versions | Change |
|---|---|---|
| `pending_task` | A running task's heartbeat, every 10 s, rewrites `release_time` (`task/task.go:refreshTaskTimestampLeases`). That moves the generated `available_block` column, which is in both poll indexes, so every heartbeat is non-HOT: about 720 versions for a 2-hour task. | Write `claim_time` every heartbeat. Extend `release_time` only when less than half the lease remains: the claim's session advisory lock guards live ownership, and `release_time` only bounds crash recovery. Fillfactor 50–70. |
| `pending_task`, `finished_task` | 16 `flush_transfer_debits_*` and 16 `flush_legacy_settlements_*` chains re-arm every 2 s even when they find nothing. That is at least 16 completions a second, about 1.4M `finished_task` rows a day, each also a `pending_task` insert, claim and delete. The legacy queue is empty. | Back off an idle chain from 2 s to 30 s, and have the enqueue path kick its shard's chain (`KickTasks`) so latency stays low when work arrives. Stop the legacy chains when that table retires. |
| `network_client_connection` | Disconnect flips `connected`, which is in five indexes: a non-HOT update writing six index entries. About 97% of rows are closed and waiting out the 8 hours, and all of them sit in the indexes the open-row reads use. | Partial indexes for open rows: `(client_id, extender_id) WHERE connected`, `(client_address_hash) WHERE connected`, keep `(handler_id) WHERE connected`, and add `(disconnect_time) WHERE NOT connected` for the reaper. The hot indexes then hold about 90k entries instead of 3.3M. Check that each reader's predicate matches. |
| `network_client_location_reliability` (132 GB) | `UpdateClientLocationReliabilitiesInTx` upserts every connected client every 30 minutes with no change guard. It sets the indexed `update_block_number`, so every rewrite is non-HOT. The retention delete is one unbatched statement every 15 minutes. | Add an `IS DISTINCT FROM` guard. Refresh `update_block_number` only when it is about a day old. Fillfactor 85. Batch the delete. Measure how many rows are hosted child identities (`source_client_id IS NOT NULL`), which most readers exclude anyway. |
| `search_provider_stats` | `RollupSearchProviderStats` re-upserts every row of the open hour every minute: about 60 versions per row. A closed hour rolls up once, and its Redis bucket is then deleted. | Add a change guard, so an unchanged row is not rewritten. If the current hour's stats may lag 10 minutes, roll up the open hour every 10 minutes. Fillfactor 70. Do not regenerate the table: a closed hour's Redis source no longer exists. |
| `device` | The re-auth branch of `authNetworkClientWithParentState` rewrites `device_spec` on every re-auth, changed or not. | Change guard. Fillfactor 90. |
| `client_connection_reliability_score` | Each reliability pass upserts every client × lookback, then deletes stale rows. The upserts are non-HOT because `max_block_number` is indexed. | Drop that index, or narrow it to `(lookback_index)`. Skip unchanged rows. Fillfactor 50. |
| `transfer_debit_journal` | Insert, then `applied = true`, then delete seconds later. The update is non-HOT because `applied` is in the age index. | Re-key the age index to `(shard, create_time)`. Fillfactor 70. |
| `provider_egress_probe_cycle` | One state row per provider, updated across 8 indexes. The slot and non-slot index pairs duplicate each other, and the priority-mode indexes are nearly empty. | Index diet; fixed thresholds (A5). |
| `provider_url_probe_run` | Insert, two non-HOT updates, delete at 7 days. Readers look back at most 24 hours. | Cut retention to 2 days. |
| `network_client_handler` | A heartbeat every 5 s per handler, non-HOT only because `heartbeat_time` is indexed, on a table of a few dozen rows. | Drop the index. |
| `transfer_balance` | In-place updates on the admission path. | Fillfactor 85–90. |
| `user_auth_attempt` | Nothing writes or reads it any more; rate limiting moved to Redis. | Drop the table, and retire `RemoveExpiredAuthAttempts` through `removedTaskTargets`. |
| `legacy_settlement_intent` | A drained queue: Redis admission now marks escrows `redis_reserved`. The 33.6 GB was a past backlog plus non-HOT retries. | Once all 16 shards are empty, drop the table, its outcome trigger on `transfer_contract`, and its FK. The weekly contract partition drop checks this table today (`db_contract_partition_maintenance.go:283`). |
| `search_projection` | The search fallback's inverted index. Each value's projections are replaced, and its update record is published, in one transaction (`search/search.go:AddRawInTx`). Since f071c331c an unchanged value is skipped, so most of the 46% is older than that. The table is read only until the in-memory search sync completes. | Leave it, or drop it together with the fallback. Do not rebuild and swap it: a write between the copy and the swap would be lost. |

A new fillfactor applies only to pages written after the change. Existing pages
take it at the next compaction: a VACUUM FULL window, or PostgreSQL 19's online
compaction.

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
  (`model/provider_egress_health_model.go:SetProviderEgressHealth`), about 60
  wide jsonb rows per provider a day.
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
- **Tests:**
  - a result with no run id, before and after the cutover;
  - a delayed result;
  - a replay at the retention boundary, which must not credit cycle progress
    twice;
  - writes during the swap.
- **Decision:** retention. 7 days covers every reader with room.

### C3. `finished_task`: possible, but needs a design proof first

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
      separate relation.
- **Key:** `run_end_time`, not `task_id`. A task's id is minted when it is
  scheduled, and one task finished 96 days later.
- **Options:**
  - **Keep daily partitions 8 days,** dropping each 7 days after its upper
    bound, behind the owner fence. That is about 14 GB at today's volume and
    about 7 GB after B2. Lookups by `task_id` probe up to 9 partitions.
  - **Or move a failed Post's row to a side table** when it fails, and drop
    history at 24 hours.
- **Cutover:** attach the current table as a legacy partition behind a
  validated `CHECK (run_end_time < C)`, as the contract cutover does. It then
  ages out as one partition.
- **Tests:**
  - with a fixed clock, a 25-hour-old and a 6-day-old failed Post that still
    retry successfully;
  - a drop racing a Post retry;
  - one task id with two end times, both in one partition and across two.

After B1, B2 and A1, the table stays small without this, so C3 is optional.

### C4. `provider_work_session_event` (136 GB): retention decision first

- **Writes:** the connection trigger (`provider_work_session_append`) appends
  an admit and a retire event for every connection of every client, provider
  or not.
- **All live:** guard triggers refuse UPDATE, DELETE and TRUNCATE.
- **Signer not deployed:** the signer these events exist for has not shipped
  (migration 824; `providerWorkSignSessionEventsInTx`).
- **Cheapest fix:** stop appending for non-provider clients while no signer
  runs.
- **If retention is allowed:**
  - weekly or monthly `RANGE (observed_at)`, with primary key
    `(client_id, sequence, observed_at)`;
  - rework the FK from `provider_work_session_receipt`;
  - give the retire lookup a lower bound from the connection id's timestamp;
  - drop only behind an explicit policy check, since a drop bypasses the
    guards.
- **Decision:** retention, or no events for non-provider clients.

### C5. `contract_close_report_evidence` (170 GB): decide before about 2027-01-05

Deletes start about 91 days after the oldest evidence rows.

- **Built:** migration 857 and `RemoveExpiredCloseEvidence` (every 15 minutes)
  delete rows accepted more than 91 days ago whose contracts are resolved.
  - At 1.3 GB an hour (SIGNALS.md §2.4f), 91 days is about 2.8 TB.
  - Removing it takes tens of millions of guarded row deletes a day.
- **Alternative: weekly `RANGE (report_id)`.**
  - `report_id` is in the primary key and is time-ordered: the reporting
    client mints it with `NewId()` (connect's `transfer_contract_manager.go`).
  - So `ON CONFLICT (client_id, report_id)` stays enforceable, and retention
    becomes a drop.
  - Costs: client clocks skew, so admission needs a range check or a
    catch-all partition. The ST epoch census
    (`model/st_provider_close_originals.go`) must also bound `report_id` using
    the contract's times.
  - A `report_id` week is when the report was minted, not when it was
    accepted. The 91 days would then count from minting.
- **Decision:** row deletes, or partitions by `report_id`.

### C6. Later, on size triggers or decisions

- **`audit_device_event`** (31 GB):
  - About 1.4M rows a day, deleted hourly at 180 days by `event_time`. It
    should level off near 75 GB.
  - Only if it passes about 100 GB, switch to monthly `RANGE (event_time)`.
    Its primary key already leads with `event_time`; `UNIQUE (event_id)`
    would go.
  - Partition by `event_time`, not `event_id`: backfills mint fresh ids for
    historical event times (`model/audit_provider_sweep_model.go`).
  - Name the event tables one by one:
    - The other event feeds are small.
    - `audit_provider_state` and `audit_device_state` are mutable state, not
      event feeds.
    - `audit_account_payment` keeps its unique event id, which is the Circle
      idempotency key.
- **`search_provider_stats`:** daily `RANGE (period_start)` would match its
  30-day retention, but buys little at 4.3 GB once Part B fixes its upsert.
- **`st_provider_usage_archive`** (1.5 GB): it grows about 20 GB a week once
  contract retention archives every retired week.
  - If it gets retention, partition it, but not by a plain copy.
  - Its insert guard requires the matching live `transfer_contract` row
    (`db_provider_usage_archive.go`). An already-archived row cannot pass the
    guard again once its contract is gone.
  - Archive writes land in contract weeks long past, so partitions must exist
    for historical ranges.
  - While retention is undecided, nothing drops it automatically.
  - **Decision:** archive retention.

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
  - Use a short horizon, never before the 75-minute contract linger, after
    checking the close and settlement readers.
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
  - Delete the head first, since `st_client_key_head` references history.
- **Decision:**
  - the horizons;
  - whether to bring the mint rate down at the source, which is a protocol and
    product change.

## Cutover requirements

Every table swap or partition cutover in this plan needs:

- a write fence or change capture, and an exact-copy check under it;
- bounded lock waits (`lock_timeout` and retry), never an unbounded queue;
- restart behavior for each step;
- a rollback point that keeps writes made after the swap;
- tests with fixed clocks and barriers: concurrent writers, a crash before and
  after the swap, and the first partition boundary.

## Decisions

1. **A4:** whether `REPACK (CONCURRENTLY)` ever runs on its own after
   PostgreSQL 19, or stays operator-run.
2. **A5:** the autovacuum cost budget and worker count, after the storage
   expansion.
3. **A6:** the `transaction_timeout` bound.
4. **C2:** `provider_egress_health_history` retention (7 days proposed).
5. **C4:** `provider_work_session_event` retention, or no events for
   non-provider clients.
6. **C5:** evidence row deletes, or `RANGE (report_id)`, before about
   2027-01-05.
7. **C6:** `st_provider_usage_archive` retention.
8. **D:** identity retention horizons, and the mint rate.

## Order of work

1. **Edge-2 maintenance:** VACUUM FULL of everything that fit (done). The five
   largest tables wait for the storage expansion and the contract cutover.
2. **Stop the queue bloat:** A1, A2, A3, and Part B's first two rows
   (`pending_task` heartbeats, idle chains).
3. **C2:** `provider_egress_health_history`. It returns about 125 GB, at low
   risk.
4. **After the storage expansion:** A6 (horizon), then A5 (pacing, then
   workers).
5. **The rest of Part B,** including retiring `user_auth_attempt` and
   `legacy_settlement_intent`.
6. **Part D,** once its horizons are decided.
7. **Contract cutover** (`CONTRACT-PARTITION.md`), then the C4 and C5
   decisions and the A7 registry. C3 is optional, and needs its design proof
   first.
8. **After the PostgreSQL 19 upgrade:** A4 compaction, reported first.

Every change ships with tests (owner rule). A change with a migration merges
to main before any database test applies it.
