package connect

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/model"
)

type residentForwardLookupCollectors struct {
	attempts  prometheus.Counter
	residence prometheus.Observer
}

// Twelve fixed scalar series per process, with no destination or client labels.
// Queue state is sampled only when a lookup begins; it is not a claim about
// queue contents throughout the lookup or about why a remote peer disappeared.
type residentForwardLookupMetrics struct {
	collectors [2][2]residentForwardLookupCollectors
	now        func() time.Time
}

func newResidentForwardLookupMetrics(registerer prometheus.Registerer) *residentForwardLookupMetrics {
	attempts := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_connect_resident_forward_lookup_attempts_total",
		Help: "Destination resident lookup attempts; initial and reconnect are distinct, and queue describes pending data at lookup admission.",
	}, []string{"phase", "queue"})
	residence := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "urnetwork_connect_resident_forward_lookup_seconds",
		Help: "Destination resident lookup wall residence including failed or canceled calls; not CPU time or provider evidence.",
	}, []string{"phase", "queue"})
	metrics := &residentForwardLookupMetrics{now: time.Now}
	for phase, phaseLabel := range [...]string{"initial", "reconnect"} {
		for queue, queueLabel := range [...]string{"empty", "pending"} {
			metrics.collectors[phase][queue] = residentForwardLookupCollectors{
				attempts:  attempts.WithLabelValues(phaseLabel, queueLabel),
				residence: residence.WithLabelValues(phaseLabel, queueLabel),
			}
		}
	}
	registerer.MustRegister(attempts, residence)
	return metrics
}

var defaultResidentForwardLookupMetrics = newResidentForwardLookupMetrics(prometheus.DefaultRegisterer)

// Observe only the existing lookup, retaining its result and panic semantics.
// The caller supplies owned pending state as well as buffered queue state.
func (self *residentForwardLookupMetrics) observe(initial, pending bool, lookup func() *model.NetworkClientResident) *model.NetworkClientResident {
	phase, queue := 1, 0
	if initial {
		phase = 0
	}
	if pending {
		queue = 1
	}
	collectors := &self.collectors[phase][queue]
	start := self.now()
	collectors.attempts.Inc()
	defer func() { collectors.residence.Observe(self.now().Sub(start).Seconds()) }()
	return lookup()
}
