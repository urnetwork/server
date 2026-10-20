# Payment & Upgrade Audit — Findings and Plan

2026-08-07. Six parallel read-only audits: android, apple, windows, linux,
mmm/ur.io + extension, and the server crediting paths + SDK payment surface.
Scope: every flow where a user pays or upgrades, hunting (a) silent failure,
(b) fail-to-credit (user loses money), (c) key logic duplicated across apps
that belongs in the SDK.

Status legend: `[ ]` open · `[x]` fixed · `[-]` accepted / by design.
A partially fixed finding stays `[ ]`, and its line starts with "Partial:".

### Status audit 2026-10-03

Every finding was re-checked against current `main` in server (94329db1),
sdk (8f4d101d), apple (b2fc0802), android (d1979043), mmm (4bf4d3c15),
windows (b15f0b6) and linux (d23b654). Unmerged `fix/*` branches count only
as notes: a finding fixed only on a branch is "open (fix on branch X)".
Most of the fix work landed in a few commits: server `3e98ccbb` and sdk
`e7680ba6`; apple `933a9fdf`; android `bf155381` and `3083e940`; mmm
`c258d3559`; windows `02bccd6`; linux `26b8e67`. All of these are from
2026-08-09, except android `3083e940` (2026-09-09).

The audit covered 46 tracked items: S1–S11, A1–A7, N1–N7, W1–W6, D1–D8, and
§4 SDK items 1–7.

| Status | Count | Items |
|---|---|---|
| Fixed `[x]` | 27 | S1 S2 S3 S4 S5 S6 S7 S8 S9 S10 S11 · A3 A5 A7 · N3 N4 · W2 W4 W6 · D3 D5 D6 D7 · §4.2 §4.3 §4.4 §4.5 |
| Partially fixed `[ ]` | 16 | A1 A2 A4 · N1 N2 N5 N7 · W1 W3 W5 · D1 D2 D4 D8 · §4.1 §4.7 |
| Open `[ ]` | 2 | N6 · §4.6 |
| Accepted `[-]` | 1 | A6 |

- The §4.2 verify endpoints are **committed** (server `3e98ccbb`, routes at
  `api/api.go:220-221`). Android (`PurchaseReporter.kt`) and Apple
  (`PurchaseReporter.swift`) both call them before acknowledge/`finish()`.
- No client uses the SDK subscription-balance controller, catalog, checkout
  envelope or balance-code classifier yet. §6 steps 2–5 are SDK-complete
  and client-incomplete.
- SDK main does not build against connect main, because
  `connect.WithHttpRedirectsDisabled` and `connect.HttpRequestExhaustedError`
  exist only under `connect/ssoenv/connect/`. The SDK payment tests could
  not be run for this audit.
- New: **S12** (Play subscription keeps billing a deleted account) and
  **S13** (memo-less USDC payment is never matched). Both fixes are merged
  to server main; see §2.
- Merged to server main 2026-10-03: the fix branches for S3, S5 (repeat
  acknowledge), S9, S11 (comment), S12, S13 and §4.3 (server half; the sdk
  half is on sdk main `bd173d29`). The table above counts them.
- 2026-10-04: S11 fixed by removing the email fallback (`5f9f5975`).

---

## 1. The central finding

**Crediting is webhook-only on every platform, and no client can recover a
lost webhook.**

No app ever tells the server "I paid":

- android never sends the Play purchase token — the call that did is
  commented out (`android app/src/google/.../MainActivity.kt:436-439`,
  `subscriptionCreatePaymentId`). The only linkage is
  `setObfuscatedAccountId(networkId)` at flow launch.
- apple never sends the transaction JWS; the link is `appAccountToken` =
  networkId. The app calls `Transaction.finish()` immediately
  (`AppStoreSubscriptionManager.swift:142` purchase path, `:218` updates
  path), so StoreKit will never redeliver.
- web, windows, linux hand control to Stripe and poll.

So every "store took the money, webhook lost" scenario has **no recovery
path anywhere**, and every client independently hand-rolls the same
"poll subscriptionBalance and hope" confirmation machine — five divergent
copies of isPro derivation, jwt reconciliation, balance arithmetic, and
polling policy. The duplication and the silent failures are the same
problem: the shared logic that should be one tested SDK implementation is
instead five approximations of the macOS view model.

- [x] Status 2026-10-03: crediting is no longer webhook-only.
      - Clients report proof: android `verifyPlayPurchase` before
        acknowledge, apple `verifyAppleTransaction` before `finish()` (§4.2).
      - The hourly reconciler (§8) repairs lost webhooks for stripe, play
        (known tokens), apple (ledger) and solana.
      - Remaining recovery gaps: pre-fix apple transactions already
        finished (A1) and android purchases already acknowledged (N1).
      - The duplication half is still open: the SDK controller exists, but
        no client uses it (§4.1).

---

## 2. Server findings (money actually disappears here)

### S1 — Helius webhook drops later transactions in a batch — HIGH
`controller/subscription_controller.go:1451-1533` (`HeliusWebhook`): the
handler iterates `transactions []` but `return`s 200 on the first
non-TRANSFER tx / tx without token transfers / no matching USDC payment /
no intent / underpayment, instead of `continue`. A valid customer payment
behind any unrelated transfer in the same delivery is never examined;
Helius gets 200 and never retries. Highest-probability genuine user money
loss found in the audit.

- [x] Fixed: every skip is a `continue`; per-tx DB failures are collected and
      returned non-2xx after the whole batch is examined (consumed intents keep
      the retry idempotent). `TestSolanaWebhookBatchSurvivesLeadingNoise`
      (unrelated TRANSFER + swap ahead of the real payment) passes unskipped.
      Re-checked 2026-10-03: `subscription_controller.go:1887-2075` (`firstErr`
      ~`:2063`), `solana_payment_test.go:442`, `3e98ccbb`.

### S2 — Stripe `invoice.paid` has no idempotency on the credit — HIGH
`controller/subscription_stripe_controller.go:562-588`
(`stripeHandleInvoicePaid`): the renewal upsert absorbs a duplicate
silently, then `AddTransferBalanceInTx` unconditionally inserts another
600 GiB `pro=true` balance and double-counts `SubsidyNetRevenue` (drives
provider subsidy payouts). Stripe is at-least-once; the 200 is sent only
after commit, so a crash between commit and response *guarantees* a
retry. It is the only store path with no gate: Apple has the
`apple_subscription_transaction` ledger, Play has
`GetOverlappingTransferBalance`, Solana has the intent.
`TestWebhookRetryDoesNotDoubleCredit` covers only the data-pack path.

- [x] Fixed: `stripe_invoice` ledger (migration) gates the credit inside the
      one tx (`stripeCreditInvoicePaid`), ON CONFLICT DO NOTHING +
      rows-affected; `TestStripeInvoicePaidRetryDoesNotDoubleCredit` pins one
      credit and one subsidy count across a double delivery.
      Re-checked 2026-10-03: `subscription_stripe_controller.go:855-882`,
      migration `db_migrations.go:4772`, `subscription_stripe_invoice_test.go:20`.

### S3 — Solana late/under payment: money kept, 200 acked — HIGH
Intents expire 1 h after creation (`model/solana_payment_intent_model.go:45`)
and are deleted ~1–2 h later (`taskworker/work/subscription_work.go:320`).
Paying after expiry hits `SearchPaymentIntents` → nil → "No payment intent
found" **with HTTP 200** (`subscription_controller.go:1510-1515`). Money
moved on-chain; no retry, no operator-visible record beyond an Info log.
Underpayment (`:1522-1533`) likewise keeps funds and acks 200.

- [x] Fixed: `solana_unfulfilled_payment` table (migration) records unmatched
      and underpaid payments (signature, amounts, quote, reference candidates,
      timestamps) while still acking 200; a redelivery of an already-credited
      tx is recognized and not recorded; late payments whose intent expired but
      is not yet swept still credit (`TestSolanaWebhookLatePaymentStillCredits`,
      `TestSolanaWebhookRecordsUnmatchedAndUnderpaid`).
- [x] Fixed 2026-10-03 (`fix/upgrade-s3` `082a1333`, merged in `9615cbd1`):
      a failed `RecordUnfulfilledSolanaPayment` write now joins the batch's
      `firstErr`, so the delivery is answered non-2xx and Helius redelivers
      it (the insert is ON CONFLICT DO NOTHING). Since the S13 merge the write
      is in `HeliusWebhook`'s record case, for both the no-intent and the
      underpaid path. `TestSolanaWebhookUnfulfilledRecordFailureFailsTheBatch`.

### S4 — Concurrent-delivery double credits (Solana, balance codes, Play) — MEDIUM
All three are read-check-then-insert with no lock:
- Solana: `MarkPaymentIntentCompletedInTx`
  (`model/solana_payment_intent_model.go:121`) has no
  `AND tx_signature IS NULL` predicate; two concurrent deliveries of the
  same tx both credit (both set the *same* signature, so the unique index
  never fires).
- Balance codes: `RedeemBalanceCodeInTx`
  (`model/transfer_balance_code_model.go:53-116`) — no `FOR UPDATE`, and
  the UPDATE's WHERE is only `balance_code_id = $1`; under READ COMMITTED
  a double-click or webhook-vs-manual race redeems twice.
- Play: the `GetOverlappingTransferBalance` gate
  (`controller/subscription_controller.go:839`) races between the inline
  webhook call and the scheduled renewal task.

