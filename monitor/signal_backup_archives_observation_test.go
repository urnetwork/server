// Exercise the two archive reads through the same visibility renderer as the watcher.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A timed out native child can return from either read. Valid-looking stdout
// alongside its error has no authority to produce archive findings.
func TestBackupArchivesObservationReadTimeouts(t *testing.T) {
	const private = "synthetic-private-archive-source"
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	metrics := backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...)
	writer := backupArchiveWriterFixtureText(backupArchiveWriterFixture{})
	failed := &observationBoundaryError{
		boundary: observationBoundaryNativeWait,
		err:      &unreachableError{host: private, err: errors.New("timeout after synthetic deadline")},
	}
	for _, control := range []struct {
		host  string
		stage string
		calls string
	}{
		{host: "metrics-1", stage: "mimir-query", calls: "metrics-1"},
		{host: "backup-1", stage: "writer-read", calls: "metrics-1,backup-1"},
	} {
		var calls []string
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			calls = append(calls, host.Name)
			output := metrics
			if command == backupArchiveWriterCommand {
				output = writer
			}
			if host.Name == control.host {
				return output, failed
			}
			return output, nil
		}}
		settings := backupArchiveSourceSettings(now, source)
		alerts, err := NewWithSignals(settings, NewBackupArchivesSignal()).Run(context.Background())
		if !errors.Is(err, failed) || strings.Join(calls, ",") != control.calls || len(alerts) != 1 {
			t.Fatalf("%s lost the returned source failure or changed observation attempts", control.host)
		}
		alert := alerts[0]
		if alert.Identity() != "monitor/visibility|cannot-observe|observability/backup-archives|" ||
			alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable {
			t.Fatalf("%s changed the archive visibility identity or credited partial data", control.host)
		}
		want := "error_class=observation-timeout execution_boundaries=native-wait/started-joined backup_archive_stage=" + control.stage
		if alert.Observed != want {
			t.Errorf("%s timeout did not identify the failed archive read", control.host)
		}
		if !strings.Contains(err.Error(), "backup_archive_stage="+control.stage) {
			t.Errorf("%s direct error lost the failed read", control.host)
		}
		public, marshalErr := json.Marshal(alert)
		if marshalErr != nil || strings.Contains(string(public), private) || strings.Contains(alert.Markdown(), private) ||
			strings.Contains(err.Error(), private) {
			t.Errorf("%s returned private source text", control.host)
		}
	}
}

// A failed fallback attempt does not taint a successful query or a later writer failure.
func TestBackupArchivesObservationGatewayFallback(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	metrics := backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...)
	for _, writerErr := range []error{nil, context.DeadlineExceeded} {
		var calls []string
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			calls = append(calls, host.Name)
			switch host.Name {
			case "metrics-1":
				return metrics, &observationBoundaryError{boundary: observationBoundaryNativeWait, err: context.DeadlineExceeded}
			case "metrics-2":
				return metrics, nil
			case "backup-1":
				return backupArchiveWriterFixtureText(backupArchiveWriterFixture{}), writerErr
			default:
				t.Fatal("unexpected archive source")
				return "", nil
			}
		}}
		settings := backupArchiveSourceSettings(now, source)
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-2", Roles: []string{"services"}})
		alerts, err := NewWithSignals(settings, NewBackupArchivesSignal()).Run(context.Background())
		if strings.Join(calls, ",") != "metrics-1,metrics-2,backup-1" {
			t.Fatal("changed gateway fallback or writer attempts")
		}
		if writerErr == nil {
			if len(alerts) != 0 || err != nil {
				t.Fatal("recovered gateway produced a spurious archive failure")
			}
		} else if !errors.Is(err, writerErr) || len(alerts) != 1 ||
			alerts[0].Observed != "error_class=observation-timeout backup_archive_stage=writer-read" {
			t.Error("writer failure lost its stage or inherited the recovered gateway's native proof")
		}
	}
}

