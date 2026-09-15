package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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

func TestSubtensorSignalDistinguishesStaleConvergenceFromActivelySyncingLag(t *testing.T) {
	for _, syncing := range []bool{false, true} {
		observation := healthySubtensorObservation()
		archive := &observation.Nodes[0]
		archive.FirstHead = blockHex(7_909_798)
		archive.SecondHead = blockHex(7_909_800)
		archive.Direct.Head = archive.FirstHead
		archive.Gateway.Head = archive.SecondHead
		archive.Direct.Sync.CurrentBlock = 7_909_800
		archive.Direct.Sync.HighestBlock = 7_909_800
		archive.Direct.Health.IsSyncing = syncing
		wantClass, wrongClass := "subtensor-stale-convergence", "subtensor-sync-lag"
		if syncing {
			wantClass, wrongClass = wrongClass, wantClass
		}
		alerts, err := runSyntheticSubtensor(t, observation)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, wantClass)
		if alert.Frame != "archive" || alert.Severity != SeverityWarn || alert.Sustain != 1 || !strings.Contains(alert.Observed, "lag=200") || !strings.Contains(alert.Markdown(), "SIGNALS.md §17.2") {
			t.Fatalf("sync state did not select its exact bounded lag identity: syncing=%t", syncing)
		}
		for _, other := range alerts {
			if other.Class == wrongClass {
				t.Errorf("syncing=%t emitted the opposite lag discriminator", syncing)
			}
		}
	}
}

func TestSubtensorSignalNearHeadCompletedSyncIsHealthy(t *testing.T) {
	observation := healthySubtensorObservation()
	archive := &observation.Nodes[0]
	archive.FirstHead = blockHex(7_909_872)
	archive.SecondHead = blockHex(7_909_874)
	archive.Direct.Head = archive.FirstHead
	archive.Gateway.Head = archive.SecondHead
	archive.Direct.Sync.CurrentBlock = 7_909_874
	archive.Direct.Sync.HighestBlock = 7_910_000
	archive.Direct.Health.IsSyncing = false
	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("completed sync in the near-head band produced %d alerts", len(alerts))
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
	if !strings.Contains(lightnode.Action, "canonical xops/main/ansible/run-subtensor.sh") ||
		!strings.Contains(lightnode.Action, "remove only that failed container") ||
		!strings.Contains(lightnode.Action, "retain the archive container identity") {
		t.Fatalf("lightnode action is not root-cause specific: %s", lightnode.Action)
	}
	if strings.Contains(lightnode.Action, "run-subtensor-lightnode.sh") {
		t.Fatalf("lightnode action retained removed runner: %s", lightnode.Action)
	}
	if !strings.Contains(lightnode.Verify, "unchanged archive container ID/start time") ||
		!strings.Contains(lightnode.Verify, "live /data mount") {
		t.Fatalf("lightnode verification does not preserve the archive boundary: %s", lightnode.Verify)
	}
}

func TestSubtensorSignalDoesNotInferCurrentRuntimeWhenPublicHeadIsUnavailable(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Head = ""
	observation.Public.Errors["head"] = "synthetic reference unavailable"
	observation.Public.Runtime.SpecVersion = 455

	archive := &observation.Nodes[0]
	archive.Direct.Runtime.SpecVersion = 443
	archive.Direct.Health.IsSyncing = true
	archive.Direct.Sync.HighestBlock = 8_120_000

	lightnode := &observation.Nodes[1]
	lightnode.Direct.Runtime.SpecVersion = 443
	lightnode.Direct.Health = subtensorHealth{Peers: 0, IsSyncing: false}
	lightnode.Direct.Sync.HighestBlock = lightnode.Direct.Sync.CurrentBlock

	alerts, err := runSyntheticSubtensorAtRuntime(t, observation, 455)
	if err != nil {
		t.Fatal(err)
	}
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if !strings.Contains(visibility.Target, "subtensor-public-reference") || visibility.Sustain != 2 {
		t.Fatalf("public-head visibility finding = %+v", visibility)
	}
	archiveLag := requireAlertClass(t, alerts, "subtensor-sync-lag")
	if archiveLag.Frame != "archive" || !strings.Contains(archiveLag.Observed, "target_head=8120000") {
		t.Fatalf("archive target control = %+v", archiveLag)
	}
	peers := requireAlertClass(t, alerts, "subtensor-peers")
	if peers.Frame != "lightnode" || !strings.Contains(peers.Observed, "is_syncing=false") {
		t.Fatalf("peer-loss control = %+v", peers)
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-identity" && alert.Frame == "lightnode-current-runtime" {
			t.Fatalf("unobservable convergence applied the current-runtime pin: %+v", alert)
		}
	}
}

