package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

const (
	// Circle documents a default Wallets API POST limit of five requests per
	// second. Keep two requests/second of headroom for other callers sharing
	// the processor identity, and stay below the four-attempt incident
	// precursor monitored in SIGNALS.md.
	// https://developers.circle.com/api-reference/wallets/rate-limits
	circleTransferAdmissionLimit  = int64(3)
	circleTransferAdmissionWindow = time.Second
	circleTransferAdmissionKey    = "{circle_transfer_admission}:v1"
)

// circleTransferAdmissionScript is a fleet-wide rolling-window gate. Redis
// TIME is the production clock, so host skew cannot let independent
// taskworkers over-admit. The member makes a command replay idempotent: if
// go-redis lost the first response after Redis applied ZADD, retrying the same
// script does not consume another slot.
var circleTransferAdmissionScript = redis.NewScript(`
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local ttl_ms = tonumber(ARGV[3])
local member = ARGV[4]
local now_ms = tonumber(ARGV[5])

if not now_ms or now_ms < 0 then
    local redis_time = redis.call('TIME')
    now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
end

local cutoff_ms = now_ms - window_ms
redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff_ms)

if redis.call('ZSCORE', key, member) then
    return {1, 0}
end

local count = redis.call('ZCARD', key)
if count < limit then
    redis.call('ZADD', key, now_ms, member)
    redis.call('PEXPIRE', key, ttl_ms)
    return {1, 0}
end

local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local wait_ms = tonumber(oldest[2]) + window_ms - now_ms
if wait_ms < 1 then
    wait_ms = 1
end
return {0, wait_ms}
`)

type circleTransferAdmission struct {
	allowed bool
	wait    time.Duration
}

type circleTransferAdmitFunc func(context.Context, string) (circleTransferAdmission, error)
type circleTransferSleepFunc func(context.Context, time.Duration) error
type circleTransferRedisFunc func(context.Context, func(server.RedisClient))

type circleTransferLimiter struct {
	newMember func() string
	admit     circleTransferAdmitFunc
	sleep     circleTransferSleepFunc
}

// wait obtains one admission while retaining the same Redis member across
// deferrals and command retries. It returns the requested wait duration for
// bounded, privacy-safe telemetry.
func (l circleTransferLimiter) wait(ctx context.Context) (time.Duration, int, error) {
	if l.newMember == nil || l.admit == nil || l.sleep == nil {
		return 0, 0, fmt.Errorf("circle transfer admission limiter is incomplete")
	}
	member := l.newMember()
	if member == "" {
		return 0, 0, fmt.Errorf("circle transfer admission member is empty")
	}

	var totalWait time.Duration
	for deferrals := 0; ; deferrals++ {
		decision, err := l.admit(ctx, member)
		if err != nil {
			return totalWait, deferrals, err
		}
		if decision.allowed {
			return totalWait, deferrals, nil
		}
		if decision.wait < time.Millisecond || circleTransferAdmissionWindow < decision.wait {
			return totalWait, deferrals, fmt.Errorf(
				"circle transfer admission returned invalid wait %s",
				decision.wait,
			)
		}
		if err := l.sleep(ctx, decision.wait); err != nil {
			return totalWait, deferrals + 1, err
		}
		totalWait += decision.wait
	}
}

func redisCircleTransferAdmission(
	ctx context.Context,
	client server.RedisClient,
	key string,
	member string,
	nowMillis int64,
) (circleTransferAdmission, error) {
	values, err := circleTransferAdmissionScript.Run(
		ctx,
		client,
		[]string{key},
		circleTransferAdmissionLimit,
		circleTransferAdmissionWindow.Milliseconds(),
		(2 * circleTransferAdmissionWindow).Milliseconds(),
		member,
		nowMillis,
	).Slice()
	if err != nil {
		return circleTransferAdmission{}, err
	}
	if len(values) != 2 {
		return circleTransferAdmission{}, fmt.Errorf(
			"circle transfer admission returned %d values, want 2",
			len(values),
		)
	}
	allowed, allowedOK := values[0].(int64)
	waitMillis, waitOK := values[1].(int64)
	if !allowedOK || !waitOK || (allowed != 0 && allowed != 1) || waitMillis < 0 {
		return circleTransferAdmission{}, fmt.Errorf(
			"circle transfer admission returned invalid values %#v",
			values,
		)
	}
	return circleTransferAdmission{
		allowed: allowed == 1,
		wait:    time.Duration(waitMillis) * time.Millisecond,
	}, nil
}

