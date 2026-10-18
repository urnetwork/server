// Mixed responses retain the measured scope instead of turning one missing
// image or an omitted transport observation into fleet-wide evidence.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// One missing image on one address cannot diagnose every configured edge.
func TestEmailAssetsSingleEdgeMissingPreservesScope(t *testing.T) {
	failedAddress := "2001:db8:19::401"
	healthyAddress := "2001:db8:19::402"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		if strings.Contains(joined, "--resolve") && strings.Contains(joined, failedAddress) && requestedEmailAsset(joined) == emailAssets[0].path {
			return emailAssetHTTPFixture("404", "0", "text/html", "128", failedAddress), nil
		}
		return emailAssetHTTPFixture("200", "0", "image/png", "1024", healthyAddress), nil
	}}
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), emailAssetSyntheticSettings(source, failedAddress, healthyAddress))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "web-email-assets")
	markdown := alert.Markdown()
	for _, want := range []string{"checked=6 failed=1 public_failed=0 edge_failed=1 missing_404=1", "1 of 4 configured exact-edge checks"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing bounded scope %q: %s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Every exact edge returns 404") || alert.Sustain != 1 {
		t.Fatalf("one missing response changed scope or threshold: %s", markdown)
	}
}

// An omitted exact-edge transport result is unknown for both semantic and
// transport-only public failures, including when no exact edge was observable.
func TestEmailAssetsOmittedEdgesDoNotBecomeHealthy(t *testing.T) {
	for _, publicExit := range []string{"0", "35"} {
		for _, healthyPeer := range []bool{false, true} {
			addresses := []string{"2001:db8:19::428"}
			if healthyPeer {
				addresses = append(addresses, "2001:db8:19::200")
			}
			source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				if name != "curl" {
					return "", errors.New("unexpected local command")
				}
				if strings.Contains(joined, "--resolve") {
					if strings.Contains(joined, addresses[0]) {
						return emailAssetHTTPFixture("000", "28", "", "0", ""), errors.New("synthetic transport failure")
					}
					return emailAssetHTTPFixture("200", "0", "image/png", "1024", addresses[1]), nil
				}
				if requestedEmailAsset(joined) == emailAssets[0].path {
					if publicExit == "35" {
						return emailAssetHTTPFixture("000", "35", "", "0", ""), errors.New("synthetic public transport failure")
					}
					return emailAssetHTTPFixture("429", "0", "text/html", "128", "203.0.113.19"), nil
				}
				return emailAssetHTTPFixture("200", "0", "image/png", "1024", "203.0.113.19"), nil
			}}
			alerts, err := NewEmailAssetsSignal().Run(context.Background(), emailAssetSyntheticSettings(source, addresses...))
			if err != nil || len(alerts) != 1 {
				t.Fatalf("public exit %s peer %t: alerts=%v err=%v", publicExit, healthyPeer, alerts, err)
			}
			markdown := alerts[0].Markdown()
			for _, unsupported := range []string{"Every checked exact edge is healthy", "Every exact edge completed its semantic image check", "image bytes themselves are present and reachable"} {
				if strings.Contains(markdown, unsupported) {
					t.Fatalf("public exit %s peer %t: unavailable edges became healthy: %s", publicExit, healthyPeer, markdown)
				}
			}
			if !strings.Contains(markdown, "2 configured exact-edge checks were unavailable") || !strings.Contains(markdown, "public_failed=1 edge_failed=0") {
				t.Fatalf("public exit %s peer %t: omitted coverage/count missing: %s", publicExit, healthyPeer, markdown)
			}
		}
	}
}

// An affirmative 429 remains an immediate semantic warning with exact counts.
func TestEmailAssetsEdge429PreservesResponseFailure(t *testing.T) {
	address := "2001:db8:19::429"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		if strings.Contains(joined, "--resolve") && requestedEmailAsset(joined) == emailAssets[0].path {
			return emailAssetHTTPFixture("429", "0", "text/html", "128", address), nil
		}
		return emailAssetHTTPFixture("200", "0", "image/png", "1024", address), nil
	}}
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), emailAssetSyntheticSettings(source, address))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "web-email-assets")
	markdown := alert.Markdown()
	for _, want := range []string{"checked=4 failed=1 public_failed=0 edge_failed=1 missing_404=0", "other_response_failures=1", "required image returned HTTP 429"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("429 response lost %q: %s", want, markdown)
		}
	}
	if alert.Sustain != 1 {
		t.Fatal("affirmative response did not retain its immediate warning")
	}
}