func TestSubtensorSignalDoesNotMixHistoricalRuntimeWithSecondHeadConvergence(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Head = blockHex(8_120_000)
	observation.Public.Runtime.SpecVersion = 455
	for i := range observation.Nodes {
		observation.Nodes[i].Direct.Runtime.SpecVersion = 455
	}

	lightnode := &observation.Nodes[1]
	lightnode.FirstHead = blockHex(7_910_000)
	lightnode.SecondHead = blockHex(8_119_999)
	lightnode.Direct.Head = lightnode.FirstHead
	lightnode.Direct.Runtime.SpecVersion = 443
	lightnode.Direct.Sync = subtensorSyncState{
		CurrentBlock: 7_910_000,
		HighestBlock: 8_120_000,
	}
	lightnode.Gateway.Head = lightnode.SecondHead

	alerts, err := runSyntheticSubtensorAtRuntime(t, observation, 455)
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-identity" && alert.Frame == "lightnode-current-runtime" {
			t.Fatalf("first-sample historical runtime was applied to the converged second head: %+v", alert)
		}
	}
}

func TestSubtensorSignalChecksCurrentRuntimeAfterStableConvergence(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Runtime.SpecVersion = 455
	for i := range observation.Nodes {
		observation.Nodes[i].Direct.Runtime.SpecVersion = 455
	}
	lightnode := &observation.Nodes[1]
	lightnode.Direct.Runtime.SpecVersion = 443
	lightnode.Direct.EVMChainID = "0xsynthetic-mismatch"

	alerts, err := runSyntheticSubtensorAtRuntime(t, observation, 455)
	if err != nil {
		t.Fatal(err)
	}
	identity := requireAlertClass(t, alerts, "subtensor-identity")
	if identity.Frame != "lightnode-current-runtime" {
		t.Fatalf("stable current-runtime identity frame = %+v", identity)
	}
	for _, want := range []string{"specVersion=443 expected=455", `evm_chain_id="0xsynthetic-mismatch" expected="0x3b1"`} {
		if !strings.Contains(identity.Observed, want) {
			t.Fatalf("stable current-runtime identity missing %q: %+v", want, identity)
		}
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
		"canonical xops/main/ansible/run-subtensor.sh",
		"record and preserve the failed lightnode identity",
		"same live /data generation",
	} {
		if !strings.Contains(resume.Markdown(), want) {
			t.Fatalf("warp resume alert missing %q:\n%s", want, resume.Markdown())
		}
	}
	if strings.Contains(resume.Action, "run-subtensor-lightnode.sh") {
		t.Fatalf("warp resume action retained removed runner: %s", resume.Action)
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
		"canonical xops/main/ansible/run-subtensor.sh",
	} {
		if !strings.Contains(checkpoint.Markdown(), want) {
			t.Fatalf("checkpoint alert missing %q:\n%s", want, checkpoint.Markdown())
		}
	}
	if strings.Contains(checkpoint.Action, "run-subtensor-lightnode.sh") {
		t.Fatalf("checkpoint action retained removed runner: %s", checkpoint.Action)
	}
	drift := requireAlertClass(t, alerts, "subtensor-deployment-drift")
	if !strings.Contains(drift.Observed, "subtensor-lightnode-warp-v2") ||
		!strings.Contains(drift.Observed, "subtensor-lightnode-warp-v3") {
		t.Fatalf("deployment drift lost generation identity: %+v", drift)
	}
	if !strings.Contains(drift.Action, "canonical xops/main/ansible/run-subtensor.sh") ||
		strings.Contains(drift.Action, "run-subtensor-lightnode.sh") {
		t.Fatalf("deployment drift action does not use the sole canonical runner: %s", drift.Action)
	}
}

