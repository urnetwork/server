package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Synthetic public-source observations only; no DB, subprocess or fixture.
func pointsPartialSettings(t *testing.T, snapshot []Row, schema []Row, schemaErr error, source []Row, sourceErr error, after func(int)) (SignalSettings, *[]string) {
	t.Helper()
	stages := []string{}
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		var rows []Row
		var err error
		switch query {
		case pointsSnapshotQuery:
			stages = append(stages, "snapshot")
			rows = snapshot
		case pointsOperatorSchemaQuery:
			stages = append(stages, "schema")
			rows, err = schema, schemaErr
		case pointsOperatorSourceQuery():
			stages = append(stages, "source")
			rows, err = source, sourceErr
		default:
			t.Error("unexpected points query")
			return nil, errors.New("unexpected points query")
		}
		if after != nil {
			after(len(stages))
		}
		return rows, err
	}})
	return settings, &stages
}

func requirePointsPartial(t *testing.T, alerts Alerts, layer string) {
	t.Helper()
	if len(alerts) != 2 {
		t.Fatal("validated stale warning and dependency unknown must both survive")
	}
	stale := requireAlertClass(t, alerts, "points-leaderboard-stale")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if stale.Severity != SeverityWarn || stale.Sustain != 2 || unknown.Severity != SeverityWarn ||
		unknown.Sustain != 2 || !unknown.unobservable || unknown.Target != "points-readiness/"+layer {
		t.Fatal("partial observation lost its existing severity, sustain or unknown contract")
	}
	if strings.Contains(alerts.Markdown(), "synthetic-private-value") {
		t.Fatal("partial result leaked source or malformed-cell text")
	}
}

func TestPointsPartialRetainsIndependentStaleAcrossDependencyFailures(t *testing.T) {
	for _, age := range []int64{7201, -1} {
		for _, control := range []struct {
			name      string
			schema    []Row
			schemaErr error
			source    []Row
			sourceErr error
			layer     string
			stages    string
		}{
			{name: "schema-read", schema: nil, schemaErr: errors.New("synthetic-private-value"), source: nil, sourceErr: nil, layer: "operator-schema", stages: "snapshot,schema"},
			{name: "schema-empty", schema: nil, schemaErr: nil, source: nil, sourceErr: nil, layer: "operator-schema", stages: "snapshot,schema"},
			{name: "schema-shape", schema: []Row{{"true", "synthetic-private-value"}}, schemaErr: nil, source: nil, sourceErr: nil, layer: "operator-schema", stages: "snapshot,schema"},
			{name: "schema-value", schema: []Row{{"synthetic-private-value"}}, schemaErr: nil, source: nil, sourceErr: nil, layer: "operator-schema", stages: "snapshot,schema"},
			{name: "source-read", schema: []Row{{"true"}}, schemaErr: nil, source: nil, sourceErr: errors.New("synthetic-private-value"), layer: "operator-source", stages: "snapshot,schema,source"},
			{name: "source-empty", schema: []Row{{"true"}}, schemaErr: nil, source: nil, sourceErr: nil, layer: "operator-source", stages: "snapshot,schema,source"},
			{name: "source-value", schema: []Row{{"true"}}, schemaErr: nil, source: []Row{{"12", "60", "synthetic-private-value", "0", "0"}}, sourceErr: nil, layer: "operator-source", stages: "snapshot,schema,source"},
			{name: "source-local-deadline", schema: []Row{{"true"}}, schemaErr: nil, source: nil, sourceErr: context.DeadlineExceeded, layer: "operator-source", stages: "snapshot,schema,source"},
			{name: "source-local-cancel", schema: []Row{{"true"}}, schemaErr: nil, source: nil, sourceErr: context.Canceled, layer: "operator-source", stages: "snapshot,schema,source"},
		} {
			settings, stages := pointsPartialSettings(t, syntheticPointsSnapshot(age, 12, true, 10, 0), control.schema, control.schemaErr, control.source, control.sourceErr, nil)
			alerts, err := NewWithSignals(settings, NewPointsReadinessSignal()).Run(context.Background())
			if err != nil || strings.Join(*stages, ",") != control.stages {
				t.Fatalf("age=%d case=%s: public partial result aborted or crossed the failed dependency", age, control.name)
			}
			requirePointsPartial(t, alerts, control.layer)
		}
	}
}

func TestPointsPartialSetupAndHealthyControls(t *testing.T) {
	for _, age := range []int64{60, 7200, 7201, -1} {
		settings, stages := pointsPartialSettings(t, syntheticPointsSnapshot(age, 12, true, 10, 0), []Row{{"true"}}, nil, syntheticPointsOperatorSource(12, 60, 0, 0, 0), nil, nil)
		alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
		if err != nil || strings.Join(*stages, ",") != "snapshot,schema,source" {
			t.Fatal("healthy dependency control failed")
		}
		if age == 7201 || age == -1 {
			if len(alerts) != 1 {
				t.Fatal("stale setup must have exactly one independent warning")
			}
			requireAlertClass(t, alerts, "points-leaderboard-stale")
		} else if len(alerts) != 0 {
			t.Fatal("healthy or exact two-hour boundary became a positive")
		}
	}
	settings, stages := pointsPartialSettings(t, syntheticPointsSnapshot(7201, 12, true, 10, 0), []Row{{"false"}}, nil, nil, errors.New("must not query"), nil)
	alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || strings.Join(*stages, ",") != "snapshot,schema" {
		t.Fatal("known rollout absence changed")
	}
	requireAlertClass(t, alerts, "points-leaderboard-stale")
	requireAlertClass(t, alerts, "points-operator-schema-unavailable")
}

