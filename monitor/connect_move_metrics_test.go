package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Invented placement, instance nonces and values only; no production identity.
var (
	connectMoveTestHosts  = []string{"alpha-edge-1.example", "beta-edge-2.example"}
	connectMoveTestBlocks = []string{"blue", "green"}
)

func connectMoveTestNow() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }

// The instant every probe evaluates at: two minutes behind now.
func connectMoveTestAt() time.Time { return connectMoveTestNow().Add(-2 * time.Minute) }

// One synthetic Mimir answer, built row by row with the probe's tags.
type connectMoveTestAnswer struct {
	rows        []map[string]any
	evaluatedAt float64
	extra       map[string]any
}

func newConnectMoveTestAnswer() *connectMoveTestAnswer {
	return &connectMoveTestAnswer{evaluatedAt: float64(connectMoveTestAt().Unix())}
}

func (self *connectMoveTestAnswer) add(tag string, labels map[string]string, value float64) {
	metric := map[string]string{"monitor_move": tag}
	for name, labelValue := range labels {
		metric[name] = labelValue
	}
	self.rows = append(self.rows, map[string]any{"metric": metric, "value": []any{self.evaluatedAt, strconv.FormatFloat(value, 'g', -1, 64)}})
}

func (self *connectMoveTestAnswer) scalar(tag string, value float64) {
	self.add(tag, nil, value)
}

// The raw labels of a pushed connect series.
func connectMoveTestRaw(hostName, block, instance string) map[string]string {
	return map[string]string{"env": "synthetic", "job": "connect", "service": "connect", "host": hostName, "block": block, "instance": instance}
}

// A process's start and the receiver time of its newest push.
func (self *connectMoveTestAnswer) process(hostName, block, instance string, start time.Time, pushedAt time.Time) {
	self.add("start", connectMoveTestRaw(hostName, block, instance), float64(start.Unix()))
	self.add("start_time", connectMoveTestRaw(hostName, block, instance), float64(pushedAt.Unix()))
}

// The process series of every desired slot, pushed ten seconds before the
// evaluation instant, started two days earlier.
func (self *connectMoveTestAnswer) fleet() {
	for _, hostName := range connectMoveTestHosts {
		for _, block := range connectMoveTestBlocks {
			self.process(hostName, block, connectMoveTestInstance(hostName, block), connectMoveTestAt().Add(-48*time.Hour), connectMoveTestAt().Add(-10*time.Second))
		}
	}
}

func connectMoveTestInstance(hostName, block string) string {
	return "0a" + strconv.Itoa(len(hostName)) + block[:1] + "nonce"
}

