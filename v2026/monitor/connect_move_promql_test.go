package monitor

// The connect placement probes' exact PromQL (SIGNALS.md §24) runs against raw
// synthetic series in the pinned Mimir engine; its native vectors then enter
// the probes through their public adapters. No tagged value is fabricated: a
// subquery, join, gate or exact count that evaluates otherwise than the
// probes assume fails here.

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const connectMovePromqlEngineFixture = "testdata/connect_move_promql_engine_test.go.txt"

type connectMoveEnginePoint struct {
	Seconds int64   `json:"seconds"`
	Value   float64 `json:"value"`
}

type connectMoveEngineSeries struct {
	Labels map[string]string        `json:"labels"`
	Points []connectMoveEnginePoint `json:"points"`
}

// One raw-series case and what the probe must report from its native vector.
type connectMoveEngineCase struct {
	Name       string                    `json:"name"`
	Query      string                    `json:"query"`
	Evaluation int64                     `json:"evaluation"`
	Series     []connectMoveEngineSeries `json:"series"`
	signal     func() Signal
	marker     string
	control    []string
	want       []string
	forbid     []string
}

// Pushed series of one synthetic fleet, every 15 s over the case's history.
type connectMoveEngineFleet struct {
	from   int64
	to     int64
	series []connectMoveEngineSeries
}

func newConnectMoveEngineFleet(history time.Duration) *connectMoveEngineFleet {
	at := connectMoveTestAt().Unix()
	return &connectMoveEngineFleet{from: at - int64(history/time.Second), to: at - 10}
}

// Adds one series; value reports the sample at a push time, if any.
func (self *connectMoveEngineFleet) add(labels map[string]string, value func(at int64) (float64, bool)) {
	series := connectMoveEngineSeries{Labels: labels}
	for at := self.from; at <= self.to; at += 15 {
		if sample, present := value(at); present {
			series.Points = append(series.Points, connectMoveEnginePoint{Seconds: at, Value: sample})
		}
	}
	if len(series.Points) != 0 {
		self.series = append(self.series, series)
	}
}

// Adds one series of a connect process, with its metric name and extra labels.
func (self *connectMoveEngineFleet) process(hostName, block, instance, metric string, extra map[string]string, value func(at int64) (float64, bool)) {
	labels := connectMoveTestRaw(hostName, block, instance)
	labels["__name__"] = metric
	for name, labelValue := range extra {
		labels[name] = labelValue
	}
	self.add(labels, value)
}

func connectMoveEngineConstant(value float64) func(int64) (float64, bool) {
	return func(int64) (float64, bool) { return value, true }
}

// A counter rising at a rate per minute from a start time, zero before it.
func connectMoveEngineCounter(perMinute float64, since int64) func(int64) (float64, bool) {
	return func(at int64) (float64, bool) {
		if at < since {
			return 0, true
		}
		return float64(int64(perMinute*float64(at-since)/60 + 0.5)), true
	}
}

// Every desired slot's process start, with a mode and a tick rate.
func (self *connectMoveEngineFleet) movers(mode func(int64) (float64, bool), outcome string) {
	for _, hostName := range connectMoveTestHosts {
		for _, block := range connectMoveTestBlocks {
			instance := connectMoveTestInstance(hostName, block)
			self.process(hostName, block, instance, "process_start_time_seconds", nil, connectMoveEngineConstant(float64(self.from-3600)))
			self.process(hostName, block, instance, "urnetwork_connect_mover_mode", nil, mode)
			for _, tickOutcome := range moverTickOutcomes {
				rate := 0.0
				if tickOutcome == outcome {
					rate = 6
				}
				self.process(hostName, block, instance, "urnetwork_connect_mover_ticks_total", map[string]string{"outcome": tickOutcome}, connectMoveEngineCounter(rate, self.from))
			}
			self.process(hostName, block, instance, "urnetwork_connect_move_send_errors_total", map[string]string{"kind": "advisory", "trigger": "balance"}, connectMoveEngineConstant(0))
			for _, refusedCap := range []string{"fleet", "source", "target"} {
				self.process(hostName, block, instance, "urnetwork_connect_move_budget_refused_total", map[string]string{"cap": refusedCap}, connectMoveEngineConstant(0))
			}
			self.process(hostName, block, instance, "urnetwork_connect_move_host_excess", nil, connectMoveEngineConstant(0))
		}
	}
}

