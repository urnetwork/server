# PENDING-TASK-SWAP-RUNBOOK — the pending_task swap, Wed 11-18

Written 2026-11-16 by the implementation of PARTITION-CANDIDATES.md §4 (owner
go 11-16 about 04:47–04:52Z: copy and swap, run Wed 11-18). The figures about
Main come from catalog and statistics views read through `pg187/pgq.sh` with a
20 s statement timeout. Nothing in this runbook has run in production.

**The owner runs each step, or approves it.** Agents do not run the swap, its
rehearsal or the drop on Main: each is DDL on production.

`$T` below is `/Users/brien/.claude/jobs/dcdb2db1/tmp`, and `$P` is
`$T/pg187/pgq.sh`.

## 0. What and why

- `pending_task` is 39 GB (28 GB heap) for about 130k live rows (11-16
  07:14Z: 129,501 live, 409,973 dead between vacuums). It never shrinks: long
  snapshots let it reach its high-water mark, and vacuum does not truncate.
- The swap copies the live rows into a fresh twin and swaps the names in one
  transaction, during a taskworker stop of about 3 minutes. The old file is
  kept as `pending_task_old_<stamp>` for a day, then dropped by a separate
  step (§5), which returns about 39 GiB.
- The command is `bringyourctl model migrate table-swap --table=pending_task
  [--dry-run] [--rollback] [--drop-old]` (server `db_table_swap.go`,
  `db_table_swap_catalog.go`; the pending_task profile is
  `task/pending_task_swap.go`).

## 1. What the command does

1. **Preflight**, read-only:
   - It refuses a table it cannot copy faithfully: triggers, rules, policies,
     publications, foreign keys, inheritance, partitioning, identity columns,
     dependent views or functions, users of its row type, extended
     statistics, column grants, a tablespace, a replica identity, an invalid
     index, a deferrable or exclusion constraint, or no primary key.
     pending_task has none of these (checked on Main 11-16: no triggers,
     rules, policies, publications, statistics objects or dependents; owner
     `bringyour`; default grants; no TOAST options).
   - It refuses while an old table is kept, while a relation is named
     `pending_task_next`, and while any client transaction older than 2
     minutes is open in the database.
   - **The taskworker check:** it reads every leased row (lease ahead). It
     refuses a row whose task advisory lock a session holds, which means a
     live owner. It also refuses a row whose heartbeat is under 30 s old, so a
     worker ran moments ago. It reports the rest, the claims a stopped worker
     left: they are copied as they are and claimed again once their lease
     ends, at most 5 minutes (`TaskLeaseTimeout`).
2. **Rehearsal**, in a transaction that is rolled back. It holds only ACCESS
   SHARE on pending_task, which blocks nothing but DDL.
   - It builds the twin, copies every row with its count and checksums,
     builds the indexes, and compares the twin with the table column for
     column and index for index.
   - It reports how long the copy and the index builds took. **That is about
     how long the swap will hold its lock.**
   - It warms the live rows into shared buffers, and reads the table's catalog
     fingerprint.
3. **Swap**: one script sent in one message, BEGIN to COMMIT. No network
   round trip happens under the lock; this Mac is about 90 ms from edge-2.
   - It runs in read committed, with lock_timeout 500 ms and statement_timeout
     30 s.
   - It takes ACCESS EXCLUSIVE on pending_task.
   - Checks under the lock: the table's fingerprint must be the one the
     rehearsal read, no old table may be kept, and the taskworker check runs
     again.
   - It creates `pending_task_next` without indexes, with `LIKE`: columns in
     order, defaults, the three generated columns, NOT NULL and CHECK
     constraints with their names, storage, compression and comments. Then it
     sets the 8 reloptions, the owner, the grants and the comments.
   - It copies every row through the primary key, with sequential scans off.
     It checks the count and two 64-bit checksums over every column,
     generated ones included, and refuses more than 1,000,000 rows.
   - It builds the four indexes as `<name>_next`: the primary key, the global
     `UNIQUE (run_once_key)` and both poll indexes. It checks the twin's
     fingerprint against the table's.
   - It renames:
     - pending_task becomes `pending_task_old_<stamp>`, and each of its
       indexes `<name>_old_<stamp>`;
     - the twin becomes pending_task, and each of its indexes takes the
       published name.
     Every name the code, the hot vacuum, db audit and the monitor's catalog
     contracts use is unchanged.
   - It commits, then runs ANALYZE.
   - pending_task owns no sequence. The command moves owned sequences
     (tested on a synthetic table), but there are none here.
   - **Any failed check raises and rolls everything back. An interrupted run
     changes nothing.**
4. **db audit and the REINDEX rotation** leave `pending_task_old_<stamp>` and
   `pending_task_next` alone.

**Producers in the other services keep writing through the swap.**
- A main-pool transaction waits at most 2 s for a lock (`PgLockTimeout`), and
  a lock timeout is not retried. The swap waits at most 500 ms for its lock,
  then holds it for the rehearsed copy time. If the copy takes about 1.5 s or
  more, a task-scheduling statement that arrives at the start can fail once.
  §2 measures that time ahead.
