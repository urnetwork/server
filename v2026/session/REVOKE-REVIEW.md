# REVOKE design review

Reviewed 2026-10-09. **Recommendation: revise before enabling revocation.** The core design is sound: a root session identifier inherited by children, a shared validation boundary, atomic same-slot mint/revoke scripts, and retryable dependency failures are appropriate building blocks. Keeping the package move separate from the security change is also useful. The remaining gaps concern what a successful revoke guarantees, whether all accepted credentials remain discoverable, and how Redis state stays consistent with database writes and a mixed server fleet.

This is a review of a proposal, not a report of implemented vulnerabilities. P1 items should be resolved before enabling the feature; P2 items should be resolved before shipping the affected functionality. Explicit policy tradeoffs are identified separately below. Each finding includes a deterministic regression scenario for the eventual implementation.

## Required amendment from the user's follow-up

The user specified a new `root_client_id` JWT claim and this authorization precedence. This is an accepted design instruction, not a request for another approval. It supplements the reviewed plan; `REVOKE.md` remains unchanged.

**Every successful acceptance must still require the JWT's own client to exist, belong to the authenticated network, and be active, with the existing device binding, and must still check the account's `credential_change_time`.** Preserve signature, audience, lifetime, network/admin identity, and caller-specific ownership checks as well. A revoked session may still be rejected before acquiring Postgres; the invariant is that a successful session/root/ancestry check never replaces these common requirements.

| Available authority on the JWT | Additional check, in order |
|---|---|
| `session_id` present | Check that session's revocation marker. Do not fall through to root or ancestry checks when the marker is absent, present, or unavailable; an unavailable store remains a retryable 503. |
| No session id; `root_client_id` present | Require the named root client to exist in the same network and be active. Do not substitute recursive fallback after a root rejection. |
| Neither claim; the JWT's client has a parent | Resolve `network_client.source_client_id` and recursively require active same-network ancestors through the topmost client. |
| Neither claim and no client parent | No additional lineage lookup beyond the common account/own-client checks. A network JWT has no own client row. |

`root_client_id` means the topmost **client row** whose `source_client_id` is NULL. It is distinct from the top-level network JWT that owns a session. The current JWT contains `client_id`, not a parent-client claim ([claims](/Users/brien/urnetwork/server/jwt/by_jwt.go:215)); the durable parent link is [the database column](/Users/brien/urnetwork/server/db_migrations.go:2109). Resolve the recursive fallback from that row, not from an invented existing JWT parent field.

Stamp `root_client_id` on every newly signed client JWT and carry it through refresh and derivation. A new top-level client uses its own ID. A descendant inherits the validated parent's root; when the parent lacks that claim, resolve the existing ancestry before minting. A network credential creating a child of an explicitly supplied source client must derive the root from that source's validated database lineage. Refreshing or reissuing an existing client must preserve/resolve that client's actual lineage, even when a different network-session credential authorizes the operation. Include registration replay, adoption, hosted proxies, localclient, and internal client-token producers in the propagation inventory. A caller-supplied source ID is not itself proof of the root.

Current construction already binds a client caller's source to itself ([source authorization](/Users/brien/urnetwork/server/model/network_client_model.go:340)), validates the source network ([source lookup](/Users/brien/urnetwork/server/model/network_client_model.go:655)), and stores the parent at insertion. I found no production writer reparenting `network_client` in the inspected model paths. Treat the user's same-network, acyclic construction guarantee as the trusted minting model; basic missing-row/network checks and bounded traversal are defensive fallback behavior, not new blockers based on hypothetical corrupt graphs. An inactive ancestor after removal is an ordinary lifecycle case and must be covered.

The current removal code deactivates the **requested row only**. The own-client predicate authorizes removing oneself or a direct child; it is not a recursive deletion or deactivation cascade ([selection](/Users/brien/urnetwork/server/model/network_client_model.go:1202), [single-row handoff](/Users/brien/urnetwork/server/model/network_client_model.go:1231)). The recursive fallback therefore has real work to do when an older descendant's ancestor is removed.

