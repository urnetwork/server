// Shared task-system collection and evaluation used by task-canaries and
// task-health.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// taskFailureSummarySQL returns one representative row per failing task
// function, plus whole-family counts.  Never cap raw pending_task rows before
// grouping: a large family such as AdvancePayment can otherwise consume the
// entire result and hide an unrelated control-plane failure.
const taskFailureSummarySQL = `
	WITH failures AS (
		SELECT split_part(function_name,'.',3) AS task,
		       reschedule_error_count,
		       coalesce(round(extract(epoch FROM run_at-now())),0)::bigint AS run_at_in_s,
		       claim_time > now() - interval '2 minutes' AS fresh_claim,
		       left(coalesce(reschedule_error,''),160) AS last_error,
		       run_max_time_seconds,
		       CASE
		         WHEN lower(coalesce(reschedule_error,'')) LIKE '%asset amount owned by the wal%'
		           OR lower(coalesce(reschedule_error,'')) LIKE '%insufficient token balance%'
		           THEN 'wallet-insufficient'
		         WHEN split_part(function_name,'.',3) = 'Payout'
		           AND lower(coalesce(reschedule_error,'')) LIKE '%pgconn.connlockerror=conn closed%'
		           THEN 'idle-transaction-timeout'
		         WHEN lower(coalesce(reschedule_error,'')) LIKE '%failed to deallocate cached statement(s): conn closed%'
		           THEN 'connection-cleanup-deadline'
		         WHEN coalesce(reschedule_error,'') LIKE '%429 Too Many Requests%'
		           THEN 'processor-rate-limit'
		         WHEN lower(coalesce(reschedule_error,'')) LIKE '%invalid destination address%'
		           THEN 'processor-invalid-destination'
		         WHEN coalesce(reschedule_error,'') LIKE '%400 Bad Request%'
		           THEN 'processor-bad-request'
		         WHEN lower(coalesce(reschedule_error,'')) LIKE '%sqlstate 42703%'
		           OR lower(coalesce(reschedule_error,'')) LIKE '%sqlstate 42p01%'
		           OR lower(coalesce(reschedule_error,'')) LIKE '%sqlstate 42883%'
		           OR lower(coalesce(reschedule_error,'')) LIKE '%sqlstate 42704%'
		           THEN 'schema-object-missing'
		         WHEN trim(coalesce(reschedule_error,'')) = 'Timeout'
		           THEN 'deadline-timeout'
		         WHEN lower(coalesce(reschedule_error,'')) LIKE '%context canceled%'
		           OR lower(coalesce(reschedule_error,'')) LIKE '%interrupted: done%'
		           THEN 'context-canceled'
		         ELSE 'other'
		       END AS error_class
		FROM pending_task
		WHERE reschedule_error_count > 0
	), cause_counts AS (
		SELECT task, error_class, count(*) AS class_count
		FROM failures
		GROUP BY task, error_class
	), cause_summaries AS (
		SELECT task,
		       count(*) AS cause_class_count,
		       string_agg(error_class || '=' || class_count::text, ',' ORDER BY class_count DESC, error_class) AS cause_summary
		FROM cause_counts
		GROUP BY task
	), ranked AS (
		SELECT failures.*,
		       count(*) OVER (PARTITION BY task) AS family_count,
		       count(*) FILTER (WHERE run_at_in_s > 300) OVER (PARTITION BY task) AS parked_count,
		       count(*) FILTER (WHERE fresh_claim) OVER (PARTITION BY task) AS fresh_claim_count,
		       row_number() OVER (
		           PARTITION BY task
		           ORDER BY reschedule_error_count DESC, run_at_in_s DESC, last_error
		       ) AS sample_rank
		FROM failures
	)
	SELECT ranked.task, family_count, parked_count, fresh_claim_count,
	       reschedule_error_count, run_at_in_s, last_error,
	       run_max_time_seconds, cause_class_count, cause_summary
	FROM ranked
	JOIN cause_summaries USING (task)
	WHERE sample_rank = 1
	ORDER BY reschedule_error_count DESC, task;
`

