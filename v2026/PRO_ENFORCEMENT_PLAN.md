# Pro plan enforcement + x402 + upgrade flows — audit & plan

Deep audit of `server`, `config`, `sdk`, `connect`, `apple`, `android` (2026-07). Product spec now lives in `config/main/pro.yml`. This doc turns the audit into an implementation plan. All refs are `file:line`.

## The one seam that matters

Every payment rail converges on two model calls: **grant Pro** = `AddSubscriptionRenewalInTx` (writes `subscription_renewal`, type `supporter`) **+** `AddTransferBalanceInTx` (paid balance); **add data** = `AddTransferBalance` / `RedeemBalanceCode`. Canonical: Stripe `invoice.paid` at `controller/subscription_stripe_controller.go:498-522`. **x402 = the same grant, a different rail.**

Spendable data only ever comes from a `transfer_balance` row. "Insufficient balance" is raised at `model/subscription_model.go:914-917` in `createTransferEscrowInTx`. Pro status has **two** definitions that can disagree — `IsPro` (any *paid* balance, `subscription_model.go:3015`) vs `HasSubscriptionRenewal` (supporter renewal, `:2976`). **x402 must write both**, like Stripe does, to avoid skew (the code itself flags this, `:3020-3023`).

## 1. config/<env>/pro.yml  ✅ created (config/main/pro.yml)

free/pro: `concurrent_clients`, `data`, `data_period`, `features{http_proxy,https_proxy,socks_proxy,wireguard_proxy}`; plus `providers` (unlimited), `referral{bonus_per_referral,period,max_referrals}`, `data_code{duration:8760h, skus:[$5/1tb,$30/10tb]}`.

## 2. server/pro.go (new, `package server`)

`sync.OnceValue` loader mirroring `controller/subscription_controller.go:74-94`:
`server.Config.RequireSimpleResource("pro.yml").UnmarshalYaml(&c)`; byte fields via `model.ParseByteCount`. Expose `server.Pro()` with: `MaxConcurrent(pro bool) int`, `Data(pro bool) (ByteCount, time.Duration)`, `FeatureEnabled(pro bool, feat string) bool`, `ReferralBonus() (ByteCount, time.Duration, maxN int)`, `DataCodeDuration() time.Duration`, `DataCodeSkus()`.
- Replace hardcoded grant constants `RefreshSupporter/FreeTransferBalance` + `RefreshTransferBalanceDuration` (`subscription_controller.go:35,39,40`) with `server.Pro()` (map already keyed by supporter at `:1033-1036`).
- **Fix first:** `jwt/by_jwt.go:516` hardcodes `pro:=false` when a JWT is rebuilt from client_id (`// FIXME`). Every connect-time `byJwt.Pro` check is untrustworthy until fixed.

## 3. Enforcement + 402

**Concurrent = CONNECTED top-level clients (not provisioned).** The limit counts *active* connections, with a public-provider exemption. Enforce in two places:
- **Connection activation** — when a client goes active (websocket connect, `connect/transport.go:169`), reject if the network is already at `server.Pro().MaxConcurrent(byJwt.Pro)` connected clients. This is a *new* count source: live connections via `exchange.registerConnection` (`connect/transport.go:345`), **not** the `network_client` provisioned count.
- **Client creation** — `model.AuthNetworkClient` new-client branch (`network_client_model.go:221-252`): also reject provisioning a new top-level client when already at the connected limit (replaces the fixed `LimitTopLevelClientIdsPerNetwork=100`).
- **Exemption:** a client running as a **public provider (public + stream provide mode)** does NOT count toward the limit. The active-count query/registry must exclude public providers (provide mode is available at the connect handshake).

**OPEN — cross-instance counting:** a per-network *connected* limit is global, but connections land on different server instances (`registerConnection` is in-memory). Enforcing it needs a shared per-network live-connection count (redis incr-on-connect / decr-on-disconnect, with crash reconciliation like the net-escrow pattern), or a residency service. This is the main new infrastructure piece for decision 4.

**Feature gate:** server `ProxyConfig{EnableWg,...}` at `network_client_model.go:115-123` + provider-exit `proxy/server.go:91` — gate per `server.Pro().FeatureEnabled`.

**402 mechanics — the hard part (3 surfaces, 3 mechanisms):**
- **HTTP JSON API** (`/network/auth-client`): true 402 works via `router` numeric-prefix — `return nil, fmt.Errorf("402 Payment Required.")` → `RaiseHttpError` (`router/handler_utils.go:440-446`, same trick as 401). **But** the current limit path returns **HTTP 200 + `error.client_limit_exceeded`** (`network_client_model.go:244-251`); changing to 402 breaks existing clients. → **Add a stable error code** (`payment_required` / `feature_not_included` / `concurrent_limit_exceeded`) to the error object **and** set 402 status. Clients key on the code, not the status.
- **Websocket connect** (`connect/transport.go:169`): post-`Upgrade` there's no HTTP status; enforce pre-upgrade (before `:279`) or via a protocol frame.
- **Contract open** (`controller/connect_controller.go:335-350`): protocol frame only — add a `ContractError_*` code mirroring `InsufficientBalance`.

## 4. x402 (Stripe facilitator, USDC on Base/Solana/Tempo)

