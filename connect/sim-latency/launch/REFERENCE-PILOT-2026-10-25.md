# Warm-reader reference pilot — predeclared execution

Prepared after the cold phase was published in server main `8790d9626`.
This is research qualification, not an eligible submission, official epoch
baseline, hidden-seed campaign or source promotion. The production opening
remains 2026-10-28T00:00:00Z.

## Immutable scope

Use the exact image/source, scale, resources, timers and public seed from the
[completed cold R9 check](../baseline/prelaunch-cold-r9-2026-10-25/completed/README.md):
Go 1.27.3, server `0d58d0d91b069c62bcfb39cd72166c26925b786a`, evaluator
`sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`,
3,600 providers / 200 clients / impairment on, public workload seed 20261024,
R9, 180-second measured windows, and a three-hour complete invocation budget.
Do not change any gate, margin, timeout, allowed path or source pin.

The sole candidate is
[`warm-readers.patch`](../evaluator/references/2026-10-25/warm-readers.patch),
SHA-256 `2957ef507f7edc9dadcb09abd6425664e88748612917959a9a65243a0365ebe4`.
It changes only `connect/resident_contract_manager.go`. Its selected correctness
tests already passed 312 race executions; those timings are not performance
evidence. No other candidate may be substituted into this attempt.

## Execution

1. Prepare a fresh private mode-0700 directory and new offline round/job IDs.
   The complete typed request and fixture declaration must precede execution.
   Synthetic API/worker/hardware labels remain explicitly synthetic.
2. Run the whole preflight and evaluator under the same UID-0 manager principal,
   using the pinned trusted script and unchanged private synthetic local leaves.
   Hold the shared scoring slot through terminal cleanup; ensure original
   containers/networks only and inactive staging supervisor before launch.
3. Supply **no cached baseline**. Collect nine new controls, build this exact
   patch offline, then collect nine new candidates and score normally. The
   cold no-op's control measurements are not reused. Reusing the public workload
   bytes is input reuse, not measurement reuse.
4. Keep the actual three-hour deadline and bounded safety cleanup. No overlapping
   scoring, tests, builds or resource-pressure work. Observe this one invocation;
   do not restart because an observation times out.
5. Preserve every result, raw log, score, resource/accounting report, identity
   and cleanup outcome. No automatic external retry or second patch follows.

## Decision before seeing results

First require complete integrity/resource evidence, unchanged G1–G6, placement
and exact cleanup. A failed build/gate/budget/infrastructure outcome is retained
and diagnosed, not censored or silently replaced.

For advancement to held-out research testing, require **both** the candidate's
median raw score and run-level mean raw score to be lower than its fresh control.
Retain the exact effect, variance, Welch p-value and takeover verdict. Passing
this directional pilot alone does **not** establish statistical improvement or
qualify it as the final better reference. Production takeover still requires
its full practical-margin and significance rules without alteration.

If either direction fails, do not call the patch better and do not retry the same
pilot for a favorable draw. Diagnose/research before proposing another patch.
If the directional screen passes, predeclare the independent-seed comparison
and its acceptance rule before creating/using its held-out seeds. Keep pilot
seed 20261024 separate from that campaign. Fresh formal stability/margin
calibration is also separate; neither this pilot nor the earlier cold control
is automatically included in it.
