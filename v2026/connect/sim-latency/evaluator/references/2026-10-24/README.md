# October 24 reference preparation — not qualified

These inputs were prepared against frozen server
`67c87445abe6725b25b3df2a13af7eb99cd4f8ed`, not moving main. Both also pass
`git apply --check` against the corrected frozen server
`3d96d16ddb4fc2fb08d3f9003c52e30b9a47241d`: the impairment-forwarding
backport did not change this product file. This is source compatibility only,
not a build, correctness or performance qualification. They are private
calibration candidates, not submissions, accepted references, published images
or evidence of measured separability.

- `noop.patch` changes only the opening comment of the allowed contract-manager
  file. It is intended as an unchanged-behavior control.
- `worse.patch` deliberately adds a fixed 25 ms delay to each live contract
  authorization call. This is an artificial negative control, not a safe or
  desirable implementation pattern: blocking shared packet callbacks is exactly
  a performance defect the competition should detect. Correctness, containment,
  placeability and measured ordering still need verification.
- A genuinely honest faster reference is **not selected yet**. The old
  two-direction PostgreSQL-query optimization does not apply to the current
  Redis-projection/cold-read ownership architecture. Do not label a no-op, a
  bypassed authorization check or synthetic metric manipulation as "better".

Before any campaign, authenticate patch bytes, build each against all ten frozen
repositories through the offline isolated builder, run correctness tests, review
authorization/expiry/cancellation/ownership behavior, and retain actual image and
binary digests. Then predeclare the seed and measurement protocol and retain all
attempts. Source compatibility alone proves none of those gates. Existing
historical reference performance results must not be carried into this campaign.
