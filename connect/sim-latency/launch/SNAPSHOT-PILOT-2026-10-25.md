# Snapshot reference pilot — prospective execution

Status: preparation only; no pilot request generated or execution started.
This is a separate hypothesis from the failed warm-reader pilot, not a retry
of that patch. It is not production activation or a formal calibration cohort.
The October 28 00:00 UTC opening is unchanged.

The [correctness/supporting screen](../evaluator/references/2026-10-25/SNAPSHOT-QUALIFICATION.md)
passed 516 race-enabled test executions and two deterministic negative controls.
Warm-hit median improved 73.4887%, but 128-pair publication cost rose 40.3179×
and allocations increased. This trade-off justifies one end-to-end experiment,
not a conclusion about actual scoring or memory performance.

## Frozen experiment

Use server `c1f9dda603d6279666fca5d92b99c5663306c5f7`, the unchanged other nine
repository pins, actual Go 1.27.3 and exact base image
`sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3`.
The sole candidate is `warm-snapshot.patch`, SHA-256
`46bc95e3f2719f26f939be7a86c274d0b98bb2b1027f1ef4537d61de4c4ec924`;
only `connect/resident_contract_manager.go` changes. Qualification tests and
benchmark files are not part of the candidate patch.

The complete workload/policy is unchanged from corrected-release Stage A:
3,600 providers, 200 clients, impairment on, public workload seed 20261024,
180-second measured windows, nine fresh controls and nine fresh candidates,
provisional margin 16.1% and unchanged G1–G6. This pilot neither selects a new
margin nor contributes samples to formal Stage A. No existing measurements
or cached baseline may be reused.

Use the exact separately qualified trusted bundle
`/usr/local/libexec/urnetwork/competition-737dd2371-go1273-isolated-v1/container/`
and the immutable runner recorded in the corrected-release freeze. Hold the
canonical exclusive slot through logging and cleanup; keep management cores
reserved and expose only the unchanged direct synthetic config/local and
vault/local leaves. Synthetic API/worker/host labels are not live receipts.

## Preconditions and execution

1. Publish the completed correctness/benchmark phase before this larger run.
2. Generate new offline job/round IDs in a fresh root-owned mode-0700 directory.
   The [preparation helper](snapshot-pilot-preparation.go.txt) is a draft until
   compiled and independently verified; do not treat this document as that proof.
   Pin the complete request/provenance/patch/provider digests and preserve them
   with the generator and runner identities before execution. Independently
   compare all unchanged request fields against the frozen template.
3. Publish the complete pre-data freeze before invoking the evaluator. No
   substitution of another patch, scale, policy, source or workload is permitted.
4. Run once under the UID-0 manager, management CPUs 20,22 and the canonical
   slot. The ordinary invocation collects nine new controls, builds the candidate
   offline within its existing whole-job budget, records actual candidate
   image/binary identities before its runs, then collects nine candidates.
5. Retain the 10,800-second execution deadline and the runner's bounded safety
   cleanup/outer watchdog. No other scoring, builds, tests or pressure probes
   may overlap. Observe the same live handle; an observation timeout is not a
   reason to restart.
6. Preserve every original raw artifact, accounting/resource report, gate,
   score, variance, p-value, source/build identity and terminal cleanup receipt.

## Decision fixed before results

Require complete integrity/resource evidence, unchanged G1–G6, placement,
whole-job budget and exact cleanup. Also require **both** candidate median raw
score and candidate run-level mean raw score to be lower than the fresh
control. Record significance, practical-margin and takeover results without
changing their thresholds. A directional pass alone is not a statistically
significant improvement or a qualified held-out better reference.

If either direction fails, retain the unfavorable outcome and do not retry
this pilot for a favorable draw. Build, gate, infrastructure, integrity, budget
or cleanup failures are retained and diagnosed, not silently replaced. No
automatic retry is authorized by this protocol. Further hypotheses need their
own declared source, correctness screen and prospective experiment.

After a directional pass, Stage A still must complete and freeze its accepted
margin. Then precommit the independent held-out seeds, exact reference patch
and build identities, workload bindings and randomized order under the
[unchanged Stage B acceptance rules](CALIBRATION-PROTOCOL-2026-10-25.md).
Keep this public pilot seed excluded from that screen. Main maintenance and
all live host/provider/artifact/runtime gates remain separate launch holds.
