package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	syntheticSubtensorGenesis = "0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105"
	syntheticSubtensorImage   = "ghcr.io/raofoundation/subtensor@sha256:a1ac7792b5279cdad701eec15742296f91d4be83e256a29fe57cffd500fa8f13"
	syntheticSubtensorArchive = "/data/subtensor"
	syntheticSubtensorData    = "/data/subtensor-lightnode-warp-v3"
)

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
	observation.Nodes[1].WarpFallback = true
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
	if lightnode.Sustain != 1 {
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

func TestSubtensorSignalDetectsRevokedRuntimeDataPermission(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[1]
	node.DataPathUID = 0
	node.DataPathGID = 0
	node.DataPathMode = 0o750
	node.DataRuntimeWritable = false
	node.DataPermissionError = true

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subtensor-data-permission")
	if alert.Frame != "lightnode" || alert.Sustain != 1 {
		t.Fatalf("data permission alert = %+v", alert)
	}
	for _, want := range []string{
		"root:root 0750",
		"runtime_uid=10001",
		"data_uid=0",
		"data_mode=0750",
		"runtime_writable=false",
		"current_generation_database_permission_error=true",
		"head_advanced=true",
		"run-subtensor.sh",
		"preserve both container identities",
		"two further bounded samples",
		"one node at a time",
		"SIGNALS.md §17.4",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("data permission alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestSubtensorSignalRetainsPermissionRootCauseAfterOwnershipRepair(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[1]
	node.DataPermissionError = true
	node.SecondHead = node.FirstHead
	node.Direct.Sync.CurrentBlock, _ = subtensorHex(node.FirstHead)
	node.Direct.Sync.HighestBlock = node.Direct.Sync.CurrentBlock
	node.Direct.Head = node.FirstHead
	node.Gateway.Head = node.FirstHead

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subtensor-data-permission")
	for _, want := range []string{
		"runtime_writable=true",
		"current_generation_database_permission_error=true",
		"head_advanced=false",
		"started_at=2026-09-01T20:00:00Z",
		"Ownership is already repaired",
		"do not rerun run-subtensor.sh",
		"service-scoped same-generation restart",
		"Recover the archive first",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("latched permission alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Action, "Apply the committed Xops runtime-ownership repair") {
		t.Fatalf("repaired ownership retained the pre-repair action: %s", alert.Action)
	}
}

func TestSubtensorSignalClearsRetainedPermissionRootCauseOnExactProcessProgress(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Nodes[1].DataPermissionError = true

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-data-permission" {
			t.Fatalf("advancing exact process retained stale permission alert: %+v", alert)
		}
	}
}

func TestSubtensorSignalDistinguishesProgressedWarpResume(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[1]
	node.Direct.Sync = subtensorSyncState{
		StartingBlock: 6_413_262,
		CurrentBlock:  6_433_399,
		HighestBlock:  7_913_976,
	}
	node.Direct.Health = subtensorHealth{Peers: 8, IsSyncing: true}
	node.WarpFallback = true
	node.FirstHead = blockHex(6_433_390)
	node.SecondHead = blockHex(6_433_399)
	node.Direct.Head = node.FirstHead
	node.Gateway.Head = node.SecondHead

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	resume := requireAlertClass(t, alerts, "subtensor-warp-resume")
	if resume.Sustain != 1 {
		t.Fatalf("warp resume sustain = %d, want immediate", resume.Sustain)
	}
	for _, want := range []string{
		"already-progressed database",
		"starting_block=6413262",
		"Do not reset this progressing generation",
		"full-host lightnode-preservation guard",
		"same live /data generation",
	} {
		if !strings.Contains(resume.Markdown(), want) {
			t.Fatalf("warp resume alert missing %q:\n%s", want, resume.Markdown())
		}
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-warp-fallback" {
			t.Fatalf("progressed resume was misclassified as cold fallback: %+v", alert)
		}
	}
}

func TestSubtensorSignalDistinguishesProgressedWarpResumeWithoutRetainedFallbackLine(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[1]
	node.Direct.Sync = subtensorSyncState{
		StartingBlock: 6_447_926,
		CurrentBlock:  6_518_461,
		HighestBlock:  7_922_041,
	}
	node.Direct.Health = subtensorHealth{Peers: 17, IsSyncing: true}
	node.WarpFallback = false
	node.FirstHead = blockHex(6_518_450)
	node.SecondHead = blockHex(6_518_461)
	node.Direct.Head = node.FirstHead
	node.Gateway.Head = node.SecondHead

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	resume := requireAlertClass(t, alerts, "subtensor-warp-resume")
	if resume.Sustain != 1 {
		t.Fatalf("warp resume without retained fallback sustain = %d, want immediate", resume.Sustain)
	}
	for _, want := range []string{
		"same-generation resume rather than a cold warp bootstrap",
		"nonzero process-start block is authoritative",
		"startup_fallback=false",
		"starting_block=6447926",
		"Do not reset this progressing generation",
	} {
		if !strings.Contains(resume.Markdown(), want) {
			t.Fatalf("warp resume without retained fallback missing %q:\n%s", want, resume.Markdown())
		}
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-warp-bootstrap" || alert.Class == "subtensor-warp-fallback" {
			t.Fatalf("progressed resume without retained fallback was misclassified: %+v", alert)
		}
	}
}

