// Taskworker drain / task-plane liveness probes (SIGNALS.md §12, TASKDRAIN1).
//
// The taskworker plane has no client connections; a healthy deploy pauses
// nothing (shared leased queue + make-before-break). These probes catch the
// unhealthy paths: claims stranded by a killed worker (§12.3), a plane that
// stopped claiming due work (§12.4), and a task type shipped without its
// target registration (§12.4 — visible at a flat ~16s retry since the
// version-skew backoff clamp, so the error count climbs fast).
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// taskworkerDrainProbe emits three finding classes against the pg primary:
//
//   - pg/task-lease-stranded (12.3): a claimed task whose keepalive went
//     silent. While a task runs, its worker refreshes claim_time every
//     ~ReleaseTimeout/3 (10s); a claim with a future release_time and a
//     claim_time silent > 2 minutes means the claiming worker is gone
//     (SIGKILL, crash) and the task — and its RunOnce chain — is blocked
//     until claim + max time passes. A deploy cannot heal it (InitTasks
//     never touches claims); `bringyourctl task release` can.
//
//   - pg/task-due-lag (12.4): the oldest due-and-unclaimed task. Healthy
//     workers claim due tasks within seconds; a growing lag means no worker
//     is claiming (both blocks broken, crash loop) — the silent-halt mode
//     the readiness latch narrows but cannot fully remove.
//
//   - pg/task-target-missing (12.4): a task erroring `Target not found` far
//     beyond any deploy overlap. During an overlap this is normal version
//     skew (old workers claiming new task types) and retries flat; a count
//     past ~100 (>25 min of flat retries) on a settled fleet is a missing
//     target registration — a code bug that no longer hides behind the
//     backoff cap.
type taskworkerDrainProbe struct{}

func (self taskworkerDrainProbe) id() string             { return "pg/task-lease-stranded" }
func (self taskworkerDrainProbe) tier() string           { return tierWarn }
func (self taskworkerDrainProbe) cadence() time.Duration { return 60 * time.Second }

func (self taskworkerDrainProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	findings := []finding{}

	// stranded leases: claim held into the future with a silent keepalive
	strandedRows, err := env.runner.pg(ctx, `
		SELECT split_part(function_name,'.',3) AS task,
		       round(extract(epoch FROM now()-claim_time))::int AS silent_s,
		       round(extract(epoch FROM release_time-now()))::int AS lease_remaining_s,
		       run_max_time_seconds,
		       task_id
		FROM pending_task
		WHERE now() < release_time
		  AND claim_time < now() - interval '2 minutes'
		ORDER BY lease_remaining_s DESC LIMIT 10;
	`)
	if err != nil {
		return nil, err
	}
	strandedTasks := map[string]bool{}
	for _, r := range strandedRows {
		task := r.str(0)
		strandedTasks[task] = true
		findings = append(findings, finding{
			probeId: "pg/task-lease-stranded", tier: tierWarn,
			class: "task-lease-stranded", target: target, frame: task, sustain: 2,
			symptom: fmt.Sprintf("task %s claim keepalive silent %ss but lease held %ss more (max_time %ss) — claiming worker gone; the task and its chain are blocked until release",
				task, r.str(1), r.str(2), r.str(3)),
			baseline: "a running task refreshes claim_time every ~10s; silence > 2m with a live lease = stranded by a kill/crash (12.3)",
			observed: fmt.Sprintf("silent_s=%s lease_remaining_s=%s max_time_s=%s task_id=%s", r.str(1), r.str(2), r.str(3), r.str(4)),
			context:  "correlate with taskworker deploys/kills; a cpu-starved extender can rarely mimic this — verify the claiming worker is really gone before releasing (a release of a RUNNING task re-opens the duplicate-execution window)",
			evidence: fmt.Sprintf("recovery: bringyourctl task release %s   (then task kick <run_once_key> if the run_at is far out)", r.str(4)),
			playbook: "SIGNALS.md 12.3",
		})
	}
	if len(strandedTasks) == 0 {
		findings = append(findings, healthyFinding("pg/task-lease-stranded", tierWarn, "task-lease-stranded", target))
	}

	// due-lag: oldest due-and-unclaimed task (workers claim within seconds)
	lagRows, err := env.runner.pg(ctx, `
		SELECT coalesce(max(round(extract(epoch FROM now() - greatest(run_at, release_time)))),0)::int
		FROM pending_task
		WHERE greatest(run_at, release_time) <= now();
	`)
	if err != nil {
		return findings, err
	}
	dueLagSeconds := atoiRow(lagRows[0], 0)
	if dueLagSeconds > 180 {
		findings = append(findings, finding{
			probeId: "pg/task-due-lag", tier: tierWarn,
			class: "task-due-lag", target: target, sustain: 2,
			symptom:  fmt.Sprintf("oldest due-and-unclaimed task is %ds past available (healthy: seconds) — the task plane is not claiming", dueLagSeconds),
			baseline: "due tasks are claimed within seconds by any live worker in either block; transient spikes during a deploy overlap are normal (12.4)",
			observed: fmt.Sprintf("due_lag_s=%d", dueLagSeconds),
			context:  "if this grows after a taskworker deploy: check the deploy reverted (error not ready) or the workers crash-looped; contract close, handler reap, and reliability rollup are all stalling while this is red",
			evidence: taskErrorBattery(ctx, env),
			playbook: "SIGNALS.md 12.4",
		})
	} else {
		findings = append(findings, healthyFinding("pg/task-due-lag", tierWarn, "task-due-lag", target))
	}

	// target-not-found persistence: far beyond any deploy overlap
	missingRows, err := env.runner.pg(ctx, `
		SELECT split_part(function_name,'.',3) AS task,
		       reschedule_error_count
		FROM pending_task
		WHERE reschedule_error LIKE '%Target not found%'
		  AND reschedule_error_count > 100
		ORDER BY reschedule_error_count DESC LIMIT 5;
	`)
	if err != nil {
		return findings, err
	}
	if len(missingRows) > 0 {
		lines := []string{}
		for _, r := range missingRows {
			lines = append(lines, fmt.Sprintf("  %s errors=%s", r.str(0), r.str(1)))
		}
		findings = append(findings, finding{
			probeId: "pg/task-target-missing", tier: tierWarn,
			class: "task-target-missing", target: target, frame: missingRows[0].str(0), sustain: 2,
			symptom:  fmt.Sprintf("%d task(s) erroring `Target not found` past 100 retries — beyond any deploy overlap; a task type shipped without its target registration", len(missingRows)),
			baseline: "target-not-found is normal only during a deploy overlap (version skew, flat ~16s retry); it must clear once the fleet settles on one build (12.4)",
			observed: fmt.Sprintf("tasks=%d top_errors=%s", len(missingRows), missingRows[0].str(1)),
			evidence: "tasks with a missing target:\n" + strings.Join(lines, "\n"),
			playbook: "SIGNALS.md 12.4",
		})
	} else {
		findings = append(findings, healthyFinding("pg/task-target-missing", tierWarn, "task-target-missing", target))
	}

	return findings, nil
}
