package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestTaskCanariesSignalSyntheticDeadCanary(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "run_end_time > now() - interval '3 minutes'"):
			return []Row{{"0"}}, nil
		case strings.Contains(query, "WITH completion_minutes AS"):
			return []Row{
				{"capacity", "1023", "995", "24", "4", "", ""},
				{"minute", "2026-09-01T19:17:00Z", "1", "3", "3", "", ""},
				{"pending", "1", "0", "0", "120", "", ""},
				{"reindex", "contract_close", "contract_close_pkey_ccnew35", "building index: sorting live tuples", "180", "", ""},
			}, nil
		}
		return nil, nil
	}}
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "canary-dead")
	for _, want := range []string{
		"zero completions alone is not proof of Redis failure",
		"current_pg_clients=1023 active=995",
		"completion_minute=2026-09-01T19:17:00Z completions=1",
		"current_reindex_relation=contract_close",
		"602 statements over 30 seconds",
		"compare §1.3a direct PostgreSQL capacity",
		"do not restart Redis",
		"12-25 completions",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("dead-canary alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalRedactsTaskIdentifiersFromErrors(t *testing.T) {
	taskID := "019f77ae-de98-582a-1c07-83ea3dbd9d0d"
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "run_end_time > now() - interval '3 minutes'"):
			return []Row{{"0"}}, nil
		case strings.Contains(query, "WITH completion_minutes AS"):
			return []Row{{"pending", "1", "0", "0", "0", "", ""}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"AdvancePayment", "1", "1", "0", "2", "600", "[" + taskID + "] synthetic failure", "120", "1", "other=1", "18.6", "32", "8MB"}}, nil
		default:
			return nil, nil
		}
	}}
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := alerts.Markdown()
	if strings.Contains(markdown, taskID) {
		t.Fatalf("task identifier leaked into alert:\n%s", markdown)
	}
	if !strings.Contains(markdown, "[<task-id>] synthetic failure") {
		t.Fatalf("redacted task identifier missing:\n%s", markdown)
	}
}

func TestTaskCanariesSignalSyntheticOverdueAndParkedTasks(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"UpdateClientScores", "7200", "900", "t", "14400"}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"AdvancePayment", "10", "10", "0", "23", "3600", "synthetic OOM", "120"}}, nil
		default:
			return nil, nil
		}
	}}
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	overdue := requireAlertClass(t, alerts, "task-overdue")
	if !strings.Contains(overdue.Markdown(), "max_time_s=14400") {
		t.Fatalf("overdue alert lost configured max time: %s", overdue.Markdown())
	}
	requireAlertClass(t, alerts, "task-parked")
}

