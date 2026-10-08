//go:build acklineagetrace && h1memorytrace && darwin && cgo

package perfvar

import (
	"errors"
	"runtime/metrics"
	"sync"
	"time"
	"unsafe"
)

const (
	h1MemoryInterval           = 10 * time.Millisecond
	h1MemoryMaxLateness        = 20 * time.Millisecond
	h1MemorySampleLimit        = 8192
	h1MemoryPhaseLimit         = 16
	h1MemoryMinimumLoadSamples = 20
)

type h1MemoryPhase uint8

const (
	h1MemorySetup h1MemoryPhase = iota + 1
	h1MemoryRouteConstruction
	h1MemoryWorkloadSetup
	h1MemoryIdle
	h1MemoryBulkSetup
	h1MemoryLoad
	h1MemoryBulkJoin
	h1MemoryPostLoad
	h1MemoryPostJoin
	h1MemoryRouteTeardown
	h1MemoryFixtureTeardown
	h1MemoryEnd
)

func (p h1MemoryPhase) valid() bool { return h1MemorySetup <= p && p <= h1MemoryEnd }
func (p h1MemoryPhase) name() string {
	if !p.valid() {
		return "invalid"
	}
	return [...]string{"", "fixture-and-calibration", "route-construction", "workload-setup", "idle", "bulk-setup", "load", "bulk-join", "post-load", "post-join", "route-teardown", "fixture-teardown", "end"}[p]
}

type h1MemoryValues struct {
	HeapObjects      uint64 `json:"heap_objects_bytes"`
	HeapUnused       uint64 `json:"heap_unused_bytes"`
	HeapFree         uint64 `json:"heap_free_bytes"`
	HeapReleased     uint64 `json:"heap_released_bytes"`
	HeapStacks       uint64 `json:"heap_stacks_bytes"`
	OSStacks         uint64 `json:"os_stacks_bytes"`
	Total            uint64 `json:"runtime_total_bytes"`
	GCCycles         uint64 `json:"gc_cycles"`
	AllocatedBytes   uint64 `json:"allocated_bytes"`
	AllocatedObjects uint64 `json:"allocated_objects"`
	RSS              uint64 `json:"process_rss_bytes"`
}

func (v h1MemoryValues) heapAndStackInuse() uint64 {
	// Match the canonical MemStats HeapInuse+StackInuse, not RSS or HeapAlloc.
	return v.HeapObjects + v.HeapUnused + v.HeapStacks
}

func h1MemoryReader() func() (h1MemoryValues, error) {
	names := [...]string{"/memory/classes/heap/objects:bytes", "/memory/classes/heap/unused:bytes", "/memory/classes/heap/free:bytes", "/memory/classes/heap/released:bytes", "/memory/classes/heap/stacks:bytes", "/memory/classes/os-stacks:bytes", "/memory/classes/total:bytes", "/gc/cycles/total:gc-cycles", "/gc/heap/allocs:bytes", "/gc/heap/allocs:objects"}
	samples := make([]metrics.Sample, len(names))
	for i, name := range names {
		samples[i].Name = name
	}
	return func() (h1MemoryValues, error) {
		metrics.Read(samples)
		var values [len(names)]uint64
		for i := range samples {
			if samples[i].Value.Kind() != metrics.KindUint64 {
				return h1MemoryValues{}, errors.New("runtime-metric-unavailable")
			}
			values[i] = samples[i].Value.Uint64()
		}
		rss := h1DiagnosticSelfRSS()
		if rss == 0 {
			return h1MemoryValues{}, errors.New("self-rss-unavailable")
		}
		return h1MemoryValues{values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9], rss}, nil
	}
}

type h1MemorySample struct {
	Offset            time.Duration `json:"offset_nanoseconds"`
	ScheduledOffset   time.Duration `json:"scheduled_offset_nanoseconds"`
	Lateness          time.Duration `json:"lateness_nanoseconds"`
	ReadDuration      time.Duration `json:"read_duration_nanoseconds"`
	Phase             string        `json:"phase"`
	CrossedPhase      bool          `json:"crossed_phase"`
	Periodic          bool          `json:"periodic"`
	HeapAndStackInuse uint64        `json:"heap_and_stack_inuse_bytes"`
	h1MemoryValues
}

type h1MemoryPhaseEvent struct {
	Offset time.Duration `json:"offset_nanoseconds"`
	Phase  string        `json:"phase"`
}

type h1MemoryReading struct {
	Enabled             bool                 `json:"enabled"`
	Valid               bool                 `json:"valid"`
	Failure             string               `json:"failure,omitempty"`
	Interval            time.Duration        `json:"interval_nanoseconds"`
	MaxAllowedLateness  time.Duration        `json:"max_allowed_lateness_nanoseconds"`
	SampleLimit         int                  `json:"sample_limit"`
	SampleOverflow      uint64               `json:"sample_overflow"`
	PhaseOverflow       uint64               `json:"phase_overflow"`
	MissedTicks         uint64               `json:"missed_ticks"`
	LateSamples         uint64               `json:"late_samples"`
	SlowReads           uint64               `json:"slow_reads"`
	ReadErrors          uint64               `json:"read_errors"`
	InvalidPhases       uint64               `json:"invalid_phases"`
	LoadSamples         int                  `json:"load_samples"`
	SamplerStorageBytes int                  `json:"sampler_storage_bytes"`
	Phases              []h1MemoryPhaseEvent `json:"phases"`
	Samples             []h1MemorySample     `json:"samples"`
}

