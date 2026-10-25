package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

var connectCloseNow = time.Date(2026, 10, 13, 7, 14, 30, 0, time.UTC)

var connectCloseSlots = []fleetSlot{
	{service: "connect", host: "connect-a.example", block: "g1", allowed: true},
	{service: "connect", host: "connect-b.example", block: "g1", allowed: true},
}

// Per-minute history over the default lookback, keyed by part and labels.
type connectCloseFixture struct {
	steps  []int64
	series map[string]map[int64]float64
}

func connectCloseSteps() []int64 {
	settings := DefaultConnectCloseCohortsSettings()
	end := fleetEvaluationTime(connectCloseNow, settings.EvaluationLag).Truncate(settings.Step)
	steps := []int64{}
	for at := end.Add(-settings.Lookback); !at.After(end); at = at.Add(settings.Step) {
		steps = append(steps, at.Unix())
	}
	return steps
}

func connectCloseKey(part string, labels ...string) string {
	return part + "|" + strings.Join(labels, "|")
}

// A quiet fleet: about 350 websocket read_timeout closes a minute, steady
// release cohorts and long-running Connect processes.
func quietConnectCloseFixture() *connectCloseFixture {
	fixture := &connectCloseFixture{steps: connectCloseSteps(), series: map[string]map[int64]float64{}}
	set := func(key string, value func(int) float64) {
		fixture.series[key] = map[int64]float64{}
		for i, step := range fixture.steps {
			fixture.series[key][step] = value(i)
		}
	}
	set(connectCloseKey("closes", "carrier=websocket", "reason=read_timeout"), func(i int) float64 { return float64(340 + 5*(i%4)) })
	set(connectCloseKey("closes", "carrier=websocket", "reason=peer_close"), func(i int) float64 { return 200 })
	set(connectCloseKey("closes", "carrier=h1plus", "reason=read_timeout"), func(i int) float64 { return 30 })
	set(connectCloseKey("closes", "carrier=h1plus", "reason=peer_close"), func(i int) float64 { return 20 })
	for label, connections := range map[string]float64{"2026.9.9": 3000, "2026.3": 15000, "2026.7": 10000, "other": 1500, "unknown": 1000} {
		value := connections
		set(connectCloseKey("cohorts", "app_version="+label), func(int) float64 { return value })
	}
	for _, slot := range connectCloseSlots {
		identity := []string{"job=" + slot.service, "host=" + slot.host, "block=" + slot.block}
		for _, family := range []string{"start", "closes", "cohorts"} {
			set(connectCloseKey("fresh/"+family, identity...), func(i int) float64 { return float64(fixture.steps[i] - 15) })
		}
		set(connectCloseKey("newest_start", identity...), func(int) float64 { return float64(fixture.steps[0] - 7200) })
	}
	return fixture
}

func (self *connectCloseFixture) set(step int, key string, value float64) {
	self.series[key][self.steps[step]] = value
}

func (self *connectCloseFixture) json(t testing.TB) string {
	t.Helper()
	keys := []string{}
	for key := range self.series {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	results := []map[string]any{}
	for _, key := range keys {
		fields := strings.Split(key, "|")
		metric := map[string]string{fleetPartLabel: fields[0]}
		for _, field := range fields[1:] {
			name, value, _ := strings.Cut(field, "=")
			metric[name] = value
		}
		steps := []int64{}
		for step := range self.series[key] {
			steps = append(steps, step)
		}
		sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })
		values := []any{}
		for _, step := range steps {
			values = append(values, []any{float64(step), strconv.FormatFloat(self.series[key][step], 'f', -1, 64)})
		}
		results = append(results, map[string]any{"metric": metric, "values": values})
	}
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": results}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func connectCloseSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return connectCloseNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	hosts := []string{}
	for _, slot := range connectCloseSlots {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: slot.host, Roles: []string{"edge"}})
		hosts = append(hosts, slot.host)
	}
	settings.LogServices = []string{"connect"}
	settings.LogServiceHosts = map[string][]string{"connect": hosts}
	settings.LogServiceBlocks = map[string][]string{"connect": {"g1"}}
	return settings
}

func runConnectCloseFixture(t *testing.T, fixture *connectCloseFixture) (Alerts, []string) {
	t.Helper()
	commands := []string{}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			t.Fatalf("range read contacted a non-gateway host")
		}
		commands = append(commands, command)
		return fixture.json(t), nil
	}}
	alerts, err := NewConnectCloseCohortsSignal().Run(context.Background(), connectCloseSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts, commands
}

// The 2026-10-13 07:12Z shape: websocket read_timeout closes from the other
// bucket plus 76% of 2026.9.9, h1plus flat, no restart.
func cohortBurstFixture() *connectCloseFixture {
	fixture := quietConnectCloseFixture()
	fixture.set(10, connectCloseKey("closes", "carrier=websocket", "reason=read_timeout"), 3564)
	fixture.set(10, connectCloseKey("cohorts", "app_version=2026.9.9"), 720)
	fixture.set(10, connectCloseKey("cohorts", "app_version=other"), 300)
	return fixture
}

