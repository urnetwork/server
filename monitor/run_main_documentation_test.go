package monitor

import (
	"os"
	"strings"
	"testing"
)

func runMainDocumentation(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("RUN-MAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

func TestRunMainRequiresDailyThreeWayImprovementResearch(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"Daily three-way improvement research",
		"at least once per UTC day",
		"catalog → implementation",
		"implementation → catalog",
		"ledger → catalog and implementation",
		"A live incident discovery triggers this reconciliation immediately",
		"does not defer an active incident",
		"promote the watcher through the safe handoff",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
}

func TestRunMainKeepsOnePrimaryLedgerWriter(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"The primary agent owns the append-only run ledger",
		"The ledger has one writer: the primary agent",
		"Terra and Sol may prepare manifests; they do not append",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
	if strings.Contains(documentation, "The ledger has one writer: the long-lived Terra runner") {
		t.Fatal("RUN-MAIN.md assigns the primary ledger to Terra")
	}
}

func TestRunMainRetainsWholeHostScopeSafety(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"-exclude-host HOSTNAME",
		"immutable transport policy",
		"retain desired topology, service blocks, and expected denominators",
		"monitor-host-scope-partial",
		"explicitly unknown",
		"operator reason, owner, UTC start, and re-enable condition",
		"Empty, wildcard, unknown, or ambiguous names fail closed",
		"settings-freshness reload",
		"`-exclude-signal` excludes only probe constructors",
		"helper proves its own scope only",
		"controlled handoff",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
}
