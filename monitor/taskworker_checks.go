// Shared taskworker drain and task-plane collection used by stuck-leases and
// task-convergence (SIGNALS.md §12, TASKDRAIN1).
//
// The taskworker plane has no client connections; a healthy deploy pauses
// nothing (shared leased queue + make-before-break). These probes catch the
// unhealthy paths: claims stranded by a killed worker (§12.3), a plane that
// accumulates overdue timestamp availability (§12.4), and a task type shipped without its
// target registration (§12.4 — visible at a flat ~16s retry since the
// version-skew backoff clamp, so the error count climbs fast).
package monitor

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
//     claim_time silent > 2 minutes means its timestamp refresh is unhealthy,
//     not that its worker is dead. Session advisory ownership can outlive the
//     five-minute timestamp lease. A deploy does not rewrite
//     the claim (InitTasks never touches claims); `bringyourctl task release`
//     is available when immediate recovery is worth the operator check.
//
//   - pg/task-due-lag (12.4): the oldest timestamp-overdue task. The bounded
//     advisory-lock context separates observed owners from an unowned prefix;
//     neither timestamp lag nor an idle owner proves a fleet-wide halt.
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
		       run_max_time_seconds
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
			symptom: fmt.Sprintf("task %s timestamp keepalive silent %ss but timestamp lease held %ss more (max_time %ss) — owner liveness needs independent verification",
				task, r.str(1), r.str(2), r.str(3)),
			baseline: "a running task refreshes claim_time every ~10s; the timestamp lease expires within 5m, but a surviving session advisory lock continues to exclude duplicate ownership (12.3)",
			observed: fmt.Sprintf("silent_s=%s lease_remaining_s=%s max_time_s=%s claim_identity=withheld", r.str(1), r.str(2), r.str(3)),
			context:  "correlate with exact taskworker generation, resource pressure, advisory session heartbeat, refresh errors and deploys; timestamp expiry alone does not make an advisory-owned task claimable",
			evidence: fmt.Sprintf("timestamp lease expires in %ss, advisory ownership not established by this probe; the durable claim identifier remains available only through the protected operator lookup", r.str(2)),
			action:   "Observe timestamp expiry and independently establish session ownership. If immediate recovery is required, first prove the claiming worker is dead, then obtain the exact claim through the protected operator path and use the supported task release command without copying the identifier into an alert or transcript. Releasing a running task can permit duplicate execution.",
			verify:   "The stranded lease expires or is safely released after its owner is proven dead, one successor claims the task, and the task reaches a real terminal result without duplicate execution.",
			playbook: "SIGNALS.md 12.3",
		})
	}
	if len(strandedTasks) == 0 {
		findings = append(findings, healthyFinding("pg/task-lease-stranded", tierWarn, "task-lease-stranded", target))
	}

	// Timestamp availability is not advisory ownership or proof of claimability.
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
			symptom:  fmt.Sprintf("oldest timestamp-overdue task is %ds past available; distinguish stale owner heartbeats from unowned work", dueLagSeconds),
			baseline: "unowned due tasks normally make progress within seconds; timestamp availability can be old while an advisory session still owns the task (12.4)",
			observed: fmt.Sprintf("due_lag_s=%d", dueLagSeconds),
			context:  "one old class does not prove a global stop: inspect per-class run/claim/release times, advisory owners, fresh per-process claim/finalization rates and exact generation; future retry backoff is not due until both run_at and release_time have elapsed",
			evidence: taskDueOwnershipEvidence(ctx, env) + "\n" + taskErrorBattery(ctx, env),
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
