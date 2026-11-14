// Fixed phase observations preserve missing/error semantics and expose blocked
// picker reads without attaching request or location identities to metrics.
package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Gather only the independent fixed phase family used by this control.
func providerPickerPhaseSummary(t testing.TB, metrics *providerPickerMetricSet, surface string, phase string) (uint64, float64) {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.phaseSeconds)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if len(labels) != 2 {
				t.Fatal("phase metric acquired an unbounded label")
			}
			if labels["surface"] == surface && labels["phase"] == phase {
				return metric.GetSummary().GetSampleCount(), metric.GetSummary().GetSampleSum()
			}
		}
	}
	t.Fatal("fixed phase missing")
	return 0, 0
}

func TestProviderPickerPhaseExclusiveClockAndIdempotentFinish(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	now := time.Unix(100, 0)
	metrics.now = func() time.Time { return now }
	observation := &providerPickerObservation{metrics: metrics, surface: "initial"}
	observation.enter("caller_location")
	now = now.Add(2 * time.Second)
	observation.enter("initial_cache")
	if testutil.ToFloat64(metrics.phases["initial/caller_location"].inflight) != 0 || testutil.ToFloat64(metrics.phases["initial/initial_cache"].inflight) != 1 {
		t.Fatal("one request occupied overlapping phases")
	}
	now = now.Add(3 * time.Second)
	observation.finish()
	observation.finish()
	for _, expected := range []struct {
		phase   string
		seconds float64
	}{{phase: "caller_location", seconds: 2}, {phase: "initial_cache", seconds: 3}} {
		count, seconds := providerPickerPhaseSummary(t, metrics, "initial", expected.phase)
		if count != 1 || seconds != expected.seconds || testutil.ToFloat64(metrics.phases["initial/"+expected.phase].inflight) != 0 {
			t.Fatal("phase duration doubled or gauge retained after completion")
		}
	}
}

func TestProviderPickerPhasePanicAndUnknownRemainBounded(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("synthetic panic did not propagate through observation")
			}
		}()
		observation := &providerPickerObservation{metrics: metrics, surface: "search"}
		defer observation.finish()
		observation.enter("filters")
		observation.enter("synthetic-unrecognized-phase")
		panic("synthetic picker unwind")
	}()
	count, _ := providerPickerPhaseSummary(t, metrics, "search", "filters")
	if count != 1 || testutil.ToFloat64(metrics.phases["search/filters"].inflight) != 0 || len(metrics.phases) != 18 {
		t.Fatal("panic retained inflight or unknown phase created a metric child")
	}
}

type providerPickerPhaseContextKey struct{}

// The test-owned Redis hook blocks only explicitly marked picker pipelines.
// It never changes shared production clients outside the attested TestEnv.
type providerPickerPhaseDelayHook struct {
	filterKey string
	entered   chan struct{}
	release   chan struct{}
}

func (self *providerPickerPhaseDelayHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *providerPickerPhaseDelayHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *providerPickerPhaseDelayHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		filterRead := false
		for _, command := range commands {
			args := command.Args()
			if command.Name() == "get" && len(args) == 2 && args[1] == self.filterKey {
				filterRead = true
			}
		}
		// Automatic pipelining can route the wrapper PING and initial GET
		// through this hook too. Only the owned filter read is the barrier.
		if ctx.Value(providerPickerPhaseContextKey{}) != true || !filterRead {
			return next(ctx, commands)
		}
		self.entered <- struct{}{}
		select {
		case <-self.release:
			return next(ctx, commands)
		case <-ctx.Done():
			for _, command := range commands {
				command.SetErr(ctx.Err())
			}
			return ctx.Err()
		}
	}
}

