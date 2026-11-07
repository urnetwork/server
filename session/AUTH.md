# Authentication and authorization

How the server authenticates a request or a connection, and what each credential may do. This is the reference. The background lives in these documents:

- [AUTHZ1.md](../AUTHZ1.md): the route audit, its fixes and the full route table.
- [sdk/AUTHZ-CLIENT.md](../../sdk/AUTHZ-CLIENT.md): which credential the SDK sends on each call.
- [REVOKE-OPERATIONS.md](REVOKE-OPERATIONS.md): tracked sessions and revocation.
- [EMBED1.md](../EMBED1.md): the Embed plan.

State as of 2026-11-06. What the sidecar's P0 auth fix adds (DESIGN.md §7.4) is built log-only and marked below; §8 says what turns it on.

## Terms

- **Network credential (admin).** The network's sign-in token, which is a `by_jwt` that names no client, or an API key. It administers the network and the account.
- **Client token.** The `by_jwt` of one client. It names `client_id` and `device_id`, and it is what an installation runs on. It acts for its own client. On an ordinary network it can still reach the app-admin routes until the hard gate lands (§4).
- **Embed network.** A network the team ever enabled Embed for, or one that has the Embed plan's client allowance. Its client tokens are in the hands of a third party's users, so none of them administers anything.
- **Marked client.** A client minted with `admin_refused` or `strict_exp` (§4). A sidecar's client has both. The marks are rows of `network_client_owner`, set at mint, immutable, and inherited by children.
- **Sidecar token.** The client token of a `strict_exp` client: the `strict_exp` claim, a 7-day `exp`, a route allowlist and a proof of its ClientKey (§4). Only sidecars hold one.

## 1. Credentials

| Credential | What it is | Where it comes from | Lifetime | Renewal | What ends it |
|---|---|---|---|---|---|
| Network token | JWT with no `client_id` | The sign-in routes (all Public), listed below | 30 days | `POST /auth/network-refresh` | Password reset; session revocation (tagged tokens); expiry, once `reject_expired` is on |
| Client token | JWT with `client_id`, `device_id`, `root_client_id`, and `session_id` when sessions are on | `POST /network/auth-client`; `POST /network/register-client-v1` | 30 days; 7 for a sidecar token | `GET /auth/refresh`; a sidecar token also proves its ClientKey | Removing or deactivating the client; for untagged tokens, any inactive ancestor; password reset; session revocation |
| API key | `urn_…`, 56 characters, stored as its SHA-256 | `POST /account/api-key` | Never expires | None: both refresh routes refuse it | `POST /account/api-key/remove`. The key is looked up on every request, so removal takes effect on the next one |
| Auth code | A sign-in code | `POST /auth/code-create` (at most 24 h and 100 uses) | ≤ 24 h | None | Its expiry or uses; removing the API key it descends from |
| MCP OAuth identity | An unsigned request identity (`UnsignedIdentity`, `mcp/auth.go`) | `POST /oauth/authorize`, `POST /oauth/consent` | The OAuth access token's `expires_in` | The OAuth refresh grant (`oauth/grant.go`); both refresh routes refuse it | Expiry of its OAuth tokens |
| Lag guest token | A client token with the `lag_guest` claim: the one client of a lag.dev guest network | `POST /lag/session` (Public), limited per /24 (/48) of the caller | 30 days as a token; the guest network is removed 24 h after creation | None: lag.dev starts a new guest session | The lag sweep removing the guest network at its expiry |

**The sign-in routes** are `POST /auth/login` (SSO and user auth), `/auth/login-with-password`, `/auth/verify`, `/auth/network-create` and `/auth/code-login`.

**Notes on each credential:**

