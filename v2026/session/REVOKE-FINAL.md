# Session revocation — final implementation plan

Status: implementation plan, 2026-10-09. No implementation is claimed by this document.

This plan supersedes the alternatives and open questions in [REVOKE.md](REVOKE.md) and resolves [REVOKE-REVIEW.md](REVOKE-REVIEW.md). It covers the server, connect protocol, API, SDK and bindings through `sdk/client_session_view_controller.go`. App UI work belongs to [REVOKE-UI-FINAL.md](REVOKE-UI-FINAL.md), for a separate developer. The source proposal and review remain unchanged.

## 1. Product and authorization contract

A session identifies a sign-in, represented by a network JWT with no `client_id`. Its client JWTs carry the same `session_id`; client rows are not separate sign-ins. Auth-code redemption creates an independent session. Device adoption creates its own session. Internal probes remain explicitly exempt from session registration.

The API lists every non-revoked tracked session whose acceptance horizon has not ended. It supports revoking one session, including the current session, and all other tracked sessions. It never silently evicts an accepted session from the inventory. Last-used time, location, device type and app version are included as typed metadata.

Every successful JWT acceptance retains signature, audience, lifetime, network/admin identity and `credential_change_time` validation. A client JWT must additionally identify an existing, active client in the authenticated network with the existing device binding. Caller-specific ownership checks remain mandatory. The additional revocation/lineage check is exactly:

| JWT authority | Additional check |
|---|---|
| `session_id` present | Check that session's revocation marker. No root or ancestry fallback, including on Redis failure. |
| No session ID, `root_client_id` present | Require that root client to exist, belong to the same network and be active. No recursive fallback on rejection. |
| Neither claim, own client has `source_client_id` | Recursively require active same-network ancestors through the topmost client. |
| Neither claim, no client parent | Only the common requirements. A network JWT has no own client row. |

Put this combined logic in `ValidateByJwtState`, `ValidateByJwtStateInTx` and `ValidateByJwtStateForParent`. The last helper must retain its direct-parent ownership predicate. An already-revoked session may be rejected before borrowing Postgres; successful Redis validation never replaces the SQL requirements.

`root_client_id` identifies the topmost **database client row**, whose `source_client_id` is NULL. It is distinct from the network JWT owning the session. The JWT names its own `client_id`; its parent is the database's `source_client_id`.

The same-network, acyclic graph established by existing mint construction is trusted. Check missing rows and network membership and use a bounded recursive query for defensive failure, with a documented high depth limit of 1,024. Exceeding the limit refuses the operation; it never accepts a partially checked chain. This is not a request to redesign graph construction.

The requested precedence intentionally has different removal semantics. For `A → B → C`, with A and C active and B inactive, the legacy recursive branch rejects C. A valid session branch or a root branch naming active A can still accept C. Removing a client does not introduce a recursive cascade. Revoking a session invalidates all JWTs carrying that session ID and separately deactivates currently associated client rows. These policies must be tested and documented as distinct actions.

## 2. Claims, lifetime and compatibility

Add nullable `SessionId` and `RootClientId` fields with JSON names `session_id` and `root_client_id`. Every newly signed client credential has a root ID, including internal client credentials. Network JWTs have no root client ID.

- A new top-level client uses its own client ID as root.
- A child inherits the validated parent's root. If absent, resolve its actual database ancestry.
- A network credential supplying a source client must validate that source and resolve its database root; a supplied ID is not proof of ancestry.
- Refresh and reissue preserve the existing client's actual topology, including when another network session authorizes reissue.
- Topology resolution and legacy ancestor-activity authorization are separate helpers: computing a root must not silently add recursive activity checks to the session branch.
- A parent-copy constructor preserves network/user identity, `CreateTime`, session ID, roles and principal; fresh registered claims still receive their own `jti` and expiration. Live name/Pro refresh behavior stays intact.

Unsigned API-key and MCP OAuth request identities remain unsigned and have no session claim. Signing requires a registered session and, for client tokens, a resolved root, except named internal producer exemptions. Use a registered-mint helper/proof internal to the package; direct production signing must not bypass registration. Never use a client-supplied flag to select an exemption.

### One fleet-wide horizon

For session-v1 credentials, require `exp` and define:

```
credential_accept_until = exp + 60 days + clockLeeway
session_accept_until    = max(credential_accept_until for registered mints)
retain_until            = session_accept_until + retention_margin
```

Keep normal new token expiration at 30 days after issuance. `retention_margin` is at least five minutes and must exceed the measured maximum server/Redis clock disagreement plus expiration rounding. Session-tagged and newly root-tagged tokens always enforce this terminal horizon, regardless of `reject_expired`. Strict instances may reject sooner at `exp + clockLeeway`; they must retain revocation state through the longer fleet horizon. No process shortens retention because its local expiration flag changed. Clock bounds are an operational release prerequisite; exceeding them triggers an alert and suspends new session minting until corrected.

Store absolute millisecond deadlines and pass explicit `now_ms` to scripts/tests. Use ceiling conversion for retention and an exclusive acceptance boundary: `now >= accept_until` rejects. Listing and pruning use the acceptance horizon, not raw `exp`. A token accepted one day after nominal expiration remains represented and revocable. A future shortening of the policy requires its own fleet/rollback migration after all longer-lived credentials have aged out.

### Legacy limits, deliberately retained

Untagged JWTs keep their existing parser compatibility. Request authentication does not invent a session ID. Their client invalidation improves through the root/recursive rules above, but an untagged network JWT has no client ancestor and cannot be revoked by a session marker.

At the next refresh or client mint, derive the legacy session ID from the documented v1 domain, network ID, user ID and canonical UTC `CreateTime` nanoseconds, retaining the original time bits and 80 hash bits. Allocate a stable per-operation random ID if creation time is absent. Register kind `legacy`; never reuse `auth_session_ids`.

