package model

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"maps"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type FindNetworkResult struct {
	NetworkId   server.Id
	NetworkName string
	UserAuths   []string
}

func FindNetworksByName(ctx context.Context, networkName string) ([]*FindNetworkResult, error) {
	findNetworkResults := []*FindNetworkResult{}

	searchResults := networkNameSearch().Around(ctx, networkName, 3)

	if len(searchResults) == 0 {
		return []*FindNetworkResult{}, nil
	}

	server.Db(ctx, func(conn server.PgConn) {
		args := []any{}
		networkIdPlaceholders := []string{}
		for i, searchResult := range searchResults {
			args = append(args, searchResult.ValueId)
			networkIdPlaceholders = append(networkIdPlaceholders, fmt.Sprintf("$%d", i+1))
		}

		result, err := conn.Query(
			ctx,
			`
				SELECT
					network.network_id,
					network.network_name,
					network_user.user_auth
				FROM network
				INNER JOIN network_user ON network_user.user_id = network.admin_user_id
				WHERE network.network_id IN (`+strings.Join(networkIdPlaceholders, ",")+`)
			`,
			args...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				findNetworkResult := &FindNetworkResult{}
				var adminUserAuth string
				server.Raise(result.Scan(
					&findNetworkResult.NetworkId,
					&findNetworkResult.NetworkName,
					&adminUserAuth,
				))
				findNetworkResult.UserAuths = append(findNetworkResult.UserAuths, adminUserAuth)
				findNetworkResults = append(findNetworkResults, findNetworkResult)
			}
		})
	})

	return findNetworkResults, nil
}

func FindNetworksByUserAuth(ctx context.Context, userAuth string) ([]*FindNetworkResult, error) {
	findNetworkResults := []*FindNetworkResult{}

	normalUserAuth, _ := NormalUserAuthV1(&userAuth)

	if normalUserAuth == nil {
		return nil, fmt.Errorf("Bad user auth: %s", userAuth)
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE network_user.user_auth = $1
			`,
			*normalUserAuth,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				findNetworkResult := &FindNetworkResult{}
				server.Raise(result.Scan(
					&findNetworkResult.NetworkId,
					&findNetworkResult.NetworkName,
				))
				findNetworkResult.UserAuths = append(findNetworkResult.UserAuths, *normalUserAuth)
				findNetworkResults = append(findNetworkResults, findNetworkResult)
			}
		})
	})

	return findNetworkResults, nil
}

func RemoveNetwork(
	ctx context.Context,
	networkId server.Id,
	adminUserId *server.Id,
) (success bool, userAuths map[string]bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		// Tx may rerun the callback after a transient database failure. Never let
		// a value produced by an aborted attempt escape a later refusal.
		success = false
		userAuths = nil

		// Serialize deletion against every renewal credit. The matching credit
		// path takes FOR KEY SHARE before it consumes a provider idempotency
		// ledger or payment intent. ReadCommitted below is required: if this
		// waits behind a credit, the active-renewal query must see that credit's
		// commit rather than retain a pre-wait RepeatableRead snapshot.
		networkAdminUserId, networkFound := lockPaymentNetworkForRemoveInTx(tx, ctx, networkId)
		if !networkFound || (adminUserId != nil && *adminUserId != networkAdminUserId) {
			return
		}

		// Controller deletion cancels and closes Stripe renewals first. Keep
		// this invariant in the model too, so direct CLI/model callers fail
		// closed and a Stripe credit that won the row-lock race forces a retry.
		activeStripeRenewal := false
		result, err := tx.Query(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM subscription_renewal
					WHERE network_id = $1
						AND market = $2
						AND now() < end_time
				)
			`,
			networkId,
			SubscriptionMarketStripe,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&activeStripeRenewal))
			}
		})
		if activeStripeRenewal {
			return
		}

		userIds := map[server.Id]bool{networkAdminUserId: true}
		userAuths = map[string]bool{}

		server.CreateTempTableInTx(ctx, tx, "temp_user_id(user_id uuid)", slices.Collect(maps.Keys(userIds))...)

		result, err = tx.Query(
			ctx,
			`
			SELECT
				user_auth
			FROM network_user_auth_password
			INNER JOIN temp_user_id ON temp_user_id.user_id = network_user_auth_password.user_id

			UNION ALL

			SELECT
				user_auth
			FROM network_user_auth_sso
			INNER JOIN temp_user_id ON temp_user_id.user_id = network_user_auth_sso.user_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userAuth string
				server.Raise(result.Scan(&userAuth))
				userAuths[userAuth] = true
			}
		})

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for userId, _ := range userIds {
				batch.Queue(
					`
						DELETE FROM network_user
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_wallet
				batch.Queue(
					`
						DELETE FROM network_user_auth_wallet
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_password
				batch.Queue(
					`
						DELETE FROM network_user_auth_password
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_sso
				batch.Queue(
					`
						DELETE FROM network_user_auth_sso
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_seedphrase
				batch.Queue(
					`
						DELETE FROM network_user_auth_seedphrase
						WHERE user_id = $1
					`,
					userId,
				)
			}
		})

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network
				WHERE network_id = $1
			`,
			networkId,
		))

		networkNameSearch().RemoveInTx(ctx, networkId, tx)

		success = true
	}, server.TxReadCommitted)

	if success {
		// the networks stats series (ComputeStats) replays created/deleted
		// events; creation is recorded in network_model.go, and without this
		// the active-network count only ever grew
		auditNetworkEvent := NewAuditNetworkEvent(AuditEventTypeNetworkDeleted)
		auditNetworkEvent.NetworkId = networkId
		AddAuditEvent(ctx, auditNetworkEvent)
	}
	return
}

// lockPaymentNetworkForRemoveInTx serializes owner deletion against paid
// entitlement and data writers and returns the administrator under that lock.
func lockPaymentNetworkForRemoveInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
) (networkAdminUserId server.Id, found bool) {
	result, err := tx.Query(
		ctx,
		`
			/* payment-network-delete-lock */
			SELECT admin_user_id
			FROM network
			WHERE network_id = $1
			FOR UPDATE
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkAdminUserId))
			found = true
		}
	})
	return
}
