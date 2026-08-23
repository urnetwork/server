package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

// TestSweepDestinationIdDenormalization covers the three parts of the provider-
// payout denormalization: settleEscrowInTx stamps the contract's destination_id
// on the sweep, BackfillSweepDestinationIds repopulates a row that predates the
// column, and the single-table stats query attributes the payout to the
// destination.
func TestSweepDestinationIdDenormalization(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})

		// fund the source network
		balanceCode, err := CreateBalanceCode(ctx, ByteCount(1024*1024), 365*24*time.Hour, UsdToNanoCents(10.00), "", "", "")
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		// settle a contract -> settleEscrowInTx creates a sweep
		usedTransferByteCount := ByteCount(1024)
		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false), nil)
		connect.AssertEqual(t, CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false), nil)

		countWithDestination := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id = $1 AND destination_id = $2`,
					transferEscrow.ContractId, destinationId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}
		countNull := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id = $1 AND destination_id IS NULL`,
					transferEscrow.ContractId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}

		// 1. the write path stamped the contract's destination_id
		if countWithDestination() < 1 {
			t.Fatalf("settleEscrowInTx did not stamp destination_id on the sweep")
		}

		// 2. backfill repopulates a row that predates the column (NULL)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_escrow_sweep SET destination_id = NULL WHERE contract_id = $1`,
				transferEscrow.ContractId,
			))
		})
		if countNull() < 1 {
			t.Fatalf("precondition: sweep should be NULL after the reset")
		}
		BackfillSweepDestinationIds(ctx, 50000)
		if 0 < countNull() {
			t.Fatalf("backfill left NULL destination_id rows")
		}
		if countWithDestination() < 1 {
			t.Fatalf("backfill did not set destination_id")
		}

		// 3. the single-table stats query attributes the payout to the destination
		payout := func(dest server.Id) NanoCents {
			var sum NanoCents
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT s.destination_id, COALESCE(SUM(s.payout_net_revenue_nano_cents), 0)
					FROM transfer_escrow_sweep s
					WHERE s.destination_id = ANY($1::uuid[]) AND s.sweep_time >= $2
					GROUP BY s.destination_id
					`,
					[]server.Id{dest}, server.NowUtc().Add(-time.Hour),
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						var d server.Id
						server.Raise(result.Scan(&d, &sum))
					}
				})
			})
			return sum
		}
		if payout(destinationId) <= 0 {
			t.Fatalf("expected a positive payout for the destination, got %d", payout(destinationId))
		}
		connect.AssertEqual(t, payout(server.NewId()), NanoCents(0))
	})
}
