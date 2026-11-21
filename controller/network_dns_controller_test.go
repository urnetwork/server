package controller

import (
	"testing"

	"github.com/urnetwork/server"
)

// The administrative DNS routes use the same deployment-wide switches as the DNS host.
func TestNetworkDnsGateUsesGlobalSwitches(t *testing.T) {
	for _, test := range []struct {
		gate  string
		read  bool
		write bool
	}{
		{"{enabled: true, writes: true}", true, true},
		{"{enabled: true, writes: false}", true, false},
		{"{enabled: false, writes: true}", false, false},
	} {
		restore := server.Testing_PushSidecarConfig("sidecar:\n  dns: " + test.gate + "\n")
		read := networkDnsGate(false)
		write := networkDnsGate(true)
		restore()
		if (read == nil) != test.read || (write == nil) != test.write {
			t.Fatalf("%s: read %v, write %v", test.gate, read, write)
		}
	}
}
