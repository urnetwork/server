// Controls for the §22.7 settlement v2 dispute reader (incident 2026-11-05,
// plan R4b). vault2 events are abi-encoded with the pinned binding and
// relayed through the synthetic archive node of the cutover fixture.
package monitor

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/stabi"
)

// Synthetic dispute parties; they must never be rendered.
var subnetDisputeTestDemander = common.HexToAddress("0x00000000000000000000000000000000000d3a4d")
var subnetDisputeTestChallenger = common.HexToAddress("0x00000000000000000000000000000000000c4a1e")

func subnetDisputeTestVaultAbi(t *testing.T) *abi.ABI {
	t.Helper()
	vault, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	return vault
}

// Appends one vault2 event to the fixture's finalized logs.
func (self *subnetSettlementCutoverFixture) emitV2(t *testing.T, name string, block uint64, values ...any) {
	t.Helper()
	index := uint(len(self.vaultV2Logs) + 1)
	self.vaultV2Logs = append(self.vaultV2Logs, subnetReplayTestLog(t, subnetDisputeTestVaultAbi(t), subnetRootDeadlineTestVaultV2, name, block, index, subnetReplayTestTx(uint64(9000+index)), values...))
}

// The ordinary v2 life of operator 1's pool: a capture, a first commit, a
// finalization and claims. None of it is a dispute.
func (self *subnetSettlementCutoverFixture) routineV2(t *testing.T, end uint64) {
	t.Helper()
	noId := big.NewInt(1)
	self.emitV2(t, "EmissionCaptured", end, big.NewInt(2), noId, big.NewInt(500), big.NewInt(500), big.NewInt(500))
	self.emitV2(t, "DistributionCommitted", end+5, noId, uint64(1), uint64(2), [32]byte{0x0a}, big.NewInt(500), uint64(3), [32]byte{0x0b}, uint64(0))
	self.emitV2(t, "DistributionFinalized", end+90, noId, uint64(1), big.NewInt(0), uint64(2), [32]byte{0x0a}, big.NewInt(500), uint64(3), [32]byte{0x0b})
	self.emitV2(t, "Claimed", end+95, noId, big.NewInt(0), big.NewInt(1), [32]byte{0xa1}, big.NewInt(200), big.NewInt(200), subnetDisputeTestDemander)
}

// Routine commits, finalizations and claims are silent: the only alert is
// the standing gap for the v2 settlement this family does not replay.
func TestSubnetSettlementV2RoutineHistoryIsQuiet(t *testing.T) {
	f := newSubnetSettlementCutoverFixture(t)
	end := f.chain.end(subnetSettlementCutoverTestFirst)
	f.routineV2(t, end)
	requireSubnetSettlementOnlyV2ReaderGap(t, f.run(t, end+200))
	// The silence came from a reader that read this history: one demand in
	// it pages.
	f.emitV2(t, "PathDemanded", end+96, big.NewInt(1), uint64(1), big.NewInt(0), big.NewInt(1), subnetDisputeTestDemander)
	f.signal = nil
	requireSubnetReplayAlert(t, f.run(t, end+200), "subnet-payout-root", "subnet-settlement/no-1", "v2-seq-1/path-demanded")
}

// Each dispute kind pages on the operator's pending root sequence, once per
// sequence however many events it holds, with the dispute parties redacted.
func TestSubnetSettlementV2DisputesPage(t *testing.T) {
	for _, test := range []struct {
		name     string
		emit     func(f *subnetSettlementCutoverFixture, end uint64)
		frame    string
		observed []string
	}{
		{name: "path-demanded", frame: "v2-seq-2/path-demanded", emit: func(f *subnetSettlementCutoverFixture, end uint64) {
			f.emitV2(t, "DistributionCommitted", end+5, big.NewInt(1), uint64(2), uint64(2), [32]byte{0x0c}, big.NewInt(700), uint64(4), [32]byte{0x0d}, uint64(0))
			for i := int64(0); i < 3; i++ {
				f.emitV2(t, "PathDemanded", end+20+uint64(i), big.NewInt(1), uint64(2), big.NewInt(0), big.NewInt(i), subnetDisputeTestDemander)
			}
		}, observed: []string{"invariant=v2-pending-root-needs-no-demand", "observed=path-demanded events=3 demanded_root_index=0"}},
		{name: "distribution-deleted", frame: "v2-seq-2/distribution-deleted", emit: func(f *subnetSettlementCutoverFixture, end uint64) {
			f.emitV2(t, "DistributionDeleted", end+30, big.NewInt(1), uint64(2), big.NewInt(0), big.NewInt(7), uint8(2), big.NewInt(3), subnetDisputeTestChallenger)
		}, observed: []string{"invariant=v2-pending-root-has-no-fault", "fault=lower-value level=3 root_index=0 index=7"}},
		{name: "replacing-commit", frame: "v2-seq-3/replacing-commit", emit: func(f *subnetSettlementCutoverFixture, end uint64) {
			f.emitV2(t, "DistributionCommitted", end+40, big.NewInt(1), uint64(3), uint64(2), [32]byte{0x0e}, big.NewInt(900), uint64(5), [32]byte{0x0f}, uint64(2))
		}, observed: []string{"invariant=v2-pending-root-finalizes-before-replacement", "replaced_seq=2 epoch=2 leaf_count=5 total=900"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			end := f.chain.end(subnetSettlementCutoverTestFirst)
			f.routineV2(t, end)
			test.emit(f, end)
			alerts := f.run(t, end+200)
			alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", test.frame)
			if alert.Severity != SeverityPage || alert.Sustain != 1 || alert.unobservable {
				t.Fatalf("dispute did not page at once: %+v", alert)
			}
			for _, observed := range append(test.observed, "challenge_blocks=14400") {
				if !strings.Contains(alert.Observed, observed) {
					t.Fatalf("dispute observation lost %q: %s", observed, alert.Observed)
				}
			}
			if !strings.Contains(alert.Action, "REMEDIATION-PLAN.md") {
				t.Fatalf("dispute action does not name the runbook: %s", alert.Action)
			}
			for _, other := range alerts {
				if other.Frame != "settlement-v2" && other.Frame != test.frame {
					t.Fatalf("unexpected alert beside the dispute: %s|%s|%s", other.Class, other.Target, other.Frame)
				}
			}
			requireSubnetReplayRedacted(t, alerts, subnetDisputeTestDemander.Hex(), subnetDisputeTestChallenger.Hex())
		})
	}
}

