# DEBIT-JOURNAL-PARTITION-RUNBOOK — transfer_debit_journal to daily partitions

Written 2026-11-16 for owner decision 3 of PARTITION-CANDIDATES.md §10 (11-16
about 04:47–04:52Z): daily partitions by contract day, chosen over the swap.
The figures about Main come from catalog and statistics views read through
`pg187/pgq.sh` with a 20 s statement timeout. Nothing in this runbook has run
in production.

**The owner runs each step on Main, or approves it.** `$T` is
`/Users/brien/.claude/jobs/dcdb2db1/tmp`, `$P` is `$T/pg187/pgq.sh`, and `$E`
is the environment prefix of `db-migrate-from-mac`:

```
E="env -u PGOPTIONS WARP_ENV=main WARP_HOST=owner-local WARP_VERSION=local+$(git rev-parse --short HEAD) WARP_CONFIG_HOME=/Users/brien/urnetwork/config WARP_VAULT_HOME=/Users/brien/urnetwork/vault BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1"
```

## 0. What and why

- **Today:** about 7 GiB for 5–10k live rows (36–57k by DBDISK's count;
  PARTITION-CANDIDATES.md §6.1). Every live row is a debit
  waiting seconds for the settlement worker. Churn is about 1,318 inserts,
  updates and deletes a second, and the heap stays at the high-water mark that
  long snapshots leave behind.
- **After the cutover:**
  - every contract day gets its own leaf, `transfer_debit_journal_pYYYYMMDD`;
  - contracts live 60 minutes, so a day takes its last normal debit about 75
    minutes after it ends;
  - the registry drops a day 2 days after it ends, once it holds no row at all
    (not even an applied row awaiting its Redis release);
  - the bloat of a day leaves with its file.
- **The other leaves:**
  - `transfer_debit_journal_legacy` is today's table. It drains within about
    75 minutes and drops 2 days after the cutover day, which returns its
    7 GiB.
  - `transfer_debit_journal_default` takes late debits of contracts whose day
    is gone, and ids from clocks far off. It is never dropped, and no debit is
    ever refused.