This is a legacy lineage group, not reliable historical sign-in identity: old auth-code logins copied `CreateTime` and may join the same group. Upgrading does not invalidate the original signed token. Its ordinary requests remain usable, and after the derived marker's finite lifetime it may derive that group again. The API explicitly returns `legacy_coverage: "partial"`; neither the API nor UI promises that revoke-others removes untagged credentials. Credential rotation remains the account-wide fallback. No mandatory legacy logout is added in this project.

## 3. Typed client information and last-used metadata

Use concrete types, not arbitrary maps or JSON strings at application boundaries. JSON is the Redis/header/wire encoding of those types.

```go
type ClientInfo struct {
    Version    int    `json:"v"`             // schema version, currently 1
    DeviceType string `json:"device_type"`
    AppVersion string `json:"app_version"`
    SdkVersion string `json:"sdk_version,omitempty"`
}

type SessionLastUsed struct {
    UnixTime    int64  `json:"unix_time"`      // UTC Unix seconds
    City        string `json:"city"`
    Region      string `json:"region"`
    Country     string `json:"country"`        // GeoLite2 English country name
    CountryCode string `json:"country_code"`   // lower-case ISO alpha-2, or empty
    DeviceType  string `json:"device_type"`
    AppVersion  string `json:"app_version"`
}
```

`NetworkSession.LastUsed` is `*SessionLastUsed`, JSON `last_used`, explicitly null when no observation is known. An observed use has all fields; unknown location/version strings are empty and unknown device type is `unknown`. An SDK mirror exposes typed fields/getters through gomobile and other bindings. The shared controller returns this typed value directly. Do not add a second independently maintained timestamp with ambiguous units.

### X-UR-ClientInfo

API and connect H1/H1+ requests send one optional header, for example:

```
X-UR-ClientInfo: {"v":1,"device_type":"android","app_version":"1.2.3","sdk_version":"1.2.3"}
```

Limit the encoded header to 512 bytes. Bound version strings to 64 UTF-8 bytes and device type to the enum `android`, `ios`, `macos`, `windows`, `linux`, `web`, `cli`, `server`, `unknown`. Reject duplicate fields, duplicate header values, invalid JSON and unsupported versions as metadata; authenticate normally with unknown metadata. Unknown object fields are ignored for forward compatibility. Invalid fields do not become high-cardinality metric labels or raw log content.

Add typed `ClientInfo` to `session.ClientSession`, populated once from request headers. The header is advisory presentation data, not authentication, device binding, entitlement or abuse-policy evidence. It contains no device identifier or raw address. Old clients can omit it. A valid new `app_version` takes precedence; otherwise the existing connect `X-UR-AppVersion`/`Auth.app_version` provides bounded fallback. Do not infer an authoritative device type from User-Agent.

Connect's H3 path is raw QUIC and authenticates with `protocol.Auth`, not an HTTP header. Add `string client_info = 8` containing the same bounded JSON (8 is free in the inspected schema; verify before allocating). Carry it in legacy H1 first-frame auth, H3 and DNS-carried H3. Add it to `connect.ClientAuth` and all auth snapshots/copies, including datagram negotiation. Retain the old app-version field for compatibility. The H1 header and frame representations must normalize through the same parser; header-auth mode uses the header, frame-auth mode uses the frame, without mixing generations.

The SDK supplies this metadata through its API GET/POST transports, network renewal, client refresh, auth-code requests, remote RPC transports and connect auth. Add a typed API/device configuration setter so embedding apps supply their app version and device type; SDK version can be filled by the SDK. Defaults remain unknown. The UI developer receives this setter as a documented integration requirement; this plan does not implement app screens.

### Recording last use

Build one immutable `SessionLastUsed` from the successfully authenticated `ClientSession`: server-observed time; GeoLite2 lookup of `ParseClientIpPort()`; parsed client information. API address extraction already uses the ingress-owned `X-UR-Forwarded-For` contract. Connect must build the equivalent observation context from its authenticated transport address and parsed ClientInfo, using the existing trusted address path. Never accept location or time supplied in the header, and never store raw IP. Hash-only task sessions have no recoverable location; background tasks do not count as user activity.

Record completed sign-in, successful JWT request authentication and successful connect admission. Internal periodic authorization checks and mint maintenance do not manufacture usage observations. An open connection's packet traffic does not update this field in v1; it means last observed authenticated use, not continuous traffic or device presence. A session aggregates uses from all its child credentials; location/device/version describe the latest retained observation, not every device in the session.

Use a separate string key `T u:<sid>` containing the typed JSON. Its TTL ends at **session_accept_until**, the effective session expiration including accepted grace and descendant mints. Minting extends an existing observation's TTL when this horizon grows, without changing its observation time or creating a fabricated use.

After common authentication succeeds and any owned SQL connection is released, a bounded best-effort sample script checks the revocation marker, current live membership and future horizon. It updates the entire JSON atomically only for a newer observation; a delayed older location cannot overwrite a newer one. It does not extend authority, restore membership, or recreate metadata after revocation. Revoke deletes the use key in the same script that writes the marker.

Throttle attempts per process/session to once per 60 seconds with a bounded cache. The script also limits successful writes across processes to one per 60-second observation interval, retaining the first eligible newer complete observation; equal timestamps retain the existing object. These two throttles can combine: under healthy storage and ongoing requests, recording can lag observed use by up to 120 seconds plus the write budget. The screen adds its own refresh interval. A suppressed one-off use can likewise be up to 120 seconds newer than the retained observation. Test the cross-process phase offset, not just one process. Use an independent 100ms write budget, no unbounded retry queue and no per-packet work. Failed optional samples increment a bounded metric and preserve request success; freshness has no outage guarantee. Required revocation reads still fail closed with 503. Last-used writes do not increment the sessions change counter.

## 4. Redis authority and inventory

All network-local keys share `T = {ns_<network_id>}`. Scripts declare every key and pass integer milliseconds. Redis is a denylist authority; persistence/replication loss that removes acknowledged markers can restore access. This is an accepted residual risk, not a guarantee provided by replication. The SQL operation journal below recovers application crashes and cleanup, not a durable allowlist consulted on every request.

