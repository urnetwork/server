package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/connect"
	"bringyour.com/protocol"
)

var deviceLog = logFn("device")

const DebugUseSingleClientConnect = false

type ProvideChangeListener interface {
	ProvideChanged(provideEnabled bool)
}

type ConnectChangeListener interface {
	ConnectChanged(connectEnabled bool)
}

// receive a packet into the local raw socket
type ReceivePacket interface {
	ReceivePacket(packet []byte)
}

// TODO methods to manage extenders

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
	ctx    context.Context
	cancel context.CancelFunc

	byJwt       string
	platformUrl string
	apiUrl      string

	deviceDescription string
	deviceSpec        string
	appVersion        string

	settings *deviceSettings

	clientId   connect.Id
	instanceId connect.Id

	client *connect.Client

	// contractManager *connect.ContractManager
	// routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	stateLock sync.Mutex

	// when nil, packets get routed to the local user nat
	remoteUserNatClient connect.UserNatClient

	remoteUserNatProviderLocalUserNat *connect.LocalUserNat
	remoteUserNatProvider             *connect.RemoteUserNatProvider

	openedViewControllers map[ViewController]bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

	provideChangeListeners *connect.CallbackList[ProvideChangeListener]
	connectChangeListeners *connect.CallbackList[ConnectChangeListener]

	api *BringYourApi

	localUserNatUnsub func()
}

func NewBringYourDeviceWithDefaults(
	byJwt string,
	platformUrl string,
	apiUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
) (*BringYourDevice, error) {
	return newBringYourDevice(
		byJwt,
		platformUrl,
		apiUrl,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		defaultDeviceSettings(),
	)
}

func newBringYourDevice(
	byJwt string,
	platformUrl string,
	apiUrl string,
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

	cancelCtx, cancel := context.WithCancel(context.Background())
	clientOob := connect.NewApiOutOfBandControl(cancelCtx, byJwt, apiUrl)
	client := connect.NewClient(
		cancelCtx,
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
		platformUrl,
		auth,
		client.RouteManager(),
	)

	// go platformTransport.Run(connectClient.RouteManager())

	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	api := newBringYourApiWithContext(cancelCtx, apiUrl)
	api.SetByJwt(byJwt)

	byDevice := &BringYourDevice{
		ctx:               cancelCtx,
		cancel:            cancel,
		byJwt:             byJwt,
		platformUrl:       platformUrl,
		apiUrl:            apiUrl,
		deviceDescription: deviceDescription,
		deviceSpec:        deviceSpec,
		appVersion:        appVersion,
		settings:          settings,
		clientId:          clientId,
		instanceId:        instanceId.toConnectId(),
		client:            client,
		// contractManager: contractManager,
		// routeManager: routeManager,
		platformTransport:                 platformTransport,
		localUserNat:                      localUserNat,
		remoteUserNatClient:               nil,
		remoteUserNatProviderLocalUserNat: nil,
		remoteUserNatProvider:             nil,
		openedViewControllers:             map[ViewController]bool{},
		receiveCallbacks:                  connect.NewCallbackList[connect.ReceivePacketFunction](),
		provideChangeListeners:            connect.NewCallbackList[ProvideChangeListener](),
		connectChangeListeners:            connect.NewCallbackList[ConnectChangeListener](),
		api:                               api,
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

func (self *BringYourDevice) Api() *BringYourApi {
	return self.api
}

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

// `ReceivePacketFunction`
func (self *BringYourDevice) receive(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
	// deviceLog("GOT A PACKET %d", len(packet))
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		receiveCallback(source, ipProtocol, packet)
	}
}

func (self *BringYourDevice) IsProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatProvider != nil
}

func (self *BringYourDevice) IsConnectEnabled() bool {
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
	self.provideChanged(self.IsProvideEnabled())
}

func (self *BringYourDevice) GetProvideMode() ProvideMode {
	maxProvideMode := protocol.ProvideMode_None
	for provideMode, _ := range self.client.ContractManager().GetProvideModes() {
		maxProvideMode = max(maxProvideMode, provideMode)
	}
	return ProvideMode(maxProvideMode)
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

			destinations := []connect.MultiHopId{}
			for _, connectSpec := range connectSpecs {
				if connectSpec.ClientId != nil {
					destinations = append(destinations, connect.RequireMultiHopId(*connectSpec.ClientId))
				}
			}

			// connect to a single client
			// no need to optimize this case, use the simplest user nat client
			if DebugUseSingleClientConnect && len(connectSpecs) == len(destinations) && len(destinations) == 1 {
				self.remoteUserNatClient, returnErr = connect.NewRemoteUserNatClient(
					self.client,
					self.receive,
					destinations,
					protocol.ProvideMode_Network,
				)
				if returnErr != nil {
					return
				}
			} else {
				generator := connect.NewApiMultiClientGenerator(
					connectSpecs,
					[]connect.Id{self.clientId},
					self.apiUrl,
					self.byJwt,
					self.platformUrl,
					self.deviceDescription,
					self.deviceSpec,
					self.appVersion,
					// connect.DefaultClientSettingsNoNetworkEvents,
					connect.DefaultClientSettings,
					connect.DefaultApiMultiClientGeneratorSettings(),
				)
				self.remoteUserNatClient = connect.NewRemoteUserNatMultiClientWithDefaults(
					self.ctx,
					generator,
					self.receive,
				)
			}
		}
		// else no specs, not an error
	}()
	if returnErr != nil {
		return
	}

	self.connectChanged(self.IsConnectEnabled())
	return
}

func (self *BringYourDevice) Shuffle() {
	self.stateLock.Lock()
	remoteUserNatClient := self.remoteUserNatClient
	self.stateLock.Unlock()

	if remoteUserNatClient != nil {
		remoteUserNatClient.Shuffle()
	}
}

func (self *BringYourDevice) SendPacket(packet []byte, n int32) bool {
	packetCopy := make([]byte, n)
	copy(packetCopy, packet[0:n])
	source := connect.SourceId(self.clientId)

	self.stateLock.Lock()
	remoteUserNatClient := self.remoteUserNatClient
	localUserNat := self.localUserNat
	self.stateLock.Unlock()

	if remoteUserNatClient != nil {
		return remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else {
		// route locally
		return localUserNat.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
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

func (self *BringYourDevice) OpenWalletViewController() *WalletViewController {
	vm := newWalletViewController(self.ctx, self)
	self.openViewController(vm)
	return vm
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

func (self *BringYourDevice) OpenOverlayViewController() *OverlayViewController {
	vc := newOverlayViewController(self.ctx, self)
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
