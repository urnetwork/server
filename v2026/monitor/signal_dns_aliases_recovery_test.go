package monitor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Each phase changes only one synthetic alias/family. Other complete healthy
// aliases cannot incidentally block the notifier's complete-absence credit.
func dnsRecoverySource(phase *atomic.Int32) *dnsAliasSyntheticSource {
	source := healthyDNSAliasSource()
	baseAuthority, baseRecursive := source.authoritativeFn, source.recursiveFn
	source.authoritativeFn = func(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
		if hostname == "alt.example.test" && recordType == DNSRecordA {
			switch phase.Load() {
			case 1:
				return DNSAuthoritativeObservation{NameserverCount: 2, Responses: []DNSResponseObservation{
					{Addresses: []string{"203.0.113.90"}, ResponseCode: DNSResponseSuccess, Authoritative: true},
					{Addresses: []string{"203.0.113.90"}, ResponseCode: DNSResponseSuccess, Authoritative: true},
				}}, nil
			case 2:
				return DNSAuthoritativeObservation{}, errors.New("synthetic-private-authority-detail")
			case 4:
				return DNSAuthoritativeObservation{NameserverCount: 2, FailedNameservers: 1, Responses: []DNSResponseObservation{
					{Addresses: []string{"203.0.113.90"}, ResponseCode: DNSResponseSuccess, Authoritative: true},
				}}, nil
			}
		}
		return baseAuthority(ctx, zone, hostname, recordType)
	}
	source.recursiveFn = func(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
		if hostname == "alt.example.test" && recordType == DNSRecordA && phase.Load() == 0 {
			return DNSResponseObservation{Addresses: []string{"203.0.113.91"}, ResponseCode: DNSResponseSuccess}, nil
		}
		return baseRecursive(ctx, hostname, recordType)
	}
	return source
}

// Use the production execution-loop engine, public signal adapter and notifier
// Handle. Only the ticker factory is replaced; manual ticks and handler joins
// force every observation and retain one cadence gate across all phases.
func dnsRecoveryLoop(t *testing.T, phase *atomic.Int32, source *dnsAliasSyntheticSource, notifier *SlackAlertNotifier) func(int32) SignalExecution {
	t.Helper()
	signal := NewDNSAliasesSignal()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan cadenceFloorTick, 1)
	handled := make(chan SignalExecution, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- NewWithSignals(dnsAliasFixtureSettings(source), signal).runExecutionLoop(ctx,
			RunLoopOptions{MinimumProbeCadence: signal.Cadence()},
			func(_ context.Context, current Signal, execution SignalExecution) error {
				notifier.Handle(current, execution)
				handled <- execution
				return nil
			},
			func(cadence time.Duration) runLoopTicker {
				tick := cadenceFloorTick{cadence: cadence, ticks: make(chan time.Time, 1)}
				ticks <- tick
				return &manualRunLoopTicker{c: tick.ticks}
			})
	}()
	t.Cleanup(func() {
		cancel()
		if err := receiveCadenceFloorValue(t, finished); err != nil {
			t.Error("DNS recovery execution loop failed to join")
		}
	})
	return func(next int32) SignalExecution {
		t.Helper()
		tick := receiveCadenceFloorValue(t, ticks)
		if tick.cadence != signal.Cadence() {
			t.Fatal("DNS recovery control changed the five-minute native cadence")
		}
		phase.Store(next)
		tick.ticks <- time.Time{}
		return receiveCadenceFloorValue(t, handled)
	}
}

// Snapshot the real notifier state after synchronous Handle has returned.
func dnsRecoveryIncident(notifier *SlackAlertNotifier) (bool, int, Severity) {
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	incident := notifier.keyIncidents["synthetic/dns-aliases|dns-recursive-answer-family|alt.example.test"]
	if incident == nil {
		return false, 0, ""
	}
	return true, incident.signalKeyAbsentRuns["dns-aliases"], incident.severity
}

