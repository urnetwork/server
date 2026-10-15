package monitor

// Optional Slack incoming-webhook notifier for the continuous watcher
// (MONITOR.md §3.6). The webhook comes from vault monitor.yml
// alerts.slack_webhook_url, the webhook Grafana's default contact point also
// posts to (`bringyourctl grafana reconcile-alerting`).
//
// It posts one concise message per alert transition. An incident is keyed by
// (signal id, class, target): frame is left out on purpose, because per-
// incident attribution that varies between observations must not re-raise
// (SIGNALS.md §7 identity discipline). An incident is
//   - raised when it first appears in a signal's gated alerts;
//   - escalated, once, when a later execution reports it at page severity;
//   - cleared after it is absent from clearAfterCompleteRuns consecutive
//     complete executions of every signal that reported it.
// Absent means the execution observed nothing with the incident's key: an
// alert the cadence gate holds below its Sustain, as it does for the first
// runs after a one-run dip, is present. An execution is incomplete when it
// failed or carries a visibility alert, gated or held: its signal did not
// observe everything, so absent incidents keep their state. Silence is not
// recovery, and a clear message says only that the incident is no longer
// reported.
//
// State is process-local, so a newly started watcher announces each alert
// that is active when it starts, including every alert a predecessor already
// announced. A raise from a signal's first Sustain complete executions, within
// StartWindow of the notifier's start, is such an alert: the gate let it
// through at the first chance this process had. Those raises are folded into
// one start digest instead of one message each. The digest posts
// StartDigestHold after its first raise, and at once when it gains a page,
// whether a page raise or the escalation of an incident it holds. A clear of
// a digest incident posts the digest first, so the channel never shows a
// clear ahead of its raise.
//
// Messages are capped at maxMessagesPerMinute over a sliding minute. Over the
// cap, transitions are folded into one summary message, sent as soon as the
// cap allows. Handle never waits on the network: it decides transitions under
// stateLock and hands messages to the one sender goroutine through a bounded
// queue with a zero-wait offer. A transition the full queue refuses is held
// for the summary like one over the cap, pages first. The sender spaces posts
// by postInterval, the rate Slack accepts for incoming webhooks.
//
// A post that Slack refuses with a 429 or a 503 is retried once, after the
// wait Slack asks for in Retry-After (RetryDelay when it names none), at most
// MaxRetryDelay. Those answers mean Slack did not take the post. A timeout or
// any other 5xx is not retried: Slack may have taken the post already, and a
// retry would post it twice. Only the sender waits; Handle keeps offering,
// and a queue that fills meanwhile holds transitions for the summary. A
// message that fails, retry included, is undelivered. Once a post succeeds
// again, or the periodic report probe finds Slack accepting posts, one report
// says how many messages failed and when they were queued; Close tries a
// pending report once more.
//
// The webhook URL is a credential. It is never logged, put in a message, or
// put in an error. net/http errors embed the request URL, so a failed post is
// reduced to a fixed class before it is logged. Message text gets the same
// address scrubbing as the process's stdout and stderr.
//
// Handle and Close are safe for concurrent use.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
)

// Lines of a rate-limit summary that name a transition; the rest are counted.
const slackSummaryLines = 20

// Transitions kept for the next summary, which picks the lines it shows from
// them; beyond this they are only counted.
const slackSummaryHeldLimit = 1000

// Spacing of undelivered reports while Slack keeps refusing them. Each report
// is also the probe for whether Slack takes posts again.
const slackUndeliveredReportInterval = time.Minute

// Slack answers a failed webhook post with a short reason token such as
// no_service or invalid_payload. Anything else in the body is not logged.
var slackReasonPattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// A Retry-After in seconds, of any length.
var slackRetryAfterSecondsPattern = regexp.MustCompile(`^[0-9]+$`)

// Settings for NewSlackAlertNotifier. Zero values take the defaults, except
// PostInterval, where zero posts back to back, and StartDigestHold, where
// zero posts every raise on its own.
type SlackAlertNotifierSettings struct {
	// empty disables the notifier
	WebhookUrl  string
	Environment string
	// sliding one-minute cap on messages, the rate-limit summary included
	MaxMessagesPerMinute int
	// consecutive complete executions without an incident before it clears
	ClearAfterCompleteRuns int
	// how long the start digest collects raises after its first before it
	// posts; a page posts it at once
	StartDigestHold time.Duration
	// how long after the notifier starts a raise can still be at start
	StartWindow time.Duration
	// minimum spacing between posts
	PostInterval time.Duration
	PostTimeout  time.Duration
	// wait before the one retry when Slack names none
	RetryDelay time.Duration
	// cap on the wait before the retry, Retry-After included
	MaxRetryDelay time.Duration
	// how often the sender checks for a held summary the cap now allows, and
	// for an undelivered report that is due
	SummaryInterval time.Duration
	QueueSize       int
	// one line per call, without a trailing newline
	Logf func(format string, args ...any)
	Now  func() time.Time

	// waits before a retry; false when ctx ends first
	retryWait func(ctx context.Context, delay time.Duration) bool
	// the sender's SummaryInterval wake-up
	after func(time.Duration) <-chan time.Time
}

