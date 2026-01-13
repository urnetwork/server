package main

import (
	"context"
	"errors"
	"fmt"
	// "slices"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

type exchangeGenerator struct {
	ctx                     context.Context
	exchange                *Exchange
	byJwt                   *jwt.ByJwt
	specs                   []*model.ProviderSpec
	excludeClientIds        []server.Id
	sourceClientId          server.Id
	clientSettingsGenerator func() *connect.ClientSettings
	settings                *ResidentProxyDeviceSettings
}

func newExchangeGenerator(
	ctx context.Context,
	exchange *Exchange,
	byJwt *jwt.ByJwt,
	connectSpecs []*connect.ProviderSpec,
	excludeClientIds []server.Id,
	sourceClientId server.Id,
	clientSettingsGenerator func() *connect.ClientSettings,
	settings *ResidentProxyDeviceSettings,
) *exchangeGenerator {

	specs := []*model.ProviderSpec{}
	for _, connectSpec := range connectSpecs {
		spec := &model.ProviderSpec{}
		if connectSpec.LocationId != nil {
			locationId := server.Id(*connectSpec.LocationId)
			spec.LocationId = &locationId
		}
		if connectSpec.LocationGroupId != nil {
			locationGroupId := server.Id(*connectSpec.LocationGroupId)
			spec.LocationGroupId = &locationGroupId
		}
		if connectSpec.ClientId != nil {
			clientId := server.Id(*connectSpec.ClientId)
			spec.ClientId = &clientId
		}
		spec.BestAvailable = connectSpec.BestAvailable
		specs = append(specs, spec)
	}

	return &exchangeGenerator{
		ctx:                     ctx,
		exchange:                exchange,
		byJwt:                   byJwt,
		specs:                   specs,
		excludeClientIds:        excludeClientIds,
		sourceClientId:          sourceClientId,
		clientSettingsGenerator: clientSettingsGenerator,
		settings:                settings,
	}
}

func (self *exchangeGenerator) clientSession() *session.ClientSession {
	return session.NewLocalClientSession(self.ctx, "127.0.0.1", self.byJwt)
}

func (self *exchangeGenerator) NextDestinations(count int, excludeDestinations []connect.MultiHopId, rankMode string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	excludeDestinationsIds := [][]server.Id{}
	for _, excludeDestination := range excludeDestinations {
		excludeDestinationIds := []server.Id{}
		for _, id := range excludeDestination.Ids() {
			excludeDestinationIds = append(excludeDestinationIds, server.Id(id))
		}
		excludeDestinationsIds = append(excludeDestinationsIds, excludeDestinationIds)
	}

	findProviders2 := &model.FindProviders2Args{
		Specs:               self.specs,
		ExcludeClientIds:    self.excludeClientIds,
		ExcludeDestinations: excludeDestinationsIds,
		Count:               count,
		RankMode:            rankMode,
	}

	result, err := model.FindProviders2(findProviders2, self.clientSession())
	if err != nil {
		return nil, err
	}

	// FIXME only use single hop since we are already on the exchange

	destinations := map[connect.MultiHopId]connect.DestinationStats{}
	for _, provider := range result.Providers {
		ids := []connect.Id{}
		if 0 < len(provider.IntermediaryIds) {
			for _, id := range provider.IntermediaryIds {
				ids = append(ids, connect.Id(id))
			}
		}
		ids = append(ids, connect.Id(provider.ClientId))
		// use the tail if the length exceeds the allowed maximum
		if connect.MaxMultihopLength < len(ids) {
			ids = ids[len(ids)-connect.MaxMultihopLength:]
		}
		if destination, err := connect.NewMultiHopId(ids...); err == nil {
			destinations[destination] = connect.DestinationStats{
				EstimatedBytesPerSecond: provider.EstimatedBytesPerSecond,
				Tier:                    provider.Tier,
			}
		}
	}

	return destinations, nil
}

func (self *exchangeGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	byJwtStr, clientId, err := func() (string, server.Id, error) {
		// note the derived client id will be inferred by the api jwt
		authNetworkClient := &model.AuthNetworkClientArgs{
			SourceClientId: &self.sourceClientId,
			Description:    self.settings.ProxyDeviceDescription,
			DeviceSpec:     self.settings.ProxyDeviceSpec,
		}

		result, err := model.AuthNetworkClient(authNetworkClient, self.clientSession())
		if err != nil {
			return "", server.Id{}, err
		}

		if result.Error != nil {
			return "", server.Id{}, errors.New(result.Error.Message)
		}

		return *result.ByClientJwt, *result.ClientId, nil
	}()
	if err != nil {
		return nil, err
	}

	// byJwt, err := jwt.ParseByJwtUnverified(self.ctx, byJwtStr)
	// if err != nil {
	// 	// in this case we cannot clean up the client because we don't know the client id
	// 	panic(err)
	// }

	// clientId := *byJwt.ClientId

	clientAuth := &connect.ClientAuth{
		ByJwt:      byJwtStr,
		InstanceId: connect.NewId(),
		AppVersion: server.RequireVersion(),
	}
	return &connect.MultiClientGeneratorClientArgs{
		ClientId:   connect.Id(clientId),
		ClientAuth: clientAuth,
	}, nil
}

func (self *exchangeGenerator) RemoveClientArgs(args *connect.MultiClientGeneratorClientArgs) {
	removeNetworkClient := &model.RemoveNetworkClientArgs{
		ClientId: server.Id(args.ClientId),
	}

	model.RemoveNetworkClient(removeNetworkClient, self.clientSession())
}

func (self *exchangeGenerator) RemoveClientWithArgs(client *connect.Client, args *connect.MultiClientGeneratorClientArgs) {
	self.RemoveClientArgs(args)
}

func (self *exchangeGenerator) NewClientSettings() *connect.ClientSettings {
	return self.clientSettingsGenerator()
}

func (self *exchangeGenerator) NewClient(
	ctx context.Context,
	args *connect.MultiClientGeneratorClientArgs,
	clientSettings *connect.ClientSettings,
) (*connect.Client, error) {
	clientOob := newExchangeOutOfBandControl(ctx, server.Id(args.ClientId))
	client := connect.NewClient(ctx, args.ClientId, clientOob, clientSettings)

	transport := NewResidentTransport(
		client.Ctx(),
		self.exchange,
		server.Id(args.ClientId),
		server.Id(args.ClientAuth.InstanceId),
	)
	go server.HandleError(func() {
		defer client.Cancel()
		transport.Run()
		// close is done in the write
	})
	go server.HandleError(func() {
		defer client.Cancel()
		select {
		case <-client.Done():
		case <-transport.Done():
		}
	})

	// the platform can route any destination,
	// since every client has a platform transport
	sendTransport := connect.NewSendGatewayTransport()
	receiveTransport := connect.NewReceiveGatewayTransport()

	routeManager := client.RouteManager()

	routeManager.UpdateTransport(sendTransport, []connect.Route{transport.send})
	routeManager.UpdateTransport(receiveTransport, []connect.Route{transport.receive})

	// defer func() {
	// 	self.routeManager.RemoveTransport(sendTransport)
	// 	self.routeManager.RemoveTransport(receiveTransport)
	// }()

	// enable return traffic for this client
	client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		map[protocol.ProvideMode]bool{},
		nil,
	)

	return client, nil
}

func (self *exchangeGenerator) FixedDestinationSize() (int, bool) {
	specClientIds := []server.Id{}
	for _, spec := range self.specs {
		if spec.ClientId != nil {
			specClientIds = append(specClientIds, *spec.ClientId)
		}
	}
	// glog.Infof("[multi]eval fixed %d/%d\n", len(specClientIds), len(self.specs))
	return len(specClientIds), len(specClientIds) == len(self.specs)
}

type exchangeOutOfBandControl struct {
	ctx      context.Context
	clientId server.Id
}

func newExchangeOutOfBandControl(ctx context.Context, clientId server.Id) *exchangeOutOfBandControl {
	return &exchangeOutOfBandControl{
		ctx:      ctx,
		clientId: clientId,
	}
}

func (self *exchangeOutOfBandControl) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	go server.HandleError(func() {
		resultFrames, err := controller.ConnectControlFrames(
			self.ctx,
			self.clientId,
			frames,
		)
		callback(resultFrames, err)
	}, func() {
		callback(nil, fmt.Errorf("Unexpected error"))
	})
}
