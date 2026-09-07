package monitor

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §5.11 maps to signal_netescrow.go and signal_netescrow_test.go.
// This root-cause probe watches authoritative task durations and aggregate
// drift corrections; log-errors independently reports negative aftermath.
func NewNetEscrowSignal() Signal {
	return &signalAdapter{
		number: "5.11",
		key:    "netescrow",
		name:   "Net-escrow reconciliation freshness",
		probe:  &netEscrowProbe{},
	}
}

type netEscrowStatementCounters struct {
	reservationCalls          int64
	reservationTotalMs        float64
	reservationMaxMs          float64
	legacyReservationCalls    int64
	boundedReservationCalls   int64
	unsettledReservationCalls int64
	balanceCalls              int64
	balanceTotalMs            float64
	balanceMaxMs              float64
}

type netEscrowStatementProfile struct {
	netEscrowStatementCounters
	reservationDeltaCalls          int64
	reservationDeltaMeanMs         float64
	legacyReservationDeltaCalls    int64
	boundedReservationDeltaCalls   int64
	unsettledReservationDeltaCalls int64
	balanceDeltaCalls              int64
	balanceDeltaMeanMs             float64
}

type netEscrowProbe struct {
	profileMu          sync.Mutex
	profileInitialized bool
	lastProfile        netEscrowStatementCounters
}

var netEscrowAggregateRe = regexp.MustCompile(
	`(?i)\[sm\]reconcile net escrow: ([0-9]+) balances, ([0-9]+) networks drifted, over-reserved ([0-9]+(?:\.[0-9]+)?)([kmgt]?i?b), under-reserved ([0-9]+(?:\.[0-9]+)?)([kmgt]?i?b)`,
)

const (
	netEscrowReconcileRunLimit = 2 * time.Minute
	netEscrowActiveLogLookback = 2 * time.Minute
	netEscrowIncidentLookback  = 45 * time.Minute
	netEscrowDriftLogLookback  = 15 * time.Minute
	netEscrowLargeDriftBytes   = int64(256) << 30
)

type netEscrowAggregate struct {
	balances   int
	networks   int
	overText   string
	underText  string
	overBytes  int64
	underBytes int64
	identity   warpLogIdentity
}

func (*netEscrowProbe) id() string             { return "pg/netescrow-reconcile-overrun" }
func (*netEscrowProbe) tier() string           { return tierWarn }
func (*netEscrowProbe) cadence() time.Duration { return time.Minute }

func netEscrowProfileInt64(row pgRow, column int) int64 {
	value, _ := strconv.ParseInt(row.str(column), 10, 64)
	return value
}

func (self *netEscrowProbe) observeStatementProfile(current netEscrowStatementCounters) netEscrowStatementProfile {
	self.profileMu.Lock()
	defer self.profileMu.Unlock()

	profile := netEscrowStatementProfile{netEscrowStatementCounters: current}
	if self.profileInitialized &&
		self.lastProfile.reservationCalls <= current.reservationCalls &&
		self.lastProfile.reservationTotalMs <= current.reservationTotalMs {
		profile.reservationDeltaCalls = current.reservationCalls - self.lastProfile.reservationCalls
		if 0 < profile.reservationDeltaCalls {
			profile.reservationDeltaMeanMs = (current.reservationTotalMs - self.lastProfile.reservationTotalMs) /
				float64(profile.reservationDeltaCalls)
		}
	}
	if self.profileInitialized &&
		self.lastProfile.balanceCalls <= current.balanceCalls &&
		self.lastProfile.balanceTotalMs <= current.balanceTotalMs {
		profile.balanceDeltaCalls = current.balanceCalls - self.lastProfile.balanceCalls
		if 0 < profile.balanceDeltaCalls {
			profile.balanceDeltaMeanMs = (current.balanceTotalMs - self.lastProfile.balanceTotalMs) /
				float64(profile.balanceDeltaCalls)
		}
	}
	if self.profileInitialized && self.lastProfile.legacyReservationCalls <= current.legacyReservationCalls {
		profile.legacyReservationDeltaCalls = current.legacyReservationCalls - self.lastProfile.legacyReservationCalls
	}
	if self.profileInitialized && self.lastProfile.boundedReservationCalls <= current.boundedReservationCalls {
		profile.boundedReservationDeltaCalls = current.boundedReservationCalls - self.lastProfile.boundedReservationCalls
	}
	if self.profileInitialized && self.lastProfile.unsettledReservationCalls <= current.unsettledReservationCalls {
		profile.unsettledReservationDeltaCalls = current.unsettledReservationCalls - self.lastProfile.unsettledReservationCalls
	}
	self.lastProfile = current
	self.profileInitialized = true
	return profile
}

