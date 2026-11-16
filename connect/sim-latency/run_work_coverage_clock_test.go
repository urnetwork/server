// Virtual owner clocks and complete synthetic scorer inputs test the runner's
// timing authority, not a live exploit, latency measurement or qualification.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/model"
	statspkg "github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/stats/sample"
	"google.golang.org/protobuf/proto"
)

// The timer and probe loop are the runner's real primitives. Only the product
// clock supplied to the remaining-time call is shifted, as editable util can be.
type coverageRunClockObservation struct {
	elapsed           time.Duration
	probeOffsets      []time.Duration
	productClockCalls int
	completed         time.Time
}

// This fixture must run inside synctest. It joins the real scheduled probe
// loop even on a fatal path; no process, service or physical workload starts.
func observeCoverageRunClock(t *testing.T, coverage *model.CompetitionWorkCoveragePolicy, duration time.Duration, shift time.Duration) coverageRunClockObservation {
	t.Helper()
	productCalls := 0
	productClock := func(shift time.Duration) func() time.Time {
		return func() time.Time {
			productCalls++
			return time.Now().Add(shift)
		}
	}
	measureStart := runClockNow(coverage, productClock(0))
	measureEnd := measureStart.Add(duration)
	remaining := measureEnd.Sub(runClockNow(coverage, productClock(shift)))
	if remaining <= 0 {
		t.Fatal("virtual owner did not have a positive timer budget")
	}
	offsets := matchmakingProbeOffsets(duration)
	if len(offsets) < 2 || offsets[0] != 0 {
		t.Fatal("virtual owner did not retain the real complete probe schedule")
	}
	actualOffsets := []time.Duration{time.Now().Sub(measureStart)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- runMatchmakingProbes(ctx, measureStart, offsets[1:], 1,
			func(ctx context.Context, target time.Time) error {
				return waitPhase(ctx, target.Sub(runClockNow(coverage, productClock(0))))
			}, func(ctx context.Context, index int) error {
				actualOffsets = append(actualOffsets, time.Now().Sub(measureStart))
				return nil
			})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("virtual owner fixture failed to join its canceled probe loop")
		}
	})
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	<-timer.C
	elapsed := time.Now().Sub(measureStart)
	cancel()
	if err := <-done; err != nil {
		t.Fatal("virtual owner lost its genuine completed probe schedule", err)
	}
	<-joined
	if len(actualOffsets) != len(offsets) {
		t.Fatal("clock counterexample did not retain all required probe calls", len(actualOffsets), len(offsets))
	}
	for index, offset := range offsets {
		if actualOffsets[index] != offset {
			t.Fatal("clock counterexample moved the unpoisoned actual probe schedule", index, actualOffsets[index], offset)
		}
	}
	completed := runClockNow(coverage, productClock(0))
	return coverageRunClockObservation{elapsed: elapsed, probeOffsets: actualOffsets, productClockCalls: productCalls, completed: completed}
}

// The full pristine scorer accepts the synthetic artifact chain while an
// editable remaining-clock can close its owner early. The fix must prevent
// that shortening at its source, not waive a gate or alter scorer tolerances.
func TestCoverageRunClockCannotShortenAcceptedMeasurement(t *testing.T) {
	assertCoverageRunClockCannotShortenAcceptedMeasurement(t)
}

