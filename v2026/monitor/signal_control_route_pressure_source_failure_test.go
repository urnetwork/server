package monitor

// Source diagnostics must preserve the owning context guard and the existing
// pressure, healthy and unavailable verdicts without another request.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type controlRouteSourceReply struct {
	body         string
	err          error
	beforeReturn func()
}

// The real adapter and first-success helper consume only these owned replies.
// A later unused reply proves that success or owner termination stops failover.
func runControlRouteSourceReplies(t *testing.T, ctx context.Context, now time.Time, wantCalls int, replies ...controlRouteSourceReply) (Alerts, error) {
	t.Helper()
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		for _, want := range []string{
			"curl -fsS", "--max-time 15", "--max-filesize 2097152",
			"--data-urlencode " + shellSingleQuote("query="+controlRoutePressureQuery("synthetic")),
		} {
			if !strings.Contains(command, want) {
				t.Fatal("control-route source query, route or existing limits changed")
			}
		}
		if calls >= len(replies) {
			t.Fatal("control-route source made an unowned extra request")
		}
		reply := replies[calls]
		calls++
		if reply.beforeReturn != nil {
			reply.beforeReturn()
		}
		return reply.body, reply.err
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = nil
	for i := range replies {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: fmt.Sprintf("synthetic-control-source-%d.invalid", i), Roles: []string{"services"}})
	}
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-a.invalid"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"lane-a"}}
	alerts, err := NewControlRoutePressureSignal().Run(ctx, settings)
	if calls != wantCalls {
		t.Fatal("control-route source did not preserve the owned gateway sequence")
	}
	for _, alert := range alerts {
		for _, configured := range settings.Hosts {
			requireAlertOmits(t, alert, configured.Name)
		}
	}
	return alerts, err
}

// The same coverage warning gains finite details; it does not become pressure
// or proof that the request reached a particular host or backend.
func requireControlRouteSourceDetails(t *testing.T, alerts Alerts, err error, name, details string) Alert {
	t.Helper()
	if err != nil || len(alerts) != 1 {
		t.Fatal("source failure changed the existing single coverage warning")
	}
	alert := requireAlertClass(t, alerts, "control-route-pressure-unobservable")
	if alert.SignalID != "runtime/control-route-pressure" || alert.Target != "api-fleet" || alert.Frame != "coverage" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
		t.Fatal("source diagnostics changed the control-route warning policy")
	}
	if alert.Observed != "Bounded Mimir query unavailable "+details {
		t.Fatalf("causal assertion: control-route source failure erased its available fixed details (%s)", name)
	}
	return alert
}

func TestControlRoutePressureSourceFailurePreservesTypedAdmissionAndLifecycle(t *testing.T) {
	private := errors.New("synthetic-private-control-source-cause")
	for _, test := range []struct {
		name    string
		err     error
		details string
	}{
		{name: "acquire", err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}, details: "error_class=observation-admission-unavailable ssh_admission_failures=acquire"},
		{name: "release", err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease}, details: "error_class=observation-admission-unavailable ssh_admission_failures=release"},
		{name: "legacy-phase", err: &sshAdmissionUnavailableError{err: private}, details: "error_class=observation-admission-unavailable ssh_admission_failures=unknown"},
		{name: "deadline-with-live-owner", err: errors.Join(context.DeadlineExceeded, &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}), details: "error_class=observation-timeout ssh_admission_failures=acquire"},
		{name: "cancellation-with-live-owner", err: errors.Join(context.Canceled, &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease}), details: "error_class=observation-canceled ssh_admission_failures=release"},
	} {
		alerts, err := runControlRouteSourceReplies(t, context.Background(), time.Unix(1_700_000_000, 0).UTC(), 1,
			controlRouteSourceReply{err: fmt.Errorf("synthetic-private-control-wrapper: %w", test.err)})
		alert := requireControlRouteSourceDetails(t, alerts, err, test.name, test.details)
		requireAlertOmits(t, alert, "synthetic-private-control-source-cause", "synthetic-private-control-wrapper")
	}
}

