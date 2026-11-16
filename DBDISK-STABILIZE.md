# DBDISK-STABILIZE — stabilizing Main's database disk on edge-2 (2026-11-15)

The owner asked on 11-15 at about 20:15Z: "we need a plan to stabilize the db disk
usage." This is that plan. It rests on read-only measurements of Main taken on 11-15
between 19:59Z and 21:40Z, Mimir history back to 10-14, the server source at
662886c32, and the earlier storage documents (`STORAGE.md`, `CONTRACT-PARTITION.md`
and its review, `PARTITION-MAINTENANCE-PLAN.md` with its review and fixes, CPUOPT6
Part D, and the 11-08 runway report `temp/disk-runway/REPORT.md`).

**This research changed nothing in production:** no DDL, delete, rebuild, setting or
control write. The owner has since approved four items (status in §6); everything
else waits for the owner's go.

- Sizes are GiB (2^30), as `pg_size_pretty` and Mimir report them. Decimal GB is
  marked where an older document used it.
- *(inferred)* marks a claim reasoned from code or arithmetic rather than measured.
- "Today's load" means 11-15: about 1,350 contracts created a second.

## 1. Summary

**Where we are.** edge-2's root filesystem (ext4, 14,077 GiB) had **4,633 GiB free
(32.9%)** at 20:23Z. The database is 9,294 GiB and WAL 40 GiB. Over the measurement
window the relations grew **+302 GiB/day**. The daily 00:00Z drops of the oldest
client_reliability and egress-history days give back about 65–75 GiB, so the net loss is
about **237 GiB/day** at this hour, and it has risen every day since 11-13.

**Why. Three causes, in order of size:**
1. **The contract horizon reaper stopped deleting contracts on 11-12/13** (§3.2). It
   is on, at its 35-day horizon, and reports zero lag, but its cursor walks past every
   contract that closed after the horizon and leaves it for a rescan 30 days later.
   Since its pass caught up at 11-13 14:09Z it has deleted **0.02–9 contracts a
   second** (6-hour means) against 54–358 scanned; **253M contracts were skipped as
   `recent_close` in four days**. So the six contract tables grow at the full creation
   rate, and the free space the reaper's earlier deletes left inside them has run out.
2. **Contract creation doubled with the bot sweeps** (CPUOPT6 D): 560–735 a second
   from 11-06 to 11-13, 984 on 11-14 and 1,312 on 11-15.
3. **Every 35-day window is refilling at the higher volume:** close evidence,
   work-session events, key history and, once the reaper works again, the usage
   archive.

**Approved on 11-15 and in progress** (§6): **I1**, the reaper cursor fix with an
immediate rescan (rolling with the taskworker tonight); **I4**, dropping the 366 GiB
legacy work-session events tonight; the vacuum-gap rule for the O2 rebuilds; and
PostgreSQL 18.7 before 19.

