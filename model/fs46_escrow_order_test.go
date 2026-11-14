//go:build sidecar_pending

package model

// FS-46 (§20.9, FI-13 (d); QUOTA-M4-03) on Postgres and Redis, the gate on: a payer holding a data pack
// ending in a year and a team_credit line ending in 30 days, both written straight to transfer_balance, since
// no Team flow exists before M6. After every step FI-13's oracle reads the payer's balances with what open
// contracts and Redis hold of each, and the step reads its own clause:
//
//	E-a  escrow takes the pack first, and the line only past it
//	E-b  a partial settlement keeps bytes on the pack and returns the line's reservation first
//	E-e  the provider is paid for the line's bytes at the line's rate
//	E-d  a lease on the line whose edge is killed leaves no reservation and no debit once its token's
//	     lifetime has passed
//
// E-c, the line's usage reaching Stripe once, waits for QUOTA-M6-03's rows and is QUOTA-M6-06's.
//
// Redis keeps a token's lifetime on its own clock, so a lifetime passing is the test setting the token's
// expiry into the past, which the next operation on the balance then finds, as it would 24 h later.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The payer's balances as FI-13's oracle reads them: what is left of each, which a settlement's payout takes
// and an open contract's escrow does not, and what open contracts and Redis's lease tokens hold of it.
type fs46OracleSource struct {
	ctx context.Context
}

func (self *fs46OracleSource) ReadQuota(ctx context.Context, networkId connect.Id) (*sidecartest.QuotaOracleReading, error) {
	reading := &sidecartest.QuotaOracleReading{}
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			self.ctx,
			`
				SELECT
					transfer_balance.balance_id,
					transfer_balance.balance_byte_count,
					`+escrowDrawCreditSql+`,
					transfer_balance.start_time,
					transfer_balance.end_time,
					COALESCE((
						SELECT SUM(transfer_escrow.balance_byte_count)
						FROM transfer_escrow
						WHERE transfer_escrow.balance_id = transfer_balance.balance_id AND NOT transfer_escrow.settled
					), 0)
				FROM transfer_balance
				WHERE transfer_balance.network_id = $1
				ORDER BY `+escrowDrawCreditSql+`, transfer_balance.end_time, transfer_balance.start_time, transfer_balance.balance_id
			`,
			server.Id(networkId),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var escrowed int64
				balance := &sidecartest.QuotaBalance{}
				server.Raise(rows.Scan(&balanceId, &balance.ByteCount, &balance.Credit, &balance.StartTime, &balance.EndTime, &escrowed))
				balance.BalanceId = connect.Id(balanceId)
				balance.ReservedByteCount = escrowed
				reading.Balances = append(reading.Balances, balance)
			}
		})
	})
	for _, balance := range reading.Balances {
		for token, amount := range testingReadLeaseReservation(self.ctx, server.Id(balance.BalanceId)).tokenAmounts {
			// a contract's own token mirrors the escrow already counted
			if !strings.HasPrefix(token, "lease/") {
				continue
			}
			held, err := strconv.ParseInt(amount, 10, 64)
			if err != nil {
				return nil, err
			}
			balance.ReservedByteCount += held
		}
	}
	return reading, nil
}

// What the provider was paid for the contract's bytes on each balance: bytes and net revenue.
func fs46Payouts(ctx context.Context, contractId server.Id) (byteCounts map[server.Id]ByteCount, revenues map[server.Id]NanoCents) {
	byteCounts = map[server.Id]ByteCount{}
	revenues = map[server.Id]NanoCents{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT balance_id, SUM(payout_byte_count)::bigint, SUM(payout_net_revenue_nano_cents)::bigint
				FROM transfer_escrow_sweep
				WHERE contract_id = $1
				GROUP BY balance_id
			`,
			contractId,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var byteCount ByteCount
				var revenue NanoCents
				server.Raise(rows.Scan(&balanceId, &byteCount, &revenue))
				byteCounts[balanceId] = byteCount
				revenues[balanceId] = revenue
			}
		})
	})
	return
}

func fs46BalanceByteCount(ctx context.Context, balanceId server.Id) (byteCount ByteCount) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id = $1`, balanceId).Scan(&byteCount))
	})
	return
}