// The production limits. Logf writes to stderr, the monitor's diagnostic
// stream.
func DefaultSlackAlertNotifierSettings() SlackAlertNotifierSettings {
	return SlackAlertNotifierSettings{
		MaxMessagesPerMinute:   10,
		ClearAfterCompleteRuns: 2,
		StartDigestHold:        2 * time.Minute,
		StartWindow:            30 * time.Minute,
		PostInterval:           time.Second,
		PostTimeout:            10 * time.Second,
		RetryDelay:             time.Second,
		MaxRetryDelay:          30 * time.Second,
		SummaryInterval:        5 * time.Second,
		QueueSize:              32,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
		Now: time.Now,
		retryWait: func(ctx context.Context, delay time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(delay):
				return true
			}
		},
		after: time.After,
	}
}

// Fills unset limits and seams from DefaultSlackAlertNotifierSettings.
func (self SlackAlertNotifierSettings) withDefaults() SlackAlertNotifierSettings {
	defaults := DefaultSlackAlertNotifierSettings()
	if self.MaxMessagesPerMinute <= 0 {
		self.MaxMessagesPerMinute = defaults.MaxMessagesPerMinute
	}
	if self.ClearAfterCompleteRuns <= 0 {
		self.ClearAfterCompleteRuns = defaults.ClearAfterCompleteRuns
	}
	self.StartDigestHold = max(self.StartDigestHold, 0)
	if self.StartWindow <= 0 {
		self.StartWindow = defaults.StartWindow
	}
	self.PostInterval = max(self.PostInterval, 0)
	if self.PostTimeout <= 0 {
		self.PostTimeout = defaults.PostTimeout
	}
	if self.MaxRetryDelay <= 0 {
		self.MaxRetryDelay = defaults.MaxRetryDelay
	}
	if self.RetryDelay <= 0 {
		self.RetryDelay = defaults.RetryDelay
	}
	self.RetryDelay = min(self.RetryDelay, self.MaxRetryDelay)
	if self.SummaryInterval <= 0 {
		self.SummaryInterval = defaults.SummaryInterval
	}
	if self.QueueSize <= 0 {
		self.QueueSize = defaults.QueueSize
	}
	if self.Logf == nil {
		self.Logf = defaults.Logf
	}
	if self.Now == nil {
		self.Now = defaults.Now
	}
	if self.retryWait == nil {
		self.retryWait = defaults.retryWait
	}
	if self.after == nil {
		self.after = defaults.after
	}
	return self
}

// What happened to an incident.
type slackTransitionKind int

const (
	slackRaised slackTransitionKind = iota
	slackEscalated
	slackCleared
)

// One incident change to announce.
type slackTransition struct {
	kind  slackTransitionKind
	alert Alert
	// a raise from its signal's first Sustain complete executions
	atStart bool
}

// One announced incident.
type slackIncident struct {
	// the latest report, for rendering
	alert Alert
	// the highest severity announced
	severity Severity
	// signal key -> consecutive complete executions without the incident
	signalKeyAbsentRuns map[string]int
}

// One queued post.
type slackMessage struct {
	text       string
	queuedTime time.Time
}

// Messages that still failed after their retry, not yet reported in Slack.
type slackUndelivered struct {
	count           int
	firstQueuedTime time.Time
	lastQueuedTime  time.Time
	// the first line of each of the first slackSummaryLines messages
	headlines []string
	// the last attempt to post the report
	reportTime time.Time
}

// The result of one post attempt.
type slackPostOutcome struct {
	delivered bool
	// a 429 or a 503: Slack did not take the post, so one retry after
	// retryDelay cannot post it twice
	retryable  bool
	retryDelay time.Duration
}

