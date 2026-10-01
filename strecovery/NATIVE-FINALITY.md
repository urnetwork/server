# Native finality proofs for retained receipts

`strecovery verify-finality` verifies GRANDPA certificates and the native
Frontier digest against an exact receipt collection entirely offline. It
authenticates signatures, scheduled authority handoffs, native header ancestry
and the EVM header commitment **relative to a separately pinned checkpoint**.
It does not approve that checkpoint, genesis, deployed runtime or recovery spend.
No independently approved mainnet checkpoint is shipped with this code.

[`capture-finality`](NATIVE-FINALITY-CAPTURE.md) captures bounded native proof
bytes from an operator-selected owned archive route into private resumable
custody. Capture requires the same separately pinned checkpoint and never
derives its authority from a node reply. Its v2 proofs can certify a descendant
when the collection's exact native boundary has no stored justification.

```text
strecovery verify-finality \
  --archive /private.example/archive.json \
  --collection /private.example/collection.json \
  --collection-sha256 sha256:<exact-file-digest> \
  --checkpoint /private.example/checkpoint.json \
  --checkpoint-sha256 sha256:<exact-file-digest> \
  --proof /private.example/proof.json \
  --proof-sha256 sha256:<exact-file-digest>
```

Every file must satisfy existing owner-private, physical-file custody rules.
Pins cover exact file bytes. JSON rejects duplicate and unknown fields,
including invented approval flags. The command reads no RPC or database and
writes only the complete result to stdout. Failure or cancellation emits no
partial result. The archive, collection and signed histories remain unchanged.

## Checkpoint contract

The `urnetwork-native-finality-checkpoint-v1` object has these exact fields:

| Field | Required meaning |
| --- | --- |
| `schema` | `urnetwork-native-finality-checkpoint-v1` |
| `codec_profile` | `subtensor-u32-blake2-grandpa-ed25519-scheduled-v1` |
| `genesis` | Native genesis hash, matching the selected archive |
| `header_scale` | Complete, lowercase `0x` SCALE native header |
| `set_id` | GRANDPA client's active consensus set ID after finalizing this checkpoint |
| `authorities` | Distinct Ed25519 `public_key` and nonzero u64 `weight` pairs |
| `live_state` | `live`, with no pending forced, pause/resume or disabled-voter state |
| `pending_change` | `null`, or the complete pending scheduled change |

A pending change has `scheduled_at`, `enactment_number` and `authorities`.
It must have been signaled at or before the checkpoint and enact strictly after
it. Any scheduled signal in the checkpoint's own digest must agree with this
state. A zero-delay signal in that header must agree with the post-enactment
active set. Signals before the checkpoint are part of its trust input; this
verifier does not prove their presence or absence from storage.

Runtime `Grandpa.CurrentSetId` storage is insufficient as checkpoint authority:
the reviewed Subtensor administrative path increments that storage when
scheduling a change. This verifier follows the consensus client's set-ID
transition after the outgoing set finalizes enactment.

`NativeFinalityCheckpoint.Hash()` is the SHA256 content digest of compact typed
JSON, including `pending_change:null`. The proof's `checkpoint_hash` uses that
digest, not the file pin. Neither digest approves the source keys or genesis.

## Proof and verification

The `urnetwork-operator-receipt-finality-proof-v1` object has `schema`,
`collection_hash`, `checkpoint_hash` and `segments`. `collection_hash` is the
collection's existing content seal. Each segment contains:

- `headers`: complete native SCALE headers, ascending and consecutive from
  the preceding checkpoint/certificate, excluding that predecessor;
- `justification_scale`: full encoded GRANDPA justification for the last
  header: u64 round, commit `(H256, u32, Vec<SignedPrecommit>)`, and ancestry
  `Vec<Header>`.