There is an intentional consequence of the requested precedence: for `A → B → C`, with A and C active but B removed, the recursive legacy path rejects C. The root shortcut with `root_client_id=A` checks A and C and does not inspect B; the session path checks the session and C and likewise skips B. Record and test this branch distinction. If deactivating every intermediate ancestor must invalidate every descendant even on the faster paths, that requires an additional cascade/fence policy; do not silently replace the user's precedence with a recursive walk on every request.

Put the combined logic in every validation entry point, including `ValidateByJwtState`, `ValidateByJwtStateInTx`, and the newly present `ValidateByJwtStateForParent` ([entry points](/Users/brien/urnetwork/server/jwt/by_jwt.go:583)). The last helper's current direct-parent ownership predicate must remain in force; checking a root in the same network does not grant one hosted authority access to another authority's child. It presently verifies own-client state and direct ownership, not ancestor activity.

**Deterministic regression coverage:** exercise each branch on API, H1/H3, hosted validation, and the parent-mint transaction; in every branch, independently deactivate the JWT's own client and advance `credential_change_time` and verify rejection. Test both claims present to establish session precedence, missing/active/revoked session markers, 503 without fallback, an inactive root, a removed immediate parent and a longer ancestor chain, and the `A → B → C` distinction above. Verify top-level root assignment and propagation through every mint/refresh site, including a legacy parent and reissue authorized by another session. Retain the hosted cross-owner refusal test and test traversal on a valid long chain with explicit hooks and no sleeps. Optional corruption fixtures may verify defensive failure, but they are not prerequisites for accepting the trusted construction model.

## Findings

### R1 — P1: The proposed cap makes valid, previously tracked sessions unrevocable

**References:** [cap policy](/Users/brien/urnetwork/server/session/REVOKE.md:254), [revoke script](/Users/brien/urnetwork/server/session/REVOKE.md:220), [authentication check](/Users/brien/urnetwork/server/session/REVOKE.md:341).

At 1,001 sessions, the proposed cap removes a still-valid session from `z` without creating its revocation marker. The session-revocation check consults only the marker, while single-session revoke returns `NOT_FOUND` when the member and marker are absent. The evicted token therefore continues to authenticate when its common account/client checks pass, disappears from the Sessions screen, and survives revoke-others. This requires neither Redis data loss nor an expired token. A credential holder who can create sessions can deliberately cause it.

This is an explicit recommendation in the plan, not an accidental omission. Nevertheless, it contradicts the feature's promise for sessions minted after launch. A metric and a password-reset fallback do not make a successful “sign out all other sessions” comprehensive.

**Change:** reject a new session at capacity, or atomically revoke an evicted session while retaining its full acceptance horizon. If presentation must be capped, keep a complete authorization inventory independently of the displayed rows. Include revocation markers in the memory budget: a live-session cap does not bound cumulative revoked sessions, especially when one revoke-others call can create 1,000 markers.

**Regression:** create 1,001 sessions with distinct expirations and retain their signed tokens. After overflow and revoke-others, authenticate every token except the kept session. Assert that none remains usable merely because its listing entry was evicted. Test the chosen capacity behavior explicitly, rather than pinning silent eviction as success.

### R2 — P1: A finite revocation lifetime needs a fleet-wide acceptance horizon, including live connections

**References:** [expiry policy](/Users/brien/urnetwork/server/session/REVOKE.md:391), [connection watcher](/Users/brien/urnetwork/server/session/REVOKE.md:458), [parser](/Users/brien/urnetwork/server/jwt/by_jwt.go:524), [H1 lifetime](/Users/brien/urnetwork/server/connect/transport.go:1462), [H3 lifetime](/Users/brien/urnetwork/server/connect/transport.go:2300).

The 60-day grace addresses an important existing hazard, but the proposed transition to `clockLeeway` when `reject_expired` becomes true can revive revoked tokens. For example, one strict API instance revokes a target network session whose `exp` has already passed and writes a short marker, while a lenient API instance still accepts that network token for 60 days. After the marker expires, the lenient instance accepts it again, allowing new client creation even if cleanup deactivated the old clients. Reverting the setting after writing short-lived markers has the same problem.

