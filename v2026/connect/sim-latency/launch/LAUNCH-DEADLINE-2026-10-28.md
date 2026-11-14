# Midnight launch-readiness deadline: missed

The required opening was **2026-10-28 00:00 UTC**. The competition was not
verified ready by that deadline. This record does not amend the date, approve
late admission, or claim that partial qualification satisfies the objective.

## Direct observations

At **00:00:10 UTC**:

- Public `GET /competition/info` returned HTTP 503, `source_not_ready`.
- A correctly formed `GET /competition/v2/events?after_cursor=0&limit=1`
  carrying the production settlement token returned HTTP 401.
- Native calibration session `27905` was still live. It reported nine control
  replicates and seven candidate replicates complete for the first reference
  job. The complete three-reference-job screen was not accepted.
- Genuine three-member Snow qualification remained incomplete. No successful
  replacement for the retained failed Muse qualification had been established.
- Production activation had not been performed by this preparation workflow.

## Preserve the real boundaries

The earlier 3 GiB management-scope OOM was our launcher error, not a submission
failure. Its zero-measurement proof and failed artifacts remain retained. The
owner approved a one-use same-seed recovery with 8 GiB management memory and a
fresh first-job deadline of **00:28:07 UTC**. That exception did **not** extend
the public launch deadline. See
[the incident and recovery record](UNMEASURED-MANAGEMENT-OOM-2026-10-27.md).

Continue observing the existing native process; do not restart it on an
observation timeout. R9, evaluation budgets, gates, significance, margin, input
bytes and the unstarted second committed seed remain unchanged.

The requested vault commit contains the correct credential registration, but
its loaded deployment on serving API replicas remains unverified. Compare the
actual decrypted/mounted `competition.yml` SHA-256 with
`a29c3f85076634f49fb5c515fad0f55318e973d12772dd8775a73eb6fb68e562`, then
restart API processes after vault convergence. Do not print credentials.

Calibration acceptance, genuine Snow qualification, exact-source/runtime
qualification and API/worker readiness acknowledgments remain necessary.
The prepared bootstrap cannot create a new initial opening in the past. Do not
fabricate an earlier intent, backdate activation, bypass the scoring lock, or
silently reschedule. A late public opening needs explicit owner direction.