func (self *netEscrowProbe) statementProfile(ctx context.Context, env *probeEnv) (netEscrowStatementProfile, error) {
	rows, err := env.runner.pg(ctx, `
		WITH statements AS (
			SELECT calls, total_exec_time, max_exec_time,
			       query ILIKE '%transfer_escrow.balance_id = ANY%'
			         AND query ILIKE '%transfer_contract.outcome IS NULL%' AS legacy_reservation_page,
			       query ILIKE '%FROM unnest(%'
			         AND query ILIKE '%CROSS JOIN LATERAL%'
			         AND query ILIKE '%requested_balance.balance_id%'
			         AND query ILIKE '%transfer_contract.outcome IS NULL%' AS bounded_reservation_page,
			       query ILIKE '%FROM unnest(%'
			         AND query ILIKE '%CROSS JOIN LATERAL%'
			         AND query ILIKE '%requested_balance.balance_id%'
			         AND query ILIKE '%transfer_escrow.settled%'
			         AND query ILIKE '%transfer_contract.outcome IS NULL%' AS unsettled_reservation_page,
			       query ILIKE '%FROM transfer_balance%'
			         AND query ILIKE '%balance_id >%'
			         AND query ILIKE '%ORDER BY balance_id%'
			         AND query ILIKE '%LIMIT%' AS balance_page
			FROM pg_stat_statements
			WHERE query NOT ILIKE '%FROM pg_stat_statements%'
			  AND query NOT ILIKE 'EXPLAIN%'
		)
		SELECT coalesce(sum(calls) FILTER (WHERE legacy_reservation_page OR bounded_reservation_page),0),
		       coalesce(sum(total_exec_time) FILTER (WHERE legacy_reservation_page OR bounded_reservation_page),0),
		       coalesce(max(max_exec_time) FILTER (WHERE legacy_reservation_page OR bounded_reservation_page),0),
		       coalesce(sum(calls) FILTER (WHERE balance_page),0),
		       coalesce(sum(total_exec_time) FILTER (WHERE balance_page),0),
		       coalesce(max(max_exec_time) FILTER (WHERE balance_page),0),
		       coalesce(sum(calls) FILTER (WHERE legacy_reservation_page),0),
		       coalesce(sum(calls) FILTER (WHERE bounded_reservation_page),0),
		       coalesce(sum(calls) FILTER (WHERE unsettled_reservation_page),0)
		FROM statements;
	`)
	if err != nil {
		return netEscrowStatementProfile{}, err
	}
	if len(rows) != 1 {
		return netEscrowStatementProfile{}, fmt.Errorf("net-escrow statement profile returned %d rows, want 1", len(rows))
	}
	return self.observeStatementProfile(netEscrowStatementCounters{
		reservationCalls:          netEscrowProfileInt64(rows[0], 0),
		reservationTotalMs:        atof(rows[0].str(1)),
		reservationMaxMs:          atof(rows[0].str(2)),
		balanceCalls:              netEscrowProfileInt64(rows[0], 3),
		balanceTotalMs:            atof(rows[0].str(4)),
		balanceMaxMs:              atof(rows[0].str(5)),
		legacyReservationCalls:    netEscrowProfileInt64(rows[0], 6),
		boundedReservationCalls:   netEscrowProfileInt64(rows[0], 7),
		unsettledReservationCalls: netEscrowProfileInt64(rows[0], 8),
	}), nil
}

