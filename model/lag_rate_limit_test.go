package model

import (
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
)

// A prefix counter's key is the peppered hash of the /24 (/48): every address
// of the prefix shares it, and no form of the address or the prefix appears
// in it (lag.dev/PLAN.md A1, A6, amendment 9).
func TestLagPrefixRateLimitKey(t *testing.T) {
	for _, c := range []struct {
		addr  string
		same  string
		other string
		texts []string
	}{
		{"203.0.113.77", "203.0.113.200", "203.0.114.77", []string{"203.0.113", "cb0071"}},
		{"2001:db8:1234:5678::1", "2001:db8:1234:ffff::2", "2001:db8:1235::1", []string{"2001:db8", "20010db81234"}},
	} {
		addr := netip.MustParseAddr(c.addr)
		key := lagPrefixRateLimitKey("test", addr)
		if lagPrefixRateLimitKey("test", netip.MustParseAddr(c.same)) != key {
			t.Errorf("%s and %s are one caller, but their keys differ", c.addr, c.same)
		}
		if lagPrefixRateLimitKey("test", netip.MustParseAddr(c.other)) == key {
			t.Errorf("%s and %s are different callers, but share a key", c.addr, c.other)
		}
		for _, text := range append(c.texts, c.addr, hex.EncodeToString(addr.AsSlice())) {
			if strings.Contains(key, text) {
				t.Errorf("the key %q names %s as %q", key, c.addr, text)
			}
		}
	}
}
