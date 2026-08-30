package monitor

import (
	"context"
	"fmt"
	"time"
)

// SIGNALS.md §2.6a maps to signal_close_duration.go and
// signal_close_duration_test.go.
func NewCloseDurationSignal() Signal {
	return &signalAdapter{
		number: "2.6a",
		key:    "close-duration",
		name:   "Close-contract checkpoint duration",
		probe:  closeDurationProbe{},
	}
}

type closeDurationProbe struct{}

const (
	closeDurationLimit             = 2 * time.Minute
	closeDurationActiveLogLookback = 2 * time.Minute
	closeDurationIncidentLookback  = 45 * time.Minute
)

func (closeDurationProbe) id() string             { return "pg/close-duration-overrun" }
func (closeDurationProbe) tier() string           { return tierWarn }
func (closeDurationProbe) cadence() time.Duration { return time.Minute }

func (closeDurationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := pgTarget(env)
	activeLog, activeLogErr := env.runner.warpctl(
		ctx,
		"logs", env.cfg.env, "taskworker",
		fmt.Sprintf("--since=%dm", int(closeDurationIncidentLookback/time.Minute)),
		"--limit=5000", "--query=CloseExpiredContracts", "--utc",
	)
	active := parseTaskActiveRun(activeLog, "CloseExpiredContracts")
	terminal := parseTaskTerminalRun(activeLog, "CloseExpiredContracts")
	retryActive := parseTaskActiveRunForID(activeLog, "CloseExpiredContracts", terminal.taskID)

	// These ids come only from taskRunIDRe's fixed hexadecimal shape, so they
	// are safe SQL literals. Selecting both identities prevents later fast
	// successors from pushing a completed retry or active attempt out of the
	// small ranked result set.
	activeTaskIDLiteral := "NULL"
	if active.taskID != "" {
		activeTaskIDLiteral = "'" + active.taskID + "'"
	}
	terminalTaskIDLiteral := "NULL"
	if terminal.taskID != "" {
		terminalTaskIDLiteral = "'" + terminal.taskID + "'"
	}
	rows, err := env.runner.pg(ctx, fmt.Sprintf(`
		WITH recent AS (
			SELECT task_id::text AS task_id,
			       'completed'::text AS phase,
			       round(extract(epoch FROM run_end_time-run_start_time))::int AS duration_s,
			       round(extract(epoch FROM now()-run_end_time))::int AS age_s,
			       extract(epoch FROM run_end_time)::bigint AS completed_unix_s,
			       run_end_time,
			       run_end_time-run_start_time >= interval '120 seconds' AS is_overrun
			FROM finished_task
			WHERE split_part(function_name,'.',3) = 'CloseExpiredContracts'
			  AND run_end_time > now() - interval '45 minutes'
		), ranked AS (
			SELECT recent.*,
			       row_number() OVER (ORDER BY run_end_time DESC) AS latest_rank,
			       row_number() OVER (PARTITION BY is_overrun ORDER BY run_end_time DESC) AS band_rank
			FROM recent
		)
		SELECT task_id, phase, duration_s, age_s, completed_unix_s
		FROM ranked
		WHERE latest_rank = 1
		   OR (is_overrun AND band_rank = 1)
		   OR task_id = %s
		   OR task_id = %s
		ORDER BY completed_unix_s DESC;
	`, activeTaskIDLiteral, terminalTaskIDLiteral))
	if err != nil {
		return nil, err
	}

	type completion struct {
		taskID          string
		durationSeconds int
		ageSeconds      int
		unixSeconds     int64
	}
	latestCompletion := completion{}
	hasLatestCompletion := false
	completedOverrun := completion{}
	hasCompletedOverrun := false
	activeCompletion := completion{}
	hasActiveCompletion := false
	terminalCompletion := completion{}
	hasTerminalCompletion := false
	for _, row := range rows {
		candidate := completion{
			taskID:          row.str(0),
			durationSeconds: atoi(row.str(2)),
			ageSeconds:      atoi(row.str(3)),
			unixSeconds:     atoi64(row.str(4)),
		}
		if !hasLatestCompletion || latestCompletion.unixSeconds < candidate.unixSeconds {
			latestCompletion = candidate
			hasLatestCompletion = true
		}
		if candidate.durationSeconds >= int(closeDurationLimit/time.Second) &&
			(!hasCompletedOverrun || completedOverrun.unixSeconds < candidate.unixSeconds) {
			completedOverrun = candidate
			hasCompletedOverrun = true
		}
		if active.taskID != "" && candidate.taskID == active.taskID &&
			(!hasActiveCompletion || activeCompletion.unixSeconds < candidate.unixSeconds) {
			activeCompletion = candidate
			hasActiveCompletion = true
		}
		if terminal.taskID != "" && candidate.taskID == terminal.taskID &&
			(!hasTerminalCompletion || terminalCompletion.unixSeconds < candidate.unixSeconds) {
			terminalCompletion = candidate
			hasTerminalCompletion = true
		}
	}
	completionForHeartbeat := latestCompletion
	hasCompletionForHeartbeat := hasLatestCompletion
	if active.taskID != "" {
		completionForHeartbeat = activeCompletion
		hasCompletionForHeartbeat = hasActiveCompletion
	}
	completionSupersedesHeartbeat := hasCompletionForHeartbeat && completedTaskSupersedesHeartbeat(
		completionForHeartbeat.taskID,
		completionForHeartbeat.durationSeconds,
		completionForHeartbeat.ageSeconds,
		active,
		closeDurationActiveLogLookback,
	)

	phase := ""
	taskID := ""
	durationSeconds := 0
	ageSeconds := 0
	identity := warpLogIdentity{}
	errorText := ""
	failedAt := time.Time{}
	activeAttemptEnded := !active.observedAt.IsZero() &&
		!terminal.observedAt.IsZero() &&
		!terminal.observedAt.Before(active.observedAt)
	terminalIsNewestIncident := terminal.seconds >= int(closeDurationLimit/time.Second) &&
		(!hasCompletedOverrun || terminal.observedAt.IsZero() || completedOverrun.unixSeconds == 0 ||
			completedOverrun.unixSeconds <= terminal.observedAt.Unix())
	failedPrecursorToActive := terminalIsNewestIncident &&
		!terminal.observedAt.IsZero() &&
		!active.observedAt.IsZero() &&
		terminal.observedAt.Before(active.observedAt)
	if active.seconds >= int(closeDurationLimit/time.Second) &&
		!activeAttemptEnded &&
		!completionSupersedesHeartbeat {
		phase = "active"
		taskID = active.taskID
		durationSeconds = active.seconds
		identity = active.identity
	} else if terminalIsNewestIncident {
		phase = "failed"
		taskID = terminal.taskID
		durationSeconds = terminal.seconds
		identity = terminal.identity
		errorText = terminal.errorText
		failedAt = terminal.observedAt
	} else if hasCompletedOverrun {
		phase = "completed"
		taskID = completedOverrun.taskID
		durationSeconds = completedOverrun.durationSeconds
		ageSeconds = completedOverrun.ageSeconds
	} else if activeLogErr != nil {
		return nil, fmt.Errorf("read active CloseExpiredContracts task logs: %w", activeLogErr)
	}

	if phase == "" {
		return []finding{healthyFinding(
			"pg/close-duration-overrun",
			tierWarn,
			"close-duration-overrun",
			target,
		)}, nil
	}

	observed := fmt.Sprintf(
		"phase=%s duration_s=%d threshold_s=%d lookback_s=%d",
		phase,
		durationSeconds,
		int(closeDurationLimit/time.Second),
		int(closeDurationIncidentLookback/time.Second),
	)
	if taskID != "" {
		observed += " task_id=" + taskID
	}
	if phase == "active" && identity.host != "" {
		observed += fmt.Sprintf(
			" active_host=%s active_generation=%s active_container=%s",
			identity.host,
			identity.generation,
			identity.container,
		)
	}
	if phase == "active" && failedPrecursorToActive {
		observed += fmt.Sprintf(
			" precursor_failed_duration_s=%d precursor_failed_task_id=%s",
			terminal.seconds,
			terminal.taskID,
		)
		if terminal.errorText != "" {
			observed += fmt.Sprintf(" precursor_failed_error=%q", terminal.errorText)
		}
		observed += " precursor_failed_at=" + terminal.observedAt.UTC().Format(time.RFC3339Nano)
		if terminal.identity.host != "" {
			observed += fmt.Sprintf(
				" precursor_failed_host=%s precursor_failed_generation=%s precursor_failed_container=%s",
				terminal.identity.host,
				terminal.identity.generation,
				terminal.identity.container,
			)
		}
	}
	retryObserved := false
	if phase == "failed" {
		if errorText != "" {
			observed += fmt.Sprintf(" failed_error=%q", errorText)
		}
		if !failedAt.IsZero() {
			observed += " failed_at=" + failedAt.UTC().Format(time.RFC3339Nano)
		}
		if identity.host != "" {
			observed += fmt.Sprintf(
				" failed_host=%s failed_generation=%s failed_container=%s",
				identity.host,
				identity.generation,
				identity.container,
			)
		}
		retryHeartbeatObserved := retryActive.taskID != "" &&
			!retryActive.observedAt.IsZero() && !terminal.observedAt.IsZero() &&
			terminal.observedAt.Before(retryActive.observedAt)
		retryCompleted := hasTerminalCompletion &&
			!terminal.observedAt.IsZero() && terminal.observedAt.Unix() < terminalCompletion.unixSeconds
		if retryCompleted {
			retryObserved = true
			observed += fmt.Sprintf(
				" retry_phase=completed retry_duration_s=%d retry_completed_age_s=%d retry_completed_at=%s",
				terminalCompletion.durationSeconds,
				terminalCompletion.ageSeconds,
				time.Unix(terminalCompletion.unixSeconds, 0).UTC().Format(time.RFC3339),
			)
		} else if retryHeartbeatObserved {
			retryObserved = true
			observed += fmt.Sprintf(
				" retry_phase=active retry_last_heartbeat_duration_s=%d retry_observed_at=%s",
				retryActive.seconds,
				retryActive.observedAt.UTC().Format(time.RFC3339Nano),
			)
		}
		if (retryCompleted || retryHeartbeatObserved) && retryActive.identity.host != "" {
			observed += fmt.Sprintf(
				" retry_host=%s retry_generation=%s retry_container=%s",
				retryActive.identity.host,
				retryActive.identity.generation,
				retryActive.identity.container,
			)
		}
	}
	if phase == "completed" {
		observed += fmt.Sprintf(" completed_age_s=%d", ageSeconds)
	}

	phaseWithArticle := "a " + phase
	if phase == "active" {
		phaseWithArticle = "an active"
	}
	incidentContext := "Correlate, rather than conflate, this duration with open-contract five/30-minute buckets, the exact legacy payment-retention query, and transfer_contract autovacuum phase. Host/generation/container is chronology; executor overlap alone is not causal proof."
	if phase == "active" && failedPrecursorToActive {
		incidentContext += " The precursor fields preserve the latest deadline failure alongside the current active attempt; compare their executor identities and cohort timings without treating co-location as a uniquely identified mechanism."
	}
	retryEvidence := ""
	if retryObserved {
		retryEvidence = " The retry fields preserve the same task id's next observed lifecycle and executor; a fast peer retry is an A/B control for load sensitivity, not permission to erase the failed precursor."
		incidentContext += retryEvidence
	}
	return []finding{{
		probeId: "pg/close-duration-overrun", tier: tierWarn,
		class: "close-duration-overrun", target: target, frame: "CloseExpiredContracts", sustain: 1,
		symptom: fmt.Sprintf(
			"CloseExpiredContracts has %s checkpoint lasting %ds (healthy band < %ds)",
			phaseWithArticle,
			durationSeconds,
			int(closeDurationLimit/time.Second),
		),
		mechanism: "The deployed 100,000-contract task checkpoint commits contracts individually but acknowledges scheduler progress only when the whole cohort returns. Legacy payment-retention writes and transfer_contract vacuum debt can stretch that boundary to the 1,800-second task deadline; a timeout preserves per-contract commits but repeats discovery and loses the task-level checkpoint.",
		baseline:  "Healthy full legacy cohorts finish in roughly 20–30s; warn at 120s, and every checkpoint must finish well before the 1,800s deadline. Current source caps a checkpoint at 25,000 contracts.",
		observed:  observed,
		evidence:  "Taskworker eval-active is the live elapsed-time source; eval-error retains rescheduled deadline attempts that never become a finished duration; finished_task retains completed duration. The latest failed overrun remains visible for 45 minutes, including beside a newer active successor, so retry progress cannot erase the precursor." + retryEvidence,
		context:   incidentContext,
		action:    "Roll out the bounded retention queue and the 25,000-contract checkpoint, then let scheduled closes and autovacuum drain the debt. Do not raise the deadline, increase closer concurrency, cancel vacuum, or restart a taskworker to hide the elapsed heartbeat.",
		verify:    "Every active generation keeps close checkpoints below 120s, no exact-1,800s timeout recurs, and the older-than-five/30-minute open-contract buckets fall on consecutive samples.",
		playbook:  "SIGNALS.md §2.6 and §2.10",
	}}, nil
}
