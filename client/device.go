package client

import (
	"context"
	"fmt"

	// "net/netip"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/golang/glog"

	"bringyour.com/connect"
	"bringyour.com/protocol"
)

// the device upgrades the api, including setting the client jwt
// closing the device does not close the api

var deviceLog = logFn("device")

type ProvideChangeListener interface {
	ProvideChanged(provideEnabled bool)
}

type ConnectChangeListener interface {
	ConnectChanged(connectEnabled bool)
}

type RouteLocalChangeListener interface {
	RouteLocalChanged(routeLocal bool)
}

// receive a packet into the local raw socket
type ReceivePacket interface {
	ReceivePacket(packet []byte)
}

type deviceSettings struct {
	// time to give up (drop) sending a packet to a destination
	SendTimeout time.Duration
	// ClientDrainTimeout time.Duration
}

func defaultDeviceSettings() *deviceSettings {
	return &deviceSettings{
		SendTimeout: 5 * time.Second,
		// ClientDrainTimeout: 30 * time.Second,
	}
}

type BringYourDevice struct {
	networkSpace *NetworkSpace

	ctx    context.Context
	cancel context.CancelFunc

	byJwt string
	// platformUrl string
	// apiUrl      string

	deviceDescription string
	deviceSpec        string
	appVersion        string

	settings *deviceSettings

	clientId   connect.Id
	instanceId connect.Id

	clientStrategy *connect.ClientStrategy
	// this is the client for provide
	client *connect.Client

	// contractManager *connect.ContractManager
	// routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	stats *DeviceStats

	stateLock sync.Mutex

	// when nil, packets get routed to the local user nat
	remoteUserNatClient connect.UserNatClient

	remoteUserNatProviderLocalUserNat *connect.LocalUserNat
	remoteUserNatProvider             *connect.RemoteUserNatProvider

	routeLocal bool

	openedViewControllers map[ViewController]bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

	provideChangeListeners    *connect.CallbackList[ProvideChangeListener]
	connectChangeListeners    *connect.CallbackList[ConnectChangeListener]
	routeLocalChangeListeners *connect.CallbackList[RouteLocalChangeListener]

	localUserNatUnsub func()
}

// FIXME pass NetworkSpace here instead of API
func NewBringYourDeviceWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
) (*BringYourDevice, error) {
	return traceWithReturnError(
		func() (*BringYourDevice, error) {
			return newBringYourDevice(
				networkSpace,
				byJwt,
				deviceDescription,
				deviceSpec,
				appVersion,
				instanceId,
				defaultDeviceSettings(),
			)
		},
	)
}

func newBringYourDevice(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	settings *deviceSettings,
) (*BringYourDevice, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	// ctx, cancel := api.ctx, api.cancel
	apiUrl := networkSpace.apiUrl
	clientStrategy := networkSpace.clientStrategy

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byJwt, apiUrl)
	client := connect.NewClient(
		ctx,
		clientId,
		clientOob,
		// connect.DefaultClientSettingsNoNetworkEvents(),
		connect.DefaultClientSettings(),
	)

	// routeManager := connect.NewRouteManager(connectClient)
	// contractManager := connect.NewContractManagerWithDefaults(connectClient)
	// connectClient.Setup(routeManager, contractManager)
	// go connectClient.Run()

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId.toConnectId(),
		AppVersion: Version,
	}
	platformTransport := connect.NewPlatformTransportWithDefaults(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
	)

	// go platformTransport.Run(connectClient.RouteManager())

	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	// api := newBringYourApiWithContext(cancelCtx, clientStrategy, apiUrl)
	networkSpace.api.SetByJwt(byJwt)

	byDevice := &BringYourDevice{
		networkSpace: networkSpace,
		ctx:          ctx,
		cancel:       cancel,
		byJwt:        byJwt,
		// apiUrl:            apiUrl,
		deviceDescription: deviceDescription,
		deviceSpec:        deviceSpec,
		appVersion:        appVersion,
		settings:          settings,
		clientId:          clientId,
		instanceId:        instanceId.toConnectId(),
		clientStrategy:    clientStrategy,
		client:            client,
		// contractManager: contractManager,
		// routeManager: routeManager,
		platformTransport:                 platformTransport,
		localUserNat:                      localUserNat,
		stats:                             newDeviceStats(),
		remoteUserNatClient:               nil,
		remoteUserNatProviderLocalUserNat: nil,
		remoteUserNatProvider:             nil,
		routeLocal:                        true,
		openedViewControllers:             map[ViewController]bool{},
		receiveCallbacks:                  connect.NewCallbackList[connect.ReceivePacketFunction](),
		provideChangeListeners:            connect.NewCallbackList[ProvideChangeListener](),
		connectChangeListeners:            connect.NewCallbackList[ConnectChangeListener](),
		routeLocalChangeListeners:         connect.NewCallbackList[RouteLocalChangeListener](),
	}

	// set up with nil destination
	localUserNatUnsub := localUserNat.AddReceivePacketCallback(byDevice.receive)
	byDevice.localUserNatUnsub = localUserNatUnsub

	return byDevice, nil
}