- New handlers `POST /subscription/pro/x402`, `POST /subscription/balance/x402` (raw `http.HandlerFunc` writing `402` + x402 `accepts` terms; the `Wrap*` helpers can't set arbitrary status).
- On verified payment, settle by calling the **exact** grant Tx from `stripeHandleInvoicePaid` (Pro month) or `AddTransferBalance` (data top-up). Add `const SubscriptionMarketX402 = "x402"` at `subscription_model.go:2915` (market is free `varchar(64)`, no migration).
- Idempotency: reuse `subscription_payment` or the `solana_payment_intent` intent-table pattern.
- Template to copy: the Helius/Solana USDC webhook `controller/subscription_controller.go:1451,1526-1571`.

## 5. Referrals — 3 GB/day for life

No data grant exists today (referrals only share *payout points*, `account_point_model.go:198`). Build a daily task (like `ScheduleRefreshTransferBalances`, `subscription_controller.go:1009`) that per referrer iterates `GetReferralsByReferralNetworkId` and calls `AddBasicTransferBalance(referrer, 3gb, today, today+period)` — **free/unpaid** so it doesn't falsely confer Pro. Lift the hardcoded `maxReferralsPerNetwork=5` (`network_referral_model.go:60`) per pro.yml. Expose `total_referrals` (already returned by `/account/referral-code`).

## 6. SDK/connect — surface the signals (prerequisite for all apps)

The SDK flattens every non-200 into an opaque string at `connect/net_http.go:1226/1342/1428`, and drops `ClientLimitExceeded` at `ip_remote_multi_client_api.go:173`; provider-window auth errors are swallowed + infinitely retried (`ip_remote_multi_client.go:2408-2417`). **Work:**
- Introduce typed `HttpStatusError{StatusCode,Body}` at the 3 flatten points.
- Extend `sdk.ContractStatus` (`sdk/device.go:149-153`, aggregated `device_local.go:1047-1100`, bridged `device_rpc.go`) with `NeedsUpgrade` / `ClientLimitExceeded` — reusing the existing `ContractStatusChangeListener` pipeline that already carries `InsufficientBalance`/`NoPermission`/`Premium`.
- The **data-code trigger already exists**: `ContractStatus.InsufficientBalance`. Apps just need to act on it when Pro.

## 7. Apps — 402→Upgrade, Pro+insufficient→data code

Both apple & android are structurally identical: **the redeem-data-code UI and the upgrade/paywall UI are already fully built** — only triggers/routing are missing, and every insufficient-balance UI is wrongly gated to `!isPro`, so **a Pro user out of data is silently disconnected with no prompt**.

**Apple** (`apple/app/network/...`): hub is `ConnectViewModel.updateContractStatus()` (`ConnectViewModel.swift:262-279`) — add `noPermission`→upgrade + Pro-insufficient→data-code branches; un-gate `ConnectActions.swift:56`, `ConnectButtonView.swift:46`, `ConnectStatusIndicator.swift:24`; present existing `UpgradeSubscriptionSheet.swift` / `RedeemBalanceCodeSheet.swift`; stop the blind auto-disconnect (`:276`). Wire the already-dead `openUpgradeSheet` hook (`ConnectButtonView.swift:24`).

**Android** (`android/app/app/src`): hub `ConnectViewModel.kt` (`_contractStatus`, auto-disconnect `:254-258` is plan-blind); un-gate `ConnectScreen.kt:94` (`&& !isPro`); branch in `ConnectActions.kt:123-140`/`ConnectButton.kt`/`ConnectStatusIndicator.kt` (also localize hardcoded strings `:40-41`); host existing `RedeemTransferBalanceCodeSheet.kt`; reuse `Route.Upgrade`. `noPermission`/`premium` currently dead-end at a log (`MainApplication.kt:585`).

**Not yet audited:** extension, windows, linux (windows/linux clients are early; `sdk/proxy_device.go` run() is a stub).

## Decisions (resolved 2026-07)
1. ✅ Binary units (GiB/TiB), shown on the user-facing site too.
2. ✅ SOCKS + WireGuard are Pro-only (free: http + https only).
3. ✅ Referral cap = 10.
4. ✅ Concurrent = CONNECTED top-level clients; gate at connect AND create; public providers (public+stream) exempt.

## Still open
- **402 delivery mechanics:** error-code in body + 402 status on HTTP endpoints, protocol code on connect/contract (recommended cross-client-safe approach).
- **Cross-instance live-connection count** store for the connected limit (see §3) — the main new infra piece.
- **Data-code Pro skew:** a *paid* data code currently reads as Pro via `IsPro` (any paid balance). Should buying a data code grant Pro *entitlements* (1k concurrent, socks/wireguard) or only data? Recommend **data-only** — decouple entitlements from data-code balances so features stay tied to the subscription.
- **Public-provider detection:** confirm "public + stream" maps to the provide-mode flags available at the connect handshake for the exemption query.

## Suggested build order
1. `pro.go` + wire data-grant amounts (behavior-neutral: matches product page) + fix `by_jwt.go:516`.
2. Concurrent-client enforcement + error code + 402 on `/network/auth-client`.
3. SDK typed-error + `ContractStatus.NeedsUpgrade`.
4. Apps: un-gate Pro-insufficient → data code (highest-impact UX fix), then 402→upgrade.
5. x402 endpoints + settlement.
6. Referral daily grant.
7. Feature gating (wireguard) once client support lands; extension/windows/linux.
