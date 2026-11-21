//go:build sidecar_pending && sidecar_testhooks

package tlsedgehost

// 17.6's challenge-routing and eligibility rows on T3, FS-122's TLS-ALPN-01 clause and FS-41's custom-host step
// (TLS-M6-07; DESIGN-DETAILS 17.2.9, 17.2.10, 17.6; FS-35, FS-41, FS-122, §22.5's custom-host rows), through the real
// splitter and tlsedge to the real sockets engine on the stack's Redis and §13.7's production front at each listener.
//
// The world: tlsedge_t3_test.go's block on the private stack's Postgres and Redis, for a network whose authorization
// log is sidecartest's admission world, its members publishing and listening on tls/443/web to the public; one custom
// host of a verified domain mapped to web in Postgres, with TLS-M6-05's account row; the sockets engine on
// model.NewSocketStore over the stack's Redis, the production SocketProjectionGate over that log, tls/'s
// TlsNamespaceHooks and the production ticket keys; and at each listener device a TlsFront, whose custom source and
// TLS-ALPN-01 responder say what the registration lists, sent through the engine's own registration route on each
// change (TlsFrontArgs.UpdateTls). tlsedge's dial is SocketEdgeDialer's: Find through the engine's route as the edge's
// platform client, with the hello's ALPN ids and, for a custom host, its server name, the ticket checked as the
// dialer checks it, and the stream handed to the front of the ticket's listener. Everything runs on one fake clock.
//
//   - challenge routing: three listeners of tls/443/web, one holding the account's claim, which TLS-M6-05's Claim
//     takes on Postgres: every acme-tls/1 hello for the custom host, through the splitter and tlsedge, reaches the
//     holder's TLS-ALPN-01 answer; after a takeover, the holder's lease lapsed and another listener's Claim raising the
//     sequence, every one reaches the new holder, the old holder's stale claim standing on Redis until its next page
//     and then narrowed away; and a hello for a network name offering acme-tls/1 finds no listener, tlsedge's alert and
//     no certificate.
//   - eligibility: five listeners, two certified for the host by custom_issued rows TLS-M6-05's Register wrote and a
//     certificate installed on each device: 1,000 visitor hellos for the host reach only those two, on their own
//     certificates; when one's certificate enters CertifiedMargin on the clock, only the other; with none, the alert
//     and never a certificate for another name.
//   - FS-122's TLS-ALPN-01 clause: with the host pending, one lb interface stopped, an ordering desktop with a
//     TLS-ALPN-01 path starts no order, while one whose customer delegated _acme-challenge to the account's acme name
//     starts a DNS-01 one; once the interface is current the host turns ready, and the first desktop's TLS-ALPN-01
//     order completes, a CA surrogate forced to each lb interface's address reaching the desktop's front's answer,
//     and its chain registered.
//   - FS-41's custom-host step: the stack's Redis loses every key, as a site loss leaves it; the host's listeners come
//     back by their next heartbeat, their certified names narrowed again, visitors reaching only the certified one
//     before and after.
//
// What stands in, and why: a listener's stream reaches its front through TlsFrontTestListener, for §9.6's admission on
// the ticket the engine signed and SocketEdgeDialer's open over connect, as in tlsedge_front_t3_test.go. The listen
// facts past 17.2.10's two maps are the test's, as SOCK-M5-17's T3 sets them: the network verified, standing standard,
// at Team, and every device holding the network's pair; the two maps are model.GetTlsListenFacts's own, from
// TLS-M6-05's rows. The CA is a surrogate, since no test reaches a real CA and Pebble in its container cannot reach the
// test's loopback splits: it validates TLS-ALPN-01 by dialing each lb interface's split itself, as RFC 8737's
// validation would dial each address the host advertises. The orderer's custom routes are TLS-M6-05's
// model.TlsCustomStore, called with the caller its routes would build; POST /tls/custom-challenge, which writes a
// delegated DNS-01 value through certs and DNS, is no route here and answers 404, so the DNS-01 order starts and goes
// no further, its desktop releasing the claim. A desktop's TLS-ALPN-01 path is the test's over its own front, since no
// composition gives the orderer one yet (Sidecar.TlsCustomOrdererDeps leaves it nil).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The owner label the custom host maps to, as mapCustomHost maps every host, and its tls/ name.
const (
	t3RouteOwner = "web"
	t3RouteName  = "tls/443/" + t3RouteOwner
)

// The account's URI at the CA surrogate.
const t3RouteAccountUri = "https://ca.t3route.example/acme/acct/1"

// id-pe-acmeIdentifier (RFC 8737 §6.1).
var t3RouteAcmeIdentifierOid = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}

// The devices a world admits for its member: enough for five listeners and the orderers beside them.
const t3RouteDeviceCount = 8

// One world of the rows. See the file header.
type t3RouteWorld struct {
	*t3EdgeWorld
	net           *sidecartest.Net
	admission     *sidecartest.AdmissionNet
	devices       []*sidecartest.AdmissionDevice
	authenticator *sidecartest.FakeAuthenticator
	engine        *socketsservice.SocketsService
	gate          *socketsservice.SocketProjectionGate
	hooks         *socketsservice.TlsNamespaceHooks
	ticketKeys    *socketsservice.SocketTicketKeySet
	edgeNetworkId connect.Id
	edgeClientIds []sidecar.Id
	edgeToken     string
	// TLS-M6-05's store on the stack's Postgres, on a clock of its own, so a takeover moves it past a lease while the
	// engine's, tlsedge's and the fronts' clock stands
	custom      *model.TlsCustomStore
	customClock *sidecartest.FakeClock
	// the custom host, its verified domain, the domain's acme name, and the account's key
	host       string
	domain     string
	acmeName   string
	accountKey *ecdsa.PrivateKey
	ca         *t3RouteCa

	stateLock sync.Mutex
	// by listener client id
	listeners map[connect.Id]*t3RouteListener
}

