# Crypto payments: the server's API, as built

The hand-off from the server to the OpenAPI spec (`connect/api/bringyour.yml`) and the SDKs. It lists every
endpoint of the crypto payment rail with the exact JSON the server reads and writes. The Go types are in
`controller/crypto_pay_controller.go` and `controller/crypto_pay_quote.go` and keep these names;
`api/spec_conformance_test.go` maps each route to them. The contract is `mmm/ZCASH-CONTRACT.md` §2 for USDC, Zcash
and Monero and `mmm/BTCPAY.md` §17 for TAO, Bitcoin, ETH and SOL; where this file is more precise, it describes what
the server does.

Every coin can be paid end to end through this API alone, with the API's normal auth and no app: list the coins
(`GET /pay/crypto/assets`), price an item (`POST /pay/crypto/quote`), make the invoice (`POST /pay/crypto/intent`
with a client token, or `POST /pay/data/crypto-intent` without one), send the coin to `address` (or open `uri`),
and poll `POST /pay/crypto/status` until `paid`, reading top-ups (`seen`, `remaining`), confirmations, overpayments
(`excess`) and review states on the way.

Conventions:

- Every body is JSON. A refusal is HTTP 200 with an `error` object `{"code", "message"}`; clients pick a localized
  message by `code` and fall back to `message`. The codes are the `PurchaseErrorCode*` values.
- Atomic amounts are decimal strings (`"72386000000"`). Display amounts are exact decimal strings in coin units with
  no trailing zeros and at most the coin's quote decimals (8 for Bitcoin, 6 for every other coin: `"0.072386"`,
  `"0.00054622"`, `"0"`). Never floats.
- Times are RFC 3339 strings (Go `time.Time`); a time that does not apply is `null`.
- `status_token` is the invoice's bearer secret. Keep it in memory, device storage, request bodies and URL fragments;
  never in a query string or a log. The server stores only its SHA-256.

## The coins

| `asset` | Coin | `network` | Decimals | Quote decimals | Buffer | Link (`uri`) | Credited at | Detected through |
|---|---|---|---|---|---|---|---|---|
| `usdc_solana` | USDC on Solana | `solana` | 6 | 6 | none (a unique sub-cent suffix) | Solana Pay with `spl-token` | Helius's confirmed transfer | the Helius webhook; the hourly reconcile reads the chain for each open reference |
| `zec` | Zcash (shielded) | `zcash` | 8 | 6 | 0.5% | ZIP-321 | 10 confirmations | our zecd (watch-only) |
| `xmr` | Monero | `monero` | 12 | 6 | 0.5% | `monero:` | 1 confirmation | our monero-wallet-rpc (view-only) |
| `tao` | TAO | `bittensor` | 9 | 6 | 1% | none: address and amount | finalized | snow's Finney node (finalized blocks) |
| `btc` | Bitcoin | `bitcoin` | 8 | 8 | 0.5% | BIP 21 | 1 confirmation, final at 6 | Alchemy (address history, UTXO API) |
| `eth` | ETH | `ethereum` (mainnet) | 18 | 6 | 0.5% | EIP-681 | finalized (about 15 minutes) | Alchemy (incoming transfers, finalized tag) |
| `sol` | native SOL | `solana` | 9 | 6 | 0.5% | Solana Pay, no `spl-token` | finalized | Helius RPC (signatures of the address) |

TAO, Bitcoin, ETH and SOL are received into our Coinbase account: every invoice of these coins has a Coinbase deposit
address of its own (BTCPAY.md §17.2), made ahead by the taskworker and named with the id of the invoice it will
serve. A top-up to the same address counts toward the invoice; a shortfall at expiry goes to review. Credit comes
from the chain alone, never from Coinbase's account view, and no balance is read (Coinbase moves funds out of its
deposit addresses at any time).

Every coin sells the yearly plan, the welcome offer, the data packs and balance codes, at the card price in USD;
monthly is card only.

## GET /pay/crypto/assets

No auth. Limited per client address (60 a minute); over the limit the answer is HTTP 429.

Response `CryptoPaymentAssetsResult`:

