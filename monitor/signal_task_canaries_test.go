package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestTaskCanariesSignalSyntheticDeadCanary(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "UpdateClientLocations") && strings.Contains(query, "count(*)") {
			return []Row{{"0"}}, nil
		}
		return nil, nil
	}}
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "canary-dead")
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
				!strings.Contains(joined, "--query="+taskID) {
				t.Fatalf("unexpected task heartbeat command: %s %s", name, joined)
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
		"task_id=" + taskID,
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
			if name != "warpctl" || !strings.Contains(joined, "--query="+taskID) {
				t.Fatalf("unexpected task heartbeat command: %s %s", name, joined)
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
		"1,800-second safety boundary",
		"dedicated §5.11 aggregate and negative-counter discriminator",
		"Do not raise MaxTime, manually kick the live claim",
		"SIGNALS.md §5.11 and §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("live net-escrow diagnosis missing %q:\n%s", want, markdown)
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

func TestTaskCanariesSignalExplainsReliabilityReanchorOverrun(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "UpdateClientLocations"):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"UpdateReliabilities", "2482", "1122", "t", "7200"}}, nil
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
		"multi-billion-row history",
		"active INSERT into client_reliability_running",
		"four-hour re-anchor cadence",
		"per-lookback transaction checkpoints",
		"interrupted retry preserves those checkpoints",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("reliability diagnosis missing %q: %s", want, markdown)
		}
	}
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
		"definitive invalid-destination pre-chain attempts",
		"preserving ambiguous-submit idempotency keys",
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
		"definitive invalid-destination pre-chain attempts",
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
		"Solana base58 key registered as MATIC",
		"definitively rejects the destination before creating a transfer",
		"release only this typed definitive pre-chain rejection",
		"Preserve the idempotency key for transport errors",
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
		"balance_id index",
		"page-local additive reconciler",
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
		"100,000-contract cohort",
		"per-contract commits made durable progress",
		"25,000-contract close cohort",
		"existing 92-worker inner pool",
		"older-than-five-minute open set falls",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("close cohort diagnosis missing %q: %s", want, markdown)
		}
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
