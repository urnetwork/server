package monitor

// Exercises the natural ErrorJson-to-alert path, with private synthetic fields
// and the source reader's exact symbol/file contract. No external fixtures.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Synthetic source frames retain the real symbol/file relationship only.
func panicProviderTestStack(version string) []string {
	root := "github.com/urnetwork/server" + version
	return []string{
		"goroutine 42 [running]:", "runtime/debug.Stack()", "/synthetic-private-path/stack.go:99",
		root + ".HandleError.func1(0x12345678)", "/synthetic-private-path/trace.go:138 +0x1",
		"panic({0x87654321, 0x12345678})", "/synthetic-private-path/runtime/panic.go:1",
		root + ".Raise(0x12345678)", "/synthetic-private-path/util.go:1",
		root + "/controller.statsRefreshProviderEgress(0x12345678)", "/synthetic-private-path/stats_collector.go:836 +0x1",
	}
}

// Matches the standing stream's wrapper while exercising the real producer.
func panicProviderTestLine(err error, stack []string) string {
	return "[synthetic-private-host][taskworker][synthetic-generation][cid:synthetic-correlation]" +
		"[I][2000-01-01T00:00:00Z][trace.go:138]Unexpected error: " +
		server.ErrorJson(err, []byte(strings.Join(stack, "\n")))
}

// Only privacy-safe diagnostics may survive both the alert and sample sinks.
func panicProviderRequirePrivate(t *testing.T, text string) {
	t.Helper()
	for _, private := range []string{
		"synthetic-private-host", "synthetic-generation", "synthetic-correlation", "cid:",
		"synthetic-private-path", "0x12345678", "0x87654321", "stats_collector.go",
		"synthetic-secret", "synthetic-customer", "192.0.2.88", "SELECT private", "provider egress stats",
	} {
		if strings.Contains(text, private) {
			t.Fatalf("panic diagnostic retained private synthetic material %q", private)
		}
	}
}

// These exact refusals previously collapsed to the same error type. Retention
// must not change the aggregate/owner identities, five-line threshold or tier.
func TestPanicProviderEgressNaturalErrorJsonRetainsFiniteClass(t *testing.T) {
	for _, test := range []struct{ message, class string }{
		{message: "provider egress stats refresh in progress", class: "provider_egress_refresh_busy"},
		{message: "provider egress stats unavailable", class: "provider_egress_unavailable"},
		{message: "provider egress stats snapshot invalid", class: "provider_egress_snapshot_invalid"},
	} {
		for _, version := range []string{"", "/v2026"} {
			tailer := newLogTailer("taskworker", nil)
			line := panicProviderTestLine(errors.New(test.message), panicProviderTestStack(version))
			for range 5 {
				tailer.classify(line)
			}
			if sample := tailer.classSamples["panic"]; !strings.Contains(sample, "error_class="+test.class) || len(sample) > 200 {
				t.Fatalf("finite refusal was lost from bounded natural sample: class=%s", test.class)
			}
			frames := map[string]bool{}
			for _, finding := range tailer.drainWindow() {
				if finding.class != "panic" || finding.healthy {
					continue
				}
				alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
				frames[alert.Frame] = true
				if alert.Severity != SeverityPage || alert.Target != "taskworker" || alert.SignalID != "logs/panic" || !strings.Contains(alert.Observed, "rate=5/min") {
					t.Fatal("finite refusal changed panic severity, rate, identity or service")
				}
				if !strings.Contains(alert.Evidence, "error_class="+test.class) || !strings.Contains(alert.Evidence, "error_type=*errors.errorString sqlstate=unknown") {
					t.Fatal("finite refusal was lost between decoder, sample and alert")
				}
				panicProviderRequirePrivate(t, alert.Markdown())
			}
			if len(frames) != 2 || !frames[""] || !frames["server"+version+"/controller.statsRefreshProviderEgress"] {
				t.Fatal("finite refusal merged or removed overlapping aggregate and owner views")
			}
		}
	}
}

// A sample explains only that line, not every line in its overlapping bucket.
func TestPanicProviderEgressMixedSampleKeepsAggregateMeaning(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for range 3 {
		tailer.classify(panicProviderTestLine(errors.New("provider egress stats refresh in progress"), panicProviderTestStack("/v2026")))
	}
	for range 2 {
		tailer.classify("panic: synthetic-secret SELECT private synthetic-customer")
	}
	active := 0
	for _, finding := range tailer.drainWindow() {
		if finding.class != "panic" || finding.healthy {
			continue
		}
		active++
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
		if alert.Frame != "" || alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "rate=5/min") || !strings.Contains(alert.Evidence, "error_class=provider_egress_refresh_busy") {
			t.Fatal("mixed aggregate was suppressed or promoted to an owner-qualified page")
		}
		panicProviderRequirePrivate(t, alert.Markdown())
	}
	if active != 1 {
		t.Fatal("mixed aggregate page disappeared")
	}
}

// A healthy zero-error window and a four-event threshold edge remain unchanged.
func TestPanicProviderEgressThresholdAndHealthyControls(t *testing.T) {
	for _, count := range []int{0, 4} {
		tailer := newLogTailer("taskworker", nil)
		for range count {
			tailer.classify(panicProviderTestLine(errors.New("provider egress stats refresh in progress"), panicProviderTestStack("")))
		}
		for _, finding := range tailer.drainWindow() {
			if finding.class == "panic" && !finding.healthy {
				t.Fatal("finite diagnostic changed the existing threshold")
			}
		}
	}
}

