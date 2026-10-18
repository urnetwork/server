// SIGNALS.md §2.9b/§2.9c retain finite source failures without changing metrics authority.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The real adapters must preserve typed admission witnesses even when a failed
// source returns a valid healthy payload. No private error text becomes evidence.
func TestProviderMetricSourceFailuresRetainFiniteAuthority(t *testing.T) {
	for _, selection := range []bool{false, true} {
		signal := NewProviderPickerSignal()
		class := "provider-picker-unobservable"
		payload := pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/nonempty": 20}))
		settings := pickerTestSettings(t, payload)
		if selection {
			signal = NewProviderSelectionSignal()
			class = "provider-selection-unavailable"
			payload = pickerTestPayload(t, selectionTestRows("returned", 20))
			settings = selectionTestSettings(t, payload)
		}
		for _, testCase := range []struct {
			name       string
			err        error
			errorClass string
			phases     string
		}{
			{name: "unknown source", err: errors.New("private-source-canary"), errorClass: observationErrorClassUnclassified},
			{name: "acquire deadline", err: &sshAdmissionUnavailableError{err: fmt.Errorf("private-source-canary: %w", context.DeadlineExceeded), phase: sshAdmissionFailureAcquire}, errorClass: observationErrorClassTimeout, phases: "acquire"},
			{name: "release deadline", err: &sshAdmissionUnavailableError{err: fmt.Errorf("private-source-canary: %w", context.DeadlineExceeded), phase: sshAdmissionFailureRelease}, errorClass: observationErrorClassTimeout, phases: "release"},
			{name: "mixed admission", err: errors.Join(&sshAdmissionUnavailableError{err: errors.New("private-source-canary"), phase: sshAdmissionFailureAcquire}, &sshAdmissionUnavailableError{err: errors.New("private-source-canary"), phase: sshAdmissionFailureRelease}), errorClass: observationErrorClassAdmissionUnavailable, phases: "acquire,release"},
			{name: "legacy admission", err: &sshAdmissionUnavailableError{err: errors.New("private-source-canary")}, errorClass: observationErrorClassAdmissionUnavailable, phases: "unknown"},
			{name: "plain phase text", err: errors.New("SSH admission acquire private-source-canary"), errorClass: observationErrorClassUnclassified},
		} {
			calls := 0
			settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
				calls++
				providerHttpsTestRequest(t, request, selection)
				return providerHttpsTestResponse(payload), testCase.err
			}}
			var observation *ProviderSelectionObservation
			settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error {
				observation = &value
				return nil
			}
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || calls != 1 || len(alerts) != 1 {
				t.Fatalf("%s/%s changed the source-failure boundary", signal.Key(), testCase.name)
			}
			alert := requireAlertClass(t, alerts, class)
			want := "error_class=" + testCase.errorClass
			if testCase.phases != "" {
				want += " ssh_admission_failures=" + testCase.phases
			}
			if !strings.Contains(alert.Observed, want+" mimir_query_phase=http") {
				t.Errorf("%s/%s lost finite source authority: got %q, want suffix %q", signal.Key(), testCase.name, alert.Observed, want)
			}
			if !strings.Contains(alert.Observed, "reason=bounded-source-unavailable") || !strings.Contains(alert.Observed, "paired_processes=0") || alert.Identity() != signal.ID()+"|"+class+"|api-fleet|" || alert.Severity != SeverityWarn || alert.Sustain != 1 || !isVisibilityAlert(alert) {
				t.Errorf("%s/%s changed the retained unknown identity or source counts", signal.Key(), testCase.name)
			}
			if testCase.phases == "" && strings.Contains(alert.Observed, "ssh_admission_failures=") {
				t.Error("untyped phase text invented admission authority")
			}
			requireAlertOmits(t, alert, "private-source-canary", "private-selection-instance", "private-synthetic-instance", "api-synthetic", "ssh_spawn=", "http_status=")
			if selection && (observation == nil || observation.Reason != "bounded-source-unavailable" || observation.SourceCoverageComplete || observation.ShapeCoverageComplete || len(observation.Outcomes) != 0) {
				t.Fatal("finite source error manufactured selection counts or completeness")
			}
		}
	}
}

