package monitor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestPostgresStateSignalSyntheticIdleTransactionStorm(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "count(*) FILTER") {
			return []Row{{"3", "121", "532", "140"}}, nil
		}
		if strings.Contains(query, "WITH idle AS MATERIALIZED") {
			return []Row{
				{"0", "oldest", "1", "277", "1742249", "", "SELECT start_time, end_time FROM subsidy_payment WHERE start_time < $2 AND $1 < end_time"},
				{"1", "shape", "119", "1", "0", "", "UPDATE transfer_contract SET outcome = $2, close_time = $3 WHERE contract_id = $1"},
			}, nil
		}
		return nil, nil
	}}
	alerts, err := NewPostgresStateSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "idle-in-tx")
	for _, detail := range []string{
		"oldest_xact_s=532",
		"count and oldest age can have different owners",
		"oldest transaction: pid=1742249 continuously_idle=277s",
		"application=withheld query=withheld",
		"backends=119 oldest_continuous_idle=1s",
		"transaction-local idle-timeout fix",
		"Do not mass-terminate",
		"battery begins after the state summary",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, detail) {
			t.Fatalf("mixed idle-in-transaction alert missing %q:\n%s", detail, markdown)
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "zombie-tx" {
			t.Fatalf("532s transaction incorrectly crossed the 30-minute zombie threshold: %+v", alerts)
		}
	}
}

func TestPostgresStateSignalSyntheticActivePileup(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "count(*) FILTER") {
			return []Row{{"101", "0", "0", "120"}}, nil
		}
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // keep the synthetic escalation battery from waiting for its 15s delta
	alerts, err := NewPostgresStateSignal().Run(ctx, syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "active-pileup")
	for _, want := range []string{
		"battery begins after the state summary",
		"not an arithmetic partition of the observed total",
		"empty statement delta is unknown rather than healthy",
	} {
		if !strings.Contains(alert.Context, want) {
			t.Fatalf("one-shot active context missing %q: %s", want, alert.Context)
		}
	}
}

func TestPostgresStateSignalCachedBatteryPrecedesLargerSustainedTotal(t *testing.T) {
	stateCalls := 0
	batteryCalls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "count(*) FILTER"):
			stateCalls++
			if stateCalls == 1 {
				return []Row{{"101", "0", "0", "140"}}, nil
			}
			return []Row{{"574", "13", "5", "714"}}, nil
		case strings.Contains(query, "GROUP BY query_id ORDER BY backends DESC LIMIT 5"):
			batteryCalls++
			return []Row{{"73", "11", "-:-", "SELECT bounded_fixture"}}, nil
		default:
			return nil, nil
		}
	}}
	signal := NewPostgresStateSignal()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // keep the plan-wall battery's bounded delta from waiting
	if _, err := signal.Run(ctx, syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	alerts, err := signal.Run(ctx, syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "active-pileup")
	for _, check := range []struct {
		name string
		got  string
		want string
	}{
		{name: "current total", got: alert.Observed, want: "active=574"},
		{name: "trip battery", got: alert.Evidence, want: "backends=11"},
		{name: "cached annotation", got: alert.Evidence, want: "battery collected once at trip"},
		{name: "earlier frame", got: alert.Context, want: "precedes this later sustained state summary"},
		{name: "non-atomic", got: alert.Context, want: "separate snapshots"},
		{name: "no subtraction", got: alert.Context, want: "not an arithmetic partition of the observed total"},
		{name: "empty delta unknown", got: alert.Context, want: "empty statement delta is unknown rather than healthy"},
	} {
		if !strings.Contains(check.got, check.want) {
			t.Fatalf("%s missing %q: %q", check.name, check.want, check.got)
		}
	}
	if batteryCalls != 1 {
		t.Fatalf("active battery calls = %d, want one trip-time collection", batteryCalls)
	}
}

