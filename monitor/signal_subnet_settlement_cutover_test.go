package monitor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"
)

// Operator 1's first v2 epoch in the cutover fixture.
const subnetSettlementCutoverTestFirst uint64 = 2

// Synthetic vault2 coldkey and sweep record. The fixture builds the sweep
// root with the producer's merkle package, which is independent of the
// reader's own leaf computation.
var subnetSettlementCutoverTestColdkey = [32]byte{0x5e, 0xc0, 0x1d}
var subnetSettlementCutoverTestRecord = sha256.Sum256([]byte("synthetic v1 carry sweep record"))

// Synthetic settlement v2 pair addresses.
var subnetSettlementTestSettlementV2 = common.HexToAddress("0x00000000000000000000000000000000000c0006")
var subnetSettlementTestVaultV2 = common.HexToAddress("0x00000000000000000000000000000000000c0007")

// The reviewed synthetic pin with the settlement v2 pair declared.
func subnetSettlementCutoverTestPin() []byte {
	return append(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash),
		[]byte(fmt.Sprintf("  settlement_v2_address: %q\n  vault_v2_address: %q\n", subnetSettlementTestSettlementV2.Hex(), subnetSettlementTestVaultV2.Hex()))...)
}

func subnetSettlementCutoverTestLeaf() [32]byte {
	return [32]byte(merkle.PayoutLeaf(subnetSettlementCutoverTestColdkey, big.NewInt(subnetSettlementBps)))
}

// Operator 1's v1 history beside a synthetic settlement v2 pair whose vault2
// registers operator 1's pool from epoch 2. Epoch 0 is finalized and claimed
// in full; epoch 1, the last v1 epoch, missed its root into carry. Epochs are
// 1,000 blocks, so the default finalization overdue band fits inside one.
type subnetSettlementCutoverFixture struct {
	chain       *subnetSettlementTestChain
	pools       map[uint64]stabi.ISTSettlementVault2Pool
	selfColdkey [32]byte
	// The vault2 view handler; a test wraps it to fail or contradict a read.
	vaultV2        subnetReplayTestHandler
	vaultV2Missing bool
	// A release owner's enrollment parsed from monitor.yml; nil keeps the
	// synthetic default.
	enrollment *SubnetReplaySettings
}

func newSubnetSettlementCutoverFixture(t *testing.T) *subnetSettlementCutoverFixture {
	t.Helper()
	chain := newSubnetSettlementTestChain(t, 1)
	chain.policy.EpochBlocks = 1000
	chain.policy.CloseGraceBlocks = 20
	chain.policy.RootCommitWindowBlocks = 120
	chain.policy.FinalizeOffsetBlocks = 500
	alice := [32]byte{0xa1}
	chain.coordinatorFixed()
	chain.capture(0, 1, 10_000, chain.end(0))
	chain.commit(0, 1, chain.end(0)+5, alice)
	chain.finalize(0, 1, chain.end(0)+500)
	chain.claim(0, 1, alice, 10_000, chain.end(0)+510, true)
	chain.capture(1, 1, 4_000, chain.end(1))
	chain.finalize(1, 1, chain.end(1)+500)
	chain.currentEpoch = subnetSettlementCutoverTestFirst + 1
	f := &subnetSettlementCutoverFixture{
		chain: chain,
		pools: map[uint64]stabi.ISTSettlementVault2Pool{
			1: {Hotkey: [32]byte{0x52, 1}, FirstEpoch: subnetSettlementCutoverTestFirst, Registered: true},
		},
		selfColdkey: subnetSettlementCutoverTestColdkey,
	}
	f.vaultV2 = f.vaultV2Handler
	return f
}

func (self *subnetSettlementCutoverFixture) vaultV2Handler(method string, args []any) ([]any, string) {
	switch method {
	case "coordinator":
		return []any{subnetSettlementTestSettlementV2}, ""
	case "netuid":
		return []any{uint16(25)}, ""
	case "selfColdkey":
		return []any{self.selfColdkey}, ""
	case "pools":
		return []any{self.pools[args[0].(*big.Int).Uint64()]}, ""
	}
	self.chain.t.Fatalf("unexpected vault2 view %s", method)
	return nil, ""
}

