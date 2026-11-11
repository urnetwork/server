# The billing split

Status, 2026-11-11: phase 1 is done in this change; phase 2 is a design that
needs owner decisions before it can be built.

## What the split is

When a contract settles, `allocateContractParticipantPayouts`
(`model/subscription_model.go`) takes each funding balance's payout bytes and
the provider revenue share of that balance (`ProviderRevenueShare`, 50% of the
balance's net revenue per byte) and divides both evenly over the contract's
participants: the egress client, the stream's intermediaries and the
extenders. A participant on the payer's own network keeps its share out of the
split (`contractShareIsIntraNetwork`). The result is one row per funding
balance and provider network in `transfer_escrow_sweep`.

The owner, 2026-11-10/11: "i believe the split should no longer matter. if it
doesn't please remove it for simplicity", and "all traffic counts as paid
traffic". The SN25 ledger (`transfer_contract.provider_usage` and
`st_provider_usage_archive`, read by `GetStEpochProviderUsage`) is the source of
provider credit, and "payment balances, escrow sweeps, and current provider
membership never determine pool weight" (`model/st_provider_usage.go`). USDC
payouts have been off since 2026-10-28.

The split could not go at once, because two things users see still read it:
operator block points and the apps' "Unpaid Data". The owner approved two
steps: (1) remove the provider totals that nothing reads, now; (2) move points
(and Unpaid Data) onto the SN25 ledger, next.

## Phase 1: what the split wrote, and what was removed

Every value one settlement writes from the split, its readers, and what this
change does:

| Value | Written by | Readers | Phase 1 |
|---|---|---|---|
| `transfer_escrow_sweep.payout_byte_count`, `payout_net_revenue_nano_cents`, `sweep_time`, `network_id`, `balance_id` | `participantSweepInsertSQL` | Unpaid Data (`GetTransferStats`), operator block points (`readOperatorBlockTraffic`), provider payout stats (`queryProviderPayoutStats`), the payout planner, SN reconcile, legacy correction, reimbursement and bonus, the points leaderboard rollups, the contract horizon reaper, retention, `xops/kpis.sql` | kept |
| `transfer_escrow_sweep.provider_payouts` | same | provider payout stats (`contractProviderPayoutRowsSql`) | kept |
| `transfer_escrow_sweep.destination_id` | same | only the legacy branch of `contractProviderPayoutRowsSql`, for rows whose `provider_payouts IS NULL`; every row settlement writes has `provider_payouts`. Its index went in migration 816 | **no longer written**; new sweeps leave it NULL |
| `account_balance.provided_byte_count`, `provided_net_revenue_nano_cents` | one `ApplyLegacyProviderTotals` row per settled contract in `pending_task` (`queueLegacyProviderTotalsInTx`), applied by the drain or the per-task reader | `GetAccountBalance`, which has no production caller (only tests have ever called it: `git log -S model.GetAccountBalance`) | **no longer queued** |
| Redis `{account_balance_<id>}npbc` and `np` | the non-inline settlement path, which only `settleEscrowInTx` reaches and only tests call | `GetAccountBalance` | **writer removed** |
| the `legacy_provider_totals` run-once ownership key | settlement and deadline closes, to admit the queue row | the queue row's publication | **no longer taken** |

Evidence that nothing reads the removed values:

- `git grep` over server for `account_balance`, `provided_byte_count`,
  `provided_net_revenue`, `GetAccountBalance` and `accountBalanceNetPayout`
  finds only the writers above, `GetAccountBalance` and tests. No api route,
  handler, controller or dashboard reads them.
- The same grep over mmm, web, sdk, sn, xops, monitor, warp, connect, proxy,
  config, cli, android, apple, windows, linux and extension finds no reader:
  only an xops schema audit CSV, a privacy review doc that lists the table,
  and the xops `redis-key-cleanup.sh` comment that never touches the keys.
- `monitor/signal_key_families.go` only classifies existing
  `{account_balance_<id>}` keys by shape for the key-family probe.
