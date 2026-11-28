# CONTRACT-PARTITION-CUTOVER-NOW — the C1 cutover now, with a short transition horizon (2026-11-23)

The owner, 2026-11-23 at about 14:35Z: the daily-partition cutover of the contract tables (C1,
CONTRACT-PARTITION.md, the "C" step) has to happen now. Can contracts be dropped faster during it, to manage
free space?

This is the plan. It rests on read-only reads of Main on 11-23 (catalog, `pg_stat_*`, `db_bloat_measurement`,
small `TABLESAMPLE` reads, each statement under a 25 s timeout), on Mimir, and on server main at f09d3edd6 and
d8fadb36a. It builds on these documents:
- CONTRACT-PARTITION.md: its runbook, S1 and "Decisions for the owner";
- the 11-17 readiness runbook (`temp/contract-cutover-1117/RUNBOOK.md`);
- DBDISK-STABILIZE.md, DBDISK-INDEX-RUNBOOK.md and PARTITION-CANDIDATES.md;
- DBDISK-RETENTION-REVIEW.md, merged today.

**Nothing was changed in production.**

- Sizes are GiB (2^30) and TiB (2^40). M is million, B billion. Times are UTC.
- *(inferred)* marks a figure reasoned from code or arithmetic rather than measured.
- N is the contract horizon during the transition.
- C is the cutover day: from 00:00Z that day new contracts land in daily leaves.

## 0. The answer

**A shorter horizon is the right lever. It does not work on its own, and 14 days is too short.**
- Deleting rows returns no disk. Space comes back only through S1's compaction and drops, and through rebuilds
  of the legacy partition's indexes.
- A shorter horizon shrinks what the daily leaves hold before the first one drops, about (N + 1.25) × 245 GiB,
  and it brings the legacy partition's return forward to C + N.
- **Alone, no horizon fits** in the 4.8–5.0 TiB free at C: from 1.4 TiB short at 14 days to 6.7 TiB short at
  35 (§2.3).
- **With five companions, 14, 18 and 21 days fit** (§2.4). The companions:
  1. rebuild the legacy's indexes as the reaper empties it;
  2. return the legacy within hours of its emptying;
  3. drop B2's index first;
  4. move the archive's floor to N + 7 days;
  5. run the evidence retention as its own task, on a window of 14 days or less.
- **14 days is too short for SN25's amended epochs.**
  - An epoch's build needs the live rows of its contracts until about 9 days after its window opens (§3.1).
  - At 14 days that leaves 5 days of slack, under the week of chain halt the code budgets; 18 days leave 9.
- **28 and 35 days do not fit,** during the transition or at the steady state after it.

**Free space needed at C, including the 500 GiB floor** (C = 11-27, horizon cut 11-24 21:00Z, zero-byte pass
on until C):

| N | The horizon alone | With the companions | Lowest point with them | Legacy back | Free on 03-01 with them |
|---|---|---|---|---|---|
| 35 | 11.5 TiB | 7.9 TiB | −2.5 TiB (12-30) | 01-01 | −2.7 TiB |
| 28 | 9.6 | 6.0 | −0.6 TiB (12-23) | 12-25 | −1.1 |
| 21 | 7.6 | 4.2 | 1.2 TiB (12-16) | 12-18 | 0.5 |
| **18** | 6.9 | **3.4** | **2.0 TiB (12-13)** | **12-15** | 1.0 |
| 14 | 6.3 | 3.1 | 2.3 TiB (12-13) | 12-13 | 2.1 |

Free space at C is about 4.84–4.96 TiB in every case (5,394 GiB at 15:06Z today). With the zero-byte pass
off from now, the lowest point is 0.1–0.3 TiB lower.

**Corrections to today's first read:**
1. Rebuilding the legacy's indexes as the reaper empties it is the largest single lever, about 1.8 TiB before
   the peak (§2.4).
2. The zero-byte pass deletes 0.2% of what it reads today. Its cursor is still in October contracts.
3. The reaper deletes at most about 1,900 contracts a second, so the backlog of a cut drains slowly.
4. Every reaped contract leaves about 420 bytes in the usage archive for weeks.
5. A fresh daily leaf takes about 1.9 KB a contract, 245 GiB a day, not the 140–180 GiB a day of the 11-17
   model.
6. **No rescan runs tonight.** The reaper's pass began 11-22 23:11Z, so the 21.05M contracts whose review flag
   was cleared at 14:51Z go at the pass after 11-29 23:11Z.
7. The cutover cuts the close-evidence retention's capacity about 2.5×.

**Earliest safe start:** C = **2026-11-27 00:00Z**, the swap on **11-26 between about 07:30Z and 17:30Z** (§4.5).
Before it:
- 11-24 after 20:16Z: a taskworker build with the companions' code, and the horizon set to 18 days right after;
- 11-25: B2 and prepare.

The swap must not run from this Mac (§4.3). C = 11-26 is possible if the owner waives the backup-point
precondition.

**Top risks** (§5):
- a prepare left unfinalized and unaborted refuses every new contract from C;
- the swap run from this Mac would hold every contract table's lock for 8–25 s;
- the legacy's index rebuilds blocked by its vacuums;
- fresh leaves larger than estimated;
- inflow above the reaper's 1,900 a second;
- the legacy's 60,337 unsettled kept contracts;
- the other tables' slow decline after December.

**Decisions** (§6): the horizon (18 days recommended), when to cut it, the steady state after (21 days), the
evidence window (7 or 14 days), decisions 5 and 6 (both settled 11-18, 5 not built), and nine new ones.

## 1. What "drop contracts faster" can and cannot do

### 1.1 The first read, checked against the code

