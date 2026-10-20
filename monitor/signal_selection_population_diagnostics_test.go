package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Exercise each real read boundary through the ordinary signal adapter. A
// returned deadline alone must never be promoted to child-owner expiration.
func TestSelectionPopulationRedisDiagnosticReadBoundaries(t *testing.T) {
	for _, test := range []struct {
		phase     string
		failRead  int
		wantReads int
		legacy    bool
	}{
		{phase: "eligibility", failRead: 1, wantReads: 9},
		{phase: "family-before", failRead: 2, wantReads: 2},
		{phase: "facet", failRead: 3, wantReads: 3},
		{phase: "family-after", failRead: 9, wantReads: 9},
		{phase: "legacy", failRead: 9, wantReads: 9, legacy: true},
	} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		marker := "1"
		if test.legacy {
			marker = ""
			documents = map[string]string{
				selectionPopulationFamilyTestKey(false, ""): selectionPopulationFamilyTestGob(t, []int{31}),
				selectionPopulationFamilyTestKey(true, ""):  selectionPopulationFamilyTestGob(t, []int{57}),
			}
		}
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{marker: marker, documents: documents, eligibilityMarker: "1"})
		baseSource := settings.Source
		reads := 0
		settings.Source = &selectionPopulationFamilyContextSource{
			SignalSource: baseSource,
			redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
				reads++
				if ctx.Err() != nil {
					t.Fatal("source deadline fixture unexpectedly expired its actual owner")
				}
				if reads == test.failRead {
					return "", errors.Join(errors.New("synthetic-private-diagnostic.example"), context.DeadlineExceeded)
				}
				return baseSource.Redis(ctx, host, port, args...)
			},
		}
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil || reads != test.wantReads || len(alerts) != 1 {
			t.Fatalf("%s: reads=%d alerts=%d err=%v", test.phase, reads, len(alerts), err)
		}
		alert := alerts[0]
		for _, expected := range []string{"redis_read_phase=" + test.phase, "redis_child_state=live", "redis_returned_context=deadline", "redis_error_class=" + observationErrorClassTimeout} {
			if !strings.Contains(alert.Observed, expected) {
				t.Fatalf("%s missing %s: %s", test.phase, expected, alert.Observed)
			}
		}
		if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn {
			t.Fatal("diagnostic changed unknown-source severity/class")
		}
		requireAlertOmits(t, alert, "synthetic-private-diagnostic.example")
	}
}

// A real already-expired qualification child is deterministic and leaves the
// live parent alone. Its expiry cannot become a fabricated source result.
func TestSelectionPopulationRedisDiagnosticActualChildDeadline(t *testing.T) {
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{})
	reads := 0
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: settings.Source,
		redisContextFn: func(ctx context.Context, _ HostSettings, _ int, _ ...string) (string, error) {
			reads++
			if ctx.Err() != context.DeadlineExceeded {
				t.Fatal("actual expired child not passed to eligibility read")
			}
			return "", ctx.Err()
		},
	}
	probe := pgSelectionPopulationProbe{redisQualificationContext: func(parent context.Context) (context.Context, context.CancelFunc) {
		return context.WithDeadline(parent, time.Time{})
	}}
	signal := &signalAdapter{number: "2.9", key: "selection-population", name: "Provider-selection population", probe: probe}
	parent := context.Background()
	alerts, err := signal.Run(parent, settings)
	if err != nil || parent.Err() != nil || reads != 1 || len(alerts) != 2 {
		t.Fatalf("child expiry changed lifecycle or command count: reads=%d alerts=%d err=%v", reads, len(alerts), err)
	}
	for _, alert := range alerts {
		if !strings.Contains(alert.Observed, "redis_child_state=deadline") || alert.Class != "cannot-observe" {
			t.Fatal("actual child deadline lost its fixed unknown diagnostic")
		}
	}
}

