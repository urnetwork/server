package main

// G7 adds an independent completion floor without changing the raw latency
// objective or G1/G2. Trusted workload bytes regenerate the offered cohort.

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/urnetwork/server/model"
)

// A missing workload is never reconstructed from a candidate's counters or
// echoed seed. The evaluator must retain the exact authenticated providers.
func scoreWorkCoverage(stats *RunStats, rows []resultRow, workloadPath string, baseline *ScoreBaselineManifest) (*model.CompetitionWorkCoverageSummary, error) {
	if stats.WorkCoveragePolicy == nil {
		if stats.WorkCoverage != nil {
			return nil, scoreError("unexpected_work_coverage", "historical run has a prospective work ledger")
		}
		return nil, nil
	}
	if workloadPath == "" || stats.WorkCoverage == nil {
		return nil, scoreError("missing_work_inventory", "coverage scoring requires the authenticated workload and completion ledger")
	}
	content, err := readBoundedFile(workloadPath, "coverage workload")
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(content)) != stats.ConfigSha256 {
		return nil, scoreError("workload_mismatch", "coverage workload does not match the frozen providers digest")
	}
	config, err := decodeConfig(content)
	if err != nil || config.Seed != stats.Seed || stats.ClientsPool != config.Clients.PoolSize || stats.ClientsEstablished != config.Clients.PoolSize {
		return nil, scoreError("workload_mismatch", "coverage workload seed or representation is invalid")
	}
	duration, durationErr := time.ParseDuration(stats.Flags["duration"])
	timeout, timeoutErr := time.ParseDuration(stats.Flags["request_timeout"])
	if durationErr != nil || timeoutErr != nil || duration <= 0 || timeout <= 0 || timeout.Milliseconds() != stats.RequestTimeoutMs {
		return nil, scoreError("invalid_work_inventory", "coverage timing contract is incomplete")
	}
	plan, err := buildWorkCoveragePlan(config, duration, timeout, defaultWorkCoverageLimits())
	if err != nil {
		return nil, scoreError("invalid_work_inventory", "authenticated workload cannot be inventoried: %v", err)
	}
	return validateWorkCoverageReport(stats, rows, plan, baseline)
}

// Regenerates credit from exact CSV records and nanosecond deadline facts.
// In particular, missing descendants and never-launched roots stay missing.
func validateWorkCoverageReport(stats *RunStats, rows []resultRow, plan *workCoveragePlan, baseline *ScoreBaselineManifest) (*model.CompetitionWorkCoverageSummary, error) {
	report := stats.WorkCoverage
	if report == nil || report.Kind != plan.Kind || model.ValidateCompetitionWorkCoverageSummary(&report.CompetitionWorkCoverageSummary) != nil ||
		report.PlanSha256 != plan.Sha256 || report.MeasureStartUnixNs <= 0 || report.MeasureStartUnixNs/int64(time.Millisecond) != stats.MeasureStartMs ||
		(report.MeasureStartUnixNs+plan.DurationNs)/int64(time.Millisecond) != stats.MeasureEndMs ||
		int64(len(report.Completions)) > plan.TotalPages {
		return nil, scoreError("invalid_work_coverage", "completion ledger does not match the independently offered cohort")
	}
	collector, err := newWorkCoverageCollector(plan, time.Unix(0, report.MeasureStartUnixNs))
	if err != nil {
		return nil, scoreError("invalid_work_coverage", "completion ledger has an invalid barrier")
	}
	if baseline != nil && len(baseline.Replicates) != 0 {
		control := baseline.Replicates[0].WorkCoverage
		if control == nil || control.PlanSha256 != plan.Sha256 {
			return nil, scoreError("work_inventory_mismatch", "candidate offered cohort differs from the authenticated baseline")
		}
	}
	usedRows := map[int]bool{}
	for _, completion := range report.Completions {
		if completion.Root < 0 || len(plan.Roots) <= completion.Root || completion.Page < 0 || len(plan.Roots[completion.Root].Pages) <= completion.Page ||
			completion.Row < 0 || len(rows) <= completion.Row || usedRows[completion.Row] || completion.StartedUnixNs <= 0 || completion.CompletedUnixNs < completion.StartedUnixNs {
			return nil, scoreError("invalid_work_coverage", "completion ledger has a duplicate or unknown completion")
		}
		page, row := plan.Roots[completion.Root].Pages[completion.Page], rows[completion.Row]
		durationText := fmt.Sprintf("%.3f", float64(completion.CompletedUnixNs-completion.StartedUnixNs)/float64(time.Millisecond))
		millis, parseErr := strconv.ParseFloat(durationText, 64)
		if parseErr != nil || row.path != page.Uri || row.depth != page.Depth || row.status != 200 || row.bytes != page.Bytes ||
			row.tStartMs != completion.StartedUnixNs/int64(time.Millisecond) || row.totalMs != millis ||
			!collector.record(completion.Root, completion.Page, completion.Row, time.Unix(0, completion.StartedUnixNs), time.Unix(0, completion.CompletedUnixNs)) {
			return nil, scoreError("invalid_work_coverage", "completion does not bind a complete expected response before its original deadline")
		}
		usedRows[completion.Row] = true
	}
	want := collector.report().CompetitionWorkCoverageSummary
	if !reflect.DeepEqual(report.CompetitionWorkCoverageSummary, want) {
		return nil, scoreError("invalid_work_coverage", "completion counters differ from their independently recomputed ledger")
	}
	return &want, nil
}

