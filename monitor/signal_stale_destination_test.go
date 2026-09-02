package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStaleDestinationSignalSyntheticLifecycleRejection(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 30, 0, 0, time.UTC)
	alerts := runStaleDestinationFixture(t, now, now, 480.125, 21.5)
	if len(alerts) != 1 || alerts[0].Frame != "lifecycle-rejection" {
		t.Fatalf("alerts=%+v, want one lifecycle-rejection alert", alerts)
	}
	alert := alerts[0]
	if alert.SignalNumber != "2.18" || alert.SignalKey != "stale-destination" || alert.Sustain != 1 {
		t.Fatalf("wrong signal identity: %+v", alert)
	}
	for _, want := range []string{
		"inactive contract destinations at 501.6/min",
		"companion_false_rate=480.125",
		"companion_true_rate=21.500",
		"stale Redis provide advertisement",
		"server commit c8dfe570",
		"Connect commit 5b33c91",
		"retires only the emitting window channel",
		"not a hardware-capacity signal",
		"no client, network, contract, or destination identifier",
		"SIGNALS.md §2.18 and §5.9",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stale-destination alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestStaleDestinationSignalSyntheticHealthyBoundary(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 31, 0, 0, time.UTC)
	if alerts := runStaleDestinationFixture(t, now, now, 49.5, 0.5); len(alerts) != 0 {
		t.Fatalf("boundary rate alerted: %+v", alerts)
	}
}

func TestStaleDestinationSignalSyntheticRequiresBothInitializedPartitions(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 32, 0, 0, time.UTC)
	payload := staleDestinationFixtureJSON(t, now, []staleDestinationFixtureRate{{"false", 0}})
	_, err := NewStaleDestinationSignal().Run(
		context.Background(),
		staleDestinationSyntheticSettings(t, now, payload),
	)
	if err == nil || !strings.Contains(err.Error(), `missing companion partition "true"`) {
		t.Fatalf("missing initialized partition error=%v", err)
	}
}

func TestStaleDestinationSignalSyntheticRejectsDuplicateAndUnknownPartitions(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 33, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		rates []staleDestinationFixtureRate
		want  string
	}{
		{
			name:  "duplicate",
			rates: []staleDestinationFixtureRate{{"false", 1}, {"false", 2}, {"true", 0}},
			want:  `duplicate companion partition "false"`,
		},
		{
			name:  "unknown",
			rates: []staleDestinationFixtureRate{{"false", 0}, {"unknown", 0}},
			want:  `unexpected companion partition "unknown"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := staleDestinationFixtureJSON(t, now, test.rates)
			_, err := NewStaleDestinationSignal().Run(
				context.Background(),
				staleDestinationSyntheticSettings(t, now, payload),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestStaleDestinationSignalSyntheticRejectsStaleInvalidAndSkewedSamples(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 34, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		falseTime time.Time
		trueTime  time.Time
		falseRate float64
		trueRate  float64
		want      string
	}{
		{name: "stale", falseTime: now.Add(-2 * time.Minute), trueTime: now, want: "stale companion=false sample"},
		{name: "negative", falseTime: now, trueTime: now, falseRate: -1, want: "invalid companion=false rate"},
		{name: "skewed", falseTime: now, trueTime: now.Add(-time.Second), want: "partition sample times differ"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := staleDestinationFixtureJSONTimes(t, []staleDestinationFixtureSample{
				{companion: "false", sampleTime: test.falseTime, rate: test.falseRate},
				{companion: "true", sampleTime: test.trueTime, rate: test.trueRate},
			})
			_, err := NewStaleDestinationSignal().Run(
				context.Background(),
				staleDestinationSyntheticSettings(t, now, payload),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func runStaleDestinationFixture(
	t testing.TB,
	now time.Time,
	sampleTime time.Time,
	falseRate float64,
	trueRate float64,
) Alerts {
	t.Helper()
	payload := staleDestinationFixtureJSON(t, sampleTime, []staleDestinationFixtureRate{
		{"false", falseRate},
		{"true", trueRate},
	})
	alerts, err := NewStaleDestinationSignal().Run(
		context.Background(),
		staleDestinationSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func staleDestinationSyntheticSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_connect_contract_failures_total") ||
			!strings.Contains(command, "inactive_destination") ||
			!strings.Contains(command, "sum+by+%28companion%29") ||
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

type staleDestinationFixtureRate struct {
	companion string
	rate      float64
}

type staleDestinationFixtureSample struct {
	companion  string
	sampleTime time.Time
	rate       float64
}

func staleDestinationFixtureJSON(t testing.TB, sampleTime time.Time, rates []staleDestinationFixtureRate) string {
	t.Helper()
	samples := make([]staleDestinationFixtureSample, 0, len(rates))
	for _, rate := range rates {
		samples = append(samples, staleDestinationFixtureSample{
			companion:  rate.companion,
			sampleTime: sampleTime,
			rate:       rate.rate,
		})
	}
	return staleDestinationFixtureJSONTimes(t, samples)
}

func staleDestinationFixtureJSONTimes(t testing.TB, samples []staleDestinationFixtureSample) string {
	t.Helper()
	result := []map[string]any{}
	for _, sample := range samples {
		result = append(result, map[string]any{
			"metric": map[string]string{"companion": sample.companion},
			"value":  []any{float64(sample.sampleTime.Unix()), fmt.Sprintf("%.6f", sample.rate)},
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
