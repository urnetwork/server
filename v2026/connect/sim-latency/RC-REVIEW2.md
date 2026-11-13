# Independent release-candidate review — change 667

## Executive summary

Recommendation: revise before publishing this as an unqualified description of the currently operating competition. The central production-v2 scoring and retrospective-review model is substantially accurate. In particular, do **not** revert the page to the legacy model in which human approval precedes publication.

Five actionable accuracy findings remain:

1. **High — distinguish the planned production season from the live staging season.** Public discovery currently reports fee-free, two-day, zero-margin v2 staging, not an active seven-day paid production epoch.
2. **High — correct the source-promotion promise.** Approval alone does not guarantee inclusion in the following epoch. There is a frozen source-choice deadline and independent build/readiness gates. Moreover, the implemented source-selection path accepts separately approved eligible candidates; it does not enforce a latest-reward-winner-only restriction.
3. **Medium — repair the local example.** Its metadata and completion-marker outputs make the checkout dirty, preventing a subsequent measured run. Its working-directory assumptions and distinction between a clean baseline run and testing an edited candidate also need clarification.
4. **Medium — distinguish execution budget from turnaround time.** The three-hour budget excludes initial queue wait; backlog drain has no fixed upper bound.
5. **Low — qualify the provider warm-up estimate.** The ordinary 12-hour reliability floor explains approximately 8.4 hours, but that is not an unconditional production admission rule.

Do not silently change the advertised Go version solely because current `main` uses Go 1.27.3. The frozen evaluated source and the builder release are different identities; the production image/toolchain pairing needs an explicit release confirmation. Apex rate limiting, dashboard fields, fee deduplication, settlement timing, and contributor attribution likewise need their respective owners' confirmation, not inference from the UR API.

This was a read-only investigation. No submissions, deployments, migrations, workload runs, commits, or external writes were performed. Only this review file was created.

## Scope, provenance, and evidence hierarchy

