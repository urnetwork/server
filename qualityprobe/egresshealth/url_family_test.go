// A URL load is made over every IP family its provider advertises and its URL
// serves, each leg forced to its family, and passes only if every leg does.
// These tests build their catalogs from the pool's wire format, as the
// prober receives it, and their provider paths record the family every leg
// was forced to.
package egresshealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

type familyTestKey struct{}

// A provider path with family control (providertunnel's grant): each leg's
// grant records its transport and family, and the leg is answered by that
// family's round tripper, as the tunnel would dial only that family. A
// family with no round tripper is one the test never offers: a leg over it
// is a defect of the probe.
type familyTestTransport struct {
	https map[int]http.RoundTripper
	h3    map[int]http.RoundTripper

	stateLock sync.Mutex
	legs      []string
	targets   []string
}

func (self *familyTestTransport) record(leg string, target *url.URL) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.legs = append(self.legs, leg)
	self.targets = append(self.targets, leg+" "+target.Host)
}

func (self *familyTestTransport) madeLegs() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.legs)
}

// Every grant's leg and the host it was for, in order.
func (self *familyTestTransport) madeTargets() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.targets)
}

func (self *familyTestTransport) ProviderUrlProbeFamilyContext(ctx context.Context, target *url.URL, redirect bool, ipVersion int) (context.Context, error) {
	self.record(fmt.Sprintf("tcp%d", ipVersion), target)
	return context.WithValue(ctx, familyTestKey{}, ipVersion), nil
}

func (self *familyTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return familyTestRoundTrip(request, self.https)
}

func (self *familyTestTransport) ProviderUrlProbeH3RoundTripper() http.RoundTripper {
	if self.h3 == nil {
		return nil
	}
	return &familyTestH3Transport{parent: self}
}

// The same provider's HTTP/3 path, granted per family like its HTTPS path.
type familyTestH3Transport struct {
	parent *familyTestTransport
}

func (self *familyTestH3Transport) ProviderUrlProbeFamilyContext(ctx context.Context, target *url.URL, redirect bool, ipVersion int) (context.Context, error) {
	self.parent.record(fmt.Sprintf("udp%d", ipVersion), target)
	return context.WithValue(ctx, familyTestKey{}, ipVersion), nil
}

func (self *familyTestH3Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	return familyTestRoundTrip(request, self.parent.h3)
}

func familyTestRoundTrip(request *http.Request, byFamily map[int]http.RoundTripper) (*http.Response, error) {
	ipVersion, ok := request.Context().Value(familyTestKey{}).(int)
	if !ok {
		return nil, errors.New("a leg reached the provider without a family grant")
	}
	transport := byFamily[ipVersion]
	if transport == nil {
		return nil, fmt.Errorf("a leg was dialed over IPv%d, which this provider path does not carry", ipVersion)
	}
	return transport.RoundTrip(request)
}

// Real content, as a site that works answers.
func familyTestContent() http.RoundTripper {
	return roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic real content"))}, nil
	})
}

// A leg that fails at the provider's stage, as the tunnel reports it.
func familyTestFails(stage string) http.RoundTripper {
	return roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, &echoStageSyntheticError{stage: stage, cause: errors.New("synthetic " + stage)}
	})
}

const familyTestDualCatalog = `[{"name":"synthetic-dual","class":"site","url":"https://dual.example/","expect":"body","h3":true,"ipv6":true}]`
const familyTestIpv6TcpCatalog = `[{"name":"synthetic-dual-tcp","class":"site","url":"https://dual-tcp.example/","expect":"body","ipv6":true}]`
const familyTestIpv4Catalog = `[{"name":"synthetic-v4","class":"site","url":"https://v4.example/","expect":"body","h3":true}]`

func familyTestCheck(t *testing.T, transport http.RoundTripper, catalog string, family IpFamily, udpProbesDisabled bool) *Result {
	t.Helper()
	result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, catalog), Rand: rand.New(rand.NewSource(3)),
		ProviderIpFamily: family, UdpProbesDisabled: udpProbesDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.UrlProbeEvidence == nil {
		t.Fatal("URL outcome lost its versioned evidence")
	}
	if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatalf("measured evidence fails ingest validation: %v", err)
	}
	return result
}

func familyTestPath(httpsV4, httpsV6, h3V4, h3V6 http.RoundTripper) *familyTestTransport {
	transport := &familyTestTransport{https: map[int]http.RoundTripper{}, h3: map[int]http.RoundTripper{}}
	if httpsV4 != nil {
		transport.https[4] = httpsV4
	}
	if httpsV6 != nil {
		transport.https[6] = httpsV6
	}
	if h3V4 != nil {
		transport.h3[4] = h3V4
	}
	if h3V6 != nil {
		transport.h3[6] = h3V6
	}
	return transport
}

// A provider that advertises IPv4 alone, legacy or v4-only, is probed over
// the URL's A record only, over both transports, even on a URL that serves
// IPv6: no leg is ever forced to IPv6.
func TestUrlProbeIpv4ProviderUsesARecordsOnly(t *testing.T) {
	for _, family := range []IpFamily{IpFamilyLegacy, IpFamilyV4Only, "a-family-this-build-does-not-know"} {
		transport := familyTestPath(familyTestContent(), nil, familyTestContent(), nil)
		result := familyTestCheck(t, transport, familyTestDualCatalog, family, false)
		if legs := transport.madeLegs(); !slices.Equal(legs, []string{"tcp4", "udp4"}) {
			t.Fatalf("%q: legs %v; want IPv4 HTTPS then IPv4 HTTP/3 alone", family, legs)
		}
		if result.OkCount != 1 || result.Total != 1 || result.UrlProbeEvidence.Ipv6 != nil || result.UrlProbeEvidence.IpFamily != "" {
			t.Fatalf("%q: ok=%d/%d ipv6=%+v family=%q", family, result.OkCount, result.Total, result.UrlProbeEvidence.Ipv6, result.UrlProbeEvidence.IpFamily)
		}
		if summary := result.Ipv6Summary(); summary != "" {
			t.Fatalf("%q: an IPv4 provider's line names IPv6 legs: %q", family, summary)
		}
	}
}

