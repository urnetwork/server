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
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"t"}}, nil
			}
			for _, want := range []string{"AND NOT peh.tls_authentication_failure", "WHERE tls_authentication_failure"} {
				if !strings.Contains(query, want) {
					t.Fatalf("provider supply query missing TLS-integrity clause %q", want)
				}
			}
			return []Row{{"100442", "88903", "88000", "800", "103", "88000", "0", "88000", "88000", "target-us"}}, nil
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
	for _, want := range []string{
		"fresh_passing_excluding_tls=0",
		"tls_integrity_armed=true",
		"tls_authentication_failures=88000",
		"aggregate-only",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("selection alert missing %q:\n%s", want, alert.Markdown())
		}
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
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"f"}}, nil
			}
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
			if strings.Contains(query, "tls_authentication_failure") {
				t.Fatalf("pre-migration population query references the absent TLS column:\n%s", query)
			}
			return []Row{{"150544", "390110", "90298", "297776", "2036", "0", "0", "0", "0", "target-us"}}, nil
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
		"tls_integrity_armed=false",
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
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"t"}}, nil
			}
			return []Row{{"150544", "390110", "90298", "297776", "2036", "0", "0", "0", "0", "target-us"}}, nil
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

func TestSelectionPopulationSignalRejectsAmbiguousTLSIntegritySchemaState(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "pg_attribute") {
				t.Fatal("population query ran after an ambiguous TLS-integrity schema result")
			}
			return []Row{{"unknown"}}, nil
		},
	}
	_, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || !strings.Contains(err.Error(), "invalid TLS-integrity arming state") {
		t.Fatalf("Run error = %v, want explicit ambiguous TLS-integrity schema failure", err)
	}
}