| Claim | Verdict |
|---|---|
| Deleting rows does not shrink files; only drops do | **True, with two more ways back.** `vacuum_truncate` is off on Main. On the plain tables, the reaper's freed space is reused by new rows. After C, a day leaf and the frozen legacy partition take no new rows, so a delete there returns nothing by itself. Space comes back through S1's compaction of an emptied day or of the legacy (CLUSTER, `db_contract_day_retirement.go`), through the day's drop, and through a rebuild of a leaf's indexes. A rebuild (`db reindex-index` on the legacy's `_l` names) returns the index share of what the reaper has deleted. |
| "Faster" means a shorter horizon, which shrinks both the legacy wait and the daily partitions' fill | **True, with four limits:** the code, the reaper's speed, the archive and the legacy's return. Each is below the table. |
| Zero-byte reaping helps today's plain tables but not daily partitions | **True, but it barely runs yet, and it costs archive space.** See below the table. |

**The four limits on a shorter horizon:**
1. **The code.** It clamps the horizon at 35 days twice: `model.ContractHorizonFloor` (`model/contract_horizon_reaper.go:164`) and `server.ContractDayRetentionFloor` (`db_contract_day_retirement.go:65`). A shorter horizon is a code change and a taskworker deploy.
2. **The reaper's speed.** It is one worker. On 11-17 and 11-18 it deleted about 1,720 contracts a second, in batches of 2,000 that took 0.86–1.02 s each. Inflow is 1,600–1,650 a second. A cut on 11-24 leaves a backlog of 0.26B contracts at 28 days, 0.78B at 21, 1.01B at 18 and 1.22B at 14. The reaper drains it only with what its capacity leaves above inflow.
3. **The archive.** Each reaped contract leaves about 420 bytes in `st_provider_usage_archive` until its week's close age passes `usagearchive.MinCloseAge` (42 days). The shorter the horizon, the longer a row waits there, unless the floor moves with it.
4. **The legacy's return.** The legacy partition gives its space back only once three things hold: the reaper has emptied it, its kept rows fit S1's bound, and its primary key is clean (§1.3).

**Zero-byte reaping, measured:**
- In the last 24 h it read 72.8M contracts and deleted 144k (0.2%).
- Its cursor is in late-October contracts, 25 days behind its 8-day horizon. Of those, 54% carry bytes and 44% are one-sided.
- A sample of younger contracts is 49% zero-byte for 10-28..11-01 and 58% for November. At its 2,366 reads a second it reaches them from about 11-25 and catches up about 12-01.
- **Before C** it frees about 1.3 KB net per zero-byte contract, reused in place.
- **After C** it frees nothing, adds archive rows and takes the reaper's time. So it goes off at C, as CONTRACT-PARTITION.md says.

### 1.2 What a day costs on disk

A fresh daily leaf holds every contract minted that day until S1 retires the day, N days and 6 hours after it
ends. Measured on 11-21..23 (§2.1), a contract and its rows take about **1.9 KB** in fresh leaves (range
1.6–2.3 KB):
- **Heap, about 1,050 bytes:** transfer_contract 454; one escrow 109; two closes 234; 0.54 sweeps 138; 1.25
  close reports 116.
- **Indexes, about 640 bytes at compact density,** and 0.85–1.0 KB at the density new entries are written at.
  Per inserted row on 11-21..23: escrow 2.0× compact, close reports 1.5×, closes 1.4×, sweeps 1.05×.
  transfer_contract is assumed at 1.3–1.8×.

At 1,600 contracts a second that is about **245 GiB a day** (206–296).
- **Why the 11-17 model had 140–180 GiB a day:** it took 1,439 contracts a second and 1.2–1.5 KB a contract.
  That was the marginal growth of 11-15, while transfer_escrow's heap was filling its own free space and the
  indexes were recycling pages the 11-01..11-12 deletes had emptied.
- B2's index, `transfer_contract_open_source_id` (323 GiB, still on Main), is about 0.1 KB of the 1.9 KB.

Before the first day drops, the leaves alone take (N + 1.25) × 245 GiB:

| N | 35 | 28 | 21 | 18 | 14 |
|---|---|---|---|---|---|
| Leaves | 8.7 TiB | 7.0 | 5.3 | 4.6 | 3.6 |

### 1.3 When the legacy partition comes back

At C the six plain tables become the legacy partition, about 4.6 TiB: 4,615 GiB at 14:30Z plus about 60 GiB
of growth before the horizon cut stops it. S1 treats it as one range that expires at C + N + 6 h. It then needs, per leaf:
1. **An emptied legacy.** The reaper must have deleted every pre-C contract it may delete. With an 18-day cut
   on 11-24 and C on 11-27, that is about 12-15 at 1,900 a second.
2. **At most 10,000 live rows** (`MaxCompactRows`). Today the reaper keeps:
   - 21.05M under attribution review, whose flag was cleared at 14:51Z; they go at the pass that starts after
     11-29 23:11Z;
   - 26.6M that closed late, which go at the same pass;
   - **60,337 with an unsettled escrow, for good.** These alone block the legacy.
3. **A clean primary key.** S1 counts those rows along the primary key within 5 s, and runs CLUSTER within
   500 ms. On a 1.2 TB leaf whose key holds billions of dead entries, neither finishes until a vacuum or a
   rebuild of the primary key has removed them. A vacuum of each multi-TiB leaf takes hours to days, and
   returns no space itself.

With the legacy's primary keys rebuilt and a legacy-only bound above its 60k kept rows, the compaction returns
the legacy within hours of the reaper emptying it (§4.6). Without them, it waits for six vacuums and for an
owner decision on the 60k.

## 2. Disk model

### 2.1 Measured inputs (read-only, 11-23)

**Free space and load:**
- **edge-2 free space** (Mimir `node_filesystem_avail_bytes`): 5,402 GiB at 14:30Z, 5,394 GiB at 15:06Z. It
  falls 10.5–11 GiB an hour between the 00:00Z drops, 229 GiB over the last 24 h.
- **The database** grew 167.5 GiB a day from 11-21 05:18Z to 11-23 14:37Z. Each end is a snapshot of every
  table family: `pg_total_relation_size`, `pg_relation_size`, `pg_indexes_size` and the `pg_stat_user_tables`
  counters.
- **Contracts opened** (`urnetwork_contract_opened_total`): 1,649 a second over the last 24 h, 1,416–1,516 on
  each of the six days before, 1,614 in the last hour.

**The six tables and their neighbours,** 11-21 05:18Z to 11-23 14:37Z (2.39 days):

| Table | Size now | Net GiB/day (heap / indexes) | Inserts/s | Deletes/s | Rows per contract | Bytes per live row |
|---|---|---|---|---|---|---|
| transfer_contract | 2,091 GiB | −22.6 (+41.2 / −63.8: the index walk rebuilt three of its indexes) | 1,561 | 436 | 1 | 833 |
| transfer_escrow | 783 | +22.1 (0 / +22.1: its heap still fills 231 GiB of its own free space) | 1,561 | 402 | 1.00 | 328 |
| contract_close | 750 | +37.1 (+22.7 / +14.4) | 3,122 | 683 | 2.00 | 155 |
| transfer_escrow_sweep | 541 | +26.5 (+14.1 / +12.4) | 847 | 206 | 0.54 | 461 |
| contract_close_report | 442 | +24.1 (+12.5 / +11.6) | 1,959 | 366 | 1.25 | 168 |
| contract_extender | 7.7 | 0 | 5 | 2 | 0.003 | 222 |
| *contract_close_report_evidence* | 741 | +42.4 | 1,959 | 415 | 1.25 | 296 |
| *st_provider_usage_archive* (17 leaves) | 347 | +13.3 | 436 | 0 | — | 295; new rows 420 |

- **Heap per inserted row matches the compact width** of the 11-22 bloat run (`db_bloat_measurement`).
  transfer_contract's heap grew 41.2 GiB a day for 1,125 net rows a second: 454 bytes a row.
- **Index bytes per inserted row,** adding back what the window's deletes freed: escrow 2.0× its compact
  size, close reports 1.5×, closes 1.4×, sweeps 1.05×. transfer_contract's own index growth is hidden by the
  walk's rebuilds and by pages recycled after the 11-17/18 mass delete.
- **Live contracts by mint day** (`pg_stats` histogram of `transfer_contract.create_time`, analyzed 13:50Z,
  2.69B rows):
  - about 27M kept rows from before 10-17;
  - 16–103M a day for 10-17..11-04;
  - 48–65M a day for 11-05..11-13;
  - 92M on 11-14;
  - 117–131M a day since.
- **Zero-byte share** (`TABLESAMPLE SYSTEM`, 1,300–1,700 contracts a window, by the pass's own tests short of
  its sweep, extender and escrow checks): 6% of 10-22..10-26, 49% of 10-28..11-01, 58% of 11-03..11-07 and
  of 11-11..11-15.

**The reaper** (`urnetwork_contract_horizon_*`):
- **The horizon pass is caught up at 35 days,** with a lag of 2.0–2.4 h, about its close settle time. It reads
  228 contracts a second and deletes 49: the cohort now turning 35 days old held only 16.5M contracts.
- **Its current pass began 11-22 23:11Z.** It has kept 21.05M under review, 26.6M closed late, 60,337 with an
  unsettled escrow and 1,602 busy.
- **The next pass** starts at the first caught-up run after 11-29 23:11Z: `RescanInterval` is 7 days, and the
  exp207 runbook changes no control.
- **Throughput:** 149M contracts deleted on each of 11-17 and 11-18, about 1,720 a second, in batches of 2,000
  that took 0.86–1.02 s. It is one worker, so the model uses **1,900 deletes a second**.

**Close evidence** (DBDISK-RETENTION-REVIEW §4):
- **Flow:** 1,927 rows in a second against 397 out, +38 GiB a day. At its 35-day window it heads for
  1.45–1.6 TiB by late December.
- **Capacity:** its retention carries at most about 65M rows a day at the default brake (1,000 a second for a
  third of `maintain_contract_partitions`' run).
- **From C,** the same task runs the day partitions first, which leaves about 26M rows a day: the cutover cuts
  its capacity about 2.5×.

**The usage archive:**
- The export control has been rolling since 11-22 09:05Z, mode on, 4 weeks a pass.
- Weeks 09-07..10-26 (335 GiB) are still on Main, waiting for the reviewed contracts to be reaped and for
  edge-6's export runs.
- A new row takes about 420 bytes. The 10-19 week's leaf: 327 bytes of heap and 95 of indexes a row.

**Key history** (DBDISK-RETENTION-REVIEW §3):
- Its retention has been stalled since about 11-21 18Z.
- New rows reuse about 410 GiB of free space inside it until about 12-12, then it grows about 21 GiB a day.
- The batch fix ships with the next taskworker build and adds about 185–240 GiB of room inside. The model takes
  its growth from about 12-21.

### 2.2 The model

`disk_model.py` (§8) steps an hour at a time from 11-23 15:00Z to 03-01.

**Contracts:**
- 1,600 contracts a second.
- **Before C,** the plain tables grow 1.7 KB per contract inserted, less what the reaper freed (the 11-21..23
  relation).
- **From C,** each day's leaf grows 1.9 KB per contract (1.8 KB with B2) and reuses nothing.

**The reaper and S1:**
- The reaper deletes up to 1,900 contracts a second, oldest first. Its two passes share the worker, half each
  while both have work. A contract the zero-byte pass reads and keeps costs a fifth of a delete.
- The kept rows go at the rescan after 11-29 23:11Z.
- **S1** retires a day N days and 6 h after it ends, once the reaper has passed it.
- **The legacy** is retired once the reaper has passed C and N days and 6 h have passed, plus 2 days for
  vacuum and compaction. With the companions this is 0.25 day, and its indexes are rebuilt on the days in §2.4.

**The archive:** each reaped contract adds 420 bytes. A week leaves once its close age passes `MinCloseAge`
plus 3.5 days of export cadence, and the reaper has passed it.

**Evidence:**
- Rows are kept by day: 1.2 rows per new contract, 296 bytes a row.
- Deletes run from the oldest day past its window, at the retention's capacity (§2.1). The brake is 2,500 a
  second in the task it shares, or 5,000 a second as its own task.

**Everything else:**
- Key history: +21 GiB a day from 12-21.
- All other tables: +25 GiB a day. This is the measured residual, database growth less the six tables, evidence
  and archive, and may be lower over months.

**Floor and timing:**
- The floor is 500 GiB.
- "Free space needed at C" = free space at C − the lowest free space between C and three days after the legacy
  returns + 500 GiB.
- The horizon cut takes effect 11-24 21:00Z, a taskworker roll after the edge-6 replay. The evidence window
  changes 11-25 12:00Z. C is 11-27 00:00Z.

The model leaves out WAL, the daily sawtooth of the tables that drop a day at 00:00Z, and any change in inflow.
Treat dates as ±3 days and sizes as ±15%.

### 2.3 Results: the horizon alone

Only the floor change. The evidence brake is raised to 2,500 a second, which is needed anyway, and its window
stays at 35 days. The archive floor stays at 42 days. The legacy waits for vacuum, and its indexes are not
rebuilt.

| N | Zero-byte | Free at C | Free needed at C | Short by | Lowest point | First day drops | Legacy back |
|---|---|---|---|---|---|---|---|
| 35 | on until C | 4.85 TiB | 11.5 TiB | 6.7 TiB | 01-03 | 01-02 | 01-03 |
| 28 | on until C | 4.85 | 9.6 | 4.7 | 12-27 | 12-26 | 12-27 |
| 21 | on until C | 4.84 | 7.6 | 2.7 | 12-20 | 12-19 | 12-20 |
| 18 | on until C | 4.84 | 6.9 | 2.1 | 12-17 | 12-16 | 12-17 |
| 14 | on until C | 4.84 | 6.3 | 1.4 | 12-15 | 12-14 | 12-15 |
| 35 | off now | 4.52 | 11.7 | 7.1 | 01-03 | 01-02 | 01-03 |
| 28 | off now | 4.70 | 9.6 | 4.9 | 12-27 | 12-26 | 12-27 |
| 21 | off now | 4.70 | 7.6 | 2.9 | 12-20 | 12-19 | 12-20 |
| 18 | off now | 4.70 | 6.9 | 2.2 | 12-17 | 12-16 | 12-17 |
| 14 | off now | 4.70 | 6.4 | 1.7 | 12-15 | 12-14 | 12-15 |

- **No horizon fits on its own.** The lowest point comes just before the legacy returns. Until then each day
  adds:
  - about 245 GiB of leaves;
  - about 45 GiB of archive rows, since the reaper's deletes now free nothing;
  - 25–40 GiB of evidence, whose retention the cutover slows;
  - 25 GiB of other growth.
- **Zero-byte reaping until C** leaves 0.1–0.4 TiB more at the lowest point. With C three days away, its
  cursor is still in October.

### 2.4 The companions

At N = 18, each companion added in turn:

| Step | Lowest free space | Gain |
|---|---|---|
| The horizon alone | −1,606 GiB (12-17) | |
| + the evidence retention as its own task, brake 5,000 a second (B′ in DBDISK-RETENTION-REVIEW) | −1,587 | +19 GiB now; it keeps evidence from growing about 30 GiB a day after C |
| + rebuild the legacy's indexes at C+7, C+12 and C+16. Each round returns the index share of what the reaper has deleted by then | 296 | +1,883 |
| + return the legacy 6 h after the reaper empties it: rebuild its primary keys, then compact it under a legacy-only bound above its 60k kept rows (§4.6) | 669 (12-15) | +373 |
| + B2 before the swap: no `open_source_id` index in the leaves, 1.8 KB a contract | 907 | +238 |
| + archive `MinCloseAge` at N + 7, 25 days | 1,490 | +583 |
| + close-evidence window of 14 days | **2,040 (12-13)** | +550 |

Every horizon with all of them: the evidence window is 14 days (or N if shorter). The legacy's indexes are
rebuilt in rounds from C+7, every 5–7 days, the last 2 days before the legacy is due.

| N | Zero-byte | Free needed at C | Lowest point | Margin over the floor | Legacy back | First day drops | Free on 01-15 | Free on 03-01 |
|---|---|---|---|---|---|---|---|---|
| 35 | on until C | 7.9 TiB | −2.5 TiB (12-30) | short 3.0 TiB | 01-01 | 01-02 | −0.5 TiB | −2.7 TiB |
| 28 | on until C | 6.0 | −0.6 TiB (12-23) | short 1.1 | 12-25 | 12-26 | 1.1 | −1.1 |
| 21 | on until C | 4.2 | 1.2 TiB (12-16) | 0.7 | 12-18 | 12-19 | 2.7 | 0.5 |
| **18** | on until C | **3.4** | **2.0 TiB (12-13)** | **1.5** | **12-15** | **12-16** | 3.2 | 1.0 |
| 14 | on until C | 3.1 | 2.3 TiB (12-13) | 1.8 | 12-13 | 12-14 | 4.3 | 2.1 |
| 21 | off now | 4.2 | 1.1 TiB | 0.6 | 12-18 | 12-19 | 2.7 | 0.5 |
| 18 | off now | 3.4 | 1.9 TiB | 1.4 | 12-15 | 12-16 | 3.2 | 1.0 |
| 14 | off now | 3.3 | 2.0 TiB | 1.5 | 12-14 | 12-14 | 4.3 | 2.1 |

### 2.5 Sensitivity of the plan (N = 18, all companions)

| Change | Lowest free space |
|---|---|
| the plan | 2,040 GiB on 12-13 |
| fresh leaves of 1.6 / 2.1 / 2.3 KB a contract | 2,478 / 1,355 / 878 |
| 1,800 contracts a second | 1,460 |
| the reaper at 2,500 a second | 2,031 |
| the legacy waits 2 days for vacuum | 1,757 (12-17) |
| **no legacy index rebuilds** | **187: short of the floor** |
| one rebuild round only, at C+12 / all rounds 2 days late | 1,696 / 1,918 |
| archive floor stays at 42 days | 1,486 |
| zero-byte contracts not archived | 2,406 |
| zero-byte pass off from now | 1,936 |
| other growth of 40 GiB a day | 1,734 (−401 on 03-01) |
| key history grows from 12-12 (no batch fix) | 1,996 |
| the horizon cut at C, not 11-24 | 2,042 |
| C on 11-26 / 12-01 / 12-08 | 1,952 / 1,751 / 1,424 |
| client_reliability at 9 days from 11-24 | 2,098 |

**Other horizons with all companions:**
- At N = 21 the lowest point is 1,201 GiB. It is short at 2.3 KB a contract (−86) and lands at 541 GiB at
  1,800 contracts a second.
- At N = 14 it is 2,323 GiB, and 1,250 at 2.3 KB.

### 2.6 Evidence

What the evidence window does (N = 18, every other companion, own task at 5,000 a second):

| Evidence window | Lowest point | Free on 03-01 |
|---|---|---|
| 35 days | 1,490 GiB | 268 GiB |
| 18 days | 2,027 | 1,046 |
| 14 days | 2,040 | 1,059 |
| 7 days | 2,040 | 1,059 |
| 14 days, still in the shared task | 1,938 | 195: it falls behind again once the backlog is gone |
| 35 days, shared task at 2,500 a second | 1,469 | −1,641: it grows about 30 GiB a day after C |

- **The evidence window should be no longer than the contract horizon.**
  - Evidence of a contract the reaper has deleted has nothing left to refer to.
  - Its readers need about a day (DBDISK-RETENTION-REVIEW §4.1).
  - The SN census reads none for a production-size window (`model/st_provider_close_originals.go:7-11`).
  - Admission accepts reports only for an unresolved contract (`db_contract_close_evidence_horizon.go:28-40`),
    and a dispute keeps its contract unresolved.
- **7 and 14 days are alike for the disk** until a C5 legacy drop or a rewrite exists.
  - Its deletes make room inside the file and return none.
  - 14 days already stops the file growing.
  - 7 days leaves more room inside, and about 0.75 TiB to return once C5's legacy drop exists.
- **What either window needs:**
  - the guard migration: the horizon guard's body hard-codes `interval '35 days'`;
  - the code floor;
  - the retention as its own task, because at C `maintain_contract_partitions` gives the day partitions its run
    first.
- **Without its own task, no brake keeps evidence current after C.** At the 5,000 cap the shared task carries
  about 131M rows a day, against 166M coming in *(inferred from DBDISK-RETENTION-REVIEW §4.3)*.

### 2.7 After the transition

- **The steady state.** On daily partitions every contract is held for N + 0.25 to N + 1.25 days, and zero-byte
  reaping cannot shorten that: a day returns its space only whole. With the companions, at 1,600 contracts a
  second and 1.8 KB a contract, that is 3.5 TiB of leaves at 14 days, 4.4 at 18, 5.0 at 21 and 8.2 at 35. The
  plain tables hold 4.5 TiB today. With the zero-byte pass caught up, they would hold about that at 35 days.
- **Raising the horizon from 18 on 12-20**, with the archive floor at N + 7, leaves this free on 03-01:

  | Raised to | Free on 03-01 |
  |---|---|
  | 21 days | 0.5 TiB |
  | 28 days | −1.1 TiB |
  | 35 days | −2.7 TiB |

  35 days on daily partitions needs the ≥35% cut in contract volume the owner made C's premise on 11-18, or
  new disk.
- **The long decline.** Even at 18 days free space falls about 45 GiB a day from late December: key history
  +21 GiB a day until it is rewritten, and the other tables +25. That is outside C1. It sets the date by which
  the next items of DBDISK-RETENTION-REVIEW §6 must land.

### 2.8 Without a cutover, for comparison

| Plain tables | Lowest free space by 01-15 | Free on 03-01 |
|---|---|---|
| 35 days, zero-byte on, evidence 35 days at 1,000 a second (today) | 569 GiB | −2.5 TiB |
| the same with the evidence brake at 2,500 | 1,197 | −0.7 TiB |
| 35 days, zero-byte on, evidence 14 days at 5,000 a second | 2,004 | 0.1 TiB |
| 35 days, zero-byte off, evidence 35 days at 2,500 | −1,332 | −3.2 TiB |
| 18 days, zero-byte on, archive 25 days, evidence 14 days | 2,901 | 0.6 TiB |

**The cutover saves no disk in the next three months; its gains are structural.**
- With the companions it bottoms out in mid-December at 2.0 TiB. The plain tables would bottom out at 2.9 TiB
  at 18 days, or 2.0 TiB at 35 days with evidence at 14.
- By 03-01 it ends a little ahead: 1.0 TiB against 0.6 and 0.1. Zero-byte rows held in the archive for 42 days
  cost the plain tables more than the transition costs.
- **The structural gains:**
  - drop-based retention, with no index bloat and no rebuild cycle;
  - no multi-TiB vacuums or anti-wraparound passes over the contract tables;
  - today's bloat leaves with the legacy partition.

## 3. Who needs contracts older than N days

Read from the code on 2026-11-23 (server main f09d3edd6). File and line references are to that tree.

### 3.1 The one hard limit: SN25 amended epochs

- **The read.** From epoch 8 (about 11-27), each epoch's first build reads every contract that closed in its
  7-day window through `stEpochAmendedUsageSql` (`model/st_extender_pool.go:185-226`). Every rebuild before
  finalization reads them again.
- **What it needs.** A contract with bytes needs its **live** `transfer_contract` row for the usage origin
  network, the payer and the create time. The archive does not carry them (`db_provider_usage_archive.go:42-47`).
- **What fails.** An archived contract with bytes fails the whole epoch: "epoch %d must be built within the
  contract horizon" (`st_extender_pool.go:97-105`).
- **The relay read.** `model/st_extender_relay.go:270-335` reads live `contract_extender` rows. A reaped
  contract drops out of it silently.
- **Timing:**
  - an epoch is recorded about 9 days after its window opens;
  - rebuilds run until finalization, 14,400 blocks or about 48 h (`controller/st_ledger_v2_build.go:250-282`,
    `controller/st_ledger_v2_driver.go:40`);
  - the archive's SN floor budgets a chain halt of up to a week (`db_provider_usage_archive_export.go:385-392`).
- **Slack** between an on-time first build and the horizon is N − 9 days: 26 at 35, 19 at 28, 12 at 21,
  **9 at 18**, 5 at 14. For the last rebuild before finalization it is N − 11 days: 7 at 18.
  - Only 14 falls below the week the code budgets for a halt, so **a 14-day horizon can fail a whole epoch** if a
    build is late.
  - Removing the limit needs a migration that archives the origin network, payer and create time with the usage
    proof, and a matching change to the relay read.
- **Offline replays** (`controller/st_ledger_v2_extender_replay.go`) work only for epochs whose window opened
  less than N days ago.

### 3.2 Protected by the reaper's keep rules at any horizon

| Reader | What it needs | Keep rule | 28 / 21 / 18 / 14 |
|---|---|---|---|
| Payment holds, incomplete, recent and reviewed payments (`model/contract_horizon_reaper.go:1243-1295`, `db_contract_retention.go:49-83`), and the review export and clear (`usagearchive/review*.go`) | sweeps of their payments, at any age | payment_hold, payment_incomplete, payment_recent, payment_review | unaffected: every such contract is from the USDC era and already older than 35 days |
| Operator block points (`model/operator_block_points.go:422-439`, `:626-665`) | unpaid sweeps until the block's award, at most about 10.5 days (`ClientReliabilityRetention`, 10 days) | block_points | unaffected |
| Net escrow witness and reconcile (`model/subscription_model.go:654-671`); balance retention (`model/subscription_balance_retention.go:47-60`) | unsettled escrows | unsettled | unaffected |
| Disputes (`model/subscription_model.go:4575-4584`) | contracts with no outcome | open, unresolved | unaffected |
| Late closes (expiry repair, outages) | a contract whose close falls inside the horizon | recent_close. The pass reads a contract only `CloseSettleTime` (2 h) past the horizon; one that closed later waits for the weekly rescan | unaffected; such a contract stays up to a week past N |
| Points leaderboard rollup (`model/points_leaderboard_rollup.go:92-160`) | sweeps of USDC payments | payment rules; it falls back to payout weeks once they are reaped | unaffected |

- The weekly payout run no longer pays per contract. USDC payouts ended, and the planner, sender and
  `payouts sn-reconcile` were removed on 2026-11-12 (d923f1505).
- The reaper's comments on the sn-reconcile floor (`contract_horizon_reaper.go:147-148`) and on /transfer/stats
  (`:160-163`, `:296-298`) are stale.

### 3.3 Degraded: provider stats

`model/provider_model.go` serves windows of up to 366 days, and 90-day routes (`:399-410`, `:626-635`):
- bytes are read by `close_time` from the contract and its closes, so a 90-day view shows N days of bytes, and
  only kept rows before that;
- contract counts already lose zero-byte contracts after 8 days;
- raising the horizon later does not bring the deleted rows back.

Nothing else degrades.

### 3.4 Unaffected for other reasons

| Reader | Why it is unaffected |
|---|---|
| /transfer/stats | It reads `account_payment` only, since 8d1405adb (`model/account_payment_model.go:208-238`) |
| Contract stats | They read the trailing 24 h (`model/contract_stats_model.go`) |
| The transfer audit rollup | It reads 3 days (`taskworker/work/audit_work.go:244`). Its first day follows `ContractHorizonFloor` on its own (`model/audit_provider_sweep_model.go:1084-1101`), and it never rewrites a written day |
| /stats/last-90 and the public clock | They read audit rollup rows |
| Prober shards | They keep 8 generations (`model/prober_shard_retention.go`) |
| The force-close sweep, deadline closes, the open-byte and balance reads, provider-work receipts | They read open contracts only (`model/subscription_model.go:4889-4960`, `:7068-7095`; `taskworker/work/contract_deadline_close.go:66`) |
| Degradation, zero carry, extender stats, bandwidth | They read 72 h or less |
| SN epochs before the amendment | They read live rows and the archive in one snapshot (`model/st_provider_usage.go:54-80`), so they need only the archive's window |

### 3.5 The usage archive and its export

- **Why the floor is 42 days.** `usagearchive.MinCloseAge` is the lowest reaper horizon (35) plus its weekly
  rescan (7) (`usagearchive/eligibility.go:33-40`; owner, 2026-11-21, down from 70). It keeps a week's
  late-closing contracts out of the DEFAULT partition, which never leaves.
- **Epochs not yet recorded** are protected separately, by the SN floor (`db_provider_usage_archive_export.go:385-453`).
- **No horizon needs a different floor.** N + 7 days follows the same reasoning. Every day the horizon is cut
  adds a day of archive rows on Main until the floor moves too.

### 3.6 Where 35 days is written down

- **Must change for any N below 35** (code only, no migration):
  - `model.ContractHorizonFloor` (`model/contract_horizon_reaper.go:164`), with `horizon()` (`:284-286`),
    `contractHorizonFromDays` (`:382-392`) and the metric help (`:680`);
  - `server.ContractDayRetentionFloor` (`db_contract_day_retirement.go:65`) and `ContractDayRetention`
    (`:130-136`). If only the reaper's floor moves, S1 keeps every emptied day until 35 days and 6 hours, and
    the cut saves nothing after C.
- **Follows the floor on its own:** `TransferAuditRollupFirstDay`.
- **Stays at 35 days unless a migration changes its guard:**
  - Close evidence. The horizon guard migration hard-codes `interval '35 days'` (`db_contract_close_evidence.go:105-132`,
    `db_migrations.go:11218-11226`). `closeEvidenceGuardFloorOf` (`db_contract_close_evidence_horizon.go:120-125`)
    returns 35 only for that exact body and 91 for any other, so a mismatch fails safe. The monitor's contract
    (`monitor/migrations_close_evidence.go:82-86`) must match.
  - The C4 work-session journal (`db_provider_work_session_partition.go:92`, `:101-107`, `:194-212`).
- **The archive floor:** `usagearchive.MinCloseAge` (`usagearchive/eligibility.go:40`). Code only. The exporter
  on edge-6 runs from a server checkout, so it needs the same build.
- **Monitor and dashboards:**
  - §2.4r (`monitor/signal_contract_horizon_reaper.go:332-350`) warns while the reaper drains a cut's backlog.
    That is expected. Its "21 days and 13 hours" text (`:357`) is stale.
  - §2.4i still judges a day at 91 + 14 days (`monitor/signal_contract_partition_retention_blocked.go:45-52`).
  - The taskworker dashboard's panels 53, 63 and 89 say "35 to 91" (`grafana/dashboards/taskworker.json:68`,
    `:80`, `:87`).
- **Tests that pin 35 or 42:**
  - `model/contract_horizon_reaper_horizon_test.go`
  - `model/contract_horizon_reaper_floor_db_test.go`
  - `model/contract_day_retirement_db_test.go:198-215`
  - `db_contract_day_retirement_test.go:52`, `:82`
  - the close evidence horizon tests
  - the C4 partition tests
  - `usagearchive/usagearchive_test.go:121-158`
  - `db_provider_usage_archive_export_test.go`
  - `db_provider_usage_archive_floor_test.go`

### 3.7 Kept rows, the day compaction and the legacy

**How S1 retires a day** (`db_contract_day_retirement.go:471-578`):
1. **Expiry:** the day's upper bound + N + 6 h.
2. **Statistics gate:** a leaf with more than 1M live rows by the statistics waits, with no read and no lock.
3. **Count:** the live rows along the primary key, up to 10,001, within 5 s.
4. **Compaction:** CLUSTER on the primary key, after a lock wait of at most 1 s and with 500 ms of work. It
   copies every live and recently dead row version with its transaction ids. So every snapshot, the epoch
   reader's REPEATABLE READ included, sees the same rows afterwards.
5. **Drop:** once all six leaves hold no heap or TOAST bytes, read again under ACCESS EXCLUSIVE.

**After C, days keep few rows.** The payment rules cannot keep a post-cutover contract, and block points stop
binding after about 10.5 days. Contracts minted since 10-29 close in a median of 2–2.6 minutes. So a day keeps
a few rows, far under 10,000 a leaf.

**The legacy partition** is one range that ends at C (`db_partition_registry.go:743-745`). It expires at
C + N + 6 h, then:
- holds the 21.05M reviewed and 26.6M late-closed contracts until the pass after 11-29 23:11Z, so it fails the
  statistics gate;
- holds 60,337 unsettled contracts for good, which fail the 10,000-row bound;
- cannot finish the 5 s count or the 500 ms CLUSTER while its key holds billions of dead entries.

In every one of these cases it stays attached, readable and full size. Later days are not held up: each range
retires on its own, oldest first (`:454-467`).

## 4. Runbook

Read-only checks on 11-23 between 14:50Z and 15:16Z: Main's catalog and `pg_stat_*`, Mimir, the 11-17 rehearsal
logs (`temp/contract-cutover-1117/logs/`, `temp/c1daily-1117/logs/`) and the code. The 11-17 runbook
(`temp/contract-cutover-1117/RUNBOOK.md` §5) stays the base. This section says where today differs from it.

### 4.1 What must be deployed first

**The cutover as built needs no service deploy and no migration.**
- Every running build carries the daily rework (a1d4cf7c4) and B1/B3 (06614fa99):
  - api, mcp and proxy (20 of 20) from b896cf6d9;
  - the taskworker (8 of 8) from 81287a91a;
  - connect from b896cf6d9;
  - gossip from ff9c9f996;
  - competitionworker from c07a2ea4e.
  alt was retired on 11-20.
- Main's migration head is 1008, the same as main. The cutover adds no migration.
- Only bringyourctl has to be built (RUNBOOK §5.2).

**The plan needs one server branch, tested and merged, and one taskworker build.** The branch:
1. **The horizon floors.** Set `model.ContractHorizonFloor` and `server.ContractDayRetentionFloor` to 14 days,
   the lowest the control may then set; the control picks 18. Update the tests and texts in §3.6.
2. **The evidence retention as its own task** (B′): taskworker.
3. **`usagearchive.MinCloseAge` = 25 days** (N + 7): the taskworker's drop fence and the edge-6 exporter.
4. **A legacy compaction for an operator** (§4.6): S1's admission, count and CLUSTER on the six legacy leaves,
   with a bound of about 100,000 live rows and a statement bound of a few seconds. In bringyourctl.
5. **The key-history batch fix**, already in flight (DBDISK-RETENTION-REVIEW §3.5 A), rides the same
   taskworker build.

**Optional before C:**
- decision 5's bound on `GetOpenTransferByteCount` and `GetOpenContractIds` (api, connect, taskworker);
- the evidence window, if the owner shortens it: a guard migration (`db migrate`), the code floor and the
  monitor's contract.

**Then the controls:**
- `horizonDays` 18 in `{contract_horizon}control`. The reaper reads it before every batch.
- `maxRowsPerSecond` 5,000. The worker itself caps near 1,900.
- `zeroByteMode` stays on until C, then off.
- The evidence brake at 5,000 a second, once its task is its own.

**Run finalize and rollback on a host on Main's network, not on this Mac.**
- `contractPartitionSwap` (`db_contract_partition_statistics.go:86-137`) sends each statement in its own
  round trip after `LOCK TABLE`: about 114 round trips. Rollback sends about 250
  (`db_contract_partition_rollback.go:84-97`).
- The round trip from this Mac to edge-2 measured 69–225 ms today. That would hold ACCESS EXCLUSIVE on all six
  tables for 8–25 s for the swap and 17–57 s for a rollback.
- Every application statement gives up after `PgLockTimeout` (2 s).
- **Two ways out:**
  - a host on Main's network, as usagearchivectl runs on edge-6: no code change;
  - the statements batched into one round trip, as `db_table_swap.go:980` does. That is code, with DB tests.
- Prepare and abort hold their locks for one or two round trips and are fine from this Mac.

### 4.2 Preconditions now

| # | Precondition (RUNBOOK §5.0) | Status, 11-23 about 15:15Z |
|---|---|---|
| 1 | Main includes ab2d3ed87 and a1d4cf7c4; bringyourctl is built from it | Code yes; no change since a1d4cf7c4. Binary not built |
| 2 | B1 day bounds in every service, B3 in the taskworker | Yes (§4.1) |
| 2b | **B2:** `transfer_contract_open_source_id` dropped | **No.** Details below |
| 3 | The osid trigger is on transfer_contract | Yes: `transfer_contract_origin_linger`, enabled |
| 4 | The index walker is finished or paused | No. Two transfer_contract rebuilds are left (destination_id_close_time 219 GB, destination_id_create_time 218 GB), 75–87 min each, from 22:30Z, with a hard stop at 11-24 13:00Z |
| 5 | Deploy locks are free, and no release is rolling | The locks are free. A CPUOPT7 R2 proxy roll (about 4 h) is staged; it must not overlap the swap |
| 6 | gate.sh prints GATE OK | WAIT: a backup line, because the edge-6 replay counts as a backup in progress (`urnetwork_backup_onsite_in_progress{archive="pg"}` 1). No transaction runs over 60 s. Needs a waiver until the replay ends |
| 7 | The contract-table gate (RUNBOOK §5.1) passes | Fails now: transfer_contract, transfer_escrow and contract_close are under autovacuum |
| 8 | PgBouncer is healthy | Yes: 0 clients waiting now, 21 at most in the last hour |
| 9 | A fresh backup point | **No.** Details below |
| 10 | Outside the maintenance, nightly, payout and full-copy windows | §4.5 |

**B2:**
- The index is 323 GiB and valid.
- Its `idx_scan` has been flat at 6,095,490,834 since 09:29:54Z today, but it is 15.07M higher than on 11-20.
  That was the api's companion-origin read switching to it after an autoanalyze while the source-pair partial
  was bloated; the pair partials are back to 5.1 and 4.9 GB.
- The scheduled drops were cancelled on 11-20 at about 19:21Z.
- `osid_drop.sh` needs a flat 24 h (from about 11-24 15:10Z) or `FORCE=1`, GATE OK and a vacuum gap.
- **After the swap**, PostgreSQL refuses a concurrent drop of a partitioned index. So the held record
  (`DROP INDEX CONCURRENTLY IF EXISTS`, branch `feat/open-source-id-drop-record-1116`) would fail
  `db migrate`, and every daily leaf would get its own copy of the index.

**The backup point:**
- The last published PostgreSQL backup is the copy of 11-10 21:15Z.
- The 11-19 copy replays until about 11-24 20:16Z, then dumps and publishes, about 11-26 07:30Z.
- `archive_mode` is off, so there is no point-in-time recovery.

**Autovacuum:**
- transfer_contract's current pass began 13:51Z, 71 s after the previous one ended, and was still in its heap
  phase at 15:16Z.
- It carries 23M dead tuples against a 5M threshold, and updates add about 1,900 a second. So a pass is due
  every 45 min or so, and each runs over 70 min.
- Every cutover step's admission waits for a moment with no autovacuum on its tables
  (`db_contract_partition_admission.go:179-192`). The swap needs all six at once.

### 4.3 The steps

| # | Step | Statements | Locks | Duration | Undo |
|---|---|---|---|---|---|
| 0 | The branch, the taskworker roll and the controls (§4.1) | — | — | about a day to build and test; the roll about 40 min with its gates | `horizonDays` back up at any time. Deleted rows do not come back |
| 1 | **B2:** `ROUTINE=wait\|cancel bash osid1116/scripts/osid_drop.sh`, then merge the held record | `DROP INDEX CONCURRENTLY` | SHARE UPDATE EXCLUSIVE on transfer_contract; lock waits bounded | minutes, in a vacuum gap | rebuild it concurrently: hours |
| 2 | **Prepare:** `bringyourctl model migrate contract-partition --cutover-day=2026-11-27 --dry-run`, then the RUNBOOK §5.3 loop until "prepared" (retry every ~15 s, not 120) | per table: `CREATE TABLE <t>_p (LIKE <t> INCLUDING …) PARTITION BY RANGE (contract_id)` and leaves C..C+3; `ALTER TABLE <t> ADD CONSTRAINT <t>_legacy_bound CHECK (contract_id < '<floor(C)>'::uuid) NOT VALID`; `ALTER TABLE <t> VALIDATE CONSTRAINT <t>_legacy_bound` | the bound: ACCESS EXCLUSIVE, catalog only, ≤1 s wait. VALIDATE: SHARE UPDATE EXCLUSIVE, ≤5 s wait; reads and writes go on, and its scan holds the xmin horizon | rehearsal 1.1–1.9 s. On Main the scans take 30–33 min (transfer_contract 12.5–13.8, contract_close 5.7–6.2, transfer_escrow 5.4–5.9, sweeps 3.4–3.8, reports 2.8–3.1 min), up to about 1 h under load, each in its table's vacuum gap. VALIDATE writes no WAL | `--abort`: per table, ACCESS EXCLUSIVE ≤1 s, 24 tries 5 s apart |
| 3 | **The swap**, on a host on Main's network: taskworker stopped through the settlement gate (`ST_QUIET_MIN=0 pg187/tw.sh stop`, about 1.5 min), `--finalize --dry-run`, `--finalize` | the attach probe, in a rolled-back transaction; then one transaction: `LOCK TABLE` the six tables and their staged parents `IN ACCESS EXCLUSIVE MODE`; drop `contract_close_report_contract_id_fkey`; per table drop the triggers, rename to `<t>_legacy` and the indexes to `_l`, `ALTER TABLE <t>_p ATTACH PARTITION <t>_legacy FOR VALUES FROM (MINVALUE) TO ('<floor(C)>')`, rename `<t>_p` to `<t>`, recreate the triggers, copy the statistics; then `ANALYZE ONLY` the six parents | ACCESS EXCLUSIVE on all six and the staged parents: ≤1 s wait, 30 s statement bound, up to 60 tries 10 s apart | rehearsal (1M contracts, 18.7): wait 4 ms, held 56–78 ms (241 ms in the daily DB test), finalize 1.6–2.0 s. Main from its own network: held ≤0.5 s *(inferred: about 840 backends take the invalidations)*. ANALYZE ONLY 1–3 min | `--rollback` until a row lands in day C: about 0.3 s of locks from Main's network. It restores the FK `NOT VALID`, and the monitor pages until it is validated, a scan of about 2.8B rows |
| 4 | **Verify** (RUNBOOK §5.5), restart the taskworker, watch 60 min | read only | — | 1 h | roll back on RUNBOOK §5.5's triggers |
| 5 | **C at 00:00Z:** `zeroByteMode` off | — | — | — | none once the first contract lands in day C. Decide by C−15 min |
| 6 | **The task's work from C:** days ahead (3), analyze, S1 | ATTACH of new days; `ANALYZE ONLY`; S1's CLUSTER and drops | ATTACH: SHARE UPDATE EXCLUSIVE, 5 s; S1: ACCESS EXCLUSIVE ≤1 s + 500 ms | compaction and drop of an emptied week: 204 ms (rehearsal) | — |

**Prepare must not outlive C.**
- The legacy bound refuses every contract id from C. A prepare that is neither finalized nor aborted by C
  therefore stops contract creation at 00:00Z.
- The task abandons a stale prepare from C − 6 h (`db_contract_partition_maintenance.go:172`), but each attempt
  waits for its own vacuum gap.
- So: finalize early, or abort by hand by C − 12 h. Have the owner's go to cancel a routine autovacuum for that
  abort.
- The monitor's §2.4j warns from C − 24 h.

**The swap needs all six tables free of autovacuum at once.** Today that moment rarely comes. Choices:
- with the owner's go, let the swap cancel routine passes, as the walker's `ROUTINE=cancel` does. Anti-wraparound
  passes are waited out;
- or retry until a gap comes.

### 4.4 Service interplay

**Writers and reads:**
- **The taskworker** stops only for the swap, about 10–15 min. That pauses the reaper, the zero-byte pass,
  partition maintenance (with the abandon) and the debit flush. Keep the stop out of the payout window (Sat
  22:00Z – Sun 16:00Z) and away from the epoch-7 close (11-27 18:29Z + 4 h).
- **api, connect and proxy writers** queue for the lock wait plus the hold, ≤1.5 s from Main's network, which
  is under the 2 s bound. Each plan is invalidated once.
- **Hot reads (B1):**
  - until 23:30Z on C − 1 they plan the legacy leaf alone;
  - until 01:30Z they plan it with day C;
  - then they plan day C alone.
- **Reads without a contract id bound** plan 5 leaves after the swap, and about 23 at the 18-day steady
  state.
- **The validator:** gate.sh opens about 25–57 min after each 72-minute epoch step (steps at about 14:20:23Z +
  k × 72 min).

**Configuration and scripts:**
- **xops:** `playbook-dbs.yml:1940-1955` (`autovacuum_tuning`) names contract_close, transfer_contract,
  transfer_escrow and transfer_escrow_sweep, and would error on partitioned parents. Move the entries to
  `<t>_legacy` and the newest leaf.
- **Operator scripts that read the parent's relid** go blind to the leaves' vacuums after the swap: the walker
  (`rebuild_walk6.sh:147`, `:424`, `:430`), `osid_drop.sh:47` and `:116`, `osid_trigger.sh:66-68`, and
  `exp207/gates.sh:43`. `db reindex-index` refuses the partitioned names, so give it the `_l` names. gate.sh and
  the RUNBOOK §5.1 query already read the leaves.

**Monitoring:**
- **Monitor:** §2.4j warns from C − 24 h until finalized. §2.4h and §2.4i start after the swap. The close-report
  foreign key must stay absent.
- **Grafana:** no panel reads these tables, and the LockManager panel and alert asked for in RUNBOOK §6 do not
  exist yet.

**Backups and WAL:**
- The schedule is held until it is put back by 11-29. The first run after that is Sun 11-29 or Thu 12-03 at
  06:00Z. The full copies of 12-17 and 12-20 already sit at the 96-h replay bound.
- The cutover steps write almost no WAL. The reaper's backlog (11-24 to about 12-15) and the legacy's rebuilds
  do.
- Lower the reaper's rate during a copy phase (`maxRowsPerSecond`). Each day of lower rate returns the legacy
  correspondingly later.

### 4.5 Schedule: the earliest safe start

| When (UTC) | What | Gates |
|---|---|---|
| 11-23 evening | Nothing on the contract tables. The walker runs from 22:30Z; the 21.05M reviewed contracts wait for the pass after 11-29 23:11Z | — |
| 11-24 daytime | The branch of §4.1, tested through test-env.sh (local-pg resolving to 10.213.0.1), merged; bringyourctl built for Main's network | — |
| 11-24 after 20:16Z | The taskworker build and roll, then `horizonDays` 18 and the evidence brake | The edge-6 replay ended; the walk's hard stop (13:00Z) passed; settlement and payout gates |
| 11-25 | B2: the osid drop in a vacuum gap, then the record merged | A flat 24 h from about 11-24 15:10Z; the owner's choice of `ROUTINE` |
| 11-25 (each run must start before 11-26 00:00Z) | Prepare, `--cutover-day=2026-11-27` | Vacuum gaps; outside 17:30–19:30Z |
| 11-25 | The evidence window migration, if decided | Owner |
| **11-26, 07:30–17:30Z** | **The swap** on Main's network. The abandon line is 18:00Z | The 11-19 copy published (precondition 9); an autovacuum gap on all six tables (or the owner's go to cancel routine passes); no proxy roll |
| **11-27 00:00Z** | **C.** `zeroByteMode` off | — |
| 12-04, 12-09, 12-13 | Rebuild rounds of the legacy's indexes (C+7, C+12, C+16) | The walker; vacuum gaps on the legacy leaves |
| About 12-15 | The legacy returned (§4.6) | The reaper past C |
| 12-16 06:00Z | The first day drops | — |
| About 12-22 | Review a week of daily leaves. Raise to 21 days if they are 1.9 KB a contract or less | Owner |