// A world with the custom host mapped and its account made, tlsedge reading the host, the engine ready and no listener
// yet. The block's caps are off, since one visitor opens every connection inside a second of the clock.
func newT3RouteWorld(t testing.TB) *t3RouteWorld {
	t.Helper()
	ctx := t.Context()
	netSettings := sidecartest.DefaultNetSettings()
	netSettings.ClockStart = time.Now().UTC().Truncate(time.Second)
	net := sidecartest.NewNet(ctx, netSettings)
	t.Cleanup(net.Close)
	tier, err := net.AdmissionTier()
	if err != nil {
		t.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	admission := sidecartest.NewAdmissionNet(t, tier)
	t.Cleanup(admission.Close)
	memberId, first := admission.ConsentedMember("m")
	devices := []*sidecartest.AdmissionDevice{first}
	for index := 1; index < t3RouteDeviceCount; index += 1 {
		device := admission.Device(fmt.Sprintf("m device %d", index), memberId)
		admission.AdmitDevice(admission.Owner(), device, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
		devices = append(devices, device)
	}
	expires := net.Clock().Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	admission.SaveAclPolicy([]byte(fmt.Sprintf(
		`{"grants": [{ "src": ["autogroup:member"], "dst": ["dns:%s", "sock:%s"], "allow": ["publish", "listen"], "audience": "public", "expires": %q }]}`,
		t3RouteOwner, t3RouteName, expires,
	)), sidecartest.AclSaveMembers{})

	none := int64(-1)
	networkId := admission.NetworkId()
	t.Cleanup(model.SetDnsScopeLabelSource(func(_ context.Context, id server.Id) ([]string, bool) {
		if id != server.Id(networkId) {
			return nil, false
		}
		return admission.State().TeamScopeLabels(), true
	}))
	edge := newT3EdgeWorldOn(t, net.Clock(), &networkId, func(settings *HostSettings) {
		settings.TlsEdge.ColdLimits = &tlsedgeservice.TlsEndpointLimits{
			EndpointConcurrent: none, EndpointNewPerS: none, EndpointMbitPerS: none,
			VisitorConcurrent: none, VisitorNewPerS: none, VisitorAggregateMultiple: none,
		}
	})
	edge.fakes.Quota.SetLimits(connect.Id(networkId), &sidecar.QuotaLimits{Tier: "team", Limits: map[string]int64{
		"tls_endpoint_concurrent":        none,
		"tls_endpoint_new_per_s":         none,
		"tls_endpoint_mbit_per_s":        none,
		"tls_visitor_concurrent":         none,
		"tls_visitor_new_per_s":          none,
		"tls_visitor_aggregate_multiple": none,
	}})

	world := &t3RouteWorld{
		t3EdgeWorld:   edge,
		net:           net,
		admission:     admission,
		devices:       devices,
		authenticator: net.Fakes().Authenticator,
		edgeNetworkId: connect.NewId(),
		customClock:   sidecartest.NewFakeClock(net.Clock().Now()),
		listeners:     map[connect.Id]*t3RouteListener{},
	}
	world.ca = newT3RouteCa(t, world)

	// the custom host, its verified domain and the account
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	world.domain = fmt.Sprintf("corp-%x.example", suffix)
	world.host = "app." + world.domain
	acmeTokenBytes := make([]byte, 16)
	if _, err := rand.Read(acmeTokenBytes); err != nil {
		t.Fatal(err)
	}
	acmeToken := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(acmeTokenBytes))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time, acme_token) VALUES ($1, $2, 'challenge', $3, $4)`,
			server.Id(networkId), world.domain, server.NowUtc(), acmeToken,
		))
	}, server.TxReadCommitted)
	world.mapCustomHost(world.host)
	world.reloadNames()
	custom, err := model.NewTlsCustomStore(&model.TlsCustomStoreOptions{Roots: []string{world.root}, Now: world.customClock.Now})
	if err != nil {
		t.Fatal(err)
	}
	world.custom = custom
	world.accountKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	account, err := custom.AccountOp(ctx, world.caller(world.devices[0], true), &sidecar.TlsCustomAccountArgs{
		Domain:        world.domain,
		Op:            sidecar.TlsCustomAccountCreate,
		AccountUri:    t3RouteAccountUri,
		KeyThumbprint: sidecar.TlsCustomKeyThumbprint(&world.accountKey.PublicKey),
		KeyRepoId:     sidecar.NewId(),
	})
	if err != nil {
		t.Fatalf("the account: %v", err)
	}
	world.acmeName = account.AcmeName
	if world.acmeName == "" {
		t.Fatal("the account names no acme name")
	}

	// the engine on the stack's Redis, the production gate over the network's log, tls/'s hooks and the ticket keys
	gates := sidecartest.NewFakeServiceGates(nil)
	if err := gates.Set("sidecar:\n  sockets: {enabled: true, percent: 100}\n  urnetwork_edge: " + world.edgeNetworkId.String() + "\n"); err != nil {
		t.Fatal(err)
	}
	serviceHost := net.Fakes().ServiceHost()
	serviceHost.Redis = sidecarhost.NewRedis(nil)
	serviceHost.Gates = gates
	settings := socketsservice.DefaultSocketsSettings()
	gateSettings := *settings.Gate
	cacheSettings := *gateSettings.Cache
	cacheSettings.PollJitter = 0
	cacheSettings.FailureBackoff = 0
	gateSettings.Cache = &cacheSettings
	gateSettings.BehindReloadInterval = time.Hour
	gate, err := socketsservice.NewSocketProjectionGate(ctx, serviceHost, sidecar.NewAclGateDirectSource(nil, net.Host().Blobs, nil), &gateSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Close)
	world.gate = gate
	engine, err := socketsservice.NewSocketsService(ctx, serviceHost, model.NewSocketStore(net.Clock(), nil), gate, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	world.engine = engine
	hooks, err := socketsservice.NewTlsNamespaceHooks(ctx, socketsservice.TlsListenFactsFunc(world.listenFacts), net.Clock(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hooks.Close)
	hooks.SetScopeConsent(gate)
	if err := engine.RegisterNamespace(sidecar.SocketNamespaceTls, hooks); err != nil {
		t.Fatal(err)
	}
	world.hooks = hooks
	keyFile, err := socketsservice.SocketTicketKeyFileAdd(nil, net.Clock().Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ticketKeys, err := socketsservice.NewSocketTicketKeySet(ctx, func() ([]byte, error) { return keyFile, nil }, net.Clock(), &hostTestLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.SetTicketKeys(ticketKeys)
	world.ticketKeys = ticketKeys
	t3RouteAwaitCount(t, "the engine's first probe", engine.Probes)
	t3RouteAwaitCount(t, "the gate's first over-limit read", gate.OverLimitReads)
	for {
		_, changed := gate.Loads()
		if _, err := gate.View(connect.Id(networkId)); err == nil {
			break
		}
		select {
		case <-changed:
		case <-time.After(t3EdgeGuardTimeout):
			t.Fatal("the gate never loaded the network")
		}
	}
	world.loadFacts()

	// the edge's K platform clients, the first one's sockets token on every Find (QD-1054)
	for range 2 {
		world.edgeClientIds = append(world.edgeClientIds, sidecar.NewId())
	}
	now := net.Clock().Now()
	world.edgeToken = world.authenticator.Issue(&sidecar.Principal{
		NetworkId: world.edgeNetworkId,
		ClientId:  connect.Id(world.edgeClientIds[0]),
		Audience:  socketsservice.SocketsAudience,
		IssuedAt:  now,
		ExpiresAt: now.Add(48 * time.Hour),
	})
	world.dial.setRoute(world.edgeDial)
	return world
}

// Waits until counts is past zero.
func t3RouteAwaitCount(t testing.TB, what string, counts func() (int, chan struct{})) {
	t.Helper()
	guard := time.After(t3EdgeGuardTimeout)
	for {
		count, changed := counts()
		if 0 < count {
			return
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("%s never came", what)
		}
	}
}

// The listen facts the hooks read: model.GetTlsListenFacts on the stack's Postgres, whose 17.2.10 maps are TLS-M6-05's
// rows read at the custom store's time, with what lies past those maps as SOCK-M5-17's T3 sets it (see the header).
func (self *t3RouteWorld) listenFacts(ctx context.Context, networkId connect.Id) (*socketsservice.TlsListenFacts, error) {
	facts, err := model.GetTlsListenFacts(ctx, server.Id(networkId), self.customClock.Now())
	if err != nil {
		return nil, err
	}
	facts.Verified = true
	facts.Standing = sidecar.AbuseStandingStandard
	facts.Tier = "team"
	for _, device := range self.devices {
		facts.Certs = append(facts.Certs, &socketsservice.TlsListenCert{
			SidecarId:   connect.Id(device.SidecarId),
			NotAfter:    self.clock.Now().Add(24 * time.Hour),
			NetworkPair: true,
		})
	}
	return facts, nil
}

// Has the hooks read the facts as Postgres holds them now.
func (self *t3RouteWorld) loadFacts() {
	self.t.Helper()
	if err := self.hooks.LoadFacts(self.ctx, connect.Id(self.networkId)); err != nil {
		self.t.Fatal(err)
	}
}

// The caller TLS-M6-05's routes would build for device from its certs token and the network's projection: a sidecar
// holding the publish grant on web, an admin when admin.
func (self *t3RouteWorld) caller(device *sidecartest.AdmissionDevice, admin bool) *model.TlsCustomCaller {
	return &model.TlsCustomCaller{
		NetworkId: server.Id(self.networkId),
		ClientId:  server.NewId(),
		SidecarId: server.Id(device.SidecarId),
		PublishGranted: func(ownerLabel string) bool {
			return ownerLabel == t3RouteOwner
		},
		Admin:             admin,
		IdentityPublicKey: device.PublicKey(),
	}
}

// One request to the engine's routes, as caller's bearer.
func (self *t3RouteWorld) serve(method string, path string, token string, body any) *httptest.ResponseRecorder {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, "http://sockets.t3route.example"+path, nil)
	} else {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			self.t.Fatal(err)
		}
		request = httptest.NewRequest(method, "http://sockets.t3route.example"+path, bytes.NewReader(bodyBytes))
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	self.engine.Handler().ServeHTTP(recorder, request)
	return recorder
}

// The error a Find's refusal is, as SocketEdgeDialer maps it.
func t3RouteFindError(refusal *sidecar.SocketError) error {
	switch refusal.Code {
	case sidecar.SocketErrorCodeNoListener:
		return &sidecar.SocketCodeError{Err: sidecar.ErrSocketNoListener}
	case sidecar.SocketErrorCodeBusy:
		return &sidecar.SocketCodeError{Err: sidecar.ErrSocketBusy}
	case sidecar.SocketErrorCodeQuota:
		return &sidecar.SocketCodeError{Err: sidecar.ErrSocketQuota}
	case sidecar.SocketErrorCodeUnavailable:
		return &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}
	}
	return &sidecar.SocketCodeError{Err: sidecar.ErrSocketDenied}
}

// tlsedge's dial, as SocketEdgeDialer makes it: Find through the engine's route with the edge's K client ids, the
// hello's ALPN ids and its server name, then each ticket checked, its signature, a client this edge holds, the network
// and the name, and the stream to the ticket's listener handed to its front, the hello's first bytes first.
func (self *t3RouteWorld) edgeDial(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
	unavailable := &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}
	networkId := connect.Id(request.NetworkId)
	clientIds := []*connect.Id{}
	for _, clientId := range self.edgeClientIds {
		id := connect.Id(clientId)
		clientIds = append(clientIds, &id)
	}
	recorder := self.serve(http.MethodPost, "/socket/find", self.edgeToken, &sidecar.SocketFindArgs{
		Name:        request.Name,
		NetworkId:   &networkId,
		Protocol:    request.Protocol,
		Alpn:        request.Alpns,
		ServerName:  request.ServerName,
		AffinityKey: request.AffinityKey,
		ClientIds:   clientIds,
	})
	result := &sidecar.SocketFindResult{}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), result) != nil {
		return nil, unavailable
	}
	if result.Error != nil {
		return nil, t3RouteFindError(result.Error)
	}
	for _, findTicket := range []*sidecar.SocketFindTicket{result.Primary, result.Backup} {
		if findTicket == nil || len(findTicket.Ticket) == 0 {
			continue
		}
		ticket, err := sidecar.VerifySocketTicket(findTicket.Ticket, self.ticketKeys.VerifyKeys(), uint64(self.clock.Now().UnixMilli()))
		if err != nil {
			continue
		}
		dialerClientId, err := sidecar.IdFromBytes(ticket.GetDialerClientId())
		if err != nil || !slices.Contains(self.edgeClientIds, dialerClientId) || !bytes.Equal(ticket.GetNetworkId(), request.NetworkId.Bytes()) ||
			ticket.GetName() != request.Name {
			continue
		}
		listenerClientId, err := connect.IdFromBytes(ticket.GetListenerClientId())
		if err != nil {
			continue
		}
		listener := self.listenerOf(listenerClientId)
		if listener == nil {
			continue
		}
		metadata, err := request.Metadata(ticket.GetGrantExpireTimeMillis())
		if err != nil {
			return nil, unavailable
		}
		stream := &t3EdgeStream{
			toDevice:   newT3EdgePipe(),
			fromDevice: newT3EdgePipe(),
			sent:       &t3EdgeRecording{},
			resets:     make(chan protocol.StreamResetCode, 1),
		}
		conn := &t3EdgeDeviceConn{reader: io.MultiReader(bytes.NewReader(slices.Clone(request.EarlyData)), stream.toDevice.reader), stream: stream}
		if !listener.listener.Deliver(&t3FrontStream{t3EdgeDeviceConn: conn}, ticket, metadata) {
			stream.Close()
			continue
		}
		listener.accepted.Add(1)
		return stream, nil
	}
	return nil, &sidecar.SocketCodeError{Err: sidecar.ErrSocketNoListener}
}

// The listener whose client id a ticket names, nil for none.
func (self *t3RouteWorld) listenerOf(clientId connect.Id) *t3RouteListener {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.listeners[clientId]
}

// What a front reads its custom names from: TLS-M5-18's certificate manager on a device, here the test's.
type t3RouteCustom struct {
	stateLock sync.Mutex
	entries   []*sidecar.TlsFrontCustomCertificate
	changed   chan struct{}
}

var _ sidecar.TlsFrontCustomSource = (*t3RouteCustom)(nil)

func newT3RouteCustom(entries ...*sidecar.TlsFrontCustomCertificate) *t3RouteCustom {
	return &t3RouteCustom{entries: entries, changed: make(chan struct{})}
}

func (self *t3RouteCustom) CustomCertificates() []*sidecar.TlsFrontCustomCertificate {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.entries)
}

func (self *t3RouteCustom) CustomChanged() <-chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.changed
}

// One listener of tls/443/web: an admitted device of the member, its client and sockets token, its registration's
// listen_id and claim, and its front.
type t3RouteListener struct {
	world    *t3RouteWorld
	device   *sidecartest.AdmissionDevice
	clientId connect.Id
	token    string
	listenId connect.Id
	claim    []byte
	front    *sidecar.TlsFront
	listener *sidecar.TlsFrontTestListener
	custom   *t3RouteCustom
	// the edge's streams its front took
	accepted atomic.Int64

	stateLock  sync.Mutex
	setVersion uint64
	// the lists its newest accepted page carried, and the pages accepted so far
	registered   *sidecar.TlsFrontUpdate
	pages        int
	pagesChanged chan struct{}
}

// A listener on the world's device index, its front serving web's network name on a certificate of its own and the
// custom names custom lists, none for nil, registered through the engine at once. Returns once its first page was
// accepted.
func (self *t3RouteWorld) addListener(index int, custom *t3RouteCustom) *t3RouteListener {
	t := self.t
	t.Helper()
	device := self.devices[index]
	now := self.clock.Now()
	listener := &t3RouteListener{
		world:        self,
		device:       device,
		clientId:     connect.NewId(),
		listenId:     connect.NewId(),
		custom:       custom,
		listener:     sidecar.NewTlsFrontTestListener(sidecar.Id(self.edgeNetworkId)),
		pagesChanged: make(chan struct{}),
	}
	sidecarId := connect.Id(device.SidecarId)
	listener.token = self.authenticator.Issue(&sidecar.Principal{
		NetworkId: connect.Id(self.networkId),
		ClientId:  listener.clientId,
		SidecarId: &sidecarId,
		Audience:  socketsservice.SocketsAudience,
		IssuedAt:  now,
		ExpiresAt: now.Add(48 * time.Hour),
	})
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.SocketRegistrationClaim{
		NetworkId: self.networkId.Bytes(),
		Name:      t3RouteName,
		ListenId:  listener.listenId.Bytes(),
		Protocol:  "tls",
		AppMode:   "http",
	})
	if err != nil {
		t.Fatal(err)
	}
	listener.claim, err = proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.Signed{
		Body:      body,
		Signature: ed25519.Sign(device.Key, bridge.SignedBytes(sidecar.SocketRegistrationPurpose, bridge.SignedType_SIGNED_SOCKET_REGISTRATION, body)),
		Type:      uint32(bridge.SignedType_SIGNED_SOCKET_REGISTRATION),
	})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.listeners[listener.clientId] = listener
	}()
	networkCertificate := newT3EdgeDeviceCertificate(t, []string{self.serverName(t3RouteOwner)}, now)
	// a nil source stays a nil interface, the network's own names alone
	var customSource sidecar.TlsFrontCustomSource
	if custom != nil {
		customSource = custom
	}
	settings := sidecar.DefaultTlsFrontSettings()
	// the probe would dial the service, which nothing here serves
	settings.TargetProbeInterval = 0
	front, err := sidecar.NewTlsFront(self.ctx, &sidecar.TlsFrontArgs{
		Settings:  settings,
		Target:    &sidecar.TlsFrontTarget{Name: t3RouteName, ServerNames: []string{self.serverName(t3RouteOwner)}, AppMode: "http", LocalAddress: "127.0.0.1:9"},
		Listener:  listener.listener,
		NetworkId: sidecar.Id(self.networkId),
		SidecarId: device.SidecarId,
		Policy:    sidecar.NewTlsFrontConfigPolicy(device.SidecarId, self.admission.State),
		Certificate: func(serverName string) (*tls.Certificate, error) {
			return &networkCertificate.certificate, nil
		},
		Secrets:   &t3FrontSecrets{values: map[string][]byte{}},
		IssuerUrl: "https://issuer.t3route.example",
		Limits: func(ctx context.Context) (*sidecar.QuotaLimits, error) {
			return &sidecar.QuotaLimits{Tier: "team"}, nil
		},
		DialTarget: func(ctx context.Context) (net.Conn, error) {
			service, peer := net.Pipe()
			go func() {
				defer service.Close()
				io.Copy(service, service)
			}()
			return peer, nil
		},
		Custom:    customSource,
		UpdateTls: listener.register,
		Clock:     self.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { front.Close() })
	listener.front = front
	listener.awaitPage(func(update *sidecar.TlsFrontUpdate) bool { return true })
	return listener
}

// Sends update as the listener's whole-set page, a new set_version under the same listen_id and claim, as
// SocketListener.UpdateTls does: nil once the engine accepted the entry.
func (self *t3RouteListener) register(update *sidecar.TlsFrontUpdate) error {
	setVersion := func() uint64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.setVersion += 1
		return self.setVersion
	}()
	listenId := self.listenId
	recorder := self.world.serve(http.MethodPost, "/socket/registrations", self.token, &sidecar.SocketRegistrationsArgs{
		SetVersion: setVersion,
		Registrations: []*sidecar.SocketRegistration{{
			Name:           t3RouteName,
			ListenId:       &listenId,
			Protocol:       "tls",
			AppMode:        "http",
			Alpn:           update.Alpn,
			Weight:         100,
			MaxStreams:     256,
			Serving:        true,
			Claim:          self.claim,
			CertifiedNames: update.CertifiedNames,
			AcmeNames:      update.AcmeNames,
		}},
	})
	result := &sidecar.SocketRegistrationsResult{}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), result) != nil {
		return fmt.Errorf("the page answered %d", recorder.Code)
	}
	if result.Error != nil || len(result.Statuses) != 1 || result.Statuses[0] != sidecar.SocketRegistrationStatusOk {
		return fmt.Errorf("the page's entry answered %+v, %v", result.Error, result.Statuses)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	updateCopy := *update
	self.registered = &updateCopy
	self.pages += 1
	close(self.pagesChanged)
	self.pagesChanged = make(chan struct{})
	return nil
}

// Sends the newest accepted page again, as the registrar's heartbeat does, so its lease outlives a clock moved past it.
func (self *t3RouteListener) heartbeat() {
	t := self.world.t
	t.Helper()
	registered := func() *sidecar.TlsFrontUpdate {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.registered
	}()
	if registered == nil {
		t.Fatal("a heartbeat of a listener that registered nothing")
	}
	if err := self.register(registered); err != nil {
		t.Fatalf("a heartbeat: %v", err)
	}
}

// Waits until the newest accepted page's lists satisfy accept.
func (self *t3RouteListener) awaitPage(accept func(update *sidecar.TlsFrontUpdate) bool) {
	t := self.world.t
	t.Helper()
	if err := self.awaitPageCtx(t.Context(), accept); err != nil {
		t.Fatal(err)
	}
}

// awaitPage within ctx and the guard, an error past either.
func (self *t3RouteListener) awaitPageCtx(ctx context.Context, accept func(update *sidecar.TlsFrontUpdate) bool) error {
	guard := time.After(t3EdgeGuardTimeout)
	for {
		changed := func() chan struct{} {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.registered != nil && accept(self.registered) {
				return nil
			}
			return self.pagesChanged
		}()
		if changed == nil {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-guard:
			return errors.New("the listener's front sent no page the test waits for")
		}
	}
}

// Whether update lists name as an ACME name under claimSeq.
func t3RouteListsAcme(update *sidecar.TlsFrontUpdate, name string, claimSeq uint64) bool {
	return slices.ContainsFunc(update.AcmeNames, func(acmeName *sidecar.SocketAcmeName) bool {
		return acmeName.Name == name && acmeName.ClaimSeq == claimSeq
	}) && slices.Contains(update.Alpn, "acme-tls/1")
}

// The leases Redis holds for web, as the pick reads them: each listener client's certified and ACME names.
func (self *t3RouteWorld) storedLeases() map[connect.Id]*socketsservice.SocketCandidate {
	self.t.Helper()
	store := model.NewSocketStore(self.clock, nil)
	listeners, err := store.ListListeners(self.ctx, connect.Id(self.networkId), []byte(t3RouteName))
	if err != nil {
		self.t.Fatal(err)
	}
	leases := map[connect.Id]*socketsservice.SocketCandidate{}
	for _, listener := range listeners {
		leases[listener.Candidate.ListenerClientId] = listener.Candidate
	}
	return leases
}

// A self-signed CA the rows' certificates chain to, the custom chains TLS-M6-05's Register checks.
func newT3RouteCaCertificate(t testing.TB, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "t3route surrogate root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

// A device's CSR for the custom host on a fresh P-256 key and its attestation by the device's identity key, as
// TLS-M5-18's manager posts them to POST /tls/cert-order.
func (self *t3RouteWorld) customCsr(device *sidecartest.AdmissionDevice) (*ecdsa.PrivateKey, *sidecar.TlsCertOrderArgs) {
	t := self.t
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDer, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{self.host}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(csrDer)
	if err != nil {
		t.Fatal(err)
	}
	set, err := sidecar.TlsCustomIdentifierSet(self.host, []string{self.root})
	if err != nil {
		t.Fatal(err)
	}
	setSha256, err := sidecar.TlsIdentifierSetHash(set)
	if err != nil {
		t.Fatal(err)
	}
	spkiSha256 := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
	attestation, err := sidecar.SignTlsKeyAttestation(&protocol.TlsKeyAttestation{
		NetworkId:           self.networkId.Bytes(),
		SidecarId:           device.SidecarId.Bytes(),
		SpkiSha256:          spkiSha256[:],
		IdentifierSetSha256: setSha256[:],
		IssueTimeMillis:     uint64(self.clock.Now().UnixMilli()),
	}, func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
		return &bridge.Signed{
			Body:      body,
			SignerId:  device.SidecarId.Bytes(),
			Signature: ed25519.Sign(device.Key, bridge.SignedBytes(purpose, signedType, body)),
			Type:      uint32(signedType),
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return key, &sidecar.TlsCertOrderArgs{Csr: csrDer, Attestation: attestation}
}

// A device's queued custom row for the host, through TLS-M6-05's custom branch of POST /tls/cert-order.
func (self *t3RouteWorld) queueOrder(device *sidecartest.AdmissionDevice) (*ecdsa.PrivateKey, sidecar.Id) {
	t := self.t
	t.Helper()
	key, args := self.customCsr(device)
	result, err := self.custom.QueueOrder(self.ctx, self.caller(device, false), args, server.NewId(), 0)
	if err != nil {
		t.Fatalf("the custom order: %v", err)
	}
	if result.State != sidecar.TlsCertStateCustomQueued {
		t.Fatalf("the custom order is %s", result.State)
	}
	return key, result.CertId
}

// device's claim of the account's lock to order certId, through TLS-M6-05's POST /tls/custom-claim.
func (self *t3RouteWorld) claimOrder(device *sidecartest.AdmissionDevice, certId sidecar.Id) *sidecar.TlsCustomClaimResult {
	t := self.t
	t.Helper()
	claim, err := self.custom.Claim(self.ctx, self.caller(device, false), &sidecar.TlsCustomClaimArgs{
		Domain:  self.domain,
		Purpose: sidecar.TlsCustomPurposeOrder,
		CertId:  certId,
		KeySeq:  1,
	})
	if err != nil {
		t.Fatalf("the claim: %v", err)
	}
	return claim
}

// A certificate for the device's custom row as the CA would issue it and the device install it: its own row queued,
// claimed and registered with a chain to the surrogate's root whose leaf ends at notAfter, so the row reads
// custom_issued until then. The certificate on the device's key, for its front.
func (self *t3RouteWorld) certify(device *sidecartest.AdmissionDevice, notAfter time.Time) *tls.Certificate {
	t := self.t
	t.Helper()
	key, certId := self.queueOrder(device)
	claim := self.claimOrder(device, certId)
	chain := self.ca.issue(&key.PublicKey, notAfter)
	if err := self.custom.Register(self.ctx, self.caller(device, false), &sidecar.TlsCertRegisterArgs{
		CertId:     certId,
		ClaimSeq:   claim.ClaimSeq,
		Chain:      chain,
		AccountUri: t3RouteAccountUri,
	}); err != nil {
		t.Fatalf("the register: %v", err)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}
}

// The acmeIdentifier a TLS-ALPN-01 certificate carries: SHA-256 of a key authorization, critical (RFC 8737 §3).
func t3RouteAcmeIdentifier(certificate *x509.Certificate) ([]byte, bool) {
	for _, extension := range certificate.Extensions {
		if !extension.Id.Equal(t3RouteAcmeIdentifierOid) || !extension.Critical {
			continue
		}
		var digest []byte
		if rest, err := asn1.Unmarshal(extension.Value, &digest); err == nil && len(rest) == 0 && len(digest) == sha256.Size {
			return digest, true
		}
	}
	return nil, false
}

// What a TLS-ALPN-01 validation at address reads for host: a handshake offering only acme-tls/1 that negotiates it,
// on a certificate whose one name is host. Its leaf, or why not.
func t3RouteAcmeAnswer(address string, host string, timeout time.Duration) (*x509.Certificate, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	client := tls.Client(conn, &tls.Config{ServerName: host, NextProtos: []string{"acme-tls/1"}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := client.Handshake(); err != nil {
		return nil, err
	}
	state := client.ConnectionState()
	if state.NegotiatedProtocol != "acme-tls/1" || len(state.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no acme-tls/1 answer: %q", state.NegotiatedProtocol)
	}
	leaf := state.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != host {
		return nil, fmt.Errorf("a challenge certificate for %v", leaf.DNSNames)
	}
	return leaf, nil
}

// The SHA-256 of keyAuthorization, which a challenge certificate for it carries.
func t3RouteKeyAuthorizationDigest(keyAuthorization string) []byte {
	digest := sha256.Sum256([]byte(keyAuthorization))
	return digest[:]
}

// The split's address, the one an lb interface advertises for the host.
func (self *t3Split) address() string {
	return fmt.Sprintf("127.0.0.1:%d", self.port)
}

// A visitor's hello for the custom host through split, offering http/1.1 and h2: the leaf it completed TLS on, or why
// not.
func (self *t3RouteWorld) visitCustom(split *t3Split) (*x509.Certificate, error) {
	conn, err := net.Dial("tcp", split.address())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(t3EdgeGuardTimeout))
	client := tls.Client(conn, &tls.Config{ServerName: self.host, NextProtos: []string{"h2", "http/1.1"}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := client.Handshake(); err != nil {
		return nil, err
	}
	state := client.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil, errors.New("no certificate")
	}
	return state.PeerCertificates[0], nil
}

// The split in front of the world for one lb interface of api, started and loaded at the custom host's version.
func (self *t3RouteWorld) startSplit(api *t3SplitApi, interfaceName string) *t3Split {
	self.t.Helper()
	split := newT3SplitOn(self.t, self.t3EdgeWorld, api, interfaceName, nil)
	split.startLb(t3RouteMapVersion(self.t, self.t3EdgeWorld))
	return split
}

// The newest custom-host map's version.
func t3RouteMapVersion(t testing.TB, world *t3EdgeWorld) uint64 {
	t.Helper()
	customHostMap, err := model.GetDnsCustomHostMap(world.ctx, world.root)
	if err != nil || customHostMap == nil {
		t.Fatalf("no custom-host map: %v", err)
	}
	return customHostMap.Version
}

// 17.6's challenge-routing row on T3 (extends FS-35): three listeners of tls/443/web, one holding the account's claim:
// every acme-tls/1 hello for the custom host through the real splitter and tlsedge reaches the holder's TLS-ALPN-01
// answer; after a takeover every one reaches the new holder, the old holder's stale claim standing on Redis until its
// next page narrows it away; and a hello for a network name offering acme-tls/1 finds no listener.
func TestT3CustomHostAcmeHellosReachTheClaimHolder(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouteWorld(t)
		split := world.startSplit(newT3SplitApi(t, world.t3EdgeWorld, []string{t3SplitInterface}), t3SplitInterface)
		listeners := []*t3RouteListener{}
		for index := range 3 {
			// each serves the host once a certificate installs, none has yet: the host is listed, not certified
			listeners = append(listeners, world.addListener(index, newT3RouteCustom(&sidecar.TlsFrontCustomCertificate{Name: world.host})))
		}
		// the row the claims name: another device's queued order for the host
		_, certId := world.queueOrder(world.devices[3])
		expireTime := world.clock.Now().Add(10 * time.Minute)

		// the first holder's claim, and its front's challenge under it
		claim := world.claimOrder(listeners[0].device, certId)
		firstKeyAuthorization := "first-token.t3route"
		if err := listeners[0].front.SetCustomAlpnChallenge(world.host, firstKeyAuthorization, expireTime, claim.ClaimSeq); err != nil {
			t.Fatal(err)
		}
		listeners[0].awaitPage(func(update *sidecar.TlsFrontUpdate) bool {
			return t3RouteListsAcme(update, world.host, claim.ClaimSeq)
		})
		if lease := world.storedLeases()[listeners[0].clientId]; lease == nil || lease.AcmeNames[world.host] != claim.ClaimSeq {
			t.Fatalf("Redis holds the holder's lease as %+v", lease)
		}
		for range 20 {
			leaf, err := t3RouteAcmeAnswer(split.address(), world.host, t3EdgeGuardTimeout)
			if err != nil {
				t.Fatalf("an acme-tls/1 hello for the custom host: %v", err)
			}
			if digest, ok := t3RouteAcmeIdentifier(leaf); !ok || !bytes.Equal(digest, t3RouteKeyAuthorizationDigest(firstKeyAuthorization)) {
				t.Fatal("an acme-tls/1 hello for the custom host reached another answer than the holder's")
			}
		}
		if accepted := listeners[1].accepted.Load() + listeners[2].accepted.Load(); accepted != 0 {
			t.Fatalf("listeners holding no claim took %d acme-tls/1 streams", accepted)
		}

		// a network name offering acme-tls/1 finds no listener: tlsedge's alert, and no certificate
		if answer := split.raw(t3EdgeHello(t, world.serverName(t3RouteOwner), []string{"acme-tls/1"})); !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertInternalError)) {
			t.Fatalf("a network name's acme-tls/1 hello answered %x", answer)
		}

		// the takeover: the holder's lease lapses on the store's clock, and another listener claims, raising the sequence
		world.customClock.Advance(certsservice.DefaultCertsCustomSettings().LockLease + time.Second)
		takeover := world.claimOrder(listeners[1].device, certId)
		if takeover.ClaimSeq <= claim.ClaimSeq {
			t.Fatalf("the takeover's claim %d is not above %d", takeover.ClaimSeq, claim.ClaimSeq)
		}
		secondKeyAuthorization := "second-token.t3route"
		if err := listeners[1].front.SetCustomAlpnChallenge(world.host, secondKeyAuthorization, expireTime, takeover.ClaimSeq); err != nil {
			t.Fatal(err)
		}
		listeners[1].awaitPage(func(update *sidecar.TlsFrontUpdate) bool {
			return t3RouteListsAcme(update, world.host, takeover.ClaimSeq)
		})
		// the old holder's claim still stands on Redis until its next page; the pick takes the higher
		if lease := world.storedLeases()[listeners[0].clientId]; lease == nil || lease.AcmeNames[world.host] != claim.ClaimSeq {
			t.Fatalf("the old holder's lease before its next page: %+v", lease)
		}
		checkNewHolder := func() {
			t.Helper()
			for range 20 {
				leaf, err := t3RouteAcmeAnswer(split.address(), world.host, t3EdgeGuardTimeout)
				if err != nil {
					t.Fatalf("an acme-tls/1 hello after the takeover: %v", err)
				}
				if digest, ok := t3RouteAcmeIdentifier(leaf); !ok || !bytes.Equal(digest, t3RouteKeyAuthorizationDigest(secondKeyAuthorization)) {
					t.Fatal("an acme-tls/1 hello after the takeover reached another answer than the new holder's")
				}
			}
		}
		checkNewHolder()
		// the old holder's next page: its stale claim is narrowed away, and every hello still reaches the new holder
		listeners[0].heartbeat()
		if lease := world.storedLeases()[listeners[0].clientId]; lease == nil || 0 < len(lease.AcmeNames) {
			t.Fatalf("the old holder's lease after its next page: %+v", lease)
		}
		checkNewHolder()
		if accepted := listeners[2].accepted.Load(); accepted != 0 {
			t.Fatalf("the listener that never held a claim took %d acme-tls/1 streams", accepted)
		}
	})
}

// 17.6's eligibility row on T3 (extends FS-35 and FS-122): five listeners of tls/443/web, two certified for the custom
// host by TLS-M6-05's rows: 1,000 visitor hellos through the real splitter and tlsedge reach only those two, on their
// own certificates; once one's certificate enters CertifiedMargin on the clock, only the other; with none, tlsedge's
// alert and never a certificate for another name.
func TestT3CustomHostVisitorsReachOnlyCertifiedListeners(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouteWorld(t)
		split := world.startSplit(newT3SplitApi(t, world.t3EdgeWorld, []string{t3SplitInterface}), t3SplitInterface)
		margin := sidecar.DefaultTlsFrontSettings().CertifiedMargin
		start := world.clock.Now()
		// the first certificate leaves the list 30 s from now, the second 40 s
		notAfters := []time.Time{start.Add(margin + 30*time.Second), start.Add(margin + 40*time.Second)}
		certificates := []*tls.Certificate{}
		for index, notAfter := range notAfters {
			certificates = append(certificates, world.certify(world.devices[index], notAfter))
		}
		world.loadFacts()
		listeners := []*t3RouteListener{}
		for index := range 5 {
			custom := newT3RouteCustom(&sidecar.TlsFrontCustomCertificate{Name: world.host})
			if index < len(certificates) {
				custom = newT3RouteCustom(&sidecar.TlsFrontCustomCertificate{Name: world.host, Certificate: certificates[index], NotAfter: notAfters[index]})
			}
			listeners = append(listeners, world.addListener(index, custom))
		}
		for index, listener := range listeners {
			listener.awaitPage(func(update *sidecar.TlsFrontUpdate) bool {
				return slices.Contains(update.CertifiedNames, world.host) == (index < len(certificates))
			})
		}
		leases := world.storedLeases()
		for index, listener := range listeners {
			certified := slices.Contains(leases[listener.clientId].CertifiedNames, world.host)
			if certified != (index < len(certificates)) {
				t.Fatalf("listener %d's lease on Redis certified %v", index, certified)
			}
		}

		// visitors' hellos: each reaches one of the two certified listeners, on its own certificate
		visit := func(count int, want []int) map[int]int {
			t.Helper()
			reached := map[int]int{}
			for range count {
				leaf, err := world.visitCustom(split)
				if err != nil {
					t.Fatalf("a visitor's hello for the custom host: %v", err)
				}
				index := slices.IndexFunc(certificates, func(certificate *tls.Certificate) bool {
					return bytes.Equal(certificate.Certificate[0], leaf.Raw)
				})
				if !slices.Contains(want, index) {
					t.Fatalf("a visitor's hello reached a certificate of no listener it may reach (%d)", index)
				}
				reached[index] += 1
			}
			for index := range listeners[len(certificates):] {
				if accepted := listeners[len(certificates)+index].accepted.Load(); accepted != 0 {
					t.Fatalf("an uncertified listener took %d visitor streams", accepted)
				}
			}
			return reached
		}
		reached := visit(1000, []int{0, 1})
		t.Logf("1,000 visitors: %d to the first certified listener, %d to the second", reached[0], reached[1])

		// the first certificate enters the margin: its front drops the host, and every visitor reaches the second
		world.clock.Advance(notAfters[0].Add(-margin).Sub(world.clock.Now()) + time.Second)
		listeners[0].awaitPage(func(update *sidecar.TlsFrontUpdate) bool { return !slices.Contains(update.CertifiedNames, world.host) })
		for _, listener := range listeners[1:] {
			listener.heartbeat()
		}
		visit(200, []int{1})

		// the second enters it too: no listener is certified, so tlsedge's alert answers and no certificate is presented
		world.clock.Advance(notAfters[1].Add(-margin).Sub(world.clock.Now()) + time.Second)
		listeners[1].awaitPage(func(update *sidecar.TlsFrontUpdate) bool { return !slices.Contains(update.CertifiedNames, world.host) })
		for _, listener := range append([]*t3RouteListener{listeners[0]}, listeners[2:]...) {
			listener.heartbeat()
		}
		for range 20 {
			if answer := split.raw(t3EdgeHello(t, world.host, []string{"h2", "http/1.1"})); !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertInternalError)) {
				t.Fatalf("a visitor with no certified listener read %x", answer)
			}
		}
		for index, listener := range listeners {
			if index < len(certificates) {
				continue
			}
			if accepted := listener.accepted.Load(); accepted != 0 {
				t.Fatalf("an uncertified listener took %d visitor streams", accepted)
			}
		}
	})
}

// One TLS-ALPN-01 validation the surrogate made: the address it dialed and how the answer failed, nil for a pass.
type t3RouteValidation struct {
	address string
	err     error
}

// The CA surrogate (see the file header): RFC 8555's order, authorization and challenge states, one authorization an
// order offering tls-alpn-01 and dns-01; a TLS-ALPN-01 challenge validated by dialing every address the lb interfaces
// advertise for the host and reading back a certificate for the account's key authorization from each; chains issued
// to its own root. Safe for concurrent use.
type t3RouteCa struct {
	world       *t3RouteWorld
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	serial      atomic.Int64

	stateLock      sync.Mutex
	addresses      []string
	newOrders      int
	orders         map[string]*sidecar.TlsAcmeOrder
	authorizations map[string]*sidecar.TlsAcmeAuthorization
	// each authorization's order, each challenge's authorization, and each certificate URL's chain
	authorizationOrders     map[string]string
	challengeAuthorizations map[string]string
	chains                  map[string][][]byte
	validations             []*t3RouteValidation
}

var _ sidecar.TlsAcmeClient = (*t3RouteCa)(nil)

func newT3RouteCa(t testing.TB, world *t3RouteWorld) *t3RouteCa {
	t.Helper()
	certificate, key := newT3RouteCaCertificate(t, world.clock.Now())
	return &t3RouteCa{
		world:                   world,
		certificate:             certificate,
		key:                     key,
		orders:                  map[string]*sidecar.TlsAcmeOrder{},
		authorizations:          map[string]*sidecar.TlsAcmeAuthorization{},
		authorizationOrders:     map[string]string{},
		challengeAuthorizations: map[string]string{},
		chains:                  map[string][][]byte{},
	}
}

// Has each validation dial addresses, the host's advertised ones.
func (self *t3RouteCa) setAddresses(addresses ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addresses = slices.Clone(addresses)
}

// The newOrders the surrogate took.
func (self *t3RouteCa) newOrderCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.newOrders
}

// The validations the surrogate made.
func (self *t3RouteCa) validationsMade() []*t3RouteValidation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.validations)
}

// A chain to the root for the host on publicKey, its leaf ending at notAfter.
func (self *t3RouteCa) issue(publicKey crypto.PublicKey, notAfter time.Time) [][]byte {
	t := self.world.t
	t.Helper()
	now := self.world.clock.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1000 + self.serial.Add(1)),
		Subject:      pkix.Name{CommonName: self.world.host},
		DNSNames:     []string{self.world.host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, self.certificate, publicKey, self.key)
	if err != nil {
		t.Fatal(err)
	}
	return [][]byte{der, self.certificate.Raw}
}

func (self *t3RouteCa) NewAccount(ctx context.Context, key crypto.Signer, onlyReturnExisting bool) (string, error) {
	return "", errors.New("the surrogate makes no account")
}

func (self *t3RouteCa) Account(ctx context.Context, key crypto.Signer, accountUri string) error {
	return nil
}

func (self *t3RouteCa) KeyChange(ctx context.Context, accountUri string, oldKey crypto.Signer, newKey crypto.Signer) error {
	return errors.New("the surrogate changes no key")
}

func (self *t3RouteCa) Deactivate(ctx context.Context, key crypto.Signer, accountUri string) error {
	return errors.New("the surrogate deactivates no account")
}

func (self *t3RouteCa) NewOrder(ctx context.Context, key crypto.Signer, accountUri string, name string, profile string, replacesLeaf []byte) (*sidecar.TlsAcmeOrder, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if name != self.world.host || accountUri != t3RouteAccountUri {
		return nil, &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemRejectedIdentifier, Status: http.StatusBadRequest}
	}
	self.newOrders += 1
	prefix := fmt.Sprintf("https://ca.t3route.example/acme/%d", self.newOrders)
	authorization := &sidecar.TlsAcmeAuthorization{
		Url:    prefix + "/authz",
		Status: sidecar.TlsAcmeStatusPending,
		Challenges: []*sidecar.TlsAcmeChallenge{
			{Type: sidecar.TlsAcmeChallengeTlsAlpn, Url: prefix + "/chall/tls-alpn-01", Token: token, Status: sidecar.TlsAcmeStatusPending},
			{Type: sidecar.TlsAcmeChallengeDns, Url: prefix + "/chall/dns-01", Token: token, Status: sidecar.TlsAcmeStatusPending},
		},
	}
	order := &sidecar.TlsAcmeOrder{
		Url:            prefix + "/order",
		Status:         sidecar.TlsAcmeStatusPending,
		Authorizations: []string{authorization.Url},
		Finalize:       prefix + "/finalize",
	}
	self.orders[order.Url] = order
	self.authorizations[authorization.Url] = authorization
	self.authorizationOrders[authorization.Url] = order.Url
	for _, challenge := range authorization.Challenges {
		self.challengeAuthorizations[challenge.Url] = authorization.Url
	}
	orderCopy := *order
	return &orderCopy, nil
}

func (self *t3RouteCa) Order(ctx context.Context, key crypto.Signer, accountUri string, orderUrl string) (*sidecar.TlsAcmeOrder, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	order, ok := self.orders[orderUrl]
	if !ok {
		return nil, sidecar.ErrTlsAcmeGone
	}
	orderCopy := *order
	return &orderCopy, nil
}

func (self *t3RouteCa) Authorization(ctx context.Context, key crypto.Signer, accountUri string, url string) (*sidecar.TlsAcmeAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	authorization, ok := self.authorizations[url]
	if !ok {
		return nil, sidecar.ErrTlsAcmeGone
	}
	authorizationCopy := *authorization
	authorizationCopy.Challenges = nil
	for _, challenge := range authorization.Challenges {
		challengeCopy := *challenge
		authorizationCopy.Challenges = append(authorizationCopy.Challenges, &challengeCopy)
	}
	return &authorizationCopy, nil
}

// RFC 8737's validation of a tls-alpn-01 challenge: a handshake to each advertised address offering only acme-tls/1,
// each answer a certificate for the host carrying SHA-256 of the account's key authorization. Any other challenge the
// surrogate does not validate.
func (self *t3RouteCa) Challenge(ctx context.Context, key crypto.Signer, accountUri string, url string) error {
	token, addresses, err := func() (string, []string, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		authorization := self.authorizations[self.challengeAuthorizations[url]]
		if authorization == nil {
			return "", nil, sidecar.ErrTlsAcmeGone
		}
		for _, challenge := range authorization.Challenges {
			if challenge.Url == url && challenge.Type == sidecar.TlsAcmeChallengeTlsAlpn {
				return challenge.Token, slices.Clone(self.addresses), nil
			}
		}
		return "", nil, errors.New("the surrogate validates tls-alpn-01 alone")
	}()
	if err != nil {
		return err
	}
	keyAuthorization, err := self.KeyAuthorization(key, token)
	if err != nil {
		return err
	}
	validations := []*t3RouteValidation{}
	valid := 0 < len(addresses)
	for _, address := range addresses {
		leaf, err := t3RouteAcmeAnswer(address, self.world.host, t3EdgeGuardTimeout)
		if err == nil {
			if digest, ok := t3RouteAcmeIdentifier(leaf); !ok || !bytes.Equal(digest, t3RouteKeyAuthorizationDigest(keyAuthorization)) {
				err = errors.New("a challenge certificate for another key authorization")
			}
		}
		validations = append(validations, &t3RouteValidation{address: address, err: err})
		valid = valid && err == nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.validations = append(self.validations, validations...)
	authorizationUrl := self.challengeAuthorizations[url]
	authorization := self.authorizations[authorizationUrl]
	order := self.orders[self.authorizationOrders[authorizationUrl]]
	status := sidecar.TlsAcmeStatusInvalid
	if valid {
		status = sidecar.TlsAcmeStatusValid
	}
	for _, challenge := range authorization.Challenges {
		if challenge.Url == url {
			challenge.Status = status
		}
	}
	authorization.Status = status
	if valid {
		order.Status = sidecar.TlsAcmeStatusReady
	} else {
		authorization.ProblemType = sidecar.TlsAcmeProblemUnauthorized
		order.Status = sidecar.TlsAcmeStatusInvalid
	}
	return nil
}

func (self *t3RouteCa) Finalize(ctx context.Context, key crypto.Signer, accountUri string, order *sidecar.TlsAcmeOrder, csr []byte) (*sidecar.TlsAcmeOrder, error) {
	request, err := x509.ParseCertificateRequest(csr)
	if err != nil || request.CheckSignature() != nil || !slices.Equal(request.DNSNames, []string{self.world.host}) {
		return nil, &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemBadCsr, Status: http.StatusBadRequest}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	stored, ok := self.orders[order.Url]
	if !ok {
		return nil, sidecar.ErrTlsAcmeGone
	}
	if stored.Status != sidecar.TlsAcmeStatusReady {
		return nil, &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemOrderNotReady, Status: http.StatusForbidden}
	}
	stored.Status = sidecar.TlsAcmeStatusValid
	stored.Certificate = strings.TrimSuffix(stored.Url, "/order") + "/cert"
	self.chains[stored.Certificate] = self.issue(request.PublicKey, self.world.clock.Now().Add(45*24*time.Hour))
	storedCopy := *stored
	return &storedCopy, nil
}

func (self *t3RouteCa) Certificate(ctx context.Context, key crypto.Signer, accountUri string, url string) ([][]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	chain, ok := self.chains[url]
	if !ok {
		return nil, sidecar.ErrTlsAcmeGone
	}
	return slices.Clone(chain), nil
}

func (self *t3RouteCa) RenewalInfo(ctx context.Context, leaf []byte) (time.Time, time.Time, error) {
	certificate, err := x509.ParseCertificate(leaf)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return certificate.NotAfter.Add(-15 * 24 * time.Hour), certificate.NotAfter.Add(-14 * 24 * time.Hour), nil
}

func (self *t3RouteCa) Revoke(ctx context.Context, key crypto.Signer, accountUri string, leaf []byte, reason int) error {
	return nil
}

func (self *t3RouteCa) DeactivateAuthorization(ctx context.Context, key crypto.Signer, accountUri string, url string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if authorization, ok := self.authorizations[url]; ok {
		authorization.Status = sidecar.TlsAcmeStatusDeactivated
		if order, ok := self.orders[self.authorizationOrders[url]]; ok && order.Status != sidecar.TlsAcmeStatusValid {
			order.Status = sidecar.TlsAcmeStatusInvalid
		}
	}
	return nil
}

// RFC 8555 §8.1: the token, a dot, and the account key's RFC 7638 thumbprint in base64url.
func (self *t3RouteCa) KeyAuthorization(key crypto.Signer, token string) (string, error) {
	publicKey, ok := key.Public().(*ecdsa.PublicKey)
	if !ok {
		return "", errors.New("an account key that is not P-256")
	}
	return token + "." + base64.RawURLEncoding.EncodeToString(sidecar.TlsCustomKeyThumbprint(publicKey)), nil
}

// TLS-M6-05's custom routes as an ordering desktop calls them, here TLS-M6-05's store under the caller the routes would
// build. POST /tls/custom-challenge, which writes a delegated DNS-01 value through certs and DNS, is no route on this
// stack and answers 404: a refusal no retry under the claim changes, so the desktop releases its claim, its order
// recorded, where it sends a value again under the claim when no answer came or the server failed (owner decision
// 680). Each value asked for is kept.
type t3RouteCustomApi struct {
	world  *t3RouteWorld
	caller *model.TlsCustomCaller

	stateLock  sync.Mutex
	challenges []*sidecar.TlsCustomChallengeArgs
}

var _ sidecar.TlsCustomApi = (*t3RouteCustomApi)(nil)

func (self *t3RouteCustomApi) Work(ctx context.Context) (*sidecar.TlsCertsResult, error) {
	return self.world.custom.Work(ctx, self.caller)
}

func (self *t3RouteCustomApi) Claim(ctx context.Context, args *sidecar.TlsCustomClaimArgs) (*sidecar.TlsCustomClaimResult, error) {
	return self.world.custom.Claim(ctx, self.caller, args)
}

func (self *t3RouteCustomApi) Progress(ctx context.Context, args *sidecar.TlsCustomProgressArgs) (*sidecar.TlsCustomClaimResult, error) {
	return self.world.custom.Progress(ctx, self.caller, args)
}

func (self *t3RouteCustomApi) Register(ctx context.Context, args *sidecar.TlsCertRegisterArgs) error {
	return self.world.custom.Register(ctx, self.caller, args)
}

func (self *t3RouteCustomApi) Account(ctx context.Context, args *sidecar.TlsCustomAccountArgs) (*sidecar.TlsCustomAccount, error) {
	return self.world.custom.AccountOp(ctx, self.caller, args)
}

func (self *t3RouteCustomApi) Challenge(ctx context.Context, args *sidecar.TlsCustomChallengeArgs) (*sidecar.TlsCustomChallengeResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	argsCopy := *args
	self.challenges = append(self.challenges, &argsCopy)
	return nil, &sidecar.SidecarApiError{Route: "POST /tls/custom-challenge", Status: http.StatusNotFound}
}

func (self *t3RouteCustomApi) ChallengeStatus(ctx context.Context, challengeId uint64) (*sidecar.TlsCustomChallengeResult, error) {
	return nil, &sidecar.SidecarApiError{Route: "GET /tls/custom-challenge/<id>", Status: http.StatusNotFound}
}

// The DNS-01 values a desktop asked to have written.
func (self *t3RouteCustomApi) challengesAsked() []*sidecar.TlsCustomChallengeArgs {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.challenges)
}

// A desktop's verified view of the network's devices: the admission world's identity keys, each holding the publish
// grant on web, and whether this desktop holds port 443's on it.
type t3RouteDevices struct {
	world     *t3RouteWorld
	sidecarId sidecar.Id
	holds443  bool
}

var _ sidecar.TlsCustomDevices = (*t3RouteDevices)(nil)

func (self *t3RouteDevices) Device(sidecarId sidecar.Id, ownerLabel string) ([]byte, bool) {
	for _, device := range self.world.devices {
		if device.SidecarId == sidecarId {
			return device.PublicKey(), ownerLabel == t3RouteOwner
		}
	}
	return nil, false
}

func (self *t3RouteDevices) Holds443(ownerLabel string) bool {
	return self.holds443 && ownerLabel == t3RouteOwner
}

func (self *t3RouteDevices) SidecarId() sidecar.Id {
	return self.sidecarId
}

func (self *t3RouteDevices) NetworkId() sidecar.Id {
	return self.world.networkId
}

// The account's key file as every desktop reads it from the secret repo.
type t3RouteKeys struct {
	file []byte
}

var _ sidecar.TlsCustomKeyStore = (*t3RouteKeys)(nil)

func (self *t3RouteKeys) ReadKeyFile(ctx context.Context, repoId sidecar.Id, domain string) ([]byte, error) {
	return slices.Clone(self.file), nil
}

func (self *t3RouteKeys) WriteKeyFile(ctx context.Context, repoId sidecar.Id, domain string, data []byte) error {
	return errors.New("the T3 desktops change no key")
}

func (self *t3RouteKeys) LocalKeyFile(domain string) []byte {
	return nil
}

func (self *t3RouteKeys) SetLocalKeyFile(domain string, data []byte) error {
	return nil
}

// Public DNS as a desktop's system resolver answers it: the CNAMEs the customer published.
type t3RouteResolver struct {
	cnames map[string]string
}

var _ sidecar.TlsCustomResolver = (*t3RouteResolver)(nil)

func (self *t3RouteResolver) LookupCname(ctx context.Context, name string) (string, error) {
	if target, ok := self.cnames[name]; ok {
		return target, nil
	}
	return "", errors.New("no such name")
}

func (self *t3RouteResolver) LookupTxt(ctx context.Context, name string) ([]string, error) {
	return nil, errors.New("no such name")
}

// A desktop's TLS-ALPN-01 path through its own front (17.2.9): the challenge set on the front under the claim, whose
// registration lists it as an ACME name at once; the probe dialing the host at the address public DNS answers, one lb
// interface's.
type t3RouteAlpn struct {
	listener *t3RouteListener
	address  string
}

var _ sidecar.TlsCustomAlpnPath = (*t3RouteAlpn)(nil)

func (self *t3RouteAlpn) Serve(ctx context.Context, host string, ownerLabel string, keyAuthorization string, expireTime time.Time, claimSeq uint64) error {
	if ownerLabel != t3RouteOwner {
		return errors.New("a challenge on another owner")
	}
	if err := self.listener.front.SetCustomAlpnChallenge(host, keyAuthorization, expireTime, claimSeq); err != nil {
		return err
	}
	return self.listener.awaitPageCtx(ctx, func(update *sidecar.TlsFrontUpdate) bool {
		return t3RouteListsAcme(update, host, claimSeq)
	})
}

func (self *t3RouteAlpn) Clear(host string) {
	self.listener.front.ClearAlpnChallenge(host)
}

func (self *t3RouteAlpn) Probe(ctx context.Context, host string, keyAuthorization string, timeout time.Duration) error {
	leaf, err := t3RouteAcmeAnswer(self.address, host, timeout)
	if err != nil {
		return err
	}
	if digest, ok := t3RouteAcmeIdentifier(leaf); !ok || !bytes.Equal(digest, t3RouteKeyAuthorizationDigest(keyAuthorization)) {
		return errors.New("the probe read another desktop's challenge")
	}
	return nil
}

// An ordering desktop's orderer on device: TLS-M6-05's store, the surrogate CA, resolver, the account's key file and,
// when alpn is given, its TLS-ALPN-01 path, which also says it holds port 443's grant on web. Its waits are short and
// on real time, since the surrogate answers each step at once.
func (self *t3RouteWorld) orderer(device *sidecartest.AdmissionDevice, resolver *t3RouteResolver, alpn *t3RouteAlpn) (*sidecar.TlsCustomDomainOrderer, *t3RouteCustomApi) {
	t := self.t
	t.Helper()
	keyDer, err := x509.MarshalPKCS8PrivateKey(self.accountKey)
	if err != nil {
		t.Fatal(err)
	}
	keyFile, err := (&sidecar.TlsCustomKeyFile{
		Format:     1,
		Domain:     self.domain,
		Ca:         certsservice.CertsCustomCaLetsEncrypt,
		AccountUri: t3RouteAccountUri,
		KeySeq:     1,
		Key:        keyDer,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	api := &t3RouteCustomApi{world: self, caller: self.caller(device, false)}
	deps := &sidecar.TlsCustomDomainOrdererDeps{
		Api:      api,
		Acme:     self.ca,
		Resolver: resolver,
		Devices:  &t3RouteDevices{world: self, sidecarId: device.SidecarId, holds443: alpn != nil},
		Keys:     &t3RouteKeys{file: keyFile},
		Roots:    []string{self.root},
		Clock:    sidecar.RealClock{},
	}
	if alpn != nil {
		deps.Alpn = alpn
	}
	settings := sidecar.DefaultTlsCustomSettings()
	settings.OrderPollInterval = 10 * time.Millisecond
	orderer, err := sidecar.NewTlsCustomDomainOrderer(deps, settings)
	if err != nil {
		t.Fatal(err)
	}
	return orderer, api
}

// A custom row's state on Postgres.
func (self *t3RouteWorld) rowState(certId sidecar.Id) string {
	self.t.Helper()
	var state string
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT state FROM network_tls_cert WHERE cert_id = $1`, server.Id(certId)).Scan(&state))
	})
	return state
}