// Each host's normalized load, pushed by both of its blocks.
func (self *connectMoveEngineFleet) loads(hostLoads map[string]func(int64) (float64, bool)) {
	for hostName, load := range hostLoads {
		for _, block := range connectMoveTestBlocks {
			self.process(hostName, block, connectMoveTestInstance(hostName, block), "urnetwork_connect_move_host_normalized_load", nil, load)
		}
	}
}

// The mover's sends from the first host's processes to the second host, and
// the arrivals at the second host's processes.
func (self *connectMoveEngineFleet) moves(sendsPerMinute, arrivalsPerMinute float64) {
	source, target := connectMoveTestHosts[0], connectMoveTestHosts[1]
	for _, block := range connectMoveTestBlocks {
		self.process(source, block, connectMoveTestInstance(source, block), "urnetwork_connect_moves_sent_total",
			map[string]string{"kind": "advisory", "trigger": "balance", "source_host": source, "target_host": target}, connectMoveEngineCounter(sendsPerMinute/2, self.from))
		self.process(target, block, connectMoveTestInstance(target, block), "urnetwork_connect_moves_arrived_total",
			map[string]string{"kind": "advisory", "target_host": target}, connectMoveEngineCounter(arrivalsPerMinute/2, self.from))
	}
}

func connectMoveEngineCases(t *testing.T) []*connectMoveEngineCase {
	t.Helper()
	at := connectMoveTestAt().Unix()
	on, off := connectMoveEngineConstant(2), connectMoveEngineConstant(0)
	steady := func(load float64) func(int64) (float64, bool) { return connectMoveEngineConstant(load) }
	cases := []*connectMoveEngineCase{}
	mover := func(name string, want []string, forbid []string, build func(*connectMoveEngineFleet)) {
		fleet := newConnectMoveEngineFleet(3 * time.Hour)
		build(fleet)
		cases = append(cases, &connectMoveEngineCase{Name: name, Series: fleet.series, signal: NewMoverSignal, marker: "imbalance_min", control: []string{"mode", "on"}, want: want, forbid: forbid})
	}
	mover("mover-balanced-on", nil, nil, func(f *connectMoveEngineFleet) {
		f.movers(on, "in_band")
		f.loads(map[string]func(int64) (float64, bool){"alpha-edge-1.example": steady(110), "beta-edge-2.example": steady(95)})
	})
	// false-positive qualifier: a host mid-deploy reads light for minutes, and
	// cannot hold the ratio high for the whole window alone
	mover("mover-host-mid-deploy-reads-light", nil, nil, func(f *connectMoveEngineFleet) {
		f.movers(on, "in_band")
		f.loads(map[string]func(int64) (float64, bool){
			"alpha-edge-1.example": func(sample int64) (float64, bool) {
				if at-3600 <= sample && sample < at-3000 {
					return 20, true
				}
				return 110, true
			},
			"beta-edge-2.example": steady(95),
		})
	})
	mover("mover-imbalanced-on", []string{"move-host-imbalance"}, []string{"move-host-filling", "mover-warming"}, func(f *connectMoveEngineFleet) {
		f.movers(on, "no_target")
		f.loads(map[string]func(int64) (float64, bool){"alpha-edge-1.example": steady(300), "beta-edge-2.example": steady(100)})
	})
	mover("mover-turned-on-half-an-hour-ago", []string{"mover-warming"}, []string{"move-host-imbalance"}, func(f *connectMoveEngineFleet) {
		f.movers(func(sample int64) (float64, bool) {
			if sample < at-1800 {
				return 0, true
			}
			return 2, true
		}, "no_target")
		f.loads(map[string]func(int64) (float64, bool){"alpha-edge-1.example": steady(300), "beta-edge-2.example": steady(100)})
	})
	mover("mover-off", []string{"mover-off"}, []string{"move-host-imbalance", "mover-mode-divergent"}, func(f *connectMoveEngineFleet) {
		f.movers(off, "off")
		f.loads(map[string]func(int64) (float64, bool){"alpha-edge-1.example": steady(500), "beta-edge-2.example": steady(100)})
	})
	// the shipped default: no control at all
	cases[len(cases)-1].control = []string{}
	mover("mover-off-under-an-on-control", []string{"mover-mode-divergent", "mover-off"}, []string{"move-host-imbalance"}, func(f *connectMoveEngineFleet) {
		f.movers(off, "store_error")
		f.loads(map[string]func(int64) (float64, bool){"alpha-edge-1.example": steady(500), "beta-edge-2.example": steady(100)})
	})
	mover("mover-load-publish-gap", []string{"move-host-load-incomplete"}, []string{"move-host-imbalance"}, func(f *connectMoveEngineFleet) {
		f.movers(on, "no_target")
		f.loads(map[string]func(int64) (float64, bool){
			"alpha-edge-1.example": steady(300),
			"beta-edge-2.example": func(sample int64) (float64, bool) {
				return 100, sample < at-3600 || at-2400 < sample
			},
		})
	})
	mover("mover-filling-new-host", []string{"move-host-filling"}, []string{"move-host-imbalance"}, func(f *connectMoveEngineFleet) {
		f.movers(on, "budget_refused")
		f.loads(map[string]func(int64) (float64, bool){
			"alpha-edge-1.example": steady(400),
			"beta-edge-2.example": func(sample int64) (float64, bool) {
				return 5 + 45*float64(sample-f.from)/float64(f.to-f.from), true
			},
		})
		f.moves(40, 20)
	})

	outcomes := func(name string, want []string, forbid []string, build func(*connectMoveEngineFleet)) {
		fleet := newConnectMoveEngineFleet(3 * time.Hour)
		fleet.movers(on, "moved")
		build(fleet)
		cases = append(cases, &connectMoveEngineCase{Name: name, Series: fleet.series, signal: NewMoveOutcomesSignal, marker: "success_max", want: want, forbid: forbid})
	}
	failures := func(f *connectMoveEngineFleet, perMinute float64) {
		// reports land on an api process: the counter carries no service filter
		f.add(map[string]string{"__name__": "urnetwork_connect_moves_failed_total", "env": "synthetic", "job": "api", "service": "api", "host": "api-1.example", "block": "blue", "instance": "api1nonce", "reason": "connect_timeout", "target_host": "beta-edge-2.example"},
			connectMoveEngineCounter(perMinute, f.from))
	}
	outcomes("outcomes-healthy", nil, nil, func(f *connectMoveEngineFleet) { f.moves(10, 7) })
	outcomes("outcomes-success-low", []string{"move-success-low"}, nil, func(f *connectMoveEngineFleet) { f.moves(10, 2) })
	outcomes("outcomes-target-failures", []string{"move-target-failures"}, nil, func(f *connectMoveEngineFleet) {
		f.moves(10, 6)
		failures(f, 3)
	})
	outcomes("outcomes-target-failures-explained-by-a-drain", nil, []string{"move-target-failures"}, func(f *connectMoveEngineFleet) {
		f.moves(10, 6)
		failures(f, 3)
		for _, block := range connectMoveTestBlocks {
			f.process("beta-edge-2.example", block, connectMoveTestInstance("beta-edge-2.example", block), "urnetwork_connect_drain_moves_sent_total", map[string]string{"capable": "true"}, connectMoveEngineCounter(2, f.from))
		}
	})
	outcomes("outcomes-breakers-held-open", []string{"move-target-breakers-open"}, nil, func(f *connectMoveEngineFleet) {
		f.moves(10, 7)
		for _, family := range []string{"4", "6"} {
			f.process("alpha-edge-1.example", "blue", connectMoveTestInstance("alpha-edge-1.example", "blue"), "urnetwork_connect_move_target_breaker_open",
				map[string]string{"target_host": "beta-edge-2.example", "interface": "eno1np0", "family": family}, connectMoveEngineConstant(1))
		}
	})
	outcomes("outcomes-breaker-without-target-host", []string{"move-outcomes-telemetry-incomplete"}, []string{"move-target-breakers-open"}, func(f *connectMoveEngineFleet) {
		f.moves(10, 7)
		f.process("alpha-edge-1.example", "blue", connectMoveTestInstance("alpha-edge-1.example", "blue"), "urnetwork_connect_move_target_breaker_open",
			map[string]string{"interface": "eno1np0", "family": "4"}, connectMoveEngineConstant(1))
	})

	drains := func(name string, want []string, forbid []string, build func(*connectMoveEngineFleet)) {
		fleet := newConnectMoveEngineFleet(25 * time.Hour)
		for _, hostName := range connectMoveTestHosts {
			for _, block := range connectMoveTestBlocks {
				instance := connectMoveTestInstance(hostName, block)
				fleet.process(hostName, block, instance, "process_start_time_seconds", nil, connectMoveEngineConstant(float64(fleet.from-3600)))
				fleet.process(hostName, block, instance, "urnetwork_connect_moves_compulsory_closed_total", nil, connectMoveEngineConstant(0))
				for _, capable := range []string{"true", "false"} {
					fleet.process(hostName, block, instance, "urnetwork_connect_drain_moves_sent_total", map[string]string{"capable": capable}, connectMoveEngineConstant(0))
				}
				// the last slot's gates and drain gauge are the case's
				if !(hostName == "beta-edge-2.example" && block == "green") {
					fleet.process(hostName, block, instance, "urnetwork_connect_drain_residents_remaining", nil, connectMoveEngineConstant(0))
					for _, carrier := range drainRefusalCarriers {
						fleet.process(hostName, block, instance, "urnetwork_connect_drain_refusals_total", map[string]string{"carrier": carrier}, connectMoveEngineConstant(0))
					}
				}
			}
		}
		build(fleet)
		cases = append(cases, &connectMoveEngineCase{Name: name, Series: fleet.series, signal: NewDrainMovesSignal, marker: "mover_compulsory", want: want, forbid: forbid})
	}
	// the last slot refuses for the last 50 minutes; its drain gauge reads
	// drainRemaining for the drain's 20 minutes before the evaluation
	refusals := func(f *connectMoveEngineFleet, drainRemaining float64) {
		instance := connectMoveTestInstance("beta-edge-2.example", "green")
		for _, carrier := range drainRefusalCarriers {
			f.process("beta-edge-2.example", "green", instance, "urnetwork_connect_drain_refusals_total", map[string]string{"carrier": carrier}, connectMoveEngineCounter(4, at-3000))
		}
		f.process("beta-edge-2.example", "green", instance, "urnetwork_connect_drain_residents_remaining", nil, func(sample int64) (float64, bool) {
			if at-1200 <= sample {
				return drainRemaining, true
			}
			return 0, true
		})
	}
	drains("drain-refusals-with-the-gauge-at-zero", []string{"drain-refusals-outside-drain"}, nil, func(f *connectMoveEngineFleet) { refusals(f, 0) })
	drains("drain-refusals-during-a-drain", nil, []string{"drain-refusals-outside-drain"}, func(f *connectMoveEngineFleet) { refusals(f, 900) })
	// a drained process that exited five hours ago keeps its counters in the
	// window; max less min counts its drain exactly
	drained := func(f *connectMoveEngineFleet, closes float64) {
		// Refusal cases fill this intentionally omitted current slot themselves.
		// Historical-drain cases instead need its complete quiet baseline;
		// the exited process below cannot supply current-process coverage.
		currentInstance := connectMoveTestInstance("beta-edge-2.example", "green")
		f.process("beta-edge-2.example", "green", currentInstance, "urnetwork_connect_drain_residents_remaining", nil, connectMoveEngineConstant(0))
		for _, carrier := range drainRefusalCarriers {
			f.process("beta-edge-2.example", "green", currentInstance, "urnetwork_connect_drain_refusals_total", map[string]string{"carrier": carrier}, connectMoveEngineConstant(0))
		}
		instance := "drained-alpha-blue"
		ended := at - 5*3600
		drainStart := ended - 600
		alive := func(value func(int64) float64) func(int64) (float64, bool) {
			return func(sample int64) (float64, bool) { return value(sample), sample <= ended }
		}
		step := func(total float64) func(int64) float64 {
			return func(sample int64) float64 {
				if sample < drainStart {
					return 0
				}
				return float64(int64(total*float64(min(sample, ended)-drainStart)/float64(ended-drainStart) + 0.5))
			}
		}
		f.process("alpha-edge-1.example", "blue", instance, "process_start_time_seconds", nil, alive(func(int64) float64 { return float64(f.from - 7200) }))
		f.process("alpha-edge-1.example", "blue", instance, "urnetwork_connect_drain_residents_remaining", nil, alive(func(sample int64) float64 {
			if sample < drainStart {
				return 0
			}
			return 1500
		}))
		f.process("alpha-edge-1.example", "blue", instance, "urnetwork_connect_moves_compulsory_closed_total", nil, alive(step(closes)))
		f.process("alpha-edge-1.example", "blue", instance, "urnetwork_connect_drain_moves_sent_total", map[string]string{"capable": "true"}, alive(step(1200)))
		f.process("alpha-edge-1.example", "blue", instance, "urnetwork_connect_drain_moves_sent_total", map[string]string{"capable": "false"}, alive(step(300)))
	}
	drains("drain-closes-above-the-legacy-residents", []string{"drain-closes-excess"}, nil, func(f *connectMoveEngineFleet) { drained(f, 330) })
	drains("drain-closes-within-the-legacy-residents", nil, []string{"drain-closes-excess"}, func(f *connectMoveEngineFleet) { drained(f, 250) })
	return cases
}

