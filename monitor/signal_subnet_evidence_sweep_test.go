package monitor

import (
	"crypto/sha256"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"
)

// The §22.8 history over the healthy settlement chain with a settlement v2
// pair pinned. Operator 1's vault2 pool starts at epoch 3, and its committed
// epoch 3 root is the single sweep leaf, whose artifact hash names a sweep
// record rather than a payout artifact.
type subnetEvidenceSweepFixture struct {
	chain      *subnetSettlementTestChain
	journal    *subnetEvidenceTestJournal
	settings   SignalSettings
	record     []byte
	recordHash [32]byte
	failPools  bool
}

func newSubnetEvidenceSweepFixture(t *testing.T) *subnetEvidenceSweepFixture {
	t.Helper()
	chain, journal, node, settings, _ := newSubnetEvidenceFixture(t)
	record := []byte(`{"schema":"synthetic-v1-sweep-record"}`)
	f := &subnetEvidenceSweepFixture{chain: chain, journal: journal, settings: settings, record: record, recordHash: sha256.Sum256(record)}
	key := subnetSettlementKey{epoch: 3, noId: 1}
	committed := chain.roots[key]
	chain.roots[key] = &stabi.RootCommitmentsOutput{
		PayoutRoot:   [32]byte(merkle.PayoutLeaf(subnetSettlementCutoverTestColdkey, big.NewInt(subnetSettlementBps))),
		ArtifactHash: f.recordHash, Committer: committed.Committer, CommitBlock: committed.CommitBlock,
	}
	f.settings.SubnetDeployment = NewSubnetReplayPin(subnetSettlementCutoverTestPin())
	settlement, err := stabi.STSettlement2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	vault, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	node.contracts[subnetSettlementTestSettlementV2] = &subnetReplayTestContract{contract: settlement, handle: func(method string, _ []any) ([]any, string) {
		switch method {
		case "v1Coordinator":
			return []any{subnetReplayTestCoordinator}, ""
		case "vault":
			return []any{subnetSettlementTestVaultV2}, ""
		}
		t.Fatalf("unexpected v2 settlement view %s", method)
		return nil, ""
	}}
	node.contracts[subnetSettlementTestVaultV2] = &subnetReplayTestContract{contract: vault, handle: func(method string, args []any) ([]any, string) {
		switch method {
		case "coordinator":
			return []any{subnetSettlementTestSettlementV2}, ""
		case "netuid":
			return []any{uint16(25)}, ""
		case "selfColdkey":
			return []any{subnetSettlementCutoverTestColdkey}, ""
		case "pools":
			if f.failPools {
				return nil, "InvalidConfiguration"
			}
			if args[0].(*big.Int).Uint64() == 1 {
				return []any{stabi.ISTSettlementVault2Pool{Hotkey: [32]byte{0x52, 1}, FirstEpoch: 3, Registered: true}}, ""
			}
			return []any{stabi.ISTSettlementVault2Pool{}}, ""
		}
		t.Fatalf("unexpected vault2 view %s", method)
		return nil, ""
	}}
	return f
}

func (self *subnetEvidenceSweepFixture) run(t *testing.T) Alerts {
	t.Helper()
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), self.settings)
	requireSubnetReplayRedacted(t, alerts, subnetSettlementTestSettlementV2.Hex(), subnetSettlementTestVaultV2.Hex(), common.Bytes2Hex(self.recordHash[:]))
	return alerts
}

// The sanctioned sweep's record is not a payout artifact, so the history check
// skips it: a record absent from both origins is not a lost artifact, and a
// served record is not tampered bytes.
func TestSubnetEvidenceSkipsSanctionedSweepRecord(t *testing.T) {
	for _, served := range []bool{false, true} {
		name := "record-absent"
		if served {
			name = "record-served"
		}
		t.Run(name, func(t *testing.T) {
			f := newSubnetEvidenceSweepFixture(t)
			if served {
				f.chain.artifacts[f.recordHash] = f.record
			}
			requireSubnetEvidenceOnlySemanticGap(t, f.run(t))
		})
	}
}

// Ordinary committed artifacts are still judged beside the sweep: one lost at
// every origin still pages.
func TestSubnetEvidenceJudgesOrdinaryArtifactsBesideTheSweep(t *testing.T) {
	f := newSubnetEvidenceSweepFixture(t)
	lost := f.chain.roots[subnetSettlementKey{epoch: 1, noId: 1}].ArtifactHash
	for origin := 0; origin < 2; origin++ {
		f.journal.missingArtifacts[origin][lost] = true
	}
	alerts := f.run(t)
	page := requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/no-1", "epoch-1/artifact-lost")
	if page.Severity != SeverityPage {
		t.Fatalf("a lost finalized artifact did not page: %+v", page)
	}
	for _, alert := range alerts {
		if strings.HasPrefix(alert.Frame, "epoch-3/") {
			t.Fatalf("the sweep record was judged as a payout artifact: %+v", alert)
		}
	}
}

// With the cutover unknown any committed root could be the sweep, so every
// committed artifact stays undecided under an explicit gap: no page, and not
// healthy.
func TestSubnetEvidenceUnknownCutoverWithholdsCommittedArtifacts(t *testing.T) {
	f := newSubnetEvidenceSweepFixture(t)
	f.failPools = true
	lost := f.chain.roots[subnetSettlementKey{epoch: 1, noId: 1}].ArtifactHash
	for origin := 0; origin < 2; origin++ {
		f.journal.missingArtifacts[origin][lost] = true
	}
	alerts := f.run(t)
	gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-evidence/coverage", "settlement-v2-cutover")
	if !gap.unobservable || !strings.Contains(gap.Observed, "state=unknown reason=view-failed undecided_classes=subnet-artifact-integrity,subnet-history-availability") ||
		!strings.Contains(gap.Context, "completeness=partial:settlement-v2-cutover-unknown") {
		t.Fatalf("an unknown cutover was not an explicit gap: %+v", gap)
	}
	for _, alert := range alerts {
		if alert.Class == "subnet-history-availability" || alert.Class == "subnet-artifact-integrity" {
			t.Fatalf("an unknown cutover decided a committed artifact: %+v", alert)
		}
	}
}
