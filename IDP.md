# URnetwork as an OAuth 2.1 / OpenID Connect identity provider

URnetwork becomes an identity provider. The immediate driver is the MCP server:
the Claude Connectors Directory requires OAuth 2.0 for authenticated services,
and the MCP specification requires an MCP server to act as an OAuth 2.1
resource server. The same authorization server makes every other first-party
surface (ur.io, the apps) able to authenticate against one issuer instead of
against the ad-hoc auth-code flow.

Today URnetwork is only an OAuth *client* — it consumes Apple and Google SSO via
`AuthLoginArgs.AuthJwt`. It has never been an issuer. This document is the design
for becoming one.

## 1. Roles

| Role | Who | Where |
|---|---|---|
| Authorization server | URnetwork | `https://auth.bringyour.com` |
| Authorization endpoint (user-facing) | ur.io web app | `https://ur.io/authorize` |
| Resource server | the MCP server | `https://mcp.bringyour.com` |
| Client | Claude, and any other MCP client | — |

The authorization endpoint deliberately lives on a **different origin** from the
issuer. The consent page must reuse the logged-in session, and that session is
held in browser storage on the ur.io origin. Browser storage is origin-scoped
and the two are different apex domains, so neither `localStorage` nor cookies
can cross from `auth.bringyour.com` to `ur.io`. RFC 8414 lets the metadata
declare an `authorization_endpoint` on any origin, so the issuer identity stays
`auth.bringyour.com` while the user-facing page stays where the session is.

## 2. Identity model

A token represents **a user, acting on one network**.

- `sub` is the user id.
- `network_id` is a separate claim, bound at authorization time.

The network is bound at authorization rather than resolved per request. A token
must never silently follow the user to a different network — for example if the
user later administers a second one. `getNetworkIdForUser` selects on
`network.admin_user_id` and returns the first row, so per-request resolution
would be ambiguous as soon as that happens.

`principal` carries through from the authorizing session and identifies the
user. `roles` carries through as well, but in practice is empty: roles are only
assigned at client or auth-code creation, and an ordinary ur.io login carries
none. Guest networks cannot authorize at all — a guest must upgrade first,
because every scope that matters bills a network.

## 3. Signing keys: the hard security boundary

**OAuth tokens are signed with keys that are disjoint from the ByJwt keys.**
This is a security boundary, not a convention.

`jwt.ParseByJwt` parses with `gojwt.WithoutClaimsValidation()` and verifies
against the entire key set. It validates no registered claims — not `aud`, not
`iss`, and **not `exp`**. If an OAuth access token were signed with a ByJwt key,
then a token scoped to `mcp:read` would verify as a full, unscoped, effectively
non-expiring platform credential on every API route. Disjoint key sets make that
impossible rather than merely disallowed.

Consequences that must hold:

- An OAuth signer key must **never** appear in `jwt.yml` `tls_key_paths`.
- OAuth signer keys are purpose-generated EC P-256 keys, not domain TLS keys.
  Reusing a TLS key would be cross-protocol key reuse and would couple token
  signing to the ACME renewal pipeline.
- `oauth.VerifyAccessToken` requires an expected audience. There is no
  audience-free entry point, so a resource server cannot accidentally accept
  another resource's token.
- Unlike `ParseByJwt`, OAuth verification validates every registered claim:
  issuer, audience, and a required expiry.

`server/oauth/token_test.go:TestAccessTokenIsNotAByJwt` asserts the boundary
directly: the ByJwt key set must fail to verify an OAuth token.

### Key management

Keys live in the shared vault alongside TLS, under a dated version directory:

```
vault/all/oauth/<Y.M.D>/<kid>.key           # active
vault/all/oauth.pending/<Y.M.D>/<kid>.key   # staged, not yet signing
```

`services.yml` references a key as `oauth/<kid>.key`; the vault resolver's
`versionLookup` expands the version directory, so the reference never names one.

The `kid` is the RFC 7638 JWK thumbprint of the public key. It is derived from
the key rather than assigned, so the file name, the `services.yml` entry, the
JWT header, and the JWKS entry cannot drift apart. The loader recomputes the
thumbprint at startup and refuses to start on a mismatch — a mismatch would
publish a JWKS that cannot verify the tokens actually being signed.

Generate with:

```
warpctl oauth keygen <env>
```

which writes to `oauth.pending` and prints the `services.yml` block.

**Rollout order matters more than for TLS.** A host told to sign with a key it
does not have cannot serve, and a host missing a key another host is already
signing with cannot verify those tokens. So:

1. deploy (xops edges) so every host has the pending key —
   `playbook-edges.yml` merges `oauth.pending` into `oauth` during vault staging,
   mirroring `tls.pending`
2. move `all/oauth.pending/...` into `all/oauth/...`
3. add the key to the **top** of `oauth.signer_keys` in `services.yml`
4. keep the previous key listed until every token it signed has expired

`signer_keys` is newest-first: the first entry signs, and every entry is
published in the JWKS so tokens signed before a rotation keep verifying. Because
access tokens live one hour, a rotated key can be dropped the day after.

## 4. Tokens

| Token | Lifetime | Format | Audience |
|---|---|---|---|
| Access token | 1 hour | signed JWT (ES256) | the resource, e.g. `https://mcp.bringyour.com` |
| Refresh token | 90 days, sliding | opaque, hashed at rest | — |
| ID token | 1 hour | signed JWT (ES256) | the `client_id` |
| Authorization code | 1 minute | opaque, single use | — |

Access tokens are short so that revoking a refresh token takes effect quickly.
Refresh tokens are the long-lived, revocable half: opaque random values stored
hashed in Postgres, so revocation and the sliding window actually work. A JWT
refresh token could not be revoked, given there is no deny list.

Access-token verification is a signature, issuer, audience, and expiry check
with no database read, which keeps the stateless MCP hot path free of a lookup.

Refresh tokens **rotate**: each use issues a new one and retires the old. Reuse
of a retired token is treated as theft — the entire token family is revoked.
This is the OAuth 2.1 recommendation for public clients, which Claude Desktop
and the CLI are.

ID tokens are addressed to the client and are never presented to a resource
server. They carry `at_hash` binding them to the access token issued alongside
(OpenID Connect Core 3.1.3.6).

## 5. Scopes

Deliberately minimal. One scope for reads, and a separate one for the egress
tool because that provisions billed clients — so a caller can be granted lookups
without being granted spend.

| Scope | Grants |
|---|---|
| `mcp:read` | `providerLocations` |
| `mcp:fetch` | `fetch` — provisions a billed egress client |
| `openid` | an ID token |
| `offline_access` | a refresh token |

`offline_access` is **never** advertised in the protected resource metadata or
in a `WWW-Authenticate` challenge: per the MCP spec it is not a resource
requirement.

## 6. Discovery documents

| Document | Host | Spec |
|---|---|---|
| `/.well-known/oauth-protected-resource` | `mcp.bringyour.com` | RFC 9728 — **MUST** for MCP servers |
| `/.well-known/oauth-authorization-server` | `auth.bringyour.com` | RFC 8414 |
| `/.well-known/openid-configuration` | `auth.bringyour.com` | OpenID Connect Discovery 1.0 |
| `/.well-known/jwks.json` | `auth.bringyour.com` | the signer public keys |

The MCP spec requires *at least one* of RFC 8414 or OIDC Discovery, but requires
clients to support both. We publish both.

The authorization server metadata must set
`authorization_response_iss_parameter_supported: true` and the authorization
response must actually include `iss` (RFC 9207), including on error responses.

The issuer must not end in a slash: clients compare it verbatim, and RFC 9207
forbids normalizing before comparison. `oauth.Config()` rejects a trailing slash
at startup.

## 7. Endpoints

| Endpoint | Host | Purpose |
|---|---|---|
| `GET /authorize` | ur.io | consent UI; mints the authorization code |
| `POST /oauth/token` | auth | code → tokens; refresh → tokens |
| `POST /oauth/register` | auth | dynamic client registration (RFC 7591) |
| `POST /oauth/revoke` | auth | token revocation (RFC 7009) |
| `GET /oauth/userinfo` | auth | OIDC UserInfo |

## 8. Client registration

Three mechanisms, in the priority the spec defines:

1. **Client ID Metadata Documents** (preferred). The `client_id` is an HTTPS
   URL; the authorization server fetches the metadata from it. Deprecates DCR.
2. **Dynamic Client Registration** (RFC 7591). Supported for backwards
   compatibility; deprecated by the spec.
3. **Pre-registered** clients.

CIMD means the authorization server fetches an arbitrary attacker-supplied
HTTPS URL. That is an SSRF vector *inside the API* and gets the same treatment
as the MCP `fetch` tool: refuse private, loopback, and link-local targets;
bound redirects, response size, and time; and cache by URL so a `client_id` is
not refetched on every authorization. DCR's open registration endpoint is rate
limited.

Native clients (Claude Desktop, the CLI) use loopback redirect URIs. The
authorization server accepts `http://127.0.0.1:<port>/...` for clients whose
`application_type` is native, and requires HTTPS otherwise.

## 9. Authorization flow

