package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestNetEscrowSignalSyntheticReconcileOverrun(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM pg_stat_statements") {
				return []Row{{"0", "0", "0", "0", "0", "0"}}, nil
			}
			if !strings.Contains(query, "run_end_time > now() - interval '45 minutes'") {
				t.Fatalf("query does not retain a completed overrun through its aftermath: %s", query)
			}
			if !strings.Contains(query, "ORDER BY run_end_time DESC") {
				t.Fatalf("query does not select the latest completed overrun for lifecycle attribution: %s", query)
			}
			if strings.Contains(query, "pending_task") || strings.Contains(query, "now()-run_at") {
				t.Fatalf("query mistakes a pending task's due time for its execution start: %s", query)
			}
			return []Row{{"completed", "1182", "300"}}, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "netescrow-reconcile-overrun")
	if !strings.Contains(alert.Observed, "duration_s=1182") ||
		!strings.Contains(alert.Mechanism, "Duration alone cannot identify which algorithm ran") ||
		!strings.Contains(alert.Action, "Confirm the exact executor has the balance_id index and page-local additive reconciler") ||
		!strings.Contains(alert.Action, "roll them out only where version or code evidence says they are absent") ||
		!strings.Contains(alert.Verify, "already-correct mirrors receive no rewrite") {
		t.Fatalf("overrun alert lost its discriminating evidence or remediation: %+v", alert)
	}
	if strings.Contains(alert.Markdown(), "Roll out the balance_id index and the page-local additive reconciler") {
		t.Fatalf("overrun alert retained a stale unconditional rollout diagnosis: %s", alert.Markdown())
	}
}

func TestNetEscrowSignalAttributesReservationPageAmplification(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM pg_stat_statements") {
				for _, want := range []string{"legacy_reservation_page", "bounded_reservation_page", "CROSS JOIN LATERAL"} {
					if !strings.Contains(query, want) {
						t.Fatalf("statement classifier is missing %q:\n%s", want, query)
					}
				}
				return []Row{{"2128", "16758829.6", "74031.3", "968502", "4456375.4", "12251.5", "2128", "0"}}, nil
			}
			return []Row{{"completed", "177", "30"}}, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "netescrow-reconcile-overrun").Markdown()
	for _, detail := range []string{
		"reservation_page_lifetime_mean_ms=7875.4",
		"reservation_page_max_ms=74031.3",
		"reservation_page_legacy_any_calls=2128",
		"reservation_page_bounded_lateral_calls=0",
		"balance_page_lifetime_mean_ms=4.6",
		"10,000-ID ANY predicate",
		"parallel sequential scan",
		"roughly one-billion-row transfer_escrow",
		"not merely a missing INCLUDE payload",
		"bounded-lateral reservation page",
		"OFFSET 0 optimization boundary",
		"needs no new index or migration",
		"zero new legacy-ANY calls",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("net-escrow page attribution missing %q:\n%s", detail, markdown)
		}
	}
}

func TestNetEscrowSignalDoesNotMisdiagnoseBoundedLateralAsLegacyPlan(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM pg_stat_statements") {
				return []Row{{"91", "182000", "9000", "900", "3600", "25", "0", "91"}}, nil
			}
			return []Row{{"completed", "210", "30"}}, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "netescrow-reconcile-overrun").Markdown()
	for _, detail := range []string{
		"reservation_page_legacy_any_calls=0",
		"reservation_page_bounded_lateral_calls=91",
		"bounded-lateral reservation page is present",
		"whole-table ANY-plan regression is excluded",
		"isolated production-shaped benchmark",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("bounded-lateral attribution missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "Deploy the bounded-lateral reservation page") {
		t.Fatalf("bounded-lateral profile prescribed its already-present fix:\n%s", markdown)
	}
	if strings.Contains(markdown, "a adjacent-sample") {
		t.Fatalf("bounded-lateral alert rendered the wrong timing-window article:\n%s", markdown)
	}
}

func TestNetEscrowSignalAdjacentWindowRendersHumanReadableMarkdown(t *testing.T) {
	profiles := []Row{
		{"10", "10000", "2000", "10", "100", "10", "10", "0"},
		{"12", "14000", "2000", "12", "120", "10", "12", "0"},
	}
	profileIndex := 0
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM pg_stat_statements") {
				row := profiles[profileIndex]
				profileIndex++
				return []Row{row}, nil
			}
			return []Row{{"completed", "210", "30"}}, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	signal := NewNetEscrowSignal()
	if _, err := signal.Run(context.Background(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "netescrow-reconcile-overrun").Markdown()
	if !strings.Contains(markdown, "reports the adjacent-sample mean") || strings.Contains(markdown, "a adjacent-sample") {
		t.Fatalf("adjacent timing window is not human readable:\n%s", markdown)
	}
}