// Both this supplemental root and the mandatory G7 deadline root execute the
// actual scorer/owner control; merely compiling the new file is insufficient.
func assertCoverageRunClockCannotShortenAcceptedMeasurement(t *testing.T) {
	t.Helper()
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	var observation coverageRunClockObservation
	synctest.Test(t, func(t *testing.T) {
		observation = observeCoverageRunClock(t, &coverage, 2*time.Second, 80*time.Millisecond)
	})
	fixture := completeWorkCoverageScoreFixture(t)
	stats := fixture.runStats
	writeCoverageClockProbeSamples(t, fixture, observation.probeOffsets)
	// Keep completion and sampler timestamps honest to the synthetic allowed
	// post-window drain. No invalid chronology or forged coverage is required.
	completedOffset := observation.elapsed
	for _, completion := range stats.WorkCoverage.Completions {
		completedOffset = max(completedOffset, time.Duration(completion.CompletedUnixNs-stats.WorkCoverage.MeasureStartUnixNs))
	}
	stats.CompletedUnixMs = stats.MeasureStartMs + (completedOffset + time.Millisecond).Milliseconds()
	var resource ResourceReport
	content, err := os.ReadFile(fixture.resource)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrictJSONBytes(content, &resource, "synthetic clock resource"); err != nil {
		t.Fatal(err)
	}
	resource.MeasurementStartMs, resource.MeasurementEndMs = stats.MeasureStartMs-1, stats.CompletedUnixMs+1
	writeScoreJSON(t, fixture.resource, resource)
	writeScoreJSON(t, scoreSidecarForCSV(fixture.run), stats)
	if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), stats); err != nil {
		t.Fatal(err)
	}
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable || len(result.Gates) != 7 || !result.Gates["G7_work_coverage"].Passed ||
		stats.WorkCoverage.CompletedPages != stats.WorkCoverage.ExpectedPages || stats.WorkCoverage.CompletedBytes != stats.WorkCoverage.ExpectedBytes {
		t.Fatal("short-owner counterexample did not retain a genuine complete seven-gate artifact fixture", result.EvalError, result.Gates)
	}
	t.Logf("synthetic full_score_placeable=true gates=7 coverage_pages=1 coverage_bytes=1 actual_owner=%s declared_window=2s", observation.elapsed)
	if observation.elapsed != 2*time.Second {
		t.Fatal("editable product clock shortened the owner despite a complete accepted seven-gate artifact", observation.elapsed)
	}
	if observation.productClockCalls != 0 {
		t.Fatal("future coverage owner still consulted a mutable product clock")
	}
}

