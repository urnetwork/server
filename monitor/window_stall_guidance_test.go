// A platform reason can describe local setup or contract work without a failed
// provider. The alert must preserve that branch without changing event counts.
package monitor

import (
	"fmt"
	"strings"
	"testing"
)

// Exercise the real classifier and rendered operator guidance at both existing
// thresholds. The identity-free event cannot select a local or remote cause.
func TestWindowStallGuidanceRetainsLocalControlAndUnknownCause(t *testing.T) {
	for _, test := range []struct {
		name      string
		class     string
		event     string
		threshold int
	}{
		{
			name:      "nonterminal",
			class:     "window-stall",
			event:     "event=window_stall window=quality reason=platform-unreachable failed=0",
			threshold: 20,
		},
		{
			name:      "terminal",
			class:     "window-stall-terminal",
			event:     "event=window_failed window=quality reason=platform-unreachable after=45000",
			threshold: 1,
		},
	} {
		line := "[synthetic-host][taskworker][synthetic-generation][cid:synthetic-container][rel] " + test.event
		below := newLogTailer("taskworker", nil)
		for range test.threshold - 1 {
			below.classify(line)
		}
		if got := findingByClass(t, below.drainWindow(), test.class); !got.healthy {
			t.Fatalf("%s: below-threshold diagnostic count became a violation: %+v", test.name, got)
		}

		atThreshold := newLogTailer("taskworker", nil)
		for range test.threshold {
			atThreshold.classify(line)
		}
		finding := findingByClass(t, atThreshold.drainWindow(), test.class)
		if finding.healthy || finding.tier != tierWarn {
			t.Fatalf("%s: existing threshold lost its warning: %+v", test.name, finding)
		}
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log errors", finding)
		wantIdentity := "logs/" + test.class + "|" + test.class + "|taskworker|"
		if alert.Identity() != wantIdentity {
			t.Fatalf("%s: diagnostic guidance changed identity: got %q, want %q", test.name, alert.Identity(), wantIdentity)
		}
		if want := fmt.Sprintf("rate=%d/min", test.threshold); !strings.Contains(alert.Observed, want) {
			t.Fatalf("%s: diagnostic guidance changed the counted rate: %q", test.name, alert.Observed)
		}

		markdown := alert.Markdown()
		for _, required := range []string{
			"local contract acquisition before provider contact",
			"owned client setup deadlines",
			"Missing companion logs leave the cause unknown",
			"emitting artifact's Connect source",
			"not task execution progress",
			"provider-window progress",
			"§2.19",
		} {
			if !strings.Contains(markdown, required) {
				t.Errorf("%s: window diagnosis omits required causal discriminator %q", test.name, required)
			}
		}
		for _, privateValue := range []string{"synthetic-host", "synthetic-generation", "synthetic-container"} {
			if strings.Contains(markdown, privateValue) {
				t.Errorf("%s: window guidance retained unrelated source identity %q", test.name, privateValue)
			}
		}
	}
}
