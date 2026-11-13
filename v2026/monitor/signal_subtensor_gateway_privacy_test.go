// Gateway RPC errors retain only fixed method names through the actual public
// signal and every renderer. Source messages never supply Alert field text.
package monitor

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every selected method remains a failure even when its message is private.
func TestSubtensorGatewayErrorMessagesStayPrivate(t *testing.T) {
	const privateMessage = "synthetic-private-rpc-error Authorization=Bearer synthetic-rpc-token https://gateway.fixture.example/private?signature=synthetic-signed-value\n## synthetic-injected-heading"
	for _, method := range []string{"healthz", "chain", "genesis", "head", "runtime", "evm_chain_id", "eth_get_logs"} {
		observation := healthySubtensorObservation()
		observation.Nodes[0].Gateway.Errors[method] = privateMessage
		alerts, err := runSyntheticSubtensor(t, observation)
		if err != nil {
			t.Fatal("synthetic gateway observation failed")
		}
		gateway := requireAlertClass(t, alerts, "subtensor-gateway")
		if gateway.SignalID != "subtensor/node-health" || gateway.Target != "chain.example.test" || gateway.Frame != "archive" || gateway.Severity != SeverityPage || gateway.Sustain != 2 {
			t.Fatal("private error projection changed the gateway identity or failure band")
		}
		for _, alert := range alerts {
			encoded, encodeErr := json.Marshal(alert)
			if encodeErr != nil {
				t.Fatal("synthetic alert serialization failed")
			}
			rendered := alert.Markdown() + string(encoded)
			for _, forbidden := range []string{privateMessage, "synthetic-private-rpc-error", "synthetic-rpc-token", "gateway.fixture.example", "synthetic-signed-value", "synthetic-injected-heading"} {
				if strings.Contains(rendered, forbidden) {
					t.Fatal("gateway response message reached a public Alert field")
				}
			}
		}
		var jsonl strings.Builder
		if err := alerts.WriteJSONL(&jsonl); err != nil {
			t.Fatal("synthetic JSONL serialization failed")
		}
		if strings.Contains(jsonl.String(), "synthetic-private-rpc-error") || strings.Contains(jsonl.String(), "synthetic-rpc-token") {
			t.Fatal("gateway response message reached JSONL")
		}
		if gateway.Observed != "gateway_http=200 failed_methods="+method {
			t.Fatal("fixed failed-method evidence was lost")
		}
	}
}

// Fixed method order is independent of the remote map's iteration order.
func TestSubtensorGatewayErrorsKeepAllFixedMethodFailures(t *testing.T) {
	observation := healthySubtensorObservation()
	for _, method := range []string{"eth_get_logs", "runtime", "head", "genesis", "evm_chain_id", "chain", "healthz"} {
		observation.Nodes[0].Gateway.Errors[method] = "synthetic-private-rpc-error"
	}
	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal("synthetic gateway observation failed")
	}
	gateway := requireAlertClass(t, alerts, "subtensor-gateway")
	if gateway.Observed != "gateway_http=200 failed_methods=healthz,chain,genesis,head,runtime,evm_chain_id,eth_get_logs" {
		t.Fatal("gateway projection lost or reordered a fixed method failure")
	}
}

// No message is needed to retain an independently received failed HTTP status.
func TestSubtensorGatewayStatusOnlyFailureRemainsVisible(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Nodes[0].GatewayHTTP = 503
	observation.Nodes[0].Gateway.Errors = nil
	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal("synthetic gateway observation failed")
	}
	gateway := requireAlertClass(t, alerts, "subtensor-gateway")
	if gateway.Observed != "gateway_http=503 failed_methods=none" || gateway.Severity != SeverityPage || gateway.Sustain != 2 {
		t.Fatal("status-only failure acquired method authority or lost its band")
	}
}

// The method allowlist and successful observation keep their current scope.
func TestSubtensorGatewayHealthyAndUnknownMethodControls(t *testing.T) {
	for _, extraMessage := range []bool{false, true} {
		observation := healthySubtensorObservation()
		if extraMessage {
			observation.Nodes[0].Gateway.Errors["synthetic-unselected-method"] = "synthetic-private-rpc-error"
		}
		alerts, err := runSyntheticSubtensor(t, observation)
		if err != nil || len(alerts) != 0 {
			t.Fatal("healthy or unselected method acquired a new failure")
		}
	}
}

