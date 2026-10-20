// Completed task observations survive a later independent source failure at
// the public signal, accept-filter, cadence and notifier boundaries.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

const taskPartialPrivate = "synthetic-private-task-source password=fixture-only 192.0.2.71"

// Each unknown has a fixed component identity and never renders private input.
func requireTaskPartialUnknown(t *testing.T, signal Signal, alerts Alerts, component, errorClass string) Alert {
	t.Helper()
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if unknown.SignalID != "monitor/visibility" || unknown.SignalKey != signal.Key() ||
		unknown.SignalNumber != signal.Number() || unknown.Target != "pg-1/"+component || unknown.Frame != "" ||
		unknown.Severity != SeverityWarn || unknown.Sustain != 2 || unknown.PageSustain != 0 || !unknown.unobservable ||
		unknown.Observed != "error_class="+errorClass ||
		!strings.Contains(unknown.Context, "subsequent components were not observed") {
		t.Fatal("partial source failure changed its fixed identity, privacy or unknown-coverage contract")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, taskPartialPrivate, "fixture-only", "192.0.2.71")
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{taskPartialPrivate, "fixture-only", "192.0.2.71"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatal("partial source JSON rendered private source input")
			}
		}
	}
	return unknown
}

// A complete zero count proves the page before any of the three later source
// failures. A positive count stays non-page; failed partial rows add no finding.
func TestTaskCanariesPartialObservationPreservesCompletedCanary(t *testing.T) {
	for _, component := range []string{"task-overdue", "task-maintenance", "task-failures"} {
		for _, completions := range []string{"0", "12"} {
			t.Logf("component=%s completions=%s", component, completions)
			canaryReads, overdueReads, maintenanceReads, failureReads := 0, 0, 0, 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				switch {
				case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
					canaryReads++
					return []Row{{completions}}, nil
				case strings.Contains(query, "WITH completion_minutes AS"):
					return nil, nil
				case strings.Contains(query, "WITH history AS"):
					overdueReads++
					if component == "task-overdue" {
						return []Row{{"PartialTask", "9000", "30", "t", "14400", "30", ""}}, errors.New("permission denied: " + taskPartialPrivate)
					}
					if component == "task-maintenance" {
						return []Row{{"DbMaintenance", "9000", "1800", "f", "86400", "0", ""}}, nil
					}
					return nil, nil
				case strings.Contains(query, "pg_stat_progress_create_index"):
					maintenanceReads++
					return nil, errors.New("permission denied: " + taskPartialPrivate)
				case strings.Contains(query, "WITH failures AS"):
					failureReads++
					if component == "task-failures" && (completions != "0" || failureReads == 2) {
						return []Row{{"PartialTask", "1", "1", "0", "101", "900", "Timeout", "60", "1", "deadline-timeout=1"}}, errors.New("permission denied: " + taskPartialPrivate)
					}
					return nil, nil
				default:
					t.Fatal("unexpected task query")
					return nil, nil
				}
			}}
			signal := NewTaskCanariesSignal()
			alerts, err := signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("later source failure discarded the completed canary observation")
			}
			requireTaskPartialUnknown(t, signal, alerts, component, observationErrorClassAccessDenied)
			wantAlerts, wantFailureReads := 1, 0
			if completions == "0" {
				wantAlerts++
				wantFailureReads++ // Optional evidence read before the later source.
				page := requireAlertClass(t, alerts, "canary-dead")
				if page.SignalID != "pg/canary-dead" || page.Target != "pg-1" ||
					page.Severity != SeverityPage || page.Sustain != 1 || page.unobservable {
					t.Fatal("independent complete canary PAGE changed identity or authority")
				}
			}
			if component == "task-failures" {
				wantFailureReads++
			}
			wantMaintenanceReads := 0
			if component == "task-maintenance" {
				wantMaintenanceReads = 1
			}
			if len(alerts) != wantAlerts || canaryReads != 1 || overdueReads != 1 ||
				maintenanceReads != wantMaintenanceReads || failureReads != wantFailureReads {
				t.Fatal("partial source changed query order, retried, or promoted uncompleted evidence")
			}
		}
	}
}

