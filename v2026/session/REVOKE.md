# REVOKE — sign-in sessions: tracking, listing and revocation

Status: design, 2026-10-09. Nothing here is implemented yet.

A user who thinks someone else is using their account (support inbox item
7881) cannot see where the account is signed in, and cannot cut one sign-in
off. Today there are two levers: a password change, which moves
`credential_change_time` and invalidates every token of the network, and
removing clients one at a time. This design adds sessions. Every sign-in gets
a session id, the server tracks each network's live sessions in Redis, the
apps show them on a Sessions screen, and the user can revoke one session or
all the others. A revoked session is a per-session Redis key that every
authenticated request checks.

Paths are relative to the server repo. Paths in other repos are prefixed with
the repo (`sdk: api.go`, `connect: protocol/frame.proto`). Line numbers are
from main on 2026-10-09.

## 1. Decisions (owner, 2026-10-09)

1. A session is a top-level JWT, one with no parent client. Child (client)
   JWTs are not sessions and are not listed. Revoking a session invalidates
   all of its children.
2. Every top-level JWT carries a session id claim, set when it is minted.
   Every child JWT carries its root's session id, so one revocation key covers
   the session and all of its children.
3. Revocation is one Redis key per session id, with a TTL to the session's
   expiration. Every authenticated request checks it, on the API and on
   connect. A revoked session cannot refresh or mint new tokens.
4. Every mint adds the session to a per-network Redis set that maps session id
   to expiration time. A taskworker task reviews each network's set and
   removes expired sessions. All live session tracking reads this set.
5. A new live update message, like the network peers notification, tells a
   network's clients when its sessions change.
6. Placement: the session model is `model/session_model.go`, the per-request
   check is in `session/`, and `jwt/` merges into `session/` (§10).
7. Tokens minted before this ships have no session id. They are ignored: not
   tracked, not revocable. Each gets a session id at its next refresh.

In code, decision 1 reads as follows. A JWT is a child exactly when it has a
`client_id` claim (`jwt/by_jwt.go:221`, `authCredentialKind` at :420-425; the
router gate in `router/client_credential.go:81-101` and the sdk's
`renewableNetworkCredential` in `sdk: api_network_credential_renewal.go:99-102`
use the same test). So the sessions are the network tokens minted at sign-in.
Every client token is a child of the session that minted it, including the
token of a device client signed in directly
(`network_client.source_client_id IS NULL`). See open question 1.

## 2. Background

### 2.1 Tokens today

- `ByJwt` (`jwt/by_jwt.go:215-230`) has these claims: network_id,
  network_name, user_id, create_time, device_id, client_id, guest_mode
  (deprecated), pro, roles, principal, and the registered claims.
  `newRegisteredClaims` (:232-243) gives every token a fresh `jti` and sets
  `exp` to now + 30 days (`expiryDuration`, :66).
- `Client()` (:749-763) derives a client token from a network token. The only
  lineage today is `CreateTime`, which every derived credential copies
  (`model/network_client_model.go:817-819`,
  `controller/auth_controller.go:633-639`, auth codes at
  `model/auth_model.go:1791-1793`).
- On every API request, `ClientSession.authenticate`
  (`session/client_session.go:182-237`) parses the token (:221) and calls
  `ValidateByJwtState` (:225). That runs one Postgres query
  (`jwt/by_jwt.go:586-691`) for the client's `active` flag and the user's
  `credential_change_time`. A token that fails it is told "Signed token is
  no longer active." (:680, :688).
- On connect, H1 (`connect/transport.go:1423-1434`) and H3/QUIC
  (`:2246-2259`) parse with the connect audience, require a client id, and call
  `connectClientAuthentication` (`connect/transport_authentication.go:20-32`).
  That runs the same `ValidateByJwtState(…, true)`. The exchange (resident to
  resident) carries no JWT, so this is the only client auth boundary on
  connect.
- The other callers of the same validation are `localclient/authority.go:52`
  and :81, `model/network_client_model.go:578` (`InTx`, inside the
  parent-mint transaction), and
  `taskworker/work/provider_egress_credentials.go:57`.
- `reject_expired` is off (vault `auth.yml`, `jwt/by_jwt.go:330-360`), so
  tokens past `exp` are still accepted. This matters for revocation TTLs
  (§5.5).

### 2.2 The earlier session model, and why this one is different

Until bb4d0676 (2026-07-12), `ByJwt` carried `auth_session_ids`, and
`IsByJwtActive` looked those ids up in the Postgres tables `auth_session` and
`auth_session_expiration`. The lookup was short-circuited for performance.
`auth_session` grew by one row per login and was never read, and the tables
have been dropped (`db_migrations.go:3399-3417`). This design is different in
three ways:

- The check is one O(1) Redis `EXISTS`, next to the Postgres query that
  already runs on every request.
- Every key has a TTL.
- Each network has a cap, and a sweeper bounds growth.

