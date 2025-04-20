package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/session"
)

func TestAccountPreferences(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		// no preferences set
		preferences := AccountPreferencesGet(session)
		assert.Equal(t, preferences, nil)

		// set preferences
		setPreferencesArgs := &AccountPreferencesSetArgs{
			ProductUpdates: true,
		}

		_, err := AccountPreferencesSet(setPreferencesArgs, session)
		assert.Equal(t, err, nil)

		// fetched preferences should equal updated preferences
		preferences = AccountPreferencesGet(session)
		assert.Equal(t, preferences.ProductUpdates, true)

		// update again to false
		setPreferencesArgs = &AccountPreferencesSetArgs{
			ProductUpdates: false,
		}

		_, err = AccountPreferencesSet(setPreferencesArgs, session)
		assert.Equal(t, err, nil)

		// should pass
		preferences = AccountPreferencesGet(session)
		assert.Equal(t, preferences.ProductUpdates, false)

	})
}
