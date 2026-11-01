// Returned capability sources remain privately inspectable after owner
// termination, while retired findings and private source text stay unpublished.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Preserve the actual source context at the public adapter's synthetic seam.
type capabilityOwnerSource struct {
	*syntheticSource
	host func(context.Context, HostSettings, string) (string, error)
}

// No process or network is involved in these deterministic source returns.
func (self *capabilityOwnerSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.host(ctx, host, command)
}

// An unrelated disabled host selects the scoped adapter without blocking the
// synthetic metrics gateways or changing the capability signal's identity.
func capabilityOwnerSettings(source SignalSource, scoped bool, hosts int) SignalSettings {
	settings := syntheticSettings(source)
	for index := 0; index < hosts; index++ {
		settings.Hosts = append(settings.Hosts, HostSettings{
			Name: fmt.Sprintf("capability-metrics-%d.example", index), Roles: []string{"services"},
		})
	}
	if scoped {
		settings.disabledHosts = []HostSettings{{Name: "excluded-capability.example"}}
	}
	return settings
}

// Check both private cause reachability and the existing finite public
// projection; a local execution proof is not a remote completion claim.
func requireCapabilityOwnerAuthority(t *testing.T, signal Signal, settings SignalSettings, err error, parentErr error, sourceErrors []error, boundary, admissionPhase string) {
	t.Helper()
	if err == nil || err.Error() != parentErr.Error() || !errors.Is(err, parentErr) {
		t.Fatal("capability interruption lost its parent verdict or fixed error text")
	}
	for _, sourceErr := range sourceErrors {
		if sourceErr != nil && !errors.Is(err, sourceErr) {
			t.Fatal("capability interruption discarded a returned source cause")
		}
	}
	if observationBoundaryDetails(err) != boundary || sshAdmissionFailurePhases(err) != admissionPhase {
		t.Fatal("capability interruption lost or invented a finite source proof")
	}
	var proof *observationBoundaryError
	if boundary != "" && !errors.As(err, &proof) {
		t.Fatal("capability interruption lost the typed local boundary")
	}
	var admission *sshAdmissionUnavailableError
	if admissionPhase != "" && !errors.As(err, &admission) {
		t.Fatal("capability interruption lost typed admission authority")
	}
	visibility := visibilityAlert(settings, signal, err)
	if visibility.Class != "cannot-observe" || visibility.Severity != SeverityWarn ||
		visibility.Sustain != 2 || visibility.Target != capabilityProbeProbeId || !visibility.unobservable {
		t.Fatal("capability interruption changed generic visibility authority")
	}
	wantClass := observationErrorClassCanceled
	if parentErr == context.DeadlineExceeded {
		wantClass = observationErrorClassTimeout
	}
	if !strings.Contains(visibility.Observed, "error_class="+wantClass) {
		t.Fatal("capability interruption changed its parent classification")
	}
	if boundary != "" && !strings.Contains(visibility.Observed, boundary) {
		t.Fatal("capability visibility discarded a returned local boundary")
	}
	if admissionPhase != "" && !strings.Contains(visibility.Observed, "ssh_admission_failures="+admissionPhase) {
		t.Fatal("capability visibility discarded the returned admission phase")
	}
	for _, text := range []string{err.Error(), visibility.Markdown(), visibility.Observed} {
		if strings.Contains(text, "synthetic-capability-private") {
			t.Fatal("capability interruption exposed private source text")
		}
	}
}

// End the actual initially live owner synchronously in the source callback.
// Joined mode returns one failed gateway before a different terminal reply;
// no scheduling, elapsed timeout or unowned child supplies the ordering.
func TestCapabilityOwnerReturnedMetricsAuthority(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, scoped := range []bool{false, true} {
			for _, kind := range []string{"private", "native", "admission", "joined", "joined-nil-source", "nil-source"} {
				ctx, cancel := context.WithCancel(context.Background())
				var ownerCtx context.Context = ctx
				if deadline {
					ownerCtx = observationBoundaryDeadlineContext{Context: ctx}
				}
				privateCause := errors.New("synthetic-capability-private")
				admission := &sshAdmissionUnavailableError{err: privateCause, phase: sshAdmissionFailureAcquire}
				precontact := &observationBoundaryError{err: admission, boundary: observationBoundaryHost}
				native := &observationBoundaryError{err: privateCause, boundary: observationBoundaryNativeWait}
				replies := []error{privateCause}
				wantBoundary, wantAdmission := "", ""
				switch kind {
				case "native":
					replies = []error{native}
					wantBoundary = " execution_boundaries=native-wait/started-joined"
				case "admission":
					replies = []error{precontact}
					wantBoundary = " execution_boundaries=host-admission/not-attempted"
					wantAdmission = "acquire"
				case "joined":
					replies = []error{precontact, native}
					wantBoundary = " execution_boundaries=host-admission/not-attempted,native-wait/started-joined"
					wantAdmission = "acquire"
				case "joined-nil-source":
					replies = []error{precontact, nil}
					wantBoundary = " execution_boundaries=host-admission/not-attempted"
					wantAdmission = "acquire"
				case "nil-source":
					replies = []error{nil}
				}
				calls := 0
				source := &capabilityOwnerSource{syntheticSource: &syntheticSource{}}
				source.host = func(sourceCtx context.Context, _ HostSettings, command string) (string, error) {
					if sourceCtx.Err() != nil || calls >= len(replies) {
						t.Fatal("capability metrics contacted another gateway after retirement")
					}
					if !strings.Contains(command, "--max-time 15 --max-filesize 65536") {
						t.Fatal("capability metrics changed its existing query bounds")
					}
					err := replies[calls]
					calls++
					if calls == len(replies) {
						cancel()
						if sourceCtx.Err() != ownerCtx.Err() {
							t.Fatal("capability source did not receive the actual owner lifecycle")
						}
					}
					return "synthetic-capability-private", err
				}
				settings := capabilityOwnerSettings(source, scoped, len(replies)+1)
				signal := newCapabilityProbeSignal(func() (capabilityProbeDesired, error) {
					return capabilityProbeDesired{enabled: true}, nil
				}, defaultCapabilityProbeSettings())
				alerts, err := signal.Run(ownerCtx, settings)
				cancel()
				if calls != len(replies) || len(alerts) != 0 {
					t.Fatal("capability interruption changed contact count or published a retired finding")
				}
				causes := append([]error(nil), replies...)
				if kind != "nil-source" {
					causes = append(causes, privateCause)
				}
				if kind == "admission" || kind == "joined" || kind == "joined-nil-source" {
					causes = append(causes, admission)
				}
				requireCapabilityOwnerAuthority(t, signal, settings, err, ownerCtx.Err(), causes, wantBoundary, wantAdmission)
			}
		}
	}
}

