// Pure drain-policy coverage. An in-memory shard follows the read script's
// admission, cleanup and paging rules on simulated stream ids and a simulated
// clock, so capacity is proven by arithmetic over claims, never by elapsed time.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type testingTallyEntry struct {
	millis int64
	seq    int64
	record ProviderEgressTallyRecord
	size   int64
}

func (self testingTallyEntry) id() string {
	return fmt.Sprintf("%d-%d", self.millis, self.seq)
}

// Admission and cleanup follow the append and read scripts: a full shard
// refuses whole records, and a call deletes at most one page of records at or
// before the durable cursor before paging after its read cursor.
type testingTallyQueue struct {
	t          testing.TB
	entries    []testingTallyEntry
	head       int
	count      int64
	bytes      int64
	lastMillis int64
	lastSeq    int64
	calls      int
	// failCall makes that 1-based call fail; zero never fails.
	failCall int
	// Each call advances the simulated clock by callTime.
	now      time.Time
	callTime time.Duration
}

func newTestingTallyQueue(t testing.TB) *testingTallyQueue {
	return &testingTallyQueue{t: t, now: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
}

func (self *testingTallyQueue) push(atMillis int64, record ProviderEgressTallyRecord, size int64) bool {
	if providerEgressTallyQueueLength <= self.count || providerEgressTallyQueueBytes < self.bytes+size {
		return false
	}
	millis, seq := atMillis, int64(0)
	if millis <= self.lastMillis {
		millis, seq = self.lastMillis, self.lastSeq+1
	}
	self.lastMillis, self.lastSeq = millis, seq
	self.entries = append(self.entries, testingTallyEntry{millis: millis, seq: seq, record: record, size: size})
	self.count += 1
	self.bytes += size
	return true
}

func testingParseTallyId(t testing.TB, id string) (int64, int64) {
	t.Helper()
	parts := strings.Split(id, "-")
	if len(parts) != 2 {
		t.Fatalf("invalid tally id %q", id)
	}
	millis, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return millis, seq
}

func testingTallyIdAfter(millis, seq, afterMillis, afterSeq int64) bool {
	return millis > afterMillis || (millis == afterMillis && seq > afterSeq)
}

// The page reader for an owner whose durable cursor is ackCursor.
func (self *testingTallyQueue) reader(ackCursor string) func(context.Context, string, int, bool) (*providerEgressTallyCall, error) {
	ackMillis, ackSeq := testingParseTallyId(self.t, ackCursor)
	return func(ctx context.Context, fromCursor string, pageLength int, first bool) (*providerEgressTallyCall, error) {
		self.calls += 1
		self.now = self.now.Add(self.callTime)
		if self.calls == self.failCall {
			return nil, errors.New("synthetic tally read failure")
		}
		fromMillis, fromSeq := testingParseTallyId(self.t, fromCursor)
		if testingTallyIdAfter(ackMillis, ackSeq, fromMillis, fromSeq) {
			return nil, errors.New("tally read cursor outside retained range")
		}
		for deleted := 0; deleted < providerEgressTallyPageLength && self.head < len(self.entries); deleted++ {
			entry := self.entries[self.head]
			if testingTallyIdAfter(entry.millis, entry.seq, ackMillis, ackSeq) {
				break
			}
			self.count -= 1
			self.bytes -= entry.size
			self.head += 1
		}
		if 4096 < self.head && len(self.entries) < 2*self.head {
			self.entries = append([]testingTallyEntry(nil), self.entries[self.head:]...)
			self.head = 0
		}
		retained := self.entries[self.head:]
		start := sort.Search(len(retained), func(i int) bool {
			return testingTallyIdAfter(retained[i].millis, retained[i].seq, fromMillis, fromSeq)
		})
		call := &providerEgressTallyCall{nextCursor: fromCursor}
		bytes := int64(0)
		end := start
		for end < len(retained) && end-start < pageLength && bytes+retained[end].size <= providerEgressTallyPageBytes {
			bytes += retained[end].size
			call.records = append(call.records, retained[end].record)
			call.nextCursor = retained[end].id()
			end += 1
		}
		call.more = end < len(retained)
		call.dirty = 0 < len(retained) && !testingTallyIdAfter(retained[0].millis, retained[0].seq, ackMillis, ackSeq)
		call.backlog = ProviderEgressTallyBacklog{Records: self.count, Bytes: self.bytes, ObservedAt: self.now}
		pending := sort.Search(len(retained), func(i int) bool {
			return testingTallyIdAfter(retained[i].millis, retained[i].seq, ackMillis, ackSeq)
		})
		if pending < len(retained) {
			call.backlog.PendingSince = time.UnixMilli(retained[pending].millis)
		}
		return call, nil
	}
}

func (self *testingTallyQueue) clock() time.Time {
	return self.now
}

// A URL-probe-shaped run: one place and one measured destination, as the
// scheduled URL probe hands off.
func testingUrlProbeTally(at time.Time, site int) ProviderEgressTallyRecord {
	return ProviderEgressTallyRecord{
		MeasuredAt: at,
		Run:        ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic drain"}, Healthy: true},
		Loads:      []ProviderEgressSiteLoad{{Name: fmt.Sprintf("synthetic-%d.example", site), Ok: true, Healthy: true}},
	}
}

func testingTallySize(t testing.TB, record ProviderEgressTallyRecord) int64 {
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(raw))
}

