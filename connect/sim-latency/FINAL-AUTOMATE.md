# Automated reward decisions and safe epoch progression

Design and implementation requirements, 2026-10-16. The implementation remains
opt-in and disabled by default. This document is not approval of new reward
policy or authorization to deploy. The repository-state comparisons below
describe the pre-implementation behavior. See [AUTOMATION.md](AUTOMATION.md)
for the implemented protocol and activation gates, and [RUN-MAIN.md](RUN-MAIN.md)
for operational commands. Neither code nor local conformance tests establish
that Apex already implements the proposed settlement protocol.

## Recommendation

Accept the direction of Macrocosmos's proposal, subject to the policy decisions
below: automatically elect a **provisional reward winner** after a closed epoch
has completely drained, publish the result, and progress the competition without
waiting for discretionary review. Conduct honesty and safety review asynchronously.
UR alone authorizes measured-source promotion and product shipping.

The essential change is to separate three decisions that are currently coupled:

1. **Evaluation seal:** all admitted work is terminal; the score evidence and
   deterministic ranking are immutable and can be published after the reveal gate.
2. **Reward election:** the first eligible, non-disqualified entry is the current
   reward recipient, subject to a versioned retrospective decision and Apex's
   explicitly agreed settlement rules.
3. **Source selection:** the next epoch uses an exact, reviewed source snapshot.
   If a new reviewed snapshot is not ready at the source-choice deadline, it uses
   the last approved snapshot. An unreviewed reward winner is never a source pin.

Do not implement automation by writing `honesty_review: approved` without a real
review, deleting the production review guard, or replacing the current manual
pause with a mandatory agent review. Those approaches either remove a safety
boundary or leave the same review dependency on the critical path.

There is a real tradeoff: epochs can continue before a reward winner's code has
become their incumbent. This is safe for source, but changes repeat-reward and
incumbent semantics. UR and Macrocosmos must explicitly accept that tradeoff.
If every epoch must build on the immediately preceding reward winner, immediate
progression and mandatory pre-promotion review cannot both be guaranteed.

### Suggested response to Macrocosmos

We support automatically selecting the qualifying reward winner after each
round closes and all accepted evaluations finish, with retrospective review.
We would keep reward eligibility separate from approval to use or ship the
code. If review is not complete, the next round can start on our last approved
source, with its own fresh baseline and frozen rules; that requires agreement
on repeated rewards while source is unchanged. We will publish review criteria,
specific rejection reasons, and an appeal process. Rejections should produce
versioned, authenticated replacement decisions, not edits to prior results.
Before enabling automatic payments, we need the exact Apex revoke/replace
interface, what happens to rewards already paid, the precise replacement-time
burn formula, and no-winner handling. Once those are agreed and proved in
staging, neither ordinary review nor a manual next-round/config deployment
should remain on the competition's critical path.

## Existing behavior and implementation boundaries

These are repository facts, not a claim about the currently deployed release.

