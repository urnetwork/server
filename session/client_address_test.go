package session

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestParseTrustedProxyPrefixes(t *testing.T) {
	prefixes, err := ParseTrustedProxyPrefixes(" 127.0.0.0/8, 2001:db8::/32 ")
	if err != nil || len(prefixes) != 2 {
		t.Fatalf("prefixes=%v err=%v", prefixes, err)
	}
	if _, err := ParseTrustedProxyPrefixes("0.0.0.0/0,broken"); err == nil {
		t.Fatal("malformed trusted proxy CIDR accepted")
	}
	if _, err := ParseTrustedProxyPrefixes(" , "); err == nil {
		t.Fatal("empty trusted proxy set accepted")
	}
}

func TestResolveClientAddressTrustBoundary(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	req := httptest.NewRequest("POST", "/verify", nil)
	req.RemoteAddr = "127.0.0.2:12345"
	req.Header.Set("X-UR-Forwarded-For", "[2001:db8::7]:443")
	got, err := ResolveClientAddress(req, trusted)
	if err != nil || got != "[2001:db8::7]:443" {
		t.Fatalf("trusted forwarded address=%q err=%v", got, err)
	}

	req.RemoteAddr = "192.0.2.44:54321"
	got, err = ResolveClientAddress(req, trusted)
	if err != nil || got != "192.0.2.44:54321" {
		t.Fatalf("untrusted spoof address=%q err=%v", got, err)
	}
}

// TestResolveClientAddressReadsChainsAndPartialHeaders replaces a test that
// asserted the opposite, and the replacement is deliberate.
//
// It used to read:
//
//	req.Header.Set("X-UR-Forwarded-For", "192.0.2.1:1, 192.0.2.2:2")
//	if _, err := ResolveClientAddress(req, trusted); err == nil {
//	        t.Fatal("forwarded chain accepted")
//	}
//	req.Header.Set("X-Forwarded-For", "192.0.2.1")
//	if _, err := ResolveClientAddress(req, trusted); err == nil {
//	        t.Fatal("partial forwarded identity accepted")
//	}
//
// Rejecting is safe in the sense that no address is forged, but the rejection
// is an ERROR, and router.wrap / router.wrapWithInput turn a session
// construction failure into HTTP 500 for every endpoint. Both shapes above are
// what real proxies send: nginx with the documented
// proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for sends a bare
// X-Forwarded-For with no source port, and appends a comma on the second hop.
// So the old contract meant "enumerate your ingress proxy in
// BRINGYOUR_TRUSTED_PROXY_CIDRS" and "keep the api up" were mutually
// exclusive.
//
// Both are now read, and the chain is read from the RIGHT: the entry a trusted
// hop appended, never the entry a client prepended. See
// TestClientCannotForgeItsAddressThroughATrustedProxy, which is where that
// property is pinned properly.
func TestResolveClientAddressReadsChainsAndPartialHeaders(t *testing.T) {
	captureProxyReports(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	req.Header.Set("X-UR-Forwarded-For", "192.0.2.1:1, 192.0.2.2:2")
	got, err := ResolveClientAddress(req, trusted)
	if err != nil {
		t.Fatalf("a forwarded chain from a trusted peer errored (HTTP 500 on every endpoint): %v", err)
	}
	if got != "192.0.2.2:2" {
		t.Fatalf("forwarded chain resolved to %q, want the last hop 192.0.2.2:2", got)
	}

	req.Header.Del("X-UR-Forwarded-For")
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	got, err = ResolveClientAddress(req, trusted)
	if err != nil {
		t.Fatalf("a bare X-Forwarded-For from a trusted peer errored (HTTP 500 on every endpoint): %v", err)
	}
	if got != "192.0.2.1:0" {
		t.Fatalf("bare X-Forwarded-For resolved to %q, want 192.0.2.1:0", got)
	}
}

// An ipv6 "addr:port" element without brackets is readable only when the
// address part cannot absorb the port: a COMPRESSED ipv6 plus ":443" is itself
// a valid address ("2001:db8::7:443" — the port becomes one more hex group),
// so that form is inherently ambiguous and parses as the bare address by
// design. The warp lb now brackets ipv6 in X-UR-Forwarded-For (warpctl
// $warp_client_addr) to remove the ambiguity at the source; this test pins the
// two shapes the reader must get right regardless:
//
//  1. a FULL-FORM ipv6 with a trailing port (too many groups to be a bare
//     address) splits at the last colon instead of reporting the header
//     unusable and collapsing the client onto the lb's address, and
//  2. the lb's bracketed pair — X-UR-Forwarded-For with the port,
//     X-Forwarded-For without — resolves as one client, not a conflict.
func TestResolveClientAddressReadsBracketlessIpv6WithPort(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	req.Header.Set("X-UR-Forwarded-For", "2001:db8:0:0:0:0:0:7:443")
	got, err := ResolveClientAddress(req, trusted)
	if err != nil || got != "[2001:db8::7]:443" {
		t.Fatalf("full-form bracketless ipv6 element resolved to %q err=%v, want [2001:db8::7]:443", got, err)
	}

	req.Header.Set("X-Forwarded-For", "2001:db8::7")
	got, err = ResolveClientAddress(req, trusted)
	if err != nil || got != "[2001:db8::7]:443" {
		t.Fatalf("full-form lb header pair resolved to %q err=%v, want [2001:db8::7]:443", got, err)
	}

	req.Header.Set("X-UR-Forwarded-For", "[2001:db8::7]:443")
	got, err = ResolveClientAddress(req, trusted)
	if err != nil || got != "[2001:db8::7]:443" {
		t.Fatalf("bracketed lb header pair resolved to %q err=%v, want [2001:db8::7]:443", got, err)
	}
}
