// Payout fixtures observe durable consumption before invoking the same bounded
// debit worker used in production. Redis observation failures are never zero.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

type payoutDebitTestState struct {
	networkId                              server.Id
	initial, credit, pendingBytes, settled ByteCount
	legacy, reserved                       ByteCount
	pending, applied, escrows, invalid     int
	inWindow                               bool
}

// Only an acknowledged missing key represents zero. Malformed replies,
// connection failures and cancellation fail the conservation observation.
func readPayoutDebitTestState(t testing.TB, ctx context.Context, balanceId server.Id) payoutDebitTestState {
	t.Helper()
	var state payoutDebitTestState
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id,start_balance_byte_count,balance_byte_count,
			start_time <= clock_timestamp() AT TIME ZONE 'UTC' AND clock_timestamp() AT TIME ZONE 'UTC' < end_time
			FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&state.networkId, &state.initial, &state.credit, &state.inWindow))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT applied),
			COALESCE(sum(debit_byte_count) FILTER (WHERE NOT applied),0),count(*) FILTER (WHERE applied)
			FROM transfer_debit_journal WHERE balance_id=$1`, balanceId).Scan(&state.pending, &state.pendingBytes, &state.applied))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE NOT settled OR NOT redis_reserved),
			COALESCE(sum(payout_byte_count),0) FROM transfer_escrow WHERE balance_id=$1`, balanceId).
			Scan(&state.escrows, &state.invalid, &state.settled))
	})
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		for _, counter := range []struct {
			key   string
			value *ByteCount
		}{{key: netEscrowKey(balanceId), value: &state.legacy}, {key: redisContractReservationKeys(balanceId)[0], value: &state.reserved}} {
			value, err := client.Get(bounded, counter.key).Int64()
			if errors.Is(err, redis.Nil) {
				value = 0
			} else if err != nil {
				return fmt.Errorf("observe payout reservation %s: %w", counter.key, err)
			}
			if value < 0 {
				return fmt.Errorf("negative payout reservation %s: %d", counter.key, value)
			}
			*counter.value = ByteCount(value)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("payout debit reservation is unavailable, not zero: %v", err)
	}
	return state
}

type payoutDebitTestSweep struct {
	contractId, networkId server.Id
	bytes                 ByteCount
	revenue               NanoCents
	providers             string
}