| Key | Value and expiration |
|---|---|
| `T z` | Live-session sorted set; member sid, score session_accept_until. |
| `T held` | All registered IDs, including revoked ones; score retain_until. Bounds retained tombstones and reserves capacity. |
| `T s:<sid>` | Bounded hash: kind, create time, last mint time, nominal maximum token expiry, origin session ID, sign-in metadata. Expires at session_accept_until. |
| `T u:<sid>` | Typed last-used JSON; expires at session_accept_until. |
| `T r:<sid>` | Revocation marker; expires no earlier than retained maximum horizon. |
| `T generation`, `T eid` | Notification incarnation and positive counter. Extend with shared state; initialize/reset together. |
| `T op:<operation_id>` | Idempotent revoke receipt, including exact targets/result/cutoff. Retain through target retention and at least the 24-hour response replay window. |

Shared-key expiry is the maximum required by **all members**, any outstanding receipt and its previous absolute deadline. Never set `z`, `held` or event-key TTL from just the touched member. A shorter retry must not shorten any score or deadline. Shared keys may survive briefly after becoming empty and expire naturally. Reads prune/filter ended sessions even when the sweeper is late.

Default limits are 1,000 live sessions and 10,000 retained IDs per network. Existing-session refresh is allowed at capacity; a new ID is refused with `409 session_limit_reached` or `409 session_retention_limit_reached`. No accepted live entry or marker is evicted to make room. Expired entries release capacity. Retained-ID capacity explicitly covers repeated create/revoke cycles and bulk markers; operation receipts and SQL journal retention have separate bounded retention and existing account rate limits. Instrument actual memory rather than treating the proposal's size estimates as measured figures.

### Mint script

Allocate session IDs, operation IDs, `jti`, registered timestamps and metadata before retryable callbacks. Every successful signing path runs check-and-register/extend before returning its signed credential, including servers where new-session creation is disabled but the parent is already tagged.

The same-slot script checks its own revocation marker, and, **only for initial auth-code redemption**, the creator marker. It prunes elapsed membership/reservations, enforces capacity for genuinely new IDs, takes maxima for horizons, writes bounded metadata, extends an existing last-used TTL, and increments the event counter only on membership creation/expiry. It returns the authoritative horizons and inventory revision. A missing member may be restored with kind `restored` using the presented credential's proven horizon, subject to capacity; a marker always wins. Restoration is not proof of full inventory recovery after Redis loss.

Mint and revoke serialize in Redis: an accepted mint extends the recorded horizon before a revoke reads it, or a prior marker refuses the mint. Signing/response delivery after a concurrent revoke is harmless because the issued JWT carries the revoked ID. Uncommitted or failed SQL mints may leave a registered but never-delivered session; it remains bounded, visible and revocable until expiration. Do not delete such a reservation on an ambiguous failure and risk erasing a successful concurrent mint.

### Single and bulk revoke scripts

Single revoke atomically reads the live/reserved horizon, writes the marker, removes live metadata and last-use data, retains the reservation, increments the event counter and saves an operation receipt. Replaying the same operation ID returns its saved outcome without shortening TTLs. A different operation finding a marker returns `already_revoked` and still arranges cleanup. An unknown/expired ID returns 404 without revealing other networks.

Revoke-others uses **one bounded atomic script**, not chunked snapshot semantics. Read the inventory and its generation/revision, declare all target keys, then compare the revision and exact membership before applying. Retry a changed snapshot within the request budget. The live cap bounds the script to 1,000 sessions; benchmark its worst case before enabling the endpoint. SQL lifecycle serialization in §5 excludes concurrent production mints while assembling/applying this operation; revision checks also cover pruning/maintenance races.

The script validates the kept session, marks every other live session and stores the exact target set in the operation receipt atomically. Its execution is the cutoff: an independent session created before it is included; a fresh password/SSO/etc. sign-in afterward is allowed. Auth-code redemption after the cutoff from a targeted creator is refused. An already-redeemed independent session is included if present at the cutoff. The action cannot revoke untracked legacy credentials or stop someone who still knows a reusable sign-in secret from signing in again.

## 5. SQL serialization, associations and recoverable operations

Add nullable `network_client.session_id` and a stored credential-lineage `session_create_time`. Add `auth_code.origin_session_id`. Add a bounded, durable revocation-operation journal with operation ID, network, action, actor identity/credential epoch, request fingerprint, status, exact target IDs, Redis cutoff/result, cleanup progress and retention deadline. Add auth-code redemption receipts as described below, and a per-network session-index repair outbox. These records serve retry/maintenance workflows; they are not a new per-request SQL session lookup.

Build concurrent partial indexes matching actual predicates: `(network_id, session_id, client_id) WHERE active AND session_id IS NOT NULL`, plus `(network_id, origin_session_id)` for active auth codes. The existing top-level-client index is insufficient for descendants. Follow the repository's concurrent-index migration convention and validate readiness before queries rely on it.

### Lifecycle lock

All mint/association/auth-code write transactions acquire a shared transaction-scoped advisory lock for the network. Revocation enforcement, password rotation and network deletion acquire its exclusive counterpart. Acquire this lock before client/auth-code rows and keep a fixed sorted row-lock order. Use READ COMMITTED: after waiting for the advisory lock, the next query must see the preceding transaction's commit. Do not rely on the current default repeatable-read snapshot taken before the lock wait.

The shared holder validates current account/caller authority, performs any client/code writes, registers the resulting horizon in Redis under its bounded budget, and commits before returning a credential. SQL retries reuse immutable mint identity/claims and redo authority checks. Registration errors must roll back SQL writes, not merely return from a transaction callback that then commits. Redis may have succeeded despite an error; replay the same identity.

An exclusive revoker waits for earlier shared transactions to commit, applies the Redis marker(s), durably records the result and queues cleanup before releasing the lock. Later transactions see the marker and cannot commit new associations/codes from that revoked session. This closes the late-commit cleanup hole without holding a network lock for the entire descendant cleanup job. Never acquire a second Postgres pool connection while one is owned; use the `InTx` helpers.

