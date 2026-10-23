package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// One current process of the synthetic mover fleet.
type moverTestProcess struct {
	host, block, instance string
	start, pushedAt       time.Time
	// nil for a build without the mover
	mode       *float64
	modeAt     *time.Time
	ticks      map[string]float64
	sent       *float64
	sendErrors float64
}

// A synthetic fleet and the fleet-wide parts of its mover query.
type moverTestFleet struct {
	processes     []*moverTestProcess
	window        map[string]float64
	loads         map[string]float64
	loadStarts    map[string]float64
	arrivals      map[string]float64
	windowTicks   map[string]float64
	failedPublish map[string]float64
	refused       map[string]float64
	wouldSend     map[string]float64
	excess        map[string]float64
}

func moverTestMode(mode float64) *float64 { return &mode }

// A balanced fleet with the mover on for the whole window.
func newMoverTestFleet() *moverTestFleet {
	fleet := &moverTestFleet{
		window: map[string]float64{
			"imbalance_min": 1.2, "imbalance_points": 24, "empty_points": 0, "on_points": 24,
			"mode_points": 24, "load_points": 24, "expected_points": 24,
		},
		loads:         map[string]float64{"alpha-edge-1.example": 110, "beta-edge-2.example": 95},
		loadStarts:    map[string]float64{"alpha-edge-1.example": 140, "beta-edge-2.example": 70},
		arrivals:      map[string]float64{"beta-edge-2.example": 300},
		windowTicks:   map[string]float64{"in_band": 2600, "moved": 200},
		failedPublish: map[string]float64{},
		refused:       map[string]float64{"fleet": 0, "source": 0, "target": 0},
		wouldSend:     map[string]float64{},
		excess:        map[string]float64{"alpha-edge-1.example": 0, "beta-edge-2.example": 0},
	}
	for _, hostName := range connectMoveTestHosts {
		for _, block := range connectMoveTestBlocks {
			sent := 10.0
			fleet.processes = append(fleet.processes, &moverTestProcess{
				host: hostName, block: block, instance: connectMoveTestInstance(hostName, block),
				start: connectMoveTestAt().Add(-48 * time.Hour), pushedAt: connectMoveTestAt().Add(-10 * time.Second),
				mode:  moverTestMode(2),
				ticks: map[string]float64{"in_band": 80, "moved": 10},
				sent:  &sent,
			})
		}
	}
	return fleet
}

// Every process at one mode, with ticks to match.
func (self *moverTestFleet) setMode(mode float64) {
	for _, process := range self.processes {
		process.mode = moverTestMode(mode)
		process.sent = nil
		switch mode {
		case 0:
			process.ticks = map[string]float64{"off": 90}
		case 1:
			process.ticks = map[string]float64{"in_band": 80, "moved": 10}
		}
	}
	if mode < 2 {
		self.window["on_points"] = 0
		self.window["imbalance_points"] = 0
	}
}

func (self *moverTestFleet) answer(t *testing.T) string {
	t.Helper()
	answer := newConnectMoveTestAnswer()
	for _, process := range self.processes {
		answer.process(process.host, process.block, process.instance, process.start, process.pushedAt)
		raw := connectMoveTestRaw(process.host, process.block, process.instance)
		if process.mode != nil {
			answer.add("mode", raw, *process.mode)
			modeAt := process.pushedAt
			if process.modeAt != nil {
				modeAt = *process.modeAt
			}
			answer.add("mode_time", raw, float64(modeAt.Unix()))
			for _, outcome := range moverTickOutcomes {
				answer.add("ticks", map[string]string{"host": process.host, "block": process.block, "instance": process.instance, "outcome": outcome}, process.ticks[outcome])
			}
			answer.add("send_errors", map[string]string{"host": process.host, "block": process.block, "instance": process.instance}, process.sendErrors)
			if process.sent != nil {
				answer.add("sent", map[string]string{"host": process.host, "block": process.block, "instance": process.instance}, *process.sent)
			}
		}
	}
	for tag, value := range self.window {
		answer.scalar(tag, value)
	}
	for cap, value := range self.refused {
		answer.add("refused", map[string]string{"cap": cap}, value)
	}
	for _, values := range []struct {
		tag    string
		label  string
		values map[string]float64
	}{
		{tag: "load", label: "host", values: self.loads},
		{tag: "load_start", label: "host", values: self.loadStarts},
		{tag: "arrivals", label: "target_host", values: self.arrivals},
		{tag: "ticks_window", label: "outcome", values: self.windowTicks},
		{tag: "publish_failed", label: "host", values: self.failedPublish},
		{tag: "would_send", label: "source_host", values: self.wouldSend},
		{tag: "excess", label: "host", values: self.excess},
	} {
		for labelValue, value := range values.values {
			answer.add(values.tag, map[string]string{values.label: labelValue}, value)
		}
	}
	return answer.json(t)
}