// Capture complete provider allocations so draining payer debt cannot change
// their identities or mint another account payout.
func readPayoutDebitTestSweeps(t testing.TB, ctx context.Context, balanceId server.Id) []payoutDebitTestSweep {
	t.Helper()
	var sweeps []payoutDebitTestSweep
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT contract_id,network_id,payout_byte_count,
			payout_net_revenue_nano_cents,provider_payouts::text FROM transfer_escrow_sweep
			WHERE balance_id=$1 ORDER BY contract_id,network_id`, balanceId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var sweep payoutDebitTestSweep
				server.Raise(rows.Scan(&sweep.contractId, &sweep.networkId, &sweep.bytes, &sweep.revenue, &sweep.providers))
				sweeps = append(sweeps, sweep)
			}
		})
	})
	return sweeps
}

// Fully consumed grants use the same conservation check as a partial final
// settlement. Neither fixture has an implicit background debit worker.
func assertPayoutDebitTestConsumedAndDrained(t testing.TB, ctx context.Context, balanceId server.Id, want ByteCount) {
	t.Helper()
	assertPayoutDebitTestConsumptionAndDrain(t, ctx, balanceId, want, want)
}

// Require every settled escrow to match one pending journal entry while its
// consumed bytes remain reserved; a partial grant keeps its exact remainder.
func assertPayoutDebitTestConsumptionAndDrain(t testing.TB, ctx context.Context, balanceId server.Id, initial, consumed ByteCount) {
	t.Helper()
	before := readPayoutDebitTestState(t, ctx, balanceId)
	if consumed < 0 || consumed > initial || before.initial != initial || before.credit != initial || before.pendingBytes != consumed || before.settled != consumed ||
		before.pending <= 0 || before.pending != before.escrows || before.applied != 0 || before.invalid != 0 ||
		before.legacy != 0 || before.reserved != consumed {
		t.Fatalf("settled grant did not retain exact asynchronous consumption: state=%+v initial=%d consumed=%d", before, initial, consumed)
	}
	drainPayoutDebitTestPending(t, ctx, balanceId, before)
}

// Check public available credit before writeback, including durable journals
// whose paid contract metadata has already expired. A one-balance public page
// selects the exact grant using its preceding UUID; unrelated pending grants
// remain untouched. Replaying the empty exact worker cannot debit twice.
func drainPayoutDebitTestPending(t testing.TB, ctx context.Context, balanceId server.Id, before payoutDebitTestState) {
	t.Helper()
	if before.pending <= 0 || before.applied != 0 || before.pendingBytes < 0 || before.pendingBytes > before.credit ||
		before.legacy != 0 || before.reserved != before.pendingBytes {
		t.Fatalf("pending debit fixture lacks exact retained consumption: %+v", before)
	}
	wantAvailable := ByteCount(0)
	if before.inWindow {
		wantAvailable = before.credit - before.pendingBytes
	}
	if available := GetActiveTransferBalanceByteCount(ctx, before.networkId); available != wantAvailable {
		t.Fatalf("pending consumed credit became spendable: got=%d want=%d", available, wantAvailable)
	}
	shard := transferDebitShard(balanceId)
	previous := balanceId
	for index := len(previous) - 1; ; index-- {
		if index < 0 {
			t.Fatal("synthetic balance has no preceding UUID")
		}
		if previous[index] != 0 {
			previous[index]--
			break
		}
		previous[index] = 255
	}
	sweeps := readPayoutDebitTestSweeps(t, ctx, balanceId)
	accounts := map[server.Id]contractPayoutTestAmount{
		before.networkId: contractPayoutTestAccountAmount(t, ctx, before.networkId),
	}
	for _, sweep := range sweeps {
		accounts[sweep.networkId] = contractPayoutTestAccountAmount(t, ctx, sweep.networkId)
	}
	state := before
	pageLimit := (before.pending + transferDebitBatchSize - 1) / transferDebitBatchSize
	for page := 0; page < pageLimit; page++ {
		result, err := FlushTransferDebits(ctx, shard, &previous, 1)
		if err != nil || result.Failed != 0 || result.Busy != 0 || result.Balances != 1 || !result.More ||
			result.LastBalanceId == nil || *result.LastBalanceId != balanceId {
			t.Fatalf("public debit page did not retain the exact bounded grant cursor: %+v %v", result, err)
		}
		next := readPayoutDebitTestState(t, ctx, balanceId)
		removed := state.pending - next.pending
		debit := state.pendingBytes - next.pendingBytes
		if removed <= 0 || removed > transferDebitBatchSize || result.Applied != removed || result.Released != removed ||
			next.applied != 0 || next.credit != state.credit-debit || next.reserved != state.reserved-debit ||
			next.legacy != 0 || next.settled != before.settled || next.invalid != before.invalid || next.escrows != before.escrows {
			t.Fatalf("public debit page did not conserve exact consumption: before=%+v after=%+v result=%+v", state, next, result)
		}
		if available := GetActiveTransferBalanceByteCount(ctx, before.networkId); available != wantAvailable {
			t.Fatalf("public debit page changed available credit: got=%d want=%d", available, wantAvailable)
		}
		state = next
	}
	if state.credit != before.credit-before.pendingBytes || state.pending != 0 || state.applied != 0 || state.pendingBytes != 0 || state.reserved != 0 {
		t.Fatalf("public debit pages left consumed credit or liability: %+v", state)
	}
	applied, released, busy, err := flushTransferDebitBalance(ctx, balanceId)
	if err != nil || applied != 0 || released != 0 || busy {
		t.Fatalf("empty exact debit replay changed the journal: applied=%d released=%d busy=%t err=%v", applied, released, busy, err)
	}
	if after := readPayoutDebitTestState(t, ctx, balanceId); after != state {
		t.Fatalf("debit replay changed settled credit: before=%+v after=%+v", state, after)
	}
	if after := readPayoutDebitTestSweeps(t, ctx, balanceId); !slices.Equal(sweeps, after) {
		t.Fatalf("payer debit changed provider allocations: before=%+v after=%+v", sweeps, after)
	}
	for networkId, amount := range accounts {
		if after := contractPayoutTestAccountAmount(t, ctx, networkId); after != amount {
			t.Fatalf("payer debit changed provider account %s: before=%+v after=%+v", networkId, amount, after)
		}
	}
}