func TestNetEscrowStatementProfileUsesAdjacentCounterDelta(t *testing.T) {
	probe := &netEscrowProbe{}
	first := probe.observeStatementProfile(netEscrowStatementCounters{
		reservationCalls:        100,
		reservationTotalMs:      1000,
		legacyReservationCalls:  100,
		boundedReservationCalls: 0,
		balanceCalls:            200,
		balanceTotalMs:          400,
	})
	if first.reservationDeltaCalls != 0 || first.legacyReservationDeltaCalls != 0 ||
		first.boundedReservationDeltaCalls != 0 || first.balanceDeltaCalls != 0 {
		t.Fatalf("first profile fabricated a delta: %+v", first)
	}
	second := probe.observeStatementProfile(netEscrowStatementCounters{
		reservationCalls:        104,
		reservationTotalMs:      5000,
		legacyReservationCalls:  100,
		boundedReservationCalls: 4,
		balanceCalls:            220,
		balanceTotalMs:          500,
	})
	if second.reservationDeltaCalls != 4 || second.reservationDeltaMeanMs != 1000 ||
		second.legacyReservationDeltaCalls != 0 || second.boundedReservationDeltaCalls != 4 ||
		second.balanceDeltaCalls != 20 || second.balanceDeltaMeanMs != 5 {
		t.Fatalf("adjacent profile delta = %+v", second)
	}
}

func TestNetEscrowSignalCompletedRunSupersedesLingeringHeartbeat(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"completed", "1492", "60"}}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[taskworker]eval active(1481.89s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "netescrow-reconcile-overrun")
	if !strings.Contains(alert.Symptom, "a completed run lasting 1492s") ||
		!strings.Contains(alert.Observed, "phase=completed") {
		t.Fatalf("finished_task did not supersede the same run's lingering active heartbeat: %+v", alert)
	}
}

func TestNetEscrowSignalSyntheticActiveHeartbeat(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" {
				t.Fatalf("unexpected active-task command: %s %s", name, strings.Join(args, " "))
			}
			if strings.Contains(joined, "--query=[sm]reconcile net escrow") {
				return "", nil
			}
			if !strings.Contains(joined, "--query=ReconcileNetEscrow") {
				t.Fatalf("unexpected active-task command: %s %s", name, strings.Join(args, " "))
			}
			return "[edge-3][taskworker][g2][cid:old123][I][2026-08-30T14:03:44Z][task.go:1938][01a05319-b85b-2d49-4564-7eb70075486c]eval active(243.75s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})\n" +
				"[taskworker]eval active(900.00s) github.com/urnetwork/server/taskworker/work.DbMaintenance({})", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "netescrow-reconcile-overrun")
	if !strings.Contains(alert.Observed, "phase=active duration_s=243") {
		t.Fatalf("active heartbeat was not used as live duration: %+v", alert)
	}
	for _, want := range []string{
		"active_task_id=01a05319-b85b-2d49-4564-7eb70075486c",
		"active_host=edge-3",
		"active_generation=g2",
		"active_container=old123",
		"fleet alternates fast and long runs",
		"one executor's fast pass does not prove the deployed algorithm is fixed",
		"one fast run does not prove fleet convergence",
		"treat repeated overruns as a page-walk or storage regression",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("active alert lost rollout identity %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Observed, "completed_age_s") {
		t.Fatalf("active run without a completed precursor fabricated completion age: %+v", alert)
	}
}

