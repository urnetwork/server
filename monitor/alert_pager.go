package monitor

// Pager notifier for the continuous watcher (§19.8, D-OPS-9, R-PHASE-12, owner
// decision 143). Slack carries every alert; this notifier beside it wakes the
// rota for §19.8's page classes only, at any hour.
//
// Handle takes the executions Slack's Handle takes and applies the same
// incident rule, observeIncidents, on its own state, since the pager must work
// without Slack and must not stop for a watcher that keeps Slack quiet. So an
// execution that failed or carries a visibility alert, gated or held, clears
// nothing, and a signal that cannot see does not end a page.
//
// An incident of a class with a pageClasses row triggers a page when it reaches
// page severity, whether raised there or promoted by PageSustain, and resolves
// the page when it clears. A warn alert pages nothing, whatever its class, and
// a SeverityPage alert of a class with no row stays on Slack.
//
// A page carries only its class, the time, the environment, a key and the
// row's fixed text, because it leaves the site (I-13). The key is a hash of the
// environment and the incident identity. An alert's target, symptom and
// evidence are never read into a page: the person paged finds them on Slack and
// in the watcher's output, under the page's class.
//
// Handle never waits on the network. A pager.RetrySink holds the events and
// retries each until the sink takes it, because a dropped page is the worst
// failure here. While the sink will not take pages, a warn goes to Slack through
// Warn, and clears when it takes them again. The notifier does not own the
// rota's acknowledgment or the second person: plan-OPS-pager-service leaves
// those to the pager service, and pager.FakeSink models them for FS-50.
//
// State is process-local, so a restarted watcher re-raises each open incident
// under the same key and the sink's dedup absorbs it, leaving the page and its
// acknowledgment clock as they were. A page whose incident ended while no
// watcher ran stays open for a person to close. Two overlapping watchers both
// page under one key, and either one's clear resolves the page.
//
// Handle and Close are safe for concurrent use.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/pager"
)

// Settings for NewPagerAlertNotifier. Zero values take the defaults.
type PagerAlertNotifierSettings struct {
	// nil leaves the watcher unable to page, which DisabledReason explains
	Sink pager.Sink
	// why Sink is nil, for the log and the warn; holds no credential
	DisabledReason string
	Environment    string
	// uninterrupted complete executions without an incident before it clears;
	// as SlackAlertNotifierSettings
	ClearAfterCompleteRuns int
	// the sender's limits, zero values taking pager's defaults; DeliveryFailing
	// is the notifier's own, and an unset Logf takes this Logf
	Delivery pager.RetrySinkSettings
	// takes the pager's own warn, as a signal execution; Slack's Handle in the
	// CLI. It must not wait on the network. nil leaves the warn in the log.
	Warn func(signal Signal, execution SignalExecution)
	// the complete runs without the warn that Warn's carrier takes to clear it:
	// Slack's ClearAfterCompleteRuns in the CLI. Non-positive takes
	// ClearAfterCompleteRuns, which has Slack's default.
	WarnClearRuns int
	// one line per call, without a trailing newline
	Logf func(format string, args ...any)
	Now  func() time.Time
}

// The production limits. Logf writes to stderr, the monitor's diagnostic
// stream.
func DefaultPagerAlertNotifierSettings() PagerAlertNotifierSettings {
	return PagerAlertNotifierSettings{
		ClearAfterCompleteRuns: 2,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
		Now: time.Now,
	}
}

// Fills unset limits and seams from DefaultPagerAlertNotifierSettings.
func (self PagerAlertNotifierSettings) withDefaults() PagerAlertNotifierSettings {
	defaults := DefaultPagerAlertNotifierSettings()
	if self.ClearAfterCompleteRuns <= 0 {
		self.ClearAfterCompleteRuns = defaults.ClearAfterCompleteRuns
	}
	if self.WarnClearRuns <= 0 {
		self.WarnClearRuns = self.ClearAfterCompleteRuns
	}
	if self.Logf == nil {
		self.Logf = defaults.Logf
	}
	if self.Now == nil {
		self.Now = defaults.Now
	}
	if self.Delivery.Logf == nil {
		self.Delivery.Logf = self.Logf
	}
	return self
}

// The name the pager's own warn reports under. It is no registered signal: it
// never runs, and its executions come from the notifier.
type pagerSinkSignal struct{}

func (pagerSinkSignal) Number() string         { return "" }
func (pagerSinkSignal) Key() string            { return "pager-sink" }
func (pagerSinkSignal) ID() string             { return "monitor/pager-sink" }
func (pagerSinkSignal) Name() string           { return "Pager sink" }
func (pagerSinkSignal) Cadence() time.Duration { return 0 }
func (pagerSinkSignal) Run(context.Context, SignalSettings) (Alerts, error) {
	return nil, nil
}

// Pages the rota for §19.8's page classes (see the file comment).
type PagerAlertNotifier struct {
	settings PagerAlertNotifierSettings
	// nil when the watcher cannot page
	sender *pager.RetrySink

	stateLock    sync.Mutex
	closed       bool
	keyIncidents map[string]*slackIncident
}