### Revocation operation lifecycle

1. Authenticate/authorize, atomically reserve the account action limit and insert a durable `prepared` operation before mutating Redis. Reuse the caller's operation ID; a different request fingerprint is 409.
2. Under the exclusive lifecycle lock, first read any Redis operation receipt, then verify an unapplied operation is still applicable to its recorded account credential epoch. An unapplied request from before password rotation must not later revoke new sign-ins. Revalidate the actor for initial execution; an already-applied operation proceeds with cleanup even if that actor is now revoked. This ordering recovers self-revoke after Redis succeeded but its SQL result commit was interrupted.
3. Apply/replay the Redis script. Its receipt resolves an applied-but-unacknowledged result and supplies the exact bulk target list. Persist that result and enqueue a session-aware cleanup task transactionally. If the process dies after Redis but before SQL commit, the prepared row remains and recovery reads/replays the receipt.
4. Cleanup locks candidates and rechecks `(network_id, session_id)` before deactivation. Carry the target session identity in every synchronous/queued batch; never pass captured IDs blindly into the generic bulk-removal task. Deactivate codes with the same origin predicate. Checkpoint batches until no matching active rows/codes remain.
5. Mark complete only after cleanup. Retrying `already_revoked` resumes pending cleanup. Never delete an unfinished journal row; alert on age/backlog. Completed rows expire after their target retention/replay windows. Missing both an expected receipt and authoritative state after Redis loss is an operational error, not inferred success.

HTTP 200 means the Redis cutoff is authoritative and durable cleanup is queued; report `cleanup_pending` honestly. HTTP 202 means only that a durable request was accepted and enforcement is still pending; apps must not present that as completed revocation. Dependency errors with unknown outcome return 503 plus the stable operation ID; workers resume prepared operations and the client retries the same ID. An operation is not abandoned because its HTTP caller disconnected. Provide `GET /network/session-operations/{operation_id}` for authenticated network-scoped status/recovery.

### Rebinding and hosted clients

Explicit network-authorized re-auth may bind an existing active client to a new session under its row lock. Cleanup for session A must skip a row now bound to B. Ordinary client refresh and registration replay preserve their association; they may fill NULL conditionally but never overwrite a newer different binding. A conflict returns `409 client_session_changed` and requires explicit re-auth. The old session's JWTs remain governed by their own marker and the common own-client check; stored association is cleanup/list ownership, not a substitute JWT lineage check.

For a NULL hosted-proxy association, allocate and persist one independent `legacy_proxy` session under the client-row lock, with a stable credential lineage time sampled once from current authorized account state. Concurrent starts reuse the winner. Do not derive from the loader's newly fabricated `CreateTime`. Subsequent starts carry the persisted ID/time and resolve the actual client root; an inactive or revoked proxy cannot bootstrap a fresh session. A newer explicit re-auth wins over a stale bootstrap/refresh. Initial bootstrap is an acknowledged conversion of an existing hosted credential, not recovery of a provable historical sign-in.

API-key-authorized client creation gets a distinct `api_key_client` session per top-level client creation; descendants carry it. The API-key request identity itself is never signed or listed. Removing that session does not revoke the API key, which can authorize a later creation. MCP identities have the same unsigned-identity boundary; only explicitly authorized mint endpoints may create such client sessions. Persist these associations just like interactive clients.

## 6. Auth codes, reset, sign-out and deletion

Auth-code sessions are independent once redemption commits. `origin_session_id` is provenance and a **pending-code** authorization fence, never a continuing dependency on refresh of the redeemed session.

For tagged creators, code-create takes the shared lifecycle lock, checks the creator, and inserts the origin. Clamp the code's effective end time to the lesser of the requested/default maximum 24-hour duration and the creator session's current acceptance horizon; return the effective duration. A pending code therefore cannot outlive the creator's revocation coverage. Near terminal expiration, require refresh if no positive duration remains. The lock prevents a code insert authorized before revoke from committing after its cleanup snapshot. Redis origin checking at redemption still applies while cleanup is pending.

Redeem under the shared network lock and an auth-code row lock. Recheck code state, expiry, remaining uses, current account credential cutoff and origin marker. Allocate one independent session per redemption operation; register it before committing use consumption. A registration failure rolls back consumption. Store an immutable redemption receipt in the same SQL commit, keyed by a bounded client request ID and the hash of the high-entropy code, containing the session/claims/result. An HTTP retry with the same ID resumes the same redemption after last-use deletion; it does not consume another use or extend expiration. A conflicting payload is 409. Retain receipts until at least 24 hours after code expiration, then remove them. Old clients omitting a request ID retain at-most-once consumption per server request and may need a new code after an ambiguous lost response; updated SDKs always supply it.

A Redis registration followed by SQL rollback can leave an unused reservation, but cannot consume the last code use. A durable prepared redemption identity or deterministic server-keyed derivation from the code identity and request ID must preserve the same session across process retries; choose the durable prepared receipt with immutable claims, then mark it consumed in the use transaction. Never create a second session inside a retried callback. Replaying a committed redemption checks its own session/account validity; it does not newly depend on its former creator.

Pre-feature codes and codes created by untagged legacy credentials can lack an origin. They retain existing credential-time validation and the same explicit legacy limit; a per-session revoke cannot claim to cancel them. The maximum existing code lifetime is 24 hours. Fresh code-login results are tracked regardless of whether their origin was legacy.

Explicit app sign-out attempts current-session revoke best effort, then clears local credentials without waiting indefinitely. Offline sign-out alone does not claim server revocation. API-key callers have no current sign-in to sign out.

Password set/reset keeps its authoritative SQL `credential_change_time` update. Under the exclusive lifecycle lock it also journals revocation of existing tracked sessions and deactivates pending codes predating the cutoff. If Redis is unavailable after the password transaction commits, the password change remains committed and durable recovery completes session cleanup; do not roll back or misreport the credential change. Existing connection revalidation (§8) covers legacy connections as well. Fresh sign-ins after the rotation survive old cleanup because cleanup is epoch/session-specific.

