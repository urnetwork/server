package monitor

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func runSubnetSettlement(t *testing.T, signal Signal, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("settlement run failed: %v", err)
	}
	return alerts
}

// The healthy control spans paid and deferred claims, a missed root whose
// carry the next root consumes, a dust epoch, an expiry into carry, and an
// epoch awaiting finalization inside its window: nothing alerts.
func TestSubnetSettlementHealthyHistoryHasNoAlerts(t *testing.T) {
	_, node, settings, source := newSubnetSettlementFixture(t)
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	if len(alerts) != 0 {
		t.Fatalf("healthy settlement history alerted: %+v", alerts)
	}
	if node.exchanges == 0 || settings.SubnetReplay.RpcMaximumExchanges < node.exchanges {
		t.Fatalf("relayed exchanges=%d outside the run budget", node.exchanges)
	}
	if source.apiCalls == 0 {
		t.Fatal("committed roots were not reproduced from their artifacts")
	}
}

// Live escrow below accounted obligations is a proved deficit: page once,
// with the exact integer delta and no rendered identity.
func TestSubnetSettlementEscrowDeficitPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(7))
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
	if alert.Severity != SeverityPage || alert.Sustain != 1 || !strings.Contains(alert.Observed, "delta=-7") || !strings.Contains(alert.Context, "checkpoint=finalized-evm-block-") {
		t.Fatalf("escrow deficit was not an exact page: %+v", alert)
	}
	requireSubnetReplayRedacted(t, alerts)
}

// A getter that contradicts its own counters is a governance-grade finding.
func TestSubnetSettlementConservationGetterMismatchPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	falseValue := false
	chain.holds = &falseValue
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", "getter-recomputation")
	if alert.Severity != SeverityPage {
		t.Fatalf("getter mismatch severity=%s", alert.Severity)
	}
}

// Counters that agree with each other but not with the complete census are
// caught only by the independent reconstruction.
func TestSubnetSettlementConsistentButFabricatedCountersPage(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	for _, counter := range []*big.Int{chain.captured, chain.accounted, chain.pending, chain.escrowStake} {
		counter.Add(counter, big.NewInt(1))
	}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	for _, frame := range []string{"captured-census", "pending-census", "journal-captured"} {
		requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", frame)
	}
	for _, frame := range []string{"captured-paid-accounted", "accounted-pending-outstanding", "escrow-backing"} {
		for _, alert := range alerts {
			if alert.Frame == frame {
				t.Fatalf("pairwise-consistent counters falsely tripped %s", frame)
			}
		}
	}
}

// Carry moved from one operator to another keeps the global sum but breaks
// both operators' own histories.
func TestSubnetSettlementCrossOperatorCarryPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	chain.carry[1].Add(chain.carry[1], big.NewInt(500))
	chain.carry[2].Sub(chain.carry[2], big.NewInt(500))
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	for _, target := range []string{"subnet-settlement/no-1", "subnet-settlement/no-2"} {
		census := requireSubnetReplayAlert(t, alerts, "subnet-carry-isolation", target, "carry-census")
		requireSubnetReplayAlert(t, alerts, "subnet-carry-isolation", target, "journal-carry")
		if census.Severity != SeverityPage {
			t.Fatalf("cross-operator carry severity=%s", census.Severity)
		}
	}
}

// Claims above a finalized total are excess credit.
func TestSubnetSettlementClaimAboveTotalPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	record := chain.entitlement(0, 1)
	record.Claimed = new(big.Int).Add(record.Total, big.NewInt(1))
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-claim-correctness", "subnet-settlement/no-1", "epoch-0/claimed-exceeds-total")
	requireSubnetReplayAlert(t, alerts, "subnet-claim-correctness", "subnet-settlement/no-1", "epoch-0/journal-claimed")
}

// Payment above every claim ever credited is a double payment even when the
// three global identities still balance.
func TestSubnetSettlementPaidAboveClaimedPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	chain.paid.Add(chain.paid, big.NewInt(1000))
	for _, counter := range []*big.Int{chain.accounted, chain.outstanding, chain.escrowStake} {
		counter.Sub(counter, big.NewInt(1000))
	}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-claim-credit-payment", "subnet-settlement/vault", "paid-exceeds-claimed")
	requireSubnetReplayAlert(t, alerts, "subnet-claim-credit-payment", "subnet-settlement/vault", "journal-paid")
	if len(subnetReplayAlerts(alerts, "subnet-vault-conservation")) != 0 {
		t.Fatal("balanced identities falsely tripped conservation")
	}
}

