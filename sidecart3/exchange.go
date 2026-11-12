package sidecart3

// Server's exchange as a T3 Net's platform (§22.4's in-process exchange, Q-TEST-1): sidecartest.NetPlatform over
// server/connect's exchange, so a T3 row's connect clients run on what production gives them, and a row that passed on
// T2's fake contract issuer and ControlId sink either passes here too or diverges.
//
// What is real: the exchange (NewExchangeWithListeners, server/connect/resident.go:902's in-process form) and its
// ConnectHandler, on loopback listeners of their own whose ports the OS assigns; the h1 platform transport each client
// dials, carrying a by-client jwt that model.AuthNetworkClient mints in a network made for the Net and funded so its
// contracts escrow; and server's controller, which takes each client's provide modes, keys and closes and signs every
// contract between them (controller.ConnectControlFrames), on server's Postgres and Redis.
//
// What differs from production, and why:
//
//   - Out-of-band control reaches the controller in process, as server's own connect tests reach it, rather than over
//     api's control route, which T3 does not host: the route's own auth and http faults are not on this path.
//   - The handler listens on h1 alone. h3 and the dns carrier need fixed UDP ports and a TLS identity, and the
//     environment's tls.yml is read only to construct the handler, through an empty override when there is none.
//   - Clients gather loopback ICE candidates alone, with no STUN server, and dial no extender, so no T3 client
//     reaches past this host; and the handler takes the burst of every client of every Net from one loopback address.
//   - Time is real: contracts and residents keep server.NowUtc's time, not the Net's fake clock (TEST-M2-07's).
//
// A platform is built at a Net's first client (sidecartest's NetSettings.Platform), so a T3 Net with no clients needs
// no schema; one with clients needs server's migrations applied, which a TestEnv with ApplyDbMigrations gives. Its
// frames pass no exchange of the Net's, so CC2 does not capture them (§22.7's capture on T3 is TEST-M2-09's). Safe
// for concurrent use.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// What server's exchange runs a T3 Net's clients with.
type ExchangeSettings struct {
	// The exchange's own: server's defaults, as production runs it.
	Exchange *connectserver.ExchangeSettings
	// The handler's: server's defaults, with no h3 or dns listener and a burst for one loopback address.
	Handler *connectserver.ConnectHandlerSettings
	// Each client's platform transport: connect's defaults with no dns carrier. Clients run h1 alone.
	Transport *connect.PlatformTransportSettings
	// The strategy every client of one platform dials with: connect's defaults with no extender, which is a host past
	// this one.
	ClientStrategy *connect.ClientStrategySettings
	// The app version a client's auth names.
	AppVersion string
	// How long, in real time, opening a client waits for the controller's ack of its provide modes, after which the
	// client is refused. connect's timers are real (X9), so this bounds a broken platform, not a schedule.
	ProvideAckTimeout time.Duration
	// How long, in real time, opening a client waits for server to hold the client key the client publishes in band,
	// after which the client is refused, and how often it looks meanwhile: nothing acks the publication.
	KeyRegistrationTimeout      time.Duration
	KeyRegistrationPollInterval time.Duration
	// How long, in real time, closing waits for the http server, the handler and the exchange to go idle. One that
	// does not fails the test.
	CloseTimeout time.Duration
	// What the Net's network holds, so its clients' contracts escrow, and for how long from the platform's start.
	NetworkBalanceByteCount model.ByteCount
	NetworkBalanceDuration  time.Duration
}

