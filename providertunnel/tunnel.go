// Package providertunnel builds an http.Client whose every request egresses
// through one specific urnetwork provider, so a geolocation lookup made with it
// reports that provider's egress location.
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
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/operator-proxy/controlplane"
)

// Config is the operator-provided identity and endpoints the prober uses to
// build tunnels. ClientId is the prober's own client id (so it can exclude
// itself when selecting providers); ByJwt is its network client jwt.
type Config struct {
	ApiURL            string
	PlatformURL       string
	ByJwt             string
	ClientId          connect.Id
	Pins              map[string][]string
	DeviceDescription string
	DeviceSpec        string
	Version           string
	// ContractReservationByteCount optionally bounds the per-contract size
	// ramp for a small probe. Zero preserves the shared transfer defaults.
	// This is a reservation target, not a traffic quota: packet-size floors
	// and the server's admission bounds still apply, and contracts renew.
	ContractReservationByteCount connect.ByteCount
}

// ErrPinsRequired is returned by Open when Config.Pins is nil or empty. The
// geolocation endpoint set this tunnel exists to probe is closed and known
// (see geolocate/sources.go), so a tunnel opened with no pins at all could
// never pin any of them -- it would carry a probe with pinning silently
// disabled for every request. Refusing to open is cheaper than discovering
// that later from a skewed geolocation result.
var ErrPinsRequired = errors.New("providertunnel: Config.Pins must not be empty; a tunnel with no pins cannot safely carry a geolocation probe")

// Negative reservation targets are malformed, not a request for defaults.
var ErrContractReservation = errors.New("providertunnel: contract reservation byte count must not be negative")

// ErrPlainHTTPRefused is returned for any http:// request made through a
// tunnel client. The allowlist and the certificate pins live in
// DialTLSContext, which only https traffic reaches; a plain-http request
// would ride the raw tunnel dialer in cleartext, forgeable by the very
// provider being measured.
var ErrPlainHTTPRefused = errors.New("providertunnel: plain http refused; only https, which the allowlist and pin check cover, may traverse the tunnel")

// createTun builds the gvisor tun Open routes through. It is a var, and takes
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
// provider tunnel uses the shared IPv4-only constructor.
var newControlplaneClientStrategy = controlplane.NewClientStrategy

// The fixed-provider tunnel is itself the instrument used by the outer
// geolocation/egress-health probe. Running RemoteUserNatMultiClient's ordinary
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

// inTunnelOnlyDnsResolverSettings returns DNS resolver settings under which
// every name the tunnel resolves is resolved THROUGH the tunnel, encrypted.
//
// This exists because connect.CreateTunWithDefaults (and CreateTun, which
// passes a nil resolver) inherits connect.DefaultDnsResolverSettings, whose
// EnableLocalDns is true: when in-tunnel DoH produces nothing within its
// budget -- a flaky provider, a provider that blocks 1.1.1.1:443, or simply a
// tunnel that has not finished coming up -- DohCache.resolve falls back to a
// plaintext port-53 query issued from the HOST's dialer. The TCP connection
// that follows still egresses through the tunnel, so the geolocation verdict
// stays correct, but the prober's own server IP has just asked a public
// resolver, in the clear, for "ipinfo.io". This operator tool must never emit
// a packet to (or about) a geolocation endpoint from its own address, so it
// fails closed instead: a probe that cannot resolve in-tunnel fails and is
// retried on the next pass.
//
// This mirrors connect's own DefaultUpgradeMuxSettings
// (connect/ip_mux_upgrade.go), which disables the same toggles for the same
// reason ("would resolve off-tunnel or in the clear"). Every Enable* toggle
// other than EnableRemoteDoh is left false, and the Local* server lists are
// left empty so that even a future connect default or an accidental toggle
// flip has no host-side resolver to reach for. RemoteDns* IS carried: it is
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