func taskCauseClasses(summary string) map[string]struct{} {
	classes := map[string]struct{}{}
	for _, part := range strings.Split(summary, ",") {
		class, _, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && class != "" {
			classes[class] = struct{}{}
		}
	}
	return classes
}

func advancePaymentMixedGuidance(causeSummary string) (string, string, string) {
	classes := taskCauseClasses(causeSummary)
	actions := []string{}
	verifications := []string{}
	known := map[string]struct{}{}
	add := func(class, action, verification string) {
		known[class] = struct{}{}
		if _, ok := classes[class]; !ok {
			return
		}
		actions = append(actions, action)
		verifications = append(verifications, verification)
	}

	add("wallet-insufficient",
		"fund or pause the payout wallet for wallet-insufficient rows",
		"funded wallet rows clear")
	add("schema-object-missing",
		"restore migration coherence per §8.9 for schema-object-missing rows before dependent services run; do not hand-create the artifact",
		"the migration head and artifacts reach the binary requirement and schema-object-missing clears")
	add("connection-cleanup-deadline",
		"deploy the queued, cursor-batched CompletePayment retention path for connection-cleanup-deadline rows",
		"the legacy retention query and new 120-second cleanup failures disappear")
	add("processor-invalid-destination",
		"correct chain-mismatched payout wallets and release only Circle's definitive invalid-destination pre-chain attempts while preserving ambiguous-submit idempotency keys",
		"corrected destination retries switch wallets without duplicate sends")
	add("processor-bad-request",
		"inspect processor-bad-request rows while preserving ambiguous-submit idempotency keys",
		"processor-bad-request rows reach a definitive safe outcome")
	add("processor-rate-limit",
		"retain normal backoff for processor-rate-limit rows",
		"processor-rate-limit returns to its normal bounded retry rate")
	add("deadline-timeout",
		"correlate deadline-timeout rows with their exact evaluator boundary before changing batch size or MaxTime",
		"deadline-timeout rows finish inside their justified boundary")
	add("context-canceled",
		"separate context-canceled rows by deploy drain versus exact task deadline before changing their retry policy",
		"context-canceled rows reach their documented drain or deadline outcome")

	unknown := false
	for class := range classes {
		if _, ok := known[class]; !ok {
			unknown = true
			break
		}
	}
	if unknown {
		actions = append(actions, "inspect the original task error for every remaining cause class")
		verifications = append(verifications, "every remaining cause class is diagnosed and clears")
	}
	if len(actions) == 0 {
		actions = append(actions, "inspect and remediate each listed cause class independently")
		verifications = append(verifications, "each listed cause class is diagnosed and clears")
	}

	playbook := "SIGNALS.md §1.2 and §5.7"
	if _, ok := classes["schema-object-missing"]; ok {
		playbook = "SIGNALS.md §1.2, §5.7, and §8.9"
	}
	return "Handle only the present AdvancePayment classes: " + strings.Join(actions, "; ") + ". Do not delete or manually replay the mixed family.",
		"Verify each present cause independently: " + strings.Join(verifications, "; ") + ".",
		playbook
}

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
	// its historical duration guard — the "long-running vs stuck" signature
	// (1.2) that parked-detection misses because error_count may be low and
	// run_at is past, not future. A median-tail cap prevents repeated historical
	// overruns from inflating p95 until the same defect becomes the baseline.
	// The taskworker heartbeat, when available, verifies actual elapsed time so
	// a task that merely waited in the due queue is not called long-running.
	overdueRows, err := env.runner.pg(ctx, `
		WITH history AS (
			SELECT split_part(function_name,'.',3) AS task,
			       percentile_cont(0.50) WITHIN GROUP (ORDER BY extract(epoch FROM run_end_time-run_start_time)) AS p50_s,
			       percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM run_end_time-run_start_time)) AS p95_s
			FROM finished_task
			WHERE run_end_time > now() - interval '7 days'
			GROUP BY 1 HAVING count(*) >= 10
		), live AS (
			SELECT split_part(function_name,'.',3) AS task,
			       task_id::text,
			       round(extract(epoch FROM now()-run_at))::int AS overdue_s,
			       run_max_time_seconds
			FROM pending_task
			WHERE run_at < now() - interval '10 minutes'
			  AND claim_time > now() - interval '2 minutes'
		), ranked AS (
			SELECT live.*,
			       row_number() OVER (PARTITION BY task ORDER BY overdue_s DESC, task_id) AS task_rank
			FROM live
		)
		SELECT p.task, p.overdue_s, round(coalesce(h.p95_s,1800))::int,
		       (h.p95_s IS NOT NULL)::text,
		       p.run_max_time_seconds,
		       round(coalesce(h.p50_s,0))::int,
		       p.task_id
		FROM ranked p
		LEFT JOIN history h USING (task)
		WHERE p.task_rank = 1
		ORDER BY p.overdue_s DESC LIMIT 50;
	`)
	if err != nil {
		return findings, err
	}
	var dbMaintenanceEvidence pgRow
	for _, r := range overdueRows {
		if r.str(0) != "DbMaintenance" {
			continue
		}
		maintenanceRows, maintenanceErr := env.runner.pg(ctx, `
			SELECT p.relid::regclass::text,
			       p.index_relid::regclass::text,
			       p.phase,
			       round(extract(epoch FROM clock_timestamp()-a.query_start))::int,
			       coalesce(a.wait_event_type,''),
			       coalesce(a.wait_event,''),
			       pg_blocking_pids(p.pid)::text,
			       coalesce(blocker.pid::text,''),
			       left(regexp_replace(coalesce(blocker.query,''), E'[\\n\\r\\t]+', ' ', 'g'), 240),
			       p.blocks_done,
			       p.blocks_total
			FROM pg_stat_progress_create_index p
			JOIN pg_stat_activity a USING (pid)
			LEFT JOIN LATERAL (
				SELECT blocked_by.pid, blocked_by.query
				FROM unnest(pg_blocking_pids(p.pid)) AS blocker_pid(pid)
				JOIN pg_stat_activity blocked_by ON blocked_by.pid = blocker_pid.pid
				ORDER BY blocked_by.xact_start NULLS LAST
				LIMIT 1
			) blocker ON true
			WHERE p.command = 'REINDEX CONCURRENTLY'
			ORDER BY a.query_start
			LIMIT 1;
		`)
		if maintenanceErr != nil {
			return findings, maintenanceErr
		}
		if len(maintenanceRows) > 0 {
			dbMaintenanceEvidence = maintenanceRows[0]
		}
		break
	}
	overdueTasks := map[string]bool{}
	for _, r := range overdueRows {
		task := r.str(0)
		overdueSeconds := atoi(r.str(1))
		p95Seconds := atoi(r.str(2))
		haveHistory := strings.EqualFold(r.str(3), "t") || strings.EqualFold(r.str(3), "true")
		p50Seconds := atoi(r.str(5))
		thresholdSeconds, comparisonMode := taskOverdueThresholdSeconds(p50Seconds, p95Seconds, haveHistory)
		if overdueSeconds <= thresholdSeconds {
			continue
		}

		taskID := r.str(6)
		active := taskActiveRun{}
		var activeLogErr error
		if taskID != "" {
			var activeLog string
			activeLog, activeLogErr = env.runner.warpctl(
				ctx,
				"logs", env.cfg.env, "taskworker",
				"--since=5m", "--limit=500", "--query="+taskID, "--utc",
			)
			active = parseTaskActiveRun(activeLog, task)
			if active.taskID == taskID && 0 < active.seconds && active.seconds <= thresholdSeconds {
				// The row waited in the due queue and only recently began running.
				// Task convergence owns queue delay; this signal owns execution time.
				continue
			}
		}

		elapsedSeconds := overdueSeconds
		elapsedSource := "run-at-fallback"
		if taskID != "" && active.taskID == taskID && 0 < active.seconds {
			elapsedSeconds = active.seconds
			elapsedSource = "eval-active"
		}
		overdueTasks[task] = true
		comparisonSource := "fallback"
		comparisonLabel := "fallback"
		if haveHistory {
			comparisonSource = "history-p95"
			comparisonLabel = "7-day p95"
		}
		if comparisonMode == "median-tail-cap" {
			comparisonSource = comparisonMode
			comparisonLabel = "median-tail guard"
		}
		symptom := fmt.Sprintf(
			"task %s claimed and running but %ss past run_at (> 2x its %s %ss) — long-running or stuck",
			task, r.str(1), comparisonLabel, r.str(2),
		)
		baseline := fmt.Sprintf(
			"healthy runs finish within their comparison band (%s %ss); claim keepalive alive means running, not parked (1.2)",
			comparisonLabel, r.str(2),
		)
		if comparisonMode == "median-tail-cap" {
			symptom = fmt.Sprintf(
				"task %s has run for %ds, above its %ds median-tail guard (7-day p50 %ds, p95 %ds)",
				task, elapsedSeconds, thresholdSeconds, p50Seconds, p95Seconds,
			)
			baseline = fmt.Sprintf(
				"healthy 7-day p50 is %ds; alert at max(4x p50, 1200s)=%ds when that is earlier than 2x the polluted p95 tail",
				p50Seconds, thresholdSeconds,
			)
		}
		observed := fmt.Sprintf(
			"overdue_s=%s elapsed_s=%d elapsed_source=%s comparison_s=%s comparison_source=%s alert_threshold_s=%d p50_s=%d p95_s=%d max_time_s=%s claim=live",
			r.str(1), elapsedSeconds, elapsedSource, r.str(2), comparisonSource, thresholdSeconds, p50Seconds, p95Seconds, r.str(4),
		)
		if taskID != "" {
			observed += " task_id=" + taskID
		}
		if active.taskID == taskID && active.identity.host != "" {
			observed += fmt.Sprintf(
				" active_host=%s active_generation=%s active_container=%s",
				active.identity.host,
				active.identity.generation,
				active.identity.container,
			)
		}
		alertContext := "Compare against finished_task history before declaring stuck; the configured max time bounds this attempt, but whatever the task maintains is going stale while it grinds."
		if elapsedSource == "eval-active" {
			alertContext += " The authoritative taskworker heartbeat confirms execution time; run_at remains the scheduler due time."
		} else if activeLogErr != nil {
			alertContext += " Taskworker heartbeat lookup failed, so elapsed_s falls back to run_at chronology: " + activeLogErr.Error()
		} else if taskID != "" {
			alertContext += " No matching recent taskworker heartbeat was found, so elapsed_s falls back to run_at chronology; validate actual execution time before intervening."
		}
		alert := finding{
			probeId: "pg/task-overdue", tier: tierWarn,
			class: "task-overdue", target: target, frame: task, sustain: 2,
			symptom:  symptom,
			baseline: baseline,
			observed: observed,
			context:  alertContext,
			playbook: "SIGNALS.md 5.7",
		}
		if task == "RemoveDisconnectedNetworkClients" {
			alert.mechanism = "The deployed post-delete path performs several serialized Redis round trips and a separate provide-key pipeline per reaped client. Once PostgreSQL has produced a large client-id cohort, that cross-slot tail can keep one taskworker occupied for well over an hour and co-reside with unrelated close, score, and escrow work on the same executor."
			alert.context += " Confirm the variant by checking the five bounded PostgreSQL eligibility bands and active reaper statements; empty bands plus a continuing heartbeat isolate the post-delete Redis/cascade tail. Executor identity is chronology for correlation, not permission to restart the container."
			alert.action = "Roll out the 1,000-client idempotent Redis cleanup chunks and bounded provide-key pipelines. Do not raise the four-hour task deadline, restart the taskworker, or discard the in-memory cleanup cohort."
			alert.verify = "A large reaper run returns toward its seconds/minutes band, the five PostgreSQL eligibility probes remain drained, target Redis state is removed, a reassigned forward egress owner is preserved, and co-resident task durations normalize."
		}
		if task == "UpdateReliabilities" {
			alert.mechanism = "The reliability running-window threshold was shorter than the task cadence, forcing a full re-anchor of the multi-billion-row history on every cycle instead of using the exact add-entering/subtract-leaving path. The 7-day anchor has a long I/O-contention tail and can reach the task deadline when its lookbacks share one transaction."
			alert.context += ". Confirm this variant with an active INSERT into client_reliability_running and running-window markers whose last re-anchor is only a few blocks behind; a different active statement requires separate diagnosis"
			alert.action = "Roll out the four-hour re-anchor cadence plus per-lookback transaction checkpoints, and defer optional anchors while established VACUUM/REINDEX work is active. Retain the 30-minute incremental cadence; do not raise the two-hour task deadline or cancel a progressing query merely to hide this alert."
			alert.verify = "Most half-hour cycles take the rolling path and finish below the historical p95. A quiet-period anchor commits one lookback at a time; an interrupted retry preserves those checkpoints, and rolling-equivalence tests remain green."
		}
		if task == "DbMaintenance" && len(dbMaintenanceEvidence) > 0 {
			alert.evidence = fmt.Sprintf(
				"relation=%s index=%s phase=%s query_age_s=%s wait=%s:%s blocking_pids=%s blocker_pid=%s blocker_query=%s blocks_done=%s blocks_total=%s",
				dbMaintenanceEvidence.str(0), dbMaintenanceEvidence.str(1), dbMaintenanceEvidence.str(2),
				dbMaintenanceEvidence.str(3), dbMaintenanceEvidence.str(4), dbMaintenanceEvidence.str(5),
				dbMaintenanceEvidence.str(6), dbMaintenanceEvidence.str(7), dbMaintenanceEvidence.str(8),
				dbMaintenanceEvidence.str(9), dbMaintenanceEvidence.str(10),
			)
			if strings.Contains(strings.ToLower(dbMaintenanceEvidence.str(8)), "client_reliability_running") {
				alert.mechanism = "The concurrent index rebuild is waiting for old snapshots, and PostgreSQL identifies the UpdateReliabilities running-window re-anchor as its virtual-XID blocker. REINDEX CONCURRENTLY cannot complete its index swap while that older transaction remains visible."
				alert.context += ". The maintenance claim heartbeat is live: this is downstream lock coupling, not an abandoned task lease."
				alert.action = "Let the configured deadlines arbitrate the current work. Roll out the four-hour cadence, per-lookback reliability checkpoints, and optional-anchor maintenance deferral; do not cancel either progressing task, raise its deadline, or rebuild the same index again."
				alert.verify = "UpdateReliabilities releases each checkpoint transaction, the reindex advances beyond waiting for old snapshots and completes, and DbMaintenance clears without a maintenance error."
			} else {
				alert.mechanism = "The daily maintenance task is actively executing REINDEX CONCURRENTLY. Its overdue task age includes earlier objects and waits; the current PostgreSQL progress row proves this claim is doing bounded per-object work rather than holding an abandoned lease."
				alert.context += ". Compare query_age_s with the two-hour per-object limit and blocks_done/blocks_total across samples. A phase or block increase is progress even when the overall task remains overdue."
				alert.action = "Allow the current concurrent reindex to run under its per-object two-hour limit. Investigate only if progress is flat on consecutive samples or the statement reaches that limit; do not start a duplicate rebuild or cancel it based only on task run_at age."
				alert.verify = "blocks_done or phase advances on consecutive samples, this index completes within its per-object limit, and DbMaintenance eventually clears or identifies a specific later object error."
			}
			alert.playbook = "SIGNALS.md 2.2"
		}
		findings = append(findings, alert)
	}
	if len(overdueTasks) == 0 {
		findings = append(findings, healthyFinding("pg/task-overdue", tierWarn, "task-overdue", target))
	}

	// Failing / parked tasks (warn). Emit one identity per function so one
	// high-volume family cannot hide another task's root-cause text. Parked
	// means error_count > 0 and run_at more than five minutes in the future;
	// A fresh claim heartbeat distinguishes a live/recent attempt from a fully
	// dormant retry. It is deliberately not a disjoint bucket: during reschedule
	// handoff, the same row can already have a future run_at while its previous
	// claim heartbeat is still fresh.
	failRows, err := env.runner.pg(ctx, taskFailureSummarySQL)
	if err != nil {
		return findings, err
	}
	for _, r := range failRows {
		task := r.str(0)
		familyCount, parkedCount, freshClaimCount := atoiRow(r, 1), atoiRow(r, 2), atoiRow(r, 3)
		lastError, maxTimeSeconds := r.str(6), r.str(7)
		causeClassCount, causeSummary := atoiRow(r, 8), r.str(9)
		mixedCauses := 1 < causeClassCount
		lowerError := strings.ToLower(lastError)
		schemaObjectMissing := strings.Contains(causeSummary, "schema-object-missing=") ||
			strings.Contains(lowerError, "sqlstate 42703") ||
			strings.Contains(lowerError, "sqlstate 42p01") ||
			strings.Contains(lowerError, "sqlstate 42883") ||
			strings.Contains(lowerError, "sqlstate 42704")
		disabledVerifyRetry := task == "RefreshVerifyProxyEgress" &&
			!env.cfg.verificationEnabled &&
			(strings.Contains(lowerError, "context canceled") ||
				strings.Contains(lowerError, "interrupted: done"))
		alertMechanism, alertAction, alertVerify := "", "", ""
		alertPlaybook := "SIGNALS.md 5.7"
		alertContext := "Each task function is grouped before reporting; another noisy function cannot consume a global row limit and hide this failure. Parked and fresh-claim counts are independent predicates and can overlap briefly during reschedule handoff; do not add them together."
		if strings.EqualFold(strings.TrimSpace(lastError), "Timeout") {
			alertContext += fmt.Sprintf(" This literal Timeout is the task evaluator's configured deadline of %ss. Compare the matching eval-error duration; an exact match means the task needs a smaller checkpointed batch or a justified task-specific MaxTime, not a database restart.", maxTimeSeconds)
		} else if strings.Contains(lowerError, "context canceled") && !strings.Contains(lastError, "Drained:") && !disabledVerifyRetry {
			alertContext += fmt.Sprintf(" This is a non-drain context cancellation with a configured task deadline of %ss; compare the taskworker eval-error duration with that deadline. An exact match identifies an undersized task-specific MaxTime, not a deploy drain.", maxTimeSeconds)
		}
		if mixedCauses {
			alertMechanism = fmt.Sprintf("This task family contains %d distinct error classes. Its representative row is selected by error count for bounded evidence and cannot describe every failing row; use the complete cause breakdown instead of attributing the whole family to that sample.", causeClassCount)
			alertContext += " The cause breakdown is computed across every failing row in this function before selecting the representative error."
			if task == "AdvancePayment" {
				alertAction, alertVerify, alertPlaybook = advancePaymentMixedGuidance(causeSummary)
			} else {
				alertAction = "Investigate and remediate each listed cause class independently; do not apply the representative error's action to the entire mixed family or delete task rows to hide it."
				alertVerify = "Each cause-class count converges to zero or its explicitly documented background state, and no minority class remains hidden behind the former dominant sample."
			}
		} else if schemaObjectMissing {
			alertMechanism = "The running task references a PostgreSQL schema object that does not exist in its connected database. During a rollout this normally means schema-dependent code activated before its append-only migration and artifact check; if the successful migration head already claims that version, the database instead has migration-schema drift."
			alertContext += " SQLSTATE 42703, 42P01, 42883, and 42704 identify undefined columns, tables, functions, and objects respectively; the exact object in the representative error must be mapped to the versioned artifact table in §8.9."
			alertAction = "Compare the running binary's required MigrationCount, the successful migration_audit head, and the versioned artifact in §8.9. If the database is behind, reject the rollout as incomplete and run the migration phase from the exact service commit before dependent services; if the head is current, repair migration-schema-drift. Do not create the object by hand or delete the task row."
			alertVerify = "The migration head reaches the binary-required version, every versioned artifact probe passes, and this same task family succeeds and clears its reschedule error without manual row deletion."
			alertPlaybook = "SIGNALS.md §8.9"
		} else if disabledVerifyRetry {
			alertMechanism = "The verification subsystem is disabled, but a RefreshVerifyProxyEgress RunOnce row from an older generation is still executing and rescheduling. Its exact task-deadline cancellation is wasted disabled work, not evidence that the 15-minute deadline is too small."
			alertContext += fmt.Sprintf(" The monitor loaded verification_enabled=false from the same environment. A matching eval error at the configured %ss boundary confirms the stale ungated chain; `Interrupted: Done` is the same disabled-work family.", maxTimeSeconds)
			alertAction = "Roll out the StEnabled guards on verification task seeding, execution, and Post scheduling, plus the taskworker-startup reap for all four verification task functions. Do not raise the deadline or pull this row forward."
			alertVerify = "After taskworker startup, this pending row disappears without a replacement, all disabled verification task families remain absent, and disabled /verify routes fail closed before required-vault access."
		} else if task == "UpdateReliabilities" &&
			strings.Contains(lowerError, "failed to deallocate cached statement(s): conn closed") {
			alertMechanism = "A full reliability running-window anchor exhausted the task's configured deadline. pgx surfaced the interrupted connection while deallocating cached statements, after PostgreSQL canceled the work. Without per-lookback commits, the transaction rollback discards every completed lookback and the retry starts the same full anchor sequence again."
			alertContext += fmt.Sprintf(" Confirm the matching taskworker eval-error duration is exactly the configured %ss and that the successor claim retains the same args before applying this diagnosis.", maxTimeSeconds)
			alertAction = "Roll out the four-hour re-anchor cadence, checkpoint each lookback in its own transaction, and defer optional anchors while a VACUUM or concurrent index build has already run for five minutes. Do not raise the deadline or manually kick the fresh retry."
			alertVerify = "A retry preserves completed lookback markers, rolls those windows instead of rescanning them, then clears the task error; the blocked concurrent reindex and vacuum chain advance after each checkpoint releases its snapshot."
		} else if task == "CloseExpiredContracts" && strings.EqualFold(strings.TrimSpace(lastError), "Timeout") {
			alertMechanism = "The deployed closer selected one 100,000-contract cohort. Its per-contract commits made durable progress, but the scheduler did not checkpoint success before the 30-minute task boundary, so an exact Timeout made the retry rescan the remaining ordered cohort while old contracts accumulated."
			alertContext += " A successor with a new task id after the retry is evidence that per-contract work survived; it does not make the oversized scheduler boundary safe under recurring write/vacuum pressure."
			alertAction = "Roll out the 25,000-contract close cohort while retaining the existing 92-worker inner pool and immediate successor scheduling for full cohorts. Do not raise the 30-minute deadline or add task-level shards."
			alertVerify = "Each full 25,000-contract cohort acknowledges task success before the deadline, schedules its immediate successor, and the older-than-five-minute open set falls on consecutive samples without another Timeout."
		} else if task == "Payout" && strings.Contains(lowerError, "connlockerror=conn closed") {
			alertMechanism = "The bounded payment-plan transaction remained intentionally idle while a separate reliability-maintenance transaction ran longer than PostgreSQL's five-minute idle-in-transaction timeout, so PostgreSQL closed the outer connection before it could commit."
			alertContext += " Scope the exception to this payment-plan transaction with SET LOCAL; the task MaxTime and bounded plan slice remain the safety limits, while every unrelated session keeps the global timeout."
			alertAction = "Roll out the transaction-local idle_in_transaction_session_timeout override for payment planning. Do not disable the database-wide guard or repeatedly kick the still-running row."
			alertVerify = "One Payout slice commits and clears this row's error, and a new unrelated PostgreSQL session still reports the configured five-minute idle-in-transaction timeout."
		} else if task == "AdvancePayment" && (strings.Contains(lowerError, "asset amount owned by the wal") || strings.Contains(lowerError, "insufficient token balance")) {
			alertMechanism = "The external payout wallet does not own enough of the requested asset to fund pending payouts. Task retries cannot create that balance; repeated HTTP 400 responses only move the rows through backoff."
			alertAction = "Finance/operations must fund the named payout wallet with the required asset, or explicitly pause payouts. Do not treat this as an API, PostgreSQL, or Redis availability incident."
			alertVerify = "After funding, AdvancePayment retries succeed and the family count converges to zero without manual row deletion."
		} else if task == "AdvancePayment" && strings.Contains(lowerError, "invalid destination address") {
			alertMechanism = "The payout wallet address is invalid for its declared chain. A Solana base58 key registered as MATIC passes the deployed chain-blind validator, then Circle definitively rejects the destination before creating a transfer; the pinned idempotency key prevents a corrected payout wallet from being selected."
			alertAction = "Correct the network's payout wallet, roll out chain-specific SOL/MATIC validation, and release only this typed definitive pre-chain rejection so UpdatePaymentWallet can select the correction. Preserve the idempotency key for transport errors, rate limits, and any ambiguous submit result; do not delete payment or sweep rows."
			alertVerify = "The retry selects the corrected chain-compatible wallet with a fresh key, completes without a duplicate transfer, and processor-invalid-destination converges to zero."
		}
		symptom := fmt.Sprintf("task family %s has %d failing row(s) on %s (%d parked >5m; %d with a fresh claim heartbeat; sets may overlap)",
			task, familyCount, target, parkedCount, freshClaimCount)
		if mixedCauses {
			symptom += fmt.Sprintf("; %d error classes", causeClassCount)
		}
		findings = append(findings, finding{
			probeId: "pg/task-parked", tier: tierWarn,
			class: "task-parked", target: target, frame: task, sustain: 1,
			symptom:   symptom,
			mechanism: alertMechanism,
			baseline:  "reschedule_error_count 0 for all recurring tasks (1.2)",
			observed: fmt.Sprintf("task=%s failing_rows=%d parked_over_5m=%d fresh_claim_heartbeats=%d counts_may_overlap=true max_errors=%s sample_run_at_in_s=%s sample_max_time_s=%s cause_classes=%d cause_breakdown=%s",
				task, familyCount, parkedCount, freshClaimCount, r.str(4), r.str(5), maxTimeSeconds, causeClassCount, causeSummary),
			evidence: "representative error from this task family:\n  " + lastError,
			context:  alertContext,
			action:   alertAction,
			verify:   alertVerify,
			playbook: alertPlaybook,
		})
	}
	if len(failRows) == 0 {
		findings = append(findings, healthyFinding("pg/task-parked", tierWarn, "task-parked", target))
	}

	return findings, nil
}