// One native response keeps the exact query and its healthy or failing verdict
// independently of every SSH gateway. The pilot deliberately has no failover.
func TestProviderMetricSourceFailuresPreserveNativeResponseControls(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, failure := range []bool{false, true} {
			signal := NewProviderPickerSignal()
			pageClass := "provider-picker-effective-empty"
			rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
			if failure {
				rows = pickerTestRows(map[string]float64{"initial/empty": 20})
			}
			settings := pickerTestSettings(t, "")
			if selection {
				signal = NewProviderSelectionSignal()
				pageClass = "provider-selection-empty-despite-eligible"
				reason := "returned"
				if failure {
					reason = "eligible_not_selected"
				}
				rows = selectionTestRows(reason, 3)
				settings = selectionTestSettings(t, "")
			}
			payload := pickerTestPayload(t, rows)
			settings.Hosts = []HostSettings{{Name: "gateway-first", Roles: []string{"services"}}, {Name: "gateway-second", Roles: []string{"services"}}, {Name: "api-synthetic", Roles: []string{"api"}}}
			calls, sshCalls := 0, 0
			settings.Source = &providerHttpsTestSource{
				syntheticSource: &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
					sshCalls++
					return "", &sshAdmissionUnavailableError{err: errors.New("private-source-canary"), phase: sshAdmissionFailureAcquire}
				}},
				do: func(request *http.Request) (*http.Response, error) {
					calls++
					providerHttpsTestRequest(t, request, selection)
					return providerHttpsTestResponse(payload), nil
				},
			}
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || calls != 1 || sshCalls != 0 {
				t.Fatalf("%s did not preserve the single native request boundary", signal.Key())
			}
			if failure {
				if len(alerts) != 1 || requireAlertClass(t, alerts, pageClass).Severity != SeverityPage {
					t.Fatal("finite diagnostics suppressed the true provider failure")
				}
			} else if len(alerts) != 0 {
				t.Fatal("a healthy source inherited a prior gateway failure")
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "error_class=", "ssh_admission_failures=", "private-source-canary")
			}
		}
	}
}

// A returned envelope has different authority from a failed source command.
// Empty and malformed payloads stay unknown without invented transport fields.
func TestProviderMetricSourceFailuresKeepDataAbsenceSeparate(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, testCase := range []struct{ payload, reason string }{
			{payload: pickerTestPayload(t, nil), reason: "incomplete-process-window"},
			{payload: "private-source-canary", reason: "invalid-source-response"},
		} {
			signal := NewProviderPickerSignal()
			class := "provider-picker-unobservable"
			settings := pickerTestSettings(t, testCase.payload)
			if selection {
				signal, class = NewProviderSelectionSignal(), "provider-selection-unavailable"
				settings = selectionTestSettings(t, testCase.payload)
			}
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 {
				t.Fatal("returned source response lost its visibility finding")
			}
			alert := requireAlertClass(t, alerts, class)
			if !strings.Contains(alert.Observed, "reason="+testCase.reason) || !isVisibilityAlert(alert) {
				t.Fatal("data absence or invalid data became source-command failure or health")
			}
			requireAlertOmits(t, alert, "error_class=", "ssh_admission_failures=", "private-source-canary")
		}
	}
}

// A real prior page crosses the cadence gate and notifier, then survives both
// the enriched source failure and a returned partial healthy process subset.
func TestProviderMetricSourceFailuresRetainPriorPage(t *testing.T) {
	for _, selection := range []bool{false, true} {
		signal := NewProviderPickerSignal()
		pageClass := "provider-picker-effective-empty"
		if selection {
			signal, pageClass = NewProviderSelectionSignal(), "provider-selection-empty-despite-eligible"
		}
		settings := providerInventoryTestSettings()
		slots := providerInventoryTestSlots(4)
		failure := pickerTestPayload(t, providerInventoryTestRows(selection, true, slots))
		partial := pickerTestPayload(t, providerInventoryTestRows(selection, false, slots[:10]))
		var run atomic.Int32
		settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(*http.Request) (*http.Response, error) {
			switch run.Load() {
			case 2:
				return nil, &mimirQueryError{phase: "http-status", status: http.StatusForbidden}
			case 3:
				return providerHttpsTestResponse(partial), nil
			default:
				return providerHttpsTestResponse(failure), nil
			}
		}}
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		postSignalRunsThroughTheGate(t, signal, settings, 6, func(i int) { run.Store(int32(i)) }, notifier)
		closeSlackNotifier(t, notifier)
		identity := signal.ID() + " · " + pageClass + " · api-fleet"
		texts := webhook.posted()
		if countSlackPosts(texts, "[PAGE] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 0 {
			t.Fatalf("%s source failure or partial coverage cleared or re-raised the prior page", signal.Key())
		}
		for _, text := range texts {
			if strings.Contains(text, "private-source-canary") {
				t.Fatal("private source error reached the notifier")
			}
		}
	}
}
