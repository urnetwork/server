// Provider-count parser failures expose only code-owned error messages.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Both public boundaries must stay unavailable without returning response text.
// The callback count proves each invocation uses its existing single query.
func requireProviderCountParserErrorPrivacy(t *testing.T, payload, wantError, wantClass string) {
	t.Helper()
	const privateMarker = "synthetic-private-provider-error"
	now := time.Date(2036, 1, 2, 3, 4, 0, 0, time.UTC)
	settings := providerCountSyntheticSettings(t, now, payload)
	original := settings.Source
	var calls atomic.Int64
	settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls.Add(1)
		return original.Host(t.Context(), host, command)
	}}
	signal := NewProviderCountSignal()
	alerts, err := signal.Run(t.Context(), settings)
	if t.Context().Err() != nil || err == nil || len(alerts) != 0 || calls.Load() != 1 {
		t.Fatal("parser fixture did not reach the live public error boundary once")
	}
	if strings.Contains(err.Error(), privateMarker) {
		t.Fatal("provider-count public error exposed backend response text")
	}
	if err.Error() != wantError || classifyObservationError(err) != wantClass {
		t.Fatal("parser failure changed its fixed message or observation class")
	}
	alerts, err = NewWithSignals(settings, signal).Run(t.Context())
	if err == nil || calls.Load() != 2 {
		t.Fatal("monitor parser failure lost error authority or retried the query")
	}
	if strings.Contains(err.Error(), privateMarker) {
		t.Fatal("aggregate public error exposed backend response text")
	}
	requireAggregateVisibility(t, signal, alerts, wantClass)
	for _, alert := range alerts {
		requireAlertOmits(t, alert, privateMarker, payload)
	}
}

// Failed envelope strings cannot escape either public error boundary.
func TestProviderCountFailedEnvelopePublicErrorPrivacy(t *testing.T) {
	const privateMarker = "synthetic-private-provider-error"
	now := time.Date(2036, 1, 2, 3, 4, 0, 0, time.UTC)
	clean := providerCountFixture(t, now, map[string]float64{"3-9": 30}, "false")
	for _, fields := range []map[string]any{
		{"status": "error", "error": privateMarker + " timeout forbidden"},
		{"status": privateMarker},
		{"data": map[string]any{"resultType": privateMarker, "result": []any{}}},
		{"status": "error", "error": privateMarker, "warnings": []string{privateMarker}, "infos": []string{privateMarker}},
	} {
		payload := providerCountAnnotatedFixture(t, clean, fields)
		requireProviderCountParserErrorPrivacy(t, payload,
			"provider count: Mimir response status or result type is unsupported", observationErrorClassUnclassified)
	}
}

// Decoder diagnostics remain fixed for malformed or mistyped responses.
func TestProviderCountDecodePublicErrorPrivacy(t *testing.T) {
	for _, payload := range []string{
		"synthetic-private-provider-error",
		`{"status": "synthetic-private-provider-error`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"synthetic-private-provider-error":[]},"value":[1,"0"]}]}}`,
	} {
		requireProviderCountParserErrorPrivacy(t, payload,
			"provider count: decode Mimir response", observationErrorClassInvalidResponse)
	}
}

// Invalid numeric sample text stays private while response authority stays unknown.
func TestProviderCountInvalidValuePublicErrorPrivacy(t *testing.T) {
	now := time.Date(2036, 1, 2, 3, 4, 0, 0, time.UTC)
	var response mimirInstantResponse
	if json.Unmarshal([]byte(providerCountFixture(t, now, map[string]float64{"0": 20}, "false")), &response) != nil || len(response.Data.Result) != 1 {
		t.Fatal("invalid synthetic outcome-value fixture")
	}
	value, err := json.Marshal("synthetic-private-provider-error")
	if err != nil {
		t.Fatal("invalid synthetic outcome string")
	}
	response.Data.Result[0].Value[1] = value
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal("invalid synthetic outcome envelope")
	}
	requireProviderCountParserErrorPrivacy(t, string(payload),
		"provider count: parse Mimir outcome value", observationErrorClassInvalidResponse)
}

// Independent source causes keep their identity and lifecycle classification.
func TestProviderCountParserPrivacyPreservesSourceCauses(t *testing.T) {
	sourceFailure := errors.New("synthetic independent source failure")
	for _, testCase := range []struct {
		cause error
		class string
	}{
		{cause: sourceFailure, class: observationErrorClassUnclassified},
		{cause: context.Canceled, class: observationErrorClassCanceled},
		{cause: context.DeadlineExceeded, class: observationErrorClassTimeout},
	} {
		now := time.Date(2036, 1, 2, 3, 4, 0, 0, time.UTC)
		settings := providerCountSyntheticSettings(t, now, "unused")
		var calls atomic.Int64
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls.Add(1)
			return "synthetic-private-provider-error", testCase.cause
		}}
		alerts, err := NewProviderCountSignal().Run(t.Context(), settings)
		if t.Context().Err() != nil || !errors.Is(err, testCase.cause) || len(alerts) != 0 || calls.Load() != 1 {
			t.Fatal("parser privacy correction changed independent source cause or query count")
		}
		if classifyObservationError(err) != testCase.class || strings.Contains(err.Error(), "synthetic-private-provider-error") {
			t.Fatal("source failure was replaced by payload parsing or lost lifecycle class")
		}
	}
}

// A callback retires the owner before returning the failed envelope. The
// existing scoped adapter must keep parent precedence and the safe source cause.
func TestProviderCountParserPrivacyPreservesCanceledParent(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		now := time.Date(2036, 1, 2, 3, 4, 0, 0, time.UTC)
		ctx, cancel := context.WithCancel(t.Context())
		var owner context.Context = ctx
		cause := context.Canceled
		if deadline {
			owner = observationBoundaryDeadlineContext{Context: ctx}
			cause = context.DeadlineExceeded
		}
		settings := providerCountSyntheticSettings(t, now, "unused")
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "excluded-provider-control.example"})
		settings.ExcludedHosts = []string{"excluded-provider-control.example"}
		var calls atomic.Int64
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls.Add(1)
			cancel()
			return `{"status":"error","error":"synthetic-private-provider-error","data":{"resultType":"vector","result":[]}}`, nil
		}}
		alerts, err := NewProviderCountSignal().Run(owner, settings)
		cancel()
		var interrupted *signalContextError
		if !errors.Is(err, cause) || !errors.As(err, &interrupted) || len(alerts) != 0 || calls.Load() != 1 {
			t.Fatal("retired owner lost its lifecycle or independent parser failure")
		}
		if err.Error() != cause.Error() || interrupted.sourceErr.Error() != "provider count: Mimir response status or result type is unsupported" {
			t.Fatal("retired owner lost parent precedence or safe retained source cause")
		}
	}
}

// Clean response authority and the genuine empty-list PAGE remain unchanged.
func TestProviderCountParserPrivacyPreservesHealthyAndFailureControls(t *testing.T) {
	now := time.Date(2036, 1, 2, 3, 4, 0, 0, time.UTC)
	settings := providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"3-9": 30}, "false"))
	alerts, err := NewProviderCountSignal().Run(t.Context(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("clean independent healthy response changed")
	}
	settings = providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"0": 20}, "false"))
	alerts, err = NewProviderCountSignal().Run(t.Context(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatal("clean independent effective-empty failure changed")
	}
	page := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if page.Severity != SeverityPage || page.Sustain != 1 {
		t.Fatal("effective-empty PAGE identity or sustain changed")
	}
}
