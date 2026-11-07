// The TLS judge's decision rule (provider_egress_tls_verdict.go), without a
// database.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Runs the real judge as it decides when every finding's URL verifies
// directly from the server and nothing common-mode or place-level explains it:
// each fresh failure is the provider's own. For tests of what a finding does
// once judged, not of the judgement.
func testingJudgeTlsFindingsTheProvidersOwn(t testing.TB, ctx context.Context) {
	t.Helper()
	findings := GetProviderEgressTlsFindings(ctx, 100000)
	served := map[string]bool{}
	direct := map[string]ProviderEgressTlsDirectOutcome{}
	now := time.Time{}
	for _, finding := range findings {
		served[finding.Name] = true
		direct[finding.Host()] = ProviderEgressTlsDirectVerified
		if now.Before(finding.MeasuredAt) {
			now = finding.MeasuredAt
		}
	}
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	SetProviderEgressTlsVerdicts(ctx, JudgeProviderEgressTlsFindings(ctx, now, findings, served, direct, settings))
}

// Every window and share is the existing convention it names.
func TestTlsJudgeSettingsAreTheProbeAndRefreshConventions(t *testing.T) {
	site := DefaultProviderEgressSiteSettings()
	settings := ProviderEgressTlsJudgeSettingsFor(site, testingTlsCommonModeNetworkCount)
	if settings.DirectWindow != time.Hour || settings.DirectWindow != site.SiteProberFaultWindow() {
		t.Errorf("direct window %s is not the prober-fault window", settings.DirectWindow)
	}
	if settings.PlaceWindow != 3*24*time.Hour || settings.PlaceWindow != site.SiteWindow() {
		t.Errorf("place window %s is not the site window", settings.PlaceWindow)
	}
	if settings.PlaceFailShare != 0.9 || settings.PlaceMinSamples != 30 || settings.PlaceWorkingShare != 0.5 {
		t.Errorf("place rule %v/%d/%v is not the refresh's incompatibility and retirement lines", settings.PlaceFailShare, settings.PlaceMinSamples, settings.PlaceWorkingShare)
	}
	if settings.CommonModeNetworkCount != 16 {
		t.Errorf("common-mode network count %d is not the URL common-mode guard's", settings.CommonModeNetworkCount)
	}
}

// The rule, case by case, at its boundaries.
func TestTlsJudgeDecidesEachFindingOnEvidenceCloseToIt(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-2 * time.Hour)
	working := providerEgressTlsPlaceEvidence{siteLoads: 30, siteFailures: 2, placeLoads: 1000, placeFailures: 50}
	blocked := providerEgressTlsPlaceEvidence{siteLoads: 30, siteFailures: 27, placeLoads: 1000, placeFailures: 80}
	finding := func(age time.Duration, verdict ProviderEgressTlsVerdict, decidedAt *time.Time) *ProviderEgressTlsFinding {
		return &ProviderEgressTlsFinding{ClientId: server.NewId(), UrlKey: "url", Name: "site", Url: "https://site.example/",
			MeasuredAt: now.Add(-age), Verdict: verdict, VerdictMeasuredAt: decidedAt, CountryCode: "zz"}
	}
	decidedAt := func(age time.Duration) *time.Time {
		at := now.Add(-age)
		return &at
	}
	for _, c := range []struct {
		name        string
		finding     *ProviderEgressTlsFinding
		served      bool
		networks    int
		direct      ProviderEgressTlsDirectOutcome
		place       providerEgressTlsPlaceEvidence
		wantVerdict ProviderEgressTlsVerdict
		wantDecided bool
	}{
		{"unserved is retired whatever else holds", finding(time.Minute, ProviderEgressTlsVerdictProvider, decidedAt(time.Minute)), false, 0, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictRetired, true},
		{"corroborated by a direct check", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"fifteen networks are still provider-specific", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 15, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"sixteen networks are common-mode", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 16, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictSite, true},
		{"the server's own check fails", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectFailed, working, ProviderEgressTlsVerdictSite, true},
		{"inconclusive stays pending", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectInconclusive, working, ProviderEgressTlsVerdictPending, false},
		{"inconclusive never lifts a quarantine", finding(time.Minute, ProviderEgressTlsVerdictProvider, decidedAt(30*time.Minute)), true, 1, ProviderEgressTlsDirectInconclusive, working, ProviderEgressTlsVerdictProvider, false},
		{"the site does not work from the place", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPlace, true},
		{"one load short of the place rule", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{siteLoads: 29, siteFailures: 29, placeLoads: 1000, placeFailures: 80}, ProviderEgressTlsVerdictProvider, true},
		{"below the place failure share", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{siteLoads: 100, siteFailures: 89, placeLoads: 1000, placeFailures: 120}, ProviderEgressTlsVerdictProvider, true},
		{"a place whose exits fail most sites judges none", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{siteLoads: 30, siteFailures: 30, placeLoads: 100, placeFailures: 50}, ProviderEgressTlsVerdictProvider, true},
		{"an unknown country has no place", &ProviderEgressTlsFinding{Name: "site", Url: "https://site.example/", MeasuredAt: now.Add(-time.Minute)}, true, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictProvider, true},
		{"a decided failure is not judged again", finding(10*time.Minute, ProviderEgressTlsVerdictSite, decidedAt(10*time.Minute)), true, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictSite, false},
		{"a newer failure of a decided finding is judged", finding(time.Minute, ProviderEgressTlsVerdictSite, decidedAt(30*time.Minute)), true, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"a failure past the direct window is judged by place alone", finding(2*time.Hour, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPlace, true},
		{"a newer failure past the direct window keeps what it had", finding(2*time.Hour, ProviderEgressTlsVerdictProvider, decidedAt(3*time.Hour)), true, 20, ProviderEgressTlsDirectFailed, working, ProviderEgressTlsVerdictProvider, false},
		{"a failure past the place window keeps what it had", finding(4*24*time.Hour, ProviderEgressTlsVerdictPending, nil), true, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPending, false},
		{"a retired failure stays retired when its URL is served again", finding(2*time.Hour, ProviderEgressTlsVerdictRetired, &earlier), true, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictRetired, false},
		{"a newer failure of a retired finding waits as pending", finding(2*time.Hour, ProviderEgressTlsVerdictRetired, decidedAt(3*time.Hour)), true, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictPending, false},
	} {
		verdict, decided := judgeProviderEgressTlsFinding(now, c.finding, c.served, c.networks, c.direct, c.place, settings)
		if verdict != c.wantVerdict || decided != c.wantDecided {
			t.Errorf("%s: verdict=%s decided=%t, want %s decided=%t", c.name, verdict, decided, c.wantVerdict, c.wantDecided)
		}
	}
}

