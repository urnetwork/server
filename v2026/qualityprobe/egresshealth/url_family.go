// A URL load is made over every IP family its provider advertises: IPv4
// through the URL's A record, IPv6 through an AAAA record, each leg forced to
// its family with no fallback to the other, on every redirect hop. A
// dual-stack provider's load passes only if its IPv4 and its IPv6 legs pass,
// over TCP and, for a URL that declares h3, over UDP as well. The IPv6 legs
// load the turn's URL when it serves IPv6, and otherwise a stand-in the turn
// draws from its lane's URLs that serve IPv6 alone (ipv4: false).
package egresshealth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// Reports a URL probe that no URL can serve: the provider advertises no IPv4,
// which every load's first leg needs (urlProbeServesProvider), or no URL of
// the catalog serves IPv4. The provider is not measured; it is not a failure.
var ErrNoUrlForProviderFamily = errors.New("egresshealth: no URL serves an IPv4 HTTPS leg for the provider's advertised families")

// The IP families a provider advertises: its proven category, which is what
// find-providers2 serves to clients (the server's ClientScore.IpFamily) and
// what connect routes a flow's family on. The values are connect.IpFamily's
// spellings. The zero value is legacy: a provider, or a server, that reports
// nothing, which carries IPv4 alone, as every provider did before families
// were proven.
type IpFamily string

const (
	IpFamilyLegacy    IpFamily = ""
	IpFamilyDualstack IpFamily = "dualstack"
	IpFamilyV4Only    IpFamily = "v4-only"
	IpFamilyV6Only    IpFamily = "v6-only"
)

// A value this build does not know reads as legacy: IPv4 alone.
func (self IpFamily) Normalize() IpFamily {
	switch self {
	case IpFamilyDualstack, IpFamilyV4Only, IpFamilyV6Only:
		return self
	default:
		return IpFamilyLegacy
	}
}

// Whether the provider advertises IPv4: every category but v6-only.
func (self IpFamily) Ipv4() bool {
	return self.Normalize() != IpFamilyV6Only
}

// Whether the provider advertises IPv6: dual-stack and v6-only.
func (self IpFamily) Ipv6() bool {
	switch self.Normalize() {
	case IpFamilyDualstack, IpFamilyV6Only:
		return true
	default:
		return false
	}
}

// Whether the URL serves the probe over IPv4, through its A record: unless
// its entry says ipv4: false.
func (self Destination) ServesIpv4() bool {
	return self.Ipv4 == nil || *self.Ipv4
}

// Whether the URL serves the probe over IPv6, through its AAAA record: only
// when its entry says ipv6: true.
func (self Destination) ServesIpv6() bool {
	return self.Ipv6
}

// The FailureStage of a load whose IPv4 legs passed and whose IPv6 leg did
// not; the leg's own stage is CheckResult.Ipv6.FailureStage.
const FailureStageIpv6 = "ipv6"

// The FailureStage of an IPv6 leg the load's client could not force onto
// IPv6: a transport with no family control, or a tunnel with no IPv6 path. An
// instrument that cannot make the leg says nothing about the provider's IPv6.
const FailureStageIpv6Unavailable = "ipv6_unavailable"

// What a provider's IPv6 HTTPS leg says (UrlProbeIpv6Tcp).
//
// Only a measured failure is evidence against the provider: the leg resolved
// the URL's AAAA record, dialed the IPv6 address through the provider and did
// not get the URL's contract over it. A leg that never reached the point of
// dialing the site over IPv6 is not measured, and neither is one the tunnel's
// loss or the run's end cut short: the URL's AAAA record was empty from the
// provider's place (a CDN that answers AAAA in some regions only), the
// resolver failed (it runs over the provider's IPv4 path, which the load's
// IPv4 leg has just used), the address was not public, or the instrument had
// no IPv6 path. A load whose IPv6 leg is not measured is judged on its IPv4
// legs: IPv6 can fail a load, never leave it unmeasured.
const (
	UrlProbeIpv6TcpPassed      = "passed"
	UrlProbeIpv6TcpFailed      = "failed"
	UrlProbeIpv6TcpNotMeasured = "not_measured"
)

