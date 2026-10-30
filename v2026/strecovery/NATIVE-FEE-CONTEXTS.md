# Pinned receipt/native contexts before actual fee attribution

`VerifyReceiptFeeContexts` and `strecovery verify-fee-contexts` derive historical
native commitment contexts from the existing immutable archive, receipt
collection, checkpoint and GRANDPA proof. They make the next storage/runtime
proof inputs explicit. They do not close MG-03/PF-03 actual-fee evidence: every
withdrawal, refund, gas debit and nested `actual_gas_fee` stays null, and every
authority, canonical-accounting, actual-fee and spending flag stays false.

The [actual-fee dependency decision](ACTUAL-FEE-DEPENDENCIES.md) remains the
runtime attribution contract. Native storage proofs can supply authenticated
raw reads once independently qualified; storage roots alone do not identify
the fee hook, executing runtime, native transaction phase or actual refund.
This slice is independent of any native-storage-proof implementation.

## Accepted inputs and derived facts

The public verifier first calls the complete existing `VerifyReceiptFinality`
path: archive signature/census checks, collection seal, exact RLP transaction
and receipt inclusion, EVM ancestry, SCALE native header ancestry, weighted
GRANDPA signatures/transitions and exact final-boundary Frontier commitment.
An invalid proof returns an error and no partial report. Relative mathematical
proof remains distinct from independent genesis/checkpoint/runtime admission.

For each verified EVM receipt block, it derives:

- The exact EVM identity and intermediate state root from the raw committed RLP
  header, retaining its own EVM height.
- Every native header in the checkpoint-through-original-collection interval
  whose supported Frontier post-log commits that exact EVM hash. Native and EVM
  heights are never used as an identity join. No supplied mapping/root is input.
- Each candidate's native post-state root and parent hash from its exact SCALE
  bytes. A parent identity/root is available only when that exact consecutive
  linked parent header is retained in the proof. The checkpoint's unseen parent
  stays null even though its hash is known.

The original collection boundary is preserved under both proof versions. V2
may certify a later descendant to establish that boundary's finality. Headers
after that boundary never supply historical fee contexts; the nested native
report retains `native_certified` separately from `native_finalized`.

Every archived signature, origin and observation state survives, including
original/replacement/cancellation alternatives and missing receipts. Committed
receipt status, gas, raw-byte hash and EVM transaction index remain unchanged.
`native_extrinsic_index` stays null: the reviewed Ethereum pallet assigns its
EVM index through `Pending`, while native extrinsics, inherent imports and
initialization-phase execution need separate exact evidence.

`profile_mapped_account` is only `blake2_256("evm:" || signed_sender_H160)` under
the named reviewed source profile. It is not an admitted payer. The exact
runtime executing this receipt has not been proved or approved; both
`runtime_source_authenticated` and `payer_binding_authenticated` remain false.
The report identifies the source commit and carries explicit missing evidence.

## Coverage states and bounds

| State | Meaning |
| --- | --- |
| `receipt_unavailable` | This exact archived signature has no proved receipt. |
| `native_mapping_unavailable` | No supported exact Frontier commitment to this receipt block appears in the retained historical interval. |
| `native_mapping_ambiguous` | Multiple native headers commit the same EVM block; all candidates survive and none is selected. |
| `native_parent_unavailable` | A unique candidate is the checkpoint, whose parent header/root is absent. |
| `native_context_complete` | A unique committed native candidate and its linked parent root are retained, relative to the unapproved checkpoint and source profile. |

The context-complete state is a structural prerequisite, not transaction-level
fee evidence or runtime approval. `found_receipt_contexts_complete` requires at
least one proved receipt and complete contexts for every proved receipt. It
does not claim that every archived alternative has a receipt, that custody is
complete, or that historical receipt/accounting authority is admitted.

The source profile supports the existing Frontier post-log variants 1 and 3.
A missing log simply supplies no mapping. A present malformed, duplicated or
unsupported `fron` digest within the historical interval fails the bounded
profile rather than being silently ignored. Pre-log imports and full-block
post-log variants need a separately reviewed decoder/profile.