// The first window is vault2's challenge window. A dispute older than it is
// history; a later run reads only new blocks, and a proved dispute stays
// retained after its window has passed.
func TestSubnetSettlementV2DisputeFrontier(t *testing.T) {
	f := newSubnetSettlementCutoverFixture(t)
	f.challengeBlocks = 500
	end := f.chain.end(subnetSettlementCutoverTestFirst)
	checkpoint := end + 200
	f.emitV2(t, "PathDemanded", checkpoint-500, big.NewInt(1), uint64(1), big.NewInt(0), big.NewInt(1), subnetDisputeTestDemander)
	f.emitV2(t, "PathDemanded", checkpoint-499, big.NewInt(1), uint64(2), big.NewInt(0), big.NewInt(1), subnetDisputeTestDemander)
	alerts := f.run(t, checkpoint)
	requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "v2-seq-2/path-demanded")
	for _, alert := range alerts {
		if alert.Frame == "v2-seq-1/path-demanded" {
			t.Fatal("a dispute before the first window was read")
		}
	}

	f.emitV2(t, "DistributionDeleted", checkpoint+10, big.NewInt(1), uint64(3), big.NewInt(0), big.NewInt(4), uint8(1), big.NewInt(0), subnetDisputeTestChallenger)
	alerts = f.run(t, checkpoint+700)
	deleted := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "v2-seq-3/distribution-deleted")
	if !strings.Contains(deleted.Observed, "fault=inconsistent-node") {
		t.Fatalf("deletion lost its fault: %s", deleted.Observed)
	}
	retained := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "v2-seq-2/path-demanded")
	if retained.Severity != SeverityPage || !strings.Contains(retained.Observed, "retained=true") || !strings.Contains(retained.Observed, "events=1") {
		t.Fatalf("the earlier dispute was not retained: %+v", retained)
	}
}

// A failed or malformed dispute read is unknown and names subnet-payout-root
// as undecided; it is never read as no dispute.
func TestSubnetSettlementV2DisputeReadGapIsUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
		mutate func(t *testing.T, f *subnetSettlementCutoverFixture, end uint64)
	}{
		{name: "challenge-window-unreadable", reason: "view-failed", mutate: func(t *testing.T, f *subnetSettlementCutoverFixture, end uint64) {
			ordinary := f.vaultV2
			f.vaultV2 = func(method string, args []any) ([]any, string) {
				if method == "CHALLENGE_BLOCKS" {
					return nil, "InvalidConfiguration"
				}
				return ordinary(method, args)
			}
		}},
		{name: "challenge-window-zero", reason: "challenge-window-invalid", mutate: func(t *testing.T, f *subnetSettlementCutoverFixture, end uint64) {
			f.challengeBlocks = 0
		}},
		{name: "malformed-demand", reason: "dispute-event-malformed", mutate: func(t *testing.T, f *subnetSettlementCutoverFixture, end uint64) {
			f.emitV2(t, "PathDemanded", end+20, big.NewInt(1), uint64(2), big.NewInt(0), big.NewInt(1), subnetDisputeTestDemander)
			f.vaultV2Logs[len(f.vaultV2Logs)-1].Data = f.vaultV2Logs[len(f.vaultV2Logs)-1].Data[:64]
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			end := f.chain.end(subnetSettlementCutoverTestFirst)
			test.mutate(t, f, end)
			alerts := f.run(t, end+200)
			gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "settlement-v2-disputes")
			if !gap.unobservable || gap.Severity != SeverityWarn || !strings.Contains(gap.Observed, fmt.Sprintf("reason=%s undecided_classes=subnet-payout-root", test.reason)) {
				t.Fatalf("dispute read gap = %+v", gap)
			}
			for _, alert := range alerts {
				if alert.Class == "subnet-payout-root" {
					t.Fatalf("an unknown dispute read decided a root: %+v", alert)
				}
			}
		})
	}
}

// Without a declared pair, or with its links unproven, vault2 is not read.
func TestSubnetSettlementV2DisputesNeedProvenLinks(t *testing.T) {
	f := newSubnetSettlementCutoverFixture(t)
	ordinary := f.vaultV2
	f.vaultV2 = func(method string, args []any) ([]any, string) {
		if method == "coordinator" {
			return []any{subnetReplayTestCoordinator}, ""
		}
		if method == "CHALLENGE_BLOCKS" {
			t.Fatal("vault2 disputes were read through an unproven link")
		}
		return ordinary(method, args)
	}
	end := f.chain.end(subnetSettlementCutoverTestFirst)
	f.emitV2(t, "PathDemanded", end+20, big.NewInt(1), uint64(2), big.NewInt(0), big.NewInt(1), subnetDisputeTestDemander)
	alerts := f.run(t, end+200)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "settlement-v2-cutover")
}