func (self *testingTallyQueue) fill(t testing.TB, n int, site func(int) int) {
	t.Helper()
	for i := range n {
		record := testingUrlProbeTally(self.now, site(i))
		if !self.push(self.now.UnixMilli(), record, testingTallySize(t, record)) {
			t.Fatalf("fixture shard refused record %d", i)
		}
	}
}

// The batch layer carries one full shard as one owner execution's handback,
// not a single page. Before the fix every batch above 256 runs was refused, so
// no claim could commit more than one page.
func TestProviderEgressTallyBatchHoldsOneFullShardDrain(t *testing.T) {
	at := time.Date(2026, 10, 14, 2, 40, 0, 0, time.UTC)
	records := make([]ProviderEgressTallyRecord, providerEgressTallyQueueLength)
	for i := range records {
		records[i] = ProviderEgressTallyRecord{
			MeasuredAt: at,
			Run:        ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic drain"}, Healthy: i%2 == 0},
			Loads:      []ProviderEgressSiteLoad{{Name: fmt.Sprintf("synthetic-%d.example", i%100), Ok: i%3 != 0, Healthy: true}},
		}
	}
	batch, err := prepareProviderEgressTallyBatch(records)
	if err != nil {
		t.Fatalf("one full shard drain refused: %v", err)
	}
	ValidateProviderEgressTallyBatch(batch)
	if len(batch.Places) != 1 || batch.Places[0].Runs != len(records) || batch.Places[0].HealthyRuns != len(records)/2 || len(batch.Sites) != 100 {
		t.Fatalf("drain batch places=%+v sites=%d", batch.Places, len(batch.Sites))
	}
	loads, failures := 0, 0
	for _, site := range batch.Sites {
		loads += site.Loads
		failures += site.Failures
	}
	if loads != len(records) || failures != (len(records)+2)/3 {
		t.Fatalf("drain batch loads=%d failures=%d", loads, failures)
	}
	if _, err := prepareProviderEgressTallyBatch(append(records, records[0])); err == nil {
		t.Fatal("batch beyond one full shard accepted")
	}
}

// One claim reads every retained page, not one. Reading acknowledges nothing:
// all records remain until a committed successor names them.
func TestProviderEgressTallyDrainReadsEveryRetainedPage(t *testing.T) {
	queue := newTestingTallyQueue(t)
	queue.fill(t, 1000, func(i int) int { return i % 7 })
	drain, err := drainProviderEgressTallies(t.Context(), "0-0", false, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock)
	if err != nil {
		t.Fatal(err)
	}
	last := queue.entries[len(queue.entries)-1].id()
	if drain.Records != 1000 || drain.Pages != 4 || drain.Calls != 4 || drain.Stop != "caught_up" || drain.Err != nil || drain.NextCursor != last {
		t.Fatalf("drain records=%d pages=%d calls=%d stop=%q err=%v next=%q want next=%q", drain.Records, drain.Pages, drain.Calls, drain.Stop, drain.Err, drain.NextCursor, last)
	}
	if len(drain.Batch.Places) != 1 || drain.Batch.Places[0].Runs != 1000 || len(drain.Batch.Sites) != 7 {
		t.Fatalf("drain batch=%+v", drain.Batch)
	}
	if queue.count != 1000 || drain.Backlog.Records != 1000 {
		t.Fatalf("an uncommitted drain deleted records: retained=%d backlog=%d", queue.count, drain.Backlog.Records)
	}
}

