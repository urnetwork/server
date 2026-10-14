// The in-process operator's per-method call metrics. Over HTTP the API
// router kept per-route request, in-flight, duration and error series for
// these calls; in-process they never reach a router, so the operator keeps
// the same view per method, plus the wait for one of its pass's call slots.
// The series are identity-free: the method and a bounded error kind are the
// only labels.
package localoperator

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/qualityprobe/ingest"
)

// The error kinds of a call, as the call_errors_total kind label names them.
const (
	// The API would have answered a 4xx.
	callErrorRefused = "refused"
	// The API would have answered a 5xx, or the call failed in this process.
	callErrorFailed = "failed"
	// The caller gave up, or the call bound passed, while the call ran.
	callErrorCanceled = "canceled"
	// The caller gave up, or the call bound passed, before a call slot freed,
	// so the call never ran.
	callErrorUnadmitted = "unadmitted"
)

// One process's collectors. Safe for concurrent use; every method is a no-op
// on nil.
type operatorMetrics struct {
	inflight *prometheus.GaugeVec
	waiting  *prometheus.GaugeVec
	duration *prometheus.SummaryVec
	slotWait *prometheus.SummaryVec
	errors   *prometheus.CounterVec
}

// Registers a new collector set, panicking on a duplicate like MustRegister.
func newOperatorMetrics(registerer prometheus.Registerer) *operatorMetrics {
	metrics := &operatorMetrics{
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "local_operator",
			Name:      "calls_inflight",
			Help:      "In-process operator calls running in one of their pass's call slots, by operator method",
		}, []string{"method"}),
		waiting: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "local_operator",
			Name:      "calls_waiting",
			Help:      "In-process operator calls waiting for a call slot because every one of their pass's operator_concurrency slots is running, by operator method",
		}, []string{"method"}),
		duration: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Namespace:  "urnetwork",
			Subsystem:  "local_operator",
			Name:       "call_duration_seconds",
			Help:       "In-process operator call duration in its call slot, by operator method; the counterpart of the API's per-route handler duration",
			Objectives: nil,
		}, []string{"method"}),
		slotWait: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Namespace:  "urnetwork",
			Subsystem:  "local_operator",
			Name:       "call_slot_wait_seconds",
			Help:       "Time each in-process operator call waited for a call slot, zero when one was free, including calls that gave up waiting, by operator method",
			Objectives: nil,
		}, []string{"method"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "local_operator",
			Name:      "call_errors_total",
			Help:      "In-process operator calls that did not end in an answer the API gives as success, by operator method and kind: refused (a 4xx answer), failed (a 5xx answer or a local failure), canceled (ended while running), unadmitted (ended waiting for a call slot)",
		}, []string{"method", "kind"}),
	}
	registerer.MustRegister(metrics.inflight, metrics.waiting, metrics.duration, metrics.slotWait, metrics.errors)
	return metrics
}

// A process registers the collectors when it builds its first operator, so a
// taskworker that keeps the operator API publishes none of these series.
var processMetrics = sync.OnceValue(func() *operatorMetrics {
	return newOperatorMetrics(prometheus.DefaultRegisterer)
})

// The bounded kind of a call's error, or "" for an answer the API gives as
// success, the due maintenance page included. A done error inside a call whose
// caller still waits is the router's 503, so it is failed, not canceled.
func callErrorKind(err error) string {
	var refusal *handlerRefusal
	var failure *handlerFailure
	switch {
	case err == nil, errors.Is(err, ingest.ErrDuePriorityMaintenancePending):
		return ""
	case errors.As(err, &refusal):
		if refusal.status < http.StatusInternalServerError {
			return callErrorRefused
		}
		return callErrorFailed
	case errors.As(err, &failure):
		return callErrorFailed
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return callErrorCanceled
	default:
		return callErrorFailed
	}
}

// Marks a call waiting for a slot until the returned function ends the wait.
func (self *operatorMetrics) beginWait(method string) func() {
	if self == nil {
		return func() {}
	}
	waiting := self.waiting.WithLabelValues(method)
	waiting.Inc()
	return waiting.Dec
}

// Records a call that ended before a slot freed.
func (self *operatorMetrics) unadmitted(method string, wait time.Duration) {
	if self == nil {
		return
	}
	self.slotWait.WithLabelValues(method).Observe(wait.Seconds())
	self.errors.WithLabelValues(method, callErrorUnadmitted).Inc()
}

// Marks a call that took a slot after wait as running until the returned
// function records its end with the call's error.
func (self *operatorMetrics) beginCall(method string, wait time.Duration) func(error) {
	if self == nil {
		return func(error) {}
	}
	self.slotWait.WithLabelValues(method).Observe(wait.Seconds())
	inflight := self.inflight.WithLabelValues(method)
	inflight.Inc()
	started := time.Now()
	return func(err error) {
		inflight.Dec()
		self.duration.WithLabelValues(method).Observe(time.Since(started).Seconds())
		if kind := callErrorKind(err); kind != "" {
			self.errors.WithLabelValues(method, kind).Inc()
		}
	}
}
