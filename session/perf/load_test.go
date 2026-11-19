package perf

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/session"
)

// One timed call of a measured code path.
type opSample struct {
	start    int64 // unix nanos
	duration int64 // nanos
	tag      int16
}

type opSamples struct {
	samples  []opSample
	errors   map[string]int
	examples map[string]string
}

// Every measured call, by operation. Errors are classified by cause, so a
// designed refusal (a capacity code) is told apart from an unavailable store.
type opRecorder struct {
	lock sync.Mutex
	ops  map[string]*opSamples
	tags []string
}

func newOpRecorder() *opRecorder {
	return &opRecorder{ops: map[string]*opSamples{}, tags: []string{""}}
}

func (self *opRecorder) tagIndex(tag string) int16 {
	for index, name := range self.tags {
		if name == tag {
			return int16(index)
		}
	}
	self.tags = append(self.tags, tag)
	return int16(len(self.tags) - 1)
}

func (self *opRecorder) observe(op string, start time.Time, err error) {
	self.observeTagged(op, "", start, time.Since(start), classify(err), err)
}

func (self *opRecorder) observeClass(op string, start time.Time, class string, detail error) {
	self.observeTagged(op, "", start, time.Since(start), class, detail)
}

func (self *opRecorder) observeTagged(op string, tag string, start time.Time, duration time.Duration, class string, detail error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	samples := self.ops[op]
	if samples == nil {
		samples = &opSamples{errors: map[string]int{}, examples: map[string]string{}}
		self.ops[op] = samples
	}
	if class != "" {
		samples.errors[class]++
		if _, ok := samples.examples[class]; !ok && detail != nil {
			text := detail.Error()
			if 160 < len(text) {
				text = text[:160]
			}
			samples.examples[class] = text
		}
		return
	}
	samples.samples = append(samples.samples, opSample{start: start.UnixNano(), duration: int64(duration), tag: self.tagIndex(tag)})
}

// Error classes. A refusal carries the session machine code.
func classify(err error) string {
	if err == nil {
		return ""
	}
	var panicked *routePanic
	if errors.As(err, &panicked) {
		return "panic:" + classify(panicked.err)
	}
	var refusal *session.SessionError
	if errors.As(err, &refusal) {
		return "refused:" + refusal.Code
	}
	switch {
	case errors.Is(err, session.ErrSessionRevoked):
		return "rejected:session_revoked"
	case errors.Is(err, session.ErrAuthUnavailable), errors.Is(err, session.ErrSessionStoreUnavailable):
		return "unavailable"
	case errors.Is(err, session.ErrByJwtInactive):
		return "rejected:inactive"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "session_limit_reached"):
		return "refused:session_limit_reached"
	case strings.Contains(text, "session_retention_limit_reached"):
		return "refused:session_retention_limit_reached"
	case strings.Contains(text, "unavailable"):
		return "unavailable"
	}
	return "error"
}

type opSummary struct {
	Op       string            `json:"op"`
	Count    int               `json:"count"`
	Errors   map[string]int    `json:"errors,omitempty"`
	Examples map[string]string `json:"error_examples,omitempty"`
	P50Ms    float64           `json:"p50_ms"`
	P90Ms    float64           `json:"p90_ms"`
	P99Ms    float64           `json:"p99_ms"`
	P999Ms   float64           `json:"p999_ms"`
	MaxMs    float64           `json:"max_ms"`
	MeanMs   float64           `json:"mean_ms"`
	// calls per second over the span from the first call to the last
	Rate float64 `json:"rate_per_second"`
}

func (self *opSummary) errorCount() int {
	count := 0
	for _, value := range self.Errors {
		count += value
	}
	return count
}

// The error rate, counting only failures that are not a designed outcome.
func (self *opSummary) failureCount() int {
	count := 0
	for class, value := range self.Errors {
		if !strings.HasPrefix(class, "refused:") && !strings.HasPrefix(class, "rejected:") {
			count += value
		}
	}
	return count
}

func (self *opSummary) errorRate() float64 {
	total := self.Count + self.errorCount()
	if total == 0 {
		return 0
	}
	return float64(self.errorCount()) / float64(total)
}

