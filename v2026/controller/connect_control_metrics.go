package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Fixed-cardinality handler evidence, not a contract-authorization or transport
// acknowledgement count. A handler_ok reply can contain a protocol rejection.
type controlFrameMetrics struct {
	completions *prometheus.CounterVec
	duration    *prometheus.SummaryVec
	inflight    *prometheus.GaugeVec
	now         func() time.Time
}

var controlFrameMessages = [...]string{
	"create_contract", "close_contract", "provide", "encrypted_key",
	"client_key", "control_ping", "provide_ping", "other",
}

// Exactly 64 completion cells and 16 duration/inflight children. Source is
// stamped by the HTTP entry point, never inferred from client-supplied data.
func newControlFrameMetrics(registerer prometheus.Registerer) *controlFrameMetrics {
	metrics := &controlFrameMetrics{
		completions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_connect_control_frames_total",
			Help: "Control frame handler completions; handler_ok may include a protocol-level contract rejection, not an ACK.",
		}, []string{"ingress", "message", "outcome"}),
		duration: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Name: "urnetwork_connect_control_frame_seconds",
			Help: "Control frame handler residence by fixed ingress/message; sum/count is a mean including failures.",
		}, []string{"ingress", "message"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "urnetwork_connect_control_frames_inflight",
			Help: "Control frame handlers in progress, separate from HTTP requests still reading/authenticating.",
		}, []string{"ingress", "message"}),
		now: time.Now,
	}
	for _, ingress := range []string{"http", "internal"} {
		for _, message := range controlFrameMessages {
			metrics.duration.WithLabelValues(ingress, message)
			metrics.inflight.WithLabelValues(ingress, message)
			for _, outcome := range []string{"handler_ok", "error", "canceled", "panic"} {
				metrics.completions.WithLabelValues(ingress, message, outcome)
			}
		}
	}
	registerer.MustRegister(metrics.completions, metrics.duration, metrics.inflight)
	return metrics
}

var defaultControlFrameMetrics = newControlFrameMetrics(prometheus.DefaultRegisterer)

type controlHttpIngressKey struct{}

// Retain the existing cancellation-panic boundary: canceled callers unwind;
// live-context handler panics become this frame's error and siblings continue.
func observeControlFrame(
	ctx context.Context,
	message any,
	metrics *controlFrameMetrics,
	handle func() error,
) (err error) {
	ingress := "internal"
	if httpIngress, _ := ctx.Value(controlHttpIngressKey{}).(bool); httpIngress {
		ingress = "http"
	}
	messageLabel := controlFrameMessageLabel(message)
	start := metrics.now()
	metrics.inflight.WithLabelValues(ingress, messageLabel).Inc()
	defer func() {
		recovered := recover()
		outcome := "handler_ok"
		switch {
		case ctx.Err() != nil:
			outcome = "canceled"
		case recovered != nil:
			outcome = "panic"
		case err != nil:
			outcome = "error"
		}
		metrics.inflight.WithLabelValues(ingress, messageLabel).Dec()
		metrics.duration.WithLabelValues(ingress, messageLabel).Observe(metrics.now().Sub(start).Seconds())
		metrics.completions.WithLabelValues(ingress, messageLabel, outcome).Inc()
		if recovered != nil {
			if ctx.Err() != nil {
				panic(recovered)
			}
			err = fmt.Errorf("control frame %T panicked: %v", message, recovered)
		}
	}()
	return handle()
}