// Real flat-sample serialization retains the observed virtual probe times.
// These bytes are visibly synthetic, not an API or live sampler observation.
func writeCoverageClockProbeSamples(t *testing.T, fixture *scoreFixture, offsets []time.Duration) {
	t.Helper()
	writer, err := statspkg.NewFlatWriter(fixture.samples)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range offsets {
		content, err := proto.Marshal(&sample.FindProviders2Sample{TimeUnixMilli: uint64(fixture.runStats.MeasureStartMs + offset.Milliseconds()),
			LoadMillis: 10, PoolCount: 100, Candidates: []*sample.Candidate{{ClientId: []byte("synthetic-clock-provider")}}})
		if err != nil {
			writer.Close()
			t.Fatal(err)
		}
		if err := writer.WriteFrame(content); err != nil {
			writer.Close()
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// A shorter owner can also erase the last offered tick. The permitted G7/G2
// floors and G4 sample span need not expose that unattempted lost population.
func TestCoverageRunClockCannotPruneAcceptedOfferedCohort(t *testing.T) {
	assertCoverageRunClockCannotPruneAcceptedOfferedCohort(t)
}

// This counterpart is also called by the already-required G7 deadline root.
// Expected work comes from the real seeded planner, never candidate counters.
func assertCoverageRunClockCannotPruneAcceptedOfferedCohort(t *testing.T) {
	t.Helper()
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	var observation coverageRunClockObservation
	synctest.Test(t, func(t *testing.T) {
		observation = observeCoverageRunClock(t, &coverage, 30*time.Second, 1500*time.Millisecond)
	})
	fixture := newScoreFixture(t, baselineScoreFixtureOptions())
	config := workCoverageConfigForTest()
	config.Site.MeanDepth, config.Site.Branching = 0, 1
	config.Site.MinBodyBytes, config.Site.MaxBodyBytes = throughputMinBytes+1, throughputMinBytes+1
	config.Clients.MeanPerMinute = 6000
	workload := filepath.Join(fixture.dir, "providers.yml")
	if err := SaveConfig(workload, config); err != nil {
		t.Fatal(err)
	}
	workloadBytes, err := os.ReadFile(workload)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildWorkCoveragePlan(config, 30*time.Second, 120*time.Second, defaultWorkCoverageLimits())
	if err != nil {
		t.Fatal(err)
	}
	start := time.UnixMilli(fixture.runStats.MeasureStartMs)
	full, err := newWorkCoverageCollector(plan, start)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := newWorkCoverageCollector(plan, start)
	if err != nil {
		t.Fatal(err)
	}
	var csv bytes.Buffer
	driver := &ClientDriver{out: bufio.NewWriter(&csv)}
	driver.writeCsvHeader()
	for rootIndex, root := range plan.Roots {
		if len(root.Pages) != 1 {
			t.Fatal("uniform single-page timing control grew a descendant population")
		}
		page := root.Pages[0]
		started := start.Add(time.Duration(root.OffsetNs) + time.Millisecond)
		completed := started.Add(100 * time.Millisecond)
		if !full.record(rootIndex, 0, rootIndex, started, completed) {
			t.Fatal("complete baseline clock control lost an offered root")
		}
		if observation.elapsed <= time.Duration(root.OffsetNs) {
			continue
		}
		row := driver.writeCsvRow(started, "synthetic-clock-client", page.Uri, page.Depth, 200, page.Bytes, 10*time.Millisecond, 100*time.Millisecond)
		if !retained.record(rootIndex, 0, row, started, completed) {
			t.Fatal("candidate clock control lost a genuinely completed admitted root")
		}
	}
	if err := driver.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.run, csv.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	stats := fixture.runStats
	stats.Schema, stats.ScorerVersion, stats.G2VolumePolicy = 3, model.CompetitionWorkCoverageScorerVersion, model.CompetitionG2VolumePolicyMinimumWork
	stats.WorkCoveragePolicy, stats.WorkCoverage = &coverage, retained.report()
	stats.ConfigSha256, stats.Seed = fmt.Sprintf("%x", sha256.Sum256(workloadBytes)), config.Seed
	stats.ClientsPool, stats.ClientsEstablished = config.Clients.PoolSize, config.Clients.PoolSize
	stats.Flags["duration"], stats.Flags["request_timeout"] = "30s", "2m0s"
	stats.RequestTimeoutMs, stats.MeasureEndMs = 120000, start.Add(30*time.Second).UnixMilli()
	stats.CompletedUnixMs = stats.MeasureStartMs + observation.elapsed.Milliseconds() + 1
	stats.ResultsCsvSha256, stats.ResultsCsvBytes = driver.csvIdentity()
	summarizeRows(driver.resultRows(), stats)
	writeScoreJSON(t, fixture.accounting, AccountingSnapshot{Schema: 1, Kind: accountingSnapshotKind, EvaluationId: stats.EvaluationId,
		Complete: true, MeasureStartMs: stats.MeasureStartMs, MeasureEndMs: stats.MeasureEndMs, ProviderEgressBytes: stats.WorkCoverage.CompletedBytes})
	writeScoreJSON(t, fixture.resource, ResourceReport{Schema: 1, Kind: resourceReportKind, EvaluationId: stats.EvaluationId,
		CgroupId: "synthetic-clock-cgroup", MeasurementStartMs: stats.MeasureStartMs - 1, MeasurementEndMs: stats.CompletedUnixMs + 1, Complete: true})
	writeScoreJSON(t, scoreSidecarForCSV(fixture.run), stats)
	if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), stats); err != nil {
		t.Fatal(err)
	}
	control := fixture.baselineData
	control.G2VolumePolicy, control.WorkCoveragePolicy, control.ScorerVersion = stats.G2VolumePolicy, &coverage, stats.ScorerVersion
	control.ConfigSha256, control.RequestTimeoutMs = stats.ConfigSha256, stats.RequestTimeoutMs
	control.RunFlags = stats.Flags
	control.Replicates[0].RequestCount, control.Replicates[0].ReceivedBytes = plan.TotalPages, plan.TotalBytes
	control.Replicates[0].WorkCoverage = &full.report().CompetitionWorkCoverageSummary
	writeScoreJSON(t, fixture.baseline, control)
	fixture.inputs.WorkloadPath = workload
	writeCoverageClockProbeSamples(t, fixture, observation.probeOffsets)
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable || len(result.Gates) != 7 || !result.Gates["G7_work_coverage"].Passed {
		t.Fatal("tail-pruning counterexample did not retain an accepted seven-gate fixture", result.EvalError, result.Gates)
	}
	t.Logf("synthetic full_score_placeable=true gates=7 actual_owner=%s declared_window=30s completed_pages=%d expected_pages=%d page_coverage=%f byte_coverage=%f", observation.elapsed,
		stats.WorkCoverage.CompletedPages, stats.WorkCoverage.ExpectedPages, stats.WorkCoverage.PageCoverage, stats.WorkCoverage.ByteCoverage)
	if observation.elapsed != 30*time.Second || stats.WorkCoverage.CompletedPages != stats.WorkCoverage.ExpectedPages ||
		stats.WorkCoverage.CompletedBytes != stats.WorkCoverage.ExpectedBytes {
		t.Fatal("editable product clock pruned offered work despite accepted success volume and coverage gates", observation.elapsed)
	}
}

// A backward product clock cannot extend the fixed measurement owner either.
func TestCoverageRunClockCannotExtendMeasurement(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	synctest.Test(t, func(t *testing.T) {
		observation := observeCoverageRunClock(t, &coverage, 2*time.Second, -80*time.Millisecond)
		if observation.elapsed != 2*time.Second || observation.productClockCalls != 0 {
			t.Fatal("editable product clock extended the coverage owner", observation.elapsed, observation.productClockCalls)
		}
	})
}

// Barrier and completion timestamps use the same protected standard clock;
// this also proves a product callback is not evaluated for its side effects.
func TestCoverageRunClockOwnsBarrierAndCompletion(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	synctest.Test(t, func(t *testing.T) {
		productCalls := 0
		poison := func() time.Time { productCalls++; return time.Now().Add(24 * time.Hour) }
		start := runClockNow(&coverage, poison)
		if start != time.Now() || productCalls != 0 {
			t.Fatal("product code chose the coverage barrier or was evaluated")
		}
		time.Sleep(123 * time.Nanosecond)
		complete := runClockNow(&coverage, poison)
		if complete != time.Now() || complete.Sub(start) != 123*time.Nanosecond || productCalls != 0 {
			t.Fatal("product code chose coverage completion or stripped its monotonic owner")
		}
	})
}

// Historical omission retains the exact product value, location, precision,
// callback count and remaining-time behavior; no epoch-one clock is replaced.
func TestCoverageRunClockPreservesLegacyProductClock(t *testing.T) {
	expected := time.Unix(1000, 123456000).UTC()
	calls := 0
	if actual := runClockNow(nil, func() time.Time { calls++; return expected }); actual != expected || calls != 1 {
		t.Fatal("nil coverage changed the historical product clock", actual, calls)
	}
	synctest.Test(t, func(t *testing.T) {
		observation := observeCoverageRunClock(t, nil, 2*time.Second, 80*time.Millisecond)
		if observation.elapsed != 1920*time.Millisecond || observation.productClockCalls == 0 {
			t.Fatal("prospective clock fix silently changed the legacy owner", observation.elapsed, observation.productClockCalls)
		}
	})
}

// Canceling the original owner still interrupts an outstanding real timer;
// a protected clock supplies no new lifetime, retry or replacement deadline.
func TestCoverageRunClockRetainsParentCancellation(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		joined := make(chan struct{})
		start := runClockNow(&coverage, time.Now)
		go func() {
			defer close(joined)
			done <- waitPhase(ctx, start.Add(2*time.Second).Sub(runClockNow(&coverage, time.Now)))
		}()
		t.Cleanup(func() { cancel(); <-joined })
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || time.Now() != start {
			t.Fatal("protected clock widened its canceled original owner", err)
		}
		<-joined
	})
}
