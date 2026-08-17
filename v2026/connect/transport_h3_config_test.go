// This file pins the server's H3 path-MTU discovery behavior.
package connect

import "testing"

// TestConnectQuicConfigEnablesPathMtuDiscovery prevents a server-only fixed
// MTU ceiling from defeating the client's DPLPMTUD support.
func TestConnectQuicConfigEnablesPathMtuDiscovery(t *testing.T) {
	config := newConnectQuicConfig(DefaultConnectHandlerSettings())
	if config.DisablePathMTUDiscovery {
		t.Fatal("server H3 path MTU discovery is disabled")
	}
	if config.InitialPacketSize != 1400 {
		t.Fatalf("initial packet size=%d want=1400", config.InitialPacketSize)
	}
}
