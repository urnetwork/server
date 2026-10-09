# AUTHZ1: a client token must not administer the network

Owner requirement (2026-10-08): "the client ids created by embed should not be
able to admin the network in any way", and "We don't want third party customer
jwts to be able to admin their account in any way."

This document audits every route the API serves, `api/api.go` plus
`oauth.Routes()` (229 routes), for what a client token can do with it. It
records the rule the router now enforces, what remains open, and how each
admin route is tested.

Branch `fix/client-jwt-admin-authz`, from main `9a89d017`.

## 1. Credentials and the threat

- **Network credential.** The network's root token (the `by_jwt` from a
  sign-in, with no `client_id`) or an API key. `session/client_session.go`
  authenticates an API key as `jwt.NewByJwt(network_id, admin_user_id, ...)`,
  also without a client id.
- **Client token.** The `by_client_jwt` of one client (`client_id` and
  `device_id` set), minted by `POST /network/auth-client`. An embed customer's
  backend mints one for each installation of its app, so anyone who holds the
  app holds the token. Client tokens are minted for both the API and connect
  audiences, so they authenticate on every API route.

Before this branch, the router had no rule that separated the two.
`WrapRequireAuth` and `WrapWithInputRequireAuth` accept any authenticated
session, and only a few model functions checked `ByJwt.ClientId`. A client
token could therefore call almost every admin route.

**The SDK detail that shapes the fix.** `sdk/api_device_owner.go`
`setDeviceByJwt` installs the device's client token as the shared `Api`
credential when the device starts (`device_local.go:1803`), and
`replaceDeviceByJwt` keeps it there on refresh. Once the URnetwork apps have a
device, every SDK call they make, account settings included, carries the
client token. The apps mint their client with the root token at sign-in
(`DeviceManager.swift:1855`, `MainApplication.kt:264`), then hand the API to
the device.

## 2. Findings

| # | Gap | Severity | Routes | Status on this branch |
|---|---|---|---|---|
| G1 | A client token reissued **any** client's token (impersonation of every installation) | Critical | `POST /network/auth-client` with `client_id` | **Fixed** (model): only its own client and its children |
| G2 | A client token removed **any or all** clients (up to 1M per call) | Critical | `POST /network/remove-client`, `POST /network/remove-clients` | **Fixed**: remove-client is limited to its own client and children (model); remove-clients refuses a client token (router) |
| G3 | Escalation to a network credential: an API key is a full network credential | Critical | `POST /account/api-key` (and `/remove`, `GET /account/api-keys`) | **Fixed** (router) |
| G4 | Escalation through OAuth: a client token could approve a third-party OAuth client (dynamic registration is open) | Critical | `POST /oauth/authorize`, `POST /oauth/consent` | **Fixed** (router) |
| G5 | Escalation through sign-in methods: `/auth/code-create` makes an auth code that `/auth/code-login` exchanges for a **network** token; add-auth and the seedphrase routes add a way to sign in as the network | Critical | `POST /auth/code-create`, `/auth/add-auth`, `/auth/generate-seedphrase`, `/auth/regenerate-seedphrase` | **Fixed for Embed networks.** Open for ordinary networks: the apps call these with their client token (decision 1) |
| G6 | Account destruction and lockout | Critical | `POST /auth/network-delete`, `POST /auth/remove-auth` | **Fixed for Embed networks**; ordinary networks: decision 1 |
| G7 | Money movement and payout redirection | High | `POST /wallet/circle-transfer-out`, `/wallet/circle-init`, `GET /wallet/balance` (**fixed**, router); `/account/payout-wallet`, `/account/wallet`, `/account/wallets/remove`, `/account/wallets/verify-seeker` (Embed fixed; decision 1) | see routes |
| G8 | Billing | High | `POST /subscription/cancel`, `/subscription/resume`, `GET /subscription/details` (**fixed**, router); `POST /stripe/customer-portal` (Embed fixed; decision 1) | see routes |
| G9 | Device sharing and adoption into the network | High | `/device/add`, `/device/create-share-code`, share status/confirm/QR, associations | **Fixed** (router) |
| G10 | A client token minted top-level clients, and children of other clients | High | `POST /network/auth-client` without `client_id` | **Fixed** (model): a client token creates only children of itself |
| G11 | Network identity and settings | Medium | `POST /network/user/update` (**fixed**, router); change-name, claim-name, leaderboard visibility, emoji, location blocks, preferences, referral (Embed fixed; decision 1) | see routes |
| G12 | Account data a client does not need | Medium | API keys, provider earnings (`/stats/providers*`), proxies, subscription details, Circle balance (**fixed**, router); clients list, user profile, payments, wallets, payout wallet, balance codes (Embed fixed; decision 1) | see routes |
| G13 | A client token renamed **any** device | Low | `POST /device/set-name` | **Fixed** (model): only its own client's device |
| G14 | Test balance drain/restore (environment-gated) | Low | `POST /test/balance-drain`, `/test/balance-restore` | **Fixed** (router) |
| R1 | A client token maps its **own** client's provider payout coldkey | Low | `POST /sn/wallet`, `/sn/wallet/consent` | Unchanged, by design for provider apps (decision 3) |

G3 to G5 matter more than their routes suggest. While any route hands out a
network credential, or a way to sign in as the network, a client token can
become the network, and every other refusal is moot. On an Embed network all
of them are now closed. On an ordinary network the API key and OAuth routes
are closed, and the sign-in routes stay open until decision 1 is made.

## 3. The rule

1. **Every route is classified** in `api/route_authz.go`
   (`routeAccessByRoute`, keyed by `METHOD pattern`). The classes are:

   | Class | Meaning | Routes |
   |---|---|---|
   | Public | No credential, or one of its own kind (operator secret, webhook signature, signed objects, competition or admin token, OAuth protocol). A by_jwt grants nothing more | 113 |
   | Client | Any credential: acts for the caller's own client, reads what every installation shows, or buys or credits something the caller pays for | 43 |
   | Own client | A client token is accepted; the model limits it to its own client and the clients it created | 9 |
   | App admin | Administration the URnetwork apps call with their client token today. Refused for a client token of an Embed network | 27 |
   | Network only | Administration: the network credential only | 37 |

2. **The router enforces it.** `api.applyRouteAccess` wraps the route list
   that `routesWithReservedAttemptUpload` returns. That is the only change to
   `api/api.go`, so the hook stays mergeable. The router puts
   `router.RefuseClientCredentials` in front of every Network only and App
   admin route. The gate sits in the route's handler, ahead of whatever
   wrapper the handler uses
   (`router/client_credential.go`):
   - It reads the bearer token's claims unverified. A network token or an API
     key costs nothing extra and goes straight to the handler.
   - A token that names a client is verified (signature, lifetime, audience).
     A verified client token that the route refuses gets **403** with
     `A client token cannot administer the network. Use the network's root
     token or an API key.` The handler never runs, so nothing is read or
     written.
   - A token that fails verification, or a missing credential, goes to the
     handler, which refuses it exactly as before (401).
   - The status is 403, not 401, on purpose. The SDK's confirmed logout treats
     a 401 as a revoked credential, so a 401 here would sign a user out
     because of one admin call.
