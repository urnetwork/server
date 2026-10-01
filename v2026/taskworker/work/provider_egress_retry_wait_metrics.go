package work

import "github.com/prometheus/client_golang/prometheus"

// A load can wait while its provider check still owns a worker. Concurrent
// destinations in one check can each wait, so this is not a worker count.
type blackholeRetryWaitMetrics struct {
	active    prometheus.Gauge
	started   prometheus.Counter
	completed prometheus.Counter
}

func newBlackholeRetryWaitMetrics() *blackholeRetryWaitMetrics {
	return &blackholeRetryWaitMetrics{
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_egress_probe_blackhole_retry_waiting_loads",
			Help: "Site load attempts currently sleeping between blackhole retries; multiple loads can belong to one occupied worker",
		}),
		started: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "urnetwork_egress_probe_blackhole_retry_wait_started_total",
			Help: "Site load retry waits started during blackhole checks",
		}),
		completed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "urnetwork_egress_probe_blackhole_retry_wait_completed_total",
			Help: "Site load retry waits ended, including context cancellation",
		}),
	}
}

func (self *blackholeRetryWaitMetrics) observe(waiting bool) {
	if waiting {
		self.active.Inc()
		self.started.Inc()
	} else {
		self.active.Dec()
		self.completed.Inc()
	}
}

var egressProbeBlackholeRetryWait = newBlackholeRetryWaitMetrics()

func init() {
	prometheus.MustRegister(
		egressProbeBlackholeRetryWait.active,
		egressProbeBlackholeRetryWait.started,
		egressProbeBlackholeRetryWait.completed,
	)
}