// Invalid summaries and real source failures must preserve cached trip evidence.
func TestPostgresStateAggregateFailurePreservesBatteryCache(t *testing.T) {
	sourceFailure := errors.New("synthetic state source unavailable")
	for _, test := range []struct {
		name      string
		rows      []Row
		sourceErr error
		wantClass string
	}{
		{name: "missing row"},
		{name: "missing all columns", rows: []Row{{}}},
		{name: "missing final columns", rows: []Row{{"0", "0"}}},
		{name: "empty active", rows: []Row{{"", "0", "0", "20"}}},
		{name: "malformed active", rows: []Row{{"synthetic-private-state 192.0.2.33", "0", "0", "20"}}},
		{name: "malformed idle", rows: []Row{{"0", "invalid", "0", "20"}}},
		{name: "malformed age", rows: []Row{{"0", "1", "invalid", "20"}}},
		{name: "malformed total", rows: []Row{{"0", "0", "0", "invalid"}}},
		{name: "negative active", rows: []Row{{"-1", "0", "0", "20"}}},
		{name: "negative idle", rows: []Row{{"0", "-1", "0", "20"}}},
		{name: "negative age", rows: []Row{{"0", "1", "-1", "20"}}},
		{name: "negative total", rows: []Row{{"0", "0", "0", "-1"}}},
		{name: "fractional count", rows: []Row{{"0.5", "0", "0", "20"}}},
		{name: "fractional age", rows: []Row{{"0", "1", "1800.5", "20"}}},
		{name: "overflowing count", rows: []Row{{"9223372036854775808", "0", "0", "20"}}},
		{name: "extra row", rows: []Row{{"0", "0", "0", "20"}, {"0", "0", "0", "20"}}},
		{name: "extra column", rows: []Row{{"0", "0", "0", "20", "0"}}},
		{name: "source failure", sourceErr: sourceFailure, wantClass: observationErrorClassUnclassified},
		{name: "source deadline", sourceErr: context.DeadlineExceeded, wantClass: observationErrorClassTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			signal, state, before := seededPostgresStateSignal()
			rows := test.rows
			sourceErr := test.sourceErr
			calls := 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				calls++
				if !strings.Contains(query, "count(*) FILTER") {
					t.Fatal("an invalid state summary started an optional battery")
				}
				return rows, sourceErr
			}}
			monitor := NewWithSignals(syntheticSettings(source), signal)
			alerts, err := monitor.Run(context.Background())
			if err == nil || len(alerts) != 1 {
				t.Fatalf("invalid state became health or a condition alert: err=%v alerts=%d", err, len(alerts))
			}
			wantClass := test.wantClass
			if wantClass == "" {
				wantClass = observationErrorClassInvalidResponse
			}
			alert := alerts[0]
			if alert.Class != "cannot-observe" || alert.Target != "pg/state-split" ||
				alert.Severity != SeverityWarn || alert.Sustain != 2 ||
				alert.Observed != "error_class="+wantClass {
				t.Fatalf("state failure changed the shared visibility contract: class=%s observed=%s", alert.Class, alert.Observed)
			}
			if sourceErr != nil && !errors.Is(err, sourceErr) {
				t.Fatal("state source error lost its original cause")
			}
			for _, forbidden := range []string{"synthetic-private-state", "192.0.2.33"} {
				if strings.Contains(err.Error(), forbidden) || strings.Contains(alert.Markdown(), forbidden) {
					t.Fatal("invalid aggregate leaked synthetic private data")
				}
			}
			if calls != 1 || !reflect.DeepEqual(state.batteries.tripped, before) {
				t.Fatal("failed summary re-armed or changed cached trip evidence")
			}

			// A later valid broken summary must still use the original trip.
			rows, sourceErr = []Row{{"101", "101", "1", "220"}}, nil
			alerts, err = monitor.Run(context.Background())
			if err != nil || len(alerts) != 2 || calls != 2 {
				t.Fatalf("next complete state failed: err=%v alerts=%d calls=%d", err, len(alerts), calls)
			}
			for _, class := range []string{"active-pileup", "idle-in-tx"} {
				alert := requireAlertClass(t, alerts, class)
				if !strings.Contains(alert.Evidence, before[class].evidence) ||
					!strings.Contains(alert.Evidence, "battery collected once at trip") {
					t.Fatal("next complete state lost the preceding trip evidence")
				}
			}
			if !reflect.DeepEqual(state.batteries.tripped, before) {
				t.Fatal("valid continued trip replaced cached evidence")
			}
		})
	}
}

