// Public-constructor controls keep untrusted association response fields
// private while retaining independent metadata failures and edge unknowns.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Check every serialized field, identity and public rendering without ever
// printing a forbidden source value when the regression fails.
func requireAssociationPublicPrivacy(t *testing.T, alerts Alerts, forbidden ...string) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal("association alerts could not be encoded")
	}
	var lines strings.Builder
	if err := alerts.WriteJSONL(&lines); err != nil {
		t.Fatal("association alerts could not be rendered as JSONL")
	}
	outputs := []string{string(encoded), lines.String(), alerts.Markdown()}
	for _, alert := range alerts {
		outputs = append(outputs, alert.Identity())
	}
	for _, output := range outputs {
		for _, value := range forbidden {
			if value != "" && strings.Contains(output, value) {
				t.Error("association public output retained a synthetic private source value")
			}
		}
	}
}

// A genuine failed platform document must retain a public fixed finding,
// without reflecting arbitrary response bodies, errors or metadata fields.
func TestAssociationFilesSignalPublicResponsePrivacy(t *testing.T) {
	address := "2001:db8:63::1"
	for _, test := range []struct {
		name        string
		body        string
		status      string
		exit        string
		contentType string
		requestErr  error
		canary      string
	}{
		{name: "body-404", body: "synthetic-association-body-404-secret", status: "404", exit: "0", contentType: "text/html", canary: "synthetic-association-body-404-secret"},
		{name: "body-429", body: "synthetic-association-body-429-secret", status: "429", exit: "0", contentType: "text/html", canary: "synthetic-association-body-429-secret"},
		{name: "invalid-contract", body: `[{"synthetic_private":"synthetic-association-invalid-secret"}]`, status: "200", exit: "0", contentType: "application/json", canary: "synthetic-association-invalid-secret"},
		{name: "native-error", body: validAssetlinksFixture(), status: "200", exit: "0", contentType: "application/json", requestErr: errors.New("synthetic-association-error-secret"), canary: "synthetic-association-error-secret"},
		{name: "content-type", body: validAssetlinksFixture(), status: "200", exit: "0", contentType: "application/synthetic-association-type-secret", canary: "synthetic-association-type-secret"},
		{name: "http-field", body: validAssetlinksFixture(), status: "synthetic-association-http-secret", exit: "0", contentType: "application/json", canary: "synthetic-association-http-secret"},
		{name: "exit-field", body: validAssetlinksFixture(), status: "200", exit: "synthetic-association-exit-secret", contentType: "application/json", canary: "synthetic-association-exit-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
				if name != "curl" {
					return "", errors.New("unexpected synthetic association command")
				}
				if strings.Contains(strings.Join(args, " "), "/.well-known/assetlinks.json") {
					return associationHTTPFixture(test.body, test.status, test.exit, test.contentType, address), test.requestErr
				}
				return associationHTTPFixture(validAppleAssociationFixture(), "200", "0", "application/json", address), nil
			}}
			alerts, err := NewAssociationFilesSignal().Run(context.Background(), associationSyntheticSettings(source, address))
			if err != nil || len(alerts) != 1 {
				t.Fatal("failed metadata did not retain exactly one public association finding")
			}
			alert := alerts[0]
			if alert.Class != "web-association-files" || alert.SignalID != "synthetic/web-association-files" ||
				alert.SignalKey != "association-files" || alert.SignalNumber != "19.1" || alert.Target != "ur.io" ||
				alert.Frame != "" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
				t.Fatal("response projection changed the existing association identity or severity")
			}
			requireAssociationPublicPrivacy(t, alerts, test.canary)
		})
	}
}

// Valid platform contracts still complete with no public product or
// visibility finding; the privacy regression must not disable acquisition.
func TestAssociationFilesSignalPrivacyHealthyControl(t *testing.T) {
	address := "2001:db8:63::2"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected synthetic association command")
		}
		body := validAppleAssociationFixture()
		if strings.Contains(strings.Join(args, " "), "/.well-known/assetlinks.json") {
			body = validAssetlinksFixture()
		}
		return associationHTTPFixture(body, "200", "0", "application/json", address), nil
	}}
	alerts, err := NewAssociationFilesSignal().Run(context.Background(), associationSyntheticSettings(source, address))
	if err != nil || len(alerts) != 0 {
		t.Fatal("valid synthetic association contracts did not remain healthy")
	}
}

// Independent real metadata failures remain actionable beside a dead edge;
// the latter stays unknown rather than becoming a second product failure.
func TestAssociationFilesSignalPrivacyMixedFailureAndUnknown(t *testing.T) {
	failed, unknown := "2001:db8:63::3", "2001:db8:63::4"
	const bodyCanary = "synthetic-association-mixed-body-secret"
	const errorCanary = "synthetic-association-mixed-transport-secret"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected synthetic association command")
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "["+unknown+"]") {
			return associationHTTPFixture("", "000", "28", "", unknown), errors.New(errorCanary)
		}
		if strings.Contains(joined, "/.well-known/assetlinks.json") {
			return associationHTTPFixture(bodyCanary, "404", "0", "text/html", failed), nil
		}
		return associationHTTPFixture(validAppleAssociationFixture(), "200", "0", "application/json", failed), nil
	}}
	alerts, err := NewAssociationFilesSignal().Run(context.Background(), associationSyntheticSettings(source, failed, unknown))
	if err != nil || len(alerts) != 2 {
		t.Fatal("mixed source did not preserve one metadata failure and one unknown coverage finding")
	}
	product, visibility := 0, 0
	for _, alert := range alerts {
		switch alert.Class {
		case "web-association-files":
			product++
			if alert.Target != "ur.io" || alert.Severity != SeverityWarn || alert.Sustain != 1 ||
				!strings.Contains(alert.Observed, "missing_404=1") || !strings.Contains(alert.Observed, "failed=1") {
				t.Fatal("mixed metadata failure lost its fixed identity or exact failed-document count")
			}
		case "cannot-observe":
			visibility++
			if !alert.unobservable || !strings.Contains(alert.Observed, "unobserved=2") {
				t.Fatal("dead edge was presented as observed association metadata")
			}
		default:
			t.Fatal("mixed association source introduced an unexpected public class")
		}
	}
	if product != 1 || visibility != 1 {
		t.Fatal("mixed source lost the independent failure or unknown")
	}
	requireAssociationPublicPrivacy(t, alerts, bodyCanary, errorCanary)
}