// The final failure summary is independent of a complete prior overdue row.
func TestTaskCanariesPartialObservationPreservesCompletedOverdue(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"SyntheticLongTask", "9000", "30", "t", "14400", "30", ""}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return nil, errors.New("permission denied: " + taskPartialPrivate)
		default:
			t.Fatal("unexpected task query")
			return nil, nil
		}
	}}
	signal := NewTaskCanariesSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 2 {
		t.Fatal("later failure summary discarded the independent overdue observation")
	}
	requireTaskPartialUnknown(t, signal, alerts, "task-failures", observationErrorClassAccessDenied)
	overdue := requireAlertClass(t, alerts, "task-overdue")
	if overdue.SignalID != "pg/task-overdue" || overdue.Frame != "SyntheticLongTask" || overdue.Sustain != 2 || overdue.unobservable {
		t.Fatal("independent overdue warning changed identity or authority")
	}
}

// Missing/partial initial authority cannot establish the canary page. Complete
// healthy input still needs every later source to complete before it is quiet.
func TestTaskCanariesPartialObservationInitialAuthorityAndHealthyControl(t *testing.T) {
	for _, test := range []struct {
		name string
		rows []Row
		err  error
	}{
		{name: "missing"},
		{name: "extra row", rows: []Row{{"0"}, {taskPartialPrivate}}},
		{name: "partial source", rows: []Row{{"0"}}, err: errors.New("permission denied: " + taskPartialPrivate)},
	} {
		t.Logf("case=%s", test.name)
		reads := 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			reads++
			if !strings.Contains(query, taskCanaryAggregateFixture().queryMarker) {
				t.Fatal("initial unknown source continued collection")
			}
			return test.rows, test.err
		}}
		signal := NewTaskCanariesSignal()
		alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
		if err == nil || reads != 1 || len(alerts) != 1 {
			t.Fatal("initial partial observation acquired outage authority")
		}
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		if unknown.Target != signal.ID() || !unknown.unobservable || unknown.Severity != SeverityWarn {
			t.Fatal("initial failure changed its whole-signal unknown contract")
		}
		requireAlertOmits(t, unknown, taskPartialPrivate)
	}
	reads := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		reads++
		if strings.Contains(query, taskCanaryAggregateFixture().queryMarker) {
			return []Row{{"12"}}, nil
		}
		return nil, nil
	}}
	alerts, err := NewTaskCanariesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 || reads != 3 {
		t.Fatal("complete healthy canary and empty groups did not stay healthy")
	}
}