// Posts alert transitions to a Slack incoming webhook (see the file comment).
type SlackAlertNotifier struct {
	settings   SlackAlertNotifierSettings
	enabled    bool
	httpClient *http.Client
	ctx        context.Context
	cancel     context.CancelFunc
	messages   chan slackMessage
	done       chan struct{}

	stateLock    sync.Mutex
	closed       bool
	keyIncidents map[string]*slackIncident
	sentTimes    []time.Time
	// transitions held for the next summary, over the cap or refused by the
	// full queue
	suppressed      []slackTransition
	suppressedCount int
	// when the notifier started, for StartWindow
	startTime time.Time
	// signal key -> executions that did not fail
	signalKeyCompleteRuns map[string]int
	// raises waiting for the start digest, and their index by incident key
	startDigest          []slackTransition
	startDigestKeyIndex  map[string]int
	startDigestFirstTime time.Time
	// the digest posts before anything else: it holds a page, or a clear of
	// one of its incidents waits behind it
	startDigestDue bool

	// owned by the sender goroutine
	postFailing bool
	failedPosts int
	undelivered slackUndelivered
	// posts cut short by Close's deadline
	abandonedCount int
}

// NewSlackAlertNotifier logs one line saying whether notifications are on.
// Without a usable webhook it returns a disabled notifier whose methods do
// nothing; otherwise the sender goroutine is running when it returns.
func NewSlackAlertNotifier(settings SlackAlertNotifierSettings) *SlackAlertNotifier {
	settings = settings.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	notifier := &SlackAlertNotifier{
		settings:              settings,
		ctx:                   ctx,
		cancel:                cancel,
		done:                  make(chan struct{}),
		keyIncidents:          map[string]*slackIncident{},
		startTime:             settings.Now(),
		signalKeyCompleteRuns: map[string]int{},
		startDigestKeyIndex:   map[string]int{},
	}
	settings.WebhookUrl = strings.TrimSpace(settings.WebhookUrl)
	if settings.WebhookUrl == "" {
		settings.Logf("monitor: Slack alert notifications disabled: vault monitor.yml alerts.slack_webhook_url is empty")
		cancel()
		close(notifier.done)
		return notifier
	}
	// url.Parse errors embed their input, so only the verdict is used. Plain
	// http is accepted for loopback test servers only.
	parsed, err := url.Parse(settings.WebhookUrl)
	usable := err == nil && parsed.Host != "" && parsed.User == nil
	if usable && parsed.Scheme != "https" {
		hostname := parsed.Hostname()
		address := net.ParseIP(hostname)
		usable = parsed.Scheme == "http" && (hostname == "localhost" || (address != nil && address.IsLoopback()))
	}
	if !usable {
		settings.Logf("monitor: Slack alert notifications disabled: vault monitor.yml alerts.slack_webhook_url is not an absolute https URL")
		cancel()
		close(notifier.done)
		return notifier
	}
	notifier.settings = settings
	notifier.enabled = true
	notifier.messages = make(chan slackMessage, settings.QueueSize)
	notifier.httpClient = &http.Client{
		Transport: server.NewHttpTransport(),
		Timeout:   settings.PostTimeout,
		// a redirect would send alert content to another host
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	settings.Logf(
		"monitor: Slack alert notifications enabled: one message per alert transition, at most %d per minute",
		settings.MaxMessagesPerMinute,
	)
	go notifier.run()
	return notifier
}

// Handle takes every gated execution of a signal, empty ones included:
// absence from complete executions is what clears an incident.
func (self *SlackAlertNotifier) Handle(signal Signal, execution SignalExecution) {
	if !self.enabled {
		return
	}
	now := self.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	self.admitWithLock(now, self.observeWithLock(now, signal.Key(), execution))
}

// Close stops accepting batches, queues a pending start digest and a held
// summary regardless of the cap, and waits for the sender to post what is
// queued and a pending undelivered report. When ctx ends first, the remaining
// posts are abandoned. The first Close logs what was never posted: abandoned
// messages, including a digest or summary the full queue refused, and
// undelivered messages never reported in Slack.
func (self *SlackAlertNotifier) Close(ctx context.Context) {
	if !self.enabled {
		return
	}
	first := false
	abandonedCount := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return
		}
		first = true
		self.closed = true
		heldTexts := []string{}
		if 0 < len(self.startDigest) {
			heldTexts = append(heldTexts, self.startDigestTextWithLock())
		}
		if 0 < self.suppressedCount {
			heldTexts = append(heldTexts, self.summaryTextWithLock())
		}
		for _, text := range heldTexts {
			select {
			case self.messages <- slackMessage{text: text, queuedTime: self.settings.Now()}:
			default:
				abandonedCount++
			}
		}
		close(self.messages)
	}()
	select {
	case <-self.done:
	case <-ctx.Done():
		self.cancel()
		<-self.done
	}
	self.cancel()
	if !first {
		return
	}
	// the sender has returned, so its state is settled and what is still
	// queued was never posted
	for range self.messages {
		abandonedCount++
	}
	abandonedCount += self.abandonedCount
	if 0 < abandonedCount {
		self.settings.Logf("monitor: Slack alert notifier abandoned %d unposted message(s) at close", abandonedCount)
	}
	if 0 < self.undelivered.count {
		self.settings.Logf(
			"monitor: %d Slack alert message(s) queued between %s and %s failed to post and were never reported in Slack",
			self.undelivered.count,
			self.undelivered.firstQueuedTime.UTC().Format(time.RFC3339),
			self.undelivered.lastQueuedTime.UTC().Format(time.RFC3339),
		)
	}
}

