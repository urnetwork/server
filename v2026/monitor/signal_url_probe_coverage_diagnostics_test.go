// The owning URL warning retains finite source failures without acquiring
// provider measurements, exposing private errors or adding gateway attempts.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Each synthetic gateway supplies one reply to the real first-success helper.
type urlProbeCoverageSourceReply struct {
	body string
	err  error
}

// Exercise the public adapter and continuous observation together; no source
// callback can contact a host or manufacture another request.
func runUrlProbeCoverageSourceReplies(t *testing.T, wantCalls int, replies ...urlProbeCoverageSourceReply) (Alerts, UrlProbeCoverageObservation) {
	t.Helper()
	now := time.Date(2026, 10, 21, 2, 7, 11, 0, time.UTC)
	calls := 0
	settings := syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, "--max-time 15 --max-filesize 2097152") ||
			!strings.Contains(command, shellSingleQuote("query="+urlProbeCoverageQuery("synthetic"))) {
			t.Fatal("URL coverage changed the existing query or bounds")
		}
		if calls >= len(replies) {
			t.Fatal("URL coverage added an unowned request")
		}
		reply := replies[calls]
		calls++
		return reply.body, reply.err
	}})
	settings.Now = func() time.Time { return now }
	settings.Hosts = nil
	for i := range replies {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: fmt.Sprintf("private-url-gateway-%d.invalid", i), Roles: []string{"services"}})
	}
	settings.LogServices = []string{"taskworker"}
	settings.LogServiceHosts = map[string][]string{"taskworker": {"worker-a.example", "worker-b.example"}}
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"worker-group"}}
	var observation UrlProbeCoverageObservation
	records := 0
	settings.UrlProbeCoverageObserver = func(record UrlProbeCoverageObservation) error {
		records++
		observation = record
		return nil
	}
	signal := NewUrlProbeCoverageSignal().(*signalAdapter)
	signal.probe = urlProbeCoverageProbe{loadDesired: func() (urlProbeCoverageDesired, error) {
		return urlProbeCoverageDesired{enabled: true, shardCount: 2}, nil
	}}
	alerts, err := NewWithSignals(settings, signal).Run(context.Background())
	if err != nil || calls != wantCalls || records != 1 {
		t.Fatal("URL source diagnostics changed request or observation ownership")
	}
	for _, alert := range alerts {
		for _, gateway := range settings.Hosts {
			requireAlertOmits(t, alert, gateway.Name)
		}
	}
	return alerts, observation
}

// Typed local proofs remain finite; neither partial responses nor private
// errors can turn a failed query into provider numbers or remote attribution.
func TestUrlProbeCoverageSourceDiagnosticsPreserveUnavailableAndPrivacy(t *testing.T) {
	private := "password=private-url-password token=private-url-token SELECT private_customer FROM private_relation"
	cause := errors.New(private)
	started := &observationBoundaryError{err: &sshCommandError{err: syntheticProcessExit(t, 255)}, boundary: observationBoundaryNativeWait}
	precontact := &observationBoundaryError{err: &sshAdmissionUnavailableError{err: cause, phase: sshAdmissionFailureAcquire}, boundary: observationBoundaryHost}
	for _, test := range []struct {
		name    string
		replies []urlProbeCoverageSourceReply
		details string
	}{
		{name: "precontact", replies: []urlProbeCoverageSourceReply{{err: precontact}}, details: "error_class=observation-admission-unavailable ssh_admission_failures=acquire execution_boundaries=host-admission/not-attempted"},
		{name: "started-transport", replies: []urlProbeCoverageSourceReply{{err: started}}, details: "error_class=observation-ssh-exit-255 execution_boundaries=native-wait/started-joined"},
		{name: "unknown-boundary", replies: []urlProbeCoverageSourceReply{{err: &observationBoundaryError{err: cause, boundary: observationBoundaryUnknown}}}, details: "error_class=observation-unclassified execution_boundaries=unknown/unknown"},
		{name: "unknown-partial-private", replies: []urlProbeCoverageSourceReply{{body: private, err: cause}}, details: "error_class=observation-unclassified"},
		{name: "joined-gateways", replies: []urlProbeCoverageSourceReply{{err: precontact}, {err: started}}, details: "error_class=observation-admission-unavailable ssh_admission_failures=acquire execution_boundaries=host-admission/not-attempted,native-wait/started-joined"},
		{name: "timeout-partial", replies: []urlProbeCoverageSourceReply{{body: private, err: fmt.Errorf("%s: %w", private, context.DeadlineExceeded)}}, details: "error_class=observation-timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			alerts, record := runUrlProbeCoverageSourceReplies(t, len(test.replies), test.replies...)
			if len(alerts) != 1 {
				t.Fatal("source failure changed the single owning warning")
			}
			alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
			if alert.Observed != "bounded Mimir observation unavailable "+test.details || alert.SignalID != "runtime/url-probe-coverage" ||
				alert.Target != "provider-url-fleet" || alert.Frame != "rolling-four-hours" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
				t.Fatalf("URL source diagnostic or warning policy lost: %s", alert.Observed)
			}
			requireAlertOmits(t, alert, private, "private-url-password", "private-url-token", "private_customer", "remote-contact")
			encoded, err := json.Marshal(record)
			if err != nil || record.CensusReason != "transport_unavailable" || record.MatureCohortReason != "census_unavailable" ||
				record.SourceCoverageComplete || record.WholeFleetAgeKnown || record.SourceObservedSeconds != nil || record.SourceSampleSeconds != nil ||
				record.AllCurrent != nil || record.Mature != nil || record.Warming != nil || record.AgeUnknown != nil || record.KnownMatureQuotaPercent != nil ||
				record.SecureComplete != nil || record.SecurityPending != nil || record.SecurityUnknownTargets != nil || len(record.Gaps) != 0 || strings.Contains(string(encoded), private) {
				t.Fatal("source diagnostic changed unavailable census semantics or leaked data")
			}
		})
	}
}

// A successful fallback uses only its accepted census; earlier failed gateway
// details cannot contaminate healthy output or trigger another request.
func TestUrlProbeCoverageSourceDiagnosticsHealthyFallbackStops(t *testing.T) {
	now := time.Date(2026, 10, 21, 2, 7, 11, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	setUrlProbeAdmissionCohortFixture(first, [3][3]float64{{100, 100, 0}, {30, 2, 140}, {0, 0, 0}})
	valid := urlProbeCoverageFixtureJson(t, now, first, second)
	for _, replies := range [][]urlProbeCoverageSourceReply{
		{{body: valid}, {err: errors.New("unused-private-url-source")}},
		{{err: errors.New("failed-private-url-source")}, {body: valid}, {err: errors.New("unused-private-url-source")}},
	} {
		alerts, record := runUrlProbeCoverageSourceReplies(t, len(replies)-1, replies...)
		if len(alerts) != 0 || record.CensusReason != "ok" || !record.SourceCoverageComplete || record.Mature == nil ||
			record.KnownMatureQuotaPercent == nil || *record.KnownMatureQuotaPercent != 100 {
			t.Fatal("source diagnostics contaminated healthy first-success census")
		}
	}
}
