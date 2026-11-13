// Failure observations preserve real entrypoint outcomes, cancellation and panic
// ownership while exposing only fixed classes and per-call elapsed residence.
package model

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Formatting the synthetic value would leak; classifiers must only inspect types.
type providerPickerPrivateError struct{}

func (self providerPickerPrivateError) Error() string { panic("private error formatted") }

// A custom timeout method may panic; observation must remain unknown without
// formatting the error or replacing the owning request's result.
type providerPickerPanicTimeoutError struct{}

func (self providerPickerPanicTimeoutError) Error() string { panic("private timeout formatted") }
func (self providerPickerPanicTimeoutError) Timeout() bool {
	panic("synthetic timeout inspection refusal")
}
func (self providerPickerPanicTimeoutError) Temporary() bool { return false }

// A cycle is unavailable cause authority, never a healthy cancellation.
type providerPickerCycleError struct{}

func (self *providerPickerCycleError) Error() string { panic("private cycle formatted") }
func (self *providerPickerCycleError) Unwrap() error { return self }

func TestProviderPickerFailureCauseFiniteAndMixed(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline"},
		{providerPickerPrivateError{}, "other"},
		{errors.Join(server.DbContextDoneError, context.Canceled), "canceled"},
		{errors.Join(context.Canceled, providerPickerPrivateError{}), "mixed"},
		{errors.Join(context.DeadlineExceeded, providerPickerPrivateError{}), "mixed"},
		{&providerPickerCycleError{}, "unknown"}, {nil, "unknown"},
		{providerPickerPanicTimeoutError{}, "unknown"},
		{&net.DNSError{IsTimeout: true}, "network-timeout"},
		{errors.Join(context.Canceled, &net.DNSError{IsTimeout: true}), "mixed"},
	} {
		if got := providerPickerFailureCause(test.err); got != test.want {
			t.Fatalf("finite cause got=%s want=%s", got, test.want)
		}
	}
}

func TestProviderPickerFailureClockPanicAndObserverFailure(t *testing.T) {
	for _, panicObserver := range []bool{false, true} {
		metrics := newProviderPickerMetricSet()
		now := time.Unix(100, 0)
		metrics.now = func() time.Time { return now }
		var events []providerPickerFailure
		metrics.failure = func(event providerPickerFailure) {
			events = append(events, event)
			if panicObserver {
				panic("synthetic observer refusal")
			}
		}
		observation := &providerPickerObservation{metrics: metrics, surface: "initial"}
		value := &struct{}{}
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			var returnErr error
			defer observation.finishInitial(context.Background(), &returnErr)
			observation.enter("caller_location")
			now = now.Add(2 * time.Second)
			observation.enter("initial_cache")
			now = now.Add(3 * time.Second)
			panic(value)
		}()
		if recovered != value || len(events) != 1 {
			t.Fatal("observer lost/replaced panic or terminal event")
		}
		event := events[0]
		if event.phase != "initial_cache" || event.kind != "panic" || event.cause != "unknown" || event.caller != "live" || event.total != 5*time.Second || event.residence != 3*time.Second || !event.completed.Equal(now) {
			t.Fatal("failure clock or fixed partition changed")
		}
		if testutil.ToFloat64(metrics.phases["initial/initial_cache"].inflight) != 0 {
			t.Fatal("panic left phase inflight")
		}
	}
}

