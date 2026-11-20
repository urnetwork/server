// SIGNALS.md §25.5a reads the off-site hosted-proxy setup windows.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	hostedSetupHost     = "planetoid"
	hostedSetupSlow     = "hosted-proxy-setup-slow"
	hostedSetupUnknown  = "hosted-proxy-setup-unobservable"
	hostedSetupMaxBytes = 128 * 1024
	hostedSetupMaxAge   = 24 * time.Hour
	hostedSetupActivity = 10 * time.Minute
	hostedSetupFresh    = 2 * time.Minute
)

// Each family has one finite label schema in tests/proxy-test/metrics.go.
// Fluent Bit adds env, host and job. A range selector returns raw scrape clocks.
const hostedSetupQuery = `{env="main",host="planetoid",job="node",__name__=~"urnetwork_proxy_test_(up|heartbeat_timestamp_seconds|all_setup_window_(samples|seconds|timeouts|age_seconds)|lane_runs_total)",lane=~"|round_robin|us_probe",stat=~"|p90",outcome=~"|ready|ok|ready_timeout|browser_error|navigate_timeout|http_error|site_refused|load_timeout"}[10m]`

var hostedSetupLanes = []struct {
	name     string
	target   float64
	outcomes []string
}{
	{"round_robin", 7, []string{"ok", "ready_timeout", "browser_error", "navigate_timeout", "http_error", "site_refused", "load_timeout"}},
	{"us_probe", 3, []string{"ready", "ready_timeout"}},
}

// Read service properties together. Export only start and host wall clocks;
// bound commands and discard systemd and date diagnostics on the remote side.
const hostedSetupUnitCommand = `set -eu
export TZ=UTC LC_ALL=C
exec 2>/dev/null
fields=$(timeout 4s systemctl show --no-pager --property=LoadState,ActiveState,SubState,MainPID,ExecMainStartTimestamp proxy-test.service)
start= pid= loaded= active= sub=
while IFS='=' read -r name value; do
    case "$name" in
        LoadState) loaded=$value ;;
        ActiveState) active=$value ;;
        SubState) sub=$value ;;
        MainPID) pid=$value ;;
        ExecMainStartTimestamp) start=$value ;;
    esac
done <<UNIT
$fields
UNIT
[ "$loaded/$active/$sub" = "loaded/active/running" ]
case "$pid" in ''|*[!0-9]*) exit 1 ;; esac
[ "$pid" -gt 1 ] && [ -n "$start" ] || exit 1
started=$(date -u -d "$start" +%s)
observed=$(date -u +%s)
printf 'proxy-test-unit-v1 %s %s\n' "$started" "$observed"`

type hostedProxySetupProbe struct{}

func NewHostedProxySetupSignal() Signal {
	return &signalAdapter{number: "25.5a", key: "hosted-proxy-setup", name: "Off-site hosted proxy setup p90", probe: hostedProxySetupProbe{}}
}

func (hostedProxySetupProbe) id() string             { return "mimir/hosted-proxy-setup" }
func (hostedProxySetupProbe) tier() string           { return tierWarn }
func (hostedProxySetupProbe) cadence() time.Duration { return time.Minute }

func (self hostedProxySetupProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.cfg.env != "main" {
		return nil, nil
	}
	var observer *host
	for _, h := range env.cfg.scopeHosts() {
		if h.name != hostedSetupHost {
			continue
		}
		if scoped, ok := env.runner.(*hostScopeRunner); ok {
			if err := scoped.guardHost(ctx, h); err != nil {
				return nil, err
			}
		}
		if observer != nil || h.disabled {
			return self.findings(nil, 0, time.Time{}, "observer-inventory"), nil
		}
		observer = h
	}
	if observer == nil {
		return self.findings(nil, 0, time.Time{}, "observer-inventory"), nil
	}
	now := env.now().UTC().Truncate(time.Second)
	raw, err := readProviderMimirQuery(ctx, env, hostedSetupQuery, now, hostedSetupMaxBytes)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		findings := self.findings(nil, 0, now, "mimir-source")
		for i := range findings {
			if findings[i].class == hostedSetupUnknown {
				findings[i].observed += " " + observationFailureDetails(err, classifyObservationError(err)) + mimirQueryFailureDetails(err)
			}
		}
		return findings, nil
	}
	frame := parseHostedSetup(raw, now)
	if frame == nil {
		return self.findings(nil, 0, now, "invalid-source"), nil
	}
	unitReadStarted := env.now()
	unit, err := env.runner.shell(ctx, observer, hostedSetupUnitCommand)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	finished := env.now()
	started := hostedSetupStart(unit, unitReadStarted, finished)
	if err != nil || started == 0 || finished.Before(now) || finished.Sub(now) > time.Minute {
		return self.findings(nil, 0, now, "unit-or-clock-unavailable"), nil
	}
	return self.findings(frame, started, now, ""), nil
}

