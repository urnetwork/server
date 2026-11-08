// The TLS judge's decision rule (provider_egress_tls_verdict.go), without a
// database.
package model

import (
	"context"
	"errors"
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
	if settings.PlaceMinNetworks != ProviderClientVerdictQuorum {
		t.Errorf("place network floor %d is not the distinct-network quorum", settings.PlaceMinNetworks)
	}
	if settings.PlaceMinNetworks != 3 {
		t.Errorf("place network floor %d is not three", settings.PlaceMinNetworks)
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
	working := providerEgressTlsPlaceEvidence{networks: 3, failed: 3, verified: 27, countryLoads: 1000, countryFailures: 50}
	blocked := providerEgressTlsPlaceEvidence{networks: 3, failed: 27, verified: 3, countryLoads: 1000, countryFailures: 80}
	withheld := providerEgressTlsPlaceEvidence{networks: 3, withheld: ProviderEgressTlsPlaceWithheldBudget}
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
		{"withheld place evidence still excludes a failure the direct check verified, undecided", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, withheld, ProviderEgressTlsVerdictProvider, false},
		{"withheld place evidence leaves an uncorroborated new failure pending", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectInconclusive, withheld, ProviderEgressTlsVerdictPending, false},
		{"withheld place evidence leaves a failure past the direct window as stored", finding(2*time.Hour, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, withheld, ProviderEgressTlsVerdictPending, false},
		{"withheld place evidence keeps a decided verdict", finding(time.Minute, ProviderEgressTlsVerdictProvider, decidedAt(30*time.Minute)), served, 1, ProviderEgressTlsDirectInconclusive, withheld, ProviderEgressTlsVerdictProvider, false},
		{"withheld place evidence keeps a place verdict the server cannot recheck", finding(time.Minute, ProviderEgressTlsVerdictPlace, decidedAt(30*time.Minute)), served, 1, ProviderEgressTlsDirectInconclusive, withheld, ProviderEgressTlsVerdictPlace, false},
		{"withheld place evidence excludes a newer verified failure of a place finding, undecided", finding(time.Minute, ProviderEgressTlsVerdictPlace, decidedAt(30*time.Minute)), served, 1, ProviderEgressTlsDirectVerified, withheld, ProviderEgressTlsVerdictProvider, false},
		{"withheld place evidence still lets a site fault through", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectFailed, withheld, ProviderEgressTlsVerdictSite, true},
		{"withheld place evidence leaves an uncorroborated returned finding pending", finding(time.Minute, ProviderEgressTlsVerdictRetired, nil), served, 1, ProviderEgressTlsDirectInconclusive, withheld, ProviderEgressTlsVerdictPending, false},
		{"withheld place evidence excludes a returned finding the direct check verified, undecided", finding(time.Minute, ProviderEgressTlsVerdictRetired, nil), served, 1, ProviderEgressTlsDirectVerified, withheld, ProviderEgressTlsVerdictProvider, false},
		{"the URL's certificate fails across the place's networks", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified, blocked, ProviderEgressTlsVerdictPlace, true},
		{"one other network short of the place rule", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{networks: 2, failed: 27, verified: 0, countryLoads: 1000, countryFailures: 80}, ProviderEgressTlsVerdictProvider, true},
		{"below the place failure share", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{networks: 9, failed: 89, verified: 11, countryLoads: 1000, countryFailures: 120}, ProviderEgressTlsVerdictProvider, true},
		{"a place whose exits fail most sites judges none", finding(time.Minute, ProviderEgressTlsVerdictPending, nil), served, 1, ProviderEgressTlsDirectVerified,
			providerEgressTlsPlaceEvidence{networks: 3, failed: 30, verified: 0, countryLoads: 100, countryFailures: 50}, ProviderEgressTlsVerdictProvider, true},
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

// The place rule, one condition at a time at its boundary.
func TestTlsPlaceRuleNeedsOtherNetworksTheirTlsOutcomesAndAWorkingCountry(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	met := providerEgressTlsPlaceEvidence{networks: 3, failed: 9, verified: 1, countryLoads: 30, countryFailures: 14}
	if !met.broken(settings) {
		t.Fatalf("evidence meeting every condition at its boundary is not a place fault: %+v", met)
	}
	for _, c := range []struct {
		name   string
		change func(*providerEgressTlsPlaceEvidence)
	}{
		{"two other networks", func(e *providerEgressTlsPlaceEvidence) { e.networks = 2 }},
		{"no failed outcome among the other networks' providers", func(e *providerEgressTlsPlaceEvidence) { e.failed, e.verified = 0, 0 }},
		{"just under nine in ten of the other networks' outcomes failed", func(e *providerEgressTlsPlaceEvidence) { e.verified = 2 }},
		{"fewer than thirty loads of the country's other sites", func(e *providerEgressTlsPlaceEvidence) { e.countryLoads = 29 }},
		{"the country's exits fail half their other loads", func(e *providerEgressTlsPlaceEvidence) { e.countryFailures = 15 }},
	} {
		evidence := met
		c.change(&evidence)
		if evidence.broken(settings) {
			t.Errorf("%s: judged a place fault on %+v", c.name, evidence)
		}
	}
}

// Seeded from the review's proof (REVIEW-D1 R2): about seventeen providers on
// one network filled the site tally of a low-volume country with their own
// draws, all failing, and the next failed recheck of a provider already judged
// its own was released as "place". The farm's network is the finding's own,
// so none of its findings, outcomes or loads count: the provider verdict holds.
func TestTlsPlaceCannotBeManufacturedByOneNetwork(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	network := server.NewId()
	countryLocationId := server.NewId()
	decidedAt := now.Add(-2 * time.Hour)
	findings := []*ProviderEgressTlsFinding{}
	for range 17 {
		findings = append(findings, &ProviderEgressTlsFinding{ClientId: server.NewId(), UrlKey: "u", Name: "bank", Url: "https://bank.example/",
			MeasuredAt: now.Add(-time.Minute), Verdict: ProviderEgressTlsVerdictProvider, VerdictMeasuredAt: &decidedAt,
			NetworkId: &network, CountryCode: "zz", CountryLocationId: &countryLocationId})
	}
	judged := make([]bool, len(findings))
	for i := range judged {
		judged[i] = true
	}
	// no read happens: one network never reaches the other-network floor
	evidence, _ := getProviderEgressTlsPlaceEvidence(context.Background(), now, settings, findings, judged, nil)
	for i, place := range evidence {
		if place.networks != 0 || place.broken(settings) {
			t.Fatalf("farm finding %d counted its own network as place evidence: %+v", i, place)
		}
		verdict, _ := judgeProviderEgressTlsFinding(now, findings[i], providerEgressTlsServed, 0, ProviderEgressTlsDirectVerified, place, settings)
		if verdict != ProviderEgressTlsVerdictProvider {
			t.Fatalf("a single-network farm's recheck failure was judged %s", verdict)
		}
	}
}

// Seeded from the review's proof (REVIEW-D1 R2): the country-works share
// counted the blocked site's own failures. On the live Turkish tally of
// 2026-11-07 (5458 loads, 2866 failed; discord 1436 of 1436 failed, roblox
// 973 of 973) the share with discord is 0.525, so the block was never a
// place fault; over the other sites it is 1430 of 4022, 0.356.
func TestTlsPlaceWorkingShareLeavesOutTheBlockedSite(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	turkey := providerEgressTlsCountryTally{loads: 5458, failures: 2866, sites: map[string]providerEgressTlsCountryTally{
		"discord": {loads: 1436, failures: 1436},
		"roblox":  {loads: 973, failures: 973},
	}}
	if share := float64(turkey.failures) / float64(turkey.loads); share < settings.PlaceWorkingShare {
		t.Fatalf("the precondition failed: Turkey's share with discord is %.3f", share)
	}
	loads, failures := turkey.without("discord")
	if loads != 4022 || failures != 1430 {
		t.Fatalf("Turkey without discord: %d loads, %d failed", loads, failures)
	}
	place := providerEgressTlsPlaceEvidence{networks: 4, failed: 11, verified: 1, countryLoads: loads, countryFailures: failures}
	if !place.broken(settings) {
		t.Fatalf("a block whose own failures alone push the country over the line is no place fault: %+v", place)
	}
}

// A site whose handshakes were reset for days, then intercepted, fills the
// site tally with failures but holds no TLS outcome from anyone else, so it
// builds no place, and neither do resets the other networks see: only their
// TLS verification outcomes count.
func TestTlsResetsCountForNothingTowardPlace(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	for _, place := range []providerEgressTlsPlaceEvidence{
		// the farm alone: its resets are no TLS outcome, and its network is
		// its own
		{networks: 0, failed: 0, verified: 0, countryLoads: 4000, countryFailures: 600},
		// three other networks intercept too, while the honest providers
		// there verify the URL
		{networks: 3, failed: 3, verified: 2, countryLoads: 4000, countryFailures: 600},
	} {
		if place.broken(settings) {
			t.Errorf("judged a place fault on %+v", place)
		}
	}
}

// A site or prober fault is decided before place is asked, so a fault that
// fails TLS across many networks reads no place evidence at all: this runs
// with no database, and any read would fail it.
func TestTlsSiteFaultsReadNoPlaceEvidence(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	countryLocationId := server.NewId()
	findings := []*ProviderEgressTlsFinding{}
	for range 5 {
		network := server.NewId()
		findings = append(findings, &ProviderEgressTlsFinding{ClientId: server.NewId(), UrlKey: "u", Name: "expired", Url: "https://expired.example/",
			MeasuredAt: now.Add(-time.Minute), NetworkId: &network, CountryCode: "zz", CountryLocationId: &countryLocationId})
	}
	pool := NewProviderEgressTlsPool(now, findings, map[string]bool{"expired": true}, nil)
	direct := map[string]ProviderEgressTlsDirectOutcome{"expired.example": ProviderEgressTlsDirectFailed}
	for _, judgement := range JudgeProviderEgressTlsFindings(context.Background(), now, findings, pool, direct, settings) {
		if judgement.Verdict != ProviderEgressTlsVerdictSite || judgement.PlaceNetworks != 0 {
			t.Fatalf("a site fault was judged %s with place evidence on %d networks", judgement.Verdict, judgement.PlaceNetworks)
		}
	}
}

// Place-evidence findings in one country: one finding on each of networks
// networks, all failing the URL a minute ago.
func testingTlsPlaceFindings(now time.Time, urlKey string, countryCode string, countryLocationId server.Id, networks int) []*ProviderEgressTlsFinding {
	findings := []*ProviderEgressTlsFinding{}
	for range networks {
		network := server.NewId()
		findings = append(findings, &ProviderEgressTlsFinding{ClientId: server.NewId(), UrlKey: urlKey, Name: "site-" + urlKey,
			Url: "https://" + urlKey + ".example/", MeasuredAt: now.Add(-time.Minute), NetworkId: &network,
			CountryCode: countryCode, CountryLocationId: &countryLocationId})
	}
	return findings
}

// Place reads that are faked: tallies by country, and outcomes that succeed,
// raise, or wait out their context, recording the order the pairs are read in.
type testingTlsPlaceReads struct {
	countryLoads map[string]int
	tallyFails   bool
	failing      map[string]bool
	waiting      map[string]bool
	read         []string
}

func (self *testingTlsPlaceReads) reads() providerEgressTlsPlaceReads {
	return providerEgressTlsPlaceReads{
		tallies: func(ctx context.Context, _ time.Time, _ *ProviderEgressTlsJudgeSettings, countryCodes []string) map[string]providerEgressTlsCountryTally {
			if self.tallyFails {
				panic(errors.New("synthetic tally read failure"))
			}
			tallies := map[string]providerEgressTlsCountryTally{}
			for _, countryCode := range countryCodes {
				tallies[countryCode] = providerEgressTlsCountryTally{loads: self.countryLoads[countryCode], failures: self.countryLoads[countryCode] / 10}
			}
			return tallies
		},
		outcomes: func(ctx context.Context, _ time.Time, key providerEgressTlsPlaceKey) map[server.Id]providerEgressTlsPlaceOutcome {
			self.read = append(self.read, key.urlKey)
			switch {
			case self.failing[key.urlKey]:
				panic(errors.New("synthetic outcome read failure"))
			case self.waiting[key.urlKey]:
				<-ctx.Done()
				server.Raise(ctx.Err())
			}
			return map[server.Id]providerEgressTlsPlaceOutcome{server.NewId(): {failed: 9}}
		},
	}
}

// The place reads of one run stay inside their budget and never fail the run.
// They go cheapest country first, so a large country reads last; a pair whose
// read fails is withheld as an error, one that runs out the budget, and every
// pair after it, as budget; and the pairs read in time keep their evidence.
func TestTlsPlaceReadsStayInTheirBudgetAndNeverFailTheRun(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	settings.PlaceReadBudget = 200 * time.Millisecond
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	small, medium, large := server.NewId(), server.NewId(), server.NewId()
	findings := []*ProviderEgressTlsFinding{}
	findings = append(findings, testingTlsPlaceFindings(now, "large-a", "zl", large, 4)...)
	findings = append(findings, testingTlsPlaceFindings(now, "medium", "zm", medium, 4)...)
	findings = append(findings, testingTlsPlaceFindings(now, "small", "zs", small, 4)...)
	findings = append(findings, testingTlsPlaceFindings(now, "large-b", "zl", large, 4)...)
	candidates := make([]bool, len(findings))
	for i := range candidates {
		candidates[i] = true
	}
	fake := &testingTlsPlaceReads{
		countryLoads: map[string]int{"zs": 100, "zm": 5000, "zl": 80000},
		failing:      map[string]bool{"medium": true},
		waiting:      map[string]bool{"large-a": true, "large-b": true},
	}
	evidence, _ := readProviderEgressTlsPlaceEvidence(context.Background(), now, settings, findings, candidates, nil, fake.reads())

	if len(fake.read) != 3 || fake.read[0] != "small" || fake.read[1] != "medium" {
		t.Fatalf("pairs read in the order %v, want the small country, the medium one, then one large pair", fake.read)
	}
	want := map[string]string{"small": "", "medium": ProviderEgressTlsPlaceWithheldError,
		"large-a": ProviderEgressTlsPlaceWithheldBudget, "large-b": ProviderEgressTlsPlaceWithheldBudget}
	for i, finding := range findings {
		if evidence[i].withheld != want[finding.UrlKey] {
			t.Errorf("%s: withheld %q, want %q", finding.UrlKey, evidence[i].withheld, want[finding.UrlKey])
		}
		if finding.UrlKey == "small" && (evidence[i].failed != 9 || evidence[i].countryLoads != 100) {
			t.Errorf("a pair read in time lost its evidence: %+v", evidence[i])
		}
	}
}

// A tally read that fails withholds place from every pair of the run, without
// failing the run or reading any outcome.
func TestTlsPlaceTallyReadFailureWithholdsEveryPair(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	findings := testingTlsPlaceFindings(now, "blocked", "zz", server.NewId(), 4)
	candidates := []bool{true, true, true, true}
	fake := &testingTlsPlaceReads{tallyFails: true}
	evidence, _ := readProviderEgressTlsPlaceEvidence(context.Background(), now, settings, findings, candidates, nil, fake.reads())
	for i, place := range evidence {
		if place.withheld != ProviderEgressTlsPlaceWithheldError || place.broken(settings) {
			t.Errorf("finding %d: %+v after a failed tally read", i, place)
		}
	}
	if len(fake.read) != 0 {
		t.Fatalf("outcomes read after the tally failed: %v", fake.read)
	}
}

// Past the run's pair limit, pairs are withheld; and within one country the
// order turns with the judge's interval, so a budget that runs out inside a
// large country cuts different pairs on successive runs.
func TestTlsPlaceReadsAreLimitedAndTurnWithinACountry(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	country := server.NewId()
	findings := []*ProviderEgressTlsFinding{}
	for i := range providerEgressTlsPlaceKeyLimit + 6 {
		findings = append(findings, testingTlsPlaceFindings(now, fmt.Sprintf("site-%03d", i), "zz", country, 4)...)
	}
	candidates := make([]bool, len(findings))
	for i := range candidates {
		candidates[i] = true
	}
	fake := &testingTlsPlaceReads{countryLoads: map[string]int{"zz": 1000}}
	evidence, _ := readProviderEgressTlsPlaceEvidence(context.Background(), now, settings, findings, candidates, nil, fake.reads())
	limited := map[string]bool{}
	for i, finding := range findings {
		if evidence[i].withheld == ProviderEgressTlsPlaceWithheldLimit {
			limited[finding.UrlKey] = true
		}
	}
	if len(fake.read) != providerEgressTlsPlaceKeyLimit || len(limited) != 6 {
		t.Fatalf("read %d pairs and limited %d, want %d and 6", len(fake.read), len(limited), providerEgressTlsPlaceKeyLimit)
	}

	keys := []providerEgressTlsPlaceKey{}
	for i := range 10 {
		keys = append(keys, providerEgressTlsPlaceKey{urlKey: fmt.Sprintf("site-%d", i), countryLocationId: country})
	}
	sameCost := func(providerEgressTlsPlaceKey) int { return 1 }
	first := providerEgressTlsPlaceReadOrder(now, keys, sameCost, nil)
	again := providerEgressTlsPlaceReadOrder(now.Add(time.Minute), keys, sameCost, nil)
	next := providerEgressTlsPlaceReadOrder(now.Add(5*time.Minute), keys, sameCost, nil)
	if !slices.Equal(first, again) {
		t.Fatal("the read order changed within one judge interval")
	}
	if slices.Equal(first, next) {
		t.Fatal("the read order within one country did not turn with the next judge interval")
	}
}

// Seeded from the follow-ups review's proof (REVIEW-D1 F1): with 64 cheap
// pairs needing place evidence and one in a costly country, the pair limit
// withholds the costly pair, and a withheld failure that the server's direct
// check verified was left pending, excluding no one, run after run for as
// long as the cheaper pairs kept it past the cut. A withheld pair must never
// gate the exclusion: such a failure is the provider's, undecided, so it
// excludes and is judged again, and place releases it on a run that reads
// its pair.
func TestTlsWithheldPlaceEvidenceNeverLeavesAVerifiedFailurePending(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	cheap, costly := server.NewId(), server.NewId()
	base := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	chain := &testingTlsPlaceReadChain{}
	withheldTurns := 0
	for turn := range 24 {
		now := base.Add(time.Duration(turn) * 5 * time.Minute)
		findings := []*ProviderEgressTlsFinding{}
		for i := range providerEgressTlsPlaceKeyLimit {
			findings = append(findings, testingTlsPlaceFindings(now, fmt.Sprintf("cheap-%03d", i), "zz", cheap, 4)...)
		}
		findings = append(findings, testingTlsPlaceFindings(now, "target", "us", costly, 4)...)
		candidates := make([]bool, len(findings))
		for i := range candidates {
			candidates[i] = true
		}
		fake := &testingTlsPlaceReads{countryLoads: map[string]int{"zz": 1000, "us": 9000000}}
		evidence := chain.read(now, settings, findings, candidates, fake.reads())
		withheld := false
		for i, finding := range findings {
			if finding.UrlKey != "target" {
				continue
			}
			verdict, decided := judgeProviderEgressTlsFinding(now, finding, providerEgressTlsServed, 1, ProviderEgressTlsDirectVerified, evidence[i], settings)
			switch {
			case evidence[i].withheld != "":
				withheld = true
				if verdict != ProviderEgressTlsVerdictProvider || decided {
					t.Fatalf("turn %d: a failure the direct check verified, on a pair withheld (%s), was judged %s decided=%t, want provider undecided",
						turn, evidence[i].withheld, verdict, decided)
				}
			case verdict != ProviderEgressTlsVerdictPlace || !decided:
				t.Fatalf("turn %d: a failure on a pair read as a place fault was judged %s decided=%t, want place", turn, verdict, decided)
			}
		}
		if withheld {
			withheldTurns += 1
		}
	}
	if withheldTurns == 0 {
		t.Fatal("the costly country's pair was never withheld, so nothing was tested")
	}
}

// The place-read clock a run carries on holds every pair the run needed and
// no other: now for each pair it began to read, completed or not, and the
// carried time for the rest, a time ahead of now (another host's clock) taken
// as now. Past its bound it keeps the pairs read most recently.
func TestTlsPlaceReadClockCarriesTheNeededPairsWithinItsBound(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	settings.PlaceReadBudget = 50 * time.Millisecond
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	country := server.NewId()
	findings := append(testingTlsPlaceFindings(now, "stale", "zz", country, 4), testingTlsPlaceFindings(now, "ahead", "zz", country, 4)...)
	candidates := make([]bool, len(findings))
	for i := range candidates {
		candidates[i] = true
	}
	stale := providerEgressTlsPlaceKey{urlKey: "stale", countryLocationId: country}.String()
	ahead := providerEgressTlsPlaceKey{urlKey: "ahead", countryLocationId: country}.String()
	gone := providerEgressTlsPlaceKey{urlKey: "gone", countryLocationId: country}.String()
	carried := map[string]time.Time{stale: now.Add(-10 * time.Minute), ahead: now.Add(time.Hour), gone: now.Add(-5 * time.Minute)}
	// the stale pair is read first and waits out the budget, so the other is
	// never begun
	fake := &testingTlsPlaceReads{countryLoads: map[string]int{"zz": 1000}, waiting: map[string]bool{"stale": true}}
	_, readAt := readProviderEgressTlsPlaceEvidence(context.Background(), now, settings, findings, candidates, carried, fake.reads())
	if len(fake.read) != 1 || fake.read[0] != "stale" {
		t.Fatalf("pairs begun %v, want the stale one alone", fake.read)
	}
	if len(readAt) != 2 || !readAt[stale].Equal(now) || !readAt[ahead].Equal(now) {
		t.Fatalf("clock %v, want the begun pair at now and the one ahead of the clock taken as now, and no pair the run did not need", readAt)
	}

	many := map[string]time.Time{}
	for i := range providerEgressTlsPlaceClockLimit + 76 {
		many[fmt.Sprintf("pair-%04d", i)] = now.Add(-time.Duration(i) * time.Second)
	}
	bounded := providerEgressTlsBoundPlaceClock(many)
	if len(bounded) != providerEgressTlsPlaceClockLimit {
		t.Fatalf("bounded clock holds %d pairs, want %d", len(bounded), providerEgressTlsPlaceClockLimit)
	}
	for i := range providerEgressTlsPlaceClockLimit + 76 {
		if _, kept := bounded[fmt.Sprintf("pair-%04d", i)]; kept != (i < providerEgressTlsPlaceClockLimit) {
			t.Fatalf("pair %d kept=%t, want the most recently read kept and the stalest dropped", i, kept)
		}
	}
}
