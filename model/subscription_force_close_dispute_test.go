// Exercises expiry-created disputes through the real sweep and settlement path.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

const forceCloseDisputeInitialBalance = ByteCount(4 * 1024 * 1024 * 1024)

// Generated identities and fixed synthetic age keep each case independent of scheduling.
type forceCloseDisputeFixture struct {
	contractId           server.Id
	balanceId            server.Id
	providerNetworkId    server.Id
	cutoff               time.Time
	sourceByteCount      ByteCount
	destinationByteCount ByteCount
}

// Comparable state includes durable and mirrored money so a repeat cannot hide duplicate posts.
type forceCloseDisputeState struct {
	outcome                 string
	dispute                 bool
	open                    bool
	closeTime               string
	escrowSettled           bool
	escrowPayoutByteCount   ByteCount
	payerBalanceByteCount   ByteCount
	sourceByteCount         ByteCount
	destinationByteCount    ByteCount
	sourceCheckpoint        bool
	destinationCheckpoint   bool
	netEscrowByteCount      ByteCount
	providerPayoutByteCount ByteCount
	streamFound             bool
}

// Creates either a resumable checkpoint pair or an already-disputed final pair.
func newForceCloseDisputeFixture(
	t testing.TB,
	ctx context.Context,
	sourceCheckpoint bool,
	destinationCheckpoint bool,
	sourceByteCount ByteCount,
	destinationByteCount ByteCount,
	escrowByteCount ByteCount,
) *forceCloseDisputeFixture {
	t.Helper()
	payerNetworkId := server.NewId()
	providerNetworkId := server.NewId()
	sourceId := server.NewId()
	destinationId := server.NewId()
	Testing_CreateNetwork(ctx, payerNetworkId, "synthetic-payer-"+payerNetworkId.String(), server.NewId())
	Testing_CreateNetwork(ctx, providerNetworkId, "synthetic-provider-"+providerNetworkId.String(), server.NewId())
	insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
		sourceId:      payerNetworkId,
		destinationId: providerNetworkId,
	})
	AddBasicTransferBalance(ctx, payerNetworkId, forceCloseDisputeInitialBalance,
		server.NowUtc(), server.NowUtc().Add(24*time.Hour))
	balances := GetActiveTransferBalances(ctx, payerNetworkId)
	connect.AssertEqual(t, 1, len(balances))
	contractId, _, err := CreateContract(ctx, payerNetworkId, sourceId,
		providerNetworkId, destinationId, escrowByteCount)
	connect.AssertEqual(t, nil, err)
	AddToStream(ctx, contractId, sourceId, destinationId, nil)
	connect.AssertEqual(t, nil, CloseContract(ctx, contractId, sourceId, sourceByteCount, sourceCheckpoint))
	connect.AssertEqual(t, nil, CloseContract(ctx, contractId, destinationId, destinationByteCount, destinationCheckpoint))
	createTime := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`UPDATE transfer_contract SET create_time = $2 WHERE contract_id = $1`, contractId, createTime))
	}, server.TxReadCommitted)
	return &forceCloseDisputeFixture{
		contractId:           contractId,
		balanceId:            balances[0].BalanceId,
		providerNetworkId:    providerNetworkId,
		cutoff:               createTime.Add(time.Hour),
		sourceByteCount:      sourceByteCount,
		destinationByteCount: destinationByteCount,
	}
}