func (self *BringYourDevice) ClientId() *Id {
	// clientId := self.client.ClientId()
	return newId(self.clientId)
}

// func (self *BringYourDevice) client() *connect.Client {
// 	return self.client
// }

func (self *BringYourDevice) GetApi() *BringYourApi {
	return self.networkSpace.GetApi()
}

// func (self *BringYourDevice) SetCustomExtender(extender *Extender) {
// 	extenderIpSecrets := map[netip.Addr]string{}
// 	if extender != nil {
// 		if ip, err := netip.ParseAddr(extender.Ip); err == nil {
// 			extenderIpSecrets[ip] = extender.Secret
// 		}
// 	}
// 	self.clientStrategy.SetCustomExtenders(extenderIpSecrets)
// }

// func (self *BringYourDevice) GetCustomExtender() *Extender {
// 	extenderIpSecrets := self.clientStrategy.CustomExtenders()
// 	extenders := []*Extender{}
// 	for ip, secret := range extenderIpSecrets {
// 		extender := &Extender{
// 			Ip:     ip.String(),
// 			Secret: secret,
// 		}
// 		extenders = append(extenders, extender)
// 	}
// 	if 0 < len(extenders) {
// 		return extenders[0]
// 	}
// 	return nil
// }

// func (self *BringYourDevice) SetCustomExtenderAutoConfigure(extenderAutoConfigure *ExtenderAutoConfigure) {
// 	// FIXME
// }

// func (self *BringYourDevice) GetCustomExtenderAutoConfigure() *ExtenderAutoConfigure {
// 	// FIXME
// 	return nil
// }

func (self *BringYourDevice) GetStats() *DeviceStats {
	return self.stats
}

func (self *BringYourDevice) SetRouteLocal(routeLocal bool) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.routeLocal != routeLocal {
			self.routeLocal = routeLocal
			set = true
		}
	}()
	if set {
		self.routeLocalChanged(routeLocal)
	}
}

func (self *BringYourDevice) GetRouteLocal() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.routeLocal
}

// func (self *BringYourDevice) SetCustomExtenderResolver(extenderResolver *ExtenderResolver) {
// 	// FIXME
// }

// func (self *BringYourDevice) CustomExtenderResolver() *ExtenderResolver {
// 	// FIXME
// 	return nil
// }

// func (self *BringYourDevice) SetRouteLocal(routeLocal bool) {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	self.routeLocal = routeLocal
// }

// func (self *BringYourDevice) GetRouteLocal() bool {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	return self.routeLocal
// }

func (self *BringYourDevice) WindowEvents() *WindowEvents {
	switch v := self.remoteUserNatClient.(type) {
	case *connect.RemoteUserNatMultiClient:
		return newWindowEvents(v.Monitor().Events())
	default:
		return nil
	}
}

func (self *BringYourDevice) AddProvideChangeListener(listener ProvideChangeListener) Sub {
	callbackId := self.provideChangeListeners.Add(listener)
	return newSub(func() {
		self.provideChangeListeners.Remove(callbackId)
	})
}

func (self *BringYourDevice) AddConnectChangeListener(listener ConnectChangeListener) Sub {
	callbackId := self.connectChangeListeners.Add(listener)
	return newSub(func() {
		self.connectChangeListeners.Remove(callbackId)
	})
}

func (self *BringYourDevice) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	callbackId := self.routeLocalChangeListeners.Add(listener)
	return newSub(func() {
		self.routeLocalChangeListeners.Remove(callbackId)
	})
}

