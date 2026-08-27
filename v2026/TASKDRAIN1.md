# TASKDRAIN1 — taskworker drain: already graceful; bound it and make it honest

How a taskworker deploy affects client connectivity (only indirectly, through
stalled task chains), why taskworker is structurally the best-positioned
drainer in the fleet (stateless workers over a shared leased pg queue, with a
real graceful drain already implemented), and the staged fixes for the
remaining failure paths: unbounded drain waits that convert to SIGKILL plus
hours-long stuck leases, a fake readiness signal that lets a bad rollout halt
the whole task plane, and a drain-window race that can abort a graceful drain.

Companion to CONNECTDRAIN2.md, PROXYDRAIN1.md, and APIDRAIN1.md, reusing the
warpctl scaffold facts documented there.

Status: IMPLEMENTED 2026-07-19 — all phases (§2.1–§2.6) landed with tests;
deviations from the plan in §7. §4/§6 kept as the design record.

## 1. How a taskworker deploy works today

### 1.1 The warpctl scaffold (same make-before-break as everywhere)

Topology: `exposed: false`, service port 80 (status only), blocks g1 and g2
(`vault/main/services.yml:336-343`). NO client traffic terminates at
taskworker — clients cannot observe a taskworker deploy at the network layer
at all. Deploy per block (`warp/warpctl/run.go:582-665`), both blocks
flipping within ~a minute of each other on a version publish:

1. New container starts on rotated internal ports; health-polled up to 120s
   on `GET /status` directly against the container (`run.go:46,1200-1292`).
   The poll parses the JSON and a `^error\s` status fails it
   (`docker.go:659-663`); a failed poll reverts the deploy (`KillWorker`,
   `run.go:595-599`) and the old container keeps running.
2. Old containers drain asynchronously: `docker update --restart=no` +
   `docker container stop -t 3600` → SIGTERM with a 60min SIGKILL ceiling
   (`DrainTimeout`, `run.go:48,2124-2141`; `cli/taskworker/Dockerfile:18`).
   Drains are serialized one-per-host by the host drain flock; a holder that
   exceeds `DrainTimeout+5min` makes the next drain proceed WITHOUT stagger
   (`host_drain_lock.go:32`, `run.go:683-696`).

### 1.2 Why deploys don't pause the task plane (the shared leased queue)

Every worker in every block claims from ONE pg queue. `takeTasks` polls
`pending_task` by `available_block <= now` ordered by priority with
`FOR UPDATE SKIP LOCKED`, then stamps `claim_time`/`release_time`
(`task/task.go:1006-1125`). `available_block` is a stored generated column =
1 + epoch of `GREATEST(run_at, release_time)` at 1s block size
(`db_migrations.go:1984-1988`, `task.go:48-50`), served by the
`pending_task_poll_order` index (`db_migrations.go:3104-3106`). Key facts:

- **Lease.** `release_time = claim + max(30s, task's declared MaxTime)` —
  deliberately covers the full declared runtime so a starved keepalive can
  never open a duplicate-claim window (the 2026-07-15 pile-up fix,
  `task.go:1090-1107`). The keepalive re-extends by 30s every ~10s with a
  GREATEST floor guard (`task.go:1255-1286`).
- **Parallelism.** 8 `Run` loops × batch 4 per container (docopt defaults;
  Dockerfile CMD passes only `-p 80`) → up to 32 concurrent tasks per
  container, 5s poll when idle (`cli/taskworker/main.go:52-84`).
- **Overlap.** The new container runs `InitTasks` and starts claiming BEFORE
  the old one is stopped, and the sibling block claims throughout. In a
  healthy deploy the task plane never has a zero-worker instant — briefly it
  has double capacity. And the queue tolerates even a zero-worker gap by
  construction: work defers, nothing is dropped.
