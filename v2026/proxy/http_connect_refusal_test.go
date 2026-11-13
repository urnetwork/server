package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	proxylib "github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// newHttpConnectTestManager admits CONNECTs without Redis or PostgreSQL: the
// ip-lock cache is seeded per id, and the tripwire context fails any
// PostgreSQL access the admission path would make.
func newHttpConnectTestManager(t testing.TB) *ProxyDeviceManager {
	t.Helper()
	ctx := server.WithoutPostgres(context.Background())
	manager := NewProxyDeviceManager(ctx, &ProxyDeviceManagerSettings{
		CheckProxyDeviceIdleTimeout:    time.Minute,
		NetworkSpace:                   &sdk.NetworkSpace{},
		ProxyLockCacheTtl:              30 * time.Second,
		ProxyLockCacheMaxEntries:       32,
		DeviceCreationRetryTimeout:     time.Second,
		CreationFailureSweepMinEntries: 1024,
	})
	t.Cleanup(func() {
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return manager
}

// seedHttpConnectTestLock installs one proxy id's ip lock as ValidCaller
// would have loaded it.
func seedHttpConnectTestLock(manager *ProxyDeviceManager, proxyId server.Id, found bool, lockSubnets ...netip.Prefix) {
	now := time.Now()
	manager.lockCache.put(proxyId, proxyLockEntry{
		found:       found,
		lockSubnets: lockSubnets,
		expiry:      now.Add(time.Hour),
	}, now)
}

// newHttpConnectTestProxy is the production http proxy and dial callback
// around the manager.
func newHttpConnectTestProxy(manager *ProxyDeviceManager) *proxylib.HttpProxy {
	return (&httpServer{
		ctx:                manager.ctx,
		proxyDeviceManager: manager,
		settings:           DefaultProxySettings(),
	}).newHttpProxy()
}

// disableHttpConnectTestPlanEnforcement keeps the plan check free of i/o.
func disableHttpConnectTestPlanEnforcement(t testing.TB) {
	config := model.Pro()
	previous := config.EnforceFeatures
	config.EnforceFeatures = false
	t.Cleanup(func() { config.EnforceFeatures = previous })
}

func httpConnectTestBasic(proxyId server.Id) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(model.SignProxyId(proxyId)+":x"))
}

// A CONNECT the proxy can never serve is answered at once with its class's
// status and dialed once. The loop retried unauthorized and unknown ids every
// second for as long as the client stayed, re-running admission each time.
func TestHttpConnectRefusalsAreAnsweredWithoutRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		disableHttpConnectTestPlanEnforcement(t)
		manager := newHttpConnectTestManager(t)
		unknownId, lockedId, removedId := server.NewId(), server.NewId(), server.NewId()
		seedHttpConnectTestLock(manager, unknownId, false)
		seedHttpConnectTestLock(manager, lockedId, true, netip.MustParsePrefix("203.0.113.0/24"))
		// the ip-lock cache outlives a removed device's configuration
		seedHttpConnectTestLock(manager, removedId, true)
		var builds atomic.Int32
		manager.proxyDeviceBuilder = func(proxyId server.Id) (*ProxyDevice, error) {
			builds.Add(1)
			if proxyId == removedId {
				return nil, errProxyDeviceNotFound
			}
			return nil, fmt.Errorf("unexpected device construction for %s", proxyId)
		}
		core := newHttpConnectTestProxy(manager)
		origin := httptest.NewServer(core)
		defer origin.Close()

		cases := []struct {
			name          string
			authorization string
			statusCode    int
		}{
			{"no credential", "", http.StatusProxyAuthRequired},
			{"unknown proxy", httpConnectTestBasic(unknownId), http.StatusBadGateway},
			{"outside the ip lock", httpConnectTestBasic(lockedId), http.StatusForbidden},
			{"removed device", httpConnectTestBasic(removedId), http.StatusBadGateway},
		}
		for _, c := range cases {
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(origin.URL, "http://"), 2*time.Second)
			if err != nil {
				t.Fatalf("%s: dial proxy: %v", c.name, err)
			}
			start := time.Now()
			conn.SetDeadline(start.Add(2 * time.Second))
			request := "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n"
			if c.authorization != "" {
				request += "Proxy-Authorization: " + c.authorization + "\r\n"
			}
			fmt.Fprint(conn, request+"\r\n")
			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
			if err != nil {
				conn.Close()
				t.Fatalf("%s: the CONNECT was not answered while the client stayed: %v", c.name, err)
			}
			response.Body.Close()
			conn.Close()
			if response.StatusCode != c.statusCode {
				t.Fatalf("%s: status = %d, want %d", c.name, response.StatusCode, c.statusCode)
			}
			if elapsed := time.Since(start); 500*time.Millisecond < elapsed {
				t.Fatalf("%s: answered after %s; a refusal must not wait out a backoff", c.name, elapsed)
			}
		}

		// past the first backoff: a retry would have counted another dial error
		time.Sleep(1500 * time.Millisecond)
		stats := core.Stats()
		if stats.ConnectDialErrors != 4 ||
			stats.DialRefusedUnauthenticated != 1 ||
			stats.DialRefusedForbidden != 1 ||
			stats.DialRefusedUnknownProxy != 2 ||
			stats.ConnectTimeouts != 0 {
			t.Fatalf("stats %+v: want one dial and one refusal per CONNECT", stats)
		}
		if got := builds.Load(); got != 1 {
			t.Fatalf("%d device constructions, want 1 (the removed device's)", got)
		}
		if got := server.PacketPostgresAttempts(manager.ctx); got != 0 {
			t.Fatalf("refused admissions attempted PostgreSQL %d times", got)
		}
	})
}

