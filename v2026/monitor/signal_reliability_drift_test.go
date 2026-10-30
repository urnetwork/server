package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestReliabilityDriftSignalSyntheticMovingMedianCorruption(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			for _, required := range []string{
				"to_jsonb(w)->>'degraded_classification_version'",
				"to_jsonb(w)->>'degraded_classification_write_token'",
				"client_reliability_running_window_classification_guard",
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
				"479", "414", "721", "76945", "2", "33", "307", "f", "f",
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
		"classification_guard_present=false",
		"schema head 603",
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
				"721", "721", "721", "76945", "33", "33", "33", "t", "t",
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
				"101225", "1", "1.087622", "0.50",
				"450", "360", "721", "76000", "0", "0", "0", "t", "t",
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
	for _, want := range []string{
		"admits 1 of 101225",
		"Fewer than one in 1,000",
		"One extreme outlier cannot supply meaningful provider diversity",
		"genuine fleet-wide unreliability",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("alert loses post-migration discriminator %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestReliabilityGateEffectivelyEmptyBoundary(t *testing.T) {
	tests := []struct {
		name        string
		scoreRows   int
		passingRows int
		empty       bool
	}{
		{name: "bootstrap belongs to empty-state signal", scoreRows: 999, passingRows: 0},
		{name: "zero at minimum corpus", scoreRows: 1000, passingRows: 0, empty: true},
		{name: "exact one-per-thousand boundary", scoreRows: 1000, passingRows: 1},
		{name: "one below scaled boundary", scoreRows: 100000, passingRows: 99, empty: true},
		{name: "scaled boundary", scoreRows: 100000, passingRows: 100},
		{name: "production isolated outlier", scoreRows: 101225, passingRows: 1, empty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reliabilityGateEffectivelyEmpty(tt.scoreRows, tt.passingRows); got != tt.empty {
				t.Fatalf("reliabilityGateEffectivelyEmpty(%d, %d) = %t, want %t", tt.scoreRows, tt.passingRows, got, tt.empty)
			}
		})
	}
}

func TestReliabilityDriftSignalSyntheticLegacyWriterResetsGuardedVersion(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{
				"0", "29805040", "29805761", "29805761",
				"100737", "0", "0.58", "0.52",
				"414", "414", "721", "76000", "2", "33", "33", "t", "t",
			}}, nil
		},
	}

	alerts, err := NewReliabilityDriftSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reliability-classification-drift")
	if alert.Frame != "legacy-writer-reset" {
		t.Fatalf("frame = %q, want legacy-writer-reset", alert.Frame)
	}
	for _, want := range []string{
		"fail-safe signature",
		"legacy Taskworker",
		"atomically revoked trust",
		"Finish converging every Taskworker",
		"classification_write_token_present=true",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("legacy-writer alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestReliabilityDriftSignalSyntheticRejectsUnguardedVersionOne(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{
				"1", "29805040", "29805761", "29805761",
				"100737", "74172", "1.0", "1.0",
				"721", "721", "721", "76945", "33", "33", "33", "f", "f",
			}}, nil
		},
	}

	alerts, err := NewReliabilityDriftSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reliability-classification-drift")
	if alert.Frame != "unguarded-version" {
		t.Fatalf("frame = %q, want unguarded-version", alert.Frame)
	}
	for _, want := range []string{
		"without both a rotated write token",
		"legacy Taskworker can preserve",
		"Apply schema migration 603",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("unguarded-version alert missing %q:\n%s", want, alert.Markdown())
		}
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
				"0", "0", "0", "0", "0", "0", "0", "f", "f",
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

// The actual seventeen-column projection includes bounded window NULLs and
// uncapped nonnegative weights; these fixtures do not substitute a toy reducer.
func reliabilityDriftAuthorityRow() Row {
	return Row{
		"1", "29805040", "29805761", "29805761",
		"100737", "74172", "1.0", "1.0",
		"721", "721", "721", "76945", "33", "33", "33", "t", "t",
	}
}

