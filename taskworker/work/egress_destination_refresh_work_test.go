// Tests for the daily destination pool refresh of connect/GEOMAP.md §11.4:
// the pure decision over a pool and its tally, the promotion order, and
// refreshes against the database, including over URL turns recorded the way
// the probe task records them.
package work

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The refresh clock every pure test runs at.
var testRefreshNow = time.Date(2026, time.September, 20, 6, 0, 0, 0, time.UTC)

// Explicit catalog input keeps refresh tests independent of a workstation's config.
const testRefreshCatalog = `
schema_version: 1
destinations:
  - {name: synthetic-dns, class: dns, category: dns, url: "https://dns.example/dns-query?name=example.com&type=A", verify: {kind: dns_json}}
  - {name: synthetic-connectivity, class: connectivity, category: connectivity, url: "https://connectivity.example/", verify: {kind: contains, text: synthetic-success}}
  - {name: synthetic-cdn, class: cdn, category: cdn, url: "https://cdn.example/"}
  - {name: synthetic-retiring-news, class: site, category: news, url: "https://retiring-news.example/"}
  - {name: synthetic-steady-news, class: site, category: news, url: "https://steady-news.example/"}
`

const testRefreshCatalogSize = `
settings:
  site_pool_size: {dns: 1, connectivity: 1, cdn: 1, site: 2}
  site_sample_size: {dns: 1, connectivity: 1, cdn: 1, site: 1}
`

// The catalog with a sample floor a test fills in a few hundred URL turns, and
// no candidates from a workstation's qualityprobe.yml.
const testRefreshUrlTurnCatalog = testRefreshCatalog + `
settings:
  site_pool_size: {dns: 1, connectivity: 1, cdn: 1, site: 2}
  site_sample_size: {dns: 1, connectivity: 1, cdn: 1, site: 1}
  site_min_samples: 20
candidates: []
`

// One provider's URL turn on site from place, recorded the way the probe
// task's is: accepted into the health history through the model write the
// health ingest makes, and tallied through the site tally's own reading of the
// run. outcome is pass (a small complete document), refused (the site
// answered with a failing status) or unreached (the URL's DNS lookup through
// the tunnel never answered: the provider was not reached).
func testRefreshUrlTurn(ctx context.Context, at time.Time, site string, place model.ProviderEgressPlace, outcome string) {
	destination := egresshealth.Destination{Name: site, Class: egresshealth.ClassSite, Url: "https://" + site + ".example/"}
	check := egresshealth.CheckResult{Name: site, Class: egresshealth.ClassSite, Attempts: 1, UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{}}
	switch outcome {
	case "pass":
		check.Ok, check.StatusCode, check.RequestWritten, check.FirstByteReceived = true, http.StatusOK, true, true
		check.ContentClassification, check.PerformanceClassification = "content", "insufficient_sample"
		check.ByteCount, check.WireByteCount, check.WireSampleByteCount, check.BodyComplete = 32, 32, 31, true
	case "refused":
		check.StatusCode, check.RequestWritten, check.FirstByteReceived = http.StatusForbidden, true, true
		check.ContentClassification, check.FailureStage = "unexpected_status", "response_content"
	case "unreached":
		check.FailureStage, check.DnsMeasured = "dial_dns", true
	default:
		panic("unknown synthetic URL outcome " + outcome)
	}
	if check.StatusCode != 0 {
		check.UrlProbeSecurity = []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}}
	}
	okCount, failedNames := 0, site
	if check.Ok {
		okCount, failedNames = 1, ""
	}
	evidence := &egresshealth.UrlProbeEvidence{
		PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
		Destination: destination, MeasuredAt: at, Security: check.UrlProbeSecurity,
		ContentClassification: check.ContentClassification, PerformanceClassification: check.PerformanceClassification,
		FailureStage: check.FailureStage, ContentMatcherVersion: egresshealth.UrlProbeContentMatcherVersion,
		StatusCode: check.StatusCode, ByteCount: check.ByteCount, WireByteCount: check.WireByteCount,
		WireSampleByteCount: check.WireSampleByteCount, BodyComplete: check.BodyComplete,
		RequestWritten: check.RequestWritten, FirstByteReceived: check.FirstByteReceived,
	}
	if check.Ok {
		evidence.TtfbMillis, evidence.BodyMillis, evidence.BodyBitsPerSecond = 10, 1, 248000
	}
	model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
		RunId:            server.NewId(),
		ClientId:         server.NewId(),
		MeasuredAt:       at,
		UrlProbeEvidence: evidence,
		OKCount:          okCount,
		Total:            1,
		ClassResults:     map[string]model.ProviderEgressHealthClassResult{string(egresshealth.ClassSite): {OK: okCount, Total: 1}},
		FailedNames:      failedNames,
	})
	run := &egresshealth.Result{Checks: []egresshealth.CheckResult{check}, UrlProbeEvidence: evidence, OkCount: okCount, Total: 1}
	loads, healthy := egressHealthSiteLoads(run, place, nil, model.DefaultProviderEgressSiteSettings().SiteHealthyExitShare)
	model.AddProviderEgressRunTally(ctx, at, model.ProviderEgressRunTally{Place: place, Healthy: healthy}, loads)
}

// Count URL turns on site from place, the first ones unreached, then refused,
// then passing.
func testRefreshUrlTurns(ctx context.Context, at time.Time, site string, place model.ProviderEgressPlace, unreached int, refused int, passes int) {
	for i := range unreached + refused + passes {
		outcome := "pass"
		switch {
		case i < unreached:
			outcome = "unreached"
		case i < unreached+refused:
			outcome = "refused"
		}
		testRefreshUrlTurn(ctx, at, site, place, outcome)
	}
}

// The pool rows by name.
func testRefreshDestinationsByName(ctx context.Context) map[string]*model.ProviderEgressDestination {
	byName := map[string]*model.ProviderEgressDestination{}
	for _, d := range model.GetProviderEgressDestinations(ctx) {
		byName[d.Name] = d
	}
	return byName
}

// A host check for a refresh with no candidates listed: none may run.
func testRefreshNoCandidateCheck(t testing.TB) egressCandidateCheck {
	return func(_ context.Context, candidate egresshealth.Destination, _ egresshealth.RequestProfile, _ int, _ time.Duration) error {
		t.Errorf("a refresh with no candidates listed checked %s", candidate.Name)
		return nil
	}
}

// The default rules over a pool of one class: the other classes are empty,
// and the site class is sized to the fixture, so no test gets an opening it
// did not ask for.
func testRefreshSettings(sitePoolSize int, siteSampleSize int) *model.ProviderEgressSiteSettings {
	settings := model.DefaultProviderEgressSiteSettings()
	settings.SitePoolSize = map[string]int{"dns": 0, "connectivity": 0, "cdn": 0, "site": sitePoolSize}
	settings.SiteSampleSize = map[string]int{"dns": 0, "connectivity": 0, "cdn": 0, "site": siteSampleSize}
	return settings
}

// An active, scored site added long ago.
func testRefreshSite(name string, category string) *model.ProviderEgressDestination {
	return &model.ProviderEgressDestination{
		Name:      name,
		Class:     "site",
		Category:  category,
		Active:    true,
		AddedTime: testRefreshNow.Add(-90 * 24 * time.Hour),
	}
}

// One site's loads at one place, every one of them from a healthy exit.
func testHealthyTally(name string, country string, region string, loads int, failures int) model.ProviderEgressSiteTally {
	return model.ProviderEgressSiteTally{
		Name:                name,
		Place:               model.ProviderEgressPlace{CountryCode: country, Region: region},
		LoadCount:           loads,
		FailureCount:        failures,
		HealthyLoadCount:    loads,
		HealthyFailureCount: failures,
	}
}

// The names of a plan's changes, in order.
func testChangeNames(changes []egressRefreshChange) []string {
	names := []string{}
	for _, change := range changes {
		names = append(names, change.name)
	}
	return names
}

