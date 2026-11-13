package monitor

// Refill controls use the public journal signal, production run loop and
// cadence gate, and real notifier Handle. All sources and HTTP replies are
// in memory; explicit tick/handler barriers order every observation.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// One immutable host answer for a manually advanced execution.
type journalRefillObservation struct {
	values map[string]string
	err    error
}

// In-memory webhook transport; it cannot resolve or dial any address.
type journalRefillWebhook struct {
	stateLock sync.Mutex
	texts     []string
}

// Consume the real webhook payload and answer a successful post in memory.
func (self *journalRefillWebhook) RoundTrip(request *http.Request) (*http.Response, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if request.Method != http.MethodPost || json.NewDecoder(request.Body).Decode(&payload) != nil {
		return nil, errors.New("synthetic invalid webhook payload")
	}
	self.stateLock.Lock()
	self.texts = append(self.texts, payload.Text)
	self.stateLock.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header), Request: request}, nil
}

// Read posted messages after Close has joined the sender.
func (self *journalRefillWebhook) posted() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.texts...)
}

// Keep the real notifier sender, formatting, queue and drain without network.
func newJournalRefillNotifier(t *testing.T) (*SlackAlertNotifier, *journalRefillWebhook) {
	t.Helper()
	harness := newSlackTestHarness()
	settings := harness.settings("https://journal-webhook.example/services/synthetic-private-webhook")
	settings.after = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	settings.MaxMessagesPerMinute = 100
	webhook := &journalRefillWebhook{}
	notifier := NewSlackAlertNotifier(settings)
	notifier.httpClient = &http.Client{Transport: webhook, Timeout: settings.PostTimeout}
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	return notifier, webhook
}

// Drive the actual public Run through one persistent gate and real Handle.
// Each call waits until the scheduler is parked, publishes an immutable host
// frame, and joins Handle before permitting the next frame.
func journalRefillLoop(t *testing.T, notifier *SlackAlertNotifier, targets ...string) func(map[string]journalRefillObservation) SignalExecution {
	t.Helper()
	var frame atomic.Pointer[map[string]journalRefillObservation]
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, journalBufferMarker) {
			return "", errors.New("synthetic unexpected command")
		}
		observations := frame.Load()
		if observations == nil {
			return "", errors.New("synthetic missing frame")
		}
		observation, ok := (*observations)[host.Name]
		if !ok {
			return "", errors.New("synthetic missing host")
		}
		return journalBufferFixture(observation.values), observation.err
	}}
	settings := syntheticSettings(source)
	settings.Hosts = nil
	for _, target := range targets {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: target, Roles: []string{"services"}})
	}
	signal := NewJournalBufferSignal()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan chan time.Time, 1)
	handled := make(chan SignalExecution, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- NewWithSignals(settings, signal).runExecutionLoop(ctx,
			RunLoopOptions{MinimumProbeCadence: signal.Cadence()},
			func(_ context.Context, current Signal, execution SignalExecution) error {
				notifier.Handle(current, execution)
				handled <- execution
				return nil
			},
			func(cadence time.Duration) runLoopTicker {
				tick := make(chan time.Time, 1)
				ticks <- tick
				return &manualRunLoopTicker{c: tick}
			})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error("journal execution loop failed to join")
			}
		case <-time.After(30 * time.Second):
			t.Error("journal execution loop did not stop")
		}
	})
	return func(observations map[string]journalRefillObservation) SignalExecution {
		t.Helper()
		select {
		case tick := <-ticks:
			frame.Store(&observations)
			tick <- time.Time{}
		case <-time.After(30 * time.Second):
			t.Fatal("journal scheduler did not reach its cadence barrier")
		}
		select {
		case execution := <-handled:
			return execution
		case <-time.After(30 * time.Second):
			t.Fatal("journal execution did not reach its handler barrier")
			return SignalExecution{}
		}
	}
}

