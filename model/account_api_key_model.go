package model

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// §7.8: what the key may do, within its creator's reach. Empty is all. Until
	// roles map routes to permissions (TEAM-M6-03) a narrower scope cannot be
	// honored, so only all is accepted.
	Scopes []string `json:"scopes,omitempty"`
	// §7.8: the key's lifetime, at most 365 days, which is also the lifetime of a
	// key created without one
	DurationSeconds *int64 `json:"duration_seconds,omitempty"`
}

type CreateApiKeyError struct {
	Message string `json:"message"`
}

type CreateApiKeyResult struct {
	Id         server.Id          `json:"id,omitempty"`
	ApiKey     string             `json:"api_key,omitempty"`
	Name       string             `json:"name,omitempty"`
	Scopes     []string           `json:"scopes,omitempty"`
	ExpireTime *time.Time         `json:"expire_time,omitempty"`
	Error      *CreateApiKeyError `json:"error,omitempty"`
}

// §7.8's scopes, shared with OAuth clients. A key acts as its creator within
// them.
const (
	ApiKeyScopeAll         = "all"
	ApiKeyScopeDevicesCore = "devices:core"
	ApiKeyScopeAuthKeys    = "auth_keys"
	ApiKeyScopePolicyRead  = "policy:read"
	ApiKeyScopeLogsRead    = "logs:read"
)

// The scopes of a new key: all, when none is asked. A scope §7.8 names but
// nothing can honor yet is refused rather than granted as all.
func apiKeyScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{ApiKeyScopeAll}, nil
	}
	for _, scope := range scopes {
		switch scope {
		case ApiKeyScopeAll:
		case ApiKeyScopeDevicesCore, ApiKeyScopeAuthKeys, ApiKeyScopePolicyRead, ApiKeyScopeLogsRead:
			return nil, fmt.Errorf("The scope %q is not available for API keys yet; use %q.", scope, ApiKeyScopeAll)
		default:
			return nil, errors.New("An API key scope is one of all, devices:core, auth_keys, policy:read and logs:read.")
		}
	}
	return []string{ApiKeyScopeAll}, nil
}

// A name the column cannot store is refused before the transaction. A failed
// insert used to be assigned to the error result while the transaction went on
// to commit: postgres turned that commit into a rollback, and server.Tx retried
// it for its whole retry window (a minute) before the call failed. A failed
// statement now raises, which ends the transaction at once.
//
// A key created owes the API key security notice in the same transaction
// (security_notice_api_key.go), with the key's name and never the key.
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

	// §7.8's scopes, expiry and creator live in MG0's columns; a binary
	// without them makes a legacy key, and refuses to make one that asks for
	// what it cannot store
	var scopes []string
	var expireTime *time.Time
	if server.SidecarMg0Linked() {
		var scopeErr error
		scopes, scopeErr = apiKeyScopes(createApiKey.Scopes)
		if scopeErr != nil {
			return &CreateApiKeyResult{
				Error: &CreateApiKeyError{
					Message: scopeErr.Error(),
				},
			}, nil
		}
		maxLifetime := DefaultNetworkAuthKeySettings().ApiKeyMaxLifetime
		lifetime := maxLifetime
		if createApiKey.DurationSeconds != nil {
			durationSeconds := *createApiKey.DurationSeconds
			if durationSeconds <= 0 || int64(maxLifetime/time.Second) < durationSeconds {
				return &CreateApiKeyResult{
					Error: &CreateApiKeyError{
						Message: fmt.Sprintf("An API key lives at most %d seconds.", int64(maxLifetime/time.Second)),
					},
				}, nil
			}
			lifetime = time.Duration(durationSeconds) * time.Second
		}
		expire := server.NowUtc().Add(lifetime)
		expireTime = &expire
	} else if 0 < len(createApiKey.Scopes) || createApiKey.DurationSeconds != nil {
		return &CreateApiKeyResult{
			Error: &CreateApiKeyError{
				Message: "API key scopes and expiry are not available yet.",
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
		if server.SidecarMg0Linked() {
			scopesJson, jsonErr := json.Marshal(scopes)
			server.Raise(jsonErr)
			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					INSERT INTO account_api_key
					(
						api_key_id,
						network_id,
						api_key,
						name,
						scopes,
						expire_time,
						creator_user_id
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
				`,
				apiKeyId,
				session.ByJwt.NetworkId,
				hex.EncodeToString(apiKeyHash[:]),
				createApiKey.Name,
				string(scopesJson),
				expireTime,
				session.ByJwt.UserId,
			))
		} else {
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
		}

		oweSecurityApiKeyNoticeInTx(
			session.Ctx,
			tx,
			session.ByJwt.NetworkId,
			newSecurityApiKeyChange(session.Ctx, SecurityApiKeyCreated, createApiKey.Name, nil),
		)
		AddAdminEventInTx(
			session.Ctx,
			tx,
			NewSessionAdminEvent(session, AdminEventActionApiKeyCreate, AdminEventTargetApiKey, &apiKeyId),
		)
	})

	if err != nil {
		return nil, err
	}

	return &CreateApiKeyResult{
		Id:         apiKeyId,
		ApiKey:     apiKey,
		Name:       createApiKey.Name,
		Scopes:     scopes,
		ExpireTime: expireTime,
	}, nil
}

// A failed delete raises, which ends the transaction at once; the error result
// is always nil. Its error used to be assigned to the result while the
// transaction went on to a commit that server.Tx retried for a minute.
//
// A key deleted owes the API key security notice in the same transaction
// (security_notice_api_key.go), with the key's name and creation time, read
// before the delete. An id the network has no key for deletes nothing and owes
// nothing.
//
// The key's auth codes die with it (§2.8 api-11), and the network token one
// minted names the key, so it ends with the row (session.ByJwt.ApiKeyId).
func DeleteApiKey(apiKeyId *server.Id, session *session.ClientSession) error {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		watch := watchApiKeyDeleteInTx(session.Ctx, tx, session.ByJwt.NetworkId, apiKeyId)
		tag := server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				DELETE FROM account_api_key
				WHERE api_key_id = $1 AND network_id = $2
			`,
			apiKeyId,
			session.ByJwt.NetworkId,
		))
		if tag.RowsAffected() == 1 {
			watch.noticeInTx(session.Ctx, tx)
			if server.SidecarMg0Linked() {
				removeApiKeyAuthCodesInTx(session.Ctx, tx, *apiKeyId)
			}
			AddAdminEventInTx(
				session.Ctx,
				tx,
				NewSessionAdminEvent(session, AdminEventActionApiKeyDelete, AdminEventTargetApiKey, apiKeyId),
			)
		}
	})
	return nil
}

