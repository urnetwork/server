package session

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

var sessionStorageSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "urnetwork_session_storage_seconds", Help: "Required session authority/inventory storage latency", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2}}, []string{"operation", "outcome"})
var sessionCapacityRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_capacity_refusals_total", Help: "Admission refused without evicting an accepted session"}, []string{"capacity"})
var sessionLeaseRetirements = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_lease_retirements_total", Help: "Independent authorization lease retirements"}, []string{"cause"})
var sessionLeaseUnavailableRechecks = prometheus.NewCounter(prometheus.CounterOpts{Name: "urnetwork_session_lease_unavailable_rechecks_total", Help: "Authorization lease rechecks that produced no definitive answer; the admitted transport is kept"})
var sessionLeaseUnverifiedSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "urnetwork_session_lease_unverified_seconds", Help: "Time since a lease's last successful authoritative check, observed at each unanswered recheck", Buckets: []float64{60, 120, 300, 600, 1800, 3600, 7200, 21600, 86400}})
var sessionJournalBacklog = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_session_journal_pending", Help: "Durable session operation and index repair backlog"}, []string{"state"})
var sessionJournalAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_session_journal_oldest_seconds", Help: "Age of oldest unfinished session operation or index repair"}, []string{"state"})

// The session cap (session_cap.go). kind is the minting session's R-kind
// (signin, code, client) and cap the cap it reached (live, retained). None of
// these declares a push grouping label (env, service, block, host, instance;
// grafana.go): a metric that carries one makes the pusher refuse every push of
// the process.
var sessionCapWouldRefuse = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_cap_would_refuse_total", Help: "Session mints admitted over a session cap while pro.yml enforce_session_cap is false"}, []string{"kind", "cap"})
var sessionCapRefused = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_cap_refused_total", Help: "Session mints refused at a session cap (409 session_limit_reached or session_retention_limit_reached), nothing evicted"}, []string{"kind", "cap"})

// Client sessions a client's removal freed from the live set, by removal path.
var sessionFreedOnRemoval = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_freed_on_removal_total", Help: "Client-owned sessions freed from the live set when their last client was removed"}, []string{"path"})

// Each pass of the taskworker's session level sample (SampleSessionLevels)
// observes every network with a live set once: its live sessions the cap
// counts. No per-network label.
var sessionLivePerNetwork = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "urnetwork_session_live_per_network", Help: "Live sessions the cap counts, per network with a live set, observed once per network per sample pass", Buckets: []float64{10, 100, 500, 1000, 2000, 5000, 10000}})
var sessionLiveSamplePasses = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_session_live_sample_passes_total", Help: "Session level sample passes, complete or cut short by the budget"}, []string{"outcome"})

// The paths a removal frees from, each exported from the start.
const (
	SessionFreePathRemoveClient    = "remove_client"
	SessionFreePathRemoveClients   = "remove_clients"
	SessionFreePathTrimProxies     = "trim_proxies"
	SessionFreePathNetworkDeletion = "network_deletion"
	SessionFreePathLagGuest        = "lag_guest"
	SessionFreePathSignOutAll      = "sign_out_all"
	SessionFreePathIdleReap        = "idle_reap"
	SessionFreePathStaleChild      = "stale_child"
	SessionFreePathProber          = "prober"
	SessionFreePathOther           = "other"
)

func init() {
	prometheus.MustRegister(sessionStorageSeconds, sessionCapacityRefusals, sessionLeaseRetirements, sessionLeaseUnavailableRechecks, sessionLeaseUnverifiedSeconds, sessionJournalBacklog, sessionJournalAge)
	prometheus.MustRegister(sessionCapWouldRefuse, sessionCapRefused, sessionFreedOnRemoval, sessionLivePerNetwork, sessionLiveSamplePasses)
	for _, kind := range []string{SessionKindSignin, SessionKindCode, SessionKindClient} {
		for _, capacity := range []string{"live", "retained"} {
			sessionCapWouldRefuse.WithLabelValues(kind, capacity)
			sessionCapRefused.WithLabelValues(kind, capacity)
		}
	}
	for _, path := range []string{SessionFreePathRemoveClient, SessionFreePathRemoveClients, SessionFreePathTrimProxies, SessionFreePathNetworkDeletion, SessionFreePathLagGuest, SessionFreePathSignOutAll, SessionFreePathIdleReap, SessionFreePathStaleChild, SessionFreePathProber, SessionFreePathOther} {
		sessionFreedOnRemoval.WithLabelValues(path)
	}
	for _, outcome := range []string{"complete", "partial"} {
		sessionLiveSamplePasses.WithLabelValues(outcome)
	}
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