func TestNetEscrowSignalSyntheticShortMatchedLargeDrift(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" {
				t.Fatalf("unexpected local command: %s %s", name, joined)
			}
			if strings.Contains(joined, "--query=ReconcileNetEscrow") {
				return "[taskworker]eval active(20.01s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
			}
			if !strings.Contains(joined, "--since=15m") || !strings.Contains(joined, "--query=[sm]reconcile net escrow") {
				t.Fatalf("aggregate query lost its bounded incident window: %s", joined)
			}
			return "[edge-3][taskworker][g2][cid:old123][I][2026-08-30T13:47:40Z][subscription_work.go:341][sm]reconcile net escrow: 897957 balances, 1678 networks drifted, over-reserved 15.14gib, under-reserved 1004.36gib\n" +
				"[edge-4][taskworker][g1][cid:new456][I][2026-08-30T13:53:00Z][subscription_work.go:341][sm]reconcile net escrow: 897970 balances, 1661 networks drifted, over-reserved 1006.26gib, under-reserved 14.09gib", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("short large drift should produce one aggregate alert, got %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, "netescrow-large-drift")
	markdown := alert.Markdown()
	for _, detail := range []string{
		"over_reserved=1006.26gib",
		"under_reserved=14.09gib",
		"previous_under_reserved=1004.36gib",
		"matched_reversal=true",
		"reversal_direction=under-to-over",
		"source_host=edge-4",
		"source_generation=g1",
		"source_container=new456",
		"previous_source_host=edge-3",
		"previous_source_generation=g2",
		"previous_source_container=old123",
		"within the nominal 120s duration band",
		"Negative counters can remain zero",
		"page-local additive reconciler",
		"Confirm the exact source executor",
		"roll them out only where version or code evidence says they are absent",
		"treat it as a regression",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("large-drift alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "matched_reversal=false") {
		t.Fatalf("matched reversal rendered a contradictory false field:\n%s", markdown)
	}
}

func TestNetEscrowSignalSyntheticShortOneDirectionLargeDrift(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn: func(_ string, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "--query=[sm]reconcile net escrow") {
				return "[sm]reconcile net escrow: 898091 balances, 1464 networks drifted, over-reserved 29.05gib, under-reserved 380.77gib", nil
			}
			return "[taskworker]eval active(19.00s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("one-direction large drift should produce one aggregate alert, got %+v", alerts)
	}
	markdown := alerts[0].Markdown()
	for _, detail := range []string{"under_reserved=380.77gib", "threshold_bytes=274877906944", "matched_reversal=false", "requires tracing the affected durable reservation"} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("one-direction large-drift alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "do not clear a matched aggregate reversal") {
		t.Fatalf("one-direction alert claimed that an unmatched correction was already a matched reversal:\n%s", markdown)
	}
}

func TestNetEscrowSignalWindowBoundaryDoesNotRewriteMatchedHistory(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn: func(_ string, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "--query=[sm]reconcile net escrow") {
				// The preceding 2.70TiB over-reserved half of this proven
				// reversal has just aged out; only its 2.37TiB inverse and two
				// later healthy passes remain in the bounded window.
				return "[sm]reconcile net escrow: 898179 balances, 2047 networks drifted, over-reserved 115.91gib, under-reserved 2.37tib\n" +
					"[sm]reconcile net escrow: 898192 balances, 994 networks drifted, over-reserved 66.33gib, under-reserved 42.61gib\n" +
					"[sm]reconcile net escrow: 898206 balances, 1012 networks drifted, over-reserved 37.03gib, under-reserved 65.06gib", nil
			}
			return "", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "netescrow-large-drift")
	markdown := alert.Markdown()
	for _, detail := range []string{
		"matched_reversal=unknown_window_boundary",
		"oldest aggregate retained",
		"preceding scheduled aggregate is no longer observable",
		"Do not replace an earlier matched-reversal attribution",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("window-boundary alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "matched_reversal=false") {
		t.Fatalf("truncated history was rewritten as an unmatched correction:\n%s", markdown)
	}
}

func TestNetEscrowSignalSyntheticShortOrdinaryDriftIsHealthy(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn: func(_ string, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "--query=[sm]reconcile net escrow") {
				return "[sm]reconcile net escrow: 897944 balances, 966 networks drifted, over-reserved 45.63gib, under-reserved 42.09gib", nil
			}
			return "[taskworker]eval active(20.01s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("ordinary short reconcile drift produced alerts: %+v", alerts)
	}
}

func TestNetEscrowSignalActiveSuccessorRetainsCompletedPrecursor(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"completed", "1096", "420"}}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[taskworker]eval active(265.12s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
		},
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "netescrow-reconcile-overrun")
	for _, detail := range []string{
		"phase=active duration_s=265",
		"precursor_completed_duration_s=1096",
		"precursor_completed_age_s=420",
		"active successor follows a completed 1096s overrun",
		"active successor does not erase its completed overrun precursor",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, detail) {
			t.Fatalf("overrun chain alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestNetEscrowSignalSyntheticHealthyRun(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn:    func(string, ...string) (string, error) { return "", nil },
	}
	alerts, err := NewNetEscrowSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy reconciliation produced alerts: %+v", alerts)
	}
}
