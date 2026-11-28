# Actual EVM fee proof: MG-03 / PF-03 dependency decision

The archived inputs accepted at server `44636e5e` cannot establish actual native
gas debits for general operator contract calls. Keep every `actual_gas_fee` null
and `actual_fees_reconciled` false. This is a source review and proposed interface,
not an implementation, behavioral qualification or deployed-runtime attestation.

An approved live checkpoint is **not** needed to develop or test another offline
proof verifier: it can establish facts relative to synthetic or unapproved
anchors, as [native finality](NATIVE-FINALITY.md) already does. Its absence must
not be confused with the separate missing debit/refund evidence. Neither
checkpoint approval nor a native state root alone supplies that evidence.

## What the exact runtime establishes

The reviewed Subtensor commit is
`67dcf7f791dc495064c293f080a0702cb433e51e`; its locked SDK commit is
`cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a`. The
[18-file byte manifest](actual-fee-reviewed-sources.tsv) records exact Git blobs,
including `Cargo.lock`. No working-tree successor or live RPC was substituted.

| Source | Consequence for proof |
| --- | --- |
| [Runtime converter/config, lines 1097–1192][runtime] | EVM units have 18 decimals; native units have 9. Conversion divides by `10^9`, floors, and refuses a result above u64. The payer is the runtime's hashed H160 mapping; `Balances` is the custom fee handler's currency. |
| [Runner, lines 232–275 and 367–417][runner] | Requested priority fees are suppressed. For an ordinary nonzero-fee transaction, precharge is runtime base fee times the adjusted gas limit; correction uses effective gas times that same price. Special zero-fee and precompile early-return branches exist. Signed fee caps and Ethereum RPC price arithmetic do not establish the debit. |
| [Custom fee handler, lines 518–575][handler] | Precharge uses exact native withdrawal with `Preserve`. Correction requests `paid - converted_corrected_fee`, saturating at zero; actual refund is a best-effort deposit. A deposit error becomes zero debt. The handler emits no fee-specific event containing the transaction hash, debit and actual refund. |
| [SDK balanced operations, lines 458–489][balanced], [balances hooks, lines 346–355][balance-hooks] | Successful withdrawal/deposit emits generic `Balances.Withdraw` / `Balances.Deposit` with the actual native amount. Best-effort partial or zero deposits must be distinguished from the requested refund. Errors bypass the success hook. |
| [Native balance effects, lines 185–210 and 265–299][tao], [SDK resolve, lines 499–511][balanced], [staking precompile, lines 196–214][staking] | Native call effects also use withdrawal and deposit/resolve; precompiles dispatch those calls during EVM execution. The event variants are not reserved for gas. |
| [Ethereum pallet, lines 610–742][ethereum] | `Ethereum.Executed` binds sender, destination, transaction hash and exit reason; receipt cumulative gas includes effective gas. This does not label preceding native balance events as fee operations. |
| [System phase/event record, lines 1182–1207 and 1846–1874][system] | Events contain an extrinsic phase and topics, not an internal call or fee-hook identity. Multiple inner calls share the phase. |
| [BaseFee storage/hooks/setter, lines 100–101, 130–201 and 209–235][base-fee] | The price is native state and can change at finalization or through a root call. A post-block price is not automatically the price used earlier in that block. |

For an admitted ordinary execution, let `W` be the actual native fee withdrawal
and `R` the actual native refund. The gas debit is `W - R`, in rao. If the refund
is complete, it reduces to the converted corrected fee; otherwise it need not.
Any EVM-denominated equivalent must explicitly multiply the native result by
`10^9`. A raw EVM product can contain fractional rao and must not silently be
called actual native expenditure. Transfer value, native call costs, burn, stake
and dust are separate economic effects.

The SDK's best-effort increase may return a successful partial or zero amount;
`write_balance` can also return an error through account mutation. Those paths
are visible in [increase/decrease balance, lines 178–234][balanced] and
[account mutation, lines 1035–1122][balances]. This review does not claim a live
refund error has occurred or that every theoretical failure is reachable under
an admitted runtime invariant. The verifier has no proof establishing such an
invariant, and may not replace actual refund evidence with that assumption.

## Why the tempting joins are insufficient