// FS-122's TLS-ALPN-01 clause on T3: with the custom host pending, one of two lb interfaces stopped, an ordering
// desktop with a TLS-ALPN-01 path starts no order, its work list reading the host not ready, while a desktop whose
// customer delegated _acme-challenge to the account's acme name starts a DNS-01 one, since DNS-01 orders never wait;
// once the stopped interface loads the version and passes its probes the host turns ready, and the first desktop's
// TLS-ALPN-01 order, resuming the order the DNS-01 desktop recorded, completes: its probe and a CA surrogate forced to
// each lb interface's address reach its front's answer through the real splitter, tlsedge and the sockets engine's
// ACME pick, and its chain is registered custom_issued.
func TestT3Fs122TlsAlpnOrdersWaitForTheHostToBeReady(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouteWorld(t)
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		api := newT3SplitApi(t, world.t3EdgeWorld, interfaces)
		version := t3RouteMapVersion(t, world.t3EdgeWorld)
		up := world.startSplit(api, interfaces[0])
		down := newT3SplitOn(t, world.t3EdgeWorld, api, interfaces[1], nil)
		api.probeRound(t, world.t3EdgeWorld, []*t3Split{up})
		if row := t3SplitReadiness(t, world.t3EdgeWorld, world.host); row.ReadyTime != nil || !slices.Equal(row.PendingInterfaces, []string{interfaces[1]}) {
			t.Fatalf("the host with one interface down: %+v", row)
		}
		world.ca.setAddresses(up.address(), down.address())

		// a device's queued order for the host, and two desktops
		_, certId := world.queueOrder(world.devices[2])
		desktop := world.addListener(0, newT3RouteCustom(&sidecar.TlsFrontCustomCertificate{Name: world.host}))
		alpnOrderer, _ := world.orderer(desktop.device, &t3RouteResolver{}, &t3RouteAlpn{listener: desktop, address: up.address()})
		dnsOrderer, dnsApi := world.orderer(world.devices[1], &t3RouteResolver{cnames: map[string]string{"_acme-challenge." + world.host: world.acmeName + "."}}, nil)

		// pending: the TLS-ALPN-01 desktop starts no order
		report, err := alpnOrderer.RunOnce(world.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !report.WorkSeen || 0 < len(report.Worked) || report.NewOrders != 0 || world.ca.newOrderCount() != 0 {
			t.Fatalf("the TLS-ALPN-01 desktop with the host pending: %+v, %d at the CA", report, world.ca.newOrderCount())
		}
		if state := world.rowState(certId); state != string(sidecar.TlsCertStateCustomQueued) {
			t.Fatalf("the row is %s", state)
		}
		// the DNS-01 desktop does not wait: its newOrder goes, and it asks for its value at the delegated name
		report, err = dnsOrderer.RunOnce(world.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if report.NewOrders != 1 || world.ca.newOrderCount() != 1 {
			t.Fatalf("the DNS-01 desktop with the host pending: %+v, %d at the CA", report, world.ca.newOrderCount())
		}
		if asked := dnsApi.challengesAsked(); len(asked) != 1 || asked[0].Domain != world.domain || len(asked[0].Value) != 43 {
			t.Fatalf("the DNS-01 desktop's values: %+v", asked)
		}

		// the stopped lb starts on the current version and passes its probes: the host turns ready
		down.startLb(version)
		api.probeRound(t, world.t3EdgeWorld, []*t3Split{up, down})
		t3SplitAwaitReady(t, world.t3EdgeWorld, world.host)

		// ready: the TLS-ALPN-01 desktop resumes the recorded order and completes it, every lb interface's address
		// reaching its front's answer
		report, err = alpnOrderer.RunOnce(world.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(report.Issued, []sidecar.Id{certId}) || report.NewOrders != 0 {
			t.Fatalf("the TLS-ALPN-01 desktop with the host ready: %+v", report)
		}
		validations := world.ca.validationsMade()
		if len(validations) != 2 {
			t.Fatalf("the surrogate made %d validations", len(validations))
		}
		for index, address := range []string{up.address(), down.address()} {
			if validations[index].address != address || validations[index].err != nil {
				t.Fatalf("the validation at %s: %v", validations[index].address, validations[index].err)
			}
		}
		if state := world.rowState(certId); state != string(sidecar.TlsCertStateCustomIssued) {
			t.Fatalf("the row is %s after the order", state)
		}
		if accepted := desktop.accepted.Load(); accepted < 2+int64(sidecar.DefaultTlsCustomSettings().ProbeCount) {
			t.Fatalf("the desktop's front took %d acme-tls/1 streams", accepted)
		}
	})
}

// FS-41's custom-host step on T3: sockets keep nothing durable, so once the stack's Redis loses every key, as a site
// loss leaves it, a custom host's listeners come back by their next heartbeat with no other action, each certified
// name narrowed against TLS-M6-05's rows again: before the loss and after the heartbeats a visitor's hello for the host
// reaches only the certified listener; in between Redis holds no lease of web.
func TestT3Fs41CustomHostListenersComeBackByTheirNextHeartbeat(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouteWorld(t)
		split := world.startSplit(newT3SplitApi(t, world.t3EdgeWorld, []string{t3SplitInterface}), t3SplitInterface)
		notAfter := world.clock.Now().Add(30 * 24 * time.Hour)
		certificate := world.certify(world.devices[0], notAfter)
		world.loadFacts()
		certified := world.addListener(0, newT3RouteCustom(&sidecar.TlsFrontCustomCertificate{Name: world.host, Certificate: certificate, NotAfter: notAfter}))
		plain := world.addListener(1, newT3RouteCustom(&sidecar.TlsFrontCustomCertificate{Name: world.host}))
		certified.awaitPage(func(update *sidecar.TlsFrontUpdate) bool { return slices.Contains(update.CertifiedNames, world.host) })
		visit := func(when string) {
			t.Helper()
			for range 50 {
				leaf, err := world.visitCustom(split)
				if err != nil {
					t.Fatalf("%s, a visitor's hello for the custom host: %v", when, err)
				}
				if !bytes.Equal(leaf.Raw, certificate.Certificate[0]) {
					t.Fatalf("%s, a visitor's hello reached another certificate than the certified listener's", when)
				}
			}
			if accepted := plain.accepted.Load(); accepted != 0 {
				t.Fatalf("%s, the uncertified listener took %d visitor streams", when, accepted)
			}
		}
		visit("before the loss")

		// the loss: the test's own database on the stack's Redis emptied
		server.Redis(world.ctx, func(client server.RedisClient) {
			if err := client.FlushDB(world.ctx).Err(); err != nil {
				t.Fatal(err)
			}
		})
		if leases := world.storedLeases(); len(leases) != 0 {
			t.Fatalf("Redis holds %d leases of web after the loss", len(leases))
		}

		// each listener's next heartbeat, the registrar's clock moved on as a restarted one's would be
		world.clock.Advance(15 * time.Second)
		certified.heartbeat()
		plain.heartbeat()
		leases := world.storedLeases()
		if lease := leases[certified.clientId]; lease == nil || !slices.Equal(lease.CertifiedNames, []string{world.host}) {
			t.Fatalf("the certified listener's lease after its heartbeat: %+v", lease)
		}
		if lease := leases[plain.clientId]; lease == nil || 0 < len(lease.CertifiedNames) {
			t.Fatalf("the uncertified listener's lease after its heartbeat: %+v", lease)
		}
		visit("after the heartbeats")
	})
}

