// The public-udp signal (SIGNALS.md §16.9): one pass of the public UDP probe
// (public_udp.go, public_udp_transport.go) per cadence, its observations
// turned into findings against the pass's own settings.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// NewPublicUdpSignal implements SIGNALS.md §16.9. Enrollment is explicit.
// A successful check proves an exact authenticated transport return path, and
// for a connect api front also one /hello the api answered through it.
func NewPublicUdpSignal() Signal {
	return &signalAdapter{number: "16.9", key: "public-udp", name: "Exact public UDP/QUIC return path", probe: publicUdpProbe{settings: DefaultPublicUdpProbeSettings()}}
}

// One pass's attempts and findings. Its settings reach the matrix, the
// transport and the findings alike.
type publicUdpProbe struct {
	// nil takes the defaults
	settings *PublicUdpProbeSettings
	// Local deterministic deadline controls may replace context construction,
	// never the protocol or source. Production uses the attempt timeout.
	attemptContext func(context.Context) (context.Context, context.CancelFunc)
}

func (publicUdpProbe) id() string   { return "public/udp" }
func (publicUdpProbe) tier() string { return tierWarn }

// The settings' cadence, or the default's when it is not positive.
func (self publicUdpProbe) cadence() time.Duration {
	if self.settings != nil && 0 < self.settings.Cadence {
		return self.settings.Cadence
	}
	return DefaultPublicUdpProbeSettings().Cadence
}

func (self publicUdpProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !env.cfg.publicUdp.Enabled {
		return nil, nil
	}
	settings := self.settings
	if settings == nil {
		settings = DefaultPublicUdpProbeSettings()
	}
	requests, reason := publicUdpRequests(env.cfg.publicUdp, settings, env.cfg.hosts)
	if reason != "" {
		return []finding{{
			probeId: "public/udp", tier: tierWarn, class: "public-udp-inventory", target: "public-udp-inventory", sustain: 1,
			symptom:   "The declared public UDP path matrix is incomplete or invalid; no path was contacted.",
			mechanism: "Each service, alias, carrier and configured address family needs explicit ownership, a pinned public address, an exact port and authenticated protocol authority, and the probe settings must bound every attempt. An empty or partial matrix cannot establish healthy coverage.",
			baseline:  "An independently declared target count matches a complete explicit matrix joined to enabled host inventory.",
			observed:  "inventory_complete=false reason=" + reason,
			evidence:  "Only the fixed validation class is retained. Settings values, endpoints, names used for TLS and DNS codec authority are omitted.",
			action:    "Reconcile explicit enrollment with the intended active services and aliases under the normal settings workflow. Preserve configured family denominators and all disabled or excluded host boundaries.",
			verify:    "Require a complete matrix and a fresh authenticated exact-tuple handshake for each admitted path. No network fallback or health assertion follows from missing inventory.",
			playbook:  "SIGNALS.md §16.9",
		}}, nil
	}
	transport, available := env.runner.(publicUdpRunner)
	newAttempt := self.attemptContext
	if newAttempt == nil {
		newAttempt = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, settings.AttemptTimeout)
		}
	}
	results := make([][]finding, len(requests))
	jobs := make(chan int, len(requests))
	for index := range requests {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(max(settings.WorkerCount, 1), len(requests)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				request := requests[index]
				nonce := make([]byte, 16)
				if _, err := rand.Read(nonce); err != nil {
					results[index] = []finding{publicUdpUnknown(request, "attempt-authority-unavailable")}
					continue
				}
				request.AttemptId = hex.EncodeToString(nonce)
				if !available {
					results[index] = []finding{publicUdpUnknown(request, "source-unavailable")}
					continue
				}
				attemptCtx, cancel := newAttempt(ctx)
				observed, err := transport.publicUdp(attemptCtx, request, settings)
				// Capture deadline authority before our own cleanup cancel. A
				// source returning nil after expiry cannot establish health.
				if attemptErr := attemptCtx.Err(); err == nil && attemptErr != nil {
					err = attemptErr
				}
				cancel()
				results[index] = publicUdpFindings(request, observed, err, settings)
			}
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		// Cancellation publishes neither a partial failure set nor recovery.
		return nil, err
	}
	findings := []finding{healthyFinding("public/udp", tierWarn, "public-udp-inventory", "public-udp-inventory")}
	for _, result := range results {
		findings = append(findings, result...)
	}
	return findings, nil
}

