package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Clean deferred legacy acknowledgements are separately owned pending work.
// Their presence must not erase the verified retry authority of an unrelated
// refused dispute or falsely count any deferred amount as a financial close.
func TestForceCloseDeferredLegacySiblingKeepsAccountingRetryAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		const reserved = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*reserved, reserved)
		before := bad.state(t, ctx)
		deferred := make([]*forceCloseDisputeFixture, 8)
		for index := range deferred {
			deferred[index] = newLegacyForceCloseDisputeFixture(t, ctx, true, true, 1024, 1024, reserved)
		}
		selected, cursor, err := ForceCloseOpenContractIdsPage(ctx, bad.cutoff, len(deferred), 4, 1, 0, nil)
		if bad.state(t, ctx) != before {
			t.Fatal("mixed page changed the protected refusal's accounting")
		}
		for _, fixture := range deferred {
			state := fixture.state(t, ctx)
			var pending bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)`, fixture.contractId).Scan(&pending))
			})
			if !pending || state.outcome != "" || state.escrowSettled || !state.streamFound || state.providerPayoutByteCount != 0 || state.payerBalanceByteCount != forceCloseDisputeInitialBalance || state.netEscrowByteCount != reserved {
				t.Fatal("deferred acknowledgement changed financial ownership or abandoned its reservation")
			}
		}
		if selected != 1 || cursor == nil || err == nil {
			t.Fatalf("mixed page failed to exercise refusal plus deferred continuation: selected=%d cursor=%+v err=%v", selected, cursor, err)
		}
		accounting, ok := err.(*ForceCloseAccountingError)
		if !ok || accounting.VerifiedCloseCount() != 0 || accounting.AccountingRejectionCount() != 1 || accounting.QuarantinedAccountingRejectionCount() != 0 {
			t.Fatalf("clean deferred siblings discarded accounting retry authority: selected=%d error_type=%T err=%v", selected, err, err)
		}
		count, next, err := ForceCloseOpenContractIdsPage(ctx, bad.cutoff, len(deferred), 4, 1, 0, cursor)
		if err != nil || count != 0 || next != nil {
			t.Fatalf("continued cursor did not finish its bounded pass: count=%d next=%+v err=%v", count, next, err)
		}
		completed := 0
		for shard := range LegacySettlementShardCount {
			result, err := FlushLegacySettlements(ctx, shard, nil, 64)
			if err != nil || result.Failed != 0 {
				t.Fatalf("deferred worker failed independent financial completion: %+v, %v", result, err)
			}
			completed += result.Completed
		}
		if completed != len(deferred) || bad.state(t, ctx) != before {
			t.Fatal("deferred worker count or protected refused reservation changed")
		}
		for _, fixture := range deferred {
			state := fixture.state(t, ctx)
			var swept, provided ByteCount
			var pending bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT
                COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
                COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
                EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)`,
					fixture.contractId, fixture.providerNetworkId).Scan(&swept, &provided, &pending))
			})
			// Legacy workers commit provider totals in PostgreSQL. The shared
			// fixture exposes only the Redis delta, which must remain zero to
			// avoid counting the same contribution twice in the account API.
			if state.outcome != ContractOutcomeSettled || !state.escrowSettled || state.escrowPayoutByteCount != 1024 ||
				state.streamFound || state.providerPayoutByteCount != 0 || swept != 1024 || provided != 1024 || pending ||
				state.payerBalanceByteCount != forceCloseDisputeInitialBalance-1024 || state.netEscrowByteCount != 0 {
				t.Fatalf("deferred worker final conservation: terminal=%t metadata=%t escrow_payout=%t stream_removed=%t redis_delta_zero=%t durable_sweep=%t durable_provider=%t intent_removed=%t payer_debit=%t reservation_released=%t",
					state.outcome == ContractOutcomeSettled, state.escrowSettled, state.escrowPayoutByteCount == 1024,
					!state.streamFound, state.providerPayoutByteCount == 0, swept == 1024, provided == 1024, !pending,
					state.payerBalanceByteCount == forceCloseDisputeInitialBalance-1024, state.netEscrowByteCount == 0)
			}
		}
	})
}

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

func TestForceCloseDeferredAuthorityRequiresEveryPhase(t *testing.T) {
	operation := errors.New("synthetic close operation failure")
	for _, test := range []struct {
		name                                string
		closeErr, quarantineErr, cleanupErr error
		want                                bool
	}{
		{"clean-pending", nil, nil, errLegacySettlementPending, true},
		{"terminal-success", nil, nil, nil, false},
		{"failed-close", operation, nil, errLegacySettlementPending, false},
		{"failed-quarantine", nil, operation, errLegacySettlementPending, false},
		{"joined-cleanup", nil, nil, errors.Join(errLegacySettlementPending, operation), false},
		{"wrapped-cleanup", nil, nil, fmt.Errorf("other verification: %w", errLegacySettlementPending), false},
		{"same-text", nil, nil, errors.New(errLegacySettlementPending.Error()), false},
		{"canceled-close", context.Canceled, nil, errLegacySettlementPending, false},
		{"settled-close", errContractAlreadySettled, nil, errLegacySettlementPending, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isForceCloseDeferredSettlement(test.closeErr, test.quarantineErr, test.cleanupErr); got != test.want {
				t.Fatalf("deferred authority=%t want=%t", got, test.want)
			}
		})
	}
}
