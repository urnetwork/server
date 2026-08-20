// This file pins the server's H3 path-MTU discovery behavior.
package connect

import (
	"testing"

	quic "github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect"
)

func TestConnectH3InitialDatagramPathByteCountUsesQuicLimit(t *testing.T) {
	settings := clientconnect.DefaultH3DatagramSettings()
	probeByteCount := 0
	maximum := initialConnectH3DatagramPathByteCount(
		settings.TargetDatagramByteCount,
		func(probe []byte) error {
			probeByteCount = len(probe)
			return &quic.DatagramTooLargeError{
				MaxDatagramPayloadSize: clientconnect.H3InitialDatagramByteCount,
			}
		},
	)
	if probeByteCount != 2048 || maximum != clientconnect.H3InitialDatagramByteCount {
		t.Fatalf("probe bytes/maximum = %d/%d", probeByteCount, maximum)
	}
}

func TestConnectH3TransferCarrierEnablesBoundedAckReserve(t *testing.T) {
	properties := connectH3TransferCarrierProperties(true)
	if !properties.Unreliable || !properties.UnreliableFlowIsolation ||
		!properties.UnreliableFlowReserve {
		t.Fatalf("H3 DATAGRAM carrier properties = %+v", properties)
	}
	properties = connectH3TransferCarrierProperties(false)
	if properties.Unreliable || properties.UnreliableFlowIsolation ||
		properties.UnreliableFlowReserve {
		t.Fatalf("legacy H3 carrier properties = %+v", properties)
	}
}

// TestConnectQuicConfigEnablesPathMtuDiscovery prevents a server-only fixed
// MTU ceiling from defeating the client's DPLPMTUD support. It also pins
// connection-level keepalive independently from the possibly blocked
// application DATAGRAM writer.
func TestConnectQuicConfigEnablesPathMtuDiscovery(t *testing.T) {
	settings := DefaultConnectHandlerSettings()
	config := newConnectQuicConfig(settings)
	if config.DisablePathMTUDiscovery {
		t.Fatal("server H3 path MTU discovery is disabled")
	}
	if config.InitialPacketSize != clientconnect.H3InitialPacketByteCount {
		t.Fatalf(
			"initial packet size=%d want=%d",
			config.InitialPacketSize,
			clientconnect.H3InitialPacketByteCount,
		)
	}
	if !config.EnableDatagrams {
		t.Fatal("H3 DATAGRAM capability is not advertised by default")
	}
	if config.KeepAlivePeriod != settings.MaxPingTimeout {
		t.Fatalf(
			"server H3 keepalive period=%s want=%s",
			config.KeepAlivePeriod,
			settings.MaxPingTimeout,
		)
	}
	if config.Tracer != nil {
		t.Fatal("server H3 packet tracing enabled without an explicit collector")
	}
	settings.H3QuicPacketStats = &clientconnect.H3QuicPacketStats{}
	if newConnectQuicConfig(settings).Tracer == nil {
		t.Fatal("server H3 packet stats did not enable the QUIC tracer")
	}
	settings.EnableH3Datagrams = false
	if newConnectQuicConfig(settings).EnableDatagrams {
		t.Fatal("H3 DATAGRAM rollout setting did not disable QUIC advertisement")
	}
}

// The server binds the unprivileged post-DNAT port rather than public UDP/53.
func TestDefaultConnectHandlerDnsListenerUsesInternalPort(t *testing.T) {
	settings := DefaultConnectHandlerSettings()
	if settings.ListenDnsPort != 4053 {
		t.Fatalf("DNS-encoded QUIC listener port=%d want=4053", settings.ListenDnsPort)
	}
	if len(settings.ListenDnsCompatibilityPorts) != 1 || settings.ListenDnsCompatibilityPorts[0] != 8053 {
		t.Fatalf("DNS-encoded QUIC compatibility ports=%v want=[8053]", settings.ListenDnsCompatibilityPorts)
	}
}