// Tunnel is a live data path pinned to exactly one provider. Every connection
// dialed through it egresses from that provider.
type Tunnel struct {
	cancelData      context.CancelFunc
	cancelLifecycle context.CancelFunc
	tun             *connect.Tun
	mc              *connect.RemoteUserNatMultiClient
	generator       *connect.ApiMultiClientGenerator
	clientStrategy  *connect.ClientStrategy
	pumpDone        <-chan struct{}
	pins            map[string][]string
	closeOnce       sync.Once
	closeErr        error
}

const tunnelCloseTimeout = 30 * time.Second

type closeAndWaiter interface {
	CloseAndWait(context.Context) error
}

// Open builds a tunnel that routes exclusively through providerClientId.
// It mirrors the proven construction in urnetwork/proxy socks/main.go: an api
// multiclient generator pinned to one ProviderSpec, a gvisor tun, and a packet
// pump in both directions.
func Open(ctx context.Context, cfg Config, providerClientId connect.Id) (*Tunnel, error) {
	if cfg.ContractReservationByteCount < 0 {
		return nil, ErrContractReservation
	}
	if len(cfg.Pins) == 0 {
		return nil, ErrPinsRequired
	}

	// The packet path and the control-plane lifecycle have separate child
	// contexts. Close must stop Tun traffic first, while keeping the API and
	// ClientStrategy alive long enough to retire the short-lived derived
	// network client. A single shared cancellation edge made every final
	// remove-client request start on an already-canceled strategy.
	lifecycleCtx, cancelLifecycle := context.WithCancel(ctx)
	dataCtx, cancelData := context.WithCancel(lifecycleCtx)
	clientStrategy := newControlplaneClientStrategy(lifecycleCtx)

	generator := connect.NewApiMultiClientGenerator(
		lifecycleCtx,
		[]*connect.ProviderSpec{
			{ClientId: &providerClientId},
		},
		clientStrategy,
		// exclude self
		[]connect.Id{cfg.ClientId},
		cfg.ApiURL,
		cfg.ByJwt,
		cfg.PlatformURL,
		cfg.DeviceDescription,
		cfg.DeviceSpec,
		cfg.Version,
		&cfg.ClientId,
		func() *connect.ClientSettings {
			return providerTunnelClientSettings(cfg.ContractReservationByteCount)
		},
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	tun, err := createTun(dataCtx, inTunnelOnlyDnsResolverSettings())
	if err != nil {
		cancelData()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), tunnelCloseTimeout)
		defer closeCancel()
		closeErr := generator.CloseAndWait(closeCtx)
		clientStrategy.Close()
		cancelLifecycle()
		return nil, errors.Join(
			fmt.Errorf("create tun: %w", err),
			wrapCloseError("generator", closeErr),
		)
	}

	mc := connect.NewRemoteUserNatMultiClient(
		dataCtx,
		generator,
		func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
			_, _ = tun.Write(packet)
		},
		protocol.ProvideMode_Network,
		providerTunnelMultiClientSettings(),
	)

	// pump tun -> provider
	source := connect.SourceId(cfg.ClientId)
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			packet, err := tun.Read()
			if err != nil {
				reportTunReadError(dataCtx, err, log.Println)
				return
			}
			mc.SendPacket(source, protocol.ProvideMode_Network, packet, 15*time.Second)
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
		mc:              mc,
		generator:       generator,
		clientStrategy:  clientStrategy,
		pumpDone:        pumpDone,
		pins:            pins,
	}, nil
}

// HTTPClient returns a client whose every connection is dialed through the
// tunnel, with the configured certificate pins applied. Only the pinned hosts
// may be reached.
func (t *Tunnel) HTTPClient(timeout time.Duration) *http.Client {
	return httpClientOverDialer(t.tun.DialContext, t.pins, timeout)
}