// A replayed claim for the same leaf is caught by the event journal.
func TestSubnetSettlementDuplicateClaimEventPages(t *testing.T) {
	chain, node, settings, _ := newSubnetSettlementFixture(t)
	duplicate := chain.logs
	var claimed types.Log
	for _, log := range duplicate {
		if log.Topics[0] == chain.contracts.vault.Events["Claimed"].ID {
			claimed = log
			break
		}
	}
	claimed.Index = 900
	claimed.BlockNumber = chain.end(0) + 62
	claimed.BlockHash = subnetReplayTestBlockHash(claimed.BlockNumber, 0)
	node.logs[subnetReplayTestVault] = append(append([]types.Log(nil), chain.logs...), claimed)
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-claim-correctness", "subnet-settlement/no-1", "epoch-0/duplicate-claim")
}

// A dust deferral above the immutable transfer floor fabricates a zero epoch.
func TestSubnetSettlementDustAboveFloorPages(t *testing.T) {
	chain, node, settings, _ := newSubnetSettlementFixture(t)
	logs := append([]types.Log(nil), chain.logs...)
	for i, log := range logs {
		if log.Topics[0] == chain.contracts.vault.Events["EmissionDustDeferred"].ID {
			logs[i] = subnetReplayTestLog(t, chain.contracts.vault, subnetReplayTestVault, "EmissionDustDeferred", log.BlockNumber, log.Index, log.TxHash,
				big.NewInt(2), big.NewInt(1), chain.versions[1].PoolHotkey, big.NewInt(5), big.NewInt(int64(chain.transferFlr)), chain.transferFlr)
		}
	}
	node.logs[subnetReplayTestVault] = logs
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-capture-delta", "subnet-settlement/no-1", "epoch-2/dust-above-floor")
}

// The same finalization identity warns when due and pages once overdue.
func TestSubnetSettlementFinalizationDeadlineBeforeAndAfter(t *testing.T) {
	for _, step := range []struct {
		block    uint64
		severity Severity
		alerted  bool
	}{
		{block: 1440, alerted: false},
		{block: 1452, severity: SeverityWarn, alerted: true},
		{block: 1460, severity: SeverityPage, alerted: true},
	} {
		chain := newHealthySubnetSettlementChain(t)
		node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, step.block)
		chain.install(node)
		settings, source := subnetReplayTestSettings(t, node)
		settings.SubnetReplay.ReplayFromBlock = subnetSettlementTestDeployBlock
		settings.SubnetReplay.FinalizeOverdueBlocks = 10
		source.api = chain.apiHandler(t)
		alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
		progress := []Alert{}
		for _, alert := range subnetReplayAlerts(alerts, "subnet-epoch-progress") {
			if alert.Frame == "epoch-3/finalization" {
				progress = append(progress, alert)
			}
		}
		if !step.alerted {
			if len(progress) != 0 {
				t.Fatalf("block %d: finalization inside its offset alerted", step.block)
			}
			continue
		}
		if len(progress) != 2 || progress[0].Severity != step.severity || !strings.Contains(progress[0].Observed, "deadline_block=1460") {
			t.Fatalf("block %d: finalization alerts=%+v", step.block, progress)
		}
	}
}

// A missing root warns while at risk and after its window, never pages.
func TestSubnetSettlementRootWindowAtRiskAndMissed(t *testing.T) {
	for _, block := range []uint64{1425, 1440} {
		chain := newHealthySubnetSettlementChain(t)
		delete(chain.roots, subnetSettlementKey{epoch: 3, noId: 2})
		node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, block)
		chain.install(node)
		settings, source := subnetReplayTestSettings(t, node)
		settings.SubnetReplay.ReplayFromBlock = subnetSettlementTestDeployBlock
		source.api = chain.apiHandler(t)
		alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
		alert := requireSubnetReplayAlert(t, alerts, "subnet-epoch-progress", "subnet-settlement/no-2", "epoch-3/root")
		if alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatalf("block %d: missing root severity=%s sustain=%d", block, alert.Severity, alert.Sustain)
		}
	}
}