func TestTaskCanariesSignalMedianTailCapCatchesRepeatedReaperOverrun(t *testing.T) {
	taskID := "01a052d2-9c33-c78e-1e37-66e411e45c1e"
	var historyQuery string
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				historyQuery = query
				// Exact production shape: a normally short reaper accumulated
				// enough prior hour-scale tails to inflate its p95 to 3,552s.
				return []Row{{"RemoveDisconnectedNetworkClients", "6283", "3552", "t", "14400", "42", taskID}}, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--since=5m") ||
				!strings.Contains(joined, "--limit=5000") ||
				!strings.Contains(joined, "--query=RemoveDisconnectedNetworkClients") ||
				strings.Contains(joined, taskID) {
				t.Fatal("task heartbeat lookup did not use the identifier-free task-family query")
			}
			return "[by-us-fmt-5-edge-3][taskworker][g2][cid:4cf91fd25a2e][I][2026-08-30T15:03:44.466258Z][task.go:1938][" + taskID + "]eval active(6220.53s) github.com/urnetwork/server/taskworker/work.RemoveDisconnectedNetworkClients({})", nil
		},
	}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-overdue")
	for _, want := range []string{
		"has run for 6220s",
		"1200s median-tail guard",
		"7-day p50 42s, p95 3552s",
		"elapsed_source=eval-active",
		"comparison_source=median-tail-cap",
		"heartbeat_attempt_correlated=true",
		"active_host=by-us-fmt-5-edge-3",
		"active_generation=g2",
		"active_container=4cf91fd25a2e",
		"serialized Redis round trips",
		"1,000-client idempotent Redis cleanup chunks",
		"reassigned forward egress owner is preserved",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("median-tail reaper diagnosis missing %q:\n%s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, taskID)
	for _, want := range []string{
		"percentile_cont(0.50)",
		"row_number() OVER (PARTITION BY task",
		"p.task_rank = 1",
	} {
		if !strings.Contains(historyQuery, want) {
			t.Fatalf("overdue query lost %q: %s", want, historyQuery)
		}
	}
}

func TestTaskCanariesSignalDiagnosesLiveNetEscrowOverrun(t *testing.T) {
	taskID := "01a0546b-c769-9420-2055-cd95e18fab76"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return []Row{{"ReconcileNetEscrow", "1530", "1142", "t", "1800", "21", taskID}}, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--query=ReconcileNetEscrow") ||
				strings.Contains(joined, taskID) {
				t.Fatal("task heartbeat lookup did not use the identifier-free task-family query")
			}
			return "[by-us-fmt-5-edge-4][taskworker][g1][cid:52c4c3be3b3f][I][2026-08-30T22:16:30Z][task.go:1938][" + taskID + "]eval active(1517.00s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
		},
	}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-overdue").Markdown()
	for _, want := range []string{
		"has run for 1517s",
		"balance_id lookup index",
		"Duration alone does not identify which algorithm is running",
		"Retain them where present",
		"dedicated §5.11 aggregate and negative-counter discriminator",
		"Do not raise MaxTime, manually kick the live claim",
		"SIGNALS.md §5.11 and §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("live net-escrow diagnosis missing %q:\n%s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalDiagnosesLivePayoutOverrun(t *testing.T) {
	taskID := "01a0088a-327f-ed06-0c69-9b994a1e70fe"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return []Row{{"Payout", "4538", "1800", "f", "21600", "0", taskID}}, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--query=Payout") ||
				strings.Contains(joined, taskID) {
				t.Fatal("task heartbeat lookup did not use the identifier-free task-family query")
			}
			return "[by-us-fmt-5-edge-4][taskworker][g1][cid:52c4c3be3b3f][I][2026-08-30T22:30:00Z][task.go:1938][" + taskID + "]eval active(4500.00s) github.com/urnetwork/server/taskworker/work.Payout({})", nil
		},
	}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-overdue").Markdown()
	for _, want := range []string{
		"Payout",
		"temp_account_payment and subsidy windows",
		"global five-minute idle-in-transaction guard",
		"explicit oldest PostgreSQL transaction",
		"SET LOCAL idle_in_transaction_session_timeout=0",
		"do not disable the database-wide timeout",
		"SIGNALS.md §5.6 and §5.7",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("live Payout diagnosis missing %q:\n%s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalDoesNotMistakeDueQueueDelayForExecutionTime(t *testing.T) {
	taskID := "01a052d2-9c33-c78e-1e37-66e411e45c1e"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return []Row{{"RemoveDisconnectedNetworkClients", "6283", "3552", "t", "14400", "42", taskID}}, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(string, ...string) (string, error) {
			return "[edge-1][taskworker][g1][cid:new][I][2026-08-30T15:03:44Z][task.go:1938][" + taskID + "]eval active(600.00s) github.com/urnetwork/server/taskworker/work.RemoveDisconnectedNetworkClients({})", nil
		},
	}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "task-overdue" {
			t.Fatalf("600s execution was misreported from 6283s-old due time: %+v", alert)
		}
	}
}