func TestSubtensorSignalCollectsContainerStartupDiscriminators(t *testing.T) {
	for _, want := range []string{
		`["sudo", "-n", "/usr/local/sbin/subtensor-monitor", name]`,
		`SUBTENSOR_HELPER_TIMEOUT_SECONDS = 30`,
		`timeout=SUBTENSOR_HELPER_TIMEOUT_SECONDS`,
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
	if alert := requireAlertClass(t, alerts, "subtensor-peers"); alert.Sustain != 3 || alert.PageSustain != 5 {
		t.Fatalf("peer escalation = %d/%d", alert.Sustain, alert.PageSustain)
	}
	if alert := requireAlertClass(t, alerts, "subtensor-progress"); alert.Frame != "archive" || alert.Sustain != 3 || alert.PageSustain != 5 {
		t.Fatalf("progress alert = %+v", alert)
	}
}

func TestSubtensorSignalKeepsOlderHelperAsCannotObserve(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Nodes[1].PeerDiagnostics = nil

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Target != "chain.example.test/lightnode/peer-diagnostics" ||
		!strings.Contains(visibility.Observed, "error_class="+observationErrorClassContractMismatch) {
		t.Fatalf("older helper did not retain an exact visibility boundary: %+v", visibility)
	}
	requireAlertOmits(t, visibility, "installed helper predates")
}

func TestSubtensorSignalLegacyHelperDoesNotHideCoreNodeHealth(t *testing.T) {
	encoded, err := json.Marshal(healthySubtensorObservation())
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	nodes, ok := document["nodes"].([]any)
	if !ok || len(nodes) != 2 {
		t.Fatalf("synthetic nodes=%T/%d", document["nodes"], len(nodes))
	}
	legacyDiagnostics := map[string]any{
		"version": 1,
		"log": map[string]any{
			"outcomes": map[string]any{
				"database_or_import_rejection":     2,
				"chain_or_fork_rejection":          0,
				"notification_negotiation_failure": 0,
				"reconnect_or_dial_failure":        1,
			},
		},
	}
	for _, rawNode := range nodes {
		node, ok := rawNode.(map[string]any)
		if !ok {
			t.Fatalf("synthetic node type=%T", rawNode)
		}
		node["peer_diagnostics"] = legacyDiagnostics
	}
	legacyEncoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := parseSubtensorObservation(string(legacyEncoded))
	if err != nil {
		t.Fatalf("legacy helper hid the complete host observation: %v", err)
	}
	for _, node := range parsed.Nodes {
		if node.PeerDiagnostics == nil || node.PeerDiagnostics.Version != 1 {
			t.Fatalf("legacy helper version was not retained for %s: %+v", node.Name, node.PeerDiagnostics)
		}
		if len(node.PeerDiagnostics.Log.Outcomes) != 0 {
			t.Fatalf("legacy helper body was interpreted for %s: %+v", node.Name, node.PeerDiagnostics)
		}
	}

	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "chain.example.test" || !strings.Contains(command, subtensorMarker) {
			return "", fmt.Errorf("unexpected synthetic Subtensor observation target")
		}
		return string(legacyEncoded), nil
	}}
	settings := subtensorSyntheticSettings(source)
	alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("legacy helper alerts=%d, want two narrow visibility findings: %+v", len(alerts), alerts)
	}
	wantTargets := map[string]bool{
		"chain.example.test/archive/peer-diagnostics":   true,
		"chain.example.test/lightnode/peer-diagnostics": true,
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !wantTargets[alert.Target] ||
			!strings.Contains(alert.Observed, "error_class="+observationErrorClassUnclassified) {
			t.Fatalf("legacy helper widened or obscured its boundary: %+v", alert)
		}
		delete(wantTargets, alert.Target)
		requireAlertOmits(t, alert, "database_or_import_rejection")
	}
	if len(wantTargets) != 0 {
		t.Fatalf("legacy helper visibility targets missing: %+v", wantTargets)
	}

	// The compatibility boundary is version-gated. A malformed current helper
	// must still fail closed instead of discarding evidence under a v2 label.
	legacyDiagnostics["version"] = subtensorPeerDiagnosticsVersion
	malformedCurrent, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSubtensorObservation(string(malformedCurrent)); err == nil {
		t.Fatal("malformed current helper was accepted as an unsupported generation")
	}
}