// A provider that advertises IPv6 alone has no IPv4 HTTPS leg, which every
// load's first leg is (site tallies, TLS findings and retirement read it), and
// the URL lane does not claim such providers. Should one be asked for, it is
// not measured: no request goes out over its A record, or at all.
func TestUrlProbeIpv6OnlyProviderNeverUsesAnARecord(t *testing.T) {
	transport := familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestContent())
	result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, familyTestDualCatalog), Rand: rand.New(rand.NewSource(3)),
		ProviderIpFamily: IpFamilyV6Only,
	})
	if !errors.Is(err, ErrNoUrlForProviderFamily) || result != nil {
		t.Fatalf("result=%+v err=%v; an IPv6-only provider must be not measured", result, err)
	}
	if legs := transport.madeLegs(); len(legs) != 0 {
		t.Fatalf("legs %v; an IPv6-only provider was probed", legs)
	}
}

// The owner's dual-stack case: separate probes over the A record and over the
// AAAA record, each over TCP and, for an h3 URL, UDP. Every leg passes, so
// the URL passes, and the IPv6 legs are kept beside the IPv4 ones.
func TestUrlProbeDualStackProviderRunsBothFamilies(t *testing.T) {
	transport := familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestContent())
	result := familyTestCheck(t, transport, familyTestDualCatalog, IpFamilyDualstack, false)
	if legs := transport.madeLegs(); !slices.Equal(legs, []string{"tcp4", "udp4", "tcp6", "udp6"}) {
		t.Fatalf("legs %v; want IPv4 HTTPS, IPv4 HTTP/3, IPv6 HTTPS, IPv6 HTTP/3", legs)
	}
	evidence := result.UrlProbeEvidence
	if result.OkCount != 1 || evidence.Ipv6 == nil || evidence.Ipv6.Tcp != UrlProbeIpv6TcpPassed ||
		evidence.Ipv6.H3 == nil || evidence.Ipv6.H3.Udp != UrlProbeH3UdpPassed || evidence.IpFamily != IpFamilyDualstack {
		t.Fatalf("ok=%d evidence=%+v", result.OkCount, evidence)
	}
	if result.Ipv6Summary() != "ok:1" || result.Ipv6H3Summary() != "ok:1" || result.H3Summary() != "ok:1" {
		t.Fatalf("summaries ipv6=%q ipv6_h3=%q h3=%q", result.Ipv6Summary(), result.Ipv6H3Summary(), result.H3Summary())
	}
}

// Pass means every leg passes: a dual-stack provider whose IPv6 fails a URL
// over TCP, or carries it over TCP but not UDP, fails it, although its IPv4
// legs passed; and one whose IPv4 HTTP/3 leg is silent fails it too, its IPv6
// legs still made, since whether that silence counts is the server's place
// judgement.
func TestUrlProbeDualStackProviderFailsWhenEitherFamilyFails(t *testing.T) {
	for _, c := range []struct {
		name      string
		path      *familyTestTransport
		legs      []string
		stage     string
		ipv6Tcp   string
		ipv6Udp   string
		h3Udp     string
		ipv6Words string
	}{
		{"IPv6 connect fails", familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestContent()),
			[]string{"tcp4", "udp4", "tcp6"}, FailureStageIpv6, UrlProbeIpv6TcpFailed, "", UrlProbeH3UdpPassed, "dial_tcp:1"},
		{"IPv6 TLS fails", familyTestPath(familyTestContent(), familyTestFails("tls"), familyTestContent(), familyTestContent()),
			[]string{"tcp4", "udp4", "tcp6"}, FailureStageIpv6, UrlProbeIpv6TcpFailed, "", UrlProbeH3UdpPassed, "tls:1"},
		{"IPv6 carries TCP but not UDP", familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestFails(FailureStageDialQuic)),
			[]string{"tcp4", "udp4", "tcp6", "udp6"}, FailureStageIpv6, UrlProbeIpv6TcpPassed, UrlProbeH3UdpSilent, UrlProbeH3UdpPassed, "ok:1"},
		{"IPv4 UDP silent, IPv6 passes", familyTestPath(familyTestContent(), familyTestContent(), familyTestFails(FailureStageDialQuic), familyTestContent()),
			[]string{"tcp4", "udp4", "tcp6", "udp6"}, FailureStageH3, UrlProbeIpv6TcpPassed, UrlProbeH3UdpPassed, UrlProbeH3UdpSilent, "ok:1"},
	} {
		result := familyTestCheck(t, c.path, familyTestDualCatalog, IpFamilyDualstack, false)
		if legs := c.path.madeLegs(); !slices.Equal(legs, c.legs) {
			t.Fatalf("%s: legs %v, want %v", c.name, legs, c.legs)
		}
		evidence := result.UrlProbeEvidence
		if result.OkCount != 0 || result.Total != 1 || evidence.FailureStage != c.stage || evidence.Ipv6 == nil || evidence.Ipv6.Tcp != c.ipv6Tcp {
			t.Fatalf("%s: ok=%d/%d stage=%q ipv6=%+v; a dual-stack provider passes only with every leg", c.name, result.OkCount, result.Total, evidence.FailureStage, evidence.Ipv6)
		}
		if udp := ""; evidence.Ipv6.H3 != nil {
			udp = evidence.Ipv6.H3.Udp
			if udp != c.ipv6Udp {
				t.Fatalf("%s: IPv6 HTTP/3 %q, want %q", c.name, udp, c.ipv6Udp)
			}
		} else if c.ipv6Udp != "" {
			t.Fatalf("%s: no IPv6 HTTP/3 leg recorded", c.name)
		}
		if evidence.H3 == nil || evidence.H3.Udp != c.h3Udp || result.Ipv6Summary() != c.ipv6Words {
			t.Fatalf("%s: IPv4 HTTP/3 %+v ipv6=%q", c.name, evidence.H3, result.Ipv6Summary())
		}
		if result.TlsAuthenticationFailure || 0 < len(evidence.Security) && evidence.Security[0].TlsFailure {
			t.Fatalf("%s: an IPv6 leg's outcome reached the IPv4 HTTPS identity's TLS evidence", c.name)
		}
		if !result.Checks[0].HttpsOk() || !evidence.HttpsOk(result.OkCount) {
			t.Fatalf("%s: the IPv4 HTTPS leg passed, which the site tallies must read", c.name)
		}
	}
}

