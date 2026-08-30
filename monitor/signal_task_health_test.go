package monitor

import (
	"context"
	"testing"
)

func TestTaskHealthSignalSyntheticDurationRegression(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"UpdateClientScores", "120.0", "30.0", "12"}}, nil
	}}
	alerts, err := NewTaskHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "task-duration-regression")
}
