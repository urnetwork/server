// Source-tested ownership and publication clocks must not become unsupported
// claims about a deployed artifact or a particular provider failure.
package monitor

import (
	"os"
	"strings"
	"testing"
)

func egressRequestOwnerCatalogSection(t *testing.T, first string, next string) string {
	t.Helper()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	start := strings.Index(catalog, first)
	end := strings.Index(catalog, next)
	if start < 0 || end <= start {
		t.Fatal("owning catalog section is absent")
	}
	return strings.Join(strings.Fields(catalog[start:end]), " ")
}

func TestEgressRequestOwnerCatalogRetainsDeploymentAndCauseLimits(t *testing.T) {
	section := egressRequestOwnerCatalogSection(t, "### 2.19a ", "### 2.19b ")
	for _, expected := range []string{
		"context.WithoutCancel", "TestProbeHttpRequestOwner*",
		"six full-health fetch slots admitted 18 overlapping synthetic dials",
		"joins admitted dial work before release", "response-body lifetime",
		"WebPKI/pins", "existing guard semantics",
		"A live-path deadline remains measured failure; genuine path loss remains NotMeasured",
		"not a proven cause of Main's broad HTTP/load failures",
		"not a measured live amplification factor", "same-attempt logical outcomes",
		"Deployment of this correction is not yet verified",
		"adds no watcher query, metric family, label or new alert identity",
	} {
		if !strings.Contains(section, expected) {
			t.Errorf("request-owner catalog omitted %q", expected)
		}
	}
}

func TestEgressCheckStartCatalogRejectsPublicationCutoffInference(t *testing.T) {
	section := egressRequestOwnerCatalogSection(t, "### 2.19 ", "### 2.19a ")
	for _, expected := range []string{
		"check-start can precede a rollout-time cutoff",
		"Zero `checked_at >= cutoff` alone therefore does not prove zero measured reports",
		"bounded `update_time` publication cohorts",
		"stored verdict classes can predate a NotMeasured upsert and are not the new submitted payload",
		"Neither a batch acknowledgement nor matching row counts establishes a same-attempt or exact-artifact join",
	} {
		if !strings.Contains(section, expected) {
			t.Errorf("blackhole publication catalog omitted %q", expected)
		}
	}
}
