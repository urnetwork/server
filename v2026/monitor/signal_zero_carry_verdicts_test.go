package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var zeroCarryVerdictsNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

type zeroCarryVerdictsFixture struct {
	// verdicts made in the trailing hour, summed over Taskworkers
	created float64
	// the newest judge run's age at the evaluation clock
	judgedAge time.Duration
	// a run time this far past the evaluation clock (another host's clock)
	judgedAhead time.Duration
	// no Taskworker exports the judge's series: a fleet built before them
	noSeries bool
	// the judge's last-run series without its changes series
	noCreated bool
	mimirErr  error
	// the queries the fleet read sent
	queries []string
}

// A judge that ran a minute ago and made three verdicts in the last hour.
func healthyZeroCarryVerdictsFixture() *zeroCarryVerdictsFixture {
	return &zeroCarryVerdictsFixture{created: 3, judgedAge: time.Minute}
}

func (self *zeroCarryVerdictsFixture) settings(t *testing.T) SignalSettings {
	t.Helper()
	at := fleetEvaluationTime(zeroCarryVerdictsNow, DefaultZeroCarryVerdictsSettings().EvaluationLag)
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatalf("the fleet read contacted a non-gateway host")
			}
			self.queries = append(self.queries, command)
			if self.mimirErr != nil {
				return "", self.mimirErr
			}
			fixture := newFleetFixture(at)
			if !self.noSeries {
				judgedAt := float64(at.Add(-self.judgedAge).Unix())
				if 0 < self.judgedAhead {
					judgedAt = float64(at.Add(self.judgedAhead).Unix())
				}
				fixture.scalar("judged", judgedAt)
				if !self.noCreated {
					fixture.scalar("created", self.created)
				}
			}
			return fixture.json(t), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return zeroCarryVerdictsNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func (self *zeroCarryVerdictsFixture) signal() Signal {
	return NewZeroCarryVerdictsSignal()
}

// The alerts of each cadence over ticks consecutive cadences, as the
// scheduler's cadence gate releases them, none naming a provider.
func (self *zeroCarryVerdictsFixture) gated(t *testing.T, ticks int) []Alerts {
	t.Helper()
	signal := self.signal()
	gate := newCadenceAlertGate()
	released := []Alerts{}
	for range ticks {
		alerts, err := signal.Run(context.Background(), self.settings(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "client_id")
		}
		released = append(released, gate.filter(signal.(*signalAdapter), alerts))
	}
	return released
}

// The healthy classes one run reports.
func (self *zeroCarryVerdictsFixture) healthy(t *testing.T) map[string]bool {
	t.Helper()
	env, err := newProbeEnv(self.settings(t).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := self.signal().(*signalAdapter).probe.check(context.Background(), env)
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

func TestZeroCarryVerdictsRegisteredAndClassified(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "zero-carry-verdicts" {
			found++
			if signal.Number() != "2.32" || signal.Cadence() != 5*time.Minute {
				t.Fatalf("registered as §%s every %s", signal.Number(), signal.Cadence())
			}
		}
	}
	if found != 1 {
		t.Fatalf("zero-carry-verdicts registered %d times", found)
	}
	for class, kind := range map[string]alertClassKind{
		"zero-carry-verdict-spike":         incidentAlertClass,
		"zero-carry-verdicts-unobservable": visibilityAlertClass,
	} {
		if got, ok := alertClassKinds[class]; !ok || got != kind {
			t.Errorf("%s is classified %v", class, got)
		}
	}
}

// The read sums the verdicts made over the trailing hour across Taskworker
// processes, reset-aware, beside the newest run.
func TestZeroCarryVerdictsQuery(t *testing.T) {
	query := zeroCarryVerdictsQuery("main", DefaultZeroCarryVerdictsSettings())
	for _, part := range []string{
		`sum(increase(urnetwork_provider_zero_carry_changes_total{env="main",job="taskworker",change="created"}[3600s]))`,
		`max(urnetwork_provider_zero_carry_judge_last_run_timestamp_seconds{env="main",job="taskworker"})`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s", part)
		}
	}
}

// A few verdicts an hour raise nothing and resolve the spike class.
func TestZeroCarryVerdictsHealthy(t *testing.T) {
	fixture := healthyZeroCarryVerdictsFixture()
	for _, released := range fixture.gated(t, 3) {
		if len(released) != 0 {
			t.Fatalf("a quiet judge alerted: %+v", released)
		}
	}
	if !fixture.healthy(t)["zero-carry-verdict-spike"] {
		t.Fatal("the spike class did not resolve healthy")
	}
	// just under the bound
	fixture.created = 99
	for _, released := range fixture.gated(t, 3) {
		if len(released) != 0 {
			t.Fatalf("99 verdicts an hour alerted: %+v", released)
		}
	}
}

// A hundred verdicts or more in an hour warn from the second cadence, and
// never page.
func TestZeroCarryVerdictsSpikeWarns(t *testing.T) {
	fixture := healthyZeroCarryVerdictsFixture()
	fixture.created = 150
	for tick, released := range fixture.gated(t, 8) {
		cadence := tick + 1
		if cadence < 2 {
			if len(released) != 0 {
				t.Fatalf("cadence %d released %+v before the sustain", cadence, released)
			}
			continue
		}
		if len(released) != 1 || released[0].Class != "zero-carry-verdict-spike" || released[0].Severity != SeverityWarn {
			t.Fatalf("cadence %d released %+v, want one spike warning", cadence, released)
		}
		if !strings.Contains(released[0].Observed, "verdicts_made=150") || !strings.Contains(released[0].Symptom, "150 verdicts") {
			t.Fatalf("cadence %d observed %q", cadence, released[0].Observed)
		}
	}
	if fixture.healthy(t)["zero-carry-verdict-spike"] {
		t.Fatal("a spike resolved as healthy")
	}
}

// A failed read, a fleet without the judge's series, a missing changes
// series and a run time ahead of the clock are unknown states, never an
// incident or its clear.
func TestZeroCarryVerdictsUnobservable(t *testing.T) {
	for name, fixture := range map[string]*zeroCarryVerdictsFixture{
		"read failed":    {mimirErr: errors.New("connection refused"), created: 3, judgedAge: time.Minute},
		"no series":      {noSeries: true},
		"no changes":     {noCreated: true, judgedAge: time.Minute},
		"run time ahead": {created: 3, judgedAhead: 10 * time.Minute},
	} {
		released := fixture.gated(t, 2)
		if len(released[1]) != 1 || released[1][0].Class != "zero-carry-verdicts-unobservable" {
			t.Fatalf("%s: released %+v, want one unobservable", name, released[1])
		}
		if fixture.healthy(t)["zero-carry-verdict-spike"] && name != "run time ahead" {
			t.Fatalf("%s: the spike class resolved healthy on an unknown state", name)
		}
	}
}