// The IPv6 legs follow the IPv4 HTTPS leg: when it fails the load has failed
// already, and no IPv6 leg is made.
func TestUrlProbeIpv6LegsFollowAPassedIpv4HttpsLeg(t *testing.T) {
	path := familyTestPath(familyTestFails("dial_tcp"), familyTestContent(), familyTestContent(), familyTestContent())
	result := familyTestCheck(t, path, familyTestDualCatalog, IpFamilyDualstack, false)
	if legs := path.madeLegs(); !slices.Equal(legs, []string{"tcp4"}) {
		t.Fatalf("legs %v; want the IPv4 HTTPS leg alone", legs)
	}
	if result.OkCount != 0 || result.UrlProbeEvidence.Ipv6 != nil || result.Ipv6Summary() != "skipped:1" || result.Ipv6H3Summary() != "skipped:1" {
		t.Fatalf("ok=%d ipv6=%+v summaries %q %q", result.OkCount, result.UrlProbeEvidence.Ipv6, result.Ipv6Summary(), result.Ipv6H3Summary())
	}
}

// An IPv6 leg that never reached the point of dialing its site over IPv6 is
// not measured, never failed: the URL's AAAA record was empty from the
// provider's place, the resolver failed, the address was not public, or the
// tunnel was lost. The load is judged on its IPv4 legs.
func TestUrlProbeUnmeasuredIpv6LegIsNeverAFailure(t *testing.T) {
	canceled := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("the tunnel under the leg was lost: %w", context.Canceled)
	})
	for stage, leg := range map[string]http.RoundTripper{
		"dial_dns":           familyTestFails("dial_dns"),
		"dial_dns_or_socket": familyTestFails("dial_dns_or_socket"),
		"policy":             familyTestFails("policy"),
		"request_canceled":   canceled,
	} {
		path := familyTestPath(familyTestContent(), leg, familyTestContent(), familyTestContent())
		result := familyTestCheck(t, path, familyTestDualCatalog, IpFamilyDualstack, false)
		evidence := result.UrlProbeEvidence
		if result.OkCount != 1 || result.Total != 1 || evidence.FailureStage != "" || evidence.Ipv6 == nil ||
			evidence.Ipv6.Tcp != UrlProbeIpv6TcpNotMeasured || evidence.Ipv6.H3 != nil || result.Ipv6Summary() != "not_measured:1" {
			t.Fatalf("%s: ok=%d/%d stage=%q ipv6=%+v summary=%q", stage, result.OkCount, result.Total, evidence.FailureStage, evidence.Ipv6, result.Ipv6Summary())
		}
	}
	// a provider path that cannot force IPv6 at all is an instrument that
	// cannot make the leg
	plain := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic real content"))}, nil
	})
	result := familyTestCheck(t, plain, familyTestIpv6TcpCatalog, IpFamilyDualstack, false)
	if result.OkCount != 1 || result.UrlProbeEvidence.Ipv6 == nil || result.UrlProbeEvidence.Ipv6.Tcp != UrlProbeIpv6TcpNotMeasured ||
		result.UrlProbeEvidence.Ipv6.FailureStage != FailureStageIpv6Unavailable {
		t.Fatalf("a path without family control: ok=%d ipv6=%+v", result.OkCount, result.UrlProbeEvidence.Ipv6)
	}
}

// A URL without AAAA (no ipv6: true) is never used for an IPv6 leg: a
// dual-stack provider's load of it makes its IPv4 legs alone, and the IPv6
// family is not measured on it (no_url), never failed.
func TestUrlProbeUrlWithoutAaaaIsNeverUsedForIpv6(t *testing.T) {
	path := familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestFails(FailureStageDialQuic))
	result := familyTestCheck(t, path, familyTestIpv4Catalog, IpFamilyDualstack, false)
	if legs := path.madeLegs(); !slices.Equal(legs, []string{"tcp4", "udp4"}) {
		t.Fatalf("legs %v; a URL without AAAA was used for IPv6", legs)
	}
	if result.OkCount != 1 || result.UrlProbeEvidence.Ipv6 != nil || !result.Checks[0].Ipv6NoUrl || result.Ipv6Summary() != "no_url:1" || result.Ipv6H3Summary() != "" {
		t.Fatalf("ok=%d ipv6=%+v summary=%q h3 summary=%q", result.OkCount, result.UrlProbeEvidence.Ipv6, result.Ipv6Summary(), result.Ipv6H3Summary())
	}
}

