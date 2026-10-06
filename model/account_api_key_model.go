package model

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The account_api_key.name column is varchar(128) (db_migrations.go), and
// varchar counts characters, not bytes.
const MaxApiKeyNameLength = 128

type CreateApiKeyArgs struct {
	Name string `json:"name"`
}

type CreateApiKeyError struct {
	Message string `json:"message"`
}

type CreateApiKeyResult struct {
	Id     server.Id          `json:"id,omitempty"`
	ApiKey string             `json:"api_key,omitempty"`
	Name   string             `json:"name,omitempty"`
	Error  *CreateApiKeyError `json:"error,omitempty"`
}

// A name the column cannot store is refused before the transaction. A failed
// insert used to be assigned to the error result while the transaction went on
// to commit: postgres turned that commit into a rollback, and server.Tx retried
// it for its whole retry window (a minute) before the call failed. A failed
// statement now raises, which ends the transaction at once.
func CreateApiKey(createApiKey *CreateApiKeyArgs, session *session.ClientSession) (result *CreateApiKeyResult, err error) {
	// the name is client input, so a name the column cannot store is a
	// refusal, not a failed statement
	if MaxApiKeyNameLength < utf8.RuneCountInString(createApiKey.Name) {
		return &CreateApiKeyResult{
			Error: &CreateApiKeyError{
				Message: fmt.Sprintf("Name is too long (limit %d characters).", MaxApiKeyNameLength),
			},
		}, nil
	}
	// postgres text cannot hold a NUL character
	if strings.ContainsRune(createApiKey.Name, 0) {
		return &CreateApiKeyResult{
			Error: &CreateApiKeyError{
				Message: "Name contains a NUL character.",
			},
		}, nil
	}

	var apiKeyId server.Id
	var apiKey string

	server.Tx(session.Ctx, func(tx server.PgTx) {
		apiKeyId = server.NewId()

		var keyErr error
		apiKey, keyErr = generateApiKey()
		if keyErr != nil {
			// nothing has been written in this attempt
			err = keyErr
			return
		}

		apiKeyHash := sha256.Sum256([]byte(apiKey))
		server.RaisePgResult(tx.Exec(
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
			hex.EncodeToString(apiKeyHash[:]),
			createApiKey.Name,
		))

	})

	if err != nil {
		return nil, err
	}

	return &CreateApiKeyResult{
		Id:     apiKeyId,
		ApiKey: apiKey,
		Name:   createApiKey.Name,
	}, nil
}

// A failed delete raises, which ends the transaction at once; the error result
// is always nil. Its error used to be assigned to the result while the
// transaction went on to a commit that server.Tx retried for a minute.
func DeleteApiKey(apiKeyId *server.Id, session *session.ClientSession) error {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				DELETE FROM account_api_key
				WHERE api_key_id = $1 AND network_id = $2
			`,
			apiKeyId,
			session.ByJwt.NetworkId,
		))
	})
	return nil
}

type PublicAccountApiKey struct {
	Id         server.Id `json:"id"`
	Name       string    `json:"name"`
	CreateTime time.Time `json:"create_time"`
}

/**
 * for dashboard listing of API keys
 *
 * A key row that cannot be scanned fails the listing with the scan's error and
 * no keys, which the controller answers with its error message. A failed
 * query raises, as every model read does.
 */
func GetAccountApiKeys(session *session.ClientSession) (apiKeys []*PublicAccountApiKey, err error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		// reset in case the tx is retried on a transient error
		apiKeys = nil
		err = nil

		result, queryErr := tx.Query(
			session.Ctx,
			`
				SELECT api_key_id, name, create_time
				FROM account_api_key
				WHERE network_id = $1
			`,
			session.ByJwt.NetworkId,
		)
		server.Raise(queryErr)
		// not server.WithPgResult: it raises a failed scan, which pgx records
		// as the rows' error, after the callback returns
		defer result.Close()

		for result.Next() {
			var apiKeyId server.Id
			var name string
			var createTime time.Time
			err = result.Scan(&apiKeyId, &name, &createTime)
			if err != nil {
				apiKeys = nil
				return
			}
			apiKeys = append(apiKeys, &PublicAccountApiKey{
				Id:         apiKeyId,
				Name:       name,
				CreateTime: createTime,
			})
		}
		server.Raise(result.Err())
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

func generateApiKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	e := base32.HexEncoding.WithPadding(base32.NoPadding)
	return fmt.Sprintf("urn_%s", e.EncodeToString(b)), nil
}