- **API keys.** An API key authenticates as its network. The request gets a network identity built for that request (`ApiKeyAuthenticated`, Pro false), and it never has a token to refresh.
- **Auth codes.** `/auth/code-login` returns a network token whose `CreateTime` is that of the root token that created the code. When session creation is on, a redemption starts an independent session. A code created by an API key, or by a credential that descends from one, carries the key's id in `auth_code.api_key_id`, and the token it mints carries it as the `api_key_id` claim. That token and everything derived from it (its renewal, a client it mints, a code it creates) end when the key is removed. A client token an API key mints directly carries no key: an embed backend's installations survive a rotation of the backend's key.
- **SSO.** Google and Apple id tokens are inputs to `/auth/login`, not credentials. Their audience must be one of our OAuth client ids: the `client_id` key of vault `google.yml` and `apple.yml`. An empty value skips the audience check (`session/sso_jwt.go`). The OAuth callbacks (`/auth/apple/callback`, `/auth/google/callback`) only hand the provider's answer back to the app; they mint nothing.
- **Internal credentials.** Hosted proxy devices run on hosted session credentials (`MintHostedSession`) and never hold a network credential. Internal probes (`SignInternalProbe`) carry no session and their real client root, with the normal lifetime.
- **Lag guests** ([lag.dev](../../mmm/lag.dev/PLAN.md) A1). A browser on lag.dev gets a guest network marked to expire after 24 h, with one client, and that client's token. The token carries `lag_guest`, which every credential derived from it and every renewal keeps. It works on the two routes the browser needs, `GET /lag/extenders` and `POST /lag/track`, and nowhere else:
  - every other route answers it 403, "A lag.dev guest token works only on the lag.dev routes.", before its handler runs, whatever its class (§4);
  - the connect transport refuses it as a refused credential, on h1 and h3 alike, so a guest is never a VPN client;
  - the guest network gets no automatic data grant: no daily free data, no initial balance and no referral data.

  So a guest administers nothing, buys nothing and connects nowhere. The lag sweep removes the network at its expiry, which ends the token while `reject_expired` is off (§8). Only networks marked as lag guests expire this way.

**No cookies.** Every credential above is sent by the caller in the `Authorization` header. No server reads a cookie, sets one or keeps state in one. This is enforced in code, not left to convention (`session/cookies.go`):

- The router drops `Cookie` and the obsolete `Cookie2` from every request before any route runs. `NewClientSessionFromRequest` drops them again, so neither a handler nor `ClientSession.Header` sees one.
- The router's response writer drops `Set-Cookie` and `Set-Cookie2`, in any letter case, before the header is sent. That includes the header of an informational (1xx) response.

The reason is the shared domain. Any host under `bringyour.com` can set a cookie for the whole domain and receives every cookie scoped to it, and hosts the operator does not run may be named under it (the planned extender names). A browser's login lives in storage on the ur.io origin ([IDP.md §1](../IDP.md)). A feature that seems to need a cookie needs a different design.

## 2. The token (`by_jwt`)

**Signing.** The keys are the versions of the vault `jwt.yml` `tls_key_paths`, the domain TLS keys. Tokens are signed with the newest ECDSA key version whose file was written to the vault at least 2 hours ago (`byJwtKeySigningDelay`), else the newest such RSA key, and the header names its `kid`. With no version that old, the newest key signs. Verification tries the key that `kid` names first, then every held key. The signing method is pinned.

**Key rotation.** A certificate rotation adds a key version to the vault. A running process reads the vault again when a token names a `kid` it does not hold, or names none and fails every held key, at most once per 10 seconds, so it verifies the new version without a restart. The signing delay lets the vault push reach every host before any host signs with the version. Keys are only added while a process runs: a version removed from the vault keeps verifying until restart, and stops signing. Before 2026-10-12 a process read its keys once, and verifiers started before a rotation refused every token signed with the new version until they restarted (`session/by_jwt_keyring.go`).

**Registered claims:**

| Claim | Value |
|---|---|
| `iss` | `urnetwork:byjwt` |
| `sub` | The user id |
| `aud` | `urnetwork:api` and `urnetwork:connect` |
| `exp` | Mint time + 30 days; + 7 days for a sidecar token |
| `nbf`, `iat` | Mint time |
| `jti` | Fresh for every mint |

**URnetwork claims:**

