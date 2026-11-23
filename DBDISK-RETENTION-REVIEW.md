# DBDISK-RETENTION-REVIEW — retention of Main's three largest tables outside the contract cleanup (2026-11-23)

The owner asked on 11-23 at about 14:30Z for a retention review of the three largest tables on Main that the
contract cleanup does not touch: `st_client_key_history`, `contract_close_report_evidence` and
`client_reliability`. edge-2 had about 5,400 GiB free and was losing about 11–12 GiB an hour, and the disk
runway warning had fired at about 19 days. The question is how much real disk these tables can give back,
and how safely.

**This review changed nothing in production.** It read Main's catalog and statistics, took small page
samples and index-bounded counts (each statement under a `statement_timeout` of at most 25 s), read Mimir,
three hours of taskworker logs, the retention brakes in Main's Redis (`HGETALL` only) and the evidence
mover's own progress log on edge-0. Code is server main at f09d3edd6. Method and queries: §8.

**Fixed in 6b0c08ff0 (11-23, not deployed).** The key-history stall (§3.3, option A of §3.5) is fixed on
server main. It ships with the next taskworker build, which needs the owner's go.
- A batch deletes at most 1,000 registrations, and its cursor moves only past clients with nothing left,
  so a heavy page goes over several batches instead of rolling back.
- A pass starts over at the oldest record an hour after it began, and at once for today's cursor, so the
  55.3M backdated records are reached.
- A batch that runs into its bound fails the task run. RetentionStalled (warp 329ed07,
  `grafana/alerting/retention-stall.yml`) warns when a table's newest due count
  (`urnetwork_retention_due_rows`) is above zero and nothing of it was deleted for 6 h. It needs the next
  Grafana deploy as well.
