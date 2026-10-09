# EMBED1: server support for embedding URnetwork

Status: implemented on `feat/embed-business`; migrations 797–800 **not applied**
(owner rule: merge to main, pull and push before any migration, including DB test
suites). Companion work: the ur.io Embed guide and Services page (mmm), the embed
examples and `EMBED_CONTRACT.md` (examples repo), and the OpenAPI entries in
`connect/api/bringyour.yml`.

A customer embeds the SDK in its own app. Its backend holds the network's root
token (or an API key), authenticates its own users, and provisions **one
top-level client per running installation** with `POST /network/auth-client`. Four
server features support that:

1. **The Embed plan client allowance**: a per-network override of both client
   limits, the 100 active top-level client cap and the tier's concurrent
   connection limit.
2. **Per-client data caps and usage**: per installation, monthly and/or
   running-total caps, plus monthly usage for every top-level client.
3. **Services contact sales**: a public lead form that stores each lead and
   posts it to Slack.
4. **ACL groups**: a per-client group, `default` or `isolated`; an isolated
   client is kept out of the network's peer list.

## 1. The Embed plan client allowance

Two limits apply to a network's top-level clients, and both are enforced only
while pro.yml `enforce_concurrent_clients` is on:

- The **top-level client limit**: `LimitTopLevelClientIdsPerNetwork` (100) caps
  **active top-level clients**, one per running installation. A client idle for
  30 days (`TopLevelClientIdleExpiration`) is deactivated and stops counting;
  provider installs (`provide_intent`) and child clients (`source_client_id`)
  never count. A refusal is `error.client_limit_exceeded`.
- The **concurrent connection limit**: the tier's pro.yml `concurrent_clients`
  caps connected top-level clients, at creation (`NetworkConcurrentClientsExceeded`,
  `error.upgrade_required`) and at connection activation (`CanConnectNetworkPeer`,
  the client limit exceeded kick).

An Embed plan sets the network's client allowance — the top-level client limit
and the concurrent connection limit:

- Storage: `network_top_level_client_limit (network_id PK, top_level_client_limit,
  update_time)`. No row means the defaults: 100 and the tier's limit.
- Enforcement: the `AuthNetworkClient` create cap reads the network's limit in the
  provisioning transaction (`networkTopLevelClientLimitInTx`) and counts with
  `LIMIT limit+1`. The concurrent gates and the provider intent's normal client
  limit read it through `networkConcurrentClientLimit`, which uses the row when
  there is one and the tier otherwise. Every gate stays behind the rollout switch.
- Cache: the concurrent gates read the override on every connection activation
  while enforcement is on, so each process caches it for 30 seconds (bounded at
  8,192 networks). Set and clear refresh the process that runs them at once;
  other processes pick a change up within the ttl.
- The peer valve (`model/peer_model.go`) keeps the constant: a network past 100
  recently active top-level clients has its peer list off, so a large embedded
  network's users stay invisible to each other.
- Ops: `bringyourctl network client-limit --network_id=<id> [--set=<n> | --clear]`
  prints the effective allowance and whether it comes from an Embed plan
  (1 ≤ n ≤ 10,000,000; unknown networks are refused).

## 2. Per-client data caps and usage

Caps are **per client, i.e. per installation**. All bytes below are billable
bytes: a settled contract's used bytes, counted for the client that paid for it.

### API

All three routes answer 200 with `{"error":{"message"}}` for a refusal or an
invalid argument (auth-client house style).

| Route | Session | Purpose |
| --- | --- | --- |
| `POST /network/client-data-cap` | network: root JWT or API key | merge caps for one top-level client of the network |
| `GET /network/client-data-cap?client_id=` | network (any top-level client of its network), or a client token (its own; a child client's token reads its top-level client's) | one client's caps and usage |
| `GET /network/client-data-caps?cursor=&limit=` | network | page the clients with a cap set, in client id order; `limit` 1–1000, default 100; `next_cursor` is opaque, `null` on the last page |

A "network session" is any session without a client id: the root JWT, or an API
key session (`jwt.NewByJwt(network, user, name, false, false)`). Pro mode and
other JWT-only claims are not read.

POST body, **merge semantics**:

```json
{"client_id":"<uuid>","monthly_byte_limit":5000000000,"total_byte_limit":null,"reset_total":false}
```

- omit a limit to keep it; `null` clears it; an integer `0 ≤ n ≤ 2^60` sets it.
- `0` pauses the client: it is capped at once, without removing the client.
- `reset_total: true` zeroes the running total and starts a new total period.
- caps apply to top-level clients; a child client id is refused.

