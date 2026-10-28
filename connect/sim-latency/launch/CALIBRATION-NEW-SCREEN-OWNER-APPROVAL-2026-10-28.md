# Prospective replacement screen: owner approval

The owner replied **“Approved”** to the following question on October 28, 2026,
after the first committed screen had completed and failed:

> The approved calibration screen failed its frozen ordering, and independent
> review found no evaluator defect. Do you approve a new prospectively committed
> screen with a causally stronger regression reference, fresh control and newly
> committed seed, retaining the failed screen and keeping R9, gates, significance
> and the 26.9% margin unchanged? Exact references and the plan would be reviewed
> and published before measurement; the existing second seed stays unstarted.

This approves preparation and execution of that **new prospective screen** only
after its exact reference, protocol, native qualification and precommit have been
reviewed and published. It does not turn the old failed screen into a pass.

## Unchanged requirements

- One newly committed seed, one fresh R9 control and three R9 reference jobs;
  no reuse of another campaign's control measurements.
- Correct predeclared better / no-op / worse median ordering, valid original
  significance results, and a placeable, non-takeover no-op.
- All evaluation gates, artifact authentication, containment, the unchanged
  three-hour per-job bound, significance method/alpha and the prospectively
  frozen 26.9% practical margin remain unchanged.
- Frozen application source and base evaluator image are not changed by this
  approval. A reference patch is a separately identified calibration candidate.
- New private namespace, exact new reference identity and fresh one-use native
  claims; no reset of an old claim, clock, seed or artifact directory.
- Publish the exact prospective protocol/reference bindings and seed commitment
  before observing any new screen measurement. No result-selected retry.

## Records that must remain preserved

The failed first screen's result SHA-256 is
`c85bf64bb18418219b5bd6f2c1ae30d88d02612fab5ff7684e7d9dea5882fdda`.
Its result remains `accepted: false`, reason
`first_committed_seed_not_correctly_ordered`. See the
[complete failed screen](FIRST-COMMITTED-SCREEN-2026-10-28.md).

The original second committed seed remains private and **unstarted**. It is not
the new seed and must not be selected as a replacement draw. Keep all earlier
OOM/recovery records, controls, candidates, rejected outcomes and commitments.

## Not authorized by this approval

No late public opening, backdating or new opening date; no Snow qualification
waiver, member removal, malformed-output repair, practical-margin reduction,
changed acceptance criterion, reduced replicate count, skipped gate, or admission
before independent runtime readiness.

This record is approval, not evidence of a prepared seed, qualified reference,
completed new screen or production activation. At recording, no new seed had
been drawn and no new measurement had started.
