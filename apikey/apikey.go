package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"

	"github.com/urnetwork/server"
)

type NetworkByApiKey struct {
	NetworkId   server.Id
	UserId      server.Id
	NetworkName string
}

func GetNetworkByApiKey(apiKey string, ctx context.Context) *NetworkByApiKey {
	var result *NetworkByApiKey

	server.Db(ctx, func(conn server.PgConn) {

		apiKeyHash := sha256.Sum256([]byte(apiKey))
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
			hex.EncodeToString(apiKeyHash[:]),
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

type CreateApiKeyResult struct {
	Id     server.Id `json:"id,omitempty"`
	ApiKey string    `json:"api_key,omitempty"`
}

func Testing_CreateApiKey(networkId server.Id, ctx context.Context) (result *CreateApiKeyResult, err error) {
	var apiKeyId server.Id
	var apiKey string

	server.Tx(ctx, func(tx server.PgTx) {
		apiKeyId = server.NewId()

		var keyErr error
		apiKey, keyErr = generateApiKey()
		if keyErr != nil {
			err = keyErr
			return
		}

		apiKeyHash := sha256.Sum256([]byte(apiKey))
		_, err = tx.Exec(
			ctx,
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
			networkId,
			hex.EncodeToString(apiKeyHash[:]),
			"testkey",
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

// for testing
func generateApiKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	e := base32.HexEncoding.WithPadding(base32.NoPadding)
	return fmt.Sprintf("urn_%s", e.EncodeToString(b)), nil
}