func TestTaskCanariesSignalDiagnosesReliabilityRollingIndexRegression(t *testing.T) {
	taskID := "01a05700-1111-2222-3333-444444444444"
	otherTaskID := "01a05700-5555-6666-7777-888888888888"
	var diagnosticQuery string
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return []Row{{"UpdateReliabilities", "2482", "1158", "t", "7200", "78", taskID}}, nil
			case strings.Contains(query, "monitor_reliability_task_diagnostic"):
				diagnosticQuery = query
				return []Row{{
					"rolling-leave", "1515", "", "", "192.0.2.44/32",
					"2", "1", "4", "4", "4", "55", "true", "true", "3", "1900",
				}}, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--query=UpdateReliabilities") ||
				strings.Contains(joined, taskID) {
				t.Fatal("reliability heartbeat lookup did not use the identifier-free task-family query")
			}
			return strings.Join([]string{
				"[edge-4][taskworker][g1][cid:reliabilityworker][I][2026-08-29T11:59:50Z][task.go:1938][" + taskID + "]eval active(1510.00s) github.com/urnetwork/server/taskworker/work.UpdateReliabilities({})",
				"[edge-1][taskworker][g2][cid:unrelatedworker][I][2026-08-29T11:59:55Z][task.go:1938][" + otherTaskID + "]eval active(50.00s) github.com/urnetwork/server/taskworker/work.UpdateReliabilities({})",
			}, "\n"), nil
		},
	}

	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:       "edge-4",
		LANAddress: "192.0.2.44",
		Roles:      []string{"services"},
	})
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-overdue")
	markdown := alert.Markdown()
	for _, want := range []string{
		"heartbeat_attempt_correlated=true",
		"active_host=edge-4 active_generation=g1 active_container=reliabilityworker",
		"reliability_sql_phase=rolling-leave",
		"sql_source=edge-4",
		"transaction_blocked_queries=1",
		"current_windows=true",
		"old_index_present=true covering_index_ready=true",
		"directly falsifies the old repeated-full-anchor diagnosis",
		"All 4 running windows",
		"legacy child for a 30-block rolling-leave slice estimated at 2,142,377 rows",
		"PostgreSQL retained that transaction",
		"Server commit fcb4de54",
		"transaction-local two-hour PostgreSQL checkpoint timeout",
		"supported finalizer drops the old parent",
		"Do not redeploy only for the already-present four-hour/checkpoint fix",
		"representative bounded rolling EXPLAIN selects the covering child",
		"SIGNALS.md §1.2, §5.7, and §8.10",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("reliability diagnosis missing %q: %s", want, markdown)
		}
	}
	for _, stale := range []string{
		"threshold was shorter than the task cadence",
		"forcing a full re-anchor of the multi-billion-row history on every cycle",
		"Roll out the four-hour re-anchor cadence",
	} {
		if strings.Contains(markdown, stale) {
			t.Fatalf("rolling reliability diagnosis retained stale guidance %q", stale)
		}
	}
	for _, want := range []string{
		"monitor_reliability_task_diagnostic",
		"pg_stat_all_indexes",
		"last_idx_scan",
		"client_reliability_running_window",
		"a.pid <> pg_backend_pid()",
	} {
		if !strings.Contains(diagnosticQuery, want) {
			t.Fatalf("reliability diagnostic query missing %q", want)
		}
	}
	requireAlertOmits(t, alert, taskID, otherTaskID, "192.0.2.44")
}

