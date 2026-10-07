package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Production construction captures one independent parent budget, while the
// explicit final-only setting removes that capability altogether.
func TestResidentContractFallbackExchangeConstructionAndDisable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		settings := DefaultExchangeSettings()
		if !settings.ContractHoleCompatibilityFallback {
			t.Fatal("transition defaults lost their explicit compatibility mode")
		}
		settings.KeyEventDelivery.Enabled = false
		first := NewExchange(ctx, "synthetic", "connect", "synthetic", nil, nil, settings)
		second := NewExchange(ctx, "synthetic", "connect", "synthetic", nil, nil, settings)
		defer first.Close()
		defer second.Close()
		if first.contractHoleFallback == nil || second.contractHoleFallback == nil || first.contractHoleFallback == second.contractHoleFallback || cap(first.contractHoleFallback.slots) != residentContractFallbackMaxConcurrent {
			t.Fatal("exchange did not capture its independent bounded source capability")
		}
		settings.ContractHoleCompatibilityFallback = false
		finalOnly := NewExchange(ctx, "synthetic", "connect", "synthetic", nil, nil, settings)
		defer finalOnly.Close()
		if finalOnly.contractHoleFallback != nil {
			t.Fatal("disabled compatibility mode retained a source capability")
		}
	})
}

// Many different resident owners share one exchange budget without a queue.
// A full budget does not consume a cached permission or another exchange's cap.
func TestResidentContractFallbackSharedCapDropsWithoutWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fallback := newResidentContractFallback()
		started := make(chan struct{}, residentContractFallbackMaxConcurrent)
		release := make(chan struct{})
		var calls atomic.Int32
		fallback.readContract = residentContractSourceForTest(func(context.Context, server.Id, server.Id) (bool, error) {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return true, nil
		})
		var managers []*residentContractManager
		results := make(chan bool, residentContractFallbackMaxConcurrent)
		for range residentContractFallbackMaxConcurrent {
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			manager := newResidentContractManagerWithFallback(ctx, cancel, server.NewId(), DefaultExchangeSettings(), fallback)
			manager.readContract = func(_ context.Context, source, destination server.Id) residentContractAllowance {
				return fallback.check(ctx, source, destination)
			}
			managers = append(managers, manager)
			go func() { results <- manager.HasActiveContract(manager.clientId, server.NewId()) }()
		}
		for range residentContractFallbackMaxConcurrent {
			<-started
		}
		for range 128 {
			if fallback.check(t.Context(), server.NewId(), server.NewId()).active {
				t.Fatal("saturated fallback admitted or retained work")
			}
		}
		if calls.Load() != residentContractFallbackMaxConcurrent || len(fallback.slots) != residentContractFallbackMaxConcurrent {
			t.Fatal("exchange fallback exceeded its fixed shared capacity")
		}
		manager := managers[0]
		destination := server.NewId()
		manager.stateLock.Lock()
		manager.activeContracts[model.NewUnorderedTransferPair(manager.clientId, destination)] = &activeContractEntry{checkTime: time.Now(), validUntil: time.Now().Add(time.Hour)}
		manager.stateLock.Unlock()
		if !manager.HasActiveContract(manager.clientId, destination) {
			t.Fatal("full source budget refused an already fresh permission")
		}
		independent := newResidentContractFallback()
		independent.readContract = residentContractSourceForTest(func(context.Context, server.Id, server.Id) (bool, error) { return true, nil })
		if !independent.check(t.Context(), server.NewId(), server.NewId()).active {
			t.Fatal("independent exchange contended for a process singleton")
		}
		close(release)
		for range residentContractFallbackMaxConcurrent {
			if !<-results {
				t.Fatal("admitted fallback lost its successful result")
			}
		}
		for _, manager := range managers {
			if err := manager.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		if len(fallback.slots) != 0 {
			t.Fatal("joined checks retained shared admission")
		}
	})
}

