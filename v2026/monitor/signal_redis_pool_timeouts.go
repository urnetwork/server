package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.7 maps to signal_redis_pool_timeouts.go and
// signal_redis_pool_timeouts_test.go. A go-redis pool timeout is a caller that
// waited the pool timeout without getting a connection. §3.5b's redis-pool
// warns at a ten-minute floor of 50 timeouts/s; in incident B the proxy
// passed 10/s at 14:48Z but 50/s only at about 15:00Z, so this is the earlier
// band: a service above 10 timeouts/s at each minute of a five-minute hold.
// The counters are registered with every pool, so a placed service that
// stops publishing is unknown, not zero.
func NewRedisPoolTimeoutsSignal() Signal {
	return NewRedisPoolTimeoutsSignalWithSettings(DefaultRedisPoolTimeoutsSettings())
}

// The signal with explicit thresholds.
func NewRedisPoolTimeoutsSignalWithSettings(settings *RedisPoolTimeoutsSettings) Signal {
	return &signalAdapter{
		number: "26.7", key: "redis-pool-timeouts", name: "Redis client pool timeouts per service",
		probe: redisPoolTimeoutsProbe{settings: settings},
	}
}

const redisPoolTimeoutsProbeId = "mimir/redis-pool-timeouts"

// The thresholds and bounds of §26.7.
type RedisPoolTimeoutsSettings struct {
	// the services that export the pool counters; one the active placement
	// does not run creates no requirement
	Services []string
	// the rate window of each evaluation
	Window time.Duration
	// a service's pool timeouts above WarnTimeouts per second for Hold warn
	WarnTimeouts float64
	Hold         time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.7.
func DefaultRedisPoolTimeoutsSettings() *RedisPoolTimeoutsSettings {
	return &RedisPoolTimeoutsSettings{
		Services:          []string{"alt", "api", "connect", "gossip", "mcp", "proxy", "taskworker"},
		Window:            5 * time.Minute,
		WarnTimeouts:      10,
		Hold:              5 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *RedisPoolTimeoutsSettings) valid() bool {
	return self != nil && 0 < len(self.Services) && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 <= self.WarnTimeouts && 0 < self.MaxListed && self.StormReadSettings.valid(self.Hold)
}

type redisPoolTimeoutsProbe struct {
	settings *RedisPoolTimeoutsSettings
}

func (redisPoolTimeoutsProbe) id() string   { return redisPoolTimeoutsProbeId }
func (redisPoolTimeoutsProbe) tier() string { return tierWarn }

func (self redisPoolTimeoutsProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self redisPoolTimeoutsProbe) effectiveSettings() *RedisPoolTimeoutsSettings {
	if self.settings == nil {
		return DefaultRedisPoolTimeoutsSettings()
	}
	return self.settings
}

// §26.7's expression, and the misses (new dials) beside it.
func redisPoolTimeoutsQuery(environment string, settings *RedisPoolTimeoutsSettings) string {
	selector := "{env=" + strconv.Quote(environment) + "}[" + stormDuration(settings.Window) + "]"
	return strings.Join([]string{
		fleetPart("timeouts", "sum by (service) (rate(urnetwork_redis_pool_timeouts_total"+selector+"))"),
		fleetPart("misses", "sum by (service) (rate(urnetwork_redis_pool_misses_total"+selector+"))"),
	}, " or ")
}

var redisPoolTimeoutsShapes = map[string]stormShape{
	"timeouts": {keys: []string{"service"}},
	"misses":   {keys: []string{"service"}},
}

func (self redisPoolTimeoutsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/redis-pool-timeouts"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(redisPoolTimeoutsProbeId, "redis-pool-timeouts-unobservable", unobservableTarget, "§26.7", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, redisPoolTimeoutsQuery(env.cfg.env, settings), redisPoolTimeoutsShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	required := stormPlacedServices(env, settings.Services)
	serviceBools := map[string]bool{}
	for _, service := range required {
		serviceBools[service] = true
	}
	for _, service := range history.keys("timeouts") {
		serviceBools[service] = true
	}
	services := []string{}
	for service := range serviceBools {
		services = append(services, service)
	}
	sort.Strings(services)

	reasons := history.invalidReasons()
	if len(required) == 0 {
		reasons = append(reasons, "placement_missing")
	}
	steps := history.last(settings.holdSteps(settings.Hold))
	missing := []string{}
	findings := []finding{}
	for _, service := range services {
		timeoutsAt := func(step int64) stormReading { return history.reading("timeouts", service, step) }
		hold := stormHoldOf(steps, stormLevel{threshold: settings.WarnTimeouts}, timeoutsAt)
		target := env.cfg.env + "/" + service
		if !hold.complete {
			missing = append(missing, service+"[missing]")
		}
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: redisPoolTimeoutsProbeId, tier: tierWarn, class: "redis-pool-timeouts", target: target, sustain: 1,
				symptom:   fmt.Sprintf("%s's Redis pools timed out more than %g times a second at every evaluation for %s", service, settings.WarnTimeouts, settings.Hold),
				mechanism: "A pool timeout is a caller that waited the pool timeout without getting a connection: the service's Redis connections are all busy, or new ones cannot be dialed. In incident B the proxy's contract-creation cleanup loops churned the admission Redis client's connections until Redis main threads saturated; its timeouts passed 10/s at 14:48Z and reached 14,348/s at 15:43Z.",
				baseline:  fmt.Sprintf("0 on every service when healthy (2026-10-20, 10-25 08:00-12:00Z, 10-26 14:10-14:40Z); connect up to 1.3/s while starved of CPU on the morning of 10-26. WARN when a service passes %g/s at every one-minute evaluation for %s; §3.5b warns at a 50/s floor and pages at 1,000/s.", settings.WarnTimeouts, settings.Hold),
				observed:  fmt.Sprintf("service=%s timeouts_per_second_now=%s misses_per_second_now=%s evaluation_unix=%d", service, stormNumber(timeoutsAt(history.at()), "%.1f"), stormNumber(history.reading("misses", service, history.at()), "%.1f"), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of urnetwork_redis_pool_timeouts_total and urnetwork_redis_pool_misses_total, %s rates by service, through a services host's loopback Mimir.", stormDuration(settings.Window)),
				context:   "The counters name no node, and nested pipeline and pub/sub pools are not exported (§3.5b). A CPU-starved client times out its own pool without a Redis fault: connect fired through its CPU-saturated stretch of 10-21 to 10-23, so read services-tier CPU (§26.2) before blaming Redis for connect.",
				action:    "Find the node with the HELLO and main-thread entries (§26.8), and do not raise pool sizes or timeouts. For the proxy, see incident B's follow-ups (a connection closed on every timeout, a PING per callback).",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the service's pool timeouts at or under %g/s.", settings.Hold, settings.WarnTimeouts),
				playbook:  "SIGNALS.md §26.7 and §3.5b",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(redisPoolTimeoutsProbeId, tierWarn, "redis-pool-timeouts", target))
		default:
			findings = append(findings, stormClearHold(redisPoolTimeoutsProbeId, "redis-pool-timeouts", target))
		}
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, missing))
	} else {
		findings = append(findings, healthyFinding(redisPoolTimeoutsProbeId, tierWarn, "redis-pool-timeouts-unobservable", unobservableTarget))
	}
	return findings, nil
}
