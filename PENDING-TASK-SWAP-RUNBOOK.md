# PENDING-TASK-SWAP-RUNBOOK — the pending_task swap

Written 2026-11-16 by the implementation of PARTITION-CANDIDATES.md §4 (owner
go 11-16 about 04:47–04:52Z: copy and swap). Revised 2026-11-17 after the
rehearsal on Main measured 4.6 s under the lock (§1, "Why the copy left the
lock"); the owner's go of 11-17 is "as soon as possible". The figures about
Main come from catalog and statistics views read through `pg187/pgq.sh` with a
20 s statement timeout. Nothing in this runbook has run in production but the
rehearsals.

**The owner runs each step, or approves it.** Agents do not run the swap, its
rehearsal or the drop on Main: each is DDL on production.

`$T` below is `/Users/brien/.claude/jobs/dcdb2db1/tmp`, and `$P` is
`$T/pg187/pgq.sh`. The operator scripts in `$T/ptswap1116` (pt-rehearse.sh,
pt-swap.sh, pt-rollback.sh, pt-drop-old.sh) run the steps below.

## 0. What and why

- `pending_task` is 39 GB (28 GB heap) for about 115k live rows (11-17: 113,749
  rows copied by the rehearsal; 118,095 in pg_class). It never shrinks: long
  snapshots let it reach its high-water mark, and vacuum does not truncate.
- The swap copies the live rows into a fresh twin and builds its indexes
  **before** its lock, with a change capture on, during a taskworker stop of
  about 3 minutes. Under the lock it applies only the changes made since and
  swaps the names in one short transaction. The old file is kept as
  `pending_task_old_<stamp>` for a day, then dropped by a separate step (§5),
  which returns about 39 GiB.
- The command is `bringyourctl model migrate table-swap --table=pending_task
  [--dry-run] [--rollback] [--drop-old] [--abort]` (server `db_table_swap.go`,
  `db_table_swap_capture.go`, `db_table_swap_catalog.go`; the pending_task
  profile is `task/pending_task_swap.go`).

## 1. What the command does

**Why the copy left the lock (11-17).** The first design copied the rows and
built the indexes under ACCESS EXCLUSIVE. On 11-17 at 08:47Z the rehearsal on
Main measured a copy of 2.921 s and index builds of 1.662 s (113,749 rows): a
4.6 s lock, past the go rule's 3 s, so the swap stopped. The copy and the
builds now run before the lock. The lock is held for the changes captured
since the copy, the checks and the renames: tens of milliseconds.

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
     `pending_task_next` that is not its own, while a change capture an
     interrupted run left is in place (`--abort` removes it, §3a), while a
     run of the swap is in progress, and while any client transaction older
     than 2 minutes is open in the database.
   - **The taskworker check:** it reads every leased row (lease ahead). It
     refuses a row whose task advisory lock a session holds, which means a
     live owner. It also refuses a row whose heartbeat is under 30 s old, so a
     worker ran moments ago. It reports the rest, the claims a stopped worker
     left: they are copied as they are and claimed again once their lease
     ends, at most 5 minutes (`TaskLeaseTimeout`).
2. **Rehearsal**, in a repeatable read transaction that is rolled back. It
   holds only ACCESS SHARE on pending_task, which blocks nothing but DDL.
   - It builds the twin, copies every row with its count and checksums,
     builds the indexes, and compares the twin with the table column for
     column and index for index.
   - It creates the change capture's log and function as the swap does, and
     runs the verification the swap runs before its lock.
   - It runs the swap's **exclusive section** with the most changes the swap
     applies under its lock (1,000): the capture triggers' check and drop,
     the fingerprint and old-table checks, the captured changes applied, the
     taskworker check (on the twin, see 4), the twin's fingerprint and as
     many renames, each check reporting instead of raising.
   - **Column options:** the comparison and the fingerprint cover the options
     a user sets (`n_distinct`). PostgreSQL's internal ones are left out.
     Since 18.7 (Main from 11-16 02:02Z) every new column, the twin's
     included, gets `pg_attver`: its row-type version, the WAL position when
     it was created. A twin can never carry its table's version (the server
     drops a value that `ALTER COLUMN SET` names). pending_task, created
     before 18.7, has none.
   - It reports the copy and the index builds, now before the lock, and the
     exclusive section: **that is about how long the swap will hold its
     lock**, with the largest delta it allows. The swap refuses an exclusive
     section over 1 s (`MaxLockHold`).
   - It warms the live rows into shared buffers, and reads the table's catalog
     fingerprint.