func DefaultExchangeSettings() *ExchangeSettings {
	handler := connectserver.DefaultConnectHandlerSettings()
	handler.ListenH3Port = 0
	handler.ListenDnsPort = 0
	// every client of every T3 Net comes from 127.0.0.1, where production's come from many addresses
	handler.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	transport := connect.DefaultPlatformTransportSettings()
	transport.DnsPort = 0
	clientStrategy := connect.DefaultClientStrategySettings()
	clientStrategy.EnableResilient = false
	return &ExchangeSettings{
		Exchange:                    connectserver.DefaultExchangeSettings(),
		Handler:                     handler,
		Transport:                   transport,
		ClientStrategy:              clientStrategy,
		AppVersion:                  "0.0.0",
		ProvideAckTimeout:           60 * time.Second,
		KeyRegistrationTimeout:      60 * time.Second,
		KeyRegistrationPollInterval: 20 * time.Millisecond,
		CloseTimeout:                30 * time.Second,
		NetworkBalanceByteCount:     model.ByteCount(1024 * 1024 * 1024 * 1024),
		NetworkBalanceDuration:      24 * time.Hour,
	}
}

// The exchange's host name in its routes, which map it to loopback.
const exchangeHost = "sidecart3"

// A tls.yml naming no host, for an environment with none: the h1 handler serves no TLS, but its constructor reads
// the file.
var emptyTransportTlsConfig = []byte("allowed_hosts: []\n")

// Server's exchange, its handler and http server, and the network a Net's clients join.
type exchangePlatform struct {
	t        testing.TB
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ExchangeSettings

	exchange       *connectserver.Exchange
	handler        *connectserver.ConnectHandler
	httpServer     *server.TestHttpServer
	platformUrl    string
	clientStrategy *connect.ClientStrategy
	userSession    *session.ClientSession
}

// A loopback listener on a port the OS assigns from its ephemeral range, which holds none of the ports a production
// data tunnel could hold; one on such a port is refused all the same.
func listenLoopback(network string) (net.Listener, int, error) {
	listener, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if server.Testing_LoopbackTunnelPort(uint16(port)) {
		listener.Close()
		return nil, 0, fmt.Errorf("loopback port %d could be a production data tunnel's", port)
	}
	return listener, port, nil
}

// Starts server's exchange, its handler on an http server, and a funded network, until ctx ends or Close. A failure
// leaves nothing running.
func newExchangePlatform(ctx context.Context, t testing.TB, settings *ExchangeSettings) (platform *exchangePlatform, returnErr error) {
	cancelCtx, cancel := context.WithCancel(ctx)
	closes := []func(){cancel}
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("server's exchange did not start: %v", recovered)
		}
		if returnErr != nil {
			for index := len(closes) - 1; 0 <= index; index -= 1 {
				closes[index]()
			}
		}
	}()

	exchangeListener, exchangePort, err := listenLoopback("tcp4")
	if err != nil {
		return nil, err
	}
	exchangeSettings := *settings.Exchange
	exchange := connectserver.NewExchangeWithListeners(
		cancelCtx,
		exchangeHost,
		"connect",
		"sidecart3",
		map[int]int{exchangePort: exchangePort},
		map[string]string{exchangeHost: "127.0.0.1"},
		&exchangeSettings,
		map[int]net.Listener{exchangePort: exchangeListener},
	)
	closes = append(closes, exchange.Close)

	// the handler writes what it derives into its settings, so each platform gets a copy
	handlerSettings := *settings.Handler
	handler := func() *connectserver.ConnectHandler {
		if _, err := server.Config.SimpleResource("tls.yml"); err != nil {
			defer server.Config.PushSimpleResource("tls.yml", emptyTransportTlsConfig)()
		}
		return connectserver.NewConnectHandler(cancelCtx, server.NewId(), exchange, &handlerSettings)
	}()
	closes = append(closes, handler.Close)

	httpListener, httpPort, err := listenLoopback("tcp4")
	if err != nil {
		return nil, err
	}
	httpServer := server.NewTestHttpServer(cancelCtx, httpListener, &http.Server{
		Handler: router.NewRouter(cancelCtx, []*router.Route{
			router.NewRoute("GET", "/", handler.Connect),
		}),
	})
	closes = append(closes, func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), settings.CloseTimeout)
		defer closeCancel()
		httpServer.CloseAndWait(closeCtx)
	})

	networkId := server.NewId()
	userId := server.NewId()
	networkName := fmt.Sprintf("sidecart3-%s", networkId)
	model.Testing_CreateNetwork(cancelCtx, networkId, networkName, userId)
	now := server.NowUtc()
	if err := model.AddBasicTransferBalance(cancelCtx, networkId, settings.NetworkBalanceByteCount, now, now.Add(settings.NetworkBalanceDuration)); err != nil {
		return nil, err
	}
	// minted through NewByJwt, so each by-client jwt derived from it carries every claim the transport's auth checks
	userSession := session.Testing_CreateClientSession(cancelCtx, session.NewByJwt(networkId, userId, networkName, false, false))
	closes = append(closes, userSession.Cancel)

	clientStrategy := connect.NewClientStrategy(cancelCtx, settings.ClientStrategy)

	return &exchangePlatform{
		t:              t,
		ctx:            cancelCtx,
		cancel:         cancel,
		settings:       settings,
		exchange:       exchange,
		handler:        handler,
		httpServer:     httpServer,
		platformUrl:    fmt.Sprintf("ws://127.0.0.1:%d", httpPort),
		clientStrategy: clientStrategy,
		userSession:    userSession,
	}, nil
}

