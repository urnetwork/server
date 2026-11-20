package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The normal registry must expose the catalogued H1 admission signal.
func TestConnectHandshakeRefusalsRegistry(t *testing.T) {
	for _, signal := range NewSignals() {
		if signal.Number() == "25.2" && signal.Key() == "connect-handshake-refusals" {
			return
		}
	}
	t.Fatal("catalog §25.2 has no registered Connect handshake refusal signal")
}

// Two desired placements on enabled synthetic services hosts.
func connectHandshakeTestScope(settings *SignalSettings) {
	settings.LogServiceHosts["connect"] = []string{"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1"}
	settings.LogServiceBlocks["connect"] = []string{"g0"}
}

// Build the five fixed responses per slot, at the sixteen endpoints in a
// 15-minute hold. mutate can remove or replace individual observations.
func connectHandshakeTestHistory(t *testing.T, total, accepted, dependency float64, mutate func(slot int, part string, values []float64)) string {
	t.Helper()
	series := []stormSyntheticSeries{}
	for slot := 0; slot < 2; slot++ {
		for _, part := range []struct {
			key   string
			value float64
		}{
			{"publishers", 1}, {"qualified", 1}, {"total", total}, {"accepted", accepted}, {"dependency", dependency},
		} {
			values := stormRepeat(part.value, 16)
			if mutate != nil {
				mutate(slot, part.key, values)
			}
			series = append(series, stormSyntheticSeries{part.key, map[string]string{"host": fmt.Sprintf("by-us-fmt-5-edge-%d", slot), "block": "g0"}, values})
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(16), time.Minute, series)
}

// Verify the exact threshold edges, including the older endpoint of each
// hold, through the registered signal's actual reducer and renderer.
func TestConnectHandshakeRefusalsBands(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		total, accepted, dependency float64
		mutate                      func(int, string, []float64)
		incident, visibility        string
	}{
		{"healthy lazy dependency", 100, 72, 0, nil, "healthy", "healthy"},
		{"dependency warn inclusive", 100, 70, 10, nil, tierWarn, "healthy"},
		{"dependency page inclusive", 200, 120, 50, nil, tierPage, "healthy"},
		{"October 19 scale", 100, 20, 79.5, nil, tierPage, "healthy"},
		{"acceptance warn edge healthy", 100, 50, 0, nil, "healthy", "healthy"},
		{"acceptance page edge warn", 100, 30, 0, nil, tierWarn, "healthy"},
		{"accepted lazy zero", 100, 0, 0, nil, tierPage, "healthy"},
		{"zero denominator", 0, 0, 0, nil, "hold", "unobservable"},
		{"one short of a dependency hold", 200, 120, 50, func(_ int, part string, v []float64) {
			if part == "dependency" {
				v[len(v)-6] = 0
			}
		}, "hold", "healthy"},
		{"one short of acceptance page hold", 100, 29, 0, func(_ int, part string, v []float64) {
			if part == "accepted" {
				v[len(v)-11] = 30
			}
		}, tierWarn, "healthy"},
		{"one short of acceptance warn hold", 100, 49, 0, func(_ int, part string, v []float64) {
			if part == "accepted" {
				v[0] = 50
			}
		}, "hold", "healthy"},
		{"old gap beside new dependency page", 200, 120, 50, func(_ int, part string, v []float64) {
			if part == "qualified" {
				v[0] = math.NaN()
			}
		}, tierPage, "unobservable"},
		{"missing sibling cannot prove fleet ratio", 100, 20, 0, func(slot int, part string, v []float64) {
			if slot == 1 && part == "publishers" {
				for i := range v {
					v[i] = math.NaN()
				}
			}
		}, "hold", "unobservable"},
		{"qualified subset proves dependency page", 200, 80, 100, func(slot int, part string, v []float64) {
			if slot == 1 && part == "publishers" {
				for i := range v {
					v[i] = 2
				}
			}
		}, tierPage, "unobservable"},
		{"missing returned part cannot become lazy zero", 100, 0, 0, func(slot int, part string, v []float64) {
			if slot == 1 && part == "accepted" {
				for i := range v {
					v[i] = math.NaN()
				}
			}
		}, "hold", "unobservable"},
		{"inconsistent result rates", 100, 90, 20, nil, "hold", "unobservable"},
		{"nonfinite fleet sum", math.MaxFloat64, 0, 0, nil, "hold", "unobservable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := connectHandshakeTestHistory(t, tc.total, tc.accepted, tc.dependency, tc.mutate)
			states := stormFindingStates(t, connectHandshakeProbe{}, stormSyntheticNow, response, connectHandshakeTestScope)
			requireStormStates(t, tc.name, states, map[string]string{
				connectHandshakeClass + " main/connect-h1":        tc.incident,
				connectHandshakeUnknownClass + " main/connect-h1": tc.visibility,
			})
			if tc.name == "October 19 scale" {
				alerts := runStormSynthetic(t, NewConnectHandshakeRefusalsSignal(), stormSyntheticNow, response, connectHandshakeTestScope)
				alert := requireAlertClass(t, alerts, connectHandshakeClass)
				if alert.Severity != SeverityPage || alert.Sustain != 1 || alert.Target != "main/connect-h1" || alert.Frame != "" ||
					!strings.Contains(alert.Markdown(), "dependency_per_second=159.000 acceptance_share=0.2000") || !strings.Contains(alert.Markdown(), "client_canceled") {
					t.Fatalf("unexpected H1 impact evidence: %s", alert.Markdown())
				}
			}
		})
	}
}

