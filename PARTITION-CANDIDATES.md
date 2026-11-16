# PARTITION-CANDIDATES — what to partition on Main, and what to fix instead (2026-11-16)

The owner approved "Design it for review" for the queue tables on 11-16 at about 03:45Z,
and asked: "What other tables are candidates for partitioning?" This is that design. It
covers the queue tables first (`pending_task`, `finished_task`, `transfer_debit_journal`),
then every other table on the 11-16 candidate list.

**Design only.** Nothing changed in production: no DDL, delete, setting or control write.
The measurements are read-only catalog and statistics reads on Main (§12).

- Sizes are GiB (2^30). Code references are server `origin/main` at 3939d79b4.
- *(inferred)* marks a claim reasoned from code or arithmetic rather than measured.
- Context: DBDISK-STABILIZE.md (the 11-15 disk plan; its I5 sketched the queue tables),
  PARTITION-MAINTENANCE-PLAN.md (C2–C7, the registry, Part D), CONTRACT-PARTITION.md.
- **Already partitioned:** `client_reliability`, `st_provider_usage_archive`,
  `provider_egress_health_history`, the work-session journal (its legacy leaf dropped at
  01:14Z, f0024a43f).
- **Built but not cut over:** C1, the contract tables (parked for 6–7 TiB of headroom), and
  C5, the close evidence (it needs a legacy-drop step). Neither is in scope here.
- The owner's rules apply: no pg_repack, no VACUUM FULL.

## 1. Summary

**Recommended, in order:**

| # | Table | Method | Space | Other gains | Effort |
|---|---|---|---|---|---|
| 1 | `finished_task` | Cut over C3, built 10-24 and never run | **−20 GiB** at C + 7 days (34.1 → about 13.5) | `TaskCleanup`'s 31 deletes a second stop; retention by drop | operations only |
| 2 | `pending_task` | A drained swap into a twin table (a rotating pair) | **−39 GiB** at once (39.3 → about 0.2) | most of 33 ms/s of vacuum and 72k heap buffer hits a second go | 1–1.5 days; a 2–4 minute taskworker stop |
| 3 | `transfer_debit_journal` | The same swap. No drain is needed: every statement on it is READ COMMITTED | **−7 GiB** at once | its age gauge drops from 1,988 buffer hits a call to about 600 | +0.5 day; seconds online |
| 4 | `audit_device_event` | Weekly (or daily) partitions by `event_time`, a raw-event window, and a per-day rollup for its one reader | Stops growth of **+5.4 GiB/day** at the window. Today's 180 days leads to about 1 TiB. With a rollup and 7 days it holds about 40 GiB (−35 GiB). | the hourly export stops reading every row ever kept | 2–3 days, plus 2–3 for the rollup |

**Not partition candidates; retention is the lever:**
- `st_client_key_history` (986 GiB): no key range ever empties. Its readers need a gone
  client's keys for a day or two, not 30.
  - **Proposal:** 7 days after the client is gone. This releases about 360–400 GiB inside the
    table one to three weeks sooner, and lowers the steady state by about 110–170 GiB
    *(inferred)*.
  - Separately, drop the redundant `UNIQUE (registration_hash)`: **−20.6 GiB at once**.
- `network_client_location_reliability` (190 GiB, +7.8 GiB/day): 89% of the clients its
  daily pass writes are hosted children, whose rows outlive the client by about 30 days.
  - **Fix:** delete the row with the client, and keep 11 days instead of 31.
  - That avoids about +120 GiB of growth to its steady state and frees most of its 475M rows
    in place *(inferred)*.

**Not candidates (confirmed):** `network_client_connection`, `network_client_location`,
`network_client`, `provide_key`, `st_client_key_head`. Also `provider_work_session_head` and
`audit_device_state`, which get delete retention (S4, approved).

**Corrections to DBDISK-STABILIZE I5:**
- finished_task's 8-day steady state is about 13–14 GiB, not 4–6. Rows are about 590 bytes,
  so I5's −74 GiB becomes about −66.
- The debit journal has no `ON CONFLICT` anywhere.
- Contracts now live 60 minutes, not up to 10 days 16 hours.

The owner decisions are in §10, and the order, windows and risks in §11.

## 2. Every candidate

Growth was measured from the 11-15 21:35:40Z family snapshot of DBDISK-STABILIZE §2.2 to
11-16 04:15:09Z (6.7 h). DBDISK's evening window (11-15 20:05–21:35Z) is in brackets. Rows
are per second over the same 6.7 h.

| Table | GiB | GiB/day | in / out / upd per s | Access pattern | Partitioning helps? | Key, interval | Verdict |
|---|---|---|---|---|---|---|---|
| `pending_task` | 39.3 (heap 27.8) | 0.0 [0.0] | 31 / 31 / 33 | Queue: claim cursor, `FOR UPDATE SKIP LOCKED`, leases, run-once upserts | No. `UNIQUE (run_once_key)` is global, rows wait indefinitely, and the schedule changes on every claim | — | Rotating pair (§4) |
| `finished_task` | 34.1 (heap 27.9) | 0.0 [0.0] | 31 / 31 / 9 | History: written once at completion, read by task id, kept 1 day (7 for a failed Post) | Yes | `run_end_time`, daily, 8 days (C3) | Cut over (§5) |
| `transfer_debit_journal` | 7.2 (heap 7.0) | 0.0 [0.0] | 1,318 / 1,318 / 1,318 | Journal: inserted at a contract's outcome, applied and deleted seconds later | Yes; each contract day empties about 75 min after it ends | `contract_id`, daily | Swap (§6); partitions as the alternative |
| `network_client_connection` | 7.9 | +0.3 [+4.3, its vacuum cycle] | 397 / 386 / 396 | Live connections. PK reads on every connect and disconnect; work-session triggers | No. Open provider rows pin every range | — | No (§9.2) |
| `network_client_location` | 6.5 | +0.3 [+1.0] | 394 / 384 / 0 | 1:1 child of a connection | No | — | No (§9.2) |
| `st_client_key_history` | 986.5 (TOAST 738) | +13.1 [+15.8] | 56 / 17 / 0 | Immutable registrations, every read by `client_id`; deleted 30 days after the client is gone | No. No range empties, and old leaves would not reuse freed space | — | Retention 30 → 7 days (§8) |
| `network_client_location_reliability` | 189.5 | +7.8 [+7.7] | 291 / 96 / 92 | One row per client seen in 31 days; no reader has a time bound | No | — | Delete with the client; 11 days (§9.1) |
| `audit_device_event` | 74.6 | +5.4 [+5.7] | 210 / 0 / 0 | Append-only; its one reader scans every row hourly; deletes start 2027-02 | Yes | `event_time`, weekly (daily if the window is 14 days or less) | Partition, with a window (§7) |
| `provider_work_session_head` | 55.0 | +2.6 [+2.6] | 297 / 0 / 496 | A guarded per-client sequence counter | No. There is no time column, and a head lives as long as its client | — | S4 deletes (§9.3) |
| `audit_device_state` | 12.6 | +1.2 [+1.3] | 103 / 0 / 108 | One mutable state row per device | No | — | S4 deletes (§9.3) |
| `network_client` | 265.8 | +11.6 [+10.7], all index regrowth | 372 / 282 / 401 | Identity directory, updated in place (0 HOT) | No | — | No (§9.4) |
| `provide_key` | 140.0 | +5.9 [+5.2], index | 713 / 911 / 32 | Per-client keys, deleted at deactivation | No | — | No (§9.4) |
| `st_client_key_head` | 71.8 | +0.8 [+0.8] | 56 / 17 / 22 | FK to history; current-key flag | No | — | Follows history (§9.4) |

The queue tables read 0.0 because they stay at their high-water mark: they never grow and
never shrink. That is the problem §4–§6 solve.

## 3. Queues and partitions

A partition is dropped whole only when nothing in it is still needed. A queue's rows leave
one at a time, whenever their work finishes, and some wait a long time. So for each
queue-like table the questions are:
- does a range of some key ever empty?
- do its unique keys and hot statements survive that key being added?

### 3.1 Time partitions or a rotating pair

