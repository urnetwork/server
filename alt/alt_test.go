package alt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	connectcore "github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Synthetic dispatch names. The connect front is reached by a platform
// transport, which resolves the name it presents as sni, so its name is the
// one loopback name every host has.
const (
	testApiHost       = "api.alt.example"
	testConnectHost   = "localhost"
	testConnectV6Host = "connect-v6.alt.example"
	testUnknownHost   = "other.alt.example"
)

const testProgressTimeout = 30 * time.Second

// One alt with both listeners on loopback, its exchange, and the network a
// platform transport authenticates against.
type altEnv struct {
	t   testing.TB
	ctx context.Context

	alt            *Alt
	connectHandler *connectserver.ConnectHandler
	exchange       *connectserver.Exchange
	transportTls   *server.TransportTls

	h3Port  int
	dnsPort int

	apiRequestPaths chan string
	dispatched      chan altDispatch

	userSession *session.ClientSession

	closeOnce   sync.Once
	closeErrors []func()
}

type altDispatch struct {
	serverName string
	front      string
}

// Binds one udp port on both loopback families, which is the shape a proxy
// host's alt block has: one socket per family on one allocated port, so a
// client that resolves either family reaches the same listener.
func listenLoopbackPacketConns(t testing.TB) []net.PacketConn {
	for range 16 {
		packetConn4, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := packetConn4.LocalAddr().(*net.UDPAddr).Port
		packetConn6, err := net.ListenPacket("udp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if err == nil {
			return []net.PacketConn{packetConn4, packetConn6}
		}
		packetConn4.Close()
	}
	t.Fatal("could not bind one loopback port on both families")
	return nil
}

// The self-signed identity the fixture serves for one name, as a root the
// client verifies against. The production certificates are the api's and
// connect's; this is the same shape with a test-only issuer.
func testFixtureRoots(t testing.TB, transportTls *server.TransportTls, serverNames ...string) *x509.CertPool {
	roots := x509.NewCertPool()
	for _, serverName := range serverNames {
		tlsConfig, err := transportTls.GetTlsConfig(serverName)
		if err != nil {
			t.Fatal(err)
		}
		for _, certBytes := range tlsConfig.Certificates[0].Certificate {
			certificate, err := x509.ParseCertificate(certBytes)
			if err != nil {
				t.Fatal(err)
			}
			roots.AddCert(certificate)
		}
	}
	return roots
}

func testing_newAltEnv(ctx context.Context, t testing.TB, mutate func(*Settings)) *altEnv {
	settings := DefaultSettings()
	settings.ApiHosts = []string{testApiHost}
	settings.ConnectHosts = []string{testConnectHost, testConnectV6Host}
	settings.ExchangeSettings.ExchangeResidentTtl = 5 * time.Second
	settings.ExchangeSettings.TransportTlsSettings.EnableSelfSign = true
	settings.ExchangeSettings.ConnectionAnnounceTimeout = 0
	settings.ExchangeSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	if mutate != nil {
		mutate(settings)
	}

	exchangeListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	servicePort := exchangeListener.Addr().(*net.TCPAddr).Port
	exchange := connectserver.NewExchangeWithListeners(
		ctx,
		"host0",
		"connect",
		"test",
		map[int]int{servicePort: servicePort},
		map[string]string{"host0": "127.0.0.1"},
		settings.ExchangeSettings,
		map[int]net.Listener{servicePort: exchangeListener},
	)
	connectHandler := connectserver.NewConnectHandlerWithPacketConns(
		ctx,
		server.NewId(),
		exchange,
		&settings.ExchangeSettings.ConnectHandlerSettings,
		connectserver.ConnectHandlerPacketConns{},
	)

	// the api front is a fixture route table: this test owns the dispatch and
	// the limits, not the production route list
	apiRequestPaths := make(chan string, 64)
	apiHandler := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("GET", "/hello", func(w http.ResponseWriter, r *http.Request) {
			select {
			case apiRequestPaths <- r.URL.Path:
			default:
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "hello")
		}),
	})

	altServer, err := NewAlt(ctx, connectHandler, apiHandler, settings)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan altDispatch, 64)
	altServer.dispatchObserverForTest = func(serverName string, front string) {
		select {
		case dispatched <- altDispatch{serverName: serverName, front: front}:
		default:
		}
	}

	h3PacketConns := listenLoopbackPacketConns(t)
	dnsPacketConns := listenLoopbackPacketConns(t)
	for _, packetConn := range h3PacketConns {
		go server.HandleError(func() {
			altServer.ListenH3(packetConn)
		})
	}
	for _, packetConn := range dnsPacketConns {
		go server.HandleError(func() {
			altServer.ListenWhodis(packetConn)
		})
	}

	networkId := server.NewId()
	userId := server.NewId()
	networkName := fmt.Sprintf("alt-%s", networkId)
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	userSession := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(
		networkId,
		userId,
		networkName,
		false,
		false,
	))

	return &altEnv{
		t:               t,
		ctx:             ctx,
		alt:             altServer,
		connectHandler:  connectHandler,
		exchange:        exchange,
		transportTls:    connectHandler.TransportTls(),
		h3Port:          h3PacketConns[0].LocalAddr().(*net.UDPAddr).Port,
		dnsPort:         dnsPacketConns[0].LocalAddr().(*net.UDPAddr).Port,
		apiRequestPaths: apiRequestPaths,
		dispatched:      dispatched,
		userSession:     userSession,
	}
}

