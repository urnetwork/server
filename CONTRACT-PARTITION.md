# Contract table partitioning plan

Status: BUILT, NOT CUT OVER. Planned 2026-10-22; the owner settled every
**Decision** on 2026-10-23 (below). See "Implementation (2026-10-23)" for what
exists, where the build differs from this plan, and the production runbook.
**Daily (owner decision 2026-11-17 ~15:35Z):** the partitions are daily, one
per UTC day, not weekly. The cutover, the hot reads' bounds and S1 were
reworked the same day (section "Daily partitions", below); where the older
sections below say week, the build now has a day unless they describe the
2026-10-23 build.
**S1 (2026-11-17):** the days retire at the contract horizon reaper's
horizon, and retirement deletes no row: it keeps exactly what the reaper
keeps (section "S1", below).
**B1 and B3 (2026-11-17):** the hot contract reads carry their partition
days as literals, so a partitioned table prunes them at plan time, and the
stranded grant and removed client reads are bounded by contract id (section
"The hot reads' partition days", under Implementation).

## Decisions (settled 2026-10-23)

- **Retention horizon R: 13 weeks (91 days).** A weekly partition is dropped
  once its upper bound is older than R and it passes the preconditions in
  "Retention". *Superseded by S1 (2026-11-17):* a day retires 6 hours after
  its upper bound passes the contract horizon reaper's horizon, 35 days while
  the reaper's mode is on, and keeps every row the reaper keeps. R stays the
  longest retention, the one that applies with the reaper's mode off.
- **Granularity: weekly**, Monday 00:00 UTC bounds, the same on every table.
  *Superseded 2026-11-17 ~15:35Z (owner):* **daily**, 00:00 UTC bounds, the
  same on every table (section "Daily partitions").
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
daily bounds on every table so a day's rows can be dropped together (weekly
until 2026-11-17).

Partitioning fixes write amplification, maintenance cost and planner
statistics. It does not bound size. Size is bounded only by a retention
horizon, and today the effective horizon is 300 days because almost nothing is
reaped (see "Retention"). At the measured growth that does not fit on edge-2.

Recommended approach: a zero-copy cutover. Each existing table becomes the
first partition of its new parent (the "legacy partition", attached without a
copy), and new contracts go into daily partitions. The cutover then needs no
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
- The bounds for the day starting at T are
  `[contract_id_floor(T), contract_id_floor(T + 1 day))`. Days start at
  00:00 UTC. Partition names follow the partition registry's daily style:
  `transfer_contract_p20261221` (weekly ones were `transfer_contract_w20261026`).
- No DEFAULT partition, as with `client_reliability`. An insert past the
  last partition fails, so the task creates 3 days ahead, and an alert fires
  when fewer than 2 future days exist (§2.4h).

Granularity: weekly was recommended on 10-22 and decided on 10-23; **the owner
chose daily on 2026-11-17 (~15:35Z)**, once S1 put the horizon at 35 days.

| | daily (2026-11-17) | weekly | monthly |
|---|---|---|---|
| Size of the newest partition | 140-180 GiB at Main's 11-17 inflow | 1.0-1.3 TiB at that inflow (0.2 TB at the long-run average) | about 0.9 TB at the average |
| Partitions at S1's 35-day horizon | 36-37, plus 3 ahead | 6-7, plus 4 ahead | 2 |
| Partitions at a 13-week horizon | 92 | 13 | 3-4 |
| Held past the horizon before a drop | at most a day and 6 hours | up to 8 days | up to a month |
| Unbounded queries probe | one index per partition | same | same |

