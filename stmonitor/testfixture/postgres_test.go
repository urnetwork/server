// Pure checks of the fixture's administration URL validation; no URL here is
// ever dialed.
package testfixture

import (
	"net"
	"strings"
	"testing"
)

// A plain test run on a host that also runs the production data tunnel must
// not take a loopback tunnel port for its disposable fixture: CREATE ROLE and
// CREATE DATABASE would run on production. Only a loopback-only network, such
// as the SN runner's isolated namespace, may use those ports.
func TestAdminUrlRefusesLoopbackTunnelPortsOutsideLoopbackOnlyNetwork(t *testing.T) {
	cases := []struct {
		raw          string
		loopbackOnly bool
		errorText    string
	}{
		{raw: "postgres://admin:synthetic@127.0.0.1:5432/postgres", errorText: "loopback port 5432, which a production data tunnel forwards"},
		{raw: "postgres://admin:synthetic@127.0.0.1:6432/postgres", errorText: "loopback port 6432"},
		{raw: "postgres://admin:synthetic@127.0.0.1:6379/postgres", errorText: "loopback port 6379"},
		{raw: "postgres://admin:synthetic@127.0.0.1:6427/postgres", errorText: "loopback port 6427"},
		{raw: "postgres://admin:synthetic@[::1]:5432/postgres", errorText: "loopback port 5432"},
		{raw: "postgres://admin:synthetic@127.0.0.1:5432/postgres", loopbackOnly: true},
		{raw: "postgres://admin:synthetic@127.0.0.1:35432/postgres"},
		{raw: "postgres://admin:synthetic@127.0.0.1:6428/postgres"},
		{raw: "postgres://admin:synthetic@192.0.2.1:35432/postgres", errorText: "explicitly disposable loopback"},
		{raw: "postgres://admin:synthetic@db.fixture.example:35432/postgres", errorText: "explicitly disposable loopback"},
		{raw: "postgres://admin:synthetic@127.0.0.1/postgres", errorText: "explicitly disposable loopback"},
		{raw: "", errorText: "explicitly disposable loopback"},
	}
	for _, c := range cases {
		value, err := adminUrl(c.raw, c.loopbackOnly)
		if c.errorText == "" {
			if err != nil || value == nil {
				t.Errorf("%q (loopback only %t) refused: %v", c.raw, c.loopbackOnly, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.errorText) {
			t.Errorf("%q (loopback only %t) = %v; want a refusal containing %q", c.raw, c.loopbackOnly, err, c.errorText)
		}
	}
}

// A network is loopback only when every address on an up interface is
// loopback; one other address, such as a launcher alias or a link-local
// address, could carry a tunnel.
func TestLoopbackOnlyAddrs(t *testing.T) {
	cidr := func(value string) net.Addr {
		ip, network, err := net.ParseCIDR(value)
		if err != nil {
			t.Fatal(err)
		}
		network.IP = ip
		return network
	}
	if !loopbackOnlyAddrs([]net.Addr{cidr("127.0.0.1/8"), cidr("::1/128")}) {
		t.Error("loopback addresses were not taken for a loopback-only network")
	}
	for _, other := range []string{"192.0.2.5/24", "10.213.0.1/32", "fe80::1/64", "2001:db8::5/64"} {
		if loopbackOnlyAddrs([]net.Addr{cidr("127.0.0.1/8"), cidr(other)}) {
			t.Errorf("a network with %s was taken for loopback only", other)
		}
	}
}