// Source failures render fixed reasons. Unconfigured or excluded placements,
// corrupted shapes and canceled reads cannot publish healthy fleet evidence.
func TestConnectHandshakeRefusalsSourceBoundaries(t *testing.T) {
	signal := NewConnectHandshakeRefusalsSignal()
	for _, tc := range []struct {
		name      string
		body      string
		sourceErr error
		reason    string
	}{
		{"unavailable", "private-response", errors.New("private-source-error"), "source_unavailable"},
		{"invalid", "private-response", nil, "invalid_response"},
		{"empty", stormSyntheticResponse(t, stormSyntheticStart(16), time.Minute, nil), nil, "slot_coverage_incomplete"},
		{"malformed labels", stormSyntheticResponse(t, stormSyntheticStart(16), time.Minute, []stormSyntheticSeries{{"accepted", map[string]string{"instance": "private-process"}, []float64{1}}}), nil, "unrecognized_label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) { return tc.body, tc.sourceErr }}
			settings := stormMainSettings(stormSyntheticNow, source)
			connectHandshakeTestScope(&settings)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || alerts[0].Class != connectHandshakeUnknownClass || !alerts[0].unobservable || !strings.Contains(alerts[0].Observed, tc.reason) {
				t.Fatalf("source boundary %v %+v", err, alerts)
			}
			requireAlertOmits(t, alerts[0], "private-source-error", "private-response", "private-process")
		})
	}
	for _, tc := range []struct {
		name   string
		signal Signal
		change func(*SignalSettings)
		reason string
	}{
		{"no placement", signal, func(s *SignalSettings) { delete(s.LogServiceBlocks, "connect") }, "placement_unavailable"},
		{"excluded placement", signal, func(s *SignalSettings) { s.LogServiceHosts["connect"] = []string{"unknown-host"} }, "placement_unavailable"},
		{"invalid policy", NewConnectHandshakeRefusalsSignalWithSettings(&ConnectHandshakeRefusalsSettings{}), func(*SignalSettings) {}, "invalid_policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				t.Fatal("unavailable placement or policy contacted a gateway")
				return "", nil
			}}
			settings := stormMainSettings(stormSyntheticNow, source)
			tc.change(&settings)
			alerts, err := tc.signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || !alerts[0].unobservable || !strings.Contains(alerts[0].Observed, tc.reason) {
				t.Fatalf("pre-read qualification: %v %+v", err, alerts)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { cancel(); return "", context.Canceled }}
	alerts, err := signal.Run(ctx, stormMainSettings(stormSyntheticNow, source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled read: %v %+v", err, alerts)
	}
}

// Exercise the wire request with the exact production query, range and caps.
func TestConnectHandshakeRefusalsBoundedRange(t *testing.T) {
	policy := DefaultConnectHandshakeRefusalsSettings()
	inventory := fleetSlotInventory{slots: []fleetSlot{
		{service: "connect", host: "by-us-fmt-5-edge-0", block: "g0", allowed: true},
		{service: "connect", host: "by-us-fmt-5-edge-1", block: "g0", allowed: true},
	}}
	end := stormSyntheticNow.Add(-2 * time.Minute)
	replay := stormReplayCase{
		Name: "H1 October 19 scale", Now: stormSyntheticNow, Query: connectHandshakeQuery(inventory.selector("main"), policy),
		Start: end.Add(-15 * time.Minute).Unix(), End: end.Unix(), Step: 60,
		Response: []byte(connectHandshakeTestHistory(t, 100, 20, 79.5, nil)),
	}
	alerts := runStormReplay(t, NewConnectHandshakeRefusalsSignal(), replay, connectHandshakeTestScope)
	if len(alerts) != 1 || alerts[0].Severity != SeverityPage {
		t.Fatalf("bounded H1 read returned %+v", alerts)
	}
}

