package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SIGNALS.md §2.1 maps to signal_active_queries.go and signal_active_queries_test.go.
func NewActiveQueriesSignal() Signal {
	return &signalAdapter{number: "2.1", key: "active-queries", name: "Active query sampling", probe: pgActiveQueryProbe{}}
}

type pgActiveQueryProbe struct{}

const concurrentReindexActiveGrace = 2 * time.Hour

func expectedConcurrentReindex(sample string, oldestSeconds int) bool {
	upper := strings.ToUpper(strings.TrimSpace(sample))
	isConcurrentReindex := strings.HasPrefix(upper, "REINDEX TABLE CONCURRENTLY ") ||
		strings.HasPrefix(upper, "REINDEX INDEX CONCURRENTLY ")
	return isConcurrentReindex && time.Duration(oldestSeconds)*time.Second < concurrentReindexActiveGrace
}

func legacyPayoutSubsidyRangeQuery(sample string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(sample), " "))
	return strings.Contains(normalized, "min(transfer_contract.create_time)") &&
		strings.Contains(normalized, "max(transfer_contract.close_time)") &&
		strings.Contains(normalized, "from transfer_escrow_sweep")
}

func (pgActiveQueryProbe) id() string             { return "pg/persistent-active-query" }
func (pgActiveQueryProbe) tier() string           { return tierWarn }
func (pgActiveQueryProbe) cadence() time.Duration { return 5 * time.Minute }

func (pgActiveQueryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := pgTarget(env)
	rows, err := env.runner.pg(ctx, `
		WITH active AS MATERIALIZED (
			SELECT query_id, count(*) AS active_count,
			       round(extract(epoch FROM max(clock_timestamp()-query_start)))::int AS oldest_seconds,
			       left(regexp_replace(min(query),'\s+',' ','g'),240) AS sample
			FROM pg_stat_activity
			WHERE backend_type='client backend' AND state='active'
			  AND pid<>pg_backend_pid()
			  AND coalesce(query,'') !~* 'client_reliability_running|contract_close|pending_task'
			GROUP BY query_id
			HAVING max(clock_timestamp()-query_start) > interval '2 minutes'
		), history AS (
			SELECT queryid,
			       round(sum(total_exec_time)/nullif(sum(calls),0)/1000)::int AS mean_exec_seconds
			FROM pg_stat_statements
			WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
			GROUP BY queryid
		)
		SELECT coalesce(active.query_id::text,'unknown'), active.active_count,
		       active.oldest_seconds, active.sample,
		       coalesce(history.mean_exec_seconds,0)
		FROM active
		LEFT JOIN history ON history.queryid=active.query_id
		ORDER BY active.oldest_seconds DESC LIMIT 10;
	`)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	for _, row := range rows {
		activeCount := atoi(row.str(1))
		oldestSeconds := atoi(row.str(2))
		historicalMeanSeconds := atoi(row.str(4))
		legacyPayoutRange := legacyPayoutSubsidyRangeQuery(row.str(3))
		if expectedConcurrentReindex(row.str(3), oldestSeconds) {
			// DbMaintenance explicitly gives each background CONCURRENTLY
			// reindex two hours. Utility statements do not appear in
			// pg_stat_statements, so history=0 would otherwise alert every
			// healthy daily rotation after two minutes (a 40kB table was
			// observed completing normally after 420s).
			continue
		}
		// A single known heavy query is not anomalous merely because it is
		// older than two minutes. Production's reliability-score INSERT, for
		// example, averaged 640s across 99 completed calls and correctly ran
		// for 877s. Alert when the shape is new, exceeds twice its completed
		// mean, or piles up concurrently; pg/active-pileup independently pages
		// the whole-backend saturation case.
		if !legacyPayoutRange && activeCount < 5 && historicalMeanSeconds > 0 && oldestSeconds <= 2*historicalMeanSeconds {
			continue
		}
		alert := finding{
			probeId: "pg/persistent-active-query", tier: tierWarn,
			class: "persistent-active-query", target: target, frame: row.str(0), sustain: 2,
			symptom:   fmt.Sprintf("query %s has %s active backend(s), oldest %ss", row.str(0), row.str(1), row.str(2)),
			mechanism: "A query shape remaining active across samples is load; a short connection count snapshot alone cannot distinguish it.",
			baseline:  "Known heavy shapes may exceed two minutes; one is anomalous after 2x its completed mean. Five concurrent copies are anomalous regardless of history.",
			observed:  fmt.Sprintf("query_id=%s active=%s oldest_s=%s historical_mean_s=%d", row.str(0), row.str(1), row.str(2), historicalMeanSeconds),
			evidence:  "sample: " + row.str(3),
			action:    "Compare this query ID with pg_stat_statements deltas and its plan before changing pool sizes or terminating a backend.",
			verify:    "The query completes below twice its historical mean and concurrent copies return below five.",
			playbook:  "SIGNALS.md §2.1 and §5.8",
		}
		if legacyPayoutRange {
			alert.sustain = 1
			alert.symptom = fmt.Sprintf("Payout subsidy-range query %s has scanned the complete sweep history for %ss", row.str(0), row.str(2))
			alert.mechanism = "The deployed planner already materializes its exact unpaid/safely-canceled and optionally close-time-bounded rows in temp_account_payment, but this later MIN/MAX query ignores that selected set and reads all of transfer_escrow_sweep. The historical scan is both semantically overbroad and proportional to the lifetime sweep table rather than this payout slice."
			alert.baseline = "The subsidy range is computed from temp_account_payment; no active query reads the complete transfer_escrow_sweep history to derive one payout epoch."
			alert.evidence = "legacy sample: " + row.str(3)
			alert.context = "Completed-history means can normalize a repeatedly deployed defect, so this exact legacy shape remains actionable even while its current duration is below twice that bad mean. The task heartbeat and MaxTime still bound the in-flight attempt; its age alone is not permission to cancel it."
			alert.action = "Deploy the taskworker payment planner that joins transfer_contract to temp_account_payment for this MIN/MAX. Let the current attempt reach its bounded outcome; do not add an index for the redundant full-history query or manually replay the Payout row."
			alert.verify = "After deployment, pg_stat_activity and pg_stat_statements show no new legacy subsidy-range executions, the selected-set query finishes within the bounded Payout slice, and the same Payout task commits and clears its error."
			alert.playbook = "SIGNALS.md §2.1 and §5.7"
		}
		findings = append(findings, alert)
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/persistent-active-query", tierWarn, "persistent-active-query", target))
	}
	return findings, nil
}
