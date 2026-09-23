package monitor

import (
	"encoding/json"
	"strings"
	"testing"
)

// A trimpath build emits module-relative source paths after function records.
// All data is synthetic; no production stack or identity is a fixture.
func panicOwnerTrimpathTestLine(t *testing.T, innerFunction, innerSource string) string {
	t.Helper()
	stack := []string{
		"goroutine 42 [running]:",
		"runtime/debug.Stack()",
		"runtime/debug/stack.go:26 +0x5e",
		"github.com/urnetwork/server.HandleError.func1(0x12345678)",
		"github.com/urnetwork/server/trace.go:58 +0x85",
		"panic({0x12345678?, 0x87654321?})",
		"runtime/panic.go:860 +0x13a",
		"github.com/urnetwork/server.dbWithPool.func1.2(0x12345678)",
		"github.com/urnetwork/server/db.go:619 +0x1a5",
		"github.com/urnetwork/server.txWithPool.func1.1(0x12345678)",
		"github.com/urnetwork/server/db.go:767 +0x74",
		"github.com/urnetwork/server.Raise(...)",
		"github.com/urnetwork/server/util.go:154",
		innerFunction + "(0x12345678, 0x87654321)",
		innerSource,
		"github.com/urnetwork/server/model.SyntheticOuter(0x12345678)",
		"github.com/urnetwork/server/model/synthetic_outer.go:12345 +0x99",
	}
	body, err := json.Marshal(map[string]any{
		"error": "*errors.errorString=Escrow does not have enough value to pay out the full amount. synthetic-secret",
		"stack": stack,
	})
	if err != nil {
		t.Fatal(err)
	}
	return "[synthetic-private-host][taskworker][synthetic-generation][cid:synthetic-correlation] Unexpected error: " + string(body)
}

// Recovery wrappers own the following validated source record as a pair;
// that source record must not become an unknown function before the real owner.
func TestPanicOwnerTrimpathWrappersRetainApplicationOwner(t *testing.T) {
	line := panicOwnerTrimpathTestLine(t,
		"github.com/urnetwork/server/model.ForceCloseOpenContractIds.func5.1",
		"github.com/urnetwork/server/model/subscription_model.go:3980 +0x296",
	)
	observation := parsePanicLogObservation(line)
	if observation.owner != "server/model.ForceCloseOpenContractIds" ||
		observation.errorType != "*errors.errorString" || observation.sqlstate != "unknown" {
		t.Fatalf("trimpath projection = %#v, want the exact normalized application owner", observation)
	}
}

// Versioned server module roots must not turn a recovery wrapper into the
// owning application frame; the service-wide panic count remains unchanged.
func TestPanicOwnerVersionedRootSkipsRecoveryWrapper(t *testing.T) {
	line := panicOwnerTrimpathTestLine(t,
		"github.com/urnetwork/server/model.SyntheticWrite.func1",
		"github.com/urnetwork/server/model/synthetic_write.go:123 +0x99",
	)
	line = strings.ReplaceAll(line, "github.com/urnetwork/server", "github.com/urnetwork/server/v2026")
	observation := parsePanicLogObservation(line)
	if observation.owner != "server/v2026/model.SyntheticWrite" || observation.errorType != "*errors.errorString" {
		t.Fatalf("versioned root owner = %#v", observation)
	}
	tailer := newLogTailer("taskworker", nil)
	for range 5 {
		tailer.classify(line)
	}
	frames := map[string]bool{}
	for _, finding := range tailer.drainWindow() {
		if finding.class != "panic" || finding.healthy {
			continue
		}
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
		if alert.Severity != SeverityPage || alert.Target != "taskworker" || alert.SignalID != "logs/panic" {
			t.Fatal("versioned recovery wrapper changed service-wide panic visibility")
		}
		frames[alert.Frame] = true
	}
	if len(frames) != 2 || !frames[""] || !frames["server/v2026/model.SyntheticWrite"] {
		t.Fatalf("versioned root panic frames = %#v, want aggregate and application owner", frames)
	}
}

func TestPanicOwnerVersionedConnectRootSkipsRecoveryWrapper(t *testing.T) {
	line := panicOwnerTestLine(t, "*errors.errorString=synthetic-private-error",
		"github.com/urnetwork/connect/v42.HandleError1[...].func1",
		"github.com/urnetwork/server/model.SyntheticWrite",
	)
	if owner := panicLogOwner(line); owner != "server/model.SyntheticWrite" {
		t.Fatalf("versioned Connect wrapper owner = %q, want application frame", owner)
	}
}

