// The endpoint route is exercised by SDK startup WarmDns before any test DNS
// query. Follow-up A/AAAA queries verify the actual application-TUN return path.
package perfvar

import (
	"context"
	"crypto/x509"
	"net"
	"net/url"
	"reflect"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"golang.org/x/net/dns/dnsmessage"
)

// Original endpoint identities, family lists and all mux policy inputs stay
// identical; only the explicitly scoped test trust anchor is substituted.
func TestSyntheticFastOwnedDohPreservesDefaultResolverPolicy(t *testing.T) {
	owned, err := newSyntheticFastOwnedDoh(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := owned.close(ctx); err != nil {
			t.Error(err)
		}
	})
	settings, defaults := owned.settings(), clientconnect.DefaultUpgradeMuxSettings()
	if settings == nil || settings.Dns == nil || settings.Dns.Resolver == nil || settings.Dns.Resolver.TlsConfig == nil ||
		settings.Dns.Resolver.TlsConfig.InsecureSkipVerify || settings.Dns.Resolver.TlsConfig.RootCAs == nil {
		t.Fatal("owned DoH must use scoped certificate verification")
	}
	leaf, err := x509.ParseCertificate(owned.server.TLSConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, urls := range [][]string{defaults.Dns.Resolver.RemoteDohUrlsIpv4, defaults.Dns.Resolver.RemoteDohUrlsIpv6} {
		for _, raw := range urls {
			endpoint, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: settings.Dns.Resolver.TlsConfig.RootCAs, DNSName: endpoint.Hostname()}); err != nil {
				t.Fatal("unchanged endpoint identity failed scoped certificate verification: ", err)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: x509.NewCertPool(), DNSName: endpoint.Hostname()}); err == nil {
				t.Fatal("owned endpoint certificate was accepted without its scoped CA")
			}
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: settings.Dns.Resolver.TlsConfig.RootCAs, DNSName: "unowned-doh.fixture.example"}); err == nil {
		t.Fatal("owned endpoint certificate accepted an unrelated identity")
	}
	resolver := *settings.Dns.Resolver
	resolver.TlsConfig = nil
	settings.Dns.Resolver = &resolver
	if !reflect.DeepEqual(settings, defaults) {
		t.Fatal("owned DoH changed default endpoint, family, warm, racing, fallback or sizing policy")
	}
	// Every unchanged configured endpoint has its own verified identity. An
	// unrelated address cannot use this exact fixture substitution.
	if owned.matches(net.IPv4(192, 0, 2, 250), 443) || owned.matches(net.ParseIP("2001:db8::250"), 443) {
		t.Fatal("owned DoH route admitted an unconfigured destination")
	}
}