| Boundary | Current behavior and relevant source |
|---|---|
| Production completion | The worker seals/drains, calls `PrepareCandidateReview`, and exits with a pending candidate. Approval publishes the winner; rejection advances through ranked candidates. [Controller](../../controller/competition_controller.go#L4751), [worker](../../controller/competition_controller.go#L6287). |
| Manual progression | `run-main.sh` returns status 20 for mandatory review; promotion precedes the next round. [Harness](run-main.sh#L314), [runbook](RUN-MAIN.md#L193). |
| Round creation | `CreateRound` requires the prior production round's `finalized_at`, serializes creation, and enforces the season count. [Controller](../../controller/competition_controller.go#L4048). |
| Shared statistical control | A round owns one append-only baseline, authenticated against source, workload, image, scorer, hardware, and qualification. Scored completion cannot replace it. [Validation](../../controller/competition_controller.go#L1416), [completion](../../controller/competition_controller.go#L5682), [migration](../../db_migrations.go#L7880). |
| Ranking and eligibility | Shared-control rounds rank absolute raw latency ascending, then `submitted_at`, then `job_id`. Eligible production candidates must pass every gate, takeover, significance, and the supported next-margin check. Historical per-job-control rounds retain normalized-score-first ordering. [Ranking SQL](../../controller/competition_controller.go#L4578). |
| Immutability and review | Database triggers require approval for a production winner and prevent changing a finalized `winner_job_id`. Candidate reviews are append-only and ordered before finalization. [Review guard](../../db_migrations.go#L8276), [immutable guard](../../db_migrations.go#L9001), [review schema](../../db_migrations.go#L6476). |
| Promotion | The final interlock requires the exact approved winner, or a finalized no-winner round. Promotion authenticates the patch and score, works in temporary clones, pushes source branches, and activates the config ledger last. [Interlock](../../controller/competition_controller.go#L5004), [bundle authentication](epoch_review.go#L259), [promotion](source_promote.go#L443). |
| Source ledger | Schema 1 permits `winner_promotion` or `no_winner_carry_forward`; production source selection assumes round N uses source epoch N−1. Carry-forward cannot truthfully represent a pending reward winner today. [Ledger validation](source_epoch.go#L195), [source selection](../../controller/competition_controller.go#L1496). |
| Apex contract | The reference adapter rejects an unapproved production winner. OpenAPI describes production winners as honesty-approved and final results as immutable. [Adapter](../../controller/competition_controller.go#L6982), [OpenAPI](../../../sn/api/competition.yml#L792). |
| Review evidence | The runbook provides substantive review criteria, but persistence currently validates a bounded JSON object, digest, reviewer, and free-text reason—not a complete machine-checkable findings schema. [Criteria](RUN-MAIN.md#L200), [validation](../../controller/competition_controller.go#L4612). |

The reference adapter is a durable submission/reconciliation implementation,
not evidence that this repository can revoke a chain payment or change Apex's
burn accounting. That part requires an agreed, tested external integration.

## Contracts to preserve

- Six sequential production epochs, each with a seven-day, end-exclusive
  admission window. Do not turn this into live best-so-far winner replacement or
  overlapping production rounds.
- Unlimited admitted submissions, the agreed $20 production submission fee,
  fee-free staging, one authoritative FIFO, one evaluator host/slot, and the
  three-hour total submission evaluation budget. Transport retries do not create
  new fee charges or new statistical draws.
- One immutable nine-replicate scoring baseline per evaluated epoch, then nine
  candidate replicates per distinct canonical patch. Identical patch bytes
  within a round share one result. The preparation/readiness rebaseline is not
  the scoring baseline.
- Production gate thresholds, one-sided Welch significance, and the calibrated
  takeover floor. Staging's zero-margin, best-placeable-winner behavior is a
  deliberate exception, not a production shortcut.
- The complete eleven-repository evaluation lock for new releases: `server`,
  `connect`, `sdk`, `proxy`, `glog`, `goidenticons`, `userwireguard`, `sn`,
  `operator-proxy`, `warp`, and `gvisor`. Historical ten-repository locks remain
  unchanged evidence and cannot authorize a new activation; a separately frozen
  and qualified eleven-repository checkpoint is required. Evaluation and
  promotion use private temporary checkouts; candidate
  code never runs in or modifies the continuously deployed main checkout.
- Frozen evaluator/scorer/source identities and per-evaluation API/worker
  runtime digests. Main API/worker code is currently separately maintained; the
  forthcoming main freeze is a release-planning constraint, not permission to
  alter evaluator history or to skip rollout checks.
- Retained authenticated evidence, secret-workload embargo, host qualification,
  containment, management resource reserves, and UR source-shipping authority.

Macrocosmos's existing fee/hardware agreement and acceptance of the external
evaluator are settled inputs. This plan does not reopen them.

## Two independent progress paths

```text
7-day admission → drain every accepted job → immutable evaluation seal
                                             │
                      ┌──────────────────────┴──────────────────────┐
                      ▼                                             ▼
             versioned reward election                    private code review
                      │                                             │
             publish after reveal gate                    signed source approval
                      │                                             │
             durable Apex settlement event                verified source build
                                                                    │
source-choice deadline → ready approved snapshot, otherwise carry-forward
                      → qualified next-epoch preparation → next admission window
```

The source-choice path reads completed approvals; it never waits indefinitely
for one. A settlement delivery backlog also does not block the next safe epoch,
unless an explicitly authorized incident hold is required. Evaluation integrity
and qualified readiness remain hard gates.

Private review may start while jobs are being evaluated to reduce turnaround,
but it must not leak embargoed scores, patches, workloads, or candidate status.
Public election and public review outcomes wait for the publication boundary.
Known malicious work can be quarantined immediately under incident policy.

## Deterministic automatic election

At close, stop admission and finish every already-admitted job, including
retriable infrastructure work within its original budget. The authoritative
database must show no `queued` or `running` jobs before sealing. A worker's exit,
an empty Redis list, or a stale heartbeat alone is not proof of drain.

In one serialized operation, authenticate the frozen round policy and evidence,
seal the complete job inventory and ranked score manifest, and append election
revision 1. The automatic candidate must satisfy the existing production rule:

- successful, valid stored score under this round's source and baseline;
- `placeable: true`, every required G1–G6 gate present and passing;
- raw candidate score at or below `baseline_raw × (1 − frozen_margin)`;
- `takeover_eligible: true`, statistically significant under the pinned scorer,
  and `recommended_next_epoch_takeover_margin_supported: true`;
- no applicable, authenticated disqualification under the published policy.

Do not derive eligibility from a rounded display score or infer missing gates
as passing. Reuse one validated eligibility/ranking specification across the
controller, database guard, API, adapter conformance fixtures, and documentation.
Select the first eligible entry by raw score ascending, admission time, then
job id. Do not use an epsilon tie, completion time, approval time, token identity,
or an operator's preferred replacement. Publish both overall rank and eligible
candidate rank where necessary; they are not currently the same index.

Every new production round uses its own immutable shared baseline. Do not
compare candidates evaluated against different controls, carry old baseline
samples into a new seed, rescore against the provisional winner, or reset the
baseline after seeing candidate outcomes. A baseline/host integrity incident
invalidates trust in the affected round; it is not an invitation to silently
replace the control or selectively rerun losing submissions.

The present per-candidate test is one-sided Welch at alpha 0.05, not a guarantee
of a 5% season-wide false-winner rate with unlimited submissions. Preserve the
agreed scorer in this change and describe that limitation honestly. Any
multiple-comparison correction or new confirmatory evaluation rule is a separate,
prospectively approved scoring-policy change, not an automation implementation
detail. See [the significance implementation](score_significance.go#L39).

### No winner

Append an explicit no-winner reward decision when there are no qualifying
entries, or when retrospective decisions exhaust them. Distinguish no
submissions, no gate-passing candidate, no significant improvement, and all
eligible candidates disqualified. An untrustworthy evaluation is a separate
`invalidated` incident outcome, not ordinary no improvement.

Publish the sealed results and reason; choose reviewed-source carry-forward;
continue the season if readiness is safe. A null reward recipient does not
implicitly mean all emissions burn, an earlier champion keeps earning, or fees
are refunded. Macrocosmos must specify that settlement behavior.

## Automatic next epochs without unsafe source

Introduce a small durable supervisor/reconciler rather than keeping an
interactive shell or reviewer agent alive as the scheduler. Preserve the
one-shot worker and singleton evaluator slot. The supervisor starts or resumes
the appropriate worker, watches durable state, seals/elects idempotently, chooses
the next source, prepares readiness, and creates at most one next round.

For production epoch N:

1. Wait for N's seven-day window and full FIFO drain, then seal/elect N.
2. Set a published, policy-frozen source-choice deadline relative to the seal.
   The deadline is a bounded opportunity to include an already-progressing
   review/build, not a review SLA disguised as another mandatory pause. A zero
   extra review wait is the recommended default; a short grace interval requires
   explicit approval.
3. At the deadline, choose a fully reviewed, authenticated, build-ready source
   transition if available and applicable to the exact current incumbent.
   Otherwise record `reviewed_source_carry_forward` with the actual reason
   (`review_pending`, `promotion_not_ready`, `no_reward_winner`, etc.).
4. Freeze N+1's exact eleven-repository source lock, threshold provenance, workload
   commitment, evaluator identity, and schedule before accepting submissions.
   Reserve the existing 16-hour preparation/readiness interval unless a separate
   policy change is approved. Do not advertise an admission time that the
   readiness process cannot honor.
5. Verify the new round's required production qualification, source/config
   activation, containment, archive availability, and worker identity before
   admission. A fallback source does not waive these checks. Generate a fresh
   scoring control for the new round through the existing trusted R=9 path;
   if no submissions arrive, no scored baseline need be manufactured.
6. Open N+1 for a full seven days only after the prior grading and source-choice
   boundary. Once its policy is committed, a late approval cannot change it.
   After epoch 6, stop automatic creation; retain review and settlement services
   until their remaining obligations finish.

If the chosen build or readiness fails, first attempt a safe, not-yet-committed
carry-forward preparation when that path is available. Otherwise enter a visible
`preparation_blocked` state, alert, and retry the same transition. Do not open an
unqualified round, silently change a committed round's schedule, or accept paid
work against an unresolved source identity. An exceptional cancellation or
replacement round requires the published incident policy and operator authority.

Unlimited admission means the post-close queue can be arbitrarily large. Six
seven-day windows do not imply a six-week season or a fixed weekly winner date.
Automation removes review/round-start administration, not physical evaluation
time. The three-hour per-job bound does not bound total drain. Publish queue
depth and honest estimated drain ranges, and keep retention, season-end
configuration, credentials, and settlement coverage valid through actual drain
and appeals. Do not introduce an admission cap or another evaluator as part of
this plan.

### Source identity and threshold provenance

Version the source ledger so round identity and source generation are explicit
and no longer rely solely on `round.epoch − 1`. A round can reuse a source
generation while retaining a fresh round/seed/baseline identity. Record the
chosen source decision, actual commit map, source-lock digest, originating
review/evaluation, threshold and its evidence, and activation receipts.

There can be a reward winner and no source promotion. Do not call the existing
`promote --no-winner` in that case: its meaning and SQL interlock require an
actual no-winner round. Introduce a distinct source-only carry-forward transition
whose reward-decision reference is provenance, not proof of source approval.
Keep historical schema-1 ledger entries unchanged.

The scorer's next-margin recommendation models the winning candidate's variance
as the next incumbent's variance. It is applicable only when that exact evaluated
implementation becomes the incumbent. Therefore:

- Promoting the exact approved evaluated patch uses its authenticated supported
  recommendation, without weakening the prior calibrated floor.
- Carrying forward source carries forward that source's approved threshold.
  Do not raise or lower it using an unshipped reward winner's variance. The next
  round still measures a new baseline and applies the frozen significance rule.
- A deferred approval may be applied at a later boundary only if its evaluated
  base, patch, dependencies, and protected runner identity still match. If the
  incumbent changed, do not silently rebase/combine patches or transplant their
  old significance. Require new review and appropriately bound measurement
  under an explicitly authorized process.

This is a source-policy choice, not a data field to fill with the latest winner.

### Repeated rewards while source is unchanged

Per-round caching does not prevent the same patch beating the same incumbent
again in a later epoch. Cosmetic variants are also possible. Publish an explicit
policy before enabling carry-forward automation:

- Allow repeat victories and specify whether they extend one entitlement or
  create another payout; or
- Exclude already-rewarded implementations against that same source identity
  from new reward entitlements, while retaining their honest evaluation results.

Recommendation: prevent duplicate new entitlements for the exact
`(base_source_lock, canonical_patch)` identity and publish a narrow evidence-based
anti-evasion rule. Semantic equivalence is not reliably solved by patch hashing;
do not pretend it is, or introduce opaque heuristic disqualification. This
recommendation requires UR/Macrocosmos approval and advance participant notice.
Tell participants about known reward ineligibility before charging/admitting;
do not change the settled fee amount or retroactively reinterpret past entries.

## Reviewed source promotion remains a separate capability

Review the authenticated canonical patch, exact base tree, full code behavior,
score and gate evidence, and relevant tests. A reward approval does not itself
authorize a merge; an integrity-approved patch can remain unshipped because it
needs maintainability work or additional product testing. Conversely, declining
to ship an otherwise honest optimization is not automatically reward fraud.

Retain the existing promotion invariants: private temporary clones of all eleven
locked repositories; only the evaluated allowed server patch; unchanged runner
tree and dependencies; verified base and remote refs; independently passed
tests; and config activation last. Product `main` shipping is a separate UR
release decision, never a side effect of an Apex notification. Known rejected
or malicious code must never become the measured incumbent or run in main.
Any necessary reproduction stays in a separately contained environment without
main credentials or source-push authority.

Make promotion a resumable transaction/saga with a durable intent containing
expected old refs, proposed new refs, review decision and source-lock digests.
Use compare-and-swap/fast-forward checks. Source pushes that succeeded before a
config failure are staged work, not an active incumbent. A retry verifies those
same immutable refs instead of generating a different commit or overwriting a
branch. Recovery and carry-forward must use pinned commits, never "whatever is
currently at the remote head." No forced pushes or history rewrites.

Make config propagation automatic as well. Recommended v2 authority: Git holds
the immutable authenticated manifest content; one compare-and-swap database
activation pointer selects its exact commit/digest for runtime use. Git branch
heads and local YAML files are not competing runtime selectors. Publish all
source commits first and the manifest/config commit last, automatically propagate
that exact generation through the config/release loader, and require API and
worker readiness acknowledgments for it. Only then activate the pointer and
allow round creation to transactionally snapshot that exact manifest. Existing
rounds retain their frozen snapshots. The deployment/config-updater integration
must reconcile this process unattended and resume partial failures; a human
pull, config push, or service rollout per epoch would leave the proposal's main
delay unresolved. Scope the automated source publisher to preauthorized
carry-forwards or genuinely approved promotions, not arbitrary source changes.

If a promoted source is later found unsafe, freeze new source activation and
affected admission, quarantine the host if containment is in doubt, and examine
the complete descendant source chain. An already committed descendant round
cannot switch to a clean source in place. Mark the incident append-only and use
an explicitly authorized cancel/invalidate/re-round procedure with fresh
identities and a participant/fee remedy. Never edit old baselines, scores,
source locks, or rankings to make a rejection appear not to have happened.

## Durable state and database design

Prefer additive, versioned records rather than making the existing finalized
winner mutable. Proposed logical records (names may change in implementation):

| Record | Required content and invariant |
|---|---|
| Round automation policy | Frozen decision mode/version, election rule, review-criteria version, source-choice policy, settlement/burn policy version, and API compatibility requirements. Existing rounds remain legacy mode. |
| Evaluation seal | One immutable row per round: sealed/publication times, job inventory and ranking manifest digests, baseline and frozen-policy identities, terminal counts, and reason. No seal with active work; no admission or new claim after seal. |
| Reward decision | Append-only `(round_id, revision)`, previous-decision digest, action, selected job or null, superseded job/revision, selection/reason codes, review provenance, decision time, effective settlement anchor, and policy version. Revision 1 is automatic; later revisions never edit scores or earlier decisions. |
| Integrity review | Versioned findings for exact job/patch/base/evidence, actor identity, criteria version, public reason, private evidence digest, appeal links, and supersession. Separate reward eligibility from source suitability. |
| Source decision/activation | Exact source generation and commit map, approved patch/review if any, carry-forward reason, threshold provenance, target round, old/new config identities, and activation receipts. Reward identity is not authorization. |
| Integration outbox/inbox | Durable event id, aggregate revision, payload digest, retries/acknowledgments, Apex transaction or accounting receipt, and reconciliation status. Acknowledgment is not proof of irrevocable chain settlement. |

Keep original score artifacts, job outcomes, baseline rows, and historical
`competition_round.winner_job_id`/`finalized_at` meanings unchanged. Recommended
first implementation: use new seal/election tables for v2-mode rounds and leave
legacy finalization/review tables as the v1 path. Do not manufacture a legacy
approval/finalization just to satisfy `CreateRound` or `RequirePromotionDecision`.
The v2 scheduler explicitly gates on the seal plus the chosen source/readiness
record; v2 source authorization explicitly gates on real source approval.

This is not only a new table: update admission, claim, completion, worker
termination, round-status projection, publication, cancellation, source selection,
and the new-round predecessor check to understand the frozen mode. Database
constraints must prevent late jobs, baseline insertion, or mutable terminal
evidence after a v2 seal even though legacy `finalized_at` is not its signal.
Fence old workers and legacy round-control commands from v2-mode rounds rather
than allowing them to operate under incomplete assumptions.

Serialize each epoch decision and source transition with row/advisory locks,
expected revision, unique keys, and consistent lock ordering. Recheck close,
drain, terminal evidence, and current disqualifications inside the transaction.
Database retries may rerun callbacks: reset transient results and keep network
calls, Git pushes, payout calls, and other side effects out of transaction bodies.
The decision and its outbox event commit atomically. Delivery is at least once;
idempotent downstream application provides one accounting effect.

Existing append-only migration history, trigger protections, and legacy ranking
discriminators stay intact. Append new migrations; never edit an applied
migration or reclassify a historical round by inserting a late baseline.

## API, OpenAPI, and Apex integration

Use an explicit v2 decision contract/capability, not a silent new interpretation
of `winner`, `honesty_review`, or `finalized`. Keep score schema/version unchanged
if statistical semantics and scorer bytes are unchanged; reward protocol
versioning is a different concern.

Proposed API surface for review during implementation:

- Versioned epoch result and decision-history reads, for example
  `GET /competition/v2/rounds/{roundId}/result` and `/decisions`, returning the
  immutable ranking, current reward revision and full history, review state,
  source decision, publication identity, and settlement acknowledgment state.
- Versioned score/status URLs for v2 rounds, preserving authenticated job
  access and the pre-publication `evaluation_status`/safe typed-failure contract.
  Return the correct immutable URL at admission; clients must not guess paths.
- A bounded, authenticated review-decision write accepting expected revision,
  exact candidate identity, reason codes, criteria/evidence digests, and an
  idempotency key. The server computes the replacement from the sealed ranking;
  an optional client-supplied expected replacement is only a consistency check.
- A durable, cursor-based reward-event feed and/or signed outbox delivery, plus
  idempotent receipts. Push notifications need polling reconciliation because
  notifications can be dropped, duplicated, delayed, or delivered out of order.

Expose `provisional_reward_winner`, `reward_revision`, `integrity_review`,
`source_approval`, and `settlement_status` as distinct concepts. Do not return
`honesty_review: approved` for an unreviewed candidate or let a paid status imply
safe source. Approval is a no-op for the reward recipient if it is already the
current winner, but it still creates an auditable review event and can make a
separate source approval possible.

For v1 rounds, preserve existing endpoint results, staging behavior, winner
immutability, and historical ordering. For v2 rounds, legacy consumers must
either explicitly upgrade or receive a clear capability/unsupported-mode
response; do not give them a misleading "finalized approved winner" projection.
Additive discovery fields can advertise the mode and required contract before
submission. Define this mixed-mode behavior in OpenAPI and conformance fixtures
before implementation; avoid a rolling deployment in which different replicas
publish incompatible winner meanings for the same round.

Update `model/competition_model.go`, controller projections/adapter validation,
API routes/handlers, `sn/api/competition.yml`, onboarding examples, and the Apex
reference adapter together. The adapter must persist election revisions and
event receipts, reject conflicting reuse of an event/revision id, tolerate safe
replays, and recover missed revisions. Its current production honesty gate must
remain for v1 and be deliberately replaced by the v2 provisional-election rule
only for an explicitly enabled v2 competition. Apex's submitter token gains no
review, source, or evaluator privileges.

The existing adapter has no demonstrated revoke/replace/burn endpoint. Agree
the runnable integration owner, actual Apex interface, release identities, and
settlement receipts; prove them in stage. A chat message saying "tell us the
replacement" is an emergency procedure, not an automated protocol.

## Retrospective rejection, money, and burn timing

For an authenticated disqualification, append a review finding and the next
reward revision atomically. Choose the next eligible non-disqualified entry
from the original sealed ranking, even when that entry has not yet had its own
review. Mark it provisional and enqueue review. If it later fails, repeat;
exhaustion produces an explicit no-winner revision. A replacement does not get
a new evaluation, a different baseline, or a fresh favorable noise draw.

Separate the following events in both documentation and accounting:

1. UR records the authoritative reward decision.
2. Apex acknowledges receipt/application of that revision.
3. Future emissions change at a specified effective boundary.
4. Any compensating payment, recovery, escrow release, or chain transaction is
   separately recorded and eventually confirmed.

Already transferred rewards may be irreversible. A database "revocation" cannot
promise a clawback. Before automatic payouts, Macrocosmos must state whether
rejection stops only future rewards, reverses held funds, permits actual
recovery, creates an offset against future earnings, or uses a funded
compensation mechanism. Do not send duplicate replacement payments or negative
balances by assumption. Holdback/escrow is an alternative if supported and
approved, but is not silently part of Macrocosmos's pay-first proposal.

"Recalculate burn from the replacement's evaluation time" is not precise
enough to implement. Store and distinguish `submitted_at`, first `started_at`,
successful attempt finish, terminal accepted `completed_at`, seal/publication,
decision, Apex application, and relevant chain block times. Current
`completed_at` is the server terminal-result commit after authenticated archival,
not necessarily the last measured sample or first attempt's finish.

Recommendation: if the intended anchor is completion, use the replacement's
original authenticated terminal-success `completed_at`, never review time or
notification time, and state its archival overhead. Do not infer an anchor from
`submitted_at` or `completed_at − started_at`; retries and the first job's
baseline work make those different quantities. A different trusted finish time
requires an explicit versioned timestamp contract, not retroactive inference.

UR/Macrocosmos must approve a worked accounting example covering both cases:
the replacement finished before the original winner, and it finished later.
Specify rounding, clock/block mapping, start/end inclusivity, queue wait,
infrastructure retry time, preparation time, overlapping historical reward
intervals, double-count prevention, no-winner burn, and responsibility for funds
already paid. An evaluation-time anchor can penalize a later FIFO position for
queue delay; whether that is intended fairness policy cannot be decided by the
controller. Record any backdated entitlement calculation separately from the
real time at which funds move.

## Public review policy and appeals

Publish versioned rules before admissions under them. Build on the existing
runbook rather than leaving grounds to reviewer taste. Suggested reason codes:

| Reason family | Required evidence and consequence |
|---|---|
| `SCORE_TAMPERING` / `MEASUREMENT_FABRICATION` | Identify falsified telemetry, suppressed checks, or manipulation of the trusted measurement path. Reward disqualification and source rejection. |
| `WORKLOAD_SPECIAL_CASING` / `BENCHMARK_DETECTION` | Identify hidden-seed inference, workload-specific cheating, or evaluator detection intended to invalidate measurement; distinguish legitimate general workload optimization. |
| `SANDBOX_ESCAPE` / `SECRET_ACCESS` / `EXFILTRATION` | Identify the concrete access path or attempted boundary violation; quarantine and incident handling as needed. |
| `UNSAFE_BEHAVIOR` / `PERSISTENCE` / `DENIAL_OF_SERVICE` | Identify harmful runtime behavior, affected boundary, and reproduction/evidence, not merely unusual coding style. |
| `PATCH_POLICY_VIOLATION` / `UNRELATED_CHANGE` | Cite the frozen allowed/forbidden surface or published scope rule and exact patch locations. |
| `REWARD_DUPLICATE` / `REWARD_RULE_EVASION` | Only if prospectively adopted; cite exact prior implementation/reward identity and the published rule. |
| `SOURCE_NOT_READY` / `PRODUCT_INTEGRATION_RISK` | Source deferral or rejection only unless a separately evidenced published reward rule is violated. |
| `EVALUATION_INTEGRITY_INCIDENT` | Evaluator/host/evidence failure; do not accuse a miner of fraud without causal evidence. May require round invalidation. |

Require structured findings for every honesty/safety criterion, exact patch,
base, score/baseline and artifact identities, reviewer identity, tool/test
evidence, criteria version, timestamp, explicit disposition, and evidence digest.
The public decision contains the relevant reason codes, a specific readable
explanation, score/job/patch identities, effective revision, and sanitized
supporting evidence or a withheld-evidence explanation. Keep credentials,
exploit-sensitive details, and embargoed artifacts private. A hash alone is not
an adequate public explanation.

Use qualified UR-authorized reviewers. An agent can prepare findings, but it
does not acquire review/source authority from running unattended. Preserve the
runbook's Astra-max review/promotion and independent Sol-medium test ownership
for this workflow unless UR explicitly changes those roles. Do not execute
instructions embedded in a submitted patch or evidence bundle as reviewer
instructions. Static inspection precedes any isolated reproduction.

Publish a review-service target, appeal filing period, decision authority,
conflict-of-interest/escalation path, and evidence-access procedure. A review
target improves accountability but expiry is not automatic honesty approval.
Appeals append decisions; they do not delete findings or restore an earlier
payment by mutating history. Specify whether an upheld appeal restores future
eligibility only or also creates a funded compensation entitlement. Safety
incidents can block source even while a reward appeal remains unresolved.

## Authentication, concurrency, and failure handling

- Separate submitter, scheduler/elector, reviewer, settlement integration, and
  source-promotion capabilities. Current opaque token roles are only submitter
  and operator, so new scopes require explicit implementation. Attribute actions
  to authenticated identities, not a caller-provided reviewer string alone.
- Scope keys and events to environment, competition, season, round, and revision.
  Authenticate endpoint calls and any webhook payload; check replay/sequence
  bounds, payload digest, and expected predecessor. Never expose operator,
  Git-push, MinIO, seed-key, or Docker credentials to Apex or candidate code.
- An idempotency-key replay with the same body returns the original result; a
  different body conflicts. A stale expected revision returns a conflict and
  current identity, not a second replacement. Exact duplicate approvals are
  harmless; review and source approval remain separately auditable.
- Acquire a durable fenced supervisor lease and per-transition lock; reconcile
  desired state after restart. Two schedulers cannot seal/elect twice, create
  epoch N+1 twice, send duplicate accounting effects, or push competing source
  activations. Keep the current singleton evaluator lease and reserve intact.
- Election racing an admission/archival completion must serialize on the round.
  Define the boundary for in-flight requests using the existing admission
  contract; a request not durably admitted cannot appear after the seal.
- Review rejection racing approval/source activation must be resolved against
  the same current decision and source revision. Recheck approval immediately
  before activation; if safety changes after activation, enter incident handling
  rather than pretending the activation never occurred.
- An unavailable Apex endpoint leaves a durable pending outbox and visible
  settlement lag. It does not cause a new election or hidden re-payment. An
  untrustworthy DB/archive/host stops the affected evaluation/publication path.
- Provide separately authorized pauses for admission, source activation,
  evaluation, and reward delivery. Pausing one is not permission to discard
  accepted work, reveal secrets, or rewrite historical results.

## Verification and staging proof

Add deterministic tests before live activation, with independent execution
ownership from the existing runbook. Do not rely on real noisy staging winners
to prove every state transition.

1. Election fixtures: passing significant candidate, failed/missing gates,
   margin boundary, unsupported recommendation, equal raw scores, exact time/id
   ties, no submissions, all failures, no significant candidate, and all
   disqualified. Controller/SQL/adapter choose the same job and ranks.
2. Statistical identity: one persisted R=9 baseline, later candidate reuse,
   conflicting baseline rejection, same-round patch cache, fresh control for a
   carried source in a new round, and unchanged historical normalized rankings.
   Preserve the existing significance/scorer regression suite.
3. Publication: no score/seed/ranking/winner leak before close plus drain plus
   reveal; typed outcome-neutral polling remains compatible. A slow queued job
   prevents sealing. Neither private review nor a settlement event bypasses
   the embargo.
4. Revision history: approve after payment produces no second entitlement;
   reject once, multiple replacements, no-winner exhaustion, appeal reversal,
   duplicate/out-of-order events, conflicting idempotency keys, and replay after
   restart. Scores, original winner history, timestamps, and ranking stay intact.
5. Concurrency/crash tests: two supervisors, reviewer races, close racing last
   admission, seal racing completion, DB transaction retry, crash after decision
   before send, send before lost acknowledgment, and source pushes before config
   activation. Verify one next epoch, one active source decision, and one effect
   per acknowledged settlement revision.
6. Source safety: review never finishes but N+1 still carries forward safely;
   approval misses deadline; approval targets obsolete base; failed build;
   source/ref drift; partial push recovery; readiness failure; rejected source
   and contaminated descendants. No auto-promotion of an unreviewed patch and
   no source/threshold change within an open round.
7. Protocol/auth: v1 legacy production approval still required, v2 provisional
   winner explicit, OpenAPI/model consistency, correct job ownership, scope
   denial, revoked credentials, tampered event signatures, seed/evidence
   redaction, and unsupported-mode behavior during rolling deployment.
8. Staging: preserve current fee-free, zero-margin, best-placeable named winner,
   `honesty_review: not_reviewed`, optional genuine manual approval, and no source
   promotion. Exercise v2 election/reversal/accounting in a clearly labeled
   shadow/test mode with no production payment authority. Use production-like
   deterministic fixtures for significance; staging's weaker rule does not prove
   production eligibility.
9. End-to-end Apex proof: delayed review, auto safe next round, rejection with
   the computed next candidate, both burn-time orderings, duplicated delivery,
   acknowledged no-winner, and reconciliation after an outage. Capture exact
   round/job/patch, event revision, release digests, and settlement receipts.

Relevant existing suites include `competition_ranking_test.go`,
`competition_staging_winner_test.go`, controller/store retry and archive tests,
`db_migrations_test.go`, `epoch_review_test.go`, `source_epoch_test.go`,
`source_repository_test.go`, run-main lifecycle tests, and scorer significance
tests. Add new tests to these boundaries rather than bypassing them in a separate
happy-path script. No tests or live operations are executed by this document.

## Implementation packages and release order

Implement this in reviewable increments, with automation disabled by default:

1. **Policy/protocol agreement:** settle the explicit approvals below, publish
   participant rules, specify v2 schemas/events and mixed-mode behavior, and
   assign the UR/Apex integration and accounting owners.
2. **Pure domain/state work:** add election/seal/revision/source types and
   deterministic selectors, fake-clock state-machine tests, and the structured
   review schema. Do not modify evaluator/scorer behavior for scheduling work.
3. **Persistence:** append migrations, DB guards, new mode-aware transitions,
   outbox/inbox, source activation intents, and migration upgrade tests. Preserve
   all existing historical rows and applied migration bodies.
4. **API and adapter:** implement versioned reads/writes and scoped auth; update
   OpenAPI, model/controller/API conformance, durable adapter revisions, and
   release-discovery/capability checks. V1 remains its current contract.
5. **Supervisor and source tools:** replace the mandatory interactive progression
   dependency with durable reconciliation and explicit reviewed carry-forward;
   version source-ledger mapping and recovery. Keep private clones and protected
   source checks. Update `run-main.sh`, the review CLI, and runbooks without
   turning them into an alternate unguarded control path.
6. **Evidence and operational proof:** run deterministic suites, shadow decisions
   against closed staging data without rewriting it, and then new fee-free stage
   rounds with the actual Apex integration. Retain independent results.

Deployment order, after explicit approval:

1. Audit live schema, active rounds/jobs, source/config locks, consumer versions,
   and rollback/recovery readiness. Finish or drain the current staging boundary;
   never convert an open round to v2.
2. Apply additive database migrations through the reviewed migration path. Verify
   guards and upgrade compatibility; do not begin a v2 round yet.
3. Deploy backward-compatible API/model/OpenAPI, adapter receiver, and mode-aware
   worker/supervisor binaries with v2 automation off. Confirm all API replicas
   agree, old workers cannot claim v2 work, and the scheduler has a fenced lease.
4. Prove new staging/shadow rounds, outbox recovery, source carry-forward, and the
   real replacement/burn integration. Record release identities and owner signoff.
5. Install the approved future-round policy, capabilities, source ledger, and
   settlement configuration. Complete exact-image production readiness and the
   publication/participant notice before activating the first v2 production
   epoch. The first production round still follows the existing safe staging-era
   shutdown rules and cannot race running staging evaluation.
6. Monitor the first seal/reward revision/next-round transition with staffed
   on-call. Rollback disables new automated transitions and uses a compatible
   reader/reconciler; it does not drop tables, revert a live policy, or erase
   decisions already consumed by Apex.

The upcoming main freeze is a deadline for a separately authorized implementation
and reviewed release, not for accepting incomplete economic policy or skipping
staging. If the protocol/proofs are not ready, retain existing manual production
behavior or postpone v2 activation. Control-plane-only changes need not rebuild
the frozen scorer; any change that reaches the measured source graph, source-lock
reader, evaluator protocol, or trusted image must be classified and separately
released/qualified at a safe boundary. Preserve all prior images and evidence.

## Operations and ownership

UR owns the scorer/evidence contract, evaluator safety, authoritative election
and review records, source approvals, and source activation. Macrocosmos/Apex
owns actual emission/payment/burn execution and confirmation. Name one joint
integration owner for delivery/reconciliation incidents; neither party should
infer settlement success from a sent message. Named UR reviewers and an appeal
authority own findings and reversals. The current incident contact remains a
starting point; confirm a staffed roster before unattended launch.

Expose and alert on queue depth/oldest age, drain estimate, evaluator/supervisor
heartbeats, active leases, sealed-but-unpublished rounds, review age, pending
source carry-forwards, preparation failures, outbox age/retry count, missing or
conflicting Apex acknowledgments, decision-revision gaps, source/config drift,
and evidence retention/replication. Avoid hidden candidate identities or scores
in pre-publication metrics. The public view should explain "grading backlog,"
"review pending; next epoch uses approved source," and "settlement pending"
without conflating them.

Reconcile UR decision history against Apex receipts regularly and after any
restart. Keep a tested manual emergency procedure that uses the same authorized,
idempotent, append-only transitions. Manual SQL winner edits, ad hoc payout
messages, and source-branch repairs are not the recovery plan.

## Explicit approvals needed before implementation activation

These are remaining policy choices, not a request to renegotiate settled fees,
hardware, or the approved external-evaluator architecture:

1. Does UR accept automatic provisional reward election plus asynchronous review,
   and may the next epoch use the last approved source instead of the latest
   reward winner? Approve the source-choice deadline/default and carry-forward
   wording.
2. What exactly happens to rewards already transferred after rejection, who funds
   replacement/appeal compensation, and what happens when no winner remains?
   Confirm Apex can implement the agreed actions and provide receipts.
3. What exact timestamp and interval formula define replacement burn, including
   FIFO waiting, retries, archive latency, overlaps, and no-winner intervals?
   Approve concrete examples before encoding a formula.
4. May an already-rewarded implementation receive a new entitlement while the
   incumbent is unchanged? Approve an objective repeat/anti-evasion rule and how
   participants learn ineligibility before admission.
5. Who can issue integrity disqualifications, source approvals, incident holds,
   and appeal decisions; what are the public criteria, review target, and appeal
   period? Which exceptional source-compromise cases authorize cancellation and
   replacement rounds, and what is the participant remedy?
6. Who owns the production v2 Apex adapter/event receiver, compatibility rollout,
   registry/release activation, and settlement monitoring? Confirm protocol
   acceptance rather than assuming the supplied proposal is already operational.

Completion means a closed-and-drained epoch can publish a statistically valid
provisional reward winner, continue safely with reviewed source without waiting
for review, and later replace that reward decision with authenticated evidence
and auditable accounting—while all historical measurements remain immutable.
