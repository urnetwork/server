# R2 scoring restoration: readiness before hold release

## Current maintenance status — October 30, 23:11 UTC

Production R2 epoch 1 remains open for intake, but evaluation hold **4**
(`CALIBRATION_MAINTENANCE`) and reward-delivery hold **1** are active. The real
worker is stopped and joined. The original one-use public pilot is running;
all nine controls and one of nine candidate runs have completed. It is not
accepted, and no fresh hidden seed has been drawn. Completed owners can now
read their private scores while public results stay sealed.

Use the [current checkpoint](current-state.json) and
[live maintenance evidence](CALIBRATION-MAINTENANCE-LIVE-2026-10-30.json), not
the earlier grading snapshot below. Maintenance authority expires at its
original **October 31, 07:19:42.289655 UTC** bound and must not be renewed.
Do not restart the worker during timed calibration. Before release, require
actual accepted calibration, independent review, scoped COMPLIANCE custody,
joined cleanup and genuine worker readiness; neither this guide nor an archive
receipt supplies that approval.

## Historical provisional-grading snapshot — October 30

The owner subsequently approved [provisional grading](PROVISIONAL-GRADING-OWNER-APPROVAL-2026-10-30.md),
moving calibration to a pre-finalization gate. Production grading is now running:
the shared R9 baseline was published at 09:23 UTC, the first submission succeeded
at 10:50 UTC, and the next FIFO submission is running. The exact observations
are in [the live checkpoint](PROVISIONAL-SCORING-LIVE-2026-10-30.json).

Do not replay the hold-release sequence or replace the current unit with the
historical unit below. The active unit now hashes to
`9ede9888a35bfcc14f65b219a12ce3d1219f7cb8195c109be761c81b5d82303e`;
its independently qualified worker fixes the zero-sample preparation failure
without changing the frozen evaluator, R9, gates, margin, or submission clock.
Reward delivery remains held and the normal epoch embargo still applies.
Reserve a genuine serial calibration window; never race production timing for
the canonical lock or fabricate host freshness to keep intake green.

## Historical pre-amendment sequence

This is an audited operational sequence, **not a release approval or evidence
that scoring is enabled**. At 03:14 UTC on October 30, intake was open for R2
round `01a1855d-49bd-0250-90db-d0c0885f0e46`, while grading and publication were
held. The worker is deliberately stopped for uncontended calibration.

The original failed hidden screen and complete rejected public pilot remain
failed. The owner-approved reduced reference, new public pilot and one fresh
hidden-seed screen retain R9, all gates, alpha 0.05, the 26.9% practical margin
and the 10,800-second per-job cap. Source, toolchain and scoring inputs remain
frozen; no main API/config redeployment is required for this amendment.

## 1. Finish and authenticate the required evidence

Require the actual new operator's three 233-test processes and final integrity
checks; an accepted complete replacement public R9/R9 pilot; its direction
publication and native AFTER13 census; and the accepted single hidden-seed
screen. Preserve every failure and the original unstarted second seed.
Complete independent result review and the new scoped COMPLIANCE archive's
exact-version SHA-256 readback. The old Qwen/Gemma and strong-reference archive
does not cover these later results.

Reuse the genuine six-case Qwen/Gemma group qualification only with its exact
installed profile/provider tuple and retained disagreements. Do not make fresh
inference calls or add another qualification gate merely to restore the worker.
No image export, heavy archive work, recurrent Docker probe or unrelated test
may overlap timed calibration. Production needs its own fresh epoch baseline;
none of these calibration controls is that baseline.

## 2. Restore the existing worker with both holds still active

After all strict calibration replay/verification/census operations have finished,
start the existing unit, without bootstrapping a new source or round:

```sh
sudo systemctl start urnetwork-competition-production-r2.service
```

