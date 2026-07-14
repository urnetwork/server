package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNetworkBlocking(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.BlockedLocations), 1)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.BlockedLocations), 0)

	})
}
