# Store Sandbox Test Plans

2026-08-08. The consolidated on-device test passes for the purchase-reporting
reorder (wave 2, `server/UPGRADE.md` §4.2): android report-before-acknowledge
and apple report-before-finish. These require real sandbox accounts and
devices and cannot run on the build host — this document is the execution
plan; check items off in a copy per test pass.

Server-side prerequisites:
- The verify endpoints accept both `Production` and `Sandbox` environments
  (`vault/main/apple.yml app_store_notifications.environments`), so TestFlight
  JWS verifies rather than answering `invalid`.
- **Sandbox purchases credit internal testers only** (owner, 2026-11-17:
  "Both fixes", then "Keep buttons, neutral notice";
  `controller/apple_sandbox_testers.go`). A sandbox purchase is free, so it
  is credited only when its network is listed in
  `vault/main/apple.yml app_store_notifications.sandbox_tester_network_ids`,
  a YAML list of network id strings (an App Store `appAccountToken` is the
  network id):

  ```yaml
  app_store_notifications:
    sandbox_tester_network_ids:
      - "<network id>"
  ```

  A missing or empty list credits no sandbox purchase. Every Apple test pass
  below runs on a listed network, and so does App Review: the App Review demo
  account's network must be on the list, or the reviewer's purchase is
  refused. For any other network:
  - the purchase report (verify and restore) answers `invalid` with
    `error.code = "sandbox_purchase"`, and the app finishes the transaction
    and shows its neutral line ("Test purchases in test builds don't unlock
    Pro.");
  - the webhook acknowledges the notification and credits nothing (no ledger
    row, so the same purchase is credited once its network is listed);
  - a sandbox subscription's billing grace holds nothing.

  Production purchases are unchanged, and nothing already credited is ended:
  an existing sandbox window runs out, and its next sandbox renewal is
  credited only for a listed network. The apps keep their purchase buttons
  in TestFlight builds, so App Review can buy.
- Rate limit: `verify_store_purchase` 30/account/hour — the bounded client
  retries (≤3/session + daily worker/redelivery) stay well inside it, but
  watch for tight loops in logs during the failure drills below.

## Apple (StoreKit sandbox / TestFlight)

0. **Not a tester**: on a network that is not in `sandbox_tester_network_ids`,
   buy in a TestFlight build → the server answers `invalid` with
   `sandbox_purchase`, the transaction finishes, the sheet shows "Test
   purchases in test builds don't unlock Pro.", and the plan stays Free.
   Restore purchases shows the same line. Then add the network to the list
   and restore → `credited`.

1. **Happy path**: purchase → server answers `credited` → transaction
   finishes; confirmation poll starts only after the server answer (not at
   StoreKit success); `Transaction.unfinished` empty on relaunch; the
   persisted JWS entry (`ur.pendingPurchaseReports` in UserDefaults) cleared.
2. **Kill between purchase and report** (airplane mode during purchase,
   force-quit): relaunch → launch sweep redelivers → reports → finishes.
   No user action, no duplicate credit (`already_credited` on the rerun).
3. **Server unreachable**: block the api → processing-shaped copy (never
   "You're premium"), transaction stays unfinished, JWS persisted; restore
   network → next pass credits and finishes.
4. **Ask to Buy**: child purchase + deferred approval; approval arriving
   while logged OUT defers; logging into the purchasing network reports and
   starts the poll.
5. **Cross-account**: purchase under network A, sign into network B →
   `wrong_network` snackbar, transaction finished (no StoreKit redelivery
   loop), B's poll never starts, A credited via webhook/reconciler.
6. **Webhook-first race**: let the sandbox server notification credit before
   the client report → report answers `already_credited`, finishes cleanly,
   exactly one credit (verify in `subscription_renewal` / balances).
7. **Restore purchases**: fresh install, same Apple ID →
   Settings → Restore: `restored` outcome, poll starts; a different
   network's purchase → "purchased under a different account".

## Google Play (license-tester sandbox)

1. **Happy path**: buy `supporter` → server `credited` → THEN
   `isAcknowledged` flips (check log ordering); overlay processing-shaped
   until `currentSubscription` confirms; prefs
   (`pending_purchase_reconcile`) cleared; worker settles.
2. **Kill between PURCHASED and terminal answer**: token persisted,
   purchase unacknowledged; next app start (or daily worker) reports →
   acknowledges. Must complete well inside Play's 3-day auto-refund window.
3. **Server unreachable**: after ~3 attempts the delayed-confirmation
   dialog shows; purchase stays unacknowledged; recovery on next reconcile.
4. **PENDING purchase** (slow test card): PENDING overlay; on flip to
   PURCHASED the report runs before ack; server `pending` keeps the loop
   alive without acknowledging.
5. **Cross-account**: purchase under A's network, sign in as B →
   "purchased under a different account", purchase still acknowledged
   (no auto-refund), no credit to B.
6. **RTDN-first race**: exactly one credit, one poll start.
7. **Revocation** (refund from Play console): RTDN SUBSCRIPTION_REVOKED →
   entitlement ends + `revoked` audit event; EXPIRED lapse does NOT claw.

## Stripe (test mode — scriptable, no device)

Run against a test-mode api server with the stripe CLI:
- `stripe trigger invoice.paid` → exactly one credit on redelivery
  (`stripe_invoice` ledger); `stripe trigger charge.refunded` /
  `refund.created` → ONE clawback for the pair; dispute → `disputed` event.
- `/checkout/success` page: pay with a pre-existing balance → confirmation
  only on Pro flip or balance INCREASE (the W1 fix).
- Legacy invoice (S11): a legacy-shaped invoice.paid (no subscription
  metadata, no checkout client_reference_id) whose customer email matches an
  account → 200, NOT credited, one `credit_unfulfillable` event (stripe,
  evidence = invoice id) with subscription, customer, email, amount and
  period; a redelivery adds no second event.

## Solana (devnet/mainnet dry)

- Pay a quote after the 1h intent expiry → credited via the unfulfilled
  sweep or recorded operator-visible (never silently kept).
- `payments reconcile --dry-run` after a webhook-suppressed payment →
  `would_credit` for it.