func TestTaskCanariesSignalDiagnosesIntentionalReliabilityAnchor(t *testing.T) {
	taskID := "01a05700-aaaa-bbbb-cccc-111111111111"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return []Row{{"UpdateReliabilities", "2482", "1158", "t", "7200", "78", taskID}}, nil
			case strings.Contains(query, "monitor_reliability_task_diagnostic"):
				return []Row{{
					"full-anchor-insert", "1515", "IO", "DataFileRead", "2001:db8::3/128",
					"1", "0", "4", "4", "4", "245", "false", "true", "-1", "12",
				}}, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--query=UpdateReliabilities") ||
				strings.Contains(joined, taskID) {
				t.Fatal("reliability heartbeat lookup did not use the identifier-free task-family query")
			}
			return "[edge-3][taskworker][g2][cid:reliabilityworker][I][2026-08-29T11:59:50Z][task.go:1938][" + taskID + "]eval active(1510.00s) github.com/urnetwork/server/taskworker/work.UpdateReliabilities({})", nil
		},
	}

	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:           "edge-3",
		OverlayAddress: "2001:db8::3",
		Roles:          []string{"services"},
	})
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-overdue")
	markdown := alert.Markdown()
	for _, want := range []string{
		"reliability_sql_phase=full-anchor-insert",
		"sql_wait=IO:DataFileRead",
		"sql_source=edge-3",
		"max_reanchor_distance_blocks=245",
		"directly confirming a full reliability re-anchor",
		"four-hour (240-block) recompute boundary",
		"deploy Taskworker from Server commit fcb4de54 or later",
		"transaction-local two-hour PostgreSQL checkpoint timeout",
		"next ordinary half-hour cycle uses the rolling path",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("intentional reliability anchor diagnosis missing %q: %s", want, markdown)
		}
	}
	requireAlertOmits(t, alert, taskID, "2001:db8::3")
}

func TestTaskCanariesSignalDoesNotInventReliabilityCauseWithoutDiagnostic(t *testing.T) {
	taskID := "01a05700-dddd-eeee-ffff-222222222222"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return []Row{{"UpdateReliabilities", "2482", "1158", "t", "7200", "78", taskID}}, nil
			case strings.Contains(query, "monitor_reliability_task_diagnostic"):
				return nil, nil
			case strings.Contains(query, "WITH failures AS"):
				return nil, nil
			default:
				return nil, nil
			}
		},
		localFn: func(_ string, _ ...string) (string, error) {
			return "[edge-2][taskworker][g1][cid:reliabilityworker][I][2026-08-29T11:59:50Z][task.go:1938][" + taskID + "]eval active(1510.00s) github.com/urnetwork/server/taskworker/work.UpdateReliabilities({})", nil
		},
	}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-overdue")
	markdown := alert.Markdown()
	for _, want := range []string{
		"reliability_diagnostic=unavailable",
		"former repeating-full-anchor mechanism is not established",
		"Restore the bounded read-only phase diagnostic",
		"Do not redeploy the already-present cadence fix",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("unavailable reliability diagnosis missing %q: %s", want, markdown)
		}
	}
	for _, stale := range []string{
		"forcing a full re-anchor of the multi-billion-row history on every cycle",
		"Roll out the four-hour re-anchor cadence",
	} {
		if strings.Contains(markdown, stale) {
			t.Fatalf("unavailable reliability diagnostic invented stale cause %q", stale)
		}
	}
	requireAlertOmits(t, alert, taskID)
}

func TestTaskCanariesSignalExplainsMaintenanceBlockedByReliabilityAnchor(t *testing.T) {
	var historyQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			historyQuery = query
			return []Row{{"DbMaintenance", "3683", "1800", "f", "86400"}}, nil
		case strings.Contains(query, "pg_stat_progress_create_index"):
			return []Row{{
				"pending_task", "pending_task_pkey_ccnew", "waiting for old snapshots", "3683",
				"Lock", "virtualxid", "{774955}", "774955", "INSERT INTO client_reliability_running SELECT ...", "636", "637",
			}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return nil, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-overdue")
	markdown := alert.Markdown()
	for _, want := range []string{
		"2x its fallback 1800s",
		"comparison_source=fallback",
		"waiting for old snapshots",
		"UpdateReliabilities running-window re-anchor",
		"blocks_done=636 blocks_total=637",
		"do not cancel either progressing task",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("maintenance diagnosis missing %q: %s", want, markdown)
		}
	}
	if !strings.Contains(historyQuery, "coalesce(h.p95_s,1800)") {
		t.Fatalf("missing-history comparison and display do not share the 1800s fallback: %s", historyQuery)
	}
}

