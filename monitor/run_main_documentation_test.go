package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Execute the operator's documented selector with synthetic lsof records only;
// no live process census, installed Warpctl, or monitor transport is involved.
func runMainImageSelector(t *testing.T, fixture string, withoutAnd bool) (string, error) {
	t.Helper()
	data, err := os.ReadFile("RUN-MAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "```sh\n# monitor-warpctl-image-selector\n"
	_, remaining, found := strings.Cut(string(data), marker)
	if !found {
		t.Fatal("RUN-MAIN.md is missing its PID-scoped executable-image selector")
	}
	command, _, found := strings.Cut(remaining, "\n```")
	if !found {
		t.Fatal("RUN-MAIN.md executable-image selector is unterminated")
	}
	if withoutAnd {
		command = strings.Replace(command, "-a -p", "-p", 1)
	}
	binDirectory := t.TempDir()
	const syntheticLsof = `#!/bin/sh
case "$*" in
  '-nP -a -p 4242 -d txt -Fpcfn') ;;
  '-nP -p 4242 -d txt -Fpcfn') printf 'p4343\ncwarpctl\nftxt\nn/synthetic/decoy/warpctl\n' ;;
  *) printf 'unexpected synthetic lsof arguments\n' >&2; exit 99 ;;
esac
case "$MONITOR_IMAGE_FIXTURE" in
  missing) exit 1 ;;
  foreign) printf 'p4343\ncwarpctl\nftxt\nn/synthetic/decoy/warpctl\n'; exit 0 ;;
  wrong-fd) printf 'p4242\ncwarpctl\nf3\nn/synthetic/validated/warpctl\n'; exit 0 ;;
  orphan-name) printf 'ftxt\nn/synthetic/validated/warpctl\n'; exit 0 ;;
esac
printf 'p4242\ncwarpctl\nftxt\nn/synthetic/validated/warpctl\nftxt\nn/synthetic/libobserver.dylib\n'
case "$MONITOR_IMAGE_FIXTURE" in
  ambiguous) printf 'ftxt\nn/synthetic/another/warpctl\n' ;;
  command-error) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDirectory, "lsof"), []byte(syntheticLsof), 0o700); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, bash, "-c", command)
	process.Env = []string{
		"PATH=" + binDirectory + string(os.PathListSeparator) + filepath.Dir(awk),
		"monitor_tail_pid=4242",
		"monitor_image_evidence=" + filepath.Join(binDirectory, "image-evidence.txt"),
		"MONITOR_IMAGE_FIXTURE=" + fixture,
	}
	output, err := process.CombinedOutput()
	return string(output), err
}

// A target's text image must win by PID and descriptor association, not order
// among executable-looking names from the target and an unrelated decoy.
func TestRunMainImageSelectorKeepsExactTarget(t *testing.T) {
	output, err := runMainImageSelector(t, "target", false)
	if err != nil || output != "/synthetic/validated/warpctl\n" {
		t.Fatalf("target selection = %q, %v", output, err)
	}
	if output, err := runMainImageSelector(t, "target", true); err == nil {
		t.Fatalf("unscoped decoy/target records were accepted: %q", output)
	}
}

// Unknown, malformed, ambiguous, or failed observations cannot certify an
// image even if one plausible executable name remains in the output.
func TestRunMainImageSelectorRejectsUnknownEvidence(t *testing.T) {
	for _, fixture := range []string{"missing", "foreign", "wrong-fd", "orphan-name", "ambiguous", "command-error"} {
		if output, err := runMainImageSelector(t, fixture, false); err == nil {
			t.Fatalf("%s evidence was accepted: %q", fixture, output)
		}
	}
}

// Preserve the operator's evidence boundary independently of the selector.
func TestRunMainRequiresPidScopedExecutableEvidence(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"lsof ORs selection options unless `-a` is present",
		"PID and `txt` association",
		"missing or ambiguous result is unknown",
		"parent ownership and process start identity",
		"Never select an image with `head -n1`",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md lost executable-image evidence boundary %q", required)
		}
	}
}

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
		"Sol and Astra may prepare manifests; they do not append",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
	if strings.Contains(documentation, "The ledger has one writer: the long-lived Sol runner") ||
		strings.Contains(documentation, "The ledger has one writer: the long-lived Terra runner") {
		t.Fatal("RUN-MAIN.md assigns the primary ledger to a test runner")
	}
}

func TestRunMainAssignsRequestedMonitorModels(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"A `gpt-6-sol` agent at `medium` reasoning (\"Sol Medium\") owns monitor execution",
		"monitor-test gates",
		"bounded read-only failure fact collection, initial triage",
		"A `gpt-6-astra` agent at `max` reasoning (\"Astra Max\") owns all",
		"Astra consumes Sol's initial",
		"every self-improvement repair to a probe, shared monitoring utility, signal catalog, or this harness",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain %q", required)
		}
	}
	if strings.Contains(documentation, "gpt-5.6-terra") {
		t.Fatal("RUN-MAIN.md still assigns the test and initial-triage role to Terra")
	}
}

func TestRunMainUsesAttestedLocalSequentialMonitorGates(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"`WARP_ENV=main` selects the production probe target; it is not a Go-test environment",
		"separate attested Bash subshells",
		"setting bare `WARP_ENV=local` is not equivalent",
		"Do not overlap the package and race gates",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md does not retain attested test-gate guidance %q", required)
		}
	}
	if got := strings.Count(documentation, "unset WARP_ENV; source ./test-env.sh && exec go test"); got != 5 {
		t.Fatalf("attested monitor test command count = %d, want 5", got)
	}
}

func TestMonitorDocumentationRequiresEvidenceBackedErrorQualifiers(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"RUN-MAIN.md", "SIGNALS.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		documentation := strings.Join(strings.Fields(string(data)), " ")
		for _, required := range []string{
			"false-positive",
			"false-negative",
			"healthy control",
			"unknown or `cannot-observe`",
			"deterministic test",
			"NUL-safe",
			"empty optional",
		} {
			if !strings.Contains(strings.ToLower(documentation), strings.ToLower(required)) {
				t.Errorf("%s does not retain qualifier guidance %q", path, required)
			}
		}
	}
}

// Keeps actual probe traffic distinct from report acceptance and conditional
// failure diagnostics, without promoting either aggregate to a per-pass join.
func TestMonitorDocumentationEgressEvidenceBoundaries(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "### 2.19a ")
	if !found {
		t.Fatal("SIGNALS.md lost the egress admission section")
	}
	section, _, found = strings.Cut(section, "\n### 2.20 ")
	if !found {
		t.Fatal("SIGNALS.md lost the egress admission section boundary")
	}
	documentation := strings.Join(strings.Fields(section), " ")
	for _, required := range []string{
		"`urnetwork_egress_probe_health_checks_total{result=\"ok\"}` records a validated fetch",
		"status/body contract passed before the health report API call",
		"not a byte counter or a report acknowledgment",
		"fresh same-process positive delta proves carried probe traffic",
		"not a join to an individual failed geolocation source",
		"later success cannot exclude an earlier formation failure",
		"`urnetwork_egress_probe_geolocation_diagnostics_total` contains source outcomes only from diagnostic-bearing `no_consensus` probes",
		"including any source that succeeded within that failed probe",
		"not an all-source request denominator or a fleet-wide DNS/TLS failure rate",
		"Missing or newly created auxiliary series are unknown, not healthy zero",
		"not added to the fixed admission query or its alert thresholds",
		"fallback includes manual TLS inside the custom `DialTLSContext`",
		"timeout at this stage is not a DNS-specific verdict",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("SIGNALS.md lost the egress evidence boundary %q", required)
		}
	}
}

