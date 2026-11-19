// Synthetic current-stage evidence uses no network, credentials or wall-clock sleeps.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The two ingress cells and all source clocks belong to one fixed generation.
func contractStageInflightTestProcess(now time.Time, count float64) *contractStageInflightProcess {
	p := &contractStageInflightProcess{host: "proxy.example.test", block: "lane-a", instance: "generation-a", values: map[string]float64{}, sources: map[string]time.Time{}}
	for _, metric := range contractStageInflightMetrics {
		p.values[metric] = 0
		p.sources[metric] = now
	}
	p.values["process_start_time_seconds"] = float64(now.Add(-time.Hour).Unix())
	p.values["urnetwork_contract_creation_stage_timing_enabled"] = 1
	p.values["other_internal"] = count
	return p
}

// Keeps the expected slot authoritative while test cases mutate only the observation.
func contractStageInflightTestAssess(probe *contractStageInflightProbe, now time.Time, p *contractStageInflightProcess) []finding {
	return probe.assess(context.Background(), map[string]*contractStageInflightProcess{"process": p}, map[string]string{"proxy.example.test\x00lane-a": "proxy.example.test/lane-a"}, now)
}

// Fixed identifiers only; failed assertions do not dump observations.
func contractStageInflightTestHas(findings []finding, class string) bool {
	for _, f := range findings {
		if f.class == class && !f.clearHold {
			return true
		}
	}
	return false
}

// Two distinct source clocks see the backlog while no completed call is necessary.
func TestContractStageInflightDistinctClockMaturity(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
	p := contractStageInflightTestProcess(now, 1001)
	if contractStageInflightTestHas(contractStageInflightTestAssess(probe, now, p), contractStageInflightClass) {
		t.Fatal("first source observation matured")
	}
	later := now.Add(15 * time.Minute)
	q := contractStageInflightTestProcess(later, 1001)
	q.values["process_start_time_seconds"] = p.values["process_start_time_seconds"]
	rows := contractStageInflightTestAssess(probe, later, q)
	if !contractStageInflightTestHas(rows, contractStageInflightClass) {
		t.Fatal("second fresh same-generation observation did not mature")
	}
	for _, f := range rows {
		if f.class == contractStageInflightClass && (f.tier != tierWarn || f.pageSustain != 0 || f.sustain != 1) {
			t.Fatal("wrong source-mature WARN contract")
		}
	}
}

// A cached scrape and its next fresh successor cannot masquerade as consecutive evidence.
func TestContractStageInflightRepeatedClockCannotMature(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
	p := contractStageInflightTestProcess(now, 1001)
	contractStageInflightTestAssess(probe, now, p)
	rows := contractStageInflightTestAssess(probe, now, p)
	if !contractStageInflightTestHas(rows, "contract-stage-inflight-unobservable") || contractStageInflightTestHas(rows, contractStageInflightClass) {
		t.Fatal("repeated source became mature or complete")
	}
	q := contractStageInflightTestProcess(now.Add(time.Minute), 1001)
	q.values["process_start_time_seconds"] = p.values["process_start_time_seconds"]
	if contractStageInflightTestHas(contractStageInflightTestAssess(probe, now.Add(time.Minute), q), contractStageInflightClass) {
		t.Fatal("repeated-clock gap retained a high streak")
	}
}

