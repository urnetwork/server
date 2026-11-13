// The legacy port-exhaustion bucket measures diagnostic text at rate. These
// controls retain its positive and healthy boundaries while refusing a native
// resource diagnosis or recovery promise from an untyped log exemplar.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Exercise the public log-window probe and renderer with one synthetic stream;
// partial output plus an error must remain an unavailable observation.
func portSignatureTestAlerts(t *testing.T, lines string, sourceErr error) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-service", nil
		}
		return lines, sourceErr
	}}
	settings := syntheticSettings(source)
	settings.LogServices = []string{"synthetic-service"}
	if settings.Validate() != nil {
		t.Fatal("synthetic signature fixture has invalid settings scope")
	}
	return NewLogErrorsSignal().Run(context.Background(), settings)
}

// A harmless quotation and an ordinary diagnostic both match the legacy text
// rule. Neither fixture supplies native emitter, resource or attempt evidence.
func TestPortExhaustionSignatureDoesNotDiagnoseNativeCauseOrRecovery(t *testing.T) {
	for _, line := range []string{
		"connect: cannot assign requested address",
		"synthetic relay quoted example='connect: cannot assign requested address'",
	} {
		alerts, err := portSignatureTestAlerts(t, strings.Repeat(line+"\n", 100), nil)
		if err != nil {
			t.Fatal("synthetic completed log window failed")
		}
		alert := requireAlertClass(t, alerts, "port-exhaustion")
		if alert.SignalID != "logs/port-exhaustion" || alert.Severity != SeverityWarn || alert.Target != "synthetic-service" || alert.Sustain != 1 ||
			!strings.Contains(alert.Observed, "rate=100/min class=port-exhaustion") {
			t.Fatal("signature correction changed positive identity, scope or threshold")
		}
		markdown := alert.Markdown()
		for _, required := range []string{"not unique connection attempts", "not a measured diagnosis", "configured log selector", "not a destination bucket", "no fixed self-drain interval", "Falling or missing log counts alone", "counts do not prove retries"} {
			if !strings.Contains(markdown, required) {
				t.Errorf("signature omitted authority limit %q", required)
			}
		}
		for _, unsupported := range []string{"redial storm to one destination", "self-drains ~60s", "fix the target node", "storm self-drains", "volume is retry amplification"} {
			if strings.Contains(strings.ToLower(markdown), unsupported) {
				t.Errorf("signature retained unsupported claim %q", unsupported)
			}
		}
	}
}

// Equality at 100 is already the WARN boundary. A later empty window resets
// the diagnostic rate, without supplying any native allocation-health proof.
func TestPortExhaustionSignaturePreservesThresholdAndWindowScope(t *testing.T) {
	for _, count := range []int{0, 99, 100, 101} {
		tailer := newLogTailer("synthetic-service", nil)
		for range count {
			tailer.classify("connect: cannot assign requested address")
		}
		finding := findingByClass(t, tailer.drainWindow(), "port-exhaustion")
		if finding.healthy != (count < 100) {
			t.Fatalf("signature changed rate boundary at synthetic count %d", count)
		}
		if count >= 100 && (finding.tier != tierWarn || !strings.Contains(finding.observed, fmt.Sprintf("rate=%d/min", count))) {
			t.Fatal("positive diagnostic volume lost WARN or exact count")
		}
		if !findingByClass(t, tailer.drainWindow(), "port-exhaustion").healthy {
			t.Fatal("prior window count survived a drain")
		}
	}
	for _, other := range []string{"bind: cannot assign requested address", "connect: connection refused", "synthetic allocation healthy"} {
		tailer := newLogTailer("synthetic-service", nil)
		for range 100 {
			tailer.classify(other)
		}
		if !findingByClass(t, tailer.drainWindow(), "port-exhaustion").healthy {
			t.Fatal("unmatched text gained the legacy port signature")
		}
	}
}

// The existing first-address frame is preserved as a hint. Mixed destination
// text still shares one class count and cannot become a native owner join.
func TestPortExhaustionSignatureFirstHintDoesNotDefineDestinationBucket(t *testing.T) {
	tailer := newLogTailer("synthetic-service", nil)
	for range 50 {
		tailer.classify("metadata=192.0.2.44:9000 dial tcp 192.0.2.10:443: connect: cannot assign requested address")
	}
	for range 50 {
		tailer.classify("dial tcp 192.0.2.20:8443: connect: cannot assign requested address")
	}
	finding := findingByClass(t, tailer.drainWindow(), "port-exhaustion")
	if finding.healthy || finding.target != "synthetic-service" || finding.frame != "192.0.2.44:9000" || !strings.Contains(finding.observed, "rate=100/min") {
		t.Fatal("signature changed legacy first-hint or aggregate-count semantics")
	}
	alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
	if !strings.Contains(alert.Context, "mixed-destination") || !strings.Contains(alert.Context, "not a destination bucket") {
		t.Fatal("mixed textual destinations became an authoritative single target")
	}
}

// A failed bounded collector with a tempting positive partial body cannot
// authorize either a completed count or recovery from that source window.
func TestPortExhaustionSignatureUnavailableWindowCannotReportRecovery(t *testing.T) {
	alerts, err := portSignatureTestAlerts(t, strings.Repeat("connect: cannot assign requested address\n", 100), errors.New("synthetic-private-source-canary"))
	if err == nil || len(alerts) != 0 {
		t.Fatal("partial failed source became a completed log observation")
	}
}

// The numbered owner and taxonomy must retain unknown cause and the inclusive
// threshold rather than restoring the old target-fix/self-drain recipe.
func TestPortExhaustionSignatureCatalogKeepsNativeAuthorityUnknown(t *testing.T) {
	raw, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal("owning catalog unavailable")
	}
	catalog := string(raw)
	for _, required := range []string{"`port-exhaustion` retains its legacy class name", "native allocation authority", "No fixed tuple budget or self-drain period", "first exemplar's textual address hint", ">= 100/min, WARN"} {
		if !strings.Contains(catalog, required) {
			t.Errorf("catalog omitted signature boundary %q", required)
		}
	}
	for _, unsupported := range []string{"CLIENT-side ephemeral-port exhaustion (redial storm to one dst)", "storm self-drains ≤60s", "680 sustainable dials/sec per destination"} {
		if strings.Contains(catalog, unsupported) {
			t.Errorf("catalog retained unsupported attribution %q", unsupported)
		}
	}
}