| | Time partitions (RANGE on a key fixed at insert) | Rotating pair (copy the live rows to a twin, swap the names, drop the old file) |
|---|---|---|
| Space comes back | when a whole range is empty and dropped | at each swap, all at once |
| Needs | a key fixed for the row's life, in every unique key, and ranges that empty by themselves | a small live set (it is copied under a lock), and no transaction with an older snapshot touching the table after the swap |
| Concurrency | rows never move between leaves, so claims, row locks and `ON CONFLICT` behave as on one table | the copied rows carry the swap's transaction id. A REPEATABLE READ transaction whose snapshot predates the swap sees the twin as empty, as after REPACK or a rewriting ALTER TABLE |
| Fits | history with a time retention; journals whose rows go soon after a time they carry | a queue whose rows can wait indefinitely, or whose unique key cannot take a partition key |

Two PostgreSQL rules decide most cases:
- **A unique index on a partitioned table must contain the partition key.** There are no
  global indexes in 18 or in 19. A table whose producers rely on `ON CONFLICT (k)` cannot be
  partitioned unless `k` can carry the partition key, or the identity moves to another table.
- **An UPDATE that changes the partition key moves the row** (a delete plus an insert). A
  concurrent statement that then locks or updates the old version gets a serialization error
  ("tuple to be locked was already moved to another partition due to concurrent update"), even
  in READ COMMITTED. A queue keyed on its schedule would move rows at every claim, heartbeat
  and retry.

**Verdicts:**
- `pending_task`: **rotating pair** (§4). Its run-once identity is a global unique key, its
  rows can wait for weeks, and its schedule columns change on every claim.
- `finished_task`: **time partitions** (§5). It is history, not a queue: each row is written
  once and expires by its end time. C3 is built.
- `transfer_debit_journal`: **time partitions would work** (each contract day empties), but
  the **rotating pair is cheaper and safe without a drain** (§6).
- `network_client_connection`, `network_client_location`: **neither, for now** (§9.2).

## 4. `pending_task`: a rotating pair

### 4.1 Today

- **39.3 GiB for about 118k live rows.**
  - Heap 27.8 GiB.
  - Indexes 12.4 GiB: `pending_task_function_poll_order` 6.3, `pending_task_run_once_key_key`
    4.0, `pending_task_pkey` 1.0, `pending_task_poll_order` 0.4.
  - Live rows average about 380 bytes (`pg_stats`). A compact copy is about 0.1 GiB: 45 MB of
    rows, 90 MB of heap at fillfactor 50, plus four small indexes.
- **Churn:** 31 inserts, 31 deletes and 33 updates a second, plus HOT heartbeats.
- **It does not grow and does not shrink.**
  - It was 0.6 GB after the 10-23 VACUUM FULL.
  - Each long snapshot since then kept dead rows unremovable while the queue churned: the
    11-03/04 dump held one for 3.5 h, and the O2 rebuilds 915–1,400 s each. The heap grew to
    its high-water mark, and `vacuum_truncate = off` keeps it there.
- **What the bloat costs** (04:00:44Z–04:15:11Z, `pg_stat_user_tables` and
  `pg_statio_user_tables`):
  - Autovacuum runs every 6 s (594 an hour; its threshold is 50 dead rows), plus the hot vacuum
    every 5 minutes. Together they spend 33 ms of vacuum time a second.
  - It takes 72k heap and 139k index buffer hits a second, and reads 2.7k heap pages a second
    from outside shared buffers. `transfer_contract` takes 133k heap hits a second for 19,000
    times the rows.
  - Its cumulative heap hits, 2.9 trillion, are the highest of any table.
  - CPUOPT6 Part D #8 puts the tasks family at 0.14 backends.

### 4.2 How the task runner uses it

- **Produce.**
  - `ScheduleTaskInTx` inserts with `ON CONFLICT (run_once_key) DO UPDATE`, merging a
    run-once wake into the pending row (task/task.go:467; statement at :483).
    `ScheduleTaskInTxIfAbsent` uses `DO NOTHING` (:535).
  - The batch producers queue the same insert (task/task_batch_schedule.go:14,
    task/task_batch_schedule_many.go:59).
  - `RaiseRunOnceMaxTimeInTx` updates by key (:616), and `PendingRunOnceKeys` reads by key
    (:630).
  - All of these run inside callers' transactions in every service, which are REPEATABLE READ
    by default (db.go:1090).
- **Claim.**
  - Discovery declares a NO SCROLL cursor over the eligible rows in poll order, with no row
    locks (task/task.go:2122). The query is task/claim_query.go:99, ordered by
    `available_block, run_priority DESC, run_max_time_seconds DESC`.
  - Each candidate's queue key is admitted (advisory). Then the row is rechecked and locked by
    primary key with `FOR UPDATE SKIP LOCKED`, in READ COMMITTED (:2294).
  - The claim writes `claim_time`, `release_time` and `claim_generation + 1` (:2425).
    `available_block` is generated from `run_at` and `release_time`, so a claim moves the row
    in both poll indexes.
- **Lease.**
  - The heartbeat writes `claim_time` on every beat. That update is HOT, since fillfactor 50
    leaves room.
  - It extends `release_time` once per half lease, under `FOR UPDATE OF pending_task SKIP
    LOCKED` (task/task.go:2579).
  - A session advisory lock, not the timestamp, keeps a second live owner out.
- **Retry.** A failed run rewrites the same row, fenced by `claim_generation`
  (task/task_completion.go:100): `reschedule_error`, the error count, and `run_at` at the
  exponential backoff (capped at an hour, with jitter).
- **Finish.**
  - One statement moves the row: `DELETE ... WHERE task_id AND claim_generation RETURNING *`
    feeding `INSERT INTO finished_task ... WHERE NOT EXISTS` (task/task_run_once.go:23; batch
    form task/task_completion_batch.go:38).
  - It runs in the completion transaction: REPEATABLE READ for ordinary targets
    (task/task_completion.go:52), READ COMMITTED for owned ones.
  - A run-once wake that arrived during the run inserts a successor from the finished row
    (task/task_run_once.go:74).
- **Other writers.**
  - Closers delete queued scheduled closes by `ctid`, skipping locked rows
    (model/subscription_contract_scheduled_close_queue.go:31).
  - The scheduled-close reap (taskworker/work/scheduled_contract_close_reap.go:214) and the
    provider totals drain (model/subscription_provider_totals_drain.go:199) delete by id.
  - Operator commands: `RemovePendingTask`, `ReleaseTask`, `KickTasks` (task/task.go:1029,
    :1080, :1104).
  - Idle wakes, checkpoints and run-once arguments update by id (task/task_idle_wake.go:199,
    task/task_checkpoint.go:44, task/task_run_once_args.go:78).
- **Only the taskworker claims.** `task.NewTaskWorker` has one caller
  (taskworker/taskworker.go:337). Its profiles narrow what it claims
  (taskworker/claim_admission.go:77).

### 4.3 Why not partitions

PARTITION-MAINTENANCE-PLAN.md C7 still holds, and the code since adds to it:
- `UNIQUE (run_once_key)` is the conflict target of all four run-once producer statements
  above. A partitioned table can only enforce `(run_once_key, key)`.
- Rows can wait indefinitely (backoff, a far-future `run_at`, a chain parked on a missing
  target), so no range of any fixed key empties. On 10-23 the oldest row dated from 08-16.
- Keyed on `available_block`, every claim, lease extension and retry would move the row
  between leaves. The recheck at task/task.go:2294 would then meet serialization errors
  instead of skipping.
- Two writers delete by `ctid`, and a ctid repeats across leaves.

Partitioning it would take a separate run-once identity table and a claim cursor that merges
every leaf. That is a rewrite of the scheduler's core, for a 118k-row table.

### 4.4 Design: a drained swap into a twin

One operator command builds a twin, copies the live rows into it and swaps the names in one
short transaction, while the taskworker is stopped. The old table is dropped afterwards.
Repeat whenever the table-bloat signal (monitor/SIGNALS.md §2.4k) fires again.

**The command:** `bringyourctl model migrate table-swap --table=pending_task [--dry-run]
[--rollback] [--drop-old]`. It sits beside the other cutover commands and keeps their
conventions: resumable steps, bounded lock waits, and a dry run that reports and changes
nothing. A per-table profile in code holds the stored column list, the drain requirement, the
lock bounds and the canonical index and constraint names. §6 reuses it.

1. **Twin.**
   - `CREATE TABLE pending_task_next (LIKE pending_task INCLUDING ALL)`. That copies the
     columns in order, the three generated columns (`run_at_block`, `available_block`,
     `has_reschedule_error`), defaults, the primary key, `UNIQUE (run_once_key)` and the four
     indexes.
   - Then it sets what `LIKE` does not copy: the reloptions (`fillfactor=50` and the seven
     autovacuum settings now on the table), the owner and the grants.
   - The command creates the twin, rather than a migration, so a rotation can be repeated.
