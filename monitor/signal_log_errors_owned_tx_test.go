package monitor

import (
	"encoding/json"
	"strings"
	"testing"
)

// Owned-transaction admission and recovery adapters do not identify the business
// caller. Exact wrapper pairs are skipped at ordinary and versioned roots.
func TestPanicOwnerOwnedTransactionWrappersRetainCaller(t *testing.T) {
	for _, module := range []string{"server/v2026", "server"} {
		for _, wrapper := range []string{
			"ownedTxWithResourcePolicy.func1", "ownedTxWithResource", "OwnedTx", "TryOwnedTx",
			"(*PgOwnedSession).Tx", "(*PgOwnedSession).txWithResource.func2",
			"(*pgOwnedConnection).run.func1",
		} {
			for _, source := range []string{
				"/synthetic-private-path/owner.go:12345 +0x99",
				"github.com/urnetwork/" + module + "/db_owned_transaction.go:12345 +0x99",
			} {
				caller := module + "/model.SyntheticWork"
				line := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
					"github.com/urnetwork/"+module+"."+wrapper,
					"github.com/urnetwork/"+caller+".func7")
				line = strings.ReplaceAll(line, "/synthetic-private-path/owner.go:12345 +0x99", source)
				observation := parsePanicLogObservation(line)
				if observation.owner != caller || observation.errorType != "*errors.errorString" || observation.sqlstate != "unknown" {
					t.Fatalf("owned transaction adapter hid business caller: wrapper=%s module=%s observation=%#v", wrapper, module, observation)
				}
			}
		}
	}
}

// The aggregate's first exemplar is independent of a later owner bucket. A
// corrected caller keeps its page and cannot borrow that first sample's owner.
func TestPanicOwnerOwnedTransactionKeepsAggregateAndMatchingSamples(t *testing.T) {
	tailer := newLogTailer("api", nil)
	otherLine := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
		"github.com/urnetwork/server/v2026/controller.SyntheticGauge")
	for range 2 {
		tailer.classify(otherLine)
	}
	line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret",
		"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy.func1",
		"github.com/urnetwork/server/v2026.ownedTxWithResource",
		"github.com/urnetwork/server/v2026.OwnedTx",
		"github.com/urnetwork/server/v2026/model.SyntheticWork")
	for range 7 {
		tailer.classify(line)
	}
	if tailer.classCounts["panic"] != 9 || tailer.classCounts["panic\x00server/v2026/model.SyntheticWork"] != 7 {
		t.Fatal("owned wrapper correction changed overlapping diagnostic counts")
	}
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 2 || !strings.Contains(alerts[""].Observed, "rate=9/min") ||
		!strings.Contains(alerts[""].Evidence, "owner=server/v2026/controller.SyntheticGauge error_type=*errors.errorString") {
		t.Fatal("owned wrapper correction lost aggregate page or borrowed its sample")
	}
	callerAlert := alerts["server/v2026/model.SyntheticWork"]
	if !strings.Contains(callerAlert.Observed, "rate=7/min") ||
		!strings.Contains(callerAlert.Evidence, "owner=server/v2026/model.SyntheticWork error_type=unknown sqlstate=unknown") {
		t.Fatal("owned caller page borrowed an unrelated sample or invented an error cause")
	}
	for _, alert := range alerts {
		for _, want := range []string{"counts overlap", "business caller and cause unknown", "changed owner after parser correction is not recovery"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("owned wrapper authority qualifier missing %q", want)
			}
		}
		for _, private := range []string{"ownedTxWithResourcePolicy", "syntheticprivate.Error"} {
			requireAlertOmits(t, alert, private)
		}
	}
}

// Wrapper-only, truncated and unsupported remaining stacks keep the original
// page without manufacturing a caller. A malformed wrapper source stops too.
func TestPanicOwnerOwnedTransactionUnknownCallerKeepsAggregate(t *testing.T) {
	valid := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
		"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy",
		"github.com/urnetwork/server/v2026.OwnedTx",
		"github.com/urnetwork/server/v2026/model.SyntheticWork")
	for _, line := range []string{
		panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
			"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy",
			"github.com/urnetwork/server/v2026.OwnedTx"),
		panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
			"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy",
			"github.com/urnetwork/unlisted.SyntheticUnknown",
			"github.com/urnetwork/server/v2026/model.SyntheticWork"),
		panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
			"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy",
			"github.com/urnetwork/server/v2026/model.SyntheticUnknown[not-supported]",
			"github.com/urnetwork/server/v2026/model.SyntheticWork"),
		valid[:len(valid)-1],
		strings.Replace(valid, "/synthetic-private-path/owner.go:12345 +0x99", "synthetic-secret", 1),
	} {
		tailer := newLogTailer("api", nil)
		for range 5 {
			tailer.classify(line)
		}
		alerts := panicOwnerTestAlerts(t, tailer)
		if len(alerts) != 1 || !strings.Contains(alerts[""].Observed, "rate=5/min") ||
			!strings.Contains(alerts[""].Evidence, "owner=unknown") {
			t.Fatal("owned wrapper without a validated caller invented ownership or lost aggregate page")
		}
	}
}

// Pair validation still precedes wrapper skipping; it cannot consume a malformed
// source record and then borrow the next application's apparent ownership.
func TestPanicOwnerOwnedTransactionMalformedSourceDoesNotBorrowCaller(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"error": "*errors.errorString=synthetic-secret",
		"stack": []string{
			"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy(0x12345678)",
			"github.com/urnetwork/server/v2026/db_owned_transaction.go:unknown",
			"github.com/urnetwork/server/v2026/model.SyntheticWork(0x12345678)",
			"github.com/urnetwork/server/v2026/model/work.go:12345 +0x99",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if owner := panicLogOwner("Unexpected error: " + string(body)); owner != "" {
		t.Fatalf("malformed owned wrapper source borrowed caller %q", owner)
	}
}

// Exact roots and methods matter: an application function with the same short
// name or a different ownership method still owns its own diagnostic.
func TestPanicOwnerOwnedTransactionLookalikesRemainApplicationOwners(t *testing.T) {
	for _, function := range []string{
		"server/model.OwnedTx",
		"server/v2026/model.ownedTxWithResourcePolicy",
		"server.PgOwnedSession.SyntheticWork",
		"server/v2026.pgOwnedConnection.SyntheticWork",
		"connect.OwnedTx",
	} {
		line := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
			"github.com/urnetwork/"+function,
			"github.com/urnetwork/server/model.SyntheticOuter")
		if owner := panicLogOwner(line); owner != function {
			t.Fatalf("owned wrapper lookalike lost application owner: got %q want %q", owner, function)
		}
	}
}

// Known wrappers alone are never counted twice in the aggregate or allowed to
// cross the unchanged threshold early.
func TestPanicOwnerOwnedTransactionPreservesThresholdEdge(t *testing.T) {
	tailer := newLogTailer("api", nil)
	line := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
		"github.com/urnetwork/server/v2026.ownedTxWithResourcePolicy")
	for range 4 {
		tailer.classify(line)
	}
	if alerts := panicOwnerTestAlerts(t, tailer); len(alerts) != 0 {
		t.Fatal("owned wrapper correction crossed the panic threshold early")
	}
	if alerts := panicOwnerTestAlerts(t, tailer); len(alerts) != 0 {
		t.Fatal("owned wrapper correction carried a prior window into empty observation")
	}
}