// Frame is left out: SIGNALS.md §7 matches resolution on (probe, class,
// target).
func slackIncidentKey(alert Alert) string {
	return strings.Join([]string{alert.SignalID, alert.Class, alert.Target}, "|")
}

// A visibility alert says a signal could not observe part of its source, so
// the rest of that execution's identities are unknown rather than recovered.
// An alert from a finding is marked by its class (alertClassKinds, see
// finding.isUnobservable), and a direct constructor of an unknown state marks
// its own alert.
func isVisibilityAlert(alert Alert) bool {
	return alert.unobservable
}

// Applies one signal execution to the incidents and returns its transitions:
// raises and escalations in severity-then-identity order, then clears in key
// order.
func (self *SlackAlertNotifier) observeWithLock(now time.Time, signalKey string, execution SignalExecution) []slackTransition {
	complete := !execution.Failed
	keyAlerts := map[string]Alert{}
	for _, alert := range execution.Alerts {
		if isVisibilityAlert(alert) {
			complete = false
		}
		// a state the configuration chose, reported every run, is not an
		// incident: it would only fill every start digest
		if alert.configuredOff {
			continue
		}
		key := slackIncidentKey(alert)
		if previous, ok := keyAlerts[key]; ok && (previous.Severity == SeverityPage || alert.Severity != SeverityPage) {
			continue
		}
		keyAlerts[key] = alert
	}
	// a held alert raises nothing yet, but its incident is still observed
	heldKeys := map[string]bool{}
	for _, alert := range execution.HeldAlerts {
		if isVisibilityAlert(alert) {
			complete = false
		}
		heldKeys[slackIncidentKey(alert)] = true
	}
	present := make(Alerts, 0, len(keyAlerts))
	for _, alert := range keyAlerts {
		present = append(present, alert)
	}
	if !execution.Failed {
		self.signalKeyCompleteRuns[signalKey]++
	}
	completeRuns := self.signalKeyCompleteRuns[signalKey]
	// A raise is at start when the gate passed it at the first chance this
	// process had: within the first Sustain complete runs, since the gate
	// resets a streak at every run without the alert, or for a failed run,
	// before any complete one. Only within StartWindow of the start: a signal
	// that fails for hours has no start left when it first completes.
	atStart := func(alert Alert) bool {
		if self.settings.StartDigestHold <= 0 || self.settings.StartWindow < now.Sub(self.startTime) {
			return false
		}
		if execution.Failed {
			return completeRuns == 0
		}
		return completeRuns <= max(alert.Sustain, 1)
	}

	transitions := []slackTransition{}
	for _, alert := range orderedAlerts(present) {
		key := slackIncidentKey(alert)
		incident, ok := self.keyIncidents[key]
		if !ok {
			self.keyIncidents[key] = &slackIncident{
				alert:               alert,
				severity:            alert.Severity,
				signalKeyAbsentRuns: map[string]int{signalKey: 0},
			}
			transitions = append(transitions, slackTransition{kind: slackRaised, alert: alert, atStart: atStart(alert)})
			continue
		}
		incident.alert = alert
		incident.signalKeyAbsentRuns[signalKey] = 0
		if alert.Severity == SeverityPage && incident.severity != SeverityPage {
			incident.severity = SeverityPage
			transitions = append(transitions, slackTransition{kind: slackEscalated, alert: alert})
		}
	}
	for key := range heldKeys {
		if incident, ok := self.keyIncidents[key]; ok {
			incident.signalKeyAbsentRuns[signalKey] = 0
		}
	}
	if !complete {
		return transitions
	}

	keys := make([]string, 0, len(self.keyIncidents))
	for key := range self.keyIncidents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		incident := self.keyIncidents[key]
		absentRuns, owned := incident.signalKeyAbsentRuns[signalKey]
		_, reported := keyAlerts[key]
		if !owned || reported || heldKeys[key] {
			continue
		}
		absentRuns++
		if absentRuns < self.settings.ClearAfterCompleteRuns {
			incident.signalKeyAbsentRuns[signalKey] = absentRuns
			continue
		}
		delete(incident.signalKeyAbsentRuns, signalKey)
		if len(incident.signalKeyAbsentRuns) != 0 {
			// another signal still reports it
			continue
		}
		delete(self.keyIncidents, key)
		cleared := incident.alert
		cleared.Severity = incident.severity
		transitions = append(transitions, slackTransition{kind: slackCleared, alert: cleared})
	}
	return transitions
}