func TestConnectCloseCohortsNamesTheConfinedClientPattern(t *testing.T) {
	alerts, _ := runConnectCloseFixture(t, cohortBurstFixture())
	alert := requireAlertClass(t, alerts, "connect-close-burst-cohort")
	if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Frame != "2026.9.9" || alert.Target != "synthetic/connect-h1-closes" || alert.Sustain != 1 {
		t.Fatalf("cohort alerts = %+v", alerts)
	}
	burstMinute := time.Unix(connectCloseSteps()[10], 0).UTC().Format(time.RFC3339)
	for _, want := range []string{
		"burst_minute=" + burstMinute, "websocket/read_timeout=3564(median=", "h1plus/read_timeout=30",
		"struck_cohorts=2026.9.9:2280/3000,other:1200/1500", "rest_of_fleet_drop_share=0.000", "restarted_slots=0",
		"cohort_minutes=1 spread_minutes=0", "do not restart Connect",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("cohort alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "Check the LB, network and Connect path") {
		t.Fatal("the client pattern was sent to the server path")
	}
}

func TestConnectCloseCohortsQuietFleetIsHealthy(t *testing.T) {
	if alerts, _ := runConnectCloseFixture(t, quietConnectCloseFixture()); len(alerts) != 0 {
		t.Fatalf("quiet fleet alerted: %+v", alerts)
	}
	// a large but sub-floor rise is not a burst
	fixture := quietConnectCloseFixture()
	fixture.set(8, connectCloseKey("closes", "carrier=websocket", "reason=read_timeout"), 999)
	if alerts, _ := runConnectCloseFixture(t, fixture); len(alerts) != 0 {
		t.Fatalf("sub-floor rise alerted: %+v", alerts)
	}
}

// Each discriminator moves a burst off the client pattern.
func TestConnectCloseCohortsSpreadDiscriminators(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*connectCloseFixture)
		reason string
	}{
		{name: "h1plus burst too", mutate: func(f *connectCloseFixture) {
			f.set(10, connectCloseKey("closes", "carrier=h1plus", "reason=read_timeout"), 1500)
		}, reason: "carriers_spread"},
		{name: "h1plus alone", mutate: func(f *connectCloseFixture) {
			f.set(10, connectCloseKey("closes", "carrier=websocket", "reason=read_timeout"), 345)
			f.set(10, connectCloseKey("closes", "carrier=h1plus", "reason=read_timeout"), 1500)
		}, reason: "carriers_spread"},
		{name: "connect restarted", mutate: func(f *connectCloseFixture) {
			f.set(10, connectCloseKey("newest_start", "job=connect", "host=connect-b.example", "block=g1"), float64(f.steps[10]-30))
		}, reason: "connect_restarted"},
		{name: "every cohort dropped", mutate: func(f *connectCloseFixture) {
			for label, before := range map[string]float64{"2026.3": 15000, "2026.7": 10000, "unknown": 1000} {
				f.set(10, connectCloseKey("cohorts", "app_version="+label), 0.8*before)
			}
		}, reason: "drop_spread_across_cohorts"},
		{name: "no cohort dropped", mutate: func(f *connectCloseFixture) {
			f.set(10, connectCloseKey("cohorts", "app_version=2026.9.9"), 3000)
			f.set(10, connectCloseKey("cohorts", "app_version=other"), 1500)
		}, reason: "no_struck_cohort"},
	} {
		fixture := cohortBurstFixture()
		c.mutate(fixture)
		alerts, _ := runConnectCloseFixture(t, fixture)
		alert := requireAlertClass(t, alerts, "connect-close-burst-spread")
		if len(alerts) != 1 || alert.Frame != "" || !strings.Contains(alert.Observed, "spread_reasons="+c.reason) ||
			!strings.Contains(alert.Action, "Check the LB, network and Connect path") {
			t.Fatalf("%s: alerts = %+v", c.name, alerts)
		}
	}
}