func (self *altEnv) Close() {
	self.closeOnce.Do(func() {
		for _, closeError := range self.closeErrors {
			closeError()
		}
		self.alt.Close()
		self.exchange.Close()
	})
}

func (self *altEnv) authClient() (server.Id, server.Id, string) {
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
		Description: "alt",
		DeviceSpec:  "spec",
	}, self.userSession)
	if err != nil {
		self.t.Fatal(err)
	}
	if result.Error != nil {
		self.t.Fatal(result.Error.Message)
	}
	return *result.ClientId, server.NewId(), *result.ByClientJwt
}

// An http3 client that dials the fixture's loopback socket directly and
// presents serverName as sni, optionally through the client half of the dns
// translation, which is how the alt whodis dialer reaches the api front.
func (self *altEnv) apiClient(serverName string, whodis bool) *http.Client {
	port := self.h3Port
	if whodis {
		port = self.dnsPort
	}
	udpAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	roundTripper := &http3.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    testFixtureRoots(self.t, self.transportTls, serverName),
			ServerName: serverName,
			MinVersion: tls.VersionTLS13,
		},
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout: testProgressTimeout,
			InitialPacketSize:    connectcore.H3InitialPacketByteCount,
		},
		Dial: func(
			ctx context.Context,
			addr string,
			tlsConfig *tls.Config,
			quicConfig *quic.Config,
		) (*quic.Conn, error) {
			packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			if whodis {
				ptSettings := connectcore.DefaultPacketTranslationSettings()
				ptSettings.DnsTlds = [][]byte{[]byte(DefaultDnsTld)}
				// the connection owns the translation, not the dial: the dial
				// context is canceled as soon as the handshake returns
				packetConn, err = connectcore.NewPacketTranslation(
					context.WithoutCancel(ctx),
					connectcore.PacketTranslationModeDns,
					packetConn,
					ptSettings,
				)
				if err != nil {
					return nil, err
				}
			}
			quicTransport := &quic.Transport{
				Conn: packetConn,
			}
			self.closeErrors = append(self.closeErrors, func() {
				quicTransport.Close()
				packetConn.Close()
			})
			return quicTransport.DialEarly(ctx, udpAddr, tlsConfig, quicConfig)
		},
	}
	self.closeErrors = append(self.closeErrors, func() { roundTripper.Close() })
	return &http.Client{Transport: roundTripper}
}

// A platform transport in one of the H3 family modes, pointed at the
// fixture's loopback listeners.
func (self *altEnv) newTransport(
	byClientJwt string,
	instanceId server.Id,
	routeManager *connectcore.RouteManager,
	mode connectcore.TransportMode,
) *connectcore.PlatformTransport {
	settings := connectcore.DefaultPlatformTransportSettings()
	settings.QuicTlsConfig.InsecureSkipVerify = true
	settings.H3Port = self.h3Port
	settings.DnsPort = self.dnsPort
	settings.DnsPumpHost = testConnectHost
	budgetStats := connectcore.DefaultPlatformTransportBudget().Stats()
	settings.PlatformTransportBudget = connectcore.NewPlatformTransportBudget(
		budgetStats.TotalByteCount,
		budgetStats.MaxTransportCount,
	)
	return connectcore.NewPlatformTransportWithTargetMode(
		self.ctx,
		connectcore.NewClientStrategyWithDefaults(self.ctx),
		routeManager,
		fmt.Sprintf("wss://%s", testConnectHost),
		&connectcore.ClientAuth{
			ByJwt:      byClientJwt,
			InstanceId: connectcore.Id(instanceId),
			AppVersion: "0.0.0",
		},
		mode,
		settings,
	)
}

// Waits for the exact next dispatch record.
func (self *altEnv) nextDispatch() altDispatch {
	select {
	case dispatch := <-self.dispatched:
		return dispatch
	case <-time.After(testProgressTimeout):
		self.t.Fatal("no connection was dispatched")
		return altDispatch{}
	}
}

func altGet(t testing.TB, client *http.Client, serverName string, path string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), testProgressTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("https://%s%s", serverName, path),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, "", err
	}
	return response.StatusCode, string(body), nil
}

