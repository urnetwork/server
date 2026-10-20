package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Exercise the required COUNT through the public monitor adapter, including
// its fixed visibility class and the stop before optional diagnostic reads.
func requireTaskCanaryCountUnknown(t *testing.T, rows []Row) {
	t.Helper()
	requiredReads, optionalReads := 0, 0
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, taskCanaryAggregateFixture().queryMarker) {
				requiredReads++
				return rows, nil
			}
			optionalReads++
			return nil, nil
		},
		localFn: func(string, ...string) (string, error) {
			optionalReads++
			return "", nil
		},
	}
	signal := NewTaskCanariesSignal()
	alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(t.Context())
	if err == nil || requiredReads != 1 || optionalReads != 0 {
		t.Fatal("invalid required canary count did not fail before optional reads")
	}
	requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	var jsonl bytes.Buffer
	if err := alerts.WriteJSONL(&jsonl); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-private-count", "fixture-count-only"} {
		if strings.Contains(err.Error(), forbidden) || strings.Contains(string(encoded), forbidden) || strings.Contains(jsonl.String(), forbidden) {
			t.Error("required count error or serialized alert disclosed a rejected scalar")
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, forbidden)
		}
	}
}

func TestTaskCanaryRequiredCountMalformedIsUnknown(t *testing.T) {
	for _, count := range []string{"synthetic-private-count password=fixture-count-only", "NaN", "Inf", "0x0"} {
		requireTaskCanaryCountUnknown(t, []Row{{count}})
	}
}

func TestTaskCanaryRequiredCountEmptyIsUnknown(t *testing.T) {
	for _, count := range []string{"", " ", "\t"} {
		requireTaskCanaryCountUnknown(t, []Row{{count}})
	}
}

func TestTaskCanaryRequiredCountFractionIsUnknown(t *testing.T) {
	for _, count := range []string{"0.9", "12.9", "1e3"} {
		requireTaskCanaryCountUnknown(t, []Row{{count}})
	}
}

func TestTaskCanaryRequiredCountNegativeIsUnknown(t *testing.T) {
	for _, count := range []string{"-1", "-9223372036854775808"} {
		requireTaskCanaryCountUnknown(t, []Row{{count}})
	}
}

func TestTaskCanaryRequiredCountOverflowIsUnknown(t *testing.T) {
	for _, count := range []string{"9223372036854775808", strings.Repeat("9", 128)} {
		requireTaskCanaryCountUnknown(t, []Row{{count}})
	}
}

func TestTaskCanaryRequiredCountShapeRemainsUnknown(t *testing.T) {
	for _, test := range requiredAggregateBadShapes(Row{"12"}) {
		requireTaskCanaryCountUnknown(t, test.rows)
	}
}

func TestTaskCanaryRequiredCountPositiveAndBigintMaximumStayQuiet(t *testing.T) {
	for _, count := range []string{"1", "12", "9223372036854775807"} {
		requiredReads, optionalReads := 0, 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, taskCanaryAggregateFixture().queryMarker) {
				requiredReads++
				return []Row{{count}}, nil
			}
			optionalReads++
			return nil, nil
		}}
		signal := NewTaskCanariesSignal()
		alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(t.Context())
		if err != nil || len(alerts) != 0 || requiredReads != 1 || optionalReads == 0 || signal.Cadence() != time.Minute {
			t.Fatal("valid positive bigint count changed quiet canary behavior or optional empty groups")
		}
	}
}