`urnetwork-operator-receipt-finality-proof-v2` has the same fields and certificate
rules, but permits the last certified header to be a descendant of the exact
collection boundary. The complete consecutive ancestry must contain that
boundary's number **and hash**. A later quorum cannot substitute a different
ancestor. The original collection, EVM boundary and retained history stay intact.
The checkpoint-to-certified-tip interval still advances fewer than 4096 blocks,
and all path/vote-ancestry headers share the existing count and byte bounds.

Each precommit contains `(H256, u32)`, a 64-byte signature and a 32-byte public
key. Ed25519 verification covers SCALE `(Message::Precommit, round, set_id)`:
index `1`, target hash, little-endian u32 height, u64 round and u64 set ID.
Go's canonical Ed25519 verifier is used; unusual encodings accepted by the SDK's
wider Zebra verifier are not admitted by this profile.

Distinct known authorities must have weight at least
`total - (total - 1)/3`. Duplicate/equivocating or unknown voters are refused.
Descendant votes require exact hash/number ancestry and the commit must be the
exact precommit GHOST. Unused/duplicated ancestry and authority signals in vote
ancestry are refused. Descendants cannot cross a pending enactment boundary.

Scheduled changes come only from complete `Consensus(FRNK, ...)` native
digests: index `1`, next-authority vector, u32 delay. Enactment is
`signal_height + delay`. The outgoing set must certify that exact block before
the new set and incremented set ID certify later blocks. Delays can span
intermediate certificates. Duplicate/overlapping schedules, overflow, forced
changes, disabled voters and pause/resume are terminal for this profile.
A fresh independently approved checkpoint is needed after an unsupported
transition; a runtime reply cannot grant that approval.

V1's last certified native header must equal the collection's native boundary;
its exact-boundary semantics and report shape are unchanged. V2 can certify a
later tip, but both versions authenticate the collection boundary's single
`Consensus(fron, PostLog)`, which must use reviewed variant `1` (block hash
and complete transaction-hash vector) or `3` (block hash). The vector is bounded
and completely decoded; transaction membership still comes from trie proofs.
The boundary digest's EVM hash must equal the exact final RLP15 header authenticated by
`VerifyReceiptCollection`. EVM height comes from that EVM header; native height
never selects an EVM block. Every archived signed transaction/receipt proof is
replayed, so a native certificate cannot bypass existing evidence checks.

The old collector still accepts supplied boundaries for capture. This new
verification path refuses an unrelated EVM boundary even when that collection
is self-consistent and a real quorum signed the unrelated native header. The
externally claimed `mapping_evidence_hash` remains history, not trust authority.

## Bounds and rolling checkpoints

One call admits at most 4096 native headers in total (checkpoint, finalized path
and vote ancestry); 64 certificates; 1024 authorities or votes per set or
certificate; and 16 MiB of shared encoded proof/header strings. A header is at
most 256 KiB, with at most 256 digest items of at most 64 KiB. A justification
is at most 4 MiB. Native numbers are u32; weights, rounds and set IDs are u64.
The boundary must advance fewer than 4096 native blocks from the checkpoint.
File bounds are 1 MiB for the checkpoint and 16 MiB for the proof. The CLI adds
a default five-minute deadline, configurable up to fifteen minutes. Verification
checks cancellation between headers, votes and ancestry steps and returns no
partial success.

A rolling-checkpoint policy must separately approve the initial genesis and
complete authority state, including pending signals, and retain the authority
and provenance of that approval. For an interval accepted under that external
policy, retain old checkpoint pin, complete proof, collection pin and result
together. A subsequent checkpoint can use the last retained raw native header
and the result's `next_set_id`, `next_authorities` and `pending_change`. Roll
forward before exceeding the count/byte interval. The custody record must link
back to the approved initial checkpoint. Independently approve a new anchor
whenever an unsupported transition or provenance gap breaks that chain.
Ordinary scheduled rotation is covered cryptographically; keys are not manually
replaced in the middle of a proof.

