// The measured mid-session workload uses canonical production through public
// APIs. This test-only fixture adds no production bridges or scheduling policy.
package perfvar

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// One registered provider owns its real H1 carrier, NAT readers, return retry
// budget and lifecycle trackers. It is never an alias for another provider.
type privateSdkBrowsingProvider struct {
	lifecycle        atomic.Int32
	carrier          *clientconnect.Tun
	strategy         *clientconnect.ClientStrategy
	client           *clientconnect.Client
	id               clientconnect.Id
	transport        *clientconnect.PlatformTransport
	localNat         *clientconnect.LocalUserNat
	remoteNat        *clientconnect.RemoteUserNatProvider
	resendBudget     *clientconnect.TransferMemoryBudget
	noAck            *noAckSendTracker
	packs            *sendPackLifecycleTracker
	returns          *providerReturnSendTracker
	provideAcked     bool
	ownedDials       atomic.Uint64
	ownedReadBytes   atomic.Uint64
	ownedWriteBytes  atomic.Uint64
	ownedReturnBytes atomic.Uint64
	httpWrittenBytes atomic.Uint64
}

// Only a completed constructor publishes pointers to concurrent diagnostics.
// The absent state is expected solely for the deliberately late fifth peer.
const (
	privateSdkBrowsingProviderAbsent int32 = iota
	privateSdkBrowsingProviderPartial
	privateSdkBrowsingProviderReady
)

// The public SDK owns all generated client settings and shares one 32-MiB
// owner's admission hierarchy. Co-resident providers are outside that owner;
// this screen cannot qualify the host Go runtime at a mobile memory ceiling.
type privateSdkBrowsingFixture struct {
	productionReadiness   *privateSdkProductionReadinessMode
	defaultContracts      *privateSdkDefaultContractMode
	t                     testing.TB
	ctx                   context.Context
	setupCtx              context.Context
	cancelSetup           context.CancelFunc
	cancel                context.CancelFunc
	environment           *routeEnvironment
	providerNetworkId     server.Id
	providerUserId        server.Id
	providers             [6]privateSdkBrowsingProvider
	deviceCarrier         *clientconnect.Tun
	deviceStrategy        *clientconnect.ClientStrategy
	space                 *sdk.NetworkSpace
	device                *sdk.DeviceLocal
	serverTls             *tls.Config
	clientTls             *tls.Config
	path                  *fullTunPath
	origins               privateSdkBrowsingOrigins
	bridgeWaitGroup       sync.WaitGroup
	bridgeDone            chan struct{}
	bridgeRunning         bool
	bridgeStarted         atomic.Uint64
	bridgeCompleted       atomic.Uint64
	bridgeAccepted        atomic.Uint64
	bridgeRefused         atomic.Uint64
	bridgeMaximum         atomic.Int64
	bridgeInvalid         atomic.Bool
	closeOnce             sync.Once
	closeErr              error
	report                privateSdkBrowsingReport
	providerBaseline      [6]*privateSdkBrowsingProviderReport
	providerBaselineScope [6]string
	originProviderMasks   [nativeBrowsingOriginCount]atomic.Uint32
}

// Only the four listeners owned by the unchanged workload are exceptions to
// the normal public endpoint policy. Probe destinations retain normal policy.
type privateSdkBrowsingOrigins struct {
	stateLock        sync.Mutex
	midSession       *privateSdkMidSession
	ports            [nativeBrowsingOriginCount + 1]int
	count            int
	providerBySource map[string]int
}

// The listener callback installs only the exact already-bound TCP endpoint.
func (self *privateSdkBrowsingOrigins) add(listener net.Listener) {
	address := listener.Addr().(*net.TCPAddr)
	if !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		panic("browsing origin is not the owned loopback listener")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	limit := nativeBrowsingOriginCount
	if self.midSession != nil && self.midSession.cold {
		limit++
	}
	if limit <= self.count {
		panic("browsing origin ownership exceeded four listeners")
	}
	self.ports[self.count] = address.Port
	self.count++
}

// A port belongs to this exception only while it names an owned origin.
func (self *privateSdkBrowsingOrigins) matches(ip net.IP, port int, ipProtocol clientconnect.IpProtocol) bool {
	if ipProtocol != clientconnect.IpProtocolTcp || port != 443 && port != 8443 {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for index := 0; index < self.count; index++ {
		if 0 < self.ports[index] && ip.Equal(net.IPv4(203, 0, 113, byte(10+index))) && (port == 443 || index == 0) {
			return true
		}
	}
	return false
}

// Four documentation addresses are logical fixture destinations, never host
// routes. Their actual application packets carry port443 through the SDK.
func (self *privateSdkBrowsingOrigins) destination(index int, _ net.Listener) string {
	return net.JoinHostPort(net.IPv4(203, 0, 113, byte(10+index)).String(), "443")
}

// Closing the actual servers removes their exceptions; live NAT connections
// still have independent mapping entries until their exact Close disposition.
func (self *privateSdkBrowsingOrigins) clear() {
	self.stateLock.Lock()
	self.ports = [nativeBrowsingOriginCount + 1]int{}
	self.count = 0
	self.stateLock.Unlock()
}

// One mapped upstream socket retains its provider attribution until Close.
// Byte counters are actual TLS socket bytes, not HTTP body-delivery evidence.
type privateSdkBrowsingConn struct {
	net.Conn
	provider  *privateSdkBrowsingProvider
	origins   *privateSdkBrowsingOrigins
	source    string
	closeOnce sync.Once
	closeErr  error
}

// Reads count actual bytes returned by the owned TLS origin socket.
func (self *privateSdkBrowsingConn) Read(buffer []byte) (int, error) {
	n, err := self.Conn.Read(buffer)
	self.provider.ownedReadBytes.Add(uint64(n))
	if self.origins.midSession != nil {
		self.origins.midSession.observeRead(self.source, n)
	}
	return n, err
}

// Writes count actual client-to-origin TLS bytes accepted by that socket.
func (self *privateSdkBrowsingConn) Write(buffer []byte) (int, error) {
	n, err := self.Conn.Write(buffer)
	self.provider.ownedWriteBytes.Add(uint64(n))
	return n, err
}

// Exact connection ownership, not a TTL, releases the source-port mapping.
func (self *privateSdkBrowsingConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.Conn.Close()
		self.origins.stateLock.Lock()
		delete(self.origins.providerBySource, self.source)
		self.origins.stateLock.Unlock()
	})
	return self.closeErr
}

// Only the fixture documentation tuples translate to owned loopback listeners.
// Ordinary public probe destinations still use the production socket dialer;
// no answer, TLS result, qualification state or site content is synthesized.
func (self *privateSdkBrowsingFixture) dialProvider(index int, ctx context.Context, network, address string) (net.Conn, error) {
	if self.origins.midSession != nil && ctx.Err() == nil && self.origins.midSession.rejectDial(index, address) {
		return nil, errors.New("synthetic scoped mid-session origin refusal")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, err
	}
	prefix := netip.MustParsePrefix("203.0.113.0/24")
	if !prefix.Contains(ip) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	self.origins.stateLock.Lock()
	destination := ""
	for origin := 0; origin < self.origins.count; origin++ {
		if ip == netip.AddrFrom4([4]byte{203, 0, 113, byte(10 + origin)}) && (port == "443" || port == "8443" && origin == 0) {
			destination = net.JoinHostPort("127.0.0.1", fmt.Sprint(self.origins.ports[origin]))
			break
		}
	}
	self.origins.stateLock.Unlock()
	if destination == "" || network != "tcp" && network != "tcp4" {
		return nil, errors.New("unowned documentation fixture endpoint")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", destination)
	if err != nil {
		return nil, err
	}
	source := connection.LocalAddr().String()
	self.origins.stateLock.Lock()
	if self.origins.providerBySource == nil {
		self.origins.providerBySource = map[string]int{}
	}
	if _, exists := self.origins.providerBySource[source]; exists {
		self.origins.stateLock.Unlock()
		connection.Close()
		return nil, errors.New("live upstream socket attribution collided")
	}
	self.origins.providerBySource[source] = index
	self.origins.stateLock.Unlock()
	provider := &self.providers[index]
	provider.ownedDials.Add(1)
	return &privateSdkBrowsingConn{Conn: connection, provider: provider, origins: &self.origins, source: source}, nil
}

// Actual successful server writes are attributed before the request can be
// canceled. These are per-attempt HTTP body bytes, not unique received bytes.
type privateSdkBrowsingWriter struct {
	http.ResponseWriter
	provider *privateSdkBrowsingProvider
}

// A partial write counts only the real writer's accepted prefix.
func (self *privateSdkBrowsingWriter) Write(body []byte) (int, error) {
	n, err := self.ResponseWriter.Write(body)
	self.provider.httpWrittenBytes.Add(uint64(n))
	return n, err
}

// Streaming and cancellation use the same underlying server flush boundary.
func (self *privateSdkBrowsingWriter) Flush() {
	if flusher, ok := self.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Synthetic readiness requests are deliberately excluded from useful-body
// accounting. Unknown ownership is a test failure, never assigned to a peer.
func (self *privateSdkBrowsingFixture) wrapOriginHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/page/") && !strings.HasPrefix(request.URL.Path, "/asset/") {
			handler.ServeHTTP(writer, request)
			return
		}
		self.origins.stateLock.Lock()
		index, ok := self.origins.providerBySource[request.RemoteAddr]
		self.origins.stateLock.Unlock()
		if !ok {
			self.t.Error("actual browsing HTTP request lost its upstream provider ownership")
			http.Error(writer, "unowned fixture request", http.StatusServiceUnavailable)
			return
		}
		for origin := range self.originProviderMasks {
			if request.Host == fmt.Sprintf("origin%d.test", origin) {
				self.originProviderMasks[origin].Or(uint32(1) << index)
			}
		}
		if self.productionReadiness != nil {
			self.productionReadiness.observeRequest(self, index, request)
		}
		handler.ServeHTTP(&privateSdkBrowsingWriter{ResponseWriter: writer, provider: &self.providers[index]}, request)
	})
}