func TestPointsPartialFreshDependencyFailureRemainsFatal(t *testing.T) {
	readErr := errors.New("synthetic-private-value")
	for _, schemaFailure := range []bool{true, false} {
		schema, schemaErr, sourceErr := []Row{{"true"}}, error(nil), readErr
		if schemaFailure {
			schema, schemaErr, sourceErr = nil, readErr, nil
		}
		settings, _ := pointsPartialSettings(t, syntheticPointsSnapshot(60, 12, true, 10, 0), schema, schemaErr, nil, sourceErr, nil)
		alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
		if !errors.Is(err, readErr) || len(alerts) != 0 {
			t.Fatal("fresh source failure became readiness or health")
		}
	}
}

func TestPointsPartialOriginalSnapshotUnavailableHasNoStale(t *testing.T) {
	for _, rows := range [][]Row{nil, {{"synthetic-private-value"}}, {{"7201", "12", "10", "true", "10", "11", "0", "0"}}} {
		settings, stages := pointsPartialSettings(t, rows, nil, errors.New("must not query"), nil, nil, nil)
		alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
		if strings.Join(*stages, ",") != "snapshot" {
			t.Fatal("invalid initial snapshot reached dependencies")
		}
		if len(rows) == 0 {
			if err != nil || len(alerts) != 1 {
				t.Fatal("known missing snapshot warning changed")
			}
			requireAlertClass(t, alerts, "points-leaderboard-unavailable")
		} else if err == nil || len(alerts) != 0 || strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatal("malformed original snapshot fabricated positive evidence or leaked content")
		}
	}
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, errors.New("synthetic-private-value") }})
	alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	if err == nil || len(alerts) != 0 {
		t.Fatal("failed original snapshot produced a partial result")
	}
}

func TestPointsPartialActualParentCancellationDiscardsFindings(t *testing.T) {
	for _, stopAt := range []int{0, 1, 2, 3} {
		for _, schemaReady := range []string{"true", "false"} {
			ctx, cancel := context.WithCancel(context.Background())
			settings, stages := pointsPartialSettings(t, syntheticPointsSnapshot(7201, 12, true, 10, 0), []Row{{schemaReady}}, nil, syntheticPointsOperatorSource(12, 60, 0, 0, 0), nil, func(stage int) {
				if stage == stopAt {
					cancel()
				}
			})
			if stopAt == 0 {
				cancel()
			}
			alerts, err := NewPointsReadinessSignal().Run(ctx, settings)
			cancel()
			if stopAt == 3 && schemaReady == "false" {
				continue
			} // The third read was intentionally skipped.
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 || len(*stages) != stopAt {
				t.Fatal("ended parent emitted a positive, healthy result or another query")
			}
		}
	}
}

func TestPointsPartialPublicExecutionLoopRetainsHeldUnknown(t *testing.T) {
	settings, _ := pointsPartialSettings(t, syntheticPointsSnapshot(7201, 12, true, 10, 0), nil, errors.New("synthetic-private-value"), nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	err := NewWithSignals(settings, NewPointsReadinessSignal()).RunExecutionLoop(ctx, RunLoopOptions{}, func(_ context.Context, _ Signal, execution SignalExecution) error {
		calls++
		if execution.Failed || len(execution.Alerts) != 0 {
			t.Error("partial execution became fatal or bypassed sustain")
		}
		requirePointsPartial(t, execution.HeldAlerts, "operator-schema")
		cancel()
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatal("public execution loop did not deliver the partial observation exactly once")
	}
}

func TestPointsPartialUnknownCannotClearUntilCompleteRecovery(t *testing.T) {
	signal := NewPointsReadinessSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	raised, cleared := 0, 0
	for run := 0; run < 6; run++ {
		age, sourceErr := int64(7201), error(nil)
		if run == 2 || run == 3 {
			sourceErr = errors.New("synthetic-private-value")
		}
		if run >= 4 {
			age = 60
		}
		missing := uint64(0)
		if run < 2 {
			missing = 1
		}
		settings, _ := pointsPartialSettings(t, syntheticPointsSnapshot(age, 12, true, 10, 0), []Row{{"true"}}, nil, syntheticPointsOperatorSource(12, 60, missing, 0, 0), sourceErr, nil)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("partial recovery sequence aborted")
		}
		ready, held := gate.split(signal, alerts)
		for _, transition := range notifier.observeWithLock(settings.Now().Add(time.Duration(run)*5*time.Minute), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held}) {
			if transition.alert.Class != "points-leaderboard-stale" && transition.alert.Class != "points-payment-link-missing" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
				if run != 5 {
					t.Fatal("unknown or first healthy observation cleared the stale incident")
				}
			}
		}
	}
	if raised != 2 || cleared != 2 {
		t.Fatal("stale and operator incidents require two complete healthy observations")
	}
}

func TestPointsPartialExpiredParentAndCanceledDependencyError(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	settings, stages := pointsPartialSettings(t, syntheticPointsSnapshot(7201, 12, true, 10, 0), nil, nil, nil, nil, nil)
	alerts, err := NewPointsReadinessSignal().Run(ctx, settings)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 || len(*stages) != 0 {
		t.Fatal("expired parent started an observation")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	settings, _ = pointsPartialSettings(t, syntheticPointsSnapshot(7201, 12, true, 10, 0), nil, errors.New("synthetic-private-value"), nil, nil, func(stage int) {
		if stage == 2 {
			cancel()
		}
	})
	alerts, err = NewPointsReadinessSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("parent cancellation was hidden behind partial dependency visibility")
	}
}