// The share a site is judged on, and what it stands on.
func TestJudgeEgressSiteFallsBackToContactedExitsAndNeedsEnoughSamples(t *testing.T) {
	for _, test := range []struct {
		name                                                             string
		healthyLoads, healthyFailures, contactedLoads, contactedFailures int
		share                                                            float64
		samples                                                          int
		basis                                                            string
	}{
		{name: "healthy exits", healthyLoads: 250, healthyFailures: 150, contactedLoads: 400, contactedFailures: 300, share: 0.6, samples: 250, basis: "healthy"},
		{name: "too few healthy", healthyLoads: 100, healthyFailures: 90, contactedLoads: 300, contactedFailures: 150, share: 0.5, samples: 300, basis: "contacted"},
		{name: "too few either way", healthyLoads: 100, healthyFailures: 90, contactedLoads: 150, contactedFailures: 100, share: 0.9, samples: 100, basis: "too_few"},
		{name: "only unhealthy exits", contactedLoads: 150, contactedFailures: 15, share: 0.1, samples: 150, basis: "too_few"},
	} {
		share, samples, basis := judgeEgressSite(test.healthyLoads, test.healthyFailures, test.contactedLoads, test.contactedFailures, 200)
		if share == nil || *share != test.share || samples != test.samples || basis != test.basis {
			t.Errorf("%s: judged %v over %d (%s), want %v over %d (%s)", test.name, share, samples, basis, test.share, test.samples, test.basis)
		}
	}
	if share, samples, basis := judgeEgressSite(0, 0, 0, 0, 200); share != nil || samples != 0 || basis != "too_few" {
		t.Errorf("a site with no loads was judged: %v over %d (%s)", share, samples, basis)
	}
}

// A site that fails more than half of 250 healthy exits is retired and opens a
// promotion of its category; one under the share, and one over it on too few
// samples, stay.
func TestPlanEgressDestinationRefreshRetiresAboveTheShareWithEnoughSamples(t *testing.T) {
	destinations := []*model.ProviderEgressDestination{
		testRefreshSite("bad-site", "news"),
		testRefreshSite("fine-site", "news"),
		testRefreshSite("thin-site", "video"),
		testRefreshSite("steady-site", "search"),
	}
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:          testRefreshNow,
		settings:     testRefreshSettings(4, 2),
		destinations: destinations,
		windowTallies: []model.ProviderEgressSiteTally{
			testHealthyTally("bad-site", "de", "", 250, 150),
			testHealthyTally("fine-site", "de", "", 250, 50),
			testHealthyTally("thin-site", "de", "", 150, 140),
			testHealthyTally("steady-site", "de", "", 3000, 0),
		},
	})
	if plan.skipped != "" {
		t.Fatalf("the refresh was skipped: %s", plan.skipped)
	}
	if names := testChangeNames(plan.retired); !slices.Equal(names, []string{"bad-site"}) {
		t.Fatalf("retired = %v, want only bad-site", names)
	}
	bad := plan.rows["bad-site"]
	if bad.Active || bad.RetireCount != 1 || bad.RetiredTime == nil || !bad.RetiredTime.Equal(testRefreshNow) {
		t.Fatalf("retired row = %+v", bad)
	}
	if !strings.HasPrefix(bad.RetireReason, "failed 60.0% of 250 healthy exits over 72h0m0s") {
		t.Errorf("retire reason = %q", bad.RetireReason)
	}
	if !destinations[0].Active {
		t.Fatal("the plan changed the pool it was given rather than a copy")
	}
	thin := plan.rows["thin-site"]
	if !thin.Active || thin.SampleCount != 150 || thin.AboveRetireSince != nil {
		t.Errorf("a site judged on too few samples was treated as judged: %+v", thin)
	}
	if fine := plan.rows["fine-site"]; fine.FailureShare == nil || *fine.FailureShare != 0.2 || fine.JudgedTime == nil {
		t.Errorf("the kept site's judgement was not recorded: %+v", fine)
	}
	if len(plan.needs) != 1 || plan.needs[0].class != "site" || plan.needs[0].category != "news" || plan.needs[0].place != nil {
		t.Fatalf("openings = %+v, want one news site in bad-site's place", plan.needs)
	}
}

// Two sites over the line in one class: the worse is retired this run, and
// the other is only noted as over the line since its first judgement there.
func TestPlanEgressDestinationRefreshRetiresTheWorstOnePerClass(t *testing.T) {
	overSince := testRefreshNow.Add(-24 * time.Hour)
	bad := testRefreshSite("bad-site", "news")
	bad.AboveRetireSince = &overSince
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:          testRefreshNow,
		settings:     testRefreshSettings(3, 2),
		destinations: []*model.ProviderEgressDestination{bad, testRefreshSite("worse-site", "video"), testRefreshSite("steady-site", "search")},
		windowTallies: []model.ProviderEgressSiteTally{
			testHealthyTally("bad-site", "de", "", 250, 150),
			testHealthyTally("worse-site", "de", "", 250, 200),
			testHealthyTally("steady-site", "de", "", 5000, 0),
		},
	})
	if names := testChangeNames(plan.retired); !slices.Equal(names, []string{"worse-site"}) {
		t.Fatalf("retired = %v, want only the worse site", names)
	}
	kept := plan.rows["bad-site"]
	if !kept.Active || kept.AboveRetireSince == nil || !kept.AboveRetireSince.Equal(overSince) {
		t.Fatalf("the site left over the line = %+v, want active and over since %s", kept, overSince)
	}
	if len(plan.needs) != 1 || plan.needs[0].category != "video" {
		t.Fatalf("openings = %+v, want one video site", plan.needs)
	}
}

// With too few healthy exits to judge on, a site is judged on every exit the
// probe reached, and the plan says it was.
func TestPlanEgressDestinationRefreshJudgesOnContactedExitsWhenTooFewAreHealthy(t *testing.T) {
	tally := model.ProviderEgressSiteTally{
		Name:                "bad-site",
		Place:               model.ProviderEgressPlace{CountryCode: "de"},
		LoadCount:           400,
		FailureCount:        300,
		HealthyLoadCount:    50,
		HealthyFailureCount: 45,
	}
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:           testRefreshNow,
		settings:      testRefreshSettings(2, 1),
		destinations:  []*model.ProviderEgressDestination{testRefreshSite("bad-site", "news"), testRefreshSite("steady-site", "search")},
		windowTallies: []model.ProviderEgressSiteTally{tally, testHealthyTally("steady-site", "de", "", 4000, 0)},
	})
	if names := testChangeNames(plan.retired); !slices.Equal(names, []string{"bad-site"}) {
		t.Fatalf("retired = %v, want bad-site on its share over all exits", names)
	}
	if !slices.Equal(plan.fallbacks, []string{"bad-site"}) {
		t.Fatalf("fallbacks = %v, want the judgement on contacted exits reported", plan.fallbacks)
	}
	if reason := plan.rows["bad-site"].RetireReason; !strings.HasPrefix(reason, "failed 75.0% of 400 contacted exits, healthy or not") {
		t.Errorf("retire reason does not say what it was judged on: %q", reason)
	}
}

// Nothing is judged while the fleet fails too much at once on the exits it
// reaches, whether the recent runs or the window show it; on the line itself
// the refresh runs, and the runs that reached no provider are no part of it.
func TestPlanEgressDestinationRefreshSkipsDuringAProberFault(t *testing.T) {
	destinations := func() []*model.ProviderEgressDestination {
		return []*model.ProviderEgressDestination{testRefreshSite("bad-site", "news"), testRefreshSite("fine-site", "news")}
	}
	faultyWindow := []model.ProviderEgressSiteTally{
		testHealthyTally("bad-site", "de", "", 250, 150),
		testHealthyTally("fine-site", "de", "", 250, 0),
	}
	quietWindow := append(slices.Clone(faultyWindow), testHealthyTally("steady-site", "de", "", 5000, 0))
	for _, test := range []struct {
		name    string
		recent  model.ProviderEgressContactedTotal
		window  []model.ProviderEgressSiteTally
		skipped bool
	}{
		{name: "recent runs", recent: model.ProviderEgressContactedTotal{Ok: 70, Total: 100}, window: quietWindow, skipped: true},
		{name: "recent runs beside as many no-contact failures", recent: model.ProviderEgressContactedTotal{Ok: 70, Total: 100, NoContactFailures: 100}, window: quietWindow, skipped: true},
		{name: "the window", window: faultyWindow, skipped: true},
		{name: "on the line", recent: model.ProviderEgressContactedTotal{Ok: 80, Total: 100}, window: quietWindow, skipped: false},
		{name: "half the runs reaching no provider", recent: model.ProviderEgressContactedTotal{Ok: 95, Total: 100, NoContactFailures: 100}, window: quietWindow, skipped: false},
	} {
		plan := planEgressDestinationRefresh(egressRefreshInput{
			now:           testRefreshNow,
			settings:      testRefreshSettings(2, 1),
			destinations:  destinations(),
			windowTallies: test.window,
			recent:        test.recent,
		})
		if (plan.skipped != "") != test.skipped {
			t.Errorf("%s: skipped = %q, want skipped %t", test.name, plan.skipped, test.skipped)
		}
		if test.skipped && (0 < len(plan.rows) || 0 < len(plan.retired) || 0 < len(plan.needs)) {
			t.Errorf("%s: a skipped refresh still planned changes: %+v", test.name, plan)
		}
		if !test.skipped && len(plan.retired) != 1 {
			t.Errorf("%s: retired = %v, want bad-site", test.name, testChangeNames(plan.retired))
		}
	}
}

