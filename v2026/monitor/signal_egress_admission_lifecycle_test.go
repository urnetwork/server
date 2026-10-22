package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Exposes the actual public transport context; all other source seams retain
// the ordinary synthetic implementation and cannot contact production.
type egressAdmissionLifecycleSource struct {
	*syntheticSource
	read func(context.Context, HostSettings, string) (string, error)
}

func (self *egressAdmissionLifecycleSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.read(ctx, host, command)
}

func egressAdmissionLifecycleSettings(source SignalSource, now time.Time, scoped bool) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-metrics.example", Roles: []string{"services"}}}
	settings.Now = func() time.Time { return now }
	if scoped {
		// Enable the scoped adapter without denying the permitted gateway.
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "synthetic-excluded.example"})
		settings.ExcludedHosts = []string{"synthetic-excluded.example"}
	}
	return settings
}

// Both empty and previously warmed observers must retain their exact authority.
// Each ordering uses actual context cancellation, never a returned cancellation
// error as a substitute for ending the owning parent.
func checkEgressAdmissionInterruptedState(t *testing.T, phase string) {
	t.Helper()
	for _, scoped := range []bool{false, true} {
		for _, seeded := range []bool{false, true} {
			now := time.Unix(1_700_000_000, 0).UTC()
			signal := NewEgressAdmissionSignal()
			probe := signal.(*signalAdapter).probe.(*egressAdmissionProbe)
			processes := egressAdmissionTestProcesses(now)
			if seeded {
				first := now.Add(-time.Minute)
				runEgressAdmissionTest(t, signal, first, egressAdmissionTestJson(t, first, processes))
			}
			beforeAt := probe.observedAt
			var before map[egressAdmissionIdentity]egressAdmissionObservation
			if probe.previous != nil {
				before = map[egressAdmissionIdentity]egressAdmissionObservation{}
				for identity, observation := range probe.previous {
					before[identity] = observation
				}
			}
			body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
			ctx, cancel := context.WithCancel(context.Background())
			calls, clocks := 0, 0
			source := &egressAdmissionLifecycleSource{syntheticSource: &syntheticSource{}, read: func(sourceCtx context.Context, _ HostSettings, _ string) (string, error) {
				calls++
				if phase == "source" {
					cancel()
					<-sourceCtx.Done()
				}
				return body, nil
			}}
			settings := egressAdmissionLifecycleSettings(source, now, scoped)
			clockReached := make(chan struct{})
			settings.Now = func() time.Time {
				clocks++
				if clocks == 2 {
					if phase == "clock" {
						cancel()
					}
					if phase == "lock" {
						close(clockReached)
					}
				}
				return now
			}
			var alerts Alerts
			var err error
			if phase == "lock" {
				probe.stateLock.Lock()
				finished := make(chan struct{})
				go func() {
					alerts, err = signal.Run(ctx, settings)
					close(finished)
				}()
				// The public run has obtained valid data and reached the final
				// clock callback. It cannot acquire the reduction lock yet.
				select {
				case <-clockReached:
				case <-time.After(30 * time.Second):
					cancel()
					probe.stateLock.Unlock()
					t.Fatal("public run did not reach the explicit clock barrier")
				}
				cancel()
				probe.stateLock.Unlock()
				select {
				case <-finished:
				case <-time.After(30 * time.Second):
					t.Fatal("public run did not join the canceled reduction")
				}
			} else {
				alerts, err = signal.Run(ctx, settings)
			}
			cancel()
			if !reflect.DeepEqual(probe.previous, before) || !probe.observedAt.Equal(beforeAt) {
				t.Fatalf("ended owner changed observer state: phase=%s scoped=%t seeded=%t", phase, scoped, seeded)
			}
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 1 {
				t.Fatalf("ended owner lost lifecycle authority: phase=%s scoped=%t seeded=%t", phase, scoped, seeded)
			}
			if !seeded {
				next := now.Add(time.Minute)
				warmup := requireAlertClass(t, runEgressAdmissionTest(t, signal, next, egressAdmissionTestJson(t, next, processes)), "egress-admission-unobservable")
				if !strings.Contains(warmup.Observed, "complete_process_intervals=0") {
					t.Fatal("ended owner supplied next-run warmup credit")
				}
			}
		}
	}
}

func TestEgressAdmissionEndedOwnerRejectsLateValidNativeBody(t *testing.T) {
	checkEgressAdmissionInterruptedState(t, "source")
}

func TestEgressAdmissionEndedOwnerInClockCannotCommit(t *testing.T) {
	checkEgressAdmissionInterruptedState(t, "clock")
}

func TestEgressAdmissionEndedOwnerWaitingForReductionCannotCommit(t *testing.T) {
	checkEgressAdmissionInterruptedState(t, "lock")
}