There is also no stated bound on an already-authenticated connection. The current transports retire the authentication deadline once admitted. The proposed corrective watcher checks only `EXISTS r:<sid>`. A connection can therefore remain open past `exp + grace`, after its session disappears from the inventory and becomes `NOT_FOUND` to revoke. Idle timeouts do not bound a connection carrying traffic.

**Change:** define one `accept_until` contract used by parsing, listing, cleanup, marker retention, and connection lifetime. Retain the longest acceptance policy supported anywhere in the fleet or by an allowed rollback; do not shorten marker retention on one process's local flag. Bound admitted connections by their credential's acceptance deadline, or explicitly preserve their revocation inventory until they close. Include clock skew and expiry rounding in the retention margin. Listing must use the grace-adjusted horizon; filtering at raw `exp` would hide credentials that still work.

**Regression:** with explicit clocks, have strict and lenient instances share Redis; revoke just after `exp`, advance beyond the short marker lifetime, and verify the lenient instance still refuses the network JWT and new-client creation. Repeat through a configuration rollback. Keep H1 and H3 connections active across their acceptance deadline and assert closure or continued revocability. Verify a token one day past `exp` remains listed and revocable while the 60-day policy accepts it.

### R3 — P1: Delayed client cleanup can deactivate a different, unrevoked session

**References:** [re-auth updates session ownership](/Users/brien/urnetwork/server/session/REVOKE.md:138), [cleanup reuse](/Users/brien/urnetwork/server/session/REVOKE.md:513), [existing lock predicate](/Users/brien/urnetwork/server/model/network_client_model.go:1163), [existing update predicate](/Users/brien/urnetwork/server/model/network_client_model.go:1244), [task payload](/Users/brien/urnetwork/server/model/network_client_model.go:1462).

The proposed mutable `network_client.session_id` and reuse of the existing client-removal path do not compose safely as described. Revoke A can select client C for removal, then session B can re-authenticate C and change its stored session id before the cleanup runs. The existing helper locks by client and network only, and then deactivates those IDs. The background task also stores only IDs. A lock prevents simultaneous writes; it does not establish that C still belongs to A. Cleanup for A can thus disable B, potentially long after the revoke request.

The reverse ordering also needs coverage: a client transaction can extend A in Redis, remain uncommitted while revocation selects its clients, and commit after that selection. Its A token will be rejected by the marker, but the promised database deactivation is missed.

**Change:** carry the revoked session identity into synchronous and queued cleanup; lock and re-check the current association before changing `active`. Specify the serialization or reconciliation rule for client mints that commit after the first cleanup pass. A session-aware cleanup task is needed; blindly passing a captured ID list to the generic task is insufficient.

**Regression:** pause A's cleanup after candidate selection, re-authenticate C under B, then resume the synchronous and queued variants. B and its client must remain active while A's credentials fail. Separately pause an A client mint before SQL commit, finish revoke, release the commit, and assert eventual cleanup according to the chosen protocol. Use barriers, not sleeps.

### R4 — P1: Redis's origin check does not make auth-code revocation and SQL cleanup atomic

**References:** [origin check](/Users/brien/urnetwork/server/session/REVOKE.md:205), [auth-code deactivation](/Users/brien/urnetwork/server/session/REVOKE.md:519), [atomicity claim](/Users/brien/urnetwork/server/session/REVOKE.md:546), [24-hour code limit](/Users/brien/urnetwork/server/model/auth_model.go:1677), [code insertion](/Users/brien/urnetwork/server/model/auth_model.go:1791), [code consumption and subsequent mint](/Users/brien/urnetwork/server/model/auth_model.go:2018).

`create` serializes session creation against a marker, but `auth_code` insertion and deactivation happen in Postgres. A code-create request can pass authentication, pause, and commit its code after revoke has finished deactivating the existing codes. Checking the origin when that code is redeemed blocks it only while the origin marker exists.

The code can outlive that marker: near the end of a session's 60-day grace, a still-accepted token can create a code valid for another 24 hours, while its revocation marker may have only seconds remaining. After the marker expires, the late-inserted active code can create a fresh independent session. A process failure between the Redis revoke and SQL cleanup can leave the same state. Neither a successful Redis retry nor an `ALREADY` response proves cleanup completed.

