// The failure count's abandon gate through the real client and database: the
// latest block and the commit cutoff from the chain, and the commit intent
// reserved under root:E:N.
package controller

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// sweepCommitFactsRpc serves the latest and finalized heads, and the epoch's
// end block and root window only at the finalized head.
type sweepCommitFactsRpc struct {
	stateLock sync.Mutex
	chainId   uint64
	epoch     uint64
	latest    uint64
	finalized uint64
	end       uint64
	window    uint64
	// policyFault makes policyAt fail, so the cutoff cannot be read.
	policyFault bool
}

func (self *sweepCommitFactsRpc) set(latest uint64, policyFault bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.latest, self.policyFault = latest, policyFault
}

func (self *sweepCommitFactsRpc) ChainId(context.Context) (hexutil.Uint64, error) {
	return hexutil.Uint64(self.chainId), nil
}

func (self *sweepCommitFactsRpc) GetBlockByNumber(_ context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var block uint64
	switch {
	case full:
		return nil, errors.New("unexpected full block")
	case tag == rpc.LatestBlockNumber:
		block = self.latest
	case tag == rpc.FinalizedBlockNumber:
		block = self.finalized
	default:
		return nil, errors.New("unexpected block selector")
	}
	return map[string]any{"number": hexutil.EncodeUint64(block), "hash": stTransactionReconcileBlockHash(block), "timestamp": "0x65000000"}, nil
}

func (self *sweepCommitFactsRpc) Call(_ context.Context, call map[string]hexutil.Bytes, block rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if number, ok := block.Number(); !ok || number != rpc.BlockNumber(self.finalized) {
		return nil, errors.New("a commit fact read off the finalized head")
	}
	data := call["data"]
	if len(data) == 0 {
		data = call["input"]
	}
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	coordinator, epoch := stabi.NewSTCoordinator(), new(big.Int).SetUint64(self.epoch)
	switch {
	case bytes.Equal(data, coordinator.PackEpochEndBlock(epoch)):
		return parsed.Methods["epochEndBlock"].Outputs.Pack(new(big.Int).SetUint64(self.end))
	case bytes.Equal(data, coordinator.PackPolicyAt(epoch)) && !self.policyFault:
		return parsed.Methods["policyAt"].Outputs.Pack(stabi.STCoordinatorPolicySnapshot{RootCommitWindowBlocks: self.window, FinalizeOffsetBlocks: 14_400, CloseGraceBlocks: 120, EpochDepositCapRao: new(big.Int), CampaignDepositCapRao: new(big.Int)})
	}
	return nil, errors.New("unexpected commit fact read")
}

// newSweepCommitFactsClient is a real client on the fixture endpoint, with
// the v1 sweep of epoch 4 configured for operator 1.
func newSweepCommitFactsClient(t testing.TB, fixture *sweepCommitFactsRpc) *CoreStClient {
	t.Helper()
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("eth", fixture); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(rpcServer)
	rpcClient, err := ethclient.Dial(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rpcClient.Close(); httpServer.Close(); rpcServer.Stop() })
	rootKey, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 77))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &StConfig{Profile: "testnet", Enabled: true, ChainId: fixture.chainId, GenesisHash: [32]byte{1}, NoId: 1, ContractAddress: common.Address{2}, SettlementVault: common.Address{3}, DeploymentId: "sweep-commit-facts", RootKey: rootKey, SettlementV2: common.HexToAddress(testLedgerV2Settlement), VaultV2: common.HexToAddress(testLedgerV2Vault), V1SweepEpoch: 4, RpcUrls: []string{httpServer.URL}}
	return &CoreStClient{cfg: cfg, coordinator: stabi.NewSTCoordinator(), vault: stabi.NewSTSettlementVault(), clients: map[string]*ethclient.Client{httpServer.URL: rpcClient}}
}

