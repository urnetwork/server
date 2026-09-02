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
			return []Row{{"100442", "88903", "88000", "800", "103", "0", "0", "0", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, providerEligibilityReadyKey) {
				return "", nil
			}
			if strings.Contains(joined, "{cs_0_q_") {
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

func TestSelectionPopulationSignalSyntheticRejectsIneligibleSupply(t *testing.T) {
	encode := func(counts []int) string {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(counts); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			for _, want := range []string{
				"INNER JOIN network_client nc",
				"pk.provide_mode IN (1,3)",
				"source_client_id IS NOT NULL",
				"NOT active",
			} {
				if !strings.Contains(query, want) {
					t.Fatalf("provider supply query missing %q", want)
				}
			}
			return []Row{{"150544", "390110", "90298", "297776", "2036", "0", "0", "0", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, providerEligibilityReadyKey) {
				return "", nil
			}
			return encode([]int{80000}), nil
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-supply-ineligible")
	if alert.Frame != "legacy-filter" {
		t.Fatalf("frame = %q, want legacy-filter", alert.Frame)
	}
	for _, want := range []string{
		"297776 derived",
		"derived_providing=297776",
		"eligibility_ready=false",
		"b7599962",
		"do not delete client",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("provider eligibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestSelectionPopulationSignalSyntheticReadyMarkerClearsRawResidue(t *testing.T) {
	encode := func(counts []int) string {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(counts); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"150544", "390110", "90298", "297776", "2036", "0", "0", "0", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), providerEligibilityReadyKey) {
				return providerEligibilityReadyValue, nil
			}
			return encode([]int{80000}), nil
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("completed eligibility export retained alerts: %+v", alerts)
	}
}