// Classifies an IPv6 HTTPS leg (UrlProbeIpv6Tcp* above), whatever its HTTP/3
// leg did; "" for no leg. An HTTP/3 leg is made only once the HTTPS leg of
// its family passed.
func UrlProbeIpv6Tcp(leg *CheckResult) string {
	switch {
	case leg == nil:
		return ""
	case leg.Ok || leg.H3 != nil:
		return UrlProbeIpv6TcpPassed
	case leg.NotMeasured:
		return UrlProbeIpv6TcpNotMeasured
	}
	switch leg.FailureStage {
	case "policy", "request_build", "dial_dns", "dial_dns_or_socket", "request_dns_timeout", FailureStageIpv6Unavailable,
		"local_control_registration", "local_contract_acquisition", "local_transport_admission", "tunnel_unavailable", "run_ended",
		"request_canceled":
		return UrlProbeIpv6TcpNotMeasured
	}
	return UrlProbeIpv6TcpFailed
}

// Whether an IPv6 leg fails its load: its HTTPS leg failed, or its HTTPS leg
// passed and its HTTP/3 leg was silent. A leg that was not measured, in either
// transport, fails nothing.
func urlProbeIpv6LegFailed(leg *CheckResult) bool {
	switch UrlProbeIpv6Tcp(leg) {
	case UrlProbeIpv6TcpFailed:
		return true
	case UrlProbeIpv6TcpPassed:
		return UrlProbeH3Udp(leg.H3) == UrlProbeH3UdpSilent
	}
	return false
}

// The legs one URL load makes, from the provider's families, the families
// and h3 flags of the URL and of its IPv6 stand-in, and the provider's
// country's UDP setting.
type urlProbeLegPlan struct {
	// The URL the IPv6 legs load: the load's own when it serves IPv6, else the
	// turn's stand-in (Options.urlProbeIpv6StandIn); nil when the provider
	// advertises no IPv6 or neither exists.
	ipv6 *Destination
	// The provider advertises IPv6 and no URL of the turn serves it: the
	// family is not measured on this load, never failed.
	ipv6NoUrl bool
	// The IPv4 HTTP/3 leg: the load's URL declares h3 and the provider's
	// country does not disable UDP probes.
	h3 bool
	// The load's URL declares h3 and the provider's country disables UDP
	// probes (Pool.CountrySettings): no IPv4 HTTP/3 leg, and the IPv4 legs are
	// judged on HTTPS alone, as a URL without h3 is.
	udpProbesDisabled bool
	// The same for the IPv6 legs' URL.
	ipv6H3                bool
	ipv6UdpProbesDisabled bool
}

// The plan of a load whose primary leg is IPv4 (urlProbeServesProvider holds).
func (self Options) urlProbeLegPlan(destination Destination) urlProbeLegPlan {
	family := self.ProviderIpFamily
	plan := urlProbeLegPlan{
		h3:                destination.H3 && !self.UdpProbesDisabled,
		udpProbesDisabled: destination.H3 && self.UdpProbesDisabled,
	}
	if !family.Ipv6() {
		return plan
	}
	switch {
	case destination.ServesIpv6():
		ipv6 := destination
		plan.ipv6 = &ipv6
	case self.urlProbeIpv6StandIn != nil && self.urlProbeIpv6StandIn.ServesIpv6():
		ipv6 := *self.urlProbeIpv6StandIn
		plan.ipv6 = &ipv6
	default:
		plan.ipv6NoUrl = true
		return plan
	}
	plan.ipv6H3 = plan.ipv6.H3 && !self.UdpProbesDisabled
	plan.ipv6UdpProbesDisabled = plan.ipv6.H3 && self.UdpProbesDisabled
	return plan
}

// Whether a URL can be drawn as a load's URL for the provider: its first leg,
// the one site tallies, TLS findings and site retirement read, is the IPv4
// HTTPS leg, so the provider advertises IPv4 and the URL serves it. IPv6 legs
// follow it. A provider that advertises IPv6 alone has no such leg (the URL
// lane does not claim IPv6-only providers), and a URL whose entry says
// ipv4: false is never a load's URL: it serves IPv6 legs only, as a stand-in
// (urlProbeIpv6StandIns).
func urlProbeServesProvider(destination Destination, family IpFamily) bool {
	return family.Ipv4() && destination.ServesIpv4()
}

