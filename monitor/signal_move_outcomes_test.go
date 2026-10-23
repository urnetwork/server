package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The fleet-wide parts of one move-outcomes query, over a fleet whose every
// desired slot is current.
type moveOutcomesTestAnswer struct {
	scalars        map[string]float64
	sentByTarget   map[string]float64
	arrivedTarget  map[string]float64
	failedByReason map[[2]string]float64
	declined       map[string]float64
	dropped        map[string]float64
	expired        map[string]float64
	failureMin     map[string]float64
	failurePoints  map[string]float64
	breakers       map[[3]string][2]float64
	omitProcesses  int
}

// A fleet whose mover sends and mostly arrives.
func newMoveOutcomesTestAnswer() *moveOutcomesTestAnswer {
	return &moveOutcomesTestAnswer{
		scalars: map[string]float64{
			"mode_max": 2, "mode_series": 4, "sent": 600, "arrived": 420, "would_send": 0,
			"success_max": 0.7, "success_points": 24, "sends_points": 24, "slot_points": 24, "expected_success_points": 24,
			"expected_failure_points": 12, "expected_breaker_points": 10,
		},
		sentByTarget:   map[string]float64{"beta-edge-2.example": 600},
		arrivedTarget:  map[string]float64{"beta-edge-2.example": 420},
		failedByReason: map[[2]string]float64{{"connect_timeout", "beta-edge-2.example"}: 12},
		declined:       map[string]float64{"memory_budget": 60, "recent_move": 30, "unknown": 0},
		dropped:        map[string]float64{"no_intent": 4, "foreign_move": 0, "invalid_move_id": 0, "invalid_result": 0},
		expired:        map[string]float64{"advisory": 70, "compulsory": 0, "drain": 0},
		failureMin:     map[string]float64{"beta-edge-2.example": 0.02},
		failurePoints:  map[string]float64{"beta-edge-2.example": 12},
		breakers:       map[[3]string][2]float64{},
	}
}

func (self *moveOutcomesTestAnswer) json(t *testing.T) string {
	t.Helper()
	answer := newConnectMoveTestAnswer()
	answer.fleet()
	answer.rows = answer.rows[2*self.omitProcesses:]
	for tag, value := range self.scalars {
		answer.scalar(tag, value)
	}
	for hostName, value := range self.sentByTarget {
		answer.add("sent_by_target", map[string]string{"target_host": hostName}, value)
	}
	for hostName, value := range self.arrivedTarget {
		answer.add("arrived_by_target", map[string]string{"target_host": hostName}, value)
	}
	for key, value := range self.failedByReason {
		answer.add("failed_by_reason", map[string]string{"reason": key[0], "target_host": key[1]}, value)
	}
	for reason, value := range self.declined {
		answer.add("declined_by_reason", map[string]string{"reason": reason}, value)
	}
	for cause, value := range self.dropped {
		answer.add("dropped_by_cause", map[string]string{"cause": cause}, value)
	}
	for kind, value := range self.expired {
		answer.add("expired_by_kind", map[string]string{"kind": kind}, value)
	}
	for hostName, value := range self.failureMin {
		answer.add("failure_min", map[string]string{"target_host": hostName}, value)
	}
	for hostName, value := range self.failurePoints {
		answer.add("failure_points", map[string]string{"target_host": hostName}, value)
	}
	for key, value := range self.breakers {
		labels := map[string]string{"target_host": key[0], "interface": key[1], "family": key[2]}
		answer.add("breaker_held", labels, value[0])
		answer.add("breaker_held_points", labels, value[1])
	}
	return answer.json(t)
}

