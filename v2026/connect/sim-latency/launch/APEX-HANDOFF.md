# Apex external-evaluator handoff

Status: integration contract approved, 2026-08-29. Macrocosmos has approved the
non-standard external-evaluator design; concrete integration ownership, staging
evidence, release identities, and registry activation remain to be recorded.

## Competition contract

The competition is a six-epoch season. Each epoch accepts submissions for
exactly seven days. Each admitted canonical patch enters the dedicated
single-job FIFO immediately. At close, new admissions stop while the worker
continues through every accepted job, seals the deterministic significant-
candidate ranking, then exits without publishing a winner. The operator-controlled
agent harness inspects candidates in deterministic rank order, appends either a
rejection or approval with a JSON evidence digest, and finalizes only the first
honest significant candidate. If every candidate is rejected, the epoch has no
winner. The external control loop then promotes the approved winner and
prepares the next hidden-seed epoch. A 16-hour preparation window is reserved
for the next same-round rebaseline.

The number of admitted submissions per epoch is unbounded. The Apex adapter
collects the fixed $20 USD submission fee exactly once before forwarding each
production admission; transport retries of its durable admission record are
not recharged.
Identical canonical patch bytes share one `(round_id, patch_sha256)` result and
do not consume another noise draw. One score job has a three-hour hard execution
limit; the adapter must therefore be asynchronous and tolerate an unbounded
post-close grading window.
A continuously available host supplies 56 full worst-case three-hour slots
during a seven-day admission window. The first job also establishes the one
nine-run epoch control; every later job runs only nine candidate replicates.
At the observed staging rate, later jobs are projected around 1 hour 16 minutes,
or roughly 130 theoretical slots before build, scoring, transition, and
recovery overhead. These are planning capacities, not admission caps; excess
accepted work remains queued through post-close grading.

Before production epoch 1, the API can expose a staging era with sequential
`staging_round` epochs beginning at zero. Its submissions are fee-free and use
the real retention, cache, FIFO, evaluator, scoring, embargo, and polling paths
against frozen source epoch zero. After admission closes and the FIFO drains,
a staging round automatically names its highest-ranked placeable, statistically
significant candidate passing every gate, or no winner if none qualifies, then
publishes each score or typed failure at the immutable job status URL. Its
finalized result is also available through
the opt-in `GET /competition/leaderboard?include_staging=true` view with
`staging: true`, the same `winner_job_id`, and a matching winning entry. All
staging entries retain `honesty_review: not_reviewed`, including the winner;
this does not attest honesty or safety. Staging never enters production ranking,
honesty review, source promotion, or the default production leaderboard, and
never changes the source-epoch-zero threshold. The first production-round commit
ends the era and atomically cancels queued staging work; it refuses to race a
running evaluation. The adapter must prefer `active_round` once present and
persist the returned `staging` boolean.

## Adapter mapping

The Apex-facing adapter has no evaluator privileges and never receives the
hidden seed, MinIO credentials, Docker socket, host resource reports, operator
token, or candidate filesystem. It holds one submitter token in its secret
store and maps the public Apex identity to its own durable record of the
returned immutable job id.

| Apex action | Main API action |
|---|---|
| Discover policy and active epoch | `GET /competition/info` |
| Discover staging-era test identity | Read `staging_round` only when no `active_round` exists |
| Submit canonical text patch | `POST /competition/score` |
| Persist accepted identity | Store `job_id`, `round_id`, `patch_sha256`, and `status_url` atomically |
| Poll result | `GET /competition/score/{jobId}` using the returned status URL |
| Publish completed production epochs | `GET /competition/leaderboard` |
| Reconcile completed staging epochs | `GET /competition/leaderboard?include_staging=true` and select `staging: true` |
| Reproduce after reveal | `GET /competition/round/{roundId}/providers.yml` and authenticate `X-Content-SHA256` |

The adapter must send the exact patch text accepted from the player. It must
not download repositories, accept player-built images, retry a transport-unknown
submission under changed bytes, or submit a second identity to bypass a pending
job. HTTP 429 is backpressure; typed retriable 5xx results retain the same
identity. Typed submission failures are terminal.

Patch serialization must preserve canonical bytes: UTF-8, LF-only line
endings, exactly one final LF, no trailing blank line, and lexicographically
sorted strict unified-diff sections. The server validates but does not repair
the value. The adapter must read a patch as bytes/text directly into its JSON
encoder; shell command substitution and trimming helpers are forbidden because
they can remove the final LF. The byte-level rules and a safe `jq --rawfile`
example are in `launch/ONBOARDING.md`.

Results remain embargoed while admission is open and while any accepted job is
queued or running. The legacy polling `state` reports terminal work as
outcome-neutral `completed` until finalization commits. The additive
`evaluation_status` distinguishes running work from terminal failure without
revealing scores; `evaluation_failure` may expose a reviewed code and kind,
never a raw message, and terminal failures have `retriable: false`. Unknown
failure codes remain private. These fields were verified on the live API on
2026-09-15; clients should still tolerate their absence during rolling updates.
Staging then publishes full outcomes through polling and
the explicit staging-inclusive leaderboard; the default view remains production
only. Production rows identify
approved, rejected, and unreviewed honesty status without exposing the private
review report.

