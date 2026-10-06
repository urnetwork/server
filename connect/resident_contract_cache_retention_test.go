package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A live resident can visit fresh provider pairs for its entire lifetime. The
// production cache must retain recent positive checks, not every past provider.
// Virtual time advances past the real freshness window without wall-clock waits.
func TestResidentContractCacheRetiresExpiredProviderHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettings()
		manager := newResidentContractManager(ctx, cancel, server.NewId(), settings)
		var calls int
		manager.readContract = func(context.Context, server.Id, server.Id) bool {
			calls++
			return true
		}
		const cohorts, providers = 8, 128
		var peakRetained int
		for cohort := range cohorts {
			if cohort > 0 {
				time.Sleep(settings.ContractManagerCheckTimeout + time.Nanosecond)
			}
			for range providers {
				providerId := server.NewId()
				if !manager.HasActiveContract(manager.clientId, providerId) ||
					!manager.HasActiveContract(manager.clientId, providerId) {
					t.Fatal("healthy provider pair lost its positive contract result")
				}
			}
			manager.stateLock.Lock()
			peakRetained = max(peakRetained, len(manager.activeContracts))
			manager.stateLock.Unlock()
		}
		if calls != cohorts*providers {
			t.Fatalf("fresh same-pair cache hit performed extra reads: %d", calls)
		}
		t.Logf("provider_pairs=%d reads=%d peak_retained=%d recent_cohort=%d", cohorts*providers, calls, peakRetained, providers)
		if peakRetained != providers {
			t.Fatalf("expired provider history retained: %d entries, want %d recent entries", peakRetained, providers)
		}
	})
}

// Expiring an old cache entry must not detach the database-read owner. A late
// caller still joins that owner and receives its fresh positive result.
func TestResidentContractCacheExpiryPreservesInFlightRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		manager := newResidentContractManager(ctx, cancel, server.NewId(), DefaultExchangeSettings())
		providerId := server.NewId()
		pair := model.NewTransferPair(manager.clientId, providerId)
		refreshStarted, releaseRefresh := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		manager.readContract = func(_ context.Context, _, destinationId server.Id) bool {
			if destinationId == providerId && calls.Add(1) == 2 {
				close(refreshStarted)
				<-releaseRefresh
			}
			return true
		}
		if !manager.HasActiveContract(manager.clientId, providerId) {
			t.Fatal("initial positive read failed")
		}
		time.Sleep(3 * time.Second)
		if !manager.HasActiveContract(manager.clientId, providerId) {
			t.Fatal("fresh positive cache blocked on its refresh")
		}
		<-refreshStarted
		time.Sleep(3 * time.Second)
		if !manager.HasActiveContract(manager.clientId, server.NewId()) {
			t.Fatal("unrelated provider did not remain independent")
		}
		result := make(chan bool, 1)
		go func() { result <- manager.HasActiveContract(manager.clientId, providerId) }()
		synctest.Wait()
		manager.stateLock.Lock()
		read := manager.activeReads[pair]
		joined := read != nil && read.waiters == 1
		manager.stateLock.Unlock()
		if !joined {
			t.Error("late caller did not join the held refresh owner")
		}
		close(releaseRefresh)
		if !<-result {
			t.Fatal("shared fresh positive result became a refusal")
		}
		synctest.Wait()
		if !manager.HasActiveContract(manager.clientId, providerId) || calls.Load() != 2 {
			t.Fatal("completed refresh did not publish one reusable positive cache entry")
		}
		manager.stateLock.Lock()
		defer manager.stateLock.Unlock()
		if len(manager.activeReads) != 0 {
			t.Fatal("completed read owner remained registered")
		}
	})
}

// A disabled freshness cache still returns positive reads but must not retain
// historical results which no subsequent invocation is allowed to reuse.
func TestResidentContractCacheDisabledDoesNotRetainProviderHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := newResidentContractManager(ctx, cancel, server.NewId(), &ExchangeSettings{})
	var calls int
	manager.readContract = func(context.Context, server.Id, server.Id) bool {
		calls++
		return true
	}
	for range 128 {
		providerId := server.NewId()
		if !manager.HasActiveContract(manager.clientId, providerId) ||
			!manager.HasActiveContract(manager.clientId, providerId) {
			t.Fatal("disabled cache changed a positive database result")
		}
	}
	if calls != 256 {
		t.Fatalf("disabled cache reused a result: reads=%d", calls)
	}
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	if len(manager.activeContracts) != 0 {
		t.Fatalf("disabled cache retained %d historical positive pairs", len(manager.activeContracts))
	}
}