// Preserve the child's state at the failed eligibility read, even when a
// later family read ends that child. Neither error can cancel the parent.
func TestSelectionPopulationRedisDiagnosticSnapshotsEachRead(t *testing.T) {
	documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{marker: "1", documents: documents})
	baseSource := settings.Source
	var cancelChild context.CancelFunc
	reads := 0
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: baseSource,
		redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
			reads++
			if reads == 1 {
				return "", errors.New("synthetic-private-snapshot.example")
			}
			if reads == 9 {
				cancelChild()
				return "", ctx.Err()
			}
			return baseSource.Redis(ctx, host, port, args...)
		},
	}
	probe := pgSelectionPopulationProbe{redisQualificationContext: func(parent context.Context) (context.Context, context.CancelFunc) {
		child, cancel := context.WithCancel(parent)
		cancelChild = cancel
		return child, cancel
	}}
	signal := &signalAdapter{number: "2.9", key: "selection-population", name: "Provider-selection population", probe: probe}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || reads != 9 || len(alerts) != 2 {
		t.Fatalf("snapshot path changed: reads=%d alerts=%d err=%v", reads, len(alerts), err)
	}
	for _, alert := range alerts {
		state := "redis_child_state=canceled"
		if alert.Frame == "eligibility-observation" {
			state = "redis_child_state=live"
		}
		if !strings.Contains(alert.Observed, state) {
			t.Fatal("a later read rewrote an earlier diagnostic")
		}
		requireAlertOmits(t, alert, "synthetic-private-snapshot.example")
	}
}

// Parent cancellation still returns lifecycle error without any visibility
// findings; the new diagnostic path must not invent a partial result.
func TestSelectionPopulationRedisDiagnosticParentCancellation(t *testing.T) {
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: settings.Source,
		redisContextFn: func(ctx context.Context, _ HostSettings, _ int, _ ...string) (string, error) {
			reads++
			cancel()
			return "", ctx.Err()
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(parent, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || reads != 1 {
		t.Fatalf("parent cancellation changed: reads=%d alerts=%d err=%v", reads, len(alerts), err)
	}
}

// Complete and partial data controls retain the original schema behavior;
// adding diagnostics cannot turn an incomplete observation into clearance.
func TestSelectionPopulationRedisDiagnosticHealthyAndPartialControls(t *testing.T) {
	for _, partial := range []bool{false, true} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		if partial {
			delete(documents, selectionPopulationFamilyTestKey(false, "6"))
		}
		settings, capture := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{marker: "1", documents: documents, eligibilityMarker: "1"})
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil || len(capture.readKeys) != 9 {
			t.Fatalf("data control changed reads: partial=%t err=%v", partial, err)
		}
		if !partial {
			if len(alerts) != 0 {
				t.Fatal("healthy source gained a warning")
			}
			continue
		}
		if len(alerts) != 1 || !alerts[0].unobservable ||
			!strings.Contains(alerts[0].Observed, "cache_state=facets-incomplete") ||
			strings.Contains(alerts[0].Observed, "redis_read_phase=") {
			t.Fatal("partial data invented transport attribution or lost no-clear state")
		}
	}
}

// Joined local admission markers survive lifecycle-class precedence. Matching
// text without the typed wrapper cannot invent acquire/release evidence.
func TestSelectionPopulationRedisDiagnosticJoinedAdmissionPhases(t *testing.T) {
	for _, typed := range []bool{false, true} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{marker: "1", documents: documents, eligibilityMarker: "1"})
		baseSource := settings.Source
		reads := 0
		settings.Source = &selectionPopulationFamilyContextSource{
			SignalSource: baseSource,
			redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
				reads++
				if reads == 2 {
					private := errors.New("synthetic-private-admission.example acquire release")
					if typed {
						return "", errors.Join(context.DeadlineExceeded,
							&sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire},
							&sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease})
					}
					return "", errors.Join(context.DeadlineExceeded, private)
				}
				return baseSource.Redis(ctx, host, port, args...)
			},
		}
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil || reads != 2 || len(alerts) != 1 {
			t.Fatalf("typed=%t reads=%d alerts=%d err=%v", typed, reads, len(alerts), err)
		}
		alert := alerts[0]
		if !strings.Contains(alert.Observed, "redis_child_state=live") ||
			!strings.Contains(alert.Observed, "redis_returned_context=deadline") ||
			!strings.Contains(alert.Observed, "redis_error_class="+observationErrorClassTimeout) {
			t.Fatal("joined admission error rewrote child or lifecycle classification")
		}
		if typed != strings.Contains(alert.Observed, "redis_ssh_admission_failures=acquire,release") {
			t.Fatal("typed admission phases were erased or invented from text")
		}
		if !typed && strings.Contains(alert.Observed, "redis_ssh_admission_failures=") {
			t.Fatal("plain error text invented admission authority")
		}
		requireAlertOmits(t, alert, "synthetic-private-admission.example")
	}
}
