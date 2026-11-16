package monitor

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// SIGNALS.md §1.5: a current loss event cannot turn a historical deployment
// or ring-health observation into evidence about the current runtime.
func TestLokiLossGuidanceRequiresCurrentRuntimeEvidence(t *testing.T) {
	settings := syntheticSettings(nil)
	settings.Now = func() time.Time { return time.Date(2031, 7, 8, 9, 10, 0, 0, time.UTC) }
	historicalRuntime := regexp.MustCompile(`(?i)current [0-9]{4}[.][0-9]{1,2}[.][0-9]{1,2}`)
	for _, testCase := range []struct {
		class string
		line  string
		count int
		frame string
	}{
		{
			class: "loki-tail-dropped-streams",
			line:  `level=info caller=tailer.go:123 msg="tailer dropped streams is reset" length=10`,
			count: 1,
		},
		{
			class: "loki-tail-backend-eof",
			line:  `level=error caller=tail.go:123 component=tail-querier msg="Error receiving response from grpc tail client" addr=192.0.2.42:6490 err=EOF`,
			count: 5,
			frame: "backend=192.0.2.42:6490",
		},
		{
			class: "loki-tail-backend-eof",
			line:  `level=error caller=tail.go:123 component=tail-querier msg="Error receiving response from grpc tail client" err=EOF`,
			count: 5,
		},
	} {
		tailer := newLogTailer("fixture-grafana", nil)
		for range testCase.count {
			tailer.classify(testCase.line)
		}
		finding := findingByClass(t, tailer.drainWindow(), testCase.class)
		alert := alertFromFinding(settings, "1.5", "log-errors", "Log error-class rates", finding)
		if finding.healthy || alert.Severity != SeverityWarn || alert.Target != "fixture-grafana" ||
			alert.Frame != testCase.frame || !strings.Contains(alert.Observed, fmt.Sprintf("rate=%d/min", testCase.count)) {
			t.Fatalf("%s frame %q changed the loss identity, threshold or severity", testCase.class, testCase.frame)
		}
		if historicalRuntime.MatchString(alert.Markdown()) {
			t.Errorf("%s frame %q: fresh loss promoted a fixed historical artifact to current runtime", testCase.class, testCase.frame)
		}
		for _, falseConclusion := range []string{
			"This rules out a stale rollout and a single failed backend",
			"All six active Grafana nodes were healthy",
			"missing LAN identity is no longer a current prerequisite",
			"removes missing LAN identity as the current prerequisite",
			"The current off-grid EOF wave",
		} {
			if strings.Contains(alert.Markdown(), falseConclusion) {
				t.Errorf("%s frame %q: fresh loss inherited a historical rollout or health conclusion", testCase.class, testCase.frame)
			}
		}
		if !strings.Contains(alert.Context, "Current artifact ancestry and backend health are unknown from this log event") {
			t.Errorf("%s frame %q: missing current runtime evidence was not kept unknown", testCase.class, testCase.frame)
		}
		for _, required := range []string{
			"only when the running artifact is proven to lack",
			"If the fixes are already present",
			"Missing ancestry remains unknown",
		} {
			if !strings.Contains(alert.Action, required) {
				t.Errorf("%s frame %q: repair guidance omitted %q", testCase.class, testCase.frame, required)
			}
		}
	}
}

// Expected client retirement and a subthreshold EOF window remain healthy
// for this class; changing its explanation must not broaden loss detection.
func TestLokiLossGuidancePreservesThresholdAndRetirementControls(t *testing.T) {
	tailer := newLogTailer("fixture-grafana", nil)
	for range 4 {
		tailer.classify(`level=error caller=tail.go:123 component=tail-querier msg="Error receiving response from grpc tail client" err=EOF`)
	}
	for range 20 {
		tailer.classify(`level=error caller=tail.go:123 component=tail-querier msg="Error receiving response from grpc tail client" err="rpc error: code = Canceled desc = context canceled"`)
	}
	findings := tailer.drainWindow()
	for _, class := range []string{"loki-tail-backend-eof", "loki-tail-dropped-streams"} {
		if !findingByClass(t, findings, class).healthy {
			t.Fatalf("%s turned an expected retirement or subthreshold window into loss", class)
		}
	}
}
