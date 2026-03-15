package controller

import (
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

/**
 * Block location
 */

type NetworkBlockLocationArgs struct {
	LocationId server.Id `json:"location_id"`
}

type NetworkBlockLocationResult struct {
	Error *NetworkBlockLocationError `json:"error,omitempty"`
}

type NetworkBlockLocationError struct {
	Message string `json:"message"`
}

func NetworkBlockLocation(
	args *NetworkBlockLocationArgs,
	session *session.ClientSession,
) (*NetworkBlockLocationResult, error) {

	model.NetworkBlockLocation(session.Ctx, session.ByJwt.NetworkId, args.LocationId)

	return &NetworkBlockLocationResult{}, nil
}

/**
 * Unblock location
 */

type NetworkUnblockLocationArgs struct {
	LocationId server.Id `json:"location_id"`
}

type NetworkUnblockLocationResult struct {
	Error *NetworkUnblockLocationError `json:"error,omitempty"`
}

type NetworkUnblockLocationError struct {
	Message string `json:"message"`
}

func NetworkUnblockLocation(
	args *NetworkUnblockLocationArgs,
	session *session.ClientSession,
) (*NetworkUnblockLocationResult, error) {

	model.NetworkUnblockLocation(session.Ctx, session.ByJwt.NetworkId, args.LocationId)

	return &NetworkUnblockLocationResult{}, nil
}

/**
 * Get network blocked locations
 */

type GetNetworkBlockedLocationsResult struct {
	BlockedLocations []model.BlockedLocation          `json:"blocked_locations"`
	Error            *GetNetworkBlockedLocationsError `json:"error,omitempty"`
}

type GetNetworkBlockedLocationsError struct {
	Message string `json:"message"`
}

func GetNetworkBlockedLocations(
	session *session.ClientSession,
) (*GetNetworkBlockedLocationsResult, error) {

	locations := model.GetNetworkBlockedLocations(session.Ctx, session.ByJwt.NetworkId)

	return &GetNetworkBlockedLocationsResult{
		BlockedLocations: locations,
	}, nil

}
