package main

// What the soak compares (§22.8's soak row, D-TEST-7; TEST-M2-14): each signal at the run's end against its
// baseline, both read after the load stops and a quiet period lets caches expire, so a cache's fill never reads as a
// leak. A phase's reading is the median of its samples, each taken after a collection, so one burst of garbage or a
// goroutine passing through moves nothing.
//
//	heap_bytes      live heap after a collection: back while the end's is at most the baseline's times
//	                (1 + HeapGrowth) plus HeapSlackBytes, which leaves room for caches that are bounded and full
//	goroutines      back within GoroutineSlack of the baseline
//	redis_keys      back within RedisKeySlack: soft state that rebuilds from its owner stays the same size
//	orphan_objects  a count by orphan GC's own definition (EVSB-M2-13), through an orphanCounter, back within
//	                OrphanSlack. evsservice offers no count that deletes nothing, so no target here reads one yet,
//	                and the signal reads open (question test-m2-03-soak-orphan-count), never back
//
// A signal its target cannot read honestly reads open, with why: never back, and never a pass's evidence.

import (
	"context"
	"fmt"
	"slices"

	"github.com/urnetwork/sidecar/sidecartest/load"
)

type soakSignal string

const (
	soakSignalHeap       soakSignal = "heap_bytes"
	soakSignalGoroutines soakSignal = "goroutines"
	soakSignalRedisKeys  soakSignal = "redis_keys"
	soakSignalOrphans    soakSignal = "orphan_objects"
)

// Every signal, in the order a report lists them.
var soakSignals = []soakSignal{soakSignalHeap, soakSignalGoroutines, soakSignalRedisKeys, soakSignalOrphans}

type soakOutcome string

const (
	// Within its tolerance of the baseline.
	soakBack soakOutcome = "back"
	// Past it: a leak, which fails the run.
	soakLeaked soakOutcome = "leaked"
	// Not read, or not readable honestly on this target: never a pass.
	soakOpen soakOutcome = "open"
)

// How far each signal may stand above its baseline and still read back.
type soakTolerances struct {
	// The heap may grow by this share of its baseline, plus HeapSlackBytes.
	HeapGrowth     float64
	HeapSlackBytes int64
	GoroutineSlack int64
	RedisKeySlack  int64
	OrphanSlack    int64
}

// A tenth of the heap and 16 MiB, eight goroutines, sixteen keys and no orphan: room for bounded caches and the
// runtime's own, and none for growth that tracks the load.
func defaultSoakTolerances() *soakTolerances {
	return &soakTolerances{
		HeapGrowth:     0.1,
		HeapSlackBytes: 16 * 1024 * 1024,
		GoroutineSlack: 8,
		RedisKeySlack:  16,
		OrphanSlack:    0,
	}
}

// Counts orphan objects by orphan GC's own definition, deleting nothing.
type orphanCounter interface {
	CountOrphans(ctx context.Context) (int64, error)
}

// One phase's reading: its samples and its orphan count, -1 when unread with why.
type soakReading struct {
	Samples    []load.Sample `json:"samples"`
	Orphans    int64         `json:"orphans"`
	OrphansWhy string        `json:"orphans_why,omitempty"`
}

// The median of a signal over the reading's samples, false when no sample holds it.
func (self *soakReading) median(signal soakSignal) (int64, bool) {
	values := []int64{}
	for _, sample := range self.Samples {
		switch signal {
		case soakSignalHeap:
			values = append(values, int64(sample.HeapAllocBytes))
		case soakSignalGoroutines:
			values = append(values, int64(sample.Goroutines))
		case soakSignalRedisKeys:
			if 0 <= sample.RedisKeys {
				values = append(values, sample.RedisKeys)
			}
		}
	}
	if signal == soakSignalOrphans {
		return self.Orphans, 0 <= self.Orphans
	}
	if len(values) == 0 {
		return 0, false
	}
	slices.Sort(values)
	return values[len(values)/2], true
}

// One signal's verdict.
type soakVerdict struct {
	Signal   soakSignal  `json:"signal"`
	Baseline int64       `json:"baseline"`
	End      int64       `json:"end"`
	Limit    int64       `json:"limit"`
	Outcome  soakOutcome `json:"outcome"`
	Why      string      `json:"why,omitempty"`
}

// Each signal of end against baseline within tolerances; a signal in open, or one either reading lacks, reads open.
func compareSoak(baseline *soakReading, end *soakReading, tolerances *soakTolerances, open map[soakSignal]string) []*soakVerdict {
	verdicts := []*soakVerdict{}
	for _, signal := range soakSignals {
		verdict := &soakVerdict{Signal: signal}
		verdicts = append(verdicts, verdict)
		if why, isOpen := open[signal]; isOpen {
			verdict.Outcome = soakOpen
			verdict.Why = why
			continue
		}
		baselineValue, baselineRead := baseline.median(signal)
		endValue, endRead := end.median(signal)
		if !baselineRead || !endRead {
			verdict.Outcome = soakOpen
			verdict.Why = "not read"
			if signal == soakSignalOrphans {
				verdict.Why = firstNonEmpty(end.OrphansWhy, baseline.OrphansWhy, "not read")
			}
			continue
		}
		verdict.Baseline = baselineValue
		verdict.End = endValue
		switch signal {
		case soakSignalHeap:
			verdict.Limit = baselineValue + int64(float64(baselineValue)*tolerances.HeapGrowth) + tolerances.HeapSlackBytes
		case soakSignalGoroutines:
			verdict.Limit = baselineValue + tolerances.GoroutineSlack
		case soakSignalRedisKeys:
			verdict.Limit = baselineValue + tolerances.RedisKeySlack
		case soakSignalOrphans:
			verdict.Limit = baselineValue + tolerances.OrphanSlack
		}
		if verdict.Limit < endValue {
			verdict.Outcome = soakLeaked
			verdict.Why = fmt.Sprintf("%d at the end, past %d", endValue, verdict.Limit)
			continue
		}
		verdict.Outcome = soakBack
	}
	return verdicts
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
