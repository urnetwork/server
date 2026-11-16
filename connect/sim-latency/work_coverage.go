package main

// Completion credit is a trusted, deduplicated ledger over the independent
// offered plan. Neither parent success nor a received URL creates its work.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/model"
)

// Explicit capability selection cannot silently turn historical omission
// into a prospective workload. The full policy is frozen in authenticated data.
func coveragePolicyOption(options docopt.Opts) *model.CompetitionWorkCoveragePolicy {
	kind := optString(options, "--work-coverage-policy", "")
	if kind == "" {
		return nil
	}
	if kind != model.CompetitionWorkCoverageKind {
		fatalf("unsupported work coverage policy")
	}
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	return &policy
}

// Row is the exact zero-based record in the authenticated CSV, including
// failures and post-window records. Nanoseconds retain deadline precision.
type workCoverageCompletion struct {
	Root            int   `json:"root"`
	Page            int   `json:"page"`
	Row             int   `json:"row"`
	StartedUnixNs   int64 `json:"started_unix_ns"`
	CompletedUnixNs int64 `json:"completed_unix_ns"`
}

// This report is authenticated by run.json and its final marker. A pristine
// scorer regenerates every expected descriptor from the retained workload.
type workCoverageReport struct {
	Kind               string `json:"kind"`
	MeasureStartUnixNs int64  `json:"measure_start_unix_ns"`
	model.CompetitionWorkCoverageSummary
	Completions []workCoverageCompletion `json:"completions"`
}

// All root inventories exist before measurement; a missing launch therefore
// cannot remove the root or any descendant from the denominator.
type workCoverageCollector struct {
	plan  *workCoveragePlan
	start time.Time
	lock  sync.Mutex
	seen  map[[2]int]bool
	rows  []workCoverageCompletion
}

// Captures the barrier once, never at a late actual crawl start.
func newWorkCoverageCollector(plan *workCoveragePlan, start time.Time) (*workCoverageCollector, error) {
	if plan == nil || start.UnixNano() <= 0 || plan.DurationNs <= 0 || plan.TimeoutNs <= 0 ||
		math.MaxInt64-start.UnixNano() < plan.DurationNs || math.MaxInt64-start.UnixNano()-plan.DurationNs < plan.TimeoutNs {
		return nil, fmt.Errorf("invalid work coverage barrier or deadline")
	}
	return &workCoverageCollector{plan: plan, start: start, seen: map[[2]int]bool{}, rows: []workCoverageCompletion{}}, nil
}

// A received URI can select only already inventoried work at the exact depth.
func (self *workCoverageCollector) page(root int, uri string, depth int) (int, workCoveragePage, bool) {
	if root < 0 || len(self.plan.Roots) <= root {
		return 0, workCoveragePage{}, false
	}
	for index, page := range self.plan.Roots[root].Pages {
		if page.Uri == uri && page.Depth == depth {
			return index, page, true
		}
	}
	return 0, workCoveragePage{}, false
}

