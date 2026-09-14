package monitor

import (
	"context"
	"testing"
)

func TestContractRateSignalSyntheticContractCollapse(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"700"}}, nil }}
	alerts, err := NewContractRateSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "contracts-collapse")
}
