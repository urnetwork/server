package monitor

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"
)

// Regression for the epoch-3 incident: computation has produced no leaves or
// task result. Only independent finalized chain state can raise the T-2h page.
func TestSubnetRootDeadlineStalledClosePagesAtTwoHours(t *testing.T) {
	chain := newSubnetSettlementTestChain(t, 1)
	chain.currentEpoch = 4
	chain.policy.EpochBlocks = 5000
	chain.policy.RootCommitWindowBlocks = 1200
	chain.policy.FinalizeOffsetBlocks = 1500
	end := chain.end(3)
	chain.capture(3, 1, 1000, end)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, end+600)
	chain.install(node)
	settings, source := subnetReplayTestSettings(t, node)
	settings.SubnetReplay.Schema = ""
	settings.SubnetReplay.ApiOrigins = nil
	source.postgresFn = func(string) ([]Row, error) {
		t.Fatal("deadline alert depends on producer database state")
		return nil, nil
	}

	var alerts Alerts
	for _, signal := range NewSignals() {
		if signal.Key() == "subnet-settlement" || signal.Key() == "subnet-root-deadline" {
			observed, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			alerts = append(alerts, observed...)
		}
	}
	alert := requireSubnetReplayAlert(t, alerts, "subnet-root-deadline", "subnet-root-deadline/no-1", "epoch-3/root")
	if alert.Severity != SeverityPage || alert.Sustain != 1 || alert.unobservable {
		t.Fatalf("T-2h must page on its first observation: %+v", alert)
	}
	if source.apiCalls != 0 {
		t.Fatal("deadline alert depends on artifact publication")
	}
	requireSubnetReplayRedacted(t, alerts)

	// A confirmed root resolves the deadline incident even without its artifact.
	chain.roots[subnetSettlementKey{epoch: 3, noId: 1}] = &stabi.RootCommitmentsOutput{PayoutRoot: [32]byte{1}}
	for _, signal := range NewSignals() {
		if signal.Key() == "subnet-root-deadline" {
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("confirmed root did not resolve: alerts=%+v err=%v", alerts, err)
			}
		}
	}
}

var subnetRootDeadlineTestSettlementV2 = common.HexToAddress("0x00000000000000000000000000000000000c0004")
var subnetRootDeadlineTestVaultV2 = common.HexToAddress("0x00000000000000000000000000000000000c0005")

// Real ABI encoding through the relay; the fixture never reads a database,
// producer task status, API, or historical event journal.
type subnetRootDeadlineFixture struct {
	chain      *subnetSettlementTestChain
	node       *subnetReplayTestNode
	settings   SignalSettings
	source     *subnetReplayTestSource
	pool       stabi.ISTSettlementVault2Pool
	pending    stabi.ISTSettlementVault2Pending
	roots      []stabi.ISTSettlementVault2Root
	lastFunded uint64
	funded     *big.Int
}

