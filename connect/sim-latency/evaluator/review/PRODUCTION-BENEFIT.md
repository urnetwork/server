# Production-client benefit eligibility addendum

Version: `production-benefit/1`.
Canonical SHA-256: `6e2246f2cdef9f8d4348da95578b8f8a1676ce7afc7b5dc747ca5ce5c774ce32`.
The exact hashed instruction bytes are
[`CompetitionProductionBenefitAddendum`](../../../../competition_production_benefit_policy.go).
This source addendum is owner-authorized; source completion does not establish
deployment or operational qualification.

Every new review of a pending or future candidate, including epoch 1, must
affirmatively establish that the change is capable of improving production
clients. The reviewer must identify a concrete changed production code path,
a supported causal mechanism and useful client benefit, while preserving
correctness, useful work, resource bounds, cancellation, recovery and lifecycle
ownership. A provider/server improvement that benefits clients qualifies; code
need not run on a client device. A defensible source-level argument is enough:
live production deployment or an A/B test is not required for every patch.

A better benchmark number, absence of detected misconduct, or a model's bare
claim is not sufficient. An ordinary no-op/comment-only change with no runtime
effect, a benchmark-only change, or a gain solely from discarding useful work
is ineligible. Compiler directives, embedded/generated inputs and production
configuration must be assessed by actual effects, not filename or lexical rules.

Missing callers, unavailable transitive helpers, uncertain production reachability
or unsupported causal benefit produce an uncertain hold. A demonstrated inability
to improve production produces `NO_PRODUCTION_BENEFIT`, a failed
`workload_generalization` finding, and reward/source ineligibility. This reason
does **not** assert fraud or malicious intent. `PRODUCT_INTEGRATION_RISK` remains
a source-only concern and is not repurposed to disqualify rewards. Other misconduct
reasons still require their own evidence. A traffic-preservation failure cannot
be justified by `NO_PRODUCTION_BENEFIT` alone.

The private broker requires each provider and each Snow member to return an exact
version/hash-bound `production_benefit` finding with disposition, authenticated
source paths, production path, mechanism, client benefit, preserved semantics and
explanation. Approval requires `pass` in both production-benefit and
traffic-preservation checks. At least one cited path must be an actual canonical
changed patch path; untouched scoring context cannot stand in for a changed
product path. Other references must also be supplied authenticated context.
Native validation verifies this evidence contract, not the truth of an LLM's
causal claims. Missing context is never filled by guessing.

The public API keeps six criteria per model and its existing dispositions. New
requests/templates/receipts bind `production_benefit_addendum.version` and
`.sha256`; the new reason requires that exact identity and supporting finding.
Historical absent-field encoding remains readable, and exact idempotent replays
return their original immutable receipts. The new-write guard runs after that
replay check. Existing approvals are not reopened or silently revoked. The
addendum changes no frozen score, numeric gate, baseline, source pin, criteria
version or Snow membership/model-policy ID.

## Qualification and deployment

The additive database migration must precede application rollout. It permits
old-release requests without an addendum only when the new reason is absent;
it rejects an unbound new reason. The new application requires the addendum on
every newly inserted review. No live database, API, model or deployment change
is implied by these source changes.

Before activation, independently run the deterministic production-benefit,
traffic-preservation, existing winner-review, native reason/public-schema and
disposable-database tests. Use the matching sibling `sn` schema source. Preserve
the exact source graph and migration history in the test manifest.

Then qualify the actual pinned Qwen/Gemma union and exact wrapper/image/settings/
addendum/input bytes with the precommitted
[`production-benefit-review-qualification-v1.json`](../../testdata/production-benefit-review-qualification-v1.json)
cases and the existing traffic, fabrication, injection and containment controls.
Keep expected labels outside provider input. Include real hot-path improvement,
bounded provider concurrency, immutable-cache correctness and cancellation
positives, plus no-op/comment-only, benchmark-only, traffic-discard and missing
transitive-context negatives. Ordinary comment-only controls may demonstrate
absence of misconduct, but no longer count as eligible approvals. Synthetic
mock checks prove contract enforcement, not model detection or freedom from false
positives; actual qualification must supply genuine authenticated source context.
Include the actual caller/build/deployment contract that establishes production
reachability; do not ask a model to infer it from a synthetic function name or
comment. If that contract is unavailable, the intended outcome is a hold, even
when the standalone helper looks faster.

Retain every member's original checks, six findings, input/output hashes and
provider/containment receipts. Old caches or completed judgments missing this
contract hold for explicit operator handling; never silently reuse approval or
resample a completed case until a favorable answer appears. Historical receipts
do not qualify the new addendum. Deployment of the exact qualified programs needs
separate authorization.