- `destination_id`: the only reader is the legacy branch above. The stats moved
  to `provider_payouts` in b12af6b3, and migration 816 dropped
  `transfer_escrow_sweep_destination_id_sweep_time`. The manual
  `bringyourctl db backfill-sweep-destination-id` now backfills only sweeps
  without `provider_payouts`, so it never fills new rows.

Tests:

- `TestBillingSplitReadersAfterSettlement` (`model/billing_split_readers_test.go`)
  settles one real multi-hop contract and pins every remaining reader: the
  sweeps and their `provider_payouts`, `GetTransferStats` (Unpaid Data),
  `queryProviderPayoutStats`, `readOperatorBlockTraffic` (operator block points
  are a pure function of it, `planOperatorBlockPoints`), the payout planner's
  selection, and `GetStEpochProviderUsage`. It passes unchanged on the code
  before this change.
- `TestBillingSplitSettlementWritesNoProviderTotals` pins the removal: no
  provider totals row, no provided totals in PostgreSQL or Redis, and sweeps
  with `provider_payouts` and no `destination_id`. It fails on the code before
  this change.
- The existing settlement, expiry, deadline and owner-batch tests now assert
  that no provider totals are produced (`assertContractPayoutTestNoProviderTotals`,
  `requireLegacyProviderDurability`, `assertProviderSweepTotalsForTest`).
- Tests that used the provider key to make a settlement busy now hold the
  payer's grant on a legacy reservation, the busy path that remains. Tests that
  used the per-contract keys to see which contracts an owner turn closed use
  `Testing_WithContractDeadlineOwnerTurnObservation`.

### What remains, and why

- **The sweeps.** Every column but `destination_id` has the readers above.
  They go in phase 2, reader by reader.
- **The provider totals consumer.** The per-task reader
  (`ApplyLegacyProviderTotals`), the drain chain (`drain_legacy_provider_totals`),
  their claim shares, metrics, dashboard panels and `provider_totals.yml` stay,
  to consume the rows already queued on Main and the rows older binaries queue
  while a rollout replaces them. Remove them once every service runs a binary
  from this change and Main shows none left:

  ```sql
  SELECT count(*) FROM pending_task
  WHERE function_name LIKE '%ApplyLegacyProviderTotals';
  ```

  The drain chain reschedules itself every 5 s while idle.
- **`account_balance.paid_*`.** Payment completion writes them; only
  `GetAccountBalance` reads them. They belong to the USDC payout path, not the
  split, and go with the USDC retirement.
- **History.** The existing `account_balance.provided_*` values, the
  `{account_balance_<id>}` Redis keys and old sweeps' `destination_id` stay as
  they are. Deleting them is a separate owner decision after a verified backup.

### Write load saved

Per settled contract with a provider payout, the removed producer wrote one
`pending_task` row and the drain then locked and deleted it, crediting
`account_balance` once per provider network per pass. Native settlements also
spent two statements on the provider key's admission. At about 20M contracts
a day, that is roughly 28 GB a day of `pending_task` heap and index pages
written and vacuumed, 60M fewer statements in settlement transactions (the
insert and the two admission statements) and 40M fewer drain row operations;
dropping `destination_id` saves another 320 MB a day of sweep heap. See
"Measurements" at the end for how this was estimated.

### Migration

None. No schema changes; nothing needs `db migrate`.

## Phase 2: points and Unpaid Data on the SN25 ledger

### The ledger

At its outcome claim every contract stores an immutable usage snapshot in
`transfer_contract.provider_usage` (`newContractUsageSnapshot`): the
contract's usage, `min(capacity, source report, destination report)` for a
settled contract (`contractCompletedUsage`), split evenly over the same
participants as the billing split, but **including** participants on the
payer's own network. Retention copies the snapshot to
`st_provider_usage_archive` (contract id, outcome, close time, snapshot), and
`GetStEpochProviderUsage(start, end)` reads both by close time. Paid and free
bytes count the same, and no escrow or payment is consulted.

