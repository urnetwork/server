# Contract table partitioning plan

Status: BUILT, NOT CUT OVER. Planned 2026-10-22; the owner settled every
**Decision** on 2026-10-23 (below). See "Implementation (2026-10-23)" for what
exists, where the build differs from this plan, and the production runbook.

## Decisions (settled 2026-10-23)

- **Retention horizon R: 13 weeks (91 days).** A weekly partition is dropped
  once its upper bound is older than R and it passes the preconditions in
  "Retention".
- **Granularity: weekly**, Monday 00:00 UTC bounds, the same on every table.
- **`contract_close_report_evidence`: the same horizon, by age.** A migration
  lets only the retention task delete rows older than R, in batches; the
  table stays plain and otherwise immutable. Since the horizon guard migration
  (owner 2026-11-01) the rows go at the contract horizon reaper's horizon
  while its mode is on, 35 days at the shortest, whatever the reaper's keep
  rules.
- **The cascade foreign key `contract_close_report -> transfer_contract` is
  dropped at cutover.** Reports leave with their contracts' partitions.
- Defaults taken: no MinIO export before a drop for now (MinIO is days from
  full; revisit with large objects only); the index diet is a separate,
  measured step after partitioning; the production cutover waits for edge-2's
  storage expansion and the owner's go-ahead.

Pattern to follow: the `client_reliability` partitioning
(`xops/db/client_reliability_partition_plan.md`, code in
`model/network_client_reliability_partition_model.go`). This plan differs from
it in two ways. The contract tables are written transactionally with foreign
keys and triggers, so writes cannot be paused behind a Redis buffer. And their
history must be kept, not discarded at cutover.

## Summary

Range-partition the contract tables by `contract_id`. Contract ids lead with
their 48-bit creation millisecond, so an id range is a time range. Use the same
weekly bounds on every table so a week's rows can be dropped together.

Partitioning fixes write amplification, maintenance cost and planner
statistics. It does not bound size. Size is bounded only by a retention
horizon, and today the effective horizon is 300 days because almost nothing is
reaped (see "Retention"). At the measured growth that does not fit on edge-2.

Recommended approach: a zero-copy cutover. Each existing table becomes the
first partition of its new parent (the "legacy partition", attached without a
copy), and new contracts go into weekly partitions. The cutover then needs no
free space and holds a lock for only seconds. The legacy partitions are
reclaimed later, by retention.

## Why (measured 2026-10-22 on main)

| Table | Total | Heap | Indexes | Live rows | Growth |
|---|---|---|---|---|---|
| `transfer_contract` | 1,895 GB | 588 GB | 1,307 GB | 2.37 B | 2.8 GB/h |
| `transfer_escrow` | 1,211 GB | 421 GB | 790 GB | 4.40 B | 0.25 GB/h |
| `contract_close` | 553 GB | 330 GB | 223 GB | 4.33 B | 1.0 GB/h |
| `transfer_escrow_sweep` | 222 GB | 101 GB | 120 GB | 536 M | 0.25 GB/h |
| `contract_close_report` | 73 GB | 40 GB | 32 GB | 451 M | 0.85 GB/h |
| `contract_close_report_evidence` | 130 GB | 57 GB | 68 GB | 454 M | 1.3 GB/h |
| `contract_extender` | 0.6 GB | | | 2.8 M | |

Growth is the size delta over four minutes at about 05:40Z. Identity churn
was high then (about 310 hosted window identities minted per second, against
about 60 a week earlier).

1. **Size.** These tables hold about 4.1 TB of the 6.67 TB database on
   edge-2's single 7 TB volume. Since the oldest row (2026-06-01) they have
   averaged about 0.9 TB a month.
2. **No retention in practice.** In a sample of 4,664 rows, under 1% in any
   month had `reap_time` set. `RemoveCompletedContracts` stamps `reap_time`
   only from a completed payment (completion + 7 days). Subsidy payouts
   stopped with the epoch that ended 2026-10-05, so rows wait for the 300-day
   straggler horizon (`StragglerContractExpiration`). The first rows reach it
   on 2027-03-28.
3. **Write amplification on a saturated disk.** `transfer_contract` has 21
   indexes, totalling 1.3 TB. Indexes that don't lead with `contract_id` or
   time get random inserts across their whole size, for example
   `open_source_id` (246 GB), `destination_id_create_time` (160 GB),
   `destination_id_close_time` (149 GB) and `transfer_escrow_balance_contract`
   (334 GB). edge-2's disk has been 90-100% busy for days. With weekly
   partitions, each of these is about 1/40 of today's size in the partition
   that takes the inserts.
4. **Maintenance never finishes.** Vacuum and reindex on multi-TB tables run
   for days. One `REINDEX TABLE CONCURRENTLY` on `transfer_escrow` has already
   caused an incident (`monitor/task_checks.go`). `SweepOrphanContractData`
   has never completed. Closed weekly partitions need neither.