// The SNI form of the credential, with and without a domain, reaches the same
// classification as the header form.
func TestHttpConnectSniProxyIdIsClassified(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		manager := newHttpConnectTestManager(t)
		unknownId := server.NewId()
		seedHttpConnectTestLock(manager, unknownId, false)
		core := newHttpConnectTestProxy(manager)
		signed := strings.ToLower(model.SignProxyId(unknownId))
		for _, serverName := range []string{signed + ".proxy.example", signed} {
			r := httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			r.TLS = &tls.ConnectionState{ServerName: serverName}
			_, err := core.ConnectDialContextWithRequest(context.Background(), r, "tcp", "example.com:443")
			var refusal *proxylib.ConnectRefusal
			if !errors.As(err, &refusal) || refusal.Class != proxylib.ConnectRefusalUnknownProxy {
				t.Fatalf("SNI %q: err = %v, want an unknown-proxy refusal", serverName, err)
			}
		}
		r := httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.TLS = &tls.ConnectionState{ServerName: "not-a-signed-id.proxy.example"}
		_, err := core.ConnectDialContextWithRequest(context.Background(), r, "tcp", "example.com:443")
		var refusal *proxylib.ConnectRefusal
		if !errors.As(err, &refusal) || refusal.Class != proxylib.ConnectRefusalUnauthenticated {
			t.Fatalf("unsigned SNI: err = %v, want an unauthenticated refusal", err)
		}
	})
}

// httpConnectTestTun is a device tun whose outbound queue is read by the test.
func httpConnectTestTun(t testing.TB) *connect.Tun {
	t.Helper()
	tun, err := connect.CreateTunWithResolver(context.Background(), proxyDeviceTunSettings(DefaultProxyDeviceSettings()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tun.Close() })
	return tun
}

// A device window with no provider cannot carry a dial, so the dial waits for
// one, within the caller's budget, and opens no stack connection meanwhile.
// It used to dial into the empty window and park a connection behind
// retransmitted SYNs, and its CONNECT polled it every second.
func TestHttpConnectWaitsForARouteBeforeDialing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		disableHttpConnectTestPlanEnforcement(t)
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		tun := httpConnectTestTun(t)
		source := newProxyDeviceReadinessTestSource(false)
		deviceCtx, cancelDevice := context.WithCancel(manager.ctx)
		defer cancelDevice()
		manager.proxyDevices[proxyId] = &proxyDeviceState{ProxyDevice: &ProxyDevice{
			ctx:         deviceCtx,
			cancel:      cancelDevice,
			deviceState: source,
			tun:         tun,
			settings:    DefaultProxyDeviceSettings(),
		}}
		core := newHttpConnectTestProxy(manager)
		dial := func(wait time.Duration) error {
			r := httptest.NewRequest(http.MethodConnect, "https://192.0.2.1:443", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Proxy-Authorization", httpConnectTestBasic(proxyId))
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			conn, err := core.ConnectDialContextWithRequest(ctx, r, "tcp", "192.0.2.1:443")
			if conn != nil {
				conn.Close()
			}
			return err
		}
		openings := func() uint64 {
			return tun.Stats().TCP.ActiveConnectionOpenings.Value()
		}

		start := time.Now()
		err := dial(300 * time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("dial into a window with no provider = %v, want the caller's deadline", err)
		}
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
			t.Fatalf("returned after %s, before the caller's deadline", elapsed)
		}
		if got := openings(); got != 0 {
			t.Fatalf("a window with no provider opened %d stack connections", got)
		}

		// one provider routes even below the window's minimum
		source.mutex.Lock()
		source.status.ProviderStateAdded = 1
		source.mutex.Unlock()
		_ = dial(300 * time.Millisecond)
		if got := openings(); got == 0 {
			t.Fatal("a routable window did not dial")
		}
	})
}

