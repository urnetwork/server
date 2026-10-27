package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.1 maps to signal_pgbouncer_backlog.go and
// signal_pgbouncer_backlog_test.go. Clients queue in PgBouncer when every
// server connection is busy; in the 2026-10-25/26 incident A, CPU-starved
// connect processes held their transactions open, so PgBouncer's servers
// stayed busy while 1,500 to 11,000 clients waited for hours. The probe reads
// the pool exporter's waiting clients (five-minute averages summed over every
// pool) and oldest wait (five-minute maximum over every pool) at each minute
// of its hold, with the count of PgBouncer instances each answer covers. A
// sum over fewer pools can only be lower, so a held band is proven whatever
// the coverage; a clear needs every configured instance at every evaluation.
func NewPgBouncerBacklogSignal() Signal {
	return NewPgBouncerBacklogSignalWithSettings(DefaultPgBouncerBacklogSettings())
}

// The signal with explicit thresholds.
func NewPgBouncerBacklogSignalWithSettings(settings *PgBouncerBacklogSettings) Signal {
	return &signalAdapter{
		number: "26.1", key: "pgbouncer-backlog", name: "PgBouncer backlog",
		probe: pgbouncerBacklogProbe{settings: settings},
	}
}

const pgbouncerBacklogProbeId = "mimir/pgbouncer-backlog"

