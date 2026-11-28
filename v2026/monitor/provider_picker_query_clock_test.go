// Query evaluation belongs to the serialized POST, not the later render clock.
// All sources are memory-only HTTP callbacks with synthetic credentials.
package monitor

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Advancing the render clock cannot move the actual requested counter window.
func TestProviderPickerRequestedWindowClockUsesActualPost(t *testing.T) {
	probeAt := pickerTestNow().Add(875 * time.Millisecond)
	renderAt := probeAt.Add(37 * time.Second)
	clock := probeAt
	postedEvaluationUnix := int64(0)
	calls := 0
	payload := pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/empty": 20}))
	settings := pickerTestSettings(t, payload)
	settings.Now = func() time.Time { return clock }
	settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
		calls++
		providerHttpsTestRequest(t, request, false)
		var err error
		postedEvaluationUnix, err = strconv.ParseInt(request.PostForm.Get("time"), 10, 64)
		if err != nil {
			t.Fatal("owned request omitted its finite evaluation argument")
		}
		clock = renderAt
		return providerHttpsTestResponse(payload), nil
	}}
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 || len(alerts) != 1 {
		t.Fatal("clock metadata changed the source count or empty detection")
	}
	alert := requireAlertClass(t, alerts, "provider-picker-effective-empty")
	if !alert.ObservedAt.Equal(renderAt) || postedEvaluationUnix != probeAt.Unix() || postedEvaluationUnix == alert.ObservedAt.Unix() {
		t.Fatal("fake clocks did not separate POST evaluation and rendering")
	}
	expected := map[string]int64{"mimir_requested_eval_unix": postedEvaluationUnix, "mimir_requested_window_start_unix": postedEvaluationUnix - 300, "mimir_requested_window_end_unix": postedEvaluationUnix}
	for key, want := range expected {
		values := []string{}
		for _, field := range strings.Fields(alert.Observed) {
			name, value, ok := strings.Cut(field, "=")
			if ok && name == key {
				values = append(values, value)
			}
		}
		if len(values) != 1 || values[0] != strconv.FormatInt(want, 10) {
			t.Fatal("finding did not retain the exact serialized requested window")
		}
	}
	requireAlertOmits(t, alert, "private-synthetic-instance", "query-synthetic", "private-query-password", "Authorization", "Basic ")
}

// A refused source is not an accepted producer window, even if a request was
// constructed. Its existing phase/status and unknown-state contract remain.
func TestProviderPickerRequestedClockDoesNotQualifyFailedSource(t *testing.T) {
	settings := pickerTestSettings(t, "unused")
	calls := 0
	settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
		calls++
		providerHttpsTestRequest(t, request, false)
		response := providerHttpsTestResponse("private-response-body")
		response.StatusCode = http.StatusServiceUnavailable
		return response, nil
	}}
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 || len(alerts) != 1 {
		t.Fatal("failed source changed unknown or request ownership")
	}
	alert := requireAlertClass(t, alerts, "provider-picker-unobservable")
	requireAlertOmits(t, alert, "mimir_requested_eval_unix=", "mimir_requested_window_start_unix=", "mimir_requested_window_end_unix=", "private-response-body")
	if !strings.Contains(alert.Observed, "mimir_query_phase=http-status") || !strings.Contains(alert.Observed, "http_status=503") {
		t.Fatal("clock change erased finite failure authority")
	}
}
