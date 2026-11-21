//go:build sidecar_e2e && sidecar_pending

// The hosts of the new server services as one test process runs them (e2e-hosts-in-process; the owner's end-to-end
// tests of 2026-11-21): each block through its own exported Run, on ports the OS picks, with throwaway credentials in
// its deps, none read from vault, over the private stores server.DefaultTestEnv gives each test. What a block's main
// reads from vault, the harness makes here: the svc- key set the blocks verify service tokens under, evs's receipt
// keys, sockets' ticket key file, smtpedge's X-UR-Ref keys, the dns API's bearer digests and the edge's bearer and
// ZSK, mxedge's platform certificate and mcp's seal secret.
//
// The process-wide state the blocks share is set here once, for every block: sidecar.yml (server.Testing_PushSidecarConfig),
// and mcp's seal key and issuer (mcp.Testing_SetSealSecret, oauth.Testing_SetIssuer). Everything else a block reads
// it is given in its own deps. A world of
// clients and the scenarios over it are later tracks' (e2e-world-clients): this file gathers what they build on.
//
// The blocks' engines take real time and the stores the env gives: Redis for every block's soft state and key events,
// Postgres for the blocks that hold a pool. The file clusters, quotas, write budgets and platform clients are
// sidecartest's fakes, each block's own, until the world brings the localstack MinIO and the in-process api.

package sidecare2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"
	"github.com/urnetwork/sidecar/socketsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/gkm"
	"github.com/urnetwork/server/mcp"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/smtpedgehost"
	"github.com/urnetwork/server/socketshost"
	"github.com/urnetwork/server/tlsedgehost"
)

// The harness's tunables (QD-702): each bound on a wait, and how the blocks run.
type e2eHostsSettings struct {
	// How long a block's /status may take to answer as a test wants it, ready or merely up: a bound on a hang, never
	// a pause the outcome rests on.
	StatusTimeout time.Duration
	// How often /status is asked while a block comes up. /status is the block's own latch, with no hook to wait on.
	StatusPollInterval time.Duration
	// How long a block's Run may take to return once its context ends: its drain and Close.
	StopTimeout time.Duration
	// How long the goroutines the blocks started may take to end once every block has returned, and how often they
	// are counted meanwhile. An exiting goroutine has no hook to wait on.
	GoroutineSettleTimeout time.Duration
	GoroutinePollInterval  time.Duration
	// sidecarhost.Run's for every block: readiness polled often, and no keepalive grace at the drain, since no lb
	// holds a test's connections.
	Run *sidecarhost.RunSettings
	// The issuer the throwaway svc- key set verifies under.
	ServiceTokenIssuer string
	// The root names the blocks serve under, each a documentation name.
	TlsEdgeRoot    string
	DnsRoot        string
	MxRouteRoot    string
	MxReportHost   string
	MxPlatformRoot string
	MxBannerHost   string
	SmtpRoot       string
	SmtpReportHost string
	SmtpHeloDomain string
	SmtpHeloName   string
	SmtpAddress    netip.Addr
	// How long the platform certificate is valid on either side of the test's start.
	CertificateValidity time.Duration
	// dns.yml's environment values, which have no default: the edge's signing rate, its TCP connections a source
	// may hold, and its response rate limit.
	DnsSignaturesPerSecond     int
	DnsTcpConnectionsPerSource int
	DnsRrlResponsesPerSecond   int
}

// The defaults.
func defaultE2eHostsSettings() *e2eHostsSettings {
	run := sidecarhost.DefaultRunSettings()
	run.ReadyPollInterval = 50 * time.Millisecond
	run.HttpServer.KeepaliveDrainTimeout = 0
	run.HttpServer.ShutdownTimeout = 30 * time.Second
	return &e2eHostsSettings{
		StatusTimeout:              120 * time.Second,
		StatusPollInterval:         50 * time.Millisecond,
		StopTimeout:                90 * time.Second,
		GoroutineSettleTimeout:     30 * time.Second,
		GoroutinePollInterval:      20 * time.Millisecond,
		Run:                        run,
		ServiceTokenIssuer:         "https://auth.e2e.example",
		TlsEdgeRoot:                "edge.e2e.example",
		DnsRoot:                    "zone.e2e.example.",
		MxRouteRoot:                "root.e2e.example",
		MxReportHost:               "rpt.report.e2e.example",
		MxPlatformRoot:             "platform.e2e.example",
		MxBannerHost:               "mx.report.e2e.example",
		SmtpRoot:                   "root.e2e.example",
		SmtpReportHost:             "rpt.report.e2e.example",
		SmtpHeloDomain:             "out.report.e2e.example",
		SmtpHeloName:               "o1.out.report.e2e.example",
		SmtpAddress:                netip.MustParseAddr("192.0.2.10"),
		CertificateValidity:        24 * time.Hour,
		DnsSignaturesPerSecond:     1000,
		DnsTcpConnectionsPerSource: 2,
		DnsRrlResponsesPerSecond:   1000,
	}
}

