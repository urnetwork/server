// Subnet catalog checks retain the required detection contract while its
// observers are implemented separately. They do not simulate on-chain probes.
package monitor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Isolates the numbered specification without depending on later additions.
func subnetCorrectnessDocumentation(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## 22. Subnet continuous correctness — SUBNET1"
	start := strings.Index(string(data), heading)
	if start < 0 {
		t.Fatal("SIGNALS.md is missing subnet correctness section 22")
	}
	section := string(data)[start:]
	if next := strings.Index(section[1:], "\n## "); next >= 0 {
		section = section[:next+1]
	}
	return section
}

// Every reserved class has its own nonempty trip condition and response;
// duplicate identifiers or lost economic channels cannot pass a generic count.
func TestSubnetCorrectnessDocumentationCoversRequiredAlertClasses(t *testing.T) {
	t.Parallel()
	section := subnetCorrectnessDocumentation(t)
	classes := []string{
		"subnet-runtime-identity", "subnet-finality-divergence", "subnet-deployment-governance", "subnet-policy-schedule",
		"subnet-pool-custody-identity", "subnet-validator-eligibility", "subnet-fleet-binding-invalid", "subnet-commitment-mirror-lag",
		"subnet-deposit-attribution", "subnet-operator-deposit-mismatch", "subnet-deposit-penalty-bypass", "subnet-reserve-principal", "subnet-reserve-credit-delta", "subnet-reserve-yield",
		"subnet-path-proof-invalid", "subnet-verify-poisoning", "subnet-attempt-history-gap", "subnet-measurement-coverage", "subnet-pool-quality-mismatch", "subnet-egress-attribution", "subnet-measurement-bias",
		"subnet-head-selection", "subnet-head-weight-bypass", "subnet-promotion-churn", "subnet-tier-double-pay", "subnet-head-native-payout", "subnet-promotion-economics",
		"subnet-weight-vector", "subnet-crv4-lifecycle", "subnet-consensus-divergence", "subnet-validator-native-payout",
		"subnet-epoch-progress", "subnet-capture-delta", "subnet-payout-root", "subnet-claim-correctness", "subnet-claim-credit-payment", "subnet-carry-isolation", "subnet-vault-conservation",
		"subnet-artifact-integrity", "subnet-evidence-anchor", "subnet-history-availability", "subnet-audit-replay-gap",
		"subnet-runtime-liveness", "subnet-state-durability", "subnet-capacity-margin", "subnet-rpc-read-ownership", "subnet-observation-quota", "subnet-evidence-stream-capacity",
		"subnet-writer-funding", "subnet-adversarial-resilience", "subnet-monitor-coverage",
	}
	rowPattern := regexp.MustCompile("(?m)^\\| `(subnet-[a-z0-9]+(?:-[a-z0-9]+)*)` \\| ([^|]+) \\| ([^|]+) \\|$")
	// Numeric segments are valid registry keys; empty segments, aliases and
	// noncanonical labels must not become documentation rows accidentally.
	for _, example := range []struct {
		key   string
		valid bool
	}{
		{key: "subnet-crv4-lifecycle", valid: true},
		{key: "subnet-2fa", valid: true},
		{key: "subnet-weight-vector", valid: true},
		{key: "subnet--crv4", valid: false},
		{key: "subnet-crv4-", valid: false},
		{key: "subnet-CRV4-lifecycle", valid: false},
		{key: "subnet-crv4_lifecycle", valid: false},
		{key: "subnet-crv4 lifecycle", valid: false},
		{key: "subnet-", valid: false},
	} {
		row := "| `" + example.key + "` | detected mismatch | Page: investigate |"
		if rowPattern.MatchString(row) != example.valid || signalKeyPattern.MatchString(example.key) != example.valid {
			t.Errorf("subnet documentation key %q differs from registry grammar; want valid=%t", example.key, example.valid)
		}
	}
	rows := rowPattern.FindAllStringSubmatch(section, -1)
	if len(rows) != len(classes) {
		t.Fatalf("subnet alert rows=%d, want %d complete rows", len(rows), len(classes))
	}
	seen := map[string]bool{}
	// Each newly reserved class must keep its own authority/resource distinction;
	// a generic nonempty row or a phrase in another class cannot satisfy it.
	requiredRules := map[string][]string{
		"subnet-rpc-read-ownership":       {"same deployment, runtime, transport and exact finalized block hash", "Every client's nonce, signature and complete history", "Http admissions, logical methods", "not fabricated empty history"},
		"subnet-observation-quota":        {"full configured client population", "Every plural member is charged independently", "including failed initial admission", "batching is not one logical reservation"},
		"subnet-evidence-stream-capacity": {"both configured content/history replicas", "data, control/graph metadata and active body/reader ownership separately bounded", "must not become a whole-corpus allocation", "never truncate it to clear the alert"},
	}
	for _, row := range rows {
		if seen[row[1]] {
			t.Errorf("duplicate alert class %s", row[1])
		}
		seen[row[1]] = true
		for _, required := range requiredRules[row[1]] {
			if !strings.Contains(row[2]+" "+row[3], required) {
				t.Errorf("alert %s lost its own required rule %q", row[1], required)
			}
		}
		if strings.TrimSpace(row[2]) == "" || strings.TrimSpace(row[3]) == "" ||
			!(strings.Contains(row[3], "Page") || strings.Contains(row[3], "Warn")) {
			t.Errorf("alert %s lacks a trip condition/severity/action", row[1])
		}
	}
	for _, class := range classes {
		if !seen[class] {
			t.Errorf("missing required subnet alert %s", class)
		}
	}
}

