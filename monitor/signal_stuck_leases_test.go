package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestStuckLeasesSignalSyntheticStrandedLease(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "claim_time <"):
			return []Row{{"CloseExpiredContracts", "180", "120", "300", "task-1"}}, nil
		case strings.Contains(query, "greatest(run_at"):
			return []Row{{"0"}}, nil
		default:
			return nil, nil
		}
	}}
	alerts, err := NewStuckLeasesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "task-lease-stranded")
}
