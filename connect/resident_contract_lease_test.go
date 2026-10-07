// Checks strict source leases at cache, refresh and real callback boundaries.
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

// Source expiry is an independent ceiling even while the ordinary five-second
// cache and one-second retry interval would otherwise retain the result.
func TestResidentContractLeaseExpiresBeforeOrdinaryCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		deadline := time.Now().Add(1500 * time.Millisecond)
		var calls atomic.Int32
		manager.readContract = func(context.Context, server.Id, server.Id) residentContractAllowance {
			calls.Add(1)
			return residentContractAllowance{active: true, validUntil: deadline}
		}
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("healthy short lease was refused")
		}
		time.Sleep(time.Second)
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("live lease was refused at refresh admission")
		}
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("short lease did not refresh at its first permitted half-window check")
		}
		time.Sleep(500*time.Millisecond - time.Nanosecond)
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("lease expired before its exact boundary")
		}
		time.Sleep(time.Nanosecond)
		if manager.HasActiveContract(source, destination) || calls.Load() != 2 {
			t.Fatal("cache or pair limiter extended an expired permission")
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

// A read that completes after its signed/source deadline cannot populate the
// cache or authorize even its original cold packet.
func TestResidentContractLeaseExpiredDuringReadCannotPublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		deadline := time.Now().Add(time.Second)
		started, release := make(chan struct{}), make(chan struct{})
		manager.readContract = func(context.Context, server.Id, server.Id) residentContractAllowance {
			close(started)
			<-release
			return residentContractAllowance{active: true, validUntil: deadline}
		}
		result := make(chan bool, 1)
		go func() { result <- manager.HasActiveContract(source, destination) }()
		<-started
		time.Sleep(time.Second)
		close(release)
		if <-result || len(manager.activeContracts) != 0 {
			t.Fatal("late positive source result created expired authority")
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

// A delay before taking the map lock must not make a pre-lock timestamp grant
// expired permission. An explicit channel barrier owns the ordering; mutex
// waits themselves are not durably blocked within a virtual-clock test.
func TestResidentContractLeaseDelayedAdmissionDoesNotReuseOldClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		now := time.Now()
		manager.activeContracts[model.NewUnorderedTransferPair(source, destination)] = &activeContractEntry{checkTime: now, validUntil: now.Add(time.Second)}
		manager.readContract = func(context.Context, server.Id, server.Id) residentContractAllowance {
			return residentContractAllowance{}
		}
		started, release := make(chan struct{}), make(chan struct{})
		manager.beforeCheckLockForTest = func() { close(started); <-release }
		result := make(chan bool, 1)
		go func() { result <- manager.HasActiveContract(source, destination) }()
		<-started
		time.Sleep(2 * time.Second)
		close(release)
		if <-result {
			t.Fatal("pre-lock clock reused an expired cache entry")
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

// A blocked positive refresh may outlive the prior lease, but packets cannot.
// Its eventual stale result remains refused and its owner is still joined.
func TestResidentContractLeaseBlockedRefreshDoesNotExtendAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		deadline := time.Now().Add(2 * time.Second)
		started, release := make(chan struct{}), make(chan struct{})
		calls := 0
		manager.readContract = func(context.Context, server.Id, server.Id) residentContractAllowance {
			calls++
			if calls == 2 {
				close(started)
				<-release
			}
			return residentContractAllowance{active: true, validUntil: deadline}
		}
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("initial live lease was refused")
		}
		time.Sleep(time.Second)
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("refresh admission discarded a still-live lease")
		}
		<-started
		time.Sleep(time.Second)
		if manager.HasActiveContract(source, destination) {
			t.Fatal("in-flight refresh extended the old lease")
		}
		close(release)
		synctest.Wait()
		if len(manager.activeContracts) != 0 || len(manager.activeReads) != 0 {
			t.Fatal("stale refresh republished authority or retained read ownership")
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

// The rollout source carries the same strict deadline as Redis. Neither a
// successful boolean, an absent deadline nor a completed context grants more.
func TestResidentContractLeaseFallbackPreservesDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fallback := newResidentContractFallback()
		deadline := time.Now().Add(100 * time.Millisecond)
		calls := 0
		fallback.readContract = func(context.Context, server.Id, server.Id) (model.ContractHoleStatus, time.Time, error) {
			calls++
			return model.ContractHolePositive, deadline, nil
		}
		manager := newResidentContractManagerWithFallback(ctx, cancel, server.NewId(), DefaultExchangeSettings(), fallback)
		manager.readContract = func(_ context.Context, source, destination server.Id) residentContractAllowance {
			return fallback.check(ctx, source, destination)
		}
		source, destination := server.NewId(), server.NewId()
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("healthy source lease was refused")
		}
		time.Sleep(100 * time.Millisecond)
		if manager.HasActiveContract(source, destination) || calls != 1 {
			t.Fatal("source deadline was lost in the local cache or interval")
		}
		if allowance := fallback.check(ctx, source, destination); allowance.active {
			t.Fatal("source accepted an already expired positive lease")
		}
		deadline = time.Time{}
		if allowance := fallback.check(ctx, source, destination); allowance.active || !allowance.validUntil.IsZero() {
			t.Fatal("source accepted a missing lease deadline")
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

// The real callback/shard/forward path must stop accepting offers at the exact
// source deadline, then reuse that forward after a replacement lease appears.
func TestResidentPacketLeaseDeadlineRevokesAndResumesLiveForward(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		attempts, stop := server.DenyPostgresForTest(t)
		defer stop()
		ctx, cancel := context.WithCancel(server.WithoutPostgres(t.Context()))
		defer cancel()
		settings := DefaultExchangeSettings()
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		ledger := &residentPayloadLedger{}
		resident.exchange.payloadOwnerLedger = ledger
		destination := server.NewId()
		forward := NewResidentForward(ctx, resident.exchange, destination)
		defer forward.Close()
		defer func() { _ = resident.CloseAndWait(context.Background()) }()
		resident.forwards[destination] = forward
		deadline := time.Now().Add(1500 * time.Millisecond)
		resident.residentContractManager.readContract = func(context.Context, server.Id, server.Id) residentContractAllowance {
			return residentContractAllowance{active: true, validUntil: deadline}
		}
		first := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if len(forward.send) != 1 {
			t.Fatal("healthy lease did not deliver through the callback")
		}
		acceptedAt := forward.lastActivityNanos.Load()
		time.Sleep(1500 * time.Millisecond)
		refused := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if len(forward.send) != 1 || forward.lastActivityNanos.Load() != acceptedAt {
			t.Fatal("expired cached lease accepted or renewed live-forward work")
		}
		requireResidentPoolOwnerReturned(t, refused, "expired lease offer")
		deadline = time.Now().Add(time.Hour)
		time.Sleep(time.Second)
		resumed := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if len(forward.send) != 2 {
			t.Fatal("replacement lease did not resume the existing forward")
		}
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		forward.Close()
		requireResidentPoolOwnersReturned(t, [][]byte{first, resumed}, "lease-delivered forward packets")
		if attempts() != 0 || server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("lease-bound callback attempted PostgreSQL")
		}
		if snapshot := ledger.snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 {
			t.Fatal("lease revocation retained pooled payload ownership")
		}
	})
}
