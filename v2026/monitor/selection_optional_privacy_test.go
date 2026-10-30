package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const selectionOptionalPrivateMarker = "synthetic-private-journal-tag-canary"
const selectionOptionalPrivateTaskID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func selectionOptionalPrivacySettings(gap, journal string, gatewayErr, journalErr error) SignalSettings {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return []Row{{gap}}, nil },
		localFn:    func(string, ...string) (string, error) { return "", gatewayErr },
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			return journal, journalErr
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "worker.example", Roles: []string{"services"}})
	return settings
}

func selectionOptionalJournal(malformed, active bool) string {
	rows := []string{}
	if malformed {
		rows = append(rows, `{"SYSLOG_TIMESTAMP":"2026-08-29T12:00:00Z","MESSAGE":"synthetic row","CONTAINER_TAG":"`+selectionOptionalPrivateMarker+`"}`)
	}
	if active {
		rows = append(rows, `{"SYSLOG_TIMESTAMP":"2026-08-29T12:00:00Z","MESSAGE":"[`+selectionOptionalPrivateTaskID+`]eval active(1458.47s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})","CONTAINER_TAG":"warp|synthetic|taskworker|g1","CONTAINER_ID":"syntheticworker","_HOSTNAME":"worker.example"}`)
	}
	return strings.Join(rows, "\n")
}

// Check every public field, both document writers and the normal returned error.
// Failure output deliberately never prints the offending payload.
func requireSelectionOptionalPublicPrivacy(t *testing.T, alerts Alerts, err error) {
	t.Helper()
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil {
		t.Fatal("could not encode public alerts")
	}
	var jsonl strings.Builder
	if writeErr := alerts.WriteJSONL(&jsonl); writeErr != nil {
		t.Fatal("could not write public JSONL")
	}
	values := []string{string(encoded), alerts.Markdown(), jsonl.String()}
	if err != nil {
		values = append(values, err.Error())
	}
	for _, alert := range alerts {
		values = append(values, fmt.Sprintf("%+v", alert), alert.Markdown(), alert.String())
	}
	for _, value := range values {
		for _, marker := range []string{selectionOptionalPrivateMarker, selectionOptionalPrivateTaskID} {
			if strings.Contains(value, marker) {
				t.Fatal("optional lifecycle private canary reached public output")
			}
		}
	}
}

// The malformed journal tag reaches the real normalizer and the public
// constructor. On the old reducer this exports the tag through Evidence.
func TestSelectionOptionalLifecycleMalformedJournalPrivacy(t *testing.T) {
	settings := selectionOptionalPrivacySettings("6000", selectionOptionalJournal(true, false), errors.New("synthetic gateway unavailable"), nil)
	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("optional lifecycle failure became a required-source error")
	}
	requireSelectionOptionalPublicPrivacy(t, alerts, err)
	stale := requireAlertClass(t, alerts, "selection-stale")
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if len(alerts) != 2 || stale.Severity != SeverityWarn || stale.Sustain != 1 ||
		!strings.Contains(stale.Observed, "completion_gap_s=6000 ") ||
		!strings.Contains(stale.Observed, "active_log_error_class=observation-unclassified") ||
		strings.Contains(stale.Observed, "active_duration_s=") || visibility.Frame != "selection-lifecycle" || !visibility.unobservable {
		t.Fatal("optional failure lost stale completion or independent visibility")
	}
}

func TestSelectionOptionalLifecyclePartialHeartbeatPrivacy(t *testing.T) {
	settings := selectionOptionalPrivacySettings("10801", selectionOptionalJournal(true, true), errors.New("synthetic gateway unavailable"), nil)
	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatal("partial heartbeat lost stale completion or incomplete-source visibility")
	}
	requireSelectionOptionalPublicPrivacy(t, alerts, err)
	stale := requireAlertClass(t, alerts, "selection-stale")
	requireAlertClass(t, alerts, "cannot-observe")
	if stale.Severity != SeverityPage || !strings.Contains(stale.Observed, "active_duration_s=1458") ||
		!strings.Contains(stale.Observed, "active_attempt_correlated=true") ||
		!strings.Contains(stale.Mechanism, "actively rebuilding full-fleet export") {
		t.Fatal("partial journal discarded valid heartbeat authority")
	}
}

