// Synthetic complete source observations exercise the registered bounded freshness diagnostic.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Small synthetic block numbers and generated JSON are not production snapshots.
func testingReliabilityFreshnessSnapshot() *reliabilityFreshnessSnapshot {
	const nowBlock int64 = 100000
	due := float64(600)
	value := &reliabilityFreshnessSnapshot{Schema: 1, ObservedUnix: float64(nowBlock * 60), SampleCount: 16, TaskRows: 1, TaskDueInSeconds: &due}
	drainAge := float64(30)
	value.Drain = &reliabilityFreshnessDrain{MaxExclusive: nowBlock - 2, UpdateAgeSeconds: &drainAge}
	for _, index := range []int{0, 1, 2, 1000} {
		maxBlock := nowBlock - 5
		window := reliabilityFreshnessWindow{Index: index, RunningMaxExclusive: &maxBlock}
		if index != 1000 {
			oldest, newest := maxBlock, maxBlock
			window.ScoreRows = 16
			window.OldestMaxExclusive = &oldest
			window.NewestMaxExclusive = &newest
		}
		value.Windows = append(value.Windows, window)
	}
	return value
}

// Exercise decoding through the public Signal API, not just a hand-filled evaluator struct.
func testingRunReliabilityFreshness(t testing.TB, value *reliabilityFreshnessSnapshot) Alerts {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if query != reliabilityFreshnessQuery {
			t.Fatal("signal bypassed its fixed bounded query")
		}
		return []Row{{string(body)}}, nil
	}}
	alerts, err := NewReliabilityFreshnessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// The thirty-minute schedule and exact forty-minute warning boundary are legitimate grace.
func TestReliabilityFreshnessExpectedCadenceIsHealthy(t *testing.T) {
	for _, ageMinutes := range []int64{5, 30, 40} {
		value := testingReliabilityFreshnessSnapshot()
		for index := range 3 {
			maxBlock := int64(value.ObservedUnix/60) - ageMinutes
			value.Windows[index].RunningMaxExclusive = &maxBlock
			value.Windows[index].OldestMaxExclusive = &maxBlock
			value.Windows[index].NewestMaxExclusive = &maxBlock
		}
		if alerts := testingRunReliabilityFreshness(t, value); len(alerts) != 0 {
			t.Fatalf("age=%d minutes unexpected alerts: %+v", ageMinutes, alerts)
		}
	}
}

// A fresh heartbeat cannot conceal sixty-minute-old published scores after running state advanced.
func TestReliabilityFreshnessActivePublicationLagPages(t *testing.T) {
	value := testingReliabilityFreshnessSnapshot()
	value.FreshClaims = 1
	oldMax := int64(value.ObservedUnix/60) - 60
	value.Windows[1].OldestMaxExclusive = &oldMax
	value.Windows[1].NewestMaxExclusive = &oldMax
	alerts := testingRunReliabilityFreshness(t, value)
	alert := requireAlertClass(t, alerts, "reliability-score-stale")
	if alert.Frame != "lookback-1" || alert.Severity != Severity(tierPage) || alert.Sustain != 2 {
		t.Fatalf("wrong threshold/identity: %+v", alert)
	}
	for _, want := range []string{"boundary=score-publication-lag", "task_state=fresh-claim-and-lease", "not score freshness or a dead task", "not a native-Quality count", "no usable covered/nondegraded blocks", "SIGNALS.md §2.15a"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("missing causal qualifier %q", want)
		}
	}
}

// A lagging running window is distinguishable from a later score-publication barrier.
func TestReliabilityFreshnessRunningLagWarnsWithoutInventingDeadOwner(t *testing.T) {
	value := testingReliabilityFreshnessSnapshot()
	oldMax := int64(value.ObservedUnix/60) - 41
	value.Windows[2].RunningMaxExclusive = &oldMax
	value.Windows[2].OldestMaxExclusive = &oldMax
	value.Windows[2].NewestMaxExclusive = &oldMax
	due := float64(-90)
	value.TaskDueInSeconds = &due
	alert := requireAlertClass(t, testingRunReliabilityFreshness(t, value), "reliability-score-stale")
	if alert.Severity != Severity(tierWarn) || alert.Sustain != 2 {
		t.Fatal("warning grace changed")
	}
	if !strings.Contains(alert.Observed, "boundary=running-window-lag") || !strings.Contains(alert.Observed, "task_state=overdue-without-fresh-claim") {
		t.Fatal("running/task boundary lost")
	}
}

// Upstream stale drain prevents false attribution of an intentional writer wait to publication.
func TestReliabilityFreshnessStaleDrainIsUnavailable(t *testing.T) {
	for _, staleTimestamp := range []bool{true, false} {
		value := testingReliabilityFreshnessSnapshot()
		if staleTimestamp {
			*value.Drain.UpdateAgeSeconds = 600
		} else {
			value.Drain.MaxExclusive = int64(value.ObservedUnix/60) - 10
		}
		oldMax := int64(value.ObservedUnix/60) - 90
		value.Windows[1].OldestMaxExclusive = &oldMax
		value.Windows[1].NewestMaxExclusive = &oldMax
		alerts := testingRunReliabilityFreshness(t, value)
		if len(alerts) != 1 || alerts[0].Class != "reliability-freshness-unobservable" {
			t.Fatalf("stale drain attributed to publication: %+v", alerts)
		}
	}
}