// The successor's calls delete the whole committed prefix, in step with its
// own reads and then by cleanup-only calls, and never a record after the
// durable cursor; admission space returns as fast as pages are read.
func TestProviderEgressTallyDrainCleansOnlyTheCommittedPrefix(t *testing.T) {
	queue := newTestingTallyQueue(t)
	queue.fill(t, 1000, func(i int) int { return i })
	first, err := drainProviderEgressTallies(t.Context(), "0-0", false, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock)
	if err != nil || first.Records != 1000 {
		t.Fatalf("first drain records=%d err=%v", first.Records, err)
	}
	queue.now = queue.now.Add(time.Second)
	queue.fill(t, 300, func(i int) int { return i })
	// A replay of the uncommitted drain rereads the identical prefix.
	replay, err := drainProviderEgressTallies(t.Context(), "0-0", false, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock)
	if err != nil || replay.Records != 1300 || queue.count != 1300 {
		t.Fatalf("uncommitted prefix changed: replay=%d retained=%d err=%v", replay.Records, queue.count, err)
	}
	queue.calls = 0
	second, err := drainProviderEgressTallies(t.Context(), first.NextCursor, false, defaultProviderEgressTallyDrainBudget(), queue.reader(first.NextCursor), queue.clock)
	if err != nil {
		t.Fatal(err)
	}
	if second.Records != 300 || second.Stop != "caught_up" || second.NextCursor != queue.entries[len(queue.entries)-1].id() {
		t.Fatalf("successor records=%d stop=%q next=%q", second.Records, second.Stop, second.NextCursor)
	}
	// Two reading calls clean 512; two cleanup-only calls clean the other 488.
	// The drain reports the claim's occupancy: 1300 less the first call's 256.
	if second.Calls != 4 || queue.count != 300 || second.Backlog.Records != 1300-providerEgressTallyPageLength {
		t.Fatalf("committed prefix not cleaned in step: calls=%d retained=%d backlog=%d", second.Calls, queue.count, second.Backlog.Records)
	}
	committedMillis, committedSeq := testingParseTallyId(t, first.NextCursor)
	for _, entry := range queue.entries[queue.head:] {
		if !testingTallyIdAfter(entry.millis, entry.seq, committedMillis, committedSeq) {
			t.Fatal("a committed record survived cleanup")
		}
	}
}

// The shared-row budget ends a drain on a page boundary. The excluded page
// stays retained and is the next claim's first page, so totals stay exact.
func TestProviderEgressTallyDrainStopsOnAPageBoundaryAtTheRowBudget(t *testing.T) {
	queue := newTestingTallyQueue(t)
	queue.fill(t, 600, func(i int) int { return i })
	budget := defaultProviderEgressTallyDrainBudget()
	budget.rows = 300
	first, err := drainProviderEgressTallies(t.Context(), "0-0", false, budget, queue.reader("0-0"), queue.clock)
	if err != nil {
		t.Fatal(err)
	}
	// One place plus 256 distinct sites fits; the second page would not.
	if first.Records != 256 || first.Pages != 1 || first.Stop != "rows" || first.NextCursor != queue.entries[255].id() || len(first.Batch.Places)+len(first.Batch.Sites) != 257 {
		t.Fatalf("row budget split or overran a page: records=%d pages=%d stop=%q rows=%d", first.Records, first.Pages, first.Stop, len(first.Batch.Places)+len(first.Batch.Sites))
	}
	total := first.Records
	cursor := first.NextCursor
	for range 3 {
		next, err := drainProviderEgressTallies(t.Context(), cursor, false, budget, queue.reader(cursor), queue.clock)
		if err != nil {
			t.Fatal(err)
		}
		total += next.Records
		cursor = next.NextCursor
	}
	if total != 600 || cursor != queue.entries[len(queue.entries)-1].id() {
		t.Fatalf("row-budget drains lost or repeated records: total=%d", total)
	}
}

// A later call's failure keeps the complete pages before it; only the first
// call's failure fails the owner, keeping repair and retry meanings unchanged.
func TestProviderEgressTallyDrainLaterFailureKeepsTheCompletePrefix(t *testing.T) {
	queue := newTestingTallyQueue(t)
	queue.fill(t, 1000, func(i int) int { return i % 5 })
	queue.failCall = 3
	drain, err := drainProviderEgressTallies(t.Context(), "0-0", false, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock)
	if err != nil {
		t.Fatal(err)
	}
	if drain.Records != 512 || drain.Pages != 2 || drain.Stop != "error" || drain.Err == nil || drain.NextCursor != queue.entries[511].id() || drain.Batch.Places[0].Runs != 512 {
		t.Fatalf("partial drain records=%d pages=%d stop=%q err=%v next=%q", drain.Records, drain.Pages, drain.Stop, drain.Err, drain.NextCursor)
	}
	queue.calls = 0
	queue.failCall = 1
	if failed, err := drainProviderEgressTallies(t.Context(), "0-0", false, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock); err == nil || failed != nil {
		t.Fatalf("first-call failure became a drain: %+v", failed)
	}
}