func TestSubtensorSignalLocalizesLitep2pNotificationFailureAgainstArchive(t *testing.T) {
	observation := healthySubtensorObservation()
	archive := &observation.Nodes[0]
	archive.PeerDiagnostics.Metrics.BlockAnnounceOpenedTotal = 2_136
	archive.PeerDiagnostics.Metrics.BlockAnnounceClosedTotal = 2_125
	archive.PeerDiagnostics.Metrics.SyncRequestClosedTotal = 5

	lightnode := &observation.Nodes[1]
	lightnode.Direct.Health.Peers = 0
	lightnode.Direct.Health.IsSyncing = false
	lightnode.FirstHead = lightnode.SecondHead
	lightnode.Direct.Head = lightnode.SecondHead
	lightnode.Gateway.Head = lightnode.SecondHead
	lightnode.PeerDiagnostics.Metrics.BlockAnnounceOpenedTotal = 13_897
	lightnode.PeerDiagnostics.Metrics.BlockAnnounceClosedTotal = 13_897
	lightnode.PeerDiagnostics.Metrics.RawDistinctOpenedTotal = 647_163
	lightnode.PeerDiagnostics.Metrics.RawDistinctClosedTotal = 647_099
	lightnode.PeerDiagnostics.Metrics.SyncRequestSuccessTotal = 39_930
	lightnode.PeerDiagnostics.Metrics.SyncRequestClosedTotal = 9_477
	lightnode.PeerDiagnostics.Metrics.PendingHandshakeFailureTotal = 7
	lightnode.PeerDiagnostics.Metrics.PendingTransportFailureTotal = 13
	// Timestamped tail alternatives must not outrank affirmative current-state
	// metrics until their interval and causal ordering match the peer loss.
	lightnode.PeerDiagnostics.Log.Outcomes[subtensorPeerLogDatabaseOrImport] = syntheticSubtensorPeerLogOutcome(2)
	lightnode.PeerDiagnostics.Log.Outcomes[subtensorPeerLogChainOrFork] = syntheticSubtensorPeerLogOutcome(3)

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	peer := requireAlertClass(t, alerts, "subtensor-peers")
	if peer.Frame != "lightnode" ||
		!strings.Contains(peer.Mechanism, "litep2p notification negotiation or peerset reconnect") ||
		!strings.Contains(peer.Mechanism, "advancing, peer-connected archive") {
		t.Fatalf("peer failure was not localized against archive: %+v", peer)
	}
	for want := range map[string]bool{
		"block_announce_opened=13897": true,
		"block_announce_closed=13897": true,
		"raw_distinct_live=64":        true,
		"sync_closed=9477":            true,
		"archive_control=healthy":     true,
		"block_announce_live=11":      true,
	} {
		if !strings.Contains(peer.Evidence, want) {
			t.Fatalf("peer evidence missing %q: %s", want, peer.Evidence)
		}
	}
	if !strings.Contains(peer.Action, "do not restart, reset") {
		t.Fatalf("peer action lost preservation boundary: %s", peer.Action)
	}
	if !strings.Contains(peer.Mechanism, "bounded first/last UTC timestamps") ||
		!strings.Contains(peer.Action, "timestamped SyncingEngine") {
		t.Fatalf("peer alert lost its bounded timestamp qualification: %+v", peer)
	}
	progress := requireAlertClass(t, alerts, "subtensor-progress")
	if !strings.Contains(progress.Mechanism, "litep2p notification negotiation or peerset reconnect") ||
		progress.Evidence != peer.Evidence {
		t.Fatalf("static-head alert did not retain the peer discriminator: %+v", progress)
	}
}