func TestSubtensorSignalDetectsHistoricalWarpCheckpointFailure(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[1]
	node.Direct.Sync = subtensorSyncState{CurrentBlock: 0, HighestBlock: 7_910_000}
	node.Direct.Health = subtensorHealth{Peers: 3, IsSyncing: true}
	node.Direct.Runtime.SpecVersion = 365
	node.FirstHead = blockHex(0)
	node.SecondHead = blockHex(0)
	node.Direct.Head = blockHex(0)
	node.Gateway.Head = blockHex(0)
	node.WarpFallback = false
	node.WarpProofStarted = true
	node.ContainerImage = "ghcr.io/raofoundation/subtensor@sha256:3e37b8d9a4f3c60ba66652cae79fe54d81d868558fb0159842ff952eee5115de"
	node.DataPath = "/data/subtensor-lightnode-warp-v2"

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := requireAlertClass(t, alerts, "subtensor-warp-checkpoint")
	for _, want := range []string{
		"remained at genesis",
		"v447",
		"v448",
		"add2b31a19ccf650ad50d79e8ba2668e6494f56f",
		"0876234316a3b9107ce1eb0781b04ae55f5df89e",
		"run-subtensor-lightnode.sh",
	} {
		if !strings.Contains(checkpoint.Markdown(), want) {
			t.Fatalf("checkpoint alert missing %q:\n%s", want, checkpoint.Markdown())
		}
	}
	drift := requireAlertClass(t, alerts, "subtensor-deployment-drift")
	if !strings.Contains(drift.Observed, "subtensor-lightnode-warp-v2") ||
		!strings.Contains(drift.Observed, "subtensor-lightnode-warp-v3") {
		t.Fatalf("deployment drift lost generation identity: %+v", drift)
	}
}

func TestSubtensorSignalCollectsContainerStartupDiscriminators(t *testing.T) {
	for _, want := range []string{
		`["sudo", "-n", "/usr/local/sbin/subtensor-monitor", name]`,
		`result = json.loads(output)`,
		`"container_error"`,
	} {
		if !strings.Contains(subtensorScript, want) {
			t.Fatalf("Subtensor collector missing %q", want)
		}
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

// A same-chain public runtime advance must identify the stale configuration
// boundary without accusing the public RPC or progressing local databases.
func TestSubtensorSignalClassifiesPublicRuntimeAhead(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Runtime.SpecVersion = 453

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subtensor-runtime-ahead")
	if alert.Severity != SeverityPage || alert.Target != "snow" || alert.Frame != "public-reference" || alert.Sustain != 1 {
		t.Fatalf("runtime-ahead alert framing = %+v", alert)
	}
	for _, want := range []string{
		"public_head=7910000",
		"specVersion=453",
		"expected_specVersion=452",
		"exact configured chain, genesis, runtime name, and EVM chain identity",
		"official upstream release artifact and exact on-chain transition",
		"monitor inventory and Subtensor Xops host variables",
		"do not restart either node solely for this pin update",
		"progressing historical nodes retain their ordinary lag classifications",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("runtime-ahead alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "subtensor-identity" && candidate.Frame == "public-reference" {
			t.Fatalf("same-chain runtime advance retained generic identity diagnosis: %+v", candidate)
		}
	}
}

// A higher version does not override a mismatched stable chain identity; that
// remains a potentially wrong RPC surface rather than a routine pin update.
func TestSubtensorSignalRejectsRuntimeAheadClassificationOnWrongGenesis(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Runtime.SpecVersion = 453
	observation.Public.Genesis = "0xwrong"

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	identity := requireAlertClass(t, alerts, "subtensor-identity")
	if identity.Frame != "public-reference" || !strings.Contains(identity.Observed, "genesis=") {
		t.Fatalf("wrong-genesis identity alert = %+v", identity)
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-runtime-ahead" {
			t.Fatalf("wrong genesis was misclassified as a routine runtime advance: %+v", alert)
		}
	}
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
				{
					Name: "archive", SyncMode: "full", RPCPort: 9945, GatewayPort: 9944,
					ContainerName: "subtensor", ExpectedImage: syntheticSubtensorImage,
					ExpectedDataPath: syntheticSubtensorArchive,
				},
				{
					Name: "lightnode", SyncMode: "warp", RPCPort: 9947, GatewayPort: 9946,
					ContainerName: "subtensor-lightnode", ExpectedImage: syntheticSubtensorImage,
					ExpectedDataPath: syntheticSubtensorData,
				},
			},
		},
	})
	return settings
}

func healthySubtensorObservation() subtensorObservation {
	public := healthySubtensorRPC(7_910_000)
	archive := healthySubtensorNode("archive", "full", 9945, 9944, 7_909_990, 7_909_992)
	lightnode := healthySubtensorNode("lightnode", "warp", 9947, 9946, 7_909_994, 7_909_996)
	archive.ContainerImage = syntheticSubtensorImage
	archive.ContainerStarted = "2026-09-01T20:00:00Z"
	archive.DataPath = syntheticSubtensorArchive
	archive.RuntimeUID = 10001
	archive.RuntimeGID = 10001
	archive.DataPathUID = 10001
	archive.DataPathGID = 10001
	archive.DataPathMode = 0o750
	archive.DataPermissionObserved = true
	archive.DataRuntimeWritable = true
	lightnode.ContainerImage = syntheticSubtensorImage
	lightnode.ContainerStarted = "2026-09-01T20:00:00Z"
	lightnode.DataPath = syntheticSubtensorData
	lightnode.RuntimeUID = 10001
	lightnode.RuntimeGID = 10001
	lightnode.DataPathUID = 10001
	lightnode.DataPathGID = 10001
	lightnode.DataPathMode = 0o750
	lightnode.DataPermissionObserved = true
	lightnode.DataRuntimeWritable = true
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