func TestTaskCanariesSignalExplainsProgressingMaintenanceAfterBlockerRelease(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"DbMaintenance", "6529", "1800", "f", "86400"}}, nil
		case strings.Contains(query, "pg_stat_progress_create_index"):
			return []Row{{
				"transfer_contract", "transfer_contract_pair_open_create_time_ccnew",
				"building index: scanning table", "195", "", "", "{}", "", "",
				"10114170", "23790366",
			}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return nil, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-overdue").Markdown()
	for _, want := range []string{
		"actively executing REINDEX CONCURRENTLY",
		"blocks_done=10114170 blocks_total=23790366",
		"two-hour per-object limit",
		"A phase or block increase is progress",
		"do not start a duplicate rebuild",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("progressing maintenance diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalAttributesOversizedTransferEscrowReindex(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"DbMaintenance", "9120", "1800", "f", "86400"}}, nil
		case strings.Contains(query, "pg_stat_progress_create_index"):
			return []Row{{
				"transfer_escrow", "transfer_escrow_pkey_ccnew36",
				"building index: scanning table", "190", "IO", "DataFileExtend", "{}", "", "",
				"48213", "1081742992",
			}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return nil, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-overdue").Markdown()
	for _, want := range []string{
		"very large, high-churn relation",
		"two-hour per-object policy",
		"Connect logins and unrelated tasks to time out",
		"numbered _ccnew debris",
		"PgBouncer timeout symptoms are downstream queueing",
		"Do not cancel or duplicate the protected in-progress rebuild",
		"excludes transfer_escrow from full-table reindex",
		"cleans incomplete indexes before and after every selected object",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("transfer_escrow maintenance alert missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsOverlappingParkedAndFreshClaimCounts(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			// One row satisfies both independent snapshots: its next run_at is
			// parked while the previous attempt's claim heartbeat is still fresh.
			return []Row{{"RefreshVerifyProxyEgress", "1", "1", "1", "937", "600", "Interrupted: context canceled", "900"}}, nil
		default:
			return nil, nil
		}
	}}

	settings := syntheticSettings(source)
	settings.VerificationEnabled = false
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-parked")
	markdown := alert.Markdown()
	for _, want := range []string{
		"1 parked >5m; 1 with a fresh claim heartbeat; sets may overlap",
		"parked_over_5m=1 fresh_claim_heartbeats=1 counts_may_overlap=true",
		"do not add them together",
		"verification_enabled=false",
		"stale ungated chain",
		"Do not raise the deadline or pull this row forward",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("overlap diagnosis missing %q: %s", want, markdown)
		}
	}
	if strings.Contains(markdown, "undersized task-specific MaxTime") {
		t.Fatalf("disabled verification retry received the generic deadline diagnosis: %s", markdown)
	}
}

func TestTaskCanariesSignalKeepsEnabledVerificationDeadlineDiagnosisGeneric(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"RefreshVerifyProxyEgress", "1", "1", "0", "1", "600", "Interrupted: context canceled", "900"}}, nil
		default:
			return nil, nil
		}
	}}
	settings := syntheticSettings(source)
	settings.VerificationEnabled = true
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	if !strings.Contains(markdown, "undersized task-specific MaxTime") {
		t.Fatalf("enabled verification retry lost the generic deadline diagnosis: %s", markdown)
	}
	if strings.Contains(markdown, "stale ungated chain") {
		t.Fatalf("enabled verification retry was misclassified as disabled work: %s", markdown)
	}
}

