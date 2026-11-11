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
	"net/http"
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
}

func (self *familyTestTransport) record(leg string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.legs = append(self.legs, leg)
}

func (self *familyTestTransport) madeLegs() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.legs)
}

func (self *familyTestTransport) ProviderUrlProbeFamilyContext(ctx context.Context, target *url.URL, redirect bool, ipVersion int) (context.Context, error) {
	self.record(fmt.Sprintf("tcp%d", ipVersion))
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
	self.parent.record(fmt.Sprintf("udp%d", ipVersion))
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
// it is never drawn while every load starts over IPv4; the rest of the
// catalog is drawn as before, whatever families the provider advertises.
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