The old claim name is never reused, because auth-code tokens minted before
2026-07-12 still carry `auth_session_ids`. A test pins that such a token
parses with no session id.

## 3. The session id

- **Claim:** `session_id`, declared as
  `SessionId *server.Id \`json:"session_id,omitempty"\`` next to `ClientId`
  (`jwt/by_jwt.go:221`). `jti` can't serve, because every token gets a fresh
  one, including each child and each refresh.
  `CreateTime` can't serve either: it is lineage, not identity, since auth
  codes copy it.
- **Minting:**
  - Every top-level mint sets the claim (§3.1).
  - Every lineage mint copies it. `Client()` copies it.
  - A new `NewByJwtFromParent(parent, networkName, pro)` copies NetworkId,
    UserId, CreateTime, GuestMode and SessionId. It replaces the hand-written
    copies at the lineage sites, following the CODESTYLE rule that the routing
    key is stamped at every producer.
- **Unsigned identities stay pure:** `NewByJwt` and `NewByJwtWithCreateTime`
  set no session id. The per-request identities for API keys
  (`session/client_session.go:203-210`) and MCP OAuth (`mcp/auth.go:143-156`)
  build a `ByJwt` that is never signed and is not a session.
- **Guard:** once minting is on, `ByJwt.Sign()` refuses a token with no
  session id unless its site is explicitly exempt (§3.1, the internal rows).
  A test per mint site makes sure a new sign-in path can't forget.

### 3.1 Mint sites

| # | Site | Where | Shape | Session |
|---|---|---|---|---|
| 1 | SSO login (Apple, Google) | `model/auth_model.go:442-455`, called at :641 | top-level | new, kind `apple`/`google` |
| 2 | Wallet login (Solana, EVM, Bittensor) | `model/auth_model.go:756-771` | top-level | new, kind `wallet` |
| 3 | Password login | `model/auth_model.go:1042-1063` | top-level | new, kind `password` |
| 4 | Verify code (finishes sign-up or sign-in) | `model/auth_model.go:1225-1245` | top-level | new, kind `verify` |
| 5 | Auth-code login | `model/auth_model.go:2130-2141` | top-level | new, kind `auth_code`, `origin` = the creating session (open question 3) |
| 6 | Seedphrase login | `model/seedphrase_auth_model.go:117-118` | top-level | new, kind `seedphrase` |
| 7 | Sign-up | `model/network_model.go:298-305`, :439-446, :511-518, :671-678 | top-level | new, kind `signup` |
| 8 | Network refresh | `controller/auth_controller.go:737-751` | top-level, same session | carry (derive if legacy, §3.2), touch |
| 9 | Client refresh `/auth/refresh` | `controller/auth_controller.go:641-652`, also via `localclient/authority.go:277-287` | child | carry, touch |
| 10 | `/network/auth-client`, new client (also `AuthNetworkClientFromParent` at `model/network_client_model.go:319-326`, and `localclient/authority.go:151-179`) | `model/network_client_model.go:817-830` | child | carry; store `network_client.session_id` |
| 11 | `/network/auth-client`, re-auth of an existing client | `model/network_client_model.go:1016-1030` | child | carry; update `session_id` |
| 12 | `register-client-v1` | `model/network_client_registration.go:213-215` | child | carry |
| 13 | Device adoption confirm | `model/device_association_model.go:950-957` | child of a throwaway parent | new, kind `device_adopt` (open question 4) |
| 14 | Hosted proxy device | `LoadByJwtFromClientId` at `jwt/by_jwt.go:908-991`, signed at `proxy/proxy_device.go:777-783` | child of a throwaway parent | carry the proxy client's `network_client.session_id` |
| 15 | Prober shard, prober identity, sim-latency | `model/prober_shard_model.go:124`, `model/prober_identity_model.go:570-578`, `connect/sim-latency/fleet.go:470-471` | internal | exempt, no session id |
| 16 | API-key request identity | `session/client_session.go:203-210` | never signed | not a session |
| 17 | MCP OAuth request identity | `mcp/auth.go:143-156` | never signed | not a session |

Guest sign-up is retired (`model/network_model.go:1590-1591`), and
`GuestMode` is deprecated (`jwt/by_jwt.go:222`). Legacy guest tokens renew
through #8 (`controller/auth_controller.go:742`). `ByJwt.User()`
(`jwt/by_jwt.go:765-777`) has no non-test callers and is deleted during the
merge.

### 3.2 Legacy tokens

Following decision 7, a token with no session id makes no Redis lookup, isn't
listed, and can't be revoked. It gets a session id at its next lineage mint,
either a refresh or a child.

That id is derived, so a legacy network token and its legacy client tokens
land on one session whichever of them refreshes first:

- The time bits come from the token's `CreateTime`.
- The other 80 bits are SHA-256 over `urnetwork:legacy-session:v1`,
  network_id, user_id and create_time in nanoseconds.
- The session is registered with kind `legacy`.
- A zero `CreateTime` gets a random id instead.