// HTTPClientForHosts is HTTPClient widened by a set of additional hosts that
// are allowed WITHOUT a certificate pin. The pinned hosts stay pinned exactly
// as before; extraHosts are reached under ordinary WebPKI chain verification
// (the same verification crypto/tls does for any https client -- MinVersion
// TLS 1.2, ServerName set, InsecureSkipVerify never set anywhere in this
// package).
//
// This exists for the egress-health probe (see egresshealth/), which checks
// that a provider carries traffic to a random sample of ~140 well-known
// internet destinations. Those
// destinations are deliberately NOT pinned, and the reason is that pinning them
// would make the signal worse, not better:
//
//   - The threat pinning defends against here is a provider forging a result.
//     For geolocation that is a live threat with a durable consequence: a forged
//     country is written to the database and served to users. For egress health,
//     forging a pass requires presenting a chain-valid certificate for
//     cloudflare-dns.com, www.amazon.com and so on -- i.e. compromising or
//     mis-issuing from a public CA. A provider cannot do that by being on the
//     path, which is the capability actually in question.
//
//   - Pinning ~140 additional hosts, whose leaves rotate on ~140 independent
//     schedules, would turn every routine certificate rotation into a
//     pin-mismatch failure. That failure would be indistinguishable, in the
//     recorded result, from the provider blackholing the destination -- the
//     exact fault this probe exists to detect. A maintenance chore that
//     manufactures false accusations against providers is worse than the
//     residual risk it removes.
//
// The allowlist itself is preserved: a host that is neither pinned nor in
// extraHosts is still refused outright by DialTLSContext, so this widens the
// closed set rather than opening it.
func (t *Tunnel) HTTPClientForHosts(timeout time.Duration, extraHosts []string) *http.Client {
	return httpClientOverDialerWithHosts(t.tun.DialContext, t.pins, extraHosts, timeout)
}

// Close tears the tunnel down. It is safe to call more than once; only the
// first call has effect and its result is what every call returns.
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), tunnelCloseTimeout)
		defer cancel()
		t.closeErr = closeTunnelParts(
			ctx,
			t.cancelData,
			t.tun.Close,
			t.pumpDone,
			t.mc,
			t.generator,
			t.clientStrategy.Close,
			t.cancelLifecycle,
		)
	})
	return t.closeErr
}

// closeTunnelParts makes every layer's ownership terminal. The multi-client
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

func wrapCloseError(owner string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close %s: %w", owner, err)
}

type dialContextFunc func(ctx context.Context, network string, address string) (net.Conn, error)

// httpClientOverDialer builds an http.Client that dials exclusively through
// dial and applies certificate pinning per pins.
//
// pins is a map, not a prebuilt *tls.Config: Task 1's PinnedTLSConfig
// returns a TEMPLATE whose VerifyPeerCertificate closure reads the
// template's own ServerName field, not a clone's. Cloning it per host and
// mutating ServerName on the clone would copy the func value but leave it
// bound to the original (empty) ServerName -- the check would then find no
// pin entry and silently pass, defeating pinning entirely while ordinary CA
// validation kept working. Building a fresh config per host with
// PinnedTLSConfigForHost(pins, host) avoids that trap by construction: the
// verifier closes over host by value, not over any *tls.Config field.
func httpClientOverDialer(dial dialContextFunc, pins map[string][]string, timeout time.Duration) *http.Client {
	return httpClientOverDialerWithHosts(dial, pins, nil, timeout)
}

