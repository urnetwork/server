package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Constructs the production manager with a deterministic in-process read seam.
func residentReadControl(t *testing.T) (*residentContractManager, context.CancelFunc, server.Id, server.Id) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	manager := newResidentContractManager(ctx, cancel, server.NewId(), DefaultExchangeSettings())
	return manager, cancel, server.NewId(), server.NewId()
}

// Cold synchronous callers own their read until it returns too; shutdown may
// not wait only for the asynchronous positive-refresh branch.
func TestResidentContractReadCloseJoinsColdOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		manager.readContract = residentContractReadForTest(func(ctx context.Context, _, _ server.Id) bool {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release
			return true
		})
		result := make(chan bool, 1)
		go func() { result <- manager.HasActiveContract(source, destination) }()
		<-started
		closed := make(chan error, 1)
		go func() { closed <- manager.CloseAndWait(context.Background()) }()
		<-cancelled
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("close passed an admitted cold reader")
		default:
		}
		close(release)
		if <-result {
			t.Fatal("cancelled cold owner published its result")
		}
		if err := <-closed; err != nil {
			t.Error(err)
		}
		if len(manager.activeReads) != 0 || len(manager.activeContracts) != 0 {
			t.Fatal("joined cold read retained ownership or authorization")
		}
	})
}

// A blocked first check must not turn later packets into waiting work. The
// first positive result remains reusable once the read owner publishes it.
func TestResidentContractReadDropsConcurrentUncheckedPackets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		var calls atomic.Int32
		started, release := make(chan struct{}), make(chan struct{})
		manager.readContract = residentContractReadForTest(func(ctx context.Context, s, d server.Id) bool {
			if ctx != manager.ctx || s != source || d != destination {
				panic("contract read changed its context or pair")
			}
			calls.Add(1)
			close(started)
			<-release
			return true
		})
		first := make(chan bool, 1)
		go func() { first <- manager.HasActiveContract(source, destination) }()
		<-started
		const callers = 32
		results := make(chan bool, callers)
		for range callers {
			go func() { results <- manager.HasActiveContract(source, destination) }()
		}
		synctest.Wait()
		if len(results) != callers || calls.Load() != 1 {
			t.Fatalf("unchecked packets retained: returned=%d reads=%d", len(results), calls.Load())
		}
		for range callers {
			if <-results {
				t.Fatal("in-flight cold check authorized an unchecked packet")
			}
		}
		close(release)
		if !<-first || !manager.HasActiveContract(source, destination) {
			t.Fatal("completed positive check was not reusable")
		}
		if len(manager.activeReads) != 0 || calls.Load() != 1 {
			t.Fatal("completed read retained ownership or duplicated its work")
		}
	})
}

// A new contract becomes visible at the next admitted check. Refused packets
// do not slide the one-second boundary forward, even under a sustained flood.
func TestResidentContractReadLimitsNegativeAndRechecksAtOneSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		calls, active := 0, false
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls++; return active })
		for range 128 {
			if manager.HasActiveContract(source, destination) {
				t.Fatal("missing contract authorized a packet")
			}
		}
		active = true
		time.Sleep(time.Second - time.Nanosecond)
		if manager.HasActiveContract(source, destination) || calls != 1 {
			t.Fatalf("read admitted before one second: %d", calls)
		}
		time.Sleep(time.Nanosecond)
		if !manager.HasActiveContract(source, destination) || calls != 2 {
			t.Fatal("fresh contract was not read at the exact admission boundary")
		}
	})
}

