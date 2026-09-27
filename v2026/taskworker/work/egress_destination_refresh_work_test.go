// Tests for the daily destination pool refresh of connect/GEOMAP.md §11.4:
// the pure decision over a pool and its tally, the promotion order, and one
// refresh against the database.
package work

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The refresh clock every pure test runs at.
var testRefreshNow = time.Date(2026, time.September, 20, 6, 0, 0, 0, time.UTC)

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
func TestJudgeEgressSiteFallsBackToAllExitsAndNeedsEnoughSamples(t *testing.T) {
	for _, test := range []struct {
		name                                           string
		healthyLoads, healthyFailures, loads, failures int
		share                                          float64
		samples                                        int
		basis                                          string
	}{
		{name: "healthy exits", healthyLoads: 250, healthyFailures: 150, loads: 400, failures: 300, share: 0.6, samples: 250, basis: "healthy"},
		{name: "too few healthy", healthyLoads: 100, healthyFailures: 90, loads: 300, failures: 150, share: 0.5, samples: 300, basis: "all"},
		{name: "too few either way", healthyLoads: 100, healthyFailures: 90, loads: 150, failures: 100, share: 0.9, samples: 100, basis: "too_few"},
		{name: "only unhealthy exits", loads: 150, failures: 15, share: 0.1, samples: 150, basis: "too_few"},
	} {
		share, samples, basis := judgeEgressSite(test.healthyLoads, test.healthyFailures, test.loads, test.failures, 200)
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

// With too few healthy exits to judge on, a site is judged on every exit, and
// the plan says it was.
func TestPlanEgressDestinationRefreshJudgesOnAllExitsWhenTooFewAreHealthy(t *testing.T) {
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
		t.Fatalf("fallbacks = %v, want the judgement on all exits reported", plan.fallbacks)
	}
	if !strings.Contains(plan.rows["bad-site"].RetireReason, "healthy or not") {
		t.Errorf("retire reason does not say what it was judged on: %q", plan.rows["bad-site"].RetireReason)
	}
}

// Nothing is judged while the fleet fails too much at once, whether the
// recent runs or the window show it; on the line itself the refresh runs.
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
		recent  model.ProviderEgressClassTotal
		window  []model.ProviderEgressSiteTally
		skipped bool
	}{
		{name: "recent runs", recent: model.ProviderEgressClassTotal{Ok: 70, Total: 100}, window: quietWindow, skipped: true},
		{name: "the window", window: faultyWindow, skipped: true},
		{name: "on the line", recent: model.ProviderEgressClassTotal{Ok: 80, Total: 100}, window: quietWindow, skipped: false},
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

// One refresh against the database: the pool seeds from the built-in table,
// the candidates sync in, a news site failing the healthy exits is retired,
// the news candidate that loads cleanly from the host is promoted on
// probation, and the served pool and the scoring follow. A second refresh the
// same day changes nothing.
func TestRefreshEgressDestinationsRetiresAndPromotesFromTheTally(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pop := server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(`
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
				{name: "cnn", loads: 250, failures: 150},
				{name: "bbc", loads: 5000, failures: 0},
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
			t.Fatalf("refresh result = %+v, want seeded, 2 synced, cnn retired and one promotion", result)
		}
		slices.Sort(checked)
		if !slices.Equal(checked, []string{"news-candidate", "video-candidate"}) {
			t.Fatalf("host checks = %v, want the class's candidates", checked)
		}

		byName := map[string]*model.ProviderEgressDestination{}
		for _, d := range model.GetProviderEgressDestinations(ctx) {
			byName[d.Name] = d
		}
		if cnn := byName["cnn"]; cnn.Active || cnn.RetireCount != 1 || cnn.RetiredTime == nil {
			t.Fatalf("cnn after the refresh = %+v, want retired", cnn)
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
		if served["cnn"] || !served["news-candidate"] || served["video-candidate"] {
			t.Fatalf("served pool holds cnn=%t news-candidate=%t video-candidate=%t", served["cnn"], served["news-candidate"], served["video-candidate"])
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
	pop := server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(`
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
		// no candidates from a workstation's egress-sites.yml
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte("candidates: []\n")))
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
				{name: "cnn", loads: 250, failures: 150},
				{name: "bbc", loads: 5000, failures: 0},
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
