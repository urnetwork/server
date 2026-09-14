package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mimirContinuityFixture(t *testing.T, timestamps []time.Time) string {
	return mimirContinuityFixtureWithMetric(t, timestamps, map[string]string{})
}

func mimirContinuityFixtureWithMetric(t *testing.T, timestamps []time.Time, metric map[string]string) string {
	t.Helper()
	values := make([][]any, 0, len(timestamps))
	for _, timestamp := range timestamps {
		values = append(values, []any{timestamp.Unix(), "1"})
	}
	payload := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result": []any{map[string]any{
				"metric": metric,
				"values": values,
			}},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func mimirContinuityOmitRange(first, last time.Time) map[time.Time]bool {
	omit := map[time.Time]bool{}
	for timestamp := first; !timestamp.After(last); timestamp = timestamp.Add(mimirContinuityStep) {
		omit[timestamp] = true
	}
	return omit
}

func mimirContinuityTimes(start, end time.Time, omit map[time.Time]bool) []time.Time {
	timestamps := []time.Time{}
	for timestamp := start; !timestamp.After(end); timestamp = timestamp.Add(mimirContinuityStep) {
		if !omit[timestamp] {
			timestamps = append(timestamps, timestamp)
		}
	}
	return timestamps
}

func runMimirContinuitySynthetic(t *testing.T, timestamps []time.Time) ([]Alert, error, string) {
	now := syntheticSettings(&syntheticSource{}).Now().UTC().Truncate(mimirContinuityStep)
	return runMimirContinuitySyntheticAt(
		t, NewMimirContinuitySignal(), now, timestamps, map[string]string{},
	)
}

func runMimirContinuitySyntheticAt(
	t *testing.T,
	signal Signal,
	now time.Time,
	timestamps []time.Time,
	metric map[string]string,
) ([]Alert, error, string) {
	t.Helper()
	var observedCommand string
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "mimir-gateway.example.test" {
			return "", fmt.Errorf("unexpected Mimir gateway %s", host.Name)
		}
		observedCommand = command
		return mimirContinuityFixtureWithMetric(t, timestamps, metric), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "mimir-gateway.example.test", Roles: []string{"services"}},
		{Name: "postgres-only.example.test", Roles: []string{"pg-primary"}},
	}
	alerts, err := signal.Run(context.Background(), settings)
	return alerts, err, observedCommand
}

func TestMimirContinuitySignalSyntheticHealthyHistory(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now().UTC().Truncate(mimirContinuityStep)
	start := now.Add(-mimirContinuityWindow)
	alerts, err, command := runMimirContinuitySynthetic(
		t,
		mimirContinuityTimes(start, now, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("continuous Mimir history alerted: %+v", alerts)
	}
	for _, want := range []string{
		mimirContinuityMarker,
		"/prometheus/api/v1/query_range?",
		"urnetwork_build_info",
		"instance%21%3D%22%22",
		fmt.Sprintf("start=%d", start.Unix()),
		fmt.Sprintf("end=%d", now.Unix()),
		"step=300",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("continuity command lacks %q: %s", want, command)
		}
	}
}

