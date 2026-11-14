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

The narrow offline validator subsequently passed 75 ordinary and 75 race-test
executions. Its first build's compiler succeeded, but a post-build check wrongly
expected linker flags in `go version -m` despite `-trimpath`; that actor failure
remains preserved. A separately reviewed passive ELF inspector passed 21 ordinary
and 21 race tests and verified the original binary's exact source-hash binding
without executing it. The original binary was not rebuilt.

The network-disabled offline Qwen validation then succeeded, with original
request/response/receipt hashes unchanged. Its separate receipt is
`706f503960bf354ad2032442bc4323e91d5c30aa7e6b76c54a679e6ae1c74125`.
It validates only the supplied partial scope and records the remaining source,
instrument, transitive-context and paired-review obligations. The original local
failure receipt still says invalid; neither that receipt nor Gemma's rejected
response was rewritten. A separate Qwen scoring-instrument response subsequently
completed but named `canonical.patch` as a production code path. That is evidence,
not product code: this response remains rejected without filtering, rewriting or
rerunning it. Other prepared scopes remain unstarted pending a prospectively
stricter renderer with explicit production-path choices.

These are private, nonfinal observations, not submission approvals, complete
honesty reviews, or authority to elect, reward or promote a candidate. No
candidate patch was executed by this review tooling. Full-source paired review
and protected archival of the separate offline validation remain unfinished.

At 22:06 UTC, the original Qwen execution-run call's eight original evidence
leaves and manifest were successfully archived privately in MinIO, with exact
version readback, digest verification and COMPLIANCE retention through
2028-01-01. Manifest version: `8bfc2240-7c74-460a-a1e1-56f52c873b76`;
local publication receipt SHA-256:
`931b60a762f97f7414f977853d5e36d67107df74f7be3fa49b3eefeb65e168ff`.
Actor, resource guard and original-file capture all completed successfully.
The original failed-validation flag remains unchanged. This archives provenance,
not an approval, and grants no election, reward or source-promotion authority.
Durable private operational evidence is retained at
`/var/lib/urnetwork/competition-prereview-protected-original-20261112.peSJpxpS`.

## Full-source expansion requirements

First-file observations are a provider/protocol pilot, not the requested review
of every eligible submission. The next planner must derive a private coverage
ledger from the authenticated export inventory, not from whichever passes
happen to succeed. Each obligation binds the job, patch and source-lock digests,
model member/revision, source path/blob digest, supplied content range and
relevant changed hunks. Complete-file and patch-hunk-only evidence are different
provenance classes; neither can silently stand in for the other.

Batch complete changed files within tokenizer-verified budgets, including the
candidate changes and needed callers/helpers. If a file cannot fit, prospectively
define bounded ranges with complete changed-hunk coverage and enclosing
declaration context; retain unresolved cross-range dependencies explicitly.
Do not silently truncate source or treat source-census hashes as observed code.
Instrument-only passes may be shared only as immutable factual context with
exact source identity, never as another candidate's honesty judgment.

Collect each model's missing-context requests into a union and retrieve those
files only from the frozen repository commits. Continue until every required
changed-source obligation and material transitive dependency is covered, or
report the review incomplete. A final per-candidate synthesis must see the
paired evidence, all disagreements and unresolved findings, and the six
criteria plus traffic-preservation and production-benefit checks. Findings from
either member remain present; a pass from the other does not erase them.

Before scheduling more inference, the planner and completion checker need
deterministic controls for omitted files/hunks, duplicate credit, wrong model
or source digest, partial-versus-complete provenance, unresolved dependencies,
failed/incomplete provider output and changes to the eligible-job inventory.
Persist immutable progress so reconnecting does not repeat completed calls.
Protected archival failure retries archival only, never model execution.

## Bounded preparation qualification, 22:31 UTC

The lossless coverage planner passed eleven synthetic cases three times each in
ordinary and race execution. The bounded source/hunk renderer passed six cases
three times each in both modes. The linked offline-validation retention helper
and its unchanged original publisher passed sixteen synthetic cases three times
each in both modes. Actor, guard and capture checks completed successfully.
Durable private evidence:
`/var/lib/urnetwork/competition-render-linked-synthetic-qualification-20261112.WtE2fpwl`
(manifest SHA-256
`553e2db57205e84950211b4c35da77b57810d8ed9da4d463e81d9e2e844cb78d`).

These tests qualify their stated preparation/retention behavior, not complete
reviews. No actual inventory plan or new model inference ran in those tests.
The prospectively bound full-scope provider adapter, result validation,
cross-scope context closure and actual paired reviews remain in progress.

## Protected validation and complete offline ledger

The separate offline Qwen validation is now protected without modifying its
original failed call receipt. All 23 evidence leaves and the final manifest
passed exact-version readback and COMPLIANCE retention through 2028-01-01.
Publication receipt SHA-256:
`f14b95a4c5c459c8418d9dedea6de9283e1660c751e920c213d0e0ddabd7af80`;
manifest version: `c1888ab7-0e27-4bc9-b198-df498bb27aa5`.
Durable private proof:
`/var/lib/urnetwork/competition-linked-retention-build-runtime-20261112.XE4A5FMu`.

The qualified planner also processed the complete authenticated context
inventory offline, producing ledger SHA-256
`de3ac1723be6001d80a584f85ae6ad3a73647c2028a7b6d40a8b13a7f47e1f2e`.
Every obligation remains pending; model requests, observed obligations and
accepted findings remain zero, and token admission, completion and authority
remain false. All input bytes and custody metadata stayed unchanged.
Durable private proof:
`/var/lib/urnetwork/competition-complete-offline-coverage-plan-20261112.1CXy4E8r`.
Neither operation called a model, executed a validator, queried the production
database, or published a review decision.
