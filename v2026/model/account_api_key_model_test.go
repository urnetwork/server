package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
)

func TestAccountApiKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "testnetwork"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		clientId := server.NewId()
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		// create some api keys
		key1Args := CreateApiKeyArgs{
			Name: "key1",
		}
		key1Result, err := CreateApiKey(&key1Args, userSession)
		connect.AssertEqual(t, err, nil)

		key2Args := CreateApiKeyArgs{
			Name: "key2",
		}
		key2Result, err := CreateApiKey(&key2Args, userSession)
		connect.AssertEqual(t, err, nil)

		// list all account api keys
		keys, err := GetAccountApiKeys(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(keys), 2)

		// // delete API key
		err = DeleteApiKey(&key1Result.Id, userSession)
		connect.AssertEqual(t, err, nil)

		// list should now just be 1 key
		keys, err = GetAccountApiKeys(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(keys), 1)

		// the only remaining key should be key2
		connect.AssertEqual(t, keys[0].Name, key2Args.Name)
		connect.AssertEqual(t, keys[0].Id, key2Result.Id)

	})
}

// A key that cannot be read fails the listing with the scan's error, and no
// partial list comes back. The scan error used to be assigned to a variable
// that shadowed the listing's error result, so the listing never returned it:
// the failure escaped as a panic instead, and the controller's error answer
// for a failed listing never ran.
func TestGetAccountApiKeysReturnsScanError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		_, err := CreateApiKey(&CreateApiKeyArgs{Name: "readable"}, userSession)
		connect.AssertEqual(t, err, nil)

		// a key whose null name cannot be scanned into a string
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`ALTER TABLE account_api_key ALTER COLUMN name DROP NOT NULL`,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO account_api_key (api_key_id, network_id, api_key, name)
				VALUES ($1, $2, $3, NULL)
				`,
				server.NewId(),
				networkId,
				"unreadable",
			))
		})

		apiKeys, err, panicValue := func() (listedApiKeys []*PublicAccountApiKey, listErr error, panicValue any) {
			defer func() {
				panicValue = recover()
			}()
			listedApiKeys, listErr = GetAccountApiKeys(userSession)
			return
		}()
		if panicValue != nil {
			t.Fatalf("the scan failure escaped the listing as a panic: %v", panicValue)
		}
		if err == nil {
			t.Fatalf("the scan failure was lost: the listing returned %d keys and no error", len(apiKeys))
		}
		if apiKeys != nil {
			t.Fatalf("the failed listing returned %d keys", len(apiKeys))
		}
	})
}

// A name the column cannot store is refused before the transaction, with the
// endpoint's refusal and nothing created: a name over 128 characters, counted
// in characters as varchar counts them, and a name with a NUL character. Such
// a name used to fail the insert, after which the transaction went on to a
// commit that server.Tx retried for a minute before the call failed. Each call
// here has a deadline far inside that minute, and each refused name is also
// refused with the caller's context already canceled, so no transaction is
// opened for it at all.
func TestCreateApiKeyRefusesNameTheColumnCannotStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		byJwt := &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}

		apiKeyCount := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*) FROM account_api_key WHERE network_id = $1`,
					networkId,
				).Scan(&count))
			})
			return
		}

		tooLongMessage := fmt.Sprintf("Name is too long (limit %d characters).", MaxApiKeyNameLength)
		for _, c := range []struct {
			name    string
			keyName string
			message string
		}{
			{
				name:    "129 ascii characters",
				keyName: strings.Repeat("n", MaxApiKeyNameLength+1),
				message: tooLongMessage,
			},
			{
				name:    "129 two-byte characters",
				keyName: strings.Repeat("é", MaxApiKeyNameLength+1),
				message: tooLongMessage,
			},
			{
				name:    "a nul character",
				keyName: "api\x00key",
				message: "Name contains a NUL character.",
			},
		} {
			callCtx, cancel := context.WithTimeout(ctx, forcedFailureCallTimeout)
			canceledCtx, cancelCanceled := context.WithCancel(ctx)
			cancelCanceled()
			for _, callSession := range []*session.ClientSession{
				session.Testing_CreateClientSession(callCtx, byJwt),
				session.Testing_CreateClientSession(canceledCtx, byJwt),
			} {
				result, err, panicValue := func() (createResult *CreateApiKeyResult, createErr error, panicValue any) {
					defer func() {
						panicValue = recover()
					}()
					createResult, createErr = CreateApiKey(&CreateApiKeyArgs{Name: c.keyName}, callSession)
					return
				}()
				if panicValue != nil || err != nil {
					t.Errorf("%s: the call failed instead of refusing: %v %v", c.name, panicValue, err)
				} else if result.Error == nil || result.Error.Message != c.message || result.ApiKey != "" || result.Id != (server.Id{}) {
					t.Errorf("%s: answered %+v, want the refusal %q", c.name, result, c.message)
				}
			}
			if callCtx.Err() != nil {
				t.Errorf("%s: the call outlasted its deadline", c.name)
			}
			cancel()
		}
		connect.AssertEqual(t, apiKeyCount(), 0)

		// the longest name the column stores, in two-byte characters
		longestName := strings.Repeat("é", MaxApiKeyNameLength)
		userSession := session.Testing_CreateClientSession(ctx, byJwt)
		result, err := CreateApiKey(&CreateApiKeyArgs{Name: longestName}, userSession)
		connect.AssertEqual(t, err, nil)
		if result.Error != nil || result.ApiKey == "" {
			t.Fatalf("the longest name was refused: %+v", result)
		}
		apiKeys, err := GetAccountApiKeys(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(apiKeys), 1)
		connect.AssertEqual(t, apiKeys[0].Name, longestName)
	})
}

// The name limit is the column's own: information_schema reports the
// account_api_key.name length the migrations created.
func TestApiKeyNameLimitMatchesColumn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		var columnLength int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT character_maximum_length
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = 'account_api_key' AND column_name = 'name'
				`,
			).Scan(&columnLength))
		})
		connect.AssertEqual(t, columnLength, MaxApiKeyNameLength)
	})
}

// A failed delete surfaces at once with its own error, and deletes nothing.
// Its error used to be assigned to the result while the transaction went on to
// a commit that server.Tx retried for a minute, after which the call failed
// with "commit unexpectedly resulted in rollback".
func TestDeleteApiKeyStatementFailureSurfacesAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		byJwt := &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}
		userSession := session.Testing_CreateClientSession(ctx, byJwt)

		created, err := CreateApiKey(&CreateApiKeyArgs{Name: "key"}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, created.Error, nil)

		removeFailure := forceStatementFailures(ctx, "account_api_key", "DELETE")
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			DeleteApiKey(&created.Id, session.Testing_CreateClientSession(callCtx, byJwt))
		})
		removeFailure()
		if !isForcedFailure(panicValue, "P0001", "injected failure on DELETE account_api_key") {
			t.Fatalf("the failed delete ended with %v, want the injected failure", panicValue)
		}

		// the key was not deleted, and a later delete works
		apiKeys, err := GetAccountApiKeys(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(apiKeys), 1)
		connect.AssertEqual(t, DeleteApiKey(&created.Id, userSession), nil)
		apiKeys, err = GetAccountApiKeys(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(apiKeys), 0)
	})
}
