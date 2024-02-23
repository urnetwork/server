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


const clientDrainTimeout = 30 * time.Second


// receive a packet into the local raw socket
type ReceivePacket interface {
    ReceivePacket(packet []byte)
}


// TODO methods to manage extenders


// FIXME all client go code should be thread safe
// FIXME state lock


// conforms to `Router`
type BringYourDevice struct {
	ctx context.Context
	cancel context.CancelFunc

	byJwt string
	platformUrl string
	apiUrl string

	clientId connect.Id
	instanceId connect.Id
	connectClient *connect.Client

	// contractManager *connect.ContractManager
	// routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	stateLock sync.Mutex

	// when nil, packets get routed to the local user nat
	remoteUserNatClient *connect.RemoteUserNatClient

	remoteUserNatProvider *connect.RemoteUserNatProvider

	openedViewControllers map[ViewController]bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

	api *BringYourApi
}

func NewBringYourDevice(byJwt string, platformUrl string, apiUrl string, instanceId *Id) (*BringYourDevice, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(context.Background())

	connectClient := connect.NewClientWithDefaults(
        cancelCtx,
        clientId,
    )

    // routeManager := connect.NewRouteManager(connectClient)
    // contractManager := connect.NewContractManagerWithDefaults(connectClient)
    // connectClient.Setup(routeManager, contractManager)
    go connectClient.Run()

    auth := &connect.ClientAuth{
    	ByJwt: byJwt,
    	InstanceId: instanceId.toConnectId(),
    	AppVersion: Version,
    }
    platformTransport := connect.NewPlatformTransportWithDefaults(cancelCtx, platformUrl, auth)

    go platformTransport.Run(connectClient.RouteManager())

    localUserNat := connect.NewLocalUserNatWithDefaults(cancelCtx)

    remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat)

    api := newBringYourApiWithContext(cancelCtx, apiUrl)
    api.SetByJwt(byJwt)

	byDevice := &BringYourDevice{
		ctx: cancelCtx,
		cancel: cancel,
		byJwt: byJwt,
		platformUrl: platformUrl,
		apiUrl: apiUrl,
		clientId: clientId,
		instanceId: instanceId.toConnectId(),
		connectClient: connectClient,
		// contractManager: contractManager,
		// routeManager: routeManager,
		platformTransport: platformTransport,
		localUserNat: localUserNat,
		remoteUserNatClient: nil,
		remoteUserNatProvider: remoteUserNatProvider,
		openedViewControllers: map[ViewController]bool{},
		receiveCallbacks: connect.NewCallbackList[connect.ReceivePacketFunction](),
		api: api,
	}

	// set up with nil destination
	localUserNat.AddReceivePacketCallback(byDevice.receive)

	return byDevice, nil
}

func (self *BringYourDevice) ClientId() *Id {
	clientId := self.connectClient.ClientId()
	return newId(clientId)
}

func (self *BringYourDevice) client() *connect.Client {
	return self.connectClient
}

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
	provideModes := map[protocol.ProvideMode]bool{}
	if ProvideModePublic <= provideMode {
		provideModes[protocol.ProvideMode_Public] = true
	}
	if ProvideModeNetwork <= provideMode {
		provideModes[protocol.ProvideMode_Network] = true
	}
	self.connectClient.ContractManager().SetProvideModes(provideModes)
}

func (self *BringYourDevice) RemoveDestination() error {
	return self.SetDestination(nil, ProvideModeNone)
}

func (self *BringYourDevice) SetDestinationPublicClientId(clientId *Id) error {
	exportedDestinations := NewPathList()
	exportedDestinations.Add(&Path{
		ClientId: clientId,
	})
	return self.SetDestination(exportedDestinations, ProvideModePublic)
}

// `Router` implementation
func (self *BringYourDevice) SetDestination(destinations *PathList, provideMode ProvideMode) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	} else {
		self.localUserNat.RemoveReceivePacketCallback(self.receive)
	}

	if destinations == nil || destinations.Len() == 0 {
		self.localUserNat.AddReceivePacketCallback(self.receive)
		return nil
	} else {
		connectDestinations := []connect.Path{}
		for i := 0; i < destinations.Len(); i += 1 {
			connectDestinations = append(connectDestinations, destinations.Get(i).toConnectPath())
		}
		remoteUserNatClient_, err := connect.NewRemoteUserNatClient(
			self.connectClient,
			self.receive,
			connectDestinations,
			protocol.ProvideMode(provideMode),
		)
		if err != nil {
			return err
		}
		self.remoteUserNatClient = remoteUserNatClient_
		return nil
	}
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
		remoteUserNatClient.SendPacket(source, protocol.ProvideMode_Network, packetCopy)
	} else {
		// route locally
		localUserNat.SendPacket(source, protocol.ProvideMode_Network, packetCopy)
	}
}

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(destination connect.Path, ipProtocol connect.IpProtocol, packet []byte) {
		receivePacket.ReceivePacket(packet)
	}
	self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(receive)
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
	vc := newStatusViewController(self.ctx, self.connectClient)
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

	self.connectClient.Cancel()

	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	} else {
		self.localUserNat.RemoveReceivePacketCallback(self.receive)
	}

	self.remoteUserNatProvider.Close()

	self.localUserNat.Close()

	for vc, _ := range self.openedViewControllers {
		vc.Close()
	}
	clear(self.openedViewControllers)

	go func() {
		select {
		case <- time.After(clientDrainTimeout):
		}

		self.connectClient.Close()
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

