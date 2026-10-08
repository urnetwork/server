package monitor

import (
	"context"
	"strings"
	"testing"
)

func reliabilityIndexSyntheticSettings(t *testing.T, row Row) SignalSettings {
	t.Helper()
	return syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"pg_get_indexdef",
			"pg_inherits",
			"client_reliability_valid_bnch_net_client",
			"client_reliability_valid_block_number_client_address_hash",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("reliability-index query is missing %q:\n%s", want, query)
			}
		}
		return []Row{row}, nil
	}})
}

func TestReliabilityIndexSignalReportsProductionOldShape(t *testing.T) {
	settings := reliabilityIndexSyntheticSettings(t, Row{
		"t", "t", "f", "f", "", "10", "0", "0",
	})
	alerts, err := NewReliabilityIndexSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reliability-index-drift")
	markdown := alert.Markdown()
	for _, want := range []string{
		"old non-covering parent index is still present",
		"INCLUDE (network_id, client_id)",
		"old_index_present=true",
		"table_partitions=10",
		"operational database-maintenance alert",
		"service release cannot create the physical index",
		"Do not start the upgrade while the current measurement must remain undisturbed",
		"bringyourctl model upgrade-client-reliability-index",
		"Do not CREATE INDEX on the partitioned parent inline",
		"no new [crp] secondary-index-drift warning appears for five minutes",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("reliability-index alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestReliabilityIndexSignalReportsInterruptedUpgrade(t *testing.T) {
	definition := "CREATE INDEX client_reliability_valid_bnch_net_client ON ONLY public.client_reliability" + reliabilityIndexDesiredDefinitionSuffix
	settings := reliabilityIndexSyntheticSettings(t, Row{
		"t", "f", "t", "f", definition, "10", "4", "0",
	})
	alerts, err := NewReliabilityIndexSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "reliability-index-drift").Markdown()
	for _, want := range []string{
		"partition upgrade is incomplete",
		"desired_index_present=true",
		"desired_index_valid=false",
		"desired_shape_matches=true",
		"attached_child_indexes=4",
		"rerun the same command if interrupted",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("interrupted-upgrade alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestReliabilityIndexSignalReportsFinalizationOnlyWithoutRebuild(t *testing.T) {
	definition := "CREATE INDEX client_reliability_valid_bnch_net_client ON ONLY public.client_reliability" + reliabilityIndexDesiredDefinitionSuffix
	settings := reliabilityIndexSyntheticSettings(t, Row{
		"t", "t", "t", "t", definition, "34", "34", "0",
	})
	alerts, err := NewReliabilityIndexSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "reliability-index-drift").Markdown()
	for _, want := range []string{
		"covering replacement is complete",
		"Only the supported upgrade's final old-parent DROP remains",
		"finalization-only operational maintenance",
		"skip every completed child",
		"rather than scan or sort the 34 partitions again",
		"15-second lock timeout",
		"wait until the protected measurement permits that lock",
		"require it to skip all completed partition children",
		"Do not manually drop either index",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("finalization-only alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Each partition build scans and sorts one large partition") {
		t.Fatalf("finalization-only alert incorrectly prescribed another build:\n%s", markdown)
	}
}

func TestReliabilityIndexSignalHealthyAtExactPhysicalContract(t *testing.T) {
	definition := "CREATE INDEX client_reliability_valid_bnch_net_client ON ONLY public.client_reliability" + reliabilityIndexDesiredDefinitionSuffix
	settings := reliabilityIndexSyntheticSettings(t, Row{
		"t", "f", "t", "t", definition, "10", "10", "0",
	})
	alerts, err := NewReliabilityIndexSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("exact reliability-index contract produced alerts: %+v", alerts)
	}
}

func TestReliabilityIndexSignalRejectsMalformedCatalogState(t *testing.T) {
	settings := reliabilityIndexSyntheticSettings(t, Row{
		"t", "f", "t", "not-a-bool", "definition", "10", "10", "0",
	})
	if _, err := NewReliabilityIndexSignal().Run(context.Background(), settings); err == nil || !strings.Contains(err.Error(), "desired-index-valid") {
		t.Fatalf("malformed catalog boolean error = %v", err)
	}
}
