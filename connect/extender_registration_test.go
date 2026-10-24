package connect

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	connectextender "github.com/urnetwork/connect/extender"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Extender registration across the whole path an extender takes
// (connect/EXTENDER.md C2, lag.dev/PLAN.md C4): connect's activation loop and
// measurement certificate, as `connectctl extender` and the sdk provider run
// them, post with a client jwt to the api's own routes and handlers. The
// handler probes the extender back on its carriers and forwards a /hello
// through it, stores it, and stores the measurement request the next
// activation carries for the id the first one answered.
//
// Every other test of registration stands in for one side: the controller's
// call the handler directly, and connect's and the sdk's post to a stub. This
// one runs both, so a request one side never sends or the other never stores
// fails here. lag.dev went empty on Main with every extender activating and
// none asking for a certificate (2026-10-22).

const (
	testRegistrationApiHost     = "api.registration.example"
	testRegistrationNetworkHost = "ur.registration.example"
	testRegistrationDnsTld      = "r.example."
	testRegistrationZone        = "extender.registration.example"
)

// The api /hello an activation's forward probe reaches through the extender,
// on both loopback families under one certificate for the api name.
type testRegistrationHello struct {
	rootCAs *x509.CertPool
	// dial network to the listener address of that family
	familyAddresses map[string]string
}

func newTestRegistrationHello(t testing.TB) *testRegistrationHello {
	t.Helper()
	certificate := testRegistrationCertificate(t, testRegistrationApiHost)
	hello := &testRegistrationHello{
		rootCAs:         x509.NewCertPool(),
		familyAddresses: map[string]string{},
	}
	hello.rootCAs.AddCert(certificate.Leaf)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/hello" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"client_address":%q}`, req.RemoteAddr)
	})
	for _, family := range []struct {
		network string
		address string
	}{
		{network: "tcp4", address: "127.0.0.1:0"},
		{network: "tcp6", address: "[::1]:0"},
	} {
		listener, err := net.Listen(family.network, family.address)
		if err != nil {
			// a host with no ::1 probes the family it has
			continue
		}
		helloServer := httptest.NewUnstartedServer(handler)
		helloServer.Listener.Close()
		helloServer.Listener = listener
		helloServer.TLS = &tls.Config{Certificates: []tls.Certificate{*certificate}}
		helloServer.StartTLS()
		hello.familyAddresses[family.network] = listener.Addr().String()
		t.Cleanup(helloServer.Close)
	}
	return hello
}

// A self-signed certificate for one name, usable as its own root.
func testRegistrationCertificate(t testing.TB, host string) *tls.Certificate {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey, Leaf: leaf}
}

// One extender with its tcp carrier bound on every address, so the probe of
// the activation reaches it at the loopback address it posted from, and whose
// forward resolves the api name to the /hello listener.
type testRegistrationExtender struct {
	keySeed   []byte
	publicKey ed25519.PublicKey
	tcpPort   int
	server    *connectextender.ExtenderServer
}

func newTestRegistrationExtender(t testing.TB, hello *testRegistrationHello) *testRegistrationExtender {
	t.Helper()
	keySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	tcpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	ext := &testRegistrationExtender{
		keySeed:   keySeed,
		publicKey: publicKey,
		tcpPort:   tcpListener.Addr().(*net.TCPAddr).Port,
	}

	settings := connectextender.DefaultExtenderSettings()
	settings.DnsTlds = []string{testRegistrationDnsTld}
	settings.HeaderTimeout = 5 * time.Second
	settings.IdentityKeySeed = keySeed
	settings.ProxyTlsConfig = &tls.Config{RootCAs: hello.rootCAs}
	settings.Listen = func(network string, address string) (net.Listener, error) {
		if address != fmt.Sprintf(":%d", ext.tcpPort) {
			return nil, fmt.Errorf("unexpected extender listen %s %s", network, address)
		}
		return tcpListener, nil
	}
	settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if host != testRegistrationApiHost {
			return nil, fmt.Errorf("the extender dialed %s, which is not the api", host)
		}
		familyAddress, ok := hello.familyAddresses[network]
		if !ok {
			return nil, fmt.Errorf("no %s address for %s", network, host)
		}
		return (&net.Dialer{}).DialContext(ctx, network, familyAddress)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ext.server = connectextender.NewExtenderServer(
		ctx,
		[]string{},
		[]string{testRegistrationApiHost},
		map[int][]connect.ExtenderConnectMode{
			ext.tcpPort: {connect.ExtenderConnectModeTcpTls},
		},
		&net.Dialer{},
		settings,
	)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ext.server.ListenAndServe()
	}()
	t.Cleanup(func() {
		ext.server.CloseAndWait()
		cancel()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("extender server did not stop")
		}
	})
	return ext
}

// Installs an `extender.yml` with the measurement block on, naming the hello
// api, and verifies the probe's inner tls against the hello certificate.
func installTestRegistrationConfig(t testing.TB, hello *testRegistrationHello) {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(strings.Join([]string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPublicKey)),
		fmt.Sprintf("network_host: %s", testRegistrationNetworkHost),
		"network_hosts:",
		fmt.Sprintf("  - %s", testRegistrationNetworkHost),
		fmt.Sprintf("  - %s", testRegistrationApiHost),
		fmt.Sprintf("api_url: https://%s", testRegistrationApiHost),
		"dns:",
		"  enabled: false",
		"measurement:",
		"  enabled: true",
		"  zone_name: " + testRegistrationZone,
	}, "\n")))
	controller.Testing_ResetExtenderConfig()
	restoreProbe := controller.Testing_SetExtenderForwardProbeRootCAs(hello.rootCAs)
	t.Cleanup(func() {
		restoreProbe()
		pop()
		controller.Testing_ResetExtenderConfig()
	})
}