func TestSubtensorSignalSeparatesPeerRejectionAndContainerPathOutcomes(t *testing.T) {
	tests := []struct {
		name              string
		alter             func(*subtensorPeerDiagnostics)
		wantMechanism     string
		dontWantMechanism string
	}{
		{
			name: "database import rejection",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.Log.Outcomes[subtensorPeerLogDatabaseOrImport] = syntheticSubtensorPeerLogOutcome(2)
			},
			wantMechanism: "database/import rejection class",
		},
		{
			name: "chain fork rejection",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.Log.Outcomes[subtensorPeerLogChainOrFork] = syntheticSubtensorPeerLogOutcome(3)
			},
			wantMechanism: "chain/fork or block-announcement rejection class",
		},
		{
			name: "container dns",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.ContainerDNSStatus = "timeout"
			},
			wantMechanism: "failing before a bootnode transport",
		},
		{
			name: "bootnode tcp",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.BootnodeTCPStatus = "failed"
			},
			wantMechanism: "below notification negotiation and above name resolution",
		},
		{
			name: "bootnode unconfigured",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.ContainerDNSStatus = "unconfigured"
				diagnostics.BootnodeTCPStatus = "unconfigured"
			},
			wantMechanism:     "configuration/observation boundary",
			dontWantMechanism: "cannot resolve",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation := healthySubtensorObservation()
			node := &observation.Nodes[1]
			node.Direct.Health.Peers = 0
			test.alter(node.PeerDiagnostics)
			alerts, err := runSyntheticSubtensor(t, observation)
			if err != nil {
				t.Fatal(err)
			}
			peer := requireAlertClass(t, alerts, "subtensor-peers")
			if !strings.Contains(peer.Mechanism, test.wantMechanism) {
				t.Fatalf("mechanism=%q, want %q", peer.Mechanism, test.wantMechanism)
			}
			if test.dontWantMechanism != "" && strings.Contains(peer.Mechanism, test.dontWantMechanism) {
				t.Fatalf("mechanism=%q unexpectedly contains %q", peer.Mechanism, test.dontWantMechanism)
			}
			if strings.Contains(test.name, "rejection") &&
				(!strings.Contains(peer.Mechanism, "first/last times") ||
					!strings.Contains(peer.Action, "bounded rejection interval")) {
				t.Fatalf("log-only outcome was not conservative: %+v", peer)
			}
		})
	}
}

func TestSubtensorSignalKeepsTimestampedProtocolExitCausallyBounded(t *testing.T) {
	observation := healthySubtensorObservation()
	node := &observation.Nodes[1]
	node.Direct.Health.Peers = 0
	node.PeerDiagnostics.Metrics.BlockAnnounceOpenedTotal = 10
	node.PeerDiagnostics.Metrics.BlockAnnounceClosedTotal = 10
	node.PeerDiagnostics.Metrics.RawDistinctOpenedTotal = 0
	node.PeerDiagnostics.Metrics.RawDistinctClosedTotal = 0
	node.PeerDiagnostics.Log.Outcomes[subtensorPeerLogSyncEngineTermination] = syntheticSubtensorPeerLogOutcome(1)
	node.PeerDiagnostics.Log.Outcomes[subtensorPeerLogBlockAnnounceProtocolExit] = syntheticSubtensorPeerLogOutcome(1)

	alerts, err := runSyntheticSubtensor(t, observation)
	if err != nil {
		t.Fatal(err)
	}
	peer := requireAlertClass(t, alerts, "subtensor-peers")
	for _, want := range []string{
		"explicit SyncingEngine/NotificationService termination",
		"proves the current cause only when",
		"do not assume that protocol unregistration alone restarts",
		"sync_engine_termination=1",
		"block_announce_protocol_exit=1",
		"2099-04-05T06:07:09.000000Z",
	} {
		if !strings.Contains(peer.Markdown(), want) {
			t.Fatalf("timestamped protocol exit alert missing %q: %s", want, peer.Markdown())
		}
	}
}

