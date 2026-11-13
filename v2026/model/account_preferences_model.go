package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type AccountPreferencesSetArgs struct {
	ProductUpdates bool `json:"product_updates"`
}

type AccountPreferencesSetResult struct {
}

func AccountPreferencesSet(
	preferencesSet *AccountPreferencesSetArgs,
	session *session.ClientSession,
) (*AccountPreferencesSetResult, error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			session.Ctx,
			`
				INSERT INTO account_preferences (network_id, product_updates)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO UPDATE SET product_updates = $2
			`,
			session.ByJwt.NetworkId,
			preferencesSet.ProductUpdates,
		)
		server.Raise(err)
	})

	result := &AccountPreferencesSetResult{}
	return result, nil
}

// AccountPreferencesSetForNetwork writes the product-updates preference for a
// network with no session (a sign-up that still awaits verification has no jwt
// yet). The Brevo sync is scheduled by the controller once the network can act.
func AccountPreferencesSetForNetwork(ctx context.Context, networkId server.Id, productUpdates bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_preferences (network_id, product_updates)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO UPDATE SET product_updates = $2
			`,
			networkId,
			productUpdates,
		))
	})
}

func AccountProductUpdatesSetForEmail(ctx context.Context, userEmail string, productUpdates bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO account_preferences (network_id, product_updates)
			SELECT
				network.network_id,
				$2::boolean AS product_updates
			FROM (
				SELECT
					user_id
				FROM network_user_auth_sso
				WHERE
					user_auth = $1

				UNION ALL

				SELECT
					user_id
				FROM network_user_auth_password
				WHERE
					user_auth = $1
			) t
			INNER JOIN network ON network.admin_user_id = t.user_id
			ON CONFLICT (network_id) DO UPDATE
			SET
				product_updates = $2
			`,
			userEmail,
			productUpdates,
		))
	})

}

// AccountProductUpdatesUnsubscribeForEmail turns product updates off for
// every network the address signs in to, recording the unsubscribe as the
// preference's source (the campaigns' consent evidence).
func AccountProductUpdatesUnsubscribeForEmail(ctx context.Context, userEmail string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO account_preferences (network_id, product_updates, product_updates_source, product_updates_source_time)
			SELECT
				network.network_id,
				false,
				'unsubscribe',
				$2::timestamp
			FROM (
				SELECT user_id FROM network_user_auth_sso WHERE user_auth = $1
				UNION ALL
				SELECT user_id FROM network_user_auth_password WHERE user_auth = $1
			) t
			INNER JOIN network ON network.admin_user_id = t.user_id
			ON CONFLICT (network_id) DO UPDATE
			SET
				product_updates = false,
				product_updates_source = 'unsubscribe',
				product_updates_source_time = $2::timestamp
			`,
			userEmail,
			now,
		))
	})
}

type AccountPreferencesGetResult struct {
	ProductUpdates bool `json:"product_updates"`
}

func AccountPreferencesGet(session *session.ClientSession) *AccountPreferencesGetResult {
	var preferences *AccountPreferencesGetResult
	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
			SELECT
					product_updates
			FROM account_preferences
			WHERE
					network_id = $1
		`,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				preferences = &AccountPreferencesGetResult{}
				server.Raise(result.Scan(
					&preferences.ProductUpdates,
				))
			}
		})
	})
	return preferences
}
