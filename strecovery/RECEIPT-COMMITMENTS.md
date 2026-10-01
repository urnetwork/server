# Committed operator receipt evidence

`strecovery verify-receipts` strengthens the existing offline receipt join by
checking exact transaction and receipt trie membership under raw EVM headers.
It refuses plausible RPC gas or outcome values that disagree with committed
receipt bytes. The previous `reconcile` command and its conditional semantics
remain available unchanged.

```sh
go run ./cli/strecovery verify-receipts \
  --archive /private/recovery/census.json \
  --observations /private/recovery/observations.json \
  --observations-sha256 sha256:EXACT_OBSERVATION_FILE_DIGEST \
  --commitments /private/recovery/commitments.json \
  --commitments-sha256 sha256:EXACT_COMMITMENT_FILE_DIGEST \
  --timeout 5m
```

All files must pass the existing private physical-file checks. Pins cover
exact file bytes. Unknown, duplicate and case-folded JSON keys are refused.
The command makes no RPC or database call, writes no custody, and has no signer
or broadcast port. Error or cancellation emits no partial JSON report.

## What is verified

The commitment file uses the fixed `ReceiptCommitments` wire structure in
`receipt_commitments.go`:

| Field | Required content |
| --- | --- |
| `schema` | `urnetwork-operator-receipt-commitments-v1` |
| `census_hash` | The validated archive's exact seal |
| `observation_hash` | SHA-256 of the decoded `ReceiptObservations` object re-encoded with Go `encoding/json`; this is the existing reconciliation report's `observation_hash`, not necessarily the whitespace-sensitive file pin |
| `headers` | Ascending consecutive raw RLP headers, encoded as lower-case `0x` hex, from the oldest supplied canonical block through the supplied EVM boundary |
| `receipts` | Exactly one inclusion proof for every `found` archived hash, with no proofs for `not-found` or `unavailable` claims |

Every inclusion proof contains `hash`, `transaction_nodes`, `receipt_nodes` and
`previous_receipt_nodes`. Each node is its raw RLP as lower-case `0x` hex. Node
database keys are derived locally with Keccak-256; supplied keys are never
trusted. Repeated nodes within one proof are refused. Proof keys are the
RLP-encoded unsigned transaction index from the matched observation.

The verifier checks every raw header's number, gas limits and parent link. The
last raw header must hash to the exact supplied EVM boundary. Every supplied
canonical block projection must match the corresponding header's hash and gas.
Sparse source-claimed block-number lookups cannot replace that ancestry chain.

For each found receipt, the transaction trie at its index must contain the
archive's exact signed bytes. The receipt trie at the same index supplies type,
status and cumulative gas. At index zero, gas used is that cumulative value.
At later indices, the preceding receipt must be proven under the same receipt
root, and gas used is their cumulative difference. This works when the
preceding transaction is unrelated to the selected operator census. The result
must be positive, within the signed gas limit and consistent with the committed
block gas. Supplied receipt type, status, gas and cumulative gas must agree.

The supported raw header profile is the reviewed Frontier fifteen-field RLP
format. Its stored timestamp is preserved exactly; a rendered Ethereum JSON
header with a converted timestamp or added base fee is refused. The source
reference is Subtensor
[`67dcf7f791dc495064c293f080a0702cb433e51e`](https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/frame/ethereum/src/lib.rs#L413-L478),
which commits ordered encoded receipts and links stored EVM parent hashes.
Its [receipt construction](https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/frame/ethereum/src/lib.rs#L640-L684)
accumulates effective gas. This is a wire-profile reference, not source-to-Wasm
attestation or proof that an observed network runs that source.

Files are limited to 32 MiB, headers to 4096, each raw header to 64 KiB,
each proof to 64 nodes and each raw node to 1 MiB. The direct API also applies
one 32 MiB budget to all encoded header/node strings combined. It cannot reset
that budget per proof. A longer history needs a separately reviewed segmented
evidence workflow; the command does not silently truncate it.

## Remaining authority and fee boundaries

Successful output sets only `evm_header_ancestry_verified` and
`found_receipt_commitments_verified`. These describe byte commitments relative
to the supplied boundary, including a possible empty set of found receipts.
An internally consistent, wholly fabricated branch can still pass: independent
native consensus finality and the runtime-qualified native-to-EVM mapping are
not present in these inputs. A file pin, mapping-evidence hash or RPC finalized
label cannot authenticate that missing authority.

`account_nonces_authenticated`, `finality_authenticated`,
`canonical_receipts_reconciled`, `actual_fees_reconciled` and
`spending_authorized` remain false. There is no flag or approval boolean to
change them. Account nonces, not-found/unavailable observations and branch
selection remain external claims. Every original/replacement/cancellation
signature and unresolved sibling survives in `conditional_observations`.

Every committed receipt reports `actual_gas_fee: null`. Consensus receipt bytes
do not encode effective gas price. Frontier RLP15 headers do not commit the
runtime-derived base fee rendered by the RPC. Even legacy signed-price gas
products do not prove the runtime's actual debit or native-unit conversion.
The existing conditional fee totals are retained only in
`conditional_observations` and are never promoted to actual spending evidence.

The command is a verifier for retained proofs. The separate
[bounded collector](RECEIPT-COLLECTION.md) now produces its two input objects
from explicit owned-RPC reads and publishes them together after offline replay.
Its source and mapping remain unapproved observations. Owned-node capability
qualification, a native finality/mapping verifier,
exact runtime fee evidence, production service adoption and cross-host custody
qualification remain open MG03/PF03 work. Found orphan/future receipts cannot be
proved under this selected boundary and refuse this command; the original
conditional command remains available to retain those unresolved observations.
