# Bounded historical native storage capture

`CaptureReceiptHistoricalNativeState` and `strecovery capture-historical-state`
produce the witness consumed by `VerifyReceiptHistoricalNativeState`. They
replay the original custody, receipt and native finality inputs before selecting
one receipt block's linked parent execution state or child post-state. The
caller supplies no native root or state block. Missing parents and ambiguous
native mappings refuse capture before journal ownership or network access.

The result remains `unapproved_historical_storage_capture`. The nested verifier
reports authenticated raw storage relative to the supplied proof inputs; every
independent checkpoint/genesis/runtime, payer, fee, custody and spending authority
flag stays false. All original/replacement/cancellation histories survive, with
actual withdrawal, refund and gas-debit fields still null. This produces a
missing MG-03/PF-03 proof input; it does not attribute fees or close those gates.

## Selection and RPC

The private, byte-pinned config uses schema
`urnetwork-historical-native-capture-config-v1` and includes:

- `collection_hash`, `checkpoint_hash`, `finality_hash`: the typed proof digests
  reported by `verify-fee-contexts`, distinct from command input file byte pins.
- `source`, `rpc_url`: a source label and one explicit credential-free HTTP(S)
  route. Redirects, environment proxies, query strings and fragments are refused.
- `evm_block`: exact committed receipt number/hash; `root_role` is
  `parent_execution` or `child_post_state`.
- `keys`: 1–32 canonical, lexicographically sorted, distinct raw storage keys,
  each at most 1024 decoded bytes. Keys are not interpreted as runtime fields.
- `retry_window_seconds`: 0 for the 300-second default, or 60–900 seconds.

Each incomplete invocation first compares `chain_getBlockHash(0)` to the supplied
checkpoint's genesis. It then requests `state_getReadProof(keys, exactNativeHash)`
and `state_getStorage(key, exactNativeHash)` for each selected key. Proof `at`
must equal that hash, and the existing trie verifier must establish every
reported value or absence at the internally derived root. JSON null is absence;
`"0x"` is a present empty value. Neither RPC null nor a missing proof node alone
establishes absence. Best/latest labels, height-selected state, runtime calls,
enumeration and transaction submission are outside the capture's method profile.

The RPC shape was checked against pinned SDK `cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a`:
[state API](https://github.com/paritytech/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/client/rpc-api/src/state/mod.rs),
[ReadProof encoding](https://github.com/paritytech/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/client/rpc-api/src/state/helpers.rs),
and [full-state implementation](https://github.com/paritytech/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/client/rpc/src/state/state_full.rs).
Exact source hashes are in `historical-capture-reviewed-sources.tsv`. This codec
review does not admit a deployed runtime or endpoint's archive capability.

## Custody, restart and limits

Create an owner-private empty capture directory. The existing finality capture
lock, private create-only file publisher and fsync path retain `capture.json`,
request reservations, response byte debits, `storage-proof.json` and completed
`storage-NN.json` results. The directory manifest binds the full config and
original census. It cannot be reused for a different route, key set, root role,
proof or another capture kind. Complete result JSON is retained before semantic
interpretation; invalid retained results refuse restart instead of being replaced
by a later answer. Never delete partial files to reset an interrupted attempt.

Every request is durably reserved before transport. A missing completion after
interruption conservatively spends a full reply allowance. Request and byte
budgets persist across invocations: 32,768 requests, 16 MiB per wire response and
128 MiB lifetime response bytes. The existing 30-second HTTP attempt deadline,
finite per-read retry window and maximum 15-minute invocation deadline apply.
These are capture resource limits, not spending permission. Restart can consume
another finite invocation deadline but cannot replenish its lifetime counters.

Proofs are limited to 8,192 nodes, 16 MiB per decoded item, and 64 MiB total decoded
nodes/keys/values. The capture checks those limits while assembling results and
the verifier checks them again. The 16 MiB wire bound is stricter than the
verifier's item bound when JSON hex doubles the bytes. Large historical proofs
may require a separately designed bounded capture format; this path never
silently truncates or splits a selected key census.

Only complete successful replay can publish private mode-0400 `witness.json`.
A completed restart replays all original proofs and the exact selected witness
without creating an RPC client. A valid smaller key subset cannot replace the
selected census. Original archive, collection, checkpoint and finality files
remain unchanged. Local owner-private custody does not prove cross-host
exclusivity, distributed completeness, anti-rollback or retention against an
administrator who deletes the journal.

## Commands

```sh
strecovery capture-historical-state \
  --archive /private/archive.json \
  --collection /private/collection.json --collection-sha256 sha256:... \
  --checkpoint /private/checkpoint.json --checkpoint-sha256 sha256:... \
  --proof /private/finality.json --proof-sha256 sha256:... \
  --config /private/storage-config.json --config-sha256 sha256:... \
  --capture-dir /private/historical-capture --timeout 10m

strecovery verify-historical-state \
  --archive /private/archive.json \
  --collection /private/collection.json --collection-sha256 sha256:... \
  --checkpoint /private/checkpoint.json --checkpoint-sha256 sha256:... \
  --proof /private/finality.json --proof-sha256 sha256:... \
  --witness /private/historical-capture/witness.json --witness-sha256 sha256:...
```

Both commands require complete pinned inputs and a 60-second to 15-minute total
deadline. Verification is offline. Neither command opens the operator database,
constructs a signature, changes a transaction status or submits chain writes.
Runtime/source/metadata admission and actual historical debit/refund attribution,
account nonce interpretation, production collector capability, service adoption,
release composition and live custody/restart remain separate launch work.