// The base finding of one enrolled family row. A connect api row's texts
// cover its /hello as well as its handshake.
func publicUdpPathFinding(request PublicUdpRequest, class string) finding {
	mechanism := "A fresh bounded socket sends protocol-appropriate QUIC to the pinned public tuple. Exact wire-source filtering precedes any DNS translation, and normal TLS certificate/SNI verification is required. Connect H3 is custom QUIC, not HTTP/3; an api front, Alt's or connect's lb_alt front, negotiates h3."
	baseline := "The enrolled family/carrier completes a fresh authenticated handshake with positive sent/received wire evidence from the exact requested address and port."
	contextText := "This proves the selected transport path only, not provider authentication, API/application success, egress delivery or the exact DNAT generation. An explicitly published Alt direct UDP port is not a Connect-private listener leak. Explicit enrollment is not an exhaustive dynamic fleet census."
	if publicUdpCompletesApiRequest(request) {
		mechanism += " Connect's api front forwards api requests to the api blocks, so its handshake proves nothing about the api behind it: the same attempt then sends one unauthenticated GET /hello over HTTP/3 and requires the api's answer."
		baseline += " The front then answers /hello with 200 and a JSON object in the api's shape for the probed family."
		contextText = "This proves the selected transport path and one unauthenticated /hello answered through the front by an api block, not provider authentication, authenticated API success, egress delivery or the exact DNAT generation. Explicit enrollment is not an exhaustive dynamic fleet census."
	}
	return finding{
		// Healthy resolution matches target independently of frame. Keep each
		// family/protocol in the target so one healthy path cannot clear another.
		probeId: "public/udp", tier: tierWarn, class: class,
		target:    request.Host + "/public-udp/" + request.Target + "/" + request.Family + "/" + request.Service + "/" + request.Front + "/" + request.Carrier,
		frame:     "family=" + request.Family + " service=" + request.Service + " front=" + request.Front + " carrier=" + request.Carrier,
		sustain:   1,
		mechanism: mechanism,
		baseline:  baseline,
		evidence:  "Only fixed status, an HTTP status code and bounded packet counts are retained. Pinned addresses, TLS names, DNS suffixes, packet bytes, response bodies, attempt correlation tokens and raw errors are private. Unsolicited or mismatched datagrams alone cannot identify an SNAT fault.",
		context:   contextText,
		action:    "Correlate this exact private family/carrier with its enrollment, observer route, certificate authority and admitted remote listener before attributing a router or NAT cause. Do not contact excluded/shared endpoints or substitute DNS, ping, TCP, HTTPS or a static permit as proof.",
		verify:    "Require a later fresh authenticated handshake from the exact requested tuple on every admitted configured family. Missing capability, missing family route, cancellation or partial inventory remains unknown, never recovery.",
		playbook:  "SIGNALS.md §16.9 and §14.5",
	}
}

func publicUdpUnknown(request PublicUdpRequest, reason string) finding {
	finding := publicUdpPathFinding(request, "public-udp-observation")
	finding.symptom = "The enrolled public UDP transport path could not be verified from complete authenticated evidence."
	finding.observed = "verified=false reason=" + reason
	return finding
}