// Closes epoch on v1 and commits the single sweep leaf as the root signer.
func (self *subnetSettlementCutoverFixture) commitSweep(epoch uint64, funded int64, commitBlock uint64) {
	self.chain.capture(epoch, 1, funded, self.chain.end(epoch))
	self.chain.roots[subnetSettlementKey{epoch: epoch, noId: 1}] = &stabi.RootCommitmentsOutput{
		PayoutRoot: subnetSettlementCutoverTestLeaf(), ArtifactHash: subnetSettlementCutoverTestRecord,
		Committer: self.chain.versions[1].RootSigner, CommitBlock: commitBlock,
	}
}

// A third party's permissionless deferral after close grace: funded at zero.
func (self *subnetSettlementCutoverFixture) deferMissed(epoch uint64, block uint64) {
	self.chain.nextTx()
	self.chain.entitlement(epoch, 1).Status = subnetSettlementFunded
	self.chain.emit("EmissionDeferred", block, new(big.Int).SetUint64(epoch), big.NewInt(1))
}

// Runs the settlement family at one finalized block with the v2 pair pinned.
func (self *subnetSettlementCutoverFixture) run(t *testing.T, block uint64) Alerts {
	t.Helper()
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, block)
	self.chain.install(node)
	settings, source := subnetReplayTestSettings(t, node)
	settings.SubnetReplay.ReplayFromBlock = subnetSettlementTestDeployBlock
	if self.enrollment != nil {
		settings.SubnetReplay = self.enrollment
		node.settings = self.enrollment
	}
	source.api = self.chain.apiHandler(t)
	settings.SubnetDeployment = NewSubnetReplayPin(subnetSettlementCutoverTestPin())
	if settings.SubnetDeployment.publicStatus() != subnetReplayPinReviewed || !settings.SubnetDeployment.settlementV2Declared {
		t.Fatal("v2 pin is invalid")
	}
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
	if !self.vaultV2Missing {
		node.contracts[subnetSettlementTestVaultV2] = &subnetReplayTestContract{contract: vault, handle: self.vaultV2}
	}
	alerts := runSubnetSettlement(t, NewSubnetSettlementSignal(), settings)
	if node.exchanges == 0 || settings.SubnetReplay.RpcMaximumExchanges < node.exchanges {
		t.Fatalf("relayed exchanges=%d outside the run budget", node.exchanges)
	}
	leaf := subnetSettlementCutoverTestLeaf()
	requireSubnetReplayRedacted(t, alerts, subnetSettlementTestSettlementV2.Hex(), subnetSettlementTestVaultV2.Hex(),
		common.Bytes2Hex(subnetSettlementCutoverTestColdkey[:]), common.Bytes2Hex(leaf[:]), common.Bytes2Hex(subnetSettlementCutoverTestRecord[:]))
	return alerts
}

// The v1 family has nothing to report: the only alert is the standing gap for
// the settlement v2 history this family does not replay.
func requireSubnetSettlementOnlyV2ReaderGap(t *testing.T, alerts Alerts) {
	t.Helper()
	gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "settlement-v2")
	if !strings.Contains(gap.Observed, "reason=settlement-v2-reader-unimplemented") {
		t.Fatalf("unexpected v2 reader gap: %+v", gap)
	}
	for _, alert := range alerts {
		if alert.Frame != "settlement-v2" {
			t.Fatalf("unexpected alert %s|%s|%s severity=%s observed=%s", alert.Class, alert.Target, alert.Frame, alert.Severity, alert.Observed)
		}
	}
}

// The reader's leaf is vault1's payoutLeaf. The producer's merkle leaf is an
// independent implementation that the sn sweep e2e claims against vault1.
func TestSubnetSettlementSweepLeafMatchesVaultPayoutLeaf(t *testing.T) {
	for _, coldkey := range [][32]byte{{}, {0x01}, subnetSettlementCutoverTestColdkey, {0xff, 0x00, 0xee, 0x11}} {
		want := [32]byte(merkle.PayoutLeaf(coldkey, big.NewInt(subnetSettlementBps)))
		if got := subnetSettlementPayoutLeaf(coldkey, subnetSettlementBps); got != want {
			t.Fatalf("sweep leaf differs from payoutLeaf for coldkey %x", coldkey[:4])
		}
	}
	if subnetSettlementPayoutLeaf(subnetSettlementCutoverTestColdkey, subnetSettlementBps-1) == subnetSettlementCutoverTestLeaf() {
		t.Fatal("a partial share produced the sweep leaf")
	}
}

