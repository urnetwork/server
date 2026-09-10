package session

// Source attribution instrumentation for ResolveClientAddress.
//
// When a deployment's ingress does not overwrite X-UR-Forwarded-For, every
// request resolves to the ingress's own address. server.ClientIpHashForAddr
// then masks that one address to a /29 (ipv4) or /56 (ipv6) and hands the
// whole deployment a single per-address budget: five network creates per
// sliding 24h (model.CheckNetworkCreateRateLimit) and five auth attempts per
// five minutes (model.UserAuthAttempt), both of which refuse with 429. Real
// users are then refused on their first account. This has happened.
//
// The condition was silent. The resolver logs a repeated header and a
// malformed value, but the absent header - the shape that caused the outage -
// returned the peer with no log, no metric, and nothing for an operator
// holding a "users report 429" report to correlate against.
//
// This counts, and deliberately does not warn. The server cannot tell a
// misconfigured ingress from a deployment that is legitimately reachable
// without one: both arrive with no header, and the peer is the ingress in the
// first case and the client in the second. Nothing local breaks the tie. The
// trusted-proxy CIDR configuration that once declared the answer was removed
// on purpose (SIGNALS.md 8.8: "There is no trusted-proxy environment
// setting"), so there is no operator declaration left to validate at boot,
// and a startup assertion here would be fabricated. Every per-request gate
// considered mislabels a real topology: a public-peer gate warns forever on a
// directly exposed self-hosted deployment; a non-loopback gate goes blind to
// the common same-host nginx ingress; a "many requests from few peers"
// heuristic fires on an idle deployment whose only traffic is /status
// readiness polls, which build a session too (router.WarpStatus).
//
// So the metric reports the fact and leaves the verdict to the operator, who
// knows whether an ingress is supposed to be in front. For anyone who runs
// one, peer_absent holding at the full resolution rate is the outage; for
// anyone who does not, it is the expected steady state. That is a partition
// of outcomes, not a guess, so it has no false positives to tune.
//
// The address itself is never a label. A request-path metric may only carry a
// bounded partition (controller/connect_controller.go), and the whole point of
// this failure is that the address collapses to one value anyway.

import (
	"net/netip"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
)

// clientAddressSource is the bounded branch of ResolveClientAddress that
// produced the address. Every value is decided by server-side control flow,
// never by request content.
type clientAddressSource string

const (
	// The ingress-owned header carried one usable ip:port.
	clientAddressSourceUrHeader clientAddressSource = "ur_header"
	// No header at all: an ingress that does not set it, or direct traffic.
	clientAddressSourcePeerAbsent clientAddressSource = "peer_absent"
	// More than one header value: an ingress appending instead of overwriting.
	clientAddressSourcePeerRepeated clientAddressSource = "peer_repeated"
	// One empty value: an unresolved ingress variable, distinct from absent.
	clientAddressSourcePeerEmpty clientAddressSource = "peer_empty"
	// One value that is not an ip:port pair.
	clientAddressSourcePeerMalformed clientAddressSource = "peer_malformed"
)

// clientAddressSources is the closed set. An unparseable RemoteAddr is not a
// member: that path resolves no address and returns an error to the caller.
var clientAddressSources = []clientAddressSource{
	clientAddressSourceUrHeader,
	clientAddressSourcePeerAbsent,
	clientAddressSourcePeerRepeated,
	clientAddressSourcePeerEmpty,
	clientAddressSourcePeerMalformed,
}

var clientAddressResolutionCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "session",
		Name:      "client_address_resolutions_total",
		Help:      "Client address resolutions partitioned by the bounded resolver branch that produced the address",
	},
	[]string{"source"},
)

func init() {
	// CounterVec label children are lazy. Materialize the whole partition so a
	// fleet that never sets the header is distinguishable from a generation
	// that predates this instrumentation and cannot report it at all.
	for _, source := range clientAddressSources {
		clientAddressResolutionCounter.WithLabelValues(string(source))
	}
	prometheus.MustRegister(clientAddressResolutionCounter)
}

// noteClientAddressSource counts one resolution outcome.
//
// The counter is the lossless signal. The absent-header peer is additionally
// emitted at V(1) for the operator who runs no metrics backend: a deployment
// with no ingress takes that branch on every request, so the line stays off
// the default level and the operator opts into the volume - the same contract
// as jwt.rejectByJwt. The value of the header is never logged, following the
// existing resolver lines: a future ingress regression could make it
// caller-controlled.
func noteClientAddressSource(source clientAddressSource, remote netip.AddrPort) {
	clientAddressResolutionCounter.WithLabelValues(string(source)).Inc()
	if source == clientAddressSourcePeerAbsent && glog.V(1) {
		glog.Infof(
			"[session]%s absent from peer %s; using the peer address\n",
			urForwardedForHeader,
			remote.Addr(),
		)
	}
}