// A refresh never reads a tally day from before the tally left out the runs
// that reached no provider: it reads from its window's first day, or from the
// first reached-only day when that is later, and a site on probation since
// before that day is judged from it.
func TestEgressRefreshTallyStartLeavesOutDaysBeforeTheReachedOnlyTally(t *testing.T) {
	contactedSince := time.Date(2026, time.October, 14, 0, 0, 0, 0, time.UTC)
	window := 3 * 24 * time.Hour
	for _, test := range []struct {
		name  string
		start time.Time
		want  time.Time
	}{
		{name: "the window of the run that skipped at 0.652", start: model.ProviderEgressSiteTallyWindowStart(time.Date(2026, time.October, 14, 3, 26, 8, 0, time.UTC), window), want: contactedSince},
		{name: "the window of the next run", start: model.ProviderEgressSiteTallyWindowStart(time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC), window), want: contactedSince},
		{name: "the first window wholly after it", start: model.ProviderEgressSiteTallyWindowStart(time.Date(2026, time.October, 17, 3, 26, 11, 0, time.UTC), window), want: contactedSince},
		{name: "a later window", start: model.ProviderEgressSiteTallyWindowStart(time.Date(2026, time.October, 20, 3, 26, 14, 0, time.UTC), window), want: time.Date(2026, time.October, 17, 0, 0, 0, 0, time.UTC)},
		{name: "a probation since before it", start: time.Date(2026, time.October, 9, 3, 26, 3, 0, time.UTC), want: contactedSince},
		{name: "a probation since after it", start: time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC), want: time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC)},
	} {
		if got := egressRefreshTallyStart(test.start, contactedSince); !got.Equal(test.want) {
			t.Errorf("%s: reads from %s, want %s", test.name, got, test.want)
		}
	}
	// the tally query compares whole UTC days
	if since := model.ProviderEgressSiteTallyContactedSince; !since.Equal(since.UTC().Truncate(24 * time.Hour)) {
		t.Errorf("the first reached-only tally day %s is not a whole UTC day", since)
	}
}

// The tally reads of a refresh start no earlier than the first reached-only
// day: the window's site tally, and the per-day totals of the sites on
// probation, even of one promoted before it. A later refresh reads from its
// own window's first day and from a later promotion, and never from before
// the retention. The known incomplete day/places go to both reads.
func TestReadEgressRefreshInputReadsNoTallyDayBeforeTheReachedOnlyDay(t *testing.T) {
	reachedSince := time.Date(2026, time.October, 14, 0, 0, 0, 0, time.UTC)
	excluded := []model.ProviderEgressTallyExclusion{{Day: "2026-10-14", CountryCode: "de"}}
	probation := func(name string, promotedTime time.Time) *model.ProviderEgressDestination {
		d := testRefreshSite(name, "news")
		d.Probation = true
		d.PromotedTime = &promotedTime
		return d
	}
	for _, test := range []struct {
		name           string
		now            time.Time
		destinations   []*model.ProviderEgressDestination
		tallyFrom      time.Time
		probationFrom  time.Time
		probationNames []string
	}{
		{
			name: "the next refresh, beside a probation from before the reached-only day",
			now:  time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC),
			destinations: []*model.ProviderEgressDestination{
				testRefreshSite("scored-site", "news"),
				probation("early-probation", time.Date(2026, time.October, 9, 3, 26, 3, 0, time.UTC)),
				probation("late-probation", time.Date(2026, time.October, 14, 3, 26, 8, 0, time.UTC)),
			},
			tallyFrom:      reachedSince,
			probationFrom:  reachedSince,
			probationNames: []string{"early-probation", "late-probation"},
		},
		{
			name: "a later refresh",
			now:  time.Date(2026, time.October, 20, 3, 26, 14, 0, time.UTC),
			destinations: []*model.ProviderEgressDestination{
				testRefreshSite("scored-site", "news"),
				probation("later-probation", time.Date(2026, time.October, 18, 3, 26, 12, 0, time.UTC)),
			},
			tallyFrom:      time.Date(2026, time.October, 17, 0, 0, 0, 0, time.UTC),
			probationFrom:  time.Date(2026, time.October, 18, 3, 26, 12, 0, time.UTC),
			probationNames: []string{"later-probation"},
		},
		{
			name:           "a probation older than the retention",
			now:            time.Date(2026, time.November, 10, 3, 26, 35, 0, time.UTC),
			destinations:   []*model.ProviderEgressDestination{probation("long-probation", time.Date(2026, time.October, 20, 3, 26, 14, 0, time.UTC))},
			tallyFrom:      time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC),
			probationFrom:  time.Date(2026, time.October, 27, 0, 0, 0, 0, time.UTC),
			probationNames: []string{"long-probation"},
		},
	} {
		var tallyFrom, probationFrom time.Time
		var probationNames []string
		var tallyExcluded, probationExcluded []model.ProviderEgressTallyExclusion
		reads := egressRefreshTallyReads{
			siteTallies: func(_ context.Context, minDay time.Time, excluded ...model.ProviderEgressTallyExclusion) []model.ProviderEgressSiteTally {
				tallyFrom, tallyExcluded = minDay, excluded
				return nil
			},
			siteDayTotals: func(_ context.Context, minDay time.Time, names []string, excluded ...model.ProviderEgressTallyExclusion) []model.ProviderEgressSiteDayTotal {
				probationFrom, probationNames, probationExcluded = minDay, names, excluded
				return nil
			},
		}
		input := readEgressRefreshInput(context.Background(), test.now, model.DefaultProviderEgressSiteSettings(), test.destinations, reachedSince, excluded, model.ProviderEgressContactedTotal{}, reads)
		if !tallyFrom.Equal(test.tallyFrom) {
			t.Errorf("%s: the site tally is read from %s, want %s", test.name, tallyFrom, test.tallyFrom)
		}
		if !probationFrom.Equal(test.probationFrom) || !slices.Equal(probationNames, test.probationNames) {
			t.Errorf("%s: the probation totals of %v are read from %s, want %v from %s", test.name, probationNames, probationFrom, test.probationNames, test.probationFrom)
		}
		if !slices.Equal(tallyExcluded, excluded) || !slices.Equal(probationExcluded, excluded) {
			t.Errorf("%s: the reads left out %v and %v, want %v", test.name, tallyExcluded, probationExcluded, excluded)
		}
		if !input.reachedSince.Equal(reachedSince) || !input.now.Equal(test.now) {
			t.Errorf("%s: the plan input = %+v, want the reached-only day and the refresh's clock", test.name, input)
		}
	}
}