// Only the aggregate contract changes; valid thresholds and re-arming do not.
func TestPostgresStateAggregateValidBandsAndOptionalEmpty(t *testing.T) {
	for _, test := range []struct {
		name           string
		row            Row
		wantClasses    []string
		wantBatteryRun int
	}{
		{name: "numeric zero", row: Row{"0", "0", "0", "0"}},
		{name: "healthy state", row: Row{"6", "2", "30", "20"}},
		{name: "trimmed state", row: Row{" 6 ", " 2 ", " 30 ", " 20 "}},
		{name: "exact thresholds", row: Row{"100", "100", "1800", "250"}},
		{name: "active threshold exceeded", row: Row{"101", "0", "0", "125"}, wantClasses: []string{"active-pileup"}},
		{name: "idle threshold exceeded", row: Row{"0", "101", "0", "125"}, wantClasses: []string{"idle-in-tx"}},
		{name: "age threshold exceeded", row: Row{"0", "1", "1801", "20"}, wantClasses: []string{"idle-in-tx", "zombie-tx"}, wantBatteryRun: 1},
		{name: "maximum count", row: Row{"9223372036854775807", "0", "0", "9223372036854775807"}, wantClasses: []string{"active-pileup"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			signal, state, _ := seededPostgresStateSignal()
			summaryCalls, batteryCalls := 0, 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "count(*) FILTER") {
					summaryCalls++
					return []Row{test.row}, nil
				}
				if !strings.Contains(query, "WITH idle AS MATERIALIZED") {
					t.Fatal("unexpected optional battery query")
				}
				batteryCalls++
				return nil, nil // Work may finish before this later GROUP BY.
			}}
			alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
			if err != nil || len(alerts) != len(test.wantClasses) {
				t.Fatalf("valid state changed bands: err=%v alerts=%d want=%d", err, len(alerts), len(test.wantClasses))
			}
			for _, class := range test.wantClasses {
				alert := requireAlertClass(t, alerts, class)
				wantSeverity, wantSustain := SeverityWarn, 2
				if class == "active-pileup" {
					wantSeverity = SeverityPage
				}
				if class == "zombie-tx" {
					wantSustain = 1
				}
				if alert.Severity != wantSeverity || alert.Sustain != wantSustain {
					t.Fatal("valid state changed severity or sustain")
				}
			}
			if summaryCalls != 1 || batteryCalls != test.wantBatteryRun {
				t.Fatal("valid state repeated its summary or changed optional collection")
			}
			if len(test.wantClasses) == 0 && len(state.batteries.tripped) != 0 {
				t.Fatal("complete healthy summary failed to re-arm batteries")
			}
		})
	}
}

// Empty or failed attribution cannot erase a concrete idle-count violation.
func TestPostgresStateOptionalBatteryDoesNotOwnSummaryHealth(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "legitimate empty grouped snapshot"},
		{name: "optional source failure", err: errors.New("synthetic optional source unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				calls++
				if strings.Contains(query, "count(*) FILTER") {
					return []Row{{"0", "101", "1", "125"}}, nil
				}
				if !strings.Contains(query, "WITH idle AS MATERIALIZED") {
					t.Fatal("unexpected optional query")
				}
				return nil, test.err
			}}
			alerts, err := NewWithSignals(syntheticSettings(source), NewPostgresStateSignal()).Run(context.Background())
			if err != nil || len(alerts) != 1 || calls != 2 {
				t.Fatalf("optional attribution erased the state result: err=%v alerts=%d calls=%d", err, len(alerts), calls)
			}
			alert := requireAlertClass(t, alerts, "idle-in-tx")
			wantEvidence := "idle-in-tx by last query shape:"
			if test.err != nil {
				wantEvidence = "idle-tx battery failed:"
			}
			if !strings.Contains(alert.Evidence, wantEvidence) {
				t.Fatal("optional attribution result was not retained")
			}
		})
	}

	// Both helpers have valid zero-row GROUP BY results; neither is a scalar.
	settings := syntheticSettings(&syntheticSource{})
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if got := activeBattery(context.Background(), env); got != "top active query_ids:" {
		t.Fatalf("empty active grouping became a source failure: %q", got)
	}
	if got := idleTxBattery(context.Background(), env); got != "idle-in-tx by last query shape:" {
		t.Fatalf("empty idle grouping became a source failure: %q", got)
	}
}

