package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/geo/solve"
	"github.com/urnetwork/server/model"
)

// SIGNALS.md §2.19c: every condition against fixture rows, a fixed clock and a
// fake Redis history, the comparisons across runs, and the production queries
// against a seeded database.

// What the three queries and the Redis history return. Its zero value is not
// healthy; start from healthyDerivedLocationsFixture.
type derivedLocationsFixture struct {
	table derivedLocationsTable
	// the last complete hour, and the other hours of the day that had pings
	lastHour   derivedLocationsPingHour
	otherHours []derivedLocationsPingHour
	state      derivedLocationsState
	partitions derivedLocationsPartitions
	// newest first
	runs []*model.DeriveLocationsRun

	redisErr error
	// replaces the history the runs encode
	redisOut *string

	queries   []string
	redisArgs [][]string
}

// The synthetic clock of syntheticSettings.
var derivedLocationsNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// A healthy run `age` before the synthetic clock that published `published`
// nodes, with a projection of the next run well inside the solve's budget.
func derivedLocationsRun(age time.Duration, published int) *model.DeriveLocationsRun {
	return &model.DeriveLocationsRun{
		RunTime:           derivedLocationsNow.Add(-age),
		Nodes:             1100,
		Sources:           1080,
		Terms:             33000,
		CosignedPings:     90000,
		Published:         published,
		ExcludedSources:   5,
		ResidualKm:        6.2,
		GenesisResidualKm: 48,
		Converged:         true,
		LastRoundSweeps:   12,
		SweepCap:          100,
		Sweeps:            30,
		Cores:             16,
		ProjectedSeconds:  4.5,
		ProjectedBytes:    180 * 1024 * 1024,
		MaxSolveSeconds:   600,
		MaxSolveBytes:     8 * 1024 * 1024 * 1024,
	}
}

// Every condition healthy, with three runs of history.
func healthyDerivedLocationsFixture() *derivedLocationsFixture {
	return &derivedLocationsFixture{
		table: derivedLocationsTable{
			published:            400,
			crossedRegion:        8,
			crossedCountry:       2,
			atMinPeers:           40,
			medianResidualKm:     6.5,
			medianReputation:     0.9,
			lastUpdateAgeSeconds: 3600,
			expired:              0,
		},
		lastHour: derivedLocationsPingHour{
			cosigned:         900,
			rejected:         30,
			rateLimited:      5,
			unknown:          50,
			relayed:          10,
			zeroRtt:          0,
			beyondHalfPlanet: 5,
		},
		otherHours: []derivedLocationsPingHour{
			{cosigned: 880, rejected: 25, rateLimited: 4, unknown: 40, relayed: 9, beyondHalfPlanet: 4},
			{cosigned: 910, rejected: 28, rateLimited: 6, unknown: 45, relayed: 11, beyondHalfPlanet: 3},
		},
		state: derivedLocationsState{
			taskRows:           1,
			failingTaskRows:    0,
			maxRescheduleCount: 0,
			activeExtenders:    1,
			connectedProviders: 1,
		},
		// yesterday, today and two days ahead
		partitions: derivedLocationsPartitions{
			partitioned:                  true,
			partitions:                   4,
			overdueDrops:                 0,
			oldestOverdueUpperAgeSeconds: -1,
			checkedDayCovered:            true,
		},
		runs: []*model.DeriveLocationsRun{
			derivedLocationsRun(time.Hour, 400),
			derivedLocationsRun(9*time.Hour, 395),
			derivedLocationsRun(17*time.Hour, 390),
		},
	}
}

// One row of the ping query, as psql renders it.
func derivedLocationsHourRow(hour derivedLocationsPingHour, label string, lastComplete bool) Row {
	return Row{
		label,
		strconv.FormatBool(lastComplete),
		strconv.FormatInt(hour.cosigned, 10),
		strconv.FormatInt(hour.rejected, 10),
		strconv.FormatInt(hour.rateLimited, 10),
		strconv.FormatInt(hour.unknown, 10),
		strconv.FormatInt(hour.relayed, 10),
		strconv.FormatInt(hour.zeroRtt, 10),
		strconv.FormatInt(hour.beyondHalfPlanet, 10),
	}
}

// The source that answers the queries and the history from the fixture,
// recording what was asked.
func (self *derivedLocationsFixture) source(t testing.TB) *syntheticSource {
	return &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			self.queries = append(self.queries, query)
			switch {
			case strings.Contains(query, "monitor-signal-2.19c-derived-locations-table"):
				table := self.table
				return []Row{{
					strconv.FormatInt(table.published, 10),
					strconv.FormatInt(table.crossedRegion, 10),
					strconv.FormatInt(table.crossedCountry, 10),
					strconv.FormatInt(table.atMinPeers, 10),
					strconv.FormatFloat(table.medianResidualKm, 'f', -1, 64),
					strconv.FormatFloat(table.medianReputation, 'f', -1, 64),
					strconv.FormatInt(table.lastUpdateAgeSeconds, 10),
					strconv.FormatInt(table.expired, 10),
				}}, nil
			case strings.Contains(query, "monitor-signal-2.19c-derived-locations-pings"):
				rows := []Row{}
				for i, hour := range self.otherHours {
					rows = append(rows, derivedLocationsHourRow(hour, fmt.Sprintf("2026-08-29 %02d:00:00", 8+i), false))
				}
				if self.lastHour.pings() != 0 {
					rows = append(rows, derivedLocationsHourRow(self.lastHour, "2026-08-29 11:00:00", true))
				}
				return rows, nil
			case strings.Contains(query, "monitor-signal-2.19c-derived-locations-state"):
				state := self.state
				return []Row{{
					strconv.FormatInt(state.taskRows, 10),
					strconv.FormatInt(state.failingTaskRows, 10),
					strconv.FormatInt(state.maxRescheduleCount, 10),
					strconv.FormatInt(state.activeExtenders, 10),
					strconv.FormatInt(state.connectedProviders, 10),
				}}, nil
			case strings.Contains(query, "monitor-signal-2.19c-derived-locations-partitions"):
				partitions := self.partitions
				return []Row{{
					strconv.FormatBool(partitions.partitioned),
					strconv.FormatInt(partitions.partitions, 10),
					strconv.FormatInt(partitions.overdueDrops, 10),
					strconv.FormatInt(partitions.oldestOverdueUpperAgeSeconds, 10),
					strconv.FormatBool(partitions.checkedDayCovered),
				}}, nil
			default:
				t.Fatalf("unexpected query %q", query)
				return nil, nil
			}
		},
		redisFn: func(host HostSettings, port int, args ...string) (string, error) {
			self.redisArgs = append(self.redisArgs, args)
			if self.redisErr != nil {
				return "", self.redisErr
			}
			if self.redisOut != nil {
				return *self.redisOut, nil
			}
			lines := []string{}
			for _, run := range self.runs {
				runJson, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(runJson))
			}
			return strings.Join(lines, "\n") + "\n", nil
		},
	}
}

