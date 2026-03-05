package model

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type CreateApiKeyArgs struct {
	Name string `json:"name"`
}

type CreateApiKeyError struct {
	Message string `json:"message"`
}

type CreateApiKeyResult struct {
	Id     server.Id          `json:"id,omitempty"`
	ApiKey string             `json:"api_key,omitempty"`
	Error  *CreateApiKeyError `json:"error,omitempty"`
}

func CreateApiKey(createApiKey *CreateApiKeyArgs, session *session.ClientSession) (result *CreateApiKeyResult, err error) {
	var apiKeyId server.Id
	var apiKey string

	server.Tx(session.Ctx, func(tx server.PgTx) {
		apiKeyId = server.NewId()

		var keyErr error
		apiKey, keyErr = generateApiKey()
		if keyErr != nil {
			err = keyErr
			return
		}

		_, err = tx.Exec(
			session.Ctx,
			`
				INSERT INTO account_api_key
				(
					api_key_id,
					network_id,
					api_key,
					name
				)
				VALUES ($1, $2, $3, $4)
			`,
			apiKeyId,
			session.ByJwt.NetworkId,
			apiKey,
			createApiKey.Name,
		)
	})

	if err != nil {
		return nil, err
	}

	return &CreateApiKeyResult{
		Id:     apiKeyId,
		ApiKey: apiKey,
	}, nil
}

func DeleteApiKey(apiKeyId *server.Id, session *session.ClientSession) (err error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		_, err = tx.Exec(
			session.Ctx,
			`
				DELETE FROM account_api_key
				WHERE api_key_id = $1 AND network_id = $2
			`,
			apiKeyId,
			session.ByJwt.NetworkId,
		)
	})
	return
}

type PublicAccountApiKey struct {
	Id         server.Id `json:"id"`
	Name       string    `json:"name"`
	CreateTime time.Time `json:"create_time"`
}

/**
 * for dashboard listing of API keys
 */
func GetAccountApiKeys(session *session.ClientSession) (apiKeys []*PublicAccountApiKey, err error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			session.Ctx,
			`
				SELECT api_key_id, name, create_time
				FROM account_api_key
				WHERE network_id = $1
			`,
			session.ByJwt.NetworkId,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var apiKeyId server.Id
				var name string
				var createTime time.Time
				err = result.Scan(&apiKeyId, &name, &createTime)
				if err != nil {
					return
				}
				apiKeys = append(apiKeys, &PublicAccountApiKey{
					Id:         apiKeyId,
					Name:       name,
					CreateTime: createTime,
				})
			}
		})
	})
	return
}

type GetApiKeyError struct {
	Message string `json:"message"`
}

type NetworkByApiKey struct {
	NetworkId   server.Id
	UserId      server.Id
	NetworkName string
}

func Test_GetNetworkByApiKey(apiKey string, ctx context.Context) *NetworkByApiKey {
	var result *NetworkByApiKey

	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT
					network.network_id,
					network.admin_user_id,
					network.network_name
				FROM account_api_key
				JOIN network ON network.network_id = account_api_key.network_id
				WHERE account_api_key.api_key = $1
			`,
			apiKey,
		)

		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var r NetworkByApiKey
				server.RaisePgResult(rows.Scan(
					&r.NetworkId,
					&r.UserId,
					&r.NetworkName,
				), err)
				result = &r
			}
		})
	})

	return result
}

func generateApiKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	e := base32.HexEncoding.WithPadding(base32.NoPadding)
	return fmt.Sprintf("urn_%s", e.EncodeToString(b)), nil
}