- **InitTasks is a repair pass.** Every container start re-seeds every
  recurring chain in one tx (`taskworker/taskworker.go:16-76`) via RunOnce
  upsert: `ON CONFLICT run_at = LEAST(existing, offered)`, priority LEAST,
  max_time GREATEST (`task.go:212-243`). So (a) a chain pushed out by error
  backoff (exponential, capped 1h, `task.go:56-64,1358-1388`) is pulled back
  in at every deploy — deploys HEAL drifted chains; (b) the upsert never
  touches `claim_time`/`release_time` — a stuck lease cannot be healed by a
  deploy; (c) helpers that offer `runAt = now` (the 8 CloseExpiredContracts
  chains, `delay=false`, `taskworker.go:30-32`,
  `work/subscription_work.go:36-58`) are pulled to "run now" at every
  container start — a small synchronized burst (§2.7).

### 1.3 What the binary does on SIGTERM: a real graceful drain

`cli/taskworker/main.go:42-95`: SIGTERM/SIGQUIT → quitEvent →
`taskWorker.Drain()` → then cancel the root ctx → status server
`Shutdown(30s)` → exit (via panic if the listen call returns an error —
§2.5). `Drain()` (`task/task.go:932-936`) cancels `runCtx` (no new batches)
and waits on `runWg` for in-flight `EvalTasks` batches to complete. In-flight
task functions, the keepalive, and the finalize tx all run on contexts
derived from the ROOT ctx, not the drain ctx (`task.go:1143,1263,1303`; the
per-task session ctx derives from evalCtx — `task.go:720-725`,
`session/client_session.go:66-76`) — so SIGTERM does not abort in-flight
work; unlike proxy's `// FIXME drain`, the graceful semantics are real.
Chains re-arm via their post functions inside the finalize tx
(`task.go:1303-1430`; a post error is recorded and retried via a scheduled
`RunPost` task, so chains don't silently die).

Crash semantics (the SIGKILL path) are at-least-once by design
(`task.go:30-41`): a killed worker's claims sit until `release_time` then
re-run in full; a task that completed its side effects but died before the
finalize tx re-runs entirely; a task that RETURNS an error is rescheduled
with jitter + backoff and — important for §2.1 — `release_time = now`
(`task.go:1379`): the error path already hands the claim back immediately.
Only silent death waits out the lease.

### 1.4 What a client experiences

Directly: nothing, ever — there is no client connection to drop. The
coupling is through chains whose stall degrades connectivity inputs:

| chain | cadence | MaxTime | stall cost to clients |
|---|---|---|---|
| CloseExpiredContracts ×8 slices | continuous when busy, else 1–5min (`subscription_work.go:36-93`) | 30min | expired open contracts stay unsettled → escrow stays reserved → "insufficient balance" contract denials for high-throughput networks |
| CloseExpiredNetworkClientHandlers | 60s (`network_client_work.go:20-49`) | 15min | crashed/killed CONNECT handlers' clients linger `connected=true` (`model/network_client_model.go:2738-2788`) → dead candidates linger in the provider pool |
| RollupClientReliabilityStats | 1min (`network_client_reliability_work.go:256-288`) | 15min | redis reliability buffer backs up; >10min lag flips `ClientReliabilityRollupSynced` false (`model:1010-1037`) which pauses score/window updates (deliberate self-protection, `reliability_work.go:218-223`) |
| UpdateClientScores | 30s (`network_client_location_work.go:20-43`) | 120min | matchmaking scores freeze (redis, ttl 300min — stale-but-served) |
| UpdateReliabilities | 30min (`reliability_work.go:173-242`) | 120min | location reliabilities go stale — including the `network_client_location_reliability.connected = true` candidate filter FindProviders2 selects against (`model/network_client_location_model.go:1478,2409,2474`, flip logic `model/network_client_reliability_model.go:2472-2546`); new-provider warmup stops progressing |
| ReconcileNetEscrow | 5min (`network_client_work.go` sibling, `subscription_work.go:250-295`) | 30min | redis netEscrow drift accumulates → spurious insufficient-balance |

The rest (Payout weekly/6h max, ProcessPendingPayouts 1h max, DbMaintenance
daily 03:00/07:00 UTC/24h max epoch chain, retention + orphan sweeps
weekly/4h, StSyncChain 1min, ExportProvidersMap 5min) is money/retention/
telemetry — no connectivity coupling, but their MaxTimes matter for §2.1's
lease math.

Note the cross-service dependency: the handler reap is what cleans up after
CONNECT deploys and crashes. A fleet-wide rollout cycles taskworker at the
same time as connect — the reap chain keeps the provider pool truthful
during exactly that churn, which is why its continuity across taskworker's
own deploy matters.

## 2. The gaps (ranked by expected client impact)

### 2.1 P0 — drain waits unboundedly on long tasks; overrun converts to SIGKILL plus hours-long stuck leases

`Drain()` has no deadline: it waits for every in-flight task to return, and
several tasks legitimately run long — DbMaintenance (MaxTime 24h, runs
nightly for potentially hours), UpdateClientScores and UpdateReliabilities
(120min; 20+min recomputes observed in prod, `task.go:1094-1103`), Payout
(6h), the weekly sweeps (4h). A deploy that catches one of these mid-run:

- holds the host drain flock for up to the 60min docker ceiling, degrading
  every other service's drain stagger on that host (the CONNECTDRAIN2
  28-minute lesson, doubled);
