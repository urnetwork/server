package monitor

import (
	"context"
	"strings"
	"testing"
)

func syntheticHMACCutoverSource(row Row) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "monitor-signal-2.24-hmac-cutover") {
			return nil, nil
		}
		return []Row{row}, nil
	}}
}

func TestHMACCutoverSignalSyntheticIncompatibleCohort(t *testing.T) {
	// The legacy cohort is exactly 90% dark. The compatible cohort is exactly
	// 50% healthy, providing the negative control at both threshold edges.
	row := Row{"t", "86400", "1000", "300", "20", "200", "500", "100", "90", "10", "100", "50", "50"}
	alerts, err := NewHMACCutoverSignal().Run(
		context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one incompatibility page: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "contract-hmac-incompatible")
	if alert.Severity != SeverityPage {
		t.Fatalf("severity = %s, want page", alert.Severity)
	}
	for _, want := range []string{
		"90 of 100 current checks",
		"minimum_dual_verifier_version=2026.5.14",
		"claimed_legacy=300",
		"legacy_networks=20",
		"legacy_dark=90",
		"compatible_ok=50",
		"reject each newly created contract before forwarding traffic",
		"compatible-version cohort remains a healthy control",
		"Raw descriptions plus provider and network identifiers never leave",
		"explicit security/availability decision",
		"must converge both API and Connect signer paths",
		"not a Proxy RAM or active-client hardware ceiling",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("incompatibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestHMACCutoverSignalSyntheticNeedsBehavioralControl(t *testing.T) {
	row := Row{"t", "86400", "1000", "300", "20", "200", "500", "19", "19", "0", "100", "50", "50"}
	alerts, err := NewHMACCutoverSignal().Run(
		context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one readiness warning: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "contract-hmac-readiness")
	if alert.Severity != SeverityWarn {
		t.Fatalf("severity = %s, want warn", alert.Severity)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"current controls do not yet prove one disposition",
		"claimed old version is not by itself proof",
		"Complete §2.19 coverage",
		"Unknown metadata is not treated as compatible",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("readiness alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "reject each newly created contract") {
		t.Fatalf("insufficient cohort was given the causal diagnosis:\n%s", markdown)
	}
}

func TestHMACCutoverSignalSyntheticPrecutoverRisk(t *testing.T) {
	row := Row{"f", "-86400", "40", "25", "4", "5", "10", "0", "0", "0", "0", "0", "0"}
	alerts, err := NewHMACCutoverSignal().Run(
		context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "contract-hmac-readiness")
	if !strings.Contains(alert.Markdown(), "before the stored-contract HMAC cutover") {
		t.Fatalf("precutover warning lost its boundary:\n%s", alert.Markdown())
	}
}

func TestHMACCutoverSignalSyntheticHealthyNoLegacyClaim(t *testing.T) {
	row := Row{"t", "86400", "1000", "0", "0", "200", "800", "0", "0", "0", "100", "10", "90"}
	alerts, err := NewHMACCutoverSignal().Run(
		context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy cohort returned alerts: %+v", alerts)
	}
}

func TestParseHMACCutoverSnapshotRejectsAmbiguity(t *testing.T) {
	tests := []struct {
		name string
		rows []pgRow
	}{
		{name: "missing", rows: nil},
		{name: "bad shape", rows: []pgRow{{"t", "1"}}},
		{name: "bad bool", rows: []pgRow{{"maybe", "1", "1", "0", "0", "1", "0", "0", "0", "0", "0", "0", "0"}}},
		{name: "active before boundary", rows: []pgRow{{"t", "-1", "1", "0", "0", "1", "0", "0", "0", "0", "0", "0", "0"}}},
		{name: "inactive after boundary", rows: []pgRow{{"f", "0", "1", "0", "0", "1", "0", "0", "0", "0", "0", "0", "0"}}},
		{name: "population mismatch", rows: []pgRow{{"t", "1", "10", "2", "1", "2", "2", "0", "0", "0", "0", "0", "0"}}},
		{name: "legacy tally mismatch", rows: []pgRow{{"t", "1", "10", "5", "1", "2", "3", "4", "3", "0", "0", "0", "0"}}},
		{name: "compatible checked exceeds cohort", rows: []pgRow{{"t", "1", "10", "5", "1", "2", "3", "0", "0", "0", "3", "1", "2"}}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseHMACCutoverSnapshot(testCase.rows); err == nil {
				t.Fatal("ambiguous HMAC aggregate was accepted")
			}
		})
	}
}

func TestHMACCutoverQueryIsBoundedAndPrivate(t *testing.T) {
	query := hmacCutoverQuery()
	for _, want := range []string{
		"monitor-signal-2.24-hmac-cutover",
		"nc.active",
		"nc.source_client_id IS NULL",
		"nclr.connected",
		"nclr.valid",
		"pk.provide_mode = 3",
		"regexp_count",
		"ROW(2026, 5, 14)",
		"provider_blackhole_check",
		"interval '10800 seconds'",
		"timestamp '2026-09-01 00:00:00'",
		"count(DISTINCT network_id)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
	finalSelect := query[strings.LastIndex(query, "SELECT cutover_active"):]
	for _, forbidden := range []string{"description", "client_id", "network_id", "claimed_version"} {
		if strings.Contains(finalSelect, forbidden) {
			t.Fatalf("final aggregate exports %q:\n%s", forbidden, finalSelect)
		}
	}
}