func TestProviderPickerPhaseBarrierMatchesOnlyFilterRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), providerPickerPhaseContextKey{}, true))
	cancel()
	hook := &providerPickerPhaseDelayHook{filterKey: "synthetic-filter-key", entered: make(chan struct{}, 1), release: make(chan struct{})}
	forwarded := 0
	process := hook.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { forwarded++; return nil })
	for _, command := range []redis.Cmder{redis.NewCmd(ctx, "ping"), redis.NewCmd(ctx, "get", "synthetic-initial-key")} {
		if err := process(ctx, []redis.Cmder{command}); err != nil {
			t.Fatal("automatic initial/PING pipeline was mistaken for filter residence")
		}
	}
	filter := redis.NewCmd(ctx, "get", hook.filterKey)
	if err := process(ctx, []redis.Cmder{filter}); !errors.Is(err, context.Canceled) || !errors.Is(filter.Err(), context.Canceled) || forwarded != 2 {
		t.Fatal("exact filter barrier failed its canceled-read boundary")
	}
	select {
	case <-hook.entered:
	default:
		t.Fatal("exact filter read did not enter the barrier")
	}
}

// Explicit barriers prove where all requests reside; the timeout only bounds
// a broken test. No short sleep or scheduling poll supplies the assertion.
func providerPickerPhaseBlockedBatch(t testing.TB, cancelReads bool) {
	t.Helper()
	ctx, locationId, callerId, _ := providerPickerReadFixture(t, "valid")
	guardCtx, guardCancel := context.WithTimeout(ctx, 15*time.Second)
	defer guardCancel()
	const requests = 16
	hook := &providerPickerPhaseDelayHook{filterKey: clientScoreLocationFilterKey(false, RankModeQuality, locationId, callerId), entered: make(chan struct{}, requests), release: make(chan struct{})}
	server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
	readCtx, cancel := context.WithCancel(context.WithValue(guardCtx, providerPickerPhaseContextKey{}, true))
	defer cancel()
	beforeCount, _ := providerPickerPhaseSummary(t, providerPickerMetrics, "initial", "filters")
	completed := make(chan error, requests)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	for range requests {
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				if recover() != nil {
					completed <- errors.New("synthetic picker worker panicked")
				}
			}()
			result, err := GetProviderLocations(&session.ClientSession{Ctx: readCtx})
			if !cancelReads && (err != nil || result == nil || result.CountryCount != 1) {
				completed <- errors.New("synthetic valid country lost after release")
			} else {
				completed <- err
			}
		}()
	}
	for range requests {
		select {
		case <-hook.entered:
		case <-guardCtx.Done():
			t.Fatal("picker did not reach explicit filter barrier")
		}
	}
	filterInflight := testutil.ToFloat64(providerPickerMetrics.phases["initial/filters"].inflight)
	initialInflight := testutil.ToFloat64(providerPickerMetrics.phases["initial/initial_cache"].inflight)
	if filterInflight != requests || initialInflight != 0 {
		t.Fatalf("blocked picker phase counts: filters=%g initial=%g expected_filters=%d", filterInflight, initialInflight, requests)
	}
	if cancelReads {
		cancel()
	} else {
		close(hook.release)
	}
	for range requests {
		select {
		case err := <-completed:
			if (cancelReads && !errors.Is(err, context.Canceled)) || (!cancelReads && err != nil) {
				t.Fatal("picker cancellation or valid result changed")
			}
		case <-guardCtx.Done():
			t.Fatal("picker did not unwind after explicit release")
		}
	}
	afterCount, _ := providerPickerPhaseSummary(t, providerPickerMetrics, "initial", "filters")
	if afterCount-beforeCount != requests || testutil.ToFloat64(providerPickerMetrics.phases["initial/filters"].inflight) != 0 {
		t.Fatal("completed filter observations missing or inflight retained")
	}
}

func TestProviderPickerPhaseActualBlockedReadsCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { providerPickerPhaseBlockedBatch(t, true) })
}

func TestProviderPickerPhaseActualBlockedReadsRelease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { providerPickerPhaseBlockedBatch(t, false) })
}