// Missing, stale, invalid and replaced telemetry is observation loss, not a zero.
func TestContractStageInflightUnknownBoundaries(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	for _, kind := range []string{"missing-http", "missing-start-source", "stale", "unequal-clock", "capability-zero", "negative", "fractional", "nan", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
			p := contractStageInflightTestProcess(now, 1001)
			contractStageInflightTestAssess(probe, now, p)
			later := now.Add(time.Minute)
			q := contractStageInflightTestProcess(later, 1001)
			q.values["process_start_time_seconds"] = p.values["process_start_time_seconds"]
			switch kind {
			case "missing-http":
				delete(q.values, "other_http")
			case "missing-start-source":
				delete(q.sources, "process_start_time_seconds")
			case "stale":
				for metric := range q.sources {
					q.sources[metric] = now.Add(-time.Hour)
				}
			case "unequal-clock":
				q.sources["other_http"] = now
			case "capability-zero":
				q.values["urnetwork_contract_creation_stage_timing_enabled"] = 0
			case "negative":
				q.values["other_http"] = -1
			case "fractional":
				q.values["other_http"] = 0.5
			case "nan":
				q.values["other_http"] = math.NaN()
			case "replacement":
				q.instance = "generation-b"
				q.values["process_start_time_seconds"] = float64(now.Unix())
			}
			rows := contractStageInflightTestAssess(probe, later, q)
			if !contractStageInflightTestHas(rows, "contract-stage-inflight-unobservable") || contractStageInflightTestHas(rows, contractStageInflightClass) {
				t.Fatal("incomplete generation certified occupancy")
			}
		})
	}
}

// The strict boundary is >1000; two ingress cells sum within one process, never fleetwide.
func TestContractStageInflightThresholdAndIngressSum(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	for _, count := range []float64{0, 1000, 1001} {
		probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
		p := contractStageInflightTestProcess(now, count/2)
		p.values["other_internal"] = math.Floor(count / 2)
		p.values["other_http"] = math.Ceil(count / 2)
		contractStageInflightTestAssess(probe, now, p)
		for metric := range p.sources {
			p.sources[metric] = now.Add(time.Minute)
		}
		rows := contractStageInflightTestAssess(probe, now.Add(time.Minute), p)
		if contractStageInflightTestHas(rows, contractStageInflightClass) != (count > 1000) {
			t.Fatal("wrong strict per-process ingress sum boundary")
		}
	}
}

// A canceled evaluation cannot install a source baseline for a later Run.
func TestContractStageInflightCancellationNoCommit(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe.assess(ctx, map[string]*contractStageInflightProcess{"p": contractStageInflightTestProcess(now, 1001)}, map[string]string{"proxy.example.test\x00lane-a": "slot"}, now)
	if len(probe.previousKVs) != 0 {
		t.Fatal("canceled observation committed state")
	}
}

// A fixed vector is produced through the public constructor and bounded query adapter.
func TestContractStageInflightPublicRun(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	started := now.Add(-time.Hour)
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, "monitor-signal-26-contract-stage-inflight") || !strings.Contains(command, "--max-filesize 4194304") || !strings.Contains(command, "timestamp(") {
			return "", fmt.Errorf("unexpected fixed query")
		}
		p := contractStageInflightTestProcess(now, 1001)
		p.values["process_start_time_seconds"] = float64(started.Unix())
		rows := []any{}
		for _, metric := range contractStageInflightMetrics {
			for _, stamp := range []bool{false, true} {
				alias, value := metric, p.values[metric]
				if stamp {
					alias += contractStageInflightSource
					value = float64(now.Unix())
				}
				labels := map[string]string{"host": p.host, "block": p.block, "instance": p.instance, "monitor_metric": alias}
				if strings.HasPrefix(metric, "other_") {
					labels["stage"] = "other"
					labels["ingress"] = strings.TrimPrefix(metric, "other_")
				}
				rows = append(rows, map[string]any{"metric": labels, "value": []any{now.Unix(), fmt.Sprint(value)}})
			}
		}
		payload, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"lane-a"}}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "proxy.example.test", Proxy: &ProxyHostSettings{}}, HostSettings{Name: "metrics.example.test", Roles: []string{"services"}})
	signal := NewContractStageInflightSignal()
	first, err := signal.Run(context.Background(), settings)
	if err != nil || len(first) != 1 || first[0].Class != "contract-stage-inflight-unobservable" {
		t.Fatal("public warmup contract failed")
	}
	now = now.Add(15 * time.Minute)
	second, err := signal.Run(context.Background(), settings)
	if err != nil || len(second) != 1 || second[0].Class != contractStageInflightClass || second[0].Severity != Severity(tierWarn) {
		t.Fatal("public current occupancy was not detected")
	}
	encoded, _ := json.Marshal(second)
	var jsonl strings.Builder
	_ = second.WriteJSONL(&jsonl)
	if strings.Contains(second[0].Markdown()+string(encoded)+jsonl.String(), "generation-a") {
		t.Fatal("private process nonce entered public rendering")
	}
}

