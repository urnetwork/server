// Redirect admission is request-local and never widens the tunnel globally.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

// A closed-host transport must expose a scoped route, not a mutable allowlist.
func TestUrlProbeRedirectUsesExactRequestCapability(t *testing.T) {
	dials := 0
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("synthetic dial")
	}, nil, []string{"configured.example"}, time.Second)
	authorizer, ok := client.Transport.(interface {
		ProviderUrlProbeContext(context.Context, *url.URL, bool) (context.Context, error)
	})
	if !ok {
		t.Fatal("provider transport cannot authorize a request-local redirect")
	}
	target, _ := url.Parse("https://redirect.example/")
	ctx, err := authorizer.ProviderUrlProbeContext(context.Background(), target, true)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://unrelated.example/", nil)
	if _, err := client.Do(request); err == nil || dials != 0 {
		t.Fatal("redirect capability authorized an unrelated host")
	}
	request, _ = http.NewRequest("GET", target.String(), nil)
	if _, err := client.Do(request); err == nil || dials != 0 {
		t.Fatal("redirect authorization leaked to the next request")
	}
}

// The provider's DNS response cannot turn a public name into a private socket.
func TestUrlProbeRedirectRejectsNonPublicResolution(t *testing.T) {
	client := httpClientOverDialerWithHosts(nil, nil, []string{"configured.example"}, time.Second)
	authorizer, ok := client.Transport.(interface {
		ProviderUrlProbeContext(context.Context, *url.URL, bool) (context.Context, error)
	})
	if !ok {
		t.Fatal("provider transport lacks URL target validation")
	}
	target, _ := url.Parse("https://redirect.example/")
	ctx, err := authorizer.ProviderUrlProbeContext(context.Background(), target, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "192.0.2.1"} {
		resolver := &providerUrlResolver{
			query: func(context.Context, string, string) ([]netip.Addr, bool) {
				return []netip.Addr{netip.MustParseAddr(address)}, true
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				t.Errorf("nonpublic DNS answer %s reached the socket", address)
				return nil, errors.New("unexpected dial")
			},
		}
		dialCtx, trace := traceProviderHttpDial(ctx, "tcp", "redirect.example:443")
		if _, err := resolver.dialContext(dialCtx, "tcp", "redirect.example:443", trace); err == nil {
			t.Errorf("nonpublic DNS answer %s accepted", address)
		}
	}
}

// A legitimate cross-host hop uses only this tunnel's resolver and exact
// resolved address set; its capability does not survive into another request.
func TestUrlProbeRedirectAdmitsOnlyItsPublicResolvedTarget(t *testing.T) {
	queries, dials := 0, 0
	stop := errors.New("synthetic stop before TLS")
	resolver := &providerUrlResolver{
		query: func(_ context.Context, kind, host string) ([]netip.Addr, bool) {
			queries++
			if kind != "A" || host != "redirect.example" {
				t.Errorf("wrong private resolver target: %s %s", kind, host)
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, true
		},
		dial: func(_ context.Context, _, address string, addresses []netip.Addr) (net.Conn, error) {
			dials++
			if address != "redirect.example:443" || len(addresses) != 1 || addresses[0].String() != "8.8.8.8" {
				t.Error("redirect left exact resolved-target boundary")
			}
			return nil, stop
		},
	}
	client := httpClientOverDialerWithResolver(nil, resolver, nil, []string{"configured.example"}, time.Second)
	transport := client.Transport.(*providerHttpTransport)
	target, _ := url.Parse("https://redirect.example/")
	ctx, err := transport.ProviderUrlProbeContext(context.Background(), target, true)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", target.String(), nil)
	_, err = client.Do(request)
	if !errors.Is(err, stop) || queries != 1 || dials != 1 {
		t.Fatalf("legitimate redirect refused or escaped: queries=%d dials=%d err=%v", queries, dials, err)
	}
	request, _ = http.NewRequest("GET", target.String(), nil)
	_, err = client.Do(request)
	if !errors.Is(err, ErrPinHostUnknown) || queries != 1 || dials != 1 {
		t.Fatalf("redirect capability leaked: queries=%d dials=%d err=%v", queries, dials, err)
	}
}

// Pin failures expose the same typed identity evidence as failed WebPKI.
func TestPinMismatchHasTypedTlsIdentityEvidence(t *testing.T) {
	var classified interface{ TLSAuthenticationFailure() bool }
	if !errors.As(ErrPinMismatch, &classified) || !classified.TLSAuthenticationFailure() {
		t.Fatal("pin mismatch is not classified as TLS authentication failure")
	}
	if normalizeHost("PINNED.EXAMPLE.:443") != "pinned.example" {
		t.Fatal("DNS root suffix bypasses host pin normalization")
	}
}