- Read committed statements that waited on the lock see the copied rows: they
  plan and take their snapshot after the commit.
- A repeatable read producer whose snapshot predates the swap meets a copied
  row through `ON CONFLICT (run_once_key)`. It gets a serialization failure,
  and `server.Tx` reruns it (tested).
- The UPDATE-by-key path that a straddling snapshot could miss,
  `RaiseRunOnceMaxTimeInTx`, runs only inside taskworker tasks
  (taskworker/work/st_ledger_v2_usage_work.go:122, st_work.go:432), so the
  stop covers it.

## 2. Tue 11-17: the rehearsal, with the taskworker running (recommended)

This tells you the lock time a day ahead. It refuses, as it should while the
taskworker runs, but it still rehearses.

```
cd ~/urnetwork/server && git checkout main && git pull --ff-only
lsof -nP -iTCP:5432 -sTCP:LISTEN     # the Main tunnel's listener on 127.0.0.1
env -u PGOPTIONS WARP_ENV=main WARP_HOST=owner-local WARP_VERSION=local+$(git rev-parse --short HEAD) \
  WARP_CONFIG_HOME=/Users/brien/urnetwork/config WARP_VAULT_HOME=/Users/brien/urnetwork/vault \
  BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1 \
  go run ./bringyourctl model migrate table-swap --table=pending_task --dry-run
```

**Expect:**
- `pending_task: N leased row(s) ... M with a live owner`;
- `rehearsal, rolled back: ~130000 rows copied in X, indexes built in Y`;
- the script it would send;
- `dry run: nothing changed`;
- a non-zero exit with `the swap of pending_task refuses: a taskworker is
  running ...`.

**Decide on X + Y:**
- **Under about 1.2 s:** go as planned.
- **1.2–3 s:** some task schedules in the other services may fail once at the
  swap. Accept it, or tell the implementer first.
- **Over 10 s:** the swap refuses itself (`MaxRehearsalDuration`).

**What it costs:** DDL on a twin inside a rolled-back transaction, ACCESS
SHARE on pending_task for a few seconds, and about 0.1–0.2 GB of WAL. Keep it
off the 07:00Z database maintenance (±30 minutes). Also, no long bringyourctl
read may be running through the tunnel (main-tunnel-saturation).

## 3. Wed 11-18: the window

**When:** daytime, for example a start between 14:00 and 17:00Z. Each item
below must hold:

| Constraint | Check |
|---|---|
| Validator idle, at least 15 minutes to its boundary, no measurement running | `$T/pg187/gate.sh` prints `GATE OK` |
| No client transaction older than 2 minutes; no base backup, index build, VACUUM FULL or DDL running | the same gate |
| Database maintenance at 03:00Z and 07:00Z | keep 30 minutes clear |
| The contract reaper's backlog delete (from about 11-17 00:00Z, 2–3 days) | the stop pauses it for about 3 minutes, which is harmless. It adds IO, which the step 5 dry run measures at that moment |
| The edge-6 full copy (Thu 11-19 06:00Z, about 25 h) | Wednesday is clear |
| Both deploy locks | step 1 |
| No other long bringyourctl read through the tunnel | `ps aux \| grep bringyourctl` |

**Steps.** The env prefix `E` is the one from §2:

```
E="env -u PGOPTIONS WARP_ENV=main WARP_HOST=owner-local WARP_VERSION=local+$(git rev-parse --short HEAD) WARP_CONFIG_HOME=/Users/brien/urnetwork/config WARP_VAULT_HOME=/Users/brien/urnetwork/vault BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1"
```

1. **Locks.** Take them in this order; if a `mkdir` fails, someone holds that
   lock, so wait.
   ```
   mkdir $T/deploy.lock.d && echo "agent=owner what=pending_task swap since=$(date -u +%FT%TZ)" > $T/deploy.lock.d/owner
   mkdir $T/deploy-proxy.lock.d && echo "agent=owner what=pending_task swap since=$(date -u +%FT%TZ)" > $T/deploy-proxy.lock.d/owner
   ```
2. **Gate and baseline:** `$T/pg187/gate.sh` (must print `GATE OK`), then
   `$T/pg187/health.sh 1m`.
3. **Stop:** `$T/pg187/tw.sh stop`.
   - It runs the settlement gate with ST_QUIET_MIN=0 first.
   - It stops the units and `docker stop -t 60`s the containers on edge-0, 1,
     3 and 4. Each host must print `left: 0`.
   - Then run `$T/pg187/tw.sh status`: every unit inactive, no containers.
4. **Wait 30 s** after the stop returns. That is the heartbeat window: a
   claim heartbeat under 30 s old refuses the swap.
5. **Dry run:** `$E go run ./bringyourctl model migrate table-swap
   --table=pending_task --dry-run`.
   - Expect exit 0, `0 with a live owner, 0 heartbeat within 30s`, the
     rehearsal line and `dry run: nothing changed`.
   - It may report leased rows. Those are claims a stopped worker kept; they
     are copied with their leases.