func (self *BringYourDevice) provideChanged(provideEnabled bool) {
	for _, listener := range self.provideChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideChanged(provideEnabled)
		})
	}
}

func (self *BringYourDevice) connectChanged(connectEnabled bool) {
	for _, listener := range self.connectChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectChanged(connectEnabled)
		})
	}
}

func (self *BringYourDevice) routeLocalChanged(routeLocal bool) {
	for _, listener := range self.routeLocalChangeListeners.Get() {
		connect.HandleError(func() {
			listener.RouteLocalChanged(routeLocal)
		})
	}
}

// `ReceivePacketFunction`
func (self *BringYourDevice) receive(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
	// deviceLog("GOT A PACKET %d", len(packet))
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		receiveCallback(source, ipProtocol, packet)
	}
}

func (self *BringYourDevice) GetProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatProvider != nil
}

func (self *BringYourDevice) GetConnectEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatClient != nil
}

func (self *BringYourDevice) SetProvideMode(provideMode ProvideMode) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// TODO create a new provider only client?

		provideModes := map[protocol.ProvideMode]bool{}
		if ProvideModePublic <= provideMode {
			provideModes[protocol.ProvideMode_Public] = true
		}
		if ProvideModeFriendsAndFamily <= provideMode {
			provideModes[protocol.ProvideMode_FriendsAndFamily] = true
		}
		if ProvideModeNetwork <= provideMode {
			provideModes[protocol.ProvideMode_Network] = true
		}
		self.client.ContractManager().SetProvideModesWithReturnTraffic(provideModes)

		// recreate the provider user nat
		if self.remoteUserNatProviderLocalUserNat != nil {
			self.remoteUserNatProviderLocalUserNat.Close()
			self.remoteUserNatProviderLocalUserNat = nil
		}
		if self.remoteUserNatProvider != nil {
			self.remoteUserNatProvider.Close()
			self.remoteUserNatProvider = nil
		}

		if ProvideModeNone < provideMode {
			self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNatWithDefaults(self.client.Ctx(), self.clientId.String())
			self.remoteUserNatProvider = connect.NewRemoteUserNatProviderWithDefaults(self.client, self.remoteUserNatProviderLocalUserNat)
		}
	}()
	self.provideChanged(self.GetProvideEnabled())
}

func (self *BringYourDevice) GetProvideMode() ProvideMode {
	maxProvideMode := protocol.ProvideMode_None
	for provideMode, _ := range self.client.ContractManager().GetProvideModes() {
		maxProvideMode = max(maxProvideMode, provideMode)
	}
	return ProvideMode(maxProvideMode)
}

func (self *BringYourDevice) SetProvidePaused(providePaused bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	glog.Infof("[device]provide paused = %t\n", providePaused)

	self.client.ContractManager().SetProvidePaused(providePaused)
}

func (self *BringYourDevice) GetProvidePaused() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.client.ContractManager().IsProvidePaused()
}

func (self *BringYourDevice) RemoveDestination() error {
	return self.SetDestination(nil, ProvideModeNone)
}

func (self *BringYourDevice) SetDestination(specs *ProviderSpecList, provideMode ProvideMode) (returnErr error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.remoteUserNatClient != nil {
			self.remoteUserNatClient.Close()
			self.remoteUserNatClient = nil
		}

		if specs != nil && 0 < specs.Len() {
			connectSpecs := []*connect.ProviderSpec{}
			for i := 0; i < specs.Len(); i += 1 {
				connectSpecs = append(connectSpecs, specs.Get(i).toConnectProviderSpec())
			}

			generator := connect.NewApiMultiClientGenerator(
				self.ctx,
				connectSpecs,
				self.clientStrategy,
				// exclude self
				[]connect.Id{self.clientId},
				self.networkSpace.apiUrl,
				self.byJwt,
				self.networkSpace.platformUrl,
				self.deviceDescription,
				self.deviceSpec,
				self.appVersion,
				// connect.DefaultClientSettingsNoNetworkEvents,
				connect.DefaultClientSettings,
				connect.DefaultApiMultiClientGeneratorSettings(),
			)
			remoteReceive := func(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
				self.stats.UpdateRemoteReceive(ByteCount(len(packet)))
				self.receive(source, ipProtocol, packet)
			}
			self.remoteUserNatClient = connect.NewRemoteUserNatMultiClientWithDefaults(
				self.ctx,
				generator,
				remoteReceive,
				protocol.ProvideMode_Network,
			)
		}
		// else no specs, not an error
	}()
	if returnErr != nil {
		return
	}
	connectEnabled := self.GetConnectEnabled()
	self.stats.UpdateConnect(connectEnabled)
	self.connectChanged(connectEnabled)
	return
}