// reserveSweepCommitIntent reserves a commit intent from the root key under
// root:E:N, as CommitPayoutRoot does before it signs.
func reserveSweepCommitIntent(t testing.TB, client *CoreStClient, epoch uint64, noId uint64) {
	t.Helper()
	cfg := client.cfg
	logicalKey, err := stTransactionLogicalKey(cfg, fmt.Sprintf("root:%d:%d", epoch, noId))
	if err != nil {
		t.Fatal(err)
	}
	calldata := client.coordinator.PackCommitOperatorRoot(new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), StV1SweepRoot(sweepTestVault2Coldkey), [32]byte{0xaa})
	from := strings.ToLower(crypto.PubkeyToAddress(cfg.RootKey.PublicKey).Hex())
	if model.ReserveStTransactionIntent(context.Background(), logicalKey, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, "0x"+hex.EncodeToString(cfg.GenesisHash[:]), from, strings.ToLower(cfg.ContractAddress.Hex()), crypto.Keccak256Hash(calldata).Hex(), calldata, 7) == nil {
		t.Fatalf("reserve root:%d:%d", epoch, noId)
	}
}

// Through the real client, the count's abandon reads the latest block, the
// cutoff end(4) + root window - the commit margin at the finalized head
// (9449928 on mainnet), and whether any intent is reserved under root:4:1, the
// key the sweep commit is sent under. It alerts without abandoning one block
// before the cutoff, while the cutoff cannot be read, and with the commit
// intent reserved; it abandons at the cutoff when only other epochs' or
// operators' commit intents exist. The abandon is a recorded publish row.
func TestStV1SweepCountAbandonReadsTheChainAndTheCommitIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := &sweepCommitFactsRpc{chainId: 945, epoch: 4, finalized: 9449890, end: 9448828, window: 1200}
		client := newSweepCommitFactsClient(t, fixture)
		cfg := client.cfg
		SetStConfig(cfg)
		t.Cleanup(func() { SetStConfig(nil) })
		SetStV1SweepClient(client)
		t.Cleanup(func() { SetStV1SweepClient(nil) })
		alerts := 0
		priorAlert := stV1SweepAlertSink
		stV1SweepAlertSink = func(string, string) { alerts++ }
		t.Cleanup(func() { stV1SweepAlertSink = priorAlert })

		fixture.set(9449927, false)
		latest, cutoff, reserved, err := client.V1SweepCommitFacts(ctx, 4, 1)
		if err != nil || latest != 9449927 || cutoff != 9449928 || reserved {
			t.Fatalf("facts: latest %d cutoff %d reserved %t err %v", latest, cutoff, reserved, err)
		}
		abandons := func() int {
			count := 0
			for _, publish := range model.GetStPublishes(ctx, cfg.DeploymentKey(), 4) {
				if publish.Kind == StPublishKindV1SweepAbandon {
					count++
				}
			}
			return count
		}
		failed := &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "rpc down", Retry: true}
		turn := func(name string, latest uint64, policyFault bool, abandon bool, abandoned int) {
			fixture.set(latest, policyFault)
			before := alerts
			failures, stop := StV1SweepAfterTurn(ctx, 4, stV1SweepAbandonFailures-1, failed, nil)
			if failures != stV1SweepAbandonFailures || stop != abandon || abandons() != abandoned || alerts == before {
				t.Fatalf("%s: failures %d stop %t abandon rows %d alerts %d", name, failures, stop, abandons(), alerts-before)
			}
		}
		turn("one block before the cutoff", 9449927, false, false, 0)
		turn("an unreadable cutoff", 9449928, true, false, 0)
		reserveSweepCommitIntent(t, client, 5, 1)
		reserveSweepCommitIntent(t, client, 4, 2)
		turn("at the cutoff beside other epochs' and operators' commits", 9449928, false, true, 1)
		reserveSweepCommitIntent(t, client, 4, 1)
		if _, _, reserved, err := client.V1SweepCommitFacts(ctx, 4, 1); err != nil || !reserved {
			t.Fatalf("the reserved commit: reserved %t err %v", reserved, err)
		}
		turn("past the cutoff with the commit reserved", 9449978, false, false, 1)
		// A failed intent read is an error, never "not reserved".
		logicalKey, err := stTransactionLogicalKey(cfg, "root:4:1")
		if err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if reserved, err := stV1SweepIntentReserved(canceled, logicalKey); err == nil || reserved {
			t.Fatalf("a failed intent read: reserved %t err %v", reserved, err)
		}
	})
}
