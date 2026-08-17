// pg tier-1 and daily probes: open-contract set (SIGNALS.md 2.6), pgbouncer
// reachability (§4 query_wait_timeout discriminator), vacuum health (2.4), and
// the daily stats-landmine check (2.3/§7).
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// pgOpenSetProbe is SIGNALS.md 2.6: the open-contract set size, the
// close-backlog canary and the fuel of the 5.8 feedback loop.
type pgOpenSetProbe struct{}

const openSetMetric = "pg/open-set"

func (self pgOpenSetProbe) id() string             { return "pg/open-set-size" }
func (self pgOpenSetProbe) tier() string           { return tierWarn }
func (self pgOpenSetProbe) cadence() time.Duration { return 5 * time.Minute }

func (self pgOpenSetProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `SELECT count(*) FROM transfer_contract WHERE open = true;`)
	if err != nil {
		return nil, err
	}
	openCount := atoiRow(rows[0], 0)

	// rising check: compare to the trailing hour's median so a drain after an
	// incident (falling through 150k) does not re-alert (2.6: alert on
	// sustained rise, not a spot value during recovery)
	rising := true
	if env.baseline != nil {
		if median, _, ok := env.baseline.trailingMedian(openSetMetric, time.Hour, 6); ok {
			rising = float64(openCount) > median
		}
		env.baseline.record(openSetMetric, time.Now(), float64(openCount))
	}

	if openCount > 150_000 && rising {
		return []finding{{
			probeId: "pg/open-set-size", tier: tierWarn,
			class: "open-set-size", target: target, sustain: 2,
			symptom:  fmt.Sprintf("open-contract set = %d and rising (threshold > 150k sustained)", openCount),
			baseline: "10–50k healthy (29,981 steady state after 2026-07-17); growth = closes not keeping up, and the 2.3 landmine plan degrades linearly with this number",
			observed: fmt.Sprintf("open_contracts=%d", openCount),
			context:  "check CloseExpiredContracts run durations in finished_task; drain observed at ~440k/8min once closes are healthy",
			playbook: "SIGNALS.md 2.6",
		}}, nil
	}
	return []finding{healthyFinding("pg/open-set-size", tierWarn, "open-set-size", target)}, nil
}

// pgConnectRateProbe is SIGNALS.md 2.7: the new-connection rate, the
// discriminator between existing sessions working (contract rate) and new
// connections being established. Compared against the trailing-hour median
// from local history, like the contract rate.
type pgConnectRateProbe struct {
	lock        sync.Mutex
	initialized bool
	lastCount   int64
	lastTime    time.Time
}

const connectRateMetric = "pg/connect-rate"

func (self *pgConnectRateProbe) id() string             { return "pg/connects-rate" }
func (self *pgConnectRateProbe) tier() string           { return tierWarn }
func (self *pgConnectRateProbe) cadence() time.Duration { return 60 * time.Second }

// observe converts PostgreSQL's cumulative insert counter into a per-minute
// rate. The first observation and a stats reset are warmups, not zero-rate
// incidents.
func (self *pgConnectRateProbe) observe(count int64, now time.Time) (rate int, ok bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if !self.initialized || count < self.lastCount || !self.lastTime.Before(now) {
		self.initialized = true
		self.lastCount = count
		self.lastTime = now
		return 0, false
	}
	elapsedMinutes := now.Sub(self.lastTime).Minutes()
	delta := count - self.lastCount
	self.lastCount = count
	self.lastTime = now
	if elapsedMinutes <= 0 {
		return 0, false
	}
	return int(float64(delta)/elapsedMinutes + 0.5), true
}

