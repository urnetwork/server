package monitor

// The immediate net/http writer frame cannot establish its caller, panic,
// process generation, or fix ancestry. Exercise the actual log signal so its
// rendered guidance cannot turn that missing authority into a deployment order.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Legacy, forwarding, foreign, missing and malformed frames retain the same
// warning while requiring the original event and artifact before attribution.
func TestLogErrorsHttpHijackWriteUnqualifiedSourceKeepsCauseUnknown(t *testing.T) {
	for _, frame := range []string{
		"from github.com/urnetwork/server/router.(*Router).ServeHTTP.func1.1 (router.go:73)",
		"from github.com/urnetwork/server/v2026/router.(*requestMetricsWriter).WriteHeader (metrics.go:73)",
		"from fixture.example/relay.(*Writer).Fail (writer.go:17)",
		"",
		"from [source unavailable]",
		"from fixture.example/" + strings.Repeat("synthetic/", 30) + "(*Writer).Fail (writer.go:17)",
	} {
		line := "http: response.WriteHeader on hijacked connection " + frame
		source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
			if name != "warpctl" || len(args) == 0 {
				return "", fmt.Errorf("unexpected synthetic command")
			}
			switch args[0] {
			case "ls":
				return "repo names synthetic-connect", nil
			case "logs":
				return line + "\n", nil
			default:
				return "", fmt.Errorf("unexpected synthetic operation")
			}
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "http-hijack-write")
		// This is the causal pre-fix assertion: the old generic matcher emitted
		// a deployment command without any event or artifact authority.
		if strings.Contains(alert.Action, "Deploy the router recovery fix that returns immediately") ||
			!strings.Contains(alert.Action, "Deploy the Done recovery fix only when proved missing") {
			t.Fatalf("unqualified warning prescribed the historical Done fix: frame=%q action=%q", frame, alert.Action)
		}
		if !strings.Contains(alert.Evidence, "responsible caller and cause remain unknown") ||
			!strings.Contains(alert.Context, "forwarding wrapper, not its caller") ||
			!strings.Contains(alert.Mechanism, "does not establish that historical cause") {
			t.Fatalf("immediate writer frame became a causal finding: frame=%q alert=%s", frame, alert.Markdown())
		}
		for _, required := range []string{
			"source event time", "emitting process and immutable artifact", "source ancestry",
			"02f4d29ae575ae68c2a5f0c8ba3207361b353170", "artifact already contains it",
			"same-event [h]unhandled error from route", "bounded caller stack",
			"Missing, stale, partial, or ambiguous evidence leaves cause unknown",
			"complete same-event source coverage",
		} {
			if !strings.Contains(alert.Action, required) {
				t.Errorf("unqualified warning omitted authority boundary %q for frame %q", required, frame)
			}
		}
		if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Frame != "" ||
			alert.Identity() != "logs/http-hijack-write|http-hijack-write|connect|" ||
			alert.Observed != "rate=1/min class=http-hijack-write target=connect" {
			t.Fatalf("guidance changed warning identity or counting: %+v", alert)
		}
		if frame != "" && len(line) <= 200 && !strings.Contains(alert.Evidence, frame) {
			t.Fatalf("warning lost its available immediate source frame: %q", frame)
		}
		_, sample, ok := strings.Cut(alert.Evidence, "\nsample: ")
		if !ok || len(sample) > 200 || !strings.Contains(alert.Context, "missing or truncated frame does not prove caller absence") {
			t.Fatalf("bounded sample lost its completeness qualifier: frame=%q evidence=%q", frame, alert.Evidence)
		}
	}
}

// Mixed writer frames remain one service warning; each paired body diagnostic
// is excluded so the unchanged rate counts only canonical header attempts.
func TestLogErrorsHttpHijackWriteCountsCanonicalHeadersOnly(t *testing.T) {
	lines := []string{
		"http: response.WriteHeader on hijacked connection from fixture.example/relay.(*Writer).First (writer.go:17)",
		"http: response.Write on hijacked connection from fixture.example/relay.(*Writer).First (writer.go:18)",
		"http: response.WriteHeader on hijacked connection from fixture.example/relay.(*Writer).Second (writer.go:27)",
		"http: response.Write on hijacked connection from fixture.example/relay.(*Writer).Second (writer.go:28)",
		"http: response.WriteHeader on hijacked connection",
		"http: response.Write on hijacked connection",
		"request completed status=200",
	}
	for _, lineCount := range []int{2, len(lines)} {
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-connect", nil
			}
			return strings.Join(lines[:lineCount], "\n"), nil
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "http-hijack-write")
		wantRate := 1
		if lineCount == len(lines) {
			wantRate = 3
		}
		if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Frame != "" ||
			alert.Observed != fmt.Sprintf("rate=%d/min class=http-hijack-write target=connect", wantRate) {
			t.Fatalf("paired body diagnostics or writer frames changed canonical counting: %+v", alerts)
		}
	}
}

// Healthy traffic and a body-only partial record do not manufacture the
// canonical header observation. Their absence cannot prove runtime recovery.
func TestLogErrorsHttpHijackWriteHealthyAndBodyOnlyControls(t *testing.T) {
	for _, line := range []string{"", "request completed status=200", "http: response.Write on hijacked connection"} {
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-connect", nil
			}
			return line, nil
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		for _, alert := range alerts {
			if alert.Class == "http-hijack-write" {
				t.Fatalf("noncanonical input manufactured a header warning: %q", line)
			}
		}
	}
}

// A failed bounded source cannot turn its partial warning into a completed
// observation or a healthy sample. Transport failure remains explicit.
func TestLogErrorsHttpHijackWritePartialSourceRemainsUnavailable(t *testing.T) {
	for _, partial := range []string{"", "http: response.WriteHeader on hijacked connection"} {
		unavailable := errors.New("synthetic log source unavailable")
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-connect", nil
			}
			return partial, unavailable
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if !errors.Is(err, unavailable) || len(alerts) != 0 {
			t.Fatalf("incomplete source became a completed observation: alerts=%+v error=%v", alerts, err)
		}
	}
}
