package server

// The model routes' per-address count (CSRV-M11-01; compute-plan-model-mirror-store, QD-1409) against Redis, on a
// fixed clock: each route counts an address's /29 or /56 apart, under a key of the address's peppered hash that names
// no address and no file, refusing past its limit with the rest of the window, and counting afresh in the next.

import (
	"context"
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The key a route's count is kept under: the route's own prefix and the address's peppered /29 or /56 hash, in api-3's
// {address_<hash>} family; addresses in one /29 share it, and the key shows neither the address nor its prefix.
func TestComputeModelRouteAddressKeyIsAPepperedHash(t *testing.T) {
	for _, address := range []string{"198.51.100.10", "198.51.100.15", "2001:db8:0:1::7", "2001:db8:0:1:ffff::9"} {
		addr := netip.MustParseAddr(address)
		hash := ClientIpHashForAddr(addr)
		key, err := ComputeModelRouteAddressKey("chunk", hex.EncodeToString(hash[:]))
		if err != nil {
			t.Fatal(err)
		}
		if key != "compute_model_chunk.{address_"+hex.EncodeToString(hash[:])+"}.address" {
			t.Fatalf("%s's key = %s", address, key)
		}
		if strings.Contains(key, address) || strings.Contains(key, "198.51.100") || strings.Contains(key, "2001:db8") {
			t.Fatalf("%s's key shows the address: %s", address, key)
		}
	}
	first := ClientIpHashForAddr(netip.MustParseAddr("198.51.100.10"))
	sameSubnet := ClientIpHashForAddr(netip.MustParseAddr("198.51.100.15"))
	otherSubnet := ClientIpHashForAddr(netip.MustParseAddr("198.51.100.20"))
	if first != sameSubnet || first == otherSubnet {
		t.Fatal("the hash does not bucket by /29")
	}
	for _, route := range []string{"", "Chunk", "chunk.", "{chunk}", "chunk_x"} {
		if _, err := ComputeModelRouteAddressKey(route, hex.EncodeToString(first[:])); err == nil {
			t.Errorf("the route %q made a key", route)
		}
	}
}

// One address's requests count to the route's limit within a window, the next refused with the rest of the window;
// another address of the same /29 shares the count, one of another /29 has its own, and the other route its own;
// the next window counts afresh; and Redis holds only the peppered hashes' keys, none naming an address.
func TestComputeModelRouteAddressCountsPerWindow(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		window := time.Minute
		start := time.Unix(1_800_000_000, 0).UTC().Truncate(window).Add(20 * time.Second)

		for i := range int64(3) {
			address := "198.51.100.10:4000"
			if i == 1 {
				// the same /29, another port and host
				address = "198.51.100.13:4001"
			}
			count, err := CountComputeModelRouteAddress(ctx, address, "index", window, 3, start.Add(time.Duration(i)*time.Second))
			if err != nil || !count.Allowed || count.Count != i+1 {
				t.Fatalf("request %d: %+v %v", i+1, count, err)
			}
		}
		refused, err := CountComputeModelRouteAddress(ctx, "198.51.100.10:4000", "index", window, 3, start.Add(10*time.Second))
		if err != nil || refused.Allowed || refused.RetryAfter != 30*time.Second {
			t.Fatalf("past the limit: %+v %v, want refused for the window's last 30 s", refused, err)
		}
		other, err := CountComputeModelRouteAddress(ctx, "198.51.100.20:4000", "index", window, 3, start.Add(10*time.Second))
		if err != nil || !other.Allowed || other.Count != 1 {
			t.Fatalf("another /29: %+v %v", other, err)
		}
		chunk, err := CountComputeModelRouteAddress(ctx, "198.51.100.10:4000", "chunk", window, 3, start.Add(10*time.Second))
		if err != nil || !chunk.Allowed || chunk.Count != 1 {
			t.Fatalf("the chunk route: %+v %v", chunk, err)
		}
		next, err := CountComputeModelRouteAddress(ctx, "198.51.100.10:4000", "index", window, 3, start.Add(40*time.Second))
		if err != nil || !next.Allowed || next.Count != 1 {
			t.Fatalf("the next window: %+v %v", next, err)
		}
		closed, err := CountComputeModelRouteAddress(ctx, "198.51.100.30:4000", "chunk", window, 0, start)
		if err != nil || closed.Allowed {
			t.Fatalf("a limit of none: %+v %v", closed, err)
		}
		if _, err := CountComputeModelRouteAddress(ctx, "198.51.100.30:4000", "chunk", time.Microsecond, 3, start); err == nil {
			t.Fatal("a window under a millisecond counted")
		}
		if _, err := CountComputeModelRouteAddress(ctx, "not an address", "chunk", window, 3, start); err == nil {
			t.Fatal("an unreadable address counted")
		}

		var keys []string
		Redis(ctx, func(r RedisClient) {
			var err error
			keys, err = r.Keys(ctx, "compute_model_*").Result()
			if err != nil {
				t.Fatal(err)
			}
		})
		hashHex := func(address string) string {
			hash := ClientIpHashForAddr(netip.MustParseAddr(address))
			return hex.EncodeToString(hash[:])
		}
		windowNumber := start.UnixMilli() / window.Milliseconds()
		want := map[string]bool{
			"compute_model_index.{address_" + hashHex("198.51.100.10") + "}.address_" + strconv.FormatInt(windowNumber, 10):   true,
			"compute_model_index.{address_" + hashHex("198.51.100.10") + "}.address_" + strconv.FormatInt(windowNumber+1, 10): true,
			"compute_model_index.{address_" + hashHex("198.51.100.20") + "}.address_" + strconv.FormatInt(windowNumber, 10):   true,
			"compute_model_chunk.{address_" + hashHex("198.51.100.10") + "}.address_" + strconv.FormatInt(windowNumber, 10):   true,
			"compute_model_chunk.{address_" + hashHex("198.51.100.30") + "}.address_" + strconv.FormatInt(windowNumber, 10):   true,
		}
		if len(keys) != len(want) {
			t.Fatalf("Redis holds %v, want %v", keys, want)
		}
		for _, key := range keys {
			if !want[key] {
				t.Fatalf("Redis holds %s, which is none of %v", key, want)
			}
			if strings.Contains(key, "198.51.100") || strings.Contains(key, "4000") {
				t.Fatalf("a key shows the address: %s", key)
			}
		}
	})
}