func TestSubtensorSignalRejectsIncompleteTimestampedPeerDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		alter  func(*subtensorPeerDiagnostics)
		needle string
	}{
		{
			name: "old helper",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.Version = 1
			},
			needle: "unsupported peer diagnostics version=1",
		},
		{
			name: "missing class",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				delete(diagnostics.Log.Outcomes, subtensorPeerLogSyncEngineTermination)
			},
			needle: "classes are incomplete",
		},
		{
			name: "count without timestamps",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.Log.Outcomes[subtensorPeerLogDatabaseOrImport] = subtensorPeerLogOutcome{Count: 1}
			},
			needle: "not an explicit UTC timestamp",
		},
		{
			name: "reversed outcome",
			alter: func(diagnostics *subtensorPeerDiagnostics) {
				diagnostics.Log.Outcomes[subtensorPeerLogDatabaseOrImport] = subtensorPeerLogOutcome{
					Count: 1, FirstUTC: "2099-04-05T06:07:11Z", LastUTC: "2099-04-05T06:07:10Z",
				}
			},
			needle: "outside its window",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation := healthySubtensorObservation()
			test.alter(observation.Nodes[1].PeerDiagnostics)
			problem := subtensorPeerDiagnosticsProblem(observation.Nodes[1].PeerDiagnostics)
			if problem == nil || !strings.Contains(problem.Error(), test.needle) {
				t.Fatalf("diagnostics problem=%v, want %q", problem, test.needle)
			}
			alerts, err := runSyntheticSubtensor(t, observation)
			if err != nil {
				t.Fatal(err)
			}
			visibility := requireAlertClass(t, alerts, "cannot-observe")
			if visibility.Target != "chain.example.test/lightnode/peer-diagnostics" || !strings.Contains(visibility.Observed, "error_class="+observationErrorClassUnclassified) {
				t.Fatalf("invalid diagnostics did not fail closed: %+v", visibility)
			}
			requireAlertOmits(t, visibility, test.needle)
		})
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
	if alert.Severity != SeverityPage || alert.Target != "chain.example.test" || alert.Frame != "public-reference" || alert.Sustain != 1 {
		t.Fatalf("runtime-ahead alert framing = %+v", alert)
	}
	for _, want := range []string{
		"public_head=7910000",
		"specVersion=453",
		"expected_specVersion=452",
		"exact configured chain, genesis, runtime name, and EVM chain identity",
		"ruleset-locked live-network mirror commit and its runtime source",
		"exact on-chain transition and code hash",
		"each stale owning configuration while preserving owners that already match",
		"every configuration owner agrees on it",
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

// Updating only the verified runtime expectation clears that configuration
// fault, not the independently observed historical-node lag or its identity.
func TestSubtensorSignalRuntimePinUpdateKeepsHistoricalNodeLag(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Head = blockHex(7_938_093)
	observation.Public.Runtime.SpecVersion = 454
	for i := range observation.Nodes {
		node := &observation.Nodes[i]
		first := int64(6_650_000 + i*25_000)
		second := first + 12
		node.FirstHead = blockHex(first)
		node.SecondHead = blockHex(second)
		node.Direct.Head = node.FirstHead
		node.Gateway.Head = node.SecondHead
		node.Direct.Health.IsSyncing = true
		node.Direct.Runtime.SpecVersion = 449
		node.Gateway.Runtime = node.Direct.Runtime
		node.Direct.Sync = subtensorSyncState{
			StartingBlock: 6_400_000, CurrentBlock: second, HighestBlock: 7_938_093,
		}
	}
	priorLagObservations := map[string]string{}
	for _, expectedSpecVersion := range []int64{453, 454} {
		alerts, err := runSyntheticSubtensorAtRuntime(t, observation, expectedSpecVersion)
		if err != nil {
			t.Fatal(err)
		}
		if expectedSpecVersion == 453 {
			requireAlertClass(t, alerts, "subtensor-runtime-ahead")
		}
		for _, class := range []string{"subtensor-sync-lag", "subtensor-warp-resume"} {
			alert := requireAlertClass(t, alerts, class)
			if expectedSpecVersion == 453 {
				priorLagObservations[class] = alert.Observed
			} else if alert.Observed != priorLagObservations[class] {
				t.Errorf("runtime-only pin update changed the %s observation", class)
			}
		}
		for _, alert := range alerts {
			switch alert.Class {
			case "subtensor-sync-lag", "subtensor-warp-resume":
			case "subtensor-runtime-ahead":
				if expectedSpecVersion == 454 {
					t.Error("matching runtime pin retained its stale-configuration page")
				}
			default:
				t.Errorf("runtime-only update misclassified historical nodes: %s/%s", alert.Class, alert.Frame)
			}
		}
	}
}

// Advancing the expectation must not convert an older public reference into
// a healthy one. Historical local runtimes and the current reference differ.
func TestSubtensorSignalCurrentPinRejectsOlderPublicRuntime(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Public.Runtime.SpecVersion = 453
	alerts, err := runSyntheticSubtensorAtRuntime(t, observation, 454)
	if err != nil {
		t.Fatal(err)
	}
	identity := requireAlertClass(t, alerts, "subtensor-identity")
	if identity.Frame != "public-reference" {
		t.Fatalf("older reference did not retain its identity failure: %s", identity.Frame)
	}
	for _, alert := range alerts {
		if alert.Class == "subtensor-runtime-ahead" {
			t.Error("older public runtime was classified as a forward upgrade")
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
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "chain.example.test", Roles: []string{"subtensor"}})
	alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func runSyntheticSubtensor(t *testing.T, observation subtensorObservation) (Alerts, error) {
	t.Helper()
	return runSyntheticSubtensorAtRuntime(t, observation, 452)
}

// The expected runtime is configuration, independent of the observed local
// historical runtime. Existing fixtures keep their original 452 expectation.
func runSyntheticSubtensorAtRuntime(t *testing.T, observation subtensorObservation, expectedSpecVersion int64) (Alerts, error) {
	t.Helper()
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "chain.example.test" || host.Subtensor == nil {
			return "", fmt.Errorf("unexpected host settings: %+v", host)
		}
		if !strings.Contains(command, subtensorMarker) {
			return "", errors.New("unexpected command")
		}
		return string(encoded), nil
	}}
	settings := subtensorSyntheticSettings(source)
	for i := range settings.Hosts {
		if settings.Hosts[i].Subtensor != nil {
			settings.Hosts[i].Subtensor.ExpectedSpecVersion = expectedSpecVersion
		}
	}
	return NewSubtensorSignal().Run(context.Background(), settings)
}

func subtensorSyntheticSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "chain.example.test", OverlayAddress: "192.0.2.88", Roles: []string{"subtensor"},
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
		PeerDiagnostics: healthySubtensorPeerDiagnostics(),
	}
}

func healthySubtensorPeerDiagnostics() *subtensorPeerDiagnostics {
	return &subtensorPeerDiagnostics{
		Version: subtensorPeerDiagnosticsVersion, ContainerDNSStatus: "ok", BootnodeTCPStatus: "ok", MetricsStatus: "ok",
		Log: subtensorPeerLogDiagnostics{
			Scope: subtensorPeerLogScope, EventTimeCorrelated: true,
			WindowStartUTC: "2099-04-05T06:07:08Z", WindowEndUTC: "2099-04-05T06:08:08Z",
			TailLimit: 5000, LinesScanned: 120,
			Outcomes: syntheticSubtensorPeerLogOutcomes(),
		},
		Metrics: subtensorPeerMetrics{
			BlockAnnounceOpenedTotal: 20, BlockAnnounceClosedTotal: 12,
			RawDistinctOpenedTotal: 30, RawDistinctClosedTotal: 20,
			SyncRequestSuccessTotal: 100,
		},
	}
}

func syntheticSubtensorPeerLogOutcomes() map[string]subtensorPeerLogOutcome {
	outcomes := map[string]subtensorPeerLogOutcome{}
	for _, name := range subtensorPeerLogOutcomeNames {
		outcomes[name] = subtensorPeerLogOutcome{}
	}
	return outcomes
}

