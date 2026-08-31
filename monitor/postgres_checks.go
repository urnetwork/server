// Shared PostgreSQL checks used by focused named signals: open-contract set (SIGNALS.md 2.6), pgbouncer
// reachability (§4 query_wait_timeout discriminator), vacuum health (2.4), and
// the daily stats-landmine check (2.3/§7).
package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// pgOpenSetProbe is SIGNALS.md 2.6: the open-contract set size, the
// close-backlog canary and the fuel of the 5.8 feedback loop.
type pgOpenSetProbe struct {
	lock        sync.Mutex
	initialized bool
	lastCount   int
}

func (self *pgOpenSetProbe) id() string             { return "pg/open-set-size" }
func (self *pgOpenSetProbe) tier() string           { return tierWarn }
func (self *pgOpenSetProbe) cadence() time.Duration { return 5 * time.Minute }

// observe returns the immediately preceding sample. A process-local adjacent
// sample is the only evidence needed for "rising"; defaulting that predicate
// true while a longer trailing baseline warmed up made every monitor restart
// mislabel a high-but-draining recovery set as growth.
func (self *pgOpenSetProbe) observe(openCount int) (previous int, ready bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if !self.initialized {
		self.initialized = true
		self.lastCount = openCount
		return 0, false
	}
	previous = self.lastCount
	self.lastCount = openCount
	return previous, true
}

