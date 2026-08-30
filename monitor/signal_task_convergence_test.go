package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestTaskConvergenceSignalSyntheticTaskPlaneLag(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "greatest(run_at") {
			return []Row{{"181"}}, nil
		}
		return nil, nil
	}}
	alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "task-due-lag")
}

func TestTaskConvergenceSignalSyntheticMissingTarget(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "greatest(run_at"):
			return []Row{{"0"}}, nil
		case strings.Contains(query, "Target not found"):
			return []Row{{"NewRecurringTask", "101"}}, nil
		default:
			return nil, nil
		}
	}}
	alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "task-target-missing")
}
