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

// A signing agent's counter key is a hash of its Signature-Agent value: one
// agent shares it wherever its browsers run, two agents never do, and the
// value itself is not in it (amendment 12).
func TestLagSignatureRateLimitKey(t *testing.T) {
	key := lagSignatureRateLimitKey("track", `"https://chatgpt.com"`)
	if lagSignatureRateLimitKey("track", `"https://chatgpt.com"`) != key {
		t.Error("one agent's key is not stable")
	}
	if lagSignatureRateLimitKey("track", `"https://agent.example"`) == key {
		t.Error("two agents share a key")
	}
	if lagSignatureRateLimitKey("session", `"https://chatgpt.com"`) == key {
		t.Error("two limits share a key")
	}
	for _, text := range []string{"chatgpt", "https"} {
		if strings.Contains(key, text) {
			t.Errorf("the key %q names the agent as %q", key, text)
		}
	}
	if !strings.HasPrefix(key, "{lag_track_signature_") {
		t.Errorf("the key %q is not in the lag limits' hash slot form", key)
	}
}
