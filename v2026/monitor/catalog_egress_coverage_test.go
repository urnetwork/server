// Catalog and filename boundaries distinguish an owning probe, its helper,
// and a still-unimplemented measurement contract without inventing health.
package monitor

import (
	"os"
	"strings"
	"testing"
)

// Metric recovery belongs to the existing site-pool signal, not a new signal
// manufactured from a helper filename. The registry's generic check stays strict.
func TestEgressSitePoolMetricRecoveryHasSingleOwningSignal(t *testing.T) {
	owners := 0
	for _, signal := range NewSignals() {
		switch signal.Key() {
		case "egress-site-pool":
			owners++
		case "egress-site-pool-metrics":
			t.Fatal("metric helper was registered as independent health coverage")
		}
	}
	if owners != 1 {
		t.Fatal("site-pool metric recovery lacks one owning registered signal")
	}
	if _, err := os.Stat("signal_egress_site_pool_metrics.go"); !os.IsNotExist(err) {
		t.Fatal("metric helper occupies the standalone signal filename namespace")
	}
	source, err := os.ReadFile("egress_site_pool_metrics.go")
	if err != nil || !strings.Contains(string(source), "SIGNALS.md §2.19b") {
		t.Fatal("metric helper lost its source and owning catalog link")
	}
}

// A future real implementation can replace the planned state, but an absent
// producer must not be hidden by a Probe declaration or a placeholder registry entry.
func TestCountrySiteCatalogCoverageCannotClaimUnregisteredProbe(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	start := strings.Index(catalog, "### 2.19d ")
	if start < 0 {
		t.Fatal("country-site acceptance contract disappeared")
	}
	section := catalog[start:]
	if next := strings.Index(section[1:], "\n### "); next >= 0 {
		section = section[:next+1]
	}
	registered := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "egress-country-sites" {
			registered++
		}
	}
	coverage := catalogMarkerSection(t, catalog, "<!-- numbered-coverage-start -->", "<!-- numbered-coverage-end -->")
	gap := strings.Contains(coverage, "| 2.19d | Coverage gap |")
	switch registered {
	case 0:
		if catalogProbePattern.MatchString(section) || !gap || !strings.Contains(section, "Planned probe: `egress-country-sites`") {
			t.Fatal("unregistered country-site contract is not an explicit planned coverage gap")
		}
		for _, required := range []string{
			"no registered runtime probe currently implements",
			"country-list provenance/verification records",
			"generation-linked split-run/skip",
			"not healthy country coverage",
		} {
			if !strings.Contains(section, required) {
				t.Error("planned country-site coverage omitted a source-authority prerequisite")
			}
		}
	case 1:
		if gap || !strings.Contains(section, "\nProbe: `egress-country-sites`\n") {
			t.Fatal("registered country-site implementation and catalog coverage disagree")
		}
	default:
		t.Fatal("country-site probe was registered more than once")
	}
}

// Labeling missing implementation must not lower the accepted country sample,
// discard source freshness, or turn repeated retained skips into new attempts.
func TestCountrySiteCatalogRetainsAcceptanceControls(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(data), "### 2.19d ")
	if start < 0 {
		t.Fatal("country-site acceptance contract disappeared")
	}
	section := string(data[start:])
	if next := strings.Index(section[1:], "\n### "); next >= 0 {
		section = section[:next+1]
	}
	for _, required := range []string{
		"13 general destinations and 13",
		"100-site target for both the country and global lists",
		"two distinct consecutive probe cadences",
		"advancing attempt ids or counters",
		"cannot emit healthy evidence",
		"Region-specific hard exclusions",
		"does not assert that the current\nMain watcher",
	} {
		if !strings.Contains(section, required) {
			t.Error("country-site coverage labeling weakened an acceptance control")
		}
	}
}
