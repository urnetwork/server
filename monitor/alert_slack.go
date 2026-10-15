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
// reported. State is process-local, so a newly started watcher announces each
// alert that is active when it starts.
//
// Messages are capped at maxMessagesPerMinute over a sliding minute. Over the
// cap, transitions are folded into one summary message, sent as soon as the
// cap allows. Handle never waits on the network: it decides transitions under
// stateLock and hands messages to the one sender goroutine through a bounded
// queue with a zero-wait offer. A full queue drops the message and counts it
// in the next summary. The sender spaces posts by postInterval, the rate Slack
// accepts for incoming webhooks.
//
// The webhook URL is a credential. It is never logged, put in a message, or
// put in an error. net/http errors embed the request URL, so a failed post is
// reduced to a fixed class before it is logged.
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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
)

// Lines of a rate-limit summary that name a transition; the rest are counted.
const slackSummaryLines = 20

// Slack answers a failed webhook post with a short reason token such as
// no_service or invalid_payload. Anything else in the body is not logged.
var slackReasonPattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// Settings for NewSlackAlertNotifier. Zero values take the defaults, except
// PostInterval, where zero posts back to back.
type SlackAlertNotifierSettings struct {
	// empty disables the notifier
	WebhookUrl  string
	Environment string
	// sliding one-minute cap on messages, the rate-limit summary included
	MaxMessagesPerMinute int
	// consecutive complete executions without an incident before it clears
	ClearAfterCompleteRuns int
	// minimum spacing between posts
	PostInterval time.Duration
	PostTimeout  time.Duration
	// how often the sender checks for a held summary the cap now allows
	SummaryInterval time.Duration
	QueueSize       int
	// one line per call, without a trailing newline
	Logf func(format string, args ...any)
	Now  func() time.Time
}

