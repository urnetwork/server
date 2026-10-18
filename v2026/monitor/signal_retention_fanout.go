package monitor

import (
	"context"
	"fmt"
	"time"
)

const retentionFanoutQueryID = "-3312164664690273449"
const retentionFanoutRowsPerCallEvidence = 100_000

// Signal retention-fanout implements SIGNALS.md §2.10.
func NewRetentionFanoutSignal() Signal {
	return &signalAdapter{number: "2.10", key: "retention-fanout", name: "Payment-completion retention fan-out", probe: pgRetentionFanoutProbe{}}
}

type pgRetentionFanoutProbe struct{}

func (pgRetentionFanoutProbe) id() string             { return "pg/retention-fanout" }
func (pgRetentionFanoutProbe) tier() string           { return tierWarn }
func (pgRetentionFanoutProbe) cadence() time.Duration { return time.Minute }

func (pgRetentionFanoutProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH activity AS (
		 SELECT count(*) AS active,
		        coalesce(round(extract(epoch FROM max(clock_timestamp()-query_start))),0)::int AS oldest_s
		 FROM pg_stat_activity WHERE state='active' AND query_id=-3312164664690273449
		), statement AS (
		 SELECT coalesce(calls,0) AS calls, coalesce(round(mean_exec_time),0) AS mean_ms,
		        coalesce(round(max_exec_time),0) AS max_ms,
		        coalesce(round(rows/NULLIF(calls,0)),0) AS rows_per_call,
		        coalesce(shared_blks_hit,0) AS shared_blks_hit,
		        coalesce(shared_blks_read,0) AS shared_blks_read,
		        coalesce(shared_blks_dirtied,0) AS shared_blks_dirtied,
		        coalesce(shared_blks_written,0) AS shared_blks_written
		 FROM pg_stat_statements WHERE queryid=-3312164664690273449
		), task_failure AS (
		 SELECT count(*) FILTER (
		          WHERE reschedule_error_count > 0
		        ) AS advance_failures,
		        count(*) FILTER (
		          WHERE reschedule_error ILIKE '%failed to deallocate cached statement(s): conn closed%'
		        ) AS deadline_cleanup_failures
		 FROM pending_task
		 WHERE function_name LIKE '%AdvancePayment'
		)
		SELECT a.active, a.oldest_s, coalesce(s.calls,0), coalesce(s.mean_ms,0),
		       coalesce(s.max_ms,0), coalesce(s.rows_per_call,0), coalesce(s.shared_blks_hit,0),
		       coalesce(s.shared_blks_read,0), coalesce(s.shared_blks_dirtied,0), coalesce(s.shared_blks_written,0),
		       tf.advance_failures, tf.deadline_cleanup_failures
		FROM activity a
		LEFT JOIN statement s ON true
		CROSS JOIN task_failure tf;
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("retention fan-out query returned no rows")
	}
	active, oldest := atoiRow(rows[0], 0), atoiRow(rows[0], 1)
	rowsPerCall := atoiRow(rows[0], 5)
	advanceFailures, deadlineCleanupFailures := atoiRow(rows[0], 10), atoiRow(rows[0], 11)
	durableDeadlineEvidence := 0 < deadlineCleanupFailures && retentionFanoutRowsPerCallEvidence <= rowsPerCall
	activeIncident := 0 < active && (2 <= active || 30 < oldest)
	if !activeIncident && !durableDeadlineEvidence {
		return []finding{healthyFinding("pg/retention-fanout", tierWarn, "retention-fanout", pgTarget(env))}, nil
	}
	var symptom string
	if activeIncident {
		symptom = fmt.Sprintf("payment retention query has %d active execution(s), oldest %ds", active, oldest)
	}
	if durableDeadlineEvidence && symptom == "" {
		symptom = fmt.Sprintf("%d AdvancePayment row(s) retain the 120s connection-cleanup deadline signature while the legacy retention query averages %d rows/call", deadlineCleanupFailures, rowsPerCall)
	} else if 0 < deadlineCleanupFailures {
		symptom += fmt.Sprintf("; %d AdvancePayment row(s) retain the 120s connection-cleanup deadline signature", deadlineCleanupFailures)
	}
	return []finding{{
		probeId: "pg/retention-fanout", tier: tierWarn,
		class: "retention-fanout", target: pgTarget(env), frame: retentionFanoutQueryID, sustain: 2,
		symptom:   symptom,
		mechanism: "The deployed CompletePayment path still performs one synchronous UPDATE for every transfer_contract referenced by a payment's transfer_escrow_sweep rows. A payment with millions of contracts can therefore hit AdvancePayment's 120-second task deadline; cancellation closes the PostgreSQL connection and the cleanup error can obscure the expensive statement that consumed the deadline. The same write burst creates vacuum debt and delays unrelated close work.",
		baseline:  "No legacy execution remains active beyond 30s, fewer than two overlap, and AdvancePayment has no new 120-second connection-cleanup failures attributable to completion retention.",
		observed: fmt.Sprintf("active=%d oldest_s=%d calls=%s mean_ms=%s max_ms=%s rows_per_call=%s shared_hit=%s shared_read=%s dirtied=%s written=%s advance_failures=%d advance_deadline_cleanup_failures=%d",
			active, oldest, rows[0].str(2), rows[0].str(3), rows[0].str(4), rows[0].str(5), rows[0].str(6), rows[0].str(7), rows[0].str(8), rows[0].str(9), advanceFailures, deadlineCleanupFailures),
		action:   "Deploy the CompletePayment path that commits processor completion and only sets contract_retention_pending. Let RemoveCompletedContracts advance contract_retention_cursor in bounded, committed keyset batches. Preserve the processor idempotency key; do not raise the global task deadline, manually replay payments, or kill an active payment without proving retry safety.",
		verify:   "Query ID -3312164664690273449 disappears, no new AdvancePayment attempt reaches the 120-second cleanup signature, queued contract-retention cursors advance and drain, payment completion remains idempotent, and transfer_contract vacuum/close throughput recovers.",
		playbook: "SIGNALS.md §2.10",
	}}, nil
}