func TestTaskCanariesSignalDoesNotLetNoisyFamilyHideAnother(t *testing.T) {
	var failureQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			failureQuery = query
			// The payment family has ten rows; UpdateClientScores has only one.
			// Both are already grouped by SQL and must become separate alerts.
			return []Row{
				{"AdvancePayment", "10", "10", "0", "1051", "3500", "wallet insufficient", "1800"},
				{"UpdateClientScores", "1", "0", "1", "1", "-2000", "write tcp -> redis:6402: i/o timeout", "7200"},
			}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failureQuery, "PARTITION BY task") || strings.Contains(failureQuery, "LIMIT 10") {
		t.Fatalf("failure query must group before any cap: %s", failureQuery)
	}
	seen := map[string]Alert{}
	for _, alert := range alerts {
		if alert.Class == "task-parked" {
			seen[alert.Frame] = alert
		}
	}
	for _, task := range []string{"AdvancePayment", "UpdateClientScores"} {
		alert, ok := seen[task]
		if !ok {
			t.Fatalf("missing independent alert for %s: %+v", task, alerts)
		}
		if !strings.Contains(alert.Markdown(), task) {
			t.Fatalf("%s alert lost family evidence: %s", task, alert.Markdown())
		}
	}
}

func TestTaskCanariesSignalDoesNotLetDominantCauseMisdescribeFamily(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{
				"AdvancePayment", "384", "384", "0", "1062", "3600",
				"asset amount owned by the wallet is insufficient", "120", "4",
				"wallet-insufficient=368,connection-cleanup-deadline=10,processor-invalid-destination=5,processor-rate-limit=1",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"4 error classes",
		"cannot describe every failing row",
		"cause_breakdown=wallet-insufficient=368,connection-cleanup-deadline=10,processor-invalid-destination=5,processor-rate-limit=1",
		"queued, cursor-batched CompletePayment retention path",
		"verify typed-reset commit b8af229f in every active taskworker artifact",
		"deploy it only to blocks whose artifact predates it",
		"on already-current blocks, repeated rejection means the invalid configured wallet is still selected",
		"every active taskworker contains typed-reset commit b8af229f",
		"never clear payment rows or keys manually",
		"do not accelerate processor-rate-limit rows",
		"verify current-main server commit 66525afc in every active taskworker artifact",
		"shared Redis-time Circle transfer admission",
		"keep the transfer-admission gate fail closed",
		"canonical payout attempts stay below four per second for a full 90-minute retry window",
		"Do not delete or manually replay the mixed family",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("mixed-family alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "The external payout wallet does not own enough") {
		t.Fatalf("dominant wallet sample misdescribed the mixed family:\n%s", markdown)
	}
	if strings.Contains(markdown, "processor-bad-request rows") {
		t.Fatalf("mixed-family alert rendered guidance for an absent class:\n%s", markdown)
	}
	if strings.Contains(markdown, "b8718420") {
		t.Fatalf("mixed-family alert retained former non-ancestor deployment guidance:\n%s", markdown)
	}
	for _, staleRuntimeClaim := range []string{
		"2026.8.31-outerwerld+1033655820",
		"source 1d8f01e5 contains that reset",
		"production taskworker",
	} {
		if strings.Contains(markdown, staleRuntimeClaim) {
			t.Fatalf("mixed-family alert retained stale runtime claim %q:\n%s", staleRuntimeClaim, markdown)
		}
	}
}

func TestTaskCanariesSignalMixedGuidanceMatchesPresentMigrationCauses(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{
				"AdvancePayment", "384", "384", "0", "1071", "1864",
				"the asset amount owned by the wallet is insufficient", "120", "3",
				"wallet-insufficient=369,schema-object-missing=10,processor-invalid-destination=5",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"Handle only the present AdvancePayment classes",
		"restore migration coherence per §8.9",
		"schema-object-missing clears",
		"verify typed-reset commit b8af229f in every active taskworker artifact",
		"deploy it only to blocks whose artifact predates it",
		"SIGNALS.md §1.2, §5.7, and §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration mixed-family alert missing %q:\n%s", want, markdown)
		}
	}
	for _, absent := range []string{
		"CompletePayment retention path",
		"processor-bad-request rows",
		"processor-rate-limit rows",
		"2026.8.31-outerwerld+1033655820",
		"source 1d8f01e5 contains that reset",
	} {
		if strings.Contains(markdown, absent) {
			t.Fatalf("migration mixed-family alert rendered absent cause %q:\n%s", absent, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsInvalidDestinationRecovery(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{
				"AdvancePayment", "5", "5", "0", "1059", "1800",
				"400 Bad Request Invalid destination address.", "120", "1",
				"processor-invalid-destination=5",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"invalid for its declared chain",
		"pre-fix chain-blind validator allowed a Solana base58 key to be stored as MATIC",
		"definitively rejects the destination before creating a transfer",
		"current taskworker clears only that typed pre-chain attempt automatically",
		"same invalid wallet on the next retry",
		"one-hour-mean backoff",
		"dispersed across 30–90 minutes",
		"through the supported account API",
		"Do not manually release the attempt",
		"preserving keys for transport errors",
		"at most 90 minutes plus ingestion delay",
		"without a duplicate transfer",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("invalid-destination alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsNonDrainDeadlineCancellation(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"ExportStats", "1", "0", "1", "1", "-30", "Interrupted: context canceled", "120"}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-parked")
	markdown := alert.Markdown()
	for _, want := range []string{"ExportStats", "sample_max_time_s=120", "non-drain context cancellation", "task-specific MaxTime"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("deadline diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalTreatsNetEscrowDeadlineAsContainment(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{
				"ReconcileNetEscrow", "1", "0", "1", "2", "-1498",
				"Interrupted: context canceled", "1800", "1", "context-canceled=1",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"ReconcileNetEscrow reached its configured safety boundary",
		"not evidence that MaxTime is undersized",
		"balance_id lookup index",
		"page-local additive reconciler",
		"Retain them where present",
		"roll them out only where version or code evidence says they are absent",
		"Do not raise MaxTime or manually kick",
		"SIGNALS.md §5.11 and §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("net-escrow deadline diagnosis missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "undersized task-specific MaxTime") {
		t.Fatalf("net-escrow containment was rendered as an undersized deadline:\n%s", markdown)
	}
	if strings.Contains(markdown, "roll out the page-local additive reconciler plus atomic negative clamp across every taskworker generation") {
		t.Fatalf("net-escrow containment retained a stale unconditional rollout diagnosis:\n%s", markdown)
	}
}

func TestTaskCanariesSignalExplainsClockBackfillDeadline(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{
				"BackfillClock", "1", "0", "1", "3", "-30",
				"Interrupted: context canceled", "600", "1", "context-canceled=1",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"BackfillClock reached its configured safety boundary",
		"2.18 billion contract_close rows",
		"repeats the same aggregate",
		"not a scheduler, RunOnce, Redis, or deadline-size failure",
		"One pending row and one live lease",
		"contiguous, unique daily-rollup prefix",
		"first missing or duplicate day",
		"clock_unrolled_tail",
		"Keep the ten-minute task boundary",
		"Do not add a broad multi-billion-row index",
		"completes below 600 seconds",
		"SIGNALS.md §1.2 and §5.7",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("clock-backfill diagnosis missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "undersized task-specific MaxTime") {
		t.Fatalf("clock backfill was rendered as an undersized deadline:\n%s", markdown)
	}
}

func TestTaskCanariesSignalExplainsLiteralTaskDeadlineTimeout(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"CloseExpiredContracts", "1", "0", "1", "1", "-2", "Timeout", "1800"}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-parked")
	markdown := alert.Markdown()
	for _, want := range []string{"CloseExpiredContracts", "sample_max_time_s=1800", "configured deadline of 1800s", "smaller checkpointed batch"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("literal timeout diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsReliabilityCheckpointRetry(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{
				"UpdateReliabilities", "1", "0", "1", "1", "-3",
				"Interrupted: failed to deallocate cached statement(s): conn closed", "7200",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"exhausted the task's configured deadline",
		"transaction rollback discards every completed lookback",
		"exactly the configured 7200s",
		"checkpoint each lookback in its own transaction",
		"successor claim retains the same args",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("reliability retry diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsCloseCohortDeadline(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"CloseExpiredContracts", "1", "0", "1", "1", "-10", "Timeout", "1800"}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"task row does not contain the selected cohort size",
		"matching live selection log",
		"older 100,000-contract generation from the current 25,000 cap",
		"per-contract commits made durable progress",
		"If n exceeds 25,000",
		"if n is already at or below 25,000",
		"92-worker inner pool",
		"older-than-five-minute open set falls",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("close cohort diagnosis missing %q: %s", want, markdown)
		}
	}
	if strings.Contains(markdown, "The deployed closer selected one 100,000-contract cohort") {
		t.Fatalf("close timeout diagnosis invented an unobserved deployed cohort size: %s", markdown)
	}
}

func TestTaskCanariesSignalExplainsPayoutIdleTransactionClosure(t *testing.T) {
	var failureQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			failureQuery = query
			return []Row{{
				"Payout", "1", "1", "0", "149", "3558",
				"Unhandled: *pgconn.connLockError=conn closed", "21600", "1",
				"idle-transaction-timeout=1",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failureQuery, "THEN 'idle-transaction-timeout'") {
		t.Fatalf("payout conn-closed signature is not classified in SQL: %s", failureQuery)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"cause_breakdown=idle-transaction-timeout=1",
		"five-minute idle-in-transaction timeout",
		"SET LOCAL",
		"Do not disable the database-wide guard",
		"new unrelated PostgreSQL session",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("payout diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsPostgresLocalBufferExhaustion(t *testing.T) {
	var failureQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			failureQuery = query
			return []Row{{
				"Payout", "1", "1", "0", "153", "3600",
				"Unhandled: *pgconn.PgError=ERROR: no empty local buffer available (SQLSTATE 53000)",
				"21600", "1", "postgres-local-buffer-exhaustion=1",
				"18.4 (Ubuntu 18.4-1.pgdg22.04+1)", "200", "8MB",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failureQuery, "THEN 'postgres-local-buffer-exhaustion'") {
		t.Fatalf("PostgreSQL local-buffer signature is not classified in SQL: %s", failureQuery)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"cause_breakdown=postgres-local-buffer-exhaustion=1",
		"pg_server_version=18.4",
		"effective_io_concurrency=200",
		"temp_buffers=8MB",
		"PaymentPlanner.finalizePayments",
		"SET LOCAL effective_io_concurrency = 32",
		"PostgreSQL 18.6",
		"fresh unrelated session retains the configured global value",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("PostgreSQL local-buffer diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsMissingMigrationArtifact(t *testing.T) {
	var failureQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			failureQuery = query
			return []Row{{
				"RemoveCompletedContracts", "1", "0", "1", "7", "43",
				"Unhandled: ERROR: column account_payment.contract_retention_cursor does not exist",
				"1800", "1", "schema-object-missing=1",
			}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failureQuery, "THEN 'schema-object-missing'") {
		t.Fatalf("undefined PostgreSQL objects are not classified in SQL: %s", failureQuery)
	}
	if strings.Index(failureQuery, "sqlstate 42703") > strings.Index(failureQuery, "context canceled") {
		t.Fatalf("specific missing-schema classification must precede generic cancellation: %s", failureQuery)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"cause_breakdown=schema-object-missing=1",
		"schema-dependent code activated before its append-only migration",
		"successful migration_audit head",
		"Do not create the object by hand",
		"SIGNALS.md §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing-schema diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestTaskCanariesSignalExplainsUnfundedPayoutWallet(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return nil, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"AdvancePayment", "384", "384", "0", "1055", "3000", "the asset amount owned by the wallet is insufficient", "120"}}, nil
		default:
			return nil, nil
		}
	}}

	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-parked").Markdown()
	for _, want := range []string{
		"external payout wallet",
		"repeated HTTP 400",
		"Finance/operations must fund",
		"without manual row deletion",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("wallet diagnosis missing %q: %s", want, markdown)
		}
	}
}
