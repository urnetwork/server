package monitor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const evaluationPingExpiryFixture = "[synthetic-taskworker.example][taskworker][synthetic-generation][cid:synthetic-private-correlation][I][2000-01-01T00:00:00Z][ip_remote_multi_client.go:42][multi]evaluation ping error [abcdefab-0000-4000-8000-000000000001] = queued Pack expired before serialization: send Pack was not admitted"

// Exact source-reviewed expiry stays visible without inventing a remote cause.
func TestEvaluationPingUnwrittenExpiryClassifiesAndRenders(t *testing.T) {
	for _, testCase := range []struct {
		name string
		line string
	}{
		{name: "canonical-collected", line: evaluationPingExpiryFixture},
		{name: "no-identity-prefix", line: evaluationPingExpiryFixture[strings.Index(evaluationPingExpiryFixture, "[ip_remote_multi_client.go:"):]},
		{name: "uppercase-id", line: strings.Replace(evaluationPingExpiryFixture, "abcdefab", "ABCDEFAB", 1)},
		{name: "throttled", line: evaluationPingExpiryFixture + " (7 suppressed)"},
	} {
		tailer := newLogTailer("taskworker", nil)
		for range novelRateThreshold {
			tailer.classify(testCase.line)
		}
		findings := tailer.drainWindow()
		got := findingByClass(t, findings, "window-evaluation-unwritten-expiry")
		if got.healthy || got.tier != tierWarn || got.sustain != 1 ||
			got.target != "taskworker" || got.frame != "pre-serialization" {
			t.Fatalf("%s: incorrect bounded expiry finding", testCase.name)
		}
		if !strings.Contains(got.observed, "rate=20/min") {
			t.Fatalf("%s: suppressed suffix changed diagnostic count", testCase.name)
		}
		if novel := findingByClass(t, findings, "novel"); !novel.healthy {
			t.Fatalf("%s: exact expiry remained novel", testCase.name)
		}
		markdown := alertFromFinding(SignalSettings{
			Environment: "synthetic",
			Now:         func() time.Time { return time.Date(2000, 1, 1, 0, 1, 0, 0, time.UTC) },
		}, "1.5", "log-errors", "Log error-class rates", got).Markdown()
		for _, want := range []string{
			"before serialization or sequence-number assignment",
			"does not identify the queue-delay cause",
			"not a provider-response result",
			"distinct from the synchronous signaling refusal",
			"diagnostic lines, not unique candidates",
			"ten minutes",
		} {
			if !strings.Contains(markdown, want) {
				t.Errorf("%s: Markdown lacks %q", testCase.name, want)
			}
		}
		for fieldIndex, private := range []string{
			"synthetic-taskworker.example", "synthetic-generation",
			"synthetic-private-correlation", "abcdefab-0000-4000-8000-000000000001",
			"ip_remote_multi_client.go:42",
		} {
			if strings.Contains(strings.ToLower(markdown), strings.ToLower(private)) {
				t.Errorf("%s: Markdown retained private fixture field %d", testCase.name, fieldIndex)
			}
		}
	}
}

// The usual known-class boundary counts lines, and drain resets the window.
func TestEvaluationPingUnwrittenExpiryThresholdAndRecovery(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		lines   int
		healthy bool
	}{
		{name: "empty", healthy: true},
		{name: "one-throttled-line", lines: 1, healthy: true},
		{name: "below", lines: novelRateThreshold - 1, healthy: true},
		{name: "boundary", lines: novelRateThreshold},
		{name: "above", lines: novelRateThreshold + 1},
	} {
		tailer := newLogTailer("taskworker", nil)
		for range testCase.lines {
			tailer.classify(evaluationPingExpiryFixture + " (99 suppressed)")
		}
		got := findingByClass(t, tailer.drainWindow(), "window-evaluation-unwritten-expiry")
		if got.healthy != testCase.healthy {
			t.Fatalf("%s: healthy=%t, want %t", testCase.name, got.healthy, testCase.healthy)
		}
		if !got.healthy && !strings.Contains(got.observed, fmt.Sprintf("rate=%d/min", testCase.lines)) {
			t.Fatalf("%s: diagnostic rate changed", testCase.name)
		}
		if recovered := findingByClass(t, tailer.drainWindow(), "window-evaluation-unwritten-expiry"); !recovered.healthy {
			t.Fatalf("%s: old count survived drain", testCase.name)
		}
	}
}