// Seed real latches so malformed summaries cannot silently re-arm a trip.
func seededPostgresStateSignal() (Signal, *pgStateProbe, map[string]trippedBattery) {
	signal := NewPostgresStateSignal()
	state := signal.(*signalAdapter).probe.(*pgStateProbe)
	before := map[string]trippedBattery{}
	for _, class := range []string{"active-pileup", "idle-in-tx"} {
		state.batteries.broken(class, func() string { return "synthetic trip evidence for " + class })
		before[class] = state.batteries.tripped[class]
	}
	return signal, state, before
}

// Optional errors must be safe before they enter reusable alerts, without
// loading Vault or relying on process-output scrubbing.
func TestPostgresStateOptionalBatteryErrorsRenderOnlyFixedClasses(t *testing.T) {
	const credential = "synthetic-battery-unregistered-credential"
	privateSuffix := "password=" + credential + " address=192.0.2.34"
	for _, helper := range []struct {
		name   string
		class  string
		tier   string
		prefix string
		run    func(context.Context, *probeEnv) string
	}{
		{name: "active", class: "active-pileup", tier: tierPage, prefix: "active battery failed: ", run: activeBattery},
		{name: "idle", class: "idle-in-tx", tier: tierWarn, prefix: "idle-tx battery failed: ", run: idleTxBattery},
	} {
		for _, cause := range []struct {
			name string
			err  error
			want string
		}{
			{name: "unclassified", err: errors.New(privateSuffix), want: observationErrorClassUnclassified},
			{name: "wrapped deadline", err: fmt.Errorf("%s: %w", privateSuffix, context.DeadlineExceeded), want: observationErrorClassTimeout},
			{name: "access denied", err: errors.New("permission denied: " + privateSuffix), want: observationErrorClassAccessDenied},
		} {
			t.Run(helper.name+"/"+cause.name, func(t *testing.T) {
				calls := 0
				source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
					calls++
					return nil, cause.err
				}}
				settings := syntheticSettings(source)
				env, err := newProbeEnv(settings.withDefaults())
				if err != nil {
					t.Fatal("synthetic diagnostic environment failed")
				}
				// Exercise the real helper and Alert conversion/rendering,
				// without starting the separate 15s plan-wall battery.
				alert := alertFromFinding(settings, "1.3", "pg-state", "PostgreSQL transaction state", finding{
					probeId: "pg/" + helper.class, tier: helper.tier,
					class: helper.class, target: "pg-1", sustain: 2,
					evidence: helper.run(context.Background(), env),
				})
				want := helper.prefix + "error_class=" + cause.want
				if calls != 1 || alert.Evidence != want {
					t.Error("optional failure did not retain exactly one fixed diagnostic class")
				}
				if !strings.Contains(alert.Markdown(), want) {
					t.Error("rendered optional failure lost its fixed diagnostic class")
				}
				for _, rendered := range []string{alert.Evidence, alert.Markdown()} {
					for _, forbidden := range []string{credential, "192.0.2.34", privateSuffix} {
						if strings.Contains(rendered, forbidden) {
							t.Error("optional failure rendered an unregistered synthetic credential or source detail")
						}
					}
				}
			})
		}
	}
}

