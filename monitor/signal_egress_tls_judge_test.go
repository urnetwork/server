package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var egressTlsJudgeNow = time.Date(2026, 11, 7, 18, 30, 0, 0, time.UTC)

var egressTlsJudgeSlots = []fleetSlot{
	{service: "taskworker", host: "worker-a.example", block: "g1", allowed: true},
	{service: "taskworker", host: "worker-b.example", block: "g1", allowed: true},
}

type egressTlsJudgeFixture struct {
	// the newest judged run's age at the evaluation clock, on judgedHost
	judgedAge  time.Duration
	judgedHost string
	// every process exports 0: none has judged since it started
	neverJudged bool
	// no process exports the judge gauges: a fleet built before them
	noSeries bool
	// a judged time this far past the evaluation clock (another host's clock)
	judgedAhead time.Duration
	undecided   float64
	// how long ago the newest process exporting the gauges started; and none
	// exports its start time
	startedAge time.Duration
	noStarted  bool
	// the hosts whose last-judged gauge is scraped fresh
	fresh    map[string]bool
	mimirErr error
}

// Two Taskworkers on the build that exports the gauges, the newest judged run
// three minutes old, and nothing left undecided.
func healthyEgressTlsJudgeFixture() *egressTlsJudgeFixture {
	fixture := &egressTlsJudgeFixture{judgedAge: 3 * time.Minute, judgedHost: "worker-b.example", startedAge: 4 * time.Minute, fresh: map[string]bool{}}
	for _, slot := range egressTlsJudgeSlots {
		fixture.fresh[slot.host] = true
	}
	return fixture
}

func (self *egressTlsJudgeFixture) settings(t *testing.T) SignalSettings {
	t.Helper()
	at := fleetEvaluationTime(egressTlsJudgeNow, DefaultEgressTlsJudgeSettings().EvaluationLag)
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatalf("the fleet read contacted a non-gateway host")
			}
			if self.mimirErr != nil {
				return "", self.mimirErr
			}
			fixture := newFleetFixture(at)
			if !self.noSeries {
				judgedAt := float64(at.Add(-self.judgedAge).Unix())
				switch {
				case self.neverJudged:
					judgedAt = 0
				case 0 < self.judgedAhead:
					judgedAt = float64(at.Add(self.judgedAhead).Unix())
				}
				labels := map[string]string{"job": "taskworker", "host": self.judgedHost, "block": "g1", "instance": self.judgedHost + ":7100"}
				fixture.labeled("judged", labels, judgedAt)
				fixture.labeled("undecided", labels, self.undecided)
				if !self.noStarted {
					fixture.labeled("started", map[string]string{}, float64(at.Add(-self.startedAge).Unix()))
				}
				for _, slot := range egressTlsJudgeSlots {
					if self.fresh[slot.host] {
						fixture.fresh("judged", slot, at.Add(-20*time.Second))
					}
				}
			}
			return fixture.json(t), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return egressTlsJudgeNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	hosts := []string{}
	for _, slot := range egressTlsJudgeSlots {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: slot.host, Roles: []string{"edge"}})
		hosts = append(hosts, slot.host)
	}
	settings.LogServices = []string{"taskworker"}
	settings.LogServiceHosts = map[string][]string{"taskworker": hosts}
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"g1"}}
	return settings
}

func (self *egressTlsJudgeFixture) signal() *signalAdapter {
	return &signalAdapter{
		number: "2.31", key: "egress-tls-judge", name: "TLS finding judge liveness and undecided findings",
		probe: egressTlsJudgeProbe{settings: DefaultEgressTlsJudgeSettings()},
	}
}

// The alerts of one run, which never render a URL.
func (self *egressTlsJudgeFixture) run(t *testing.T) Alerts {
	t.Helper()
	alerts, err := self.signal().Run(context.Background(), self.settings(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "https://")
	}
	return alerts
}

// The alerts of each cadence over ticks consecutive cadences, as the
// scheduler's cadence gate releases them.
func (self *egressTlsJudgeFixture) gated(t *testing.T, ticks int) []Alerts {
	t.Helper()
	signal := self.signal()
	gate := newCadenceAlertGate()
	released := []Alerts{}
	for range ticks {
		alerts, err := signal.Run(context.Background(), self.settings(t))
		if err != nil {
			t.Fatal(err)
		}
		released = append(released, gate.filter(signal, alerts))
	}
	return released
}

