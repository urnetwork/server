# STORAGE.md — Postgres storage diagnosis & vacuum-tuning runbook

A practical guide for diagnosing pg storage growth on the urnetwork primary and
turning the findings into durable fixes (autovacuum tuning, partitioning,
repack). Written from the 2026-07 storage effort; keep it current as you learn.

The mental model: **storage problems are almost always one of three things —
(1) an unbounded/under-retained table, (2) dead-tuple + index bloat that vacuum
never reclaims, or (3) a table whose retention mechanism can't keep up with
inflow.** The queries below tell you which, per table. The fixes are: fix
retention, tune autovacuum so it actually completes, or (for the biggest churn
tables) partition so retention is `DROP PARTITION`.

> Run the read-only diagnostics on the primary freely — they touch only catalogs
> and stats. Be careful with anything that scans a big table (see the COUNT
> warning below). Cheap-vs-expensive is called out per query.

---

## 1. Diagnostic queries

### 1a. Where is the space? (cheap — catalog only)

```sql
SELECT
  relname,
  pg_size_pretty(pg_table_size(c.oid))   AS data,
  pg_size_pretty(pg_indexes_size(c.oid)) AS indexes,
  pg_size_pretty(pg_total_relation_size(c.oid)) AS total
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r','p')
ORDER BY pg_total_relation_size(c.oid) DESC
LIMIT 30;
```

Look for: **indexes larger than data** (bloat — the healthy ratio is well under
1:1 for these tables) and tables far larger than their retention window implies.
Snapshot this to `xops/db/STORAGE<N>.csv` each time so you can diff growth.

### 1b. Vacuum health — the single most important query (cheap)

```sql
SELECT
  relname,
  n_live_tup,
  n_dead_tup,
  round(100 * n_dead_tup / NULLIF(n_live_tup + n_dead_tup, 0), 1) AS dead_pct,
  last_autovacuum,
  last_autoanalyze,
  autovacuum_count
FROM pg_stat_user_tables
ORDER BY n_dead_tup DESC
LIMIT 30;
```

Look for:
- **`last_autovacuum = NULL` on a big table** — the smoking gun. It means
  autovacuum has *never completed* on that table. In 2026-07 every large table
  (`client_reliability`, `contract_close`, `network_client`, the `transfer_*`
  tables, `provide_key`) showed NULL here while small tables vacuumed fine the
  same day. Dead space is never reclaimed and the planner runs on absent stats.
- **High `dead_pct`** (say >20%) — bloat accumulating.
- **`last_autoanalyze = NULL`** — the planner has no stats; it will pick bad
  plans (seq scans, wrong joins).
- **Suspiciously low `n_live_tup`** on a table you know is large — the stats
  were reset (`pg_stat_reset()` zeroes these *and* the timestamps together), so
  treat the NULLs as "unknown", and prefer **fixed** autovacuum thresholds over
  scale factors (a scale factor of a bogus `n_live_tup` mis-triggers).

### 1c. Per-table autovacuum settings currently in effect (cheap)

```sql
SELECT relname, reloptions
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND reloptions IS NOT NULL
ORDER BY relname;
```

Look for: which tables have custom `autovacuum_*` set (these come from
`db_migrations.go`). A big churn table with **no** reloptions is running at the
server defaults (`scale_factor 0.2`, `cost_delay 2ms`) — usually wrong for our
biggest tables (see §3).

### 1d. Is autovacuum keeping up right now? (cheap)

```sql
-- currently running (auto)vacuums and how far along
SELECT p.pid, p.relid::regclass AS table, p.phase,
       pg_size_pretty(p.heap_blks_total * 8192) AS table_size,
       round(100 * p.heap_blks_scanned / NULLIF(p.heap_blks_total,0), 1) AS pct,
       p.index_vacuum_count,
       a.query_start
FROM pg_stat_progress_vacuum p
JOIN pg_stat_activity a ON a.pid = p.pid;
```

Look for: a vacuum that's been running for hours with a high `index_vacuum_count`
(many passes) is thrashing — its `maintenance_work_mem`/`autovacuum_work_mem` is
too small for the table's dead-tuple count, so it re-scans the (bloated) indexes
once per batch. That's the mechanism by which vacuum "never completes" (§3).

### 1e. Retention / backlog probes (per suspected table)

For a time/block-keyed table, ask "how far behind is retention?" **without**
counting the backlog. Never `COUNT(*)` an expired range on a multi-TB table —
it seq-scans the whole heap and pins the vacuum xmin horizon while it runs.
Instead use `MIN` on the retention key (rides the index, instant) and `EXPLAIN`
for an estimate:

```sql
-- client_reliability example: block_number is epoch-minutes, 1440/day.
SELECT
  min(block_number)                                           AS oldest_block,
  (extract(epoch from now() - interval '30 days')/60)::bigint AS cutoff_block,
  ((extract(epoch from now() - interval '30 days')/60)::bigint - min(block_number)) / 1440.0 AS days_behind
FROM client_reliability;

-- estimated backlog size, from planner stats, in milliseconds (read rows= on top node)
EXPLAIN SELECT * FROM client_reliability WHERE block_number <= /* cutoff */ 0;
```

`days_behind` climbing over successive runs = the retention task is losing to
inflow (a structural problem, not a tuning one → partition, §4).

### 1f. Partition health (for partitioned tables like client_reliability)

```sql
-- partitions + sizes; confirms create-ahead and drop-expired are working
SELECT c.relname,
       pg_size_pretty(pg_total_relation_size(c.oid)) AS total
FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'client_reliability'::regclass
ORDER BY c.relname;
```

Look for: a contiguous set of daily partitions from ~30 days ago through
today+3 (the create-ahead buffer). A **gap at the recent end** is an emergency —
an insert for a block with no partition fails; the maintenance task must be
creating ahead (check the `[ncr]partitions created` task log).

---

## 2. Reading the results → which problem is it?

