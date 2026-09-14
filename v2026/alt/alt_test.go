package alt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	connectcore "github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
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
	// closed when each registered listener's serve returns, which is the
	// barrier a drain is observed on
	listenerDone []chan struct{}

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
	listenerDone := []chan struct{}{}
	serve := func(listen func() error) {
		done := make(chan struct{})
		listenerDone = append(listenerDone, done)
		go server.HandleError(func() {
			defer close(done)
			listen()
		})
	}
	for _, packetConn := range h3PacketConns {
		serve(func() error { return altServer.ListenH3(packetConn) })
	}
	for _, packetConn := range dnsPacketConns {
		serve(func() error { return altServer.ListenWhodis(packetConn) })
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
		listenerDone:    listenerDone,
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

// The warp deploy poll's view of this alt.
func (self *altEnv) status() int {
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	self.alt.Status(w, r)
	return w.Code
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

// Warp must not activate a replacement alt before both udp fronts accept, so
// a registered socket that is not yet serving keeps the status route at 503.
func TestAltStatusWaitsForEveryListener(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		// the fixture's listeners are up once a request has been served
		if _, _, err := altGet(t, env.apiClient(testApiHost, false), testApiHost, "/hello"); err != nil {
			t.Fatal(err)
		}
		if err := env.alt.ListenerReady(); err != nil {
			t.Fatalf("alt is not ready with both listeners up: %s", err)
		}
		if code := env.status(); code != http.StatusOK {
			t.Fatalf("status with both listeners up = %d", code)
		}

		// one more registered socket that never serves
		packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer packetConn.Close()
		env.alt.AddListener(ListenerTransportH3, packetConn)
		if err := env.alt.ListenerReady(); err == nil {
			t.Fatal("alt is ready with a listener that never started")
		} else if !strings.Contains(err.Error(), packetConn.LocalAddr().String()) {
			t.Fatalf("readiness error does not name the down listener: %s", err)
		}
		if code := env.status(); code != http.StatusServiceUnavailable {
			t.Fatalf("status with a down listener = %d", code)
		}
	})
}

// An alt with no socket at all is never ready, so an empty allocation cannot
// authorize an activation.
func TestAltStatusIsNotReadyWithoutAListener(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		settings := DefaultSettings()
		settings.ApiHosts = []string{testApiHost}
		settings.ConnectHosts = []string{testConnectHost}
		bare, err := NewAlt(ctx, env.connectHandler, http.NotFoundHandler(), settings)
		if err != nil {
			t.Fatal(err)
		}
		defer bare.cancel()
		if err := bare.ListenerReady(); err == nil {
			t.Fatal("an alt with no listener is ready")
		}
	})
}

// A raw QUIC dial to the fixture's h3 socket, which is what reaches the
// dispatch before either front sees a request.
func (self *altEnv) dialQuicWithTlsConfig(tlsConfig *tls.Config) (*quic.Conn, error) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		self.t.Fatal(err)
	}
	quicTransport := &quic.Transport{
		Conn: packetConn,
	}
	self.closeErrors = append(self.closeErrors, func() {
		quicTransport.Close()
		packetConn.Close()
	})
	ctx, cancel := context.WithTimeout(self.ctx, testProgressTimeout)
	defer cancel()
	return quicTransport.DialEarly(
		ctx,
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: self.h3Port},
		tlsConfig,
		&quic.Config{
			HandshakeIdleTimeout: testProgressTimeout,
			InitialPacketSize:    connectcore.H3InitialPacketByteCount,
		},
	)
}

// The same dial presenting serverName. nextProtos is the client's alpn offer:
// an api client offers h3, and the connect transport offers nothing at all.
func (self *altEnv) dialQuic(serverName string, nextProtos []string) (*quic.Conn, error) {
	return self.dialQuicWithTlsConfig(&tls.Config{
		RootCAs:    testFixtureRoots(self.t, self.transportTls, serverName),
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
		NextProtos: nextProtos,
	})
}

// The same dial with no server name at all, which is what a client that sends
// no sni presents. The certificate then comes from the front's default host
// name, so the handshake completes and the dispatch decides on the empty name.
func (self *altEnv) dialQuicWithoutServerName() (*quic.Conn, error) {
	return self.dialQuicWithTlsConfig(&tls.Config{
		// no server name means no certificate to verify it against
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{http3.NextProtoH3},
	})
}