Response (the GET, the POST and each list item):

```json
{
  "client_id": "<uuid>",
  "monthly_byte_limit": 5000000000,
  "monthly_used_byte_count": 123,
  "monthly_period_start": "2026-10-01T00:00:00Z",
  "monthly_period_end": "2026-11-01T00:00:00Z",
  "total_byte_limit": null,
  "total_used_byte_count": 0,
  "total_period_start": "2026-10-08T17:00:00Z",
  "capped": false,
  "capped_reason": ""
}
```

- The monthly period is the UTC calendar month.
- The running total counts from the client's first cap request, or its last
  `reset_total`. `total_period_start` is `null` for a client that never had a
  cap request; its `monthly_used_byte_count` is still reported.
- `capped_reason` is `"total"`, `"monthly"` or `""`. When both caps are reached,
  `total` wins, since only it outlasts the month.

Go callers: `SetClientDataCapArgs` exposes the limits as `*int64` (the spec's
nullable integers). Decoding records which limits the request named, so an
omitted limit is told from `null`. In Go, a non-nil limit sets, and
`SetMonthlyByteLimit(nil)` / `SetTotalByteLimit(nil)` clear.

### Usage metering (every top-level client)

```
settlement claim (once per contract)
  └─ post after commit: RecordClientDataUsage(payer, used bytes)
       └─ redis HINCRBY client_data_usage.<block>.<shard>   (30s blocks, 32 shards, 15 min ttl)
RollupClientDataUsage task (rescheduled 30s after each run)
  └─ each closed block (≤ current − 2), each shard:
       tx { insert network_client_data_usage_drain (block, shard)   -- skip if present
            resolve payer → top-level client (COALESCE(source_client_id, client_id))
            upsert network_client_data_usage (client, UTC month) += bytes
            update network_client_data_cap: running total, capped markers }
       then DEL the hash
```

- **Payer**: `contractOrigin` (`subscription_settlement_calculation.go`), the same
  rule settlement billing uses, so bytes count for the client that paid. It was
  extracted from `contractParticipantsFromRows` unchanged.
- **Billable only**: contracts with escrow; zero-escrow contracts cost the
  customer nothing and are not metered.
- **Exactly once per drain**: a (block, shard)'s drain row commits with its
  usage, and the hash is deleted after the commit. A crash in between re-reads
  the hash and skips it. `network_client_data_usage_rollup` (a high-water mark)
  bounds the scan to new blocks, inside the hash ttl.
- **At most once per contract**: the redis increment is a post after the
  settlement commit. A crash before it under-counts; nothing re-meters a
  contract.
- **Running total**: counts only blocks that started inside its period, so usage
  recorded before a reset is not added after it.
- Retention: monthly usage 400 days; drain rows 24 hours. The task reaps up to
  1,000 rows of each per run.

### Enforcement

`createTransferEscrowInTx` (the contract hot path) refuses a positive contract
whose paying client, or that client's top-level client, is capped. It runs
before the Redis/PostgreSQL admission split, so both paths are covered. The
refusal is the ordinary `Insufficient balance (0).`, which the client sees as
`ContractError_InsufficientBalance`. Zero-byte contracts keep working.

- The capped set is an in-memory snapshot of capped top-level clients and their
  networks. It is refreshed every 5s with one query over the partial indexes
  (`total_capped OR monthly_capped_period_start = <this month> OR
  monthly_byte_limit = 0`), and on month rollover. A `POST` invalidates this
  host's snapshot at once.
- A payer in a network without a capped client adds **no query**. In a network
  with one, the payer's top-level client is resolved once and cached for the
  process, since `source_client_id` never changes.
- Latency: a cap acts within about 1.5–2 minutes of the usage that crosses it:
  up to one 30s block, plus two blocks of finality, the 30s drain cadence and the
  5s snapshot. A client can exceed a cap by that much traffic, plus any
  contracts already open. Other hosts see a `POST` within 5s.

## 3. Services contact sales

`POST /services/contact-sales`, public (`security: []`):

```json
{"name","email","company","monthly_active_users":5000,"monthly_data_budget_byte_count":10000000000000,"message":"(the use case, optional)","website":"(honeypot)"}
```

→ `{"request_id":"<uuid>"}`