// Pins the distinctions which prevent plausible but false healthy verdicts.
func TestSubnetCorrectnessDocumentationRetainsEvidenceAndRecoveryRules(t *testing.T) {
	t.Parallel()
	section := strings.Join(strings.Fields(subnetCorrectnessDocumentation(t)), " ")
	for _, required := range []string{
		"**healthy:**", "**violated:**", "**unknown:**", "**not deployed:**",
		"200 **fleets**, not 200 clients", "same height/hash", "50,400 blocks",
		"effective signed policy", "complete configured operator and validator census",
		"Both underdeposit and overdeposit are mismatches", "zero_pool_weight",
		"not a trustless revenue/usage oracle", "each validator independently",
		"outside that validator's selected list receives zero/absent head weight",
		"selected, live, eligible, non-self claimant", "from an older valid cycle",
		"both completed-proof signatures", "history fabricated from successful trails alone",
		"head clients are absent from every NO tail artifact", "10,000-bps allocation",
		"`Claimed` means logical credit", "`ClaimPaymentDeferred` means unpaid durable credit",
		"`ClaimPaid` requires measured escrow decrease/recipient increase",
		"already-claimed credit is excluded", "survives expiry",
		"totalCaptured = totalPaid + escrowAccounted",
		"escrowAccounted = pendingFunding + outstandingLiability",
		"liveEscrowStake >= escrowAccounted", "liveReserveStake >= principal",
		"server API backed by server/blob MinIO", "A digest is not full proof bytes",
		"not proof of on-chain inclusion", "not independent storage failure domains",
		"No validator effort bounty", "stronger `VALIDATOR.md` §10 defenses remain outside v1",
		"All probes are **read-only**", "one shared endpoint RPC budget",
		"never blanket alert suppression", "never delete journals",
		"not a passive probe", "raw egress IPs", "connect/CODESTYLE.md",
		"five-minute recovery window **and** reconciliation of missed work",
		"counterexamples remain open until independently replayed",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("subnet monitoring contract is missing %q", required)
		}
	}
}

// A runbook addition cannot silently advertise unregistered automated probes.
func TestSubnetCorrectnessDocumentationLabelsUnimplementedCoverage(t *testing.T) {
	t.Parallel()
	section := subnetCorrectnessDocumentation(t)
	if len(catalogProbePattern.FindAllStringSubmatch(section, -1)) != 0 {
		t.Fatal("prospective subnet catalog declares an implemented Probe")
	}
	compact := strings.Join(strings.Fields(section), " ")
	for _, required := range []string{
		"required monitoring specification; new subnet probes are not implemented or registered by this documentation change",
		"reserved alert classes, not `Probe:` declarations or metric names",
		"explicit coverage gap, never a green result from an absent metric",
		"proposed helper or authored test is not a deployed signal",
		"This documentation has coverage regression tests, **not** an implemented or qualified subnet probe battery",
	} {
		if !strings.Contains(compact, required) {
			t.Errorf("subnet implementation status is missing %q", required)
		}
	}
}
