# `bringyourctl payouts sn-reconcile`: runbook

A one-time command that pays the legacy USDC obligations which neither the
weekly planner nor the sender pays on its own. Run it once against production,
then retire it.

## What it pays

It pays only `canceled_with_evidence` rows. It reports `unretained_key` rows
but never pays them (RC-24). Both kinds are stuck rows:

- `canceled_with_evidence`: canceled, not completed, with a Circle idempotency
  key, a payment record or a tx hash. These were canceled before the
  2026-08-17 fix and left for manual reconciliation. Since the fix their
  sweeps are never re-planned. Before it, the planner moved a canceled row's
  sweeps into its network's next plan, and a completed payout's sweeps were
  reaped seven days later (see `replanned`).
- `unretained_key`: not canceled, with a key but no record or hash, and no
  `audit_account_payment` row under the key. The key was reserved before
  request journaling began, so the sender refuses the row forever. Upstream's
  `reconcile-legacy-attempts` decides these rows: it binds a found transfer,
  releases the key, or holds an uncertain row. This command classifies them
  and reports its evidence; one that passes every check is `upstream_scope`.

Every other row is out of scope and is never written. That includes an
original that `bringyourctl payouts correct-legacy-attempts` replaced (RC-22):
it stays canceled with its key (a `legacy_payment_replaced` event), and its
replacement carries the corrected obligation through the sender. Unplanned
pre-cutoff earnings stay with the weekly planner.

The command never pays what it cannot prove unpaid. It never POSTs to Circle.
`--send` revives each payable row and schedules it, and the existing sender
pays it through every existing guard, from the developer wallets in vault
`circle.yml` (`circle.solana_wallet_id`, `circle.polygon_wallet_id`). The
command reads `circle.yml` and `sn.yml` as the sender does. It refuses before
reading Circle when a payout wallet id or USDC token address
(`circle.polygon_wallet_id`, `circle.polygon_usdc_address`,
`circle.solana_wallet_id`, `circle.solana_usdc_address`) is missing or empty.

## 1. Run order

The weekly planner and the sender may keep running: they never touch stuck
rows. The upstream legacy tools bind, release, complete or replace the same
legacy rows this command reads, so the run has this order:

1. `bringyourctl payouts correct-legacy-attempts`, dry run, then `--apply`.
   It goes first: the reconciliation would release an inflated attempt, which
   the sender then pays as recorded.
2. `bringyourctl payouts reconcile-legacy-attempts`, dry run, then `--apply`.
3. Wait until the sender completes every attempt those two scheduled.
4. `bringyourctl payouts sn-reconcile`, dry run (steps 2 to 4 below), then
   `--send --expect=<digest>` (step 5).
5. No legacy tool runs again until the revived rows complete.

Any other person's or tool's payout work must also have finished. Apart from
the weekly planner and the sender, no person or tool may pay, journal or
change a payout row, or send to a provider, while the command runs and until
its revived rows complete. The command's checks catch a concurrent change
only up to the moment each row is revived.

## 2. Dry run

```
bringyourctl payouts sn-reconcile          # text
bringyourctl payouts sn-reconcile --json   # the same report as one JSON document
```

The dry run writes nothing, schedules nothing and calls only Circle GET
endpoints. A listing or balance read error fails the command (exit 1) with no
report; fix the cause and run it again. An own record that Circle cannot
return only makes that row `record_unavailable`.

## 3. Review the report

- Header: run id, time, mode, policy hash, and the `digest` that `--send`
  needs (step 5).
- `listing`: the wallets listed and the transaction count. Listing starts a
  day before the earliest contract close, or creation, of any stuck row. The
  wallets are both payout wallets, every developer wallet of the Circle
  entity, the wallet of every own record Circle returned, and every
  `walletId` in a receipt paid since then. The listing must hold every payment
  record stored from an hour after its start to an hour before it was read,
  and every listed transfer a payment record names must go to that row's
  wallet. Addresses compare without surrounding whitespace, ignoring case.
  `INCOMPLETE` lists the missing and the mismatched record ids, and every row
  that needs the listing is then `listing_incomplete`. Find the cause before
  going on.