// The refreshes of 2026-10-14 and 2026-10-15 in synthetic numbers of the same
// shape. Every URL run loads one site, so no exit qualifies as healthy and
// each site is judged on every exit the probe reached; about two in five runs
// reach no provider. Until 2026-10-14 the tally counted those as failures of
// the site they loaded, from then on it leaves them out. Over the recent hour
// 12.2% of the loads that reached their provider failed -- the numbers the
// first refresh logged -- and over the reached-only tally 14%, so neither is
// a prober fault. Read from the window's first day, the tally's older days
// held the fleet over the prober-fault line and the refresh skipped; read from
// the first reached-only day, the refresh runs, retires the one site per
// class that refuses every provider it is sent through, keeps the other site
// over the line for a later run, and judges the steady sites on the exits the
// probe reached.
func TestPlanEgressDestinationRefreshJudgesTheReachedOnlyTallyBesideOfflineExits(t *testing.T) {
	contactedSince := time.Date(2026, time.October, 14, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return contactedSince.Add(time.Duration(n) * 24 * time.Hour) }
	settings := testRefreshSettings(14, 4)
	destinations := func() []*model.ProviderEgressDestination {
		destinations := []*model.ProviderEgressDestination{testRefreshSite("refusing-news", "news"), testRefreshSite("refusing-shopping", "shopping")}
		for i := 1; i <= 12; i++ {
			destinations = append(destinations, testRefreshSite(fmt.Sprintf("steady-%02d", i), "search"))
		}
		return destinations
	}
	// one tally day of one site at one place: before the reached-only day a
	// run that reached no provider was a load and a failure of its site
	type tallyDay struct {
		day   time.Time
		tally model.ProviderEgressSiteTally
	}
	tallyDays := []tallyDay{}
	tallied := func(on time.Time, name string, unreached int, reached int, refused int) {
		loads, failures := reached, refused
		if on.Before(contactedSince) {
			loads, failures = loads+unreached, failures+unreached
		}
		tallyDays = append(tallyDays, tallyDay{day: on, tally: model.ProviderEgressSiteTally{
			Name:         name,
			Place:        model.ProviderEgressPlace{CountryCode: "de"},
			LoadCount:    loads,
			FailureCount: failures,
		}})
	}
	// per site and day: the runs that reached no provider, those that reached
	// it, and of those the refusals of each site
	for _, d := range []struct {
		on                                      time.Time
		unreached, reached                      int
		steadyRefused, newsRefused, shopRefused int
	}{
		// the offline exits after the 2026-10-10 outage
		{on: day(-3), unreached: 280, reached: 120, steadyRefused: 1, newsRefused: 120, shopRefused: 108},
		{on: day(-2), unreached: 180, reached: 220, steadyRefused: 2, newsRefused: 220, shopRefused: 198},
		{on: day(-1), unreached: 168, reached: 232, steadyRefused: 2, newsRefused: 232, shopRefused: 209},
		// the reached-only days: the first refresh ran at 03:26 of the first
		{on: day(0), unreached: 245, reached: 300, steadyRefused: 3, newsRefused: 300, shopRefused: 270},
		{on: day(1), unreached: 33, reached: 40, steadyRefused: 0, newsRefused: 40, shopRefused: 36},
	} {
		tallied(d.on, "refusing-news", d.unreached, d.reached, d.newsRefused)
		tallied(d.on, "refusing-shopping", d.unreached, d.reached, d.shopRefused)
		for i := 1; i <= 12; i++ {
			tallied(d.on, fmt.Sprintf("steady-%02d", i), d.unreached, d.reached, d.steadyRefused)
		}
	}
	// what the tally query returns at now from a first day on: each site's
	// rows summed per place, the days dropped
	read := func(now time.Time, from time.Time) []model.ProviderEgressSiteTally {
		names := []string{}
		nameTallies := map[string]*model.ProviderEgressSiteTally{}
		for _, d := range tallyDays {
			if d.day.Before(from) || now.Before(d.day) {
				continue
			}
			sum, ok := nameTallies[d.tally.Name]
			if !ok {
				sum = &model.ProviderEgressSiteTally{Name: d.tally.Name, Place: d.tally.Place}
				nameTallies[d.tally.Name] = sum
				names = append(names, d.tally.Name)
			}
			sum.LoadCount += d.tally.LoadCount
			sum.FailureCount += d.tally.FailureCount
		}
		tallies := []model.ProviderEgressSiteTally{}
		for _, name := range names {
			tallies = append(tallies, *nameTallies[name])
		}
		return tallies
	}
	recent := model.ProviderEgressContactedTotal{Ok: 49947 - 6097, Total: 49947, NoContactFailures: 39595}

	for _, test := range []struct {
		now     time.Time
		samples int
	}{
		{now: time.Date(2026, time.October, 14, 3, 26, 8, 0, time.UTC), samples: 300},
		{now: time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC), samples: 340},
	} {
		// the refresh's own tally reads, over this fixture as of now
		reads := egressRefreshTallyReads{
			siteTallies: func(_ context.Context, minDay time.Time, _ ...model.ProviderEgressTallyExclusion) []model.ProviderEgressSiteTally {
				return read(test.now, minDay)
			},
			siteDayTotals: func(context.Context, time.Time, []string, ...model.ProviderEgressTallyExclusion) []model.ProviderEgressSiteDayTotal {
				return nil
			},
		}
		// read with no first reached-only day, as before it was known
		whole := planEgressDestinationRefresh(readEgressRefreshInput(context.Background(), test.now, settings, destinations(), time.Time{}, nil, recent, reads))
		if !strings.HasSuffix(whole.skipped, "above the prober-fault line 0.200") {
			t.Fatalf("%s: the whole window's tally, older days in, = %q, want it over the prober-fault line", test.now, whole.skipped)
		}

		plan := planEgressDestinationRefresh(readEgressRefreshInput(context.Background(), test.now, settings, destinations(), model.ProviderEgressSiteTallyContactedSince, nil, recent, reads))
		if plan.skipped != "" {
			t.Fatalf("%s: a fleet whose reached loads fail 12%% skipped its refresh: %s", test.now, plan.skipped)
		}
		if names := testChangeNames(plan.retired); !slices.Equal(names, []string{"refusing-news"}) {
			t.Fatalf("%s: retired = %v, want the site refusing every reached exit, alone in its class", test.now, names)
		}
		wantReason := fmt.Sprintf("failed 100.0%% of %d contacted exits, healthy or not (too few healthy exits to judge on) since 2026-10-14", test.samples)
		if reason := plan.rows["refusing-news"].RetireReason; reason != wantReason {
			t.Errorf("%s: retire reason = %q, want %q", test.now, reason, wantReason)
		}
		if kept := plan.rows["refusing-shopping"]; !kept.Active || kept.AboveRetireSince == nil || kept.SampleCount != test.samples {
			t.Errorf("%s: the other site over the line = %+v, want kept for a later run, over the line since now", test.now, kept)
		}
		for i := 1; i <= 12; i++ {
			steady := plan.rows[fmt.Sprintf("steady-%02d", i)]
			wantShare := 3.0 / float64(test.samples)
			if !steady.Active || steady.SampleCount != test.samples || steady.FailureShare == nil || 1e-9 < math.Abs(*steady.FailureShare-wantShare) || steady.AboveRetireSince != nil {
				t.Fatalf("%s: %s = %+v, want judged at %.4f over the %d exits the probe reached", test.now, steady.Name, steady, wantShare, test.samples)
			}
		}
		if len(plan.needs) != 1 || plan.needs[0].category != "news" {
			t.Fatalf("%s: openings = %+v, want one news site in the retired site's place", test.now, plan.needs)
		}
	}
}

// A real prober fault fails the loads that reach their provider broadly: six
// in ten of the recent ones, beside the usual two in five runs reaching no
// provider, or six in ten over the reached-only tally. The refresh skips and
// changes nothing, whichever shows it.
func TestPlanEgressDestinationRefreshSkipsWhileReachedLoadsFailBroadly(t *testing.T) {
	healthyTally := []model.ProviderEgressSiteTally{
		testHealthyTally("refusing-site", "de", "", 250, 250),
		testHealthyTally("steady-site", "de", "", 5000, 50),
	}
	faultyTally := []model.ProviderEgressSiteTally{
		testHealthyTally("refusing-site", "de", "", 250, 250),
		testHealthyTally("steady-site", "de", "", 5000, 2900),
	}
	for _, test := range []struct {
		name   string
		recent model.ProviderEgressContactedTotal
		tally  []model.ProviderEgressSiteTally
	}{
		{name: "the recent loads", recent: model.ProviderEgressContactedTotal{Ok: 400, Total: 1000, NoContactFailures: 820}, tally: healthyTally},
		{name: "the reached-only tally", recent: model.ProviderEgressContactedTotal{Ok: 880, Total: 1000, NoContactFailures: 820}, tally: faultyTally},
	} {
		now := time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC)
		plan := planEgressDestinationRefresh(egressRefreshInput{
			now:           now,
			settings:      testRefreshSettings(2, 1),
			destinations:  []*model.ProviderEgressDestination{testRefreshSite("refusing-site", "news"), testRefreshSite("steady-site", "news")},
			windowTallies: test.tally,
			reachedSince:  time.Date(2026, time.October, 14, 0, 0, 0, 0, time.UTC),
			recent:        test.recent,
		})
		if !strings.Contains(plan.skipped, " is 0.600") || !strings.HasSuffix(plan.skipped, "above the prober-fault line 0.200") {
			t.Errorf("%s: skipped = %q, want the share of 0.600 held over the prober-fault line", test.name, plan.skipped)
		}
		if 0 < len(plan.rows) || 0 < len(plan.retired) || 0 < len(plan.needs) || 0 < len(plan.judgements) {
			t.Errorf("%s: a skipped refresh still planned changes: %+v", test.name, plan)
		}
	}
}

