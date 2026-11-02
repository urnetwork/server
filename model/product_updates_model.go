package model

import (
	"context"
	"slices"

	"maps"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/session"
)

// GetProductUpdateUserEmailsForUser is what the product-updates sync mirrors
// to Brevo for a user: the preference of the network the user administers,
// whether the network's marketing is suppressed (a bounce or complaint on any
// campaign, a re-engagement sunset or "switched" answer), and the user's email
// logins. A user without a preference row reads as opted out.
func GetProductUpdateUserEmailsForUser(ctx context.Context, userId server.Id) (productUpdates bool, marketingSuppressed bool, userEmails []string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				account_preferences.product_updates,
				account_preferences.marketing_suppressed_at IS NOT NULL
			FROM network
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			WHERE
				network.admin_user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&productUpdates, &marketingSuppressed))
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

		userEmails = slices.Collect(maps.Keys(uniqueUserEmails))
	})

	return
}

// GetNetworkAdminUserIdInTx is the user whose logins the product-updates sync
// mirrors for the network: its admin. ok = false when the network does not
// exist.
func GetNetworkAdminUserIdInTx(tx server.PgTx, ctx context.Context, networkId server.Id) (userId server.Id, ok bool) {
	result, err := tx.Query(
		ctx,
		`
		SELECT admin_user_id
		FROM network
		WHERE network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&userId))
			ok = true
		}
	})
	return
}

// MarketingSuppressedNetworkAdmin is a network whose marketing is suppressed,
// and its admin.
type MarketingSuppressedNetworkAdmin struct {
	NetworkId   server.Id
	AdminUserId server.Id
}

// ListMarketingSuppressedNetworkAdmins pages through the existing networks
// whose marketing is suppressed, in network id order after `after` (nil for
// the first page).
func ListMarketingSuppressedNetworkAdmins(ctx context.Context, after *server.Id, limit int) (admins []*MarketingSuppressedNetworkAdmin) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				account_preferences.network_id,
				network.admin_user_id
			FROM account_preferences
			INNER JOIN network ON network.network_id = account_preferences.network_id
			WHERE
				account_preferences.marketing_suppressed_at IS NOT NULL AND
				($1::uuid IS NULL OR $1::uuid < account_preferences.network_id)
			ORDER BY account_preferences.network_id
			LIMIT $2
			`,
			after,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				admin := &MarketingSuppressedNetworkAdmin{}
				server.Raise(result.Scan(&admin.NetworkId, &admin.AdminUserId))
				admins = append(admins, admin)
			}
		})
	})
	return
}

// GetUserEmailsForProductUpdatesSync are the email logins the initial sync
// still has to add to the network_users list: product updates on and the
// network's marketing not suppressed. A suppressed login stays unsynced, so
// the sync adds it if the suppression is ever lifted.
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
				account_preferences.product_updates AND account_preferences.marketing_suppressed_at IS NULL
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
				account_preferences.product_updates AND account_preferences.marketing_suppressed_at IS NULL
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
				var subscribed bool
				server.Raise(result.Scan(
					&userAuth,
					&authType,
					&userId,
					&subscribed,
				))

				if subscribed {
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

// GetNetworkUserEmailsForProductUpdatesSync are the admin email logins the
// initial sync still has to add to the new_networks list, under the same rule
// as GetUserEmailsForProductUpdatesSync.
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
				account_preferences.product_updates AND account_preferences.marketing_suppressed_at IS NULL
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
				account_preferences.product_updates AND account_preferences.marketing_suppressed_at IS NULL
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
				var subscribed bool
				server.Raise(result.Scan(
					&userAuth,
					&authType,
					&networkId,
					&subscribed,
				))

				if subscribed {
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