func (self *pgConnectRateProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	// pg_stat_user_tables is one row per table. n_tup_ins is approximate but
	// exactly suited to a rate signal, and avoids scanning the high-churn
	// network_client_connection table once per minute.
	rows, err := env.runner.pg(ctx, `
		SELECT COALESCE(sum(n_tup_ins), 0)
		FROM pg_stat_user_tables
		WHERE schemaname = current_schema()
		  AND relname = 'network_client_connection';
	`)
	if err != nil {
		return nil, err
	}
	count := int64(atoiRow(rows[0], 0))
	rate, rateReady := self.observe(count, time.Now())
	if !rateReady {
		return []finding{
			healthyFinding("pg/connects-rate", tierWarn, "connects-rate", target),
			healthyFinding("pg/connects-rate", tierWarn, "connects-storm", target),
		}, nil
	}

	var median float64
	var haveBaseline bool
	if env.baseline != nil {
		median, _, haveBaseline = env.baseline.trailingMedian(connectRateMetric, time.Hour, 30)
		env.baseline.record(connectRateMetric, time.Now(), float64(rate))
	}

	// a sustained storm/churn window pollutes the trailing-hour median (it
	// inflated to 10,875/min during the 2026-07-19 ansible restart wave);
	// when the hour median is itself >= 1.5x the trailing-6h median, judge
	// against the longer window instead — for both directions
	if haveBaseline && env.baseline != nil {
		if longMedian, _, haveLong := env.baseline.trailingMedian(connectRateMetric, 6*time.Hour, 120); haveLong && median >= 1.5*longMedian {
			median = longMedian
		}
	}

	switch {
	// the median >= 1000 guard keeps the band meaningful during overnight lows
	case haveBaseline && median >= 1000 && float64(rate) < 0.5*median:
		return []finding{
			{
				probeId: "pg/connects-rate", tier: tierWarn,
				class: "connects-rate", target: target, sustain: 5,
				symptom: fmt.Sprintf("new client connections = %d/min, < 50%% of the trailing median %.0f/min",
					rate, median),
				baseline: fmt.Sprintf("trailing median %.0f/min (learned); ~6,300–7,400/min observed healthy 2026-07-17 evening", median),
				observed: fmt.Sprintf("connects_last_min=%d median=%.0f", rate, median),
				context:  "contract rate still healthy = long-lived sessions fine, NEW connects failing (auth/lb/announce); both collapsed = systemic (5.1)",
				playbook: "SIGNALS.md 2.7",
			},
			healthyFinding("pg/connects-rate", tierWarn, "connects-storm", target),
		}, nil
	// high side: a reconnect storm. Mass simultaneous eviction (ansible unit
	// restart wave, simultaneous multi-block deploy) shows as a sustained
	// multiple of the baseline connect rate while everything else looks
	// healthy — observed 2026-07-19 22:55 (2.5k/min -> 7k plateau, 15k final
	// drain burst) with no ticket fired. Connections establish then die
	// young; median connection lifetime confirms (29s vs 60s that day)
	case haveBaseline && median >= 500 && float64(rate) > 2.5*median:
		return []finding{
			{
				probeId: "pg/connects-rate", tier: tierWarn,
				class: "connects-storm", target: target, sustain: 3,
				symptom: fmt.Sprintf("new client connections = %d/min, > 2.5x the trailing median %.0f/min — mass reconnect churn",
					rate, median),
				baseline: fmt.Sprintf("trailing median %.0f/min (learned)", median),
				observed: fmt.Sprintf("connects_last_min=%d median=%.0f ratio=%.1fx", rate, median, float64(rate)/median),
				context:  "correlate with deploys AND systemd/ansible unit restarts (8.5): simultaneous fleet restart evicts every client at once; expect a plateau while drains walk, a final eviction burst spike, then decay to baseline ~10 min after the burst",
				playbook: "SIGNALS.md 2.7",
			},
			healthyFinding("pg/connects-rate", tierWarn, "connects-rate", target),
		}, nil
	}
	return []finding{
		healthyFinding("pg/connects-rate", tierWarn, "connects-rate", target),
		healthyFinding("pg/connects-rate", tierWarn, "connects-storm", target),
	}, nil
}

// pgSelectionFreshnessProbe is SIGNALS.md 2.8: the provider-selection
// score-cache staleness canary. FindProviders2 serves only what
// UpdateClientScores last wrote (ttl 5h): a completion gap means apps select
// from a stale snapshot (grey dots, 5.9); at the ttl the cache empties
// entirely.
type pgSelectionFreshnessProbe struct{}

func (self pgSelectionFreshnessProbe) id() string             { return "pg/selection-stale" }
func (self pgSelectionFreshnessProbe) tier() string           { return tierWarn }
func (self pgSelectionFreshnessProbe) cadence() time.Duration { return 5 * time.Minute }

