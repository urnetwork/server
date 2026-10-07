package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
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
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool {
			calls++
			return true
		})
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

// Expired history includes denied-pair admission state, not only positives.
func TestResidentContractCacheRetiresDeniedProviderHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, _ := residentReadControl(t)
		var calls int
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls++; return false })
		const cohorts, providers = 8, 128
		for cohort := range cohorts {
			if cohort > 0 {
				time.Sleep(time.Second)
			}
			for range providers {
				provider := server.NewId()
				if manager.HasActiveContract(source, provider) || manager.HasActiveContract(source, provider) {
					t.Fatal("denied provider authorized a packet")
				}
			}
			if len(manager.checkLimiters) != providers || len(manager.activeContracts) != 0 || len(manager.activeReads) != 0 {
				t.Fatalf("denied history accumulated: limiters=%d", len(manager.checkLimiters))
			}
		}
		if calls != cohorts*providers {
			t.Fatalf("denied cohort reads=%d", calls)
		}
	})
}

// A disabled freshness cache still returns positive reads but must not retain
// historical results which no subsequent invocation is allowed to reuse.
func TestResidentContractCacheDisabledDoesNotRetainProviderHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := newResidentContractManager(ctx, cancel, server.NewId(), &ExchangeSettings{MaxConcurrentForwardsPerResident: 128})
	var calls int
	manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool {
		calls++
		return true
	})
	for range 128 {
		providerId := server.NewId()
		if !manager.HasActiveContract(manager.clientId, providerId) ||
			manager.HasActiveContract(manager.clientId, providerId) {
			t.Fatal("disabled cache bypassed the one-second check interval")
		}
	}
	if calls != 128 {
		t.Fatalf("disabled cache bypassed the check limit: reads=%d", calls)
	}
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	if len(manager.activeContracts) != 0 {
		t.Fatalf("disabled cache retained %d historical positive pairs", len(manager.activeContracts))
	}
}