// How the server closed one connection, as the client can observe it.
//
// RFC 9000 forbids an application CONNECTION_CLOSE in an Initial or Handshake
// packet, so a refusal the client reads before it has processed the server's
// handshake flight arrives as the transport APPLICATION_ERROR with alt's own
// code substituted away. That is still alt refusing the connection; only the
// code is lost. The second return says whether the code survived.
func altCloseErrorCode(t testing.TB, conn *quic.Conn) (quic.ApplicationErrorCode, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testProgressTimeout)
	defer cancel()
	_, err := conn.AcceptStream(ctx)
	if err == nil {
		t.Fatal("the connection was not closed")
	}
	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) {
		return applicationErr.ErrorCode, true
	}
	var transportErr *quic.TransportError
	if errors.As(err, &transportErr) && transportErr.ErrorCode == quic.ApplicationErrorErrorCode {
		return 0, false
	}
	t.Fatalf("close err = %v (%T), want an application close", err, err)
	return 0, false
}

// The application error code the server closed one connection with, which must
// have survived.
func altConnApplicationErrorCode(t testing.TB, conn *quic.Conn) quic.ApplicationErrorCode {
	t.Helper()
	code, survived := altCloseErrorCode(t, conn)
	if !survived {
		t.Fatal("the connection was closed before the handshake, so no code survived")
	}
	return code
}

// A name in neither list is closed with an application close before either
// front is entered, carrying alt's own code when the connection was far enough
// along to keep it.
func TestAltRefusesAnUnknownServerNameWithAnApplicationClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		conn, err := env.dialQuic(testUnknownHost, []string{http3.NextProtoH3})
		if err != nil {
			t.Fatal(err)
		}
		if code, survived := altCloseErrorCode(t, conn); survived && code != ErrorCodeUnknownServerName {
			t.Fatalf("unknown name error code = %#x, want %#x", code, ErrorCodeUnknownServerName)
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

// A client that presents no sni at all is refused with alt's own application
// error code, which is outside the http3 range so an http3 client reports it as
// an application error rather than as a protocol violation.
//
// An empty name is also the one refusal the code is reliably observable on: the
// dispatch has to wait for the handshake to learn there is no name, so by the
// time it refuses, the connection is past the point where RFC 9000 would
// substitute the code away. It therefore pins both the wait and the code.
func TestAltRefusesAConnectionWithNoServerName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, func(settings *Settings) {
			// a certificate for a name that is on neither front's list, so the
			// handshake completes and the dispatch is what refuses
			settings.ExchangeSettings.TransportTlsSettings.DefaultHostName = testUnknownHost
		})
		defer env.Close()

		conn, err := env.dialQuicWithoutServerName()
		if err != nil {
			t.Fatal(err)
		}
		if code := altConnApplicationErrorCode(t, conn); code != ErrorCodeUnknownServerName {
			t.Fatalf("empty name error code = %#x, want %#x", code, ErrorCodeUnknownServerName)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontRefused || dispatch.serverName != "" {
			t.Fatalf("empty name dispatch = %+v", dispatch)
		}
		select {
		case path := <-env.apiRequestPaths:
			t.Fatalf("a refused connection reached the api front: %s", path)
		default:
		}
	})
}

// The refusal codes are outside the http3 error range, so an http3 client
// reports a refused connection as an application error rather than as a
// protocol violation, and the two refusals are told apart by their code.
func TestAltRefusalErrorCodesAreOutsideTheHttp3Range(t *testing.T) {
	if ErrorCodeUnknownServerName == ErrorCodeRateLimited {
		t.Fatal("the two refusals share one code")
	}
	// the http3 codes are 0x0100 to 0x0110 plus the 0x1f * N + 0x21 greasing
	for _, code := range []quic.ApplicationErrorCode{
		ErrorCodeUnknownServerName,
		ErrorCodeRateLimited,
	} {
		if code <= 0x0110 {
			t.Errorf("%#x is inside the http3 error range", code)
		}
	}
}

// The connect front refuses a connection above the per-address concurrent
// connection cap with the rate limit code, before any stream is served (L5).
//
// The slot is taken from the test rather than by a second dial, so the refusal
// is decided by the cap and never by which of two dials arrived first. A
// connection that IS admitted reaches the connect handler, which closes it with
// its own code when the auth frame does not arrive -- so the two outcomes are
// told apart by the code rather than by timing.
func TestAltRefusesAConnectConnectionOverTheConnectionCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, func(settings *Settings) {
			settings.LimitsSettings.NetConnections = 1
		})
		defer env.Close()

		loopback := netip.MustParseAddr("127.0.0.1")
		release, ok := env.alt.limits.AcquireConnection(loopback, time.Now())
		if !ok {
			t.Fatal("the only connection slot was already taken")
		}

		// the connect transport offers no application protocol at all
		refused, err := env.dialQuic(testConnectHost, nil)
		if err != nil {
			t.Fatal(err)
		}
		if code, survived := altCloseErrorCode(t, refused); survived && code != ErrorCodeRateLimited {
			t.Fatalf("over-cap error code = %#x, want %#x", code, ErrorCodeRateLimited)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect {
			t.Fatalf("over-cap dispatch = %+v", dispatch)
		}

		// with the slot back, the same dial reaches the connect handler
		release()
		admitted, err := env.dialQuic(testConnectHost, nil)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := admitted.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// an empty auth frame ends the handler, which closes with its own code
		stream.Close()
		if code := altConnApplicationErrorCode(t, admitted); code == ErrorCodeRateLimited {
			t.Fatal("an admitted connection was refused by the cap")
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect {
			t.Fatalf("admitted dispatch = %+v", dispatch)
		}
	})
}