func (self *pgOpenSetProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE create_time < now() - interval '5 minutes'),
		       count(*) FILTER (WHERE create_time < now() - interval '30 minutes')
		FROM transfer_contract
		WHERE open = true;
	`)
	if err != nil {
		return nil, err
	}
	openCount := atoiRow(rows[0], 0)
	olderFiveMinutes := atoiRow(rows[0], 1)
	olderThirtyMinutes := atoiRow(rows[0], 2)

	previous, trendReady := self.observe(openCount)
	rising := trendReady && previous < openCount
	if openCount > 150_000 && (!trendReady || rising) {
		symptom := fmt.Sprintf("open-contract set = %d above threshold; trend is warming up", openCount)
		observed := fmt.Sprintf("open_contracts=%d previous=unavailable older_5m=%d older_30m=%d", openCount, olderFiveMinutes, olderThirtyMinutes)
		if trendReady {
			symptom = fmt.Sprintf("open-contract set = %d and rising (previous %d)", openCount, previous)
			observed = fmt.Sprintf("open_contracts=%d previous_open_contracts=%d delta=%d older_5m=%d older_30m=%d", openCount, previous, openCount-previous, olderFiveMinutes, olderThirtyMinutes)
		}
		return []finding{{
			probeId: "pg/open-set-size", tier: tierWarn,
			class: "open-set-size", target: target, sustain: 3,
			symptom:   symptom,
			mechanism: "CloseExpiredContracts settles contracts older than five minutes in checkpointed cohorts of up to 25,000 in current source (older deployments used 100,000). A growing old cohort means settlement throughput is below creation; a mostly-young spike instead points to demand or reconnect churn.",
			baseline:  "10–50k healthy (29,981 steady state after 2026-07-17); growth = closes not keeping up, and the 2.3 landmine plan degrades linearly with this number",
			observed:  observed,
			evidence:  fmt.Sprintf("open age buckets: total=%d older_5m=%d older_30m=%d", openCount, olderFiveMinutes, olderThirtyMinutes),
			context:   "Compare CloseExpiredContracts live/completed duration with the retention-fanout signal and transfer_contract autovacuum phase. A full cohort with sub-second worker transactions can still be delayed by persisted write/vacuum debt after the retention query itself clears.",
			action:    "Fix or roll out the bounded retention path and let vacuum plus scheduled close cohorts converge; do not raise closer concurrency while PostgreSQL write/vacuum debt is present.",
			verify:    "The older-than-five-minute cohort falls on consecutive samples, close cohorts return to seconds, and the total open set drains toward 10–50k.",
			playbook:  "SIGNALS.md 2.6 and 2.10",
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
				context:  "correlate with deploys AND systemd/ansible unit restarts (8.5): simultaneous fleet restart evicts every client at once; confirm shortened lifetime with matched disconnect_time cohorts, because a recent connect_time cohort right-censors its long-lived survivors; expect a plateau while drains walk, a final eviction burst spike, then decay to baseline ~10 min after the burst",
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
		activeLog, activeLogSource, activeLogErr := readTaskLifecycleLog(
			ctx,
			env,
			"UpdateClientScores",
			5*time.Minute,
			1000,
		)
		active := parseTaskActiveRun(activeLog, "UpdateClientScores")
		tier := tierWarn
		if gapS > 3*60*60 || gapS < 0 {
			// past 3h the 5h ttl cliff is near: selection goes from stale to
			// EMPTY when the last run's keys expire
			tier = tierPage
		}
		mechanism := "No fresh UpdateClientScores heartbeat could be confirmed. The completion gap can therefore be a queued or unreclaimed attempt, a worker failure, or an active rebuild hidden by unavailable task logs; discriminate those states before scheduling duplicate work."
		observed := fmt.Sprintf("completion_gap_s=%d ttl_cliff_in_s=%d active_log_source=%s", gapS, 5*3600-gapS, activeLogSource)
		evidence := "finished_task supplies the last completed publication boundary."
		context := "Check the exact pending task claim and the reboot-collision signal. A later retry is recovery from an interrupted attempt, not proof that its in-process export progress survived."
		action := "Restore task-log visibility, then let the existing claimed attempt run or allow normal lease reclamation. Do not schedule a duplicate score export or restart a progressing taskworker while the cache still has TTL headroom."
		verify := "A fresh heartbeat or finished_task row identifies the exact lifecycle; the completion gap returns below 60 minutes and remains there for two consecutive runs before the five-hour cache TTL expires."
		if active.seconds > 0 {
			mechanism = "A fresh eval-active heartbeat proves the scheduler has a live UpdateClientScores attempt, so the stale completion boundary is an actively rebuilding full-fleet export rather than a parked lease. A reboot or worker exit discards that attempt's in-process scan and the same task id must restart from its durable scheduler boundary."
			observed += fmt.Sprintf(" active_duration_s=%d", active.seconds)
			if active.taskID != "" {
				observed += " active_task_id=" + active.taskID
			}
			if active.identity.host != "" {
				observed += fmt.Sprintf(
					" active_host=%s active_generation=%s active_container=%s",
					active.identity.host,
					active.identity.generation,
					active.identity.container,
				)
			}
			evidence += " The taskworker heartbeat is authoritative execution-time evidence; its host/generation/container identifies the live executor."
			if activeLogSource == "host-journal-fallback" {
				evidence += " The fleet log gateway was unavailable, so the heartbeat came from bounded host-local taskworker journals."
			}
			context = "If reboot-collision names this same task id, the active heartbeat proves lease reclamation while its reset duration proves lost in-process progress. The cache still serves its last snapshot until the five-hour TTL cliff; a live retry is not permission to erase the interrupted-attempt evidence."
			action = "Let the live attempt finish. Retain the streaming, bounded-batch score exporter on generations that already have it and roll it out only where version or code evidence says it is absent. If an uninterrupted bounded-export run still exceeds 60 minutes, profile and checkpoint the remaining full-map load and caller-location fan-out. Do not restart this worker, schedule a duplicate export, or raise its deadline merely to reset the freshness alert."
			verify = "This exact task id reaches a real finished_task result, the completion gap resets below 60 minutes, and the next two scheduled exports finish without a reboot collision, memory-skew alert, or five-hour cache expiry."
		} else if activeLogErr != nil {
			evidence += " Task lifecycle lookup failed: " + activeLogErr.Error()
		}
		return []finding{{
			probeId: "pg/selection-stale", tier: tier,
			class: "selection-stale", target: target, sustain: 1,
			symptom: fmt.Sprintf("UpdateClientScores last completed %dm ago (healthy: back-to-back runs, gap < ~60m)",
				gapS/60),
			baseline:  "runs complete every 12–50 min; the {cs_} score cache it writes carries a 5h ttl — apps serve a stale provider snapshot during any gap and an EMPTY one past the ttl",
			mechanism: mechanism,
			observed:  observed,
			evidence:  evidence,
			context:   context,
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md §2.8, §2.12, and §2.13",
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
	findings := []finding{}
	if strings.TrimSpace(out) != "open" {
		findings = append(findings, finding{
			probeId: "pg/pgbouncer", tier: tierWarn,
			class: "pgbouncer-unreachable", target: fmt.Sprintf("%s:%d", h.name, port), sustain: 2,
			symptom:  fmt.Sprintf("pgbouncer %d not accepting tcp on %s", port, h.name),
			baseline: "accepts instantly; under pg saturation it queues clients and kills them with query_wait_timeout — check 1.3 active count (§4)",
			observed: strings.TrimSpace(out),
			playbook: "SIGNALS.md 5.8",
		})
	} else {
		findings = append(findings, healthyFinding("pg/pgbouncer", tierWarn, "pgbouncer-unreachable", fmt.Sprintf("%s:%d", h.name, port)))
	}

	// The application write-timeout can coexist with an open listener and a
	// lightly loaded direct PostgreSQL server. Pull a bounded log window and
	// keep service as the stable identity; the ephemeral tuple stays evidence.
	for _, service := range []string{"api", "connect", "taskworker"} {
		logs, pullErr := env.runner.warpctl(ctx, "logs", env.cfg.env, service,
			"--query=pgproto3.writeError", "--since=2m", "--limit=1000")
		if pullErr != nil && strings.TrimSpace(logs) == "" {
			continue
		}
		count := 0
		sample := ""
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "pgproto3.writeError") && strings.Contains(line, ":6432") && strings.Contains(line, "i/o timeout") {
				count++
				if sample == "" {
					sample = truncateLine(line)
				}
			}
		}
		if count > 0 {
			findings = append(findings, finding{
				probeId: "pg/pgbouncer-write-stall", tier: tierWarn,
				class: "pgbouncer-write-stall", target: service, sustain: 2,
				symptom:   fmt.Sprintf("service %s logged %d PgBouncer :6432 client-write timeout(s) in 2m", service, count),
				mechanism: "The client could not write a request into nginx/PgBouncer before its socket deadline; the query may never have reached a PostgreSQL backend.",
				baseline:  "Zero pgproto3 write i/o timeouts to :6432.",
				observed:  fmt.Sprintf("service=%s count_2m=%d", service, count),
				evidence:  "sample: " + sample,
				action:    "Split the 6432 nginx frontend, all PgBouncer shard queues/listeners, and direct 5432; group timeouts by application route.",
				verify:    "No :6432 client-write timeout recurs while the affected route is exercised.",
				playbook:  "SIGNALS.md §2.11",
			})
		} else {
			findings = append(findings, healthyFinding("pg/pgbouncer-write-stall", tierWarn, "pgbouncer-write-stall", service))
		}
	}
	return findings, nil
}

// pgVacuumProbe is SIGNALS.md 2.4: dead-tuple accumulation on hot tables.
type pgVacuumProbe struct{}

const vacuumDeadTupleAlertFloor int64 = 10_000_000

const pgVacuumHealthSQL = `
	WITH oldest_horizon AS MATERIALIZED (
	 SELECT pid, state, backend_type,
	        greatest(coalesce(age(backend_xid),-1),
	                 coalesce(age(backend_xmin),-1)) AS horizon_age,
	        coalesce(backend_xid::text,'') AS backend_xid,
	        coalesce(backend_xmin::text,'') AS backend_xmin,
	        round(extract(epoch FROM clock_timestamp()-coalesce(xact_start,query_start)))::int AS age_s,
	        coalesce(application_name,'') AS application_name,
	        left(regexp_replace(query, E'[\\n\\r\\t ]+', ' ', 'g'), 160) AS query
	 FROM pg_stat_activity
	 WHERE (backend_xid IS NOT NULL OR backend_xmin IS NOT NULL)
	   AND backend_type != 'autovacuum worker'
	 -- Every fresh snapshot can inherit the same backend_xmin from the oldest
	 -- in-progress writer.  On an equal horizon, the oldest transaction/query
	 -- is the useful candidate rather than an arbitrary seconds-old request.
	 ORDER BY horizon_age DESC,
	          coalesce(xact_start,query_start) ASC NULLS LAST,
	          pid
	 LIMIT 1
	), dead AS (
	 SELECT s.relid, s.relname, s.n_dead_tup,
	        coalesce(to_char(s.last_autovacuum,'MM-DD HH24:MI'),'never') AS last_autovacuum,
	        coalesce((
	          SELECT option_value::bigint
	          FROM pg_options_to_table(c.reloptions)
	          WHERE option_name = 'autovacuum_vacuum_threshold'
	        ),0) AS configured_vacuum_threshold
	 FROM pg_stat_user_tables s
	 INNER JOIN pg_class c ON c.oid = s.relid
	 WHERE s.n_dead_tup > 10000000
	 ORDER BY s.n_dead_tup DESC LIMIT 5
	)
	SELECT dead.relname, dead.n_dead_tup, dead.last_autovacuum,
	       dead.configured_vacuum_threshold,
	       coalesce(v.phase,''),
	       coalesce(round(extract(epoch FROM clock_timestamp()-va.query_start)),0)::int,
	       coalesce(v.heap_blks_total,0), coalesce(v.heap_blks_scanned,0),
	       coalesce(v.heap_blks_vacuumed,0),
	       coalesce((to_jsonb(v)->>'index_vacuum_count')::bigint,0),
	       coalesce((to_jsonb(v)->>'indexes_processed')::bigint,0),
	       coalesce((to_jsonb(v)->>'indexes_total')::bigint,0),
	       coalesce(h.pid,0), coalesce(h.horizon_age,0),
	       coalesce(h.backend_xid,''), coalesce(h.backend_xmin,''),
	       coalesce(h.age_s,0), coalesce(h.state,''),
	       coalesce(h.backend_type,''), coalesce(h.application_name,''),
	       coalesce(h.query,'')
	FROM dead
	LEFT JOIN pg_stat_progress_vacuum v ON v.relid = dead.relid
	LEFT JOIN pg_stat_activity va ON va.pid = v.pid
	LEFT JOIN oldest_horizon h ON true;