// Execute the five tagged production expressions against raw counters from
// stable, newly seen, reset and overlapping processes. This complements the
// reducer's minute-by-minute controls with actual PromQL matching and rates.
func TestConnectHandshakeRefusalsPromqlEngine(t *testing.T) {
	runCatalogPromqlScript(t, func() string {
		inventory := fleetSlotInventory{slots: []fleetSlot{{service: "connect", host: "alpha", block: "g0", allowed: true}}}
		query := connectHandshakeQuery(inventory.selector("synthetic"), DefaultConnectHandshakeRefusalsSettings())
		const process = `env="synthetic",job="connect",service="connect",host="alpha",block="g0",instance="aaaabb00"`
		const success = `,carrier="h1plus",stage="authenticated",result="accepted"`
		const dependency = `,carrier="websocket",stage="upgraded",result="auth_dependency"`
		counter := func(labels, values string) string {
			return fmt.Sprintf("  %s{%s%s} %s\n", connectHandshakeMetric, process, labels, values)
		}
		start := "  process_start_time_seconds{" + process + "} 60+0x20\n"
		accepted := counter(success, "1000+2400x20")                                                    // 40/s
		failed := counter(dependency, "1000+9540x20")                                                   // 159/s
		canceled := counter(`,carrier="websocket",stage="http",result="client_canceled"`, "1000+60x20") // 1/s
		var script strings.Builder
		for _, tc := range []struct {
			name, series string
			countParts   bool
			expected     map[string]int
		}{
			{"both carriers", start + accepted + failed + canceled, false, map[string]int{"publishers": 1, "qualified": 1, "total": 200, "accepted": 40, "dependency": 159}},
			{"lazy dependency", start + accepted, false, map[string]int{"publishers": 1, "qualified": 1, "total": 40, "accepted": 40, "dependency": 0}},
			{"lazy accepted", start + failed, false, map[string]int{"publishers": 1, "qualified": 1, "total": 159, "accepted": 0, "dependency": 159}},
			{"missing family", start, false, map[string]int{"publishers": 1}},
			{"overlap", start + accepted + strings.ReplaceAll(start+failed, "aaaabb00", "aaaabb01"), false, map[string]int{"publishers": 2, "qualified": 2, "total": 199, "accepted": 40, "dependency": 159}},
			{"new child", start + accepted + counter(dependency, strings.Repeat("_ ", 19)+"100 125"), true, map[string]int{"publishers": 1, "total": 1, "accepted": 1, "dependency": 1}},
			{"reset child", start + accepted + counter(dependency, "100+5x18 0 5"), true, map[string]int{"publishers": 1, "total": 1, "accepted": 1, "dependency": 1}},
			{"stale child", start + accepted + counter(dependency, "100+5x16"), true, map[string]int{"publishers": 1, "total": 1, "accepted": 1, "dependency": 1}},
			{"changed process start", strings.Replace(start, "60+0x20", "60+0x18 90 90", 1) + accepted, true, map[string]int{"publishers": 1, "total": 1, "accepted": 1, "dependency": 1}},
			{"unknown carrier schema", start + strings.Replace(accepted, "h1plus", "unrecognized", 1), true, map[string]int{"publishers": 1, "total": 1, "accepted": 1, "dependency": 1}},
		} {
			fmt.Fprintf(&script, "# %s\nclear\nload 1m\n%s\n", tc.name, tc.series)
			if tc.countParts {
				fmt.Fprintf(&script, "eval instant at 20m count by (monitor_part) (%s)\n", query)
			} else {
				fmt.Fprintf(&script, "eval instant at 20m %s\n", query)
			}
			for _, part := range []string{"publishers", "qualified", "total", "accepted", "dependency"} {
				if value, ok := tc.expected[part]; ok {
					labels := `host="alpha",block="g0",`
					if tc.countParts {
						labels = ""
					}
					fmt.Fprintf(&script, "  {%smonitor_part=%q} %d\n", labels, part, value)
				}
			}
			script.WriteString("\n")
		}
		return script.String()
	})
}
