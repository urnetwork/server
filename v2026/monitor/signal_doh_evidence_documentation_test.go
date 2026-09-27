// Resolver-call evidence must not inherit the meaning of a dial-leg event or
// a container label. These checks preserve the catalog's authority boundary.
package monitor

import (
	"os"
	"strings"
	"testing"
)

// Read only the owning playbook; a qualifier elsewhere cannot satisfy it.
func dohEvidenceCatalogSection(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(data), "### 5.2 Node wedge")
	if start < 0 {
		t.Fatal("owning DoH evidence section is absent")
	}
	section := string(data)[start:]
	if end := strings.Index(section, "\n### "); end >= 0 {
		section = section[:end]
	}
	return strings.Join(strings.Fields(section), " ")
}

// Canceled is not a benign-only outcome, and an absent timeout increment is
// not proof that the outer dial's independent deadline remained live.
func TestDohCatalogRetainsOuterDeadlineQualifier(t *testing.T) {
	section := dohEvidenceCatalogSection(t)
	for _, want := range []string{
		"`canceled` can also follow an outer Tun dial deadline",
		"`timeout=0` therefore cannot exclude that failure",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("DoH evidence contract omits %q", want)
		}
	}
}

// Positive no-answer observations retain visibility without selecting a cause
// or treating a merely matched log envelope as process-generation proof.
func TestDohCatalogRetainsNoAnswerAndPairAuthority(t *testing.T) {
	section := dohEvidenceCatalogSection(t)
	for _, want := range []string{
		"Positive `failed` deltas establish no-answer resolver calls",
		"same-call evidence",
		"a same-container log envelope is not independent process-start attestation",
		"a per-leg PAGE alone therefore cannot prove whether the handoff fix worked or failed",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("DoH evidence contract omits %q", want)
		}
	}
}