3. **Pre-copy**, before the lock; writers keep writing:
   - It creates `pending_task_next` without indexes, with `LIKE`: columns in
     order, defaults, the three generated columns, NOT NULL and CHECK
     constraints with their names, storage, compression and comments. Then it
     sets the 8 reloptions, the owner, the grants and the comments.
   - In the same transaction, the **change capture**: a log table
     `pending_task_next_log` (the task ids of changed rows; autovacuum off; its
     comment names the twin's oid) and a function
     `pending_task_next_capture()`.
   - Then the capture triggers on pending_task: `table_swap_capture` (AFTER
     INSERT OR UPDATE OR DELETE, each row's task id into the log, in the
     writer's own transaction) and `table_swap_capture_truncate` (refuses a
     TRUNCATE), both ENABLE ALWAYS. Creating them takes **SHARE ROW EXCLUSIVE
     for about a millisecond**, admitted like the swap's lock (below) with a
     500 ms wait: it waits for every transaction that has written the table,
     so every later write is captured. Readers are not blocked.
   - It copies every row through the primary key, with sequential scans off,
     with the count and two 64-bit checksums over every column (generated
     ones included); refuses more than 1,000,000 rows; and builds the four
     indexes as `<name>_next`: the primary key, the global `UNIQUE
     (run_once_key)` and both poll indexes; then checks the twin's
     fingerprint against the rehearsal's. One transaction, whose snapshot is
     taken after the triggers committed.
   - `VACUUM (ANALYZE, SKIP_LOCKED)` of the twin: statistics from its first
     moment as pending_task.
   - Catch-up passes: each applies the captured task ids to the twin at one
     snapshot (repeatable read): it deletes the twin's rows of those ids and
     copies pending_task's current rows of them, checked by count and
     checksums. It runs until a pass applies at most 100.
   - **Verification**: at one snapshot, the twin holds exactly pending_task's
     row of every task id not captured since (count and checksums over every
     column). A missed write refuses the swap here.
4. **Swap**: one script sent in one message, BEGIN to COMMIT, after a last
   catch-up. No network round trip happens under the lock; this Mac is about
   90 ms from edge-2.
   - Read committed, lock_timeout 500 ms, statement_timeout 30 s.
   - It takes ACCESS EXCLUSIVE on the twin first (nothing else uses it), then
     on pending_task. `transaction_timeout` cuts the transaction off **1.5 s
     after its request for pending_task's lock** (500 ms wait + 1 s held), so
     no writer ever waits past 1.5 s of its 2 s bound.
   - Under the lock: the capture triggers must still be whole (both present,
     enabled always) and are dropped; the table's fingerprint must be the one
     the rehearsal read; no old table may be kept; the changes captured since
     the last catch-up are applied (more than 1,000 refuses at once, releasing
     the lock, and the swap catches up and tries again); the **taskworker
     check runs again, on the twin**, which now holds exactly pending_task's
     rows (on 11-17 every `available_block` histogram bound on Main was ahead
     of now, so the check reads nearly every row: through the bloated heap
     that alone costs about a copy, through the twin a few milliseconds); the
     twin's fingerprint must match the table's.
   - It renames:
     - pending_task becomes `pending_task_old_<stamp>`, and each of its
       indexes `<name>_old_<stamp>`;
     - the twin becomes pending_task, and each of its indexes takes the
       published name.
     Every name the code, the hot vacuum, db audit and the monitor's catalog
     contracts use is unchanged.
   - It commits, then runs ANALYZE, then drops the capture's log and
     function.
   - pending_task owns no sequence. The command moves owned sequences
     (tested on a synthetic table), but there are none here.
   - **Any failed check raises and rolls the lock's transaction back; the run
     then removes its change capture and its twin.** A run killed outright
     leaves them, and the next run refuses until `--abort` (§3a).
5. **db audit and the REINDEX rotation** leave `pending_task_old_<stamp>`,
   `pending_task_next` and `pending_task_next_log` alone.

**Producers in the other services keep writing through the swap.** Every
write path that runs while the taskworker is stopped was listed on 11-17:
schedules (`INSERT … ON CONFLICT (run_once_key) DO UPDATE / DO NOTHING`) from
api and connect, the contract settle's delete of queued scheduled closes and
the idle wake's `UPDATE run_at` (both read committed), and the operator
commands (`task rm/release/kick`, `db scrub-client-addresses`, contract close).
None updates `task_id` or `run_once_key`, none truncates, none sets
`session_replication_role`. The capture's row trigger records each of them.
- A main-pool transaction waits at most 2 s for a lock (`PgLockTimeout`), and
  a lock timeout is not retried. The swap waits at most 500 ms for its lock
  and holds it for the exclusive section, cut off at 1.5 s in all.
- Read committed statements that waited on the lock see the twin's rows: they
  plan and take their snapshot after the commit.
- A repeatable read producer whose snapshot predates the swap and meets a row
  its snapshot cannot see through `ON CONFLICT (run_once_key)` gets a
  serialization failure, and `server.Tx` reruns it (tested). One exception:
  `RequestPayerDueContractClose` runs repeatable read without rerun; its
  failed request is logged as dropped, its Redis gate reopens and a later
  request retries (model/subscription_payer_due_close.go:407-424).
- The UPDATE-by-key path that a straddling snapshot could miss,
  `RaiseRunOnceMaxTimeInTx`, runs only inside taskworker tasks
  (taskworker/work/st_ledger_v2_usage_work.go:122, st_work.go:432), so the
  stop covers it.

## 2. The rehearsal, with the taskworker running (recommended)

This tells you the lock time ahead. It refuses, as it should while the
taskworker runs, but it still rehearses. `$T/ptswap1116/pt-rehearse.sh` runs
it and writes `rehearsal.latest`.

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
- `rehearsal, rolled back: ~114000 rows copied in X and indexes built in Y
  before the lock; the swap holds ACCESS EXCLUSIVE on pending_task about Z
  (its checks and renames …, and … to apply 1000 captured change(s), the
  most it applies under the lock)`;
- `rehearsal: the checks under the lock would refuse now: a taskworker is
  running …` (expected while it runs);
- the pre-copy and the script it would send;
- `dry run: nothing changed`;
- a non-zero exit with `the swap of pending_task refuses: a taskworker is
  running ...`.

**Decide on Z** (pt-rehearse.sh prints it as the exclusive section, and its
go rule reads it where it read X + Y before):
- **Under 1.2 s:** go as planned. Off Main, on 18.7 with a bloated
  pending_task (3.2 GB heap, 114k live rows spread through it; its copy and
  builds took 2.95 s and 1.01 s against Main's 2.92 s and 1.66 s), Z was
  127 ms and the real swap held its lock 61 ms.
- **Over 1 s** the swap refuses itself (`MaxLockHold`), so the scripts'
  1.2–3 s ACCEPT_SLOW range no longer leads to a swap: tell the implementer.

X and Y (about 3 s and 2 s on Main) now run before the lock: they lengthen the
taskworker stop, not the lock.

**What it costs:** DDL on a twin inside a rolled-back transaction, ACCESS
SHARE on pending_task for a few seconds, and about 0.1–0.2 GB of WAL. Keep it
off the 07:00Z database maintenance (±30 minutes). Also, no long bringyourctl
read may be running through the tunnel (main-tunnel-saturation).

## 3. The window

**When:** daytime, for example a start between 14:00 and 17:00Z. Each item
below must hold:

| Constraint | Check |
|---|---|
| Validator idle, at least 15 minutes to its boundary, no measurement running | `$T/pg187/gate.sh` prints `GATE OK` |
| No client transaction older than 2 minutes; no base backup, index build, VACUUM FULL or DDL running | the same gate |
| Database maintenance at 03:00Z and 07:00Z | keep 30 minutes clear |
| The contract reaper's backlog delete (from about 11-17 00:00Z, 2–3 days) | the stop pauses it for about 3 minutes, which is harmless. It adds IO, which the step 5 dry run measures at that moment |
| The edge-6 full copy (Thu 11-19 06:00Z, about 25 h) | avoid it |
| Both deploy locks | step 1 |
| No other long bringyourctl read through the tunnel | `ps aux \| grep bringyourctl` |

`$T/ptswap1116/pt-swap.sh` runs steps 1–10. By hand, the env prefix `E` is
the one from §2:

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
     rehearsal line and `dry run: nothing changed`; Z under the go rule.
   - It may report leased rows. Those are claims a stopped worker kept; they
     are copied with their leases.
6. **Swap:** `$E go run ./bringyourctl model migrate table-swap
   --table=pending_task`.
   - Expect, in order: `the change capture is on pending_task (log
     pending_task_next_log): its triggers held SHARE ROW EXCLUSIVE for …`,
     `pre-copied N rows of pending_task into pending_task_next in X and built
     its indexes in Y, outside the lock`, `vacuumed and analyzed
     pending_task_next`, `caught up … captured change(s)`, `verified
     pending_task_next against pending_task: N rows equal, …`, then
     `swapped pending_task in one transaction: …; ACCESS EXCLUSIVE held Z
     before the commit`, `analyzed pending_task` and `the old table is kept
     as pending_task_old_<stamp>`.
   - Keep the output.
7. **Verify** (catalog only):
   ```
   $P "SET statement_timeout='20s'; SELECT pg_size_pretty(pg_total_relation_size('public.pending_task')), (SELECT string_agg(indexrelid::regclass::text, ',' ORDER BY 1) FROM pg_index WHERE indrelid='public.pending_task'::regclass), (SELECT reloptions::text FROM pg_class WHERE oid='public.pending_task'::regclass), (SELECT string_agg(relname || ' ' || pg_size_pretty(pg_total_relation_size(oid)), ',') FROM pg_class WHERE relname ~ '^pending_task_old_[0-9]{14}\$'), (SELECT count(*) FROM pg_trigger WHERE tgrelid='public.pending_task'::regclass AND NOT tgisinternal), to_regclass('public.pending_task_next_log')"
   ```
   Expect:
   - about 0.1–0.3 GB;
   - the four published index names: `pending_task_function_poll_order`,
     `pending_task_pkey`, `pending_task_poll_order`,
     `pending_task_run_once_key_key`;
   - the same eight reloptions as before (fillfactor=50 and the seven
     autovacuum settings);
   - one `pending_task_old_<stamp>` of about 39 GB;
   - no trigger on pending_task, and no `pending_task_next_log`.
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
20 s, the swap about 30 s (the rehearsal again, the pre-copy of about 5 s, the
catch-up and the verification of a few seconds, the lock) and the start about
15 s. **Tasks pause for about 3 minutes;** due work catches up after the
start.

**If something refuses or fails:**
- **The dry run (5) refuses.**
  - A live owner or fresh heartbeat: check `tw.sh status` for a container
    left running, stop it, wait 30 s, and repeat.
  - An old transaction: its pid and query are printed; wait it out.
  - `left its change capture`: an earlier run was killed; §3a, then repeat.
  - To give up the window instead: `tw.sh start`. Nothing changed.
- **The swap (6) fails.** The lock's transaction rolls back, and the run
  removes its change capture and twin (`removed the change capture of
  pending_task and its twin`). Start the taskworker and keep the output.
- **Lock or vacuum busy.** Each lock (the capture's and the swap's) waits for
  admission (no autovacuum, prepared transaction or transaction older than
  5 s on the table; no client transaction older than 2 minutes) and retries
  60 times, 2 s apart. If it gives up, start the taskworker.
- **More than 1,000 changes since the last catch-up** (`lock busy … more than
  the swap applies under its lock`): the lock is released at once; the next
  attempt catches up first.
- **The table changed since the rehearsal** (a migration ran in between):
  run step 6 again.

### 3a. A run killed outright (`--abort`)

A run that dies without its cleanup (the process killed, the tunnel lost)
leaves the capture triggers on pending_task, the log, the function and the
twin. Writes go on and are logged; a TRUNCATE is refused. The swap and the
rollback refuse until it is removed:

```
$E go run ./bringyourctl model migrate table-swap --table=pending_task --abort --dry-run
$E go run ./bringyourctl model migrate table-swap --table=pending_task --abort
```

It refuses while a run holds the swap's run lock (a live run removes its own).
It drops the triggers under a brief ACCESS EXCLUSIVE (admitted, 500 ms wait),
then the function and the log, and the twin the log names. It never touches
pending_task's rows. If the log says the swap committed, only the log and the
function go.

## 4. Rollback (until the drop)

This is the swap in reverse into the kept old table. Its original relation
comes back, with every row written since the swap.

Do steps 1–4 of §3, then:
1. `$E go run ./bringyourctl model migrate table-swap --table=pending_task --rollback --dry-run`
2. `$E go run ./bringyourctl model migrate table-swap --table=pending_task --rollback`
3. Steps 8–10 of §3.

(`$T/ptswap1116/pt-rollback.sh` runs all of it, with the same go rule on the
rehearsed exclusive section.)

The rollback first empties the old table and drops its indexes under its own
lock: nothing reads it, and the empty file means no unlink at the swap's
commit. Then it pre-copies pending_task into it with the change capture on,
rebuilds the indexes, catches up and verifies, and swaps the names under the
lock as the swap does. The table that was swapped in is kept as
`pending_task_old_<stamp2>`, for §5.

## 5. Drop the old table (a separate step, owner's go)

- **When:** at least 24 h after the swap (the command refuses earlier), and
  outside the edge-6 full copy (Thu 05:00Z to about Fri 07:00Z).
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
