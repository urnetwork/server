# Offline receipt observations and conditional gas accounting

`strecovery reconcile` joins a validated custody archive with one pinned,
bounded observation file. It considers every unique selected signature,
including terminal originals, fee replacements, cancellations and signatures
found only in stores. It preserves each signature's archived provenance and
reports nonce outcomes separately from individual receipt observations.

This is an adapter for a missing trusted production observation interface. It
does **not** authenticate canonical finality. A reported `observed-finalized`
state means only that the supplied receipt matches the source's supplied
canonical block and lies at or below its supplied EVM boundary. Those source
claims still require independent authentication.

The additive [`verify-receipts` command](RECEIPT-COMMITMENTS.md) now verifies
transaction/receipt trie membership and raw-header ancestry for retained
observations. It still leaves independent native finality, mapping and actual
runtime fee debits unresolved; it does not change this command's authority.

The output always sets these four fields to false:

- `finality_authenticated`
- `canonical_receipts_reconciled`
- `actual_fees_reconciled`
- `spending_authorized`

There is no flag or input boolean to change that. A source label, content pin,
database status, receipt height, `finalized` RPC label or matching native/EVM
height does not supply the missing authority. The archive inspection command's
existing authority fields also remain false.

## Command and input custody

```sh
go run ./cli/strecovery reconcile \
  --archive /private/recovery/census.json \
  --observations /private/recovery/receipt-observations.json \
  --observations-sha256 sha256:EXACT_OBSERVATION_FILE_BYTE_DIGEST \
  --timeout 5m
```

Both inputs use the existing owner-private physical file checks. The observation
pin covers exact bytes, including newlines. The command rebuilds and validates
the archive, reads no database, makes no RPC call and writes no custody or
database state. JSON output contains receipt projections and conditional totals,
not raw signed transactions. Retain the original private inputs with the output
so another reviewer can replay the result. `observation_hash` in the report is
the digest of the decoded fixed JSON object; it differs from the exact file pin
when whitespace differs. It binds source array order as well as content.

Observation input is limited to 32 MiB. Its receipt count must equal the archive's
unique selected signature count, its account count must equal the selected role
count, and its block count is at most the signature count plus one. The archive's
existing count and byte bounds still apply. Missing/extra/repeated hashes,
unselected identities, context mismatches, ambiguous availability, invalid block
metadata and oversized fields refuse a report. Bounded malformed individual
receipts remain explicit unresolved observations. No incomplete file is accepted
as a successful truncated input. The command deadline remains at most 30 minutes.

## Producer wire contract

The fixed schema is declared by `ReceiptObservations` in
`receipt_observations.go`; it is a projection contract, not a decoder for raw
Ethereum RPC responses. JSON rejects unknown fields, duplicate/case-folded keys
and trailing values. The required members are:

| Member | Meaning |
| --- | --- |
| `schema` | `urnetwork-operator-receipt-observations-v1` |
| `census_hash` | Exact validated archive seal |
| `chain_id`, `genesis_hash` | Exact selected EVM chain id and native genesis |
| `source` | Bounded source label, not an authenticated source identity |
| `native_finalized` | Native `{number, hash}` boundary, in the native hash domain |
| `evm_finalized` | Separately identified EVM `{number, hash}` boundary |
| `mapping_evidence_hash` | `sha256:` reference to retained external mapping evidence; the adapter does not load or authenticate it |
| `blocks` | Explicit source-claimed canonical EVM block projections |
| `accounts` | One available/unavailable boundary nonce observation per selected role |
| `receipts` | One found/not-found/unavailable observation per archived transaction hash |

Every `blocks` item has `number`, `hash`, `gas_used`, `gas_limit` and nullable
`base_fee_per_gas`. Include the exact EVM boundary block even when no receipt is
found there. Block numbers and hashes must each be unique. An absent inclusion
block keeps its receipt unresolved; a differing inclusion hash is retained as
an orphan observation. Hashes use one nonzero lower-case `0x` encoding. Hash
identities come from the explicit source response; this adapter never substitutes
the hash of a reconstructed Ethereum header for the source's EVM hash domain.

Every `accounts` item has `role`, `address`, `block_hash`, `outcome` and nullable
`nonce`. The address must match the selected role and the hash must equal the
EVM boundary. `outcome` is `available` with a nonce, including explicit zero, or
`unavailable` with null. Latest/pending account nonces are not this interface.

