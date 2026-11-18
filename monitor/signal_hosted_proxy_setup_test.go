package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func hostedTestNow() time.Time { return time.Unix(1800000000, 0).UTC() }

func hostedTestFrame() hostedSetupFrame {
	frame := hostedSetupFrame{"": {}, "round_robin": {}, "us_probe": {}}
	now := float64(hostedTestNow().Unix())
	put := func(lane, key string, values ...float64) {
		clock := []float64{now - 300, now - 150, now - 10}
		for i, v := range values {
			frame[lane][key] = append(frame[lane][key], jwtRejectionsSample{at: clock[i], value: v})
		}
	}
	put("", "heartbeat_timestamp_seconds", now-301, now-151, now-11)
	put("", "up", 1, 1, 1)
	for _, lane := range hostedSetupLanes {
		put(lane.name, "all_setup_window_samples", 100, 100, 100)
		put(lane.name, "all_setup_window_timeouts", 1, 1, 1)
		put(lane.name, "all_setup_window_age_seconds", 18000, 18001, 18005)
		put(lane.name, "all_setup_window_seconds", lane.target, lane.target, lane.target)
		for _, outcome := range lane.outcomes {
			if outcome == "ok" || outcome == "ready" {
				put(lane.name, outcome, 200, 201, 202)
			} else {
				put(lane.name, outcome, 1, 1, 1)
			}
		}
	}
	return frame
}

func hostedTestRows(frame hostedSetupFrame) []map[string]any {
	var rows []map[string]any
	for lane, fields := range frame {
		for key, points := range fields {
			labels := map[string]string{"env": "main", "host": "planetoid", "job": "node", "__name__": "urnetwork_proxy_test_" + key}
			if lane != "" {
				labels["lane"] = lane
			}
			if key == "all_setup_window_seconds" {
				labels["stat"] = "p90"
			}
			if lane != "" && !strings.HasPrefix(key, "all_setup_") {
				labels["__name__"] = "urnetwork_proxy_test_lane_runs_total"
				labels["outcome"] = key
			}
			var values [][]any
			for _, point := range points {
				values = append(values, []any{point.at, strconv.FormatFloat(point.value, 'f', -1, 64)})
			}
			rows = append(rows, map[string]any{"metric": labels, "values": values})
		}
	}
	return rows
}

func hostedTestPayload(t testing.TB, rows []map[string]any, annotations []string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"status": "success", "infos": annotations, "data": map[string]any{"resultType": "matrix", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hostedTestState(t testing.TB, findings []finding, lane, slow, visibility string) {
	t.Helper()
	var states []string
	for _, f := range findings {
		if f.target != "planetoid/"+lane {
			continue
		}
		state := "firing"
		switch {
		case f.healthy:
			state = "healthy"
		case f.clearHold:
			state = "hold"
		}
		states = append(states, f.class+"/"+state)
		if f.class == hostedSetupSlow && state == "firing" && f.tier != tierWarn {
			t.Fatal("single observer severity")
		}
	}
	if len(states) != 2 || !strings.Contains(strings.Join(states, " "), hostedSetupSlow+"/"+slow) || !strings.Contains(strings.Join(states, " "), hostedSetupUnknown+"/"+visibility) {
		t.Fatalf("%s states %v", lane, states)
	}
}

func TestHostedProxySetupWindowAuthority(t *testing.T) {
	tests := []struct {
		name          string
		change        func(hostedSetupFrame)
		slow, unknown string
	}{
		{"target inclusive", func(hostedSetupFrame) {}, "healthy", "healthy"},
		{"above US target", func(f hostedSetupFrame) { f["us_probe"]["all_setup_window_seconds"][2].value = 3.001 }, "firing", "healthy"},
		{"missing sample cohort", func(f hostedSetupFrame) { delete(f["us_probe"], "all_setup_window_samples") }, "hold", "firing"},
		{"partial cohort", func(f hostedSetupFrame) { f["us_probe"]["all_setup_window_samples"][2].value = 99 }, "hold", "firing"},
		{"impossible number of timeouts", func(f hostedSetupFrame) { f["us_probe"]["all_setup_window_timeouts"][2].value = 101 }, "hold", "firing"},
		{"old retained cohort despite recent return", func(f hostedSetupFrame) { f["us_probe"]["all_setup_window_age_seconds"][2].value = 86400.001 }, "hold", "firing"},
		{"no recently returned setup despite new file", func(f hostedSetupFrame) {
			for i := range f["us_probe"]["ready"] {
				f["us_probe"]["ready"][i].value = 200
			}
		}, "hold", "firing"},
		{"counter reset", func(f hostedSetupFrame) { f["us_probe"]["ready_timeout"][1].value = 0 }, "hold", "firing"},
		{"one missing eager outcome", func(f hostedSetupFrame) { delete(f["us_probe"], "ready_timeout") }, "hold", "firing"},
		{"partial scrape", func(f hostedSetupFrame) { f["us_probe"]["all_setup_window_seconds"][2].at -= 1 }, "hold", "firing"},
		{"a timeout only is a measured setup", func(f hostedSetupFrame) {
			for i := range f["us_probe"]["ready"] {
				f["us_probe"]["ready"][i].value = 200
			}
			f["us_probe"]["ready_timeout"][2].value++
		}, "healthy", "healthy"},
	}
	now := hostedTestNow()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := hostedTestFrame()
			tc.change(frame)
			results := (hostedProxySetupProbe{}).findings(parseHostedSetup(hostedTestPayload(t, hostedTestRows(frame), nil), now), float64(now.Add(-48*time.Hour).Unix()), now, "")
			hostedTestState(t, results, "us_probe", tc.slow, tc.unknown)
			hostedTestState(t, results, "round_robin", "healthy", "healthy")
		})
	}
	f := hostedTestFrame()
	f["round_robin"]["all_setup_window_seconds"][2].value = 7.01
	hostedTestState(t, (hostedProxySetupProbe{}).findings(f, float64(now.Add(-48*time.Hour).Unix()), now, ""), "round_robin", "firing", "healthy")
}