Adding Redis to auth-code login also creates a new failure boundary: the current implementation consumes the last use in SQL before signing. If session creation fails afterward, a retry may find the code already gone. The desired retry result needs to be designed, not inferred from harmless duplicate `eid` increments.

**Change:** specify a recoverable revocation operation covering the marker, client/code cleanup, and completion status. Serialize code insertion with creator revocation or preserve an authoritative creator fence through every outstanding code's `end_time`, including late commits. Define whether code consumption and a failed mint can be resumed, and keep session IDs stable across callback/operation retries. Already-revoked retries must finish pending cleanup rather than skip it.

**Regression:** pause code creation between its authorization and SQL insert, revoke a creator close to its terminal acceptance deadline, then commit the code. Advance beyond the creator marker but before code expiry and assert redemption fails. Inject failure after Redis applies revoke and before SQL cleanup; retry and verify cleanup completes. Inject an applied-but-unacknowledged create EVAL and a Redis outage after last-use consumption; verify the documented retry result and that no extra sessions are created.

### R5 — P1: Release 1 must preserve the entire mint invariant, not just the claim

**References:** [mint invariant](/Users/brien/urnetwork/server/session/REVOKE.md:237), [rollout](/Users/brien/urnetwork/server/session/REVOKE.md:721), [client refresh creates fresh claims](/Users/brien/urnetwork/server/controller/auth_controller.go:641), [network refresh](/Users/brien/urnetwork/server/controller/auth_controller.go:737), [child claims](/Users/brien/urnetwork/server/jwt/by_jwt.go:762).

During the release-2 rollout, a release-1 server can receive a tagged token. The listed release-1 obligations guarantee carrying the session id and checking revocation, but do not explicitly require extending the Redis score before signing. Copying the id while minting a later `exp` breaks the proof in §4.3: revocation retains the marker only through the old score, and the newer token can become valid again after that marker expires.

Release 1 also does not include the live-connection registry and kick watcher. A tagged connection admitted there during the rollout cannot receive the promised kick until that instance is replaced or drained. “Every auth path has checked since release 1” establishes admission protection, not live-connection protection.

**Change:** release 1 must enforce check-and-extend for every mint that already has a session id, independently of the switch that creates new session IDs. Either install dormant connection tracking/kicks there too or deploy release 2 everywhere before separately enabling minting and revocation. Define the oldest permitted rollback version after activation, and retain these guarantees in it.

**Regression:** mint on release 2, refresh the network token and derive a later-expiring child on release 1, revoke, and verify refusal through both new credentials' full acceptance horizons. In particular, test the refreshed network token and its ability to create a new client so old-client deactivation cannot mask an expired marker. Open a tagged connection on the oldest supported instance and revoke it during a rolling deployment. Repeat with notifications disabled and through the permitted rollback.

### R6 — P1: Existing proxies and legacy client rows have no complete migration path

**References:** [mandatory signing guard](/Users/brien/urnetwork/server/session/REVOKE.md:120), [proxy carry rule](/Users/brien/urnetwork/server/session/REVOKE.md:141), [nullable column](/Users/brien/urnetwork/server/session/REVOKE.md:549), [current proxy loader](/Users/brien/urnetwork/server/jwt/by_jwt.go:921), [proxy signing](/Users/brien/urnetwork/server/proxy/proxy_device.go:775).

Every existing client initially has `network_client.session_id = NULL`. A hosted proxy then loads that NULL value and signs a token, but the new guard permits missing IDs only at the internal exempt sites. Thus “carry the stored id” is incomplete for the existing fleet. The current loader constructs a fresh parent with a new `CreateTime`; simply deriving from that temporary parent would create a different session on each restart instead of recovering the original sign-in.

Similarly, the legacy client-refresh path derives a session id for its JWT, but the plan does not state how that existing client's row is associated with it. Without that update, the token can be tracked while its client is absent from the session's device list and cleanup query. The retained `register-client-v1` path also needs an explicit association rule when it issues a token for an already-existing client.

