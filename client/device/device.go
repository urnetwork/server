package client


import (
	"context"
	"time"
	"fmt"
	
	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/connect"
	"bringyour.com/protocol"

	"bringyour.com/client/vc"
	"bringyour.com/client"
)


// note: publicly exported types must be fully contained in the `client` package tree
// the `gomobile` native interface compiler won't be able to map types otherwise
// a number of types (struct, function, interface) are redefined in `client`,
// somtimes in a simplified way, and then internally converted back to the native type


const ClientDrainTimeout = 30 * time.Second


// receive a packet into the local raw socket
type ReceivePacket interface {
    ReceivePacket(packet []byte)
}


// TODO methods to manage extenders


// conforms to `client.Router`
type BringYourDevice struct {
	ctx context.Context
	cancel context.CancelFunc

	byJwt string
	platformUrl string
	localStorage string

	clientId connect.Id
	connectClient *connect.Client

	contractManager *connect.ContractManager
	routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	// when nil, packets get routed to the local user nat
	remoteUserNatClient *connect.RemoteUserNatClient

	remoteUserNatProvider *connect.RemoteUserNatProvider

	openedViewControllers map[vc.ViewController]bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]
}

// `localStorage` is a path to a local storage directory
func NewBringYourDevice(byJwt string, platformUrl string, localStorage string) (*BringYourDevice, error) {
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

    instanceId := initInstanceId(localStorage)
    auth := &connect.ClientAuth{
    	ByJwt: byJwt,
    	InstanceId: instanceId,
    	AppVersion: client.Version,
    }
    platformTransport := connect.NewPlatformTransportWithDefaults(cancelCtx, platformUrl, auth)

    localUserNat := connect.NewLocalUserNatWithDefaults(cancelCtx)

    remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat)

	return &BringYourDevice{
		ctx: cancelCtx,
		cancel: cancel,
		byJwt: byJwt,
		platformUrl: platformUrl,
		localStorage: localStorage,
		clientId: clientId,
		connectClient: connectClient,
		contractManager: contractManager,
		routeManager: routeManager,
		platformTransport: platformTransport,
		localUserNat: localUserNat,
		remoteUserNatClient: nil,
		remoteUserNatProvider: remoteUserNatProvider,
		openedViewControllers: map[vc.ViewController]bool{},
		receiveCallbacks: connect.NewCallbackList[connect.ReceivePacketFunction](),
	}, nil
}

// `ReceivePacketFunction`
func (self *BringYourDevice) receive(source connect.Path, packet []byte) {
    for _, receiveCallback := range self.receiveCallbacks.Get() {
        receiveCallback(source, packet)
    }
}

func (self *BringYourDevice) SetProvideMode(provideMode client.ProvideMode) {
	provideModes := map[protocol.ProvideMode]bool{}
	if client.ProvideModePublic <= provideMode {
		provideModes[protocol.ProvideMode_Public] = true
	}
	if client.ProvideModeNetwork <= provideMode {
		provideModes[protocol.ProvideMode_Network] = true
	}
	self.contractManager.SetProvideModes(provideModes)
}

// `client.Router` implementation
func (self *BringYourDevice) SetDestination(destinations []client.Path, provideMode client.ProvideMode) error {
	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	} else {
		self.localUserNat.RemoveReceivePacketCallback(self.receive)
	}

	if destinations == nil {
		self.localUserNat.AddReceivePacketCallback(self.receive)
		return nil
	} else {
		connectDestinations := []connect.Path{}
		for _, destination := range destinations {
			connectDestinations = append(connectDestinations, destination.ToConnectPath())
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

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) client.Sub {
	receive := func(destination connect.Path, packet []byte) {
		receivePacket.ReceivePacket(packet)
	}
	self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(receive)
	})
}

func (self *BringYourDevice) OpenConnectViewController() *vc.ConnectViewController {
	cvc := vc.NewConnectViewController(self.ctx, self.connectClient)
	self.openedViewControllers[cvc] = true
	return cvc
}

func (self *BringYourDevice) OpenProvideViewController() *vc.ProvideViewController {
	pvc := vc.NewProvideViewController(self.ctx, self.connectClient, self)
	self.openedViewControllers[pvc] = true
	return pvc
}

func (self *BringYourDevice) OpenStatusViewController() *vc.StatusViewController {
	svc := vc.NewStatusViewController(self.ctx, self.connectClient)
	self.openedViewControllers[svc] = true
	return svc
}

func (self *BringYourDevice) OpenDeviceViewController() *vc.DeviceViewController {
	dvc := vc.NewDeviceViewController(self.ctx, self.connectClient)
	self.openedViewControllers[dvc] = true
	return dvc
}

func (self *BringYourDevice) OpenAccountViewController() *vc.AccountViewController {
	avc := vc.NewAccountViewController(self.ctx, self.connectClient)
	self.openedViewControllers[avc] = true
	return avc
}

func (self *BringYourDevice) CloseViewController(viewController vc.ViewController) {
	viewController.Close()
	delete(self.openedViewControllers, viewController)
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
		case <- time.After(ClientDrainTimeout):
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


func initInstanceId(localStorage string) connect.Id {
	// FIXME store the instance id in a file

	return connect.NewId()
}




func newSub(unsubFn func()) client.Sub {
	return &simpleSub{
		unsubFn: unsubFn,
	}
}

type simpleSub struct {
	unsubFn func()
}

func (self *simpleSub) Close() {
	self.unsubFn()
}

// type Subscription interface {
// 	Close()
// }