func (self *netEscrowProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := pgTarget(env)
	rows, err := env.runner.pg(ctx, `
		SELECT 'completed'::text,
		       round(extract(epoch FROM run_end_time-run_start_time))::int AS duration_s,
		       round(extract(epoch FROM now()-run_end_time))::int AS age_s
		FROM finished_task
		WHERE split_part(function_name,'.',3) = 'ReconcileNetEscrow'
		  AND run_end_time > now() - interval '45 minutes'
		  AND run_end_time-run_start_time >= interval '120 seconds'
		ORDER BY run_end_time DESC
		LIMIT 1;
	`)
	if err != nil {
		return nil, err
	}

	// pending_task has no execution-start column: run_at is the due time and
	// claim_time is a moving heartbeat. Taskworker's bounded `eval active`
	// heartbeat is the authoritative live elapsed time.
	activeLog, activeLogErr := env.runner.warpctl(
		ctx,
		"logs", env.cfg.env, "taskworker",
		fmt.Sprintf("--since=%dm", int(netEscrowActiveLogLookback/time.Minute)),
		"--limit=1000", "--query=ReconcileNetEscrow", "--utc",
	)
	activeRun := parseTaskActiveRun(activeLog, "ReconcileNetEscrow")
	activeSeconds := activeRun.seconds
	aggregateLog, aggregateLogErr := env.runner.warpctl(
		ctx,
		"logs", env.cfg.env, "taskworker",
		fmt.Sprintf("--since=%dm", int(netEscrowDriftLogLookback/time.Minute)),
		"--limit=100", "--query=[sm]reconcile net escrow", "--utc",
	)
	aggregates := netEscrowAggregates(aggregateLog)

	phase := ""
	durationSeconds := 0
	ageSeconds := 0
	completedDurationSeconds := 0
	completedAgeSeconds := 0
	if len(rows) > 0 {
		completedDurationSeconds = atoi(rows[0].str(1))
		completedAgeSeconds = atoi(rows[0].str(2))
	}
	// The taskworker log query deliberately overlaps two minutes. Immediately
	// after a run completes it can therefore still return that run's final
	// eval-active heartbeat. A newer finished_task row whose duration reaches
	// or exceeds the heartbeat is authoritative lifecycle evidence for the same
	// run. Older completed precursors do not mask a genuinely live successor.
	completionSupersedesHeartbeat := len(rows) > 0 &&
		0 <= completedAgeSeconds &&
		completedAgeSeconds <= int(netEscrowActiveLogLookback/time.Second) &&
		activeSeconds <= completedDurationSeconds
	if activeSeconds >= int(netEscrowReconcileRunLimit/time.Second) && !completionSupersedesHeartbeat {
		phase = "active"
		durationSeconds = activeSeconds
	} else if len(rows) > 0 {
		phase = rows[0].str(0)
		durationSeconds = completedDurationSeconds
		ageSeconds = completedAgeSeconds
	} else if activeLogErr != nil {
		return nil, fmt.Errorf("read active ReconcileNetEscrow task logs: %w", activeLogErr)
	}

	findings := []finding{}
	if aggregate, previous, direction, windowBoundary, ok := latestLargeNetEscrowDrift(aggregates); ok {
		observed := fmt.Sprintf(
			"balances=%d networks_drifted=%d over_reserved=%s under_reserved=%s threshold_bytes=%d lookback_s=%d",
			aggregate.balances,
			aggregate.networks,
			aggregate.overText,
			aggregate.underText,
			netEscrowLargeDriftBytes,
			int(netEscrowDriftLogLookback/time.Second),
		)
		if aggregate.identity.host != "" {
			observed += fmt.Sprintf(
				" source_host=%s source_generation=%s source_container=%s",
				aggregate.identity.host,
				aggregate.identity.generation,
				aggregate.identity.container,
			)
		}
		mechanism := "A correction at least 256GiB is far outside the healthy tens-of-GiB band. It can be a lost large mirror write. If the adjacent pass moves nearly the same quantity in the opposite direction, reconciliation wrote a stale reservation view; exact executor and statement-shape evidence must distinguish the legacy fleet-wide absolute snapshot from the current page-local additive race."
		evidence := "Follow the adjacent scheduled aggregate, exact source executor, reservation-page statement profile, and all three negative-counter emitters. A matched opposite-direction correction identifies a reconciliation-created stale write but does not by itself identify which reconciler version caused it; a one-direction event instead requires tracing the affected durable reservation before attribution."
		if previous != nil && direction != "" {
			observed += fmt.Sprintf(
				" previous_over_reserved=%s previous_under_reserved=%s matched_reversal=true reversal_direction=%s",
				previous.overText,
				previous.underText,
				direction,
			)
			if previous.identity.host != "" {
				observed += fmt.Sprintf(
					" previous_source_host=%s previous_source_generation=%s previous_source_container=%s",
					previous.identity.host,
					previous.identity.generation,
					previous.identity.container,
				)
			}
			mechanism = "Adjacent scheduled passes moved nearly the same >=256GiB quantity in opposite directions. Independent lost writes do not repair as a matched inverse: one reconciliation wrote a stale reservation view and the next pass restored it. A legacy executor can do this with its fleet-wide absolute snapshot. On the current additive executor, each PostgreSQL statement fixes its page snapshot before running; live creates or settlements can update PostgreSQL and Redis while a slow reservation query is still executing, then the later Redis GET sees the newer mirror and the additive correction moves it back toward the older PostgreSQL snapshot. This can happen within the nominal 120s duration band, although multi-second pages make the window much larger."
			evidence = "The adjacent aggregate pair identifies a reconciliation-created stale write. Exact source executor version plus legacy-ANY, historical bounded-lateral, or unsettled-partial statement timing identifies which race was possible. Negative counters can remain zero when no settlement decrements the overwritten mirror before the corrective successor runs."
		} else if windowBoundary {
			observed += " matched_reversal=unknown_window_boundary"
			mechanism = "A correction at least 256GiB remains outside the healthy tens-of-GiB band, but it is now the oldest aggregate retained in the observation window. Its preceding scheduled aggregate is no longer observable, so later healthy passes cannot safely reclassify this event as a one-direction correction."
			evidence = "Do not replace an earlier matched-reversal attribution with an unmatched claim after its precursor ages out. Preserve the integrity incident until this large correction itself leaves the window; use retained alert history for the original adjacent pair."
		} else {
			observed += " matched_reversal=false"
		}
		findings = append(findings, finding{
			probeId: "logs/netescrow-large-drift", tier: tierWarn,
			class: "netescrow-large-drift", target: "taskworker", frame: "ReconcileNetEscrow", sustain: 1,
			symptom:   fmt.Sprintf("ReconcileNetEscrow corrected %s over-reserved and %s under-reserved in a recent pass", aggregate.overText, aggregate.underText),
			mechanism: mechanism,
			baseline:  "Scheduled aggregates normally remain in the tens-of-GiB band; alert when either direction reaches 256GiB, independent of task duration.",
			observed:  observed,
			evidence:  evidence,
			context:   "This is reservation-mirror integrity, not Redis capacity. A short task and zero negative-counter lines do not clear a large aggregate correction; the adjacent pass determines whether it is a matched reversal. Source host/generation/container identify the exact executor, so a fast pass elsewhere cannot erase this pass's evidence. Additive INCRBY preserves mirror writes after Redis GET; it cannot preserve a write that became visible before GET but was absent from the earlier PostgreSQL statement snapshot.",
			action:    "Do not manually re-run reconciliation. Confirm the exact source executor, reservation statement shape, page timing, and atomic release clamp. For a legacy absolute executor, deploy the page-local additive path. For a current additive executor with slow legacy-ANY or historical bounded-lateral pages, apply migration 601 and deploy the unsettled-partial query to shrink the PostgreSQL-snapshot-to-Redis-GET window. If matched reversals remain on fast unsettled-partial pages, treat the remaining cross-store window as a sequencing defect that requires durable per-balance fencing/versioning, not another blind rerun.",
			verify:    "Every active taskworker generation uses unsettled-partial reservation pages below 1s, scheduled reconciliations remain below 120s, recurring aggregates stay below 256GiB in the tens-of-GiB band, already-correct mirrors receive no rewrite, and all negative-counter emitters remain at zero for a full interval.",
			playbook:  "SIGNALS.md §5.11",
		})
	}

	if phase == "" {
		if len(findings) == 0 {
			if aggregateLogErr != nil {
				return nil, fmt.Errorf("read ReconcileNetEscrow aggregate logs: %w", aggregateLogErr)
			}
			return []finding{healthyFinding(
				"pg/netescrow-reconcile-overrun",
				tierWarn,
				"netescrow-reconcile-overrun",
				target,
			)}, nil
		}
		return findings, nil
	}
	phaseWithArticle := "a " + phase
	incidentContext := "A proven completed overrun remains visible for 45 minutes so a quick follow-up run cannot erase the incident precursor."
	evidence := "Correlate this run with the matching `[sm]reconcile net escrow` aggregate and any `[netescrow]negative counter` burst; large opposite-direction drift in the next run is the repair signature."
	observed := fmt.Sprintf("phase=%s duration_s=%d lookback_s=%d", phase, durationSeconds, int(netEscrowIncidentLookback/time.Second))
	if phase == "active" {
		phaseWithArticle = "an active"
		incidentContext = "Taskworker's eval-active heartbeat supplies this live elapsed time; it is not inferred from a scheduling timestamp."
		if len(rows) > 0 {
			observed += fmt.Sprintf(" precursor_completed_duration_s=%d precursor_completed_age_s=%d", completedDurationSeconds, completedAgeSeconds)
			incidentContext += fmt.Sprintf(" The active successor follows a completed %ds overrun that ended %ds ago; retain both lifecycle points until the chain converges.", completedDurationSeconds, completedAgeSeconds)
			evidence += " Compare both consecutive reconcile aggregates; an active successor does not erase its completed overrun precursor."
		}
		if activeRun.taskID != "" {
			observed += " active_attempt_correlated=true"
		}
		if activeRun.identity.host != "" {
			observed += fmt.Sprintf(
				" active_host=%s active_generation=%s active_container=%s",
				activeRun.identity.host,
				activeRun.identity.generation,
				activeRun.identity.container,
			)
			incidentContext += fmt.Sprintf(
				" The live heartbeat is from %s/%s container %s; retain its lifecycle timestamps and executor identity when the fleet alternates fast and long runs, because one executor's fast pass does not prove the deployed algorithm is fixed.",
				activeRun.identity.host,
				activeRun.identity.generation,
				activeRun.identity.container,
			)
		}
	} else {
		observed += fmt.Sprintf(" completed_age_s=%d", ageSeconds)
	}

	mechanism := "Reconciliation has exceeded its freshness band. On the current page-local additive path, migration catch-up, index warmup, storage contention, or a large dirty page set can lengthen the bounded walk. On an older absolute-snapshot path, the same duration also expands stale-snapshot exposure and rewrites every mirror. Duration alone cannot identify which algorithm ran; correlate the exact executor version with the matching aggregate and negative-counter evidence."
	action := "Do not manually re-run reconciliation. Confirm the exact executor has the balance_id index and page-local additive reconciler. Retain those fixes where present and roll them out only where version or code evidence says they are absent; if they are present, treat repeated overruns as a page-walk or storage regression and profile that phase. Observe recurring scheduled runs because one fast run does not prove fleet convergence."
	verify := "Every active taskworker generation keeps scheduled reconciliations below 120s, already-correct mirrors receive no rewrite, aggregate drift converges, and no new netescrow-negative lines appear for a full reconciliation interval."
	profile, profileErr := self.statementProfile(ctx, env)
	if profileErr != nil {
		evidence += " PostgreSQL statement attribution was unavailable: " + profileErr.Error()
	} else if 0 < profile.reservationCalls {
		reservationLifetimeMeanMs := profile.reservationTotalMs / float64(profile.reservationCalls)
		balanceLifetimeMeanMs := 0.0
		if 0 < profile.balanceCalls {
			balanceLifetimeMeanMs = profile.balanceTotalMs / float64(profile.balanceCalls)
		}
		observed += fmt.Sprintf(
			" reservation_page_calls=%d reservation_page_lifetime_mean_ms=%.1f reservation_page_max_ms=%.1f reservation_page_legacy_any_calls=%d reservation_page_bounded_lateral_calls=%d reservation_page_unsettled_partial_calls=%d balance_page_calls=%d balance_page_lifetime_mean_ms=%.1f balance_page_max_ms=%.1f",
			profile.reservationCalls,
			reservationLifetimeMeanMs,
			profile.reservationMaxMs,
			profile.legacyReservationCalls,
			profile.boundedReservationCalls,
			profile.unsettledReservationCalls,
			profile.balanceCalls,
			balanceLifetimeMeanMs,
			profile.balanceMaxMs,
		)
		reservationMeanMs := reservationLifetimeMeanMs
		balanceMeanMs := balanceLifetimeMeanMs
		profileWindow := "lifetime"
		if 0 < profile.reservationDeltaCalls {
			reservationMeanMs = profile.reservationDeltaMeanMs
			profileWindow = "adjacent-sample"
			observed += fmt.Sprintf(
				" reservation_page_delta_calls=%d reservation_page_delta_mean_ms=%.1f",
				profile.reservationDeltaCalls,
				profile.reservationDeltaMeanMs,
			)
		}
		if 0 < profile.balanceDeltaCalls {
			balanceMeanMs = profile.balanceDeltaMeanMs
			observed += fmt.Sprintf(
				" balance_page_delta_calls=%d balance_page_delta_mean_ms=%.1f",
				profile.balanceDeltaCalls,
				profile.balanceDeltaMeanMs,
			)
		}
		if 0 < profile.legacyReservationDeltaCalls {
			observed += fmt.Sprintf(" reservation_page_legacy_any_delta_calls=%d", profile.legacyReservationDeltaCalls)
		}
		if 0 < profile.boundedReservationDeltaCalls {
			observed += fmt.Sprintf(" reservation_page_bounded_lateral_delta_calls=%d", profile.boundedReservationDeltaCalls)
		}
		if 0 < profile.unsettledReservationDeltaCalls {
			observed += fmt.Sprintf(" reservation_page_unsettled_partial_delta_calls=%d", profile.unsettledReservationDeltaCalls)
		}
		if 1000 <= reservationMeanMs && (balanceMeanMs == 0 || 10*balanceMeanMs < reservationMeanMs) {
			evidence += " The statement comparison separates the reservation join from the cheap keyset scan; cumulative maxima retain tail risk, while adjacent-sample deltas describe only calls since the preceding monitor pass."
			legacyShape := 0 < profile.legacyReservationDeltaCalls ||
				(profile.reservationDeltaCalls == 0 && 0 < profile.legacyReservationCalls && profile.boundedReservationCalls == 0)
			boundedShape := 0 < profile.boundedReservationDeltaCalls ||
				(profile.reservationDeltaCalls == 0 && 0 < profile.boundedReservationCalls && profile.legacyReservationCalls == 0)
			unsettledShape := 0 < profile.unsettledReservationDeltaCalls ||
				(profile.reservationDeltaCalls == 0 && 0 < profile.unsettledReservationCalls && profile.legacyReservationCalls == 0)
			switch {
			case legacyShape:
				mechanism = fmt.Sprintf("The page-local additive algorithm is present, but the deployed reservation statement still uses one 10,000-ID ANY predicate. A read-only production EXPLAIN at schema head 597 selected a parallel sequential scan of the roughly one-billion-row transfer_escrow table for every page instead of transfer_escrow_balance_contract; pg_stat_statements reports the %s mean of %.1fms/page versus %.1fms for the balance keyset. This repeated whole-history plan is the overrun, not the old absolute writer and not merely a missing INCLUDE payload.", profileWindow, reservationMeanMs, balanceMeanMs)
				action = "Apply the online transfer_escrow_unsettled_balance_contract migration, then deploy the bounded-lateral reservation page with settled=false inside its OFFSET 0 optimization boundary while retaining the authoritative outcome IS NULL join. Keep page-local additive corrections and the task deadline; do not force a global planner setting or manually re-run reconciliation."
				verify = "Post-deploy pg_stat_statements shows unsettled-partial bounded-lateral delta calls and zero new legacy-ANY calls; the reservation-page adjacent mean stays below 1s, scheduled runs finish below 120s, aggregate drift remains below 256GiB, and negative-counter emitters remain quiet for a full interval."
			case unsettledShape:
				mechanism = fmt.Sprintf("The unsettled partial covering access path is present, but pg_stat_statements still attributes the %s mean of %.1fms/page versus %.1fms for the balance keyset. Historical settled escrow ranges and balance-byte heap fetches are excluded; remaining cost is in the small unsettled range, contract outcome probes, index visibility, or unrelated storage contention.", profileWindow, reservationMeanMs, balanceMeanMs)
				action = "Keep the partial covering index, settled=false prefilter, authoritative outcome IS NULL join, bounded-lateral boundary, and page-local additive corrections. Capture an isolated EXPLAIN (ANALYZE, BUFFERS) for one production-shaped page before changing page size or planner settings."
				verify = "The unsettled-partial adjacent mean stays below 1s with bounded tail latency, scheduled runs finish below 120s, aggregate drift remains below 256GiB, and negative-counter emitters remain quiet for a full interval."
			case boundedShape:
				mechanism = fmt.Sprintf("The bounded-lateral reservation page is present, but pg_stat_statements still attributes the %s mean of %.1fms/page versus %.1fms for the balance keyset. The whole-table ANY-plan regression is excluded, but each balance range still traverses settled history and fetches balance_byte_count from the heap; production has more than a billion escrow rows while only a small fraction is unsettled.", profileWindow, reservationMeanMs, balanceMeanMs)
				action = "Apply the online transfer_escrow_unsettled_balance_contract partial covering index, then deploy the matching settled=false prefilter inside the existing lateral boundary. Retain outcome IS NULL as the authoritative open-reservation predicate; settled=false is only a necessary prefilter and must not replace the outcome join."
				verify = "pg_stat_statements shows new unsettled-partial delta calls, its adjacent mean stays below 1s, scheduled runs finish below 120s, aggregate drift remains below 256GiB, and negative-counter emitters remain quiet for a full interval."
			default:
				mechanism = fmt.Sprintf("The current page-local additive algorithm is present, and pg_stat_statements attributes its database cost to the open-reservation join: the %s mean is %.1fms/page versus %.1fms for the balance-id keyset page. Statement counters do not yet identify whether the latest run used legacy ANY, historical bounded-lateral, or unsettled-partial bounded-lateral access.", profileWindow, reservationMeanMs, balanceMeanMs)
				action = "Keep the page-local additive semantics and task deadline. Wait for an adjacent statement sample that identifies legacy-ANY, historical bounded-lateral, or unsettled-partial calls before changing the access path; do not manually re-run reconciliation."
				verify = "A fresh adjacent sample identifies the deployed reservation shape, then its mean stays below 1s and scheduled runs finish below 120s without large drift or negative-counter aftermath."
			}
		}
	}

	findings = append(findings, finding{
		probeId: "pg/netescrow-reconcile-overrun", tier: tierWarn,
		class: "netescrow-reconcile-overrun", target: target, frame: "ReconcileNetEscrow", sustain: 1,
		symptom: fmt.Sprintf(
			"ReconcileNetEscrow has %s run lasting %ds (safe band < %ds)",
			phaseWithArticle,
			durationSeconds,
			int(netEscrowReconcileRunLimit/time.Second),
		),
		mechanism: mechanism,
		baseline:  "Recurring reconciliation finishes in under 120s; production's normal band was approximately 15-55s.",
		observed:  observed,
		evidence:  evidence,
		context:   incidentContext + " pending_task.run_at is only the due time, so it is deliberately not misreported as live execution duration.",
		action:    action,
		verify:    verify,
		playbook:  "SIGNALS.md §5.11",
	})
	return findings, nil
}

