package monitor

import (
	"bytes"
	"context"
	"encoding/gob"
	"strings"
	"testing"
)

func TestSelectionPopulationSignalSyntheticFreshEmptyCache(t *testing.T) {
	encode := func(counts []int) string {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(counts); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"100442", "88903", "0", "0", "0", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "{cs_0_q_") {
				return encode([]int{}), nil
			}
			return encode([]int{74000, 602}), nil
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "selection-empty")
	if alert.Frame != "gate-wipe" {
		t.Fatalf("frame = %q, want gate-wipe", alert.Frame)
	}
}