func TestHostedProxySetupWriterAndFrameAuthority(t *testing.T) {
	now := hostedTestNow()
	start := float64(now.Add(-48 * time.Hour).Unix())
	for _, tc := range []struct {
		name  string
		fn    func(hostedSetupFrame)
		start float64
	}{
		{"stopped", func(f hostedSetupFrame) { f[""]["up"][2].value = 0 }, start},
		{"stale file repeatedly scraped", func(f hostedSetupFrame) {
			f[""]["heartbeat_timestamp_seconds"][2].value = float64(now.Add(-121 * time.Second).Unix())
		}, start},
		{"future writer", func(f hostedSetupFrame) {
			f[""]["heartbeat_timestamp_seconds"][2].value = float64(now.Add(time.Second).Unix())
		}, start},
		{"old window from former unit", func(hostedSetupFrame) {}, float64(now.Add(-time.Hour).Unix())},
		{"former unit file after restart", func(hostedSetupFrame) {}, float64(now.Add(-5 * time.Second).Unix())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hostedTestFrame()
			tc.fn(f)
			results := (hostedProxySetupProbe{}).findings(f, tc.start, now, "")
			for _, lane := range hostedSetupLanes {
				hostedTestState(t, results, lane.name, "hold", "firing")
			}
		})
	}
	rows := hostedTestRows(hostedTestFrame())
	if parseHostedSetup(hostedTestPayload(t, rows, []string{"private annotation"}), now) != nil {
		t.Fatal("partial envelope accepted")
	}
	if parseHostedSetup(strings.Replace(hostedTestPayload(t, rows, nil), `"status":"success"`, `"status":"success","status":"success"`, 1), now) != nil {
		t.Fatal("duplicate envelope accepted")
	}
	labels := rows[0]["metric"].(map[string]string)
	labels["host"] = "foreign"
	if parseHostedSetup(hostedTestPayload(t, rows, nil), now) != nil {
		t.Fatal("foreign host accepted")
	}
	labels["host"] = "planetoid"
	labels["unexpected"] = "private"
	if parseHostedSetup(hostedTestPayload(t, rows, nil), now) != nil {
		t.Fatal("unexpected labels accepted")
	}
}