// Evaluates one attempt against the pass's settings, the same bounds its
// transport ran under. A connect api row is healthy only when its /hello
// completed with 200 and the api's answer; a failed /hello after an exact
// authenticated handshake is its own application finding, and resolves no
// other class of the row, since its result is not a complete one.
func publicUdpFindings(request PublicUdpRequest, observation PublicUdpObservation, err error, settings *PublicUdpProbeSettings) []finding {
	unknown := func(reason string) []finding { return []finding{publicUdpUnknown(request, reason)} }
	if observation.Failure == PublicUdpFailureSource {
		return unknown("source-unavailable")
	}
	apiRequest := publicUdpCompletesApiRequest(request)
	if observation.AttemptId != request.AttemptId || observation.RequestedTuple != publicUdpTuple(request) ||
		observation.SentPackets < 0 || observation.SentPackets > settings.MaxPacketCount ||
		observation.ReceivedPackets < 0 || observation.ReceivedPackets > settings.MaxPacketCount ||
		observation.UnexpectedPackets < 0 || observation.UnexpectedPackets > settings.MaxPacketCount-observation.ReceivedPackets ||
		observation.SentBytes < 0 || observation.SentBytes > settings.MaxByteCount ||
		observation.ReceivedBytes < 0 || observation.ReceivedBytes > settings.MaxByteCount ||
		observation.ApplicationStatus != 0 && (observation.ApplicationStatus < 100 || 599 < observation.ApplicationStatus) ||
		// a front that ends at its handshake never carries an api request
		!apiRequest && (observation.ApplicationRequested || observation.ApplicationStatus != 0 || observation.ApplicationContentTypeJson ||
			observation.ApplicationBodyValid || observation.ApplicationComplete) {
		return unknown("observation-authority-invalid")
	}
	expectedProtocol := ""
	if request.Front == "api" {
		expectedProtocol = "h3"
	}
	// the complete authenticated exact return a request may ride on
	transportVerified := observation.FreshSocket && observation.HandshakeComplete && observation.TlsVerified && observation.PeerTuple == observation.RequestedTuple &&
		observation.NegotiatedProtocol == expectedProtocol && 0 < observation.SentPackets && 0 < observation.SentBytes &&
		0 < observation.ReceivedPackets && 0 < observation.ReceivedBytes
	switch observation.Failure {
	case PublicUdpFailureNone:
	case PublicUdpFailureRoute, PublicUdpFailureSocket, PublicUdpFailureTls, PublicUdpFailureBudget, PublicUdpFailureConfiguration:
		return unknown(string(observation.Failure))
	case PublicUdpFailureTimeout, PublicUdpFailureHandshake:
		if !observation.FreshSocket || observation.SentPackets == 0 || observation.SentBytes == 0 || observation.HandshakeComplete && observation.TlsVerified {
			return unknown("handshake-outcome-unverified")
		}
		pathFinding := publicUdpPathFinding(request, "public-udp-path")
		pathFinding.sustain = 2
		pathFinding.symptom = "A bounded pinned public UDP/QUIC attempt sent traffic but did not complete its authenticated handshake."
		pathFinding.observed = fmt.Sprintf("verified=false result=%s sent_packets=%d received_packets=%d ignored_unexpected_packets=%d", observation.Failure, observation.SentPackets, observation.ReceivedPackets, observation.UnexpectedPackets)
		return []finding{pathFinding}
	case PublicUdpFailureApplication:
		// Only an exact authenticated handshake with a connect api front
		// carries the request, so anything less is unknown, never a failure
		// of the api behind the front.
		if !apiRequest || !observation.ApplicationRequested || observation.ApplicationComplete || !transportVerified {
			return unknown("application-outcome-unverified")
		}
		applicationFinding := publicUdpPathFinding(request, "public-udp-application")
		applicationFinding.sustain = 2
		applicationFinding.symptom = "A connect api front completed its authenticated handshake from the exact public tuple, but its /hello did not return the api's answer."
		applicationFinding.observed = fmt.Sprintf(
			"verified=false result=%s application_status=%d application_json=%t application_body_valid=%t sent_packets=%d received_packets=%d",
			observation.Failure, observation.ApplicationStatus, observation.ApplicationContentTypeJson, observation.ApplicationBodyValid,
			observation.SentPackets, observation.ReceivedPackets,
		)
		applicationFinding.action = "The authenticated QUIC path from the exact tuple works, so look behind it: check that connect's front on this interface routes the api name over h3 and that its api upstream blocks are ready, then read the api's own errors. An application_status of 0 means no answer arrived within the attempt bound; 502, 503 or 504 point at the front's forward to the api; any other status at the api. Do not treat this as a UDP, NAT or certificate fault."
		applicationFinding.verify = "Require a later fresh attempt from the exact requested tuple whose /hello completes with 200 and the api's JSON shape on every admitted family. A handshake-only success, missing capability or cancellation remains unknown, never recovery."
		return []finding{applicationFinding}
	default:
		return unknown("observation-authority-invalid")
	}
	if err != nil || !transportVerified {
		return unknown("authenticated-exact-return-unverified")
	}
	if apiRequest && (!observation.ApplicationRequested || !observation.ApplicationComplete || observation.ApplicationStatus != http.StatusOK ||
		!observation.ApplicationContentTypeJson || !observation.ApplicationBodyValid) {
		return unknown("application-outcome-unverified")
	}
	findings := []finding{}
	for _, class := range []string{"public-udp-path", "public-udp-observation", "public-udp-application"} {
		if class == "public-udp-application" && !apiRequest {
			continue
		}
		finding := publicUdpPathFinding(request, class)
		finding.healthy = true
		findings = append(findings, finding)
	}
	return findings
}