// Both client and provider policies wrap their ordinary direction-specific
// policies; no trusted-network mode or broad public-policy disable is used.
type privateSdkBrowsingOriginPolicy struct {
	clientconnect.SecurityPolicy
	origins  *privateSdkBrowsingOrigins
	provider bool
}

// Origin-bound packets alone bypass public loopback rejection.
func (self *privateSdkBrowsingOriginPolicy) InspectEgress(mode protocol.ProvideMode, path *clientconnect.IpPath, payload []byte) (clientconnect.SecurityPolicyResult, error) {
	if path != nil {
		ip, port := path.DestinationIp, path.DestinationPort
		if self.provider {
			ip, port = path.SourceIp, path.SourcePort
		}
		if self.origins.matches(ip, port, path.Protocol) {
			return clientconnect.SecurityPolicyResultAllow, nil
		}
	}
	return self.SecurityPolicy.InspectEgress(mode, path, payload)
}

// Reply-bound packets use the inverse endpoint of the same owned listener.
func (self *privateSdkBrowsingOriginPolicy) InspectIngress(mode protocol.ProvideMode, path *clientconnect.IpPath, payload []byte) (clientconnect.SecurityPolicyResult, error) {
	if path != nil {
		ip, port := path.SourceIp, path.SourcePort
		if self.provider {
			ip, port = path.DestinationIp, path.DestinationPort
		}
		if self.origins.matches(ip, port, path.Protocol) {
			return clientconnect.SecurityPolicyResultAllow, nil
		}
	}
	return self.SecurityPolicy.InspectIngress(mode, path, payload)
}

// Scalar evidence never emits JWTs, provider identities, destinations or
// payload. Queue snapshots are sampled, not continuous peak measurements.
type privateSdkBrowsingReport struct {
	InnerHttp                          *nativeBrowsingInnerHttpReport       `json:"inner_http,omitempty"`
	Scope                              string                               `json:"scope"`
	ReadyQuality                       int                                  `json:"ready_quality"`
	ReadySpeed                         int                                  `json:"ready_speed"`
	ReadyDistinctExits                 int                                  `json:"ready_distinct_exits"`
	ReadinessSnapshot                  *privateSdkBrowsingReadinessSnapshot `json:"ready_snapshot,omitempty"`
	ReadyProven                        int                                  `json:"ready_proven"`
	ExpectedReceiveCapacity            int64                                `json:"expected_receive_capacity_from_pinned_policy"`
	SampledReceivePeak                 int64                                `json:"sampled_receive_peak"`
	ReceiveCapacitySource              string                               `json:"receive_capacity_source"`
	ReceiveRefusalCountersAvailable    bool                                 `json:"receive_refusal_counters_available"`
	ReceivePressureClassification      string                               `json:"receive_pressure_classification"`
	Before                             sdk.DeviceLocalMemoryUsage           `json:"owner_before"`
	SdkJoined                          bool                                 `json:"sdk_joined"`
	AfterJoin                          *sdk.DeviceLocalMemoryUsage          `json:"owner_after_join,omitempty"`
	Providers                          [6]*privateSdkBrowsingProviderReport `json:"providers"`
	ProviderLifecycle                  [6]string                            `json:"provider_lifecycle"`
	ProviderBaselineScope              [6]string                            `json:"provider_baseline_scope"`
	ProviderBaselineKnown              [6]bool                              `json:"provider_baseline_known"`
	SdkBridgePackets                   uint64                               `json:"sdk_bridge_packets"`
	SdkBridgeAccepted                  uint64                               `json:"sdk_bridge_accepted"`
	SdkBridgeRefused                   uint64                               `json:"sdk_bridge_refused"`
	SdkBridgeMaximum                   int64                                `json:"sdk_bridge_maximum"`
	BrowsingBodyProviderCount          int                                  `json:"browsing_body_provider_count"`
	OriginProviderMasks                [nativeBrowsingOriginCount]uint32    `json:"origin_provider_masks"`
	UnavailableInheritedBoundaryFields []string                             `json:"unavailable_inherited_boundary_fields"`
	Browser                            *nativeBrowsingObservation           `json:"browser"`
}

// Provider indices name only synthetic fixture owners; they reveal no route
// identities. Source-written bodies are not called end-to-end delivered bytes.
type privateSdkBrowsingProviderReport struct {
	OwnedTcpDials             uint64 `json:"owned_tcp_dials"`
	OwnedTlsReadBytes         uint64 `json:"owned_tls_read_bytes"`
	OwnedTlsWriteBytes        uint64 `json:"owned_tls_write_bytes"`
	HttpBodyWrittenBytes      uint64 `json:"http_body_written_bytes"`
	ReturnStartedBytes        int64  `json:"return_started_bytes"`
	ReturnCompletedBytes      int64  `json:"return_completed_bytes"`
	ReturnFailedBytes         int64  `json:"return_failed_bytes"`
	OwnedReturnCompletedBytes uint64 `json:"owned_return_completed_bytes"`
}

// This bounded fixture discovery authority uses real named-provider API
// eligibility for two disjoint local candidate sets. It does not claim to
// exercise the platform's geographic/ARIN/URL-score ranking publication.
// The SDK's generator remains dynamic, owning real mint, evaluation and speed
// selection; no channel, readiness or qualification result is manufactured.
type privateSdkBrowsingDiscovery struct {
	providers *[6]privateSdkBrowsingProvider
	strategy  *clientconnect.ClientStrategy
	apiUrl    string
}

