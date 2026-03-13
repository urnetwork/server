package apikey_test

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/apikey"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestFetchNetworkByApiKey(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		server.DefaultTestEnv().Run(func() {

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
			assert.Equal(t, err, nil)

			// fetch api key
			network := apikey.GetNetworkByApiKey(key.ApiKey, ctx)
			assert.NotEqual(t, network, nil)
			assert.Equal(t, network.NetworkId, networkId)
			assert.Equal(t, network.UserId, userId)
			// assert.Equal(t, key1.ApiKeyId, key1Result.Id)

			err = model.DeleteApiKey(&key.Id, userSession)
			assert.Equal(t, err, nil)

			// attempt fetch deleted api key
			keyDeleted := apikey.GetNetworkByApiKey(key.ApiKey, ctx)
			assert.Equal(t, keyDeleted, nil)

		})
	})
}