2. **Preflight**, read-only. It checks:
   - the twin matches the table column for column: types, generated expressions, defaults,
     nullability, index definitions, constraints, reloptions and grants;
   - the twin is empty;
   - no taskworker session is connected (`pg_stat_activity` by application name);
   - no row has a live lease (`release_time > now()`);
   - no transaction older than 60 s is open.
3. **Swap**: one transaction with `lock_timeout = 3s` and `statement_timeout = 60s`, retried up
   to 20 times.
   - `LOCK TABLE pending_task, pending_task_next IN ACCESS EXCLUSIVE MODE`.
   - `INSERT INTO pending_task_next (<the 18 stored columns>) SELECT <same> FROM pending_task`,
     with `enable_seqscan = off`. The copy then reads the 118k rows through the 1 GiB primary
     key instead of scanning 27.8 GiB of heap. The generated columns recompute to the same
     values.
   - Check that the copied count equals `count(*)` of the source, and that a hash over every
     stored column, in `task_id` order, matches.
   - Rename `pending_task` to `pending_task_old_<UTC>`, and the twin to `pending_task`,
     including their indexes and constraints. Every name the code, the monitor and the hot
     vacuum use stays the same; monitor/migration_current_suffix.go:249, for example, pins
     `pending_task_function_poll_order`.
   - `ANALYZE pending_task`, so the first poll plans with statistics.
   - Commit. The lock is held for the copy: a few seconds.
4. **Restart the taskworker.** Rows keep their `task_id`, `claim_generation`, leases and
   run-once generations, so nothing that was in flight before the stop is orphaned.
5. **`--drop-old`**, after a verification period (default: the next day). `DROP TABLE
   pending_task_old_<UTC>` returns the 39 GiB.

**Why the taskworker must be stopped.** The copied rows carry the swap's transaction id. A
transaction whose snapshot was taken before the swap commits, and which first touches
`pending_task` after it, sees the twin as empty. A statement that waited on the swap's lock
took its snapshot before it started waiting.
- **Completions.** An ordinary completion is REPEATABLE READ (task/task_completion.go:52). Its
  fenced `DELETE` would find no row and raise `errTaskClaimOwnership`, and the task would run
  again after its lease. Retries and heartbeats are fenced the same way. With the taskworker
  stopped, none of them is in flight.
- **Producers in other services** either insert a new key, which is unaffected, or meet a
  copied row their snapshot cannot see. PostgreSQL raises a serialization failure for `ON
  CONFLICT` on an invisible row, and `server.Tx` always reruns 40001 with a fresh snapshot
  (db_retry_taxonomy.go:75).
- **A closer's `ctid` delete** with an old snapshot removes nothing. The queued close then
  finishes on its claim as a terminal no-op, as it already does for a locked row.

**The stop** follows the 18.7 runbook's step:
- `docker stop -t 60` on every taskworker container. Then `docker ps` must list none: a unit
  stop left containers running on 10-13.
- Under the settlement gate (`ST_QUIET_MIN=0`), with the validator idle and at least 15
  minutes from its boundary.
- Tasks pause for 2–4 minutes in all, and due work catches up on restart, as after any full
  stop. Connect handlers keep heartbeating through the connect service, so none expire.

**A drain-free variant, if rotations become frequent.**
- A mirror trigger on `pending_task` applies every row change to the twin in the same
  transaction; the C2 cutover's change capture is the precedent. It runs after a batched
  backfill that copies under `FOR SHARE` row locks.
- Rows changed through the trigger carry the same transaction ids as the originals. The
  backfill's rows become older than any open snapshot once `transaction_timeout` (15 minutes)
  has passed. So the final rename is MVCC-safe, and needs no stop.
- The cost: a second write per queue change while mirroring, and more code (the trigger, the
  backfill, the compare, the rollback). A first rotation does not need it.

### 4.5 Readers, writers and the monitor

- **No reader or writer changes.** Names, columns, keys and indexes are identical. Cached plans
  are invalidated by the rename and re-planned against the new table.
- `pg_stat_user_tables` counters start again from zero for the new relation. Before the first
  rotation, check that the signals reading `pending_task` counters as deltas treat a relid
  change as a reset (monitor/signal_table_bloat.go, monitor/signal_task_lock_chain.go).
- The hot vacuum list (db_maintenance_hot_vacuum.go:111) and the REINDEX rotation address the
  table by name. Once compact, it is eligible again for the hourly index pass: its heap is
  under `IndexMaxHeapBytes`, 8 GiB.
- `db audit` and the monitor's catalog contracts must ignore `*_next` and `*_old_*` tables
  during a rotation, the way `CutoverTables` keeps a cutover's staged and old tables out of db
  audit (db_partition_registry.go:373).

### 4.6 Indexes, keys, foreign keys

Unchanged: `PRIMARY KEY (task_id)`, `UNIQUE (run_once_key)`, and the two poll indexes. No
foreign key references `pending_task`, and it references none.

### 4.7 Autovacuum and WAL

- **Autovacuum:** the same cadence (a threshold of 50 dead rows), but each pass reads a few
  thousand pages instead of up to 3.6M, and each index pass about 30 MB instead of 12.4 GiB.
  The poll stops stepping over dead entries spread across a 6.3 GiB index.
- **WAL:** the swap writes about 0.2 GB (the rows and index builds), and the drop almost none.
- **Backups:** the edge-6 base and incremental backups lose 39 GiB.

### 4.8 Rollback

- **While the old table exists,** `--rollback` is the same swap in reverse: stop the
  taskworker, empty the old table with `TRUNCATE` (nothing reads it), copy the current live
  rows back, swap the names, and restart.
- **After `--drop-old`,** a rollback is simply another swap into a fresh twin.

### 4.9 Tests

Deterministic, on the disposable DB test services (`test-env.sh`), under `task/` and the root
package.
- **Preflight:** it accepts the twin it creates, and refuses each single difference (a missing
  index, a different reloption, a missing grant, a non-empty twin). It also refuses while a
  taskworker session or a live lease exists.
- **Swap:**
  - Seed claimed, leased, backed-off, run-once-merged and far-future rows. Every stored column
    is identical after the swap.
  - A task claimed before the stop completes after the restart.
  - A run-once producer merges into the swapped row.
  - `ON CONFLICT` from a REPEATABLE READ transaction whose snapshot predates the swap gets a
    serialization failure and succeeds on rerun (ordered with barriers).
- **Lock contention:** a held `pending_task` lock makes the swap time out at its
  `lock_timeout` and retry, and it never queues longer than that bound.
- **Crashes:** a crash between copy and rename rolls back and leaves the twin empty. After a
  commit, the next run finds the swap done and offers only `--drop-old`.
- **`--rollback`** restores the old table, including the rows written after the swap.

### 4.10 Estimate

- **Space:** 39.3 GiB down to about 0.1–0.3 GiB. **About −39 GiB once**, returned at
  `--drop-old`.
- **Vacuum and IO:** most of 33 ms of vacuum a second, 72k heap and 139k index buffer hits a
  second, and 2.7k page reads a second go, roughly in proportion to the size.
- **Durability:** it re-bloats only when a long snapshot pins the horizon during heavy churn.
  - At today's 95 row versions a second, a 2-hour pin (an I3 rebuild) adds about 0.5–1 GiB
    *(inferred)*. The 28 GiB came from far larger events.
  - Repeat the rotation if the bloat signal fires again.
  - After PostgreSQL 19, REPACK (CONCURRENTLY) is an alternative for a table this small, with
    the same need for a drain around its swap.

## 5. `finished_task`: cut over C3

### 5.1 State

C3 was decided and built on 10-24 (PARTITION-MAINTENANCE-PLAN.md C3). It has never been
prepared on Main: there is no staged parent and no legacy bound check (read 11-16 03:58Z).
Everything it needs is deployed:
- The primary key is `(task_id, run_end_time)`, and `finished_task_post_retry` exists (the
  three migrations at db_finished_task_partition.go:150–175).
- Both completion moves refuse a task id that already finished in any partition
  (`errFinishedTaskIdentityExists`; task/task_run_once.go:23, task/task_completion_batch.go:38).
- The family is registered (task/finished_task_partition.go):
  - daily by `run_end_time`, 3 days ahead;
  - a day drops 7 days after its upper bound, after its failed Posts are released through
    their finished owners;
  - the drop is fenced by ACCESS EXCLUSIVE with a 2 s wait, and defers on any older lock holder.