The audited unit is Root-owned, management CPUs 20,22, 8 GiB hard memory limit,
no swap and control-group cleanup. The historical unit-file SHA-256 was
`5a254d861ec63a99cc94fcf7dcff9a75a1c4a077874cefb59b1bf4c00e80f9a6`;
the actual October 30 maintenance unit was independently rehashed as
`9ede9888a35bfcc14f65b219a12ce3d1219f7cb8195c109be761c81b5d82303e`.
Freshly verify the actual existing unit and its qualified hooks before recovery;
do not install or replay a historical unit from these recorded pins.
Use the unit's existing pinned worker/review/build hooks and protected runtime
environment; do not capture credential contents or construct another supervisor.

The worker produces its genuine runner heartbeat every 15 seconds and performs
real host registration/self-checks every 20 seconds while evaluation is held.
It can therefore establish readiness without claiming a submission or emitting
a fake heartbeat. Recurrent probes are appropriate only after calibration ends.

## 3. Verify strict readiness and actual monitoring

Require `/competition/readyz` HTTP 200, `ready: true` and all 11 checks present
and true: configuration, frozen policy, retention window, database, FIFO slot,
queue admission, authoritative evaluator host, artifact storage, host rebaseline,
API image identity and artifact archive. `intake_ready: true` alone is not proof:
the initial-intake compatibility path deliberately permits offline grading.

Verify the actual worker's advancing heartbeat in main Grafana, its 15-second
cadence, 30-second stale warning and the separate host's 60-second freshness
bound. A fresh ingestion sample containing an old producer timestamp is stale.
Check the existing canonical `service=sim-latency`, `severity=warn|page` route to
the main Slack contact policy, owned by `support@ur.xyz`. Do not treat a method
or authentication response from a collector, an unrelated service's heartbeat,
or an old synthetic Slack test as current ingestion/delivery proof.

The retained GET-only audit is
`/home/by/urnetwork/temp/prelaunch-monitoring-readonly-20261027.FmllVCBw/audit`
(`db87631d…`); its normal main resolver reads existing Grafana credentials
in-process. Never put tokens in arguments or logs, trigger synthetic alerts, or
alter the current contact policy to obtain a green check.

## 4. Read current state, then release each hold separately

Only after reviewing an actual recovery-binding artifact containing the accepted
calibration, custody and qualification evidence, read the latest exact-scope
`evaluation` and `reward_delivery` hold rows. There is no latest-holds GET route.
Do not replay an earlier POST as a readback or reuse historical revision 1.
Scope is environment `main`, competition `sim-latency-season-1`, season
`sim-latency-production-20261028-r2`.

POST `/competition/v2/holds` with exactly the existing seven request fields:
`kind`, `active: false`, the freshly observed `expected_revision`, a new
`idempotency_key`, `reason_code: RECOVERY_APPROVED`, a truthful
`public_explanation`, and the actual reviewed `evidence_sha256`. The API checks
digest syntax, not the artifact's adequacy; the operator must review the evidence.
Use the separately scoped scheduler credential for evaluation and settlement
credential for reward delivery, read from their protected files in-process.

**Clearing evaluation also unholds score/winner publication.** Reward delivery
controls payout only; leaving it active does not preserve the publication hold.
Consequently, never clear evaluation to test readiness or before accepting the
evidence. The two releases are independent, not an atomic transaction.

Fresh-read immediately before each release. Require the exact scope/kind,
inactive state, expected next revision and matching request/evidence digest in
the response and subsequent readback. Conflict, a new incident or an uncertain
response stops the sequence for inspection, not blind retry or rollback.
Finally verify `grading_ready: true`, `publication_held: false`, the same round
and source, and the real worker's first FIFO claim. A running idle supervisor
alone is not proof that submission evaluation started.

The complete private, source-only audit and deliberately unbound request
templates are retained at
`/home/by/urnetwork/temp/r2-post-calibration-restart-release-20261030.HdAsnslr/`
(manifest `7cb61720724a6e0069f408f475712ce81240444943744e1a46daa08cf689265e`).
They do not contain current hold revisions or authorize release by themselves.
