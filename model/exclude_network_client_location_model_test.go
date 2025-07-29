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

		locationId1 := server.NewId()
		locationId2 := server.NewId()

		// block locationId1 for networkIdA
		NetworkBlockLocation(ctx, networkIdA, locationId1)

		// Verify that locationId1 is blocked
		blockedLocations := GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 1)
		assert.Equal(t, blockedLocations[0], locationId1)

		// Try and unblock a location that is not blocked
		NetworkUnblockLocation(ctx, networkIdA, locationId2)
		assert.Equal(t, len(blockedLocations), 1)

		// Add another blocked location
		NetworkBlockLocation(ctx, networkIdA, locationId2)

		// Verify that both locations are blocked
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 2)
		assert.Equal(t, blockedLocations[0], locationId1)
		assert.Equal(t, blockedLocations[1], locationId2)

		// Attempt to block locationId1 again
		NetworkBlockLocation(ctx, networkIdA, locationId1)
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 2)
		assert.Equal(t, blockedLocations[0], locationId1)
		assert.Equal(t, blockedLocations[1], locationId2)

		// Unblock locationId1
		NetworkUnblockLocation(ctx, networkIdA, locationId1)

		// Verify that locationId1 is unblocked
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdA)
		assert.Equal(t, len(blockedLocations), 1)
		assert.Equal(t, blockedLocations[0], locationId2)

		// Check empty
		blockedLocations = GetNetworkBlockedLocations(ctx, networkIdB)
		assert.Equal(t, len(blockedLocations), 0)

	})
}
