package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.3 maps to signal_db_acquire_wait.go and
// signal_db_acquire_wait_test.go. Every service takes its database
// connections from a pgx pool and exports the total wait of its successful
// acquisitions and their count (db_metrics.go). Their ratio is the service's
// mean acquire wait: the service with the wait is the one queued. In
// incident A the proxy's mean passed 100 ms at 2026-10-25 20:08Z and stayed
// there most of the night. The probe reads each service's mean at each minute
// of its hold. A service with no acquisitions in the window has no mean and
// waits for nothing, so it reads within the band; a service that stops
// publishing is unknown.
func NewDbAcquireWaitSignal() Signal {
	return NewDbAcquireWaitSignalWithSettings(DefaultDbAcquireWaitSettings())
}

// The signal with explicit thresholds.
func NewDbAcquireWaitSignalWithSettings(settings *DbAcquireWaitSettings) Signal {
	return &signalAdapter{
		number: "26.3", key: "db-acquire-wait", name: "Service database pool acquire wait",
		probe: dbAcquireWaitProbe{settings: settings},
	}
}

const dbAcquireWaitProbeId = "mimir/db-acquire-wait"

// The thresholds and bounds of §26.3.
type DbAcquireWaitSettings struct {
	// the services that export the pool's acquire metrics; one the active
	// placement does not run creates no requirement
	Services []string
	// the pool the services' transactions take connections from
	Pool string
	// the rate window of each evaluation
	Window time.Duration
	// a service's mean acquire wait above WarnMean for Hold warns
	WarnMean time.Duration
	Hold     time.Duration
	// the most services and gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.3.
func DefaultDbAcquireWaitSettings() *DbAcquireWaitSettings {
	return &DbAcquireWaitSettings{
		Services:          []string{"alt", "api", "connect", "gossip", "mcp", "proxy", "taskworker"},
		Pool:              "default",
		Window:            5 * time.Minute,
		WarnMean:          100 * time.Millisecond,
		Hold:              5 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *DbAcquireWaitSettings) valid() bool {
	return self != nil && 0 < len(self.Services) && self.Pool != "" && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 < self.WarnMean && 0 < self.MaxListed && self.StormReadSettings.valid(self.Hold)
}

type dbAcquireWaitProbe struct {
	settings *DbAcquireWaitSettings
}

func (dbAcquireWaitProbe) id() string   { return dbAcquireWaitProbeId }
func (dbAcquireWaitProbe) tier() string { return tierWarn }

func (self dbAcquireWaitProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self dbAcquireWaitProbe) effectiveSettings() *DbAcquireWaitSettings {
	if self.settings == nil {
		return DefaultDbAcquireWaitSettings()
	}
	return self.settings
}

// §26.3's expression, its denominator alone, and the canceled acquisitions
// the mean does not time.
func dbAcquireWaitQuery(environment string, settings *DbAcquireWaitSettings) string {
	selector := "env=" + strconv.Quote(environment) + ",pool=" + strconv.Quote(settings.Pool)
	window := "[" + stormDuration(settings.Window) + "]"
	duration := "sum by (service) (rate(urnetwork_pg_pool_acquire_duration_seconds_total{" + selector + "}" + window + "))"
	acquired := `sum by (service) (rate(urnetwork_pg_pool_acquires_total{` + selector + `,outcome="acquired"}` + window + "))"
	canceled := `sum by (service) (rate(urnetwork_pg_pool_acquires_total{` + selector + `,outcome="canceled"}` + window + "))"
	return strings.Join([]string{
		fleetPart("mean", duration+" / "+acquired),
		fleetPart("acquired", acquired),
		fleetPart("canceled", canceled),
	}, " or ")
}

var dbAcquireWaitShapes = map[string]stormShape{
	"mean":     {keys: []string{"service"}},
	"acquired": {keys: []string{"service"}},
	"canceled": {keys: []string{"service"}},
}

func (self dbAcquireWaitProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/db-acquire-wait"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(dbAcquireWaitProbeId, "db-acquire-wait-unobservable", unobservableTarget, "§26.3", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, dbAcquireWaitQuery(env.cfg.env, settings), dbAcquireWaitShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	// every placed publisher is required; any other answering service is
	// judged too
	required := stormPlacedServices(env, settings.Services)
	serviceBools := map[string]bool{}
	for _, service := range required {
		serviceBools[service] = true
	}
	for _, part := range []string{"mean", "acquired"} {
		for _, service := range history.keys(part) {
			serviceBools[service] = true
		}
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
	level := stormLevel{threshold: settings.WarnMean.Seconds()}
	steps := history.last(settings.holdSteps(settings.Hold))
	missing := []string{}
	findings := []finding{}
	for _, service := range services {
		meanAt := func(step int64) stormReading {
			if mean := history.reading("mean", service, step); mean.present {
				return mean
			}
			// no acquisitions in the window: no mean, and no wait
			if acquired := history.reading("acquired", service, step); acquired.present && acquired.value == 0 {
				return stormValue(0)
			}
			return stormReading{}
		}
		hold := stormHoldOf(steps, level, meanAt)
		target := env.cfg.env + "/" + service
		if !hold.complete {
			missing = append(missing, service+"[missing]")
		}
		switch {
		case hold.held:
			mean := meanAt(history.at())
			meanMilliseconds := stormNumber(stormReading{value: 1000 * mean.value, present: mean.present}, "%.1f")
			findings = append(findings, finding{
				probeId: dbAcquireWaitProbeId, tier: tierWarn, class: "db-acquire-wait", target: target, sustain: 1,
				symptom:   fmt.Sprintf("%s waited more than %s on average for a database connection at every evaluation for %s", service, settings.WarnMean, settings.Hold),
				mechanism: "The mean is the total wait of a service's successful pool acquisitions over their count. A service waits when every connection its pool may open is busy or PgBouncer queues its logins: the service with the wait is the one queued. In incident A the proxy's mean passed 100 ms at 2026-10-25 20:08Z (230 ms) and held 183 ms median through the night, peaking at 1,030 ms.",
				baseline:  fmt.Sprintf("Connect 1.2-5.3 ms and the proxy, api and taskworker under 4 ms on a healthy day (2026-10-20). WARN when a service's mean passes %s at every one-minute evaluation for %s.", settings.WarnMean, settings.Hold),
				observed:  fmt.Sprintf("service=%s mean_ms_now=%s acquired_per_second=%s canceled_per_second=%s evaluation_unix=%d", service, meanMilliseconds, stormNumber(history.reading("acquired", service, history.at()), "%.1f"), stormNumber(history.reading("canceled", service, history.at()), "%.1f"), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of urnetwork_pg_pool_acquire_duration_seconds_total over urnetwork_pg_pool_acquires_total{outcome=\"acquired\"} for pool %q, five-minute rates by service, through a services host's loopback Mimir.", settings.Pool),
				context:   "Canceled acquisitions are counted but not timed (A's postmortem counts 1.77M abandoned on connect and 780k on the proxy), so read canceled_per_second beside the mean. A service with few acquisitions (alt, gossip, mcp) can swing on a handful of slow ones.",
				action:    "Read the PgBouncer backlog (§26.1) and services-tier CPU (§26.2) entries, then the idle-in-transaction groups (§1.3). Do not raise pool sizes.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the service's mean at or under %s.", settings.Hold, settings.WarnMean),
				playbook:  "SIGNALS.md §26.3, §26.1 and §5.6",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(dbAcquireWaitProbeId, tierWarn, "db-acquire-wait", target))
		default:
			findings = append(findings, stormClearHold(dbAcquireWaitProbeId, "db-acquire-wait", target))
		}
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, missing))
	} else {
		findings = append(findings, healthyFinding(dbAcquireWaitProbeId, tierWarn, "db-acquire-wait-unobservable", unobservableTarget))
	}
	return findings, nil
}
