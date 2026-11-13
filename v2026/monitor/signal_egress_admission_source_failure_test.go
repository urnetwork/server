package monitor

// The source boundary must retain fixed diagnostics without changing the
// egress warning, native pair authority or existing gateway fallback.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

type egressAdmissionSourceReply struct {
	body string
	err  error
}

// Every configured gateway has exactly one owned synthetic reply. The real
// adapter and first-success helper run, while no command reaches a host.
func runEgressAdmissionSourceReplies(t *testing.T, signal Signal, now time.Time, replies ...egressAdmissionSourceReply) Alerts {
	t.Helper()
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		decoded, err := url.QueryUnescape(command)
		if err != nil {
			t.Fatal("synthetic egress command could not be decoded")
		}
		for _, want := range []string{"curl -fsS", "--max-time 15", "--max-filesize 2097152", egressAdmissionQuery("synthetic")} {
			if !strings.Contains(decoded, want) {
				t.Fatal("egress source route, exact query or existing limit changed")
			}
		}
		if calls >= len(replies) {
			t.Fatal("egress source made an unowned extra request")
		}
		reply := replies[calls]
		calls++
		return reply.body, reply.err
	}}
	settings := syntheticSettings(source)
	settings.Hosts = nil
	for i := range replies {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: fmt.Sprintf("synthetic-source-%d.example", i), Roles: []string{"services"}})
	}
	settings.Now = func() time.Time { return now }
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || calls != len(replies) {
		t.Fatal("synthetic source did not complete the owned gateway sequence")
	}
	for _, alert := range alerts {
		for _, configured := range settings.Hosts {
			requireAlertOmits(t, alert, configured.Name)
		}
	}
	return alerts
}

// These are additive details on the existing unknown finding, never a new
// transport incident or proof that a request reached a particular backend.
func requireEgressAdmissionSourceDetails(t *testing.T, alerts Alerts, name, details string) Alert {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatal("source failure changed the existing single unknown warning")
	}
	alert := requireAlertClass(t, alerts, "egress-admission-unobservable")
	if alert.SignalID != "runtime/egress-admission" || alert.Severity != SeverityWarn || alert.Target != "egress-fleet" || alert.Sustain != 1 {
		t.Fatal("source diagnostics changed the owning warning policy")
	}
	want := "reason=bounded-source-unavailable complete_process_intervals=0 incomplete_process_intervals=0 " + details
	if alert.Observed != want {
		t.Fatalf("causal assertion: bounded egress source failure erased its available fixed details (%s)", name)
	}
	return alert
}

func TestEgressAdmissionSourceFailurePreservesTypedAdmissionAndLifecycle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	private := errors.New("synthetic-private-source-cause")
	for _, test := range []struct {
		name    string
		err     error
		details string
	}{
		{name: "acquire", err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}, details: "error_class=observation-admission-unavailable ssh_admission_failures=acquire"},
		{name: "release", err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease}, details: "error_class=observation-admission-unavailable ssh_admission_failures=release"},
		{name: "legacy-phase", err: &sshAdmissionUnavailableError{err: private}, details: "error_class=observation-admission-unavailable ssh_admission_failures=unknown"},
		{name: "deadline-and-acquire", err: errors.Join(context.DeadlineExceeded, &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}), details: "error_class=observation-timeout ssh_admission_failures=acquire"},
		{name: "cancellation-and-release", err: errors.Join(context.Canceled, &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease}), details: "error_class=observation-canceled ssh_admission_failures=release"},
	} {
		alerts := runEgressAdmissionSourceReplies(t, NewEgressAdmissionSignal(), now, egressAdmissionSourceReply{err: fmt.Errorf("synthetic-private-wrapper: %w", test.err)})
		alert := requireEgressAdmissionSourceDetails(t, alerts, test.name, test.details)
		requireAlertOmits(t, alert, "synthetic-private-source-cause", "synthetic-private-wrapper")
	}
}

func TestEgressAdmissionSourceFailureRetainsJoinedGatewayPhases(t *testing.T) {
	private := errors.New("synthetic-private-gateway-cause")
	alerts := runEgressAdmissionSourceReplies(t, NewEgressAdmissionSignal(), time.Unix(1_700_000_000, 0).UTC(),
		egressAdmissionSourceReply{err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}},
		egressAdmissionSourceReply{err: &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureRelease}},
	)
	alert := requireEgressAdmissionSourceDetails(t, alerts, "joined-gateways", "error_class=observation-admission-unavailable ssh_admission_failures=acquire,release")
	requireAlertOmits(t, alert, "synthetic-private-gateway-cause")
}

func TestEgressAdmissionSourceFailurePreservesNativeSSHStatusAuthority(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	exit2 := syntheticProcessExit(t, 2)
	for _, test := range []struct {
		name  string
		err   error
		class string
	}{
		{name: "native-ssh-255", err: &sshCommandError{err: exit255}, class: "observation-ssh-exit-255"},
		{name: "native-ssh-other", err: &sshCommandError{err: exit2}, class: "observation-command-failed"},
		{name: "native-local-255", err: exit255, class: "observation-command-failed"},
		{name: "status-text-only", err: errors.New("exit status 255"), class: "observation-command-failed"},
		{name: "unknown-source", err: errors.New("synthetic-private-unclassified"), class: "observation-unclassified"},
	} {
		alerts := runEgressAdmissionSourceReplies(t, NewEgressAdmissionSignal(), time.Unix(1_700_000_000, 0).UTC(), egressAdmissionSourceReply{err: fmt.Errorf("synthetic-private-native-wrapper: %w", test.err)})
		alert := requireEgressAdmissionSourceDetails(t, alerts, test.name, "error_class="+test.class)
		requireAlertOmits(t, alert, "synthetic-private-native-wrapper", "synthetic-private-unclassified")
	}
}