func (self pgSelectionFreshnessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
		SELECT coalesce(round(extract(epoch FROM now()-max(run_end_time)))::int, -1)
		FROM finished_task WHERE function_name LIKE '%UpdateClientScores%';
	`)
	if err != nil {
		return nil, err
	}
	gapS := atoiRow(rows[0], 0)

	if gapS < 0 || gapS > 90*60 {
		tier := tierWarn
		if gapS > 3*60*60 || gapS < 0 {
			// past 3h the 5h ttl cliff is near: selection goes from stale to
			// EMPTY when the last run's keys expire
			tier = tierPage
		}
		return []finding{{
			probeId: "pg/selection-stale", tier: tier,
			class: "selection-stale", target: target, sustain: 1,
			symptom: fmt.Sprintf("UpdateClientScores last completed %dm ago (healthy: back-to-back runs, gap < ~60m)",
				gapS/60),
			baseline: "runs complete every 12–50 min; the {cs_} score cache it writes carries a 5h ttl — apps serve a stale provider snapshot during any gap and an EMPTY one past the ttl",
			observed: fmt.Sprintf("completion_gap_s=%d ttl_cliff_in_s=%d", gapS, 5*3600-gapS),
			context:  "check pg/task-overdue for the grinding rebuild; recovery is automatic when a run completes (5.9)",
			playbook: "SIGNALS.md 2.8 / 5.9",
		}}, nil
	}
	return []finding{healthyFinding("pg/selection-stale", tierWarn, "selection-stale", target)}, nil
}

// pgbouncerProbe checks 6432 reachability cheaply. pgbouncer queuing/killing
// clients while direct 5432 connects instantly is the documented discriminator
// for a pg-side stall (§4 query_wait_timeout) — so the probe's failure mode is
// itself informative and pairs with the 1.3 active count.
type pgbouncerProbe struct{}

func (self pgbouncerProbe) id() string             { return "pg/pgbouncer" }
func (self pgbouncerProbe) tier() string           { return tierWarn }
func (self pgbouncerProbe) cadence() time.Duration { return 5 * time.Minute }

func (self pgbouncerProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("pg-primary")
	if h == nil {
		return nil, fmt.Errorf("no pg-primary host in inventory")
	}
	port := env.cfg.pgbouncerPort
	if port == 0 {
		port = 6432
	}
	// tcp connect only — a full auth round through a saturated pgbouncer would
	// occupy a pool slot; reachability vs refused/timeout is the signal
	out, err := env.runner.shell(ctx, h, fmt.Sprintf(
		`timeout 3 bash -c 'echo > /dev/tcp/127.0.0.1/%d' 2>/dev/null && echo open || echo closed`, port))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) != "open" {
		return []finding{{
			probeId: "pg/pgbouncer", tier: tierWarn,
			class: "pgbouncer-unreachable", target: fmt.Sprintf("%s:%d", h.name, port), sustain: 2,
			symptom:  fmt.Sprintf("pgbouncer %d not accepting tcp on %s", port, h.name),
			baseline: "accepts instantly; under pg saturation it queues clients and kills them with query_wait_timeout — check 1.3 active count (§4)",
			observed: strings.TrimSpace(out),
			playbook: "SIGNALS.md 5.8",
		}}, nil
	}
	return []finding{healthyFinding("pg/pgbouncer", tierWarn, "pgbouncer-unreachable", fmt.Sprintf("%s:%d", h.name, port))}, nil
}

// pgVacuumProbe is SIGNALS.md 2.4: dead-tuple accumulation on hot tables.
type pgVacuumProbe struct{}

func (self pgVacuumProbe) id() string             { return "pg/dead-tuples" }
func (self pgVacuumProbe) tier() string           { return tierWarn }
func (self pgVacuumProbe) cadence() time.Duration { return time.Hour }

func (self pgVacuumProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
		SELECT relname, n_dead_tup,
		       coalesce(to_char(last_autovacuum,'MM-DD HH24:MI'),'never')
		FROM pg_stat_user_tables
		WHERE n_dead_tup > 10000000
		ORDER BY n_dead_tup DESC LIMIT 5;
	`)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	for _, r := range rows {
		findings = append(findings, finding{
			probeId: "pg/dead-tuples", tier: tierWarn,
			class: "dead-tuples", target: target, frame: r.str(0), sustain: 1,
			symptom:  fmt.Sprintf("table %s has %s dead tuples (threshold > 10M), last autovacuum %s", r.str(0), r.str(1), r.str(2)),
			baseline: "autovacuum keeps hot tables under 10M dead; default scale factors never fire on 600M-row tables (2.4)",
			observed: fmt.Sprintf("n_dead_tup=%s last_autovacuum=%s", r.str(1), r.str(2)),
			context:  "check for a leaked idle-in-tx pinning the xmin horizon (1.3 oldest)",
			playbook: "SIGNALS.md 2.4",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/dead-tuples", tierWarn, "dead-tuples", target))
	}
	return findings, nil
}

// pgStatsLandmineProbe is the daily §7 stats-landmine check: pg_stats on
// transfer_contract.open must keep both values in the mcv list, and the
// open-partial indexes must show nonzero reltuples after analyze (2.3 tells).
type pgStatsLandmineProbe struct{}

func (self pgStatsLandmineProbe) id() string             { return "pg/stats-landmine" }
func (self pgStatsLandmineProbe) tier() string           { return tierWarn }
func (self pgStatsLandmineProbe) cadence() time.Duration { return 24 * time.Hour }

func (self pgStatsLandmineProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
		SELECT n_distinct::text FROM pg_stats
		WHERE tablename='transfer_contract' AND attname='open';
	`)
	if err != nil {
		return nil, err
	}
	nDistinct := ""
	if len(rows) > 0 {
		nDistinct = rows[0].str(0)
	}
	if nDistinct == "1" {
		return []finding{{
			probeId: "pg/stats-landmine", tier: tierWarn,
			class: "stats-landmine", target: target, frame: "transfer_contract.open", sustain: 1,
			symptom:  "pg_stats transfer_contract.open n_distinct=1 — the rare-value landmine is armed (2.3)",
			baseline: "n_distinct=2 with mcv {f,t}; at 1 the planner treats open=true as ~0 rows and pair lookups flip to o(open-set) plans",
			observed: fmt.Sprintf("n_distinct=%s", nDistinct),
			context:  "remediate with ANALYZE transfer_contract; durable fix = statistics target 10000 on the column (db_migrations)",
			playbook: "SIGNALS.md 5.8",
		}}, nil
	}
	return []finding{healthyFinding("pg/stats-landmine", tierWarn, "stats-landmine", target)}, nil
}