// A completion exactly at its deadline is too late. The end of measurement
// closes root admission, not credit for already offered roots during drain.
func (self *workCoverageCollector) record(root, page, row int, started, completed time.Time) bool {
	if root < 0 || len(self.plan.Roots) <= root || page < 0 || len(self.plan.Roots[root].Pages) <= page || row < 0 {
		return false
	}
	admission := self.start.Add(time.Duration(self.plan.Roots[root].OffsetNs))
	deadline := admission.Add(time.Duration(self.plan.TimeoutNs))
	if started.Before(admission) || completed.Before(started) || !completed.Before(deadline) {
		return false
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	key := [2]int{root, page}
	if self.seen[key] {
		return false
	}
	self.seen[key] = true
	self.rows = append(self.rows, workCoverageCompletion{Root: root, Page: page, Row: row, StartedUnixNs: started.UnixNano(), CompletedUnixNs: completed.UnixNano()})
	return true
}

// Snapshots joined completion facts; expected counts never depend on rows.
func (self *workCoverageCollector) report() *workCoverageReport {
	self.lock.Lock()
	defer self.lock.Unlock()
	summary := model.CompetitionWorkCoverageSummary{
		PlanSha256: self.plan.Sha256, OfferedRoots: int64(len(self.plan.Roots)),
		ExpectedPages: self.plan.TotalPages, ExpectedBytes: self.plan.TotalBytes,
		CompletedPages: int64(len(self.rows)),
	}
	for _, completion := range self.rows {
		summary.CompletedBytes += self.plan.Roots[completion.Root].Pages[completion.Page].Bytes
	}
	summary.PageCoverage = float64(summary.CompletedPages) / float64(summary.ExpectedPages)
	summary.ByteCoverage = float64(summary.CompletedBytes) / float64(summary.ExpectedBytes)
	return &workCoverageReport{Kind: self.plan.Kind, MeasureStartUnixNs: self.start.UnixNano(), CompetitionWorkCoverageSummary: summary, Completions: append([]workCoverageCompletion{}, self.rows...)}
}

// Starts each scheduled root against the common barrier. Late scheduling can
// lose credit but cannot buy a fresh timeout. Unlaunched roots remain offered.
func (self *ClientDriver) runWorkCoverage() error {
	defer self.Close()
	if len(self.clients) == 0 {
		return fmt.Errorf("no warm clients for offered workload")
	}
	collector := self.workCoverage
	for index, root := range collector.plan.Roots {
		admission := collector.start.Add(time.Duration(root.OffsetNs))
		timer := time.NewTimer(max(0, time.Until(admission)))
		select {
		case <-self.ctx.Done():
			timer.Stop()
			self.active.Wait()
			return self.flush()
		case <-self.arrivalsStop:
			timer.Stop()
			self.active.Wait()
			return self.flush()
		case <-timer.C:
		}
		deadline := admission.Add(time.Duration(collector.plan.TimeoutNs))
		if !time.Now().Before(deadline) || !time.Now().Before(collector.start.Add(time.Duration(collector.plan.DurationNs))) {
			continue
		}
		client, index := self.clients[index%len(self.clients)], index
		self.active.Add(1)
		go func() {
			defer self.active.Done()
			ctx, cancel := context.WithDeadline(self.ctx, deadline)
			defer cancel()
			self.crawl(ctx, client.label, client.httpClient, index)
		}()
	}
	// Do not signal an early driver exit merely because the fixed schedule has
	// no more roots: the ordinary measurement owner still closes admission.
	select {
	case <-self.ctx.Done():
	case <-self.arrivalsStop:
	}
	self.active.Wait()
	return self.flush()
}

// Consumes only the exact expected representation plus one detection byte.
// Extra/duplicate JSON keys, forged children, partial bodies and altered
// padding never receive completion credit or generate discovered work.
func readExpectedSiteResponse(response *http.Response, expected workCoveragePage) (result siteResponse, retErr error) {
	if response == nil || response.Body == nil || expected.Bytes <= 0 || expected.Page.Size < 0 || expected.Bytes == math.MaxInt64 {
		return result, fmt.Errorf("missing expected site response")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, err)
			result.complete = false
		}
	}()
	header, err := json.Marshal(expected.Page)
	if err != nil || int64(len(header))+1+int64(expected.Page.Size) != expected.Bytes {
		return result, fmt.Errorf("invalid expected site descriptor")
	}
	header = append(header, '\n')
	reader := io.LimitReader(response.Body, expected.Bytes+1)
	buffer := make([]byte, 32<<10)
	valid := response.StatusCode == http.StatusOK && (response.ContentLength < 0 || response.ContentLength == expected.Bytes)
	for {
		n, readErr := reader.Read(buffer)
		for i, value := range buffer[:n] {
			offset := result.receivedBytes + int64(i)
			var want byte
			if offset < int64(len(header)) {
				want = header[offset]
			} else {
				want = byte('a' + ((offset-int64(len(header)))%(32<<10))%26)
			}
			if offset >= expected.Bytes || value != want {
				valid = false
			}
		}
		result.receivedBytes += int64(n)
		if readErr != nil {
			if readErr != io.EOF {
				return result, readErr
			}
			break
		}
		if n == 0 {
			return result, io.ErrNoProgress
		}
	}
	result.complete = valid && result.receivedBytes == expected.Bytes
	if result.complete {
		result.page = expected.Page
	}
	return result, nil
}

// Canonical equality is separate from the body consumer and useful to the
// frozen-reference tests without a network fixture.
func equalWorkCoveragePage(left, right sitePage) bool {
	a, aErr := json.Marshal(left)
	b, bErr := json.Marshal(right)
	return aErr == nil && bErr == nil && bytes.Equal(a, b)
}
