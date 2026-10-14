// Package privateheapprofile implements an opt-in, root-only, single-use local
// allocation profile. It never installs a network or default HTTP handler.
package privateheapprofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/metrics"
	"time"
)

const (
	MaxRecords      = 4096
	MaxProfileBytes = 2 << 20
	SampleInterval  = 3 * time.Second
	CaptureBudget   = 10 * time.Second
)

var errProfileBound = errors.New("heap_profile_bound")

// Identity is private process metadata, never a metric label or log field.
type Identity struct {
	PID              int    `json:"pid"`
	StartTicks       uint64 `json:"start_ticks"`
	BootID           string `json:"boot_id"`
	Revision         string `json:"revision"`
	Modified         bool   `json:"modified"`
	ExecutableSHA256 string `json:"executable_sha256"`
	Host             string `json:"host"`
	Block            string `json:"block"`
}

type PoolClass struct {
	Size     int    `json:"size"`
	Capacity int    `json:"capacity"`
	Retained int    `json:"retained"`
	Taken    uint64 `json:"taken"`
	Returned uint64 `json:"returned"`
	Created  uint64 `json:"created"`
}

// Companion contains only fixed-size context; it must not walk residents,
// sockets, queues or frames. Pool counters are accounting, not reachable heap.
type Companion struct {
	Available           bool                          `json:"available"`
	ListenerReady       bool                          `json:"listener_ready"`
	ServingActive       bool                          `json:"serving_active"`
	PoolClassesComplete bool                          `json:"pool_classes_complete"`
	PoolClasses         [4]PoolClass                  `json:"pool_classes"`
	ResidentPayload     *ResidentPayloadSnapshot      `json:"resident_payload,omitempty"`
	SDKPayload          *TransferPayloadOwnerSnapshot `json:"sdk_payload,omitempty"`
}

type ResidentPayloadStage struct {
	Stage              string `json:"stage"`
	Messages           int64  `json:"messages"`
	LogicalBytes       int64  `json:"logical_bytes"`
	BackingByteCharges int64  `json:"backing_byte_charges"`
	AdmittedTotal      uint64 `json:"admitted_total"`
	ReleasedTotal      uint64 `json:"released_total"`
}

type ResidentPayloadSnapshot struct {
	Enabled  bool                    `json:"enabled"`
	Complete bool                    `json:"complete"`
	Stages   [3]ResidentPayloadStage `json:"stages"`
}

type TransferPayloadOwnerGroup struct {
	Owners             int64  `json:"owners"`
	BackingByteCharges int64  `json:"backing_byte_charges"`
	AdmittedTotal      uint64 `json:"admitted_total"`
	ReleasedTotal      uint64 `json:"released_total"`
}

type TransferPayloadOwnerSnapshot struct {
	Enabled  bool                      `json:"enabled"`
	Complete bool                      `json:"complete"`
	Revision uint64                    `json:"revision"`
	SendAck  TransferPayloadOwnerGroup `json:"send_ack"`
	Forward  TransferPayloadOwnerGroup `json:"forward"`
}

type RuntimeContext struct {
	ObservedUTC         time.Time `json:"observed_utc"`
	HeapObjectBytes     uint64    `json:"heap_object_bytes"`
	HeapUnusedBytes     uint64    `json:"heap_unused_bytes"`
	HeapFreeBytes       uint64    `json:"heap_free_bytes"`
	HeapReleasedBytes   uint64    `json:"heap_released_bytes"`
	HeapStackBytes      uint64    `json:"heap_stack_bytes"`
	Goroutines          uint64    `json:"goroutines"`
	GCCycles            uint64    `json:"gc_cycles"`
	ForcedGCCycles      uint64    `json:"forced_gc_cycles"`
	ProcessUserMicros   int64     `json:"process_user_micros"`
	ProcessSystemMicros int64     `json:"process_system_micros"`
	Companion           Companion `json:"companion"`
}

type Capture struct {
	Profile                []byte         `json:"-"`
	Records                int            `json:"records"`
	Rate                   int            `json:"sampling_rate"`
	StackPCBound           int            `json:"stack_pc_bound"`
	ZeroInUseSitesIncluded bool           `json:"zero_inuse_sites_included"`
	ProfileSHA256          string         `json:"profile_sha256"`
	ProfileBytes           int            `json:"profile_bytes"`
	Before                 RuntimeContext `json:"before"`
	After                  RuntimeContext `json:"after"`
	IntervalNS             int64          `json:"interval_ns"`
	CPUCoreMean            float64        `json:"cpu_core_mean"`
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxProfileBytes-b.Len() {
		return 0, errProfileBound
	}
	return b.Buffer.Write(p)
}