// sidecar.yml as every block in the process reads it: each service's gate open to every network, evs's with the
// env's block count, and urnetwork_edge the platform clients' network.
func e2eSidecarYaml(urnetworkEdge connect.Id) string {
	return strings.Join([]string{
		"sidecar:",
		"  evs: {enabled: true, percent: 100, blocks: 1}",
		"  gkm: {enabled: true, percent: 100}",
		"  sockets: {enabled: true, percent: 100}",
		"  tlsedge: {enabled: true, percent: 100}",
		"  mxedge: {enabled: true, percent: 100}",
		"  smtpedge: {enabled: true, percent: 100}",
		"  dns: {enabled: true, percent: 100}",
		"  urnetwork_edge: " + urnetworkEdge.String(),
		"",
	}, "\n")
}

// The throwaway credentials every block's deps take in place of its vault's, made fresh for each test.
type e2eCredentials struct {
	// The svc- key set: its signing key, and the verifier each block authenticates callers and reads rev/ under.
	serviceKey    *ecdsa.PrivateKey
	serviceKid    string
	serviceTokens *oauth.ServiceTokenVerifier
	// evs's upload receipt keys (evs-receipt-keys.json).
	receiptKeys *evsservice.ReceiptKeys
	// sockets' ticket key file (socket-ticket-keys.yml).
	ticketKeyFile []byte
	// smtpedge's X-UR-Ref keys (smtpedge-x-ur-ref.yml).
	xUrRefKeys []*smtpedgeservice.SmtpXUrRefKey
	// The dns API's bearer digests (internal-verify.yml), over the edge's bearer (internal-dnsedge.yml).
	dnsBearer string
	dnsAuth   dnsservice.DnsInternalAuth
	// The dns root's KSK, the edge's ZSK (dns-zsk.private), and the window the KSK signed over both, which the API
	// releases.
	dnsKskRecord *dns.DNSKEY
	dnsZsk       *ecdsa.PrivateKey
	dnsWindow    *dnsservice.DnsKeyWindow
	// mxedge-platform's certificate and key (platform-mail.pem, platform-mail.key).
	platformCertificate *tls.Certificate
	// mcp's seal secret, proxy.yml's first non-blank secret in production.
	sealSecret string
}