// Finalized state before the finalize offset contradicts the policy clock.
func TestSubnetSettlementEarlyFinalizationPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	record := chain.entitlement(3, 1)
	root := chain.roots[subnetSettlementKey{epoch: 3, noId: 1}]
	record.Status = subnetSettlementFinalized
	record.Total = new(big.Int).Set(record.Funded)
	record.PayoutRoot, record.ArtifactHash = root.PayoutRoot, root.ArtifactHash
	record.ExpiryBlock = chain.start(3+4) - 1
	chain.pending.Sub(chain.pending, record.Funded)
	chain.outstanding.Add(chain.outstanding, record.Funded)
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-epoch-progress", "subnet-settlement/no-1", "epoch-3/finalization")
	if alert.Severity != SeverityPage {
		t.Fatalf("early finalization severity=%s", alert.Severity)
	}
}

// A finalized entitlement that holds another root than its commitment.
func TestSubnetSettlementFinalizedRootRewritePages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	chain.entitlement(1, 1).PayoutRoot = [32]byte{0x99}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "epoch-1/root-rewrite")
	if alert.Severity != SeverityPage {
		t.Fatalf("root rewrite severity=%s", alert.Severity)
	}
}

// A committed root that its own verified artifact does not reproduce.
func TestSubnetSettlementCommittedRootDiffersFromArtifactPages(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	chain.roots[subnetSettlementKey{epoch: 3, noId: 1}].PayoutRoot = [32]byte{0x77}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "epoch-3/artifact-reproduction")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "artifact-payout-root-differs") {
		t.Fatalf("artifact reproduction mismatch=%+v", alert)
	}
}

// Unavailable artifact bytes leave the root unknown, never a page.
func TestSubnetSettlementUnavailableArtifactIsUnknown(t *testing.T) {
	chain, _, settings, source := newSubnetSettlementFixture(t)
	served := chain.apiHandler(t)
	missing := chain.roots[subnetSettlementKey{epoch: 3, noId: 2}].ArtifactHash
	source.api = func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.RawQuery, common.Bytes2Hex(missing[:])) {
			return subnetReplayTestHttpResponse(request, http.StatusNotFound, []byte("missing"), ""), nil
		}
		return served(request)
	}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-2", "epoch-3/artifact-reproduction")
	if alert.Severity != SeverityWarn || !alert.unobservable || !strings.Contains(alert.Observed, "state=unknown reason=artifact-unavailable") {
		t.Fatalf("unavailable artifact was not unknown: %+v", alert)
	}
}

// Missing pins, endpoints and sources are unknown; only an explicit disable
// without a reviewed pin is an expected absence.
func TestSubnetSettlementUnknownSourcesNeverGreen(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*SignalSettings, *subnetReplayTestNode, *subnetSettlementTestChain)
		reason  string
	}{
		{name: "pin-missing", reason: "deployment-pin-missing", prepare: func(settings *SignalSettings, _ *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			settings.SubnetDeployment = nil
		}},
		{name: "pin-invalid", reason: "deployment-pin-invalid", prepare: func(settings *SignalSettings, _ *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			settings.SubnetDeployment = NewSubnetReplayPin([]byte("schema: other"))
		}},
		{name: "no-archive", reason: "archive-endpoint-unconfigured", prepare: func(settings *SignalSettings, _ *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			settings.Hosts = settings.Hosts[:2]
		}},
		{name: "chain-id", reason: "chain-identity-mismatch", prepare: func(_ *SignalSettings, node *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			node.chainId = 945
		}},
		{name: "link", reason: "deployment-link-mismatch", prepare: func(_ *SignalSettings, _ *subnetReplayTestNode, chain *subnetSettlementTestChain) {
			chain.coordinator = common.HexToAddress("0x00000000000000000000000000000000000c0009")
		}},
		{name: "rpc-error", reason: "view-failed", prepare: func(_ *SignalSettings, node *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			node.failMethods["eth_call"] = true
		}},
		{name: "oversized", reason: "rpc-response-oversized", prepare: func(settings *SignalSettings, _ *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			settings.SubnetReplay.RpcMaximumResponseBytes = 64
		}},
		{name: "budget", reason: "rpc-budget-exhausted", prepare: func(settings *SignalSettings, _ *subnetReplayTestNode, _ *subnetSettlementTestChain) {
			settings.SubnetReplay.RpcMaximumExchanges = 2
		}},
	}
	for _, c := range cases {
		chain, node, settings, _ := newSubnetSettlementFixture(t)
		c.prepare(&settings, node, chain)
		node.settings = settings.SubnetReplay
		alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
		found := false
		for _, alert := range alerts {
			if alert.Class == "subnet-monitor-coverage" && strings.Contains(alert.Observed, "reason="+c.reason) && alert.unobservable {
				found = true
			}
			if alert.Severity == SeverityPage {
				t.Fatalf("%s: an unknown source paged: %+v", c.name, alert)
			}
		}
		if !found {
			t.Fatalf("%s: no unknown coverage alert with reason %s in %+v", c.name, c.reason, alerts)
		}
		requireSubnetReplayRedacted(t, alerts)
	}
}