// Folds at-start raises into the start digest, then offers what the
// sliding-minute cap allows: a due digest first, then a held summary, then the
// new transitions, so the channel keeps transition order. The rest is held for
// the next summary.
func (self *SlackAlertNotifier) admitWithLock(now time.Time, transitions []slackTransition) {
	windowStart := now.Add(-time.Minute)
	sentTimes := self.sentTimes[:0]
	for _, sentTime := range self.sentTimes {
		if sentTime.After(windowStart) {
			sentTimes = append(sentTimes, sentTime)
		}
	}
	self.sentTimes = sentTimes
	allowed := func() bool {
		return len(self.sentTimes) < self.settings.MaxMessagesPerMinute
	}
	offer := func(text string) bool {
		select {
		case self.messages <- slackMessage{text: text, queuedTime: now}:
			self.sentTimes = append(self.sentTimes, now)
			return true
		default:
			return false
		}
	}

	posting := make([]slackTransition, 0, len(transitions))
	for _, transition := range transitions {
		key := slackIncidentKey(transition.alert)
		index, inDigest := self.startDigestKeyIndex[key]
		switch {
		case transition.atStart:
			// the digest carries the incident's latest state: a transition of
			// it still held for the summary, such as its clear behind a
			// refused digest, would post after the digest and contradict it
			self.dropHeldWithLock(key)
			if inDigest {
				self.startDigest[index] = transition
			} else {
				if len(self.startDigest) == 0 {
					self.startDigestFirstTime = now
				}
				self.startDigestKeyIndex[key] = len(self.startDigest)
				self.startDigest = append(self.startDigest, transition)
			}
			// a page does not wait for the rest of the start
			if transition.alert.Severity == SeverityPage {
				self.startDigestDue = true
			}
		case inDigest && transition.kind == slackEscalated:
			// the digest announces the page, at once
			self.startDigest[index].alert = transition.alert
			self.startDigestDue = true
		default:
			if inDigest {
				self.startDigestDue = true
			}
			posting = append(posting, transition)
		}
	}

	// a digest or summary the full queue refuses keeps what it holds for the
	// next try, and a due digest holds back everything after it
	digestBlocking := 0 < len(self.startDigest) && (self.startDigestDue || self.settings.StartDigestHold <= now.Sub(self.startDigestFirstTime))
	if digestBlocking && allowed() && offer(self.startDigestTextWithLock()) {
		self.startDigest = nil
		self.startDigestKeyIndex = map[string]int{}
		self.startDigestDue = false
		digestBlocking = false
	}
	if !digestBlocking && 0 < self.suppressedCount && allowed() && offer(self.summaryTextWithLock()) {
		self.suppressed = nil
		self.suppressedCount = 0
	}
	for _, transition := range posting {
		// a transition the full queue refuses is held like one over the cap,
		// so the summary names it and later transitions stay behind it
		if !digestBlocking && self.suppressedCount == 0 && allowed() &&
			offer(slackTransitionText(self.settings.Environment, transition, self.settings.ClearAfterCompleteRuns)) {
			continue
		}
		self.suppressedCount++
		if len(self.suppressed) < slackSummaryHeldLimit {
			self.suppressed = append(self.suppressed, transition)
		}
	}
}

// Removes an incident's transitions from those held for the summary.
func (self *SlackAlertNotifier) dropHeldWithLock(key string) {
	kept := self.suppressed[:0]
	for _, transition := range self.suppressed {
		if slackIncidentKey(transition.alert) == key {
			self.suppressedCount--
			continue
		}
		kept = append(kept, transition)
	}
	self.suppressed = kept
}