// The production limits. Logf writes to stderr, the monitor's diagnostic
// stream.
func DefaultSlackAlertNotifierSettings() SlackAlertNotifierSettings {
	return SlackAlertNotifierSettings{
		MaxMessagesPerMinute:   10,
		ClearAfterCompleteRuns: 2,
		PostInterval:           time.Second,
		PostTimeout:            10 * time.Second,
		SummaryInterval:        5 * time.Second,
		QueueSize:              32,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
		Now: time.Now,
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
	self.PostInterval = max(self.PostInterval, 0)
	if self.PostTimeout <= 0 {
		self.PostTimeout = defaults.PostTimeout
	}
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

// Posts alert transitions to a Slack incoming webhook (see the file comment).
type SlackAlertNotifier struct {
	settings   SlackAlertNotifierSettings
	enabled    bool
	httpClient *http.Client
	ctx        context.Context
	cancel     context.CancelFunc
	messages   chan string
	done       chan struct{}

	stateLock       sync.Mutex
	closed          bool
	keyIncidents    map[string]*slackIncident
	sentTimes       []time.Time
	suppressed      []slackTransition
	suppressedCount int
	droppedCount    int

	// owned by the sender goroutine
	postFailing bool
	failedPosts int
}

// NewSlackAlertNotifier logs one line saying whether notifications are on.
// Without a usable webhook it returns a disabled notifier whose methods do
// nothing; otherwise the sender goroutine is running when it returns.
func NewSlackAlertNotifier(settings SlackAlertNotifierSettings) *SlackAlertNotifier {
	settings = settings.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	notifier := &SlackAlertNotifier{
		settings:     settings,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		keyIncidents: map[string]*slackIncident{},
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
	notifier.messages = make(chan string, settings.QueueSize)
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
	self.admitWithLock(now, self.observeWithLock(signal.Key(), execution))
}

// Close stops accepting batches, queues a held summary regardless of the cap,
// and waits for the sender to post what is queued. When ctx ends first, the
// remaining posts are abandoned.
func (self *SlackAlertNotifier) Close(ctx context.Context) {
	if !self.enabled {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return
		}
		self.closed = true
		if 0 < self.suppressedCount || 0 < self.droppedCount {
			select {
			case self.messages <- self.summaryTextWithLock():
			default:
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
}

// Frame is left out: SIGNALS.md §7 matches resolution on (probe, class,
// target).
func slackIncidentKey(alert Alert) string {
	return strings.Join([]string{alert.SignalID, alert.Class, alert.Target}, "|")
}

// A visibility alert says a signal could not observe its source, so the rest
// of that execution's identities are unknown rather than recovered.
func isVisibilityAlert(alert Alert) bool {
	return alert.SignalID == "monitor/visibility" || alert.Class == "pg-query-sample-unavailable"
}

// Applies one signal execution to the incidents and returns its transitions:
// raises and escalations in severity-then-identity order, then clears in key
// order.
func (self *SlackAlertNotifier) observeWithLock(signalKey string, execution SignalExecution) []slackTransition {
	complete := !execution.Failed
	keyAlerts := map[string]Alert{}
	for _, alert := range execution.Alerts {
		if isVisibilityAlert(alert) {
			complete = false
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
			transitions = append(transitions, slackTransition{kind: slackRaised, alert: alert})
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

// Offers what the sliding-minute cap allows, a held summary first so the
// channel keeps transition order, and holds the rest for the next summary.
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
		case self.messages <- text:
			self.sentTimes = append(self.sentTimes, now)
			return true
		default:
			return false
		}
	}

	// a summary the full queue refuses keeps what it holds for the next try
	if (0 < self.suppressedCount || 0 < self.droppedCount) && allowed() && offer(self.summaryTextWithLock()) {
		self.suppressed = nil
		self.suppressedCount = 0
		self.droppedCount = 0
	}
	for _, transition := range transitions {
		if self.suppressedCount == 0 && allowed() {
			if !offer(slackTransitionText(self.settings.Environment, transition, self.settings.ClearAfterCompleteRuns)) {
				self.droppedCount++
			}
			continue
		}
		self.suppressedCount++
		if len(self.suppressed) < slackSummaryLines {
			self.suppressed = append(self.suppressed, transition)
		}
	}
}

// The rate-limit summary of the held transitions and dropped messages.
func (self *SlackAlertNotifier) summaryTextWithLock() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[RATE LIMIT] (%s)", slackEscape(slackEnvironment(self.settings.Environment)))
	if 0 < self.suppressedCount {
		fmt.Fprintf(
			&b,
			" %d alert transition(s) were not posted one by one (at most %d messages per minute):",
			self.suppressedCount,
			self.settings.MaxMessagesPerMinute,
		)
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
		for _, transition := range self.suppressed {
			fmt.Fprintf(&b, "\n- %s: %s", label(transition), slackIdentityLine(transition.alert))
		}
		if hidden := self.suppressedCount - len(self.suppressed); 0 < hidden {
			fmt.Fprintf(&b, "\n- and %d more", hidden)
		}
	}
	if 0 < self.droppedCount {
		fmt.Fprintf(&b, "\n%d message(s) were dropped while the Slack sender was backlogged.", self.droppedCount)
	}
	b.WriteString("\nThe monitor's alert output carries every alert.")
	return b.String()
}

// Posts held summaries once the cap allows them.
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
// checks for a held summary every SummaryInterval. It returns when the queue
// is closed and drained, or when Close's deadline cancels it.
func (self *SlackAlertNotifier) run() {
	defer close(self.done)
	for {
		select {
		case <-self.ctx.Done():
			return
		case text, ok := <-self.messages:
			if !ok {
				return
			}
			self.post(text)
			if 0 < self.settings.PostInterval {
				select {
				case <-self.ctx.Done():
					return
				case <-time.After(self.settings.PostInterval):
				}
			}
		case <-time.After(self.settings.SummaryInterval):
			self.flushSummary()
		}
	}
}

// Posts one message. Link and media unfurling stay off, so nothing in an
// alert makes Slack fetch a URL.
func (self *SlackAlertNotifier) post(text string) {
	payload, err := json.Marshal(map[string]any{
		"text":         text,
		"unfurl_links": false,
		"unfurl_media": false,
	})
	if err != nil {
		self.postFailed("encode")
		return
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.PostTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.settings.WebhookUrl, bytes.NewReader(payload))
	if err != nil {
		self.postFailed("request")
		return
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
		}
		self.postFailed(errorClass())
		return
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
	response.Body.Close()
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		class := fmt.Sprintf("http-%d", response.StatusCode)
		if reason := strings.TrimSpace(string(body)); slackReasonPattern.MatchString(reason) {
			class += " " + reason
		}
		self.postFailed(class)
		return
	}
	if self.postFailing {
		self.settings.Logf("monitor: Slack alert posts recovered after %d failed post(s)", self.failedPosts)
		self.postFailing = false
		self.failedPosts = 0
	}
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
