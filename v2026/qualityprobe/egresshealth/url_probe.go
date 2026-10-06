// Scheduled probes measure one random URL; durable scheduling owns pacing.
package egresshealth

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

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
	rng := opts.rng()
	source := "general"
	if len(opts.SecurityDestinations) > 0 && rng.Intn(2) == 0 {
		compatible = opts.SecurityDestinations
		source = "security_recheck"
	} else {
		if len(country) == 0 {
			source = "general_country_unavailable"
		} else if rng.Intn(2) == 1 {
			compatible = country
			source = "country"
		}
	}
	chosen := compatible[rng.Intn(len(compatible))]
	if source == "security_recheck" {
		chosen.Canary = false
		chosen.Incompatible = nil
	}
	opts.LoadAttempts = 1
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
			}
		}
	}
	return result, err
}
