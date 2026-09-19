package mcp

// A full in-process urnetwork stack for end-to-end fetch tests, plus a local
// test web server reachable through the stack's provider egress.
//
// This mirrors server/proxy/proxy_test.go's setup, adapted so the egress
// target is a local httptest server rather than the real internet. It stands
// up, in-process:
//
//   - a real connect server (exchange + connect handler) on a plain ws port
//   - a real api server (the full api.Routes()) on a plain http port
//   - a local provider built from connect primitives (client strategy, out of
//     band control, client, platform transport, local user nat, remote user
//     nat provider) that egresses through the host network stack
//   - a proxy device config + proxy client, and the real proxy http/https
//     ingress servers
//   - an httptest web server on 127.0.0.1 serving a small page with static
//     sub-resources, a cookie pair, and a slow endpoint
//
// The proxy device is pinned to the local provider client id, resolved through
// the real find-providers2 discovery path, so traffic driven at the proxy
// ingress leaves through the provider and lands on the httptest server.
//
// Loopback egress: the production security policy allows only public unicast
// destinations for public provide relationships, so 127.0.0.1 would be an
// incident on both the client egress inspection and the provider ingress
// inspection. Both sides are therefore wired to
// connect.DisableSecurityPolicyWithStats here (see setupFetchTestStack).
//
// Like proxy_test, this expects the standard local test environment
// (WARP_ENV=local plus the local postgres/redis and vault, e.g. via test.sh).

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/api"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/proxy"
	"github.com/urnetwork/server/v2026/router"
)

const (
	// distinct from the proxy package harness so both test binaries can run at
	// the same time without fighting over the exchange service socket
	fetchTestConnectServicePort = 7310

	fetchTestInitialBalance = model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024)

	// recognizable in the body of the test web server's index page
	fetchTestPageMarker = "URNETWORK_FETCH_TEST_PAGE"

	fetchTestCookieName = "urtest"

	// /slow holds the response long enough to exercise timeout and
	// continuation handling
	fetchTestSlowDelay = 2 * time.Second
)

// 1x1 png, enough to be a valid decodable image
const fetchTestPngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// Handles the stack a fetch test drives. All fields are read-only after
// setupFetchTestStack returns.
type fetchTestStack struct {
	t      testing.TB
	ctx    context.Context
	cancel context.CancelFunc

	// basic-auth username at the proxy ingress (empty password)
	signedProxyId    string
	mcpSignedProxyId string
	proxyClient      *model.ProxyClient

	// base url of the local web server, reachable through the provider egress
	webUrl string

	pdNetworkId       server.Id
	pdUserId          server.Id
	pdClientId        server.Id
	providerNetworkId server.Id
	providerClientId  server.Id
	proxyId           server.Id

	apiUrl string

	// proxy ingress ports on 127.0.0.1
	httpPort  int
	httpsPort int

	proxyDeviceManager *proxy.ProxyDeviceManager
	connectServer      *connectserver.ConnectHandler
	webServer          *httptest.Server

	closeOnce sync.Once
}

// Selects production security policy and optional target-request observation.
type fetchTestStackOptions struct {
	disableSecurityPolicies bool
	onWebRequest            func()
}

// Tears down the stack. Admission is stopped before the root ctx is canceled:
// connect's deferred rate-limit decrement uses redis, so every admitted
// handler must finish before DefaultTestEnv closes this test's redis pool.
func (self *fetchTestStack) close() {
	self.closeOnce.Do(func() {
		self.connectServer.Close()
		self.cancel()

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		if !self.connectServer.WaitForIdle(closeCtx) {
			self.t.Errorf("connect handlers did not finish during stack teardown")
		}

		self.webServer.Close()
	})
}

// Builds an http client that reaches the local web server through the proxy's
// plain http ingress. The proxy reads Proxy-Authorization basic, taking the
// username as the signed proxy id and ignoring the password (see
// authHeaderProxyId in server/proxy/server.go).
func (self *fetchTestStack) httpProxyClient(timeout time.Duration) (*http.Client, error) {
	proxyUrl, err := url.Parse(fmt.Sprintf("http://%s:@127.0.0.1:%d", self.signedProxyId, self.httpPort))
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyUrl),
		},
		Timeout: timeout,
	}, nil
}

// Reserved ingress ports for the proxy http servers.
type fetchTestPorts struct {
	http  int
	https int
}