- at 3600s docker SIGKILLs the process, and EVERY in-flight task (up to 32)
  keeps its lease until `claim + max(30s, MaxTime)`: a CloseExpiredContracts
  slice pauses ≤30min (⅛ of the close pipeline each), UpdateClientScores
  freezes ≤2h, a killed DbMaintenance pins its RunOnce chain ≤24h — likely
  skipping the next night's maintenance window entirely. Nothing can unstick
  them: InitTasks doesn't touch claims (§1.2), and `bringyourctl` has no
  release command (§2.6).

Fix — bounded drain with cancel-to-reschedule. The mechanism is ~90% built:

1. `runCancel` as today; wait `DrainFinishTimeout` (~60–120s) for batches to
   finish naturally (the overwhelming common case — most tasks run seconds).
2. Then cancel the straggler task session contexts: plumb a worker-level
   drain ctx into the RunSpecific timeout goroutine (`task.go:729-739`) as a
   third case beside maxTime — the session cancel aborts the task's db work
   (everything is ctx-threaded) and the function returns an error.
3. That error flows through the EXISTING reschedule path: `run_at ≈ now +
   jitter + backoff step`, **`release_time = now`** (`task.go:1358-1388`) —
   the claim is handed back immediately and the new container (or sibling
   block) re-runs the task within seconds. No new persistence code. The
   singleton-lease invariant holds because handback happens only via the
   normal path AFTER the function returned.
4. A function that ignores its ctx keeps its lease and rides to SIGKILL —
   unchanged, but now the exception instead of the norm. Log it.
5. Exit the instant the drain completes (already the behavior).

Polish: tag drain cancels with a distinct error (like the existing
`Timeout`) and skip the `reschedule_error_count` increment for them, so
deploys never push a healthy chain toward the 1h backoff cap. At-least-once
note: this makes partial-execution re-runs more common; task bodies are
already required to tolerate that (the crash path exists today), and the
chunked-budget idiom used across the work files bounds repeated work.

### 2.2 P1 — `/status` is a constant "ok": a bad rollout halts the task plane silently

`collectStatus` returns "ok" unconditionally (`router/warp_handlers.go:69-91`
— same defect as APIDRAIN1 §2.1). InitTasks gives an implicit pg gate (a
container that cannot reach pg panics before listening → poll times out →
revert), but redis is ungated: both blocks flip to a redis-broken version
within a minute, the poll passes, the old containers stop, and every
redis-coupled chain (rollup 1min, scores 30s, netEscrow reconcile 5min)
errors into exponential backoff. The plane looks deployed-ok while
reliability data stops flowing and the redis buffer grows; the broken window
is unbounded until a human notices. (Recovery after the fix deploy is fast —
InitTasks pulls the chains back in, §1.2 — the cost is the window itself.)

