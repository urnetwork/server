# Pro data per subscription cycle

Owner decision, 2026-11-11: "the subscription start is the first date of the
network's subscription cycle. Each network has its own subscription cycle. We
need to change the code to allow each network to refresh on its own
subscription cycle."

Before this change, one task (`RefreshProTransferBalances`) granted Pro's
10 TiB (pro.yml `pro.data`) to every subscriber at 00:00 UTC on the 1st, valid
for the calendar month plus a day (`model.ProGrantWindow`). Every activation
path credited a 600 GiB Pro balance (`RefreshSupporterTransferBalance`)
spanning the paid period, so a mid-month subscriber had 600 GiB until the
next 1st. Code: `model/pro_grant_cycle_model.go`; tests:
`model/pro_grant_cycle_model_test.go` (pure) and
`model/pro_grant_cycle_model_db_test.go`,
`controller/pro_grant_cycle_controller_db_test.go` (database).

## Cycles

A network's cycles form one chain, whichever markets pay for them.

- **Paid periods** are supporter `subscription_renewal` rows. The new column
  `subscription_renewal.paid_end_time` is the end of the period the row's
  payment or trial covers, without the market's grace. The period that
  counts is `[start_time, LEAST(paid_end_time, end_time))`, so the end paths
  (refund, revoke, reconcile cut) shorten it without knowing about cycles.
  Each credit path sets the column:

  | market | period |
  | --- | --- |
  | Stripe | invoice billing period start/end |
  | App Store | transaction purchase date to expiry |
  | Google Play | the purchase's previous paid end (or its start) to the line items' expiry |
  | Gamma | the monthly or yearly term start/end |
  | Solana, Monero, Zcash, TAO, Bitcoin, ETH | the paid term |
  | manual (bringyourctl) | the year |

  x402 writes the column too, but an x402 month is not a cycle's period. Each
  x402 purchase's own Pro balance carries the full amount for its month, as
  before, because an agent buys data where it pays, and a second purchase in
  the same month must carry data at once.

  A store's billing-grace hold (App Store, Play) leaves the column NULL: it
  keeps Pro but is not a paid period, so it never starts a cycle. A row
  written before the column, or by an old binary during the rollout, is read
  as `end_time` less its market's grace. The App Store, Play and manual have
  one day. Stripe, the crypto rails, x402 and gamma have 30 days, or one day
  on rows from before 2026-09-26; the row's length tells which. An App Store
  or Play row of that age counts only when its activation balance (positive
  bytes, same window) shows that it credited. A Play row starts at the
  subscription's first start, so each Play period starts where the
  purchase's previous paid period ended.
- **Cycles.** A cycle starts at a paid period's start and runs to the
  period's next monthly boundary (start plus whole months, the day clamped
  to the month's last day, as Stripe bills), or to the period's end. A
  monthly plan's cycle is therefore its billing or renewal period. A yearly
  plan, or a year of one-time crypto Pro, has twelve monthly cycles from its
  start. A boundary closer than 7 days (`ProCycleMinDuration`) to a cycle's
  start or to its period's end is skipped, so no period ends in a sliver of
  a cycle. A period shorter than 7 days (the stores' accelerated test
  renewals) gets one 7-day cycle.
- **The chain.** The next cycle starts where the latest one ended, if a paid
  period covers that moment. A new chain (first subscription, or after a
  lapse) starts at the start of the earliest paid period covering now. When
  overlapping periods cover a cycle start, the one whose boundaries the chain
  is already on keeps the chain, so a second market never splits a cycle.
  Cycles that ended without a grant (the sweep was down) are stepped over,
  never granted late: the cycle granted always contains now.
- **Trials.** The monthly task granted every network with an active
  supporter renewal, trials included (a trial on the 1st got the full
  10 TiB). So a trial is a paid period like any other (owner 2026-11-08:
  trials count as subscriptions): the full grant at the trial start, valid
  to the trial's end, then a new cycle at the first charge.

## The grant

