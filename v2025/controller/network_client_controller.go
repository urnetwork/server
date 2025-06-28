package controller

import (
	"context"
	// "strings"

	// "github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/model"
)

func ConnectNetworkClient(
	ctx context.Context,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
) server.Id {
	connectionId := model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	// server.Logger().Printf("Parse client address: %s", clientAddress)

	if ipStr, _, err := server.ParseClientAddress(clientAddress); err == nil {
		go server.HandleError(func() {
			setConnectionLocation(ctx, connectionId, ipStr)
		})
	}
	return connectionId
}

// FIXME only do this when the client is a provider
func setConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	ipStr string,
) {
	location, connectionLocationScores, err := GetLocationForIp(ctx, ipStr)
	if err != nil {
		// server.Logger().Printf("Get ip for location error: %s", err)
		return
	}

	model.CreateLocation(ctx, location)
	model.SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
}
