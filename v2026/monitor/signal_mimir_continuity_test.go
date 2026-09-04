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
		if host.Name != "edge-0" {
			return "", fmt.Errorf("unexpected Mimir gateway %s", host.Name)
		}
		observedCommand = command
		return mimirContinuityFixtureWithMetric(t, timestamps, metric), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "edge-0", Roles: []string{"services"}},
		{Name: "pg-only", Roles: []string{"pg-primary"}},
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
		"observed boundary movement=30m0s over elapsed=30m0s",
		"right edge stayed fixed while its left edge advanced with wall clock",
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