At the start of each cycle the network gets the full `pro.data`, valid
`[cycle start, cycle end + ProGrantGrace)`. It has `pro = true` and grant kind
`pro`, so the balance summary's supersede rule and old binaries read it like
the monthly grant. Its subsidy revenue is each paid period's revenue
pro-rated to its overlap with the cycle, so a period's revenue is counted
once, across its cycles.

**Idempotency.** `pro_grant_cycle (network_id, cycle_start_time)` is the
primary key. Each row records a cycle, its end and its grant's balance. The
grant writes its row with `ON CONFLICT DO NOTHING` under a per-network
advisory lock, in a ReadCommitted transaction (the function refuses any
other isolation and leaves the cycle to the sweep), so the renewal paths and
the sweep never grant one cycle twice. Two markets, an upgrade, a store
switch or a grace hold all join the one chain: one grant per network per
cycle.

## Detection: at renewal and by a sweep

- **At renewal.** Every credit path calls `model.GrantProCycleInTx` in the
  transaction that writes the renewal. A new or renewing subscriber gets the
  cycle's grant in the same commit as the payment.
- **Sweep.** The taskworker task `RefreshProCycleTransferBalances` runs
  hourly. It finds networks covered by a supporter renewal whose chain has no
  running cycle, and grants each the cycle that is due. This covers the
  monthly cycles inside a yearly period, a renewal recorded by a path that
  did not grant (an old binary in the rollout, a missed or late webhook
  caught by a reconciler or poll), and a grant skipped because pro.yml was
  missing.
- **Clawback.** A refund, revocation, dispute or store-reported end goes
  through `endReconciledEntitlementInTx`. It ended the period's renewal and
  the balance matched by its window end, which used to be the 600 GiB credit
  and so ended Pro at once. Under the same single ownership admission, it now
  also ends the cycle grant of a network the ending leaves with no supporter
  renewal open, even one in its last day of grace, and marks the cycle
  `ended_time`. A network still
  paying through another subscription keeps its cycle's grant. A cycle
  grant is never matched by window end. A won dispute
  (`RestoreDisputedStripeEntitlementInTx`) gives the cycle back: it clears
  `ended_time` and restores the grant to the cycle's end plus a day. The
  hourly sweep is the safety net for a renewal ended any other way. The
  chain's head is a clawed-back cycle's `ended_time`, so a resubscription
  starts a fresh chain.
- The old `RefreshProTransferBalances` stays registered as a no-op. Its
  pending row, due 2026-12-01, is consumed without granting and not
  rescheduled. Old taskworkers never claim the new task, since claims are by
  registered function name.

## Cancellation, grace, refunds

- A cancellation adds no new paid period, so grants stop after the paid
  term. A yearly plan cancelled at period end keeps its monthly cycles to the
  end of the paid year.
- A billing-grace hold, or the 30-day manual-payment grace of a period paid
  through Stripe, the crypto rails or gamma, keeps Pro through the period's
  marker but starts no cycle. A Stripe period no payment paid for (the yearly
  plan's trial) runs a day past its end instead, and ends when Stripe cancels
  it for non-payment (owner, 2026-11-13; controller/subscription_stripe_grace.go).
  The last cycle's grant expires a day after the cycle. The
  renewal that recovers the charge starts the next cycle. (Before, a network
  in grace on the 1st got a new 10 TiB.) From that expiry until the charge
  recovers, the network has the daily free grant, as every network does
  (owner, 2026-11-11: "extend daily data in grace"; see The daily free
  grant). Before, the 600 GiB credit spanned the grace.

## The daily free grant

Owner, 2026-11-11: "3. yes extend daily data in grace" and "5. i think pro
should get the 1tib per month plus the 30gb per day". The monthly amount is
pro.yml's 10 TiB, confirmed the same day. So the daily free grant (pro.yml
`free.data`, 30 GiB, `RefreshFreeTransferBalances` at 00:00 UTC) goes to every
network on top of any cycle grant: Pro networks, both kinds of payment grace,
and x402 networks beside their month's data. Only a lag.dev guest and a prober
shard's private network are left out, as before. A Pro Seeker holder gets the
Seeker multiplier on it, as a free one does.

