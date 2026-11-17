# Idle host liveness ownership

An idle worker's authenticated host refresh has its own cancellable child.
Runtime/source checks, round/finish reads, physical availability and claim waits
cannot prevent that child from servicing the existing heartbeat cadence. Each
refresh still performs a complete new self-check followed by actual host
registration. Reachability, a cached check or a job-lease ACK is not new health.

The child keeps its latest report and conservative registration expiry private.
Run cancels and physically joins it before evaluateOne, then transfers the last
fully acknowledged state to the unchanged per-job owner. A genuinely reserved
legacy baseline producer performs the same private transfer before starting its
own observer. Merely configuring that role or polling an already-warm baseline
does not reset the idle ticker. The old direct claim helper uses a nil transfer
hook and retains its existing behavior and reservation cleanup.

An idle health failure cancels only that worker's runtime/admission context;
other host workers retain their independent parents. Unknown or canceled checks
do not publish new reports. A completed failed check may withdraw a host only
when its unmodified report is ordinarily ineligible. A report that is ordinarily
eligible but fails separate private-probe/integrity provenance cannot be
registered as a positive heartbeat by a failure handler; the worker fails and
the earlier row expires under the existing freshness bound. This same rule
applies to startup and post-job failure handlers.

Healthy request, database and evaluation budgets are unchanged. Only the new
failed-check invalidation cleanup has a finite independently owned eight-second
budget; it cannot publish positive eligibility. Cancellation joins the exact
self-check and database owner before returning. A late nil database return after
cancellation cannot advance the local confirmed bound. No work item, native run,
source/round policy, lease, heartbeat max-age or original execution clock is
changed. If health fails after a claim, this code does not manually clear or
terminalize that job; ordinary fenced same-attempt reconciliation remains its
owner and retains the original deadline.

Each transport exchange already owns a distinct process, request, response and
all manual pipe ends. Concurrent idle self-check and read-only availability do
not share mutable transport/evaluator state or issue start/cancel/artifact
operations. Per-job evaluation and baseline production begin only after the
idle child joins. PostgreSQL calls retain their own outer connection boundaries.

Fifteen deterministic synthetic tests use genuine Worker.Run entry points,
source/runtime-shaped fixtures, exact callback barriers and virtual ticker
edges. They cover blocked availability/runtime/finish reads; sibling isolation;
the real private-probe rejection at startup/idle/post-job; self-check/database
cancel and physical joins; conservative late ACKs; the latest report at
evaluation; warm-baseline polling; actual producer transfer and cleanup; finite
negative invalidation; and a completed-looking canceled report. The fast causal
oracle fails the old serial Run at a specific starved-heartbeat assertion after
synctest quiescence, without waiting for a watchdog timeout.

This source-only cut is not deployment authorization or proof of the operation
that caused any observed host-freshness interval. Independent ordinary/race,
historical causal control and adjacent baseline/heartbeat/transport tests remain
qualification gates. No database migration, API release or evaluator image/run
change is part of this worker-only scheduling correction.