`

func vacuumDeadTupleAlertThreshold(configuredThreshold int64) int64 {
	if configuredThreshold > vacuumDeadTupleAlertFloor {
		return configuredThreshold
	}
	return vacuumDeadTupleAlertFloor
}

func vacuumDeadTupleThresholdLabel(threshold int64) string {
	if threshold%1_000_000 == 0 {
		return fmt.Sprintf("%dM", threshold/1_000_000)
	}
	return fmt.Sprintf("%d", threshold)
}

func isPaymentPlannerVacuumHorizon(query string) bool {
	lowerQuery := strings.ToLower(query)
	if strings.Contains(lowerQuery, "temp_account_payment") {
		return true
	}
	if strings.Contains(lowerQuery, "from transfer_escrow_sweep") &&
		strings.Contains(lowerQuery, "subsidy_start_time") &&
		strings.Contains(lowerQuery, "subsidy_end_time") {
		return true
	}
	return strings.Contains(lowerQuery, "from subsidy_payment") &&
		strings.Contains(lowerQuery, "start_time <") &&
		strings.Contains(lowerQuery, "< end_time")
}

func (self pgVacuumProbe) id() string             { return "pg/dead-tuples" }
func (self pgVacuumProbe) tier() string           { return tierWarn }
func (self pgVacuumProbe) cadence() time.Duration { return 5 * time.Minute }

func (self pgVacuumProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, pgVacuumHealthSQL)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	for _, r := range rows {
		configuredThreshold := int64(atoiRow(r, 3))
		alertThreshold := vacuumDeadTupleAlertThreshold(configuredThreshold)
		if int64(atoiRow(r, 1)) <= alertThreshold {
			// Some pure cascade victims deliberately tolerate more dead space
			// than query-critical tables. An explicit larger fixed threshold is
			// policy, not an overdue vacuum.
			continue
		}
		recoveryTarget := fmt.Sprintf(
			"%s returns below %s",
			r.str(0),
			vacuumDeadTupleThresholdLabel(alertThreshold),
		)
		vacuum := "no vacuum currently reported"
		if r.str(4) != "" {
			vacuum = fmt.Sprintf(
				"vacuum phase=%s age_s=%s heap_scanned=%s/%s heap_vacuumed=%s index_vacuum_count=%s indexes_processed=%s/%s",
				r.str(4), r.str(5), r.str(7), r.str(6), r.str(8), r.str(9), r.str(10), r.str(11),
			)
		}
		horizon := "no backend_xid/backend_xmin horizon candidate reported"
		if r.str(12) != "" && r.str(12) != "0" {
			horizon = fmt.Sprintf(
				"oldest MVCC horizon candidate: pid=%s horizon_age_xids=%s backend_xid=%s backend_xmin=%s xact_age_s=%s state=%s backend=%s application=%s query=%s",
				r.str(12), r.str(13), r.str(14), r.str(15), r.str(16), r.str(17), r.str(18), r.str(19), r.str(20),
			)
		}
		mechanism := "Dead-row production is outrunning cleanup, or an old backend_xid/backend_xmin horizon is preventing cleanup. A vacuum that has scanned the full heap but remains in index vacuum while dead tuples keep rising points to continuing writer churn, not a stalled heap scan."
		context := "An old backend_xid or backend_xmin can pin cleanup, including active writers and pg_dump/COPY snapshots. Fresh snapshots inherit the oldest in-progress xid, so equal horizons are ranked by oldest transaction/query start; confirm the candidate's owner before acting. If no old horizon exists, correlate high-row UPDATE writers such as payment retention before tuning or interrupting autovacuum."
		action := "Remove or bound the identified write fan-out first. Only address a horizon holder after confirming its owner and safety; do not cancel a progressing autovacuum merely because its index phase is long."
		verify := "The high-row writer is bounded, the active vacuum completes, and " + recoveryTarget + " on consecutive five-minute samples."
		if strings.Contains(strings.ToLower(r.str(20)), "client_reliability_running") {
			mechanism += " The selected horizon is UpdateReliabilities performing a full running-window re-anchor; the pre-fix threshold was shorter than the task cadence, so this multi-billion-row transaction recurred every cycle and restricted each vacuum to rows removable before that old horizon. It can reduce, rather than completely prevent, reclamation."
			context += " The task-overdue signal is the authoritative task diagnosis; this vacuum signal describes its MVCC consequence."
			action = "Allow a progressing bounded re-anchor to finish, and roll out the four-hour reliability re-anchor cadence while retaining the 30-minute incremental cadence. Do not cancel the transaction, raise its deadline, or retune autovacuum to hide the shared root cause."
			verify = "Most reliability cycles take the incremental path below their historical p95; " + recoveryTarget + " on consecutive five-minute samples, and after the bounded re-anchor completes the waiting maintenance proceeds and later vacuums can see the full removable cohort."
		} else if lowerQuery := strings.ToLower(r.str(20)); strings.Contains(lowerQuery, "update transfer_contract") &&
			strings.Contains(lowerQuery, "reap_time") {
			mechanism += " The selected horizon is the legacy CompletePayment retention fan-out: one indexed payment lookup is updating millions of transfer_contract reap_time values inside one transaction, generating the dead rows and holding their old snapshot while autovacuum works."
			context += " The retention-fanout signal is the authoritative statement diagnosis; this vacuum signal describes the same writer's MVCC and cleanup consequence."
			action = "Roll out the contract_retention_pending queue and bounded, committed contract_retention_cursor batches. Do not cancel the progressing vacuum, kill an in-flight payment without proving retry safety, or tune autovacuum around the unbounded writer."
			verify = "The legacy retention query disappears, cursor batches commit with bounded row counts, the active vacuum completes, and " + recoveryTarget + " on consecutive five-minute samples."
		} else if lowerQuery := strings.ToLower(r.str(20)); strings.Contains(lowerQuery, "update transfer_contract") &&
			strings.Contains(lowerQuery, "set outcome") && strings.Contains(lowerQuery, "close_time") {
			mechanism += " The selected horizon is one bounded per-contract CloseExpiredContracts transaction. During backlog recovery, many short committed closes legitimately create a new dead-row cohort; a seconds-old closer is workload evidence, not an old MVCC pin or the legacy multi-million-row retention fan-out."
			context += " The open-contract signal is authoritative for whether the closer is draining. Compare its five- and 30-minute age buckets and the active close-task duration before attributing this dead-row wave to a stuck writer."
			action = "Let the bounded close cohort and progressing autovacuum run, and roll out the 25,000-contract task checkpoint. Do not cancel the closer, raise its concurrency, or revive a draining backlog merely to reduce the current dead-tuple estimate."
			verify = "Older open-contract buckets fall, each close cohort checkpoints before its deadline, autovacuum completes, and " + recoveryTarget + " on consecutive five-minute samples after the backlog drains."
		} else if isPaymentPlannerVacuumHorizon(r.str(20)) {
			mechanism += " The selected horizon is the Payout payment planner building its bounded temp_account_payment working set. This statement reads transfer_contract and can retain an MVCC snapshot while the plan runs, but it is not the unbounded transfer_contract retention writer that produced the dead-row wave."
			context += " The task-canary signal is authoritative for the Payout row. In the affected deployment, the outer plan transaction can later sit idle while a deliberately separate reliability-maintenance transaction runs; PostgreSQL's global five-minute idle-in-transaction timeout then closes that outer connection."
			action = "Let the bounded payout attempt reach its task outcome, and roll out the payment-plan SET LOCAL idle_in_transaction_session_timeout override together with bounded plan slices. Do not cancel this bounded reader solely from its sampled age, disable the database-wide timeout, or blame it for the retention write fan-out."
			verify = "A Payout slice commits and clears its task error, an unrelated PostgreSQL session retains the global five-minute idle-in-transaction timeout, autovacuum completes, and " + recoveryTarget + " on consecutive five-minute samples."
		} else if lowerQuery := strings.TrimSpace(strings.ToLower(r.str(20))); atoiRow(r, 16) < 60 && strings.HasPrefix(lowerQuery, "select ") {
			mechanism += " The selected candidate is a fresh read-only snapshot, not a persistent horizon holder. Its large backend_xmin age is inherited from cluster transaction state; a seconds-old SELECT does not become the owner merely because it was sampled after the real old transaction released."
			context += " Treat this row as negative attribution evidence. Continuing dead-row growth with per-index progress points to writer churn and cleanup debt; use the retention, close-backlog, and active-query signals to name those writers."
			action = "Do not cancel or tune around the fresh SELECT. Let the progressing vacuum continue and address only a separately proven high-row writer or genuinely old transaction."
			verify = "The sampled read disappears normally, index progress continues, known high-row writers are bounded, and " + recoveryTarget + " on consecutive five-minute samples."
		}
		findings = append(findings, finding{
			probeId: "pg/dead-tuples", tier: tierWarn,
			class: "dead-tuples", target: target, frame: r.str(0), sustain: 1,
			symptom:   fmt.Sprintf("table %s has %s dead tuples (alert threshold > %d), last autovacuum %s", r.str(0), r.str(1), alertThreshold, r.str(2)),
			mechanism: mechanism,
			baseline:  "alert at 10M dead unless the table has an explicit larger fixed autovacuum threshold; default scale factors never fire promptly on giant tables (2.4)",
			observed:  fmt.Sprintf("n_dead_tup=%s alert_threshold=%d configured_vacuum_threshold=%d last_autovacuum=%s vacuum_age_s=%s", r.str(1), alertThreshold, configuredThreshold, r.str(2), r.str(5)),
			evidence:  vacuum + "\n" + horizon,
			context:   context,
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md 2.4",
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
		SELECT
		 coalesce((SELECT n_distinct::text FROM pg_stats
		           WHERE schemaname=current_schema() AND tablename='transfer_contract' AND attname='open'),'missing'),
		 (SELECT count(*) FROM pg_class i JOIN pg_index x ON x.indexrelid=i.oid
		  JOIN pg_class t ON t.oid=x.indrelid JOIN pg_namespace n ON n.oid=t.relnamespace
		  WHERE n.nspname=current_schema() AND t.relname='transfer_contract'
		    AND x.indpred IS NOT NULL AND pg_get_expr(x.indpred,x.indrelid) ILIKE '%open%'
		    AND i.reltuples=0);
	`)
	if err != nil {
		return nil, err
	}
	nDistinct := ""
	zeroPartialIndexes := 0
	if len(rows) > 0 {
		nDistinct = rows[0].str(0)
		zeroPartialIndexes = atoiRow(rows[0], 1)
	}
	if nDistinct == "1" || zeroPartialIndexes > 0 {
		return []finding{{
			probeId: "pg/stats-landmine", tier: tierWarn,
			class: "stats-landmine", target: target, frame: "transfer_contract.open", sustain: 1,
			symptom:  fmt.Sprintf("transfer_contract.open planner landmine is armed: n_distinct=%s zero-row open partial indexes=%d", nDistinct, zeroPartialIndexes),
			baseline: "n_distinct=2 with mcv {f,t}; at 1 the planner treats open=true as ~0 rows and pair lookups flip to o(open-set) plans",
			observed: fmt.Sprintf("n_distinct=%s zero_open_partial_indexes=%d", nDistinct, zeroPartialIndexes),
			context:  "remediate with ANALYZE transfer_contract; durable fix = statistics target 10000 on the column (db_migrations)",
			playbook: "SIGNALS.md 5.8",
		}}, nil
	}
	return []finding{healthyFinding("pg/stats-landmine", tierWarn, "stats-landmine", target)}, nil
}