**Change:** define a migration for NULL proxy/client associations, covering concurrent restarts and first refreshes without overwriting a newer session association. Either persist a documented independent legacy-proxy session or use recoverable historical lineage; do not infer lineage from a newly fabricated timestamp. If the user's new root-client fallback is intended to admit these proxies during migration, explicitly adjust the signing guard and mint a root derived from the existing client hierarchy. Enumerate every database association writer alongside every JWT mint. Also specify how API-key-created children are grouped: the unsigned request identity is not a historical sign-in session.

**Regression:** start and restart a pre-feature hosted proxy whose column is NULL after enabling the guard, including concurrent starts. Assert a stable, revocable association and a successful bootstrap. Refresh a pre-feature client and retry retained registration; verify its list membership and revoke cleanup. Race first association with a new-session re-auth and ensure the newer association survives.

### R7 — P2: `eid` expiry invalidates the “highest event_id” protocol

**References:** [32-day counter lifetime](/Users/brien/urnetwork/server/session/REVOKE.md:183), [counter comparisons](/Users/brien/urnetwork/server/session/REVOKE.md:428), [client keeps the highest value](/Users/brien/urnetwork/server/session/REVOKE.md:446), [screen refetch rule](/Users/brien/urnetwork/server/session/REVOKE.md:564).

An active network can have no create/revoke/expire operation for 32 days while refreshes keep its sessions alive. Its counter then expires. A client that has observed, for example, event 20 ignores the next incarnation's events 1 through 20. Forwarding the expiration as zero does not help a highest-only consumer. The corrective poll reads the same reset counter, so it cannot repair this. Foreground refetch or optional polling may mask the stale screen but does not fix the protocol.

**Change:** carry an epoch plus counter, or define an explicit reset/incarnation event and client behavior. Keeping the counter alive while sessions exist is useful but is not sufficient by itself to handle normal empty-network expiry and recreation. The list response and notification state need a common reset contract.

**Regression:** observe a high event id, explicitly expire/delete `eid` while leaving session state alive, create or revoke a session, and deliver both event-driven and corrective updates. Assert the already-open screen refetches once for the new incarnation; delayed frames from the previous incarnation must not roll state back.

### R8 — P2: A two-second retry budget does not bound the current Redis socket calls

**References:** [proposed budget](/Users/brien/urnetwork/server/session/REVOKE.md:375), [ordinary pool timeouts](/Users/brien/urnetwork/server/redis.go:101), [context-timeout option](/Users/brien/urnetwork/server/redis.go:152), [pool construction](/Users/brien/urnetwork/server/redis.go:224), [existing deadline boundary](/Users/brien/urnetwork/server/redis.go:370).

The ordinary `server.Redis` client has 15-second read/write/pool timeouts, command retries, and `ContextTimeoutEnabled` false. Shortening the wrapper's retry budget or adding a two-second context alone does not give the proposed authentication dependency a two-second socket deadline. This also matters when validation runs while a Postgres transaction owns a connection.

**Change:** choose a client path whose pool acquisition, command execution, routing, and retries obey the authentication budget, and preserve 503 classification at all callers. The existing `RedisWithDeadline` and its tests are useful references, but it currently allows only specific command shapes and performs a preflight PING; do not silently turn the promised one-round-trip check into two or assume its small cleanup pool is sized for all authentication traffic.

**Regression:** use the existing controlled Redis connection/deadline seams to stall command reads, exhaust the pool, and exercise cluster routing. Verify actual cancellation at the command boundary, bounded connection ownership, and retryable 503 outcomes from API, H1/H3, localclient, and parent-mint validation. A test of retry-option values alone is insufficient.

### R9 — P2: Requesting client refresh does not sign out an API-only network-token caller

**References:** [API-only view controller](/Users/brien/urnetwork/server/session/REVOKE.md:570), [proposed 401 handling](/Users/brien/urnetwork/server/session/REVOKE.md:593), [refresh entry point](/Users/brien/urnetwork/sdk/api.go:329), [refresh worker gate](/Users/brien/urnetwork/sdk/device_token_manager.go:373), [client identity requirement](/Users/brien/urnetwork/sdk/device_token_manager.go:79), [network rejection behavior](/Users/brien/urnetwork/sdk/api_network_credential_renewal.go:265).

