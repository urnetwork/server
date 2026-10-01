# MG03 committed receipt source handoff

Base: server `05fee56f3051b8b96ff00f16bbf7f994ad57404d`.
Branch: `fix/mg03-receipt-commitments-20260929`.
Implementation worktree:
`/home/by/urnetwork/temp/server-mg03-receipt-commitments-20260929/server`.

This source increment adds the offline `verify-receipts` recovery command and
its public read-only verifier. See [RECEIPT-COMMITMENTS.md](RECEIPT-COMMITMENTS.md)
for the trust contract. Exact signed transaction membership, receipt
type/status/cumulative gas, predecessor-derived gas and consecutive raw-header
ancestry are authenticated relative to the supplied EVM boundary. Actual fees
remain null; independent finality, mapping, account nonces and spending
authority remain false. There is no live observer, controller/database change,
RPC, signer, deployment or MG03/PF03 closure claim.

Astra performed formatting, diff checks and compile-only `go test -c` for
`./strecovery` and `./cli/strecovery`; no test body was run. Initial read-only
compiles refused missing indirect declarations required by the already pinned
go-ethereum trie package. The five necessary existing-version indirect modules
were added to tracked `go.mod`; tracked `go.sum` is unchanged. Initial refusal
logs remain under the external compile evidence directory rather than being
reported as test results.

Compile evidence:
`/mnt/data/sn-testnet/qualification/mg03-receipt-commitments-20260929/compile/`.
The `resolve.mod` external modfile maps local dependencies to the same physical,
previously qualified graph as the MG03/R48 successor. It does not alter tracked
replacement paths. Sol should build a separate physical candidate graph at the
frozen commit supplied with this handoff and these exact sibling commits:

| Module | Commit |
| --- | --- |
| connect (including `connect/sctp`) | `b163f9dd9ac374942fe97331f26631248a9c1f81` |
| sdk | `516521fb16da46c9f4bff0b58221e1941694f616` |
| sn | `5198f6c96a363b86c7a9951ccefe395a2b54ef56` |
| glog | `892ade4a6be396b32ea82a550f243190b5992180` |
| goidenticons | `325750b38314313dc5f44c880ab6f12f6c1ecb3c` |
| proxy | `6204ae7df2a9868bbb3a7b61231917a36e4f5c9f` |
| userwireguard | `85fb1ca4086fa5dbfcda526bec7a17a894e691b9` |
| warp | `7498864c7cd3605aad3c43eabfab9008ed7f7228` |

The reference graph and its original fences are retained at
`/mnt/data/sn-testnet/qualification/mg03-r48-server-composition-20260929/`.
The current SN/SDK/Connect roots are not implicitly qualified by those pins.
Use `GOWORK=off`, read-only module mode and before/after module/source fences.

## Required behavioral scope

Sol medium owns all normal/race execution against the frozen candidate:

1. All 14 new roots:
   `^TestReceiptCommitments.*$` in `./strecovery` (12 roots), and
   `^TestRecoveryCommand(VerifiesPinnedReceiptCommitmentsWithoutAuthority|RequiresCompleteReceiptCommitmentInputs)$`
   in `./cli/strecovery` (2 roots). These need only synthetic private files and
   local signed test data, with no database, service, live RPC or signing port.
2. All pre-existing `./strecovery ./cli/strecovery` roots (38) normally and
   under race, for 52 total candidate roots. Three `TestCensusDatabase*` roots
   require the existing disposable private PostgreSQL fixture harness and its
   attestation; do not silently skip them or point them at a shared database.
3. `go vet ./strecovery ./cli/strecovery`, formatting, diff and source/module
   fences. No production controller/model behavior changed in this candidate;
   their earlier qualified receipts retain their original source scope.

The new root `TestReceiptCommitmentsRejectPlausibleForgedGasAndOutcome`
deterministically shows the old conditional join accepting internally coherent
forged gas/status, then requires the commitment verifier to reject each one.
The public command test repeats the forged-gas refusal after archive/file/pin
validation and requires zero partial output and no further database reads.
Adjacent roots cover real original/replacement/reverted-cancellation bytes,
unrelated preceding transactions, incomplete sibling census, unknown base fee,
header substitution, unsupported profile, incomplete/excess proofs, context
seals, cancellation and private input/byte bounds.

After positive roots pass, qualify these isolated causal controls:

- Remove only `gasUsed != observed.GasUsed` from the final receipt comparison.
  `TestReceiptCommitmentsRejectPlausibleForgedGasAndOutcome` must fail for its
  `gas` case: old conditional fee evidence wrongly acquires a verified label.
- Remove only `receipt.Status != *observed.Status` from that comparison.
  The same root must fail for `status`, while its gas case still passes.
- Replace `!bytes.Equal(rawTransaction, transaction.Raw)` with
  `!bytes.Equal(rawTransaction, rawTransaction)` (keeping valid imports).
  `TestReceiptCommitmentsRequireArchivedBytesAtReceiptPosition` must fail: a
  real different signed replacement at the same slot incorrectly authenticates
  the selected original's receipt.
- Disable only the `index > 0 && header.ParentHash != previousHash` refusal.
  `TestReceiptCommitmentsRejectDisconnectedHeaderAncestry` must fail although
  both observed endpoints and all supplied raw header hashes still match.

Preserve harness/compile failures and incomplete runs. A failing mutant must
reach the intended assertion; compiler errors do not count as causal controls.
Report source or fixture defects for a new Astra-frozen commit instead of
changing expectations. These gates qualify source evidence only.
