# Sim-latency incident-response draft

Operational owner, evidence-deletion owner, on-call address, and incident
contact: `support@ur.xyz`.

The main Grafana contact policy routes competition alerts carrying
`service=sim-latency` and `severity=warn|page` to this contact. The runner emits
an immediate heartbeat and then one every 15 seconds. An active epoch warns
when no runner heartbeat has been observed for 30 seconds.

## Severity and first response

`page` means admission, score integrity, retained evidence, or evaluation
progress may be unsafe. Acknowledge immediately, stop new admission at ingress
when integrity is uncertain, preserve the running container and immutable
evidence where safe, and record UTC times and exact job/round identities.

`warn` means capacity or liveness is approaching a boundary but evidence is
not yet known to be lost. Investigate promptly. Escalate to `page` if the state
persists, crosses the critical boundary, or affects an accepted job.

Never reveal an embargoed score, seed, workload, candidate patch, credential,
or private review artifact in chat, tickets, dashboards, or public logs.

## Alert-specific actions

### Runner heartbeat stale over 30 seconds

1. Confirm whether the epoch is scheduled, open, or grading.
2. Check the `RUN-MAIN.sh` process and one-shot `competitionworker` without
   starting a second worker.
3. Confirm main PostgreSQL/Redis connectivity and the worker runtime image
   identity. Preserve stdout/stderr and process/service-manager events.
4. If the worker is gone, restart the same control-plane release. PostgreSQL
   leases and immutable patch identity recover the FIFO; do not create a new
   job or reorder the queue.

### Queue present with no running evaluator

Inspect the current job lease, retry count, three-hour budget, runner heartbeat,
and Docker labels. A recoverable infrastructure attempt retains the original
job identity and remaining total budget. Structural, build, resource, timeout,
and policy failures are terminal and must not be retried as infrastructure.

### CPU/memory bomb or stuck evaluator

Use only the exact evaluation labels and the trusted management reserve. Send
the evaluator's bounded TERM/KILL sequence, retain container inspect/cgroup
counters/stderr/partial artifacts, and verify that no labeled container or
network remains. Never run a broad Docker cleanup command. Rerun the host self
check before resuming. A three-hour bound hit fails the submission.

### Artifact archive or replication unready

Stop new admissions. `/competition/readyz` must fail closed unless the MinIO
bucket proves compliance object lock, versioning, and a server-validated enabled
replication destination. Confirm exact object versions, retention dates,
replication status/backlog, and read-back SHA-256. Do not fall back to local
disk or an unversioned bucket.

### MinIO capacity

At 75% used, forecast the unbounded paid-submission backlog and expand the
approved allocation/replica capacity. At 90%, stop admission before accepting
evidence that cannot be retained. Record the capacity check in launch/incident
evidence. No competition evidence may be deleted before its `retain_until`.
After that date, deletion still requires a ticket approved by
`support@ur.xyz` and an immutable record of the exact object versions removed.

### Credential compromise

Generate a replacement named token atomically, deploy it through the private
channel, prove authentication, revoke the compromised name, and prove the old
token receives HTTP 401. Do not rotate the seed key while any round is
unrevealed; the CLI requires explicit confirmation of that invariant.

### Suspected dishonest winner or score tampering

Do not approve or promote. Preserve the private candidate directory and write
a bounded JSON review report identifying the observed behavior. Reject through
`RUN-MAIN.sh`, which records the decision append-only and advances to the next
ranked significant candidate. If a promoted branch is suspected, stop the next
round before admission and retain all Git, score, patch, and MinIO identities;
do not rewrite published history.

### Embargo leak

Disable the leaking surface, preserve access logs and response bodies, revoke
affected credentials, and page support. The public leaderboard must include
only finalized epochs. Treat disclosed hidden seeds/workloads or candidate
scores as an integrity incident and do not silently continue the round.

## Recovery and closure

Restore PostgreSQL only with its matching retained-object generation. Redis is
a rebuildable dispatch index and must be reconstructed from authoritative
PostgreSQL FIFO order. There is no migration downgrade or moving-source
fallback. Resume only after API readiness, MinIO protection/capacity, runner
heartbeat, Grafana routing, host self-check, and source/image identities all
pass again.

Close the incident with a UTC timeline, affected round/job ids, alerts,
commands/actions, evidence version ids and hashes, credential actions, data
exposure assessment, recovery proof, and follow-up owner. Commercial incident
notification deadlines and the named backup on-call person remain external
business inputs and must be filled before public launch.