- **End to end:** about 3.5 days to C, 3 weeks to the legacy's return, and about a month to the steady state.
- **C = 11-26** is possible, with the swap on 11-25 before 18:00Z, if the owner waives the backup point. The
  model changes little (§2.5).
- **C = 11-25** would need prepare tonight before 00:00Z and the swap tomorrow morning, before the companions
  exist. Not recommended.

### 4.6 After C: returning the legacy

1. **Rebuild rounds at C+7, C+12 and C+16.**
   - Each round runs `db reindex-index` on the legacy's large indexes, smallest first, through the walker:
     - transfer_contract: destination_id_close_time, destination_id_create_time, closed_usage, pkey;
     - transfer_escrow: balance_contract, pkey;
     - contract_close: pkey;
     - transfer_escrow_sweep: pkey, payment_contract, sweep_time_covering;
     - contract_close_report: pkey.
   - Each rebuild holds SHARE UPDATE EXCLUSIVE on its leaf, and the xmin horizon for its heap scan (about 26 min
     per transfer_contract index).
   - The reaper's deletes keep the legacy leaves under autovacuum, so the walker cancels routine passes and
     waits out anti-wraparound ones. Rounds that slip 2 days cost about 120 GiB at the low point; a single round
     at C+12 costs about 340 GiB (§2.5).
