package model

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/exp/maps"

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
		if adminUserId != nil {
			result, err := tx.Query(
				ctx,
				`
				SELECT
					admin_user_id
				FROM network
				WHERE network_id = $1
				`,
				networkId,
			)

			adminMatch := false
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var userId server.Id
					server.Raise(result.Scan(&userId))
					if *adminUserId == userId {
						adminMatch = true
					}
				}
			})

			if !adminMatch {
				return
			}
		}

		userIds := map[server.Id]bool{}
		userAuths = map[string]bool{}

		result, err := tx.Query(
			ctx,
			`
			SELECT
				network.admin_user_id
			FROM network
			WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userId server.Id
				server.Raise(result.Scan(&userId))
				userIds[userId] = true
			}
		})

		server.CreateTempTableInTx(ctx, tx, "temp_user_id(user_id uuid)", maps.Keys(userIds)...)

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
	})
	return
}
