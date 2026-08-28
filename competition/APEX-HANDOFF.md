# Apex external-evaluator handoff

Status: integration-ready draft, 2026-08-28. Macrocosmos staging acceptance and
registry activation are external approvals and are not represented as complete.

## Competition contract

The competition is a six-epoch season. Each epoch accepts submissions for
exactly seven days. No candidate evaluation starts while submissions are open.
At close, the dedicated evaluator grades every admitted canonical patch through
one FIFO worker, freezes the deterministic winner, publishes the finalized
leaderboard, and prepares the next hidden-seed epoch. A 16-hour preparation
window is reserved for the next same-round rebaseline.

The launch cap is ten distinct canonical patches per epoch. Identical canonical
patch bytes share one `(round_id, patch_sha256)` result and do not consume
another noise draw. One score job is bounded by 49,392 seconds; the adapter must
therefore be asynchronous and tolerate the complete post-close grading window.

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

Only a finalized leaderboard is public. A winner must be placeable,
`takeover_eligible`, and pass every G1-G6 gate. Ordering is normalized score
descending, raw score ascending, submission time, then job id. Public rows use
job and patch identities rather than bearer-token principal names; the adapter
may associate those job ids with Apex identities in its own publication layer.

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

## External acceptance record

Before Apex stage activation, Macrocosmos and UR must append a signed record
containing:

- the accepted external-evaluator adapter protocol and maximum asynchronous
  grading interval;
- stage and production adapter image repository digests and cosign identities;
- the current main-API, worker, evaluator, OpenAPI, base-source, scorer, and
  patch-policy digests;
- the Apex competition repository/spec release and completed public
  `HANDOFF.md` expected by the Apex registry process;
- stage credentials, registry activation identifiers, and at least one
  end-to-end stage submission whose job, patch, and leaderboard identities
  reconcile; and
- fee, reward, eligibility, legal/abuse, incident, and notification owners.

Until that signed record exists, the dedicated evaluator and REST contract are
launchable directly through the main API, but the Apex-facing launch remains
`external_acceptance_required`.
