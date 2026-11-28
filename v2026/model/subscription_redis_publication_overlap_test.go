// A committed request must not advertise its pending acknowledgement as peer recovery work.
package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Hold the first acknowledged creator at its Redis publication boundary while
// a second public creator on the same grant completes. Both SQL owners joined
// before that boundary, so neither request needs a peer's SQL custody proof.
func TestRedisConfirmedPublicationConcurrentCreatorsDoNotRecoverEachOther(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0], started: make(chan server.Id, 1), release: make(chan struct{})}
		installRedisPublicationHook(t, ctx, hook)
		type result struct {
			escrow *TransferEscrow
			err    error
		}
		firstResult := make(chan result, 1)
		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			got := result{}
			panicErr := redisPublicationHandleError(func() {
				got.escrow, got.err = CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
			})
			got.err = errors.Join(got.err, panicErr)
			firstResult <- got
		}()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(hook.release) }) }
		defer func() { release(); <-firstDone }()
		var firstId server.Id
		select {
		case firstId = <-hook.started:
		case got := <-firstResult:
			t.Fatal("first creator missed its acknowledgement boundary", got.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var second *TransferEscrow
		var secondErr error
		panicErr := redisPublicationHandleError(func() {
			second, secondErr = CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 29)
		})
		if err := errors.Join(secondErr, panicErr); err != nil || second == nil {
			t.Fatal("healthy sibling did not create its contract", err)
		}
		release()
		first := <-firstResult
		if first.err != nil || first.escrow == nil || first.escrow.ContractId != firstId {
			t.Fatal("held creator did not finish its own acknowledgement", first.err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, firstId, 69, 3)
		stored := GetTransferEscrow(ctx, second.ContractId)
		if stored == nil || stored.TransferByteCount != 29 || len(stored.Balances) != 1 || stored.Balances[0].BalanceId != fixture.balanceId || stored.Balances[0].BalanceByteCount != 29 {
			t.Fatal("sibling lost its exact committed allocation")
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, second.ContractId, false)
		observer.require(t, 2*(1+redisPublicationDiscoveryAcquisitions), 0)
		hook.require(t, 0, 0)
	})
}