// The alerts of one run of the signal over the fixture, at the given settings
// or the defaults.
func (self *derivedLocationsFixture) run(t *testing.T, settings *DerivedLocationsSettings) Alerts {
	t.Helper()
	if settings == nil {
		settings = DefaultDerivedLocationsSettings()
	}
	alerts, err := NewDerivedLocationsSignalWithSettings(settings).Run(context.Background(), syntheticSettings(self.source(t)))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// The findings of one check, healthy ones included.
func (self *derivedLocationsFixture) findings(t *testing.T) []finding {
	t.Helper()
	settings := syntheticSettings(self.source(t)).withDefaults()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (derivedLocationsProbe{settings: DefaultDerivedLocationsSettings()}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// The classes and frames of the alerts, as class/frame.
func derivedLocationsAlertKeys(alerts Alerts) []string {
	keys := []string{}
	for _, alert := range alerts {
		keys = append(keys, alert.Class+"/"+alert.Frame)
	}
	return keys
}

// How the alerts differ from the classes and frames wanted, or from what every
// §2.19c alert carries; "" when they do not.
func derivedLocationsAlertsMismatch(alerts Alerts, want ...string) string {
	got := derivedLocationsAlertKeys(alerts)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Sprintf("alerts %v, want %v", got, want)
	}
	for _, alert := range alerts {
		if alert.Severity != SeverityWarn || alert.SignalNumber != "2.19c" || alert.SignalKey != "derived-locations" || alert.Target != "derive-locations" {
			return fmt.Sprintf("alert not identified as §2.19c: %+v", alert)
		}
		if !strings.Contains(alert.Observed, "threshold=") && alert.Class != "derive-run-history-unobservable" {
			return fmt.Sprintf("%s does not name its threshold: %q", alert.Class, alert.Observed)
		}
	}
	return ""
}

// Fails the test unless the alerts are the classes and frames wanted, in
// order, each a complete §2.19c alert.
func requireDerivedLocationsAlerts(t *testing.T, alerts Alerts, want ...string) {
	t.Helper()
	if mismatch := derivedLocationsAlertsMismatch(alerts, want...); mismatch != "" {
		t.Fatal(mismatch)
	}
	for _, alert := range alerts {
		requireAlertClass(t, alerts, alert.Class)
	}
}

// Every condition healthy: no alert, and one healthy finding per class, so an
// open ticket of any of them resolves.
func TestDerivedLocationsSignalHealthyBaseline(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	classes := map[string]int{}
	for _, finding := range healthyDerivedLocationsFixture().findings(t) {
		if !finding.healthy {
			t.Fatalf("unhealthy finding %s/%s in the baseline", finding.class, finding.frame)
		}
		classes[finding.class] += 1
	}
	for _, class := range []string{
		"derive-run-history-unobservable",
		"derive-not-running",
		"derive-supply-gone",
		"derive-refusals",
		"derive-no-verdicts",
		"derive-published-collapse",
		"derive-residual-not-improving",
		"derive-crossings",
		"derive-exclusions",
		"derive-non-convergence",
		"derive-thin-evidence",
		"derive-capacity",
		"derive-sweep-stalled",
		"derive-clock-or-wire",
	} {
		if classes[class] != 1 {
			t.Errorf("class %s has %d healthy findings, want 1", class, classes[class])
		}
	}
	if len(classes) != 14 {
		t.Errorf("healthy classes %v, want the thirteen conditions and the history", classes)
	}

	// the history is read three runs deep, the deepest comparison
	connect.AssertEqual(t, fixture.redisArgs, [][]string{{"-c", "--raw", "LRANGE", model.DeriveLocationsRunsRedisKey, "0", "2"}})
}

// Each condition and each of its parts, alone against the healthy baseline.
func TestDerivedLocationsSignalConditions(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(fixture *derivedLocationsFixture)
		want []string
	}{
		{
			name: "not running: the last run is older than twice the cadence",
			edit: func(fixture *derivedLocationsFixture) {
				fixture.runs = []*model.DeriveLocationsRun{derivedLocationsRun(17*time.Hour, 400), derivedLocationsRun(25*time.Hour, 395)}
			},
			want: []string{"derive-not-running/stale-run"},
		},
		{
			name: "not running: no pending_task row",
			edit: func(fixture *derivedLocationsFixture) { fixture.state.taskRows = 0 },
			want: []string{"derive-not-running/task-missing"},
		},
		{
			name: "not running: the row is parked on its error backoff",
			edit: func(fixture *derivedLocationsFixture) {
				fixture.state.failingTaskRows = 1
				fixture.state.maxRescheduleCount = 4
			},
			want: []string{"derive-not-running/task-parked"},
		},
		{
			name: "supply gone",
			edit: func(fixture *derivedLocationsFixture) {
				// too few pings for any share to be judged
				fixture.lastHour = derivedLocationsPingHour{cosigned: 40, rejected: 2, unknown: 3}
			},
			want: []string{"derive-supply-gone/"},
		},
		{
			name: "refusals: rejected",
			edit: func(fixture *derivedLocationsFixture) { fixture.lastHour.rejected = 150 },
			want: []string{"derive-refusals/rejected"},
		},
		{
			name: "refusals: rate limited",
			edit: func(fixture *derivedLocationsFixture) {
				fixture.lastHour.rejected = 70
				fixture.lastHour.rateLimited = 60
			},
			want: []string{"derive-refusals/rate-limited"},
		},
		{
			name: "no verdicts",
			edit: func(fixture *derivedLocationsFixture) { fixture.lastHour.unknown = 450 },
			want: []string{"derive-no-verdicts/"},
		},
		{
			name: "crossings: country",
			edit: func(fixture *derivedLocationsFixture) {
				fixture.table.crossedCountry = 12
				fixture.table.crossedRegion = 12
			},
			want: []string{"derive-crossings/country"},
		},
		{
			name: "crossings: region",
			edit: func(fixture *derivedLocationsFixture) { fixture.table.crossedRegion = 48 },
			want: []string{"derive-crossings/region"},
		},
		{
			name: "exclusions: excluded share",
			edit: func(fixture *derivedLocationsFixture) { fixture.runs[0].ExcludedSources = 120 },
			want: []string{"derive-exclusions/excluded-share"},
		},
		{
			name: "exclusions: median reputation",
			edit: func(fixture *derivedLocationsFixture) { fixture.table.medianReputation = 0.45 },
			want: []string{"derive-exclusions/median-reputation"},
		},
		{
			name: "non-convergence: nodes still moving",
			edit: func(fixture *derivedLocationsFixture) { fixture.runs[0].RefusedStillMoving = 120 },
			want: []string{"derive-non-convergence/still-moving"},
		},
		{
			name: "thin evidence",
			edit: func(fixture *derivedLocationsFixture) { fixture.table.atMinPeers = 130 },
			want: []string{"derive-thin-evidence/"},
		},
		{
			name: "capacity: the next run's seconds",
			edit: func(fixture *derivedLocationsFixture) { fixture.runs[0].ProjectedSeconds = 540 },
			want: []string{"derive-capacity/seconds"},
		},
		{
			name: "capacity: the next run's bytes",
			edit: func(fixture *derivedLocationsFixture) { fixture.runs[0].ProjectedBytes = 7 * 1024 * 1024 * 1024 },
			want: []string{"derive-capacity/bytes"},
		},
		{
			name: "sweep: a day partition past its drop",
			edit: func(fixture *derivedLocationsFixture) {
				fixture.partitions.overdueDrops = 2
				fixture.partitions.oldestOverdueUpperAgeSeconds = 3 * 24 * 3600
			},
			want: []string{"derive-sweep-stalled/partition-drop"},
		},
		{
			name: "sweep: no partition for tomorrow",
			edit: func(fixture *derivedLocationsFixture) { fixture.partitions.checkedDayCovered = false },
			want: []string{"derive-sweep-stalled/partition-ahead"},
		},
		{
			name: "sweep: a table the conversion has not reached has no partitions to judge",
			edit: func(fixture *derivedLocationsFixture) {
				fixture.partitions = derivedLocationsPartitions{partitioned: false, overdueDrops: 3, oldestOverdueUpperAgeSeconds: 600000, checkedDayCovered: false}
			},
			want: []string{},
		},
		{
			name: "sweep: derived locations",
			edit: func(fixture *derivedLocationsFixture) { fixture.table.expired = 2 },
			want: []string{"derive-sweep-stalled/derived_location"},
		},
		{
			name: "clock or wire: zero round trips",
			edit: func(fixture *derivedLocationsFixture) { fixture.lastHour.zeroRtt = 12 },
			want: []string{"derive-clock-or-wire/zero-rtt"},
		},
		{
			name: "clock or wire: beyond half the planet",
			edit: func(fixture *derivedLocationsFixture) { fixture.lastHour.beyondHalfPlanet = 55 },
			want: []string{"derive-clock-or-wire/beyond-half-planet"},
		},
	} {
		fixture := healthyDerivedLocationsFixture()
		test.edit(fixture)
		alerts := fixture.run(t, nil)
		if mismatch := derivedLocationsAlertsMismatch(alerts, test.want...); mismatch != "" {
			t.Errorf("%s: %s", test.name, mismatch)
			continue
		}
		for _, alert := range alerts {
			requireAlertClass(t, alerts, alert.Class)
		}
	}
}

// The thresholds are the lines: at the threshold nothing fires, one past it
// the condition does.
func TestDerivedLocationsSignalThresholdBoundaries(t *testing.T) {
	// 10 % of 1000 verdicts exactly, then one more
	fixture := healthyDerivedLocationsFixture()
	fixture.lastHour = derivedLocationsPingHour{cosigned: 900, rejected: 100}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.lastHour = derivedLocationsPingHour{cosigned: 899, rejected: 101}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-refusals/rejected")

	// sixteen hours old exactly, then one second more
	fixture = healthyDerivedLocationsFixture()
	fixture.runs = []*model.DeriveLocationsRun{derivedLocationsRun(16*time.Hour, 400)}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.runs = []*model.DeriveLocationsRun{derivedLocationsRun(16*time.Hour+time.Second, 400)}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-not-running/stale-run")

	// 100 co-signed pings is enough, 99 is not
	fixture = healthyDerivedLocationsFixture()
	fixture.lastHour = derivedLocationsPingHour{cosigned: 100}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.lastHour = derivedLocationsPingHour{cosigned: 99}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-supply-gone/")

	// 5 % of 1100 solved nodes still moving is the line: 55 is quiet, 56
	// fires
	fixture = healthyDerivedLocationsFixture()
	fixture.runs[0].RefusedStillMoving = 55
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.runs[0].RefusedStillMoving = 56
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-non-convergence/still-moving")

	// 20 % under the budget is the margin: a projection just short of it is
	// quiet, one at it fires
	fixture = healthyDerivedLocationsFixture()
	fixture.runs[0].ProjectedSeconds = 479.9
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.runs[0].ProjectedSeconds = 480
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-capacity/seconds")
}

// The capacity condition judges the planner's projection against the budget
// the run recorded with it, which is the job's own, and falls back to the
// settings' budget only for a record that carries none; the finding carries
// the projection, the budget and the share, and the first run after a deploy
// has nothing to judge.
func TestDerivedLocationsSignalCapacity(t *testing.T) {
	// the job on a larger host records a larger budget: the same projection
	// is inside it
	fixture := healthyDerivedLocationsFixture()
	fixture.runs[0].ProjectedBytes = 7 * 1024 * 1024 * 1024
	fixture.runs[0].MaxSolveBytes = 32 * 1024 * 1024 * 1024
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// a record without a budget is judged against the settings'
	fixture = healthyDerivedLocationsFixture()
	fixture.runs[0].MaxSolveSeconds = 0
	fixture.runs[0].MaxSolveBytes = 0
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.runs[0].ProjectedSeconds = 590
	fixture.runs[0].ProjectedBytes = 9 * 1024 * 1024 * 1024
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-capacity/seconds", "derive-capacity/bytes")
	for _, want := range []string{
		"projected=590s budget=600s share=0.9833 threshold=DeriveCapacityMargin=0.20 terms=33000 nodes=1100 sweeps=30 cores=16",
		"projected=9663676416B budget=8589934592B share=1.1250 threshold=DeriveCapacityMargin=0.20",
	} {
		found := false
		for _, alert := range alerts {
			if strings.Contains(alert.Observed, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no capacity alert observes %q", want)
		}
	}

	// an environment the defaults do not fit sets its own margin
	settings := DefaultDerivedLocationsSettings()
	settings.DeriveCapacityMargin = 0.01
	requireDerivedLocationsAlerts(t, fixture.run(t, settings), "derive-capacity/bytes")

	// a projection of nothing, as a run on an empty day makes, is healthy, and
	// an empty history has nothing to judge
	fixture = healthyDerivedLocationsFixture()
	fixture.runs[0].ProjectedSeconds = 0
	fixture.runs[0].ProjectedBytes = 0
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
	fixture.runs = nil
	fixture.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// A share is judged only over enough evidence, and the supply only while
// someone is there to ping: a quiet environment is not a broken one.
func TestDerivedLocationsSignalSmallPopulations(t *testing.T) {
	// forty verdicts, half of them refused, one without a verdict for every two
	// with: under DeriveMinPingSamples, so no share is judged
	fixture := healthyDerivedLocationsFixture()
	fixture.lastHour = derivedLocationsPingHour{cosigned: 20, rejected: 20, unknown: 20, zeroRtt: 10, beyondHalfPlanet: 10}
	fixture.state.activeExtenders = 0
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// no connected provider: the supply is not expected
	fixture = healthyDerivedLocationsFixture()
	fixture.lastHour = derivedLocationsPingHour{cosigned: 10}
	fixture.state.connectedProviders = 0
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// ten published nodes, half of them across a country line and all at the
	// gate: under DeriveMinNodeSamples
	fixture = healthyDerivedLocationsFixture()
	fixture.table = derivedLocationsTable{published: 10, crossedRegion: 5, crossedCountry: 5, atMinPeers: 10, medianResidualKm: 3, medianReputation: 0.2, lastUpdateAgeSeconds: 3600}
	fixture.runs[0].Sources = 10
	fixture.runs[0].ExcludedSources = 5
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// an environment the defaults do not fit sets its own
	settings := DefaultDerivedLocationsSettings()
	settings.DeriveMinNodeSamples = 5
	requireDerivedLocationsAlerts(t, fixture.run(t, settings),
		"derive-crossings/country", "derive-crossings/region",
		"derive-exclusions/excluded-share", "derive-exclusions/median-reputation",
		"derive-thin-evidence/",
	)
}

// The published collapse compares the last run with the one before it: a
// halving on a held supply is the gates or the solve; a halving on a fallen
// supply is the data; a halving from a handful is noise.
func TestDerivedLocationsSignalPublishedCollapse(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs[0].Published = 150
	fixture.table.published = 150
	// the gates that refused the rest travel with the finding, the solver's
	// still-moving refusal beside the others; that many nodes still moving
	// is also the non-convergence it came from
	fixture.runs[0].RefusedFewPings = 40
	fixture.runs[0].RefusedStillMoving = 700
	fixture.runs[0].RefusedProbeCountry = 3
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-published-collapse/", "derive-non-convergence/still-moving")
	if !strings.Contains(alerts[0].Context, "last run refused: few_pings=40 few_peers=0 still_moving=700 no_improvement=0 probe_country=3 unmapped=0") {
		t.Fatalf("context %q does not carry the refusals", alerts[0].Context)
	}

	// the supply dipped by a rolling window's few percent: still held
	fixture.runs[0].RefusedStillMoving = 0
	fixture.runs[0].CosignedPings = 85000
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-published-collapse/")

	// the supply fell with the publication
	fixture.runs[0].CosignedPings = 50000
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// under half of a publication too small to judge
	fixture = healthyDerivedLocationsFixture()
	fixture.runs[0].Published = 3
	fixture.runs[1].Published = 10
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// the first run after a deploy has nothing to compare with
	fixture = healthyDerivedLocationsFixture()
	fixture.runs = fixture.runs[:1]
	fixture.runs[0].Published = 1
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// The residual must fail on two consecutive runs: above its ceiling or not
// below genesis. One bad run is not a finding, and a run without terms has no
// residual to judge.
func TestDerivedLocationsSignalResidualAcrossRuns(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs[0].ResidualKm = 55
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	fixture.runs[1].ResidualKm = 60
	fixture.runs[1].GenesisResidualKm = 80
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-residual-not-improving/")
	if !strings.Contains(alerts[0].Observed, "newest_first=55.00/48.00,60.00/80.00") {
		t.Fatalf("observed %q does not carry both runs", alerts[0].Observed)
	}

	// under the ceiling but no better than genesis, twice
	fixture = healthyDerivedLocationsFixture()
	for _, run := range fixture.runs[:2] {
		run.ResidualKm = 40
		run.GenesisResidualKm = 40
	}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil), "derive-residual-not-improving/")

	// the same, on a run that solved nothing
	fixture.runs[0].Terms = 0
	fixture.runs[0].ResidualKm = 0
	fixture.runs[0].GenesisResidualKm = 0
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	// one run of history
	fixture = healthyDerivedLocationsFixture()
	fixture.runs = fixture.runs[:1]
	fixture.runs[0].ResidualKm = 90
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// The exclusion findings carry the peers the run expected each kind of node to
// measure, as the job's settings made them, since a pinger kind measured
// against more peers than it is designed to reach is marked down wholesale.
func TestDerivedLocationsSignalExclusionsCarryTheExpectation(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs[0].ExcludedSources = 120
	fixture.runs[0].ExpectedExtenderPeers = 64
	fixture.runs[0].ExpectedProviderPeers = 4
	fixture.table.medianReputation = 0.3
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-exclusions/excluded-share", "derive-exclusions/median-reputation")
	for _, alert := range alerts {
		if !strings.Contains(alert.Context, "expected_peers: extender=64 provider=4") {
			t.Fatalf("%s/%s context %q does not carry the run's expectation", alert.Class, alert.Frame, alert.Context)
		}
	}
}

// Non-convergence takes the sweep cap on three consecutive runs.
func TestDerivedLocationsSignalNonConvergenceAcrossRuns(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	for _, run := range fixture.runs[:2] {
		run.Converged = false
		run.LastRoundSweeps = 100
	}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))

	fixture.runs[2].Converged = false
	fixture.runs[2].LastRoundSweeps = 100
	fixture.runs[0].RefusedStillMoving = 12
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-non-convergence/sweep-cap")
	if !strings.Contains(alerts[0].Observed, "newest_first=100/100,100/100,100/100") {
		t.Fatalf("observed %q does not carry the three runs", alerts[0].Observed)
	}
	if !strings.Contains(alerts[0].Context, "still_moving=12") {
		t.Fatalf("context %q does not carry the nodes still moving", alerts[0].Context)
	}

	// two runs of history cannot show three
	fixture.runs = fixture.runs[:2]
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// The nodes a run refused as still moving when its solve stopped are the
// other half of non-convergence: past DeriveMaxStillMovingShare of the solved
// nodes the finding names the stop, stagnation or the sweep cap, and sets the
// share against the run before; a run too small to judge is not judged.
func TestDerivedLocationsSignalStillMoving(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs[0].RefusedStillMoving = 120
	fixture.runs[0].Stagnated = true
	fixture.runs[1].RefusedStillMoving = 10
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-non-convergence/still-moving")
	if !strings.Contains(alerts[0].Observed, "still_moving=120 nodes=1100 share=0.1091 threshold=DeriveMaxStillMovingShare=0.05 converged=true stagnated=true last_round_sweeps=12 sweep_cap=100") {
		t.Fatalf("observed %q", alerts[0].Observed)
	}
	if !strings.Contains(alerts[0].Context, "previous run: still_moving=10 nodes=1100 share=0.0091") {
		t.Fatalf("context %q does not set the share against the run before", alerts[0].Context)
	}

	// ten solved nodes, all of them still moving: under DeriveMinNodeSamples
	fixture = healthyDerivedLocationsFixture()
	fixture.runs[0].Nodes = 10
	fixture.runs[0].Sources = 10
	fixture.runs[0].RefusedStillMoving = 10
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// A history that cannot be read reports itself and suppresses the
// comparisons across runs -- neither a fault nor health -- while every
// condition of the table and the pings is still judged, and "not running"
// falls back to the table.
func TestDerivedLocationsSignalUnobservableHistory(t *testing.T) {
	requireUnjudged := func(t *testing.T, fixture *derivedLocationsFixture) {
		t.Helper()
		for _, finding := range fixture.findings(t) {
			switch finding.class {
			case "derive-published-collapse", "derive-residual-not-improving", "derive-non-convergence", "derive-exclusions", "derive-capacity":
				t.Fatalf("%s was judged without a history (healthy %v)", finding.class, finding.healthy)
			}
		}
	}

	fixture := healthyDerivedLocationsFixture()
	fixture.redisErr = errors.New("synthetic redis unavailable")
	alerts := fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-run-history-unobservable/")
	if !strings.Contains(alerts[0].Observed, "reason=redis-read-failed") {
		t.Fatalf("observed %q", alerts[0].Observed)
	}
	requireUnjudged(t, fixture)

	// a record that does not parse
	fixture = healthyDerivedLocationsFixture()
	malformed := "{\"run_time\":\"2026-08-29T11:00:00Z\",\"published\":400}\nnot a run record\n"
	fixture.redisOut = &malformed
	alerts = fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts, "derive-run-history-unobservable/")
	if !strings.Contains(alerts[0].Observed, "reason=malformed-run-record malformed_records=1") {
		t.Fatalf("observed %q", alerts[0].Observed)
	}
	requireUnjudged(t, fixture)

	// the table still says the derivations stopped, and a table condition
	// still fires beside the history's
	fixture = healthyDerivedLocationsFixture()
	fixture.redisErr = errors.New("synthetic redis unavailable")
	fixture.table.lastUpdateAgeSeconds = 17 * 3600
	fixture.table.medianReputation = 0.3
	alerts = fixture.run(t, nil)
	requireDerivedLocationsAlerts(t, alerts,
		"derive-run-history-unobservable/",
		"derive-not-running/stale-run",
		"derive-exclusions/median-reputation",
	)
	if !strings.Contains(alerts[1].Observed, "source=table") {
		t.Fatalf("observed %q", alerts[1].Observed)
	}

	// an empty history is observable: the first derivation after a deploy is
	// eight hours out, and nothing is wrong before it
	fixture = healthyDerivedLocationsFixture()
	fixture.runs = nil
	fixture.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// The defaults are the ones SIGNALS.md §2.19c states, and the job's own where
// the section names the job's settings.
func TestDerivedLocationsSettingsDefaults(t *testing.T) {
	settings := DefaultDerivedLocationsSettings()
	connect.AssertEqual(t, *settings, DerivedLocationsSettings{
		DeriveMaxRunAge:                16 * time.Hour,
		DeriveMinCosignedPerHour:       100,
		DeriveSupplyMinExtenders:       1,
		DeriveSupplyMinProviders:       1,
		DeriveMinPingSamples:           100,
		DeriveMaxRefusalShare:          0.10,
		DeriveMaxRateLimitedShare:      0.05,
		DeriveMaxUnknownShare:          0.30,
		DeriveMinNodeSamples:           20,
		DerivePublishedCollapseShare:   0.5,
		DeriveCollapseSupplyShare:      0.9,
		DeriveMaxResidualKm:            50,
		DeriveResidualRuns:             2,
		DeriveMaxCountryCrossingShare:  0.02,
		DeriveMaxRegionCrossingShare:   0.10,
		DeriveMaxExcludedShare:         0.10,
		DeriveMinMedianReputation:      0.5,
		DeriveCapRuns:                  3,
		DeriveMaxStillMovingShare:      0.05,
		MinDerivePeers:                 3,
		DeriveMaxThinEvidenceShare:     0.30,
		DeriveCapacityMargin:           0.2,
		MaxSolveSeconds:                600,
		MaxSolveBytes:                  8 * 1024 * 1024 * 1024,
		PingRetention:                  24 * time.Hour,
		DeriveSweepGrace:               time.Hour,
		PingPartitionKeepTimeout:       25*time.Hour + 5*time.Minute,
		PingPartitionAheadDays:         2,
		DeriveMaxZeroRttShare:          0.01,
		DeriveMaxBeyondHalfPlanetShare: 0.05,
		DeriveHalfPlanetRttMs:          200,
	})
	// the peer gate is the solver's own, never a second copy of it
	connect.AssertEqual(t, settings.MinDerivePeers, solve.DefaultSettings().MinDerivePeers)
	signal := NewDerivedLocationsSignal()
	connect.AssertEqual(t, signal.Number(), "2.19c")
	connect.AssertEqual(t, signal.Key(), "derived-locations")
	connect.AssertEqual(t, signal.ID(), "pg/derived-locations")
	connect.AssertEqual(t, signal.Cadence(), 15*time.Minute)
}

// The queries are the section's, and select counts, shares and ages only:
// no id, no coordinate, no nonce or signature ever leaves the database.
func TestDerivedLocationsQueriesSelectOnlyAggregates(t *testing.T) {
	settings := DefaultDerivedLocationsSettings()
	table := strings.Join(strings.Fields(derivedLocationsTableQuery(settings)), " ")
	for _, want := range []string{
		"count(*) FILTER (WHERE crossed_region) AS crossed_region",
		"count(*) FILTER (WHERE crossed_country) AS crossed_country",
		"count(*) FILTER (WHERE peer_count <= 3) AS at_min_peers",
		"percentile_cont(0.5) WITHIN GROUP (ORDER BY residual_km)",
		"percentile_cont(0.5) WITHIN GROUP (ORDER BY reputation)",
		"max(update_time)",
		"update_time < (now() AT TIME ZONE 'utc') - interval '90000 seconds'",
		"FROM derived_location",
	} {
		if !strings.Contains(table, want) {
			t.Fatalf("table query lacks %q: %s", want, table)
		}
	}
	pings := strings.Join(strings.Fields(derivedLocationsPingsQuery(settings)), " ")
	for _, want := range []string{
		"COALESCE(sum(ping_count) FILTER (WHERE cosign = 1), 0)::bigint AS cosigned",
		"COALESCE(sum(ping_count) FILTER (WHERE cosign = 2), 0)::bigint AS rejected",
		"COALESCE(sum(ping_count) FILTER (WHERE cosign = 2 AND cosign_reason = 6), 0)::bigint AS rate_limited",
		"COALESCE(sum(ping_count) FILTER (WHERE cosign = 0), 0)::bigint AS unknown",
		"COALESCE(sum(ping_count) FILTER (WHERE relayed), 0)::bigint AS relayed",
		"sum(zero_rtt_count)::bigint AS zero_rtt",
		"sum(beyond_half_planet_count)::bigint AS beyond_half_planet",
		"FROM network_ping_hour_tally",
		"WHERE date_trunc('hour', now() AT TIME ZONE 'utc') - interval '23 hours' <= hour GROUP BY hour ORDER BY hour",
	} {
		if !strings.Contains(pings, want) {
			t.Fatalf("ping query lacks %q: %s", want, pings)
		}
	}
	// the day of pings is the ingest's hour tally, never a scan of the rows
	if strings.Contains(pings+" ", "FROM network_ping ") {
		t.Fatalf("ping query reads network_ping rows: %s", pings)
	}
	state := strings.Join(strings.Fields(derivedLocationsStateQuery(settings)), " ")
	for _, want := range []string{
		"function_name = 'github.com/urnetwork/server/taskworker/work.DeriveLocations'",
		"reschedule_error_count > 0",
		"FROM network_extender WHERE active LIMIT 1",
		"pk.provide_mode = 3",
	} {
		if !strings.Contains(state, want) {
			t.Fatalf("state query lacks %q: %s", want, state)
		}
	}
	// no row age on network_ping: a row lives until its whole day goes
	if strings.Contains(state, "network_ping") {
		t.Fatalf("state query reads network_ping: %s", state)
	}
	partitions := strings.Join(strings.Fields(derivedLocationsPartitionsQuery(settings)), " ")
	for _, want := range []string{
		"FROM pg_inherits AS inheritance",
		"pg_get_expr(partition_relation.relpartbound, partition_relation.oid)",
		"WHERE inheritance.inhparent = to_regclass('public.network_ping')",
		"isfinite(bounds[2]::timestamp)",
		// only the sweep's own partitions are its drops
		"name ~ '^network_ping_p[0-9]{8}$'",
		// the span the sweep keeps a ping -- the two clock skews a report is
		// accepted across, 24 h and 5 min, and a sweep interval -- and one
		// sweep interval more
		"upper_bound < (now() AT TIME ZONE 'utc') - interval '93900 seconds'",
		// tomorrow, the day before the last of the two days kept ahead
		"date_trunc('day', now() AT TIME ZONE 'utc') + interval '1 days' AS day",
		"lower_bound < checked_day.day + interval '1 day' AND checked_day.day < upper_bound",
		"relkind = 'p'",
	} {
		if !strings.Contains(partitions, want) {
			t.Fatalf("partition query lacks %q: %s", want, partitions)
		}
	}
	for _, query := range []string{table, pings, state, partitions} {
		for _, forbidden := range []string{"latitude", "longitude", "node_id", "pinger_id", "target_extender_id", "location_id", "task_id", "probe_nonce", "signature", "args_json", "reschedule_error,"} {
			if strings.Contains(query, forbidden) {
				t.Fatalf("query selects %q: %s", forbidden, query)
			}
		}
	}
}

// A malformed database row is a probe failure, never a green observation.
func TestDerivedLocationsSignalRejectsMalformedRows(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
		rows  []Row
	}{
		{name: "table shape", query: "derived-locations-table", rows: []Row{{"1", "2"}}},
		{name: "table value", query: "derived-locations-table", rows: []Row{{"x", "0", "0", "0", "-1", "-1", "-1", "0"}}},
		{name: "ping shape", query: "derived-locations-pings", rows: []Row{{"2026-08-29 11:00:00", "t", "1"}}},
		{name: "two last hours", query: "derived-locations-pings", rows: []Row{
			derivedLocationsHourRow(derivedLocationsPingHour{cosigned: 1}, "2026-08-29 10:00:00", true),
			derivedLocationsHourRow(derivedLocationsPingHour{cosigned: 1}, "2026-08-29 11:00:00", true),
		}},
		{name: "state shape", query: "derived-locations-state", rows: []Row{}},
		{name: "partition shape", query: "derived-locations-partitions", rows: []Row{{"t", "4"}}},
		{name: "partition value", query: "derived-locations-partitions", rows: []Row{{"t", "4", "x", "-1", "t"}}},
	} {
		fixture := healthyDerivedLocationsFixture()
		source := fixture.source(t)
		healthy := source.postgresFn
		source.postgresFn = func(query string) ([]Row, error) {
			if strings.Contains(query, "monitor-signal-2.19c-"+test.query+" */") {
				return test.rows, nil
			}
			return healthy(query)
		}
		if _, err := NewDerivedLocationsSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
			t.Errorf("%s: a malformed row produced an observation", test.name)
		}
	}
}

// A source that runs the signal's queries against the test's own database and its history reads against the test's own Redis, so
// the production SQL and the job's record shape are what is exercised. Values
// are rendered as psql's CSV output renders them.
type derivedLocationsDatabaseSource struct {
	syntheticSource
}

// Implements the source's PostgreSQL read, against the test's database.
func (self *derivedLocationsDatabaseSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	rows := []Row{}
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, query)
		if err != nil {
			queryErr = err
			return
		}
		defer result.Close()
		for result.Next() {
			values, err := result.Values()
			if err != nil {
				queryErr = err
				return
			}
			row := make(Row, len(values))
			for i, value := range values {
				switch v := value.(type) {
				case nil:
					row[i] = ""
				case bool:
					if v {
						row[i] = "t"
					} else {
						row[i] = "f"
					}
				case float64:
					row[i] = strconv.FormatFloat(v, 'f', -1, 64)
				case time.Time:
					row[i] = v.Format("2006-01-02 15:04:05")
				default:
					row[i] = fmt.Sprint(v)
				}
			}
			rows = append(rows, row)
		}
		queryErr = result.Err()
	})
	return rows, queryErr
}