// httpClientOverDialerWithHosts is httpClientOverDialer with an additional set
// of allowed-but-unpinned hosts. See Tunnel.HTTPClientForHosts for why the
// egress-health destinations are allowed unpinned, and why that is not a
// weakening of the geolocation guarantee: pinned hosts are unaffected, an
// unpinned host still gets full WebPKI chain verification, and a host in
// neither set is still refused.
func httpClientOverDialerWithHosts(dial dialContextFunc, pins map[string][]string, extraHosts []string, timeout time.Duration) *http.Client {
	// Normalized once, up front: this is the allowlist of the closed set of
	// hosts this tunnel is permitted to speak TLS to -- the pinned geolocation
	// endpoints (see geolocate/sources.go) plus any explicitly-permitted
	// unpinned hosts. Any https host not in this set is refused below, in
	// DialTLSContext -- see the ErrPinHostUnknown check.
	allowed := normalizePins(pins)
	for _, host := range extraHosts {
		host = normalizeHost(host)
		if host == "" {
			continue
		}
		if _, pinned := allowed[host]; pinned {
			// Already pinned: leave the pin set alone. An extra host must never
			// be able to REMOVE a pin, or adding a name to the health table
			// would silently unpin a geolocation endpoint.
			continue
		}
		allowed[host] = nil
	}

	tr := &http.Transport{
		// The raw dialer is deliberately NOT installed as DialContext.
		// net/http uses DialContext for plain-http requests, which would
		// bypass both the allowlist and the pin check below -- they live in
		// DialTLSContext, and only https traffic reaches them. Refusing the
		// dial here kills the scheme before any bytes traverse the tunnel;
		// CheckRedirect (below) already refuses the downgrade-redirect route
		// to the same hole.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, fmt.Errorf("%w (dial %s)", ErrPlainHTTPRefused, addr)
		},
		// TLSHandshakeTimeout is NOT set here: it only bounds the
		// transport's own internal TLS handshake, which never runs because
		// DialTLSContext (below) fully owns dialing and handshaking for
		// https requests. The timeout is instead applied explicitly to the
		// manual HandshakeContext call below, so the setting is real rather
		// than silently dead.
		//
		// IdleConnTimeout is NOT set here either: DisableKeepAlives is true,
		// so connections are never pooled or reused idle in the first
		// place -- there is nothing for an idle timeout to expire.
		DisableKeepAlives: true,

		// HTTP/2 is refused, explicitly rather than by accident.
		//
		// The bandwidth probe measures a provider by opening
		// bandwidth.StreamCount requests at once, because a single TCP flow
		// cannot exceed (connect's 1 MiB window / RTT) and N flows get N
		// windows. That only holds if N requests are N TRANSPORT connections.
		// Multiplexed as N h2 streams over one connection they share one
		// window and the probe silently goes back to measuring 1 MiB / RTT --
		// with every test still green, because the requests are all still
		// made.
		//
		// Today h2 does not happen here for two separate incidental reasons:
		// this transport sets DialTLSContext, and http.Transport only attempts
		// h2 with a custom dialer when ForceAttemptHTTP2 is set; and
		// PinnedTLSConfigForHost offers no ALPN protocols, so no server can
		// select h2 anyway. Both are load-bearing for a measurement in another
		// package and neither says so where someone would change it. A
		// non-nil empty TLSNextProto is the direct statement, and it holds
		// regardless of what those two later become.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}

		// Allowlist-only: the geolocation endpoint set is closed and known,
		// so any host without a pin-map entry is refused outright rather
		// than allowed to connect unpinned. This is deliberately enforced
		// here, not in checkPin/PinnedTLSConfig (pinning.go), because
		// checkPin's "unpinned host passes" behavior is a documented,
		// tested contract other callers may rely on for a general-purpose
		// pinning verifier. This tunnel is the layer that actually knows
		// the endpoint set is closed, so it is where "unknown host" can
		// safely mean "reject" instead of "pass through".
		if _, pinned := allowed[normalizeHost(host)]; !pinned {
			return nil, fmt.Errorf("%w: %s", ErrPinHostUnknown, host)
		}

		raw, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		handshakeCtx := ctx
		if timeout > 0 {
			var cancel context.CancelFunc
			handshakeCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		cfg := PinnedTLSConfigForHost(pins, host)
		tlsConn := tls.Client(raw, cfg)
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			raw.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// A geolocation probe has no legitimate reason to follow a
		// redirect: the three sources it talks to (geolocate/sources.go)
		// answer directly. Refusing redirects closes off a provider MITM
		// path where a pinned host's response 3xx's to a different host,
		// or downgrades to plain http://, either of which would be
		// followed outside of any pin check and could hand back a forged
		// location.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