- [x] Fixed: guarded UPDATEs checking rows-affected (`tx_signature IS NULL` in
      `MarkPaymentIntentCompletedInTx`, consumed FIRST in the credit tx;
      `redeem_balance_id IS NULL` in `RedeemBalanceCodeInTx`); the Play credit
      now runs in one tx under a purchase-token advisory xact lock with the
      overlap re-checked inside. `TestSolanaMarkCompletedConcurrent` and
      `TestRedeemBalanceCodeConcurrent` (two goroutines, one credit) pass.
      Re-checked 2026-10-03: `model/solana_payment_intent_model.go:136-147`,
      `model/transfer_balance_code_model.go:127-137`, Play lock
      `subscription_controller.go:1205`/overlap `:1223` (`8e5ba5fe`, 2026-09-12).

### S5 — Play inline credit error discarded; ack failure invites auto-refund — MEDIUM
`PlayWebhook` calls `PlaySubscriptionRenewal(...)` at
`subscription_controller.go:701` and ignores both return values; Pub/Sub
still gets 200. A persistent error (sku missing from play.yml, Google API
failure) means entitlement arrives only via the task scheduled at the
*end* of the paid period, or never. The `:acknowledge` POST (`:691`) also
discards its result — an unacknowledged purchase is auto-refunded by
Google after 3 days while any granted balance stands. Play webhook has
**zero live tests** (`subscription_controller_test.go:103` is commented
out).

- [x] Fixed: `:acknowledge` result checked and the inline credit error
      propagated -- both are non-2xx so Pub/Sub retries. First live Play
      webhook tests (`play_webhook_test.go`, fake Android Publisher API via
      hermetic seams): credit+acknowledge+redelivery-idempotence, sku-missing
      -> non-2xx, acknowledge-failure -> non-2xx then recovery.
      Re-checked 2026-10-03: `subscription_controller.go:927-973`, tests
      `play_webhook_test.go:162/227/267`, commit `3e98ccbb`.
- [x] Fixed 2026-10-03 (`fix/upgrade-s5` `d542237b`, merged in `81fdcd7f`):
      the acknowledge is skipped when Play already reports the subscription
      `ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED`, so renewals no longer depend on
      Play accepting a repeat acknowledge. A first purchase is still
      acknowledged and a failed acknowledge still fails the delivery.
      `TestPlayAcknowledgeSkipsAlreadyAcknowledgedPurchase`.

### S6 — SDK `UpgradeGuest` / `UpgradeGuestExisting` → 404 — MEDIUM
`sdk/api.go:1747` posts `/auth/upgrade-guest`, `:1793`
`/auth/upgrade-guest-existing`; both routes were removed in server commit
`340d828a` (2026-07-15) with no remaining handler. Every shipped app using
the SDK guest-upgrade flow gets a 404. A guest who bought Pro/data has no
supported conversion path that keeps what they paid for.

- [x] Resolved 2026-10-03 by deprecation; the routes stay removed.
      - Guest mode was removed from the server in `9b4cf420` and `340d828a`
        (both 2026-07-15, server repo). `NetworkCreateArgs` has no guest
        field, and both create paths hard-code `guestMode := false`
        (`model/network_model.go:465`, `:610`). No new guest networks can
        exist.
      - SDK `UpgradeGuest`/`UpgradeGuestExisting` are deprecated and fail
        immediately, with no HTTP call, with "guest upgrade is no longer
        supported" (`sdk/api.go:2227-2292`, `errGuestUpgradeRouteRemoved`;
        `TestUpgradeGuestFailsImmediatelyWithoutHttp`; sdk `e7680ba6`).
        `TestSdkPaymentEndpointsMatchServerRoutes` asserts the routes stay
        absent.
      - Apple and android never call them.
      - Windows (`LoginPage.cpp:366`) and linux (`AuthViews.cpp:439-442`)
        still call `UpgradeGuest`. That code is reachable only for a legacy
        guest JWT from before July. The leftover UI is tracked under D8/A4,
        including the open product question of what a legacy guest who paid
        should see. Server leftovers (`jwt/by_jwt.go:223` GuestMode claim,
        `oauth/grant.go:108`) are inert.

### S7 — No refund/revocation clawback anywhere — MEDIUM (company loss)
Apple `REFUND`/`REVOKE`, Play `SUBSCRIPTION_REVOKED`, Stripe
`charge.refunded`/disputes, Coinbase resolution events: none handled.
Balances and Pro persist to window end after the store returns the money.
Also: Coinbase `Event.Data.Metadata.Email` / `Payments[0]` nil-deref on a
malformed-but-signed event → panic → permanent 500-retry loop
(`subscription_controller.go:282,292`).