func TestMonitorDocumentationRejectsMissingPredecessorTransitions(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"RUN-MAIN.md", "SIGNALS.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		documentation := strings.Join(strings.Fields(string(data)), " ")
		for _, required := range []string{
			"authoritative predecessor",
			"prior severity",
			"identity as new",
			"incomplete or unsealed",
			"new=1, transitions=0",
			"new=0, transitions=1",
		} {
			if !strings.Contains(documentation, required) {
				t.Errorf("%s lost exact-identity transition guidance %q", path, required)
			}
		}
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

// Dependency identity alone does not prove compatibility with current settings;
// keep the local failure discriminator and recovery boundary in both owners.
func TestMonitorDocumentationRequiresObserverSchemaCompatibility(t *testing.T) {
	t.Parallel()
	for _, document := range []struct {
		path, start, end string
	}{
		{path: "RUN-MAIN.md", start: "## Start the authoritative continuous watcher", end: "## Safe watcher promotion"},
		{path: "SIGNALS.md", start: "### 1.5 ", end: "### 1.6 "},
	} {
		data, err := os.ReadFile(document.path)
		if err != nil {
			t.Fatal(err)
		}
		_, section, found := strings.Cut(string(data), document.start)
		if !found {
			t.Fatalf("%s lost its observer documentation section", document.path)
		}
		section, _, found = strings.Cut(section, document.end)
		if !found {
			t.Fatalf("%s lost its observer documentation boundary", document.path)
		}
		documentation := strings.ToLower(strings.Join(strings.Fields(section), " "))
		for _, required := range []string{
			"effective `services.yml` schema",
			"pre-query",
			"not a loki outage",
			"exit status 2 alone",
			"long-lived tails",
			"fresh reconciliation children",
			"raw child output private",
			"existing local warp checkout",
			"controlled handoff",
			"two fresh same-generation reconciliation windows",
			"complete intended collector inventory",
		} {
			if !strings.Contains(documentation, required) {
				t.Errorf("%s lost observer compatibility guidance %q", document.path, required)
			}
		}
	}
}

func TestRunMainPromotesCompleteMarkdownDespiteProbeFailure(t *testing.T) {
	t.Parallel()
	documentation := runMainDocumentation(t)
	for _, required := range []string{
		"monitor_exit=0",
		"|| monitor_exit=$?",
		"monitor's probe exit status",
		"complete report can contain a `monitor/visibility` Alert",
		"rg -qx '<!-- monitor-alerts-complete -->'",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("RUN-MAIN.md lost completed-output promotion contract %q", required)
		}
	}
}
