// Status-only failures cannot authorize a bundle rollout or cache invalidation.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// Public and exact-edge refusals have distinct controls but neither locates its producer.
func TestEmailAssetsSignalResponseFailureDoesNotInventRepair(t *testing.T) {
	for _, failingScope := range []string{"edge", "public"} {
		var calls atomic.Int32
		source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
			calls.Add(1)
			joined := strings.Join(args, " ")
			if name != "curl" || requestedEmailAsset(joined) == "" || strings.Contains(joined, "--retry") || strings.Contains(joined, "--location") {
				return "", fmt.Errorf("unexpected synthetic email asset request")
			}
			scope := "public"
			if strings.Contains(joined, "--resolve") {
				scope = "edge"
			}
			if scope == failingScope {
				return emailAssetHTTPFixture("429", "0", "text/html", "128", "2001:db8:19::3"), nil
			}
			return emailAssetHTTPFixture("200", "0", "image/png", "1024", "2001:db8:19::3"), nil
		}}
		settings := emailAssetSyntheticSettings(source, "2001:db8:19::3")
		settings.WebsiteDomain = "site.synthetic.example"
		settings.Hosts[0].Name = "synthetic-edge"
		alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s refusal lost: alerts=%+v err=%v", failingScope, alerts, err)
		}
		alert := requireAlertClass(t, alerts, "web-email-assets")
		if alert.Identity() != "synthetic/web-email-assets|web-email-assets|site.synthetic.example|" || alert.Severity != SeverityWarn || alert.Sustain != 1 || calls.Load() != 4 {
			t.Errorf("%s refusal changed identity, severity, sustain, or request count: alert=%+v calls=%d", failingScope, alert, calls.Load())
		}
		wantScope := "public_failed=0 edge_failed=2"
		if failingScope == "public" {
			wantScope = "public_failed=2 edge_failed=0"
		}
		if !strings.Contains(alert.Observed, wantScope) || !strings.Contains(alert.Observed, "missing_404=0 non_image=0 empty_body=0 transport_failures=0 other_response_failures=2") {
			t.Errorf("%s refusal lost scope or status-before-content classification: %s", failingScope, alert.Observed)
		}
		if !strings.Contains(alert.Action, "responding") || !strings.Contains(alert.Action, "upstream") {
			t.Errorf("%s refusal selected a repair before identifying its responder: %s", failingScope, alert.Action)
		}
		for _, unsupported := range []string{"if every edge fails, deploy", "then invalidate only", "the fault is in DNS", "Build and deploy only"} {
			if strings.Contains(alert.Action+alert.Mechanism, unsupported) {
				t.Errorf("%s refusal retained unsupported remediation %q", failingScope, unsupported)
			}
		}
	}
}

// Missing responses require current route and artifact evidence before attributing build age.
func TestEmailAssetsSignalMissingImageDoesNotProveBuildAge(t *testing.T) {
	source := &syntheticSource{localFn: func(string, ...string) (string, error) {
		return emailAssetHTTPFixture("404", "0", "text/html", "128", "2001:db8:19::4"), nil
	}}
	settings := emailAssetSyntheticSettings(source, "2001:db8:19::4")
	settings.WebsiteDomain = "site.synthetic.example"
	settings.Hosts[0].Name = "synthetic-edge"
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("missing image failure lost: alerts=%+v err=%v", alerts, err)
	}
	alert := requireAlertClass(t, alerts, "web-email-assets")
	if !strings.Contains(alert.Observed, "missing_404=4") || !strings.Contains(alert.Action, "If the serving artifact is confirmed to omit") {
		t.Errorf("404 selected bundle repair before proving artifact contents: %s", alert.Action)
	}
	if strings.Contains(alert.Mechanism, "The site bundle on the edges does not contain") {
		t.Errorf("404 manufactured artifact-content evidence: %s", alert.Mechanism)
	}
}

// Healthy peers localize one missing response without proving the failed artifact's contents.
func TestEmailAssetsSignalPartialMissingDoesNotProveArtifactContents(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "[2001:db8:19::5]") && requestedEmailAsset(joined) == emailAssets[0].path {
			return emailAssetHTTPFixture("404", "0", "text/html", "128", "2001:db8:19::5"), nil
		}
		return emailAssetHTTPFixture("200", "0", "image/png", "1024", "2001:db8:19::6"), nil
	}}
	settings := emailAssetSyntheticSettings(source, "2001:db8:19::5", "2001:db8:19::6")
	settings.WebsiteDomain = "site.synthetic.example"
	settings.Hosts[0].Name = "synthetic-edge"
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("partial missing-image failure lost: alerts=%+v err=%v", alerts, err)
	}
	alert := requireAlertClass(t, alerts, "web-email-assets")
	if !strings.Contains(alert.Observed, "checked=6 failed=1 public_failed=0 edge_failed=1 missing_404=1") || alert.Severity != SeverityWarn || alert.Sustain != 1 {
		t.Errorf("partial missing-image control changed the failure boundary: %+v", alert)
	}
	if strings.Contains(alert.Mechanism, "which site generations lack the requested image") || !strings.Contains(alert.Action, "route") || !strings.Contains(alert.Action, "artifact") {
		t.Errorf("partial 404 assumed artifact contents before route evidence: mechanism=%s action=%s", alert.Mechanism, alert.Action)
	}
}