func TestConnectCloseCohortsUnknownControls(t *testing.T) {
	for _, c := range []struct {
		name     string
		mutate   func(*connectCloseFixture)
		reason   string
		incident string
	}{
		{name: "missing slot", mutate: func(f *connectCloseFixture) {
			delete(f.series, connectCloseKey("fresh/cohorts", "job=connect", "host=connect-b.example", "block=g1"))
		}, reason: "slot_coverage_incomplete", incident: "connect-close-burst-cohort"},
		{name: "stale slot", mutate: func(f *connectCloseFixture) {
			f.set(len(f.steps)-1, connectCloseKey("fresh/start", "job=connect", "host=connect-a.example", "block=g1"), float64(f.steps[len(f.steps)-1]-600))
		}, reason: "slot_coverage_incomplete", incident: "connect-close-burst-cohort"},
		{name: "cohorts missing in the burst minute", mutate: func(f *connectCloseFixture) {
			for key, points := range f.series {
				if strings.HasPrefix(key, "cohorts|") {
					delete(points, f.steps[10])
				}
			}
		}, reason: "cohort_points_missing"},
		{name: "young series", mutate: func(f *connectCloseFixture) {
			key := connectCloseKey("closes", "carrier=h1plus", "reason=write_error")
			f.series[key] = map[int64]float64{f.steps[12]: 0, f.steps[13]: 0, f.steps[14]: 4000}
		}, reason: "baseline_short", incident: "connect-close-burst-cohort"},
		{name: "foreign cohort label", mutate: func(f *connectCloseFixture) {
			f.series[connectCloseKey("cohorts", "app_version=hostile-label")] = map[int64]float64{f.steps[0]: 1}
		}, reason: "unrecognized_cohort", incident: "connect-close-burst-cohort"},
		{name: "foreign carrier", mutate: func(f *connectCloseFixture) {
			f.series[connectCloseKey("closes", "carrier=quic", "reason=read_timeout")] = map[int64]float64{f.steps[0]: 1}
		}, reason: "unrecognized_close_label", incident: "connect-close-burst-cohort"},
		{name: "no close series", mutate: func(f *connectCloseFixture) {
			for key := range f.series {
				if strings.HasPrefix(key, "closes|") {
					delete(f.series, key)
				}
			}
		}, reason: "close_series_missing"},
	} {
		fixture := cohortBurstFixture()
		c.mutate(fixture)
		alerts, _ := runConnectCloseFixture(t, fixture)
		unknown := requireAlertClass(t, alerts, "connect-close-telemetry-unobservable")
		if !unknown.unobservable || !strings.Contains(unknown.Observed, c.reason) {
			t.Fatalf("%s: unknown alert lacks %s: %+v", c.name, c.reason, unknown)
		}
		incidents := 0
		if c.incident != "" {
			requireAlertClass(t, alerts, c.incident)
			incidents = 1
		}
		if len(alerts) != 1+incidents {
			t.Fatalf("%s: alerts = %+v", c.name, alerts)
		}
		requireAlertOmits(t, unknown, "hostile-label")
	}
}

func TestConnectCloseCohortsSourceFailuresAreUnknown(t *testing.T) {
	steps := connectCloseSteps()
	for _, c := range []struct {
		name    string
		payload string
		err     error
	}{
		{name: "transport", err: errors.New("synthetic gateway refused")},
		{name: "vector", payload: `{"status":"success","data":{"resultType":"vector","result":[]}}`},
		{name: "off step", payload: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"monitor_part":"closes","carrier":"websocket","reason":"read_timeout"},"values":[[` + strconv.FormatInt(steps[0]+7, 10) + `,"1"]]}]}}`},
		{name: "outside range", payload: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"monitor_part":"closes","carrier":"websocket","reason":"read_timeout"},"values":[[` + strconv.FormatInt(steps[len(steps)-1]+60, 10) + `,"1"]]}]}}`},
		{name: "repeated step", payload: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"monitor_part":"closes","carrier":"websocket","reason":"read_timeout"},"values":[[` + strconv.FormatInt(steps[0], 10) + `,"1"],[` + strconv.FormatInt(steps[0], 10) + `,"2"]]}]}}`},
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return c.payload, c.err }}
		alerts, err := NewConnectCloseCohortsSignal().Run(context.Background(), connectCloseSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		unknown := requireAlertClass(t, alerts, "connect-close-telemetry-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("%s: alerts = %+v", c.name, alerts)
		}
		requireAlertOmits(t, unknown, "synthetic gateway refused")
	}
}

// One bounded range read whose steps end behind now on a minute boundary.
func TestConnectCloseCohortsReadShape(t *testing.T) {
	_, commands := runConnectCloseFixture(t, quietConnectCloseFixture())
	if len(commands) != 1 {
		t.Fatalf("range read ran %d commands", len(commands))
	}
	steps := connectCloseSteps()
	for _, want := range []string{
		"/api/v1/query_range", "--max-time 15", "--max-filesize 4194304",
		"start=" + strconv.FormatInt(steps[0], 10), "end=" + strconv.FormatInt(steps[len(steps)-1], 10), "step=60",
		`sum by (carrier, reason) (increase(urnetwork_connect_h1_sessions_closed_total{env="synthetic",job="connect"}[60s]))`,
		`sum by (app_version) (urnetwork_connect_provider_connections{env="synthetic",job="connect"})`,
		`max by (job,host,block) (process_start_time_seconds{env="synthetic",job=~"connect"`,
	} {
		if !strings.Contains(commands[0], want) {
			t.Fatalf("range read lacks %q:\n%s", want, commands[0])
		}
	}
	if steps[len(steps)-1]%60 != 0 || connectCloseNow.Unix()-steps[len(steps)-1] < 120 {
		t.Fatal("range does not end behind now on a minute boundary")
	}
}

func TestConnectCloseCohortsCanceledParentPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	alerts, err := NewConnectCloseCohortsSignal().Run(ctx, connectCloseSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run = %+v, %v", alerts, err)
	}
}