Summing all payer withdrawals minus deposits in an `ApplyExtrinsic` phase mixes
gas with staking, recycling and other precompile effects. Selecting the first
withdrawal and last deposit is also unqualified: a failed refund has no success
event, leaving an earlier call-effect deposit as a possible last deposit.
Event ordering plus a transaction hash is not a fee-hook identifier. A future
closed runtime/call profile could prove stronger attribution rules; none is
established by the current collection schema or this review.

Proving `System.Account(mapped_sender)` in the parent and child states gives a
block balance delta. It includes all transactions, initialization/finalization,
value transfers, reserves, dust and native effects in that interval. Even
transaction-boundary balances would require complete accounting of non-fee
effects inside a contract call. Subtracting only the EVM transaction's `value`
does not close that gap. Untrusted RPC traces add observations, not authenticated
execution. A native `TransactionPayment.TransactionFeePaid` event belongs to a
different charging path and cannot stand in for this custom EVM handler.

The current collector fetches no `state_getReadProof`, raw `System.Events`,
parent runtime Wasm or native execution witness. The finality verifier exposes
only its final native state root in the report. An earlier EVM receipt can lie
before the native checkpoint interval; its native inclusion root must be proved
separately. Never select a native inclusion block by copying an EVM height.
The EVM header's [intermediate runtime root][ethereum] is not automatically the
final native header's post-state root either.

## Next bounded offline interface (proposal, not accepted input)

Implement native storage read verification first, without changing fee fields.
A proposed `urnetwork-operator-native-state-witness-v1` object has exactly:

```text
schema: string
collection_hash: sha256 digest
checkpoint_hash: sha256 digest
finality_proof_hash: sha256 digest
blocks: [
  {
    native_hash: H256,
    trie_profile: "substrate-blake2-layout0-v1" | "substrate-blake2-layout1-v1",
    reads: [{ key: hex bytes, value: hex bytes | null }],
    storage_proof_nodes: [hex bytes]
  }
]
```

`VerifyReceiptNativeState(ctx, archive, collection, checkpoint, finalityProof,
witness)` must replay the existing proofs, derive every requested state root
from an included certified native header or its linked checkpoint, then check
every read against that root. Neither a JSON root nor a claimed verification
boolean is authoritative. Every receipt inclusion additionally needs its exact
native/EVM mapping; the final boundary's mapping cannot supply earlier roots.

The codec accepts raw `StorageProof` nodes, as returned by the SDK's
[`state_getReadProof` implementation][read-proof]. They form a content-addressed
database for `StorageProof::into_memory_db` / `read_trie_value`. They must not
be confused with the proof generated by `generate_trie_proof` for
`verify_trie_proof`, or with `StorageProof`'s separate `CompactProof` encoding;
see [the SDK proof APIs][trie]. Retain independent SDK vectors
for inline children, hashed values and authenticated absence before implementing
another language's decoder. Absence (`null`) differs from present empty bytes;
missing proof nodes are errors, never evidence of absence.

Initial proposed ceilings are 64 blocks, 32 reads per block, 8192 distinct proof
nodes, 16 MiB per node/value and 64 MiB total decoded witness bytes. Use the
existing five-minute default/fifteen-minute maximum deadline with cancellation
inside traversal. Bound key length, path depth and total decode work; reject
duplicates, unknown JSON fields, unsupported layouts and trailing SCALE. These
ceilings need qualification; oversized legitimate evidence remains unsupported
until a separately reviewed bounded profile exists.

Derived read keys include `System.Events` and `BaseFee.BaseFeePerGas`
(`twox128(pallet) || twox128(item)`), and `System.Account`
(`twox128("System") || twox128("Account") || blake2_128(account) || account`).
Derive `account = blake2_256("evm:" || H160)` from the signed transaction and
reviewed [address mapping][mapping]; do not accept a substituted payer. Key,
balance and event layouts still require execution-runtime qualification.

The next layer needs an execution-runtime reference with the exact native
parent hash, proved parent `:code` bytes/hash, full runtime version, metadata
bytes/hash and source/build manifest. Metadata must be generated from or
independently admitted for those exact execution bytes; pinning arbitrary
metadata is not admission. At upgrade blocks retain the parent execution and
child post-state runtimes separately. Full event decoding must consume all
events/topics with bounds and reject ambiguous pallet/type indices. Source and
metadata approval remain an independent policy input, never a witness flag.

For general contract calls, actual fee attribution then requires one of:

