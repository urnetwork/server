package controller

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNetworkBlocking(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		location := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Kamakura",
			Region:       "Kanagawa",
			Country:      "Japan",
			CountryCode:  "jp",
		}
		model.CreateLocation(ctx, location)

		/**
		 * block location
		 */
		NetworkBlockLocation(
			&NetworkBlockLocationArgs{
				LocationId: location.LocationId,
			},
			userSession,
		)

		result, err := GetNetworkBlockedLocations(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(result.BlockedLocations), 1)

		/**
		 * unblock location
		 */

		NetworkUnblockLocation(
			&NetworkUnblockLocationArgs{
				LocationId: location.LocationId,
			},
			userSession,
		)

		result, err = GetNetworkBlockedLocations(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(result.BlockedLocations), 0)

	})
}