// An explicitly disabled subnet without a reviewed pin is not deployed.
func TestSubnetSettlementExplicitDisableWithoutPinIsNotDeployed(t *testing.T) {
	_, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetDeployment = nil
	settings.STConfigStatus = STConfigurationExplicitDisabled
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	if len(alerts) != 0 {
		t.Fatalf("explicitly disabled subnet alerted: %+v", alerts)
	}
}

// A reviewed pin is observed even when new subnet work is disabled: custody
// and claims outlive a disabled producer.
func TestSubnetSettlementReviewedPinObservedWhileDisabled(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	settings.STConfigStatus = STConfigurationExplicitDisabled
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(1))
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
}

// A checkpoint that stops being canonical before the closing recheck leaves
// every verdict unknown and does not advance the retained cursor.
func TestSubnetSettlementCheckpointReorgWithholdsVerdicts(t *testing.T) {
	_, counted, countedSettings, _ := newSubnetSettlementFixture(t)
	runSubnetSettlement(t, NewSubnetSettlementSignal(), countedSettings)
	recheck := counted.exchanges
	chain, node, settings, _ := newSubnetSettlementFixture(t)
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(1))
	signal := NewSubnetSettlementSignal()
	node.beforeExchange = func(exchange int) {
		if exchange != recheck {
			return
		}
		node.stateLock.Lock()
		defer node.stateLock.Unlock()
		node.blocks[node.finalized] = subnetReplayTestBlockHash(node.finalized, 1)
	}
	alerts := runSubnetSettlement(t, signal, settings)
	if len(subnetReplayAlerts(alerts, "subnet-vault-conservation")) != 0 {
		t.Fatalf("verdicts survived a non-canonical checkpoint: %+v", alerts)
	}
	alert := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "source")
	if !strings.Contains(alert.Observed, "reason=checkpoint-not-canonical") {
		t.Fatalf("reorged checkpoint reason=%q", alert.Observed)
	}
	probe := signal.(*signalAdapter).probe.(*subnetSettlementProbe)
	if next, _ := probe.journal.Snapshot(); next != subnetSettlementTestDeployBlock {
		t.Fatalf("journal advanced past an unproven checkpoint to %d", next)
	}
}

// A bounded frontier catches up over runs; until then the event classes are
// unknown while the state identities are still decided.
func TestSubnetSettlementJournalFrontierCatchesUp(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetReplay.LogRangeBlocks = 100
	settings.SubnetReplay.LogMaximumRangesPerRun = 2
	signal := NewSubnetSettlementSignal()
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(1))
	behind := 0
	for run := 0; run < 4; run++ {
		alerts := runSubnetSettlement(t, signal, settings)
		requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
		for _, alert := range alerts {
			if alert.Class == "subnet-monitor-coverage" && alert.Frame == "event-journal" && strings.Contains(alert.Observed, "frontier-behind") {
				behind = run + 1
			}
		}
	}
	if behind != 2 {
		t.Fatalf("frontier reported behind through run %d, want 2", behind)
	}
}

// A replay start after the vault's deployment can never be complete.
func TestSubnetSettlementJournalStartAfterDeploymentIsUnknown(t *testing.T) {
	_, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetReplay.ReplayFromBlock = subnetSettlementTestDeployBlock + 1
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "event-journal")
	if !strings.Contains(alert.Observed, "reason=journal-start-after-deployment") {
		t.Fatalf("late journal start=%q", alert.Observed)
	}
}