- **Rate limit first**: every request counts, at 5 per hour per client address
  (subnet-bucketed, `server.CheckIpRateLimitAttempt`) and 60 per minute
  globally. Over the limit it answers HTTP 429 (no `Retry-After`: the sliding
  window's reset is unknown). A rate limit store error lets the request through.
- **Honeypot**: a non-empty `website` answers like a success and stores nothing.
- **Validation**: 200 with `error.message`. Name and company are 1–256
  characters with no control characters; the email is one bare address; monthly
  active users are 1–10⁹; the budget is 0–2^60 bytes; the use case is ≤ 4000
  characters.
- **Storage**: `services_lead (lead_id, create_time, name, email, company,
  monthly_active_users, monthly_data_budget_byte_count, message, notify_time)`.
- **Slack**: posted asynchronously (the response never waits) to the incoming
  webhook in `vault/main/sales.yml`:

  ```yaml
  slack_webhook_url: https://hooks.slack.com/services/T000/B000/XXXX
  ```

  - Re-read every 60s, so no restart is needed. Only `https` URLs are used.
    Without the key, the lead is only stored.
  - Title `New Services lead`. Every user field is escaped (`&`, `<`, `>`), so a
    lead cannot mention `<!channel>` or spoof a link. `unfurl_links` and
    `unfurl_media` are off.
  - Three attempts (10s each, 2s and 6s backoff). A 4xx other than 429 stops.
    Success sets `notify_time`.
  - The webhook URL is never logged: a transport error is unwrapped from
    `*url.Error`, whose text includes the URL.

## 4. ACL groups

A top-level client is in the `default` group unless its network's root
credential (the root JWT or an API key) puts it in `isolated`. Only a non-default
group is stored, in `network_client_acl_group (client_id PK, network_id,
acl_group, update_time)`; no row means `default`.

### API

- `POST /network/client-acl-group` — the root credential only (a client token is
  refused), for a top-level client of the caller's network. Body
  `{"client_id","acl_group":"default"|"isolated"}` → `{"client_id","acl_group"}`.
  Refusals answer 200 with `error.message`: an unknown or foreign client is "Client
  not found in this network.", a child client "ACL groups apply to top-level
  clients.".
- `GET /network/client-acl-group?client_id=` — the root credential reads any
  top-level client of its network; a client token reads its own (a child client's
  token reads its top-level client's).

### Semantics

An isolated client:

- never appears in the network's peer list: `GET /network/peers`, the peer
  listener's full reads and its key-event deltas;
- receives no peer list: its resident runs no peer listener, and its own
  `GET /network/peers` is empty;
- does not count toward the peer valve (`NetworkPeersEnabled`), like a provider
  install, so a network of isolated installations keeps peer registration (and
  with it the connected count) on;
- still counts toward the top-level client limit and the concurrent connection
  limit.

### Implementation

- `GetNetworkPeerProfile` resolves `NetworkPeerCategoryIsolated` (proxy and
  provider categories take precedence). The resident registers an isolated client
  in the counted proxy zset (`AddNetworkIsolatedPeer` at announce and as the
  heartbeat, `RemoveNetworkIsolatedPeer` on close) and starts no listener.
- A change takes effect promptly. After the change commits,
  `applyNetworkClientAclGroupChange` drops the network's valve cache entry and
  moves the registration. Isolating runs `isolateNetworkPeer`, one Lua
  transition that removes the meta, member key, connected entry and any
  disconnect marker (no marker, so the client is not reported as recently
  disconnected), advances the mutation fence, bumps the version, and moves a
  connected client to the proxy zset with its remaining ttl. Returning to
  default removes it from the proxy zset. Either way the client's resident record
  is retired, so its poll closes it and the next connection registers with the
  new group.
- The heartbeat's re-add path re-lists only a client still in the default group,
  so a heartbeat racing an isolation cannot list the client again.

## Migrations (appended after 796)

| # | Change |
| --- | --- |
| 797 | `network_top_level_client_limit` |
| 798 | `network_client_data_cap` (+ partial indexes for the capped snapshot and the list), `network_client_data_usage`, `network_client_data_usage_drain`, `network_client_data_usage_rollup` |
| 799 | `services_lead` |
| 800 | `network_client_acl_group` |

All four are new tables: nothing is rewritten. Each must exist before a binary
that reads it serves (the peer profile and the peer valve query 800).

## Observability

- `urnetwork_client_data_usage_record_total{result}`: `ok` or `error` (redis).
- `urnetwork_client_data_usage_drain_total{result}`: `ok`, `replay`,
  `no_clients`, `malformed_field`, `malformed_value`.
- `urnetwork_services_lead_total{result}`: `stored`, `honeypot`, `invalid`,
  `rate_limited`, `rate_limit_error`.
- `urnetwork_services_lead_notify_total{result}`: `ok`, `error`, `unconfigured`.

Client-driven errors are logged only at V(1). A Slack failure is logged at the
default level, with the lead id only.

## Test plan

Every changed file has tests; the coverage table is in the branch's final report.

- Pure (run on the branch):
  - `model/network_client_data_cap_unit_test.go`: merge JSON, limits, periods,
    cursor/limit parsing, capped reasons, the drain block window, field parsing,
    capped markers, the wire shape, API-key/root/client session acceptance, and
    `contractOrigin` against `contractParticipantsFromRows`.
  - `model/services_lead_unit_test.go`: validation, Slack escaping and text,
    config parsing, and the notifier against `httptest` (retry, 429 retry, 4xx
    stop, give up, cancel, URL-free errors).
  - `model/services_lead_config_unit_test.go`: the sales.yml reload window, the
    absent/empty/non-https configs, the unconfigured path posting nothing, an
    unclassifiable address passing the limiter, the limiter settings, URL-free
    post errors.
  - `model/network_client_limit_unit_test.go`: the override cache and its bound,
    the folded enforcement switch, the override driving the concurrent and
    provider intent limits.
  - `model/network_client_acl_group_unit_test.go`: group parsing, the category
    precedence, the refusals before any query, the valve cache `Remove`.
  - `api/handlers/embed_handlers_test.go`: 401 without a token on each
    authenticated route, the malformed contact body (400), the 429 mapping, the
    wire field names.
  - `bringyourctl/network_client_limit_test.go`: the usage forms and the help text.
  - `taskworker/network_client_data_usage_registration_test.go`: the rollup
    registered in both workload profiles.
  - `api/spec_conformance_test.go`, with registry entries for the six routes.
- DB-backed (after merge, under the owner rule):
  - `TestSetClientDataCapMergeSemantics` (root JWT and API key),
    `TestGetClientDataCapAuth`, `TestListClientDataCapsPaging`,
    `TestClientDataCapEscrowAdmission`, `TestRollupClientDataUsage` (exactly-once,
    child attribution, reset period, markers → admission),
    `TestNetworkTopLevelClientLimitOverride`, `TestServicesContactSales`
    (store, honeypot, validation, 429).
  - `TestNetworkClientAllowanceAppliesToTheConcurrentLimit`,
    `TestNetworkClientLimitOverrideCache`, `TestNetworkTopLevelClientLimitInTx`.
  - `TestNetworkClientAclGroupAuth`, `TestNetworkClientAclGroupPeerExclusion`,
    `TestIsolateNetworkPeerMovesTheRegistration`,
    `TestNetworkPeersEnabledExcludesIsolatedClients`.
  - `TestSettlementMetersThePayingClient`, `TestClientDataCapForeignNetworkIsNotFound`.
  - `TestServicesContactSalesGlobalRateLimit`, `TestServicesContactSalesPostsTheLeadToSlack`.
  - `TestNetworkClientLimitCommand` (bringyourctl), `TestRollupClientDataUsageScheduleArmsOneOwner`
    (taskworker).
- Integration, `server/connect` (after merge, under the owner rule):
  `TestExchangeAclGroupIsolatesAPeer`, `TestExchangeDataCapPausesStopsAndResumesTraffic`,
  `TestConnectEmbedPlanAllowanceLiftsTheConcurrentLimit`.
  - The existing settlement participant matrices exercise the `contractOrigin`
    extraction: `TestContractPayoutParticipantMatrix`,
    `TestCompanionContractPayoutParticipantsJoinByStreamId`,
    `TestContractParticipantPayoutDisputeOutcomes`,
    `TestContractExtenderPayoutParticipantMatrix`,
    `TestCompanionContractExtenderPayoutPaysTheReversedParties`.

## Follow-ups

- An isolated client's SDK keeps the last peer list it received before the
  isolation; the server sends it nothing afterward. An explicit empty update on
  isolation would clear it, at the cost of a resident change.
- A network over the peer valve registers no peers at all, so its connected
  count, and with it the concurrent gate, sees none of its clients (this
  predates the branch). Isolated installations do not count toward the valve, so
  isolating an embedded fleet keeps registration and counting on.

- A cap row for a client the inactive reap deletes lingers. The list hides it
  (inner join), and a capped one in the snapshot is harmless (the client makes no
  contracts). A bounded sweep could remove them.
- The create cap's refusal text is unchanged (`Client limit exceeded.`), so
  existing clients and tests keep matching. Pointing customers at the Embed plan
  belongs in the docs and the SDK's error mapping.