// Regression for the settlement v2 cutover. From an operator's first v2 epoch
// its v1 rows stay unset forever: v2 settles them, and v1 finalization of an
// unset epoch reverts. Without the cutover the family warned about the close
// and root and then paged an overdue finalization that can never happen.
func TestSubnetSettlementPostCutoverUnsetEpochIsSilent(t *testing.T) {
	for _, step := range []struct {
		name   string
		offset uint64
	}{
		{name: "close-and-root-at-risk", offset: 10},
		{name: "close-and-root-missed", offset: 200},
		{name: "finalization-overdue", offset: 850},
	} {
		t.Run(step.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			alerts := f.run(t, f.chain.end(subnetSettlementCutoverTestFirst)+step.offset)
			requireSubnetSettlementOnlyV2ReaderGap(t, alerts)
		})
	}
}

// The owner-approved v1 carry sweep at the first v2 epoch is an ordinary,
// healthy v1 epoch at every stage: the close captures reveal-lag stake, the
// root signer commits the single leaf paying vault2's coldkey 10,000 bps in
// its window, finalization folds all v1 carry into it, and the leaf is
// claimed and paid. Every conservation, carry and claim identity holds, so
// the completed sweep is silent. Its sweep record is not a payout artifact.
func TestSubnetSettlementSanctionedSweepIsHealthy(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, stage := range []struct {
		name                    string
		offset                  uint64
		commit, finalize, claim bool
	}{
		{name: "closed-before-commit", offset: 15},
		{name: "committed", offset: 200, commit: true},
		{name: "finalized", offset: 520, commit: true, finalize: true},
		{name: "claimed-and-paid", offset: 850, commit: true, finalize: true, claim: true},
	} {
		t.Run(stage.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			end := f.chain.end(first)
			if stage.commit {
				f.commitSweep(first, 333, end+5)
			} else {
				f.chain.capture(first, 1, 333, end)
			}
			if stage.finalize {
				f.chain.finalize(first, 1, end+500)
				if total := f.chain.entitlement(first, 1).Total; total.Cmp(big.NewInt(333+4_000)) != 0 || f.chain.carry[1].Sign() != 0 {
					t.Fatalf("the synthetic sweep did not fold the v1 carry: total=%s", total)
				}
			}
			if stage.claim {
				f.chain.claim(first, 1, f.selfColdkey, subnetSettlementBps, end+510, true)
				if f.chain.outstanding.Sign() != 0 || f.chain.accounted.Sign() != 0 || f.chain.paid.Cmp(f.chain.captured) != 0 {
					t.Fatal("the synthetic sweep did not pay out every v1 obligation")
				}
			}
			requireSubnetSettlementOnlyV2ReaderGap(t, f.run(t, end+stage.offset))
		})
	}
}

// The sweep is sanctioned as a root, never exempt from the ordinary rules: a
// sweep root committed after its window or by another committer pages
// exactly as any v1 root does.
func TestSubnetSettlementSanctionedSweepKeepsOrdinaryRootRules(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, tc := range []struct {
		name   string
		frame  string
		mutate func(root *stabi.RootCommitmentsOutput, end uint64)
	}{
		{name: "outside-window", frame: "epoch-2/commit-window", mutate: func(root *stabi.RootCommitmentsOutput, end uint64) {
			root.CommitBlock = end + 121
		}},
		{name: "other-committer", frame: "epoch-2/committer", mutate: func(root *stabi.RootCommitmentsOutput, _ uint64) {
			root.Committer = common.BigToAddress(big.NewInt(0xbad))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			end := f.chain.end(first)
			f.commitSweep(first, 333, end+5)
			tc.mutate(f.chain.roots[subnetSettlementKey{epoch: first, noId: 1}], end)
			alerts := f.run(t, end+200)
			alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", tc.frame)
			if alert.Severity != SeverityPage || alert.Sustain != 1 {
				t.Fatalf("sweep root rule was not a page: %+v", alert)
			}
			for _, other := range alerts {
				if other.Frame == "epoch-2/post-cutover-root" {
					t.Fatalf("the only sweep-leaf root lost its sanction: %+v", other)
				}
			}
		})
	}
}

