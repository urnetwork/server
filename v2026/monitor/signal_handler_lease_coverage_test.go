package monitor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Baseline-compatible rows respond to the actual query schema. A causal RED
// reaches the unsafe healthy verdict, rather than an appended-column failure.
func handlerLeaseTestRows(query string, selection bool, stale, mature, eligible int64) []Row {
	if selection {
		if strings.Contains(query, "pg_attribute") {
			return []Row{{"t"}}
		}
		row := Row{"75000", strconv.FormatInt(eligible, 10), strconv.FormatInt(eligible, 10), "0", "0", "0", "0", "0", "0", "12345678-1234-4234-8234-123456789abc"}
		if strings.Contains(query, "stale_open_provider_rows") {
			row = append(row, "75000", strconv.FormatInt(stale, 10))
		}
		return []Row{row}
	}
	staleHandlers := "0"
	if stale > 0 {
		staleHandlers = "1"
	}
	if stale == 75000 {
		staleHandlers = "22"
	}
	row := Row{"75000", strconv.FormatInt(mature, 10), strconv.FormatInt(mature, 10), "180", "22", staleHandlers}
	if strings.Contains(query, "stale_open_connection_count") {
		row = append(row, strconv.FormatInt(stale, 10))
	}
	return []Row{row}
}

func handlerLeaseTestCache(t testing.TB, populated bool) func(HostSettings, int, ...string) (string, error) {
	t.Helper()
	var document bytes.Buffer
	values := []int{}
	if populated {
		values = []int{1}
	}
	if err := gob.NewEncoder(&document).Encode(values); err != nil {
		t.Fatal(err)
	}
	return func(_ HostSettings, _ int, args ...string) (string, error) {
		key := args[len(args)-1]
		if key == "client_score_provider_eligibility_v1_ready" {
			return "1", nil
		}
		if key == "client_score_ip_family_v1_ready" || !strings.HasSuffix(key, "}c_l") {
			return "", nil
		}
		return document.String(), nil
	}
}

func handlerLeaseTestHasVisibility(alerts Alerts) bool {
	for _, alert := range alerts {
		if alert.Class == "cannot-observe" && alert.Frame == "handler-lease-coverage" {
			return true
		}
	}
	return false
}

func TestHandlerLeaseCoverageOrphansPublicCausal(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return handlerLeaseTestRows(query, false, 75000, 0, 0), nil }}
	alerts, err := NewConnectionOrphansSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal("lease control failed before the semantic assertion")
	}
	if !handlerLeaseTestHasVisibility(alerts) {
		t.Fatal("stale existing handlers granted healthy connection ownership")
	}
	if requireAlertClassCount(alerts, "connection-orphans") != 0 {
		t.Fatal("stale lease was misdiagnosed as a missing or dead handler")
	}
}

func TestHandlerLeaseCoverageSelectionPublicCausal(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return handlerLeaseTestRows(query, true, 75000, 0, 0), nil }, redisFn: handlerLeaseTestCache(t, false)}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal("selection lease control failed before the semantic assertion")
	}
	if !handlerLeaseTestHasVisibility(alerts) {
		t.Fatal("zero heartbeat-qualified eligibility granted healthy selection recovery")
	}
	if requireAlertClassCount(alerts, "selection-empty") != 0 {
		t.Fatal("lease coverage changed the existing eligible-population PAGE threshold")
	}
}

// Complete positive incident evidence survives independent stale-lease ambiguity.
func TestHandlerLeaseCoveragePreservesIndependentPages(t *testing.T) {
	for _, selection := range []bool{false, true} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return handlerLeaseTestRows(query, selection, 100, 1, 1001), nil }, redisFn: handlerLeaseTestCache(t, false)}
		var signal Signal = NewConnectionOrphansSignal()
		class := "connection-orphans"
		if selection {
			signal = NewSelectionPopulationSignal()
			class = "selection-empty"
		}
		alerts, err := signal.Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if !handlerLeaseTestHasVisibility(alerts) || requireAlertClassCount(alerts, class) != 1 {
			t.Fatal("lease visibility erased independent complete failure")
		}
		for _, alert := range alerts {
			if alert.Class == "cannot-observe" && alert.Severity != SeverityWarn {
				t.Fatal("lease ambiguity became a product PAGE")
			}
			requireAlertOmits(t, alert, "private-handler-lease", "handler-token")
		}
	}
}