// Clean empty and heartbeat fallback controls must not become false unknowns.
func TestSelectionOptionalLifecycleCleanControls(t *testing.T) {
	for _, active := range []bool{false, true} {
		settings := selectionOptionalPrivacySettings("6000", selectionOptionalJournal(false, active), errors.New("synthetic gateway unavailable"), nil)
		alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("clean optional fallback gained false visibility failure")
		}
		requireSelectionOptionalPublicPrivacy(t, alerts, err)
		stale := requireAlertClass(t, alerts, "selection-stale")
		if strings.Contains(stale.Observed, "active_duration_s=") != active || strings.Contains(stale.Observed, "active_log_error_class=") {
			t.Fatal("clean fallback activity classification changed")
		}
	}
}

func TestSelectionOptionalLifecycleRawTransportFailurePrivacy(t *testing.T) {
	settings := selectionOptionalPrivacySettings("-1", selectionOptionalPrivateMarker, errors.New(selectionOptionalPrivateMarker), errors.New(selectionOptionalPrivateMarker))
	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatal("raw transport failure replaced the no-completion sentinel")
	}
	requireSelectionOptionalPublicPrivacy(t, alerts, err)
	stale := requireAlertClass(t, alerts, "selection-stale")
	if stale.Severity != SeverityPage || !strings.Contains(stale.Observed, "active_log_source=unavailable") {
		t.Fatal("unavailable lifecycle changed the required sentinel meaning")
	}
}

func TestSelectionOptionalLifecyclePartialFleetErrorClasses(t *testing.T) {
	for _, control := range []struct{ sourceText, class string }{
		{sourceText: "permission denied ", class: observationErrorClassAccessDenied},
		{sourceText: "invalid response ", class: observationErrorClassInvalidResponse},
		{sourceText: "timeout ", class: observationErrorClassTimeout},
	} {
		settings := selectionOptionalPrivacySettings("6000", "", errors.New("synthetic gateway unavailable"), nil)
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "missing-worker.example", Roles: []string{"services"}})
		journal := selectionOptionalJournal(false, true)
		settings.Source.(*syntheticSource).hostTimeoutFn = func(host HostSettings, _ string, _ time.Duration) (string, error) {
			if host.Name == "missing-worker.example" {
				return selectionOptionalPrivateMarker, errors.New(control.sourceText + selectionOptionalPrivateMarker)
			}
			return journal, nil
		}
		alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatal("partial fleet error replaced independent product evidence")
		}
		requireSelectionOptionalPublicPrivacy(t, alerts, err)
		stale := requireAlertClass(t, alerts, "selection-stale")
		visibility := requireAlertClass(t, alerts, "cannot-observe")
		if !strings.Contains(stale.Observed, "active_log_error_class="+control.class) ||
			!strings.Contains(stale.Observed, "active_duration_s=1458") ||
			!strings.Contains(visibility.Observed, "error_class="+control.class) {
			t.Fatal("optional source fixed class or surviving heartbeat changed")
		}
	}
}

func TestSelectionOptionalLifecycleHealthyAgeSkipsOptionalRead(t *testing.T) {
	reads := 0
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return []Row{{"0"}}, nil },
		localFn: func(string, ...string) (string, error) {
			reads++
			return "", errors.New(selectionOptionalPrivateMarker)
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			reads++
			return "", errors.New(selectionOptionalPrivateMarker)
		},
	}
	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 || reads != 0 {
		t.Fatal("healthy required age acquired an optional visibility dependency")
	}
	requireSelectionOptionalPublicPrivacy(t, alerts, err)
}

