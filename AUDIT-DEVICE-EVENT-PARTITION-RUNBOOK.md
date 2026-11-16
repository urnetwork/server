# AUDIT-DEVICE-EVENT-PARTITION-RUNBOOK — the device day rollup and the daily partitions

Written 2026-11-16 by the implementation of PARTITION-CANDIDATES.md §7. The
owner decided on 11-16, about 04:47–04:52Z:
- daily partitions by `event_time`, keeping 7 days of raw events;
- a per-day summary that the stats export reads instead;
- dropping `UNIQUE (event_id)`.

The figures about Main come from catalog and statistics views. Nothing here
has run in production. **The owner runs each step, or approves it.**

`$T` is `/Users/brien/.claude/jobs/dcdb2db1/tmp`, `$P` is `$T/pg187/pgq.sh`,
and `E` is the env prefix of PENDING-TASK-SWAP-RUNBOOK.md §3 (`env -u
PGOPTIONS WARP_ENV=main ... BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1`).

## 0. What and why

- `audit_device_event` holds 75 GB:
  - heap 27 GB;
  - indexes: the device index 22.6 GB, the primary key 17.1 GB and
    `UNIQUE (event_id)` 7.8 GB;
  - 269M events since 08-10.
- **Growth:** about 5.4 GiB a day. Rows go only after 180 days, so it heads
  for about 1 TiB.
- **Its only reader** is the hourly stats export, which replays every retained
  row for the public devices series (`stats.last-90`).
- **The rollup** `audit_device_day` keeps one row per UTC day with the day's
  device count:
  - the device sweep keeps today's row in its own transaction;
  - the export reads at most 91 small rows instead of 269M events.
  So the raw events need only days.
- **Then the table becomes daily partitions** kept 7 days. The registry drops
  each day whole: no row deletes, no WAL for retention, and no REINDEX of the
  table. The steady state is about 40 GB, about −35 GB against today and about
  −900 GB against the 180-day path.

## 1. Phase A: the rollup ships with a normal release (no owner step beyond the release)

1. **Migration**: `audit_device_day`, one new empty table, created only when
   absent, under a 5 s lock bound. It locks nothing existing.
   - Its position is at the end of the list; the merge report gives the
     number.
   - Apply it with `db migrate` before the taskworker that uses it.
   - A taskworker deployed first still sweeps and exports as before: both
     check for the table.
2. **The taskworker build**, from the merge on, does two things:
   - **The device sweep** keeps today's row. The first sweep of a day inserts
     the count of devices online at the day's start. Each later sweep adds the
     devices it adds that were neither online at the start nor touched earlier
     that day.
   - **The hourly export** reads the rollup when every closed day of its
     90-day window has a complete row, and today has its row or none yet.
     Otherwise it replays the raw events as before, and writes each closed day
     it lacked as a complete row, except the window's first day, which a
     replay counts inexactly. That day was an interior day of the day
     before's window.
   - So the **exports of the deploy day** cost what every export costs today,
     and backfill the window. Today's row is incomplete (events were written
     before the new sweep), so those exports replay. The first export of the
     next day writes the deploy day, now closed; from then on the export reads
     only the rollup.
3. **Verify** after a day. These are reads of a 91-row table: run them
   yourself, or ask, since agents keep to catalog views on Main.
   ```
   $P "SET statement_timeout='20s'; SELECT count(*), min(day), max(day), count(*) FILTER (WHERE NOT complete) FROM audit_device_day"
   ```
   - Expect about 91 rows with no incomplete day older than today.
   - The cutover's prepare also checks this, and refuses while any of the last
     90 days lacks a complete row.
   - The published series (`GET /stats/last-90`, `devices_data`) must keep
     its values. The rollup is tested equal to the replay day by day.

## 2. Phase B: the cutover (owner-run, `bringyourctl model migrate audit-device-event-partition`)

**When.** Pick the cutover day C for a quiet window that keeps clear of:
- the validator gate (`$T/pg187/gate.sh`, `GATE OK`);
- both deploy locks;
- database maintenance at 03:00Z and 07:00Z, ±30 minutes;
- the reaper backlog, from about 11-17 00:00Z for 2–3 days;
- the edge-6 full copy: no DDL from 11-19 05:00Z to about 11-20 07:00Z, then
  Sundays and Thursdays at 06:00Z;
- the payout block, Sat 22:00Z to Sun 16:00Z.

**Proposal:**
- prepare and finalize Fri 11-20 after 08:00Z, with C = Mon 11-23 00:00Z;
- the legacy partition then drops at C + 7 days, Mon 11-30, as the design
  scheduled;
- any day works once the rollup is complete. C must be at least 12 h after
  the prepare.

### B1. Prepare: a dry run first, then the real run

```
$E go run ./bringyourctl model migrate audit-device-event-partition --cutover-day=2026-11-23 --dry-run
$E go run ./bringyourctl model migrate audit-device-event-partition --cutover-day=2026-11-23
```

