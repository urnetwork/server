package monitor

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A half-passing control excludes a universal outage, not every shared or
// cohort-specific fault. Preserve the existing PAGE at its exact boundary.
func TestHmacCutoverQualifierDoesNotOverclaimPartialControl(t *testing.T) {
	row := Row{"t", "86400", "300", "100", "2", "100", "100", "100", "100", "0", "100", "50", "50"}
	alerts, err := NewHMACCutoverSignal().Run(context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)))
	if err != nil || len(alerts) != 1 {
		t.Fatal("the exact PAGE control did not produce one finding")
	}
	alert := requireAlertClass(t, alerts, "contract-hmac-incompatible")
	if alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatal("wording-only correction changed PAGE semantics")
	}
	text := strings.Join(strings.Fields(alert.Markdown()), " ")
	for _, expected := range []string{
		"exclude only a universal shared-path failure",
		"do not rule out concurrent or cohort-specific",
		"does not attest a running receiver or signer",
		"successful tunnel construction does not establish later API, contract, or route usability",
		"not necessarily zero bytes",
		"Preserve separate integrity failures",
		"unknown=100", "legacy_dark=100", "compatible_ok=50",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("partial control overstates cause or loses its discriminator: %q", expected)
		}
	}
	if strings.Contains(text, "ruling out the shared prober tunnel and API path") {
		t.Error("a partially passing control was treated as proof against all shared faults")
	}
}

// Change only the compatible pass share; the same all-dark legacy cohort must
// retain the original class thresholds without being called receiver recovery.
func TestHmacCutoverQualifierKeepsThresholdAndHealthyControls(t *testing.T) {
	for _, state := range []struct {
		dark  string
		pass  string
		class string
	}{
		{dark: "51", pass: "49", class: "contract-hmac-readiness"},
		{dark: "50", pass: "50", class: "contract-hmac-incompatible"},
	} {
		row := Row{"t", "86400", "300", "100", "2", "100", "100", "100", "100", "0", "100", state.dark, state.pass}
		alerts, err := NewHMACCutoverSignal().Run(context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)))
		if err != nil || len(alerts) != 1 {
			t.Fatal("threshold transition did not produce exactly one finding")
		}
		alert := requireAlertClass(t, alerts, state.class)
		if !strings.Contains(alert.Observed, "legacy_dark=100") || !strings.Contains(alert.Observed, "legacy_ok=0") {
			t.Fatal("unchanged legacy evidence disappeared during class reassignment")
		}
		if state.class == "contract-hmac-readiness" && !strings.Contains(alert.Context, "not an HMAC-specific or runtime-attestation discriminator") {
			t.Error("readiness still promises exact attribution from an aggregate pattern")
		}
	}
	row := Row{"t", "86400", "100", "0", "0", "100", "0", "0", "0", "0", "100", "0", "100"}
	alerts, err := NewHMACCutoverSignal().Run(context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)))
	if err != nil || len(alerts) != 0 {
		t.Fatal("healthy no-legacy control changed")
	}
}

// Document both directions of uncertainty without changing signing policy,
// claiming current clock contamination, or authorizing permanent removal.
func TestHmacCutoverQualifierCatalogKeepsEvidenceBoundaries(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(data), "### 2.24 Stored-contract HMAC cutover compatibility")
	end := strings.Index(string(data), "### 2.25 Egress-prober derived-client retirement")
	if start < 0 || end <= start {
		t.Fatal("catalog section boundaries missing")
	}
	text := strings.Join(strings.Fields(string(data)[start:end]), " ")
	for _, expected := range []string{
		"False-positive qualifiers:", "False-negative qualifiers:",
		"exclude only a universal shared-path failure",
		"does not establish later API, contract, or route usability",
		"not necessarily zero bytes",
		"no upper DB-time bound",
		"unknown descriptions can conceal legacy receivers",
		"does not by itself prove new legacy failure onset",
		"Never permanently exclude a provider from its self-reported description alone",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("catalog misses evidence boundary %q", expected)
		}
	}
}
