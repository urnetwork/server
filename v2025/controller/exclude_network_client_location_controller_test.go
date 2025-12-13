package controller

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
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