- `network_id`, `network_name`, `user_id`.
- `create_time`: the lineage time (§5). It is not the mint time.
- `client_id`, `device_id`: client tokens only.
- `session_id`: the sign-in session, when sessions are on. A network token and its client tokens share it.
- `root_client_id`: the topmost client row, on client tokens.
- `pro`: re-derived at every mint.
- `roles`, `principal`: assigned at client or auth code creation. They mean nothing to the network.
- `lag_guest`: a lag.dev guest's credential (§1). Limits the token to the lag routes and keeps it off the connect transport.
- `strict_exp`: a sidecar token. Every renewal and derived credential keeps it, and a mint reads it from the client's owner row, whoever asks.
- `api_key_id`: the API key this credential descends from through an auth code (§1). Every renewal and derived credential keeps it.
- `guest_mode`: deprecated, and false on new tokens.

## 3. Verification

Every API request (`ClientSession.Auth`) and every connect transport (`connect/transport.go`) verifies its token in this order. The connect transport requires the audience `urnetwork:connect` and a client token.

1. **Signature and method.**
2. **Registered claims.**
   - A wrong `iss`, `aud` or `sub`, or a future `nbf` or `iat`, is always refused.
   - A token with no `exp` (or other missing claims) is accepted only while vault `auth.yml` `reject_missing_expiration` is false.
   - An expired token is accepted only while `reject_expired` is false, with 30 s of leeway.
   - Each such acceptance counts in `urnetwork_auth_jwt_legacy_accepts_total{cause,kind}`, where `kind` is `network` or `client`. A sidecar token's counts under the cause `strict_expired`.
3. **Session horizon.** A token that carries `session_id` or `root_client_id` must have `exp`. It is refused from `exp + 60 days + 30 s`, whatever the gates say. A sidecar token must have `exp`, `client_id` and `root_client_id`, and once `p0_enforce` is on (§8) it is refused from `exp` itself, with no leeway, whatever `reject_expired` or the 60 days say. The check runs wherever a credential is accepted, so an authorization lease retires a sidecar's transport at `exp`.
4. **Session marker.** A `session_id` is checked against its Redis revocation marker. If Redis is unavailable, the token is refused; there is no fallback.
5. **Account state, in one database read** (`ValidateByJwtState`).
   - The network exists and the user is its admin.
   - `create_time` is not before `network_user.credential_change_time`.
   - A client token is also checked against its client:
     - the client is active in the network;
     - the device matches;
     - its lineage holds. A token with `root_client_id` requires that root to be active in the same network. A token with neither `session_id` nor `root_client_id` requires every ancestor to be active, in the same network, reaching a top-level client within 1,024 levels.
   - A token with `api_key_id` also requires that key to exist in the network. No Redis record holds this, so such a token stays on this query with the revocation record on.

**Failures.**

- A failure answers 401. A failed state check reads "Signed token is no longer active."
- Rejections count in `urnetwork_auth_jwt_rejections_total{cause}`, with causes such as `signature`, `expired`, `missing_claims`, `no_active_row` and `credential_rotated`.
- An API key skips steps 1–5: its hash lookup is the check.

**Connect transports.** These register before their final check. After admission, an authorization lease checks each connection again every 60 s and retires it after 90 s without a successful check. So a revoked session or a removed client loses its connections within about 90 s.

**Redis revocation authority (owner decision, 2026-10-26; REVOKE-FINAL.md §14).** With vault `auth.yml` `revocation_record_enabled: true`, a tagged credential skips steps 4 and 5.
- It reads one Redis revocation record, keyed by its root client or its session in one of 64 shards of its network. The record holds the session, own-client, root, admin and credential-cutoff state.
- If the record cannot answer, the SQL state query does, after the session marker and the session journal, and its valid answer is projected into the record.
- If Redis is unavailable, the credential is refused with 503 and nothing falls back to SQL.
- Untagged credentials keep steps 4 and 5 unchanged.

## 4. Route authorization: admin, client, Embed

Every route has a class in `api/route_authz.go`: 241 routes as of 2026-10-13.

- `TestEveryRouteIsClassified` fails for any route without a class.
- At runtime, an unclassified route refuses client tokens.
- The router enforces the class before the handler runs, whatever wrapper the handler uses (`router/client_credential.go`).