A legacy token and its children share `CreateTime` by construction, which is
why the derived ids agree.

## 4. Redis model (`model/session_model.go`)

### 4.1 Keys

All of a network's keys share the hash tag `T = {ns_<networkId>}`. This
mirrors the peers registry's `{np_<networkId>}` (`model/peer_model.go:86-96`),
so each script runs on one slot.

| Key | Type | Contents | TTL |
|---|---|---|---|
| `T z` | sorted set | member: session id (16 raw bytes); score: session expiry in ms, the latest `exp` of any token that carried the id | the latest score + grace, extended on touch |
| `T s:<sid>` | hash | kind, create_ms, mint_ms, origin session id, country code and region (local GeoLite2, `ip.go:557`; the IP is never stored), user agent (at most 128 bytes), device hint | score + grace |
| `T r:<sid>` | string | revocation marker | (score − now) + grace |
| `T eid` | integer | change counter: incremented on create, revoke and expire, not on touch | 32 days, refreshed on increment |
| `{ns_index_<k>}n` | sorted set | member: network id; score: next review time (never later than the network's earliest expiry). Sharded 32 ways: k = last byte of the network id mod 32 | 60 days, refreshed on write |

Follow `server.Redis` (`redis.go:327-346`): it reruns the callback on
connection errors, and other errors panic.

- **Cluster mode:** it comes from vault `redis.yml` (:137-167). Multi-key
  operations must share a tag, and `RedisDb()` is 0 in cluster mode
  (:492-498).
- **TTLs:** `redisTtlWarnHook` (`redis_ttl_warn.go:3-38`) warns on TTLs over
  120 days and on raw `time.Duration` arguments, which go out as nanoseconds.
  Pass script arguments as integer milliseconds.
- **Index:** the network index is sharded so it never becomes one hot slot
  (`model/network_client_reliability_model.go:210-216`).

### 4.2 Scripts

Each script is inline `r.Eval` with every key declared under `T`, in the style
of `model/peer_model.go:688-780`. MULTI can't express the conditionals.
Every script takes `now_ms` as an argument and never uses Redis `TIME`, which
keeps tests deterministic.

- **create** (sid, exp_ms, now_ms, metadata, origin):
  1. If `r:<sid>` exists, return `REVOKED`. If `r:<origin>` exists, return
     `ORIGIN_REVOKED`.
  2. Add the member to `z`, keeping the larger score (compared in Lua, so
     there's no dependency on `ZADD GT`).
  3. Write the metadata to `s:<sid>`, using HSETNX for create_ms, set mint_ms,
     and set the expiries with PEXPIREAT.
  4. If the member is new, INCR `eid`.
  - After the script, `ZADD NX` the network into its index shard. This is a
    second command on a different slot. Its score is never later than the
    network's real earliest expiry.
- **touch** (on refresh and on child mints): the same steps as create,
  without the metadata. If the member is missing (the set was lost, or a
  legacy id is being derived), it is registered again with kind `legacy` or
  `restored`.
- **revoke** (sid, now_ms):
  - Read `ZSCORE`. If it's missing, return `ALREADY` if the marker exists,
    otherwise `NOT_FOUND`.
  - Otherwise, SET `r:<sid>` with PX = max(1s, score − now + grace), ZREM the
    member, DEL `s:<sid>`, and INCR `eid`.
- **revoke-others:** read the members in Go, then EVAL in chunks of 100 with
  every key declared, re-checking each score. This follows "read candidates
  first, then re-check the score in Lua" (PEERS2.md §3.1,
  `model/peer_model.go:1612-1740`).
- **review** (the sweeper):
  - `ZRANGEBYSCORE -inf (now − grace) LIMIT 100` in Go.
  - Then EVAL: re-check the scores, ZREM the expired members, DEL their
    hashes, INCR `eid` once, and return the network's new minimum score.

`eid` may be incremented twice on a retry. That is harmless, so plain
`server.Redis` is enough.

### 4.3 Mint and revoke cannot race

A session's score covers every token that has carried its id. Mint (check,
then extend) and revoke (read the score, then set the marker) are serialized
on the network's slot. So every token is either minted before the revoke, in
which case its `exp` is covered by the marker's TTL, or it is refused. A token
signed between a request's check and its mint carries the same session id, so
the revoke catches it anyway.

### 4.4 Memory and cap

| Item | Size |
|---|---|
| live session | about 0.4-0.5 KB (sorted-set entry about 30-100 B, hash about 300-400 B with key overhead) |
| revoked session, until its marker expires | about 0.15 KB |
| network in the index | about 0.1 KB |

`MaxLiveSessionsPerNetwork` is 1000. On overflow, the session that expires
soonest is dropped from the set without being revoked, and a metric records
it (open question 8). A password change stays the catch-all.

### 4.5 Expiry review task

- The task follows `taskworker/work/auth_work.go:22-52`: a `Schedule…`
  function using `task.RunOnce`, the task itself, and a `…Post` that
  reschedules it.
