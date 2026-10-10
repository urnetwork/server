package session

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

var sessionStorageSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "urnetwork_session_storage_seconds", Help: "Required session authority/inventory storage latency", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2}}, []string{"operation", "outcome"})
var sessionCapacityRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_capacity_refusals_total", Help: "Admission refused without evicting an accepted session"}, []string{"capacity"})
var sessionLeaseRetirements = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_lease_retirements_total", Help: "Independent authorization lease retirements"}, []string{"cause"})
var sessionJournalBacklog = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_session_journal_pending", Help: "Durable session operation and index repair backlog"}, []string{"state"})
var sessionJournalAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_session_journal_oldest_seconds", Help: "Age of oldest unfinished session operation or index repair"}, []string{"state"})

func init() {
	prometheus.MustRegister(sessionStorageSeconds, sessionCapacityRefusals, sessionLeaseRetirements, sessionJournalBacklog, sessionJournalAge)
}

func observeSessionStorage(operation string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "refused"
		if errors.Is(err, ErrAuthUnavailable) || errors.Is(err, ErrSessionStoreUnavailable) {
			outcome = "unavailable"
		}
		var refusal *SessionError
		if errors.As(err, &refusal) {
			switch refusal.Code {
			case "session_limit_reached":
				sessionCapacityRefusals.WithLabelValues("live").Inc()
			case "session_retention_limit_reached":
				sessionCapacityRefusals.WithLabelValues("retained").Inc()
			}
		}
	}
	sessionStorageSeconds.WithLabelValues(operation, outcome).Observe(time.Since(start).Seconds())
}

// Run only in the bounded maintenance family, never on a request's SQL owner.
// Unfinished work is deliberately retained; these gauges make a stopped worker
// or a repeatedly failing receipt recoverable and operationally visible.
func ObserveSessionMaintenance(ctx context.Context, now time.Time) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = ErrAuthUnavailable
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		for _, state := range []string{"prepared", "enforced", "index"} {
			var count int64
			var oldest *time.Time
			if state == "index" {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),min(update_time) FROM network_session_index_outbox`).Scan(&count, &oldest))
			} else {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),min(create_time) FROM network_session_operation WHERE status=$1`, state).Scan(&count, &oldest))
			}
			age := 0.0
			if oldest != nil {
				age = max(0, now.Sub(*oldest).Seconds())
			}
			sessionJournalBacklog.WithLabelValues(state).Set(float64(count))
			sessionJournalAge.WithLabelValues(state).Set(age)
		}
	})
	return
}