// Each case's exact production query, captured from the probe itself.
func connectMoveEngineQuery(t *testing.T, engineCase *connectMoveEngineCase) string {
	t.Helper()
	source := newConnectMoveTestSource()
	if engineCase.control != nil {
		source.control = engineCase.control
	}
	source.queryErr = errors.New("synthetic capture")
	if _, err := engineCase.signal().Run(context.Background(), connectMoveTestSettings(source)); err != nil {
		t.Fatal(err)
	}
	if len(source.queries) != 1 {
		t.Fatalf("%s: captured %d queries", engineCase.Name, len(source.queries))
	}
	return source.queries[0]
}

// Required as a release gate for SIGNALS.md §24: point
// MONITOR_PROMQL_ENGINE_SOURCE at the pinned Mimir source with its vendored
// Prometheus engine. A skip is not a semantic pass.
func TestConnectMovePromqlEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("set MONITOR_PROMQL_ENGINE_SOURCE to run the required pinned-engine regression; a skip is not a semantic pass")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	cases := connectMoveEngineCases(t)
	for _, engineCase := range cases {
		engineCase.Query = connectMoveEngineQuery(t, engineCase)
		engineCase.Evaluation = connectMoveTestAt().Unix()
	}
	request, err := json.Marshal(struct {
		Cases []*connectMoveEngineCase `json:"cases"`
	}{Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	requestPath, outputPath := filepath.Join(directory, "request.json"), filepath.Join(directory, "vectors.json")
	if err := os.WriteFile(requestPath, request, 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(connectMovePromqlEngineFixture)
	if err != nil {
		t.Fatal(err)
	}
	engineTest := filepath.Join(directory, "connect_move_promql_engine_test.go")
	if err := os.WriteFile(engineTest, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestConnectMoveNativeVectors$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "MONITOR_MOVE_ENGINE_REQUEST="+requestPath, "MONITOR_MOVE_ENGINE_OUTPUT="+outputPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned PromQL engine regression: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	vectors := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(cases) {
		t.Fatal("native engine omitted cases")
	}
	for _, engineCase := range cases {
		source := newConnectMoveTestSource()
		if engineCase.control != nil {
			source.control = engineCase.control
		}
		source.answers[engineCase.marker] = string(vectors[engineCase.Name])
		alerts, err := engineCase.signal().Run(context.Background(), connectMoveTestSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", engineCase.Name, err)
		}
		classes := connectMoveTestClasses(alerts)
		for _, alert := range alerts {
			if strings.HasSuffix(alert.Class, "-telemetry-incomplete") && !slices.Contains(engineCase.want, alert.Class) {
				t.Fatalf("%s: native vector left the probe unknown: %s", engineCase.Name, alert.Observed)
			}
		}
		for _, class := range engineCase.want {
			if !slices.Contains(classes, class) {
				t.Fatalf("%s: native vector did not produce %s; classes=%v", engineCase.Name, class, classes)
			}
		}
		for _, class := range engineCase.forbid {
			if slices.Contains(classes, class) {
				t.Fatalf("%s: native vector produced %s", engineCase.Name, class)
			}
		}
		if len(engineCase.want) == 0 && len(alerts) != 0 {
			t.Fatalf("%s: a healthy native vector alerted: %v", engineCase.Name, classes)
		}
	}
	t.Logf("pinned PromQL engine: %d native cases", len(cases))
}

// External engine source stays data to the Server module and materializes as
// a valid Go test for the pinned Mimir module.
func TestConnectMovePromqlEngineFixtureIsInertServerData(t *testing.T) {
	source, err := os.ReadFile(connectMovePromqlEngineFixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "connect_move_promql_engine_test.go")
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse materialized engine test: %v", err)
	}
	if file.Name.Name != "connect_move_test" || file.Scope.Lookup("TestConnectMoveNativeVectors") == nil {
		t.Fatal("materialized engine fixture does not define connect_move_test.TestConnectMoveNativeVectors")
	}
}
