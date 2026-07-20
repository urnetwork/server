// pg tier-0 probes: the state split (SIGNALS.md 1.3) and the contract
// creation rate (1.1).
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// pgStateProbe is SIGNALS.md 1.3: the active / idle-in-tx state split, the
// tier-0 read of pg load and the redis-latency mirror. It emits two signals:
// active-pileup (page, §7) and idle-in-tx (warn, §7). When either trips it
// runs the 1.3 / 5.8 escalation batteries — once per trip, not per tick
// (batteryLatch) — so the ticket names the real query load without the
// batteries themselves landing load on the sick target every minute.
type pgStateProbe struct {
	batteries *batteryLatch
}

func newPgStateProbe() *pgStateProbe {
	return &pgStateProbe{batteries: newBatteryLatch()}
}

func (self *pgStateProbe) id() string             { return "pg/state-split" }
func (self *pgStateProbe) tier() string           { return tierPage }
func (self *pgStateProbe) cadence() time.Duration { return 60 * time.Second }

func (self *pgStateProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}

	rows, err := env.runner.pg(ctx, `
		SELECT
			count(*) FILTER (WHERE state='active') AS active,
			count(*) FILTER (WHERE state='idle in transaction') AS idle_in_tx,
			coalesce(round(extract(epoch FROM max(now()-xact_start) FILTER (WHERE state='idle in transaction'))),0) AS oldest_idle_s,
			count(*) AS total_client
		FROM pg_stat_activity WHERE backend_type='client backend';
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("pg state query returned no rows")
	}
	active := atoiRow(rows[0], 0)
	idleInTx := atoiRow(rows[0], 1)
	oldestIdleS := atoiRow(rows[0], 2)
	totalClient := atoiRow(rows[0], 3)

	findings := []finding{}

	// active-pileup (page): active > 100 for 2 min (SIGNALS.md §7 / 1.3 / 5.8)
	if active > 100 {
		evidence := self.batteries.broken("active-pileup", func() string {
			// full 5.8 discriminators: query_id grouping + statements/idx_scan
			// deltas + landmine check
			return activeBattery(ctx, env) + "\n" + planWallBattery(ctx, env)
		})
		findings = append(findings, finding{
			probeId: "pg/active-pileup", tier: tierPage,
			class: "active-pileup", target: target, sustain: 2,
			symptom:  fmt.Sprintf("pg (%s) active client backends = %d (threshold > 100 for 2m)", target, active),
			baseline: "active 2–6 healthy; real active backends were ~6 even during past incidents (1.3)",
			observed: fmt.Sprintf("active=%d idle_in_tx=%d total_client=%d", active, idleInTx, totalClient),
			evidence: evidence,
			playbook: "SIGNALS.md 5.8",
		})
	} else {
		self.batteries.healthy("active-pileup")
		findings = append(findings, healthyFinding("pg/active-pileup", tierPage, "active-pileup", target))
	}

	// idle-in-tx (warn): count > 100 or oldest > 30 min (SIGNALS.md §7 / 1.3 / 5.6)
	if idleInTx > 100 || oldestIdleS > 30*60 {
		findings = append(findings, finding{
			probeId: "pg/idle-in-tx", tier: tierWarn,
			class: "idle-in-tx", target: target, sustain: 2,
			symptom:  fmt.Sprintf("pg (%s) idle-in-transaction = %d, oldest %ds (threshold > 100 / > 30m)", target, idleInTx, oldestIdleS),
			baseline: "idle_in_tx < 30 healthy (2 when healthy); > 100 = redis latency leaking into pg through tx-scoped calls (1.3)",
			observed: fmt.Sprintf("idle_in_tx=%d oldest=%ds active=%d", idleInTx, oldestIdleS, active),
			evidence: self.batteries.broken("idle-in-tx", func() string {
				return idleTxBattery(ctx, env)
			}),
			playbook: "SIGNALS.md 5.6",
		})
	} else {
		self.batteries.healthy("idle-in-tx")
		findings = append(findings, healthyFinding("pg/idle-in-tx", tierWarn, "idle-in-tx", target))
	}

	return findings, nil
}

// activeBattery groups active backends by query_id (SIGNALS.md 5.8 step 1) —
// if 1–3 shapes own the pile it is a plan problem, not organic load.
func activeBattery(ctx context.Context, env *probeEnv) string {
	rows, err := env.runner.pg(ctx, `
		SELECT query_id, count(*) AS backends,
		       coalesce(string_agg(DISTINCT coalesce(wait_event_type,'-')||':'||coalesce(wait_event,'-'), ','),'-') AS waits,
		       left(regexp_replace(min(query),'\s+',' ','g'),90) AS sample
		FROM pg_stat_activity
		WHERE backend_type='client backend' AND state='active' AND pid<>pg_backend_pid()
		GROUP BY query_id ORDER BY backends DESC LIMIT 5;
	`)
	if err != nil {
		return "active battery failed: " + err.Error()
	}
	lines := []string{"top active query_ids:"}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("  qid=%s backends=%s waits=%s :: %s",
			r.str(0), r.str(1), r.str(2), r.str(3)))
	}
	return strings.Join(lines, "\n")
}

// idleTxBattery groups idle-in-tx backends by last query shape (SIGNALS.md
// 5.6) — names the tx-scoped redis coupling sites that are stalling.
func idleTxBattery(ctx context.Context, env *probeEnv) string {
	rows, err := env.runner.pg(ctx, `
		SELECT count(*) AS backends,
		       coalesce(round(extract(epoch FROM max(now()-state_change))),0) AS oldest_s,
		       left(regexp_replace(coalesce(query,''),'\s+',' ','g'),90) AS last_query
		FROM pg_stat_activity
		WHERE backend_type='client backend' AND state='idle in transaction'
		GROUP BY left(regexp_replace(coalesce(query,''),'\s+',' ','g'),90)
		ORDER BY backends DESC LIMIT 6;
	`)
	if err != nil {
		return "idle-tx battery failed: " + err.Error()
	}
	lines := []string{"idle-in-tx by last query shape:"}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("  backends=%s oldest=%ss :: %s", r.str(0), r.str(1), r.str(2)))
	}
	return strings.Join(lines, "\n")
}

// pgContractRateProbe is SIGNALS.md 1.1: the user-facing throughput proxy.
// The static floor (< 1,000/min = outage) always applies; with enough local
// history the learned band takes over: < 50% of the trailing-hour median for
// 3 consecutive minutes = brownout. Every reading is recorded to the baseline
// store, so the band self-assembles after ~30 minutes of running.
type pgContractRateProbe struct{}

const contractRateMetric = "pg/contract-rate"

func (self pgContractRateProbe) id() string             { return "pg/contracts-collapse" }
func (self pgContractRateProbe) tier() string           { return tierPage }
func (self pgContractRateProbe) cadence() time.Duration { return 60 * time.Second }

func (self pgContractRateProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
		SELECT count(*) FROM transfer_contract
		WHERE create_time >= date_trunc('minute',now())-interval '1 minute'
		  AND create_time <  date_trunc('minute',now());
	`)
	if err != nil {
		return nil, err
	}
	rate := atoiRow(rows[0], 0)

	// learned band: trailing-hour median (needs >= 30 samples). Read before
	// recording so the median reflects history, not this reading's own
	// collapse.
	var median float64
	var haveBaseline bool
	if env.baseline != nil {
		median, _, haveBaseline = env.baseline.trailingMedian(contractRateMetric, time.Hour, 30)
		env.baseline.record(contractRateMetric, time.Now(), float64(rate))
	}

	// a churn window pollutes the trailing-hour median: the 2026-07-19
	// ansible restart wave drove contracts to ~11k/min for 40 min, and the
	// recovery back to the ~4.5k baseline then paged as a < 50% "collapse".
	// When the hour median is itself >= 1.5x the trailing-6h median, judge
	// against the longer window instead
	if haveBaseline && env.baseline != nil {
		if longMedian, _, haveLong := env.baseline.trailingMedian(contractRateMetric, 6*time.Hour, 120); haveLong && median >= 1.5*longMedian {
			median = longMedian
		}
	}

	baselineText := func() string {
		if haveBaseline {
			return fmt.Sprintf("trailing median %.0f/min (learned); daily cycle 8,000–13,000/min; < 1,000 observed only during full redis outage", median)
		}
		return "8,000–13,000/min daily cycle (static; learned band pending history); < 1,000/min observed only during full redis outage"
	}

	switch {
	case rate < 1000:
		return []finding{{
			probeId: "pg/contracts-collapse", tier: tierPage,
			class: "contracts-collapse", target: target, sustain: 3,
			symptom:  fmt.Sprintf("pg (%s) contract creation = %d/min (static outage floor < 1,000)", target, rate),
			baseline: baselineText(),
			observed: fmt.Sprintf("contracts_last_min=%d", rate),
			context:  "correlate with deploys/restarts in the last 10 min",
			playbook: "SIGNALS.md 5.1",
		}}, nil
	case haveBaseline && median >= 2000 && float64(rate) < 0.5*median:
		// brownout: < 50% of trailing-hour median for 3 consecutive minutes.
		// The median >= 2000 guard keeps the learned band meaningful during
		// overnight lows where 50% of a small median is noise. Same class as
		// the collapse case so one healthy finding resolves both.
		return []finding{{
			probeId: "pg/contracts-collapse", tier: tierPage,
			class: "contracts-collapse", target: target, sustain: 3,
			symptom: fmt.Sprintf("pg (%s) contract creation = %d/min, < 50%% of the trailing median %.0f/min",
				target, rate, median),
			baseline: baselineText(),
			observed: fmt.Sprintf("contracts_last_min=%d trailing_median=%.0f ratio=%.0f%%", rate, median, 100*float64(rate)/median),
			context:  "cliff = systemic (cluster state, deploy); sag = partial failure; ramp = recovery (1.1)",
			playbook: "SIGNALS.md 5.1",
		}}, nil
	}
	return []finding{healthyFinding("pg/contracts-collapse", tierPage, "contracts-collapse", target)}, nil
}
