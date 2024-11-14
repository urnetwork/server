package model

import (
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
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
