// SIGNALS.md §19.3 retains private query causes behind fixed public text.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The production source interface passes the real parent context to this seam.
// Other source methods remain the existing inert synthetic implementation.
type accountMessagesContextSource struct {
	*syntheticSource
	host func(context.Context, HostSettings, string) (string, error)
}

func (self *accountMessagesContextSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.host(ctx, host, command)
}

// An unrelated exclusion enables the real public scoped-adapter branch while
// both query gateways remain admitted. No real endpoints or credentials exist.
func accountMessagesErrorSettings(t *testing.T, source SignalSource, scoped bool) SignalSettings {
	t.Helper()
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return accountMessagesTestNow }
	settings.Hosts = []HostSettings{
		{Name: "synthetic-private-gateway-a.example", Roles: []string{"services"}},
		{Name: "synthetic-private-gateway-b.example", Roles: []string{"services"}},
		{Name: "synthetic-private-excluded.example"},
	}
	if scoped {
		var err error
		settings, err = ExcludeHosts(settings, "synthetic-private-excluded.example")
		if err != nil {
			t.Fatal("could not enable synthetic host scope")
		}
	}
	return settings
}

// Error identity and alert classification are independent of public formatting.
// Inspect the bounded cause graph as well as errors.Is/errors.As consumers.
func requireAccountMessagesErrorIdentity(t *testing.T, settings SignalSettings, signal Signal, err error, wantText, wantClass, wantPhases string, causes ...error) {
	t.Helper()
	if err == nil || err.Error() != wantText {
		t.Fatal("query error lost its fixed public diagnostic")
	}
	inspection := server.InspectErrorCauses(err)
	if !inspection.Complete {
		t.Fatal("returned source cause graph became incomplete")
	}
	for _, cause := range causes {
		if !errors.Is(err, cause) {
			t.Fatal("query error discarded a returned source cause")
		}
		found := false
		for _, node := range inspection.Nodes {
			found = found || node.Err == cause
		}
		if !found {
			t.Fatal("bounded cause inspection cannot reach a returned source cause")
		}
	}
	if classifyObservationError(err) != wantClass || sshAdmissionFailurePhases(err) != wantPhases {
		t.Fatal("query error changed lifecycle or admission classification")
	}
	alert := visibilityAlert(settings, signal, err)
	if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
		!strings.Contains(alert.Observed, "error_class="+wantClass) {
		t.Fatal("query failure changed its public visibility finding")
	}
	encoded, encodeErr := json.Marshal(alert)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	var markdown strings.Builder
	if err := WriteAlertsMarkdown(&markdown, Alerts{alert}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(err.Error()+string(encoded)+markdown.String(), "synthetic-private-") {
		t.Fatal("private transport or source text escaped into public output")
	}
}

// The parent stays live: source-only wrapped cancellation remains a source
// failure, and the existing gateway helper still tries its other candidate.
func TestAccountMessagesErrorLiveParentPreservesWrappedSourceCauses(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, sourceContext := range []error{context.Canceled, context.DeadlineExceeded} {
			privateCause := errors.New("synthetic-private-source-cause")
			first := fmt.Errorf("synthetic-private-transport: %w", errors.Join(sourceContext, privateCause))
			admissionCause := errors.New("synthetic-private-admission-cause")
			second := &sshAdmissionUnavailableError{err: admissionCause, phase: sshAdmissionFailureAcquire}
			calls := 0
			source := &accountMessagesContextSource{syntheticSource: &syntheticSource{}, host: func(ctx context.Context, _ HostSettings, _ string) (string, error) {
				if ctx.Err() != nil {
					t.Fatal("source-only cancellation ended the parent")
				}
				calls++
				if calls == 1 {
					return "synthetic-private-body", first
				}
				if calls != 2 {
					t.Fatal("unexpected additional source call")
				}
				return "", second
			}}
			settings := accountMessagesErrorSettings(t, source, scoped)
			signal := NewAccountMessagesSignal()
			alerts, err := signal.Run(context.Background(), settings)
			if calls != 2 || len(alerts) != 0 {
				t.Fatal("live-parent gateway failure changed fallback or emitted findings")
			}
			wantClass := observationErrorClassCanceled
			if sourceContext == context.DeadlineExceeded {
				wantClass = observationErrorClassTimeout
			}
			requireAccountMessagesErrorIdentity(t, settings, signal, err, "account messages: query Mimir failed", wantClass, "acquire", first, sourceContext, privateCause, second, admissionCause)
			var admission *sshAdmissionUnavailableError
			if !errors.As(err, &admission) || admission != second {
				t.Fatal("typed admission cause was discarded")
			}
		}
	}
}

