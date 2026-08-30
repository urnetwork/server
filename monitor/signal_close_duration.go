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
	rows, err := env.runner.pg(ctx, `
		SELECT task_id::text,
		       'completed'::text,
		       round(extract(epoch FROM run_end_time-run_start_time))::int AS duration_s,
		       round(extract(epoch FROM now()-run_end_time))::int AS age_s,
		       extract(epoch FROM run_end_time)::bigint AS completed_unix_s
		FROM finished_task
		WHERE split_part(function_name,'.',3) = 'CloseExpiredContracts'
		  AND run_end_time > now() - interval '45 minutes'
		  AND run_end_time-run_start_time >= interval '120 seconds'
		ORDER BY run_end_time DESC
		LIMIT 1;
	`)
	if err != nil {
		return nil, err
	}

	activeLog, activeLogErr := env.runner.warpctl(
		ctx,
		"logs", env.cfg.env, "taskworker",
		fmt.Sprintf("--since=%dm", int(closeDurationIncidentLookback/time.Minute)),
		"--limit=5000", "--query=CloseExpiredContracts", "--utc",
	)
	active := parseTaskActiveRun(activeLog, "CloseExpiredContracts")
	terminal := parseTaskTerminalRun(activeLog, "CloseExpiredContracts")

	completedTaskID := ""
	completedDurationSeconds := 0
	completedAgeSeconds := 0
	completedUnixSeconds := int64(0)
	if len(rows) > 0 {
		completedTaskID = rows[0].str(0)
		completedDurationSeconds = atoi(rows[0].str(2))
		completedAgeSeconds = atoi(rows[0].str(3))
		completedUnixSeconds = atoi64(rows[0].str(4))
	}
	completionSupersedesHeartbeat := len(rows) > 0 && completedTaskSupersedesHeartbeat(
		completedTaskID,
		completedDurationSeconds,
		completedAgeSeconds,
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
		(len(rows) == 0 || terminal.observedAt.IsZero() || completedUnixSeconds == 0 ||
			completedUnixSeconds <= terminal.observedAt.Unix())
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
	} else if len(rows) > 0 {
		phase = rows[0].str(1)
		taskID = completedTaskID
		durationSeconds = completedDurationSeconds
		ageSeconds = completedAgeSeconds
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
		evidence:  "Taskworker eval-active is the live elapsed-time source; eval-error retains rescheduled deadline attempts that never become a finished duration; finished_task retains completed duration. The latest failed overrun remains visible for 45 minutes, including beside a newer active successor, so retry progress cannot erase the precursor.",
		context:   incidentContext,
		action:    "Roll out the bounded retention queue and the 25,000-contract checkpoint, then let scheduled closes and autovacuum drain the debt. Do not raise the deadline, increase closer concurrency, cancel vacuum, or restart a taskworker to hide the elapsed heartbeat.",
		verify:    "Every active generation keeps close checkpoints below 120s, no exact-1,800s timeout recurs, and the older-than-five/30-minute open-contract buckets fall on consecutive samples.",
		playbook:  "SIGNALS.md §2.6 and §2.10",
	}}, nil
}