// The healthy classes one run reports.
func (self *egressTlsJudgeFixture) healthy(t *testing.T) map[string]bool {
	t.Helper()
	env, err := newProbeEnv(self.settings(t).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := self.signal().probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	healthy := map[string]bool{}
	for _, finding := range findings {
		if finding.healthy {
			healthy[finding.class] = true
		}
	}
	return healthy
}

func egressTlsJudgeReleasedClasses(released Alerts) map[string]Severity {
	classes := map[string]Severity{}
	for _, alert := range released {
		classes[alert.Class] = alert.Severity
	}
	return classes
}

func TestEgressTlsJudgeRegisteredAndClassified(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "egress-tls-judge" {
			found++
			if signal.Number() != "2.31" || signal.Cadence() != 5*time.Minute {
				t.Fatalf("registered as §%s every %s", signal.Number(), signal.Cadence())
			}
		}
	}
	if found != 1 {
		t.Fatalf("egress-tls-judge registered %d times", found)
	}
	for class, kind := range map[string]alertClassKind{
		"egress-tls-judge-stale":        incidentAlertClass,
		"egress-tls-findings-undecided": incidentAlertClass,
		"egress-tls-judge-unobservable": visibilityAlertClass,
	} {
		if alertClassKinds[class] != kind {
			t.Errorf("%s is classified %v", class, alertClassKinds[class])
		}
	}
}

// A judge that judged three minutes ago and left nothing undecided raises
// nothing, and resolves both incident classes.
func TestEgressTlsJudgeHealthy(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	if alerts := fixture.run(t); len(alerts) != 0 {
		t.Fatalf("a healthy judge alerted: %+v", alerts)
	}
	healthy := fixture.healthy(t)
	if !healthy["egress-tls-judge-stale"] || !healthy["egress-tls-findings-undecided"] {
		t.Fatalf("healthy classes = %v, want both incident classes resolved", healthy)
	}
	// nineteen minutes is still inside the bound
	fixture.judgedAge = 19 * time.Minute
	requireNoAlertClass(t, fixture.run(t), "egress-tls-judge-stale")
}

// No judged run in twenty minutes: every new finding stays pending and
// excludes nobody. It warns from its second cadence and pages from its sixth.
func TestEgressTlsJudgeStaleWarnsThenPages(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.judgedAge = 25 * time.Minute
	for tick, released := range fixture.gated(t, 8) {
		cadence := tick + 1
		classes := egressTlsJudgeReleasedClasses(released)
		want := SeverityWarn
		switch {
		case cadence < 2:
			if len(released) != 0 {
				t.Fatalf("cadence %d released %+v before the sustain", cadence, released)
			}
			continue
		case 6 <= cadence:
			want = SeverityPage
		}
		if len(released) != 1 || classes["egress-tls-judge-stale"] != want {
			t.Fatalf("cadence %d released %+v, want one stale %s", cadence, released, want)
		}
		if !strings.Contains(released[0].Observed, "newest_judged_age=25m0s") || !strings.Contains(released[0].Observed, "judged_slot=taskworker/worker-b.example/g1") {
			t.Fatalf("cadence %d observed %q", cadence, released[0].Observed)
		}
	}
	if fixture.healthy(t)["egress-tls-judge-stale"] {
		t.Fatal("a stale judge resolved as healthy")
	}
}

// Every process on the new build exports 0 until it judges: a fleet where
// none has is stale too, once it holds.
func TestEgressTlsJudgeNeverJudgedIsStale(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.neverJudged = true
	// a count no judged run set says nothing
	fixture.undecided = 3
	alerts := fixture.run(t)
	alert := requireAlertClass(t, alerts, "egress-tls-judge-stale")
	if !strings.Contains(alert.Symptom, "since it started") || !strings.Contains(alert.Observed, "newest_judged_run=none") {
		t.Fatalf("never-judged alert = %+v", alert)
	}
	requireNoAlertClass(t, alerts, "egress-tls-findings-undecided")
}