// Systemd renders start to whole seconds. Require the clock read after the
// metrics request to agree with the monitor's bounded local read interval.
func hostedSetupStart(raw string, begin, end time.Time) float64 {
	parts := strings.Fields(raw)
	if len(parts) != 3 || parts[0] != "proxy-test-unit-v1" || end.Before(begin) || end.Sub(begin) > time.Minute {
		return 0
	}
	start, err1 := strconv.ParseInt(parts[1], 10, 64)
	wall, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || start <= 0 || start > wall || wall < begin.Unix()-2 || wall > end.Unix()+2 {
		return 0
	}
	return float64(start)
}

type hostedSetupFrame map[string]map[string][]jwtRejectionsSample

func parseHostedSetup(raw string, now time.Time) hostedSetupFrame {
	var response struct {
		mimirRangeResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > hostedSetupMaxBytes || !providerConnectionsJsonShape(raw) || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Error != "" || response.Data.ResultType != "matrix" || len(response.Warnings)+len(response.Infos) != 0 || len(response.Data.Result) > 26 {
		return nil
	}
	frame := hostedSetupFrame{"": {}, "round_robin": {}, "us_probe": {}}
	for _, row := range response.Data.Result {
		m := row.Metric
		lane, key := m["lane"], strings.TrimPrefix(m["__name__"], "urnetwork_proxy_test_")
		if m["env"] != "main" || m["host"] != hostedSetupHost || m["job"] != "node" || m["__name__"] != "urnetwork_proxy_test_"+key || frame[lane] == nil {
			return nil
		}
		counter := false
		switch key {
		case "up", "heartbeat_timestamp_seconds":
			if lane != "" || len(m) != 4 {
				return nil
			}
		case "all_setup_window_samples", "all_setup_window_timeouts", "all_setup_window_age_seconds":
			if lane == "" || len(m) != 5 {
				return nil
			}
		case "all_setup_window_seconds":
			if lane == "" || len(m) != 6 || m["stat"] != "p90" {
				return nil
			}
		case "lane_runs_total":
			if lane == "" || len(m) != 6 {
				return nil
			}
			switch m["outcome"] {
			case "ok", "ready", "ready_timeout", "browser_error", "navigate_timeout", "http_error", "site_refused", "load_timeout":
			default:
				return nil
			}
			key = m["outcome"]
			counter = true
		default:
			return nil
		}
		if _, dup := frame[lane][key]; dup {
			frame[lane][key] = nil
			continue
		}
		points, ok := providerScoreReadPoints(row.Values, now, counter)
		if !ok || points[0].at <= float64(now.Add(-hostedSetupActivity).Unix()) {
			frame[lane][key] = nil
			continue
		}
		frame[lane][key] = points
	}
	if len(frame[""]["up"]) < 2 || len(frame[""]["heartbeat_timestamp_seconds"]) < 2 {
		return nil
	}
	return frame
}

func hostedSetupLast(points []jwtRejectionsSample, stamp float64) (float64, bool) {
	if len(points) == 0 || points[len(points)-1].at != stamp {
		return 0, false
	}
	return points[len(points)-1].value, true
}

type hostedSetupWindow struct{ p90, samples, timeouts, oldestAge float64 }

func (frame hostedSetupFrame) window(lane string, outcomes []string, started float64, now time.Time) (hostedSetupWindow, string) {
	var w hostedSetupWindow
	header, fields := frame[""], frame[lane]
	pulses := header["heartbeat_timestamp_seconds"]
	upPoints := header["up"]
	if len(pulses) < 2 || len(upPoints) != len(pulses) {
		return w, "writer-clock-missing"
	}
	last := pulses[len(pulses)-1]
	up, ok := hostedSetupLast(header["up"], last.at)
	end := float64(now.Unix())
	if !ok || up != 1 || last.at > end || last.value > last.at+2 || last.value > end || end-last.at > hostedSetupFresh.Seconds() || end-last.value > hostedSetupFresh.Seconds() || last.value <= started+1 {
		return w, "writer-stopped-or-stale"
	}
	for _, item := range []struct {
		field string
		dest  *float64
	}{{"all_setup_window_samples", &w.samples}, {"all_setup_window_timeouts", &w.timeouts}, {"all_setup_window_seconds", &w.p90}, {"all_setup_window_age_seconds", &w.oldestAge}} {
		*item.dest, ok = hostedSetupLast(fields[item.field], last.at)
		if !ok {
			return w, "unpaired-window"
		}
	}
	// The check's target is defined on a full 100 setups. The source retains
	// old runs indefinitely; an active publisher can keep re-exporting them.
	if w.samples != 100 || w.timeouts > w.samples || math.Trunc(w.timeouts) != w.timeouts || w.oldestAge > hostedSetupMaxAge.Seconds() || last.value-w.oldestAge <= started+1 {
		return w, "immature-old-or-previous-generation-window"
	}
	// The last 100 represent current evidence only after a measured setup
	// advanced during the ten-minute lookback. New writes and create errors
	// alone can continue indefinitely while the measurements stand still.

	for index, pulse := range pulses {
		if upPoints[index].at != pulse.at || upPoints[index].value != 1 || pulse.value > pulse.at+2 || pulse.at-pulse.value > hostedSetupFresh.Seconds() || pulse.value <= started+1 {
			return w, "unpaired-activity-clocks"
		}
		if index > 0 && pulse.value < pulses[index-1].value {
			return w, "source-reset"
		}
	}
	recent := false
	for _, outcome := range outcomes {
		points := fields[outcome]
		if len(points) != len(pulses) {
			return w, "missing-outcome-census"
		}
		for index, point := range points {
			pulse := pulses[index]
			if point.at != pulse.at {
				return w, "unpaired-activity-clocks"
			}
			if index == 0 {
				continue
			}
			previous := points[index-1]
			if point.value < previous.value || point.value != previous.value && pulse.value == pulses[index-1].value {
				return w, "source-reset"
			}
			if point.value > previous.value && pulse.value > pulses[index-1].value && pulses[index-1].value >= end-hostedSetupActivity.Seconds() {
				recent = true
			}
		}
	}
	if !recent {
		return w, "no-recent-measured-setup"
	}
	return w, ""
}

func (self hostedProxySetupProbe) findings(frame hostedSetupFrame, started float64, now time.Time, sourceReason string) []finding {
	var result []finding
	for _, lane := range hostedSetupLanes {
		target := hostedSetupHost + "/" + lane.name
		var w hostedSetupWindow
		reason := sourceReason
		if reason == "" {
			w, reason = frame.window(lane.name, lane.outcomes, started, now)
		}
		if reason != "" {
			result = append(result, finding{probeId: self.id(), tier: tierWarn, class: hostedSetupUnknown, target: target, sustain: 1,
				symptom: "The off-site runner cannot establish the current hosted setup percentile for this lane.", observed: "reason=" + reason,
				baseline: "A current proxy-test unit and fresh textfile supply one advancing 100-setup window per lane.",
				action:   "Inspect proxy-test.service, its measurement schedule and Planetoid's textfile publication to Mimir.", verify: "Require a fresh complete window from the current unit and a new measured setup.", playbook: "SIGNALS.md §25.5a"},
				stormClearHold(self.id(), hostedSetupSlow, target))
			continue
		}
		result = append(result, healthyFinding(self.id(), tierWarn, hostedSetupUnknown, target))
		if w.p90 <= lane.target {
			result = append(result, healthyFinding(self.id(), tierWarn, hostedSetupSlow, target))
			continue
		}
		result = append(result, finding{probeId: self.id(), tier: tierWarn, class: hostedSetupSlow, target: target, sustain: 1,
			symptom: "The off-site hosted proxy runner measured slow setup on this lane.", mechanism: "Setup measures time from the successful creation attempt to a parsed trace through the hosted proxy. A readiness timeout contributes its elapsed time.",
			baseline: fmt.Sprintf("The lane's nearest-rank p90 target is at most %.0f seconds across 100 measured setups.", lane.target),
			observed: fmt.Sprintf("p90_seconds=%.3f samples=100 ready_timeouts=%.0f oldest_seconds=%.3f evaluation_unix=%d", w.p90, w.timeouts, w.oldestAge, now.Unix()),
			context:  "Both lanes use one external observer. The window excludes failed creates and interrupted runs. The trace destination and the observer's own network can affect setup.",
			action:   "Compare hosted creation, ready timeouts, provider selection and the observer's connectivity for the same window.", verify: "Require a full advancing window inside the lane's setup target.", playbook: "SIGNALS.md §25.5a"})
	}
	return result
}
