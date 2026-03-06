package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAccountApiKeys(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

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
		assert.Equal(t, err, nil)

		key2Args := CreateApiKeyArgs{
			Name: "key2",
		}
		key2Result, err := CreateApiKey(&key2Args, userSession)
		assert.Equal(t, err, nil)

		// list all account api keys
		keys, err := GetAccountApiKeys(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(keys), 2)

		// // delete API key
		err = DeleteApiKey(&key1Result.Id, userSession)
		assert.Equal(t, err, nil)

		// list should now just be 1 key
		keys, err = GetAccountApiKeys(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(keys), 1)

		// the only remaining key should be key2
		assert.Equal(t, keys[0].Name, key2Args.Name)
		assert.Equal(t, keys[0].Id, key2Result.Id)

	})
}
