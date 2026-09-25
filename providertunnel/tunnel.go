// Package providertunnel builds an http.Client whose every request egresses
// through one specific urnetwork provider, so what a request made with it sees
// -- the operator's own /ip echo, a site's answer -- is that provider's egress.
package providertunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/operator-proxy/controlplane"
)

// The operator-provided identity and endpoints the prober uses to
// build tunnels. ClientId is the prober's own client id (so it can exclude
// itself when selecting providers); ByJwt is its network client jwt.
type Config struct {
	ApiUrl      string
	PlatformUrl string
	ByJwt       string
	ClientId    connect.Id
	// Certificate pins per host: a pinned host must present a chain
	// with one of its pinned keys on the verified path, on top of ordinary
	// WebPKI verification. They are optional -- a host without an entry is
	// verified by WebPKI alone (see HttpClientForHosts). The map is also part
	// of the allowlist (a pinned host may be dialed), so a caller must not hand
	// it hosts the tunnel should not reach: fleetprobe restricts the served set
	// to the hosts a probe dials before it opens a tunnel.
	Pins              map[string][]string
	DeviceDescription string
	DeviceSpec        string
	Version           string
	// Optionally bounds the per-contract size
	// ramp for a small probe. Zero preserves the shared transfer defaults.
	// This is a reservation target, not a traffic quota: packet-size floors
	// and the server's admission bounds still apply, and contracts renew.
	ContractReservationByteCount connect.ByteCount
	// Descendant carriers may share an explicitly supplied probe-owner budget.
	// Nil gives this tunnel a fresh independent budget using default values.
	PlatformTransportBudget *connect.PlatformTransportBudget
	// Bounds how long Close, or an Open that fails part-way, waits for the
	// multi-client and the generator to retire. Zero or negative means 30s.
	CloseTimeout time.Duration
	// Bounds how long the packet pump waits for the multi-client to take one
	// packet from the tun. Zero or negative means 15s.
	PacketSendTimeout time.Duration
}

// Returns CloseTimeout, or its default when unset.
func (self Config) closeTimeout() time.Duration {
	if 0 < self.CloseTimeout {
		return self.CloseTimeout
	}
	return 30 * time.Second
}

// Returns PacketSendTimeout, or its default when unset.
func (self Config) packetSendTimeout() time.Duration {
	if 0 < self.PacketSendTimeout {
		return self.PacketSendTimeout
	}
	return 15 * time.Second
}

// The cause (context.Cause) of Tunnel.Lost when the
// tunnel's path to its provider went away while the tunnel was open: the
// pinned provider left the multi-client with nothing added in its place, or
// the packet pump's read from the tun failed. A measurement in flight when it
// happens is not a measurement of the provider, and the prober reports it as
// aborted rather than as the failures a dead tunnel produces.
var ErrTunnelLost = errors.New("providertunnel: the tunnel's path to the provider is gone")

// Lost's cause once the tunnel has been closed on purpose.
var ErrTunnelClosed = errors.New("providertunnel: the tunnel was closed")

// Negative reservation targets are malformed, not a request for defaults.
var ErrContractReservation = errors.New("providertunnel: contract reservation byte count must not be negative")

// Returned for any http:// request made through a
// tunnel client. The allowlist and the certificate pins live in
// DialTLSContext, which only https traffic reaches; a plain-http request
// would ride the raw tunnel dialer in cleartext, forgeable by the very
// provider being measured.
var ErrPlainHttpRefused = errors.New("providertunnel: plain http refused; only https, which the allowlist and pin check cover, may traverse the tunnel")

// Builds the gvisor tun Open routes through. It is a var, and takes
// the resolver settings explicitly rather than reaching for them itself, so a
// test can observe exactly what Open asks for -- see
// TestOpenUsesInTunnelOnlyDnsResolution. Reverting to
// connect.CreateTunWithDefaults (the off-tunnel-DNS default, see
// inTunnelOnlyDnsResolverSettings) would bypass this seam entirely and fail
// that test rather than silently reintroducing the leak.
var createTun = func(ctx context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
	return connect.CreateTunWithResolver(ctx, connect.DefaultTunSettings(), resolver)
}

// The control-plane strategy is a seam so the Open-path test can prove every
// provider tunnel gets its own strategy from the IPv4-only constructor.
var newControlplaneClientStrategy = controlplane.NewClientStrategy

// Tests observe the actual generator settings without starting a provider.
var newApiMultiClientGenerator = connect.NewApiMultiClientGenerator