// Reads exact fixture rows; a missing or duplicated escrow row is a fixture failure.
func (self *forceCloseDisputeFixture) state(t testing.TB, ctx context.Context) forceCloseDisputeState {
	t.Helper()
	state := forceCloseDisputeState{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
            SELECT coalesce(c.outcome,''), c.dispute, c.open,
                   coalesce(c.close_time::text,''), e.settled,
                   coalesce(e.payout_byte_count,0), b.balance_byte_count,
                   s.used_transfer_byte_count, d.used_transfer_byte_count,
                   s.checkpoint, d.checkpoint
            FROM transfer_contract c
            JOIN transfer_escrow e ON e.contract_id=c.contract_id
            JOIN transfer_balance b ON b.balance_id=e.balance_id
            JOIN contract_close s ON s.contract_id=c.contract_id AND s.party='source'
            JOIN contract_close d ON d.contract_id=c.contract_id AND d.party='destination'
            WHERE c.contract_id=$1
        `, self.contractId)
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				t.Fatal("synthetic contract state missing")
			}
			server.Raise(rows.Scan(&state.outcome, &state.dispute, &state.open,
				&state.closeTime, &state.escrowSettled, &state.escrowPayoutByteCount,
				&state.payerBalanceByteCount, &state.sourceByteCount, &state.destinationByteCount,
				&state.sourceCheckpoint, &state.destinationCheckpoint))
			if rows.Next() {
				t.Fatal("synthetic contract has multiple funding rows")
			}
		})
	})
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, netEscrowKey(self.balanceId)).Int64()
		if err != server.RedisNil {
			server.Raise(err)
			state.netEscrowByteCount = ByteCount(value)
		}
		value, err = r.Get(ctx, accountBalanceNetPayoutByteCountKey(self.providerNetworkId)).Int64()
		if err != server.RedisNil {
			server.Raise(err)
			state.providerPayoutByteCount = ByteCount(value)
		}
	})
	_, _, state.streamFound = GetStream(ctx, self.contractId)
	return state
}

// Both orientations and both-checkpoint pairs must converge in one bounded sweep.
// Equal totals are healthy controls; reversed unequal totals pin average settlement.
func TestForceCloseCheckpointDisputeConvergesInOneSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		orientations := []struct {
			name                  string
			sourceCheckpoint      bool
			destinationCheckpoint bool
		}{
			{name: "source-checkpoint", sourceCheckpoint: true},
			{name: "destination-checkpoint", destinationCheckpoint: true},
			{name: "both-checkpoint", sourceCheckpoint: true, destinationCheckpoint: true},
		}
		const low = ByteCount(1024 * 1024)
		const high = low + AcceptableTransfersByteDifference + 2
		usages := []struct {
			name        string
			source      ByteCount
			destination ByteCount
		}{
			{name: "equal", source: low, destination: low},
			{name: "destination-higher", source: low, destination: high},
			{name: "source-higher", source: high, destination: low},
		}
		fixtures := []*forceCloseDisputeFixture{}
		caseNames := []string{}
		for _, orientation := range orientations {
			for _, usage := range usages {
				fixture := newForceCloseDisputeFixture(t, ctx,
					orientation.sourceCheckpoint, orientation.destinationCheckpoint,
					usage.source, usage.destination, ByteCount(1024*1024*1024))
				state := fixture.state(t, ctx)
				if !state.open || state.dispute || state.outcome != "" || !state.streamFound {
					t.Fatal("checkpoint fixture is not initially open and non-disputed")
				}
				fixtures = append(fixtures, fixture)
				caseNames = append(caseNames, orientation.name+"/"+usage.name)
			}
		}
		closeCount, err := ForceCloseOpenContractIds(ctx, fixtures[0].cutoff, 10, 1, 0, 0)
		if err != nil {
			t.Error("first sweep returned an error for valid checkpoint totals")
		}
		connect.AssertEqual(t, int64(len(fixtures)), closeCount)
		firstStates := make([]forceCloseDisputeState, 0, len(fixtures))
		for index, fixture := range fixtures {
			state := fixture.state(t, ctx)
			firstStates = append(firstStates, state)
			mean := (fixture.sourceByteCount + fixture.destinationByteCount) / 2
			if state.outcome != ContractOutcomeSettled || state.dispute || state.open || state.streamFound ||
				!state.escrowSettled || state.sourceCheckpoint || state.destinationCheckpoint ||
				state.sourceByteCount != fixture.sourceByteCount || state.destinationByteCount != fixture.destinationByteCount ||
				state.escrowPayoutByteCount != mean || state.providerPayoutByteCount != mean ||
				state.payerBalanceByteCount != forceCloseDisputeInitialBalance-mean || state.netEscrowByteCount != 0 {
				t.Errorf("%s: first sweep did not settle and release exactly once", caseNames[index])
			}
		}
		closeCount, err = ForceCloseOpenContractIds(ctx, fixtures[0].cutoff, 10, 1, 0, 0)
		if err != nil || closeCount != 0 {
			t.Error("second sweep performed work after the required one-pass convergence")
		}
		for index, fixture := range fixtures {
			if state := fixture.state(t, ctx); state != firstStates[index] {
				t.Errorf("%s: repeat sweep changed terminal or accounting state", caseNames[index])
			}
		}
	})
}

// A statement trigger rejects even zero-row dispute updates, proving healthy
// finalized rows do not enter the extra settlement transaction.
func TestForceCloseHealthyFinalizationSkipsDisputeSettlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixtures := []*forceCloseDisputeFixture{
			newForceCloseDisputeFixture(t, ctx, true, false, 1024, 1024, 4096),
			newForceCloseDisputeFixture(t, ctx, false, true, 1024, 1024, 4096),
			newForceCloseDisputeFixture(t, ctx, true, true, 1024, 1024, 4096),
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
                CREATE FUNCTION synthetic_reject_dispute_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    RAISE EXCEPTION 'synthetic unexpected dispute settlement';
                END;
                $$;
                CREATE TRIGGER synthetic_reject_dispute_settlement
                BEFORE UPDATE OF dispute ON transfer_contract
                FOR EACH STATEMENT EXECUTE FUNCTION synthetic_reject_dispute_settlement();
            `))
		}, server.TxReadCommitted)
		recovered := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=dispute WHERE false`))
			}, server.TxReadCommitted)
		})
		guardErr, ok := recovered.(error)
		if !ok || !strings.Contains(guardErr.Error(), "synthetic unexpected dispute settlement") {
			t.Fatal("statement-level guard did not detect a zero-row dispute update")
		}
		closeCount, err := ForceCloseOpenContractIds(ctx, fixtures[0].cutoff, 10, 1, 0, 0)
		if err != nil || closeCount != int64(len(fixtures)) {
			t.Error("healthy finalization entered dispute settlement")
		}
		for _, fixture := range fixtures {
			state := fixture.state(t, ctx)
			if state.outcome != ContractOutcomeSettled || state.dispute || state.open || state.streamFound ||
				!state.escrowSettled || state.escrowPayoutByteCount != 1024 || state.netEscrowByteCount != 0 ||
				state.providerPayoutByteCount != 1024 || state.payerBalanceByteCount != forceCloseDisputeInitialBalance-1024 {
				t.Error("healthy finalization changed settlement accounting")
			}
		}
	})
}

// A failed dispute settlement must retain its reservation, including when this
// sweep created the dispute. Non-disputed malformed quarantine is a separate policy.
func TestForceCloseDisputeSettlementFailureRollsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		fixtures := []*forceCloseDisputeFixture{
			newForceCloseDisputeFixture(t, ctx, true, false, 0, 4*escrow, escrow),
			newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*escrow, escrow),
		}
		connect.AssertEqual(t, false, fixtures[0].state(t, ctx).dispute)
		connect.AssertEqual(t, true, fixtures[1].state(t, ctx).dispute)
		firstStates := make([]forceCloseDisputeState, len(fixtures))
		for pass := 0; pass < 2; pass++ {
			closeCount, err := ForceCloseOpenContractIds(ctx, fixtures[0].cutoff, 10, 1, 0, 0)
			if err == nil || !strings.Contains(err.Error(), "Escrow does not have enough value") {
				t.Error("insufficient disputed escrow did not retain its settlement error")
			}
			if closeCount != int64(len(fixtures)) {
				t.Error("failed disputed settlement disappeared from the next sweep")
			}
			for index, fixture := range fixtures {
				state := fixture.state(t, ctx)
				if state.outcome != "" || !state.dispute || state.open || state.escrowSettled ||
					state.escrowPayoutByteCount != 0 || state.providerPayoutByteCount != 0 ||
					state.payerBalanceByteCount != forceCloseDisputeInitialBalance || state.netEscrowByteCount != escrow {
					t.Errorf("case %d: failed dispute settlement changed terminal or accounting state", index)
				}
				if pass == 0 {
					firstStates[index] = state
				} else if state != firstStates[index] {
					t.Errorf("case %d: failed retry changed preserved dispute state", index)
				}
			}
		}
	})
}

// An injected PostgreSQL query-cancelled error at the dispute clear must roll
// back the row. The trigger exists only in DefaultTestEnv's disposable database.
func TestForceCloseDisputeCancellationPreservesReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newForceCloseDisputeFixture(t, ctx, false, false, 0,
			AcceptableTransfersByteDifference+2, ByteCount(1024*1024*1024))
		before := fixture.state(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
                CREATE SEQUENCE synthetic_dispute_clear_attempts;
                CREATE FUNCTION synthetic_cancel_dispute_clear() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    PERFORM nextval('synthetic_dispute_clear_attempts');
                    RAISE EXCEPTION USING ERRCODE='57014', MESSAGE='synthetic dispute cancellation';
                END;
                $$;
                CREATE TRIGGER synthetic_cancel_dispute_clear
                AFTER UPDATE ON transfer_contract
                FOR EACH ROW WHEN (OLD.dispute AND NOT NEW.dispute AND NEW.outcome IS NULL)
                EXECUTE FUNCTION synthetic_cancel_dispute_clear();
            `))
		}, server.TxReadCommitted)
		_, err := ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 0, 0)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "57014" {
			t.Error("database cancellation did not retain its typed error")
		}
		var progress *ForceCloseAccountingError
		if errors.As(err, &progress) {
			t.Error("database cancellation gained accounting retry authority")
		}
		if after := fixture.state(t, ctx); after != before {
			t.Error("database cancellation changed disputed or accounting state")
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT last_value, is_called FROM synthetic_dispute_clear_attempts`)
			server.WithPgResult(rows, err, func() {
				connect.AssertEqual(t, true, rows.Next())
				var attempts int64
				var called bool
				server.Raise(rows.Scan(&attempts, &called))
				if !called || attempts != 1 {
					t.Error("failed original close was retried by terminal verification")
				}
			})
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_cancel_dispute_clear ON transfer_contract`))
		}, server.TxReadCommitted)
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()
		var callErr error
		recovered := server.HandleError(func() {
			_, callErr = ForceCloseOpenContractIds(cancelledCtx, fixture.cutoff, 10, 1, 0, 0)
		})
		if recovered == nil && callErr == nil {
			t.Error("cancelled sweep reported success")
		}
		if recoveredErr, ok := recovered.(error); ok && errors.As(recoveredErr, &progress) || errors.As(callErr, &progress) {
			t.Error("caller cancellation gained accounting retry authority")
		}
		if after := fixture.state(t, ctx); after != before {
			t.Error("caller cancellation changed disputed or accounting state")
		}
	})
}

// Exact settled duplicates remain errors when terminal verification or cleanup fails.
func TestForceCloseSettledDuplicateRetainsTerminalGuards(t *testing.T) {
	settled := fmt.Errorf("%w: synthetic contract", errContractAlreadySettled)
	guardErrors := []error{
		errors.New("contract disappeared before force-close verification"),
		errors.New("contract remained non-final after force-close attempt"),
		context.Canceled,
	}
	for _, guardErr := range guardErrors {
		err := finishForceCloseContract(settled, func() error {
			t.Fatal("typed settled duplicate attempted quarantine")
			return nil
		}, func() error { return guardErr })
		if !errors.Is(err, errContractAlreadySettled) || !errors.Is(err, guardErr) {
			t.Errorf("terminal guard %q was suppressed", guardErr)
		}
	}
}