// Summarizes one operation's samples, optionally only those that started in
// [from, to) and carry the tag.
func (self *opRecorder) summary(op string, from, to time.Time, tag *string) *opSummary {
	self.lock.Lock()
	defer self.lock.Unlock()
	summary := &opSummary{Op: op, Errors: map[string]int{}, Examples: map[string]string{}}
	samples := self.ops[op]
	if samples == nil {
		return summary
	}
	tagIndex := int16(-1)
	if tag != nil {
		tagIndex = -2
		for index, name := range self.tags {
			if name == *tag {
				tagIndex = int16(index)
			}
		}
	}
	durations := []int64{}
	var first, last int64 = math.MaxInt64, 0
	for _, sample := range samples.samples {
		if !from.IsZero() && sample.start < from.UnixNano() {
			continue
		}
		if !to.IsZero() && to.UnixNano() <= sample.start {
			continue
		}
		if tag != nil && sample.tag != tagIndex {
			continue
		}
		durations = append(durations, sample.duration)
		first = min(first, sample.start)
		last = max(last, sample.start+sample.duration)
	}
	if from.IsZero() && to.IsZero() && tag == nil {
		for class, count := range samples.errors {
			summary.Errors[class] = count
		}
		for class, example := range samples.examples {
			summary.Examples[class] = example
		}
	}
	summary.Count = len(durations)
	if len(durations) == 0 {
		return summary
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	quantile := func(q float64) float64 {
		index := int(math.Ceil(q*float64(len(durations)))) - 1
		index = max(0, min(len(durations)-1, index))
		return float64(durations[index]) / 1e6
	}
	var sum float64
	for _, duration := range durations {
		sum += float64(duration)
	}
	summary.P50Ms = quantile(0.50)
	summary.P90Ms = quantile(0.90)
	summary.P99Ms = quantile(0.99)
	summary.P999Ms = quantile(0.999)
	summary.MaxMs = float64(durations[len(durations)-1]) / 1e6
	summary.MeanMs = sum / float64(len(durations)) / 1e6
	if span := float64(last-first) / 1e9; 0 < span && 1 < len(durations) {
		summary.Rate = float64(len(durations)) / span
	}
	return summary
}

func (self *opRecorder) opNames() []string {
	self.lock.Lock()
	defer self.lock.Unlock()
	names := make([]string, 0, len(self.ops))
	for name := range self.ops {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// One open-loop workload: calls arrive at rate regardless of how long earlier
// calls take, up to workers in flight. An arrival that finds every worker
// busy and the queue full is dropped and counted, so a machine that cannot
// keep up shows it instead of silently slowing the arrival rate.
type load struct {
	name    string
	rate    float64
	workers int
	op      func(ctx context.Context)
}

type loadResult struct {
	Name     string  `json:"name"`
	Target   float64 `json:"target_per_second"`
	Issued   int64   `json:"issued"`
	Dropped  int64   `json:"dropped"`
	Done     int64   `json:"done"`
	Achieved float64 `json:"achieved_per_second"`
}

type runningLoads struct {
	stop    context.CancelFunc
	wait    sync.WaitGroup
	started time.Time
	results []*loadResult
	done    []*atomic.Int64
	dropped []*atomic.Int64
	issued  []*atomic.Int64
	ended   time.Time
}

// Starts the loads. Calls run under ctx, which the caller keeps alive until
// stop returns: stop ends the arrivals and waits for calls in flight, so no
// call is cut short and counted as an error by the end of a phase.
func startLoads(ctx context.Context, loads []*load) *runningLoads {
	return startRecordedLoads(ctx, loads, nil)
}

// startLoads, recording a panic an op raised under "load.<name>" in recorder.
func startRecordedLoads(ctx context.Context, loads []*load, recorder *opRecorder) *runningLoads {
	arrivalCtx, stop := context.WithCancel(ctx)
	running := &runningLoads{stop: stop, started: time.Now()}
	for _, item := range loads {
		if item.rate <= 0 || item.workers <= 0 {
			continue
		}
		result := &loadResult{Name: item.name, Target: item.rate}
		running.results = append(running.results, result)
		done, dropped, issued := &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}
		running.done = append(running.done, done)
		running.dropped = append(running.dropped, dropped)
		running.issued = append(running.issued, issued)
		arrivals := make(chan struct{}, item.workers*2)
		for range item.workers {
			running.wait.Add(1)
			go func() {
				defer running.wait.Done()
				for range arrivals {
					if err := routeCall(func() error { item.op(ctx); return nil }); err != nil {
						if recorder != nil {
							recorder.observeTagged("load."+item.name, "", time.Now(), 0, classify(err), err)
						}
					}
					done.Add(1)
				}
			}()
		}
		running.wait.Add(1)
		go func() {
			defer running.wait.Done()
			defer close(arrivals)
			start := time.Now()
			tick := 2 * time.Millisecond
			if item.rate < 50 {
				tick = 10 * time.Millisecond
			}
			ticker := time.NewTicker(tick)
			defer ticker.Stop()
			var count int64
			for {
				select {
				case <-arrivalCtx.Done():
					return
				case now := <-ticker.C:
					due := int64(item.rate * now.Sub(start).Seconds())
					for count < due {
						count++
						issued.Add(1)
						select {
						case arrivals <- struct{}{}:
						default:
							dropped.Add(1)
						}
					}
				}
			}
		}()
	}
	return running
}

func (self *runningLoads) finish() []*loadResult {
	self.stop()
	self.wait.Wait()
	self.ended = time.Now()
	elapsed := self.ended.Sub(self.started).Seconds()
	for index, result := range self.results {
		result.Issued = self.issued[index].Load()
		result.Dropped = self.dropped[index].Load()
		result.Done = self.done[index].Load()
		if 0 < elapsed {
			result.Achieved = float64(result.Done) / elapsed
		}
	}
	return self.results
}

// Runs fn count times over workers goroutines, closed loop.
func runParallel(count int, workers int, fn func(index int)) {
	var next atomic.Int64
	var wait sync.WaitGroup
	for range max(1, workers) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				index := int(next.Add(1) - 1)
				if count <= index {
					return
				}
				fn(index)
			}
		}()
	}
	wait.Wait()
}

func describeLoads(results []*loadResult) string {
	parts := []string{}
	for _, result := range results {
		parts = append(parts, fmt.Sprintf("%s %.1f/%.1f/s (dropped %d)", result.Name, result.Achieved, result.Target, result.Dropped))
	}
	return strings.Join(parts, ", ")
}
