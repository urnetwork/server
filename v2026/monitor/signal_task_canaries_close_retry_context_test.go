// A stored task failure cannot inherit progress or financial authority from
// raw error text or a nearby summary. All fixtures are synthetic and identity-free.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const closeRetryPrivateText = "synthetic-private-close-error password=fixture-only 192.0.2.73"

// A live parent keeps a failed late source as component visibility. The public
// canceled-owner controls retain source-error identity for an ended parent.
func closeRetryContextAlerts(t *testing.T, rows []Row, queryError error) Alerts {
	t.Helper()
	queryMarkers := []string{"UpdateClientLocations", "WITH history AS", "WITH failures AS"}
	queryReads := 0
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if queryReads >= len(queryMarkers) || !strings.Contains(query, queryMarkers[queryReads]) {
				t.Error("retry context changed source order or read past the owning failure summary")
				return nil, errors.New("synthetic unexpected task query")
			}
			queryReads++
			switch queryReads {
			case 1:
				return []Row{{"12"}}, nil
			case 2:
				return nil, nil
			default:
				return rows, queryError
			}
		},
		localFn: func(string, ...string) (string, error) {
			t.Error("retry context added an unauthorized auxiliary observation")
			return "", errors.New("synthetic unexpected local read")
		},
	}
	alerts, err := NewWithSignals(syntheticSettings(source), NewTaskCanariesSignal()).Run(context.Background())
	if err != nil {
		t.Fatal("live-parent retry observation returned a top-level source error")
	}
	if queryReads != len(queryMarkers) {
		t.Fatal("retry context changed the owning read count")
	}
	return alerts
}

// Preserve the financial warning's identity, counts, and fixed error class.
func requireCloseRetryContextWarning(t *testing.T, alerts Alerts, parked string, fresh string) Alert {
	t.Helper()
	alert := requireAlertClass(t, alerts, "task-parked")
	if alert.Frame != "CloseExpiredContracts" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
		t.Fatal("retry context changed the unresolved task identity or severity")
	}
	for _, required := range []string{
		"failing_rows=1", "max_errors=17", "parked_over_5m=" + parked,
		"fresh_claim_heartbeats=" + fresh,
	} {
		if !strings.Contains(alert.Observed, required) {
			t.Error("retry context changed a stored failure or schedule value")
		}
	}
	encoded, err := json.Marshal(alert)
	if err != nil {
		t.Fatal(err)
	}
	var jsonl bytes.Buffer
	if err := (Alerts{alert}).WriteJSONL(&jsonl); err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{alert.Markdown(), string(encoded), jsonl.String()} {
		for _, forbidden := range []string{"synthetic-private-close-error", "fixture-only", "192.0.2.73"} {
			if strings.Contains(rendered, forbidden) {
				t.Error("retry context exposed a private synthetic error")
			}
		}
	}
	return alert
}

// Useful terminal siblings are a healthy control on the claim of total failure,
// not permission to mark a still-failing task healthy. The raw summary is not authority.
func TestTaskCanariesCloseRetryPartialProgressDoesNotClearFailure(t *testing.T) {
	alerts := closeRetryContextAlerts(t, []Row{{"CloseExpiredContracts", "1", "0", "1", "17", "3",
		closeRetryPrivateText + " terminal_verified=7 unresolved_accounting=1", "1800", "1", "other=1"}}, nil)
	alert := requireCloseRetryContextWarning(t, alerts, "0", "1")
	if !strings.Contains(alert.Evidence, "representative_error_class=unclassified") {
		t.Fatal("partial-progress text changed the fixed cause class")
	}
	for _, required := range []string{
		"cumulative stored retry count",
		"independently terminal-verified siblings make useful progress",
		"this query does not observe that batch proof",
		"Do not clear the financial warning",
	} {
		if !strings.Contains(alert.Context, required) {
			t.Error("retry alert omits the useful-sibling versus failed-task distinction")
		}
	}
	if strings.Contains(alert.Observed, "terminal_verified=7") {
		t.Error("private error text manufactured observed batch progress")
	}
}

