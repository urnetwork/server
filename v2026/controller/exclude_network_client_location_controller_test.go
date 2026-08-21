package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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
