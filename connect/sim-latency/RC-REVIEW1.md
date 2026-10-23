# Release-candidate review 1

## Executive summary

Recommendation: revise before paid-production publication. The measurement,
ranking, significance, patch-format and isolation descriptions are substantially
consistent with UR's implementation. No scoring redesign is indicated by this
review. The important corrections are around source progression, billing
idempotency, the execution deadline, and the local-development example.

Two P1 items need resolution before participants rely on this as a paid contract:

- Approval alone does not put a patch into the immediately following epoch.
  The implementation has an exact-base, build-readiness and source-choice
  deadline, followed by independent activation/readiness checks.
- Round-level evaluation deduplication does not establish the promised
  no-additional-fee behavior for a new Apex submission identity. The supplied
  reference adapter guarantees fee idempotency by admission identity instead.
  Macrocosmos may implement stronger deduplication, but that needs confirmation.

The public service currently advertises fee-free, 48-hour, zero-margin **v2
staging**, not an active paid-production epoch. That does not invalidate the
intended production rules; it does mean the page should identify its scope and
not present a staging check as proof of production activation or settlement.

The local example also has a reproducible static failure: after its first
successful run, unignored metadata/marker files make the repository dirty, and
the next measured run rejects that checkout. Further, the example is an
unmodified-baseline procedure, not a complete way to benchmark an edited patch.

Severity: P1 = resolve before paid-launch reliance; P2 = material accuracy,
reproducibility or participant-contract clarification; P3 = precision/editorial.
There are no P0 findings. Apex-owned assertions are not treated as UR API bugs.

## Scope, evidence and accessibility

Reviewed on 2026-10-23 UTC, independently, without reading another review.
`connect/CODESTYLE.md` was read; no applicable ancestor `AGENTS.md` was present.
Only this review file was created. No submissions, authenticated control writes,
migrations, evaluator runs, repository updates or commits were performed.