// Deletes the auth codes that descend from the key, and their roles.
func removeApiKeyAuthCodesInTx(ctx context.Context, tx server.PgTx, apiKeyId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM auth_code_role
			USING auth_code
			WHERE
				auth_code_role.auth_code_id = auth_code.auth_code_id AND
				auth_code.api_key_id = $1
		`,
		apiKeyId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM auth_code WHERE api_key_id = $1`,
		apiKeyId,
	))
}

type PublicAccountApiKey struct {
	Id         server.Id `json:"id"`
	Name       string    `json:"name"`
	CreateTime time.Time `json:"create_time"`
	// §7.8's scopes, expiry and creator; absent on a legacy key, which keeps
	// its creator's whole reach and never expires
	Scopes     []string   `json:"scopes,omitempty"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`
	Creator    *server.Id `json:"creator,omitempty"`
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

		linked := server.SidecarMg0Linked()
		sql := `
			SELECT api_key_id, name, create_time
			FROM account_api_key
			WHERE network_id = $1
		`
		if linked {
			// an expired key is no credential, and the hourly expiry removes it
			sql = `
				SELECT api_key_id, name, create_time, scopes::text, expire_time, creator_user_id
				FROM account_api_key
				WHERE network_id = $1 AND (expire_time IS NULL OR $2 < expire_time)
			`
		}
		args := []any{session.ByJwt.NetworkId}
		if linked {
			args = append(args, server.NowUtc())
		}
		result, queryErr := tx.Query(session.Ctx, sql, args...)
		server.Raise(queryErr)
		// not server.WithPgResult: it raises a failed scan, which pgx records
		// as the rows' error, after the callback returns
		defer result.Close()

		for result.Next() {
			apiKey := &PublicAccountApiKey{}
			if linked {
				var scopesJson *string
				err = result.Scan(&apiKey.Id, &apiKey.Name, &apiKey.CreateTime, &scopesJson, &apiKey.ExpireTime, &apiKey.Creator)
				if err == nil && scopesJson != nil {
					err = json.Unmarshal([]byte(*scopesJson), &apiKey.Scopes)
				}
			} else {
				err = result.Scan(&apiKey.Id, &apiKey.Name, &apiKey.CreateTime)
			}
			if err != nil {
				apiKeys = nil
				return
			}
			apiKeys = append(apiKeys, apiKey)
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

// Removes the API keys that expired before minTime with the auth codes that
// descend from them (§2.8 api-11: a key lives at most 365 days, and its codes
// die with it), a batch to a transaction, for at most the retention run budget.
// A legacy key has no expiry and stays. account_api_key is small and its expiry
// unindexed, so each batch scans it.
func RemoveExpiredApiKeys(ctx context.Context, minTime time.Time) (removedCount int64) {
	if !server.SidecarMg0Linked() {
		return 0
	}
	removedCount, _ = removeRetentionBatches(
		ctx,
		RetentionTableAccountApiKey,
		retentionBatchSize,
		time.Now().Add(retentionRunBudget),
		func(tx server.PgTx) int64 {
			apiKeyIds := []server.Id{}
			result, err := tx.Query(
				ctx,
				`
					SELECT api_key_id
					FROM account_api_key
					WHERE expire_time < $1
					ORDER BY expire_time
					LIMIT $2
					FOR UPDATE SKIP LOCKED
				`,
				minTime.UTC(),
				retentionBatchSize,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var apiKeyId server.Id
					server.Raise(result.Scan(&apiKeyId))
					apiKeyIds = append(apiKeyIds, apiKeyId)
				}
			})
			if len(apiKeyIds) == 0 {
				return 0
			}
			for _, apiKeyId := range apiKeyIds {
				removeApiKeyAuthCodesInTx(ctx, tx, apiKeyId)
			}
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM account_api_key WHERE api_key_id = ANY($1)`,
				apiKeyIds,
			))
			return tag.RowsAffected()
		},
	)
	return
}
