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
		"Terra and Astra may prepare manifests; they do not append",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
	if strings.Contains(documentation, "The ledger has one writer: the long-lived Terra runner") {
		t.Fatal("RUN-MAIN.md assigns the primary ledger to Terra")
	}
}

func TestRunMainAssignsRequestedMonitorModels(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"A `gpt-5.6-terra` agent at `max` reasoning owns monitor execution",
		"A `gpt-6-astra` agent at `max` reasoning owns diagnosis and repair",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
	if strings.Contains(documentation, "gpt-5.6-sol") {
		t.Fatal("RUN-MAIN.md still assigns the diagnosis role to Sol")
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

func TestMonitorDocumentationDistinguishesActiveAlertsFromLegacyEvents(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"MONITOR.md", "SIGNALS.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		documentation := strings.Join(strings.Fields(string(data)), " ")
		for _, required := range []string{
			"The current CLI emits active Alerts, not ticket lifecycle events.",
			"The current CLI does not emit an all-probes heartbeat.",
			"Silence is not recovery.",
		} {
			if !strings.Contains(documentation, required) {
				t.Errorf("%s does not distinguish the current output contract: missing %q", path, required)
			}
		}
	}
}

func TestRunMainRequiresRetainedReconciliationReceipts(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"`monitor-log-reconcile `",
		"schema-1 JSON from the current watcher generation's stderr artifact",
		"collectors=enabled=fresh=consecutive_two > 0",
		"collector_started_at",
		"previous_window_start",
		"latest_window_start",
		"previous_completed_at",
		"latest_completed_at",
		"never combine predecessor and candidate histories",
		"alert absence alone is insufficient",
		"does not change alert-only JSONL",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md lost reconciliation evidence contract %q", required)
		}
	}
}