// The api front is the same router served over H3 on the public udp socket,
// selected by the api name in the sni (L1).
func TestAltServesTheApiOverH3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		code, body, err := altGet(t, env.apiClient(testApiHost, false), testApiHost, "/hello")
		if err != nil {
			t.Fatal(err)
		}
		if code != http.StatusOK || body != "hello" {
			t.Fatalf("api over h3 = %d %q", code, body)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontApi || dispatch.serverName != testApiHost {
			t.Fatalf("api over h3 dispatch = %+v", dispatch)
		}
	})
}

// The whodis listener is the same dispatch under the decode53 translation, so
// whodis reaches the api front as well as connect (L1).
func TestAltServesTheApiOverWhodis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		code, body, err := altGet(t, env.apiClient(testApiHost, true), testApiHost, "/hello")
		if err != nil {
			t.Fatal(err)
		}
		if code != http.StatusOK || body != "hello" {
			t.Fatalf("api over whodis = %d %q", code, body)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontApi || dispatch.serverName != testApiHost {
			t.Fatalf("api over whodis dispatch = %+v", dispatch)
		}
	})
}

// A platform transport in each H3 family mode reaches the connect handler
// through the same sni dispatch and completes its auth handshake, which is
// what registering its routes proves.
func TestAltAuthenticatesAPlatformTransport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, mode := range []connectcore.TransportMode{
			connectcore.TransportModeH3,
			connectcore.TransportModeH3Dns,
		} {
			ctx, cancel := context.WithCancel(context.Background())
			env := testing_newAltEnv(ctx, t, nil)

			clientId, instanceId, byClientJwt := env.authClient()
			routeManager := connectcore.NewRouteManager(ctx, clientId.String())
			transport := env.newTransport(byClientJwt, instanceId, routeManager, mode)

			connected := false
			deadline := time.Now().Add(testProgressTimeout)
			for !connected && time.Now().Before(deadline) {
				notify := transport.ConnectedNotify()
				if transport.IsConnected() {
					connected = true
					break
				}
				select {
				case <-notify:
				case <-time.After(time.Until(deadline)):
				}
			}
			if !connected {
				transport.Close()
				env.Close()
				cancel()
				t.Fatalf("%s transport did not authenticate", mode)
			}
			if dispatch := env.nextDispatch(); dispatch.front != frontConnect || dispatch.serverName != testConnectHost {
				transport.Close()
				env.Close()
				cancel()
				t.Fatalf("%s dispatch = %+v", mode, dispatch)
			}
			transport.Close()
			env.Close()
			cancel()
		}
	})
}

// A name in neither list is refused with an application error before any
// stream is served, so an unrelated name cannot reach either front.
func TestAltRefusesAnUnknownServerName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		_, _, err := altGet(t, env.apiClient(testUnknownHost, false), testUnknownHost, "/hello")
		if err == nil {
			t.Fatal("an unknown server name was served")
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontRefused || dispatch.serverName != testUnknownHost {
			t.Fatalf("unknown name dispatch = %+v", dispatch)
		}
		select {
		case path := <-env.apiRequestPaths:
			t.Fatalf("a refused connection reached the api front: %s", path)
		default:
		}
	})
}

// Each name reaches exactly one front. An api request to a connect name is
// never answered by the api router, and a connect name never enters the api
// front, whichever listener the connection arrived on.
func TestAltDispatchNeverCrossesBetweenFronts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		// an http3 request to a connect name reaches the connect handler,
		// which has no http3 to answer with
		if code, body, err := altGet(t, env.apiClient(testConnectV6Host, false), testConnectV6Host, "/hello"); err == nil {
			t.Fatalf("a connect name answered an api request: %d %q", code, body)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect || dispatch.serverName != testConnectV6Host {
			t.Fatalf("connect name dispatch = %+v", dispatch)
		}
		select {
		case path := <-env.apiRequestPaths:
			t.Fatalf("a connect name reached the api front: %s", path)
		default:
		}

		// the same name over whodis takes the same front
		if code, body, err := altGet(t, env.apiClient(testConnectV6Host, true), testConnectV6Host, "/hello"); err == nil {
			t.Fatalf("a connect name answered an api request over whodis: %d %q", code, body)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect || dispatch.serverName != testConnectV6Host {
			t.Fatalf("connect name whodis dispatch = %+v", dispatch)
		}

		// and the api name still answers, so the fronts are selected by name
		// rather than by listener
		code, body, err := altGet(t, env.apiClient(testApiHost, true), testApiHost, "/hello")
		if err != nil {
			t.Fatal(err)
		}
		if code != http.StatusOK || body != "hello" {
			t.Fatalf("api over whodis = %d %q", code, body)
		}
	})
}