- **First-day brake: keep 1,000/s** (as set). Batches measured 67 ms for 500 rows and 114 ms for 1,000
  locally, about 2.5× that on Main *(inferred from the old statement's 163 ms)*, every statement by index.
  At the 0.6 duty of 11-18..11-20 the ~57M rows go in about 26 h *(inferred)*. Use 300/s (about 4 days)
  only if the edge-6 replay test is still running, and not 2,000/s on the first day.

- Sizes are GiB (2^30), as `pg_size_pretty` and Mimir report them. M is million, B billion. Times are UTC.
- *(inferred)* marks a figure reasoned from code or arithmetic rather than measured.
- "Real disk" means free space on edge-2's filesystem. `vacuum_truncate` is off server-wide, so a delete
  only makes room inside a table's files, which new rows then reuse. Files shrink only by a rewrite, a
  partition drop or a table drop.

## 1. Summary

| | st_client_key_history | contract_close_report_evidence | client_reliability |
|---|---|---|---|
| Size at 14:26Z | 1,006 GiB: TOAST 775, heap 196, primary key 36 | 740 GiB: heap 369, indexes 362, TOAST 9 | 615 GiB: heap 291, indexes 324, in 11 daily leaves (plus 3 empty ahead) |
| Live rows | 183M | 2.69B | 1.57B |
| Change since 06:58Z | +0.3 GiB: flat, new rows fill ~410 GiB of free space inside | +16.1 GiB; about 38 GiB/day over the last 2.8 days | +16.9 GiB, handed back by each 00:00Z drop (~60 GiB) |
| Retention | **Stalled since about 11-21 18Z**: every run's first batch hits its 5 s statement timeout and rolls back. ~57M rows (31%) are due | Current at its 35-day horizon, but ~400 rows/s go out against ~1,930/s in | Working: the oldest day is dropped at 10 days |
| Real disk that retention returns | none | none | about 58 GiB per day taken off the window, at once |
| Largest lever | fix the stall; reap retired SDK window clients sooner; then a rewrite (~600 GiB) | a shorter window: no code reader needs more than about a day; 35 days takes the table to ~1.5–1.6 TiB by late December | a per-client daily rollup so raw rows can go after ~2 days (~470 GiB) |
| Next step | the batch fix (code, taskworker roll) | owner: the window; raise the brake meanwhile | owner: 10 → 9 days now; rollup design |

**Findings.**
1. **The key-history retention has deleted nothing since about 11-21 18Z** (§3.3). It is on, at 1,000
   clients a second and a 7-day horizon, and it runs every 15 minutes. But its delete batch is bounded by
   gone *clients* (500), not by history *rows*, and the page at its cursor holds 500 clients that own
   73,879 registrations. That delete takes longer than the 5 s statement bound, rolls back, and is
   retried every 15 minutes. Behind the cursor wait 55.3M due records, written after the pass had gone by.
   The table is flat today only because ~410 GiB of space inside it is being reused. With no deletes that
   lasts about 19 days, to around 12-12, and then it grows about 21 GiB a day. Nothing alerted.
2. **The MinIO evidence mover is not about `contract_close_report_evidence`** (§3.4). It moves the
   key-history evidence objects in MinIO from the flat folder to sharded keys, reading its hashes from a
   snapshot file of `st_client_key_history`. It frees no PostgreSQL space and reads no live table rows, and
   the key-history retention never touches MinIO, so the two are independent. It started 11-18 at about
   01:00Z, is 18.8% through, and should end around 12-15 *(inferred)*. **No code moves close evidence rows
   to MinIO or edge-6, or deletes "moved" rows, and none is planned.**
3. **`contract_close_report_evidence` is where these tables grow** (§4). One row is written per accepted
   close report, about 1,930 a second. Its retention keeps up with the 35-day horizon, but the evidence due
   at 35 days is the thin mid-October inflow. At today's inflow the table levels off at about 1.45–1.6 TiB
   in late December, twice today's size *(inferred)*. Its readers need far less: admission retries need hours,
   the zero-carry check 24 h, and the SN epoch census reads evidence only for windows of at most 32,768
   closed contracts, which production epochs never are. The 35 days is the owner's alignment with the
   contract horizon (11-01), not a reader's need. Its retention also loses about 2.5× of its capacity at
   the C1 cutover, which shares its task.
4. **client_reliability is the only one of the three whose retention returns disk** (§5). It drops whole
   daily leaves. Each day of window is about 58 GiB. What binds the window is the weekly operator points
   award, which must read a whole Sunday-to-Sunday block: 10 days gives it three scheduled attempts, 9
   days two, 8 days one.
5. **Deleting rows returns no disk on any of them.** Only partition drops (client_reliability today, C5 for
   the evidence once a legacy drop exists) or rewrites do. pg_repack 1.5.3 is installed on Main, but the
   owner's 10-24 rule excludes it, and its copy holds one snapshot for hours, the failure mode of the 11-04
   dump (`pending_task` bloat, taskworker lag). PostgreSQL 19's REPACK (CONCURRENTLY), planned for
   mid-December, is the rewrite path.

**Two beliefs from earlier today need correcting.** The key-history retention was not held after 11-18: it
ran 11-18..11-20 at about 640 rows a second and has been stalled by its own batch bound since 11-21. And the
"evidence move to MinIO" moves key-history objects, not close evidence.

**Recommended order** (dependencies in §6, decisions in §7):
1. Tonight: nothing. The index walk from 22:30Z and the 23:05Z rescan run as planned.
2. 11-24, after the walk's 13:00Z hard stop and the edge-6 replay test (~20:16Z): one taskworker build
   with the key-history batch fix (§3.5 A) and, if the owner agrees, client_reliability at 9 days (§5.4 A1).
   That returns ~58 GiB at once and restarts the key-history deletes.
3. By 11-26: the evidence brake to 1,500 rows a second, and to 2,500 or more before any C1 cutover
   (§4.5 B). It is a control write, and the batch time has to be measured first.
4. This week, owner decisions: the evidence window (7 or 14 days), and the reap of retired SDK window
   clients (30 days → 1–2).
5. Later: the client_reliability rollup; the C5 legacy drop for the evidence; rewrites after PostgreSQL 19.

## 2. Measurements

### 2.1 Disk

- **edge-2 `/`** (Mimir `node_filesystem_avail_bytes`): 5,401.6 GiB free at 14:40Z.
  - It fell 62 GiB in the last 6 h and 134 GiB in 12 h: about 10.5–11 GiB an hour between drops.
  - Over 24 h it fell 227 GiB, net of this morning's 00:00Z drop of the oldest client_reliability day
    (+61 GiB).
- **The database** is 8,499 GiB, +224 GiB in 24 h (`pg_database_size_bytes`).
- **WAL** runs at about 45–55 MiB/s (two `pg_current_wal_lsn()` reads 14 s apart, in line with earlier
  measurements).

### 2.2 The three tables

`pg_total_relation_size`, partitions summed, against the 06:58Z snapshot:

| Table | 06:58Z | 14:26Z | Change | Heap | TOAST | Indexes | Live rows | Dead rows |
|---|---|---|---|---|---|---|---|---|
| st_client_key_history | 1,005.9 | 1,006.2 | +0.3 | 195.5 | 775.2 | 35.5 | 183.3M | 0 |
| contract_close_report_evidence | 724.2 | 740.3 | +16.1 | 368.6 | 9.4 | 362.2 | 2.69B | 0.49M |
| client_reliability (14 leaves) | 597.8 | 614.7 | +16.9 | 290.7 | 0 | 323.8 | 1.57B | 0 |

A closing read at 15:05:43Z gave 1,006.3, 741.7 and 616.1 GiB, with 5,393.7 GiB free on edge-2.

Rows a second, from `pg_stat_user_tables` read at 14:28, 14:38 and 14:43Z, and averaged against an
11-20 18:53Z read (2.8 days):

| Table | Inserts/s now | Inserts/s, 2.8-day mean | Deletes/s, 2.8-day mean | Size change, 2.8-day mean |
|---|---|---|---|---|
| st_client_key_history | 103–105 | 77 (6.6M a day) | 0 committed since 11-21 (§3.3) | flat |
| contract_close_report_evidence | 1,920–2,020 | 1,927 (166M a day) | 397 | +37.8 GiB/day |
| client_reliability | ~1,660 (99.7k a minute) | 145–162M rows a day | whole days dropped | none, day to day |
| for scale: transfer_contract | 1,532 | | | |

## 3. st_client_key_history

### 3.1 What it holds and who reads it

**One immutable, signed key registration per client, domain and generation.** Each row carries a sealed
evidence envelope of about 2,046 bytes (2,022–2,110 on a 2,106-row sample). It is incompressible and is
always stored out of line, as one full TOAST chunk and a 50-byte tail. Key: `(client_id, domain_hash,
generation)`.
- `st_client_key_head` holds each client's current generation, with a foreign key into this table.
- `st_client_key_gone` records when a client went.
- A row trigger refuses UPDATE and DELETE, except a delete from the retention's own transaction. That
  delete must be for a client gone longer than `st_client_key_history_retention_floor()` (7 days on Main,
  migration 981), with no client row and no head left. Schema: db_client_key_history.go,
  db_client_key_policy_history.go, db_identity_retention.go.
- Rows written before 10-22 also carry an inline copy of the registration (about 1.5 KB of heap). Writers
  stopped filling it then, and the column was never dropped.

**Writer.** There is one: a `ClientKey` frame on connect
(`StoreStClientKeyRegistrationReportingRetry`, model/st_client_key_history.go). It inserts a row and
upserts the head for each new key or generation, capped at 1,024 a client.
- Hosted proxy window children have kept their keys local since 11-02 (1f72fe5ca), except under a
  post-quantum profile.
- URL-probe children have skipped publication since 10-14.

**Readers.**
- **Close-report admission** (`readOriginalCloseKeyRegistration`, model/contract_close_original.go) reads
  the reporting client's own generations, for a signed report on a contract that is still unresolved. The
  admitted report copies the registration into its own evidence row (`original_key_registration`), and SN
  close originals read that copy afterwards. A report with no history is still admitted, recording
  `history_not_found`. In practice a gone client's last report lands within minutes, because deactivation
  revokes its credential.
- **The current-key readers** read active clients only:
  - `GetClientContractMetadata`, on every CreateContract;
  - `stClientKeyCurrent` behind `GetClientPublicKey` and `GET /key/<id>`;
  - `RetainProviderWorkOwner`;
  - `LoadStClientKeyHistory`, for `GET /key/<id>/history` and the SN validator observation routes.
- **The SN epoch key overlay** (`overlayStClientKeyCurrent`) reads the current heads of the providers being
  evaluated and drops retired or inactive clients.

So no reader needs a gone client's history beyond about the 60-minute contract life plus sweep lag. The
guard keeps 7 days. No payout, audit, legal or financial reader of old rows exists in code or documents.
Every reader decodes the copy in PostgreSQL. MinIO holds a public copy, and nothing reads it back.

### 3.2 Size, free space inside, and inflow

1,006 GiB: TOAST 775 (chunks about 760, its index about 15), heap 195.5, primary key 35.5. 183.3M live
rows and no dead rows; the TOAST table was vacuumed at 13:47Z.

**About 410 GiB of that is free space inside the file** *(inferred)*:
- **heap:** 94 GiB reclaimable (weekly bloat run, 11-22: 195.5 GiB, compact 101.1);
- **TOAST:** about 290 GiB. Each blob is a full 1,996-byte chunk and a small tail, and in practice three
  blobs fit a page. On 11-08, when the table was compact, that came to 2.65 KiB a row (709.7 GiB for
  281.2M rows), so 183.3M rows need about 465–475 GiB of the 760;
- **indexes:** 19.5 GiB of the primary key and 6.6 of the TOAST index are reclaimable (bloat run).

That is why the file is flat (+0.3 GiB from 06:58 to 14:26Z) while it takes 77 rows a second on average and
103–105 at midday: about 21 GiB a day of new data *(inferred: 6.6M rows × 3.35 KiB)*. The inflow was 56 rows
a second on 11-16 (PARTITION-CANDIDATES §8.1).

**What the inflow is.** A 10-minute slice of clients minted two and a half hours earlier held 55,651 new
keyed clients (93 a second):
- 99.7% were children of top-level user devices: 4,036 parents in 3,588 networks;
- none were hosted proxies;
- they lived about 19 minutes before deactivation, and 89% of that hour's new keyed clients were already
  retired.

These are the SDK's window clients. A retired one keeps its `network_client` row for
`NetworkClientReapAfterDeactivate`, 30 days (model/network_client_model.go). Hosted windows go after a day
and hosted proxies after two. When the reap deletes the row, the delete trigger records the client gone
**as of its deactivation**, 30 days earlier, so its history is due at once. In practice the table therefore
holds about 30 days of SDK window children: at 6.6–8.9M a day, 200–265M rows, or about 640–850 GiB compact
*(inferred)*.

A 4,243-row sample by the client's mint time:

| Minted | Share of rows | State |
|---|---|---|
| before 10-19 | 1.6% | mostly gone |
| 10-19..10-24 | 25.7% | 98% of them gone and due |
| from 10-25 | 72.7% | children that still have their client row, nearly all retired |

### 3.3 The retention is stalled

**State.**
- `{client_key_history_retention}control` reads `horizonDays 30, maxClientsPerSecond 1000, mode on`.
- The horizon in use is **7 days**. The code clamps the brake's `horizonDays` to its own 7-day `Retention`
  (`stClientKeyHistoryHorizonFromDays`), and the logs say `gone over 168h0m0s`.
- The task `remove_gone_st_client_key_history` (`RemoveGoneStClientKeyHistory`) runs every 15 minutes. It
  has a `pending_task` row, no reschedule error, and 79 "succeeded" executions in the last 24 h.

**It has deleted nothing since about 11-21 18Z.** `urnetwork_retention_deleted_rows_total` for the table,
per 6 hours:

| Window ending | Rows deleted |
|---|---|
| 11-18 08Z .. 11-20 08Z | 12.4–14.2M each (the ramp to 1,000/s ended 11-18 02:23Z) |
| 11-20 14Z | 2.6M |
| 11-20 20Z .. 11-22 02Z | 80k–690k each |
| 11-22 08Z .. 11-23 14Z | 0 |

Every run since then logs the same two lines, five seconds after it starts:
- `a delete batch ran into its bound and rolled back ... canceling statement due to statement timeout
  (SQLSTATE 57014)`;
- `deletes ended batch_bound`.

Its cursor, in the task's result, has stood at a gone time of 11-12 20:35Z since 11-21 18:13Z.

**Why.**
- A batch reads a page of 500 gone records after the cursor (`DeleteBatchCount`). It deletes all the
  history of the due clients among them under a 5 s statement timeout (`BatchStatementTimeout`) and a
  10 s transaction bound.
- The page is bounded by records, not rows. The code assumes "a gone client has one registration on
  average", and for most cohorts that holds.
- **The page at the cursor holds 500 gone clients that own 73,879 history rows**, up to 435 each: clients
  that rotated keys many times. Deleting them takes longer than 5 s, so the batch rolls back and the next
  run retries the same page.
- The page query takes 2 ms (`EXPLAIN ANALYZE`). The history delete is the slow statement:
  `pg_stat_statements` shows a mean of 163 ms and a longest completed run of 4,946 ms.
- Between 14:28 and 14:38Z the table counted 61,005 row deletes and 500 head deletes with nothing
  committed: one attempt got through about 61k rows before it was cancelled. A 15 s statement bound
  (and a 30 s transaction bound) would pass this page, though not necessarily every heavy page after it.

**What is waiting.**
- Of 58.9M gone records, **56.5M are due** (gone more than 7 days). **55.3M lie behind the cursor**, gone
  10-19..10-24.
- Those are the SDK children reaped 30 days after deactivation (§3.2). Their records were written from
  11-18 onward with backdated times, after the pass had walked past 10-24: its cursor was at 10-24 17:02Z
  by 11-18 06:00Z.
- A pass returns to them only after it completes, so they wait for the stall to clear **and** for the
  current pass to finish.
- Each has one history row. That makes about **57–58M rows due today, 31% of the table: about 185–240 GiB
  of data**, the more because these older rows carry the inline registration copy *(inferred)*.

**Cost of leaving it.**
- Nothing today: the file is flat.
- With no deletes, the inflow uses up the ~410 GiB of free space inside in about 19 days (around 12-12).
  The file then grows about 21 GiB a day *(inferred)*.
- Meanwhile the due backlog grows by about 1.4M records a day.

**No alarm fired.**
- The task "succeeds".
- The monitor pins the table's guards and schema (monitor/migration_identity_retention.go) but has no
  liveness signal for its deletes.

**History, for the record.**
- On 11-16 the retention moved to 7 days (migration 981), and its batch planned as an anti-join of whole
  tables. It ran 30 minutes on a 17M-XID snapshot, and the owner paused it.
- The fix, d47a8aa51 (one page per batch, short bounds), rolled on 11-17.
- On 11-17 at about 17:30Z the owner chose "snapshot first, then retain": that is what "held until
  confirmed" referred to.
- On 11-18 the brake was ramped 100 → 300 → 600 → 1,000/s, 01:15–02:23Z, with no stop rule tripped.

### 3.4 The evidence mover and MinIO

The mover on edge-0 (`bringyourctl evidence move-flat`, server 6e2fa91e8, package evidencemove/) is **not**
about `contract_close_report_evidence`, and it frees no PostgreSQL space.
- It moves the legacy *flat* MinIO objects of key-history evidence,
  `.../st/v1/evidence/content/sha256/<h>.json`, to their sharded keys. It verifies each copy and only then
  removes the flat object. The flat folder is what made MinIO's data scanner run edge-5 out of memory on
  11-17.
- It reads its content hashes from a **snapshot file** of `st_client_key_history`'s evidence hashes:
  294.5M rows, exported 11-17 19:29–23:03Z. It covers clients minted before 11-16 18:00Z, whose evidence
  predates the sharded layout. It opens PostgreSQL read-only and never reads or deletes table rows.
- Its build was merged 11-17 23:36Z, and its first progress line is 11-18 01:00:38Z. It did not start on
  11-17 at 00:56Z.
- The key-history retention deletes PostgreSQL rows, heads, gone records and the legacy Redis key
  (`removeGoneClientPublicKeys`). The server's blob store has no delete at all. So the retention cannot
  touch the mover's objects, and its deletes do not change the mover's input: the two are independent.

**Progress** (its log at 14:40:38Z):
- 55.4M of 294.5M snapshot rows done (18.8%): 8.55M objects moved, 46.9M absent (no object under either
  key), 2 deduplicated, 0 refused.
- Its cursor is at clients minted 10-22 18:02Z. Its gates were open, and it moves 200 objects a second
  while they are.
- Over the last 37 h it averaged about 11M rows a day: its gates close for the validator and for Main's
  health (edge-2 busy over 0.70 or iowait over 0.15, api over 30 ms, taskworker overdue, lock waiters).
- So **it should finish around 12-15** *(inferred)*, later than the ~24 days first expected. The MinIO data
  scanner stays off until then (owner, 11-17).

Two notes for the MinIO side, not for this table:
- 85% of the hashes processed so far have no object under either key.
- Evidence content has no lifecycle rule, so objects of deleted history stay in MinIO. The history deleted
  11-08..11-16, before the snapshot (about 34M rows), may have left flat objects the mover cannot reach
  *(inferred)*.

### 3.5 Options

| Option | GiB | When | Risk | Needs first |
|---|---|---|---|---|
| **A. Fix the stall.** Bound a batch by history rows as well as records, or delete a heavy client's rows in chunks across short transactions: heads, then history chunks, then the gone record, an order the guard already admits. Ship it with a test that fails on today's code: 500 gone clients with ~150 registrations each, under a short statement bound, make no progress | Disk: 0. Inside the file: ~185–240 GiB freed, and the backlog stops growing | A server change and a taskworker roll (`ST_QUIET_MIN=0`). The current pass then completes within a run or two, and the next pass deletes the 55.3M backdated records in about a day at 1,000/s | Low: the same guard and brake. About 210 GB of extra WAL over that day *(inferred: 1.9 MB per 500 records, 11-16)*, then an autovacuum of the 760 GiB TOAST table | Nothing: a bug fix. As an interim, a 15 s statement bound and a 30 s transaction bound would pass today's page |
| **A′. A liveness signal:** the retention ending `batch_bound` for an hour, or deleting nothing while due records exist | 0 | with A | none | the monitor developer |
| **B. Reap retired SDK window children after 1–2 days** instead of 30, as hosted windows already are | ~450–600 GiB less live data at steady state, about 8 days of children instead of 30 *(inferred)*. `network_client` shrinks too. Real disk only with C | A reap change and a taskworker roll; the effect builds over 30 days | Medium: every reader of a retired child's client row inside 30 days must be checked. Close admission needs about an hour; also disputes, account deletion and support tools | owner decision; a reader audit; tests |
| B′. Stop SDK window children publishing keys, as hosted children did on 11-02 | most of the inflow | an SDK and connect release, weeks | Medium–high: their close reports then record `history_not_found` | protocol owner |
| **C. Rewrite the table:** PostgreSQL 19's REPACK (CONCURRENTLY), or a chunked copy-and-swap | ~600 GiB at once after A (~400 GiB compact) *(inferred)*. **Without B it regrows toward 640–850 GiB within weeks** | after A and B; 19 is planned for mid-December | High on Main today. pg_repack is excluded by the 10-24 rule and holds one snapshot for hours. VACUUM FULL would block every CreateContract. It needs free space for the copy plus its WAL | A and B; 19, or a reviewed copy-and-swap design |
| D. Partition by client mint week (PARTITION-CANDIDATES §8.3) | the ~1 TiB legacy file, once emptied | months | High: long-lived clients' rows pin every week, so survivors must be moved out | not now |
| E. A window below 7 days | small: gone clients' rows are a fraction of the table | code, floor migration, monitor contract | Low for readers | owner; not worth it before B |

Recommendation: A now. B as the owner's decision. C only after A and B.

## 4. contract_close_report_evidence

### 4.1 What it holds and who reads it

**One immutable row per accepted, identified close report.** Each party files its own reports, and
checkpoints add rows. Key `(client_id, report_id)`; the census index is `(contract_id, client_id,
report_id)`.
- Columns: the contract, the party, acked and unacked bytes, the checkpoint flag and `accepted_at`.
- For signed reports it also holds the original report, a copy of the key registration and the inventory
  (`original_*`). These are null on 99.5% of rows; hosted window clients sign none.
- Migrations: 767–769, the index changes of 825, and the retention guards 857, 861 and 913
  (db_contract_close_evidence.go, db_contract_close_original.go, db_contract_close_inventory.go).
- A row trigger refuses UPDATE and DELETE, except deletes from the retention ("on", older than the guard's
  35-day floor) and C5's moves ("move").
- No foreign keys point in or out.

**Writer.** `admitContractCloseReportInTx` (model/contract_close_report.go), from connect's CloseContract
when it carries a report id. It writes in the same transaction as the receipt in `contract_close_report`,
and both took 1,923 rows a second. Exact retries write nothing. Rows per contract rose from 0.94 on 11-15 to
about 1.25 now; why is open.

**Readers**, all in code:

| Reader | What it needs |
|---|---|
| **Admission** (`readRetainedContractCloseReport`, `matchContractCloseReportInTx`): answer exact retries, refuse a reused report id | Reports still being retried: hours. Once the row is gone, an exact retry is answered from the receipt while the contract exists |
| **Zero-carry** (`providerZeroCarryIdleSql`, model/provider_zero_carry.go): a final source report with zero bytes, for contracts closed in the last 24 h | 24 h |
| **The SN epoch census** (`stEpochProviderOriginalUsageSql`, model/st_provider_close_originals.go) | Only when the epoch window holds at most 32,768 closed contracts (`MaxClosedWorkRecords`, model/st_provider_usage.go). Production windows hold hundreds of millions, so it reads the usage statement "without close reports": **no evidence in production** |
| C5's week blocker | Only after a C5 cutover |

**Not readers:** settlement, disputes, payouts and reconciliation, stats, and the usage archive. The SN25
usage proof lives in `st_provider_usage_archive`. Nothing in the sn, connect, mmm or monitor repositories
reads the table.
- A dispute keeps its contract unresolved, and the retention never deletes evidence of an unresolved
  contract, at any age.
- Account deletion keeps the table as settlement evidence "under its own retention"
  (model/network_deletion_cleanup.go). It is not in the class kept for legal, tax and dispute reasons.

So code needs about a day of evidence; **35 days is a policy**, the owner's 11-01 alignment with the
contract horizon (DBDISK-STABILIZE §3.5).

### 4.2 Size and growth

740 GiB at 14:26Z:
- **heap** 368.6;
- **indexes** 362.2: the census index 199.9 and the primary key 162.3;
- **TOAST** 9.4, flat since about 11-20.

That is 2.69B live rows at about 296 bytes each. Autovacuum is tuned per table (scale factor 0, threshold
50M rows): 0.49M dead rows, last vacuumed 13:59Z.

**Growth.** +37.8 GiB a day over the last 2.8 days, and +52 GiB a day between 06:58 and 14:26Z.
- Inserts run at 1,927 a second (166M a day), deletes at 397.
- The heap grows about 17 GiB a day, the indexes about 23.

**The rebuilds of 11-16 and 11-18 were a loan of days:**

| Index | Rebuilt | Size before → after | Size now |
|---|---|---|---|
| primary key | 11-16 | 151.2 → 96.9 GiB | 162.3 |
| census | 11-18 | 185.0 → 141.2 GiB | 199.9 |

Both are past their pre-rebuild size within a week. Neither index is in tonight's walk.

**Where it is heading.**
- The retention deletes at 35 days, so it removes what was accepted 35 days ago:
  - ~38M rows a day now (mid-October);
  - ~63M a day from about 11-26;
  - ~130M around 12-20;
  - 150–170M from about 12-25.
- **The table grows 30–38 GiB a day now and about 28 from 11-26. It levels off around 12-20..12-25 at
  about 5.3–5.9B rows, 1.45–1.6 TiB** *(inferred, at today's inflow)*, and only if the retention can
  then delete 130–170M rows a day (§4.3).

### 4.3 Retention

`RemoveExpiredCloseEvidence` (db_contract_close_evidence_retention.go) runs inside the task
`maintain_contract_partitions` (taskworker/work/contract_partition_work.go).

**The horizon.** It is the larger of two values (db_contract_close_evidence_horizon.go):
- the contract reaper's horizon, clamped to 35–91 days while the reaper is on; 35 days now
  (`urnetwork_close_evidence_retention_seconds` 3,024,000);
- the guard's floor: 35 days if the installed guard body is migration 913's, 91 days otherwise.

**What a batch deletes.** It walks the census index below the first contract id inside the horizon. It
deletes by ctid the rows accepted before the horizon whose contract is resolved or gone, 5,000 rows a batch,
under a 30 s statement bound.

**The pace.** It is the brake `{close_evidence_retention}control`, **empty on Main**, so the default applies:
1,000 rows a second, out of a 5,000 cap. Each run gives the evidence a third of the task's 10-minute budget,
200 s, so at most 200,000 rows a run.
- A run that stops on its budget is followed a minute later.
- A run that finishes the pass (`done`) waits 15 minutes.
- So the default carries about 65M rows a day at most *(inferred)*.

**State.**
- 24 h: 419 rows a second, 156 runs stopped on `budget` and 46 on `done`.
- 7 days: 676 `budget`, 423 `done`, 9 `statement_timeout`, 1 `brake_unread`.
- Daily means over the last 11 days: 59–422 rows a second, rising with the cohorts.
- It is current. 448k rows of contracts minted 35–36 days ago remain, and one row older (census-index
  counts).

**What a delete gives back.**
- The row's heap space and its index entries are reused in place by new rows. The file never shrinks.
- Index bloat comes back only by a rebuild, and these indexes regrow within a week (§4.2).

**What is coming:**
- **~63M rows a day are due from about 11-26, about the default's whole capacity.** The `done` stops will
  disappear.
- Around 12-20 it needs 2,000–2,600 rows a second, a speed no batch has been measured at.
- **The C1 cutover cuts its capacity about 2.5×.** The same task runs C1's day partition work first, up
  to 400 s a run, so about 131 runs a day remain: ~26M rows a day at the default *(inferred)*. Every
  document so far plans the brake at 1,500/s by 12-06..12-15, sized for ≤100M rows a day. That is too
  low.

**C5 (weekly partitions) is built, not cut over.**
- Its legacy leaf, today's table, never drops: `KeepLegacyPartition`, `IsExpired`, `DropRange`, the drop
  fence and the blocker all refuse it.
- No legacy-drop step exists, so a cutover alone returns nothing.
- After a cutover, new weeks stop reusing the legacy file's space. At 35 days about 1.4–1.6 TiB of weeks
  would pile up on top of the frozen ~0.75 TiB before it could go.

### 4.4 Is anything moving this table to MinIO?

No. No code moves close evidence rows or their content to MinIO or edge-6, or deletes "moved" rows.
- The C5 "move" guard is for moving rows between partition leaves.
- CONTRACT-PARTITION.md records "no MinIO export before a drop for now".
- Rows leave only through the retention's deletes, and those become dead tuples that autovacuum turns into
  reusable space, not disk.

### 4.5 Options

| Option | GiB | When | Risk | Needs first |
|---|---|---|---|---|
| **A. Window 35 → 7 days**, decoupled from the contract horizon | Steady state ~0.3 TiB instead of ~1.5–1.6 TiB, so the file stops growing once the backlog is gone. ~1.1–1.3 TiB of growth avoided by late December, and ~0.4 TiB free inside the file. Real disk only with C or D *(inferred)* | Code: a floor of its own instead of following the reaper (`CloseEvidenceRetention`), a guard migration, the body match in `closeEvidenceGuardFloorOf`, the monitor contract. Then a ~1.5B-row backlog **plus ~166M rows a day crossing 7 days**: about 10 days at the 5,000/s cap (~326M a day, if batches sustain it); at 2,500/s or less it never catches up. So A needs B′ or the cap | Readers: low (§4.1). IO: medium. 50M-row autovacuum passes over 362 GiB of indexes come every few hours while the backlog goes, plus the WAL | owner decision; tests; a measured batch speed; B′ |
| A2. 35 → 14 days | Steady state ~0.65 TiB: the current file holds it, with no growth. ~0.9 TiB avoided by late December | as A. Backlog ~1.3B rows plus 45–75M a day crossing 14 days until ~11-29, then ~130–170M: about 5 days at the cap, ~13 at 2,500/s | as A | as A |
| **B. Raise the brake**, needed while the window stays at 35 days and at any C1 cutover: 1,500/s from ~11-26, 2,500/s or more before the C1 cutover or ~12-20. At today's inflow any window ends up needing ~166M deletes a day, which is ~2,600/s under the 200 s-per-run limit | 0. It keeps the plateau from rising further | a control write (`bringyourctl db close-evidence-retention --max-rows-per-second`) | Low–medium: WAL and vacuum load. Batch time above 1,000/s is unmeasured | measure rows per run and batch time at 1,500 first |
| B′. The evidence retention as its own task | 0. Removes the C1 coupling and the 1-minute gaps | a small server change | low | before C1 |
| **C. C5 cutover plus a legacy-drop step** (detach, then drop; a precondition that no legacy row belongs to an unresolved contract or lies inside the window) | Returns the legacy file, ~0.75 TiB at today's size, at cutover + window + a slow-clock margin | code for the drop step; the cutover itself | Medium: the weeks must fit the free disk meanwhile. Sensible only with A: at 7 days the peak is ~0.3 TiB of weeks on top of the frozen legacy | A; owner |
| D. Rewrite after A (REPACK in PostgreSQL 19) | ~0.4 TiB | mid-December or later | as §3.5 C | A |
| E. Delete zero-byte reports' evidence with their contract at 8 days | ~15–20% of the steady state *(inferred: 20–26% of rows are zero-byte reports)* | code and a guard | low | superseded by A |
| F. Rebuild the indexes again | ~60 GiB each, gone again within a week | a walk window | low | not worth it before A |

Recommendation: B now, ahead of C1. A as the owner's decision; 7 days covers every reader with a wide
margin. C after A.

## 5. client_reliability

### 5.1 What it holds and who reads it

**A row is one UTC minute of one provider client on one address bucket.** The bucket is the client's
IPv4 /29 or IPv6 /56, hashed. Each row holds the minute's connection, provide and message/byte counters, and
`valid`: whether the minute counts as reliable (one connection kept, provide enabled and unchanged, at
least one message received). Key `(block_number, client_address_hash, client_id)`, where a block is an
epoch minute. Columns: migrations 269, 429 and 522.

**Writers.**
- connect's announce loop counts every 30 s into Redis, but only for clients that provide publicly or
  changed their provide key in that round. The code's words: "stats only matter for providers"
  (connect/transport_announce.go).
- The task `rollup_client_reliability_stats` (`RollupClientReliabilityStats`, every minute) drains each
  final minute into its day's leaf, in sorted inserts of 5,000 rows. Rows are never updated after that.
- Measured: 145–162M rows a day (`client_reliability_block`, 11-14..11-22), 98–99% valid; 99% of a sampled
  day's rows had provide enabled.
- That is about 1,440 rows a day for each of about 105k connected provider slots. Payer and hosted window
  clients write nothing, so filtering by role gains nothing.

**Readers**, all by block range:

| Reader | Reaches back | Needs raw rows |
|---|---|---|
| Client score windows of 5 min, 60 min and 12 h (admission and ranking): `UpdateReliabilities` → `advanceReliabilityRunningLookback`, `UpdateClientReliabilityScores` | 12 h | yes |
| **The weekly operator points award:** `AwardDueOperatorBlockPoints` → `readOperatorBlockReliability` → `readPaymentPlanReliabilityPartials`, daily at 12:00Z and 15 min after a taskworker start | the whole Sunday-to-Sunday block, from 2 h after it closes | yes: exact sums of valid rows |
| The 7-day network window (`UpdateNetworkReliabilityWindowScoresInTx`), whose only use is `GET /network/reliability`'s choice of countries to list | 7 days; a full reread takes ~55 min | yes |
| SN Top board presence (`GetSnTopPresence`): one 72-minute tempo; missed tempos replay up to 7 days | 72 min; 7 days on replay | yes |
| Country multipliers (`RefreshCountryReliabilityMultipliers`), weekly | 24 h | yes |
| 15-minute network buckets and block health | about 1 h | at write time |
| Operator one-shot audit backfills (`bringyourctl`) | the whole window | yes |

**Not readers:** `provider inspect` and the provider status page read `network_client_location_reliability`.
SN25 settlement reads `verify_provider_stats`. Account deletion keeps these rows.

The catalog agrees. On 11-23 the leaves of 11-17..11-21 were last read at 00:03–00:23Z, 11-15's at 01:12Z
and 11-16's at 14:18Z. The 10-day-old leaf of 11-13 was last read on 11-22.

**Retention needs.** No legal one appears in code or documents. The binding reader is the points award:
- It fails closed when a block's rows are gone (`ErrOperatorBlockPointsReliabilityExpired`), and a block
  recorded expired is never awarded (model/operator_block_points.go). `operator_block_points_expired` is
  empty on Main.
- It needs the block's 7 days, plus up to 12 h to its first daily run, plus the run itself.
- **10 days gives three scheduled attempts, 9 days two, 8 days one. Under about 7 d 2 h every week
  expires.**
- The SN Top replay needs 7 days for its replays to be correct.
- The block that closed 11-22 00:00Z was awarded at 06:27Z that day (`payment_report`).

### 5.2 Size and growth

615 GiB at 14:26Z: heap 291 and indexes 324. The indexes are the primary key, 12–13 GiB a day, and the
covering `(valid, block_number, client_address_hash) INCLUDE (network_id, client_id)`, 17–19 GiB a day.
That is about 400 bytes a row, and each full day is 57–64 GiB.
- Daily leaves `client_reliability_pYYYYMMDD` by `RANGE (block_number)`, with three created ahead (empty).
- No dead rows, updates or deletes: the leaves are insert-only, and autovacuum only freezes them.

It does not grow from day to day. It adds about 2.4 GiB an hour during the day, and that is returned at
about 00:00Z when the oldest day is dropped: +61 GiB of free space on 11-23, +63.5 on 11-21, +79.5 on 11-20.

### 5.3 Retention

**The window** is `ClientReliabilityRetention = 10 * 24h`, a code constant
(model/network_client_reliability_model.go) set by the owner: 21 → 14 days on 11-02, 14 → 10 days on 11-10
(O3).

**The task** is `remove_old_client_reliability_stats` (`MaintainClientReliabilityPartitions`, every
minute):
- it creates the days ahead;
- it `DROP TABLE`s each leaf whose range ended before the cutoff, with a 5 s lock timeout;
- the drop returns the leaf's files at commit.

It is working: leaves 11-13..11-23 are present, plus three empty ahead.
- The monitor's §2.4g `partition-retention-stuck` watches it.
- There is no metric for rows or drops, only `[ncr]` log lines.

### 5.4 Options

| Option | GiB | When | Risk | Needs first |
|---|---|---|---|---|
| **A1. 10 → 9 days** | −58 at once | the first run after the taskworker roll | Low: the award keeps two scheduled attempts. One-way: a dropped day is gone | The block that closed 11-22 is awarded; the next closes 11-29. Update the texts that say "10 days" (the operator block points signal, SIGNALS §2.4q). `TestContractHorizonKeepsSweepsOperatorBlockPointsStillRead`, which already fails part of each week since the 10-day change, needs its fixture fixed |
| A2. 10 → 8 days | −117 at once | same | Medium: one award attempt a week. A failed or late run loses that week's points for good | as A1, plus the owner accepting the award risk |
| A3. Under 8 days | | | Breaks the award and SN Top replays | needs B |
| **B. A per-client daily rollup** (valid count, exact Σ1/n, observed count, degraded blocks excluded) feeding the award and the 7-day window, with raw rows kept about 2 days | about −470 at steady state, by drops *(inferred)* | 2–3 weeks to build and test, then the drops | Medium: a new table and writer, the award's and the window's readers rewritten, a replay guard for SN Top | owner go; design review; the award checked on both paths for a week |
| C. Retire the 7-day network window (it only orders the app's country list) | 0; saves its 55-minute rereads | days | low | product check |
| D. Slim the indexes of new leaves | about −30 (fillfactor 100) to −200 (no covering index) over 10 days | as leaves turn over | Medium–high. The covering index makes every `valid` range read index-only: the range aggregates run ~10k times at 4–5 s each. The primary key is the drain's `ON CONFLICT` target | a measured plan per reader |
| E. Coarser blocks (5 minutes) | about −480 | weeks | High: the minute is the unit of validity, of the partition key, of the award and of SN Top | not recommended |

Recommendation: A1 now. B as the structural fix. A2 only if the disk gets worse before B lands.

## 6. Order of operations and dependencies

| When | Step | Effect | Depends on |
|---|---|---|---|
| Tonight, 11-23 | **Nothing** | — | The walk (from 22:30Z: transfer_contract's two destination indexes; hard stop 11-24 13:00Z; it stops on edge-2 health). The 23:05Z rescan reaps 21.0M review contracts plus the weekly rescan. It adds no evidence work: a resolved contract's evidence goes at 35 days whether or not the reaper kept the contract |
| 11-24, after 13:00Z and ideally after the edge-6 replay test (~20:16Z) | **One taskworker build:** the key-history batch fix (§3.5 A) and, with the owner's go, client_reliability at 9 days (§5.4 A1) | ~58 GiB of real disk at once. Key-history deletes resume: the backdated backlog goes in about a day at 1,000/s | `ST_QUIET_MIN=0` and the usual gates. If the replay test must not see extra WAL, set the key-history brake to 300/s for the first day: about 3 days for the backlog |
| By 11-26 | **Evidence brake to 1,500/s.** Measure batch time and rows per run first | Keeps the 35-day horizon current as ~63M rows a day come due | none; a control write |
| Before any C1 cutover | Evidence brake to ≥2,500/s, or the evidence retention in its own task (§4.5 B′) | Avoids a backlog of ~37M rows a day | the C1 cutover plan |
| This week | **Owner:** the evidence window (§4.5 A) and the SDK window children's reap (§3.5 B) | Evidence: no further growth. Key history: steady state about 0.45–0.6 TiB smaller | code and migrations; reader audits |
| After the evidence window | The evidence backlog delete at 2,000–5,000/s, outside walk windows and backups; then the C5 legacy-drop step | Evidence holds ~0.3–0.65 TiB; real disk at the legacy drop | batch speed measured; free disk for the new weeks |
| 2–3 weeks | The client_reliability rollup (§5.4 B) | ~470 GiB by drops | owner go |
| After PostgreSQL 19 (mid-December) | REPACK (CONCURRENTLY) of `st_client_key_history`, then the evidence | ~600 GiB and ~400 GiB of real disk *(inferred)* | the fixes above first, or the tables refill |

**Dependencies, one by one:**
- **The evidence mover** (edge-0, until about 12-15). It is independent of all three tables. It pauses
  whenever edge-2 is busy over 0.70, iowait is over 0.15, the api is over 30 ms, the taskworker is
  overdue or lock waiters pile up, so heavy delete ramps on Main slow it. The MinIO scanner stays off
  until it ends.
- **The edge-6 backup replay test** (until about 11-24 20:16Z). Keep heavy WAL producers off Main until
  it is done: the key-history backlog (~210 GB of WAL in a day), the evidence backlog, any rewrite.
- **Tonight's index walk and the 23:05Z rescan.** No retention change tonight. The walk stops on edge-2
  health, and the rescan is itself a large delete.
- **The C1 cutover** (planned "now"). Every GiB these tables give back counts toward its headroom, and
  shorter windows here are durable. The evidence retention shares C1's task and must be raised or split
  first.
- **PostgreSQL 19** (mid-December) brings REPACK (CONCURRENTLY), the only rewrite path that fits the
  owner's rules.

## 7. Owner decisions

1. **Key-history stall fix:**
   - (a) ship the row-bounded batch with its test in the next taskworker build **(recommended)**;
   - (b) an interim: a 15 s statement bound and a 30 s transaction bound only;
   - (c) leave it: the table starts growing ~21 GiB a day around 12-12.

   Also: the brake for the backlog, at 1,000/s (set) or 300/s for the first day.
2. **Evidence window:**
   - (a) keep 35 days: ~1.5–1.6 TiB by late December, and a brake of 2,000–2,600/s by 12-20;
   - (b) 14 days: holds at about today's size;
   - (c) **7 days (recommended):** the steady state is ~0.3 TiB, and every code reader needs a day or less.
     It needs the retention in its own task or at its 5,000/s cap, because ~166M rows a day cross 7 days
     (§4.5 A).
3. **Evidence brake now:**
   - (a) **1,500/s from ~11-26 and ≥2,500/s before the C1 cutover (recommended)**;
   - (b) leave the 1,000/s default.
4. **client_reliability window:**
   - (a) keep 10 days;
   - (b) **9 days now: −58 GiB, two award attempts (recommended)**;
   - (c) 8 days: −117 GiB, one award attempt.
5. **client_reliability rollup** (raw rows ~2 days, −~470 GiB):
   - (a) design and build;
   - (b) not now.
6. **Reap of retired SDK window clients:**
   - (a) keep 30 days;
   - (b) **1–2 days like hosted windows, after a reader audit (recommended to study):** the key-history
     steady state falls ~0.45–0.6 TiB, and `network_client` falls too.
7. **Evidence C5 legacy-drop step:**
   - (a) build it after the window decision;
   - (b) not now.
8. **Rewrites:**
   - (a) **PostgreSQL 19's REPACK (CONCURRENTLY), after the fixes above (recommended)**;
   - (b) an exception to the no-pg_repack rule: not recommended, because of the hours-long snapshot.

## 8. Method and evidence

All reads were made on 11-23 between 14:23 and 15:06Z. Nothing was written to production.

**Main,** one read-only psql statement at a time (`statement_timeout` ≤ 25 s):
- **Sizes:** `pg_total_relation_size`, `pg_relation_size` and `pg_indexes_size`, and the TOAST relation of
  each table and leaf, against the 06:58Z snapshot of the same query.
- **Statistics:**
  - `pg_stat_user_tables`, three reads for rates, plus an 11-20 18:53Z read for the 2.8-day means;
  - `pg_stat_all_tables` for the TOAST tables;
  - `pg_stat_user_indexes`, for scans and last scans per leaf index;
  - `pg_stat_progress_vacuum`;
  - `pg_stat_statements`, for the retention's own statements and the heaviest readers;
  - `db_bloat_measurement`, the weekly runs of 11-08, 11-15 and 11-22.
- **Catalog:** columns, `pg_stats` widths, indexes, triggers, foreign keys, the guard and floor functions,
  reloptions, `pg_settings`.
- **Samples:**
  - `TABLESAMPLE SYSTEM` of 0.0005–0.002% of a large table's pages (2–30k rows);
  - 0.01–0.5% of `st_client_key_gone`;
  - index-bounded counts on `st_client_key_gone_gone_time`, and on the evidence census index by
    contract-id floor (the first 6 bytes of a server id are its Unix milliseconds);
  - new keyed clients by a primary-key range of `st_client_key_history`.
- **Plans:** one `EXPLAIN ANALYZE` of the key-history retention's page query, which reads and locks
  nothing, and `EXPLAIN` of the whole batch statement (custom and generic plans), which does not run it.
- **The retention tasks:** their rows in `pending_task`, and their results in `finished_task` (sampled
  windows, 11-17..11-23).

**Mimir:**
- `urnetwork_retention_deleted_rows_total{table="st_client_key_history"}`, 6-hour increases over 8 days;
- `urnetwork_taskworker_executions_total`;
- `urnetwork_close_evidence_retention_removed_rows_total`, `_stops_total` by `stop`, and `_seconds`;
- `node_filesystem_avail_bytes` and `pg_database_size_bytes`.

**Other sources:**
- **Loki:** `{env="main",service="taskworker"} |= "[ckh]"`, the last 3 hours.
- **Redis:** `HGETALL` of `{client_key_history_retention}control`, `{close_evidence_retention}control` and
  `{contract_horizon}control`.
- **edge-0:** the evidence mover's progress lines in its own log, read only.
- **Code** (server main f09d3edd6):
  - key history: model/st_client_key_history_retention.go and its control, db_identity_retention.go,
    taskworker/work/client_key_history_work.go, evidencemove/;
  - close evidence: db_contract_close_evidence_retention.go, db_contract_close_evidence_horizon.go,
    db_contract_close_evidence_partition.go, model/st_provider_usage.go;
  - client reliability: model/network_client_reliability_model.go,
    model/network_client_reliability_partition_model.go, model/operator_block_points.go;
  - client reaps: model/network_client_model.go.
- **Documents:** DBDISK-STABILIZE.md, PARTITION-CANDIDATES.md (§8), PARTITION-MAINTENANCE-PLAN.md (C5,
  Part D), DBDISK-INDEX-RUNBOOK.md, CONTRACT-PARTITION.md.
