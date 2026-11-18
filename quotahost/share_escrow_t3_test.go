//go:build sidecar_pending

package quotahost

// §20.6's money for a cross-network share (owner decision 11; QUOTA-M6-06) on T3: the dialer's network pays
// both directions. ACL-M6-03 moved the branch that picks the payer, and controller's
// TestCreateContractShareEscrowsTheDialerAlone shows it picks the dialer; this shows what the choice costs on
// real rows. The dialer's network is a Team as team_billing_t3_test.go makes one, the target's holds a balance
// of its own, and the forward contract and its companion both escrow from the dialer's balances in
// escrowDrawLess's order, the line last, while the target's balance, its open transfer and its escrow rows stay
// as they were through creation and settlement. FI-13 (d)'s E-a is read on both networks after every step.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// How many escrow rows the network's balances hold, open or settled.
func shareEscrowRowCount(ctx context.Context, networkId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*)
				FROM transfer_escrow
				INNER JOIN transfer_balance ON transfer_balance.balance_id = transfer_escrow.balance_id
				WHERE transfer_balance.network_id = $1
			`,
			networkId,
		).Scan(&count))
	})
	return
}

func TestShareEscrowBillsTheDialerBothDirectionsT3(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetEscrowDrawOrder(true)()
		defer model.Testing_SetTeamPrices(model.Testing_TeamPrices{BaseUsd: 100, SeatUsd: 10, IncludedSeats: 5, TransferTibUsd: 9_000_000})()
		model.Pro().Pro.Data = teamBillingRoom
		model.Pro().Team.CreditLine = teamBillingRoom

		// the dialer's network and its client; the world's provider is the share's target
		world := newTeamBillingWorld(t, ctx)
		dialerNetworkId, dialerId := world.networkId, world.payerId
		targetNetworkId, targetId := world.providerNetworkId, world.providerId
		now := server.NowUtc()
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             targetNetworkId,
			StartTime:             now.Add(-time.Hour),
			EndTime:               now.AddDate(1, 0, 0),
			StartBalanceByteCount: teamBillingRoom,
			BalanceByteCount:      teamBillingRoom,
			NetRevenue:            3_000_000,
		})
		var targetBalanceId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_id FROM transfer_balance WHERE network_id = $1`, targetNetworkId).Scan(&targetBalanceId))
		})
		check := func(what string) {
			t.Helper()
			world.checkCreditLast(t, what)
			if err := world.oracle.CheckCreditLast(ctx, connect.Id(targetNetworkId), server.NowUtc()); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			// the target's network pays nothing, holds nothing and is asked for nothing
			connect.AssertEqual(t, world.balance(targetBalanceId), teamBillingRoom)
			connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, targetNetworkId), model.ByteCount(0))
			connect.AssertEqual(t, shareEscrowRowCount(ctx, targetNetworkId), 0)
			connect.AssertEqual(t, len(t3Tokens(ctx, targetBalanceId)), 0)
		}
		check("before any contract")

		// forward, dialer to target: the grant, then the pack
		forward, err := model.CreateTransferEscrow(ctx, dialerNetworkId, dialerId, targetNetworkId, targetId, 6000)
		if err != nil {
			t.Fatal(err)
		}
		escrowed, _ := world.escrow(forward.ContractId)
		connect.AssertEqual(t, escrowed[world.grantId], teamBillingRoom)
		connect.AssertEqual(t, escrowed[world.packId], 6000-teamBillingRoom)
		connect.AssertEqual(t, escrowed[world.lineId], model.ByteCount(0))
		connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, dialerNetworkId), model.ByteCount(6000))
		check("the forward contract")

		// the companion, target to dialer: the dialer's network again, the pack's rest and then the line
		companion, err := model.CreateCompanionTransferEscrow(ctx, targetNetworkId, targetId, dialerNetworkId, dialerId, 5000, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		escrowed, _ = world.escrow(companion.ContractId)
		packRest := 2*teamBillingRoom - 6000
		connect.AssertEqual(t, escrowed[world.grantId], model.ByteCount(0))
		connect.AssertEqual(t, escrowed[world.packId], packRest)
		connect.AssertEqual(t, escrowed[world.lineId], 5000-packRest)
		connect.AssertEqual(t, escrowed[targetBalanceId], model.ByteCount(0))
		connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, dialerNetworkId), model.ByteCount(11000))
		check("the companion contract")

		// The companion closes first, since it holds the line: reservations return in reverse draw order (E-b).
		// It settles the pack's bytes and 308 of the line's, whose rest returns
		const lineUsed = model.ByteCount(308)
		connect.AssertEqual(t, nil, model.CloseContract(ctx, companion.ContractId, targetId, packRest+lineUsed, false))
		check("the companion, one side closed")
		connect.AssertEqual(t, nil, model.CloseContract(ctx, companion.ContractId, dialerId, packRest+lineUsed, false))
		teamBillingFlushTransferDebits(t, ctx)
		_, paid := world.escrow(companion.ContractId)
		connect.AssertEqual(t, paid[world.packId], packRest)
		connect.AssertEqual(t, paid[world.lineId], lineUsed)
		connect.AssertEqual(t, paid[targetBalanceId], model.ByteCount(0))
		check("the companion settled")

		// the forward contract settles 5000 of its 6000: the pack's last 1000 return
		connect.AssertEqual(t, nil, model.CloseContract(ctx, forward.ContractId, dialerId, 5000, false))
		connect.AssertEqual(t, nil, model.CloseContract(ctx, forward.ContractId, targetId, 5000, false))
		teamBillingFlushTransferDebits(t, ctx)
		_, paid = world.escrow(forward.ContractId)
		connect.AssertEqual(t, paid[world.grantId], teamBillingRoom)
		connect.AssertEqual(t, paid[world.packId], 5000-teamBillingRoom)
		check("the forward contract settled")

		// every byte of both directions came from the dialer's balances
		connect.AssertEqual(t, world.balance(world.grantId), model.ByteCount(0))
		connect.AssertEqual(t, world.balance(world.packId), model.ByteCount(1000))
		connect.AssertEqual(t, world.balance(world.lineId), teamBillingRoom-lineUsed)
		connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, dialerNetworkId), model.ByteCount(0))
		spent := 3*teamBillingRoom - world.balance(world.grantId) - world.balance(world.packId) - world.balance(world.lineId)
		connect.AssertEqual(t, spent, 5000+packRest+lineUsed)
	})
}
