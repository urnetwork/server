# Offline historical native storage facts

`VerifyReceiptHistoricalNativeState` verifies one bounded raw `StorageProof`
against a native root derived by freshly replaying `VerifyReceiptFeeContexts`.
Every call therefore rechecks original archived signatures, receipt commitments,
native header ancestry, GRANDPA certificates and exact Frontier mappings. A
caller-supplied root or previous reconciliation cannot replace those inputs.
Existing collection-boundary, fee-context and trie-verifier APIs are unchanged.

The [bounded capture commands](HISTORICAL-NATIVE-CAPTURE.md) now produce this
witness from exact-hash RPC storage reads with durable partial evidence and
lifetime request/byte budgets. `verify-historical-state` replays it offline.

## Exact receipt and root role

The witness schema is `urnetwork-receipt-historical-native-state-witness-v1` and
the codec remains `substrate-blake2-raw-storage-proof-v1`. It binds the exact
collection, checkpoint and finality-proof digests, EVM receipt block number/hash,
mapped native child number/hash, root role, selected native state block, expected
raw reads and proof nodes. There is no supplied root, runtime version, context
report, endpoint, signer, custody route or authority field.

Only an EVM block containing a proved archived receipt is selectable. A retained
receipt-free ancestor or final EVM boundary cannot stand in for it. The derived
native mapping must be unique; absent or ambiguous candidates fail even when a
supplied proof would work at one convenient root. Native and EVM heights remain
different identity domains.

`parent_execution` requires the linked native parent's retained identity and
state root. A checkpoint's parent hash alone is insufficient. `child_post_state`
uses the mapped child's own retained root. A unique checkpoint child can prove
its own raw state while its parent remains unknown; the nested fee context keeps
`native_parent_unavailable` and incomplete coverage. No fallback converts that
child root into a parent execution root. Selecting a parent does not interpret
`:code` or authenticate its executing runtime; selecting a child does not decode
events or native fees.

The selected state identity must match the role even when parent and child have
equal trie roots. Changing only the role or only that identity fails. Changing
both consistently can describe a valid different raw fact; root equality does
not make the blocks identical. A later certified descendant proves finality of
the original boundary but contributes no historical receipt mappings or selected
state root after that boundary.

## Bounds, loading and ownership

Each call handles one exact receipt block and one root role, 1–32 reads and at
most 8,192 nodes. It reuses the qualified no-extension Blake2-256 raw trie reader,
including LayoutV0/LayoutV1 inline and externally hashed values, 1,024-byte keys,
16-MiB items and a shared 64-MiB decoded budget. A nil expected value is absence;
`"0x"` is a present empty value. Missing proof nodes are not absence. Canonical
bytes, duplicate rejection and the existing bounded traversal still apply.

`LoadReceiptHistoricalNativeStateWitness` uses the existing private-file reader
with a 129-MiB limit, exact `sha256:<hex>` byte pin and strict JSON decoding.
Malformed digest syntax and a canonical-but-wrong byte pin are different failures.
Unknown caller-root or authority fields and duplicate keys are rejected.

Inputs are borrowed immutable for the call. The result owns its raw value
pointers and freshly derived context report. Cancellation and all validation
errors return no partial result. Work is bounded between context checks; an
individual bounded synchronous hash/JSON operation is not a hard real-time
deadline promise.

## Authority and qualification boundary

The result is an `unapproved_historical_storage_observation`. Only selected
historical context and raw header/storage verification facts become true.
Checkpoint/genesis admission, executing runtime source/decoding, payer binding,
native extrinsic placement, live finality, actual fee attribution, owner authority
window, global custody, fee exposure and spending remain false. All original
replacement/cancellation histories and null actual fee fields survive in the
nested fresh fee-context report. Proving selected raw reads does not mark every
required native read or every receipt context complete.

This source adds no CLI, proof collector, runtime decoder, signing mechanism,
live RPC or transaction. Runtime-aware interpretation requires independently
admitted source/compiler/metadata/storage semantics and its own qualified
evidence path. Actual debit/refund attribution remains separate from raw facts.

Twenty new deterministic roots use the independently sealed 18-vector SDK oracle
and genuine synthetic header/certificate encodings. They cover both root roles,
all original alternatives including a reverted cancellation, exact associations,
same-root identities, missing/ambiguous/checkpoint-parent contexts, descendant
certificates, corrupt resealed evidence, raw proofs, ownership, bounds,
cancellation and strict loading. The initial checkpoint-child refusal was removed
after independent static review; the child proof now retains useful facts without
inventing parent coverage. No behavioral qualification is implied by that review.
The author performs compilation/vet only. Independent normal/race roots and exact
causal controls must pass on the frozen candidate before integration.