- `work.ReviewExpiredSessions` runs every 15 minutes and calls
  `model.ReviewExpiredSessions(ctx, now)`.
- **Per run:** for each index shard, `ZRANGEBYSCORE … LIMIT 0 200` returns the
  networks due for review. The per-network review EVALs are pipelined
  (go-redis groups them by slot), and the run stops at a fixed budget.
  The sweeper is the only writer that raises an index score. A network
  emptied by revocations is removed from the index on its next visit.
- **Registration:** in the startup schedule (`taskworker/taskworker.go:39-52`),
  in the targets list (near :187-191), and in `taskworker/workload_profile.go:127`
  in the auth maintenance family, with a row in WORKLOADS.md.
- **Correctness never depends on the sweeper.** Reads filter by score, and
  every key has a TTL. A lost index entry heals on the network's next create.

### 4.6 Model API (sketch)

```go
type SessionKind string

const (
	SessionKindPassword    SessionKind = "password"
	SessionKindApple       SessionKind = "apple"
	SessionKindGoogle      SessionKind = "google"
	SessionKindWallet      SessionKind = "wallet"
	SessionKindVerify      SessionKind = "verify"
	SessionKindAuthCode    SessionKind = "auth_code"
	SessionKindSeedphrase  SessionKind = "seedphrase"
	SessionKindSignup      SessionKind = "signup"
	SessionKindDeviceAdopt SessionKind = "device_adopt"
	SessionKindLegacy      SessionKind = "legacy"
)

type SessionMint struct {
	Kind            SessionKind
	OriginSessionId *server.Id
	CountryCode     string
	Region          string
	UserAgent       string
}

var ErrSessionRevoked = errors.New("session revoked")

// assigns byJwt.SessionId and registers it; call before Sign
func CreateSession(clientSession *session.ClientSession, byJwt *session.ByJwt, mint *SessionMint) error
// carries (or derives, for a legacy token) byJwt.SessionId and extends it; call before Sign
func TouchSession(ctx context.Context, byJwt *session.ByJwt) error
func GetNetworkSessions(ctx context.Context, networkId server.Id) ([]*NetworkSession, error)
func RevokeSession(ctx context.Context, networkId server.Id, sessionId server.Id) (*RevokeSessionResult, error)
func RevokeOtherSessions(ctx context.Context, networkId server.Id, keepSessionId *server.Id) (int, error)
func ReviewExpiredSessions(ctx context.Context, now time.Time) (*ReviewExpiredSessionsResult, error)
func DeleteNetworkSessions(ctx context.Context, networkId server.Id) error
```

Both mint functions run before `Sign()`. When the session or its origin is
revoked, they return `ErrSessionRevoked` instead of letting the caller sign.

**Import direction:** `model` already imports `session`, and `session` (with
`jwt` merged in) imports only `server` and `apikey`. So the key-name function
`session.SessionRevocationKey(networkId, sessionId)` and the check live in
`session`, and the model calls them. The writer and the check can't disagree
about key names, and there is no cycle.

## 5. The per-request check (`session/session_revocation.go`)

### 5.1 Placement

The check goes in `ValidateByJwtState` and `ValidateByJwtStateInTx`
(`jwt/by_jwt.go:586-603`), after `validateByJwtStateIdentity` (:606-616) and
before the Postgres query (:620). After the merge this code lives in
`session`, and the one implementation covers every caller: API (§2.1),
connect H1/H3, localclient, the parent-mint transaction, and provider egress
credentials. A revoked token never takes a Postgres connection.

A revoked session is rejected with the existing inactive-token message
("Signed token is no longer active."), and a new cause, `session_revoked`, is
added to the rejection counter (`jwt/by_jwt.go:383-395`, :427-435).

### 5.2 Cost

- **Today, per request:** one signature check (`jwt/by_jwt.go:517`) and one
  Postgres query.
- **Added:** one `EXISTS {ns_<networkId>}r:<sid>`, a single small key, one
  round trip.
- **Who pays:** only tokens with a session id. Legacy tokens, API keys and
  OAuth identities pay nothing.
- **On connect:** it runs once per connection auth, never per packet.

### 5.3 Redis errors: fail closed with 503, never 401

**Why closed:**

- **Precedent:** Postgres failures already become `ErrAuthUnavailable`
  (`session/auth_deadline.go:15-46`). The router answers those with 503
  (`router/handler_utils.go:137-142`, :448-454), and so does connect
  (`connect/transport_authentication.go:34-73`).
- **No mass sign-outs:** the sdk signs out only on a confirmed 401
  (`sdk: api_client_control_json.go:26-48`, `sdk: device_token_manager.go:546-556`).
  A Redis outage therefore delays requests but signs nobody out.
- **Fail open is worse:** it would let a revoked attacker back in during any
  outage.

**Required changes:**

