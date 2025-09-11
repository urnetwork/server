package model

import (
	"context"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/session"
)

func GetProductUpdateUserEmailsForUser(ctx context.Context, userId server.Id) (productUpdates bool, userEmails []string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				account_preferences.product_updates
			FROM network
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			WHERE
				network.admin_user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&productUpdates))
			}
		})

		uniqueUserEmails := map[string]server.Id{}

		result, err = conn.Query(
			ctx,
			`
			SELECT
				user_auth,
				'' AS auth_type
			FROM network_user_auth_sso
			WHERE
				user_id = $1

			UNION ALL

			SELECT
				user_auth,
				network_user_auth_password.auth_type
			FROM network_user_auth_password
			WHERE
				user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userAuth string
				var authType UserAuthType
				server.Raise(result.Scan(
					&userAuth,
					&authType,
				))

				if authType == "" {
					_, authType = NormalUserAuthV1(&userAuth)
				}

				if authType == UserAuthTypeEmail {
					uniqueUserEmails[userAuth] = userId
				}
			}
		})

		userEmails = maps.Keys(uniqueUserEmails)
	})

	return
}

func GetUserEmailsForProductUpdatesSync(ctx context.Context) (userEmailUserIds map[string]server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		userEmailUserIds = map[string]server.Id{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_user_auth_sso.user_auth,
				'' AS auth_type,
				network_user_auth_sso.user_id,
				account_preferences.product_updates
			FROM network_user_auth_sso
			INNER JOIN network ON network.admin_user_id = network_user_auth_sso.user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			WHERE
				network_user_auth_sso.product_updates_sync = false

			UNION ALL

			SELECT
				network_user_auth_password.user_auth,
				network_user_auth_password.auth_type,
				network_user_auth_password.user_id,
				account_preferences.product_updates
			FROM network_user_auth_password
			INNER JOIN network ON network.admin_user_id = network_user_auth_password.user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			WHERE
				network_user_auth_password.product_updates_sync = false
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userAuth string
				var authType UserAuthType
				var userId server.Id
				var productUpdates bool
				server.Raise(result.Scan(
					&userAuth,
					&authType,
					&userId,
					&productUpdates,
				))

				if productUpdates {
					if authType == "" {
						_, authType = NormalUserAuthV1(&userAuth)
					}

					if authType == UserAuthTypeEmail {
						userEmailUserIds[userAuth] = userId
					}
				}
			}
		})
	})

	return
}

func SetProductUpdatesSyncForUsers(ctx context.Context, userIdProductUpdatesSync map[server.Id]bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for userId, productUpdatesSync := range userIdProductUpdatesSync {
				batch.Queue(
					`
					UPDATE network_user_auth_sso
					SET
						product_updates_sync = $2
					WHERE
						user_id = $1
					`,
					userId,
					productUpdatesSync,
				)

				batch.Queue(
					`
					UPDATE network_user_auth_password
					SET
						product_updates_sync = $2
					WHERE
						user_id = $1
					`,
					userId,
					productUpdatesSync,
				)
			}
		})
	})
}

func GetNetworkUserEmailsForProductUpdatesSync(ctx context.Context) (userEmailNetworkIds map[string]server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		userEmailNetworkIds = map[string]server.Id{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_user_auth_sso.user_auth,
				'' AS auth_type,
				network.network_id,
				account_preferences.product_updates
			FROM network
			INNER JOIN network_user_auth_sso ON network_user_auth_sso.user_id = network.admin_user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			WHERE
				network.product_updates_sync = false

			UNION ALL

			SELECT
				network_user_auth_password.user_auth,
				network_user_auth_password.auth_type,
				network.network_id,
				account_preferences.product_updates
			FROM network
			INNER JOIN network_user_auth_password ON network_user_auth_password.user_id = network.admin_user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			WHERE
				network.product_updates_sync = false
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userAuth string
				var authType UserAuthType
				var networkId server.Id
				var productUpdates bool
				server.Raise(result.Scan(
					&userAuth,
					&authType,
					&networkId,
					&productUpdates,
				))

				if productUpdates {
					if authType == "" {
						_, authType = NormalUserAuthV1(&userAuth)
					}

					if authType == UserAuthTypeEmail {
						userEmailNetworkIds[userAuth] = networkId
					}
				}
			}
		})
	})

	return
}

func SetNetworkProductUpdatesSyncForUsers(ctx context.Context, networkIdProductUpdatesSync map[server.Id]bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for networkId, productUpdatesSync := range networkIdProductUpdatesSync {
				batch.Queue(
					`
					UPDATE network
					SET
						product_updates_sync = $2
					WHERE
						network_id = $1
					`,
					networkId,
					productUpdatesSync,
				)
			}
		})
	})
}