func newSubnetRootDeadlineFixture(t *testing.T, epoch uint64, remaining int64, v2 bool) *subnetRootDeadlineFixture {
	t.Helper()
	chain := newSubnetSettlementTestChain(t, 1)
	chain.currentEpoch = epoch + 1
	chain.policy.EpochBlocks = 5000
	chain.policy.RootCommitWindowBlocks = 1200
	chain.policy.FinalizeOffsetBlocks = 1500
	block := uint64(int64(chain.end(epoch)+1200) - remaining)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, block)
	chain.install(node)
	settings, source := subnetReplayTestSettings(t, node)
	settings.SubnetReplay.Schema = ""
	settings.SubnetReplay.ApiOrigins = nil
	source.postgresFn = func(string) ([]Row, error) {
		t.Fatal("deadline reader queried producer database")
		return nil, nil
	}
	f := &subnetRootDeadlineFixture{
		chain: chain, node: node, settings: settings, source: source,
		pool:       stabi.ISTSettlementVault2Pool{Registered: true, FirstEpoch: 4, Hotkey: [32]byte{4}},
		pending:    stabi.ISTSettlementVault2Pending{Total: new(big.Int)},
		lastFunded: epoch, funded: big.NewInt(1000),
	}
	if v2 {
		pin := append(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash),
			[]byte(fmt.Sprintf("  settlement_v2_address: %q\n  vault_v2_address: %q\n", subnetRootDeadlineTestSettlementV2.Hex(), subnetRootDeadlineTestVaultV2.Hex()))...)
		f.settings.SubnetDeployment = NewSubnetReplayPin(pin)
		if f.settings.SubnetDeployment.publicStatus() != subnetReplayPinReviewed {
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
		node.contracts[subnetRootDeadlineTestSettlementV2] = &subnetReplayTestContract{contract: settlement, handle: func(method string, args []any) ([]any, string) {
			switch method {
			case "v1Coordinator":
				return []any{subnetReplayTestCoordinator}, ""
			case "vault":
				return []any{subnetRootDeadlineTestVaultV2}, ""
			default:
				t.Fatalf("unexpected v2 settlement view %s", method)
				return nil, ""
			}
		}}
		node.contracts[subnetRootDeadlineTestVaultV2] = &subnetReplayTestContract{contract: vault, handle: func(method string, args []any) ([]any, string) {
			switch method {
			case "coordinator":
				return []any{subnetRootDeadlineTestSettlementV2}, ""
			case "netuid":
				return []any{uint16(25)}, ""
			case "pools":
				return []any{f.pool}, ""
			case "pending":
				return []any{f.pending}, ""
			case "rootCount":
				return []any{big.NewInt(int64(len(f.roots)))}, ""
			case "roots":
				return []any{f.roots[args[1].(*big.Int).Uint64()]}, ""
			case "lastFundedEpoch":
				return []any{new(big.Int).SetUint64(f.lastFunded)}, ""
			case "fundedThrough":
				if args[1].(*big.Int).Uint64() != epoch {
					t.Fatal("wrong epoch funding read")
				}
				return []any{f.funded}, ""
			default:
				t.Fatalf("unexpected v2 vault view %s", method)
				return nil, ""
			}
		}}
	}
	return f
}

func (f *subnetRootDeadlineFixture) run(t *testing.T) Alerts {
	t.Helper()
	alerts, err := NewSubnetRootDeadlineSignal().Run(context.Background(), f.settings)
	if err != nil {
		t.Fatal(err)
	}
	if f.source.apiCalls != 0 {
		t.Fatal("deadline reader queried artifacts")
	}
	for _, method := range f.node.calls {
		if method == "eth_getLogs" {
			t.Fatal("deadline reader depends on history replay")
		}
	}
	if f.node.exchanges > f.settings.SubnetReplay.RpcMaximumExchanges {
		t.Fatal("unbounded deadline read")
	}
	requireSubnetReplayRedacted(t, alerts, subnetRootDeadlineTestSettlementV2.Hex(), subnetRootDeadlineTestVaultV2.Hex())
	return alerts
}

func TestSubnetRootDeadlineWindowBoundaries(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		for _, remaining := range []int64{1200, 601, 600, 1, 0, -1} {
			t.Run(fmt.Sprintf("v2=%t/remaining=%d", v2, remaining), func(t *testing.T) {
				f := newSubnetRootDeadlineFixture(t, 4, remaining, v2)
				alerts := f.run(t)
				if 600 < remaining {
					if len(alerts) != 0 {
						t.Fatalf("alert before T-2h: %+v", alerts)
					}
					return
				}
				alert := requireSubnetReplayAlert(t, alerts, "subnet-root-deadline", "subnet-root-deadline/no-1", "epoch-4/root")
				if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 1 || alert.unobservable {
					t.Fatalf("deadline must page once immediately: %+v", alerts)
				}
				if remaining < 0 && !strings.Contains(alert.Observed, "deadline_passed=true") {
					t.Fatal("missed deadline not identified")
				}
			})
		}
	}
}