// An excluded subnet bypasses the connection cap on the connect front, exactly
// as nginx maps an excluded prefix to an empty limit key. The slot is held for
// the whole test, so nothing but the exclusion can be what admits the dial.
func TestAltAdmitsAnExcludedAddressOverTheConnectionCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, func(settings *Settings) {
			settings.LimitsSettings.NetConnections = 1
			settings.LimitsSettings.ExcludePrefixes = []netip.Prefix{
				netip.MustParsePrefix("127.0.0.0/8"),
			}
		})
		defer env.Close()

		// an excluded address takes no slot, so this cannot exhaust the cap
		release, ok := env.alt.limits.AcquireConnection(netip.MustParseAddr("127.0.0.1"), time.Now())
		if !ok {
			t.Fatal("an excluded address was refused a slot")
		}
		defer release()

		conn, err := env.dialQuic(testConnectHost, nil)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		stream.Close()
		if code := altConnApplicationErrorCode(t, conn); code == ErrorCodeRateLimited {
			t.Fatal("an excluded address was refused by the cap")
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect {
			t.Fatalf("excluded dispatch = %+v", dispatch)
		}
	})
}

// Alt advertises the intersection of its own protocols with the client's offer.
// RFC 9001 makes a QUIC server with any advertised protocol reject a client
// that offers none, so a connect client that offers nothing must be answered
// with nothing rather than with h3.
func TestNegotiableNextProtos(t *testing.T) {
	cases := []struct {
		offered []string
		want    []string
	}{
		{offered: nil, want: nil},
		{offered: []string{}, want: nil},
		{offered: []string{http3.NextProtoH3}, want: []string{http3.NextProtoH3}},
		{offered: []string{"h2", http3.NextProtoH3}, want: []string{http3.NextProtoH3}},
		{offered: []string{"h2", "http/1.1"}, want: nil},
	}
	for _, c := range cases {
		got := negotiableNextProtos(c.offered)
		if !slices.Equal(got, c.want) {
			t.Errorf("negotiableNextProtos(%v) = %v, want %v", c.offered, got, c.want)
		}
	}
}

// The whodis listener has to decode something, so a settings block with no tld
// is refused at construction rather than binding a socket that answers nothing.
func TestNewAltRefusesNoDnsTld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		settings := DefaultSettings()
		settings.ApiHosts = []string{testApiHost}
		settings.ConnectHosts = []string{testConnectHost}
		settings.DnsTlds = nil
		if _, err := NewAlt(ctx, env.connectHandler, http.NotFoundHandler(), settings); err == nil {
			t.Fatal("an alt with no dns tld was constructed")
		}
	})
}

// A drain releases both fronts and every listener with them, so warp can
// activate a replacement without this block taking new work. The listeners
// returning is the barrier the drain is observed on -- readiness follows from
// it, and a process that kept accepting would never reach it.
func TestAltCloseReleasesEveryListener(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		// serving, on both listeners, before the drain
		for _, whodis := range []bool{false, true} {
			code, body, err := altGet(t, env.apiClient(testApiHost, whodis), testApiHost, "/hello")
			if err != nil {
				t.Fatal(err)
			}
			if code != http.StatusOK || body != "hello" {
				t.Fatalf("api before the drain (whodis %t) = %d %q", whodis, code, body)
			}
		}
		if err := env.alt.ListenerReady(); err != nil {
			t.Fatalf("alt is not ready before the drain: %s", err)
		}
		if len(env.listenerDone) == 0 {
			t.Fatal("the fixture registered no listener")
		}

		env.Close()

		for i, done := range env.listenerDone {
			select {
			case <-done:
			case <-time.After(testProgressTimeout):
				t.Fatalf("listener %d did not return with the drain", i)
			}
		}
		if err := env.alt.ListenerReady(); err == nil {
			t.Fatal("alt is ready after every listener returned")
		}
	})
}
