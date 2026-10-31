package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.6 maps to signal_reservation_release.go and
// signal_reservation_release_test.go. urnetwork_redis_contract_reservation_total
// counts a service's Redis reservation operations by result (§1.3c).
// release-pending releases tokens a request reserved but did not publish, and
// its error result is a release that failed and is retried or left pending.
// In incident B those errors ran at 23/s then 409/s within four minutes of
// onset and 66k/s median through the storm. The deferred compensation owner
// counts a compensation it could not queue as dropped and one whose budget
// ended in its queue as expired; both leave tokens reserved until recovery
// takes them.
//
// Result children are created on first use. A service that publishes the
// operation runs its code, so a missing result child there is zero events.
// The api and the proxy publish both operations at every minute; a minute
// either publishes nothing is unknown. Other services (alt, taskworker)
// publish an operation only after their processes first use it, so they are
// judged while they publish. A build without the deferred owner never emits
// dropped or expired, so the compensation arm reads zero there: that is no
// evidence the owner is healthy.
func NewReservationReleaseSignal() Signal {
	return NewReservationReleaseSignalWithSettings(DefaultReservationReleaseSettings())
}

// The signal with explicit thresholds.
func NewReservationReleaseSignalWithSettings(settings *ReservationReleaseSettings) Signal {
	return &signalAdapter{
		number: "26.6", key: "reservation-release", name: "Reservation release errors and compensation outcomes",
		probe: reservationReleaseProbe{settings: settings},
	}
}

const reservationReleaseProbeId = "mimir/reservation-release"

