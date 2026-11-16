// Response-status controls keep current delivery evidence separate from old packaging incidents.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// An affirmative refusal is still a metadata failure, without identifying its producer.
func TestAssociationFilesSignalResponseFailureDoesNotInventBundleCause(t *testing.T) {
	for _, code := range []string{"429", "503"} {
		var calls atomic.Int32
		source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
			calls.Add(1)
			joined := strings.Join(args, " ")
			if name != "curl" || strings.Contains(joined, "--retry") || strings.Contains(joined, "--location") {
				return "", fmt.Errorf("unexpected synthetic association request")
			}
			if strings.Contains(joined, "/assetlinks.json") {
				return associationHTTPFixture(validAssetlinksFixture(), "200", "0", "application/json", "2001:db8:19::1"), nil
			}
			return associationHTTPFixture("<html>synthetic response refusal</html>", code, "0", "text/html", "2001:db8:19::1"), nil
		}}
		settings := associationSyntheticSettings(source, "2001:db8:19::1")
		settings.WebsiteDomain = "site.synthetic.example"
		settings.Hosts[0].Name = "synthetic-edge"
		alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("status %s lost response failure: alerts=%+v err=%v", code, alerts, err)
		}
		alert := requireAlertClass(t, alerts, "web-association-files")
		if alert.Identity() != "synthetic/web-association-files|web-association-files|site.synthetic.example|" || alert.Severity != SeverityWarn || alert.Sustain != 1 || calls.Load() != 2 {
			t.Errorf("status %s changed identity, severity, sustain, or request count: alert=%+v calls=%d", code, alert, calls.Load())
		}
		if alert.Observed != "checked=2 failed=1 missing_404=0 invalid_contract=0 other_response_failures=1" {
			t.Errorf("status %s lost status-before-content classification: %s", code, alert.Observed)
		}
		if !strings.Contains(alert.Mechanism, "responding component") || !strings.Contains(alert.Action, "upstream") {
			t.Errorf("status %s invented a bundle cause instead of retaining responder uncertainty: %+v", code, alert)
		}
		for _, unsupported := range []string{"deploy one web generation", "Build and deploy", "production build used", "tree omitted"} {
			if strings.Contains(alert.Action+alert.Mechanism, unsupported) {
				t.Errorf("status %s retained unsupported remediation %q", code, unsupported)
			}
		}
	}
}

// A current missing path does not re-prove the historical dotfile staging cause.
func TestAssociationFilesSignalMissingPathDoesNotReplayHistoricalCause(t *testing.T) {
	source := &syntheticSource{localFn: func(string, ...string) (string, error) {
		return associationHTTPFixture("<html>synthetic missing route</html>", "404", "0", "text/html", "2001:db8:19::2"), nil
	}}
	settings := associationSyntheticSettings(source, "2001:db8:19::2")
	settings.WebsiteDomain = "site.synthetic.example"
	settings.Hosts[0].Name = "synthetic-edge"
	alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("missing metadata failure lost: alerts=%+v err=%v", alerts, err)
	}
	alert := requireAlertClass(t, alerts, "web-association-files")
	if !strings.Contains(alert.Observed, "missing_404=2") || !strings.Contains(alert.Action, "running artifact") || !strings.Contains(alert.Action, "historical") {
		t.Errorf("404 omitted current artifact and historical-cause qualification: %+v", alert)
	}
	for _, unsupported := range []string{"production build used", "web tree omitted", "legitimately returned 404 from every generation"} {
		if strings.Contains(alert.Mechanism, unsupported) {
			t.Errorf("404 reasserted historical cause %q", unsupported)
		}
	}
}
