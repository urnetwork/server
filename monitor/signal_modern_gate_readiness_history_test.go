// Current fleet evidence survives failures of the separate historical read.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A transport error, malformed response, or invalid historical observation
// cannot remove the independently observed mixed-gate page from Signal.Run.
func TestModernGateReadinessSignalPreservesFleetPageWhenHistoryFails(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(6 * time.Hour)
	before := start.Add(-time.Second)
	off := false
	processes := append(modernGateReadinessTestProcesses(3, now, true, &start), modernGateReadinessTestProcess{started: now.Add(-24 * time.Hour), on: &off})
	current := modernGateReadinessTestPayload(t, now, processes, start.Add(time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 30, model.RankModeSpeed: 60})
	for _, c := range []struct {
		name      string
		payload   string
		failQuery bool
	}{
		{name: "query failure", failQuery: true},
		{name: "malformed response", payload: "synthetic malformed response"},
		{name: "invalid bucket", payload: fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_modern_gate_part":"bucket","bucket":"synthetic-unknown"},"value":[%d,"100"]}]}}`, before.Unix())},
	} {
		for _, scheduleFailure := range []string{"", "quality_speed_modern_provider_start is neither an RFC 3339 instant nor a date"} {
			payloads := map[int64]string{now.Unix(): current}
			if !c.failQuery {
				payloads[before.Unix()] = c.payload
			}
			alerts, err := modernGateReadinessTestSignal(scheduleFailure).Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
			if err != nil {
				t.Fatalf("%s: current fleet evidence was discarded: %v", c.name, err)
			}
			wantCount := 2
			if scheduleFailure != "" {
				wantCount++
				requireAlertClass(t, alerts, "modern-gate-schedule-unreadable")
			}
			if len(alerts) != wantCount {
				t.Fatalf("%s: alerts %+v, want fleet and visibility findings", c.name, alerts)
			}
			fleet := requireAlertClass(t, alerts, "modern-gate-fleet-disagrees")
			if fleet.Severity != SeverityPage || !strings.Contains(fleet.Markdown(), "gate_on_min=0 gate_on_max=1") {
				t.Errorf("%s: lost the observed mixed-gate page: %+v", c.name, fleet)
			}
			visibility := requireAlertClass(t, alerts, "cannot-observe")
			if visibility.Severity != SeverityWarn || !isVisibilityAlert(visibility) {
				t.Errorf("%s: historical failure is not a visibility warning: %+v", c.name, visibility)
			}
		}
	}
}

// A fleet that agrees has no independent page to retain, and the unavailable
// baseline cannot establish a flip drop even when the current bucket is small.
func TestModernGateReadinessSignalCannotJudgeDropWithoutHistory(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(6 * time.Hour)
	current := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, true, &start), start.Add(time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 1, model.RankModeSpeed: 1})
	alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): current}))
	if err == nil || len(alerts) != 0 {
		t.Fatalf("missing history invented a bucket verdict: alerts %+v err %v", alerts, err)
	}
}