6. **Swap:** `$E go run ./bringyourctl model migrate table-swap
   --table=pending_task`.
   - Expect `swapped pending_task in one transaction: N rows copied in X,
     indexes built in Y; ACCESS EXCLUSIVE held Z before the commit`, then
     `analyzed pending_task`, then `the old table is kept as
     pending_task_old_<stamp>`.
   - Keep the output.
7. **Verify** (catalog only):
   ```
   $P "SET statement_timeout='20s'; SELECT pg_size_pretty(pg_total_relation_size('public.pending_task')), (SELECT string_agg(indexrelid::regclass::text, ',' ORDER BY 1) FROM pg_index WHERE indrelid='public.pending_task'::regclass), (SELECT reloptions::text FROM pg_class WHERE oid='public.pending_task'::regclass), (SELECT string_agg(relname || ' ' || pg_size_pretty(pg_total_relation_size(oid)), ',') FROM pg_class WHERE relname ~ '^pending_task_old_[0-9]{14}\$')"
   ```
   Expect:
   - about 0.1–0.3 GB;
   - the four published index names: `pending_task_function_poll_order`,
     `pending_task_pkey`, `pending_task_poll_order`,
     `pending_task_run_once_key_key`;
   - the same eight reloptions as before (fillfactor=50 and the seven
     autovacuum settings);
   - one `pending_task_old_<stamp>` of about 39 GB.
8. **Start:** `$T/pg187/tw.sh start`, then `$T/pg187/tw.sh status` until all 8
   containers are Up.
9. **Health:** run `$T/pg187/health.sh 1m` a few times.
   - The taskworker's oldest overdue climbs during the stop and falls back
     within about 5–10 minutes.
   - Execution errors return to baseline.
   - api and connect show at most a blip at the swap's second.
10. **Release the locks** in reverse order:
    ```
    rm $T/deploy-proxy.lock.d/owner && rmdir $T/deploy-proxy.lock.d
    rm $T/deploy.lock.d/owner && rmdir $T/deploy.lock.d
    ```

**Timing:** the stop takes about 1 minute, the wait 30 s, the dry run about
20 s, the swap about 20 s and the start about 15 s. **Tasks pause for about
2.5–3 minutes;** due work catches up after the start.

**If something refuses or fails:**
- **The dry run (5) refuses.**
  - A live owner or fresh heartbeat: check `tw.sh status` for a container
    left running, stop it, wait 30 s, and repeat.
  - An old transaction: its pid and query are printed; wait it out.
  - To give up the window instead: `tw.sh start`. Nothing changed.
- **The swap (6) fails.** Nothing changed, because it is one transaction.
  Start the taskworker and keep the output.
- **Lock or vacuum busy.** The swap retries 60 times, 2 s apart. If it gives
  up, start the taskworker.
- **The table changed since the rehearsal** (a migration ran in between):
  run step 6 again.

## 4. Rollback (until the drop)

This is the swap in reverse into the kept old table. Its original relation
comes back, with every row written since the swap.

Do steps 1–4 of §3, then:
1. `$E go run ./bringyourctl model migrate table-swap --table=pending_task --rollback --dry-run`
2. `$E go run ./bringyourctl model migrate table-swap --table=pending_task --rollback`
3. Steps 8–10 of §3.

The rollback first empties the old table and drops its indexes under its own
lock: nothing reads it, and the empty file means no unlink at the swap's
commit. Then one script copies the live rows back, rebuilds the indexes and
swaps the names. The table that was swapped in is kept as
`pending_task_old_<stamp2>`, for §5.

## 5. Drop the old table (a separate step, owner's go)

- **When:** at least 24 h after the swap (the command refuses earlier), and
  outside the edge-6 full copy (Thu 11-19 05:00Z to about Fri 11-20 07:00Z).
  So **Fri 11-20, after 07:00Z**.
- **No taskworker stop.** It takes only the old table's lock, which nothing
  else reads.
- **Commands:**
  ```
  $E go run ./bringyourctl model migrate table-swap --table=pending_task --drop-old --dry-run
  $E go run ./bringyourctl model migrate table-swap --table=pending_task --drop-old
  ```
- **Expect:** `dropped pending_task_old_<stamp> (39.x GiB)`, about −39 GiB on
  edge-2 within seconds, and almost no WAL.
- It drops with RESTRICT, so it can never take a sequence the new table uses.

## 6. Afterwards

- **Counters:** the `pg_stat_user_tables` and `pg_statio_user_tables` counters
  of pending_task start again from zero, because it is a new relation.
  - Grafana rates treat that as a counter reset.
  - The monitor's per-table reads key by relation oid or by name with sizes:
    the retention liveness and autovacuum discard probes, and the table bloat
    signal (§2.4k), which credits the shrink against its last measurement.
- **Autovacuum:** the same thresholds (50 dead rows), but each pass reads a
  few MB, not up to 28 GB of heap.
- **Re-bloat:** only a long snapshot over heavy churn re-bloats it; a 2-hour
  pin adds about 0.5–1 GiB. Repeat the swap if the bloat signal fires again.
  The command refuses while an old table is kept, so drop it first.