The ledger does not hold the payer network. `transfer_contract` does
(`payer_network_id`, or the origin derived as in
`contractProviderPayoutRowsSql`) for as long as retention keeps the contract;
the archive keeps only the snapshot. An award runs within days of its block,
well inside that.

### Reader 1: operator block points

What it reads today (`operatorBlockPointsSweepSql`), per completed operator
block past the earning boundary: the unpaid sweeps of settled contracts that
closed after the boundary, with sweep time in the block, giving

- per provider network: payout bytes, net revenue (the 50% revenue share) and
  the first sweep time;
- per payer network and provider network: bytes, split by whether the funding
  balance was paid;
- the paid balances' subscription revenue, which sizes the subsidy.

`planOperatorBlockPoints` then gives each provider a computed payout of its
net revenue plus a subsidy (each payer network weighs equally, spread over its
providers by bytes) plus a reliability subsidy, and splits
`account_points_per_payout` over providers by computed payout, with
reliability points, the seeker multiplier and referral bonuses on top.

The ledger equivalent: per provider network, the sum of its usage shares in
the snapshots of contracts that closed in the block (the same outcome set);
per payer and provider, the same sums grouped by the contract's payer network
from `transfer_contract`. The reliability inputs are unchanged.

How the numbers change. Each of these is an owner decision:

- **D1, revenue weighting.** The ledger has no revenue. Today a provider's
  points follow its revenue share, so serving paid traffic earns more points
  than serving free traffic. "All traffic counts as paid traffic" suggests
  weighting points by bytes alone. Recommended: bytes alone.
- **D2, subsidy size.** The subsidy is the largest of a minimum, a per-active-
  user amount and a fraction of subscription revenue. Without sweeps, the
  subscription revenue term needs another source (the payer networks' active
  balances in the window) or goes. A block's points are a fixed total shared in
  proportion to computed payout, so the subsidy's size only sets its weight
  against the revenue term (D1) and the reliability subsidy. Recommended: drop
  the revenue term together with D1.
- **D3, same-network hops.** The ledger's snapshot credits a participant on
  the payer's own network as provider usage (the §8.6 amendment moves such
  shares to the extender pool from epoch 8); the split gives it nothing.
  Recommended: follow the ledger's epoch rules, so points match what the pool
  pays.
- **D4, unfunded traffic.** The ledger counts contracts that settle without
  escrow (zero-escrow contracts, which since 586673c74 no longer include
  same-network and friends-and-family contracts); the split does not. And when
  funding runs short, the split pays only the bytes actually charged, while the
  ledger counts the verified usage. Recommended: count them, as the pool does.
- **D5, byte basis and time.** The ledger counts the smaller of the two
  reports, the split their mean, so a settled contract counts at most half the
  16 MiB dispute tolerance (`AcceptableTransfersByteDifference`) less; and the
  ledger dates a contract by its close time instead of its sweep time, the
  same transaction for an ordinary settlement. Small; recommended: accept.
- **D6, cutover.** Which block first uses the ledger. Recommended: the first
  block after a dry run shows the owner both awards for one completed block
  (block 20 is due after 11-15 02:00Z and should keep today's rules unless the
  owner says otherwise).

### Reader 2: Unpaid Data

`GetTransferStats` returns `unpaid_bytes_provided`: the network's sweeps not yet
in a completed USDC payment, and `paid_bytes_provided`: the bytes of its
completed payments. With USDC payouts off since 10-28, unpaid bytes only grow.

The ledger has no "paid" or "unpaid" bytes; it has epochs, finalized and
paid by the SN25 pool. Options, an owner decision (**D7**), each changing the
number users see:

- A. The network's provider usage since the start of the current, unfinalized
  epoch ("not yet paid by the pool"), with paid bytes the usage of finalized
  epochs.
- B. The network's provider usage since the USDC cutoff, with paid bytes
  frozen at the last USDC payment.