Daily keeps a day's rows from waiting up to a week past the horizon, and
keeps the inserting partition's indexes small; its cost is the number of
partitions that a read without a contract id bound plans (section "Daily
partitions").

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
days as literals (weeks until 2026-11-17), which is what lets a partitioned
table prune them at plan time (see "The hot reads' partition days" under
Implementation).

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

For each table, with C a future Monday 00:00 UTC chosen at dry-run (since
2026-11-17 any future day at 00:00 UTC):

1. **Pre-validate without a long lock.** Run `ALTER TABLE t ADD CONSTRAINT
   t_legacy_bound CHECK (contract_id < contract_id_floor(C)) NOT VALID`, then
   `VALIDATE CONSTRAINT`. The validation scans under SHARE UPDATE EXCLUSIVE,
   so reads and writes continue. New rows satisfy the check as long as the
   clock is before C.
2. **Build the empty partitioned parents.** Create `t_p` with the same
   columns, the primary key, and every index of `t` declared `ON ONLY`. Create
   weekly partitions from C through C + 4 weeks, with their indexes (since
   2026-11-17 daily partitions from C through C + 3 days).
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

After cutover, every new day is a small partition. The legacy partitions
keep everything before C and are reclaimed by retention: dropped whole once
their newest row passes the horizon and they hold nothing that must stay. To
reclaim them sooner, split them by copy into monthly partitions once edge-2
has more storage and their rows have resolved.

Expose the cutover as an operator command, not a boot migration:
`bringyourctl model migrate contract-partition [--dry-run] [--finalize]`.

## Retention (Decision)

*Superseded by S1 (2026-11-17), below:* retirement no longer checks these
preconditions or archives anything. The reaper alone deletes contracts, by its
keep rules, and a week's partitions drop once they hold no row. This section
is the 2026-10-22 plan.

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

- Create partitions 3 days ahead (4 weeks ahead in the weekly build). Each
  new partition gets the leaf autovacuum settings.
- Drop partitions that have expired and pass the precondition check, with a
  5 s lock_timeout, retrying on the next run. *Since S1:* compact each
  expired day the reaper has emptied, and drop it once it holds no row, under
  the 11-17 lock admission with waits of at most 1 s.
- Run `ANALYZE` on a partition once its day has closed, and on the parents
  once a day.
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
  - `db_contract_day_retirement.go`: S1's retirement of expired days.
- The operator command is
  `bringyourctl model migrate contract-partition [--cutover-day=<date>] [--dry-run] [--finalize] [--rollback] [--abort]`
  (`--cutover-week=<Monday>` until 2026-11-17).
- The task `maintain_contract_partitions` runs every 15 minutes, or a minute
  after a run that left work behind. Each run has a 10-minute budget: two
  thirds for the partitions, a third for the close evidence. On plain tables
  it only removes evidence and guards a prepared cutover, so it is safe to
  deploy before the cutover.
- `URNETWORK_TEST_CONTRACT_PARTITIONS=1` cuts every test database over to
  partitioned tables (the current day as the cutover day) right after its
  migrations. Any suite can then run against the partitioned layout.
  `server.Testing_PartitionContractsFrom(ctx, day)` cuts a test database over
  at a past day, with every daily partition from it through the days ahead.

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
   (the legacy bound, the swap, rollback, abort, a range's drop) first waits
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
   - A range's drop has the same admission since S1 (the six parents, and the
     day's leaves with their TOAST).
4. **TRUNCATE guards stay on every leaf.** They are kept on the legacy
   partition and repeated on each daily partition, because a parent's
   statement trigger does not fire for a statement that names a partition.
5. **The outside foreign key is validated at once.**
   `legacy_settlement_intent`'s foreign key is recreated `NOT VALID` on the
   parent and validated right after the swap; that table is small. Once the
   legacy settlement queue's retirement has applied (2026-10-24) there is no
   outside foreign key left to carry.
6. **The close report foreign key is dropped**, as decided. Rollback restores
   it `NOT VALID`.
7. **The swap can be undone.** Between the swap and the cutover day every new
   contract still lands in the legacy partition, so `--rollback` can undo the
   swap until a daily partition holds a row. Rollback takes exclusive locks
   on all six parents and their children before reading the current inventory
   and checking emptiness. These checks and every drop share one transaction.
8. **A stale prepare is abandoned.** The prepared check refuses ids from the
   cutover day on. The task therefore abandons a cutover that is still
   unfinalized 6 hours before that day, dropping the staged parents and the
   checks.
9. **The legacy partition covers the days before the cutover day** until
   that day starts. Maintenance neither creates partitions for those days
   nor counts them as missing.

### The hot reads' partition days (2026-11-17)

Gaps B1 and B3 of the 11-17 readiness review (`temp/contract-cutover-1117/RUNBOOK.md`
§2 and §4.3), built on branch `fix/contract-hot-read-bounds-1117`: commits
4a2e9b0b8 (B1), 5c7df1bfb (B3) and 7df8e6bbd (tests), merged to main on
2026-11-17 with week bounds, and moved to day bounds the same day with the
daily partitions (section "Daily partitions"). No migration. The change is
default on and ships with any build from main.

- **B1, the hot reads.** Each read bounded by the 75-minute floor on
  `statement_timestamp()` also carries the bounds of the whole partition days
  that hold every id the floor admits. They are computed in the process and
  rendered into the statement as uuid literals before it runs
  (`model/contract_id_bound.go`: `unexpiredContractIdBoundSql`,
  `unexpiredContractDaysAt`).
  - The days run from the start of the day of now less 90 minutes (the
    floor's age plus the 15-minute margin for this process's clock against the
    database's) to the end of the day of now plus 30 minutes: the current
    day's partition alone from 01:30 to 23:30 UTC, and the previous or the
    next day's too around midnight.
  - The reads: the companion origin existence read, the plain and chained
    origin selections and the zero escrow origin reads, each in its side-table
    and its legacy form, and `GetOpenTransferEscrowsOrderedByPriorityCreateTime`.
  - PostgreSQL prunes partitions at plan time only by constants, so the
    literals prune the custom plans and the generic plan it caches alike. A
    statement's text changes when a day boundary enters or leaves the window,
    at 23:30 and at 01:30 UTC (with weeks, twice a week, Sunday 23:30 and
    Monday 01:30). Each connection then plans the new text afresh: five
    custom plans, then the cached generic one, twice a day.
  - An id after the days was minted by a host more than twice the margin (30
    minutes) ahead of this one, the same guarantee as with weeks; the days
    reach that minimum every day at 23:29, the weeks every Sunday.
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
  of the day that holds the window's end plus the margin, a rendered literal.
  On the partitioned layout each probe executes only the partitions from the
  grant's start, and the days after the window are not planned.
- **B3b, `RequestRemovedClientContractCloses`.** Both arms carry the hot
  reads' bound (`removedClientOpenContractsSql`). An open contract minted
  before the 75-minute floor is past its deadline and is left to the
  force-close sweep, as when a full queue drops the read. A counterparty with
  a live sequence with the client holds an open contract inside the bound, so
  it is still asked to close every sequence with the client.
- **Tests** (model): on the partitioned layout each read plans and locks only
  its days, in custom and generic plans, while its statement with the floor
  alone plans and locks the days ahead; days after a cutover, each read plans
  the current day's partition and none outside its days, while the weekly
  build's bounds lock the rest of their week; the text changes at 23:30 and
  01:30 UTC and at no other time; the stranded grant read probes only the
  grant's days. On the plain tables each read returns the same rows and scans
  the same indexes as with the floor alone, in both plan modes.
- **Measured** with weeks on 2026-11-17 (morning), postgres:18.7, 1M contracts,
  the plain tables against the legacy leaf and five weekly leaves (the layout
  between the swap and the cutover week), 64 pgbench clients sending every
  argument as a parameter, as the app does. Medians of three alternating
  rounds, under outside load, so compare within a row. "Fresh" plans every
  execution (`-M extended`); "cached" is the plan cache (`-M prepared`), which
  chose generic plans in every case. The days' measurement follows the table.

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
- **Measured with days** on 2026-11-17 (evening), postgres:18.7, 1M contracts
  spread over 30 days, in three layouts: the plain tables; the swap (the
  legacy leaf and the four day leaves C..C+3, C two weeks ahead), where the
  weeks were measured before; and 30 days after a cutover (the legacy leaf,
  30 past days, today and 3 ahead: 35 leaves, the steady state has about 40).
  Each read with the floor alone, with its days, and with the weeks the
  weekly build rendered. Plan time is the median of 41 `EXPLAIN ANALYZE` runs
  of each variant, interleaved (the plain tables in a run of their own), so
  outside load (load average 95–130 on the 10-core Mac) shifts every variant
  alike; locks are the relation locks of one execution that plans (custom
  and generic plans alike), with the fast-path ones in parentheses when
  they are fewer:

  | Read | Plain, floor | Swap, days | Swap, weeks | Swap, floor | Daily, days | Daily, weeks | Daily, floor |
  |---|---|---|---|---|---|---|---|
  | rehearsal pair read | 0.21 ms, 16 | 0.23 ms, 31 | 0.24 ms, 31 | 0.71 ms, 91 (64) | 0.28 ms, 31 | 0.70 ms, 91 (64) | 4.6 ms, 541 (64) |
  | companion origin exists | 0.42 ms, 19 | 0.47 ms, 34 | 0.47 ms, 34 | 1.2 ms, 94 (64) | 0.52 ms, 34 | 1.2 ms, 94 (64) | 9.7 ms, 544 (64) |
  | legacy companion origin exists (Main today) | 0.39 ms, 16 | 0.41 ms, 31 | 0.38 ms, 31 | 1.0 ms, 91 (64) | 0.42 ms, 31 | 2.0 ms, 91 (64) | 16 ms, 541 (64) |
  | open transfer escrows | 0.47 ms, 23 | 0.54 ms, 45 | 0.54 ms, 45 | 1.2 ms, 133 (64) | 0.69 ms, 45 | 1.9 ms, 133 (64) | 10 ms, 793 (64) |
  | removed client open contracts | 0.35 ms, 16 | 0.42 ms, 31 | 0.40 ms, 31 | 2.1 ms, 91 (64) | 0.34 ms, 31 | 2.2 ms, 91 (64) | 11 ms, 541 (64) |
  | plain origin selection | 2.05 ms, 30 | 1.44 ms, 45 | 1.47 ms, 45 | 4.5 ms, 105 (64) | 2.12 ms, 45 | 7.9 ms, 105 (64) | 52 ms, 555 (64) |
  | legacy plain origin selection (Main today) | 1.12 ms, 27 | 1.58 ms, 42 | 1.42 ms, 42 | 6.0 ms, 102 (64) | 2.34 ms, 42 | 8.2 ms, 102 (64) | 55 ms, 552 (64) |
  | chained origin selection | 0.86 ms, 30 | 1.79 ms, 45 | 0.99 ms, 45 | 4.2 ms, 105 (64) | 1.00 ms, 45 | 4.5 ms, 105 (64) | 23 ms, 555 (64) |
  | open transfer byte count, no bound | 0.13 ms, 16 | | | 0.53 ms, 91 (64) | | | 2.2 ms, 541 (64) |

  - With days the reads plan and lock one leaf in every layout: the legacy
    leaf on the swap, today's leaf after the cutover (two around midnight),
    all within the fast-path slots. Their execution times match the plain
    tables'; planning costs the parent and its pruning (measured below in one
    database: 0.06–0.3 ms more).
  - On the swap layout days and weeks plan the same leaf; nothing changes
    there with days.
  - After the cutover the weekly build's bounds plan and lock every existing
    day of their week (five leaves on this Tuesday, up to seven, 10–11 from
    Sunday 23:30 to Monday 01:30): 2.4–6.4 times the days' plan time, about
    twice their execution time, and 91–133 locks, past the 64 fast-path
    slots. The floor alone plans all 35 leaves: 541–793 locks and 14–39 times
    the days' plan time, which is what the day bounds save on every fresh
    plan.
  - The zero escrow reads and the legacy chained origin selection fall in the
    same ranges (logs below).
- **pgbench 30 days after a daily cutover, in one database.** The 35 leaves
  against plain copies of the same six tables' rows (schema `c1dplain`,
  reached by `search_path`), each read with its days and with the floor
  alone, 64 clients sending every argument as a parameter, medians of three
  rounds that alternate the four variants, so that outside load (load
  average about 100) shifts them alike. Fresh / cached tps, and the change
  against today's statement on the plain tables:

  | Read | Plain, floor (today) | Plain, days (ships to the plain tables) | Daily, days | Daily, floor alone |
  |---|---|---|---|---|
  | rehearsal pair read | 32k / 143k | 23k / 155k (−30% / +9%) | 23k / 151k (−28% / +6%) | 0.8k / 25k (−97% / −82%) |
  | companion origin exists | 11k / 98k | 13k / 102k (+14% / +4%) | 11k / 104k (0% / +6%) | 0.5k / 31k (−96% / −68%) |
  | legacy companion origin exists (Main today) | 18k / 128k | 16k / 118k (−10% / −8%) | 13k / 132k (−24% / +3%) | 0.6k / 31k (−97% / −76%) |
  | open transfer escrows | 14k / 126k | 11k / 107k (−19% / −14%) | 10k / 108k (−26% / −14%) | 0.3k / 13k (−98% / −90%) |
  | removed client open contracts | 22k / 144k | 18k / 142k (−16% / −1%) | 15k / 160k (−34% / +11%) | 0.5k / 36k (−98% / −75%) |
  | plain origin selection | 1.8k / 61k | 2.3k / 60k (+32% / −2%) | 1.6k / 65k (−7% / +7%) | 0.1k / 16k (−92% / −73%) |
  | legacy plain origin selection (Main today) | 5.7k / 85k | 5.4k / 82k (−5% / −3%) | 1.9k / 80k (−67% / −6%) | 0.2k / 17k (−97% / −80%) |
  | chained origin selection | 4.2k / 43k | 1.3k / 38k (−70% / −12%) | 0.4k / 41k (−90% / −5%) | 0.1k / 14k (−98% / −67%) |
  | open transfer byte count, no bound | 46k / 165k | | 0.9k / 16k (−98% / −90%) | |

  - From the cached plan, the steady state, the daily layout with days runs
    as the plain tables do: −14% to +11% against today's statement, and
    within 12% of the same statement on the plain tables. With weeks on the
    swap layout the morning run measured −11% to +24%.
  - Fresh plans cost the parent and its pruning: 0.06–0.3 ms more a plan
    with one client (0.29–1.57 ms against 0.23–1.34 ms with the same
    statement on the plain tables). Under 64 clients that took 0–34% off the
    fresh rate of most reads and about two thirds off the legacy plain and
    the chained origin selections, whose fresh rounds varied threefold
    (logs). PostgreSQL plans fresh for a statement's first five executions on
    a connection and after an invalidation; the days add one such change of
    the text at 23:30 and one at 01:30 UTC.
  - On the plain tables the days cost about what the weeks did, from the
    cached plan −14% to +9% against the floor alone (the weeks: −9% to +4%);
    fresh rounds are too noisy here to rank them.
  - The floor alone after a daily cutover plans all 35 leaves: 541–793
    locks, 3.5–28 ms to plan, −92% to −98% fresh and −67% to −90% cached. That
    is what the days save, and why a build without them must not serve the
    daily layout (RUNBOOK §5.0, precondition 2).
- **The read without a bound, by number of leaves.** `GetOpenTransferByteCount`
  (/subscription/balance) over the same 1M contracts in one database: a plain
  copy, a copy in 12 leaves (about the weekly layout's count at its steady
  state) and `transfer_contract` in 42 (about the daily one's), pgbench
  rounds alternating, 64 clients; then again with `max_locks_per_transaction`
  1024, which PostgreSQL 18 turns into 1024 fast-path slots:

  | | Plain | 12 leaves | 42 leaves |
  |---|---|---|---|
  | plan / execution, one client | 0.12 / 0.02 ms | 0.71 / 0.08 ms | 2.18 / 0.21 ms |
  | relation locks (fast-path), 64 slots | 16 (16) | 196 (64) | 646 (64) |
  | pgbench fresh / cached, 64 slots | 70k / 248k tps | 6.7k / 68k | 1.4k / 14k |
  | pgbench fresh / cached, 1024 slots (all locks fast-path) | 39k / 141k | 4.6k / 52k | 0.14k / 13k |

  - From the cached plan, 42 leaves run the read at about a fifth of its
    rate at 12 leaves, and at 6% of the plain table's; fresh, at a fifth
    and 2%.
  - The fast-path slots are not what limits it: with every lock in the fast
    path (the 1024-slot run, under heavier outside load, its plain variant
    down 44%), the 42 leaves stayed at a quarter of the 12 leaves' cached
    rate and fell further fresh (82–239 tps in its rounds). The cost is the
    planning and execution of one index probe per leaf. Raising
    `max_locks_per_transaction` does not buy it back; a bound on the read
    does.
- **The weekly run's method this evening.** Plain, then the swap layout,
  then the daily one, one after another, as on the morning of 11-17: under
  this evening's load the layouts drifted apart by more than the effects
  measured (the same statement on the plain tables ran 15k and 20k fresh in
  alternating rounds), so its cross-layout numbers are not used. Within the
  swap layout its alternating rounds agree with the morning's: the days ran
  0.8–4.5 times the floor's fresh rate and 1.4–3.2 times its cached rate,
  the weeks there 2.6–3.8 and 2.0–3.0 times.
  - Logs: `temp/c1daily-1117/logs/` (`plantime-*`, `ab-*`, `bench-*`).
- Not changed: `contractHoleMembersSql`, `hasOpenContractForPairSQL`,
  `GetOpenTransferByteCount` and the `GetOpenContractIds` reads have no
  contract id bound at all, and the first two admit contracts with no
  deadline, so a bound would change their rows. They probe one partial index
  per partition, as accepted in b23fb286b; with daily partitions that is about
  40 partitions at the steady state, not about 11 (section "Daily
  partitions").

### Retention as built

*Superseded by S1 (2026-11-17), below.* This was the 2026-10-23 build: the
drop at 91 days, its blockers and its archive step. S1 removed the blockers
and the archive step, which deleted contracts the contract horizon reaper
keeps.

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
autovacuum on the large tables, the disk headroom the cutover day needs, daily
partitions) is `temp/contract-cutover-1117/RUNBOOK.md` in the workspace. It
supersedes the steps below where they differ: prepare is rerun until it
reports "prepared", finalize runs with the taskworker stopped through the
settlement gate, and the cutover day is chosen for disk headroom.

1. Deploy a build with this code and apply migrations 856-857. Also deploy
   the transaction fixes.
2. `bringyourctl model migrate contract-partition --cutover-day=<a day at least 2 days ahead> --dry-run`
   prints every statement and checks for invalid indexes.
3. Run the same command without `--dry-run`, off-peak, to prepare. It is
   resumable.
4. Complete `--finalize --dry-run` and then `--finalize` before the cutover
   day minus 6 hours, allowing time for verification. Maintenance abandons an
   unfinished prepare at that boundary. The command's 10-minute final refusal
   margin is an additional safeguard, not the operating deadline. Finalize
   probes, swaps, analyzes the parents and validates the outside foreign key.
5. Watch contract creation, the runway probe and the task's `[cp]` log lines.
   Until the cutover day starts, `--rollback` undoes the swap.
   - Rollback restores `contract_close_report_contract_id_fkey` NOT VALID.
     The monitor's migration-coherence check pages until it is validated off
     peak: `ALTER TABLE contract_close_report VALIDATE CONSTRAINT contract_close_report_contract_id_fkey`.
   - Once partitioned, the monitor requires that key to stay absent: a key
     restored by hand, or by `db audit --fix`, would block each day's drop.

## Daily partitions (owner decision 2026-11-17 ~15:35Z)

Status: **BUILT, NOT CUT OVER**, on branch `feat/contract-daily-partitions-1117`
(7aa9f9c9d, the code and tests; the documents in the commit after it), merged
to server main on 2026-11-17. No migration. Merged code ships with the
nightly; on today's plain tables every change is a no-op (the cutover, the
task's partition steps) or plan-identical (the hot reads' bounds, tested
below). The owner chose one partition per UTC day for the six contract
tables, in place of the weeks of 2026-10-23, after S1's disk section showed
that weekly leaves hold up to 7 days past the horizon (1.0–1.3 TiB at
140–180 GiB a day). C1's earliest date stays January (disk), so the rework
was done whole: the cutover, the maintenance, the hot reads' bounds, S1, the
monitor and the runbook.

### What changed

- **Partitions:** `ContractPartitionGranularity` is the registry's daily
  granularity: `[contract_id_floor(D), contract_id_floor(D + 1 day))` for each
  UTC day D, named `<table>_pYYYYMMDD` as the registry names a daily range
  (`ContractPartitionName`, `ParseContractPartitionName`,
  `ContractPartitionDayStart`). The staged parent stays `<table>_p`; no name
  collides with it, its `_p` index suffix or the legacy `_l` one.
- **Days ahead: 3** (`ContractPartitionAheadDays`; 4 weeks before). The
  monitor pages below 2 future days (§2.4h), so a stalled task leaves a day or
  two before contract creation fails. Each partition ahead costs every read
  without a contract id bound one more leaf to plan and lock, 15 locks on
  `transfer_contract` (its leaf and 14 indexes on Main), so each day more
  ahead would buy a day more to react at that price.
- **Cutover:** `--cutover-day=<YYYY-MM-DD>` (any day, 00:00 UTC, at least 24
  hours ahead; `--cutover-week` was a Monday). Prepare stages the days
  C..C+3. Every refusal and the 11-17 lock admission are unchanged: the
  bound's add and validation defer a busy table, the swap waits at most 1 s
  for its locks, rollback refuses once a daily partition holds a row, a
  prepare unfinalized 6 hours before C is abandoned, finalize refuses within
  10 minutes of C, and the attach probe proves every index is adopted.
- **The hot reads' bounds (B1, B3):** day literals instead of week literals
  (section "The hot reads' partition days"). The text changes twice a day,
  at 23:30 and 01:30 UTC, so each connection plans each statement afresh
  twice a day; from 01:30 to 23:30 a read plans and locks only the current
  day's partition. Bounding to two days always, to change the text once a
  day, would double the partitions every read plans for 22 hours of the day,
  for a saving of one re-plan per connection a day, so the window is kept to
  the days that hold it.
- **Retirement (S1):** a day expires 6 hours past the horizon (a week
  expired a day past it; section "S1"). The compaction stays: at day size
  nearly every day still holds a few kept rows when it expires, and Main
  keeps the files of an emptied leaf (`vacuum_truncate` off).
- **The maintenance task** analyzes the parents once a day (it was once a
  week), each `ANALYZE ONLY` sampling across the leaves (the runbook expected
  1–3 minutes of reads for the six), and each closed day's leaves once, a day
  after the day.
- **Monitor:** §2.4h counts covered days (pages below 2), §2.4i judges days
  (91 + 14 days, unchanged), §2.4j names the cutover day; the db maintenance
  log parser takes `_pYYYYMMDD` contract leaves.
- **The weekly families stay weekly:** close evidence (C5), the usage archive
  and the work-session journal keep their weeks and their own 4-week runways.

### What daily costs

- **Reads without a contract id bound** (`GetOpenTransferByteCount` for
  /subscription/balance, `GetOpenContractIds`, `contractHoleMembersSql`,
  `hasOpenContractForPairSQL`, sweeps by payment, provider stats) plan and
  lock every partition. At the steady state that is the legacy partition,
  36–37 past days and 3 ahead, about 41 leaves, where weeks had about 11: on
  `transfer_contract` alone about 630 relation locks for an execution that
  plans (16 plus 15 a leaf; 541 measured at 35 leaves), against about 180
  with weeks, far beyond the 64 fast-path slots Main has
  (`max_locks_per_transaction` 64 on 11-17; PostgreSQL 18 sizes the
  fast-path slots from it). Measured below. Not new with daily, only larger:
  the reads that the floor and the days cannot bound without changing their
  rows were accepted in b23fb286b.
- **The drop's parent lock once a day:** a day's drop detaches six leaves
  under the parents' ACCESS EXCLUSIVE, catalog work of about 0.2 s, at most a
  1 s wait and 0.5 s of work, now daily rather than weekly.
- **Vacuum of the reaper's deletes is smaller:** the reaper's deletes trigger
  autovacuum on the leaf they hit (5M dead rows on `transfer_contract`'s
  leaves), and each pass reads that leaf's indexes whole: a day's, not a
  week's.

**Measured** (section "The hot reads' partition days"; postgres:18.7, 1M
contracts, 64 clients, in one database against plain copies of the same
rows): the hot reads with their days run on the daily layout as on the
plain tables from the cached plan (−14% to +11%), and each fresh plan costs
0.06–0.3 ms more. The read without a bound is the cost:
`GetOpenTransferByteCount` at 42 leaves runs at about a fifth of its rate
at 12, the weekly count (14k against 68k tps cached, 1.4k against 6.7k
fresh; a plain copy 248k and 70k), and 1024 fast-path lock slots did not
bring it back. At its 11-05 rate (21 calls a second, CPUOPT5) that is at
most about 1.5% of a core from the cached plan; reads without a bound that
run more often would cost in proportion. Decision 5 under S1.

### The flake of `TestContractWeekRetirementKeepsWhatTheReaperKeeps`

On the local test environment (shared by every package's tests, each in its
own database) the test failed now and then with "transfer_contract_w20261123
stayed once empty": after the reaper's deletes of the kept contracts, one
maintenance run did not drop the emptied weeks.
- **Root cause:** CLUSTER decides which deleted rows it must copy against the
  oldest transaction of its database's backends, its own included, and its own
  snapshot's xmin is the oldest transaction id running anywhere on the server,
  in any database (PostgreSQL's snapshots do not filter by database). The
  test's wait read only its own database's backends. A transaction of another
  test database, open across the deletes, made CLUSTER copy them as recently
  dead; the leaves kept their bytes, the run reported the week kept with 0
  rows, and the week stayed. Reproduced on 18.7: a transaction holding an id
  in another database, a delete in the test's, the old wait returns in 26 ms,
  CLUSTER leaves 147 kB (all the deleted rows), and 0 once that transaction
  ends.
- **Fix:** `server.Testing_WaitForSnapshotHorizon` waits until every
  transaction id assigned before the call has ended in every database
  (`pg_snapshot_xmin(pg_current_snapshot())`, prepared transactions included)
  and no backend of this database holds an older snapshot, and names what it
  waits for. Every S1 test waits with it before it expects a drop. The
  retirement itself was right (a later run drops the day), but it now reports
  such a day as waiting for those rows, not as rows the reaper keeps.
- **Deterministic:** `TestContractDayCompactionSeesTransactionsOfOtherDatabases`
  holds a transaction in another database across the reaper's delete: the old
  wait returns, the new one names the other database's transaction, the run
  reports the day waiting and keeps it, and once that transaction ends the
  next run drops it. On the weekly build (ddc043eea) a scratch variant fails
  ("kept 0 rows", the week stays), and the unmodified weekly model test fails
  every time with "…_w20261123 stayed once empty" while a transaction of the
  container's `postgres` database is held open; the daily model test, with the
  same holder, waits for it and passes.

### Tests

Fail before, pass after (scratch variants on the weekly build ddc043eea, the
branch point, on postgres:18.7):
- the day's retirement at its horizon: a day the reaper emptied (a Tuesday)
  is still there at its end + 35 days + 6 hours on the weekly build; it drops
  then on the daily one (`TestContractDayRetiresAtTheReaperHorizon`);
- daily pruning, custom and generic plans: each hot read plans the current
  day's partition and none outside its days, and the weekly build's bounds on
  the same layout lock the rest of their week
  (`model.TestUnexpiredContractReadsPlanExactlyTheirDaysAfterTheCutover`);
  the floor alone locks every partition
  (`model.TestUnexpiredContractReadsPlanAndLockOnlyTheirDaysWhenPartitioned`);
  the statement text changes at 23:30 and 01:30 UTC only
  (`model.TestUnexpiredContractDaysRender`);
- the flake (above).

Kept for days: the keep rules (`model.TestContractDayRetirementKeepsWhatTheReaperKeeps`,
payment holds), no queueing behind an autovacuum or a long leaf lock, the
bounds on the work under a lock, retirement without vacuum, the no-op on plain
tables, the cutover lifecycle with days (staging C..C+3 exactly, routing by
day, the last millisecond of a day, the refusal past the last partition,
rollback refusing a daily row), the maintenance pass 140 days after the
cutover, the rollback and prepare admission tests, the plain-table
equivalence and plan tests of the hot reads, the B3 tests, the monitor's
catalog and native tests, and the CLI's `--cutover-day`.

Runs on 2026-11-17, all passing: on a throwaway postgres:18.7, server 88,
model 91, monitor 10 native and 23 synthetic, taskworker 4 (the first server
run failed one test, whose input still named `analyzed_week`; fixed and
rerun); on the local test environment (10.213.0.1, PostgreSQL 18.6, shared
with other packages' tests), server 88, model 91 and monitor 10; and the
merge set's pure part on the merged tree: `go test ./grafana/`,
`TestEveryAlertClassIsClassified`, `TestBuildsForDarwin`, the CLI parses and
the pure unit tests. Logs: `temp/c1daily-1117/logs/`.

## S1: days retire at the reaper's horizon (2026-11-17)

Status: **BUILT** (owner go 2026-11-17 ~08:30Z) for weeks, on branch
`feat/contract-week-reaper-horizon-1117`:
- code: 2d55bc9f3, 6bbba8761 (the live-row gate) and 2b5b72df2 (the bound);
- tests: de9552b8e and e61aa3eea.

**Reworked for days the same day** (owner decision ~15:35Z; section "Daily
partitions"): a day retires 6 hours after its upper bound passes the horizon,
with the same steps, admission and bounds. Where this section says day, the
weekly build had a week.

No migration; it ships with the taskworker. It supersedes the 91-day drop and
the archive step of "Retention as built" above. It is a prerequisite for the
cutover day C (RUNBOOK gates C1 and C2), but it does not make C feasible on
edge-2's current volume (below).

### The problem

- The weeks dropped only at `ContractRetention`, 91 days. From C, new
  partitions grow 140–180 GiB a day (1,439 contracts a second), so 91 days is
  12–17 TiB of them, more than edge-2's 13.75 TiB volume.
- The 91-day drop deleted, through its archive step, every contract its own
  narrower checks admitted (resolved, escrows settled, no pending payment).
  That loses contracts the contract horizon reaper keeps on purpose: paid
  under attribution review, paid by a payment with no row, paid within the
  discharge time, closed inside the horizon, and unpaid sweeps an operator
  block award can still read.

### Design

**The reaper is the only judge.** `model/contract_horizon_reaper.go` deletes a
contract with its closes, escrows, sweeps, extender parties and close reports
in one transaction, under its keep rules, and the usage archive trigger copies
the proof in the same statement. Retirement deletes no row and evaluates no
keep rule. It only returns the storage of rows the reaper has already
deleted. That makes "keep exactly what the reaper keeps" true by construction,
for every rule the reaper has today and any it gains later.

**Expiry** (`ContractDayRetention`, `contractDayRangeExpired`): a day
expires 6 hours (`RetireDelay`) after its upper bound passes the reaper's
horizon (a day in the weekly build). The horizon comes from the reaper's
control once per run, the same way close evidence reads it
(`model.WithContractHorizonControl`):
- while the mode is on, the control's `horizonDays`, between 35 and 91 days;
- with the mode off, in a dry run, or with the control unread, 91 days. The
  reaper then deletes nothing, so the day stays.

The 6 hours cover the reaper's 2-hour close settle time and the hours its
weekly rescan holds back its frontier (about 3 hours at 1,900 contracts a
second). A day examined before the reaper has emptied it waits, reported,
without a lock (step 1). The legacy partition expires the same way from the
cutover day.

**Per expired day**, oldest first, each run
(`db_contract_day_retirement.go`):
1. **Wait for the reaper.** A leaf is examined only once the statistics count
   at most 1M live rows in it (`n_live_tup`, which follows the reaper's
   deletes as they commit), or it is at most 256 MiB on disk. Vacuum is not
   required:
   - Main's leaves inherit vacuum thresholds of 5M to 25M dead rows, and two
     tables keep the default 10% analyze threshold.
   - So a day's last deleted rows can stay unvacuumed, and its row estimate
     stale, for good.
   - A first build gated on `n_dead_tup` and `reltuples`, which would have kept
     such a range forever (`TestContractDayRetiresWithoutVacuum`).
2. **Count its live rows** along its key, up to 10,000 (`MaxCompactRows`), in
   a read-only statement of at most 5 s. Above the bound the leaf waits,
   reported.
   - Like any index scan, the walk marks the key entries of rows every snapshot
     sees deleted as dead. Unvacuumed deletes are therefore passed over a little
     further each run, until the count finishes within its 5 s; CLUSTER then
     skips them.
   - It also brings the kept rows into the cache.
3. **Compact** each leaf that holds more than 256 MiB, or no live row:
   `CLUSTER` along its primary key, in its own transaction.
   - It takes that leaf's ACCESS EXCLUSIVE lock first, waiting at most 1 s, and
     gives the rewrite 500 ms.
   - Every planner cost is set to zero, so CLUSTER always scans the key and
     never the whole heap. PostgreSQL 18 counts disabled plans apart from their
     costs, so `enable_*` settings cannot force this, and physical correlation
     would otherwise choose a sequential scan of the full day.
   - CLUSTER copies every live and recently dead row version with its
     transaction ids. It is MVCC-safe; a copy or TRUNCATE is not. Every
     snapshot, including the epoch usage reader's long REPEATABLE READ one,
     sees the same rows afterwards.
   - "Recently dead" is judged against CLUSTER's own snapshot too, whose xmin
     is the oldest transaction running anywhere on the server, in any
     database. While one that began before the reaper's last deletes runs,
     CLUSTER copies those rows, and the day stays until a later run: reported
     "its leaves hold no live row, only rows deleted after the oldest open
     transaction (of any database) began", not as kept rows (the root cause
     of the S1 test's flake, below).
   - The old files are unlinked after the commit releases the lock.
   - Then `SET WITHOUT CLUSTER`, so the leaf's catalog matches the others.
4. **Drop** once the day's six leaves hold no tuple at all, live or dead, in
   any heap or TOAST table: detach and drop all six in one transaction,
   children first. It locks the six parents `ONLY` and the day's leaves, waits
   at most 1 s, gives the work 500 ms, and reads the leaves again under its
   locks. Nothing any snapshot could see goes with them.

A day whose rows the reaper keeps stays compacted, a few small leaves,
until the reaper's weekly rescans delete those rows. The next run then
empties the leaves and drops the day.

**Why the compaction stays at day size.** A simpler rule, drop a day once it
is empty, would not return the space:
- the reaper keeps about one contract in 900,000 at its horizon, so about 140
  of a day's 124M at 1,439 a second, and some keep reasons last weeks or, for
  the reviewed USDC-era contracts, for good. Nearly every day expires holding
  a few kept rows, and one would hold the whole day's 140–180 GiB;
- Main runs with `vacuum_truncate` off, so even a day the reaper has emptied
  keeps its files after vacuum; only a rewrite or the drop returns them;
- a drop is MVCC-safe only when no snapshot can see a row of the day.
  CLUSTER's empty result proves that with one read of the leaf's size, where
  a scan for dead tuples of a day's 60 GiB heap would not fit under a lock.

At day size each compaction moves about a seventh of a week's kept rows, so
it holds its leaf's lock for less.

**Locks (the 11-17 rules).** Every step reads `contractLockAdmission` first.
It never queues behind an autovacuum, a prepared transaction, or a transaction
older than 5 s holding or awaiting a conflicting lock. A busy leaf or day is
left for the next run, reported. The scopes are exact:
- CLUSTER: the leaf and its TOAST table, found through `reltoastrelid`;
- the drop: the six parents `ONLY`, plus the day's leaves and their TOAST.

Neither waits for the vacuums of other leaves, such as the legacy one.
- Every exclusive wait is at most 1 s. The wait plus the work stays under the
  application's 2 s `PgLockTimeout`.
- The family's own drop lock timeout goes from 5 s to 1 s.
- **Found by S1's tests:** the task's closed-leaf and parent `ANALYZE` had no
  admission and no lock timeout. It queued behind a vacuum on a leaf, which
  would cancel a routine autovacuum after `deadlock_timeout`, or wait out an
  anti-wraparound one with the whole task stalled behind it. It now uses the
  same admission (registry `AnalyzeAdmission`) and a 1 s lock timeout.

**Who waits for a compaction.** Only the statements that lock that day's
leaf:
- unbounded reads that probe every leaf (sweeps by payment, provider stats);
- generic plans;
- before B1, the hot reads, whose `statement_timestamp()` bound locks every
  leaf (RUNBOOK §4.3).

They wait for the lock wait plus the rewrite, under the application's 2 s
bound. B1 is a precondition of the swap, so after it the hot reads prune to
the current day at plan time and never meet an old day's lock. The drop locks
the parents for its catalog work, as the swap does: with days, once a day
rather than once a week, each time for its catalog work (about 0.2 s in the
rehearsal below), a wait of at most 1 s and work of at most 0.5 s.

**Work under a lock is bounded.** CLUSTER handles at most the rows the reaper
keeps (10,000 per leaf); the drop is catalog work. On Main, the reaper kept
about 1 row in 900,000 of early October's contracts at the horizon (4
unsettled and 4 recent_close an hour out of 7.2M read), about 1,000 a week at
1,439 contracts a second.

**Removed:** the archive step (`contractArchiveBatchSql`), the old blockers
(`contractWeekBlocker`), and the cursor's `archive_partition` and
`archive_cursor`, which an old cursor still carries and which are now ignored.

**Rehearsed on PostgreSQL 18.7** with weeks (a throwaway container): two
identical weeks of 500,000 contracts each, reaped down to 1,000 or 20,000 kept
and vacuumed. Eight sessions ran statements that lock every leaf, each with
the 2 s lock bound. A day of Main holds about a seventh of a week's rows, and
of its kept rows.

| | 1,000 kept | 20,000 kept |
|---|---|---|
| count, per leaf | 9–33 ms | 9–214 ms |
| CLUSTER, per leaf (`pg_stat_progress_cluster`: "index scanning heap") | 18–57 ms | 19–174 ms |
| transfer_contract leaf | 136 MB → 272 kB heap | 136 MB → 5.4 MB heap |
| other statements during it (p99 / max / errors) | 24 / 29 ms / 0 | 54 / 117 ms / 0 |
| the empty week's compaction and drop | 204 ms, statements max 16 ms | 232 ms, max 24 ms |
| CLUSTER of the other week's identical leaf at the default costs | "seq scanning heap": 120 ms here, a full read of a ~400 GiB leaf on Main | the same |

Logs: `temp/contract-cutover-1117/s1/logs/rehearsal-500k-*`.

### Each keep rule survives a retirement

| Reaper keeps (reason) | Why it survives S1 | Covered by |
|---|---|---|
| `open`, `unresolved` (no outcome, dispute) | not deleted by the reaper, so its rows are live; CLUSTER copies every live row; the drop needs zero tuples | TestContractDayRetirementKeepsWhatTheReaperKeeps (day B) |
| `recent_close` | same | same (day A) |
| `unsettled` escrow | same; the escrow, its contract and every dependent stay together | same (day B) |
| `payment_hold` (pending, or an external attempt) | same | same (day B); TestContractPartitionRetirementPreservesPaymentHolds |
| `payment_incomplete` (incl. a payment row that does not exist) | same | same (day A) |
| `payment_recent` (within `CompletedContractExpiration`) | same | same (day A) |
| `block_points` (unpaid sweep in a block still to be awarded) | same | same (day A) |
| `payment_review` (attribution review, never cleared) | same | same (day A) |
| `busy` (another transaction holds it) | the reaper retries it; the leaf's lock or row locks make S1 wait or skip it, never take it | TestContractDayRetirementDoesNotQueueBehindALeafLock |
| a deleted row an older snapshot still sees | CLUSTER keeps recently dead versions with their xmin/xmax | TestContractDayCompactionKeepsEveryRowVersion; TestContractDayCompactionSeesTransactionsOfOtherDatabases (a transaction of another database) |

On ab2d3ed87 (before S1) the same test, then weekly, lost every week-A
contract: the reviewed, missing-payment, recently paid, recently closed and
block-points ones were archived, deleted and dropped with their week.

### Related tables

- **The six C1 tables** have the same daily bounds. A compaction keeps every
  row of its leaf. Every pair (contract with escrow, close, sweep, report,
  extender) therefore stays exactly as the reaper left it, which deletes a
  contract and all its dependents in one transaction. The drop removes all six
  leaves of a day together, and only when every one is empty.
- **`contract_close_report_evidence` (C5)** keeps its own rule, unchanged:
  35 days from acceptance for a resolved or deleted contract (owner,
  2026-11-01), not the reaper's keep rules. S1 never makes a live contract
  "gone", so the evidence retention sees exactly the contract states it saw
  before.
- **`st_provider_usage_archive`** is written only by the reaper's deletes.
  S1 never drops a live contract, so it never needs the capture trigger and
  archives nothing.
- **`account_payment`, `payment_report` (block awards)**: read by the reaper,
  untouched by S1.

### With the reaper, before and after C

- **Plain tables (today):** retirement is a no-op (`IsContractPartitioned`
  false). The reaper works as it does now.
- **Between the swap and C:** the legacy partition's bound is C, and the
  daily partitions are empty and in the future. Nothing expires before
  C + 35 days and 6 hours.
- **From C:** the reaper deletes each contract 35 days and 2 hours after
  creation, through the parent, pruned per leaf, exactly as before. Six hours
  after the day's upper bound passes the horizon, S1 compacts and drops it,
  whether or not vacuum has visited it since.
- **No double work.** S1 never classifies or deletes, and its reads are
  bounded by the kept rows. The reaper's rescans reread a compacted day's
  few kept rows, as they reread kept rows today.
- **No gap.** At no point is a row the reaper keeps outside a live,
  attached partition or invisible to a snapshot.
- **Reaper off or behind:** the day waits ("the reaper has not emptied
  it"), reported each run. With the mode off it waits from 91 days.
- **The zero-byte pass** (owner, 2026-11-21; `model/contract_horizon_zero_byte.go`)
  deletes zero-byte contracts 8 days and 2 hours after creation, by the
  same statements. On the plain tables the space it frees is reused by new
  rows. From C it frees nothing on disk: a day's leaves take no new rows
  after the day and keep their files (`vacuum_truncate` off), so the space
  returns only when S1 compacts the day at the horizon. Turn it off at the
  cutover (control `zeroByteMode` off). Expiry is unchanged: it reads the
  horizon, never the zero-byte horizon.

### The legacy partition

At C + 35 days and 6 hours the legacy partition expires. It then holds every pre-C
contract the reaper keeps. On 11-17 its pass had kept:
- about 20.7M under attribution review, which no code clears
  (`attribution_review_required` is never reset);
- 19.4M paid recently (these go within a week);
- about 57k with an unsettled escrow.

That is far over the 10,000-row bound, so the legacy partition stays, reported.
Its indexes can be rebuilt once the reaper has emptied it (C3: about 53% of
its size, `db reindex-index` on the `_l` names). Its heap, about 2.4 TiB
today, stays until those rows go. **That needs an owner decision** (see
"Decisions" below).

**Decided 2026-11-17 ~15:30Z: (d), export, then clear the review.** Built
2026-11-21 as `usagearchivectl review` (server `usagearchive/review*.go`),
run from the backup host like the usage archive's weekly export:
- `review export` copies every row of the six tables the reaper deletes with a
  contract, for each contract a sweep of the named completed payments under
  review paid, to the archive pool's encrypted dataset, in the week export's
  format (one COPY per table and batch, zstd, a manifest with row counts, row
  digests and SHA-256s, each copy read back before its manifest). Batches
  follow the sweep index `(payment_id, contract_id)` in contract id order,
  each one short REPEATABLE READ READ ONLY transaction; a stopped run resumes
  after its last manifest.
- `review verify` reads every copy back and counts each batch's contracts on
  Main again.
- `review clear` refuses unless that verification passed and the coverage
  still holds, and each payment is the row the export recorded. It writes the
  rows as they were first, then clears the flag in one transaction guarded by
  each row's values, journaled as `attribution_review_cleared`;
  `review restore-flag` reverses it.

The reaper then deletes those contracts at its next pass over them, archiving
their usage as any deleted contract's. Their mint weeks' usage archive leaves
were already exported and dropped, so those archive rows land in the archive's
DEFAULT partition.

### Disk

Recomputed for days on 2026-11-17 by
`temp/c1daily-1117/scripts/disk_model.py` (output `logs/disk-model.txt`): the
S1 model (`temp/contract-cutover-1117/s1/scripts/disk_model.py`) stepped an
hour at a time, so the 6-hour delay shows, with the partition span a
parameter, weekly and daily computed alike. Each hour after C consumes that
hour's inflow, because the frozen legacy partition's freed space is never
reused. The hourly steps count the 38th day's inflow before the legacy rebuild
starts, so its weekly figures are about 0.15 TiB above the S1 model's day steps.

**Held at steady state.** A day drops 35 days and 6 hours after it ends, so
35.25 to 36.25 days of inflow are on disk; with two days of lag (the reaper or
a busy leaf), 37.25 to 38.25. A week dropped 36 days after it ended: 36 to 43
days, or 38 to 45.

| GiB/day | Daily (35.25 d) | Daily, 2 d lag | Weekly (36 d) | Weekly, 2 d lag | Before S1 (91 d) |
|---|---|---|---|---|---|
| 140 | 4.82–4.96 TiB | 5.09–5.23 TiB | 4.92–5.88 TiB | 5.20–6.15 TiB | 12.4–13.4 TiB |
| 180 | 6.20–6.37 TiB | 6.55–6.72 TiB | 6.33–7.56 TiB | 6.68–7.91 TiB | 16.0–17.2 TiB |

Daily holds 0.92–1.19 TiB less at the top of the cycle, 0.51–0.66 TiB less on
average, and 0.10–0.13 TiB less at its bottom. This is about the plain tables'
steady state at the same load: 35 days, 4.8–6.2 TiB compact, plus their index
bloat. With S1 there is no bloat and no rebuild cycle.

**Free space needed at C**, at the peak between C and C+45. Each case
includes a 500 GiB floor and +10 GiB a day from the other families; the legacy
partition is 4,979 GiB at C (11-17):

| Legacy partition after C | Daily, 140 GiB/day | Daily, 180 | Weekly, 140 | Weekly, 180 | Peak, daily |
|---|---|---|---|---|---|
| stays whole | 5.87 TiB | 7.29 TiB | 6.78 TiB | 8.46 TiB | C+44 |
| its indexes rebuilt once the reaper has emptied it (C+38..45), or the whole leaf compacted or dropped at C+38 by an owner decision | 5.80 TiB | 7.22 TiB | 6.05 TiB | 7.54 TiB | C+37 |
| its indexes rebuilt each week as the reaper empties it | 3.55 TiB | 4.92 TiB | 4.20 TiB | 5.88 TiB | C+35 |
| before S1 (weekly, 91 d) | | | 15.0 TiB | 18.9 TiB | C+119 |

With two days of lag the daily figures are 6.14/7.64, 6.05/7.54 and
3.57/5.06 TiB, still at or below the weekly ones without lag. With the legacy
partition left whole, daily saves 0.9–1.2 TiB at C; with the RUNBOOK's plan,
the legacy indexes rebuilt from C+38, the saving is 0.25–0.33 TiB, because
the peak then comes before the rebuild, while the first days are still
filling.

**edge-2 had 5.4 TiB free** (5,467 GiB, 11-17 ~09:40Z), with no trim and no
new disk. The plain tables are still refilling to 35 days at the higher rate.
On 11-17 they held 2.28B contracts, against about 4.35B at 35 days and 1,439
a second. That is about 1.8–2.3 TiB more by about 12-22, net of the free space
the backlog deletes left, before the other families *(inferred)*.

So C needs about 3.6–7.3 TiB free on the day, and 5.8–7.2 TiB with the plan
in the RUNBOOK (the legacy indexes rebuilt once emptied). The current volume
cannot provide that unless one of these comes first:
- the RAID-1 tablespace (DBDISK §5.2, about +6.8 TiB);
- a trim of 35% or more together with weekly legacy index rebuilds; each
  round rereads the legacy heap once per index.

**Implied C:** a day at least a week after the tablespace is online and the
swap's preconditions hold (B1–B3); with days, C need not be a Monday. With
drives in 1–3 weeks, that is early January 2027 or later, not 12-21. S1 turns
the cutover from impossible (15–19 TiB) into one capacity step.

### Decisions for the owner

1. **The legacy partition's kept rows.** About 20.7M USDC-era contracts under
   attribution review are kept by the reaper for good. Its options:
   - (a) keep them: the legacy heap (about 2.4 TiB) stays, and its indexes are
     rebuilt once emptied;
   - (b) close the reviews, so the reaper deletes the contracts and S1 drops
     the legacy partition (about −5 TiB);
   - (c) compact the legacy partition in a window: CLUSTER of about 21M kept
     contracts holds its lock for an hour or more, with the taskworker stopped;
   - (d) export the reviewed contracts to edge-6 and let the reaper delete
     them, a new keep-rule exception.

   **Decided 2026-11-17: (d)**, through the export and the guarded clear of
   the reviews' flag rather than a keep-rule exception, so the reaper's rules
   stay as they are ("The legacy partition" above). Capacity is decided after
   the export, with the disk model run again.
2. **Capacity for C:** the RAID-1 tablespace, or the trim with weekly legacy
   index rebuilds (above).
3. **The monitor's §2.4i** still judges a day at 91 + 14 days, since the
   monitor does not read the reaper's horizon. A 35-day line needs the reaper's
   horizon in the monitor (the `urnetwork_contract_horizon_horizon_seconds`
   gauge). Until then, the days-to-full alert (DBDISK §5.6) is the earlier
   signal.
4. **Granularity.** Weekly leaves hold up to 7 days past the horizon, about
   1.0–1.3 TiB at 140–180 GiB a day. **Decided 2026-11-17 ~15:35Z: daily**
   (section "Daily partitions"); a day holds at most a day and 6 hours past
   the horizon.
5. **Reads without a contract id bound, with days** (section "What daily
   costs"): each plans and probes every partition, about 41 at the steady
   state. Measured, `GetOpenTransferByteCount` runs at about a fifth of its
   rate at the weekly count, and raising `max_locks_per_transaction` (1024
   fast-path slots) did not bring it back. Before C: bound
   `GetOpenTransferByteCount` and `GetOpenContractIds` by the floor, so
   that they stop counting contracts past their deadline that the
   force-close sweep has not closed yet, or accept the cost at their rates
   (21 calls a second for the first on 11-05, at most about 1.5% of a
   core).
6. **Two constants of the rework**, chosen here: 3 days ahead
   (`ContractPartitionAheadDays`) and 6 hours past the horizon
   (`ContractDayRetirementSettings.RetireDelay`).

### Tests

PostgreSQL 18.7 (throwaway container) and the local test environment (18.6,
launcher 10.213.0.1), as reworked for days on 2026-11-17 (the weekly names
before: `TestContractWeek...`):
- `TestContractDayRetentionFollowsTheReaperHorizon`
- `TestContractDayRetirementLockBoundsStayBelowTheApplicationBound`
- `TestContractDayRetiresAtTheReaperHorizon`
- `TestContractDayCompactionKeepsEveryRowVersion`
- `TestContractDayCompactionSeesTransactionsOfOtherDatabases` (new, the
  flake's root cause)
- `TestContractDayRetirementDoesNotQueueBehindALeafLock`
- `TestContractDayRetirementWaitsOutAnAutovacuum`
- `TestContractDayRetirementBoundsItsWork`
- `TestContractDayRetiresWithoutVacuum`
- `TestContractDayRetirementIsANoOpOnPlainTables`
- `model.TestContractDayRetirementKeepsWhatTheReaperKeeps`
- `TestContractPartitionMaintenance` and the payment-hold, archive and registry
  tests, rewritten for S1 and for days.

On ab2d3ed87, scratch variants that use only that build's API fail on each
root cause:
- the week is still there 36 days past its end at a 35-day horizon;
- a live contract's six rows are removed;
- the closed-leaf ANALYZE queues behind a long leaf lock;
- the drop waits up to 5 s for each lock;
- week A's five kinds of kept contracts are archived and dropped.

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
