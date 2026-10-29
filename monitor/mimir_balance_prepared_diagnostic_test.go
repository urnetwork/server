// Prepared balance diagnostics exercise the public signal and each public
// serialization with synthetic failures; no source error becomes alert text.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Check all public representations, not only the reducer's private enum.
func requireMimirBalancePreparedOutput(t *testing.T, alert Alert, source, class, boundary string) {
	t.Helper()
	requireMimirBalanceDiagnostic(t, alert, "metrics-a.invalid/mimir-balance", mimirBalanceStageHostObservationFailed, class)
	encoded, err := json.Marshal(alert)
	if err != nil {
		t.Fatal("synthetic alert encoding failed")
	}
	var jsonl strings.Builder
	if err := (Alerts{alert}).WriteJSONL(&jsonl); err != nil {
		t.Fatal("synthetic alert JSONL failed")
	}
	for _, output := range []string{string(encoded), alert.Markdown(), jsonl.String()} {
		if !strings.Contains(output, "observation_source="+source) {
			t.Error("public output lost the owned source branch")
		}
		if boundary == "" && strings.Contains(output, "execution_boundaries=") {
			t.Error("public output manufactured an execution boundary")
		}
		if boundary != "" && !strings.Contains(output, "execution_boundaries="+boundary) {
			t.Error("public output lost the owned execution boundary")
		}
		if strings.Contains(output, "PREPARED-PRIVATE-CANARY") || strings.Contains(output, "PRIVATE-ARGV-CANARY") {
			t.Error("public output leaked a private typed class or command")
		}
	}
}

// Real public command failures preserve finite classes and typed native proof;
// malformed typed classes and plain-text marker lookalikes stay unclassified.
func TestMimirBalancePreparedCommandDiagnosticsPublic(t *testing.T) {
	for _, test := range []struct {
		name     string
		failure  error
		class    string
		boundary string
	}{
		{"deadline", &observationBoundaryError{err: context.DeadlineExceeded, boundary: observationBoundaryNativeWait}, observationErrorClassTimeout, "native-wait/started-joined"},
		{"canceled", context.Canceled, observationErrorClassCanceled, ""},
		{"typed-private", &warpctlLogQueryError{class: "PREPARED-PRIVATE-CANARY\nPRIVATE-ARGV-CANARY", err: errors.New(mimirBalanceDiagnosticPrivate)}, observationErrorClassUnclassified, ""},
		{"typed-prefix", &warpctlLogQueryError{class: observationErrorClassTimeout + "PREPARED-PRIVATE-CANARY"}, observationErrorClassUnclassified, ""},
		{"typed-whitespace", &warpctlLogQueryError{class: " " + observationErrorClassTimeout}, observationErrorClassUnclassified, ""},
		{"typed-empty", &warpctlLogQueryError{}, observationErrorClassUnclassified, ""},
		{"typed-unknown-log-class", &warpctlLogQueryError{class: observationErrorClassLogHttp429}, observationErrorClassUnclassified, ""},
		{"typed-known-marker-spoof", &warpctlLogQueryError{class: observationErrorClassTimeout, err: errors.New("native-wait/started-joined PREPARED-PRIVATE-CANARY")}, observationErrorClassTimeout, ""},
		{"typed-proof-private-class", &observationBoundaryError{err: &warpctlLogQueryError{class: "PREPARED-PRIVATE-CANARY"}, boundary: observationBoundaryNativeGuard}, observationErrorClassUnclassified, "native-guard/not-attempted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2033, 2, 3, 4, 5, 0, 0, time.UTC)
			runMimirBalanceDiagnostic(t, dir, now, mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0), nil)
			before := mimirBalanceDiagnosticState(t, dir)
			alerts := runMimirBalanceDiagnostic(t, dir, now.Add(time.Minute), mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 1), map[string]error{"metrics-a.invalid": test.failure})
			found := 0
			for _, alert := range alerts {
				if alert.Target == "metrics-a.invalid/mimir-balance" {
					found++
					requireMimirBalancePreparedOutput(t, alert, "host-command", test.class, test.boundary)
				}
			}
			if found != 1 {
				t.Fatal("public signal omitted the one failed target")
			}
			after := mimirBalanceDiagnosticState(t, dir)
			for index, history := range after.Histories {
				if history.Host == "metrics-a.invalid" {
					if !reflect.DeepEqual(history, before.Histories[index]) {
						t.Error("failed source changed its prior history")
					}
				} else if history.ObservedUnixNS != now.Add(time.Minute).UnixNano() {
					t.Error("failed source prevented valid sibling history progress")
				}
			}
		})
	}
}