// Both registered consumers retain unknown coverage through their accept
// filters. Neither may report the other's concrete findings as its own.
func TestTaskDrainPartialObservationPreservesCompletedComponents(t *testing.T) {
	for _, test := range []struct {
		name      string
		component string
		lagRows   []Row
		lagErr    error
		wantDue   bool
	}{
		{name: "lag query", component: "task-due-lag", lagRows: []Row{{"181"}}, lagErr: errors.New("permission denied: " + taskPartialPrivate)},
		{name: "lag missing", component: "task-due-lag"},
		{name: "lag shape", component: "task-due-lag", lagRows: []Row{{"181"}, {taskPartialPrivate}}},
		{name: "lag value", component: "task-due-lag", lagRows: []Row{{taskPartialPrivate}}},
		{name: "missing target after due", component: "task-target-missing", lagRows: []Row{{"181"}}, wantDue: true},
		{name: "missing target after healthy lag", component: "task-target-missing", lagRows: []Row{{"0"}}},
	} {
		for _, newSignal := range []func() Signal{NewStuckLeasesSignal, NewTaskConvergenceSignal} {
			signal := newSignal()
			t.Logf("case=%s signal=%s", test.name, signal.Key())
			strandedReads, lagReads, targetReads := 0, 0, 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				switch {
				case strings.Contains(query, "claim_time <"):
					strandedReads++
					return []Row{{"SyntheticStrandedTask", "180", "120", "300"}}, nil
				case query == taskDueOwnershipQuery:
					return nil, nil
				case strings.Contains(query, "greatest(run_at"):
					lagReads++
					return test.lagRows, test.lagErr
				case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
					targetReads++
					return []Row{{"PartialMissingTarget", "101"}}, errors.New("permission denied: " + taskPartialPrivate)
				default:
					return nil, nil // Optional ownership and error batteries.
				}
			}}
			alerts, err := signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("later drain source failure discarded completed components")
			}
			errorClass := observationErrorClassInvalidResponse
			if test.lagErr != nil || test.component == "task-target-missing" {
				errorClass = observationErrorClassAccessDenied
			}
			requireTaskPartialUnknown(t, signal, alerts, test.component, errorClass)
			wantAlerts := 1
			if signal.Key() == "stuck-leases" {
				wantAlerts++
				stranded := requireAlertClass(t, alerts, "task-lease-stranded")
				if stranded.SignalID != "pg/task-lease-stranded" || stranded.Frame != "SyntheticStrandedTask" || stranded.Sustain != 2 || stranded.unobservable {
					t.Fatal("complete stranded observation changed identity or authority")
				}
			} else if test.wantDue {
				wantAlerts++
				due := requireAlertClass(t, alerts, "task-due-lag")
				if due.SignalID != "pg/task-due-lag" || due.Target != "pg-1" || due.Sustain != 2 || due.unobservable {
					t.Fatal("complete due-lag observation changed identity or authority")
				}
			}
			wantTargetReads := 0
			if test.component == "task-target-missing" {
				wantTargetReads = 1
			}
			if len(alerts) != wantAlerts || strandedReads != 1 || lagReads != 1 || targetReads != wantTargetReads {
				t.Fatal("shared drain filter admitted another signal's finding or retried a source")
			}
		}
	}
}

// An initial failed drain read cannot prove a stranded lease, and a complete
// empty drain observation is healthy for both consumers.
func TestTaskDrainPartialObservationInitialAuthorityAndHealthyControl(t *testing.T) {
	for _, newSignal := range []func() Signal{NewStuckLeasesSignal, NewTaskConvergenceSignal} {
		signal := newSignal()
		reads := 0
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			reads++
			return []Row{{"PartialStrandedTask", "180", "120", "300"}}, errors.New("permission denied: " + taskPartialPrivate)
		}}
		alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
		if err == nil || reads != 1 || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
			t.Fatal("partial initial drain result acquired stranded authority")
		}
		reads = 0
		source.postgresFn = func(query string) ([]Row, error) {
			reads++
			if strings.Contains(query, "greatest(run_at") {
				return []Row{{"0"}}, nil
			}
			return nil, nil
		}
		alerts, err = signal.Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 0 || reads != 3 {
			t.Fatal("complete healthy drain did not remain healthy")
		}
	}
}

// State inspection occurs after Handle has returned, so pending delivery
// cannot make a missing clear or retained identity appear healthy by timing.
func requireTaskPartialIncident(t *testing.T, notifier *SlackAlertNotifier, id, class, target string, present bool) {
	t.Helper()
	notifier.stateLock.Lock()
	_, exists := notifier.keyIncidents[id+"|"+class+"|"+target]
	notifier.stateLock.Unlock()
	if exists != present {
		t.Fatal("task incident cleared during unknown coverage or failed to clear after complete recovery")
	}
}