// The URLs of a catalog the provider can be probed on (urlProbeServesProvider).
func urlProbeDestinationsForFamily(destinations []Destination, family IpFamily) []Destination {
	served := make([]Destination, 0, len(destinations))
	for _, destination := range destinations {
		if urlProbeServesProvider(destination, family) {
			served = append(served, destination)
		}
	}
	return served
}

// The URLs of a lane, already compatible with the provider's place, that an
// IPv6 leg can stand in on when the URL a turn drew serves no IPv6: the ones
// that serve IPv6 alone (ipv6: true, ipv4: false), such as the IPv6-literal
// twin of an IPv4-literal URL. They are never a load's URL, so this is the
// only way they are loaded. A URL that serves both families is loaded as
// itself and never stands in, so a catalog without ipv4: false URLs measures
// IPv6 only on the turns whose URL has AAAA, and the marks stage the rollout.
func urlProbeIpv6StandIns(destinations []Destination) []Destination {
	standIns := make([]Destination, 0, len(destinations))
	for _, destination := range destinations {
		if destination.ServesIpv6() && !destination.ServesIpv4() {
			standIns = append(standIns, destination)
		}
	}
	return standIns
}

// Whether a URL request failed on its host's empty answer for the record of
// its family: the provider's resolver answered, authoritatively, with no
// address (providertunnel reports it as a net.DNSError that IsNotFound). For an
// IPv6 leg that is a URL whose AAAA comes and goes by resolver and place
// (bunny.net, flipkart), or a redirect hop with no AAAA (mail.ru to
// login.vk.ru): the URL's outcome, never the provider's. The leg is not
// measured (dial_dns), and the taskworker counts it against the URL
// (urnetwork_url_probe_ipv6_destination_legs_total, outcome no_aaaa).
func urlProbeAddressRecordMissing(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// The IP version a URL request is forced to: 4 or 6, carried by the
// request's context from the load to its transport's grant.
type urlProbeIpVersionKey struct{}

func withUrlProbeIpVersion(ctx context.Context, ipVersion int) context.Context {
	return context.WithValue(ctx, urlProbeIpVersionKey{}, ipVersion)
}

// The IP version a URL request is forced to, 4 when the load set none.
func urlProbeIpVersion(ctx context.Context) int {
	if ipVersion, ok := ctx.Value(urlProbeIpVersionKey{}).(int); ok && ipVersion == 6 {
		return 6
	}
	return 4
}

// A provider transport that can force a URL request onto one IP family
// (providertunnel): its grant resolves only the family's record (A for 4,
// AAAA for 6) and dials only that family's addresses, with no fallback across
// families, for every hop of the request.
type providerUrlProbeFamilyTransport interface {
	ProviderUrlProbeFamilyContext(ctx context.Context, target *url.URL, redirect bool, ipVersion int) (context.Context, error)
}

// One URL load's every leg, in order: the IPv4 HTTPS leg (with the URL's
// first-request timeout, which can carry a cold tunnel's establishment); its
// HTTP/3 leg once it passed; then, for a provider that advertises IPv6 with a
// URL of the turn that serves it (urlProbeLegPlan), the IPv6 HTTPS leg once
// the IPv4 HTTPS leg passed, and its HTTP/3 leg once that passed. Every leg
// after the first has the ordinary per-request timeout: the tunnel is warm.
// An IPv4 HTTP/3 leg that was silent does not stop the IPv6 legs: whether its
// silence counts is the server's place judgement.
func fetchUrlProbeLoad(ctx context.Context, client *http.Client, destination Destination, firstTimeout time.Duration, legTimeout time.Duration, profile RequestProfile, opts Options) CheckResult {
	plan := opts.urlProbeLegPlan(destination)
	ipv4Ctx := withUrlProbeIpVersion(ctx, 4)
	httpsCtx := ipv4Ctx
	if opts.E2e != nil {
		// Fence the request's writes with the same live sending epoch. A seal
		// observed only after the writes cannot prove which epoch carried them.
		var started sync.Once
		var written sync.Once
		httpsCtx = httptrace.WithClientTrace(ipv4Ctx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) {
				started.Do(opts.E2e.RequestStarted)
			},
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					written.Do(opts.E2e.RequestWritten)
				}
			},
		})
	}
	result := fetchUrlProbe(httpsCtx, client, destination, firstTimeout, profile, opts)
	result.UdpProbesDisabled = plan.udpProbesDisabled
	if plan.h3 {
		result.H3Required = true
		if result.Ok {
			result = joinUrlProbeH3(result, fetchUrlProbeH3(ipv4Ctx, client, destination, legTimeout, profile, opts))
		}
	}
	result.Ipv6NoUrl = plan.ipv6NoUrl
	if plan.ipv6 != nil {
		result.Ipv6Required = true
		result.Ipv6H3Required, result.Ipv6UdpProbesDisabled = plan.ipv6H3, plan.ipv6UdpProbesDisabled
		if result.HttpsOk() && !result.NotMeasured {
			result = joinUrlProbeIpv6(result, fetchUrlProbeIpv6(ctx, client, *plan.ipv6, legTimeout, profile, opts, plan.ipv6H3))
		}
	}
	return result
}

