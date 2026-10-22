# Crypto payments: the server's API, as built

The hand-off from the server to the OpenAPI spec (`connect/api/bringyour.yml`) and the SDKs. It lists every
endpoint of the crypto payment rail with the exact JSON the server reads and writes. The Go types are in
`controller/crypto_pay_controller.go` and keep these names; `api/spec_conformance_test.go` maps each route to them.
The contract is `mmm/ZCASH-CONTRACT.md` §2; where this file is more precise, it describes what the server
does.

Conventions:

- Every body is JSON. A refusal is HTTP 200 with an `error` object `{"code", "message"}`; clients pick a localized
  message by `code` and fall back to `message`. The codes are the `PurchaseErrorCode*` values.
- Atomic amounts are decimal strings (`"72386000000"`). Display amounts are exact decimal strings in coin units with
  no trailing zeros and at most 6 decimals (`"0.072386"`, `"40.000202"`, `"0"`). Never floats.
- Times are RFC 3339 strings (Go `time.Time`); a time that does not apply is `null`.
- `status_token` is the invoice's bearer secret. Keep it in memory, device storage, request bodies and URL fragments;
  never in a query string or a log. The server stores only its SHA-256.

## GET /pay/crypto/assets

No auth. Limited per client address (60 a minute); over the limit the answer is HTTP 429.

Response `CryptoPaymentAssetsResult`:

```json
{"assets": [
  {"asset": "usdc_solana", "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
   "confirmations_required": 1, "quote_ttl_seconds": 3600},
  {"asset": "xmr", "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
   "confirmations_required": 1, "quote_ttl_seconds": 1800}
]}
```

`CryptoPaymentAsset`: `asset` (string: `usdc_solana` | `zec` | `xmr`), `plans` (string array, never `monthly`),
`items` (string array), `confirmations_required` (integer), `quote_ttl_seconds` (integer). `plans` and `items` are
always arrays, `[]` when the coin sells none, never `null`; a coin that sells neither is not listed. Only coins that
are on are listed; an empty list is `"assets": []`. The order means nothing.

## Additions to GET /subscription/balance (`SubscriptionBalanceResult`)

- `crypto_assets`: `[CryptoPaymentAsset]`, the list above. Left out when no coin is on.
- `crypto_term`: `CryptoTerm`, left out when the network has no one-time crypto credit (markets `solana`, `monero`,
  `zcash`) whose grace has not ended:

  ```json
  {"market": "monero", "term_end_time": "2027-10-20T18:00:00Z", "grace_end_time": "2027-11-19T18:00:00Z"}
  ```

  `term_end_time` = `grace_end_time` − 30 days. Show the renewal notice while `term_end_time <= now <
  grace_end_time`, never before.

## POST /pay/crypto/intent

Bearer auth (any client token of the network). Request `CreateCryptoPaymentIntentArgs`:

```json
{"asset": "xmr", "plan": "yearly", "item_id": "", "storefront_country": "US"}
```

| Field | Type | |
|---|---|---|
| `asset` | string | `usdc_solana`, `zec` or `xmr` |
| `plan` | string, optional | `yearly` or `yearly_onboarding` |
| `item_id` | string, optional | `data_1tib` or `data_10tib` |
| `storefront_country` | string, optional | the store's storefront country (price tier) |

Exactly one of `plan` and `item_id`. Delivery is `account`: the plan or the data lands on the caller's network.
Refusals, in the order they are checked:

| `error.code` | When |
|---|---|
| `invalid_request` | unknown `asset`; neither or both of `plan` and `item_id` |
| `asset_unavailable` | the coin is off |
| `plan_unavailable` | the coin does not sell the plan (`monthly` included), or no price is configured |
| `item_unavailable` | the coin does not sell the item; no price is configured; the price is over the per-invoice cap |
| `guest_sign_in_required` | plans only: the network has no sign-in method |
| `offer_unavailable` | `yearly_onboarding` while the welcome offer is not redeemable |
| `rate_limited` | more than 10 intents a minute for this network (shared with `/solana/payment-intent`) |
| `asset_unavailable` | the coin cannot be quoted now: fewer than two fresh rates, rates more than 3% apart, the scanner or the node lagging, the address pool empty, too many open invoices |
| `start_failed` | a database failure; a retry may work |

An open invoice of this network for the same asset, item and delivery (not credited, not expired, nothing in
review) is returned again, with the same address and quote and a new `status_token` (every token handed out stays
valid). The delivery is part of the key: `/pay/data/crypto-intent` naming a network never returns the network's
signed-in invoice, and the reverse.

A coin switched off (`asset_unavailable`) still credits the invoices it already handed out: the kill switch stops
new invoices and re-quotes only.

Response `CryptoPaymentIntentResult`:

```json
{
  "intent_id": "0b6f2f7e-4f1c-4a8e-9d1a-1f6a2b3c4d5e", "status_token": "kq3…43 chars…",
  "asset": "xmr", "plan": "yearly", "item_id": "",
  "delivery": "account", "network_name": "",
  "tier": "standard", "offer_applied": false,
  "price_usd": 40, "regular_price_usd": 40, "rate_usd": 555.36,
  "address": "8…", "amount": "0.072386", "amount_atomic": "72386000000",
  "memo": "",
  "uri": "monero:8…?tx_amount=0.072386&tx_description=URnetwork%20Pro%20yearly",
  "quoted_at": "2026-10-20T18:00:00Z", "expires_at": "2026-10-20T18:30:00Z",
  "confirmations_required": 1,
  "status": { /* CryptoPaymentStatusResult */ }
}
```

| Field | Type | |
|---|---|---|
| `intent_id` | string (uuid) | |
| `status_token` | string | 32 random bytes, base64url without padding (43 characters) |
| `asset`, `plan`, `item_id` | string | `plan` is `""` for a data pack, `item_id` is `""` for a plan |
| `delivery` | string | `account`, `network` or `code` |
| `network_name` | string | the named network (delivery `network`), else `""` |
| `tier` | string | the price tier of a plan; `""` for a data pack |
| `offer_applied` | boolean | the welcome offer's price |
| `price_usd` | number | what the buyer pays |
| `regular_price_usd` | number | the item's regular price (the full yearly price under the offer) |
| `rate_usd` | number | the coin's USD rate the amount was quoted at; `1` for USDC |
| `address` | string | where to pay: a Monero subaddress, a Zcash unified address, or the Solana merchant address |
| `amount`, `amount_atomic` | string | exactly what to send. XMR and ZEC: price × (1 + 0.5% buffer) ÷ rate, rounded up to 6 decimals ($40 at $555.36 is `0.072386`; mmm/ZCASH-CONTRACT.md §2.3's example `0.072387` is off by one). USDC: the price plus a unique sub-cent suffix |
| `memo` | string | USDC only: the Solana Pay reference, the memo of a transfer sent by hand; else `""` |
| `uri` | string | the payment URI to show as a QR and to open. Never built by a client |
| `quoted_at`, `expires_at` | string (time) | |
| `confirmations_required` | integer | |
| `status` | `CryptoPaymentStatusResult` | the invoice's current status; left out on a refusal |
| `error` | `CryptoPaymentError` | left out on success |

On a refusal every other field is empty (`""`, `0`, `false`, `null`) and `status` is left out.

URIs:

- XMR: `monero:<address>?tx_amount=<amount>&tx_description=<description>`
- ZEC (ZIP-321): `zcash:<address>?amount=<amount>&message=<description>` (no memo)
- USDC (Solana Pay): `solana:<recipient>?amount=<amount>&spl-token=<mint>&reference=<reference>&label=URnetwork&message=<description>`

`description` is one of `URnetwork Pro yearly`, `URnetwork Pro yearly (welcome offer)`, `URnetwork 1 TiB data`,
`URnetwork 10 TiB data`, percent-encoded with spaces as `%20`.

## POST /pay/data/crypto-intent

No auth. Limited per client address (10 a minute). Request `CreateDataCryptoPaymentIntentArgs`:

```json
{"asset": "zec", "item_id": "data_1tib", "network_name": "alice", "email": ""}
```

| Field | Type | |
|---|---|---|
| `asset` | string | |
| `item_id` | string | `data_1tib` or `data_10tib` |
| `network_name` | string, optional | set: delivery `network`; empty: delivery `code` |
| `email` | string, optional | delivery `code`: where the code is also emailed once paid |
| `plan` | string, optional | never accepted here: a plan is `invalid_request` (the field exists only so that refusal is explicit) |

Refusals: as for `/pay/crypto/intent` without the plan rules, plus `invalid_email` (an email that does not look
like one) and `network_not_found` (no network has that name). Delivery `network` reuses an open invoice for the
network, asset and item as above; delivery `code` is a new invoice every time. The response is
`CryptoPaymentIntentResult`.

Delivery `code` (no network named) has limits of its own, so anonymous invoices cannot take every receive address:

- `asset_unavailable` when the coin does not deliver codes. USDC on Solana does not until every server runs the
  build with migration 853 (`code_delivery: false` in the server config); the coin's network delivery is unaffected.
- `rate_limited` past a few code invoices an hour from one client address (counted in memory by a hash of the
  address; no address is stored).
- `asset_unavailable` while the coin has its maximum of open code invoices.

## POST /pay/crypto/status

No auth; the token is the credential. Limited per client address (60 a minute); over the limit the answer is HTTP
429 (keep the last state and poll again later). Request `CryptoPaymentStatusArgs`:

```json
{"intent_id": "0b6f2f7e-…", "status_token": "kq3…"}
```

Response `CryptoPaymentStatusResult`:

```json
{
  "intent_id": "0b6f2f7e-…", "asset": "xmr", "plan": "yearly", "item_id": "",
  "delivery": "account", "network_name": "",
  "status": "confirming", "review_reason": "",
  "amount": "0.072386", "amount_atomic": "72386000000",
  "received": "0.072386", "received_atomic": "72386000000",
  "remaining": "0", "remaining_atomic": "0",
  "confirmations": 0, "confirmations_required": 1,
  "expires_at": "2026-10-20T18:30:00Z", "credited_at": null,
  "balance_code": ""
}
```

| Field | Type | |
|---|---|---|
| `status` | string | below |
| `review_reason` | string | with `review`: `underpaid`, `late_underpaid`, `extra`, `orphaned`, `voided` or `credit_failed`; else `""`. Show the review copy for any value |
| `amount(_atomic)`, `received(_atomic)`, `remaining(_atomic)` | string | `received` counts mempool and mined receipts, but not a mempool payment the wallet flags as a double spend (it counts once mined) |
| `confirmations` | integer | the lowest among the receipts that make up the payment (0 in the mempool) |
| `confirmations_required` | integer | |
| `expires_at` | string (time) | `null` for `unknown` |
| `credited_at` | string (time) or `null` | |
| `balance_code` | string | delivery `code`, once paid: the code to redeem; else `""` |
| `error` | `CryptoPaymentError` | `quote_expired` with `expired`; `underpaid` with `review`/`underpaid` and `review`/`late_underpaid`; else left out |

`status`:

| status | when |
|---|---|
| `unknown` | no such invoice, or the token does not match. Nothing else is filled in |
| `paid` | credited. A credited invoice that later received a second payment stays `paid` |
| `review` | needs an operator: `underpaid` (short at expiry), `late_underpaid` (arrived after expiry, worth less than the price at its first-seen rate), `orphaned` (a credited payment left the chain: its entitlement was ended and its credit taken back, `credited_at` is `null` again, until an operator grants it again), `voided` (a payment to an invoice an operator closed), `credit_failed` (the payment is there but its credit kept failing; an operator credits it by hand) |
| `confirming` | received at least 99.5% of the amount (or, after expiry, a late payment worth the price at its first-seen rate), not yet at `confirmations_required` |
| `seen` | something arrived, less than 99.5% of the amount; `remaining` says how much more |
| `expired` | past `expires_at` with nothing received |
| `pending` | nothing received yet |

`usdc_solana` reports only `pending`, `paid` and `expired` (Helius reports confirmed transfers only); a voided USDC
invoice is `expired`, and a payment to it is held for an operator. A late payment that covers the price is
credited.

On time is judged by the chain's time: a payment counts as made when its block was (or when it reached the
mempool), not when the server noticed, so a payment made before `expires_at` is on time even if the server sees it
later.