// Implements the source's Redis read, for the history's range read only,
// against the test's Redis.
func (self *derivedLocationsDatabaseSource) Redis(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
	if len(args) != 6 || args[2] != "LRANGE" {
		return "", fmt.Errorf("unexpected redis command %v", args)
	}
	start, _ := strconv.ParseInt(args[4], 10, 64)
	stop, _ := strconv.ParseInt(args[5], 10, 64)
	var values []string
	var readErr error
	server.Redis(ctx, func(client server.RedisClient) {
		values, readErr = client.LRange(ctx, args[3], start, stop).Result()
	})
	if readErr != nil {
		return "", readErr
	}
	return strings.Join(values, "\n") + "\n", nil
}

// The partition checks against the conversion migration's own partitions:
// yesterday, today and two days ahead is the healthy shape; a partition of the
// sweep's naming four days old is past the drop cut on any clock; and with
// tomorrow's partition gone (and the day after, so a midnight between the
// drop and the read changes nothing) creation is overdue. Each shape on its
// own database, the partitions as the catalog has them.
func TestDerivedLocationsSignalPartitionsOnADatabase(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("derived-location query fixtures require the attested local test environment")
	}
	partitionName := func(day time.Time) string {
		return "network_ping_p" + day.Format("20060102")
	}
	for _, test := range []struct {
		name    string
		prepare func(ctx context.Context, today time.Time)
		want    []string
	}{
		{
			name:    "the migration's partitions",
			prepare: func(ctx context.Context, today time.Time) {},
			want:    []string{},
		},
		{
			name: "a day partition past its drop",
			prepare: func(ctx context.Context, today time.Time) {
				day := today.AddDate(0, 0, -4)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
						`CREATE TABLE %s PARTITION OF network_ping FOR VALUES FROM ('%s') TO ('%s')`,
						partitionName(day),
						day.Format("2006-01-02"),
						day.AddDate(0, 0, 1).Format("2006-01-02"),
					)))
				})
			},
			want: []string{"derive-sweep-stalled/partition-drop"},
		},
		{
			name: "no partition for tomorrow",
			prepare: func(ctx context.Context, today time.Time) {
				server.Tx(ctx, func(tx server.PgTx) {
					for _, day := range []time.Time{today.AddDate(0, 0, 1), today.AddDate(0, 0, 2)} {
						server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, partitionName(day))))
					}
				})
			},
			want: []string{"derive-sweep-stalled/partition-ahead"},
		},
	} {
		server.DefaultTestEnv().Run(t, func(tb testing.TB) {
			ctx := context.Background()
			now := server.NowUtc()
			test.prepare(ctx, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))

			settings := syntheticSettings(&derivedLocationsDatabaseSource{})
			settings.Now = func() time.Time { return now }
			alerts, err := NewDerivedLocationsSignal().Run(ctx, settings)
			if err != nil {
				tb.Fatal(err)
			}
			got := []string{}
			for _, alert := range alerts {
				if alert.Class == "derive-sweep-stalled" {
					got = append(got, alert.Class+"/"+alert.Frame)
				}
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				tb.Fatalf("%s: sweep alerts %v, want %v", test.name, got, test.want)
			}
			for _, alert := range alerts {
				if alert.Class == "derive-sweep-stalled" && alert.Frame == "partition-drop" && !strings.Contains(alert.Observed, "overdue_partitions=1 ") {
					tb.Fatalf("%s: observed %q", test.name, alert.Observed)
				}
				if alert.Class == "derive-sweep-stalled" && alert.Frame == "partition-ahead" && !strings.Contains(alert.Observed, "checked_day=today+1 covered=false") {
					tb.Fatalf("%s: observed %q", test.name, alert.Observed)
				}
			}
		})
	}
}

