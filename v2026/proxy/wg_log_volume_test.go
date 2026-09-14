package proxy

import (
	"net/netip"
	"testing"

	connectproxy "github.com/urnetwork/proxy/v2026"
)

// A full production peer synchronization can apply more than ten thousand
// clients. Default logging must stay O(1): logClientCounts owns the one summary
// line, while exact per-client details require an explicit verbose setting.
func TestAppliedClientDetailsDoNotAmplifyDefaultSyncLogs(t *testing.T) {
	applied := make(map[netip.Addr]*connectproxy.WgClient, 25000)
	for i := 0; i < 25000; i++ {
		addr := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		applied[addr] = &connectproxy.WgClient{PublicKey: "synthetic-public-key"}
	}

	logAppliedClientDetails("sync", applied, false, func(string, ...any) {
		t.Fatal("default verbosity emitted a per-client WireGuard log")
	})
}

func TestAppliedClientDetailsRemainAvailableWhenVerbose(t *testing.T) {
	addr := netip.MustParseAddr("10.0.0.1")
	applied := map[netip.Addr]*connectproxy.WgClient{
		addr: {PublicKey: "synthetic-public-key"},
	}

	logged := 0
	logAppliedClientDetails("sync", applied, true, func(format string, args ...any) {
		logged++
		if format != "[wg]%s peer installed client_ipv4=%s public_key=%s\n" {
			t.Fatalf("verbose format = %q", format)
		}
		if len(args) != 3 || args[0] != "sync" || args[1] != addr || args[2] != "synthetic-public-key" {
			t.Fatalf("verbose args = %#v", args)
		}
	})
	if logged != 1 {
		t.Fatalf("verbose detail lines = %d, want 1", logged)
	}
}
