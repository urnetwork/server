// A URL that declares HTTP/3 is loaded twice through the same provider: HTTPS
// over TCP, then the same request over QUIC (UDP). It passes only if both do,
// so a provider that carries TCP but not UDP fails it.
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
// (ProviderHttpStage): dial_quic when the provider path brought no handshake
// to completion in time -- the signature of a provider that does not carry
// UDP -- and quic when the handshake was refused or reset. A certificate that
// does not authenticate is the ordinary tls stage.
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

// Folds a load's HTTP/3 leg into the result of its HTTPS leg, which passed.
// Every field but the verdict keeps describing the HTTPS leg, and the leg is
// kept whole beside it. A failed leg fails the load at FailureStageH3, and a
// leg that could not be measured leaves the whole load unmeasured: its HTTPS
// pass alone is not the judgement the destination asks for.
//
// The leg's TLS outcome stays inside the leg. A certificate that does not
// authenticate over QUIC fails the load, but it is neither the load's
// TlsAuthenticationFailure nor URL security evidence: TLS findings, their
// judge and its exclusions remain about the HTTPS identity.
func joinUrlProbeH3(https CheckResult, h3 CheckResult) CheckResult {
	result := https
	h3.Name, h3.Class, h3.H3Required, h3.H3 = https.Name, https.Class, false, nil
	result.H3Required = true
	result.H3 = &h3
	if h3.Ok {
		return result
	}
	result.Ok = false
	result.FailureStage = FailureStageH3
	result.Err = "HTTP/3: " + h3.Err
	result.NotMeasured = result.NotMeasured || h3.NotMeasured
	return result
}