// Caller contexts, JWT authentication, server common gates, exclusions and
// native family filters remain effective. Only the synthetic fixture pool is
// substituted for global location discovery, never provider admission.
func (self *privateSdkBrowsingDiscovery) FindProviders2(ctx context.Context, token string, args *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
	if args == nil || token == "" {
		return nil, errors.New("fixture discovery lacks caller authority")
	}
	if args.IpFamily != clientconnect.IpFamilyFilterDefault && args.IpFamily != clientconnect.IpFamilyFilterV4Capable && args.IpFamily != clientconnect.IpFamilyFilterV4Only {
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
	}
	start, end := 0, 5
	if args.RankMode == "speed" {
		start, end = 5, 6
	} else if args.RankMode != "" && args.RankMode != "quality" {
		return nil, errors.New("unsupported local fixture rank mode")
	}
	query := *args
	query.Specs = nil
	for _, spec := range args.Specs {
		if spec == nil || spec.LocationId != nil || spec.LocationGroupId != nil || !spec.BestAvailable && spec.ClientId == nil {
			return nil, errors.New("fixture discovery received an unowned scope")
		}
		if spec.ClientId != nil {
			found := false
			for index := start; index < end; index++ {
				if self.providers[index].id == *spec.ClientId {
					found = true
				}
			}
			if !found {
				return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
			}
			query.Specs = append(query.Specs, &clientconnect.ProviderSpec{ClientId: spec.ClientId})
		}
	}
	if len(query.Specs) == 0 {
		for index := start; index < end; index++ {
			provider := &self.providers[index]
			if provider.provideAcked && provider.transport != nil && provider.transport.IsConnected() && provider.remoteNat != nil {
				query.Specs = append(query.Specs, &clientconnect.ProviderSpec{ClientId: &provider.id})
			}
		}
	}
	if len(query.Specs) == 0 || args.Count <= 0 {
		return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
	}
	api := clientconnect.NewBringYourApi(ctx, self.strategy, self.apiUrl)
	defer api.Close()
	api.SetByJwt(token)
	result, err := api.FindProviders2SyncWithCtx(ctx, &query)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("actual discovery API returned no result envelope")
	}
	for _, candidate := range result.Providers {
		owned := false
		for index := start; index < end; index++ {
			if candidate.ClientId == self.providers[index].id {
				owned = true
			}
		}
		if !owned {
			return nil, errors.New("actual discovery API returned an unowned fixture candidate")
		}
	}
	if args.Count < len(result.Providers) {
		result.Providers = result.Providers[:args.Count]
	}
	return result, nil
}

// A fresh private CA signs all four exact synthetic origin names. Both HTTPS
// and local qualification validate this root, hostname, TLS1.3 and H1 ALPN.
func privateSdkBrowsingTlsConfigs(extraOrigins ...int) (*tls.Config, *tls.Config, error) {
	extra := 0
	if len(extraOrigins) > 1 {
		return nil, nil, errors.New("invalid synthetic origin extent")
	}
	if len(extraOrigins) == 1 {
		extra = extraOrigins[0]
	}
	if extra < 0 || extra > 1 {
		return nil, nil, errors.New("invalid synthetic origin extent")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic browsing fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return nil, nil, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, nativeBrowsingOriginCount+extra)
	for index := range names {
		names[index] = fmt.Sprintf("origin%d.test", index)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	serverConfig := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certificateBytes, caBytes}, PrivateKey: serverKey}}, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}
	clientConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}
	return serverConfig, clientConfig, nil
}

// Warmup is finite and separate from the 100-page application deadline. HTTPS
// on443 takes the real quality race; a real TLS control on8443 takes speed.
// Every provider must have completed an actual owned upstream TCP dial and
// every public exit must be proven, or this is a setup-premise failure.
// One provider's absolute counters are sampled at warmup and after work; raw
// return bytes include headers/retries and are never called unique body bytes.
func (self *privateSdkBrowsingFixture) providerSample(index int) *privateSdkBrowsingProviderReport {
	provider := &self.providers[index]
	if provider.lifecycle.Load() != privateSdkBrowsingProviderReady || provider.client == nil || provider.carrier == nil || provider.remoteNat == nil || provider.returns == nil {
		return nil
	}
	return provider.sampleCounters()
}

// The constructor calls this only after tracker initialization, before any
// client can use it; all later calls require the ready publication above.
func (self *privateSdkBrowsingProvider) sampleCounters() *privateSdkBrowsingProviderReport {
	return &privateSdkBrowsingProviderReport{OwnedTcpDials: self.ownedDials.Load(), OwnedTlsReadBytes: self.ownedReadBytes.Load(), OwnedTlsWriteBytes: self.ownedWriteBytes.Load(), HttpBodyWrittenBytes: self.httpWrittenBytes.Load(), ReturnStartedBytes: self.returns.startedByteCount.Load(), ReturnCompletedBytes: self.returns.completedByteCount.Load(), ReturnFailedBytes: self.returns.failureByteCount.Load(), OwnedReturnCompletedBytes: self.ownedReturnBytes.Load()}
}

// A late constructor samples its initialized atomics before creating a client
// or making the peer eligible; setup absence is never a fabricated sample.
func (self *privateSdkBrowsingFixture) captureProviderBirthBaseline(index int) error {
	if self.providerBaselineScope[index] != "awaiting_late_birth" {
		return nil
	}
	provider := &self.providers[index]
	if provider.lifecycle.Load() != privateSdkBrowsingProviderPartial || provider.returns == nil {
		return fmt.Errorf("provider %d birth baseline lacks initialized tracker", index)
	}
	self.providerBaseline[index] = provider.sampleCounters()
	self.providerBaselineScope[index] = "birth_pre_use"
	return nil
}

// A physically absent late peer has no numeric sample. Its actual birth
// baseline is captured by the constructor before the first possible use.
func (self *privateSdkBrowsingFixture) captureProviderBaselines(lateAbsent bool) error {
	var result error
	for index := range self.providers {
		sample := self.providerSample(index)
		self.providerBaseline[index] = sample
		self.providerBaselineScope[index] = "unknown"
		if sample != nil {
			self.providerBaselineScope[index] = "pre_navigation"
		} else if lateAbsent && index == 4 && self.providers[index].lifecycle.Load() == privateSdkBrowsingProviderAbsent {
			self.providerBaselineScope[index] = "awaiting_late_birth"
		} else {
			result = errors.Join(result, fmt.Errorf("provider %d baseline unavailable: lifecycle=%d", index, self.providers[index].lifecycle.Load()))
		}
	}
	return result
}

// Unknown or partially constructed owners remain null in the report. A
// missing baseline or regressing counter cannot become a wrapped delta.
func (self *privateSdkBrowsingFixture) captureProviderReports(lateAbsent bool) error {
	var result error
	self.report.BrowsingBodyProviderCount = 0
	for index := range self.providers {
		provider := &self.providers[index]
		lifecycle := provider.lifecycle.Load()
		self.report.ProviderLifecycle[index] = "partial"
		if lifecycle == privateSdkBrowsingProviderAbsent {
			self.report.ProviderLifecycle[index] = "absent"
		} else if lifecycle == privateSdkBrowsingProviderReady {
			self.report.ProviderLifecycle[index] = "ready"
		}
		before, after := self.providerBaseline[index], self.providerSample(index)
		self.report.ProviderBaselineScope[index] = self.providerBaselineScope[index]
		self.report.ProviderBaselineKnown[index] = before != nil
		self.report.Providers[index] = nil
		if lateAbsent && index == 4 && lifecycle == privateSdkBrowsingProviderAbsent && self.providerBaselineScope[index] == "awaiting_late_birth" {
			continue
		}
		if before == nil || after == nil {
			result = errors.Join(result, fmt.Errorf("provider %d report unavailable: lifecycle=%d baseline_known=%t", index, lifecycle, before != nil))
			continue
		}
		if after.OwnedTcpDials < before.OwnedTcpDials || after.OwnedTlsReadBytes < before.OwnedTlsReadBytes || after.OwnedTlsWriteBytes < before.OwnedTlsWriteBytes || after.HttpBodyWrittenBytes < before.HttpBodyWrittenBytes || after.ReturnStartedBytes < before.ReturnStartedBytes || after.ReturnCompletedBytes < before.ReturnCompletedBytes || after.ReturnFailedBytes < before.ReturnFailedBytes || after.OwnedReturnCompletedBytes < before.OwnedReturnCompletedBytes {
			result = errors.Join(result, fmt.Errorf("provider %d counters regressed", index))
			continue
		}
		self.report.Providers[index] = &privateSdkBrowsingProviderReport{OwnedTcpDials: after.OwnedTcpDials - before.OwnedTcpDials, OwnedTlsReadBytes: after.OwnedTlsReadBytes - before.OwnedTlsReadBytes, OwnedTlsWriteBytes: after.OwnedTlsWriteBytes - before.OwnedTlsWriteBytes, HttpBodyWrittenBytes: after.HttpBodyWrittenBytes - before.HttpBodyWrittenBytes, ReturnStartedBytes: after.ReturnStartedBytes - before.ReturnStartedBytes, ReturnCompletedBytes: after.ReturnCompletedBytes - before.ReturnCompletedBytes, ReturnFailedBytes: after.ReturnFailedBytes - before.ReturnFailedBytes, OwnedReturnCompletedBytes: after.OwnedReturnCompletedBytes - before.OwnedReturnCompletedBytes}
		if self.report.Providers[index].HttpBodyWrittenBytes > 0 {
			self.report.BrowsingBodyProviderCount++
		}
	}
	return result
}

