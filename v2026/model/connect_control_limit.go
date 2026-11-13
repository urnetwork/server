// The per-client limit of POST /connect/control (support inbox 8093, 8077).
//
// Every contract request a client makes, and most of its other control
// messages, is its own /connect/control request, so a provider's request rate
// grows with the clients it carries. The load balancer used to count the
// route in its shared per-address zone (450/min with a burst of 150 in main),
// and a phone sharing with about 1,000 clients got about 13,900 429s in 8
// minutes: its return contracts failed, its clients' replies were dropped and
// its network checks failed. The owner's decision (2026-10-27): the limit for
// /connect/control is per client id, with its own tuned limits.
//
// The api is the authoritative limit. It keys a GCRA (a token bucket read
// from the other side) on the client id of the verified credential, so a
// client can neither spend another client's budget nor escape its own with a
// made-up id, and keeps the state in Redis, so the limit holds across every
// api process the load balancer spreads one client's requests over. One
// script reads and advances the client's theoretical arrival time in one
// round trip. The lb keeps only a per-address backstop for the route
// (services.yml route_rate_limits), well above this limit.
//
// A refused request is answered 429 with Retry-After, which a client treats
// as a rate limit on its own contract requests (connect 7753f993). A Redis
// failure or deadline admits the request without a decision: the limit
// protects the api from one client and must not refuse every client when
// Redis is slow.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// config/<env>/connect_control_limit.yml overrides the defaults below. Absent
// is the defaults.
const ConnectControlLimitResourceName = "connect_control_limit.yml"

// The per-client limit: a sustained rate and the requests admitted at once
// above it, in nginx's units. A client admits burst+1 requests at once, then
// one per 60/RequestsPerMinute seconds.
type ConnectControlLimitSettings struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	Burst             int `yaml:"burst"`
}

// The defaults, from the per-client rates on Main on 2026-10-27. The busiest
// provider client created 2,331 contracts to 1,663 distinct clients in 10
// minutes (20:25-20:35Z), 1.4 per client served, and the api answered 5.7 to
// 9.3 /connect/control requests per contract created, so a provider carrying
// 2,000 clients sends about 1,600-2,600 requests a minute. 6,000 a minute
// leaves it 2.3x headroom at the higher ratio. A burst of 3,000 (30 s of the
// rate) absorbs a provider restart, when every client asks for a return
// contract at once. Half of all clients created one contract in those 10
// minutes and 99.9% at most 103, under 100 requests a minute even at the
// higher ratio. One client can't sustain more than 100 requests a second,
// about 1% of the route's peak (10,700/s).
func DefaultConnectControlLimitSettings() ConnectControlLimitSettings {
	return ConnectControlLimitSettings{
		RequestsPerMinute: 6000,
		Burst:             3000,
	}
}

// The largest rate and burst a profile may set; beyond them the limit means
// nothing and the script's microsecond arithmetic stays exact.
const (
	maxConnectControlLimitRequestsPerMinute = 6_000_000
	maxConnectControlLimitBurst             = 1_000_000
)

func (self ConnectControlLimitSettings) Validate() error {
	if self.RequestsPerMinute < 1 || maxConnectControlLimitRequestsPerMinute < self.RequestsPerMinute {
		return fmt.Errorf("connect control limit requests_per_minute must be 1..%d", maxConnectControlLimitRequestsPerMinute)
	}
	if self.Burst < 0 || maxConnectControlLimitBurst < self.Burst {
		return fmt.Errorf("connect control limit burst must be 0..%d", maxConnectControlLimitBurst)
	}
	return nil
}

// The time one request takes from the budget, in microseconds, at least one.
func (self ConnectControlLimitSettings) emissionIntervalMicros() int64 {
	return max(1, int64(math.Round(60_000_000/float64(self.RequestsPerMinute))))
}

// How far ahead of now a client's arrival time may run and still be admitted.
func (self ConnectControlLimitSettings) toleranceMicros() int64 {
	return int64(self.Burst) * self.emissionIntervalMicros()
}

// Parses a profile: both fields as integers, nothing else, one document.
// The fields are read as YAML values rather than decoded into the struct,
// whose int decoder would truncate a fractional rate.
func ParseConnectControlLimitSettings(data []byte) (ConnectControlLimitSettings, error) {
	var settings ConnectControlLimitSettings
	if len(data) == 0 || 4096 < len(data) {
		return settings, errors.New("connect control limit profile size invalid")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		return settings, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return settings, errors.New("connect control limit profile must have one document")
	}
	if len(fields) != 2 {
		return settings, errors.New("connect control limit profile must set requests_per_minute and burst, and nothing else")
	}
	requestsPerMinute, ok := fields["requests_per_minute"].(int)
	if !ok {
		return settings, errors.New("connect control limit requests_per_minute must be an integer")
	}
	burst, ok := fields["burst"].(int)
	if !ok {
		return settings, errors.New("connect control limit burst must be an integer")
	}
	settings = ConnectControlLimitSettings{
		RequestsPerMinute: requestsPerMinute,
		Burst:             burst,
	}
	return settings, settings.Validate()
}

