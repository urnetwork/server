// Credentials remain metadata-only; these controls exercise public acceptance.
package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Any source use changes the counter; no real source or resource is reachable.
func credentialsParentSettings() (SignalSettings, *int) {
	calls := new(int)
	unexpected := errors.New("synthetic source must not be contacted")
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { *calls++; return nil, unexpected },
		redisFn:    func(HostSettings, int, ...string) (string, error) { *calls++; return "", unexpected },
		hostFn:     func(HostSettings, string) (string, error) { *calls++; return "", unexpected },
		localFn:    func(string, ...string) (string, error) { *calls++; return "", unexpected },
	}
	return syntheticSettings(source), calls
}

// The parent ends synchronously inside the existing observed-time callback.
func TestCredentialsSignalLiveParentAcceptance(t *testing.T) {
	for _, requirement := range []CredentialRequirement{
		{Key: "synthetic-missing", Resource: "synthetic.yml", Required: true},
		{Key: "synthetic-malformed", Resource: "synthetic.yml", Required: true, Present: true, Malformed: true},
		{Key: "synthetic-partial", Resource: "synthetic.yml", Required: true, Present: true, MissingFields: []string{"token"}},
		{Key: "synthetic-activation", Resource: "synthetic.yml", ActivationUnknown: true},
	} {
		t.Run(requirement.Key, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settings, sourceCalls := credentialsParentSettings()
			settings.Credentials = []CredentialRequirement{requirement}
			now := settings.Now
			nowCalls := 0
			settings.Now = func() time.Time {
				nowCalls++
				if ctx.Err() != nil {
					t.Fatal("evaluation did not reach the cancellation seam live")
				}
				cancel()
				return now()
			}
			alerts, err := NewCredentialsSignal().Run(ctx, settings)
			if nowCalls != 1 || *sourceCalls != 0 || err != context.Canceled || len(alerts) != 0 {
				t.Fatal("ended live readiness evaluation retained a verdict or lost lifecycle authority")
			}
		})
	}
}

// This legacy local-only entry is deliberately distinct from a live owner ending.
func TestCredentialsSignalPreCanceledLocalCompatibility(t *testing.T) {
	for _, broken := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		settings, sourceCalls := credentialsParentSettings()
		settings.Credentials = []CredentialRequirement{{Key: "synthetic", Resource: "synthetic.yml", Required: true, Present: true}}
		if broken {
			settings.Credentials[0].MissingFields = []string{"token"}
		}
		alerts, err := NewCredentialsSignal().Run(ctx, settings)
		if err != nil || *sourceCalls != 0 || (broken && len(alerts) != 1) || (!broken && len(alerts) != 0) {
			t.Fatal("pre-canceled metadata-only compatibility changed")
		}
		if broken {
			requireAlertClass(t, alerts, "credential-fields-missing")
		}
	}
}

// A live owner still distinguishes required, optional and unknown readiness.
func TestCredentialsSignalLiveReadinessCountercases(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		requirement CredentialRequirement
		class       string
		severity    Severity
	}{
		{name: "required-complete", requirement: CredentialRequirement{Required: true, Present: true}},
		{name: "optional-absent"},
		{name: "required-missing", requirement: CredentialRequirement{Required: true}, class: "credential-resource-missing", severity: SeverityPage},
		{name: "optional-partial", requirement: CredentialRequirement{Present: true, MissingFields: []string{"token"}}, class: "credential-fields-missing", severity: SeverityWarn},
		{name: "activation-unknown", requirement: CredentialRequirement{ActivationUnknown: true}, class: "credential-activation-unobservable", severity: SeverityWarn},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settings, sourceCalls := credentialsParentSettings()
			fixture.requirement.Key, fixture.requirement.Resource = "synthetic", "synthetic.yml"
			settings.Credentials = []CredentialRequirement{fixture.requirement}
			alerts, err := NewCredentialsSignal().Run(ctx, settings)
			if err != nil || ctx.Err() != nil || *sourceCalls != 0 {
				t.Fatal("live metadata evaluation lost source-free authority")
			}
			if fixture.class == "" {
				if len(alerts) != 0 {
					t.Fatal("healthy local metadata invented an alert")
				}
			} else if len(alerts) != 1 || requireAlertClass(t, alerts, fixture.class).Severity != fixture.severity {
				t.Fatal("live readiness classification changed")
			}
		})
	}
}

// Direct registered evaluation and one-shot visibility retain their own API contracts.
func TestCredentialsSignalPublicEntryPointsWithholdEndedReadiness(t *testing.T) {
	for _, oneShot := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		settings, sourceCalls := credentialsParentSettings()
		settings.Credentials = []CredentialRequirement{{Key: "synthetic", Resource: "synthetic.yml", Required: true}}
		now := settings.Now
		settings.Now = func() time.Time { cancel(); return now() }
		monitor := NewWithSignals(settings, NewCredentialsSignal())
		var alerts Alerts
		var err error
		if oneShot {
			alerts, err = monitor.Run(ctx)
		} else {
			alerts, err = monitor.RunSignal(ctx, "credentials")
		}
		cancel()
		if !errors.Is(err, context.Canceled) || *sourceCalls != 0 {
			t.Fatal("public entry lost parent cancellation or contacted a source")
		}
		if oneShot {
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
				t.Fatal("one-shot retained readiness instead of its visibility error")
			}
		} else if len(alerts) != 0 {
			t.Fatal("RunSignal accepted an ended readiness result")
		}
	}
}

// Existing metadata validation failures remain inspectable behind safe parent text.
func TestCredentialsSignalEndedParentRetainsValidationCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, sourceCalls := credentialsParentSettings()
	now := settings.Now
	settings.PGQuerySampleUntil = now().Add(25 * time.Hour)
	settings.Now = func() time.Time { cancel(); return now() }
	alerts, err := NewCredentialsSignal().Run(ctx, settings)
	var interrupted *signalContextError
	if len(alerts) != 0 || *sourceCalls != 0 || !errors.As(err, &interrupted) || interrupted.contextErr != context.Canceled || interrupted.sourceErr == nil || err.Error() != context.Canceled.Error() {
		t.Fatal("ended metadata validation discarded its cause or exposed its error text")
	}
}
