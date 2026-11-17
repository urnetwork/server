# MG03 receipt observation source handoff

Source base: server `71efeb1f30d254de2f5282d747194b33935545ec`.
Candidate branch: `codex/mg03-receipt-fees-20260928`.
Implementation worktree:
`/home/by/urnetwork/temp/server-mg03-receipt-fees-20260928`.

This additive slice is confined to `strecovery/` and `cli/strecovery/`.
It adds the offline `reconcile` command, a pinned observation-file adapter and
conditional per-signature/per-nonce gas accounting. It changes no controller,
model, migration, dependency or live service path. The archive schema and
collect/inspect/restore behavior remain unchanged.

Read [RECEIPT-OBSERVATIONS.md](RECEIPT-OBSERVATIONS.md) before interpreting the
output. The missing trusted production interface is independently authenticated
native-to-EVM finality mapping and canonical source/receipt observation. The
candidate deliberately keeps finality authentication, canonical reconciliation,
actual chain fee reconciliation and spending authority false. Its conditional
accounting flag cannot authorize recovery actions or close MG03/PF03.

Implementation formatted the source and checked the diff. It did not compile
or run a test body. Sol owns behavioral qualification against the exact frozen
commit supplied with this handoff; implementation/debug remains with Astra.
No live database, RPC, chain state, signer or custody file was accessed.

Use the isolated physical module graph from the preceding census qualification
or reproduce the sibling module pins in `QUALIFICATION-HANDOFF.md`. Replace
only server with this frozen candidate in a new isolated graph. Preserve
`GOWORK=off`, before/after source/module fences, exact go.mod/go.sum bytes and
the existing private PostgreSQL/Redis fixture attestation. Do not change shared
worktrees or quietly broaden the fixture profile.

Required gates:

1. All new `TestReceiptReconciliation*`, `TestReceiptObservations*` and
   `TestRecoveryCommandReconcilesPinnedObservationsWithoutAuthority` roots,
   normal and race. There are 15 new top-level roots: 14 in `strecovery` and one
   in `cli/strecovery`. They use only synthetic signatures and private temporary
   files; no database or chain RPC is needed.
2. All pre-existing `./strecovery ./cli/strecovery` roots, normal and race. The
   three `TestCensusDatabase*` roots still require isolated PostgreSQL fixtures.
3. Adjacent existing controller account/receipt recovery roots matching
   `^(TestStAccountRecovery.*|TestStReplacementWaitReadErrorCannotAuthorizeReplacement|TestStAccountReconcile.*|TestStReceiptCandidates.*|TestStReceiptObservation.*)$`,
   plus the six relevant model durable transaction-intent/attempt roots from the
   previous qualification, normal and race on isolated fixtures.
4. `go vet ./strecovery ./cli/strecovery`, formatting, diff and source/module
   fences. Shard bounded jobs if necessary without dropping roots or hiding an
   incomplete run behind an inflated timeout.

After the positive roots pass, build isolated causal controls:

- Remove the `case incomplete` refusal from nonce reconciliation.
  `TestReceiptReconciliationUnresolvedSiblingWithholdsWinnerAccounting` must
  fail because a finalized original/replacement/cancellation incorrectly hides
  an unavailable or malformed sibling and contributes a fee.
- Replace `receipt.GasUsed` with `tx.Gas()` in the fee multiplication.
  `TestReceiptReconciliationAccountsEveryAttemptKindOnce` must fail on the
  exact role total, exposing maximum gas being passed off as actual gas.
- Remove the comparison between receipt and supplied canonical block hash.
  `TestReceiptReconciliationPreservesOrphanMissingAndFutureInclusions` must
  fail because the orphan loses its explicit orphan state.
- Remove the loop that marks repeated inclusion slots as conflicts.
  `TestReceiptReconciliationConflictingInclusionSlotsCrossNonceGroups` must
  fail; its synthetic cumulative gas intervals remain disjoint so the adjacent
  gas-overlap guard cannot mask this control.

Retain every failed positive, harness failure and incomplete run. A compile or
environmental failure is not a successful causal control. Report source defects
back for a new frozen commit; do not change expectations to obtain a receipt.
Offline qualification remains source evidence, not production finality, custody
approval, service restart or spending authorization.
