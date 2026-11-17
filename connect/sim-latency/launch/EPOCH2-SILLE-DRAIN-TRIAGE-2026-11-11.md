# Epoch 2: Sille admission drain-timeout triage

Goofe has a retained passing full-R9 admission. Sille does not. The corrected
coordinator authenticated both original executions and retained their evidence,
without rerunning measurements or extending their clocks. Customer scoring is
still blocked by the frozen requirement that both hosts pass admission.

## Established facts

- Sille's trusted comment-only no-op completed all nine replicates. Replicate 3
  alone has `unexpected_recovery`; its other six scoring gates pass. Its measured
  sample has 3,044 rows and zero failures, and its final expected-page and
  expected-byte coverage are both 1. These facts do not waive G5.
- The measurement window ended at Unix millisecond `1794359872644`. At
  `2026-11-11T01:18:36.931686Z`, 44.287 seconds later, the product reported a
  PostgreSQL cached-statement deallocation read timeout during connection
  announcement's final disconnect. The run completed at Unix millisecond
  `1794359919847`.
- The per-run resource report is complete, with exit code 0, no OOM, no hard
  kill, no containment escape and no missing measurement. Completion and
  resource cleanliness are not substitutes for a passing stability gate.
- The error precedes the fleet's drain messages. It must not simply be labeled
  harmless activity after the entire run ended.
- No PostgreSQL server log or query trace appears in the retained raw inventory.
  The client stack identifies the failed operation, but does not establish why
  its database response timed out.

## Adjacent ownership and lifecycle checks

These are source inspections, not a claim that additional tests ran:

- `server/connect/sim-latency/run.go:675` waits for the admitted client-driver
  work before the final cancellation at line 687. The measurement boundary
  does not cancel admitted crawls immediately.
- `server/connect/sim-latency/services.go:441` closes handlers and joins their
  lifetimes before its final registration cleanup; line 470 joins the service
  workers before that cleanup. The existing barrier-based test in
  `services_lifecycle_test.go` exercises that ordering. These simulator files
  are unchanged between the frozen server checkpoint and the coordinator's
  qualified `fc55a2de4` source.
- The frozen connection-announcement code joins its child workers, then runs
  detached, bounded final disconnect cleanup. That final cleanup remains a
  product database operation, not evidence that PostgreSQL itself was stopped
  early by the harness.
- Current main vendors pgx, whereas the frozen checkpoint uses pgx v5.10.0
  from its module cache. The deallocation implementation was therefore also
  inspected directly inside evaluator image
  `sha256:5e5dfa2715c980d7ec037b6f15208e1d240a6bc80668aabc3335c1f71b2b92d9`,
  with a read-only, network-disabled, capability-dropped container restricted
  to management CPUs. That implementation passes the caller's context to
  `StartPipeline`, sends the invalidated statement deallocations and reports
  errors from `Sync`/`Close`; it does not establish a separate deallocation
  timeout in that function. The diagnostic container was removed on exit.

## What is not justified

There is not yet a deterministic reproduction supporting a product-code fix,
an early-PostgreSQL-shutdown claim, or a stability-gate relaxation. No cleanup
error was silenced, no replicate was discarded, and no frozen source, image,
baseline or scoring rule was changed to make this sample pass.

The separate confirmed coordinator defect was a strict source decoder missing
the emitted `work_coverage_policy` leaf. It is fixed and deployed; its regression
reproduced the pre-fix rejection, and both ordinary and race source-authentication
tests passed three times on the final qualified tree. Real evidence from both
hosts subsequently passed that boundary. This does not fix or excuse Sille's
independent G5 result.

## Recovery boundary

Both original three-hour admission clocks are now past their deadlines. A new
Sille admission requires an explicitly approved, append-only recovery identity
and reviewed authority; do not overwrite its original reservation. The pending
operator choice is whether to continue root-cause diagnosis first or authorize
one controlled replacement full-R9 qualification with the original failure,
shared baseline and every gate preserved. A passing replacement alone would
not prove that the original timeout's root cause was fixed.

See [the original failure](EPOCH2-PRIVATE-ADMISSION-BLOCKER-2026-11-11.json) and
[Goofe's retained R9 admission](EPOCH2-GOOFE-R9-ADMISSION-2026-11-11.json).