| Class | Routes | Network credential | Client token, ordinary network | Client token, Embed network |
|---|---|---|---|---|
| Public | 115 | Not needed | Not needed | Not needed |
| Client | 47 | Yes | Yes | Yes |
| OwnClient | 5 | Yes, any client | Its own client and children | Its own client and children |
| OwnClientPayout | 4 | Yes | Its own client | **403** |
| AppAdmin | 27 | Yes | Yes, until the hard gate | **403** |
| Network | 43 | Yes | **403** | **403** |

- **Public.** These routes need no credential, or a credential of their own kind: sign-in, network check and create, webhooks, signed objects, operator secrets, the OAuth protocol. A `by_jwt` grants nothing more.
- **Client.** The call acts for the caller's own client, reads what every installation shows, or buys something the caller pays for. Examples: `GET /auth/refresh`, provider discovery, balances, payment intents.
- **OwnClient.** `POST /network/auth-client`, `POST /network/remove-client`, `POST /device/set-name`, `GET /network/client-data-cap` and `GET /network/client-acl-group`. The model limits a client token as follows (`model/network_client_model.go`):
  - it creates only children of its own client ("A client token cannot create a top-level client.");
  - it reissues only its own client and its children;
  - it removes only itself and its children;
  - it renames only its own device.

  Any other client's id answers "Client does not exist."
- **OwnClientPayout.** `GET /sn/wallet`, `POST /sn/wallet`, `POST /sn/wallet/consent` and `POST /sn/head/binding`. These are a provider's own payout. What an Embed network's clients provide is the network's to be paid for.
- **AppAdmin.** The 27 account and network administration routes that the shipped apps still call with their client token: account deletion, auth codes, sign-in methods, seed phrases, the client list, the network user, preferences, wallets and payouts, referrals and names. The list is in `route_authz.go` and AUTHZ1.md §7.
- **Network.** Every other administration route:
  - API keys and OAuth consent;
  - the Embed APIs (data caps, ACL groups, `GET /network/embed`);
  - sessions and revocation;
  - device sharing, subscriptions, Circle wallets and provider stats;
  - `register-client-v1`, `remove-clients`, `/network/user/update` and `/auth/network-refresh`.

**The gate.**

- The router reads the bearer token's claims unverified. Network tokens and API keys pass at no cost.
- A token that names a client is verified before it is refused. A token that fails verification goes to the handler, whose authentication answers 401 as usual.
- The refusal is **403** with "A client token cannot administer the network. Use the network's root token or an API key." It is 403, not 401, because clients read a 401 as a dead token: the SDK signs the app out when its client-token refresh gets one. A 403 only refuses the call.

**The lag guest gate.** Separate from the classes, every route except `GET /lag/extenders` and `POST /lag/track` (`lagGuestRoutes`) refuses a lag guest's token (§1) with **403**, "A lag.dev guest token works only on the lag.dev routes.". This includes the Public routes. The gate reads the `lag_guest` claim unverified, so any other credential costs one decode of its claims. It verifies a token that claims to be a guest's before refusing it; a token that fails verification goes to the handler, as above. `TestLagGuestTokenServesOnlyTheLagRoutes` keeps every route but the two gated.

**The P0 auth fix (DESIGN.md §7.4, stage 1 and D-TEAM-14).** Each of the three `refusal` values below is counted in `urnetwork_auth_child_admin_calls_total{route,refusal,result}`, and until `p0_enforce` is on (§8) that is all it does: `result` is `logged` and the call goes through. Once it is on, `result` is `refused`. None of this changes what a network credential does.