// Each SDK device owns a non-nil generated instance, like the mobile host.
// The public constructor treats this as required input, not an optional value.
func privateSdkBrowsingNewDevice(space *sdk.NetworkSpace, byJwt string, settings *sdk.DeviceLocalSettings) (*sdk.DeviceLocal, error) {
	return sdk.NewDeviceLocal(space, byJwt, "sdk browsing device", "synthetic sdk browsing device", "perfvar", sdk.NewId(), settings)
}

// Constructor failure must retire partial owners before TestEnv resets its
// database. testing.Cleanup is only the later idempotent fallback.
func (self *privateSdkBrowsingFixture) closeUnlessConstructed(constructed *bool) {
	if !*constructed {
		if err := self.close(); err != nil {
			self.t.Error(err)
		}
	}
}

// Sharing the consumer's network would select Network, not the Public mode
// these real providers advertise, and fall back to an origin-less reply.
func (self *privateSdkBrowsingFixture) createProviderNetwork() {
	self.providerNetworkId, self.providerUserId = server.NewId(), server.NewId()
	model.Testing_CreateNetwork(self.ctx, self.providerNetworkId, fmt.Sprintf("perfvar-provider-%s", self.providerNetworkId), self.providerUserId)
}

// Each actual provider repeats the existing full-TUN provider construction,
// including real TLS, authenticated control and the socket-owned TCP return.
func (self *privateSdkBrowsingFixture) buildProvider(index int, plan *nativeBrowsingPressurePlan) error {
	provider := &self.providers[index]
	provider.lifecycle.Store(privateSdkBrowsingProviderPartial)
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(self.ctx, self.providerNetworkId, deviceId, clientId, "sdk browsing provider", "synthetic sdk browsing provider")
	jwt := session.NewByJwt(self.providerNetworkId, self.providerUserId, fmt.Sprintf("perfvar-provider-%s", self.providerNetworkId), false, false).Client(deviceId, clientId).Testing_Sign()
	provider.id = clientconnect.Id(clientId)
	provider.carrier, provider.strategy = self.environment.newClientNodeWithProfileAt(false, plan.provider, self.environment.providerEdgeName)
	provider.noAck, provider.packs, provider.returns = newNoAckSendTracker(), newSendPackLifecycleTracker(), newProviderReturnSendTracker()
	if err := self.captureProviderBirthBaseline(index); err != nil {
		return err
	}
	settings := fullTunClientSettingsWithFeatures(fullTunRouteExchangeH1, &clientconnect.P2pDataPlaneStats{}, provider.noAck, provider.packs, plan.resources.LogicalDataLaneCount, plan.resources.Features)
	provider.resendBudget = clientconnect.NewTransferMemoryBudget(2 * 1024 * 1024)
	settings.SendBufferSettings.ResendQueueBudget = provider.resendBudget
	settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
	settings.WebRtcSettings.IceServerUrls = nil
	settings.WebRtcSettings.IceServerPoolUrls = nil
	settings.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
	apiUrl := fmt.Sprintf("http://%s:%d", self.environment.providerEdgeAddress, self.environment.providerApiPort)
	apiControl := clientconnect.NewApiOutOfBandControl(self.ctx, provider.strategy, jwt, apiUrl)
	var control clientconnect.OutOfBandControl = apiControl
	if self.defaultContracts != nil {
		privateSdkApplyProductionContractCredits(settings)
		control = self.defaultContracts.wrapControl(index, apiControl)
	}
	provider.client = clientconnect.NewClient(self.ctx, provider.id, control, settings)
	if self.defaultContracts != nil {
		self.defaultContracts.observeProvider(index, provider.client)
	}
	platformSettings := fullTunPlatformSettings(self.environment.providerH3Port, clientconnect.TransportModeH1, provider.carrier, &clientconnect.PlatformTransportReceiveStats{}, newFullTunEndpointPlatformBudget())
	provider.transport = clientconnect.NewPlatformTransportWithTargetMode(self.ctx, provider.strategy, provider.client.RouteManager(), fmt.Sprintf("wss://%s:%d", self.environment.providerEdgeAddress, self.environment.providerH1Port), &clientconnect.ClientAuth{ByJwt: jwt, InstanceId: clientconnect.NewId(), AppVersion: "perfvar"}, clientconnect.TransportModeH1, platformSettings)
	if !waitForPlatform(self.setupCtx, provider.transport) {
		return errors.New("local provider H1 platform never became connected")
	}
	localSettings := clientconnect.DefaultLocalUserNatSettings()
	localSettings.TcpBufferSettings.DialContextSettings = &clientconnect.DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return self.dialProvider(index, ctx, network, address)
	}}
	provider.localNat = clientconnect.NewLocalUserNat(provider.client.Ctx(), clientId.String(), localSettings)
	natSettings := clientconnect.DefaultRemoteUserNatProviderSettings()
	natSettings.ReturnSendObserver = func(observation clientconnect.RemoteUserNatProviderReturnSendObservation) {
		provider.returns.observe(observation)
		key := observation.FlowKey
		if observation.Phase == clientconnect.RemoteUserNatProviderReturnSendPhaseCompleted && observation.Sent && key.Valid && key.IpVersion == 4 && key.Protocol == clientconnect.IpProtocolTcp && key.SourceIp[0] == 203 && key.SourceIp[1] == 0 && key.SourceIp[2] == 113 && 10 <= key.SourceIp[3] && key.SourceIp[3] < 10+nativeBrowsingOriginCount && (key.SourcePort == 443 || key.SourceIp[3] == 10 && key.SourcePort == 8443) {
			provider.ownedReturnBytes.Add(uint64(observation.PacketByteCount))
		}
	}
	natSettings.SecurityPolicyGenerator = func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
		return &privateSdkBrowsingOriginPolicy{SecurityPolicy: clientconnect.DefaultProviderSecurityPolicyWithStats(ctx, stats), origins: &self.origins, provider: true}
	}
	var err error
	provider.remoteNat, err = clientconnect.TryNewRemoteUserNatProvider(provider.client, provider.localNat, natSettings)
	if err != nil {
		return fmt.Errorf("construct real provider NAT %d: %w", index, err)
	}
	if provider.remoteNat == nil {
		return errors.New("provider NAT constructor returned no owner")
	}
	registered := make(chan error, 1)
	provider.client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true}, func(err error) {
		select {
		case registered <- err:
		default:
		}
	})
	select {
	case err := <-registered:
		provider.provideAcked = err == nil
		if err == nil {
			provider.lifecycle.Store(privateSdkBrowsingProviderReady)
		}
		return err
	case <-self.setupCtx.Done():
		return self.setupCtx.Err()
	}
}