// The time budget is checked between calls on the injected clock.
func TestProviderEgressTallyDrainTimeBudgetStopsBetweenCalls(t *testing.T) {
	queue := newTestingTallyQueue(t)
	queue.fill(t, 2000, func(i int) int { return i % 5 })
	queue.callTime = 2 * time.Second
	drain, err := drainProviderEgressTallies(t.Context(), "0-0", false, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock)
	if err != nil {
		t.Fatal(err)
	}
	// Calls start at 0s, 2s and 4s of the 5s budget.
	if drain.Calls != 3 || drain.Records != 768 || drain.Stop != "time" {
		t.Fatalf("time budget calls=%d records=%d stop=%q", drain.Calls, drain.Records, drain.Stop)
	}
}

// An initializing owner keeps the single-page bootstrap: one call only.
func TestProviderEgressTallyDrainInitializeMakesOneCall(t *testing.T) {
	queue := newTestingTallyQueue(t)
	drain, err := drainProviderEgressTallies(t.Context(), "0-0", true, defaultProviderEgressTallyDrainBudget(), queue.reader("0-0"), queue.clock)
	if err != nil || drain.Calls != 1 || drain.Stop != "initialize" || drain.Records != 0 || drain.NextCursor != "0-0" {
		t.Fatalf("initialize drain=%+v err=%v", drain, err)
	}
}

// Simulates one shard's producer at rate runs a second for duration, with the
// owner claimed every cycle. Each claim drains under budget and commits its
// cursor as the finishing transaction does; the next claim cleans that prefix.
// After the producer stops, claims continue until the shard is empty, and
// every accepted run must have been committed exactly once.
func testingSimulateTallyOwner(t *testing.T, budget providerEgressTallyDrainBudget, rate float64, cycle time.Duration, duration time.Duration) (accepted int, full int) {
	t.Helper()
	queue := newTestingTallyQueue(t)
	start := queue.now
	size := testingTallySize(t, testingUrlProbeTally(start, 0))
	cursor := "0-0"
	committed := 0
	claim := func(at time.Time) {
		queue.now = at
		drain, err := drainProviderEgressTallies(t.Context(), cursor, false, budget, queue.reader(cursor), queue.clock)
		if err != nil {
			t.Fatal(err)
		}
		ValidateProviderEgressTallyBatch(drain.Batch)
		committed += drain.Records
		cursor = drain.NextCursor
	}
	nextClaim := start.Add(cycle)
	for i := 0; ; i++ {
		at := start.Add(time.Duration(float64(i) / rate * float64(time.Second)))
		if !at.Before(start.Add(duration)) {
			break
		}
		for !nextClaim.After(at) {
			claim(nextClaim)
			nextClaim = nextClaim.Add(cycle)
		}
		if queue.push(at.UnixMilli(), testingUrlProbeTally(at, i%64), size) {
			accepted += 1
		} else {
			full += 1
		}
	}
	for range 1 + 2*providerEgressTallyQueueLength/providerEgressTallyPageLength {
		claim(nextClaim)
		nextClaim = nextClaim.Add(cycle)
		if queue.count == 0 {
			break
		}
	}
	if queue.count != 0 || committed != accepted {
		t.Fatalf("simulated owner retained=%d committed=%d accepted=%d", queue.count, committed, accepted)
	}
	return accepted, full
}