// Missing/partial histories remain unavailable and cannot resolve an earlier stale-score finding.
func TestReliabilityFreshnessMissingScoreIsNotFreshZero(t *testing.T) {
	for _, rows := range []int{0, 7} {
		value := testingReliabilityFreshnessSnapshot()
		value.Windows[0].ScoreRows = rows
		if rows == 0 {
			value.Windows[0].OldestMaxExclusive = nil
			value.Windows[0].NewestMaxExclusive = nil
		}
		findings := reliabilityFreshnessFindings(value, "synthetic-pg")
		unknown := false
		for _, finding := range findings {
			if finding.frame == "lookback-0" && finding.class == "reliability-freshness-unobservable" && !finding.healthy {
				unknown = true
			}
			if finding.frame == "lookback-0" && finding.class == "reliability-score-stale" && finding.healthy {
				t.Fatal("partial history falsely resolved stale publication")
			}
		}
		if !unknown {
			t.Fatal("missing or partial history silently became current")
		}
	}
}

// Concrete stale rows remain reportable even when some siblings lack evidence.
func TestReliabilityFreshnessPartialSamplePreservesStaleEvidence(t *testing.T) {
	value := testingReliabilityFreshnessSnapshot()
	value.Windows[1].ScoreRows = 7
	oldMax := int64(value.ObservedUnix/60) - 80
	value.Windows[1].OldestMaxExclusive = &oldMax
	value.Windows[1].NewestMaxExclusive = &oldMax
	alerts := testingRunReliabilityFreshness(t, value)
	requireAlertClass(t, alerts, "reliability-freshness-unobservable")
	requireAlertClass(t, alerts, "reliability-score-stale")
}

// Parser failures cannot fabricate a complete healthy observation or a phantom lookback.
func TestReliabilityFreshnessRejectsMalformedObservation(t *testing.T) {
	value := testingReliabilityFreshnessSnapshot()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]pgRow{nil, {{string(body), "extra"}}, {{string(body) + " trailing"}}, {{string(body) + " {}"}}, {{strings.Repeat(" ", 64*1024+1)}}} {
		if _, err := parseReliabilityFreshness(rows); err == nil {
			t.Fatal("malformed source observation accepted")
		}
	}
	value.Windows[3].Index = 3
	body, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseReliabilityFreshness([]pgRow{{string(body)}}); err == nil {
		t.Fatal("phantom client index 3 accepted as network stage")
	}
	value = testingReliabilityFreshnessSnapshot()
	value.Drain.UpdateAgeSeconds = nil
	body, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseReliabilityFreshness([]pgRow{{string(body)}}); err == nil {
		t.Fatal("unknown drain timestamp became a fresh zero age")
	}
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, errors.New("synthetic unavailable") }}
	if _, err := NewReliabilityFreshnessSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
		t.Fatal("source error became a healthy publication")
	}
}

// Future publisher ages, duplicate owners and absent metadata cannot make a false healthy frame.
func TestReliabilityFreshnessUnknownMarkersRetainVisibility(t *testing.T) {
	for _, kind := range []string{"drain", "running", "sample", "task"} {
		value := testingReliabilityFreshnessSnapshot()
		switch kind {
		case "drain":
			value.Drain = nil
		case "running":
			value.Windows[1].RunningMaxExclusive = nil
		case "sample":
			value.SampleCount = 0
			for index := range 3 {
				value.Windows[index].ScoreRows = 0
				value.Windows[index].OldestMaxExclusive = nil
				value.Windows[index].NewestMaxExclusive = nil
			}
		case "task":
			value.TaskRows = 2
		}
		requireAlertClass(t, testingRunReliabilityFreshness(t, value), "reliability-freshness-unobservable")
	}
}

// Selection uses the public registry and an exact finite key/range plan, not a population score scan.
func TestReliabilityFreshnessRegisteredAndBounded(t *testing.T) {
	found := false
	for _, signal := range NewSignals() {
		if signal.Key() == "reliability-freshness" {
			found = signal.Number() == "2.15a" && signal.ID() == "pg/reliability-freshness"
		}
	}
	if !found {
		t.Fatal("freshness probe not exposed by the signal API")
	}
	for _, fragment := range []string{"LIMIT 1", "SELECT DISTINCT client_id FROM heads", "r.client_id=s.client_id AND r.lookback_index=v.lookback_index", "(VALUES (0),(1),(2))", "run_once_key IN", "WHERE singleton_id=1", "(VALUES (0),(1),(2),(1000))"} {
		if !strings.Contains(reliabilityFreshnessQuery, fragment) {
			t.Errorf("bounded query lost %q", fragment)
		}
	}
	if strings.Count(reliabilityFreshnessQuery, "LIMIT 1") != 2 || strings.Contains(reliabilityFreshnessQuery, "FROM client_reliability ") || strings.Contains(reliabilityFreshnessQuery, "FROM client_connection_reliability_score") {
		t.Fatal("freshness widened into a raw/fleet history scan")
	}
}