func TestMimirContinuitySignalClassifiesMovingLeftEdgePastDefaultBoundaryAsQueryStoreRecovery(t *testing.T) {
	signal := NewMimirContinuitySignal()
	firstNow := time.Date(2026, 9, 4, 8, 15, 0, 0, time.UTC)
	firstMissingStart := time.Date(2026, 9, 3, 12, 35, 0, 0, time.UTC)
	missingEnd := time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
	firstAlerts, err, _ := runMimirContinuitySyntheticAt(
		t,
		signal,
		firstNow,
		mimirContinuityTimes(
			firstNow.Add(-mimirContinuityWindow), firstNow,
			mimirContinuityOmitRange(firstMissingStart, missingEnd),
		),
		map[string]string{"private_label": "must-not-cross-the-monitor-boundary"},
	)
	if err != nil {
		t.Fatal(err)
	}
	first := requireAlertClass(t, firstAlerts, "mimir-continuity-gap-unclassified")
	requireAlertOmits(t, first, "must-not-cross-the-monitor-boundary")

	secondNow := firstNow.Add(30 * time.Minute)
	secondMissingStart := firstMissingStart.Add(30 * time.Minute)
	alerts, err, _ := runMimirContinuitySyntheticAt(
		t,
		signal,
		secondNow,
		mimirContinuityTimes(
			secondNow.Add(-mimirContinuityWindow), secondNow,
			mimirContinuityOmitRange(secondMissingStart, missingEnd),
		),
		map[string]string{"private_label": "must-not-cross-the-monitor-boundary"},
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-query-store-visibility-gap")
	if alert.SignalNumber != "11.20" || alert.SignalKey != "mimir-continuity" {
		t.Fatalf("wrong continuity signal identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"classification=query-store-recovering",
		"historical_restoration_movement=30m0s historical_restoration_elapsed=30m0s",
		"right edge stayed fixed while its left edge advanced between observations",
		"individual movement need not match wall clock",
		"not permanent raw-sample loss",
		"operator architecture decision",
		"SIGNALS.md §11.20",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("continuity alert lacks %q: %s", want, markdown)
		}
	}
	requireAlertOmits(t, alert, "must-not-cross-the-monitor-boundary")
}

func TestMimirContinuitySignalClassifiesBatchedVisibilityAdvanceAndOmitsPrivateLabels(t *testing.T) {
	signal := NewMimirContinuitySignal()
	firstNow := time.Date(2099, 1, 8, 12, 0, 0, 0, time.UTC)
	firstMissingStart := time.Date(2099, 1, 7, 0, 0, 0, 0, time.UTC)
	missingEnd := time.Date(2099, 1, 7, 6, 0, 0, 0, time.UTC)
	privateMarker := "synthetic-private-label-must-not-cross-boundary"
	run := func(now, missingStart time.Time) []Alert {
		t.Helper()
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t,
			signal,
			now,
			mimirContinuityTimes(
				now.Add(-mimirContinuityWindow), now,
				mimirContinuityOmitRange(missingStart, missingEnd),
			),
			map[string]string{"synthetic_private_label": privateMarker},
		)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}

	first := requireAlertClass(t, run(firstNow, firstMissingStart), "mimir-continuity-gap-unclassified")
	requireAlertOmits(t, first, privateMarker)

	alerts := run(firstNow.Add(mimirContinuityStep), firstMissingStart.Add(4*mimirContinuityStep))
	alert := requireAlertClass(t, alerts, "mimir-query-store-visibility-gap")
	markdown := alert.Markdown()
	for _, want := range []string{
		"classification=query-store-recovering",
		"historical_restoration_movement=20m0s historical_restoration_elapsed=5m0s",
		"Query/store discovery can expose several steps in one batch",
		"individual movement need not match wall clock",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("batched continuity alert lacks %q: %s", want, markdown)
		}
	}
	requireAlertOmits(t, alert, privateMarker)
}

func TestMimirContinuityAnyLaterForwardMovementIsRecovery(t *testing.T) {
	baseNow := time.Date(2099, 2, 3, 12, 0, 0, 0, time.UTC)
	missingStart := time.Date(2099, 2, 2, 1, 0, 0, 0, time.UTC)
	missingEnd := time.Date(2099, 2, 2, 4, 0, 0, 0, time.UTC)
	gap := func(start, end time.Time) mimirContinuityGap {
		return mimirContinuityGap{
			previous: start.Add(-mimirContinuityStep),
			resumed:  end.Add(mimirContinuityStep),
			missing:  int(end.Sub(start)/mimirContinuityStep) + 1,
		}
	}

	for _, test := range []struct {
		name         string
		elapsed      time.Duration
		movement     time.Duration
		wantMovement time.Duration
		wantElapsed  time.Duration
	}{
		{name: "faster batch", elapsed: mimirContinuityStep, movement: 4 * mimirContinuityStep, wantMovement: 4 * mimirContinuityStep, wantElapsed: mimirContinuityStep},
		{name: "slower discovery", elapsed: 4 * mimirContinuityStep, movement: mimirContinuityStep, wantMovement: mimirContinuityStep, wantElapsed: 4 * mimirContinuityStep},
	} {
		probe := &mimirContinuityProbe{}
		first := probe.observeGaps(baseNow, []mimirContinuityGap{gap(missingStart, missingEnd)})
		if first[0].classification != mimirContinuityUnclassified {
			t.Fatalf("%s: first observation = %s, want unclassified", test.name, first[0].classification)
		}
		second := probe.observeGaps(
			baseNow.Add(test.elapsed),
			[]mimirContinuityGap{gap(missingStart.Add(test.movement), missingEnd)},
		)
		if second[0].classification != mimirContinuityRecovering {
			t.Fatalf("%s: later forward observation = %s, want recovering", test.name, second[0].classification)
		}
		if second[0].recoveryMovement != test.wantMovement || second[0].recoveryElapsed != test.wantElapsed {
			t.Fatalf(
				"%s: recovery movement/elapsed = %s/%s, want %s/%s",
				test.name, second[0].recoveryMovement, second[0].recoveryElapsed,
				test.wantMovement, test.wantElapsed,
			)
		}
	}
}