// The actual parent ends synchronously inside the first source callback.
// Both live-entry adapters give that verdict canonical public authority while
// preserving every private source cause and refusing another gateway read.
func TestAccountMessagesErrorEndedParentPreservesReturnedCauseAndStopsFallback(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, sourceContext := range []error{context.Canceled, context.DeadlineExceeded} {
			ctx, cancel := context.WithCancel(context.Background())
			privateCause := errors.New("synthetic-private-returned-cause")
			admission := &sshAdmissionUnavailableError{err: privateCause, phase: sshAdmissionFailureRelease}
			sourceErr := fmt.Errorf("synthetic-private-transport: %w", errors.Join(sourceContext, admission))
			calls := 0
			source := &accountMessagesContextSource{syntheticSource: &syntheticSource{}, host: func(sourceCtx context.Context, _ HostSettings, _ string) (string, error) {
				calls++
				if calls != 1 || sourceCtx.Err() != nil {
					t.Fatal("canceled parent admitted an additional gateway observation")
				}
				cancel()
				if sourceCtx.Err() != context.Canceled {
					t.Fatal("source did not receive the actual ended parent")
				}
				return "synthetic-private-body", sourceErr
			}}
			settings := accountMessagesErrorSettings(t, source, scoped)
			signal := NewAccountMessagesSignal()
			alerts, err := signal.Run(ctx, settings)
			cancel()
			if calls != 1 || len(alerts) != 0 {
				t.Fatal("ended parent admitted fallback or published findings")
			}
			wantClass, wantText := observationErrorClassCanceled, context.Canceled.Error()
			var interrupted *signalContextError
			if !errors.Is(err, context.Canceled) || !errors.As(err, &interrupted) {
				t.Fatal("live-entry adapter lost the parent lifecycle verdict")
			}
			requireAccountMessagesErrorIdentity(t, settings, signal, err, wantText, wantClass, "release", sourceErr, sourceContext, admission, privateCause)
		}
	}
}

// Real already-ended parents cover both context sentinels without sleeps.
// Scoped admission performs no read; unscoped callers retain probe authority
// and the existing helper stops after the first returned source failure.
func TestAccountMessagesErrorAlreadyEndedParentsPreservePolicy(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, deadline := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
			}
			if ctx.Err() == nil {
				t.Fatal("parent fixture did not end synchronously")
			}
			calls := 0
			privateCause := errors.New("synthetic-private-ended-parent-cause")
			sourceErr := fmt.Errorf("synthetic-private-transport: %w", errors.Join(ctx.Err(), privateCause))
			source := &accountMessagesContextSource{syntheticSource: &syntheticSource{}, host: func(sourceCtx context.Context, _ HostSettings, _ string) (string, error) {
				calls++
				if calls != 1 || sourceCtx.Err() == nil {
					t.Fatal("ended parent fixture made an unexpected source call")
				}
				return "", sourceErr
			}}
			settings := accountMessagesErrorSettings(t, source, scoped)
			signal := NewAccountMessagesSignal()
			alerts, err := signal.Run(ctx, settings)
			cancel()
			wantCalls, wantText := 1, "account messages: query Mimir failed"
			causes := []error{ctx.Err(), sourceErr, privateCause}
			if scoped {
				wantCalls, wantText = 0, ctx.Err().Error()
				causes = []error{ctx.Err()}
			}
			if calls != wantCalls || len(alerts) != 0 {
				t.Fatal("already-ended parent changed scope or fallback authority")
			}
			wantClass := observationErrorClassCanceled
			if deadline {
				wantClass = observationErrorClassTimeout
			}
			requireAccountMessagesErrorIdentity(t, settings, signal, err, wantText, wantClass, "", causes...)
		}
	}
}