It is the grant a free network gets: no revenue, `pro = false`, kind `free`,
valid `[day start, next day + 1 h)`. It never confers Pro, so a network in grace
keeps Pro only through its period's marker, as before.

**Funding order** (`model/transfer_balance_funding_order.go`). Contracts were
funded from a payer's grants in earliest-expiry order. A daily grant ends
within a day, before any cycle grant, so a Pro network would have spent its free
data first every day. Now a payer's paid Pro grants fund first: recurring Pro
grants (grant kind `pro`) that carry subscription revenue (`paid`), meaning a
paid period's cycle grant and, until it runs out, the 11-01 calendar grant.
Everything else follows. Each group goes earliest expiry first. Settlement and
the deadline close charge a contract's escrows in the same order.

- A network without a paid Pro grant keeps exactly the old order. That covers
  every free network: a free network with a data pack still spends its daily
  grant first. It also covers every network that already got the daily grant
  while it was Pro: a Pro balance with no grant kind is not a paid Pro grant.
  So a network whose Pro comes from an AppSumo or other data code redeemed
  while codes still conferred Pro keeps spending its daily grant before the
  code, as before.
- A subscriber's 600 GiB activation credit from before per-cycle grants has no
  grant kind either. It funds after the daily grant, which matters only once
  the 10 TiB cycle grant is used up.
- A Pro network's traffic draws its paid cycle grant while that has bytes, at
  paid priority. The daily grant funds it only once the cycle grant is used up,
  or after it has expired in a grace. Daily grants still expire each day, unused
  while the cycle grant lasts. So in practice "plus 30 GiB a day" is a floor of
  30 GiB a day once the month's grant is used or has lapsed, not 900 GiB more a
  month.
- A Pro network's unpaid grants that used to fund before a later-ending paid
  Pro grant now fund after it: referral grants, and a short-lived data pack or
  code. An x402 Pro month records no revenue, so it is not a paid Pro grant.
- The Redis admission census reads paid Pro grants in their own keyset phase,
  but only for a payer that its Pro cache reads as Pro. Every other payer runs
  the same statements as before. Reads inside a contract transaction (the
  zero-byte anchor, the zero-escrow priority) use one bounded statement, gated
  by one probe of `transfer_balance_pro`.

**Payouts.**

- A per-contract payout is the provider share of the funding balance's net
  revenue. Neither the cycle grant nor the daily grant carries any (the cycle
  grant's revenue is subsidy revenue), so per-contract payouts are unchanged.
- The subsidy pool's revenue input counts a balance's subsidy revenue,
  pro-rated to the plan's window, only when that balance funded swept traffic
  in the plan. Under the funding order, a Pro network with traffic keeps funding
  it from its cycle grant, so the input is what it would be without the daily
  grant. Without the order, a Pro network that used less than 30 GiB a day
  would never touch its cycle grant, and its subscription revenue would drop
  out of the input.
- Traffic the daily grant funds for a Pro network (after the cycle grant is
  used up, or in a grace) is unpaid. It pays providers nothing and adds nothing
  to the revenue input. Before, that traffic was refused for lack of balance.
  Like a free user's traffic, it counts in the pool's split, which is per
  traffic user, and toward the active-user term. Points weigh it like any
  traffic.

## The 600 GiB activation credit

It is gone. Every credit path still writes the period's Pro balance with the
same window, revenue and purchase token, so the entitlement, the end paths'
window matching, Play's overlap gate and the sales history are unchanged.
But it now holds zero bytes: it is the period's Pro marker, like a grace
hold's marker, but with revenue. The data is the cycle grant. (x402 is not a
credit path of this kind: its month's balance keeps its full amount.)

## Cutover for existing Pro networks

Nothing already granted changes: no balance is shortened, moved or deleted.

- A network holds a **legacy grant** when it has a Pro balance of kind `pro`
  that is not linked to a cycle: the calendar grant of the monthly task or of
  the old refresh. Its chain starts no earlier than the end of that grant's
  month (00:00 UTC on the 1st). Pro balances without a kind are left alone:
  x402 months, which keep their own data, and the long-lived manual Pro
  balances SIGNALS.md describes.
- So a network granted on 11-01 gets its first per-cycle grant at 12-01
  00:00 UTC, as the 11-01 grant's month ends. That grant is valid to the end
  of the network's own current cycle (for example 12-15 plus a day for a
  network that renews on the 15th). After that, grants come at the network's
  own cycle starts. There is never a gap, and never two grants in one of the
  network's cycles: the 11-01 grant fell in the network's previous cycle.
