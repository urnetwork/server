package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const syntheticSubtensorGenesis = "0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105"

func TestSubtensorSignalHealthySyntheticNodes(t *testing.T) {
	observation := healthySubtensorObservation()
	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy Subtensor produced alerts: %+v", alerts)
	}
}

func TestSubtensorSignalDetectsWarpFallbackAndArchiveLag(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Head = blockHex(7_910_000)
	observation.Nodes[0].Direct.Sync.HighestBlock = 7_910_000
	observation.Nodes[0].Direct.Health.IsSyncing = true
	observation.Nodes[0].Direct.Runtime.SpecVersion = 371
	observation.Nodes[0].FirstHead = blockHex(6_345_000)
	observation.Nodes[0].SecondHead = blockHex(6_345_004)
	observation.Nodes[0].Direct.Head = observation.Nodes[0].FirstHead
	observation.Nodes[0].Gateway.Head = observation.Nodes[0].SecondHead
	observation.Nodes[1].Direct.Sync.HighestBlock = 7_910_000
	observation.Nodes[1].Direct.Health.IsSyncing = true
	observation.Nodes[1].Direct.Runtime.SpecVersion = 365
	observation.Nodes[1].FirstHead = blockHex(6_290_000)
	observation.Nodes[1].SecondHead = blockHex(6_290_006)
	observation.Nodes[1].Direct.Head = observation.Nodes[1].FirstHead
	observation.Nodes[1].Gateway.Head = observation.Nodes[1].SecondHead

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	archive := requireAlertClass(t, alerts, "subtensor-sync-lag")
	if archive.Frame != "archive" || !strings.Contains(archive.Observed, "lag=1564996") {
		t.Fatalf("archive lag alert = %+v", archive)
	}
	lightnode := requireAlertClass(t, alerts, "subtensor-warp-fallback")
	if lightnode.Frame != "lightnode" || !strings.Contains(lightnode.Mechanism, "falls back to full sync") {
		t.Fatalf("lightnode fallback alert = %+v", lightnode)
	}
	if lightnode.Sustain != 15 {
		t.Fatalf("lightnode fallback sustain = %d", lightnode.Sustain)
	}
	if !strings.Contains(lightnode.Action, "run-subtensor-lightnode.sh") ||
		!strings.Contains(lightnode.Action, "recreate only subtensor-lightnode") ||
		!strings.Contains(lightnode.Action, "Do not run the full run-subtensor.sh") {
		t.Fatalf("lightnode action is not root-cause specific: %s", lightnode.Action)
	}
	if !strings.Contains(lightnode.Verify, "unchanged archive container ID/start time") ||
		!strings.Contains(lightnode.Verify, "live /data mount") {
		t.Fatalf("lightnode verification does not preserve the archive boundary: %s", lightnode.Verify)
	}
}

func TestSubtensorSignalDetectsZeroPeersAndFrozenHead(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[0]
	node.Direct.Health.Peers = 0
	node.FirstHead = node.SecondHead
	node.Direct.Head = node.SecondHead
	node.Gateway.Head = node.SecondHead

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	if alert := requireAlertClass(t, alerts, "subtensor-peers"); alert.Sustain != 3 {
		t.Fatalf("peer sustain = %d", alert.Sustain)
	}
	if alert := requireAlertClass(t, alerts, "subtensor-progress"); alert.Frame != "archive" {
		t.Fatalf("progress alert = %+v", alert)
	}
}

func TestSubtensorSignalDetectsGatewayAndIdentityProblems(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Nodes[0].GatewayHTTP = 0
	observation.Nodes[0].Gateway.Errors = map[string]string{"healthz": "connection refused"}
	observation.Nodes[1].Direct.Chain = "Wrong chain"

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "subtensor-gateway")
	requireAlertClass(t, alerts, "subtensor-identity")
}

func TestSubtensorSignalTurnsMalformedObservationIntoVisibilityAlert(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, subtensorMarker) {
			return "", errors.New("unexpected command")
		}
		return "not-json", nil
	}}
	settings := subtensorSyntheticSettings(source)
	alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestSubtensorSignalRequiresExplicitHostConfiguration(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "snow", Roles: []string{"subtensor"}})
	alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func runSyntheticSubtensor(t *testing.T, observation subtensorObservation) (Alerts, error) {
	t.Helper()
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "snow" || host.Subtensor == nil {
			return "", fmt.Errorf("unexpected host settings: %+v", host)
		}
		if !strings.Contains(command, subtensorMarker) {
			return "", errors.New("unexpected command")
		}
		return string(encoded), nil
	}}
	return NewSubtensorSignal().Run(context.Background(), subtensorSyntheticSettings(source))
}

func subtensorSyntheticSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "snow", OverlayAddress: "172.28.208.185", Roles: []string{"subtensor"},
		Subtensor: &SubtensorHostSettings{
			PublicRPCURL:               "https://reference.example",
			ExpectedChain:              "Bittensor",
			ExpectedGenesisHash:        syntheticSubtensorGenesis,
			ExpectedSpecName:           "node-subtensor",
			ExpectedSpecVersion:        452,
			ExpectedTransactionVersion: 1,
			ExpectedEVMChainID:         "0x3b1",
			WarpMaxLag:                 4096,
			Nodes: []SubtensorNodeSettings{
				{Name: "archive", SyncMode: "full", RPCPort: 9945, GatewayPort: 9944},
				{Name: "lightnode", SyncMode: "warp", RPCPort: 9947, GatewayPort: 9946},
			},
		},
	})
	return settings
}

func healthySubtensorObservation() subtensorObservation {
	public := healthySubtensorRPC(7_910_000)
	archive := healthySubtensorNode("archive", "full", 9945, 9944, 7_909_990, 7_909_992)
	lightnode := healthySubtensorNode("lightnode", "warp", 9947, 9946, 7_909_994, 7_909_996)
	return subtensorObservation{
		Units: map[string]string{
			"subtensor": "active", "nginx": "active", "openvpn@by-pre": "active",
		},
		OverlayPresent: true,
		Public:         public,
		Nodes:          []subtensorNodeObservation{archive, lightnode},
	}
}

func healthySubtensorNode(name, syncMode string, rpcPort, gatewayPort int, first, second int64) subtensorNodeObservation {
	direct := healthySubtensorRPC(first)
	direct.Health = subtensorHealth{Peers: 8, IsSyncing: false}
	direct.Sync = subtensorSyncState{CurrentBlock: second, HighestBlock: second}
	gateway := healthySubtensorRPC(second)
	return subtensorNodeObservation{
		Name: name, SyncMode: syncMode, RPCPort: rpcPort, GatewayPort: gatewayPort,
		Direct: direct, Gateway: gateway, FirstHead: blockHex(first), SecondHead: blockHex(second), GatewayHTTP: 200,
	}
}

func healthySubtensorRPC(head int64) subtensorRPCObservation {
	return subtensorRPCObservation{
		Chain: "Bittensor", Genesis: syntheticSubtensorGenesis, Head: blockHex(head),
		Runtime:    subtensorRuntimeVersion{SpecName: "node-subtensor", SpecVersion: 452, TransactionVersion: 1},
		EVMChainID: "0x3b1", EthGetLogs: true,
		Errors: map[string]string{},
	}
}

func blockHex(block int64) string { return fmt.Sprintf("0x%x", block) }