- `TaskCleanup` deletes nothing once the table is partitioned. It only makes sure today and
  tomorrow have a leaf (task/task_cleanup.go:251).

### 5.2 Readers and writers

- **Writers.**
  - Only the two moves insert (task/task_run_once.go:23, task/task_completion_batch.go:38).
  - Updates: a failed Post's `post_error` (task/task_completion.go:141); RunPost's
    acknowledgement (task/task.go:1994); checkpoints (task/task_checkpoint.go:73); the egress
    tally's result (taskworker/work/provider_egress_tally_work.go:205).
  - Deletes: `TaskCleanup` (task/task_cleanup.go:84), which stops after the cutover.
- **Readers by task id.** Each costs one index probe per leaf, about 12:
  - `GetFinishedTasks` (task/task.go:806);
  - the run-once wake and reschedule copies (task/task_run_once.go:87, :122);
  - owner validation (task/task_completion_ownership.go:70);
  - checkpoints (task/task_checkpoint.go:65);
  - the profile claim's RunPost check (task/claim_query.go:58);
  - the egress tally (taskworker/work/provider_egress_tally_work.go:149).
- **Readers by time,** which prune to the leaves they need:
  - `ListFinishedTasks` (task/task.go:1011);
  - the monitor's task checks: monitor/task_checks.go:637, :680, :1082, :1169, :1177, :1244,
    :1253; monitor/signal_close_duration.go:69; monitor/signal_netescrow.go:207;
    monitor/signal_pg_autovacuum_discard.go:236; monitor/signal_reboot_collision.go:317;
    monitor/signal_subscription_metrics.go:41; monitor/postgres_checks.go:530.
  - A reader without a `run_end_time` bound probes every leaf; none reads past the retention.
- **No code changes.** One check before the cutover: run the monitor's task checks against a
  partitioned test schema. The existing DB tests cover the task package's readers.

### 5.3 Keys and indexes per leaf

- Each leaf carries the parent's three indexes: the `(task_id, run_end_time)` primary key,
  `(run_end_time, task_id)`, and the partial failed-Post index.
- Uniqueness of `task_id` alone cannot be enforced across leaves. Instead the fenced move is the
  only writer: it deletes the pending row it copies under `claim_generation`, and refuses a
  task id already present (§5.1).
- No foreign keys in either direction.

### 5.4 Online migration

The command is built: `bringyourctl model migrate finished-task-partition`.
1. **Prepare**, with `--cutover-day=C` at least 12 hours ahead
   (db_finished_task_partition.go:105).
   - It stages `finished_task_staged` with every index and the days C..C+3.
   - It adds and validates `CHECK (run_end_time < C)`. The validation scans the 27.8 GiB heap
     under SHARE UPDATE EXCLUSIVE, so completions continue. It holds a snapshot for the scan,
     about 1–2 minutes.
   - It is resumable.
2. **Finalize**, before C − 2 h.
   - It proves, in a rolled-back transaction, that every parent index adopts a legacy index.
   - Then it swaps in one catalog-only transaction: the table becomes `finished_task_legacy`,
     and the staged parent becomes `finished_task`. The validated check lets ATTACH skip its
     scan. Lock waits are 2 s, with up to 60 attempts.
3. **From C,** completions land in `finished_task_pYYYYMMDD`.
4. **The registry retires the legacy leaf at C + 7 days** like any other day, after releasing
   its failed Posts. Each later day goes 7 days after its end.

No rows are copied, so no task is written twice and the MVCC concern of §4 does not arise.

### 5.5 Drops, guards and monitoring

All reused as built:
- the registry's guarded drop (`DropRange`): guard, fence, detach, the precondition again on the
  detached leaf, then the drop, all in one transaction;
- the archive step's owner-admitted release of failed Posts;
- the monitor's §2.4p `partition-runway` and §2.4q `partition-retention-blocked` probes, which
  cover every registered family without a dedicated probe.

The three migrations' catalog contracts (monitor/migration_finished_task_partition.go) hold on
the plain table, on a prepared cutover and on the partitioned parent.

### 5.6 Autovacuum and WAL

- `TaskCleanup`'s 31 deletes a second stop. Retention then creates no dead rows, no index
  cleanup and no WAL.
- The day leaves are insert-mostly, and their few updates are HOT.
- A day goes by `DROP`, which writes almost no WAL.
- The hot vacuum expands the parent to its small leaves (db_maintenance_hot_vacuum.go:327).

### 5.7 Rollback

- **Before C,** `--abort` drops the staged parent and the check; nothing has landed in a day
  leaf yet.
- **After C,** `--rollback` copies the daily leaves' rows back into the legacy table under its
  lock, checks the count, refuses a task id present in both, and swaps back.
  - Its default limit is 200,000 rows (db_finished_task_partition.go:111), about 1.8 hours of
    completions at 31 a second.
  - Beyond that, raise `--max-copy-rows` and accept a longer stall. A full day, 2.7M rows, is
    about a minute under the lock.

### 5.8 Tests

- **Built with C3:**
  - task/finished_task_cutover_db_test.go, task/finished_task_partition_db_test.go,
    db_finished_task_partition_test.go, task/finished_task_partition_test.go,
    monitor/migration_finished_task_partition_test.go.
  - They cover the retention classes with a fixed clock, a drop racing a Post retry, one task id
    with two end times, an interrupted cutover overlapping a completion, and lookups probing
    each leaf.
- **Add before the run:**
  - the monitor's finished_task readers against the partitioned schema;
  - this family in db_maintenance_hot_vacuum_partition_test.go, which already covers a
    partitioned table with a locked leaf.

### 5.9 Estimate

- **Space:** −20 GiB at C + 7 days.
  - Today: 34.1 GiB (heap 27.9) for 9–14M live rows (the estimate moves with each analyze).
  - After the cutover the table holds 8 days of completions. At 2.65M a day and about 590 bytes
    a row (`pg_stats` widths, weighted by null fractions), that is about 21M rows, 12 GiB of
    heap and 1.5 GiB of indexes.
  - So **about 13–14 GiB at steady state, about −20 GiB**, from C + 7 days when the legacy leaf
    drops.
  - Until then, the new days add up to about 13 GiB on top of the legacy leaf.
  - DBDISK §5.1's "4–6 GiB", and so its −29, assumed about 200 bytes a row.
- **Vacuum and IO:** no retention deletes, and once the legacy leaf drops its 28 GiB heap is no
  longer vacuumed every 5 minutes.
- **Not worth it:** keeping successful history 1 day instead of 8 would save about another
  10 GiB, but it needs failed Posts moved to a side table, the alternative C3 did not choose.

## 6. `transfer_debit_journal`: the same swap, without a drain

### 6.1 Today

- **Size:** 7.23 GiB (heap 7.04, indexes 0.14 after the hourly rebuilds) for 5–10k live rows
  (DBDISK: 36–57k).
- **Churn:** 1,318 inserts, updates (93% HOT) and deletes a second.
- **Vacuum:** autovacuum every 6 s (602 an hour) plus the hot vacuum, 14 ms of vacuum a second
  in all, and 49k heap buffer hits a second.
- **Dead rows:** about 490k right after each vacuum. CPUOPT6 reads this as pages pinned by the
  constant shard scans.
- **The age gauge** costs 1,988 buffer hits a call (CPUOPT6, 11-15), against 587 when the table
  was compact (CPUOPT5).

### 6.2 How it works

- **Table:** migration v758, db_transfer_debit_journal.go:7–30.
  - `PRIMARY KEY (balance_id, contract_id)`, a `shard` check, and indexes on
    `(shard, balance_id, contract_id)`, `(shard, create_time)` and `(contract_id, balance_id)`.
  - `fillfactor = 70` (db_write_path_migrations.go:84).
  - No foreign keys in or out. The only trigger is on `transfer_balance`: deleting a balance
    with a pending debit raises (db_transfer_debit_journal.go:20–26).
- **Writers:**
  - one row per escrow balance at a contract's terminal outcome (model/subscription_model.go:4264);
  - the deadline close (model/subscription_contract_scheduled_close.go:270).
- **Apply and delete:** `UPDATE ... SET applied=true` (model/subscription_transfer_debit.go:278),
  then a delete (:315). These run on 16 shard chains, every 2 s while active
  (taskworker/work/settlement_chain_cadence.go:52–60).