- [x] Handle revocation events per store -- real-time webhook handlers landed
      2026-08-07 (policy: money returned = entitlement over NOW; the
      reconciler's hourly sweep remains the safety net behind them):
      - **stripe** (`subscription_stripe_controller.go`): `charge.refunded`,
        `refund.created`, `charge.dispute.created`. Subscription-linked
        (charge -> invoice in the `stripe_invoice` ledger) -> end that
        invoice's renewal + the pro balance it granted
        (`EndReconciledEntitlementForTransactions`, market=stripe scoped to
        the invoice). Single-charge/data-pack (charge -> checkout session ->
        the balance-code ledger): unredeemed code -> VOIDED (`cancel_time`
        column; redeem/check exclude cancelled codes), redeemed -> the granted
        transfer_balance ended. Unmappable charge -> `refund_unmatched`
        operator event, never a guess, still 200. Dedupe: a refund delivers
        via BOTH charge.refunded and refund.created, so the clawback is gated
        on the refund (or dispute) id in the `stripe_refund` ledger (migration)
        inside the same tx -- one clawback, one event. Disputes get their own
        `disputed` action.
      - **apple** (`apple_notification_controller.go`): `REFUND` / `REVOKE`
        through the SAME pinned-root verified path as SUBSCRIBED/DID_RENEW ->
        end the transaction's renewal + balance; the
        `apple_subscription_transaction` ledger row stays as history; the
        notification-UUID ledger absorbs redeliveries.
      - **play** (`subscription_controller.go`): RTDN `SUBSCRIPTION_REVOKED`
        (type 12) -> end by purchase token (network derived from the renewal
        rows -- works even when the token is 410-Gone at the store), verified
        against Google's state first (still-ACTIVE = ignored). `EXPIRED`
        (type 13) is a normal lapse and never claws back.
      - All clawbacks refresh pro state (`UpdateProNetwork`) and write a
        `payment_reconciliation_event` row (`refunded` / `disputed` /
        `revoked` / `refund_unmatched`), joining the reconciler's audit
        stream. Coinbase resolution events remain unhandled (no clawback
        surface in the current Coinbase flow -- data codes only; a refunded
        Coinbase charge shows up operator-side in Coinbase itself).
      - Not covered: Play voided one-time products (RTDN
        `OneTimeProductNotification`/voidedpurchases API -- the webhook only
        parses `subscriptionNotification`), and partial refunds claw back the
        FULL grant (money returned = entitlement over; the amounts are in the
        event details for the operator).
      - RATIFIED 2026-08-08 (user): the full-grant clawback policy for
        partial refunds, and both not-covered gaps above, are accepted as
        decided -- not defaults pending review. Revisit only if a real
        partial-refund or voided-one-time case shows up in the audit stream.
- [x] Coinbase decode guarded: nil event/data/metadata/payments on a
      signed-but-malformed event now return a descriptive error (retryable,
      dashboard-visible) instead of panicking into a permanent 500 loop.
      Re-checked 2026-10-03: stripe `subscription_stripe_controller.go:293-330`,
      apple `apple_notification_controller.go:59,175`, play
      `subscription_controller.go:744-751`, coinbase guards `:311-344`; tests
      `subscription_stripe_refund_test.go`, `apple_refund_test.go`,
      `play_revoke_test.go`. Ratified gaps unchanged; no voidedpurchases handling.

### S8 — `subscription_renewal` PK collapse — MEDIUM (known, consumers mapped)
`AddSubscriptionRenewalInTx` (`model/subscription_model.go:3188`) upserts
on `(network_id, subscription_type, end_time, start_time)` updating only
net_revenue/purchase_token: a second market with an identical window keeps
the first row's `market`/`transaction_id` and **overwrites** (not sums)
revenue. Consumers: `HasSubscriptionRenewal` (`:3236`),
`GetActiveSubscriptionRenewalMarkets` (`:3290` — second market invisible →
no cancel path in the UI), `UnsubscribeStripe` (`WHERE market='stripe'` —
collapsed renewal uncancellable), `AddProTransferBalanceToAllNetworks`
(`:3391` — subsidy revenue misstated). x402 is the most collision-prone
writer (its window is exactly the calendar month, see S9).

- [x] Fixed: migration adds market to the PK (NULLs normalized to '', existing
      rows kept -- the new key is a superset so nothing collides); the
      `AddSubscriptionRenewalInTx` ON CONFLICT target includes market;
      `AddProTransferBalanceToAllNetworks` now SUMS per-market subsidy revenue.
      All §S8 consumers verified; `TestSubscriptionBalanceMultipleMarkets`
      passes and `TestSubscriptionBalanceIdenticalWindowsTwoMarkets` pins that
      identical windows in two markets both surface with their own
      revenue/transaction ids.
      Re-checked 2026-10-03: migration `db_migrations.go:4808-4815`, ON CONFLICT
      `model/subscription_model.go:4645`.

### S9 — x402 `pro_1month` grants the remainder of the calendar month — MEDIUM
`x402GrantProMonth` (`controller/x402_controller.go:677`) uses
`ProGrantWindow(now)` = start-of-month → start-of-next-month+1d. Paying
full price on the 28th buys ~3 days; a second purchase in the same month
collapses onto the same renewal PK and extends nothing. Settle-then-grant
failure is logged `SETTLED BUT NOT GRANTED` (`:526`) and needs manual
repair — an agent retry re-charges.

- [x] Fixed: `x402GrantProMonth` grants a rolling 30d+grace from purchase
      time; settle→grant is idempotent on the settle transaction (renewal
      transaction_id / balance purchase_token checked in the grant tx), and a
      second purchase extends via its own row+window (rides S8).
      `TestX402GrantProMonthRollingWindow`,
      `TestX402GrantIdempotentOnSettleTransaction`,
      `TestX402SecondPurchaseInOneCalendarMonthExtends`.
- [x] Fixed 2026-10-03 (`fix/upgrade-s9` `1f035242`, merged in `c4a65512`):
      a grant failure after settlement writes a `settled_not_granted`
      `payment_reconciliation_event` keyed by the settle transaction. The
      hourly reconciler has an x402 leg that retries the grant (idempotent on
      the settle transaction) and resolves the event; `bringyourctl payments
      reconcile --store=x402`. `TestX402SettledButNotGrantedIsRecordedAndReconciled`,
      `TestX402ReconcileResolvesWithoutDoubleGrant`. The event query has not
      been run against Postgres.

### S10 — Stripe checkout session: per-session key, quantity ignored — LOW (latent)
`stripeHandleCheckoutSessionCompleted` loops line items
(`subscription_stripe_controller.go:305`) but `CreateBalanceCode` keys on
the session id alone — a two-line-item session fulfills only the first;
`Quantity` is parsed and never multiplied. Currently self-created sessions
are single-item/qty-1. Also `purchaseEmail == ""` errors out before
crediting even when `redeemNetworkId` is known (`:299`) — after Stripe's
72 h retry window, paid and unfulfilled.

- [x] Fixed: per-line purchase-event keys (line 0 keeps the bare session id so
      already-fulfilled sessions stay idempotent); quantity multiplies the data
      granted; a known network is credited even without an email (the email
      error now only fires when BOTH are missing).
      `TestStripeCheckoutFulfillsEveryLineAndQuantity`.
      Re-checked 2026-10-03: `subscription_stripe_controller.go:435,467-471,526,538`;
      `subscription_stripe_checkout_test.go:293`.

### S11 — Wrong-network credit via legacy email fallback — LOW — FIXED (removed 2026-10-04)
`stripeHandleInvoicePaid:484-499` falls back to `FindNetworkIdByEmail` for
renewals without subscription metadata; a Stripe customer email matching a
different account credits that account. Acknowledged in a comment
(`:1244-1247`) but live.

- [x] Mitigated: the email fallback is demoted to LAST resort (metadata, then
      checkout-session client_reference_id, then email) and warns loudly when
      used. Full retirement still needs live verification that every legacy
      subscription carries metadata.
- [x] Kept + audited (user decision 2026-08-07, superseded 2026-10-04): every invoice.paid credit
      that resolves its network by the email fallback ALSO writes a
      `payment_reconciliation_event` (store=stripe, action=`email_fallback`,
      evidence=invoice id, details incl. subscription id + the matched email)
      -- once per credited invoice, redeliveries excluded by the
      stripe_invoice gate. The stripe reconciler leg counts these since the
      last watermark into its store result + heartbeat, and `bringyourctl
      payments reconcile` prints the count and a line per event, so any use
      is explicitly surfaced until the fallback can be retired.
- [x] Superseded (2026-10-04): mitigated, then retired below.
      - The comment that listed the old order (email second) was corrected
        (`fix/upgrade-s11` `ef72f6be`, merged in `9ed88be1`).
- [x] Fixed (removed, user decision 2026-10-04): the email fallback is
      deleted (`fix/stripe-remove-email-fallback` `5f9f5975`). An invoice
      resolves its network only from subscription metadata `network_id`, then
      the checkout session's `client_reference_id`. `FindNetworkIdByEmail`,
      the `email_fallback` action and its reconciler/CLI summary are gone.
      - An invoice that names no network is never credited. The invoice.paid
        webhook records it once as a stripe `credit_unfulfillable`
        `payment_reconciliation_event` (evidence = invoice id; details:
        subscription, customer, customer email, amount, currency, paid
        period, `leg=webhook`) and answers 2xx, so Stripe does not retry it
        for 72h or disable the endpoint. It answered 500 before. A failed
        record write still answers non-2xx so Stripe redelivers. The hourly
        reconciler writes the same details with `leg=credit`.
      - Monitor §2.22 `payment-credit-unfulfillable` (WARN) counts distinct
        such invoices in 24h; it replaces `payment-identity-fallback`.
      - Support repair: verify the paying account from payment evidence (not
        the email alone), set `network_id` metadata on the Stripe
        subscription, then resend the invoice.paid event from the Stripe
        dashboard; the stripe_invoice ledger keeps the credit single.
      - Tests: hermetic `controller/subscription_stripe_destination_test.go`
        (metadata and checkout paths unchanged; email never used; webhook
        2xx + one record; record failure is non-2xx). DB-backed
        `TestStripeLegacyInvoiceEmailMatchIsNotCredited` and the updated
        `TestPaymentReconcileStripeLegacyDestinationResolutionMatchesDryRun`
        need Postgres and were not run here.

### S12 — Play subscription keeps billing a deleted account — MEDIUM (new 2026-10-03)
`NetworkRemove` (`controller/network_controller.go:176-214`) cancels only
Stripe (`UnsubscribeStripe`, `:189`) before `model.RemoveNetwork` (`:198`). An
active Google Play subscription keeps charging after the account is deleted,
and nothing on the server can credit it any more.

- [x] Fixed 2026-10-03 (`fix/play-cancel-on-delete` `1a67d9d3`, merged in
      `5a76b6c7`): cancels Play via `subscriptionsv2.cancel` (`DEVELOPER_REQUESTED_STOP_PAYMENTS`)
      and blocks deletion if lookup/cancel fails. It also refuses deletion
      while the App Store reports an auto-renewing subscription, which the
      server cannot cancel. That Apple-side refusal is a product/UX decision.
      Needs Play sandbox verification.

### S13 — Memo-less USDC payment never matches an intent — LOW (new 2026-10-03)
A USDC transfer without the reference/memo reaches S3's `no_intent` record
(`subscription_controller.go:1985-2006`) and can only be credited by hand.

- [x] Fixed 2026-10-03 (server `fix/usdc-memoless-match` `1ad2fc26`+`b841c08f`,
      merged in `76ed6bbb`; mmm `38e8f8a61`): every quote reserves a unique sub-cent amount suffix
      (new `solana_payment_amount_reservation` table), and an unambiguous
      exact-amount match credits.
      - **Release order:** the mmm branch must ship with or before the
        server change. It was not on mmm main when the server branch merged. Main's `UsdcPayPanel.jsx:57` rounds the quote to
        cents, so buyers who copy it would underpay.
      - Needs a live check of wallet USDC rounding.

---

## 3. Client findings (failure is silent here)

### Apple (`/Users/brien/urnetwork/apple`)
- **A1 — `finish()` before any server contact + no JWS + no restore = permanent
  dead end** on a lost webhook (`AppStoreSubscriptionManager.swift:142,:218`).
  The code's own comments acknowledge "a webhook can be lost".
  - [ ] Partial (2026-10-03, apple `933a9fdf`).
        - Fixed: every delivery path goes through
          `AppStoreTransactionMonitor.process` (`:165`) to
          `PurchaseReporter.reportAndFinish` (`PurchaseReporter.swift:135`).
          The paths are purchase (`AppStoreSubscriptionManager.swift:416-419`),
          `Transaction.updates` (`Monitor:104`), the launch sweep of
          `Transaction.unfinished` (`:119`), and restore. The JWS is
          persisted first (`:139`, `:269-306`) and reported via
          `verifyAppleTransaction` (`:247-264`) until terminal. `finish()`
          comes only after that (`:150-160`).
        - Remaining: restore's `currentEntitlements` scan
          (`Manager:575-584`) never REPORTS already-finished entitlements.
          Pre-fix victims (finished before any server contact, first webhook
          lost) stay unrecoverable, and the reconciler can't see them either
          (it walks the ledger). Fix: report each verified entitlement whose
          `appAccountToken` matches the network, report-only and no finish.
        - Sandbox check: offer-code redemptions (`redeemOffer`, `:293-352`)
          may carry no `appAccountToken`. The server then answers `invalid`
          (`apple_notification_controller.go:270-277`) and the transaction
          is finished silently. If confirmed, a product/security decision is
          needed on how to bind it.
        - There are no unit tests for `PurchaseReporter` or the monitor.
        - 2026-10-04: offer-code redemptions without an `appAccountToken`
          are bound to the network that was issued the welcome offer code
          (server `d1885478`, `apple_offer_code_binding_controller.go`;
          apple `958a025f`).
        - 2026-10-04, Play parity (`play_purchase_binding_controller.go`).
          A purchase token with no `externalAccountIdentifiers` (a Play
          Store promo code redemption, a purchase outside the app's billing
          flow) used to be credited by `VerifyPlayPurchase` to whichever
          session reported it first. It is now credited only when:
          - the token (or its `linkedPurchaseToken`) is already bound to the
            session network: a renewal, credited or already_credited; or
          - it is unbound, the session network holds an unredeemed welcome
            offer issued with a Play offer tag, a line item's
            `offerDetails.offerTags` carries `play_offer_tag` (or the issued
            tag) or its `signupPromotion.vanityCode.promotionCode` is
            `onboarding.offer.play_promotion_code` (new, default empty = off),
            and Google's `startTime` is in `[issued_at - 5 min, expires_at)`.
            The binding (`play_purchase_binding`, keyed by token with the
            chain root), the offer redemption (store `play`) and the credit
            through the purchase-token gate (`playCreditSubscriptionInTx`)
            commit in one ReadCommitted tx.
          Everything else is `invalid` and persists nothing. Tokens with an
          obfuscated account id are unchanged. The RTDN webhook resolves
          unlinked tokens through the binding (inheriting it along
          `linkedPurchaseToken`); never-bound stays unresolved (200 with a
          message, no credit). The reconciler uses the binding for the repair
          match and skips a renewal row whose unlinked token is bound to
          another network. Rows of never-bound unlinked tokens credited
          before this change keep their network.
        - No Play code pool: onboarding issues no Play codes (the welcome
          offer is bought in-app with its offer token, which sets the
          obfuscated id), and subscriptionsv2 reports a one-time promo code
          as an empty `oneTimeCode` with no identifier, so an issued code
          could not be matched to its redemption.
        - Sandbox check (license tester): redeem a Play promo code from the
          Play Store with the app closed and read the token's
          subscriptionsv2. Confirm `externalAccountIdentifiers` is absent,
          which of `offerDetails.offerId`/`offerTags` and
          `signupPromotion` are set, and that a promo code (a 3-90 day free
          trial on the backwards-compatible base plan, per the Play Console
          docs) never carries the `onboarding25` tag. If the welcome offer
          should be reachable by promo code, configure a custom code as
          `play_promotion_code`.
- **A2 — Optimistic "You're premium"**: `purchaseSuccess` set at finish time
  (`:144-145`); the 120 s poll's `purchaseConfirmationTimedOut`
  (`SubscriptionBalanceViewModel.swift:304-310`) has **zero consumers** —
  user pays, sees success, silently stays Free.
  - [ ] Partial (2026-10-03).
        - Fixed in the UI: `purchaseSuccess` is set only on a credited
          answer, or on a non-terminal answer while the poll runs
          (`Manager:424-468`). The success copy tracks the poll phase.
          `purchaseConfirmationTimedOut` is consumed (`MainView.swift:198-208`,
          `AccountRootView:576`, `ConnectView-iOS:482`, `ConnectView-macOS:317`).
        - Remaining: the app still uses its own `SubscriptionBalanceViewModel`
          (not the SDK controller). Its 120 s deadline is wall-clock
          (`:49`, `:366`, `:430`) and runs while the app is inactive. Fix:
          port onto the SDK `SubscriptionBalanceViewController` (§6 step 2).
- **A3 — No restore-purchases mechanism** (no `AppStore.sync`, no
  `currentEntitlements` scan, no button). Compounds A1; also a review risk.
  - [x] Fixed (2026-10-03): `AppStore.sync()`, then the unfinished sweep, then
        the `currentEntitlements` scan (`Manager:533-600`). Buttons in
        `SettingsForm-iOS.swift:206/223`, `SettingsForm-macOS.swift:280/297`,
        the upgrade sheet, success and intro views. The proof-reporting gap
        is tracked under A1. Minor: a second tap during restore returns
        `.failed` silently (`:534-536`).
- **A4 — Guest purchase can strand paid balance**: guests can buy (Connect
  tab + intro funnel are not `isGuest`-gated); the app bypasses the SDK's
  `UpgradeGuest` and re-runs `networkCreate`/`authLogin`
  (`ConnectView-iOS.swift:424-440`, `AccountRootView.swift:480-530`) —
  whether the guest's subscription survives depends on server semantics
  (and see S6).
  - [ ] Partial (2026-10-03). Guest creation is gone (apple `6618d9ea`,
        `58307d97`; server S6). `createInstantAccount` still sends
        `guestMode = true` (`UrApiService.swift:499`), but the server ignores
        it and mints a real seedphrase network. Nothing calls `UpgradeGuest*`.
        Remaining: dead UI for legacy guest JWTs. Purchase is not gated on
        `isGuest` (`ConnectView-iOS:155,261`). "Create an account" logs into
        a NEW network (`AccountRootView:447-454,634-650`;
        `ConnectView-iOS:508-522`). `isGuest` defaults to true without a JWT
        (`AccountRootView:103`). Fix (small product decision): hide purchase
        for `guestMode == true`, or delete the legacy guest-upgrade UI.
- **A5 — Silent errors**: purchase no-ops if networkId is nil/unparseable
  (`AppStoreSubscriptionManager.swift:120-130`); all four call sites handle
  purchase errors with `print` only; `fetchProducts` failure at init is
  never retried → eternal spinner (`UpgradeSubscriptionSheet.swift:193-196`).
  - [x] Fixed (2026-10-03): nil networkId surfaces an error (`Manager:389-393`).
        Errors flow to `purchaseError` and render inline (`:496-500`).
        `fetchProductsError` + `retryFetchProductsIfNeeded` (`:209-235`) end
        the eternal spinner (`70173cde`). Ask to Buy pending is surfaced
        (`:481-490`). Minor leftovers: the unknown-result branch is
        print-only (`:492-493`), and `purchaseCompleted` analytics fire on
        invalid/wrong_network (`:421`).
- **A6 — iOS purchases ride the VPN; macOS disconnects first** ("purchase
  fails in mac app store if vpn is connected") — the highest-intent buyer
  (insufficient balance) purchases over a tunnel that may not carry traffic.
  - [-] Accepted / by design (documented `Manager:242-268`): iOS doesn't set
        `includeAllNetworks`, so StoreKit bypasses the tunnel. macOS
        disconnects around the purchase. Note: if `fix/insufficient-balance-disconnect`
        merges (out-of-balance holds traffic instead of disconnecting), the
        app's own verify/poll calls may be blocked on iOS. Sandbox-verify
        before merging it.
- **A7 — Transaction listener is session-scoped** (starts with `MainView`),
  not process-scoped; cross-account transactions trigger the wrong-account
  poll.
  - [x] Fixed (2026-10-03): `AppStoreTransactionMonitor.shared.start` at app
        launch (`NetworkApp.swift:134`). Wrong-network is answered by the
        server (`wrong_network`) and gated by the manager's token check
        (`Manager:130-154`), with a snackbar (`MainView:187-197`).

### Android (`/Users/brien/urnetwork/android`)
- **N1 — Acknowledge destroys the safety net**: `acknowledgePurchases`
  (`google/.../PlanViewModel.kt:296-320`) acknowledges as soon as Play says
  PURCHASED, with no server contact; `reconcileExistingSubscriptions`
  filters `!isAcknowledged` (`:218`), so acknowledged-but-uncredited is
  invisible to every future reconcile. Client cannot detect or repair.
  - [ ] Partial (2026-10-03, android `bf155381`).
        - Fixed: `google/.../PurchaseReporter.kt` persists the token
          (`:100-108`), calls `verifyPlayPurchase` (`:238-259`) until
          `isPurchaseReportTerminal` (`:171`) with SDK backoff (`:184`), and
          acknowledges only after that (`:205-235`). A daily WorkManager
          job takes over after 3 in-session tries.
        - Remaining: `reconcileExistingSubscriptions` still filters
          `PURCHASED && !isAcknowledged` (`PlanViewModel.kt:348-350`).
          Purchases acknowledged by pre-fix builds and never credited stay
          invisible, and the server reconciler only knows tokens from
          existing renewal rows. Fix: report every PURCHASED token at least
          once, with a per-token "reported-terminal" flag, plus a one-time
          legacy sweep.
        - Sandbox check: the worker cold start has a non-null
          `MainApplication.api` (`Worker:91`).
- **N2 — Optimistic overlay** ("You're premium.") before acknowledgement or
  any server confirmation; 120 s poll gives up with only a log line.
  - [ ] Partial (2026-10-03).
        - Fixed on the Play flavor: success/restore overlays fire only on
          credited/already_credited (`PlanViewModel.kt:528-530`). A
          confirmation-delayed dialog shows on deferral or timeout
          (`SubscriptionBalanceViewModel.kt:427-448`, `MainNavHost.kt:516,532`).
          The deadline pauses while backgrounded (`:395-408`).
        - Remaining: on the Stripe-sheet flavors, `onStripePaymentSuccess`
          (`MainNavHost.kt:1276-1279`) and the `UPGRADE_SUBSCRIPTION_SUCCESS`
          path (`google/.../MainActivity.kt:211-215` and flavor copies) still
          launch `OverlayMode.Upgrade` before the server confirms. The SDK
          controller isn't used. Fix: port to `SubscriptionBalanceViewController`
          and launch the overlay only on its confirmed state.
- **N3 — PENDING → PURCHASED depends on the app being opened**: parental
  approval + >3 days unopened = Play auto-refund of an approved purchase.
  No WorkManager job, no persistence of tokens.
  - [x] Fixed (2026-10-03): `PendingPurchaseReconcileWorker.kt:192-241` (daily,
        network-constrained, KEEP), armed on PENDING (`PlanViewModel.kt:457-463`)
        and re-armed at start (`:722`). Tokens are persisted (N1). Sandbox:
        parental approval with the app closed. Daily cadence against the 3-day
        window gives only 2–3 attempts.
- **N4 — Reconcile-on-start is the whole restore story** and its
  `queryPurchasesAsync` error path is logged-and-dropped with no retry
  (`PlanViewModel.kt:233-239`).
  - [x] Fixed (2026-10-03): 3 retries with 1/2/4 s backoff (`PlanViewModel.kt:385-414`).
        The worker returns `Result.retry()` on query/connect errors
        (`Worker:72-88`).
- **N5 — Stripe PaymentSheet failure swallowed** in solana_dapp/ethos_dapp
  (`onStripePaymentFailed = {}`); payment-link buttons silently no-op on
  nil networkId (`ungoogle/.../UpgradePlanAlt.kt:196-203`).
  - [ ] Partial (2026-10-03, `3083e940`). `onStripePaymentFailed = {}` and
        `UpgradePlanAlt.kt` are gone. `PaymentSheetResult.Failed` sets
        `setChangePlanError` (`stripeSheet/.../PlanPurchasers.kt:52-56`).
        Remaining: both purchasers silently `return` when
        `planViewModel.api == null` (`stripeSheet/...PlanPurchasers.kt:64`,
        `webPay/...PlanPurchasers.kt:93`). Fix: surface `setChangePlanError`
        there.
- **N6 — Solana return-path reference is memory-only**; process death while
  the wallet is foregrounded loses the confirmation UX; poll cap 20 s is
  shorter than typical finality + webhook latency.
  - [ ] Open (2026-10-03).
        - The reference lives in a plain `MutableStateFlow`
          (`SolanaPaymentViewModel.kt:21-25`) and is lost on process death.
        - `pollSolanaTransaction(maxDurationMs = 20_000L)`
          (`SubscriptionBalanceViewModel.kt:381`, triggered at
          `MainNavHost.kt:622-631`) doesn't set `isPollingSubscriptionBalance`,
          so its timeout is silent (`:442`).
        - Fix: persist the reference and plan (SavedStateHandle or
          SharedPreferences), lengthen the cap to about 60–120 s or use the
          SDK confirmation budget, and show a "still checking" message. The
          cap needs a light product call.
- **N7 — Balance-code errors collapse** into one toast; a
  network-failure-after-commit looks like "bad code" while consumed.
  - [ ] Partial (2026-10-03). Transport vs invalid is split, and the transport
        copy says "may already be applied"
        (`RedeemTransferBalanceCodeViewModel.kt:21-30,67-91`). Remaining: the
        AlreadyRedeemed branch matches "already"/"redeemed" in the server
        message (`:81-84`). The server returns "Unknown balance code." for
        both cases (`server/model/transfer_balance_code_model.go:94`), so
        that branch never fires. Fix: classify with
        `Sdk.classifyBalanceCodeRedeem` against the network's redeemed-code
        list, and gate with `Sdk.isBalanceCodeFormatValid` (`:45` hardcodes 26).

### Web + extension (`/Users/brien/urnetwork/mmm/ur.io`)
- **W1 — `/checkout/success` false-confirms**: `CheckoutReturn.jsx:43`
  captures `startingBalance` on first render of a fresh page load, when
  balance is still `EMPTY_BALANCE` — any pre-existing balance instantly
  "confirms" (`:57`), masking a lost webhook with a success page. The 90 s
  honesty path is effectively unreachable.
  - [ ] Partial (2026-10-03, mmm `c258d3559`).
        - Fixed: a sessionStorage baseline recorded before every checkout
          (`BuyData.jsx:181`, `UpgradeSheet.jsx:152`, `FreeTrialPanel.jsx:69`,
          …), plus a pure `purchaseLanded()` (`auth/checkoutBaseline.js:79-102`)
          that never counts an unfetched balance. Tests:
          `scripts/checkout-baseline.test.mjs`.
        - Remaining (a), product decision: `?network=` returns jump straight
          to `STATE_APPLIED` without polling (`CheckoutReturn.jsx:52`). The
          server adds `network=` to every named-network data buy
          (`pay_data_checkout_controller.go:277-288`), including the signed-in
          `/buy-data` card flow, so a lost webhook still shows a ✓. Either
          accept that (the copy is hedged) or poll when the network is your
          own.
        - Remaining (b): email-a-code returns (`item=` without `network=`)
          spin forever logged out (`:77` early exit), and say "taking longer
          than usual" logged in. Fix: route to an "emailed code" state. Check
          the live Stripe return URL.
- **W2 — Proxies screen sells data packs where Pro is gated**: the same
  duplicated create-proxy flow renders `BuyDataPacks` in
  `app/screens/Proxies.jsx:373-385` but `UpgradeSheet` (Pro) in
  `AccountPanel.jsx:395-409`. If data packs don't grant Pro, users pay and
  still can't create the proxy. (Also: the flow is duplicated wholesale —
  that's how they diverged.)
  - [x] Fixed (2026-10-03): both screens mount the shared
        `components/CreateProxyFlow.jsx` (`Proxies.jsx:7,95`,
        `AccountPanel.jsx:12,128`). The only gate upsell is `UpgradeSheet`
        (`CreateProxyFlow.jsx:363`). Confirmed that data packs never grant Pro
        (`server/model/pro_model.go:12-19`; balance-code/solana/x402 data
        write `pro=false`).
- **W3 — Solana waiting state can spin forever**: polls only
  `refreshBalance()` (`AccountPanel.jsx:732-743`), never `refreshSession()`
  — a jwt-only Pro grant never resolves the sheet; no deadline; no wallet
  installed = silent no-op into "waiting".
  - [ ] Partial (2026-10-03). Fixed: polls `refreshSession()` +
        `refreshBalance()` (`UpgradeSheet.jsx:86-108`), 5 min deadline with a
        timeout view (`:41`, `:274-287`), and the pay panel shows address,
        memo and QR in-tab. Remaining: the Pro flip resolves only from
        `waiting` (`:73`), and the deadline is wall-clock while polling
        pauses when the tab is hidden. A return after more than 5 min can
        show "not seen" on a paid purchase (`:98-101`). Fix: move
        `timeout` to `success` on `isPro`, or pause the deadline while hidden.
- **W4 — bfcache strands checkout buttons**: in-flight flags are never
  reset on back-navigation (`BuyData.jsx:55-56`, UpgradeSheet, FreeTrial) —
  restored page shows a permanently disabled "Opening checkout…".
  - [x] Fixed (2026-10-03): `hooks/useInFlightReset.js:19-45` (`pageshow`
        persisted, plus an 8 s visibility reset) is wired into BuyData,
        UpgradeSheet, FreeTrialPanel, BuyDataPacks, Offer and OnboardingDialog.
        Live Safari/Chrome check pending.
- **W5 — 15 s client timeout without abort** (`api.js:9-17`): a slow
  redeem/checkout call reports failure after the server committed.
  - [ ] Partial (2026-10-03). `auth/api.js:15,21-34` now uses the SDK client,
        which aborts and sets `isTimeout`. Regressed for redeem: `40181d349`
        (2026-09-02) moved redeem to the wasm host
        (`BalanceCodes.jsx:72`, `AccountPanel.jsx:223`). There, a Go 15 s
        timeout rejects with a plain `Error` (`sdk/js/sn.go:28`,
        `sdk/js/account_host.go:343-353`), so the ambiguity branches
        (`BalanceCodes.jsx:94`, `AccountPanel.jsx:241`) never run. Fix: set
        `isTimeout`/`kind` in `sdk/js` `apiPromise` for deadline/timeout
        errors, or redeem via api.js. Then classify already-redeemed via
        `ClassifyBalanceCodeRedeem`.
- **W6 — extension**: initiates no payments; jwt pushed once at SETUP and
  never re-pushed after upgrade.
  - [x] Fixed (2026-10-03): `repushExtensionJwt` (`app/extension/extensionStore.js:159-189`)
        runs on session refresh, checkout confirm, Solana success and redeem.
        The extension has a `REFRESH_JWT` verb (extension `ea08128`, tested).
        Live check: the store build (0.1.7) includes it. Older builds fall
        back to SETUP.

### Windows / Linux (`/Users/brien/urnetwork/{windows,linux}`)
- **D1 — Focus-gated confirmation polling guarantees a false "timed out"**
  for a normal hosted checkout: focus loss stops the 5 s poll while the
  2-minute wall-clock deadline keeps running
  (win `SubscriptionBalance.cpp:95-133`, `AppController.cpp:194,227-246`;
  linux `SubscriptionBalance.cpp:76-86,221-262`). User types card details
  in the browser (> 2 min, zero polls), returns → TimedOut without a single
  fetch. Windows additionally never leaves TimedOut once shown
  (`BalanceSheets.cpp:919-926` only transitions off Waiting).
  - [ ] Partial (2026-10-03).
        - Fixed: the budget is monotonic and counts only while polling, and
          TimedOut moves to Success on Pro (win
          `SubscriptionBalance.cpp:15-33,139-174`, `BalanceSheets.cpp:1195-1207`,
          `02bccd6`; linux `SubscriptionBalance.cpp:87-110,350-409`,
          `UpgradeSheet.cpp:738-751`, `26b8e67`).
        - Remaining: pause is driven by visibility, not focus (win
          `AppController.cpp:29-40,728-738`; linux `MainWindow.cpp:46-48,191-197`).
          In hosted checkout the window stays visible behind the browser, so
          the budget still burns while the user pays (it recovers to Success
          later). The comments claim focus pauses it (win
          `SubscriptionBalance.h:5-9,106-112`; linux `SubscriptionBalance.hpp:16-18`).
        - Fix: pause the confirmation budget on focus loss too (or port to
          the SDK controller with `SetForeground`), and fix the comments.
- **D2 — "Invalid balance code" for every failure** including
  network-failure-after-server-commit (win `BalanceSheets.cpp:351-379`;
  linux `RedeemCodeSheet.cpp:260-263`) — the user is told a credited code
  is invalid.
  - [ ] Partial (2026-10-03). Transport failure gets its own
        `balance_code_transport_error` message (win `BalanceSheets.cpp:394-431`;
        linux `RedeemCodeSheet.cpp:276-296`). Remaining: there is no
        already-redeemed case, so a retry after a lost-but-credited response
        shows the server's "Unknown balance code." Neither app uses
        `ClassifyBalanceCodeRedeem`/`IsBalanceCodeFormatValid`/`CheckBalanceCode`
        (26 hardcoded: win `BalanceSheets.cpp:42`, linux `RedeemCodeSheet.cpp:10-11`).
        They also disagree on an empty result: success on linux, failure on
        windows.
- **D3 — (linux, FIXED) tray Quit called `Logout()`**, whose SDK
  implementation is `os.RemoveAll(localStorageDir)` — permanently
  destroying guest accounts and their paid balance.
  - [x] Fixed: `SdkHost::Shutdown()` (teardown without auth wipe) wired to
        `tray->on_quit`; build-verified green 2026-08-07.
        Re-checked 2026-10-03: linux `main.cpp:129-137` → `SdkHost::Shutdown`
        (`SdkHost.cpp:3650-3660`). Windows never had it: Quit →
        `AppController::Shutdown` (`AppController.cpp:140,228-270`).
- **D4 — WebView2 process death after charge, before redirect** shows an
  error and starts no polling (win `BalanceSheets.cpp:804-814`); retry
  creates a second session.
  - [ ] Partial (2026-10-03). Windows fixed: when the page has loaded,
        process death shows `checkout_interrupted_confirming` and starts
        polling; otherwise it falls back to hosted (`BalanceSheets.cpp:833-847,1056-1081`;
        `02bccd6`, `7859cbd`). Linux open (new with WebKit embedded checkout,
        `a89ed74`): `web-process-terminated` (`UpgradeSheet.cpp:606-620`)
        always returns to Options with "Something went wrong" and starts no
        polling. Fix: if `pageLoaded_`, show `checkout_interrupted_confirming`,
        call `balance_.StartConfirmationPolling()` and enter Waiting;
        otherwise call `OnCheckoutLoadFailed()`.
- **D5 — hosted-checkout `LaunchUriAsync` fire-and-forget** (win
  `BalanceSheets.cpp:741-745`): async launch failure still advances to
  Waiting with no payment page open.
  - [x] Fixed (2026-10-03): `LaunchHosted` awaits `LaunchUriAsync` and shows a
        copyable URL on failure (win `BalanceSheets.cpp:979-1007`). Linux
        launches synchronously and checks the result (`UpgradeSheet.cpp:494-508`).
        Minor: the windows portal `OpenUrl` (`SettingsPage.cpp:107-114`) is
        still fire-and-forget.
- **D6 — Tray-resident app never refreshes entitlement** (both): no polls
  while hidden/unfocused; "Pro with balance" stops polling for the session.
  - [x] Fixed (2026-10-03): fetch on every window show, Pro included (win
        `SubscriptionBalance.cpp:119-137`; linux `:106-121`). `OnJwtRefreshed`
        catches a lapse. No polling while in the tray is by design (the
        server enforces).
- **D7 — No subscription management entry point** on windows (unwired
  `site_billing_portal_error`/`site_manage_billing_hint` resources): users
  must find the Stripe portal on the website themselves.
  - [x] Fixed (2026-10-03): windows "Manage Subscription" →
        `stripeCreateCustomerPortal` (`SettingsPage.cpp:195,569-576,1093-1125`;
        `f780285`). Linux: `AccountPage.cpp:1481-1540` (`40878c7`). Minor:
        the row shows for free and store subscribers too (likely a raw portal
        error; product call), and `site_billing_portal_error` is still unused.
- **D8 — guest checkout not diverted (linux)**: the insufficient-balance
  banner routes guests into Pro checkout (`ConnectDrawer.cpp:96-122,505`)
  while the plan card makes guests create an account first — a guest can
  buy a subscription bound to an account that D3 (pre-fix) could destroy.
  - [ ] Partial (2026-10-03). Routing is fixed: guests go to account
        creation, not checkout (linux `ConnectDrawer.cpp:136-146`,
        `MainWindow.cpp:1590-1594,1681-1685`; win `MainWindow.xaml.cpp:154-163,1514-1524,1754-1757`).
        Guest sign-up buttons are gone (win `dcdec98`, linux `9409d7a`).
        Remaining: the target still calls `UpgradeGuest` (win
        `LoginPage.cpp:366` → `SdkHost.cpp:960-972`; linux
        `AuthViews.cpp:439-442` → `SdkHost.cpp:830-851`), which now always
        fails. A legacy guest sees the raw SDK error. Fix (product decision
        plus a count of live legacy-guest networks): replace with "sign out
        and create an account" plus a balance-loss warning, or offer a
        server conversion (e.g. add-auth + claim-name). Delete the
        unreachable `LoginAsGuest` code (win `LoginPage.cpp:990-1010`,
        linux `SdkHost.cpp:463`).

---

## 4. What belongs in the SDK (the duplication inventory)

Each of these is implemented ≥3 times today, with drift:

1. **Subscription-balance view controller** (the big one; linux
   `SubscriptionBalance.hpp:6-9` says it outright: "There is no SDK view
   controller for the subscription balance"). One implementation of:
   - isPro derivation (`current_subscription != nil`) + jwt `pro`-claim
     reconciliation + refresh-jwt-on-disagreement;
   - balance arithmetic `used = start − available − pending`;
   - polling policy: 30–60 s background / 5 s confirmation / deadline —
     with the deadline **paused while polling is paused** (fixes D1
     structurally) and a terminal state distinguishing
     confirmed / still-waiting / give-up-with-reason (fixes A2/N2's silent
     timeout);
   - the "supporter with balance" stop rule.
   Consumers: apple `SubscriptionBalanceViewModel`, android
   `SubscriptionBalanceViewModel`, windows + linux `SubscriptionBalance`,
   web `AuthContext`/`CheckoutReturn`.
   - [ ] Partial (2026-10-03, sdk `e7680ba6`).
         - SDK done: `subscription_balance_view_controller.go` has isPro
           with jwt fallback (`:495-500,829`), reconcile in both directions
           (`:834-837`, `JwtRefreshed` `:446-458`), math (`:541-549`), 30 s /
           5 s / 120 s (`:32-38`), and a budget paused by
           `SetForeground(false)` (`:93-135,342-344`). States: idle / waiting
           / confirmed / gave_up (`:65-80`). Stop rule at `:794-796`. 17 tests.
         - Remaining: (1) no client uses it (apple, android, windows,
           linux, web all still hand-roll). (2) Give-up has no reason, and
           fetch errors are swallowed (`:79`, `:810-815`). Unmerged sdk
           `fix/web-fetch-error-states` (`15b6fd93`) adds
           `SubscriptionBalanceFetchErrorListener` but doesn't regenerate
           cgo. (3) Porting desktop is blocked: the controller fetches plain
           `SubscriptionBalance` (`:247`), but desktop needs
           `subscriptionBalanceForStorefront` (price tier/offer/experiments).
2. **Purchase reporting** ("submit proof, retry with backoff, then finalize"):
   android should report the Play token via `SubscriptionCreatePaymentId`
   *before* acknowledging (N1); apple should send the JWS before
   `finish()` (A1) — needs a server verify endpoint per store. This turns
   lost-webhook from money-gone into retryable, on every platform at once.
   - [x] Foundation landed 2026-08-07 (committed in `3e98ccbb`, 2026-08-09): session-authed
         `POST /subscription/verify-play-purchase`
         `{package_name?, product_id, purchase_token}` and
         `POST /subscription/verify-apple-transaction` `{signed_transaction}`,
         both answering `{status, expiry_time?}` with status ∈
         `credited|already_credited|pending|invalid|wrong_network`
         (`controller/subscription_verify_controller.go`). Credits flow through
         the EXISTING gates: play via `PlaySubscriptionRenewal` (purchase-token
         advisory xact lock + in-tx overlap re-check), apple via
         `appleCreditSubscriptionTransactionInTx` (transaction ledger) after
         the FULL pinned-root webhook verifier (`verifyTransaction` in
         `api/handlers/apple_notification_verifier.go` — a client JWS is an
         unauthenticated push, unlike the reconciler's authenticated pulls).
         Both rate-limited 30/account/hour (`verify_store_purchase` action).
         SDK: `VerifyPlayPurchase` / `VerifyAppleTransaction`
         (async + Sync/SyncWithContext), `IsPurchaseReportTerminal`,
         `PurchaseReportBackoffMillis` (1s/5s/30s/5m cap) with the client
         contract documented in `sdk/purchase_report.go`: persist proof →
         retry until terminal → only THEN acknowledge (android, N1) /
         `finish()` (apple, A1). The client reorders are the remaining half.
   - [x] Committed and adopted (2026-10-03). Server `3e98ccbb` (2026-08-09):
         routes `api/api.go:220-221`, handlers `subscription_handlers.go:94,114`,
         tests `subscription_verify_controller_test.go`,
         `subscription_verify_handlers_test.go`. SDK `e7680ba6`:
         `purchase_report.go`, `api.go:3209-3300`, tests
         `api_payment_test.go:560,607,649,666`. Clients report before
         finalizing: android `PurchaseReporter.kt:238-259` (acknowledge after
         terminal), apple `PurchaseReporter.swift:247-264` (`finish()` after
         terminal). Residual gaps are tracked in A1 and N1.
3. **Product/plan catalog**: `supporter`, `pro_monthly|pro_yearly`,
   `data_1tib|data_10tib`, Stripe payment-link URLs, Solana merchant +
   USDC mint, displayed-price fallbacks — scattered across ~12 files in
   5 repos.
   - [x] Fixed 2026-10-03: the intent result returns `recipient` and
         `spl_token_mint` (server `fix/upgrade-4-3` `23bc3004`, merged in
         `be8d2ce2`), and the SDK decodes them (sdk `bd173d29`). The
         `StripeItem*` constants were already in the cgo headers. No app
         builds the payment url from them yet.
   - Before the fix (2026-10-03). `payment_catalog.go` has the plan/item ids
         (`:30,42-47`), ui modes, store classification (`:75-109`). Prices
         come from the server (`StripePrices`, `onboarding_api.go:379-397`).
         No app uses Stripe payment links any more. Missing: the Solana
         merchant address and USDC mint. `solana_pay.go:77-84` says never
         hardcode them, but `SolanaPaymentIntentResult` (sdk `api.go:2854-2867`,
         server `subscription_controller.go:2229-2240`) has no recipient/mint
         field. The server keeps them private (`:1880-1885`). Fix: return
         `recipient` + `spl_token_mint` in the intent result and mirror them
         in the SDK/cgo/js. Also: the `StripeItem*` constants are not in the
         cgo exports.
4. **Checkout bridge envelope** (desktop): the
   `https://ur.io/checkout?client_secret&redirect_link` construction and
   `urnetwork://checkout?status=…` parsing, duplicated verbatim in windows
   + linux (plus two copies of a percent-encoder in windows alone).
   `StripeCreateCheckoutSessionArgs` should also grow
   `RedirectOnCompletion` (server supports it; SDK omits it).
   - [x] SDK done (2026-10-03): `payment_catalog.go:129-214`
         (`BuildCheckoutBridgeUrl[WithRedirect]`, `ParseCheckoutRedirect`),
         `RedirectOnCompletion` at `api.go:2972`, with tests and cgo export.
         Not adopted: windows `BalanceSheets.cpp:54-56,134,1086-1135` and
         linux `UpgradeSheet.cpp:45-46,71,673-692` still hand-build and parse
         (pay-sheet links too). Windows keeps two encoders
         (`BalanceSheets.cpp:93`, `WalletConnect.cpp:52`). Adoption is §6 step 4.
5. **Balance-code client rules**: the 26-char gate, and — more importantly —
   result classification that distinguishes transport failure / invalid /
   already-redeemed (fixes D2/N7/W5's "told it failed after it credited").
   - [x] SDK done (2026-10-03): `IsBalanceCodeFormatValid`
         (`payment_catalog.go:222-229`), and `ClassifyBalanceCodeRedeem`
         (`:273-294`, redeemed/already_redeemed/invalid/unknown, using the
         network's redeemed-code list). Tests at `payment_catalog_test.go:184,207`.
         Not adopted by any client (see D2, N7, W5). A code redeemed by
         ANOTHER network still classifies as invalid; telling those apart
         needs a server error code (minor product decision).
6. **Wallet-connect protocol** (auth): bridge URL assembly, NaCl envelope
   sequencing, the `"Welcome to URnetwork"` challenge — SDK has the
   primitives; every client re-implements the protocol.
   - [ ] Open (2026-10-03): the SDK has only primitives (`sdk.go:1162-1240`
         base58/NaCl/nonce, `AuthWalletChallenge` `api.go:468-527`). There is
         no session helper for bridge URLs or envelope sequencing. Needs a
         scoping decision (which wallets; who owns the redirect scheme)
         before building it.
7. **Missing SDK bindings**: `CheckBalanceCode` (apps do raw HTTP), and the
   stale `UpgradeGuest`/`UpgradeGuestExisting` (S6). Add an SDK↔routes
   integration test so removed server routes fail loudly.
   - [ ] Partial (2026-10-03). `CheckBalanceCode` is bound (`api.go:3158-3197`,
         tests). `UpgradeGuest*` are deprecated and fail fast (see S6).
         `TestSdkPaymentEndpointsMatchServerRoutes` (`api_payment_test.go:726-769`)
         exists, but it checks a hand-written list of 11 routes and misses
         `/solana/payment-intent`, `/subscription/stripe/payment-sheet` and
         `/subscription/stripe/prices`. It also `t.Skip`s when `../server`
         isn't checked out. Fix: derive the list by scanning SDK sources for
         URL formats, and make CI check out the server. Also, SDK main
         doesn't currently build against connect main
         (`api_client_registration.go:126`,
         `api_client_control_transport_test.go:101`).

## 5. Test coverage baseline (from the audit)

- Strong: Apple notification pipeline (idempotency, verifier suite),
  `solana_pay_test.go` (17 tests), pro derivation (`pro_model_test.go`).
- Absent: Stripe `invoice.paid` crediting (S2), **Play webhook — zero live
  tests** (S5), Helius batch handling (S1), balance-code concurrency (S4),
  x402 purchase/settle, and the SDK payment surface (everything except
  solana_pay: `SubscriptionBalance` decode, all four Stripe methods,
  `RedeemBalanceCode`, the dead guest-upgrade methods).
- Update 2026-10-03: all of the "absent" gaps above now have tests:
  - `subscription_stripe_invoice_test.go`
  - `play_webhook_test.go`, `play_revoke_test.go`
  - `solana_payment_test.go:442/506/562`
  - `transfer_balance_code_model_test.go:152`
  - `x402_grant_test.go`
  - `payment_reconcile_controller_test.go`
  - sdk `api_payment_test.go`, `subscription_balance_view_controller_test.go`
    and `payment_catalog_test.go`

  Still untested: apple `PurchaseReporter`/`AppStoreTransactionMonitor`.
  The SDK tests could not run on 2026-10-03 (sdk main does not build against
  connect main).

## 6. Implementation order

1. **Server money fixes** (S1–S5, S7 Coinbase guard) — small diffs, real
   money, each with a webhook/concurrency test. S1 and S2 first.
2. **SDK subscription-balance view controller** + tests; port apple,
   android, windows, linux, web to it (also closes D1, A2/N2 timeouts).
3. **Purchase reporting path** (SDK + server verify endpoints); reorder
   android acknowledge and apple finish behind it (A1, N1); add apple
   restore (`AppStore.sync` + `currentEntitlements` reconcile) (A3).
4. **Catalog + checkout envelope into the SDK** (kills the scattered ids
   and the duplicated bridge parsing); add `CheckBalanceCode`,
   `RedirectOnCompletion`; resolve guest upgrade (S6 + A4 + D8).
5. **Client silent-failure cleanup**: W1 CheckoutReturn baseline, W2
   product divergence, W4 bfcache resets, A5/N5 error surfacing, D2/N7
   balance-code classification (rides item 5's SDK work).
6. **Refund/revocation handling** (S7) and the renewal-PK schema fix (S8),
   which need design decisions (clawback policy; key shape).

Status 2026-10-03:
- [x] Step 1: S1–S5 and the S7 Coinbase guard are done (server `3e98ccbb`).
      The residual S3/S9 edges are fixed too (2026-10-03, §2).
- [ ] Step 2, partial: the SDK controller is done; no client is ported (§4.1).
      A2/N2/D1 timeouts are fixed per-app, not structurally.
- [ ] Step 3, partial: verify endpoints, SDK and both reorders are done. Open:
      apple restore doesn't report finished entitlements (A1), and android
      never re-reports legacy acknowledged tokens (N1). A3 restore is done.
- [ ] Step 4, partial: SDK catalog, envelope, `CheckBalanceCode` and
      `RedirectOnCompletion` are done (the Solana merchant/mint is on the
      intent result since 2026-10-03, §4.3).
      No client has adopted them. S6 is resolved by deprecation. The A4/D8
      legacy-guest UI needs a product decision.
- [ ] Step 5, partial: W1 (core), W2, W4, A5 and N5 (core) are done. Open: D2/N7/W5
      classification is not on the SDK, and W5 regressed on the redeem path.
- [x] Step 6: S7 and S8 are done (`3e98ccbb`).

## 7. Not statically determinable (needs sandbox/live verification)

- Store retry semantics in practice: Stripe/Helius/Pub/Sub redelivery on
  5xx, Stripe's 72 h horizon, Helius batching frequency.
- Apple sandbox: Ask-to-Buy declines, cross-device `Transaction.updates`
  timing, notification types beyond `SUBSCRIBED`/`DID_RENEW` for this
  product set.
- Play: RTDN concurrency, acknowledge-failure frequency, first-offer
  selection when multiple offers exist (`MainActivity.kt:416`).
- Whether a data-pack purchase grants Pro (decides W2's severity).
- Whether the Stripe payment links' success URL still redirects to the
  `ur.io/?subscription` deep link (lives in the Stripe dashboard).
- Live pro.yml / stripe.yml / play.yml / x402.yml contents, which several
  guards depend on.

---

## 8. Reconciliation (the lost-webhook safety net) — DESIGN, approved 2026-08-07

Every crediting path in §2 is webhook-only, so a lost webhook is a lost
credit and an unhandled revocation is a free subscription. Reconciliation
converts webhook-only into webhook-plus-safety-net: an hourly task pulls
payment truth from each store and repairs the server's subscription state —
in both directions.

### Principles

1. **Repairs flow through the idempotent crediting paths, never fresh
   writes.** The reconciler credits via the same gates the webhooks use
   (S2's stripe-invoice ledger, `apple_subscription_transaction`, the Play
   overlap gate, the Solana intent one-shot). A reconciler with its own
   write path would be a new double-credit source racing the webhooks it
   checks. This is why reconciliation lands after the S1–S5 fixes.
2. **Both directions.**
   - Store paid, server missing → credit (the lost-webhook repair).
   - Store affirmatively entitled, renewal present, Pro metadata missing →
     restore an exact-window, zero-byte/zero-revenue Pro marker after an
     in-transaction network/renewal recheck. This is metadata repair, not a
     second credit; it never trusts the local renewal without provider truth.
   - Store cancelled/refunded/expired, server active → end the entitlement
     (adjust `end_time`, refresh pro state). This delivers the recurring
     half of S7 clawback as a side effect: an hourly sweep against store
     truth catches revocations even with no webhook handler for them.
     Per user decision: auto-fix (not record-and-alert first); the audit
     table below makes every auto-fix reviewable after the fact.
3. **Every repair is recorded** in a reconciliation audit table
   (store, network_id, direction, what changed, store evidence id,
   run id, time). Operator visibility is the point: a spike in repair
   counts IS the alarm that webhooks are broken. A run that repairs
   nothing writes only a heartbeat row.
4. **Bounded work per run.** Iterate the server's own ledgers, not all
   networks: renewals active or expiring within ±48 h, plus store-side
   listings of recent activity since the last successful run (watermark
   per store in the audit table). Per-store API budgets; a store that
   rate-limits or errors is skipped for the run and reported — one broken
   store must not starve the other three.
5. **Missing credentials = skip + log, never fail.** Local/test envs
   don't carry store credentials; the reconciler runs with whatever
   stores are configured. A skipped store is visible in the heartbeat.

### Per-store truth

| store  | truth source | iterate over | credentials |
|--------|--------------|--------------|-------------|
| stripe | Subscriptions/Invoices API (stripe-go, existing key) | `subscription_renewal` market=stripe ±48 h + store-side recent subscriptions | `vault/<env>/stripe.yml` (existing key) |
| apple  | App Store Server API (Get Transaction / Subscription Statuses) | `apple_subscription_transaction` ledger | `vault/<env>/apple.yml` + `app_store_server_api_key_id`, `issuer_id`, `private_key` (p8) |
| google | Android Publisher `purchases.subscriptionsv2` (creds already used by RTDN verification) | purchase tokens from market=google renewal rows | `vault/<env>/google.yml` / `play.yml` (existing service account) |
| solana | Helius API (existing key) — token transfers to the pinned receivers | payment intents (incl. the S3 unmatched-payments table: late payments whose reference still resolves get credited here) | `vault/<env>/helius.yml` (existing) |

### Mechanics

- Task-system periodic task (`taskworker` registration + self-reschedule
  with `RunOnce("payment_reconciliation")`), cadence 1 h. One run at a
  time by construction (tasks are singletons).
- New table `payment_reconciliation_event` (migration): run id, store,
  network_id NULL-able, action (`credited` / `ended` /
  `entitlement_repaired` / `skipped_store` / `heartbeat` / `error`), evidence
  (store object id), details json, event_time. Plus a per-store watermark for
  the incremental store-side listing.
- Apple/Google are per-transaction lookups — iterate our ledger rows, not
  store-wide listings. Stripe supports listing by created/current-period
  windows. Solana reconciles from our intent + unmatched tables against
  Helius transfer history for the receiver addresses.
- The credit leg reuses the exact controller crediting functions (post
  S1–S5), so a reconcile credit is idempotent against a late webhook
  arriving for the same event, and vice versa.
- Manual runs: `bringyourctl payments reconcile [--dry-run]
  [--store=<stripe|apple|google|solana>]` runs the same orchestrator the
  task runs (`RunPaymentReconciliationWithOptions`), never a separate
  implementation. `--dry-run` audits what a real run WOULD repair: store
  reads happen for real, every write is suppressed (no credit, no ended
  entitlement, no unfulfilled-record clearing, no watermark advance — a
  dry run must not eat the incremental window a later real run needs),
  and each suppressed repair is recorded BOTH as a printed line and as a
  durable `would_credit`/`would_end`/`would_repair_entitlement` audit row
  tagged `dry_run = true`
  (column added by migration, default false, so existing
  heartbeat/error/repair queries exclude dry runs unchanged). Mutual
  exclusion: every real run — task or CLI — holds a run-level session
  advisory lock (`payment_reconciliation_run`) for its whole duration;
  RunOnce only serializes task-scheduled runs, the lock covers the CLI
  entry point too. A second real run reports busy (the task errors and is
  rescheduled with backoff; the CLI exits non-zero); dry runs are
  lock-free since they write nothing. The CLI exits non-zero if any store
  errored.
- Deployment order: dry-run audit (`bringyourctl payments reconcile
  --dry-run`, review the would_ lines against real store data) → manual
  real run (`bringyourctl payments reconcile`) → enable the hourly task.

### Status

- [x] Implement — landed 2026-08-07 on top of the wave-1 gates:
      `taskworker/work/payment_reconcile_work.go` (hourly,
      `RunOnce("payment_reconciliation")`, registered in taskworker),
      `controller/payment_reconcile_controller.go` (all four reconcilers +
      seams), `model/payment_reconcile_model.go`, migrations
      `payment_reconciliation_event` + `payment_reconciliation_watermark`
      (watermark is a sibling table: one mutable value per store vs. the
      append-only audit trail). Every credit flows through the existing
      gates (`stripeHandleInvoicePaid`/`stripe_invoice` ledger, the
      `apple_subscription_transaction` gate factored into
      `appleCreditSubscriptionTransactionInTx`, `PlaySubscriptionRenewal`,
      the Solana intent one-shot factored into
      `solanaCreditPaymentIntent`). Implementation decisions §8 left open:
      stripe listing = `GET /v1/invoices?status=paid&created[gte]=watermark`;
      apple statuses via Get All Subscription Statuses with the response JWS
      decoded WITHOUT re-verification (authenticated TLS pull from Apple,
      unlike unauthenticated webhook pushes) and billing-retry (status 3)
      treated as entitlement-over; solana on-chain re-verification via
      `getSignatureStatuses` (searchTransactionHistory) only for credits
      older than 1 h; underpaid unfulfilled records stay for the operator
      (still underpaid); the end repair claws back the renewal rows AND the
      pro balances they granted (matched by identical window end), per the
      refund-means-money-returned reading — cancel-at-period-end is never
      touched; per-store budget 500 API calls/run, watermark advances only
      on a complete error-free store pass.
- [x] Add App Store Server API credentials to `vault/<env>/apple.yml`
      (the only genuinely new credential; the other three stores' keys
      exist): top-level `app_store_server_api_key_id`, `issuer_id`,
      `private_key` (the .p8 contents); `bundle_id`/`product_ids` are read
      from the existing `app_store_notifications` block.
      Done 2026-10-03: the user added them to `vault/main/apple.yml`, and a
      read-only App Store Server API probe authenticated. Read by
      `appleReconcileCredentialsFromVault`
      (`controller/payment_reconcile_controller.go:175-200`). Verify the
      next hourly run writes no apple `skipped_store` row.
- [x] Local vault: stub `stripe.yml`/`apple.yml`/`google.yml` absent —
      reconciler must skip cleanly (tested:
      `TestPaymentReconcileSkipsStoresWithoutCredentials`; the suite pins
      the credential seams to absent so it is hermetic either way).
- [x] Real-time refund/revocation webhook leg (S7) + S11 email-fallback audit
      landed 2026-08-07 -- see §2 S7/S11 for the per-store handling. The
      handlers reuse this section's machinery (`EndReconciledEntitlement*`
      scoped variants, `payment_reconciliation_event` as the shared operator
      stream), so webhook clawbacks and reconciler end-repairs read as one
      audit trail. The previous deferred item -- charge-level stripe refunds
      invisible to the reconciler -- is now covered by the real-time
      `charge.refunded`/`refund.created` handlers.
- Deploy notes (verified against the live Stripe API 2026-08-07): the
  production webhook endpoint ALREADY subscribes to `charge.refunded`,
  `refund.created`, and `charge.dispute.created` -- the full event catalog is
  enabled, so NO dashboard work is needed; the change is handler-side only.
  Because the full catalog is enabled, the handler's unknown-event behavior
  (ignore + 200, pinned by `TestStripeWebhookUnknownEventTypeStill200`) is
  load-bearing: a non-2xx on an unhandled type would make Stripe retry for
  72h and then DISABLE the endpoint, taking the crediting webhooks with it.
  Apple REFUND/REVOKE and Play SUBSCRIPTION_REVOKED arrive on the existing
  notification endpoints -- no store-side configuration either. New
  migrations: `stripe_refund` (refund-id idempotency ledger) and
  `transfer_balance_code.cancel_time` (voided codes).
- Deferred: no operator alerting/dashboard on repair-count spikes yet (the
  audit table is queryable; grafana wiring is a follow-up).
  Update 2026-10-03: a monitor signal now exists
  (`monitor/signal_payment_reconciliation.go`, `c4faf3f9`, 2026-09-08). The
  reconciler task is registered at `taskworker/taskworker.go:116,496`.
