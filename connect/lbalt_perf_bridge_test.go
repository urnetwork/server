package connect

// The lb_alt front for the carrier cases of the performance harness
// (connect_perf_test.go, LBALT.md §9 "Performance"). The harness runs in
// package connect_test, which can't reach the fixture, so this test file
// exports what it needs and nothing more: a front over a real exchange, as
// newTestLbAltEnv builds it, and how a client reaches it on each carrier.
// None of it is production API.

import (
	"testing"

	"github.com/urnetwork/connect"
)

// One connect block fronting its loopback interfaces through connect's
// lb_alt front, over a real exchange.
type TestingLbAltFront struct {
	env *testLbAltEnv
}

// Starts the front, with the both-families layout. Runs inside a test env,
// with Postgres and Redis; everything closes with the test.
func Testing_NewLbAltFront(t testing.TB) *TestingLbAltFront {
	return &TestingLbAltFront{
		env: newTestLbAltEnv(t, nil),
	}
}

// How a client reaches the front's IPv4 interface address on a carrier mode,
// h1Plus choosing the urnetwork-framer/1 upgrade over the websocket on h1:
// the platform url, and new settings for its strategy and its transport,
// which the caller may adjust before it builds them.
func (self *TestingLbAltFront) Dial(mode connect.TransportMode, h1Plus bool) (string, *connect.ClientStrategySettings, *connect.PlatformTransportSettings) {
	address, _ := self.env.address(testLbAltInterfaceA, frontFamilyIpv4)
	carrier := testLbAltCarrier{
		name:   string(mode),
		mode:   mode,
		h1Plus: h1Plus,
	}
	return self.env.clientDialSettings(carrier, &testLbAltDial{address: address})
}