// Out-of-band control as api's control route answers it, in process: each pack goes to server's controller for the
// client, on the platform's context and off the caller's goroutine. Once SendControl returns, the frames and the
// callback are this control's (connect/CODESTYLE.md's out-of-band boundary).
type controllerOutOfBandControl struct {
	ctx                     context.Context
	clientId                server.Id
	contractManagerSettings *connect.ContractManagerSettings
}

func (self *controllerOutOfBandControl) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	go func() {
		defer func() {
			for _, frame := range frames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		var resultFrames []*protocol.Frame
		var err error
		// a canceled platform raises out of the db layer at teardown, which HandleError takes: the callback then hears the
		// raise as its error, as api's control route would answer one, since a client counts a control in flight until its
		// callback runs and its close waits for every one (a raise that skipped it left a client that never closed)
		server.HandleError(func() {
			resultFrames, err = controller.ConnectControlFrames(self.ctx, self.clientId, frames, self.contractManagerSettings)
		}, func(raised error) {
			resultFrames = nil
			err = raised
		})
		defer func() {
			for _, frame := range resultFrames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		if callback != nil {
			server.HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}()
}

// Opens a Required client in the platform's network: a client id and by-client jwt from model.AuthNetworkClient, the
// controller as its out-of-band control, and an h1 platform transport to the handler. Answers once the controller has
// taken its provide modes and server holds its key, so a peer's contract to it can be signed and carries that key.
func (self *exchangePlatform) OpenClient(ctx context.Context, name string, settings *connect.ClientSettings) (platformClient *sidecartest.NetPlatformClient, returnErr error) {
	// what ends the client once it exists, so a failure past that point leaves nothing running
	var closeClient func()
	defer func() {
		// the model raises a database failure
		if recovered := recover(); recovered != nil {
			if closeClient != nil {
				closeClient()
			}
			returnErr = fmt.Errorf("server refused client %q: %v", name, recovered)
		}
	}()
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
		Description: name,
		DeviceSpec:  "sidecart3",
	}, self.userSession)
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("server refused client %q: %+v", name, *result.Error)
	}
	clientId := *result.ClientId

	// loopback candidates alone and no STUN server: a T3 client reaches nothing past this host
	clientSettings := *settings
	webRtcSettings := *settings.WebRtcSettings
	webRtcSettings.IceServerUrls = nil
	webRtcSettings.IceServerPoolUrls = nil
	webRtcSettings.UseLoopbackOnlyIceInterfaces = true
	clientSettings.WebRtcSettings = &webRtcSettings

	client := connect.NewClient(
		ctx,
		connect.Id(clientId),
		&controllerOutOfBandControl{
			ctx:                     self.ctx,
			clientId:                clientId,
			contractManagerSettings: clientSettings.ContractManagerSettings,
		},
		&clientSettings,
	)
	transport := connect.NewPlatformTransportWithTargetMode(
		ctx,
		self.clientStrategy,
		client.RouteManager(),
		self.platformUrl,
		&connect.ClientAuth{
			ByJwt:      *result.ByClientJwt,
			InstanceId: connect.NewId(),
			AppVersion: self.settings.AppVersion,
		},
		connect.TransportModeH1,
		self.settings.Transport,
	)
	closeClient = func() {
		client.Cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), self.settings.CloseTimeout)
		defer closeCancel()
		if err := transport.CloseAndWait(closeCtx); err != nil {
			self.t.Errorf("client %q's transport did not close within %s: %v", name, self.settings.CloseTimeout, err)
		}
		if err := client.CloseAndWait(closeCtx); err != nil {
			self.t.Errorf("client %q did not close within %s: %v", name, self.settings.CloseTimeout, err)
		}
	}

	provideAck := make(chan error, 1)
	client.ContractManager().SetProvideModesWithReturnTrafficWithOobAckCallback(
		map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true},
		func(err error) {
			select {
			case provideAck <- err:
			default:
			}
		},
	)
	select {
	case err := <-provideAck:
		if err != nil {
			closeClient()
			return nil, fmt.Errorf("the controller refused client %q's provide modes: %w", name, err)
		}
	case <-ctx.Done():
		closeClient()
		return nil, ctx.Err()
	case <-time.After(self.settings.ProvideAckTimeout):
		closeClient()
		return nil, fmt.Errorf("no ack of client %q's provide modes within %s", name, self.settings.ProvideAckTimeout)
	}

	// The controller seals into a contract the key server holds for its destination, and a client learns its peer's
	// key from its own contract to that peer. A contract signed before the key arrives carries none, and the client's
	// session to the peer then cannot verify the peer (no T3 client has an out-of-band key fetcher, as no sidecar
	// has). So the client is open once server holds the key it publishes in band, which nothing acks.
	publicKey := client.ClientKeyManager().PublicKey()
	keyRegistered := func() bool {
		heldKey, err := model.GetClientPublicKey(self.ctx, clientId)
		return err == nil && bytes.Equal(heldKey, publicKey)
	}
	keyRegistrationTimeout := time.After(self.settings.KeyRegistrationTimeout)
	for !keyRegistered() {
		select {
		case <-ctx.Done():
			closeClient()
			return nil, ctx.Err()
		case <-keyRegistrationTimeout:
			closeClient()
			return nil, fmt.Errorf("server held no key for client %q within %s", name, self.settings.KeyRegistrationTimeout)
		case <-time.After(self.settings.KeyRegistrationPollInterval):
		}
	}
	return &sidecartest.NetPlatformClient{
		ClientId: connect.Id(clientId),
		Client:   client,
		Close:    closeClient,
	}, nil
}

// Closes the http server, the handler and the exchange, and fails the test with any that does not go idle within
// CloseTimeout. The Net has closed every client first.
func (self *exchangePlatform) Close() {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), self.settings.CloseTimeout)
	defer closeCancel()
	if err := self.httpServer.CloseAndWait(closeCtx); err != nil {
		self.t.Errorf("the exchange's http server did not close within %s: %v", self.settings.CloseTimeout, err)
	}
	self.exchange.Close()
	self.handler.Close()
	if !self.handler.WaitForIdle(closeCtx) {
		self.t.Errorf("server's connect handler did not go idle within %s", self.settings.CloseTimeout)
	}
	if !self.exchange.WaitForIdle(closeCtx) {
		self.t.Errorf("server's exchange did not go idle within %s", self.settings.CloseTimeout)
	}
	self.clientStrategy.Close()
	self.userSession.Cancel()
	self.cancel()
}
