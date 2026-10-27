package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const convergenceIdentityPrivate = "synthetic-private-target-source-canary"

func requireConvergenceIdentityPrivacy(t *testing.T, alerts Alerts) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal("could not encode convergence alerts")
	}
	var jsonl strings.Builder
	if err := alerts.WriteJSONL(&jsonl); err != nil {
		t.Fatal("could not write convergence JSONL")
	}
	for _, value := range []string{string(encoded), alerts.Markdown(), jsonl.String(), fmt.Sprintf("%+v", alerts)} {
		if strings.Contains(value, convergenceIdentityPrivate) {
			t.Fatal("private failed-source input reached convergence public output")
		}
	}
}

// Both task families remain over 100 retries and both counts increase. Only
// their ordering changes. The initial two actual cadence executions must
// raise the same aggregate incident; a top-row frame starves that admission.
func TestTaskConvergenceAlternatingRankReachesCadenceNotifier(t *testing.T) {
	var run atomic.Int32
	var missingReads atomic.Int32
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		phase := run.Load()
		switch {
		case strings.Contains(query, "claim_time <"):
			return nil, nil
		case query == taskDueOwnershipQuery:
			// Advisory context is unavailable; the valid due-lag observation
			// remains independent and must survive the later source failure.
			return nil, nil
		case strings.Contains(query, "greatest(run_at"):
			if phase == 2 || phase == 3 {
				return []Row{{"181"}}, nil
			}
			return []Row{{"0"}}, nil
		case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
			missingReads.Add(1)
			switch phase {
			case 0:
				return []Row{{"SyntheticTaskAlpha", "202"}, {"SyntheticTaskBeta", "201"}}, nil
			case 1:
				return []Row{{"SyntheticTaskBeta", "205"}, {"SyntheticTaskAlpha", "204"}}, nil
			case 2, 3:
				return []Row{{convergenceIdentityPrivate, "999"}}, errors.New("permission denied: " + convergenceIdentityPrivate)
			case 4:
				return []Row{{"SyntheticTaskAlpha", "220"}, {"SyntheticTaskBeta", "219"}}, nil
			case 5:
				return []Row{{"SyntheticTaskBeta", "223"}, {"SyntheticTaskAlpha", "222"}}, nil
			default:
				return nil, nil
			}
		default:
			return nil, nil // Optional, fixed-output task error battery.
		}
	}}
	signal := NewTaskConvergenceSignal()
	settings := syntheticSettings(source)
	webhook, webhookURL := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookURL))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	postSignalRunsThroughTheGate(t, signal, settings, 8, func(i int) {
		// The existing helper calls this only after the prior real run-loop
		// execution and notifier Handle have completed. No delivery polling,
		// wall-clock delay or scheduler ordering supplies these assertions.
		if i >= 2 && i <= 7 {
			requireTaskPartialIncident(t, notifier, "pg/task-target-missing", "task-target-missing", "pg-1", true)
		}
		if i == 4 {
			requireTaskPartialIncident(t, notifier, "pg/task-due-lag", "task-due-lag", "pg-1", true)
			requireTaskPartialIncident(t, notifier, "monitor/visibility", "cannot-observe", "pg-1/task-target-missing", true)
		}
		run.Store(int32(i))
	}, notifier)
	if missingReads.Load() != 8 {
		t.Fatal("convergence rank control did not complete its expected observations")
	}
	requireTaskPartialIncident(t, notifier, "pg/task-target-missing", "task-target-missing", "pg-1", false)
	requireTaskPartialIncident(t, notifier, "pg/task-due-lag", "task-due-lag", "pg-1", false)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	identity := "pg/task-target-missing · task-target-missing · pg-1"
	if countSlackPosts(texts, "[WARN] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
		t.Fatal("rank change lost admission, duplicated the incident, or changed complete recovery")
	}
	for _, text := range texts {
		if strings.Contains(text, convergenceIdentityPrivate) {
			t.Fatal("failed target-source canary reached actual notifier output")
		}
	}
}

func TestTaskConvergenceStableFailureStillRaisesAfterTwoRuns(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "greatest(run_at"):
			return []Row{{"0"}}, nil
		case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
			return []Row{{"SyntheticStableTask", "101"}}, nil
		default:
			return nil, nil
		}
	}}
	webhook, webhookURL := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookURL))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	postSignalRunsThroughTheGate(t, NewTaskConvergenceSignal(), syntheticSettings(source), 2, func(int) {}, notifier)
	requireTaskPartialIncident(t, notifier, "pg/task-target-missing", "task-target-missing", "pg-1", true)
	closeSlackNotifier(t, notifier)
	if countSlackPosts(webhook.posted(), "[WARN] raised", "pg/task-target-missing · task-target-missing · pg-1") != 1 {
		t.Fatal("stable missing target changed its two-run warning")
	}
}