func TestHostedProxySetupRegisteredRead(t *testing.T) {
	var signal Signal
	for _, candidate := range NewSignals() {
		if candidate.Key() == "hosted-proxy-setup" {
			signal = candidate
			break
		}
	}
	if signal == nil || signal.Number() != "25.5a" || !isVisibilityAlertClass(hostedSetupUnknown) || isVisibilityAlertClass(hostedSetupSlow) {
		t.Fatal("missing hosted setup signal or class")
	}
	now := hostedTestNow()
	f := hostedTestFrame()
	f["us_probe"]["all_setup_window_seconds"][2].value = 4
	payload := hostedTestPayload(t, hostedTestRows(f), nil)
	calls, units := 0, 0
	source := &providerHttpsTestSource{syntheticSource: &syntheticSource{}}
	source.do = func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "POST" || req.URL.Hostname() != "main-grafana.metrics.example" || req.URL.Path != "/prometheus/api/v1/query" || req.ParseForm() != nil || req.Form.Get("query") != hostedSetupQuery || req.Form.Get("time") != strconv.FormatInt(now.Unix(), 10) {
			t.Fatal("unbounded observer request")
		}
		return providerHttpsTestResponse(payload), nil
	}
	source.hostFn = func(h HostSettings, command string) (string, error) {
		units++
		if h.Name != hostedSetupHost || command != hostedSetupUnitCommand {
			t.Fatal("unexpected observer unit request")
		}
		return fmt.Sprintf("proxy-test-unit-v1 %d %d", now.Add(-48*time.Hour).Unix(), now.Unix()), nil
	}
	settings := syntheticSettings(source)
	settings.Environment = "main"
	settings.Now = hostedTestNow
	settings.Hosts = []HostSettings{{Name: hostedSetupHost}}
	providerHttpsTestCredentials(t, &settings)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || calls != 1 || units != 1 {
		t.Fatalf("registered read: alerts=%d http=%d unit=%d err=%v", len(alerts), calls, units, err)
	}
	alert := requireAlertClass(t, alerts, hostedSetupSlow)
	if alert.Target != "planetoid/us_probe" || !strings.Contains(alert.Observed, "p90_seconds=4.000") {
		t.Fatal("lost lane or value")
	}
	source.hostFn = func(HostSettings, string) (string, error) { return "private output", errors.New("private error") }
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatal("failed native observation did not retain both lane gaps")
	}
	for _, alert := range alerts {
		if alert.Class != hostedSetupUnknown {
			t.Fatal("unit error opened incident")
		}
		requireAlertOmits(t, alert, "private")
	}
	settings, err = ExcludeHosts(settings, hostedSetupHost)
	if err != nil {
		t.Fatal(err)
	}
	before := calls
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || calls != before {
		t.Fatal("excluded observer was read")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err = signal.Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != before {
		t.Fatal("cancellation was not terminal")
	}
}

func TestHostedProxySetupNativeUnit(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "systemctl")
	err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' 'LoadState=loaded' 'ActiveState=active' 'SubState=running' 'MainPID=55' 'ExecMainStartTimestamp=2026-01-01 00:00:00 UTC'\n"), 0700)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", hostedSetupUnitCommand)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
	begin := time.Now()
	out, err := cmd.Output()
	if err != nil || hostedSetupStart(string(out), begin, time.Now()) != float64(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()) {
		t.Fatal("native unit read lost its start or source clock")
	}
	for _, row := range []string{"private", "proxy-test-unit-v1 0 1", fmt.Sprintf("proxy-test-unit-v1 99 %d", begin.Unix()+50)} {
		if hostedSetupStart(row, begin, time.Now()) != 0 {
			t.Fatal("accepted invalid unit frame")
		}
	}
}

// The real range selector retains native scrape points, including header
// gauges with no lane, outcome or stat label.
func TestHostedProxySetupPromqlEngine(t *testing.T) {
	runCatalogPromqlScript(t, func() string {
		return fmt.Sprintf(`clear
load 1m
  urnetwork_proxy_test_up{env="main",job="node",host="planetoid"} 1+0x10
  urnetwork_proxy_test_all_setup_window_seconds{env="main",job="node",host="planetoid",lane="us_probe",stat="p90"} 3+0x10
  urnetwork_proxy_test_lane_runs_total{env="main",job="node",host="planetoid",lane="round_robin",outcome="ok"} 50+1x10
  urnetwork_proxy_test_lane_runs_total{env="main",job="node",host="planetoid",lane="round_robin",outcome="create_error"} 9+1x10
  urnetwork_proxy_test_all_setup_window_seconds{env="main",job="node",host="planetoid",lane="us_probe",stat="p50"} 1+0x10
  urnetwork_proxy_test_up{env="main",job="node",host="another-observer"} 1+0x10

eval instant at 10m sum(count_over_time(%s))
  {} 30
`, hostedSetupQuery)
	})
}