**Projection** (§4, today's load):

| Path | 500 GiB floor | Free GiB on 12-15 | Free GiB on 02-14 | Net GiB/day in February |
|---|---|---|---|---|
| Nothing done | 12-13 (stress 12-11) | 12 (stress: full from 12-14) | full from 12-16 (stress 12-14) | +172 (stress +195) |
| I1 + I4 (approved 11-15) | 12-17 (stress 12-16) | 783 (stress 503) | full from 12-24 (stress 12-19) | +11 (stress +8) |
| + I2, I3, I5 (rebuilds, queue tables) | 01-17 (stress 12-22) | 2,095 (stress 1,442) | 198 (stress: full from 12-27) | +11 (stress +8) |
| + the hosted idle trim at −20% contracts | not reached | 3,438 (stress 2,933) | 2,208 (stress 1,446) | +6 (stress +4) |
| + trim, events for providers only, index diet | not reached | 3,650 (stress 3,145) | 3,084 (stress 2,301) | +8 (stress +6) |
| + trim + RAID-1 tablespace (+6,980 GiB, 12-15) | not reached | 10,418 (stress 9,913) | 9,188 (stress 8,426) | +6 (stress +4) |
| + trim, structural items and the tablespace | not reached | 10,630 (stress 10,125) | 10,064 (stress 9,281) | +8 (stress +6) |

- The approved items move the floor by four to five days, to about 12-17, and the
  disk-full date by about a week; they do not clear it.
- With I2, I3 and I5 as well, the floor comes in late December (stress) to mid-January
  (base). The plan holds through February only if the trim takes about 20% or more off
  contract inflow, or if more disk arrives. The structural items alone (§4, row E0) hold
  in the base case but not under stress.
- **50% free is out of reach on the current 14 TiB volume at today's load.** With the
  extra RAID-1 tablespace, the trim and the structural items together, free space is
  about 48–50% of 21,057 GiB (§4, row F).
- The PostgreSQL 19 upgrade (mid-December) should go only with at least 2 TiB free
  (§5.4); without the trim or more disk the projection is below that by then.

**Top five actions** (§5; GiB at today's load):

| # | Action | Effect | Status |
|---|---|---|---|
| 1 | I1: reaper cursor fix and immediate rescan | Deletes the ~340M-contract backlog in about 2 days, then each 35-day cohort; about +900 GiB of free space by 12-01 against doing nothing; net growth about zero by February | **approved; rolling tonight** |
| 2 | I4: drop `provider_work_session_event_legacy` now | 366 GiB at once, instead of 12-21 or later | **approved; tonight** |
| 3 | I2–I3: O2's remainder, the three drained partials (144 GiB), then the full contract indexes after the backlog delete (1.5–1.8 TiB) | One-time; about half regrows within ~35 days | **vacuum-gap rule decided (a)**; execution later |
| 4 | Measure the hosted idle trim (CPUOPT6 B #1, rolling tonight) | Each 10% fewer contracts saves about 16 GiB/day now and about 10% of the steady state | approved; measure |
| 5 | I5: partition finished_task and transfer_debit_journal, swap pending_task | about −74 GiB once, and they stop re-bloating | **recommended**; decision |

**Open decisions** are in §6: the queue tables (I5), checking edge-2's free drive
bays, the structural items (events for providers only, the index diet, C1), the 18.7
window and the 19 method, and the alerts.

## 2. Where the space goes

### 2.1 Disk and database by day

Mimir, `node_filesystem_avail_bytes{host="by-us-fmt-5-edge-2",mountpoint="/"}` and
`pg_database_size_bytes`, at 20:00Z each day. Root-reserved blocks are 71.5 GiB, so
"avail" is what PostgreSQL can use. Inodes are 0.02% used.

| Date | Free | Database | What happened |
|---|---|---|---|
| 10-23 | 0 | n/a | disk full, outage |
| 10-24 | 7,298 | 6,640 | RAID-10 expansion to 14 TiB |
| 10-25 | 7,025 | 6,899 | |
| 11-01 | 5,196 | 8,722 | the reaper's current pass begins 15:30Z |
| 11-02 | 5,461 | 8,436 | client_reliability 21 → 14 days, index drops |
| 11-08 | 4,426 | 9,505 | |
| 11-10 | 4,240 | 9,695 | that evening: C4 prepared for week 11-16, O1 export floor live, O3 (client_reliability 14 → 10 days, +192 at 20:27Z) |
| 11-11 | 4,721 | 9,203 | O2's identity indexes (network_client, provide_key), 18:00–22:10Z |
| 11-12 | 4,789 | 9,139 | from 02:00Z the reaper meets late-closed contracts and starts skipping them |
| 11-13 | 4,789 | 9,137 | the reaper's pass reaches the horizon at 14:09Z; O2 partial 1 of 5 at 19:35Z |
| 11-14 | 4,799 | 9,125 | O2 partials 2 and 3 by 06:16Z, then stopped (no vacuum gap); bot sweeps from about 14Z |
| 11-15 | 4,636 | 9,274 | |

The free-space curve is a sawtooth: a steady fall of 6–15 GiB an hour, and a rise of
55–75 GiB at about 00:00Z when the oldest client_reliability day is dropped. From 11-15
00:07Z to 20:07Z free space fell 210 GiB, 252 GiB/day. The 11-12 to 11-14 plateau was
not stability: O2's rebuilds (−774 GiB from 11-11 to 11-14) and the reaper's earlier
mass deletes, whose freed space new rows reused, hid the growth.

### 2.2 Growth per relation, measured

Snapshots of every table family (partitions grouped under their root:
`pg_total_relation_size`, `pg_relation_size`, `pg_indexes_size`, TOAST, and the
`pg_stat_user_tables` counters), from 20:05:23Z to 21:35:40Z (90 minutes, about
2.4 sweep cycles). Rates are scaled to a day; rows are a second. These rates
predate I1 and I4.

| Family | Size | GiB/day | heap | index | TOAST | inserts/s | deletes/s | net rows/s |
|---|---|---|---|---|---|---|---|---|
| transfer_contract | 2,442 | +75.2 | +43.4 | +31.8 | +0.0 | 1,397 | 181 | +1,216 |
| client_reliability (daily drop) | 655 | +56.1 | +26.5 | +29.6 | +0.0 | 1,728 | 0 | +1,728 |
| contract_close_report_evidence | 572 | +32.6 | +13.9 | +17.5 | +1.2 | 1,317 | 600 | +717 |
| provider_work_session_event | 367 | +22.7 | +9.7 | +13.0 | +0.0 | 1,104 | 0 | +1,104 |
| contract_close_report | 325 | +18.8 | +10.2 | +8.6 | +0.0 | 1,317 | 136 | +1,180 |
| st_client_key_history | 983 | +15.8 | +0.0 | +0.6 | +15.2 | 68 | 28 | +39 |
| contract_close | 784 | +13.6 | +0.0 | +13.6 | +0.0 | 2,786 | 219 | +2,567 |
| transfer_escrow_sweep | 497 | +11.2 | +4.2 | +7.1 | +0.0 | 637 | 38 | +598 |
| network_client | 263 | +10.7 | +0.0 | +10.7 | +0.0 | 384 | 277 | +107 |
| provider_egress_health_history (daily drop) | 76 | +8.2 | +7.3 | +0.8 | +0.0 | 65 | 0 | +65 |
| network_client_location_reliability | 187 | +7.7 | +3.8 | +3.9 | +0.0 | 326 | 93 | +233 |
| audit_device_event | 73 | +5.7 | +2.0 | +3.8 | +0.0 | 223 | 0 | +223 |
| st_provider_usage_archive | 160 | +5.4 | +4.0 | +1.4 | +0.0 | 181 | 0 | +181 |
| provide_key | 138 | +5.2 | +0.0 | +5.2 | +0.0 | 731 | 934 | -204 |
| network_client_connection | 8 | +4.3 | +0.0 | +4.3 | +0.0 | 555 | 358 | +197 |
| provider_work_session_head | 54 | +2.6 | +1.5 | +1.1 | +0.0 | 351 | 0 | +351 |
| audit_device_state | 12 | +1.3 | +0.8 | +0.5 | +0.0 | 110 | 0 | +110 |
| network_client_location | 6 | +1.0 | +0.0 | +1.0 | +0.0 | 535 | 358 | +177 |
| st_client_key_head | 72 | +0.8 | +0.0 | +0.8 | +0.0 | 67 | 28 | +39 |
| provider_egress_url_security | 15 | +0.6 | +0.5 | +0.0 | +0.0 | 7 | 0 | +7 |
| client_tls_certificate | 6 | +0.3 | +0.3 | +0.0 | +0.0 | 12 | 2 | +10 |
| client_connection_reliability_score | 2 | +0.2 | +0.0 | +0.2 | +0.0 | 57 | 14 | +44 |
| verify_original_transition | 3 | +0.2 | +0.0 | +0.0 | +0.2 | 1 | 0 | +1 |
| client_reliability_running | 2 | +0.2 | +0.0 | +0.2 | +0.0 | 90 | 50 | +40 |
| **All families** | 9,294 | **+301.7** | | | | | | |
| **Net of the daily drops** (client_reliability and provider_egress_health_history) | | **+237.4** | | | | | | |

**Reading the table.**
- The six contract tables and the close evidence are 64% of the net growth.
  transfer_contract alone takes 75 GiB/day: 1,397 inserts a second against 181
  deletes, nearly all of them the prober retention's.
- transfer_escrow is not in the table because it did not grow at all: 1,433.52 GiB at
  both ends of the window, through 7.6M inserts. Its heap holds 264 GiB of free space
  that the reaper's 11-01..11-12 deletes left behind, and its indexes reuse the pages
  those deletes emptied. New escrows fill that first.
- contract_close's heap is flat for the same reason; its primary key grows.
- client_reliability and provider_egress_health_history are matched by their daily
  drops.
- network_client and provide_key grow only in their indexes: O2 rebuilt them on 11-11
  (network_client 417 → 139 GiB of indexes) and they are growing back toward their old
  density.
- provider_work_session_head and audit_device_state have **no deletes at all**
  (`n_tup_del` 0 since the 10-23 reset); they grow with every new client and device.
- network_client_connection's +4.3 is its vacuum cycle: it is vacuumed every few
  minutes and stays near 8 GiB.

### 2.3 Load

- **Contracts:** `urnetwork_contract_opened_total` summed over the services
  (api 898/s, proxy 328/s, taskworker 134/s, alt 3/s at 20:00Z) gives 1,363 a second;
  transfer_contract took 1,348–1,409 inserts a second in the snapshots. Daily means:
  560–735 a second from 11-06 to 11-13, 984 on 11-14, 1,312 on 11-15.
- **The live contract mix** (`pg_stats` histogram of `transfer_contract.create_time`,
  analyzed 19:39Z, 2.27B rows, each bound about 22.7M rows): about 16% are older than
  the 35-day horizon (10-11); about 23M a day were created 10-11..10-20, 60M a day
  10-21..10-29, 85M a day 10-30..11-04, 55M a day 11-05..11-13, 90M on 11-14 and over
  110M on 11-15. These are the cohorts a working reaper deletes 35 days later.
- **Bytes per contract** in the six contract tables: about 2.6 KB on average today with
  bloat (2.9 KB with the close evidence), about 1.55 KB compact (1.8 KB; bloat run of
  11-15 12:00Z), and about 1,215 bytes per added contract at the margin (their growth
  over net new contracts in §2.2, with transfer_escrow still on its free space).
- **Transaction ids:** about 10,100 a second (three `pg_current_snapshot()` reads,
  20:44–21:36Z), about 870M a day. `autovacuum_freeze_max_age` (200M) is passed about
  every 5.5 hours (§3.1).

## 3. Retention inventory

### 3.1 Every large relation

"Keeping up" means its deletes or drops match its inflow at the window it is meant to
keep. "Freed space" says whether what retention frees is reused in place (heap free
space, recycled index pages) or only returned by a rebuild. `vacuum_truncate` is off
server-wide, so no plain vacuum returns space to the filesystem.

| Relation | Size | Retention (task, window) | Running? Keeping up? | Freed space |
|---|---|---|---|---|
| transfer_contract | 2,440 | Contract horizon reaper (`remove_contracts_past_horizon`, row deletes at 35 days, mode on, control at the 2,000/s maximum); prober retention (127/s); C1 partitions built, not cut over (drops at 91 days) | Running; **not keeping up since 11-12** (§3.2); I1 fixes it | Heap: reused (compact today). Indexes: 822 GiB reclaimable, by rebuild only |
| transfer_escrow | 1,434 | Deleted with its contract (reaper, prober) | Same defect | Heap: 264 GiB free, reused in place. Indexes: 755 GiB reclaimable (pkey 324, balance_contract 314, the drained unsettled partial 117) |
| st_client_key_history | 982 | `remove_gone_st_client_key_history` every 15 min: a gone client's history after 30 days (`horizonDays` 30 on its brake; floor 30) | Running at 20–25 clients/s; registrations 71–73/s. The bulk, 113M clients gone 10-24..11-03, comes due **11-23..12-03** | TOAST and heap reused in place: about 360 GiB freed by the bulk *(11-08 report)* |
| contract_close | 783 | Deleted with its contract | Same defect | Heap: reused. pkey 154 GiB reclaimable |
| client_reliability | 653 (11 days + 3 ahead) | `remove_old_client_reliability_stats` drops a day at 00:00Z; 10 days (O3) | Yes; 55–64 GiB a day in and out | Returned by the drop |
| contract_close_report_evidence | 570 | `RemoveExpiredCloseEvidence` (in `maintain_contract_partitions`, every 15 min) at the contract horizon, 35 days, resolved or deleted contracts only; brake default 1,000/s; C5 built, not cut over | Yes today (176/s over 24 h, 600–800/s in the evening; the cohorts due are small). Needs 1,500/s or more from about 12-15 | Heap reused; indexes (pkey 51, census 41 GiB reclaimable) only by rebuild |
| transfer_escrow_sweep | 497 | Deleted with its contract, only when discharged (unpaid legacy sweeps forfeited 11-13) | Same defect | pkey 42, sweep_time 31 GiB reclaimable |
| provider_work_session_event | 366 (legacy + 5 weeks ahead) | C4 cut over with week C = 11-16; weekly drops at the contract horizon. The legacy leaf: **dropped tonight (I4)** instead of C + 35 days = 12-21 | Immutable, no deletes. New weeks grow about 23 GiB/day | Returned by the drop |
| contract_close_report | 325 | Deleted with its contract; prober retention (119/s) | Same defect | pkey 38 GiB reclaimable |
| network_client | 262 | Retired hosted children hard-deleted after 1 day, removed proxies after 2, others after 30; prober | Yes (279–334 deletes/s against 364–387 inserts/s) | Indexes regrowing after O2 |
| network_client_location_reliability | 187 | 15-minute batched delete, 31 days after last seen | Yes | Reused |
| st_provider_usage_archive | 160 (16 weeks + DEFAULT) | Capture on every contract delete (SN25 proof); O1 export to edge-6 and early drop under a weekly floor (close age 70 days); otherwise 52 weeks | Yes. Grows with the reaper's deletes | Returned by the weekly drop |
| provide_key | 138 | Deleted at deactivation; prober | Yes | Heap: 69 GiB free, reused |
| provider_egress_health_history | 76 | Daily partitions, 7 days | Yes | Returned by the drop |
| audit_device_event | 73 | Hourly row delete at 180 days | **First delete 2027-02-06**; grows about 6 GiB/day | Reused |
| st_client_key_head, provider_work_session_head | 72, 54 | key heads follow key history; **work-session heads are never deleted** | heads: yes; work-session heads: no retention | Reused |
| pending_task, finished_task, transfer_debit_journal | 39, 34, 7 | Queue churn; vacuumed every few seconds | Yes, but the files keep their high-water size: pending_task is a 28 GiB heap for 111k rows, finished_task 28 GiB for 12M rows (26 reclaimable), the journal 7 GiB for 36k rows | Only a rewrite, a partition drop or a swap (I5) |

Autovacuum keeps up on all of them: dead rows are 0–1.4% on the big tables, so their
space is live rows, heap free space and index bloat. But it is nearly continuous. At
10,100 transaction ids a second a table passes `autovacuum_freeze_max_age` (200M) about
5.5 hours after its last freeze, and a pass over transfer_contract spends hours in its
1,480 GiB of indexes. A pass freezes only rows older than `vacuum_freeze_min_age`
(50M), so one that runs longer than about 4 hours (150M more ids) ends with its table
already past 200M, and the next pass is an anti-wraparound one, which nothing can
cancel. At 20:30Z
contract_close and transfer_escrow were at xid age 227M and 223M, transfer_contract at
178M.

### 3.2 The reaper stopped deleting on 11-12/13

**What it is meant to do.** `model.RemoveContractsPastHorizon` walks
transfer_contract's primary key from a cursor up to `contract_id_floor(now − horizon)`
in batches. A contract is deleted, with its closes, escrows, sweeps, reports and
extenders, when it is resolved, its close time is before the horizon, its escrows are
settled and its sweeps discharged. The cursor then moves to the batch's last id,
**whether or not the batch's contracts were deleted**. A pass that reaches the horizon
starts over from the oldest contract once it began `RescanInterval` ago: 30 days when
this was written, 7 days since the owner's go on 11-16 (§6).

**What happens once it is caught up.** At the horizon the window holds contracts
created in the last few minutes before `now − 35 days`. They close 12 minutes or more
after creation (the quiet cutoff; up to the 60-minute deadline), so their close time
is after the horizon. The scan counts them as `recent_close`, keeps them, and the
cursor moves past them. Nothing returns to them until the next rescan.

**Measured** (`urnetwork_contract_horizon_*`, taskworker):

| Six hours to | Scanned/s | Eligible/s | Kept as `recent_close`/s | Lag |
|---|---|---|---|---|
| 11-11 20:09Z | 1,923 | 1,915 | 0.005 | 226 h |
| 11-12 08:09Z | 1,929 | 21 | 1,908 | 187 h |
| 11-13 08:09Z | 1,929 | 293 | 1,636 | 34 h |
| 11-13 20:09Z | 196 | 0.25 | 195 | 0 |
| 11-14 14:09Z | 136 | 0.08 | 136 | 0 |
| 11-15 14:09Z | 358 | 0.21 | 358 | 0 |
| 11-15 20:09Z | 316 | 9.2 | 306 | 0 |

- Until 11-12 the pass was weeks behind, every contract it met had closed long ago,
  and it deleted 1.53B contracts in 15 days (3.06B closes, 3.54B escrows, 188M
  sweeps), all archived.
- From 11-12 02:00Z it reached the contracts of 09-29..10-10, many of which ran to
  deadlines of up to 10 days 16 hours (CONTRACT-PARTITION.md, "Contract deadlines").
  Their closes fell after the horizon, so most were skipped.
- From 11-13 14:09Z it has been at the horizon, and almost every contract is skipped.
  The protected counters for every other reason (payment holds, block points,
  unresolved, unsettled, busy) read zero.
- In four days 253M contracts were kept as `recent_close` and 42M were eligible. The
  pass began 11-01 15:30Z, so its rescan would start about 12-01 15:30Z and need 5–6
  days at 2,000/s to reread the pile, after which the same skip would start again: in
  effect a horizon of 35 to 65 days and more.
- The monitor's §2.4r signal watches runs, errors, lag growth and block-point
  protection. Lag reads zero, so nothing fired.

**Fix: I1, approved by the owner at about 20:45Z on 11-15 and being built separately**
(server branch `fix/contract-reaper-cursor-1115`, taskworker roll the same night).
This plan assumes it is live from 11-16. What it has to achieve, whatever its exact
shape:
- The cursor must not pass a contract only because its close is still inside the
  horizon. One way: walk ids only up to `contract_id_floor(now − horizon − bound)`,
  with the bound at least the longest normal contract life (the 75-minute
  `CompanionOriginCloseLinger`, or 2 hours), so every contract the window reaches has
  already closed. Contracts that close later than that (expiry repair, an outage) are
  left to a rescan.
- The pile skipped since 11-12 must go now, not at the 12-01 rescan: an immediate
  rescan from the oldest contract. A shorter `RescanInterval` (7 days instead of 30)
  does that on the first caught-up run, since the current pass began 11-01, and keeps
  later rescans cheap. *(11-16: I1 started its new pass at 11-15 23:05Z itself; the
  7-day interval came after it, for the contracts that pass keeps, §6.)*
- A test that fails on today's code: contracts created 5 minutes before the horizon
  and closed 12 minutes after creation; a caught-up run must not move past them, and
  once their closes pass the horizon a later run deletes them.
- A signal: §2.4r should also warn when a caught-up reaper keeps more than half of
  what it scans as `recent_close` for an hour (§5.6).

**Effect** *(inferred, model in §4)*: the backlog below the horizon, about 340M
contracts (16% of 2.27B by the histogram), goes in about two days at 2,000/s and frees
about 0.4–0.5 TiB inside the contract tables, which new rows then reuse. After that
the reaper deletes each 35-day cohort: about 23M a day until about 11-24, 60–85M a day from
late November, 90M or more from 12-19. At today's load the contract tables' growth
falls from about 119 GiB/day to about 100 GiB/day once the backlog's space is used,
and to about zero by 12-19. Every deleted contract is archived (about 213 bytes), so
the usage archive grows by up to about 40 GiB/day on the backlog days, then 5–20
GiB/day, until O1's weekly export floor caps it near 35 days of cohorts (§3.5).
Watch WAL, vacuum, debit lag and the archive while the backlog goes; 11-01..11-12 ran
at the same 1,930/s for eleven days without incident.

**I1 alone is not enough.** It turns permanent growth (about +180 GiB/day in the
no-fix case, once its 12-01 rescan is spent) into about zero by February, but the
transition does not fit: until 12-19 the cohorts it deletes are smaller than today's
inflow, and the other families keep growing.

### 3.3 `st_client_key_history`: what it is

- **One immutable, signed key registration per client identity** (every hosted proxy
  window child included), with a sealed 2,047-byte evidence blob. The blob is
  incompressible and stored out of line: about 2.76 KB of TOAST a row
  (`model/st_client_key_history.go`; the 11-08 report's 200-row sample). It is also
  published to MinIO.
- **Readers:** close-report admission (the original close key), the current-key
  readers and the SN epoch key overlay (`db_identity_retention.go`). It is SN25
  evidence.
- **Size:** 982 GiB: heap 196, TOAST 734, indexes 52 (pkey 32 GiB; the unique
  `registration_hash_key`, 20.6 GiB, has never been scanned and is an integrity
  guard).
- **Retention:** PARTITION-MAINTENANCE-PLAN Part D. A gone client's history is deleted
  once it has been gone `horizonDays` (30, set on the brake 11-02; the guard's floor
  is 30). Deletes run at 20–25 clients/s now; registrations arrive at 71–73/s and gone
  records at 18–25/s.
- **Next:** the 113M clients that went 10-24..11-03 come due 11-23..12-03, about 11M a
  day on average. At the brake's default 150/s (13M a day) the peak days (10-24, 10-26
  and 11-02, 15–18M each) fall behind for a day or two; the ramp planned on 11-02
  (300, then 500, then 1,000/s) keeps it current. They free about 360 GiB inside the
  table, which absorbs about three weeks of inflow. Afterwards it grows by the registrations
  of clients that are not gone.
- **To do:** per-table autovacuum settings before the bulk deletes (the 11-08 report;
  it has none, so a 20% scale factor under the 100M cap applies).

### 3.4 `provider_work_session_event_legacy`: is anything reading it?

- **What it is:** the pre-11-16 leaf of the provider work-session journal: an admit
  and a retire event for every connection of every client, provider or not, written by
  the `provider_work_session_append` trigger. 1.59B rows, 366 GiB.
- **The journal exists for a signer that was never deployed.** Migration 824 dropped
  the signer's index; a vault-loaded signer refuses to sign until it is rebuilt, and
  "old SQL rows are never promoted to originals when a signer later becomes available"
  (`model/provider_work_session.go`). So no legacy row would ever be signed.
- **The only reader is the append trigger's retire lookup:** a connection admitted
  before 11-16 finds its admitting network id there when it retires (bounded by the
  connection id's mint time less a day, falling back to an unbounded lookup). Its
  primary key (`provider_work_session_event_pkey_l`, 83.5 GiB) has never been scanned.
- **Scheduled reclaim was 12-21 at the earliest:** the registry drops the legacy leaf
  at C + 35 days, and only while no connected row's admission is still in it (the P1
  fix in PARTITION-MAINTENANCE-FIXES.md), so one long-lived provider connection could
  have held all 366 GiB past 12-21.
- **I4, approved at about 20:55Z on 11-15 and being done tonight by a separate agent:**
  a guard migration admits the legacy leaf's drop now. A pre-11-16 connection still
  open when it retires writes its retire event without a network id; with no signer
  there is no signed chain to break, which was the P1 concern. Copies exist in the
  edge-6 backups (the 11-10 base and later runs).

### 3.5 What the evidence, report and archive tables must keep

| Table | Who reads it | Must keep |
|---|---|---|
| contract_close_report | settlement, SN close originals, the reapers; billing of one-sided closes at the sender's count (owner 11-11) | as long as its contract: it is deleted with it |
| contract_close_report_evidence | admission (exact retries, refusing a reused report id) and the SN epoch census (windows of at most 32,768 closed contracts); not settlement, disputes or payouts | 35 days, the guard's floor (owner 11-01). An open dispute keeps its contract unresolved, and unresolved contracts keep their evidence at any age |
| transfer_escrow_sweep | payout history, operator block points, leaderboard, SN reconcile, reimbursements | with its contract once discharged; unchanged |
| st_provider_usage_archive | SN25 subnet usage proof, read by the epoch usage statement | on Main until O1 exports a week past the floor (close age 70 days); exported weeks live on edge-6 |
| st_client_key_history | original-close verification, SN epoch key overlay | 30 days after the client is gone |

Nothing here argues for shorter windows on its own. The windows are the owner's, and
each shortening needs a decision (§5.3, S5).

### 3.6 Bloat that only a rebuild returns

The weekly bloat run (`db_bloat_measurement`, 11-15 12:00Z: 405 relations, 8,887 GiB,
2,642 GiB estimated reclaimable) at current row counts:

| Index | Size | Reclaimable | Note |
|---|---|---|---|
| transfer_escrow_pkey | 422 | 324 | time-ordered; sparse survivors of the 11-01..11-12 deletes |
| transfer_escrow_balance_contract | 413 | 314 | |
| transfer_contract_create_time | 318 | 215 | 70M scans; M4 of the held index diet |
| transfer_contract_open_source_id | 323 | 163 | indexes closed rows too |
| contract_close_pkey | 339 | 154 | |
| transfer_contract_destination_id_create_time | 218 | 134 | |
| transfer_contract_pkey | 197 | 132 | |
| transfer_escrow_unsettled_balance_contract | 117 | 117 | drained partial, compact size about 0 |
| transfer_contract_destination_id_close_time | 211 | 108 | |
| contract_close_report_evidence_pkey, census | 140, 163 | 51, 41 | |
| transfer_escrow_sweep_pkey, sweep_time_covering | 105, 77 | 42, 31 | |
| contract_close_report_pkey | 143 | 38 | |
| transfer_contract_closed_usage | 138 | 35 | |
| transfer_contract_outcome_null, open_partial_create_time | 18, 10 | 18, 10 | drained partials (O2's last two) |

Heap free space, reused in place by new rows rather than lost: transfer_escrow 264,
provide_key 69, st_client_key_history 42 (TOAST not measured), contract_close 30,
network_client 21, pending_task 28, finished_task 26, transfer_debit_journal 7.

**A rebuild is partly a loan.** These indexes take new entries at about 2–3× their
compact density: every contract is inserted and then updated (about 1,600 updates a
second on transfer_contract, 3% HOT; none HOT on transfer_escrow), so the pages at the
insert edge split and are left part full. As the reaper empties old, compact pages,
they are recycled for new entries, so a rebuilt index grows back toward its churn
density over one retention cycle (about 35 days) *(inferred; network_client's
indexes, rebuilt 11-11, are growing back at about 12–19 GiB/day)*. Today's bloat also
holds sparse survivors of the 11-01..11-12 mass deletes, which do not come back. The
model assumes half of a rebuild's gain regrows (all of it in the stress case).
Permanent cuts come from fewer indexes (the index diet) or from partitions dropped
whole (C1).

## 4. Projection

`model.py` (evidence folder) steps free space a day at a time from 11-16 with the
measured rates of §2 and these assumptions:
- Load stays at 11-15's 1,350 contracts a second unless a scenario trims it; the
  prober retention keeps deleting 127/s.
- I1 is live from 11-16: the 340M backlog goes at 2,000/s, then each cohort of §2.3.
  In "Nothing done", each cohort is skipped and the 12-01 rescan walks the pile at
  2,000/s.
- I4 drops the legacy events (366 GiB) on 11-16; new event weeks grow until weekly
  drops start 12-28.
- transfer_escrow's 264 GiB of free heap absorbs its share first.
- Close evidence growth falls as its deleted cohorts approach today's inflow.
- Key history stops growing 11-23..12-20 while the bulk deletes' freed space is
  reused, then grows at 70% of today's rate.
- network_client's and provide_key's index regrowth stops at their pre-O2 sizes; the
  work-session heads, location tables and audit tables continue at today's rates.
- The usage archive receives every deleted contract; O1 drops weeks past 70 days.
- I2 frees 144 GiB on 11-18; I3 frees 1,500 GiB in four steps (11-22, 11-25, 12-01,
  12-04), half of it regrowing over 35 days (all of it in the stress case); I5 frees
  74 GiB, modelled in one step on 11-28.
- Base: 1,215 bytes per net contract. Stress: 1,450 bytes and full regrowth.

Free GiB at each date (a negative number is the shortfall: the disk would already be
full), base case:

| Scenario | 11-22 | 12-01 | 12-08 | 12-15 | 12-31 | 01-15 | 02-14 | lowest | 500 GiB floor | zero | GiB/day on 11-16 | GiB/day in Feb |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A. Nothing (no I1, no I4) |   3065 |   1258 |   1110 |     12 |  -2755 |  -5418 | -10625 | -10765 on 02-15 | 12-13 | 12-16 | +224 | +172 |
| B. I1 + I4, approved 11-15 |   3897 |   2508 |   1738 |    783 |   -165 |   -352 |   -727 | -727 on 02-14 | 12-17 | 12-24 | -324 | +11 |
| B2. B, trim −20% |   4183 |   3196 |   2701 |   2126 |   1624 |   1511 |   1283 | 1283 on 02-14 | none | none | -324 | +6 |
| C. B + I2, I3, I5; no trim |   4436 |   3825 |   3208 |   2095 |    823 |    573 |    198 | 198 on 02-14 | 01-17 | none | -324 | +11 |
| C2. C, trim −20% |   4723 |   4512 |   4171 |   3438 |   2612 |   2435 |   2208 | 2208 on 02-14 | none | none | -324 | +6 |
| C3. C, trim −35% |   4938 |   5028 |   4929 |   4471 |   3824 |   3703 |   3586 | 3586 on 02-14 | none | none | -324 | +3 |
| E0. C + events for providers only (12-01) + index diet (12-20); no trim |   4436 |   3842 |   3349 |   2360 |   1878 |   1600 |   1169 | 1169 on 02-14 | none | none | -324 | +12 |
| E. E0, trim −20% |   4723 |   4526 |   4284 |   3650 |   3572 |   3367 |   3084 | 3084 on 02-14 | none | none | -324 | +8 |
| D0. C + RAID-1 tablespace (+6,980 GiB) 12-15; no trim |   4436 |   3825 |   3208 |   9075 |   7803 |   7553 |   7178 | 2261 on 12-14 | none | none | -324 | +11 |
| D. D0, trim −20% |   4723 |   4512 |   4171 |  10418 |   9592 |   9415 |   9188 | 3549 on 12-14 | none | none | -324 | +6 |
| F. E + RAID-1 tablespace 12-15 |   4723 |   4526 |   4284 |  10630 |  10552 |  10347 |  10064 | 3747 on 12-14 | none | none | -324 | +8 |

Stress case:

| Scenario | 11-22 | 12-01 | 12-08 | 12-15 | 12-31 | 01-15 | 02-14 | lowest | 500 GiB floor | zero | GiB/day on 11-16 | GiB/day in Feb |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A. Nothing (no I1, no I4) |   2903 |    925 |    881 |   -341 |  -3479 |  -6488 | -12389 | -12552 on 02-15 | 12-11 | 12-14 | +247 | +195 |
| B. I1 + I4, approved 11-15 |   3844 |   2342 |   1528 |    503 |   -453 |   -604 |   -907 | -907 on 02-14 | 12-16 | 12-19 | -344 | +8 |
| B2. B, trim −20% |   4161 |   3106 |   2604 |   1994 |   1505 |   1427 |   1271 | 1271 on 02-14 | none | none | -344 | +4 |
| C. B + I2, I3, I5; no trim |   4383 |   3572 |   2776 |   1442 |   -152 |   -429 |   -732 | -732 on 02-14 | 12-22 | 12-27 | -344 | +8 |
| C2. C, trim −20% |   4700 |   4336 |   3852 |   2933 |   1805 |   1602 |   1446 | 1446 on 02-14 | none | none | -344 | +4 |
| C3. C, trim −35% |   4938 |   4909 |   4693 |   4077 |   3144 |   2995 |   2950 | 2950 on 02-14 | none | none | -344 | +0 |
| E0. C + events for providers only (12-01) + index diet (12-20); no trim |   4383 |   3590 |   2917 |   1707 |    898 |    588 |    218 | 218 on 02-14 | 01-23 | none | -344 | +10 |
| E. E0, trim −20% |   4700 |   4350 |   3965 |   3145 |   2760 |   2524 |   2301 | 2301 on 02-14 | none | none | -344 | +6 |
| D0. C + RAID-1 tablespace (+6,980 GiB) 12-15; no trim |   4383 |   3572 |   2776 |   8422 |   6828 |   6551 |   6248 | 1640 on 12-14 | none | none | -344 | +8 |
| D. D0, trim −20% |   4700 |   4336 |   3852 |   9913 |   8785 |   8582 |   8426 | 3071 on 12-14 | none | none | -344 | +4 |
| F. E + RAID-1 tablespace 12-15 |   4700 |   4350 |   3965 |  10125 |   9740 |   9504 |   9281 | 3269 on 12-14 | none | none | -344 | +6 |

These are linear sums: treat dates as ±1 week. The largest unknowns are the trim's
effect on contracts, whether the bot sweeps grow, and how fast rebuilt indexes regrow.
"Net growth in February" is not zero in any row: about 10 GiB/day remain from key
history of clients that are not gone, the work-session heads and audit device state
(no deletes), and audit_device_event until its first delete on 2027-02-06.

## 5. The plan

**Goal:** net growth at or below zero at today's load, then headroom to 50% free
(7,038 GiB free on today's volume, or 10,528 GiB with the extra tablespace).
- **Net growth:** with I1 every path in §4 falls to +3 to +12 GiB/day by February. The
  rest comes from tables with no deletes (the work-session heads, audit device state),
  key history of clients that are not gone, and audit_device_event until its first
  delete on 2027-02-06; S4's head retention takes most of it.
- **Headroom:** 50% needs the tablespace, the trim and the structural items together
  (§4, row F). On today's volume the realistic plateau is 1.4–3.6 TiB free, 10–26%.

### 5.1 Immediate (this week and next)

| # | Step | GiB | Effort | Risk | Status |
|---|---|---|---|---|---|
| I1 | Reaper cursor fix and immediate rescan (§3.2) | about +900 GiB of free space by 12-01 against doing nothing; growth about zero by February | small code change and test; taskworker roll under the settlement gate (`ST_QUIET_MIN=0`) | low–medium: WAL, vacuum, archive inserts | **approved 11-15 ~20:45Z; in progress** (`fix/contract-reaper-cursor-1115`, rolling tonight) |
| I2 | O2 partials: `transfer_escrow_unsettled_balance_contract`, `transfer_contract_outcome_null`, `transfer_contract_open_partial_create_time` | −144 | three `bringyourctl db reindex-index` runs, 20–40 min each | low; each holds the horizon while it builds | O2 approved; **vacuum-gap rule decided (a)**; execution later, first in line |
| I3 | O2 full indexes, after I1's backlog delete and a fresh bloat run: transfer_escrow's pkey and balance_contract, contract_close_pkey, the evidence and sweep keys, then transfer_contract's six | −1,500 to −1,800, about half regrowing over ~35 days | 25–35 h of supervised rebuilds over 2–3 weeks | medium: heap read twice per index, horizon held (915–1,400 s longest transaction in the 11-13/14 windows), extra space while building | O2 approved; rule (a); execution after the backlog delete; windows each need a go |
| I4 | Drop `provider_work_session_event_legacy` now (§3.4) | −366 | guard migration and the drop | low | **approved 11-15 ~20:55Z; tonight** (separate agent) |
| I5 | Queue tables: partition finished_task and transfer_debit_journal, swap pending_task (below) | about −74 once; then bounded | two registry families (one built), one swap command, one taskworker release | low–medium | **recommended**; decision |
| I6 | Measure the hosted idle trim (CPUOPT6 B #1, rolling tonight) | each −10% of contracts ≈ −16 GiB/day now | queries below | none | approved |
| I7 | Brakes and settings already designed: evidence brake to 1,500/s by about 12-15; key history brake ≥150/s for 11-23..12-03; per-table autovacuum for st_client_key_history | keeps retention on schedule | control writes; one migration or the A5 playbook tag | low | go |
| I8 | Days-to-full alert and the reaper signal (§5.6) | 0 | warp and monitor changes with tests | none | go |

**I2–I3: the vacuum gap, decided (a)** (owner, about 20:55Z). O2's contract walk
stopped on 11-14 at 11:26Z and 16:27Z with "no vacuum gap with every gate open within
18000s": `db reindex-index` refuses to start while its table is vacuumed, and these
tables are vacuumed back to back. Under (a) a rebuild starts right after the table's
anti-wraparound pass ends, and its lock request cancels the ordinary autovacuum that
would follow (PostgreSQL cancels a non-wraparound autovacuum that blocks another
lock). Two details for the execution:
- At 10,100 xids a second a long pass ends past 200M, so the *next* pass on
  transfer_contract, transfer_escrow and contract_close is often an anti-wraparound
  one too (§3.1). The reliable way to start "right after" is to have the rebuild's
  lock request already queued when the pass ends: the tool waits behind the running
  pass (up to its expected length) instead of refusing. Autovacuum takes tables only
  with a conditional lock, so it cannot jump ahead of a queued request. Ordinary
  queries are unaffected by a queued SHARE UPDATE EXCLUSIVE request.
- Each rebuild shrinks every later pass over its table, which opens more gaps and
  eases the anti-wraparound cycle. PostgreSQL 19's parallel index vacuuming helps the
  same way (§5.4).

The O2 gates stay: free space at least 1,000 GiB plus 2.5× the index's compact size,
validator idle with 15 minutes to its boundary, taskworker overdue and debit-lag stops,
no transaction older than 15 minutes, not from Saturday 22:00Z to Sunday 16:00Z, and
no edge-6 backup reading Main. Order: the three partials first; the full indexes after
the backlog delete and a fresh bloat run, which should raise every reclaimable figure.
Then turn on `priority_reindex_enabled` in `db_maintenance.yml` so the daily maintenance
keeps the drained partials compact; they regrow with churn.

**I5: partition the queue tables where a range empties, swap where none does.**
Recommended over VACUUM FULL or pg_repack (the owner's 10-24 rule excludes pg_repack;
CPUOPT6 D #8 proposed it; the method stays the owner's decision). Measured 20:05–20:50Z:
pending_task and finished_task each take about 30 rows a second in and out.

| Table | Today | Design | One-time | Ongoing |
|---|---|---|---|---|
| finished_task | 34.1 GiB (heap 27.9, 94% reclaimable by pgstattuple; 12M rows) | **C3, built 10-24:** daily `RANGE (run_end_time)` in the partition registry, 3 days ahead, each day dropped 7 days after it ends behind the owner fence, so failed Posts keep their 7-day retries. Its schema migrations are already on Main: the primary key is `(task_id, run_end_time)` and `finished_task_post_retry` exists | the legacy leaf (34 GiB) drops at C + 7 days | 8 days of completions, about 4–6 GiB at 2.6M a day *(inferred)*; never more than a day's high-water |
| transfer_debit_journal | 7.3 GiB for 36–57k live rows | **A new registry family:** daily `RANGE (contract_id)` on contract-day bounds (`contract_id_floor`), 3 days ahead, plus a DEFAULT leaf; a day drops once it is 2 days past and empty. The primary key `(balance_id, contract_id)` already contains the key, so its `ON CONFLICT` stays exact. A debit lands in its contract's day and is deleted seconds after it is applied, so each day empties within about the longest contract life plus the debit lag | the legacy leaf drops a few hours after C, once the last pre-C contracts have settled | a pin's high-water lasts only until its day drops; the shard walk stops crossing empty pages (CPUOPT6 D #8's CPU saving) |
| pending_task | 39.3 GiB (heap 27.8, 99% reclaimable; 111k rows) | **Not partitionable** (PARTITION-MAINTENANCE-PLAN C7): `UNIQUE (run_once_key)` is global and every run-once producer relies on `ON CONFLICT (run_once_key)`; `task_id` is the identity the claim fences use; rows can wait indefinitely, so no range empties. Partitioning it would need a separate run-once identity table and a poll query merging every leaf: a rewrite of the scheduler's core. **Instead, a drained swap**, below | 39 GiB → under 1 GiB | re-bloats only while a long transaction pins the horizon (the weekly payout, rebuild windows); repeat when the table-bloat signal (SIGNALS.md §2.4k) fires, or use REPACK (CONCURRENTLY) after the 19 upgrade |

Cutover, in the pattern C3 and C4 already follow:
1. **Additive migration.** finished_task: none left. transfer_debit_journal: the
   family's functions and its monitor contract. pending_task: a `pending_task_next`
   table with the same columns, keys, indexes, reloptions (fillfactor 50, its
   autovacuum options) and grants. None takes a lock on the live table.
2. **Code switch,** one taskworker release: the journal family's runway and retention
   (drop precondition: the day is empty), the partition-aware readers (every delete
   that names rows by ctid must name them by key: a ctid repeats across leaves, the
   finding of the contract reviews), and the monitor's catalog contracts for the new
   shapes. finished_task's code is already deployed.
3. **Prepare.** A validated `CHECK (key < floor(C))` on the live table, resumable
   (finished_task on `run_end_time`, the journal on `contract_id`); the parent staged
   with `LIKE … INCLUDING INDEXES` and the leaves C..C+3, plus the journal's DEFAULT.
4. **Drain and swap.** Wait for a quiet table, then one short transaction with 3 s lock
   waits, retried: rename the old table to `_legacy`, attach it as the legacy leaf
   below C, rename the staged parent. The task queue and the debit chains wait
   seconds. For pending_task the swap is a copy: take ACCESS EXCLUSIVE on pending_task
   (3 s lock waits, retried), copy the ~111k live rows through the primary key index
   (seconds to tens of seconds, not a 28 GiB scan), rename both tables and their
   indexes, commit. Done while the taskworker is stopped for the 18.7 restart (§5.4),
   it needs no window of its own.
5. **The old table goes.** The registry drops the legacy leaves when their ranges
   expire (finished_task at C + 7 days, the journal once empty). pending_task's old
   table is dropped right after its swap.

Effect: about −74 GiB once (pending_task 38, finished_task about 29 net of its 8-day
steady state, the journal 7). finished_task and the journal can then never hold more
than a day's high-water, whatever vacuum does; pending_task stays compact until the
next long pin.

**I6: measuring the trim.** Before and 24 h after the trim reaches every proxy host,
across at least six sweep cycles (they come every 38 minutes):
`sum(rate(urnetwork_contract_opened_total[1h]))`, transfer_contract and transfer_escrow
inserts a second (two `pg_stat_user_tables` snapshots an hour apart, as in §8),
provider_work_session_event and network_client inserts, st_client_key_history inserts,
and `urnetwork_proxy_window_clients`. CPUOPT6 B #1 expects 22–37% fewer window clients;
how much that cuts contracts is not known.

### 5.2 Capacity

- **More disk on edge-2.** Two 7.68 TB SSDs as a *separate* RAID-1 virtual disk with
  its own tablespace: about +6,980 GiB, online. Move whole indexes with
  `REINDEX (TABLESPACE …) CONCURRENTLY`, which rebuilds as it moves. Do not expand the
  RAID-10 in place: on 10-24 the online expansion forced a second full outage. **First
  step: find out whether edge-2 has free bays** (iDRAC inventory or remote hands). Lead
  time for drives and a visit is likely 1–3 weeks. Owner: spend.
- **Cold data on edge-6.** edge-6's 29 TiB pool holds exports, not live data. The O1
  usage-archive export is live (first real export 11-16 15:30Z). After C1, legacy
  contract weeks can be exported from edge-6's restored copy before they are dropped,
  with no load on Main.
- With +6,980 GiB, the 50% goal comes within reach (§4, row F) and C1's headroom
  exists.

### 5.3 Structural

**S1. Contract partitioning (C1).** Built 10-23; its review findings were fixed in
09cd98565; not cut over. What is left:
- Drops at the reaper's horizon (35 days), not the 91-day `ContractRetention`, as C4
  and C5 already do (`db_contract_partition_maintenance.go:49`). Code, guard and tests.
- Headroom. After the cutover new weeks stop reusing the legacy leaf's freed space, so
  it needs about 35 days of new contract data free, at 1.5–2 KB a contract: about 6–7
  TiB at today's load, 4–5.5 TiB after a 20–35% trim *(inferred)*. **Not available on
  the current volume**: C1 waits for the capacity of §5.2.
- The legacy leaf (about 5.4 TiB of today's contract tables) shrinks only by the reaper's
  row deletes until it is dropped, or by a split-by-copy into monthly leaves, which
  needs space.
- Prepare's VALIDATE of the legacy bounds reads each heap under SHARE UPDATE EXCLUSIVE
  and holds the horizon for hours; run it off-peak and outside backups.
- What it buys: retention by DROP, no sparse-survivor bloat, no rebuild loans, smaller
  insert-path indexes. The weekly drop still archives each contract by row delete
  first.

**S2. Close evidence partitioning (C5).** Built, not cut over. Its legacy leaf is never
dropped and `vacuum_truncate` is off, so a cutover returns nothing until a legacy-drop
step exists. Not now.

**S3. The index diet.** `transfer_contract_create_time` (318 GiB, M4),
`transfer_contract_open_payer_network_id_transfer_byte_count` (38, M5), and
`transfer_contract_open_source_id` (323) as a partial index on open rows: about
600–680 GiB **permanently**, and about 15–20% fewer bytes for every future contract
*(inferred)*. Readers must be rewritten first (`feat/index-diet-v2` df37ab075, held
since 11-02). Development and owner.

**S4. Inflow at the source.**
- The idle trim (I6).
- **Work-session events only for provider clients.** The journal has no reader beyond
  the retire lookup and no signer. Writing it only for providers would save most of its
  20+ GiB/day and of its steady state *(inferred: most connections are hosted window
  clients, which are payers)*. This reverses the 10-24 "not chosen": owner decision.
- **Retention for provider_work_session_head and audit_device_state,** which have no
  deletes: about 4 GiB/day between them.
- Lazy contracts: about 60% of contracts carried no data when measured on 11-05. A
  protocol change; the largest long-term lever on every contract table.

**S5. Retention windows** (each an owner decision; none is recommended now):
- The usage archive's O1 floor, close age 70 → 49 days: about −0.4 TiB at steady state.
  SN25 epoch reads set the floor.
- audit_device_event 180 → 90 days (O5): it caps near half its 180-day size. Check
  readers beyond `/stats/last-90`.
- A contract horizon below 35 days: every week shorter is about 20% less contract data
  at steady state. It needs code (the 35-day floor), guard migrations for evidence, and
  changes what users see in provider stats and `/transfer/stats`. Contingency only.

### 5.4 PostgreSQL: 18.7 now, 19 in mid-December

**Release status.** 18.7 shipped 2026-11-12 ([18.7 notes][pg-187]); PGDG has
`18.7-1.pgdg22.04+2` for jammy, which edge-2 runs (Ubuntu 22.04), and
`18.7-1.pgdg24.04+2` for noble. 19 RC 2 shipped the same day, and 19's general
availability is planned for 2026-11-19 ([announcement][pg-rc2];
[open items][pg-open]); that is unconfirmed until it ships. A mid-December upgrade
would run 19.0, about four weeks after GA. The owner chose 18.7 first (11-15, about
20:55Z).

#### Step 1: 18.7

- **Why first.** 18.7 fixes incremental-backup corruption ("existing incremental
  backup files should not be trusted"); one of its cases, stale visibility-map bits
  after a vacuum truncation, applies when both `data_checksums` and `wal_log_hints`
  are off, which is Main's configuration (`vacuum_truncate = off` limits that case; the
  merge-order and inclusion bugs do not depend on it). It also fixes row loss in table
  rewrites ("loss of tuples updated by later-aborted subtransactions") and the
  TOAST-index `REINDEX CONCURRENTLY` bug, and the release fixes 52 security
  vulnerabilities.
- **Post-update steps that apply: none.** All 1,408 indexes on Main are btree (no GIN or
  GiST), no column is an unbounded `bpchar`, and pg_trgm is not installed (read 11-15).
- **Playbook path: none exists yet.** playbook-dbs.yml disables automatic APT updates
  and manages no PostgreSQL packages, and playbook-edges.yml's `apt_updates` tag runs
  only on the `edges` group (edge-2 is in `dbs`). edge-2's apt lists date from 10-19.
  Add a tag to playbook-dbs.yml, run as `./run-dbs.sh --postgresql-minor-only`
  (`--check` first), that:
  - refreshes the PGDG source and the cache;
  - installs exactly `postgresql-18`, `postgresql-client-18` and `libpq5` at
    `18.7-1.pgdg22.04+2` (and `postgresql-18-repack` only if apt requires a newer build
    with them), with `policy_rc_d: 101` so no package script restarts the cluster;
  - holds them, verifies the installed versions, and never restarts PostgreSQL. Tests
    enforce that, as they do for the huge-pages block.
  Run it inside the restart window, minutes before the restart, so the running server
  never loads a newer library.
- **The restart window**, following factury/postmortem/2026-10-13-hugepages-restart.md
  §6–§9:
  - outside the Sun/Thu 06:00Z backup runs and the Saturday 22:00Z–Sunday 16:00Z payout
    block; the validator idle with at least 15 minutes to its boundary; both deploy
    locks held so no release rolls; no O2 rebuild from T−30 to T+30 minutes;
  - T−10: stop the taskworker containers, not only their units (`docker stop -t 60`,
    then `docker ps` must list none). `CloseExpiredNetworkClientHandlers` deletes connect
    handlers 120 s into an outage; on 10-13 the containers survived a unit stop;
  - the pending_task swap of I5 fits here, with the taskworker stopped;
  - T−2: two CHECKPOINTs, so the shutdown checkpoint is short;
  - T0: install the packages (the tag), then restart PostgreSQL. With huge pages
    reserved at boot and already in use, the stop should take well under 10-13's 100–140
    s and no pool reservation is needed;
  - **expect the post-start fork stall.** On 10-13 the postmaster forked at about 0.4
    backends a second for about 23 minutes while fresh huge pages were touched; 31 of
    32 PgBouncer pools starved and connect containers exited until it cleared. Plan for
    up to about 25 minutes of degraded service;
  - T+3 onward: start the taskworker once the `stale` check reads 0; return any runtime
    PgBouncer SETs to the file values; verify `SELECT version()` reads 18.7 and
    `huge_pages_status` is on.
- **Then a new full base backup on edge-6** (a forced full run), as 18.7's notes
  recommend: about 20 hours reading edge-2. It needs the full-copy time limit raised
  first (the 11-10 copy used 71,482 of `BRINGYOUR_PG_COPY_MAX_SECONDS_FULL`'s 72,000 s,
  and the database has grown since), and no I3 rebuild while it runs. The 28-day rule
  counts from the last full copy, so the full copy otherwise due after 12-08 moves to
  about 28 days after this one.

#### Step 2: 19, mid-December

**Methods, for disk.**

| Method | Extra disk | Bloat | Outage | Verdict |
|---|---|---|---|---|
| `pg_upgrade --link` | a few GB of catalogs | carried over | minutes, plus the restart (above) | **feasible**; the old cluster is unusable once the new one starts, so the rollback is a restore from edge-6 |
| `pg_upgrade --swap` (since 18) | as link | carried over | can beat link with many relations | feasible, same rollback; the old cluster is unsafe once the transfer begins |
| `pg_upgrade --copy` | about the database again, 9.3 TiB | carried over | hours | impossible at 4.6 TiB free |
| `--copy-file-range` | on ext4, a real copy | carried over | hours | impossible here |
| `--clone` | none | carried over | minutes | not available: needs reflinks (XFS or Btrfs); `/` is ext4 |
| Logical replication into a fresh 19 cluster | a target of at least ~10 TB | **all reclaimed**: about 2.6 TiB of index bloat and ~0.5 TiB of heap free space | a cutover | not now (below) |
| Dump and restore | a target as above | all reclaimed | days | impossible |

Sources: [pg_upgrade 19][pg-upgrade].

**Why not logical replication now.** It is the only method that compacts, but here:
- the target needs about 10 TB usable for today's ~6.3 TiB of compact data plus
  growth during the sync, and no such host exists;
- on 18 the publisher needs `wal_level = logical`, which is a restart of Main (19's
  automatic enablement applies only to a 19 publisher);
- each table's initial copy holds a snapshot on Main for its whole duration, hours for
  transfer_contract: the pinned horizon that degraded the taskworker on 11-04. At
  10,100 xids a second, a snapshot held a day also pushes every big table's xid age
  past a billion;
- the subscriber must apply about 40k commits a second; sequences, DDL and large
  objects do not replicate, so migrations freeze meanwhile.

Revisit it with new hardware, as a way to move to a bigger compact cluster.

**Recommendation: `pg_upgrade --link`** (or `--swap`) in a planned window, with:
1. **At least 2 TiB free going in**, for the backup runs, the restart and any
   post-upgrade rebuilds. In the projection that needs the trim or more disk (§4).
2. **A trustworthy rollback base:** the full copy taken on 18.7 (step 1) and its later
   incremental runs.
3. **A new full base on 19 right after.** The incremental chain cannot continue across
   the upgrade *(inferred: a new cluster with new WAL summaries; the docs do not say)*.
   edge-6's scripts need 19's `pg_basebackup`, `pg_combinebackup`, server and
   `pg_dump`.
4. **After:** `pg_upgrade` carries most optimizer statistics; run
   `vacuumdb --all --analyze-in-stages --missing-stats-only`, then
   `vacuumdb --all --analyze-only`, and the extension update script it writes
   (pg_stat_statements is at 1.10 on Main). No pg_trgm, full-text or btree_gist indexes
   exist, so 19's reindex notes do not apply.
5. **Config:** 19 removes `io_workers` (now `io_min_workers` and `io_max_workers`),
   doubles the `max_locks_per_transaction` default, and turns JIT off by default; the
   playbook's postgresql.conf must follow ([19 notes][pg-19]).

**What 19 changes for disk** ([release notes][pg-19]; [REPACK reference][pg-repack]):
- **REPACK** replaces VACUUM FULL and CLUSTER, and **REPACK (CONCURRENTLY) made the
  release**: it rebuilds without an exclusive lock except for the final swap, using
  logical decoding and a replication slot (`max_repack_replication_slots`). It needs a
  primary key or an index replica identity, works per leaf (not on a partitioned
  parent), cannot run in a transaction block, and can fail on concurrent DDL.
- **Parallel autovacuum of a table's indexes** (`autovacuum_max_parallel_workers`,
  per-table `autovacuum_parallel_workers`) and **autovacuum scoring**. For
  transfer_contract's hours-long index passes this is the most useful change: shorter
  passes, more REINDEX gaps, an easier anti-wraparound cycle. Set it in the playbook
  after the upgrade.
- New TOAST data compresses with lz4 by default (key history's evidence is
  incompressible, so little gain there). Table scans can set all-visible bits.
  Wraparound warnings start at 100M.
- **Not in the notes:** nothing about `summarize_wal`, WAL summaries or
  `pg_combinebackup` beyond a `backup_type` column in `pg_stat_progress_basebackup`; no
  btree format change; no new pg_upgrade transfer mode.
- **Extensions:** pgstattuple ships with 19 (faster, streaming reads). PGDG publishes
  `postgresql-19-repack` 1.5.3-2 for jammy (2026-11-12, [package][pg-repack-pkg]); an
  upstream statement of 19 support was not found. The owner's rule excludes pg_repack
  anyway.

**REPACK's real yield on the big tables is small and its costs are large.** Their dead
rows are 0–1.4%; their space is live rows, index bloat, and heap free space that new
rows reuse.
- *Yield:* the index bloat (~1.8–2.0 TiB) is the same whether REPACK or REINDEX
  CONCURRENTLY rebuilds it, and REINDEX works today on 18 without a heap rewrite.
  REPACK adds only the heap free space: about 440 GiB across the big tables, 264 of it
  in transfer_escrow, which new escrows are already consuming. A repacked table
  refills that space within a retention cycle.
- *Space:* "free disk space at least equal to the sum of the table size and the index
  sizes": 1.4 TiB for transfer_escrow, 2.4 TiB for transfer_contract, plus the captured
  changes on disk.
- *WAL:* the new heap and indexes are WAL-logged, roughly the table's compact size
  again (0.7–1.6 TiB for those two), on top of today's 44–54 MB/s. That loads the
  checkpoints and the WAL summarizer, and a run during a backup could push the backup
  slot past its 32 GB `max_slot_wal_keep_size` and invalidate it.
- *Horizon:* "REPACK is considered a single long-running transaction, which prevents
  VACUUM from cleaning up dead rows from other tables", for hours on these tables.
- *Limits:* it fails "if more than about 104 million rows are concurrently updated or
  deleted". transfer_contract takes about 1,600 updates a second plus the reaper's
  deletes, 6–12M an hour, so a run longer than about 9–17 hours fails after most of
  its work *(inferred)*.
- *Correctness:* "not MVCC-safe": a transaction whose snapshot predates the swap sees
  the table as empty. `server.Tx` defaults to REPEATABLE READ, so the application must
  be drained around the swap, as A4 of PARTITION-MAINTENANCE-PLAN.md says.
- **So:** no REPACK for the large tables. REINDEX CONCURRENTLY for index bloat now, C1
  for the contract tables' long-term shape, and REPACK (CONCURRENTLY) after the upgrade
  only where a table is small (pending_task, if it bloats again).

### 5.5 Schedule

| When | Step | Notes |
|---|---|---|
| 11-15 night | I1 rolls with the taskworker; I4 drops the legacy events (separate agents, approved) | the rescan starts on the reaper's first caught-up run |
| 11-16..11-18 | The backlog delete (about 2 days at 2,000/s): watch WAL, vacuum, debit lag, archive growth, and `recent_close` falling to about zero. I6 before/after measurement. I8 alert and signal written. The 18.7 playbook tag and the I5 code written | read-only and code |
| 11-18 | I2 partials, under rule (a), once the backlog delete is done (its deletes keep autovacuum busy on the same tables) | **no heavy I/O from 11-19 05:00Z** until the Thursday backup ends |
| week of 11-16 or 11-23 | The 18.7 restart window (owner picks), with the pending_task swap inside it if its code has landed, then the full base copy on edge-6 (about 20 h; limit raised first) | no I3 during the full copy |
| 11-20..11-28 | Fresh bloat run; I3: transfer_escrow's two, contract_close_pkey, sweeps, evidence; the I5 release, then the finished_task and journal cutovers | outside Sun 11-22, Thu 11-26 and the Saturday payout block |
| 11-23..12-03 | Key history bulk deletes: check its brake and vacuums | |
| 11-26..12-06 | I3: transfer_contract's six (largest heap, 1–2 h each) | outside the Sun/Thu runs |
| by 12-08 | Evidence brake to 1,500/s; decide 19's date from the free space and the trim's measured effect | |
| mid-December | PostgreSQL 19 `--link` window, then a full base on 19 | §5.4, only with ≥2 TiB free |
| after | Parallel autovacuum settings; capacity; C1 | |

### 5.6 Monitoring

**A days-to-full alert in Grafana** (`warp/grafana/alerting/db-disk.yml`, new, with a
test beside `data_disk_alerts_test.go`). The monitor's §2.4b warns only at an absolute
100 GB, and §2.4c's forecast was never built. Runway to a 500 GiB floor, on the net
change over 24 hours, which includes exactly one 00:00Z drop:

```promql
(max by (host) (node_filesystem_avail_bytes{env="main",host="by-us-fmt-5-edge-2",mountpoint="/"}) - 500 * 2^30)
/ clamp_min(
    max by (host) (node_filesystem_avail_bytes{env="main",host="by-us-fmt-5-edge-2",mountpoint="/"} offset 1d)
  - max by (host) (node_filesystem_avail_bytes{env="main",host="by-us-fmt-5-edge-2",mountpoint="/"}),
  2^30)
```

The result is days. Proposed rules:

| Rule | Condition | For | Severity |
|---|---|---|---|
| DbDiskRunwayLow | runway < 30 days | 3 h | warn |
| DbDiskRunwayCritical | runway < 10 days, or free < 1,000 GiB | 1 h | page |
| DbDiskTelemetryMissing | `absent(...)` of the series | 15 m | warn |

- Backtested over 10-31..11-15 it read 19–35 days through the growth of early
  November, reads 25.8 days now, and would have warned since 11-15 08:25Z. It went
  below 10 days only on 11-01 (8.7), so the page would not have been noisy.
- 1,000 GiB is the free space under which O2's rebuilds refuse to start.
- A 3-day `predict_linear` over the daily minimum was tried and rejected: the
  11-12..11-14 plateau, which O2's frees produced, made it read 15 GiB/day today.

**The reaper signal** (§3.2): warn when a caught-up reaper keeps more than half of what
it scans as `recent_close` for an hour, and when its eligible rate is below 1% of the
contract creation rate for 6 hours while its mode is on.

**Retention watch:** `urnetwork_close_evidence_retention_removed_rows_total`,
`urnetwork_retention_deleted_rows_total{table="st_client_key_history"}`,
`urnetwork_contract_horizon_{eligible,scanned}_contracts_total`, the weekly bloat run,
and the backup runs' durations.

## 6. Decisions

**Decided on 11-15:**
- **I1**, the reaper cursor fix and immediate rescan: approved about 20:45Z, in progress
  (`fix/contract-reaper-cursor-1115`).
- **I2–I3 vacuum gap: (a).** A rebuild starts right after the anti-wraparound pass and
  its lock request cancels the ordinary autovacuum. Execution later: partials first, the
  full indexes after the backlog delete.
- **I4**, dropping the legacy work-session events: approved about 20:55Z, done tonight.
- **PostgreSQL 18.7 first,** then 19.

**Decided on 11-16:**
- **The reaper rescans weekly** (`ContractHorizonSettings.RescanInterval` 30 → 7 days),
  approved about 03:45Z; it ships with the next taskworker. The pass I1 started at 11-15
  23:05Z moves past what it keeps as `payment_review`, `payment_recent` or
  `unsettled`. About 5.8M `payment_recent` contracts qualify about 11-19/20: with 30
  days they waited until about 12-15, weekly they go about 11-22. The cost: each week
  the rescan rereads the roughly 21M contracts kept for their payments at about 1,900 a
  second, about three hours. A pass still behind its horizon when the week ends reads
  on from its cursor and starts over once it reaches the horizon, as before. The 2,000/s
  cap, the batch bounds, the 35-day floor and the 2-hour `CloseSettleTime` are unchanged.

**Open:**
1. **Queue tables (I5):** the recommended partition-and-swap design, or VACUUM FULL in a
   window (on 18.7 or later), or an exception to the 10-24 no-pg_repack rule, or 19's
   REPACK after the upgrade.
2. **Capacity:** check edge-2's free bays now, and order two 7.68 TB drives for a RAID-1
   tablespace if they exist.
3. **18.7:** the window, and the playbook tag for it.
4. **19:** `--link` (or `--swap`), and the date, against the free space and the trim's
   measured effect.
5. **Structural items:** events only for provider clients and retention for the
   work-session heads (S4, the first reverses a 10-24 choice); the index diet (S3); C1
   with drops at the reaper's horizon once capacity exists (S1).
6. **Alerts:** the days-to-full rules and the reaper signal (§5.6).
7. **Windows, only if the trim and the steps above fall short:** the archive floor,
   audit retention, and as a contingency a contract horizon below 35 days (S5).

## 7. Constraints and risks

- **Migrations** are additive and applied before the code that needs them; never with
  PGOPTIONS (PgBouncer refuses the startup option; 10-13). I4's guard migration, I5's
  journal family and S1's horizon change are the migrations here.
- **Index work is concurrent only** (`REINDEX INDEX CONCURRENTLY`, through
  `db reindex-index`).
- **No long snapshots on Main:** every rebuild holds the horizon while it builds (the O2
  gates watch it); no REPACK of large tables; no dumps on Main.
- **Heavy I/O outside the backup runs** (Sun and Thu 06:00Z, and the post-18.7 full
  copy) and the validator's busy minutes.
- **One contract size stays** (owner 11-05); nothing here changes contract sizing.
- **Settings and host changes** go through the xops playbooks.
- **A primary restart** (18.7 now, 19 later) costs up to about 25 minutes of degraded
  service while huge pages are first touched; stop the taskworker containers first.
- **The bot sweeps may grow.** The owner declined blocking them on 11-14. If the
  runway alert reads under 10 days with the steps above in place, the contingency
  levers of S5 come back to the owner.

## 8. Method and evidence

- **Load on Main:** about 40 read-only statements through `support db query` (READ ONLY
  transaction, 30 s cap), each 1–3 s: 9 family snapshots (one statement each over the
  catalog and `pg_stat_user_tables`), partition lists, index sizes and scans,
  reloptions, settings, extensions, `pg_stat_progress_vacuum`, xid ages and the next
  xid, replication slots, `pg_ls_waldir()`, `db_bloat_measurement` and `pg_stats`
  histograms. No heap scans, no pgstattuple, no snapshot held between statements.
- **Mimir:** `node_filesystem_*`, `pg_database_size_bytes`, `pg_wal_size_bytes`,
  `urnetwork_contract_horizon_*`, `urnetwork_close_evidence_retention_*`,
  `urnetwork_retention_deleted_rows_total`, `urnetwork_prober_retention_*`,
  `urnetwork_due_contract_reap_*`, `urnetwork_contract_opened_total`,
  `urnetwork_build_info`.
- **Files** (`$T/dbdisk1115/`, where `$T` is the dcdb2db1 job tmp): `snap.sh` and the
  `snap-*.tsv` snapshots, `xid.txt`, `scripts/compare.py`, `scripts/growth_table.py`,
  `scripts/model.py` (run as `model.py 1215 0.5` for the base case and
  `model.py 1450 1.0` for the stress case), `scripts/fill.py`, `scripts/mq.py`.
- **Sources for §5.4:**
  - [PostgreSQL 18.7, 17.12, 16.16, 15.20, 14.25 and 19 RC 2 released][pg-rc2]
  - [PostgreSQL 19 open items (GA date)][pg-open]
  - [PostgreSQL 18.7 release notes][pg-187]
  - [PostgreSQL 19 release notes (RC 2 docs)][pg-19]
  - [REPACK reference, PostgreSQL 19][pg-repack]
  - [pg_upgrade, PostgreSQL 19][pg-upgrade]
  - [postgresql-19-repack for jammy-pgdg][pg-repack-pkg]

[pg-rc2]: https://www.postgresql.org/about/news/postgresql-187-1712-1616-1520-1425-and-19-rc-2-released-3442/
[pg-open]: https://wiki.postgresql.org/wiki/PostgreSQL_19_Open_Items
[pg-187]: https://www.postgresql.org/docs/release/18.7/
[pg-19]: https://www.postgresql.org/docs/19/release-19.html
[pg-repack]: https://www.postgresql.org/docs/19/sql-repack.html
[pg-upgrade]: https://www.postgresql.org/docs/19/pgupgrade.html
[pg-repack-pkg]: https://ubuntuupdates.org/package/postgresql/jammy-pgdg/main/base/postgresql-19-repack