Fix: the same latched readiness as APIDRAIN1 §2.1 — one-shot pg `SELECT 1` +
redis `PING` after InitTasks and before listen; `/status` serves the latch,
plus `error: draining` after SIGTERM (observability only). A failed check
keeps the container un-ready → poll times out → deploy reverts → old
containers keep the plane running. Zero warpctl changes; ideally the same
`collectStatus` implementation the api P0 work produces.

### 2.3 P2 — the drain-window WaitGroup reuse race can abort a graceful drain

The main.go worker loops re-enter `taskWorker.Run()` every 1s until the root
ctx cancels (`cli/taskworker/main.go:71-84`); `Run()` calls `runWg.Add(1)`
unconditionally (`task.go:897`); `Drain()` runs `runCancel` +
`runWg.Wait()`. At the drain tail a re-entering `Run` can `Add` from a zero
counter concurrently with `Wait` — the documented WaitGroup misuse, which
panics. `HandleError` recovers it (`trace.go:45-69`), but in the drain
goroutine the deferred `cancel()` then shuts the process down while sibling
workers may still be mid-task: a graceful drain silently degrades into a
partial hard kill with §2.1's stuck-lease costs. Narrow window, trivially
closed: check `runCtx` before `Add` in `Run()` and give the outer loop a
terminal condition (e.g. `Run` returns a "draining" bool) so it stops
re-entering after drain starts.

### 2.4 P2 — version-skew: Target-not-found churn is safe but backoff-shaped

During the deploy overlap, old workers can claim task types that only the
new version registers → "Target not found." → error-reschedule with
exponential backoff (`task.go:1215,1358-1388`). A repeatedly-claimed new
chain can be pushed minutes out during a long overlap — and §2.1's long
drains lengthen exactly that overlap. (The inverse case is handled well:
retired task types keep registered targets with no-op schedule/post so stale
rows finish cleanly — `work/api_work.go`, `network_client_reliability_work.go:91-163`
— and renames are covered by `alternateFunctionNames`.) Fix: don't apply the
exponential step to Target-not-found errors (flat ~2s retry, optionally
capped-flat) — one branch in the reschedule write. The two-release
discipline (ship targets one release before scheduling them) remains good
hygiene but shouldn't be load-bearing.

### 2.5 P3 — exit path and observability

- Listen error → panic (`cli/taskworker/main.go:126-128`) — same APIDRAIN1
  §2.2 fix: treat drain-deadline shutdown as a logged outcome, exit cleanly.
- No drain or queue metrics exist. Add: `urnetwork_taskworker_ready` (0/1 +
  reason), `urnetwork_taskworker_drain_seconds`, `_drain_inflight` (tasks at
  SIGTERM), `_drain_canceled` (stragglers cancel-rescheduled — should be ~0),
  and queue health: oldest-due lag (now − min available time over due
  tasks), error-rescheduled count (`ListRescheduledTasks` already exists,
  `task.go:444-468`), rollup lag (`client_reliability_rollup.update_time`
  age vs the 10min gate), redis reliability buffer depth. A drain summary
  log line (finished/canceled/leases-left). `monitor/SIGNALS.md` entry:
  post-deploy, oldest-due lag returns to ~0 within ~1min and rollup age
  stays <10min; `drain_canceled > 0` on a quiet deploy or any stuck lease =
  investigate.

### 2.6 P3 — ops tooling for stuck leases

`bringyourctl task` has only `ls`/`rm` (`bringyourctl/main.go:79-80`). After
a SIGKILL or crash the only remedies for a stuck claim are waiting out
`release_time` or hand-SQL. Add `task release <task_id>` (set
claim/release → now, making it immediately claimable) and `task kick
<run_once_key>` (pull `run_at` → now) for chain-level nudges.