// The native ABI copies compact input before taking every resulting pooled
// packet. TUN originals stay ours and are returned exactly once after the call.
// Its public result is only an aggregate: no per-packet bitmap is fabricated.
func (self *privateSdkBrowsingFixture) startNativeBridge(resources tunResourceProfile) {
	self.bridgeRunning = true
	self.bridgeWaitGroup.Add(1)
	go func() {
		defer func() { self.bridgeWaitGroup.Done(); close(self.bridgeDone) }()
		packets := make([][]byte, max(1, resources.BatchSize))
		var midCall uint64
		for {
			midCall++
			if self.origins.midSession != nil {
				self.origins.midSession.bridge.readBegin(midCall)
			}
			count, err := self.path.appTun.ReadBatch(packets)
			if err != nil {
				return
			}
			if count <= 0 || len(packets) < count || 64 < count {
				for index, packet := range packets {
					if packet != nil {
						clientconnect.MessagePoolReturn(packet)
						packets[index] = nil
					}
				}
				self.bridgeInvalid.Store(true)
				return
			}
			if self.origins.midSession != nil {
				self.origins.midSession.bridge.readReturned(midCall, packets[:count])
			}
			byteCount := 0
			valid := true
			for _, packet := range packets[:count] {
				valid = valid && 0 < len(packet) && len(packet) <= 65535
				byteCount += 2 + len(packet)
			}
			if !valid || 96*1024 < byteCount {
				for index, packet := range packets[:count] {
					clientconnect.MessagePoolReturn(packet)
					packets[index] = nil
				}
				self.bridgeInvalid.Store(true)
				return
			}
			compact := make([]byte, byteCount)
			offset := 0
			for _, packet := range packets[:count] {
				binary.BigEndian.PutUint16(compact[offset:offset+2], uint16(len(packet)))
				offset += 2
				copy(compact[offset:], packet)
				offset += len(packet)
			}
			self.bridgeStarted.Add(uint64(count))
			for old := self.bridgeMaximum.Load(); old < int64(count) && !self.bridgeMaximum.CompareAndSwap(old, int64(count)); old = self.bridgeMaximum.Load() {
			}
			self.path.bridgeSends.batchCountByPacketCount[count].Add(1)
			if resources.AppDelay > 0 {
				delayStart := time.Now()
				time.Sleep(resources.AppDelay)
				self.path.bridgeSends.delayDurationNanoseconds.Add(uint64(time.Since(delayStart)))
			}
			start := time.Now()
			if self.origins.midSession != nil {
				self.origins.midSession.bridge.send(midCall, false, -1)
			}
			accepted := self.device.SendPacketBatch(compact)
			if self.origins.midSession != nil {
				self.origins.midSession.bridge.send(midCall, true, int(accepted))
			}
			self.path.bridgeSends.sendDurationNanoseconds.Add(uint64(time.Since(start)))
			if accepted < 0 || int32(count) < accepted {
				for index, packet := range packets[:count] {
					clientconnect.MessagePoolReturn(packet)
					packets[index] = nil
				}
				self.bridgeInvalid.Store(true)
				return
			}
			self.bridgeAccepted.Add(uint64(accepted))
			self.bridgeRefused.Add(uint64(int32(count) - accepted))
			for index, packet := range packets[:count] {
				clientconnect.MessagePoolReturn(packet)
				packets[index] = nil
			}
			self.bridgeCompleted.Add(uint64(count))
		}
	}()
}

// Two public pull views are not an atomic snapshot. Counts retain the
// actual observation, while masks refer only to six synthetic fixture ordinals.
type privateSdkBrowsingReadinessSnapshot struct {
	Quality                int   `json:"quality"`
	Speed                  int   `json:"speed"`
	Proven                 int   `json:"proven"`
	Exits                  int   `json:"exit_rows"`
	LocalDistinct          int   `json:"local_distinct"`
	MissingLocalIdentities int   `json:"missing_local_identities"`
	MinSatisfied           bool  `json:"min_satisfied"`
	Generation             int64 `json:"connection_generation"`
	MonitorAdded           int   `json:"window_status_added"`
	Locations              int   `json:"location_rows"`
	DistinctOwned          int   `json:"distinct_owned_destinations"`
	OwnedMask              uint8 `json:"owned_ordinal_mask"`
	MissingIdentities      int   `json:"missing_identities"`
	Unowned                int   `json:"unowned_destinations"`
}

// Scalars alone are safe in diagnostics; identities and payload are omitted.
func (self privateSdkBrowsingReadinessSnapshot) String() string {
	return fmt.Sprintf("quality=%d speed=%d proven=%d exits=%d local_distinct=%d missing_local=%d min_satisfied=%t generation=%d status_added=%d locations=%d distinct_owned=%d owned_mask=%02x missing=%d unowned=%d", self.Quality, self.Speed, self.Proven, self.Exits, self.LocalDistinct, self.MissingLocalIdentities, self.MinSatisfied, self.Generation, self.MonitorAdded, self.Locations, self.DistinctOwned, self.OwnedMask, self.MissingIdentities, self.Unowned)
}

// Unknown locations are still real providers. Only the authenticated egress
// identity, not geographic metadata, joins them to the owned candidate set.
func privateSdkBrowsingReadinessSnapshotFrom(exits *sdk.ExitList, status *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList, providers *[6]privateSdkBrowsingProvider) privateSdkBrowsingReadinessSnapshot {
	snapshot := privateSdkBrowsingReadinessSnapshot{}
	if status != nil {
		snapshot.MinSatisfied, snapshot.Generation, snapshot.MonitorAdded = status.MinSatisfied, status.ConnectionGeneration, status.ProviderStateAdded
	}
	localIds := map[string]bool{}
	if exits != nil {
		snapshot.Exits = exits.Len()
		for index := 0; index < exits.Len(); index++ {
			exit := exits.Get(index)
			if exit == nil || exit.ClientId == nil {
				snapshot.MissingLocalIdentities++
			}
			if exit == nil {
				continue
			}
			if exit.ClientId != nil {
				localIds[exit.ClientId.String()] = true
			}
			if exit.Done || exit.Warning || exit.Quarantined {
				continue
			}
			if exit.Proven {
				snapshot.Proven++
			}
			if exit.WindowType == "quality" {
				snapshot.Quality++
			} else if exit.WindowType == "speed" {
				snapshot.Speed++
			}
		}
	}
	snapshot.LocalDistinct = len(localIds)
	if locations != nil {
		snapshot.Locations = locations.Len()
		for index := 0; index < locations.Len(); index++ {
			location := locations.Get(index)
			if location == nil || location.ClientId == nil {
				snapshot.MissingIdentities++
				continue
			}
			owned := false
			for ordinal := range providers {
				if location.ClientId.String() == providers[ordinal].id.String() {
					bit := uint8(1) << ordinal
					if snapshot.OwnedMask&bit == 0 {
						snapshot.DistinctOwned++
					}
					snapshot.OwnedMask |= bit
					owned = true
					break
				}
			}
			if !owned {
				snapshot.Unowned++
			}
		}
	}
	return snapshot
}

// A partially published owned projection is still forming, not success or a
// fatal verdict. Permanent duplicates/missing identities exhaust the same
// original setup deadline; foreign ownership is an immediate fixture error.
func privateSdkBrowsingReadiness(snapshot privateSdkBrowsingReadinessSnapshot, requireProven bool) (bool, error) {
	if snapshot.Unowned != 0 {
		return false, fmt.Errorf("SDK_SIX_PROVIDER_UNOWNED_DESTINATION: %s", snapshot)
	}
	channelsReady := snapshot.Quality == 5 && snapshot.Speed == 1 && snapshot.Exits == 6 && snapshot.LocalDistinct == 6 && snapshot.MinSatisfied && (!requireProven || snapshot.Proven == 6)
	projectionReady := snapshot.Locations == 6 && snapshot.DistinctOwned == 6 && snapshot.OwnedMask == 0x3f && snapshot.MissingIdentities == 0
	return channelsReady && projectionReady, nil
}

// The production fixture passes its original200ms ticker and180s setup
// context. Controls pass explicit barrier ticks under the same owner contract.
func privateSdkBrowsingAwaitReadiness(ctx context.Context, tick <-chan time.Time, read func() privateSdkBrowsingReadinessSnapshot, requireProven bool) (privateSdkBrowsingReadinessSnapshot, error) {
	snapshot := privateSdkBrowsingReadinessSnapshot{}
	for {
		if err := ctx.Err(); err != nil {
			return snapshot, fmt.Errorf("SDK_SIX_PROVIDER_READINESS_PREMISE: %s: %w", snapshot, err)
		}
		snapshot = read()
		ready, err := privateSdkBrowsingReadiness(snapshot, requireProven)
		if err != nil {
			return snapshot, err
		}
		if ready {
			if err := ctx.Err(); err != nil {
				return snapshot, fmt.Errorf("SDK_SIX_PROVIDER_READINESS_PREMISE: %s: %w", snapshot, err)
			}
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return snapshot, fmt.Errorf("SDK_SIX_PROVIDER_READINESS_PREMISE: %s: %w", snapshot, ctx.Err())
		case <-tick:
		}
	}
}

// Every original readiness condition remains required, including all six
// distinct owned tails and actual qualification once warmup has completed.
func (self *privateSdkBrowsingFixture) waitReady(requireProven bool) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	snapshot, err := privateSdkBrowsingAwaitReadiness(self.setupCtx, ticker.C, func() privateSdkBrowsingReadinessSnapshot {
		return privateSdkBrowsingReadinessSnapshotFrom(self.device.GetExits(), self.device.GetWindowStatus(), self.device.GetConnectedProviderLocations(), &self.providers)
	}, requireProven)
	if err != nil {
		return err
	}
	self.report.ReadyQuality, self.report.ReadySpeed, self.report.ReadyProven, self.report.ReadyDistinctExits = snapshot.Quality, snapshot.Speed, snapshot.Proven, snapshot.DistinctOwned
	self.report.ReadinessSnapshot = &snapshot
	return nil
}

