package main

// Coverage gate controls keep work omission distinct from dishonesty and from
// the historical attempted-request p95/success/volume population.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// Builds a complete authenticated synthetic artifact chain, including genuine
// workload serialization, generated expected paths, CSV identity and marker.
func completeWorkCoverageScoreFixture(t *testing.T) *scoreFixture {
	t.Helper()
	options := baselineScoreFixtureOptions()
	options.sampleEndOffsetMs = 1810
	fixture := newScoreFixture(t, options)
	config := workCoverageConfigForTest()
	config.Site.MeanDepth = 1000
	config.Site.MinBodyBytes, config.Site.MaxBodyBytes = throughputMinBytes+1, throughputMinBytes+1
	workload := filepath.Join(fixture.dir, "providers.yml")
	if err := SaveConfig(workload, config); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(workload)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildWorkCoveragePlan(config, 2*time.Second, 120*time.Second, defaultWorkCoverageLimits())
	if err != nil {
		t.Fatal(err)
	}
	start := time.UnixMilli(fixture.runStats.MeasureStartMs)
	collector, err := newWorkCoverageCollector(plan, start)
	if err != nil {
		t.Fatal(err)
	}
	var csv bytes.Buffer
	driver := &ClientDriver{out: bufio.NewWriter(&csv)}
	driver.writeCsvHeader()
	for rootIndex, root := range plan.Roots {
		for pageIndex, page := range root.Pages {
			started := start.Add(time.Duration(root.OffsetNs) + time.Millisecond + time.Duration(page.Depth)*100*time.Millisecond)
			completed := started.Add(100 * time.Millisecond)
			index := driver.writeCsvRow(started, "synthetic-client", page.Uri, page.Depth, 200, page.Bytes, 10*time.Millisecond, 100*time.Millisecond)
			if !collector.record(rootIndex, pageIndex, index, started, completed) {
				t.Fatal("synthetic expected completion was not credited")
			}
		}
	}
	if err := driver.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.run, csv.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	stats := fixture.runStats
	stats.Schema, stats.ScorerVersion, stats.G2VolumePolicy = 3, model.CompetitionWorkCoverageScorerVersion, model.CompetitionG2VolumePolicyMinimumWork
	stats.WorkCoveragePolicy, stats.WorkCoverage = &policy, collector.report()
	stats.ConfigSha256, stats.Seed = fmt.Sprintf("%x", sha256.Sum256(content)), config.Seed
	stats.ClientsPool, stats.ClientsEstablished = config.Clients.PoolSize, config.Clients.PoolSize
	stats.Flags["duration"] = "2s"
	stats.Flags["request_timeout"] = "2m0s"
	stats.RequestTimeoutMs = 120000
	stats.MeasureEndMs = start.Add(2 * time.Second).UnixMilli()
	stats.ResultsCsvSha256, stats.ResultsCsvBytes = driver.csvIdentity()
	summarizeRows(driver.resultRows(), stats)
	if stats.Rows <= stats.RowsInWindow {
		t.Fatal("synthetic coverage fixture did not exercise post-window crawl drain")
	}
	writeScoreJSON(t, fixture.accounting, AccountingSnapshot{Schema: 1, Kind: accountingSnapshotKind, EvaluationId: stats.EvaluationId, Complete: true, MeasureStartMs: stats.MeasureStartMs, MeasureEndMs: stats.MeasureEndMs, ProviderEgressBytes: plan.TotalBytes})
	writeScoreJSON(t, scoreSidecarForCSV(fixture.run), stats)
	if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), stats); err != nil {
		t.Fatal(err)
	}
	control := fixture.baselineData
	control.G2VolumePolicy, control.WorkCoveragePolicy, control.ScorerVersion = stats.G2VolumePolicy, &policy, stats.ScorerVersion
	control.ConfigSha256 = stats.ConfigSha256
	control.RequestTimeoutMs = stats.RequestTimeoutMs
	measured, err := scoreRows(driver.resultRows(), stats)
	if err != nil {
		t.Fatal(err)
	}
	control.Replicates[0].RequestCount, control.Replicates[0].ReceivedBytes = measured.RequestCount, measured.ReceivedBytes
	control.Replicates[0].WorkCoverage = &stats.WorkCoverage.CompetitionWorkCoverageSummary
	writeScoreJSON(t, fixture.baseline, control)
	fixture.inputs.WorkloadPath = workload
	return fixture
}