// The fixed-provider tunnel is itself the instrument used by the outer
// egress-health probe. Running RemoteUserNatMultiClient's ordinary
// background provider-qualification sweep inside it asks a second, unrelated
// set of health questions through every short-lived probe tunnel. Those nested
// probes cannot affect the outer verdict, but they multiply traffic and emit
// transition/failure telemetry that looks like another failing workload.
// Start from the shared defaults so every transport behavior stays aligned,
// and disable only the redundant inner authority.
func providerTunnelMultiClientSettings() *connect.MultiClientSettings {
	settings := connect.DefaultMultiClientSettings()
	settings.ProviderProbe = false
	return settings
}

// Each generated client owns fresh settings. Full and bandwidth probes leave
// the reservation target unset and retain the shared contract ramp.
func providerTunnelClientSettings(reservationByteCount connect.ByteCount) *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	if 0 < reservationByteCount {
		contracts := settings.ContractManagerSettings
		contracts.InitialContractTransferByteCount = min(contracts.InitialContractTransferByteCount, reservationByteCount)
		contracts.InitialNetworkPeerContractTransferByteCount = min(contracts.InitialNetworkPeerContractTransferByteCount, reservationByteCount)
		contracts.StandardContractTransferByteCount = min(contracts.StandardContractTransferByteCount, reservationByteCount)
	}
	return settings
}

// A read failure while the tunnel is live makes the data path silently stop,
// so it must remain visible. Tunnel.Close cancels this same context before it
// closes the tun, however, and Tun.Read then returns its ordinary terminal
// "Done" error. That lifecycle completion is not a failed connection and is
// deliberately silent.
func reportTunReadError(ctx context.Context, err error, print func(...any)) {
	if ctx.Err() == nil {
		print("providertunnel: tun read error:", err)
	}
}

// Returns DNS resolver settings under which
// every name the tunnel resolves is resolved through the tunnel, encrypted.
//
// This exists because connect.CreateTunWithDefaults (and CreateTun, which
// passes a nil resolver) inherits connect.DefaultDnsResolverSettings, whose
// EnableLocalDns is true: when in-tunnel DoH produces nothing within its
// budget -- a flaky provider, a provider that blocks 1.1.1.1:443, or simply a
// tunnel that has not finished coming up -- DohCache.resolve falls back to a
// plaintext port-53 query issued from the host's dialer. The TCP connection
// that follows still egresses through the tunnel, so the measurement stays
// correct, but the prober's own server IP has just asked a public resolver,
// in the clear, for a destination it is probing through a provider. This
// operator tool must never emit a packet to (or about) a probe destination
// from its own address, so it fails closed instead: a probe that cannot
// resolve in-tunnel fails and is retried.
//
// This mirrors connect's own DefaultUpgradeMuxSettings
// (connect/ip_mux_upgrade.go), which disables the same toggles for the same
// reason ("would resolve off-tunnel or in the clear"). Every Enable* toggle
// other than EnableRemoteDoh is left false, and the Local* server lists are
// left empty so that even a future connect default or an accidental toggle
// flip has no host-side resolver to reach for. RemoteDns* is carried: it is
// the one permitted plaintext use, resolving a hostname-form DoH server
// through the tunnel, and it stays inside the tunnel (see DohCache.resolve).
func inTunnelOnlyDnsResolverSettings() *connect.DnsResolverSettings {
	base := connect.DefaultDnsResolverSettings()
	return &connect.DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: base.RemoteDohUrlsIpv4,
		RemoteDohUrlsIpv6: base.RemoteDohUrlsIpv6,
		RemoteDnsIpv4:     base.RemoteDnsIpv4,
		RemoteDnsIpv6:     base.RemoteDnsIpv6,
	}
}

// A live data path pinned to exactly one provider. Every connection
// dialed through it egresses from that provider.
type Tunnel struct {
	cancelData      context.CancelFunc
	cancelLifecycle context.CancelFunc
	tun             *connect.Tun
	multiClient     *connect.RemoteUserNatMultiClient
	generator       *connect.ApiMultiClientGenerator
	clientStrategy  *connect.ClientStrategy
	pumpDone        <-chan struct{}
	pins            map[string][]string
	// Ends when the path to the provider goes away while the tunnel is
	// open, or when it is closed; lose sets why. unwatch stops the
	// multi-client subscription that watches for the provider leaving.
	lost         context.Context
	lose         context.CancelCauseFunc
	unwatch      func()
	closeTimeout time.Duration
	closeOnce    sync.Once
	closeErr     error
}

