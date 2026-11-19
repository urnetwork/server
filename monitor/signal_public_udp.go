// The public-udp signal (SIGNALS.md §16.9): one pass of the public UDP probe
// (public_udp.go, public_udp_transport.go) per cadence, its observations
// turned into findings against the pass's own settings.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// NewPublicUdpSignal implements SIGNALS.md §16.9. Enrollment is explicit;
// successful checks prove an exact authenticated transport return path only.
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

func publicUdpPathFinding(request PublicUdpRequest, class string) finding {
	return finding{
		// Healthy resolution matches target independently of frame. Keep each
		// family/protocol in the target so one healthy path cannot clear another.
		probeId: "public/udp", tier: tierWarn, class: class,
		target:    request.Host + "/public-udp/" + request.Target + "/" + request.Family + "/" + request.Service + "/" + request.Front + "/" + request.Carrier,
		frame:     "family=" + request.Family + " service=" + request.Service + " front=" + request.Front + " carrier=" + request.Carrier,
		sustain:   1,
		mechanism: "A fresh bounded socket sends protocol-appropriate QUIC to the pinned public tuple. Exact wire-source filtering precedes any DNS translation, and normal TLS certificate/SNI verification is required. Connect H3 is custom QUIC, not HTTP/3; only an enrolled Alt API front negotiates h3.",
		baseline:  "The enrolled family/carrier completes a fresh authenticated handshake with positive sent/received wire evidence from the exact requested address and port.",
		evidence:  "Only fixed status and bounded packet counts are retained. Pinned addresses, TLS names, DNS suffixes, packet bytes, attempt correlation tokens and raw errors are private. Unsolicited or mismatched datagrams alone cannot identify an SNAT fault.",
		context:   "This proves the selected transport path only, not provider authentication, API/application success, egress delivery or the exact DNAT generation. An explicitly published Alt direct UDP port is not a Connect-private listener leak. Explicit enrollment is not an exhaustive dynamic fleet census.",
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
// transport ran under.
func publicUdpFindings(request PublicUdpRequest, observation PublicUdpObservation, err error, settings *PublicUdpProbeSettings) []finding {
	unknown := func(reason string) []finding { return []finding{publicUdpUnknown(request, reason)} }
	if observation.Failure == PublicUdpFailureSource {
		return unknown("source-unavailable")
	}
	if observation.AttemptId != request.AttemptId || observation.RequestedTuple != publicUdpTuple(request) ||
		observation.SentPackets < 0 || observation.SentPackets > settings.MaxPacketCount ||
		observation.ReceivedPackets < 0 || observation.ReceivedPackets > settings.MaxPacketCount ||
		observation.UnexpectedPackets < 0 || observation.UnexpectedPackets > settings.MaxPacketCount-observation.ReceivedPackets ||
		observation.SentBytes < 0 || observation.SentBytes > settings.MaxByteCount ||
		observation.ReceivedBytes < 0 || observation.ReceivedBytes > settings.MaxByteCount {
		return unknown("observation-authority-invalid")
	}
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
	default:
		return unknown("observation-authority-invalid")
	}
	expectedProtocol := ""
	if request.Front == "api" {
		expectedProtocol = "h3"
	}
	if err != nil || !observation.FreshSocket || !observation.HandshakeComplete || !observation.TlsVerified || observation.PeerTuple != observation.RequestedTuple ||
		observation.NegotiatedProtocol != expectedProtocol || observation.SentPackets == 0 || observation.SentBytes == 0 ||
		observation.ReceivedPackets == 0 || observation.ReceivedBytes == 0 {
		return unknown("authenticated-exact-return-unverified")
	}
	findings := []finding{}
	for _, class := range []string{"public-udp-path", "public-udp-observation"} {
		finding := publicUdpPathFinding(request, class)
		finding.healthy = true
		findings = append(findings, finding)
	}
	return findings
}