type h1MemorySampler struct {
	mu           sync.Mutex
	start        time.Time
	phase        h1MemoryPhase
	lastPeriodic time.Time
	stopping     bool
	reading      h1MemoryReading
	read         func() (h1MemoryValues, error)
	stopOnce     sync.Once
	stopChan     chan struct{}
	done         chan struct{}
	started      chan struct{}
}

func newH1MemorySampler(read func() (h1MemoryValues, error)) *h1MemorySampler {
	s := &h1MemorySampler{
		start: time.Now(), phase: h1MemorySetup, read: read,
		stopChan: make(chan struct{}), done: make(chan struct{}), started: make(chan struct{}),
		reading: h1MemoryReading{Enabled: true, Interval: h1MemoryInterval, MaxAllowedLateness: h1MemoryMaxLateness, SampleLimit: h1MemorySampleLimit,
			Samples: make([]h1MemorySample, 0, h1MemorySampleLimit), Phases: make([]h1MemoryPhaseEvent, 0, h1MemoryPhaseLimit)},
	}
	s.reading.Phases = append(s.reading.Phases, h1MemoryPhaseEvent{0, h1MemorySetup.name()})
	s.reading.SamplerStorageBytes = h1MemorySampleLimit*int(unsafe.Sizeof(h1MemorySample{})) + h1MemoryPhaseLimit*int(unsafe.Sizeof(h1MemoryPhaseEvent{}))
	go s.run()
	<-s.started
	return s
}

func (s *h1MemorySampler) mark(phase h1MemoryPhase) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	if !phase.valid() || phase < s.phase {
		s.reading.InvalidPhases++
		return
	}
	if phase == s.phase {
		return
	}
	s.phase = phase
	if len(s.reading.Phases) == cap(s.reading.Phases) {
		s.reading.PhaseOverflow++
		return
	}
	s.reading.Phases = append(s.reading.Phases, h1MemoryPhaseEvent{time.Since(s.start), phase.name()})
}

func (s *h1MemorySampler) collect(due time.Time, periodic bool) {
	begin := time.Now()
	s.mu.Lock()
	phase := s.phase
	s.mu.Unlock()
	values, err := s.read()
	end := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.reading.ReadErrors++
		return
	}
	if periodic {
		if !s.lastPeriodic.IsZero() {
			s.reading.MissedTicks += h1MemoryMissedTicks(due.Sub(s.lastPeriodic))
		}
		s.lastPeriodic = due
	}
	lag := max(0, begin.Sub(due))
	if lag > h1MemoryMaxLateness {
		s.reading.LateSamples++
	}
	if end.Sub(begin) > h1MemoryInterval {
		s.reading.SlowReads++
	}
	if len(s.reading.Samples) == cap(s.reading.Samples) {
		s.reading.SampleOverflow++
		return
	}
	crossed := phase != s.phase
	s.reading.Samples = append(s.reading.Samples, h1MemorySample{begin.Sub(s.start), due.Sub(s.start), lag, end.Sub(begin), phase.name(), crossed, periodic, values.heapAndStackInuse(), values})
	if periodic && !crossed && phase == h1MemoryLoad {
		s.reading.LoadSamples++
	}
}

// Protocol shared with the Go diagnostic consumer: round scheduled spacing to
// the nearest nominal tick, with a half-interval tie assigned upward. Ticker
// timestamps have sub-tick jitter; 10ms+1ns is not a missing tick, but 20ms-1ns
// still is. This does not change the independent 20ms lateness limit.
func h1MemoryMissedTicks(delta time.Duration) uint64 {
	if delta <= 0 {
		return 1
	}
	return uint64(max(0, (delta+h1MemoryInterval/2)/h1MemoryInterval-1))
}

func (s *h1MemorySampler) run() {
	ticker := time.NewTicker(h1MemoryInterval)
	defer ticker.Stop()
	defer close(s.done)
	s.collect(s.start, false)
	close(s.started)
	for {
		select {
		case <-s.stopChan:
			s.collect(time.Now(), false)
			return
		case due := <-ticker.C:
			s.collect(due, true)
		}
	}
}

func h1MemoryFailure(r h1MemoryReading) string {
	switch {
	case !r.Enabled:
		return "sampler-disabled"
	case r.SampleOverflow != 0 || r.PhaseOverflow != 0:
		return "sampler-overflow"
	case r.MissedTicks != 0 || r.LateSamples != 0 || r.SlowReads != 0:
		return "sampling-lateness"
	case r.ReadErrors != 0:
		return "sampling-read-error"
	case r.InvalidPhases != 0 || len(r.Phases) != int(h1MemoryEnd):
		return "sampling-phase-ownership"
	case r.LoadSamples < h1MemoryMinimumLoadSamples:
		return "insufficient-load-samples"
	}
	for i, phase := range r.Phases {
		if phase.Phase != h1MemoryPhase(i+1).name() || phase.Offset < 0 || i > 0 && phase.Offset < r.Phases[i-1].Offset {
			return "sampling-phase-order"
		}
	}
	return ""
}

func (s *h1MemorySampler) stop() h1MemoryReading {
	if s == nil {
		return h1MemoryReading{Failure: "sampler-disabled"}
	}
	s.stopOnce.Do(func() {
		s.mark(h1MemoryEnd)
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		close(s.stopChan)
	})
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.reading
	r.Failure = h1MemoryFailure(r)
	r.Valid = r.Failure == ""
	return r
}