func netEscrowAggregates(logOutput string) []netEscrowAggregate {
	aggregates := []netEscrowAggregate{}
	for _, line := range strings.Split(logOutput, "\n") {
		match := netEscrowAggregateRe.FindStringSubmatch(line)
		if len(match) < 7 {
			continue
		}
		overBytes, overOK := parseNetEscrowBytes(match[3], match[4])
		underBytes, underOK := parseNetEscrowBytes(match[5], match[6])
		if !overOK || !underOK {
			continue
		}
		aggregates = append(aggregates, netEscrowAggregate{
			balances:   atoi(match[1]),
			networks:   atoi(match[2]),
			overText:   strings.ToLower(match[3] + match[4]),
			underText:  strings.ToLower(match[5] + match[6]),
			overBytes:  overBytes,
			underBytes: underBytes,
			identity:   parseWarpLogIdentity(line),
		})
	}
	return aggregates
}

func parseNetEscrowBytes(value, unit string) (int64, bool) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	multiplier := int64(1)
	switch strings.ToLower(unit) {
	case "b":
	case "kib":
		multiplier = 1 << 10
	case "mib":
		multiplier = 1 << 20
	case "gib":
		multiplier = 1 << 30
	case "tib":
		multiplier = 1 << 40
	default:
		return 0, false
	}
	return int64(n * float64(multiplier)), true
}