// Exercise required shape, every numeric/Boolean position and the aggregate
// relationships through the public signal and the real parser/classifier.
func TestReliabilityDriftAuthorityRejectsMalformedSnapshot(t *testing.T) {
	valid := reliabilityDriftAuthorityRow()
	cases := requiredAggregateBadShapes(valid)
	for _, column := range []int{0, 1, 2, 3, 4, 5, 10, 12, 13, 14} {
		for _, invalid := range []struct{ name, value string }{
			{"private text", aggregatePrivateCell}, {"null", ""}, {"blank", " "},
			{"negative", "-1"}, {"fraction", "0.9"}, {"integral decimal", "1.0"},
			{"not a number", "NaN"}, {"infinity", "Inf"}, {"overflow", "9223372036854775808"},
			{"exponent", "1e3"}, {"leading plus", "+1"},
		} {
			row := append(Row(nil), valid...)
			row[column] = invalid.value
			cases = append(cases, struct {
				name string
				rows []Row
			}{fmt.Sprintf("integer column %d %s", column, invalid.name), []Row{row}})
		}
	}
	for _, column := range []int{6, 7, 8, 9, 11} {
		for _, invalid := range []struct{ name, value string }{
			{"private text", aggregatePrivateCell}, {"null", ""}, {"blank", " "}, {"negative", "-0.1"},
			{"not a number", "NaN"}, {"infinity", "Inf"}, {"negative infinity", "-Inf"},
			{"overflow", "1e309"}, {"underflow", "1e-9999"}, {"hexadecimal", "0x1p0"}, {"underscore", "1_0"},
		} {
			row := append(Row(nil), valid...)
			row[column] = invalid.value
			cases = append(cases, struct {
				name string
				rows []Row
			}{fmt.Sprintf("float column %d %s", column, invalid.name), []Row{row}})
		}
	}
	for _, column := range []int{15, 16} {
		for _, invalid := range []struct{ name, value string }{
			{"private text", aggregatePrivateCell}, {"null", ""}, {"blank", " "},
			{"numeric true", "1"}, {"numeric false", "0"}, {"uppercase", "TRUE"},
		} {
			row := append(Row(nil), valid...)
			row[column] = invalid.value
			cases = append(cases, struct {
				name string
				rows []Row
			}{fmt.Sprintf("Boolean column %d %s", column, invalid.name), []Row{row}})
		}
	}
	for _, test := range []struct {
		name  string
		cells map[int]string
	}{
		{"classification int32 overflow", map[int]string{0: "2147483648"}},
		{"passing exceeds rows", map[int]string{5: "100738"}},
		{"nonzero weights without rows", map[int]string{4: "0", 5: "0"}},
		{"passing without qualifying maximum", map[int]string{6: "0.6", 7: "0.5"}},
		{"qualifying maximum without passing", map[int]string{5: "0"}},
		{"median exceeds maximum", map[int]string{7: "1.1"}},
		{"all passing below median threshold", map[int]string{5: valid[4], 7: "0.5"}},
		{"running median exceeds maximum", map[int]string{9: "722"}},
		{"reversed bounds", map[int]string{1: "29805762"}},
		{"recompute beyond window", map[int]string{3: "29805762"}},
		{"health exceeds window width", map[int]string{10: "722"}},
		{"moving exceeds health", map[int]string{12: "722"}},
		{"immutable exceeds health", map[int]string{13: "722"}},
		{"anchor exceeds window width", map[int]string{14: "722"}},
		{"median without health rows", map[int]string{10: "0", 12: "0", 13: "0"}},
		{"version without a window", map[int]string{1: "", 2: "", 3: "", 10: "0", 11: "0", 12: "0", 13: "0", 14: "0", 15: "f"}},
		{"token without a window", map[int]string{0: "0", 1: "", 2: "", 3: "", 10: "0", 11: "0", 12: "0", 13: "0", 14: "0"}},
		{"health without a window", map[int]string{0: "0", 1: "", 2: "", 3: "", 15: "f"}},
	} {
		row := append(Row(nil), valid...)
		for column, value := range test.cells {
			row[column] = value
		}
		cases = append(cases, struct {
			name string
			rows []Row
		}{test.name, []Row{row}})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				reads++
				for _, fragment := range []string{
					"FROM (VALUES (1)) seed(singleton)", "LEFT JOIN client_reliability_running_window",
					"COUNT(*) FILTER (WHERE independent_reliability_weight >= 0.7)",
					"COALESCE(MAX(independent_reliability_weight), 0)",
					"w.classification_write_token_present,", "g.present",
				} {
					if !strings.Contains(query, fragment) {
						t.Fatal("reliability authority control did not reach its actual mixed aggregate query")
					}
				}
				return test.rows, nil
			}}
			signal := NewReliabilityDriftSignal()
			alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
			if err == nil || reads != 1 {
				t.Error("malformed reliability snapshot established a product judgment")
			}
			requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
		})
	}
}