// The thresholds and bounds of §26.6.
type ReservationReleaseSettings struct {
	// the services that must publish both operations at every evaluation; a
	// service the active placement does not run creates no requirement. Any
	// other service that publishes an operation is judged while it does.
	Services []string
	// the rate window of each evaluation
	Window time.Duration
	// a service's release-pending errors above ReleaseErrors per second for
	// ReleaseHold warn
	ReleaseErrors float64
	ReleaseHold   time.Duration
	// a service's dropped plus expired compensations above CompensationLost
	// per second for CompensationHold warn
	CompensationLost float64
	CompensationHold time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.6.
func DefaultReservationReleaseSettings() *ReservationReleaseSettings {
	return &ReservationReleaseSettings{
		Services:          []string{"api", "proxy"},
		Window:            5 * time.Minute,
		ReleaseErrors:     10,
		ReleaseHold:       5 * time.Minute,
		CompensationLost:  1,
		CompensationHold:  10 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ReservationReleaseSettings) valid() bool {
	return self != nil && 0 < len(self.Services) && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 <= self.ReleaseErrors && 0 <= self.CompensationLost && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.ReleaseHold, self.CompensationHold)
}

type reservationReleaseProbe struct {
	settings *ReservationReleaseSettings
}

func (reservationReleaseProbe) id() string   { return reservationReleaseProbeId }
func (reservationReleaseProbe) tier() string { return tierWarn }

func (self reservationReleaseProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self reservationReleaseProbe) effectiveSettings() *ReservationReleaseSettings {
	if self.settings == nil {
		return DefaultReservationReleaseSettings()
	}
	return self.settings
}

// §26.6's two expressions, and the services that publish each operation.
func reservationReleaseQuery(environment string, settings *ReservationReleaseSettings) string {
	counter := "urnetwork_redis_contract_reservation_total"
	env := "env=" + strconv.Quote(environment)
	window := "[" + stormDuration(settings.Window) + "]"
	return strings.Join([]string{
		fleetPart("release_errors", "sum by (service) (rate("+counter+"{"+env+`,operation="release-pending",result="error"}`+window+"))"),
		fleetPart("compensation", "sum by (service, result) (rate("+counter+"{"+env+`,operation="compensation"}`+window+"))"),
		fleetPart("release_publishers", "count by (service) ("+counter+"{"+env+`,operation="release-pending"})`),
		fleetPart("compensation_publishers", "count by (service) ("+counter+"{"+env+`,operation="compensation"})`),
	}, " or ")
}

var reservationReleaseShapes = map[string]stormShape{
	"release_errors":          {keys: []string{"service"}},
	"compensation":            {keys: []string{"service", "result"}},
	"release_publishers":      {keys: []string{"service"}},
	"compensation_publishers": {keys: []string{"service"}},
}

func (self reservationReleaseProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/reservation-release"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(reservationReleaseProbeId, "reservation-release-unobservable", unobservableTarget, "§26.6", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, reservationReleaseQuery(env.cfg.env, settings), reservationReleaseShapes, settings.holdSteps(max(settings.ReleaseHold, settings.CompensationHold)), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	reasons := history.invalidReasons()
	gaps := []string{}
	findings := []finding{}

	required := stormPlacedServices(env, settings.Services)
	requiredBools := map[string]bool{}
	for _, service := range required {
		requiredBools[service] = true
	}
	// the required services and every other service that publishes the part
	judged := func(part string) []string {
		serviceBools := map[string]bool{}
		for _, service := range required {
			serviceBools[service] = true
		}
		for _, service := range history.keys(part) {
			serviceBools[service] = true
		}
		services := []string{}
		for service := range serviceBools {
			services = append(services, service)
		}
		sort.Strings(services)
		return services
	}
	releaseServices := judged("release_publishers")
	if len(releaseServices) == 0 {
		reasons = append(reasons, "release_operation_missing")
	}
	releaseSteps := history.last(settings.holdSteps(settings.ReleaseHold))
	for _, service := range releaseServices {
		errorsAt := func(step int64) stormReading {
			if rate := history.reading("release_errors", service, step); rate.present {
				return rate
			}
			// a publisher without the error child has had no error
			if history.reading("release_publishers", service, step).present {
				return stormValue(0)
			}
			return stormReading{}
		}
		hold := stormHoldOf(releaseSteps, stormLevel{threshold: settings.ReleaseErrors}, errorsAt)
		target := env.cfg.env + "/" + service
		if !hold.complete && requiredBools[service] {
			gaps = append(gaps, service+"[release-missing]")
		}
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: reservationReleaseProbeId, tier: tierWarn, class: "reservation-release-errors", target: target, sustain: 1,
				symptom:   fmt.Sprintf("%s's release-pending reservation releases failed more than %g times a second at every evaluation for %s", service, settings.ReleaseErrors, settings.ReleaseHold),
				mechanism: "release-pending returns the tokens a request reserved in Redis but did not publish; an error is a release that failed and is retried or left pending. In incident B a connect drain burst contract creation on every proxy process, each failed creation retried its release, and the retry loops churned the admission Redis client's connections until Redis main threads saturated: 23 errors/s at 14:46Z, 409/s a minute later.",
				baseline:  fmt.Sprintf("At most 0.6 errors/s on any service when healthy (2026-10-26 08:00-12:00Z and 14:10-14:40Z). WARN when a service passes %g/s at every one-minute evaluation for %s.", settings.ReleaseErrors, settings.ReleaseHold),
				observed:  fmt.Sprintf("service=%s release_pending_errors_per_second_now=%s evaluation_unix=%d", service, stormNumber(errorsAt(history.at()), "%.1f"), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of urnetwork_redis_contract_reservation_total{operation=\"release-pending\",result=\"error\"}, %s rates by service, through a services host's loopback Mimir.", stormDuration(settings.Window)),
				context:   "A Redis failover or slot migration fails in-flight releases once; the hold covers it. The counter has no node label: find the node with the Redis entries (§26.7, §26.8). A release error leaves its tokens pending, so a payer's displayed balance can stay low for up to 25 hours: the errors cost credit as well as load.",
				action:    "Read the pool-timeout (§26.7) and HELLO (§26.8) entries for the node, and hold connect, alt and proxy rollouts.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the service's release-pending errors at or under %g/s.", settings.ReleaseHold, settings.ReleaseErrors),
				playbook:  "SIGNALS.md §26.6 and §1.3c",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(reservationReleaseProbeId, tierWarn, "reservation-release-errors", target))
		default:
			findings = append(findings, stormClearHold(reservationReleaseProbeId, "reservation-release-errors", target))
		}
	}

	compensationSteps := history.last(settings.holdSteps(settings.CompensationHold))
	for _, service := range judged("compensation_publishers") {
		lostAt := func(step int64) stormReading {
			if !history.reading("compensation_publishers", service, step).present {
				return stormReading{}
			}
			// a publisher without a result child has had none of that result
			lost := 0.0
			for _, result := range []string{"dropped", "expired"} {
				if rate := history.reading("compensation", service+"/"+result, step); rate.present {
					lost += rate.value
				}
			}
			return stormValue(lost)
		}
		hold := stormHoldOf(compensationSteps, stormLevel{threshold: settings.CompensationLost}, lostAt)
		target := env.cfg.env + "/" + service
		if !hold.complete && requiredBools[service] {
			gaps = append(gaps, service+"[compensation-missing]")
		}
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: reservationReleaseProbeId, tier: tierWarn, class: "compensation-dropped", target: target, sustain: 1,
				symptom:   fmt.Sprintf("%s's deferred compensation owner dropped or expired more than %g compensations a second at every evaluation for %s", service, settings.CompensationLost, settings.CompensationHold),
				mechanism: "A compensation releases a refused contract's reserved tokens. The deferred owner counts dropped when its 1,024-balance queue is full and expired when a request's budget ends in its queue; both leave the tokens reserved until a page on the balance takes them after 10 minutes, or their 24-hour lease ends. A sustained dropped rate means Redis refuses releases faster than the owner retries them.",
				baseline:  fmt.Sprintf("No baseline yet; re-derive the level after a week of the owner in production. WARN when dropped plus expired passes %g/s at every one-minute evaluation for %s.", settings.CompensationLost, settings.CompensationHold),
				observed:  fmt.Sprintf("service=%s dropped_plus_expired_per_second_now=%s dropped_per_second_now=%s expired_per_second_now=%s evaluation_unix=%d", service, stormNumber(lostAt(history.at()), "%.2f"), stormNumber(history.reading("compensation", service+"/dropped", history.at()), "%.2f"), stormNumber(history.reading("compensation", service+"/expired", history.at()), "%.2f"), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of urnetwork_redis_contract_reservation_total{operation=\"compensation\"}, %s rates by service and result, through a services host's loopback Mimir.", stormDuration(settings.Window)),
				context:   "Tokens a dropped or expired compensation leaves reserved under-admit their payer until recovery takes them; they never over-admit. A build without the deferred owner emits neither result.",
				action:    "Read the release-error arm, the pool-timeout (§26.7) and HELLO (§26.8) entries to find the Redis node refusing releases, and hold connect, alt and proxy rollouts. Do not raise the owner's queue or worker count.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the service's dropped plus expired compensations at or under %g/s.", settings.CompensationHold, settings.CompensationLost),
				playbook:  "SIGNALS.md §26.6",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(reservationReleaseProbeId, tierWarn, "compensation-dropped", target))
		default:
			findings = append(findings, stormClearHold(reservationReleaseProbeId, "compensation-dropped", target))
		}
	}

	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(reservationReleaseProbeId, tierWarn, "reservation-release-unobservable", unobservableTarget))
	}
	return findings, nil
}