Network deletion first commits loss of account/network authority and durable retirement work under the lifecycle lock. Only afterward remove session inventory, observations and other keys. Do not erase a denylist first while the account still authenticates. Connection revalidation observes missing ownership. Network IDs are not reused; deletion cleanup may discard that network's markers once SQL deletion is authoritative to all acceptance paths.

## 7. Deadlines, errors and maintenance

Add a dedicated authentication Redis path, based on the existing deadline-aware client's transport settings but **not** its small cleanup pool or preflight PING. One normal revocation `EXISTS`/`GET` is one command round trip. Configure context-aware socket, pool, dial and cluster-routing deadlines; disable hidden command retries. Allow only bounded explicit retries/redirects inside a total two-second budget, shortened by the caller's remaining deadline. Mutating scripts are replayable by operation identity. Do not assume wrapping the ordinary 15-second socket client in a two-second context provides this bound.

Map session-store dependency failures to `ErrSessionStoreUnavailable` and `ErrAuthUnavailable`: API, H1/H3, localclient, hosted authority, parent-mint transactions and taskworker callers must preserve retryable 503 semantics. Fix current catch-all conversions in localclient and parent validation. Confirmed inactive credentials produce 401; client tokens on network-only management routes produce 403. Optional last-use/notification/index writes have their own non-authoritative error handling and cannot turn a valid credential into a 401.

Do not cache absence of a marker across requests. Concurrent in-flight reads may be coalesced, provided a request does not reuse a completed pre-revocation result. A bounded positive revoked cache is an optional later optimization. After activation, disabling required revocation reads is not a supported availability fallback; disable new mints or management UI writes, or roll back only to a compatible enforcement version. Notification switches do not disable enforcement or corrective leases.

Use 32 maintenance index shards. Each network index write performs lower-if-earlier, not `ZADD NX`, and refreshes a per-network index revision token on that shard even when the score stays unchanged. The sweeper reads score/revision, reviews the network, then raises/removes the index entry only with a matching revision. Every writer attempt publishes a fresh revision token; an absent/recreated token fails an old compare-and-set. Index guard keys have bounded TTL and cannot reuse an expired revision. A create between review and index removal either defeats the compare-and-set or recreates the entry afterward.

The next review time is the earliest effective acceptance/retention deadline, not an already-passed nominal `exp`. Queue failed cross-slot publication in the SQL repair outbox committed with the mint/operation; retry it with a fresh revision. An outbox item is removed only after a successful repair for its current revision. A later old repair may schedule an unnecessary early review but must not raise a newer earlier deadline. Natural key TTLs and score filtering preserve authorization/list correctness independently of the index.

Schedule review and repair in taskworker's auth maintenance family, with fixed shard/network/batch/time budgets and checkpointed continuation. Add startup registration, targets, workload profile and WORKLOADS documentation. Maintenance uses explicit clocks and no keyspace-wide SCAN for normal work.

## 8. Notifications and active connections

Use the existing process-wide Redis key-event subscriber and corrective polling, with no per-client Redis subscription or application-level publish. Session create/revoke/expire changes increment the per-network counter; mint extension and last-used samples do not.

The protocol/list revision is `{generation, event_id}`, where generation is a random incarnation ID and event_id is positive. If either event key is missing, initialize both together to a new generation and counter 1. Extend their shared lifetime with session state; never interpret missing as an ordered zero. Add `NetworkSessionsChanged` carrying both fields and allocate the next free transfer message value (33 in the inspected proposal inventory; verify the actual enum before generation).

A hint from the same generation with a higher counter triggers refresh. A different generation triggers an authoritative list refresh, without ordering UUIDs or adopting that hint as a snapshot. Old delayed frames may cause an extra refresh; they cannot replace a newer snapshot. Corrective polling and subscriber reconnect/drop epochs also trigger refetch. Keep list refresh serialized/coalesced and bind results to the current credential generation and request sequence. Coalesce outward session hints to at most one per five seconds per network. Last-use freshness comes from explicit/foreground/visible-screen refresh, not a stream of activity notifications.

### Platform connection guarantee

Track every H1/H3 connection's network, user, session/root claims, own client/device, credential lineage time, acceptance deadline and authentication generation. Register before the final state recheck and publish admission only after it succeeds, closing the initial-check/registration race. A session event prompts immediate revalidation of affected local connections.

Every admitted connection, including legacy ones, has a renewable authorization lease of at most 90 seconds from the start of its last successful authoritative check, capped by its credential acceptance deadline when finite. Recheck at most 60 seconds apart, with bounded jitter no later than 72 seconds. The independent lease timer still closes the connection while a check is stalled; a late result cannot resurrect a closed generation. Rechecks include account/own-client state and the selected session/root/ancestry branch, so reset/removal and legacy account invalidation reach open transports. Batch/coalesce identical work without granting a fresh lease from stale cache results. This adds periodic state work, not per-packet queries.

After the revoke script's cutoff, new authoritative checks reject the session; checks already in progress have their normal ordering. Already admitted platform transports and their relay work retire within 90 seconds under functioning timer/scheduling bounds, normally sooner via events. Expiration closes them by their credential deadline. On dependency outage, lease expiry closes with a retryable-unavailable reason; it must not claim revocation or force SDK logout. Confirmed revocation uses the distinct session-revoked close cause; verify available protocol close codes before allocation rather than overloading unrelated reasons.

### P2P guarantee and release boundary

Closing the attacker's platform socket alone does not prove P2P cutoff. Required server retirement must identify stream/hop authorization generations created by the revoked connection and send authoritative retirement to the other endpoints. Updated connect endpoints must cancel the complete stream lifecycle, including P2P transports and retry/admission loops; migration/reset must not revive a retired generation.