// A pending finding the newest judged run left undecided, although a direct
// check could still decide it, warns once every run has for half an hour,
// and never pages.
func TestEgressTlsJudgeUndecidedFindingsWarnAfterHalfAnHour(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.undecided = 2
	for tick, released := range fixture.gated(t, 9) {
		cadence := tick + 1
		classes := egressTlsJudgeReleasedClasses(released)
		if cadence < 6 {
			if len(released) != 0 {
				t.Fatalf("cadence %d released %+v before half an hour", cadence, released)
			}
			continue
		}
		if len(released) != 1 || classes["egress-tls-findings-undecided"] != SeverityWarn {
			t.Fatalf("cadence %d released %+v, want one undecided warning", cadence, released)
		}
		if !strings.Contains(released[0].Observed, "undecided_findings=2") {
			t.Fatalf("cadence %d observed %q", cadence, released[0].Observed)
		}
	}
	if fixture.healthy(t)["egress-tls-findings-undecided"] {
		t.Fatal("undecided findings resolved as healthy")
	}
	// a stale judge's count says nothing: the stale class owns it
	fixture.judgedAge = time.Hour
	alerts := fixture.run(t)
	requireAlertClass(t, alerts, "egress-tls-judge-stale")
	requireNoAlertClass(t, alerts, "egress-tls-findings-undecided")
}

// Before the roll no Taskworker exports the gauges: the state is unknown,
// never stale, and over hours of cadences the visibility warning never pages.
func TestEgressTlsJudgeBeforeTheRollIsUnobservableAndNeverPages(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.noSeries = true
	alerts := fixture.run(t)
	visibility := requireAlertClass(t, alerts, "egress-tls-judge-unobservable")
	if !strings.Contains(visibility.Observed, "judge_series_missing") || !strings.Contains(visibility.Observed, "slot_coverage_incomplete") {
		t.Fatalf("visibility alert = %q", visibility.Observed)
	}
	requireNoAlertClass(t, alerts, "egress-tls-judge-stale")
	requireNoAlertClass(t, alerts, "egress-tls-findings-undecided")
	if healthy := fixture.healthy(t); len(healthy) != 0 {
		t.Fatalf("an unexported judge resolved %v", healthy)
	}
	for tick, released := range fixture.gated(t, 24) {
		for _, alert := range released {
			if alert.Class != "egress-tls-judge-unobservable" || alert.Severity == SeverityPage {
				t.Fatalf("cadence %d before the roll released %s %s", tick+1, alert.Class, alert.Severity)
			}
		}
	}
}

// Mid-roll, a slot still on the old build exports no gauge: a newest run 40
// minutes old cannot prove the judge stopped, since the unread slot may hold
// a newer one, so it is unobservable and not stale, and pages nobody; an
// undecided finding the newest read run reports is still raised. A recent
// judged run proves the judge runs, so stale resolves healthy even mid-roll,
// while undecided, which an unread slot's newer run might contradict, does
// not.
func TestEgressTlsJudgeMidRollIsUnobservable(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.fresh["worker-a.example"] = false
	fixture.judgedAge = 40 * time.Minute
	alerts := fixture.run(t)
	visibility := requireAlertClass(t, alerts, "egress-tls-judge-unobservable")
	if !strings.Contains(visibility.Observed, "slot_coverage_incomplete") || !strings.Contains(visibility.Observed, "worker-a.example") {
		t.Fatalf("visibility alert = %q", visibility.Observed)
	}
	requireNoAlertClass(t, alerts, "egress-tls-judge-stale")
	for tick, released := range fixture.gated(t, 12) {
		for _, alert := range released {
			if alert.Severity == SeverityPage {
				t.Fatalf("cadence %d mid-roll paged %s", tick+1, alert.Class)
			}
		}
	}

	fixture.judgedAge = 3 * time.Minute
	fixture.undecided = 1
	alerts = fixture.run(t)
	requireAlertClass(t, alerts, "egress-tls-findings-undecided")
	requireAlertClass(t, alerts, "egress-tls-judge-unobservable")
	if healthy := fixture.healthy(t); !healthy["egress-tls-judge-stale"] || healthy["egress-tls-findings-undecided"] {
		t.Fatalf("mid-roll with a recent judged run resolved %v, want stale alone", healthy)
	}
}