// A gateway projection cannot hide an independently observed peer failure.
func TestSubtensorGatewayPrivateErrorPreservesIndependentPeerFailure(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Nodes[0].Gateway.Errors["chain"] = "synthetic-private-rpc-error"
	observation.Nodes[1].Direct.Health.Peers = 0
	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal("synthetic gateway observation failed")
	}
	gateway := requireAlertClass(t, alerts, "subtensor-gateway")
	peer := requireAlertClass(t, alerts, "subtensor-peers")
	if gateway.Frame != "archive" || peer.Frame != "lightnode" || peer.Sustain != 3 || peer.PageSustain != 5 {
		t.Fatal("public error projection changed an independent finding")
	}
}

// Historical runtime remains mandatory, while pre-Frontier EVM methods stay
// outside the active method set. No remote message becomes public evidence.
func TestSubtensorGatewayHistoricalRuntimeErrorMessagesStayPrivate(t *testing.T) {
	const privateMessage = "synthetic-private-historical-rpc-error Authorization=Bearer synthetic-historical-token https://historical.fixture.example/private?signature=synthetic-historical-signature\n## synthetic-historical-heading"
	observation := healthySubtensorObservation()
	archive := &observation.Nodes[0]
	archive.FirstHead = blockHex(6_900_000)
	archive.SecondHead = blockHex(6_900_010)
	archive.GatewayFirstHead = archive.FirstHead
	archive.Direct.Head = archive.FirstHead
	archive.Gateway.Head = archive.SecondHead
	archive.Direct.Health.IsSyncing = true
	archive.Direct.Sync = subtensorSyncState{CurrentBlock: 6_900_010, HighestBlock: 7_910_000}
	archive.Direct.Runtime.SpecVersion = 440
	archive.Gateway.Runtime = archive.Direct.Runtime
	for _, method := range []string{"runtime", "evm_chain_id", "eth_get_logs"} {
		archive.Gateway.Errors[method] = privateMessage
	}
	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal("synthetic historical gateway observation failed")
	}
	gateway := requireAlertClass(t, alerts, "subtensor-gateway")
	if gateway.SignalID != "subtensor/node-health" || gateway.Target != "chain.example.test" || gateway.Frame != "archive" ||
		gateway.Severity != SeverityPage || gateway.Sustain != 2 || gateway.Observed != "gateway_http=200 failed_methods=runtime" {
		t.Fatal("historical gateway error lost required runtime metadata or adopted pre-Frontier method failures")
	}
	runtimeUnknown := false
	for _, alert := range alerts {
		if alert.SignalID == "monitor/visibility" && alert.Class == "cannot-observe" &&
			alert.Target == "chain.example.test/archive/gateway-runtime" && alert.unobservable {
			runtimeUnknown = true
		}
		if alert.Class == "subtensor-identity" && alert.Frame == "archive-gateway" {
			t.Fatal("unavailable historical runtime became a measured identity mismatch")
		}
		encoded, encodeErr := json.Marshal(alert)
		if encodeErr != nil {
			t.Fatal("synthetic historical alert serialization failed")
		}
		rendered := alert.Markdown() + string(encoded)
		for _, forbidden := range []string{privateMessage, "synthetic-private-historical-rpc-error", "synthetic-historical-token", "historical.fixture.example", "synthetic-historical-signature", "synthetic-historical-heading"} {
			if strings.Contains(rendered, forbidden) {
				t.Fatal("historical RPC error message reached a public Alert field")
			}
		}
	}
	if !runtimeUnknown {
		t.Fatal("historical RPC runtime failure did not retain independent visibility")
	}
	var jsonl strings.Builder
	if err := alerts.WriteJSONL(&jsonl); err != nil {
		t.Fatal("synthetic historical JSONL serialization failed")
	}
	for _, forbidden := range []string{"synthetic-private-historical-rpc-error", "synthetic-historical-token", "historical.fixture.example", "synthetic-historical-signature", "synthetic-historical-heading"} {
		if strings.Contains(jsonl.String(), forbidden) {
			t.Fatal("historical RPC error message reached JSONL")
		}
	}
}