// The summary of the held transitions. Incidents with a page raise or
// escalation lead, each incident's lines in order, so a page is named even
// when the summary has more lines than it shows.
func (self *SlackAlertNotifier) summaryTextWithLock() string {
	var b strings.Builder
	fmt.Fprintf(
		&b,
		"[RATE LIMIT] (%s) %d alert transition(s) were not posted one by one (at most %d messages per minute, or the sender was backlogged):",
		slackEscape(slackEnvironment(self.settings.Environment)),
		self.suppressedCount,
		self.settings.MaxMessagesPerMinute,
	)
	pageKeys := map[string]bool{}
	for _, transition := range self.suppressed {
		if transition.kind == slackEscalated || (transition.kind == slackRaised && transition.alert.Severity == SeverityPage) {
			pageKeys[slackIncidentKey(transition.alert)] = true
		}
	}
	held := append([]slackTransition(nil), self.suppressed...)
	sort.SliceStable(held, func(i, j int) bool {
		return pageKeys[slackIncidentKey(held[i].alert)] && !pageKeys[slackIncidentKey(held[j].alert)]
	})
	label := func(transition slackTransition) string {
		switch transition.kind {
		case slackEscalated:
			return "[PAGE] escalated"
		case slackCleared:
			return "[CLEARED] " + slackSeverityLabel(transition.alert.Severity)
		default:
			return "[" + slackSeverityLabel(transition.alert.Severity) + "] raised"
		}
	}
	shown := held[:min(len(held), slackSummaryLines)]
	for _, transition := range shown {
		fmt.Fprintf(&b, "\n- %s: %s", label(transition), slackIdentityLine(transition.alert))
	}
	if hidden := self.suppressedCount - len(shown); 0 < hidden {
		fmt.Fprintf(&b, "\n- and %d more", hidden)
	}
	b.WriteString("\nThe monitor's alert output carries every alert.")
	return b.String()
}

// The start digest of the pending at-start raises, pages first.
func (self *SlackAlertNotifier) startDigestTextWithLock() string {
	entries := append([]slackTransition(nil), self.startDigest...)
	sort.SliceStable(entries, func(i, j int) bool {
		if (entries[i].alert.Severity == SeverityPage) != (entries[j].alert.Severity == SeverityPage) {
			return entries[i].alert.Severity == SeverityPage
		}
		return slackIncidentKey(entries[i].alert) < slackIncidentKey(entries[j].alert)
	})
	var b strings.Builder
	fmt.Fprintf(
		&b,
		"[ACTIVE AT START] (%s) %d incident(s) reported by this watcher's first runs of their signals:",
		slackEscape(slackEnvironment(self.settings.Environment)),
		len(entries),
	)
	for _, entry := range entries[:min(len(entries), slackSummaryLines)] {
		fmt.Fprintf(&b, "\n- [%s] %s", slackSeverityLabel(entry.alert.Severity), slackIdentityLine(entry.alert))
	}
	if hidden := len(entries) - slackSummaryLines; 0 < hidden {
		fmt.Fprintf(&b, "\n- and %d more", hidden)
	}
	b.WriteString("\nAlert state is per watcher, so these can repeat earlier announcements. Later transitions post one by one.")
	return b.String()
}

// Posts held summaries and a settled start digest once the cap allows them.
func (self *SlackAlertNotifier) flushSummary() {
	now := self.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	self.admitWithLock(now, nil)
}

// The sender: posts queued messages in order, spaced by PostInterval, and
// every SummaryInterval checks for a held summary and a due undelivered
// report. It returns when the queue is closed and drained, after one last
// try at a pending report, or when Close's deadline cancels it.
func (self *SlackAlertNotifier) run() {
	defer close(self.done)
	for {
		select {
		case <-self.ctx.Done():
			return
		case message, ok := <-self.messages:
			if !ok {
				if 0 < self.undelivered.count {
					self.reportUndelivered()
				}
				return
			}
			self.deliver(message)
			if !self.waitPostInterval() {
				return
			}
		case <-self.settings.after(self.settings.SummaryInterval):
			self.flushSummary()
			if 0 < self.undelivered.count && slackUndeliveredReportInterval <= self.settings.Now().Sub(self.undelivered.reportTime) {
				self.reportUndelivered()
			}
		}
	}
}

