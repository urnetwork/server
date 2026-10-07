package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

func TestAccountPreferences(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		// no preferences set
		preferences := AccountPreferencesGet(session)
		connect.AssertEqual(t, preferences, nil)

		// set preferences
		setPreferencesArgs := &AccountPreferencesSetArgs{
			ProductUpdates: true,
		}

		_, err := AccountPreferencesSet(setPreferencesArgs, session)
		connect.AssertEqual(t, err, nil)

		// fetched preferences should equal updated preferences
		preferences = AccountPreferencesGet(session)
		connect.AssertEqual(t, preferences.ProductUpdates, true)

		// update again to false
		setPreferencesArgs = &AccountPreferencesSetArgs{
			ProductUpdates: false,
		}

		_, err = AccountPreferencesSet(setPreferencesArgs, session)
		connect.AssertEqual(t, err, nil)

		// should pass
		preferences = AccountPreferencesGet(session)
		connect.AssertEqual(t, preferences.ProductUpdates, false)

	})
}