- **Marks.** `POST /network/auth-client` takes `admin_refused: true` and `strict_exp: true` and confirms them in its reply. `strict_exp` implies `admin_refused`. A new client keeps them for life, its children inherit both, and a re-auth may repeat them but never add one. A binary that does not link migration group MG0 refuses a request for a mark rather than dropping it.
- **`refusal="client_admin"`.** On the 27 AppAdmin and 10 OwnClientPayout routes, a child's token or an `admin_refused` client's token is refused with the client token message (`model.ClientRefusesAdmin`). So is one whose client cannot be read; those count in `urnetwork_auth_client_admin_lookup_errors_total` too. A top-level client with no owner row is a legacy client and keeps the routes until the hard gate. Both facts are immutable, so each process caches the answer.
- **`refusal="strict_exp_route"`.** A sidecar token reaches only the routes `strictExpRoutes` lists, and every other route, a Public one or an unclassified one included, answers **403**, "A sidecar token cannot call this route.", before its handler. `TestSidecarTokenRoutes` keeps the list exact. The route's own class still applies.
- **`refusal="strict_exp_proof"`.** On `GET /auth/refresh` and `POST /network/auth-client` a sidecar token also carries a `DPoP` header (RFC 9449, `session/dpop_proof.go`) signed by the client's bound key, or the call is refused with **403**, so the sdk does not sign out. The key lookup (`model.ClientProofPublicKey`) reads tables migration group MG1 creates, so until then no client has a bound key and every proof fails, which is §7.4's rule for a client with none.
- **Preview.** Until stage 2, a mint that ends in a sidecar token is refused outside `preview_networks` (§8). It is a refusal in every mode.
- **Payment.** A sidecar token is refused `X-PAYMENT` on `POST /network/auth-client`, in every mode: no client holds one yet, so this needs no log-only days.

**Embed.**

- **Definition.** A network counts as Embed when Embed was ever enabled for it (a `network_embed` row, even a disabled one) or it has the Embed plan's client allowance (`model.NetworkRefusesClientAdmin`). A disable never reopens the admin routes: the client tokens handed out while Embed was on are still valid.
- **Control.** `bringyourctl network embed --enable [--client-limit n] | --disable`.
- **The Embed APIs follow the current flag.** These are the data caps and the ACL groups. A network without the flag gets "Embed isn't enabled for this network." Caps and groups already stored stay enforced after a disable, because escrow admission and peer isolation read their own tables.
- **How a customer runs it.** The customer's backend holds the network credential. An API key is the recommended choice, because it can be revoked on its own. The backend mints one client token per installation with `POST /network/auth-client`. Installations administer nothing.

**The hard gate (pending, by owner decision: wait until clients have updated).** The 27 AppAdmin routes become Network for every network once both of these hold:

1. The apps ship an SDK at or after `bc72b109`, which sends the network credential on admin calls.
2. Old app builds have aged out. A counter on the gate can show this, or a minimum app version enforced through `upgrade_required`.

To flip a route, change `routeAccessAppAdmin` to `routeAccessNetwork`. The SDK's copy of the class (`apiAdminRouteAccess`) must change with it; `TestApiAdminRoutesMatchTheServer` keeps the two equal. Flip auth codes, sign-in methods, seed phrases and account deletion first.

## 5. Minting, refresh and renewal

**The `CreateTime` rule.** Every credential minted from a presented credential keeps that credential's `create_time`. Only the registered claims are fresh. This covers both refresh routes, auth codes, client creation and client registration. Only a real sign-in stamps a new `create_time`.

The reason: a password reset sets `credential_change_time = now()`, and in Postgres that is the reset transaction's *start*. A refresh minted while a reset was committing would get a fresh time and outlive the reset. With the time carried forward, a token minted from a revoked credential is revoked with it. Both refreshes also check state again inside the mint transaction, under the session lifecycle lock (`session.RenewSessionCredential`).

**`GET /auth/refresh` (Client class).**

- Client tokens only. A network token gets `{"error": {"message": "Client ID is required for token refresh."}}`.
- It needs the device and an active client in the same network.
- It re-reads the network name and Pro.
- It keeps identity, `create_time`, session, roles, principal and the client's topology.

**`POST /auth/network-refresh` (Network class).**

- Network tokens only. A client token gets 403 at the router, and the handler refuses one again. An API key gets a result error.
- A network that is gone, or a user who is no longer its admin, gets 401.
- It re-reads the name and Pro, and keeps guest mode, roles, principal and `create_time`.
- While the gates are off, it renews expired and legacy no-`exp` tokens too, which moves the installed base onto renewable tokens.
- Outcomes count in `urnetwork_auth_network_refreshes_total{outcome}`.