- **Readers:**
  - the shard walk (model/subscription_transfer_debit.go:136, :139);
  - the page read, with `FOR UPDATE SKIP LOCKED` (:240);
  - the age gauge (:45) and the wake check (model/subscription_transfer_debit_wake.go:29);
  - the balance guards (model/subscription_balance_retention.go:48–62,
    model/prober_shard_model.go:623);
  - Redis admission, recovery and expiry repair, by contract (model/subscription_redis_admission.go:657,
    model/subscription_redis_recovery.go:208, model/subscription_contract_expiry_repair.go:152);
  - deadline sums (model/subscription_contract_scheduled_close.go:54);
  - monitor §2.5a (monitor/signal_transfer_debits.go:27).
- **Every statement on it is READ COMMITTED or autocommit:**
  - the writers, the flush, both balance deletes, and Redis recovery
    (model/subscription_redis_recovery.go:483) are READ COMMITTED;
  - expiry repair runs through the READ COMMITTED `contractExpiryContinuationTx`
    (model/subscription_contract_expiry_repair.go:166);
  - the gauge, the wake check, admission reconcile and the deadline reads are autocommit
    `server.Db` statements;
  - the one exception is the operator's read-only move plan
    (model/transfer_balance_code_move_model.go:214), which is re-read under locks at commit.
- **No `ON CONFLICT`, `ctid`, TRUNCATE or LOCK TABLE** touches it. DBDISK §5.1 I5's "its ON
  CONFLICT stays exact" is wrong: there is none.

### 6.3 Design: swap (recommended) or daily partitions (alternative)

**Swap:** `bringyourctl model migrate table-swap --table=transfer_debit_journal`, the command
of §4.4 with a different profile.
- **No drain.** A READ COMMITTED statement that waited on the swap's lock re-plans against the
  renamed table and takes its snapshot after the commit, so it sees the copied rows. Settlement
  keeps running.
- **Lock bound:** `lock_timeout = 250ms`, the flush's own lock bound
  (model/subscription_transfer_debit.go:222–279), retried until the lock is granted. The copy
  reads about 10k rows through the 36 MB primary key in 0.1–0.3 s. Settlement inserts wait that
  long, once.
- **The twin** carries `fillfactor=70` and the A1 autovacuum options, and its names are
  restored. The monitor contracts then pass unchanged: monitor/migrations_async_debit.go:24–28
  and :64–84 (exact `pg_get_indexdef`, no FK, no trigger), and
  monitor/migration_current_suffix.go:776–782 and :1044–1048 (fillfactor).
- **The `transfer_balance` trigger** re-plans by name and reads the new table.

**Daily partitions** (I5, corrected) are the durable alternative if the swap has to be repeated
often:
- **Layout:** `RANGE (contract_id)` on contract-day bounds (`contract_id_floor`,
  db_contract_partition.go:20–26), 3 days ahead, plus a DEFAULT leaf for late closes of old
  contracts (operator scans, startup closure, deadline backlog, expiry repairs). The legacy leaf
  is today's table. A day drops once it is 2 days past and empty.
- **Why it would work:** contracts live 60 minutes (model/subscription_contract_expiration.go:14),
  so a day takes its last normal debit about 75 minutes after it ends.
- **Template:** the close-evidence family, which has an id key, a DEFAULT leaf and a legacy leaf
  (db_contract_close_evidence_partition.go:137–177).
- **What it costs:**
  - a daily DETACH under ACCESS EXCLUSIVE on the parent (no CONCURRENTLY with a DEFAULT leaf),
    which queues settlement inserts for up to its lock bound;
  - a daily ATTACH that locks and scans the DEFAULT leaf;
  - a cutover CHECK `contract_id < floor(C)` that would refuse every debit of a post-C contract
    if finalize slipped past C (it needs the abandon margins C3 and C5 use);
  - every shard and balance statement probes every leaf;
  - rewritten monitor contracts: the parent's index definitions read `ON ONLY`, and a
    partitioned parent cannot carry reloptions;
  - test changes at db_write_path_migration_test.go:82 and :112–126,
    model/write_path_plan_test.go:84–85, monitor/migration_current_suffix_test.go:273–277 and
    monitor/signal_migrations_merge_test.go:81–84. `transferDebitJournalSchemaSql`'s published
    body stays pinned (db_migration_artifact_bodies.go:13–15).
- **Effort:** about 2–3 days, against +0.5 day for the swap.

**Recommendation: the swap now.** Revisit partitions only if the bloat signal fires again after
the I3 rebuild windows.

### 6.4 Keys, indexes, foreign keys

For the swap, unchanged. For partitions, the primary key `(balance_id, contract_id)` already
contains the key, and there are no foreign keys.

### 6.5 Autovacuum, WAL, rollback, tests

- **Autovacuum:** the same cadence; each pass reads a few MB instead of 7 GiB.
- **WAL:** the copy writes a few MB.
- **Rollback:** as in §4.8. No stop is needed.
- **Tests** (the §4.9 set, plus):
  - a swap with debit inserts, applies and deletes running concurrently (barriers), with no debit
    lost or applied twice;
  - `protect_pending_transfer_debit` right after the swap sees the copied rows;
  - the async-debit monitor contracts pass on the swapped table;
  - a preflight that does not require a taskworker stop for this profile.

### 6.6 Estimate

- **Space:** −7 GiB once.
- **CPU:** the gauge drops from 1,988 to about 600 hits a call, and vacuum from 14 ms/s to a
  fraction. This is part of CPUOPT6's 0.15–0.3 cores. The walk itself is already cheap (28 hits a
  call, CPUOPT5), so the saving is mostly the gauge and vacuum.
- **Durability:** it re-bloats with long pins, as `pending_task` does. At about 4k row versions a
  second, a 2-hour pin adds about 2–3 GiB *(inferred)*.

## 7. `audit_device_event`: partition by `event_time`, with a window

### 7.1 Today

- **Size:** 74.6 GiB.
  - Heap 27.1 GiB.
  - Indexes 47.5 GiB: `(device_id, event_time, event_id) INCLUDE (event_type)` 22.6,
    `PRIMARY KEY (event_time, device_id, event_id)` 17.1, `UNIQUE (event_id)` 7.8.
- **Growth:** +5.4 GiB/day at about 210 inserts a second, arriving in bursts every 15 minutes
  (the sweep stamps one time per pass). No deletes until 2027-02-06, 180 days after its first
  rows of 08-10.
- **The volume has grown thirteenfold:** 87.6M events from 08-10 to 10-12, about 1.4M a day
  (monitor/SIGNALS.md §5.7), and about 18M a day now. At 5.4 GiB/day, the 180-day steady state
  is about 1 TiB. PARTITION-MAINTENANCE-PLAN.md C6's "levels off near 75 GB" is out of date.
- **The export has hit its limit before:** on 10-12, at 87.6M rows, `ExportStats` timed out at
  600 s while reading every retained row (monitor/SIGNALS.md §5.7). Migrations 830 and 831
  followed. The table now holds three times as many rows.

### 7.2 Writers and readers

- **DDL:** db_migrations.go:303–315; the device index from migration 830 (:10154–10163). No
  foreign keys, no triggers, no reloptions.
- **One live writer:** `SweepDeviceAuditEvents` (model/audit_provider_sweep_model.go:392–530,
  insert at :479–494), run every 15 minutes (task `sweep_provider_audit_events`,
  taskworker/work/audit_work.go:118).
  - It writes `device_added` or `device_removed` for each change it sees between connected clients
    and `audit_device_state`, and re-asserts online devices every 30 days (:66).
  - It has no filter for hosted window children. About two events per new short-lived client
    explain the rise *(inferred)*.
  - The backfill (:552–700) reaches back 10 days at most and does nothing on Main.
  - No statement uses `ON CONFLICT`, so nothing relies on `UNIQUE (event_id)`. That index was
    scanned 1,096 times since the reset.
- **One reader:** `computeStatsDevice` (model/audit_model.go:410–531), in the hourly
  `ExportStats` (taskworker/work/audit_work.go:46, max 10 minutes at :53), which builds the Redis
  blob `stats.last-90`.
  - It has no `event_time` lower bound. Its carry-in (model/audit_model.go:489) takes each
    device's latest event before the window, so every run reads every retained row: 266M today,
    and 18M more each day.
  - *(inferred)* At this rate the export outgrows its 10-minute limit before the disk fills.

### 7.3 What retention the reader needs

- **With the current reader,** at least the 90-day window plus the 30-day reassert plus one
  sweep: **about 121 days** (pick 125).
- **90 days** works only if the reassert interval drops to 7 days or less.
- **A per-day rollup needs only days of raw events.** The export would read finalized daily
  counts from a small table, plus today's carry-in from `audit_device_state`. Raw retention could
  then be 7 days.
