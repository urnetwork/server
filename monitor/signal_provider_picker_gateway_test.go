// The HTTPS pilot explicitly replaces ordered SSH failover for this fleet query.
package monitor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Healthy, real-empty, malformed and failed native responses each use one
// request. No configured, disabled or excluded gateway becomes a fallback.
func TestProviderPickerNativeQueryDoesNotFallBackToGateways(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		failed    bool
		malformed bool
		empty     bool
		wantClass string
	}{
		{name: "healthy"},
		{name: "real empty", empty: true, wantClass: "provider-picker-effective-empty"},
		{name: "unavailable", failed: true, wantClass: "provider-picker-unobservable"},
		{name: "malformed", malformed: true, wantClass: "provider-picker-unobservable"},
	} {
		field := "initial/nonempty"
		if scenario.empty {
			field = "initial/empty"
		}
		payload := pickerTestPayload(t, pickerTestRows(map[string]float64{field: 20}))
		settings := pickerTestSettings(t, payload)
		settings.Hosts = []HostSettings{
			{Name: "gateway-first", Roles: []string{"services"}},
			{Name: "gateway-paused", Roles: []string{"services"}},
			{Name: "gateway-sibling", Roles: []string{"services"}},
			{Name: "api-synthetic", Roles: []string{"api"}},
		}
		settings.ExcludedHosts = []string{"gateway-paused"}
		settings.disabledHosts = []HostSettings{{Name: "gateway-disabled", Roles: []string{"services"}}}
		queryCalls, sshCalls := 0, 0
		settings.Source = &providerHttpsTestSource{
			syntheticSource: &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				sshCalls++
				return payload, nil
			}},
			do: func(request *http.Request) (*http.Response, error) {
				queryCalls++
				providerHttpsTestRequest(t, request, false)
				if scenario.failed {
					return nil, errors.New("private-source-credential: observer transport unavailable")
				}
				if scenario.malformed {
					return providerHttpsTestResponse("not a metrics response"), nil
				}
				return providerHttpsTestResponse(payload), nil
			},
		}
		alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
		if err != nil || queryCalls != 1 || sshCalls != 0 {
			t.Fatalf("%s crossed the one-request/no-SSH boundary", scenario.name)
		}
		if scenario.wantClass != "" {
			requireAlertClass(t, alerts, scenario.wantClass)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-source-credential")
			if strings.HasPrefix(alert.Class, "provider-picker-") && alert.Class != scenario.wantClass {
				t.Fatalf("%s changed the picker verdict", scenario.name)
			}
		}
	}
}