// Posts one queued message, retrying once. A success after failures is the
// sign Slack takes posts again, so the undelivered report follows it.
func (self *SlackAlertNotifier) deliver(message slackMessage) {
	if self.postWithRetry(message.text) {
		if 0 < self.undelivered.count && self.waitPostInterval() {
			self.reportUndelivered()
		}
		return
	}
	if self.ctx.Err() != nil {
		// Close's deadline cut the post short; Slack did not refuse it
		self.abandonedCount++
		return
	}
	if self.undelivered.count == 0 {
		self.undelivered.firstQueuedTime = message.queuedTime
	}
	self.undelivered.count++
	self.undelivered.lastQueuedTime = message.queuedTime
	if len(self.undelivered.headlines) < slackSummaryLines {
		headline, _, _ := strings.Cut(message.text, "\n")
		self.undelivered.headlines = append(self.undelivered.headlines, slackTruncate(headline, 200))
	}
}

// Posts the undelivered report. It is not held by the cap, since Slack has
// just recovered, but later offers count it. A failed report keeps the
// undelivered messages for the next one.
func (self *SlackAlertNotifier) reportUndelivered() {
	self.undelivered.reportTime = self.settings.Now()
	var b strings.Builder
	fmt.Fprintf(
		&b,
		"[UNDELIVERED] (%s) %d message(s) queued between %s and %s failed to post:",
		slackEscape(slackEnvironment(self.settings.Environment)),
		self.undelivered.count,
		self.undelivered.firstQueuedTime.UTC().Format(time.RFC3339),
		self.undelivered.lastQueuedTime.UTC().Format(time.RFC3339),
	)
	for _, headline := range self.undelivered.headlines {
		fmt.Fprintf(&b, "\n- %s", headline)
	}
	if hidden := self.undelivered.count - len(self.undelivered.headlines); 0 < hidden {
		fmt.Fprintf(&b, "\n- and %d more", hidden)
	}
	b.WriteString("\nThe monitor's alert output carries every alert.")
	if !self.postWithRetry(b.String()) {
		return
	}
	self.undelivered = slackUndelivered{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.sentTimes = append(self.sentTimes, self.settings.Now())
	}()
}

// Spaces posts by PostInterval; false when Close's deadline ends the wait.
func (self *SlackAlertNotifier) waitPostInterval() bool {
	if self.settings.PostInterval <= 0 {
		return true
	}
	select {
	case <-self.ctx.Done():
		return false
	case <-time.After(self.settings.PostInterval):
		return true
	}
}

// Posts once, and once more after the wait Slack asks for when the first
// attempt is retryable. Only the sender waits.
func (self *SlackAlertNotifier) postWithRetry(text string) bool {
	outcome := self.post(text)
	if outcome.delivered || !outcome.retryable {
		return outcome.delivered
	}
	if !self.settings.retryWait(self.ctx, outcome.retryDelay) {
		return false
	}
	return self.post(text).delivered
}