// The first partial run must raise the established page. Later positive canary
// counts cannot clear it while another required component stays unobserved.
func TestTaskCanariesPartialObservationCadenceAndNotifier(t *testing.T) {
	var run atomic.Int32
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
			if run.Load() < 2 {
				return []Row{{"0"}}, nil
			}
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			if run.Load() < 4 {
				return nil, errors.New("permission denied: " + taskPartialPrivate)
			}
		}
		return nil, nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	signal := NewTaskCanariesSignal()
	postSignalRunsThroughTheGate(t, signal, syntheticSettings(source), 4, func(i int) { run.Store(int32(i)) }, notifier)
	requireTaskPartialIncident(t, notifier, "pg/canary-dead", "canary-dead", "pg-1", true)
	requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-overdue", true)
	postSignalRunsThroughTheGate(t, signal, syntheticSettings(source), 2, func(i int) { run.Store(int32(i + 4)) }, notifier)
	requireTaskPartialIncident(t, notifier, "pg/canary-dead", "canary-dead", "pg-1", false)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if countSlackPosts(texts, "[PAGE] raised", "pg/canary-dead · canary-dead · pg-1") != 1 ||
		countSlackPosts(texts, "[CLEARED]", "pg/canary-dead · canary-dead · pg-1") != 1 ||
		countSlackPosts(texts, "[WARN] raised", "monitor/visibility · cannot-observe · pg-1/task-overdue") != 1 {
		t.Fatal("partial task observation changed PAGE delivery, visibility sustain or recovery")
	}
}

// Both adapters share one source-unknown identity. A complete sibling run may
// clear only its ownership; an unknown owner keeps its own incident alive.
func TestTaskDrainPartialObservationSharedNotifierOwnership(t *testing.T) {
	var phase atomic.Int32
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "claim_time <"):
			if phase.Load() == 0 {
				return []Row{{"SyntheticStrandedTask", "180", "120", "300"}}, nil
			}
		case query == taskDueOwnershipQuery:
			return nil, nil
		case strings.Contains(query, "greatest(run_at"):
			if phase.Load() == 0 {
				return []Row{{"181"}}, nil
			}
			return []Row{{"0"}}, nil
		case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
			if phase.Load() < 2 {
				return nil, errors.New("permission denied: " + taskPartialPrivate)
			}
		}
		return nil, nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	settings := syntheticSettings(source)
	stuck, convergence := NewStuckLeasesSignal(), NewTaskConvergenceSignal()
	for _, signal := range []Signal{stuck, convergence} {
		postSignalRunsThroughTheGate(t, signal, settings, 2, func(int) { phase.Store(0) }, notifier)
	}
	for _, signal := range []Signal{stuck, convergence} {
		postSignalRunsThroughTheGate(t, signal, settings, 2, func(int) { phase.Store(1) }, notifier)
	}
	requireTaskPartialIncident(t, notifier, "pg/task-lease-stranded", "task-lease-stranded", "pg-1", true)
	requireTaskPartialIncident(t, notifier, "pg/task-due-lag", "task-due-lag", "pg-1", true)
	postSignalRunsThroughTheGate(t, convergence, settings, 2, func(int) { phase.Store(2) }, notifier)
	requireTaskPartialIncident(t, notifier, "pg/task-due-lag", "task-due-lag", "pg-1", false)
	requireTaskPartialIncident(t, notifier, "pg/task-lease-stranded", "task-lease-stranded", "pg-1", true)
	requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-target-missing", true)
	postSignalRunsThroughTheGate(t, stuck, settings, 2, func(int) { phase.Store(2) }, notifier)
	requireTaskPartialIncident(t, notifier, "pg/task-lease-stranded", "task-lease-stranded", "pg-1", false)
	requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-target-missing", false)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if countSlackPosts(texts, "[WARN] raised", "monitor/visibility · cannot-observe · pg-1/task-target-missing") != 1 ||
		countSlackPosts(texts, "[CLEARED]", "monitor/visibility · cannot-observe · pg-1/task-target-missing") != 1 {
		t.Fatal("shared source-unknown identity was duplicated or cleared by the wrong owner")
	}
}

// A successful command with a malformed later scalar is the parser variant
// of the same loss: it cannot erase a complete prior stranded observation.
func TestTaskDrainPartialObservationParserPreservesStranded(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "claim_time <") {
			return []Row{{"SyntheticStrandedTask", "180", "120", "300"}}, nil
		}
		if strings.Contains(query, "greatest(run_at") {
			return []Row{{taskPartialPrivate}}, nil
		}
		t.Fatal("parser failure continued to an unvisited component")
		return nil, nil
	}}
	signal := NewStuckLeasesSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 2 {
		t.Fatal("later parser failure discarded the independent stranded observation")
	}
	requireTaskPartialUnknown(t, signal, alerts, "task-due-lag", observationErrorClassInvalidResponse)
	requireAlertClass(t, alerts, "task-lease-stranded")
}