- `orphans`: hidden Circle transfers. No payment record or receipt names
  them, they are not FAILED, DENIED or CANCELLED, and they are not INBOUND.
  Each line names the networks whose wallets registered the destination. An
  orphan is a stuck row's lost record or a manual transfer. An orphan to an
  address no network registered, and that is none of our developer wallets,
  is `UNATTRIBUTED`. Investigate each one: it is part of the digest.
- `classes`: the count and the total payout of each class and reason.
- `funding`: per chain, the payable send total, plus rows earlier runs revived
  that the sender has not yet submitted (no payment record and no tx hash),
  against the payout wallet's USDC balance, and any shortfall.
- `rows`: one line per stuck row: the class or reason, what is owed, the send
  amount and chain, Circle's view of the row's own attempt (transaction id,
  state, hash), and any orphans that block it. Under a `replanned` row, its
  subsidy and its reliability each follow on their own line, with their
  guidance.
- `duplicates`: see below.

| Class or reason | Meaning | Action |
|---|---|---|
| `payable` | A `canceled_with_evidence` row that is exact, unpaid and unblocked. Owed is its payout; the send amount is payout minus the sender's $0.01 fee, on the current payout wallet's chain. | `--send` revives it |
| `paid` | Circle completed its own attempt, and its sweeps are still its own. | none |
| `paid_replanned` | Circle completed it, but its earnings were re-planned before the fix. | see duplicates |
| `replanned` | It has no sweeps and its attempt moved no funds. Its network created a row after its `cancel_time` (its `create_time` when that is empty), and that row was created on or before 2026-08-17T23:59:59Z or holds early sweeps that closed before the stuck row was created. The re-plan moved its sweeps, even if they were reaped since. Subsidy: the re-plan's epoch already weighted these bytes in its subsidy pool; paying it by hand would reward them twice. Reliability: earned per window, and the re-plan paid none of it. | never pay by hand; the owner may decide its reliability separately |
| `unresolved: review_required` | Its attribution is flagged for review. | by hand |
| `unresolved: contradictory` | Circle failed, denied or canceled it with a tx hash, or two listed transfers share its hash. | by hand |
| `unresolved: in_flight` | Circle has not finished its attempt. | run again later |
| `unresolved: record_unavailable` | Its record cannot be read, or no listed transfer has its hash. | by hand |
| `unresolved: listing_incomplete` | The listing is incomplete. | fix the listing |
| `unresolved: orphan_transfer` | A hidden transfer to its pinned wallet or to any wallet of its network, from a day before its earliest contract closed, may have paid it. | check the orphan |
| `unresolved: census_mismatch` | Its amounts do not match its sweeps and corrections exactly. | by hand |
| `unresolved: sweeps_missing` | It has no sweeps and nothing shows they were re-planned; they may have been reaped, and it may be owed. | never pay by hand; the owner may decide its reliability separately |
| `unresolved: inflated` | RC-23: it has a subsidy or reliability subsidy that the legacy correction of `correct-legacy-attempts` lowers: the July to September planner inflated both. A payout cannot change, so the row is never paid as recorded. The report shows its corrected payout. | owner decides; correct it by replacement |
| `unresolved: duplicate_risk` | The row itself holds early earnings (its own early sweeps, or corrections from another row), which an earlier payment may already have paid. `duplicates` has an entry for its network only when another, non-stuck row there holds early earnings. | by hand, from the row's own early earnings |
| `unresolved: no_payout_wallet` | Its network has no active Solana or Polygon payout wallet. | wait for a wallet |
| `unresolved: below_fee` | Its payout does not cover the $0.01 fee. | none |
| `unresolved: upstream_scope` | RC-24: an `unretained_key` row that passes every check. Upstream's `reconcile-legacy-attempts` decides it. | none here; see `reconcile-legacy-attempts` |