// A URL whose entry says ipv4: false has no IPv4 HTTPS leg to make first, so
// it is never drawn as a load's URL while every load starts over IPv4; the
// rest of the catalog is drawn as before, whatever families the provider
// advertises. (It serves IPv6 legs, as a stand-in: below.)
func TestUrlProbeUrlWithoutARecordIsNeverDrawn(t *testing.T) {
	catalog := `[{"name":"synthetic-v6-only","class":"site","url":"https://v6only.example/","expect":"body","ipv4":false,"ipv6":true},` +
		`{"name":"synthetic-both","class":"site","url":"https://both.example/","expect":"body","ipv6":true}]`
	for seed := int64(0); seed < 40; seed++ {
		for _, family := range []IpFamily{IpFamilyV4Only, IpFamilyDualstack} {
			path := familyTestPath(familyTestContent(), familyTestContent(), nil, nil)
			result, err := Check(context.Background(), &http.Client{Transport: path}, Options{
				UrlProbe: true, Destinations: h3TestDestinations(t, catalog), Rand: rand.New(rand.NewSource(seed)), ProviderIpFamily: family,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.UrlProbeEvidence.Destination.Name != "synthetic-both" {
				t.Fatalf("seed %d %q drew %q, which serves no IPv4", seed, family, result.UrlProbeEvidence.Destination.Name)
			}
		}
	}
	only := `[{"name":"synthetic-v6-only","class":"site","url":"https://v6only.example/","expect":"body","ipv4":false,"ipv6":true}]`
	if _, err := Check(context.Background(), &http.Client{Transport: familyTestPath(familyTestContent(), familyTestContent(), nil, nil)}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, only), Rand: rand.New(rand.NewSource(1)), ProviderIpFamily: IpFamilyDualstack,
	}); !errors.Is(err, ErrNoUrlForProviderFamily) {
		t.Fatalf("a catalog with no IPv4 URL: err=%v; want not measured", err)
	}
}

// The owner's country switch: a provider whose exit country disables UDP
// probes makes no UDP leg in any family. Its URLs are judged on their HTTPS
// legs alone, as a URL without h3 is, and the legs not made are counted as
// country_disabled, never as failures; a provider that drops UDP passes there.
func TestUrlProbeCountryWithUdpProbesDisabledRunsNoUdpLeg(t *testing.T) {
	path := familyTestPath(familyTestContent(), familyTestContent(), familyTestFails(FailureStageDialQuic), familyTestFails(FailureStageDialQuic))
	result := familyTestCheck(t, path, familyTestDualCatalog, IpFamilyDualstack, true)
	if legs := path.madeLegs(); !slices.Equal(legs, []string{"tcp4", "tcp6"}) {
		t.Fatalf("legs %v; a country without UDP probes made a UDP leg", legs)
	}
	evidence := result.UrlProbeEvidence
	if result.OkCount != 1 || !evidence.UdpProbesDisabled || evidence.H3 != nil || evidence.Ipv6 == nil || evidence.Ipv6.H3 != nil || !evidence.Destination.H3 {
		t.Fatalf("ok=%d evidence=%+v; the URL is judged on its HTTPS legs", result.OkCount, evidence)
	}
	if result.H3Summary() != "country_disabled:1" || result.Ipv6H3Summary() != "country_disabled:1" || result.FailureStageSummary() != "" {
		t.Fatalf("summaries h3=%q ipv6_h3=%q stages=%q", result.H3Summary(), result.Ipv6H3Summary(), result.FailureStageSummary())
	}
	// an IPv4 provider in the same country, and the same URL elsewhere
	v4 := familyTestPath(familyTestContent(), nil, familyTestFails(FailureStageDialQuic), nil)
	if result := familyTestCheck(t, v4, familyTestIpv4Catalog, IpFamilyV4Only, true); result.OkCount != 1 || !slices.Equal(v4.madeLegs(), []string{"tcp4"}) {
		t.Fatalf("IPv4 provider: ok=%d legs=%v", result.OkCount, v4.madeLegs())
	}
	elsewhere := familyTestPath(familyTestContent(), nil, familyTestFails(FailureStageDialQuic), nil)
	if result := familyTestCheck(t, elsewhere, familyTestIpv4Catalog, IpFamilyV4Only, false); result.OkCount != 0 || result.H3Summary() != "silent:1" {
		t.Fatalf("UDP probes on: ok=%d h3=%q; a provider that drops UDP fails an h3 URL", result.OkCount, result.H3Summary())
	}
}

// The pool's wire format: a URL that says nothing about families keeps its
// shape (so its pool version), the marks round-trip, ipv4: false without
// ipv6: true serves no family and is refused, and the country settings are
// read case-insensitively, with only an explicit false disabling UDP.
func TestUrlProbeFamilyMarksOnTheWire(t *testing.T) {
	plain := Destination{Name: "synthetic", Class: ClassSite, Url: "https://plain.example/"}
	raw, err := json.Marshal(plain)
	if err != nil || strings.Contains(string(raw), "ipv4") || strings.Contains(string(raw), "ipv6") {
		t.Fatalf("an unmarked URL changed shape: %s %v", raw, err)
	}
	marked := h3TestDestinations(t, `[{"name":"m","class":"site","url":"https://m.example/","ipv6":true,"ipv4":false}]`)[0]
	if !marked.ServesIpv6() || marked.ServesIpv4() || marked.Validate() != nil {
		t.Fatalf("marks lost: ipv6=%t ipv4=%t err=%v", marked.ServesIpv6(), marked.ServesIpv4(), marked.Validate())
	}
	if !plain.ServesIpv4() || plain.ServesIpv6() {
		t.Fatal("an unmarked URL must serve IPv4 alone")
	}
	none := h3TestDestinations(t, `[{"name":"n","class":"site","url":"https://n.example/","ipv4":false}]`)[0]
	if err := none.Validate(); err == nil || !strings.Contains(err.Error(), "no IP family") {
		t.Fatalf("ipv4: false without ipv6: true was accepted: %v", err)
	}
	pool := &Pool{}
	if err := json.Unmarshal([]byte(`{"destinations":[],"country_settings":{"ru":{"udp_probes":false},"de":{"udp_probes":true},"fr":{}}}`), pool); err != nil {
		t.Fatal(err)
	}
	if !pool.UdpProbesDisabled("ru") || !pool.UdpProbesDisabled(" RU ") || pool.UdpProbesDisabled("de") || pool.UdpProbesDisabled("fr") ||
		pool.UdpProbesDisabled("") || pool.UdpProbesDisabled("cn") || (*Pool)(nil).UdpProbesDisabled("ru") {
		t.Fatal("country settings misread")
	}
	pool.Destinations = []Destination{plain}
	pool.CountrySettings["RU"] = CountrySettings{}
	if err := ValidatePool(pool); err == nil {
		t.Fatal("a country settings key that is not a lower-case code was accepted")
	}
}

