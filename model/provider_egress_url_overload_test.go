package model

import (
	"strings"
	"testing"
	"time"
)

// The overload judge's verdict and the window's discount
// (provider_egress_url_overload.go), pure: the tallies a run reads, the verdict
// it stores, and the SQL every URL window reads.

// A bucket as the judge tallies it: outcomes and failures, those of its worst
// country and network, and its baseline.
func overloadTestBucket(outcomeCount int, errorCount int, countryOutcomeCount int, countryErrorCount int, networkOutcomeCount int, networkErrorCount int, baselineOutcomeCount int64, baselineErrorCount int64) *ProviderEgressUrlFleetBucket {
	return &ProviderEgressUrlFleetBucket{
		OutcomeCount:         outcomeCount,
		ErrorCount:           errorCount,
		CountryOutcomeCount:  countryOutcomeCount,
		CountryErrorCount:    countryErrorCount,
		NetworkOutcomeCount:  networkOutcomeCount,
		NetworkErrorCount:    networkErrorCount,
		BaselineOutcomeCount: baselineOutcomeCount,
		BaselineErrorCount:   baselineErrorCount,
	}
}

// The 2026-11-25 overload, as one of its buckets read: 88% of the fleet's
// outcomes failed against a third normally, and about as many without its
// worst country or network. It is discounted.
func TestProviderEgressUrlOverloadDiscountsAFleetWideOverload(t *testing.T) {
	settings := DefaultProviderEgressUrlOverloadSettings()
	bucket := overloadTestBucket(4800, 4224, 900, 810, 60, 54, 380000, 125400)
	if !settings.overloaded(bucket) {
		t.Fatalf("a fleet-wide 88%% bucket against a 33%% baseline is not an overload: %+v", bucket)
	}
}

// A real provider-side outage in one large country: its providers, 45% of the
// bucket, fail every load, which takes the fleet's share to 63%, above the
// line and the margin. Without that country the rest fails at its usual third,
// so the bucket is scored and the failures count against those providers.
func TestProviderEgressUrlOverloadScoresAOneCountryOutage(t *testing.T) {
	settings := DefaultProviderEgressUrlOverloadSettings()
	// 2250 of the country's loads fail, and 907 of the other 2750
	bucket := overloadTestBucket(5000, 3157, 2250, 2250, 400, 400, 380000, 125400)
	if 0.6 >= bucket.ErrorShare() {
		t.Fatalf("the fixture's fleet share %.2f is not above the line", bucket.ErrorShare())
	}
	if settings.overloaded(bucket) {
		t.Fatal("one country's outage is discounted as an overload of the platform")
	}
}

// One operator's network of many providers failing together, across
// countries, is the same: without it the fleet fails at its usual share.
func TestProviderEgressUrlOverloadScoresOneNetworkFailing(t *testing.T) {
	settings := DefaultProviderEgressUrlOverloadSettings()
	bucket := overloadTestBucket(5000, 3157, 600, 500, 2250, 2250, 380000, 125400)
	if settings.overloaded(bucket) {
		t.Fatal("one network failing is discounted as an overload of the platform")
	}
}

// A bucket, the rest of it without its worst country or network, and a
// baseline each need MinOutcomeCount outcomes; fewer are no verdict.
func TestProviderEgressUrlOverloadNeedsEnoughOutcomes(t *testing.T) {
	settings := DefaultProviderEgressUrlOverloadSettings()
	for _, c := range []struct {
		name   string
		bucket *ProviderEgressUrlFleetBucket
	}{
		{"a thin bucket", overloadTestBucket(199, 199, 20, 20, 1, 1, 380000, 125400)},
		{"a thin rest without the worst country", overloadTestBucket(400, 400, 201, 201, 1, 1, 380000, 125400)},
		{"a thin rest without the worst network", overloadTestBucket(400, 400, 20, 20, 201, 201, 380000, 125400)},
		{"a thin baseline", overloadTestBucket(4800, 4224, 900, 810, 60, 54, 199, 66)},
		{"no baseline", overloadTestBucket(4800, 4224, 900, 810, 60, 54, 0, 0)},
	} {
		if settings.overloaded(c.bucket) {
			t.Errorf("%s is discounted", c.name)
		}
	}
	// the same bucket at the minimum is
	if !settings.overloaded(overloadTestBucket(400, 400, 200, 200, 1, 1, 200, 66)) {
		t.Error("a bucket at the minimums is not discounted")
	}
}