// The exact literal cannot supply ownership, and a later valid caller cannot
// repair an unknown or mismatched inner source record.
func TestPanicProviderEgressRejectsUnprovedSourcePairs(t *testing.T) {
	for _, change := range []func([]string) []string{
		func(stack []string) []string { return stack[:9] },
		func(stack []string) []string {
			stack[9] = strings.Replace(stack[9], "statsRefreshProviderEgress", "SyntheticOther", 1)
			return stack
		},
		func(stack []string) []string { stack[10] = "/synthetic-private-path/other.go:836"; return stack },
		func(stack []string) []string { stack[4] = "/synthetic-private-path/other.go:138"; return stack },
		func(stack []string) []string { stack[8] = "/synthetic-private-path/util.go:0"; return stack },
		func(stack []string) []string {
			stack[10] = "/synthetic-private-path/stats_collector.go:1000001"
			return stack
		},
		func(stack []string) []string {
			stack[3] = "github.com/urnetwork/connect.HandleError(0x12345678)"
			return stack
		},
		func(stack []string) []string {
			stack[3] = "github.com/urnetwork/server/v2027.HandleError(0x12345678)"
			return stack
		},
		func(stack []string) []string {
			stack[3] = "github.com/urnetwork/server.UnknownWrapper(0x12345678)"
			return stack
		},
		func(stack []string) []string {
			return append([]string{"github.com/urnetwork/server/model.SyntheticWrite(0x12345678)", "/synthetic-private-path/model.go:1"}, stack...)
		},
	} {
		line := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), change(panicProviderTestStack("/v2026")))
		if strings.Contains(panicLogSample(line), "error_class=provider_egress_") {
			t.Fatal("unproved source pair entered retained evidence")
		}
		panicProviderRequirePrivate(t, panicLogSample(line))
	}
}

// Ownership wrappers are skipped only with their exact file; they cannot
// create a business caller when the caller is absent.
func TestPanicProviderEgressOwnedWrapperKeepsCallerAuthority(t *testing.T) {
	stack := panicProviderTestStack("/v2026")
	wrapper := []string{"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy.func1(0x12345678)", "/synthetic-private-path/db_owned_transaction.go:172"}
	stack = append(stack[:9], append(wrapper, stack[9:]...)...)
	line := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack)
	if observation := parsePanicLogObservation(line); !strings.Contains(panicLogSample(line), "error_class=provider_egress_refresh_busy") || observation.owner != "server/v2026/controller.statsRefreshProviderEgress" {
		t.Fatal("reviewed wrapper handling lost the actual caller")
	}
	if strings.Contains(panicLogSample(panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack[:11])), "error_class=provider_egress_") {
		t.Fatal("wrapper-only stack invented a provider-egress owner")
	}
}

// No raw text, joined/wrapped type, SQL phrase, or near-match expands the
// finite source vocabulary. Unknown records still count as ordinary panics.
func TestPanicProviderEgressExactErrorAndSqlCanaries(t *testing.T) {
	for _, errorText := range []string{
		"*errors.errorString=provider egress stats refresh in progress trailing",
		"*errors.errorString=provider egress stats refresh in progress\n",
		"*fmt.wrapError=provider egress stats refresh in progress",
		"*errors.joinError=provider egress stats refresh in progress",
		"string=provider egress stats refresh in progress",
		"*pgconn.PgError=SELECT private synthetic-secret (SQLSTATE 25006)",
		"*errors.errorString=SELECT private synthetic-secret provider egress stats refresh in progress (SQLSTATE 25006)",
	} {
		body, err := json.Marshal(map[string]any{"error": errorText, "stack": panicProviderTestStack("/v2026")})
		if err != nil {
			t.Fatal("synthetic encoding failed")
		}
		line := "Unexpected error: " + string(body)
		if strings.Contains(panicLogSample(line), "error_class=provider_egress_") {
			t.Fatal("nonexact error acquired the finite cache class")
		}
		panicProviderRequirePrivate(t, panicLogSample(line))
		tailer := newLogTailer("taskworker", nil)
		for range 5 {
			tailer.classify(line)
		}
		if finding := findingByClass(t, tailer.drainWindow(), "panic"); finding.healthy || finding.tier != tierPage {
			t.Fatal("unclassified error lost the original panic alert")
		}
	}
}

// Malformed, duplicated and over-cap input never gains finite attribution.
func TestPanicProviderEgressMalformedAndCappedStayUnknown(t *testing.T) {
	valid := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), panicProviderTestStack("/v2026"))
	for _, line := range []string{
		valid[:len(valid)-1],
		strings.Replace(valid, `"error":`, `"error":"synthetic-secret","error":`, 1),
		strings.Replace(valid, `"stack":`, `"stack":[],"stack":`, 1),
		strings.Replace(valid, `"stack":[`, `"stack":[null,`, 1),
		strings.Repeat("x", panicLogMaxBytes) + valid,
		strings.Replace(valid, "synthetic-private-path", "synthetic-private-path\xff", 1),
		"Unexpected error: {",
	} {
		if strings.Contains(panicLogSample(line), "error_class=provider_egress_") {
			t.Fatal("incomplete or malformed source gained a finite error class")
		}
		panicProviderRequirePrivate(t, panicLogSample(line))
	}
	stack := append(panicProviderTestStack("/v2026"), make([]string, panicLogMaxStackLines)...)
	if strings.Contains(panicLogSample(panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack)), "error_class=provider_egress_") {
		t.Fatal("capped stack acquired finite source attribution")
	}
}