// Logs one line saying whether pages can reach the rota. Without a sink it
// returns a notifier that pages nothing and says so on Slack, since a watcher
// asked to page must not look as if it does; otherwise the sender is running
// when it returns.
func NewPagerAlertNotifier(settings PagerAlertNotifierSettings) *PagerAlertNotifier {
	settings = settings.withDefaults()
	notifier := &PagerAlertNotifier{
		settings:     settings,
		keyIncidents: map[string]*slackIncident{},
	}
	if settings.Sink == nil {
		reason := firstNonempty(settings.DisabledReason, "no pager sink")
		settings.Logf("monitor: pager notifications disabled: %s", reason)
		notifier.reportFailing("The watcher cannot page: " + reason)
		return notifier
	}
	settings.Logf("monitor: pager notifications enabled: one page per incident of §19.8's page classes")
	delivery := settings.Delivery
	delivery.DeliveryFailing = notifier.deliveryFailing
	notifier.sender = pager.NewRetrySink(settings.Sink, delivery)
	return notifier
}

// Handle takes every gated execution of a signal, empty ones included:
// absence from complete executions is what clears an incident.
func (self *PagerAlertNotifier) Handle(signal Signal, execution SignalExecution) {
	if self.sender == nil {
		return
	}
	now := self.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	notAtStart := func(Alert) bool { return false }
	for _, transition := range observeIncidents(self.keyIncidents, self.settings.ClearAfterCompleteRuns, signal.Key(), execution, notAtStart) {
		self.applyWithLock(now, transition)
	}
}

// Stops taking executions and gives the sender until ctx ends to hand the sink
// what it holds, retries included. Its log counts what it abandons: a later
// watcher re-raises whatever is still active.
func (self *PagerAlertNotifier) Close(ctx context.Context) {
	if self.sender == nil {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
	}()
	self.sender.Close(ctx)
}

// Queues what one transition asks of the sink. The sender's calls do not wait
// on the network, so the lock keeps an incident's events in order across
// concurrent Handles. An error here is a closing sender or a page this file
// built wrongly, and either way it must not pass unseen.
func (self *PagerAlertNotifier) applyWithLock(now time.Time, transition slackTransition) {
	alert := transition.alert
	row, paged := pageClassOf(alert.Class)
	if !paged || alert.Severity != SeverityPage {
		return
	}
	var err error
	if transition.kind == slackCleared {
		err = self.sender.Resolve(context.Background(), pagerDedupKey(self.settings.Environment, alert))
	} else {
		err = self.sender.Trigger(context.Background(), self.page(now, alert, row))
	}
	if err != nil {
		self.settings.Logf("monitor: a page of class %s was not queued: %v", row.class, err)
	}
}

// The page for one incident, built from the row and never from the alert's
// free text.
func (self *PagerAlertNotifier) page(now time.Time, alert Alert, row pageClass) pager.Page {
	observedTime := alert.ObservedAt
	if observedTime.IsZero() {
		observedTime = now
	}
	return pager.Page{
		DedupKey:   pagerDedupKey(self.settings.Environment, alert),
		Class:      row.class,
		Summary:    fmt.Sprintf("[%s] %s: %s. Then: %s.", slackEnvironment(self.settings.Environment), row.class, row.row, row.action),
		Time:       observedTime,
		BothAtOnce: row.bothAtOnce,
	}
}

// Stable across restarts, and opaque: an incident's target can be a host name,
// which must not leave the site.
func pagerDedupKey(environment string, alert Alert) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{environment, slackIncidentKey(alert)}, "|")))
	return "monitor-" + hex.EncodeToString(sum[:16])
}

// Called by the sender as hand-offs start failing and recover.
func (self *PagerAlertNotifier) deliveryFailing(failing bool) {
	if failing {
		self.reportFailing("The pager sink is not taking pages: they are held and retried until it does")
		return
	}
	if self.settings.Warn == nil {
		return
	}
	// the carrier clears an incident on complete runs without it
	for range self.settings.WarnClearRuns {
		self.settings.Warn(pagerSinkSignal{}, SignalExecution{})
	}
}

// Raises the pager's own warn.
func (self *PagerAlertNotifier) reportFailing(symptom string) {
	if self.settings.Warn == nil {
		return
	}
	alert := Alert{
		SignalKey:   "pager-sink",
		SignalID:    "monitor/pager-sink",
		SignalName:  "Pager sink",
		Severity:    SeverityWarn,
		Class:       "pager-sink-failing",
		Target:      "pager",
		Environment: self.settings.Environment,
		ObservedAt:  self.settings.Now(),
		Symptom:     symptom,
		Mechanism:   "Page-class alerts still reach Slack, but the rota's pager does not have them, so nobody is woken.",
		Action:      "Check the watcher's stderr for the failure class and the pager service's status; watch Slack for page-class alerts until it recovers.",
		Verify:      "This warn clears once the sink takes a page again.",
	}
	self.settings.Warn(pagerSinkSignal{}, SignalExecution{Alerts: Alerts{alert}})
}