func TestMimirContinuityGapIdentityChangesResetRecovery(t *testing.T) {
	baseNow := time.Date(2099, 3, 4, 12, 0, 0, 0, time.UTC)
	missingStart := time.Date(2099, 3, 3, 1, 0, 0, 0, time.UTC)
	missingEnd := time.Date(2099, 3, 3, 4, 0, 0, 0, time.UTC)
	gap := func(start, end time.Time) mimirContinuityGap {
		return mimirContinuityGap{
			previous: start.Add(-mimirContinuityStep),
			resumed:  end.Add(mimirContinuityStep),
			missing:  int(end.Sub(start)/mimirContinuityStep) + 1,
		}
	}

	for _, test := range []struct {
		name  string
		third mimirContinuityGap
	}{
		{name: "left edge regression", third: gap(missingStart, missingEnd)},
		{name: "new right edge", third: gap(missingStart.Add(2*mimirContinuityStep), missingEnd.Add(mimirContinuityStep))},
	} {
		probe := &mimirContinuityProbe{}
		first := probe.observeGaps(baseNow, []mimirContinuityGap{gap(missingStart, missingEnd)})
		if first[0].classification != mimirContinuityUnclassified {
			t.Fatalf("%s: first observation = %s, want unclassified", test.name, first[0].classification)
		}
		second := probe.observeGaps(
			baseNow.Add(mimirContinuityStep),
			[]mimirContinuityGap{gap(missingStart.Add(mimirContinuityStep), missingEnd)},
		)
		if second[0].classification != mimirContinuityRecovering {
			t.Fatalf("%s: second observation = %s, want recovering", test.name, second[0].classification)
		}
		third := probe.observeGaps(baseNow.Add(2*mimirContinuityStep), []mimirContinuityGap{test.third})
		if third[0].classification != mimirContinuityUnclassified {
			t.Fatalf("%s: changed gap observation = %s, want unclassified", test.name, third[0].classification)
		}
	}
}

func TestMimirContinuitySignalClassifiesFixedPostBoundaryGapAsLoss(t *testing.T) {
	signal := NewMimirContinuitySignal()
	missingStart := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	missingEnd := time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
	firstNow := missingEnd.Add(
		mimirContinuityDefaultQueryStoreAfter + mimirContinuityStoreBoundarySlack + mimirContinuityStep,
	)
	for index, now := range []time.Time{firstNow, firstNow.Add(mimirContinuityStep)} {
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t,
			signal,
			now,
			mimirContinuityTimes(
				now.Add(-mimirContinuityWindow), now,
				mimirContinuityOmitRange(missingStart, missingEnd),
			),
			map[string]string{},
		)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			requireAlertClass(t, alerts, "mimir-continuity-gap-unclassified")
			continue
		}
		alert := requireAlertClass(t, alerts, "mimir-ingestion-gap")
		markdown := alert.Markdown()
		for _, want := range []string{
			"classification=fixed-loss",
			"fixed bounded gap",
			"12-hour query-store default",
			"default recent-store split can no longer explain it",
			"real historical observation loss",
		} {
			if !strings.Contains(markdown, want) {
				t.Errorf("fixed continuity alert lacks %q: %s", want, markdown)
			}
		}
		for _, other := range alerts {
			if other.Class == "mimir-query-store-visibility-gap" {
				t.Fatalf("fixed gap was also classified as recovery: %+v", alerts)
			}
		}
	}
}