// A stopped judge pages even while a desired slot is not reporting (REVIEW-D1
// N11): once the newest judged run on the slots that report is older than a
// fully read judge would have paged at, stale warns and pages from its second
// cadence, beside the unobservable visibility alert. It resolves healthy as
// soon as a judged run is recent again.
func TestEgressTlsJudgeStalePagesWithASlotMissing(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.fresh["worker-a.example"] = false
	fixture.judgedAge = 50 * time.Minute
	for tick, released := range fixture.gated(t, 4) {
		cadence := tick + 1
		classes := egressTlsJudgeReleasedClasses(released)
		severity, stale := classes["egress-tls-judge-stale"]
		switch {
		case cadence < 2 && stale:
			t.Fatalf("cadence %d released stale before its sustain", cadence)
		case 2 <= cadence && (!stale || severity != SeverityPage):
			t.Fatalf("cadence %d released %+v, want stale paging with a slot missing", cadence, released)
		}
	}
	alerts := fixture.run(t)
	requireAlertClass(t, alerts, "egress-tls-judge-unobservable")
	if alert := requireAlertClass(t, alerts, "egress-tls-judge-stale"); !strings.Contains(alert.Symptom, "on any Taskworker slot that reports") {
		t.Fatalf("partial-coverage stale symptom = %q", alert.Symptom)
	}

	fixture.judgedAge = 2 * time.Minute
	if !fixture.healthy(t)["egress-tls-judge-stale"] {
		t.Fatal("a recent judged run did not resolve the stale alert while a slot is missing")
	}
}

// The roll stays quiet. With a slot not reporting, new processes that have not
// judged since they started (every reporting slot at zero), or a newest judged
// run older than the warning bound but short of the page bound, raise no
// stale alert, and nothing pages over hours of cadences.
func TestEgressTlsJudgeRollWithASlotMissingStaysQuiet(t *testing.T) {
	for name, change := range map[string]func(*egressTlsJudgeFixture){
		"new processes not yet judged": func(fixture *egressTlsJudgeFixture) { fixture.neverJudged = true },
		"short of the page bound":      func(fixture *egressTlsJudgeFixture) { fixture.judgedAge = 44 * time.Minute },
	} {
		fixture := healthyEgressTlsJudgeFixture()
		fixture.fresh["worker-a.example"] = false
		change(fixture)
		requireNoAlertClass(t, fixture.run(t), "egress-tls-judge-stale")
		for tick, released := range fixture.gated(t, 24) {
			for _, alert := range released {
				if alert.Severity == SeverityPage || alert.Class == "egress-tls-judge-stale" {
					t.Fatalf("%s: cadence %d released %s %s", name, tick+1, alert.Class, alert.Severity)
				}
			}
		}
	}
}

// An unreadable Mimir path, or a judged time ahead of the clock, is unknown,
// never healthy.
func TestEgressTlsJudgeUnreadableSourcesAreUnobservable(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.mimirErr = errors.New("synthetic gateway failure")
	alerts := fixture.run(t)
	visibility := requireAlertClass(t, alerts, "egress-tls-judge-unobservable")
	if !strings.Contains(visibility.Observed, "fleet_unavailable") {
		t.Fatalf("visibility alert = %q", visibility.Observed)
	}
	requireNoAlertClass(t, alerts, "egress-tls-judge-stale")
	if healthy := fixture.healthy(t); len(healthy) != 0 {
		t.Fatalf("an unread fleet resolved %v", healthy)
	}

	fixture = healthyEgressTlsJudgeFixture()
	fixture.judgedAhead = 5 * time.Minute
	visibility = requireAlertClass(t, fixture.run(t), "egress-tls-judge-unobservable")
	if !strings.Contains(visibility.Observed, "invalid_fleet_sample") {
		t.Fatalf("visibility alert = %q", visibility.Observed)
	}
}

// The read asks for the newest judged run across the env's Taskworkers, that
// process's undecided count, and the gauge's freshness on each desired slot.
func TestEgressTlsJudgeQueryReadsTheNewestJudgedRun(t *testing.T) {
	inventory := fleetSlotInventory{slots: egressTlsJudgeSlots}
	query := egressTlsJudgeQuery("main", DefaultEgressTlsJudgeSettings(), inventory)
	for _, want := range []string{
		`topk(1, urnetwork_egress_tls_judge_last_judged_timestamp_seconds{env="main",job="taskworker"})`,
		`urnetwork_egress_tls_undecided_findings{env="main",job="taskworker"} and on(job,host,block,instance) topk(1, `,
		`max by (job,host,block) (timestamp(urnetwork_egress_tls_judge_last_judged_timestamp_seconds{env="main",job=~"taskworker"`,
		`"monitor_part","judged"`, `"monitor_part","undecided"`, `"monitor_part","fresh/judged"`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query lacks %s:\n%s", want, query)
		}
	}
}

