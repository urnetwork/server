# Macrocosmos competition documentation review — 2026-11-02

## Executive summary

The current [URnetwork competition page](https://docs.macrocosmos.ai/subnets/readme/subnet-1-current-competitions/urnetwork-latency-competition)
now agrees with the important workload, ranking, private-score feedback, and
broad patch-surface behavior. The remaining material corrections are winner
review/promotion wording and an explicit immutable source checkpoint. The
earlier humanoid-olympics URL was inaccessible and was not used for this review.

UR's stale single-file document is corrected in
[PATCH-SURFACE.md](../evaluator/PATCH-SURFACE.md). The shareable
[epoch-1 checkpoint](../EPOCH-SOURCE.md#production-epoch-1--initial-baseline) lists every commit and explains why
the canonical API digest differs from a preserved JSON file's raw hash.

This review checks UR's API and source contract, not Apex's private adapter,
fee collection, payment execution, CLI release, or commercial approval.

## Corrections to make

### 1. Review is automated under the current policy

Replace the manual-review characterization in the winner and submission
sections. Discovery selects `elected_winner_snow_union_v2`, with a 1,800-second
review timeout. The adopted contract automatically runs the policy-bound
Qwen/Gemma honesty/safety review, unions their findings, and does not treat a
missing, incomplete, or expired review as approval. UR retains rejection and
incident authority; that is different from routine manual gating.

Suggested replacement:

> After publication, URnetwork automatically reviews the provisional winner for
> honesty and safety. Rejection publishes grounds and advances to the next
> eligible sealed candidate. Approval gates building, qualification, and source
> promotion before the next epoch opens. Infrastructure or review failure
> pauses progression rather than approving a winner by default.

Implementation/policy evidence: `automation_policy` in live discovery,
`server/model/competition_snow_two_member_test.go`, the automated supervisor,
and [AUTOMATION.md](../AUTOMATION.md).

### 2. Do not promise a new epoch while winner review is pending

An unresolved elected winner does not authorize opening the successor on the
incumbent. The adopted flow waits for review, then the approved winner's build
and qualification. With no eligible candidate remaining, the incumbent and
margin carry forward. Missing review or failed qualification pauses and alerts.
Historical empty-policy source-choice deadlines are not the selected policy.
Source and reward approvals remain distinct authenticated records, but the
current closing policy connects them; it is not an arbitrary additional manual
source-choice delay after every successful review.

Operational caveat: the supervisor/companion compatibility upgrade still needs
maintenance qualification before first production close. Describing the policy
does not prove that future automatic closing has already passed live testing.

### 3. Link the complete source checkpoint

Link [EPOCH-SOURCE.md](../EPOCH-SOURCE.md), not only the server's `base_sha`.
Epoch 1 uses source generation 0, activation generation 1, and canonical digest
`ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf`.
Connect is `e90d59eb404f973dba6e9d0689ecd94e2718c10e`.
The preserved C2 source-lock JSON already contains that same ten-repository map.
The ordinary `local/source-graph/lock.yml` is not the competition's source
authority. Sharing source-only pins does not require distributing private config.

### 4. Qualify the local example's working directory

From the mono-root, terminal 2 must enter `server/connect/sim-latency`;
`connect/sim-latency` works only when starting inside `server`.
The local command's `--epoch 0` selects baseline source generation 0, not an
API staging round or a submission target. Keep `--reset` for repeated local
runs and explain the separate local config/services prerequisite. A local
diagnostic never replaces the official same-round control and nine replicates.

### 5. Preserve repository-specific licensing

The editable graph includes forks with different license notices: for example,
the frozen gVisor and glog roots contain Apache-2.0 licenses. Do not describe
every editable repository/file as MPL-2.0 solely because server is MPL-2.0.
State that submissions must respect the applicable repository/file licenses and
the agreed contribution terms; commercial/legal owners should approve any
additional license grant. This is a repository-notice check, not legal advice.

## Confirmed behavior to retain

The live API advertises Go 1.27.3, 3,600 providers, 200 clients, 80 arrivals/minute,
quality window 2, four exchange hosts and fleet shards, impairment, a 180-second
measurement window, nine replicates, a three-hour job budget, and margin 0.269.
The season policy is six seven-day admission windows with a USD 20 submission
fee. Evaluation continues FIFO after admission closes; there is no UR epoch
submission-count cap. Apex's additional admission limits and duplicate charging
remain Apex-owned behavior, not verified by this review.

Scoring code ranks absolute median candidate p95 latency. Normalization is
display-only. Each round shares one authenticated nine-run baseline. Owner-only
completed scores are visible before reveal, but public rankings and winner
decisions remain embargoed. V2 public reconciliation uses the round result
endpoint, not the legacy leaderboard.

Current epoch G2 keeps both 95% floors and 105% ceilings. The approved future
change removes the two ceilings and retains the floors; it must not be presented
as active in epoch 1. Next-epoch variance recommendations cannot lower the
incumbent takeover margin. Final calibration acceptance remains outstanding;
current scoring is explicitly provisional, not a newly completed calibration.

The live mono-root policy permits all ten repositories except
`server/connect/sim-latency/**`. Regular text creation/deletion and 100644/100755
are supported; mode changes, binary/symlink/submodule operations, and
rename/copy metadata are not. Build constraints are not banned structurally in
mono-root mode. Offline dependency availability and honesty review still apply.

## What UR changed in this documentation pass

Updated PATCH-SURFACE.md; published the epoch-1 source explanation; corrected
the README's obsolete intake-only status; and marked historical onboarding/model
policy text as historical. No evaluator policy, baseline, code, credentials,
submission budget, or service was changed. No costly tests ran alongside scoring.
