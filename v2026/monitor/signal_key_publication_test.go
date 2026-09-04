package monitor

import (
	"context"
	"testing"
)

func TestKeyPublicationSignalSyntheticCoverageCollapse(t *testing.T) {
	stateDir := t.TempDir()
	values := make([]float64, 12)
	for i := range values {
		values[i] = 0.5
	}
	populateMetric(t, stateDir, e2eCoverageMetric, values...)
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"100", "10"}}, nil
	}}
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	alerts, err := NewKeyPublicationSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "e2e-key-coverage")
}