// Serves the api's own activation and certificate routes and handlers on
// loopback, and returns the url an extender posts to.
func newTestRegistrationApi(t testing.TB, ctx context.Context) string {
	t.Helper()
	routes := []*router.Route{
		router.NewRoute("POST", connect.ExtenderActivatePath, handlers.ExtenderActivate),
		router.NewRoute("GET", connectextender.ExtenderMeasurementCertPath, handlers.ExtenderMeasurementCert),
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiServer := httptest.NewUnstartedServer(router.NewRouter(ctx, routes))
	apiServer.Listener.Close()
	apiServer.Listener = listener
	apiServer.Start()
	t.Cleanup(apiServer.Close)
	return apiServer.URL
}

// A client jwt for a new network, user, device and client, as an extender
// activates under.
func newTestRegistrationClientJwt(ctx context.Context) string {
	networkId := server.NewId()
	userId := server.NewId()
	deviceId := server.NewId()
	clientId := server.NewId()
	networkName := fmt.Sprintf("extender-registration-%s", networkId)
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "extender", "extender-registration")
	return session.NewByJwt(networkId, userId, networkName, false, false).Client(deviceId, clientId).Testing_Sign()
}

// Runs connect's activation loop for ext against the api at apiUrl, asking for
// a certificate for measurementCert's key when it is set, as an extender with
// a kept measurement key does.
func startTestRegistration(
	t testing.TB,
	ctx context.Context,
	ext *testRegistrationExtender,
	apiUrl string,
	byJwt string,
	measurementCert *connectextender.ExtenderMeasurementCert,
) *connect.ExtenderActivator {
	t.Helper()
	settings := connect.DefaultExtenderActivatorSettings()
	// the plain url: one post a pass, whose family the operator derives from
	// the caller address (C2)
	settings.ApiUrl = apiUrl
	settings.ByJwt = func() string { return byJwt }
	settings.ClientStrategy = connect.NewClientStrategyWithDefaults(ctx)
	settings.PublicKey = ext.publicKey
	settings.TcpPort = ext.tcpPort
	settings.DnsTld = testRegistrationDnsTld
	settings.Carriers = func() []string {
		return []string{connect.ExtenderCarrierTcp}
	}
	if measurementCert != nil {
		settings.MeasurementCsr = measurementCert.ActivationCsr
	}
	activator := connect.NewExtenderActivator(ctx, settings)
	t.Cleanup(activator.Close)
	return activator
}

// Waits for the active extender stored for publicKey, and returns its id.
func waitTestRegistered(t testing.TB, ctx context.Context, publicKey ed25519.PublicKey) server.Id {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, entry := range model.GetRandomActiveNetworkExtenders(ctx, 64, server.Id{}) {
			if string(entry.Extender.PublicKey) == string(publicKey) {
				return entry.Extender.ExtenderId
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the extender was never registered")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// An extender with a measurement key registers through the api, learns its id,
// and asks for a certificate for that id's measurement name, which the api
// stores pending for the issuance task (C4, C5).
func TestExtenderRegistrationThroughTheApiStoresTheMeasurementRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hello := newTestRegistrationHello(t)
		installTestRegistrationConfig(t, hello)
		ext := newTestRegistrationExtender(t, hello)
		apiUrl := newTestRegistrationApi(t, ctx)

		measurementCert, err := connectextender.NewExtenderMeasurementCert(
			filepath.Join(t.TempDir(), connectextender.ExtenderMeasurementKeyFileName),
			testRegistrationZone,
		)
		if err != nil {
			t.Fatal(err)
		}
		startTestRegistration(t, ctx, ext, apiUrl, newTestRegistrationClientJwt(ctx), measurementCert)

		extenderId := waitTestRegistered(t, ctx, ext.publicKey)
		name := controller.ExtenderMeasurementName(extenderId, testRegistrationZone)
		deadline := time.Now().Add(30 * time.Second)
		var measurement *model.NetworkExtenderMeasurement
		for {
			measurement = model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			if measurement != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the activation loop never carried a measurement request")
			}
			time.Sleep(100 * time.Millisecond)
		}
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		if !slices.Equal(measurement.Names, []string{name}) {
			t.Fatalf("names = %v, expected %v", measurement.Names, []string{name})
		}
		expectedPublicKey, err := x509.MarshalPKIXPublicKey(measurementCert.PublicKey())
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, slices.Equal(measurement.PublicKey, expectedPublicKey), true)
	})
}

// An extender without a measurement key -- the sn miner before 2026-10-22 --
// still registers through the api, and leaves nothing for lag.dev: no request
// is stored, so no name is issued or offered (A4).
func TestExtenderRegistrationThroughTheApiWithoutAMeasurementKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hello := newTestRegistrationHello(t)
		installTestRegistrationConfig(t, hello)
		ext := newTestRegistrationExtender(t, hello)
		apiUrl := newTestRegistrationApi(t, ctx)

		activator := startTestRegistration(t, ctx, ext, apiUrl, newTestRegistrationClientJwt(ctx), nil)
		extenderId := waitTestRegistered(t, ctx, ext.publicKey)
		// a pass after the one that registered, which is where a request would
		// have ridden
		activator.Reactivate()
		time.Sleep(2 * time.Second)
		if measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId); measurement != nil {
			t.Fatalf("a request was stored with no measurement key: %v", measurement.State)
		}
	})
}