// Loaded once per process, as the api reads its other request profiles. An
// absent profile is the defaults; an unreadable or invalid one is logged and
// the defaults apply, so a bad profile never removes the limit.
var configuredConnectControlLimitSettings = sync.OnceValue(func() ConnectControlLimitSettings {
	resource, err := server.Config.SimpleResource(ConnectControlLimitResourceName)
	if errors.Is(err, server.ErrResourceNotFound) {
		return DefaultConnectControlLimitSettings()
	}
	var data []byte
	if err == nil {
		data, err = resource.BytesE()
	}
	var settings ConnectControlLimitSettings
	if err == nil {
		settings, err = ParseConnectControlLimitSettings(data)
	}
	if err != nil {
		glog.Errorf("[connect]control limit profile %s unusable; using the defaults: %s\n", ConnectControlLimitResourceName, err)
		return DefaultConnectControlLimitSettings()
	}
	glog.Infof("[connect]control limit %d/min burst %d\n", settings.RequestsPerMinute, settings.Burst)
	return settings
})

// The settings this process applies.
func ConnectControlLimitSettingsForProcess() ConnectControlLimitSettings {
	return configuredConnectControlLimitSettings()
}

// One decision waits at most this long for Redis, then admits.
const connectControlLimitRedisTimeout = 250 * time.Millisecond

// KEYS[1] is the client's theoretical arrival time (TAT), in integer
// microseconds of the clock below. ARGV[1] is the emission interval and
// ARGV[2] the tolerance (burst * interval), in microseconds. ARGV[3] is now in
// microseconds, or -1 for the Redis node's clock, which every api process
// shares for this key, so no process clock decides. A request is admitted
// unless the TAT runs more than the tolerance ahead of now; an admitted
// request moves the TAT one interval on and keeps the key until the TAT,
// when the bucket is full again. A refused request changes nothing, so a
// client that keeps asking still gets its sustained rate. Returns {1, 0} for
// an admitted request, else {0, microseconds until one would be admitted}.
// The TAT is written with %.0f because Lua's default number format keeps 14
// digits, fewer than a microsecond timestamp has.
var connectControlLimitScript = redis.NewScript(`
local interval = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
if now < 0 then
    local time = redis.call('TIME')
    now = tonumber(time[1]) * 1000000 + tonumber(time[2])
end
local tat = tonumber(redis.call('GET', KEYS[1]))
if tat == nil or tat < now then
    tat = now
end
local wait = tat - tolerance - now
if 0 < wait then
    return {0, wait}
end
local next_tat = tat + interval
redis.call('SET', KEYS[1], string.format('%.0f', next_tat), 'PX', math.ceil((next_tat - now) / 1000) + 1)
return {1, 0}
`)

// One key per client, on its own hash slot, so the clients spread over the
// cluster.
func connectControlLimitKey(clientId server.Id) string {
	return fmt.Sprintf("{connect_control_limit_%s}", clientId)
}

// The decision for one request.
type ConnectControlLimitResult struct {
	Allowed bool
	// how long until the client's next request would be admitted; zero when
	// allowed
	RetryAfter time.Duration
}

// Admits or refuses one /connect/control request of a verified client
// against the client's shared budget. An error is a Redis failure or the
// deadline; the request has no decision then.
func AdmitConnectControl(
	ctx context.Context,
	clientId server.Id,
	settings ConnectControlLimitSettings,
) (ConnectControlLimitResult, error) {
	return AdmitConnectControlAt(ctx, clientId, settings, -1)
}

// AdmitConnectControl at nowMicros, microseconds since the epoch, or at the
// Redis node's clock for -1. Production passes -1; a test passes its own
// clock.
func AdmitConnectControlAt(
	ctx context.Context,
	clientId server.Id,
	settings ConnectControlLimitSettings,
	nowMicros int64,
) (result ConnectControlLimitResult, returnErr error) {
	if err := settings.Validate(); err != nil {
		return result, err
	}
	boundedCtx, cancel := context.WithTimeout(ctx, connectControlLimitRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		values, err := connectControlLimitScript.Run(
			boundedCtx,
			r,
			[]string{connectControlLimitKey(clientId)},
			settings.emissionIntervalMicros(),
			settings.toleranceMicros(),
			nowMicros,
		).Int64Slice()
		if err != nil {
			return err
		}
		if len(values) != 2 || values[0] != 0 && values[0] != 1 || values[1] < 0 {
			return fmt.Errorf("connect control limit answered %v", values)
		}
		result = ConnectControlLimitResult{
			Allowed:    values[0] == 1,
			RetryAfter: time.Duration(values[1]) * time.Microsecond,
		}
		return nil
	}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies())
	if returnErr != nil {
		return ConnectControlLimitResult{}, returnErr
	}
	return
}

// The answer to a refused request: 429 with the whole seconds until the
// client's next request would be admitted, at least one.
func NewConnectControlLimitError(retryAfter time.Duration) error {
	return &rateLimitError{
		message:           "429 Too many control requests for this client.",
		retryAfterSeconds: max(1, int(math.Ceil(retryAfter.Seconds()))),
	}
}
