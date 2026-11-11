package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// TestSweepDestinationIdDenormalization covers the sweep's provider
// attribution since settlement stopped writing destination_id (2026-11-11,
// BILLING-SPLIT.md): settlement names the provider client in provider_payouts
// and leaves destination_id NULL, BackfillSweepDestinationIds repopulates only
// a sweep without provider_payouts (one that predates them) and leaves the new
// sweep alone, and the provider payout stats attribute both the same payout
// to the destination.
func TestSweepDestinationIdDenormalization(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		testingCreatePaymentClient(ctx, sourceNetworkId, sourceId)
		testingCreatePaymentClient(ctx, destinationNetworkId, destinationId)

		sourceSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})

		// fund the source network
		balanceCode, err := CreateBalanceCode(ctx, ByteCount(1024*1024), 365*24*time.Hour, UsdToNanoCents(10.00), "", "", "")
		connect.AssertEqual(t, err, nil)
		testingRedeemPaymentBalanceCode(t, sourceSession.Ctx, sourceNetworkId, balanceCode.Secret)

		// settle a contract, which writes its sweep
		usedTransferByteCount := ByteCount(1024)
		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false), nil)
		connect.AssertEqual(t, CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false), nil)

		type sweepState struct {
			count           int
			nullDestination int
			destination     int
			attributed      int
			payout          NanoCents
			sweepTime       time.Time
		}
		read := func() (state sweepState) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`
					SELECT
						COUNT(*),
						COUNT(*) FILTER (WHERE destination_id IS NULL),
						COUNT(*) FILTER (WHERE destination_id = $2),
						COUNT(*) FILTER (WHERE provider_payouts @> jsonb_build_array(jsonb_build_object('client_id', $2::uuid))),
						COALESCE(SUM(payout_net_revenue_nano_cents), 0)::bigint,
						COALESCE(MIN(sweep_time), now() AT TIME ZONE 'UTC')
					FROM transfer_escrow_sweep
					WHERE contract_id = $1
					`,
					transferEscrow.ContractId, destinationId,
				).Scan(&state.count, &state.nullDestination, &state.destination, &state.attributed, &state.payout, &state.sweepTime))
			})
			return
		}
		stats := func() map[server.Id]map[string]NanoCents {
			var payouts map[server.Id]map[string]NanoCents
			server.Db(ctx, func(conn server.PgConn) {
				var err error
				payouts, err = queryProviderPayoutStats(ctx, conn, destinationNetworkId, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour))
				server.Raise(err)
			})
			return payouts
		}

		// 1. the sweep names its provider in provider_payouts, not destination_id
		settled := read()
		if settled.count != 1 || settled.nullDestination != 1 || settled.attributed != 1 || settled.payout <= 0 {
			t.Fatalf("settled sweep state %+v, want one sweep with provider_payouts naming the destination and no destination_id", settled)
		}
		want := map[server.Id]map[string]NanoCents{destinationId: {dayKey(settled.sweepTime): settled.payout}}
		gotStats := stats()
		connect.AssertEqual(t, gotStats, want)

		// 2. the backfill leaves a sweep with provider_payouts alone
		BackfillSweepDestinationIds(ctx, 50000)
		if backfilled := read(); backfilled != settled {
			t.Fatalf("backfill changed a sweep with provider_payouts: %+v -> %+v", settled, backfilled)
		}

		// 3. a sweep from before provider_payouts gets the contract's destination,
		// and the stats attribute it as before
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_escrow_sweep SET provider_payouts = NULL WHERE contract_id = $1`,
				transferEscrow.ContractId,
			))
		})
		BackfillSweepDestinationIds(ctx, 50000)
		legacy := read()
		if legacy.count != 1 || legacy.destination != 1 || legacy.attributed != 0 || legacy.payout != settled.payout {
			t.Fatalf("backfilled legacy sweep state %+v, want the destination on the sweep", legacy)
		}
		connect.AssertEqual(t, stats(), want)
	})
}