// An owner whose retirement Close joins.
type closeAndWaiter interface {
	CloseAndWait(context.Context) error
}

// Builds a tunnel that routes exclusively through providerClientId.
// It mirrors the proven construction in urnetwork/proxy socks/main.go: an api
// multiclient generator pinned to one ProviderSpec, a gvisor tun, and a packet
// pump in both directions.
func Open(ctx context.Context, cfg Config, providerClientId connect.Id) (*Tunnel, error) {
	if cfg.ContractReservationByteCount < 0 {
		return nil, ErrContractReservation
	}

	// The packet path and the control-plane lifecycle have separate child
	// contexts. Close must stop Tun traffic first, while keeping the API and
	// ClientStrategy alive long enough to retire the short-lived derived
	// network client. A single shared cancellation edge made every final
	// remove-client request start on an already-canceled strategy.
	lifecycleCtx, cancelLifecycle := context.WithCancel(ctx)
	dataCtx, cancelData := context.WithCancel(lifecycleCtx)
	clientStrategy := newControlplaneClientStrategy(lifecycleCtx)
	transportBudget := cfg.PlatformTransportBudget
	if transportBudget == nil {
		// Copy limits, never mutable ownership, from default settings. The
		// same private root follows every window and replacement of this tunnel.
		limits := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
		transportBudget = connect.NewPlatformTransportBudget(limits.TotalByteCount, limits.MaxTransportCount)
	}
	generatorSettings := connect.DefaultApiMultiClientGeneratorSettings()
	generatorSettings.PlatformTransportSettingsGenerator = func() *connect.PlatformTransportSettings {
		settings := connect.DefaultPlatformTransportSettings()
		settings.PlatformTransportBudget = transportBudget
		return settings
	}

	generator := newApiMultiClientGenerator(
		lifecycleCtx,
		[]*connect.ProviderSpec{
			{ClientId: &providerClientId},
		},
		clientStrategy,
		// exclude self
		[]connect.Id{cfg.ClientId},
		cfg.ApiUrl,
		cfg.ByJwt,
		cfg.PlatformUrl,
		cfg.DeviceDescription,
		cfg.DeviceSpec,
		cfg.Version,
		&cfg.ClientId,
		func() *connect.ClientSettings {
			return providerTunnelClientSettings(cfg.ContractReservationByteCount)
		},
		generatorSettings,
	)

	// Do not hoist the TUN or its DoH cache to the Taskworker pass: every full
	// and blackhole provider probe needs an independently owned DNS cache and
	// admission state. The constructor creates that cache for this TUN alone.
	tun, err := createTun(dataCtx, inTunnelOnlyDnsResolverSettings())
	if err != nil {
		cancelData()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.closeTimeout())
		defer closeCancel()
		closeErr := generator.CloseAndWait(closeCtx)
		clientStrategy.Close()
		cancelLifecycle()
		return nil, errors.Join(
			fmt.Errorf("create tun: %w", err),
			wrapCloseError("generator", closeErr),
		)
	}

	multiClient := connect.NewRemoteUserNatMultiClient(
		dataCtx,
		generator,
		func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
			_, _ = tun.Write(packet)
		},
		protocol.ProvideMode_Network,
		providerTunnelMultiClientSettings(),
	)

	// Independent of every other context here on purpose: it must outlive a
	// read failure to say why, and it is ended by Close, not by the caller's
	// ctx -- the caller ending is its own signal, not the tunnel's loss.
	lost, lose := context.WithCancelCause(context.Background())
	unwatch := watchProviderPath(multiClient.Monitor(), lose)

	// pump tun -> provider
	source := connect.SourceId(cfg.ClientId)
	packetSendTimeout := cfg.packetSendTimeout()
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			packet, err := tun.Read()
			if err != nil {
				pumpStopped(dataCtx, err, lose)
				reportTunReadError(dataCtx, err, log.Println)
				return
			}
			multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, packetSendTimeout)
		}
	}()

	// The pin map is copied, not aliased: a caller that mutates its own map
	// after Open -- the cmd layer refreshes pins on a timer -- would otherwise
	// race the per-dial reads in DialTLSContext. The cmd layer already hands
	// over a fresh copy per Open, so this makes the package safe by
	// construction rather than by the caller remembering.
	pins := make(map[string][]string, len(cfg.Pins))
	for host, allowed := range cfg.Pins {
		pins[host] = append([]string(nil), allowed...)
	}

	return &Tunnel{
		cancelData:      cancelData,
		cancelLifecycle: cancelLifecycle,
		tun:             tun,
		multiClient:     multiClient,
		generator:       generator,
		clientStrategy:  clientStrategy,
		pumpDone:        pumpDone,
		pins:            pins,
		lost:            lost,
		lose:            lose,
		unwatch:         unwatch,
		closeTimeout:    cfg.closeTimeout(),
	}, nil
}