`RequestJwtRefresh` wakes the client-token worker, which remains dormant unless the current JWT has both client and device IDs. It cannot produce the refresh-401/logout sequence for an API-only caller holding just a network JWT. The existing network-renewal rejection deliberately drops the network credential without firing the device logout pipeline. Therefore the proposed extra calls to `RequestJwtRefresh` do not implement the claimed end-to-end behavior for the explicitly supported no-device case.

**Change:** add a rejection path for the exact network credential/session generation that was rejected, including persisted state and logout notification where appropriate. Preserve the existing protection against a delayed response signing out a newer login. A rejected network credential must not automatically invalidate an unrelated current client session.

**Regression:** with a fake API and no device/client JWT, return 401 to the Sessions request or network renewal and assert one logout plus credential cleanup. Delay that 401 across a new login and assert the new login survives. Include client-plus-network credentials from different sessions and retain the 503/no-logout cases.

## Explicit security semantics to settle

These are not all accidental implementation bugs. They need to be stated accurately in the product contract and tested.

- **Legacy network tokens remain an independent way back in.** Decision 7 explicitly leaves untagged tokens unrevocable by session. Refreshing one does not invalidate the original signed credential. The original network token still works on ordinary requests; with expiry rejection off it can outlive the finite marker for the derived session and later recreate that same session. Auth-code creation from an untagged credential can also have no recorded origin. The user's client-root/ancestor fallback improves legacy client invalidation after client removal, but a network JWT has no client ancestor and is unaffected by that fallback. Thus “this legacy session was revoked” does not mean its pre-upgrade network credentials were cut off. Either accept and clearly disclose this migration limit, require credential rotation, or define a bounded legacy retirement/fence. Test the original token both while the derived marker exists and after it expires. This is a consequence of the stated legacy policy, not an undiscovered Redis failure.

- **Legacy derivation groups lineages, not necessarily sign-ins.** The plan itself observes that auth codes copy `CreateTime`. Current [code creation](/Users/brien/urnetwork/server/model/auth_model.go:1791) and [code login](/Users/brien/urnetwork/server/model/auth_model.go:2130) confirm that two independent pre-upgrade logins can have the same network, user, and creation time. They derive the same session id, so revoking one revokes both. Retaining this heuristic is possible, but the UI and policy must describe a legacy lineage group instead of promising exact historical sign-in identity. Test a root plus multiple auth-code logins before upgrade, not only one root and its clients.

- **Revoke-others currently has snapshot semantics.** Reading candidates in Go and revoking chunks leaves room for a candidate session to redeem an auth code into a new independent session after the candidate snapshot but before its chunk is revoked. The new session is absent from every chunk and survives. Decide whether the action means “these snapshot entries” or an account-wide cutoff with a single authorization fence. For the stronger meaning, minting and bulk revocation need a shared network generation/fence or an equivalent protocol; repeatedly scanning until empty is not a bounded solution. A deterministic test should pause after the snapshot, create the independent session, then complete revocation. Fresh password/SSO logins after the chosen cutoff are a separate policy question.

- **Password reset cannot yet promise to close every connection.** The [reset paragraph](/Users/brien/urnetwork/server/session/REVOKE.md:541) revokes tracked sessions, but legacy connections have no session marker to check, and evicted sessions are also absent. The existing `credential_change_time` check protects subsequent authentication; it is not a check on already-open connections. Either add a network/account invalidation signal covering these connections or narrow the claim and document the migration limit. Test a legacy H1/H3 connection held open across reset.

- **Accepting Redis marker loss is a conscious residual risk.** Open question 16 already accepts that loss can restore authority. This review does not treat persistence or replication as a proof against that risk, nor silently replace the chosen denylist with a durable allowlist. The failure cases in R1–R6 occur without needing to assume marker loss and require separate treatment.

## Additional implementation requirements