// All gateway admission causes remain distinguishable, including phases and
// private underlying identities; none becomes an unclassified fixed string.
func TestAccountMessagesErrorPreservesJoinedAdmissionPhases(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		acquireCause, releaseCause := errors.New("synthetic-private-acquire"), errors.New("synthetic-private-release")
		acquire := &sshAdmissionUnavailableError{err: acquireCause, phase: sshAdmissionFailureAcquire}
		release := &sshAdmissionUnavailableError{err: releaseCause, phase: sshAdmissionFailureRelease}
		calls := 0
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			calls++
			if calls == 1 {
				return "", acquire
			}
			return "", release
		}}
		settings := accountMessagesErrorSettings(t, source, scoped)
		signal := NewAccountMessagesSignal()
		alerts, err := signal.Run(context.Background(), settings)
		if calls != 2 || len(alerts) != 0 {
			t.Fatal("admission fixture changed gateway count or emitted findings")
		}
		requireAccountMessagesErrorIdentity(t, settings, signal, err, "account messages: query Mimir failed", observationErrorClassAdmissionUnavailable, "acquire,release", acquire, release, acquireCause, releaseCause)
	}
}

// A live parent can still recover on the existing next gateway after a
// source-only context failure. The wrapper must not manufacture parent death.
func TestAccountMessagesErrorLiveParentKeepsSuccessfulGatewayFallback(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, sourceContext := range []error{context.Canceled, context.DeadlineExceeded} {
			payload := accountMessagesFixtureJSON(t, accountMessagesTestNow, []accountMessagesFixture{
				{job: "api", channel: "email", result: "sent", value: 20},
				{job: "api", channel: "sms", result: "sent", value: 20},
			})
			calls := 0
			source := &accountMessagesContextSource{syntheticSource: &syntheticSource{}, host: func(ctx context.Context, _ HostSettings, _ string) (string, error) {
				if ctx.Err() != nil {
					t.Fatal("source-only context failure ended the live parent")
				}
				calls++
				if calls == 1 {
					return "", fmt.Errorf("synthetic-private-transport: %w", sourceContext)
				}
				return payload, nil
			}}
			settings := accountMessagesErrorSettings(t, source, scoped)
			alerts, err := NewAccountMessagesSignal().Run(context.Background(), settings)
			if calls != 2 || err != nil || len(alerts) != 0 {
				t.Fatal("live-parent fallback failed to preserve the healthy control")
			}
		}
	}
}

// The fixed-text wrapper must also preserve intentional scope-denial leaves;
// an excluded gateway is unknown coverage and never a contacted-source fault.
func TestAccountMessagesErrorPreservesIntentionalGatewayScopeDenial(t *testing.T) {
	calls := 0
	settings := accountMessagesErrorSettings(t, &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		calls++
		return "", errors.New("synthetic-private-unexpected-contact")
	}}, false)
	var err error
	settings, err = ExcludeHosts(settings, "synthetic-private-gateway-a.example", "synthetic-private-gateway-b.example")
	if err != nil {
		t.Fatal("could not exclude the synthetic gateways")
	}
	alerts, err := NewAccountMessagesSignal().Run(context.Background(), settings)
	if err != nil || calls != 0 || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" ||
		!strings.Contains(alerts[0].Observed, "blocked_hosts=2") {
		t.Fatal("query wrapper changed intentional gateway scope into source failure")
	}
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	var markdown strings.Builder
	if err := WriteAlertsMarkdown(&markdown, alerts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded)+markdown.String(), "synthetic-private-") {
		t.Fatal("scope diagnostic exposed private host identities")
	}
}