1. The client requests the MCP server without a token.
2. The MCP server answers `401` with
   `WWW-Authenticate: Bearer resource_metadata="https://mcp.bringyour.com/.well-known/oauth-protected-resource", scope="..."`.
3. The client reads the protected resource metadata, finds the authorization
   server, and reads its metadata.
4. The client resolves a `client_id` (CIMD / DCR / pre-registered), generates
   PKCE parameters, and records the expected issuer.
5. The client opens `https://ur.io/authorize?...` with `code_challenge`,
   `resource`, `scope`, and `state`.
6. ur.io reuses the logged-in session if there is one, otherwise renders the
   existing login options (email/phone, SSO, wallet, password). **Consent is
   always shown for a client that has not been approved for these scopes**, even
   when the user is already signed in — silent approval of a third party is a
   confused-deputy risk and would fail directory review.
7. On approval, ur.io posts to the authorization server, which mints a single-use
   authorization code bound to `client_id`, `redirect_uri`, `code_challenge`,
   `resource`, `scope`, user, and network.
8. The user agent is redirected back with `code`, `state`, and `iss`.
9. The client exchanges the code at `/oauth/token` with `code_verifier` and
   `resource`, receiving an access token (audience = `resource`), a refresh
   token if `offline_access` was granted, and an ID token if `openid` was.

Consent is remembered per (user, client, scope set) so re-authorization is
silent when nothing new is being requested. A "connected apps" screen in ur.io
lists grants and revokes them.

## 10. The MCP server as a resource server

The MCP server wraps its handler in the SDK's `auth.RequireBearerToken`, which
emits the correct `401` and `WWW-Authenticate` challenge, and serves the RFC
9728 document with `auth.ProtectedResourceMetadataHandler`. The verifier checks
signature, issuer, expiry, and that `aud` is exactly
`https://mcp.bringyour.com`.

Scopes map to tools: `providerLocations` requires `mcp:read`, `fetch` requires
`mcp:fetch`. An insufficient scope returns `403` with
`WWW-Authenticate: Bearer error="insufficient_scope", scope="...", resource_metadata="..."`.

Internally the verified token is turned into an in-memory `ByJwt` carrying the
token's user and network, so the model layer (`AuthNetworkClient`,
`FindProviderLocations`) is untouched. That synthesized value is never signed
and never leaves the process.

### Cut-over

The MCP endpoint **stops accepting** network JWTs and `urn_` API keys. The MCP
spec is explicit: a server must only accept tokens issued by its own
authorization server, and must not accept or transit any other tokens. This is a
hard cut, not a migration — the MCP server is `0.0.1-beta.1`.

`docs/mcp/SKILL.md` must be rewritten: it currently instructs agents through the
auth-code → JWT → `Authorization: Bearer` flow, which will stop working.

## 11. Schema

| Table | Purpose |
|---|---|
| `oauth_client` | registered clients: CIMD-resolved, DCR-registered, or pre-registered |
| `oauth_authorization_code` | single-use codes bound to PKCE, redirect, resource, scope, user, network |
| `oauth_refresh_token` | hashed opaque refresh tokens, with family id for rotation and reuse detection |
| `oauth_consent` | remembered (user, client, scope set) approvals |

## 12. Deployment prerequisites

- DNS for `auth.bringyour.com`
- a TLS certificate covering it — it is **not** in the current SAN list, which
  covers `api`, `app`, `connect`, `mcp`, `grafana` and several wildcards
- a load-balancer route to whichever service serves the authorization endpoints
- at least one signer key generated, staged through `oauth.pending`, and listed
  in `services.yml`

The issuer is baked into every issued token and published in the discovery
documents. Changing it later invalidates every outstanding token, so it is
settled before the first token is issued.

## 13. Security considerations

- **Key-set disjointness** (§3) is the load-bearing property. Guard it with the
  test, and never add an OAuth key to `jwt.yml`.
- **Audience binding** — a resource server accepts only tokens minted for it
  (RFC 8707). Verification requires naming the audience.
- **PKCE is mandatory** (OAuth 2.1). No implicit grant.
- **Issuer validation** — emit `iss` on authorization responses and advertise
  it, so clients can defend against mix-up attacks.
- **CIMD fetching is SSRF-exposed** (§8) and hardened accordingly.
- **Refresh reuse is treated as theft** — the family is revoked.
- **Consent is never silently granted** to a client that has not been approved
  for the requested scopes.
- **Secrets never travel in prompts or tool results.** The MCP `fetch` tool's
  sealed cookie jar and the OAuth tokens are separate concerns, but the same
  rule applies to both.