func TestControlRoutePressureSourceFailureRetainsJoinedGatewayPhases(t *testing.T) {
	private := errors.New("synthetic-private-control-gateway-cause")
	alerts, err := runControlRouteSourceReplies(t, context.Background(), time.Unix(1_700_000_000, 0).UTC(), 2,
		controlRouteSourceReply{err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}},
		controlRouteSourceReply{err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease}},
	)
	alert := requireControlRouteSourceDetails(t, alerts, err, "joined-gateways", "error_class=observation-admission-unavailable ssh_admission_failures=acquire,release")
	requireAlertOmits(t, alert, "synthetic-private-control-gateway-cause")
}

func TestControlRoutePressureSourceFailurePreservesNativeSSHStatusAuthority(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	exit2 := syntheticProcessExit(t, 2)
	for _, test := range []struct {
		name  string
		err   error
		class string
	}{
		{name: "native-ssh-255", err: &sshCommandError{err: exit255}, class: "observation-ssh-exit-255"},
		{name: "native-ssh-connect-timeout", err: &sshCommandError{err: exit255, connectTimeoutDiagnostic: true}, class: "observation-timeout"},
		{name: "native-ssh-other", err: &sshCommandError{err: exit2}, class: "observation-command-failed"},
		{name: "native-local-255", err: exit255, class: "observation-command-failed"},
		{name: "status-text-only", err: errors.New("exit status 255"), class: "observation-command-failed"},
		{name: "unknown-source", err: errors.New("synthetic-private-control-unclassified"), class: "observation-unclassified"},
	} {
		alerts, err := runControlRouteSourceReplies(t, context.Background(), time.Unix(1_700_000_000, 0).UTC(), 1,
			controlRouteSourceReply{err: fmt.Errorf("synthetic-private-native-wrapper: %w", test.err)})
		alert := requireControlRouteSourceDetails(t, alerts, err, test.name, "error_class="+test.class)
		requireAlertOmits(t, alert, "synthetic-private-native-wrapper", "synthetic-private-control-unclassified")
	}
}

func TestControlRoutePressureSourceFailureDoesNotRenderPrivateErrorOrBody(t *testing.T) {
	canaries := []string{
		"password=synthetic-control-password",
		"token=synthetic-control-private-token",
		"statement=synthetic-private-statement",
		`{tenant="synthetic-control-tenant",stream="synthetic-control-stream"}`,
	}
	private := strings.Join(canaries, " ")
	wrapped := errors.Join(context.DeadlineExceeded, &sshAdmissionUnavailableError{err: errors.New(private), phase: sshAdmissionFailureAcquire})
	alerts, err := runControlRouteSourceReplies(t, context.Background(), time.Unix(1_700_000_000, 0).UTC(), 1,
		controlRouteSourceReply{body: private, err: fmt.Errorf("%s: %w", private, wrapped)})
	alert := requireControlRouteSourceDetails(t, alerts, err, "private-error-and-body", "error_class=observation-timeout ssh_admission_failures=acquire")
	requireAlertOmits(t, alert, canaries...)
}

func TestControlRoutePressureSourceDiagnosticsPreserveOwnerTermination(t *testing.T) {
	for _, expected := range []error{context.Canceled, context.DeadlineExceeded} {
		ctx, cancel := context.WithCancel(context.Background())
		if expected == context.DeadlineExceeded {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
		}
		t.Cleanup(cancel)
		alerts, err := runControlRouteSourceReplies(t, ctx, time.Unix(1_700_000_000, 0).UTC(), 1,
			controlRouteSourceReply{beforeReturn: cancel, err: &sshAdmissionUnavailableError{err: errors.New("synthetic-private-owner-failure"), phase: sshAdmissionFailureRelease}},
			controlRouteSourceReply{err: errors.New("synthetic-unreached-gateway")},
		)
		cancel()
		if err != expected || len(alerts) != 0 {
			t.Fatal("terminated owner became a coverage warning or lost its exact context error")
		}
	}
}

