# MG03 bounded receipt collector source handoff

Base: server `fbe0c039949618ca9b9c66a7ebd7b160a37e25ce`.
Branch: `fix/mg03-receipt-collector-20260929`.
Worktree: `/home/by/urnetwork/temp/server-mg03-receipt-collector-20260929/server`.
Use the frozen commit delivered with this handoff; no shared root was edited.

Publication-fixture successor: implementation commit
`48a1878ee36214df627642b27306cfe715f49527` remains frozen. The follow-up branch
`fix/mg03-receipt-collector-publication-fixture-20260929`, at
`/home/by/urnetwork/temp/server-mg03-receipt-collector-publication-fixture-20260929/server`,
changes tests/docs only. Sol's first new-root normal run correctly exposed the
new test's wrong `0600` final-mode expectation: the unchanged shared publisher
has always sealed new evidence to `0400` before fsync/rename. Its raw failure is
retained at the qualification directory's `sol/new-strecovery-normal.json`.
Production bytes are unchanged. The corrected test requires a regular `0400`
file, unchanged inode on identical retry, and refusal of exposed permission,
symlink, hard-link and nonprivate-directory reuse through the collection API.
No root counts/selectors change; rerun the affected qualification on the exact
successor commit supplied with this handoff. No prior failed run becomes green.

The additive `collect-receipts` command and `CollectReceiptEvidence` API read
an explicit owned endpoint and explicit native/EVM boundary claims, authenticate
complete raw EVM bodies/receipt vectors, construct proofs, and invoke the
existing offline verifier before private create-only evidence publication.
`verify-collection` replays the combined file without network/database access.
The original `verify-receipts` and reconciliation behavior is unchanged.
See [RECEIPT-COLLECTION.md](RECEIPT-COLLECTION.md) for all bounds and authority
limitations, including configurable 60–900 second read retries (default 300).

The node, native finality/mapping and exact runtime capability are explicitly
unapproved. Actual fees remain null, all finality/actual-fee/spending flags false,
and original signed histories unchanged. No live endpoint, signer, submission,
service/controller/database mutation or mainnet approval was used or added.
The reviewed Frontier codec reference supplies API semantics, not deployed
source-to-Wasm evidence. MG03/PF03 remain open.

Astra performed formatting, diff checks, `go test -c` for both affected
packages and `go vet`. **No behavioral test body was run.** Compile evidence:
`/mnt/data/sn-testnet/qualification/mg03-receipt-collector-20260929/compile/`.
The external `resolve.mod` retains the physical dependency graph already used
for fbe0's commitment qualification. Tracked `go.mod`/`go.sum` are unchanged.

| Module | Frozen sibling commit |
| --- | --- |
| connect (including `connect/sctp`) | `b163f9dd9ac374942fe97331f26631248a9c1f81` |
| sdk | `516521fb16da46c9f4bff0b58221e1941694f616` |
| sn | `5198f6c96a363b86c7a9951ccefe395a2b54ef56` |
| glog | `892ade4a6be396b32ea82a550f243190b5992180` |
| goidenticons | `325750b38314313dc5f44c880ab6f12f6c1ecb3c` |
| proxy | `6204ae7df2a9868bbb3a7b61231917a36e4f5c9f` |
| userwireguard | `85fb1ca4086fa5dbfcda526bec7a17a894e691b9` |
| warp | `7498864c7cd3605aad3c43eabfab9008ed7f7228` |

Source/module fences are required before and after qualification. Use
`GOWORK=off`, read-only module mode and a separately instantiated physical
candidate graph. These sibling pins do not qualify current moving roots.

## Sol medium behavioral scope

1. New `./strecovery` roots: selector
   `^TestReceiptCollect(ion|orRpc).*$` (17 roots).
2. New `./cli/strecovery` roots: selector
   `^TestRecoveryCommand(CollectsAndReplaysPrivateReceiptEvidence|RequiresCompleteReceiptCollectionInputs)$`
   (2 roots).
3. All `./strecovery ./cli/strecovery` roots, 71 declared total (52 prior plus
   19 new), normal and race. Three `TestCensusDatabase*` roots require the
   existing attested disposable PostgreSQL harness. Do not skip them or use a
   shared database. Retain exact selectors and one result per declared root.
4. Vet, formatting, diff/source/module fences. There are no production
   controller/model changes and no new module versions. Their earlier receipts
   retain their original dependency scope.

The fixture constructs exact signed transactions and trie commitments
independently, then serves real HTTP envelopes through the production client.
It covers original/replacement/reverted-cancellation winners and unrelated
preceding transactions. Faults occur at explicit method/response barriers:
partial receipt vectors, contradictory raw receipts, forged JSON gas, omitted
zero-valued fields, unavailable raw methods, late network/boundary drift,
multiple gateway and transport timeouts, cancellation, shared request/response
budgets, ambiguous replies, RPC capability errors, and mutation-method refusal.
Publication coverage requires private create-only/idempotent evidence, preserved
prior bytes and no artifact/report on incomplete collection. The command's
offline replay runs after the synthetic server is closed. There are no sleeps
in the deterministic retry tests; private wait hooks carry cancellation.

## Isolated causal controls after positive qualification

- Remove only the final `VerifyReceiptCollection` call inside
  `collectReceiptEvidence` (not the write path's verifier).
  `TestReceiptCollectionRejectsForgedObservedGas` must fail because plausible
  JSON gas no longer has to agree with the receipt bytes before API return.
- Remove only the final `client.canonicalBlock(ctx, config.EvmFinalized)` call
  after the final network check.
  `TestReceiptCollectionRechecksBoundaryAfterBodyReads` must fail because the
  fourth canonical lookup no longer observes the forced late hash change.
- Remove only the final `client.network` call after body reads, keeping the
  initial network check. `TestReceiptCollectionRechecksNetworkAfterBodyReads`
  must fail because the forced second chain-ID observation is skipped.
- Remove only `self.remaining -= len(raw)` in the RPC response accounting.
  `TestReceiptCollectorRpcExhaustsSharedResponseBudget` must fail after a third
  request; the fixture deliberately returns terminal HTTP 400 on that request
  so the control cannot pass by timing out or hanging.

All controls must compile and reach the named assertion; compile/harness errors
do not count. Preserve failures and report source/fixture defects to Astra for
a new frozen commit rather than changing expectations. Production owned-node
capability, independent finality/mapping, exact runtime fees, service adoption
and live recovery still require their separate authority and qualification.

For the fixture successor, also change only `file.Chmod(0400)` to
`file.Chmod(0600)` in the shared publisher inside an isolated causal control.
`TestReceiptCollectionPublicationIsPrivateVerifiedAndCreateOnly` must fail at
the explicit final mode/inode assertion. This confirms the corrected expectation
is the sealed publication contract, not an arbitrary relaxation of privacy.
