package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func deletedNetworkSource(t testing.TB, ctx context.Context, networkId server.Id) string {
	source := ""
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT source FROM deleted_network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&source))
			}
		})
	})
	return source
}

// The migration's backfill records, once, the networks App Store records name
// whose row is gone: the credit ledger's and the App Store renewals'. A live
// network, the nil network of the pre-token purchases and another market's
// renewal are not recorded, and a deletion already recorded keeps its source.
func TestDeletedNetworkBackfillRecordsGoneAppStoreNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		liveNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, liveNetworkId, "backfilllive", server.NewId())
		ledgerGoneId := server.NewId()
		renewalGoneId := server.NewId()
		stripeGoneId := server.NewId()
		recordedGoneId := server.NewId()

		server.Tx(ctx, func(tx server.PgTx) {
			for i, networkId := range []server.Id{liveNetworkId, ledgerGoneId, {}, recordedGoneId} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO apple_subscription_transaction (transaction_id, notification_uuid, network_id, product_id)
					VALUES ($1, $2, $3, 'supporter_monthly_26')
					`,
					"backfill-"+server.NewId().String()+string(rune('a'+i)),
					server.NewId(),
					networkId,
				))
			}
			// rows left by networks that are gone (AddSubscriptionRenewalInTx
			// refuses a network without a row)
			for _, renewal := range []struct {
				networkId server.Id
				market    SubscriptionMarket
			}{
				{renewalGoneId, SubscriptionMarketApple},
				{stripeGoneId, SubscriptionMarketStripe},
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO subscription_renewal (
						network_id,
						subscription_type,
						start_time,
						end_time,
						net_revenue_nano_cents,
						market,
						transaction_id
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
					`,
					renewal.networkId,
					SubscriptionTypeSupporter,
					now.Add(-24*time.Hour),
					now.Add(24*time.Hour),
					UsdToNanoCents(1),
					renewal.market,
					"backfill-renewal-"+server.NewId().String(),
				))
			}
			RecordNetworkDeletionInTx(tx, ctx, recordedGoneId, NetworkDeletionSourceAccount, now)
		})

		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, server.DeletedNetworkBackfillSql))
			})
		}

		connect.AssertEqual(t, IsNetworkDeleted(ctx, ledgerGoneId), true)
		connect.AssertEqual(t, deletedNetworkSource(t, ctx, ledgerGoneId), NetworkDeletionSourceBackfill)
		connect.AssertEqual(t, IsNetworkDeleted(ctx, renewalGoneId), true)
		connect.AssertEqual(t, deletedNetworkSource(t, ctx, recordedGoneId), NetworkDeletionSourceAccount)
		connect.AssertEqual(t, IsNetworkDeleted(ctx, liveNetworkId), false)
		connect.AssertEqual(t, IsNetworkDeleted(ctx, stripeGoneId), false)
		connect.AssertEqual(t, IsNetworkDeleted(ctx, server.Id{}), false)
	})
}

// An account deletion records itself in its own transaction, and the lag guest
// expiry's deletion does the same; a network that was never deleted has no
// record.
func TestNetworkRemovalRecordsTheDeletion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "deletionrecord", server.NewId())
		connect.AssertEqual(t, IsNetworkDeleted(ctx, networkId), false)

		outcome, _ := RemoveNetworkWithStoreSnapshot(ctx, networkId, nil, &RemoveNetworkStoreSnapshot{})
		connect.AssertEqual(t, outcome, RemoveNetworkRemoved)
		connect.AssertEqual(t, IsNetworkDeleted(ctx, networkId), true)
		connect.AssertEqual(t, deletedNetworkSource(t, ctx, networkId), NetworkDeletionSourceAccount)
	})
}