// The actual notifier keeps a mature incident through unknown lease coverage;
// only a later complete fresh measurement grants recovery credit.
func TestHandlerLeaseCoverageNotifierRetainsIncident(t *testing.T) {
	for _, selection := range []bool{false, true} {
		var run atomic.Int32
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			i := run.Load()
			stale, mature, eligible := int64(0), int64(1), int64(1001)
			if i == 2 || i == 3 {
				stale, mature, eligible = 75000, 0, 0
			}
			if i >= 4 {
				mature, eligible = 0, 0
			}
			return handlerLeaseTestRows(query, selection, stale, mature, eligible), nil
		}, redisFn: handlerLeaseTestCache(t, false)}
		var signal Signal = NewConnectionOrphansSignal()
		class := "connection-orphans"
		cadence := time.Minute
		if selection {
			signal = NewSelectionPopulationSignal()
			class = "selection-empty"
			cadence = 5 * time.Minute
		}
		settings := syntheticSettings(source)
		settings.Now = func() time.Time {
			return time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC).Add(time.Duration(run.Load()) * cadence)
		}
		webhook, url := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(url))
		t.Cleanup(func() { closeSlackNotifier(t, notifier) })
		if notifier.settings.ClearAfterCompleteRuns != 2 {
			t.Fatal("handler lease fixture changed the production recovery window")
		}
		assertIncident := func(incidentClass string, wantPresent bool, wantAbsenceCredit int) {
			t.Helper()
			found, absenceCredit, owned := false, 0, false
			func() {
				notifier.stateLock.Lock()
				defer notifier.stateLock.Unlock()
				incident, present := notifier.keyIncidents[signal.ID()+"|"+incidentClass+"|pg-1"]
				found = present
				if present {
					absenceCredit, owned = incident.signalKeyAbsentRuns[signal.Key()]
				}
			}()
			if found != wantPresent || found && (!owned || absenceCredit != wantAbsenceCredit) {
				t.Fatalf("%s/%s: incident present=%t owned=%t recovery_credit=%d; want present=%t credit=%d", signal.Key(), incidentClass, found, owned, absenceCredit, wantPresent, wantAbsenceCredit)
			}
		}
		postSignalRunsThroughTheGate(t, signal, settings, 6, func(i int) {
			// Handle completes before the next tick: unknown observations grant
			// zero credit, and one complete healthy observation cannot clear.
			if i >= 2 && i <= 4 {
				assertIncident(class, true, 0)
			}
			if i == 4 {
				assertIncident("cannot-observe", true, 0)
			}
			if i == 5 {
				assertIncident(class, true, 1)
				assertIncident("cannot-observe", true, 1)
			}
			run.Store(int32(i))
			if i > 0 {
				harness.advance(cadence)
			}
		}, notifier)
		closeSlackNotifier(t, notifier)
		assertIncident(class, false, 0)
		assertIncident("cannot-observe", false, 0)
		posts := webhook.posted()
		// Both clears share a signal ID containing the product class. A class
		// substring counts the visibility clear too; match the complete line.
		identity := "\n" + signal.ID() + " · " + class + " · pg-1\n"
		visibilityIdentity := "\n" + signal.ID() + " · cannot-observe · pg-1\n"
		raised := countSlackPosts(posts, "[PAGE] raised", identity)
		cleared := countSlackPosts(posts, "[CLEARED] PAGE", identity)
		visibilityRaised := countSlackPosts(posts, "[WARN] raised", visibilityIdentity)
		visibilityCleared := countSlackPosts(posts, "[CLEARED] WARN", visibilityIdentity)
		if raised != 1 || cleared != 1 || visibilityRaised != 1 || visibilityCleared != 1 {
			t.Fatalf("%s: product raised=%d cleared=%d; visibility raised=%d cleared=%d; ambiguous class-only clears=%d", signal.Key(), raised, cleared, visibilityRaised, visibilityCleared, countSlackPosts(posts, "[CLEARED]", class))
		}
	}
}