// Host accounting exposes real queue use. Provider identity checks remain
// fail-closed during workload capture; no receive capacity/refusal is invented.
func (self *privateSdkBrowsingFixture) sampleReceive(ready bool) (int, error) {
	snapshot := privateSdkBrowsingReadinessSnapshotFrom(nil, nil, self.device.GetConnectedProviderLocations(), &self.providers)
	if snapshot.MissingIdentities != 0 || snapshot.Unowned != 0 {
		return 0, fmt.Errorf("SDK_SIX_PROVIDER_WORKLOAD_OWNERSHIP: %s", snapshot)
	}
	if !ready {
		memory := self.device.MemoryUsed()
		// MemoryUsed explicitly includes Pack once in ClientReceive. Subtract
		// that subset rather than counting it as ReceiveSequence hold bytes.
		used := memory.ClientReceiveByteCount - memory.PackQueueUsedByteCount
		if used < 0 {
			return 0, errors.New("SDK public receive/Pack snapshot could not be decomposed coherently")
		}
		self.report.SampledReceivePeak = max(self.report.SampledReceivePeak, used)
	}
	return snapshot.DistinctOwned, nil
}

// Browser boundary fields retain their original meanings where available;
// the separate SDK report carries authoritative SDK-owned memory and topology.
func (self *privateSdkBrowsingFixture) captureBoundary(path *fullTunPath, page int, pressure *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary {
	if _, err := self.sampleReceive(false); err != nil {
		self.t.Error(err)
	}
	boundary := &nativeBrowsingPressureBoundary{Page: page, AtNs: time.Now().UnixNano(), AssetOwnership: pressure.assetOwnershipSnapshot(), PoolRoots: clientconnect.MessagePoolOutstandingCount(), PoolBytes: clientconnect.MessagePoolOutstandingByteCount()}
	boundary.BridgeStarted, boundary.BridgeRefused = self.bridgeStarted.Load(), self.bridgeRefused.Load()
	for index, tun := range []*clientconnect.Tun{self.path.appTun, self.deviceCarrier} {
		stats := tun.Stats().TCP
		boundary.Tcp[index] = [4]uint64{stats.SegmentsSent.Value(), stats.ValidSegmentsReceived.Value(), stats.Retransmits.Value(), stats.Timeouts.Value()}
	}
	exits := self.device.GetExits()
	for index := 0; index < exits.Len(); index++ {
		if exits.Get(index).WindowType == "quality" {
			boundary.Multi.QualityClientCount++
		} else if exits.Get(index).WindowType == "speed" {
			boundary.Multi.SpeedClientCount++
		}
	}
	if err := self.captureProviderBoundary(boundary, false); err != nil {
		self.t.Error(err)
		return nil
	}
	for _, link := range path.environment.network.snapshotLinks() {
		boundary.LinkCount++
		boundary.LinkWireBytes += link.WireByteCount
		boundary.LinkLossDrops += link.LossDropPacketCount
		boundary.LinkQueueDrops += link.QueueDropPacketCount
		boundary.LinkQueuedBytes += link.QueuedByteCount
	}
	return boundary
}

// Only ready providers expose their stable client, carrier and NAT pointers.
// The deliberate late absence contributes no traffic and is identified by mask.
func (self *privateSdkBrowsingFixture) captureProviderBoundary(boundary *nativeBrowsingPressureBoundary, lateAbsent bool) error {
	var result error
	for index := range self.providers {
		provider := &self.providers[index]
		lifecycle := provider.lifecycle.Load()
		if lateAbsent && index == 4 && lifecycle == privateSdkBrowsingProviderAbsent {
			boundary.SdkProviderAbsentMask |= uint8(1 << index)
			continue
		}
		if lifecycle != privateSdkBrowsingProviderReady || provider.client == nil || provider.carrier == nil || provider.remoteNat == nil || provider.returns == nil {
			result = errors.Join(result, fmt.Errorf("provider %d boundary unavailable: lifecycle=%d", index, lifecycle))
			continue
		}
		boundary.SdkProviderReadyMask |= uint8(1 << index)
		recovery, receive := provider.client.SendRecoveryStats(), provider.client.ReceiveStats()
		boundary.ProviderInitialWrites += recovery.InitialWriteCount
		boundary.ProviderTimeoutResends += recovery.TimeoutResendWriteCount
		boundary.ProviderMissingContracts += recovery.MissingContractRequestCount
		boundary.ProviderAdmissionWaitNs += int64(recovery.ReliableAdmissionWaitDuration)
		boundary.ProviderPackDrops += receive.PackHandoffDropCount
		boundary.ProviderUnacked += receive.ResendQueueUnackedItemCount
		boundary.ProviderPackWaits += receive.PackHandoffWaitCount
		boundary.ProviderPackWaitSuccess += receive.PackHandoffWaitSuccess
		boundary.ProviderPackSaturations += receive.PackHandoffSaturationCount
		boundary.ProviderAckBlocked += receive.AckRouteWriteBlockedCount
		stats := provider.carrier.Stats().TCP
		for index, count := range [4]uint64{stats.SegmentsSent.Value(), stats.ValidSegmentsReceived.Value(), stats.Retransmits.Value(), stats.Timeouts.Value()} {
			boundary.Tcp[2][index] += count
		}
		nat := provider.remoteNat.PacketStats()
		boundary.ProviderNatRemotePackets += nat.RemoteEgressPacketCount + nat.RemoteIngressPacketCount
		boundary.ProviderNatBlockedPackets += nat.BlockEgressPacketCount + nat.BlockIngressPacketCount
	}
	return result
}

// Cleanup joins the native producer before retiring the SDK and its provider
// readers; every independent owner is attempted even after one join failure.
func (self *privateSdkBrowsingFixture) close() error {
	self.closeOnce.Do(func() {
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelJoin()
		if self.path != nil && self.path.appTun != nil {
			self.closeErr = errors.Join(self.closeErr, self.path.appTun.Close())
		}
		if self.device != nil {
			self.device.Cancel()
		}
		if self.bridgeRunning {
			select {
			case <-self.bridgeDone:
			case <-joinCtx.Done():
				self.closeErr = errors.Join(self.closeErr, errors.New("SDK native producer did not join"))
			}
		}
		if self.device != nil {
			joinErr := self.device.CloseAndWait(joinCtx)
			self.closeErr = errors.Join(self.closeErr, joinErr)
			if joinErr == nil {
				self.report.SdkJoined = true
				self.report.AfterJoin = self.device.MemoryUsed()
				m := self.report.AfterJoin
				if m.TransferRootUsedByteCount != 0 || m.TransferRootReservedByteCount != m.TransferRootReleasedByteCount || m.PlatformTransportUsedByteCount != 0 || m.PlatformTransportUsedCount != 0 || m.PlatformTransportPendingH1Count != 0 || m.PlatformTransportPendingHandoffCount != 0 || m.PlatformTransportActiveHandoffCount != 0 || m.PlatformTransportReservedBytes != m.PlatformTransportReleasedBytes {
					self.closeErr = errors.Join(self.closeErr, errors.New("SDK constructor-owned memory or carrier custody remained after join"))
				}
			}
		}
		if self.space != nil {
			self.space.Close()
		}
		if self.deviceStrategy != nil {
			self.deviceStrategy.Close()
		}
		if self.deviceCarrier != nil {
			self.closeErr = errors.Join(self.closeErr, self.deviceCarrier.Close())
		}
		for index := range self.providers {
			provider := &self.providers[index]
			if provider.remoteNat != nil {
				provider.remoteNat.Close()
			}
			if provider.localNat != nil {
				self.closeErr = errors.Join(self.closeErr, provider.localNat.CloseAndWait(joinCtx))
			}
			if provider.client != nil {
				self.closeErr = errors.Join(self.closeErr, closeClientAndOutOfBandWait(joinCtx, provider.client))
			}
			if provider.transport != nil {
				self.closeErr = errors.Join(self.closeErr, provider.transport.CloseAndWait(joinCtx))
			}
			if provider.strategy != nil {
				provider.strategy.Close()
			}
			if provider.carrier != nil {
				self.closeErr = errors.Join(self.closeErr, provider.carrier.Close())
			}
			if provider.noAck != nil {
				provider.noAck.close()
			}
			if provider.packs != nil {
				boundary, ok := provider.packs.boundary(joinCtx)
				if !ok || !provider.packs.waitThrough(joinCtx, boundary) {
					self.closeErr = errors.Join(self.closeErr, fmt.Errorf("provider %d Pack lifecycle did not join", index))
				}
				provider.packs.close()
			}
			if provider.returns != nil {
				boundary, ok := provider.returns.lifecycleBoundary(joinCtx)
				if !ok || !provider.returns.waitLifecycleThrough(joinCtx, boundary) {
					self.closeErr = errors.Join(self.closeErr, fmt.Errorf("provider %d return lifecycle did not join", index))
				}
				provider.returns.close()
			}
			if provider.resendBudget != nil {
				reserved, released := provider.resendBudget.Counts()
				if provider.resendBudget.UsedByteCount() != 0 || reserved != released {
					self.closeErr = errors.Join(self.closeErr, fmt.Errorf("provider %d retained retry ownership after join", index))
				}
			}
		}
		self.origins.stateLock.Lock()
		if len(self.origins.providerBySource) != 0 {
			self.closeErr = errors.Join(self.closeErr, errors.New("owned upstream socket mapping remained after provider joins"))
		}
		self.origins.stateLock.Unlock()
		self.report.SdkBridgePackets, self.report.SdkBridgeAccepted, self.report.SdkBridgeRefused, self.report.SdkBridgeMaximum = self.bridgeStarted.Load(), self.bridgeAccepted.Load(), self.bridgeRefused.Load(), self.bridgeMaximum.Load()
		if self.bridgeInvalid.Load() || self.bridgeStarted.Load() != self.bridgeCompleted.Load() || self.bridgeCompleted.Load() != self.bridgeAccepted.Load()+self.bridgeRefused.Load() {
			self.closeErr = errors.Join(self.closeErr, errors.New("SDK native aggregate admission did not close every input ownership"))
		}
		if self.environment != nil {
			self.environment.close()
		}
		self.cancelSetup()
		self.cancel()
	})
	return self.closeErr
}

// Synthetic view builders exercise the actual public SDK list/identity types;
// they do not manufacture admission, qualification or success for actual100.
func privateSdkBrowsingReadinessViews(t testing.TB) (*sdk.ExitList, *sdk.WindowStatus, *sdk.ConnectedProviderLocationList, *[6]privateSdkBrowsingProvider) {
	t.Helper()
	exits, locations := sdk.NewExitList(), sdk.NewConnectedProviderLocationList()
	providers := &[6]privateSdkBrowsingProvider{}
	for ordinal := range providers {
		providers[ordinal].id = clientconnect.NewId()
		providerId, err := sdk.ParseId(providers[ordinal].id.String())
		if err != nil {
			t.Fatal(err)
		}
		windowType := "quality"
		if ordinal == 5 {
			windowType = "speed"
		}
		exits.Add(&sdk.Exit{ClientId: sdk.NewId(), WindowType: windowType, Proven: true})
		locations.Add(&sdk.ConnectedProviderLocation{ClientId: providerId})
	}
	return exits, &sdk.WindowStatus{MinSatisfied: true, ProviderStateAdded: 6, ConnectionGeneration: 1}, locations, providers
}

// The constructor retains the public readiness and owned-identity gate used
// by the measured workload; no strict-six qualification or synthetic warmup.
func newPrivateSdkProductionReadinessFixture(t testing.TB, plan *nativeBrowsingPressurePlan, mode *privateSdkDefaultContractMode, readiness *privateSdkProductionReadinessMode) (*privateSdkBrowsingFixture, error) {
	t.Helper()
	if mode == nil || readiness == nil {
		return nil, errors.New("production-readiness root requires default-contract and readiness observers")
	}
	if plan.resources.BatchSize <= 0 || 64 < plan.resources.BatchSize {
		return nil, errors.New("SDK native fixture requires the unchanged ABI-bounded bridge batch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	setupCtx, cancelSetup := context.WithTimeout(ctx, 180*time.Second)
	fixture := &privateSdkBrowsingFixture{defaultContracts: mode, productionReadiness: readiness, t: t, ctx: ctx, setupCtx: setupCtx, cancelSetup: cancelSetup, cancel: cancel, bridgeDone: make(chan struct{})}
	if readiness.midSession != nil {
		fixture.origins.midSession = readiness.midSession
		readiness.midSession.fixture = fixture
	}
	constructed := false
	defer fixture.closeUnlessConstructed(&constructed)
	t.Cleanup(func() {
		if err := fixture.close(); err != nil {
			t.Error(err)
		}
	})
	fixture.environment = newRouteEnvironmentWithNetworkPeers(ctx, t, plan.profile, false)
	fixture.environment.deviceAccessProfile = plan.profile
	fixture.environment.providerAccessProfile = plan.provider
	fixture.createProviderNetwork()
	extraOrigins := 0
	if readiness.midSession != nil && readiness.midSession.cold {
		extraOrigins = 1
	}
	serverTls, clientTls, err := privateSdkBrowsingTlsConfigs(extraOrigins)
	if err != nil {
		return nil, err
	}
	fixture.serverTls, fixture.clientTls = serverTls, clientTls
	for index := range fixture.providers {
		if err := fixture.buildProvider(index, plan); err != nil {
			return nil, fmt.Errorf("construct local provider %d: %w", index, err)
		}
	}
	fixture.deviceCarrier, fixture.deviceStrategy = fixture.environment.newClientNodeWithProfileAt(false, plan.profile, fixture.environment.deviceEdgeName)
	strategySettings := routeClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategySettings.ConnectSettings = *fixture.deviceStrategy.ConnectSettings()
	apiUrl := fmt.Sprintf("http://%s:%d", fixture.environment.edgeAddress, fixture.environment.apiPort)
	platformUrl := fmt.Sprintf("wss://%s:%d", fixture.environment.edgeAddress, fixture.environment.h1Port)
	fixture.space = sdk.NewNetworkSpaceWithUrls(ctx, apiUrl, platformUrl, strategySettings)
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(ctx, fixture.environment.networkId, deviceId, clientId, "sdk browsing device", "synthetic sdk browsing device")
	for index := range fixture.providers {
		details := model.GetProvideRelationshipDetails(setupCtx, clientId, server.Id(fixture.providers[index].id))
		if details.Mode != model.ProvideModePublic || details.SourceLifecycle != model.NetworkClientLifecycleActiveTop || details.DestinationLifecycle != model.NetworkClientLifecycleActiveTop {
			return nil, fmt.Errorf("SDK_SIX_PROVIDER_PUBLIC_RELATIONSHIP_PREMISE: provider=%d relationship=%d source=%s destination=%s", index, details.Mode, details.SourceLifecycle, details.DestinationLifecycle)
		}
	}
	jwt := session.NewByJwt(fixture.environment.networkId, fixture.environment.userId, fmt.Sprintf("perfvar-%s", fixture.environment.networkId), false, false).Client(deviceId, clientId).Testing_Sign()
	settings := sdk.DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = 32 * 1024 * 1024
	settings.LowMemoryProfile = true
	settings.AllowProvider = false
	settings.ProvideExtenderEnabled = false
	settings.EnableRpc = false
	settings.DefaultRouteLocal = false
	settings.DefaultOffline = false
	settings.DefaultTunnelStarted = true
	settings.GeneratorFunc = nil
	settings.ProviderDiscovery = &privateSdkBrowsingDiscovery{providers: &fixture.providers, strategy: fixture.deviceStrategy, apiUrl: apiUrl}
	device, err := privateSdkBrowsingNewDevice(fixture.space, jwt, settings)
	if err != nil {
		return nil, err
	}
	fixture.device = device
	device.SetClientSecurityPolicyGenerator(func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
		return &privateSdkBrowsingOriginPolicy{SecurityPolicy: clientconnect.DefaultSecurityPolicyWithStats(ctx, stats), origins: &fixture.origins}
	})
	device.SetTransportSettings(&sdk.TransportSettings{Mode: sdk.TransportModeH1})
	device.SetRouteLocal(false)
	specs := sdk.NewProviderSpecList()
	specs.Add(&sdk.ProviderSpec{BestAvailable: true})
	if err := readiness.captureStart(fixture); err != nil {
		return nil, err
	}
	device.SetDestination(&sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true}}, specs)
	appSettings := clientconnect.DefaultTunSettingsWithBufferSize(plan.resources.ChannelSize)
	path := &fullTunPath{t: t, ctx: ctx, environment: fixture.environment, route: fullTunRouteExchangeH1, p2pHopCount: 1, bridgeSends: newFullTunBridgeSendTracker()}
	appSettings.Mtu = resolvedFullTunApplicationMtu(plan.profile, plan.resources)
	applyFullTunApplicationDialSettings(appSettings, path)
	applyTunResourceProfile(appSettings, plan.resources)
	appTun, err := clientconnect.CreateTun(ctx, appSettings)
	if err != nil {
		return nil, err
	}
	path.appTun = appTun
	fixture.path = path
	device.AddReceivePacketsCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packets [][]byte) {
		_, _ = appTun.WriteBatch(packets)
	})
	fixture.startNativeBridge(plan.resources)
	if err := readiness.wait(fixture); err != nil {
		return nil, err
	}
	if settings.SendBufferSettings.ApplicationPacing || settings.GeneratorFunc != nil {
		return nil, errors.New("SDK fixture changed default pacing or bypassed its generator constructor")
	}
	fixture.report.Before = *device.MemoryUsed()
	if fixture.report.Before.TargetByteCount != 32*1024*1024 || fixture.report.Before.TransferRootBudgetByteCount != 21810380 || fixture.report.Before.ClientTransferBudgetByteCount != 21810380 || fixture.report.Before.PackQueueCapacityByteCount != 2181038 {
		return nil, errors.New("SDK constructor did not retain its 32-MiB owner and shared low-profile admission hierarchy")
	}
	// The public host readout exposes exact used bytes but no receive leaf
	// capacity. Its capacity is the pinned SDK policy, not caller settings:
	// NewDeviceLocal clones and replaces those caller-owned budget pointers.
	fixture.report.ExpectedReceiveCapacity = 2181038
	fixture.report.ReceiveCapacitySource = "pinned SDK32 provider-off policy; matching root/client/Pack capacities independently read through public MemoryUsed; receive leaf capacity is not exported on host"
	fixture.report.ReceivePressureClassification = "UNMEASURED_HOST_RECEIVE_CAPACITY_AND_REFUSALS"
	constructed = true
	return fixture, nil
}