func admitCircleTransfer(ctx context.Context, member string) (
	decision circleTransferAdmission,
	returnErr error,
) {
	return admitCircleTransferWithRedis(
		ctx,
		member,
		func(ctx context.Context, callback func(server.RedisClient)) {
			server.Redis(ctx, callback)
		},
	)
}

// admitCircleTransferWithRedis converts the server Redis wrapper's narrowly
// scoped panic contract into an ordinary admission error. The caller can then
// increment fail-closed telemetry and log the event before AdvancePayment
// returns; no Circle HTTP request has occurred at this point.
func admitCircleTransferWithRedis(
	ctx context.Context,
	member string,
	redisCall circleTransferRedisFunc,
) (
	decision circleTransferAdmission,
	returnErr error,
) {
	if err := ctx.Err(); err != nil {
		return circleTransferAdmission{}, err
	}
	if redisCall == nil {
		return circleTransferAdmission{}, fmt.Errorf("circle transfer admission Redis call is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			switch value := recovered.(type) {
			case error:
				returnErr = fmt.Errorf("circle transfer admission Redis failure: %w", value)
			default:
				returnErr = fmt.Errorf("circle transfer admission Redis failure: %v", value)
			}
			decision = circleTransferAdmission{}
		}
	}()
	redisCall(ctx, func(client server.RedisClient) {
		decision, returnErr = redisCircleTransferAdmission(
			ctx,
			client,
			circleTransferAdmissionKey,
			member,
			-1,
		)
	})
	return
}

func sleepForCircleTransferAdmission(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var defaultCircleTransferLimiter = circleTransferLimiter{
	newMember: func() string { return server.NewId().String() },
	admit:     admitCircleTransfer,
	sleep:     sleepForCircleTransferAdmission,
}

var circleTransferAdmissions = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "circle",
	Name:      "transfer_admissions_total",
	Help:      "Circle developer transfer POSTs admitted by the fleet-wide rolling-window gate",
})

var circleTransferDeferrals = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "circle",
	Name:      "transfer_deferrals_total",
	Help:      "Circle developer transfer POST admission decisions deferred because the rolling fleet-wide window was full",
})

var circleTransferAdmissionErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "circle",
	Name:      "transfer_admission_errors_total",
	Help:      "Circle developer transfer POSTs failed closed before submission because admission could not be obtained",
})

var circleTransferAdmissionWait = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "circle",
	Name:      "transfer_admission_wait_seconds",
	Help:      "Time a Circle developer transfer POST waited for the fleet-wide rolling-window gate",
	Buckets:   []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 15, 30, 60},
})

func init() {
	prometheus.MustRegister(
		circleTransferAdmissions,
		circleTransferDeferrals,
		circleTransferAdmissionErrors,
		circleTransferAdmissionWait,
	)
}

func waitForCircleTransferAdmission(ctx context.Context) error {
	waited, deferrals, err := defaultCircleTransferLimiter.wait(ctx)
	if 0 < deferrals {
		circleTransferDeferrals.Add(float64(deferrals))
	}
	if err != nil {
		circleTransferAdmissionErrors.Inc()
		glog.Infof(
			"[circlec][transfer-admission] failed closed after %d deferral(s), wait=%s: %s",
			deferrals,
			waited,
			err,
		)
		return fmt.Errorf("circle transfer admission: %w", err)
	}

	circleTransferAdmissions.Inc()
	circleTransferAdmissionWait.Observe(waited.Seconds())
	if 0 < deferrals {
		glog.Infof(
			"[circlec][transfer-admission] admitted after %d deferral(s), wait=%s",
			deferrals,
			waited,
		)
	}
	return nil
}