**Answers.** A refresh answers 200 with `{"by_jwt": …}` or `{"error": {"message": …}}` for a refusal of the request's kind; 401 when the credential is rejected; 403 for a client token on a Network route.

## 6. Revocation

- **Password reset.** Every credential whose `create_time` is before the reset is rejected: the network token and every client token minted from it.
- **Removing a client.** Its tokens stop working at once. For untagged tokens, so do its descendants'. Connections close within the lease, about 90 s.
- **Sessions.** These routes are Network class: `GET /network/sessions`, `POST /network/revoke-session`, `POST /network/revoke-other-sessions` and `GET /network/session-operations/…`. Revoking writes a Redis marker that every check reads. See REVOKE-OPERATIONS.md.
- **API keys.** Removing a key stops it at its next request.
- **No per-token deny list.** Apart from session markers, a JWT stays valid until expiry, a password reset, or its client's removal.

- **Removing an API key** deletes the auth codes that descend from it, and `/auth/code-login` refuses any that survive a race with the delete. The network token such a code minted names the key, so it stops at its next state check, with everything derived from it.

**Known gap, until MG0 registers.** A network credential can create an auth code, and that includes an API key. Logging in with the code yields a network token that does not end when the key is removed, in a build that does not link migration group MG0, which holds `auth_code.api_key_id`. Revoke such a token with a password reset, or with session revocation once sessions are on. A code created before MG0 applied names no key, so this holds for it too, for its 24 hours.

## 7. Clients of these rules

**SDK** (sdk/AUTHZ-CLIENT.md):

- An `Api` keeps the network credential (the sign-in token or an API key) beside the device's client token.
- The request seam picks the credential by route class. It is a copy of `route_authz.go`'s AppAdmin and Network classes.
- An admin call with no network credential fails locally with `ErrNetworkCredentialRequired`, and nothing is sent. The SDK never falls back to the client token. `Api.HasNetworkCredential()` reports which case applies.
- Client tokens refresh at a point between 40% and 50% of their lifetime, never later than the half-life. The kept sign-in token renews in the same window at `/auth/network-refresh` and is saved to `LocalState` with a compare-and-swap.
- The app signs out only when the server rejects the client-token refresh, with a 401 or a result error. A 401 on an admin call only drops the network credential from memory.

**ur.io** stores the network token. It refreshes the token at `POST /auth/network-refresh` on load, after purchases and when the SDK reports stale claims. A 401 or 403 there signs out.

**sn miner** (`sn/clientauth`, `sn/miner`):

- Each `provide` process renews its `<state>/jwt` at a point between 40% and 50% of its lifetime, never later than the half-life.
- A `<jwt>.lineage` file keeps revoked clients blocked across renewals.
- Explicit sign-ins write under the same lock (`WriteNetworkTokenWithContext`).
- Under `--all-operators --auto-register`, a token the server rejects is set aside and the supervisor signs in again with the hotkey, with a rate limit.
- The validator does not renew; it reads the token only for explicit registrations.

## 8. Gates and rollout state

| vault `auth.yml` | main, 2026-10-09 | Effect |
|---|---|---|
| `reject_missing_expiration` | false | Tokens without `exp` (pre-hardening) are accepted |
| `reject_expired` | false | Expired tokens are accepted (except a tagged token past its horizon) |
| `session_creation_enabled` | absent (false) | New mints carry no `session_id`. Tagged tokens are still enforced |

The values are read once per process, so a flip needs a restart.

| config `sidecar.yml` | main, 2026-11-06 | Effect |
|---|---|---|
| `p0_enforce: {enabled: …}` | absent (false) | The P0 auth fix (§4) and the sidecar token's `exp` refuse what they otherwise only count. On at M1, after 14 log-only days of `urnetwork_auth_child_admin_calls_total` |
| `preview_networks: […]` | absent (empty) | The networks that may hold sidecar tokens before stage 2 |