// A fault on the prober or its tunnel path that ends runs before any
// destination answers is in no failure share: through the 2026-10-10 outage
// under one in seven runs reached its provider, while the loads that did
// failed about one in ten, under the prober-fault line. The refresh skips
// while fewer than the prober-fault floor of the recent loads reach their
// provider, and runs on the floor and in a healthy fleet.
func TestPlanEgressDestinationRefreshSkipsWhileTooFewRunsReachTheirProvider(t *testing.T) {
	for _, test := range []struct {
		name    string
		recent  model.ProviderEgressContactedTotal
		skipped string
	}{
		{
			name:    "the outage",
			recent:  model.ProviderEgressContactedTotal{Ok: 90, Total: 100, NoContactFailures: 9900},
			skipped: "only 0.010 of the 10000 loads of the last 1h0m0s reached their provider (9900 no-contact failures), under the prober-fault floor 0.200",
		},
		{
			name:    "every run reaching no provider",
			recent:  model.ProviderEgressContactedTotal{NoContactFailures: 500},
			skipped: "only 0.000 of the 500 loads of the last 1h0m0s reached their provider (500 no-contact failures), under the prober-fault floor 0.200",
		},
		{
			name:    "just under the floor",
			recent:  model.ProviderEgressContactedTotal{Ok: 199, Total: 199, NoContactFailures: 801},
			skipped: "only 0.199 of the 1000 loads of the last 1h0m0s reached their provider (801 no-contact failures), under the prober-fault floor 0.200",
		},
		{name: "on the floor", recent: model.ProviderEgressContactedTotal{Ok: 200, Total: 200, NoContactFailures: 800}},
		{name: "a healthy fleet", recent: model.ProviderEgressContactedTotal{Ok: 49947 - 6097, Total: 49947, NoContactFailures: 39595}},
	} {
		plan := planEgressDestinationRefresh(egressRefreshInput{
			now:          testRefreshNow,
			settings:     testRefreshSettings(2, 1),
			destinations: []*model.ProviderEgressDestination{testRefreshSite("refusing-site", "news"), testRefreshSite("steady-site", "news")},
			windowTallies: []model.ProviderEgressSiteTally{
				testHealthyTally("refusing-site", "de", "", 250, 250),
				testHealthyTally("steady-site", "de", "", 5000, 50),
			},
			recent: test.recent,
		})
		if plan.skipped != test.skipped {
			t.Errorf("%s: skipped = %q, want %q", test.name, plan.skipped, test.skipped)
		}
		if retired := testChangeNames(plan.retired); test.skipped == "" && !slices.Equal(retired, []string{"refusing-site"}) {
			t.Errorf("%s: retired = %v, want the refusing site", test.name, retired)
		} else if test.skipped != "" && (0 < len(plan.rows) || 0 < len(retired)) {
			t.Errorf("%s: a skipped refresh still planned changes: %+v", test.name, plan)
		}
	}
}

// A site on probation is judged on its loads since the day it was promoted:
// passing half of them it counts from now on, under half it is retired first
// in its class, and short of the minimum nothing is decided.
func TestPlanEgressDestinationRefreshDecidesProbation(t *testing.T) {
	promotedTime := testRefreshNow.Add(-5 * 24 * time.Hour)
	probation := func(name string, category string) *model.ProviderEgressDestination {
		d := testRefreshSite(name, category)
		d.Probation = true
		d.PromotedTime = &promotedTime
		return d
	}
	dayTotal := func(name string, day time.Time, loads int, failures int) model.ProviderEgressSiteDayTotal {
		return model.ProviderEgressSiteDayTotal{
			Day:                 day.Truncate(24 * time.Hour),
			Name:                name,
			LoadCount:           loads,
			FailureCount:        failures,
			HealthyLoadCount:    loads,
			HealthyFailureCount: failures,
		}
	}
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:      testRefreshNow,
		settings: testRefreshSettings(5, 2),
		destinations: []*model.ProviderEgressDestination{
			probation("new-good", "news"),
			probation("new-bad", "video"),
			probation("new-thin", "search"),
			testRefreshSite("bad-scored", "shopping"),
			testRefreshSite("steady-site", "search"),
		},
		windowTallies: []model.ProviderEgressSiteTally{
			testHealthyTally("bad-scored", "de", "", 250, 150),
			testHealthyTally("steady-site", "de", "", 5000, 0),
		},
		probationTotals: []model.ProviderEgressSiteDayTotal{
			// loads before the promotion day are another life of the site
			dayTotal("new-good", promotedTime.Add(-5*24*time.Hour), 1000, 1000),
			dayTotal("new-good", promotedTime.Add(24*time.Hour), 220, 60),
			dayTotal("new-bad", promotedTime.Add(24*time.Hour), 220, 180),
			dayTotal("new-thin", promotedTime.Add(24*time.Hour), 50, 10),
		},
	})
	if names := testChangeNames(plan.graduated); !slices.Equal(names, []string{"new-good"}) {
		t.Fatalf("graduated = %v, want new-good", names)
	}
	if plan.rows["new-good"].Probation || !plan.rows["new-good"].Active {
		t.Fatalf("the graduated row = %+v, want active and scored", plan.rows["new-good"])
	}
	if names := testChangeNames(plan.retired); !slices.Equal(names, []string{"new-bad"}) {
		t.Fatalf("retired = %v, want the failed probation first and alone in its class", names)
	}
	if !strings.HasPrefix(plan.rows["new-bad"].RetireReason, "failed probation") {
		t.Errorf("retire reason = %q", plan.rows["new-bad"].RetireReason)
	}
	if thin := plan.rows["new-thin"]; !thin.Probation || !thin.Active {
		t.Errorf("a probation short of the minimum was decided: %+v", thin)
	}
	if scored := plan.rows["bad-scored"]; !scored.Active || scored.AboveRetireSince == nil {
		t.Errorf("the scored site over the line = %+v, want kept for the next run", scored)
	}
	if len(plan.needs) != 1 || plan.needs[0].category != "video" {
		t.Fatalf("openings = %+v, want one video site in new-bad's place", plan.needs)
	}
}

// A site promoted before the first reached-only tally day is judged on its
// loads from that day on, and its verdict says so: the days before it count
// every run that reached no provider as a failure, and over them it would
// fail probation. A site promoted since is judged since its promotion.
func TestPlanEgressDestinationRefreshJudgesAProbationFromTheFirstReachedOnlyDay(t *testing.T) {
	reachedSince := time.Date(2026, time.October, 14, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC)
	probation := func(name string, promotedTime time.Time) *model.ProviderEgressDestination {
		d := testRefreshSite(name, "news")
		d.Probation = true
		d.PromotedTime = &promotedTime
		return d
	}
	// a site's loads on one day; no single-URL run's exit qualifies as healthy
	dayTotal := func(name string, day time.Time, loads int, failures int) model.ProviderEgressSiteDayTotal {
		return model.ProviderEgressSiteDayTotal{Day: day, Name: name, LoadCount: loads, FailureCount: failures}
	}
	totals := []model.ProviderEgressSiteDayTotal{}
	for day := time.Date(2026, time.October, 9, 0, 0, 0, 0, time.UTC); day.Before(reachedSince); day = day.Add(24 * time.Hour) {
		// 180 of the 400 runs reached no provider and were tallied failed
		totals = append(totals, dayTotal("early-probation", day, 400, 180+50))
	}
	for _, name := range []string{"early-probation", "late-probation"} {
		totals = append(totals, dayTotal(name, reachedSince, 150, 30), dayTotal(name, reachedSince.Add(24*time.Hour), 70, 20))
	}
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:      now,
		settings: testRefreshSettings(3, 1),
		destinations: []*model.ProviderEgressDestination{
			probation("early-probation", time.Date(2026, time.October, 9, 3, 26, 3, 0, time.UTC)),
			probation("late-probation", time.Date(2026, time.October, 14, 3, 26, 8, 0, time.UTC)),
			testRefreshSite("steady-site", "search"),
		},
		windowTallies:   []model.ProviderEgressSiteTally{testHealthyTally("steady-site", "de", "", 5000, 50)},
		reachedSince:    reachedSince,
		probationTotals: totals,
	})
	if names := testChangeNames(plan.retired); 0 < len(names) {
		t.Fatalf("retired = %v, want both probations passed on the loads that reached their provider", names)
	}
	details := map[string]string{}
	for _, change := range plan.graduated {
		details[change.name] = change.detail
	}
	for name, want := range map[string]string{
		"early-probation": "passed 77.3% of 220 contacted exits, healthy or not (too few healthy exits to judge on) since 2026-10-14",
		"late-probation":  "passed 77.3% of 220 contacted exits, healthy or not (too few healthy exits to judge on) since its promotion",
	} {
		if details[name] != want {
			t.Errorf("%s graduated with %q, want %q", name, details[name], want)
		}
		if row := plan.rows[name]; row.Probation || !row.Active {
			t.Errorf("%s after the plan = %+v, want active and scored", name, row)
		}
	}
}