// Defaults do not silently disable; invalid overrides fail the public observation.
func TestContractStageInflightSettings(t *testing.T) {
	for _, value := range []*ContractStageInflightSettings{nil, {}, {WarnCalls: 25, Sustain: 3}} {
		signal := NewContractStageInflightSignalWithSettings(value).(*signalAdapter)
		p := signal.probe.(*contractStageInflightProbe)
		if p.settings.WarnCalls <= 0 || p.settings.Sustain < 2 || signal.Number() != "26" {
			t.Fatal("invalid default or custom policy")
		}
	}
}

// Reusable public-run fixture keeps private labels in the synthetic vector only.
func contractStageInflightPublicSettings(now func() time.Time, process func() *contractStageInflightProcess) SignalSettings {
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		p := process()
		rows := []any{}
		for _, metric := range contractStageInflightMetrics {
			value, present := p.values[metric]
			if !present {
				continue
			}
			for _, stamp := range []bool{false, true} {
				alias := metric
				sample := value
				if stamp {
					alias += contractStageInflightSource
					sample = float64(p.sources[metric].Unix())
				}
				labels := map[string]string{"host": p.host, "block": p.block, "instance": p.instance, "monitor_metric": alias}
				if strings.HasPrefix(metric, "other_") {
					labels["stage"] = "other"
					labels["ingress"] = strings.TrimPrefix(metric, "other_")
				}
				rows = append(rows, map[string]any{"metric": labels, "value": []any{now().Unix(), fmt.Sprint(sample)}})
			}
		}
		payload, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = now
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"lane-a"}}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "proxy.example.test", Proxy: &ProxyHostSettings{}}, HostSettings{Name: "metrics.example.test", Roles: []string{"services"}})
	return settings
}

// A mature original incident survives repeated clocks and replacement generations.
// Only fresh below-band full scope gives the normal complete-run recovery credit.
func TestContractStageInflightPublicNotifierRecovery(t *testing.T) {
	clock := newSlackTestHarness()
	started := clock.Now().Add(-time.Hour)
	phase := "high"
	lastSource := clock.Now()
	signal := NewContractStageInflightSignal()
	gate := newCadenceAlertGate()
	notifier := NewSlackAlertNotifier(SlackAlertNotifierSettings{Now: clock.Now, ClearAfterCompleteRuns: 2, Logf: func(string, ...any) {}})
	if notifier.enabled {
		t.Fatal("synthetic notifier enabled a sender")
	}
	settings := contractStageInflightPublicSettings(clock.Now, func() *contractStageInflightProcess {
		source := clock.Now()
		if phase == "repeated" {
			source = lastSource
		}
		p := contractStageInflightTestProcess(source, 1001)
		p.values["process_start_time_seconds"] = float64(started.Unix())
		if phase == "missing" {
			delete(p.values, "other_http")
		}
		if phase == "below" {
			p.values["other_internal"] = 0
		}
		if phase == "replacement" {
			p.instance = "generation-b"
			p.values["process_start_time_seconds"] = float64(started.Add(time.Minute).Unix())
		}
		lastSource = source
		return p
	})
	tick := func() Alerts {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("public notifier observation failed")
		}
		ready, held := gate.split(signal, alerts)
		notifier.stateLock.Lock()
		notifier.observeWithLock(clock.Now(), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		notifier.stateLock.Unlock()
		clock.advance(15 * time.Minute)
		return alerts
	}
	tick()
	alerts := tick()
	if len(alerts) != 1 || alerts[0].Class != contractStageInflightClass {
		t.Fatal("mature public incident unavailable")
	}
	seed := alerts[0]
	for _, next := range []string{"repeated", "missing", "replacement"} {
		phase = next
		tick()
		notifier.stateLock.Lock()
		incident := notifier.keyIncidents[slackIncidentKey(seed)]
		valid := incident != nil && incident.signalKeyAbsentRuns[signal.Key()] == 0
		notifier.stateLock.Unlock()
		if !valid {
			t.Fatal("unknown evidence advanced original incident absence")
		}
	}
	phase = "below"
	tick() // returning to the original generation is a replacement, not recovery
	tick() // first authoritative below-band absence
	notifier.stateLock.Lock()
	incident := notifier.keyIncidents[slackIncidentKey(seed)]
	kept := incident != nil
	notifier.stateLock.Unlock()
	if !kept {
		t.Fatal("one complete below-band run bypassed recovery hold")
	}
	tick()
	notifier.stateLock.Lock()
	incident = notifier.keyIncidents[slackIncidentKey(seed)]
	cleared := incident == nil
	notifier.stateLock.Unlock()
	if !cleared {
		t.Fatal("fresh complete below-band evidence did not permit standard recovery")
	}
}