```json
{"assets": [
  {"asset": "usdc_solana", "network": "solana", "decimals": 6, "quote_decimals": 6, "payment_link": true,
   "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
   "confirmations_required": 1, "quote_ttl_seconds": 3600, "available": true},
  {"asset": "btc", "network": "bitcoin", "decimals": 8, "quote_decimals": 8, "payment_link": true,
   "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
   "confirmations_required": 1, "quote_ttl_seconds": 1800, "available": true}
 ],
 "unavailable_assets": [
  {"asset": "tao", "network": "bittensor", "decimals": 9, "quote_decimals": 6, "payment_link": false,
   "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
   "confirmations_required": 1, "quote_ttl_seconds": 1800, "available": false, "unavailable_reason": "address_pool_empty"},
  {"asset": "eth", "network": "ethereum", "decimals": 18, "quote_decimals": 6, "payment_link": true,
   "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
   "confirmations_required": 1, "quote_ttl_seconds": 1800, "available": false, "unavailable_reason": "not_configured"}
]}
```

`CryptoPaymentAsset`: `asset` (string: `usdc_solana` | `zec` | `xmr` | `tao` | `btc` | `eth` | `sol`), `network`
(string, above), `decimals` (integer: `amount_atomic` = `amount` × 10^decimals), `quote_decimals` (integer: the
most decimals an amount has), `payment_link` (boolean: false for `tao`), `plans` (string array, never `monthly`),
`items` (string array), `confirmations_required` (integer), `quote_ttl_seconds` (integer), `available` (boolean),
`unavailable_reason` (string, left out when `available`). `plans` and `items` are always arrays, `[]` when the coin
sells none, never `null`; a coin that sells neither is not listed. The order means nothing.

`assets` lists only the coins that can be paid with now: a coin that is on and whose quote the server would make.
A client offers every coin it names and no other. A coin that is on but cannot be paid with now is in
`unavailable_assets` instead, with `available: false` and the quote path's reason, for a page that shows it as
temporarily unavailable:

