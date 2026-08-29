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
admission; transport retries of its durable admission record are not recharged.
Identical canonical patch bytes share one `(round_id, patch_sha256)` result and
do not consume another noise draw. One score job has a three-hour hard execution
limit; the adapter must therefore be asynchronous and tolerate an unbounded
post-close grading window.

## Adapter mapping

The Apex-facing adapter has no evaluator privileges and never receives the
hidden seed, MinIO credentials, Docker socket, host resource reports, operator
token, or candidate filesystem. It holds one submitter token in its secret
store and maps the public Apex identity to its own durable record of the
returned immutable job id.

| Apex action | Main API action |
|---|---|
| Discover policy and active epoch | `GET /competition/info` |
| Submit canonical text patch | `POST /competition/score` |
| Persist accepted identity | Store `job_id`, `round_id`, `patch_sha256`, and `status_url` atomically |
| Poll result | `GET /competition/score/{jobId}` using the returned status URL |
| Publish completed epochs | `GET /competition/leaderboard` |
| Reproduce after reveal | `GET /competition/round/{roundId}/providers.yml` and authenticate `X-Content-SHA256` |

The adapter must send the exact patch text accepted from the player. It must
not download repositories, accept player-built images, retry a transport-unknown
submission under changed bytes, or submit a second identity to bypass a pending
job. HTTP 429 is backpressure; typed retriable 5xx results retain the same
identity. Typed submission failures are terminal.

Results remain embargoed while admission is open and while any accepted job is
queued or running. Polling reports terminal work as outcome-neutral `completed`
until the post-review finalization transaction commits. Only a finalized
leaderboard is public; its rows identify approved, rejected, and unreviewed
honesty status without exposing the private review report.
A winner must be placeable,
`takeover_eligible`, and pass every G1-G6 gate. Ordering is normalized score
descending, raw score ascending, submission time, then job id. Statistical
eligibility only enters the review queue; it does not establish that a patch is
honest. Public rows use
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
Redis, and only direct read-only `config/local` and `vault/local` mounts. Two
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