// Unknown evidence cannot bypass the existing exact pair interval, even when
// the underlying source starts returning positive between two packets.
func TestResidentContractFallbackRechecksAtExactPairInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fallback := newResidentContractFallback()
		calls, active := 0, false
		fallback.readContract = residentContractSourceForTest(func(context.Context, server.Id, server.Id) (bool, error) { calls++; return active, nil })
		manager := newResidentContractManagerWithFallback(ctx, cancel, server.NewId(), DefaultExchangeSettings(), fallback)
		manager.readContract = func(_ context.Context, source, destination server.Id) residentContractAllowance {
			return fallback.check(ctx, source, destination)
		}
		source, destination := server.NewId(), server.NewId()
		for range 128 {
			if manager.HasActiveContract(source, destination) {
				t.Fatal("negative source authorized a packet")
			}
		}
		active = true
		time.Sleep(time.Second - time.Nanosecond)
		if manager.HasActiveContract(destination, source) || calls != 1 {
			t.Fatal("source rechecked inside the unordered pair interval")
		}
		time.Sleep(time.Nanosecond)
		if !manager.HasActiveContract(destination, source) || calls != 2 {
			t.Fatal("source was not rechecked at the exact non-sliding boundary")
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

// The shorter caller deadline bounds even a source helper with a longer native
// timeout. An error that accompanies a positive boolean cannot authorize.
func TestResidentContractFallbackDeadlineErrorAndPanicReleaseAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fallback := newResidentContractFallback()
		source, destination := server.NewId(), server.NewId()
		fallback.readContract = residentContractSourceForTest(func(ctx context.Context, _, _ server.Id) (bool, error) {
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Sub(time.Now()) != residentContractFallbackTimeout {
				t.Fatal("source did not receive the exact bounded deadline")
			}
			<-ctx.Done()
			return true, nil
		})
		start := time.Now()
		if fallback.check(t.Context(), source, destination).active || time.Since(start) != residentContractFallbackTimeout || len(fallback.slots) != 0 {
			t.Fatal("timed out source authorized or retained capacity")
		}
		fallback.readContract = residentContractSourceForTest(func(context.Context, server.Id, server.Id) (bool, error) {
			return true, errors.New("synthetic source failure")
		})
		if fallback.check(t.Context(), source, destination).active || len(fallback.slots) != 0 {
			t.Fatal("failed source authorized or retained capacity")
		}
		failure := errors.New("synthetic source panic")
		fallback.readContract = residentContractSourceForTest(func(context.Context, server.Id, server.Id) (bool, error) { panic(failure) })
		func() {
			defer func() {
				if recover() != failure {
					t.Fatal("source panic lost its cause")
				}
			}()
			fallback.check(t.Context(), source, destination)
		}()
		if len(fallback.slots) != 0 {
			t.Fatal("panicking source retained admission")
		}
	})
}

// Cancellation closes source admission and the resident joins the admitted call
// rather than abandoning it when PostgreSQL starts unwinding.
func TestResidentContractFallbackCloseJoinsSourceOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fallback := newResidentContractFallback()
		started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		fallback.readContract = residentContractSourceForTest(func(ctx context.Context, _, _ server.Id) (bool, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return true, nil
		})
		manager := newResidentContractManagerWithFallback(ctx, cancel, server.NewId(), DefaultExchangeSettings(), fallback)
		manager.readContract = func(_ context.Context, source, destination server.Id) residentContractAllowance {
			return fallback.check(ctx, source, destination)
		}
		result := make(chan bool, 1)
		go func() { result <- manager.HasActiveContract(server.NewId(), server.NewId()) }()
		<-started
		joined := make(chan error, 1)
		go func() { joined <- manager.CloseAndWait(context.Background()) }()
		<-canceled
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal("resident join passed an admitted source call")
		default:
		}
		close(release)
		if <-result {
			t.Fatal("canceled source published a positive cache entry")
		}
		if err := <-joined; err != nil {
			t.Error(err)
		}
		if len(fallback.slots) != 0 || len(manager.activeContracts) != 0 {
			t.Fatal("closed source owner retained capacity or authorization")
		}
	})
}

// The bridge cannot remove an inherited packet guard. A caller that supplies
// only a packet context has not obtained a general PostgreSQL escape hatch.
func TestResidentContractFallbackPreservesInheritedPostgresGuard(t *testing.T) {
	ctx := server.WithoutPostgres(t.Context())
	fallback := newResidentContractFallback()
	if fallback.check(ctx, server.NewId(), server.NewId()).active || server.PacketPostgresAttempts(ctx) != 1 || len(fallback.slots) != 0 {
		t.Fatal("source fallback bypassed an inherited guard or retained capacity")
	}
}

// Already canceled callers never enter the source, even with spare capacity.
func TestResidentContractFallbackCanceledAdmissionHasNoSource(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fallback := newResidentContractFallback()
	calls := 0
	fallback.readContract = residentContractSourceForTest(func(context.Context, server.Id, server.Id) (bool, error) { calls++; return true, nil })
	if fallback.check(ctx, server.NewId(), server.NewId()).active || calls != 0 || len(fallback.slots) != 0 {
		t.Fatal("canceled caller entered the source or retained admission")
	}
}

// Zero-valued counters are predeclared and no observation can add a new label.
func TestResidentContractFallbackMetricsHaveFiniteOutcomes(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newResidentContractAllowanceMetrics(registry)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 || len(families[0].Metric) != int(contractAllowanceCount) {
		t.Fatalf("missing predeclared metrics: families=%d error=%v", len(families), err)
	}
	for outcome := contractAllowanceOutcome(0); outcome < contractAllowanceCount; outcome++ {
		metrics.add(outcome)
	}
	families, err = registry.Gather()
	if err != nil || len(families[0].Metric) != int(contractAllowanceCount) {
		t.Fatal("allowance observations changed metric cardinality")
	}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "outcome" || metric.GetCounter().GetValue() != 1 {
			t.Fatal("allowance metrics included an unbounded label or lost an outcome")
		}
	}
}