It runs these steps, each resumable. A rerun skips what is done.
1. **It refuses** unless:
   - the rollup has a complete row for each of the last 90 days;
   - C is at least 12 h ahead;
   - the table is a plain table with the published primary key and no
     trigger or foreign key.
2. **It drops `audit_device_event_event_id_key`** (−7.8 GiB):
   - a short ACCESS EXCLUSIVE lock;
   - lock waits of 2 s, retried 60 times, each after waiting out any
     transaction older than 5 s that holds the table;
   - no statement uses the index: event ids are `server.NewId()`, and no
     `ON CONFLICT` targets it;
   - **abort and rollback do not restore it.**
3. **It stages `audit_device_event_staged`**, with the primary key and the
   device index under staged names, and the days C..C+3. All empty, under
   SHARE UPDATE EXCLUSIVE.
4. **It adds `CHECK (event_time < C) NOT VALID`** (a brief exclusive lock),
   then validates it:
   - one scan of the 27 GB heap under SHARE UPDATE EXCLUSIVE, on a session
     without `transaction_timeout`;
   - the sweep's inserts continue;
   - expect a few minutes of read IO, and a snapshot held that long. Keep it
     away from the queue-heavy windows.

### B2. Finalize, before C − 2 h

```
$E go run ./bringyourctl model migrate audit-device-event-partition --finalize --dry-run
$E go run ./bringyourctl model migrate audit-device-event-partition --finalize
```

1. **Probe:** in a rolled-back transaction, it proves that every parent index
   adopts a legacy index.
2. **Swap**, one catalog-only transaction with 2 s lock waits:
   - the table and its indexes take the legacy names
     (`audit_device_event_legacy`, `..._l`);
   - the table attaches FROM (MINVALUE) TO (C), and the validated check skips
     the scan;
   - the staged parent becomes `audit_device_event`, and its indexes take the
     published names.
3. **ANALYZE ONLY** of the parent.

A prepare not finalized by C − 2 h is abandoned by the partition runway pass,
every 10 minutes, because its check would refuse every event from C on.
Finalize refuses from then on; prepare a later day.

### B3. After C

- Events from C on go to `audit_device_event_pYYYYMMDD`. The registry:
  - keeps 3 days ahead (runway every 10 minutes);
  - analyzes the parent once a day and each closed day an hour after it ends;
  - drops a day once its upper bound is 7 days past.
- **The legacy partition** (everything before C) drops at C + 7 days in the
  same way. It holds about 100 GB for C = Mon 11-23: 75 GB on 11-16, less the
  7.8 GB unique index, plus about 5 GiB a day until C.
  - Its drop transaction holds the parent's ACCESS EXCLUSIVE lock for the
    catalog work only. PostgreSQL unlinks a dropped relation's files and
    drops its buffers after the commit has released the locks
    (`CommitTransaction` calls `smgrDoPendingDeletes` after the lock release,
    because it "may take many seconds").
  - A sweep that meets the lock waits up to its 2 s lock bound, or retries on
    its next 15-minute run.
  - Expect an IO burst while about 100 GB of files are unlinked.
- **The hourly row delete stops** (`RemoveOldAuditDeviceEvents` deletes
  nothing from a partitioned table). The REINDEX rotation skips the parent and
  its days. The export reads only the rollup.
- **Monitor:**
  - §2.4p `partition-runway` and §2.4q `partition-retention-blocked` cover the
    new family;
  - contract 830 (the device index) accepts the partitioned parent's ON ONLY
    definition;
  - the rollup has its own catalog contract at its migration's version.

### B4. Undo

- **Before finalize:** `--abort [--dry-run]` drops the staged parent and the
  check. The unique index stays dropped.
- **After finalize:** `--rollback [--dry-run]` copies the daily partitions'
  rows back into the legacy table under the parent's lock, then swaps back.
  - It refuses more than 1,000,000 rows (`--max-copy-rows`). About 18M events
    land a day, so a rollback belongs in the first hour or so after C, or not
    at all: fix forward.
  - Before C there is nothing to copy.

## 3. Follow-up for the owner

- **A record migration for the unique index.** The migrations still define
  `UNIQUE (event_id)`, and db audit accepts its absence. Once B1 has dropped
  it on Main, merge a migration `ALTER TABLE audit_device_event DROP
  CONSTRAINT IF EXISTS audit_device_event_event_id_key`. It is a no-op on Main
  and keeps fresh databases equal. It must not merge before the drop: inside
  `db migrate` it would take the exclusive lock itself.
- **Hosted window children are not devices** (4c, owner 11-16 about 15:15Z).
  From the taskworker build that carries it, the device sweep counts top-level
  clients only (`source_client_id IS NULL`), the definition the client limits
  use. A derived client is a hosted proxy's window client or an app's
  per-stream window.
  - The first sweep after the deploy removes, once, every derived client still
    marked online.
  - The public series counts top-level clients from the next UTC day on.
    Earlier days keep the counts they had.
  - The raw-event inflow falls about tenfold *(inferred)*, so the daily
    partitions after C are much smaller than §0 estimates. The legacy leaf
    holds the events written before the deploy.