| Symptom (from §1) | Diagnosis | Fix |
|---|---|---|
| Table >> its retention window; `days_behind` rising | retention can't keep up with inflow | Partition + DROP PARTITION (§4) |
| `last_autovacuum = NULL`, high `dead_pct`, indexes > data | vacuum never completes → bloat never reclaimed | Per-table autovacuum tuning (§3) + one-time repack |
| Unbounded table with no retention task at all | missing retention | add a retention task/cascade (see model layer) |
| indexes >> data but retention is fine | index bloat only | REINDEX (or it's on the skip list → repack) |
| planner picking seq scans on an indexed predicate | `last_autoanalyze` NULL / stale stats | ANALYZE + fix autovacuum analyze threshold |

---

## 3. Autovacuum tuning — the two recipes we use

All per-table settings live in **`db_migrations.go`** as
`ALTER TABLE <t> SET (...)` migrations (search `autovacuum_vacuum` there). The
ALTER takes only `SHARE UPDATE EXCLUSIVE` — safe on a hot table. **Autovacuum
only reclaims going forward**; existing bloat still needs a one-time
`VACUUM (VERBOSE, ANALYZE)` and `pg_repack` to shrink files.

Two things make vacuum "complete" on a big table: **remove the throttle**
(`autovacuum_vacuum_cost_delay = 0`) and **keep each pass small** so it finishes
before anything cancels it. Pick the trigger by table shape:

**Recipe A — very large, high-churn tables** (`contract_close`,
`transfer_contract`, `transfer_escrow`, `transfer_escrow_sweep`,
`network_client_location_reliability`). Use **fixed thresholds**, not scale
factors — a fixed threshold keeps per-pass work constant regardless of table
size, and keeps triggering correct even when a stats reset zeroes `n_live_tup`:

```sql
ALTER TABLE <t> SET (
  autovacuum_vacuum_cost_delay = 0,          -- unthrottled: minutes not days
  autovacuum_vacuum_scale_factor = 0,        -- disable the proportional trigger
  autovacuum_vacuum_threshold = 5000000,     -- vacuum every ~5M dead tuples
  autovacuum_vacuum_insert_scale_factor = 0,
  autovacuum_vacuum_insert_threshold = 10000000,  -- steady VM/freeze work on append-heavy tables
  autovacuum_analyze_scale_factor = 0,
  autovacuum_analyze_threshold = 1000000     -- several ANALYZEs/day so the planner has stats
);
```

**Recipe B — smaller churn tables** (`network_client`, `provide_key`, `device`,
`pending_task`). Live-set-proportional passes finish fast unthrottled; the
`pending_task` original recipe fits:

```sql
ALTER TABLE <t> SET (
  autovacuum_vacuum_cost_delay = 0,
  autovacuum_vacuum_scale_factor = 0.01,
  autovacuum_analyze_scale_factor = 0.02
);
```

**Do NOT tune `client_reliability` this way** — it's partitioned; its daily leaf
partitions are small enough for the defaults, and its retention is
`DROP PARTITION`, not vacuum. The partitions are created at runtime by the
maintenance task (`ensureClientReliabilityPartition`), not by a migration, so
`db_migrations.go` can't set their autovacuum, and reloptions on the empty
parent don't drive it. If you ever *measure* a problem — the score queries doing
excessive heap fetches on recent partitions (check with `EXPLAIN (ANALYZE,
BUFFERS)` on a reliability-score query, or heap-fetch vs `idx_scan` counts in
`pg_stat_user_tables`) — the one knob worth adding is a lower
`autovacuum_vacuum_insert_scale_factor` in the partition's `CREATE TABLE …
PARTITION OF … WITH (...)`, so the visibility map refreshes sooner after a day's
inserts and the index-only scans stay index-only. Measure first; don't add it
preemptively.

### Server-level knobs (postgresql.conf, not per-table — set where pg config is managed)
- `autovacuum_work_mem` → **~1 GB**. The biggest completion lever: it caps how
  many dead TIDs a vacuum batches, and each batch costs one full pass over the
  table's indexes. 64 MB (default) on a table with a huge index = dozens of
  passes; 1 GB collapses that.
- `autovacuum_max_workers` → **5–6** (default 3 starves when several big tables
  qualify at once).
- Consider a higher `autovacuum_naptime` floor is *not* needed here; the issue
  was completion, not frequency.

---

## 4. When tuning isn't enough — partition

If a table's inflow structurally outruns row-by-row `DELETE` retention (rising
`days_behind` in §1e, backlog in the billions), vacuum tuning can't save it:
even a fast vacuum can't reclaim faster than deletes churn, and the deletes
can't outrun inserts. Convert to **range partitioning** so retention becomes
`DROP PARTITION` (metadata-only: no dead tuples, no vacuum, no index bloat, disk
returns to the OS immediately).

`client_reliability` is the worked example — see
`xops/db/client_reliability_partition_plan.md` and the implementation
(`model/network_client_reliability_partition_model.go`, the
`bringyourctl model migrate client-reliability-partition` command, and the
partition-aware `remove_old_client_reliability_stats` task). The pattern
generalizes to any block/time-keyed high-churn table where **every read filters
on the partition key** (so partition pruning applies) — verify that first.

### Reclaiming existing bloat (one-time, needs transient disk)
- `VACUUM (VERBOSE, ANALYZE) <t>` — returns freed space to the table's FSM (so
  inserts stop extending the file) and refreshes stats. No file shrink, no
  exclusive lock.
- `pg_repack <t>` — rewrites the table+indexes to shrink the file, needs
  transient free space ≈ the live table size. This is what actually returns
  disk to the OS for a non-partitioned table.
- `DROP INDEX CONCURRENTLY` a bloated secondary index — instant reclaim, no
  rewrite; the emergency lever when disk is tight (freed 659 GB on
  `client_reliability` in 2026-07).

---

## 5. The continual-tuning loop

Storage tuning is not one-and-done — inflow and schema change. Each cycle:

1. **Measure** (§1a snapshot to `xops/db/STORAGE<N>.csv`, §1b vacuum health).
   Diff against the previous snapshot: which tables grew, which now show
   `last_autovacuum = NULL` or rising `dead_pct`.
2. **Classify** each grower via §2.
3. **Act:** add/adjust the `db_migrations.go` autovacuum reloptions (§3), or
   escalate to partition (§4), or add missing retention. Commit the migration.
4. **One-time cleanup** for any table with existing bloat: manual VACUUM/ANALYZE,
   then pg_repack when disk allows.
5. **Verify next cycle:** the tuned table should now show a recent
   `last_autovacuum`, falling `dead_pct`, and stable size. If `last_autovacuum`
   is *still* NULL after tuning, the pass is being cancelled — check §1d for a
   thrashing vacuum and raise `autovacuum_work_mem`.

Cadence: snapshot monthly, and any time an alert fires on primary disk. The
`bringyourctl db maintenance <epoch> [--reindex] [--cleanup] [--analyze]`
command drives the periodic REINDEX/ANALYZE rotation (`db_maintenance.go`);
per-table autovacuum handles the between-maintenance reclamation.

### Watch items carried out of 2026-07 (update as resolved)
- [ ] `contract_close`, `transfer_contract`, `transfer_escrow`: one-time
  `pg_repack` to reclaim existing bloat (autovacuum now tuned going forward).
- [ ] `client_reliability`: finish the partition cutover + `--finalize` to drop
  the old table; confirm daily partitions create/drop on schedule (§1f).
- [ ] Set `autovacuum_work_mem`/`autovacuum_max_workers` at the server level.
- [ ] Re-snapshot STORAGE and confirm `last_autovacuum` is non-NULL on all the
  tables tuned in the 2026-07 migration batch.