// Returns a context that is done once the tunnel's path to its provider is
// gone for good while the tunnel is open -- context.Cause says why, wrapping
// ErrTunnelLost -- and once the tunnel is closed, with ErrTunnelClosed. A probe's egresshealth.Path
// reports it as the current client's loss signal, so a run or check whose
// tunnel dies part-way re-creates it for the loads it had not finished,
// instead of failing them: a provider that disconnected mid-run did not refuse
// the sites the run had not reached yet.
func (self *Tunnel) Lost() context.Context {
	return self.lost
}

// Ends lost when the tunnel's provider, once added to the
// multi-client, is no longer added at all -- it disconnected, or its window
// client was reaped as dead -- and returns the unsubscribe.
//
// "No provider added" rather than any one removal, because a client past its
// lifetime is replaced make-before-break: the new client is added while the
// old one drains, so an ordinary rotation never empties the set. An empty set
// after the provider was up is the path going away. It is permanent for this
// tunnel even if the provider comes back a moment later: whatever a run
// measured across the gap is not a measurement of the exit, and re-running it
// costs less than charging a provider for a reconnect.
//
// A provider that never gets added at all is not a loss: the tunnel never had
// a path, and every load failing is then exactly what it looks like.
//
// The monitor delivers callbacks on a worker of their own, never under its
// state lock, so reading the current events from inside one is safe.
func watchProviderPath(monitor connect.MultiClientMonitor, lose context.CancelCauseFunc) func() {
	var wasAdded atomic.Bool
	return monitor.AddMonitorEventCallback(func(*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
		for _, event := range monitor.ProviderEvents() {
			if event.State.IsActive() {
				wasAdded.Store(true)
				return
			}
		}
		if wasAdded.Load() {
			lose(fmt.Errorf("%w: the provider left the multi-client and nothing was added in its place", ErrTunnelLost))
		}
	})
}

// Ends lost when the packet pump's read fails while the tunnel is
// still live: nothing more can reach the provider. A read that fails because
// Close canceled the data path first is teardown, not a loss.
func pumpStopped(dataCtx context.Context, err error, lose context.CancelCauseFunc) {
	if dataCtx.Err() == nil {
		lose(fmt.Errorf("%w: the packet pump's tun read failed: %w", ErrTunnelLost, err))
	}
}

// Returns a client whose every connection is dialed through the
// tunnel, with the configured certificate pins applied. Only the pinned hosts
// may be reached.
func (self *Tunnel) HttpClient(timeout time.Duration) *http.Client {
	return httpClientOverDialer(self.tun.DialContext, self.pins, timeout)
}

// Returns the HttpClient client widened by a set of additional hosts that
// are allowed without a certificate pin. The pinned hosts stay pinned exactly
// as before; extraHosts are reached under ordinary WebPKI chain verification
// (the same verification crypto/tls does for any https client -- MinVersion
// TLS 1.2, ServerName set, InsecureSkipVerify never set anywhere in this
// package).
//
// This exists for the egress-health probe (see egresshealth/), which checks
// that a provider carries traffic to a random sample of ~140 well-known
// internet destinations -- the built-in table or the server's pool -- after
// fetching the operator's own /ip echo. Those hosts are reached unpinned
// unless the server serves a pin for one, and the reason is that pinning them
// by default would make the signal worse, not better:
//
//   - The threat pinning defends against here is a provider forging a result.
//     Forging a pass, or the echo's answer, requires presenting a chain-valid
//     certificate for cloudflare-dns.com, www.amazon.com or the operator's own
//     api host -- i.e. compromising or mis-issuing from a public CA. A
//     provider cannot do that by being on the path, which is the capability
//     actually in question.
//
//   - Pinning ~140 additional hosts, whose leaves rotate on ~140 independent
//     schedules, would turn every routine certificate rotation into a
//     pin-mismatch failure. That failure would be indistinguishable, in the
//     recorded result, from the provider blackholing the destination -- the
//     exact fault this probe exists to detect. A maintenance chore that
//     manufactures false accusations against providers is worse than the
//     residual risk it removes.
//
// A host the server does serve a pin for keeps it: extraHosts never unpins a
// pinned host. The allowlist itself is preserved: a host that is neither
// pinned nor in extraHosts is still refused outright by DialTLSContext, so
// this widens the closed set rather than opening it.
func (self *Tunnel) HttpClientForHosts(timeout time.Duration, extraHosts []string) *http.Client {
	return httpClientOverDialerWithHosts(self.tun.DialContext, self.pins, extraHosts, timeout)
}

