# Store Sandbox Test Plans

2026-08-08. The consolidated on-device test passes for the purchase-reporting
reorder (wave 2, `server/UPGRADE.md` §4.2): android report-before-acknowledge
and apple report-before-finish. These require real sandbox accounts and
devices and cannot run on the build host — this document is the execution
plan; check items off in a copy per test pass.

Server-side prerequisites (all verified in code/config, nothing to change):
- The verify endpoints accept both `Production` and `Sandbox` environments
  (`vault/main/apple.yml app_store_notifications.environments`), so TestFlight
  JWS verifies rather than answering `invalid`. Note the standing tradeoff
  this implies (pre-existing, webhook path included): a sandbox (free)
  purchase can credit real entitlement. Accepted for TestFlight testing.
- Rate limit: `verify_store_purchase` 30/account/hour — the bounded client
  retries (≤3/session + daily worker/redelivery) stay well inside it, but
  watch for tight loops in logs during the failure drills below.

## Apple (StoreKit sandbox / TestFlight)

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
- Email-fallback: a legacy-shaped invoice.paid (no metadata) →
  `email_fallback` audit event + `bringyourctl payments reconcile` summary
  line.

## Solana (devnet/mainnet dry)

- Pay a quote after the 1h intent expiry → credited via the unfulfilled
  sweep or recorded operator-visible (never silently kept).
- `payments reconcile --dry-run` after a webhook-suppressed payment →
  `would_credit` for it.
