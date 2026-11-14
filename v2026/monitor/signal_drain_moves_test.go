package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// One process of the synthetic drain fleet, current or drained.
type drainMovesTestProcess struct {
	host, block, instance string
	current               bool
	gates                 bool
	refusals              map[string]float64
	drainRecent           float64
	drained               float64
	closes                float64
	capable, legacy       *float64
	moverCompulsory       float64
}

func drainMovesTestCount(value float64) *float64 { return &value }

// A fleet of current processes that run the drain gates and have not drained.
func newDrainMovesTestFleet() []*drainMovesTestProcess {
	fleet := []*drainMovesTestProcess{}
	for _, hostName := range connectMoveTestHosts {
		for _, block := range connectMoveTestBlocks {
			fleet = append(fleet, &drainMovesTestProcess{
				host: hostName, block: block, instance: connectMoveTestInstance(hostName, block),
				current: true, gates: true, refusals: map[string]float64{},
				capable: drainMovesTestCount(0), legacy: drainMovesTestCount(0),
			})
		}
	}
	return fleet
}

func drainMovesTestJson(t *testing.T, fleet []*drainMovesTestProcess) string {
	t.Helper()
	answer := newConnectMoveTestAnswer()
	for _, process := range fleet {
		labels := map[string]string{"host": process.host, "block": process.block, "instance": process.instance}
		if process.current {
			answer.process(process.host, process.block, process.instance, connectMoveTestAt().Add(-48*time.Hour), connectMoveTestAt().Add(-10*time.Second))
			answer.add("drain_recent", labels, process.drainRecent)
		}
		if process.gates && process.current {
			answer.add("refusal_series", labels, 3)
			for _, carrier := range drainRefusalCarriers {
				answer.add("refusals", map[string]string{"host": process.host, "block": process.block, "instance": process.instance, "carrier": carrier}, process.refusals[carrier])
			}
		}
		answer.add("drained", labels, process.drained)
		answer.add("closes", labels, process.closes)
		answer.add("mover_compulsory", labels, process.moverCompulsory)
		if process.capable != nil {
			answer.add("drain_moves", map[string]string{"host": process.host, "block": process.block, "instance": process.instance, "capable": "true"}, *process.capable)
		}
		if process.legacy != nil {
			answer.add("drain_moves", map[string]string{"host": process.host, "block": process.block, "instance": process.instance, "capable": "false"}, *process.legacy)
		}
	}
	return answer.json(t)
}

func drainMovesTestRun(t *testing.T, fleet []*drainMovesTestProcess) (Alerts, *connectMoveTestSource) {
	t.Helper()
	source := newConnectMoveTestSource()
	source.answers["mover_compulsory"] = drainMovesTestJson(t, fleet)
	alerts, err := NewDrainMovesSignal().Run(context.Background(), connectMoveTestSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts, source
}

// A drained process that has exited: its counters remain in the drain window.
func drainMovesTestDrained(hostName, block string, drained, closes, capable, legacy, moverCompulsory float64) *drainMovesTestProcess {
	return &drainMovesTestProcess{
		host: hostName, block: block, instance: "drained" + block,
		drained: drained, closes: closes, capable: drainMovesTestCount(capable), legacy: drainMovesTestCount(legacy), moverCompulsory: moverCompulsory,
	}
}

// False-positive qualifier: a client that redials into a draining process is
// refused by design; refusals while its gauge is above zero are no alarm.
func TestDrainMovesRefusalsDuringADrainAreByDesign(t *testing.T) {
	fleet := newDrainMovesTestFleet()
	fleet[0].refusals = map[string]float64{"h1": 400, "quic": 90, "exchange": 12}
	fleet[0].drainRecent = 1500
	fleet = append(fleet, drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 40, 1200, 300, 0))
	alerts, _ := drainMovesTestRun(t, fleet)
	if len(alerts) != 0 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
}

// SIGNALS.md §24.7's ticket: refusals outside any drain window, from a process
// that still publishes.
func TestDrainMovesRefusalsWithTheGaugeAtZeroAreAStuckDrain(t *testing.T) {
	fleet := newDrainMovesTestFleet()
	fleet[3].refusals = map[string]float64{"h1": 250, "quic": 0, "exchange": 3}
	alerts, _ := drainMovesTestRun(t, fleet)
	stuck := connectMoveTestAlert(t, alerts, "drain-refusals-outside-drain")
	if stuck.Target != "synthetic/connect-drains/beta-edge-2.example/green" || stuck.Sustain != 2 || stuck.unobservable ||
		!strings.Contains(stuck.Observed, "refusals=253") || !strings.Contains(stuck.Observed, "by_carrier=h1:250,exchange:3") {
		t.Fatalf("target=%s sustain=%d observed=%s", stuck.Target, stuck.Sustain, stuck.Observed)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
}

// SIGNALS.md §24.3's ticket: closes above the drained process's legacy
// residents mean capable clients failed their replacement too.
func TestDrainMovesClosesAboveTheLegacyResidentsAlert(t *testing.T) {
	fleet := append(newDrainMovesTestFleet(),
		drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 330, 1200, 300, 0),
		drainMovesTestDrained("beta-edge-2.example", "green", 900, 150, 700, 200, 0),
	)
	alerts, _ := drainMovesTestRun(t, fleet)
	excess := connectMoveTestAlert(t, alerts, "drain-closes-excess")
	if excess.Target != "synthetic/connect-drains" || excess.Playbook != "SIGNALS.md §24.3" ||
		!strings.Contains(excess.Observed, "judged_drains=2") || !strings.Contains(excess.Observed, "excess_drains=1") ||
		!strings.Contains(excess.Observed, "drains=alpha-edge-1.example/blue:closes=330,legacy=300,capable=1200,mover_compulsory=0") {
		t.Fatalf("target=%s observed=%s", excess.Target, excess.Observed)
	}
	requireAlertOmits(t, excess, "drainedblue", "drainedgreen")
}