// Same-slot ties and newer incomplete processes cannot be replaced by a draining healthy sample.
func TestContractStageInflightGenerationSelection(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	for _, kind := range []string{"equal-start", "newer-missing", "missing-start"} {
		t.Run(kind, func(t *testing.T) {
			p := contractStageInflightTestProcess(now, 0)
			q := contractStageInflightTestProcess(now, 1001)
			q.instance = "generation-b"
			if kind == "newer-missing" {
				q.values["process_start_time_seconds"] += 60
				delete(q.values, "other_http")
			}
			if kind == "missing-start" {
				delete(q.values, "process_start_time_seconds")
			}
			probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
			rows := probe.assess(context.Background(), map[string]*contractStageInflightProcess{"old": p, "new": q}, map[string]string{"proxy.example.test\x00lane-a": "proxy.example.test/lane-a"}, now)
			if !contractStageInflightTestHas(rows, "contract-stage-inflight-unobservable") {
				t.Fatal("ambiguous or incomplete replacement certified full coverage")
			}
		})
	}
}

// Invalid finite policy values fail before any observation adapter is invoked.
func TestContractStageInflightInvalidSettings(t *testing.T) {
	for _, settings := range []ContractStageInflightSettings{{WarnCalls: -1, Sustain: 2}, {WarnCalls: math.NaN(), Sustain: 2}, {WarnCalls: 1.5, Sustain: 2}, {WarnCalls: 1000, Sustain: 1}} {
		p := NewContractStageInflightSignalWithSettings(&settings).(*signalAdapter).probe.(*contractStageInflightProbe)
		if _, err := p.check(context.Background(), nil); err == nil {
			t.Fatal("invalid count policy was accepted")
		}
	}
}

// Invalid response details and synthetic private text cannot become public evidence.
func TestContractStageInflightParserFailClosed(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	expected := map[string]string{"proxy.example.test\x00lane-a": "slot"}
	for _, body := range []string{`{"status":"success","warnings":["synthetic-private-response"],"data":{"resultType":"vector","result":[]}}`, `{"status":"success","infos":["synthetic-private-response"],"data":{"resultType":"vector","result":[]}}`, `{"status":"error","error":"synthetic-private-response"}`, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_metric":"synthetic-private-label"},"value":[0,"1"]}]}}`, strings.Repeat("x", contractStageInflightMaxBytes+1)} {
		_, err := parseContractStageInflight(body, now, expected)
		if err == nil || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("invalid private response accepted or reflected")
		}
	}
}

