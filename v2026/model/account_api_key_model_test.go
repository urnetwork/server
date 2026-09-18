package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
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
