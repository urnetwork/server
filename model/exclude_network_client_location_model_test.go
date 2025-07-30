package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestNetworkLocationBlocking(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()
		networkIdB := server.NewId()

		location1 := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, location1)

		location2 := &Location{
			LocationType: LocationTypeCity,
			City:         "Kamakura",
			Region:       "Kanagawa",
			Country:      "Japan",
			CountryCode:  "jp",
		}
		CreateLocation(ctx, location2)

		// block locationId1 for networkIdA
		NetworkBlockLocation(ctx, networkIdA, location1.LocationId)

		// Verify that locationId1 is blocked
		blockedLocations := GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 1)
		assert.Equal(t, blockedLocations[0].LocationId, location1.LocationId)
		assert.Equal(t, blockedLocations[0].LocationName, location1.City)
		assert.Equal(t, blockedLocations[0].CountryCode, location1.CountryCode)
		assert.Equal(t, blockedLocations[0].LocationType, LocationTypeCity)

		// Try and unblock a location that is not blocked
		NetworkUnblockLocation(ctx, networkIdA, location2.LocationId)
		assert.Equal(t, len(blockedLocations), 1)

		// Add another blocked location
		NetworkBlockLocation(ctx, networkIdA, location2.LocationId)

		// Verify that both locations are blocked
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 2)
		assert.Equal(t, blockedLocations[0].LocationId, location1.LocationId)
		assert.Equal(t, blockedLocations[1].LocationId, location2.LocationId)

		// Attempt to block locationId1 again
		NetworkBlockLocation(ctx, networkIdA, location1.LocationId)
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 2)
		assert.Equal(t, blockedLocations[0].LocationId, location1.LocationId)
		assert.Equal(t, blockedLocations[1].LocationId, location2.LocationId)

		// Unblock locationId1
		NetworkUnblockLocation(ctx, networkIdA, location1.LocationId)

		// Verify that locationId1 is unblocked
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 1)
		assert.Equal(t, blockedLocations[0].LocationId, location2.LocationId)

		// Check empty
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdB)
		assert.Equal(t, len(blockedLocations), 0)

	})
}