### 2.7 Explicitly not problems (audited, leave alone)

- Deploys pausing task processing: no — shared queue + overlap (§1.2); the
  plane's "clients" are cadences, not connections.
- The InitTasks pull-to-now of the 8 CloseExpiredContracts slices at every
  container start: a bounded burst (each run caps its id scan and re-paces
  via the `Full` result), and doubles as a close-pipeline liveness check at
  every deploy.
- At-least-once re-runs after a crash: the framework contract
  (`task.go:30-41`); task bodies are written idempotent and chunked.
- finished/post atomicity: finalize + post share one tx; post errors are
  recorded and retried via `RunPost`; `TaskCleanup` keeps post-error rows 7
  days (`task.go:537-561,1391-1428`).
- The rollup-stale gate pausing score updates during a backlog
  (`reliability_work.go:218-223`): deliberate — scores wait for truth
  rather than compute over drifted windows. Don't "fix" it; surface it
  (§2.5 rollup-lag metric).
- Two-block topology with concurrent flips: fine — drains serialize via the
  flock and the queue absorbs any brief capacity dip.

## 3. Hazard review

| Concern | Answer |
|---|---|
| Drain-cancel makes partial executions more common — do all task bodies tolerate it? | They already must: the SIGKILL/crash path re-runs full tasks today, and error-reschedule re-runs failed ones. Cancel aborts between ctx-checked db operations, and the chunked-budget idiom bounds repeated work. Audit any task doing non-transactional external side effects (payments are idempotency-keyed already). |
| Early lease handback re-opens the 2026-07-15 duplicate-claim window | No: handback happens only via the existing reschedule write AFTER the function returned. A hung function keeps its lease exactly as today. |
| Canceling DbMaintenance mid-operation corrupts maintenance state | pg aborts the in-flight statement on ctx cancel; the epoch chain re-runs the same epoch next window. Same behavior as today's MaxTime ctx-cancel, just triggered earlier. |
| Flat retry for Target-not-found hot-loops on a permanently missing target | Cap it (flat for the first N, then backoff) or accept ~2s cadence noise that `ListRescheduledTasks` + the §2.5 metric make visible. A permanently missing target is a registration bug to page on, not to hide behind a 1h backoff. |
| Readiness latch blocks all deploys when redis is down fleet-wide | Correct behavior — identical to the APIDRAIN1 §3 answer: poll timeout → revert → old containers keep the plane running; the latch converts a silent halt into a failed deploy. |
| DrainFinishTimeout + cancel wait slows rollouts | Bounds them: worst case ~2–3min per block vs today's unbounded-to-60min ride to SIGKILL. The flock stagger is preserved instead of timing out. |
| Two taskworker versions running different task-type sets during overlap | Already the steady state of every deploy; RunOnce upserts are idempotent across versions, and §2.4 removes the backoff penalty from the residual churn. |

## 4. Rollout

1. **P2 WaitGroup guard + P3 clean exit** (`task/task.go`,
   `cli/taskworker/main.go`) — pure correctness, ships first.
2. **P0 bounded drain** — `DrainFinishTimeout`/`DrainCancelTimeout` on
   `TaskWorkerSettings` (no globals, per convention), drain ctx into the
   RunSpecific timeout select, drained-error tag + backoff skip, drain
   summary log. Verify: deploy during a deliberately slow task → task is
   cancel-rescheduled, claim handed back, re-run completes on the new
   container; process exit well under the flock timeout.
3. **P1 readiness latch** (`router/warp_handlers.go` shared with the api P0
   work + one-shot checks in `cli/taskworker/main.go`) — verify a
   redis-broken container on one block times out the poll and reverts.
4. **P3 observability** (`grafana.go` counters + `monitor/SIGNALS.md`) and
   **P3 bringyourctl release/kick** — with or after (2).
5. **P2 Target-not-found flat retry** — anytime.