// reserveFetchTestPorts allocates the proxy http/https listen ports through
// server.ReserveTestListenPorts: probed on the wildcard address the servers
// actually bind, from below the OS ephemeral range so the release -> bind
// window cannot lose a port to the process's own outbound dials (see the
// allocator doc in server/test_util.go; certification failure c12-1).
func reserveFetchTestPorts(t testing.TB) (*fetchTestPorts, func()) {
	ports, release, err := server.ReserveTestListenPorts("tcp", "tcp")
	if err != nil {
		t.Fatalf("reserve fetch test ports: %v", err)
	}
	return &fetchTestPorts{
		http:  ports[0],
		https: ports[1],
	}, release
}

// An already-bound listener, so an in-process server keeps its OS-assigned
// port continuously from setup through serving.
func listenFetchTestTcp(t testing.TB) net.Listener {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on dynamic test port: %v", err)
	}
	return listener
}

func fetchTestTcpPort(listener net.Listener) int {
	return listener.Addr().(*net.TCPAddr).Port
}

// Fills in the warp environment the server packages read at startup.
func setFetchTestEnv() {
	// WARP_ENV must already be "local" (asserted by DefaultTestEnv before setup).
	setIfEmpty := func(k string, v string) {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	setIfEmpty("WARP_SERVICE", "test")
	setIfEmpty("WARP_BLOCK", "test")
	setIfEmpty("WARP_VERSION", "0.0.0")
	setIfEmpty("WARP_DOMAIN", "bringyour.com")
	// 0 disables the (linux-only) SO_MARK, read via RequireFwMark()
	setIfEmpty("WARP_FWMARK", "0")
}

// Polls cond until it holds, failing the test at the timeout.
func fetchTestWaitFor(t testing.TB, timeout time.Duration, desc string, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// Tops up a network's transfer balance so its client can open contracts.
func fetchTestRedeemBalance(t testing.TB, ctx context.Context, networkId server.Id, initialTransferBalance model.ByteCount) {
	balanceCode, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		365*24*time.Hour,
		0,
		// unique per call so a network can be topped up repeatedly
		fmt.Sprintf("test-%s-%s", networkId, server.NewId()),
		"",
		"",
	)
	if err != nil {
		t.Fatalf("create balance code: %v", err)
	}
	result, err := model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: networkId,
		},
		ctx,
	)
	if err != nil {
		t.Fatalf("redeem balance code: %v", err)
	}
	if result.Error != nil {
		t.Fatalf("redeem balance code: %v", result.Error.Message)
	}
}

// Inserts a small block of high-sequence rows. CreateProxyClient selects a row
// with sequence_id >= rand(0, ~31/32*ProxyClientIpv4Count), so the rows must
// sit near the top of the sequence space.
func seedFetchTestProxyClientIpv4(t testing.TB, ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		base := int64(model.ProxyClientIpv4Count - 64)
		for i := 0; i < 64; i += 1 {
			seq := base + int64(i)
			// arbitrary distinct ipv4 (10.b.c.d), avoiding 10.0.0.0
			ipv4 := int64(0x0A000000) + seq + 1
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO proxy_client_ipv4 (sequence_id, client_ipv4)
				VALUES ($1, $2)
				`,
				seq,
				ipv4,
			))
		}
	})
}

// Starts the local web server on 127.0.0.1. The index page references its
// sub-resources both relatively and by absolute url, so a fetch that follows
// sub-resources exercises both forms against the same origin. The callback,
// when present, observes every request before the route handler runs.
func startFetchTestWebServer(t testing.TB, onWebRequest func()) *httptest.Server {
	pngBytes, err := base64.StdEncoding.DecodeString(fetchTestPngBase64)
	if err != nil {
		t.Fatalf("decode test png: %v", err)
	}

	// the absolute-url resource needs the assigned port, which is only known
	// after the server starts; requests cannot arrive before then
	var webServer *httptest.Server

	mux := http.NewServeMux()

	mux.HandleFunc("/img.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes)
	})

	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, `
body { background: #101010; color: #f0f0f0; font-family: sans-serif; }
img { image-rendering: pixelated; }
`)
	})

	mux.HandleFunc("/setcookie", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:  fetchTestCookieName,
			Value: "1",
			Path:  "/",
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><body><p>cookie set</p></body></html>`)
	})

	mux.HandleFunc("/showcookie", func(w http.ResponseWriter, r *http.Request) {
		value := "absent"
		if cookie, err := r.Cookie(fetchTestCookieName); err == nil {
			value = cookie.Value
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "cookie=%s\n", value)
	})

	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(fetchTestSlowDelay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "slow ok\n")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html>
