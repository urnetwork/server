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
// campaign, a re-engagement sunset or "switched" answer, an operator's
// suppression), the network's sign-up country (network_onboarding.country,
// "" for a network that never entered the onboarding program), and the user's
// email logins. A user without a preference row reads as opted out.
func GetProductUpdateUserEmailsForUser(ctx context.Context, userId server.Id) (productUpdates bool, marketingSuppressed bool, country string, userEmails []string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				account_preferences.product_updates,
				account_preferences.marketing_suppressed_at IS NOT NULL,
				COALESCE(network_onboarding.country, '')
			FROM network
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			LEFT JOIN network_onboarding ON network_onboarding.network_id = network.network_id
			WHERE
				network.admin_user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&productUpdates, &marketingSuppressed, &country))
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

// ProductUpdatesNetworkAdmin is a network whose admin's logins the
// product-updates sync takes off the lists (its marketing is suppressed, or
// its country gets no marketing), and that admin.
type ProductUpdatesNetworkAdmin struct {
	NetworkId   server.Id
	AdminUserId server.Id
}

// ListMarketingSuppressedNetworkAdmins pages through the existing networks
// whose marketing is suppressed, in network id order after `after` (nil for
// the first page).
func ListMarketingSuppressedNetworkAdmins(ctx context.Context, after *server.Id, limit int) (admins []*ProductUpdatesNetworkAdmin) {
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
				admin := &ProductUpdatesNetworkAdmin{}
				server.Raise(result.Scan(&admin.NetworkId, &admin.AdminUserId))
				admins = append(admins, admin)
			}
		})
	})
	return
}

// ListNoMarketingCountryNetworkAdmins pages through the existing networks
// whose sign-up country (network_onboarding.country) is one of `countries`
// (upper-case alpha-2), in network id order after `after` (nil for the first
// page). One pass reads network_onboarding once, in its key order.
func ListNoMarketingCountryNetworkAdmins(ctx context.Context, countries []string, after *server.Id, limit int) (admins []*ProductUpdatesNetworkAdmin) {
	if len(countries) == 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_onboarding.network_id,
				network.admin_user_id
			FROM network_onboarding
			INNER JOIN network ON network.network_id = network_onboarding.network_id
			WHERE
				network_onboarding.country = ANY($1::text[]) AND
				($2::uuid IS NULL OR $2::uuid < network_onboarding.network_id)
			ORDER BY network_onboarding.network_id
			LIMIT $3
			`,
			countries,
			after,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				admin := &ProductUpdatesNetworkAdmin{}
				server.Raise(result.Scan(&admin.NetworkId, &admin.AdminUserId))
				admins = append(admins, admin)
			}
		})
	})
	return
}

// GetUserEmailsForProductUpdatesSync are the email logins the initial sync
// still has to add to the network_users list: product updates on, the
// network's marketing not suppressed, and its sign-up country not one of
// `noMarketingCountries` (upper-case alpha-2). A login left out stays
// unsynced, so the sync adds it if the suppression is ever lifted or the
// country leaves the list.
func GetUserEmailsForProductUpdatesSync(ctx context.Context, noMarketingCountries []string) (userEmailUserIds map[string]server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		userEmailUserIds = map[string]server.Id{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_user_auth_sso.user_auth,
				'' AS auth_type,
				network_user_auth_sso.user_id,
				account_preferences.product_updates AND
					account_preferences.marketing_suppressed_at IS NULL AND
					NOT COALESCE(network_onboarding.country = ANY($1::text[]), false)
			FROM network_user_auth_sso
			INNER JOIN network ON network.admin_user_id = network_user_auth_sso.user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			LEFT JOIN network_onboarding ON network_onboarding.network_id = network.network_id
			WHERE
				network_user_auth_sso.product_updates_sync = false

			UNION ALL

			SELECT
				network_user_auth_password.user_auth,
				network_user_auth_password.auth_type,
				network_user_auth_password.user_id,
				account_preferences.product_updates AND
					account_preferences.marketing_suppressed_at IS NULL AND
					NOT COALESCE(network_onboarding.country = ANY($1::text[]), false)
			FROM network_user_auth_password
			INNER JOIN network ON network.admin_user_id = network_user_auth_password.user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			LEFT JOIN network_onboarding ON network_onboarding.network_id = network.network_id
			WHERE
				network_user_auth_password.product_updates_sync = false
			`,
			noMarketingCountries,
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
func GetNetworkUserEmailsForProductUpdatesSync(ctx context.Context, noMarketingCountries []string) (userEmailNetworkIds map[string]server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		userEmailNetworkIds = map[string]server.Id{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_user_auth_sso.user_auth,
				'' AS auth_type,
				network.network_id,
				account_preferences.product_updates AND
					account_preferences.marketing_suppressed_at IS NULL AND
					NOT COALESCE(network_onboarding.country = ANY($1::text[]), false)
			FROM network
			INNER JOIN network_user_auth_sso ON network_user_auth_sso.user_id = network.admin_user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			LEFT JOIN network_onboarding ON network_onboarding.network_id = network.network_id
			WHERE
				network.product_updates_sync = false

			UNION ALL

			SELECT
				network_user_auth_password.user_auth,
				network_user_auth_password.auth_type,
				network.network_id,
				account_preferences.product_updates AND
					account_preferences.marketing_suppressed_at IS NULL AND
					NOT COALESCE(network_onboarding.country = ANY($1::text[]), false)
			FROM network
			INNER JOIN network_user_auth_password ON network_user_auth_password.user_id = network.admin_user_id
			INNER JOIN account_preferences ON account_preferences.network_id = network.network_id
			LEFT JOIN network_onboarding ON network_onboarding.network_id = network.network_id
			WHERE
				network.product_updates_sync = false
			`,
			noMarketingCountries,
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
