# Deadline-compatible calibration amendment

The competition owner explicitly approved **one corrected fresh R9/R9 bridge
and two independent seeds**, requiring correct reference ordering on **both**
seeds, on October 27, 2026. This supersedes the previously approved three-seed
count prospectively, before either independent seed has been drawn. It reduces
calibration confidence; it is not equivalent to the original 20-seed plan.

The three accepted original same-seed A/A pairs remain preserved evidence.
The earlier failed C2 bridge remains failed, with its nine raw control runs
retained unscored. It is not resumed, re-scored or substituted for the approved
fresh bridge. The corrected bridge must pass before independent-seed testing.

Unchanged requirements:

- Frozen C2 source, ten-repository lock, Go 1.27.3, workload and impairment.
- Nine replicates, the frozen 26.9% margin, significance test, all gates and
  the three-hour submission evaluation cap.
- Fixed no-op/worse/better reference artifacts and correct ordering on both
  independently drawn seeds; no favorable-seed selection, extra replacement
  seed, automatic reroll or discarded unfavorable outcome.
- One scoring process on the dedicated evaluator host. No builds, unit tests
  or model qualification may overlap timed bridge or independent-seed scoring.
- Authenticated plans, source/image/tool identities, original deadlines,
  retained outputs, containment, artifact integrity and cleanup.

This document records the owner's **scope approval**, not a completed
calibration or an executable seed-draw authorization. The corrected native
two-seed tool must pass its complete 134 noncommand guards three consecutive
times with zero skips, then publish its exact executable/plan/input authority
before any real draw or measurement. The two opt-in operator commands are not
unit tests. A failed private fixture test remains a failed test, not evidence
that the tool is qualified.

Production's separate repeat-reward choice is also explicitly approved:
`repeat_reward_policy: allow`. An identical identity may compete across epochs
only under that epoch's own fresh baseline, gates, margin and significance.
Within-round duplicate result reuse is unchanged. An approved winner still
becomes the next epoch's source through the adopted review/build/qualification
flow; cross-epoch eligibility does not bypass source promotion or review.

The owner is applying main migrations 891–893 through the normal deployment
pipeline. Their completion requires a fresh read-only ledger check; this
approval does not claim they have already run or permit migrating through the
pooled PostgreSQL connection.