5. **Statistics.** The 2026-07-17 incident was caused by `ANALYZE` missing the
   rare `open = true` rows. Per-partition statistics keep the current week's
   distribution visible.

## Verified inputs

- **Ids are time-ordered.** `connect.NewId` writes the wall-clock
  millisecond into the first 48 bits and keeps ids strictly increasing within
  a process.
  - Sample of 4,664 rows: none had an id time more than 60 s from
    `create_time`, and the largest skew was 4.8 s.
  - No contract id comes from `gen_random_uuid()` in `model/`.
  - The code already derives id lower bounds from time:
    `(lpad(to_hex(epoch_ms), 12, '0') || '00000000000000000000')::uuid` in
    `subscription_model.go` and `subscription_zero_escrow.go`.
- **Unique keys include the partition key.** Each in-scope table's only
  unique index is its primary key, and every one contains `contract_id`:
  - `transfer_contract (contract_id)`
  - `transfer_escrow (contract_id, balance_id)`
  - `contract_close (contract_id, party)`
  - `transfer_escrow_sweep (contract_id, balance_id, network_id)`
  - `contract_close_report (contract_id, party, report_id)`
  - `contract_extender (contract_id, extender_id, party)`
- **Foreign keys into these tables:**
  - `contract_close_report -> transfer_contract` ON DELETE CASCADE
  - `legacy_settlement_intent -> transfer_contract` (no action; its comment
    says it blocks retention). Retired 2026-10-24: the legacy settlement
    queue's retirement migration drops this key and the outcome guard below
    (PARTITION-MAINTENANCE-PLAN.md, Part B). The cutover reads the keys and
    triggers it finds, so after that migration it carries neither.

  Nothing references the other tables.
- **The partition key never moves.** No statement updates `contract_id`, and
  the usage guard trigger rejects it (`db_contract_usage_guard.go`).
- **Triggers to carry onto the parents:**
  - `transfer_contract`:
    - a BEFORE row usage guard
    - a BEFORE UPDATE OF `outcome` legacy guard
    - a BEFORE DELETE usage-archive capture into `st_provider_usage_archive`
    - a TRUNCATE guard
    - statement-level triggers with transition tables (net escrow revision,
      `db_net_escrow_revision.go`); `transfer_escrow` has these too
  - `transfer_escrow_sweep`: a BEFORE UPDATE OF `payment_id` guard
- **Contract deadlines.** `DefaultContractExpiration` is 60 minutes, and the
  existing code bounds closed-origin reads at 75 minutes
  (`CompanionOriginCloseLinger`).
  - That bound holds for every sampled contract created since 2026-10-12.
  - Contracts created in the weeks of 09-28 and 10-05 had deadlines up to 10
    days 16 hours after creation (317 of the 4,664 sampled). All of those
    expired by about 04:00Z on 2026-10-22.
  - Recheck that no unexpired contract violates the bound before adding it to
    the open-contract reads (step 2 below).
- **PostgreSQL 18** in production (`monitor/task_checks.go`).

## Scope

In, with the same weekly bounds on every table:

- `transfer_contract`
- `transfer_escrow`
- `contract_close`
- `transfer_escrow_sweep`
- `contract_close_report`
- `contract_extender`

Out for now:

- `contract_close_report_evidence`:
  - Its primary key is `(client_id, report_id)`, with no `contract_id`.
  - `contract_close_report.go` relies on `ON CONFLICT (client_id, report_id)`
    being unique across all contracts, which a partitioned table cannot
    enforce.
  - Keep it a plain table and give it its own retention (**Decision**). It is
    immutable by trigger, so retention means a migration that allows deleting
    rows older than the horizon.
- `transfer_debit_journal`: a queue that is drained continuously (582 live
  rows).
- `legacy_settlement_intent` and `st_provider_usage_archive`: small, and
  append-only or legacy.
- The other `contract_id` tables (`payment_plan_sweep`, `provider_work_*`,
  `audit_contract_event`): review separately.

## Partition key and bounds

- `PARTITION BY RANGE (contract_id)`.
- Add one SQL function and use it everywhere:
  `contract_id_floor(timestamptz) RETURNS uuid IMMUTABLE`. It returns the
  smallest id for that millisecond, built with the expression above.
- The bounds for the week starting at T are
  `[contract_id_floor(T), contract_id_floor(T + 7 days))`. Weeks start
  Monday 00:00 UTC. Partition names follow the existing style:
  `transfer_contract_w20261026`.
- No DEFAULT partition, as with `client_reliability`. An insert past the
  last partition fails, so the task creates 4 weeks ahead, and an alert fires
  when fewer than 2 future weeks exist.

Granularity: weekly is recommended.

| | weekly | monthly |
|---|---|---|
| Size of the newest partition | about 0.2 TB at the long-run average | about 0.9 TB |
| Partitions at a 13-week horizon | 13 | 3-4 |
| Partitions at the 300-day horizon | 43 | 10 |
| Unbounded queries probe | one index per partition | same |

