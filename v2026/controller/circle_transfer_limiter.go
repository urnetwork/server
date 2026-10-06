package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
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
// taskworkers over-admit. While its reservation remains in the active rolling
// window, the member makes a lost-response command replay idempotent. After
// that reservation is cut off, replaying the same member must obtain one fresh
// current slot and bucket before the caller can proceed to the HTTP POST.
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

local existing_score = redis.call('ZSCORE', key, member)
if existing_score then
    return {1, 0, math.floor(tonumber(existing_score) / 1000)}
end

local count = redis.call('ZCARD', key)
if count < limit then
    redis.call('ZADD', key, now_ms, member)
    redis.call('PEXPIRE', key, ttl_ms)
    return {1, 0, math.floor(now_ms / 1000)}
end

local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local wait_ms = tonumber(oldest[2]) + window_ms - now_ms
if wait_ms < 1 then
    wait_ms = 1
end
return {0, wait_ms, 0}
`)

type circleTransferAdmission struct {
	allowed         bool
	wait            time.Duration
	admissionSecond int64
}

type circleTransferAdmitFunc func(context.Context, string) (circleTransferAdmission, error)
type circleTransferSleepFunc func(context.Context, time.Duration) error
type circleTransferRedisFunc func(context.Context, func(server.RedisClient))

type circleTransferLimiter struct {
	newMember func() string
	admit     circleTransferAdmitFunc
	sleep     circleTransferSleepFunc
}

// wait obtains one current admission while retaining the same Redis member
// across deferrals and command retries. Replay reuses an existing reservation
// only while it remains inside the rolling window; after cutoff, the script
// returns one fresh admission. wait returns the requested wait duration and
// authoritative Redis admission second for bounded, privacy-safe telemetry.
func (l circleTransferLimiter) wait(ctx context.Context) (time.Duration, int, int64, error) {
	if l.newMember == nil || l.admit == nil || l.sleep == nil {
		return 0, 0, 0, fmt.Errorf("circle transfer admission limiter is incomplete")
	}
	member := l.newMember()
	if member == "" {
		return 0, 0, 0, fmt.Errorf("circle transfer admission member is empty")
	}

	var totalWait time.Duration
	for deferrals := 0; ; deferrals++ {
		decision, err := l.admit(ctx, member)
		if err != nil {
			return totalWait, deferrals, 0, err
		}
		if decision.allowed {
			if decision.admissionSecond <= 0 {
				return totalWait, deferrals, 0, fmt.Errorf(
					"circle transfer admission returned invalid admission second %d",
					decision.admissionSecond,
				)
			}
			return totalWait, deferrals, decision.admissionSecond, nil
		}
		if decision.wait < time.Millisecond || circleTransferAdmissionWindow < decision.wait {
			return totalWait, deferrals, 0, fmt.Errorf(
				"circle transfer admission returned invalid wait %s",
				decision.wait,
			)
		}
		if err := l.sleep(ctx, decision.wait); err != nil {
			return totalWait, deferrals + 1, 0, err
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
	if len(values) != 3 {
		return circleTransferAdmission{}, fmt.Errorf(
			"circle transfer admission returned %d values, want 3",
			len(values),
		)
	}
	allowed, allowedOK := values[0].(int64)
	waitMillis, waitOK := values[1].(int64)
	admissionSecond, admissionSecondOK := values[2].(int64)
	if !allowedOK || !waitOK || !admissionSecondOK ||
		(allowed != 0 && allowed != 1) || waitMillis < 0 ||
		(allowed == 1 && (waitMillis != 0 || admissionSecond <= 0)) ||
		(allowed == 0 && admissionSecond != 0) {
		return circleTransferAdmission{}, fmt.Errorf(
			"circle transfer admission returned invalid values %#v",
			values,
		)
	}
	return circleTransferAdmission{
		allowed:         allowed == 1,
		wait:            time.Duration(waitMillis) * time.Millisecond,
		admissionSecond: admissionSecond,
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

var circleTransferAdmissionObservableInfo = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "circle",
	Name:      "transfer_admission_observable_info",
	Help:      "Whether this process emits one bounded pre-POST event for every admitted Circle developer transfer",
})

var circleTransferAdmissionSequence atomic.Uint64

func init() {
	prometheus.MustRegister(
		circleTransferAdmissions,
		circleTransferDeferrals,
		circleTransferAdmissionErrors,
		circleTransferAdmissionWait,
		circleTransferAdmissionObservableInfo,
	)
	circleTransferAdmissionObservableInfo.Set(1)
}

func waitForCircleTransferAdmission(ctx context.Context) error {
	return waitForCircleTransferAdmissionWith(
		ctx,
		defaultCircleTransferLimiter,
		recordCircleTransferAdmission,
	)
}

func waitForCircleTransferAdmissionWith(
	ctx context.Context,
	limiter circleTransferLimiter,
	recordAdmission func(time.Duration, int, int64),
) error {
	if recordAdmission == nil {
		return fmt.Errorf("circle transfer admission observer is nil")
	}
	waited, deferrals, admissionSecond, err := limiter.wait(ctx)
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
	recordAdmission(waited, deferrals, admissionSecond)
	return nil
}

// recordCircleTransferAdmission is the exact observation boundary for the
// fleet ceiling. The caller invokes it only after Redis admits the request;
// waitForCircleTransferAdmission returns only after this fixed, identifier-free
// marker is emitted, and CreateTransferTransaction performs the HTTP POST only
// after that return. Its bucket is the Redis admission second returned by the
// atomic script, so host clock and logger scheduling do not own the invariant.
// Response and task-evaluator timestamps are deliberately excluded because
// independent requests can finish together.
func recordCircleTransferAdmission(waited time.Duration, deferrals int, admissionSecond int64) {
	circleTransferAdmissions.Inc()
	circleTransferAdmissionWait.Observe(waited.Seconds())
	sequence := circleTransferAdmissionSequence.Add(1)
	glog.Infof("%s", circleTransferAdmissionLogLine(waited, deferrals, sequence, admissionSecond))
}

func circleTransferAdmissionLogLine(
	waited time.Duration,
	deferrals int,
	sequence uint64,
	admissionSecond int64,
) string {
	return fmt.Sprintf(
		"[circlec][transfer-admission] admitted observable=v1 redis_second=%d sequence=%d deferrals=%d wait_ms=%d",
		admissionSecond,
		sequence,
		deferrals,
		waited.Milliseconds(),
	)
}