// The same synthetic row can claim zero progress in its private error text. The
// grouped query cannot authenticate that claim and must preserve the failure.
func TestTaskCanariesCloseRetryZeroProgressDoesNotBecomeHealthy(t *testing.T) {
	alerts := closeRetryContextAlerts(t, []Row{{"CloseExpiredContracts", "1", "0", "0", "17", "90",
		closeRetryPrivateText + " terminal_verified=0 unresolved_accounting=1", "1800", "1", "other=1"}}, nil)
	alert := requireCloseRetryContextWarning(t, alerts, "0", "0")
	for _, required := range []string{
		"Batch progress, typed retry authority, selected retry delay and executor lineage remain unobserved here",
		"other/unclassified class does not distinguish ordinary backoff",
	} {
		if !strings.Contains(alert.Context, required) {
			t.Error("retry alert turned unclassified error text into progress or cadence authority")
		}
	}
	if strings.Contains(alert.Observed, "terminal_verified=0") {
		t.Error("retry alert manufactured zero batch progress from error text")
	}
}

// A genuinely parked row proves a future due time, but not that its prior
// attempt made zero terminal progress. It must remain visible without that claim.
func TestTaskCanariesCloseRetryParkedStateDoesNotProveNoProgress(t *testing.T) {
	alerts := closeRetryContextAlerts(t, []Row{{"CloseExpiredContracts", "1", "1", "0", "17", "901",
		closeRetryPrivateText, "1800", "1", "other=1"}}, nil)
	alert := requireCloseRetryContextWarning(t, alerts, "1", "0")
	if !strings.Contains(alert.Observed, "sample_run_at_in_s=901") {
		t.Fatal("parked retry lost its authoritative future schedule")
	}
	if !strings.Contains(alert.Context, "describe scheduling and lease state, not successful or zero batch progress") ||
		!strings.Contains(alert.Context, "a nearby completed-batch summary does not establish a same-attempt join") {
		t.Error("parked retry alert lacks schedule-versus-progress and join boundaries")
	}
}

// Mixed class visibility and existing remediation remain; the generic qualifier
// must not convert one matching escrow diagnostic into accounting-only authority.
func TestTaskCanariesCloseRetryMixedCauseStaysMixed(t *testing.T) {
	alerts := closeRetryContextAlerts(t, []Row{{"CloseExpiredContracts", "2", "1", "1", "17", "901",
		closeRetryPrivateText, "1800", "2", "other=1,schema-object-missing=1"}}, nil)
	alert := requireAlertClass(t, alerts, "task-parked")
	if !strings.Contains(alert.Observed, "cause_classes=2") ||
		!strings.Contains(alert.Mechanism, "2 distinct error classes") ||
		!strings.Contains(alert.Action, "each listed cause class independently") {
		t.Fatal("retry context weakened the mixed-cause owner")
	}
	if !strings.Contains(alert.Context, "complete error authority") {
		t.Error("mixed retry alert lost the complete-error authority requirement")
	}
}

// Neither an empty failed response nor partial rows with an error establish
// task state. The failed component keeps a fixed, private, unobservable identity.
func TestTaskCanariesCloseRetryUnavailableEvidenceControl(t *testing.T) {
	for _, partialRows := range []bool{false, true} {
		var rows []Row
		if partialRows {
			rows = []Row{{"CloseExpiredContracts", "1", "1", "0", "17", "901",
				closeRetryPrivateText, "1800", "1", "other=1"}}
		}
		alerts := closeRetryContextAlerts(t, rows, errors.New("permission denied: "+closeRetryPrivateText))
		if len(alerts) != 1 {
			t.Fatal("unavailable retry query fabricated task state or lost its visibility warning")
		}
		alert := requireTaskPartialUnknown(t, NewTaskCanariesSignal(), alerts, "task-failures", observationErrorClassAccessDenied)
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		var jsonl bytes.Buffer
		if err := alerts.WriteJSONL(&jsonl); err != nil {
			t.Fatal(err)
		}
		for _, rendered := range []string{alert.Markdown(), string(encoded), jsonl.String()} {
			for _, forbidden := range []string{"synthetic-private-close-error", "fixture-only", "192.0.2.73"} {
				if strings.Contains(rendered, forbidden) {
					t.Error("unavailable retry query exposed a private synthetic error")
				}
			}
		}
	}
}

