// Retain prior HMAC incidents through incomplete compatibility controls.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The actual signal, cadence gate and notifier share one unchanged legacy
// cohort. Handled-run barriers make absence checks independent of scheduling.
func TestHmacCutoverNotifierRequiresCompleteRecovery(t *testing.T) {
	const privateMarker = "synthetic-hmac-recovery-private-marker"
	modes := []string{"page", "readiness", "readiness", "schema", "malformed", "source", "page", "healthy", "healthy"}
	step := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "monitor-schema-readiness") {
			if modes[step] == "schema" {
				return []Row{{"false"}}, nil
			}
			return []Row{{"true"}}, nil
		}
		if !strings.Contains(query, "monitor-signal-2.24-hmac-cutover") {
			return nil, errors.New("HMAC recovery control reached an unrelated query")
		}
		if modes[step] == "malformed" {
			return []Row{{privateMarker}}, nil
		}
		if modes[step] == "source" {
			return nil, errors.New(privateMarker)
		}
		row := Row{"t", "86400", "300", "100", "2", "100", "100", "100", "100", "0", "100", "50", "50"}
		if modes[step] == "readiness" {
			row[11], row[12] = "51", "49"
		}
		if modes[step] == "healthy" {
			row = Row{"t", "86400", "100", "0", "0", "100", "0", "0", "0", "0", "100", "0", "100"}
		}
		return []Row{row}, nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifierSettings := harness.settings(webhookUrl)
	notifierSettings.ClearAfterCompleteRuns = 2
	notifier := NewSlackAlertNotifier(notifierSettings)
	incompatibleKey := "pg/hmac-cutover|contract-hmac-incompatible|provider-fleet"
	readinessKey := "pg/hmac-cutover|contract-hmac-readiness|provider-fleet"
	postSignalRunsThroughTheGate(t, NewHMACCutoverSignal(), syntheticSettings(source), len(modes), func(run int) {
		if run > 0 {
			notifier.stateLock.Lock()
			incident, retained := notifier.keyIncidents[incompatibleKey]
			absent := -1
			if retained {
				absent = incident.signalKeyAbsentRuns["hmac-cutover"]
			}
			_, readinessRetained := notifier.keyIncidents[readinessKey]
			notifier.stateLock.Unlock()
			wantAbsent := 0
			if run == len(modes)-1 {
				wantAbsent = 1
			}
			// Wait for both readiness samples before proving incident deletion.
			if run != 2 && (!retained || absent != wantAbsent) {
				t.Fatal("incomplete compatibility evidence advanced absence or cleared the prior HMAC incident")
			}
			if run == 3 && !readinessRetained {
				t.Fatal("unknown compatibility recovery suppressed the independent readiness warning")
			}
		}
		step = run
	}, notifier)
	closeSlackNotifier(t, notifier)
	notifier.stateLock.Lock()
	_, retained := notifier.keyIncidents[incompatibleKey]
	notifier.stateLock.Unlock()
	if retained {
		t.Fatal("two complete no-legacy runs did not clear the monitor incident")
	}
	texts := webhook.posted()
	identity := "pg/hmac-cutover · contract-hmac-incompatible · provider-fleet"
	if countSlackPosts(texts, "[PAGE] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
		t.Fatal("HMAC uncertainty cleared or re-raised the incident before complete recovery")
	}
	readinessIdentity := "pg/hmac-cutover · contract-hmac-readiness · provider-fleet"
	if countSlackPosts(texts, "[WARN] raised", readinessIdentity) != 1 {
		t.Fatal("readiness warning lost its independent notifier identity")
	}
	if strings.Contains(strings.Join(append(texts, harness.logLines()...), "\n"), privateMarker) {
		t.Fatal("private HMAC source bytes entered notifier output")
	}
}

// Readiness keeps its public identity and thresholds while withholding
// incompatible-recovery authority; complete no-legacy observations stay quiet.
func TestHmacCutoverReadinessRetainsPositiveAndUnknownAuthority(t *testing.T) {
	for _, mode := range []string{"compatible-below-half", "compatible-missing", "legacy-partial", "before-cutover", "page", "healthy"} {
		row := Row{"t", "86400", "300", "100", "2", "100", "100", "100", "100", "0", "100", "51", "49"}
		switch mode {
		case "compatible-missing":
			row[10], row[11], row[12] = "0", "0", "0"
		case "legacy-partial":
			row[7], row[8] = "19", "19"
		case "before-cutover":
			row[0], row[1] = "f", "-1"
		case "page":
			row[11], row[12] = "50", "50"
		case "healthy":
			row = Row{"t", "86400", "100", "0", "0", "100", "0", "0", "0", "0", "100", "0", "100"}
		}
		alerts, err := NewHMACCutoverSignal().Run(context.Background(), syntheticSettings(syntheticHMACCutoverSource(row)))
		if err != nil {
			t.Fatal("valid HMAC source control failed")
		}
		if mode == "healthy" {
			if len(alerts) != 0 {
				t.Fatal("complete no-legacy control became unknown")
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatal("HMAC recovery qualification changed current-sample detection counts")
		}
		if mode == "page" {
			if alerts[0].Class != "contract-hmac-incompatible" || alerts[0].Severity != SeverityPage || alerts[0].Sustain != 1 || alerts[0].unobservable {
				t.Fatal("exact compatible threshold changed the causal PAGE")
			}
			continue
		}
		alert := alerts[0]
		if alert.Class != "contract-hmac-readiness" || alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable || alert.SignalKey != "hmac-cutover" || alert.Target != "provider-fleet" || alert.Frame != "legacy-linux" {
			t.Fatal("readiness lost its positive warning or unknown recovery authority")
		}
		if !strings.Contains(alert.Observed, "incompatible_recovery_observable=false") || !strings.Contains(alert.Observed, "claimed_legacy=100") {
			t.Fatal("readiness omitted its recovery qualifier or original population")
		}
		encoded, err := json.Marshal(alert)
		if err != nil || !strings.Contains(string(encoded), "incompatible_recovery_observable=false") || !strings.Contains(alert.Markdown(), "incompatible_recovery_observable=false") {
			t.Fatal("public HMAC rendering lost the recovery qualifier")
		}
	}
}

// Cancellation remains a lifecycle failure and supplies no recovery.
func TestHmacCutoverRecoveryCancellationRemainsLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, context.Canceled
	}}
	alerts, err := NewHMACCutoverSignal().Run(ctx, syntheticSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("canceled HMAC observation supplied incident recovery evidence")
	}
}
