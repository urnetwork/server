// Client-address tests pin the single ingress-owned forwarding contract.
package session

import (
	"net/http/httptest"
	"testing"
)

// Reproduces main's missing-CIDR failure with the observed ingress and client addresses.
func TestUrForwardedAddressDoesNotNeedProxyCIDRConfiguration(t *testing.T) {
	req := httptest.NewRequest("GET", "/my-ip-info", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-UR-Forwarded-For", "173.25.160.143:41001")

	clientSession, err := NewClientSessionFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Cancel()
	if clientSession.ClientAddress != "173.25.160.143:41001" {
		t.Fatalf(
			"the main ingress address replaced the client address: got %q, want 173.25.160.143:41001",
			clientSession.ClientAddress,
		)
	}
}

// Pins the bracketed IPv6 form emitted by Warp's nginx configuration.
func TestUrForwardedAddressAcceptsBracketedIPv6(t *testing.T) {
	req := httptest.NewRequest("GET", "/my-ip-info", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-UR-Forwarded-For", "[2604:2d80:6780:5c00:d087:6da4:77a1:ae96]:41002")

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[2604:2d80:6780:5c00:d087:6da4:77a1:ae96]:41002" {
		t.Fatalf("resolved address = %q, want the forwarded IPv6 client", got)
	}
}

// Pins the legacy unbracketed IPv6 form whose final decimal group is the port.
func TestUrForwardedAddressAcceptsUnbracketedIPv6(t *testing.T) {
	req := httptest.NewRequest("GET", "/my-ip-info", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-UR-Forwarded-For", "2604:2d80:6780:5c00:d087:6da4:77a1:ae96:41002")

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[2604:2d80:6780:5c00:d087:6da4:77a1:ae96]:41002" {
		t.Fatalf("resolved address = %q, want the unbracketed forwarded IPv6 client", got)
	}
}

// Standard forwarding headers are intentionally outside the UR ingress contract.
func TestLegacyForwardedHeadersAreIgnored(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Source-Port", "41001")

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "65.49.70.82:52344" {
		t.Fatalf("legacy headers changed the client address to %q", got)
	}
}

// Direct and internal calls have no ingress header and retain their socket peer.
func TestMissingUrForwardedAddressUsesRemoteAddress(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "192.0.2.44:54321"

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.0.2.44:54321" {
		t.Fatalf("resolved address = %q, want the socket peer", got)
	}
}

// A broken ingress value degrades to the peer instead of taking every route down.
func TestMalformedUrForwardedAddressUsesRemoteAddress(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-UR-Forwarded-For", "not-an-address")

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "65.49.70.82:52344" {
		t.Fatalf("resolved address = %q, want the socket peer", got)
	}
}

// The one-value rule prevents an old append-style chain from becoming authoritative.
func TestMultipleUrForwardedAddressesUseRemoteAddress(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Add("X-UR-Forwarded-For", "203.0.113.9:41001")
	req.Header.Add("X-UR-Forwarded-For", "198.51.100.20:41002")

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "65.49.70.82:52344" {
		t.Fatalf("resolved address = %q, want the socket peer", got)
	}
}

// The source port is part of the canonical value and is never inferred elsewhere.
func TestBareUrForwardedIpUsesRemoteAddress(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-UR-Forwarded-For", "203.0.113.9")

	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "65.49.70.82:52344" {
		t.Fatalf("resolved address = %q, want the socket peer", got)
	}
}