Weekly keeps the inserting partition's indexes small enough to stay cached.
Choose monthly only if the horizon stays near 300 days. That depends on the
retention decision.

## Query changes required before cutover

The full inventory came from a read-only search on 2026-10-22. Line numbers
are approximate.

### Hot paths that must be bounded first

These run on connect's contract-creation path or per request. Partitioned,
they would probe every partition. Bound each one with
`contract_id >= contract_id_floor(statement_timestamp() - interval '75 minutes')`,
the bound the closed-origin branches already use:

1. `GetOpenTransferEscrowsOrderedByPriorityCreateTime`
   (`subscription_model.go` ~2500): unexpired contracts for a
   source/destination pair. Its caller is in `connect_controller.go`.
2. `createCompanionTransferEscrow`'s open branches (~2239-2410) and
   `companionOriginExistsSql` (~2100), plus `zeroEscrowPlainOriginSql` and
   `zeroEscrowChainedOriginSql` (`subscription_zero_escrow.go`). Their
   `ORDER BY create_time LIMIT 1` would otherwise merge across every
   partition.
3. `GetOpenContractIds` (~4027) and `GetOpenContractIdsForSourceOrDestination`
   (~4187). The second uses `source_id = $1 OR destination_id = $1`, which
   doubles the probes.
4. `contractHoleMembersSql` (`subscription_contract_hole.go`, ~340). The hole
   lifetime equals `DefaultContractExpiration`.
5. `GetOpenTransferByteCount` (~6723), by `payer_network_id`, and the
   `(open, payer_network_id)` reads in `prober_shard_model.go` (~288-318).

These bounds also help today: they shrink each read to the last 75 minutes
of each index. They can ship before partitioning and be measured.

As built, the reads in item 1 and 2 also carry the bounds of their partition
weeks as literals, which is what lets a partitioned table prune them at plan
time (see "The hot reads' partition weeks" under Implementation).

### Probing every partition is acceptable

These are not on the hot path, or they run per payment or per week. Bound
them where a natural bound exists:

- **`balance_id` escrow reads:**
  - net escrow witness and reconcile (`subscription_model.go` ~648, ~805-825;
    `subscription_net_escrow.go` ~313)
  - scheduled close owner (`subscription_contract_scheduled_close_owner.go`)
  - prober shard reap
  - balance retention

  Unsettled escrows are recent, so `settled = false` reads can take an id
  bound once a maximum unsettled age is established. Until then, the small
  per-partition unsettled partial index serves them.
- **`payment_id` and `network_id` sweep reads:**
  - payout planner (`account_payment_model_plan.go` ~476-563)
  - payment transitions
  - legacy corrections
  - SN reconcile
  - `GetTransferStats`
  - the points leaderboard rollup
  - `provider_payout_model.go`
- **Provider stats** (`provider_model.go`): `destination_id` plus a
  `create_time` or `close_time` window. A contract closes within its deadline,
  so `close_time >= T` implies `contract_id >= contract_id_floor(T - 75 min)`
  for contracts created since 2026-10-12.
- **Counts** in `contract_stats_model.go` and `postgres_checks.go`.

### Correctness blockers

- **`SweepOrphanContractData`** (`subscription_orphan_sweep.go`) deletes
  `WHERE ctid = ANY(...)`.
  - A ctid repeats across partitions, so this could delete rows outside the
    selected page.
  - Delete by primary key or by `(tableoid, ctid)` instead.
  - Once aligned partitions are dropped together, orphans stop accumulating,
    and this sweep can be retired.
- **The reaper** (`removeDueContractBatches`, ~5965) deletes through
  `USING candidate`. Rewrite it as `contract_id = ANY($ids)` so each delete
  prunes to its partitions.
- **`db_maintenance.go`:**
  - The priority REINDEX of named indexes, and its size query, which requires
    `relkind = 'r'`. Partitioned parents and their leaf indexes would be
    skipped silently.
  - Its skip list uses exact table names, so new leaf partitions would enter
    the `REINDEX TABLE CONCURRENTLY` rotation. Skip closed partitions, as is
    done for `client_reliability_p########`.
- **Migrations:**
  - `withIndexCompletion(810, ...)` compares exact `pg_get_indexdef` text, and
    a partitioned index renders with `ON ONLY`.
  - `ALTER TABLE ... SET (autovacuum...)` must target leaf partitions.
  - `CREATE INDEX CONCURRENTLY` is not supported on a partitioned parent.
    Future index migrations use the pattern
    `MigrateClientReliabilityToPartitions` already follows: create the index
    `ON ONLY` the parent, then concurrently on each partition, then attach.
- **`subscription_model.go` ~6535** runs
  `ALTER TABLE transfer_escrow_sweep ADD COLUMN IF NOT EXISTS` from Go. Move
  it into a migration.
