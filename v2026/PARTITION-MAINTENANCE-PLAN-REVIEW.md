# Review of the partitioning and maintenance plan

Reviewed with Astra at maximum reasoning on 2026-10-23. This is a design
review of the **unimplemented**
`PARTITON-MAINTENANCE-PLAN.md`, against source at
`a8050a28634ebee4c4a556f63b74ed6c88dec4db`. The reviewed plan has 357 lines
and Git blob hash `1f21836f98a831c5d14addc21bf364baa359b380`. The source is in
the `docs/partition-maintenance-plan` worktree at
`/Users/brien/urnetwork/temp/contract-partition/server/PARTITON-MAINTENANCE-PLAN.md`;
all code references below use that worktree at the stated commit.

**Recommendation: revise before approving the partition cutovers, table swaps,
or automatic compaction.** The maintenance direction is sound, but several
proposed retention and replacement operations discard application invariants.
Bounded vacuuming and measurement can proceed independently once their execution
and rollout requirements below are specified.

The useful foundations are the distinction between queue churn and droppable
history, choosing completion time for finished tasks, treating identity churn as
a product/protocol decision, and requiring an explicit decision before deleting
usage archives. The owner decision to avoid external compaction tools is
consistent with the proposed maintenance-window fallback.

## Findings that change the design

### 1. [P1] Finished-task drops must preserve Post retries and ownership

**Plan:** B2, lines 261–275; A7, lines 235–243.

`finished_task` is durable execution state as well as history. The current reaper
keeps successful history for 24 hours and failed, incomplete Posts for seven days
(`task/task_cleanup.go:36–40,68–84`). It also admits the exact finished-task
owners before deletion, even after the seven-day deadline
(`task/task_cleanup.go:128–145`). `RunPost` reads the stored arguments/results;
when the row is missing it completes as a permanent no-op
(`task/task.go:1857–1878,1907–1934`).

Dropping a partition after the approximately one-day retention described in B2
can therefore erase a two-day-old failed Post that is still eligible to retry.
Replacing row deletion with DDL also bypasses the existing ownership admission.
The claimed two-day lookup fanout is not valid for the full retained workload.

**Required revision:** specify both retention classes and the concurrent Post
protocol. Options include retaining whole partitions through the seven-day
window, with an explicit ownership fence for final deletion, or separating
pending Post authority from disposable history. Keeping successful rows longer
is a storage tradeoff; shortening the retry window is a behavior change. A
pre-drop query alone is insufficient unless writers/retries cannot invalidate
its result before the drop. Recalculate partition count, storage, and task-ID
lookup cost using the chosen policy.

**Acceptance tests:** with a fixed clock, retain a 25-hour-old failed Post and a
six-day-old failed Post, and execute their effects successfully. Exercise a
seven-day-old row whose finished owner is held while retirement runs. Coordinate
the retry/drop race with barriers and verify that retirement either defers or
observes the completed effect. Preserve the existing tests in
`task/task_queue_retention_test.go` against the partitioned schema.

### 2. [P1] The proposed finished-task primary key does not preserve task identity

**Plan:** B2, lines 265–267 and 273–275.