// Independent workload driver selects only the public-readiness setup and retains the original workload.
func testPrivateSdkProductionReadinessBrowsing(t *testing.T, mode *privateSdkDefaultContractMode, readiness *privateSdkProductionReadinessMode) {
	if testing.Short() {
		t.Skip("private SDK-owned production-readiness H1/TUN browsing topology")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		plan := newNativeBrowsingPressurePlan()
		fixture, err := newPrivateSdkProductionReadinessFixture(t, plan, mode, readiness)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		innerHttp := newNativeBrowsingInnerHttpTrace(false)
		pressure := &nativeBrowsingPressureObservation{Profile: plan.profile, ProviderProfile: plan.provider.Name, MaximumLivePages: plan.maxLivePages, HttpConnectionsPerOrigin: nativeBrowsingConnectionsPerOrigin, slowNavigation: sdk.DefaultDeviceLocalSettings().SendTimeout, PhaseNames: [8]string{"get_conn", "got_conn", "connect_start", "connect_done", "tls_start", "tls_done", "wrote_request", "first_byte"}, captureBoundaryForTest: fixture.captureBoundary}
		pressure.Scope = "production-readiness-default-opening-v1; public SDK MinSatisfied plus owned identity projection, no all-six Proven or owned-probe prerequisite; no qualification setter or forced provider; original six synthetic provider candidates remain available and ordinary probes/ranking continue; original 100 HTML/400 assets/99 overlaps, bytes/TLS1.3/inner H1/outer H1, default opening credit and 180s setup/300s application/600s fixture bounds; connect-start warmup and first actual HTML byte measured inclusively without resetting elapsed; host mechanism observation, not mobile memory or production Internet qualification"
		for index := range pressure.Navigations {
			pressure.Navigations[index].ConnectionAcquireNs, pressure.Navigations[index].WrittenToFirstByteNs = -1, -1
			for phase := range pressure.Navigations[index].Phases {
				pressure.Navigations[index].Phases[phase] = -1
			}
		}
		observation := &nativeBrowsingObservation{NativeCapability: true, Baseline: nativeBrowsingMemoryNow(), Pressure: pressure}
		observation.Peak = observation.Baseline
		fixture.report.Scope, fixture.report.Browser = pressure.Scope, observation
		fixture.report.UnavailableInheritedBoundaryFields = []string{"private discovery admission/park/age/SYN wake/late bootstrap/auth phase trace; no production bridge is installed", "same-sequence unexpired opening debt/expiry attribution; public signed grants and joined usage are reported separately", "multi Transfer counters except live quality/speed counts; host SDK has no public mobile diagnostic sampler", "bridge per-input admission bitmap; SDK public native ABI returns only aggregate count"}
		before, start := fixture.path.bridgeSends.batchSnapshot(), readiness.started
		defer func() {
			readiness.captureEnd(fixture)
			pressure.finish(start)
			observation.Bridge = observeFullTunBridgeBatches(before, fixture.path.bridgeSends.batchSnapshot())
			if err := fixture.captureProviderReports(false); err != nil {
				t.Error(err)
			}
			for index := range fixture.report.OriginProviderMasks {
				fixture.report.OriginProviderMasks[index] = fixture.originProviderMasks[index].Load()
			}
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
			innerReport := innerHttp.snapshot()
			fixture.report.InnerHttp = &innerReport
			if err := innerHttp.verdict(); err != nil {
				t.Error(err)
			}
			readiness.finishOpening(t, mode, observation)
			readiness.finish(t, fixture, observation)
			if readiness.midSession != nil {
				readiness.midSession.finish(t, observation)
			}
			observation.Drain = nativeBrowsingMemoryNow()
			encoded, err := json.Marshal(fixture.report)
			if err != nil || nativeBrowsingPressureReportByteLimit < len(encoded) {
				t.Errorf("bounded SDK browsing report: bytes=%d err=%v", len(encoded), err)
				return
			}
			t.Logf("[perfvar] sdk-production-readiness-pressure100 %s", encoded)
		}()
		prepareNavigation := func(ctx context.Context) error {
			return readiness.prepareNavigation(ctx, fixture)
		}
		settings := nativeBrowsingRunSettings{maxLivePages: plan.maxLivePages, pressure: pressure, innerHttp2ForTest: false, innerHttpTraceForTest: innerHttp, originListenerForTest: fixture.origins.add, originServerTlsForTest: fixture.serverTls, originClientTlsForTest: fixture.clientTls, originDestinationForTest: fixture.origins.destination, beforeNavigationForTest: prepareNavigation, afterOriginsClosedForTest: fixture.origins.clear, wrapOriginHandlerForTest: fixture.wrapOriginHandler, applicationDeadlineForTest: plan.deadline}
		settings.requestTraceForTest = readiness.requestTrace
		settings.beforePageNavigationForTest = func(_ context.Context, page int, _ func()) error {
			mode.page.Store(int64(page))
			return nil
		}
		if readiness.midSession != nil {
			readiness.midSession.configure(&settings)
			pressure.Scope += "; original-body observation purpose=" + string(readiness.midSession.purpose) + "; no late construction or production policy change"
		}
		if err := measureNativeBrowsing100WithSettings(fixture.ctx, fixture.path, observation, settings); err != nil {
			t.Fatal(err)
		}
		if err := nativeBrowsingPressureLatencyError(observation); err != nil {
			t.Fatal(err)
		}
		if observation.OverlapProofs != nativeBrowsingPageCount-1 || pressure.MaximumPriorRemainingBodyBytes == 0 {
			t.Fatal("production-readiness browsing did not retain every original body-overlap witness")
		}
		if _, err := fixture.sampleReceive(false); err != nil {
			t.Fatal(err)
		}
		t.Log("SDK_PRODUCTION_READINESS_RECEIVE_PRESSURE_UNMEASURED: public receive-used samples retained; host capacity/refusal telemetry unavailable; not a receive-pressure or mobile memory verdict")
	})
}