// A dual-stack provider's turn can make four legs, each bounded by one
// per-request timeout, so its run budget counts four; an IPv4 provider's two.
func TestUrlProbeRunBudgetCountsEveryFamilysLegs(t *testing.T) {
	options := Options{UrlProbe: true, LoadAttempts: 1, PerRequestTimeout: DefaultPerRequestTimeout}
	ipv4 := options.RunBudget(1)
	options.ProviderIpFamily = IpFamilyDualstack
	if dual := options.RunBudget(1); dual-ipv4 != 2*DefaultPerRequestTimeout {
		t.Fatalf("dual-stack budget %s, IPv4 %s; want two more legs", dual, ipv4)
	}
}

// The evidence a server accepts: an IPv6 leg only for a URL that serves IPv6
// and a provider that advertises it; a prober never sets a verdict; a
// success with a failed IPv6 leg needs the server's place verdict.
func TestUrlProbeIpv6EvidenceValidation(t *testing.T) {
	path := familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestContent())
	failed := familyTestCheck(t, path, familyTestDualCatalog, IpFamilyDualstack, false).UrlProbeEvidence
	if failed.HasLegVerdict() {
		t.Fatal("a prober's evidence carries a verdict")
	}
	copyOf := func(evidence *UrlProbeEvidence) *UrlProbeEvidence {
		copied := *evidence
		leg := *evidence.Ipv6
		copied.Ipv6 = &leg
		return &copied
	}
	if err := failed.ValidateOutcome(1, 1, false); err == nil {
		t.Fatal("a success with an unjudged failed IPv6 leg was accepted")
	}
	place := copyOf(failed)
	place.Ipv6.Verdict = UrlProbeH3VerdictPlace
	if !place.HasLegVerdict() || place.ValidateOutcome(1, 1, false) != nil || place.ValidateOutcome(0, 1, false) != nil {
		t.Fatalf("a place-judged IPv6 failure: %v / %v", place.ValidateOutcome(1, 1, false), place.ValidateOutcome(0, 1, false))
	}
	provider := copyOf(failed)
	provider.Ipv6.Verdict = UrlProbeH3VerdictProvider
	if provider.ValidateOutcome(1, 1, false) == nil || provider.ValidateOutcome(0, 1, false) != nil {
		t.Fatal("a provider-judged IPv6 failure must fail its outcome")
	}
	undeclared := copyOf(failed)
	undeclared.Destination.Ipv6 = false
	if undeclared.Validate() == nil {
		t.Fatal("an IPv6 leg for a URL that declares no IPv6 was accepted")
	}
	unadvertised := copyOf(failed)
	unadvertised.IpFamily = ""
	if unadvertised.Validate() == nil {
		t.Fatal("an IPv6 leg for a provider that advertises no IPv6 was accepted")
	}
	stage := copyOf(failed)
	stage.FailureStage = ""
	if stage.ValidateOutcome(0, 1, false) == nil {
		t.Fatal("an outcome whose stage hides its failed IPv6 leg was accepted")
	}
	disabled := copyOf(failed)
	disabled.UdpProbesDisabled = true
	if disabled.Validate() == nil {
		t.Fatal("UDP probes disabled beside an HTTP/3 leg was accepted")
	}
}

// A catalog whose only IPv4 URL serves no IPv6, beside URLs that serve IPv6
// alone (ipv4: false, like the IPv6-literal twin of cloudflare-trace) and one
// that serves both.
const familyTestStandInCatalog = `[{"name":"synthetic-v4","class":"site","url":"https://v4.example/","expect":"body","h3":true},` +
	`{"name":"synthetic-v6-trace","class":"site","url":"https://[2606:4700:4700::1111]/","expect":"body","h3":true,"ipv4":false,"ipv6":true},` +
	`{"name":"synthetic-v6-plain","class":"site","url":"https://v6plain.example/","expect":"body","ipv4":false,"ipv6":true}]`