// Tears the tunnel down. It is safe to call more than once; only the
// first call has effect and its result is what every call returns.
func (self *Tunnel) Close() error {
	self.closeOnce.Do(func() {
		// Stop watching first: the teardown below removes the provider from
		// the multi-client, which is not the path being lost. Anything still
		// holding Lost learns the tunnel is closed instead.
		self.unwatch()
		self.lose(ErrTunnelClosed)
		ctx, cancel := context.WithTimeout(context.Background(), self.closeTimeout)
		defer cancel()
		self.closeErr = closeTunnelParts(
			ctx,
			self.cancelData,
			self.tun.Close,
			self.pumpDone,
			self.multiClient,
			self.generator,
			self.clientStrategy.Close,
			self.cancelLifecycle,
		)
	})
	return self.closeErr
}

// Makes every layer's ownership terminal. The multi-client
// owns packet dispatch and windows, but generated transfer Clients and their
// OOB transports remain owned by ApiMultiClientGenerator; closing only the
// former leaves asynchronous retirement tails behind each short-lived probe.
//
// Every close is attempted even after an earlier one reports an error. This is
// teardown: an error is useful to the caller, but it is not permission to leak
// the remaining owners.
func closeTunnelParts(
	ctx context.Context,
	cancelData context.CancelFunc,
	closeTun func() error,
	pumpDone <-chan struct{},
	multiClient closeAndWaiter,
	generator closeAndWaiter,
	closeClientStrategy func(),
	cancelLifecycle context.CancelFunc,
) error {
	// Stop packet admission first. The lifecycle context deliberately remains
	// live through generator retirement so its final authenticated
	// remove-client request can complete.
	cancelData()
	errList := []error{wrapCloseError("tun", closeTun())}
	select {
	case <-pumpDone:
	case <-ctx.Done():
		errList = append(errList, fmt.Errorf("wait for tun packet pump: %w", ctx.Err()))
	}
	errList = append(errList,
		wrapCloseError("multi-client", multiClient.CloseAndWait(ctx)),
		wrapCloseError("generator", generator.CloseAndWait(ctx)),
	)
	closeClientStrategy()
	cancelLifecycle()
	return errors.Join(errList...)
}

// Names the owner a close error came from; nil stays nil.
func wrapCloseError(owner string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close %s: %w", owner, err)
}

// The signature of net.Dialer.DialContext, which the tunnel's dialer has.
type dialContextFunc func(ctx context.Context, network string, address string) (net.Conn, error)

// Builds an http.Client that dials exclusively through
// dial and applies certificate pinning per pins.
//
// pins is a map, not a prebuilt *tls.Config: PinnedTlsConfig
// returns a template whose VerifyPeerCertificate closure reads the
// template's own ServerName field, not a clone's. Cloning it per host and
// mutating ServerName on the clone would copy the func value but leave it
// bound to the original (empty) ServerName -- the check would then find no
// pin entry and silently pass, defeating pinning entirely while ordinary CA
// validation kept working. Building a fresh config per host with
// PinnedTlsConfigForHost(pins, host) avoids that trap by construction: the
// verifier closes over host by value, not over any *tls.Config field.
func httpClientOverDialer(dial dialContextFunc, pins map[string][]string, timeout time.Duration) *http.Client {
	return httpClientOverDialerWithHosts(dial, pins, nil, timeout)
}

