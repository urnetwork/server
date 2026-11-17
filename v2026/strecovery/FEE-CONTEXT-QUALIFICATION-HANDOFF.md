# Independent fee-context qualification

The frozen candidate SHA, tree and physical module fences are recorded in
`/mnt/data/sn-testnet/qualification/mg03-native-fee-evidence-20260929/HANDOFF.md`.
Use a fresh isolated checkout of that exact SHA. Keep all shared roots and the
independent native-storage-proof candidate untouched. This candidate descends
from server `5ff7bf0264b775920049910896c23ec58862429f`.

The implementation adds only the offline historical receipt/native context
join and CLI described in [NATIVE-FEE-CONTEXTS.md](NATIVE-FEE-CONTEXTS.md).
Actual fees are not implemented. Parent/child roots and source-profile account
candidates cannot set runtime/payer/fee authority. No live RPC is required.

## Sol medium scope

Run all ten new roots normal and race:

- `./strecovery`: `^TestReceiptFeeContexts.*$` (eight roots).
- `./cli/strecovery`:
  `^TestRecoveryCommand(VerifiesFeeContextsOfflineWithoutFeeAuthority|RequiresPinnedFeeContextInputsWithoutApprovalFlags)$`
  (two roots).

Run the complete affected package regressions normal and race: 122 roots,
including the three existing `TestCensusDatabase*` roots. Create an attested
disposable database/service fixture for those roots; never use a shared/live
database. The prior fixture reference is
`/mnt/data/sn-testnet/qualification/mg03-receipt-collector-20260929/fixture-r2/with-private-services.sh`.
Retain per-root/per-mode results and all raw failures. Report any scope reduction
explicitly; a timeout, build failure or missing service is not a passing test.

The fixture independently encodes native SCALE and Ed25519 certificates and
builds indexed receipt/transaction tries. Native heights 700 onward differ from
EVM heights 90–100; parent, child and EVM roots are distinct. The history retains
six original/replacement/cancellation signatures, four nonce groups, a foreign
EVM predecessor and a reverted cancellation. Missing or ambiguous mappings and
checkpoint-parent gaps must return unresolved reports, not inferred roots.
V2 descendant certificates must preserve the original collection boundary.

## Causal controls

`controls/manifest.tsv` maps six independent patches to their exact roots.
Apply each patch separately to an exact-SHA disposable checkout, verify its
resulting diff, compile it and run the named root. Each must fail its semantic
assertion. Compiler, fixture or resource failure is not causal evidence.

| Mutant | Required rejected behavior |
| --- | --- |
| `parent_root` | Replaces the linked parent root with the child post-state root. |
| `missing_mapping` | Assigns a receipt context from an unrelated Frontier hash. |
| `ambiguous_mapping` | Treats multiple native commitments as a complete context. |
| `checkpoint_parent` | Treats the checkpoint's unseen parent as complete coverage. |
| `descendant_mapping` | Uses a header after the original boundary as historical context. |
| `mapped_account` | Changes the hashed-account namespace for the signed sender. |

The author performs compile-only builds, vet, formatting and source fences;
no behavioral test bodies or controls are executed during implementation.
Sol supplies independent normal/race/control receipts. On failure preserve the
frozen source and raw failure, and return a precise defect for a separately
frozen successor; do not silently patch the qualification source.

## Frozen resolver

Use `GOWORK=off GOPROXY=off GOSUMDB=off`, `-mod=readonly` and external
`compile/resolve.mod`. All physical replacements are pinned in
`source/local-source-graph.tsv`; only the frozen SN `protocol` package is
consumed from those replacements by the affected package closure. The external
resolver does not change candidate `go.mod`/`go.sum`. Repeat source and module
fences before and after every qualification run.