- Redis failures are wrapped as `ErrSessionStoreUnavailable` and classified
  in `authDependencyUnavailable` (`session/auth_deadline.go:48-55`) and
  `connectAuthDependencyUnavailable` (`connect/transport_authentication.go:66-73`).
  Without this, `server.Redis` panics with the raw error (`redis.go:289-325`)
  and the router answers 500.
- Two callers must stop treating every validation error as an auth failure,
  or a Redis blip would sign out hosted devices:
  - `localclient/authority.go:81-86` maps every error to 401.
  - `model/network_client_model.go:578-580` maps every error to
    `ErrClientParentInactive`.
- The check uses a short retry budget, about 2s. The default retries for up
  to 60s (`db.go:366-373`), capped only by the 30s `AuthTimeout`.
- **Kill switch:** vault `auth.yml` `session_revocation_check`, default true.
  It loads once, like `reject_expired` (`jwt/by_jwt.go:337-373`), with a
  `Testing_` override.

### 5.4 Caching

- **No negative cache:** it would delay a revocation, against decision 3.
- **Coalescing:** a per-process singleflight per (network, session id). One
  embedding backend's session can be the root of many clients, all checking
  the same key at once.
- **Revoked-id cache:** a bounded in-process cache of revoked ids would be
  safe, because a revocation stands until its TTL. Add it only if retry
  storms show up.

### 5.5 Expired tokens are still accepted

While `reject_expired` is off, a token past `exp` is still accepted. A marker
whose TTL ends at the session's expiry would therefore lapse while the
revoked token still works. The rule:

- A token that carries a session id is refused once it is more than 60 days
  past `exp`, whatever `reject_expired` says. This is checked in
  `ParseByJwtForAudience` and needs no I/O.
- Revocation markers live for (score − now) + 60 days, under the 120-day TTL
  warning.
- Once `reject_expired` is on, the grace shrinks to `clockLeeway`
  (`jwt/by_jwt.go:67`).

See open question 2.

## 6. Live updates

The notification follows the current peers transport, PEERSSTREAMS2.md
(Redis key events plus a corrective poll), and keeps every rule from the
2026-07-15 outage (PEERS2.md §2):

- no per-client subscriptions,
- no application-level publish,
- a per-network counter,
- a kill switch.

**Server side:**

1. **Writer:** the scripts in §4.2 INCR `T eid` on create, revoke and expire.
2. **Subscriber:** the exchange's key-event subscriber, one per process
   (`connect/key_event_subscriber.go:59-87`, :267-341), adds a pattern
   `model.NetworkSessionsKeyEventPattern(db)`, which is
   `__keyspace@<db>__:{ns_*}eid`. It dispatches `incrby`, `del` and `expired`
   to session listeners registered with an `AddSessionListener` modeled on
   :91-110. The production keyspace event classes (`Kg$sx`, `redis.go:659-662`)
   already include strings, so no new class is needed.
3. **Listener:** `model.NetworkSessionListener` compares counters only, with
   no full reads. A corrective `GET eid` runs at the peer listener's cadence
   (`connect/resident.go:2100-2129`). At most one frame goes out per 5s per
   network.
4. **Resident:** the session listener is registered next to the peer
   listener (`connect/resident.go:4146-4168`) for Client-category
   connections. It is gated by a new `EnableNetworkSessions` in
   ExchangeSettings (near :452 and :572).
5. **Protocol (connect repo):**
   - `TransferNetworkSessionsChanged = 33` in `protocol/frame.proto`, after
     :94-96.
   - `message NetworkSessionsChanged { uint64 event_id = 1; }` in
     `protocol/transfer.proto`, next to :416-420.
   - Both directions in `frame.go` (:80-83, :250-253).
   - Older clients ignore control types they don't know.

**Client side:**

6. **connect client:** a new case in `transfer_peer_manager.go:229-249` keeps
   the highest `event_id` in a `MonitorValue` (CODESTYLE:120).
7. **sdk:** a `watchNetworkSessions`, cloned from `DeviceLocal.watchNetworkPeers`
   (`sdk: device_local.go:7763-7799`), fires
   `NetworkSessionsChangeListener.NetworkSessionsChanged(eventId)`. RPC
   forwards it to DeviceRemote the way peers are forwarded
   (`sdk: device_rpc.go:11773-11784`, :13985-13991). The Sessions screen
   refetches the list.

The frame carries only `event_id`, never session data. The list is
Network-only (§8), while frames reach connections that hold client tokens.

## 7. Cutting off live connections

The auth check stops new connections, but an open connection would outlive a
revoke. This step closes those connections.

- **Registry:** `Exchange.registerConnection` (`connect/resident.go:1761-1771`,
  called at `connect/transport.go:1478` and :2305) also records each
  connection's (network, session id) and its kick function.