// A judge that never succeeds pages even with a slot missing (REVIEW-D1 F3):
// every slot that reports shows 0, no run since its process started, and the
// newest of those processes started 50 minutes ago, past the page bound. The
// judge is one fleet task, run every five minutes by whichever process claims
// it, so the processes that report stay at 0 that long only if none of them
// has run it: stale pages from its second cadence beside the unobservable
// alert.
func TestEgressTlsJudgeNeverJudgedWithASlotMissingPagesOnceItsProcessesAreOld(t *testing.T) {
	fixture := healthyEgressTlsJudgeFixture()
	fixture.fresh["worker-a.example"] = false
	fixture.neverJudged = true
	fixture.startedAge = 50 * time.Minute
	for tick, released := range fixture.gated(t, 4) {
		cadence := tick + 1
		severity, stale := egressTlsJudgeReleasedClasses(released)["egress-tls-judge-stale"]
		switch {
		case cadence < 2 && stale:
			t.Fatalf("cadence %d released stale before its sustain", cadence)
		case 2 <= cadence && (!stale || severity != SeverityPage):
			t.Fatalf("cadence %d released %+v, want stale paging for a fleet that never judged", cadence, released)
		}
	}
	alerts := fixture.run(t)
	requireAlertClass(t, alerts, "egress-tls-judge-unobservable")
	if alert := requireAlertClass(t, alerts, "egress-tls-judge-stale"); !strings.Contains(alert.Symptom, "since it started") ||
		!strings.Contains(alert.Observed, "newest_process_start_age=50m") {
		t.Fatalf("never-judged stale symptom %q observed %q", alert.Symptom, alert.Observed)
	}
}

// The roll stays quiet: with a slot missing and every reporting process at 0,
// processes that started minutes ago, or a newest process short of the page
// bound, or no start time to read, raise no stale alert and page nobody.
func TestEgressTlsJudgeNeverJudgedRollStaysQuiet(t *testing.T) {
	for name, change := range map[string]func(*egressTlsJudgeFixture){
		"processes started minutes ago":     func(fixture *egressTlsJudgeFixture) { fixture.startedAge = 4 * time.Minute },
		"newest process short of the bound": func(fixture *egressTlsJudgeFixture) { fixture.startedAge = 44 * time.Minute },
		"no start time to read":             func(fixture *egressTlsJudgeFixture) { fixture.startedAge, fixture.noStarted = 50*time.Minute, true },
	} {
		fixture := healthyEgressTlsJudgeFixture()
		fixture.fresh["worker-a.example"] = false
		fixture.neverJudged = true
		change(fixture)
		requireNoAlertClass(t, fixture.run(t), "egress-tls-judge-stale")
		for tick, released := range fixture.gated(t, 24) {
			for _, alert := range released {
				if alert.Severity == SeverityPage || alert.Class == "egress-tls-judge-stale" {
					t.Fatalf("%s: cadence %d released %s %s", name, tick+1, alert.Class, alert.Severity)
				}
			}
		}
	}
}

// With every slot reporting, a fleet that never judged is stale as before,
// paging after six cadences whatever its processes' age.
func TestEgressTlsJudgeNeverJudgedWithFullCoverageIsUnchanged(t *testing.T) {
	for _, startedAge := range []time.Duration{4 * time.Minute, 50 * time.Minute} {
		fixture := healthyEgressTlsJudgeFixture()
		fixture.neverJudged = true
		fixture.startedAge = startedAge
		for tick, released := range fixture.gated(t, 6) {
			cadence := tick + 1
			severity, stale := egressTlsJudgeReleasedClasses(released)["egress-tls-judge-stale"]
			switch {
			case cadence < 2 && stale, 2 <= cadence && !stale:
				t.Fatalf("started %s ago: cadence %d stale=%t", startedAge, cadence, stale)
			case cadence < 6 && severity == SeverityPage, cadence == 6 && severity != SeverityPage:
				t.Fatalf("started %s ago: cadence %d severity %s, want a page at the sixth", startedAge, cadence, severity)
			}
		}
	}
}