// Complete independent slots retain a mature WARN beside a sibling visibility gap.
func TestContractStageInflightPartialScopeRetainsPositive(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
	p := contractStageInflightTestProcess(now, 1001)
	expected := map[string]string{"proxy.example.test\x00lane-a": "slot-a", "proxy.example.test\x00lane-b": "slot-b"}
	processes := map[string]*contractStageInflightProcess{"a": p}
	probe.assess(context.Background(), processes, expected, now)
	for metric := range p.sources {
		p.sources[metric] = now.Add(time.Minute)
	}
	rows := probe.assess(context.Background(), processes, expected, now.Add(time.Minute))
	if !contractStageInflightTestHas(rows, contractStageInflightClass) || !contractStageInflightTestHas(rows, "contract-stage-inflight-unobservable") {
		t.Fatal("missing sibling erased a complete positive or its coverage gap")
	}
}

// Process start times retain fractional seconds; only occupancy counts are integers.
func TestContractStageInflightFractionalProcessStart(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	probe := NewContractStageInflightSignal().(*signalAdapter).probe.(*contractStageInflightProbe)
	p := contractStageInflightTestProcess(now, 1001)
	p.values["process_start_time_seconds"] += 0.25
	contractStageInflightTestAssess(probe, now, p)
	for metric := range p.sources {
		p.sources[metric] = now.Add(time.Minute)
	}
	if !contractStageInflightTestHas(contractStageInflightTestAssess(probe, now.Add(time.Minute), p), contractStageInflightClass) {
		t.Fatal("valid fractional process start was rejected")
	}
}

// A first high source after one below-band run interrupts recovery without inventing maturity.
func TestContractStageInflightPublicWarmupClearHold(t *testing.T) {
	clock := newSlackTestHarness()
	started := clock.Now().Add(-time.Hour)
	count := 1001.0
	signal := NewContractStageInflightSignal()
	gate := newCadenceAlertGate()
	notifier := NewSlackAlertNotifier(SlackAlertNotifierSettings{Now: clock.Now, ClearAfterCompleteRuns: 2, Logf: func(string, ...any) {}})
	settings := contractStageInflightPublicSettings(clock.Now, func() *contractStageInflightProcess {
		p := contractStageInflightTestProcess(clock.Now(), count)
		p.values["process_start_time_seconds"] = float64(started.Unix())
		return p
	})
	tick := func() (Alerts, []SignalClassClearHold) {
		observation := &signalClearHoldObservation{}
		ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
		alerts, err := signal.Run(ctx, settings)
		if err != nil {
			t.Fatal("public clear-hold observation failed")
		}
		ready, held := gate.split(signal, alerts)
		holds := observation.snapshot(false)
		notifier.stateLock.Lock()
		notifier.observeWithLock(clock.Now(), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held, ClearHolds: holds})
		notifier.stateLock.Unlock()
		clock.advance(15 * time.Minute)
		return alerts, holds
	}
	tick()
	seedAlerts, _ := tick()
	if len(seedAlerts) != 1 || seedAlerts[0].Class != contractStageInflightClass {
		t.Fatal("clear-hold seed was not source-mature")
	}
	seed := seedAlerts[0]
	count = 0
	tick()
	notifier.stateLock.Lock()
	incident := notifier.keyIncidents[slackIncidentKey(seed)]
	oneAbsent := incident != nil && incident.signalKeyAbsentRuns[signal.Key()] == 1
	notifier.stateLock.Unlock()
	if !oneAbsent {
		t.Fatal("below-band run did not establish the recovery discriminator")
	}
	count = 1001
	alerts, holds := tick()
	if len(alerts) != 0 || len(holds) != 1 || holds[0].Class != contractStageInflightClass {
		t.Fatal("first high failed to silently hold its original class")
	}
	notifier.stateLock.Lock()
	incident = notifier.keyIncidents[slackIncidentKey(seed)]
	retained := incident != nil && incident.signalKeyAbsentRuns[signal.Key()] == 0
	notifier.stateLock.Unlock()
	if !retained {
		t.Fatal("first high completed a false recovery")
	}
}