- **Kick:** reuse the existing kick paths, H1 `kickRequest`/`writeKick`
  (`connect/transport.go:1497-1503`, :1696-1701) and H3 `kickOnce`
  (:2310-2316). Add a `session_revoked` cause that sends
  `TransportControlClose` reason 2, plus WebSocket close / QUIC application
  error 4002, mirroring `connect/provider_intent.go:52-54`, :137-168 and
  `connect: transport_client_limit.go:58-66`.
- **Watcher:** an `eid` event for a network triggers a pipelined `EXISTS` over
  that network's local session ids. A corrective pass every 60s (±20%) checks
  all local (network, session id) pairs in batches of 512. Nothing runs per
  packet.

## 8. API

### 8.1 Routes

The routes sit next to `api/api.go:116-119`, with handlers in
`api/handlers/network_session_handlers.go` following
`api/handlers/network_client_handlers.go:38-52`.

**`GET /network/sessions`** returns:

```
{
  sessions: [{
    session_id, current, kind,
    create_time, last_active_time, expire_time,
    country_code, region, user_agent, origin_session_id,
    clients: [{client_id, device_id, device_name, device_spec, connected}]
  }],
  event_id
}
```

- The sessions come from Redis: one slot, one pipeline.
- The clients come from one Postgres query on `network_client.session_id`,
  using the partial index on top-level clients (`db_migrations.go:3317-3325`).
- `connected` comes from the peer registry (`model/peer_model.go:1744-1800`).
- `current` marks the caller's own session. API-key callers have none.

**`POST /network/revoke-session {session_id}`**

- A session id from another network simply isn't found, because the keys are
  per network.
- Revoking the current session is allowed; it is a server-side sign-out.

**`POST /network/revoke-other-sessions {}`** returns `{revoked_count}`.

**After a revoke:**

- The session's clients are deactivated through the existing locked path
  (`model/network_client_model.go:1151-1197`, :1235-1266). Above
  `RemoveNetworkClientsBatchCount` (:1149) this goes through
  `RemoveNetworkClientsTask` (:1360-1485).
- The session's auth codes are deactivated.
- Kicks and notifications follow from §6 and §7.

### 8.2 Authz and rate limits

- **Authz:** all three routes are `routeAccessNetwork` in
  `api/route_authz.go` (near :116 and :136), so client tokens get 403. They
  are mirrored in `sdk: api_network_credential.go:57-125` as
  `apiRouteAccessNetwork`. Two tests keep the tables in step,
  `TestEveryRouteIsClassified` and the sdk's
  `TestApiAdminRoutesMatchTheServer`. The AUTHZ1.md table gets the new rows.
- **Rate limits:** `AccountActionRevokeSession` (100 a day) and
  `AccountActionRevokeOtherSessions` (20 a day) in
  `model/account_action_rate_limit.go:14-30`. The limit is checked before the
  action and recorded after it succeeds. Listing has no limit.

### 8.3 Interplay

- **remove-client** (`model/network_client_model.go:1063-1106`) doesn't revoke
  the session. If it removes the session's last device, the session just
  shows no device. Bulk remove-clients and `RemoveDisconnectedNetworkClients`
  (30 days idle, :2995) are unchanged.
- **Password set:** after it stamps `credential_change_time`
  (`model/auth_model.go:1659`), every session of the network is revoked.
  Today a password reset doesn't close live connections; this fixes that.
- **Network delete** (`controller/network_controller.go:199`,
  `model/account_model.go:215`): the network's session keys are deleted.
- **Auth codes:** a new `auth_code.session_id` column is set on insert
  (`model/auth_model.go:1796-1819`). Create (§4.2) checks it atomically, so an
  auth code dies with its creator's session.