- **Code:**
  - server `db_transfer_debit_journal_partition.go` (the family, the create and
    drop hooks, and the drop guard's migration);
  - `db_transfer_debit_journal_cutover.go` (the command);
  - `bringyourctl model migrate debit-journal-partition`.

## 1. What changes for the settlement path

- **Unchanged statements:** every statement keeps its text. They name the
  parent, are read committed or autocommit, and need no change. The primary
  key `(balance_id, contract_id)` holds the partition key.
- **Leaf probes:** shard and balance reads probe every leaf. That is about 7
  leaves: the legacy leaf until it drops, about 5 days, and DEFAULT.
- **The daily rotation** runs in the taskworker on Main, next to the database,
  so no network round trip happens under its locks.
  - **Runway, every 10 minutes:** it keeps 3 days ahead, so a new day is
    attached once a day. The attach takes SHARE UPDATE EXCLUSIVE on the parent
    and ACCESS EXCLUSIVE on DEFAULT, and scans DEFAULT, which holds only
    stragglers.
  - **Retention, every 15 minutes:** it drops one expired day a day. The drop
    takes ACCESS EXCLUSIVE on the parent, DEFAULT and the leaf; with a DEFAULT
    leaf, DETACH CONCURRENTLY is not allowed.
  - **Before either asks for a lock,** it waits out any transaction older than
    5 s that holds a journal lock. It then waits at most 100 ms for its locks,
    and a busy pass is retried on the next one. A settlement statement queued
    behind the request waits that plus the hold: within the flush's own
    250 ms.
- **Measured** by the database's clock on the disposable test database on
  the loaded Mac (production catalog work should be faster):

  | Step | Lock held |
  |---|---|
  | Attach of a new day (ACCESS EXCLUSIVE on DEFAULT), including the DEFAULT scan | 24–38 ms |
  | A settlement read that waited on a drop's locks, after the fence: guard, detach, recheck, drop, commit | about 29 ms |
  | Whole create of a day, end to end | about 80 ms |
  | Whole drop of an empty day, from the quiet check to the commit | about 61 ms |

  - Concurrent settlement inserts, applies and deletes lost no debit and
    applied none twice, with zero lock-bounded retries.
  - A drop's file removal and its scan of the 256 GiB buffer pool run at
    commit after the locks are released (PostgreSQL 18 runs
    smgrDoPendingDeletes after releasing locks), so even the legacy leaf's
    7 GiB drop holds the parent only for the catalog work.

## 2. Order of work

1. **Merge** the branch to server main: the code, the migration and the
   monitor contract.
2. **db migrate** from local main. It adds one function,
   `transfer_debit_journal_drop_guard(text)`, under a 5 s lock bound: no table
   is locked or rewritten, and nothing calls it before the cutover. It is
   safe any time outside the windows in §5. Services built after the merge
   need this head before they take traffic (migration readiness), as with
   every migration.
3. **Deploy** before the cutover's prepare:
   - **the taskworker.** It carries the family: the runway, the retention, and
     the hook that abandons a stale prepare.
   - **the monitor** (another developer runs it). Its new contract accepts both
     layouts. An older monitor reports the partitioned journal as drift: its
     index definitions read ON ONLY, and the parent holds no fillfactor.
   - api, connect and alt need no new code for the journal; they follow the
     normal release.
4. **The A5 playbook:** before `autovacuum_tuning` runs again after the
   cutover, take `transfer_debit_journal` out of its table list.
   - A partitioned parent takes no storage settings, so the play's ALTER
     would fail.
   - The family gives every new leaf the same six options (fillfactor 70 and
     the five autovacuum options). The legacy leaf keeps the table's own.
   - That change is for xops; it is not part of this branch.
5. **Prepare** (§3), at least 12 h before the cutover day C.
6. **Finalize** (§4), before C − 2 h.
7. **Watch** for 2 days. The legacy leaf drops at C + 2 days by itself.

## 3. Prepare

Choose C, a day at 00:00Z, at least 12 h ahead (prepare refuses a nearer
day). The earliest sensible plan:
- prepare **Fri 11-20 between 07:30Z and 12:00Z**: after the Thursday full
  copy and the 07:00Z maintenance, and at least 12 h before C;
- **C = Sat 11-21 00:00Z**;
- finalize **Fri 11-20 before 22:00Z** (C − 2 h), clear of the Sat 22:00Z
  payout block.

If that window slips, prepare on a later day for the next C the same way
(any day but the payout block and the no-DDL windows of §5).

```
cd ~/urnetwork/server && git checkout main && git pull --ff-only
$E go run ./bringyourctl model migrate debit-journal-partition --cutover-day=2026-11-21 --dry-run
$E go run ./bringyourctl model migrate debit-journal-partition --cutover-day=2026-11-21
```

**What prepare does, each step resumable:**
1. Stages `transfer_debit_journal_p` (LIKE, every index and check, PARTITION BY
   RANGE (contract_id)).
2. Attaches its DEFAULT partition.
3. Stages the days C..C+3.
4. Adds `CHECK (contract_id < contract_id_floor(C)) NOT VALID`: one brief
   ACCESS EXCLUSIVE, under the quiet wait and the 100 ms lock wait.
5. Validates it: a scan of about 7 GiB under SHARE UPDATE EXCLUSIVE, on a
   session exempt from transaction_timeout. Settlement continues. Expect
   seconds to a few minutes, depending on how much of the table is cached.

**Gates:** `$T/pg187/gate.sh` must print GATE OK, and no long bringyourctl read
may be running through the tunnel.

**The bound's window:** from step 4 until the swap, the bound refuses a debit
of a contract minted at or after C. Contract ids carry their server's clock,
so none exists while C is more than 2 h away. The runway pass abandons a
prepare still unfinalized at C − 2 h, before that could happen.

## 4. Finalize

```
$E go run ./bringyourctl model migrate debit-journal-partition --finalize --dry-run
$E go run ./bringyourctl model migrate debit-journal-partition --finalize
```

- **First, a probe:** in a rolled-back transaction, it proves that every parent
  index adopts the table's own index.
- **Then one short transaction, sent as one script in one message,** so the
  lock is held for the database's work and no round trip from the Mac
  (about 90 ms each):
  - LOCK the table and the staged parent (100 ms lock wait, after the quiet
    wait, retried every 5 s up to 60 times);
  - assert, in SQL, that the prepared state is what the command read before
    the lock;
  - rename the table to `transfer_debit_journal_legacy` and its indexes to
    `*_l`;
  - attach it FROM (MINVALUE) TO (contract_id_floor(C)). The validated check
    proves the bound, so there is no scan;
  - rename the staged parent and its indexes to the published names.
- It prints how long the lock was held, catalog work only: 3.4 ms on the
  test database. The bound step (§3 step 4) held its lock 0.3–0.5 ms, an
  abort 5–9 ms and a rollback 11 ms; each is one message too. Then finalize
  analyzes the parent and checks the runway, which prepare already staged.
- **Gates:** both deploy locks (`mkdir $T/deploy.lock.d`, then
  `$T/deploy-proxy.lock.d`, each with an owner line; release in reverse) and
  GATE OK.

**Verify** (catalog only):

```
$P "SET statement_timeout='20s'; SELECT relkind, pg_get_partkeydef(oid) FROM pg_class WHERE oid='public.transfer_debit_journal'::regclass; SELECT c.relname, pg_get_expr(c.relpartbound,c.oid), pg_size_pretty(pg_total_relation_size(c.oid)) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent='public.transfer_debit_journal'::regclass ORDER BY 1"
```

Expect relkind `p` with `RANGE (contract_id)`, then the legacy leaf (about
7 GB), DEFAULT and the days from C. After C plus a few minutes, the day C leaf
holds the new debits. The debit worker's oldest-age gauges
(`urnetwork_transfer_debit_oldest_seconds`) and `flush_total{result}` look as
before.

## 5. Windows and gates for every step on Main

- **The validator gate:** idle, at least 15 minutes to its boundary
  (`$T/pg187/gate.sh`).
- **No DDL from Thu 11-19 05:00Z to about Fri 11-20 07:00Z** (the edge-6 full
  copy), nor during the Sunday 06:00Z copy that follows. The daily rotation
  runs through backups by design; its operations are catalog work on small
  leaves.
- **Database maintenance** runs at 03:00Z and 07:00Z; keep 30 minutes clear.
- **The contract reaper's backlog delete,** from about 11-17 00:00Z for 2–3
  days, adds IO. The prepare's validation scan reads about 7 GiB and is
  better after it ends.
- **The weekly payout block:** no steps from Sat 22:00Z to Sun 16:00Z.

## 6. Rollback and abort

- **Before finalize,** `--abort` (`--dry-run` first) drops the staged parent
  and the check. Nothing was copied, and nothing writes to the staged parent.
- **After finalize, while `transfer_debit_journal_legacy` exists** (until
  C + 2 days), use `--rollback --dry-run`, then `--rollback`. One script in
  one message:
  - locks the parent and every leaf;
  - detaches the legacy leaf;
  - copies the rows of the days and DEFAULT back into it, checking the count,
    with at most 200,000 rows;
  - drops the partitioned parent;
  - restores the published names.

  The legacy table keeps its storage settings.
- **After the legacy leaf drops,** there is no rollback. Fix forward; the
  journal is a queue, and its rows live seconds.
- **The monitor:** its contract accepts both layouts, so a rollback needs no
  monitor change.

## 7. What to watch afterwards

- **§2.4p `partition-runway`:** the family must keep 2 or more days ahead.
- **§2.4q `partition-retention-blocked`:**
  - a day still present 2 days past its retention holds debits;
  - that means the worker is behind, or a balance cannot be applied.
- **Expected numbers:**
  - day leaves of tens of MB each;
  - the parent's size near the live rows plus about 5 small leaves;
  - edge-2 `/` free up about 7 GiB at C + 2 days.
- **Re-bloat** is bounded to a day: a long snapshot pins only the leaves it
  covers, and they drop 2 days later.