func TestSelectionOptionalLifecycleUnknownRequiredAgeSkipsOptionalRead(t *testing.T) {
	reads := 0
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return []Row{{selectionOptionalPrivateMarker}}, nil },
		localFn:    func(string, ...string) (string, error) { reads++; return "", nil },
	}
	alerts, err := NewWithSignals(syntheticSettings(source), NewSelectionFreshnessSignal()).Run(context.Background())
	if err == nil || reads != 0 || len(alerts) != 1 {
		t.Fatal("unknown required age became a product judgment or optional lookup")
	}
	requireSelectionOptionalPublicPrivacy(t, alerts, err)
	requireAlertClass(t, alerts, "cannot-observe")
}

// A normal optional failure remains available privately at the reader boundary;
// it is intentionally reduced to findings rather than returned as a run error.
func TestSelectionOptionalLifecyclePrivateReaderCauseRetained(t *testing.T) {
	settings := selectionOptionalPrivacySettings("6000", selectionOptionalJournal(true, false), errors.New("synthetic gateway unavailable"), nil)
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal("could not initialize synthetic probe")
	}
	_, _, sourceErr := readTaskLifecycleLog(context.Background(), env, "UpdateClientScores", 5*time.Minute, 1000)
	if sourceErr == nil || !strings.Contains(sourceErr.Error(), selectionOptionalPrivateMarker) {
		t.Fatal("private normalizer error lost its diagnostic cause")
	}
}

// An interrupted parent uses the existing private error chain. Its Error text
// stays safe, while the private wrapper retains the returned aggregate cause.
func TestSelectionOptionalLifecycleInterruptedPrivateCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	privateErr := errors.New(selectionOptionalPrivateMarker)
	settings := selectionOptionalPrivacySettings("6000", "", errors.New("synthetic gateway unavailable"), privateErr)
	source := settings.Source.(*syntheticSource)
	source.hostTimeoutFn = func(HostSettings, string, time.Duration) (string, error) { cancel(); return "", privateErr }
	alerts, err := NewSelectionFreshnessSignal().Run(ctx, settings)
	if len(alerts) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("interrupted optional observation became completed findings")
	}
	// task_logs aggregates host causes into private text, so the shared reader
	// does not retain journal error identity; the adapter retains that aggregate.
	var interrupted *signalContextError
	if !errors.As(err, &interrupted) || interrupted.sourceErr == nil || !strings.Contains(interrupted.sourceErr.Error(), selectionOptionalPrivateMarker) {
		t.Fatal("interrupted adapter lost its private aggregate source cause")
	}
	requireSelectionOptionalPublicPrivacy(t, alerts, err)
}

func TestSelectionOptionalLifecycleNotifierRetainsUntilCompleteRecovery(t *testing.T) {
	var run atomic.Int32
	journal := selectionOptionalJournal(true, true)
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			switch run.Load() {
			case 3:
				return []Row{{selectionOptionalPrivateMarker}}, nil
			case 5, 6:
				return []Row{{"0"}}, nil
			default:
				return []Row{{"10801"}}, nil
			}
		},
		localFn: func(string, ...string) (string, error) {
			if run.Load() == 1 || run.Load() == 2 {
				return "", errors.New("synthetic gateway unavailable")
			}
			return "", nil
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { return journal, nil },
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "worker.example", Roles: []string{"services"}})
	webhook, webhookURL := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookURL))
	postSignalRunsThroughTheGate(t, NewSelectionFreshnessSignal(), settings, 7, func(i int) { run.Store(int32(i)) }, notifier)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	identity := "pg/selection-stale · selection-stale · pg-1"
	if countSlackPosts(texts, "[PAGE] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
		t.Fatal("freshness notifier lost incident continuity or complete recovery")
	}
	for _, text := range texts {
		if strings.Contains(text, selectionOptionalPrivateMarker) || strings.Contains(text, selectionOptionalPrivateTaskID) {
			t.Fatal("optional source private canary reached notifier")
		}
	}
}