// A dual-stack provider's turn that draws a URL without AAAA makes its IPv6
// legs on a stand-in: a URL of the same lane that serves IPv6 alone
// (ipv4: false), drawn by the turn's own draw. The IPv4 legs load the drawn
// URL through its A record, and the IPv6 legs load the stand-in through its
// AAAA record (or its IPv6 literal), with the stand-in's own h3; the evidence
// names the stand-in beside the load's URL, and the load passes only if every
// leg passes. Before stand-ins such a turn never measured IPv6 (no_url).
func TestUrlProbeDualStackTurnMakesItsIpv6LegsOnAStandIn(t *testing.T) {
	seen := map[string]bool{}
	for seed := int64(0); seed < 40; seed++ {
		path := familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestContent())
		result, err := Check(context.Background(), &http.Client{Transport: path}, Options{
			UrlProbe: true, Destinations: h3TestDestinations(t, familyTestStandInCatalog), Rand: rand.New(rand.NewSource(seed)),
			ProviderIpFamily: IpFamilyDualstack,
		})
		if err != nil {
			t.Fatal(err)
		}
		evidence := result.UrlProbeEvidence
		if err := evidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
			t.Fatalf("seed %d: the evidence fails ingest validation: %v", seed, err)
		}
		if evidence.Destination.Name != "synthetic-v4" || evidence.Ipv6 == nil || evidence.Ipv6.Destination == nil {
			t.Fatalf("seed %d: drew %q with IPv6 legs %+v; want the IPv4 URL, its IPv6 legs on a stand-in", seed, evidence.Destination.Name, evidence.Ipv6)
		}
		standIn := *evidence.Ipv6.Destination
		seen[standIn.Name] = true
		host := strings.TrimSuffix(strings.TrimPrefix(standIn.Url, "https://"), "/")
		want := []string{"tcp4 v4.example", "udp4 v4.example", "tcp6 " + host}
		if standIn.H3 {
			want = append(want, "udp6 "+host)
		}
		if got := path.madeTargets(); !slices.Equal(got, want) {
			t.Fatalf("seed %d: legs %v, want %v", seed, got, want)
		}
		if result.OkCount != 1 || result.Ipv6Summary() != "ok:1" || evidence.Ipv6Destination().Name != standIn.Name || (evidence.Ipv6.H3 != nil) != standIn.H3 {
			t.Fatalf("seed %d: ok=%d ipv6=%q stand-in %q h3 leg %v", seed, result.OkCount, result.Ipv6Summary(), standIn.Name, evidence.Ipv6.H3)
		}
		wantH3 := ""
		if standIn.H3 {
			wantH3 = "ok:1"
		}
		if result.Ipv6H3Summary() != wantH3 {
			t.Fatalf("seed %d: ipv6_h3=%q on stand-in %q, want %q", seed, result.Ipv6H3Summary(), standIn.Name, wantH3)
		}
	}
	if !seen["synthetic-v6-trace"] || !seen["synthetic-v6-plain"] {
		t.Fatalf("stand-ins drawn %v; the draw covers every URL of the lane that serves IPv6", seen)
	}

	// the stand-in's IPv6 failure fails the load like the load's own would
	path := familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestContent())
	result, err := Check(context.Background(), &http.Client{Transport: path}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, familyTestStandInCatalog), Rand: rand.New(rand.NewSource(3)),
		ProviderIpFamily: IpFamilyDualstack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OkCount != 0 || result.Checks[0].FailureStage != FailureStageIpv6 || result.UrlProbeEvidence.Ipv6.Tcp != UrlProbeIpv6TcpFailed ||
		result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, false) != nil {
		t.Fatalf("ok=%d stage=%q ipv6=%+v; a stand-in's failed IPv6 leg fails the load", result.OkCount, result.Checks[0].FailureStage, result.UrlProbeEvidence.Ipv6)
	}
}

// The stand-in is the turn's: the same seed draws the same one; an IPv4
// provider never gets one; a turn whose URL serves IPv6 makes its IPv6 legs on
// that URL; a URL that serves both families never stands in for another, so a
// catalog without ipv4: false URLs measures IPv6 only where the drawn URL has
// AAAA; a country turn draws from its country's IPv6-only URLs when there are
// any; and a security recheck makes no IPv6 leg at all.
func TestUrlProbeStandInIsDrawnOnlyForIpv6AndFromTheTurnsLane(t *testing.T) {
	draw := func(options Options, seed int64) (*Result, *familyTestTransport) {
		t.Helper()
		path := familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestContent())
		options.UrlProbe, options.Rand = true, rand.New(rand.NewSource(seed))
		result, err := Check(context.Background(), &http.Client{Transport: path}, options)
		if err != nil {
			t.Fatal(err)
		}
		return result, path
	}
	general := h3TestDestinations(t, familyTestStandInCatalog)
	for seed := int64(0); seed < 10; seed++ {
		first, _ := draw(Options{Destinations: general, ProviderIpFamily: IpFamilyDualstack}, seed)
		again, _ := draw(Options{Destinations: general, ProviderIpFamily: IpFamilyDualstack}, seed)
		if first.UrlProbeEvidence.Ipv6Destination().Name != again.UrlProbeEvidence.Ipv6Destination().Name {
			t.Fatalf("seed %d drew two stand-ins", seed)
		}
		v4, path := draw(Options{Destinations: general, ProviderIpFamily: IpFamilyV4Only}, seed)
		if v4.UrlProbeEvidence.Ipv6 != nil || !slices.Equal(path.madeLegs(), []string{"tcp4", "udp4"}) {
			t.Fatalf("seed %d: an IPv4 provider made legs %v", seed, path.madeLegs())
		}
	}
	both := h3TestDestinations(t, `[{"name":"synthetic-both","class":"site","url":"https://both.example/","expect":"body","ipv6":true},`+
		`{"name":"synthetic-v6-plain","class":"site","url":"https://v6plain.example/","expect":"body","ipv4":false,"ipv6":true}]`)
	for seed := int64(0); seed < 10; seed++ {
		result, path := draw(Options{Destinations: both, ProviderIpFamily: IpFamilyDualstack}, seed)
		if result.UrlProbeEvidence.Ipv6 == nil || result.UrlProbeEvidence.Ipv6.Destination != nil ||
			!slices.Equal(path.madeTargets(), []string{"tcp4 both.example", "tcp6 both.example"}) {
			t.Fatalf("seed %d: legs %v; a URL that serves IPv6 makes its own IPv6 legs", seed, path.madeTargets())
		}
	}
	mixed := h3TestDestinations(t, `[{"name":"synthetic-v4","class":"site","url":"https://v4.example/","expect":"body"},`+
		`{"name":"synthetic-both","class":"site","url":"https://both.example/","expect":"body","ipv6":true}]`)
	for seed := int64(0); seed < 20; seed++ {
		result, path := draw(Options{Destinations: mixed, ProviderIpFamily: IpFamilyDualstack}, seed)
		if result.UrlProbeEvidence.Destination.Name == "synthetic-v4" && (result.UrlProbeEvidence.Ipv6 != nil || result.Ipv6Summary() != "no_url:1" ||
			!slices.Equal(path.madeTargets(), []string{"tcp4 v4.example"})) {
			t.Fatalf("seed %d: legs %v; a URL that serves both families stood in for another", seed, path.madeTargets())
		}
	}
	country := h3TestDestinations(t, `[{"name":"synthetic-local","class":"site","url":"https://local.example/","expect":"body"},`+
		`{"name":"synthetic-local-v6","class":"site","url":"https://local6.example/","expect":"body","ipv4":false,"ipv6":true}]`)
	countryTurns := 0
	for seed := int64(0); seed < 40; seed++ {
		result, _ := draw(Options{Destinations: general, CountryDestinations: country, ProviderIpFamily: IpFamilyDualstack}, seed)
		if result.UrlSource != "country" {
			continue
		}
		countryTurns++
		if result.UrlProbeEvidence.Destination.Name != "synthetic-local" || result.UrlProbeEvidence.Ipv6Destination().Name != "synthetic-local-v6" {
			t.Fatalf("seed %d: a country turn drew %q with stand-in %q", seed, result.UrlProbeEvidence.Destination.Name, result.UrlProbeEvidence.Ipv6Destination().Name)
		}
	}
	if countryTurns == 0 {
		t.Fatal("no seed drew the country lane")
	}
	recheck := []Destination{general[0]}
	for seed := int64(0); seed < 20; seed++ {
		result, path := draw(Options{Destinations: general, SecurityDestinations: recheck, ProviderIpFamily: IpFamilyDualstack}, seed)
		if result.UrlSource == UrlSourceSecurityRecheck && (result.UrlProbeEvidence.Ipv6 != nil || slices.Contains(path.madeLegs(), "tcp6")) {
			t.Fatalf("seed %d: a security recheck made legs %v", seed, path.madeLegs())
		}
	}
}

