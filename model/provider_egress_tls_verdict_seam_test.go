// The seam the TLS finding behaviour tests (provider_egress_tls_verdict_db_test.go)
// drive the judge through. The behaviour tests use no other symbol the judge
// added, so they read the same against code with no judge at all.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The URL common-mode guard's network count (taskworker/work
// providerUrlProbeCommonModeNetworkCount), which the taskworker passes in.
const testingTlsCommonModeNetworkCount = 16

// Judges every unresolved finding as one run of the taskworker's judge does:
// served names the destinations the pool serves, direct maps a host to its
// direct check ("verified" or "failed"; an absent host is inconclusive), and
// the windows and shares are the site settings' defaults.
func testingJudgeTlsFindings(t testing.TB, ctx context.Context, now time.Time, served []string, direct map[string]string) {
	t.Helper()
	servedSet := map[string]bool{}
	for _, name := range served {
		servedSet[name] = true
	}
	outcomes := map[string]ProviderEgressTlsDirectOutcome{}
	for host, outcome := range direct {
		switch outcome {
		case "verified":
			outcomes[host] = ProviderEgressTlsDirectVerified
		case "failed":
			outcomes[host] = ProviderEgressTlsDirectFailed
		default:
			t.Fatalf("unknown direct outcome %q", outcome)
		}
	}
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	findings := GetProviderEgressTlsFindings(ctx, 100000)
	SetProviderEgressTlsVerdicts(ctx, JudgeProviderEgressTlsFindings(ctx, now, findings, servedSet, outcomes, settings))
}

// The stored verdict of clientId's unresolved finding on target ("pending",
// "provider", "site", "place" or "retired"), "" with no unresolved finding.
func testingTlsVerdict(t testing.TB, ctx context.Context, clientId server.Id, target string) string {
	t.Helper()
	for _, finding := range GetProviderEgressTlsFindings(ctx, 100000) {
		if finding.ClientId == clientId && finding.Url == target {
			return finding.Verdict.String()
		}
	}
	return ""
}