Tests: extend `task/task_test.go` — drain finishes an in-flight batch; drain
cancels a straggler and hands the claim back (`release_time ≈ now`, no
backoff bump, re-claimable immediately); Run-vs-Drain under `-race`;
Target-not-found flat retry. An e2e in the family of
`connect/exchange_drain_e2e_test.go`: SIGTERM with a slow task → bounded
exit, chain continuity on the surviving worker.

## 5. Implementation inventory (files)

- `task/task.go`: drain ctx plumbed into `RunSpecific`/`RunPost` timeout
  selects; phased `Drain()`; drained-error tag + backoff skip in the
  reschedule write; `runCtx` guard in `Run()`; `TaskWorkerSettings` fields.
- `cli/taskworker/main.go`: terminal worker-loop condition; one-shot
  readiness checks post-`InitTasks` pre-listen; drain summary; no panic on
  shutdown error.
- `router/warp_handlers.go`: latched readiness + draining state (shared
  implementation with APIDRAIN1 §2.1).
- `bringyourctl`: `task release <task_id>`, `task kick <run_once_key>`.
- `grafana.go` / `monitor/SIGNALS.md`: queue-health + drain metrics, alert
  thresholds.
- `../warp`: no changes — the scaffold (overlap, poll+revert, flock, 60min
  grace) is already correct for taskworker.

## 6. Open decisions (resolved where noted)

1. `DrainFinishTimeout` default — RESOLVED: 60s (with `DrainCancelTimeout`
   30s), the low end; the §2.5 drain metrics can revisit after a few
   deploys.
2. Drain-cancel requeue shape — RESOLVED: reuse the error-reschedule write,
   now parameterized per error class (count delta + exponent clamp); no new
   SQL path.
3. Whether DbMaintenance should move from MaxTime 24h to a per-run budget
   with epoch resume (like the retention loops), so its lease can never pin
   a chain for a day — STILL OPEN; less urgent now that a graceful drain
   cancels it cleanly (the 24h pin needs a SIGKILL/crash), but worthwhile.
4. Whether readiness should include a queue self-test — RESOLVED: no;
   pg+redis pings only.

## 7. Implementation notes (2026-07-19 — deviations from the plan)

1. **Drain-cancel plumbing** landed at the EvalTasks layer, not inside
   RunSpecific's timeout select: each task function runs on a per-task
   context derived from evalCtx plus `context.AfterFunc(drainCtx, cancel)`
   (`task.go` eval goroutine). No Target interface change; the eval/finalize
   machinery stays on the root ctx, so canceled tasks flow the NORMAL
   finish/reschedule paths and nothing hits the "Task not run." fallback.
2. **Drained reschedules** are tagged `Drained: <err>` (`ErrDrained`
   sentinel) and take error-count delta 0 + backoff exponent clamp 0 in the
   (now per-class parameterized) reschedule write — flat ~RescheduleTimeout
   retry, claim released via the existing `release_time = now`.
3. **Target-not-found** (`ErrTargetNotFound`, preserved through RunPost's
   `%w` wraps) keeps incrementing the count (visibility) but clamps the
   exponent to 3: retries converge to ~16s. Chosen over pure-flat 2s (two
   build generations could ping-pong a claim at full poll speed) and over
   flat-first-N (N would be consumed within seconds of a long overlap).
4. **Post closures use `context.WithoutCancel`**: a function that completes
   successfully after the drain cancel still records finished and still
   re-arms its chain in the finalize tx. This also fixes a pre-existing
   race where a task succeeding exactly at MaxTime could have its post
   severed by the timeout cancel.