`PRIMARY KEY (task_id, run_end_time)` allows the same task ID with different end
times **inside the same partition**, as well as across partitions. The statement
that task-ID uniqueness remains enforced within a partition is false. PostgreSQL
requires the partition key in a parent unique constraint, but this does not make
the other columns unique. See the official [partitioning
limitations](https://www.postgresql.org/docs/18/ddl-partitioning.html#DDL-PARTITIONING-DECLARATIVE-LIMITATIONS).

The current schema enforces `PRIMARY KEY (task_id)`
(`db_migrations.go:1415–1435`). Readers collect rows into a map keyed only by
task ID (`task/task.go:755–825`), Post acknowledgement updates by task ID alone
(`task/task.go:1950–1958`), and owner validation reads a single row by that ID
(`task/task_completion_ownership.go:63–83`). If migration replay or dual writing
creates two copies with different timestamps, these paths can select one payload
and acknowledge both.

**Required revision:** choose an explicit uniqueness strategy before DDL. The
fenced `DELETE ... RETURNING` move in `task/task_run_once.go:15–43` is a useful
starting invariant, but its sufficiency must cover every writer, migration
backfill, replay, and retry. Alternatives are a separate identity relation or
carrying a canonical composite identity through every consumer. A per-leaf
unique task-ID index alone does not solve cross-partition duplicates. This is a
missing design proof, not a claim that the current queue creates duplicates.

**Acceptance tests:** submit one task ID with distinct end times in one leaf and
in adjacent leaves; the selected invariant must reject or safely coalesce both.
Test an interrupted backfill overlapping a live completion and a repeated
completion after an unacknowledged commit. Assert one durable payload and one
Post effect.

### 3. [P1] Rebuilding and renaming search tables can lose committed writes

**Plan:** A5, lines 206–210.

These tables do not share a whole-table replacement lifecycle.
`SearchDb.AddRawInTx` replaces one value's projections/aliases and publishes its
update record atomically; removals do the same
(`search/search.go:402–500,509–555`). A staging copy can read value A, then a
writer commits value B, then the rename publishes A again. Renaming in a short
transaction does not capture the intervening write. Swapping `search_value` and
`search_projection` also needs to preserve their consistency with
`search_value_update` and its consumers (`search/search.go:91–99`). The current
code already provides unchanged-value detection to avoid unnecessary rewrites
(`search/search.go:558–617`).

`search_provider_stats` is even less suitable for regeneration: it upserts
absolute counts, and deletes the source Redis bucket after rolling up a closed
hour (`model/search_provider_stats_model.go:99–153,157–187`). The bucket also has
a two-hour TTL (`:29–33`). A rebuild from Redis cannot reconstruct the retained
30-day history. A snapshot copy can lose the last update for an hour after its
Redis source is deleted.

**Required revision:** remove the blanket rebuild prescription. For search
values/projections, define either a writer drain beginning before the snapshot,
or captured changes with a final synchronization under a writer fence. Publish
the related relations coherently and preserve the update-log contract. Inventory
indexes, grants, triggers, and dependencies during replacement. Prefer B4's
partitioning for provider stats while preserving existing final counts and live
upserts. A copy/rename migration still needs the same write coordination.

**Acceptance tests:** pause a staging build after it reads a value; commit an
add, change, and removal; finish the swap and compare DB searches and the local
update consumer. Repeat with a closed-hour stats update whose Redis bucket is
deleted before the swap. Verify exact counts and unchanged historical rows.

### 4. [P1] Probe-history run IDs are not universally time-ordered

**Plan:** B5, lines 292–294; general ID assumption, lines 250–252.

`SetProviderEgressHealth` uses the supplied `RunId`, but when it is absent it
derives the ID from the first 16 bytes of a SHA-256 digest of client ID and
measurement time (`model/provider_egress_health_model.go:190–197`). Such IDs do
not encode a creation week. The same function deliberately preserves a delayed
receipt's original measurement time (`:163–169`).

A weekly UUID partition layout covering ordinary `NewId()` ranges can reject
these supported writes or route old rows into ranges with meaningless ages.
Keeping all exceptional values in a default/legacy leaf preserves inserts but
does not make that leaf age-droppable. This table also supplies deduplication:
`ON CONFLICT (run_id) DO NOTHING` gates later health/cycle publication
(`:223–233`). Retention must account for replay after removal of that receipt.

**Required revision:** inventory existing ID provenance and all writers before
selecting the key. Use a true retention timestamp with an explicit run-ID
deduplication strategy, or define a supported legacy-ID path and prove the
retention relationship for every time-ordered writer. Choose a retention horizon
that includes measurement consumers and the maximum accepted replay lifetime;
merely choosing a number of weeks is insufficient.

**Acceptance tests:** migrate a receipt created through the zero-RunId fallback,
insert another through that path after cutover, publish a delayed measurement,
and replay a receipt at the retention boundary. Verify routing, retained
measurement windows, and that replay cannot credit cycle progress twice.

### 5. [P1] Usage-archive partitioning needs its own migration and late-write policy

**Plan:** B5, lines 295–301; A7, lines 235–243; order, lines 347–350.

The existing archive's insertion guard requires an exact matching row in the
live `transfer_contract` table (`db_provider_usage_archive.go:8–23`). The capture
trigger archives that row immediately before its deletion (`:29–35`). Thus a
normal copy of already archived rows into a replacement with the same guard
installed fails: their live source rows have already gone. Disabling the guard
without a narrowly defined migration would remove a billing-proof invariant.

Archive writes also arrive *after* their contract-ID week has expired. Current
partition retirement deletes credit-bearing contracts in batches to trigger
archival (`db_contract_partition_maintenance.go:310–345,349–396`), including
pre-cutover history. A registry that creates only current/future weeks and
analyzes each once its calendar range closes is insufficient for this destination.
It needs historical receiving ranges and statistics after those ranges are
filled. Its no-expiry policy must remain distinct from live contract retention.

Finally, B5 requires this conversion before contract retention, while the order
puts contract cutover before B3–B5. The current maintenance code starts retirement
when it detects a partitioned contract table
(`db_contract_partition_maintenance.go:78–87`); cutover is not itself a separate
retention-disable gate.

**Required revision:** specify a verified attachment of the existing archive or
a controlled, exact-copy migration that preserves its guards and all retained
proof. Cover historical receiving ranges, concurrent capture, resumability, and
post-fill analysis. Make archive readiness an actual predecessor of retention,
or add an explicit retention enable gate. Unknown archive-retention policy must
mean no automatic drop. Preserve this even when sharing registry code.

**Acceptance tests:** start with an archived row whose live contract is absent,
and a terminal live contract from well before the ordinary runway. Migrate,
archive the latter concurrently, and compare exact proof fields and usage totals
before/after. Fail/restart the migration and archive creation. Deletion must
remain atomic with capture, and no shared-loop pass may drop the archive.

### 6. [P2] Audit retention is based on event time, which differs from ID time

**Plan:** B3, lines 277–283.

Existing reapers expire all five event feeds by `event_time`, with 180-day
constants (`model/audit_model.go:1235–1357`). Provider backfills generate fresh
IDs for historical event times
(`model/audit_provider_sweep_model.go:81–110`); transfer rollups similarly mint a
fresh ID while setting `event_time` to noon of the historical day
(`:1074–1112`). ID-week drops therefore implement insertion/ID-age retention,
not the current event-age policy. They also do not prune the existing
event-time-filtered reads or rollup replacement deletes.

The wildcard should become an explicit table list. `audit_provider_state` and
`audit_device_state` are mutable state keyed by device ID
(`db_migrations.go:4883–4908`); `audit_account_payment` is a separate payment
event stream (`model/audit_model.go:1192–1232`). The cited 180-day reapers do not
define a payment-audit retention policy.

**Required revision:** partition the intended event feeds by their retention
timestamp, or explicitly accept different retention semantics and query fanout.
Define historical backfill admission and handling of out-of-window events.
Account for any changed uniqueness when adding `event_time` to a primary key.
Name the eligible event tables and keep state/payment tables out unless each
has a separately justified lifecycle.

**Acceptance tests:** create a historical event with a newly minted ID and rerun
a historical daily rollup across a partition boundary. Verify the chosen
retention result, a single rollup contribution, and partition pruning for the
actual stats queries. Test the oldest permitted backfill after expiry.

### 7. [P2] The large-table autovacuum prescription cannot deliver its stated result

**Plan:** A2, lines 154–161.

The delete/update trigger is effectively
`min(max_threshold, base_threshold + scale_factor * reltuples)` when the maximum
is enabled. Raising only `autovacuum_vacuum_threshold` cannot make a large table
vacuum later than the unchanged 100-million-row cap. Changing the trigger does not by itself bound or guarantee a shorter
heap/index pass. PostgreSQL documents these
as distinct [vacuum configuration
parameters](https://www.postgresql.org/docs/18/runtime-config-vacuum.html).

This matters because the plan is specifically trying to free workers occupied
by long passes. Implementing its current prescription can leave worker occupancy
unchanged, or increase accumulated dead space if the cap is separately raised.
For inserts, setting a small insert threshold while leaving
`autovacuum_vacuum_insert_scale_factor` unchanged also does not establish that
threshold as the complete trigger.

**Required revision:** replace “rarer and shorter” with a per-table objective and
explicit values for the applicable base, scale, and maximum thresholds. Include
insert and freeze-driven work. Decide worker/I/O allocation from observed pass
duration, pages/indexes processed, dead-row accumulation, and wraparound margin;
partitioning is the mechanism here that reduces individual relation size.

**Acceptance:** validate the configured trigger calculation for representative
small and multi-TB row counts. In a controlled load trial, record completion
intervals and dead-space growth as workers are increased; verify the hot tables
continue to complete vacuum passes. Do not accept a reload alone as proof of the
intended scheduling effect.

### 8. [P2] Automatic online compaction needs safeguards beyond the REINDEX start gate

**Plan:** A4, lines 195–200; exemptions in A3, lines 168–171.

PostgreSQL 19 documents `REPACK (CONCURRENTLY)`, supporting the proposed
direction. Its documented requirements include space for the table **and all
indexes**, additional captured changes, a final exclusive lock, and eligibility
restrictions. It also holds a transaction that can delay other tables' vacuum
cleanup, and documents a pre-existing-snapshot visibility hazard. See the
official [REPACK resource and concurrency
notes](https://www.postgresql.org/docs/19/sql-repack.html).

Consequently, a table can pass the REINDEX load check and then hold the very
cleanup horizon A3 is intended to bound. “Free space covers the table” is also
insufficiently specified for an automated action intended to prevent disk-full
outages. A threshold based on estimated reclaimable space does not bound the
operation's peak usage or duration.

**Required revision:** pin the supported command/release at upgrade and define
eligibility, full storage/WAL headroom, lock and runtime bounds, in-flight
horizon monitoring, cancellation, and retry behavior before enabling automation.
Account for application transaction isolation and existing snapshots. Budget
compaction separately from routine vacuum; a maintenance timeout exemption must
not imply an unbounded automatic rewrite.

**Acceptance tests:** on the target PostgreSQL 19 release, exercise a long-lived
snapshot, a writer active during copying, a blocked final swap, cancellation,
and insufficient-headroom admission. Verify retained data, cleanup, pool reuse,
and the effect on another churn table's vacuum progress. These behaviors were
not exercised in this review.

## Decisions and implementation requirements to add

These items should be resolved in the implementation specification. They are
not additional claims of current data loss.

- **A1: define completion and cost budgets, not only a five-minute schedule.**
  Lines 121–145 allow several minutes per table but assume the full list takes
  seconds. Specify a single active run, an overall budget with fair/resumable
  progress, and a policy for repeated skips/timeouts. Measure successful vacuum
  age per table; repeated starts are not successful maintenance. Use autocommit
  maintenance connections and restore/discard modified pooled-session settings.
  Capture `VACUUM VERBOSE` notices if “not yet removable” is required:
  [`pg_stat_progress_vacuum`](https://www.postgresql.org/docs/18/progress-reporting.html#VACUUM-PROGRESS-REPORTING)
  does not expose that count. Test a busy table followed by an eligible one,
  repeated deadline expiry, and a pinned cleanup horizon. B6 promises A1 for
  `contract_close_report_evidence` (line 321), but A1's list omits it; likewise
  explain the disposition of the measured `provider_url_probe_run` table.

- **A3: make the timeout rollout observable and scope the exemption.**
  `ALTER ROLE SET` affects new sessions, as documented in
  [ALTER ROLE](https://www.postgresql.org/docs/18/sql-alterrole.html); the pool's
  default connection lifetime is eight hours (`db.go:217–232`). Include server
  connection recycling and `SHOW` verification through both application and
  maintenance paths. The maintenance pool is also used by ordinary application
  maintenance (`db.go:895–897`), so an exemption for that pool is broader than an
  operator-only exemption. Inventory elevated timeouts before asserting that 15
  minutes leaves every path headroom. PostgreSQL's
  [transaction timeout](https://www.postgresql.org/docs/18/runtime-config-client.html#GUC-TRANSACTION-TIMEOUT)
  terminates the session and covers implicit single-statement transactions;
  verify cancellation, rollback, reconnect, and retry semantics. A session
  `backend_xmin` alert is useful but does not establish a global horizon bound:
  include transaction-ID holders, relevant replication-slot horizons and prepared
  transactions, or explicitly verify those mechanisms are disabled. PostgreSQL
  documents these in [vacuum troubleshooting](https://www.postgresql.org/docs/18/routine-vacuuming.html)
  and [replication-slot horizons](https://www.postgresql.org/docs/18/view-pg-replication-slots.html).
  Exempt maintenance operations need their own horizon alarms.

- **A4 measurement: distinguish an estimate from reclaimable bytes.**
  `pgstattuple_approx` skips all-visible pages and scans the others; a heavily
  modified sub-50-GB relation can still require a large scan. Its output concerns
  the inspected relation, not automatically every index and TOAST relation. See
  the official [pgstattuple documentation](https://www.postgresql.org/docs/18/pgstattuple.html).
  Bound the weekly scan workload, record scanned percentage and metric scope,
  and allow for intentional fillfactor free space. The before/after rewrite
  deltas in the evidence should be called reclaimed space rather than measured
  dead-row percentages. Retain raw measurements so thresholds can be validated.

- **B4: select retention slack explicitly.** The current stats horizon is 30
  days (`model/search_provider_stats_model.go:35–39,193–214`). Dropping monthly
  partitions only once their upper bound expires keeps some rows for roughly
  another month. Daily/weekly partitions may fit the intended storage bound
  better. Quantify and accept the extra retention/storage if monthly partitions
  are kept. Existing reads include `period_start`
  (`model/provider_model.go:225–235`), so this key is otherwise well aligned.

- **B6/Part C: a client-ID range is a client cohort, not the age of a key
  registration.** An old active client can append a new generation today
  (`model/st_client_key_history.go:153–237`). Whole-cohort drops need a retirement
  and proof-lifetime policy for every member, or active clients pin the leaf.
  History supports original-close verification
  (`model/contract_close_original.go:91–96`) and currently has an immutable
  trigger, a `UNIQUE (registration_hash)` constraint, and references from retained
  heads (`db_client_key_history.go:9–36`,
  `db_client_key_policy_history.go:9–19`). The hash uniqueness omits the proposed
  partition key. Deciding a horizon alone does not resolve those constraints or
  authorize removal of verification history. Keeping this as a product/protocol
  decision is appropriate.

- **A7: preserve family-specific lifecycle and admission rules.** Add the
  existing `network_ping` family and its three tally tables to the inventory
  (`model/network_ping_model.go:1049–1119`). Specify create-ahead cadence and
  outage recovery independently of slow archive/compaction work; a new hourly
  `finished_task` layout cannot wait for a registry implemented later. Include
  late writes, parent and leaf statistics, never-drop families, canonical bounds,
  transactional guard rechecks, and resume state. “Analyze once when closed” and
  blanket REINDEX exclusion require exceptions for long-retained or still
  changing leaves. Reusing contract checks must preserve their financial
  preconditions; the separate contract-partition review remains a prerequisite
  to treating those checks as approved shared machinery.

## Cutover acceptance and validation scope

The unfinished “Code paths” section (plan lines 353–357) is necessary input to
the migrations, not a post-implementation documentation task. Complete it with
exact writer/reader names, retention predicates, uniqueness/FK/trigger contracts,
late-arrival limits, and expected pruning for each selected table. In particular,
the current facts are seven-day exceptional finished history, 180-day event-time
audit retention, 30-day provider-stat retention, non-time probe IDs, and
indefinitely retained usage proof.

Each cutover needs a defined write fence or change-capture mechanism, exact-copy
verification under that fence, bounded lock acquisition, restart behavior, and a
rollback point. A task drain must include running completions and Post retries.
Retaining the old table is useful only with a read/migration plan for rows still
needed there; “let it age out” does not keep those rows reachable through the
new table. Once writes resume against the new relation, rollback must preserve
those writes before discarding it. Exercise crashes before and after publication,
concurrent writers, and the first partition-boundary transition with deterministic
barriers and fixed clocks.

This review read the entire proposal, the relevant schema migrations and code
paths cited above, and official PostgreSQL 18/19 documentation. Incident sizes,
production settings, and production runtimes are treated as reported evidence,
not independently measured facts. No application tests, SQL experiments,
production queries, or PostgreSQL 19 runtime validation were performed for this
review. The acceptance tests above are requirements for the future implementation;
they are not reported as passing tests. Only this review document was added.