<html>
<head>
<title>%s</title>
<link rel="stylesheet" href="/style.css">
</head>
<body>
<h1>%s</h1>
<p>urnetwork fetch test page</p>
<img src="/img.png" alt="relative">
<img src="%s/img.png" alt="absolute">
<a href="/setcookie">set cookie</a>
<a href="/showcookie">show cookie</a>
<a href="/slow">slow</a>
</body>
</html>
`, fetchTestPageMarker, fetchTestPageMarker, webServer.URL)
	})

	webServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onWebRequest != nil {
			onWebRequest()
		}
		mux.ServeHTTP(w, r)
	}))
	return webServer
}

// Wires up the full local environment and returns the stack. It must be called
// inside DefaultTestEnv().Run (db + redis + migrations ready).
func setupFetchTestStack(t testing.TB) *fetchTestStack {
	return setupFetchTestStackWithOptions(t, &fetchTestStackOptions{
		disableSecurityPolicies: true,
	})
}

func setupFetchTestStackWithOptions(t testing.TB, options *fetchTestStackOptions) *fetchTestStack {
	setFetchTestEnv()

	ctx, cancel := context.WithCancel(context.Background())

	// ---- the local web server the provider egresses to -----------------------
	webServer := startFetchTestWebServer(t, options.onWebRequest)

	// ---- local connect server (plain ws, in-process) -------------------------
	connectHost := "fetchtest"
	service := "connect"
	block := "test"
	routes := map[string]string{connectHost: "127.0.0.1"}
	hostToServicePorts := map[int]int{fetchTestConnectServicePort: fetchTestConnectServicePort}

	exchangeSettings := connectserver.DefaultExchangeSettings()
	exchange := connectserver.NewExchange(ctx, connectHost, service, block, hostToServicePorts, routes, exchangeSettings)

	connectHandlerSettings := connectserver.DefaultConnectHandlerSettings()
	connectHandlerSettings.ConnectionAnnounceTimeout = 0
	// this stack drives the h1 websocket endpoint. Do not also bind the
	// production h3 and dns ports; a running local environment owns them.
	connectHandlerSettings.ListenH3Port = 0
	connectHandlerSettings.ListenDnsPort = 0
	connectHandler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, connectHandlerSettings)

	connectRoutes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", connectHandler.Connect),
	}
	connectListener := listenFetchTestTcp(t)
	connectClientPort := fetchTestTcpPort(connectListener)
	connectHttp := &http.Server{
		Handler: router.NewRouter(ctx, connectRoutes),
	}
	go connectHttp.Serve(connectListener)

	// ---- local api server (plain http, full route set, in-process) -----------
	apiListener := listenFetchTestTcp(t)
	apiPort := fetchTestTcpPort(apiListener)
	apiHttp := &http.Server{
		Handler: router.NewRouter(ctx, api.Routes()),
	}
	go apiHttp.Serve(apiListener)

	go func() {
		<-ctx.Done()
		connectHttp.Close()
		apiHttp.Close()
		exchange.Close()
	}()

	// give the listeners a moment to bind
	select {
	case <-time.After(1 * time.Second):
	}

	apiUrl := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	platformUrl := fmt.Sprintf("ws://127.0.0.1:%d", connectClientPort)

	// ---- the test network space pointing the sdk at the local servers --------
	connectSettings := connect.DefaultConnectSettings()
	networkSpace := sdk.Testing_NewNetworkSpaceWithUrls(
		ctx,
		apiUrl,
		platformUrl,
		connectSettings,
	)
	t.Cleanup(networkSpace.Close)

	// ---- a local provider ----------------------------------------------------
	providerNetworkId := server.NewId()
	providerUserId := server.NewId()
	providerNetworkName := fmt.Sprintf("provider-%s", providerNetworkId)
	providerDeviceId := server.NewId()
	providerClientId := server.NewId()
	providerInstanceId := server.NewId()

	model.Testing_CreateNetwork(ctx, providerNetworkId, providerNetworkName, providerUserId)
	model.Testing_CreateDevice(ctx, providerNetworkId, providerDeviceId, providerClientId, "provider", "provider")
	fetchTestRedeemBalance(t, ctx, providerNetworkId, fetchTestInitialBalance)

	providerByJwt := jwt.NewByJwt(providerNetworkId, providerUserId, providerNetworkName, false, false).
		Client(providerDeviceId, providerClientId).Sign()

	// The sdk's NewPlatformDeviceLocal hardcodes allowProvider=false (it's for
	// embedded source devices that reach providers via the multi-client
	// generator). A real provider needs its own client + platform transport +
	// egress nat, so build it directly from connect primitives.
	providerStrategySettings := connect.DefaultClientStrategySettings()
	providerStrategySettings.EnableResilient = false
	providerClientStrategy := connect.NewClientStrategy(ctx, providerStrategySettings)

	providerOob := connect.NewApiOutOfBandControl(ctx, providerClientStrategy, providerByJwt, apiUrl)
	providerClient := connect.NewClient(ctx, connect.Id(providerClientId), providerOob, connect.DefaultClientSettings())
	go func() {
		<-ctx.Done()
		providerClient.Close()
	}()

	providerAuth := &connect.ClientAuth{
		ByJwt:      providerByJwt,
		InstanceId: connect.Id(providerInstanceId),
		AppVersion: server.RequireVersion(),
	}
	providerTransport := connect.NewPlatformTransportWithDefaults(
		providerClient.Ctx(),
		providerClientStrategy,
		providerClient.RouteManager(),
		platformUrl,
		providerAuth,
	)
	go func() {
		<-ctx.Done()
		providerTransport.Close()
	}()

	// egress via a user-space nat. The default policy allows only public
	// unicast destinations for a public provide relationship, which would make
	// the loopback web server an incident on the provider's ingress
	// inspection, so the policy is disabled here.
	providerLocalUserNat := connect.NewLocalUserNatWithDefaults(providerClient.Ctx(), providerClientId.String())
	providerNatSettings := connect.DefaultRemoteUserNatProviderSettings()
	if options.disableSecurityPolicies {
		providerNatSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	}
	providerRemoteNat := connect.NewRemoteUserNatProvider(providerClient, providerLocalUserNat, providerNatSettings)
	go func() {
		<-ctx.Done()
		providerRemoteNat.Close()
		providerLocalUserNat.Close()
	}()

	// provide public, with return-traffic stream so the source can open both
	// the forward and companion contracts
	providerClient.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:  true,
		protocol.ProvideMode_Network: true,
	})

	// wait for the provider's provide to register on the platform before the
	// proxy device tries to open contracts to it
	fetchTestWaitFor(t, 30*time.Second, "provider provide registered", func() bool {
		modes, err := model.GetProvideModes(ctx, providerClientId)
		return err == nil && len(modes) > 0
	})

	// ---- the proxy device's network/device/client + balance ------------------
	pdNetworkId := server.NewId()
	pdUserId := server.NewId()
	pdNetworkName := fmt.Sprintf("proxydev-%s", pdNetworkId)
	pdDeviceId := server.NewId()
	pdClientId := server.NewId()

	model.Testing_CreateNetwork(ctx, pdNetworkId, pdNetworkName, pdUserId)
	model.Testing_CreateDevice(ctx, pdNetworkId, pdDeviceId, pdClientId, "proxydevice", "mcp")
	fetchTestRedeemBalance(t, ctx, pdNetworkId, fetchTestInitialBalance)

	// the proxy device connects "by location" pinned directly to the provider
	// client id, which the real find-providers2 path resolves
	location := &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{
			ClientId: server.ToSdkId(providerClientId),
		},
	}

	proxyDeviceConfig := &model.ProxyDeviceConfig{
		ProxyDeviceConnection: model.ProxyDeviceConnection{
			ClientId: pdClientId,
		},
		ProxyDeviceMode: model.ProxyDeviceModeDevice,
		InitialDeviceState: &model.ProxyDeviceState{
			Location: location,
		},
	}
	if err := model.CreateProxyDeviceConfig(ctx, proxyDeviceConfig); err != nil {
		t.Fatalf("create proxy device config: %v", err)
	}
	proxyId := proxyDeviceConfig.ProxyId

	// seed a few high-sequence proxy_client_ipv4 rows so CreateProxyClient(wg)
	// can allocate a client ip (avoids the 10M-row ResetProxyClientIpv4)
	seedFetchTestProxyClientIpv4(t, ctx)

	proxyClient, err := model.CreateProxyClient(
		ctx,
		proxyId,
		pdClientId,
		proxyDeviceConfig.InstanceId,
		model.CreateProxyClientOptions{EnableWg: true},
	)
	if err != nil {
		t.Fatalf("create proxy client: %v", err)
	}

	// ---- the real proxy http/https ingress -----------------------------------
	proxySettings := proxy.DefaultProxySettings()
	testPorts, releaseTestPorts := reserveFetchTestPorts(t)
	proxySettings.HttpPort = testPorts.http
	proxySettings.HttpsPort = testPorts.https

	transportTls := server.NewTransportTls(
		map[string]bool{},
		&server.TransportTlsSettings{EnableSelfSign: true, DefaultHostName: "127.0.0.1"},
	)

	// the client side of the same public-unicast rule: without this the proxy
	// device drops its own egress to the loopback web server
	pdmSettings := proxy.DefaultProxyDeviceManagerSettings()
	pdmSettings.NetworkSpace = networkSpace
	if options.disableSecurityPolicies {
		pdmSettings.ClientSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	}
	proxyDeviceManager := proxy.NewProxyDeviceManager(ctx, pdmSettings)
	go func() {
		<-ctx.Done()
		_ = proxyDeviceManager.CloseAndWait(context.Background())
	}()

	releaseTestPorts()
	proxy.NewHttpServer(ctx, cancel, proxyDeviceManager, transportTls, proxySettings)

	// give the proxy listeners a moment to bind
	select {
	case <-time.After(1 * time.Second):
	}

	// warm up the proxy device: open it and wait until it has a usable path to
	// the provider before any traffic is driven through it
	pd, err := proxyDeviceManager.OpenProxyDevice(proxyId)
	if err != nil {
		t.Fatalf("open proxy device: %v", err)
	}
	if ready := pd.WaitForReady(ctx, 60*time.Second); !ready {
		t.Fatalf("proxy device did not become ready (provider not reachable)")
	}
	proxyOwner := model.GetNetworkClient(ctx, proxyDeviceConfig.ClientId)
	if proxyOwner == nil {
		t.Fatalf("mcp proxy owner client %s does not exist", proxyDeviceConfig.ClientId)
	}
	if proxyOwner.NetworkId != pdNetworkId {
		t.Fatalf(
			"mcp proxy fixture owner network=%s device_spec=%q, want network=%s",
			proxyOwner.NetworkId,
			proxyOwner.DeviceSpec,
			pdNetworkId,
		)
	}

	mcpBinding := identityStateBinding(
		pdUserId.String(),
		pdNetworkId,
		fetchTestOAuthClientId,
		McpResource,
	)
	mcpSignedProxyId, err := seal(
		sealLabelProxy,
		mcpBinding,
		&sealedProxyHandle{SignedProxyId: proxyClient.AuthToken},
		fetchSealTtl,
	)
	if err != nil {
		t.Fatalf("seal mcp proxy handle: %v", err)
	}

	return &fetchTestStack{
		t:                  t,
		ctx:                ctx,
		cancel:             cancel,
		signedProxyId:      proxyClient.AuthToken,
		mcpSignedProxyId:   mcpSignedProxyId,
		proxyClient:        proxyClient,
		webUrl:             webServer.URL,
		pdNetworkId:        pdNetworkId,
		pdUserId:           pdUserId,
		pdClientId:         pdClientId,
		providerNetworkId:  providerNetworkId,
		providerClientId:   providerClientId,
		proxyId:            proxyId,
		apiUrl:             apiUrl,
		httpPort:           testPorts.http,
		httpsPort:          testPorts.https,
		proxyDeviceManager: proxyDeviceManager,
		connectServer:      connectHandler,
		webServer:          webServer,
	}
}

// Checks that the stack stands up and that the local web server is reachable
// end to end through the proxy ingress and the provider egress.
func TestFetchStackSanity(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		stack := setupFetchTestStack(t)
		defer stack.close()

		client, err := stack.httpProxyClient(60 * time.Second)
		connect.AssertEqual(t, err, nil)
		defer client.CloseIdleConnections()

		// the first attempts can race the proxy device establishing its path to
		// the provider, so retry until the deadline
		body := ""
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get(fmt.Sprintf("%s/", stack.webUrl))
			if err != nil {
				select {
				case <-time.After(2 * time.Second):
				}
				continue
			}
			bodyBytes, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				select {
				case <-time.After(2 * time.Second):
				}
				continue
			}
			body = string(bodyBytes)
			break
		}

		connect.AssertEqual(t, strings.Contains(body, fetchTestPageMarker), true)
	})
}
