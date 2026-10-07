// Regional DNS belongs to one provider tunnel and never uses the host resolver.
package providertunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"golang.org/x/net/dns/dnsmessage"
)

// The actual Open boundary must receive the SDK recommendation for its provider,
// with explicit remote settings taking precedence and unknown countries defaulting.
func TestProbeOpenSelectsPrivateRegionalDns(t *testing.T) {
	original := createTun
	t.Cleanup(func() { createTun = original })
	stop := errors.New("synthetic stop at private TUN boundary")
	var captured *connect.DnsResolverSettings
	createTun = func(_ context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
		captured = resolver
		return nil, stop
	}
	custom := &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{"https://custom.example/dns-query"}}
	for _, test := range []struct {
		country string
		custom  *connect.DnsResolverSettings
		want    *connect.DnsResolverSettings
	}{
		{country: " CN ", want: connect.RegionalDnsResolverSettings("cn")},
		{country: "zz", want: inTunnelOnlyDnsResolverSettings()},
		{country: "cn", custom: custom, want: custom},
	} {
		cfg := probeTransportBudgetTestConfig()
		cfg.ProviderCountry, cfg.DnsResolverSettings = test.country, test.custom
		_, err := Open(t.Context(), cfg, connect.NewId())
		if !errors.Is(err, stop) || !reflect.DeepEqual(captured, test.want) {
			t.Fatalf("provider resolver precedence lost for %q: got=%+v want=%+v err=%v", test.country, captured, test.want, err)
		}
		if test.custom != nil && captured == test.custom {
			t.Fatal("private tunnel borrowed mutable custom resolver settings")
		}
	}
}

// Even an explicit override cannot resolve provider targets from the operator.
func TestProbeRejectsOffTunnelCustomDns(t *testing.T) {
	for _, custom := range []*connect.DnsResolverSettings{{EnableLocalDns: true}, {EnableLocalDoh: true}} {
		cfg := probeTransportBudgetTestConfig()
		cfg.DnsResolverSettings = custom
		tunnel, err := Open(t.Context(), cfg, connect.NewId())
		if tunnel != nil {
			_ = tunnel.Close()
		}
		if !errors.Is(err, ErrOffTunnelDns) {
			t.Fatalf("off-tunnel custom resolver was accepted: %v", err)
		}
	}
}

// The existing SDK resolver rotates its configured regional servers after a
// failed dial. Its successful response traverses the supplied tunnel dialer;
// the test never opens a real DNS socket or invents a regional DoH URL.
func TestProbeRegionalDnsUsesConfiguredServerFallback(t *testing.T) {
	resolver, err := providerDnsResolverSettings(Config{ProviderCountry: "cn"})
	if err != nil || len(resolver.RemoteDnsIpv4) < 2 {
		t.Fatalf("missing SDK regional recommendation: %v", err)
	}
	settings := connect.DefaultDohSettings()
	settings.IpVersion = 4
	settings.DnsResolverSettings = resolver
	var calls atomic.Int32
	finished := make(chan struct{})
	settings.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "53" || !slices.Contains(resolver.RemoteDnsIpv4, host) {
			return nil, errors.New("escaped configured regional resolvers")
		}
		call := calls.Add(1)
		if call == 1 {
			return nil, errors.New("synthetic first regional server unavailable")
		}
		if call != 2 {
			return nil, errors.New("unexpected extra regional lookup")
		}
		client, server := net.Pipe()
		go func() {
			defer close(finished)
			defer server.Close()
			var size [2]byte
			if _, err := io.ReadFull(server, size[:]); err != nil {
				t.Error(err)
				return
			}
			payload := make([]byte, int(binary.BigEndian.Uint16(size[:])))
			if _, err := io.ReadFull(server, payload); err != nil {
				t.Error(err)
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(payload); err != nil || len(query.Questions) != 1 {
				t.Error("invalid synthetic query")
				return
			}
			question := query.Questions[0]
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions,
				Answers: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 77}}}}}
			encoded, err := response.Pack()
			if err != nil {
				t.Error(err)
				return
			}
			binary.BigEndian.PutUint16(size[:], uint16(len(encoded)))
			_, err = server.Write(append(size[:], encoded...))
			if err != nil {
				t.Error(err)
			}
		}()
		return client, nil
	}}
	cache := connect.NewDohCache(settings)
	defer cache.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "regional-probe.example")
	if !authoritative || len(addrs) != 1 || addrs[0].String() != "192.0.2.77" || calls.Load() != 2 {
		t.Fatalf("configured regional fallback failed: authoritative=%t calls=%d", authoritative, calls.Load())
	}
	<-finished
}

// An unavailable regional configuration remains bounded and nonauthoritative;
// it cannot fall back to the host's resolver or certify a synthetic answer.
func TestProbeRegionalDnsFailureKeepsHardDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver, err := providerDnsResolverSettings(Config{ProviderCountry: "cn"})
		if err != nil {
			t.Fatal(err)
		}
		settings := connect.DefaultDohSettings()
		settings.IpVersion, settings.DnsResolverSettings = 4, resolver
		settings.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "53" || !slices.Contains(resolver.RemoteDnsIpv4, host) {
				t.Error("regional failure escaped the private remote path")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		cache := connect.NewDohCache(settings)
		defer cache.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		addrs, authoritative := cache.QueryResult(ctx, "A", "regional-probe.example")
		if authoritative || len(addrs) != 0 || time.Since(start) != 10*time.Second {
			t.Fatalf("regional failure escaped deadline: authoritative=%t elapsed=%s", authoritative, time.Since(start))
		}
	})
}