// A log from a non-canonical block is refused.
func TestSubnetSettlementNonCanonicalLogBlockIsUnknown(t *testing.T) {
	chain, node, settings, _ := newSubnetSettlementFixture(t)
	logs := append([]types.Log(nil), chain.logs...)
	logs[3].BlockHash = subnetReplayTestBlockHash(logs[3].BlockNumber, 2)
	node.logs[subnetReplayTestVault] = logs
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "event-journal")
	if !strings.Contains(alert.Observed, "reason=log-block-not-canonical") {
		t.Fatalf("non-canonical log block=%q", alert.Observed)
	}
}

// A bounded census is explicitly partial and cannot page whole-history sums.
func TestSubnetSettlementPartialCensusCannotPageSums(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetReplay.MaximumCensusEntries = 4
	settings.SubnetReplay.ReplayFromBlock = 0
	for _, counter := range []*big.Int{chain.captured, chain.accounted, chain.pending, chain.escrowStake} {
		counter.Add(counter, big.NewInt(1))
	}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "census")
	for _, alert := range alerts {
		if strings.HasSuffix(alert.Frame, "-census") {
			t.Fatalf("partial census paged a whole-history sum: %+v", alert)
		}
	}
}

// A proved counterexample stays open after a later healthy read.
func TestSubnetSettlementRetainsCounterexampleAfterHealthyRead(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	signal := NewSubnetSettlementSignal()
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(3))
	first := runSubnetSettlement(t, signal, settings)
	requireSubnetReplayAlert(t, first, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
	chain.escrowStake.Add(chain.escrowStake, big.NewInt(3))
	second := runSubnetSettlement(t, signal, settings)
	retained := requireSubnetReplayAlert(t, second, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
	if retained.Severity != SeverityPage || !strings.Contains(retained.Observed, "retained=true") || !strings.Contains(retained.Observed, "current_read=absent") {
		t.Fatalf("healthy read erased a proved counterexample: %+v", retained)
	}
}

// Parent cancellation withholds every finding and keeps the context cause.
func TestSubnetSettlementCanceledRunWithholdsFindings(t *testing.T) {
	chain, node, settings, _ := newSubnetSettlementFixture(t)
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(1))
	ctx, cancel := context.WithCancel(context.Background())
	node.beforeExchange = func(exchange int) {
		if exchange == 3 {
			cancel()
		}
	}
	alerts, err := NewSubnetSettlementSignal().Run(ctx, settings)
	if err == nil || len(alerts) != 0 || ctx.Err() == nil {
		t.Fatalf("canceled run returned alerts=%d err=%v", len(alerts), err)
	}
}

// A malformed enrollment keeps the state identities decided.
func TestSubnetSettlementMalformedEnrollmentStaysVisible(t *testing.T) {
	chain, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetReplay.ApiOrigins = []string{"https://only.replay.example.test"}
	chain.escrowStake.Sub(chain.escrowStake, big.NewInt(1))
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "enrollment")
	requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
}

// Registration metadata follows the family convention.
func TestSubnetSettlementRegistrationMetadata(t *testing.T) {
	signal := NewSubnetSettlementSignal()
	if signal.Number() != "22.7" || signal.Key() != "subnet-settlement" || signal.ID() != "subnet/settlement" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata: %s %s %s %s", signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}
}

// §22.10 acceptance fixtures for this family.
func init() {
	subnetFamilyAcceptanceCases["22.7"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			_, _, settings, _ := newSubnetSettlementFixture(t)
			return NewSubnetSettlementSignal(), settings
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			chain, _, settings, _ := newSubnetSettlementFixture(t)
			chain.escrowStake.Sub(chain.escrowStake, big.NewInt(7))
			return NewSubnetSettlementSignal(), settings, func() { chain.escrowStake.Add(chain.escrowStake, big.NewInt(7)) }
		},
		violatedClass: "subnet-vault-conservation",
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			_, node, settings, _ := newSubnetSettlementFixture(t)
			node.chainId = 945
			return NewSubnetSettlementSignal(), settings
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			chain, node, settings, _ := newSubnetSettlementFixture(t)
			chain.escrowStake.Sub(chain.escrowStake, big.NewInt(1))
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			node.beforeExchange = func(exchange int) {
				if exchange == 3 {
					cancel()
				}
			}
			return NewSubnetSettlementSignal(), settings, ctx
		},
	}
}