// A failed optional battery cannot erase a concrete count violation; its
// cached evidence must remain safe on later sustained observations.
func TestPostgresStateIdleBatteryErrorPrivacySurvivesCaching(t *testing.T) {
	const credential = "synthetic-idle-battery-unregistered-credential"
	summaryCalls, batteryCalls := 0, 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "count(*) FILTER") {
			summaryCalls++
			return []Row{{"0", "101", "1", "125"}}, nil
		}
		if !strings.Contains(query, "WITH idle AS MATERIALIZED") {
			t.Fatal("unexpected optional query")
		}
		batteryCalls++
		return nil, errors.New("password=" + credential)
	}}
	monitor := NewWithSignals(syntheticSettings(source), NewPostgresStateSignal())
	for tick := 0; tick < 2; tick++ {
		alerts, err := monitor.Run(context.Background())
		if err != nil || len(alerts) != 1 {
			t.Fatalf("optional failure erased the core condition: source_error=%t alerts=%d", err != nil, len(alerts))
		}
		alert := alerts[0]
		if alert.Class != "idle-in-tx" || alert.SignalID != "pg/idle-in-tx" || alert.Target != "pg-1" ||
			alert.Severity != SeverityWarn || alert.Sustain != 2 ||
			alert.Observed != "idle_in_tx=101 oldest_xact_s=1 active=0" {
			t.Fatal("optional failure changed the observed state condition")
		}
		want := "idle-tx battery failed: error_class=" + observationErrorClassUnclassified
		if !strings.HasPrefix(alert.Evidence, want) || !strings.Contains(alert.Markdown(), want) {
			t.Error("optional failure did not carry a fixed diagnostic class through the real signal")
		}
		for _, rendered := range []string{alert.Evidence, alert.Markdown()} {
			if strings.Contains(rendered, credential) {
				t.Error("real state alert retained an unregistered synthetic credential")
			}
		}
		if (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
			t.Fatal("privacy handling changed trip-cache provenance")
		}
	}
	if summaryCalls != 2 || batteryCalls != 1 {
		t.Fatal("privacy handling changed summary or battery cadence")
	}
}

