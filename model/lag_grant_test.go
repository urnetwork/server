package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A lag.dev guest gets no automatic grant (lag.dev/PLAN.md A1): no daily free
// data, no initial balance from the startup backfill, and no referral data,
// as referee or referrer, nor does it count toward a referrer's bonus. An
// ordinary network beside it is granted as before.
func TestLagGuestsGetNoAutomaticGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		guest := CreateLagGuestNetwork(ctx, now, now.Add(24*time.Hour))
		ordinaryId := server.NewId()
		Testing_CreateNetwork(ctx, ordinaryId, "lag-grant-"+ordinaryId.String()[:8], server.NewId())

		// the startup backfill of an initial balance
		without := FindNetworksWithoutTransferBalance(ctx)
		connect.AssertEqual(t, slices.Contains(without, ordinaryId), true)
		connect.AssertEqual(t, slices.Contains(without, guest.NetworkId), false)

		// the daily free grant
		startTime, endTime := FreeGrantWindow(now)
		added := AddFreeTransferBalanceToAllNetworks(ctx, startTime, endTime, ByteCount(1024))
		connect.AssertEqual(t, added[ordinaryId], ByteCount(1024))
		_, granted := added[guest.NetworkId]
		connect.AssertEqual(t, granted, false)

		// referrals: the guest referred by a referrer that has one ordinary
		// referral besides
		referrerId := server.NewId()
		Testing_CreateNetwork(ctx, referrerId, "lag-referrer-"+referrerId.String()[:8], server.NewId())
		refereeId := server.NewId()
		Testing_CreateNetwork(ctx, refereeId, "lag-referee-"+refereeId.String()[:8], server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			for _, networkId := range []server.Id{guest.NetworkId, refereeId} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_referral (network_id, referral_network_id) VALUES ($1, $2)`,
					networkId,
					referrerId,
				))
			}
		})
		startTime, endTime = ReferralGrantWindow(now)
		added = AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		_, granted = added[guest.NetworkId]
		connect.AssertEqual(t, granted, false)
		connect.AssertEqual(t, added[refereeId], ByteCount(10))
		// the referrer is paid for its ordinary referral alone
		connect.AssertEqual(t, added[referrerId], ByteCount(100)*ByteCount(ReferralBonusCount(1)))

		var guestBalances int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_balance WHERE network_id = $1`, guest.NetworkId).Scan(&guestBalances))
		})
		connect.AssertEqual(t, guestBalances, 0)
	})
}