// The thresholds and bounds of §26.1.
type PgBouncerBacklogSettings struct {
	// the averaging (waiting) and maximum (oldest wait) window of each
	// evaluation
	Window time.Duration
	// clients waiting across every pool above WarnWaiting for WarnHold warn,
	// and above PageWaiting for PageHold page
	WarnWaiting float64
	WarnHold    time.Duration
	PageWaiting float64
	PageHold    time.Duration
	// the oldest wait in any pool, in seconds, above WarnOldestWait for
	// OldestWaitHold warns
	WarnOldestWait float64
	OldestWaitHold time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.1.
func DefaultPgBouncerBacklogSettings() *PgBouncerBacklogSettings {
	return &PgBouncerBacklogSettings{
		Window:            5 * time.Minute,
		WarnWaiting:       100,
		WarnHold:          10 * time.Minute,
		PageWaiting:       1000,
		PageHold:          5 * time.Minute,
		WarnOldestWait:    0.5,
		OldestWaitHold:    10 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *PgBouncerBacklogSettings) valid() bool {
	return self != nil && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 <= self.WarnWaiting && self.WarnWaiting <= self.PageWaiting && 0 <= self.WarnOldestWait &&
		0 < self.MaxListed && self.StormReadSettings.valid(self.WarnHold, self.PageHold, self.OldestWaitHold)
}

// The longest hold, which sets the read's span.
func (self *PgBouncerBacklogSettings) points() int {
	return self.holdSteps(max(self.WarnHold, self.PageHold, self.OldestWaitHold))
}

type pgbouncerBacklogProbe struct {
	settings *PgBouncerBacklogSettings
}

func (pgbouncerBacklogProbe) id() string   { return pgbouncerBacklogProbeId }
func (pgbouncerBacklogProbe) tier() string { return tierPage }

func (self pgbouncerBacklogProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self pgbouncerBacklogProbe) effectiveSettings() *PgBouncerBacklogSettings {
	if self.settings == nil {
		return DefaultPgBouncerBacklogSettings()
	}
	return self.settings
}

// §26.1's two expressions, and the PgBouncer instances each covers.
func pgbouncerBacklogQuery(environment string, settings *PgBouncerBacklogSettings) string {
	selector := "{env=" + strconv.Quote(environment) + "}"
	window := stormDuration(settings.Window)
	waiting := "avg_over_time(pgbouncer_pool_clients_waiting" + selector + "[" + window + "])"
	oldest := "max_over_time(pgbouncer_pool_maxwait_seconds" + selector + "[" + window + "])"
	return strings.Join([]string{
		fleetPart("waiting", "sum("+waiting+")"),
		fleetPart("oldest_wait", "max("+oldest+")"),
		fleetPart("waiting_instances", "count(count by (host, pgbouncer) ("+waiting+"))"),
		fleetPart("oldest_wait_instances", "count(count by (host, pgbouncer) ("+oldest+"))"),
	}, " or ")
}

// Every part is one fleet-wide series.
var pgbouncerBacklogShapes = map[string]stormShape{
	"waiting":               {},
	"oldest_wait":           {},
	"waiting_instances":     {},
	"oldest_wait_instances": {},
}

// The PgBouncer instances the inventory declares on its database hosts; zero
// when any database host leaves its census unconfigured.
func pgbouncerBacklogExpectedInstances(env *probeEnv) int {
	hosts := env.cfg.hostsWithRole("pg-primary")
	expected := 0
	for _, h := range hosts {
		if h.pgbouncerInstances <= 0 {
			return 0
		}
		expected += h.pgbouncerInstances
	}
	return expected
}

func (self pgbouncerBacklogProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	target := env.cfg.env + "/pgbouncer"
	unobservableTarget := env.cfg.env + "/pgbouncer-backlog"
	if !settings.valid() {
		return []finding{stormUnobservable(pgbouncerBacklogProbeId, "pgbouncer-backlog-unobservable", unobservableTarget, "§26.1", []string{"invalid_policy"}, nil, 8)}, nil
	}
	history, err := readStormHistory(ctx, env, pgbouncerBacklogQuery(env.cfg.env, settings), pgbouncerBacklogShapes, settings.points(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormUnobservable(pgbouncerBacklogProbeId, "pgbouncer-backlog-unobservable", unobservableTarget, "§26.1", []string{stormReadReason(err)}, nil, settings.MaxListed)}, nil
	}

	expected := pgbouncerBacklogExpectedInstances(env)
	reasons := history.invalidReasons()
	gaps := []string{}
	if expected == 0 {
		reasons = append(reasons, "instance_census_unconfigured")
	}
	// a clear needs every declared instance in both answers at every step
	censusComplete := 0 < expected
	for _, step := range history.steps {
		for _, part := range []string{"waiting_instances", "oldest_wait_instances"} {
			count := history.reading(part, "", step)
			if !count.present || count.value != float64(expected) {
				censusComplete = false
			}
		}
	}
	if !censusComplete && 0 < expected {
		reasons = append(reasons, "instances_missing")
		gaps = append(gaps, fmt.Sprintf("pgbouncer[answered=%s/%d]", stormNumber(history.reading("waiting_instances", "", history.at()), "%.0f"), expected))
	}

	waitingAt := func(step int64) stormReading { return history.reading("waiting", "", step) }
	oldestAt := func(step int64) stormReading { return history.reading("oldest_wait", "", step) }
	warnWaiting := stormHoldOf(history.last(settings.holdSteps(settings.WarnHold)), stormLevel{threshold: settings.WarnWaiting}, waitingAt)
	pageWaiting := stormHoldOf(history.last(settings.holdSteps(settings.PageHold)), stormLevel{threshold: settings.PageWaiting}, waitingAt)
	warnOldest := stormHoldOf(history.last(settings.holdSteps(settings.OldestWaitHold)), stormLevel{threshold: settings.WarnOldestWait}, oldestAt)
	for _, band := range []stormHold{warnWaiting, pageWaiting, warnOldest} {
		if !band.complete {
			reasons = append(reasons, "evaluation_missing")
			break
		}
	}

	findings := []finding{}
	if 0 < len(reasons) {
		findings = append(findings, stormUnobservable(pgbouncerBacklogProbeId, "pgbouncer-backlog-unobservable", unobservableTarget, "§26.1", reasons, gaps, settings.MaxListed))
	} else {
		findings = append(findings, healthyFinding(pgbouncerBacklogProbeId, tierWarn, "pgbouncer-backlog-unobservable", unobservableTarget))
	}

	observed := fmt.Sprintf("waiting_now=%s oldest_wait_now_seconds=%s instances_answering=%s/%d evaluation_unix=%d step=%s window=%s",
		stormNumber(waitingAt(history.at()), "%.1f"), stormNumber(oldestAt(history.at()), "%.3f"),
		stormNumber(history.reading("waiting_instances", "", history.at()), "%.0f"), expected,
		history.at(), settings.Step, stormDuration(settings.Window))
	switch {
	case pageWaiting.held || warnWaiting.held || warnOldest.held:
		tier := tierWarn
		arms := []string{}
		if pageWaiting.held {
			tier = tierPage
			arms = append(arms, fmt.Sprintf("waiting above %.0f for %s", settings.PageWaiting, settings.PageHold))
		}
		if warnWaiting.held && !pageWaiting.held {
			arms = append(arms, fmt.Sprintf("waiting above %.0f for %s", settings.WarnWaiting, settings.WarnHold))
		}
		if warnOldest.held {
			arms = append(arms, fmt.Sprintf("the oldest wait above %g s for %s", settings.WarnOldestWait, settings.OldestWaitHold))
		}
		findings = append(findings, finding{
			probeId: pgbouncerBacklogProbeId, tier: tier, class: "pgbouncer-backlog", target: target, sustain: 1,
			symptom:   fmt.Sprintf("PgBouncer clients queued for a server: %s", strings.Join(arms, "; ")),
			mechanism: "A client waits in PgBouncer when every server connection of its pool is busy. In incident A (2026-10-25 19:00Z to 10-26 14:08Z) connect ran out of CPU and its clients held their transactions open, so PgBouncer's servers stayed busy while 1,500 to 11,000 clients waited. The oldest wait stays under query_wait_timeout (4 s), which drops a client before it waits longer, so a backlog shows in the waiting count long before §2.11a's three-second rule.",
			baseline:  fmt.Sprintf("Waiting 0 to 16 (five-minute averages) and the oldest wait at most 0.37 s on a healthy day (2026-10-20). WARN above %.0f waiting for %s or the oldest wait above %g s for %s; PAGE above %.0f waiting for %s, each at every one-minute evaluation.", settings.WarnWaiting, settings.WarnHold, settings.WarnOldestWait, settings.OldestWaitHold, settings.PageWaiting, settings.PageHold),
			observed:  observed,
			evidence:  fmt.Sprintf("One bounded Mimir range read through a services host's loopback Mimir: sum(avg_over_time(pgbouncer_pool_clients_waiting[%s])) and max(max_over_time(pgbouncer_pool_maxwait_seconds[%s])) at each %s step of the hold, with the PgBouncer instances each answer covers. Only counts leave Mimir.", stormDuration(settings.Window), stormDuration(settings.Window), settings.Step),
			context:   "A deploy's reconnect wave can queue past 1,000 for minutes (10-24 21:02Z, 1,216 during the api and connect rollout); read rollout-guard. The fleet sum hides one hot instance, and a sum over fewer pools can only be lower, so a held band stands even while an instance is missing.",
			action:    "Read the idle-in-transaction groups (§1.3), services-tier CPU (§26.2) and run-queue delay (§8.14a) to find who holds the servers. Hold connect and alt rollouts: their reconnect waves made A's cancellation peaks at 21:10Z and 23:00Z. Reduce the load that holds transactions open; do not raise pool sizes.",
			verify:    fmt.Sprintf("Every evaluation of a complete %s hold has at most %.0f clients waiting and an oldest wait of at most %g s, with every PgBouncer instance answering.", max(settings.WarnHold, settings.OldestWaitHold), settings.WarnWaiting, settings.WarnOldestWait),
			playbook:  "SIGNALS.md §26.1, §2.11b and §5.6",
		})
	case warnWaiting.clear && pageWaiting.clear && warnOldest.clear && censusComplete:
		findings = append(findings, healthyFinding(pgbouncerBacklogProbeId, tierPage, "pgbouncer-backlog", target))
	default:
		findings = append(findings, stormClearHold(pgbouncerBacklogProbeId, "pgbouncer-backlog", target))
	}
	return findings, nil
}