// Both SQL boundaries preserve already observed source causes when their parent
// ends, and neither can initiate a dependent Redis read after that boundary.
func TestHandlerLeaseCoverageCancellationPreservesCause(t *testing.T) {
	for _, boundary := range []struct{ selection, schemaFailure bool }{
		{selection: false}, {selection: true, schemaFailure: true}, {selection: true},
	} {
		for _, alreadyEnded := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if alreadyEnded {
				cancel()
			}
			cause := errors.New("private-handler-lease-source")
			reads, redisReads := 0, 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				reads++
				if boundary.selection && !boundary.schemaFailure && strings.Contains(query, "pg_attribute") {
					return []Row{{"t"}}, nil
				}
				cancel()
				return nil, cause
			}, redisFn: func(HostSettings, int, ...string) (string, error) { redisReads++; return "", nil }}
			var signal Signal = NewConnectionOrphansSignal()
			if boundary.selection {
				signal = NewSelectionPopulationSignal()
			}
			alerts, err := signal.Run(ctx, syntheticSettings(source))
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 || redisReads != 0 {
				t.Fatal("ended SQL owner returned findings or consumed dependent evidence")
			}
			wantReads := 1
			if boundary.selection && !boundary.schemaFailure {
				wantReads = 2
			}
			if alreadyEnded {
				wantReads = 0
			}
			if reads != wantReads {
				t.Fatal("ended SQL owner initiated a dependent source read")
			}
			if !alreadyEnded && !errors.Is(err, cause) {
				t.Fatal("ended SQL owner erased its original source cause")
			}
			if strings.Contains(err.Error(), "private-handler-lease") {
				t.Fatal("private SQL cause escaped rendered error")
			}
		}
	}
}

// Deterministic owner-end seam: the source has returned, its boundary is still
// live, then reduction reaches an ended parent. No real deadline or sleep.
type handlerLeaseFinishingContext struct {
	context.Context
	armed  bool
	checks int
	done   chan struct{}
}

func (self *handlerLeaseFinishingContext) Done() <-chan struct{} { return self.done }
func (self *handlerLeaseFinishingContext) Err() error {
	if !self.armed {
		return nil
	}
	self.checks++
	if self.checks < 2 {
		return nil
	}
	if self.checks == 2 {
		close(self.done)
	}
	return context.DeadlineExceeded
}

func TestHandlerLeaseCoverageFinalOwnerBoundary(t *testing.T) {
	for _, stale := range []int64{0, 75000} {
		ctx := &handlerLeaseFinishingContext{Context: context.Background(), done: make(chan struct{})}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			ctx.armed = true
			return handlerLeaseTestRows(query, false, stale, 0, 0), nil
		}}
		alerts, err := NewConnectionOrphansSignal().Run(ctx, syntheticSettings(source))
		if !errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 {
			t.Fatal("finished owner returned healthy or lease findings after reduction")
		}
	}
}

// Cache visibility cannot erase independently measured SQL lease ambiguity.
func TestHandlerLeaseCoverageSelectionPartialCache(t *testing.T) {
	cache := handlerLeaseTestCache(t, false)
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return handlerLeaseTestRows(query, true, 75000, 0, 0), nil }, redisFn: func(host HostSettings, port int, args ...string) (string, error) {
		if strings.HasSuffix(args[len(args)-1], "}c_l") {
			return "", errors.New("private-handler-lease-cache")
		}
		return cache(host, port, args...)
	}}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || !handlerLeaseTestHasVisibility(alerts) || requireAlertClassCount(alerts, "cannot-observe") != 2 {
		t.Fatal("partial cache erased independent handler lease coverage")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private-handler-lease-cache")
	}
}

// The public exclusion policy still owns source admission before SQL or Redis.
func TestHandlerLeaseCoverageExcludedSource(t *testing.T) {
	for _, selection := range []bool{false, true} {
		reads := 0
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { reads++; return nil, nil }, redisFn: func(HostSettings, int, ...string) (string, error) { reads++; return "", nil }}
		settings, err := ExcludeHosts(syntheticSettings(source), "pg-1")
		if err != nil {
			t.Fatal(err)
		}
		var signal Signal = NewConnectionOrphansSignal()
		if selection {
			signal = NewSelectionPopulationSignal()
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || reads != 0 || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
			t.Fatal("excluded source was contacted or supplied handler lease health")
		}
	}
}