// TLS-M6-02's scope hosts on T3, through the real engine: a name one label below a label of the network's zone,
// <host>.<scope>.<label>.<root>, maps to the scope host tls/443/<host>.<scope> and reaches Find, which denies it, since
// tlsedge holds no scope table and nothing gives the gate a scope source until §7.9's rows carry TeamScopeRow, the
// closed direction for every scope, an unknown one's among them (the denial's cause is not told apart here: no grant
// names a scope host yet either). tlsedge answers unrecognized_name with no certificate, and its negative cache holds
// the denial, so the same hello again makes no Find.
func TestT3AScopeHostIsDeniedAtFindAndHeldByTheNegativeCache(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouteWorld(t)
		hello := t3EdgeHello(t, "blog.unknownscope."+world.label+"."+world.root, []string{"h2", "http/1.1"})
		dials := len(world.dial.Requests())
		if answer := world.answer(hello); !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName)) {
			t.Fatalf("a scope host answered %x", answer)
		}
		requests := world.dial.Requests()[dials:]
		if len(requests) != 1 || requests[0].Name != "tls/443/blog.unknownscope" || requests[0].ServerName != "" {
			t.Fatalf("the scope host's dials: %+v", requests)
		}
		if answer := world.answer(hello); !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName)) {
			t.Fatalf("a scope host answered %x the second time", answer)
		}
		if after := len(world.dial.Requests()); after != dials+1 {
			t.Fatalf("the negative cache let %d more Finds through", after-dials-1)
		}
	})
}
