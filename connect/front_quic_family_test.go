package connect

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Each accepted QUIC connection counts under its socket group's address
// family as well as its result, so the families' handshake success can be
// compared: a real connection over each family the interface has counts once
// under that family and its result, and not under the other family.
func TestFrontQuicCountsConnectionsByAddressFamily(t *testing.T) {
	var handshakes chan testFrontQuicHandshake
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		handshakes = observeTestFrontQuicHandshakes(env)
	}, http.NotFoundHandler())

	families := 0
	for _, family := range []string{frontFamilyIpv4, frontFamilyIpv6} {
		address, ok := env.address(testFrontInterfaceA, family)
		if !ok {
			continue
		}
		families += 1
		otherFamily := frontFamilyIpv6
		if family == frontFamilyIpv6 {
			otherFamily = frontFamilyIpv4
		}
		counter := frontQuicConnectionsByFamilyCounter.WithLabelValues(frontPortQuic, family, frontQuicResultConnect)
		otherCounter := frontQuicConnectionsByFamilyCounter.WithLabelValues(frontPortQuic, otherFamily, frontQuicResultConnect)
		before := testutil.ToFloat64(counter)
		otherBefore := testutil.ToFloat64(otherCounter)

		what := fmt.Sprintf("a connect connection over %s", family)
		target := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(env.bindAddress(address, frontPortQuic)))
		conn, err := env.dialQuicToWithConfig(env.clientPacketConn(address), target, frontPortQuic, env.clientTlsConfig(testFrontConnectName, nil), testFrontClientQuicConfig())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		nextTestFrontObserved(t, handshakes, what)
		env.nextHandled()
		conn.CloseWithError(0, "")

		if after := testutil.ToFloat64(counter); after != before+1 {
			t.Fatalf("%s counted %v times under %s", what, after-before, family)
		}
		if otherAfter := testutil.ToFloat64(otherCounter); otherAfter != otherBefore {
			t.Fatalf("%s counted %v times under %s", what, otherAfter-otherBefore, otherFamily)
		}
	}
	if families == 0 {
		t.Fatal("the fixture's interface has no address in either family")
	}
}