// Fresh credentials for settings' names, valid around now.
func newE2eCredentials(t testing.TB, settings *e2eHostsSettings, now time.Time) *e2eCredentials {
	t.Helper()
	random := func(byteCount int) []byte {
		value := make([]byte, byteCount)
		if _, err := rand.Read(value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	self := &e2eCredentials{}

	serviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&serviceKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	self.serviceKey = serviceKey
	self.serviceKid = oauth.SignerKeySetService.KidPrefix() + thumbprint
	self.serviceTokens, err = oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: settings.ServiceTokenIssuer,
		Keys:   []*oauth.Jwk{oauth.NewSignerJwk(self.serviceKid, "ES256", &serviceKey.PublicKey)},
	})
	if err != nil {
		t.Fatal(err)
	}

	self.receiptKeys, err = evsservice.NewReceiptKeys([]*evsservice.ReceiptKey{
		{KeyId: 1, Key: random(32), IssueFrom: now.Add(-time.Hour)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	self.ticketKeyFile, err = socketsservice.SocketTicketKeyFileAdd(nil, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	self.xUrRefKeys = []*smtpedgeservice.SmtpXUrRefKey{{KeyId: 1, Key: random(32)}}

	self.dnsBearer = base64.RawURLEncoding.EncodeToString(random(32))
	digest := sha256.Sum256([]byte(self.dnsBearer))
	self.dnsAuth, err = dnsservice.NewDnsInternalVerifier([]byte("dnsedge-1: {class: dnsedge, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dnsKey := func(flags uint16) (*ecdsa.PrivateKey, *dns.DNSKEY) {
		record := &dns.DNSKEY{
			Hdr:       dns.RR_Header{Name: settings.DnsRoot, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600},
			Flags:     flags,
			Protocol:  3,
			Algorithm: dns.ECDSAP256SHA256,
		}
		private, err := record.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		return private.(*ecdsa.PrivateKey), record
	}
	ksk, kskRecord := dnsKey(dns.ZONE | dns.SEP)
	zsk, zskRecord := dnsKey(dns.ZONE)
	notBefore := now.Add(-24 * time.Hour)
	notAfter := now.Add(13 * 24 * time.Hour)
	rrsig := &dns.RRSIG{
		Hdr:        dns.RR_Header{Name: settings.DnsRoot, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 3600},
		Algorithm:  dns.ECDSAP256SHA256,
		KeyTag:     kskRecord.KeyTag(),
		SignerName: settings.DnsRoot,
		Inception:  uint32(notBefore.Unix()),
		Expiration: uint32(notAfter.Unix()),
	}
	if err := rrsig.Sign(ksk, []dns.RR{kskRecord, zskRecord}); err != nil {
		t.Fatal(err)
	}
	rdata := func(record dns.RR) string {
		return strings.TrimSpace(strings.TrimPrefix(record.String(), record.Header().String()))
	}
	self.dnsKskRecord = kskRecord
	self.dnsZsk = zsk
	self.dnsWindow = &dnsservice.DnsKeyWindow{
		Root:      settings.DnsRoot,
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Dnskeys:   []string{rdata(kskRecord), rdata(zskRecord)},
		Rrsigs:    []string{rdata(rrsig)},
	}

	certificateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: settings.MxReportHost},
		DNSNames:     []string{settings.MxReportHost},
		NotBefore:    now.Add(-settings.CertificateValidity),
		NotAfter:     now.Add(settings.CertificateValidity),
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, certificateKey.Public(), certificateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificateBytes)
	if err != nil {
		t.Fatal(err)
	}
	self.platformCertificate = &tls.Certificate{Certificate: [][]byte{certificateBytes}, PrivateKey: certificateKey, Leaf: leaf}

	self.sealSecret = hex.EncodeToString(random(32))
	return self
}

// What every block of one test shares: the settings, the credentials, the network urnetwork_edge names, and the
// process-wide state set once for all of them.
type e2eProcess struct {
	settings      *e2eHostsSettings
	credentials   *e2eCredentials
	urnetworkEdge connect.Id
	// GET /status with no connection kept, so nothing of the client outlives a test.
	client *http.Client
}

// The process's state for one test, put back when it ends: sidecar.yml for every block, and mcp's seal key and the
// issuer its resource metadata names.
func newE2eProcess(t testing.TB) *e2eProcess {
	t.Helper()
	settings := defaultE2eHostsSettings()
	self := &e2eProcess{
		settings:      settings,
		credentials:   newE2eCredentials(t, settings, time.Now()),
		urnetworkEdge: connect.NewId(),
		client:        &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
	}
	t.Cleanup(server.Testing_PushSidecarConfig(e2eSidecarYaml(self.urnetworkEdge)))
	t.Cleanup(mcp.Testing_SetSealSecret(self.credentials.sealSecret))
	t.Cleanup(oauth.Testing_SetIssuer(settings.ServiceTokenIssuer))
	return self
}

// A loopback listener on a port the OS picks.
func (self *e2eProcess) listen(t testing.TB) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// A loopback UDP socket on a port the OS picks.
func (self *e2eProcess) listenPacket(t testing.TB) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// One block running in the test process.
type e2eHost struct {
	name string
	// GET /status's base URL, on the port the OS picked.
	base   string
	cancel context.CancelFunc
	// closes once Run has returned, err then holding what it returned
	done chan struct{}
	err  error
}

// Runs a block's Run on its own context until stop, its status at statusListener's port.
func (self *e2eProcess) start(t testing.TB, name string, statusListener net.Listener, run func(ctx context.Context) error) *e2eHost {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	host := &e2eHost{
		name:   name,
		base:   "http://" + statusListener.Addr().String(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(host.done)
		host.err = run(ctx)
	}()
	// a block a failed test left running still ends with it
	t.Cleanup(func() {
		cancel()
		select {
		case <-host.done:
		case <-time.After(self.settings.StopTimeout):
		}
	})
	return host
}

// GET /status's status, and false when the block does not answer one.
func (self *e2eProcess) status(host *e2eHost) (string, bool) {
	response, err := self.client.Get(host.base + "/status")
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	result := &router.WarpStatusResult{}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return "", false
	}
	return result.Status, true
}

// Waits until host's /status answers a status want takes, and returns it; fails the test with the last status
// past StatusTimeout.
func (self *e2eProcess) awaitStatus(t testing.TB, host *e2eHost, what string, want func(status string) bool) string {
	t.Helper()
	deadline := time.After(self.settings.StatusTimeout)
	last := "no answer"
	for {
		if status, ok := self.status(host); ok {
			if want(status) {
				t.Logf("%s's /status: %q", host.name, status)
				return status
			}
			last = status
		}
		select {
		case <-host.done:
			t.Fatalf("%s ended before its /status was %s: %v", host.name, what, host.err)
		case <-deadline:
			t.Fatalf("%s's /status was never %s; last %q", host.name, what, last)
		case <-time.After(self.settings.StatusPollInterval):
		}
	}
}

// /status answers at all, ready or not: the block is up and its latch serves from the first moment (§19.4).
func e2eStatusAnswers(status string) bool {
	return status != ""
}

// /status answers ready.
func e2eStatusReady(status string) bool {
	return status == "ok"
}

// A service token for audience signed by key under kid, as api's issuer mints one (oauth.MintServiceToken): the
// harness's svc- key, which the blocks were given, or another a test makes to be refused. The signer keys are the
// process's for the mint alone.
func (self *e2eProcess) serviceToken(t testing.TB, key *ecdsa.PrivateKey, kid string, audience string) string {
	t.Helper()
	pop := oauth.Testing_SetSignerKeys(&oauth.SignerKey{Kid: kid, Alg: "ES256", Set: oauth.SignerKeySetService, PrivateKey: key})
	defer pop()
	sidecarId := server.NewId()
	token, _, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{
		NetworkId: server.NewId(),
		ClientId:  server.NewId(),
		SidecarId: &sidecarId,
		Audience:  audience,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A svc- key the blocks were not given, with its kid.
func (self *e2eProcess) foreignServiceKey(t testing.TB) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, oauth.SignerKeySetService.KidPrefix() + thumbprint
}

// The status code a block's route answers a request with token as its bearer.
func (self *e2eProcess) routeCode(t testing.TB, host *e2eHost, method string, path string, token string, body string) int {
	t.Helper()
	request, err := http.NewRequest(method, host.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := self.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// Ends host's context, the block's SIGTERM, and waits for its Run to drain, close and return nil.
func (self *e2eProcess) stop(t testing.TB, host *e2eHost) {
	t.Helper()
	host.cancel()
	select {
	case <-host.done:
		if host.err != nil {
			t.Fatalf("%s's Run returned %v", host.name, host.err)
		}
	case <-time.After(self.settings.StopTimeout):
		t.Fatalf("%s's Run did not return within %v of its end", host.name, self.settings.StopTimeout)
	}
}

// The clusters a block holds, faked: sidecartest's blobs, every sentinel read.
type e2eClusters struct {
	*sidecartest.FakeBlobs
}

func newE2eClusters() *e2eClusters {
	return &e2eClusters{FakeBlobs: sidecartest.NewFakeBlobs(sidecar.RealClock{}, nil, nil, nil)}
}

func (self *e2eClusters) Ready() bool {
	return true
}

func (self *e2eClusters) Close() {}

// An authorization log source that holds no network: every load fails, so no network is served, as api's internal
// route answers for a network it does not know.
type e2eNoNetworkSource struct{}

func (e2eNoNetworkSource) Genesis(ctx context.Context, networkId sidecar.Id) ([]byte, error) {
	return nil, sidecar.ErrBlobNotFound
}

func (e2eNoNetworkSource) Entries(ctx context.Context, networkId sidecar.Id, after *bridge.AuthPin) ([][]byte, error) {
	return nil, sidecar.ErrBlobNotFound
}

func (e2eNoNetworkSource) Rows(ctx context.Context, networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
	return nil, sidecar.ErrBlobNotFound
}

// A block's K platform clients, connected from the start, as sidecar.EdgeClientPool's once the source answers: ids
// alone, since no client of this harness reaches urnetwork-edge.
type e2eEdgeClients struct {
	clientIds []sidecar.Id
}

func newE2eEdgeClients(clientCount int) *e2eEdgeClients {
	self := &e2eEdgeClients{}
	for range clientCount {
		self.clientIds = append(self.clientIds, sidecar.NewId())
	}
	return self
}

func (self *e2eEdgeClients) Clients() []*sidecar.SocketEdgeClient { return nil }
func (self *e2eEdgeClients) ClientIds() []sidecar.Id              { return slices.Clone(self.clientIds) }
func (self *e2eEdgeClients) Ready() bool                          { return true }
func (self *e2eEdgeClients) Close()                               {}

// Every dial answers unavailable: no listener answers in this harness.
var errE2eNoListener = &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}

// evs on fake clusters, the env's Redis, and its receipt keys and svc- verifier.
func (self *e2eProcess) startEvs(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	settings := evshost.DefaultHostSettings()
	settings.Run = self.settings.Run
	deps := &evshost.HostDeps{
		ReceiptKeys:    self.credentials.receiptKeys,
		Clusters:       newE2eClusters(),
		ClockCheck:     sidecartest.NewFakeClockCheck(nil),
		Quota:          sidecartest.NewFakeQuota(sidecar.RealClock{}, nil, nil),
		Budget:         sidecartest.NewFakeWriteBudget(nil),
		ServiceTokens:  self.credentials.serviceTokens,
		StatusListener: statusListener,
	}
	return self.start(t, "evs", statusListener, func(ctx context.Context) error {
		return evshost.RunHost(ctx, settings, deps, 0)
	})
}

// gkm on fake clusters, the env's Redis, and its svc- verifier.
func (self *e2eProcess) startGkm(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	settings := gkm.DefaultHostSettings()
	settings.Run = self.settings.Run
	deps := &gkm.HostDeps{
		Clusters:      newE2eClusters(),
		Budget:        sidecartest.NewFakeWriteBudget(nil),
		Quota:         sidecartest.NewFakeQuota(sidecar.RealClock{}, nil, nil),
		ServiceTokens: self.credentials.serviceTokens,
	}
	return self.start(t, "gkm", statusListener, func(ctx context.Context) error {
		return gkm.Run(ctx, gkm.RunOptions{StatusListener: statusListener, Settings: settings, Deps: deps})
	})
}

// sockets on the env's Redis, its store model's, with its ticket keys, its svc- verifier and a rev/ reader of fake
// clusters, and api's internal sources standing for a world that holds no network yet.
func (self *e2eProcess) startSockets(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	settings := socketshost.DefaultHostSettings()
	settings.Run = self.settings.Run
	ticketKeyFile := self.credentials.ticketKeyFile
	deps := &socketshost.HostDeps{
		Quota:         sidecartest.NewFakeQuota(sidecar.RealClock{}, nil, nil),
		Blobs:         newE2eClusters(),
		Source:        e2eNoNetworkSource{},
		KeyFile:       func() ([]byte, error) { return ticketKeyFile, nil },
		ServiceTokens: self.credentials.serviceTokens,
		TlsListenFacts: socketsservice.TlsListenFactsFunc(func(ctx context.Context, networkId connect.Id) (*socketsservice.TlsListenFacts, error) {
			return nil, errors.New("no network in this world")
		}),
	}
	return self.start(t, "sockets", statusListener, func(ctx context.Context) error {
		return socketshost.Run(ctx, &socketshost.RunOptions{StatusListener: statusListener, Settings: settings, Deps: deps})
	})
}

// tlsedge on the env's Redis, a fake label map, K fake clients and its four ports, each on a port the OS picks.
func (self *e2eProcess) startTlsedge(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	tlsEdge := tlsedgeservice.DefaultTlsEdgeSettings()
	tlsEdge.Root = self.settings.TlsEdgeRoot
	tlsEdge.Blocks = 1
	settings := tlsedgehost.DefaultHostSettings(tlsEdge)
	settings.Run = self.settings.Run
	clients := newE2eEdgeClients(tlsEdge.Clients)
	deps := &tlsedgehost.HostDeps{
		Quota: sidecartest.NewFakeQuota(sidecar.RealClock{}, nil, nil),
		Store: sidecartest.NewFakeTlsEdgeStore(nil),
		NewClients: func(ctx context.Context) (tlsedgehost.HostClients, error) {
			return clients, nil
		},
		NewDial: func(clients tlsedgehost.HostClients) (tlsedgehost.HostSocketDial, error) {
			return func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
				return nil, errE2eNoListener
			}, nil
		},
	}
	listeners := map[int]net.Listener{}
	for _, servicePort := range tlsedgehost.HostServicePorts {
		listeners[servicePort] = self.listen(t)
	}
	return self.start(t, "tlsedge", statusListener, func(ctx context.Context) error {
		return tlsedgehost.Run(ctx, &tlsedgehost.RunOptions{StatusListener: statusListener, Listeners: listeners, Settings: settings, Deps: deps})
	})
}

// smtpedge on the env's Postgres and Redis, its X-UR-Ref keys, sidecartest's MX world for its resolver and dialer,
// a fake store, gate and verifier, and a fake client, which reaches no urnetwork-edge: so ur/smtpedge is never served
// and the block answers /status not ready, as one whose client cannot connect does.
func (self *e2eProcess) startSmtpedge(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(fmt.Sprintf(
		"root: %s\nreport_host: %s\nhelo_domain: %s\nsending_addresses:\n  - {address: %s, helo: %s}\n",
		self.settings.SmtpRoot, self.settings.SmtpReportHost, self.settings.SmtpHeloDomain, self.settings.SmtpAddress, self.settings.SmtpHeloName,
	)), 0)
	if err != nil {
		t.Fatal(err)
	}
	config.Settings.XUrRefKeys = self.credentials.xUrRefKeys
	worldSettings := sidecartest.DefaultMxWorldSettings()
	world, err := sidecartest.NewMxWorld(worldSettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.AddMx(self.settings.SmtpHeloName, self.settings.SmtpAddress, nil); err != nil {
		t.Fatal(err)
	}
	world.SetPtr(self.settings.SmtpAddress, self.settings.SmtpHeloName)
	anchors, err := sidecar.NewSmtpDnssecAnchors(world.TrustAnchorDs())
	if err != nil {
		t.Fatal(err)
	}
	resolver := sidecar.NewSmtpMxDnsResolver(world, anchors, nil, sidecar.RealClock{})
	settings := smtpedgehost.DefaultHostSettings(config)
	settings.Run = self.settings.Run
	clients := newE2eEdgeClients(settings.Clients.ClientCount)
	deps := &smtpedgehost.HostDeps{
		Quota:    sidecartest.NewFakeQuota(sidecar.RealClock{}, nil, nil),
		Abuse:    sidecartest.NewFakeAbuseGate(),
		Store:    sidecartest.NewFakeSmtpEdgeStore(sidecar.RealClock{}, nil),
		Gate:     sidecartest.NewFakeSmtpGate(),
		Resolver: resolver,
		Ptr:      resolver,
		Dialer:   world,
		Verifier: sidecartest.NewFakeSmtpProofVerifier(nil),
		NewClients: func(ctx context.Context) (smtpedgehost.HostClients, error) {
			return clients, nil
		},
		Registrations: &sidecar.SocketPlatformRegistrationsArgs{
			SocketsUrl: "https://sockets.e2e.example",
			Bearer: func(context.Context, sidecar.Id) (string, error) {
				return "", errors.New("no sockets service in this harness")
			},
		},
	}
	return self.start(t, "smtpedge", statusListener, func(ctx context.Context) error {
		return smtpedgehost.Run(ctx, &smtpedgehost.RunOptions{StatusListener: statusListener, Settings: settings, Deps: deps})
	})
}

// mxedge.yml for the harness's names.
func (self *e2eProcess) mxEdgeSettings() *mxedgehost.HostSettings {
	mxEdge := mxedgeservice.DefaultMxEdgeSettings()
	mxEdge.BannerHostName = self.settings.MxBannerHost
	mxEdge.DeviationUrl = "https://deviation.e2e.example/rfc3207"
	mxEdge.PlatformReportHostName = self.settings.MxReportHost
	mxEdge.PlatformRootDomainName = self.settings.MxPlatformRoot
	mxEdge.PlatformAbuseUrl = "https://abuse.e2e.example/form"
	mxEdge.RouteRootDomainName = self.settings.MxRouteRoot
	settings := mxedgehost.DefaultHostSettings()
	settings.MxEdge = mxEdge
	settings.Run = self.settings.Run
	return settings
}

// mxedge's router on the env's Postgres and Redis, a fake store, K fake clients and TCP 25 on a port the OS picks.
func (self *e2eProcess) startMxedgeRouter(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	settings := self.mxEdgeSettings()
	clients := newE2eEdgeClients(settings.MxEdge.Clients)
	deps := &mxedgehost.RouterDeps{
		Quota:       sidecartest.NewFakeQuota(sidecar.RealClock{}, nil, nil),
		Store:       sidecartest.NewFakeMxEdgeStore(nil),
		StoreGroups: mxedgehost.RouterStoreGroups(),
		NewClients: func(ctx context.Context) (mxedgehost.RouterClients, error) {
			return clients, nil
		},
		NewDial: func(clients mxedgehost.RouterClients) (mxedgehost.RouterSocketDial, error) {
			return func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (*sidecar.SocketEdgeStream, error) {
				return nil, errE2eNoListener
			}, nil
		},
		LookupPtr: func(ctx context.Context, sender netip.Addr) ([]string, error) {
			return nil, errors.New("no PTR in this harness")
		},
		LookupHost: func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, errors.New("no name in this harness")
		},
		StatusListener: statusListener,
		SmtpListener:   self.listen(t),
	}
	return self.start(t, "mxedge", statusListener, func(ctx context.Context) error {
		return mxedgehost.RunHost(ctx, mxedgehost.HostRoleRouter, settings, nil, deps, &mxedgehost.HostPorts{Smtp: 25})
	})
}

// mxedge's platform listener on the env's Postgres and Redis with its certificate and no report intake, TCP 25 on a
// port the OS picks.
func (self *e2eProcess) startMxedgePlatform(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	settings := self.mxEdgeSettings()
	deps := &mxedgehost.HostDeps{
		Certificate:    self.credentials.platformCertificate,
		StatusListener: statusListener,
		SmtpListener:   self.listen(t),
	}
	return self.start(t, "mxedgeplatform", statusListener, func(ctx context.Context) error {
		return mxedgehost.RunHost(ctx, mxedgehost.HostRolePlatform, settings, deps, nil, &mxedgehost.HostPorts{Smtp: 25})
	})
}

// dns.yml for the harness's root, anchored at the credentials' KSK.
func (self *e2eProcess) dnsSettings(t testing.TB) *dnsservice.DnsSettings {
	t.Helper()
	rdata := strings.TrimSpace(strings.TrimPrefix(self.credentials.dnsKskRecord.String(), self.credentials.dnsKskRecord.Header().String()))
	settings := dnsservice.DefaultDnsSettings()
	settings.Roots = []string{self.settings.DnsRoot}
	settings.TrustAnchors = map[string][]string{self.settings.DnsRoot: {rdata}}
	settings.SignaturesPerSecond = self.settings.DnsSignaturesPerSecond
	settings.Tcp.ConnectionsPerSource = self.settings.DnsTcpConnectionsPerSource
	settings.Rrl.ResponsesPerSecond = self.settings.DnsRrlResponsesPerSecond
	settings.Edge.UdpReaderCount = 1
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	return settings
}

// The dns API on the env's Postgres and Redis, releasing the credentials' key window, with the edge's bearer
// digests, no lb inventory, and api's projection standing for a world with no network.
func (self *e2eProcess) startDnsApi(t testing.TB) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	window := self.credentials.dnsWindow
	settings := &dnshost.HostSettings{
		Dns:             self.dnsSettings(t),
		SecretDirectory: t.TempDir(),
		Run:             self.settings.Run,
	}
	deps := &dnshost.HostDeps{
		KeyWindow: func(root string, now time.Time) *dnsservice.DnsKeyWindow {
			if root != window.Root {
				return nil
			}
			return window
		},
		Auth:           self.credentials.dnsAuth,
		Interfaces:     []dnsservice.DnsLbInterface{},
		Projection:     e2eNoNetworkSource{},
		ServiceTokens:  self.credentials.serviceTokens,
		StatusListener: statusListener,
	}
	return self.start(t, "dns", statusListener, func(ctx context.Context) error {
		return dnshost.RunHost(ctx, dnshost.HostRoleApi, settings, deps, nil)
	})
}

// A dns edge feeding from api, the in-process dns API's base URL, with its ZSK and bearer, its probes refused, and
// DNS on TCP and UDP ports the OS picks.
func (self *e2eProcess) startDnsEdge(t testing.TB, api *e2eHost) *e2eHost {
	t.Helper()
	statusListener := self.listen(t)
	settings := &dnshost.HostSettings{
		Dns:               self.dnsSettings(t),
		SecretDirectory:   t.TempDir(),
		ApiUrl:            api.base,
		SnapshotDirectory: t.TempDir(),
		Run:               self.settings.Run,
	}
	deps := &dnshost.HostDeps{
		Zsk:            self.credentials.dnsZsk,
		Bearer:         self.credentials.dnsBearer,
		ProbeDialer:    e2eNoLbDialer{},
		StatusListener: statusListener,
		DnsListener:    self.listen(t),
		DnsPacketConn:  self.listenPacket(t),
	}
	return self.start(t, "dnsedge", statusListener, func(ctx context.Context) error {
		return dnshost.RunHost(ctx, dnshost.HostRoleEdge, settings, deps, nil)
	})
}

// Refuses every probe: no lb interface answers in this harness.
type e2eNoLbDialer struct{}

func (e2eNoLbDialer) DialProbe(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("no lb in this harness")
}

// mcp on the env's Postgres and Redis with the harness's seal key, its readiness the migration-aware check alone:
// the tools' warmup loads process-wide state, the IP databases and the location searches, that would outlive it.
func (self *e2eProcess) startMcp(t testing.TB) *e2eHost {
	t.Helper()
	listener := self.listen(t)
	httpServer := self.settings.Run.HttpServer
	return self.start(t, "mcp", listener, func(ctx context.Context) error {
		return mcp.Run(ctx, mcp.RunOptions{Listener: listener, HttpServer: &httpServer, Startup: router.StartupReadiness})
	})
}

// The goroutines running now, each by its id, with its stack.
func e2eGoroutines() map[uint64]string {
	buffer := make([]byte, 1024*1024)
	for {
		n := runtime.Stack(buffer, true)
		if n < len(buffer) {
			buffer = buffer[:n]
			break
		}
		buffer = make([]byte, 2*len(buffer))
	}
	goroutines := map[uint64]string{}
	for _, stack := range strings.Split(string(buffer), "\n\n") {
		match := e2eGoroutineHeader.FindStringSubmatch(stack)
		if match == nil {
			continue
		}
		id, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			continue
		}
		goroutines[id] = stack
	}
	return goroutines
}

// runtime.Stack's first line for a goroutine.
var e2eGoroutineHeader = regexp.MustCompile(`^goroutine (\d+) \[`)

// The process's own pools, which every block shares and which outlive any one of them by design, made before a test
// counts goroutines, so the count holds only what the blocks start: server's Postgres and Redis pools.
func (self *e2eProcess) openProcessPools(t testing.TB) {
	t.Helper()
	ctx := t.Context()
	server.Db(ctx, func(conn server.PgConn) {
		_, err := conn.Exec(ctx, "SELECT 1")
		server.Raise(err)
	})
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.Ping(ctx).Err())
	})
}

// Waits until every goroutine that was not running in before has ended, and fails the test with the stacks of those
// that remain past GoroutineSettleTimeout.
func (self *e2eProcess) awaitNoGoroutinesBeyond(t testing.TB, before map[uint64]string) {
	t.Helper()
	deadline := time.After(self.settings.GoroutineSettleTimeout)
	for {
		left := []string{}
		for id, stack := range e2eGoroutines() {
			if _, ok := before[id]; !ok {
				left = append(left, stack)
			}
		}
		if len(left) == 0 {
			return
		}
		select {
		case <-deadline:
			slices.Sort(left)
			t.Fatalf("%d goroutines the blocks started outlived them:\n\n%s", len(left), strings.Join(left, "\n\n"))
		case <-time.After(self.settings.GoroutinePollInterval):
		}
	}
}
