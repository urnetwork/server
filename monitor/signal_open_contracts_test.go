// Open-set controls separate observed backlog/trend from causal remediation.
package monitor

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openContractsAggregateFixture() requiredAggregateFixture {
	return requiredAggregateFixture{NewOpenContractsSignal, "WHERE open = true", Row{"50000", "1000", "0"}}
}

func TestOpenContractsAggregateShape(t *testing.T) {
	testRequiredAggregateShape(t, openContractsAggregateFixture())
}

func TestOpenContractsAggregateRunLoop(t *testing.T) {
	testRequiredAggregateRunLoop(t, openContractsAggregateFixture())
}

func TestOpenContractsAggregateUnknownPreservesTrend(t *testing.T) {
	for _, test := range requiredAggregateBadShapes(openContractsAggregateFixture().healthyRow) {
		t.Run(test.name, func(t *testing.T) {
			for _, seeded := range []bool{false, true} {
				name := "uninitialized"
				if seeded {
					name = "previous valid count"
				}
				t.Run(name, func(t *testing.T) {
					signal := NewOpenContractsSignal()
					probe := signal.(*signalAdapter).probe.(*pgOpenSetProbe)
					if seeded {
						probe.initialized, probe.lastCount = true, 170000
					}
					beforeInitialized, beforeCount := probe.initialized, probe.lastCount
					rows := test.rows
					reads := 0
					source := &syntheticSource{postgresFn: func(string) ([]Row, error) { reads++; return rows, nil }}
					m := NewWithSignals(syntheticSettings(source), signal)
					alerts, err, panicked := runRequiredAggregateSafely(m)
					if panicked {
						t.Error("unknown open-set aggregate panicked")
					} else {
						if err == nil {
							t.Error("unknown open-set aggregate was accepted")
						}
						requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
					}
					if probe.initialized != beforeInitialized || probe.lastCount != beforeCount || reads != 1 {
						t.Error("unknown aggregate changed the open-set adjacent-sample state")
					}
					rows = []Row{{"160000", "1000", "0"}}
					alerts, err = m.Run(context.Background())
					if err != nil || reads != 2 || !probe.initialized || probe.lastCount != 160000 {
						t.Fatal("valid continuation did not observe the next real count exactly once")
					}
					if seeded {
						if len(alerts) != 0 {
							t.Error("fall from the last valid count became a manufactured rise")
						}
					} else if len(alerts) != 1 || !strings.Contains(alerts[0].Symptom, "trend is warming up") {
						t.Error("unknown first observation consumed the valid sample's trend warmup")
					}
				})
			}
		})
	}
}

func TestOpenContractsAggregateValidBands(t *testing.T) {
	for _, test := range []struct {
		name   string
		counts []int
		want   int
	}{
		{name: "observed zero", counts: []int{0}},
		{name: "threshold equality", counts: []int{150000}},
		{name: "warmup above threshold", counts: []int{150001}, want: 1},
		{name: "rising", counts: []int{160000, 170000}, want: 1},
		{name: "flat", counts: []int{170000, 170000}},
		{name: "falling", counts: []int{170000, 160000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				count := test.counts[reads]
				reads++
				return []Row{{strconv.Itoa(count), "0", "0"}}, nil
			}}
			signal := NewOpenContractsSignal()
			m := NewWithSignals(syntheticSettings(source), signal)
			var alerts Alerts
			for range test.counts {
				var err error
				alerts, err = m.Run(context.Background())
				if err != nil {
					t.Fatal("valid open-set aggregate became unknown")
				}
			}
			if len(alerts) != test.want || reads != len(test.counts) || signal.Cadence() != 5*time.Minute {
				t.Fatal("valid open-set threshold, trend or cadence changed")
			}
			if test.want != 0 {
				alert := requireAlertClass(t, alerts, "open-set-size")
				if alert.SignalID != signal.ID() || alert.Target != "pg-1" || alert.Severity != SeverityWarn || alert.Sustain != 3 {
					t.Error("valid open-set finding changed identity or escalation")
				}
			}
		})
	}
}