// A fully bound no-op reaches all seven gates, and the same artifact chain
// produces a fresh prospective baseline rather than relabeling a v2 control.
func TestWorkCoverageScoreAndFreshBaselineAuthenticateFullArtifactChain(t *testing.T) {
	fixture := completeWorkCoverageScoreFixture(t)
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable || len(result.Gates) != 7 || result.Diagnostics.WorkCoveragePolicy == nil {
		t.Fatal("complete no-op failed the seven-gate contract", result.EvalError, result.Gates)
	}
	if _, err := marshalScoreResult(result); err != nil {
		t.Fatal("shared score consumer rejected the new instrument contract", err)
	}
	inputs := baselineInputsFromFixture(fixture)
	inputs.G2VolumePolicy, inputs.WorkCoveragePolicy, inputs.WorkloadPath = model.CompetitionG2VolumePolicyMinimumWork, fixture.runStats.WorkCoveragePolicy, fixture.inputs.WorkloadPath
	baseline, err := BuildScoreBaseline(inputs)
	if err != nil || baseline.ScorerVersion != model.CompetitionWorkCoverageScorerVersion || baseline.WorkCoveragePolicy == nil || len(baseline.Replicates) != 1 || baseline.Replicates[0].WorkCoverage == nil {
		t.Fatal("fresh coverage baseline did not bind its independent expected work", err)
	}
}

// Missing workload, policy, ledger or marker provenance fails closed before a
// placeable score can leave the pristine scorer.
func TestWorkCoverageScoreRejectsMissingAndMixedProvenance(t *testing.T) {
	for _, mutate := range []func(*scoreFixture){
		func(fixture *scoreFixture) { fixture.inputs.WorkloadPath = "" },
		func(fixture *scoreFixture) { fixture.inputs.WorkloadPath = fixture.stderr },
		func(fixture *scoreFixture) { fixture.runStats.WorkCoverage = nil },
		func(fixture *scoreFixture) { fixture.runStats.WorkCoveragePolicy = nil },
		func(fixture *scoreFixture) { fixture.runStats.Schema = 2 },
		func(fixture *scoreFixture) {
			fixture.runStats.ScorerVersion = model.CompetitionMinimumWorkScorerVersion
		},
		func(fixture *scoreFixture) { fixture.baselineData.WorkCoveragePolicy = nil },
		func(fixture *scoreFixture) { fixture.baselineData.Replicates[0].WorkCoverage = nil },
	} {
		fixture := completeWorkCoverageScoreFixture(t)
		mutate(fixture)
		writeScoreJSON(t, scoreSidecarForCSV(fixture.run), fixture.runStats)
		writeScoreJSON(t, fixture.baseline, fixture.baselineData)
		if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
			t.Fatal(err)
		}
		if result := Score(fixture.inputs); result.EvalError == nil || result.Placeable {
			t.Fatal("mixed coverage provenance produced a placeable result")
		}
	}
}

// Uniform bytes make each dimension independently adjustable in gate tests.
func workCoverageSummaryForTest(completedPages, completedBytes int64) *model.CompetitionWorkCoverageSummary {
	return &model.CompetitionWorkCoverageSummary{PlanSha256: strings.Repeat("a", 64), OfferedRoots: 100, ExpectedPages: 10000, ExpectedBytes: 1000000, CompletedPages: completedPages, CompletedBytes: completedBytes, PageCoverage: float64(completedPages) / 10000, ByteCoverage: float64(completedBytes) / 1000000}
}