func (self *connectMoveTestAnswer) json(t testing.TB) string {
	t.Helper()
	response := map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": self.rows}}
	for key, value := range self.extra {
		response[key] = value
	}
	out, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Answers the probe's one Mimir query and the mover's one control read, and
// records what each was asked. The query is routed by a tag only its probe
// asks for.
type connectMoveTestSource struct {
	syntheticSource
	answers       map[string]string
	control       []string
	controlErr    error
	queries       []string
	queryTimes    []string
	controlReads  [][]string
	queryErr      error
	cancelOnQuery context.CancelFunc
}

func newConnectMoveTestSource() *connectMoveTestSource {
	return &connectMoveTestSource{answers: map[string]string{}, control: []string{"mode", "on"}}
}

func (self *connectMoveTestSource) Host(_ context.Context, hostSettings HostSettings, command string) (string, error) {
	if hostSettings.Name != "metrics.example" {
		return "", fmt.Errorf("unexpected synthetic host")
	}
	extract := func(prefix string) string {
		start := strings.Index(command, "'"+prefix)
		if start < 0 {
			return ""
		}
		rest := command[start+1+len(prefix):]
		return rest[:strings.Index(rest, "'")]
	}
	query := extract("query=")
	self.queries = append(self.queries, query)
	self.queryTimes = append(self.queryTimes, extract("time="))
	if self.cancelOnQuery != nil {
		self.cancelOnQuery()
	}
	if self.queryErr != nil {
		return "", self.queryErr
	}
	for marker, answer := range self.answers {
		if strings.Contains(query, `"monitor_move","`+marker+`"`) {
			return answer, nil
		}
	}
	return "", fmt.Errorf("unexpected synthetic query")
}

func (self *connectMoveTestSource) Redis(_ context.Context, hostSettings HostSettings, port int, args ...string) (string, error) {
	self.controlReads = append(self.controlReads, append([]string{hostSettings.Name, strconv.Itoa(port)}, args...))
	if self.controlErr != nil {
		return "", self.controlErr
	}
	out, err := json.Marshal(self.control)
	return string(out), err
}

func connectMoveTestSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	now := connectMoveTestNow()
	settings.Now = func() time.Time { return now }
	settings.LogServices = []string{"connect"}
	settings.LogServiceBlocks = map[string][]string{"connect": connectMoveTestBlocks}
	settings.LogServiceHosts = map[string][]string{"connect": connectMoveTestHosts}
	settings.Hosts = []HostSettings{
		{Name: "metrics.example", Roles: []string{"services"}},
		{Name: "alpha-edge-1.example"},
		{Name: "beta-edge-2.example"},
		{Name: "redis.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6380, 6381, 6382}},
	}
	return settings
}

// The alert of a class, or a failure naming the classes present.
func connectMoveTestAlert(t *testing.T, alerts Alerts, class string) Alert {
	t.Helper()
	classes := []string{}
	for _, alert := range alerts {
		if alert.Class == class {
			return requireAlertClass(t, alerts, class)
		}
		classes = append(classes, alert.Class)
	}
	t.Fatalf("no alert class %q; classes=%v", class, classes)
	return Alert{}
}

func connectMoveTestClasses(alerts Alerts) []string {
	classes := []string{}
	for _, alert := range alerts {
		classes = append(classes, alert.Class)
	}
	return classes
}

func connectMoveTestNoClass(t *testing.T, alerts Alerts, classes ...string) {
	t.Helper()
	for _, alert := range alerts {
		for _, class := range classes {
			if alert.Class == class {
				t.Fatalf("unexpected alert class %q: %s", class, alert.Observed)
			}
		}
	}
}

// A standing state: reported, unknown, never posted, never an incident.
func connectMoveTestStandingState(t *testing.T, alert Alert) {
	t.Helper()
	if !alert.configuredOff || !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 1 || !isVisibilityAlertClass(alert.Class) {
		t.Fatalf("%s is not a configured-off standing state: configuredOff=%t unobservable=%t severity=%s sustain=%d", alert.Class, alert.configuredOff, alert.unobservable, alert.Severity, alert.Sustain)
	}
}

func TestConnectMoveSettingsDefaultsAreValidAndEveryBoundIsChecked(t *testing.T) {
	if !DefaultConnectMoveSettings().valid() {
		t.Fatal("default settings are invalid")
	}
	for name, mutate := range map[string]func(*ConnectMoveSettings){
		"zero window":          func(s *ConnectMoveSettings) { s.SuccessWindow = 0 },
		"fractional window":    func(s *ConnectMoveSettings) { s.ImbalanceWindow = 90*time.Minute + time.Millisecond },
		"window under step":    func(s *ConnectMoveSettings) { s.SuccessSustain = time.Minute },
		"hold under its step":  func(s *ConnectMoveSettings) { s.BreakerHold = 30 * time.Second },
		"share above one":      func(s *ConnectMoveSettings) { s.TargetFailureShare = 1.5 },
		"zero share":           func(s *ConnectMoveSettings) { s.SuccessRatioFloor = 0 },
		"ratio at one":         func(s *ConnectMoveSettings) { s.ImbalanceRatio = 1 },
		"zero minimum sends":   func(s *ConnectMoveSettings) { s.MinSends = 0 },
		"unknown default mode": func(s *ConnectMoveSettings) { s.DefaultMode = "On" },
		"negative filling":     func(s *ConnectMoveSettings) { s.FillingMinArrivalsPerMinute = -1 },
	} {
		settings := DefaultConnectMoveSettings()
		mutate(settings)
		if settings.valid() {
			t.Errorf("%s: invalid settings accepted", name)
		}
	}
	var unset *ConnectMoveSettings
	if unset.valid() {
		t.Fatal("nil settings accepted")
	}
}

func TestConnectMoveParseRejectsEveryUnqualifiedAnswer(t *testing.T) {
	settings := DefaultConnectMoveSettings()
	shape := connectMoveShape{"refused": {"cap"}, "points": {}}
	valid := func() *connectMoveTestAnswer {
		answer := newConnectMoveTestAnswer()
		answer.add("refused", map[string]string{"cap": "fleet"}, 3)
		answer.scalar("points", 24)
		return answer
	}
	if frame, reason := parseConnectMoveFrame(valid().json(t), connectMoveTestAt(), settings, shape); reason != "" || frame == nil {
		t.Fatalf("valid answer rejected: %s", reason)
	}
	bodies := map[string]string{
		"not json":        "{",
		"duplicate field": `{"status":"success","status":"error","data":{"resultType":"vector","result":[]}}`,
		"unknown field":   `{"status":"success","stats":{},"data":{"resultType":"vector","result":[]}}`,
		"error status":    `{"status":"error","data":{"resultType":"vector","result":[]}}`,
		"matrix":          `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
	}
	for name, body := range bodies {
		if frame, reason := parseConnectMoveFrame(body, connectMoveTestAt(), settings, shape); frame != nil || reason != "invalid_source_response" {
			t.Errorf("%s: reason=%q", name, reason)
		}
	}
	lastValue := func(answer *connectMoveTestAnswer, value string) {
		answer.rows[len(answer.rows)-1]["value"] = []any{answer.evaluatedAt, value}
	}
	mutations := map[string]struct {
		mutate func(*connectMoveTestAnswer)
		reason string
	}{
		"warning": {reason: "response_annotation_unqualified", mutate: func(a *connectMoveTestAnswer) {
			a.extra = map[string]any{"warnings": []string{"partial"}}
		}},
		"info": {reason: "response_annotation_unqualified", mutate: func(a *connectMoveTestAnswer) {
			a.extra = map[string]any{"infos": []string{"note"}}
		}},
		"unknown tag": {reason: "unknown_series_tag", mutate: func(a *connectMoveTestAnswer) {
			a.scalar("foreign", 1)
		}},
		"missing tag": {reason: "unknown_series_tag", mutate: func(a *connectMoveTestAnswer) {
			a.rows = append(a.rows, map[string]any{"metric": map[string]string{}, "value": []any{a.evaluatedAt, "1"}})
		}},
		"extra label": {reason: "invalid_series_labels", mutate: func(a *connectMoveTestAnswer) {
			a.add("points", map[string]string{"host": "alpha-edge-1.example"}, 1)
		}},
		"empty label value": {reason: "invalid_series_labels", mutate: func(a *connectMoveTestAnswer) {
			a.add("refused", map[string]string{"cap": ""}, 1)
		}},
		"wrong instant": {reason: "invalid_series_value", mutate: func(a *connectMoveTestAnswer) {
			a.evaluatedAt -= 60
			a.scalar("points", 1)
		}},
		"nan": {reason: "invalid_series_value", mutate: func(a *connectMoveTestAnswer) {
			a.add("refused", map[string]string{"cap": "source"}, 0)
			lastValue(a, "NaN")
		}},
		"infinity": {reason: "invalid_series_value", mutate: func(a *connectMoveTestAnswer) {
			a.add("refused", map[string]string{"cap": "source"}, 0)
			lastValue(a, "+Inf")
		}},
		"negative": {reason: "invalid_series_value", mutate: func(a *connectMoveTestAnswer) {
			a.add("refused", map[string]string{"cap": "target"}, -1)
		}},
		"duplicate series": {reason: "duplicate_series", mutate: func(a *connectMoveTestAnswer) {
			a.add("refused", map[string]string{"cap": "fleet"}, 4)
		}},
	}
	for name, test := range mutations {
		answer := valid()
		test.mutate(answer)
		frame, reason := parseConnectMoveFrame(answer.json(t), connectMoveTestAt(), settings, shape)
		if frame != nil || reason != test.reason {
			t.Errorf("%s: reason=%q, want %q", name, reason, test.reason)
		}
	}
	oversized := DefaultConnectMoveSettings()
	oversized.MaxResponseBytes = 16
	if _, reason := parseConnectMoveFrame(valid().json(t), connectMoveTestAt(), oversized, shape); reason != "invalid_source_response" {
		t.Fatalf("oversized answer: reason=%q", reason)
	}
	rowBound := DefaultConnectMoveSettings()
	rowBound.MaxRows = 1
	if _, reason := parseConnectMoveFrame(valid().json(t), connectMoveTestAt(), rowBound, shape); reason != "invalid_source_response" {
		t.Fatalf("answer over the row bound: reason=%q", reason)
	}
}

// A draining older process never stands in for a newer stale one, equal
// starts are ambiguous, and excluded or unconfigured hosts stay unknown.
func TestConnectMoveCensusSelectsTheNewestFreshProcessPerSlot(t *testing.T) {
	settings := DefaultConnectMoveSettings()
	at := connectMoveTestAt()
	scope := &connectMoveScope{hosts: connectMoveTestHosts, blocks: connectMoveTestBlocks, unknownHostReasons: map[string]string{}}
	frameOf := func(answer *connectMoveTestAnswer) *connectMoveFrame {
		frame, reason := parseConnectMoveFrame(answer.json(t), at, settings, connectMoveShape{"start": connectMoveRawProcessLabels, "start_time": connectMoveRawProcessLabels})
		if reason != "" {
			t.Fatal(reason)
		}
		return frame
	}
	answer := newConnectMoveTestAnswer()
	answer.fleet()
	// a draining older generation beside the current one
	answer.process("alpha-edge-1.example", "blue", "old", at.Add(-96*time.Hour), at.Add(-20*time.Second))
	census, reason := newConnectMoveCensus(frameOf(answer), "synthetic", scope, settings)
	if reason != "" || !census.complete() || census.slotProcesses[connectMoveSlot("alpha-edge-1.example", "blue")].instance == "old" {
		t.Fatalf("draining generation borrowed the slot: reason=%q coverage=%s", reason, census.coverage())
	}

	cases := map[string]func(*connectMoveTestAnswer){
		"stale newest": func(a *connectMoveTestAnswer) {
			a.process("alpha-edge-1.example", "blue", "newer", at.Add(-time.Hour), at.Add(-5*time.Minute))
		},
		"equal starts": func(a *connectMoveTestAnswer) {
			a.process("alpha-edge-1.example", "blue", "twin", at.Add(-48*time.Hour), at.Add(-10*time.Second))
		},
		"start after push": func(a *connectMoveTestAnswer) {
			a.process("alpha-edge-1.example", "blue", "future", at.Add(time.Hour), at.Add(-10*time.Second))
		},
	}
	for name, mutate := range cases {
		answer := newConnectMoveTestAnswer()
		answer.fleet()
		mutate(answer)
		census, reason := newConnectMoveCensus(frameOf(answer), "synthetic", scope, settings)
		if reason != "" || census.complete() || len(census.slotProcesses) != 3 {
			t.Errorf("%s: a slot without one fresh newest process was certified: reason=%q coverage=%s", name, reason, census.coverage())
		}
	}

	missing := newConnectMoveTestAnswer()
	missing.fleet()
	missing.rows = missing.rows[2:]
	census, reason = newConnectMoveCensus(frameOf(missing), "synthetic", scope, settings)
	if reason != "" || census.complete() || census.reasonCounts["missing"] != 1 {
		t.Fatalf("missing slot: reason=%q coverage=%s", reason, census.coverage())
	}

	excluded := &connectMoveScope{hosts: connectMoveTestHosts, blocks: connectMoveTestBlocks, unknownHostReasons: map[string]string{"beta-edge-2.example": "excluded"}}
	answer = newConnectMoveTestAnswer()
	answer.fleet()
	census, reason = newConnectMoveCensus(frameOf(answer), "synthetic", excluded, settings)
	if reason != "" || census.complete() || census.reasonCounts["excluded"] != 2 {
		t.Fatalf("an excluded host's series certified its slots: reason=%q coverage=%s", reason, census.coverage())
	}

	foreign := newConnectMoveTestAnswer()
	foreign.fleet()
	foreign.process("gamma-edge-3.example", "blue", "foreign", at.Add(-time.Hour), at.Add(-10*time.Second))
	if _, reason := newConnectMoveCensus(frameOf(foreign), "synthetic", scope, settings); reason != "invalid_process_identity" {
		t.Fatalf("a series outside the desired slots was accepted: reason=%q", reason)
	}
	companion := newConnectMoveTestAnswer()
	companion.fleet()
	companion.rows = companion.rows[:len(companion.rows)-1]
	if _, reason := newConnectMoveCensus(frameOf(companion), "synthetic", scope, settings); reason != "process_timestamp_companion_missing" {
		t.Fatalf("a start without its push time was accepted: reason=%q", reason)
	}
}

func TestConnectMoveScopeKeepsDesiredSlotsAndSkipsExcludedGateways(t *testing.T) {
	source := newConnectMoveTestSource()
	settings := connectMoveTestSettings(source)
	settings.Hosts = append([]HostSettings{{Name: "front.example", Roles: []string{"services"}}}, settings.Hosts...)
	settings.ExcludedHosts = []string{"front.example"}
	settings.LogServiceHosts = map[string][]string{"connect": {"beta-edge-2.example", "alpha-edge-1.example", "unknown-edge-9.example"}}
	env, err := newProbeEnv(settings.withDefaults().withRuntime())
	if err != nil {
		t.Fatal(err)
	}
	scope, reason := newConnectMoveScope(env, DefaultConnectMoveSettings())
	if reason != "" || scope.gateway == nil || scope.gateway.name != "metrics.example" {
		t.Fatalf("scope=%+v reason=%q", scope, reason)
	}
	if strings.Join(scope.hosts, ",") != "alpha-edge-1.example,beta-edge-2.example,unknown-edge-9.example" || scope.unknownHostReasons["unknown-edge-9.example"] != "inventory_unknown" {
		t.Fatalf("desired placement was not kept with its unknown host: hosts=%v reasons=%v", scope.hosts, scope.unknownHostReasons)
	}
	if selector := scope.selector("synthetic"); !strings.Contains(selector, `host=~"alpha-edge-1\\.example|beta-edge-2\\.example|unknown-edge-9\\.example"`) || !strings.Contains(selector, `job="connect",service="connect"`) {
		t.Fatalf("selector=%s", selector)
	}

	bounded := DefaultConnectMoveSettings()
	bounded.MaxSlots = 5
	if _, reason := newConnectMoveScope(env, bounded); reason != "slot_inventory_unavailable_or_over_bound" {
		t.Fatalf("over-bound placement: reason=%q", reason)
	}
	settings.LogServiceHosts = map[string][]string{"connect": {"alpha-edge-1.example", "alpha-edge-1.example"}}
	env, err = newProbeEnv(settings.withDefaults().withRuntime())
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := newConnectMoveScope(env, DefaultConnectMoveSettings()); reason != "slot_inventory_incomplete" {
		t.Fatalf("repeated host: reason=%q", reason)
	}
}
