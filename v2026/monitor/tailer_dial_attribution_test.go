// Exercises generic dial frames through the real service/class counter and
// alert renderer. All addresses, source fields, and timestamps are synthetic.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// An earlier caller or metadata endpoint must not replace the matched dial.
func TestDialTimeoutFrameUsesMatchedDestination(t *testing.T) {
	for _, prefix := range []string{
		"caller=192.0.2.10:41000 ",
		"url=https://synthetic.example/192.0.2.10:41000 upstream=203.0.113.40:9443 ",
	} {
		tailer := newLogTailer("grafana", nil)
		for range 10 {
			tailer.classify(prefix + "synthetic error: dial tcp 198.51.100.20:7443: i/o timeout")
		}
		finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout")
		if finding.healthy || finding.tier != tierPage || finding.target != "grafana" || finding.frame != "198.51.100.20:7443" {
			t.Errorf("prefix %q: dial finding = %+v", prefix, finding)
		}
		if !strings.Contains(finding.observed, "rate=10/min") || strings.Contains(finding.observed, "192.0.2.10") {
			t.Errorf("prefix %q: wrong dial count or frame: %s", prefix, finding.observed)
		}
		if strings.Contains(finding.evidence, "192.0.2.10") || strings.Contains(finding.evidence, "synthetic.example") {
			t.Errorf("prefix %q: unbounded metadata escaped the fixed dial sample", prefix)
		}
	}
}

// Distinct dial destinations still contribute to one unchanged threshold.
func TestDialTimeoutMixedDestinationsPreserveClassCount(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	for range 9 {
		tailer.classify("synthetic error: dial tcp 198.51.100.20:7443: i/o timeout")
	}
	if finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout"); !finding.healthy {
		t.Fatal("nine dial lines crossed the ten-line threshold")
	}
	for index := range 10 {
		tailer.classify(fmt.Sprintf("caller=192.0.2.10:41000 synthetic error: dial tcp 198.51.100.%d:7443: i/o timeout", index+20))
	}
	if len(tailer.classCounts) != 1 || tailer.classCounts["dial-io-timeout"] != 10 {
		t.Fatalf("destination attribution changed the service/class buckets: %v", tailer.classCounts)
	}
	brokenCount := 0
	for _, finding := range tailer.drainWindow() {
		if finding.class != "dial-io-timeout" || finding.healthy {
			continue
		}
		brokenCount++
		if finding.tier != tierPage || finding.target != "grafana" || finding.frame != "198.51.100.20:7443" ||
			!strings.Contains(finding.observed, "rate=10/min") {
			t.Errorf("mixed endpoints split or weakened the class page: %+v", finding)
		}
	}
	if brokenCount != 1 {
		t.Fatalf("mixed endpoints emitted %d class findings, want one aggregate", brokenCount)
	}
	if finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout"); !finding.healthy {
		t.Fatal("empty next window retained the previous dial count")
	}
}

// A combined error with multiple dial candidates cannot identify one frame;
// later lines cannot replace the first exemplar or hide its positive count.
func TestDialTimeoutAmbiguousExemplarRetainsClassPage(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	tailer.classify("synthetic combined errors: dial tcp 198.51.100.20:7443: i/o timeout; dial tcp 203.0.113.30:8443: i/o timeout")
	for range 9 {
		tailer.classify("synthetic error: dial tcp 198.51.100.20:7443: i/o timeout")
	}
	finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout")
	if finding.healthy || finding.tier != tierPage || finding.target != "grafana" ||
		!strings.Contains(finding.observed, "rate=10/min") {
		t.Fatalf("ambiguous exemplar lost the aggregate page: %+v", finding)
	}
	if finding.frame != "" || strings.Contains(finding.observed, "frame=") {
		t.Fatalf("combined dial errors invented one destination: frame=%q observed=%q", finding.frame, finding.observed)
	}
}