2. **When the reaper has passed C** (about 12-15 at 18 days):
   - Rebuild the six legacy primary keys (about 20 min for transfer_contract's), so that S1's count and the
     compaction read only live entries.
   - Then run the legacy compaction (§4.1, item 4). It returns the rest of the legacy, about 2.7 TiB after
     the rounds' 1.8 TiB.
   - Unbounded reads that touch the legacy wait for its locks, so run it with decision 5's bound shipped, or in
     a quiet minute.
3. **The legacy then holds about 60k unsettled contracts,** about 100 MB. It drops once they are settled or
   exported (decision 9).

## 5. Risks

| # | Risk | Effect | Watch, or what to do |
|---|---|---|---|
| R1 | **A prepare neither finalized nor aborted by C** | The legacy bound refuses every new contract from 00:00Z | Finalize early. Abort by hand by C − 12 h if the swap has not happened, with the owner's go to cancel a routine autovacuum |
| R2 | **The swap or a rollback run from this Mac** | 8–25 s (rollback 17–57 s) of ACCESS EXCLUSIVE on all six tables: contract statements fail after 2 s and PgBouncer pools fill | Run both on Main's network, or batch them first (§4.1) |
| R3 | **The legacy's index rebuilds blocked by its vacuums** | Each round that slips loses margin. With no round the lowest point is 187 GiB, under the floor | Start each round on time and keep the walker free from C+6. Cancel routine passes on the legacy leaves |
| R4 | **Fresh leaves larger than 1.9 KB a contract** (their index churn is estimated, §1.2) | At 2.3 KB the lowest point is 878 GiB | Measure the first full day's six leaves at C+1 (`pg_total_relation_size` of the `_p<C>` leaves). Above 2.1 KB, shorten the horizon by a few days at once |
| R5 | **Inflow rises:** bot sweeps, or the trim undone | At 1,800 a second the lowest point is 1,460 GiB. The reaper deletes about 1,900 a second, so above about 1,850 days retire late, and each day of lag costs about 245 GiB | `urnetwork_contract_horizon_lag_seconds` and the daily opened rate. Then a shorter horizon or a second reaper worker (decision 13) |
| R6 | **The legacy stays:** its 60,337 unsettled kept contracts, a compaction cut off by its bound, or a key not rebuilt | Its last 2.7 TiB never comes back. Free space reaches the 500 GiB floor about 01-11 (with the rebuild rounds) or 12-13 (without them) | Decision 9 and the legacy compaction of §4.6, as a rehearsed operator step |
| R7 | **An SN25 amended-epoch build late by more than N − 9 days** | The whole epoch fails | 18 days leaves 9 days of slack. Watch each epoch's recording against its window's age |
| R8 | **Evidence after C** | `maintain_contract_partitions` runs the day partitions first, so the retention falls about 2.5× and evidence grows about 30 GiB a day for good | Its own task (B′) before C |
| R9 | **Reaper and WAL load** while the backlog drains: about 1B contracts, six tables and an archive insert each | WAL above today's 45–55 MiB/s, more autovacuum, longer replays of the next copy | Lower the reaper's rate during copy phases. Watch `pending_task` dead tuples, debit lag and taskworker overdue |
| R10 | **No fresh backup point before 11-26 07:30Z** | No restore newer than 11-10 if the swap went badly wrong | Swap after the 11-19 copy publishes (the recommended schedule) |
| R11 | **The archive export stalls** (edge-6 busy, or a failed export) | The archive keeps every reaped row, about 45 GiB a day | The weekly export's logs. The old weeks (335 GiB) wait for the review reap of about 11-30 |
| R12 | **The other tables' slow decline:** key history +21 GiB a day from about 12-21, the rest +25 | Free space falls from about 3.2 TiB on 01-15 to about 1.0 TiB on 03-01 | DBDISK-RETENTION-REVIEW §6: the key-history fix and rewrite, client_reliability, evidence's C5 legacy drop |

## 6. Recommendation

**Cut over now: C on 11-27, the swap on 11-26, with an 18-day transition horizon set on 11-24. Stay at 18–21
days afterwards.** Go only with the companions.

1. **One taskworker build on 11-24,** carrying:
   - the floors at 14 days;
   - the evidence retention as its own task;
   - `MinCloseAge` at 25 days;
   - the legacy compaction;
   - the key-history fix.
2. **B2 before the swap.**
3. **The swap on Main's network,** after the 11-19 copy publishes.
4. **The legacy's index rebuilds** at C+7, C+12 and C+16.
5. **The legacy's key rebuild and compaction** once the reaper has emptied it, about 12-15.

**What that gives:** the lowest free space is about 2.0 TiB on 12-13, against 5.0 TiB at C, and SN25 amended
epochs keep 9 days of slack.

**Against the owner's three candidates:**
- **35 and 28 days** do not fit this volume, during the transition or after it.
- **21 days** fits only with every companion, by 0.7 TiB, and not at 2.3 KB a contract.
- **14 days** leaves the most disk, 1.8 TiB over the floor, but leaves the SN25 amended epochs 5 days of slack,
  under the code's own halt margin.

**Afterwards:** measure a week of daily leaves, then go to 21 days if they are 1.9 KB a contract or less. That
gives SN25 12 days of slack. 35 days needs the ≥35% volume cut or new disk.

### Decisions for the owner

| # | Decision | Choices | Recommended |
|---|---|---|---|
| 1 | **The transition horizon** | (a) 18 days; (b) 21; (c) 14; (d) 28 or 35 | **(a) 18.** (b) is safer for SN25 but thin on disk; (c) is best for disk but can fail an SN25 epoch; (d) does not fit |
| 2 | **When the horizon is cut** | (a) on 11-24, with the taskworker roll; (b) at C | **(a).** The plain tables reuse what it frees until C, and the reaper starts the backlog three days sooner |
| 3 | **After the transition** | (a) 21 days, once a week of leaves is measured and the legacy is back; (b) stay at 18; (c) back to 35 | **(a).** (c) needs the volume cut or new disk (§2.7) |
| 4 | **The close-evidence window** (asked today) | (a) 7 days; (b) 14; (c) N; (d) 35 | **(a) or (b), never longer than N.** 7 days leaves the most room for a later C5 drop, and 14 stops the growth. Either needs the guard migration and decision 7 |
| 5 | **Reads without a contract id bound** (CONTRACT-PARTITION.md decision 5) | Settled 11-18: bound `GetOpenTransferByteCount` and `GetOpenContractIds` by the floor. Not built. (a) build it before C; (b) accept the cost for the transition, about 23 leaves at 18 days, and build it before the legacy's compaction | **(b),** unless (a) can land by 11-25. At 23 leaves the cost is about half of the measured 42-leaf case, under 1% of a core *(inferred)* |
| 6 | **The constants: 3 days ahead, 6 h past the horizon** (decision 6) | Settled 11-18: keep them | **Keep.** The 6 h still covers the 2-hour close settle and the rescan lag at 18 days |
| 7 | **The evidence retention as its own task** (B′) | (a) before C; (b) after | **(a).** Without it, evidence grows about 30 GiB a day after C at any brake |
| 8 | **The archive's `MinCloseAge`** | (a) N + 7, 25 days, while the horizon is short; (b) 42 | **(a).** It keeps the 35 + 7 reasoning, the SN floor still guards epochs not yet recorded, and it adds about 0.55 TiB at the low point |
| 9 | **The legacy's 60,337 unsettled kept contracts** | (a) compact the legacy around them (§4.6), drop it later; (b) settle or export and clear them, like the reviewed contracts; (c) keep the legacy whole | **(a) for the cutover, then (b) for the drop.** (c) leaves 4.6 TiB on disk for good |
| 10 | **The legacy's index rebuilds** (the "weekly legacy-partition index rebuilds" of the 11-18 premise) | (a) C+7, C+12, C+16 with the walker, cancelling routine passes on the legacy leaves; (b) none | **(a).** The plan does not fit without them |
| 11 | **Where the swap runs** | (a) a host on Main's network; (b) batch the statements first (code); (c) this Mac | **(a) now, (b) later.** (c) holds every contract table's lock for 8–25 s |
| 12 | **Autovacuum at the swap and at an abort** | (a) cancel routine passes on the six tables (rule (a), `ROUTINE=cancel`) and wait out anti-wraparound ones; (b) wait for a natural gap | **(a).** The gaps on transfer_contract are about a minute every 45–70 min |
| 13 | **A second reaper worker** (new) | (a) build it; (b) not now | **(a), after C.** At 1,900 deletes a second the reaper has under 20% of headroom over inflow |
| 14 | **The backup point** (RUNBOOK precondition 9) | (a) keep it: swap after the 11-19 copy publishes, C = 11-27; (b) waive it: C = 11-26 | **(a).** It costs a day, and its low point is about 90 GiB higher, because the reaper drains a day more of the backlog onto reused space first |
| 15 | **Zero-byte contracts in the usage archive** (new) | (a) stop archiving them (the capture trigger, a migration); (b) keep | **(b) now.** The SN epoch reader adds every provider of a zero-byte contract to the epoch's usage list with 0 bytes (`model/st_extender_pool.go:97-99`, `:154-167`), so dropping them changes the epoch's output. It is worth about 0.35 TiB here and more without a cutover: an SN25 question |

## 7. What changes in CONTRACT-PARTITION.md's numbers

- **The fill rate.** CONTRACT-PARTITION.md "S1", Disk, and the 11-17 runbook §3 C3 used 140–180 GiB a day of
  new leaves at 1,439 contracts a second. At today's 1,600–1,650 a second and the measured 1.9 KB a contract it
  is about 245 GiB a day.
- **The cutover's need.** Their "5.8–7.3 TiB free on the day" is, at a 35-day horizon, 11.5 TiB alone and 7.9
  TiB with every companion.
- **What it leaves out:**
  - evidence's slower retention after C;
  - the archive rows of every reaped contract;
  - the reaper's speed;
  - the legacy's 60k unsettled contracts.
- **"Turn the zero-byte pass off at the cutover" stands,** for one more reason: after C its deletes only add
  archive rows.

## 8. Method and evidence

**Workspace evidence:** `temp/contract-cutover-now-1123/`.
- `scripts/disk_model.py` is the model; `scripts/final_grid.py` produces every table here
  (`python3 -I scripts/final_grid.py`).
- `logs/final-grid.txt` is its output for C = 11-27. `logs/final-grid-c1127.txt` holds the schedule's checks.
- `logs/snap-20261121T051812Z.tsv` and `logs/snap-20261123T143700Z.tsv` are the two table-family snapshots.
- `logs/tc_create_hist.txt` is the create-time histogram.
- `superseded/` holds earlier model iterations, kept for the record.

**Main** (read only, statement timeouts ≤ 25 s):
- the size and `pg_stat_user_tables` snapshots;
- `db_bloat_measurement` of 11-22, for compact sizes;
- the `pg_stats` histogram;
- `pg_index` sizes;
- three TABLESAMPLE SYSTEM reads of transfer_contract, for the zero-byte share;
- one of the archive's 10-19 week leaf, for its row width.

**Mimir:**
- `node_filesystem_avail_bytes`;
- `pg_database_size_bytes`;
- `urnetwork_contract_opened_total`;
- `urnetwork_contract_horizon_*`, for rates, the kept rows of the pass, batch durations, lag and pass start;
- `urnetwork_close_evidence_retention_*`.

**Code and documents:** the reaper, its zero-byte pass, S1's retirement, the cutover, rollback and admission,
the usage archive's eligibility and export, and the evidence retention. The readers in §3 came from a
read-only survey of server main.

**Not verified:**
- the connect build's exact commit;
- the validator's idle windows on 11-24;
- the edge-6 publish time (11-26 07:30Z is a projection);
- the fresh leaves' density, which the first day after C measures.