func TestFs46EscrowOrder(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEscrowDrawOrder(true)()
		connect.AssertEqual(t, EscrowDrawOrderEnabled(), true)
		oracle := sidecartest.NewQuotaOracle(&fs46OracleSource{ctx: ctx})
		const room = ByteCount(4096)
		// the pack's bytes were sold at 3,000,000 nano cents and the line's at 9,000,000
		payer := newTestingEscrowDrawPayer(t, ctx, room)
		payerNetworkId := connect.Id(payer.payerNetworkId)
		checkCreditLast := func(what string) {
			t.Helper()
			if err := oracle.CheckCreditLast(ctx, payerNetworkId, server.NowUtc()); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		checkCreditLast("before any contract")

		// E-a: a contract within the pack takes none of the line
		small, err := createTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, 1024)
		if err != nil {
			t.Fatal(err)
		}
		escrowed, _ := testingEscrowPayouts(t, ctx, small.ContractId)
		connect.AssertEqual(t, escrowed[payer.packId], ByteCount(1024))
		connect.AssertEqual(t, escrowed[payer.lineId], ByteCount(0))
		checkCreditLast("E-a, a contract within the pack")

		// E-a: one past the pack's rest takes the rest, then the line
		large, err := createTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, 5000)
		if err != nil {
			t.Fatal(err)
		}
		escrowed, _ = testingEscrowPayouts(t, ctx, large.ContractId)
		connect.AssertEqual(t, escrowed[payer.packId], room-1024)
		connect.AssertEqual(t, escrowed[payer.lineId], 5000-(room-1024))
		checkCreditLast("E-a, a contract past the pack")

		// E-b: the large contract settles 3500 of its 5000: the pack keeps its 3072, the line 428 of its
		// 1928, and the line's other 1500 return
		connect.AssertEqual(t, nil, CloseContract(ctx, large.ContractId, payer.payerId, 3500, false))
		checkCreditLast("E-b, one side closed")
		connect.AssertEqual(t, nil, CloseContract(ctx, large.ContractId, payer.providerId, 3500, false))
		_, closed := GetContractClose(ctx, large.ContractId)
		connect.AssertEqual(t, closed, true)
		_, paid := testingEscrowPayouts(t, ctx, large.ContractId)
		connect.AssertEqual(t, paid[payer.packId], room-1024)
		connect.AssertEqual(t, paid[payer.lineId], 3500-(room-1024))
		connect.AssertEqual(t, fs46BalanceByteCount(ctx, payer.lineId), room-paid[payer.lineId])
		// the pack keeps the small contract's 1024, still in escrow
		connect.AssertEqual(t, fs46BalanceByteCount(ctx, payer.packId), ByteCount(1024))
		checkCreditLast("E-b, settled")

		// E-b: the small contract settles 600 of its 1024, all on the pack, whose rest returns
		connect.AssertEqual(t, nil, CloseContract(ctx, small.ContractId, payer.payerId, 600, false))
		connect.AssertEqual(t, nil, CloseContract(ctx, small.ContractId, payer.providerId, 600, false))
		_, paid = testingEscrowPayouts(t, ctx, small.ContractId)
		connect.AssertEqual(t, paid[payer.packId], ByteCount(600))
		connect.AssertEqual(t, fs46BalanceByteCount(ctx, payer.packId), ByteCount(424))
		checkCreditLast("E-b, the pack's contract settled")

		// E-e: the provider's payout for the line's bytes is priced at the line's rate, three times the
		// pack's here
		payoutByteCounts, payoutRevenues := fs46Payouts(ctx, large.ContractId)
		connect.AssertEqual(t, payoutByteCounts[payer.packId], room-1024)
		connect.AssertEqual(t, payoutByteCounts[payer.lineId], 3500-(room-1024))
		// the pack's payout shows the provider's share of a byte's revenue; the line's bytes are paid that
		// share of the line's own rate, whatever the pack's is
		packRevenue := NanoCents(3_000_000) * NanoCents(payoutByteCounts[payer.packId]) / NanoCents(room)
		lineRevenue := NanoCents(9_000_000) * NanoCents(payoutByteCounts[payer.lineId]) / NanoCents(room)
		paidPack := payoutRevenues[payer.packId]
		if paidPack <= 0 || packRevenue < paidPack {
			t.Fatalf("E-e: the pack's %d bytes paid %d of the %d they were sold for", payoutByteCounts[payer.packId], paidPack, packRevenue)
		}
		wantLine := lineRevenue * paidPack / packRevenue
		if got := payoutRevenues[payer.lineId]; got < wantLine-2 || wantLine+2 < got {
			t.Fatalf("E-e: the line's %d bytes paid %d, want %d at the line's rate", payoutByteCounts[payer.lineId], got, wantLine)
		}

		// E-d: an edge leases the pack's rest and then the line, and is killed: no extend, no carve, no post
		redis := NewQuotaRateServerRedis(10 * time.Second)
		leaseId := server.NewId()
		packToken := QuotaLeaseToken(leaseId, 1)
		lineToken := QuotaLeaseToken(leaseId, 2)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, payer.packId, packToken, 424, 1000), ByteCount(424))
		checkCreditLast("E-d, the lease's draw on the pack")
		lineLeft := fs46BalanceByteCount(ctx, payer.lineId)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, payer.lineId, lineToken, lineLeft, 576), ByteCount(576))
		checkCreditLast("E-d, the lease's draw on the line")
		// the tokens' lifetime passes, 24 h and more
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZAddXX(ctx, redisLeaseReservationKeys(payer.packId)[2], testingRedisZ(1, packToken)).Err())
			server.Raise(client.ZAddXX(ctx, redisLeaseReservationKeys(payer.lineId)[2], testingRedisZ(1, lineToken)).Err())
		})
		for _, balanceId := range []server.Id{payer.packId, payer.lineId} {
			probe := QuotaLeaseToken(server.NewId(), 1)
			testingReserveLease(t, ctx, redis, balanceId, probe, 1, 1)
			connect.AssertEqual(t, ReleaseLease(ctx, redis, balanceId, probe, testingLeaseTokenTtl), nil)
			if tokens := testingReadLeaseReservation(ctx, balanceId).tokenAmounts; len(tokens) != 0 {
				t.Fatalf("E-d: tokens past their lifetime: %v", tokens)
			}
		}
		var journaled int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM service_debit_journal WHERE balance_id = ANY($1)`, []server.Id{payer.packId, payer.lineId}).Scan(&journaled))
		})
		connect.AssertEqual(t, journaled, 0)
		connect.AssertEqual(t, fs46BalanceByteCount(ctx, payer.lineId), lineLeft)
		connect.AssertEqual(t, fs46BalanceByteCount(ctx, payer.packId), ByteCount(424))
		checkCreditLast("E-d, after the lifetime")
	})
}