func TestControlRoutePressureSourceDiagnosticsKeepPressureAndHealthyGatewayControls(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, test := range []struct {
		name     string
		values   map[string]float64
		severity Severity
		sustain  int
	}{
		{name: "sustained-hot", severity: SeverityPage, sustain: 2},
		{name: "hot-without-sustained-inflight", values: map[string]float64{"inflight_min": 99}, severity: SeverityWarn, sustain: 1},
		{name: "healthy", values: map[string]float64{"canceled": 8, "seconds": 2200}},
		{name: "quiet", values: map[string]float64{"count": 0, "requests": 0, "canceled": 0, "seconds": 0, "inflight": 0, "inflight_min": 0}},
	} {
		process := controlRouteFixture(now, "synthetic-private-control-process")
		for name, value := range test.values {
			process.values[name] = value
		}
		if test.name == "quiet" {
			for name := range process.values {
				if strings.HasPrefix(name, "phase:") || strings.HasPrefix(name, "frame:") {
					process.values[name] = 0
				}
			}
		}
		alerts, err := runControlRouteSourceReplies(t, context.Background(), now, 2,
			controlRouteSourceReply{err: context.DeadlineExceeded},
			controlRouteSourceReply{body: controlRouteFixtureJson(t, now, process)},
			controlRouteSourceReply{err: errors.New("synthetic-unreached-after-success")},
		)
		if err != nil {
			t.Fatal("successful control-route gateway inherited a prior error")
		}
		if test.severity == "" {
			if len(alerts) != 0 {
				t.Fatalf("complete %s control acquired source or pressure failure", test.name)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "control-route-pressure")
		if len(alerts) != 1 || alert.Severity != test.severity || alert.Sustain != test.sustain {
			t.Fatalf("source diagnostics changed the %s pressure policy", test.name)
		}
		requireAlertOmits(t, alert, "error_class=", "ssh_admission_failures=", process.instance)
	}
}

func TestControlRoutePressureSourceDiagnosticsKeepPreSourceAndParserUnknowns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, missingInventory := range []bool{true, false} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatal("pre-source coverage gap issued a query")
			return "", nil
		}}
		settings := syntheticSettings(source)
		settings.Now = func() time.Time { return now }
		settings.Hosts = nil
		settings.LogServices = []string{"api"}
		settings.LogServiceBlocks = map[string][]string{"api": {"lane-a"}}
		settings.LogServiceHosts = nil
		want := "API host/block inventory unavailable"
		if !missingInventory {
			settings.LogServiceHosts = map[string][]string{"api": {"api-a.invalid"}}
			want = "No configured services gateway for Mimir"
		}
		alerts, err := NewControlRoutePressureSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("pre-source coverage gap changed its warning contract")
		}
		alert := requireAlertClass(t, alerts, "control-route-pressure-unobservable")
		if alert.Observed != want {
			t.Fatal("pre-source coverage gap acquired a transport diagnosis")
		}
	}
	var envelope map[string]any
	if json.Unmarshal([]byte(controlRouteFixtureJson(t, now, controlRouteFixture(now, "synthetic-private-overflow-process"))), &envelope) != nil {
		t.Fatal("synthetic complete pressure fixture was invalid")
	}
	envelope["synthetic_ignored_field"] = strings.Repeat("x", controlRouteResponseMax)
	oversized, err := json.Marshal(envelope)
	if err != nil || len(oversized) <= controlRouteResponseMax || !json.Valid(oversized) {
		t.Fatal("control-route overflow fixture was not a complete oversized response")
	}
	for _, body := range []string{
		"synthetic-invalid-body",
		`{"status":"success","warnings":["synthetic-private-warning"],"data":{"resultType":"vector","result":[]}}`,
		string(oversized),
	} {
		alerts, err := runControlRouteSourceReplies(t, context.Background(), now, 1, controlRouteSourceReply{body: body})
		if err != nil || len(alerts) != 1 {
			t.Fatal("successful invalid source changed its coverage warning")
		}
		alert := requireAlertClass(t, alerts, "control-route-pressure-unobservable")
		if alert.Observed != "Invalid or incomplete Mimir response" {
			t.Fatal("successful malformed or oversized body acquired a command-failure diagnosis")
		}
		requireAlertOmits(t, alert, "synthetic-private-warning", "synthetic-private-overflow-process")
	}
}
