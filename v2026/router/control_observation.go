package router

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
)

// Opt-in phase accounting for the fixed /connect/control route. The request
// goroutine owns transitions; collectors are safe across independent requests.
type controlHttpMetrics struct {
	completions *prometheus.CounterVec
	duration    *prometheus.SummaryVec
	inflight    *prometheus.GaugeVec
	requests    *prometheus.CounterVec
	requestTime *prometheus.SummaryVec
	requestLive *prometheus.GaugeVec
	now         func() time.Time
}

type controlHttpPhase uint8

const (
	controlHttpPrepare controlHttpPhase = iota
	controlHttpAuthenticate
	controlHttpController
	controlHttpResponse
)

var controlHttpPhases = [...]string{"prepare", "authenticate", "controller", "response"}
var controlHttpOutcomes = [...]string{"ok", "rejected", "canceled", "panic"}

// All 16 completion cells and all four phase gauges/summaries exist at startup.
// No labels originate in request bodies, paths, credentials, or error text.
func newControlHttpMetrics(registerer prometheus.Registerer) *controlHttpMetrics {
	metrics := &controlHttpMetrics{
		completions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_connect_control_http_phase_completions_total",
			Help: "Control HTTP phase exits, including canceled and panicking requests before frame dispatch.",
		}, []string{"phase", "outcome"}),
		duration: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Name: "urnetwork_connect_control_http_phase_seconds",
			Help: "Control HTTP phase residence, including failed/canceled calls; sum/count is a mean, not a quantile.",
		}, []string{"phase"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "urnetwork_connect_control_http_phase_inflight",
			Help: "Control HTTP requests currently in each phase, including time waiting for dependencies.",
		}, []string{"phase"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_connect_control_http_requests_total",
			Help: "Control HTTP handler invocations including retries, by spoofable probe marker, final phase and outcome; not authorization or ACK evidence.",
		}, []string{"source", "phase", "outcome"}),
		requestTime: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Name: "urnetwork_connect_control_http_request_seconds",
			Help: "Control HTTP handler residence by spoofable probe marker, including retries and canceled requests.",
		}, []string{"source"}),
		requestLive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "urnetwork_connect_control_http_requests_inflight",
			Help: "Control HTTP handler invocations in flight by spoofable probe marker, with no admission or auth effect.",
		}, []string{"source"}),
		now: time.Now,
	}
	for _, phase := range controlHttpPhases {
		metrics.duration.WithLabelValues(phase)
		metrics.inflight.WithLabelValues(phase)
		for _, outcome := range controlHttpOutcomes {
			metrics.completions.WithLabelValues(phase, outcome)
		}
	}
	for _, source := range []string{"probe_claimed", "unmarked"} {
		metrics.requestTime.WithLabelValues(source)
		metrics.requestLive.WithLabelValues(source)
		for _, phase := range controlHttpPhases {
			for _, outcome := range controlHttpOutcomes {
				metrics.requests.WithLabelValues(source, phase, outcome)
			}
		}
	}
	registerer.MustRegister(metrics.completions, metrics.duration, metrics.inflight, metrics.requests, metrics.requestTime, metrics.requestLive)
	return metrics
}

var defaultControlHttpMetrics = newControlHttpMetrics(prometheus.DefaultRegisterer)

type controlHttpObservationKey struct{}

// One synchronous handler owns this state, including cancellation/panic unwind.
// There are no background workers or observers that can outlive the request.
type controlHttpObservation struct {
	metrics      *controlHttpMetrics
	ctx          context.Context
	phase        controlHttpPhase
	start        time.Time
	source       string
	requestStart time.Time
}

// Attach accounting only at the configured control handler. The returned
// finalizer must be deferred directly so a panic is recorded and re-propagated.
func ObserveConnectControl(req *http.Request) (*http.Request, func()) {
	return observeConnectControl(req, defaultControlHttpMetrics)
}

// The independent collector seam keeps synthetic tests off the shared registry.
func observeConnectControl(req *http.Request, metrics *controlHttpMetrics) (*http.Request, func()) {
	start := metrics.now()
	source := "unmarked"
	if req.Header.Get(connect.ControlProbeTelemetryHeader) == "1" {
		source = "probe_claimed"
	}
	observation := &controlHttpObservation{metrics: metrics, ctx: req.Context(), start: start, requestStart: start, source: source}
	metrics.inflight.WithLabelValues(controlHttpPhases[controlHttpPrepare]).Inc()
	metrics.requestLive.WithLabelValues(source).Inc()
	req = req.WithContext(context.WithValue(req.Context(), controlHttpObservationKey{}, observation))
	return req, func() {
		recovered := recover()
		outcome := "rejected"
		switch {
		case observation.ctx.Err() != nil:
			outcome = "canceled"
		case recovered != nil:
			outcome = "panic"
		case observation.phase == controlHttpResponse:
			outcome = "ok"
		}
		observation.finishPhase(outcome)
		metrics.requestLive.WithLabelValues(source).Dec()
		metrics.requestTime.WithLabelValues(source).Observe(metrics.now().Sub(observation.requestStart).Seconds())
		metrics.requests.WithLabelValues(source, controlHttpPhases[observation.phase], outcome).Inc()
		if recovered != nil {
			panic(recovered)
		}
	}
}

// Unobserved routes are a no-op. Transitions occur only after the preceding
// phase succeeded; an early return stays in the phase that rejected the call.
func advanceControlHttpPhase(req *http.Request, phase controlHttpPhase) {
	observation, _ := req.Context().Value(controlHttpObservationKey{}).(*controlHttpObservation)
	if observation == nil {
		return
	}
	observation.finishPhase("ok")
	observation.phase = phase
	observation.start = observation.metrics.now()
	observation.metrics.inflight.WithLabelValues(controlHttpPhases[phase]).Inc()
}

// Only fixed internal enums reach this observer; metrics retain no request data.
func (self *controlHttpObservation) finishPhase(outcome string) {
	phase := controlHttpPhases[self.phase]
	self.metrics.inflight.WithLabelValues(phase).Dec()
	self.metrics.duration.WithLabelValues(phase).Observe(self.metrics.now().Sub(self.start).Seconds())
	self.metrics.completions.WithLabelValues(phase, outcome).Inc()
}
