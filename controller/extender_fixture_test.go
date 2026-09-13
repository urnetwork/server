package controller

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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"

	"github.com/urnetwork/server"
)

// The in-process operator fixture for the activation tests
// (connect/EXTENDER.md I).
//
// An activation is only meaningful against a real extender: the handler dials
// back over every carrier, verifies a challenge signature and then forwards a
// real request to the api. So the fixture is the real extender server bound on
// loopback through its listener seams, and a real https api reachable only
// through the extender's forward dial seam. Nothing is stubbed on the path the
// probe takes; what is injected is only where the sockets are.
//
// Both loopback families are bound so an activation can be run over v4 and
// over v6 and the family rules can be observed, which is what the dual-stack
// hosts these tests run on are for.

// The synthetic api name of the fixture. It is the operator host of the
// configuration, so it is on the extender whitelist and is a valid forward
// destination (A5); the probe's own outer sni is never this name.
const testExtenderApiHost = "api.example"

// The operator's primary host.
const testExtenderNetworkHost = "ur.example"

// The synthetic encoding tld of the dns carrier.
const testExtenderDnsTld = "x.example."

// testExtenderApi is one https api on both loopback families, playing the part
// of the operator's `GET /hello`.
type testExtenderApi struct {
	certificate *tls.Certificate
	rootCAs     *x509.CertPool
	// dial network to the listener address of that family
	familyAddresses map[string]string

	stateLock sync.Mutex
	// when set, the address /hello reports instead of the caller's own, which
	// is how a forward that lands on the wrong family is reproduced
	clientAddressOverride string
}

func (self *testExtenderApi) setClientAddressOverride(clientAddress string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientAddressOverride = clientAddress
}

func (self *testExtenderApi) helloClientAddress(remoteAddr string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.clientAddressOverride != "" {
		return self.clientAddressOverride
	}
	return remoteAddr
}

// Starts the api on 127.0.0.1 and on ::1 with one certificate for the api
// name, and returns the pool that verifies it. The probe's inner tls verifies
// this pool normally, exactly as production verifies the platform roots.
func newTestExtenderApi(t testing.TB) *testExtenderApi {
	t.Helper()
	certificate, err := testExtenderCertificate(testExtenderApiHost)
	if err != nil {
		t.Fatal(err)
	}
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(certificate.Leaf)

	api := &testExtenderApi{
		certificate:     certificate,
		rootCAs:         rootCAs,
		familyAddresses: map[string]string{},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/hello" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(
			w,
			`{"client_address":%q}`,
			api.helloClientAddress(req.RemoteAddr),
		)
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
			t.Fatalf("api listen %s: %v", family.network, err)
		}
		apiServer := httptest.NewUnstartedServer(handler)
		apiServer.Listener.Close()
		apiServer.Listener = listener
		apiServer.TLS = &tls.Config{
			Certificates: []tls.Certificate{*certificate},
		}
		apiServer.StartTLS()
		api.familyAddresses[family.network] = listener.Addr().String()
		t.Cleanup(apiServer.Close)
	}
	return api
}

// testExtenderFixture is one extender with all three carriers bound, serving
// both loopback families on one set of ports and forwarding only to the
// fixture api.
//
// The sockets are wildcard binds, which is what makes one fixture reachable at
// 127.0.0.1 and at ::1 on the SAME ports. A real extender is one host with one
// identity key on two families, and an activation over each family has to
// reach the same ports for the second one to add an address rather than
// replace the first.
type testExtenderFixture struct {
	api      *testExtenderApi
	server   *extender.ExtenderServer
	tcpPort  int
	quicPort int
	// every bound dns carrier port, ascending, and the first of them, which is
	// the configured port an activation names in `dns_port` (L2)
	dnsPorts  []int
	dnsPort   int
	publicKey ed25519.PublicKey
	errors    chan error
	serveDone chan error
}

