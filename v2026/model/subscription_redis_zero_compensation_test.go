package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Each real companion transaction finishes before the only default PG slot is
// held. The unchanged public admission owner then unwinds concurrently. Known
// zero reservations (and missing origins) must not need a second PG connection.
// The barrier is released even on the old implementation, so the RED control
// joins its cleanup rather than leaving detached workers behind.
func TestRedisZeroCompensationReturnsWhilePostgresIsOccupied(t *testing.T) {
	for _, mode := range []string{"ordinary-zero", "preferred-zero", "canceled-zero", "missing-origin"} {
		t.Run(mode, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
				server.PgReset()
				defer func() { server.PgReset(); pop() }()
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				clients := newEscrowSelectionTestClients(t, ctx)
				preferred := mode == "preferred-zero"
				if preferred {
					setDynamicProberIdentityForTest(t, ctx, clients)
				}
				ids := redisSelectionTestGrants(t, ctx, clients, 1, 100, preferred)
				want := errRedisReservationInsufficient
				if mode == "missing-origin" {
					want = ErrMissingCompanionOrigin
				} else {
					redisSelectionReserveLegacy(t, ctx, clients, ids, 100)
				}
				requestCtx, stopRequests := context.WithCancel(ctx)
				defer stopRequests()
				const concurrent = 16
				ready := make(chan struct{}, concurrent)
				resume := make(chan struct{})
				type outcome struct {
					owner *redisContractAdmission
					err   error
				}
				done := make(chan outcome, concurrent)
				for range concurrent {
					go func() {
						var owner *redisContractAdmission
						var returned error
						panicErr := server.HandleError(func() {
							_, returned = runRedisContractAdmission(requestCtx, func(owned context.Context) (*TransferEscrow, error) {
								owner = redisAdmissionFromContext(owned)
								escrow, err := createCompanionTransferEscrow(owned, clients.providerNetworkId, clients.providerId,
									clients.payerNetworkId, clients.payerId, 17, time.Hour)
								ready <- struct{}{}
								<-resume
								return escrow, err
							})
						})
						if panicErr != nil {
							returned = errors.Join(returned, fmt.Errorf("foreground panic: %v", panicErr))
						}
						done <- outcome{owner, returned}
					}()
				}
				for range concurrent {
					select {
					case <-ready:
					case early := <-done:
						close(resume)
						t.Fatal("foreground failed before the cleanup barrier", early.err)
					case <-ctx.Done():
						close(resume)
						t.Fatal("foreground did not join before the cleanup barrier", ctx.Err())
					}
				}
				outcomes := make([]outcome, 0, concurrent)
				server.Db(ctx, func(server.PgConn) {
					if mode == "canceled-zero" {
						stopRequests()
					}
					close(resume)
					guard := time.NewTimer(time.Second)
					defer guard.Stop()
					for len(outcomes) != concurrent {
						select {
						case result := <-done:
							outcomes = append(outcomes, result)
						case <-guard.C:
							return // Release PG before joining the baseline's cleanup.
						}
					}
				})
				returnedWhileHeld := len(outcomes)
				for len(outcomes) != concurrent {
					select {
					case result := <-done:
						outcomes = append(outcomes, result)
					case <-ctx.Done():
						t.Fatal("cleanup did not join after releasing PG", ctx.Err())
					}
				}
				if returnedWhileHeld != concurrent {
					t.Errorf("conclusive zero cleanup waited for PostgreSQL: returned %d/%d while the sole slot was held", returnedWhileHeld, concurrent)
				}
				for _, result := range outcomes {
					if !errors.Is(result.err, want) || result.owner == nil {
						t.Fatal("companion refusal changed", result.err)
					}
					if mode != "missing-origin" && len(result.owner.attemptedBalanceIds) != 1 {
						t.Fatal("zero reservation lost retained-marker discovery")
					}
					redisRecoveryRequireMarker(t, ctx, ids[0], result.owner.contractId, false)
					server.Redis(ctx, func(client server.RedisClient) {
						if client.HExists(ctx, redisContractReservationKeys(ids[0])[1], result.owner.contractId.String()).Val() {
							t.Error("refused request retained a positive token")
						}
					})
				}
				if mode != "missing-origin" && Testing_NetEscrowByteCount(ctx, ids[0]) != 100 {
					t.Error("zero refusal changed the existing neighbor's reservation")
				}
			})
		})
	}
}

// The first pass is entirely zero, not a partial positive reservation. Its
// balance must still be searched for a recoverable abandoned request.
func TestRedisZeroCompensationRetainsFullGrantRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 1000)
		request := withRedisContractAdmission(ctx)
		id := redisAdmissionFromContext(request).contractId
		escrow, err := CreateTransferEscrow(request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 29)
		if err != nil || escrow == nil || escrow.ContractId != id || escrow.TransferByteCount != 29 {
			t.Fatal("all-zero discovery did not recover and resume the same request", escrow, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 29 {
			t.Fatal("all-zero recovery duplicated or dropped custody", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
	})
}