// The gateway can return a lifecycle error after an otherwise successful
// source call. Only an independently returned source failure supplies a stage.
func TestBackupArchivesObservationParentCancellation(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	const private = "synthetic-private-retired-source"
	for _, readErr := range []error{nil, errors.New(private)} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			calls++
			cancel()
			return private, readErr
		}}
		settings := backupArchiveSourceSettings(now, source)
		alerts, err := NewWithSignals(settings, NewBackupArchivesSignal()).Run(ctx)
		cancel()
		if calls != 1 || !errors.Is(err, context.Canceled) || len(alerts) != 1 {
			t.Fatal("retired owner lost cancellation or continued to the writer")
		}
		want := "error_class=observation-canceled"
		if readErr != nil {
			want += " backup_archive_stage=mimir-query"
			if !errors.Is(err, readErr) {
				t.Fatal("retired gateway lost its independently returned source cause")
			}
		}
		if alerts[0].Observed != want {
			t.Error("canceled archive observation attributed the wrong source authority")
		}
		requireAlertOmits(t, alerts[0], private)
	}
}

// An exceeded Mimir output bound retains the partial-finding path and prevents
// another gateway or writer call, even with a plausible prefix.
func TestBackupArchivesObservationOutputBound(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	const private = "synthetic-private-overflow-prefix"
	calls := 0
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls++
		return private, &observationBoundaryError{
			boundary: observationBoundaryNativeWait,
			err:      &mimirShellOutputError{stream: "stdout"},
		}
	}}
	settings := backupArchiveSourceSettings(now, source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-2", Roles: []string{"services"}})
	alerts, err := NewWithSignals(settings, NewBackupArchivesSignal()).Run(context.Background())
	if err != nil || calls != 1 || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
		t.Fatal("output overflow lost its partial visibility finding or retried the observation")
	}
	want := "error_class=observation-unclassified execution_boundaries=native-wait/started-joined backup_archive_stage=mimir-query source_phase=output-bound stream=stdout"
	if alerts[0].Observed != want {
		t.Error("gateway output bound lost its stage or existing stream and native evidence")
	}
	requireAlertOmits(t, alerts[0], private)
}

// A successful writer read that retires the owner cannot implicate the next writer.
func TestBackupArchivesObservationCanceledBeforeNextWriter(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	metrics := backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls = append(calls, host.Name)
		if command == backupArchiveWriterCommand {
			cancel()
			return backupArchiveWriterFixtureText(backupArchiveWriterFixture{}), nil
		}
		return metrics, nil
	}}
	settings := backupArchiveSourceSettings(now, source)
	for _, host := range settings.Hosts {
		if host.Name == "backup-1" {
			host.Name = "backup-2"
			settings.Hosts = append(settings.Hosts, host)
			break
		}
	}
	alerts, err := NewWithSignals(settings, NewBackupArchivesSignal()).Run(ctx)
	if strings.Join(calls, ",") != "metrics-1,backup-1" || !errors.Is(err, context.Canceled) ||
		len(alerts) != 1 || alerts[0].Observed != "error_class=observation-canceled" {
		t.Fatal("retirement after a completed read reached or implicated the next writer")
	}
}

// Only the owning read can supply a stage. Raw error text cannot forge one,
// and the existing SSH reset routing keeps its own class and target.
func TestBackupArchivesObservationUnrelatedErrors(t *testing.T) {
	signal := NewBackupArchivesSignal()
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	const private = "synthetic-private-forged-stage"
	settings := backupArchiveSourceSettings(now, &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return private, errors.New("kex_exchange_identification " + private)
	}})
	alerts, err := NewWithSignals(settings, signal).Run(context.Background())
	if err == nil || len(alerts) != 1 || alerts[0].Class != "ssh-admission-reset" || alerts[0].Target != "ssh-target" {
		t.Fatal("source attribution changed existing SSH reset routing")
	}
	requireAlertOmits(t, alerts[0], private)
	alert := visibilityAlert(settings, signal, errors.New("timeout backup_archive_stage=writer-read "+private))
	if alert.Observed != "error_class=observation-timeout" {
		t.Fatal("untyped failure spoofed an archive stage")
	}
	requireAlertOmits(t, alert, private)
}
