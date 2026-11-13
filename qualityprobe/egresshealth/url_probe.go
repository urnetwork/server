// Scheduled probes measure one random URL; durable scheduling owns pacing.
package egresshealth

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// The UrlSource of a turn that rechecked one of the provider's stored TLS
// findings rather than drawing from the pool.
const UrlSourceSecurityRecheck = "security_recheck"

// A single URL outcome stays visible whether it passes or fails. There are no
// sleeping retry chains: the server records it and schedules the next turn.
func checkUrlProbe(ctx context.Context, client *http.Client, opts Options) (*Result, error) {
	if len(opts.Destinations) == 0 {
		return nil, ErrNoDestinations
	}
	if err := opts.urlProbePolicy().Validate(); err != nil {
		return nil, err
	}
	for _, catalog := range [][]Destination{opts.Destinations, opts.CountryDestinations} {
		for _, destination := range catalog {
			if destination.Expect == ExpectStatus && (destination.Status == http.StatusNoContent || destination.Status == http.StatusResetContent) {
				return nil, fmt.Errorf("URL catalog destination %q declares bodyless status %d; replace it with a real-content URL before probing", destination.Name, destination.Status)
			}
		}
	}
	compatible, _ := forPlace(opts.Destinations, opts.ProviderPlace)
	country, _ := forPlace(opts.CountryDestinations, opts.ProviderPlace)
	if len(compatible) == 0 {
		return nil, ErrNoDestinations
	}
	// A load's first leg is its IPv4 HTTPS leg (urlProbeServesProvider): an
	// IPv6-only provider has none, and is not measured. Otherwise the draw is
	// the catalog's, less the URLs that serve no IPv4 (ipv4: false), so the
	// URL a provider is asked for does not depend on the families it
	// advertises. The IPv6 legs of a provider that advertises IPv6 load that
	// URL when it serves IPv6, and otherwise a stand-in drawn, by the same
	// draw, from the URLs of the turn's lane that serve IPv6 alone
	// (ipv4: false; its country lane's when it has any, else the general
	// lane's): those are the only loads such a URL is ever made on.
	if !opts.ProviderIpFamily.Ipv4() {
		return nil, ErrNoUrlForProviderFamily
	}
	generalStandIns := urlProbeIpv6StandIns(compatible)
	countryStandIns := urlProbeIpv6StandIns(country)
	compatible = urlProbeDestinationsForFamily(compatible, opts.ProviderIpFamily)
	country = urlProbeDestinationsForFamily(country, opts.ProviderIpFamily)
	if len(compatible) == 0 {
		return nil, ErrNoUrlForProviderFamily
	}
	rng := opts.rng()
	source := "general"
	standIns := generalStandIns
	if len(opts.SecurityDestinations) > 0 && rng.Intn(2) == 0 {
		compatible = opts.SecurityDestinations
		source = UrlSourceSecurityRecheck
		standIns = nil
	} else {
		if len(country) == 0 {
			source = "general_country_unavailable"
		} else if rng.Intn(2) == 1 {
			compatible = country
			source = "country"
			if 0 < len(countryStandIns) {
				standIns = countryStandIns
			}
		}
	}
	chosen := compatible[rng.Intn(len(compatible))]
	var standIn *Destination
	if source == UrlSourceSecurityRecheck {
		chosen.Canary = false
		chosen.Incompatible = nil
		// A recheck asks whether the stored URL's HTTPS identity now
		// authenticates, and is never scored; it makes no HTTP/3 leg and no
		// IPv6 leg: TLS findings are about the IPv4 HTTPS identity.
		chosen.H3 = false
		chosen.Ipv6 = false
		chosen.Ipv4 = nil
	} else if opts.ProviderIpFamily.Ipv6() && !chosen.ServesIpv6() && 0 < len(standIns) {
		drawn := standIns[rng.Intn(len(standIns))]
		standIn = &drawn
	}
	opts.LoadAttempts = 1
	opts.urlProbeIpv6StandIn = standIn
	result, err := check(ctx, client, []Destination{chosen}, opts)
	if result != nil {
		result.TableTotal = len(opts.Destinations) + len(opts.CountryDestinations)
		result.UrlSource = source
		if len(result.Checks) == 1 {
			check := result.Checks[0]
			result.UrlProbeEvidence = &UrlProbeEvidence{
				PolicyVersion: UrlProbePolicyVersion, Policy: opts.urlProbePolicy(),
				Destination: chosen, MeasuredAt: opts.now(), Security: check.UrlProbeSecurity,
				ContentClassification: check.ContentClassification, PerformanceClassification: check.PerformanceClassification,
				FailureStage: check.FailureStage, ContentMatcherVersion: UrlProbeContentMatcherVersion,
				StatusCode: check.StatusCode, RedirectCount: check.RedirectCount, ByteCount: check.ByteCount, WireByteCount: check.WireByteCount, BodyComplete: check.BodyComplete,
				BodySampled:         check.BodySampled,
				WireSampleByteCount: check.WireSampleByteCount,
				RequestWritten:      check.RequestWritten, FirstByteReceived: check.FirstByteReceived,
				DnsMillis: float64(check.DnsLookupLatency) / float64(time.Millisecond), TcpMillis: float64(check.TcpConnectLatency) / float64(time.Millisecond),
				TlsMillis: float64(check.TlsHandshakeLatency) / float64(time.Millisecond), TtfbMillis: float64(check.RequestTimeToFirstByte) / float64(time.Millisecond),
				BodyMillis: float64(check.BodyDuration) / float64(time.Millisecond), BodyBitsPerSecond: check.BodyBytesPerSecond * 8,
				BodyFirstByteWaitMillis: float64(check.BodyFirstByteWait) / float64(time.Millisecond),
				H3:                      urlProbeH3EvidenceOf(check.H3),
				Ipv6:                    urlProbeIpv6EvidenceOf(check.Ipv6),
			}
			if opts.ProviderIpFamily.Ipv6() {
				result.UrlProbeEvidence.IpFamily = opts.ProviderIpFamily.Normalize()
			}
			if leg := result.UrlProbeEvidence.Ipv6; leg != nil && standIn != nil {
				// the IPv6 legs loaded the stand-in, which the evidence names
				// beside the load's own URL
				destination := *standIn
				leg.Destination = &destination
			}
			// the country's switch turned UDP off for a URL of the outcome that
			// declares h3: the load's own, or the stand-in its IPv6 legs loaded
			result.UrlProbeEvidence.UdpProbesDisabled = check.UdpProbesDisabled || (check.Ipv6UdpProbesDisabled && check.Ipv6 != nil)
			if opts.E2e != nil {
				// the session's outcome once every leg ended; positive needs
				// the IPv4 HTTPS leg to have passed through it
				result.UrlProbeEvidence.E2e = opts.E2e.Evidence(result.UrlProbeEvidence.HttpsOk(result.OkCount))
			}
		}
	}
	return result, err
}
