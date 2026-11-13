package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Place-evidence findings for pairs in several countries: pairs URLs in each,
// each failing on four networks.
type testingTlsPlaceCountry struct {
	code  string
	loads int
	pairs int
}

func testingTlsPlaceCountriesFindings(now time.Time, countries []testingTlsPlaceCountry) ([]*ProviderEgressTlsFinding, []bool, map[string]int, []string) {
	findings := []*ProviderEgressTlsFinding{}
	countryLoads := map[string]int{}
	urlKeys := []string{}
	for _, country := range countries {
		countryLocationId := server.NewId()
		countryLoads[country.code] = country.loads
		for i := range country.pairs {
			urlKey := fmt.Sprintf("%s-%03d", country.code, i)
			urlKeys = append(urlKeys, urlKey)
			findings = append(findings, testingTlsPlaceFindings(now, urlKey, country.code, countryLocationId, 4)...)
		}
	}
	candidates := make([]bool, len(findings))
	for i := range candidates {
		candidates[i] = true
	}
	return findings, candidates, countryLoads, urlKeys
}

// Seeded from the follow-ups review (REVIEW-D1 F2): with more pairs needing
// place evidence than the run's pair limit, the cost order cut the costliest
// country's pairs on every run, so their place was never read (the review cut
// a United States pair on all 24 turns of two hours). Read stalest first,
// every pair is read within ceil(pairs / limit) runs, however costly its
// country.
func TestTlsPlaceReadsReachEveryPairWithinABoundedNumberOfRunsPastTheLimit(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	base := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		countries []testingTlsPlaceCountry
	}{
		{"64 cheap pairs and one costly", []testingTlsPlaceCountry{{"zz", 1000, providerEgressTlsPlaceKeyLimit}, {"us", 9000000, 1}}},
		{"three countries past twice the limit", []testingTlsPlaceCountry{{"zz", 1000, 100}, {"yy", 50000, 10}, {"us", 9000000, 20}}},
	} {
		findings, candidates, countryLoads, urlKeys := testingTlsPlaceCountriesFindings(base, c.countries)
		bound := (len(urlKeys) + providerEgressTlsPlaceKeyLimit - 1) / providerEgressTlsPlaceKeyLimit
		chain := &testingTlsPlaceReadChain{}
		lastRead := map[string]int{}
		for run := range 24 {
			now := base.Add(time.Duration(run) * 5 * time.Minute)
			fake := &testingTlsPlaceReads{countryLoads: countryLoads}
			chain.read(now, settings, findings, candidates, fake.reads())
			if len(fake.read) != providerEgressTlsPlaceKeyLimit {
				t.Fatalf("%s: run %d read %d pairs, want the limit, %d", c.name, run, len(fake.read), providerEgressTlsPlaceKeyLimit)
			}
			for _, urlKey := range fake.read {
				lastRead[urlKey] = run
			}
			for _, urlKey := range urlKeys {
				last, ok := lastRead[urlKey]
				if !ok {
					last = -1
				}
				if bound <= run-last {
					t.Fatalf("%s: after run %d the pair %s was last read on run %d, want every pair read within %d runs",
						c.name, run, urlKey, last, bound)
				}
			}
		}
	}
}

// A pair whose read never completes cannot spend every run's budget either:
// the clock counts a read from when it began, so the pair goes behind the
// others once tried. Without that order, the one pair in the cheapest
// country was tried first on every run, waited out the budget, and no other
// pair was ever read.
func TestTlsPlaceReadsReachEveryPairWithinABoundedNumberOfRunsPastTheBudget(t *testing.T) {
	settings := ProviderEgressTlsJudgeSettingsFor(DefaultProviderEgressSiteSettings(), testingTlsCommonModeNetworkCount)
	settings.PlaceReadBudget = 100 * time.Millisecond
	base := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	findings, candidates, countryLoads, urlKeys := testingTlsPlaceCountriesFindings(base,
		[]testingTlsPlaceCountry{{"zs", 100, 1}, {"zm", 5000, 5}})
	const bound = 2
	chain := &testingTlsPlaceReadChain{}
	lastTried := map[string]int{}
	for run := range 12 {
		now := base.Add(time.Duration(run) * 5 * time.Minute)
		fake := &testingTlsPlaceReads{countryLoads: countryLoads, waiting: map[string]bool{"zs-000": true}}
		evidence := chain.read(now, settings, findings, candidates, fake.reads())
		for _, urlKey := range fake.read {
			lastTried[urlKey] = run
		}
		for i, finding := range findings {
			if finding.UrlKey == "zs-000" && evidence[i].withheld != ProviderEgressTlsPlaceWithheldBudget {
				t.Fatalf("run %d: the read that never completes was withheld %q, want budget", run, evidence[i].withheld)
			}
		}
		for _, urlKey := range urlKeys {
			last, ok := lastTried[urlKey]
			if !ok {
				last = -1
			}
			if bound <= run-last {
				t.Fatalf("after run %d the pair %s was last tried on run %d, want every pair tried within %d runs", run, urlKey, last, bound)
			}
		}
	}
}