// The wait before a retry: Retry-After in seconds or as an HTTP date when
// Slack sends one, RetryDelay otherwise, at most MaxRetryDelay.
func (self *SlackAlertNotifier) retryDelay(retryAfter string) time.Duration {
	retryAfter = strings.TrimSpace(retryAfter)
	if slackRetryAfterSecondsPattern.MatchString(retryAfter) {
		// a count past int64 is only a longer wait than the cap
		seconds, err := strconv.ParseInt(retryAfter, 10, 64)
		if err != nil || int64(self.settings.MaxRetryDelay/time.Second) <= seconds {
			return self.settings.MaxRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if retryTime, err := http.ParseTime(retryAfter); err == nil {
		return min(max(retryTime.Sub(self.settings.Now()), 0), self.settings.MaxRetryDelay)
	}
	return self.settings.RetryDelay
}

// Posts one message once. Addresses are scrubbed as on the process's stdout
// and stderr (server.ScrubProcessLogs), and link and media unfurling stay
// off, so nothing in an alert makes Slack fetch a URL.
func (self *SlackAlertNotifier) post(text string) slackPostOutcome {
	payload, err := json.Marshal(map[string]any{
		"text":         server.ScrubAddrsText(text),
		"unfurl_links": false,
		"unfurl_media": false,
	})
	if err != nil {
		self.postFailed("encode")
		return slackPostOutcome{}
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.PostTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.settings.WebhookUrl, bytes.NewReader(payload))
	if err != nil {
		self.postFailed("request")
		return slackPostOutcome{}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a fixed class: the error text is never used, since net/http errors
		// embed the webhook URL
		errorClass := func() string {
			if errors.Is(err, context.DeadlineExceeded) {
				return "timeout"
			}
			if errors.Is(err, context.Canceled) {
				return "canceled"
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return "timeout"
			}
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) {
				return "dns"
			}
			var certificateErr *tls.CertificateVerificationError
			var recordErr tls.RecordHeaderError
			if errors.As(err, &certificateErr) || errors.As(err, &recordErr) {
				return "tls"
			}
			var opErr *net.OpError
			if errors.As(err, &opErr) {
				return "network"
			}
			return "transport"
		}()
		if errorClass == "canceled" {
			// Close's deadline, not Slack: Close counts the abandoned post
			return slackPostOutcome{}
		}
		self.postFailed(errorClass)
		return slackPostOutcome{}
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
	response.Body.Close()
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		class := fmt.Sprintf("http-%d", response.StatusCode)
		if reason := strings.TrimSpace(string(body)); slackReasonPattern.MatchString(reason) {
			class += " " + reason
		}
		self.postFailed(class)
		return slackPostOutcome{
			retryable:  response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable,
			retryDelay: self.retryDelay(response.Header.Get("Retry-After")),
		}
	}
	if self.postFailing {
		self.settings.Logf("monitor: Slack alert posts recovered after %d failed post(s)", self.failedPosts)
		self.postFailing = false
		self.failedPosts = 0
	}
	return slackPostOutcome{delivered: true}
}

// Logs the first failure of a run of failures; the rest are counted.
func (self *SlackAlertNotifier) postFailed(class string) {
	self.failedPosts++
	if self.postFailing {
		return
	}
	self.postFailing = true
	self.settings.Logf("monitor: Slack alert post failed (%s); later failures are counted until a post succeeds", class)
}

// The message for one transition: a headline, the identity, and for a raise
// the SIGNALS.md reference and playbook.
func slackTransitionText(environment string, transition slackTransition, clearAfterCompleteRuns int) string {
	alert := transition.alert
	headline := firstNonempty(alert.Symptom, alert.SignalName, alert.SignalID)
	lines := []string{}
	switch transition.kind {
	case slackRaised:
		lines = append(lines, fmt.Sprintf("[%s] raised (%s): %s", slackSeverityLabel(alert.Severity), slackEscape(slackEnvironment(environment)), slackEscape(slackTruncate(headline, 280))))
	case slackEscalated:
		lines = append(lines, fmt.Sprintf("[PAGE] escalated from WARN (%s): %s", slackEscape(slackEnvironment(environment)), slackEscape(slackTruncate(headline, 280))))
	case slackCleared:
		lines = append(lines, fmt.Sprintf("[CLEARED] %s (%s): %s", slackSeverityLabel(alert.Severity), slackEscape(slackEnvironment(environment)), slackEscape(slackTruncate(firstNonempty(alert.SignalName, alert.SignalID), 280))))
	}
	lines = append(lines, slackIdentityLine(alert))
	switch transition.kind {
	case slackCleared:
		lines = append(lines, fmt.Sprintf(
			"Absent from %d consecutive complete runs of each signal that reported it. This is not proof of recovery: confirm with the alert's verify step.",
			clearAfterCompleteRuns,
		))
	default:
		reference := []string{}
		if alert.SignalNumber != "" {
			reference = append(reference, "SIGNALS.md §"+alert.SignalNumber)
		}
		if alert.SignalKey != "" {
			reference = append(reference, alert.SignalKey)
		}
		if alert.Playbook != "" {
			reference = append(reference, "playbook "+alert.Playbook)
		}
		if 0 < len(reference) {
			lines = append(lines, slackEscape(slackTruncate(strings.Join(reference, " · "), 280)))
		}
	}
	return strings.Join(lines, "\n")
}

// signal id · class · target, escaped and bounded.
func slackIdentityLine(alert Alert) string {
	parts := []string{}
	for _, part := range []string{alert.SignalID, alert.Class, alert.Target} {
		if strings.TrimSpace(part) != "" {
			parts = append(parts, part)
		}
	}
	return slackEscape(slackTruncate(strings.Join(parts, " · "), 300))
}

// The page label, or the warn label for any other severity.
func slackSeverityLabel(severity Severity) string {
	if severity == SeverityPage {
		return "PAGE"
	}
	return "WARN"
}

// The environment label, never empty.
func slackEnvironment(environment string) string {
	return firstNonempty(environment, "unknown env")
}

// Slack reads &, < and > as control sequences (<!channel>, links); alert
// text must not be able to mention or link.
func slackEscape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// Collapses whitespace and bounds the text to limit runes.
func slackTruncate(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}