func TestEgressAdmissionAlreadyEndedOwnerDoesNotEnterSource(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			return "", nil
		}}
		signal := NewEgressAdmissionSignal()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		alerts, err := signal.Run(ctx, egressAdmissionLifecycleSettings(source, time.Unix(1_700_000_000, 0).UTC(), scoped))
		probe := signal.(*signalAdapter).probe.(*egressAdmissionProbe)
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 0 || probe.previous != nil || !probe.observedAt.IsZero() {
			t.Fatal("already-ended owner entered source, returned findings or seeded state")
		}
	}
}

func TestEgressAdmissionEndedOwnerRetainsPrivateSourceCause(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		private := errors.New("synthetic-private-egress-source-error")
		source := &egressAdmissionLifecycleSource{syntheticSource: &syntheticSource{}, read: func(sourceCtx context.Context, _ HostSettings, _ string) (string, error) {
			cancel()
			<-sourceCtx.Done()
			return "synthetic-private-egress-source-body", private
		}}
		signal := NewEgressAdmissionSignal()
		alerts, err := signal.Run(ctx, egressAdmissionLifecycleSettings(source, time.Unix(1_700_000_000, 0).UTC(), scoped))
		cancel()
		if !errors.Is(err, context.Canceled) || !errors.Is(err, private) || len(alerts) != 0 {
			t.Fatal("ended owner lost lifecycle or source identity")
		}
		if err.Error() != context.Canceled.Error() {
			t.Fatal("ended owner rendered a private source error")
		}
		probe := signal.(*signalAdapter).probe.(*egressAdmissionProbe)
		if probe.previous != nil || !probe.observedAt.IsZero() {
			t.Fatal("interrupted source failure seeded state")
		}
	}
}

func TestEgressAdmissionLiveParentReturnedCancellationRemainsUnknown(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			return "synthetic-private-ignored-body", context.Canceled
		}}
		ctx, cancel := context.WithCancel(context.Background())
		signal := NewEgressAdmissionSignal()
		alerts, err := signal.Run(ctx, egressAdmissionLifecycleSettings(source, time.Unix(1_700_000_000, 0).UTC(), scoped))
		if err != nil || ctx.Err() != nil {
			t.Fatal("returned source cancellation ended a live parent")
		}
		requireEgressAdmissionSourceDetails(t, alerts, "live-parent", "error_class=observation-canceled")
		cancel()
	}
}

func TestEgressAdmissionLivePartialFailureAndHealthyControls(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		now := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(now)
		signal := NewEgressAdmissionSignal()
		body := ""
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return body, nil }}
		settings := egressAdmissionLifecycleSettings(source, now, scoped)
		settings.Now = func() time.Time { return now }
		for i := 0; i < 3; i++ {
			body = egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal("live healthy control failed")
			}
			if i == 0 {
				requireAlertClass(t, alerts, "egress-admission-unobservable")
			} else if len(alerts) != 0 {
				t.Fatal("live complete healthy interval retained a visibility warning")
			}
			now = now.Add(time.Minute)
		}
		previous := egressAdmissionTestJson(t, now.Add(-time.Minute), processes)
		processes[0].values["requests"]++
		processes[0].values["selected/stale-health/true"]++
		processes[1].omit = "enabled"
		body = egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes), previous)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("live partial positive failed")
		}
		requireAlertClass(t, alerts, "egress-admission-unobservable")
		requireAlertClass(t, alerts, "egress-admission-expired")
		if len(alerts) != 2 {
			t.Fatal("partial input erased a known positive or invented findings")
		}
	}
}

// Samples the real parent's error, then performs one actual cancellation before
// returning that sampled value. Err's read linearizes before cancel; the next
// owning check must see cancellation. Arming at the final clock callback forces
// cancellation after the locked entry check and before candidate-state commit,
// without a production hook, a sleep, or concurrent map mutation.
type egressAdmissionCommitContext struct {
	context.Context
	cancel context.CancelFunc
	armed  bool
}

func (self *egressAdmissionCommitContext) Err() error {
	err := self.Context.Err()
	if self.armed {
		self.armed = false
		self.cancel()
	}
	return err
}

func TestEgressAdmissionCanceledDuringReductionCannotCommit(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		now := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(now)
		signal := NewEgressAdmissionSignal()
		body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return body, nil }}
		parent, cancel := context.WithCancel(context.Background())
		ctx := &egressAdmissionCommitContext{Context: parent, cancel: cancel}
		settings := egressAdmissionLifecycleSettings(source, now, scoped)
		clocks := 0
		settings.Now = func() time.Time {
			clocks++
			if clocks == 2 {
				ctx.armed = true
			}
			return now
		}
		alerts, err := signal.Run(ctx, settings)
		cancel()
		probe := signal.(*signalAdapter).probe.(*egressAdmissionProbe)
		if probe.previous != nil || !probe.observedAt.IsZero() {
			t.Fatal("cancellation after the live entry check committed warmup authority")
		}
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || ctx.armed {
			t.Fatal("reduction did not cross the armed real-cancellation boundary")
		}
	}
}
