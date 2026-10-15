//go:build acklineagetrace && h1memorytrace && darwin && cgo

package perfvar

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func h1MemoryFakeRead() (h1MemoryValues, error) {
	return h1MemoryValues{HeapObjects: 10, HeapUnused: 20, HeapFree: 30, HeapReleased: 40, HeapStacks: 50, OSStacks: 60, Total: 210, RSS: 500}, nil
}

func h1MemoryCompletePhases(s *h1MemorySampler) {
	for phase := h1MemoryRouteConstruction; phase < h1MemoryEnd; phase++ {
		s.mark(phase)
		if phase == h1MemoryLoad {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

func TestH1MemorySamplerCompleteAndJoined(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newH1MemorySampler(h1MemoryFakeRead)
		h1MemoryCompletePhases(s)
		r := s.stop()
		if !r.Valid || r.LoadSamples < 20 || r.SamplerStorageBytes <= 0 {
			t.Fatalf("invalid complete sampler: %+v", r)
		}
		for _, sample := range r.Samples {
			if sample.HeapAndStackInuse != 80 || sample.RSS != 500 {
				t.Fatalf("wrong memory metric mapping: %+v", sample)
			}
		}
		before := len(r.Samples)
		s.mark(h1MemorySetup)
		time.Sleep(time.Second)
		if after := s.stop(); len(after.Samples) != before || after.InvalidPhases != r.InvalidPhases {
			t.Fatal("post-stop callback mutated terminal reading")
		}
	})
}

func TestH1MemorySamplerStopWaitsForOwnedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		s := newH1MemorySampler(func() (h1MemoryValues, error) {
			if calls.Add(1) == 2 {
				close(entered)
				<-release
			}
			return h1MemoryFakeRead()
		})
		<-entered
		joined := make(chan struct{})
		go func() { s.stop(); close(joined) }()
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal("stop returned before in-flight RSS/metrics read joined")
		default:
		}
		close(release)
		<-joined
		before := calls.Load()
		time.Sleep(time.Second)
		if calls.Load() != before {
			t.Fatal("sampler goroutine remained live")
		}
	})
}

func TestH1MemorySamplerNegativeControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newH1MemorySampler(h1MemoryFakeRead)
		h1MemoryCompletePhases(s)
		valid := s.stop()
		cases := []struct {
			name, want string
			change     func(*h1MemoryReading)
		}{
			{"sample-overflow", "sampler-overflow", func(r *h1MemoryReading) { r.SampleOverflow++ }},
			{"phase-overflow", "sampler-overflow", func(r *h1MemoryReading) { r.PhaseOverflow++ }},
			{"missed-tick", "sampling-lateness", func(r *h1MemoryReading) { r.MissedTicks++ }},
			{"late", "sampling-lateness", func(r *h1MemoryReading) { r.LateSamples++ }},
			{"slow-read", "sampling-lateness", func(r *h1MemoryReading) { r.SlowReads++ }},
			{"read-error", "sampling-read-error", func(r *h1MemoryReading) { r.ReadErrors++ }},
			{"phase-invalid", "sampling-phase-ownership", func(r *h1MemoryReading) { r.InvalidPhases++ }},
			{"phase-missing", "sampling-phase-ownership", func(r *h1MemoryReading) { r.Phases = r.Phases[:len(r.Phases)-1] }},
			{"too-few", "insufficient-load-samples", func(r *h1MemoryReading) { r.LoadSamples = 19 }},
		}
		for _, c := range cases {
			r := valid
			c.change(&r)
			if got := h1MemoryFailure(r); got != c.want {
				t.Fatalf("%s accepted/wrong reason=%s", c.name, got)
			}
		}
	})
}

func TestH1MemorySamplerActualBoundsAndLateness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &h1MemorySampler{start: time.Now(), phase: h1MemoryLoad, read: h1MemoryFakeRead, reading: h1MemoryReading{Enabled: true, Samples: make([]h1MemorySample, 0, 1), Phases: make([]h1MemoryPhaseEvent, 0, 1)}}
		s.collect(s.start.Add(-21*time.Millisecond), true)
		s.collect(s.start.Add(9*time.Millisecond), true)
		s.mark(h1MemoryBulkJoin)
		s.mark(h1MemoryPostLoad)
		if s.reading.LateSamples != 1 || s.reading.MissedTicks != 2 || s.reading.SampleOverflow != 1 || s.reading.PhaseOverflow != 1 {
			t.Fatalf("actual bounds lost: %+v", s.reading)
		}
		s.read = func() (h1MemoryValues, error) { time.Sleep(11 * time.Millisecond); return h1MemoryFakeRead() }
		s.collect(time.Now(), false)
		if s.reading.SlowReads != 1 {
			t.Fatal("slow read not invalidated")
		}
		s.read = func() (h1MemoryValues, error) {
			return h1MemoryValues{}, errors.New("private-secret-must-not-be-recorded")
		}
		s.collect(time.Now(), false)
		if s.reading.ReadErrors != 1 {
			t.Fatal("read failure omitted")
		}
		b, err := json.Marshal(s.reading)
		if err != nil || strings.Contains(string(b), "private-secret") {
			t.Fatal("raw error/private data leaked")
		}
	})
}

func TestH1MemorySamplerDisabledAndNativeReaderCost(t *testing.T) {
	var disabled *h1MemorySampler
	if n := testing.AllocsPerRun(1000, func() { disabled.mark(h1MemoryLoad); disabled.stop() }); n != 0 {
		t.Fatalf("disabled allocations=%g", n)
	}
	read := h1MemoryReader()
	if v, err := read(); err != nil || v.RSS == 0 || v.Total == 0 || v.heapAndStackInuse() == 0 {
		t.Fatalf("native/runtime values=%+v err=%v", v, err)
	}
	if n := testing.AllocsPerRun(100, func() {
		if _, err := read(); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("steady per-sample allocation=%g", n)
	}
}

func BenchmarkH1MemorySamplerRead(b *testing.B) {
	read := h1MemoryReader()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := read(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestH1MemorySamplerTickJitterOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			delta  time.Duration
			missed uint64
		}{
			{10*time.Millisecond - 20*time.Microsecond, 0},
			{10*time.Millisecond + 20*time.Microsecond, 0},
			{15*time.Millisecond - time.Nanosecond, 0},
			{15 * time.Millisecond, 1},
			{15*time.Millisecond + time.Nanosecond, 1},
			{20*time.Millisecond - time.Nanosecond, 1},
			{20*time.Millisecond - 20*time.Microsecond, 1},
			{20*time.Millisecond + 20*time.Microsecond, 1},
			{30*time.Millisecond - 20*time.Microsecond, 2},
		} {
			s := &h1MemorySampler{start: time.Now(), phase: h1MemoryLoad, read: h1MemoryFakeRead, reading: h1MemoryReading{Samples: make([]h1MemorySample, 0, 2)}}
			s.collect(s.start, true)
			s.collect(s.start.Add(c.delta), true)
			if s.reading.MissedTicks != c.missed {
				t.Errorf("delta=%s missed=%d want=%d", c.delta, s.reading.MissedTicks, c.missed)
			}
		}
	})
}