// Coverage is a paired run-level statistic. We retain each run separately and
// compare its median, never pretending requests are independent R9 samples.
func medianWorkCoverage(values []*model.CompetitionWorkCoverageSummary) *model.CompetitionWorkCoverageSummary {
	if len(values) == 0 || len(values)%2 == 0 {
		return nil
	}
	pages, bytes := []int64{}, []int64{}
	var first *model.CompetitionWorkCoverageSummary
	for _, value := range values {
		if model.ValidateCompetitionWorkCoverageSummary(value) != nil {
			return nil
		}
		if first == nil {
			first = value
		}
		if value.PlanSha256 != first.PlanSha256 || value.OfferedRoots != first.OfferedRoots || value.ExpectedPages != first.ExpectedPages || value.ExpectedBytes != first.ExpectedBytes {
			return nil
		}
		pages = append(pages, value.CompletedPages)
		bytes = append(bytes, value.CompletedBytes)
	}
	slices.Sort(pages)
	slices.Sort(bytes)
	result := *first
	result.CompletedPages, result.CompletedBytes = pages[len(pages)/2], bytes[len(bytes)/2]
	result.PageCoverage = float64(result.CompletedPages) / float64(result.ExpectedPages)
	result.ByteCoverage = float64(result.CompletedBytes) / float64(result.ExpectedBytes)
	return &result
}

// Each inclusive floor is 95% of that dimension's baseline coverage. There
// is no upper-volume penalty; independent runtime qualification remains required.
func workCoverageGate(policy *model.CompetitionWorkCoveragePolicy, baseline, candidate *model.CompetitionWorkCoverageSummary) ScoreGate {
	gate := ScoreGate{Details: map[string]any{"policy": policy, "baseline": baseline, "candidate": candidate}}
	if policy == nil || model.ValidateCompetitionWorkCoveragePolicy(policy) != nil ||
		model.ValidateCompetitionWorkCoverageSummary(baseline) != nil || model.ValidateCompetitionWorkCoverageSummary(candidate) != nil ||
		baseline.PlanSha256 != candidate.PlanSha256 || baseline.OfferedRoots != candidate.OfferedRoots || baseline.ExpectedPages != candidate.ExpectedPages || baseline.ExpectedBytes != candidate.ExpectedBytes ||
		baseline.CompletedPages <= 0 || baseline.CompletedBytes <= 0 ||
		candidate.CompletedPages == 0 || candidate.CompletedBytes == 0 {
		return gate
	}
	gate.Details["baseline_page_coverage"] = baseline.PageCoverage
	gate.Details["candidate_page_coverage"] = candidate.PageCoverage
	gate.Details["page_min_inclusive"] = baseline.PageCoverage * policy.MinimumPageCoverageRatio
	gate.Details["baseline_byte_coverage"] = baseline.ByteCoverage
	gate.Details["candidate_byte_coverage"] = candidate.ByteCoverage
	gate.Details["byte_min_inclusive"] = baseline.ByteCoverage * policy.MinimumByteCoverageRatio
	gate.Passed = model.CompetitionWorkCoveragePassed(policy, baseline, candidate)
	return gate
}