// The IPv6 legs of one load, on the URL they load (the load's own or its
// stand-in): the HTTPS leg through its AAAA record and, for a URL that
// declares h3 where UDP probes run, the same request over HTTP/3 over IPv6
// once that passed.
func fetchUrlProbeIpv6(ctx context.Context, client *http.Client, destination Destination, timeout time.Duration, profile RequestProfile, opts Options, h3 bool) CheckResult {
	ipv6Ctx := withUrlProbeIpVersion(ctx, 6)
	leg := fetchUrlProbe(ipv6Ctx, client, destination, timeout, profile, opts)
	if h3 {
		leg.H3Required = true
		if leg.Ok {
			leg = joinUrlProbeIpv6H3(leg, fetchUrlProbeH3(ipv6Ctx, client, destination, timeout, profile, opts))
		}
	}
	return leg
}

// Folds an IPv6 HTTPS leg's HTTP/3 leg into it, as joinUrlProbeH3 does for
// IPv4 except for one rule: an IPv6 HTTP/3 leg that was not measured leaves
// the IPv6 HTTPS leg's verdict standing, since IPv6 never leaves a load
// unmeasured. A silent one fails the IPv6 leg at FailureStageH3, judged by
// IPv6 place on the server; any answer proves UDP over IPv6.
func joinUrlProbeIpv6H3(https CheckResult, h3 CheckResult) CheckResult {
	result := https
	h3.Name, h3.Class, h3.H3Required, h3.H3 = https.Name, https.Class, false, nil
	h3.Ipv6Required, h3.Ipv6, h3.Ipv6NoUrl, h3.UdpProbesDisabled = false, nil, false, false
	h3.Ipv6H3Required, h3.Ipv6UdpProbesDisabled = false, false
	result.H3Required = true
	result.H3 = &h3
	if UrlProbeH3Udp(&h3) == UrlProbeH3UdpSilent {
		result.Ok = false
		result.FailureStage = FailureStageH3
		result.Err = "HTTP/3: " + h3.Err
	}
	return result
}

// Folds a load's IPv6 legs into its IPv4 result. Every field but the verdict
// keeps describing the IPv4 HTTPS leg, and the IPv6 legs are kept whole beside
// it (CheckResult.Ipv6):
//
//   - IPv6 legs that passed, or were not measured, leave the load's verdict
//     to its IPv4 legs;
//   - a failed IPv6 leg fails the load. The load's FailureStage names its first
//     failed leg: FailureStageIpv6 when its IPv4 legs passed, and the failure
//     it already had otherwise (a silent IPv4 HTTP/3 leg). The server judges an
//     IPv6 failure by IPv6 place before it counts against the provider.
//
// The IPv6 legs' TLS outcomes stay inside them: TLS findings, their judge and
// their exclusions stay about the IPv4 HTTPS identity.
func joinUrlProbeIpv6(result CheckResult, leg CheckResult) CheckResult {
	leg.Name, leg.Class = result.Name, result.Class
	leg.Ipv6Required, leg.Ipv6, leg.Ipv6NoUrl, leg.UdpProbesDisabled = false, nil, false, false
	leg.Ipv6H3Required, leg.Ipv6UdpProbesDisabled = false, false
	result.Ipv6 = &leg
	if !urlProbeIpv6LegFailed(&leg) {
		return result
	}
	if result.Ok {
		result.Ok = false
		result.FailureStage = FailureStageIpv6
		result.Err = "IPv6: " + leg.Err
	} else {
		result.Err += "; IPv6: " + leg.Err
	}
	return result
}
