package monitor

import (
	"context"
	"testing"
)

func TestSelectionFreshnessSignalSyntheticSelectionStaleness(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"6000"}}, nil }}
	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "selection-stale")
}
