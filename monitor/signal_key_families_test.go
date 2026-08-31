package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestKeyFamiliesSignalSyntheticKeyFamilyGrowth(t *testing.T) {
	stateDir := t.TempDir()
	populateMetric(t, stateDir, "redis/family/foo:<id>", 1000, 1000, 1000)
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "INFO memory") {
				return "6380 9000000 10000000\n6381 4000000 10000000", nil
			}
			return "", nil
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			return "30000 foo:<id>", nil
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
