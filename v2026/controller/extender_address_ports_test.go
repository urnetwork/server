package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The ports of each extender address (connect/EXTENDER.md S10). A router's
// mapping forwards the ipv4 carriers on ports it picks, while the ipv6 address
// answers on the bound ones. The record held one port per carrier for every
// address, so the v6 address was published on the router's port, and the
// probes dialed one family on the other's port.

// The record names an address's own ports where they are not the record's, and
// none where they are, so an extender whose addresses share their ports signs
// the record it always did.
func TestSignExtenderRecordNamesTheAddressPorts(t *testing.T) {
	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := &ExtenderConfig{
		NetworkHost: testExtenderNetworkHost,
	}
	issueTime := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	sign := func(extender *model.NetworkExtender, addresses ...*model.NetworkExtenderAddress) *protocol.ExtenderRecordBody {
		t.Helper()
		extender.ExtenderId = server.NewId()
		extender.PublicKey = rootPublicKey
		extender.DnsPort = connect.ExtenderDnsPort
		extender.DnsTld = connect.DefaultExtenderDnsTld
		record, _, err := SignExtenderRecord(config, rootPrivateKey, extender, addresses, issueTime)
		if err != nil {
			t.Fatal(err)
		}
		body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	address := func(ip string, tcpPort int, udpPort int, dnsPorts ...int) *model.NetworkExtenderAddress {
		addr := netip.MustParseAddr(ip)
		ipVersion := 6
		if addr.Is4() {
			ipVersion = 4
		}
		return &model.NetworkExtenderAddress{
			IpVersion: ipVersion,
			Ip:        addr,
			Carriers:  []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns},
			DnsPorts:  dnsPorts,
			Active:    true,
			TcpPort:   tcpPort,
			UdpPort:   udpPort,
		}
	}
	expectPorts := func(what string, recordAddress *protocol.ExtenderAddress, tcpPort int, udpPort int, dnsPorts ...uint32) {
		t.Helper()
		if int(recordAddress.TcpPort) != tcpPort || int(recordAddress.UdpPort) != udpPort ||
			!slices.Equal(recordAddress.DnsPorts, dnsPorts) {
			t.Fatalf("%s %s names %d/%d/%v, expected %d/%d/%v",
				what, recordAddress.Ip,
				recordAddress.TcpPort, recordAddress.UdpPort, recordAddress.DnsPorts,
				tcpPort, udpPort, dnsPorts)
		}
	}

	// the v6 activation was the latest: the record's ports are its, and the v4
	// address the router forwards names the router's
	body := sign(
		&model.NetworkExtender{TcpPort: 443, UdpPort: 443},
		address("192.0.2.7", 40443, 40444, 40053),
		address("2001:db8::7", 443, 443, connect.ExtenderDnsPort),
	)
	connect.AssertEqual(t, int(body.TcpPort), 443)
	connect.AssertEqual(t, int(body.UdpPort), 443)
	if !slices.Equal(body.DnsPorts, []uint32{connect.ExtenderDnsPort, 40053}) {
		t.Fatalf("record dns ports = %v, expected the union", body.DnsPorts)
	}
	expectPorts("v6 latest", body.Addresses[0], 40443, 40444, 40053)
	expectPorts("v6 latest", body.Addresses[1], 0, 0, connect.ExtenderDnsPort)

	// the v4 activation was the latest: the v6 address names the bound ports
	body = sign(
		&model.NetworkExtender{TcpPort: 40443, UdpPort: 40444},
		address("192.0.2.7", 40443, 40444, 40053),
		address("2001:db8::7", 443, 443, connect.ExtenderDnsPort),
	)
	connect.AssertEqual(t, int(body.TcpPort), 40443)
	connect.AssertEqual(t, int(body.UdpPort), 40444)
	expectPorts("v4 latest", body.Addresses[0], 0, 0, 40053)
	expectPorts("v4 latest", body.Addresses[1], 443, 443, connect.ExtenderDnsPort)

	// shared ports, and an address activated before its ports were kept, name
	// none: the record of before
	body = sign(
		&model.NetworkExtender{TcpPort: 443, UdpPort: 443},
		address("192.0.2.7", 443, 443, connect.ExtenderDnsPort),
		address("2001:db8::7", 0, 0, connect.ExtenderDnsPort),
	)
	for _, recordAddress := range body.Addresses {
		expectPorts("shared", recordAddress, 0, 0)
	}
}