- **Monitor checks:**
  - `migration_fp2.go` requires `relkind = 'r'`.
  - `migrations_close_report.go` and `migrations_legacy_settlement.go`
    hard-code foreign key trigger sets.
  - `postgres_checks.go` and `battery.go` read statistics and indexes by
    `relname = 'transfer_contract'`. Aggregate across the partitions instead.

## Cutover: attach the existing tables as legacy partitions

Copying 4 TB is not possible on edge-2: it needs about 4 TB of free space, and
the disk is already saturated. The contract tables are also mutable, because
`outcome`, `close_time`, `reap_time` and the settle flags change after insert.
A consistent copy would need change capture and replay, which is what
pg_repack does internally. Avoid both.

PostgreSQL can attach an existing table as a partition without a copy. Its
validation scan is skipped when a valid CHECK constraint already implies the
partition bound, and existing indexes that match the parent's are attached
instead of rebuilt.

For each table, with C a future Monday 00:00 UTC chosen at dry-run:

1. **Pre-validate without a long lock.** Run `ALTER TABLE t ADD CONSTRAINT
   t_legacy_bound CHECK (contract_id < contract_id_floor(C)) NOT VALID`, then
   `VALIDATE CONSTRAINT`. The validation scans under SHARE UPDATE EXCLUSIVE,
   so reads and writes continue. New rows satisfy the check as long as the
   clock is before C.
2. **Build the empty partitioned parents.** Create `t_p` with the same
   columns, the primary key, and every index of `t` declared `ON ONLY`. Create
   weekly partitions from C through C + 4 weeks, with their indexes.
3. **Swap in one transaction.** Use a 15 s lock_timeout and retry up to 5
   times, as in the `client_reliability` swap. The transaction:
   - drops `t`'s own triggers (the parent's are cloned on attach);
   - renames `t` to `t_legacy`;
   - attaches `t_legacy` as the partition `FROM (MINVALUE) TO (contract_id_floor(C))`
     and attaches each legacy index to its `ON ONLY` parent index;
   - renames `t_p` to `t`;
   - recreates the triggers on `t`.

   Swap all six tables in the same transaction so foreign keys and triggers
   never see a mixed state.
4. **Foreign keys.** Recreate `contract_close_report -> transfer_contract` and
   `legacy_settlement_intent -> transfer_contract` on the parents as
   `NOT VALID`. They are enforced for new rows. Validate later, off-peak, or
   drop the cascade FK (**Decision**). The cascade is not needed once
   `contract_close_report` partitions are dropped alongside their contracts.
5. **ANALYZE** the parents. Leave the legacy partitions as they are.

The lock window covers catalog changes only, so it should take seconds.
Rehearse it first:

- Rehearse on a restored copy, or on a local database loaded with synthetic
  volume.
- On PostgreSQL 18, check in that rehearsal:
  - statement-level transition-table triggers on a partitioned root see the
    rows of every partition;
  - BEFORE row triggers clone onto partitions;
  - whether an attached table's same-named triggers or indexes conflict;
  - that every parent index finds an exact match on the legacy table. If one
    doesn't, ATTACH would build that index under lock, on terabytes.

After cutover, every new week is a small partition. The legacy partitions
keep everything before C and are reclaimed by retention: dropped whole once
their newest row passes the horizon and they hold nothing that must stay. To
reclaim them sooner, split them by copy into monthly partitions once edge-2
has more storage and their rows have resolved.

Expose the cutover as an operator command, not a boot migration:
`bringyourctl model migrate contract-partition [--dry-run] [--finalize]`.

## Retention (Decision)

Today the horizon is effectively 300 days, because `reap_time` is stamped only
from completed payments. To make dropping partitions possible, define a
horizon R. A weekly partition is dropped only when its upper bound is older
than R and every row in it meets all of these:

- the contract is resolved (`outcome IS NOT NULL`, no open dispute);
- every escrow is settled (`transfer_escrow.settled`);
- no sweep belongs to a pending payment;
- no `legacy_settlement_intent` row references it (PostgreSQL also refuses to
  drop a partition that a foreign key still references). Retired 2026-10-24:
  the queue's retirement drops that key, and the weekly drop no longer
  consults the table;
- the subnet usage proof is archived. A drop bypasses the BEFORE DELETE
  capture trigger, so the drop task first copies settled `provider_usage` rows
  into `st_provider_usage_archive` with the same predicate as
  `ProviderUsageArchiveCaptureFunctionBodySql`.

Optionally, export the week to MinIO as compressed large objects before
dropping it. Do not export as one object per row; see the MinIO note below.

Each table drops its partition for week W in one transaction, children
first. A week whose precondition fails stays, and the task reports it.

Space at a horizon of 13 weeks (91 days): about 2.7 TB at the long-run
average of 30 GB a day. At the 2026-10-22 churn (about 155 GB a day for these
tables) it would be about 14 TB. Identity churn therefore has to come down
whatever the horizon.