func syntheticSubtensorPeerLogOutcome(count int64) subtensorPeerLogOutcome {
	if count == 0 {
		return subtensorPeerLogOutcome{}
	}
	return subtensorPeerLogOutcome{
		Count: count, FirstUTC: "2099-04-05T06:07:09.000000Z", LastUTC: "2099-04-05T06:07:10.000000Z",
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

// Complete synthetic observations exercise the existing Signal and cadence
// gate without executing the native RPC/helper script or adding product state.
func subtensorProgressPauseTestAlerts(t *testing.T, first, second int64, peers int64) Alerts {
	t.Helper()
	now := time.Date(2099, 4, 5, 6, 7, 8, 0, time.UTC)
	settings := subtensorSyntheticSettings(nil)
	settings.Now = func() time.Time { return now }
	observation := healthySubtensorObservation()
	configuredHosts := []HostSettings{}
	for index := range settings.Hosts {
		if configured := settings.Hosts[index].Subtensor; configured != nil {
			configured.PublicRPCURL = "https://reference.example.test"
			configured.ExpectedChain = "synthetic-chain"
			configured.ExpectedSpecName = "synthetic-subtensor"
			configured.ExpectedSpecVersion = 77
			configured.ExpectedTransactionVersion = 8
			configured.ExpectedEVMChainID = "0x7b"
			configuredHosts = append(configuredHosts, settings.Hosts[index])
		}
	}
	settings.Hosts = configuredHosts
	qualifyRPC := func(rpc *subtensorRPCObservation, head int64) {
		rpc.Chain = "synthetic-chain"
		rpc.Runtime = subtensorRuntimeVersion{SpecName: "synthetic-subtensor", SpecVersion: 77, TransactionVersion: 8}
		rpc.EVMChainID = "0x7b"
		rpc.Head = blockHex(head)
	}
	qualifyRPC(&observation.Public, first+16)
	for index := range observation.Nodes {
		node := &observation.Nodes[index]
		nodeFirst, nodeSecond := first-3, first-2
		nodePeers := int64(3)
		if node.Name == "lightnode" {
			nodeFirst, nodeSecond, nodePeers = first, second, peers
		}
		node.FirstHead, node.SecondHead = blockHex(nodeFirst), blockHex(nodeSecond)
		node.ContainerStarted = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
		qualifyRPC(&node.Direct, nodeFirst)
		qualifyRPC(&node.Gateway, nodeSecond)
		node.Direct.Health = subtensorHealth{Peers: nodePeers}
		node.Direct.Sync = subtensorSyncState{StartingBlock: 100, CurrentBlock: nodeSecond, HighestBlock: first + 16}
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	settings.Source = &syntheticSource{hostFn: func(configured HostSettings, command string) (string, error) {
		if configured.Name != "chain.example.test" || !strings.Contains(command, subtensorMarker) {
			return "", errors.New("unsupported synthetic progress command")
		}
		return string(encoded), nil
	}}
	alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestSubtensorProgressRepeatedPauseEscalationNarration(t *testing.T) {
	signal := NewSubtensorSignal()
	gate := newCadenceAlertGate()
	var page Alert
	for cadence := 0; cadence < 5; cadence++ {
		head := int64(1000 + 11*cadence)
		progress := requireAlertClass(t, subtensorProgressPauseTestAlerts(t, head, head, 3), "subtensor-progress")
		ready := gate.filter(signal, Alerts{progress})
		if cadence == 4 {
			page = requireAlertClass(t, ready, "subtensor-progress")
		}
	}
	if page.Severity != SeverityPage || page.Sustain != 3 || page.PageSustain != 5 {
		t.Fatal("repeated bounded pauses changed the existing five-cadence escalation")
	}
	for _, want := range []string{"repeated bounded pauses", "not proof of continuous", "reference advancement"} {
		if !strings.Contains(page.Markdown(), want) {
			t.Errorf("bounded-pause narration lacks required qualification: %s", want)
		}
	}
	if strings.Contains(page.Baseline, "while the public chain advances") {
		t.Error("a single public reference read was narrated as proved advancement during the sample")
	}
}

func TestSubtensorProgressTrueFlatAndAdvancingResetControls(t *testing.T) {
	signal := NewSubtensorSignal()
	gate := newCadenceAlertGate()
	for cadence := 0; cadence < 5; cadence++ {
		progress := requireAlertClass(t, subtensorProgressPauseTestAlerts(t, 1200, 1200, 3), "subtensor-progress")
		ready := gate.filter(signal, Alerts{progress})
		if cadence == 4 && requireAlertClass(t, ready, "subtensor-progress").Severity != SeverityPage {
			t.Fatal("true-flat control lost its existing page threshold")
		}
	}
	advancing := subtensorProgressPauseTestAlerts(t, 1200, 1201, 3)
	for _, alert := range advancing {
		if alert.Class == "subtensor-progress" {
			t.Fatal("one advancing within-run sample retained progress failure")
		}
	}
	gate.filter(signal, advancing)
	for cadence := 0; cadence < 3; cadence++ {
		progress := requireAlertClass(t, subtensorProgressPauseTestAlerts(t, 1201, 1201, 3), "subtensor-progress")
		ready := gate.filter(signal, Alerts{progress})
		if cadence < 2 && len(ready) != 0 {
			t.Fatal("advancing/reset control retained the prior progress streak")
		}
		if cadence == 2 && requireAlertClass(t, ready, "subtensor-progress").Severity != SeverityWarn {
			t.Fatal("post-reset warning did not restart at the existing three-cadence threshold")
		}
	}
}

func TestSubtensorProgressPeerLossContextUnchanged(t *testing.T) {
	alerts := subtensorProgressPauseTestAlerts(t, 1400, 1400, 0)
	progress := requireAlertClass(t, alerts, "subtensor-progress")
	peer := requireAlertClass(t, alerts, "subtensor-peers")
	if !strings.Contains(progress.Mechanism, "co-resident with complete peer loss") || progress.Sustain != 3 || progress.PageSustain != 5 || peer.Sustain != 3 || peer.PageSustain != 5 {
		t.Fatal("bounded narration correction changed independently observed peer-loss context or escalation")
	}
}
