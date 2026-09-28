# Ordered grant index rollout

Prepared locally on 2026-09-27. No Main DDL, manual vacuum, backup interruption,
or grant paging deployment has been performed. This migration installs only
the index; it does not enable the separately parked 64-row allocation writer.

## Evidence and intended effect

At 14:04:52–14:05:21 UTC, the current-window allocation query completed
908.1 calls/s, returning 241.25 rows/call and touching 9020.8 shared buffers/call.
Its 8.192 million hits/s were 73.8% of all observed top-level statement hits.
Accumulated execution wall time is not CPU time. A bounded representative Main
plan returned 322 grants with 12,069 grant-scan shared hits, no physical reads,
and 276ms total execution. Its estimate was one row. The installed index is
`(active, network_id, start_time, end_time)` and is approximately 979MB.

The proposed index orders one payer's current grants by their exact existing
allocation order. Once the writer uses a complete `(end_time,start_time,balance_id)`
keyset and stops after funding, it need not read unused later grants. An SQL
`LIMIT` without this ordering index still scans and sorts the eligible set.
No mutable byte-count column is included. The cost is another index to maintain
on grant creation, active transitions, and key updates.

The local deterministic plan test holds an old snapshot across 16 indexed-field
updates. It compares the complete read, an unaligned 64-row page, and an aligned
64-row page using actual buffer counters. It then releases the snapshot and
vacuum-cleans only its synthetic local table to measure reclaimability separately.
This is a mechanism control, not a forecast of Main's CPU improvement.

Fixture result: complete read 8,802 hits; unaligned LIMIT 64 8,799 hits plus
Sort; ordered-index LIMIT 64 1,104 hits without Sort. More importantly, with
the same index set the complete read fell from 8,777 to 102 hits only after
releasing the held snapshot and vacuuming the synthetic fixture. Keep these
two mechanisms separate: bounded allocation work versus reclaiming versions.

## Backup boundary

At 14:12:47–14:13:12 UTC the same `pg_dump` transaction, begun at midnight,
advanced its `transfer_escrow` COPY by 424MB and 3.02M rows in 24.975s. It was
moving at about 17MB/s. COPY exposes no total-byte denominator for this stream,
so these counters do not establish a completion ETA. Its xmin age was 166M.

At 14:24:39–14:25:04 UTC the same COPY advanced 603.6M to 608.1M tuples,
about 178.7K tuples/s and 25.2MB/s. The table's current estimate is 3.575B rows,
so a rate-based remainder is measured in hours (roughly 4.6–6.8 hours for this
table at the two sampled rates), not minutes. This is not a promised dump ETA:
the row estimate is not the backup snapshot's exact total, rates vary, and
other tables and publication stages remain.

The installed source script matches the reviewed Xops SHA. Its streaming
pg_dump→xz job began at 00:00 UTC; at 14:25 the partial xz was 471.8GB. Its two
complete source generations were Sept 20 and Sept 24; the Sept 24 encrypted
artifact was 515.25GB with both integrity sidecars. A fresh Mimir observation
at 14:24:39 UTC reported that same Sept 24 archive generation's ciphertext
SHA256 was verified at 11:00:04 UTC today. This verifies transfer/storage
integrity, not successful decryption or a restore rehearsal. No restore-test
evidence was established. The archive pull completed successfully at 12:43:49
UTC today. The deployed source timer is Sun/Thu at 00:00 UTC (next regular
generation after this one is Oct 1), not an hourly automatic replacement.
Preserve this moving backup. Source storage had about 869GB available at
14:25; encryption temporarily needs another artifact-sized output before
the compressed input is removed, so continue watching free storage.

`transfer_balance` had approximately 7.59M live and 1.67M dead tuples even after
the 14:12:37 autovacuum. This is consistent with the old snapshot retaining
versions; these estimates and index size do not quantify exact bloat. When the
dump completes, observe horizon advancement and a later normal autovacuum,
then repeat the same bounded plan and statement deltas. Reclamation may reduce
buffer work before any new index or paging deployment. Do not kill a progressing
dump or run an unbounded full-table bloat scan to accelerate this decision.