func TestTaskConvergenceCompleteHealthyRunsRemainQuiet(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "greatest(run_at") {
			return []Row{{"0"}}, nil
		}
		return nil, nil
	}}
	webhook, webhookURL := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookURL))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	postSignalRunsThroughTheGate(t, NewTaskConvergenceSignal(), syntheticSettings(source), 2, func(int) {}, notifier)
	closeSlackNotifier(t, notifier)
	if len(webhook.posted()) != 0 {
		t.Fatal("complete empty task sources acquired a false incident")
	}
}

func TestTaskConvergenceFailedMissingSourceKeepsDueLagAndPrivacy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case query == taskDueOwnershipQuery:
			return nil, nil
		case strings.Contains(query, "greatest(run_at"):
			return []Row{{"181"}}, nil
		case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
			return []Row{{convergenceIdentityPrivate, "999"}}, errors.New("permission denied: " + convergenceIdentityPrivate)
		default:
			return nil, nil
		}
	}}
	alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 2 {
		t.Fatal("partial target source lost independent completed due-lag evidence")
	}
	requireAlertClass(t, alerts, "task-due-lag")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if unknown.Target != "pg-1/task-target-missing" || !unknown.unobservable ||
		unknown.Observed != "error_class="+observationErrorClassAccessDenied {
		t.Fatal("partial target source acquired a product judgment or lost visibility")
	}
	requireConvergenceIdentityPrivacy(t, alerts)
}

// The limit is an examples bound. Even a completely replaced top-five set
// still represents the same observed aggregate, not per-family recovery.
func TestTaskConvergenceTopFiveExamplesKeepAggregateIdentity(t *testing.T) {
	var rows []Row
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "greatest(run_at"):
			return []Row{{"0"}}, nil
		case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
			return rows, nil
		default:
			return nil, nil
		}
	}}
	var identity string
	for phase := range 2 {
		rows = nil
		for i := range 5 {
			rows = append(rows, Row{fmt.Sprintf("SyntheticExample%d", phase*5+i), fmt.Sprint(300 + phase*100 - i)})
		}
		alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 1 {
			t.Fatal("bounded target examples lost aggregate authority")
		}
		alert := requireAlertClass(t, alerts, "task-target-missing")
		if alert.Frame != "" || alert.Sustain != 2 || !strings.Contains(alert.Observed, "tasks=5 ") {
			t.Fatal("example rank still changes aggregate identity or configured sustain")
		}
		if phase == 0 {
			identity = alert.Identity()
		} else if alert.Identity() != identity {
			t.Fatal("top-five membership change replaced the aggregate incident")
		}
		requireConvergenceIdentityPrivacy(t, alerts)
	}
}

// The owning fix must not turn advisory-context clock loss into either a
// quiet due-lag result or a lost, independently observed missing-target result.
func TestTaskConvergenceRankIdentityKeepsAdvisoryClockQualification(t *testing.T) {
	for _, offset := range []int64{-31, -30, 30, 31} {
		now := syntheticSettings(nil).Now()
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			switch {
			case query == taskDueOwnershipQuery:
				return []Row{{fmt.Sprint(now.Unix() + offset), "1", "0", "0", "181", "f"}}, nil
			case strings.Contains(query, "greatest(run_at"):
				return []Row{{"181"}}, nil
			case strings.Contains(query, "reschedule_error LIKE '%Target not found%'"):
				return []Row{{"SyntheticClockTask", "101"}}, nil
			default:
				return nil, nil
			}
		}}
		settings := syntheticSettings(source)
		settings.Now = func() time.Time { return now }
		alerts, err := NewTaskConvergenceSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatal("advisory clock qualification erased independent positive findings")
		}
		due := requireAlertClass(t, alerts, "task-due-lag")
		missing := requireAlertClass(t, alerts, "task-target-missing")
		wantUnknown := offset == -31 || offset == 31
		if strings.Contains(due.Evidence, "due_ownership=unknown") != wantUnknown || missing.Frame != "" {
			t.Fatal("rank identity fix changed advisory freshness boundary")
		}
		requireConvergenceIdentityPrivacy(t, alerts)
	}
}