func TestProviderPickerFailureReturnAndHealthyControl(t *testing.T) {
	for _, failed := range []bool{false, true} {
		metrics := newProviderPickerMetricSet()
		now := time.Unix(100, 0)
		metrics.now = func() time.Time { return now }
		var events []providerPickerFailure
		metrics.failure = func(event providerPickerFailure) { events = append(events, event) }
		original := error(providerPickerPrivateError{})
		returned := func() (returnErr error) {
			observation := &providerPickerObservation{metrics: metrics, surface: "initial"}
			defer observation.finishInitial(context.Background(), &returnErr)
			observation.enter("initial_cache")
			now = now.Add(time.Second)
			observation.enter("filters")
			now = now.Add(2 * time.Second)
			if failed {
				return original
			}
			return nil
		}()
		if failed {
			if returned != original || len(events) != 1 || events[0].phase != "filters" || events[0].cause != "other" || events[0].kind != "returned" || events[0].total != 3*time.Second || events[0].residence != 2*time.Second {
				t.Fatal("returned failure lost its phase or identity")
			}
		} else if returned != nil || len(events) != 0 {
			t.Fatal("healthy call emitted failure")
		}
	}
}

func TestProviderPickerFailureActualPreCallbackCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "valid")
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		beforeRead := testutil.ToFloat64(providerPickerMetrics.readInitial) + testutil.ToFloat64(providerPickerMetrics.readFilters)
		beforeError := testutil.ToFloat64(providerPickerMetrics.children["initial/error"])
		previous := providerPickerMetrics.failure
		defer func() { providerPickerMetrics.failure = previous }()
		var events []providerPickerFailure
		providerPickerMetrics.failure = func(event providerPickerFailure) { events = append(events, event) }
		var raised any
		func() {
			defer func() { raised = recover() }()
			GetProviderLocations(&session.ClientSession{Ctx: canceled})
		}()
		original, ok := raised.(error)
		if !ok || !errors.Is(original, context.Canceled) {
			t.Fatal("real pre-callback stop lost original panic")
		}
		if len(events) != 1 || events[0].phase != "initial_cache" || events[0].kind != "panic" || events[0].cause != "canceled" || events[0].caller != "canceled" {
			t.Fatal("real pre-callback stop has no finite failure event")
		}
		if testutil.ToFloat64(providerPickerMetrics.readInitial)+testutil.ToFloat64(providerPickerMetrics.readFilters) != beforeRead || testutil.ToFloat64(providerPickerMetrics.children["initial/error"])-beforeError != 1 {
			t.Fatal("instrumentation changed real read/outcome counters")
		}
	})
}

func TestProviderPickerFailureActualFilterCancellationAndSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		previous := providerPickerMetrics.failure
		defer func() { providerPickerMetrics.failure = previous }()
		var stateLock sync.Mutex
		var events []providerPickerFailure
		providerPickerMetrics.failure = func(event providerPickerFailure) {
			stateLock.Lock()
			defer stateLock.Unlock()
			events = append(events, event)
		}
		beforeRead := testutil.ToFloat64(providerPickerMetrics.readFilters)
		beforeError := testutil.ToFloat64(providerPickerMetrics.children["initial/error"])
		providerPickerPhaseBlockedBatch(t, true)
		stateLock.Lock()
		if len(events) != 16 {
			t.Fatal("actual canceled filter calls lost failure observations")
		}
		for _, event := range events {
			if event.phase != "filters" || event.kind != "returned" || event.cause != "canceled" || event.caller != "canceled" {
				t.Fatal("actual filter cancellation misclassified")
			}
		}
		stateLock.Unlock()
		if testutil.ToFloat64(providerPickerMetrics.readFilters)-beforeRead != 16 || testutil.ToFloat64(providerPickerMetrics.children["initial/error"])-beforeError != 16 {
			t.Fatal("instrumentation changed filter read/outcome counts")
		}
		providerPickerPhaseBlockedBatch(t, false)
		stateLock.Lock()
		defer stateLock.Unlock()
		if len(events) != 16 {
			t.Fatal("real successful filter completion emitted failure")
		}
	})
}