// Read the real incident only after synchronous Handle has returned.
func journalRefillIncident(notifier *SlackAlertNotifier, class, target string) (bool, int) {
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	incident := notifier.keyIncidents["host/journal-buffer|"+class+"|"+target]
	if incident == nil {
		return false, 0
	}
	return true, incident.signalKeyAbsentRuns["journal-buffer"]
}

// Establish a mature warning, apply two unchecked frames, and require two
// measured 50-minute witnesses to clear. A restart supplies no recovery.
func checkJournalRefillRecovery(t *testing.T, grace map[string]string) {
	t.Helper()
	notifier, webhook := newJournalRefillNotifier(t)
	advance := journalRefillLoop(t, notifier, "journal-edge.example")
	frame := func(values map[string]string) map[string]journalRefillObservation {
		return map[string]journalRefillObservation{"journal-edge.example": {values: values}}
	}
	short := map[string]string{"coverage_present": "0", "boundary_entry_age_seconds": "0"}
	first := advance(frame(short))
	if first.Failed || len(first.Alerts) != 0 || len(first.HeldAlerts) != 1 || first.HeldAlerts[0].Class != "journal-buffer-short" {
		t.Fatal("mature short buffer did not start below Sustain 2")
	}
	second := advance(frame(short))
	if alert := requireAlertClass(t, second.Alerts, "journal-buffer-short"); alert.Sustain != 2 || alert.Severity != SeverityWarn {
		t.Fatal("mature short buffer changed its warning threshold")
	}
	// A checked healthy sample starts an absence streak before the restart.
	advance(frame(nil))
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "journal-edge.example"); !present || absent != 1 {
		t.Fatal("measured first recovery did not receive one absence credit")
	}
	for range 2 {
		execution := advance(frame(grace))
		if execution.Failed || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 {
			t.Fatal("legitimate refill grace emitted a warning or source failure")
		}
	}
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "journal-edge.example"); !present || absent != 0 {
		t.Fatal("unchecked refill falsely cleared or credited the previously mature short-buffer incident")
	}
	// Exact cutoff is a valid witness at the unchanged 70-minute boundary.
	witness := map[string]string{"uptime_seconds": "4200", "journald_active_seconds": "4200", "boundary_entry_age_seconds": "3000"}
	advance(frame(witness))
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "journal-edge.example"); !present || absent != 1 {
		t.Fatal("first measured post-grace witness did not retain the incident with one credit")
	}
	advance(frame(witness))
	if present, _ := journalRefillIncident(notifier, "journal-buffer-short", "journal-edge.example"); present {
		t.Fatal("two measured post-grace witnesses did not clear the incident")
	}
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if len(texts) != 2 || !strings.Contains(texts[0], "[WARN] raised") || !strings.Contains(texts[1], "[CLEARED] WARN") {
		t.Fatalf("journal transitions changed: %v", texts)
	}
	for _, text := range texts {
		if strings.Contains(text, "synthetic-private-webhook") || strings.Contains(text, "refill") || strings.Contains(text, "cannot-observe") {
			t.Fatal("refill hold or private webhook became an emitted message")
		}
	}
}

// A service generation change does not prove measured journal recovery.
func TestJournalBufferServiceRestartRefillCannotClearMatureIncident(t *testing.T) {
	checkJournalRefillRecovery(t, map[string]string{
		"journald_active_seconds": "600", "coverage_checked": "0", "coverage_present": "0", "boundary_entry_age_seconds": "600",
	})
}

// A new host boot also supplies no current-boot 50-minute witness.
func TestJournalBufferHostRebootRefillCannotClearMatureIncident(t *testing.T) {
	checkJournalRefillRecovery(t, map[string]string{
		"uptime_seconds": "600", "journald_active_seconds": "600", "coverage_checked": "0", "coverage_present": "0", "boundary_entry_age_seconds": "600",
	})
}