// Either direction consumes the same pair interval; unrelated and self pairs
// retain separate checks rather than sharing an authorization result.
func TestResidentContractReadCanonicalPairIsolation(t *testing.T) {
	manager, _, source, destination := residentReadControl(t)
	other := server.NewId()
	calls := map[model.TransferPair]int{}
	manager.readContract = residentContractReadForTest(func(_ context.Context, s, d server.Id) bool {
		calls[model.NewUnorderedTransferPair(s, d)]++
		return false
	})
	pairs := []model.TransferPair{
		{A: source, B: destination}, {A: destination, B: source},
		{A: source, B: other}, {A: other, B: destination},
		{A: source, B: source}, {A: source, B: source},
	}
	for _, pair := range pairs {
		if manager.HasActiveContract(pair.A, pair.B) {
			t.Fatal("denied pair was accepted")
		}
	}
	if len(calls) != 4 {
		t.Fatalf("pair isolation changed: %d distinct checks", len(calls))
	}
	for _, count := range calls {
		if count != 1 {
			t.Fatal("same unordered pair escaped the interval")
		}
	}
}

// One slow pair cannot serialize an independent pair on manager stateLock.
func TestResidentContractReadOtherPairsRemainParallel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		other := server.NewId()
		started, release := make(chan struct{}), make(chan struct{})
		manager.readContract = residentContractReadForTest(func(_ context.Context, s, _ server.Id) bool {
			if s == source {
				close(started)
				<-release
			}
			return true
		})
		first := make(chan bool, 1)
		go func() { first <- manager.HasActiveContract(source, destination) }()
		<-started
		if !manager.HasActiveContract(other, destination) {
			t.Fatal("independent pair lost its check")
		}
		close(release)
		if !<-first {
			t.Fatal("first pair lost its positive result")
		}
	})
}

// A refresh keeps only the original five-second allowance. An unresolved
// refresh cannot extend it or accumulate blocked packet callers after expiry.
func TestResidentContractReadRefreshPreservesFiveSecondBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		var calls atomic.Int32
		started, release := make(chan struct{}), make(chan struct{})
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool {
			if calls.Add(1) == 1 {
				return true
			}
			close(started)
			<-release
			return false
		})
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("initial positive result was refused")
		}
		time.Sleep(2500 * time.Millisecond)
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("half-window refresh blocked fresh authorization")
		}
		<-started
		time.Sleep(2500 * time.Millisecond)
		for range 32 {
			if manager.HasActiveContract(source, destination) {
				t.Fatal("in-flight refresh extended expired authorization")
			}
		}
		if calls.Load() != 2 {
			t.Fatal("expired packets duplicated an unresolved refresh")
		}
		close(release)
		synctest.Wait()
		if len(manager.activeReads) != 0 || len(manager.activeContracts) != 0 {
			t.Fatal("denied refresh retained authorization or ownership")
		}
	})
}

// Cancellation after a successful Redis reply still refuses publication.
func TestResidentContractReadCanceledOwnerDoesNotPublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, cancel, source, destination := residentReadControl(t)
		started, release := make(chan struct{}), make(chan struct{})
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { close(started); <-release; return true })
		result := make(chan bool, 1)
		go func() { result <- manager.HasActiveContract(source, destination) }()
		<-started
		cancel()
		close(release)
		if <-result || len(manager.activeReads) != 0 || len(manager.activeContracts) != 0 {
			t.Fatal("canceled read published or retained work")
		}
	})
}

// Unexpected panics keep their cause but do not bypass the admission interval
// on the next packet or leave a permanently active read marker.
func TestResidentContractReadFailureRetainsInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		want := errors.New("synthetic contract read failure")
		calls := 0
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls++; panic(want) })
		func() {
			defer func() {
				if recover() != want {
					t.Fatal("contract read changed its failure")
				}
			}()
			manager.HasActiveContract(source, destination)
		}()
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls++; return true })
		if manager.HasActiveContract(source, destination) || calls != 1 || len(manager.activeReads) != 0 {
			t.Fatal("failed check retried early or retained its owner")
		}
		time.Sleep(time.Second)
		if !manager.HasActiveContract(source, destination) || calls != 2 {
			t.Fatal("failed check prevented the next admitted read")
		}
	})
}