## Ongoing maintenance

Extend the existing task pattern from `remove_old_client_reliability_stats`:

- Create partitions 4 weeks ahead. Each new partition gets the leaf
  autovacuum settings.
- Drop partitions that have expired and pass the precondition check, with a
  5 s lock_timeout, retrying on the next run.
- Run `ANALYZE` on a partition once its week has closed.
- Exclude closed partitions from the REINDEX rotation.
- Alert when fewer than 2 future partitions exist, or when an expired
  partition has been blocked for more than 2 weeks.

## Optional: an index diet

Within seconds, `contract_id` order is `create_time` order. A predicate
`create_time >= T` can therefore add `contract_id >= contract_id_floor(T -
interval '1 minute')` and be served by the primary key or by partition
pruning.

That could remove the create_time-led indexes on `transfer_contract`:

- `create_time` (221 GB)
- `reap_pending_create_time` (66 GB)
- `open_partial_create_time` (4 GB)

`open_source_id` (246 GB) indexes closed rows too. A partial index on open
rows would be a fraction of that size.

Do this per query with plans, either before partitioning or after. It
reduces write amplification right away.

## Order of work

1. **Decide** the retention horizon R and the granularity (weekly
   recommended), and what to do with the evidence table.
2. Bound the hot-path reads (section "Hot paths"), deploy, and measure. This
   is independent of partitioning and helps today.
3. Fix the correctness blockers: the ctid delete, the reaper's `USING`, the
   catalog assumptions in maintenance, monitor checks and migrations.
4. Build the cutover command with a dry-run. Write DB tests that follow the
   `client_reliability` partition lifecycle test, and run them after merge.
5. Rehearse the swap on PostgreSQL 18 with production-shaped schema
   (triggers, transition tables, foreign keys, index matching).
6. Cut over after edge-2's storage expansion. The cutover itself needs no
   copy space, but the margin matters if it has to be rolled back.
7. Turn on retention drops once the first weekly partition passes R.

## Implementation (2026-10-23)

Steps 1-5 are done. Step 6 waits for the owner (see "Production runbook").
Step 7 needs no action: the maintenance task drops each week once it qualifies.

### What exists

- Migration 856 adds `contract_id_floor(timestamptz)`.
- Migration 857 replaces the close evidence guard. A transaction that sets
  `urnetwork.close_evidence_retention = 'on'` may delete evidence accepted
  more than 91 days ago (13 weeks). Updates, truncates and newer rows stay
  refused.
- The monitor's migration registry covers 855-857.
- The cutover and retention code is in the `server` package:
  - `db_contract_partition.go`: naming, bounds, catalog shapes.
  - `db_contract_partition_cutover.go`: prepare, finalize, rollback, abort.
  - `db_contract_partition_maintenance.go`: the task's body.
- The operator command is
  `bringyourctl model migrate contract-partition [--cutover-week=<date>] [--dry-run] [--finalize] [--rollback] [--abort]`.
- The task `maintain_contract_partitions` runs every 15 minutes, or a minute
  after a run that left work behind. Each run has a 10-minute budget: two
  thirds for the partitions, a third for the close evidence. On plain tables
  it only removes evidence and guards a prepared cutover, so it is safe to
  deploy before the cutover.
- `URNETWORK_TEST_CONTRACT_PARTITIONS=1` cuts every test database over to
  partitioned tables (the current week as the cutover week) right after its
  migrations. Any suite can then run against the partitioned layout.

### Where the build differs from the plan above

1. **Parent indexes are cloned, not rebuilt from their definitions.**
   - On PostgreSQL 18 the printed definition of
     `transfer_contract_usage_missing_time` parses into a different expression
     tree: the varchar array cast moves into each element.
   - ATTACH adopts a legacy index only when the trees are equal. Otherwise it
     would have built that index on the whole legacy table, under the swap's
     lock.
   - So each staged parent is created with `LIKE ... INCLUDING INDEXES`, which
     copies the stored trees.
   - Before the swap, finalize proves the match: it attaches an empty `LIKE`
     clone of each table to the staged parent, in a transaction it rolls back,
     and checks that every parent index adopted one of the clone's indexes.
2. **Checks keep their validity.** A `NOT VALID` check stays `NOT VALID` on
   the parent, because a valid parent check cannot adopt a `NOT VALID` one
   (`transfer_contract_provider_usage_shape`,
   `transfer_escrow_sweep_provider_payouts_shape`).
