# Bounded operator receipt collection

`strecovery collect-receipts` produces the observation and commitment inputs
for the existing offline receipt verifier. It reads an explicitly selected
owned HTTP(S) endpoint and publishes one private, create-only evidence file
only after every found receipt passes the existing proof verifier. It never
signs, broadcasts, restores custody, mutates operator databases or chooses a
new nonce. Every archived original, replacement and cancellation remains in
the census and its observation set.

```sh
go run ./cli/strecovery collect-receipts \
  --archive /private/recovery/census.json \
  --config /private/recovery/receipt-collection-config.json \
  --config-sha256 sha256:EXACT_CONFIG_FILE_DIGEST \
  --collection /private/recovery/collected-receipts.json \
  --timeout 10m

go run ./cli/strecovery verify-collection \
  --archive /private/recovery/census.json \
  --collection /private/recovery/collected-receipts.json \
  --collection-sha256 sha256:EXACT_COLLECTION_FILE_DIGEST
```

Both commands report the existing `ReceiptCommitmentReconciliation`. The second
command works entirely offline. The collection file retains the unchanged
`ReceiptObservations` and `ReceiptCommitments` wire objects together with a
content seal. Their existing separate-file `verify-receipts` interface remains
available. A file pin covers exact bytes including the trailing newline; the
content seal covers the collection object with `content_hash` set to `""`.

The private config uses `ReceiptCollectionConfig` in `receipt_collection.go`:

```json
{
  "schema": "urnetwork-operator-receipt-collection-config-v1",
  "census_hash": "sha256:EXACT_VALIDATED_ARCHIVE_SEAL",
  "source": "owned-observer",
  "rpc_url": "https://rpc.example",
  "native_chain": "EXPLICIT_NATIVE_CHAIN_NAME",
  "native_finalized": {"number": 711, "hash": "0xEXACT_NATIVE_HASH"},
  "evm_finalized": {"number": 91, "hash": "0xEXACT_EVM_HASH"},
  "mapping_evidence_hash": "sha256:EXTERNAL_MAPPING_EVIDENCE_DIGEST",
  "retry_window_seconds": 300
}
```

Placeholder identities above are not executable approval. The node, both
boundary identities, and their mapping must be supplied explicitly. This
increment does not import or authenticate the external mapping evidence. Its
hash remains a reference, never a trust token. Native and EVM heights are never
assumed equal. The result always says `admission: unapproved_observation`.

## Collection checks and failure behavior

1. Validate the original archive and pinned private config. Check EVM chain ID,
   native chain name, native genesis and the supplied native canonical block
   against that context. Check the exact supplied EVM boundary by its own number.
2. Look up every archived transaction hash. An explicit successful JSON `null`
   becomes `not-found`, which remains an unproven absence observation. Transport
   errors, malformed replies or missing capabilities refuse the whole collection;
   they never become empty/missing receipts. Require explicit type, status,
   transaction index, block identity, gas and effective-price fields for found
   receipts, including their valid zero values. Future or too-old receipts fail.
3. Read every selected account nonce at the EVM boundary with the hash selector
   `{"blockHash":"...","requireCanonical":true}`. The nonce remains an RPC
   assertion and cannot authorize new signing.
4. Follow exact raw RLP header parent hashes from the EVM boundary backwards
   through the earliest found receipt, then retain them in consecutive ascending
   order. Require the reviewed Frontier RLP15 profile; rendered timestamps and
   base fees cannot reconstruct or replace missing raw headers.
5. Read complete raw blocks and raw receipt vectors at each inclusion hash.
   Require the three-field Frontier block envelope with no ommers, at most 2048
   transactions, equal transaction/receipt counts, supported legacy/access-list/
   dynamic-fee transaction types, correct receipt types and cumulative gas, and
   exact transaction and receipt trie roots. There is no paging/truncation mode.
   Build proofs for each selected transaction, receipt and required predecessor.
6. Repeat network/native-boundary and EVM-boundary checks after all body and
   account reads. Replay the existing offline verifier, including exact archived
   signed bytes at the same receipt index, receipt outcomes and gas differences.
   Only then publish the single complete evidence file and report.

The raw methods `debug_getRawHeader`, `debug_getRawBlock` and
`debug_getRawReceipts` are present in the reviewed codec source
[`67dcf7f791dc495064c293f080a0702cb433e51e`](https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/client/rpc/src/debug.rs).
This is a capability/wire-profile reference. It does not attest an observed
runtime, node build or source-to-Wasm mapping. Required methods absent on the
selected production node fail explicitly; no public fallback endpoint is used.

Each transient transport/timeout or HTTP 408/429/502/503/504 read retries the
same selector once per second within its configured 60–900 second window
(default 300). Semantic RPC errors, malformed or contradictory data, and
unsupported methods are terminal. Caller cancellation ends reads immediately;
the complete invocation has a hard 15-minute maximum. Command deadlines must
be 60 seconds to 15 minutes (default 10 minutes). A shorter remaining overall
deadline or exhausted shared resource budget takes precedence over retries.

The client owns its connection state, disables redirects, environment proxies
and compression, and rejects URL credentials, queries and fragments. Each reply
is bounded to 16 MiB, all replies (including failed attempts) to 128 MiB, all
requests/retries to 32768, and ancestry to 4096 headers. Existing header/node
limits and the shared 32 MiB encoded commitment budget apply during collection.
One block's trie state is discarded after exporting its paths. The complete
published file is bounded to 64 MiB. Budgets do not reset per block, proof or
retry. Requests, results and input JSON reject ambiguous duplicate/case-folded
keys; no RPC payload or endpoint credentials are emitted in error text.

Private-file, directory-lock, atomic create-only and fsync rules are the same as
the existing census publication path. New files are sealed owner-read-only
(`0400`) before fsync and publication; the temporary inode starts writable
(`0600`). Identical files preserve the existing private inode; different
evidence cannot overwrite an existing path. Collection failures publish no
artifact or successful report. An interruption after completed durable file
publication can leave that fully verified file without a printed report;
offline replay or an identical retry can recover it.

## Authority still absent

The native canonical lookup does not authenticate a native header, a GRANDPA
proof or the assertion that this boundary is finalized. The native/EVM mapping
is still externally supplied and unverified. Final canonical rechecks cannot
turn a coherent malicious branch or proxy into independent consensus authority.
Account nonces and null receipt results remain unproven RPC assertions.

All finality, canonical-accounting, actual-fee and spending-authority flags
remain false. Actual receipt fees remain null. Gas and outcomes are committed;
rendered base fee/effective price, native conversion and runtime debits are not.
No input flag can promote those claims. Runtime admission, independent finality/
mapping, exact fee evidence, service adoption, and owned-node custody/restart
qualification remain open MG03/PF03 work. Live selected-node capability is
unapproved; this candidate's deterministic qualification uses synthetic nodes.
