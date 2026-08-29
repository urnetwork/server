# Payment & Upgrade Audit — Findings and Plan

2026-08-07. Six parallel read-only audits: android, apple, windows, linux,
mmm/ur.io + extension, and the server crediting paths + SDK payment surface.
Scope: every flow where a user pays or upgrades, hunting (a) silent failure,
(b) fail-to-credit (user loses money), (c) key logic duplicated across apps
that belongs in the SDK.

Status legend: `[ ]` open · `[x]` fixed · `[-]` accepted / by design.

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

### S6 — SDK `UpgradeGuest` / `UpgradeGuestExisting` → 404 — MEDIUM
`sdk/api.go:1747` posts `/auth/upgrade-guest`, `:1793`
`/auth/upgrade-guest-existing`; both routes were removed in server commit
`340d828a` (2026-07-15) with no remaining handler. Every shipped app using
the SDK guest-upgrade flow gets a 404. A guest who bought Pro/data has no
supported conversion path that keeps what they paid for.

- [ ] Decide: restore the routes, or remove/deprecate the SDK methods and
      give guests a supported conversion path. An SDK integration test
      against `api.Routes()` would have caught this.

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

### S11 — Wrong-network credit via legacy email fallback — LOW
`stripeHandleInvoicePaid:484-499` falls back to `FindNetworkIdByEmail` for
renewals without subscription metadata; a Stripe customer email matching a
different account credits that account. Acknowledged in a comment
(`:1244-1247`) but live.

- [x] Mitigated: the email fallback is demoted to LAST resort (metadata, then
      checkout-session client_reference_id, then email) and warns loudly when
      used. Full retirement still needs live verification that every legacy
      subscription carries metadata.
- [x] Kept + audited (user decision 2026-08-07): every invoice.paid credit
      that resolves its network by the email fallback ALSO writes a
      `payment_reconciliation_event` (store=stripe, action=`email_fallback`,
      evidence=invoice id, details incl. subscription id + the matched email)
      -- once per credited invoice, redeliveries excluded by the
      stripe_invoice gate. The stripe reconciler leg counts these since the
      last watermark into its store result + heartbeat, and `bringyourctl
      payments reconcile` prints the count and a line per event, so any use
      is explicitly surfaced until the fallback can be retired.

---

## 3. Client findings (failure is silent here)

### Apple (`/Users/brien/urnetwork/apple`)
- **A1 — `finish()` before any server contact + no JWS + no restore = permanent
  dead end** on a lost webhook (`AppStoreSubscriptionManager.swift:142,:218`).
  The code's own comments acknowledge "a webhook can be lost".
- **A2 — Optimistic "You're premium"**: `purchaseSuccess` set at finish time
  (`:144-145`); the 120 s poll's `purchaseConfirmationTimedOut`
  (`SubscriptionBalanceViewModel.swift:304-310`) has **zero consumers** —
  user pays, sees success, silently stays Free.
- **A3 — No restore-purchases mechanism** (no `AppStore.sync`, no
  `currentEntitlements` scan, no button). Compounds A1; also a review risk.
- **A4 — Guest purchase can strand paid balance**: guests can buy (Connect
  tab + intro funnel are not `isGuest`-gated); the app bypasses the SDK's
  `UpgradeGuest` and re-runs `networkCreate`/`authLogin`
  (`ConnectView-iOS.swift:424-440`, `AccountRootView.swift:480-530`) —
  whether the guest's subscription survives depends on server semantics
  (and see S6).
- **A5 — Silent errors**: purchase no-ops if networkId is nil/unparseable
  (`AppStoreSubscriptionManager.swift:120-130`); all four call sites handle
  purchase errors with `print` only; `fetchProducts` failure at init is
  never retried → eternal spinner (`UpgradeSubscriptionSheet.swift:193-196`).