Every `receipts` item has `hash`, `outcome` and nullable `receipt`. `outcome` is
`found` with a receipt, `not-found` with null, or `unavailable` with null. RPC
failures, pruned history and interrupted reads must become `unavailable`, never
`not-found`. A receipt projection contains:

```json
{
  "transaction_hash": "0x1111111111111111111111111111111111111111111111111111111111111111",
  "type": 0,
  "status": 1,
  "block_number": 90,
  "block_hash": "0x2222222222222222222222222222222222222222222222222222222222222222",
  "transaction_index": 0,
  "gas_used": 25000,
  "cumulative_gas_used": 25000,
  "effective_gas_price": "100"
}
```

The example identities are synthetic placeholders. Transaction/block numbers,
indices, gas and status are unsigned integers; prices are canonical unsigned
decimal strings, at most 256 bits. A missing receipt type, status or transaction
index is null and cannot silently become legacy type, failed status or index
zero. The producer must preserve the receipt type,
exact requested transaction identity and all quantity fields instead of filling
missing data from estimates or defaults. Only the archive's legacy and dynamic
fee transaction formats are supported.

## Resolution and fee semantics

The adapter compares each found receipt to the exact archived signed
transaction: requested hash, type, status, gas envelope and inclusion identity
must agree. Gas used must be positive, within the signed gas limit and consistent
with cumulative and block gas. Canonical inclusion slots must not be shared by
different signatures, and known cumulative gas intervals must not overlap.

Legacy effective gas price must equal the signed gas price. For dynamic fees,
the inclusion block's base fee is required and the effective price must equal
`min(fee_cap, base_fee + tip_cap)`; a base fee above the signed fee cap refuses
that candidate. The conditional gas fee is `gas_used * effective_gas_price` in
the EVM unit, using exact arbitrary-precision integer arithmetic. Failed
executions and cancellations pay gas too. Signed maximum gas/price envelopes
are not actual fee substitutes. These totals do not account for native
extrinsics, value transfers, storage/collateral movements or other charges.

A nonce resolves only when exactly one candidate has an observed finalized
receipt, its boundary account nonce has advanced, and every archived sibling
has a valid observation. Not-found or orphan siblings can then receive zero
conditional fees because that winner consumed the same nonce. An unavailable,
malformed or incomplete sibling withholds accounting for the entire nonce,
even when another receipt appears valid. Multiple canonical winners, including
an above-boundary inclusion alongside an apparent finalized winner, remain
conflicts. Read failure cannot be hidden by an earlier valid receipt.

An advanced account nonce with no observed finalized archived winner becomes
`unknown-nonce-consumer`. Absence/orphan observations with no advanced nonce
become `no-finalized-winner`; they do not prove that sending is safe. Above-boundary
inclusion, unavailable accounts and inconsistent account nonces remain explicit
unresolved outcomes. No result reserves a nonce, mutates an intent, creates a
replacement, broadcasts a cancellation or starts a service.

Each signature has its original observation state and its separate nonce state.
Unresolved fees are null. Role totals count only resolved nonce winners once,
regardless of duplicate database/store provenance. A zero subtotal with unresolved
nonces is not a claim of zero expenditure. `observation_accounting_complete`
describes only the complete conditional join of selected signed nonces; unsigned
intents, excluded records and opaque native files remain separately visible.

## Missing trusted production interface

Production acceptance still needs an independently approved, bounded observer
and finality verifier that supply and authenticate all of the following:

1. Network/genesis identity, source ownership, archive capability, qualified
   runtime/metadata and a native consensus-finalized boundary.
2. The exact runtime-qualified native-to-EVM mapping for that boundary, with
   independently authenticated evidence. Native and EVM hashes and heights must
   remain separate. A node's self-declared `finalized` tag is insufficient.
3. Canonical EVM inclusion identities and receipt contents on that mapped branch,
   including transaction/receipt identity, fees and appropriate source or
   inclusion-proof authentication. Sparse block-number lookups in this adapter
   are source assertions; they do not prove ancestry to the mapped boundary.
4. Account nonces read at the exact mapped EVM boundary and complete observations
   for every archived hash, preserving unavailable/pruned/conflicting reads.
5. Durable raw evidence, source and mapping approvals, exact hashes, bounded
   retry/cancellation behavior, and independent reconciliation of disagreement.

That observer/verifier is intentionally absent from this candidate. Its API
must verify independently established authority before any future integration
can change a canonical-accounting field. A file pin or an operator-supplied
approval boolean cannot replace it. Cross-host single-writer custody, deployed
service recovery, native receipt accounting and exact release qualification
also remain open MG03/PF03 work.