// Like httpClientOverDialer, with an additional set
// of allowed-but-unpinned hosts. See Tunnel.HttpClientForHosts for why the
// egress-health destinations are allowed unpinned, and why that is not a
// weakening of pinning: pinned hosts are unaffected, an unpinned host still
// gets full WebPKI chain verification, and a host in neither set is still
// refused.
func httpClientOverDialerWithHosts(dial dialContextFunc, pins map[string][]string, extraHosts []string, timeout time.Duration) *http.Client {
	// Normalized once, up front: this is the allowlist of the closed set of
	// hosts this tunnel is permitted to speak TLS to -- the pinned hosts plus
	// any explicitly-permitted unpinned hosts. Any https host not in this set
	// is refused below, in DialTLSContext -- see the ErrPinHostUnknown check.
	allowed := normalizePins(pins)
	for _, host := range extraHosts {
		host = normalizeHost(host)
		if host == "" {
			continue
		}
		if _, pinned := allowed[host]; pinned {
			// Already pinned: leave the pin set alone. An extra host must never
			// be able to remove a pin, or adding a name to the health table
			// would silently unpin a host the server chose to pin.
			continue
		}
		allowed[host] = nil
	}

	tr := &http.Transport{
		// The raw dialer is deliberately not installed as DialContext.
		// net/http uses DialContext for plain-http requests, which would
		// bypass both the allowlist and the pin check below -- they live in
		// DialTLSContext, and only https traffic reaches them. Refusing the
		// dial here kills the scheme before any bytes traverse the tunnel;
		// CheckRedirect (below) already refuses the downgrade-redirect route
		// to the same hole.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, &providerHttpStageError{stage: "policy", err: fmt.Errorf("%w (dial %s)", ErrPlainHttpRefused, addr)}
		},
		// TLSHandshakeTimeout is not set here: it only bounds the
		// transport's own internal TLS handshake, which never runs because
		// DialTLSContext (below) fully owns dialing and handshaking for
		// https requests. The timeout is instead applied explicitly to the
		// manual HandshakeContext call below, so the setting is real rather
		// than silently dead.
		//
		// IdleConnTimeout is not set here either: DisableKeepAlives is true,
		// so connections are never pooled or reused idle in the first
		// place -- there is nothing for an idle timeout to expire.
		DisableKeepAlives: true,

		// HTTP/2 is refused, explicitly rather than by accident.
		//
		// The bandwidth probe measures a provider by opening
		// bandwidth.StreamCount requests at once, because a single TCP flow
		// cannot exceed (connect's 1 MiB window / RTT) and N flows get N
		// windows. That only holds if N requests are N transport connections.
		// Multiplexed as N h2 streams over one connection they share one
		// window and the probe silently goes back to measuring 1 MiB / RTT --
		// with every test still green, because the requests are all still
		// made.
		//
		// Today h2 does not happen here for two separate incidental reasons:
		// this transport sets DialTLSContext, and http.Transport only attempts
		// h2 with a custom dialer when ForceAttemptHTTP2 is set; and
		// PinnedTlsConfigForHost offers no ALPN protocols, so no server can
		// select h2 anyway. Both are load-bearing for a measurement in another
		// package and neither says so where someone would change it. A
		// non-nil empty TLSNextProto is the direct statement, and it holds
		// regardless of what those two later become.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		ctx, done, err := beginProviderHttpDial(ctx)
		if err != nil {
			return nil, &providerHttpStageError{stage: "dial_dns_or_socket", err: err}
		}
		defer done()
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}

		// Allowlist-only: the set of hosts a probe dials is closed and known,
		// so any host outside it is refused outright rather than allowed to
		// connect at all. This is deliberately enforced
		// here, not in checkPin/PinnedTlsConfig (pinning.go), because
		// checkPin's "unpinned host passes" behavior is a documented,
		// tested contract other callers may rely on for a general-purpose
		// pinning verifier. This tunnel is the layer that actually knows
		// the endpoint set is closed, so it is where "unknown host" can
		// safely mean "reject" instead of "pass through".
		if _, pinned := allowed[normalizeHost(host)]; !pinned {
			return nil, &providerHttpStageError{stage: "policy", err: fmt.Errorf("%w: %s", ErrPinHostUnknown, host)}
		}

		raw, err := dial(ctx, network, addr)
		if err != nil {
			return nil, &providerHttpStageError{stage: "dial_dns_or_socket", err: err}
		}

		handshakeCtx := ctx
		if 0 < timeout {
			var cancel context.CancelFunc
			handshakeCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		cfg := PinnedTlsConfigForHost(pins, host)
		tlsConn := tls.Client(raw, cfg)
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			raw.Close()
			return nil, &providerHttpStageError{stage: "tls", err: err}
		}
		return tlsConn, nil
	}
	return &http.Client{
		Transport: &providerHttpTransport{Transport: tr},
		Timeout:   timeout,
		// A probe has no legitimate reason to follow a redirect: the echo
		// answers directly, and a destination that redirects declares the
		// redirect as its answer (egresshealth's ExpectStatus). Refusing
		// redirects closes off a provider MITM path where a host's response
		// 3xx's to a different host, or downgrades to plain http://, either
		// of which would be followed outside of the allowlist and any pin
		// check and could hand back a forged answer.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
