package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestKeyFamiliesSignalSyntheticKeyFamilyGrowth(t *testing.T) {
	stateDir := t.TempDir()
	populateMetric(t, stateDir, "redis/family/ckey_<id>", 1000, 1000, 1000)
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "INFO memory") {
				return "6380 9000000 10000000\n6381 4000000 10000000", nil
			}
			return "", nil
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			return "30000 ckey_<id>", nil
		},
	}
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	alerts, err := NewKeyFamiliesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "family-growth")
}

func TestSafeRedisFamilyLabelRejectsUnnormalizedKeyMaterial(t *testing.T) {
	tests := []struct {
		name   string
		family string
		want   string
	}{
		{name: "normalized", family: "{pm_<id>}sk_1", want: "{pm_<id>}sk_1"},
		{name: "raw printable", family: "customer@example.invalid", want: "redacted-unnormalized-family"},
		{name: "raw token beside id", family: "customer_alice_<id>", want: "redacted-unnormalized-family"},
		{name: "binary", family: "{\x01\xff<id>}", want: "redacted-binary-family"},
		{name: "oversize", family: strings.Repeat("a", 161) + "<id>", want: "redacted-oversize-family"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeRedisFamilyLabel(test.family); got != test.want {
				t.Fatalf("safe family label = %q, want %q", got, test.want)
			}
		})
	}
}

func TestKeyFamiliesSignalAggregatesBinaryFamiliesBeforeAlerting(t *testing.T) {
	stateDir := t.TempDir()
	metric := "redis/family/redacted-binary-family"
	populateMetric(t, stateDir, metric, 1000, 1000, 1000)
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "INFO memory") {
				return "6380 9000000 10000000", nil
			}
			return "", nil
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			return "21000 bad\x01\xff<id>\n12000 bad\x02\xff<id>", nil
		},
	}
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	alerts, err := NewKeyFamiliesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "family-growth")
	markdown := alert.Markdown()
	if !strings.Contains(markdown, "redacted-binary-family") || !strings.Contains(markdown, "count=33000") {
		t.Fatalf("binary families were not safely aggregated: %s", markdown)
	}
	if strings.Contains(markdown, "bad") || strings.ContainsRune(markdown, '\ufffd') {
		t.Fatalf("binary key material reached alert markdown: %q", markdown)
	}
}
