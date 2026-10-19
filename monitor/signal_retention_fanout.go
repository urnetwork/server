package monitor

import (
	"context"
	"fmt"
	"strings"
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
	// Activity and task aggregates repeat across the query's statistics partitions.
	// Validate every partition before deciding health or retaining evidence.
	var active, oldest, rowsPerCall, advanceFailures, deadlineCleanupFailures int64
	statementMetrics := make([]string, 0, len(rows))
	for i, row := range rows {
		values, err := pgAggregateIntegers([]pgRow{row}, 12)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			active, oldest, advanceFailures, deadlineCleanupFailures = values[0], values[1], values[10], values[11]
		} else if active != values[0] || oldest != values[1] || advanceFailures != values[10] || deadlineCleanupFailures != values[11] {
			return nil, fmt.Errorf("invalid response for retention fan-out aggregate: inconsistent repeated counts")
		}
		if rowsPerCall < values[5] {
			rowsPerCall = values[5]
		}
		statementMetrics = append(statementMetrics, fmt.Sprintf("calls=%d mean_ms=%d max_ms=%d rows_per_call=%d shared_hit=%d shared_read=%d dirtied=%d written=%d",
			values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]))
	}
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
		symptom = fmt.Sprintf("%d AdvancePayment row(s) retain the 120s connection-cleanup deadline signature while the legacy retention query averages %d rows/call in at least one statistics partition", deadlineCleanupFailures, rowsPerCall)
	} else if 0 < deadlineCleanupFailures {
		symptom += fmt.Sprintf("; %d AdvancePayment row(s) retain the 120s connection-cleanup deadline signature", deadlineCleanupFailures)
	}
	return []finding{{
		probeId: "pg/retention-fanout", tier: tierWarn,
		class: "retention-fanout", target: pgTarget(env), frame: retentionFanoutQueryID, sustain: 2,
		symptom:   symptom,
		mechanism: "The deployed CompletePayment path still performs one synchronous UPDATE for every transfer_contract referenced by a payment's transfer_escrow_sweep rows. A payment with millions of contracts can therefore hit AdvancePayment's 120-second task deadline; cancellation closes the PostgreSQL connection and the cleanup error can obscure the expensive statement that consumed the deadline. The same write burst creates vacuum debt and delays unrelated close work.",
		baseline:  "No legacy execution remains active beyond 30s, fewer than two overlap, and AdvancePayment has no new 120-second connection-cleanup failures attributable to completion retention.",
		observed: fmt.Sprintf("active=%d oldest_s=%d advance_failures=%d advance_deadline_cleanup_failures=%d statement_partitions=%d statement_metrics=[%s]",
			active, oldest, advanceFailures, deadlineCleanupFailures, len(rows), strings.Join(statementMetrics, "; ")),
		action:   "Deploy the CompletePayment path that commits processor completion and only sets contract_retention_pending. Let RemoveCompletedContracts advance contract_retention_cursor in bounded, committed keyset batches. Preserve the processor idempotency key; do not raise the global task deadline, manually replay payments, or kill an active payment without proving retry safety.",
		verify:   "Query ID -3312164664690273449 disappears, no new AdvancePayment attempt reaches the 120-second cleanup signature, queued contract-retention cursors advance and drain, payment completion remains idempotent, and transfer_contract vacuum/close throughput recovers.",
		playbook: "SIGNALS.md §2.10",
	}}, nil
}