Other active writers also held older horizons: at 14:34 the atomic payout
planner's sweep assignment transaction was about 2h47m old; a reliability
full-window re-anchor was about 79m old; and the daily transfer-audit rollup
was about 29m old. Exact normalized-query hashes matched their source. Do not
assume removing the dump alone frees every retained version, and do not
interrupt the financial transaction or weaken its atomicity. Wait for the
actual oldest horizon to advance. The pg_dump transaction releases its
snapshot before later encryption/archive publication finishes.

PostgreSQL's [concurrent-build rules](https://www.postgresql.org/docs/18/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY)
require two scans and a wait for older snapshots before validity. A partial
index can therefore finish scanning and remain unavailable behind this backup.
Track its [progress phases](https://www.postgresql.org/docs/18/progress-reporting.html#CREATE-INDEX-PROGRESS-REPORTING)
and catalog validity rather than treating elapsed time as success or failure.
Main was confirmed as PostgreSQL 18.4. Starting this build during the current
dump cannot deliver its usable index before that older snapshot exits, while
the scans and an index already made ready for writes add work. The preferred
first step is post-snapshot normal reclamation and a new measurement, not an
immediate concurrent build. This recommendation does not authorize cancellation
of either a backup or an in-progress index build.

## Sequence

1. Recheck the live backup identity/progress, oldest horizons, direct PG CPU,
   free storage, and any concurrent index/maintenance operation on
   `transfer_balance`. Prefer the post-backup reclamation observation before
   spending build work. No production command is authorized by this document.
2. Review migration 739's exact DDL and run focused migration/catalog tests.
   Apply it through the normal audited migration owner on a direct maintenance
   connection. Do not run it inside a transaction or combine its two production
   statements into one protocol round trip. Use one owner with a known live
   process/session handle. Keep maintenance parallelism low for the build and
   choose its timeout to cover a reviewed snapshot wait; neither setting belongs
   in a global database configuration change.
3. Watch `pg_stat_progress_create_index`: phase, blocks/tuples scanned, locker
   progress, and the one exact build PID. A live old-snapshot wait is not a
   failed build and must not trigger another migration process. Application
   service, direct CPU and backup throughput remain independent controls.
4. Require the exact expected definition, `indisvalid`, `indisready`, successful
   migration audit/catalog identity, and a fresh transaction's bounded
   `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF)` on representative payers. The aligned
   `ORDER BY ... LIMIT 64` must use ordered access without a Sort and reduce
   buffer work. Test the continuation keyset too; do not infer an end-to-end
   improvement from the index's presence alone.
5. Restore/review the parked allocation writer against current source, rerun
   its cross-page funding/priority/mirror/snapshot/error tests, and verify all
   serving writer artifacts (API and Connect), not just Taskworker. Enable only
   after the schema gate above. Observe page/rank distributions as well as query
   calls, rows and hits; heavily exhausted grants can require several pages.
6. Verify user outcomes: direct PG CPU approaches the normal roughly 30% under
   comparable traffic; contract latency/cancellation improves; acknowledged
   distinct probe coverage keeps its deadline; ordinary provider cohorts recover.
   Index presence or passing local tests cannot complete that verification.

## Failure and rollback

The migration uses the repository's restartable online pattern: first drop
only `transfer_balance_active_network_end_start_id` concurrently, then create
that index concurrently. This recovers invalid residue and a create-before-audit
crash. It also drops a valid same-name index on replay. Therefore never replay
this migration after enabling a writer that depends on bounded ordered access
without first rolling that writer back to the complete-read implementation.
Keep the existing active-network-start-end index throughout this rollout.

An observation timeout does not authorize killing or duplicating a build.
Re-poll its exact live handle. After a confirmed terminal failure, inspect the
specific index relation and migration audit. Invalid residue adds write cost
but is unusable for reads; repair only that candidate through the reviewed
retry path. A writer rollback alone is sufficient to stop paging. It does not
require dropping either index or interrupting the backup.

Candidate definition:

```sql
CREATE INDEX CONCURRENTLY transfer_balance_active_network_end_start_id
ON transfer_balance (network_id, end_time, start_time, balance_id)
WHERE active;
```
