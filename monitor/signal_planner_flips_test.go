package monitor

import (
	"context"
	"testing"
)

func TestPlannerFlipsSignalSyntheticStatsLandmine(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"1", "0"}}, nil }}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "stats-landmine")
}

func TestPlannerFlipsSignalSyntheticEmptyPartialIndexStats(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"2", "2"}}, nil }}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "stats-landmine")
}