func moveOutcomesTestRun(t *testing.T, answer *moveOutcomesTestAnswer) Alerts {
	t.Helper()
	source := newConnectMoveTestSource()
	source.answers["success_max"] = answer.json(t)
	alerts, err := NewMoveOutcomesSignal().Run(context.Background(), connectMoveTestSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Clients decline advisory moves by design: a ratio below 1 is expected.
func TestMoveOutcomesRatioBelowOneButAboveTheFloorIsQuiet(t *testing.T) {
	alerts := moveOutcomesTestRun(t, newMoveOutcomesTestAnswer())
	if len(alerts) != 0 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
}

// SIGNALS.md §24.2's ticket: below 0.5 at every step of two hours, localized
// by target host, failure reasons, declines and dropped reports.
func TestMoveOutcomesSuccessBelowTheFloorForTheWholeSustainAlerts(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.scalars["success_max"] = 0.4
	answer.scalars["arrived"] = 200
	answer.arrivedTarget["beta-edge-2.example"] = 200
	alerts := moveOutcomesTestRun(t, answer)
	low := connectMoveTestAlert(t, alerts, "move-success-low")
	if low.Severity != SeverityWarn || low.Sustain != 1 || low.unobservable || low.Target != "synthetic/connect-moves" {
		t.Fatalf("not a ticket: %+v", low)
	}
	for _, want := range []string{"success_ratio_max=0.400", "arrived_over_sent_by_target=beta-edge-2.example:200/600", "failed_by_target_reason=beta-edge-2.example/connect_timeout:12", "declined_by_reason=memory_budget:60,recent_move:30", "dropped_by_cause=no_intent:4", "judged_steps=24"} {
		if !strings.Contains(low.Observed, want) {
			t.Fatalf("observed lacks %q: %s", want, low.Observed)
		}
	}
}

// False-positive qualifier: an unrelated reconnect that lands on the target
// consumes the intent and counts as an arrival; above 1 is not an error.
func TestMoveOutcomesArrivalsAboveSendsAreThePlacementWanted(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.scalars["success_max"] = 1.3
	answer.scalars["arrived"] = 780
	alerts := moveOutcomesTestRun(t, answer)
	if len(alerts) != 0 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
}

// No sends is not health: each reason the ratio has nothing to judge is its
// own standing state.
func TestMoveOutcomesWithoutSendsIsADormantStateNotHealth(t *testing.T) {
	for reason, mutate := range map[string]func(*moveOutcomesTestAnswer){
		"not_deployed": func(a *moveOutcomesTestAnswer) {
			delete(a.scalars, "mode_series")
			delete(a.scalars, "mode_max")
			a.scalars["sent"] = 0
		},
		"mover_off": func(a *moveOutcomesTestAnswer) {
			a.scalars["mode_max"] = 0
			a.scalars["sent"] = 0
		},
		// false-negative qualifier: dry run sends nothing, so its would-sends
		// have no arrivals to compare
		"dry_run": func(a *moveOutcomesTestAnswer) {
			a.scalars["mode_max"] = 1
			a.scalars["sent"] = 0
			a.scalars["would_send"] = 40
		},
		"no_sends": func(a *moveOutcomesTestAnswer) {
			a.scalars["sent"] = 0
		},
		"below_minimum_sends": func(a *moveOutcomesTestAnswer) {
			a.scalars["sends_points"] = 20
			a.scalars["success_points"] = 20
			a.scalars["success_max"] = 0.1
		},
	} {
		answer := newMoveOutcomesTestAnswer()
		mutate(answer)
		t.Logf("case %s", reason)
		alerts := moveOutcomesTestRun(t, answer)
		connectMoveTestNoClass(t, alerts, "move-success-low")
		dormant := connectMoveTestAlert(t, alerts, "move-outcomes-dormant")
		connectMoveTestStandingState(t, dormant)
		if !strings.Contains(dormant.Observed, "dormant_reason="+reason+" ") {
			t.Fatalf("observed=%s", dormant.Observed)
		}
	}
}

// A process that stops publishing removes its sends or arrivals from the sums,
// so a step without every slot leaves the ratio unknown.
func TestMoveOutcomesSlotGapAtAnyStepIsUnknown(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.scalars["slot_points"] = 22
	answer.scalars["success_points"] = 22
	answer.scalars["success_max"] = 0.1
	alerts := moveOutcomesTestRun(t, answer)
	connectMoveTestNoClass(t, alerts, "move-success-low")
	incomplete := connectMoveTestAlert(t, alerts, "move-outcomes-telemetry-incomplete")
	if !incomplete.unobservable || incomplete.configuredOff || !strings.Contains(incomplete.Observed, "slot_steps=22") {
		t.Fatalf("observed=%s", incomplete.Observed)
	}

	missing := newMoveOutcomesTestAnswer()
	missing.omitProcesses = 1
	alerts = moveOutcomesTestRun(t, missing)
	connectMoveTestAlert(t, alerts, "move-outcomes-telemetry-incomplete")
	connectMoveTestNoClass(t, alerts, "move-outcomes-dormant", "move-success-low", "move-target-failures")
}

// SIGNALS.md §24.6's ticket: failures above a quarter of a target host's sends
// at every step of an hour, per target host.
func TestMoveOutcomesTargetHostFailingAQuarterOfItsSendsForAnHourAlerts(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.sentByTarget["alpha-edge-1.example"] = 100
	answer.failureMin = map[string]float64{"beta-edge-2.example": 0.4, "alpha-edge-1.example": 0.1}
	answer.failurePoints = map[string]float64{"beta-edge-2.example": 12, "alpha-edge-1.example": 12}
	answer.failedByReason = map[[2]string]float64{{"dial_error", "beta-edge-2.example"}: 240}
	alerts := moveOutcomesTestRun(t, answer)
	failures := connectMoveTestAlert(t, alerts, "move-target-failures")
	if failures.Target != "synthetic/connect-moves/beta-edge-2.example" || failures.Playbook != "SIGNALS.md §24.6" ||
		!strings.Contains(failures.Observed, "failure_share_min=0.400") || !strings.Contains(failures.Observed, "failures=240") {
		t.Fatalf("target=%s observed=%s", failures.Target, failures.Observed)
	}
	for _, alert := range alerts {
		if alert.Class == "move-target-failures" && alert.Target != "synthetic/connect-moves/beta-edge-2.example" {
			t.Fatalf("a target within its share alerted: %s", alert.Target)
		}
	}
}

// A share above a quarter for only part of the hour, or a target the mover
// sent too few moves to judge, is not the ticket.
func TestMoveOutcomesTargetFailuresNeedEveryStepOfTheHour(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.failureMin["beta-edge-2.example"] = 0.6
	answer.failurePoints["beta-edge-2.example"] = 8
	alerts := moveOutcomesTestRun(t, answer)
	connectMoveTestNoClass(t, alerts, "move-target-failures")
}

// A drain's moves count their failures against the draining host with no kind
// label; the query takes every capable drain move of the host off its failures
// before dividing by the mover's sends to it.
func TestMoveOutcomesQueryTakesDrainMovesOffATargetsFailures(t *testing.T) {
	source := newConnectMoveTestSource()
	source.answers["success_max"] = newMoveOutcomesTestAnswer().json(t)
	if _, err := NewMoveOutcomesSignal().Run(context.Background(), connectMoveTestSettings(source)); err != nil {
		t.Fatal(err)
	}
	query := source.queries[0]
	for _, want := range []string{
		`clamp_min((sum by (target_host) (increase(urnetwork_connect_moves_failed_total{env="synthetic"}[3600s])) - on(target_host) sum by (target_host) (label_replace(increase(urnetwork_connect_drain_moves_sent_total{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green",capable="true"}[3600s]),"target_host","$1","host","(.*)"))) or sum by (target_host) (increase(urnetwork_connect_moves_failed_total{env="synthetic"}[3600s])), 0)`,
		`sum by (reason) (increase(urnetwork_connect_moves_declined_total{env="synthetic"}[3600s]))`,
		`sum by (cause) (increase(urnetwork_connect_move_results_dropped_total{env="synthetic"}[3600s]))`,
		`sum(increase(urnetwork_connect_moves_arrived_total{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green",kind=~"advisory|compulsory"}[3600s]))`,
		`[7200s:300s]`,
		`[3600s:300s]`,
		`[600s:60s]`,
		` >= 20)`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lacks %q", want)
		}
	}
	// the budget refused moves are in neither count
	if strings.Contains(query, "budget_refused") {
		t.Fatal("budget refusals entered the success ratio")
	}
}

// SIGNALS.md §24.6's breaker ticket: more than one address of a host held open.
func TestMoveOutcomesBreakersHeldOpenOnTwoAddressesOfAHostAlert(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.breakers = map[[3]string][2]float64{
		{"beta-edge-2.example", "eno1np0", "4"}:  {1, 10},
		{"beta-edge-2.example", "eno1np0", "6"}:  {1, 10},
		{"alpha-edge-1.example", "eno1np0", "4"}: {1, 10},
		{"alpha-edge-1.example", "eno2np1", "4"}: {0, 10},
	}
	alerts := moveOutcomesTestRun(t, answer)
	breakers := connectMoveTestAlert(t, alerts, "move-target-breakers-open")
	if breakers.Target != "synthetic/connect-moves/beta-edge-2.example" || !strings.Contains(breakers.Observed, "open_addresses=eno1np0/ipv4,eno1np0/ipv6") {
		t.Fatalf("target=%s observed=%s", breakers.Target, breakers.Observed)
	}
	for _, alert := range alerts {
		if alert.Class == "move-target-breakers-open" && strings.HasSuffix(alert.Target, "alpha-edge-1.example") {
			t.Fatal("one open address alerted")
		}
	}

	// open for only part of the hold
	partial := newMoveOutcomesTestAnswer()
	partial.breakers = map[[3]string][2]float64{
		{"beta-edge-2.example", "eno1np0", "4"}: {1, 4},
		{"beta-edge-2.example", "eno1np0", "6"}: {1, 10},
	}
	alerts = moveOutcomesTestRun(t, partial)
	connectMoveTestNoClass(t, alerts, "move-target-breakers-open")
}

// A host whose every address is held open receives no sends, so the breaker
// rule is judged even while the success ratio has nothing to judge.
func TestMoveOutcomesBreakersAreJudgedWithoutSends(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.scalars["sent"] = 0
	answer.breakers = map[[3]string][2]float64{
		{"beta-edge-2.example", "eno1np0", "4"}: {1, 10},
		{"beta-edge-2.example", "eno1np0", "6"}: {1, 10},
	}
	alerts := moveOutcomesTestRun(t, answer)
	connectMoveTestAlert(t, alerts, "move-outcomes-dormant")
	connectMoveTestAlert(t, alerts, "move-target-breakers-open")
}

// A breaker series without the target host, interface and family the probe
// reads cannot be grouped by target; it leaves the outcomes unknown.
func TestMoveOutcomesMisshapenBreakerSeriesAreUnknown(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.scalars["breaker_misshapen"] = 2
	alerts := moveOutcomesTestRun(t, answer)
	incomplete := connectMoveTestAlert(t, alerts, "move-outcomes-telemetry-incomplete")
	if !strings.Contains(incomplete.Observed, "breaker_misshapen_series=2") {
		t.Fatalf("observed=%s", incomplete.Observed)
	}
	connectMoveTestNoClass(t, alerts, "move-success-low", "move-target-breakers-open")
}

// Report labels are a closed set: an unknown value is a contract change that
// fails closed, not a reason to guess.
func TestMoveOutcomesUnknownReportLabelsFailClosed(t *testing.T) {
	for name, mutate := range map[string]func(*moveOutcomesTestAnswer){
		"reason": func(a *moveOutcomesTestAnswer) { a.declined["renamed"] = 1 },
		"cause":  func(a *moveOutcomesTestAnswer) { a.dropped["renamed"] = 1 },
		"kind":   func(a *moveOutcomesTestAnswer) { a.expired["renamed"] = 1 },
	} {
		answer := newMoveOutcomesTestAnswer()
		mutate(answer)
		alerts := moveOutcomesTestRun(t, answer)
		incomplete := connectMoveTestAlert(t, alerts, "move-outcomes-telemetry-incomplete")
		if !strings.Contains(incomplete.Observed, "visibility_reason=unknown_") {
			t.Fatalf("%s: observed=%s", name, incomplete.Observed)
		}
		requireAlertOmits(t, incomplete, "renamed")
	}
}

func TestMoveOutcomesAlertsOmitInstanceNonces(t *testing.T) {
	answer := newMoveOutcomesTestAnswer()
	answer.scalars["success_max"] = 0.2
	answer.failureMin["beta-edge-2.example"] = 0.9
	alerts := moveOutcomesTestRun(t, answer)
	if len(alerts) < 2 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
	for _, alert := range alerts {
		for _, hostName := range connectMoveTestHosts {
			for _, block := range connectMoveTestBlocks {
				requireAlertOmits(t, alert, connectMoveTestInstance(hostName, block))
			}
		}
	}
}

// A source failure is a fixed reason without its text; a parent that ended is
// an error that keeps both causes.
func TestConnectMoveReadKeepsSourceFailuresPrivateAndParentEndsDistinct(t *testing.T) {
	source := newConnectMoveTestSource()
	source.queryErr = errors.New("synthetic-gateway: exit status 7: private diagnostic 192.0.2.7")
	settings := connectMoveTestSettings(source)
	alerts, err := NewMoveOutcomesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := connectMoveTestAlert(t, alerts, "move-outcomes-telemetry-incomplete")
	if !strings.Contains(alert.Observed, "visibility_reason=bounded_source_unavailable") {
		t.Fatalf("observed=%s", alert.Observed)
	}
	requireAlertOmits(t, alert, "192.0.2.7", "private diagnostic")

	ctx, cancel := context.WithCancel(context.Background())
	canceled := newConnectMoveTestSource()
	canceled.cancelOnQuery = cancel
	canceled.queryErr = errors.New("synthetic interrupted read")
	alerts, err = NewMoveOutcomesSignal().Run(ctx, connectMoveTestSettings(canceled))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("an ended parent published: alerts=%v err=%v", connectMoveTestClasses(alerts), err)
	}
}
