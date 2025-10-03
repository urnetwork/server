package model

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/exp/maps"

	// "github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type FeedbackSendArgs struct {
	Uses      FeedbackSendUses  `json:"uses"`
	Needs     FeedbackSendNeeds `json:"needs"`
	StarCount int               `json:"star_count"`
}
type FeedbackSendUses struct {
	Personal bool `json:"personal"`
	Business bool `json:"business"`
}
type FeedbackSendNeeds struct {
	Private          bool    `json:"private"`
	Safe             bool    `json:"safe"`
	Global           bool    `json:"global"`
	Collaborate      bool    `json:"collaborate"`
	AppControl       bool    `json:"app_control"`
	BlockDataBrokers bool    `json:"block_data_brokers"`
	BlockAds         bool    `json:"block_ads"`
	Focus            bool    `json:"focus"`
	ConnectServers   bool    `json:"connect_servers"`
	RunServers       bool    `json:"run_servers"`
	PreventCyber     bool    `json:"prevent_cyber"`
	Audit            bool    `json:"audit"`
	ZeroTrust        bool    `json:"zero_trust"`
	Visualize        bool    `json:"visualize"`
	Other            *string `json:"other"`
}

type FeedbackSendResult struct {
}

func FeedbackSend(
	feedbackSend FeedbackSendArgs,
	session *session.ClientSession,
) (*FeedbackSendResult, error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		feedbackId := server.NewId()
		_, err := tx.Exec(
			session.Ctx,
			`
				INSERT INTO account_feedback
				(
					feedback_id,
					network_id,
					user_id,
					uses_personal,
					uses_business,
					needs_private,
					needs_safe,
					needs_global,
					needs_collaborate,
					needs_app_control,
					needs_block_data_brokers,
					needs_block_ads,
					needs_focus,
					needs_connect_servers,
					needs_run_servers,
					needs_prevent_cyber,
					needs_audit,
					needs_zero_trust,
					needs_visualize,
					needs_other,
					star_count
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
			`,
			&feedbackId,
			&session.ByJwt.NetworkId,
			&session.ByJwt.UserId,
			feedbackSend.Uses.Personal,
			feedbackSend.Uses.Business,
			feedbackSend.Needs.Private,
			feedbackSend.Needs.Safe,
			feedbackSend.Needs.Global,
			feedbackSend.Needs.Collaborate,
			feedbackSend.Needs.AppControl,
			feedbackSend.Needs.BlockDataBrokers,
			feedbackSend.Needs.BlockAds,
			feedbackSend.Needs.Focus,
			feedbackSend.Needs.ConnectServers,
			feedbackSend.Needs.RunServers,
			feedbackSend.Needs.PreventCyber,
			feedbackSend.Needs.Audit,
			feedbackSend.Needs.ZeroTrust,
			feedbackSend.Needs.Visualize,
			feedbackSend.Needs.Other,
			feedbackSend.StarCount,
		)
		server.Raise(err)
	})

	result := &FeedbackSendResult{}
	return result, nil
}

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