func (self *BringYourDevice) Shuffle() {
	var remoteUserNatClient connect.UserNatClient
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
	}()

	if remoteUserNatClient != nil {
		remoteUserNatClient.Shuffle()
	}
}

func (self *BringYourDevice) SendPacket(packet []byte, n int32) bool {
	packetCopy := make([]byte, n)
	copy(packetCopy, packet[0:n])
	source := connect.SourceId(self.clientId)

	var remoteUserNatClient connect.UserNatClient
	var localUserNat *connect.LocalUserNat
	var routeLocal bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
		localUserNat = self.localUserNat
		routeLocal = self.routeLocal
	}()

	if remoteUserNatClient != nil {
		self.stats.UpdateRemoteSend(ByteCount(n))
		return remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else if routeLocal {
		// route locally
		return localUserNat.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else {
		return false
	}
}

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(destination connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
		receivePacket.ReceivePacket(packet)
	}
	callbackId := self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(callbackId)
	})
}

func (self *BringYourDevice) openViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.openedViewControllers[vc] = true
}

func (self *BringYourDevice) closeViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.openedViewControllers, vc)
}

func (self *BringYourDevice) OpenConnectViewController() *ConnectViewController {
	vc := newConnectViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenLocationsViewController() *LocationsViewController {
	vm := newLocationsViewController(self.ctx, self)
	self.openViewController(vm)
	return vm
}

func (self *BringYourDevice) OpenConnectViewControllerV0() *ConnectViewControllerV0 {
	vm := newConnectViewControllerV0(self.ctx, self)
	self.openViewController(vm)
	return vm
}

func (self *BringYourDevice) OpenOverlayViewController() *OverlayViewController {
	vc := newOverlayViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenWalletViewController() *WalletViewController {
	vc := newWalletViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenProvideViewController() *ProvideViewController {
	vc := newProvideViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenStatusViewController() *StatusViewController {
	vc := newStatusViewController(self.ctx, self.client)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenDevicesViewController() *DevicesViewController {
	vc := newDevicesViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenAccountViewController() *AccountViewController {
	vc := newAccountViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) CloseViewController(vc ViewController) {
	vc.Close()
	self.closeViewController(vc)
}

func (self *BringYourDevice) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	self.client.Cancel()

	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	}
	// self.localUserNat.RemoveReceivePacketCallback(self.receive)
	self.localUserNatUnsub()
	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.remoteUserNatProvider != nil {
		self.remoteUserNatProvider.Close()
		self.remoteUserNatProvider = nil
	}

	self.localUserNat.Close()

	for vc, _ := range self.openedViewControllers {
		vc.Close()
	}
	clear(self.openedViewControllers)
}

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	gojwt.NewParser().ParseUnverified(byJwt, claims)

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, fmt.Errorf("byJwt does not contain claim client_id")
	}
	switch v := jwtClientId.(type) {
	case string:
		return connect.ParseId(v)
	default:
		return connect.Id{}, fmt.Errorf("byJwt hav invalid type for client_id: %T", v)
	}
}

type WindowEvents struct {
	windowExpandEvent *connect.WindowExpandEvent
	providerEvents    map[connect.Id]*connect.ProviderEvent
}

func newWindowEvents(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
) *WindowEvents {
	return &WindowEvents{
		windowExpandEvent: windowExpandEvent,
		providerEvents:    providerEvents,
	}
}

func (self *WindowEvents) CurrentSize() int {
	return self.windowExpandEvent.CurrentSize
}

func (self *WindowEvents) TargetSize() int {
	return self.windowExpandEvent.TargetSize
}

func (self *WindowEvents) InEvaluationClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateInEvaluation {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) AddedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateAdded {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) NotAddedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateNotAdded {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) EvaluationFailedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateEvaluationFailed {
			count += 1
		}
	}
	return count
}
