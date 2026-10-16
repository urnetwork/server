// SIGNALS.md §2.5a observes durable writeback debt independently of CPU or metrics delivery.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"
)

func NewTransferDebitsSignal() Signal {
	return &signalAdapter{number: "2.5a", key: "transfer-debits", name: "Asynchronous transfer debit backlog", probe: transferDebitProbe{}}
}

type transferDebitProbe struct{}

func (transferDebitProbe) id() string             { return "pg/transfer-debits" }
func (transferDebitProbe) tier() string           { return tierWarn }
func (transferDebitProbe) cadence() time.Duration { return time.Minute }

// Two oldest-row index seeks per partition, not a count of the entire journal.
// A future lease is a task-claim fact, not proof of a live executing process.
const transferDebitBacklogQuery = `
WITH observed AS MATERIALIZED(SELECT clock_timestamp() AT TIME ZONE 'UTC' AS at)
SELECT extract(epoch FROM observed.at)::double precision,shard,
 COALESCE((SELECT extract(epoch FROM observed.at-create_time)::double precision FROM transfer_debit_journal WHERE shard=partition.shard AND NOT applied ORDER BY create_time LIMIT 1),-1),
 COALESCE((SELECT extract(epoch FROM observed.at-create_time)::double precision FROM transfer_debit_journal WHERE shard=partition.shard AND applied ORDER BY create_time LIMIT 1),-1),
 scheduled.run_once_key IS NOT NULL,COALESCE(scheduled.release_time>observed.at,false)
FROM generate_series(0,15) AS partition(shard) CROSS JOIN observed
LEFT JOIN LATERAL(SELECT run_once_key,release_time FROM pending_task
 WHERE run_once_key='["flush_transfer_debits_'||partition.shard||'"]' LIMIT 1) AS scheduled ON true
ORDER BY shard;`

type transferDebitObservation struct {
	shard             int
	pending, release  float64
	scheduled, leased bool
}

func parseTransferDebits(rows []pgRow, now time.Time) ([]transferDebitObservation, error) {
	invalid := fmt.Errorf("debit backlog observation is incomplete or invalid")
	if len(rows) != 16 {
		return nil, invalid
	}
	seen := map[int]bool{}
	result := make([]transferDebitObservation, 0, 16)
	for _, row := range rows {
		if len(row) != 6 {
			return nil, invalid
		}
		observed, err := strconv.ParseFloat(row.str(0), 64)
		if err != nil || math.IsNaN(observed) || math.IsInf(observed, 0) || math.Abs(float64(now.UnixNano())/1e9-observed) > 30 {
			return nil, invalid
		}
		shard, err := strconv.Atoi(row.str(1))
		if err != nil || shard < 0 || shard >= 16 || seen[shard] {
			return nil, invalid
		}
		seen[shard] = true
		pending, e1 := strconv.ParseFloat(row.str(2), 64)
		release, e2 := strconv.ParseFloat(row.str(3), 64)
		if e1 != nil || e2 != nil || math.IsNaN(pending) || math.IsNaN(release) || math.IsInf(pending, 0) || math.IsInf(release, 0) || pending < -1 || release < -1 {
			return nil, invalid
		}
		if row.str(4) != "t" && row.str(4) != "f" || row.str(5) != "t" && row.str(5) != "f" {
			return nil, invalid
		}
		result = append(result, transferDebitObservation{shard, pending, release, row.str(4) == "t", row.str(5) == "t"})
	}
	return result, nil
}
func (p transferDebitProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := env.runner.pg(bounded, `SELECT coalesce(max(end_version_number),0)::int,to_regclass('public.transfer_debit_journal') IS NOT NULL FROM migration_audit WHERE status='success'`)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("debit schema observation is incomplete")
	}
	head, err := strconv.Atoi(rows[0].str(0))
	if err != nil {
		return nil, fmt.Errorf("debit schema head is invalid")
	}
	if head < 758 {
		return nil, nil
	}
	if rows[0].str(1) != "t" {
		return nil, fmt.Errorf("debit schema unavailable at installed version")
	}
	rows, err = env.runner.pg(bounded, transferDebitBacklogQuery)
	if err != nil {
		return nil, err
	}
	observations, err := parseTransferDebits(rows, env.now())
	if err != nil {
		return nil, err
	}
	findings := make([]finding, 0, 32)
	for _, o := range observations {
		for _, state := range []struct {
			name string
			age  float64
		}{{"pending", o.pending}, {"redis_release", o.release}} {
			tier := tierWarn
			if state.age >= 300 {
				tier = tierPage
			}
			findings = append(findings, finding{probeId: p.id(), tier: tier, class: "debit-writeback-lag", target: pgTarget(env), frame: fmt.Sprintf("partition-%d/%s", o.shard, state.name), healthy: state.age < 60, sustain: 1,
				symptom:   "Asynchronous payer consumption has not drained within its operational budget.",
				observed:  fmt.Sprintf("oldest_seconds=%.3f partition=%d state=%s scheduled=%t future_task_lease=%t", state.age, o.shard, state.name, o.scheduled, o.leased),
				baseline:  "WARN at 60s and PAGE at 300s; design escalation bands, not measured Main capacity. Empty=-1.",
				mechanism: "Pending records await grouped PostgreSQL debit; redis_release records already committed and await reservation release/cleanup. A task lease does not prove a live owner.",
				action:    "Check the partition cursor and actual task owner, bounded SQL waits, Redis release errors and worker image. Retain journal rows; do not release reservations or delete debt manually.",
				verify:    "Observe renewed successful worker completions and oldest age below 60s, with all 16 partitions and current source clock visible.", playbook: "SIGNALS.md §2.5a"})
		}
		findings = append(findings, finding{probeId: p.id(), tier: tierWarn, class: "debit-worker-missing", target: pgTarget(env), frame: fmt.Sprintf("partition-%d", o.shard), healthy: o.scheduled, sustain: 2,
			symptom: "An asynchronous debit partition has no scheduled recovery task.", observed: fmt.Sprintf("partition=%d scheduled=%t", o.shard, o.scheduled), baseline: "16 distinct scheduled keys; two observations exclude a brief completion/post transition.",
			mechanism: "Logical partition count is not actual worker capacity; missing tasks can leave durable debt while CPU falls.", action: "Check current taskworker startup registration, selected workload and post completion. Preserve debt and restore the owning scheduling path.", verify: "All partition keys recur and their source-qualified worker completions advance.", playbook: "SIGNALS.md §2.5a"})
	}
	return findings, nil
}
