# NEXTSTEPS4 — configuration and rollout for the 2026-07-11 changes

Everything below is coded, built, and tested. This file lists the
configuration, build, and sequencing steps needed to activate it, in order.

## 1. SSO audience validation — fill the vault client ids (security)

The apple/google id-token validation (`server/jwt/sso_jwt.go`) now enforces
`iss`, `aud`, `email_verified`, RS256-only, and required `exp`. The `aud`
check is driven by vault config and is **disabled while the value is empty**,
which is how it ships. Until the values are filled, the token-substitution
hole is still open.

`vault/main/google.yml` (field added at top level; scalar or list):

```yaml
# every platform's sign-in client has its own id; list them all
client_id:
  - "<ios oauth client id>.apps.googleusercontent.com"
  - "<android oauth client id>.apps.googleusercontent.com"
  - "<web oauth client id>.apps.googleusercontent.com"
```

`vault/main/apple.yml` (new file, already created with an empty value):

```yaml
# the aud of an apple id token is the app bundle id (or services id for web)
client_id:
  - "com.bringyour.network"
  - "<services id if sign-in-with-apple is used on web>"
```

Notes:
- The ids come from the Google Cloud console (OAuth clients) and the Apple
  developer portal. The existing `oauth.client_id` in google.yml is the
  play-billing client — do NOT reuse it.
- Rollout watch: sign-in failures with "Invalid audience." mean a client id
  is missing from the list; "Email not verified." is the new email_verified
  enforcement (rare, expected only for provider accounts with unverified
  emails).

### SSO nonce: evaluated and deliberately NOT implemented

A client-generated OIDC nonce adds no protection to a stateless api: the app
sends the token AND the nonce it claims to have used, and an id token is
signed, not encrypted — an attacker holding a stolen token simply reads its
`nonce` claim and echoes it. The check only has value when the expected nonce
comes from server state the attacker cannot control (a server-issued nonce,
which is what wallet login already does via `/auth/wallet-nonce`).

Decision (2026-07-11): skip the nonce. An sso id token is a short-lived bearer
credential — a compromised client leaks the `by_jwt` just as easily — so the
nonce ceremony would cost changes across every app with login for no real
gain. The protections that DO matter are implemented and tested: `aud`
(blocks token substitution from another app), `email_verified` (blocks
unverified-email takeover), `iss`, required `exp`, and RS256 pinning.

If replay protection is ever wanted, the design is a server-issued nonce
mirroring `/auth/wallet-nonce` (mint -> store -> require the token's claim to
match an outstanding unconsumed nonce -> consume). Note the flow constraint
found while prototyping: sso login legitimately re-presents the SAME id token
to `/auth/network-create` when the user does not exist yet, so any single-use
guard must bind nonce->token (allow the same token, reject a different one)
rather than consuming on first sight.

Audit finding while mapping the apps: only apple, android, and the ur.io web
app have sso login. The extension receives a finished `by_jwt` from the
website; windows and linux have no sso at all (password / auth-code / wallet
only). Android is on the legacy `GoogleSignIn` api, which cannot attach a
nonce — a nonce there would have forced a Credential Manager migration.

## 2. SDK framework rebuilds + app releases (deleted-client logout)

The sdk gained `AuthLogoutListener` (`device.AddAuthLogoutListener`), an
immediate jwt validation at device start, and conservative logout semantics
(logout only on a confirmed 200-with-error result or a 401; never on offline
networks, timeouts, or 5xx — covered by `device_token_manager_test.go`).

- Rebuild and publish the sdk frameworks (URnetworkSdk.xcframework, the
  android AAR) — the apple and android apps reference the new listener API
  and will not compile against the old artifacts.
- Ship the apple and android releases (already wired: apple
  `DeviceManager.setupDeviceListeners`, android `DeviceManager` +
  `MainApplication.initDevice`).
- Port to the remaining apps: subscribe to `AddAuthLogoutListener` and run
  the app's logout + return-to-login flow. Until ported, those apps keep the
  old behavior (sdk wipes local auth; the user lands on login at the next
  cold start).
- Rollout watch: `/auth/refresh` error rates by message. The startup refresh
  makes previously-silent failures visible (e.g. legacy jwts missing
  device_id will log out at start instead of failing silently at the
  exp−14d refresh).

## 3. Server deploy — migrations and sequencing

