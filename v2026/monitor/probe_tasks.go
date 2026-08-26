// Task-system probes: the end-to-end canary and parked/failing recurring
// tasks (SIGNALS.md 1.2).
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// taskCanaryProbe is SIGNALS.md 1.2: the cheapest end-to-end redis probes.
// UpdateClientLocations runs ~every 30s and writes redis across many slots; if
// redis is sick anywhere on the write path it errors within a minute. It also
// reports parked tasks (the exponential-backoff gotcha: a quiet failing task
// is indistinguishable from a healthy one unless run_at is checked).
type taskCanaryProbe struct{}

func (self taskCanaryProbe) id() string             { return "pg/canary-dead" }
func (self taskCanaryProbe) tier() string           { return tierPage }
func (self taskCanaryProbe) cadence() time.Duration { return 60 * time.Second }

func (self taskCanaryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	findings := []finding{}

	// canary completions in the last 3 minutes (healthy 12–25, broken 0)
	rows, err := env.runner.pg(ctx, `
		SELECT count(*) FROM finished_task
		WHERE function_name LIKE '%UpdateClientLocations%'
		  AND run_end_time > now() - interval '3 minutes';
	`)
	if err != nil {
		return nil, err
	}
	completions := atoiRow(rows[0], 0)
	if completions == 0 {
		findings = append(findings, finding{
			probeId: "pg/canary-dead", tier: tierPage,
			class: "canary-dead", target: target, sustain: 1,
			symptom:  fmt.Sprintf("UpdateClientLocations completions in last 3m = 0 (healthy 12–25) on %s", target),
			baseline: "12–25 completions / 3 min; 0 = redis sick somewhere on the write path (1.2)",
			observed: "locations_completions_3m=0",
			evidence: taskErrorBattery(ctx, env),
			playbook: "SIGNALS.md 5.1",
		})
	} else {
		findings = append(findings, healthyFinding("pg/canary-dead", tierPage, "canary-dead", target))
	}

	// overdue-but-claimed tasks (warn): a live keepalive (claim_time
	// refreshing) with run_at far in the past and the run stretching beyond
	// 2x the function's 7-day p95 duration — the "long-running vs stuck"
	// signature (1.2) that parked-detection misses because error_count may be
	// low and run_at is past, not future. Found the 2026-07-17
	// UpdateClientScores 2.5h grind that froze provider selection.
	overdueRows, err := env.runner.pg(ctx, `
		WITH history AS (
			SELECT split_part(function_name,'.',3) AS task,
			       percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM run_end_time-run_start_time)) AS p95_s
			FROM finished_task
			WHERE run_end_time > now() - interval '7 days'
			GROUP BY 1 HAVING count(*) >= 10
		)
		SELECT p.task, p.overdue_s, round(coalesce(h.p95_s,0))::int
		FROM (
			SELECT split_part(function_name,'.',3) AS task,
			       round(extract(epoch FROM now()-run_at))::int AS overdue_s
			FROM pending_task
			WHERE run_at < now() - interval '10 minutes'
			  AND claim_time > now() - interval '2 minutes'
		) p
		LEFT JOIN history h USING (task)
		WHERE p.overdue_s > greatest(2*coalesce(h.p95_s, 1800), 1200)
		ORDER BY p.overdue_s DESC LIMIT 10;
	`)
	if err != nil {
		return findings, err
	}
	overdueTasks := map[string]bool{}
	for _, r := range overdueRows {
		task := r.str(0)
		overdueTasks[task] = true
		findings = append(findings, finding{
			probeId: "pg/task-overdue", tier: tierWarn,
			class: "task-overdue", target: target, frame: task, sustain: 2,
			symptom: fmt.Sprintf("task %s claimed and running but %ss past run_at (> 2x its 7-day p95 %ss) — long-running or stuck",
				task, r.str(1), r.str(2)),
			baseline: fmt.Sprintf("healthy runs finish within their history (p95 %ss); claim keepalive alive means running, not parked (1.2)", r.str(2)),
			observed: fmt.Sprintf("overdue_s=%s p95_s=%s claim=live", r.str(1), r.str(2)),
			context:  "compare against finished_task history before declaring stuck; whatever this task maintains is going stale while it grinds",
			playbook: "SIGNALS.md 5.7",
		})
	}
	if len(overdueTasks) == 0 {
		findings = append(findings, healthyFinding("pg/task-overdue", tierWarn, "task-overdue", target))
	}

	// failing / parked tasks (warn). The error text is diagnostic gold; parked
	// = error_count > 0 and run_at far out and lease expired.
	failRows, err := env.runner.pg(ctx, `
		SELECT split_part(function_name,'.',3) AS task,
		       reschedule_error_count,
		       coalesce(round(extract(epoch FROM run_at-now())),0) AS run_at_in_s,
		       left(coalesce(reschedule_error,''),100) AS last_error
		FROM pending_task
		WHERE reschedule_error_count > 0
		ORDER BY reschedule_error_count DESC LIMIT 10;
	`)
	if err != nil {
		return findings, err
	}
	if len(failRows) > 0 {
		lines := []string{}
		parked := 0
		for _, r := range failRows {
			runAtInS := atoiRow(r, 2)
			if runAtInS > 300 {
				parked += 1
			}
			lines = append(lines, fmt.Sprintf("  %s errors=%s run_at_in=%ds :: %s",
				r.str(0), r.str(1), runAtInS, r.str(3)))
		}
		findings = append(findings, finding{
			probeId: "pg/task-parked", tier: tierWarn,
			class: "task-parked", target: target, sustain: 1,
			symptom:  fmt.Sprintf("%d recurring task(s) failing on %s (%d parked > 5m out)", len(failRows), target, parked),
			baseline: "reschedule_error_count 0 for all recurring tasks (1.2)",
			observed: fmt.Sprintf("failing_tasks=%d parked=%d", len(failRows), parked),
			evidence: "failing tasks (class + error text):\n" + strings.Join(lines, "\n"),
			playbook: "SIGNALS.md 5.7",
		})
	} else {
		findings = append(findings, healthyFinding("pg/task-parked", tierWarn, "task-parked", target))
	}

	return findings, nil
}

