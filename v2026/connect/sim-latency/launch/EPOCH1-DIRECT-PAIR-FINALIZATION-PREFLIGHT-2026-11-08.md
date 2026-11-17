# Direct pair review: finalization integration

The operator explicitly approved a bounded, complete-source Qwen/Gemma review
instead of the lengthy pre-review qualification. The real candidate run is
recorded in `EPOCH1-DIRECT-PAIR-RUN-2026-11-08.json`. Neither its local owner nor
its archive receipts are a native database-owned handoff or final winner approval.

## Existing API boundary

The existing `/competition/v2/review-handoff-imports` endpoint requires a completed
native `competition_review_handoff` record and its exact pending proposal.
The direct run deliberately does not claim those records exist. Ordinary
`/competition/v2/rounds/{roundId}/reviews` also checks the durable closing-review
checkpoint and deadline. Source activation rechecks the approved review against
that checkpoint. Merely posting the direct result to either endpoint is not a
verified finalization path.

Before final user approval can be applied, complete an explicit, separately
authorized operator import/exception for this direct protocol, or prove another
existing supported path against the actual current policy and checkpoint. Such
an import must retain:

- the exact elected revision, decision, seal, patch, source-lock, score, baseline,
  and artifact identities;
- the operator's direct-review authorization and the true local owner times;
- protected exact-version originals, full source coverage, both genuine model
  finals, all attributed findings, and their union;
- distinct model completion and operator approval timestamps;
- the unchanged traffic-preservation, production-benefit and frozen scoring
  criteria; and
- independent reward/source holds, final user approval, and source handoff.

Do not backdate a database owner, renew an expired native owner, invent native
qualification/call receipts, rewrite an old proposal, manufacture a disqualification
and appeal, bypass the database guards with ad-hoc SQL, or call the direct result
an existing qualified-native handoff. Calibration remains unaccepted and must be
explicitly acknowledged in the final approval request.

## Current next actions

Continue the same live direct run without changing its owner, source, plan,
criteria or model calls. Prepare the honest control-plane acceptance path while
it runs. Only after complete, authenticated Qwen/Gemma results are available,
present their findings and remaining caveats for explicit final user approval.
No review, reward release, source promotion, or epoch transition is authorized
merely by this preparation document.