// Adjacent samples distinguish warmup, decline, rise, and flat high counts.
func TestOpenContractsSignalSyntheticOpenSetBacklog(t *testing.T) {
	counts := []int{160000, 159000, 161000, 161000}
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		count := counts[0]
		counts = counts[1:]
		return []Row{{strconv.Itoa(count), "120000", "8000"}}, nil
	}}
	signal := NewOpenContractsSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	warmup := requireAlertClass(t, alerts, "open-set-size")
	if strings.Contains(warmup.Symptom, "rising") || !strings.Contains(warmup.Symptom, "warming up") {
		t.Fatalf("startup alert claimed an unobserved trend: %q", warmup.Symptom)
	}

	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("falling high set must reset the alert, got %+v", alerts)
	}

	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	rising := requireAlertClass(t, alerts, "open-set-size")
	if !strings.Contains(rising.Symptom, "rising (previous 159000)") ||
		!strings.Contains(rising.Observed, "delta=2000") ||
		!strings.Contains(rising.Observed, "older_5m=120000 older_30m=8000") ||
		!strings.Contains(rising.Mechanism, "independent open and disputed scans capped at 25,000 each") ||
		!strings.Contains(rising.Mechanism, "older deployments used 100,000") ||
		!strings.Contains(rising.Context, "retention-fanout") ||
		!strings.Contains(rising.Verify, "older-than-five-minute cohort falls") {
		t.Fatalf("rising alert lacks adjacent-sample evidence: %+v", rising)
	}

	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("stable high set must not claim growth, got %+v", alerts)
	}
}

// Neither a young spike nor an aged or rising set proves retention caused it.
func TestOpenContractsSignalActionRequiresCausalAttribution(t *testing.T) {
	cases := []struct {
		name        string
		rows        []Row
		wantSymptom string
	}{
		{
			name:        "young-warmup",
			rows:        []Row{{"165000", "35000", "0"}},
			wantSymptom: "trend is warming up",
		},
		{
			name:        "aged-warmup",
			rows:        []Row{{"175000", "160000", "90000"}},
			wantSymptom: "trend is warming up",
		},
		{
			name:        "adjacent-rise",
			rows:        []Row{{"160000", "10000", "0"}, {"170000", "25000", "0"}},
			wantSymptom: "rising (previous 160000)",
		},
	}
	for _, c := range cases {
		sampleIndex := 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if sampleIndex >= len(c.rows) {
				t.Fatalf("%s: unexpected extra observation", c.name)
			}
			for _, want := range []string{"interval '5 minutes'", "interval '30 minutes'", "WHERE open = true"} {
				if !strings.Contains(query, want) {
					t.Errorf("%s: observation changed the existing count or age predicate", c.name)
				}
			}
			row := c.rows[sampleIndex]
			sampleIndex++
			return []Row{row}, nil
		}}
		signal := NewOpenContractsSignal()
		var alerts Alerts
		for range c.rows {
			var err error
			alerts, err = signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatalf("%s: observation failed: %v", c.name, err)
			}
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: alert count = %d, want 1", c.name, len(alerts))
		}
		alert := alerts[0]
		if alert.Class != "open-set-size" || alert.Severity != SeverityWarn || alert.Sustain != 3 || signal.Cadence() != 5*time.Minute {
			t.Errorf("%s: guidance altered identity, severity, sustain, or cadence", c.name)
		}
		if !strings.Contains(alert.Symptom, c.wantSymptom) {
			t.Errorf("%s: guidance altered the observed trend", c.name)
		}
		lastRow := c.rows[len(c.rows)-1]
		for _, want := range []string{"open_contracts=" + lastRow[0], "older_5m=" + lastRow[1], "older_30m=" + lastRow[2]} {
			if !strings.Contains(alert.Observed, want) {
				t.Errorf("%s: guidance omitted an observed count or age bucket", c.name)
			}
		}
		for _, want := range []string{
			"consecutive age buckets",
			"CloseExpiredContracts duration/outcomes",
			"retention-fanout evidence",
			"transfer_contract autovacuum phase",
			"before changing code or deploying",
			"count alone does not establish a retention defect",
			"only after that cause is confirmed in the running path",
			"Do not raise closer concurrency while PostgreSQL write/vacuum debt is present",
			"complete stored error and next run time",
			"underfunded disputed row",
			"preserve its settlement guard and reservation",
			"terminal-verified sibling progress",
			"without inferring that cause from the backlog count",
		} {
			if !strings.Contains(alert.Action, want) {
				t.Errorf("%s: emitted action omitted causal qualifier %q", c.name, want)
			}
		}
		if strings.Contains(alert.Action, "Fix or roll out the bounded retention path") {
			t.Errorf("%s: emitted action still assumes an unproved retention cause", c.name)
		}
		if !strings.Contains(alert.Mechanism, "deduplicated union can contain up to 50,000 candidates") ||
			!strings.Contains(alert.Context, "Filtered task-name logs can omit joined-error continuation lines") ||
			!strings.Contains(alert.Verify, "unresolved financial rejection remains a task failure warning") {
			t.Errorf("%s: checkpoint/financial unknown qualifiers missing", c.name)
		}
		if !strings.Contains(alert.Markdown(), "### Action\n\n"+alert.Action+"\n\n### Verify\n") {
			t.Errorf("%s: Markdown did not preserve the qualified action", c.name)
		}
	}
}