3. **Locks wait for a quiet table.** Every step that takes ACCESS EXCLUSIVE
   (the legacy bound, the swap, rollback, abort, a week's drop) first waits
   until no transaction older than 5 s holds a lock on a contract table.
   Each lock wait is then at most 3 s, and a statement that would scan or
   build is cut off after 30 s. With about 600 idle-in-transaction sessions
   on main today, the swap would wait until the transaction fixes (escrow
   out of the transaction, the shorter close-report transaction) are
   deployed.
   - **Revised 2026-11-17** (`db_contract_partition_admission.go`), for Main
     as it now runs: the large contract tables are under autovacuum most of
     the day, often anti-wraparound passes of hours, and every main-pool
     statement gives up after `PgLockTimeout` (2 s) behind a lock.
   - The cutover's steps (the legacy bound, its validation, the swap, abort
     and rollback) never queue behind an autovacuum worker, whatever its age:
     a request that waits deadlock_timeout (1 s) behind one cancels a routine
     pass, and waits out its whole lock timeout behind an anti-wraparound
     one. They still wait out any transaction older than 5 s that holds a
     lock the request conflicts with, and a prepared transaction.
   - The scope of each step is the relations it locks and every partition
     below them, so rollback sees the vacuums of the leaves. Each prepare
     step and abort reads only its own table's scope.
   - Each ACCESS EXCLUSIVE wait is at most 1 s (`ContractPartitionCutoverSettings.LockTimeout`,
     rollback's `LockTimeout`), so a statement queued behind it stays under
     `PgLockTimeout` with the step's own work. The validation waits at most
     5 s for its SHARE UPDATE EXCLUSIVE lock, which queues no reads or
     writes.
   - Prepare no longer waits on a busy table: it defers that table's bound
     or validation to the next run, prepares the others, and ends with
     `ContractPartitionDeferredError`. Run it again until it reports
     "prepared".
   - The swap logs how long it waited for its locks and how long it held
     them; the dry run reports whether the swap would be admitted now.
   - A week's drop keeps its admission (parents, transactions older than 5 s).
4. **TRUNCATE guards stay on every leaf.** They are kept on the legacy
   partition and repeated on each weekly partition, because a parent's
   statement trigger does not fire for a statement that names a partition.
5. **The outside foreign key is validated at once.**
   `legacy_settlement_intent`'s foreign key is recreated `NOT VALID` on the
   parent and validated right after the swap; that table is small. Once the
   legacy settlement queue's retirement has applied (2026-10-24) there is no
   outside foreign key left to carry.
6. **The close report foreign key is dropped**, as decided. Rollback restores
   it `NOT VALID`.
7. **The swap can be undone.** Between the swap and the cutover week every new
   contract still lands in the legacy partition, so `--rollback` can undo the
   swap until a weekly partition holds a row. Rollback takes exclusive locks
   on all six parents and their children before reading the current inventory
   and checking emptiness. These checks and every drop share one transaction.
8. **A stale prepare is abandoned.** The prepared check refuses ids from the
   cutover week on. The task therefore abandons a cutover that is still
   unfinalized 6 hours before that week, dropping the staged parents and the
   checks.
9. **The legacy partition covers the weeks before the cutover week** until
   that week starts. Maintenance neither creates partitions for those weeks
   nor counts them as missing.

### The hot reads' partition weeks (2026-11-17)

Gaps B1 and B3 of the 11-17 readiness review (`temp/contract-cutover-1117/RUNBOOK.md`
§2 and §4.3), built on branch `fix/contract-hot-read-bounds-1117`: commits
4a2e9b0b8 (B1), 5c7df1bfb (B3) and 7df8e6bbd (tests), merged to main on
2026-11-17. No migration. The change is default on and ships with any build
from main.

- **B1, the hot reads.** Each read bounded by the 75-minute floor on
  `statement_timestamp()` also carries the bounds of the whole partition weeks
  that hold every id the floor admits. They are computed in the process and
  rendered into the statement as uuid literals before it runs
  (`model/contract_id_bound.go`: `unexpiredContractIdBoundSql`,
  `unexpiredContractWeeksAt`).
  - The weeks run from the start of the week of now less 90 minutes (the
    floor's age plus the 15-minute margin for this process's clock against the
    database's) to the end of the week of now plus 30 minutes.
  - The reads: the companion origin existence read, the plain and chained
    origin selections and the zero escrow origin reads, each in its side-table
    and its legacy form, and `GetOpenTransferEscrowsOrderedByPriorityCreateTime`.
  - PostgreSQL prunes partitions at plan time only by constants, so the
    literals prune the custom plans and the generic plan it caches alike. A
    statement's text changes only when a week boundary passes the window,
    twice a week, so each keeps one cached plan.
  - The floor still decides the rows. Bound parameters in its place were
    measured and rejected: PostgreSQL replanned every execution on the
    partitioned layout (a generic plan keeps every partition and never costs
    less than a pruned custom one), and estimated a range on a parameter at
    0.5% of the table, which turned the generic chained origin selection on
    the plain table from its ordered pair index to
    `transfer_contract_open_source_id` and a sort.
- **B3a, `strandedGrantPayersSql`.** Each grant's escrow probe reads only the
  ids of the grant's lifetime: `e.contract_id >= ContractIdCreatedFromSql(b.start_time)`
  (an escrow is published only while its grant is current), and below the end
  of the week that holds the window's end plus the margin, a rendered literal.
  On the partitioned layout each probe executes only the partitions from the
  grant's start, and the weeks after the window are not planned.
- **B3b, `RequestRemovedClientContractCloses`.** Both arms carry the hot
  reads' bound (`removedClientOpenContractsSql`). An open contract minted
  before the 75-minute floor is past its deadline and is left to the
  force-close sweep, as when a full queue drops the read. A counterparty with
  a live sequence with the client holds an open contract inside the bound, so
  it is still asked to close every sequence with the client.
- **Tests** (model): on the partitioned layout each read plans and locks only
  its weeks, in custom and generic plans, while its statement with the floor
  alone plans and locks the weeks ahead; the stranded grant read probes only
  the grant's weeks. On the plain tables each read returns the same rows and
  scans the same indexes as before, in both plan modes.
- **Measured** on postgres:18.7 with 1M contracts, the plain tables against
  the legacy leaf and five weekly leaves (the layout between the swap and the
  cutover week), 64 pgbench clients sending every argument as a parameter, as
  the app does. Medians of three alternating rounds, under outside load, so
  compare within a row. "Fresh" plans every execution (`-M extended`);
  "cached" is the plan cache (`-M prepared`), which chose generic plans in
  every case.

  | Read | Plain, floor | Partitioned, floor | Partitioned, weeks | Plain, weeks |
  |---|---|---|---|---|
  | rehearsal pair read, fresh / cached | 29.8k / 134k tps | 7.2k / 77k | 25.6k / 154k | 25.2k / 139k |
  | companion origin exists | 14.1k / 97k | 3.2k / 44k | 12.0k / 108k | 13.4k / 94k |
  | legacy companion origin exists (Main today) | 16.6k / 120k | 4.3k / 49k | 11.2k / 107k | 16.1k / 110k |
  | open transfer escrows | 14.8k / 116k | 3.7k / 40k | 11.3k / 119k | 12.8k / 107k |
  | removed client open contracts (vs the floor) | 18.2k / 127k | 5.6k / 63k | 17.7k / 158k | 18.0k / 123k |
  | plain origin selection | 2.6k / 58k | 0.7k / 23k | 2.1k / 58k | 2.6k / 53k |
  | legacy plain origin selection (Main today) | 3.5k / 68k | 0.7k / 27k | 1.8k / 72k | 2.4k / 69k |
  | chained origin selection | 4.3k / 55k | 1.0k / 23k | 3.1k / 58k | 3.7k / 50k |

  - Locks per execution of a custom plan on the partitioned layout fell from
    106-155 (64 fast-path) to 31-49, all fast-path; the plain table's are
    16-30.
  - Against the plain tables today, the partitioned reads with their weeks run
    3-47% slower when planned fresh and between 11% slower and 24% faster
    from the cached plan; with the floor alone they ran 69-80% slower fresh
    and 42-65% slower cached.
  - On the plain tables the weeks cost fresh plans 1-31% (the planner
    estimates two more range conditions per bound) and cached executions up
    to 9% on some reads (two more conditions to set up and check), within
    noise on others. PostgreSQL plans fresh only for a statement's first five
    executions on a connection and after an invalidation such as an ANALYZE.
  - B3a's per-grant bound costs about 1 µs a probe on the plain tables, about
    1.1 s on a read of Main's heaviest window (1.1M grants, about 5 s today).
- Not changed: `contractHoleMembersSql`, `hasOpenContractForPairSQL`,
  `GetOpenTransferByteCount` and the `GetOpenContractIds` reads have no
  contract id bound at all, and the first two admit contracts with no
  deadline, so a bound would change their rows. They probe one partial index
  per partition, as accepted in b23fb286b.

### Retention as built

- **Dropping a week.** A week's partitions are dropped once the week ended
  more than 13 weeks ago and none of these blockers holds: an unresolved
  contract, an unsettled escrow, or a payment hold on a sweep. (A legacy
  settlement intent was a fourth blocker until the queue retired on
  2026-10-24; PARTITION-MAINTENANCE-PLAN.md, Part B.) Payment holds match the
  row reaper: pending retention stamping, or an incomplete payment that is
  uncanceled or carries an external attempt's idempotency key, payment
  record, or transaction hash. Canceling an external attempt alone does not
  release its hold. Each check reads a partial or key index of the week's own
  partitions.
- **Usage proof is archived by deleting contracts.** All three outcomes carry
  usage credit. So archiving a week means deleting every one of its contracts,
  10,000 at a time, through the parent. The capture trigger archives each row
  in the same transaction, as the reaper does. Archive and delete must stay
  atomic: the subnet usage reader fails any window that sees a contract both
  live and archived. Each delete repeats the week's range, so it prunes to
  that week's partition.
- **The drop transaction.** The week's six partitions are then detached and
  dropped in one transaction, children first. The blockers are checked again
  on the detached partitions, which no statement can reach.
- **The legacy partition** retires the same way once the cutover week plus 13
  weeks has passed. Archiving its contracts row by row would take weeks.
  Splitting it by copy into monthly partitions after the storage expansion is
  still the faster way to reclaim it.
- **Close evidence** is deleted once accepted more than 91 days ago, if its
  contract is resolved or gone; while the contract horizon reaper's mode is
  on, once accepted longer ago than its horizon, down to 35 days. The oldest
  evidence on main is from 2026-10-06, so at 35 days deletes begin around
  2026-11-10 (at 91, around 2027-01-05), at the pace of their own brake. With
  the mode on at `horizonDays` 35 they permanently delete about a billion
  rows, the evidence accepted from 2026-10-06 on, whether or not the reaper's
  keep rules keep the contract. The brake
  (`bringyourctl db close-evidence-retention`) stops the row deletes, but not
  the weekly drops once the evidence is partitioned, which never read it; the
  reaper's `mode off` stops both below 91 days, from the next run.
- **Archive growth needs its own decision.** `st_provider_usage_archive` is
  1.5 GB for 8.7 M rows today, about 176 bytes a row. Once retention archives
  every contract, it grows about as fast as contracts are created: about
  20 GB a week at the 2026-10-22 churn.

### Rehearsals

- **Migrated schema.** The lifecycle and maintenance DB tests run on
  PostgreSQL 18 with the migrated schema. They cover dry runs, resumable
  prepare, routing by id, the usage guard and archive capture through the
  parent, the leaf TRUNCATE guards, rollback, abort, every blocker, archiving,
  the drop, the evidence deletes and the abandoned cutover.
- **Production schema.** Main's schema (a schema-only dump taken 2026-10-23,
  with its extensions stripped) was restored locally. Prepare, the probe, the
  swap (220 ms), maintenance, rollback, prepare again and abort all passed.
  Main has one contract index that no migration creates:
  `transfer_escrow_unsettled_legacy_balance_contract` (204 MB). `LIKE` stages
  it like the rest.
- **Volume.** With 1 M contracts and escrows the swap took 188 ms, the same as
  on empty tables. Validation is the step that scales. Main has about 1.5 TB
  of heap in these tables, so expect hours at edge-2's current I/O. Reads and
  writes continue during it, but it holds back the vacuum horizon, so run
  prepare off-peak.

### Production runbook

The runbook for Main as of 2026-11-17 (PostgreSQL 18.7, near-continuous
autovacuum on the large tables, the disk headroom the cutover week needs) is
`temp/contract-cutover-1117/RUNBOOK.md` in the workspace. It supersedes the
steps below where they differ: prepare is rerun until it reports "prepared",
finalize runs with the taskworker stopped through the settlement gate, and the
cutover week is chosen for disk headroom, not as the nearest Monday.

1. Deploy a build with this code and apply migrations 856-857. Also deploy
   the transaction fixes.
2. `bringyourctl model migrate contract-partition --cutover-week=<Monday at least 2 days ahead> --dry-run`
   prints every statement and checks for invalid indexes.
3. Run the same command without `--dry-run`, off-peak, to prepare. It is
   resumable.
4. Complete `--finalize --dry-run` and then `--finalize` before the cutover
   week minus 6 hours, allowing time for verification. Maintenance abandons an
   unfinished prepare at that boundary. The command's 10-minute final refusal
   margin is an additional safeguard, not the operating deadline. Finalize
   probes, swaps, analyzes the parents and validates the outside foreign key.
5. Watch contract creation, the runway probe and the task's `[cp]` log lines.
   Until the cutover week starts, `--rollback` undoes the swap.
   - Rollback restores `contract_close_report_contract_id_fkey` NOT VALID.
     The monitor's migration-coherence check pages until it is validated off
     peak: `ALTER TABLE contract_close_report VALIDATE CONSTRAINT contract_close_report_contract_id_fkey`.
   - Once partitioned, the monitor requires that key to stay absent: a key
     restored by hand, or by `db audit --fix`, would block each week's drop.

## Related: MinIO and client-key history

`st_client_key_history` (382 GB, 4.3 GB/h) is not a contract table, but its
growth has the same cause, identity churn. Each new window identity writes
one immutable signed registration row of about 3.7 KB. The evidence bytes are
also written to MinIO twice, under the content key and the history key.

MinIO's raw free space fell from 12.3 TB to 8.2 TB between 2026-10-19 and
2026-10-22. About 2.75 TB of that was a PostgreSQL base backup, written beside
MinIO on the same drives on 10-19/20 and invisible to MinIO's own metrics. The
rest is these evidence objects: about 1 TB of raw space a day on 10-22, at
roughly 20–25 KB of raw disk per 2 KB object on the 4-drive erasure set. That
is about 8 days to full at the current rate (SIGNALS §11.6b). Small objects are
expensive there, which is why any export above must use large objects.