func TestSubnetRootDeadlineV2CutoverAndRootState(t *testing.T) {
	previous := stabi.ISTSettlementVault2Root{Epoch: 4, Root: [32]byte{4}, Total: big.NewInt(500)}
	for _, tc := range []struct {
		name    string
		epoch   uint64
		mutate  func(*subnetRootDeadlineFixture)
		page    bool
		version string
	}{
		{"first-v2-epoch", 4, func(f *subnetRootDeadlineFixture) {}, true, "v2"},
		{"before-cutover", 3, func(f *subnetRootDeadlineFixture) {}, true, "v1"},
		{"unregistered-pool", 5, func(f *subnetRootDeadlineFixture) { f.pool = stabi.ISTSettlementVault2Pool{} }, true, "v1"},
		{"v1-root-does-not-cover-v2", 5, func(f *subnetRootDeadlineFixture) {
			f.chain.roots[subnetSettlementKey{epoch: 5, noId: 1}] = &stabi.RootCommitmentsOutput{PayoutRoot: [32]byte{5}}
		}, true, "v2"},
		{"pending-current", 5, func(f *subnetRootDeadlineFixture) {
			f.pending = stabi.ISTSettlementVault2Pending{Seq: 2, Epoch: 5, Root: [32]byte{5}, Total: big.NewInt(1000)}
		}, false, ""},
		{"pending-previous", 5, func(f *subnetRootDeadlineFixture) {
			f.pending = stabi.ISTSettlementVault2Pending{Seq: 1, Epoch: 4, Root: [32]byte{4}, Total: big.NewInt(500)}
		}, true, "v2"},
		{"finalized-current", 5, func(f *subnetRootDeadlineFixture) {
			f.roots = []stabi.ISTSettlementVault2Root{previous, {Epoch: 5, Root: [32]byte{5}, Total: big.NewInt(1000)}}
		}, false, ""},
		{"finalized-previous", 5, func(f *subnetRootDeadlineFixture) { f.roots = []stabi.ISTSettlementVault2Root{previous} }, true, "v2"},
		{"zero-funding-closed", 5, func(f *subnetRootDeadlineFixture) { f.funded.SetUint64(0) }, false, ""},
		{"zero-funding-not-closed", 5, func(f *subnetRootDeadlineFixture) { f.funded.SetUint64(0); f.lastFunded = 4 }, true, "v2"},
		{"no-new-funding", 5, func(f *subnetRootDeadlineFixture) {
			f.roots = []stabi.ISTSettlementVault2Root{previous}
			f.funded.SetUint64(500)
		}, false, ""},
		{"inactive-v1", 3, func(f *subnetRootDeadlineFixture) {
			op := f.chain.versions[1]
			op.Active = false
			f.chain.versions[1] = op
		}, false, ""},
		{"unknown-at-epoch", 3, func(f *subnetRootDeadlineFixture) {
			op := f.chain.versions[1]
			op.EffectiveEpoch = 4
			f.chain.versions[1] = op
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetRootDeadlineFixture(t, tc.epoch, 600, true)
			tc.mutate(f)
			alerts := f.run(t)
			if !tc.page {
				if len(alerts) != 0 {
					t.Fatalf("unnecessary deadline alert: %+v", alerts)
				}
				return
			}
			alert := requireSubnetReplayAlert(t, alerts, "subnet-root-deadline", "subnet-root-deadline/no-1", fmt.Sprintf("epoch-%d/root", tc.epoch))
			if !strings.Contains(alert.Observed, "settlement="+tc.version) {
				t.Fatalf("wrong cutover decision: %+v", alert)
			}
		})
	}
}