// Every answer says the operator keeps each address's ports, a refusal too, so
// an extender offers each family its own ports only to an operator that keeps
// them: one that predates them would sign each family on the other's.
func TestExtenderActivateAnswersThatItKeepsAddressPorts(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{})
	clientSession.ClientAddress = "127.0.0.1:54321"
	result, err := ExtenderActivate(&ExtenderActivateArgs{Carriers: []string{connect.ExtenderCarrierTcp}}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if result.Activated || !result.AddressPorts {
		t.Fatalf("activated %t, address ports %t, expected a refusal that says the operator keeps them",
			result.Activated, result.AddressPorts)
	}
	answer, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(answer), `"address_ports":true`) {
		t.Fatalf("the answer %s does not carry address_ports", answer)
	}
}

// An address is measured only where it serves tcp on 443 (decision 4), judged
// by its own port: a v4 address the router forwards on 40443 is left out
// whichever family activated last, and the v6 address on 443 is measured.
func TestExtenderMeasurementEligibleAddressesJudgesEachAddressPort(t *testing.T) {
	v4 := testMeasurementAddress("203.0.113.7", connect.ExtenderCarrierTcp)
	v4.TcpPort = 40443
	v6 := testMeasurementAddress("2001:db8::7", connect.ExtenderCarrierTcp)
	v6.TcpPort = ExtenderMeasurementPort
	for _, latestTcpPort := range []int{ExtenderMeasurementPort, 40443} {
		extender := testMeasurementExtender()
		extender.TcpPort = latestTcpPort
		ips, reason := ExtenderMeasurementEligibleAddresses(extender, []*model.NetworkExtenderAddress{v4, v6})
		connect.AssertEqual(t, reason, "")
		if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("2001:db8::7")}) {
			t.Fatalf("with the latest activation on %d, measured at %v, expected the v6 address alone", latestTcpPort, ips)
		}
	}

	// every address on another port is not measured, and says why
	extender := testMeasurementExtender()
	ips, reason := ExtenderMeasurementEligibleAddresses(extender, []*model.NetworkExtenderAddress{v4})
	if ips != nil || reason != "the extender serves tcp on 40443, not 443" {
		t.Fatalf("measured at %v (%q), expected none on 40443", ips, reason)
	}

	// an address activated before its port was kept is on the extender's
	legacy := testMeasurementAddress("203.0.113.8", connect.ExtenderCarrierTcp)
	ips, reason = ExtenderMeasurementEligibleAddresses(testMeasurementExtender(), []*model.NetworkExtenderAddress{legacy})
	connect.AssertEqual(t, reason, "")
	if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("203.0.113.8")}) {
		t.Fatalf("measured at %v, expected the address on the extender's 443", ips)
	}
}

