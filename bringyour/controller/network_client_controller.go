package controller

import (
	"context"
	"strings"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)


func ConnectNetworkClient(
	ctx context.Context,
	clientId bringyour.Id,
	clientAddress string,
) bringyour.Id {
	connectionId := model.ConnectNetworkClient(ctx, clientId, clientAddress)
	parts := strings.Split(clientAddress, ":")
	go bringyour.HandleError(
		func() {setConnectionLocation(ctx, connectionId, parts[0])},
	)
	return connectionId
}


func setConnectionLocation(
	ctx context.Context,
	connectionId bringyour.Id,
	ipStr string,
) {
	if location, err := GetLocationForIp(ctx, ipStr); err == nil {
		model.CreateLocation(ctx, location)
		model.SetConnectionLocation(ctx, connectionId, location.LocationId)
	}
}

