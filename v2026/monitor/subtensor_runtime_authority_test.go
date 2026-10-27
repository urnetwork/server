// Runtime result presence belongs to the observed RPC surface. These controls
// exercise the real parser and Signal without contacting a host or network.
package monitor

import (
	"encoding/json"
	"strings"
	"testing"
)

// Change one surface in an otherwise complete synthetic producer document.
func subtensorRuntimeDocument(t testing.TB, surface, runtime string, wrongChain bool) string {
	t.Helper()
	encoded, err := json.Marshal(healthySubtensorObservation())
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	rpc := document["public"].(map[string]any)
	if surface != "public" {
		rpc = document["nodes"].([]any)[0].(map[string]any)[surface].(map[string]any)
	}
	if runtime == "missing" {
		delete(rpc, "runtime")
	} else {
		var value any
		if err := json.Unmarshal([]byte(runtime), &value); err != nil {
			t.Fatal(err)
		}
		rpc["runtime"] = value
	}
	if wrongChain {
		rpc["chain"] = "Synthetic wrong chain"
	}
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Source injection returns raw wire bytes, so omitted/null members cannot be
// restored by marshaling the production Go struct before its decoder runs.
func runSubtensorRuntimeDocument(t testing.TB, document string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, subtensorMarker) {
			t.Fatal("unexpected synthetic source command")
		}
		return document, nil
	}}
	alerts, err := NewSubtensorSignal().Run(t.Context(), subtensorSyntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Missing data must not become an empty runtime name or a zero version page.
func TestSubtensorRuntimeMissingFieldsRemainUnknown(t *testing.T) {
	for _, surface := range []string{"public", "direct", "gateway"} {
		for _, runtime := range []string{
			"missing", "null", `{}`, `{"specName":""}`,
			`{"specName":"node-subtensor","specVersion":452}`,
			`{"specName":"node-subtensor","specVersion":null,"transactionVersion":1}`,
			`{"specName":null,"specVersion":452,"transactionVersion":1}`,
		} {
			alerts := runSubtensorRuntimeDocument(t, subtensorRuntimeDocument(t, surface, runtime, false))
			unknown := false
			for _, alert := range alerts {
				unknown = unknown || alert.Class == "cannot-observe"
				if alert.Class != "cannot-observe" {
					t.Errorf("%s runtime=%s fabricated %s: %s", surface, runtime, alert.Class, alert.Observed)
				}
			}
			if !unknown {
				t.Errorf("%s runtime=%s supplied no unknown observation", surface, runtime)
			}
		}
	}
}

// One malformed runtime must not discard independent measurements in the host.
func TestSubtensorRuntimeMalformedResultPreservesIndependentIdentity(t *testing.T) {
	for _, surface := range []string{"public", "direct", "gateway"} {
		for _, runtime := range []string{
			`[]`, `"synthetic-private-runtime"`, `false`,
			`{"specName":"node-subtensor","specVersion":"synthetic-private-version","transactionVersion":1}`,
			`{"specName":true,"specVersion":452,"transactionVersion":1}`,
		} {
			alerts := runSubtensorRuntimeDocument(t, subtensorRuntimeDocument(t, surface, runtime, true))
			unknown, independentIdentity := false, false
			for _, alert := range alerts {
				unknown = unknown || alert.Class == "cannot-observe"
				if alert.Class == "subtensor-identity" && strings.Contains(alert.Observed, "Synthetic wrong chain") {
					independentIdentity = true
				}
				if strings.Contains(alert.Markdown(), "synthetic-private") {
					t.Errorf("%s raw runtime diagnostic escaped fixed visibility reason", surface)
				}
			}
			if !unknown || !independentIdentity {
				t.Errorf("%s runtime=%s lost unknown=%t or independent identity=%t", surface, runtime, unknown, independentIdentity)
			}
		}
	}
}

// A measured member remains authoritative when a sibling member is missing.
func TestSubtensorRuntimePartialResultPreservesMeasuredMismatch(t *testing.T) {
	for _, surface := range []string{"public", "direct", "gateway"} {
		for _, runtime := range []string{
			`{"specName":"synthetic-wrong-runtime"}`,
			`{"specName":"node-subtensor","specVersion":0}`,
			`{"specName":"node-subtensor","transactionVersion":0}`,
		} {
			alerts := runSubtensorRuntimeDocument(t, subtensorRuntimeDocument(t, surface, runtime, false))
			unknown, measuredIdentity := false, false
			for _, alert := range alerts {
				unknown = unknown || alert.Class == "cannot-observe"
				measuredIdentity = measuredIdentity || alert.Class == "subtensor-identity"
			}
			if !unknown || !measuredIdentity {
				t.Errorf("%s runtime=%s lost partial visibility or measured mismatch", surface, runtime)
			}
		}
	}
}

// The normal positive and upstream-ahead paths retain their original authority.
func TestSubtensorRuntimeCompleteControlsRemainAuthoritative(t *testing.T) {
	alerts := runSubtensorRuntimeDocument(t, subtensorRuntimeDocument(t, "public", `{"specName":"node-subtensor","specVersion":452,"transactionVersion":1}`, false))
	if len(alerts) != 0 {
		t.Fatalf("complete runtime produced %d alerts", len(alerts))
	}
	alerts = runSubtensorRuntimeDocument(t, subtensorRuntimeDocument(t, "public", `{"specName":"node-subtensor","specVersion":453,"transactionVersion":1}`, false))
	if len(alerts) != 1 || alerts[0].Class != "subtensor-runtime-ahead" {
		t.Fatal("complete upstream-ahead runtime lost its original page")
	}
}

// Independently measured lag remains visible without labeling unknown runtime
// members as measured zero in either the finding or its identity evidence.
func TestSubtensorRuntimeUnknownPreservesIndependentLag(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal([]byte(subtensorRuntimeDocument(t, "direct", "null", true)), &document); err != nil {
		t.Fatal(err)
	}
	document["public"].(map[string]any)["head"] = blockHex(8_120_000)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	alerts := runSubtensorRuntimeDocument(t, string(encoded))
	unknown, lag, wrongChain := false, false, false
	for _, alert := range alerts {
		unknown = unknown || alert.Class == "cannot-observe"
		if alert.Class == "subtensor-stale-convergence" && alert.Frame == "archive" {
			lag = strings.Contains(alert.Observed, "runtime=unknown/unknown transaction=unknown")
		}
		if alert.Class == "subtensor-identity" && alert.Frame == "archive" {
			wrongChain = strings.Contains(alert.Observed, "Synthetic wrong chain") && strings.Contains(alert.Evidence, "runtime=unknown/unknown transaction=unknown")
		}
	}
	if !unknown || !lag || !wrongChain {
		t.Fatal("unknown runtime concealed independent lag/identity or rendered invented zero versions")
	}
}
