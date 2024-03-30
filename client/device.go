package client


import (
	"context"
	"time"
	"fmt"
	"sync"
	
	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/connect"
	"bringyour.com/protocol"
)


var deviceLog = logFn("device")


const DebugUseSingleClientConnect = false


// receive a packet into the local raw socket
type ReceivePacket interface {
    ReceivePacket(packet []byte)
}


// TODO methods to manage extenders


type deviceSettings struct {
	// time to give up (drop) sending a packet to a destination
	SendTimeout time.Duration
	ClientDrainTimeout time.Duration
}

func defaultDeviceSettings() *deviceSettings {
	return &deviceSettings{
		SendTimeout: 5 * time.Second,
		ClientDrainTimeout: 30 * time.Second,
	}
}


// FIXME call SetProvideModesWithReturnTraffic always
// FIXME read the stored provide mode and restore it


// conforms to `Router`
type BringYourDevice struct {
	ctx context.Context
	cancel context.CancelFunc

	byJwt string
	platformUrl string
	apiUrl string

	deviceDescription string
	deviceSpec string
	appVersion string

	settings *deviceSettings

	clientId connect.Id
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
	remoteUserNatProvider *connect.RemoteUserNatProvider

	openedViewControllers map[ViewController]bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

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

	client := connect.NewClient(
        cancelCtx,
        clientId,
        connect.DefaultClientSettingsNoNetworkEvents(),
        // connect.DefaultClientSettings(),
    )

    // routeManager := connect.NewRouteManager(connectClient)
    // contractManager := connect.NewContractManagerWithDefaults(connectClient)
    // connectClient.Setup(routeManager, contractManager)
    // go connectClient.Run()

    auth := &connect.ClientAuth{
    	ByJwt: byJwt,
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

    localUserNat := connect.NewLocalUserNatWithDefaults(client.Ctx(), clientId.String())

    api := newBringYourApiWithContext(cancelCtx, apiUrl)
    api.SetByJwt(byJwt)

	byDevice := &BringYourDevice{
		ctx: cancelCtx,
		cancel: cancel,
		byJwt: byJwt,
		platformUrl: platformUrl,
		apiUrl: apiUrl,
		deviceDescription: deviceDescription,
		deviceSpec: deviceSpec,
		appVersion: appVersion,
		settings: settings,
		clientId: clientId,
		instanceId: instanceId.toConnectId(),
		client: client,
		// contractManager: contractManager,
		// routeManager: routeManager,
		platformTransport: platformTransport,
		localUserNat: localUserNat,
		remoteUserNatClient: nil,
		remoteUserNatProviderLocalUserNat: nil,
		remoteUserNatProvider: nil,
		openedViewControllers: map[ViewController]bool{},
		receiveCallbacks: connect.NewCallbackList[connect.ReceivePacketFunction](),
		api: api,
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

// `ReceivePacketFunction`
func (self *BringYourDevice) receive(source connect.Path, ipProtocol connect.IpProtocol, packet []byte) {
	// deviceLog("GOT A PACKET %d", len(packet))
    for _, receiveCallback := range self.receiveCallbacks.Get() {
        receiveCallback(source, ipProtocol, packet)
    }
}

func (self *BringYourDevice) SetProvideMode(provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// FIXME create a new provider only client

	provideModes := map[protocol.ProvideMode]bool{}
	if ProvideModePublic <= provideMode {
		provideModes[protocol.ProvideMode_Public] = true
	}
	if ProvideModeNetwork <= provideMode {
		provideModes[protocol.ProvideMode_Network] = true
	}
	self.client.ContractManager().SetProvideModesWithReturnTraffic(provideModes)

	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.remoteUserNatProvider != nil {
		self.remoteUserNatProvider.Close()
		self.remoteUserNatProvider = nil
	}

	if ProvideModePublic <= provideMode {
	    self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNatWithDefaults(self.client.Ctx(), self.clientId.String())
	    self.remoteUserNatProvider = connect.NewRemoteUserNatProviderWithDefaults(self.client, self.remoteUserNatProviderLocalUserNat)
	}
}

func (self *BringYourDevice) RemoveDestination() error {
	return self.SetDestination(nil, ProvideModeNone)
}

func (self *BringYourDevice) SetDestinationPublicClientIds(clientIds *IdList) error {
	specs := NewProviderSpecList()
	for i := 0; i < clientIds.Len(); i += 1 {
		specs.Add(&ProviderSpec{
			ClientId: clientIds.Get(i),
		})
	}
	return self.SetDestination(specs, ProvideModePublic)
}

/*
func (self *BringYourDevice) SetDestinationPublicClientId(clientId *Id) error {
	specs := NewProviderSpecList()
	specs.Add(&ProviderSpec{
		ClientId: clientId,
	})
	return self.SetDestination(specs, ProvideModePublic)
}
*/

func (self *BringYourDevice) SetDestination(specs *ProviderSpecList, provideMode ProvideMode) error {
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

		paths := []connect.Path{}
		for _, connectSpec := range connectSpecs {
			if connectSpec.ClientId != nil {
				paths = append(paths, connect.Path{ClientId: *connectSpec.ClientId})
			}
		}

		// connect to a single client
		// no need to optimize this case, use the simplest user nat client
		if DebugUseSingleClientConnect && len(connectSpecs) == len(paths) && len(paths) == 1 {
			var err error
			self.remoteUserNatClient, err = connect.NewRemoteUserNatClient(
				self.client,
				self.receive,
				paths,
				protocol.ProvideMode_Network,
			)
			if err != nil {
				return err
			}
		} else {
			// FIXME be able to pass client settings here
			generator := connect.NewApiMultiClientGenerator(
				connectSpecs,
				self.apiUrl,
				self.byJwt,
				self.platformUrl,
				self.deviceDescription,
				self.deviceSpec,
				self.appVersion,
				connect.DefaultClientSettingsNoNetworkEvents,
				// connect.DefaultClientSettings,
			)
			self.remoteUserNatClient = connect.NewRemoteUserNatMultiClientWithDefaults(
				self.ctx,
				generator,
				self.receive,
			)
			return nil
		}
	}
	// no specs, not an error
	return nil
}

func (self *BringYourDevice) SendPacket(packet []byte, n int32) {
	packetCopy := make([]byte, n)
	copy(packetCopy, packet[0:n])
	source := connect.Path{ClientId: self.clientId}

	self.stateLock.Lock()
	remoteUserNatClient := self.remoteUserNatClient
	localUserNat := self.localUserNat
	self.stateLock.Unlock()

	if remoteUserNatClient != nil {
		remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else {
		// route locally
		localUserNat.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	}
}

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(destination connect.Path, ipProtocol connect.IpProtocol, packet []byte) {
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

	go func() {
		select {
		case <- time.After(self.settings.ClientDrainTimeout):
		}

		self.client.Close()
	}()
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