// Ended parents retain source causes at the adapter boundary. Scoped runs
// keep the parent verdict; unscoped runs keep the original source error.
func requireTaskCanceledObservation(t *testing.T, signal Signal, settings SignalSettings, scoped bool, alerts Alerts, err, sourceErr error, sourceText string) {
	t.Helper()
	if err == nil || len(alerts) != 0 {
		t.Fatal("canceled task observation returned successful partial findings")
	}
	returnedSource := err
	if scoped {
		var joined *signalContextError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &joined) || err.Error() != context.Canceled.Error() {
			t.Fatal("canceled task observation lost the adapter's parent verdict or source cause")
		}
		returnedSource = joined.sourceErr
	}
	if sourceErr != nil && (returnedSource != sourceErr || !errors.Is(err, sourceErr)) {
		t.Fatal("canceled task observation discarded or replaced the original source error")
	}
	if sourceText != "" && (returnedSource == nil || returnedSource.Error() != sourceText) {
		t.Fatal("canceled task observation discarded the original parser error")
	}
	unknown := visibilityAlert(settings, signal, err)
	requireAlertOmits(t, unknown, taskPartialPrivate, "synthetic-secondary-task-cause")
}

// Each later canary source cancels its owner synchronously before returning its
// cause. No timer, scheduler race or elapsed delay supplies the cancellation.
func TestTaskCanariesPartialObservationCanceledOwnerPreservesCauses(t *testing.T) {
	for _, component := range []string{"task-overdue", "task-maintenance", "task-failures"} {
		for _, scoped := range []bool{false, true} {
			t.Logf("component=%s scoped=%t", component, scoped)
			ctx, cancel := context.WithCancel(context.Background())
			sourceCause := errors.New(taskPartialPrivate)
			sourceErr := errors.Join(sourceCause, errors.New("synthetic-secondary-task-cause"))
			failedReads, readsAfterEnd, failureReads := 0, 0, 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if ctx.Err() != nil {
					readsAfterEnd++
					return nil, sourceErr
				}
				failSource := func() ([]Row, error) {
					failedReads++
					cancel()
					return nil, sourceErr
				}
				switch {
				case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
					return []Row{{"0"}}, nil
				case strings.Contains(query, "WITH completion_minutes AS"):
					return nil, nil
				case strings.Contains(query, "WITH history AS"):
					if component == "task-overdue" {
						return failSource()
					}
					if component == "task-maintenance" {
						return []Row{{"DbMaintenance", "9000", "1800", "f", "86400", "0", ""}}, nil
					}
					return nil, nil
				case strings.Contains(query, "pg_stat_progress_create_index"):
					return failSource()
				case strings.Contains(query, "WITH failures AS"):
					failureReads++
					if component == "task-failures" && failureReads == 2 {
						return failSource()
					}
					return nil, nil
				default:
					t.Fatal("unexpected task query before cancellation")
					return nil, nil
				}
			}}
			settings := syntheticSettings(source)
			if scoped {
				settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
			}
			signal := NewTaskCanariesSignal()
			alerts, err := signal.Run(ctx, settings)
			cancel()
			if failedReads != 1 || readsAfterEnd != 0 || !errors.Is(err, sourceCause) {
				t.Fatal("canceled canary repeated source work or lost its nested cause")
			}
			requireTaskCanceledObservation(t, signal, settings, scoped, alerts, err, sourceErr, "")
		}
	}
}

