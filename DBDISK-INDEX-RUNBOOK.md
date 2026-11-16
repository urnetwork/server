# DBDISK-INDEX-RUNBOOK — the index diet (S3) and the I2/I3 rebuilds on Main

Written 2026-11-16, 03:50–04:50Z. Every number about Main comes from read-only
catalog and statistics views. They were read through `pg187/pgq.sh` with a
20 s statement timeout, and no transaction was held. Disk and IO figures come
from Mimir. No pgstattuple, no EXPLAIN on Main, no DDL.

**Each step below needs the owner's go.** As written (11-16 about 05:00Z)
nothing here had run. The step D launcher and its log are in the dcdb2db1 job
tmp (`idiet1116/scripts/stepd_drop.sh`, `idiet1116/logs/stepd.log`). Plans and
sizes are in DBDISK-STABILIZE.md §S3 and §3.6.

## 0. What changed against DBDISK-STABILIZE §S3

1. **The M4 and M5 readers are already live everywhere.** Nothing needs to
   roll before the drops.
   - M4 is `transfer_contract_create_time`: merge 1400865e7, 11-01.
   - M5 is `transfer_contract_open_payer_network_id_transfer_byte_count`:
     merge 88efea19b with the prober rewrite 04d883882, 11-01.
   - Every running build contains all three commits:

     | Service | Build | Server commit |
     |---|---|---|
     | api | 1099882160 | 23445d250 |
     | connect | 1099886170 / 1099645440 | 23445d250 / ac8e90b89 |
     | taskworker | 1099801450 | main with bdb11f61c |
     | alt | 1099884070 | |
     | proxy | 1099720290 | ac8e90b89 |
     | gossip | 1098358910 | aa7142599 |
     | mcp | 1098368160 | aa7142599 |

   - **Open: the monitor,** which another developer runs. Its build must be
     at or after 1400865e7 (f617af4fe, the bounded per-minute count and stale
     probe). The owner should confirm. The monitor statements seen in
     pg_stat_statements already have the bounded form.
2. **The old statements have been flat for about 14 days.** That meets the
   7-day rule of df37ab075, which asks for a period that includes a Sunday
   payout and an epoch close.
   - Since 11-15 10:10Z, none of these has gained a call:
     - the pre-rewrite create_time readers: qid -1719341836518654401 (2,464
       calls), -5941807944576734764 (215) and 5134549349201100013 (969);
     - the old prober payer scan 2183257677105093204 (90,773).
   - Against the 11-01 17:14Z counts (2,335 / 922 / 86k) and their earlier
     rates, their last calls were about 11-02 03:00–04:00Z, when the
     rewrites rolled out *(inferred)*.