For a bounded guarantee despite dropped control messages or an uncooperative revoked client, add renewable platform-issued stream authorization leases enforced by the honest endpoint. Leases name the stream and endpoint authorization generations, arrive only over authenticated platform control, and last at most 90 seconds. The platform renews only while both endpoint authorization leases remain valid and the stream remains authorized. The stream's local timer closes it without waiting for voluntary logout or a peer message. Thus the worst-case updated-endpoint P2P cutoff is 180 seconds from revoke: at most 90 seconds to invalidate the platform authorization, plus at most 90 seconds of a previously granted stream lease. Event-driven retirement is faster. Preserve this rule across exchange/resident migration and clock conversion; receivers use conservative local monotonic deadlines.

Negotiate stream-lease support. Do not establish new P2P paths involving tagged sessions where the receiving honest endpoint cannot enforce the lease; use relay compatibility. Existing unleased legacy P2P paths and two colluding endpoints are outside the bounded guarantee. Do not advertise universal P2P termination for them. The required integration gate uses established relay and P2P traffic, disabled notifications, another server performing revoke, and a revoked client that ignores logout/close instructions. A close-frame unit test alone does not satisfy this gate.

## 9. API contract

All routes are network-only in server and SDK route tables and AUTHZ1 documentation. Valid client credentials get 403. Authenticated API keys may list/revoke a specified session; OAuth requires the existing network-management permission. No caller supplies the authenticated network ID. Normal route auth and existing account admission limits remain in force.

### GET /network/sessions

```json
{
  "sessions": [{
    "session_id": "...",
    "current": true,
    "kind": "password",
    "create_time": "2026-10-09T12:00:00Z",
    "last_mint_time": "2026-10-09T12:00:00Z",
    "token_expire_time": "2026-11-08T12:00:00Z",
    "accept_until": "2027-01-07T12:00:30Z",
    "origin_session_id": null,
    "last_used": {
      "unix_time": 1791547200,
      "city": "Chicago",
      "region": "Illinois",
      "country": "United States",
      "country_code": "us",
      "device_type": "android",
      "app_version": "1.2.3"
    }
  }],
  "generation": "...",
  "event_id": 1,
  "current_session_id": "...",
  "legacy_coverage": "partial"
}
```

Return all live rows, bounded by the 1,000-session admission cap, sorted current first then descending known last-use time/create time with session ID as a stable tie-breaker. Do not silently paginate/truncate this complete inventory. Read membership, metadata, observations and revision coherently on the network slot; if a multi-command snapshot is used, verify revision and retry within a deadline. Atomic activity updates may appear at either side of the read without changing membership completeness. Missing optional metadata yields explicit unknown fields, never omission of the session. A tracked entry with a marker is filtered and repaired.

`current` compares the presented tagged network credential's session ID. Untagged and API-key callers have `current_session_id: null`; do not guess from IP, client ID, device or creation time. `last_mint_time` is not last activity. `token_expire_time` and `accept_until` have different, explicit meanings. The initial response intentionally omits unbounded descendant-client arrays and peer presence; the complete session inventory and typed metadata are the required UI contract. Client association columns still support exact cleanup and later device detail without changing authority semantics.

### POST /network/revoke-session

Body: `{ "session_id": "...", "operation_id": "..." }`. Updated SDKs generate an operation ID once per action and reuse it on retry. Return `{operation_id, status, session_id, revoked_count, cleanup_pending, generation, event_id}`. Status is `revoked`, `already_revoked` or `pending`; the HTTP status follows §5. Revoking the current session is allowed. Successful self-revoke can return 200 on the already-authenticated request even though later requests get 401.

### POST /network/revoke-other-sessions

Body: `{ "operation_id": "..." }`. The kept session comes only from the authenticated JWT. Require a tagged current network session: an untagged caller gets `409 session_upgrade_required` and must network-refresh first; an API key/OAuth identity without a sign-in gets `409 current_session_required`. Do not interpret a missing keep ID as revoke-all. Return the same operation/result fields plus `kept_session_id` and the exact newly revoked count. A fresh independent sign-in after the cutoff is allowed.

### GET /network/session-operations/{operation_id}

Return only operations belonging to the authenticated network, with `prepared`, `enforced`, `complete`, `cancelled` or `failed` state and the original result. It is a recovery surface, not a bypass allowing a revoked token to authenticate. A self-revoked caller may receive 401 while its durable worker still completes cleanup.

Malformed input is 400; invalid credential 401; wrong credential class 403; unknown session/operation 404; capacity/conflicting operation/upgrade-required 409; quota 429 with Retry-After; unavailable dependency 503 with retry advice. Preserve structured machine codes and avoid credential details in error messages. Confirmed revocation may expose a trustworthy `session_revoked` code; generic 401s use generic sign-in-required wording.

Reserve single-session revokes at 100/day and revoke-others at 20/day per network, atomically with the prepared operation. Same-operation retries are free; do not charge again after a lost response or let parallel check-then-record bypass limits. Release reservations for a confirmed no-op/not-found before enforcement; ambiguous/applied work keeps its reservation. Listing uses existing read/admission limits; no new quota that would prevent normal visible-screen polling is introduced.

## 10. SDK endpoint and view-controller contract

Implement typed `NetworkSessionInfo`, `SessionLastUsed`, session list/revision and operation result models, plus `Api.GetNetworkSessions`, `Api.RevokeNetworkSession`, `Api.RevokeOtherNetworkSessions` and operation-status recovery. Mirror server route authorization so account operations select a network credential. Add required client-information configuration and transport propagation from §3.

Create exactly **`sdk/client_session_view_controller.go`**, following existing controller/list/listener conventions and build restrictions such as `!ios_extension`. This is the final implementation layer in this plan; it contains everything app session UIs need without implementing any UI.

The exported contract must provide:

