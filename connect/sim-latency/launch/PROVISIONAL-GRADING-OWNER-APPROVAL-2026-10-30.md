# Owner-approved provisional production grading

On October 30 the owner explicitly answered **“approved”** and
**“Approve provisional grading — yes”** to moving remaining reference
calibration from a pre-grading gate to a **pre-finalization gate**, and requested
maximum parallel progress. This is a prospective confidence/timing amendment,
not a claim that unfinished calibration passed.

Scope: environment `main`, competition `sim-latency-season-1`, season
`sim-latency-production-20261028-r2`, initial round
`01a1855d-49bd-0250-90db-d0c0885f0e46` (epoch 1).

## Unchanged requirements

- The frozen ten-repository source, evaluator image, Go 1.27.3, R9, all
  submission/runtime gates, 26.9% practical margin, significance and three-hour
  per-submission bound are unchanged.
- Production establishes its own fresh authenticated epoch baseline. No
  calibration control or another submission's measurements replace it.
- No simultaneous production and calibration measurements on this host.
- Scores remain under the normal epoch embargo until finalization. The
  `reward_delivery` hold remains active; this approval releases evaluation only.
- The rejected original hidden screen and public pilot remain rejected. The
  original second seed remains unstarted; reference selection is not revised
  after observing provisional production results.

## Completed evidence before provisional grading

The replacement operator completed 699 checks with zero failures/skips and
terminal source/dependency/Git/ELF integrity checks passed. Its exact native
qualification is `83ceb222…`; the reduced 100 ns/byte reference's separate
correctness scope passed 336 checks. The new native ten-source BEFORE census
passed (`c3f49e9c…`). These do not establish measured reference direction or
accepted hidden-seed calibration.

The existing worker was restored with both holds active. Strict authenticated
readiness independently returned HTTP 200 with all 11 checks true at
04:40:36 UTC. Two real Grafana observations verified advancing worker heartbeats,
fresh ingestion, enabled 30-second warnings and the canonical Slack route owned
by `support@ur.xyz`. No synthetic heartbeat or alert delivery is claimed.

## Deferred work and failure handling

The prospectively fixed replacement public R9/R9 pilot and subsequent single
fresh hidden-seed reference screen still must pass before epoch finalization.
They will run in reserved serial host time, not alongside production timing.
If they fail, pause grading, preserve all evidence, investigate, and determine
whether affected provisional evaluations require rerunning before publication.
Do not silently change the frozen baseline, gates or results.

Existing product holds couple evaluation and score/winner publication; a payout
hold alone does not block sealing. Before the November 5 submission close,
unfinished calibration must therefore durably reassert the evaluation hold.
An initial persistent cutoff timer is armed for November 5, 12:00 UTC, stopping
the sole normal R2 supervisor before the 13:06 UTC close if not canceled after
accepted calibration. This stop fence blocks the normal supervisor, not an
independently authorized API caller; a stronger bounded API-hold guard is being
prepared and is not yet claimed installed or tested here.

After a later evaluation re-hold, the original revision-1 intake-only exception
does not apply. Reserve/announce any necessary maintenance pause or qualify a
genuine non-interfering host-health path before pausing the worker for calibration;
do not fabricate host freshness or assume uninterrupted intake is automatic.

This approval does not complete the launch goal or authorize reward delivery,
unreviewed source promotion, skipped calibration or early result publication.
