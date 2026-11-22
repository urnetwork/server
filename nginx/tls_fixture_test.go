package nginx

// Fixtures for the certificate, name and TLS config tests: a generated ECDSA
// P-256 authority and the leaves it issues, vaults written in t.TempDir(), a
// client session cache that reports each ticket it stores, and loopback
// listeners that serve one connection at a time through a config under test.
// Names are .example names, and sockets bind loopback only.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/quic-go"
)

// How long one exchange, or the wait for one session ticket, may take before
// a test fails. It bounds a hang only: no test waits it out to pass.
const testTlsExchangeTimeout = 30 * time.Second

// The version directory the fixture vault writes.
const testTlsVersion = "2026.10.16"

// A generated certificate authority.
type testTlsAuthority struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
}

// A new authority, valid for a year either side of now.
func newTestTlsAuthority(t *testing.T) *testTlsAuthority {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          newTestTlsSerialNumber(t),
		Subject:               pkix.Name{CommonName: "lbalt test authority"},
		NotBefore:             now.Add(-365 * 24 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testTlsAuthority{
		certificate: certificate,
		privateKey:  privateKey,
	}
}

// A random serial number, as an authority assigns one.
func newTestTlsSerialNumber(t *testing.T) *big.Int {
	t.Helper()
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
	return serialNumber
}

// A pool trusting only the authority.
func (self *testTlsAuthority) pool() *x509.CertPool {
	certPool := x509.NewCertPool()
	certPool.AddCert(self.certificate)
	return certPool
}

// A leaf and its key, in the vault's file formats.
type testTlsPair struct {
	leaf *x509.Certificate
	// the leaf, then the authority, as a full chain
	pemBytes []byte
	keyBytes []byte
}

// Issues a leaf for dnsNames, valid from notBefore until notAfter.
func (self *testTlsAuthority) issue(t *testing.T, dnsNames []string, notBefore time.Time, notAfter time.Time) *testTlsPair {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: newTestTlsSerialNumber(t),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, self.certificate, &privateKey.PublicKey, self.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDer, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: self.certificate.Raw})...)
	return &testTlsPair{
		leaf:     leaf,
		pemBytes: pemBytes,
		keyBytes: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDer}),
	}
}

// Issues a leaf for dnsNames valid from a day ago until validFor from now.
func (self *testTlsAuthority) issueValid(t *testing.T, validFor time.Duration, dnsNames ...string) *testTlsPair {
	t.Helper()
	now := time.Now()
	return self.issue(t, dnsNames, now.Add(-24*time.Hour), now.Add(validFor))
}

// Writes fileBytes at <home>/tls/<version>/<dirName>/<dirName><suffix>, the
// layout findLatestTls reads.
func writeTestTlsFile(t *testing.T, home string, version string, dirName string, suffix string, fileBytes []byte) {
	t.Helper()
	dirPath := filepath.Join(home, "tls", version, dirName)
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, dirName+suffix), fileBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Writes pair as <dirName>.pem and <dirName>.key.
func writeTestTlsPair(t *testing.T, home string, version string, dirName string, pair *testTlsPair) {
	t.Helper()
	writeTestTlsFile(t, home, version, dirName, ".pem", pair.pemBytes)
	writeTestTlsFile(t, home, version, dirName, ".key", pair.keyBytes)
}

// The shared route fixture's table and a vault holding a certificate for each
// of its served names, in four pairs under <vault>/<env>/tls/<testTlsVersion>:
//   - test-lb.service.example in its own directory: the default server, so a
//     handshake shows when it was chosen;
//   - star.service.example (*.service.example) for every other name under
//     service.example, by the star.<parent> fallback;
//   - star.other.example (*.other.example) for the names under
//     other.example;
//   - star.connect.service.example for *.connect.service.example.
//
// Each expires on a different day, star.other.example first.
type testTlsVault struct {
	vaultDir   string
	env        string
	routeTable *RouteTable
	authority  *testTlsAuthority
	// each pair by its directory name
	dirPairs map[string]*testTlsPair
}

// The fixture vault, written in a new temporary directory.
func newTestTlsVault(t *testing.T) *testTlsVault {
	t.Helper()
	routeTable, err := ParseRouteTable(routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	authority := newTestTlsAuthority(t)
	day := 24 * time.Hour
	vault := &testTlsVault{
		vaultDir:   t.TempDir(),
		env:        routeTable.Env,
		routeTable: routeTable,
		authority:  authority,
		dirPairs: map[string]*testTlsPair{
			"test-lb.service.example":      authority.issueValid(t, 30*day, "test-lb.service.example"),
			"star.service.example":         authority.issueValid(t, 60*day, "*.service.example"),
			"star.other.example":           authority.issueValid(t, 20*day, "*.other.example"),
			"star.connect.service.example": authority.issueValid(t, 40*day, "*.connect.service.example"),
		},
	}
	for dirName, pair := range vault.dirPairs {
		writeTestTlsPair(t, vault.envHome(), testTlsVersion, dirName, pair)
	}
	return vault
}

// <vault>/<env>, where the fixture writes its pairs.
func (self *testTlsVault) envHome() string {
	return filepath.Join(self.vaultDir, self.env)
}

// The store of the fixture vault, which must load.
func (self *testTlsVault) load(t *testing.T) *CertificateStore {
	t.Helper()
	certificateStore, err := LoadCertificateStore(self.vaultDir, self.env, self.routeTable)
	if err != nil {
		t.Fatal(err)
	}
	return certificateStore
}

// A new pair of base configs over the fixture, with the first lb name as the
// default server, as the front builds them, counting into metrics of their
// own.
func (self *testTlsVault) newTlsConfigs(t *testing.T, settings *TlsSettings) (tlsConfig *tls.Config, quicTlsConfig *tls.Config) {
	t.Helper()
	return self.newTlsConfigsWithMetrics(t, settings, newFrontMetrics(prometheus.NewRegistry()))
}

// As newTlsConfigs, counting into metrics.
func (self *testTlsVault) newTlsConfigsWithMetrics(t *testing.T, settings *TlsSettings, metrics *frontMetrics) (tlsConfig *tls.Config, quicTlsConfig *tls.Config) {
	t.Helper()
	return self.newTlsConfigsAt(t, settings, metrics, time.Now)
}

// As newTlsConfigsWithMetrics, the shared ticket keys' days read from now.
func (self *testTlsVault) newTlsConfigsAt(t *testing.T, settings *TlsSettings, metrics *frontMetrics, now func() time.Time) (tlsConfig *tls.Config, quicTlsConfig *tls.Config) {
	t.Helper()
	return newTlsConfigs(self.load(t), newNameMatcher(self.routeTable), self.routeTable.Lb.Names[0], self.routeTable.Listen, self.routeTable.Interfaces, settings, metrics, now)
}

// A client session cache that reports each session it stores, so a test
// waits for the ticket itself rather than for time to pass.
type testNotifyingSessionCache struct {
	tls.ClientSessionCache
	sessionKeys chan string
}

// An empty cache.
func newTestNotifyingSessionCache() *testNotifyingSessionCache {
	return &testNotifyingSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(0),
		sessionKeys:        make(chan string, 16),
	}
}