Additional ceilings are 4096 archived signatures, 8192 total origins, 64 distinct
receipt blocks and 64 total matching native candidates. Existing receipt/native
count and byte ceilings remain in force, including the 16 MiB finality proof
budget and 4096 native-header limit. All loops are finite; archive/origin,
header and transaction loops honor cancellation. The CLI defaults to five
minutes and refuses a deadline above fifteen minutes. Oversized otherwise-valid
evidence remains unsupported rather than losing candidates or becoming zero.

```sh
strecovery verify-fee-contexts \
  --archive /private/operator/archive.json \
  --collection /private/operator/collection.json \
  --collection-sha256 sha256:<exact-file-digest> \
  --checkpoint /private/operator/checkpoint.json \
  --checkpoint-sha256 sha256:<exact-file-digest> \
  --proof /private/operator/finality-proof.json \
  --proof-sha256 sha256:<exact-file-digest>
```

All files use the existing bounded private-file loaders. Collection/checkpoint/
proof flags pin exact file bytes; report hashes identify the validated typed
objects. The command only reads local evidence and emits a complete JSON report
on stdout. It has no RPC, database, approval, signer, broadcast or write option.
It does not rewrite collection bytes, nonce history, checkpoint or finality
proof. Existing collection/finality schemas and commands are unchanged.

## Exact source dependencies and next deterministic fixtures

This slice uses the existing [18-file manifest](actual-fee-reviewed-sources.tsv)
at Subtensor `67dcf7f791dc495064c293f080a0702cb433e51e` and SDK
`cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a`. In that source, Ethereum finalization
commits a Frontier block hash; EVM state root construction is intermediate;
`HashedAddressMapping<BlakeTwo256>` hashes the `evm:` namespace; and the custom
fee handler has generic balance events without transaction-bound fee records.
These are source-profile facts, not source-to-live-runtime admission.

The next slice must consume independently verified raw storage reads against
the specific derived parent/child roots. It must prove the executing parent
`:code`, exact runtime version and admitted metadata/source correspondence;
child post-state code cannot silently stand in for parent execution at an
upgrade. Generic `System.Events` and account proofs remain raw facts until
runtime-specific decoding and attribution are independently established.

Before any fee implementation, deterministic fixtures must establish:

1. Exact native body/transaction placement, including a foreign native extrinsic
   before an EVM call, multiple EVM calls, and pre-log initialization imports.
   Copying the EVM index into an extrinsic phase must fail its causal control.
2. Parent/child Wasm and metadata swaps at a runtime upgrade, wrong hashed payer,
   missing read nodes versus authenticated absence, and changed base fee before
   execution versus after finalization. None may acquire payer/fee authority.
3. Same-phase native precompile withdrawals/deposits mixed with gas, an earlier
   internal deposit followed by failed fee refund, actual zero/partial/full
   refunds and a reverted EVM call. Selecting first/last balance events or
   subtracting block balances must fail against an independently executed oracle.
4. The reviewed fee handler's 18-to-9-decimal floor and u64 bound, zero-fee and
   precompile early-return paths, plus refund error semantics. Requested refund
   and gas-price multiplication cannot substitute for actual native balances.
5. Either bounded historical replay with complete authenticated parent state,
   body, hooks, host inputs and exact Wasm, or a future admitted dedicated fee
   record. Replay must match block commitments; unsupported host functions,
   incomplete witness and exhausted budgets produce no fee fact. A future
   event cannot reconstruct historical receipts that never contained it.

Synthetic fixtures may use unapproved checkpoints. Independent live admission
is an additional mainnet dependency, not a reason to defer offline tooling.
There is no sound general actual-debit implementation from the four current
inputs alone. MG-03/PF-03 remain open for authenticated native state capture,
runtime/debit attribution, account-nonce authority and operational adoption.