// The share must be above the line and at least the margin above the
// baseline. A lasting rise of the fleet's failure share, a stricter pass
// criterion say, becomes the baseline, and the same bucket is then scored.
func TestProviderEgressUrlOverloadIsJudgedAgainstItsBaseline(t *testing.T) {
	settings := DefaultProviderEgressUrlOverloadSettings()
	for _, c := range []struct {
		name       string
		bucket     *ProviderEgressUrlFleetBucket
		overloaded bool
	}{
		// 80% against a third
		{"far above a normal baseline", overloadTestBucket(1000, 800, 100, 80, 10, 8, 100000, 33000), true},
		// 80% against 65%: within the margin
		{"within the margin of a risen baseline", overloadTestBucket(1000, 800, 100, 80, 10, 8, 100000, 65000), false},
		// exactly at the line is not above it
		{"at the line", overloadTestBucket(1000, 600, 100, 60, 10, 6, 100000, 10000), false},
		// 59% against 10%: above the margin, not above the line
		{"under the line", overloadTestBucket(1000, 590, 100, 59, 10, 6, 100000, 10000), false},
		// 62.5% against 37.5%: the margin exactly, in shares a float holds
		// exactly
		{"at the margin", overloadTestBucket(1000, 625, 96, 60, 8, 5, 100000, 37500), true},
	} {
		if got := settings.overloaded(c.bucket); got != c.overloaded {
			t.Errorf("%s: discounted %t, want %t (share %.3f, baseline %.3f)", c.name, got, c.overloaded, c.bucket.ErrorShare(), c.bucket.BaselineErrorShare())
		}
	}
}

// Every call returns its own settings, and a grid that does not divide a day,
// or no retally, judges nothing.
func TestProviderEgressUrlOverloadSettings(t *testing.T) {
	a, b := DefaultProviderEgressUrlOverloadSettings(), DefaultProviderEgressUrlOverloadSettings()
	if a == b {
		t.Fatal("the defaults are shared")
	}
	a.MinOutcomeCount = 1
	if b.MinOutcomeCount == 1 {
		t.Fatal("changing one instance changed another")
	}
	if !b.judges() {
		t.Fatal("the defaults judge nothing")
	}
	if b.BucketDuration != 5*time.Minute || b.BaselineDuration != ProviderEgressHealthMaxAge || b.Retention != DefaultProviderEgressHealthHistorySettings().Retention {
		t.Errorf("defaults %+v", b)
	}
	for _, change := range []func(*ProviderEgressUrlOverloadSettings){
		func(settings *ProviderEgressUrlOverloadSettings) { settings.BucketDuration = 0 },
		func(settings *ProviderEgressUrlOverloadSettings) { settings.BucketDuration = 7 * time.Minute },
		func(settings *ProviderEgressUrlOverloadSettings) { settings.RetallyDuration = 0 },
	} {
		settings := DefaultProviderEgressUrlOverloadSettings()
		change(settings)
		if settings.judges() {
			t.Errorf("settings %+v judge", settings)
		}
	}
}

// Every URL window holds the discount, through the one row predicate its
// readers share, and the judge tallies the scored rows before it, so a
// discounted bucket is still tallied whole.
func TestProviderEgressWindowRowsHoldTheOverloadDiscount(t *testing.T) {
	for _, alias := range []string{"", "run", "success", "history"} {
		rows := providerEgressHealthWindowRowsSql(alias)
		if !strings.Contains(rows, providerEgressHealthScoredRowsSql(alias)) || !strings.Contains(rows, providerEgressUrlOverloadDiscountSql(alias)) {
			t.Errorf("the window's rows over %q are not the scored rows and the discount:\n%s", alias, rows)
		}
		if !strings.Contains(providerOnlineUrlEvidenceSql(alias), providerEgressUrlOverloadDiscountSql(alias)) {
			t.Errorf("the URL-success rule's evidence over %q is not discounted", alias)
		}
	}
	if !strings.Contains(providerEgressUrlOverloadDiscountSql("run"), "run.measured_at <@") || !strings.Contains(providerEgressUrlOverloadDiscountSql(""), "NOT COALESCE(measured_at <@") {
		t.Errorf("the discount does not test the rows' own measured time: %s", providerEgressUrlOverloadDiscountSql("run"))
	}
	if !strings.Contains(providerEgressHealthWindowSql(), providerEgressUrlOverloadDiscountSql("")) {
		t.Error("the URL window is not discounted")
	}
	tally := providerEgressUrlFleetBucketTallySql()
	if !strings.Contains(tally, providerEgressHealthScoredRowsSql("history")) {
		t.Error("the judge does not tally the window's scored rows")
	}
	if strings.Contains(tally, "provider_egress_url_fleet_bucket") {
		t.Error("the judge's tally reads the discount, so a discounted bucket would tally empty")
	}
}