func TestProviderPickerFailureActualReturnedReadError(t *testing.T) {
	for _, mode := range []string{"initial-failure", "filter-failure"} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx, _, _, expected := providerPickerReadFixture(t, mode)
			beforeError := testutil.ToFloat64(providerPickerMetrics.children["initial/error"])
			initial := testutil.ToFloat64(providerPickerMetrics.readInitial)
			filters := testutil.ToFloat64(providerPickerMetrics.readFilters)
			previous := providerPickerMetrics.failure
			defer func() { providerPickerMetrics.failure = previous }()
			var events []providerPickerFailure
			providerPickerMetrics.failure = func(event providerPickerFailure) { events = append(events, event); panic("synthetic observer refusal") }
			result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
			phase := "initial_cache"
			if mode == "filter-failure" {
				phase = "filters"
			}
			if result != nil || !errors.Is(err, expected) || len(events) != 1 || events[0].phase != phase || events[0].kind != "returned" || events[0].cause != "other" || events[0].caller != "live" {
				t.Fatal("real read failure changed return or lost finite event")
			}
			if testutil.ToFloat64(providerPickerMetrics.children["initial/error"])-beforeError != 1 || testutil.ToFloat64(providerPickerMetrics.readInitial)+testutil.ToFloat64(providerPickerMetrics.readFilters)-initial-filters != 1 {
				t.Fatal("real returned failure counter semantics changed")
			}
		})
	}
}

func TestProviderPickerFailureChildDeadlineAndMixedCause(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		metrics := newProviderPickerMetricSet()
		var events []providerPickerFailure
		metrics.failure = func(event providerPickerFailure) { events = append(events, event) }
		original := error(context.DeadlineExceeded)
		if mixed {
			original = errors.Join(context.DeadlineExceeded, providerPickerPrivateError{})
		}
		func() (returnErr error) {
			observation := &providerPickerObservation{metrics: metrics, surface: "initial"}
			defer observation.finishInitial(context.Background(), &returnErr)
			observation.enter("filters")
			return original
		}()
		want := "deadline"
		if mixed {
			want = "mixed"
		}
		if len(events) != 1 || events[0].caller != "live" || events[0].cause != want {
			t.Fatal("child deadline or independent dependency failure became caller cancellation")
		}
	}
}

// Malformed payloads are failures, with no payload/error text retained in events.
func TestProviderPickerFailureActualDecodeFailure(t *testing.T) {
	for _, phase := range []string{"initial_cache", "filters"} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx, locationId, callerId, _ := providerPickerReadFixture(t, "valid")
			key := initialClientLocationsKey()
			if phase == "filters" {
				key = clientScoreLocationFilterKey(false, RankModeQuality, locationId, callerId)
			}
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, key, []byte{255, 254, 253}, time.Minute).Err())
			})
			previous := providerPickerMetrics.failure
			defer func() { providerPickerMetrics.failure = previous }()
			var events []providerPickerFailure
			providerPickerMetrics.failure = func(event providerPickerFailure) { events = append(events, event) }
			before := testutil.ToFloat64(providerPickerMetrics.children["initial/error"])
			result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
			if result != nil || err == nil || len(events) != 1 || events[0].phase != phase || events[0].cause != "other" || events[0].kind != "returned" || testutil.ToFloat64(providerPickerMetrics.children["initial/error"])-before != 1 {
				t.Fatal("decode failure became success or lost its phase/counter")
			}
		})
	}
}

// A panic from a custom timeout method cannot escape or erase the event.
func TestProviderPickerFailurePanickingTimeoutMethod(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	var events []providerPickerFailure
	metrics.failure = func(event providerPickerFailure) { events = append(events, event) }
	original := error(providerPickerPanicTimeoutError{})
	returned := func() (returnErr error) {
		observation := &providerPickerObservation{metrics: metrics, surface: "initial"}
		defer observation.finishInitial(context.Background(), &returnErr)
		observation.enter("filters")
		return original
	}()
	if returned != original || len(events) != 1 || events[0].cause != "unknown" || events[0].kind != "returned" || events[0].caller != "live" {
		t.Fatal("panicking timeout method replaced return or lost unknown event")
	}
}