- API-only construction without a Device, and device-backed construction with session-change notifications; both share the same state machine.
- A typed current snapshot/list, current-session identity, complete `SessionLastUsed` metadata and the legacy-coverage indicator.
- `Start`, `Close`, `Refresh`, `RevokeSession(sessionId)` and `RevokeOtherSessions`; operation IDs and retry/recovery remain inside the controller.
- Immutable snapshot/change notifications and typed loading, refreshing, per-session action, bulk-action, pending-operation and error state. Keep the last successful list visible during retryable refresh errors and distinguish never-loaded from an empty list.
- Initial load, event/generation-triggered refetch, foreground refetch, explicit refresh and a 30-second visible-screen polling mode for API-only and last-used freshness. The app supplies visibility/foreground lifecycle. Polling stops when hidden/closed.
- Coalesced refresh requests, cancellation and stale-response protection tied to API credential generation and request sequence. A result for an old network/login must never update the new login's list. A revoke result racing a list read triggers a fresh authoritative list; it must not reinsert a just-revoked row from a stale response.
- A confirmed enforced result removes/refreshes the target; a 202 pending operation remains visibly pending until status confirms enforcement. Expose retryable failure without clearing credentials.

Use the existing gomobile list wrappers and listener subscriptions, DeviceLocal/DeviceRemote RPC forwarding, generated cgo/C#/JS surfaces and ABI baseline workflow where applicable. Export the typed metadata and controller, not serialized JSON for apps to interpret. Extend enum/message compatibility tests before regenerating bindings. The UI handoff uses these exact concepts; exact language binding spellings follow each generator's conventions.

### Credential rejection, including API-only callers

Do not rely on `RequestJwtRefresh` for a network-only caller: the existing client-token worker requires client/device IDs. Add a shared network-credential rejection path that captures the exact sent credential, store identity and generation, clears its persisted/in-memory state only if still current, and emits one appropriate logout/state notification outside auth locks. A delayed 401 cannot clear a newer login. Do not re-adopt a rejected credential from LocalState.

For API-only callers, a confirmed 401 from list, mutation, operation status or network renewal clears that current login and emits one logout. With a client credential from the same positively identified session, reject/refresh and clear the related credentials consistently. A network rejection must not automatically destroy an unrelated current client session: clear the rejected account credential, preserve the unrelated client/device, and notify that account sign-in is needed. Unknown legacy identity is not proof of sameness. Apply the same generation protection to successful self-revoke.

503, transport failures, rate limits, unsupported-feature 404 and conflicts never produce logout. A revoked transport can trigger client refresh; only confirmed credential rejection completes logout. A typed session-revoked cause may support a specific explanation; otherwise use generic sign-in-required messaging. Feature detection distinguishes a missing route from a real target-session 404 so a failed revoke does not hide the feature.

## 11. Producer and component checklist

| Producer/component | Required behavior |
|---|---|
| Apple/Google, wallet, password, verification, seedphrase and sign-up | New registered session per successful sign-in, stable IDs across callback retries; record successful sign-in observation. |
| Auth-code create/login | Origin fence and bounded code horizon; independent registered redemption with durable retry identity. |
| Network refresh | Carry/derive session, extend horizon before signing, preserve credential lineage time. |
| Client refresh | Carry/derive session, preserve/conditionally associate row, resolve/carry actual root, extend before signing. |
| New/existing `auth-client` | Shared lifecycle lock, source ownership/state, correct root, persist association and lineage time, register before response. |
| `register-client-v1` fresh/replay | Apply the same rules to both branches; a retained registration is not an exemption. |
| localclient/hosted parent authority | Preserve parent ownership, selected validation branch and 503 classification; inherit session/root. |
| Device adoption | Independent `device_adopt` session, top-level client root, transactionally stored association. |
| Hosted proxy loader/signer | Stable persisted session bootstrap/migration; carry actual root and stored credential time, never regenerate authority after revoke. |
| Prober shard/identity, sim-latency | Explicit internal session exemptions; every signed client still gets a correct root and bounded new-token lifetime. |
| API-key/MCP identity | Remain unsigned; client issuance uses the explicit independent-client-session rule. |
| Provider egress credentials | Shared validation and error semantics; no private weaker auth path. |

Server work lives in `model/session_model.go`, `session/` validation/types, lifecycle/auth/client/adoption models, migrations, API routes/handlers, connect admission/resident/stream retirement, Redis deadline client, taskworker and observability. Keep key construction/validation in a low-level session helper so `model -> session` does not create an import cycle. Put storage-free client-info/last-use types there for reuse; model owns writes. Protocol work is in the connect repository; SDK work ends at the typed API, bindings and `client_session_view_controller.go`.

Merge `jwt` into `session` first as a separate behavior-preserving series: move implementation/tests, temporarily forward exported symbols through a shim, migrate importing packages while renaming shadowing `session` variables, then remove the shim and unused `User()` helper. Preserve metric names/log tags. Do not mix mechanical package churn into the security behavior review.

## 12. Deterministic verification and release gates

Every bug fix must include a deterministic regression test exercising its root cause, per the user's repository instructions. Use explicit clocks, barriers/hooks, controlled transports and real test Redis/Postgres where semantics matter; no sleeps to create races. This document specifies required tests, not passing results.

