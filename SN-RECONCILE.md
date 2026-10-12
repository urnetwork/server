# `bringyourctl payouts sn-reconcile`: runbook

A one-time command that pays the legacy USDC obligations which neither the
weekly planner nor the sender pays on its own. Run it once against production,
then retire it.

## What it pays

Only stuck rows:

- `canceled_with_evidence`: canceled, not completed, with a Circle idempotency
  key, a payment record or a tx hash. These were canceled before the
  2026-08-17 fix and left for manual reconciliation. Their sweeps are never
  re-planned.
- `unretained_key`: not canceled, with a key but no record or hash, and no
  `audit_account_payment` row under the key. The key was reserved before
  request journaling began, so the sender refuses the row forever.

Every other row is out of scope and is never written. Unplanned pre-cutoff
earnings stay with the weekly planner.

The command never pays what it cannot prove unpaid. It never POSTs to Circle.
`--send` revives each payable row and schedules it, and the existing sender
pays it through every existing guard, from the developer wallets in vault
`circle.yml` (`circle.solana_wallet_id`, `circle.polygon_wallet_id`).

## Before running

- Tell whoever pays the pending attempts that the run is starting. Nobody may
  make a manual `wallet transfer` to an affected provider until it is over.
- The command reads `circle.yml` and `sn.yml` as the sender does. It refuses
  when either payout wallet id or USDC token address is missing.

## 1. Dry run

```
bringyourctl payouts sn-reconcile          # text
bringyourctl payouts sn-reconcile --json   # the same report as one JSON document
```

The dry run writes nothing, schedules nothing and calls only Circle GET
endpoints. A listing or balance read error fails the command (exit 1) with no
report; fix the cause and run it again. An own record that Circle cannot
return only makes that row `record_unavailable`.

## 2. Read the report

- Header: run id, time, mode, policy hash.
- `listing`: the wallets listed, from 24 hours before the oldest stuck row,
  with the transaction count. The wallets are both payout wallets, the wallet
  of every own record Circle returned, and every `walletId` in a receipt paid
  since then. The listing must hold every payment record stored from an hour
  after its start to an hour before it was read. `INCOMPLETE` lists the
  missing record ids, and every row that needs the listing is then
  `listing_incomplete`. Find the wallet that holds those records before going
  on.
- `orphans`: hidden Circle transfers. No payment record or receipt names
  them, they are not FAILED, DENIED or CANCELLED, and they are not INBOUND.
  Each line names the networks whose wallets registered the destination. An
  orphan is either a stuck row's lost record or a manual transfer.
- `classes`: the count and the total payout of each class and reason.
- `funding`: per chain, the payable send total against the payout wallet's
  USDC balance, and any shortfall.
- `rows`: one line per stuck row: the class or reason, what is owed, the send
  amount and chain, Circle's view of the row's own attempt (transaction id,
  state, hash), and any orphans that block it.

| Class or reason | Meaning | Action |
|---|---|---|
| `payable` | Exact, unpaid and unblocked. Owed is its payout; the send amount is payout minus the sender's $0.01 fee, on the current payout wallet's chain. | `--send` revives it |
| `paid` | Circle completed its own attempt, and its sweeps are still its own. | none |
| `paid_replanned` | Circle completed it, but its earnings were re-planned before the fix. | see duplicates |
| `replanned` | Its attempt moved no funds, and the planner re-planned its earnings. | none |
| `unresolved: review_required` | Its attribution is flagged for review. | by hand |
| `unresolved: contradictory` | Circle failed, denied or canceled it with a tx hash, or two listed transfers share its hash. | by hand |
| `unresolved: in_flight` | Circle has not finished its attempt. | run again later |
| `unresolved: record_unavailable` | Its record cannot be read, or no listed transfer has its hash. | by hand |
| `unresolved: listing_incomplete` | The listing is incomplete. | fix the listing |
| `unresolved: orphan_transfer` | A hidden transfer to one of its network's addresses may have paid it. | check the orphan |
| `unresolved: census_mismatch` | Its amounts do not match its sweeps and corrections exactly. | by hand |
| `unresolved: duplicate_risk` | It may hold earnings re-planned from a row that may have been paid. | see duplicates |
| `unresolved: no_payout_wallet` | Its network has no active Solana or Polygon payout wallet. | wait for a wallet |
| `unresolved: below_fee` | Its payout does not cover the $0.01 fee. | none |

## 3. Hand over the duplicates

A *source* is a `canceled_with_evidence` row whose census is not exact and
that is not proven unpaid (`paid_replanned`, or any unresolved reason). Its
earnings may already sit in a later row. For each network with sources, the
report shows:

- `paid twice risk`: the sources' payout minus subsidy and reliability. That
  is the revenue and bonus that moved when the sweeps were re-planned.
- `queue`: the network's pending rows created after the sources' earliest
  cancellation, with their state (planned, reserved, submitted, on_chain) and
  candidate amount. The candidate is the revenue of their sweeps that closed
  before the sources' latest creation, plus their corrections that came from
  a source.
- `at risk`: the smaller of the risk and the candidates.

Give this section (the JSON is easiest) to whoever runs the pending payouts,
to hold or trim those rows before they are paid. The command never modifies a
queue row. A stuck row that could hold a source's earnings is
`duplicate_risk` and is not paid either.

## 4. Fund the wallets

Top up each chain's payout wallet until its USDC balance covers the payable
send total in `funding`. The pending payouts draw on the same wallets. The
check covers USDC only.

## 5. Send

```
bringyourctl payouts sn-reconcile --send
```

The command runs the same census first. When any chain is short it prints the
report, refuses before any write and exits 1. Otherwise it handles each
payable row in its own transaction:

1. Lock the row and compare every column with the census snapshot.
2. Re-run the exact census.
3. Journal `sn_reconcile_revived` with the run id, the row's markers, every
   value the revive clears, the Circle evidence and the policy hash.
4. Clear the cancellation, key, record, token and receipt in one UPDATE.
   Amounts, plan, points, sweeps and `cancel_time` stay. The row never reads
   as safely canceled, so no plan can move its sweeps.
5. Schedule `advance_payment` for the row.

A row that changed since the census is reported as `skipped: changed` and is
not written; the run continues. Within 5 to 30 minutes the sender submits
each revived row with a fresh idempotency key, to the network's current payout
wallet.

The revivals are in `audit_account_payment` under event type
`sn_reconcile_revived`.

## 6. Confirm

Once the sender has submitted the revived rows, run the dry run again. No row
may be `payable`, and no revived row may appear. The other rows keep their
classes unless Circle or an operator changed them.

## Note: hung-payment cancellation

The daily `CancelHungAccountPayments` cancels unsubmitted rows created more
than 30 days ago. Every revived row is that old. A revived row without subsidy
or reliability can therefore be canceled again before the sender's first
attempt. With the payout policy loaded, rows with subsidy or reliability are
never canceled this way. The planner then re-plans the canceled row's sweeps,
and the weekly plan pays them, so the obligation is still paid once.
