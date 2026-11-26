package monitor

// SIGNALS.md §32.4 (CSRV-M12-06): web search's pages at their lines and not below them.

import (
	"testing"
)

// The search provider's failure rate at 5% and more over 100 searches pages; the same share over 99 does not, nor 4%
// over 100.
func TestComputeSearchFailureRatePagesFromAHundredSearches(t *testing.T) {
	for _, c := range []struct {
		all, ok float64
		pages   bool
	}{{all: 100, ok: 94, pages: true}, {all: 99, ok: 93, pages: false}, {all: 100, ok: 96, pages: false}} {
		findings := computeFamilyJudge(computeSearchFamily(), computeTestSettings("search"), []*sidecarStorageSample{
			computeTestLive(),
			computeTestSample("compute-search-failure-rate", "all", c.all, nil),
			computeTestSample("compute-search-failure-rate", "ok", c.ok, nil),
		})
		if rate := computeTestFindings(findings, "compute-search-failure-rate")["compute"]; rate.healthy == c.pages {
			t.Errorf("%.0f ok of %.0f: %+v", c.ok, c.all, rate)
		}
	}
}

// The float under a day pages; the signer's unsettled value past 80% of max_unsettled pages and 79% does not; a
// day's unattributed value past $10 pages.
func TestComputeSearchFloatUnsettledAndUnattributed(t *testing.T) {
	for _, c := range []struct {
		float, unsettled, unattributed float64
		pages                          bool
	}{
		{float: 0.9, unsettled: 81, unattributed: 11 * 1000 * 1000 * 1000, pages: true},
		{float: 1.1, unsettled: 79, unattributed: 1 * 1000 * 1000 * 1000, pages: false},
	} {
		findings := computeFamilyJudge(computeSearchFamily(), computeTestSettings("search"), []*sidecarStorageSample{
			computeTestLive(),
			computeTestSample("compute-search-float-low", "days", c.float, nil),
			computeTestSample("compute-search-unsettled-high", "unsettled", c.unsettled, nil),
			computeTestSample("compute-search-unsettled-high", "max", 100, nil),
			computeTestSample("compute-search-unattributed", "unattributed", c.unattributed, nil),
			computeTestSample("compute-search-unattributed", "settled", 1000*1000*1000*1000, nil),
			computeTestSample("compute-search-unattributed", "unbalanced", 0, nil),
		})
		for _, class := range []string{"compute-search-float-low", "compute-search-unsettled-high", "compute-search-unattributed"} {
			if found := computeTestFindings(findings, class)["compute"]; found.healthy == c.pages || found.unobservable {
				t.Errorf("%s: %+v", class, found)
			}
		}
	}
}

// Each of the signer's and the watchers' alarms pages within its window.
func TestComputeSearchAlarmsPage(t *testing.T) {
	samples := []*sidecarStorageSample{computeTestLive()}
	classes := []string{"compute-search-payer-transfer", "compute-search-nonce-unknown", "compute-search-sanctions-record", "compute-search-base-disagree", "compute-search-row-retained"}
	for _, class := range classes {
		samples = append(samples, computeTestSample(class, "alarms", 1, nil))
	}
	findings := computeFamilyJudge(computeSearchFamily(), computeTestSettings("search"), samples)
	for _, class := range classes {
		if found := computeTestFindings(findings, class)["compute"]; found.healthy {
			t.Errorf("%s: %+v", class, found)
		}
	}
}