- A cycle that continues the chain but ends within the one-day grace of the
  grant before it is recorded without a grant (`balance_id` NULL), and the
  next cycle gets the grant. Two cases produce one: a network whose own cycle
  starts on the 1st a few hours after 00:00, at the cutover; or a second
  market's period that ends just after the first's.
- A network that subscribed after the last 1st has no legacy grant. The first
  sweep grants its current cycle from its own period start, beside its
  600 GiB activation balance, which it keeps.
- x402 is unchanged: each purchase's own balance carries its month's data.

A network whose own cycle starts within a week or so after the 1st gets a
short first cycle (from 12-01 to its renewal) with a full grant, then the next
full grant at that renewal. This is a one-time cost of having no gap and no
second grant in one cycle.

## Deploy

- Migration (`db_pro_grant_cycle.go`): adds the column and the table, both
  empty or NULL, under a 5 s lock bound. Apply it before deploying binaries
  that write `paid_end_time`.
- **api and alt** (Stripe, App Store and Play webhooks, client purchase
  reports, refund and dispute handling) and the **taskworker** (Play polls,
  reconcilers, crypto and gamma credits, the new sweep).
- Order: migrate, then the taskworker, then api and alt. Rows that an old api
  writes in between have no `paid_end_time`; the sweep reads them as legacy
  rows and grants within the hour. No config release.
- Finish the taskworker rollout before 2026-12-01 00:00 UTC, with no old
  taskworker left running then. An old binary that claims the monthly task's
  12-01 row grants every Pro network 10 TiB, deduplicated per run, not per
  network, on top of the cycle grants the new sweep and api have written. If
  no new binary is deployed by then, nothing doubles: the 12-01 grant is a
  legacy grant and the per-cycle grants start on 01-01 instead.
- The daily free grant for Pro networks (The daily free grant) needs no
  migration and no config release.
  - The funding order lives in connect, api and alt (contract admission and
    client closes) and in the taskworker (deadline closes, and the contracts
    of the probes it runs).
  - The grant itself is written by the taskworker's 00:00 UTC run.
  - Roll connect, api and alt before the taskworker, or finish all four before
    the next 00:00 UTC. From a new taskworker's first run on, every Pro network
    holds a daily grant, and an old connect, api or alt binary funds Pro
    traffic from it first, unpaid, until that binary is replaced.
  - A Pro network gets its first daily grant at that first run.

## Accounting notes

- The activation marker has no bytes, so no traffic draws from it. Payout
  planning counts a balance's subsidy revenue only when traffic it funded is
  swept. Before, a period's revenue counted through both the 600 GiB balance
  (full revenue) and the monthly grant (revenue pro-rated by the grant window
  over the renewal row's window, which for a Play row runs from the
  subscription's first start). Now it counts once, through the cycle grants,
  each carrying its exact share. The provider subsidy pool's revenue input
  changes accordingly.
- A cycle grant's revenue is read when the grant is written. A second market's
  period credited inside a running cycle therefore adds its revenue to the
  cycles after it, not to the running one. That part of an overlapping
  subscription's revenue is not counted.
- A cycle grant of a paid period carries revenue, so it is `paid` (paid
  contract priority), like the monthly grant. A period recorded without
  revenue (an App Store sandbox purchase, a Stripe trial's $0 invoice, a
  manual upgrade) gives an unpaid grant, as before.
- Because it is paid, a cycle grant funds before the daily free grant (The
  daily free grant), so it keeps funding the network's traffic, and so
  carrying its revenue into the subsidy pool's input, while it has bytes.