- C. Retire the two fields and show the epoch view the apps already have
  (`GET /account/epochs`).

A and B count free, unfunded and same-network bytes (D3, D4) on the ledger's
byte basis (D5), so even B's number differs from today's. A and C change what
the "Unpaid Data" label means, so the app copy changes with them. Recommended:
A, labelled as the current epoch.

### Other readers that keep the sweeps

The split can only go once these move or retire too:

- **Provider payout stats** (`/stats/providers` and the per-provider views)
  show USD per client per day from `provider_payouts`. The ledger has bytes,
  not dollars. **D8**: show bytes, an estimated pool share, or drop the payout
  column.
- **The USDC payout path**: the planner, SN reconcile, legacy correction,
  reimbursement, bonus and the points leaderboard rollups of payment blocks.
  Pre-cutoff obligations (`legacy_usdc: finish_pre_cutoff_obligations`) still
  read their sweeps. **D9**: when are legacy USDC obligations finished, so that
  this path can be retired.
- **The contract horizon reaper** keeps sweeps for pending operator block
  windows (`OperatorBlockPointsPendingWindows`); it would keep contracts for
  their snapshots instead.

Once all are moved, settlement stops writing sweeps: one row per balance and
provider network, about 20M a day.

### Status

Not implemented. It is not contained: D1 to D9 change numbers users see or
retire API fields and app copy; the points change must not land inside a
block's award without the owner's go; and a block-scale read of the snapshots
(about 400M contracts a week on Main, 2026-10-30) needs the whole-window plan
the SN epoch reader uses, measured on Main-sized data. Suggested order once the
decisions are made:

1. A read-only dry run that computes one completed block's award both ways and
   reports the per-network difference (no writes).
2. Points on the ledger from the decided block (D1-D6).
3. Unpaid Data (D7) and provider payout stats (D8), with the app copy.
4. Retire the USDC path (D9); then stop writing sweeps, and later, after a
   verified backup and the owner's go, drop `transfer_escrow_sweep`.

## Measurements

Measured locally on 2026-11-11 in one isolated test database (the local
Postgres that `test-env.sh` attests), settling 200 direct contracts of 121
bytes each through the public `CloseContract` path, one payer balance and one
provider network, with a throwaway test that is not committed:

| | Before (956825783) | After (this change) |
|---|---|---|
| `ApplyLegacyProviderTotals` rows per settled contract | 1 | 0 |
| average row, `pg_column_size` | 472 B (204 B of it `args_json`) | none |
| `pending_task` heap growth for 200 rows | 204,800 B (about 1 KiB a row) | none |
| growth of its 4 indexes | 81,920 B (about 410 B a row) | none |
| drain to empty | 2 passes, 200 credits, 1 `account_balance` upsert per network per pass | nothing to drain |
| statements for the provider key in a native settlement | 2 (admission, `pg_backend_pid`) | 0 |
| sweep row, `pg_column_size` | 256 B, of which `destination_id` 16 B | 240 B |

Daily estimate for Main: about 20M sweeps a day, and a contract with a
provider payout has at least one sweep, so up to about 20M provider totals rows
a day (about 230 a second on average; on 2026-11-01 450 to 850 a second were
queued at the peak). Per row the producer wrote one 472-byte tuple and four
index entries (about 1.4 KiB of page space, measured above), and the drain
then wrote a row lock and a delete, leaving a dead tuple and four dead index
entries for vacuum. So per day, roughly:

- 9.4 GB of `pending_task` tuples, about 28 GB of heap and index page space,
  written, deleted and vacuumed;
- 60M fewer statements inside settlement transactions: the row's insert, and
  the provider key's two admission statements in native settlements;
- 40M fewer row operations in the drain, plus its `account_balance` upserts;
- 320 MB of sweep heap for `destination_id`.

WAL volume was not measured: the local Postgres is shared with other sessions,
so its WAL position cannot be attributed to one test. It is at least the tuple
and index bytes above, plus full-page images after each checkpoint.