// The production queries and the job's record, end to end on a seeded
// database: published rows with crossings and one past the sweep, pings with
// refusals in the last complete hour and one past the sweep, no pending_task
// row, and a history whose last run published under half of the one before
// on the same supply and projects the next run near the solve's time budget.
// No seeded id or coordinate reaches an alert.
func TestDerivedLocationsSignalOnASeededDatabase(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("derived-location query fixtures require the attested local test environment")
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		forbidden := []string{}

		derivedLocations := []*model.DerivedLocation{}
		for i := 0; i < 30; i += 1 {
			nodeId := server.NewId()
			latitude := 12.3456789 + float64(i)/1000
			longitude := 45.6789123 + float64(i)/1000
			forbidden = append(forbidden, nodeId.String(), strconv.FormatFloat(latitude, 'f', -1, 64), strconv.FormatFloat(longitude, 'f', -1, 64))
			updateTime := now
			if i == 0 {
				// past the sweep's cut
				updateTime = now.Add(-26 * time.Hour)
			}
			derivedLocations = append(derivedLocations, &model.DerivedLocation{
				NodeKind:          model.DerivedLocationNodeKindProvider,
				NodeId:            nodeId,
				GenesisLatitude:   latitude,
				GenesisLongitude:  longitude,
				GenesisAccuracyKm: 25,
				Latitude:          latitude,
				Longitude:         longitude,
				PingCount:         12,
				PeerCount:         4,
				ResidualKm:        5,
				Reputation:        0.9,
				// four across a region line, three of them across a country
				CrossedRegion:     i < 4,
				CrossedCountry:    i < 3,
				LocationId:        server.NewId(),
				CityLocationId:    server.NewId(),
				RegionLocationId:  server.NewId(),
				CountryLocationId: server.NewId(),
				UpdateTime:        updateTime,
			})
		}
		written, _ := model.ReplaceDerivedLocations(ctx, derivedLocations)
		connect.AssertEqual(tb, written, 30)

		// the last complete hour of the database's clock
		var lastHour time.Time
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT date_trunc('hour', now() AT TIME ZONE 'utc') - interval '30 minutes'`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&lastHour))
				}
			})
		})
		pingerId := server.NewId()
		targetId := server.NewId()
		forbidden = append(forbidden, pingerId.String(), targetId.String())
		ping := func(cosign int, reason int, createTime time.Time) *model.NetworkPing {
			return &model.NetworkPing{
				PingerKind:       model.NetworkPingPingerKindExtender,
				PingerId:         pingerId,
				TargetExtenderId: targetId,
				ProbeNonce:       server.NewId().Bytes(),
				RttMs:            40,
				ProbeTime:        createTime,
				Cosign:           cosign,
				CosignReason:     reason,
				PingerSignature:  []byte("pinger-signature"),
				Cosignature:      []byte("cosignature"),
				CreateTime:       createTime,
			}
		}
		pings := []*model.NetworkPing{}
		for i := 0; i < 120; i += 1 {
			pings = append(pings, ping(model.NetworkPingCosignCosigned, 0, lastHour))
		}
		for i := 0; i < 30; i += 1 {
			pings = append(pings, ping(model.NetworkPingCosignRejected, int(connect.ExtenderProbeVerdictReasonRttBelowObserved), lastHour))
		}
		// Past the retention but not past the sweep: the sweep drops a day
		// partition whole once its upper bound is past the span it keeps a
		// ping, so a ping 26 hours old may still be there on a healthy sweep.
		// A ping four days old is in a partition past that on any clock, and
		// the insert creates the partition it needs.
		pings = append(pings, ping(model.NetworkPingCosignCosigned, 0, now.Add(-26*time.Hour)))
		pings = append(pings, ping(model.NetworkPingCosignCosigned, 0, now.Add(-4*24*time.Hour)))
		connect.AssertEqual(tb, model.AddNetworkPings(ctx, pings), len(pings))

		for _, run := range []*model.DeriveLocationsRun{
			{RunTime: now.Add(-9 * time.Hour), Nodes: 40, Sources: 40, Terms: 400, CosignedPings: 1000, Published: 30, ResidualKm: 5, GenesisResidualKm: 40, Converged: true, LastRoundSweeps: 9, SweepCap: 100},
			{RunTime: now.Add(-time.Hour), Nodes: 40, Sources: 40, Terms: 400, CosignedPings: 1000, Published: 10, ResidualKm: 5, GenesisResidualKm: 40, Converged: true, LastRoundSweeps: 8, SweepCap: 100, Sweeps: 20, Cores: 4, ProjectedSeconds: 500, ProjectedBytes: 1024 * 1024, MaxSolveSeconds: 600, MaxSolveBytes: 8 * 1024 * 1024 * 1024},
		} {
			model.AddDeriveLocationsRun(ctx, run)
		}

		settings := syntheticSettings(&derivedLocationsDatabaseSource{})
		settings.Now = func() time.Time { return now }
		alerts, err := NewDerivedLocationsSignal().Run(ctx, settings)
		if err != nil {
			tb.Fatal(err)
		}
		got := derivedLocationsAlertKeys(alerts)
		want := []string{
			"derive-not-running/task-missing",
			"derive-refusals/rejected",
			"derive-published-collapse/",
			"derive-crossings/country",
			"derive-crossings/region",
			"derive-capacity/seconds",
			"derive-sweep-stalled/partition-drop",
			"derive-sweep-stalled/derived_location",
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			tb.Fatalf("alerts %v, want %v", got, want)
		}
		for _, alert := range alerts {
			requireAlertOmits(tb, alert, forbidden...)
		}
		for _, alert := range alerts {
			if alert.Class == "derive-refusals" && !strings.Contains(alert.Observed, "rejected_last_hour=30 verdicts_last_hour=150") {
				tb.Fatalf("refusals observed %q", alert.Observed)
			}
			if alert.Class == "derive-crossings" && alert.Frame == "country" && !strings.Contains(alert.Observed, "crossed_country=3 published=30") {
				tb.Fatalf("crossings observed %q", alert.Observed)
			}
			if alert.Class == "derive-sweep-stalled" && alert.Frame == "partition-drop" && !(strings.Contains(alert.Observed, "overdue_partitions=1 ") && strings.Contains(alert.Observed, "threshold=PingPartitionKeepTimeout+DeriveSweepGrace=93900s")) {
				tb.Fatalf("ping sweep observed %q", alert.Observed)
			}
			if alert.Class == "derive-capacity" && !strings.Contains(alert.Observed, "projected=500s budget=600s share=0.8333") {
				tb.Fatalf("capacity observed %q", alert.Observed)
			}
		}
	})
}
