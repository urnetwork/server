package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAccountApiKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "testnetwork"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		clientId := server.NewId()
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
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
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
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