// A site that fails nine in ten healthy exits of a place where the other
// sites pass is marked incompatible there -- the country when the country
// qualifies, else the region -- while a place where every site fails is
// never marked; the marks open a promotion for the thinned place.
func TestPlanEgressDestinationRefreshMarksABlockedPlaceOnlyWhereMostSitesPass(t *testing.T) {
	names := []string{"blocked-site", "site-b", "site-c", "site-d"}
	destinations := []*model.ProviderEgressDestination{}
	tallies := []model.ProviderEgressSiteTally{}
	for _, name := range names {
		destinations = append(destinations, testRefreshSite(name, "news"))
		tallies = append(tallies,
			testHealthyTally(name, "de", "", 1000, 0),
			testHealthyTally(name, "ir", "", 30, 30),
			testHealthyTally(name, "us", "Texas", 100, 0),
		)
		if name == "blocked-site" {
			tallies = append(tallies,
				testHealthyTally(name, "cn", "", 30, 29),
				testHealthyTally(name, "us", "California", 30, 30),
			)
		} else {
			tallies = append(tallies,
				testHealthyTally(name, "cn", "", 30, 0),
				testHealthyTally(name, "us", "California", 30, 0),
			)
		}
	}
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:           testRefreshNow,
		settings:      testRefreshSettings(4, 4),
		destinations:  destinations,
		windowTallies: tallies,
	})
	if len(plan.retired) != 0 {
		t.Fatalf("retired = %v, want a blocked site kept", testChangeNames(plan.retired))
	}
	marks := plan.rows["blocked-site"].Incompatible
	if len(marks) != 2 ||
		marks[0].Country != "cn" || marks[0].Region != "" ||
		marks[1].Country != "us" || marks[1].Region != "California" {
		t.Fatalf("marks = %+v, want cn and us/California", marks)
	}
	for _, mark := range marks {
		if mark.MarkedAt == nil || !mark.MarkedAt.Equal(testRefreshNow) {
			t.Errorf("a learned mark does not say when it was learned: %+v", mark)
		}
	}
	for _, name := range names[1:] {
		if 0 < len(plan.rows[name].Incompatible) {
			t.Errorf("%s was marked where every site fails: %+v", name, plan.rows[name].Incompatible)
		}
	}
	// cn now has three compatible sites for a sample of four, and one place
	// opening per class per run
	if len(plan.needs) != 1 || plan.needs[0].place == nil || plan.needs[0].place.CountryCode != "cn" {
		t.Fatalf("openings = %+v, want one for cn", plan.needs)
	}
}

// A learned mark whose canaries have passed for the cooldown is lifted; one
// whose canaries fail restarts its streak; a declared mark is the
// operator's; and a window without canary loads decides nothing.
func TestPlanEgressDestinationRefreshLiftsALearnedPlaceAfterTheCanaryCooldown(t *testing.T) {
	markedAt := testRefreshNow.Add(-60 * 24 * time.Hour)
	longPassing := testRefreshNow.Add(-31 * 24 * time.Hour)
	site := testRefreshSite("marked-site", "news")
	site.Incompatible = []model.ProviderEgressDestinationPlace{
		{Country: "cn", MarkedAt: &markedAt, CanaryPassingSince: &longPassing},
		{Country: "ir", MarkedAt: &markedAt, CanaryPassingSince: &longPassing},
		{Country: "kp"},
		{Country: "ru", MarkedAt: &markedAt},
		{Country: "by", MarkedAt: &markedAt, CanaryPassingSince: &longPassing},
	}
	canary := func(country string, loads int, passes int) model.ProviderEgressSiteTally {
		return model.ProviderEgressSiteTally{
			Name:            "marked-site",
			Place:           model.ProviderEgressPlace{CountryCode: country},
			CanaryLoadCount: loads,
			CanaryPassCount: passes,
		}
	}
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:          testRefreshNow,
		settings:     testRefreshSettings(1, 1),
		destinations: []*model.ProviderEgressDestination{site},
		windowTallies: []model.ProviderEgressSiteTally{
			testHealthyTally("marked-site", "de", "", 1000, 0),
			canary("cn", 10, 9),
			canary("ir", 10, 2),
			canary("kp", 10, 10),
			canary("ru", 10, 9),
		},
	})
	if len(plan.unmarked) != 1 || !strings.HasPrefix(plan.unmarked[0].detail, "cn:") {
		t.Fatalf("unmarked = %+v, want cn alone", plan.unmarked)
	}
	byCountry := map[string]model.ProviderEgressDestinationPlace{}
	for _, mark := range plan.rows["marked-site"].Incompatible {
		byCountry[mark.Country] = mark
	}
	if _, ok := byCountry["cn"]; ok || len(byCountry) != 4 {
		t.Fatalf("marks left = %+v, want every mark but cn", byCountry)
	}
	if byCountry["ir"].CanaryPassingSince != nil {
		t.Error("failing canaries did not end the passing streak")
	}
	if byCountry["kp"].MarkedAt != nil || byCountry["kp"].CanaryPassingSince != nil {
		t.Error("the refresh took over a declared mark")
	}
	if since := byCountry["ru"].CanaryPassingSince; since == nil || !since.Equal(testRefreshNow) {
		t.Errorf("passing canaries did not start a streak: %v", since)
	}
	if since := byCountry["by"].CanaryPassingSince; since == nil || !since.Equal(longPassing) {
		t.Errorf("a window without canaries changed the streak: %v", since)
	}
	if site.Incompatible[0].Country != "cn" {
		t.Fatal("the plan changed the pool it was given")
	}
}

// A class under its pool size opens promotions, at most the retirement cap
// per class per run.
func TestPlanEgressDestinationRefreshOpensPromotionsForAShortClass(t *testing.T) {
	for _, maxPerRun := range []int{1, 2} {
		settings := testRefreshSettings(5, 1)
		settings.SitePoolSize["dns"] = 2
		settings.SiteMaxRetirePerRun = maxPerRun
		resolver := &model.ProviderEgressDestination{Name: "resolver", Class: "dns", Category: "resolver", Active: true}
		plan := planEgressDestinationRefresh(egressRefreshInput{
			now:      testRefreshNow,
			settings: settings,
			destinations: []*model.ProviderEgressDestination{
				testRefreshSite("site-a", "news"), testRefreshSite("site-b", "news"), testRefreshSite("site-c", "news"), resolver,
			},
			windowTallies: []model.ProviderEgressSiteTally{testHealthyTally("site-a", "de", "", 1000, 0)},
		})
		counts := map[string]int{}
		for _, need := range plan.needs {
			if need.category != "" || need.place != nil {
				t.Errorf("a size opening asks for a category or place: %+v", need)
			}
			counts[need.class]++
		}
		if counts["site"] != maxPerRun || counts["dns"] != 1 || len(counts) != 2 {
			t.Errorf("openings at %d per run = %v, want %d site and 1 dns", maxPerRun, counts, maxPerRun)
		}
	}
}

// The promotion order: the opening's category, then the candidates that work
// at the most thin places, then those never retired, then the oldest; a
// candidate cooling down, dropped for good, active, of another class or
// already taken is never offered.
func TestOrderEgressCandidatesPromotionOrder(t *testing.T) {
	settings := testRefreshSettings(10, 2)
	day := func(n int) time.Time { return testRefreshNow.Add(time.Duration(n-100) * 24 * time.Hour) }
	candidate := func(name string, category string, added int) *model.ProviderEgressDestination {
		return &model.ProviderEgressDestination{Name: name, Class: "site", Category: category, AddedTime: day(added)}
	}
	retiredLongAgo := testRefreshNow.Add(-40 * 24 * time.Hour)
	retiredRecently := testRefreshNow.Add(-10 * 24 * time.Hour)
	video := candidate("video-candidate", "video", 1)
	oldRetired := candidate("news-retired", "news", 0)
	oldRetired.RetiredTime, oldRetired.RetireCount = &retiredLongAgo, 1
	fresh := candidate("news-fresh", "news", 2)
	blocked := candidate("news-blocked", "news", 1)
	blocked.Incompatible = []model.ProviderEgressDestinationPlace{{Country: "cn"}}
	cooling := candidate("news-cooling", "news", 0)
	cooling.RetiredTime, cooling.RetireCount = &retiredRecently, 1
	dropped := candidate("news-dropped", "news", 0)
	dropped.RetiredTime, dropped.RetireCount = &retiredLongAgo, 3
	active := candidate("news-active", "news", 0)
	active.Active = true
	otherClass := candidate("cdn-candidate", "news", 0)
	otherClass.Class = "cdn"
	taken := candidate("news-taken", "news", 0)
	destinations := []*model.ProviderEgressDestination{video, oldRetired, fresh, blocked, cooling, dropped, active, otherClass, taken}
	thin := []model.ProviderEgressPlace{{CountryCode: "cn"}}
	names := func(ordered []*model.ProviderEgressDestination) []string {
		out := []string{}
		for _, d := range ordered {
			out = append(out, d.Name)
		}
		return out
	}

	ordered := orderEgressCandidates(destinations, egressPromotionNeed{class: "site", category: "news"}, thin, testRefreshNow, settings, map[string]bool{"news-taken": true})
	if got := names(ordered); !slices.Equal(got, []string{"news-fresh", "news-retired", "news-blocked", "video-candidate"}) {
		t.Fatalf("order for a news opening = %v", got)
	}

	cn := model.ProviderEgressPlace{CountryCode: "cn"}
	ordered = orderEgressCandidates(destinations, egressPromotionNeed{class: "site", place: &cn}, thin, testRefreshNow, settings, nil)
	if got := names(ordered); slices.Contains(got, "news-blocked") || len(got) != 4 {
		t.Fatalf("order for an opening at cn = %v, want no candidate known to fail there", got)
	}
}