// A failed parent can erase a large post-window subtree while retaining the
// same in-window attempts and bytes. Those old counters do not certify work.
// The fixtures deliberately lose much more than either proposed tolerance.
func TestWorkCoverageRejectsPruningThatPassesOldSuccessAndVolume(t *testing.T) {
	base := baselineAggregate{ScoreAggregateDiagnostics: ScoreAggregateDiagnostics{RawScore: 100, SuccessRate: 1, RequestCount: 1000, ReceivedBytes: 100000, FindProvidersLoadP95Ms: 1, FindProvidersPoolP05: 100, FindProvidersSampleSpanFraction: 1}}
	pruned := base.ScoreAggregateDiagnostics
	pruned.RawScore, pruned.SuccessRate = 50, .995
	old := buildScoreGates(base, pruned, nil, model.CompetitionG2VolumePolicyMinimumWork)
	if !old["G1_success"].Passed || !old["G2_volume"].Passed {
		t.Fatal("adversarial control did not pass the historical success/volume gates", old)
	}
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	control := workCoverageSummaryForTest(10000, 1000000)
	if workCoverageGate(&policy, control, workCoverageSummaryForTest(5000, 500000)).Passed {
		t.Fatal("erased expected descendants passed independent coverage")
	}
	honest := pruned
	honest.SuccessRate = 1
	old = buildScoreGates(base, honest, nil, model.CompetitionG2VolumePolicyMinimumWork)
	if !old["G1_success"].Passed || !old["G2_volume"].Passed || !workCoverageGate(&policy, control, control).Passed {
		t.Fatal("honest faster complete work was rejected")
	}
	if pruned.RawScore != 50 || honest.RawScore != 50 {
		t.Fatal("coverage changed the raw latency objective")
	}
}

// Page and byte coverage are independent inclusive floors. Completing more
// work is never an upper-bound failure; missing one dimension cannot hide in
// a surplus in the other dimension.
func TestWorkCoverageGateInclusiveBoundariesAndDegenerateControls(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	baseline := workCoverageSummaryForTest(8000, 800000)
	for percentage := int64(2); percentage <= 100; percentage++ {
		control := workCoverageSummaryForTest(percentage*100, percentage*10000)
		candidate := workCoverageSummaryForTest(percentage*95, percentage*9500)
		if !workCoverageGate(&policy, control, candidate).Passed {
			t.Fatal("exact decimal inclusive boundary was rounded into failure", percentage)
		}
	}
	for _, test := range []struct {
		pages, bytes int64
		pass         bool
	}{
		{pages: 7600, bytes: 760000, pass: true},
		{pages: 7599, bytes: 760000, pass: false},
		{pages: 7600, bytes: 759999, pass: false},
		{pages: 7500, bytes: 750000, pass: false},
		{pages: 10000, bytes: 1000000, pass: true},
		{pages: 10000, bytes: 759999, pass: false},
		{pages: 7599, bytes: 1000000, pass: false},
		{pages: 0, bytes: 0, pass: false},
	} {
		if got := workCoverageGate(&policy, baseline, workCoverageSummaryForTest(test.pages, test.bytes)).Passed; got != test.pass {
			t.Fatal("independent inclusive coverage boundary changed", test, got)
		}
	}
	for _, control := range []*model.CompetitionWorkCoverageSummary{nil, workCoverageSummaryForTest(0, 0), workCoverageSummaryForTest(0, 10000), workCoverageSummaryForTest(100, 0)} {
		if workCoverageGate(&policy, control, baseline).Passed {
			t.Fatal("missing or unresolved baseline authorized empty useful work")
		}
	}
	if !workCoverageGate(&policy, workCoverageSummaryForTest(100, 10000), baseline).Passed {
		t.Fatal("positive low baseline coverage acquired an unapproved absolute floor")
	}
	for _, mutate := range []func(*model.CompetitionWorkCoverageSummary){
		func(value *model.CompetitionWorkCoverageSummary) { value.PageCoverage = math.NaN() },
		func(value *model.CompetitionWorkCoverageSummary) { value.ByteCoverage = math.Inf(1) },
		func(value *model.CompetitionWorkCoverageSummary) { value.PlanSha256 = strings.Repeat("b", 64) },
		func(value *model.CompetitionWorkCoverageSummary) { value.ExpectedPages++ },
		func(value *model.CompetitionWorkCoverageSummary) { value.OfferedRoots = 0 },
	} {
		candidate := *baseline
		mutate(&candidate)
		if workCoverageGate(&policy, baseline, &candidate).Passed {
			t.Fatal("invalid candidate inventory passed")
		}
	}
}

