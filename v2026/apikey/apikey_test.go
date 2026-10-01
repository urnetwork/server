package apikey_test

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/apikey"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestFetchNetworkByApiKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "testnetwork"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		clientId := server.NewId()
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		// create some api keys
		key, err := apikey.Testing_CreateApiKey(networkId, ctx)
		connect.AssertEqual(t, err, nil)

		// fetch api key
		network := apikey.GetNetworkByApiKey(key.ApiKey, ctx)
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.NetworkId, networkId)
		connect.AssertEqual(t, network.UserId, userId)
		// connect.AssertEqual(t, key1.ApiKeyId, key1Result.Id)

		err = model.DeleteApiKey(&key.Id, userSession)
		connect.AssertEqual(t, err, nil)

		// attempt fetch deleted api key
		keyDeleted := apikey.GetNetworkByApiKey(key.ApiKey, ctx)
		connect.AssertEqual(t, keyDeleted, nil)
	})
}
