package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMissingOriginSignalSyntheticFallbackFromNormal(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 36, 20, 0, time.UTC)
	alerts := runMissingOriginFixture(t, now, 1411.325)
	if len(alerts) != 1 || alerts[0].Frame != "fallback-from-normal" {
		t.Fatalf("alerts=%+v, want one fallback-from-normal alert", alerts)
	}
	fallback := &alerts[0]
	if fallback.SignalNumber != "2.17" || fallback.SignalKey != "missing-origin" || fallback.Sustain != 1 {
		t.Fatalf("wrong signal identity: %+v", *fallback)
	}
	for _, want := range []string{
		"non-companion requests are 1411.3/min",
		"fell back to Stream/companion settlement",
		"original wire bit",
		"active top-level providers",
		"maximum client-window lifetime",
		"provider return paths and same-network peers",
		"do not infer endpoint roles",
		"edit Redis blobs",
		"No client, network, contract, or destination identifier",
		"SIGNALS.md §2.17 and §5.9",
	} {
		if !strings.Contains(fallback.Markdown(), want) {
			t.Fatalf("fallback alert missing %q:\n%s", want, fallback.Markdown())
		}
	}
}

func TestMissingOriginSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 41, 0, 0, time.UTC)
	if alerts := runMissingOriginFixture(t, now, 499.999); len(alerts) != 0 {
		t.Fatalf("rates below boundary alerted: %+v", alerts)
	}
}

func TestMissingOriginSignalSyntheticMissingSeriesIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 42, 0, 0, time.UTC)
	payload := missingOriginFixtureJSON(t, now, nil)
	_, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err == nil || !strings.Contains(err.Error(), "returned 0 fallback series, want 1") {
		t.Fatalf("missing counter series error=%v", err)
	}
}

func TestMissingOriginSignalSyntheticDuplicateSeriesIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 42, 0, 0, time.UTC)
	payload := missingOriginFixtureJSON(t, now, []float64{10, 20})
	_, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err == nil || !strings.Contains(err.Error(), "returned 2 fallback series, want 1") {
		t.Fatalf("duplicate counter series error=%v", err)
	}
}

func TestMissingOriginSignalSyntheticRejectsStaleAndInvalidRates(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 43, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		sampleTime time.Time
		falseRate  float64
		want       string
	}{
		{name: "stale", sampleTime: now.Add(-2 * time.Minute), falseRate: 10, want: "stale fallback sample"},
		{name: "negative", sampleTime: now, falseRate: -1, want: "invalid fallback rate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := missingOriginFixtureJSON(t, test.sampleTime, []float64{test.falseRate})
			_, err := NewMissingOriginSignal().Run(
				context.Background(),
				missingOriginSyntheticSettings(t, now, payload),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func runMissingOriginFixture(t testing.TB, now time.Time, rate float64) Alerts {
	t.Helper()
	payload := missingOriginFixtureJSON(t, now, []float64{rate})
	alerts, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func missingOriginSyntheticSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_connect_contract_failures_total") ||
			!strings.Contains(command, "missing_companion_origin") ||
			!strings.Contains(command, "companion%3D%22false%22") ||
			!strings.Contains(command, "%5B5m%5D") ||
			!strings.Contains(command, "%22synthetic%22") {
			return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func missingOriginFixtureJSON(t testing.TB, sampleTime time.Time, rates []float64) string {
	t.Helper()
	result := []map[string]any{}
	for _, rate := range rates {
		result = append(result, map[string]any{
			"metric": map[string]string{},
			"value":  []any{float64(sampleTime.Unix()), fmt.Sprintf("%.6f", rate)},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