Never pay a `sweeps_missing` or `replanned` row by hand. Once a network's
later rows are reaped, nothing can show whether its sweeps were re-planned.
The owner may decide a row's reliability separately: it is earned per window,
and no re-plan paid it. The report lists it under a `replanned` row; for a
`sweeps_missing` row it is the row's `reliability_subsidy_nano_cents`.

The rule that a row created after the cancellation and on or before
2026-08-17T23:59:59Z makes a row `replanned` uses timing alone. Such a row
proves that a plan ran for the network, not that the stuck row's sweeps still
existed then, and 2026-08-17T23:59:59Z only approximates when the fix was
deployed. Its errors can only under-state what is owed: the row stays
`replanned` or `sweeps_missing`, which are never paid, and never becomes
`payable`.

*Early earnings* are a row's sweeps whose contract closed at or before its
plan's epoch start (the `subsidy_payment` start of its plan), or at or before
the creation of an earlier row of the network whose plan recorded no epoch;
every sweep counts when its own plan recorded none. Corrections that
originated in another row count too. They are leftovers from an earlier epoch:
re-planned from a canceled row, withheld, or swept late.

### Duplicates

For each network, `duplicates` lists every row that is not completed and not
stuck (pending or safely canceled) and holds early earnings. Each line gives
its plan, state, early revenue and bytes, early bonus, total bytes, subsidy
and reliability. The network's `canceled_with_evidence` rows are listed with
their classes and per-class payout totals: the amount Circle may already have
paid. Networks with a `paid` or `paid_replanned` row, or an orphan, come
first. The command never modifies these rows; decide by hand whether any of
them paid an obligation twice.

## 4. Fund the wallets

Top up each chain's payout wallet until its USDC balance covers the `funding`
total. The check covers USDC only.

## 5. Send

```
bringyourctl payouts sn-reconcile --send --expect=<digest>
```

`<digest>` is the digest the dry run printed. It binds the payable rows (id,
owed, send amount, chain) and the unattributed orphans you reviewed. The
command runs the same census again and refuses before any write, exiting 1
with its report, when the digest differs or a chain is short. Otherwise it
lists every wallet once more, from an hour before the census listing started.
A payable row that a new transfer may pay is skipped. Every payable row is
skipped when that listing is incomplete, or when it holds an unattributed
transfer the census did not have. Each remaining payable row is handled in
its own transaction:

1. Lock the row and compare every column with the census snapshot, and check
   that no `audit_account_payment` row has the row's key as its id.
2. Re-run the exact census.
3. Journal `sn_reconcile_revived` with the run id, the row's markers, every
   value the revive clears, the Circle evidence and the policy hash.
4. Clear the cancellation, key, record, token and receipt in one UPDATE.
   Amounts, plan, points, sweeps and `cancel_time` stay. The row never reads
   as safely canceled, so no plan can move its sweeps.
5. Schedule `advance_payment` for the row.

A skipped row (`changed`, `orphan_transfer`, `listing_incomplete` or
`unattributed_orphan`) is not written; the run continues. Within 5 to 30
minutes the sender submits each revived row with a fresh idempotency key, to
the network's current payout wallet. The revivals are in
`audit_account_payment` under event type `sn_reconcile_revived`.

## 6. Confirm

Once the sender has completed the revived rows, run the dry run again. No
revived row may appear, and no row may be `payable` except rows the `--send`
re-listing skipped. Those stay payable for a later run, once you have found
the cause and reviewed the new digest. The other rows keep their classes
unless Circle or an operator changed them.

## Note: hung-payment cancellation

The daily `CancelHungAccountPayments` cancels unsubmitted rows created more
than 30 days ago, and every revived row is that old. A revived row without
subsidy or reliability can therefore be canceled before its first send. With
the payout policy loaded, rows with subsidy or reliability are never canceled
this way. The planner then re-plans the canceled row's sweeps and the weekly
plan pays them, so the obligation is still paid once. Its payout points apply
twice, as after every hung cancellation.
