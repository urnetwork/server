package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The barrier holds all four admitted calls until the fifth wait has returned.
// Cancellation therefore cannot race a permit release and manufacture contact.
type mimirAdmissionFanoutBoundaryRunner struct {
	probeRunner
	release <-chan struct{}
	calls   atomic.Int64
}

func (self *mimirAdmissionFanoutBoundaryRunner) shell(ctx context.Context, _ *host, _ string) (string, error) {
	self.calls.Add(1)
	<-self.release
	return "", ctx.Err()
}

func TestMimirAdmissionFanoutCancellationRetainsPretransportBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := syntheticSettings(&syntheticSource{})
		settings.Hosts = nil
		for i := range 5 {
			settings.Hosts = append(settings.Hosts, HostSettings{
				Name: fmt.Sprintf("metrics-%d.example", i), Roles: []string{"services"},
			})
		}
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		transport := &mimirAdmissionFanoutBoundaryRunner{probeRunner: env.runner, release: release}
		env.runner = transport
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan []finding, 1)
		go func() {
			findings, err := (&mimirAdmissionProbe{}).check(observationBoundaryDeadlineContext{Context: ctx}, env)
			if err != nil {
				panic("synthetic probe unexpectedly returned a top-level error")
			}
			done <- findings
		}()
		synctest.Wait()
		if transport.calls.Load() != 4 {
			t.Fatal("fanout barrier did not admit exactly four calls")
		}
		cancel()
		synctest.Wait()
		close(release)
		findings := <-done
		if transport.calls.Load() != 4 || len(findings) != 5 {
			t.Fatal("canceled fanout retried contact or invented a rejection")
		}
		marked := 0
		for _, finding := range findings {
			alert := alertFromFinding(settings, "11.20a", "mimir-admission", "Mimir admission", finding)
			if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn ||
				!strings.HasPrefix(alert.Observed, "error_class=observation-timeout") || finding.healthy {
				t.Fatal("fanout cancellation became rejection, recovery or wrong lifecycle class")
			}
			if strings.Contains(alert.Observed, "execution_boundaries=mimir-host-admission/not-attempted") {
				marked++
			}
			if strings.Contains(alert.Observed, "native-") || strings.Contains(alert.Observed, "ssh_admission_failures=") {
				t.Fatal("fanout/callback invented native or shared-admission proof")
			}
		}
		if marked != 1 {
			t.Fatal("waiting host lost its exact pretransport boundary")
		}
	})
}

// Real local synthetic ssh executables exercise Start/Wait without networking.
// An injected shared refusal exercises the same owning runner before Start.
func TestMimirAdmissionTransportBoundariesPreserveHeldRateIncident(t *testing.T) {
	now := time.Date(2035, 1, 2, 3, 4, 0, 0, time.UTC)
	complete := mimirAdmissionCounterHostResponses(now.Add(-time.Hour).Unix(), 0, 3, 123456, 3456)
	for _, testCase := range []struct {
		name string
		want string
	}{
		{name: "shared-refusal", want: "ssh_admission_failures=acquire"},
		{name: "native-start", want: "execution_boundaries=native-start/failed-no-child"},
		{name: "native-wait", want: "execution_boundaries=native-wait/started-joined"},
	} {
		signal := NewMimirAdmissionSignal()
		initial := runMimirAdmissionSynthetic(t, signal, now, complete)
		page := requireAlertClass(t, initial, "mimir-ingestion-rate-limit")
		if !strings.Contains(page.Observed, "rate_discard_counter_increase=3") {
			t.Fatal("positive exact-child counter did not establish rejection")
		}
		settings := syntheticSettings(nil)
		settings.Environment = "synthetic-environment"
		settings.Now = func() time.Time { return now.Add(time.Minute) }
		settings.Hosts = []HostSettings{{Name: "metrics-a.example", OverlayAddress: "192.0.2.91", Roles: []string{"services"}}}
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		transport := env.runner.(*runner)
		dir := t.TempDir()
		t.Setenv("PATH", dir)
		if testCase.name == "native-wait" {
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf private-mimir-boundary-canary >&2\nexit 255\n"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if testCase.name == "shared-refusal" {
			transport.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{
				acquireCommand: func(context.Context, string) (func() error, error) { return nil, context.DeadlineExceeded },
			}
			transport.runSSH = func(context.Context, []string, string) (string, string, error) {
				t.Error("shared refusal entered native transport")
				return "", "", nil
			}
		}
		probe := signal.(*signalAdapter).probe.(*mimirAdmissionProbe)
		findings, err := probe.check(t.Context(), env)
		if err != nil {
			t.Fatal(err)
		}
		var alerts []Alert
		for _, finding := range findings {
			if finding.healthy {
				t.Fatal("unavailable host emitted healthy recovery")
			}
			alerts = append(alerts, alertFromFinding(settings, "11.20a", "mimir-admission", "Mimir admission", finding))
		}
		visibility := requireMimirAdmissionObservationFailure(t, alerts, "metrics-a.example/mimir-admission")
		if !strings.Contains(visibility.Observed, testCase.want) {
			t.Fatalf("%s lost its owning execution boundary", testCase.name)
		}
		if testCase.name == "shared-refusal" && strings.Contains(visibility.Observed, "native-") {
			t.Fatal("shared refusal acquired native proof")
		}
		requireAlertOmits(t, visibility, "private-mimir-boundary-canary", "192.0.2.91", "exit status 255")
		page = requireAlertClass(t, alerts, "mimir-ingestion-rate-limit")
		for _, field := range []string{"rate_discard_counter_increase=0", "direct_complete=false", "comparable=false", "quiet_complete=0s"} {
			if !strings.Contains(page.Observed, field) {
				t.Fatal("transport failure became fresh loss or healthy counter evidence")
			}
		}
		restoredAt := now.Add(3 * time.Hour)
		restored := runMimirAdmissionSynthetic(t, signal, restoredAt, complete)
		requireAlertClass(t, restored, "mimir-ingestion-rate-limit")
		if recovered := runMimirAdmissionSynthetic(t, signal, restoredAt.Add(mimirAdmissionQuietWindow), complete); len(recovered) != 0 {
			t.Fatal("complete comparable quiet window failed to recover")
		}
	}
}