Reviewed the exact [change-667 draft](https://docs.macrocosmos.ai/~/changes/667/subnets/subnet-1-apex/subnet-1-current-competitions/urnetwork-latency-competition), retrieved through its [same-change Markdown representation](https://docs.macrocosmos.ai/~/changes/667/subnets/subnet-1-apex/subnet-1-current-competitions/urnetwork-latency-competition.md) on 2026-10-23. The browser retrieval tool could not open either URL, but ordinary HTTPS retrieval succeeded. No published-page substitute was used. The retrieved Markdown SHA-256 was `17450afaa0b9b7e4f76761e9937b2b399ba61cd4d198544a3347eeb48e5f8844`.

Local snapshots inspected:

| Repository | HEAD at inspection |
|---|---|
| `server` | `f2a9cf7af613d1a9c3ff6e29ec2a0690e9ff55e4` |
| `sn` | `0f30cb8d11456158c3e59dd09f382f7c4124f4d8` |
| `config` | `14229f9ef5a81aca3d5992a1d5ffdc8fe793b267` |

Evidence priority was actual implementation and frozen configuration, then the v2 OpenAPI contract, then explanatory documentation. `FINAL-AUTOMATE.md` describes the design and policy boundaries; its explicitly historical descriptions of the earlier implementation are not treated as current behavior. The README also explicitly labels its later manual promotion instructions as legacy v1. See [server/connect/sim-latency/README.md:234](/home/by/urnetwork/server/connect/sim-latency/README.md:234) and [server/connect/sim-latency/FINAL-AUTOMATE.md:57](/home/by/urnetwork/server/connect/sim-latency/FINAL-AUTOMATE.md:57).

Implementation references below are labelled relative to `/home/by/urnetwork`, so `server/connect/...` is not confused with the sibling `connect` repository.

Draft section locators used below:

| Key | Exact section heading |
|---|---|
| D1 | Evaluation Overview |
| D2 | Scoring |
| D3 | Epochs and the Moving Threshold |
| D4 | Winner Selection and Review |
| D5 | Constraints |
| D6 | Licensing |
| D7 | Miner Submissions |
| D8 | Iterating Locally |
| D9 | The Measured Path |

## Accuracy findings, ordered by severity

### R1 — High: production rules need an explicit launch-status boundary

Draft location: D3 opening paragraph and initial-margin bullet; D7 fee, duration, and automatic-opening bullets.

The stated production design is supported, but the current operating configuration is explicitly staging. At approximately 05:16 UTC on 2026-10-23, [public competition discovery](https://api.bringyour.com/competition/info) reported:

| Field | Observed value |
|---|---|
| Active production round | Absent |
| Staging epoch | 2, open |
| Decision mode | `provisional_reward_v2` |
| Capability | `competition.reward.v2.runtime-blob.v1` |
| Admission window | 172,800 seconds, or two days |
| Submission fee | USD 0 |
| Takeover margin | 0 |
| Replicates | 9 |

The configured policy independently confirms `staging: true`, shadow settlement, two-day windows, and zero fees. See [config/main/competition.yml:25](/home/by/urnetwork/config/main/competition.yml:25), [config/main/competition.yml:46](/home/by/urnetwork/config/main/competition.yml:46), and [config/main/competition.yml:97](/home/by/urnetwork/config/main/competition.yml:97). Staging winner eligibility deliberately does not require production statistical improvement: [server/model/competition_automation_model.go:151](/home/by/urnetwork/server/model/competition_automation_model.go:151). Production and staging policy alternatives are documented in [sn/api/competition.yml:1456](/home/by/urnetwork/sn/api/competition.yml:1456).

Why it matters: an entrant checking the API today encounters different economics, timing, and eligibility from the unqualified page. Staging must not be mistaken for the start of the paid season, nor should staging values replace the intended production rules.

Suggested wording:

> This page specifies the production v2 season. Before production activation, UR may operate separately identified staging rounds with no admission fee, no live rewards, different admission windows, and no production improvement requirement. Check the selected round's published policy and staging flag. The production launch time and release identity will be announced separately.

Release action: add a dated production/staging status notice and a link to discovery. If this page is meant to become visible only after production activation, make verification of an active production round a publication prerequisite instead.

### R2 — High: approval is not a next-epoch guarantee, and source selection is not restricted to the current reward winner

Draft location: D3 source/threshold progression bullets; D6 final sentence.

There are two distinct mismatches with v2 implementation:

- **Timing:** a proposal must have authentic source approval and build readiness by the frozen `source_choice_at`. A later approval cannot change a round whose source has already been selected. Production then still has a separate 16-hour preparation gate. Approval can therefore exist while the following epoch retains the incumbent.
- **Candidate restriction:** source authorization requires an eligible entry in an immutable evaluation seal, affirmative integrity/source approval, matching incumbent source, exact patch/evidence identities, and valid readiness. It does not require that entry to equal the current reward recipient. The source selector ranks the qualifying ready proposals independently. A lower-ranked reward candidate with source approval can be selected while a higher-ranked provisional recipient is deferred; a still-applicable candidate from an earlier round can also remain usable. This is broader than a promise that only the winning patch supplies the next source.

Evidence:

- Source-choice timestamp: [server/controller/competition_automation_store.go:154](/home/by/urnetwork/server/controller/competition_automation_store.go:154).
- Deadline checks on actual review/build completion: [server/controller/competition_automation_source_store.go:361](/home/by/urnetwork/server/controller/competition_automation_source_store.go:361).
- Eligible-entry and exact-source approval checks, without a current-winner identity requirement: [server/controller/competition_automation_source_store.go:187](/home/by/urnetwork/server/controller/competition_automation_source_store.go:187).
- The database source-authorization guard likewise checks the review and deadline, not current reward-winner identity: [server/competition_automation_staging_source_graph_migration.go:113](/home/by/urnetwork/server/competition_automation_staging_source_graph_migration.go:113).
- Selection of still-applicable proposals by score/time/id: [server/connect/sim-latency/source_reviewed.go:111](/home/by/urnetwork/server/connect/sim-latency/source_reviewed.go:111).
- Independent preparation gate: [server/controller/competition_supervisor.go:19](/home/by/urnetwork/server/controller/competition_supervisor.go:19), [server/controller/competition_supervisor.go:496](/home/by/urnetwork/server/controller/competition_supervisor.go:496).
- Explicit operational explanation: [server/connect/sim-latency/RUN-MAIN.md:322](/home/by/urnetwork/server/connect/sim-latency/RUN-MAIN.md:322).

Suggested wording consistent with current implementation:

> Reward selection and source selection are independent. At each source-choice deadline, UR may select a separately approved, build-ready eligible candidate whose evaluated base still matches the incumbent. Otherwise, the next epoch retains the approved source and its threshold. A late approval cannot alter an already frozen epoch. Promotion uses the selected candidate's authenticated threshold recommendation and must complete the independent readiness checks before admissions open.

If the intended public policy is narrower—only reward winners may ever be promoted—record that as an unresolved policy/implementation discrepancy. Do not claim the code enforces it today. Resolve that choice prospectively before paid admission; this review does not authorize broadening the policy or changing source.

Also qualify the ordinary carry-forward explanation for incidents. Revoking an already activated source can trigger incident holds; it is not an automatic safe rollback to previous source. See [server/controller/competition_automation_incident.go:307](/home/by/urnetwork/server/controller/competition_automation_incident.go:307).

### R3 — Medium: the local example is not safely repeatable as written

Draft location: D8 second-terminal command block and the paragraph immediately following it.

The metadata filename in the example is `results.run.json`. A measured run writes that file and, by default, a completion marker named `results.run.json.complete.json`. Neither is ignored. The next measured command checks every repository using `git status --porcelain=v1 --untracked-files=all` and rejects a dirty worktree. Thus a successful example run can prevent the next run before measurement starts.

Evidence:

- Completion-marker default: [server/connect/sim-latency/run.go:162](/home/by/urnetwork/server/connect/sim-latency/run.go:162).
- Metadata and marker output: [server/connect/sim-latency/run.go:316](/home/by/urnetwork/server/connect/sim-latency/run.go:316).
- Clean-tree verification: [server/connect/sim-latency/source_epoch.go:478](/home/by/urnetwork/server/connect/sim-latency/source_epoch.go:478).
- Existing ignore rules: [server/connect/sim-latency/.gitignore:8](/home/by/urnetwork/server/connect/sim-latency/.gitignore:8). A read-only `git check-ignore -v` check confirmed that the CSV and provider file are ignored, but the example metadata is not.

Two related onboarding issues belong in the same correction:

1. The two terminal blocks assume different starting directories. From a common multi-repository root, terminal 1 enters `server/local`; terminal 2 must enter `server/connect/sim-latency`, not the sibling `connect` repository.
2. The clean-tree check also rejects a participant's edited Go file, and normally requires every locked repository's exact branch/commit. These commands demonstrate the frozen unmodified baseline, not an arbitrary patched candidate. A bare current-main `server` clone is insufficient. The page should link a supported candidate-testing procedure rather than imply that editing the allowed file and rerunning this command works.

Suggested replacement for the local instructions, after the complete published source checkout has been prepared:

```bash
# Both terminals start at the multi-repository checkout root.
# Terminal 1:
cd server/local
./run-local.sh

# Terminal 2, independently from that same root:
task_run_dir="$(mktemp -d)"
cd server/connect/sim-latency
./run-local.sh init \
  --count 500 --clients 100 --rate 40 --quality-window 2 \
  --seed 1 --out "$task_run_dir/providers.yml"
./run-local.sh run \
  --epoch 0 --reset --providers "$task_run_dir/providers.yml" \
  --duration 180s --meta "$task_run_dir/run.json" > "$task_run_dir/results.csv"
./run-local.sh analyze --run "$task_run_dir/results.csv"
```

This is a proposed baseline example, not a claim that a fresh external contributor can already obtain every required source/config artifact. The source manifest and correct generation must be supplied through onboarding. An explicit nonzero generation must replace `0` when appropriate. The added duration makes this a three-minute measured window; without it, the CLI default is 30 minutes, not the official 180 seconds. See [server/connect/sim-latency/main.go:325](/home/by/urnetwork/server/connect/sim-latency/main.go:325).

Add a short warning that `--reset` clears the dedicated local PostgreSQL/Redis state, and keep the foreground launcher/endpoint-attestation requirement. Those guards must not be bypassed: [server/connect/sim-latency/README.md:380](/home/by/urnetwork/server/connect/sim-latency/README.md:380).

### R4 — Medium: the deadline is an execution budget, not a submission-to-result SLA

Draft location: D1 deadline row; D3 opening paragraph's season-duration estimate.

The first queue wait is explicitly excluded from the three-hour clock. After the first claim, attempts and retry backoff consume the same budget. Publication may be later still because every accepted job must finish, and the sealed result must reach its reveal boundary.

Evidence: [server/controller/competition_controller.go:6022](/home/by/urnetwork/server/controller/competition_controller.go:6022), [server/controller/competition_controller.go:6752](/home/by/urnetwork/server/controller/competition_controller.go:6752), and the exact contract at [sn/api/competition.yml:1451](/home/by/urnetwork/sn/api/competition.yml:1451).

Unbounded unique admission plus a single evaluator means the extra season duration is not bounded to a modest extension. A per-hotkey Apex rate limit does not bound the number of participating hotkeys. The design explicitly recognizes arbitrarily long grading drains: [sn/api/competition.yml:64](/home/by/urnetwork/sn/api/competition.yml:64), [server/connect/sim-latency/FINAL-AUTOMATE.md:233](/home/by/urnetwork/server/connect/sim-latency/FINAL-AUTOMATE.md:233).

Suggested wording:

> Each job has a three-hour execution budget starting at its first evaluator claim, including retries, retry backoff, scoring, and cleanup. Initial queue wait is additional. Results remain embargoed until the entire epoch drains and its publication boundary is reached. Six seven-day admission windows do not imply a six-week season; backlog and readiness can extend it substantially, with no fixed drain-time guarantee.

### R5 — Low: the 8.4-hour estimate is not a universal production admission rule

Draft location: D9 first reliability-related bullet.

The number has a real basis: under normal conditions, the ordinary 12-hour lookback has a 0.7 minimum. But production also has a different degraded-condition floor, missing-history treatment, additional eligibility checks, and an explicit provider-intent qualification grace. The document should not turn a useful warm-up estimate into a universal rule about when every provider becomes selectable.

Evidence: the windows are defined at [server/model/network_client_reliability_model.go:58](/home/by/urnetwork/server/model/network_client_reliability_model.go:58); condition-dependent floors and missing-history behavior are at [server/model/provider_eligibility.go:11](/home/by/urnetwork/server/model/provider_eligibility.go:11); the intent-grace exception is described and wired at [server/model/provider_eligibility.go:80](/home/by/urnetwork/server/model/provider_eligibility.go:80). The simulator's own estimate is documented at [server/connect/sim-latency/run.go:452](/home/by/urnetwork/server/connect/sim-latency/run.go:452). The relevant eligibility behavior is also present in the frozen evaluated server commit `661759b163c9e4c473d63403734fdf4486bc25c5`, not just current main.

Suggested wording:

> Ordinary reliability admission can require hours of accumulated evidence; the normal 12-hour reliability floor corresponds to roughly 8.4 hours for a continuously healthy new provider. The simulator backfills established-fleet evidence, so the competition targets steady-state selection and egress rather than cold onboarding.

## Confirmed implementation behavior — retain these parts

The following checks support the draft's main technical model. These are implementation facts, not guarantees about Apex's UI or billing deployment.

| Area | Verified behavior and evidence |
|---|---|
| Workload | Current frozen policy has 1,800 providers, 200 client identities, 80 arrivals/minute, quality window 2, four exchange hosts, four shards, impairment, 180 seconds, and nine replicates: [config/main/competition.yml:85](/home/by/urnetwork/config/main/competition.yml:85). |
| CPU split | The containment policy derives ten physical evaluation cores and two management cores: [server/connect/sim-latency/evaluator/container/resource-boundary.sh:8](/home/by/urnetwork/server/connect/sim-latency/evaluator/container/resource-boundary.sh:8). This is not a claim that every laptop will outperform that host. |
| Raw score | Only requests starting within the measured window contribute; failures receive the request-timeout value before p95: [server/connect/sim-latency/score.go:1057](/home/by/urnetwork/server/connect/sim-latency/score.go:1057). |
| Complete responses | Body parsing validates expected length, and read/validation failures produce unsuccessful observations: [server/connect/sim-latency/client.go:164](/home/by/urnetwork/server/connect/sim-latency/client.go:164), [server/connect/sim-latency/client.go:1023](/home/by/urnetwork/server/connect/sim-latency/client.go:1023). |
| Aggregation and display | Candidate raw score uses replicate medians; the ratio is clamped to 1–200. Rank is raw latency, original submission time, then job ID, not the displayed ratio: [server/connect/sim-latency/score.go:273](/home/by/urnetwork/server/connect/sim-latency/score.go:273), [server/connect/sim-latency/score.go:640](/home/by/urnetwork/server/connect/sim-latency/score.go:640), [server/model/competition_automation_model.go:230](/home/by/urnetwork/server/model/competition_automation_model.go:230). |
| Shared control | The v2 seal authenticates the round-owned baseline and its raw median before electing. The nine-run control is not recreated for each candidate: [server/controller/competition_automation_store.go:163](/home/by/urnetwork/server/controller/competition_automation_store.go:163), [server/connect/sim-latency/README.md:183](/home/by/urnetwork/server/connect/sim-latency/README.md:183). |
| Production eligibility | The implementation requires valid G1–G6, the frozen median improvement bound, statistical significance, nine-replicate identity, and a supported next margin: [server/model/competition_automation_model.go:151](/home/by/urnetwork/server/model/competition_automation_model.go:151). |
| Statistical method | Welch uses run-level means and sample variances, with one-sided alpha 0.05; the next margin cannot weaken the existing floor and must remain in the supported range: [server/connect/sim-latency/score_significance.go:38](/home/by/urnetwork/server/connect/sim-latency/score_significance.go:38). |
| Publication | Close, authoritative drain, seal, and reveal are separate requirements; publication does not wait for human review: [server/controller/competition_automation_store.go:143](/home/by/urnetwork/server/controller/competition_automation_store.go:143), [server/controller/competition_automation_api.go:85](/home/by/urnetwork/server/controller/competition_automation_api.go:85). The current unpublished staging result also returned HTTP 409 with `round_not_published`. |
| Review and replacement | Reviews append dispositions and replace the recipient using the original sealed ranking; measurements and sealed eligibility are not rewritten: [server/controller/competition_automation_store.go:367](/home/by/urnetwork/server/controller/competition_automation_store.go:367), [server/model/competition_automation_model.go:278](/home/by/urnetwork/server/model/competition_automation_model.go:278). |
| Patch boundary | Current configuration admits only the existing server-tree `connect/resident_contract_manager.go`, maximum 262,144 bytes. UTF-8, LF, terminal-newline, path, operation, and build-tag checks are implemented: [config/main/competition.yml:64](/home/by/urnetwork/config/main/competition.yml:64), [server/controller/competition_controller.go:2898](/home/by/urnetwork/server/controller/competition_controller.go:2898). |
| License identification | The server repository identifies MPL 2.0 in [server/LICENSE:1](/home/by/urnetwork/server/LICENSE:1). This confirms the repository's declared license, not contributor assent, legal advice, or the promised attribution workflow. |

The six gate summaries are reasonable high-level descriptions, not literal zero-tolerance assertions. For example, G1 uses a 97% absolute success floor and a baseline-relative floor; G2 permits ±5% volume; G3 requires at least 95% accounting coverage; G4 also checks nonempty pools, sample coverage, and relative pool size. The exact definitions remain in [server/connect/sim-latency/score.go:682](/home/by/urnetwork/server/connect/sim-latency/score.go:682). Link that versioned contract if entrants need exact thresholds; do not replace it with guessed stricter rules.

## Important clarifications that are not established implementation errors

### Toolchain: authenticate the released image before changing the number

Draft location: D1 toolchain row; D5 dependency restriction.

Current `main` pins Go 1.27.3 at [server/connect/sim-latency/evaluator/container/Dockerfile.base:6](/home/by/urnetwork/server/connect/sim-latency/evaluator/container/Dockerfile.base:6), and the runbook mentions that builder at [server/connect/sim-latency/RUN-MAIN.md:129](/home/by/urnetwork/server/connect/sim-latency/RUN-MAIN.md:129). Git history dates the standardization change to 2026-10-21 (`b5d7f8d88`).

However, the live staging base is `661759b163c9e4c473d63403734fdf4486bc25c5`, frozen on 2026-10-19. Read-only `git show` of that commit's `go.mod` and evaluator Dockerfile shows Go 1.27.2. The separately recorded host-tool source `04662618bfd3a283e29c47e913328f37c675839d` also has the 1.27.2 Dockerfile. The staging build record binds the older evaluated source and image: [config/main/sim-latency-staging-image-build.json:1](/home/by/urnetwork/config/main/sim-latency-staging-image-build.json:1). The builder uses its own script-directory Dockerfile, not necessarily the evaluated source tree's copy: [server/connect/sim-latency/evaluator/container/build-base.sh:236](/home/by/urnetwork/server/connect/sim-latency/evaluator/container/build-base.sh:236).

Consequently, neither current-main `go.mod` nor the evaluated source alone proves the compiler embedded in the prospective production image. **Do not label the draft's 1.27.2 claim definitively false on this evidence.** UR should supply the exact production evaluator digest and authenticated compiler/runtime identity, then bind the published version to that release. A future rebuilt 1.27.3 evaluator must not be described as 1.27.2.

### Statistical confidence: describe the scope of alpha

Draft location: D3 calibration rationale; D2 significance requirement.

Add: “The 0.05 threshold applies to each candidate comparison; it is not a season-wide false-winner bound across many submissions.” This is already explicit in [sn/api/competition.yml:59](/home/by/urnetwork/sn/api/competition.yml:59) and [server/connect/sim-latency/FINAL-AUTOMATE.md:171](/home/by/urnetwork/server/connect/sim-latency/FINAL-AUTOMATE.md:171). The draft does not explicitly promise a family-wise bound, so this is a useful precision improvement rather than proof of a false statistical formula. It does not call for changing the frozen scorer.

### Result discovery: distinguish an Apex leaderboard from the legacy UR endpoint

Draft location: D4 final two paragraphs on embargo and publication.

If adding direct API links, use `GET /competition/v2/rounds/{roundId}/result` or `/decisions` for v2. The unversioned UR leaderboard is legacy-only and deliberately omits v2 rounds. The authoritative v2 fields are `provisional_reward_winner`, `reward_revision`, and `decisions`, not the old `winner_job_id`. See [sn/api/competition.yml:139](/home/by/urnetwork/sn/api/competition.yml:139) and [server/connect/sim-latency/README.md:114](/home/by/urnetwork/server/connect/sim-latency/README.md:114).

The page's ordinary-language reference to a leaderboard is not itself wrong: Apex can render one from the v2 result. Do not mistake a UI description for an assertion that UR's legacy endpoint is populated.

Similarly, distinguish private scoring diagnostics from embargo-safe lifecycle signals. Owned polling can report `evaluation_status` and a bounded, message-free failure classification before publication, without revealing score/gates/private error text: [sn/api/competition.yml:487](/home/by/urnetwork/sn/api/competition.yml:487).

## Open questions and owner sign-offs — not verified by UR implementation

### 1. Apex admission limit and dashboard contract

The hotkey-wide four-per-24-hours limit and `score_to_beat` placement are Apex-owned assertions. UR's request contains only `round_id` and `patch`; it has no hotkey-based cross-competition limiter or dashboard renderer. See [server/model/competition_model.go:195](/home/by/urnetwork/server/model/competition_model.go:195). Their absence from UR is **not** evidence that the page is wrong.

Ask Apex to confirm the release/version, whether the window is rolling, how invalid/cache-hit attempts consume it, and the exact meaning/units of `score_to_beat`. A median threshold alone is not sufficient to establish the statistical and gate requirements for a win.

### 2. Same-epoch fee deduplication must be demonstrated by Apex

UR deduplicates evaluation by canonical identity. That does not automatically deduplicate payments. The supplied reference adapter records/charges by Apex `submissionId`, and a new submission identity can collect a fee before UR returns its existing job. Existing-record retries are not recharged. Evidence: [server/controller/competition_controller.go:7059](/home/by/urnetwork/server/controller/competition_controller.go:7059), [server/controller/competition_controller.go:7861](/home/by/urnetwork/server/controller/competition_controller.go:7861), and [server/controller/competition_controller.go:7899](/home/by/urnetwork/server/controller/competition_controller.go:7899).

The draft's free identical-byte resubmission promise can be a valid Apex product policy. Before publishing it, Apex should show how a participant's second upload with a new frontend submission ID reuses the appropriate charge identity or is refunded. Confirm whether this guarantee is participant-scoped or global; global UR job caching does not settle that commercial choice. Do not substitute the weaker guarantee about transport retries without owner approval.

### 3. Cross-epoch repeat rewards require an explicit frozen policy

The v2 policy accepts either `allow` or `exclude_exact_identity`: [server/model/competition_automation_model.go:68](/home/by/urnetwork/server/model/competition_automation_model.go:68). Current staging chooses `allow`, but that does not establish production policy. If production selects exclusion, the same canonical patch against the same unchanged source lock can be ineligible for a new reward, and production preflight rejects known exclusions before the fee collector runs. See [server/controller/competition_automation_store.go:556](/home/by/urnetwork/server/controller/competition_automation_store.go:556) and [server/controller/competition_controller.go:7900](/home/by/urnetwork/server/controller/competition_controller.go:7900).

The page should identify the approved production rule before promising that later-epoch repeats are simply new charged submissions. A fresh round does not necessarily mean a different source: carry-forward deliberately retains it. Distinguish a new evaluation identity, a charge, and a new reward entitlement.

### 4. Immediate top-scorer updates are an Apex integration commitment

UR emits durable revisioned decisions; Apex receives/reconciles them asynchronously. The result explicitly separates recipient, integrity review, source approval, and settlement state. Publication alone is not proof that Apex has already applied a top-scorer change or moved money. See [server/connect/sim-latency/README.md:157](/home/by/urnetwork/server/connect/sim-latency/README.md:157) and [sn/api/competition.yml:49](/home/by/urnetwork/sn/api/competition.yml:49).

Ask Apex to confirm the current adapter release and update-latency target. Prefer “Apex applies the published provisional decision without waiting for source approval” over an unconditional instantaneous-update promise unless that promise is backed by an actual service commitment.

### 5. Revocation, appeals, payouts, and no-winner outcomes need published economic rules

A changed reward decision does not prove transferred funds can be clawed back. A receipt is not necessarily payment confirmation. UR intentionally does not implement Apex's burn or money-movement formula. See [sn/api/competition.yml:60](/home/by/urnetwork/sn/api/competition.yml:60) and [server/connect/sim-latency/FINAL-AUTOMATE.md:435](/home/by/urnetwork/server/connect/sim-latency/FINAL-AUTOMATE.md:435).

Before paid launch, link agreed rules for already-paid rewards after disqualification, replacement timing/compensation, no-winner periods, refunds, appeal filing and decision authority, review service targets, and evidence access. These can live in linked competition terms; this technical page need not invent them. The implementation supports append-only restoration with an appeal reference: [server/model/competition_automation_model.go:374](/home/by/urnetwork/server/model/competition_automation_model.go:374).

### 6. Contributor attribution is not automatically preserved by a unified diff

Draft location: D6 second paragraph's authorship assurance.

The submitted API object has no author identity field, and both source-promotion paths create commits under a fixed competition identity. The v2 path is at [server/connect/sim-latency/source_reviewed.go:278](/home/by/urnetwork/server/connect/sim-latency/source_reviewed.go:278); the legacy path is at [server/connect/sim-latency/source_promote.go:288](/home/by/urnetwork/server/connect/sim-latency/source_promote.go:288).

This does not prove UR denies contributor authorship or cannot provide separate credit. It means the prose promise is not demonstrated by the automated Git path. UR/Apex should confirm the mechanism—approved contributor identity, original author metadata, explicit public credit, or a documented later merge procedure—and the submission-term assent process. Avoid presenting preservation of original Git authorship as an automatic property of a plain diff.

### 7. Publish an entrant-usable source and reproduction bundle

The page appropriately tells entrants to use the epoch's source. It should also link the actual full source manifest, frozen evaluator identity/toolchain, required local configuration, and supported candidate-testing procedure. Public discovery provides the server base/image and policy, but not every sibling-repository commit or local manifest needed by `verifySourceEpoch`; see [sn/api/competition.yml:1266](/home/by/urnetwork/sn/api/competition.yml:1266) and R3.

Confirm those artifacts are accessible to an ordinary miner without UR operator credentials, and test the instructions from that permission level. Do not advise using main, fabricating a source lock, bypassing cleanliness checks, or using operator-only source-promotion commands as the participant workflow.

## Publication checklist

- Resolve R1 and R2 with a dated production release and an agreed source-selection policy.
- Correct the local paths/output locations and label the baseline-versus-candidate workflow accurately.
- State the initial-queue exclusion and unbounded grading extension.
- Qualify the warm-up estimate and add the per-candidate significance caveat.
- Bind compiler/runtime wording to the released evaluator, not merely current main.
- Obtain Apex sign-off on admission limits, UI fields, duplicate charging, repeat rewards, and revision application; link economic/review/appeal terms.
- Confirm attribution and an externally accessible source/reproduction package.

No workload or integration tests were executed because this review was constrained to read-only investigation. Findings are based on source/config tracing, read-only Git inspection, the exact RC content, and two public API read surfaces. Actual Apex service behavior and prospective production activation remain outside that evidence.