| Area | Required regression scenarios |
|---|---|
| Authority branches | Session wins when both claims exist; missing/revoked/unavailable marker; inactive root; multi-level ancestor removal; A→B→C distinction; own-client and credential-time rejection independently in every branch. Exercise API, H1/H3, hosted parent and InTx boundaries; retain cross-owner refusal. |
| Claims/mints | Every producer and replay branch stamps root/session correctly; explicit source under network auth; legacy parent; reissue under another session; old `auth_session_ids` ignored; no bypass of registration before signing. |
| Capacity (R1) | 1,001 live-session attempts; retained tombstone exhaustion; accepted tokens remain listed/revocable; revoke-others leaves only the kept tracked session. Existing refresh still works at capacity. |
| Horizon (R2) | Strict/lenient instances and allowed rollback share markers; revoke after nominal exp, advance beyond a short TTL, reject all still-in-policy tokens; max score/shared TTL never shrink; tagged connections cross terminal horizon and close. |
| Ownership/commit races (R3) | Pause cleanup after candidate selection, rebind to B, resume sync/queued cleanup and preserve B; pause A mint before commit, start revoke and prove exclusive/shared ordering; registration/proxy NULL-association races preserve newer binding. |
| Codes/retries (R4) | Code-create paused across revoke near terminal horizon; origin cannot be redeemed after cutoff; clamp end time; Redis-applied/SQL-uncommitted revoke recovers; last-use code consumption rolls back on failed registration; lost responses replay one session/use. |
| Fleet invariant (R5) | New-version mint followed by oldest-supported-version network/client refresh extends full horizon; revocation survives that horizon and rollback; oldest connect enforces registered leases even without notifications. |
| Migration (R6) | Concurrent NULL-proxy starts and restarts reuse stable association; legacy client refresh and retained registration populate rows; old auth-code lineages group as documented; original legacy token behavior before/after derived marker expiration is explicit. |
| Events (R7) | High counter then expiry/deletion/recreation; new incarnation refreshes an open controller; delayed old frames/responses cannot roll back state; dropped subscription and corrective paths heal. |
| Real deadlines (R8) | Stall socket reads, exhaust auth pool and exercise cluster routing/redirects; verify actual two-second budget and owned SQL release, not just configured durations. Every caller yields unavailable/503, never credential rejection. |
| API-only logout (R9) | No client/device credential, list/renewal 401 produces one persisted cleanup/logout; delayed 401 after new login is ignored; unrelated client session survives; all 503 paths preserve credentials. |
| ClientInfo/last use | All six requested fields round-trip typed through Redis/API/SDK/bindings; missing/malformed/duplicate/oversized/unknown headers; H1/frame/H3 metadata generation; GeoIP unknown; no raw IP; successful auth only; atomic newer tuple wins; sampling bound; TTL extension without timestamp change; in-flight sample cannot recreate revoked key; optional write outage does not reject auth. |
| Maintenance | Earlier-expiring concurrent create lowers existing index; stale sweeper raise/delete loses CAS; failed cross-slot write repairs; expired raw exp never creates repeated grace-period work; earlier-member touch and old retry cannot shorten shared TTL. |
| Bulk/reset/delete | Auth-code redemption on either side of atomic cutoff; fresh independent login after cutoff survives; API-key/legacy keep-ID refusal; parallel quota reservation; prepared operation across reset; legacy H1/H3 held open across reset/delete. |
| Data plane | Register/recheck race, missed events, expiration/outage lease closure, distinct close causes, remote honest stream retirement, no resurrection on migration; established P2P with uncooperative revoked endpoint respects the documented bound. |
| Controller | API-only and device-backed lifecycle; refresh cancellation/coalescing; stale credential/list results; last-used nulls; pending operations; self-revoke; network-only route selection; binding/ABI and RPC listeners. No app UI implementation in this work. |

Run affected Go packages with the repository test environment, required protocol generation/round-trip checks, SDK binding/ABI checks and targeted race tests. Run broader required repository checks after those pass; do not substitute source inspection for an executable interleaving test. Verify latency/memory at 1,000 live sessions, 10,000 retained IDs and representative client fan-out. The bulk Lua maximum, periodic SQL revalidation and stream lease fan-out are explicit performance gates.

### Deployment sequence

1. Land package merge and behavior-preservation checks independently.
2. Add schema/indexes, typed metadata/header support, claims, complete validation, deadline client, durable operations, mint association machinery, event protocol and connection/stream leases with new-session creation disabled. Deploy all API, connect, proxy, local/hosted authority and taskworker blocks. **Even while creation is disabled, every tagged mint checks and extends the session before signing.**
3. Release connect/SDK compatibility, typed APIs and `client_session_view_controller.go` with missing-feature tolerance and stream-lease negotiation. Bindings must be ready; app UI is a separate handoff.
4. Prove every admission/mint path and oldest rollback version enforces the complete invariant and live leases. Check deployed Redis persistence/eviction/notifications, clock bounds, deadline telemetry, outbox recovery and migration readiness. Do not enable revocation merely because an intermediate release copies claims.
5. Enable session minting for a bounded cohort; inspect producer coverage, metadata quality, capacity, horizon monotonicity and proxy migration. All credentials carrying new IDs must already be registered/enforced fleet-wide.
6. Enable list and revoke after deterministic correctness and data-plane gates pass. Enable hints independently. The SDK/controller is ready for the separately developed UI.
7. Monitor auth/Redis latency, unavailable/rejection causes, legacy branch use, cap refusals, deadline bounds, retained memory, sample drops, operation/cleanup/index backlog, notification resets and transport/stream lease retirement. Use bounded labels, not session/client IDs.

After activation the rollback floor is the compatibility release implementing **all** claim propagation, check-and-extend, lifetime, common validation and transport lease rules. An older binary is not an allowed rollback. Feature switches can stop new session creation, optional metadata and notifications; they cannot turn acknowledged revocation into fail-open acceptance.

## 13. Scope and evidence

Required implementation includes all authority, recovery, metadata, API, transport and SDK/controller work above. Deferred work includes app UI (separate plan), continuous packet-activity reporting, detailed client-tree screens, durable per-request session allowlisting, OAuth/API-key management screens, positive-marker caching and shortening the legacy grace policy. None is implicitly required to change the selected authorization precedence.

This plan was grounded in the complete proposal/review and inspected current server, connect and SDK auth, mint, Redis, transport and rejection code. No executable tests, deployed configuration audit, production latency benchmark or end-to-end P2P verification was performed while writing it. Those are release work, not evidence supplied by the plan.

Input fingerprints:

- `REVOKE.md`: `a92ceeb9d54fdfce301e2f3ad30a3612076192dbdf95c03157dbeafe1686cb5d`.
- `REVOKE-REVIEW.md`: `e8f16634aa0d21b2235156993c1c0e91755d6c4291ecb008baabb7b893e27472`.
- Inspected server HEAD: `269acffb57b315e7ee9446d9155a8858469e5ecf`; connect HEAD: `64e433f47cb5ac9f32e71a5b6d5aff659bc00623`; SDK HEAD: `f2aa25f6af5aee4780cad2e67c0e25a6d2364495`. The shared workspace may advance independently; recheck these paths before implementation.
