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

The subsequent fixed-round read-only census succeeded. Candidate identities,
eligibility details and subsequent model findings are retained privately rather
than published as a live ranking or per-submission reveal. The private census
snapshot digest is
`476e8a9a38d447322332197c2f8565243c0984639d190afa6cdade7ac0cedd8c`.
This is a database snapshot only: immutable archive authentication and complete
source preparation are still required before sending a candidate to a model.
The first census queried the wrong column (`epoch` instead of `epoch_number`);
the corrected read succeeded without weakening baseline identity checks.

The existing official winner cache does **not** accept an in-epoch advisory
result. Official reuse needs an explicit adapter that validates the actual
election and unchanged factual inputs, while preserving the original advisory
purpose and model-input provenance. Until that is implemented and verified,
retained pre-reviews are useful evidence, not automatically reusable approvals.

## Required checks before the first private model request

Use synthetic fixtures to prove that a successful but gate-failed, below-margin,
insignificant or reward-excluded job is not selected. Reject changed patch,
score projection, object version, baseline, source, instrument or policy
identities. Verify that no seal, election or authoritative review write is
needed or produced. Exercise both successful model members, conflicting
findings, incomplete responses, wrong model identities and provider failures;
all terminal evidence must survive rejection. Verify private output permissions,
bounded inputs and source coverage, cache invalidation and duplicate execution
control. Run no candidate code in the review process.

The fixed census reader passed nine synthetic cases three times ordinarily and
three times with the race detector (54 passes, zero failures/skips), using
network-disabled execution. Source and dependency identities were unchanged.
The first 512 MiB compiler attempt ran no tests and remains an unqualified OOM
failure. The fresh 2 GiB test-only attempt passed with a 630.1 MiB peak; no
submission limit, gate or scoring parameter changed. This qualifies the census
reader's tested boundaries, not the unfinished archive/provider pipeline.

## Initial provider transport evidence

Both members completed a small synthetic strict-schema request on November 12,
returning HTTP 200, their expected model name, valid JSON and real token usage:

| Member | Elapsed seconds | Input tokens | Output tokens |
| --- | ---: | ---: | ---: |
| Qwen | 129.227 | 283 | 394 |
| Gemma | 103.141 | 67 | 400 |

No candidate data or credentials were sent. Calls were serial and tool-free,
with bounded memory, CPU, response size and duration, an explicit singleton
lock and egress restricted to Snow. Raw responses, headers and transport
metadata were preserved privately, including the corrected launcher script.
An initial launcher exited without response files; it is not counted as proof
of model execution.

This checks transport, not review quality, complete source handling, model
weight provenance or the current full review schema. It does not qualify an
official review or authorize approval. Durable receipt:
`/var/lib/urnetwork/competition-private-prereview-transport-smoke-20261112.xrN2WOPt/receipt.json`,
SHA-256 `564036c230302c24c89e95907f85046fa6ec76bc34f2522d870e9cc5113cd9c5`.

## Authenticated export milestone

The corrected read-only exporter authenticated the currently eligible jobs'
retained original scores, canonical patches, shared baseline and frozen Git
context. Actor, guard, source/dependency capture and resource checks all passed.
The private inventory digest is
`38022aaaccdb05585eadabd352489ebdf08fdc36a2944179b96f386ed3a0fac1`.
All copied context bytes were compared and flushed to persistent storage at
`/mnt/data/competition-private-prereview-contexts-20261112.WABbFBO6/contexts`;
directories are root-private and files root-owned/read-only. Protected MinIO
publication of the eventual review results remains a separate step.

Corrections were confined to the new private exporter and its management
sandbox: hydrate activation timestamps from SQL rather than immutable prepared
intent JSON; bind egress to the endpoint selected after host settings load;
override the general blob bucket with the trusted competition artifact bucket;
and trust exact owning Git repositories for nested replacement modules, without
global Git trust. These did not alter live scoring or its frozen instrument.

The corrected exporter cut passed 23 synthetic tests three times ordinarily and
three times under the race detector (138 passes, zero failures/skips). Its
actor, guard and capture succeeded within the unchanged 128 MiB cache-growth
budget. A previous cut's 114 native test passes exceeded that budget and remain
an overall failed qualification, not transferred credit.

The exported packets are explicitly **not token-admitted or authoritative**.
They retain complete supplied file bytes, not a claim that every transitive
caller is already covered. Bounded partial code-observation passes may run
after their own authentication and input checks, but must remain nonfinal and
cannot approve or disqualify a submission. Complete paired review, union of
findings and independently verified official reuse are still required.

## Bounded observation milestone

The first full-instrument inputs exceeded both members' tokenizer-verified
context budgets and were refused before generation. A subsequent Qwen pass
with an 8,192-token output allowance exhausted that allowance in reasoning;
its incomplete response remains preserved and is not a review result.

The next observer cut reserves 32,768 output tokens and divides the instrument
into four explicit, complete-file groups. Sixteen deterministic controls passed
three ordinary and three race repetitions (96 passes, no failures or skips).
The model pair, reasoning efforts, context limits and generation deadline did
not change. All groups and remaining changed-source context are still required.

Both first-group provider calls returned completed responses. Their original
local validators rejected the observations, so neither currently counts as a
validated partial review. Qwen's rejection identified a citation-boundary issue:
an authenticated changed-file patch path was rejected because its complete
pristine file was deferred. A separate offline validator is being prepared to
distinguish patch-hunk-only evidence from full-pristine evidence, retaining
unknown-path rejection and every response-integrity check. It must validate the
original preserved response without another inference call or modification of
the original failure receipt. Gemma's local rejection also remains under
diagnosis. Other scopes remain held pending these checks.

These are private, nonfinal observations, not submission approvals, complete
honesty reviews, or authority to elect, reward or promote a candidate. No
candidate patch was executed by this review tooling. Protected result
publication and full-source paired review remain unfinished.