// No prior short incident means quiet initial grace creates no incident or
// webhook; measured maturity still requires the original two failing ticks.
func TestJournalBufferInitialRefillRemainsQuiet(t *testing.T) {
	notifier, webhook := newJournalRefillNotifier(t)
	advance := journalRefillLoop(t, notifier, "initial-journal.example")
	for _, age := range []string{"600", "4199"} {
		execution := advance(map[string]journalRefillObservation{"initial-journal.example": {values: map[string]string{
			"uptime_seconds": age, "journald_active_seconds": age, "coverage_checked": "0", "coverage_present": "0", "boundary_entry_age_seconds": "0",
		}}})
		if execution.Failed || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 {
			t.Fatal("initial refill grace did not remain quiet")
		}
		if present, _ := journalRefillIncident(notifier, "journal-buffer-short", "initial-journal.example"); present {
			t.Fatal("initial refill grace created a short-buffer incident")
		}
	}
	closeSlackNotifier(t, notifier)
	if texts := webhook.posted(); len(texts) != 0 {
		t.Fatalf("initial grace posted %d unexpected messages", len(texts))
	}
}

// Refill holds only the short class on its own host. Independent detections
// still raise, and complete healthy siblings retain their ordinary clears.
func TestJournalBufferRefillPreservesIndependentClassesAndHostRecovery(t *testing.T) {
	notifier, webhook := newJournalRefillNotifier(t)
	advance := journalRefillLoop(t, notifier, "refilling-journal.example", "recovered-journal.example")
	short := map[string]string{"coverage_present": "0", "boundary_entry_age_seconds": "0"}
	broken := map[string]string{
		"coverage_present": "0", "boundary_entry_age_seconds": "0", "journald_active": "failed",
		"max_file_sec": "1month", "journal_files": "257", "journal_vacuum_runtime_state": "failed",
		"journal_vacuum_last_success_age_seconds": "0", "journal_retention_rotations_5m": "1",
	}
	frame := map[string]journalRefillObservation{
		"refilling-journal.example": {values: broken}, "recovered-journal.example": {values: short},
	}
	advance(frame)
	advance(frame)
	classes := []string{"journal-buffer-config", "journal-buffer-file-headroom", "journal-buffer-vacuum", "journal-buffer-retention-rotation", "journal-buffer-unavailable"}
	for _, class := range append(append([]string(nil), classes...), "journal-buffer-short") {
		if present, absent := journalRefillIncident(notifier, class, "refilling-journal.example"); !present || absent != 0 {
			t.Fatalf("independent %s incident did not establish", class)
		}
	}
	// Preserve every independent positive during the unchecked short refill.
	refillBroken := make(map[string]string, len(broken)+2)
	for key, value := range broken {
		refillBroken[key] = value
	}
	refillBroken["journald_active_seconds"] = "600"
	refillBroken["coverage_checked"] = "0"
	frame = map[string]journalRefillObservation{
		"refilling-journal.example": {values: refillBroken}, "recovered-journal.example": {},
	}
	for range 2 {
		execution := advance(frame)
		if execution.Failed || len(execution.Alerts) != len(classes) || len(execution.HeldAlerts) != 0 {
			t.Fatal("unchecked short coverage suppressed an independent journal finding")
		}
		for _, class := range classes {
			requireAlertClass(t, execution.Alerts, class)
		}
	}
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "recovered-journal.example"); !present || absent != 0 {
		t.Fatal("inactive journald visibility lost its existing signal-wide recovery authority")
	}
	// Once journald is active, every remaining positive is independently
	// observable. The short-only hold must no longer affect the other host.
	observableRefillBroken := make(map[string]string, len(refillBroken))
	for key, value := range refillBroken {
		observableRefillBroken[key] = value
	}
	observableRefillBroken["journald_active"] = "active"
	frame = map[string]journalRefillObservation{
		"refilling-journal.example": {values: observableRefillBroken}, "recovered-journal.example": {},
	}
	classes = classes[:len(classes)-1]
	for range 2 {
		execution := advance(frame)
		if execution.Failed || len(execution.Alerts) != len(classes) || len(execution.HeldAlerts) != 0 {
			t.Fatal("active journald lost an independently observable journal positive")
		}
		for _, class := range classes {
			if alert := requireAlertClass(t, execution.Alerts, class); isVisibilityAlert(alert) {
				t.Fatal("independent complete-positive fixture unexpectedly retained visibility loss")
			}
		}
	}
	if present, _ := journalRefillIncident(notifier, "journal-buffer-short", "recovered-journal.example"); present {
		t.Fatal("one host's silent refill hold withheld another host's measured recovery")
	}
	if present, _ := journalRefillIncident(notifier, "journal-buffer-unavailable", "refilling-journal.example"); present {
		t.Fatal("active journald did not recover its separate visibility incident")
	}
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "refilling-journal.example"); !present || absent != 0 {
		t.Fatal("independent positives did not retain the refilling host's short incident")
	}
	// Fix the independent classes while short coverage remains unchecked.
	frame = map[string]journalRefillObservation{
		"refilling-journal.example": {values: map[string]string{
			"journald_active_seconds": "600", "coverage_checked": "0", "coverage_present": "0", "boundary_entry_age_seconds": "0",
		}}, "recovered-journal.example": {},
	}
	advance(frame)
	advance(frame)
	for _, class := range classes {
		if present, _ := journalRefillIncident(notifier, class, "refilling-journal.example"); present {
			t.Fatalf("short refill withheld independently measured %s recovery", class)
		}
	}
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "refilling-journal.example"); !present || absent != 0 {
		t.Fatal("independent recovery falsely cleared the unchecked short class")
	}
	closeSlackNotifier(t, notifier)
	if texts := webhook.posted(); len(texts) != 13 {
		t.Fatalf("independent raise/clear transitions=%d, want 13", len(texts))
	}
}

