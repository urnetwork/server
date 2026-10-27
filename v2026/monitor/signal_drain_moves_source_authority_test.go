// Missing eager producer series stay unknown through the public signal and
// its real cadence/notifier consumers. All source rows are synthetic.
package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Removes exactly one synthetic source row without changing its companions.
func drainMovesTestWithout(t *testing.T, answer string, process *drainMovesTestProcess, tag, child string) string {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(answer), &response); err != nil {
		t.Fatal(err)
	}
	removed := 0
	for i := 0; i < len(response.Data.Result); {
		metric := response.Data.Result[i].Metric
		if metric["monitor_move"] == tag && metric["host"] == process.host && metric["block"] == process.block && metric["instance"] == process.instance &&
			(child == "" || metric["carrier"] == child || metric["capable"] == child) {
			response.Data.Result = append(response.Data.Result[:i], response.Data.Result[i+1:]...)
			removed++
		} else {
			i++
		}
	}
	if removed != 1 {
		t.Fatalf("removed %d rows for %s/%s, want one", removed, tag, child)
	}
	out, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Runs the actual adapter on one fixed vector, without a real transport.
func drainMovesTestSourceAnswer(t *testing.T, answer string) Alerts {
	t.Helper()
	source := newConnectMoveTestSource()
	source.answers["mover_compulsory"] = answer
	alerts, err := NewDrainMovesSignal().Run(context.Background(), connectMoveTestSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Refusal children, drain gauges, closes and both drain-move children are
// initialized by the producer; their absence cannot certify zero events.
func TestDrainMovesMissingEagerSeriesRemainUnknown(t *testing.T) {
	for _, missing := range []struct {
		tag, child string
		historical bool
	}{
		{tag: "refusal_series"},
		{tag: "refusals", child: "quic"},
		{tag: "drain_recent"},
		{tag: "closes", historical: true},
		{tag: "drained", historical: true},
		{tag: "drain_moves", child: "false", historical: true},
	} {
		fleet := newDrainMovesTestFleet()
		process := fleet[0]
		if missing.historical {
			process = drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 10, 1200, 300, 0)
			fleet = append(fleet, process)
		}
		answer := drainMovesTestWithout(t, drainMovesTestJson(t, fleet), process, missing.tag, missing.child)
		alerts := drainMovesTestSourceAnswer(t, answer)
		unknownFound := false
		for _, alert := range alerts {
			if alert.Class == "drain-moves-telemetry-incomplete" {
				unknownFound = alert.unobservable && !alert.configuredOff
			}
		}
		if !unknownFound {
			t.Errorf("%s/%s: missing producer data became complete or configured-off; alerts=%v", missing.tag, missing.child, connectMoveTestClasses(alerts))
		}
		connectMoveTestNoClass(t, alerts, "drain-moves-not-deployed", "drain-closes-excess", "drain-refusals-outside-drain")
	}
}

// A partial sibling never erases an independently qualified positive drain
// or refusal, and process nonces remain private.
func TestDrainMovesPartialSourcePreservesIndependentFindings(t *testing.T) {
	fleet := newDrainMovesTestFleet()
	fleet[1].refusals["h1"] = 5
	partial := drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 10, 1200, 300, 0)
	fleet = append(fleet, partial, drainMovesTestDrained("beta-edge-2.example", "green", 900, 90, 800, 10, 0))
	answer := drainMovesTestWithout(t, drainMovesTestJson(t, fleet), partial, "closes", "")
	alerts := drainMovesTestSourceAnswer(t, answer)
	unknown := connectMoveTestAlert(t, alerts, "drain-moves-telemetry-incomplete")
	if !unknown.unobservable {
		t.Fatal("missing sibling close counter became complete")
	}
	connectMoveTestAlert(t, alerts, "drain-refusals-outside-drain")
	excess := connectMoveTestAlert(t, alerts, "drain-closes-excess")
	if !strings.Contains(excess.Observed, "excess_drains=1") {
		t.Fatal("complete positive drain was erased or inflated")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, partial.instance, "drainedgreen")
	}
}