5. **The WaitGroup race** is closed inside the worker (`enterRun` guard
   under the state lock; `Run` returns immediately once draining). The
   main worker loops intentionally keep their 1s spin during drain — their
   deferred `cancel()` must not fire before `Drain` returns, so giving them
   a terminal condition would have been wrong (§2.3's suggestion revised).
6. **Draining status is `"draining"`, not `"error: draining"`**: at audit
   time warpctl's IsError regex was `^(?i)error\s` (the colon form was
   silently ignored); the APIDRAIN1 session has since widened it to
   `^(?i)error(\s|:)`. Under both, `"draining"` is deliberately non-error
   (drains must not count as errors in fleet status sampling) and
   `"error not ready: ..."` matches, holding the deploy poll to
   timeout+revert (contract pinned by
   `router/warp_handlers_status_test.go`, shared with the APIDRAIN1 work).
7. **Readiness ordering**: the one-shot pg+redis checks run BEFORE
   InitTasks; on failure the process stays up serving the error status with
   NO workers started (exiting would flap the container without ever
   producing the truthful status).
8. **Dead flag fixed**: `--batch_size` was parsed into settings that never
   reached the worker (`InitTaskWorker` ignored them); it now takes a
   settings argument.
9. **Metrics**: 4 gauges in `cli/taskworker/main.go`
   (`urnetwork_taskworker_ready/drain_inflight/drain_seconds/
   drain_canceled`) + `StartStatsPusher` wired (taskworker previously
   pushed nothing). Queue-health signals are monitor-side SQL probes in
   `monitor/SIGNALS.md` §12, not service metrics.
10. **Ops tooling**: `task.ReleaseTask` / `task.KickTasks` behind
    `bringyourctl task release <task_id>` / `task kick <run_once_key>`
    (kick matches both the raw helper key and the stored json-encoded
    form).
11. **Family follow-through (later on 2026-07-19)**: the one-shot readiness
    check moved into a shared `router.StartupReadiness` (check + latch);
    connect and taskworker use it (connect serves ONLY /status and hosts no
    exchange when not ready, and flips draining before `Exchange.Drain()`),
    while api uses its own equivalent `api.ReadinessCheck` (the api package
    cannot be imported from router — cycle; the check bodies are identical
    and could merge later). The §2.5 monitor signals are now real probes:
    `monitor/probe_taskworker_drain.go` (`pg/task-lease-stranded`,
    `pg/task-due-lag`, `pg/task-target-missing`, 60s cadence) plus the
    `logs/taskworker-drain-gave-up` tailer class — SIGNALS.md §12 annotated
    with the probe ids.

Tests (`task/drain_test.go`, `router/warp_handlers_status_test.go`, all
under `-race`):

- `TestTaskWorkerDrainFinishesInFlight` — natural finish: task completes,
  post runs, nothing canceled; post-drain `Run` returns immediately.
- `TestTaskWorkerDrainCancelsStragglerAndHandsBackClaim` — phase-2 cancel:
  `Drained:` reschedule, error count 0, claim released, bounded drain, and
  a second worker re-runs the task promptly.
- `TestTaskWorkerDrainCompletedTaskStillPosts` — success after cancel still
  finishes + re-arms the chain (guards the WithoutCancel post fix).
- `TestTaskWorkerDrainBoundedWhenTaskIgnoresContext` — phase-3: drain gives
  up on schedule; the straggler keeps its lease.
- `TestTaskWorkerRunDrainRace` — 20 rounds of 8 main-style re-entering Run
  loops vs Drain under `-race`; guards the WaitGroup reuse fix.
- `TestTargetNotFoundFlatRetry` — 6 consecutive errors stay under the ~16s
  clamped ceiling while the count advances.
- `TestReleaseAndKickTask` — stranded claim is unclaimable → release makes
  it claimable per run_at → kick pulls run_at to now → task runs.
- `TestWarpStatusLatch` — status latch vs the mirrored warpctl IsError
  regex: default/ready "ok", not-ready matches (deploy poll fails),
  draining does not.
- Existing `TestTask` (stress), `TestTaskRescheduleErrorBackoff`, and
  `TestTaskLeaseNotShortenedByKeepalive` unchanged and passing — the normal
  reschedule path (delta 1, clamp 24) is bit-identical to the previous SQL.
