package monitor

// Connection reuse moved transaction plumbing behind an additional wrapper.
// Synthetic source pairs reproduce its effect on the real decoder and alerts.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The actual pool path and explicit-owner paths all retain their application
// caller through ordinary, closure and trimpath stack forms.
func TestPanicOwnerConnectionWrappersRetainCaller(t *testing.T) {
	for _, module := range []string{"server", "server/v2026"} {
		for _, wrapper := range []string{"txWithConnection", "txWithConnection.func1.1", "TxInConn", "TxInConn.func1", "DbInConn"} {
			for _, source := range []string{"/synthetic-private-path/owner.go:12345 +0x99", "github.com/urnetwork/" + module + "/db.go:12345 +0x99"} {
				caller := module + "/model.SyntheticConnectionWork"
				line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret",
					"github.com/urnetwork/"+module+"."+wrapper,
					"github.com/urnetwork/"+caller+".func3")
				line = strings.ReplaceAll(line, "/synthetic-private-path/owner.go:12345 +0x99", source)
				observation := parsePanicLogObservation(line)
				if observation.owner != caller || observation.errorType != "unknown" || observation.sqlstate != "unknown" {
					t.Fatalf("connection wrapper hid the application caller: got=%q want=%q wrapper=%s module=%s", observation.owner, caller, wrapper, module)
				}
			}
		}
	}
}

// Seven events keep both overlapping views, with no invented error cause or
// connection between an aggregate exemplar and other independently owned work.
func TestPanicOwnerConnectionWrappersPreservePagesAndPrivacy(t *testing.T) {
	tailer := newLogTailer("proxy", nil)
	line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret SELECT private synthetic-customer 192.0.2.88",
		"github.com/urnetwork/server/v2026.txWithConnection.func1.1",
		"github.com/urnetwork/server/v2026.TxInConn.func1",
		"github.com/urnetwork/server/v2026.txWithConnection",
		"github.com/urnetwork/server/v2026.TxInConn",
		"github.com/urnetwork/server/v2026/model.SyntheticConnectionWork")
	for range 7 {
		tailer.classify(line)
	}
	caller := "server/v2026/model.SyntheticConnectionWork"
	if tailer.classCounts["panic"] != 7 || tailer.classCounts["panic\x00"+caller] != 7 {
		t.Fatal("wrapper correction changed the overlapping event counts")
	}
	frames := map[string]bool{}
	for _, finding := range tailer.drainWindow() {
		if finding.class != "panic" || finding.healthy {
			continue
		}
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
		frames[alert.Frame] = true
		if alert.Severity != SeverityPage || alert.Target != "proxy" || alert.SignalID != "logs/panic" || !strings.Contains(alert.Observed, "rate=7/min") {
			t.Fatal("wrapper correction changed service, threshold, tier or signal")
		}
		if !strings.Contains(alert.Evidence, "owner="+caller+" error_type=unknown sqlstate=unknown") || !strings.Contains(alert.Markdown(), "counts overlap") {
			t.Fatal("caller evidence was lost or unknown error cause changed")
		}
		for _, private := range []string{"synthetic-private-host", "synthetic-generation", "synthetic-correlation", "synthetic-private-path", "synthetic-secret", "synthetic-customer", "SELECT private", "192.0.2.88", "0x12345678", "0x87654321", "syntheticprivate.Error", "txWithConnection", "TxInConn"} {
			requireAlertOmits(t, alert, private)
		}
	}
	if len(frames) != 2 || !frames[""] || !frames[caller] {
		t.Fatal("aggregate or resolved caller page disappeared")
	}
}

// No remaining application frame means unknown, including a malformed frame
// that appears before an otherwise recognizable outer caller.
func TestPanicOwnerConnectionWrappersUnknownStayAggregate(t *testing.T) {
	valid := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret",
		"github.com/urnetwork/server/v2026.txWithConnection.func1.1",
		"github.com/urnetwork/server/v2026.TxInConn",
		"github.com/urnetwork/server/v2026/model.SyntheticConnectionWork")
	for _, line := range []string{
		panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.txWithConnection.func1.1", "github.com/urnetwork/server/v2026.TxInConn", "github.com/urnetwork/server/v2026.DbInConn"),
		panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.txWithConnection", "github.com/urnetwork/unlisted.SyntheticUnknown", "github.com/urnetwork/server/v2026/model.SyntheticOuter"),
		panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.txWithConnection", "github.com/urnetwork/server/v2026/model.SyntheticUnknown[unsupported]", "github.com/urnetwork/server/v2026/model.SyntheticOuter"),
		valid[:len(valid)-1],
		strings.Replace(valid, "/synthetic-private-path/owner.go:12345 +0x99", "synthetic-secret", 1),
		strings.Replace(valid, `"error":`, `"error":"synthetic-secret","error":`, 1),
		strings.Repeat("x", panicLogMaxBytes) + valid,
	} {
		tailer := newLogTailer("api", nil)
		for range 5 {
			tailer.classify(line)
		}
		alerts := panicOwnerTestAlerts(t, tailer)
		if len(alerts) != 1 || !strings.Contains(alerts[""].Observed, "rate=5/min") || !strings.Contains(alerts[""].Evidence, "owner=unknown") {
			t.Fatal("unproved connection caller gained an owner or lost aggregate visibility")
		}
	}
}

