package evidencemove

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Spaces object starts evenly at a fixed rate across all workers, with no
// burst: a wait never banks time, so a long pause does not release a burst.
type Pacer struct {
	lock     sync.Mutex
	interval time.Duration
	next     time.Time
	now      func() time.Time
}

func NewPacer(perSecond float64) *Pacer {
	return &Pacer{interval: time.Duration(float64(time.Second) / perSecond), now: time.Now}
}

// Blocks until this caller's start time, or ctx ends.
func (self *Pacer) Wait(ctx context.Context) error {
	self.lock.Lock()
	now := self.now()
	if self.next.Before(now) {
		self.next = now
	}
	at := self.next
	self.next = self.next.Add(self.interval)
	self.lock.Unlock()
	wait := at.Sub(now)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Request latencies by operation, kept as a bounded reservoir per operation
// for percentiles, with exact counts and maxima.
type Latencies struct {
	lock sync.Mutex
	ops  map[string]*latencyOp
}

type latencyOp struct {
	count   int64
	total   time.Duration
	max     time.Duration
	samples []time.Duration
	next    int
}

const latencySamples = 4096

func NewLatencies() *Latencies {
	return &Latencies{ops: map[string]*latencyOp{}}
}

func (self *Latencies) Observe(operation string, elapsed time.Duration) {
	self.lock.Lock()
	defer self.lock.Unlock()
	op, ok := self.ops[operation]
	if !ok {
		op = &latencyOp{}
		self.ops[operation] = op
	}
	op.count += 1
	op.total += elapsed
	op.max = max(op.max, elapsed)
	if len(op.samples) < latencySamples {
		op.samples = append(op.samples, elapsed)
	} else {
		// the latest window: overwrite the oldest sample
		op.samples[op.next] = elapsed
		op.next = (op.next + 1) % latencySamples
	}
}

type LatencySummary struct {
	Count int64         `json:"count"`
	Mean  time.Duration `json:"mean"`
	P50   time.Duration `json:"p50"`
	P95   time.Duration `json:"p95"`
	P99   time.Duration `json:"p99"`
	Max   time.Duration `json:"max"`
}

// Percentiles are over the latest samples; count, mean and max over all.
func (self *Latencies) Summaries() map[string]LatencySummary {
	self.lock.Lock()
	defer self.lock.Unlock()
	summaries := map[string]LatencySummary{}
	for operation, op := range self.ops {
		sorted := slices.Clone(op.samples)
		slices.Sort(sorted)
		at := func(q float64) time.Duration {
			if len(sorted) == 0 {
				return 0
			}
			return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
		}
		summaries[operation] = LatencySummary{
			Count: op.count,
			Mean:  op.total / time.Duration(max(1, op.count)),
			P50:   at(0.50),
			P95:   at(0.95),
			P99:   at(0.99),
			Max:   op.max,
		}
	}
	return summaries
}

// One line of every operation's p50/p95/max, for progress logs.
func (self *Latencies) Line() string {
	summaries := self.Summaries()
	operations := make([]string, 0, len(summaries))
	for operation := range summaries {
		operations = append(operations, operation)
	}
	slices.Sort(operations)
	parts := make([]string, 0, len(operations))
	for _, operation := range operations {
		summary := summaries[operation]
		parts = append(parts, fmt.Sprintf(
			"%s n=%d p50=%s p95=%s max=%s",
			operation, summary.Count, summary.P50.Round(time.Millisecond), summary.P95.Round(time.Millisecond), summary.Max.Round(time.Millisecond),
		))
	}
	if len(parts) == 0 {
		return "none yet"
	}
	return strings.Join(parts, ", ")
}