func TestMimirContinuityStoreDefaultBoundaryRequiresRepeatedObservation(t *testing.T) {
	gap := mimirContinuityGap{
		previous: time.Date(2026, 9, 3, 19, 40, 0, 0, time.UTC),
		resumed:  time.Date(2026, 9, 3, 20, 5, 0, 0, time.UTC),
		missing:  4,
	}
	boundary := gap.missingEnd().Add(
		mimirContinuityDefaultQueryStoreAfter + mimirContinuityStoreBoundarySlack,
	)
	probe := &mimirContinuityProbe{}
	first := probe.observeGaps(boundary.Add(-mimirContinuityStep), []mimirContinuityGap{gap})
	second := probe.observeGaps(boundary, []mimirContinuityGap{gap})
	if first[0].classification != mimirContinuityUnclassified {
		t.Fatalf("first pre-boundary observation = %s, want unclassified", first[0].classification)
	}
	if second[0].classification != mimirContinuityFixedLoss {
		t.Fatalf("repeated observation at default boundary = %s, want fixed loss", second[0].classification)
	}
}

func TestMimirContinuitySignalReclassifiesRecoveryThatStopsAdvancingAsFixedLoss(t *testing.T) {
	signal := NewMimirContinuitySignal()
	missingStart := time.Date(2026, 9, 3, 12, 35, 0, 0, time.UTC)
	missingEnd := time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 4, 8, 15, 0, 0, time.UTC)
	run := func(at, left time.Time) []Alert {
		t.Helper()
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t,
			signal,
			at,
			mimirContinuityTimes(
				at.Add(-mimirContinuityWindow), at,
				mimirContinuityOmitRange(left, missingEnd),
			),
			map[string]string{},
		)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}

	requireAlertClass(t, run(now, missingStart), "mimir-continuity-gap-unclassified")
	now = now.Add(30 * time.Minute)
	missingStart = missingStart.Add(30 * time.Minute)
	requireAlertClass(t, run(now, missingStart), "mimir-query-store-visibility-gap")

	// One unchanged cadence is tolerated as range/discovery jitter.
	now = now.Add(mimirContinuityStep)
	requireAlertClass(t, run(now, missingStart), "mimir-query-store-visibility-gap")

	// A second unchanged cadence beyond the store boundary is a fixed residual.
	now = now.Add(mimirContinuityStep)
	alert := requireAlertClass(t, run(now, missingStart), "mimir-ingestion-gap")
	if !strings.Contains(alert.Markdown(), "classification=fixed-loss") {
		t.Fatalf("stopped recovery did not preserve fixed-loss evidence:\n%s", alert.Markdown())
	}
}

func TestMimirContinuityHealthyFindingsResolveEveryClassification(t *testing.T) {
	findings := mimirContinuityHealthyFindings(nil)
	classes := map[string]bool{}
	for _, finding := range findings {
		if !finding.healthy || finding.target != "mimir-global-continuity" {
			t.Fatalf("invalid healthy continuity finding: %+v", finding)
		}
		classes[finding.class] = true
	}
	for _, class := range []string{
		"mimir-ingestion-gap",
		"mimir-query-store-visibility-gap",
		"mimir-continuity-gap-unclassified",
	} {
		if !classes[class] {
			t.Errorf("healthy continuity findings omit %s: %+v", class, findings)
		}
	}
}

func TestMimirContinuitySignalIgnoresBoundedJitterAndRangeEdges(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now().UTC().Truncate(mimirContinuityStep)
	start := now.Add(-mimirContinuityWindow)
	interiorStart := start.Add(2 * time.Hour)
	interiorEnd := now.Add(-2 * time.Hour)
	omit := map[time.Time]bool{
		interiorStart.Add(10 * mimirContinuityStep): true,
		interiorStart.Add(11 * mimirContinuityStep): true,
	}
	alerts, err, _ := runMimirContinuitySynthetic(
		t,
		mimirContinuityTimes(interiorStart, interiorEnd, omit),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("two missing interior steps or absent range edges alerted: %+v", alerts)
	}
}