// A malformed new wrapper source cannot consume the next application record.
func TestPanicOwnerConnectionMalformedSourceStopsAttribution(t *testing.T) {
	for _, wrapper := range []string{"txWithConnection", "TxInConn", "DbInConn"} {
		body, err := json.Marshal(map[string]any{
			"error": "*syntheticprivate.Error=synthetic-secret",
			"stack": []string{
				"github.com/urnetwork/server/v2026." + wrapper + "(0x12345678)",
				"github.com/urnetwork/server/v2026/db.go:unknown",
				"github.com/urnetwork/server/v2026/model.SyntheticOuter(0x12345678)",
				"github.com/urnetwork/server/v2026/model/model.go:12345",
			},
		})
		if err != nil {
			t.Fatal("synthetic fixture encoding failed")
		}
		if owner := panicLogOwner("Unexpected error: " + string(body)); owner != "" {
			t.Fatal("malformed connection wrapper borrowed a later caller")
		}
	}
}

// Same short names in a business package, another module or another method
// still identify their own application boundary.
func TestPanicOwnerConnectionLookalikesRemainOwners(t *testing.T) {
	for _, function := range []string{"server/model.txWithConnection", "server/v2026/model.TxInConn", "server/controller.DbInConn", "connect.TxInConn", "server.txWithConnectionOther", "server.SyntheticConnection.TxInConn"} {
		line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/"+function, "github.com/urnetwork/server/model.SyntheticOuter")
		if owner := panicLogOwner(line); owner != function {
			t.Fatal("connection wrapper lookalike lost its application ownership")
		}
	}
}

// Empty windows and four-event threshold edges retain their previous behavior.
func TestPanicOwnerConnectionThresholdAndHealthyControls(t *testing.T) {
	for _, count := range []int{0, 4} {
		tailer := newLogTailer("api", nil)
		line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.txWithConnection")
		for range count {
			tailer.classify(line)
		}
		if alerts := panicOwnerTestAlerts(t, tailer); len(alerts) != 0 {
			t.Fatal("wrapper correction changed threshold or healthy-window behavior")
		}
	}
}

// The independent finite classifier admits each new wrapper only with its
// reviewed source file; generic ownership cannot supply a finite error cause.
func TestPanicOwnerConnectionFiniteClassifierRequiresSourcePairs(t *testing.T) {
	for _, wrapper := range []struct{ symbol, file string }{
		{symbol: "txWithConnection.func1.1", file: "db.go"},
		{symbol: "TxInConn.func1", file: "db_connection_owner.go"},
		{symbol: "DbInConn", file: "db_connection_owner.go"},
	} {
		for _, validFile := range []bool{true, false} {
			stack := panicProviderTestStack("/v2026")
			file := wrapper.file
			if !validFile {
				file = "synthetic_other.go"
			}
			pair := []string{"github.com/urnetwork/server/v2026." + wrapper.symbol + "(0x12345678)", "/synthetic-private-path/" + file + ":12345"}
			stack = append(stack[:9], append(pair, stack[9:]...)...)
			line := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack)
			classified := strings.Contains(panicLogSample(line), "error_class=provider_egress_refresh_busy")
			if classified != validFile {
				t.Fatal("finite classifier ignored the connection wrapper's exact source-file contract")
			}
			panicProviderRequirePrivate(t, panicLogSample(line))
			if validFile {
				wrapperOnly := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack[:11])
				if strings.Contains(panicLogSample(wrapperOnly), "error_class=provider_egress_") {
					t.Fatal("connection wrapper alone invented a finite source owner")
				}
				foreign := strings.Replace(line, "statsRefreshProviderEgress", "SyntheticOther", 1)
				if strings.Contains(panicLogSample(foreign), "error_class=provider_egress_") {
					t.Fatal("connection wrapper gave a foreign caller the stats literal")
				}
			}
		}
	}
}