// Virtual time exercises the real active-signal composition and both plan-wall
// failure sites without adding a production clock seam or a wall-clock wait.
func TestPostgresStatePlanWallErrorsRenderOnlyFixedClasses(t *testing.T) {
	const credential = "synthetic-plan-wall-unregistered-credential"
	for _, test := range []struct {
		name        string
		firstErr    error
		secondErr   error
		statsErr    error
		firstClass  string
		secondClass string
		statsClass  string
		empty       bool
	}{
		{name: "first snapshot", firstErr: errors.New("permission denied: password=" + credential), firstClass: observationErrorClassAccessDenied},
		{name: "second snapshot", secondErr: fmt.Errorf("password=%s: %w", credential, context.DeadlineExceeded), secondClass: observationErrorClassTimeout},
		{name: "both snapshots", firstErr: errors.New("password=" + credential), secondErr: errors.New("permission denied: password=" + credential), firstClass: observationErrorClassUnclassified, secondClass: observationErrorClassAccessDenied},
		{name: "pg_stats", statsErr: errors.New("decode failure: password=" + credential), statsClass: observationErrorClassInvalidResponse},
		{name: "snapshot and pg_stats", firstErr: errors.New("password=" + credential), statsErr: errors.New("permission denied: password=" + credential), firstClass: observationErrorClassUnclassified, statsClass: observationErrorClassAccessDenied},
		{name: "successful optional observations"},
		{name: "empty optional observations", empty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				summaryCalls, activeCalls, snapshotCalls, statsCalls := 0, 0, 0, 0
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					switch {
					case strings.Contains(query, "count(*) FILTER"):
						summaryCalls++
						return []Row{{"101", "0", "0", "125"}}, nil
					case strings.Contains(query, "GROUP BY query_id ORDER BY backends DESC LIMIT 5"):
						activeCalls++
						if test.empty {
							return nil, nil
						}
						return []Row{{"73", "2", "-:-", "SELECT synthetic_fixture"}}, nil
					case strings.Contains(query, "FROM pg_stat_statements s"):
						snapshotCalls++
						switch snapshotCalls {
						case 1:
							if test.firstErr != nil || test.empty {
								return nil, test.firstErr
							}
							return []Row{{"stmt", "73", "10", "100"}, {"idx", "synthetic_pair_index", "10", "0"}}, nil
						case 2:
							if test.secondErr != nil || test.empty {
								return nil, test.secondErr
							}
							return []Row{{"stmt", "73", "12", "160"}, {"idx", "synthetic_pair_index", "13", "0"}}, nil
						default:
							t.Fatal("cached plan-wall battery repeated its snapshots")
						}
					case strings.Contains(query, "FROM pg_stats WHERE tablename"):
						statsCalls++
						if test.statsErr != nil || test.empty {
							return nil, test.statsErr
						}
						return []Row{{"2", "{f,t}", "0.5,0.5"}}, nil
					default:
						t.Fatal("unexpected active diagnostic query")
					}
					return nil, nil
				}}
				settings := syntheticSettings(source)
				settings.Now = time.Now
				monitor := NewWithSignals(settings, NewPostgresStateSignal())
				started := time.Now()
				for tick := 0; tick < 2; tick++ {
					alerts, err := monitor.Run(context.Background())
					if err != nil || len(alerts) != 1 {
						t.Fatalf("plan-wall error erased the active condition: source_error=%t alerts=%d", err != nil, len(alerts))
					}
					alert := alerts[0]
					if alert.Class != "active-pileup" || alert.SignalID != "pg/active-pileup" || alert.Target != "pg-1" ||
						alert.Severity != SeverityPage || alert.Sustain != 2 ||
						alert.Observed != "active=101 idle_in_tx=0 total_client=125" {
						t.Fatal("optional plan-wall failure changed the core active condition")
					}
					for _, rendered := range []string{alert.Evidence, alert.Markdown()} {
						if strings.Contains(rendered, credential) {
							t.Error("plan-wall error rendered an unregistered synthetic credential")
						}
						for _, component := range []struct {
							label string
							class string
						}{
							{label: "snapshot delta failed: first_error_class=", class: test.firstClass},
							{label: "snapshot delta failed: second_error_class=", class: test.secondClass},
							{label: "pg_stats check failed: error_class=", class: test.statsClass},
						} {
							if component.class == "" {
								if strings.Contains(rendered, component.label) {
									t.Error("a successful component was assigned an error class")
								}
							} else if !strings.Contains(rendered, component.label+component.class) {
								t.Error("plan-wall failure lost its fixed owning component and error class")
							}
						}
					}
					wantDelta := test.firstErr == nil && test.secondErr == nil
					if strings.Contains(alert.Evidence, "pg_stat_statements 15s delta") != wantDelta {
						t.Fatal("partial snapshots changed delta availability")
					}
					if wantDelta && !test.empty && !strings.Contains(alert.Evidence, "calls_15s=2 current=30.0ms lifetime=13.3ms") {
						t.Fatal("successful statement delta changed")
					}
					if wantDelta && !test.empty && !strings.Contains(alert.Evidence, "rank=1 role=other-name-withheld delta=3") {
						t.Fatal("successful index delta changed")
					}
					wantStats := test.statsErr == nil && !test.empty
					if strings.Contains(alert.Evidence, "pg_stats transfer_contract.open: n_distinct=2") != wantStats {
						t.Fatal("independent pg_stats evidence was erased or invented")
					}
					if test.empty && strings.Contains(alert.Evidence, "healthy (both values present)") {
						t.Fatal("absent optional rows became a healthy pg_stats observation")
					}
					if (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
						t.Fatal("plan-wall error rendering changed cache provenance")
					}
					if time.Since(started) != 15*time.Second {
						t.Fatal("the real interval did not advance exactly once in virtual time")
					}
				}
				if summaryCalls != 2 || activeCalls != 1 || snapshotCalls != 2 || statsCalls != 1 {
					t.Fatal("plan-wall error rendering changed the bounded collection sequence")
				}
			})
		})
	}
}
