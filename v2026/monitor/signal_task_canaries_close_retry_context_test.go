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
	"testing"
)

const closeRetryPrivateText = "synthetic-private-close-error password=fixture-only 192.0.2.73"

// Exercise the registered public signal without another log or metric read.
func closeRetryContextAlerts(t *testing.T, rows []Row, queryError error) Alerts {
	t.Helper()
	failureReads := 0
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "UpdateClientLocations"):
				return []Row{{"12"}}, nil
			case strings.Contains(query, "WITH failures AS"):
				failureReads++
				return rows, queryError
			default:
				return nil, nil
			}
		},
		localFn: func(string, ...string) (string, error) {
			t.Error("retry context added an unauthorized auxiliary observation")
			return "", errors.New("synthetic unexpected local read")
		},
	}
	alerts, err := NewWithSignals(syntheticSettings(source), NewTaskCanariesSignal()).Run(context.Background())
	if queryError == nil && err != nil || queryError != nil && !errors.Is(err, queryError) {
		t.Fatal("retry context changed the owning query error")
	}
	if failureReads != 1 {
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

// Unreadable task state is visibility loss, not a financial recovery event.
func TestTaskCanariesCloseRetryUnavailableEvidenceControl(t *testing.T) {
	alerts := closeRetryContextAlerts(t, nil, errors.New(closeRetryPrivateText))
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "error_class=") {
		t.Fatal("unreadable retry state lost its fixed-class visibility warning")
	}
	requireAlertOmits(t, alert, closeRetryPrivateText, "fixture-only", "192.0.2.73")
	for _, other := range alerts {
		if other.Class == "task-parked" {
			t.Error("unavailable query fabricated an observed task state")
		}
	}
}

// A valid empty failing-family result stays healthy; the wording is not a new alert.
func TestTaskCanariesCloseRetryHealthyFamilyControl(t *testing.T) {
	for _, alert := range closeRetryContextAlerts(t, nil, nil) {
		if alert.Class == "task-parked" || strings.Contains(alert.Context, "cumulative stored retry count") {
			t.Error("retry context manufactured a task failure from a valid empty family")
		}
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
