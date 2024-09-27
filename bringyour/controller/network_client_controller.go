package controller

import (
	"context"
	// "strings"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)

func ConnectNetworkClient(
	ctx context.Context,
	clientId bringyour.Id,
	clientAddress string,
	handlerId bringyour.Id,
) bringyour.Id {
	connectionId := model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	// bringyour.Logger().Printf("Parse client address: %s", clientAddress)

	if ipStr, _, err := bringyour.ParseClientAddress(clientAddress); err == nil {
		go bringyour.HandleError(func() {
			setConnectionLocation(ctx, connectionId, ipStr)
		})
	}
	return connectionId
}

func setConnectionLocation(
	ctx context.Context,
	connectionId bringyour.Id,
	ipStr string,
) {
	location, connectionLocationScores, err := GetLocationForIp(ctx, ipStr)
	if err != nil {
		bringyour.Logger().Printf("Get ip for location error: %s", err)
		return
	}

	model.CreateLocation(ctx, location)
	model.SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
}