// Any other v1 root at an epoch v2 owns pages: v1 would finalize a payout
// list for an epoch that v2 also settles. Its own artifact still reproduces.
func TestSubnetSettlementPostCutoverDifferentRootPages(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	f := newSubnetSettlementCutoverFixture(t)
	end := f.chain.end(first)
	f.chain.capture(first, 1, 333, end)
	f.chain.commit(first, 1, end+5, [32]byte{0xc3})
	alerts := f.run(t, end+200)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "epoch-2/post-cutover-root")
	if alert.Severity != SeverityPage || alert.Sustain != 1 || alert.unobservable ||
		!strings.Contains(alert.Observed, "invariant=post-cutover-v1-root-is-the-sanctioned-sweep") ||
		!strings.Contains(alert.Observed, "observed=different-root post_cutover_roots=1 first_v2_epoch=2") ||
		!strings.Contains(strings.ToLower(alert.Action), "never backdate or invent a root") {
		t.Fatalf("different post-cutover root was not a page: %+v", alert)
	}
	for _, other := range alerts {
		if other.Frame != "settlement-v2" && other.Frame != "epoch-2/post-cutover-root" {
			t.Fatalf("unexpected alert beside the post-cutover root: %s|%s|%s", other.Class, other.Target, other.Frame)
		}
	}
}

// The sweep is one-time. Two sweep-leaf roots at two post-cutover epochs
// cannot both be sanctioned, and the chain cannot tell which was approved,
// so both page with the root count in the observation.
func TestSubnetSettlementSecondSweepRootPages(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	f := newSubnetSettlementCutoverFixture(t)
	f.chain.currentEpoch = first + 2
	end := f.chain.end(first)
	f.commitSweep(first, 333, end+5)
	f.chain.finalize(first, 1, end+500)
	f.chain.claim(first, 1, f.selfColdkey, subnetSettlementBps, end+510, true)
	f.commitSweep(first+1, 50, f.chain.end(first+1)+5)
	alerts := f.run(t, f.chain.end(first+1)+200)
	for _, epoch := range []uint64{first, first + 1} {
		alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", fmt.Sprintf("epoch-%d/post-cutover-root", epoch))
		if alert.Severity != SeverityPage || alert.Sustain != 1 || !strings.Contains(alert.Observed, "observed=sweep-leaf-root post_cutover_roots=2 first_v2_epoch=2") {
			t.Fatalf("epoch %d: second sweep root was not a page: %+v", epoch, alert)
		}
	}
	for _, alert := range alerts {
		if alert.Class == "subnet-vault-conservation" || alert.Class == "subnet-carry-isolation" || alert.Class == "subnet-claim-correctness" || alert.Class == "subnet-claim-credit-payment" {
			t.Fatalf("a consistent second sweep tripped an identity: %+v", alert)
		}
	}
}

// A funded post-cutover row without a root, from the sweep close before its
// commit or a third party's permissionless close or deferral, owes v1 no
// close or root, so neither warns. It must still be finalized: finalization
// due warns and overdue pages, and root-missed then carries its funding.
func TestSubnetSettlementPostCutoverFundedRowStillFinalizes(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, closing := range []string{"capture", "deferral"} {
		for _, step := range []struct {
			offset   uint64
			severity Severity
		}{
			{offset: 30}, {offset: 200}, {offset: 600, severity: SeverityWarn}, {offset: 850, severity: SeverityPage},
		} {
			t.Run(fmt.Sprintf("%s/offset=%d", closing, step.offset), func(t *testing.T) {
				f := newSubnetSettlementCutoverFixture(t)
				end := f.chain.end(first)
				if closing == "capture" {
					f.chain.capture(first, 1, 333, end)
				} else {
					f.deferMissed(first, end+25)
				}
				alerts := f.run(t, end+step.offset)
				finalization := []Alert{}
				for _, alert := range alerts {
					switch alert.Frame {
					case "settlement-v2":
					case "epoch-2/finalization":
						finalization = append(finalization, alert)
					default:
						t.Fatalf("a v2-owned funded row alerted: %s|%s|%s severity=%s", alert.Class, alert.Target, alert.Frame, alert.Severity)
					}
				}
				if step.severity == "" {
					if len(finalization) != 0 {
						t.Fatalf("finalization alerted inside its offset: %+v", finalization)
					}
					return
				}
				deadline := fmt.Sprintf("deadline_block=%d", end+500+300)
				if len(finalization) != 1 || finalization[0].Severity != step.severity || !strings.Contains(finalization[0].Observed, deadline) {
					t.Fatalf("funded row finalization alerts=%+v", finalization)
				}
			})
		}
	}
}

