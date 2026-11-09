package monitor

// SIGNALS.md §29.5 on fixture Mimir answers: P0 past the refill pages, and P0's warn line, the bucket empty or
// low, shedding and HDD refusals past 0.1% of a day's uploads warn; a loop that publishes nothing is blind.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// meta-1's loop within every line, and seg-1's uploads.
func writeBudgetTestSamples() []*sidecarStorageSample {
	depth := DefaultSidecarStorageSettings().BucketDepthByteCount
	return []*sidecarStorageSample{
		sidecarStorageTestSample(writeBudgetCheckRatio, 0.2, "cluster", "meta-1"),
		sidecarStorageTestSample(writeBudgetCheckRatioMin, 0.1, "cluster", "meta-1"),
		sidecarStorageTestSample(writeBudgetCheckBucket, 0.8*depth, "cluster", "meta-1"),
		sidecarStorageTestSample(writeBudgetCheckBucketMax, 0.9*depth, "cluster", "meta-1"),
		sidecarStorageTestSample(writeBudgetCheckShed, 0, "cluster", "meta-1"),
		sidecarStorageTestSample(writeBudgetCheckHddRefused, 0, "cluster", "seg-1"),
		sidecarStorageTestSample(writeBudgetCheckUploads, 100000),
	}
}

func evaluateWriteBudget(samples []*sidecarStorageSample) []finding {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	return writeBudgetProbe{}.evaluate(settings, []string{"meta-1"}, []string{"seg-1"}, samples)
}

// Sets check's sample to value.
func setWriteBudgetTestValue(samples []*sidecarStorageSample, check string, value float64) {
	for _, sample := range samples {
		if sample.check == check {
			sample.value = value
		}
	}
}

func TestWriteBudgetWithinEveryLineRaisesNothing(t *testing.T) {
	requireSidecarStorageRaised(t, evaluateWriteBudget(writeBudgetTestSamples()))
}

// Above the refill pages; at it does not. P0 is never paced or refused, so the page asks for capacity.
func TestWriteBudgetP0PastTheRefillPages(t *testing.T) {
	samples := writeBudgetTestSamples()
	setWriteBudgetTestValue(samples, writeBudgetCheckRatio, 1)
	requireSidecarStorageRaised(t, evaluateWriteBudget(samples))
	setWriteBudgetTestValue(samples, writeBudgetCheckRatio, 1.01)
	findings := evaluateWriteBudget(samples)
	requireSidecarStorageRaised(t, findings, "write-budget-p0-past-refill meta-1")
	for _, finding := range findings {
		if finding.class == writeBudgetClassP0PastRefill && !finding.healthy && (finding.tier != tierPage || !strings.Contains(finding.action, "never pace or refuse P0")) {
			t.Errorf("finding %+v", finding)
		}
	}
}

// Each warn at its line: P0 an hour past half the refill, the bucket at zero for 5 minutes and under 25% of
// its depth, any shed P1, and HDD refusals past 0.1% of the day's uploads.
func TestWriteBudgetWarnLines(t *testing.T) {
	depth := DefaultSidecarStorageSettings().BucketDepthByteCount
	cases := []struct {
		check string
		value float64
		want  []string
	}{
		{check: writeBudgetCheckRatioMin, value: 0.5},
		{check: writeBudgetCheckRatioMin, value: 0.51, want: []string{"write-budget-p0-warn-line meta-1"}},
		{check: writeBudgetCheckBucketMax, value: 0, want: []string{"write-budget-bucket-empty meta-1"}},
		{check: writeBudgetCheckBucket, value: 0.25 * depth},
		{check: writeBudgetCheckBucket, value: 0.24 * depth, want: []string{"write-budget-bucket-low meta-1"}},
		{check: writeBudgetCheckShed, value: 3, want: []string{"write-budget-shedding meta-1"}},
		{check: writeBudgetCheckHddRefused, value: 100},
		{check: writeBudgetCheckHddRefused, value: 101, want: []string{"write-budget-hdd-refusals seg-1"}},
	}
	for _, c := range cases {
		samples := writeBudgetTestSamples()
		setWriteBudgetTestValue(samples, c.check, c.value)
		requireSidecarStorageRaised(t, evaluateWriteBudget(samples), c.want...)
	}
	// refusals with no upload counted
	samples := writeBudgetTestSamples()
	setWriteBudgetTestValue(samples, writeBudgetCheckUploads, 0)
	setWriteBudgetTestValue(samples, writeBudgetCheckHddRefused, 1)
	requireSidecarStorageRaised(t, evaluateWriteBudget(samples), "write-budget-hdd-refusals seg-1")
}

// A metadata cluster whose loop publishes no ratio or bucket, or no Mimir: blind.
func TestWriteBudgetMissingLoopIsBlind(t *testing.T) {
	samples := writeBudgetTestSamples()
	requireSidecarStorageRaised(t, evaluateWriteBudget(samples[1:]), "write-budget-unobservable meta-1")
	requireSidecarStorageRaised(t, evaluateWriteBudget(append(samples[:2:2], samples[3:]...)), "write-budget-unobservable meta-1")

	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	probe := writeBudgetProbe{query: fixedSidecarStorageQuery(nil, errors.New("no services host"))}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "write-budget-unobservable mimir")

	probe = writeBudgetProbe{query: fixedSidecarStorageQuery(writeBudgetTestSamples(), nil)}
	findings, err = probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings)
}

// The query sums each counter per cluster and names the upload route.
func TestWriteBudgetQueryNamesItsSeries(t *testing.T) {
	query := writeBudgetQuery("synthetic", DefaultSidecarStorageSettings())
	for _, part := range []string{
		`max by (cluster) (urnetwork_evs_budget_p0_refill_ratio{env="synthetic"}`,
		`min_over_time(urnetwork_evs_budget_p0_refill_ratio{env="synthetic"}[3600s])`,
		`max_over_time(urnetwork_evs_budget_bucket_bytes{env="synthetic"}[300s])`,
		`increase(urnetwork_evs_budget_shed_total{env="synthetic"}[300s])`,
		`increase(urnetwork_evs_hdd_admission_refused_total{env="synthetic"}[86400s])`,
		`urnetwork_evs_requests_total{env="synthetic",route="PUT /evs/v1/repos/{repo_id}/segments/{segment_id}"}`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s: %s", part, query)
		}
	}
}