// Between-run medians retain all R9 diagnostics; request counts are not
// interpreted as independent evidence of statistical equivalence.
func TestWorkCoverageAggregatesIndependentRunMedians(t *testing.T) {
	values := []*model.CompetitionWorkCoverageSummary{}
	for _, completed := range []int64{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000} {
		values = append(values, workCoverageSummaryForTest(completed, (10000-completed)*100))
	}
	median := medianWorkCoverage(values)
	if median == nil || median.CompletedPages != 5000 || median.CompletedBytes != 500000 || median.PageCoverage != .5 || median.ByteCoverage != .5 {
		t.Fatal("coverage did not retain independent run medians", median)
	}
	if medianWorkCoverage(values[:8]) != nil || medianWorkCoverage(append(values, nil, nil)) != nil {
		t.Fatal("partial or invalid coverage population produced an aggregate")
	}
}

// Exact CSV linkage and original deadlines reject duplicate, partial, forged
// and cross-root claims even when their summary counters look plausible.
func TestWorkCoverageLedgerRecomputedAgainstExpectedWorkAndCsv(t *testing.T) {
	plan, err := buildWorkCoveragePlan(workCoverageConfigForTest(), 6*time.Second, 120*time.Second, defaultWorkCoverageLimits())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1000, 737)
	collector, err := newWorkCoverageCollector(plan, start)
	if err != nil {
		t.Fatal(err)
	}
	admission := start.Add(time.Duration(plan.Roots[0].OffsetNs))
	started, completed := admission.Add(time.Millisecond), admission.Add(3*time.Millisecond)
	if !collector.record(0, 0, 0, started, completed) {
		t.Fatal("positive ledger fixture did not complete")
	}
	page := plan.Roots[0].Pages[0]
	rows := []resultRow{{tStartMs: started.UnixMilli(), client: "synthetic", path: page.Uri, depth: page.Depth, status: 200, bytes: page.Bytes, totalMs: 2}}
	stats := &RunStats{MeasureStartMs: start.UnixMilli(), MeasureEndMs: start.Add(6 * time.Second).UnixMilli(), WorkCoverage: collector.report()}
	if _, err := validateWorkCoverageReport(stats, rows, plan, nil); err != nil {
		t.Fatal("valid independent completion was rejected", err)
	}
	for _, mutate := range []func(*RunStats, []resultRow){
		func(stats *RunStats, rows []resultRow) { rows[0].path = "/p/0/forged" },
		func(stats *RunStats, rows []resultRow) { rows[0].bytes-- },
		func(stats *RunStats, rows []resultRow) { rows[0].depth++ },
		func(stats *RunStats, rows []resultRow) { rows[0].status = 0 },
		func(stats *RunStats, rows []resultRow) {
			stats.WorkCoverage.Completions = append(stats.WorkCoverage.Completions, stats.WorkCoverage.Completions[0])
		},
		func(stats *RunStats, rows []resultRow) { stats.WorkCoverage.Completions[0].Root = len(plan.Roots) },
		func(stats *RunStats, rows []resultRow) {
			stats.WorkCoverage.Completions[0].StartedUnixNs = admission.UnixNano() - 1
		},
		func(stats *RunStats, rows []resultRow) {
			stats.WorkCoverage.Completions[0].CompletedUnixNs = admission.Add(120 * time.Second).UnixNano()
		},
		func(stats *RunStats, rows []resultRow) { stats.WorkCoverage.ExpectedPages-- },
		func(stats *RunStats, rows []resultRow) { stats.WorkCoverage.ExpectedBytes-- },
		func(stats *RunStats, rows []resultRow) { stats.WorkCoverage.CompletedPages++ },
		func(stats *RunStats, rows []resultRow) { stats.WorkCoverage.CompletedBytes++ },
		func(stats *RunStats, rows []resultRow) { stats.WorkCoverage.PlanSha256 = strings.Repeat("b", 64) },
		func(stats *RunStats, rows []resultRow) {
			stats.WorkCoverage.MeasureStartUnixNs += int64(time.Millisecond)
		},
	} {
		content, _ := json.Marshal(stats)
		var changed RunStats
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		changedRows := append([]resultRow{}, rows...)
		mutate(&changed, changedRows)
		if _, err := validateWorkCoverageReport(&changed, changedRows, plan, nil); err == nil {
			t.Fatal("corrupted completion earned work credit")
		}
	}
}
