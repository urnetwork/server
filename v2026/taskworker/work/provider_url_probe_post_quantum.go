// The URL pass's part of a turn that measures post-quantum (fleetprobe
// url_e2e.go): the platform-local resolver of the provider's identity key its
// session verifies, and the turn's e2e outcome counter.
package work

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The platform's read of a client's published identity key. A package-level
// seam: immutable in production, replaced by the resolver's test.
var getProviderUrlProbePeerClientPublicKey = model.GetClientPublicKey

// A post-quantum turn's resolver of its provider's published identity key,
// read from the platform's own store: the taskworker is the platform, so this
// is what the unauthenticated /key/<client_id> route would answer, without the
// request. A session runs it on a worker of its own, never on the packet
// path. A peer that never published a key reads as no key and no error; a
// failed read, raised or returned, is an error, which the session treats as
// no key yet.
func newProviderUrlProbePeerClientPublicKeyFetcher(peerId connect.Id) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) (publicKey []byte, returnErr error) {
		server.HandleError(func() {
			publicKey, returnErr = getProviderUrlProbePeerClientPublicKey(ctx, server.Id(peerId))
		}, func(err error) {
			publicKey, returnErr = nil, err
		})
		return
	}
}

// The e2e outcomes of acknowledged post-quantum turns, as the prober read
// them before the server stored them.
var urlProbeE2eOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_url_probe_e2e_outcomes_total",
	Help: "Acknowledged URL turns that measured post-quantum (about one turn per provider every four hours) and whose session recorded the provider, by the outcome of the end-to-end session the turn's tunnel opened: positive, a post-quantum group (X25519MLKEM768) sealed before the IPv4 HTTPS leg wrote its request, and that leg passed; negative, a classical group, a post-quantum session unsealed by the run's end, or a ClientHello unanswered for a whole establishment window while the provider acknowledged the turn's data; unmeasured, anything else (a handshake still pending, a re-created tunnel or replaced window client, a failed IPv4 HTTPS leg)",
}, []string{"pq"})

func init() {
	for _, pq := range []string{egresshealth.UrlProbePqPositive, egresshealth.UrlProbePqNegative, egresshealth.UrlProbePqUnmeasured} {
		urlProbeE2eOutcomes.WithLabelValues(pq)
	}
	prometheus.MustRegister(urlProbeE2eOutcomes)
}

// Counts an acknowledged turn's e2e outcome, for a turn whose session
// recorded the provider.
func observeUrlProbeE2eOutcome(result *egresshealth.Result) {
	if result == nil || result.UrlProbeEvidence == nil || result.UrlProbeEvidence.E2e == nil {
		return
	}
	urlProbeE2eOutcomes.WithLabelValues(result.UrlProbeEvidence.E2e.Pq).Inc()
}