func latestLargeNetEscrowDrift(aggregates []netEscrowAggregate) (netEscrowAggregate, *netEscrowAggregate, string, bool, bool) {
	incidentIndex := -1
	for i := range aggregates {
		if aggregates[i].overBytes >= netEscrowLargeDriftBytes || aggregates[i].underBytes >= netEscrowLargeDriftBytes {
			incidentIndex = i
		}
	}
	if incidentIndex < 0 {
		return netEscrowAggregate{}, nil, "", false, false
	}
	incident := aggregates[incidentIndex]
	for _, adjacentIndex := range []int{incidentIndex - 1, incidentIndex + 1} {
		if adjacentIndex < 0 || len(aggregates) <= adjacentIndex {
			continue
		}
		adjacent := &aggregates[adjacentIndex]
		if approximatelyEqualNetEscrowBytes(adjacent.underBytes, incident.overBytes) &&
			adjacent.underBytes >= netEscrowLargeDriftBytes && incident.overBytes >= netEscrowLargeDriftBytes {
			return incident, adjacent, "under-to-over", false, true
		}
		if approximatelyEqualNetEscrowBytes(adjacent.overBytes, incident.underBytes) &&
			adjacent.overBytes >= netEscrowLargeDriftBytes && incident.underBytes >= netEscrowLargeDriftBytes {
			return incident, adjacent, "over-to-under", false, true
		}
	}
	predecessorOutsideWindow := incidentIndex == 0 && 1 < len(aggregates)
	return incident, nil, "", predecessorOutsideWindow, true
}

func approximatelyEqualNetEscrowBytes(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	if a > b {
		a, b = b, a
	}
	return float64(a)/float64(b) >= 0.8
}