// Missing closes used to look like a complete absence, clearing a real
// incident after two runs. The actual cadence and notifier must retain it.
func TestDrainMovesMissingSourceCannotClearIncident(t *testing.T) {
	signal := NewDrainMovesSignal()
	gate := newCadenceAlertGate()
	consumer := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	fleet := newDrainMovesTestFleet()
	drained := drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 330, 1200, 300, 0)
	fleet = append(fleet, drained)
	positive := drainMovesTestJson(t, fleet)
	missing := drainMovesTestWithout(t, positive, drained, "closes", "")
	drained.closes = 10
	healthy := drainMovesTestJson(t, fleet)
	incidentKey := ""
	for run, answer := range []string{positive, missing, missing, healthy, healthy} {
		alerts := drainMovesTestSourceAnswer(t, answer)
		ready, held := gate.split(signal, alerts)
		if run == 1 && (len(ready) != 0 || len(held) != 1 || !held[0].unobservable) {
			t.Fatal("first missing-source observation must be held and remain unknown")
		}
		if run == 2 && (len(ready) != 1 || !ready[0].unobservable) {
			t.Fatal("second missing-source observation must emit visibility warning")
		}
		transitions := consumer.observeWithLock(connectMoveTestNow().Add(time.Duration(run)*time.Minute), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		raised, cleared := 0, 0
		for _, transition := range transitions {
			if transition.alert.Class == "drain-closes-excess" {
				incidentKey = slackIncidentKey(transition.alert)
				if transition.kind == slackRaised {
					raised++
				}
				if transition.kind == slackCleared {
					cleared++
				}
			}
		}
		if run == 0 {
			if raised != 1 || cleared != 0 {
				t.Fatal("qualified excess drain did not raise once")
			}
		} else if run < 4 {
			if _, present := consumer.keyIncidents[incidentKey]; !present || raised != 0 || cleared != 0 {
				t.Fatalf("run %d: missing source or first complete recovery cleared the incident", run)
			}
		} else if raised != 0 || cleared != 1 {
			t.Fatal("two consecutive complete healthy runs did not clear once")
		}
	}
}

// Historical range rows have no start record to validate their slot. Reject
// an out-of-inventory process before its labels can enter public findings.
func TestDrainMovesHistoricalLabelsStayInsideInventory(t *testing.T) {
	for _, foreign := range []struct{ host, block string }{
		{host: "untrusted.example", block: "blue"},
		{host: "alpha-edge-1.example", block: "private-block"},
	} {
		fleet := newDrainMovesTestFleet()
		fleet[0].refusals["h1"] = 5
		fleet = append(fleet, drainMovesTestDrained(foreign.host, foreign.block, 1500, 330, 1200, 300, 0))
		alerts, _ := drainMovesTestRun(t, fleet)
		unknown := connectMoveTestAlert(t, alerts, "drain-moves-telemetry-incomplete")
		if !unknown.unobservable {
			t.Fatal("foreign historical labels became authoritative")
		}
		connectMoveTestAlert(t, alerts, "drain-refusals-outside-drain")
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "untrusted.example", "private-block")
		}
	}
}

// The mover send counter is lazy. Its absence remains a valid zero when the
// drain process's eager series identify the supported producer.
func TestDrainMovesMissingLazyMoverChildRetainsKnownZero(t *testing.T) {
	fleet := newDrainMovesTestFleet()
	drained := drainMovesTestDrained("alpha-edge-1.example", "blue", 1500, 330, 1200, 300, 0)
	fleet = append(fleet, drained)
	answer := drainMovesTestWithout(t, drainMovesTestJson(t, fleet), drained, "mover_compulsory", "")
	alerts := drainMovesTestSourceAnswer(t, answer)
	connectMoveTestAlert(t, alerts, "drain-closes-excess")
	connectMoveTestNoClass(t, alerts, "drain-moves-telemetry-incomplete")
}