func TestMimirContinuitySignalRejectsAbsentControl(t *testing.T) {
	_, err, _ := runMimirContinuitySynthetic(t, nil)
	if err == nil || !strings.Contains(err.Error(), "no continuity control samples") {
		t.Fatalf("absent continuity control error = %v", err)
	}
}

func TestFindMimirContinuityGapsRejectsIrregularTimestamp(t *testing.T) {
	_, err := findMimirContinuityGaps([]time.Time{
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 7, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "irregular evaluation timestamps") {
		t.Fatalf("irregular timestamp error = %v", err)
	}
}

// Repeated stationary comparisons must not refresh or present historical
// restoration as movement during the current comparison.
func TestMimirContinuityNarrationSeparatesCurrentAndHistoricalRestoration(t *testing.T) {
	signal := NewMimirContinuitySignal()
	firstNow := time.Date(2099, 4, 8, 12, 0, 0, 0, time.UTC)
	firstStart := firstNow.Add(-4 * time.Hour)
	missingEnd := firstNow.Add(-2 * time.Hour)
	privateMarker := "synthetic-private-mimir-label-must-not-render"
	run := func(now, missingStart time.Time) []Alert {
		t.Helper()
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t, signal, now,
			mimirContinuityTimes(
				now.Add(-mimirContinuityWindow), now,
				mimirContinuityOmitRange(missingStart, missingEnd),
			),
			map[string]string{"synthetic_private_label": privateMarker},
		)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}

	requireAlertClass(t, run(firstNow, firstStart), "mimir-continuity-gap-unclassified")
	advancedStart := firstStart.Add(4 * mimirContinuityStep)
	moving := requireAlertClass(t, run(firstNow.Add(mimirContinuityStep), advancedStart), "mimir-query-store-visibility-gap")
	for index := 1; index <= 3; index++ {
		now := firstNow.Add(time.Duration(index+1) * mimirContinuityStep)
		alert := requireAlertClass(t, run(now, advancedStart), "mimir-query-store-visibility-gap")
		if alert.SignalNumber != "11.20" || alert.SignalKey != "mimir-continuity" ||
			alert.Severity != SeverityWarn || alert.Sustain != 1 {
			t.Fatalf("stationary comparison changed the existing Alert contract: %+v", alert)
		}
		if strings.Contains(alert.Symptom, "progressively restoring") {
			t.Fatalf("stationary comparison %d still claims progressive restoration: %s", index, alert.Symptom)
		}
		for _, want := range []string{
			"current_moving_gaps=0 current_stationary_gaps=1 current_uncompared_gaps=0",
			fmt.Sprintf("current_comparison=available current_boundary_movement=0s current_elapsed=5m0s stationary_observations=%d", index),
			"historical_restoration_movement=20m0s historical_restoration_elapsed=5m0s",
			"21 five-minute control evaluations remain unavailable",
			"unchanged on the latest comparison",
			"anchor observation to the last positive boundary advance",
			"not permanent raw-sample loss",
			"SIGNALS.md §11.20",
		} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Errorf("stationary comparison %d lacks %q: %s", index, want, alert.Markdown())
			}
		}
		requireAlertOmits(t, alert, privateMarker)
	}

	for _, want := range []string{
		"current_moving_gaps=1 current_stationary_gaps=0 current_uncompared_gaps=0",
		"current_comparison=available current_boundary_movement=20m0s current_elapsed=5m0s stationary_observations=0",
		"advanced on the latest comparison",
	} {
		if !strings.Contains(moving.Markdown(), want) {
			t.Errorf("moving comparison lacks %q: %s", want, moving.Markdown())
		}
	}
	resumed := requireAlertClass(
		t, run(firstNow.Add(5*mimirContinuityStep), advancedStart.Add(mimirContinuityStep)),
		"mimir-query-store-visibility-gap",
	)
	for _, want := range []string{
		"current_moving_gaps=1 current_stationary_gaps=0 current_uncompared_gaps=0",
		"current_comparison=available current_boundary_movement=5m0s current_elapsed=5m0s stationary_observations=0",
		"historical_restoration_movement=25m0s historical_restoration_elapsed=25m0s",
		"20 five-minute control evaluations remain unavailable",
	} {
		if !strings.Contains(resumed.Markdown(), want) {
			t.Errorf("resumed comparison lacks %q: %s", want, resumed.Markdown())
		}
	}
	requireAlertOmits(t, moving, privateMarker)
	requireAlertOmits(t, resumed, privateMarker)
}