// Main on 2026-10-14: the owner's successor waited about 50s (up to 100s) for
// its claim, the hottest place shard took about 35% of runs, steady demand was
// about 43 runs/s and due waves reached about 90 runs/s. One page per claim
// capped a shard at 256/50s, about 5 runs/s, so that shard refused handoffs
// from about 15 runs/s fleet-wide. One drain per claim keeps twice the wave
// peak, 64 runs/s on that shard, with no refusal even at 100s claims; the
// buffer then bounds a shard near 16384 records per two claims (cleanup waits
// for the committed successor), which the last row pins.
func TestProviderEgressTallyOwnerKeepsUpWithTwiceTheWavePeak(t *testing.T) {
	onePage := defaultProviderEgressTallyDrainBudget()
	onePage.calls = 1
	hotShare := 0.354
	for _, c := range []struct {
		name     string
		budget   providerEgressTallyDrainBudget
		rate     float64
		cycle    time.Duration
		wantFull bool
	}{
		{name: "one page per claim, steady demand", budget: onePage, rate: hotShare * 43, cycle: 50 * time.Second, wantFull: true},
		{name: "one page per claim, twice the wave peak", budget: onePage, rate: hotShare * 180, cycle: 50 * time.Second, wantFull: true},
		{name: "drain per claim, steady demand", budget: defaultProviderEgressTallyDrainBudget(), rate: hotShare * 43, cycle: 50 * time.Second, wantFull: false},
		{name: "drain per claim, twice the wave peak", budget: defaultProviderEgressTallyDrainBudget(), rate: hotShare * 180, cycle: 50 * time.Second, wantFull: false},
		{name: "drain per claim, twice the wave peak, slow claims", budget: defaultProviderEgressTallyDrainBudget(), rate: hotShare * 180, cycle: 100 * time.Second, wantFull: false},
		{name: "drain per claim beyond the two-claim buffer", budget: defaultProviderEgressTallyDrainBudget(), rate: 96, cycle: 100 * time.Second, wantFull: true},
	} {
		accepted, full := testingSimulateTallyOwner(t, c.budget, c.rate, c.cycle, 2*time.Hour)
		t.Logf("%s: %.1f runs/s, %s claims: accepted=%d full=%d", c.name, c.rate, c.cycle, accepted, full)
		if (0 < full) != c.wantFull {
			t.Errorf("%s: accepted=%d full=%d (%.1f%% lost) want full=%t", c.name, accepted, full, 100*float64(full)/math.Max(1, float64(accepted+full)), c.wantFull)
		}
	}
}

func testingGaugeValues(t *testing.T, collector *providerEgressTallyBacklogCollector) map[string]map[string]float64 {
	t.Helper()
	metrics := make(chan prometheus.Metric, 256)
	collector.Collect(metrics)
	close(metrics)
	values := map[string]map[string]float64{}
	for metric := range metrics {
		var written dto.Metric
		if err := metric.Write(&written); err != nil {
			t.Fatal(err)
		}
		name := metric.Desc().String()
		shard := ""
		for _, label := range written.GetLabel() {
			if label.GetName() == "shard" {
				shard = label.GetValue()
			}
		}
		if values[shard] == nil {
			values[shard] = map[string]float64{}
		}
		for _, metricName := range []string{"queue_records", "queue_bytes", "queue_fill_ratio", "queue_oldest_pending_seconds", "queue_observed_timestamp_seconds"} {
			if strings.Contains(name, "urnetwork_provider_egress_tally_"+metricName+"\"") {
				values[shard][metricName] = written.GetGauge().GetValue()
			}
		}
	}
	return values
}

// Backlog depth is visible before refusal: the fuller bound is reported as a
// fill ratio, and the depth of a shard this process has not read recently is
// withheld; its observed time is not (see the backlog tests).
func TestProviderEgressTallyBacklogGaugeReportsFreshDepthsOnly(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 40, 0, 0, time.UTC)
	collector := newProviderEgressTallyBacklogCollector(func() time.Time { return now })
	collector.observe(3, ProviderEgressTallyBacklog{Records: 8192, Bytes: 1024 * 1024, PendingSince: now.Add(-90 * time.Second), ObservedAt: now.Add(-10 * time.Second)})
	collector.observe(3, ProviderEgressTallyBacklog{Records: 1, ObservedAt: now.Add(-20 * time.Second)})
	collector.observe(7, ProviderEgressTallyBacklog{Records: 16384, ObservedAt: now.Add(-6 * time.Minute)})
	collector.observe(9, ProviderEgressTallyBacklog{Records: 10, Bytes: 12 * 1024 * 1024, ObservedAt: now})
	values := testingGaugeValues(t, collector)
	depths := 0
	for _, gauges := range values {
		if _, ok := gauges["queue_records"]; ok {
			depths += 1
		}
	}
	if _, stale := values["7"]["queue_records"]; depths != 2 || stale {
		t.Fatalf("stale or missing shard depths published: %+v", values)
	}
	three := values["3"]
	if three["queue_records"] != 8192 || three["queue_bytes"] != 1024*1024 || three["queue_fill_ratio"] != 0.5 || three["queue_oldest_pending_seconds"] != 80 || three["queue_observed_timestamp_seconds"] != float64(now.Add(-10*time.Second).Unix()) {
		t.Fatalf("an older read replaced a newer one or a gauge is wrong: %+v", three)
	}
	if nine := values["9"]; nine["queue_fill_ratio"] != 0.75 || nine["queue_oldest_pending_seconds"] != 0 {
		t.Fatalf("the byte bound did not dominate: %+v", nine)
	}
}
