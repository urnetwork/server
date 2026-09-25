package model

import "github.com/prometheus/client_golang/prometheus"

// Two fixed children separate failed score-read batches from valid cache misses.
// Both primary and optional backfill loads use them; these are not API outcomes.
type clientScoreReadMetricSet struct {
	errors  *prometheus.CounterVec
	counts  prometheus.Counter
	samples prometheus.Counter
}

// Prebinding exports zero even before a phase fails, without dynamic request labels.
func newClientScoreReadMetricSet() *clientScoreReadMetricSet {
	errors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_client_score_read_errors_total",
		Help: "Non-missing Redis score-read pipeline failures by phase, including primary and optional backfill reads",
	}, []string{"phase"})
	return &clientScoreReadMetricSet{
		errors:  errors,
		counts:  errors.WithLabelValues("counts"),
		samples: errors.WithLabelValues("samples"),
	}
}

var clientScoreReadMetrics = newClientScoreReadMetricSet()

func init() {
	prometheus.MustRegister(clientScoreReadMetrics.errors)
}