1. **Historical execution replay.** Retain the complete native block body,
   authenticated parent state witness (including child tries and every required
   read), exact executing Wasm and deterministic host inputs. Replay all hooks
   and extrinsics, verify body/post-state/digest commitments, and derive the
   actual fee-hook withdrawal/refund from a separately reviewed execution
   observation mechanism bound to those original Wasm bytes. A producer-supplied
   trace, altered instrumented Wasm without an equivalence justification, or
   an unproved intermediate root cannot replace this. Missing witness data,
   unsupported host functions or exhausted resources return no fee fact.
2. **A future admitted runtime event.** A dedicated, transaction-bound record
   must include payer, actual withdrawal rao, requested refund rao, actual refund
   rao and explicit correction outcome, including error/zero/partial cases.
   Prove its complete `System.Events` storage and exact phase/transaction join.
   This requires a separately reviewed runtime change; the pinned handler does
   not supply it and an upgrade cannot repair historical receipts.

Either layer may report proof facts relative to an unapproved checkpoint.
Actual mainnet accounting additionally requires independent genesis/checkpoint
and source-to-deployed-code admission. Missing, ambiguous or unsupported fee
evidence remains a typed unresolved result with null debit; no inferred zero.
Keep every original/replacement/cancellation candidate and count a proved nonce
consumer once. Such evidence alone never authorizes a new signature or spend.

A zero-value, empty-data self-cancellation might permit a smaller closed profile
after proving direct extrinsic placement, EOA/precompile restrictions and all
possible balance effects. It has not been implemented or qualified and would
leave ordinary operator contract calls unresolved. It is not MG-03/PF-03 closure.

## Deterministic qualification plan and gate impact

Future implementation should give Sol independent SDK-generated proof fixtures
and exact frozen source/module graphs. Required negative assertions include:

- Correct quorum and receipt, but substituted native inclusion root, omitted
  earlier mapping, changed storage node/value, or incorrect absent/empty claim.
- Same-phase precompile debit/deposit mixed with gas; earlier internal deposit
  followed by failed fee refund; valid partial/zero refunds; another payer or
  extrinsic's fee record. Each must refuse misattribution, not merely bad syntax.
- Parent/child runtime swap at an upgrade, arbitrary metadata substitution,
  wrong H160 mapping, changed decimal scale, u64 overflow, fractional rao,
  and base fee changed earlier in the block or at finalization.
- Revert/cancellation, duplicate nonce candidates, no-event/zero-fee early exit,
  initialization-phase imported transactions, incomplete body or replay witness,
  altered replay output, unsupported host operation, budget and cancellation.

Use separate compiling mutants to remove root equality, complete proof reads,
phase/payer/transaction binding and refund attribution guards. Each must fail
its named assertion; compilation failures are not causal evidence. Keep raw
failures and do not execute behavioral tests during implementation ownership.

MG-03/PF-03 remain **in progress** for source work and **not closed** for actual
fees. Their next dependency is authenticated native state capture/verification
plus runtime-qualified debit/refund attribution, separately from initial
checkpoint approval, account nonce proofs, historical approval correction,
service adoption, release composition and live custody/restart. No fee,
canonical-accounting, runtime, finality or spending-authority flag changes in
this documentation-only decision. No behavioral tests, live RPC, signing,
broadcast, shared database mutation or deployment were performed.

[runtime]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/runtime/src/lib.rs#L1097
[runner]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/frame/evm/src/runner/stack.rs#L232
[handler]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/pallets/transaction-fee/src/lib.rs#L518
[balanced]: https://github.com/RaoFoundation/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/frame/support/src/traits/tokens/fungible/regular.rs
[balance-hooks]: https://github.com/RaoFoundation/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/frame/balances/src/impl_fungible.rs#L346
[balances]: https://github.com/RaoFoundation/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/frame/balances/src/lib.rs#L1035
[tao]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/pallets/subtensor/src/coinbase/tao.rs#L185
[staking]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/precompiles/src/staking.rs#L196
[ethereum]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/frame/ethereum/src/lib.rs
[system]: https://github.com/RaoFoundation/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/frame/system/src/lib.rs
[base-fee]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/frame/base-fee/src/lib.rs
[read-proof]: https://github.com/RaoFoundation/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/client/rpc/src/state/state_full.rs#L359
[trie]: https://github.com/RaoFoundation/polkadot-sdk/blob/cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a/substrate/primitives/trie/src/lib.rs
[mapping]: https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/vendor/frontier/frame/evm/src/lib.rs#L910
