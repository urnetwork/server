package session

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

// At most 15 seconds per server relative to Redis bounds fleet spread by the
// 30 seconds deducted by connect's monotonic conversion.
// Retention has a further five-minute margin; neither bound is a tuning escape
// hatch for a clock fault. Minting resumes only after an observed safe sample.
const SessionClockDisagreementLimit = 15 * time.Second

var sessionClockDisagreement = prometheus.NewGauge(prometheus.GaugeOpts{Name: "urnetwork_session_clock_disagreement_seconds", Help: "Conservative Redis versus local clock disagreement observed by session minting"})
var sessionClockRefusals = prometheus.NewCounter(prometheus.CounterOpts{Name: "urnetwork_session_clock_refusals_total", Help: "Session mints suspended because measured clock disagreement exceeded the fleet bound"})

func init() { prometheus.MustRegister(sessionClockDisagreement, sessionClockRefusals) }

func sessionClockSampleSafe(start, end, authority time.Time) (bool, time.Duration) {
	// The authority read happened somewhere inside this interval. A backwards
	// local clock cannot be used to manufacture an apparently safe interval.
	if end.Before(start) {
		return false, start.Sub(end)
	}
	// The exact point inside the request interval is unknown. Use the larger
	// endpoint distance: using only the nearest endpoint would hide clock
	// disagreement inside Redis/network latency and exceed the fleet bound.
	difference := max(authority.Sub(start).Abs(), authority.Sub(end).Abs())
	return difference <= SessionClockDisagreementLimit, difference
}

func verifySessionMintClock(ctx context.Context) error {
	start := server.NowUtc()
	var authority time.Time
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		authority, err = r.Time(ctx).Result()
		return err
	})
	if err != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	safe, disagreement := sessionClockSampleSafe(start, server.NowUtc(), authority)
	sessionClockDisagreement.Set(disagreement.Seconds())
	if !safe {
		sessionClockRefusals.Inc()
		return errors.Join(ErrAuthUnavailable, errors.New("session clock bound exceeded"))
	}
	return nil
}