const taskOverdueMinimumSeconds = 20 * 60

// taskOverdueThresholdSeconds keeps p95 as the ordinary long-run guard, but
// caps a highly skewed history with max(4*p50, 20m). Without the cap, repeated
// hour-scale defects teach the monitor that the defect is normal. Missing or
// incomplete history preserves the prior one-hour fallback.
func taskOverdueThresholdSeconds(p50Seconds, p95Seconds int, haveHistory bool) (int, string) {
	if !haveHistory || p95Seconds <= 0 {
		return 2 * 1800, "fallback"
	}
	p95Threshold := 2 * p95Seconds
	if p50Seconds <= 0 {
		return max(taskOverdueMinimumSeconds, p95Threshold), "p95"
	}
	medianTailThreshold := max(taskOverdueMinimumSeconds, 4*p50Seconds)
	if medianTailThreshold < p95Threshold {
		return medianTailThreshold, "median-tail-cap"
	}
	return max(taskOverdueMinimumSeconds, p95Threshold), "p95"
}

// taskErrorBattery collects the reschedule error text of every failing task —
// the class + target in the text names the failure mode and the sick node.
func taskErrorBattery(ctx context.Context, env *probeEnv) string {
	rows, err := env.runner.pg(ctx, taskFailureSummarySQL)
	if err != nil {
		return "task error battery failed: " + err.Error()
	}
	if len(rows) == 0 {
		return "no tasks with reschedule errors (canary dead but no task-level error text — check redis directly)"
	}
	lines := []string{"failing recurring tasks (the error text names the failure mode + sick node):"}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("  %s rows=%s parked=%s active=%s max_errors=%s :: %s",
			r.str(0), r.str(1), r.str(2), r.str(3), r.str(4), r.str(6)))
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