- **A6 — iOS purchases ride the VPN; macOS disconnects first** ("purchase
  fails in mac app store if vpn is connected") — the highest-intent buyer
  (insufficient balance) purchases over a tunnel that may not carry traffic.
- **A7 — Transaction listener is session-scoped** (starts with `MainView`),
  not process-scoped; cross-account transactions trigger the wrong-account
  poll.

### Android (`/Users/brien/urnetwork/android`)
- **N1 — Acknowledge destroys the safety net**: `acknowledgePurchases`
  (`google/.../PlanViewModel.kt:296-320`) acknowledges as soon as Play says
  PURCHASED, with no server contact; `reconcileExistingSubscriptions`
  filters `!isAcknowledged` (`:218`), so acknowledged-but-uncredited is
  invisible to every future reconcile. Client cannot detect or repair.
- **N2 — Optimistic overlay** ("You're premium.") before acknowledgement or
  any server confirmation; 120 s poll gives up with only a log line.
- **N3 — PENDING → PURCHASED depends on the app being opened**: parental
  approval + >3 days unopened = Play auto-refund of an approved purchase.
  No WorkManager job, no persistence of tokens.
- **N4 — Reconcile-on-start is the whole restore story** and its
  `queryPurchasesAsync` error path is logged-and-dropped with no retry
  (`PlanViewModel.kt:233-239`).
- **N5 — Stripe PaymentSheet failure swallowed** in solana_dapp/ethos_dapp
  (`onStripePaymentFailed = {}`); payment-link buttons silently no-op on
  nil networkId (`ungoogle/.../UpgradePlanAlt.kt:196-203`).
- **N6 — Solana return-path reference is memory-only**; process death while
  the wallet is foregrounded loses the confirmation UX; poll cap 20 s is
  shorter than typical finality + webhook latency.
- **N7 — Balance-code errors collapse** into one toast; a
  network-failure-after-commit looks like "bad code" while consumed.

### Web + extension (`/Users/brien/urnetwork/mmm/ur.io`)
- **W1 — `/checkout/success` false-confirms**: `CheckoutReturn.jsx:43`
  captures `startingBalance` on first render of a fresh page load, when
  balance is still `EMPTY_BALANCE` — any pre-existing balance instantly
  "confirms" (`:57`), masking a lost webhook with a success page. The 90 s
  honesty path is effectively unreachable.
- **W2 — Proxies screen sells data packs where Pro is gated**: the same
  duplicated create-proxy flow renders `BuyDataPacks` in
  `app/screens/Proxies.jsx:373-385` but `UpgradeSheet` (Pro) in
  `AccountPanel.jsx:395-409`. If data packs don't grant Pro, users pay and
  still can't create the proxy. (Also: the flow is duplicated wholesale —
  that's how they diverged.)
- **W3 — Solana waiting state can spin forever**: polls only
  `refreshBalance()` (`AccountPanel.jsx:732-743`), never `refreshSession()`
  — a jwt-only Pro grant never resolves the sheet; no deadline; no wallet
  installed = silent no-op into "waiting".
- **W4 — bfcache strands checkout buttons**: in-flight flags are never
  reset on back-navigation (`BuyData.jsx:55-56`, UpgradeSheet, FreeTrial) —
  restored page shows a permanently disabled "Opening checkout…".
- **W5 — 15 s client timeout without abort** (`api.js:9-17`): a slow
  redeem/checkout call reports failure after the server committed.
- **W6 — extension**: initiates no payments; jwt pushed once at SETUP and
  never re-pushed after upgrade.

### Windows / Linux (`/Users/brien/urnetwork/{windows,linux}`)
- **D1 — Focus-gated confirmation polling guarantees a false "timed out"**
  for a normal hosted checkout: focus loss stops the 5 s poll while the
  2-minute wall-clock deadline keeps running
  (win `SubscriptionBalance.cpp:95-133`, `AppController.cpp:194,227-246`;
  linux `SubscriptionBalance.cpp:76-86,221-262`). User types card details
  in the browser (> 2 min, zero polls), returns → TimedOut without a single
  fetch. Windows additionally never leaves TimedOut once shown
  (`BalanceSheets.cpp:919-926` only transitions off Waiting).
- **D2 — "Invalid balance code" for every failure** including
  network-failure-after-server-commit (win `BalanceSheets.cpp:351-379`;
  linux `RedeemCodeSheet.cpp:260-263`) — the user is told a credited code
  is invalid.
- **D3 — (linux, FIXED) tray Quit called `Logout()`**, whose SDK
  implementation is `os.RemoveAll(localStorageDir)` — permanently
  destroying guest accounts and their paid balance.
  - [x] Fixed: `SdkHost::Shutdown()` (teardown without auth wipe) wired to
        `tray->on_quit`; build-verified green 2026-08-07.
- **D4 — WebView2 process death after charge, before redirect** shows an
  error and starts no polling (win `BalanceSheets.cpp:804-814`); retry
  creates a second session.
- **D5 — hosted-checkout `LaunchUriAsync` fire-and-forget** (win
  `BalanceSheets.cpp:741-745`): async launch failure still advances to
  Waiting with no payment page open.
- **D6 — Tray-resident app never refreshes entitlement** (both): no polls
  while hidden/unfocused; "Pro with balance" stops polling for the session.
- **D7 — No subscription management entry point** on windows (unwired
  `site_billing_portal_error`/`site_manage_billing_hint` resources): users
  must find the Stripe portal on the website themselves.
- **D8 — guest checkout not diverted (linux)**: the insufficient-balance
  banner routes guests into Pro checkout (`ConnectDrawer.cpp:96-122,505`)
  while the plan card makes guests create an account first — a guest can
  buy a subscription bound to an account that D3 (pre-fix) could destroy.

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
2. **Purchase reporting** ("submit proof, retry with backoff, then finalize"):
   android should report the Play token via `SubscriptionCreatePaymentId`
   *before* acknowledging (N1); apple should send the JWS before
   `finish()` (A1) — needs a server verify endpoint per store. This turns
   lost-webhook from money-gone into retryable, on every platform at once.
   - [x] Foundation landed 2026-08-07 (uncommitted): session-authed
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
3. **Product/plan catalog**: `supporter`, `pro_monthly|pro_yearly`,
   `data_1tib|data_10tib`, Stripe payment-link URLs, Solana merchant +
   USDC mint, displayed-price fallbacks — scattered across ~12 files in
   5 repos.
4. **Checkout bridge envelope** (desktop): the
   `https://ur.io/checkout?client_secret&redirect_link` construction and
   `urnetwork://checkout?status=…` parsing, duplicated verbatim in windows
   + linux (plus two copies of a percent-encoder in windows alone).
   `StripeCreateCheckoutSessionArgs` should also grow
   `RedirectOnCompletion` (server supports it; SDK omits it).
5. **Balance-code client rules**: the 26-char gate, and — more importantly —
   result classification that distinguishes transport failure / invalid /
   already-redeemed (fixes D2/N7/W5's "told it failed after it credited").
6. **Wallet-connect protocol** (auth): bridge URL assembly, NaCl envelope
   sequencing, the `"Welcome to URnetwork"` challenge — SDK has the
   primitives; every client re-implements the protocol.
7. **Missing SDK bindings**: `CheckBalanceCode` (apps do raw HTTP), and the
   stale `UpgradeGuest`/`UpgradeGuestExisting` (S6). Add an SDK↔routes
   integration test so removed server routes fail loudly.

## 5. Test coverage baseline (from the audit)

- Strong: Apple notification pipeline (idempotency, verifier suite),
  `solana_pay_test.go` (17 tests), pro derivation (`pro_model_test.go`).
- Absent: Stripe `invoice.paid` crediting (S2), **Play webhook — zero live
  tests** (S5), Helius batch handling (S1), balance-code concurrency (S4),
  x402 purchase/settle, and the SDK payment surface (everything except
  solana_pay: `SubscriptionBalance` decode, all four Stripe methods,
  `RedeemBalanceCode`, the dead guest-upgrade methods).

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
  network_id NULL-able, action (`credited` / `ended` / `skipped_store` /
  `heartbeat` / `error`), evidence (store object id), details json,
  event_time. Plus a per-store watermark for the incremental store-side
  listing.
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
  durable `would_credit`/`would_end` audit row tagged `dry_run = true`
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
- [ ] Add App Store Server API credentials to `vault/<env>/apple.yml`
      (the only genuinely new credential; the other three stores' keys
      exist): top-level `app_store_server_api_key_id`, `issuer_id`,
      `private_key` (the .p8 contents); `bundle_id`/`product_ids` are read
      from the existing `app_store_notifications` block.
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