// One refresh against the database: the pool seeds from an explicit catalog,
// the candidates sync in, a news site failing the healthy exits is retired,
// the news candidate that loads cleanly from the host is promoted on
// probation, and the served pool and the scoring follow. A second refresh the
// same day changes nothing.
func TestRefreshEgressDestinationsRetiresAndPromotesFromTheTally(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pop := server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshCatalog+testRefreshCatalogSize+`
candidates:
  - name: news-candidate
    class: site
    category: news
    region: europe
    url: https://news.example/robots.txt
    expect: body
    max_bytes: 1024
    revision: 1
  - name: video-candidate
    class: site
    category: video
    url: https://video.example/robots.txt
    revision: 1
`))
		t.Cleanup(pop)

		now := server.NowUtc()
		day := now.UTC().Truncate(24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				name            string
				loads, failures int
			}{
				{name: "synthetic-retiring-news", loads: 250, failures: 150},
				{name: "synthetic-steady-news", loads: 5000, failures: 0},
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_egress_site_tally (
						tally_day, name, country_code, region,
						load_count, failure_count, healthy_load_count, healthy_failure_count, update_time
					)
					VALUES ($1, $2, 'de', '', $3, $4, $3, $4, $5)
					`,
					day, row.name, row.loads, row.failures, now,
				))
			}
		})

		// the host checks of one refresh run at once
		var stateLock sync.Mutex
		checked := []string{}
		check := func(_ context.Context, candidate egresshealth.Destination, _ egresshealth.RequestProfile, _ int, _ time.Duration) error {
			stateLock.Lock()
			defer stateLock.Unlock()
			checked = append(checked, candidate.Name)
			return nil
		}
		result, err := refreshEgressDestinations(ctx, now, check)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if !result.Seeded || result.Synced != 2 || result.Retired != 1 || result.Promoted != 1 || result.Unfilled != 0 {
			t.Fatalf("refresh result = %+v, want seeded, 2 synced, the failing site retired and one promotion", result)
		}
		slices.Sort(checked)
		if !slices.Equal(checked, []string{"news-candidate", "video-candidate"}) {
			t.Fatalf("host checks = %v, want the class's candidates", checked)
		}

		byName := map[string]*model.ProviderEgressDestination{}
		for _, d := range model.GetProviderEgressDestinations(ctx) {
			byName[d.Name] = d
		}
		if retired := byName["synthetic-retiring-news"]; retired.Active || retired.RetireCount != 1 || retired.RetiredTime == nil {
			t.Fatalf("failing site after the refresh = %+v, want retired", retired)
		}
		if promoted := byName["news-candidate"]; !promoted.Active || !promoted.Probation || promoted.PromotedTime == nil {
			t.Fatalf("news-candidate after the refresh = %+v, want promoted on probation", promoted)
		}
		if video := byName["video-candidate"]; video.Active {
			t.Fatalf("the video candidate was promoted into a news opening: %+v", video)
		}

		pool, err := controller.GetProviderEgressDestinationPool(ctx)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		served := map[string]bool{}
		for _, destination := range pool.Destinations {
			served[destination.Name] = true
		}
		if served["synthetic-retiring-news"] || !served["news-candidate"] || served["video-candidate"] {
			t.Fatalf("served pool holds retired=%t news-candidate=%t video-candidate=%t", served["synthetic-retiring-news"], served["news-candidate"], served["video-candidate"])
		}
		if model.GetProviderEgressHealthScoring(ctx).Scores("news-candidate", model.ProviderEgressPlace{CountryCode: "de"}) {
			t.Fatal("a site on probation scores")
		}

		checked = nil
		again, err := refreshEgressDestinations(ctx, now, check)
		if err != nil {
			t.Fatalf("second refresh: %v", err)
		}
		if again.Seeded || again.Retired != 0 || again.Promoted != 0 || 0 < len(checked) {
			t.Fatalf("a second refresh changed the pool: %+v, checked %v", again, checked)
		}
	})
}

// A max time that does not cover one host check refuses the refresh before
// anything is read or written.
func TestRefreshEgressDestinationsRefusesAMaxTimeShorterThanOneCheck(t *testing.T) {
	pop := server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshCatalog+`
settings:
  site_refresh_max_time_seconds: 60
`))
	t.Cleanup(pop)
	check := func(context.Context, egresshealth.Destination, egresshealth.RequestProfile, int, time.Duration) error {
		t.Fatal("a refused refresh checked a candidate")
		return nil
	}
	if _, err := refreshEgressDestinations(context.Background(), testRefreshNow, check); err == nil || !strings.Contains(err.Error(), "site_refresh_max_time_seconds") {
		t.Fatalf("refresh error = %v, want the max time refused", err)
	}
}

// A stored candidate the prober could not run -- a row an older module
// accepted and a newer one refuses -- is refused before any host check
// starts, so its refusal cannot race the checks' results (run with -race),
// and the opening is filled from the candidates that load.
func TestRefreshEgressDestinationsRefusesAnUnrunnableCandidateBeforeChecking(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// no candidates from a workstation's qualityprobe.yml
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshCatalog+testRefreshCatalogSize+"candidates: []\n")))
		if _, err := controller.EnsureProviderEgressDestinationsSeeded(ctx); err != nil {
			t.Fatal(err)
		}
		now := server.NowUtc()
		for _, name := range []string{"news-candidate", "news-unrunnable"} {
			row := &model.ProviderEgressDestination{
				Name:      name,
				Class:     "site",
				Url:       "https://" + name + ".example/robots.txt",
				Expect:    "body",
				Category:  "news",
				Region:    "global",
				Source:    model.ProviderEgressDestinationSourceCandidates,
				AddedTime: now.Add(-time.Hour),
			}
			if name == "news-unrunnable" {
				row.Expect = "sometimes"
			}
			model.SetProviderEgressDestination(ctx, row)
		}
		day := now.UTC().Truncate(24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				name            string
				loads, failures int
			}{
				{name: "synthetic-retiring-news", loads: 250, failures: 150},
				{name: "synthetic-steady-news", loads: 5000, failures: 0},
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_egress_site_tally (
						tally_day, name, country_code, region,
						load_count, failure_count, healthy_load_count, healthy_failure_count, update_time
					)
					VALUES ($1, $2, 'de', '', $3, $4, $3, $4, $5)
					`,
					day, row.name, row.loads, row.failures, now,
				))
			}
		})

		// the valid candidate's check holds until the refresh has converted
		// every candidate, which it now does before starting any check
		release := make(chan struct{})
		var stateLock sync.Mutex
		checked := []string{}
		check := func(_ context.Context, candidate egresshealth.Destination, _ egresshealth.RequestProfile, _ int, _ time.Duration) error {
			<-release
			stateLock.Lock()
			defer stateLock.Unlock()
			checked = append(checked, candidate.Name)
			return nil
		}
		// what the refresh returned
		type refreshOutcome struct {
			result *RefreshEgressDestinationsResult
			err    error
		}
		done := make(chan refreshOutcome, 1)
		go func() {
			result, err := refreshEgressDestinations(ctx, now, check)
			done <- refreshOutcome{result: result, err: err}
		}()
		close(release)
		outcome := <-done
		if outcome.err != nil {
			t.Fatalf("refresh: %v", outcome.err)
		}
		if outcome.result.Promoted != 1 || outcome.result.Unfilled != 0 {
			t.Fatalf("refresh result = %+v, want the opening filled", outcome.result)
		}
		if !slices.Equal(checked, []string{"news-candidate"}) {
			t.Fatalf("host checks = %v, want the runnable candidate alone", checked)
		}
		for _, d := range model.GetProviderEgressDestinations(ctx) {
			switch d.Name {
			case "news-candidate":
				if !d.Active || !d.Probation {
					t.Fatalf("the runnable candidate = %+v, want promoted on probation", d)
				}
			case "news-unrunnable":
				if d.Active {
					t.Fatalf("an unrunnable candidate was promoted: %+v", d)
				}
			}
		}
	})
}