func moverTestRun(t *testing.T, fleet *moverTestFleet, source *connectMoveTestSource) Alerts {
	t.Helper()
	source.answers["imbalance_min"] = fleet.answer(t)
	alerts, err := NewMoverSignal().Run(context.Background(), connectMoveTestSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestMoverBalancedFleetWithTheMoverOnIsQuiet(t *testing.T) {
	source := newConnectMoveTestSource()
	alerts := moverTestRun(t, newMoverTestFleet(), source)
	if len(alerts) != 0 {
		t.Fatalf("balanced fleet alerted: %v", connectMoveTestClasses(alerts))
	}
	if len(source.queryTimes) != 1 || source.queryTimes[0] != strconv.FormatInt(connectMoveTestNow().Add(-2*time.Minute).Unix(), 10) {
		t.Fatalf("query was not evaluated two minutes behind now: %v", source.queryTimes)
	}
	if len(source.controlReads) != 1 || strings.Join(source.controlReads[0], " ") != "redis.example 6379 -c --json HGETALL {connect_move}control" {
		t.Fatalf("control read=%v", source.controlReads)
	}
}

// No mode series anywhere: a build without the mover. Explicit, never healthy,
// never an incident.
func TestMoverNotDeployedIsAStandingStateNotHealthOrAnIncident(t *testing.T) {
	fleet := newMoverTestFleet()
	for _, process := range fleet.processes {
		process.mode = nil
	}
	fleet.window = map[string]float64{"expected_points": 24}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	if len(alerts) != 1 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
	connectMoveTestStandingState(t, connectMoveTestAlert(t, alerts, "mover-not-deployed"))
}

// With the mover off the imbalance is observed, but the rule needs the mover
// on: a 5 to 1 fleet is the off state, not an imbalance alarm.
func TestMoverOffIsAStandingStateEvenWhenTheFleetIsImbalanced(t *testing.T) {
	for _, control := range [][]string{{}, {"mode", "off"}} {
		fleet := newMoverTestFleet()
		fleet.setMode(0)
		fleet.window["imbalance_min"] = 5
		fleet.loads = map[string]float64{"alpha-edge-1.example": 500, "beta-edge-2.example": 100}
		// store errors while the mover is meant to be off are no failure
		fleet.processes[0].ticks = map[string]float64{"store_error": 90}
		source := newConnectMoveTestSource()
		source.control = control
		alerts := moverTestRun(t, fleet, source)
		if len(alerts) != 1 {
			t.Fatalf("control=%v alerts=%v", control, connectMoveTestClasses(alerts))
		}
		off := connectMoveTestAlert(t, alerts, "mover-off")
		connectMoveTestStandingState(t, off)
		if !strings.Contains(off.Observed, "instant_ratio=5") || !strings.Contains(off.Observed, "control_mode=off") {
			t.Fatalf("off state does not carry the observed load and control: %s", off.Observed)
		}
	}
}

func TestMoverDryRunReportsWouldSendsBesideTheHostExcess(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.setMode(1)
	fleet.wouldSend = map[string]float64{"alpha-edge-1.example": 12}
	fleet.excess = map[string]float64{"alpha-edge-1.example": 150, "beta-edge-2.example": 0}
	source := newConnectMoveTestSource()
	source.control = []string{"mode", "dry_run"}
	alerts := moverTestRun(t, fleet, source)
	if len(alerts) != 1 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
	dryRun := connectMoveTestAlert(t, alerts, "mover-dry-run")
	connectMoveTestStandingState(t, dryRun)
	if !strings.Contains(dryRun.Observed, "would_send_by_source=alpha-edge-1.example:12") || !strings.Contains(dryRun.Observed, "host_excess=alpha-edge-1.example:150,beta-edge-2.example:0") {
		t.Fatalf("observed=%s", dryRun.Observed)
	}
}

// SIGNALS.md §24.1's ticket: above 1.5 at every step of two hours, the mover on.
func TestMoverImbalanceHeldForTheWholeWindowWithTheMoverOnAlerts(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["imbalance_min"] = 3
	fleet.loads = map[string]float64{"alpha-edge-1.example": 300, "beta-edge-2.example": 100}
	fleet.loadStarts = map[string]float64{"alpha-edge-1.example": 300, "beta-edge-2.example": 100}
	fleet.windowTicks = map[string]float64{"in_band": 1000, "no_target": 1800}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	imbalance := connectMoveTestAlert(t, alerts, "move-host-imbalance")
	if imbalance.Severity != SeverityWarn || imbalance.Sustain != 1 || imbalance.unobservable || imbalance.configuredOff || imbalance.Playbook != "SIGNALS.md §24.1" {
		t.Fatalf("imbalance is not a ticket: %+v", imbalance)
	}
	for _, want := range []string{"ratio_min=3", "max_host=alpha-edge-1.example", "min_host=beta-edge-2.example", "imbalance_window_no_target_share=0.643", "judged_steps=24"} {
		if !strings.Contains(imbalance.Observed, want) {
			t.Fatalf("observed lacks %q: %s", want, imbalance.Observed)
		}
	}
	if !strings.Contains(imbalance.Action, "budget_refused means the budget is the bound") {
		t.Fatalf("action lacks the tick-outcome discriminator: %s", imbalance.Action)
	}
}

func TestMoverImbalanceThatDipsIntoTheBandAtOneStepIsHealthy(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["imbalance_min"] = 1.5
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	if len(alerts) != 0 {
		t.Fatalf("a ratio at the band at one step alerted: %v", connectMoveTestClasses(alerts))
	}
}

// False-positive qualifier: a new host starts at no load and fills under the
// target cap; its rising load and arrivals mark it filling.
func TestMoverNewHostFillingUnderTheTargetCapIsNotAnImbalance(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["imbalance_min"] = 8
	fleet.loads = map[string]float64{"alpha-edge-1.example": 400, "beta-edge-2.example": 50}
	fleet.loadStarts = map[string]float64{"alpha-edge-1.example": 420}
	fleet.arrivals = map[string]float64{"beta-edge-2.example": 2400}
	fleet.windowTicks = map[string]float64{"budget_refused": 900, "moved": 1900}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	connectMoveTestNoClass(t, alerts, "move-host-imbalance")
	filling := connectMoveTestAlert(t, alerts, "move-host-filling")
	connectMoveTestStandingState(t, filling)
	if !strings.Contains(filling.Observed, "min_host_start_load=absent") || !strings.Contains(filling.Observed, "min_host_arrivals=2400") {
		t.Fatalf("observed=%s", filling.Observed)
	}
}

// The filling discriminator needs both arrivals and a rising load.
func TestMoverMinimumHostWithArrivalsButNoRisingLoadIsImbalanced(t *testing.T) {
	for name, mutate := range map[string]func(*moverTestFleet){
		"falling load": func(f *moverTestFleet) {
			f.loadStarts["beta-edge-2.example"] = 60
			f.arrivals["beta-edge-2.example"] = 2400
		},
		"few arrivals": func(f *moverTestFleet) {
			delete(f.loadStarts, "beta-edge-2.example")
			f.arrivals["beta-edge-2.example"] = 30
		},
	} {
		fleet := newMoverTestFleet()
		fleet.window["imbalance_min"] = 8
		fleet.loads = map[string]float64{"alpha-edge-1.example": 400, "beta-edge-2.example": 50}
		mutate(fleet)
		t.Logf("case %s", name)
		alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
		connectMoveTestNoClass(t, alerts, "move-host-filling")
		connectMoveTestAlert(t, alerts, "move-host-imbalance")
	}
}

// A host left at no load for the whole window has an infinite ratio: leaving
// it out of the minimum must not hide that it never filled.
func TestMoverHostEmptyForTheWholeWindowIsAnImbalance(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["empty_points"] = 24
	fleet.window["imbalance_min"] = 1.1
	fleet.loads = map[string]float64{"alpha-edge-1.example": 200, "beta-edge-2.example": 0}
	fleet.arrivals = map[string]float64{}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	imbalance := connectMoveTestAlert(t, alerts, "move-host-imbalance")
	if !strings.Contains(imbalance.Symptom, "stayed empty") || !strings.Contains(imbalance.Observed, "empty_host_steps=24") {
		t.Fatalf("symptom=%s observed=%s", imbalance.Symptom, imbalance.Observed)
	}
}

func TestMoverOnForLessThanTheWindowIsWarming(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["on_points"] = 6
	fleet.window["imbalance_points"] = 6
	fleet.window["imbalance_min"] = 4
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	connectMoveTestNoClass(t, alerts, "move-host-imbalance")
	connectMoveTestStandingState(t, connectMoveTestAlert(t, alerts, "mover-warming"))
}

// False-negative qualifier: a host whose load publishes fail drops out of the
// table and the ratio, so the window is unknown, never balanced.
func TestMoverHostWhoseLoadPublishesFailLeavesTheWindowUnknown(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["load_points"] = 20
	fleet.window["imbalance_points"] = 20
	fleet.failedPublish = map[string]float64{"beta-edge-2.example": 30}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	incomplete := connectMoveTestAlert(t, alerts, "move-host-load-incomplete")
	if !incomplete.unobservable || incomplete.configuredOff || !strings.Contains(incomplete.Observed, "failed_load_publishes=beta-edge-2.example:30") {
		t.Fatalf("load gap is not an unknown with its discriminator: unobservable=%t configuredOff=%t observed=%s", incomplete.unobservable, incomplete.configuredOff, incomplete.Observed)
	}

	gap := newMoverTestFleet()
	gap.window["mode_points"] = 22
	gap.window["on_points"] = 22
	alerts = moverTestRun(t, gap, newConnectMoveTestSource())
	connectMoveTestNoClass(t, alerts, "mover-warming")
	connectMoveTestAlert(t, alerts, "move-host-load-incomplete")
}

// A process at 0 while the others read 2: it cannot read the control (it
// fails closed) or runs with EnableMove off; its store errors discriminate.
func TestMoverProcessBelowTheFleetModeIsDivergentWithItsStoreErrors(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.processes[1].mode = moverTestMode(0)
	fleet.processes[1].ticks = map[string]float64{"store_error": 85, "off": 5}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	divergent := connectMoveTestAlert(t, alerts, "mover-mode-divergent")
	if divergent.Sustain != 3 || !strings.Contains(divergent.Observed, "divergence=processes_disagree,fleet_differs_from_control") ||
		!strings.Contains(divergent.Observed, "below_mode_slots=alpha-edge-1.example/green") || !strings.Contains(divergent.Observed, "below_mode_store_error_slots=1") {
		t.Fatalf("sustain=%d observed=%s", divergent.Sustain, divergent.Observed)
	}
	failing := connectMoveTestAlert(t, alerts, "mover-store-errors")
	if !strings.Contains(failing.Observed, "store_error_slots=alpha-edge-1.example/green") {
		t.Fatalf("observed=%s", failing.Observed)
	}
}

// The fleet agrees with itself but not with the control: every process fails
// closed, or the build turns the mover off.
func TestMoverFleetThatIgnoresAnOnControlIsDivergent(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.setMode(0)
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	divergent := connectMoveTestAlert(t, alerts, "mover-mode-divergent")
	if !strings.Contains(divergent.Observed, "divergence=fleet_differs_from_control") || !strings.Contains(divergent.Observed, "control_mode=on") {
		t.Fatalf("observed=%s", divergent.Observed)
	}
	// the fleet is effectively off, which holds the imbalance unjudged
	connectMoveTestStandingState(t, connectMoveTestAlert(t, alerts, "mover-off"))
}

func TestMoverProcessWithoutTheMoverWhileItIsOnIsDivergent(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.processes[3].mode = nil
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	divergent := connectMoveTestAlert(t, alerts, "mover-mode-divergent")
	if !strings.Contains(divergent.Observed, "divergence=processes_without_mover") || !strings.Contains(divergent.Observed, "legacy_slots=beta-edge-2.example/green") {
		t.Fatalf("observed=%s", divergent.Observed)
	}
}

// A control value that does not parse narrows to off: the operator's intent
// is lost, and the fleet reading off agrees with the narrowed control.
func TestMoverControlValueThatDoesNotParseNarrowsToOff(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.setMode(0)
	source := newConnectMoveTestSource()
	source.control = []string{"mode", "On", "maxPerMinute", "sixty", "evacuateHosts", "gamma-edge-3.example"}
	alerts := moverTestRun(t, fleet, source)
	invalid := connectMoveTestAlert(t, alerts, "mover-control-invalid")
	if !strings.Contains(invalid.Symptom, "maxPerMinute,mode") || !strings.Contains(invalid.Observed, "control_mode=off") {
		t.Fatalf("symptom=%s observed=%s", invalid.Symptom, invalid.Observed)
	}
	requireAlertOmits(t, invalid, "sixty", `"On"`, "gamma-edge-3")
	connectMoveTestNoClass(t, alerts, "mover-mode-divergent")
	connectMoveTestAlert(t, alerts, "mover-off")
}

// Without the control the imbalance is not judged: an evacuated host empties
// by design, and an operator's off cannot be told from failing processes.
func TestMoverUnreadableControlLeavesTheImbalanceUnjudged(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.window["imbalance_min"] = 6
	fleet.processes[0].ticks = map[string]float64{"store_error": 80, "moved": 10}
	source := newConnectMoveTestSource()
	source.controlErr = errors.New("synthetic redis: connection refused 192.0.2.8")
	alerts := moverTestRun(t, fleet, source)
	unobservable := connectMoveTestAlert(t, alerts, "mover-control-unobservable")
	if !unobservable.unobservable || unobservable.configuredOff || !strings.Contains(unobservable.Observed, "control_reason=redis_read_failed") {
		t.Fatalf("unobservable=%t configuredOff=%t observed=%s", unobservable.unobservable, unobservable.configuredOff, unobservable.Observed)
	}
	requireAlertOmits(t, unobservable, "192.0.2.8", "connection refused")
	connectMoveTestNoClass(t, alerts, "move-host-imbalance")
	// gauge rules still judge what the gauges show
	connectMoveTestAlert(t, alerts, "mover-store-errors")
}

// An evacuated host leaves the load the ratio is judged on.
func TestMoverEvacuatedHostIsLeftOutOfTheLoad(t *testing.T) {
	source := newConnectMoveTestSource()
	source.control = []string{"mode", "on", "evacuateHosts", "beta-edge-2.example"}
	moverTestRun(t, newMoverTestFleet(), source)
	query := source.queries[0]
	if !strings.Contains(query, `urnetwork_connect_move_host_normalized_load{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green",host!~"beta-edge-2\\.example"}`) ||
		!strings.Contains(query, ") == 1)") {
		t.Fatalf("the evacuated host was not left out of the load: %s", query)
	}
}

// Shares are of acting ticks: ticks within the band dilute nothing, and the
// mix is per tick, not per move (SIGNALS.md §24.5).
func TestMoverDatabasePausesAreJudgedOverActingTicks(t *testing.T) {
	fleet := newMoverTestFleet()
	for _, process := range fleet.processes {
		process.ticks = map[string]float64{"in_band": 225, "db_paused": 10, "moved": 1}
	}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	paused := connectMoveTestAlert(t, alerts, "mover-db-paused")
	if !strings.Contains(paused.Action, "Read the database, not the mover") || !strings.Contains(paused.Observed, "window_db_paused_share=0.042") {
		t.Fatalf("action=%s observed=%s", paused.Action, paused.Observed)
	}

	quiet := newMoverTestFleet()
	for _, process := range quiet.processes {
		process.ticks = map[string]float64{"in_band": 225, "db_paused": 4, "moved": 6}
	}
	alerts = moverTestRun(t, quiet, newConnectMoveTestSource())
	connectMoveTestNoClass(t, alerts, "mover-db-paused")

	few := newMoverTestFleet()
	for _, process := range few.processes {
		process.ticks = map[string]float64{"in_band": 225, "db_paused": 5}
	}
	alerts = moverTestRun(t, few, newConnectMoveTestSource())
	connectMoveTestNoClass(t, alerts, "mover-db-paused")
}

// A burst on every sending process points at the move primitives; one
// process's errors are ordinary churn.
func TestMoverSendErrorsOnEverySendingProcess(t *testing.T) {
	fleet := newMoverTestFleet()
	for i, process := range fleet.processes {
		sent := 0.0
		process.sent = &sent
		if i < 2 {
			process.sent = moverTestMode(4)
			process.sendErrors = 6
		}
	}
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	burst := connectMoveTestAlert(t, alerts, "move-send-errors-fleetwide")
	if !strings.Contains(burst.Observed, "sending_slots=alpha-edge-1.example/blue,alpha-edge-1.example/green") {
		t.Fatalf("observed=%s", burst.Observed)
	}

	one := newMoverTestFleet()
	one.processes[0].sendErrors = 20
	alerts = moverTestRun(t, one, newConnectMoveTestSource())
	connectMoveTestNoClass(t, alerts, "move-send-errors-fleetwide")
}

func TestMoverIncompleteSlotCoverageIsUnknownWithoutAState(t *testing.T) {
	for name, mutate := range map[string]func(*moverTestFleet){
		"missing slot": func(f *moverTestFleet) { f.processes = f.processes[1:] },
		"stale slot":   func(f *moverTestFleet) { f.processes[0].pushedAt = connectMoveTestAt().Add(-3 * time.Minute) },
		"incoherent push": func(f *moverTestFleet) {
			modeAt := f.processes[0].pushedAt.Add(-15 * time.Second)
			f.processes[0].modeAt = &modeAt
		},
		"mode out of range": func(f *moverTestFleet) { f.processes[0].mode = moverTestMode(3) },
	} {
		fleet := newMoverTestFleet()
		fleet.setMode(0)
		mutate(fleet)
		alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
		incomplete := connectMoveTestAlert(t, alerts, "mover-telemetry-incomplete")
		if !incomplete.unobservable || incomplete.configuredOff {
			t.Errorf("%s: incomplete coverage is not a posted unknown", name)
		}
		connectMoveTestNoClass(t, alerts, "mover-off", "mover-not-deployed")
	}
}

// Positive evidence is reported beside an incomplete census.
func TestMoverDivergenceAmongObservedProcessesSurvivesAMissingSlot(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.processes = fleet.processes[1:]
	fleet.processes[0].mode = moverTestMode(0)
	alerts := moverTestRun(t, fleet, newConnectMoveTestSource())
	connectMoveTestAlert(t, alerts, "mover-telemetry-incomplete")
	connectMoveTestAlert(t, alerts, "mover-mode-divergent")
}

func TestMoverRejectsAnUnknownTickOutcome(t *testing.T) {
	fleet := newMoverTestFleet()
	source := newConnectMoveTestSource()
	answer := fleet.answer(t)
	source.answers["imbalance_min"] = strings.Replace(answer, `"outcome":"no_load"`, `"outcome":"renamed"`, 1)
	alerts, err := NewMoverSignal().Run(context.Background(), connectMoveTestSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if alert := connectMoveTestAlert(t, alerts, "mover-telemetry-incomplete"); !strings.Contains(alert.Observed, "visibility_reason=unknown_tick_outcome") {
		t.Fatalf("observed=%s", alert.Observed)
	}
}

func TestMoverInvalidSettingsAreUnknownWithoutContact(t *testing.T) {
	settings := DefaultConnectMoveSettings()
	settings.ImbalanceRatio = 0.5
	source := newConnectMoveTestSource()
	alerts, err := NewMoverSignalWithSettings(settings).Run(context.Background(), connectMoveTestSettings(source))
	if err != nil || len(source.queries)+len(source.controlReads) != 0 {
		t.Fatalf("err=%v queries=%d control reads=%d", err, len(source.queries), len(source.controlReads))
	}
	if alert := connectMoveTestAlert(t, alerts, "mover-telemetry-incomplete"); !strings.Contains(alert.Observed, "visibility_reason=settings_invalid") {
		t.Fatalf("observed=%s", alert.Observed)
	}
}

func TestMoverCanceledParentPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := newConnectMoveTestSource()
	source.cancelOnQuery = cancel
	source.answers["imbalance_min"] = newMoverTestFleet().answer(t)
	alerts, err := NewMoverSignal().Run(ctx, connectMoveTestSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("alerts=%v err=%v", connectMoveTestClasses(alerts), err)
	}
}

// Instance nonces and control values never leave the source.
func TestMoverAlertsOmitInstanceNoncesAndControlValues(t *testing.T) {
	fleet := newMoverTestFleet()
	fleet.processes[1].mode = moverTestMode(0)
	fleet.processes[1].ticks = map[string]float64{"store_error": 85, "off": 5}
	source := newConnectMoveTestSource()
	source.control = []string{"mode", "on", "tolerance", "0.25", "evacuateHosts", "gamma-edge-3.example"}
	alerts := moverTestRun(t, fleet, source)
	if len(alerts) == 0 {
		t.Fatal("no alerts to inspect")
	}
	for _, alert := range alerts {
		for _, process := range fleet.processes {
			requireAlertOmits(t, alert, process.instance)
		}
		requireAlertOmits(t, alert, "0.25", "gamma-edge-3")
		if alert.Frame != "" || !strings.HasPrefix(alert.Target, "synthetic/connect-mover") {
			t.Fatalf("identity=%s", alert.Identity())
		}
	}
}

// The query reads the series SIGNALS.md §24.1, §24.4 and §24.5 name.
func TestMoverQueryReadsTheMoverSeriesOverItsWindows(t *testing.T) {
	source := newConnectMoveTestSource()
	moverTestRun(t, newMoverTestFleet(), source)
	query := source.queries[0]
	for _, want := range []string{
		`urnetwork_connect_mover_mode{env="synthetic",job="connect",service="connect",`,
		`increase(urnetwork_connect_mover_ticks_total{`,
		`increase(urnetwork_connect_move_send_errors_total{`,
		`sum by (cap) (increase(urnetwork_connect_move_budget_refused_total{`,
		`sum by (source_host) (increase(urnetwork_connect_moves_would_send_total{`,
		`max by (host) (urnetwork_connect_move_host_excess{`,
		`increase(urnetwork_connect_moves_arrived_total{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green",kind=~"advisory|compulsory"}[7200s])`,
		`increase(urnetwork_connect_move_load_publishes_total{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green",outcome="failed"}[7200s])`,
		`(max(urnetwork_connect_mover_mode{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green"}) == 2)`,
		` > 0))`,
		`[7200s:300s])`,
		`[900s]`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lacks %q", want)
		}
	}
}