// A source failure retains visibility and breaks earlier recovery credit;
// private error text never becomes a clear-hold identity or webhook payload.
func TestJournalBufferRefillSourceFailureRetainsVisibilityAndRecoveryAuthority(t *testing.T) {
	notifier, webhook := newJournalRefillNotifier(t)
	advance := journalRefillLoop(t, notifier, "source-journal.example")
	short := map[string]journalRefillObservation{"source-journal.example": {values: map[string]string{"coverage_present": "0", "boundary_entry_age_seconds": "0"}}}
	advance(short)
	advance(short)
	healthy := map[string]journalRefillObservation{"source-journal.example": {}}
	advance(healthy)
	private := "synthetic-private-journal-source-detail"
	failed := map[string]journalRefillObservation{"source-journal.example": {err: errors.New(private)}}
	first := advance(failed)
	if len(first.Alerts) != 0 || len(first.HeldAlerts) != 1 || !isVisibilityAlert(first.HeldAlerts[0]) || first.HeldAlerts[0].Class != "cannot-observe" || len(first.ClearHolds) != 0 {
		t.Fatal("source failure lost its held visibility authority")
	}
	second := advance(failed)
	if len(second.Alerts) != 1 || second.Alerts[0].Class != "cannot-observe" || !isVisibilityAlert(second.Alerts[0]) || len(second.ClearHolds) != 0 {
		t.Fatal("source failure became silent refill instead of visibility loss")
	}
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "source-journal.example"); !present || absent != 0 {
		t.Fatal("source failure cleared the mature incident or retained earlier recovery credit")
	}
	advance(healthy)
	if present, absent := journalRefillIncident(notifier, "journal-buffer-short", "source-journal.example"); !present || absent != 1 {
		t.Fatal("one post-failure witness reused pre-failure recovery credit")
	}
	advance(healthy)
	if present, _ := journalRefillIncident(notifier, "journal-buffer-short", "source-journal.example"); present {
		t.Fatal("two post-failure measured witnesses did not clear")
	}
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if len(texts) != 4 {
		t.Fatalf("source failure raise/clear transitions=%d, want 4", len(texts))
	}
	for _, text := range texts {
		if strings.Contains(text, private) || strings.Contains(text, "synthetic-private-webhook") {
			t.Fatal("private source or webhook data leaked")
		}
	}
}