Staging epoch 5 is open, verified at `2026-09-19T09:57:30Z`:
`round_id=01a0b913-c344-27f3-cb64-338dbddc8b07`,
`opens_at=2026-09-19T09:57:00Z`, `closes_at=2026-09-21T09:57:00Z`.
API version `2026.9.18+1049819730` repeatedly advertises source
`807b473c927d1ae09a03276bb9758afb715fac9e` and evaluator
`sha256:b0c07cf45c30adb483ee5c215b7426b2e098c0b35c7cf4c4abcb0166cb43a87e`.
The migration audit reaches 683 with the shared-control and historical-ranking
guards present. The singleton worker started after successful staging
preflight and remains active with zero restarts; authenticated staging host
refreshes continued through `2026-09-19T09:57:19Z`.
Repeated API checks at `2026-09-19T09:59:31Z` matched this state, and an
authenticated public-TLS metrics query verified a fresh worker heartbeat.
This is a fee-free 48-hour staging round using source epoch zero, not a
production epoch. Follow the [release record](STAGING-5-RELEASE.md) for worker
identities and verification; no successful shared-control result is claimed yet.
The worker installed at this September 19 opening still enforced the historical
null-winner policy. The automatic named-staging-winner control-plane follow-up
described above is not yet deployed; it requires a reviewed API/migration/worker
rollout, not an evaluator rebuild or source promotion. No live named winner is
claimed by this opening record.

Staging epoch 4 remains finalized:
`round_id=01a0a58b-9a3e-2e43-f612-0034ff7296ff`,
`opens_at=2026-09-15T14:56:00Z`, `closes_at=2026-09-17T14:56:00Z`.
Its staging-inclusive leaderboard has four entries; Macrocosmos reported that
its downstream path worked end to end. This closed round no longer accepts
submissions, and its worker has exited. Its immutable release policy and
published results are preserved alongside the new epoch-5 deployment.

Update, 2026-09-16: a comment-only job completed all 18 replicates but failed
G5 on a startup database recovery. Its frozen source omitted a wrapped-error
fix already present on `main`; see the
[G5 incident record](STAGING-4-G5-INCIDENT.md). The runtime repair and exact-source
regression gate passed independent tests, including three full sim-latency
suite runs. The [epoch-5 release](STAGING-5-RELEASE.md) also switches to one
immutable shared baseline and absolute-latency ranking. Its API/config rollout
is verified; successful live shared-control scoring remains pending.
Epoch 4's image and historical results remain unchanged. Exact-image production
qualification and the production launch approvals are still pending.

A winner must be placeable,
`takeover_eligible`, and pass every G1-G6 gate. Ordering is absolute raw score
ascending, submission time, then job id; normalized score is display-only.
Historical per-job-control epochs, including staging epoch 4, keep their
original normalized-score-first ranking. Rollout does not retroactively reorder
those published results.
In production, statistical eligibility only enters the review queue; it does
not establish that a patch is honest. Public rows use
job and patch identities rather than bearer-token principal names; the adapter
may associate those job ids with Apex identities in its own publication layer.

UR provides a durable Go reference adapter and conformance suite for this
mapping. Apex and UR have not yet recorded ownership of the runnable production
integration or its private-registry representation. The standard
`apex.competition.v1` two-sandbox player/referee contract does not contain an
external-evaluator field; Macrocosmos has approved this deliberate exception.

## Trust and release boundary

Candidate patches are structurally validated and built offline into one
content-addressed image per canonical patch. Runtime has default-deny external
networking, ten evaluation CPUs, bounded memory/PIDs/logs, fresh PostgreSQL and
Redis, and direct read-only `config/local` and `vault/local` configuration mounts. Two
management CPUs and reserved memory remain outside candidate limits so the
trusted runner can terminate CPU or memory bombs and remove exact labeled
containers and networks.

The main API serves `sn/api/competition.yml`; a separate submission API is not
deployed. Score evidence and generated workloads are authenticated after upload
to versioned MinIO objects with compliance retention before PostgreSQL accepts a
terminal score. Operational signals are exported by the API and worker to the
main Grafana/Mimir pipeline.

## Remaining operational handoff record

Before Apex stage activation, Macrocosmos and UR must record:

- the accepted external-evaluator adapter protocol and unbounded asynchronous
  grading interval;
- the chosen integration owner and stage/production release identity: adapter
  image digests and cosign identities, or an equivalent digest-pinned direct
  Apex platform release;
- the evaluator, OpenAPI, base-source, scorer, and patch-policy digests, plus
  evidence that the continuously maintained main API and worker persist their
  exact runtime image digests with each evaluation rather than freezing their
  source commits season-wide;
- the Apex competition repository/spec release, completed onboarding manifest,
  and any explicit waiver or replacement for the standard player/referee
  artifacts;
- stage credentials, registry activation identifiers, and at least one
  end-to-end stage submission whose job, patch, and leaderboard identities
  reconcile; and
- fee, reward, eligibility, legal/abuse, incident, and notification owners.

The design decision is no longer an open gate. Until the operational record
exists, the dedicated evaluator and REST contract are launchable directly
through the main API, while the Apex-facing launch remains
`integration_handoff_pending`.