// Both shared-drain adapters retain source and parser causes for all four
// later exits, without admitting findings or reading another component.
func TestTaskDrainPartialObservationCanceledOwnerPreservesCauses(t *testing.T) {
	for _, test := range []struct {
		name       string
		component  string
		lagRows    []Row
		parserText string
	}{
		{name: "lag query", component: "task-due-lag"},
		{name: "lag shape", component: "task-due-lag", parserText: "invalid response for task due-lag observation: expected one integer cell"},
		{name: "lag value", component: "task-due-lag", lagRows: []Row{{taskPartialPrivate}}, parserText: "invalid response for task due-lag observation: expected a non-negative integer"},
		{name: "missing target", component: "task-target-missing", lagRows: []Row{{"181"}}},
	} {
		for _, newSignal := range []func() Signal{NewStuckLeasesSignal, NewTaskConvergenceSignal} {
			for _, scoped := range []bool{false, true} {
				signal := newSignal()
				t.Logf("case=%s signal=%s scoped=%t", test.name, signal.Key(), scoped)
				ctx, cancel := context.WithCancel(context.Background())
				sourceCause := errors.New(taskPartialPrivate)
				var sourceErr error
				if test.parserText == "" {
					sourceErr = errors.Join(sourceCause, errors.New("synthetic-secondary-task-cause"))
				}
				failedReads, readsAfterEnd := 0, 0
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if ctx.Err() != nil {
						readsAfterEnd++
						return nil, sourceErr
					}
					switch {
					case strings.Contains(query, "claim_time <"):
						return []Row{{"SyntheticStrandedTask", "180", "120", "300"}}, nil
					case query == taskDueOwnershipQuery:
						return nil, nil
					case strings.Contains(query, "greatest(run_at"):
						if test.component == "task-due-lag" {
							failedReads++
							cancel()
							return test.lagRows, sourceErr
						}
						return test.lagRows, nil
					case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
						failedReads++
						cancel()
						return nil, sourceErr
					case strings.Contains(query, "WITH failures AS"):
						return nil, nil
					default:
						t.Fatal("unexpected drain query before cancellation")
						return nil, nil
					}
				}}
				settings := syntheticSettings(source)
				if scoped {
					settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
				}
				alerts, err := signal.Run(ctx, settings)
				cancel()
				if failedReads != 1 || readsAfterEnd != 0 || (sourceErr != nil && !errors.Is(err, sourceCause)) {
					t.Fatal("canceled drain repeated source work or lost its nested cause")
				}
				requireTaskCanceledObservation(t, signal, settings, scoped, alerts, err, sourceErr, test.parserText)
			}
		}
	}
}

// A source's own interrupted sub-operation does not end a still-live parent.
// It remains component unknown alongside the previously completed finding.
func TestTaskPartialObservationLiveParentKeepsSourceInterruptionsPartial(t *testing.T) {
	for _, sourceErr := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, newSignal := range []func() Signal{NewTaskCanariesSignal, NewStuckLeasesSignal, NewTaskConvergenceSignal} {
			for _, scoped := range []bool{false, true} {
				signal := newSignal()
				component := "task-due-lag"
				if signal.Key() == "task-canaries" {
					component = "task-overdue"
				}
				failed := false
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if failed {
						t.Fatal("source interruption continued collection")
					}
					switch {
					case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
						return []Row{{"0"}}, nil
					case strings.Contains(query, "claim_time <"):
						return []Row{{"SyntheticStrandedTask", "180", "120", "300"}}, nil
					case strings.Contains(query, "WITH history AS"), strings.Contains(query, "greatest(run_at"):
						failed = true
						return nil, sourceErr
					default:
						return nil, nil
					}
				}}
				ctx := context.Background()
				settings := syntheticSettings(source)
				if scoped {
					settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
				}
				alerts, err := signal.Run(ctx, settings)
				if err != nil || ctx.Err() != nil || !failed {
					t.Fatal("live parent lost independent findings after a source interruption")
				}
				requireTaskPartialUnknown(t, signal, alerts, component, classifyObservationError(sourceErr))
				wantAlerts := 2
				if signal.Key() == "task-convergence" {
					wantAlerts = 1
				} else if signal.Key() == "task-canaries" {
					requireAlertClass(t, alerts, "canary-dead")
				} else {
					requireAlertClass(t, alerts, "task-lease-stranded")
				}
				if len(alerts) != wantAlerts {
					t.Fatal("live-parent source interruption changed shared accept-filter ownership")
				}
			}
		}
	}
}