Document D is the exact [change-667 draft](https://docs.macrocosmos.ai/~/changes/667/subnets/subnet-1-apex/subnet-1-current-competitions/urnetwork-latency-competition),
retrieved from its [Markdown representation](https://docs.macrocosmos.ai/~/changes/667/subnets/subnet-1-apex/subnet-1-current-competitions/urnetwork-latency-competition.md)
with HTTP 200. Its bytes had SHA-256
`17450afaa0b9b7e4f76761e9937b2b399ba61cd4d198544a3347eeb48e5f8844`.
`D:Lx` below refers to `nl -ba` numbering of those exact Markdown bytes,
including GitBook's initial index notice. The browser tool could not render
the draft, but direct HTTPS retrieval worked; the published page was not
silently substituted. Short excerpts and precise locations identify issues
without reproducing the page.

Implementation references are relative to the mono-repository root,
`/home/by/urnetwork`, not the `server` repository root. Reviewed checkout heads:

| Repository | HEAD |
| --- | --- |
| server | `f2a9cf7af613d1a9c3ff6e29ec2a0690e9ff55e4` |
| sn | `0f30cb8d11456158c3e59dd09f382f7c4124f4d8` |
| config | `14229f9ef5a81aca3d5992a1d5ffdc8fe793b267` |
| connect | `0333da8462984c4262d1c116d45faa42f13d60de` |

Source precedence: the advertised round's frozen v2 policy and public
`sn/api/competition.yml`, checked against executing controller/model paths,
take precedence over legacy runbook passages. In particular,
`server/connect/sim-latency/FINAL-AUTOMATE.md:3` explicitly identifies its
repository-state comparisons as pre-implementation; those historical tables
are not evidence that v2 still requires review before publication.

Read-only live checks, repeated at 05:23 UTC:

- [Public discovery](https://api.bringyour.com/competition/info) returned HTTP
  200 with no `active_round`, an open `staging_round` at epoch 2, decision mode
  `provisional_reward_v2` and capability
  `competition.reward.v2.runtime-blob.v1`.
- It advertised a 172800-second admission window, fee 0, takeover margin 0,
  and the documented 1800/200/80 workload, 180-second measurement, nine
  replicates and 10800-second execution budget.
- The current round's public v2 result returned HTTP 409
  `round_not_published`, consistent with the embargo. No private results or
  credentials were requested.

These observations establish discovery/embargo behavior at that time, not
production qualification, Apex registration, payout delivery or host inspection.

## Accuracy findings, by severity

### P1-1 — Source approval is necessary, not sufficient, for the next epoch

Document: D:L95-98 and D:L135-141. In particular, the description concludes
that “each epoch is measured against the previous winner's code”.

Evidence:

- `server/controller/competition_automation_store.go:159` freezes
  `source_choice_at` relative to sealing, independently of review completion.
- `server/connect/sim-latency/source_reviewed.go:113` selects only proposals
  reviewed and build-ready by that deadline and applicable to the exact active
  source lock; an obsolete base is not implicitly rebased.
- `server/controller/competition_automation_source_store.go:370` rejects a
  promotion reviewed or built after the frozen deadline.
- `server/controller/competition_automation_source_store.go:431` requires
  exact API and worker readiness receipts before activation.
- `server/controller/competition_supervisor.go:510` and
  `server/controller/competition_controller.go:4319` enforce the production
  preparation interval; `server/connect/sim-latency/RUN-MAIN.md:322` explicitly
  distinguishes build readiness from the subsequent 16-hour preparation.

Impact: a genuine winner approved after source choice can miss the next epoch.
Even an approval before that boundary is insufficient if its build is not ready.
An already committed/open round cannot be updated by a late review. Participants
must not assume a moving base or calculate a new threshold from a reward winner
whose implementation has not actually become the incumbent.

Suggested replacement:

> Each epoch uses a separately frozen, approved incumbent. A patch can enter a
> future epoch only after exact-source approval, independent build readiness by
> that epoch's source-choice deadline, and successful runtime qualification and
> activation. Otherwise the incumbent source and threshold carry forward, even
> when a reward winner exists. A late approval cannot change an already frozen
> round and can be considered at a later compatible boundary.

Keep the statement that the margin does not weaken, but tie its provenance to
the exact promoted implementation, as enforced at
`server/controller/competition_automation_source_store.go:258`.

### P1-2 — Evaluation caching does not prove the fee promise

Document: D:L143, particularly “not charged again”.

Evidence:

- `server/controller/competition_controller.go:5441` keys an evaluation by
  round ID and exact patch bytes. This establishes reuse of a result.
- `server/controller/competition_controller.go:7061` finds an existing adapter
  record by `submission_id`, not by matching another record's patch digest.
- `server/controller/competition_controller.go:7899` calls
  `CollectOnce(..., submissionId, 20)` for a production record without a fee
  receipt, before the score submission at line 7929.
- `server/controller/competition_controller_test.go:4558` checks restart/retry
  idempotency for the same admission identity; it does not establish fee-free
  creation of a second identity containing the same bytes.

Impact: two different Apex admission identities can share one UR evaluation while the
reference adapter asks the fee collector to charge both identities. The UR API
does not itself collect/refund Apex fees. This is **not** proof that the deployed
Apex platform double-charges; the deployed platform was not inspected. It is
proof that the broader monetary guarantee does not follow from UR caching.

Suggested replacement pending Macrocosmos confirmation:

> Retrying the same accepted Apex admission does not collect another fee.
> Byte-identical patches within one epoch share a UR evaluation. Whether a new
> Apex admission of those bytes is rejected, free, charged or refunded is governed
> by the linked Apex fee policy.

If byte-based fee deduplication is the intended platform promise, retain the
stronger wording only after recording its scope, ownership, receipt/refund
behavior and end-to-end evidence. In particular, distinguish retries from a
new submission and specify whether deduplication is per hotkey or global.

### P2-1 — The three-hour limit excludes initial queue wait and publication wait

Document: D:L26, “3 hours end to end”.

Evidence: `server/controller/competition_controller.go:6022` explicitly excludes
queue wait before the first claim, includes later attempts/backoff, and computes
the deadline from `started_at`. `server/controller/competition_controller.go:6756`
uses that original start to set the evaluation deadline. Publication separately
waits for round close, complete drain, seal and reveal at
`server/controller/competition_automation_store.go:143`.

Impact: this is not a three-hour submission-to-result SLA. A queued job can wait
much longer, and a completed evaluation can stay private until publication.

Suggested replacement:

> Execution budget: three hours from the job's first evaluator claim, covering
> all attempts, retry backoff, build, measurement, scoring and cleanup. Initial
> queue wait and the wait for epoch publication are additional and unbounded by
> this per-job budget.

### P2-2 — Round preparation and the shared scoring control are different

Document: D:L92, compared with D:L35.

Evidence: `server/controller/competition_supervisor.go:496` prepares/activates
source before creating the round at line 535. The scoring control is measured
inside the first admitted job's evaluator attempt when no baseline is supplied:
`server/connect/sim-latency/evaluator/container/evaluator.sh:1544` and line 1577.
Candidate construction is deliberately delayed until after this control, at
line 1458. The returned baseline is durably authenticated/frozen by
`server/controller/competition_controller.go:6106`.

Impact: next-round readiness is not proof that its nine-run scoring denominator
already exists. An empty round need not have a scoring control at all. Do not
confuse host qualification/rebaseline preparation with the shared sample used
to score candidate submissions.

Suggested replacement:

> After the preceding round drains, the supervisor selects and qualifies the
> next frozen source and opens admission when readiness passes. The first
> admitted evaluation measures and freezes that round's nine-run scoring control
> before building its candidate; later evaluations reuse it.

### P2-3 — Winner-only source promotion is stricter than the implemented v2 rule

Document: D:L96-98 and D:L137.

Evidence: `server/controller/competition_supervisor.go:36` authenticates an
eligible sealed entry; it does not require that entry to be the current reward
recipient. `server/controller/competition_automation_source_store.go:201`
checks sealed eligibility, and line 213 checks exact-base source approval.
`server/connect/sim-latency/source_reviewed.go:139` orders ready applicable
proposals by raw score, original admission time and job ID, without filtering
them to the immediately preceding reward winner. The database guard likewise
requires current genuine source approval, not reward-recipient identity:
`server/competition_automation_staging_source_graph_migration.go:113`.

Impact: independent source choice can select a ready, reviewed eligible
candidate, including one from an earlier compatible evaluation. A current
no-winner outcome alone is not the selector's universal proof that no reviewed
source change can occur. This distinction matters when review lags reward
election or a later round carries forward the same source.

Suggested wording matching implementation:

> Source selection is independent of reward election. At each boundary, UR may
> select a genuinely approved, build-ready eligible patch applicable to the exact
> incumbent; otherwise it carries forward that incumbent and its threshold.

If UR intends a narrower, winner-only publication policy, record that explicit
policy and its enforcement before promising it. Do not silently change either
the competition policy or implementation as part of this documentation review.

### P2-4 — The local example dirties its own checkout and then cannot rerun

Document: D:L160-173.

Evidence:

- `server/connect/sim-latency/main.go:326` performs source verification before
  a measured run.
- `server/connect/sim-latency/source_epoch.go:481` includes untracked files in
  the required clean-worktree check and rejects any dirty repository.
- `server/connect/sim-latency/run.go:162` derives a completion marker from the
  metadata filename; lines 322 and 333 write both artifacts.
- `server/connect/sim-latency/.gitignore:12` ignores `run.json`, not the
  example's `results.run.json` or `results.run.json.complete.json`.

Read-only confirmation: `git check-ignore --no-index --verbose --non-matching`
matched `providers.yml` and `results.csv`, but neither metadata path. The same
ignore rules are present at the currently advertised frozen server commit
`661759b163c9e4c473d63403734fdf4486bc25c5`. No measured run was started to
demonstrate a consequence already explicit in the source.

Suggested correction: keep all generated evidence outside every measured
repository, or in a pre-existing ignored scratch directory. For example, after
preparing the exact clean source graph, the second terminal can use:

```bash
# From the mono-repository root; these are baseline diagnostics, not a score.
cd server/connect/sim-latency
run_dir="$(mktemp -d)"
./run-local.sh init \
  --count 500 --clients 100 --rate 40 --quality-window 2 \
  --seed 1 --out "$run_dir/providers.yml"
./run-local.sh run \
  --epoch 0 --reset --providers "$run_dir/providers.yml" \
  --duration 180s --meta "$run_dir/results.run.json" > "$run_dir/results.csv"
./run-local.sh analyze --run "$run_dir/results.csv"
```

The explicit duration makes this a short diagnostic; without it,
`server/connect/sim-latency/main.go:357` defaults to a 30-minute measured window.
It does not become an official workload merely by using a 180-second window.

### P2-5 — Baseline startup instructions do not yet explain patch iteration

Document: D:L149-175.

Evidence: ordinary host execution requires every repository to be on the
configured branch at the configured commit with no edits:
`server/connect/sim-latency/source_epoch.go:478`. A server candidate commit is
accepted only through the evaluator-lock path and must have exactly the frozen
base as its parent at line 511. The submission builder provides that clean,
deterministic candidate representation:
`server/connect/sim-latency/evaluator/container/Dockerfile.submission:26`.

Impact: a miner who changes the permitted file and reruns the shown command is
correctly rejected by source preflight. The warning about a clean epoch checkout
is correct, but the section still lacks a complete candidate-testing route.
It also leaves the second terminal's initial working directory implicit.

Suggested addition:

> The startup example measures an unmodified configured baseline. An edited
> checkout does not pass the measured-source gate. To test a candidate, use the
> authenticated candidate build/source-lock workflow linked below; do not disable
> source verification. Prepare all repositories in the published source lock,
> not only the server checkout, and keep generated artifacts outside them.

Link to `server/connect/sim-latency/evaluator/container/README.md:165` for the
actual candidate build contract, but provide a reviewed, participant-safe
end-to-end recipe rather than directing miners to operator credentials. Make
the two terminal starting directories explicit. Do not execute an untrusted
candidate with production/local operator credentials.

### P2-6 — Label intended production rules and link the actual v2 contract

Document: D:L78-98 and D:L139-147.

Evidence: `config/main/competition.yml:25` configures explicit v2 staging,
48-hour admission, zero fee and shadow settlement; `server/model/competition_automation_model.go:172`
deliberately selects staging winners without production significance/margin
requirements. Production validation separately requires six seven-day windows
and a $20 fee at `server/controller/competition_controller.go:738`.
The read-only live discovery above matched the staging configuration.

Suggested banner:

> This page describes the planned paid-production season. Fee-free staging can
> advertise shorter admission windows and different winner eligibility. Use the
> selected round and its frozen policy from public discovery; staging is not a
> paid-production launch or payment confirmation.

Add a link to [the public API contract](https://github.com/urfoundation/sn/blob/main/api/competition.yml)
and a short integration note: discover `decision_mode` and the exact supported
`required_api_capability`, submit/poll through the advertised v2 contract, and
reconcile the versioned round result/history. The legacy
`GET /competition/leaderboard` does **not** contain v2 rounds:
`sn/api/competition.yml:38` and `server/connect/sim-latency/README.md:114`.
Do not infer legacy mode from environment names or a zero margin.

This is a scope/discovery gap, not a finding that the intended production fee,
16.1% opening margin or seven-day policy is wrong. Likewise the draft's generic
reference to a leaderboard may mean the Apex UI; it is not itself a false claim
that the legacy UR leaderboard endpoint contains v2 results.

### P2-7 — Complete the retrospective-review policy and preserve provisional status

Document: D:L102-121.

Evidence: the result explicitly separates `provisional_reward_winner`,
`integrity_review`, `source_approval` and `settlement_status`:
`server/model/competition_automation_model.go:453`. An appeal restoration
requires a superseded disqualification and an appeal reference at line 374.
Reviews only append a new reward revision when the selected recipient changes:
`server/controller/competition_automation_store.go:419`. An evaluation-integrity
incident is a different terminal outcome, with action `invalidated`, not routine
replacement: `server/controller/competition_automation_incident.go:274`.

The implementation/design contract also calls for participant-facing appeal
and review procedures before live activation:
`server/connect/sim-latency/AUTOMATION.md:113` and
`server/connect/sim-latency/FINAL-AUTOMATE.md:516`. The draft supplies substantive
grounds, but not those procedures.

Suggested addition:

> The published recipient is provisional. UR publishes versioned review decisions;
> an upheld appeal may restore a recipient using the original sealed ranking.
> Evaluator-integrity incidents can instead invalidate an evaluation and pause
> progression. Review approval, source activation and payment are separate
> states. The linked review policy identifies the filing channel, deadlines,
> decision authority, escalation process and any funded remedy.

Macrocosmos must separately confirm the Apex top-scorer update timing and
replacement handling. A UR decision/event or receipt is not proof of immediate
payment, successful clawback or a particular burn calculation; see
`sn/api/competition.yml:50` and line 61. Do not invent those economic rules in
the replacement paragraph.

### P3-1 — Calibration is not a season-wide statistical guarantee

Document: D:L81 and D:L95.

Evidence: `server/connect/sim-latency/score_significance.go:5` applies alpha 0.05
to each candidate comparison. Lines 74-91 use the actual control and candidate
variances; lines 108-117 compute the next-margin recommendation.
`sn/api/competition.yml:59` expressly states the per-candidate scope.

Suggested clarification:

> The opening 16.1% margin comes from calibration. Every production candidate
> must still meet its frozen median-improvement threshold and its run-level
> Welch test. Alpha 0.05 applies per comparison, not to all submissions or the
> entire season; calibration does not eliminate measurement noise.

No multiple-comparison correction or scorer change is proposed here. Any such
change would require separate prospective policy approval.

### P3-2 — Local performance portability is a prediction, not a contract

Document: D:L29.

Evidence: local defaults and the official workload are intentionally different
(`server/connect/sim-latency/main.go:353` versus
`config/main/competition.yml:85`), while the evaluator fixes a shared ten-core
boundary at `server/connect/sim-latency/evaluator/container/resource-boundary.sh:8`.
Neither code nor calibration establishes that every modern laptop is faster,
or that relative gains survive different provider counts and resource pressure.

Suggested replacement:

> Local measurements are useful for diagnostics. Both absolute latency and
> relative gains can change with hardware, scale and configuration; only the
> frozen official evaluation establishes a competition score.

## Confirmed-correct claims and important non-findings

The following checks support preserving the corresponding draft material:

| D location | Confirmed implementation fact | Evidence, mono-root-relative |
| --- | --- | --- |
| L17-25 | Target scale, measurement duration, replication and shared CPU design agree with policy. | `config/main/competition.yml:85`; `server/connect/sim-latency/evaluator/container/resource-boundary.sh:8` |
| L35-39 | A trusted shared control precedes the first candidate build; later jobs reuse its authenticated bytes. | `server/connect/sim-latency/evaluator/container/evaluator.sh:1458`, `:1544`; `server/controller/competition_controller.go:6106` |
| L43-46 | Warm-up establishes mature evidence and validates the client pool before measurement. | `server/connect/sim-latency/run.go:453`; `server/connect/sim-latency/provision.go:290`; `server/connect/sim-latency/client.go:292` |
| L47-57 | CSV/body-completion behavior and timeout charging are consistent with the scorer. Complete non-200 responses can retain their HTTP status but still fail scoring. | `server/connect/sim-latency/client.go:226`, `:1017`; `server/connect/sim-latency/score.go:1057` |
| L57-65 | Raw scoring is run-level p95, aggregated by median; normalized display is clamped and not the v2 ranking key. | `server/connect/sim-latency/score.go:269`, `:641`, `:1085`; `server/model/competition_automation_model.go:230` |
| L67-82 | Six gates plus production median threshold, Welch significance and supported next-margin checks are required. | `server/connect/sim-latency/score.go:682`; `server/model/competition_automation_model.go:151` |
| L92-94 | Work drains after close; six admission windows do not bound total season duration. | `server/controller/competition_automation_store.go:143`; `sn/api/competition.yml:64` |
| L113-121 | Ordinary retrospective review preserves score/rank evidence and reselects from the sealed ranking; publication does not wait for review. | `server/controller/competition_automation_store.go:87`, `:121`, `:428` |
| L125-129, L142 | Literal one-file allowlist, maximum patch size, exact UTF-8/LF bytes, structural restrictions and protected simulator are accurate. | `config/main/competition.yml:64`; `server/controller/competition_controller.go:2898`; `server/connect/sim-latency/evaluator/PATCH-SURFACE.md:19` |
| L128-131 | Submission builds are offline and isolated; malformed/incomplete/OOM evidence fails closed. | `server/connect/sim-latency/evaluator/container/Dockerfile.submission:61`; `server/connect/sim-latency/score.go:235`; `server/connect/sim-latency/evaluator/container/README.md:87` |
| L135-137 | The repository identifies MPL-2.0; the broad file-level-copyleft description agrees with Mozilla's own explanation. | `server/LICENSE:3`; [Mozilla FAQ, Q1 and Q11](https://www.mozilla.org/en-US/MPL/2.0/FAQ/) |
| L144 | A hotkey-based daily platform limit is documented by Macrocosmos, independently of UR's unbounded FIFO. | [Apex CLI documentation](https://docs.macrocosmos.ai/subnets/subnet-1-apex/subnet-1-base-miner-setup/apex-cli); `server/controller/competition_controller.go:775` |

Toolchain non-finding: the advertised frozen server commit
`661759b163c9e4c473d63403734fdf4486bc25c5` has `go 1.27.2` in `server/go.mod:3`
and pins Go 1.27.2 in
`server/connect/sim-latency/evaluator/container/Dockerfile.base:6` **at that
commit**. Current main instead pins Go 1.27.3 in those files and documents the
new staging builder at `server/connect/sim-latency/RUN-MAIN.md:129`. A newer
builder on main does not rewrite the frozen live image. Therefore the draft's
1.27.2 assertion is not marked false merely because main advanced. Confirm the
intended production image/version together before freezing this page; the
public discovery response alone does not expose the running Go binary version.

The generic references to Apex UI fields and top-scorer promotion are also not
treated as missing UR JSON fields. UI policy belongs to Macrocosmos. In
particular, `score_to_beat` being absent from UR discovery does not prove it is
absent from the Apex dashboard.

## Suggested clarifications, not additional accuracy defects

The gate table is a useful summary, but a link to exact thresholds would help
miners explain unplaceable results. `server/connect/sim-latency/score.go:687`
defines G1 as median success at least 97% and no more than one percentage point
below control. G2 requires median request count **and bytes within 95%-105%**
of control, not merely a lower volume floor. G3 uses minimum per-replicate
coverage of 95%. G4 additionally requires nonempty pools in every replicate,
at least 90% sample-window span, load p95 at most 125% of control, and pool p05
at least 90% of control. G5/G6 require every replicate to pass. The existing
high-level table is not false, but it is not the complete gate specification.

Likewise, state that the local example intentionally uses smaller scale and,
unless overridden, an in-process fleet rather than four fleet shards
(`server/connect/sim-latency/main.go:359`). It is not an official comparison.

## Open questions and owner confirmations

These are not asserted implementation defects or grounds to invent new policy.

1. **Macrocosmos: actual platform integration.** What release/competition ID
   consumes the explicit v2 capability and reward revisions? Verify election,
   replacement, restoration, no-winner and invalidation end to end. The UR
   reference adapter and synthetic receipts do not prove deployed Apex behavior.
2. **Macrocosmos: money movement.** Confirm top-scorer update latency, reward
   asset/amount, payout cadence, burn anchor/formula, no-winner handling and
   liability/remedy for already-applied rewards. UR's original `completed_at`
   settlement anchor is not itself a formula or a transfer receipt
   (`server/controller/competition_automation_store.go:111`).
3. **Macrocosmos: fees and limits.** Resolve P1-2 and publish invalid-patch,
   terminal-failure, cache-hit and refund rules. The public CLI supports the
   four-per-hotkey daily statement, but exact rolling-window semantics,
   cross-competition scope for this release and whether cache hits consume a
   slot still need platform confirmation. This is separate from UR queue limits.
4. **UR and Macrocosmos: production repeat entitlement.** V2 requires an
   explicit `allow` or `exclude_exact_identity` policy
   (`server/model/competition_automation_model.go:68`). Current staging chooses
   `allow` at `config/main/competition.yml:44`; that does not select production
   policy. Under exclusion, matching source-lock/patch identities are screened
   before a fee and excluded from new reward entitlement
   (`server/controller/competition_automation_store.go:583`;
   `server/controller/competition_controller.go:7900`). A later epoch has a
   fresh workload, but its source can be unchanged. D:L143 should not imply
   that source necessarily changes or that every repeated patch is admissible.
5. **UR: review process and source policy.** Publish the review target, appeal
   period, contact, authorized decision makers, conflict/escalation process,
   evidence access and compensation rules. Decide whether the implemented
   eligible-candidate source selector or a narrower winner-only rule is the
   intended participant promise. Do not auto-approve when a review target expires.
6. **Release owners: production activation evidence.** Record the paid season's
   exact source lock, image/toolchain, positive margin, policy approval,
   qualification and runtime receipts. Current fee-free staging is not that
   evidence. Recheck discovery when publishing this draft; its observed state
   is time-sensitive.
7. **UR/legal/platform: contribution and attribution terms.** The repository
   license supports the MPL identification, but does not itself prove that a
   particular click-through licenses every submission or guarantees a merge.
   Confirm those terms and how authorship is retained from a plain diff: the
   evaluator uses synthetic author metadata
   (`server/connect/sim-latency/evaluator/container/Dockerfile.submission:38`),
   so evaluator Git authorship is not evidence of participant attribution in
   the final product commit. No legal conclusion about enforceability is made.

## Publication checklist

- Correct source-choice timing and distinguish the actual incumbent from the
  current reward recipient.
- Resolve the broader fee guarantee with Macrocosmos, or narrow it to proven
  admission-retry idempotency.
- Clarify queue/publication waits and preparation versus scoring control.
- Fix artifact paths and clearly label baseline versus candidate local testing.
- Label planned production versus staging, link the normative v2 contract, and
  publish the missing review/appeal and economic policy owners.
- Confirm the production image/toolchain at its frozen digest; preserve the
  technically correct scoring, patch and containment descriptions.

This is a read-only documentation/contract review. No live qualification,
payment test, patch submission or test-suite execution is claimed.
