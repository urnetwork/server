package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSourceAttributionSignalSyntheticProblems(t *testing.T) {
	tests := []struct {
		name       string
		settings   SourceAttributionSettings
		response   string
		requestErr error
	}{
		{
			name:       "endpoint is not 2xx",
			settings:   SourceAttributionSettings{ExpectedIPv4: "203.0.113.10"},
			requestErr: errors.New("synthetic HTTP 503"),
		},
		{
			name:     "family is wrong",
			settings: SourceAttributionSettings{ExpectedIPv6: "2001:db8::10"},
			response: `{"info":{"ip":"203.0.113.10"}}`,
		},
		{
			name:     "source is the ingress",
			settings: SourceAttributionSettings{ExpectedIPv4: "203.0.113.10"},
			response: `{"info":{"ip":"65.49.70.82"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
				if name != "curl" || (!strings.Contains(strings.Join(args, " "), "--ipv4") && !strings.Contains(strings.Join(args, " "), "--ipv6")) {
					t.Fatalf("unexpected local command %s %v", name, args)
				}
				return test.response, test.requestErr
			}}
			settings := syntheticSettings(source)
			settings.SourceAttribution = test.settings
			alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "source-attribution")
		})
	}
}
