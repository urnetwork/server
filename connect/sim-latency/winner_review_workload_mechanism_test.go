// Deterministic synthetic controls pin the scoring blind spot and the real
// discovery boundary. They do not change the frozen instrument or its gates.
package main

import (
	"bufio"
	"io"
	"net/http"
	"testing"

	"github.com/urnetwork/server/model"
)

// G1/G2 alone can accept reduced useful work: failed parents are attempted rows,
// while unattempted descendants are absent from both latency and success counts.
func TestWinnerWorkloadReviewSuppressedRowsCanPassLegacyGates(t *testing.T) {
	window := &RunStats{MeasureStartMs: 0, MeasureEndMs: 2000, RequestTimeoutMs: 120000}
	baselineRows, candidateRows := []resultRow{}, []resultRow{}
	for index := range 1000 {
		baselineRows = append(baselineRows, resultRow{tStartMs: int64(index), status: 200, bytes: 1000, totalMs: 1000})
		if index < 960 {
			row := resultRow{tStartMs: int64(index), status: 200, bytes: 1000, totalMs: 100}
			if index < 8 {
				row.status, row.bytes, row.totalMs = 0, 100, 1
			}
			candidateRows = append(candidateRows, row)
		}
	}
	baseline, err := scoreRows(baselineRows, window)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := scoreRows(candidateRows, window)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.RequestCount != 960 || candidate.Failures != 8 || candidate.ReceivedBytes != 952800 || candidate.RawScore != 100 || candidate.SuccessRate != float64(952)/960 {
		t.Fatal("synthetic request/partial-byte/penalty accounting changed", candidate)
	}
	base := baselineAggregate{ScoreAggregateDiagnostics: ScoreAggregateDiagnostics{RawScore: baseline.RawScore, SuccessRate: baseline.SuccessRate,
		RequestCount: float64(baseline.RequestCount), ReceivedBytes: float64(baseline.ReceivedBytes), FindProvidersLoadP95Ms: 1, FindProvidersPoolP05: 1}}
	observed := ScoreAggregateDiagnostics{RawScore: candidate.RawScore, SuccessRate: candidate.SuccessRate, RequestCount: float64(candidate.RequestCount),
		ReceivedBytes: float64(candidate.ReceivedBytes), AccountingCoverage: 1, FindProvidersLoadP95Ms: 1, FindProvidersPoolP05: 1, FindProvidersSampleSpanFraction: 1}
	replicates := []candidateReplicate{{stabilityOK: true, resourcesOK: true, diagnostics: ScoreReplicateDiagnostics{FindProvidersSamples: 1, FindProvidersSampleSpanFraction: 1}}}
	for _, policy := range []model.CompetitionG2VolumePolicy{model.CompetitionG2VolumePolicyLegacy, model.CompetitionG2VolumePolicyMinimumWork} {
		gates := buildScoreGates(base, observed, replicates, policy)
		if !allScoreGatesPass(gates) {
			t.Fatal("fixture no longer demonstrates numerical gates admitting omitted descendants", policy, gates)
		}
	}
}

// The direct penalty is monotone: merely relabeling fixed-population tail rows
// as failures does not create a latency gain. Population/contention is the gap.
func TestWinnerWorkloadReviewEarlyFailureDoesNotLowerFixedPopulationP95(t *testing.T) {
	window := &RunStats{MeasureStartMs: 0, MeasureEndMs: 2000, RequestTimeoutMs: 120000}
	rows := make([]resultRow, 1000)
	for index := range rows {
		rows[index] = resultRow{tStartMs: int64(index), status: 200, bytes: 1000, totalMs: 100}
		if index >= 950 {
			rows[index].totalMs = 2000
		}
	}
	baseline, err := scoreRows(rows, window)
	if err != nil {
		t.Fatal(err)
	}
	for index := 992; index < len(rows); index++ {
		rows[index].status, rows[index].totalMs = 0, 1
	}
	candidate, err := scoreRows(rows, window)
	if err != nil || candidate.RawScore != baseline.RawScore || candidate.Failures != 8 {
		t.Fatal("early failure was incorrectly credited as a faster observation", baseline, candidate, err)
	}
}

// Exercise the actual fetch-to-worker discovery handoff. Even a fully parsed
// child list is withheld until the parent's complete body has arrived.
func TestWinnerWorkloadReviewIncompleteParentSuppressesChildAttempt(t *testing.T) {
	for _, partial := range []bool{false, true} {
		requests := []string{}
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests = append(requests, request.URL.Path)
			body := "{\"urls\":[],\"size\":0}\n"
			if request.URL.Path == "/" {
				body = "{\"urls\":[\"/synthetic-child\"],\"size\":4}\nabcd"
				if partial {
					body = body[:len(body)-2]
				}
			}
			return testSiteResponse(http.StatusOK, body, -1), nil
		})}
		driver := &ClientDriver{config: &Config{Clients: ClientsConfig{ConnectionsPerCrawl: 1}}, siteAddr: "crawl.example", out: bufio.NewWriter(io.Discard)}
		driver.crawl(t.Context(), "synthetic-client", client, 1)
		rows := driver.resultRows()
		if partial {
			if len(requests) != 1 || len(rows) != 1 || rows[0].success() || rows[0].bytes == 0 {
				t.Fatal("incomplete parent did not remain one partial failed attempt", requests, rows)
			}
			metrics, err := scoreRows(rows, &RunStats{MeasureStartMs: rows[0].tStartMs, MeasureEndMs: rows[0].tStartMs + 1, RequestTimeoutMs: 120000})
			if err != nil || metrics.RequestCount != 1 || metrics.Failures != 1 || metrics.RawScore != 120000 {
				t.Fatal("unattempted child gained an invented observation", metrics, err)
			}
		} else if len(requests) != 2 || requests[1] != "/synthetic-child" || len(rows) != 2 || !rows[0].success() || !rows[1].success() {
			t.Fatal("healthy parent control did not discover and complete its child", requests, rows)
		}
	}
}