- **Use a device-only constant.** `AuditEventExpiration` (model/audit_model.go:1224) is shared
  with the provider, extender and contract feeds and both state cleanups.

### 7.4 Design

**Family** `audit_device_event`:
- `KeyKind: PartitionKeyTimestamp` on `event_time`.
- Granularity: **weekly** for a window of 90 days or more (leaves of about 38 GiB), **daily** for
  14 days or less (about 5.4 GiB). Ahead: 2 weeks, or 3 days.
- `Retention` = the decided window.
- No DEFAULT leaf, and `CreateFrom` = now − 10 days. Inserts carry the sweep's clock, and the
  backfill reaches back 10 days, so every insert finds a leaf.
- `ReindexExempt`. Today the REINDEX rotation rebuilds this append-only table every few days
  (db_maintenance.go:55, :673), which is wasted work.
- The parent analyzed per range period: the export's chain split reads `pg_stats`.

**Keys and indexes:**
- The primary key `(event_time, device_id, event_id)` already contains the key and stays.
- The device index goes on the parent, so each leaf gets one.
- **`UNIQUE (event_id)` goes:** ids are `server.NewId()`, unique by construction, and nothing
  depends on it. Drop it on the legacy table before the attach, for **−7.8 GiB at once**. That
  takes `ALTER TABLE ... DROP CONSTRAINT` with a short lock wait.
- No foreign keys.

**Cutover,** copying C4's (db_provider_work_session_partition_cutover.go:13–44):
1. **Prepare:** a validated `CHECK (event_time < C)` on the live table. It scans the 27 GiB heap
   under SHARE UPDATE EXCLUSIVE, so sweep inserts continue. Then stage the parent without the
   unique index (`LIKE ... INCLUDING INDEXES` would carry it), plus leaves from C.
2. **Finalize** before C, in one catalog-only transaction with bounded lock waits: rename the
   table to `audit_device_event_legacy` and attach it `FROM (MINVALUE) TO (C)`, adopting the
   primary key and the device index.
3. **At C,** turn off the hourly row delete for this table. Deleting rows from a leaf that takes
   no inserts returns no disk.
4. **The legacy leaf** retires through the registry at C + `Retention`.

**Reader changes:**
- Give the export's chunk-end and span statements an `event_time` lower bound of
  window start − reassert − 1 day, so they prune leaves.
- With the rollup, the export reads the rollup table and today's leaves only.

**Monitor and audit:**
- Contract 830 pins the exact index definition (monitor/migration_current_suffix.go:438,
  :1073–1076). Accept the partitioned form by adding the table to the partitioned-table list
  (monitor/migration_contract_partition.go:43).
- `db audit` needs a cutover acceptance (db_audit.go:438–442).
- The §2.4p and §2.4q probes cover the new family.

**Drops:** the registry's `DropRange` (detach and drop in one transaction). The family is
append-only and has no owner to fence, so the default guard suffices.

### 7.5 Autovacuum, WAL, rollback

- **Autovacuum:** insert-only leaves get insert-triggered freeze vacuums only.
- **WAL:** the 180-day row delete never starts. From 2027-02 it would have deleted 18M rows a day,
  each with WAL and index cleanup. The REINDEX rotation stops touching the table.
- **Rollback, before C:** abort, which drops the staged parent and the check.
- **Rollback, after C:** copy the new leaves' rows back into the legacy table under a write lock,
  and swap back. Rows are small (about 80 bytes), but a week is about 125M rows, so a rollback
  belongs in the first day, or not at all; fix forward instead.

### 7.6 Tests

- **Update:**
  - taskworker/work/async_maintenance_correctness_test.go:161, which inserts rows dated 2000 and
    2099: give the test schema a DEFAULT leaf, or move the dates;
  - model/audit_device_stats_query_test.go:203–204, which matches on the relation name;
  - db_audit_device_index_test.go and model/audit_model_test.go.
- **New:**
  - the export with a fixed clock on the partitioned shape, with carry-in across leaves;
  - pruning of the bounded export statements (EXPLAIN in the test);
  - a sweep insert at a leaf boundary;
  - a backfill into a leaf 10 days back;
  - a drop at the retention boundary and not before;
  - a cutover with concurrent sweep inserts;
  - with the rollup: daily counts finalized once, and a rerun across midnight that counts once.

### 7.7 Estimate

The window sets the size. At 5.4 GiB/day:

| Window | Steady state |
|---|---|
| 180 days | about 970 GiB |
| 125 days | about 675 GiB |
| 90 days (needs a 7-day reassert) | about 490 GiB |
| 14 days (needs the rollup) | about 75 GiB |
| 7 days (needs the rollup) | about 40 GiB |

- **Partitioning alone saves nothing until C + window,** apart from the 7.8 GiB unique index.
- **With the rollup and 7 days,** the legacy leaf (about 80 GiB at C) drops at C + 7. The table
  then holds about 40 GiB: −35 GiB against today, and about −900 GiB against the 180-day path by
  May.
- **The inflow is the other lever.** If hosted window children should not count as devices, the
  sweep can skip them, which cuts the inflow several-fold *(inferred)*. That is a product
  decision (§10).

## 8. `st_client_key_history`: retention, not partitions

### 8.1 Today

- **Size:** 986.5 GiB.
  - Heap 195.5, TOAST 738 (the sealed evidence, about 2.8 KB a row).
  - Indexes 52.5: the primary key 31.9, `registration_hash` 20.6.
- **Rows and growth:** 282.5M rows; +13.1 GiB/day (TOAST +12.6).
- **Churn:** 56 inserts a second. The retention deletes 17.6 clients a second (Mimir,
  `urnetwork_retention_deleted_rows_total`).
- **Schema:** db_client_key_history.go:9–19 (v651); v744 made the primary key
  `(client_id, domain_hash, generation)` and gave `st_client_key_head` a foreign key into it
  (db_client_key_policy_history.go:9–21).
- **Guards:** a row trigger refuses UPDATE and DELETE (db_client_key_history.go:34–36), and a
  statement trigger refuses TRUNCATE (db_identity_retention.go:115–117).
  - A delete passes only under `urnetwork.client_key_history_retention = on`, for a client gone
    longer than `st_client_key_history_retention_floor()` (30 days, db_identity_retention.go:159–162)
    that has no head and no client row left (:163–184).

### 8.2 Writers and readers

- **One writer.** It inserts a row and upserts the head (model/st_client_key_history.go:242–250),
  only from a `ClientKey` frame on connect (controller/st_client_key_history.go:138–177).
  - An unchanged key writes nothing. A rotation adds a generation, and a client is capped at 1,024
    rows (:197–234).
  - **Hosted window children stopped publishing keys on 11-02** (proxy/proxy_device.go:1643–1647,
    1f72fe5ca). They had been 70–94% of inserts.
- **One deleter:** `remove_gone_st_client_key_history` (model/st_client_key_history_retention.go:320–374).
  - It works in batches of 500 gone records on a `(gone_time, client_id)` cursor, deleting heads,
    then history, then the records.
  - The brake `{client_key_history_retention}control` defaults to 150 clients a second (2,000 at
    most) and is re-read every batch.
  - The horizon is clamped to 30–91 days (:84–108).
- **Readers**, each keyed by `client_id`:
  - **Close-report admission** (model/contract_close_original.go:94–96): all of a client's
    generations, for a contract that is still unresolved and undisputed, with no report retained
    (model/contract_close_report.go:183–190).
    - The admitted report keeps its own copy of the registration (:396–400). SN close originals
      read that copy (model/st_provider_close_originals.go:70–81), and disputes are refused.
    - So admission needs a gone client's keys only while a contract that was open when the client
      went can still close: **60 minutes** (model/subscription_contract_expiration.go:14) plus
      sweep lag.
    - The measured worst case is a report 3.3 s after the client went.
  - **Current-key readers,** active clients only, any age:
    - `GetClientPublicKey` (model/network_client_key_model.go:24–32);
    - `GetClientContractMetadata`, on every CreateContract (model/client_contract_metadata.go:34–45);
    - `RetainProviderWorkOwner` (model/provider_work_owner.go:90);
    - `LoadStClientKeyHistory`, for SN validator observations and `GET /key/<id>/history`
      (model/st_client_key_history.go:262–350).
  - **The SN epoch key overlay** reads the current heads of an epoch's providers
    (model/st_client_key_history.go:434–480, via model/st_model.go:2183). It drops a retired
    client, so it needs nothing from a gone one.
  - **No other reader.** MinIO publication has no database cursor; there are no exports; no SN25
    settlement, dispute or audit code reads the table.
  - The floor's own reader list (db_identity_retention.go:126–141) says the same: "a gone
    client's last report is admitted within hours of its going, not days."