// A generic admission result, other callsite, or incomplete suffix proves less.
func TestEvaluationPingUnwrittenExpiryNearMissesRemainNovel(t *testing.T) {
	for _, testCase := range []struct {
		name string
		line string
	}{
		{name: "generic-admission", line: strings.Replace(evaluationPingExpiryFixture, "queued Pack expired before serialization: ", "", 1)},
		{name: "other-source", line: strings.Replace(evaluationPingExpiryFixture, "ip_remote_multi_client.go", "synthetic_other.go", 1)},
		{name: "other-operation", line: strings.Replace(evaluationPingExpiryFixture, "evaluation ping error", "synthetic control error", 1)},
		{name: "truncated", line: strings.TrimSuffix(evaluationPingExpiryFixture, " serialization: send Pack was not admitted")},
		{name: "extra-error", line: evaluationPingExpiryFixture + "; synthetic structural failure"},
		{name: "malformed-suppression", line: evaluationPingExpiryFixture + " (unknown suppressed)"},
		{name: "extra-source-gap", line: strings.Replace(evaluationPingExpiryFixture, "][multi]", "] [multi]", 1)},
		{name: "malformed-id", line: strings.Replace(evaluationPingExpiryFixture, "abcdefab-0000-4000-8000-000000000001", "synthetic-invalid-id", 1)},
		{name: "raw-glog-not-collected-format", line: "I0101 00:00:00.000000       1 ip_remote_multi_client.go:42] " + evaluationPingExpiryFixture[strings.Index(evaluationPingExpiryFixture, "[multi]"):]},
	} {
		tailer := newLogTailer("taskworker", nil)
		for range novelRateThreshold {
			tailer.classify(testCase.line)
		}
		findings := tailer.drainWindow()
		if got := findingByClass(t, findings, "window-evaluation-unwritten-expiry"); !got.healthy {
			t.Fatalf("%s: near miss acquired unwritten proof", testCase.name)
		}
		if novel := findingByClass(t, findings, "novel"); novel.healthy {
			t.Fatalf("%s: unknown error disappeared", testCase.name)
		}
	}
}

// Independent signaling refusal and unknown errors keep their own identities.
func TestEvaluationPingUnwrittenExpiryKeepsOtherClasses(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for range novelRateThreshold {
		tailer.classify(evaluationPingExpiryFixture)
		tailer.classify("[transport_p2p_webrtc.go:9][signal]send failed mode=receive-reply reason=not-admitted")
		tailer.classify("[synthetic.go:1] synthetic unrelated error")
	}
	findings := tailer.drainWindow()
	for _, class := range []string{"window-evaluation-unwritten-expiry", "signal-send-not-admitted", "novel"} {
		got := findingByClass(t, findings, class)
		if got.healthy || !strings.Contains(got.observed, "rate=20/min") {
			t.Fatalf("%s: mixed window merged or hid an independent cause", class)
		}
	}
}

// An already canceled collector cannot manufacture an expiry observation.
func TestEvaluationPingUnwrittenExpiryCanceledTailHasNoEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tailer := newLogTailer("taskworker", nil)
	tailer.stream = func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
		return nil, nil, ctx.Err()
	}
	if err := tailer.tailOnce(ctx); err != context.Canceled {
		t.Fatal("tail cancellation did not return context.Canceled")
	}
	for _, got := range tailer.drainWindow() {
		if (got.class == "window-evaluation-unwritten-expiry" || got.class == "novel") && !got.healthy {
			t.Fatal("cancellation manufactured product evidence")
		}
	}
}

// The one-shot adapter uses the same class and privacy-safe stable frame.
func TestEvaluationPingUnwrittenExpiryOneShotAdapter(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return strings.Repeat(evaluationPingExpiryFixture+"\n", novelRateThreshold), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal("one-shot synthetic probe returned an error")
	}
	found := false
	for _, alert := range alerts {
		if alert.Class == "window-evaluation-unwritten-expiry" {
			found = true
			if alert.Frame != "pre-serialization" || alert.Severity != SeverityWarn {
				t.Fatal("one-shot expiry identity changed")
			}
		}

		if alert.Class == "novel" || alert.Class == "signal-send-not-admitted" {
			t.Fatal("one-shot expiry gained the wrong cause")
		}
	}
	if !found {
		t.Fatal("one-shot exact expiry class missing")
	}
}
