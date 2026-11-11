// A URL that declares HTTP/3 is loaded twice through the same provider: HTTPS
// over TCP, then the same request over QUIC (UDP). It passes only if the HTTPS
// leg passes and the provider carries the UDP leg to the site, so a provider
// that carries TCP but not UDP fails it.
package egresshealth

import (
	"context"
	"net/http"
	"time"
)

// The FailureStage of a load whose HTTPS leg passed and whose HTTP/3 leg did
// not. The leg's own stage is CheckResult.H3.FailureStage.
const FailureStageH3 = "h3"

// The H3.FailureStage of an HTTP/3 leg that could not be made because the
// load's client has no HTTP/3 path through the provider. Such a leg is not
// measured: an instrument that cannot speak HTTP/3 says nothing about UDP.
const FailureStageH3Unavailable = "h3_unavailable"

// Stages the provider's HTTP/3 transport reports for its QUIC handshake
// (ProviderHttpStage): dial_quic when not one datagram came back to the QUIC
// Initial within the handshake's bounds -- the signature of a provider that
// does not carry UDP -- and quic when the peer answered and then refused,
// reset or stalled the handshake. A TLS alert, or a certificate that does not
// authenticate, is the ordinary tls stage; the peer answered there too.
const (
	FailureStageDialQuic = "dial_quic"
	FailureStageQuic     = "quic"
)

// A provider transport that can carry a URL request over HTTP/3 through the
// same provider path as its HTTPS requests (providertunnel). The round
// tripper it returns is fresh for the leg, reuses no connection, and owns the
// UDP sockets it opens until the response body is closed or the request
// fails. It also answers the same URL-probe and measurement questions the
// HTTPS transport does (ProviderUrlProbeContext and the Provider*Unavailable
// probes). Nil means the path cannot carry HTTP/3 now.
type providerUrlProbeH3Transport interface {
	ProviderUrlProbeH3RoundTripper() http.RoundTripper
}

// The HTTP/3 leg of one URL load, made through the same provider as the
// HTTPS leg that just passed: the same destination, request profile and
// contract (fetchUrlProbe), on a client whose only transport is the provider's
// HTTP/3 path. The leg has the ordinary per-request timeout: the tunnel is
// already established, so no cold-start allowance applies.
func fetchUrlProbeH3(ctx context.Context, client *http.Client, destination Destination, timeout time.Duration, profile RequestProfile, opts Options) CheckResult {
	var transport http.RoundTripper
	if capable, ok := client.Transport.(providerUrlProbeH3Transport); ok {
		transport = capable.ProviderUrlProbeH3RoundTripper()
	}
	if transport == nil {
		return CheckResult{
			Name: destination.Name, Class: destination.Class, NotMeasured: true, FailureStage: FailureStageH3Unavailable,
			Err: "the provider path cannot carry HTTP/3",
		}
	}
	h3Client := &http.Client{Transport: transport, Timeout: client.Timeout}
	return fetchUrlProbe(ctx, h3Client, destination, timeout, profile, opts)
}

// What an HTTP/3 leg says about the provider's UDP path (UrlProbeH3Udp).
//
// Only silence is evidence against the provider: the leg sent its QUIC
// Initial through the provider and heard nothing back within the handshake
// bounds. Any answer proves the provider carries UDP to the site, whatever
// the site then did with it: a CONNECTION_CLOSE with a TLS alert (a QUIC edge
// that does not serve h3 for the hostname refuses it with 0x128 or 0x150), a
// version negotiation, a Retry, a certificate, or a response whose content or
// timing failed the contract. Those are the site's HTTP/3, not the
// provider's UDP, and the load is judged by its HTTPS leg.
const (
	UrlProbeH3UdpPassed      = "passed"
	UrlProbeH3UdpAnswered    = "answered"
	UrlProbeH3UdpSilent      = "silent"
	UrlProbeH3UdpNotMeasured = "not_measured"
)

// Classifies an HTTP/3 leg (UrlProbeH3Udp* above); "" for no leg.
func UrlProbeH3Udp(leg *CheckResult) string {
	switch {
	case leg == nil:
		return ""
	case leg.Ok:
		return UrlProbeH3UdpPassed
	case leg.NotMeasured:
		return UrlProbeH3UdpNotMeasured
	case leg.FailureStage == FailureStageDialQuic:
		return UrlProbeH3UdpSilent
	}
	switch leg.FailureStage {
	case "policy", "request_build", "dial_dns", "dial_dns_or_socket", "request_dns_timeout", FailureStageH3Unavailable,
		"local_control_registration", "local_contract_acquisition", "local_transport_admission", "tunnel_unavailable", "run_ended":
		// the leg never reached the point of sending UDP to the site
		return UrlProbeH3UdpNotMeasured
	}
	return UrlProbeH3UdpAnswered
}

// Folds a load's HTTP/3 leg into the result of its HTTPS leg, which passed.
// Every field but the verdict keeps describing the HTTPS leg, and the leg is
// kept whole beside it (UrlProbeH3Udp):
//
//   - a leg that passed, or that the site answered, proves the provider's
//     UDP path, and the load passes on its HTTPS leg;
//   - a silent leg fails the load at FailureStageH3. The server judges that
//     failure by place before it counts against the provider
//     (model.JudgeProviderEgressH3): only where established networks' exits
//     in the same country reach the same URL over QUIC is the silence the
//     provider's, and elsewhere the load is judged by HTTPS alone;
//   - a leg that could not be made leaves the whole load unmeasured: its
//     HTTPS pass alone is not the judgement the destination asks for.
//
// The leg's TLS outcome stays inside the leg. A certificate that does not
// authenticate over QUIC is neither the load's TlsAuthenticationFailure nor
// URL security evidence: TLS findings, their judge and its exclusions remain
// about the HTTPS identity.
func joinUrlProbeH3(https CheckResult, h3 CheckResult) CheckResult {
	result := https
	h3.Name, h3.Class, h3.H3Required, h3.H3 = https.Name, https.Class, false, nil
	result.H3Required = true
	result.H3 = &h3
	switch UrlProbeH3Udp(&h3) {
	case UrlProbeH3UdpPassed, UrlProbeH3UdpAnswered:
		return result
	case UrlProbeH3UdpNotMeasured:
		result.NotMeasured = true
	}
	result.Ok = false
	result.FailureStage = FailureStageH3
	result.Err = "HTTP/3: " + h3.Err
	return result
}