// Keep missing-window SQL NULLs, fractional/exponent output, weights above one,
// real collapse and the exact ratio threshold, including overflow-sized counts.
func TestReliabilityDriftAuthorityPreservesValidDomainsAndBands(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimumPassing := maximum/reliabilityDriftPassingDenominator + 1
	for _, test := range []struct {
		name  string
		cells map[int]string
		frame string
	}{
		{name: "guarded healthy"},
		{name: "textual Booleans", cells: map[int]string{15: "true", 16: "true"}},
		{name: "fractional and exponent diagnostics", cells: map[int]string{6: "1.087622e+00", 7: "0.75", 8: "720.5", 9: "414.5", 11: "76945.5"}},
		{name: "bootstrap below corpus", cells: map[int]string{4: "999", 5: "0", 6: "0.6", 7: "0.5"}},
		{name: "signed floating zero", cells: map[int]string{4: "999", 5: "0", 6: "-0", 7: "-0"}},
		{name: "minimum corpus zero passing", cells: map[int]string{4: "1000", 5: "0", 6: "0.6", 7: "0.5"}, frame: "gate-collapse"},
		{name: "exact one per thousand", cells: map[int]string{4: "1000", 5: "1", 6: "0.7", 7: "0.5"}},
		{name: "one below scaled boundary", cells: map[int]string{4: "100000", 5: "99", 6: "1.087622", 7: "0.5"}, frame: "gate-collapse"},
		{name: "scaled boundary", cells: map[int]string{4: "100000", 5: "100", 6: "1.087622", 7: "0.5"}},
		{name: "maximum count all passing", cells: map[int]string{4: strconv.Itoa(maximum), 5: strconv.Itoa(maximum)}},
		{name: "maximum count ratio boundary", cells: map[int]string{4: strconv.Itoa(maximum), 5: strconv.Itoa(minimumPassing), 7: "0.5"}},
		{name: "maximum count below boundary", cells: map[int]string{4: strconv.Itoa(maximum), 5: strconv.Itoa(minimumPassing - 1), 7: "0.5"}, frame: "gate-collapse"},
		{name: "zero scores", cells: map[int]string{4: "0", 5: "0", 6: "0", 7: "0"}, frame: "gate-collapse"},
		{name: "missing window existing scores", cells: map[int]string{0: "0", 1: "", 2: "", 3: "", 10: "0", 11: "0", 12: "0", 13: "0", 14: "0", 15: "false"}, frame: "moving-median-v0"},
		{name: "absent unguarded marker", cells: map[int]string{15: "false", 16: "false"}, frame: "unguarded-version"},
		{name: "legacy reset", cells: map[int]string{0: "0"}, frame: "legacy-writer-reset"},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := reliabilityDriftAuthorityRow()
			for column, value := range test.cells {
				row[column] = value
			}
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{row}, nil }}
			signal := NewReliabilityDriftSignal()
			alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
			if err != nil {
				t.Fatal("valid reliability scalar domain became source-unavailable")
			}
			if test.frame == "" {
				if len(alerts) != 0 {
					t.Error("valid reliability gate boundary became a product incident")
				}
				return
			}
			if len(alerts) != 1 {
				t.Fatal("valid reliability violation lost its single product finding")
			}
			alert := requireAlertClass(t, alerts, "reliability-classification-drift")
			if alert.Frame != test.frame || alert.Severity != SeverityPage || alert.Sustain != 1 {
				t.Error("valid reliability evidence changed its cause frame or exact band")
			}
		})
	}
}