// Causal guidance does not change the strict threshold or adjacent-trend resets.
func TestOpenContractsSignalGuidancePreservesCountAndTrendGuards(t *testing.T) {
	cases := []struct {
		name       string
		counts     []int
		wantAlerts int
	}{
		{name: "healthy-band", counts: []int{50000}, wantAlerts: 0},
		{name: "threshold-equality", counts: []int{150000}, wantAlerts: 0},
		{name: "threshold-exceeded", counts: []int{150001}, wantAlerts: 1},
		{name: "high-but-falling", counts: []int{170000, 160000}, wantAlerts: 0},
		{name: "high-but-flat", counts: []int{170000, 170000}, wantAlerts: 0},
	}
	for _, c := range cases {
		sampleIndex := 0
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			if sampleIndex >= len(c.counts) {
				t.Fatalf("%s: unexpected extra observation", c.name)
			}
			count := c.counts[sampleIndex]
			sampleIndex++
			return []Row{{strconv.Itoa(count), "10000", "0"}}, nil
		}}
		signal := NewOpenContractsSignal()
		var alerts Alerts
		for range c.counts {
			var err error
			alerts, err = signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatalf("%s: observation failed: %v", c.name, err)
			}
		}
		if len(alerts) != c.wantAlerts {
			t.Errorf("%s: alert count = %d, want %d", c.name, len(alerts), c.wantAlerts)
		}
	}
}

// An unavailable count cannot emit retention guidance or establish a trend.
func TestOpenContractsSignalObservationFailureDoesNotInferRetention(t *testing.T) {
	observationErr := errors.New("synthetic observation unavailable")
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, observationErr
	}}
	signal := NewOpenContractsSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if !errors.Is(err, observationErr) || len(alerts) != 0 {
		t.Fatal("failed observation must preserve its error without emitting remediation")
	}
	source.postgresFn = func(string) ([]Row, error) {
		return []Row{{"170000", "20000", "0"}}, nil
	}
	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Symptom, "trend is warming up") {
		t.Fatal("failed observation must not initialize the next successful trend")
	}
}

// The general catalog qualifier must not borrow proof from historical incidents.
func TestOpenContractsCatalogRequiresCausalAttribution(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "### 2.6 Open-contract set size")
	if start < 0 {
		t.Fatal("open-set catalog section is missing")
	}
	section := catalog[start:]
	end := strings.Index(section, "2026-08-30 close-tail discriminator:")
	if end < 0 {
		t.Fatal("open-set historical-evidence boundary is missing")
	}
	section = strings.Join(strings.Fields(section[:end]), " ")
	for _, want := range []string{
		"warmup or a rise alone does not identify a retention defect",
		"Before changing code or deploying",
		"consecutive age buckets",
		"closer's duration/outcomes",
		"retention-fanout evidence",
		"transfer_contract autovacuum phase",
		"only if that attribution is confirmed",
		"historical episodes below are not proof of the current running path",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("open-set catalog omitted causal qualifier %q", want)
		}
	}
}