// A held or delivered component unknown cannot consume a recovery observation.
// Explicit run-loop ticks and completed notifier handling establish the ordering.
func TestTaskCanariesCloseRetryUnavailableEvidenceDoesNotClearIncident(t *testing.T) {
	var run atomic.Int32
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH history AS"):
				return nil, nil
			case strings.Contains(query, "WITH failures AS"):
				rows := []Row{{"CloseExpiredContracts", "1", "1", "0", "17", "901",
					closeRetryPrivateText, "1800", "1", "other=1"}}
				if run.Load() == 0 {
					return rows, nil
				}
				if 2 <= run.Load() && run.Load() < 5 {
					return rows, errors.New("permission denied: " + closeRetryPrivateText)
				}
				return nil, nil
			default:
				t.Error("retry recovery added an unexpected query")
				return nil, errors.New("synthetic unexpected task query")
			}
		},
		localFn: func(string, ...string) (string, error) {
			t.Error("retry recovery added an unauthorized auxiliary observation")
			return "", errors.New("synthetic unexpected local read")
		},
	}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifierSettings := harness.settings(webhookUrl)
	notifierSettings.ClearAfterCompleteRuns = 2
	notifier := NewSlackAlertNotifier(notifierSettings)
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	signal := NewTaskCanariesSignal()
	postSignalRunsThroughTheGate(t, signal, syntheticSettings(source), 5, func(i int) {
		// One complete empty run precedes the first held unknown: treating
		// that unknown as complete would clear the established warning now.
		if i == 3 {
			requireTaskPartialIncident(t, notifier, "pg/task-parked", "task-parked", "pg-1", true)
			requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-failures", false)
		}
		run.Store(int32(i))
	}, notifier)
	requireTaskPartialIncident(t, notifier, "pg/task-parked", "task-parked", "pg-1", true)
	requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-failures", true)
	postSignalRunsThroughTheGate(t, signal, syntheticSettings(source), 2, func(i int) { run.Store(int32(i + 5)) }, notifier)
	requireTaskPartialIncident(t, notifier, "pg/task-parked", "task-parked", "pg-1", false)
	requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-failures", false)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if countSlackPosts(texts, "[WARN] raised", "pg/task-parked · task-parked · pg-1") != 1 ||
		countSlackPosts(texts, "[CLEARED]", "pg/task-parked · task-parked · pg-1") != 1 ||
		countSlackPosts(texts, "[WARN] raised", "monitor/visibility · cannot-observe · pg-1/task-failures") != 1 ||
		countSlackPosts(texts, "[CLEARED]", "monitor/visibility · cannot-observe · pg-1/task-failures") != 1 {
		t.Fatal("unavailable retry evidence changed incident ownership, visibility sustain or complete recovery")
	}
	for _, text := range texts {
		for _, forbidden := range []string{"synthetic-private-close-error", "fixture-only", "192.0.2.73"} {
			if strings.Contains(text, forbidden) {
				t.Error("retry notifier exposed a private synthetic error")
			}
		}
	}
}

// A valid empty failing-family result stays healthy; the wording is not a new alert.
func TestTaskCanariesCloseRetryHealthyFamilyControl(t *testing.T) {
	if len(closeRetryContextAlerts(t, nil, nil)) != 0 {
		t.Error("retry context manufactured an alert from a complete healthy observation")
	}
}

// The correction is owned by CloseExpiredContracts, not every task that fails.
func TestTaskCanariesCloseRetryOtherTargetControl(t *testing.T) {
	alerts := closeRetryContextAlerts(t, []Row{{"SyntheticOtherTask", "1", "0", "0", "17", "90",
		closeRetryPrivateText, "1800", "1", "other=1"}}, nil)
	alert := requireAlertClass(t, alerts, "task-parked")
	if alert.Frame != "SyntheticOtherTask" || strings.Contains(alert.Context, "cumulative stored retry count") {
		t.Error("CloseExpired-specific interpretation leaked to another target")
	}
}

// This qualifier extends §1.2 without changing the observed financial failure policy.
func TestTaskCanariesCloseRetryCatalogAuthority(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(data), "### 1.2 ")
	if start < 0 {
		t.Fatal("task canary catalog section is missing")
	}
	section := string(data[start:])
	if end := strings.Index(section[1:], "\n### "); end >= 0 {
		section = section[:end+1]
	}
	for _, required := range []string{
		"max_errors is a cumulative stored retry count",
		"outcome=failed is intentional",
		"not a failed-contract count or a matched-window execution rate",
		"missing summary is unknown, not zero terminal progress",
		"no task or attempt identity",
		"does not prove accounting-only classification",
	} {
		if !strings.Contains(section, required) {
			t.Error("task canary catalog lacks the retry/progress authority qualifier")
		}
	}
}
