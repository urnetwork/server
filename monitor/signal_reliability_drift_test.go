package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestReliabilityDriftSignalSyntheticMovingMedianCorruption(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			for _, required := range []string{
				"to_jsonb(w)->>'degraded_classification_version'",
				"CROSS JOIN LATERAL",
				"candidate.block_number - 59",
			} {
				if !strings.Contains(query, required) {
					t.Fatalf("query missing %q", required)
				}
			}
			return []Row{{
				"0", "29805040", "29805761", "29805616",
				"100737", "0", "0.666203", "0.575800",
				"479", "414", "721", "76945", "2", "33", "307",
			}}, nil
		},
	}

	alerts, err := NewReliabilityDriftSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reliability-classification-drift")
	if alert.Frame != "moving-median-v0" {
		t.Fatalf("frame = %q, want moving-median-v0", alert.Frame)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"admits 0 of 100737",
		"moving lookback",
		"mandatory one-time re-anchor",
		"classification_version=0",
		"anchor_moving_degraded=307",
		"SIGNALS.md §2.15",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestReliabilityDriftSignalSyntheticCurrentHealthy(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{
				"1", "29805040", "29805761", "29805761",
				"100737", "74172", "1.0", "1.0",
				"721", "721", "721", "76945", "33", "33", "33",
			}}, nil
		},
	}

	alerts, err := NewReliabilityDriftSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy signal returned alerts: %+v", alerts)
	}
}

func TestReliabilityDriftSignalSyntheticCurrentGateCollapse(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{
				"1", "29805040", "29805761", "29805761",
				"80000", "0", "0.62", "0.50",
				"450", "360", "721", "76000", "0", "0", "0",
			}}, nil
		},
	}

	alerts, err := NewReliabilityDriftSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reliability-classification-drift")
	if alert.Frame != "gate-collapse" {
		t.Fatalf("frame = %q, want gate-collapse", alert.Frame)
	}
	if !strings.Contains(alert.Markdown(), "genuine fleet-wide unreliability") {
		t.Fatalf("alert loses the post-migration discriminator:\n%s", alert.Markdown())
	}
}

func TestReliabilityDriftSignalSyntheticMissingRunningState(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "FROM (VALUES (1)) seed(singleton)") {
				t.Fatalf("query does not preserve a row when running state is missing")
			}
			return []Row{{
				"0", "", "", "",
				"0", "0", "0", "0",
				"0", "0", "0", "0", "0", "0", "0",
			}}, nil
		},
	}

	alerts, err := NewReliabilityDriftSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reliability-classification-drift")
	if alert.Frame != "moving-median-v0" {
		t.Fatalf("frame = %q, want moving-median-v0", alert.Frame)
	}
	if !strings.Contains(alert.Markdown(), "admits 0 of 0") {
		t.Fatalf("missing state alert lost empty-score evidence:\n%s", alert.Markdown())
	}
}
