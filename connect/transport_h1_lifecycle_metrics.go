// H1 admission and authenticated socket lifetime are distinct from HTTP
// handler completion. Only fixed server outcomes reach these metrics or logs.
package connect

import (
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
)

type connectH1Result uint8

const (
	connectH1ResultUnclassified connectH1Result = iota
	connectH1ResultHandlerClosed
	connectH1ResultDraining
	connectH1ResultAddressRefused
	connectH1ResultRateLimitInit
	connectH1ResultRateLimit
	connectH1ResultAuthDeadline
	connectH1ResultUpgradeUnavailable
	connectH1ResultUpgradeInvalid
	connectH1ResultUpgradeError
	connectH1ResultAuthRead
	connectH1ResultAuthFrame
	connectH1ResultAuthEcho
	connectH1ResultJwtRefused
	connectH1ResultStateRefused
	connectH1ResultClientMissing
	connectH1ResultAuthDependency
	connectH1ResultAccepted
)

var connectH1ResultLabels = [...]string{
	"unclassified", "handler_closed", "draining", "address_refused",
	"rate_limit_init", "rate_limit", "auth_deadline", "upgrade_unavailable",
	"upgrade_invalid", "upgrade_error", "auth_read", "auth_frame", "auth_echo",
	"jwt_refused", "state_refused", "client_missing", "auth_dependency", "accepted",
}

type connectH1CloseReason uint32

const (
	connectH1CloseUnclassified connectH1CloseReason = iota
	connectH1ClosePeer
	connectH1CloseReadTimeout
	connectH1CloseReadError
	connectH1CloseWriteError
	connectH1CloseResident
	connectH1CloseClientLimit
	connectH1CloseRequestCanceled
	connectH1CloseHandlerCanceled
)

var connectH1CloseLabels = [...]string{
	"unclassified", "peer_close", "read_timeout", "read_error", "write_error",
	"resident_ended", "client_limit", "request_canceled", "handler_canceled",
}

type connectH1LifecycleMetrics struct {
	handshakes *prometheus.CounterVec
	active     *prometheus.GaugeVec
	closed     *prometheus.CounterVec
	duration   *prometheus.HistogramVec
}

// A separate registry keeps metric tests independent of process globals.
func newConnectH1LifecycleMetrics(registerer prometheus.Registerer) *connectH1LifecycleMetrics {
	metrics := &connectH1LifecycleMetrics{
		handshakes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "connect", Name: "h1_handshake_results_total",
			Help: "One H1 admission result; upgraded means the carrier opened, authenticated means authorization also succeeded.",
		}, []string{"carrier", "stage", "result"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork", Subsystem: "connect", Name: "h1_sessions_active",
			Help: "Authenticated H1 transports whose Connect handler has not finished cleanup; excludes admission attempts.",
		}, []string{"carrier"}),
		closed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "connect", Name: "h1_sessions_closed_total",
			Help: "Authenticated H1 transports closed by the first observed terminal event; this is an observation, not a causal verdict.",
		}, []string{"carrier", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "urnetwork", Subsystem: "connect", Name: "h1_session_duration_seconds",
			Help:    "Time from successful H1 authorization and upgrade through handler cleanup, for completed authenticated transports only.",
			Buckets: []float64{0.001, 0.003, 0.01, 0.03, 0.1, 0.3, 1, 3, 10, 30, 60, 300, 1800, 3600},
		}, []string{"carrier"}),
	}
	registerer.MustRegister(metrics.handshakes, metrics.active, metrics.closed, metrics.duration)
	return metrics
}

var defaultConnectH1LifecycleMetrics = newConnectH1LifecycleMetrics(prometheus.DefaultRegisterer)

// The handler owns admission state. Worker goroutines may only publish the
// first terminal event atomically; finish runs after those workers are joined.
type connectH1Observation struct {
	metrics  *connectH1LifecycleMetrics
	carrier  string
	upgraded bool
	result   connectH1Result
	admitted time.Time
	closed   atomic.Uint32
}

func newConnectH1Observation(custom bool) *connectH1Observation {
	carrier := "websocket"
	if custom {
		carrier = "h1plus"
	}
	return &connectH1Observation{metrics: defaultConnectH1LifecycleMetrics, carrier: carrier}
}

// Called only after both the transport and authentication are ready.
func (self *connectH1Observation) admit() {
	self.result = connectH1ResultAccepted
	self.admitted = time.Now()
	self.metrics.handshakes.WithLabelValues(self.carrier, "authenticated", "accepted").Inc()
	self.metrics.active.WithLabelValues(self.carrier).Inc()
}

// The triggering worker records before cancellation wakes sibling workers.
func (self *connectH1Observation) close(reason connectH1CloseReason) {
	self.closed.CompareAndSwap(0, uint32(reason))
}

func (self *connectH1Observation) readError(err error) {
	var closeError *websocket.CloseError
	var netError net.Error
	if errors.As(err, &closeError) {
		self.close(connectH1ClosePeer)
	} else if errors.As(err, &netError) && netError.Timeout() {
		self.close(connectH1CloseReadTimeout)
	} else {
		self.close(connectH1CloseReadError)
	}
}

// Normal early returns, rejected upgraded sockets and authenticated lifetimes
// must not collapse into the HTTP router's common normal-return bucket.
func (self *connectH1Observation) finish() {
	if self.admitted.IsZero() {
		stage := "http"
		if self.upgraded {
			stage = "upgraded"
		}
		result := connectH1ResultLabels[self.result]
		self.metrics.handshakes.WithLabelValues(self.carrier, stage, result).Inc()
		if glog.V(1) {
			glog.Infof("[t]h1 admission carrier=%s stage=%s result=%s\n", self.carrier, stage, result)
		}
		return
	}
	duration := time.Since(self.admitted)
	reason := connectH1CloseLabels[self.closed.Load()]
	self.metrics.active.WithLabelValues(self.carrier).Dec()
	self.metrics.closed.WithLabelValues(self.carrier, reason).Inc()
	self.metrics.duration.WithLabelValues(self.carrier).Observe(duration.Seconds())
	if glog.V(1) {
		glog.Infof("[t]h1 closed carrier=%s reason=%s duration=%s\n", self.carrier, reason, duration)
	}
}
