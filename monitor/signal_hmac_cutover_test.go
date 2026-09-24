package monitor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
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
		"actual legacy-only receiver rejects standard-signed contracts before payload forwarding",
		"Passing claimed-compatible checks exclude only a universal shared-path failure",
		"Raw descriptions plus provider and network identifiers never leave",
		"explicit security/availability decision",
		"expires and fails open",
		"not durable quarantine",
		"self-reported description alone must never become a permanent exclusion",
		"uniformly build every API and Connect signer from one reviewed shared Connect policy source",
		"rebuild and safely promote the monitor from that same policy source",
		"bounded period with a named sunset",
		"not final protocol closure",
		"whole refresh inside the three-hour verdict lifetime",
		"passing its availability gate is not final closure",
		"not a Proxy RAM or active-client hardware ceiling",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("incompatibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestHMACCutoverCatalogPreservesDecisionAndClosureBoundaries(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "### 2.24 Stored-contract HMAC cutover compatibility")
	end := strings.Index(catalog, "### 2.25 Egress-prober derived-client retirement")
	if start < 0 || end <= start {
		t.Fatal("SIGNALS.md §2.24 section boundaries are missing")
	}
	section := strings.Join(strings.Fields(catalog[start:end]), " ")
	for _, want := range []string{
		"three-hour lifetime expires",
		"deliberately fails open",
		"not a durable quarantine",
		"Never permanently exclude a provider from its self-reported description alone",
		"uniformly build every API and Connect signer from one reviewed shared Connect policy source",
		"Rebuild and safely promote the monitor from that same policy source",
		"time-bounded risk acceptance with a named sunset, not final closure",
		"API-only or Connect-only rollout is inconsistent",
		"temporary compatibility availability gate does not close the protocol boundary",
		"upgrade-and-sunset obligation",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("SIGNALS.md §2.24 omits %q", want)
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

func TestHMACCutoverSignalSyntheticControlDegradationDoesNotClaimRecovery(t *testing.T) {
	// The legacy cohort independently meets its dark threshold, but the
	// compatible control is one observation below 50%. Current causal
	// attribution must fail closed without implying that an earlier causal
	// sample recovered.
	row := Row{"t", "86400", "1000", "300", "20", "200", "500", "100", "100", "0", "100", "51", "49"}
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
		"compatible_ok=49",
		"fails closed to readiness",
		"cannot retroactively negate an earlier behaviorally confirmed incompatibility",
		"PAGE-to-WARN class reassignment is not recovery",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("control-degradation alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "reject each newly created contract") {
		t.Fatalf("insufficient current control was given the causal diagnosis:\n%s", markdown)
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

// A verdict is what the dark rule of connect/GEOMAP.md §11.3 says it is: a
// current pass, or a provider dark over consecutive failures (or a TLS
// failure); a single failed check and a check that measured nothing are no
// verdict and join neither cohort's checked count, so a legacy cohort cannot
// look dark on failures the rule does not call dark.
func TestHMACCutoverQueryCountsOnlyCurrentVerdicts(t *testing.T) {
	query := hmacCutoverQuery()
	rules := model.DefaultProviderEgressRules()
	for _, want := range []string{
		"COALESCE(pbc.ok, false) AS ok",
		"AND (pbc.ok OR " + model.ProviderBlackholeDarkSql("pbc", "clock.utc_now - interval '10800 seconds'", rules) + ")",
		fmt.Sprintf("%d <= pbc.consecutive_failures", rules.DarkConsecutiveFailures),
		"pbc.failure = '" + model.ProviderBlackholeTlsAuthenticationFailure + "'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "           pbc.ok,\n") {
		t.Fatal("the query still reads a single failed check as dark")
	}
}