- **Repair the index protocol.** [Create's `ZADD NX`](/Users/brien/urnetwork/server/session/REVOKE.md:213) cannot lower an existing review time if an earlier-expiring mint finishes later. A sweeper can also read an empty network, race a create, and then remove the newly needed index entry. Define a lower-if-earlier writer and protection against stale sweeper updates/removals. Be explicit whether review time means raw `exp` or `exp + grace`: repeatedly returning a raw expiration that is already in the past schedules the same still-retained sessions every sweep throughout the grace period. Test these interleavings and a failed cross-slot index write. This is maintenance correctness, not by itself proof of an authentication bypass.

- **Make shared-key expiry monotonic.** The table says `z` lasts through the maximum score plus grace, which is the right invariant. The scripts must preserve the maximum across *all members* and the previous key deadline, not apply the touched member's deadline to the shared set. Test touching the earlier-expiring of two sessions after the later one, and retrying an old mint after a newer touch. The member-score test in §11 does not prove the key-TTL property.

- **Use a database index matching the new predicates.** The cited [top-level index](/Users/brien/urnetwork/server/db_migrations.go:3313) contains only `network_id` for active rows with `source_client_id IS NULL`; it does not index `session_id` or all descendant clients. Define whether the response intentionally shows only top-level clients, ensure revocation covers descendants, and add an appropriate network/session index for the intended list and cleanup queries. Follow the repository's concurrent-index rollout convention for this large table.

- **Define observable timing and labels.** §7 provides eventual kicks with a nominal corrective interval up to 72 seconds before command/scheduling delay; it does not provide immediate transport revocation. Register first and re-check before serving to close the auth-check/registry race, and test revocation between those steps. `mint_ms` records token minting, not last account/network activity, so label it accurately or define a separate bounded activity update. The current generic 401 is insufficient to distinguish “signed out from another device” from password reset, removed account, or token expiry; use a generic message unless a trustworthy cause is retained.

- **Verify the actual data-plane cutoff.** A close-reason test proves the platform transport closes, not that an uncooperative credential holder loses every established path. The connect code deliberately preserves P2P stream state across resident migration ([stream reset](/Users/brien/urnetwork/connect/transfer_stream_manager.go:400)) and has explicit retirement paths for disconnected peers ([peer retirement](/Users/brien/urnetwork/connect/transfer_stream_manager.go:287)). Add an integration scenario with established relay and P2P traffic, no voluntary SDK logout, and revocation on another server. Verify the honest remote endpoints stop traffic within the stated bound. This review has not established a P2P bypass; it identifies an unproven part of the promised cutoff.

## Readiness and verification scope

Retain the session-id architecture, the user-directed session/root/ancestry precedence with unconditional own-client and account requirements, centralized validation, same-slot serialization, fail-closed/503 distinction, explicit mint inventory, and deterministic-test approach. Resolve R1–R6 and choose the legacy/bulk/reset guarantees before implementation commits lock in conflicting semantics. R7–R9 and the index/deadline/notification tests should be included in the release acceptance criteria. The package move can proceed separately with behavior-preservation checks.

I read the complete plan and the relevant current server, connect, and SDK authentication, minting, client lifecycle, transport, Redis, and token-renewal code. This review changed only `session/REVOKE-REVIEW.md`; it made no implementation or plan edits. No executable regression tests were run: the proposed session model, routes, scripts, and protocol message do not exist yet. Regression scenarios above are requirements for the eventual changes, not claims of passing coverage. I did not validate deployed Redis configuration, fleet flag state, persistence/failover behavior, application UI implementations, or end-to-end P2P termination.

Review inputs:

- `session/REVOKE.md` SHA-256: `a92ceeb9d54fdfce301e2f3ad30a3612076192dbdf95c03157dbeafe1686cb5d`.
- Server initial HEAD: `110f49d8527d81e293e693e623955f8970aa4b46`; final checked HEAD: `269acffb57b315e7ee9446d9155a8858469e5ecf`. The shared checkout advanced externally during review. I checked the intervening diff of the principal reviewed server files: only `jwt/by_jwt.go` and `localclient/authority.go` changed among those paths, and incorporated their new `ValidateByJwtStateForParent` boundary. This is not a claim to have reviewed every unrelated intervening commit.
- Connect HEAD: `64e433f47cb5ac9f32e71a5b6d5aff659bc00623`.
- SDK HEAD: `f2aa25f6af5aee4780cad2e67c0e25a6d2364495`.

Source links use the checked-out file layout and current line locations; they may move as implementation begins. The plan's SHA-256 was verified unchanged after writing the review.