| `unavailable_reason` | When |
|---|---|
| `not_configured` | the coin is switched on but the vault does not name everything selling it needs: its chain source (Alchemy for Bitcoin and ETH, Helius for SOL, snow's Finney node for TAO, the wallet daemon for Zcash and Monero) or, for TAO, Bitcoin, ETH and SOL, the Coinbase key |
| `scanner_lagging` | the coin's scanner has not succeeded within three of its scan intervals (or never), or lags the network by more than `max_lag_blocks` |
| `network_height_unknown` | the scanner knows no network height or no wallet height (a stalled source reports none), so the lag cannot be judged |
| `address_pool_empty` | the coin has no free receive address (for TAO, Bitcoin, ETH and SOL: Coinbase made none yet, for example while our Coinbase account has no wallet for the coin) |
| `rates_unavailable` | the coin's rate sources were asked within `rate_stale_after` and left fewer than two fresh answers that agree |

Every coin of `assets` has `available: true`. A coin switched off is in neither list. Both lists are arrays, `[]`
when empty. The decision is the quote path's own, cached for 10 seconds per process, so the list, the balance, a new
invoice and a re-quote agree; the rate part is judged from the cached answers alone, so the list never waits on a
rate source. A coin that is listed can still be refused at the intent for a reason the list does not judge (rates
nobody asked lately, too many open invoices): clients keep handling `asset_unavailable` there.

TAO, Bitcoin, ETH and SOL are switched on by their entry in `config/<env>/crypto_pay.yml`, as Zcash and Monero
are. Until the file names a coin, the coin is dormant whatever the vault holds: it is in neither list (nor the
balance's), a quote or an invoice is refused, and its scanner, address refill and reconcile legs return before any
chain, Coinbase or Finney call. The entry needs only what the coin sells; the coin's own defaults do the rest:

```yaml
crypto_pay:
  assets:
    btc:
      enabled: true
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
```

Then the coin is sold once its backend is configured and healthy (the reasons above). `enabled: false` is the kill
switch: it stops new invoices and re-quotes, while the coin's open invoices are still scanned and credited. A
process reads crypto_pay.yml when it starts, so api, alt and taskworker are restarted after the config release.
The order of a coin's launch: ur.io's widget update first, then the config release with the coin's entry, and for
TAO the first scan only while the SN25 validator's deploy gate allows it.

## Additions to GET /subscription/balance (`SubscriptionBalanceResult`)

- `crypto_assets`: `[CryptoPaymentAsset]`, the `assets` list above: only the coins that can be paid with now. Left
  out when there is none.
- `crypto_unavailable_assets`: `[CryptoPaymentAsset]`, the `unavailable_assets` list above, each with `available:
  false` and its reason, for an app that shows a coin greyed out (the "More coins" tab while all four of its coins
  are unavailable). Never offered. Left out when there is none.
- `crypto_term`: `CryptoTerm`, left out when the network has no one-time crypto credit (markets `solana`, `monero`,
  `zcash`, `bittensor`, `bitcoin`, `ethereum`) whose grace has not ended:

  ```json
  {"market": "bitcoin", "term_end_time": "2027-10-20T18:00:00Z", "grace_end_time": "2027-11-19T18:00:00Z"}
  ```

  `term_end_time` = `grace_end_time` − 30 days. Show the renewal notice while `term_end_time <= now <
  grace_end_time`, never before. Native SOL credits under `solana`, as USDC does.

## POST /pay/crypto/quote

What a plan or a data pack costs in a coin now, without an invoice: no receive address is drawn and nothing is
written. The bearer token is optional: with one, the price is the network's (its tier, and the welcome offer when
the network may redeem it); without one, a plan is quoted at the caller's tier and `yearly_onboarding` is refused.
Limited per client address (60 a minute); over the limit the answer is HTTP 429. The invoice quotes again when it
is made, by the same rules; its amount is the one to pay.

Request `CryptoPaymentQuoteArgs`:

```json
{"asset": "btc", "plan": "yearly", "item_id": "", "storefront_country": "US"}
```

Response `CryptoPaymentQuoteResult`:

```json
{"asset": "btc", "plan": "yearly", "item_id": "", "tier": "standard", "offer_applied": false,
 "price_usd": 40, "regular_price_usd": 40, "rate_usd": 73596.9,
 "amount": "0.00054622", "amount_atomic": "54622",
 "network": "bitcoin", "decimals": 8, "quote_decimals": 8, "payment_link": true,
 "quote_ttl_seconds": 1800, "confirmations_required": 1, "quoted_at": "2026-11-10T18:00:00Z"}
```

`tier` is `""` for a data pack. USDC is quoted at its price (`rate_usd` 1); its invoice adds a unique sub-cent
suffix. Refusals: as for
`/pay/crypto/intent` (`invalid_request`, `asset_unavailable`, `plan_unavailable`, `item_unavailable`,
`offer_unavailable`), with every other field empty.

## POST /pay/crypto/intent

Bearer auth (any client token of the network). Request `CreateCryptoPaymentIntentArgs`:

```json
{"asset": "xmr", "plan": "yearly", "item_id": "", "storefront_country": "US"}
```

| Field | Type | |
|---|---|---|
| `asset` | string | `usdc_solana`, `zec`, `xmr`, `tao`, `btc`, `eth` or `sol` |
| `plan` | string, optional | `yearly` or `yearly_onboarding` |
| `item_id` | string, optional | `data_1tib` or `data_10tib` |
| `storefront_country` | string, optional | the store's storefront country (price tier) |

Exactly one of `plan` and `item_id`. Delivery is `account`: the plan or the data lands on the caller's network.
Refusals, in the order they are checked:

| `error.code` | When |
|---|---|
| `invalid_request` | unknown `asset`; neither or both of `plan` and `item_id` |
| `asset_unavailable` | the coin is off, or not configured |
| `plan_unavailable` | the coin does not sell the plan (`monthly` included), or no price is configured |
| `item_unavailable` | the coin does not sell the item; no price is configured; the price is over the per-invoice cap |
| `guest_sign_in_required` | plans only: the network has no sign-in method |
| `offer_unavailable` | `yearly_onboarding` while the welcome offer is not redeemable |
| `rate_limited` | more than 10 intents a minute for this network (shared with `/solana/payment-intent`) |
| `asset_unavailable` | the coin cannot be quoted now: fewer than two fresh rates, rates more than 3% apart, the scanner or its source lagging (such a coin is in `unavailable_assets`, not `assets`), the address pool empty, too many open invoices |
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
  "asset": "btc", "plan": "yearly", "item_id": "",
  "delivery": "account", "network_name": "",
  "tier": "standard", "offer_applied": false,
  "price_usd": 40, "regular_price_usd": 40, "rate_usd": 73596.9,
  "address": "bc1q…", "amount": "0.00054622", "amount_atomic": "54622",
  "memo": "",
  "uri": "bitcoin:bc1q…?amount=0.00054622&label=URnetwork&message=URnetwork%20Pro%20yearly",
  "uri_note": "",
  "network": "bitcoin",
  "network_warning": "Send BTC on the Bitcoin network to this address, exactly the amount. BTC on another network (Lightning, Base, Ethereum, Solana) is not credited. A payment needs 1 confirmation.",
  "quoted_at": "2026-11-10T18:00:00Z", "expires_at": "2026-11-10T18:30:00Z",
  "confirmations_required": 1,
  "status": { /* CryptoPaymentStatusResult */ }
}
```

| Field | Type | |
|---|---|---|
| `intent_id` | string (uuid) | for TAO, Bitcoin, ETH and SOL also the name of the invoice's Coinbase deposit address |
| `status_token` | string | 32 random bytes, base64url without padding (43 characters) |
| `asset`, `plan`, `item_id` | string | `plan` is `""` for a data pack, `item_id` is `""` for a plan |
| `delivery` | string | `account`, `network` or `code` |
| `network_name` | string | the named network (delivery `network`), else `""` |
| `tier` | string | the price tier of a plan; `""` for a data pack |
| `offer_applied` | boolean | the welcome offer's price |
| `price_usd` | number | what the buyer pays |
| `regular_price_usd` | number | the item's regular price (the full yearly price under the offer) |
| `rate_usd` | number | the coin's USD rate the amount was quoted at; `1` for USDC |
| `address` | string | where to pay: a Monero subaddress, a Zcash unified address, the Solana merchant address (USDC), or the invoice's own Coinbase deposit address (TAO as SS58 with prefix 42, Bitcoin, ETH in EIP-55 case, SOL) |
| `amount`, `amount_atomic` | string | exactly what to send: price × (1 + the coin's buffer) ÷ rate, rounded up to the coin's quote decimals ($40 at $555.36 is `0.072386` XMR; mmm/ZCASH-CONTRACT.md §2.3's example `0.072387` is off by one). USDC: the price plus a unique sub-cent suffix |
| `memo` | string | USDC: the Solana Pay reference, the memo of a transfer sent by hand; SOL: its Solana Pay reference (optional: the address alone finds the payment); else `""` |
| `uri` | string | the payment URI to show as a QR and to open. Never built by a client. `""` for TAO |
| `uri_note` | string | when `uri` is `""`, why: TAO has no payment link standard (the invoice shows the address and the amount with Copy, and a QR of the address only); else `""` |
| `network` | string | the chain the payment must be sent on (as in the coin list) |
| `network_warning` | string | what to send and on which network, in English, for a person or an agent paying by hand: a coin sent on another network or a token instead of the coin is not credited |
| `quoted_at`, `expires_at` | string (time) | |
| `confirmations_required` | integer | |
| `status` | `CryptoPaymentStatusResult` | the invoice's current status; left out on a refusal |
| `error` | `CryptoPaymentError` | left out on success |

On a refusal every other field is empty (`""`, `0`, `false`, `null`) and `status` is left out.

URIs, exactly as the server builds them (the sdk's `CheckCryptoPaymentUri` passes each, and the address in the link
is byte for byte `address`):

- XMR: `monero:<address>?tx_amount=<amount>&tx_description=<description>`
- ZEC (ZIP-321): `zcash:<address>?amount=<amount>&message=<description>` (no memo)
- USDC (Solana Pay): `solana:<recipient>?amount=<amount>&spl-token=<mint>&reference=<reference>&label=URnetwork&message=<description>`
- SOL (Solana Pay, native): `solana:<address>?amount=<amount>&reference=<reference>&label=URnetwork&message=<description>`, never `spl-token`
- Bitcoin (BIP 21): `bitcoin:<address>?amount=<amount>&label=URnetwork&message=<description>`, nothing else
- ETH (EIP-681): `ethereum:<address>@1?value=<amount_atomic>` (wei, Ethereum mainnet), nothing else
- TAO: no URI

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
- `asset_unavailable` while the coin has its maximum of open code invoices (100; for TAO, Bitcoin, ETH and SOL, half
  their pool of 40 Coinbase addresses).

## POST /pay/crypto/status

No auth; the token is the credential. Limited per client address (60 a minute); over the limit the answer is HTTP
429 (keep the last state and poll again later). Poll every few seconds while waiting. Request
`CryptoPaymentStatusArgs`:

```json
{"intent_id": "0b6f2f7e-…", "status_token": "kq3…"}
```

Response `CryptoPaymentStatusResult`:

```json
{
  "intent_id": "0b6f2f7e-…", "asset": "btc", "plan": "yearly", "item_id": "",
  "delivery": "account", "network_name": "",
  "status": "confirming", "review_reason": "",
  "amount": "0.00054622", "amount_atomic": "54622",
  "received": "0.0006", "received_atomic": "60000",
  "remaining": "0", "remaining_atomic": "0",
  "excess": "0.00005378", "excess_atomic": "5378",
  "network": "bitcoin",
  "confirmations": 0, "confirmations_required": 1,
  "expires_at": "2026-11-10T18:30:00Z", "credited_at": null,
  "balance_code": ""
}
```

| Field | Type | |
|---|---|---|
| `status` | string | below |
| `review_reason` | string | with `review`: `underpaid`, `late_underpaid`, `extra`, `orphaned`, `voided`, `credit_failed`, `screening`, `transfer_shape` or `amount_unmatched`; else `""`. Show the review copy for any value |
| `amount(_atomic)`, `received(_atomic)`, `remaining(_atomic)` | string | `received` counts mempool and mined receipts (a top-up to the same address included), but not a mempool payment the wallet flags as a double spend (it counts once mined) |
| `excess(_atomic)` | string | what arrived beyond the amount, `"0"` when nothing did. An overpayment is credited as paid; the surplus is returned on request to support, in the same coin, with the owner's go |
| `network` | string | the chain the payment is sent on |
| `confirmations` | integer | the lowest among the receipts that make up the payment (0 in the mempool; for TAO, ETH and SOL, 1 once final) |
| `confirmations_required` | integer | |
| `expires_at` | string (time) | `null` for `unknown` |
| `credited_at` | string (time) or `null` | |
| `balance_code` | string | delivery `code`, once paid: the code to redeem; else `""` |
| `error` | `CryptoPaymentError` | `quote_expired` with `expired`; `underpaid` with `review`/`underpaid` and `review`/`late_underpaid`; else left out |

`status`:

| status | when |
|---|---|
| `unknown` | no such invoice, or the token does not match. Nothing else is filled in |
| `paid` | credited. A credited invoice that later received a second payment stays `paid` (the second payment is held, `extra`, for an operator) |
| `review` | needs an operator: `underpaid` (short at expiry), `late_underpaid` (arrived after expiry, worth less than the price at its first-seen rate), `orphaned` (a credited payment left the chain: its entitlement was ended and its credit taken back, `credited_at` is `null` again, until an operator grants it again), `voided` (a payment to an invoice an operator closed), `credit_failed` (the payment is there but its credit kept failing; an operator credits it by hand), `screening` (the payment is held by the sanctions screen; it is credited once it clears), `transfer_shape` (a TAO transfer sent as a batch, a proxy or multisig call, an EVM precompile send or a stake transfer: an operator confirms it reached Coinbase, then credits it), `amount_unmatched` (reserved for a coin whose deposit address is shared, matched by amount; not produced while every invoice has its own address) |
| `confirming` | received at least 99.5% of the amount (or, after expiry, a late payment worth the price at its first-seen rate), not yet at `confirmations_required` |
| `seen` | something arrived, less than 99.5% of the amount; `remaining` says how much more to send to the same address |
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
- Every coin but USDC: a new rate, amount and expiry for the **same address**; the old quote is kept in the
  history. A coin that cannot be quoted now is `asset_unavailable`: the same refusal, from the same cached decision,
  as a new invoice's, and the coin is in `unavailable_assets` of `/pay/crypto/assets` meanwhile.
- USDC: the amount is already in dollars, so only `expires_at` moves.

## POST /pay/alchemy (internal)

Every Alchemy webhook delivers here: ETH's Address Activity webhook, and Bitcoin's if Alchemy offers one. A delivery
is verified with its own webhook's signing key (`X-Alchemy-Signature`, the hex HMAC-SHA256 of the raw body) before
anything is read; an unknown webhook id or a bad signature is refused, and with no webhook configured every delivery
is refused with 503. A verified delivery only brings the coin's next chain read forward: the invoice addresses it
names as recipients are read on the coin's next scan, whatever their age. Polling the chain is the source of truth,
so neither coin's availability depends on a webhook.

## Error codes added to `PurchaseErrorCode*`

- `asset_unavailable`: the coin is off or not configured, or cannot be quoted right now (rates, the scanner or its
  source lagging or not knowing the network's height, the address pool, too many open invoices), or does not
  deliver balance codes yet. UI: "Temporarily unavailable".
- `quote_expired`: the quote ran out with nothing received. UI offers "Refresh quote".
- `underpaid`: the invoice ended short. UI shows the review copy.

`CryptoPaymentError` is `{"code": string, "message": string}`.

## Unchanged

`/solana/payment-intent`, `/solana/payment-transaction`, `/pay/data/solana-intent` and `/pay/data/solana-status`
keep their shapes and behavior. `/solana/payment-intent` now has the per-network limit (10 a minute, `rate_limited`),
shared with `/pay/crypto/intent`. `/pay/data/solana-status` answers `unknown` for a reference of a USDC invoice made
through `/pay/crypto/intent` or `/pay/data/crypto-intent`: such a reference is on the chain, and must not name the
network it paid for. Those invoices are read through `/pay/crypto/status`.

## The rail behind the API (operations)

### Receiving: Coinbase deposit addresses (TAO, Bitcoin, ETH, SOL)

- The address refill, a taskworker task of every coin run each minute, keeps 40 free addresses per coin. Each is made with
  `POST /v2/accounts/<account_id>/addresses` (`{"name": <invoice id>, "network": <network>}`) with the CDP key of
  `vault/<env>/coinbase.yml` (`api.host`, `api.key_name`, `api.private_key`; the view and receive scopes), signed
  per request by the CDP SDK's JWT. The coin's wallet is looked up by its currency code (`GET /v2/accounts/TAO`,
  `BTC`, `ETH`, `SOL`): `api.account_id` is none of these wallets. The networks are `bittensor` (TAO: from Coinbase
  Exchange's public currency data; the first live address request confirms it), `bitcoin`, `ethereum` and `solana`.
- The invoice id the address is named with is reserved on its pool row, and the invoice that draws the address takes
  that id.
- Coinbase answering an address the pool already holds stops the refill with an error
  (`urnetwork_crypto_pay_coinbase_addresses_total{result="reused"}`): no address is ever given to a second invoice.
  If Coinbase turns out to give one static address for a coin, the coin's pool drains and it shows
  `address_pool_empty`; matching invoices on one shared address by unique amounts is designed (BTCPAY.md §17,
  `amount_unmatched`) but needs a schema change to the invoice's address uniqueness and is not built.
- On 2026-11-10 our Coinbase account had no TAO wallet (`GET /v2/accounts/TAO` answered 404): TAO is unavailable
  until one is opened (Receive TAO in the Coinbase app).
- Coinbase's own crediting of deposits is not on the path: trackers listed seven Coinbase TAO send and receive delays
  in 2026, one of 4.5 days on Prime, and a credit here comes from the chain, so such a delay does not keep a customer
  waiting.

### Detection, coin by coin

- **Bitcoin** (Alchemy only, `vault/<env>/alchemy.yml`): each invoice address's transaction history, polled every
  minute, from Alchemy's Blockbook-compatible UTXO API (`<btc_endpoint>/api/v2/address/<address>?details=txs`); only
  outputs to the address count, and each payment's inputs are read for the screen. Heights from Alchemy's JSON-RPC
  (`getblockchaininfo`). The UTXO API is an Alchemy add-on: the owner's key answered "UTXO requests are not allowed"
  (401) on 2026-11-10, and Bitcoin stays unavailable (`scanner_lagging`) until the add-on is on.
- **ETH** (Alchemy only): each invoice address's incoming transfers (`alchemy_getAssetTransfers`, `toAddress`, the
  external and internal categories), polled every 30 seconds, in exact wei; each paying transaction's receipt status
  (a reverted one moved nothing); credited once its block is at the `finalized` tag. The Address Activity webhook is
  a fast path, on when alchemy.yml names `eth_webhook_id`, `eth_webhook_signing_key` and `notify_auth_token`: the
  refill adds each new address to it through the Notify API. The webhook is made once by the owner at deploy time:
  `bringyourctl crypto alchemy-webhook [--signing-key-file=<path>]` finds the webhook for
  `https://api.bringyour.com/pay/alchemy` before it makes one, prints its id, and writes its signing key only to
  the named file.
