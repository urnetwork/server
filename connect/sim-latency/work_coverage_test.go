package main

// Pure cohort/response controls exercise original deadlines, drain credit and
// canonical response completeness without a provider or network listener.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Produces exact expected bytes without reusing the response validator.
func workCoverageBodyForTest(page sitePage) []byte {
	header, _ := json.Marshal(page)
	body := append(header, '\n')
	for offset := 0; offset < page.Size; offset++ {
		body = append(body, byte('a'+(offset%(32<<10))%26))
	}
	return body
}

// Expected bytes include the JSON line, not just the advertised padding.
func TestWorkCoverageResponseRejectsPartialForgedAndAmbiguousBodies(t *testing.T) {
	page := sitePage{Urls: []string{"/p/0/synthetic"}, Size: 32779}
	body := workCoverageBodyForTest(page)
	expected := workCoveragePage{Uri: "/?crawl=0", Page: page, Bytes: int64(len(body))}
	for _, bodyBytes := range [][]byte{body, body[:len(body)-1], append(append([]byte{}, body...), 'x'), bytes.Replace(body, []byte("synthetic"), []byte("different"), 1), bytes.Replace(body, []byte(`"size":32779`), []byte(`"size":32779,"size":32779`), 1), bytes.Replace(body, []byte(`"urls"`), []byte(`"Urls"`), 1), bytes.Replace(body, []byte("abc"), []byte("xyz"), 1)} {
		response := &http.Response{StatusCode: 200, ContentLength: int64(len(bodyBytes)), Body: io.NopCloser(bytes.NewReader(bodyBytes))}
		read, err := readExpectedSiteResponse(response, expected)
		want := bytes.Equal(bodyBytes, body)
		if err != nil || read.complete != want || read.receivedBytes > expected.Bytes+1 {
			t.Fatal("unexpected complete/partial representation classification", want, read, err)
		}
	}
	for _, status := range []int{0, 302, 404, 503} {
		read, err := readExpectedSiteResponse(&http.Response{StatusCode: status, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body))}, expected)
		if err != nil || read.complete {
			t.Fatal("non-200 response received completion credit", status, err)
		}
	}
}

// A late completion cannot extend a root clock; post-window descendants may
// still finish within that original clock. Missing roots never disappear.
func TestWorkCoverageCollectorKeepsUnlaunchedDescendantsAndDrainCredit(t *testing.T) {
	plan, err := buildWorkCoveragePlan(workCoverageConfigForTest(), 6*time.Second, 120*time.Second, defaultWorkCoverageLimits())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1000, 731)
	collector, err := newWorkCoverageCollector(plan, start)
	if err != nil {
		t.Fatal(err)
	}
	before := collector.report()
	if before.OfferedRoots != int64(len(plan.Roots)) || before.CompletedPages != 0 || before.ExpectedPages != plan.TotalPages {
		t.Fatal("unlaunched offered roots or descendants were erased")
	}
	root := 0
	for len(plan.Roots[root].Pages) < 2 && root+1 < len(plan.Roots) {
		root++
	}
	if len(plan.Roots[root].Pages) < 2 {
		t.Fatal("synthetic fixture has no expected descendant")
	}
	admission := start.Add(time.Duration(plan.Roots[root].OffsetNs))
	deadline := admission.Add(time.Duration(plan.TimeoutNs))
	if collector.record(root, 0, 0, admission.Add(-time.Nanosecond), admission) || collector.record(root, 0, 0, admission, deadline) {
		t.Fatal("pre-admission or at-deadline response earned credit")
	}
	if !collector.record(root, 0, 0, admission, admission.Add(time.Millisecond)) || collector.record(root, 0, 1, admission, admission.Add(time.Second)) {
		t.Fatal("exact-start acceptance or duplicate suppression failed")
	}
	if !collector.record(root, 1, 1, start.Add(7*time.Second), start.Add(8*time.Second)) {
		t.Fatal("post-window drain within the original root deadline lost credit")
	}
	if _, _, ok := collector.page(0, "/p/0/forged", 1); ok {
		t.Fatal("received foreign URL created expected work")
	}
	report := collector.report()
	if report.CompletedPages != 2 || report.ExpectedPages != before.ExpectedPages || report.ExpectedBytes != before.ExpectedBytes || report.OfferedRoots != before.OfferedRoots {
		t.Fatal("response success changed offered work instead of completion credit", report)
	}
}

// Actual driver context creation is tested under virtual time. The delayed
// driver receives only the remaining original budget, not a new full timeout.
func TestWorkCoverageLateDriverDoesNotResetPlannedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		page := sitePage{Urls: []string{}, Size: 3}
		body := workCoverageBodyForTest(page)
		plan := &workCoveragePlan{Kind: "seeded-crawl-coverage/v1", DurationNs: int64(3 * time.Second), TimeoutNs: int64(2 * time.Second), Sha256: strings.Repeat("a", 64), TotalPages: 1, TotalBytes: int64(len(body)), Roots: []workCoverageRoot{{OffsetNs: 0, Pages: []workCoveragePage{{Uri: "/?crawl=0", Page: page, Bytes: int64(len(body))}}}}}
		collector, err := newWorkCoverageCollector(plan, start)
		if err != nil {
			t.Fatal(err)
		}
		var deadline time.Time
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			deadline, _ = request.Context().Deadline()
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		driver := &ClientDriver{ctx: context.Background(), config: workCoverageConfigForTest(), siteAddr: "synthetic.invalid", out: bufio.NewWriter(io.Discard), arrivalsStop: make(chan struct{}), workCoverage: collector, clients: []*pooledClient{{label: "synthetic-client", httpClient: &http.Client{Transport: transport}}}}
		time.Sleep(time.Second)
		done := make(chan error, 1)
		go func() { done <- driver.Run() }()
		time.Sleep(3 * time.Second)
		driver.StopArrivals()
		if err := <-done; err != nil || !deadline.Equal(start.Add(2*time.Second)) || collector.report().CompletedPages != 0 {
			t.Fatal("late driver extended the original crawl budget", deadline, err)
		}
	})
	// The existing mandatory G7 root also executes both protected-clock
	// regressions. Newly compiled supplemental files alone are not proof.
	assertCoverageRunClockCannotShortenAcceptedMeasurement(t)
	assertCoverageRunClockCannotPruneAcceptedOfferedCohort(t)
}