func TestEgressAdmissionSourceFailureDoesNotRenderPrivateErrorOrBody(t *testing.T) {
	canaries := []string{
		"password=synthetic-source-password",
		"https://synthetic-private.example/query?token=synthetic-source-token",
		"SELECT synthetic_private_column FROM synthetic_private_table",
		`{tenant="synthetic-private-tenant",stream="synthetic-private-stream"}`,
	}
	private := strings.Join(canaries, " ")
	wrapped := errors.Join(context.DeadlineExceeded, &sshAdmissionUnavailableError{err: errors.New(private), phase: sshAdmissionFailureAcquire})
	alerts := runEgressAdmissionSourceReplies(t, NewEgressAdmissionSignal(), time.Unix(1_700_000_000, 0).UTC(), egressAdmissionSourceReply{body: private, err: fmt.Errorf("%s: %w", private, wrapped)})
	alert := requireEgressAdmissionSourceDetails(t, alerts, "private-error-and-body", "error_class=observation-timeout ssh_admission_failures=acquire")
	requireAlertOmits(t, alert, canaries...)
}

func TestEgressAdmissionSourceFailureDistinguishesSuccessfulBodyOverflow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, egressAdmissionTestProcesses(now)))
	var response map[string]any
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal("synthetic complete pair was invalid")
	}
	response["synthetic_ignored_field"] = strings.Repeat("x", egressAdmissionResponseMax)
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) <= egressAdmissionResponseMax || !json.Valid(encoded) {
		t.Fatal("overflow fixture was not a valid oversized native pair")
	}
	for _, test := range []struct {
		name    string
		err     error
		details string
	}{
		{name: "successful-body-overflow", details: "error_class=observation-bound-exceeded"},
		{name: "failed-source-with-partial-body", err: context.DeadlineExceeded, details: "error_class=observation-timeout"},
	} {
		alerts := runEgressAdmissionSourceReplies(t, NewEgressAdmissionSignal(), now, egressAdmissionSourceReply{body: string(encoded), err: test.err})
		requireEgressAdmissionSourceDetails(t, alerts, test.name, test.details)
	}
}

func TestEgressAdmissionSourceDiagnosticsKeepHealthyAndParserControls(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
	warmup := requireAlertClass(t, runEgressAdmissionSourceReplies(t, signal, now, egressAdmissionSourceReply{body: body}), "egress-admission-unobservable")
	if strings.Contains(warmup.Observed, "error_class=") {
		t.Fatal("observer warmup became a source transport failure")
	}
	now = now.Add(15 * time.Minute)
	body = egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
	if alerts := runEgressAdmissionSourceReplies(t, signal, now, egressAdmissionSourceReply{body: body}); len(alerts) != 0 {
		t.Fatal("fixed source diagnostics changed the healthy native-pair control")
	}
	for _, invalid := range []string{"", `{"status":"error"}`, `{"status":"success","warnings":["synthetic-private-warning"],"data":{"resultType":"vector","result":[]}}`} {
		alerts := runEgressAdmissionSourceReplies(t, signal, now.Add(time.Minute), egressAdmissionSourceReply{body: invalid})
		alert := requireAlertClass(t, alerts, "egress-admission-unobservable")
		if len(alerts) != 1 || alert.Observed != "reason=invalid-source-response complete_process_intervals=0 incomplete_process_intervals=0" {
			t.Fatal("successful malformed body acquired a transport diagnosis")
		}
		requireAlertOmits(t, alert, "synthetic-private-warning")
	}
}

func TestEgressAdmissionSourceDiagnosticsPreserveFirstHealthyGateway(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	for i := 0; i < 2; i++ {
		body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
		alerts := runEgressAdmissionSourceReplies(t, signal, now,
			egressAdmissionSourceReply{err: context.DeadlineExceeded},
			egressAdmissionSourceReply{body: body},
		)
		if i == 0 {
			warmup := requireAlertClass(t, alerts, "egress-admission-unobservable")
			if strings.Contains(warmup.Observed, "error_class=") {
				t.Fatal("successful gateway inherited a failed sibling's source diagnosis")
			}
		} else if len(alerts) != 0 {
			t.Fatal("existing first-success gateway fallback stopped supplying healthy native pairs")
		}
		now = now.Add(15 * time.Minute)
	}
}

func TestEgressAdmissionSourceFailureCannotSupplyObserverWarmup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	requireAlertClass(t, runEgressAdmissionSourceReplies(t, signal, now, egressAdmissionSourceReply{err: context.DeadlineExceeded}), "egress-admission-unobservable")
	now = now.Add(15 * time.Minute)
	body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
	warmup := requireAlertClass(t, runEgressAdmissionSourceReplies(t, signal, now, egressAdmissionSourceReply{body: body}), "egress-admission-unobservable")
	if !strings.Contains(warmup.Observed, "reason=incomplete-mixed-reset-or-warming") || strings.Contains(warmup.Observed, "error_class=") {
		t.Fatal("failed source supplied native history or changed first-success warmup")
	}
	now = now.Add(15 * time.Minute)
	body = egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
	if alerts := runEgressAdmissionSourceReplies(t, signal, now, egressAdmissionSourceReply{body: body}); len(alerts) != 0 {
		t.Fatal("complete advancing native pairs could not finish ordinary warmup")
	}
}