3. **The partial `transfer_contract_open_source_id` is BLOCKED.** Do not
   build it. Building it would not let the full index be dropped.
   - Two families of live statements read the index's closed rows (`open =
     false`), and no other index can serve them.
   - **The companion lingering-origin arm:** `open = false AND $3 <=
     close_time AND contract_id >= <unexpired floor> AND source_id = $1 AND
     destination_id = $2`.
     - It runs in `companionOriginExistsSql` (qid -2430697207515549092: 1.69B
       calls since 10-23, about 940/s now) and in the companion-origin
       selections (qid -8428423035090577349, about 690/s, plus the chained
       form).
     - Together that is about **1,200 scans/s** on this index, at about 0.6
       entries per scan.
     - `model/subscription_contract_expiration.go:20-25` names this index as
       the reason these reads are an index range.
   - **Prober shard retention's window:** `open = false AND source_id =
     client`, in `model/prober_shard_retention.go:1067`, about 0.7 calls/s.
   - **Why the fallback fails:** `transfer_contract_destination_id_close_time`
     is the only other index that leads with a pair column, and it would read
     each destination's whole recent fan-in.
     - pg_stats gives the top destination 0.38% of contracts. At 1,374
       contracts/s that is about 18,800 entries an hour.
     - The largest destinations would cost thousands of heap fetches per call
       on a path that runs 1,200 times a second.
   - **Shrinking this index needs a design change first,** an owner decision
     (§8): move the lingering-origin reads off the pair index, for example to
     a small side table of early-closed origins kept until their deadline, and
     move the prober window to another access path.
4. **The permanent gain is 362 GiB, not 600–680 GiB:** 323.5 GiB (M4) plus
   38.4 GiB (M5).
   - That is about 170 bytes per contract, at today's density: 24% of
     transfer_contract's 702 index bytes per row, or about 6.5% of the six
     contract tables' bytes per contract.
   - Every transfer_contract vacuum pass then reads 362 GiB less index:
     1,497 GiB becomes 1,136 GiB.
   - open_source_id's own bloat, about 158 GiB by the catalog estimate, can
     still be reclaimed by an I3 rebuild (§6).
5. **M5's index still serves about 2.4 scans/s,** at 2,000–4,000 entries per
   scan and with no heap fetches.
   - The rate tracks prober retention's classify statement, about 3.5 scans
     per call (qid 2094465610247327577, `contract_id = ANY($1) AND
     payer_network_id = $2 ...`). That fits a skip scan of the index ANDed
     with the primary key's bitmap.
   - The statement is bounded by its batch's contract ids, so without the
     index it reads only the primary key.
   - No code statement reads `open` together with `payer_network_id`: every
     payer reader uses the CASE predicate, which matches
     `transfer_contract_unresolved_payer_transfer_byte_count`.
   - `TestProberRetentionClassifyNeedsNoOpenPayerIndex`
     (model/index_diet_plan_test.go) proves it on a disposable copy: under its
     bitmap settings and with the index gone, classify reads only the
     primary key.

## 1. State and fixed windows

| Item | Value (11-16 about 04:30Z) |
|---|---|
| edge-2 `/` free | **4,986 GiB** of 14,077 (35.4%); database 8,929 GiB; pg_wal 35 GiB |
| Index sizes | create_time 323.5 GiB; open_payer 38.4 GiB; open_source_id 323.0 GiB |
| WAL | 44.7 MB/s (about 13 GiB per 300 s checkpoint); 6,622 timed / 25 requested checkpoints since 10-23; `max_wal_size` 64 GiB, which requests a checkpoint at about 33.7 GiB of WAL since the last one |
| transfer_contract vacuum | routine `VACUUM ANALYZE` 2.4 h in, now at "vacuuming heap" (its last long phase); xid age 231M, so **the next pass is anti-wraparound** and cannot be cancelled |
| Others | transfer_escrow xid age 190M (routine vacuum running); contract_close 184M; transfer_escrow_sweep 184M |
| Replication slots | none (one exists only while an edge-6 copy runs; `max_slot_wal_keep_size` 32 GiB) |
| Old transactions | no client transaction older than 10 minutes |

**Windows:**
- **The reaper's backlog delete** (I1 rescan) runs from about **11-17 00:00Z**
  for 2–3 days, to about 11-19/20.
- **The edge-6 full copy** starts **Thu 11-19 06:00Z** and reads edge-2 for
  about 25 h, to about 11-20 07:00Z. No DDL or rebuild from 11-19 05:00Z
  until it ends. The next runs are Sun 11-22 06:00Z and Thu 11-26 06:00Z.
- **The weekly payout:** Sat 22:00Z to Sun 16:00Z.
- **Database maintenance:** daily at 03:00Z and 07:00Z. Keep 30 minutes
  clear on each side.
- **The quiet window for rebuilds** (as in O2): 22:30–14:30Z.
- **The validator gate,** for every step: progress.json intent finalized or
  applied, at least 15 minutes to the next 360-block boundary, and no
  `stage=measurement` begin in the last 40 minutes (`validator_ok` in
  `cpuopt5ops/lib.sh`).
- **Deploy locks:** step D holds `$T/deploy.lock.d` around each drop, so it
  is the only production change at the time. The record merge and its
  migrate are sequenced by the coordinator.

## 2. Step P — partial `transfer_contract_open_source_id`: do not run

It is blocked for the reasons in §0.3. The index stays as it is, and §6
rebuilds it. `TestCompanionClosedOriginReadsNeedTheClosedPairIndex`
(model/index_diet_plan_test.go) pins the dependency: the lingering-origin arm
is a pair range of the full index, and with an open-rows partial in its place
it loses every pair condition.

## 3. Step R — readers: nothing to roll

All the services listed in §0.1 already run builds with the rewritten
readers. Before step D, **the monitor's build must be confirmed** to be at or
after 1400865e7.

## 4. Step D — drop create_time, then open_payer, and record both

**Never leave a drop waiting behind a vacuum.**
- While DROP INDEX CONCURRENTLY waits for the table lock, it holds an active
  snapshot. PortalRunUtility pushes it, and index_drop pops it only after its
  first commit.
- That snapshot pins Main's vacuum horizon for every table for as long as the
  wait lasts: the pattern of the 11-03 dump snapshot.
- On 11-16 a drop queued behind transfer_contract's anti-wraparound pass held
  the oldest backend_xmin from 06:39Z until it was cancelled at 07:00Z.
- So every lock wait is bounded with `lock_timeout = '30s'`. On a timeout the
  run releases everything and polls again with short read-only statements.

**What runs.** `idiet1116/scripts/stepd_after_vac.sh` (dcdb2db1 job tmp).
- It polls until no anti-wraparound vacuum holds transfer_contract, then takes
  `deploy.lock.d`, then re-checks the table and the validator gate.
- Each drop runs in its own direct session on edge-2, as postgres on the local
  socket, not through PgBouncer and never inside BEGIN:

```
SET application_name = 'index-diet-drop';
SET lock_timeout = '30s';
SET statement_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS public.transfer_contract_create_time;
-- then, in a new session:
DROP INDEX CONCURRENTLY IF EXISTS public.transfer_contract_open_payer_network_id_transfer_byte_count;
```

- `ROUTINE=cancel` keeps the server's 1 s deadlock_timeout, so PostgreSQL
  cancels an ordinary autovacuum that holds the table (rule (a) applied to the
  drops).
- `ROUTINE=wait` sets deadlock_timeout to 6 h, so no autovacuum is cancelled
  and the run waits for a gap instead.
- A drop interrupted after its first commit leaves an invalid index; the next
  try drops it again.

**Gates, read right before the launch:**
1. The validator is idle, with at least 15 minutes to the boundary.
2. `deploy.lock.d` is held.
3. The monitor's build is confirmed (step R).
4. No CREATE INDEX or REINDEX runs on transfer_contract:
   `SELECT count(*) FROM pg_stat_progress_create_index WHERE relid =
   'transfer_contract'::regclass` must be 0.
5. No transaction older than 10 minutes holds a lock on transfer_contract.
   The drop waits twice for every transaction that holds one.
6. The time is outside the 03:00/07:00Z maintenance, the payout block and
   any edge-6 run.
7. The state of transfer_contract's vacuum:
   `SELECT p.phase, a.query ~ 'wraparound' FROM pg_stat_progress_vacuum p
   JOIN pg_stat_activity a USING (pid) WHERE p.relid =
   'transfer_contract'::regclass`.
   - **No row:** launch now.
   - **Anti-wraparound pass:** do not launch. Poll until it ends; the drop
     must not wait behind it (see above).
   - **Routine pass:** with ROUTINE=cancel, launch. PostgreSQL cancels the
     pass after 1 s and loses its work. With ROUTINE=wait, poll for the gap.

**Lock behavior:**
- SHARE UPDATE EXCLUSIVE on transfer_contract, held across the drop's
  internal transactions.
- AccessExclusive on the dropped index only, for its last moment.
- **Application reads and writes are never blocked.**
- Autovacuum and analyze of transfer_contract wait meanwhile.
- Each drop invalidates cached plans on transfer_contract once, so every
  backend replans: a CPU blip of seconds.

**Duration:**
- The work itself takes seconds: two catalog commits, one sweep of
  shared_buffers, and the unlink.
- Wall time is the lock wait (at most 30 s) plus two waits for the longest
  transaction holding a lock on transfer_contract. Those are seconds today;
  reaper batches take about 2 s. The waits after the first commit hold no
  snapshot.
- If they pass 30 s, the drop stops with the index invalid, and the next try
  finishes it.

**Free space:**
- **+361.9 GiB at commit:** 4,986 becomes about 5,348 GiB, unlinked
  immediately (the first 1 GiB segment goes at the next checkpoint).
- No space is needed.

**WAL:** under 1 MB.

**Effect on the edge-6 copy:**
- No overlap: done 2–3 days before 11-19 06:00Z.
- Thursday's full copy reads 362 GiB less, about 45 minutes at the 11-10
  copy's rate.

**Effect on the reaper:**
- No conflict. Its batch transactions take ROW EXCLUSIVE, which is
  compatible, and the drop only waits for the in-flight ones.
- **Done before 11-17 00:00Z, every vacuum pass during the backlog delete
  reads 24% less index.** New contracts and non-HOT updates also write two
  index entries fewer.

**Planner:**
- create_time's endpoint probes stop: about 80/s, one tuple each.
- Ranges on create_time are then estimated from the histogram.
- The live create_time statements are already forced onto indexes that
  remain:
  - the open deadline page → `transfer_contract_open_partial_create_time`;
  - the dispute page → `transfer_contract_dispute_partial_create_time`;
  - the payer due close → `transfer_contract_unresolved_payer_transfer_byte_count`;
  - the provider stats → `transfer_contract_destination_id_create_time`;
  - the stats, degradation and straggler reads → primary-key ranges.
  - Plan tests: `model/contract_create_time_window_plan_test.go`,
    `monitor/contract_create_time_reads_plan_native_test.go` and
    `TestCloseDeadlinePagesNeedNoCreateTimeIndex`
    (model/index_diet_plan_test.go).

**Recording (same step, minutes after the drops):**
1. Merge main into `feat/index-diet-v2` first, so the two records take the
   next free numbers after main's migrations. Their versions are positional
   (`IndexDietMigrationVersion`), and the monitor contracts follow them.
   - On 11-16 main held 979 (the provider-only work session journal), so the
     records were 980 and 981.
   - If S4's head guard lands first as 980, the records become 981 and 982.
2. Merge `feat/index-diet-v2` into server main with `--no-ff`, after testing
   the merged tree. Push, then `git pull --ff-only` on local main.
3. Then `db migrate` from local main (memory `db-migrate-from-mac.md`; no
   PGOPTIONS; the Main tunnel's 127.0.0.1).
   - It must apply only the two records, as no-ops (`DROP INDEX CONCURRENTLY
     IF EXISTS`).
   - If any other migration is still pending on Main, stop and leave it to
     its owner.
   - Check: `migration_audit` shows the head moving by exactly the two
     records, with success.
- **Do not merge the records before the drops.** The next `db migrate` by
  anyone would run the drops itself, queued behind a vacuum, with no
  lock_timeout.
- **Do not run `db audit --fix` between the drops and the migrate.** It would
  rebuild the indexes with a plain, write-blocking CREATE INDEX.
- Binaries built from main after the merge need the new head before they
  report ready. So migrate before releasing the locks.

**Check after the drops:**
- `to_regclass(...)` is NULL for both indexes.
- `SELECT count(*) FROM pg_index WHERE indrelid =
  'transfer_contract'::regclass AND NOT indisvalid` is 0.
- `/` free space rose by about 362 GiB.

**Watch for 60 minutes:** pg_stat_statements mean times, each expected equal
or lower.
- 8437965724473949568, the open deadline page: about 6 ms.
- 1542465414678677863, the dispute page.
- 2499203249236021781, the payer due close.
- 2094465610247327577, prober classify: 41.6 ms.
- 5947710859228294826, provider payout stats: 459 ms.
- -7051661812922688355 and 1289807780784610269, the provider stats.
- Also taskworker oldest overdue (under 60 s), debit lag (under 300 s), and
  api 5xx and latency.

**If an invalid index is left** (the session died midway): run the same DROP
again.

**Rollback** (only on a proven regression):
- `CREATE INDEX CONCURRENTLY` of the original definitions:
  - `(open, payer_network_id, transfer_byte_count)`: about 38 GiB,
    deduplicated;
  - `(create_time, open, contract_id)`: about 106 GiB compact, plus sort temp
    of the same size.
- Each takes 1–2 h: two reads of the 963 GiB heap, plus the build.
- Then a migration that records the rebuild.

## 5. Step I2 — the three drained partials (rule (a))

| Index | Table (heap) | Size | Compact | Duration | Needs free |
|---|---|---|---|---|---|
| transfer_escrow_unsettled_balance_contract | transfer_escrow (482 GiB) | 116.5 GiB | under 0.1 | 8–12 min (inferred: two heap reads) | 1,000 GiB floor |
| transfer_contract_outcome_null | transfer_contract (963 GiB) | 18.0 GiB | under 0.1 | 16–20 min (measured 11-13/14: 16m12s–18m47s for the same shape) | 1,000 GiB floor |
| transfer_contract_open_partial_create_time | transfer_contract | 9.7 GiB | under 0.1 | 16–20 min | 1,000 GiB floor |

- **Tool:** `cpuopt5ops/o2c_reindex.sh <index>`, through `bringyourctl db
  reindex-index` (REINDEX INDEX CONCURRENTLY, parallel 4). Use a binary built
  from local main.
  - Its gates: the window, the backup, no other transaction over 10 min, the
    health limits, free space ≥ 1,000 GiB + 2.5 × compact, and the validator.
  - Its in-run stops: overdue, debit lag, snapshot age over 75 min, and dead
    rows in the queue tables.
- **Rule (a):** it starts in the gap right after a pass. Its held lock makes
  the next routine autovacuum skip the table, and an anti-wraparound one wait.
  The walker refuses while transfer_contract's xid age is ≥ 185M, so the
  contract partials go **right after the coming anti-wraparound pass**.
- **Lock:** SHARE UPDATE EXCLUSIVE on the table for the whole rebuild. Reads
  and writes continue.
- **Horizon:** a partial index has no safe-snapshot path, so the xmin horizon
  is held for the rebuild's length, 10–20 min.
- **WAL:** under 1 GiB.
- **Free space:** +144 GiB in all. No measurable space is needed.
- **Backup:** never during an edge-6 run.
- **Reaper:** autovacuum must keep up during the backlog delete, so run these
  **before 11-17 00:00Z** if a gap opens, else after it ends (from about 11-20
  08:00Z, after the Thursday copy).
- **After the three:** turn on `priority_reindex_enabled` in
  `db_maintenance.yml`, which keeps them compact.

## 6. Step I3 — full rebuilds after the backlog delete

Run these after the reaper's backlog delete and a fresh bloat run (the
weekly `db_bloat_measurement`), one at a time, with a 10-minute cool-down.
The tool and rule (a) are the same as I2.

The compact sizes below are catalog estimates (index entries × tuple width,
fillfactor 90). They agree with the 11-15 bloat run within about 5 GiB,
except destination_id_close_time and closed_usage, where this estimate
reclaims about 19 GiB more.

**Durations (inferred):**
- They assume two heap reads at the measured 1.7–2.0 GiB/s, plus about
  3 GiB/min of index written (the 11-11 network_client rebuilds).
- The table gives the central estimate.
- Budget 1.5× that, plus vacuum-gap waits, for the transfer_contract
  indexes. The first rebuild is the measurement.

| # | Index | Size GiB | Compact GiB | Reclaims GiB | Free needed (1,000 + 2.5×) | Est. duration | WAL ≈ |
|---|---|---|---|---|---|---|---|
| 1 | transfer_escrow_pkey | 422.1 | 104 | 318 | 1,260 | 45 min | 100 GiB |
| 2 | transfer_escrow_balance_contract | 412.7 | 104 | 309 | 1,260 | 45 min | 100 GiB |
| 3 | contract_close_pkey | 345.8 | 194 | 152 | 1,485 | 75 min | 190 GiB |
| 4 | transfer_contract_open_source_id (kept, §0.3) | 323.0 | 165 | 158 | 1,413 | 70 min | 160 GiB |
| 5 | transfer_contract_pkey | 200.4 | 67 | 133 | 1,168 | 40 min | 65 GiB |
| 6 | transfer_contract_destination_id_create_time | 218.1 | 87 | 131 | 1,218 | 45 min | 85 GiB |
| 7 | transfer_contract_destination_id_close_time | 214.3 | 87 | 127 | 1,218 | 45 min | 85 GiB |
| 8 | transfer_contract_closed_usage | 140.8 | 87 | 54 | 1,218 | 45 min | 85 GiB |
| 9 | contract_close_report_evidence_pkey | 144.9 | 93 | 52 | 1,233 | 35 min | 90 GiB |
| 10 | contract_close_original_census | 167.7 | 127 | 41 | 1,318 | 45 min | 125 GiB |
| 11 | transfer_escrow_sweep_pkey | 107.3 | 66 | 42 | 1,165 | 30 min | 65 GiB |
| 12 | contract_close_report_pkey | 148.8 | 110 | 38 | 1,275 | 40 min | 110 GiB |
| 13 | transfer_escrow_sweep_sweep_time_covering | 78.4 | 48 | 30 | 1,120 | 20 min | 45 GiB |
| | **All** | **2,925** | **1,339** | **1,585** | | **about 10 h of rebuilds** | **about 1.3 TiB** |

**Per rebuild:**
- **Free space:** peak transient = compact (the new copy) + about compact in
  sort temp + up to 64 GiB of WAL.
  - For example, contract_close_pkey: about 450 GiB.
  - Net gain at the end = size − compact.
  - With about 4–5 TiB free through late November (DBDISK §4 B and C), every
    gate passes with a margin of 2.5–3.5 TiB.
- **WAL:**
  - About the compact size, written over the rebuild's build phase:
    50–200 GiB, so +20–60 MB/s on top of 45 MB/s.
  - Checkpoints stay time-driven up to about 110 MB/s of total WAL. Above
    that they are requested by WAL volume (watch
    `pg_stat_checkpointer.num_requested`).
  - pg_wal stays near `max_wal_size` (64 GiB).
  - **Never during an edge-6 run:** WAL over the backup slot's 32 GiB
    `max_slot_wal_keep_size` invalidates the slot.
- **Lock:** SHARE UPDATE EXCLUSIVE for the whole rebuild. Reads and writes
  continue, and autovacuum of that table waits.
- **Horizon:** no horizon is held for plain btree keys on the safe-snapshot
  path. closed_usage is partial, so it holds the horizon for its run.
- **Reaper:** after the backlog delete only. A rebuild during it would carry
  the 255M doomed contracts' entries and block vacuum for an hour at a time.
- **Gain:** about −1,585 GiB at today's sizes. DBDISK §3.6 expects about
  half to regrow within ~35 days.
- **Removed from the list:** create_time and open_payer (dropped in D).

## 7. Proposed dates (owner's go per step)

| When | Step | Notes |
|---|---|---|
| **Mon 11-16, after transfer_contract's anti-wraparound pass (started 06:36Z)** | **R** confirm the monitor build; **D** drop create_time then open_payer with stepd_after_vac.sh (bounded 30 s lock waits, never queued behind a pass); then merge the records | Before the reaper's backlog delete (11-17 00:00Z). The 06:35Z gap was skipped during a release roll; a drop queued behind the pass was cancelled at 07:00Z because it pinned the horizon |
| Mon 11-16, if a gap allows, by about 20:00Z | **I2** transfer_escrow_unsettled_balance_contract, then the two contract partials after transfer_contract's anti-wraparound pass | Otherwise from 11-20 08:00Z |
| 11-17..11-19 05:00Z | none | the backlog delete; D could still run if it slipped (it is cheap) |
| Thu 11-19 05:00Z – Fri 11-20 ~08:00Z | none | the edge-6 full copy |
| Fri 11-20 08:00Z – Sat 11-21 22:00Z | fresh bloat run; **I3** #1–#3 (transfer_escrow's two, contract_close_pkey: −779 GiB); I2 if not done | quiet window 22:30–14:30Z |
| Sun 11-22 16:00Z – Wed 11-25 | **I3** #4–#13 | not during the Sun 11-22 06:00Z run; stop by Thu 11-26 05:00Z |

**Free-space ledger at today's free space (4,986 GiB at 11-16 04:30Z),
before ordinary growth:**
- After D: +362, about 5,348 GiB.
- After I2: +144.
- After I3: +1,585, half regrowing over ~35 days.
- About +2.1 TiB against no rebuilds by about 11-25. About 1.3 TiB of that
  is permanent: the drops and the sparse survivors of the 11-01..11-12
  deletes.

## 8. Owner decisions in this runbook

1. Go for D on 11-16, and whether rule (a) also lets the drop's lock cancel a
   routine autovacuum (ROUTINE=cancel). Without it (ROUTINE=wait), D waits for
   a gap. It never waits behind an anti-wraparound pass.
2. The monitor's build, as the other developer runs it (≥ 1400865e7).
3. The partial open_source_id stays blocked. Choose one:
   - (i) keep the index and rebuild it in I3 (−158 GiB, half regrows);
   - (ii) commission the design that moves the lingering-origin and prober
     reads off its closed rows, after which the partial build and drop free
     about 320 GiB permanently.
4. Go for I2 on 11-16 or after the copy, and for each I3 window.