### 8.3 Why not partitions

- **By registration time:** there is no time column, no reader has a time bound, and an active
  client's first generation would pin an old leaf.
- **By `client_id` mint week:** the primary key and the head's foreign key keep their text, and
  every reader prunes. But **no week ever empties**. Every week mints app, provider and SN
  clients that live for months and keep their rows.
  - Space freed in old leaves would not be reused, because new ids go to new leaves. That is worse
    than today, where freed space is reused in place.
  - Making leaves drop would need survivors moved out: a class column in the primary key and the
    head's foreign key plus a move guard, or a separate durable table with every reader changed.
    Or it would need each old leaf rewritten after its cohort is deleted.
- **Force-dropping** an active client's rows is not an option: its next registration would sign a
  second generation 1 (model/st_client_key_history.go:220–228).
- So partitioning only pays off with a retention rule that ages rows by mint time. That would end
  the keys of long-lived clients, which every current-key reader needs.

### 8.4 Retention proposal (owner decision)

**7 days after the client is gone, instead of 30.**
- **Why 7:** every reader of a gone client's keys is bounded by the 60-minute contract life
  (§8.2). Seven days covers a week-long outage of the deadline close and expiry repair, and
  incident recovery.
  - The contracts that ran 10 days 16 hours (weeks of 09-28 and 10-05) all expired by 10-22.
  - 30 days was the owner's margin when the floor followed the 35-day contract horizon (11-01/02).
- **Effect:**
  - The bulk of 113M clients gone 10-24..11-03 becomes due at once, instead of 11-23..12-03. It
    frees about 360–400 GiB inside the table one to three weeks sooner.
  - The steady state shrinks by (30 − 7) days × 1.5–2.2M gone clients a day × about 3.5 KB:
    **about 110–170 GiB** *(inferred)*.
  - The table stops growing until new registrations have used the freed space: about a month at
    13 GiB/day.
- **What changes:**
  - a migration moves the floor function to 7 days, as migration 904 did for 30, with its
    rollback;
  - the clamp in model/st_client_key_history_retention.go:84–108 moves from 30–91 to 7–91, with
    its tests;
  - `horizonDays = 7` on the brake;
  - the per-table autovacuum settings of DBDISK I7 go on before the bulk deletes;
  - the brake ramps 300 → 1,000 a second, outside the reaper's backlog and the backup (§11). At
    1,000 a second the bulk takes about 1.3 days.
- **Alternative:** 14 days, for about half of the gain.

### 8.5 The disk file, and the redundant unique index

- **Deletes free space in place, and the file does not shrink.** Returning stranded space to the
  filesystem needs a rewrite. After PostgreSQL 19, REPACK (CONCURRENTLY) could do it once the live
  share is low. That needs free space at least equal to the live size, and a drain around its
  swap. A later decision.
