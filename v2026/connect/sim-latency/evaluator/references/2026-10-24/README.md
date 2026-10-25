# October 24 reference preparation — not qualified

These inputs were prepared against frozen server
`67c87445abe6725b25b3df2a13af7eb99cd4f8ed`, not moving main. Both also pass
`git apply --check` against the corrected frozen server
`3d96d16ddb4fc2fb08d3f9003c52e30b9a47241d`: the impairment-forwarding
backport did not change this product file. On October 25, both patches also
passed `git apply --check` against clean frozen server
`0d58d0d91b069c62bcfb39cd72166c26925b786a`, which contains the transaction
snapshot and fleet-construction barrier corrections. Neither command applied
the patch or modified that checkout. Authenticated input SHA-256 values:

| Patch | SHA-256 |
| --- | --- |
| `noop.patch` | `1076be3ed61df0e14ae90c13bbe4c35442de6371064fd7d78f61ed6834e170f1` |
| `worse.patch` | `c06d1cdaef2ead29d9604263e6ec9d2f49bd95669038f6ef7038fc1ebe311f71` |

This is source compatibility only,
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