func TestTaskCanaryRequiredCountZeroKeepsPageWithUnavailableBattery(t *testing.T) {
	requiredReads, failureReads := 0, 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, taskCanaryAggregateFixture().queryMarker) {
			requiredReads++
			return []Row{{"0"}}, nil
		}
		if strings.Contains(query, "WITH failures AS") {
			failureReads++
			if failureReads == 1 {
				return nil, errors.New("permission denied: synthetic-private-count battery")
			}
		}
		return nil, nil
	}}
	signal := NewTaskCanariesSignal()
	alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(t.Context())
	if err != nil || requiredReads != 1 || len(alerts) != 1 {
		t.Fatal("valid canary zero lost its independent PAGE when optional battery failed")
	}
	alert := requireAlertClass(t, alerts, "canary-dead")
	if alert.SignalID != signal.ID() || alert.Target != "pg-1" || alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Error("valid canary zero changed identity or PAGE policy")
	}
	if !strings.Contains(alert.Evidence, "task_error_battery=unavailable error_class="+observationErrorClassAccessDenied) {
		t.Error("optional battery failure lost its fixed qualifier")
	}
	requireAlertOmits(t, alert, "synthetic-private-count")
}

func TestTaskCanaryRequiredCountPositiveKeepsOverdueAndParked(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"SyntheticLongTask", "7200", "900", "t", "14400"}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return []Row{{"SyntheticParkedTask", "10", "10", "0", "23", "3600", "synthetic OOM", "120"}}, nil
		default:
			return nil, nil
		}
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewTaskCanariesSignal()).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "task-overdue")
	requireAlertClass(t, alerts, "task-parked")
	for _, alert := range alerts {
		if alert.Class == "canary-dead" || alert.Class == "cannot-observe" {
			t.Error("valid count contaminated independent task findings")
		}
	}
}

func TestTaskCanaryRequiredCountPositiveKeepsFindingBeforePartialSource(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
			return []Row{{"12"}}, nil
		case strings.Contains(query, "WITH history AS"):
			return []Row{{"SyntheticLongTask", "7200", "900", "t", "14400"}}, nil
		case strings.Contains(query, "WITH failures AS"):
			return nil, errors.New("permission denied: synthetic-private-count later component")
		default:
			return nil, nil
		}
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewTaskCanariesSignal()).Run(t.Context())
	if err != nil || len(alerts) != 2 {
		t.Fatal("later component failure erased completed independent task evidence")
	}
	requireAlertClass(t, alerts, "task-overdue")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if unknown.Observed != "error_class="+observationErrorClassAccessDenied {
		t.Error("partial task source lost its fixed source cause")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-private-count")
	}
}

func TestTaskCanaryRequiredCountInvalidKeepsIndependentSignal(t *testing.T) {
	canaryReads, contractReads := 0, 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, taskCanaryAggregateFixture().queryMarker):
			canaryReads++
			return []Row{{"-1"}}, nil
		case strings.Contains(query, "create_time >= date_trunc('minute',now())"):
			contractReads++
			return []Row{{"0"}}, nil
		default:
			t.Error("invalid required count reached an unobserved optional component")
			return nil, nil
		}
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewTaskCanariesSignal(), NewContractRateSignal()).Run(t.Context())
	if err == nil || len(alerts) != 2 || canaryReads != 1 || contractReads != 1 {
		t.Fatal("invalid count erased an independent public signal or looked healthy")
	}
	requireAlertClass(t, alerts, "cannot-observe")
	requireAlertClass(t, alerts, "contracts-collapse")
}

func TestTaskCanaryRequiredCountSourceErrorPreservesCause(t *testing.T) {
	sourceErr := errors.New("permission denied: synthetic-private-count source")
	reads := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		reads++
		return nil, sourceErr
	}}
	signal := NewTaskCanariesSignal()
	alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(t.Context())
	if !errors.Is(err, sourceErr) || reads != 1 {
		t.Fatal("required source failure lost its original cause or repeated its read")
	}
	requireAggregateVisibility(t, signal, alerts, observationErrorClassAccessDenied)
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-private-count")
	}
}

func TestTaskCanaryRequiredCountCancellationPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		reads++
		cancel()
		return nil, ctx.Err()
	}}
	signal := NewTaskCanariesSignal()
	alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(ctx)
	if !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatal("required count cancellation lost its owner cause")
	}
	requireAggregateVisibility(t, signal, alerts, observationErrorClassCanceled)
}
