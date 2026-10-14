// SIGNALS.md §2.5b retains visibility of legacy financial work and failures.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"
)

func NewLegacySettlementsSignal() Signal {
	return &signalAdapter{number: "2.5b", key: "legacy-settlements", name: "Legacy settlement recovery", probe: legacySettlementProbe{}}
}

type legacySettlementProbe struct{}

func (legacySettlementProbe) id() string             { return "pg/legacy-settlements" }
func (legacySettlementProbe) tier() string           { return tierWarn }
func (legacySettlementProbe) cadence() time.Duration { return time.Minute }

// Three oldest-row seeks per partition use the (shard,failure_code,create_time)
// index. Backoff never hides a failed intent; no full-table count is required.
const legacySettlementBacklogQuery = `
WITH observed AS MATERIALIZED(SELECT clock_timestamp() AT TIME ZONE 'UTC' AS at)
SELECT extract(epoch FROM observed.at)::double precision,shard,
 COALESCE((SELECT extract(epoch FROM observed.at-create_time)::double precision FROM legacy_settlement_intent WHERE shard=partition.shard AND failure_code='none' ORDER BY create_time LIMIT 1),-1),
 COALESCE((SELECT extract(epoch FROM observed.at-create_time)::double precision FROM legacy_settlement_intent WHERE shard=partition.shard AND failure_code='accounting' ORDER BY create_time LIMIT 1),-1),
 COALESCE((SELECT extract(epoch FROM observed.at-create_time)::double precision FROM legacy_settlement_intent WHERE shard=partition.shard AND failure_code='operational' ORDER BY create_time LIMIT 1),-1),
 scheduled.run_once_key IS NOT NULL,COALESCE(scheduled.release_time>observed.at,false)
FROM generate_series(0,15) AS partition(shard) CROSS JOIN observed
LEFT JOIN LATERAL(SELECT run_once_key,release_time FROM pending_task
 WHERE run_once_key='["flush_legacy_settlements_'||partition.shard||'"]' LIMIT 1) AS scheduled ON true
ORDER BY shard;`

type legacySettlementObservation struct {
	shard             int
	ages              [3]float64
	scheduled, leased bool
}

func parseLegacySettlements(rows []pgRow, now time.Time) ([]legacySettlementObservation, error) {
	invalid := fmt.Errorf("legacy settlement observation is incomplete or invalid")
	if len(rows) != 16 {
		return nil, invalid
	}
	seen := map[int]bool{}
	result := make([]legacySettlementObservation, 0, 16)
	var sourceClock float64
	for i, row := range rows {
		if len(row) != 7 {
			return nil, invalid
		}
		observed, err := strconv.ParseFloat(row.str(0), 64)
		if err != nil || math.IsNaN(observed) || math.IsInf(observed, 0) || math.Abs(float64(now.UnixNano())/1e9-observed) > 30 {
			return nil, invalid
		}
		if i == 0 {
			sourceClock = observed
		} else if observed != sourceClock {
			return nil, invalid
		}
		shard, err := strconv.Atoi(row.str(1))
		if err != nil || shard < 0 || shard >= 16 || seen[shard] {
			return nil, invalid
		}
		seen[shard] = true
		o := legacySettlementObservation{shard: shard}
		for j := range o.ages {
			age, err := strconv.ParseFloat(row.str(j+2), 64)
			if err != nil || math.IsNaN(age) || math.IsInf(age, 0) || age < 0 && age != -1 {
				return nil, invalid
			}
			o.ages[j] = age
		}
		if row.str(5) != "t" && row.str(5) != "f" || row.str(6) != "t" && row.str(6) != "f" {
			return nil, invalid
		}
		o.scheduled, o.leased = row.str(5) == "t", row.str(6) == "t"
		result = append(result, o)
	}
	return result, nil
}
func (p legacySettlementProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := env.runner.pg(bounded, `SELECT coalesce(max(end_version_number),0)::int,to_regclass('public.legacy_settlement_intent') IS NOT NULL FROM migration_audit WHERE status='success'`)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("legacy settlement schema observation is incomplete")
	}
	head, err := strconv.Atoi(rows[0].str(0))
	if err != nil || head < 0 || rows[0].str(1) != "t" && rows[0].str(1) != "f" {
		return nil, fmt.Errorf("legacy settlement schema observation is invalid")
	}
	if head < 763 {
		return nil, nil
	}
	if rows[0].str(1) != "t" {
		return nil, fmt.Errorf("legacy settlement schema unavailable at installed version")
	}
	rows, err = env.runner.pg(bounded, legacySettlementBacklogQuery)
	if err != nil {
		return nil, err
	}
	observations, err := parseLegacySettlements(rows, env.now())
	if err != nil {
		return nil, err
	}
	findings := make([]finding, 0, 64)
	for _, o := range observations {
		for i, state := range []string{"pending", "accounting", "operational"} {
			age := o.ages[i]
			tier := tierWarn
			if age >= 300 {
				tier = tierPage
			}
			healthy := age == -1 || i == 0 && age < 60
			findings = append(findings, finding{probeId: p.id(), tier: tier, class: "legacy-settlement-pending", target: pgTarget(env), frame: fmt.Sprintf("partition-%d/%s", o.shard, state), healthy: healthy, sustain: 1,
				symptom:   "A legacy settlement remains reserved and has not completed its durable financial transaction.",
				observed:  fmt.Sprintf("oldest_seconds=%.3f partition=%d state=%s scheduled=%t future_task_lease=%t", age, o.shard, state, o.scheduled, o.leased),
				baseline:  "Pending WARN at 60s, retained failures WARN immediately, all states PAGE at 300s. Design bands, not measured Main capacity. Empty=-1.",
				mechanism: "An acknowledged intent retains its reservation until debit, provider payouts, metadata and outcome commit together. A retry delay or future lease does not establish recovery.",
				action:    "Check the actual worker generation, cursor, original finite failure and bounded SQL waits. Preserve the intent and its reservation; never count deferred work as completed accounting.",
				verify:    "All 16 source-clock-qualified partitions are visible, failures drain and oldest pending age falls below 60s.", playbook: "SIGNALS.md §2.5b"})
		}
		findings = append(findings, finding{probeId: p.id(), tier: tierWarn, class: "legacy-settlement-worker-missing", target: pgTarget(env), frame: fmt.Sprintf("partition-%d", o.shard), healthy: o.scheduled, sustain: 2,
			symptom: "A legacy settlement partition has no scheduled recovery task.", observed: fmt.Sprintf("partition=%d scheduled=%t", o.shard, o.scheduled), baseline: "16 independent keys; two observations allow a short completion/post transition.",
			mechanism: "An old worker cannot finalize an accepted intent. Removing the new worker before draining intents leaves durable work reserved.", action: "Restore the owning scheduler and a worker that implements migration 763. Keep that worker through rollback until all pending intents drain.", verify: "Every partition key recurs and actual worker completions advance.", playbook: "SIGNALS.md §2.5b"})
	}
	return findings, nil
}