// taskErrorBattery collects the reschedule error text of every failing task —
// the class + target in the text names the failure mode and the sick node.
func taskErrorBattery(ctx context.Context, env *probeEnv) string {
	rows, err := env.runner.pg(ctx, `
		SELECT split_part(function_name,'.',3) AS task,
		       reschedule_error_count,
		       left(coalesce(reschedule_error,''),120) AS last_error
		FROM pending_task WHERE reschedule_error_count > 0
		ORDER BY reschedule_error_count DESC LIMIT 8;
	`)
	if err != nil {
		return "task error battery failed: " + err.Error()
	}
	if len(rows) == 0 {
		return "no tasks with reschedule errors (canary dead but no task-level error text — check redis directly)"
	}
	lines := []string{"failing recurring tasks (the error text names the failure mode + sick node):"}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("  %s errors=%s :: %s", r.str(0), r.str(1), r.str(2)))
	}
	return strings.Join(lines, "\n")
}

// taskDurationProbe is SIGNALS.md 2.5 / §7 task-duration-regression: per
// function, the last hour's mean run duration vs the trailing 7-day p95,
// entirely from finished_task history (no local store needed). Only functions
// with enough history and meaningful duration are compared.
type taskDurationProbe struct{}

func (self taskDurationProbe) id() string             { return "pg/task-duration-regression" }
func (self taskDurationProbe) tier() string           { return tierWarn }
func (self taskDurationProbe) cadence() time.Duration { return time.Hour }

func (self taskDurationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
		WITH hist AS (
			SELECT split_part(function_name,'.',3) AS task,
			       percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM run_end_time-run_start_time)) AS p95_s,
			       count(*) AS n
			FROM finished_task
			WHERE run_end_time > now() - interval '7 days'
			  AND run_end_time <= now() - interval '1 hour'
			GROUP BY 1 HAVING count(*) >= 20
		), recent AS (
			SELECT split_part(function_name,'.',3) AS task,
			       avg(extract(epoch FROM run_end_time-run_start_time)) AS mean_s,
			       count(*) AS n
			FROM finished_task
			WHERE run_end_time > now() - interval '1 hour'
			GROUP BY 1
		)
		SELECT r.task, round(r.mean_s::numeric,1), round(h.p95_s::numeric,1), r.n
		FROM recent r JOIN hist h USING (task)
		WHERE h.p95_s >= 5 AND r.mean_s > 2*h.p95_s
		ORDER BY r.mean_s / h.p95_s DESC LIMIT 10;
	`)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	seenTasks := map[string]bool{}
	for _, r := range rows {
		task := r.str(0)
		seenTasks[task] = true
		findings = append(findings, finding{
			probeId: "pg/task-duration-regression", tier: tierWarn,
			class: "task-duration-regression", target: target, frame: task, sustain: 1,
			symptom: fmt.Sprintf("task %s last-hour mean %ss is > 2x its 7-day p95 %ss (%s runs)",
				task, r.str(1), r.str(2), r.str(3)),
			baseline: fmt.Sprintf("7-day p95 %ss (from finished_task history)", r.str(2)),
			observed: fmt.Sprintf("mean_1h=%ss p95_7d=%ss runs_1h=%s", r.str(1), r.str(2), r.str(3)),
			playbook: "SIGNALS.md 2.5",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/task-duration-regression", tierWarn, "task-duration-regression", target))
	}
	return findings, nil
}
