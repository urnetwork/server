package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A failed observer route must not hide a healthy authorized metrics gateway.
// Transport failover preserves the original inventory and process-clock query;
// a successful but malformed source remains unknown rather than being replaced.
func TestProviderPickerAuthorizedGatewayFailover(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		firstFail bool
		allFail   bool
		malformed bool
		empty     bool
		wantHosts []string
		wantClass string
	}{
		{name: "healthy primary", wantHosts: []string{"gateway-first"}},
		{name: "healthy sibling after route failure", firstFail: true, wantHosts: []string{"gateway-first", "gateway-sibling"}},
		{name: "real empty survives failover", firstFail: true, empty: true, wantHosts: []string{"gateway-first", "gateway-sibling"}, wantClass: "provider-picker-effective-empty"},
		{name: "all unavailable stays unknown", firstFail: true, allFail: true, wantHosts: []string{"gateway-first", "gateway-sibling"}, wantClass: "provider-picker-unobservable"},
		{name: "malformed response stays unknown", malformed: true, wantHosts: []string{"gateway-first"}, wantClass: "provider-picker-unobservable"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
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
			visited := []string{}
			firstCommand := ""
			settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
				visited = append(visited, host.Name)
				if firstCommand == "" {
					firstCommand = command
				} else if command != firstCommand {
					t.Fatal("fallback changed the exact source window or inventory query")
				}
				if !strings.Contains(command, "--max-time 15 --max-filesize 4194304") {
					t.Fatal("fallback removed the source bounds")
				}
				if scenario.allFail || scenario.firstFail && host.Name == "gateway-first" {
					return "", fmt.Errorf("private-source-credential: observer transport unavailable")
				}
				if scenario.malformed {
					return "not a metrics response", nil
				}
				return payload, nil
			}}
			alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(visited, scenario.wantHosts) {
				t.Fatalf("queried gateways %v, want %v", visited, scenario.wantHosts)
			}
			if scenario.wantClass != "" {
				requireAlertClass(t, alerts, scenario.wantClass)
			}
			for _, alert := range alerts {
				if strings.Contains(alert.Markdown(), "private-source-credential") {
					t.Fatal("source error escaped the privacy boundary")
				}
				if strings.HasPrefix(alert.Class, "provider-picker-") && alert.Class != scenario.wantClass {
					t.Fatalf("unexpected picker verdict %s", alert.Class)
				}
			}
		})
	}
}