// Half of the hour's URL runs reach no provider -- an offline exit fails its
// URL's DNS lookup through the tunnel -- and that is no prober fault: the
// refresh runs, an offline exit is in no site's tally, and the steady site is
// judged on the exits the probe reached. Counted over every run, the steady
// site's failure share would be over the retire line.
func TestRefreshEgressDestinationsJudgesBesideHalfTheRunsReachingNoProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshUrlTurnCatalog)))
		now := server.NowUtc()
		at := now.Add(-time.Minute)
		place := model.ProviderEgressPlace{CountryCode: "de"}
		testRefreshUrlTurns(ctx, at, "synthetic-steady-news", place, 100, 2, 98)

		result, err := refreshEgressDestinations(ctx, now, testRefreshNoCandidateCheck(t))
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if result.Skipped != "" {
			t.Fatalf("a fleet whose offline exits fail half the runs skipped its refresh: %s", result.Skipped)
		}
		if result.Retired != 0 {
			t.Fatalf("refresh result = %+v, want nothing retired", result)
		}
		steady := testRefreshDestinationsByName(ctx)["synthetic-steady-news"]
		if !steady.Active || steady.SampleCount != 100 || steady.FailureShare == nil || 1e-6 < math.Abs(*steady.FailureShare-0.02) ||
			steady.JudgedTime == nil || steady.AboveRetireSince != nil {
			t.Fatalf("the steady site after the refresh = %+v, want judged at 2%% over the 100 exits the probe reached", steady)
		}
		for _, tally := range model.GetProviderEgressSiteTallies(ctx, at) {
			if tally.Name == "synthetic-steady-news" && (tally.LoadCount != 100 || tally.FailureCount != 2) {
				t.Fatalf("the steady site's tally = %+v, want only the 100 runs that reached their provider", tally)
			}
		}
	})
}

// A site that refuses nearly every provider the probe reaches is retired on
// those exits, beside a steady site and half the runs reaching no provider.
// Counted over every run, the offline exits would have held the refresh back,
// and put the steady site over the retire line beside it.
func TestRefreshEgressDestinationsRetiresASiteRefusingNearlyEveryContactedExit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshUrlTurnCatalog)))
		now := server.NowUtc()
		at := now.Add(-time.Minute)
		place := model.ProviderEgressPlace{CountryCode: "de"}
		testRefreshUrlTurns(ctx, at, "synthetic-retiring-news", place, 30, 29, 1)
		testRefreshUrlTurns(ctx, at, "synthetic-steady-news", place, 170, 3, 167)

		result, err := refreshEgressDestinations(ctx, now, testRefreshNoCandidateCheck(t))
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if result.Skipped != "" || result.Retired != 1 {
			t.Fatalf("refresh result = %+v, want the refusing site retired", result)
		}
		byName := testRefreshDestinationsByName(ctx)
		refusing := byName["synthetic-retiring-news"]
		if refusing.Active || refusing.RetireCount != 1 || !strings.HasPrefix(refusing.RetireReason, "failed 96.7% of 30 contacted exits, healthy or not") {
			t.Fatalf("the refusing site after the refresh = %+v, want retired on the 30 exits the probe reached", refusing)
		}
		steady := byName["synthetic-steady-news"]
		if !steady.Active || steady.SampleCount != 170 || steady.AboveRetireSince != nil {
			t.Fatalf("the steady site after the refresh = %+v, want kept, judged on the 170 exits the probe reached", steady)
		}
	})
}

// A real prober fault fails the exits it reaches broadly, across the sites:
// the refresh skips its run and retires nothing on that evidence, however
// many runs beside it reach no provider.
func TestRefreshEgressDestinationsSkipsWhileContactedExitsFailBroadly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshUrlTurnCatalog)))
		now := server.NowUtc()
		at := now.Add(-time.Minute)
		place := model.ProviderEgressPlace{CountryCode: "de"}
		testRefreshUrlTurns(ctx, at, "synthetic-retiring-news", place, 30, 30, 0)
		testRefreshUrlTurns(ctx, at, "synthetic-steady-news", place, 100, 40, 60)

		result, err := refreshEgressDestinations(ctx, now, testRefreshNoCandidateCheck(t))
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if !strings.Contains(result.Skipped, "is 0.538 over 130 loads that reached their provider (130 no-contact failures left out), above the prober-fault line 0.200") {
			t.Fatalf("skipped = %q, want the prober-fault line held over the loads that reached their provider", result.Skipped)
		}
		if result.Judged != 0 || result.Retired != 0 {
			t.Fatalf("a skipped refresh judged or retired: %+v", result)
		}
		for name, d := range testRefreshDestinationsByName(ctx) {
			if !d.Active || d.RetireCount != 0 || d.JudgedTime != nil {
				t.Fatalf("%s was judged or retired during a prober fault: %+v", name, d)
			}
		}
	})
}

// The refresh of 2026-10-15 against the database. Its window reaches back
// over 2026-10-12 and 2026-10-13, whose tally rows hold every run that
// reached no provider as a failure of its site -- written here as the tally's
// writer wrote them then -- and would hold the fleet at 0.457, over the
// prober-fault line; from 2026-10-14 on the probe task's own URL turns are
// tallied, and the loads that reached their provider fail 15.5%. The refresh
// reads only the reached-only days: it runs, retires the site refusing nearly
// every provider it reached, and judges the steady site on the exits the probe
// reached, not on the offline ones of the older days.
func TestRefreshEgressDestinationsReadsOnlyTheReachedOnlyTally(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshUrlTurnCatalog)))
		now := time.Date(2026, time.October, 15, 3, 26, 9, 0, time.UTC)
		place := model.ProviderEgressPlace{CountryCode: "de"}
		// a run as the tally counted it before 2026-10-14: one load, failed
		// whether or not the run reached its provider
		olderTally := func(at time.Time, site string, unreached int, refused int, passes int) {
			for i := range unreached + refused + passes {
				ok := unreached+refused <= i
				model.AddProviderEgressRunTally(ctx, at, model.ProviderEgressRunTally{Place: place, Healthy: ok}, []model.ProviderEgressSiteLoad{{Name: site, Ok: ok}})
			}
		}
		for _, at := range []time.Time{
			time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC),
			time.Date(2026, time.October, 13, 12, 0, 0, 0, time.UTC),
		} {
			olderTally(at, "synthetic-steady-news", 100, 2, 98)
			olderTally(at, "synthetic-retiring-news", 20, 18, 2)
		}
		reachedOnly := time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)
		testRefreshUrlTurns(ctx, reachedOnly, "synthetic-steady-news", place, 100, 2, 118)
		testRefreshUrlTurns(ctx, reachedOnly, "synthetic-retiring-news", place, 15, 19, 1)
		// the recent hour: 10 of the 60 loads that reached their provider failed
		testRefreshUrlTurns(ctx, now.Add(-30*time.Minute), "synthetic-steady-news", place, 50, 0, 50)
		testRefreshUrlTurns(ctx, now.Add(-30*time.Minute), "synthetic-retiring-news", place, 8, 10, 0)

		result, err := refreshEgressDestinations(ctx, now, testRefreshNoCandidateCheck(t))
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if result.Skipped != "" {
			t.Fatalf("the refresh skipped on the tally days that count every offline exit as a site failure: %s", result.Skipped)
		}
		if result.Retired != 1 {
			t.Fatalf("refresh result = %+v, want the refusing site retired", result)
		}
		byName := testRefreshDestinationsByName(ctx)
		refusing := byName["synthetic-retiring-news"]
		wantReason := "failed 96.7% of 30 contacted exits, healthy or not (too few healthy exits to judge on) since 2026-10-14"
		if refusing.Active || refusing.RetireCount != 1 || refusing.RetireReason != wantReason {
			t.Fatalf("the refusing site after the refresh = %+v, want retired with %q", refusing, wantReason)
		}
		steady := byName["synthetic-steady-news"]
		if !steady.Active || steady.SampleCount != 170 || steady.FailureShare == nil || 1e-9 < math.Abs(*steady.FailureShare-2.0/170) || steady.AboveRetireSince != nil {
			t.Fatalf("the steady site after the refresh = %+v, want kept, judged at 2 of the 170 exits the probe reached", steady)
		}
	})
}
