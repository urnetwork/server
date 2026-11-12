# Private in-epoch Qwen/Gemma pre-reviews

Requested November 12: review every currently eligible submission before close,
retain results, and discover reviewer-provider failures early. This is private
preparation, not an election, approval, disqualification, or reward instruction.

## Admission and evidence

- Select successfully scored jobs using the round's frozen gates, takeover
  margin, significance and reward-eligibility rules. A successful evaluation
  alone does not establish eligibility.
- Authenticate the exact retained patch and score against their archive
  manifests, including the archived-to-stored score projection. Bind the round,
  job, attempt, baseline, source checkpoint, instrument, image, review criteria
  and production-benefit policy. Release database connections before storage
  and model calls.
- Give both `qwen3.8-27b` and `gemma-4-26b-a4b` complete authenticated review
  context, using bounded multiple passes when necessary. Do not use Muse.
- Treat source and patch contents as untrusted data, not instructions. Reviews
  must cover honesty, safety, traffic preservation and concrete production-client
  benefit; a favorable benchmark alone is insufficient.

## Retention and reuse

Store private input identities, source-coverage manifests, individual member
findings, their union, and raw terminal provider responses with model identity,
status, incomplete reason, usage and errors. Retain artifacts in protected MinIO
by exact version and digest. A failed or incomplete model call is unresolved,
not approval or evidence of misconduct. Preserve failed attempts rather than
silently replacing them with a favorable retry.

Cache only by exact immutable evidence and review-policy identities. At close,
independently validate the actual election and all current identities before
considering reuse. Never fabricate a seal or reward revision, rewrite an old
model input to claim it saw a later election, or bypass official review guards.
Changed scores, recovered attempts, source or review policy invalidate reuse.

## Implementation status

The existing official packet export requires an evaluation seal, and its model
broker requires an elected winner. Neither is a supported in-epoch review path.
A separate, explicitly nonauthorizing path is being implemented; official
winner validators remain unchanged. Historical Qwen/Gemma qualification does
not qualify newer mandatory review context automatically.

At 17:51 UTC, epoch 2 had 16 successful evaluations and four failures, with no
queued or running evaluations. This is **not** an eligible-submission count.
No Qwen/Gemma inference had started for this pre-review request at that check.
Existing scoring and the independent recovery-tool qualification are not being
interrupted.