- **Drop `UNIQUE (registration_hash)`: −20.6 GiB at once,** plus less work per insert and per
  vacuum.
  - The hash is SHA-256 of the registration bytes. Those bytes encode the client, network,
    generation, domain and previous hash, and the writer verifies them against the row
    (model/st_client_key_history.go:79–91).
  - So identical bytes already collide on the primary key, and distinct bytes with one hash would
    be a SHA-256 collision.
  - The index has never been scanned, and no monitor contract pins it.
  - `ALTER TABLE ... DROP CONSTRAINT` takes ACCESS EXCLUSIVE for the catalog change and the
    unlink, with a short lock wait and retries.
  - DBDISK-STABILIZE §3.3 called it an integrity guard; the owner decides.
  - **Owner go 11-16; checked on Main 11-16 05:00Z (catalog only):** the index is owned by the
    UNIQUE constraint of the same name (`contype u`; the index depends on it, `deptype i`), so
    `DROP INDEX CONCURRENTLY` is refused and the drop is `ALTER TABLE ... DROP CONSTRAINT`. No
    foreign key uses it (the head's key references the primary key), no code names it or uses it
    as an `ON CONFLICT` target, and `idx_scan` is 0 with no `last_idx_scan` since the statistics
    began on 10-23 20:08:05Z (the primary key: 2.25B scans since then, so the 11-16 02:02Z restart
    kept the counters). `GetClientContractMetadata` reads the table on every CreateContract (about
    1,180 a second), so the drop runs with `lock_timeout` 1 s and retries, never 0, in a vacuum gap
    under the validator gate; `db_client_key_history_unique_drop.go` records it afterwards as a
    catalog read.

## 9. The rest

### 9.1 `network_client_location_reliability`: delete with the client

- **What a row is.** One row per client (`PRIMARY KEY (client_id)`) folding its connections'
  locations and scores. The `update_reliabilities` upsert writes it
  (model/network_client_reliability_model.go:3085–3231).
- **Retention.** Rows go 31 days after the client was last seen: `ClientLocationExpiration`, 30
  days (:90), plus the daily refresh lag
  (model/network_client_location_reliability_retention.go:95–126).
- **No reader needs that long.** The longest use of a disconnected row is the payout weights,
  within client_reliability's 10 days (model/reliability_window_weights.go:76).
- **Rows of deleted clients linger.** The shared reap cascade
  (`removeReapedNetworkClientDependents`, model/network_client_model.go:4826–4895) does not delete
  them; only the prober reap does (model/prober_shard_model.go:613–616). A hosted window is
  hard-deleted 1 day after retirement, so its row stays about 30 more days.
- **Hosted children dominate.** The pass saw 5.2M hosted-child clients against 0.64M top-level
  clients in the last 24 hours (`urnetwork_client_location_reliability_pass_clients`).
- **Not at steady state.** It takes 291 inserts against 96 deletes a second. At today's inflow the
  31-day steady state is about 300 GiB *(inferred)*.
- **Not a partition candidate:**
  - providers live in every `client_id` range;
  - a last-seen key would cost the `client_id` primary key and move rows on every refresh;
  - no hot reader carries a time.
- **Fix (owner decision):**
  1. Delete the row in the shared cascade, with the same statement as the prober reap. The pass
     cannot re-create it, because both its reads join `network_client`.
  2. Lower `ClientLocationExpiration` to about 11 days. Check provider status, which would forget
     offline providers sooner.
  3. Let the 15-minute retention drain the backlog over a few days. Measure first with the sample
     at PARTITION-MAINTENANCE-PLAN.md:905–910.
- **Effect** *(inferred)*: rows fall from 475M toward the live clients (tens of millions). That
  frees well over 100 GiB in place, and the table no longer heads for about 300 GiB.

### 9.2 `network_client_connection` and `network_client_location`: no

- **Rows never age out by range.** Provider connections stay open for days, so no
  `connection_id` or connect-time range empties.
- **Partitioning closed rows by disconnect time would break the hot path:**
  - it loses the primary key;
  - every connect and disconnect lookup (about 400–550 a second each) would probe every leaf;
  - each disconnect would become a cross-partition move under the work-session triggers;
  - the orphan retire's `ctid` array (model/network_client_handler_retirement.go:117–133) would
    break.
- **The location table** is deleted in the same statement as its connection
  (model/network_client_model.go:4499–4523) and has no time column.
- **A swap would return about 5 GiB** (they were 2.6 GB compact on 10-23), but the connection
  table's triggers make a copy unsafe without care.
- **Keep the hot vacuum.** If closed-row churn ever matters, C7's append-only history of closed
  connections, partitioned hourly, is the shape.

### 9.3 `provider_work_session_head` and `audit_device_state`: S4 deletes, approved

- **`provider_work_session_head`** is the per-client sequence counter the partitioned journal's
  identity proof rests on (db_provider_work_session.go:7–10; db_provider_work_session_partition.go:9–26).
  - It has no time column, and a head must live as long as its client.
  - S4 deletes the heads of hard-deleted clients. That needs a guard change
    (db_provider_work_session.go:95–104) and republished monitor contracts
    (monitor/migration_provider_work_session_partition.go:84–85, :187).
- **`audit_device_state`** holds one row per device, updated in place.
  - An offline row behaves exactly like a missing one (model/audit_provider_sweep_model.go:365–366),
    so S4 can delete it at removal.
- **Partitioning cannot express either retention.**

### 9.4 `network_client`, `provide_key`, `st_client_key_head`: no

- **`network_client`** is the identity directory, updated in place: 401 updates a second, none HOT.
  - Retention is per row by state (Part D reaps).
  - `network_id` reads and the reaps would probe every leaf.
  - A drop would bypass the delete trigger that records gone clients for key history
    (db_identity_retention.go:75–92).
  - Its growth is index regrowth after O2.
- **`provide_key`** holds a client's keys, deleted at deactivation (911 deletes a second). It has
  no time range.
- **`st_client_key_head`** references history by foreign key, flips `is_current` in place, and is
  deleted with history.

## 10. Owner decisions

| # | Decision | Recommendation |
|---|---|---|
| 1 | **finished_task:** run C3 as built, keeping its 8-day day partitions | **Yes.** Cutover day C is the next 00:00Z at least 12 h after the prepare, finalized by C − 2 h under the validator gate |
| 2 | **pending_task:** build the swap command and run it in a 2–4 minute taskworker stop | **Yes**, on 11-18 (§11) |
| 3 | **transfer_debit_journal:** the swap, or daily contract-day partitions | **The swap** (no drain). Partitions only if it re-bloats after I3 |
| 4a | **audit_device_event:** the raw-event window | **7 days, with a per-day rollup.** Without the rollup, 125 days. Not 90 days unless the reassert drops to 7 days |
| 4b | **audit_device_event:** build the per-day rollup for the stats export | **Yes.** It also keeps the hourly export from outgrowing its 10-minute limit |
| 4c | **audit_device_event:** whether hosted window children count as devices | **Product decision.** If not, the sweep skips them, and the inflow falls several-fold |
| 4d | **audit_device_event:** partition by `event_time` (daily for a 7-day window, weekly from 90 days) and drop `UNIQUE (event_id)` | **Yes** |
| 5a | **st_client_key_history:** retention after the client is gone | **7 days** (14 as the cautious alternative), instead of 30 |
| 5b | **st_client_key_history:** drop `UNIQUE (registration_hash)` | **Yes:** −20.6 GiB, redundant with the primary key |
| 5c | **st_client_key_history:** a rewrite (REPACK) after PostgreSQL 19 | **Decide then**, from the live share after the bulk deletes |
| 6 | **network_client_location_reliability:** delete with the client, and 11 days instead of 31 | **Yes**, after the sample of §9.1 |
| 7 | Confirm the tables that are not partitioned (§9.2–§9.4) | **Confirm** |

## 11. Order, effort and risks

**Constraints.**
- The reaper's backlog delete runs from about 11-17 00:00Z for about two days at 2,000 a second,
  with heavy WAL and vacuum on the contract tables.
- The Thursday 11-19 06:00Z full backup copy reads edge-2 for about 25 h. No heavy IO from 11-19
  05:00Z to about 11-20 07:00Z. Sunday and Thursday 06:00Z runs follow.
- No production steps from Saturday 22:00Z to Sunday 16:00Z (the payout block).
- **The validator gate applies to every production step:** the validator idle (finalized or
  applied), with at least 15 minutes to its boundary.

**Schedule:**

| When | Step | Effort | Risk and guards |
|---|---|---|---|
| Mon 11-16 | Decisions. **C3 prepare** with C = 11-17 00:00Z if run by about 11:00Z (the minimum lead is 12 h; else C = 11-18), **finalize** by C − 2 h | about 1 h of operations | low: the prepare scan takes 1–2 min under SHARE UPDATE EXCLUSIVE; the finalize is catalog-only, 2 s lock waits |
| 11-16..11-17 | Build the `table-swap` command, the two profiles and the tests | 1.5–2 days | none in production |
| Mon–Tue | **Drop `UNIQUE (registration_hash)`** if approved | 0.25 day | low: a short ACCESS EXCLUSIVE with lock_timeout and retry; not during the reaper's first hours |
| Wed 11-18, daytime | **pending_task swap** in a 2–4 minute taskworker stop, after the swap command ships | operations | low–medium: tasks pause, and the reaper's backlog with them; settlement gate; check that the containers are gone |
| from Fri 11-20, after the backup | **transfer_debit_journal swap**, online | minutes | low: 250 ms lock waits on settlement |
| 11-20..11-23 | **Key history at 7 days:** floor migration, clamp, I7 autovacuum settings, then the brake ramps to 1,000 a second | 0.5 day code; about 1.5 days of deletes | medium: WAL and vacuum on a 1 TB table. Not during the reaper's backlog or a backup |
| 11-17..11-24 | Build the **audit rollup**, the family and its cutover; build the **location reliability** fix | 4–6 days, and 0.5–1 day | none in production until rollout |
| Tue 11-24 | **finished_task legacy leaf drops** (C + 7), by the registry | automatic | a held failed Post defers it; watch §2.4q |
| from 11-24 | **Location reliability** fix rolls; its backlog drains over days | — | medium: delete WAL; pace it like the key history brake |
| Mon 11-30 (weekly C) or the next day (daily C) | **audit_device_event cutover**; drop `UNIQUE (event_id)` on the legacy table | operations | low: catalog-only finalize; the export moves to the rollup first |
| C + window | audit legacy leaf drops | automatic | — |

**Effort:** about 8–11 developer days in all. The largest pieces are the audit rollup and family,
and the swap command.

**Risks:**
- **Swaps:**
  - the MVCC hazard of §4.4 (handled by the stop for `pending_task`, and by READ COMMITTED for the
    journal);
  - catalog contracts that pin names (handled by restoring every name);
  - stats counters that reset with the new relid.
- **Re-bloat:** the queues re-grow under long pins. The I3 rebuild windows are the next ones:
  `pending_task` by up to about 1 GiB, the journal by 2–3 GiB.
- **C3:** the legacy leaf waits for its failed Posts. Any held row defers every day's drop until it
  is released; that is by design.
- **Retention changes** delete hundreds of millions of rows. Pace them with their brakes, watch WAL,
  debit lag and taskworker overdue (the 11-04 lessons), and keep them out of the backup windows.
- **audit_device_event:** without the rollup, raw retention cannot go below about 121 days. Until it
  ships, the export keeps reading every row hourly.
- **Disk:** none of this needs headroom beyond C3's temporary +13 GiB. All of it lowers growth or
  returns space. The DBDISK projection gains about 66 GiB from items 1–3 and 20.6 from the unique
  index, and the retention changes keep two of the fastest-growing tables flat for weeks.

## 12. Method and evidence

- **Load on Main:** about 25 read-only statements through
  `dcdb2db1/tmp/pg187/pgq.sh` (psql as postgres), each starting `SET statement_timeout='20s'`,
  each 1–4 s, none holding a transaction open. Catalog and statistics views only:
  - `pg_class`, `pg_stat_user_tables`, `pg_statio_user_tables` (including PostgreSQL 18's
    `total_vacuum_time` and `total_autovacuum_time`), `pg_stat_user_indexes`, `pg_index`,
    `pg_constraint`, `pg_trigger`, `pg_stats` and `pg_stat_database`;
  - sizes from `pg_total_relation_size`, `pg_relation_size` and `pg_indexes_size`.
  - No table was scanned and no sample was taken.
- **Growth:**
  - family snapshots in DBDISK's format at 11-16 03:47:49Z, 04:07:54Z and 04:15:09Z, compared
    with DBDISK's 11-15 21:35:40Z snapshot (6.7 h);
  - vacuum and buffer IO from `pg_stat*` samples at 04:00:44Z and 04:15:11Z;
  - Mimir has no per-table size series, only `pg_database_size_bytes`.
- **Mimir:** `pg_database_size_bytes`, `node_filesystem_avail_bytes` (4,988 GiB free at 04:05Z),
  `urnetwork_retention_deleted_rows_total`, `urnetwork_contract_horizon_*` (the reaper scanned
  1,860 and found 296 eligible contracts a second at 04:00Z), `urnetwork_contract_opened_total`
  (1,301 a second) and `urnetwork_client_location_reliability_pass_clients`.
- **Code:** server `origin/main` 3939d79b4, read with `git show` and `git grep`.
- **Context:** Postgres restarted at 02:02:14Z (18.7). `pg_stat_database.stats_reset` is null, so
  the cumulative counters have no known start. Rates here come only from differences between
  samples.
- **Files:** `temp/partcand-1116/` beside the urnetwork checkout: `snap.sh`, `vac.sh`,
  `compare.py`, and the `snap-*.tsv` and `vac-*.tsv` samples.