// Causal control: first establish the original sustained incident, then two
// complete-but-wrong authority runs. The old adapter-filtered healthy marker
// grants two absence credits and deletes this different-class incident.
func TestDNSAliasesBrokenAuthorityCannotClearRecursiveIncident(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	var phase atomic.Int32
	advance := dnsRecoveryLoop(t, &phase, dnsRecoverySource(&phase), notifier)
	first := advance(0)
	if first.Failed || len(first.Alerts) != 0 || len(first.HeldAlerts) != 1 || first.HeldAlerts[0].Class != "dns-recursive-answer-family" {
		t.Fatal("original recursive incident did not begin below its real sustain gate")
	}
	second := advance(0)
	if len(second.Alerts) != 1 || second.Alerts[0].Class != "dns-recursive-answer-family" || second.Alerts[0].Severity != SeverityWarn {
		t.Fatal("second original observation did not raise the recursive WARN")
	}
	third := advance(0)
	if len(third.Alerts) != 1 || third.Alerts[0].Severity != SeverityPage {
		t.Fatal("third original observation did not retain the existing PAGE promotion")
	}
	if present, absent, severity := dnsRecoveryIncident(notifier); !present || absent != 0 || severity != SeverityPage {
		t.Fatal("original recursive PAGE incident was not established before the discriminator")
	}
	unknownFirst, unknownSecond := advance(1), advance(1)
	// Inspect after both executions, so baseline failure proves the real
	// unsupported deletion rather than only a missing internal marker.
	if present, absent, severity := dnsRecoveryIncident(notifier); !present || absent != 0 || severity != SeverityPage {
		t.Fatal("complete wrong authority falsely cleared or advanced absence for the established recursive incident")
	}
	for _, execution := range []SignalExecution{unknownFirst, unknownSecond} {
		if execution.Failed {
			t.Fatal("complete authoritative disagreement became a source failure")
		}
		authority := requireAlertClass(t, execution.Alerts, "dns-authoritative-rrset")
		if authority.Severity != SeverityPage {
			t.Fatal("independently valid authoritative failure lost PAGE")
		}
		for _, alert := range append(append(Alerts(nil), execution.Alerts...), execution.HeldAlerts...) {
			if alert.Class == "dns-recursive-answer-family" {
				t.Fatal("non-exact authority duplicated downstream drift")
			}
		}
	}
	if len(unknownFirst.HeldAlerts) != 1 || unknownFirst.HeldAlerts[0].Class != "dns-recursive-unobservable" || !isVisibilityAlert(unknownFirst.HeldAlerts[0]) {
		t.Fatal("first non-exact authority lost held recursive visibility")
	}
	visibility := requireAlertClass(t, unknownSecond.Alerts, "dns-recursive-unobservable")
	if !isVisibilityAlert(visibility) || visibility.Severity != SeverityWarn || visibility.Sustain != 2 {
		t.Fatal("second non-exact authority lost ready visibility or changed sustain")
	}
	if execution := advance(3); execution.Failed || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 {
		t.Fatal("exact authority and corrected recursive response did not establish a complete healthy comparison")
	}
	if present, absent, _ := dnsRecoveryIncident(notifier); !present || absent != 1 {
		t.Fatal("one truly healthy comparison cleared too early or lacked complete absence credit")
	}
	advance(3)
	if present, _, _ := dnsRecoveryIncident(notifier); present {
		t.Fatal("two exact healthy comparisons failed to clear the original recursive incident")
	}
	closeSlackNotifier(t, notifier)
	raised, escalated, cleared := 0, 0, 0
	for _, text := range webhook.posted() {
		if !strings.Contains(text, "dns-recursive-answer-family") {
			continue
		}
		if strings.HasPrefix(text, "[WARN] raised") {
			raised++
		}
		if strings.HasPrefix(text, "[PAGE] escalated") {
			escalated++
		}
		if strings.HasPrefix(text, "[CLEARED] PAGE") {
			cleared++
		}
	}
	if raised != 1 || escalated != 1 || cleared != 1 {
		t.Fatal("composed notifier did not preserve one original raise/escalation and one qualified healthy clear")
	}
}

func TestDNSAliasesMissingAuthorityCannotClearRecursiveIncident(t *testing.T) {
	_, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	var phase atomic.Int32
	advance := dnsRecoveryLoop(t, &phase, dnsRecoverySource(&phase), notifier)
	advance(0)
	advance(0)
	if present, absent, _ := dnsRecoveryIncident(notifier); !present || absent != 0 {
		t.Fatal("original recursive incident was not raised before missing authority")
	}
	for range 2 {
		execution := advance(2)
		if execution.Failed {
			t.Fatal("per-alias authority loss discarded independent signal results")
		}
		if present, absent, _ := dnsRecoveryIncident(notifier); !present || absent != 0 {
			t.Fatal("missing authority granted recursive absence credit")
		}
	}
	advance(3)
	if present, absent, _ := dnsRecoveryIncident(notifier); !present || absent != 1 {
		t.Fatal("first exact healthy comparison did not retain the incident")
	}
	advance(3)
	if present, _, _ := dnsRecoveryIncident(notifier); present {
		t.Fatal("corrected authority and recursive result did not permit two-run recovery")
	}
}

func TestDNSAliasesAuthorityVisibilityPreservesPartialPositiveAndPrivacy(t *testing.T) {
	var phase atomic.Int32
	phase.Store(4)
	source := dnsRecoverySource(&phase)
	alerts, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal("partial authority could not preserve its independent results")
	}
	if len(alerts) != 3 {
		t.Fatal("partial wrong authority must retain authoritative PAGE and both unknown boundaries")
	}
	if requireAlertClass(t, alerts, "dns-authoritative-rrset").Severity != SeverityPage {
		t.Fatal("partial authority erased a concrete wrong RRset")
	}
	requireAlertClass(t, alerts, "dns-authoritative-unobservable")
	recursive := requireAlertClass(t, alerts, "dns-recursive-unobservable")
	if !isVisibilityAlert(recursive) || !strings.Contains(recursive.Observed, "authoritative_state_exact=false recursive_response_observed=true") {
		t.Fatal("readable recursive answer acquired unsupported authority")
	}
	phase.Store(2)
	failed, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal("missing authority became a whole-signal error")
	}
	for _, alert := range append(alerts, failed...) {
		requireAlertOmits(t, alert, "203.0.113.90", "203.0.113.91", "synthetic-private-authority-detail")
	}
}

// Cancellation joins admitted sources before returning and supplies neither a
// partial visibility/failure set nor a public healthy result.
func TestDNSAliasesCanceledAuthorityPublishesNoRecovery(t *testing.T) {
	started := make(chan struct{}, 1)
	var active atomic.Int32
	source := healthyDNSAliasSource()
	source.authoritativeFn = func(ctx context.Context, _, _ string, _ DNSRecordType) (DNSAuthoritativeObservation, error) {
		active.Add(1)
		defer active.Add(-1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return DNSAuthoritativeObservation{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		alerts Alerts
		err    error
	}
	returned := make(chan outcome, 1)
	go func() {
		alerts, err := NewDNSAliasesSignal().Run(ctx, dnsAliasFixtureSettings(source))
		returned <- outcome{alerts: alerts, err: err}
	}()
	receiveCadenceFloorValue(t, started)
	cancel()
	result := receiveCadenceFloorValue(t, returned)
	if !errors.Is(result.err, context.Canceled) || len(result.alerts) != 0 || active.Load() != 0 {
		t.Fatal("canceled source published findings or outlived its parent observation")
	}
}