// Stores the session, then reports it. crypto/tls also calls Put with nil to
// evict, which reports nothing.
func (self *testNotifyingSessionCache) Put(sessionKey string, clientSessionState *tls.ClientSessionState) {
	self.ClientSessionCache.Put(sessionKey, clientSessionState)
	if clientSessionState == nil {
		return
	}
	select {
	case self.sessionKeys <- sessionKey:
	default:
	}
}

// Waits for the next session the cache stores.
func (self *testNotifyingSessionCache) awaitSession(t *testing.T) {
	t.Helper()
	select {
	case <-self.sessionKeys:
	case <-time.After(testTlsExchangeTimeout):
		t.Fatal("the client stored no session ticket")
	}
}

// A loopback TCP listener serving TLS through a config under test, one
// connection at a time.
type testTlsListener struct {
	tcpListener *net.TCPListener
	tlsListener net.Listener
}

// A listener on a new loopback port, closed when the test ends.
func listenTestTls(t *testing.T, tlsConfig *tls.Config) *testTlsListener {
	t.Helper()
	tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tcpListener.Close()
	})
	return &testTlsListener{
		tcpListener: tcpListener,
		// as connect serves TCP 443: the base config, never a clone of it
		tlsListener: tls.NewListener(tcpListener, tlsConfig),
	}
}

// Dials the listener with clientConfig and serves the one connection: the
// handshake, then a byte, which the client reads after any session ticket
// the server sent. The client's view of the connection, or its handshake
// error.
func (self *testTlsListener) exchange(t *testing.T, clientConfig *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		self.tcpListener.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
		conn, err := self.tlsListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
		// the write runs the handshake first
		if _, err := conn.Write([]byte{1}); err != nil {
			return
		}
		// until the client closes, so no reset races its read
		io.Copy(io.Discard, conn)
	}()
	defer func() {
		<-serverDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTlsExchangeTimeout)
	defer cancel()
	dialer := &tls.Dialer{
		Config: clientConfig,
	}
	conn, err := dialer.DialContext(ctx, "tcp", self.tcpListener.Addr().String())
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		return tls.ConnectionState{}, err
	}
	return conn.(*tls.Conn).ConnectionState(), nil
}

// A loopback UDP socket serving QUIC through a config under test, as one of
// connect's public sockets does.
type testQuicListener struct {
	udpConn  *net.UDPConn
	listener *quic.Listener
}

// A socket on a new loopback port, closed when the test ends.
func listenTestQuic(t *testing.T, quicTlsConfig *tls.Config) *testQuicListener {
	t.Helper()
	// quic-go warns once per process when it can't raise the socket buffers
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{
		Conn: udpConn,
	}
	listener, err := transport.Listen(quicTlsConfig, &quic.Config{})
	if err != nil {
		udpConn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
		transport.Close()
		udpConn.Close()
	})
	return &testQuicListener{
		udpConn:  udpConn,
		listener: listener,
	}
}

// Dials the socket with clientConfig and holds the connection until the
// client has stored a session ticket, when sessionCache, the client's cache,
// is set. The client's view of the connection, or its handshake error.
func (self *testQuicListener) exchange(t *testing.T, clientConfig *tls.Config, sessionCache *testNotifyingSessionCache) (quic.ConnectionState, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTlsExchangeTimeout)
	defer cancel()
	acceptedConns := make(chan *quic.Conn, 1)
	go func() {
		defer close(acceptedConns)
		// a failed handshake never reaches Accept, which ends with ctx
		conn, err := self.listener.Accept(ctx)
		if err == nil {
			acceptedConns <- conn
		}
	}()
	closeAccepted := func() {
		cancel()
		for conn := range acceptedConns {
			conn.CloseWithError(0, "")
		}
	}

	conn, err := quic.DialAddr(ctx, self.udpConn.LocalAddr().String(), clientConfig, &quic.Config{})
	if err != nil {
		closeAccepted()
		return quic.ConnectionState{}, err
	}
	if sessionCache != nil {
		// the server sends its ticket once the handshake is confirmed
		sessionCache.awaitSession(t)
	}
	connectionState := conn.ConnectionState()
	conn.CloseWithError(0, "")
	closeAccepted()
	return connectionState, nil
}