// False-positive qualifier: evacuation and emergency compulsory moves close
// clients too; every compulsory move the mover sent from the process is taken
// off before the closes are compared.
func TestDrainMovesClosesExplainedByTheMoversCompulsoryMovesAreNoAlarm(t *testing.T) {
	fleet := append(newDrainMovesTestFleet(), drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 330, 1200, 300, 40))
	alerts, _ := drainMovesTestRun(t, fleet)
	connectMoveTestNoClass(t, alerts, "drain-closes-excess")
}

// A drain without drain moves (the control's drainMoves false, or every intent
// write failing) has no compulsory closes to judge; a build from before drain
// moves is not counted at all.
func TestDrainMovesDrainsWithoutDrainMovesAreNotJudged(t *testing.T) {
	legacyBuild := drainMovesTestDrained("beta-edge-2.example", "blue", 800, 0, 0, 0, 0)
	legacyBuild.capable, legacyBuild.legacy = nil, nil
	fleet := append(newDrainMovesTestFleet(),
		drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 0, 0, 0, 0),
		legacyBuild,
	)
	alerts, _ := drainMovesTestRun(t, fleet)
	if len(alerts) != 0 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
	fleet = append(fleet, drainMovesTestDrained("alpha-edge-1.example", "green", 600, 90, 500, 10, 0))
	alerts, _ = drainMovesTestRun(t, fleet)
	excess := connectMoveTestAlert(t, alerts, "drain-closes-excess")
	if !strings.Contains(excess.Observed, "judged_drains=1 drains_without_drain_moves=1 excess_drains=1") {
		t.Fatalf("observed=%s", excess.Observed)
	}
}

// No current process runs the drain gates: explicit, never healthy, never an
// incident.
func TestDrainMovesNotDeployedIsAStandingState(t *testing.T) {
	fleet := newDrainMovesTestFleet()
	for _, process := range fleet {
		process.gates, process.capable, process.legacy = false, nil, nil
	}
	alerts, _ := drainMovesTestRun(t, fleet)
	if len(alerts) != 1 {
		t.Fatalf("alerts=%v", connectMoveTestClasses(alerts))
	}
	connectMoveTestStandingState(t, connectMoveTestAlert(t, alerts, "drain-moves-not-deployed"))
}

// A slot that stopped publishing hides its refusals: unknown, never stuck or
// healthy by a sibling.
func TestDrainMovesMissingSlotIsUnknown(t *testing.T) {
	fleet := newDrainMovesTestFleet()[1:]
	alerts, _ := drainMovesTestRun(t, fleet)
	incomplete := connectMoveTestAlert(t, alerts, "drain-moves-telemetry-incomplete")
	if !incomplete.unobservable || incomplete.configuredOff || !strings.Contains(incomplete.Observed, "missing_slots=1") {
		t.Fatalf("observed=%s", incomplete.Observed)
	}
	connectMoveTestNoClass(t, alerts, "drain-moves-not-deployed")
}

func TestDrainMovesUnknownLabelValuesFailClosed(t *testing.T) {
	fleet := newDrainMovesTestFleet()
	source := newConnectMoveTestSource()
	source.answers["mover_compulsory"] = strings.Replace(drainMovesTestJson(t, fleet), `"carrier":"quic"`, `"carrier":"renamed"`, 1)
	alerts, err := NewDrainMovesSignal().Run(context.Background(), connectMoveTestSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	incomplete := connectMoveTestAlert(t, alerts, "drain-moves-telemetry-incomplete")
	if !strings.Contains(incomplete.Observed, "visibility_reason=unknown_refusal_carrier") {
		t.Fatalf("observed=%s", incomplete.Observed)
	}
}

// Counts are exact per process: the maximum less the minimum within the drain
// window, never an extrapolated increase.
func TestDrainMovesQueryCountsExactlyPerProcess(t *testing.T) {
	_, source := drainMovesTestRun(t, newDrainMovesTestFleet())
	query := source.queries[0]
	selector := `{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green"}`
	for _, want := range []string{
		"sum by (host, block, instance) (max_over_time(urnetwork_connect_moves_compulsory_closed_total" + selector + "[86400s]) - min_over_time(urnetwork_connect_moves_compulsory_closed_total" + selector + "[86400s]))",
		"sum by (host, block, instance, capable) (max_over_time(urnetwork_connect_drain_moves_sent_total" + selector + "[86400s]) - min_over_time(urnetwork_connect_drain_moves_sent_total" + selector + "[86400s]))",
		`max_over_time(urnetwork_connect_moves_sent_total{env="synthetic",job="connect",service="connect",host=~"alpha-edge-1\\.example|beta-edge-2\\.example",block=~"blue|green",kind="compulsory"}[86400s])`,
		"increase(urnetwork_connect_drain_refusals_total" + selector + "[3600s])",
		"max_over_time(urnetwork_connect_drain_residents_remaining" + selector + "[3600s])",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lacks %q", want)
		}
	}
}