// Group narration must not apply the largest stationary gap's state to a
// smaller gap that advanced during the same comparison.
func TestMimirContinuityNarrationAggregatesMovingAndStationaryGaps(t *testing.T) {
	signal := NewMimirContinuitySignal()
	firstNow := time.Date(2099, 5, 8, 12, 0, 0, 0, time.UTC)
	firstStart := firstNow.Add(-9 * time.Hour)
	firstEnd := firstNow.Add(-6 * time.Hour)
	secondStart := firstNow.Add(-4 * time.Hour)
	secondEnd := firstNow.Add(-2 * time.Hour)
	run := func(now, leftFirst, leftSecond time.Time) []Alert {
		t.Helper()
		omit := mimirContinuityOmitRange(leftFirst, firstEnd)
		for timestamp := range mimirContinuityOmitRange(leftSecond, secondEnd) {
			omit[timestamp] = true
		}
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t, signal, now,
			mimirContinuityTimes(now.Add(-mimirContinuityWindow), now, omit),
			map[string]string{},
		)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}
	requireAlertClass(t, run(firstNow, firstStart, secondStart), "mimir-continuity-gap-unclassified")
	moving := requireAlertClass(
		t, run(firstNow.Add(mimirContinuityStep), firstStart.Add(mimirContinuityStep), secondStart.Add(mimirContinuityStep)),
		"mimir-query-store-visibility-gap",
	)
	mixed := requireAlertClass(
		t, run(firstNow.Add(2*mimirContinuityStep), firstStart.Add(mimirContinuityStep), secondStart.Add(2*mimirContinuityStep)),
		"mimir-query-store-visibility-gap",
	)
	for _, want := range []string{
		"current_moving_gaps=1 current_stationary_gaps=1 current_uncompared_gaps=0",
		"1 moving, 1 stationary, 0 uncompared gap(s)",
		"59 five-minute control evaluations remain unavailable",
		"current_comparison=available current_boundary_movement=0s current_elapsed=5m0s stationary_observations=1",
		"current_comparison=available current_boundary_movement=5m0s current_elapsed=5m0s stationary_observations=0",
		"historical_restoration_movement=5m0s historical_restoration_elapsed=5m0s",
		"historical_restoration_movement=10m0s historical_restoration_elapsed=10m0s",
	} {
		if !strings.Contains(mixed.Markdown(), want) {
			t.Errorf("mixed gap group lacks %q: %s", want, mixed.Markdown())
		}
	}
	if !strings.Contains(moving.Markdown(), "current_moving_gaps=2 current_stationary_gaps=0 current_uncompared_gaps=0") {
		t.Fatalf("all-moving group lost per-gap aggregation: %s", moving.Markdown())
	}
}