- **Clients:** a new nullable `network_client.session_id` column records which
  session minted each client (open question 6). It supports the list, the
  proxy carry (§3.1 #14) and deactivation on revoke.

## 9. SDK and apps

### 9.1 SDK

- **Api methods** in `sdk: api.go`, following `GetNetworkClients` (:1086-1097):
  `GetNetworkSessions`, `RevokeNetworkSession` and `RevokeOtherNetworkSessions`.
  New types `NetworkSessionInfo` and `NetworkSessionInfoList` follow the list
  wrappers in `sdk: gomobile.go:302-306`.
- **Device listener:** the interface goes next to `sdk: device.go:139-141`,
  with DeviceLocal wiring near `device_local.go:1828-1836` and RPC forwarding
  as for peers.
- **`sdk: sessions_view_controller.go`** (build tag `!ios_extension`), modeled
  on `devices_view_controller.go` and `peer_view_controller.go`:
  - It fetches on start, on a newer `event_id`, and when the app returns to
    the foreground.
  - It marks the current session.
  - It exposes revoke and revoke-others.
  - It has an Api-only constructor for apps without a device.
- **Bindings:** gomobile, cgo (regenerate with `cgo/gen/gen.go`, and update
  `abi_baseline_test.go`), C# `Raw.g.cs`, and js `account_host.go:88-101`.
- **Rollout tolerance:** a 404 means the server doesn't have the feature yet,
  and the screen hides itself (pattern: `mmm: ur.io/react/src/auth/api.js:279-295`).

### 9.2 When an app's own session is revoked

The existing sign-out pipeline handles it end to end:

1. The server's session-revoked close (§7), or a 401 at transport auth,
   triggers a JWT refresh (`sdk: api.go:334-336`).
2. `/auth/refresh` answers 401.
3. `rejectByJwt` (`sdk: api.go:271-293`) clears the client and network
   tokens and fires `AuthLogoutListener`.
4. `DeviceLocal.handleApiAuthLogout` (`sdk: device_local.go:2787-2798`)
   passes it to each app's sign-out:
   - Android `MainApplication.kt:2303-2311`
   - Apple `DeviceManager.swift:1563`
   - Windows `SdkHost.cpp:3199`
   - Linux `SdkHost.cpp:2642`
   - ur.io `AuthContext.jsx:228-258`

Two more paths should call `RequestJwtRefresh` the same way: the
network-renewal 401 branch (`sdk: api_network_credential_renewal.go:511-516`)
and a 401 from the Sessions screen. The user sees the normal sign-out with
the message "Signed out from another device".

### 9.3 Screens

| Platform | Where | Following |
|---|---|---|
| Android | Settings, Account block, a row after Balance codes (`ui/settings/SettingsScreen.kt:748-772`) | `Route.Sessions` like `MainNavViewModel.kt:116` and `MainNavHost.kt:1610-1616`; `ui/sessions/SessionsScreen.kt` with a Hilt ViewModel like `NetworkPeersViewModel.kt:52-120` |
| Apple iOS | the Sign-In Methods section (`SettingsForm-iOS.swift:127-158`) | `.sessions` in `AccountNavStackViewModel.swift:11-25`, destination at `AccountNavStackView.swift:180`, a folder like `TransferBalanceCodes/` |
| Apple macOS | `SettingsForm-macOS.swift:266` | same as iOS |
| Windows | `SettingsPage::BuildSecuritySection` (`SettingsPage.cpp:225-253`) | a `SessionsSheet` like `SettingsSheets.h:205` |
| Linux | `AccountPage::BuildSecurityGroup` (`AccountPage.cpp:2117-2150`) | `SessionsSheet.cpp/.hpp` |
| ur.io | `screens/Sessions.jsx`, linked from `Profile.jsx` (near :270), routed next to `AppRoutes.jsx:114` | `hostCall` as in `BalanceCodes.jsx` |

**Each row shows:**

- the device name, platform and app version,
- the sign-in method,
- when it was created and when it was last active,
- country and region,
- "This device" on the current session,
- the session's clients,
- a Revoke button.

A "Sign out all other sessions" action sits at the bottom. Strings go in
`localizations/keys/*.yaml`, followed by `npm run gen`.

## 10. Package merge: `jwt` into `session`

- **Size:** `jwt` is imported by 29 non-test and 189 test files in 20 package
  directories, with 725 `jwt.` references. `session` is imported by 176
  non-test and 285 test files. 149 files import both.
- **No cycles:** `jwt` imports only `server`, `connect` and `glog`. `session`
  imports `server`, `apikey` and `jwt`, and `apikey` imports only `server`.
  No other repo imports either package.
- **No package-level name collisions.** One hazard: about 24 files use `jwt.`
  inside functions that have a local variable
  `session *session.ClientSession`. A blind rewrite of `jwt.` to `session.`
  would resolve to the variable and fail to compile. Examples:
  `controller/auth_controller.go` (9 references), `model/auth_model.go`,
  `model/network_client_model.go`, `model/network_model.go`,
  `model/prober_identity_model.go`, `proxy/proxy_device.go`, `task/task.go`.
- **Steps, each of which builds:**
  1. `git mv jwt/*.go session/` (package `session`). Drop the `jwt.`
     qualifiers inside `session`. Leave a shim `jwt/alias.go` that forwards
     every exported identifier.
  2. One commit per importing package: rewrite `jwt.` to `session.`, and
     rename shadowing variables to `clientSession`.
  3. Delete the shim, delete `User()`, and unexport what no longer crosses a
     package.
  - Metric names (`grafana/dashboards/signals.json`) and the `[jwt]` log tags
    stay as they are.
- **Timing:** land it first, as its own series with no behavior change. That
  keeps about 900 files of mechanical churn out of the security review of the
  revocation diff.

## 11. Tests

All of these tests are deterministic: top-level `TestXxx` against
`server.DefaultTestEnv()` (Postgres and Redis), with explicit `now` arguments
and hooks, and no sleeps.

**session**

- The claim round-trips, and `Client()` copies it.
- A token with `auth_session_ids` parses with no session id.
- A revoked token is refused before Postgres, with a pool-acquire tripwire as
  in `test_pg_query_scope.go`.
- A Redis failure gives `ErrAuthUnavailable` (503 on connect, never 401 from
  localclient).
- A legacy token makes no lookup.
- The 60-day expiry cap from §5.5 holds.

**model**

- create, touch, revoke, revoke-others and review each behave as specified.
- The score never shrinks.
- The marker's TTL covers a later child's `exp`.
- A mint after a revoke is refused.
- A lost index entry heals.
- The cap evicts the soonest-expiring session.
- `eid` moves on create, revoke and expire, and not on touch.

**api** (through the router with real JWTs, as in AUTHZ1 §9)

- Every top-level mint site (§3.1 #1-#7, #13) yields a session id that appears
  in the list.
- Refreshes carry the session id, and legacy derivation converges.
- Children carry their root's session id (#10-#12, localclient).
- A revoked session is refused on `/auth/refresh`, network-refresh,
  `auth-client` and `code-create`.
- A revoke that lands between the check and the mint
  (`Testing_SetRefreshMintHook`, `controller/auth_controller.go:546-562`) is
  still caught.
- code-login is refused after the creator's session is revoked.
- A revoke deactivates the session's clients.
- Client tokens get 403 on the routes.
- `current` is correct.

**connect (server)**

- H1 and H3 answer 401 for a revoked session and 503 when Redis is down.
- A live connection receives the reason-2 close.
- The frame arrives on the key-event path and on the corrective path
  (`server.Testing_SetKeyspaceNotifications("")`, `redis.go:665`).

**connect repo**

- The new frame round-trips.
- One monitor notification fires per change.
- The new close reason is handled.
- Older clients ignore the frame.

**sdk**

- Route-table parity with the server.
- The view controller works with a fake device.
- Watcher notifications coalesce, and RPC forwards them.
- A revoked close leads to a refresh 401 and then `AuthLogout`.
- The ABI baseline and the js host are updated.

**apps**

- ViewModel and presentation tests on each platform.
- vitest on ur.io.

## 12. Rollout

1. **Server:** the `jwt` to `session` merge (§10).
2. **connect:** message 33, close reason 2, and the client callback.
3. **Server release 1:**
   - Carry the claim at every lineage mint.
   - Ship the check behind the switch. It is a no-op until a marker exists.
   - Ship the §5.5 expiry cap and the DB columns.
   - Minting stays off.
   - Deploy every block: api, connect, proxy, taskworker.
4. **Server release 2:** turn on minting, the list, revoke, notifications,
   the sweeper and kicks. Revoke is safe from here, because every auth path
   has checked since release 1 and no old block remains that could drop a
   session id.
5. **sdk.**
6. **Apps,** tolerating a 404 from an older server.

## 13. Open questions (recommended answer first)

1. **What is a session's parent?** Recommended: the network token minted at
   sign-in (no `client_id` claim), as in §1. A device client signed in
   directly (`source_client_id IS NULL`) is that session's child. The
   alternative, treating each top-level client as its own session, would list
   devices rather than sign-ins.
2. **Expired tokens while `reject_expired` is off.** Recommended: the §5.5
   rule. A token with a session id dies 60 days after `exp`, and markers live
   for the same grace.
3. **Auth-code login.** Recommended: a new, independent session that records
   its origin. Outstanding codes die with their creator's session. Sessions
   already made from a code are not revoked with the creator, since otherwise
   a sign-out would stop provider CLIs.
4. **Device adoption.** Recommended: its own session, kind `device_adopt`.
5. **Proxies and internal mints.** Recommended: proxies carry
   `network_client.session_id`. Probers and sim-latency are exempt.
6. **How a session's clients are tracked.** Recommended: a nullable
   `network_client.session_id` column. The alternative is a Redis set per
   session.
7. **Legacy session ids.** Recommended: the deterministic derivation at the
   next mint (§3.2). Nothing happens at request time.
8. **Per-network cap.** Recommended: 1000. On overflow, drop the session that
   expires soonest without revoking it, and record a metric.
9. **Deactivate a revoked session's clients.** Recommended: yes.
10. **Fail mode.** Recommended: fail closed with 503, plus the kill switch
    (§5.3).
11. **ur.io and devices without a live connection.** Recommended: refetch
    when the screen opens or regains focus, and poll every 30s while it is
    visible.
12. **Explicit sign-out.** Recommended: it revokes the app's own session on
    the server, best effort, without blocking the local sign-out.
13. **Password reset and network delete.** Recommended: a reset revokes every
    session, and a delete removes every session key.
14. **OAuth grants and API keys.** Recommended: out of scope; they get their
    own screens.
15. **Location.** Recommended: country and region from GeoLite2. The IP is
    never stored.
16. **Redis losing markers.** Recommended: accept the risk. Markers are a
    denylist, and Redis persistence and replication cover it. An audit row
    per revoke is optional.
17. **Merge timing.** Recommended: before the revocation work, as its own
    series (§10).
18. **Route names.** Recommended: `/network/sessions`,
    `/network/revoke-session` and `/network/revoke-other-sessions`.
19. **Frame contents.** Recommended: `event_id` only (§6).
