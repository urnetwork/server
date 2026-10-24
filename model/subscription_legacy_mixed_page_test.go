package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The held contract owner lets the raw selector finish, then publishes a fresh
// report before expiry can reload it. The completed eligibility rejection must
// retain the stream and credit, while the unrelated refused row keeps its
// bounded retry authority.
func TestForceCloseFreshEligibilitySkipKeepsAccountingRetryAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		const reserved = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*reserved, reserved)
		fresh := newForceCloseDisputeFixture(t, ctx, true, true, 1024, 1024, reserved)
		badBefore, freshBefore := bad.state(t, ctx), fresh.state(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, fresh.contractId))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		type pageResult struct {
			count  int64
			cursor *ContractExpiryCursor
			err    error
		}
		done := make(chan pageResult, 1)
		go func() {
			var result pageResult
			server.HandleError(func() {
				result.count, result.cursor, result.err = ForceCloseOpenContractIdsPage(ctx, bad.cutoff, 1, 1, 1, 0, nil)
			}, func(err error) { result.err = err })
			done <- result
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		server.RaisePgResult(held.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, fresh.contractId, server.NowUtc()))
		server.Raise(held.Commit(ctx))
		var result pageResult
		select {
		case result = <-done:
		case <-ctx.Done():
			t.Fatal("eligible-withdrawal page failed to join", ctx.Err())
		}
		if bad.state(t, ctx) != badBefore || fresh.state(t, ctx) != freshBefore {
			t.Fatal("fresh eligibility check changed stream or financial accounting")
		}
		accounting, ok := result.err.(*ForceCloseAccountingError)
		if !ok || result.count != 1 || result.cursor == nil || accounting.VerifiedCloseCount() != 0 || accounting.AccountingRejectionCount() != 1 {
			t.Fatalf("completed eligibility rejection discarded independent retry authority: count=%d error_type=%T err=%v", result.count, result.err, result.err)
		}
	})
}
