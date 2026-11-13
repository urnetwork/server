package model

// When the api last accepted a store notification, per store
// (store_notification_flow; controller/store_notification_flow.go).

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The stores whose notifications are tracked: Google Play real-time developer
// notifications and App Store Server Notifications, by their market names.
var StoreNotificationStores = []string{SubscriptionMarketGoogle, SubscriptionMarketApple}

// Moves a store's last accepted notification time forward to acceptedAt; an
// earlier time leaves it. Reuse the caller's PostgreSQL session; nil selects
// the outer acquisition boundary.
func MarkStoreNotificationAcceptedInConn(
	connOwner server.PgConn,
	ctx context.Context,
	store string,
	acceptedAt time.Time,
) {
	// a failed statement raises, which ends the transaction at once
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO store_notification_flow (store, last_accepted_at, update_time)
				VALUES ($1, $2, $3)
				ON CONFLICT (store) DO UPDATE
				SET last_accepted_at = greatest(store_notification_flow.last_accepted_at, EXCLUDED.last_accepted_at),
					update_time = EXCLUDED.update_time
			`,
			store,
			acceptedAt.UTC(),
			server.NowUtc(),
		))
	})
}

// A store's last accepted notification time; ok = false when the store has no
// row. Reuse the caller's PostgreSQL session; nil selects the outer
// acquisition boundary.
func GetStoreNotificationLastAcceptedInConn(
	connOwner server.PgConn,
	ctx context.Context,
	store string,
) (lastAcceptedAt time.Time, ok bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT last_accepted_at FROM store_notification_flow WHERE store = $1`,
			store,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&lastAcceptedAt))
				ok = true
			}
		})
	})
	return
}