// What a server accepts of a stand-in: a URL that serves IPv6, named apart
// from the outcome's own URL, for an outcome whose URL serves none; an IPv6
// HTTP/3 leg only when the stand-in declares h3, which the outcome's URL need
// not; and an outcome's URL is never one that serves no IPv4.
func TestUrlProbeIpv6StandInEvidenceValidation(t *testing.T) {
	path := familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestContent())
	var evidence *UrlProbeEvidence
	for seed := int64(0); evidence == nil && seed < 200; seed++ {
		result, err := Check(context.Background(), &http.Client{Transport: path}, Options{
			UrlProbe: true, Destinations: h3TestDestinations(t, familyTestStandInCatalog), Rand: rand.New(rand.NewSource(seed)),
			ProviderIpFamily: IpFamilyDualstack,
		})
		if err != nil {
			t.Fatal(err)
		}
		if leg := result.UrlProbeEvidence.Ipv6; leg != nil && leg.Destination != nil && leg.Destination.H3 && leg.H3 != nil {
			evidence = result.UrlProbeEvidence
		}
	}
	if evidence == nil {
		t.Fatal("no turn made its IPv6 legs on an h3 stand-in")
	}
	if err := evidence.ValidateOutcome(1, 1, false); err != nil {
		t.Fatalf("a stand-in outcome: %v", err)
	}
	mutated := func(change func(*UrlProbeEvidence)) *UrlProbeEvidence {
		copied := *evidence
		leg := *evidence.Ipv6
		standIn := *evidence.Ipv6.Destination
		leg.Destination = &standIn
		copied.Ipv6 = &leg
		change(&copied)
		return &copied
	}
	for name, change := range map[string]func(*UrlProbeEvidence){
		"a stand-in that serves no IPv6":            func(e *UrlProbeEvidence) { e.Ipv6.Destination.Ipv6 = false },
		"a stand-in for a URL that serves IPv6":     func(e *UrlProbeEvidence) { e.Destination.Ipv6 = true },
		"a stand-in named as the outcome's URL":     func(e *UrlProbeEvidence) { e.Ipv6.Destination.Name = e.Destination.Name },
		"a stand-in without a URL":                  func(e *UrlProbeEvidence) { e.Ipv6.Destination.Url = "" },
		"an IPv6 HTTP/3 leg on a stand-in sans h3":  func(e *UrlProbeEvidence) { e.Ipv6.Destination.H3 = false },
		"an outcome on a URL that serves no IPv4":   func(e *UrlProbeEvidence) { no := false; e.Destination.Ipv4 = &no },
		"UDP disabled beside the stand-in's HTTP/3": func(e *UrlProbeEvidence) { e.UdpProbesDisabled = true },
	} {
		if mutated(change).Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// the outcome's URL without h3, its stand-in with it: the IPv6 HTTP/3 leg
	// is the stand-in's
	withoutH3 := mutated(func(e *UrlProbeEvidence) { e.Destination.H3, e.H3 = false, nil })
	if err := withoutH3.ValidateOutcome(1, 1, false); err != nil {
		t.Fatalf("an HTTP/3 leg on an h3 stand-in of a URL without h3: %v", err)
	}
}

// The tunnel's report of an empty answer for the leg's record: the provider's
// resolver answered, with no address of the family (providertunnel's resolve).
func familyTestNoRecord(host string) error {
	return &echoStageSyntheticError{stage: "dial_dns", cause: &net.DNSError{Err: "provider URL resolution exhausted", Name: host, IsNotFound: true}}
}

