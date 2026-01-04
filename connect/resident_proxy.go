package main

// FIXME have a multi client in the proxy, set proxy as the generator

type ResidentProxyDevice struct {

	// FIXME deviceLocal
}

func CreateResidentProxyDevice() {

	// FIXME create multi client
	// FIXME no local user nat

}

// FIXME
func (self *ResidentProxy) AddTun() (
	send chan []byte,
	receive chan []byte,
	closeTransport func(),
) {
}

type MultiClientGeneratorClientArgs struct {
	ClientId   Id
	ClientAuth *ClientAuth
	P2pOnly    bool
}

func DefaultApiMultiClientGeneratorSettings() *ApiMultiClientGeneratorSettings {
	return &ApiMultiClientGeneratorSettings{}
}

type ApiMultiClientGeneratorSettings struct {
}

type ApiMultiClientGenerator struct {
	specs          []*ProviderSpec
	clientStrategy *ClientStrategy

	excludeClientIds []Id

	apiUrl      string
	byJwt       string
	platformUrl string

	deviceDescription       string
	deviceSpec              string
	appVersion              string
	sourceClientId          *Id
	clientSettingsGenerator func() *ClientSettings
	settings                *ApiMultiClientGeneratorSettings

	api *BringYourApi
}

func NewApiMultiClientGeneratorWithDefaults(
	ctx context.Context,
	specs []*ProviderSpec,
	clientStrategy *ClientStrategy,
	excludeClientIds []Id,
	apiUrl string,
	byJwt string,
	platformUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	sourceClientId *Id,
) *ApiMultiClientGenerator {
	return NewApiMultiClientGenerator(
		ctx,
		specs,
		clientStrategy,
		excludeClientIds,
		apiUrl,
		byJwt,
		platformUrl,
		deviceDescription,
		deviceSpec,
		appVersion,
		sourceClientId,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)
}

func NewApiMultiClientGenerator(
	ctx context.Context,
	specs []*ProviderSpec,
	clientStrategy *ClientStrategy,
	excludeClientIds []Id,
	apiUrl string,
	byJwt string,
	platformUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	sourceClientId *Id,
	clientSettingsGenerator func() *ClientSettings,
	settings *ApiMultiClientGeneratorSettings,
) *ApiMultiClientGenerator {
	api := NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	return &ApiMultiClientGenerator{
		specs:                   specs,
		clientStrategy:          clientStrategy,
		excludeClientIds:        excludeClientIds,
		apiUrl:                  apiUrl,
		byJwt:                   byJwt,
		platformUrl:             platformUrl,
		deviceDescription:       deviceDescription,
		deviceSpec:              deviceSpec,
		appVersion:              appVersion,
		sourceClientId:          sourceClientId,
		clientSettingsGenerator: clientSettingsGenerator,
		settings:                settings,
		api:                     api,
	}
}

func (self *ApiMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	excludeClientIds := slices.Clone(self.excludeClientIds)
	excludeDestinationsIds := [][]Id{}
	for _, excludeDestination := range excludeDestinations {
		excludeDestinationsIds = append(excludeDestinationsIds, excludeDestination.Ids())
	}
	findProviders2 := &FindProviders2Args{
		Specs:               self.specs,
		ExcludeClientIds:    excludeClientIds,
		ExcludeDestinations: excludeDestinationsIds,
		Count:               count,
		RankMode:            rankMode,
	}

	result, err := self.api.FindProviders2Sync(findProviders2)
	if err != nil {
		return nil, err
	}

	destinations := map[MultiHopId]DestinationStats{}
	for _, provider := range result.Providers {
		ids := []Id{}
		if 0 < len(provider.IntermediaryIds) {
			ids = append(ids, provider.IntermediaryIds...)
		}
		ids = append(ids, provider.ClientId)
		// use the tail if the length exceeds the allowed maximum
		if MaxMultihopLength < len(ids) {
			ids = ids[len(ids)-MaxMultihopLength:]
		}
		if destination, err := NewMultiHopId(ids...); err == nil {
			destinations[destination] = DestinationStats{
				EstimatedBytesPerSecond: provider.EstimatedBytesPerSecond,
				Tier:                    provider.Tier,
			}
		}
	}

	return destinations, nil
}

func (self *ApiMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	auth := func() (string, error) {
		// note the derived client id will be inferred by the api jwt
		authNetworkClient := &AuthNetworkClientArgs{
			SourceClientId: self.sourceClientId,
			Description:    self.deviceDescription,
			DeviceSpec:     self.deviceSpec,
		}

		result, err := self.api.AuthNetworkClientSync(authNetworkClient)
		if err != nil {
			return "", err
		}

		if result.Error != nil {
			return "", errors.New(result.Error.Message)
		}

		return result.ByClientJwt, nil
	}

	if byJwtStr, err := auth(); err == nil {
		byJwt, err := ParseByJwtUnverified(byJwtStr)
		if err != nil {
			// in this case we cannot clean up the client because we don't know the client id
			panic(err)
		}

		clientAuth := &ClientAuth{
			ByJwt:      byJwtStr,
			InstanceId: NewId(),
			AppVersion: self.appVersion,
		}
		return &MultiClientGeneratorClientArgs{
			ClientId:   byJwt.ClientId,
			ClientAuth: clientAuth,
		}, nil
	} else {
		return nil, err
	}
}

func (self *ApiMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	removeNetworkClient := &RemoveNetworkClientArgs{
		ClientId: args.ClientId,
	}

	self.api.RemoveNetworkClient(removeNetworkClient, NewApiCallback(func(result *RemoveNetworkClientResult, err error) {
	}))
}

