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

func TestResolveClientAddressRejectsChainsAndPartialHeaders(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	req.Header.Set("X-UR-Forwarded-For", "192.0.2.1:1, 192.0.2.2:2")
	if _, err := ResolveClientAddress(req, trusted); err == nil {
		t.Fatal("forwarded chain accepted")
	}
	req.Header.Del("X-UR-Forwarded-For")
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	if _, err := ResolveClientAddress(req, trusted); err == nil {
		t.Fatal("partial forwarded identity accepted")
	}
}