// Each family's activation keeps the ports it was probed on: the v4 address on
// the router's 40443, 40444 and 40053, the v6 address on the bound 443, 443
// and 4053. The record names the v4 address's own, and the uptime probe dials
// each address on its own tcp port. A later v4 activation on a moved port moves
// the v4 address alone.
func TestExtenderActivateKeepsEachFamilysPorts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPrivateKey := installTestExtenderConfig(t, nil)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = testFakeExtenderProbes(nil)
		account := newTestExtenderAccount(ctx)
		host := account.newHost(t, ctx)
		activate := func(clientIp string, tcpPort int, udpPort int, dnsPort int) *protocol.ExtenderRecordBody {
			t.Helper()
			result, err := service.activate(&ExtenderActivateArgs{
				PublicKeyHex: hex.EncodeToString(host.publicKey),
				TcpPort:      tcpPort,
				UdpPort:      udpPort,
				DnsPort:      dnsPort,
				DnsPorts:     []int{dnsPort},
				Carriers: []string{
					connect.ExtenderCarrierTcp,
					connect.ExtenderCarrierQuic,
					connect.ExtenderCarrierDns,
				},
			}, host.session(ctx, clientIp))
			if err != nil {
				t.Fatal(err)
			}
			if !result.Activated {
				t.Fatalf("the activation from %s was refused: %s", clientIp, result.Error)
			}
			if !result.AddressPorts {
				t.Fatalf("the activation from %s does not say the operator keeps each address's ports", clientIp)
			}
			return verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		}
		expectRecordPorts := func(what string, body *protocol.ExtenderRecordBody, ip string, tcpPort int, udpPort int, dnsPorts ...uint32) {
			t.Helper()
			for _, recordAddress := range body.Addresses {
				if recordAddress.Ip != ip {
					continue
				}
				if int(recordAddress.TcpPort) != tcpPort || int(recordAddress.UdpPort) != udpPort ||
					!slices.Equal(recordAddress.DnsPorts, dnsPorts) {
					t.Fatalf("%s: the record names %s on %d/%d/%v, expected %d/%d/%v",
						what, ip,
						recordAddress.TcpPort, recordAddress.UdpPort, recordAddress.DnsPorts,
						tcpPort, udpPort, dnsPorts)
				}
				return
			}
			t.Fatalf("%s: the record does not name %s", what, ip)
		}
		expectProbePorts := func(what string, expected map[string]int) {
			t.Helper()
			probed := map[string]int{}
			for _, target := range model.GetActiveNetworkExtenderProbeTargets(ctx) {
				if string(target.PublicKey) == string(host.publicKey) {
					probed[target.Ip.String()] = target.TcpPort
				}
			}
			for ip, tcpPort := range expected {
				if probed[ip] != tcpPort {
					t.Fatalf("%s: the uptime probe dials %s on %d, expected %d", what, ip, probed[ip], tcpPort)
				}
			}
		}

		activate("198.51.100.7", 40443, 40444, 40053)
		body := activate("2001:db8::7", 443, 443, connect.ExtenderDnsPort)
		// the v6 activation was the latest, so the record's ports are its
		connect.AssertEqual(t, int(body.TcpPort), 443)
		connect.AssertEqual(t, int(body.UdpPort), 443)
		connect.AssertEqual(t, len(body.Addresses), 2)
		expectRecordPorts("both families", body, "198.51.100.7", 40443, 40444, 40053)
		expectRecordPorts("both families", body, "2001:db8::7", 0, 0, connect.ExtenderDnsPort)
		expectProbePorts("both families", map[string]int{"198.51.100.7": 40443, "2001:db8::7": 443})

		// the router moved the v4 mapping, and the v4 activation posts it
		body = activate("198.51.100.7", 41443, 40444, 40053)
		connect.AssertEqual(t, int(body.TcpPort), 41443)
		expectRecordPorts("the v4 move", body, "198.51.100.7", 0, 0, 40053)
		expectRecordPorts("the v4 move", body, "2001:db8::7", 443, 443, connect.ExtenderDnsPort)
		expectProbePorts("the v4 move", map[string]int{"198.51.100.7": 41443, "2001:db8::7": 443})

		var extenderId server.Id
		for _, target := range model.GetActiveNetworkExtenderProbeTargets(ctx) {
			if string(target.PublicKey) == string(host.publicKey) {
				extenderId = target.ExtenderId
			}
		}
		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		for _, address := range stored.Addresses {
			expectedTcpPort, expectedUdpPort := 41443, 40444
			if address.IpVersion == 6 {
				expectedTcpPort, expectedUdpPort = 443, 443
			}
			if address.TcpPort != expectedTcpPort || address.UdpPort != expectedUdpPort {
				t.Fatalf("v%d is stored on %d/%d, expected %d/%d",
					address.IpVersion, address.TcpPort, address.UdpPort, expectedTcpPort, expectedUdpPort)
			}
		}
	})
}