For v2, `native_finalized` and `native_state_root` still identify the original
collection boundary. The additional `native_certified` identifies the last
certificate target. The next authority fields belong to **that certified tip**,
including a rotation after the collection boundary. A rolling checkpoint uses
the last segment's last raw header and `native_certified`, never the older
collection header with later authority state. V1 omits `native_certified`.

This code has no approval flag or approval adapter. Its report always says
`admission: unapproved_checkpoint_proof` and leaves
`authority_checkpoint_authenticated`, `genesis_authenticated`,
`runtime_source_authenticated`, `finality_authenticated`,
`canonical_receipts_reconciled`, `actual_fees_reconciled` and
`spending_authorized` false. A downstream policy owner must implement and
qualify independent checkpoint/genesis/runtime admission before using these
proof facts for recovery authority. Verifying signatures from input keys cannot
approve the same input that supplied them.

## Why actual fees remain null

The reviewed Frontier RLP15 header does not commit the RPC-rendered base fee.
The runner suppresses requested priority fees, obtains base fee from native
runtime state and computes an effective gas charge. `SubtensorEvmFeeHandler`
converts from 18-decimal EVM units to 9-decimal native balance units, withdraws
a native imbalance and applies a best-effort refund. A failed or partial refund
can change the actual debit. The EVM receipt trie contains neither those native
debits/refunds nor runtime base-fee state. An ordinary native extrinsic's
`TransactionPayment.TransactionFeePaid` cannot replace this custom EVM path.

This increment supplies the native state-root commitment relative to the
checkpoint. It supplies no authenticated storage/events, execution metadata,
source-to-deployed-Wasm admission or transaction-scoped debit/refund evidence.
Every `actual_gas_fee` remains null. Receipt gas times a claimed price remains
conditional arithmetic. Account nonce proofs, deployed owned-node capture
capability, production service adoption, release composition and
live custody/restart qualification remain open MG-03/PF-03 dependencies.

The [actual-fee dependency decision](ACTUAL-FEE-DEPENDENCIES.md) examines generic
balance-event attribution, failed refunds, block balance deltas and the next
bounded native-state/execution-witness interfaces. It is source analysis, not
additional proof implementation or fee qualification.

## Exact reviewed sources

Codec and fee analysis uses Subtensor
`67dcf7f791dc495064c293f080a0702cb433e51e`, whose `Cargo.lock` pins
RaoFoundation/polkadot-sdk `cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a` and
`finality-grandpa` 0.16.3. These are source references, not node/runtime admission.

| Source | Semantics |
| --- | --- |
| Subtensor `runtime/src/lib.rs` | u32/BlakeTwo256 header; EVM fee calculator/converter/handler; administrative set-ID update |
| Subtensor `vendor/frontier/primitives/consensus/src/lib.rs` | Frontier post-log indices/fields |
| Subtensor `vendor/frontier/frame/evm/src/runner/stack.rs` | Withdrawal, effective gas, suppressed priority fee and correction |
| Subtensor `pallets/transaction-fee/src/lib.rs` | Native EVM withdrawal, conversion and best-effort refund |
| SDK `substrate/primitives/consensus/grandpa/src/lib.rs` | Ed25519 keys, justification, consensus logs, localized message codecs |
| SDK `substrate/frame/grandpa/src/lib.rs` | Scheduled signal and enactment height |
| SDK `substrate/client/consensus/grandpa/src/authorities.rs` | Old-set voting limit, ordered finalization, set-ID advancement; forced changes lack light-client handoff proofs |
| SDK `substrate/client/consensus/grandpa/src/justification.rs` | Commit-target, signature and complete ancestry checks |
| `finality-grandpa` 0.16.3 `src/lib.rs`, `src/voter_set.rs` | Precommit index/field order, exact GHOST, weighted threshold |

The qualification handoff records source byte fences, compile-only receipts,
module resolution, test selectors and causal controls. Behavioral qualification
belongs to the separately assigned Sol run; this document is not a test receipt.
