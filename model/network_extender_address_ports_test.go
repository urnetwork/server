package model

// The ports of each extender address (connect/EXTENDER.md S10), against the
// tables: what an activation stores, what every reader that signs a record or
// dials an address reads back, and an address written before the columns
// existed, which reads the extender's ports.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// One activation of publicKey on the given carrier ports.
func testExtenderActivationOnPorts(publicKey []byte, ipVersion int, ip string, tcpPort int, udpPort int) *NetworkExtenderActivation {
	activation := testExtenderActivation(publicKey, ipVersion, ip)
	activation.TcpPort = tcpPort
	activation.UdpPort = udpPort
	return activation
}

// Each family's activation keeps its own ports, which the record readers, the
// bootstrap sample, the release reader and the uptime probe all read back; the
// extender's ports are the latest activation's. Before, the address kept none,
// so every reader had the latest activation's ports for both families.
func TestActivateNetworkExtenderKeepsEachAddressPorts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		publicKey := []byte("extender-public-key-address-ports")
		ActivateNetworkExtender(ctx, testExtenderActivationOnPorts(publicKey, 4, "192.0.2.51", 40443, 40444), sign)
		activated := ActivateNetworkExtender(ctx, testExtenderActivationOnPorts(publicKey, 6, "2001:db8::51", 443, 443), sign)
		extenderId := activated.Extender.ExtenderId
		connect.AssertEqual(t, activated.Extender.TcpPort, 443)
		connect.AssertEqual(t, activated.Extender.UdpPort, 443)

		expectAddressPorts := func(what string, addresses []*NetworkExtenderAddress) {
			t.Helper()
			ports := map[int][2]int{}
			for _, address := range addresses {
				ports[address.IpVersion] = [2]int{address.TcpPort, address.UdpPort}
			}
			if ports[4] != [2]int{40443, 40444} || ports[6] != [2]int{443, 443} {
				t.Fatalf("%s reads the ports %v, expected v4 on 40443/40444 and v6 on 443/443", what, ports)
			}
		}
		expectAddressPorts("the activation's signer", activated.Addresses)
		_, recordAddresses := GetActiveNetworkExtenderForRecord(ctx, extenderId)
		expectAddressPorts("the record reader", recordAddresses)
		expectAddressPorts("the test reader", Testing_GetNetworkExtender(ctx, extenderId).Addresses)
		for _, entry := range GetRandomActiveNetworkExtenders(ctx, 64, server.Id{}) {
			if entry.Extender.ExtenderId == extenderId {
				expectAddressPorts("the bootstrap sample", entry.Addresses)
			}
		}

		probed := map[int]int{}
		for _, target := range GetActiveNetworkExtenderProbeTargets(ctx) {
			if target.ExtenderId == extenderId {
				probed[target.IpVersion] = target.TcpPort
			}
		}
		if probed[4] != 40443 || probed[6] != 443 {
			t.Fatalf("the uptime probe dials %v, expected v4 on 40443 and v6 on 443", probed)
		}

		// the measurement request's reader, which the measurement rule judges
		// each address by
		now := server.NowUtc()
		measurementKey := []byte("key-address-ports")
		SetNetworkExtenderMeasurementRequest(ctx, extenderId, append([]byte("csr-"), measurementKey...), measurementKey, testMeasurementNames(extenderId), now.Add(-time.Minute))
		found := false
		for _, target := range GetPendingNetworkExtenderMeasurements(ctx, now, 64) {
			if target.Measurement.ExtenderId == extenderId {
				expectAddressPorts("the measurement reader", target.Addresses)
				found = true
			}
		}
		if !found {
			t.Fatal("the measurement request is not pending")
		}

		// and the gated tier's release reader
		gatedKey := []byte("extender-public-key-address-ports-gated")
		for _, activation := range []*NetworkExtenderActivation{
			testExtenderActivationOnPorts(gatedKey, 4, "192.0.2.54", 40443, 40444),
			testExtenderActivationOnPorts(gatedKey, 6, "2001:db8::54", 443, 443),
		} {
			activation.DirectoryTier = connect.ExtenderDirectoryTierGated
			ActivateNetworkExtender(ctx, activation, sign)
		}
		found = false
		for _, entry := range GetActiveGatedNetworkExtenders(ctx) {
			if string(entry.Extender.PublicKey) == string(gatedKey) {
				expectAddressPorts("the release reader", entry.Addresses)
				found = true
			}
		}
		if !found {
			t.Fatal("the gated extender is not read for release")
		}
	})
}

// An address the probes deactivated is probed for its return on its own port.
func TestGetNetworkExtenderRecoveryProbeTargetsDialTheAddressPort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		publicKey := []byte("extender-public-key-address-ports-recovery")
		activated := ActivateNetworkExtender(ctx, testExtenderActivationOnPorts(publicKey, 4, "192.0.2.52", 40443, 40444), sign)
		ActivateNetworkExtender(ctx, testExtenderActivationOnPorts(publicKey, 6, "2001:db8::52", 443, 443), sign)
		testDeactivateByProbes(ctx, activated.Extender.ExtenderId, 4)

		for _, target := range GetNetworkExtenderRecoveryProbeTargets(
			ctx,
			testRecoveryLastWorkedSince(),
			ExtenderTestingMaxConsecutiveProbeFailures,
		) {
			if target.ExtenderId != activated.Extender.ExtenderId {
				continue
			}
			connect.AssertEqual(t, target.IpVersion, 4)
			if target.TcpPort != 40443 {
				t.Fatalf("the v4 address is probed for its return on %d, expected its own 40443", target.TcpPort)
			}
			return
		}
		t.Fatal("the deactivated v4 address is not probed for its return")
	})
}

// An address written before its ports were kept, as the running release writes
// it, reads no ports of its own, and is dialed on the extender's.
func TestNetworkExtenderAddressWithoutPortsReadsTheExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		publicKey := []byte("extender-public-key-address-ports-legacy")
		activated := ActivateNetworkExtender(ctx, testExtenderActivationOnPorts(publicKey, 4, "192.0.2.53", 8443, 8444), sign)
		extenderId := activated.Extender.ExtenderId
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_address SET tcp_port = NULL, udp_port = NULL WHERE extender_id = $1`,
				extenderId,
			))
		})

		extender, addresses := GetActiveNetworkExtenderForRecord(ctx, extenderId)
		connect.AssertEqual(t, len(addresses), 1)
		connect.AssertEqual(t, addresses[0].TcpPort, 0)
		connect.AssertEqual(t, addresses[0].UdpPort, 0)
		connect.AssertEqual(t, addresses[0].DialTcpPort(extender), 8443)
		connect.AssertEqual(t, addresses[0].DialUdpPort(extender), 8444)
		for _, target := range GetActiveNetworkExtenderProbeTargets(ctx) {
			if target.ExtenderId == extenderId && target.TcpPort != 8443 {
				t.Fatalf("the uptime probe dials %d, expected the extender's 8443", target.TcpPort)
			}
		}
	})
}
