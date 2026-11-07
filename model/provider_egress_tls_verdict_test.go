// The TLS judge's decision rule (provider_egress_tls_verdict.go), without a
// database.
package model

import (
	"context"
	"fmt"
	"slices"
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
	pool := NewProviderEgressTlsPool(now, findings, served, nil)
	SetProviderEgressTlsVerdicts(ctx, JudgeProviderEgressTlsFindings(ctx, now, findings, pool, direct, settings))
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
	if settings.RetireAfter != 24*time.Hour || settings.RetireAfter != site.SiteRefreshInterval() {
		t.Errorf("retirement after %s is not the site refresh interval", settings.RetireAfter)
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
	const (
		served  = providerEgressTlsServed
		leaving = providerEgressTlsLeaving
		gone    = providerEgressTlsGone
	)
	for _, c := range []struct {
		name        string
		finding     *ProviderEgressTlsFinding
		serving     providerEgressTlsServing
		networks    int
		direct      ProviderEgressTlsDirectOutcome
		place       providerEgressTlsPlaceEvidence
		wantVerdict ProviderEgressTlsVerdict
		wantDecided bool
	}{
		{"out of the pool for a refresh interval is retired whatever else holds", finding(time.Minute, ProviderEgressTlsVerdictProvider, decidedAt(time.Minute)), gone, 0, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictRetired, false},
		{"a pending failure out of the pool for a refresh interval is retired", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), gone, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictRetired, false},
		{"out of the pool briefly keeps a decided verdict", finding(time.Minute, ProviderEgressTlsVerdictProvider, decidedAt(time.Minute)), leaving, 0, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, false},
		{"out of the pool briefly is judged as if served", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), leaving, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"corroborated by a direct check", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"fifteen networks are still provider-specific", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 15, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"sixteen networks are common-mode", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 16, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictSite, true},
		{"the server's own check fails", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectFailed, working, ProviderEgressTlsVerdictSite, true},
		{"inconclusive stays pending", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectInconclusive, working, ProviderEgressTlsVerdictPending, false},
		{"inconclusive never lifts a quarantine", finding(time.Minute, ProviderEgressTlsVerdictProvider, decidedAt(30*time.Minute)), served, 1, ProviderEgressTlsDirectInconclusive, working, ProviderEgressTlsVerdictProvider, false},
		{"the site does not work from the place", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPlace, true},
		{"one load short of the place rule", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{siteLoads: 29, siteFailures: 29, placeLoads: 1000, placeFailures: 80}, ProviderEgressTlsVerdictProvider, true},
		{"below the place failure share", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{siteLoads: 100, siteFailures: 89, placeLoads: 1000, placeFailures: 120}, ProviderEgressTlsVerdictProvider, true},
		{"a place whose exits fail most sites judges none", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{siteLoads: 30, siteFailures: 30, placeLoads: 100, placeFailures: 50}, ProviderEgressTlsVerdictProvider, true},
		{"an unknown country has no place", &ProviderEgressTlsFinding{Name: "site", Url: "https://site.example/", MeasuredAt: now.Add(-time.Minute)}, served, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictProvider, true},
		{"a decided failure is not judged again", finding(10*time.Minute, ProviderEgressTlsVerdictSite, decidedAt(10*time.Minute)), served, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictSite, false},
		{"a newer failure of a decided finding is judged", finding(time.Minute, ProviderEgressTlsVerdictSite, decidedAt(30*time.Minute)), served, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"a failure past the direct window is judged by place alone", finding(2*time.Hour, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPlace, true},
		{"a newer failure past the direct window keeps what it had", finding(2*time.Hour, ProviderEgressTlsVerdictProvider, decidedAt(3*time.Hour)), served, 20, ProviderEgressTlsDirectFailed, working, ProviderEgressTlsVerdictProvider, false},
		{"a failure past the place window keeps what it had", finding(4*24*time.Hour, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPending, false},
		{"a retired finding is pending again once its URL is served", finding(2*time.Hour, ProviderEgressTlsVerdictRetired, &earlier), served, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictPending, false},
		{"a retired finding served again is judged on a fresh failure", finding(time.Minute, ProviderEgressTlsVerdictRetired, decidedAt(3*time.Hour)), served, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictProvider, true},
		{"a newer failure of a retired finding waits as pending", finding(2*time.Hour, ProviderEgressTlsVerdictRetired, decidedAt(3*time.Hour)), served, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictPending, false},
		{"a retired finding still out of the pool stays retired", finding(time.Minute, ProviderEgressTlsVerdictRetired, nil), leaving, 1, ProviderEgressTlsDirectVerified, working, ProviderEgressTlsVerdictRetired, false},
	} {
		verdict, decided := judgeProviderEgressTlsFinding(now, c.finding, c.serving, c.networks, c.direct, c.place, settings)
		if verdict != c.wantVerdict || decided != c.wantDecided {
			t.Errorf("%s: verdict=%s decided=%t, want %s decided=%t", c.name, verdict, decided, c.wantVerdict, c.wantDecided)
		}
	}
}

// Seeded from the review's proof (REVIEW-D1 R3): a provider verdict whose
// destination drops out of the pool for one judge run keeps excluding, and
// is retired only once the name has stayed out on every run for a refresh
// interval; served again, it is pending, so rechecked, and the provider's
// next failure is judged on its own evidence.
func TestTlsRetirementNeedsADayOutOfThePoolAndEndsWhenServedAgain(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	measuredAt := now.Add(-30 * time.Minute)
	finding := &ProviderEgressTlsFinding{Name: "bank", Url: "https://bank.example/", MeasuredAt: measuredAt,
		Verdict: ProviderEgressTlsVerdictProvider, VerdictMeasuredAt: &measuredAt, CountryCode: "zz"}
	findings := []*ProviderEgressTlsFinding{finding}
	judge := func(at time.Time, pool *ProviderEgressTlsPool) (ProviderEgressTlsVerdict, bool) {
		return judgeProviderEgressTlsFinding(at, finding, pool.serving(at, finding.Name, settings), 0, ProviderEgressTlsDirectVerified, providerEgressTlsPlaceEvidence{}, settings)
	}

	// one judge run while the name is briefly out of the pool
	pool := NewProviderEgressTlsPool(now, findings, map[string]bool{}, nil)
	if verdict, decided := judge(now, pool); verdict != ProviderEgressTlsVerdictProvider || decided {
		t.Fatalf("one run out of the pool: %s decided=%t", verdict, decided)
	}
	// every run after it, until a whole refresh interval has passed
	for _, at := range []time.Time{now.Add(5 * time.Minute), now.Add(settings.RetireAfter - time.Second)} {
		pool = NewProviderEgressTlsPool(at, findings, map[string]bool{}, pool.UnservedSince)
		if verdict, _ := judge(at, pool); verdict != ProviderEgressTlsVerdictProvider {
			t.Fatalf("out of the pool since %s, at %s: %s", now, at, verdict)
		}
	}
	retireAt := now.Add(settings.RetireAfter)
	pool = NewProviderEgressTlsPool(retireAt, findings, map[string]bool{}, pool.UnservedSince)
	verdict, decided := judge(retireAt, pool)
	if verdict != ProviderEgressTlsVerdictRetired || decided || verdict.Excludes() || verdict.Rechecked() {
		t.Fatalf("out of the pool for a refresh interval: %s decided=%t", verdict, decided)
	}
	finding.Verdict = verdict

	// a clock that started again while the name is still out keeps it retired
	if verdict, _ := judge(retireAt.Add(5*time.Minute), NewProviderEgressTlsPool(retireAt.Add(5*time.Minute), findings, map[string]bool{}, nil)); verdict != ProviderEgressTlsVerdictRetired {
		t.Fatalf("still out of the pool on a new clock: %s", verdict)
	}

	// the name is served again five minutes later
	servedAt := retireAt.Add(5 * time.Minute)
	pool = NewProviderEgressTlsPool(servedAt, findings, map[string]bool{"bank": true}, pool.UnservedSince)
	verdict, decided = judge(servedAt, pool)
	if verdict != ProviderEgressTlsVerdictPending || decided || !verdict.Rechecked() || len(pool.UnservedSince) != 0 {
		t.Fatalf("served again: %s decided=%t clock=%v", verdict, decided, pool.UnservedSince)
	}
	// its recheck fails: the fresh failure is the provider's own again
	finding.Verdict, finding.VerdictMeasuredAt = verdict, nil
	finding.MeasuredAt = servedAt.Add(time.Minute)
	if verdict, decided := judge(servedAt.Add(2*time.Minute), pool); verdict != ProviderEgressTlsVerdictProvider || !decided {
		t.Fatalf("a failed recheck once served again: %s decided=%t", verdict, decided)
	}
}

// The pool clock carried from run to run: a finding's name out of the pool
// keeps the time it was first read out, or starts now; a name served again,
// or whose findings are all retired, drops; a time ahead of now starts again;
// and the clock holds at most the name limit, in name order.
func TestTlsJudgeCarriesWhenEachNameLeftThePool(t *testing.T) {
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	finding := func(name string, verdict ProviderEgressTlsVerdict) *ProviderEgressTlsFinding {
		return &ProviderEgressTlsFinding{Name: name, Url: "https://" + name + ".example/", MeasuredAt: now.Add(-time.Hour), Verdict: verdict}
	}
	findings := []*ProviderEgressTlsFinding{
		finding("kept", ProviderEgressTlsVerdictProvider),
		finding("kept", ProviderEgressTlsVerdictPending),
		finding("new", ProviderEgressTlsVerdictPending),
		finding("back", ProviderEgressTlsVerdictProvider),
		finding("ahead", ProviderEgressTlsVerdictSite),
		finding("retired", ProviderEgressTlsVerdictRetired),
	}
	previous := map[string]time.Time{
		"kept":    now.Add(-3 * time.Hour),
		"back":    now.Add(-3 * time.Hour),
		"ahead":   now.Add(time.Minute),
		"retired": now.Add(-48 * time.Hour),
		"gone":    now.Add(-3 * time.Hour),
	}
	pool := NewProviderEgressTlsPool(now, findings, map[string]bool{"back": true}, previous)
	want := map[string]time.Time{"kept": now.Add(-3 * time.Hour), "new": now, "ahead": now}
	if len(pool.UnservedSince) != len(want) {
		t.Fatalf("clock %v, want %v", pool.UnservedSince, want)
	}
	for name, since := range want {
		if !pool.UnservedSince[name].Equal(since) {
			t.Errorf("%s out since %s, want %s", name, pool.UnservedSince[name], since)
		}
	}

	many := []*ProviderEgressTlsFinding{}
	for i := range providerEgressTlsUnservedNameLimit + 3 {
		many = append(many, finding(fmt.Sprintf("site-%05d", i), ProviderEgressTlsVerdictPending))
	}
	pool = NewProviderEgressTlsPool(now, many, map[string]bool{}, nil)
	if len(pool.UnservedSince) != providerEgressTlsUnservedNameLimit {
		t.Fatalf("the clock holds %d names, want the limit %d", len(pool.UnservedSince), providerEgressTlsUnservedNameLimit)
	}
	if _, ok := pool.UnservedSince[fmt.Sprintf("site-%05d", providerEgressTlsUnservedNameLimit)]; ok {
		t.Fatal("a name past the limit started a clock")
	}
}

// Only findings a run judges on evidence have their hosts checked: undecided,
// recent enough for a check now to be close, and neither due to retire nor
// retired and still out of the pool.
func TestTlsJudgeChecksOnlyHostsItJudgesOnEvidence(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	decided := now.Add(-time.Minute)
	finding := func(name string, age time.Duration, verdict ProviderEgressTlsVerdict, decidedAt *time.Time) *ProviderEgressTlsFinding {
		return &ProviderEgressTlsFinding{Name: name, Url: "https://" + name + ".example/path", MeasuredAt: now.Add(-age), Verdict: verdict, VerdictMeasuredAt: decidedAt}
	}
	findings := []*ProviderEgressTlsFinding{
		finding("fresh", time.Minute, ProviderEgressTlsVerdictPending, nil),
		finding("decided", time.Minute, ProviderEgressTlsVerdictProvider, &decided),
		finding("old", 2*time.Hour, ProviderEgressTlsVerdictPending, nil),
		finding("leaving", time.Minute, ProviderEgressTlsVerdictPending, nil),
		finding("gone", time.Minute, ProviderEgressTlsVerdictPending, nil),
		finding("still-out", time.Minute, ProviderEgressTlsVerdictRetired, nil),
		finding("returned", time.Minute, ProviderEgressTlsVerdictRetired, nil),
	}
	pool := &ProviderEgressTlsPool{
		Served:        map[string]bool{"fresh": true, "decided": true, "old": true, "returned": true},
		UnservedSince: map[string]time.Time{"leaving": now.Add(-time.Hour), "gone": now.Add(-settings.RetireAfter)},
	}
	hosts := ProviderEgressTlsDirectHosts(now, findings, pool, settings)
	if want := []string{"fresh.example", "leaving.example", "returned.example"}; !slices.Equal(hosts, want) {
		t.Fatalf("checked hosts %v, want %v", hosts, want)
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
