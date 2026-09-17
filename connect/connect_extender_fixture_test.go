package connect

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	connectextender "github.com/urnetwork/connect/extender"
)

// The in-process extender the *WithExtender half of the connect matrix routes
// through.
//
// What it proves. Without it, every case in this file reaches the operator by
// dialing it directly, so nothing here covers the path a client takes when it
// cannot: reach an extender over whichever carrier gets through, and let the
// extender speak to the operator. That path carries real traffic in production
// and had no integration coverage.
//
// What it does NOT change. The extender relays over TCP (forwardNetwork in
// connect/extender), so the operator hop is always the h1 websocket, whatever
// the case's transport mode says. The transport mode selects the carrier of
// the CLIENT-to-extender hop instead. An h3 case with an extender therefore
// reads as "reach the extender over quic, then tcp to the operator", which is
// the shape the product actually ships, not "quic to the operator".

const (
	// defaultConnectCaseTimeout bounds a quiet case.
	defaultConnectCaseTimeout = 20 * time.Minute
	// chaosConnectCaseTimeout bounds a case that restarts residents under the
	// run. Chaos legitimately takes longer; failing it on the quiet budget
	// would report a defect that is not there.
	chaosConnectCaseTimeout = 40 * time.Minute
)

// testExtender is one extender plus the accounting that proves traffic went
// through it.
type testExtender struct {
	server *connectextender.ExtenderServer
	// config is what a client installs to dial this extender.
	config *connect.ExtenderConfig
	// forwards counts relayed connections. A *WithExtender case that finishes
	// with zero forwards did not use the extender, which is a failure however
	// green the rest of it looked -- the direct strategies being left on is
	// exactly how that happens.
	forwards atomic.Int64
	errors   chan error
}

// extenderConnectModeForTransport maps a case's transport mode to the carrier
// its client uses to REACH the extender.
//
// Auto maps to tcptls rather than being left to negotiate: with only an
// extender path available there is one carrier worth choosing, and pinning it
// keeps the case deterministic.
func extenderConnectModeForTransport(mode connect.TransportMode) connect.ExtenderConnectMode {
	switch mode {
	case connect.TransportModeH3:
		return connect.ExtenderConnectModeQuic
	case connect.TransportModeH3Dns, connect.TransportModeH3DnsPump:
		return connect.ExtenderConnectModeDns
	default:
		return connect.ExtenderConnectModeTcpTls
	}
}

// newTestExtender stands up an extender on loopback, allowed to forward to
// loopback only, and returns the config a client dials it with.
func newTestExtender(
	ctx context.Context,
	t testing.TB,
	mode connect.ExtenderConnectMode,
) *testExtender {
	t.Helper()

	extender := &testExtender{errors: make(chan error, 64)}

	// Bind the carrier port ourselves so the test knows it before the server
	// starts, and so a port collision fails here rather than inside the
	// server's bind loop.
	var port int
	switch mode {
	case connect.ExtenderConnectModeTcpTls:
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("extender tcp listener: %v", err)
		}
		port = listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatalf("release extender tcp probe: %v", err)
		}
	default:
		packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("extender udp listener: %v", err)
		}
		port = packetConn.LocalAddr().(*net.UDPAddr).Port
		if err := packetConn.Close(); err != nil {
			t.Fatalf("release extender udp probe: %v", err)
		}
	}

	settings := connectextender.DefaultExtenderSettings()
	settings.ErrorHandler = func(stage string, err error) {
		// Recorded, not failed: a client that retries a carrier produces
		// benign stage errors. The case fails on the forward count and on its
		// own assertions instead.
		select {
		case extender.errors <- fmt.Errorf("%s: %w", stage, err):
		default:
		}
	}
	forwardDialer := &net.Dialer{}
	counted := func(
		ctx context.Context, network string, address string,
	) (net.Conn, error) {
		conn, err := forwardDialer.DialContext(ctx, network, address)
		if err == nil {
			extender.forwards.Add(1)
		}
		return conn, err
	}
	// Both egresses, because an extender has two: a stream relay dials tcp and
	// the datagram relay dials udp. Counting only one made an h3 variant look
	// like it had bypassed the extender when it had in fact gone through it.
	settings.DialContext = counted
	settings.DialPacketContext = counted

	secret := fmt.Sprintf("connect-test-extender-%d", port)
	extender.server = connectextender.NewExtenderServer(
		ctx,
		[]string{secret},
		// Loopback only. The operator endpoints this suite creates all live
		// on 127.0.0.1, and an extender that would forward anywhere else in a
		// test is a liability.
		[]string{"127.0.0.1"},
		map[int][]connect.ExtenderConnectMode{port: {mode}},
		forwardDialer,
		settings,
	)
	go func() {
		_ = extender.server.ListenAndServe()
	}()

	extender.config = &connect.ExtenderConfig{
		Profile: connect.ExtenderProfile{
			ConnectMode: mode,
			// The extender presents a certificate for this name. It is never
			// resolved: the client dials Ip below.
			ServerName: "connect-test-extender.invalid",
			Port:       port,
		},
		Ip:     netip.MustParseAddr("127.0.0.1"),
		Secret: secret,
	}
	return extender
}

// applyTo points a client strategy's settings at this extender and turns the
// direct paths off.
//
// Disabling the normal and resilient strategies is the load-bearing half. With
// them left on the client dials the operator directly, the case passes, and it
// proves nothing about extenders -- so this is not an optimization, it is what
// makes the variant mean anything.
func (self *testExtender) applyTo(settings *connect.ClientStrategySettings) {
	settings.EnableNormal = false
	settings.EnableResilient = false
	// No discovery: this suite is hermetic, and an expand would reach for
	// extender profiles that do not exist here.
	settings.ExpandExtenderProfileCount = 0
	settings.ExtenderConfigs = []*connect.ExtenderConfig{self.config}
}

// assertCarriedTraffic fails the case when nothing was relayed.
func (self *testExtender) assertCarriedTraffic(t testing.TB) {
	t.Helper()
	if forwards := self.forwards.Load(); forwards <= 0 {
		t.Errorf(
			"the extender relayed no connections: the clients reached the operator " +
				"another way, so this variant proved nothing about extenders",
		)
	}
}
