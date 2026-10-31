// Signup-quality source and aggregate failures must remain distinguishable
// through the public constructor and visibility formatter without raw evidence.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Every source call is synthetic. A failed source must not become a cohort.
func TestSignupQualityObservationFailureStages(t *testing.T) {
	const private = "synthetic-signup-private-error-and-row"
	sourceCause := errors.New(private)
	nativeTimeout := &observationBoundaryError{err: fmt.Errorf("%s: %w", private, context.DeadlineExceeded), boundary: observationBoundaryNativeWait}
	for _, control := range []struct {
		name       string
		rows       []Row
		sourceErr  error
		wantClass  string
		wantStage  string
		wantNative bool
	}{
		{name: "source-unclassified", rows: []Row{{"2026-08-24", "200", "100"}}, sourceErr: sourceCause, wantClass: "observation-unclassified", wantStage: "source-read"},
		{name: "source-timeout", sourceErr: nativeTimeout, wantClass: "observation-timeout", wantStage: "source-read", wantNative: true},
		{name: "source-csv-parser", sourceErr: fmt.Errorf("parse PostgreSQL CSV output: %w", sourceCause), wantClass: "observation-invalid-response", wantStage: "source-read"},
		{name: "aggregate-missing", wantClass: "observation-invalid-response", wantStage: "aggregate-parse"},
		{name: "aggregate-private-day", rows: []Row{{private, "200", "100"}}, wantClass: "observation-invalid-response", wantStage: "aggregate-parse"},
		{name: "aggregate-private-count", rows: []Row{{"2026-08-24", private, "100"}}, wantClass: "observation-invalid-response", wantStage: "aggregate-parse"},
		{name: "aggregate-contradictory", rows: []Row{{"2026-08-24", "200", "300"}}, wantClass: "observation-invalid-response", wantStage: "aggregate-parse"},
	} {
		t.Run(control.name, func(t *testing.T) {
			calls := 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				calls++
				if query != signupQualityQuery {
					t.Fatal("provenance changed the owning query")
				}
				return control.rows, control.sourceErr
			}}
			settings := syntheticSettings(source)
			signal := NewSignupQualitySignal()
			alerts, runErr := NewWithSignals(settings, signal).Run(context.Background())
			if runErr == nil || len(alerts) != 1 || calls != 1 {
				t.Fatal("failed observation became a cohort or changed source attempts")
			}
			if control.sourceErr != nil && !errors.Is(runErr, control.sourceErr) {
				t.Fatal("signup failure lost its original source cause")
			}
			partial := alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding(signal.ID(), runErr))
			for _, alert := range []Alert{alerts[0], partial} {
				if alert.Identity() != "monitor/visibility|cannot-observe|pg/signup-quality|" ||
					alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable {
					t.Fatal("provenance changed visibility identity, severity or unknown status")
				}
				if !strings.Contains(alert.Observed, "error_class="+control.wantClass) ||
					!strings.Contains(alert.Observed, "signup_quality_stage="+control.wantStage) {
					t.Fatal("owning source/aggregate failure lacks its safe class and stage")
				}
				if strings.Contains(alert.Observed, "execution_boundaries=") != control.wantNative {
					t.Fatal("signup stage invented or lost native execution proof")
				}
				if control.wantNative && !strings.Contains(alert.Observed, "execution_boundaries=native-wait/started-joined") {
					t.Fatal("native source boundary changed")
				}
				encoded, err := json.Marshal(alert)
				if err != nil {
					t.Fatal("public alert did not serialize")
				}
				if strings.Contains(string(encoded), private) || strings.Contains(alert.Markdown(), private) {
					t.Fatal("public observation leaked original source or row content")
				}
			}
			if strings.Contains(runErr.Error(), private) {
				t.Fatal("signup wrapper exposed private source text")
			}
		})
	}
}

// An ended live owner still wins while a separately failed read retains custody.
func TestSignupQualityObservationLifecycleAndNativeCause(t *testing.T) {
	const private = "synthetic-signup-private-lifecycle"
	for _, failedSource := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		sourceCause := &observationBoundaryError{err: errors.New(private), boundary: observationBoundaryNativeWait}
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			cancel()
			if failedSource {
				return nil, sourceCause
			}
			return []Row{{"2026-08-24", "200", "1"}}, nil
		}}
		settings := syntheticSettings(source)
		alerts, runErr := NewWithSignals(settings, NewSignupQualitySignal()).Run(ctx)
		cancel()
		if !errors.Is(runErr, context.Canceled) || len(alerts) != 1 || alerts[0].Class != "cannot-observe" ||
			!strings.Contains(alerts[0].Observed, "error_class=observation-canceled") {
			t.Fatal("ended owner published a cohort or lost cancellation authority")
		}
		if failedSource {
			if !errors.Is(runErr, sourceCause) || !strings.Contains(alerts[0].Observed, "signup_quality_stage=source-read") ||
				!strings.Contains(alerts[0].Observed, "execution_boundaries=native-wait/started-joined") {
				t.Fatal("lifecycle wrapper lost the independently returned source boundary")
			}
		} else if strings.Contains(alerts[0].Observed, "signup_quality_stage=") || strings.Contains(alerts[0].Observed, "execution_boundaries=") {
			t.Fatal("successful read followed by cancellation acquired a failed source stage")
		}
		requireAlertOmits(t, alerts[0], private)
	}
}

// Complete cohort outcomes retain their old policy and stable incident token.
func TestSignupQualityObservationHealthyAndLowShareCounterexamples(t *testing.T) {
	for _, control := range []struct {
		rows    []Row
		wantLow bool
	}{
		{rows: []Row{{"2026-08-24", "200", "150"}}},
		{rows: []Row{{"2026-08-24", "200", "100"}}},
		{rows: []Row{{"2026-08-24", "40", "1"}}},
		{rows: []Row{{"2026-08-24", "0", "0"}}},
		{rows: []Row{{"2026-08-24", "200", "99"}}, wantLow: true},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return control.rows, nil }}
		alerts, err := NewWithSignals(syntheticSettings(source), NewSignupQualitySignal()).Run(context.Background())
		if err != nil || (!control.wantLow && len(alerts) != 0) || (control.wantLow && len(alerts) != 1) {
			t.Fatal("source provenance changed an existing complete cohort outcome")
		}
		if control.wantLow && (alerts[0].Class != "signup-quality" || alerts[0].Frame != signupQualityLegacyIdentityFrame ||
			alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 1 || strings.Contains(alerts[0].Observed, "signup_quality_stage=")) {
			t.Fatal("complete low-share cohort acquired visibility fields or a new identity")
		}
	}
}

// Only the owning typed failure may add a stage; raw text cannot forge one.
func TestSignupQualityObservationStageRequiresTypedProof(t *testing.T) {
	settings := syntheticSettings(nil)
	alert := visibilityAlert(settings, NewSignupQualitySignal(), errors.New("signup_quality_stage=aggregate-parse synthetic-private-stage"))
	if strings.Contains(alert.Observed, "signup_quality_stage=") {
		t.Fatal("untyped error text invented a signup source stage")
	}
	requireAlertOmits(t, alert, "synthetic-private-stage")
}
