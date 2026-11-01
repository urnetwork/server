# Phase C recovery component: epoch 3's payout through a v2 root

**Status:** built on branch `phasec/recovery-component` in server and sn on 2026-10-31. Not merged or deployed. This covers the owner decision of 2026-10-31 (hybrid allocation; sweep at the epoch 4 → 5 boundary). It is the ledger half of the sweep in VERSION2-FINAL.md §§12.2 and 16. The v1 sweep root, its readers and the `STSettlement2` upgrade with `assignSurplus` are separate work.

## What it does

The v1 sweep moves vault1's carry into vault2. The owner then assigns it as surplus to a v2 epoch's funding with `assignSurplus`, and vault2 emits `SurplusAssigned(noId, lastFundedEpoch + 1, amount)`.

- **Recovery component.** Epoch 3's funding R = 4,132.715621039 α (vault1 `entitlement(3,1)`, which becomes RootMissed when its finalize runs on 11-01) is allocated by **epoch 3's own provider rows**.
  - The allocation rule is `coldkey_usage_largest_remainder_rao_v1`. The deltas sum exactly to R.
  - Epoch 3's rows use the v2 rules over epoch 3's own window: consent-only wallets and head exclusion at epoch 3's boundaries.
- **Ordinary funding.** Everything else in the surplus is ordinary funding of the receiving epoch: epoch 1's 3,482.257057872 α, the reveal-lag capture and transfer rounding. The receiving epoch's own rows allocate f_k − R.
- **Receiving epoch.** It is chosen by the chain, not by configuration. The recovery is the first R of the pool's assigned surplus, in epoch order, through the schedule's last epoch, epoch 8 (`ledger.PlanRecovery`).
  - If `assignSurplus` is mined before epoch 5's close, the receiving epoch is 5.
  - If it is mined after, the receiving epoch is 6.
  - A split or short assignment is not dropped. Each epoch places as much of R as its own surplus holds, and the next assignment completes it. The server raises `v2_recovery_incomplete` meanwhile.
  - Once R is placed, no later surplus is recovery.
  - Surplus assigned after epoch 8 is ordinary funding. If R is still incomplete then, the alert fires, and extending the bound needs a release.
  - The server reads the mirrored events. The validator reads the logs itself through a checkpointed index. A node that has not indexed the commit block makes it retry rather than decide. Both check R against vault1.
- **Schedule.** The schedule (vault2, pool 1, source epoch 3, R) is a code constant in sn `ledger`. No config or `config_hash` changes.
- **Header format.** The receiving epoch's header entry gains `"recovery":[{"epoch":3,"amount":4132715621039,"providers":[…]}]`.
  - Older decoders refuse it. Every other header is byte-identical to before.
- **Audit.** The validator approves only a root that lists exactly the component the chain calls for (check 10, CheckRecovery), with epoch 3's rows audited like a covered epoch's.
  - It refuses, without demands, a root that omits the component, inflates it, moves it to another epoch, adds epoch 1 as a source, or shifts recovery leaves.
- **Amounts.** Recovery amounts are recorded for readers as epoch 3's (`st_ledger_v2_amount.epoch = 3`). Finalizing the receiving root sends epoch 3's earnings email under its own claim.
  - The 11-01 finalize poke treats a root-missed epoch as finalized. It has already taken epoch 3's ordinary email claim and sent epoch 3's email with no payout.
  - Once epoch 3's rows are recorded (step 5), `/account/epochs` and the earnings readers show epoch 3 as a v2 epoch with zero amounts until the receiving root is finalized and recorded.
- **Watchers.** `snclaim v2 check` has neither vault1 nor the surplus history. It takes the header's recovery components as declared, says so, and still checks their rows, allocation and fit within the epoch's funding. The validator checks the amounts.

Epoch 3's rows are never built inside the driver's epoch step, so the receiving root's 2-hour step reads only its own epoch's usage. They are built once from a preserved usage artifact by an operator command and recorded as fixed inputs. If they are not recorded when the receiving root is built:

- the step retries and raises the alert `v2_recovery_rows_missing`;
- recording them before the commit window closes (end + 1,200 blocks) lets the root commit;
- otherwise the epoch is left to the next root, which covers it with the same component.

## Deploy order (gates)

1. **Merge.** Merge sn `phasec/recovery-component`, then server `phasec/recovery-component`. The server builds against the sibling `../sn`, so its build must use the merged sn.
2. **Validator and snclaim release.** Build a validator and snclaim release that includes the audit and the watcher change, and deploy the validator. This can happen any time before step 6.
   - Its config is unchanged, so no adoption or re-signing is needed.
   - It audits recovery-free roots exactly as now, so epoch 4's root is unaffected.
   - An old validator would approve a receiving root that pays all of f_k as ordinary funding, which is why this gate comes first.
   - Old validators misbehave for two roots:
     - on the receiving root, they fail to decode the header and send demands;
     - on the next root, they cannot decode the receiving root's header as the previous root (check 2) and refuse.
   - So every quorum validator must run the new release from the receiving root's commit through the next root's approval.
   - Old snclaim binaries run with `--demand` would also send paid demands on the receiving root. Announce the snclaim release with the plan.
3. **api, alt and taskworker.** Deploy these after both of epoch 4's commits are confirmed: the v2 root (block ≥ 9,450,028, about 11-06 22:25Z, or epoch 4's `v2commit` publish row confirmed) and the v1 sweep root (cutoff block 9,449,928).
   - api and alt decode headers for `/sn/ledger/account`. An old api cannot serve proofs for the receiving root.
   - The taskworker must keep the sweep chain, which runs until claim and absorb on about 11-08. Build it from the sweep hotfix branch (`hotfix/st-v1-sweep-on-1086372490`) plus these commits.
     - The cherry-pick has one conflict: the adjacent `else if` in `bringyourctl/main.go`'s st dispatch. Keep both.
     - The combined tree builds.
   - api and alt build from their running images' sources plus these commits, or from main once main's other changes are deployable.
   - The changes add no migration.
   - Folding these commits into the sweep's pre-11-06 taskworker is possible but not recommended. Epoch 4's step would then read the event mirror for the plan, and a mirror lag would delay its build by a one-minute retry.
4. **Preserve epoch 3's usage.** Read-only, after epoch 4's commit (below).
5. **Record epoch 3's rows.** This writes and is owner-run (below). Do it before epoch 5's boundary, block 9,499,228, about 11-13 18:25Z.
6. **Upgrade and assign surplus.** Upgrade `STSettlement2`, then send `assignSurplus(1, epoch, amount)`, through the Safe. Do this only after steps 2–5.
   - Review it with `sn-mainnet owner-call-settlement2 review`. While the recovery is unplaced, the review refuses an amount that leaves part of R unplaced and an epoch after the schedule's last. Assign the whole swept surplus in one call.
   - The phase C wrapper reverts `InvalidEpoch` unless `epoch` = `vault2.lastFundedEpoch(1) + 1`. A call signed for epoch 5 that arrives after epoch 5's close reverts instead of funding epoch 6. Re-sign it with epoch 6.
   - To land in epoch 5, it must be mined before epoch 5's close transaction, which the taskworker sends at block 9,499,228. Leave margin: by about 11-13 12:00Z.
   - Mined after that close, the recovery lands in epoch 6 with nothing else to change.
7. **Watch the receiving boundary.**
   - The taskworker closes, builds and commits the receiving root.
   - The validator records `approve`.
   - The root finalizes 14,400 blocks after its commit.

`assignSurplus` for pool 1 is reserved for the sweep until the receiving root finalizes. Any assignment that reaches R in one epoch places the recovery there.

## Step 4: preserve epoch 3's usage (read-only)

Run this on a host inside the data network that can run `bringyourctl st payout-dry-run`, with the production `st.yml`/`sn.yml` and blob store configuration. Start it only after epoch 4's root commit is confirmed, so it never overlaps epoch 4's read.

```
bringyourctl st v2-recovery-usage --epoch=3 --out=<new dir> --blob
```