// RED reaches the real provider NAT's original missing-route branch. GREEN
// must serve a verified TLS/H2 WarmDns request and return parsed A/AAAA wire
// answers through the SDK private TUN and the application's native TUN.
// AAAA record delivery is not a claim that resolver transport used IPv6.
func TestSyntheticFastWarmDnsUsesOwnedProviderRoute(t *testing.T) {
	settings := defaultSyntheticFastSettings()
	profile := initialNetworkProfiles(7919)["clean-lan"]
	profile.Name = "synthetic-fast-access"
	profile.Forward = newLinkProfile(settings.OfferedBitsPerSecond, settings.AccessRoundTrip/2, 0, settings.LossProbability, 20*time.Millisecond)
	profile.Forward.QueueByteCount = min(2*1024*1024, profile.Forward.QueueByteCount)
	profile.Reverse = profile.Forward
	providerProfile := initialNetworkProfiles(7927)["clean-lan"]
	providerProfile.Forward = newLinkProfile(settings.OfferedBitsPerSecond, 0, 0, 0, 20*time.Millisecond)
	providerProfile.Forward.QueueByteCount = min(2*1024*1024, providerProfile.Forward.QueueByteCount)
	providerProfile.Reverse = providerProfile.Forward
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		fixture, err := newSyntheticFastNativeFixture(t, 32*1024*1024, profile, providerProfile)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		if selected := fixture.device.GetDnsResolverSettings(); selected == nil || selected.PresetId != "" {
			t.Fatal("owned default-endpoint emulation must be reported as custom DNS trust")
		}
		// This is only an external control bound; no SDK/mux/dial/health
		// deadline is shortened. Actual Fast adds no wait or extra query.
		ctx, cancel := context.WithTimeout(fixture.ctx, 30*time.Second)
		defer cancel()
		select {
		case <-fixture.ownedDoh.missingRoute:
			t.Fatal("OWNED_DOH_WARM_ROUTE_MISSING: SDK bootstrap reached real provider NAT without an owned DoH route")
		case <-fixture.ownedDoh.answered:
		case <-ctx.Done():
			t.Fatal("SDK WarmDns did not produce a verified owned TLS/H2 wire response: ", ctx.Err())
		}
		name, err := dnsmessage.NewName("owned-doh-control.example.")
		if err != nil {
			t.Fatal(err)
		}
		for index, recordType := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
			statusBefore := fixture.device.GetWindowStatus()
			if statusBefore == nil || statusBefore.ConnectionGeneration <= 0 || !statusBefore.MinSatisfied {
				t.Fatal("DNS control requires an actually formed default window")
			}
			resolverBefore := fixture.ownedDoh.snapshot()
			question := dnsmessage.Question{Name: name, Type: recordType, Class: dnsmessage.ClassINET}
			id := uint16(501 + index)
			builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
			if err := builder.StartQuestions(); err != nil {
				t.Fatal(err)
			}
			if err := builder.Question(question); err != nil {
				t.Fatal(err)
			}
			query, err := builder.Finish()
			if err != nil {
				t.Fatal(err)
			}
			connection, err := fixture.appTun.DialContext(ctx, "udp", net.JoinHostPort(clientconnect.DefaultDnsUpgradeMaskAddress, "53"))
			if err != nil {
				t.Fatal(err)
			}
			deadline, _ := ctx.Deadline()
			if err := connection.SetDeadline(deadline); err != nil {
				connection.Close()
				t.Fatal(err)
			}
			if n, err := connection.Write(query); err != nil || n != len(query) {
				connection.Close()
				t.Fatal("application DNS query was not written: ", n, err)
			}
			var response [1024]byte
			n, readErr := connection.Read(response[:])
			closeErr := connection.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal("application DNS response failed: ", readErr, closeErr)
			}
			statusAfter := fixture.device.GetWindowStatus()
			if statusAfter == nil || statusAfter.ConnectionGeneration != statusBefore.ConnectionGeneration ||
				statusAfter.Ipv6Available != statusBefore.Ipv6Available || !statusAfter.MinSatisfied {
				t.Fatal("DNS control capability premise changed during its query")
			}
			var parser dnsmessage.Parser
			header, err := parser.Start(response[:n])
			if err != nil || header.ID != id || !header.Response || header.RCode != dnsmessage.RCodeSuccess {
				t.Fatal("application DNS response header did not match its request: ", err)
			}
			actualQuestion, err := parser.Question()
			if err != nil || actualQuestion != question {
				t.Fatal("application DNS response question changed: ", err)
			}
			if _, err := parser.Question(); err != dnsmessage.ErrSectionDone {
				t.Fatal("application DNS response had extra questions: ", err)
			}
			if recordType == dnsmessage.TypeAAAA && !statusBefore.Ipv6Available {
				if _, err := parser.AnswerHeader(); err != dnsmessage.ErrSectionDone {
					t.Fatal("IPv4-only window did not preserve empty-AAAA policy: ", err)
				}
				if fixture.ownedDoh.snapshot().ControlAaaaResponses != resolverBefore.ControlAaaaResponses {
					t.Fatal("IPv4-only empty AAAA had an observed owned upstream response")
				}
				t.Log("[perfvar] owned DoH AAAA empty with stable IPv6-unavailable window; IPv6 transport not qualified")
				continue
			}
			answer, err := parser.AnswerHeader()
			if err != nil || answer.Name != name || answer.Type != recordType || answer.Class != dnsmessage.ClassINET {
				t.Fatal("application DNS answer identity changed: ", err)
			}
			if recordType == dnsmessage.TypeA {
				value, err := parser.AResource()
				if err != nil || value.A != [4]byte{192, 0, 2, 200} {
					t.Fatal("application A answer did not traverse the real resolver: ", err)
				}
			} else {
				value, err := parser.AAAAResource()
				want := [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x02, 0}
				if err != nil || value.AAAA != want {
					t.Fatal("application AAAA answer did not traverse the real resolver: ", err)
				}
				if fixture.ownedDoh.snapshot().ControlAaaaResponses <= resolverBefore.ControlAaaaResponses {
					t.Fatal("available-family AAAA answer lacked an owned upstream response")
				}
			}
			if _, err := parser.AnswerHeader(); err != dnsmessage.ErrSectionDone {
				t.Fatal("application DNS response had extra answers: ", err)
			}
		}
		// fixture.close joins consumer/provider NAT dial owners first, then
		// shuts down the owned DoH service. The defer remains a failure backstop.
		if err := fixture.close(); err != nil {
			t.Fatal(err)
		}
		joined := fixture.ownedDoh.snapshot()
		if !joined.Closing || !joined.ServeJoined || !joined.HandlersJoined || joined.ServeFailed ||
			joined.HandlersStarted != joined.HandlersFinished || joined.DialsStartedIpv4 != joined.DialsFinishedIpv4 ||
			joined.DialsStartedIpv6 != joined.DialsFinishedIpv6 || joined.MissingRoutes != 0 {
			t.Fatal("owned DoH service/NAT callers did not join cleanly")
		}
		t.Logf("[perfvar] owned-default-endpoint-doh %+v", joined)
	})
}