func (self *ApiMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.RemoveClientArgs(args)
}

func (self *ApiMultiClientGenerator) NewClientSettings() *ClientSettings {
	return self.clientSettingsGenerator()
}

func (self *ApiMultiClientGenerator) NewClient(
	ctx context.Context,
	args *MultiClientGeneratorClientArgs,
	clientSettings *ClientSettings,
) (*Client, error) {
	// FIXME how to handle client auth and api?
	byJwt, err := ParseByJwtUnverified(args.ClientAuth.ByJwt)
	if err != nil {
		return nil, err
	}
	clientOob := NewApiOutOfBandControl(ctx, self.clientStrategy, args.ClientAuth.ByJwt, self.apiUrl)
	client := NewClient(ctx, byJwt.ClientId, clientOob, clientSettings)
	settings := DefaultPlatformTransportSettings()
	if args.P2pOnly {
		settings.TransportGenerator = func() (sendTransport Transport, receiveTransport Transport) {
			// only use the platform transport for control
			sendTransport = NewSendClientTransport(DestinationId(ControlId))
			receiveTransport = NewReceiveGatewayTransport()
			return
		}
	}
	// FIXME create a forward route
	NewPlatformTransport(
		client.Ctx(),
		self.clientStrategy,
		client.RouteManager(),
		self.platformUrl,
		args.ClientAuth,
		settings,
	)
	// enable return traffic for this client
	client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		map[protocol.ProvideMode]bool{},
		nil,
	)
	return client, nil
}

func (self *ApiMultiClientGenerator) FixedDestinationSize() (int, bool) {
	specClientIds := []Id{}
	for _, spec := range self.specs {
		if spec.ClientId != nil {
			specClientIds = append(specClientIds, *spec.ClientId)
		}
	}
	// glog.Infof("[multi]eval fixed %d/%d\n", len(specClientIds), len(self.specs))
	return len(specClientIds), len(specClientIds) == len(self.specs)
}

// `connect.ForwardFunction`
func (self *Resident) handleClientForward(path connect.TransferPath, transferFrameBytes []byte) {
	sourceId := server.Id(path.SourceId)
	destinationId := server.Id(path.DestinationId)

	self.UpdateActivity()

	if destinationId == ControlId {
		// the resident client id is `ControlId`. It should never forward to itself.
		panic("Bad forward destination.")
	}

	if sourceId != self.clientId {
		glog.Infof("[rf]abuse not from client (%s<>%s)\n", sourceId, self.clientId)
		// the message is not from the client
		// clients are not allowed to forward from other clients
		// drop
		self.abuseLimiter.delay()
		return
	}

	// FIXME deep packet inspection to look at the contract frames and verify contracts before forwarding

	if self.exchange.settings.ForwardEnforceActiveContracts {
		if !isAck(transferFrameBytes) {
			hasActiveContract := self.residentContractManager.HasActiveContract(sourceId, destinationId)
			if !hasActiveContract {
				glog.Infof("[rf]abuse no active contract %s->%s\n", sourceId, destinationId)
				// there is no active contract
				// drop
				self.abuseLimiter.delay()
				return
			}
		}
	}

	c := func() bool {

		nextForward := func() *ResidentForward {
			forward := NewResidentForward(self.ctx, self.exchange, destinationId)
			go server.HandleError(func() {
				forward.Run()

				glog.V(1).Infof("[rf]close %s->%s\n", sourceId, destinationId)

				// note we don't call close here because only the sender should call close
				forward.Cancel()
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					if currentForward := self.forwards[destinationId]; forward == currentForward {
						delete(self.forwards, destinationId)
					}
				}()
			}, forward.Cancel)
			go server.HandleError(func() {
				defer forward.Cancel()
				for {
					if forward.IsIdle() {
						glog.V(1).Infof("[rf]idle %s->%s\n", sourceId, destinationId)
						return
					}

					select {
					case <-self.ctx.Done():
						return
					case <-forward.Done():
						return
					case <-time.After(self.exchange.settings.ForwardIdleTimeout):
					}
				}
			}, forward.Cancel)

			var replacedForward *ResidentForward
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				replacedForward = self.forwards[destinationId]
				self.forwards[destinationId] = forward
			}()
			if replacedForward != nil {
				replacedForward.Cancel()
			}
			glog.V(1).Infof("[rf]open %s->%s\n", sourceId, destinationId)

			return forward
		}

		limit := false
		var forward *ResidentForward
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			var ok bool
			forward, ok = self.forwards[destinationId]
			if !ok && self.exchange.settings.MaxConcurrentForwardsPerResident <= len(self.forwards) {
				limit = true
			}
		}()

		if forward == nil && limit {
			glog.Infof("[rf]abuse forward limit %s->%s", sourceId, destinationId)
			self.abuseLimiter.delay()
			return false
		}

		if forward == nil || forward.IsDone() {
			forward = nextForward()
		}

		select {
		case <-self.ctx.Done():
			return false
		case <-forward.Done():
			return false
		case forward.send <- transferFrameBytes:
			forward.UpdateActivity()
			return true
		case <-time.After(self.exchange.settings.WriteTimeout):
			glog.V(1).Infof("[rf]drop %s->%s", sourceId, destinationId)
			return false
		}
	}

	if glog.V(2) {
		server.TraceWithReturn(
			fmt.Sprintf("[rf]handle client forward %s->%s", sourceId, destinationId),
			c,
		)
	} else {
		c()
	}
}
