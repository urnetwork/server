// Echo diagnostics distinguish committed source from deployed and in-flight
// evidence without changing the owning monitor's observation contract.
package monitor

import (
	"os"
	"strings"
	"testing"
)

// Only the owning admission section can satisfy these source/authority limits;
// nearby outcome or site-pool prose must not hide a stale admission claim.
func TestEgressEchoCatalogKeepsSourceAndCompletionBoundaries(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	start := strings.Index(catalog, "### 2.19a ")
	end := strings.Index(catalog, "### 2.19b ")
	if start < 0 || end <= start {
		t.Fatal("owning egress-admission catalog section is missing")
	}
	section := strings.Join(strings.Fields(catalog[start:end]), " ")
	for _, expected := range []string{
		"Operator Proxy `7ce5f03`",
		"committed in source but not yet deployed to Main",
		"fixed `echo_stage` and `error_class`",
		"completed no-exit results",
		"not an in-flight stage observation",
		"DNS and socket dialing remain combined",
		"contract/window readiness remains unknown",
		"cancellation before the final result can omit the echo detail",
		"no new metric families or labels",
		"running Taskworker artifact",
	} {
		if !strings.Contains(section, expected) {
			t.Errorf("echo diagnostic catalog omitted evidence boundary %q", expected)
		}
	}
	if strings.Contains(section, "The prober no longer records the stage an attempt failed at") {
		t.Error("catalog still denies the committed echo-stage diagnostic")
	}
}