// Binds the extender with an identity key it signs challenges with. An empty
// secret list is an open extender, which is what an operator activated
// extender is (A4).
func newTestExtenderFixture(t testing.TB) *testExtenderFixture {
	t.Helper()
	return newTestExtenderFixtureWithDnsPorts(t, 1)
}

// The same fixture with dnsPortCount dns carrier ports bound rather than one,
// which is what an extender that binds 53 as well as the whodis port looks
// like (L2). The ports are held ascending, so a test can assert the order a
// record lists them in without knowing which ephemeral ports the kernel gave
// it.
func newTestExtenderFixtureWithDnsPorts(t testing.TB, dnsPortCount int) *testExtenderFixture {
	t.Helper()
	api := newTestExtenderApi(t)

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
	quicPacketConn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	dnsPacketConns := map[int]net.PacketConn{}
	dnsPorts := []int{}
	for range dnsPortCount {
		dnsPacketConn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		dnsPort := dnsPacketConn.LocalAddr().(*net.UDPAddr).Port
		dnsPacketConns[dnsPort] = dnsPacketConn
		dnsPorts = append(dnsPorts, dnsPort)
	}
	slices.Sort(dnsPorts)
	fixture := &testExtenderFixture{
		api:       api,
		tcpPort:   tcpListener.Addr().(*net.TCPAddr).Port,
		quicPort:  quicPacketConn.LocalAddr().(*net.UDPAddr).Port,
		dnsPorts:  dnsPorts,
		dnsPort:   dnsPorts[0],
		publicKey: publicKey,
		errors:    make(chan error, 64),
	}

	settings := extender.DefaultExtenderSettings()
	settings.DnsTlds = []string{testExtenderDnsTld}
	settings.HeaderTimeout = 5 * time.Second
	settings.IdentityKeySeed = keySeed
	settings.SpoofDomains = []string{"spoof.example"}
	settings.ProxyTlsConfig = &tls.Config{RootCAs: api.rootCAs}
	settings.Listen = func(network string, address string) (net.Listener, error) {
		if address != fmt.Sprintf(":%d", fixture.tcpPort) {
			return nil, fmt.Errorf("unexpected extender listen %s %s", network, address)
		}
		return tcpListener, nil
	}
	settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
		if address == fmt.Sprintf(":%d", fixture.quicPort) {
			return quicPacketConn, nil
		}
		for dnsPort, dnsPacketConn := range dnsPacketConns {
			if address == fmt.Sprintf(":%d", dnsPort) {
				return dnsPacketConn, nil
			}
		}
		return nil, fmt.Errorf("unexpected extender listen packet %s %s", network, address)
	}
	// the forward resolves the api name to the loopback listener of the family
	// the extender narrowed the dial to (A7), so a forward that arrives on the
	// wrong family is impossible to produce by accident here
	settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if host != testExtenderApiHost {
			return nil, fmt.Errorf("the extender dialed %s, which is not the api", host)
		}
		familyAddress, ok := api.familyAddresses[network]
		if !ok {
			return nil, fmt.Errorf("no %s address for %s", network, host)
		}
		return (&net.Dialer{}).DialContext(ctx, network, familyAddress)
	}
	settings.ErrorHandler = func(stage string, err error) {
		select {
		case fixture.errors <- fmt.Errorf("%s: %w", stage, err):
		default:
		}
	}

	ports := map[int][]connect.ExtenderConnectMode{
		fixture.tcpPort:  {connect.ExtenderConnectModeTcpTls},
		fixture.quicPort: {connect.ExtenderConnectModeQuic},
	}
	for _, dnsPort := range dnsPorts {
		ports[dnsPort] = []connect.ExtenderConnectMode{connect.ExtenderConnectModeDns}
	}

	ctx, cancel := context.WithCancel(context.Background())
	fixture.server = extender.NewExtenderServer(
		ctx,
		[]string{},
		[]string{testExtenderApiHost},
		ports,
		&net.Dialer{},
		settings,
	)
	fixture.serveDone = make(chan error, 1)
	go func() {
		fixture.serveDone <- fixture.server.ListenAndServe()
	}()
	t.Cleanup(func() {
		fixture.server.CloseAndWait()
		cancel()
		select {
		case err := <-fixture.serveDone:
			if err != nil {
				t.Errorf("extender server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("extender server did not stop")
		}
	})
	return fixture
}

// The activation arguments that match this fixture exactly.
func (self *testExtenderFixture) activateArgs() *ExtenderActivateArgs {
	return &ExtenderActivateArgs{
		PublicKeyHex: hex.EncodeToString(self.publicKey),
		TcpPort:      self.tcpPort,
		UdpPort:      self.quicPort,
		DnsPort:      self.dnsPort,
		DnsTld:       testExtenderDnsTld,
		Carriers: []string{
			connect.ExtenderCarrierTcp,
			connect.ExtenderCarrierQuic,
			connect.ExtenderCarrierDns,
		},
	}
}

// A udp port with nothing bound to it, the highest free one below belowPort so
// it sorts ahead of the fixture's own ports in a dns port list.
//
// A probe of a dead udp port has nothing to fail fast on -- it spends its whole
// budget -- so a list that LEADS with one is what proves the per-port budget:
// the ports behind it must still be probed and the forward must still have
// budget left.
func testClosedUdpPortBelow(t testing.TB, belowPort int) int {
	t.Helper()
	for port := belowPort - 1; 1024 < port; port -= 1 {
		packetConn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		packetConn.Close()
		return port
	}
	t.Fatalf("no free udp port below %d", belowPort)
	return 0
}

// The caller address an activation against this fixture arrives from on one
// loopback family, which is the address the handler probes back.
func (self *testExtenderFixture) clientAddress(loopbackIp string) string {
	return net.JoinHostPort(loopbackIp, "54321")
}

// Installs an `extender.yml` naming this fixture's api and a fresh root key,
// and points the forward probe at the fixture roots. The restore is
// registered as test cleanup, so the process configuration never leaks to the
// next test.
func installTestExtenderConfig(t testing.TB, api *testExtenderApi) ed25519.PrivateKey {
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

	installTestExtenderConfigYaml(t, strings.Join([]string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPublicKey)),
		fmt.Sprintf("network_host: %s", testExtenderNetworkHost),
		"network_hosts:",
		fmt.Sprintf("  - %s", testExtenderNetworkHost),
		fmt.Sprintf("  - %s", testExtenderApiHost),
		fmt.Sprintf("api_url: https://%s", testExtenderApiHost),
		"dns:",
		"  enabled: false",
	}, "\n"))

	if api != nil {
		previousTlsConfig := extenderForwardProbeTlsConfig
		extenderForwardProbeTlsConfig = func() *tls.Config {
			return &tls.Config{RootCAs: api.rootCAs}
		}
		t.Cleanup(func() {
			extenderForwardProbeTlsConfig = previousTlsConfig
		})
	}

	return rootPrivateKey
}

// Pushes one `extender.yml` body through the vault resolver and drops the
// cached configuration so the next read sees it.
func installTestExtenderConfigYaml(t testing.TB, body string) {
	t.Helper()
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(body))
	Testing_ResetExtenderConfig()
	t.Cleanup(func() {
		pop()
		Testing_ResetExtenderConfig()
	})
}

// A self-signed certificate for one synthetic name, usable as its own root.
func testExtenderCertificate(host string) (*tls.Certificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{Organization: []string{"Extender Test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, privateKey.Public(), privateKey)
	if err != nil {
		return nil, err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  privateKey,
		Leaf:        certificate,
	}, nil
}

// The extender reports connection-stage failures through its error handler.
// A refused activation is expected to produce some of them, so they are only
// drained, never asserted on.
func (self *testExtenderFixture) drainErrors() []error {
	errs := []error{}
	for {
		select {
		case err := <-self.errors:
			errs = append(errs, err)
		default:
			return errs
		}
	}
}