- **SOL** (Helius RPC, `vault/<env>/helius.yml`): each invoice address's finalized signatures and their transactions
  (`getSignaturesForAddress`, `getTransaction` jsonParsed); system transfers to the address count, the sources are the
  senders. The Helius webhook's native branch only brings a read forward. The hourly Solana reconcile also reads the
  chain for every open USDC Solana Pay reference and credits a payment whose webhook delivery was lost.
- **TAO** (snow's Finney node, through `st.yml`): finalized blocks only, at most 100 a scan and never more than 300
  behind the finalized head; each block decoded with its own runtime's metadata (`System.LastRuntimeUpgrade`), never
  replaying history across runtime upgrades. Only top-level `Balances.transfer_keep_alive`, `transfer_allow_death`
  and `transfer_all` calls to the invoice address are credited by themselves; every other shape (batches, proxy,
  multisig, the EVM precompile, stake transfers) is held as `transfer_shape`. An undecodable extrinsic is skipped and
  counted; a runtime the reader cannot read pauses it (`urnetwork_crypto_pay_tao_reader_paused`) without
  crash-looping. **The TAO scanner's first run on mainnet must pass the SN25 validator's deploy gate**: snow's node is
  shared with the validator.
- **Watch windows** (TAO, Bitcoin, ETH, SOL): an open invoice's address is read every scan; one that expired, was
  credited or was voided in the last 24 hours once every ten scan intervals; an address a webhook names, at the next
  scan whatever its age.

### Rates

Kraken (`XMRUSD`, `ZECUSD`, `TAOUSD`, `XBTUSD`, `ETHUSD`, `SOLUSD`) and Coinbase's exchange rates for every coin
they list; at least two fresh answers within 3%, quoted at their median. CoinGecko is dropped for every coin but
Monero, which Coinbase does not list. CoinPaprika, the planned second source for Monero, was checked on 2026-11-10:
its keyless tier is "Personal use only. Commercial use starts with Starter" ($99 a month), and showing the data to
customers needs Enterprise (coinpaprika.com/api/pricing and its API FAQ; the API terms of February 2026 require
attribution). It is not used: Monero keeps Kraken and CoinGecko until the owner names its second source, since one
source alone never quotes. `config/main/crypto_pay.yml` still names `coingecko` for Zcash; the server drops it and
logs that it did.

### Sanctions screening

Before a TAO, Bitcoin, ETH or SOL credit, every sender the chain names is screened with `cryptosanctions.Match`
against the newest `config/all/crypto/<release>/` table (refreshed by `cryptoctl` on every release build): a match
holds the payment (`sanctions_hold`, shown to the buyer as `screening`) for the owner and counsel; a sender that
cannot be screened, or a missing table, holds it too (`screening_unavailable`), and the scanner screens it again
each scan, never crediting it unscreened. Every refund destination is screened, in every coin
(`bringyourctl crypto refund` refuses a match or an address the table cannot read). An operator credits a cleared
sanctions hold only with `bringyourctl crypto credit --sanctions-cleared`. USDC on Solana keeps its own credit path
and is not screened at credit.

### Refunds

A refund is sent by hand from Coinbase, each with the owner's go, to an address taken from the customer's own message
and screened. The destination is never the observed sender, which may be an EVM mirror, a multisig, a proxy or an
exchange (address poisoning is a known scam). TAO refunds take an SS58 address under any prefix, written with
prefix 42.

### Alerts and metrics

The monitor's payment failures signal (SIGNALS.md §2.22) pages on `crypto-sanctions-hold`,
`crypto-screening-unavailable` and `crypto-scanner-stalled` (a scanner that succeeded before and has not for 15
minutes, including a paused TAO reader) and warns on `crypto-payment-held` (`transfer_shape`, `amount_unmatched`).
The dashboard "urnetwork / crypto payments" charts every `urnetwork_crypto_pay_*` metric.

### Database

No migration: the reserved invoice id of a Coinbase address lives in `crypto_payment_address.intent_id` (migration
853), which a free row did not use. The migration head is unchanged by this rail work.