// The survey's findings (factury reports/Quality probe IPv6 support.md): AAAA
// answers vary by resolver and place (bunny.net, flipkart), and a redirect
// hop can have no AAAA where the first hop has one (mail.ru to login.vk.ru).
// Either way the IPv6 leg is not measured and marked no_aaaa: the URL's
// outcome, never the provider's, so the load passes on its IPv4 legs. Every
// hop of the IPv6 leg is granted IPv6 alone: the hop without AAAA is never
// dialed over IPv4. A resolution that timed out is not measured either, and
// says nothing about the URL's records.
func TestUrlProbeIpv6LegWithoutAaaaIsTheUrlsNotTheProviders(t *testing.T) {
	noAaaa := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return nil, familyTestNoRecord(request.URL.Hostname())
	})
	direct := familyTestPath(familyTestContent(), noAaaa, familyTestContent(), familyTestContent())
	result := familyTestCheck(t, direct, familyTestDualCatalog, IpFamilyDualstack, false)
	evidence := result.UrlProbeEvidence
	if result.OkCount != 1 || result.Ipv6Summary() != "not_measured:1" || evidence.Ipv6 == nil || !evidence.Ipv6.NoAaaa ||
		evidence.Ipv6.Tcp != UrlProbeIpv6TcpNotMeasured || evidence.Ipv6.FailureStage != "dial_dns" || !result.Checks[0].Ipv6.AddressRecordMissing {
		t.Fatalf("ok=%d ipv6=%q evidence=%+v; an empty AAAA is the URL's, not the provider's", result.OkCount, result.Ipv6Summary(), evidence.Ipv6)
	}

	timedOut := familyTestPath(familyTestContent(), roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return nil, &echoStageSyntheticError{stage: "dial_dns", cause: &net.DNSError{Err: "provider URL resolution timed out", Name: request.URL.Hostname(), IsTimeout: true}}
	}), familyTestContent(), familyTestContent())
	if result := familyTestCheck(t, timedOut, familyTestDualCatalog, IpFamilyDualstack, false); result.OkCount != 1 ||
		result.UrlProbeEvidence.Ipv6.NoAaaa || result.UrlProbeEvidence.Ipv6.Tcp != UrlProbeIpv6TcpNotMeasured {
		t.Fatalf("a timed-out resolution: ok=%d ipv6=%+v; not measured, and no claim about the URL's records", result.OkCount, result.UrlProbeEvidence.Ipv6)
	}

	redirecting := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == "dual.example" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://login.example/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return nil, familyTestNoRecord(request.URL.Hostname())
	})
	hop := familyTestPath(familyTestContent(), redirecting, familyTestContent(), familyTestContent())
	result = familyTestCheck(t, hop, familyTestDualCatalog, IpFamilyDualstack, false)
	if want := []string{"tcp4 dual.example", "udp4 dual.example", "tcp6 dual.example", "tcp6 login.example"}; !slices.Equal(hop.madeTargets(), want) {
		t.Fatalf("legs %v, want %v: every hop of the IPv6 leg is granted IPv6 alone", hop.madeTargets(), want)
	}
	if leg := result.UrlProbeEvidence.Ipv6; result.OkCount != 1 || !leg.NoAaaa || leg.RedirectCount != 1 || leg.Tcp != UrlProbeIpv6TcpNotMeasured {
		t.Fatalf("ok=%d ipv6=%+v; a redirect hop without AAAA is the URL's outcome", result.OkCount, leg)
	}
	tampered := *result.UrlProbeEvidence
	leg := *tampered.Ipv6
	leg.Tcp, leg.FailureStage = UrlProbeIpv6TcpFailed, "dial_tcp"
	tampered.Ipv6 = &leg
	if tampered.Validate() == nil {
		t.Fatal("no_aaaa on a leg that resolved its AAAA was accepted")
	}
}

// The survey: an echo URL returns the exit's IPv6 address over IPv6. The
// IPv6 leg keeps what it echoed to itself: the provider's sampled exit, which
// the prober submits for its location, is the IPv4 HTTPS leg's alone, so the
// two families' answers never disagree into no exit, nor does an IPv6
// address stand for the exit the location reads.
func TestUrlProbeIpv6EchoNeverBecomesTheProvidersExit(t *testing.T) {
	echo := func(address string) http.RoundTripper {
		return roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(address + "\n"))}, nil
		})
	}
	catalog := `[{"name":"synthetic-echo","class":"connectivity","url":"https://echo.example/ip","expect":"body","verify":{"kind":"ip_text"},"ipv6":true}]`
	path := familyTestPath(echo("198.51.100.7"), echo("2001:db8::7"), nil, nil)
	options := Options{UrlProbe: true, Destinations: h3TestDestinations(t, catalog), Rand: rand.New(rand.NewSource(3)), ProviderIpFamily: IpFamilyDualstack}
	options.exitAddressAllowed = func(ip netip.Addr) bool {
		return netip.MustParsePrefix("198.51.100.0/24").Contains(ip) || netip.MustParsePrefix("2001:db8::/32").Contains(ip)
	}
	result, err := Check(context.Background(), &http.Client{Transport: path}, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.OkCount != 1 || result.Ipv6Summary() != "ok:1" || result.ExitIp != "198.51.100.7" || result.ExitObservedAt.IsZero() {
		t.Fatalf("ok=%d ipv6=%q exit=%q at %s; the sampled exit is the IPv4 leg's", result.OkCount, result.Ipv6Summary(), result.ExitIp, result.ExitObservedAt)
	}
	if leg := result.Checks[0].Ipv6; leg == nil || leg.ObservedExitIp != "2001:db8::7" {
		t.Fatalf("the IPv6 leg's echo %+v; it answers with the exit's IPv6 address and keeps it", leg)
	}
}