// The one-time v1 carry sweep commits a v1 root at the first v2 epoch. That
// epoch's deadline is judged on vault2 alone and v1 root state is never read
// for it, so a committed sweep root cannot satisfy the deadline and a missing
// sweep root cannot page it, before or after the deadline.
func TestSubnetRootDeadlineCutoverEpochIgnoresV1Sweep(t *testing.T) {
	sweep := &stabi.RootCommitmentsOutput{
		PayoutRoot: [32]byte(merkle.PayoutLeaf([32]byte{0x5e, 0xc0, 0x1d}, big.NewInt(10_000))), ArtifactHash: [32]byte{0x5e},
		Committer: common.BigToAddress(big.NewInt(0xa01)), CommitBlock: 1,
	}
	for _, tc := range []struct {
		name          string
		remaining     int64
		sweep, v2Root bool
		page          bool
	}{
		{name: "sweep-root-is-not-the-v2-root", remaining: 600, sweep: true, page: true},
		{name: "sweep-root-after-the-deadline", remaining: -1, sweep: true, page: true},
		{name: "missing-sweep-root-near-the-deadline", remaining: 600, v2Root: true},
		{name: "missing-sweep-root-after-the-deadline", remaining: -1, v2Root: true},
		{name: "sweep-and-v2-roots", remaining: 600, sweep: true, v2Root: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetRootDeadlineFixture(t, 4, tc.remaining, true)
			if f.pool.FirstEpoch != 4 {
				t.Fatal("epoch 4 must be the pool's first v2 epoch")
			}
			if tc.sweep {
				f.chain.roots[subnetSettlementKey{epoch: 4, noId: 1}] = sweep
			}
			if tc.v2Root {
				f.pending = stabi.ISTSettlementVault2Pending{Seq: 1, Epoch: 4, Root: [32]byte{4}, Total: big.NewInt(1000)}
			}
			coordinator := f.node.contracts[subnetReplayTestCoordinator]
			ordinary := coordinator.handle
			coordinator.handle = func(method string, args []any) ([]any, string) {
				if method == "rootCommitments" {
					t.Fatal("a v1 root was read for a v2 epoch")
				}
				return ordinary(method, args)
			}
			alerts := f.run(t)
			if !tc.page {
				if len(alerts) != 0 {
					t.Fatalf("the v1 sweep changed a v2 deadline: %+v", alerts)
				}
				return
			}
			alert := requireSubnetReplayAlert(t, alerts, "subnet-root-deadline", "subnet-root-deadline/no-1", "epoch-4/root")
			if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 1 || !strings.Contains(alert.Observed, "settlement=v2") {
				t.Fatalf("a v1 sweep root satisfied a v2 deadline: %+v", alerts)
			}
		})
	}
}

// Failed or inconsistent reads cannot be interpreted as a missing root or a
// healthy epoch. In particular, recheck the checkpoint before emitting a page.
func TestSubnetRootDeadlineSourceGaps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*subnetRootDeadlineFixture)
		reason string
	}{
		{"rpc-failure", func(f *subnetRootDeadlineFixture) { f.node.failMethods["eth_call"] = true }, "view-failed"},
		{"wrong-chain", func(f *subnetRootDeadlineFixture) { f.node.chainId++ }, "chain-identity-mismatch"},
		{"operator-bound", func(f *subnetRootDeadlineFixture) {
			f.chain.operators = []uint64{1, 2}
			f.settings.SubnetReplay.MaximumOperators = 1
		}, "operator-census-bound"},
		{"zero-lead", func(f *subnetRootDeadlineFixture) { f.settings.SubnetReplay.RootDeadlineLeadBlocks = 0 }, "deadline-lead-invalid"},
		{"wrong-v2-link", func(f *subnetRootDeadlineFixture) {
			contract := f.node.contracts[subnetRootDeadlineTestVaultV2]
			old := contract.handle
			contract.handle = func(method string, args []any) ([]any, string) {
				if method == "coordinator" {
					return []any{subnetReplayTestCoordinator}, ""
				}
				return old(method, args)
			}
		}, "deployment-link-mismatch"},
		{"reorg", func(f *subnetRootDeadlineFixture) {
			contract := f.node.contracts[subnetRootDeadlineTestVaultV2]
			old := contract.handle
			contract.handle = func(method string, args []any) ([]any, string) {
				if method == "fundedThrough" {
					f.node.blocks[f.node.finalized] = subnetReplayTestBlockHash(f.node.finalized, 1)
				}
				return old(method, args)
			}
		}, "checkpoint-not-canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetRootDeadlineFixture(t, 4, 600, true)
			tc.mutate(f)
			alerts := f.run(t)
			alert := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-root-deadline/coverage", "source")
			if len(alerts) != 1 || !strings.Contains(alert.Observed, "state=unknown reason="+tc.reason) {
				t.Fatalf("source gap misclassified: %+v", alerts)
			}
		})
	}
}