// heapText is the standard Go legacy heap profile format understood by pprof.
// It uses one fixed record buffer and never retries when the profile grows.
// Zero-in-use historical sites are excluded, so alloc_space is not a complete
// historical allocation census. The runtime snapshot may lag by two GC cycles.
// No GC or heap traversal is
// requested. The initial runtime count/copy calls are not interruptible; the
// deadline is checked around them and an over-budget result is not published.
func heapText(ctx context.Context, memProfile func([]runtime.MemProfileRecord, bool) (int, bool), rate int) ([]byte, int, error) {
	if rate <= 0 || int64(rate) > math.MaxInt64/2 || ctx.Err() != nil {
		return nil, 0, errors.New("heap_sampling_unavailable")
	}
	n, _ := memProfile(nil, false)
	if n < 0 || n > MaxRecords || ctx.Err() != nil {
		return nil, 0, errProfileBound
	}
	records := make([]runtime.MemProfileRecord, MaxRecords)
	n, ok := memProfile(records, false)
	if !ok || n < 0 || n > MaxRecords || ctx.Err() != nil {
		return nil, 0, errProfileBound
	}
	records = records[:n]
	var allocObjects, allocBytes, inuseObjects, inuseBytes int64
	for _, r := range records {
		if r.AllocBytes < r.FreeBytes || r.AllocObjects < r.FreeObjects || r.FreeBytes < 0 || r.FreeObjects < 0 {
			return nil, 0, errors.New("heap_sample_invalid")
		}
		if r.AllocObjects > math.MaxInt64-allocObjects || r.AllocBytes > math.MaxInt64-allocBytes || r.InUseObjects() > math.MaxInt64-inuseObjects || r.InUseBytes() > math.MaxInt64-inuseBytes {
			return nil, 0, errors.New("heap_sample_overflow")
		}
		allocObjects += r.AllocObjects
		allocBytes += r.AllocBytes
		inuseObjects += r.InUseObjects()
		inuseBytes += r.InUseBytes()
	}
	// Go's legacy writer uses a distinct header allocation total so pprof
	// recognizes the four-column format. Per-record counters stay exact.
	if allocBytes == inuseBytes {
		if allocBytes == math.MaxInt64 {
			return nil, 0, errors.New("heap_sample_overflow")
		}
		allocBytes++
	}
	var out boundedBuffer
	write := func(format string, args ...any) error { _, err := fmt.Fprintf(&out, format, args...); return err }
	if err := write("heap profile: %d: %d [%d: %d] @ heap/%d\n", inuseObjects, inuseBytes, allocObjects, allocBytes, 2*int64(rate)); err != nil {
		return nil, 0, err
	}
	for _, r := range records {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if err := write("%d: %d [%d: %d] @", r.InUseObjects(), r.InUseBytes(), r.AllocObjects, r.AllocBytes); err != nil {
			return nil, 0, err
		}
		for _, pc := range r.Stack() {
			if err := write(" %#x", pc); err != nil {
				return nil, 0, err
			}
		}
		if err := write("\n"); err != nil {
			return nil, 0, err
		}
		frames := runtime.CallersFrames(r.Stack())
		for frameIndex := 0; frameIndex < 64; frameIndex++ {
			frame, more := frames.Next()
			if frame.Function != "" {
				if err := write("#\t%#x\t%s+%#x\t%s:%d\n", frame.PC, frame.Function, frame.PC-frame.Entry, frame.File, frame.Line); err != nil {
					return nil, 0, err
				}
			}
			if !more {
				break
			}
		}
		if err := write("\n"); err != nil {
			return nil, 0, err
		}
	}
	return out.Bytes(), n, nil
}

func runtimeContext(companion func() Companion) (RuntimeContext, error) {
	names := [...]string{"/memory/classes/heap/objects:bytes", "/memory/classes/heap/unused:bytes", "/memory/classes/heap/free:bytes", "/memory/classes/heap/released:bytes", "/memory/classes/heap/stacks:bytes", "/sched/goroutines:goroutines", "/gc/cycles/total:gc-cycles", "/gc/cycles/forced:gc-cycles"}
	var samples [len(names)]metrics.Sample
	for i, name := range names {
		samples[i].Name = name
	}
	metrics.Read(samples[:])
	var values [len(names)]uint64
	for i := range samples {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return RuntimeContext{}, errors.New("runtime_context_unavailable")
		}
		values[i] = samples[i].Value.Uint64()
	}
	user, system, err := processCPU()
	if err != nil {
		return RuntimeContext{}, err
	}
	r := RuntimeContext{ObservedUTC: time.Now().UTC(), HeapObjectBytes: values[0], HeapUnusedBytes: values[1], HeapFreeBytes: values[2], HeapReleasedBytes: values[3], HeapStackBytes: values[4], Goroutines: values[5], GCCycles: values[6], ForcedGCCycles: values[7], ProcessUserMicros: user, ProcessSystemMicros: system}
	if companion != nil {
		r.Companion = companion()
	}
	return r, nil
}

func collect(ctx context.Context, companion func() Companion) (Capture, error) {
	start := time.Now()
	before, err := runtimeContext(companion)
	if err != nil {
		return Capture{}, err
	}
	rate := runtime.MemProfileRate
	profile, records, err := heapText(ctx, runtime.MemProfile, rate)
	if err != nil {
		return Capture{}, err
	}
	if remaining := SampleInterval - time.Since(start); remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Capture{}, ctx.Err()
		case <-timer.C:
		}
	}
	after, err := runtimeContext(companion)
	if err != nil {
		return Capture{}, err
	}
	interval := time.Since(start)
	if ctx.Err() != nil || interval > CaptureBudget || rate != runtime.MemProfileRate {
		return Capture{}, errors.New("capture_context_changed")
	}
	cpu := (after.ProcessUserMicros - before.ProcessUserMicros) + (after.ProcessSystemMicros - before.ProcessSystemMicros)
	if cpu < 0 {
		return Capture{}, errors.New("cpu_counter_changed")
	}
	digest := sha256.Sum256(profile)
	return Capture{Profile: profile, Records: records, Rate: rate, StackPCBound: 32, ProfileSHA256: hex.EncodeToString(digest[:]), ProfileBytes: len(profile), Before: before, After: after, IntervalNS: interval.Nanoseconds(), CPUCoreMean: float64(cpu) / 1e6 / interval.Seconds()}, nil
}

var _ io.Writer = (*boundedBuffer)(nil)