// The recovered accounting rejection is still a failure. Neither this parser
// repair nor recognition of the recovery boundary may lower its existing page.
func TestPanicOwnerTrimpathPreservesAccountingAggregateAndOwnerPages(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	line := panicOwnerTrimpathTestLine(t,
		"github.com/urnetwork/server/model.ForceCloseOpenContractIds.func5.1",
		"github.com/urnetwork/server/model/subscription_model.go:3980 +0x296",
	)
	for range 5 {
		tailer.classify(line)
	}
	frames := map[string]bool{}
	for _, finding := range tailer.drainWindow() {
		if finding.class != "panic" || finding.healthy {
			continue
		}
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
		if alert.Severity != SeverityPage || alert.Target != "taskworker" || alert.SignalID != "logs/panic" ||
			!strings.Contains(alert.Observed, "rate=5/min") {
			t.Fatal("trimpath recognition suppressed or weakened accounting-failure panic visibility")
		}
		if frames[alert.Frame] {
			t.Fatal("duplicate panic frame")
		}
		frames[alert.Frame] = true
		for _, want := range []string{"owner=server/model.ForceCloseOpenContractIds", "error_type=*errors.errorString", "counts overlap", "not proof of a failed primary operation or process crash"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Errorf("panic authority qualifier missing %q", want)
			}
		}
		for _, private := range []string{"synthetic-private-host", "synthetic-generation", "synthetic-correlation",
			"synthetic-secret", "0x12345678", "0x87654321", "subscription_model.go", "3980", "Escrow does not have enough"} {
			requireAlertOmits(t, alert, private)
		}
	}
	if len(frames) != 2 || !frames[""] || !frames["server/model.ForceCloseOpenContractIds"] {
		t.Fatal("trimpath wrappers hid the owning page or split away the unchanged aggregate")
	}
}

// Skip only the paired source record of a known wrapper. An unsupported inner
// application frame must not borrow the valid outer caller's apparent owner.
func TestPanicOwnerTrimpathUnknownInnerDoesNotBorrowCaller(t *testing.T) {
	for _, inner := range []string{
		"github.com/urnetwork/server/model.SyntheticUnknown[not-supported]",
		"github.com/urnetwork/unlisted.SyntheticUnknown",
	} {
		line := panicOwnerTrimpathTestLine(t, inner,
			"github.com/urnetwork/server/model/synthetic_owner.go:12345 +0x99")
		if got := panicLogOwner(line); got != "" {
			t.Fatalf("unsupported inner function borrowed owner %q", got)
		}
	}
}

func TestPanicOwnerTrimpathMalformedSourceRemainsUnknown(t *testing.T) {
	for _, source := range []string{"", "synthetic-secret", "github.com/urnetwork/server/model/owner.go:unknown"} {
		line := panicOwnerTrimpathTestLine(t,
			"github.com/urnetwork/server/model.ForceCloseOpenContractIds.func5.1", source)
		if got := panicLogOwner(line); got != "" {
			t.Fatalf("malformed source authorized owner %q", got)
		}
	}
	valid := panicOwnerTrimpathTestLine(t,
		"github.com/urnetwork/server/model.ForceCloseOpenContractIds.func5.1",
		"github.com/urnetwork/server/model/subscription_model.go:3980 +0x296")
	brokenWrapper := strings.Replace(valid, "github.com/urnetwork/server/trace.go:58 +0x85", "synthetic-secret", 1)
	if got := panicLogOwner(brokenWrapper); got != "" {
		t.Fatalf("unvalidated wrapper source was skipped to owner %q", got)
	}
}

// Raw crashes and malformed JSON remain in the original service-wide bucket;
// a missing owner never turns either into success.
func TestPanicOwnerTrimpathPreservesUnstructuredCrashPage(t *testing.T) {
	for _, line := range []string{"panic: synthetic-secret", "Unexpected error: {"} {
		tailer := newLogTailer("api", nil)
		for range 5 {
			tailer.classify(line)
		}
		alerts := panicOwnerTestAlerts(t, tailer)
		if len(alerts) != 1 || !strings.Contains(alerts[""].Observed, "rate=5/min") {
			t.Fatal("unstructured or malformed panic lost its aggregate page")
		}
	}
}
