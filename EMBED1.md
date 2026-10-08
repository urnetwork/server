# EMBED1: server support for embedding URnetwork

Status: implemented on `feat/embed-business`; migrations 797–799 **not applied**
(owner rule: merge to main, pull and push before any migration, including DB test
suites). Companion work: the ur.io Embed guide and Services page (mmm), the embed
examples and `EMBED_CONTRACT.md` (examples repo), and the OpenAPI entries in
`connect/api/bringyour.yml`.

A customer embeds the SDK in its own app. Its backend holds the network's root
token (or an API key), authenticates its own users, and provisions **one
top-level client per running installation** with `POST /network/auth-client`. Three
server features support that:

1. **The Embed plan limit**: a per-network override of the 100 active top-level
   client cap.
2. **Per-client data caps and usage**: per installation, monthly and/or
   running-total caps, plus monthly usage for every top-level client.
3. **Services contact sales**: a public lead form that stores each lead and
   posts it to Slack.

## 1. The Embed plan limit

`LimitTopLevelClientIdsPerNetwork` (100) caps **active top-level clients** per
network: one per running installation. A client idle for 30 days
(`TopLevelClientIdleExpiration`) is deactivated and stops counting; provider
installs (`provide_intent`) and child clients (`source_client_id`) never count.
The cap is enforced only while pro.yml `enforce_concurrent_clients` is on.

An Embed plan raises the cap for one network:

- Storage: `network_top_level_client_limit (network_id PK, top_level_client_limit,
  update_time)`. No row means the default 100.
- Enforcement: the `AuthNetworkClient` create cap reads the network's limit in the
  provisioning transaction (`networkTopLevelClientLimitInTx`) and counts with
  `LIMIT limit+1`. Nothing else changes.
- The peer valve (`model/peer_model.go`) keeps the constant: a network past 100
  recently active top-level clients has its peer list off, so a large embedded
  network's users stay invisible to each other.
- Ops: `bringyourctl network client-limit --network_id=<id> [--set=<n> | --clear]`
  prints the effective limit and whether it comes from an Embed plan
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

## Migrations (appended after 796)

| # | Change |
| --- | --- |
| 797 | `network_top_level_client_limit` |
| 798 | `network_client_data_cap` (+ partial indexes for the capped snapshot and the list), `network_client_data_usage`, `network_client_data_usage_drain`, `network_client_data_usage_rollup` |
| 799 | `services_lead` |

All three are new tables: nothing is rewritten. Each must exist before a binary
that reads it serves.

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

- Pure (run on the branch):
  - `model/network_client_data_cap_unit_test.go`: merge JSON, limits, periods,
    cursor/limit parsing, capped reasons, the drain block window, field parsing,
    capped markers, the wire shape, API-key/root/client session acceptance, and
    `contractOrigin` against `contractParticipantsFromRows`.
  - `model/services_lead_unit_test.go`: validation, Slack escaping and text,
    config parsing, and the notifier against `httptest` (retry, 429 retry, 4xx
    stop, give up, cancel, URL-free errors).
  - `api/spec_conformance_test.go`, with registry entries for the four routes.
- DB-backed (after merge, under the owner rule):
  - `TestSetClientDataCapMergeSemantics` (root JWT and API key),
    `TestGetClientDataCapAuth`, `TestListClientDataCapsPaging`,
    `TestClientDataCapEscrowAdmission`, `TestRollupClientDataUsage` (exactly-once,
    child attribution, reset period, markers → admission),
    `TestNetworkTopLevelClientLimitOverride`, `TestServicesContactSales`
    (store, honeypot, validation, 429).
  - The existing settlement participant matrices exercise the `contractOrigin`
    extraction: `TestContractPayoutParticipantMatrix`,
    `TestCompanionContractPayoutParticipantsJoinByStreamId`,
    `TestContractParticipantPayoutDisputeOutcomes`,
    `TestContractExtenderPayoutParticipantMatrix`,
    `TestCompanionContractExtenderPayoutPaysTheReversedParties`.

## Follow-ups

- A cap row for a client the inactive reap deletes lingers. The list hides it
  (inner join), and a capped one in the snapshot is harmless (the client makes no
  contracts). A bounded sweep could remove them.
- The create cap's refusal text is unchanged (`Client limit exceeded.`), so
  existing clients and tests keep matching. Pointing customers at the Embed plan
  belongs in the docs and the SDK's error mapping.