func TestSubnetRootDeadlineMixedOperators(t *testing.T) {
	for _, v2Confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2-confirmed=%t", v2Confirmed), func(t *testing.T) {
			f := newSubnetRootDeadlineFixture(t, 4, 600, true)
			f.chain.operators = []uint64{1, 2}
			f.chain.versions[2] = f.chain.versions[1]
			contract := f.node.contracts[subnetRootDeadlineTestVaultV2]
			old := contract.handle
			contract.handle = func(method string, args []any) ([]any, string) {
				if method == "pools" && args[0].(*big.Int).Uint64() == 2 {
					return []any{stabi.ISTSettlementVault2Pool{}}, ""
				}
				return old(method, args)
			}
			missing := uint64(1)
			if v2Confirmed {
				f.pending = stabi.ISTSettlementVault2Pending{Seq: 1, Epoch: 4, Root: [32]byte{4}, Total: big.NewInt(1000)}
				missing = 2
			} else {
				f.chain.roots[subnetSettlementKey{epoch: 4, noId: 2}] = &stabi.RootCommitmentsOutput{PayoutRoot: [32]byte{4}}
			}
			alerts := f.run(t)
			requireSubnetReplayAlert(t, alerts, "subnet-root-deadline", fmt.Sprintf("subnet-root-deadline/no-%d", missing), "epoch-4/root")
			if len(alerts) != 1 {
				t.Fatalf("one operator's root affected another: %+v", alerts)
			}
		})
	}
}

func TestSubnetRootDeadlineNoClosedEpoch(t *testing.T) {
	f := newSubnetRootDeadlineFixture(t, 0, 600, false)
	f.chain.currentEpoch = 0
	if alerts := f.run(t); len(alerts) != 0 {
		t.Fatalf("no epoch has closed: %+v", alerts)
	}
}

func TestSubnetRootDeadlineCancellationWithholdsVerdict(t *testing.T) {
	f := newSubnetRootDeadlineFixture(t, 4, 600, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	contract := f.node.contracts[subnetRootDeadlineTestVaultV2]
	old := contract.handle
	contract.handle = func(method string, args []any) ([]any, string) {
		if method == "fundedThrough" {
			cancel()
		}
		return old(method, args)
	}
	alerts, err := NewSubnetRootDeadlineSignal().Run(ctx, f.settings)
	if len(alerts) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: alerts=%+v err=%v", alerts, err)
	}
}

func TestSubnetRootDeadlinePinGenerationIncludesV2Addresses(t *testing.T) {
	f := newSubnetRootDeadlineFixture(t, 4, 600, true)
	current := f.settings.withDefaults()
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if equal, err := check(context.Background(), f.settings); err != nil || !equal {
		t.Fatal("equal settings differ")
	}
	current.SubnetDeployment.settlementVaultV2 = common.Address{9}
	if equal, err := check(context.Background(), f.settings); err != nil || equal {
		t.Fatal("changed v2 vault pin retained generation")
	}
}