`config/<env>/sidecar.yml` is optional and inert when absent. Services reread it every 60 seconds, so a flip needs no restart or deploy (`sidecar_config.go`). A file that does not parse, or is there but cannot be read (a dangling link, a directory, an inaccessible config home), keeps the last good read, and on a process's first read it enforces and previews nothing. Only a file absent from every config home is inert. Removing the file keeps the last read until a restart; to turn a gate off, edit it.

**Migration group MG0** (`network_client_owner` and the altered `account_api_key` and `auth_code`) stays pending under the `sidecar_pending` build tag, in `db_migrations_sidecar_pending.go`, until its Gn-out moves it into the migrations slice. A build with the tag panics at start outside `local`, `canary` and tests. Code reads the group only where `server.SidecarMg0Linked()` says the binary links it, and then the group is part of the head the binary requires, so without the tables the binary never reports ready.

**Before turning on `reject_expired`:**

- The apps must ship the SDK with network-token renewal (`679a836a` or later), and ur.io must deploy its switch to `/auth/network-refresh`.
- `urnetwork_auth_jwt_legacy_accepts_total{cause="expired",kind="network"}` must fall to near zero.

After the flip, a network token that expired unrenewed needs a new sign-in. Turn on `session_creation_enabled` only by the deployment gates in REVOKE-OPERATIONS.md.

## 9. Where it lives

| Concern | Code | Tests |
|---|---|---|
| Claims, parsing, the gates, state validation | `session/by_jwt.go`, `session/session_state.go` | `session/*_test.go` |
| Signing and verification keys, rereads, the signing delay | `session/by_jwt_keyring.go` | `session/by_jwt_keyring_test.go`, `session/by_jwt_rotation_test.go` |
| API authentication, API keys, the unsigned and API-key markers | `session/client_session.go`, `apikey/apikey.go`, `mcp/auth.go` | `session/client_session_*_test.go` |
| Session mint, renewal, revocation | `session/session_mint.go`, `session/session_operation.go` | `session/session_*_test.go` |
| Route classes and the gate | `api/route_authz.go`, `router/client_credential.go` | `api/route_authz_test.go`, `api/route_authz_db_test.go` |
| The P0 auth fix: marks, the client admin check, the allowlist, the proof, the gates | `model/network_client_owner_model.go`, `api/route_authz.go`, `router/strict_exp_credential.go`, `session/dpop_proof.go`, `session/session_state.go`, `sidecar_config.go` | `model/network_client_owner*_test.go`, `api/route_authz_sidecar*_test.go`, `router/strict_exp_credential_test.go`, `session/strict_exp_test.go`, `session/dpop_proof_test.go`, `connect/transport_strict_exp_test.go`, `sidecar_config_test.go` |
| Pending migration groups | `db_migrations_sidecar*.go` | `db_migrations_sidecar*_test.go` |
| Embed rules | `model/network_client_admin_model.go`, `model/network_embed_model.go` | `model/network_client_authz_test.go` |
| Client creation and own-client limits | `model/network_client_model.go` | `model/network_client_authz_test.go` |
| Refresh routes | `controller/auth_controller.go` | `api/auth_refresh_db_test.go` |
| Connect transport auth and leases | `connect/transport.go`, `connect/transport_authentication.go`, `session/authorization_lease.go` | `connect/*_test.go` |
| SSO id tokens | `session/sso_jwt.go`, `session/google_jwt.go`, `session/apple_jwt.go` | `session/*_jwt_test.go` |
| No cookies: dropped from requests and responses | `session/cookies.go`, `router/router.go`, `router/metrics.go` | `session/cookies_test.go`, `router/router_cookies_test.go` |
| Lag guests: the claim, the route gate, the connect refusal, no grants | `session/by_jwt.go`, `router/lag_guest_credential.go`, `api/route_authz.go` (`lagGuestRoutes`), `connect/transport_authentication.go`, `controller/lag_session_controller.go`, `model/lag_guest_model.go` | `session/lag_guest_jwt_test.go`, `router/lag_guest_credential_test.go`, `api/lag_route_test.go`, `connect/transport_lag_guest_test.go`, `model/lag_grant_test.go` |

Keep this document current when a class, a credential or a gate changes.