// tcpResetForSyn answers a SYN the way a refusing destination or the client
// security policy does: RST+ACK, addressed back to the dialer.
func tcpResetForSyn(syn []byte) ([]byte, bool) {
	if len(syn) < header.IPv4MinimumSize || header.IPVersion(syn) != header.IPv4Version {
		return nil, false
	}
	ip := header.IPv4(syn)
	if ip.TransportProtocol() != header.TCPProtocolNumber || len(ip.Payload()) < header.TCPMinimumSize {
		return nil, false
	}
	tcp := header.TCP(ip.Payload())
	if tcp.Flags() != header.TCPFlagSyn {
		return nil, false
	}
	packet := make([]byte, header.IPv4MinimumSize+header.TCPMinimumSize)
	reply := header.IPv4(packet)
	reply.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(packet)),
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     ip.DestinationAddress(),
		DstAddr:     ip.SourceAddress(),
	})
	reply.SetChecksum(^reply.CalculateChecksum())
	replyTcp := header.TCP(packet[header.IPv4MinimumSize:])
	replyTcp.Encode(&header.TCPFields{
		SrcPort:    tcp.DestinationPort(),
		DstPort:    tcp.SourcePort(),
		AckNum:     tcp.SequenceNumber() + 1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagRst | header.TCPFlagAck,
	})
	checksum := header.PseudoHeaderChecksum(
		header.TCPProtocolNumber,
		reply.SourceAddress(),
		reply.DestinationAddress(),
		uint16(header.TCPMinimumSize),
	)
	replyTcp.SetChecksum(^replyTcp.CalculateChecksum(checksum))
	return packet, true
}

// A dial every attempt of which was reset, and a name that does not exist,
// are refusals. Everything else stays retryable. The reset is the real tun's
// error for a SYN answered with RST, as a refusing destination or the client
// security policy answers it.
func TestConnectDialRefusalClassifiesTunFailures(t *testing.T) {
	tun := httpConnectTestTun(t)
	go func() {
		for {
			packet, err := tun.Read()
			if err != nil {
				return
			}
			if reset, ok := tcpResetForSyn(packet); ok {
				tun.Write(reset)
			}
			connect.MessagePoolReturn(packet)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resetErr := tun.DialContext(ctx, "tcp", "192.0.2.1:25")
	if resetErr == nil {
		t.Fatal("a reset dial succeeded")
	}
	if errors.Is(resetErr, context.DeadlineExceeded) || errors.Is(resetErr, os.ErrDeadlineExceeded) {
		t.Fatalf("the tun did not see its reset: %v", resetErr)
	}

	refusedAttempt := &net.OpError{Op: "connect", Net: "tcp", Err: fmt.Errorf("%s", &tcpip.ErrConnectionRefused{})}
	notFound := &net.DNSError{Err: "no such host", Name: "nothing.invalid", IsNotFound: true}
	cases := []struct {
		name  string
		err   error
		class proxylib.ConnectRefusalClass
	}{
		{"reset by the tun", resetErr, proxylib.ConnectRefusalReset},
		{"every address reset", errors.Join(refusedAttempt, refusedAttempt), proxylib.ConnectRefusalReset},
		{"no such host", notFound, proxylib.ConnectRefusalNoSuchHost},
		{"no such host, datagram form", fmt.Errorf("Could not resolve nothing.invalid:53: %w", notFound), proxylib.ConnectRefusalNoSuchHost},
		{"one address reset, one not", errors.Join(refusedAttempt, errors.New("Could not create tcp endpoint")), 0},
		{"temporary resolution failure", &net.DNSError{Err: "DoH resolution failed", Name: "x.example", IsTemporary: true}, 0},
		{"dial timeout", os.ErrDeadlineExceeded, 0},
		{"caller gone", context.Canceled, 0},
	}
	for _, c := range cases {
		err := connectDialRefusal(c.err)
		var refusal *proxylib.ConnectRefusal
		isRefusal := errors.As(err, &refusal)
		if c.class == 0 {
			if isRefusal || err != c.err {
				t.Fatalf("%s: %v became %v; a retryable error must pass through", c.name, c.err, err)
			}
			continue
		}
		if !isRefusal || refusal.Class != c.class || !errors.Is(err, c.err) {
			t.Fatalf("%s: %v became %v, want refusal class %d", c.name, c.err, err, c.class)
		}
	}
}
