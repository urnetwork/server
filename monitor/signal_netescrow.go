package monitor

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
		probe:  netEscrowProbe{},
	}
}

type netEscrowProbe struct{}

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

func (netEscrowProbe) id() string             { return "pg/netescrow-reconcile-overrun" }
func (netEscrowProbe) tier() string           { return tierWarn }
func (netEscrowProbe) cadence() time.Duration { return time.Minute }

func (netEscrowProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
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
		mechanism := "A correction at least 256GiB is far outside the healthy tens-of-GiB band. It can be a lost large mirror write; if the adjacent pass moves nearly the same quantity in the opposite direction, the deployed fleet-wide absolute snapshot overwrote live mirror traffic even though the walk itself stayed below the 120s duration threshold."
		evidence := "Follow the adjacent scheduled aggregate and all three negative-counter emitters. A matched opposite-direction correction identifies stale absolute writes; a one-direction event instead requires tracing the affected durable reservation before attribution."
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
			mechanism = "Adjacent scheduled passes moved nearly the same >=256GiB quantity in opposite directions. Independent lost writes do not repair as a matched inverse; the deployed fleet-wide absolute snapshot overwrote live mirror traffic, and the next pass restored it. This can happen within the nominal 120s duration band when a large live reservation changes during the walk."
			evidence = "The adjacent aggregate pair is the root-cause discriminator. Negative counters can remain zero when no settlement decrements the overwritten mirror before the corrective successor runs."
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
			context:   "This is reservation-mirror integrity, not Redis capacity. A short task and zero negative-counter lines do not clear a large aggregate correction; the adjacent pass determines whether it is a matched reversal. Source host/generation/container identify the exact executor, so a fast pass elsewhere cannot erase this pass's evidence.",
			action:    "Do not manually re-run reconciliation. Confirm the exact source executor has the page-local additive reconciler and atomic release clamp. Retain them where present and roll them out only where version or code evidence says they are absent; if a matched reversal comes from a current executor, treat it as a regression in the bounded delta or release ordering. Let scheduled passes supply convergence evidence.",
			verify:    "Every active taskworker generation produces recurring aggregates below 256GiB in the tens-of-GiB band, already-correct mirrors receive no rewrite, and all negative-counter emitters remain at zero for a full interval.",
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
			observed += " active_task_id=" + activeRun.taskID
		}
		if activeRun.identity.host != "" {
			observed += fmt.Sprintf(
				" active_host=%s active_generation=%s active_container=%s",
				activeRun.identity.host,
				activeRun.identity.generation,
				activeRun.identity.container,
			)
			incidentContext += fmt.Sprintf(
				" The live heartbeat is from %s/%s container %s; retain its task id and executor identity when the fleet alternates fast and long runs, because one executor's fast pass does not prove the deployed algorithm is fixed.",
				activeRun.identity.host,
				activeRun.identity.generation,
				activeRun.identity.container,
			)
		}
	} else {
		observed += fmt.Sprintf(" completed_age_s=%d", ageSeconds)
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
		mechanism: "Reconciliation has exceeded its freshness band. On the current page-local additive path, migration catch-up, index warmup, storage contention, or a large dirty page set can lengthen the bounded walk. On an older absolute-snapshot path, the same duration also expands stale-snapshot exposure and rewrites every mirror. Duration alone cannot identify which algorithm ran; correlate the exact executor version with the matching aggregate and negative-counter evidence.",
		baseline:  "Recurring reconciliation finishes in under 120s; production's normal band was approximately 15-55s.",
		observed:  observed,
		evidence:  evidence,
		context:   incidentContext + " pending_task.run_at is only the due time, so it is deliberately not misreported as live execution duration.",
		action:    "Do not manually re-run reconciliation. Confirm the exact executor has the balance_id index and page-local additive reconciler. Retain those fixes where present and roll them out only where version or code evidence says they are absent; if they are present, treat repeated overruns as a page-walk or storage regression and profile that phase. Observe recurring scheduled runs because one fast run does not prove fleet convergence.",
		verify:    "Every active taskworker generation keeps scheduled reconciliations below 120s, already-correct mirrors receive no rewrite, aggregate drift converges, and no new netescrow-negative lines appear for a full reconciliation interval.",
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