// The desired-config return has the same finding-to-retirement handoff.
// Retain only an actually returned cause and never start the metrics source.
func TestCapabilityOwnerReturnedDesiredAuthority(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, nilSource := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			var ownerCtx context.Context = ctx
			if deadline {
				ownerCtx = observationBoundaryDeadlineContext{Context: ctx}
			}
			privateCause := errors.New("synthetic-capability-private")
			var sourceErr error = privateCause
			if nilSource {
				sourceErr = nil
			}
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				t.Fatal("capability metrics ran after desired-config owner retirement")
				return "", nil
			}}
			loads := 0
			signal := newCapabilityProbeSignal(func() (capabilityProbeDesired, error) {
				if ownerCtx.Err() != nil {
					t.Fatal("desired-config control did not enter with a live owner")
				}
				loads++
				cancel()
				return capabilityProbeDesired{enabled: true}, sourceErr
			}, defaultCapabilityProbeSettings())
			settings := capabilityOwnerSettings(source, false, 1)
			alerts, err := signal.Run(ownerCtx, settings)
			cancel()
			if loads != 1 || len(alerts) != 0 {
				t.Fatal("capability desired return published a retired result")
			}
			requireCapabilityOwnerAuthority(t, signal, settings, err, ownerCtx.Err(), []error{sourceErr}, "", "")
		}
	}
}

// A source cancellation or deadline alone does not retire a live owner.
// Preserve its existing local unavailable finding instead of returning a new
// fatal signal error or inferring a stopped capability chain.
func TestCapabilityOwnerLiveSourceFailureKeepsLocalFinding(t *testing.T) {
	for _, sourceLifecycle := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		privateCause := errors.New("synthetic-capability-private")
		proof := &observationBoundaryError{err: errors.Join(privateCause, sourceLifecycle), boundary: observationBoundaryNativeWait}
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			return "synthetic-capability-private", proof
		}}
		signal := newCapabilityProbeSignal(func() (capabilityProbeDesired, error) {
			return capabilityProbeDesired{enabled: true}, nil
		}, defaultCapabilityProbeSettings())
		settings := capabilityOwnerSettings(source, false, 1)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || calls != 1 || len(alerts) != 1 || alerts[0].Class != "cannot-observe" ||
			alerts[0].Target != "capability-probe/metrics" || alerts[0].Sustain != 2 ||
			!strings.Contains(alerts[0].Observed, "execution_boundaries=native-wait/started-joined") {
			t.Fatal("live capability source failure changed local visibility behavior")
		}
		if strings.Contains(alerts[0].Markdown(), "synthetic-capability-private") {
			t.Fatal("live capability source failure exposed private text")
		}
	}
}

// Existing healthy, unavailable and desired-off inputs keep their meanings;
// a legacy already-canceled unscoped caller still gets its local finding.
func TestCapabilityOwnerExistingSourceControls(t *testing.T) {
	for _, kind := range []string{"healthy", "missing-metrics", "desired-off", "invalid-response", "pre-canceled"} {
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			if kind == "invalid-response" || kind == "pre-canceled" {
				return "synthetic-capability-private", nil
			}
			return capabilityProbeMimirFixture(t, map[string]float64{
				"disabled_increase": 0,
				"disabled_running":  0,
				"last_pass":         capabilityProbeTestUnix(0),
				"exporting":         1,
				"oldest_start":      capabilityProbeTestUnix(0),
			}), nil
		}}
		signal := newCapabilityProbeSignal(func() (capabilityProbeDesired, error) {
			return capabilityProbeDesired{enabled: kind != "desired-off"}, nil
		}, defaultCapabilityProbeSettings())
		hosts := 1
		if kind == "missing-metrics" {
			hosts = 0
		}
		settings := capabilityOwnerSettings(source, false, hosts)
		ctx, cancel := context.WithCancel(context.Background())
		if kind == "pre-canceled" {
			cancel()
		}
		alerts, err := signal.Run(ctx, settings)
		cancel()
		wantCalls, wantAlerts := 1, 0
		if kind == "missing-metrics" || kind == "desired-off" {
			wantCalls = 0
		}
		if kind == "missing-metrics" || kind == "invalid-response" || kind == "pre-canceled" {
			wantAlerts = 1
		}
		if err != nil || calls != wantCalls || len(alerts) != wantAlerts {
			t.Fatal("capability control changed source contacts or observation outcome")
		}
		if wantAlerts == 1 && (alerts[0].Class != "cannot-observe" || alerts[0].Target != "capability-probe/metrics") {
			t.Fatal("capability control changed local unavailable authority")
		}
	}
}