// The common-mode count is of distinct networks with pending or site
// findings on the URL within the direct window. A judged provider finding,
// refreshed by its own rechecks, never counts, and a provider whose client
// row is gone stands as its own network.
func TestTlsJudgeCountsCommonModeNetworks(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	shared := server.NewId()
	finding := func(urlKey string, networkId *server.Id, age time.Duration, verdict ProviderEgressTlsVerdict) *ProviderEgressTlsFinding {
		return &ProviderEgressTlsFinding{ClientId: server.NewId(), UrlKey: urlKey, NetworkId: networkId, MeasuredAt: now.Add(-age), Verdict: verdict}
	}
	findings := []*ProviderEgressTlsFinding{
		finding("a", &shared, time.Minute, ProviderEgressTlsVerdictPending),
		finding("a", &shared, time.Minute, ProviderEgressTlsVerdictPending),
		finding("a", nil, time.Minute, ProviderEgressTlsVerdictSite),
		finding("a", nil, time.Minute, ProviderEgressTlsVerdictPending),
		finding("a", nil, time.Minute, ProviderEgressTlsVerdictProvider),
		finding("a", nil, 2*time.Hour, ProviderEgressTlsVerdictPending),
		finding("b", nil, time.Minute, ProviderEgressTlsVerdictPlace),
	}
	counts := providerEgressTlsCommonModeNetworks(now, findings, settings)
	if counts["a"] != 3 || counts["b"] != 0 {
		t.Fatalf("common-mode networks a=%d b=%d, want 3 and 0", counts["a"], counts["b"])
	}
}

// The verdicts an exclusion and a recheck read.
func TestTlsVerdictsExcludeAndRecheckOnlyWhatMayBeTheProviders(t *testing.T) {
	for _, verdict := range ProviderEgressTlsVerdicts {
		excludes := verdict == ProviderEgressTlsVerdictProvider
		rechecked := verdict == ProviderEgressTlsVerdictProvider || verdict == ProviderEgressTlsVerdictPending
		if verdict.Excludes() != excludes || verdict.Rechecked() != rechecked {
			t.Errorf("%s: excludes=%t rechecked=%t", verdict, verdict.Excludes(), verdict.Rechecked())
		}
	}
	if got := providerUrlSecurityExcludesSql("s"); got != "(s.tls_failure AND s.tls_verdict = 1)" {
		t.Errorf("exclusion predicate %q", got)
	}
	if got := providerUrlSecurityRechecksSql("s"); got != "(s.tls_failure AND s.tls_verdict IN (0, 1))" {
		t.Errorf("recheck predicate %q", got)
	}
}