## POST /pay/crypto/requote

No auth; the token is the credential. Limited per client address (10 a minute; `rate_limited`). Request
`CryptoPaymentStatusArgs`. Response `CryptoPaymentIntentResult`, carrying the caller's own `status_token`.

- Allowed only while nothing was received (`pending` or `expired`) and the invoice is not closed; otherwise
  `invalid_request`. An unknown invoice or token is `invalid_request`.
- XMR and ZEC: a new rate, amount and expiry for the **same address**; the old quote is kept in the history.
  A coin that cannot be quoted now is `asset_unavailable`.
- USDC: the amount is already in dollars, so only `expires_at` moves.

## Error codes added to `PurchaseErrorCode*`

- `asset_unavailable`: the coin is off, or cannot be quoted right now (rates, the scanner or its node lagging or
  not knowing the network's height, the address pool, too many open invoices), or does not deliver balance codes
  yet. UI: "Temporarily unavailable".
- `quote_expired`: the quote ran out with nothing received. UI offers "Refresh quote".
- `underpaid`: the invoice ended short. UI shows the review copy.

`CryptoPaymentError` is `{"code": string, "message": string}`.

## Unchanged

`/solana/payment-intent`, `/solana/payment-transaction`, `/pay/data/solana-intent` and `/pay/data/solana-status`
keep their shapes and behavior. `/solana/payment-intent` now has the per-network limit (10 a minute, `rate_limited`),
shared with `/pay/crypto/intent`. `/pay/data/solana-status` answers `unknown` for a reference of a USDC invoice made
through `/pay/crypto/intent` or `/pay/data/crypto-intent`: such a reference is on the chain, and must not name the
network it paid for. Those invoices are read through `/pay/crypto/status`.
