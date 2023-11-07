package client


import (
	"context"
	"time"
	"fmt"
	
	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/connect"
	"bringyour.com/protocol"
)


const clientDrainTimeout = 30 * time.Second


// receive a packet into the local raw socket
type ReceivePacket interface {
    ReceivePacket(packet []byte)
}


// TODO methods to manage extenders


// conforms to `Router`
type BringYourDevice struct {
	ctx context.Context
	cancel context.CancelFunc

	byJwt string
	platformUrl string

	clientId connect.Id
	instanceId connect.Id
	connectClient *connect.Client

	contractManager *connect.ContractManager
	routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	// when nil, packets get routed to the local user nat
	remoteUserNatClient *connect.RemoteUserNatClient

	remoteUserNatProvider *connect.RemoteUserNatProvider

	openedViewControllers map[ViewController]bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]
}

func NewBringYourDevice(byJwt string, platformUrl string, instanceId Id) (*BringYourDevice, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(context.Background())

	connectClient := connect.NewClientWithDefaults(
        cancelCtx,
        clientId,
    )

    routeManager := connect.NewRouteManager(connectClient)
    contractManager := connect.NewContractManagerWithDefaults(connectClient)

    go connectClient.Run(routeManager, contractManager)

    auth := &connect.ClientAuth{
    	ByJwt: byJwt,
    	InstanceId: connect.Id(instanceId),
    	AppVersion: Version,
    }
    platformTransport := connect.NewPlatformTransportWithDefaults(cancelCtx, platformUrl, auth)

    go platformTransport.Run(routeManager)

    localUserNat := connect.NewLocalUserNatWithDefaults(cancelCtx)

    remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat)

	byDevice := &BringYourDevice{
		ctx: cancelCtx,
		cancel: cancel,
		byJwt: byJwt,
		platformUrl: platformUrl,
		clientId: clientId,
		instanceId: connect.Id(instanceId),
		connectClient: connectClient,
		contractManager: contractManager,
		routeManager: routeManager,
		platformTransport: platformTransport,
		localUserNat: localUserNat,
		remoteUserNatClient: nil,
		remoteUserNatProvider: remoteUserNatProvider,
		openedViewControllers: map[ViewController]bool{},
		receiveCallbacks: connect.NewCallbackList[connect.ReceivePacketFunction](),
	}


	// set up with nil destination
	localUserNat.AddReceivePacketCallback(byDevice.receive)


	return byDevice, nil
}

// `ReceivePacketFunction`
func (self *BringYourDevice) receive(source connect.Path, packet []byte) {
    for _, receiveCallback := range self.receiveCallbacks.Get() {
        receiveCallback(source, packet)
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
	self.contractManager.SetProvideModes(provideModes)
}

// `Router` implementation
func (self *BringYourDevice) SetDestination(destinations *PathList, provideMode ProvideMode) error {
	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	} else {
		self.localUserNat.RemoveReceivePacketCallback(self.receive)
	}

	if destinations.Len() == 0 {
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
	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.SendPacket(source, protocol.ProvideMode_Network, packetCopy)
	} else {
		// route locally
		self.localUserNat.SendPacket(source, protocol.ProvideMode_Network, packetCopy)
	}
}

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(destination connect.Path, packet []byte) {
		receivePacket.ReceivePacket(packet)
	}
	self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(receive)
	})
}

func (self *BringYourDevice) OpenConnectViewController() *ConnectViewController {
	vc := newConnectViewController(self.ctx, self.connectClient)
	self.openedViewControllers[vc] = true
	return vc
}

func (self *BringYourDevice) OpenProvideViewController() *ProvideViewController {
	vc := newProvideViewController(self.ctx, self.connectClient, self)
	self.openedViewControllers[vc] = true
	return vc
}

func (self *BringYourDevice) OpenStatusViewController() *StatusViewController {
	vc := newStatusViewController(self.ctx, self.connectClient)
	self.openedViewControllers[vc] = true
	return vc
}

func (self *BringYourDevice) OpenDeviceViewController() *DeviceViewController {
	vc := newDeviceViewController(self.ctx, self.connectClient)
	self.openedViewControllers[vc] = true
	return vc
}

func (self *BringYourDevice) OpenAccountViewController() *AccountViewController {
	vc := newAccountViewController(self.ctx, self.connectClient)
	self.openedViewControllers[vc] = true
	return vc
}

func (self *BringYourDevice) CloseViewController(vc ViewController) {
	vc.Close()
	delete(self.openedViewControllers, vc)
}

func (self *BringYourDevice) Close() {
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