- **Read-only.** The process sets `default_transaction_read_only` on every PostgreSQL session (`server.RequirePgReadOnly`), so any write fails.
- **The read.** The census is one REPEATABLE READ, READ ONLY transaction through the whole-window reader (6a523f17d, 7720f0deb). It covers about 404M contracts and is projected at 25–30 min with no completed production measurement.
  - It holds the xmin horizon while it runs. Watch the horizon age and the `[st]usage window … reading it whole` log line, as for epoch 4.
- **Output.** Content-addressed objects in `<new dir>`: usage chunks of 40-byte rows (client id, network id, payout bytes, in client order) and a manifest, each file named by its `sha256`.
  - `--blob` also puts them, once each, under the blob store's `st/v2/recovery/sha256/` prefix, which `/sn/ledger/object` does not serve.
  - Standard output prints counts and the manifest reference only.
- **Keep the manifest reference.** Copy `<new dir>` off the host as a second copy. Usage rows are also retained in `st_provider_usage_archive` for about 52 weeks.
- **On failure.** It changes nothing; run it again. Epoch 3 closed on 10-30, so a second run should produce the same manifest reference.

## Step 5: record epoch 3's rows (writes; owner)

```
bringyourctl st v2-recovery-record --epoch=3 --usage=sha256:<manifest> [--dir=<dir>] --by=<name>
```

- It reads the artifact from `--dir`, or from the blob store without it, and checks every object against its reference. It also checks that the manifest is this deployment's pool 1, epoch 3, over epoch 3's authenticated boundaries.
- It builds the provider rows with the builder's own code (`stLedgerV2RowsFromUsages`):
  - wallet selection by retained consents, which also fills the consent verification cache;
  - head exclusion at blocks 9,348,028 and 9,398,428.
- It then:
  1. puts the provider chunks in the ledger store;
  2. in one transaction, retains epoch 3's wallet resolutions and records `st_ledger_v2_epoch_rows` (no_id 1, epoch 3).
- A failure writes neither the resolutions nor the record. **Retry immediately**, before providers sign new consents. A resolution an earlier attempt retained is kept when it selects the same wallet through an older head; one that selects another wallet refuses the record.
- Output is counts only: rows, eligible rows, coldkeys, payout bytes and eligible bytes. If every row is ineligible, R becomes ordinary funding.
- Once recorded, a repeat with the same artifact prints "already recorded; nothing changed". It checks the artifact's census against the recorded rows and selects no wallets and reads no heads, so later consents cannot fail it. An artifact with another census is refused.
- `bringyourctl st v2-rebuild --epoch=3` is refused: epoch 3 is before the pool's first v2 epoch.
- To give up on epoch 3's rows, use `bringyourctl st v2-unrecoverable --epoch=3 --by=<name>`. R then becomes ordinary funding of the receiving epoch.

Read-only check:

```
SELECT epoch, cardinality(provider_chunks), declared_by, rebuild_requested_by, create_time FROM st_ledger_v2_epoch_rows WHERE no_id = 1 AND epoch = 3;
```

## Timeline risk

Epoch 5 needs every gate done by about 11-13 12:00Z.

| Gate | Earliest | Risk |
| --- | --- | --- |
| Review and merge of both branches | 11-01 | Review time for a ledger format change and a new audit check |
| Validator release live | 11-02 | Validator release process and owner availability |
| api, alt and taskworker deploy | 11-06 22:25Z, after epoch 4's commit | Fixed to the night of epoch 4's close, after epoch 4's own first production-scale read |
| Usage artifact | 11-06 22:25Z, plus about 30 min of read | No completed production measurement of the read |
| Record | 11-07 | Wallet selection over epoch 3's providers; owner-run |
| `STSettlement2` upgrade and `assignSurplus` | after the sweep claim and absorb, about 11-08 18:25Z+ | Ledger signing session; B's upgrade review |

If any gate slips past about 11-13 12:00Z, delay `assignSurplus` until after epoch 5's close. The recovery then lands in epoch 6 (boundary block 9,549,628, about 11-20 18:25Z) with no change to code or config. Epochs 7 and 8 are further margin. After epoch 8 the schedule needs a release.