// Query echoes, ordinary activity, DoH precedence and neighboring refusal
// classification retain their existing boundaries.
func TestDialTimeoutHealthyAndAdjacentClassControls(t *testing.T) {
	healthy := newLogTailer("grafana", nil)
	for range 10 {
		healthy.classify("synthetic connection established: dial tcp 198.51.100.20:7443")
		healthy.classify(`level=info caller=engine.go:10 msg="executing query" query="dial tcp 198.51.100.20:7443: i/o timeout"`)
	}
	if finding := findingByClass(t, healthy.drainWindow(), "dial-io-timeout"); !finding.healthy {
		t.Fatal("ordinary activity or query echo became a dial-timeout page")
	}
	doh := newLogTailer("grafana", nil)
	for range 10 {
		doh.classify("[family]dial tag=doh net=tcp err=dial tcp 198.51.100.20:7443: i/o timeout")
	}
	dohFindings := doh.drainWindow()
	if finding := findingByClass(t, dohFindings, "dial-io-timeout"); !finding.healthy {
		t.Fatal("service-specific DoH line also entered the generic class")
	}
	if finding := findingByClass(t, dohFindings, "doh-dial-timeout"); finding.healthy || finding.frame != "doh-attempt" {
		t.Fatal("DoH class lost its existing frame or threshold")
	}
	refused := newLogTailer("grafana", nil)
	for range 10 {
		refused.classify("synthetic error: dial tcp 203.0.113.30:8443: connect: connection refused")
	}
	refusedFindings := refused.drainWindow()
	if finding := findingByClass(t, refusedFindings, "dial-io-timeout"); !finding.healthy {
		t.Fatal("connection refusal became a timeout")
	}
	if finding := findingByClass(t, refusedFindings, "connection-refused"); finding.healthy || finding.frame != "203.0.113.30:8443" {
		t.Fatal("neighboring refusal lost its existing frame or threshold")
	}
}

// Standing collection keeps exact-replay suppression ahead of counting and
// frame selection, independently of the generic classification entry point.
func TestDialTimeoutStandingReplayPreservesCountAndFrame(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	now := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	tailer.clock = func() time.Time { return now }
	for index := range 10 {
		line := fmt.Sprintf("[2000-01-01T00:00:00Z] synthetic_sequence=%d caller=192.0.2.10:41000 error: dial tcp 198.51.100.20:7443: i/o timeout", index)
		tailer.ingestStanding(line, true, true)
		tailer.ingestStanding(line, true, true)
	}
	finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout")
	if finding.healthy || finding.frame != "198.51.100.20:7443" || !strings.Contains(finding.observed, "rate=10/min") {
		t.Fatalf("standing replay changed dial count or destination: %+v", finding)
	}
	for index := range 10 {
		tailer.ingestStanding(fmt.Sprintf("[2000-01-01T00:00:00Z] synthetic_sequence=%d caller=192.0.2.10:41000 error: dial tcp 198.51.100.20:7443: i/o timeout", index), true, true)
	}
	if finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout"); !finding.healthy {
		t.Fatal("exact replays created another class page")
	}
}

// The public bounded-window signal renders the same first-exemplar frame
// while reporting all service/class lines, without retaining caller fields.
func TestLogErrorsDialTimeoutAlertUsesExemplarDestination(t *testing.T) {
	lines := []string{}
	for index := range 10 {
		lines = append(lines, fmt.Sprintf("caller=192.0.2.10:41000 synthetic_correlation=synthetic-private error: dial tcp 198.51.100.%d:7443: i/o timeout", index+20))
	}
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return strings.Join(lines, "\n"), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "dial-io-timeout")
	if alert.SignalID != "logs/dial-io-timeout" || alert.Target != "grafana" || alert.Frame != "198.51.100.20:7443" ||
		alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "rate=10/min") {
		t.Fatalf("rendered dial alert changed identity or aggregate count: %+v", alert)
	}
	for _, private := range []string{"192.0.2.10", "synthetic-private", "synthetic_correlation"} {
		requireAlertOmits(t, alert, private)
	}
}