// Parser and discovery failures share a stage but have distinct owner origins;
// neither may inherit command proof or manufacture a complete fleet verdict.
func TestMimirBalancePreparedParserDiscoveryPublic(t *testing.T) {
	for _, test := range []struct{ name, frame, source, class string }{
		{"parser", "PREPARED-PRIVATE-CANARY PRIVATE-ARGV-CANARY malformed", "host-parser", observationErrorClassInvalidResponse},
		{"discovery", "source_incomplete 1\nmimir_count 0\nfront_local_connections 0\nfront_remote_connections 0\n", "child-discovery", observationErrorClassUnclassified},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2033, 2, 3, 4, 5, 0, 0, time.UTC)
			frames := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0)
			runMimirBalanceDiagnostic(t, dir, now, frames, nil)
			frames = mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 1)
			frames["metrics-a.invalid"] = test.frame
			alerts := runMimirBalanceDiagnostic(t, dir, now.Add(time.Minute), frames, nil)
			found := 0
			for _, alert := range alerts {
				if alert.Target == "metrics-a.invalid/mimir-balance" {
					found++
					requireMimirBalancePreparedOutput(t, alert, test.source, test.class, "")
				}
				if alert.Class == "mimir-distributor-skew" {
					t.Error("unavailable source forged fleet skew")
				}
			}
			if found != 1 {
				t.Error("public parser/discovery failure lost its target")
			}
		})
	}
}

// Any attempt to inspect a raw error after preparation is a control failure.
type mimirBalanceDiagnosticForbiddenError struct{}

// Formatting this sentinel proves the fixed-helper boundary was violated.
func (mimirBalanceDiagnosticForbiddenError) Error() string {
	panic("original error reached finding helper")
}

// Every fixed class and native proof has a closed representation, independently
// of the public fixture's selected examples. No raw string is retained.
func TestMimirBalancePreparedFiniteDomain(t *testing.T) {
	if len(mimirBalanceDiagnosticClasses) != 14 || len(mimirBalanceDiagnosticBoundaries) != 9 {
		t.Fatal("owned diagnostic domain changed without its contract")
	}
	for _, class := range mimirBalanceDiagnosticClasses {
		prepared := mimirBalancePrepareCommandDiagnostic(&warpctlLogQueryError{class: class})
		got, details := prepared.details()
		if got != class || !strings.Contains(details, "error_class="+class) {
			t.Error("known fixed class did not round trip")
		}
	}
	for index, name := range mimirBalanceDiagnosticBoundaries {
		prepared := mimirBalancePrepareCommandDiagnostic(&observationBoundaryError{err: errors.New("PREPARED-PRIVATE-CANARY"), boundary: observationBoundary(index)})
		_, details := prepared.details()
		if !strings.Contains(details, "execution_boundaries="+name) || strings.Contains(details, "PREPARED-PRIVATE-CANARY") {
			t.Error("fixed native proof did not round trip privately")
		}
	}
}

// The direct owner renderer never passes a command error to the shared helper.
// Corrupt prepared values fail closed without rendering numbers or raw values.
func TestMimirBalancePreparedRendererRejectsMalformedValues(t *testing.T) {
	for _, test := range []struct {
		name                    string
		diagnostic              mimirBalancePreparedDiagnostic
		source, class, boundary string
	}{
		{"legacy", mimirBalancePreparedDiagnostic{}, "unknown", observationErrorClassUnclassified, ""},
		{"origin", mimirBalancePreparedDiagnostic{source: 255, class: 1, boundaries: 64}, "unknown", observationErrorClassUnclassified, ""},
		{"class", mimirBalancePreparedDiagnostic{source: mimirBalanceSourceCommand, class: 255}, "host-command", observationErrorClassUnclassified, ""},
		{"bits", mimirBalancePreparedDiagnostic{source: mimirBalanceSourceCommand, boundaries: 1 << 15}, "host-command", observationErrorClassUnclassified, "unknown/unknown"},
		{"parser-proof", mimirBalancePreparedDiagnostic{source: mimirBalanceSourceParser, class: 1, boundaries: 64}, "host-parser", observationErrorClassInvalidResponse, ""},
		{"discovery-proof", mimirBalancePreparedDiagnostic{source: mimirBalanceSourceDiscovery, class: 1, boundaries: 64}, "child-discovery", observationErrorClassUnclassified, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			finding := mimirBalanceCannotObserveFinding(mimirBalanceVisibilityFailure{target: "metrics-a.invalid/mimir-balance", stage: mimirBalanceStageHostObservationFailed, err: mimirBalanceDiagnosticForbiddenError{}, diagnostic: test.diagnostic})
			if !strings.Contains(finding.observed, "observation_source="+test.source) || !strings.Contains(finding.observed, "error_class="+test.class) {
				t.Error("invalid prepared diagnostic escaped its fixed fallback")
			}
			if test.boundary == "" && strings.Contains(finding.observed, "execution_boundaries=") || test.boundary != "" && !strings.Contains(finding.observed, "execution_boundaries="+test.boundary) {
				t.Error("prepared proof was forged or lost")
			}
			if strings.Contains(fmt.Sprint(finding.observed), "255") || strings.Contains(finding.observed, "32768") {
				t.Error("invalid diagnostic value was exposed")
			}
		})
	}
}
