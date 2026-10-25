# Fresh release calibration — Stage A frozen

**Stage A frozen before execution; not an accepted calibration.** The
[freeze record](CALIBRATION-FREEZE-2026-10-25.md) binds the qualified analyzer,
runner and all five ordered cohort requests before the first formal observation.
Stage B still requires exact reference patches and seed/order commitments before
its first result. The separate warm-reader pilot completed under its
[own protocol](REFERENCE-PILOT-2026-10-25.md) and failed its directional screen;
its results and the completed cold check are excluded from the formal sample.

Opening remains **2026-10-28T00:00:00Z**. This uses the previously authorized
launch-compromise independent screen of five seeds / four correctly ordered
seeds, but keeps actual production **R9** throughout. It does not claim the
original twenty-seed / nineteen-pass confidence. No shorter windows, reduced
replicates, waived gates or overlapping scoring are permitted.

## Bound release

The proposed release is frozen server `0d58d0d91b069c62bcfb39cd72166c26925b786a`,
canonical ten-repository lock
`37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`,
actual Go 1.27.3, evaluator
`sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`.
Use the complete unchanged cold policy: 3,600 providers, 200 clients, impairment
on, all timers/resources and the three-hour invocation bound. No local operator
repository is modified. Direct synthetic local leaves and explicit offline
control-plane fixtures remain separately identified, never live readiness.

Changes to source, measuring instrument or workload policy invalidate their
qualification linkage and require an explicit new release/protocol; keep the
old observations. Never silently relabel them as measurements of a correction.

## A. Same-seed stability and initial margin

- Collect **five new complete A/A cohorts**, each with its own offline round/job
  identity: nine fresh controls, offline comment-only no-op build and nine fresh
  candidates. Total: **45 control + 45 no-op executions**, not five raw runs.
- All use public workload seed 20261024 and identical input bytes. Input reuse
  is intentional; no measurement, baseline file or score from another cohort,
  pilot or cold check is reused. Only source/input/build-object caches may be
  reused where their immutable identities and isolation are authenticated.
- Every cohort must complete within three hours, retain complete valid inputs,
  be placeable under unchanged G1–G6, and have exact cleanup. Retain any failure;
  do not take the first five convenient successes out of a larger sample.
- Retain raw scores and all gate diagnostics, variances, means, medians,
  significance fringes and p-values for every cohort. Five cohorts are five
  aggregate observations, not ninety independent aggregate observations.

The analyzer must reuse the production aggregation/gate/significance functions,
not a separately approximated scorer. Before formal execution, deterministic
synthetic fixtures must prove identity binding, exact R9 counts, invalid/nonfinite
input refusal, unchanged gate boundaries, failure inclusion and repeatability.
It must reproduce retained production results before bootstrap analysis.

Predeclared bootstrap: 100,000 draws, Go PCG seeds `0x20261025` and `0x9`.
For each draw, choose one of the five cohorts uniformly; independently resample
nine control records and nine no-op records with replacement **within that
cohort**, then apply the production aggregates and G1–G6. Preserve the observed
between-cohort variation; never pool records across different cohorts into a
synthetic scoring baseline. These draws are analysis, not new measurements or
proof of independent seeds. Report the fraction placeable and every gate's
pass fraction. Require overall placeability estimate **at least 0.94**, the
existing launch-compromise target; do not claim a familywise 95% guarantee.

Choose the initial practical margin as the maximum of:

1. the historical preparation floor, 0.161;
2. four times the bootstrap aggregate-score coefficient of variation, separately
   for control and no-op, taking the larger;
3. every supported production-scorer next-margin recommendation and combined
   required-improvement percentage from the five observed cohorts, as fractions;
4. the bootstrap 95th percentile of positive no-op median-score improvement
   fractions, clamped below at zero.

Round **up** to the next 0.001 fraction (0.1 percentage point). It must be finite,
positive and at most 0.50, consistent with the production supported-margin guard.
Do not lower it to manufacture a better reference or promising winner. Retain
every contributing term, algorithm identity and rounding result. Replay all
five retained pairs under that margin using the real scorer; no no-op may be
takeover-eligible. Freeze the resulting margin **before** held-out measurement.
This is calibration, not held-out validation of the margin's false-positive rate.

## B. Held-out independent-seed reference screen

Before the first result, freeze exact patch/build digests for no-op, a qualified
directionally faster reference and a worse reference. A failed pilot does not
qualify the warm-reader patch. Require valid builds and preserved correctness
screens appropriate to each control; never turn gate tampering into a reference.

Generate five fresh independent workload seeds, excluding pilot/public/prior
campaign seeds. Precommit **all** seed commitments and randomized per-seed
reference order before the first result. Keep seeds private until the campaign
ends, then reveal them with the retained commitment checks. Do not replace an
unfavorable seed or tune the patches/margin after seeing held-out results.

Each seed has its own offline round and **one freshly measured R9 control**.
The first reference's normal bounded invocation establishes it; the other two
reference jobs consume that exact authenticated baseline in the same declared
round. Each reference receives nine fresh candidate runs. This is explicit
same-round shared control, never reuse across seeds/rounds/submissions' unrelated
controls. Total: **45 control + 135 reference executions** across five seeds.
Every job retains its normal three-hour limit, even the cold first job.

Acceptance:

- at least **four of five seeds** rank canonical median raw score
  `better < no-op < worse`, all against the declared shared control;
- all five no-ops are placeable and none is takeover-eligible under the frozen
  margin; report exact p-values and any statistically significant no-op result
  even when the practical margin prevents takeover;
- complete identity/resource/integrity and cleanup evidence for every job;
- retain nonplaceable/worse results and all failures. An incomplete result cannot
  be counted as correctly ordered. Reference ordering alone does not establish
  production takeover eligibility or a twenty-seed confidence claim.

No statistical/gate failure is retried for a favorable draw. A diagnosed genuine
infrastructure retry may reuse its exact committed seed/identity within the
existing three-attempt ceiling **and the original submission-wide three-hour
deadline**. Build, measurement, scoring, drain, retry backoff and prior attempts
all consume that same budget; a retry cannot restart the clock. Retain the
original attempt and reason. A code
or instrument correction cannot be hidden as a same-release retry. A failed
held-out screen requires investigation and a new predeclared fresh campaign,
not an increased margin or substituted seeds.

## Freeze and handoff

Archive all observations and analyzer inputs/outputs, verify original bytes,
commit/pull/push each phase. Bind final scale, R9, timers and margin to the
qualified source/image/policy. Then complete exact-image host rebaseline,
provider-backed review/test qualification, live MinIO, API/worker readiness and
monitoring. Passing this statistical screen alone never activates production.

The planned measurement cost is 270 full-window executions, plus build/scoring
and cleanup overhead. The successful cold check informs planning only; it is
not a promise of identical per-seed duration. Admission stays fixed at the
announced opening, with no claim of readiness until the full acceptance evidence
is verified.