// A failed or inconsistent vault2 read leaves the cutover unknown. The run is
// explicitly partial and never healthy: a coverage gap names the progress and
// root classes as undecided, rows that depend on the cutover are withheld
// rather than paged or cleared, and the whole-history identities, which do
// not depend on it, are still decided.
func TestSubnetSettlementFailedV2ReadIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		mutate func(*subnetSettlementCutoverFixture)
	}{
		{name: "pool-read-fails", reason: "view-failed", mutate: func(f *subnetSettlementCutoverFixture) {
			ordinary := f.vaultV2
			f.vaultV2 = func(method string, args []any) ([]any, string) {
				if method == "pools" {
					return nil, "InvalidConfiguration"
				}
				return ordinary(method, args)
			}
		}},
		{name: "link-mismatch", reason: "deployment-link-mismatch", mutate: func(f *subnetSettlementCutoverFixture) {
			ordinary := f.vaultV2
			f.vaultV2 = func(method string, args []any) ([]any, string) {
				if method == "coordinator" {
					return []any{subnetReplayTestCoordinator}, ""
				}
				return ordinary(method, args)
			}
		}},
		{name: "pool-shape", reason: "pool-shape-invalid", mutate: func(f *subnetSettlementCutoverFixture) {
			f.pools[1] = stabi.ISTSettlementVault2Pool{Hotkey: [32]byte{0x52, 1}, Registered: true}
		}},
		{name: "zero-coldkey", reason: "self-coldkey-invalid", mutate: func(f *subnetSettlementCutoverFixture) {
			f.selfColdkey = [32]byte{}
		}},
		{name: "vault-absent", reason: "view-empty", mutate: func(f *subnetSettlementCutoverFixture) {
			f.vaultV2Missing = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			tc.mutate(f)
			f.chain.escrowStake.Sub(f.chain.escrowStake, big.NewInt(1))
			alerts := f.run(t, f.chain.end(subnetSettlementCutoverTestFirst)+850)
			gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "settlement-v2-cutover")
			if !gap.unobservable || gap.Severity != SeverityWarn ||
				!strings.Contains(gap.Observed, "state=unknown reason="+tc.reason+" undecided_classes=subnet-epoch-progress,subnet-payout-root") ||
				!strings.Contains(gap.Context, "completeness=partial:settlement-v2-cutover-unknown") {
				t.Fatalf("failed v2 read was not an unknown cutover: %+v", gap)
			}
			requireSubnetReplayAlert(t, alerts, "subnet-vault-conservation", "subnet-settlement/vault", "escrow-backing")
			for _, alert := range alerts {
				if alert.Class == "subnet-epoch-progress" || alert.Class == "subnet-payout-root" {
					t.Fatalf("an unknown cutover decided a row: %+v", alert)
				}
			}
		})
	}
}

// Rows before an operator's first v2 epoch, and every row of an operator
// without a registered vault2 pool, keep every v1 rule.
func TestSubnetSettlementPreCutoverRowsKeepV1Rules(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, tc := range []struct {
		name string
		pool stabi.ISTSettlementVault2Pool
	}{
		{name: "unregistered-pool", pool: stabi.ISTSettlementVault2Pool{}},
		{name: "later-first-epoch", pool: stabi.ISTSettlementVault2Pool{Hotkey: [32]byte{0x52, 1}, FirstEpoch: first + 1, Registered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			f.pools[1] = tc.pool
			alerts := f.run(t, f.chain.end(first)+850)
			finalization := requireSubnetReplayAlert(t, alerts, "subnet-epoch-progress", "subnet-settlement/no-1", "epoch-2/finalization")
			root := requireSubnetReplayAlert(t, alerts, "subnet-epoch-progress", "subnet-settlement/no-1", "epoch-2/root")
			if finalization.Severity != SeverityPage || root.Severity != SeverityWarn {
				t.Fatalf("v1 rules changed before the cutover: finalization=%s root=%s", finalization.Severity, root.Severity)
			}
		})
	}
}

// The cutover read depends on the pinned v2 pair, so a changed v2 address must
// change the settings generation instead of keeping a stale pin.
func TestSubnetSettlementCutoverPinGenerationIncludesV2Addresses(t *testing.T) {
	_, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetDeployment = NewSubnetReplayPin(subnetSettlementCutoverTestPin())
	if settings.SubnetDeployment.settlementVaultV2 != subnetSettlementTestVaultV2 || settings.SubnetDeployment.settlementV2 != subnetSettlementTestSettlementV2 {
		t.Fatal("the pin did not keep the v2 pair")
	}
	current := settings.withDefaults()
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if equal, err := check(context.Background(), settings); err != nil || !equal {
		t.Fatal("equal settings differ")
	}
	current.SubnetDeployment.settlementVaultV2 = common.Address{9}
	if equal, err := check(context.Background(), settings); err != nil || equal {
		t.Fatal("a changed v2 vault pin kept the settings generation")
	}
}
