// A refused request returns after one bounded compensation pass while Redis
// fails every command on its token, and the token is still released once
// Redis recovers. Only Redis delivery is changed; PG, Redis and the public
// creator are real.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// While armed, fails every EVAL on one balance's reservation keys except a new
// reservation, without sending it: once a request holds its token, each of its
// release and recovery calls times out, as on an exhausted deadline pool.
type redisCompensationOutageHook struct {
	key     string
	armed   atomic.Bool
	refused atomic.Int64
}

func (self *redisCompensationOutageHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (self *redisCompensationOutageHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (self *redisCompensationOutageHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if self.armed.Load() && command.Name() == "eval" && 9 < len(args) && args[3] == self.key {
			// ARGV[1] follows the six keys
			if operation, _ := args[9].(string); !strings.HasPrefix(operation, "reserve-pending") {
				self.refused.Add(1)
				return context.DeadlineExceeded
			}
		}
		return next(ctx, command)
	}
}

func TestRedisAdmissionCompensationOutageReturnsAndReleasesLater(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		// The request reserves, then its client fence refuses it before
		// publication, so it must compensate its own pending token.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false, deactivate_time=$2 WHERE client_id=$1`, f.destinationId, server.NowUtc()))
		})
		hook := &redisCompensationOutageHook{key: redisContractReservationKeys(f.balanceId)[0]}
		addRedisDeadlinePoolsHook(ctx, hook)
		hook.armed.Store(true)
		deferredBefore := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("compensation", "deferred"))
		completedBefore := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("compensation", "completed"))

		request := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(request)
		type outcome struct {
			escrow *TransferEscrow
			err    error
		}
		done := make(chan outcome, 1)
		started := time.Now()
		go func() {
			var result outcome
			if value := server.HandleError(func() {
				result.escrow, result.err = CreateTransferEscrow(request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23)
			}); value != nil {
				result.err = fmt.Errorf("admission panic: %v", value)
			}
			done <- result
		}()
		var result outcome
		select {
		case result = <-done:
		// one bounded pass, far short of the cleanup budget the caller used
		// to wait out
		case <-time.After(3 * time.Second):
			// Redis recovers so the blocked caller's own cleanup can join.
			hook.armed.Store(false)
			<-done
			t.Fatalf("the refused caller waited %s for its compensation instead of one bounded pass", time.Since(started).Round(time.Second))
		}
		if result.escrow != nil || !errors.Is(result.err, ErrContractDestinationInactive) {
			t.Fatal("the outage changed the caller's refusal", result.escrow, result.err)
		}
		if errors.Is(result.err, errRedisReservationCleanupPending) {
			t.Fatal("a compensation the deferred owner holds was reported abandoned", result.err)
		}
		if hook.refused.Load() == 0 {
			t.Fatal("the outage did not reach the request's own compensation")
		}
		if deferred := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("compensation", "deferred")) - deferredBefore; deferred != 1 {
			t.Fatal("the pending compensation did not pass to the deferred owner", deferred)
		}
		// Every release still fails, so the token stays reserved.
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("the outage released or lost the refused token", got)
		}

		hook.armed.Store(false)
		drainRedisContractCompensations(t, ctx)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("the deferred owner did not release the token after Redis recovered", got)
		}
		if completed := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("compensation", "completed")) - completedBefore; completed != 1 {
			t.Fatal("the deferred release was not counted completed", completed)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, owner.contractId, false)
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}