3. **The model limits what a client token keeps.**
   - `POST /network/auth-client`: no top-level mint ("A client token cannot
     create a top-level client."), and no child of another client. A reissue
     works only for its own client or a child it created. Any other client
     answers "Client does not exist.", the same answer as a missing client, so
     the route does not reveal which client ids exist.
   - `POST /network/remove-client`: only its own client and its children,
     through a `FOR UPDATE` lock on the owned row, then the existing
     deactivation writer.
   - `POST /device/set-name`: only its own client's device.
4. **Fail closed.** A route with no classification refuses client tokens.
   `TestEveryRouteIsClassified` fails until the route is classified, and it
   also fails for a classification that names no route.
5. **Embed networks.** `model.NetworkRefusesClientAdmin` decides the App admin
   class. Today it is true for a network on the Embed plan, which means it has
   a `network_top_level_client_limit` row (`bringyourctl network
   client-limit`). Once `feat/embed-flag` merges it should also be true when
   the explicit flag is set (§5, decision 2). The lookup goes through the
   existing per-process cache, and it runs only for a verified client token on
   an App admin route.

The first-party apps keep working on ordinary networks, and every route they
call with a client token is unchanged there. Shipped callers of the narrowed
own-client routes are unaffected: the connect multi-client generator
(`ip_remote_multi_client_api.go:812/969`) and the sn validator
(`validator/transport_client.go:204-245`) create and remove only their own
children. The apps, sn `clientauth` and the embed example backends mint
top-level clients with the root token or an API key.

## 4. Shipped clients that depend on a client token for admin routes

| Shipped client | Credential on admin routes | Dependency |
|---|---|---|
| URnetwork apps (apple, android) through the SDK device | **client token** | the 27 App admin routes, with the call sites listed in §7. They also call own-client routes (`/device/set-name` on their own device) and client routes |
| sdk view controllers (`devices_view_controller`, `network_user_view_controller`, `wallet_view_controller`, `account_preferences_view_controller`) | whatever token the app's `Api` holds (the client token) | `GET /network/clients`, `GET /network/user`, payout wallet, wallets, payments, preferences. All App admin |
| sdk `sn_wallet.go:308` (provider wallet connect) | client token, with its own `client_id` | `POST /sn/wallet`: Own client (model) |
| connect `ApiMultiClientGenerator`, sn validator | client token | `auth-client` and `remove-client` for their own children: still allowed |
| embed examples, client side (`go/embed/caps.go:90`) | client token | `GET /network/client-data-cap` for its own cap: Own client |
| embed examples, backend; provider examples | root token or API key | unaffected |
| ur.io (`react/src/auth/api.js`, `app/device/accountHost.js`), web manager (`manager/src/context/AuthContext.tsx`) | root token | unaffected |
| sn `hotkeywallet`/miner operator | root token | unaffected |

No shipped client calls a Network only route with a client token. Repos
searched: sdk Go and JS, apple, android, ur.io (mmm), examples, web, windows,
linux, extension, skill, connect and sn. The SDK has methods for some of them
(`CreateApiKey`, `ListApiKeys`, `DeleteApiKey`, `WalletBalance`,
`WalletCircleInit`, `WalletCircleTransferOut`, `NetworkUserUpdate`), but no app
and no SDK-internal caller uses them.

## 5. Owner decisions

**Decision 1: the App admin routes on ordinary networks (27 routes, including
the G5 escalation and G6 destruction routes).** The apps call these with
their client token, so refusing them for every network today would break the
apps. What is implemented now is option (a).

- **(a) Refuse client tokens on Embed networks only** (implemented). This is
  the owner's stated requirement for embed client ids and breaks no
  installed app. It leaves open any network that hands out client tokens
  without the Embed plan: today that is anyone using the SDK under the
  default 100-client limit.
- **(b) Also refuse a client in the `isolated` ACL group.** The embed examples
  put new clients there by default. This adds one cached per-client lookup on
  admin calls. It is partial: it relies on the integrator isolating its
  clients.
- **(c) The durable fix: the apps make admin calls with the network
  credential, then every App admin route becomes Network only.** The apps
  already receive the root token at sign-in, before they mint their client.
  The SDK would keep it (or an auth code) in an admin `Api` that is separate
  from the device's `Api`, and use that one for account calls. Old app builds
  would keep sending the client token, so the flip needs an adoption window.
  A counter of client tokens allowed through App admin routes, added to the
  gate, would show when the window can close. Recommended order: G5 and G6
  first (`/auth/code-create`, `/auth/add-auth`, both seedphrase routes,
  `/auth/network-delete`, `/auth/remove-auth`), then the rest.

**Decision 2: merging with `feat/embed-flag`** (another fork, same day). Three
changes are needed at merge time:
1. `model.NetworkRefusesClientAdmin`
   (`model/network_client_admin_model.go`) becomes
   `NetworkEmbedEnabled(ctx, networkId) || <the Embed plan override>`, so a
   network flagged without a client-limit override is covered too.
2. Classify the new route. `TestEveryRouteIsClassified` fails until it is:
   `"GET /network/embed": routeAccessNetwork`. That fork reads it with the
   root token or an API key.
3. The e2e fixture `routeAccessDbFixture.enableEmbed`
   (`api/route_authz_db_test.go`) also calls `model.Testing_EnableNetworkEmbed`.
   The data-cap and ACL routes refuse a network that is not Embed-enabled
   once the flag lands.

**Decision 3: R1.** A client token can map its own client's provider payout
coldkey, with the wallet's signed consent. The provider apps rely on this
(`PROVIDER_CONTRACT.md`). The earnings are for traffic that client provided.
Should an Embed network refuse it anyway? This branch leaves it as it was.

**Decision 4 (confirm): the 403 and its message.** These are new responses on
the admin routes. The OpenAPI spec (`connect/api/bringyour.yml`) does not
list 403 for them yet. That is a spec follow-up in connect, and the route
list itself is unchanged.

## 6. Tests

Pure tests run now; they sign tokens with the local vault key and touch no
database. The DB tests are written and compiled, and run after merge
(owner rule).

| Test | Kind | What it proves |
|---|---|---|
| `api/route_authz_test.go` `TestEveryRouteIsClassified` | pure | every route of `Routes()` is classified; no classification names a missing route; no route is registered twice |
| `TestAnUnclassifiedRouteFailsTheGuardAndRefusesClientTokens` | pure | a new unclassified route fails the guard and refuses client tokens until classified; a stale entry fails too |
| `TestOnlyAdminRoutesCarryTheGate` | pure | exactly the Network only and App admin routes carry the gate |
| `TestAdminRoutesRefuseAClientTokenThroughTheRouter` | pure | through the router the API serves, built from the real route objects, every admin route answers a signed client token with 403 before its handler (the App admin routes as an Embed network) |
| `TestAdminRoutesServeTheNetworkCredential` | pure | every admin route passes the root token, an API key and a missing credential to its handler, and refuses the client token |
| `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | pure | an ordinary network's client token still reaches the App admin routes (the apps), and never the Network only routes |
| `TestNonAdminRoutesServeAClientToken` | pure | no other route is gated |
| `TestCredentialMintingRoutesAreAdminRoutes` | pure | the routes that mint a network credential or a way to sign in are admin routes, and the client-minting routes are Own client |
| `router/client_credential_test.go` (5 tests) | pure | the gate: 403 with the message for a verified client token; network token, API key, no credential, another scheme and a non-token pass; a forged signature is left to the handler; a conditional refusal sees the verified claims; the gated route keeps its identity, streaming policy and captures |
| `jwt/by_jwt_names_client_test.go` `TestByJwtNamesClientUnverified` | pure | the unverified client peek |
| `model/network_client_authz_test.go` (5 tests) | DB | auth-client: no top-level mint, no child of another client, own child on its own device; reissue only its own client and its child, and another client is untouched; remove-client only its own child and itself; set-name only its own device, from its child too; each refusal writes nothing; the network session still does all of it; `NetworkRefusesClientAdmin` follows the Embed plan |
| `api/route_authz_db_test.go` `TestRealClientTokenIsRefusedOnEveryNetworkRoute` | DB | a real client token minted by `POST /network/auth-client` with the root token gets 403 on all 37 Network only routes, with requests aimed at another client and the account; the network and admin-user rows in 26 tables are unchanged; the app route `GET /network/clients` still serves it on an ordinary network |
| `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | DB | on an Embed network, a client token minted with the backend's **API key** gets 403 on all 64 admin routes, and nothing changes |
| `TestRealClientTokenActsOnlyForItsOwnClients` | DB | through the router: top-level mint, child of another client, another client's reissue, another client's removal and another device's rename are refused and change nothing; it creates, reissues and removes its own child; its token refreshes |
| `TestNetworkCredentialAdministersTheNetwork` | DB | the root token and an API key reissue another client, set the ACL group and data cap, create auth codes, set preferences, list clients and API keys, bulk-remove a client, create an API key and revoke the old one. Each effect is checked in the database |

Test output (pure, on the branch):

```
ok   github.com/urnetwork/server/router   (TestRefuseClientCredentialsGate, ...LeavesAnUnverifiedTokenToTheHandler, ...AsksTheRefusal, TestRouteWithoutTheGateServesAClientToken, TestRefuseClientCredentialsKeepsTheRoute: PASS; whole pure package: ok)
ok   github.com/urnetwork/server/api      (the 8 route_authz_test.go tests: PASS)
ok   github.com/urnetwork/server/jwt      (TestByJwtNamesClientUnverified: PASS)
```

Pre-existing failures, not from this branch:
- `api` `TestSpecRoutesImplemented` fails because the shared connect checkout
  is at `97abb1e6` and documents `GET /network/embed` from `feat/embed-flag`.
- `model` `TestContractLifecycleWritesUsePrimaryDatabaseClockAfterLocks` fails
  the same way on main. This branch adds no deactivation writer (the count
  stays 2).

The DB tests are compiled (`go vet ./model ./api`). Run them after merge:
`go test -run 'TestAuthNetworkClientClientToken|TestRemoveNetworkClientClientToken|TestDeviceSetNameClientToken|TestNetworkRefusesClientAdmin' ./model`
and `go test -run 'TestRealClientToken|TestNetworkCredentialAdministers' ./api`.

### File to tests

| File | Change | Tests |
|---|---|---|
| `router/client_credential.go` (new), `router/router.go` (one field) | the gate, `RefuseClientCredentials`, `Method`/`Pattern`/`RefusesClientCredentials`, `Testing_WithHandler` | `router/client_credential_test.go`; through the API table in `api/route_authz_test.go` |
| `jwt/by_jwt.go` | `ByJwtNamesClientUnverified` | `TestByJwtNamesClientUnverified`; the gate tests |
| `api/route_authz.go` (new), `api/api.go` (one line) | classification table, `applyRouteAccess`, the Embed refusal | `api/route_authz_test.go` (8), `api/route_authz_db_test.go` (4) |
| `model/network_client_admin_model.go` (new) | `NetworkRefusesClientAdmin` | `TestNetworkRefusesClientAdminFollowsTheEmbedPlan`; `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` |
| `model/network_client_model.go` | auth-client own-client limits (new and reissue), `deactivateOwnedNetworkClientInTx`, remove-client and set-name own-client limits | `model/network_client_authz_test.go` (4); `TestRealClientTokenActsOnlyForItsOwnClients`; `TestNetworkCredentialAdministersTheNetwork` |

## 7. Route table

Columns: the route; the handler and the implementation it wraps; the auth
wrapper; what a client token got **before** this branch; the class; the
shipped client that calls it with a client token (call sites); the verdict.

| Route | Handler | Wrapper | Client token before | Class | Shipped client-token dependency | Verdict |
|---|---|---|---|---|---|---|
| `POST /provider-work/v1/owners` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `GET /provider-work/v1/owners` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `GET /provider-work/v1/requests` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `POST /provider-work/v1/requests` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `GET /provider-work/v1/requests/([^/]+)` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `POST /provider-work/v1/cuts` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `GET /provider-work/v1/cuts/([^/]+)` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `POST /provider-work/v1/authorities` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `GET /provider-work/v1/windows` | `providerWork.ServeHTTP` | custom (signed objects) | — (no by_jwt) | Public | — | unchanged |
| `GET /privacy.txt` | `router.Txt` | router.Txt | — (no by_jwt) | Public | — | unchanged |
| `GET /terms.txt` | `router.Txt` | router.Txt | — (no by_jwt) | Public | — | unchanged |
| `GET /vdp.txt` | `router.Txt` | router.Txt | — (no by_jwt) | Public | — | unchanged |
| `GET /status` | `router.WarpStatus` | router.WarpStatus | — (no by_jwt) | Public | — | unchanged |
| `GET /clock` | `handlers.Clock` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /competition/healthz` | `handlers.CompetitionHealth` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /competition/readyz` | `handlers.CompetitionReady` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /competition/info` | `handlers.CompetitionInfo` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /competition/leaderboard` | `handlers.CompetitionLeaderboard` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /competition/round/([^/]+)/providers.yml` | `handlers.CompetitionGetRoundWorkload` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /competition/generate-staging-round` | `handlers.CompetitionGenerateStagingRound` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /competition/close-staging-round` | `handlers.CompetitionCloseStagingRound` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /competition/generate-round` | `handlers.CompetitionGenerateRound` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /competition/score` | `handlers.CompetitionSubmitScore` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /competition/score/([^/]+)` | `handlers.CompetitionGetScore` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /stats/last-90` | `handlers.StatsLast90` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /stats/providers-map` | `handlers.StatsProvidersMap` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /stats/providers` | `handlers.StatsProviders` | WrapRequireAuth | yes | Network only | none (ur.io/web use the root token) | **FIXED** at the router (403) |
| `POST /stats/providers-last-n` | `handlers.StatsProvidersLastN` | WrapWithInputRequireAuth | yes | Network only | none (ur.io/web use the root token) | **FIXED** at the router (403) |
| `POST /stats/provider-last-n` | `handlers.StatsProvider` | WrapWithInputRequireAuth | yes | Network only | none (ur.io/web use the root token) | **FIXED** at the router (403) |
| `POST /stats/providers-overview-last-n` | `handlers.StatsProvidersOverview` | WrapWithInputRequireAuth | yes | Network only | none (ur.io/web use the root token) | **FIXED** at the router (403) |
| `GET /stats/providers-overview-last-90` | `handlers.StatsProvidersOverviewLast90` | WrapRequireAuth | yes | Network only | none (ur.io/web use the root token) | **FIXED** at the router (403) |
| `POST /stats/provider-last-90` | `handlers.StatsProviderLast90` | WrapWithInputRequireAuth | yes | Network only | none (ur.io/web use the root token) | **FIXED** at the router (403) |
| `POST /stats/leaderboard` | `handlers.GetLeaderboard` → `controller.GetLeaderboard` | WrapWithInputRequireAuth | yes | Client | apps GetLeaderboard | unchanged |
| `POST /stats/points-leaderboard` | `handlers.GetPointsLeaderboard` → `controller.GetPointsLeaderboard` | WrapWithInputOptionalAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/login` | `handlers.AuthLogin` → `controller.AuthLogin` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/wallet-nonce` | `handlers.AuthWalletNonce` → `model.AuthWalletNonceCreate` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/login-with-password` | `handlers.AuthLoginWithPassword` → `controller.AuthLoginWithPassword` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/verify` | `handlers.AuthVerify` → `controller.AuthVerify` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/wallet-challenge` | `handlers.AuthWalletChallenge` → `controller.AuthWalletChallenge` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /auth/refresh` | `handlers.AuthRefreshToken` → `controller.AuthRefreshToken` | WrapWithInputNoAuth | required | Client | — | unchanged |
| `POST /auth/verify-send` | `handlers.AuthVerifySend` → `controller.AuthVerifySend` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/password-reset` | `handlers.AuthPasswordReset` → `controller.AuthPasswordReset` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/password-set` | `handlers.AuthPasswordSet` → `controller.AuthPasswordSet` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/network-check` | `handlers.NetworkCheck` → `model.NetworkCheck` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/network-create` | `handlers.NetworkCreate` → `controller.NetworkCreate` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/network-delete` | `handlers.RemoveNetwork` → `controller.NetworkRemove` | WrapRequireAuth | yes | App admin | apple UrApiService.swift:1250, android SettingsViewModel.kt:303 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /auth/code-create` | `handlers.AuthCodeCreate` → `model.AuthCodeCreate` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:882, android SettingsViewModel.kt:499 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /auth/code-login` | `handlers.AuthCodeLogin` → `model.AuthCodeLogin` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/apple/callback` | `handlers.AuthAppleOAuthCallback` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /auth/apple/callback` | `handlers.AuthAppleOAuthCallback` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /auth/google/callback` | `handlers.AuthGoogleOAuthCallback` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /auth/add-auth` | `handlers.AuthAdd` → `controller.AddAuth` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:702, android SettingsViewModel.kt:343 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /auth/remove-auth` | `handlers.AuthRemove` → `controller.RemoveAuth` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:775, android SettingsViewModel.kt:377 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /auth/regenerate-seedphrase` | `handlers.AuthRegenerateSeedphrase` → `controller.RegenerateSeedphrase` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:668, android SettingsViewModel.kt:434 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /auth/generate-seedphrase` | `handlers.AuthGenerateSeedphrase` → `controller.GenerateSeedphrase` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:639, android SettingsViewModel.kt:410 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /network/auth-client` | `handlers.AuthNetworkClient` → `controller.AuthNetworkClient` | WrapWithInputRequireAuth | yes: top-level mint, child of ANY client, reissue of ANY client | Own client | connect ip_remote_multi_client_api.go:812 (own children); apps/sn mint top-level with the ROOT token | **FIXED** (model): own client and its children only |
| `POST /network/register-client-v1` | `handlers.RegisterNetworkClient` → `controller.RegisterNetworkClient` | WrapWithInputRequireAuth | model refuses | Network only | sdk api_client_registration.go (root token) | **FIXED** at the router (403); the model already refused |
| `POST /network/remove-client` | `handlers.RemoveNetworkClient` → `model.RemoveNetworkClient` | WrapWithInputRequireAuth | yes: ANY client | Own client | connect ip_remote_multi_client_api.go:969, sn validator/transport_client.go:240 (own children) | **FIXED** (model): own client and its children only |
| `POST /network/remove-clients` | `handlers.RemoveNetworkClients` → `model.RemoveNetworkClients` | WrapWithInputRequireAuth | yes | Network only | none (web manager uses the root token) | **FIXED** at the router (403) |
| `POST /network/extender-activate` | `handlers.ExtenderActivate` → `controller.ExtenderActivate` | WrapWithInputRequireClient | required | Client | — | unchanged |
| `GET /network/extender-hint` | `handlers.ExtenderHint` → `controller.ExtenderHint` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /network/extender-latency` | `handlers.ExtenderLatencyReport` → `controller.ExtenderLatencyReport` | WrapWithInputRequireClient | required | Client | — | unchanged |
| `POST /network/extender-release` | `handlers.ExtenderRelease` → `controller.ExtenderRelease` | WrapWithInputRequireClient | required | Client | — | unchanged |
| `POST /network/extender-block-report` | `handlers.ExtenderBlockReport` → `controller.ExtenderBlockReport` | WrapWithInputRequireClient | required | Client | — | unchanged |
| `POST /network/ping-report` | `handlers.ExtenderPingReport` → `controller.ExtenderPingReport` | WrapWithInputRequireClient | required | Client | — | unchanged |
| `POST /network/provider-egress-location` | `handlers.ProviderEgressLocationSubmit` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /network/provider-egress-due` | `handlers.ProviderEgressLocationDue` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /network/provider-blackhole-due` | `handlers.ProviderBlackholeCheckDue` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /network/provider-blackhole-checks` | `handlers.SubmitProviderBlackholeChecks` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /network/provider-egress-attempt` | `handlers.ProviderEgressLocationAttempt` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /network/provider-egress-destinations` | `handlers.ProviderEgressDestinations` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /network/prober-credential` | `handlers.ProberCredential` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /network/geolocation-source-pins` | `handlers.GeolocationSourcePins` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /network/provider-bandwidth-test` | `handlers.ProviderBandwidthTest` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /network/provider-bandwidth-result` | `handlers.ProviderBandwidthResult` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /network/provider-bandwidth-reserve` | `handlers.ProviderBandwidthReserve` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /network/provider-egress-health` | `handlers.ProviderEgressHealthResult` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /network/provider-verdict` | `handlers.ProviderClientVerdictSubmit` → `model.SubmitProviderClientVerdict` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `GET /network/clients` | `handlers.NetworkClients` → `model.GetNetworkClients` | WrapRequireAuth | yes | App admin | apple DeviceManager.swift:1697, android SettingsViewModel.kt:135, NetworkPeersViewModel.kt:190 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /network/proxies` | `handlers.NetworkProxies` → `model.GetNetworkProxies` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `GET /network/peers` | `handlers.NetworkPeers` → `model.GetNetworkPeersForSession` | WrapRequireAuth | yes | Client | — | unchanged |
| `GET /network/provider-locations` | `handlers.NetworkGetProviderLocations` → `model.GetProviderLocations` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /network/find-provider-locations` | `handlers.NetworkFindProviderLocations` → `model.FindProviderLocations` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /network/find-providers2` | `findProviders2` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /network/user` | `handlers.GetNetworkUser` → `controller.GetNetworkUser` | WrapRequireAuth | yes | App admin | apple NetworkUserViewModel.swift:68, android AccountViewModel.kt (NetworkUserViewController) | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /network/provider-status` | `handlers.GetProviderStatus` → `controller.GetProviderStatus` | WrapRequireAuth | yes | Client | — | unchanged |
| `POST /network/user/update` | `handlers.UpdateNetworkName` → `controller.UpdateNetworkName` | WrapWithInputRequireAuth | yes | Network only | none (sdk NetworkUserViewController.UpdateNetworkUser has no app caller) | **FIXED** at the router (403) |
| `GET /network/ranking` | `handlers.GetLeaderboardNetworkRanking` → `controller.GetNetworkLeaderboardRanking` | WrapRequireAuth | yes | Client | apps | unchanged |
| `POST /network/ranking-visibility` | `handlers.SetNetworkLeaderboardPublic` → `controller.SetNetworkLeaderboardRankingPublic` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:158, android LeaderboardViewModel.kt:246 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /network/points-ranking-visibility` | `handlers.SetNetworkPointsLeaderboardPublic` → `controller.SetNetworkPointsLeaderboardPublic` | WrapWithInputRequireAuth | yes | App admin | apple PointsLeaderboardStore.swift:390, android PointsLeaderboardViewModel.kt:377 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /network/emoji` | `handlers.SetNetworkEmojiTag` → `controller.SetNetworkEmojiTag` | WrapWithInputRequireAuth | yes | App admin | apple PointsLeaderboardStore.swift:423, android PointsLeaderboardViewModel.kt:400 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /network/block-location` | `handlers.NetworkBlockLocation` → `controller.NetworkBlockLocation` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:1161, android BlockedRegionsViewModel.kt:54 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /network/unblock-location` | `handlers.NetworkUnblockLocation` → `controller.NetworkUnblockLocation` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:1191, android BlockedRegionsViewModel.kt:68 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /network/blocked-locations` | `handlers.GetNetworkBlockedLocations` → `controller.GetNetworkBlockedLocations` | WrapRequireAuth | yes | Client | apps | unchanged |
| `GET /network/reliability` | `handlers.GetNetworkReliability` | WrapRequireAuth | yes | Client | apps | unchanged |
| `POST /network/client-data-cap` | `handlers.NetworkClientDataCapSet` → `model.SetClientDataCap` | WrapWithInputRequireAuth | model refuses | Network only | none (examples backends use the root credential) | **FIXED** at the router (403); the model already refused |
| `GET /network/client-data-cap` | `handlers.NetworkClientDataCapGet` | WrapRequireAuth | own cap only (model) | Own client | examples go/embed/caps.go:90 (own cap) | unchanged (model already scopes to its own client) |
| `GET /network/client-data-caps` | `handlers.NetworkClientDataCapsList` | WrapRequireAuth | model refuses | Network only | none | **FIXED** at the router (403); the model already refused |
| `POST /network/client-acl-group` | `handlers.NetworkClientAclGroupSet` → `model.SetNetworkClientAclGroup` | WrapWithInputRequireAuth | model refuses | Network only | none | **FIXED** at the router (403); the model already refused |
| `GET /network/client-acl-group` | `handlers.NetworkClientAclGroupGet` | WrapRequireAuth | own group only (model) | Own client | — | unchanged (model already scopes to its own client) |
| `POST /services/contact-sales` | `handlers.ServicesContactSales` → `model.ServicesContactSales` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /preferences/set-preferences` | `handlers.AccountPreferencesSet` → `controller.AccountPreferencesSet` | WrapWithInputRequireAuth | yes | App admin | apple AccountPreferencesViewModel.swift:144 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /preferences` | `handlers.AccountPreferencesGet` → `controller.AccountPreferencesGet` | WrapRequireAuth | yes | Client | apple | unchanged |
| `POST /feedback/send-feedback` | `handlers.FeedbackSend` → `model.FeedbackSend` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /pay/stripe` | `handlers.StripeWebhook` → `controller.VerifyStripeBody` | WrapWithInputBodyFormatterNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/coinbase` | `handlers.CoinbaseWebhook` → `controller.VerifyCoinbaseBody` | WrapWithInputBodyFormatterNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/circle` | `handlers.CircleWebhook` → `controller.VerifyCircleBody` | WrapWithInputBodyFormatterNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/play` | `handlers.PlayWebhook` → `controller.VerifyPlayBody` | WrapWithInputBodyFormatterNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/solana` | `handlers.HeliusWebhook` → `controller.VerifyHeliusBody` | WrapWithInputBodyFormatterNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /solana/payment-intent` | `handlers.CreateSolanaPaymentIntent` → `controller.CreateSolanaPaymentIntent` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /solana/payment-transaction` | `handlers.CreateSolanaPaymentTransaction` → `controller.CreateSolanaPaymentTransaction` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /stripe/payment-intent` | `handlers.CreateStripePaymentIntent` → `controller.StripeCreatePaymentIntent` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /stripe/customer-portal` | `handlers.StripeCreateCustomerPortal` → `controller.StripeCreateCustomerPortal` | WrapWithInputRequireAuth | yes | App admin | apple StripeBillingClient.swift:143, android SettingsViewModel.kt:532 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /stripe/create-checkout-session` | `handlers.StripeCreateCheckoutSession` → `controller.StripeCreateCheckoutSession` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /pay/data/checkout` | `handlers.PayDataCheckout` → `controller.PayDataCheckout` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/data/network-lookup` | `handlers.PayDataNetworkLookup` → `controller.PayDataNetworkLookup` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/data/solana-intent` | `handlers.PayDataSolanaIntent` → `controller.PayDataSolanaIntent` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /pay/data/solana-status` | `handlers.PayDataSolanaStatus` → `controller.PayDataSolanaStatus` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /wallet/balance` | `handlers.WalletBalance` → `controller.WalletBalance` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /wallet/validate-address` | `handlers.WalletValidateAddress` → `controller.WalletValidateAddress` | WrapWithInputRequireAuth | yes | Client | apps | unchanged |
| `POST /wallet/circle-init` | `handlers.WalletCircleInit` → `controller.WalletCircleInit` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /wallet/circle-transfer-out` | `handlers.WalletCircleTransferOut` → `controller.WalletCircleTransferOut` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `GET /subscription/balance` | `handlers.SubscriptionBalance` | WrapRequireAuth | yes | Client | apps, widgets | unchanged |
| `POST /test/balance-drain` | `handlers.TestBalanceDrain` → `controller.TestBalanceDrain` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /test/balance-restore` | `handlers.TestBalanceRestore` → `controller.TestBalanceRestore` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /onboarding/offer/issue` | `handlers.OnboardingOfferIssue` → `controller.OnboardingOfferIssue` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /onboarding/click` | `handlers.OnboardingClick` → `controller.OnboardingClick` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /onboarding/feedback/([^/]+)` | `handlers.OnboardingFeedbackToken` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /client/events` | `handlers.ClientEventsSend` → `controller.ClientEventsSend` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `GET /admin/onboarding/results` | `handlers.AdminOnboardingResults` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /admin/onboarding/email-tracker` | `handlers.AdminOnboardingEmailTracker` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /admin/onboarding/experiments` | `handlers.AdminOnboardingExperiments` → `controller.AdminOnboardingExperiments` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /subscription/stripe/payment-sheet` | `handlers.StripePaymentSheet` → `controller.StripePaymentSheet` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `GET /subscription/stripe/prices` | `handlers.StripePrices` | WrapRequireAuth | yes | Client | — | unchanged |
| `GET /subscription/details` | `handlers.SubscriptionDetails` → `controller.SubscriptionDetails` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /subscription/cancel` | `handlers.SubscriptionCancel` → `controller.SubscriptionCancel` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /subscription/resume` | `handlers.SubscriptionResume` → `controller.SubscriptionResume` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /subscription/check-balance-code` | `handlers.SubscriptionCheckBalanceCode` → `model.CheckBalanceCode` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /subscription/redeem-balance-code` | `handlers.SubscriptionRedeemBalanceCode` → `controller.RedeemBalanceCode` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /subscription/create-payment-id` | `handlers.SubscriptionCreatePaymentId` → `model.SubscriptionCreatePaymentId` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /subscription/verify-play-purchase` | `handlers.SubscriptionVerifyPlayPurchase` → `controller.VerifyPlayPurchase` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `POST /subscription/verify-apple-transaction` | `handlers.SubscriptionVerifyAppleTransaction` → `verifyAppleTransaction` | WrapWithInputRequireAuth | yes | Client | — | unchanged |
| `GET /x402/skus` | `handlers.X402Skus` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /x402/purchase` | `handlers.X402Purchase` | custom | yes | Client | — | unchanged |
| `POST /device/add` | `handlers.DeviceAdd` → `model.DeviceAdd` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/create-share-code` | `handlers.DeviceCreateShareCode` → `model.DeviceCreateShareCode` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `GET /device/share-code/([^/]+)/qr.png` | `handlers.DeviceShareCodeQR` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/share-status` | `handlers.DeviceShareStatus` → `model.DeviceShareStatus` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/confirm-share` | `handlers.DeviceConfirmShare` → `model.DeviceConfirmShare` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/create-adopt-code` | `handlers.DeviceCreateAdoptCode` → `model.DeviceCreateAdoptCode` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /device/adopt-code/([^/]+)/qr.png` | `handlers.DeviceAdoptCodeQR` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /device/adopt-status` | `handlers.DeviceAdoptStatus` → `model.DeviceAdoptStatus` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /device/confirm-adopt` | `handlers.DeviceConfirmAdopt` → `model.DeviceConfirmAdopt` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /device/remove-adopt-code` | `handlers.DeviceRemoveAdoptCode` → `model.DeviceRemoveAdoptCode` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /device/associations` | `handlers.DeviceAssociations` → `model.DeviceAssociations` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/remove-association` | `handlers.DeviceRemoveAssociation` → `model.DeviceRemoveAssociation` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/set-association-name` | `handlers.DeviceSetAssociationName` → `model.DeviceSetAssociationName` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /device/set-name` | `handlers.DeviceSetName` → `model.DeviceSetName` | WrapWithInputRequireAuth | yes: ANY device | Own client | apple SettingsViewModel.swift:141, android SettingsViewModel.kt:163 (own device only) | **FIXED** (model): own client and its children only |
| `POST /connect/control` | `connectControl` | WrapWithInputRequireClient | required | Client | — | unchanged |
| `GET /key/([^/]+)` | `handlers.GetClientKey` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /key/([^/]+)/history` | `handlers.GetClientKeyHistory` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/client-key/observation` | `handlers.SnClientKeyObservation` | WrapRequireClient | required | Client | — | unchanged |
| `POST /sn/client-key/observations` | `handlers.SnClientKeyObservations` | WrapRequireClient | required | Client | — | unchanged |
| `POST /verify` | `handlers.Verify` → `controller.Verify` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /verify/keys` | `handlers.GetVerifyKeys` → `controller.GetVerifyKeys` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /verify/stats` | `handlers.GetVerifyStats` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /verify/proofs` | `handlers.GetVerifyProofs` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /verify/original` | `handlers.GetVerifyOriginalRequest` → `controller.GetVerifyOriginalRequest` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /verify/original/close` | `verifyRequestClosure.ServeHTTP` | custom (signed) | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/wallet` | `handlers.SnSetWallet` → `controller.SnSetWallet` | WrapWithInputRequireAuth | own client only (model) | Own client | sdk sn_wallet.go:308 (own client id) | unchanged (model already scopes to its own client) |
| `POST /sn/wallet/consent` | `handlers.SnWalletMappingChallenge` → `controller.SnWalletMappingChallenge` | WrapWithInputRequireAuth | own client only (model) | Own client | — | unchanged (model already scopes to its own client) |
| `POST /sn/wallet/consent/history` | `handlers.SnWalletMappingHistory` → `controller.SnWalletMappingHistory` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/wallet/network-consent` | `handlers.SnNetworkWalletMappingChallenge` → `controller.SnNetworkWalletMappingChallenge` | WrapWithInputRequireAuth | model refuses | Network only | none (sn hotkeywallet uses the root token) | **FIXED** at the router (403); the model already refused |
| `POST /sn/wallet/network-consent/history` | `handlers.SnNetworkWalletMappingHistory` → `controller.SnNetworkWalletMappingHistory` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/wallet/hotkey-consent` | `handlers.SnHotkeyWalletMappingConsent` → `controller.SnHotkeyWalletMappingConsent` | WrapWithInputRequireAuth | model refuses | Network only | none (sn miner uses the operator root token) | **FIXED** at the router (403); the model already refused |
| `POST /sn/wallet/hotkey-consent/history` | `handlers.SnHotkeyWalletMappingHistory` → `controller.SnHotkeyWalletMappingHistory` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/wallet/hotkey-delegation` | `handlers.SnHotkeyNetworkDelegationChallenge` → `controller.SnHotkeyNetworkDelegationChallenge` | WrapWithInputRequireAuth | model refuses | Network only | none | **FIXED** at the router (403); the model already refused |
| `POST /sn/wallet/hotkey-delegation/history` | `handlers.SnHotkeyNetworkDelegationHistory` → `controller.SnHotkeyNetworkDelegationHistory` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /sn/wallet` | `handlers.SnGetWallet` → `controller.SnGetWallet` | WrapRequireAuth | own client only (model) | Own client | — | unchanged (model already scopes to its own client) |
| `POST /sn/wallet/validate` | `handlers.SnValidateWallet` → `controller.SnValidateWallet` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /sn/head` | `handlers.SnHead` → `controller.SnHead` | WrapRequireAuth | yes | Client | apps | unchanged |
| `POST /sn/head/binding` | `handlers.SnHeadBinding` → `controller.SnHeadBinding` | WrapWithInputRequireAuth | own client only (model) | Own client | — | unchanged (model already scopes to its own client) |
| `GET /sn/pool/claim` | `handlers.SnPoolClaim` | WrapRequireAuth | required (model) | Client | — | unchanged |
| `GET /sn/epoch` | `handlers.SnEpoch` → `controller.SnEpoch` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /sn/artifact` | `handlers.SnArtifact` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /sn/attempt-artifact` | `handlers.SnAttemptArtifact` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/attempt-artifact` | `handlers.SnUploadAttemptArtifactWithReserved` | WrapRequireClient | required | Client | — | unchanged |
| `GET /sn/artifacts` | `handlers.SnArtifactHistory` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /sn/evidence` | `handlers.SnEvidence` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /sn/evidence` | `handlers.SnEvidence` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /sn/evidence/history` | `handlers.SnEvidenceHistory` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /hello` | `handlers.Hello` → `controller.Hello` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /account/api-key` | `handlers.CreateApiKey` → `model.CreateApiKey` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /account/api-key/remove` | `handlers.DeleteApiKey` → `controller.DeleteApiKey` | WrapWithInputRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `GET /account/api-keys` | `handlers.GetApiKeys` → `controller.GetApiKeys` | WrapRequireAuth | yes | Network only | none | **FIXED** at the router (403) |
| `POST /account/payout-wallet` | `handlers.SetPayoutWallet` → `controller.SetPayoutWallet` | WrapWithInputRequireAuth | yes | App admin | apple UsdcWalletsClient.swift:145, android SdkLegacyWalletSource.kt:156 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /account/payout-wallet` | `handlers.GetPayoutWallet` → `controller.GetPayoutWallet` | WrapRequireAuth | yes | App admin | apple UsdcWalletsClient.swift:114, android SdkLegacyWalletSource.kt:73 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /account/circle-wallet` | `handlers.CircleWebhook` → `controller.VerifyCircleBody` | WrapWithInputBodyFormatterNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /account/wallet` | `handlers.CreateAccountWallet` → `controller.CreateAccountWalletExternal` | WrapWithInputRequireAuth | yes | App admin | apple UsdcWalletsClient.swift:73, android SdkLegacyWalletSource.kt:137 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /account/wallets` | `handlers.GetAccountWallets` → `controller.GetAccountWallets` | WrapRequireAuth | yes | App admin | apple UsdcWalletsClient.swift:95, android SdkLegacyWalletSource.kt:43 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /account/wallets/remove` | `handlers.RemoveWallet` → `controller.RemoveWallet` | WrapWithInputRequireAuth | yes | App admin | apple UsdcWalletsClient.swift:165, android SdkLegacyWalletSource.kt:165 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /account/wallets/verify-seeker` | `handlers.VerifyHoldingSeekerToken` → `controller.VerifySeekerNftHolder` | WrapWithInputRequireAuth | yes | App admin | android EarningsViewModel.kt:667 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /account/payments` | `handlers.GetAccountPayments` → `controller.GetNetworkAccountPayments` | WrapRequireAuth | yes | App admin | apple UsdcWalletsClient.swift:192, android SdkLegacyWalletSource.kt:84 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /account/referral-code` | `handlers.GetNetworkReferralCode` → `controller.GetNetworkReferralCode` | WrapRequireAuth | yes | Client | apps | unchanged |
| `GET /account/referral-network` | `handlers.GetReferralNetwork` → `controller.GetReferralNetwork` | WrapRequireAuth | yes | Client | apps | unchanged |
| `GET /account/unlink-referral-network` | `handlers.UnlinkReferralNetwork` → `controller.UnlinkReferralNetwork` | WrapRequireAuth | yes | App admin | apple UrApiService.swift:1331, android UpdateReferralNetworkBottomSheetViewModel.kt:97 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /account/set-referral` | `handlers.SetNetworkReferral` → `controller.SetNetworkReferral` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:1306, android UpdateReferralNetworkBottomSheetViewModel.kt:54 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /account/change-name` | `handlers.ChangeNetworkName` → `controller.ChangeNetworkName` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:811, android ProfileViewModel.kt:111 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /account/claim-name` | `handlers.ClaimNetworkName` → `controller.ClaimNetworkName` | WrapWithInputRequireAuth | yes | App admin | apple UrApiService.swift:841, android ProfileViewModel.kt:148 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `GET /account/points` | `handlers.GetAccountPoints` → `controller.GetAccountPoints` | WrapRequireAuth | yes | Client | apps | unchanged |
| `GET /account/epochs` | `handlers.GetAccountEpochs` | WrapRequireAuth | yes | Client | apps | unchanged |
| `GET /account/balance-codes` | `handlers.GetNetworkRedeemedBalanceCodes` → `controller.GetNetworkRedeemedBalanceCodes` | WrapRequireAuth | yes | App admin | apple UrApiService.swift:1127, android BalanceCodesViewModel.kt:43 | **FIXED** for Embed networks (403); ordinary networks: decision 1 |
| `POST /referral-code/validate` | `handlers.ValidateReferralCode` → `controller.ValidateReferralCode` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `GET /transfer/stats` | `handlers.TransferStats` | WrapRequireAuth | yes | Client | sdk wallet_view_controller.go:781 | unchanged |
| `GET /connect` | `handlers.AuthConnect` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /connect` | `handlers.AuthConnect` | custom | — (no by_jwt) | Public | — | unchanged |
| `POST /apple/notification` | `handlers.AppleNotification` | custom | — (no by_jwt) | Public | — | unchanged |
| `GET /my-ip-info` | `handlers.MyIPInfo` → `controller.GetMyIpInfo` | WrapNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /updates/brevo` | `handlers.BrevoWebhook` → `controller.BrevoWebhook` | WrapWithInputNoAuth | — (no by_jwt) | Public | — | unchanged |
| `POST /log/([^/]+)/upload` | `handlers.LogUpload` | WrapRequireAuth | yes | Client | — | unchanged |
| `GET /\.well-known/oauth-authorization-server` | `oauth.ServerMetadataHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `GET /\.well-known/openid-configuration` | `oauth.ServerMetadataHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `GET /\.well-known/jwks\.json` | `oauth.JwksHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `POST /oauth/token` | `oauth.TokenHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `POST /oauth/register` | `oauth.RegisterHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `POST /oauth/revoke` | `oauth.RevokeHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `GET /oauth/userinfo` | `oauth.UserinfoHandler` | custom (OAuth) | — (no by_jwt) | Public | — | unchanged |
| `POST /oauth/authorize` | `oauth.AuthorizeHandler` | custom (OAuth) | yes | Network only | none (ur.io consent page uses the root token) | **FIXED** at the router (403) |
| `POST /oauth/consent` | `oauth.ConsentHandler` | custom (OAuth) | yes | Network only | none (ur.io) | **FIXED** at the router (403) |

## 8. Coverage: admin and own-client routes to tests

Every Network only and App admin route is covered by name: the pure tests
iterate the classification, and the DB tests require a request for every
admin route (`routeAccessAdminRequests` fails when a classified admin route has
none).

| Route | Class | Refusal (pure, model) | Refusal with a real client token (DB) | Network credential |
|---|---|---|---|---|
| `GET /stats/providers` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /stats/providers-last-n` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /stats/provider-last-n` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /stats/providers-overview-last-n` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /stats/providers-overview-last-90` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /stats/provider-last-90` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /auth/network-delete` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /auth/code-create` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `POST /auth/add-auth` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /auth/remove-auth` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /auth/regenerate-seedphrase` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /auth/generate-seedphrase` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/auth-client` | Own client | `TestAuthNetworkClientClientTokenCreatesOnlyItsOwnChildren`, `TestAuthNetworkClientClientTokenReissuesOnlyItsOwnClients` | `TestRealClientTokenActsOnlyForItsOwnClients` | `TestNetworkCredentialAdministersTheNetwork` (reissue of another client) |
| `POST /network/register-client-v1` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/remove-client` | Own client | `TestRemoveNetworkClientClientTokenRemovesOnlyItsOwnClients` | `TestRealClientTokenActsOnlyForItsOwnClients` | `TestRemoveNetworkClientClientTokenRemovesOnlyItsOwnClients` (network session) |
| `POST /network/remove-clients` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `GET /network/clients` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `GET /network/proxies` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /network/user` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/user/update` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/ranking-visibility` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/points-ranking-visibility` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/emoji` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/block-location` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/unblock-location` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/client-data-cap` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `GET /network/client-data-cap` | Own client | existing model tests (own-client scoping predates this branch) | — | — |
| `GET /network/client-data-caps` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /network/client-acl-group` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `GET /network/client-acl-group` | Own client | existing model tests (own-client scoping predates this branch) | — | — |
| `POST /preferences/set-preferences` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `POST /stripe/customer-portal` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /wallet/balance` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /wallet/circle-init` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /wallet/circle-transfer-out` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /test/balance-drain` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /test/balance-restore` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /subscription/details` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /subscription/cancel` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /subscription/resume` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/add` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/create-share-code` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /device/share-code/([^/]+)/qr.png` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/share-status` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/confirm-share` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /device/associations` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/remove-association` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/set-association-name` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /device/set-name` | Own client | `TestDeviceSetNameClientTokenRenamesOnlyItsOwnDevice` | `TestRealClientTokenActsOnlyForItsOwnClients` | `TestDeviceSetNameClientTokenRenamesOnlyItsOwnDevice` (network session) |
| `POST /sn/wallet` | Own client | existing model tests (own-client scoping predates this branch) | — | — |
| `POST /sn/wallet/consent` | Own client | existing model tests (own-client scoping predates this branch) | — | — |
| `POST /sn/wallet/network-consent` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /sn/wallet/hotkey-consent` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /sn/wallet/hotkey-delegation` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /sn/wallet` | Own client | existing model tests (own-client scoping predates this branch) | — | — |
| `POST /sn/head/binding` | Own client | existing model tests (own-client scoping predates this branch) | — | — |
| `POST /account/api-key` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `POST /account/api-key/remove` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `GET /account/api-keys` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential`, `TestNetworkCredentialAdministersTheNetwork` (root token + API key, state checked) |
| `POST /account/payout-wallet` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /account/payout-wallet` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /account/wallet` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /account/wallets` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /account/wallets/remove` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /account/wallets/verify-seeker` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /account/payments` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /account/unlink-referral-network` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /account/set-referral` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /account/change-name` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /account/claim-name` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `GET /account/balance-codes` | App admin | `TestAdminRoutesRefuseAClientTokenThroughTheRouter` (Embed); `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` (ordinary: served) | `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /oauth/authorize` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
| `POST /oauth/consent` | Network only | `TestAdminRoutesRefuseAClientTokenThroughTheRouter`, `TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork` | `TestRealClientTokenIsRefusedOnEveryNetworkRoute`, `TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute` | `TestAdminRoutesServeTheNetworkCredential` |