New migrations (append-only, index-tracked; keep order when merging with the
concurrent branch's `pro` migrations):
- `feedback_log_upload` table (log upload metadata + rate limit)
- `client_reliability_sync` table (drain coverage ranges)
- DROP `auth_session`, `auth_session_expiration`, `auth_code_session`,
  `device_adopt_auth_session` — the session revocation deny-list was never
  enforced at runtime (short-circuited check) and the machinery is removed
  end to end (IsByJwtActive, ExpireAllAuth, session-id jwt claims, session
  row writes). Jwts are validated by signature + expiry. Frees ~725MB
  immediately.
- `network_client.deactivate_time` (metadata-only)

New/changed tasks (seeded automatically at taskworker start):
- `remove_old_audit_network_events` (180d retention, batched)
- `cancel_hung_account_payments` (30d pending → canceled → re-planned)
- straggler contract cascade inside `remove_completed_contracts` (90d final
  fallback)
- top-level client marker inside `delete_disconnected_network_clients`
  (90d idle → inactive → hard delete 30d later)

## 4. Client reap — shipping now, ungated

Decision (2026-07-11): ship immediately. Nothing is gated: the idle marker,
the 30-day reap, the hung-payment canceler, the straggler cascade, the
refresh active-check, and the sdk logout flow are all live on deploy. (The
only gate anywhere is `exportStatsDisabled`, which is the replica-dependent
stats export loop — unrelated to the reap; see §5.)

On the first run after deploy, the marker flags the entire existing backlog
of abandoned top-level clients (90d+ idle) inactive, and their jwts stop
refreshing immediately.

- Users on app versions **with** the logout listener get a clean logout at
  next app start.
- Users on older app versions degrade to the previous behavior: the sdk
  wipes local auth when its scheduled refresh eventually runs, and they land
  on the login screen at the next cold start (silent, but recoverable).

Payout audit: the hung-payment canceler logs
`canceled hung payment <id> WITH an initiated payment record` for payments
that had an external transfer initiated. Audit those against the payment
provider — if the external transfer actually settled out of band, the
re-planned sweeps would pay a second time.

## 5. Replica enablement (when the read replica lands)

- Point `server.ReplicaDb` (db.go) at the replica pool (today it uses the
  primary pool).
- Flip `exportStatsDisabled = false` in `taskworker/work/audit_work.go` to
  resume the /stats/last-90 export loop. Until then refresh manually with
  `bringyourctl stats export`.

## 6. Reliability: what is fixed, and the policy call that remains

Two distinct causes of "all the providers drop out of the market" are fixed
and regression-tested (`TestFindProviders2ReliabilityFlushLag`,
`TestFindProviders2ReliabilityDeployGap` — both fail without their fix):

- Blocks that never reached pg (drain lag / redis loss) are excused via the
  coverage ranges (`client_reliability_sync`).
- Blocks a platform event took out are excused via per-block health
  (`client_reliability_block`): a synchronized collapse in the valid-client
  count marks the block degraded, and it counts toward neither the numerator
  nor the denominator for anyone.

A FULL outage (nobody announces) was already safe: nothing is written to
redis, so the block never drains, so it is never covered.

Third cause, fixed 2026-07-11: a single reconnect used to invalidate the whole
block (`valid` required `connection_new_count = 0`). A disconnect IS real user
impact — it drops the provider's live clients, which is why it is measured —
but at zero tolerance one reconnect (a rotated handler, a mobile blip, a NAT
rebind) cost a block, and at the 0.99 hour threshold one block (59/60 = 0.983)
took a provider out of the market for an hour. Now:

- `model.ReliabilityAllowDisconnectCountPerBlock = 1` — one disconnect per
  block is forgiven; flapping (2+) still fails the block. Both writers pass it
  to the `client_reliability_valid` sql function, which is the single place the
  rule is written, so changing the constant needs no migration.
- the hour threshold in `UpdateClientScores` is 0.95 (was 0.99), which carries
  the residual case: a reconnect that straddles a block boundary lands the
  new-connection sync in a block with no established sync, so that block is
  invalid however tolerant the rule is. 0.95 allows three of those an hour.

Deploy note: `valid` was a STORED generated column, and changing a generation
expression rewrites the table (4TB — not an option). The migration uses
`ALTER COLUMN valid DROP EXPRESSION` (catalog-only, no rewrite, keeps the
existing index) and the writers supply the value from then on. Rows already
written keep the values the strict rule gave them, so the change takes full
effect over one lookback window. Requires pg 13+.

Both changes affect payouts as well as matchmaking (the same weights feed the
subsidy split), which is expected: a provider that reconnects once an hour was
being paid as if it were unreliable.

## 7. Prod measurements (validate the storage work)

Run the queries in `../xops/db/STORAGE1-REVIEW.md`:
- dead-tuple/bloat check for the big tables (decides pg_repack)
- stuck-pending payment tail (should drain via the 30d canceler)
- expired client_reliability backlog (should drain via the batch-looped
  cleanup; the structural fix remains partitioning by block_number)