// No previous comparable observation is different from an observed zero delta.
func TestMimirContinuityNarrationReportsUnavailableFirstComparison(t *testing.T) {
	now := time.Date(2099, 6, 8, 12, 0, 0, 0, time.UTC)
	alerts, err, _ := runMimirContinuitySyntheticAt(
		t, NewMimirContinuitySignal(), now,
		mimirContinuityTimes(
			now.Add(-mimirContinuityWindow), now,
			mimirContinuityOmitRange(now.Add(-4*time.Hour), now.Add(-2*time.Hour)),
		),
		map[string]string{},
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-continuity-gap-unclassified")
	for _, want := range []string{
		"current_moving_gaps=0 current_stationary_gaps=0 current_uncompared_gaps=1",
		"current_comparison=unavailable current_boundary_movement=unknown current_elapsed=unknown stationary_observations=0",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("first comparison lacks %q: %s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, "historical_restoration_movement=")
}

// An assessment without a comparator cannot self-define a moving or stationary
// recovering group, even when historical restoration evidence is supplied.
func TestMimirContinuityNarrationDoesNotInventAnUncomparedGroupDelta(t *testing.T) {
	now := time.Date(2099, 7, 8, 12, 0, 0, 0, time.UTC)
	gap := mimirContinuityGap{
		previous: now.Add(-4 * time.Hour),
		resumed:  now.Add(-2 * time.Hour),
		missing:  23,
	}
	result := mimirContinuityGapFinding(
		mimirContinuityRecovering,
		[]mimirContinuityAssessment{{
			gap: gap, classification: mimirContinuityRecovering,
			recoveryMovement: 20 * time.Minute, recoveryElapsed: 5 * time.Minute,
		}},
		now.Add(-mimirContinuityWindow), now, "mimir-gateway.example.test",
	)
	for _, want := range []string{
		"current_moving_gaps=0 current_stationary_gaps=0 current_uncompared_gaps=1",
		"current_comparison=unavailable current_boundary_movement=unknown current_elapsed=unknown",
		"0 moving, 0 stationary, 1 uncompared gap(s)",
	} {
		if !strings.Contains(result.observed+"\n"+result.evidence+"\n"+result.symptom, want) {
			t.Errorf("uncompared group lacks %q: %+v", want, result)
		}
	}
	if strings.Contains(result.symptom, "advanced on the latest comparison") ||
		strings.Contains(result.symptom, "unchanged on the latest comparison") {
		t.Fatalf("uncompared group fabricated a comparison: %s", result.symptom)
	}
}

// Existing identity/time regression reset rules must discard both comparator
// and historical restoration narration.
func TestMimirContinuityNarrationResetsComparisonEvidenceWithHistory(t *testing.T) {
	for _, test := range []struct {
		name       string
		nowShift   time.Duration
		startShift time.Duration
		endShift   time.Duration
	}{
		{name: "left edge regression", nowShift: 10 * time.Minute, startShift: 0, endShift: 0},
		{name: "new right edge", nowShift: 10 * time.Minute, startShift: 20 * time.Minute, endShift: 5 * time.Minute},
		{name: "same evaluation time", nowShift: 5 * time.Minute, startShift: 20 * time.Minute, endShift: 0},
		{name: "evaluation time regression", nowShift: 0, startShift: 20 * time.Minute, endShift: 0},
	} {
		signal := NewMimirContinuitySignal()
		firstNow := time.Date(2099, 8, 8, 12, 0, 0, 0, time.UTC)
		firstStart := firstNow.Add(-4 * time.Hour)
		firstEnd := firstNow.Add(-2 * time.Hour)
		run := func(now, start, end time.Time) []Alert {
			t.Helper()
			alerts, err, _ := runMimirContinuitySyntheticAt(
				t, signal, now,
				mimirContinuityTimes(
					now.Add(-mimirContinuityWindow), now,
					mimirContinuityOmitRange(start, end),
				),
				map[string]string{},
			)
			if err != nil {
				t.Fatal(err)
			}
			return alerts
		}
		requireAlertClass(t, run(firstNow, firstStart, firstEnd), "mimir-continuity-gap-unclassified")
		requireAlertClass(t, run(firstNow.Add(mimirContinuityStep), firstStart.Add(4*mimirContinuityStep), firstEnd), "mimir-query-store-visibility-gap")
		alert := requireAlertClass(
			t, run(firstNow.Add(test.nowShift), firstStart.Add(test.startShift), firstEnd.Add(test.endShift)),
			"mimir-continuity-gap-unclassified",
		)
		if !strings.Contains(alert.Markdown(), "current_comparison=unavailable current_boundary_movement=unknown current_elapsed=unknown stationary_observations=0") {
			t.Errorf("%s retained an invalid comparator: %s", test.name, alert.Markdown())
		}
		requireAlertOmits(t, alert, "historical_restoration_movement=")
	}
}

// Healthy local reduction must remove gap history without adding active Alerts
// or turning a later new gap into a continuation of old restoration.
func TestMimirContinuityNarrationHealthyHistoryReset(t *testing.T) {
	signal := NewMimirContinuitySignal()
	firstNow := time.Date(2099, 9, 8, 12, 0, 0, 0, time.UTC)
	firstStart := firstNow.Add(-4 * time.Hour)
	missingEnd := firstNow.Add(-2 * time.Hour)
	run := func(now time.Time, omit map[time.Time]bool) []Alert {
		t.Helper()
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t, signal, now,
			mimirContinuityTimes(now.Add(-mimirContinuityWindow), now, omit),
			map[string]string{},
		)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}
	requireAlertClass(t, run(firstNow, mimirContinuityOmitRange(firstStart, missingEnd)), "mimir-continuity-gap-unclassified")
	requireAlertClass(
		t, run(firstNow.Add(mimirContinuityStep), mimirContinuityOmitRange(firstStart.Add(4*mimirContinuityStep), missingEnd)),
		"mimir-query-store-visibility-gap",
	)
	if alerts := run(firstNow.Add(2*mimirContinuityStep), nil); len(alerts) != 0 {
		t.Fatalf("healthy history emitted active Alerts: %+v", alerts)
	}
	alert := requireAlertClass(
		t, run(firstNow.Add(3*mimirContinuityStep), mimirContinuityOmitRange(firstStart.Add(4*mimirContinuityStep), missingEnd)),
		"mimir-continuity-gap-unclassified",
	)
	if !strings.Contains(alert.Markdown(), "current_comparison=unavailable") {
		t.Fatalf("healthy reset retained the prior gap comparator: %s", alert.Markdown())
	}
	requireAlertOmits(t, alert, "historical_restoration_movement=")
}

// Pre-boundary stationary history still crosses into the existing fixed-loss
// state; later forward movement retains the existing recovering transition.
func TestMimirContinuityNarrationPreservesStoreBoundaryTransitions(t *testing.T) {
	signal := NewMimirContinuitySignal()
	probe := &mimirContinuityProbe{}
	if probe.cadence() != 5*time.Minute || probe.tier() != tierWarn ||
		mimirContinuityMissingSteps != 3 || mimirContinuityWindow != 7*24*time.Hour ||
		mimirContinuityDefaultQueryStoreAfter+mimirContinuityStoreBoundarySlack != 12*time.Hour+10*time.Minute {
		t.Fatal("existing continuity threshold, cadence, tier, or store boundary changed")
	}
	firstNow := time.Date(2099, 10, 8, 12, 0, 0, 0, time.UTC)
	firstStart := firstNow.Add(-4 * time.Hour)
	missingEnd := firstNow.Add(-2 * time.Hour)
	run := func(now, start time.Time) []Alert {
		t.Helper()
		alerts, err, _ := runMimirContinuitySyntheticAt(
			t, signal, now,
			mimirContinuityTimes(
				now.Add(-mimirContinuityWindow), now,
				mimirContinuityOmitRange(start, missingEnd),
			),
			map[string]string{},
		)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}
	requireAlertClass(t, run(firstNow, firstStart), "mimir-continuity-gap-unclassified")
	advancedStart := firstStart.Add(4 * mimirContinuityStep)
	requireAlertClass(t, run(firstNow.Add(mimirContinuityStep), advancedStart), "mimir-query-store-visibility-gap")
	for index := 2; index <= 4; index++ {
		requireAlertClass(
			t, run(firstNow.Add(time.Duration(index)*mimirContinuityStep), advancedStart),
			"mimir-query-store-visibility-gap",
		)
	}
	boundary := missingEnd.Add(mimirContinuityDefaultQueryStoreAfter + mimirContinuityStoreBoundarySlack)
	fixed := requireAlertClass(t, run(boundary, advancedStart), "mimir-ingestion-gap")
	if !strings.Contains(fixed.Markdown(), "classification=fixed-loss") ||
		!strings.Contains(fixed.Markdown(), "historical_restoration_movement=20m0s historical_restoration_elapsed=5m0s") {
		t.Fatalf("boundary transition changed or refreshed historical restoration: %s", fixed.Markdown())
	}
	resumed := requireAlertClass(
		t, run(boundary.Add(mimirContinuityStep), advancedStart.Add(mimirContinuityStep)),
		"mimir-query-store-visibility-gap",
	)
	if !strings.Contains(resumed.Markdown(), "current_comparison=available current_boundary_movement=5m0s current_elapsed=5m0s stationary_observations=0") {
		t.Fatalf("post-boundary forward movement changed existing recovery: %s", resumed.Markdown())
	}
}
