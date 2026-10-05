// Advance a private logical clock only at actual RPC boundaries. Composite
// owners cannot renew their budget between a finalized head and its members.
package controller

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/stabi"
)

func stCompositeFixture(t *testing.T) (*CoreStClient, *atomic.Int64, *atomic.Int64, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	coordinator := stabi.NewSTCoordinator()
	policy := stabi.STCoordinatorPolicySnapshot{EpochBlocks: 2400, RootCommitWindowBlocks: 10, FinalizeOffsetBlocks: 20, CloseGraceBlocks: 5, EpochDepositCapRao: big.NewInt(1), CampaignDepositCapRao: big.NewInt(2)}
	values := map[string]json.RawMessage{
		hex.EncodeToString(coordinator.PackCurrentEpoch()[:4]):                                     stEpochRPCResult(t, "currentEpoch", big.NewInt(7)),
		hex.EncodeToString(coordinator.PackPolicyAt(big.NewInt(7))[:4]):                            stEpochRPCResult(t, "policyAt", policy),
		hex.EncodeToString(coordinator.PackEpochStartBlock(big.NewInt(7))[:4]):                     stEpochRPCResult(t, "epochStartBlock", big.NewInt(700)),
		hex.EncodeToString(coordinator.PackCumulativeConviction(big.NewInt(3))[:4]):                stEpochRPCResult(t, "cumulativeConviction", big.NewInt(100)),
		hex.EncodeToString(coordinator.PackEpochDeposits(big.NewInt(7), big.NewInt(3))[:4]):        stEpochRPCResult(t, "epochDeposits", big.NewInt(20)),
		hex.EncodeToString(coordinator.PackEpochConvictionAdded(big.NewInt(7), big.NewInt(3))[:4]): stEpochRPCResult(t, "epochConvictionAdded", big.NewInt(30)),
	}
	values[hex.EncodeToString(coordinator.PackSelfColdkey()[:4])] = stEpochRPCResult(t, "selfColdkey", [32]byte{8})
	stakeCall, err := stPackGetStake([32]byte{7}, [32]byte{8}, 9)
	if err != nil {
		t.Fatal(err)
	}
	stakeWire, _ := json.Marshal("0x" + hex.EncodeToString(big.NewInt(500).FillBytes(make([]byte, 32))))
	values[hex.EncodeToString(stakeCall[:4])] = stakeWire
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	commitWire, err := parsed.Methods["rootCommitments"].Outputs.Pack([32]byte{41}, [32]byte{42}, common.HexToAddress("0x3000000000000000000000000000000000000003"), uint64(90))
	if err != nil {
		t.Fatal(err)
	}
	values[hex.EncodeToString(coordinator.PackRootCommitments(big.NewInt(7), big.NewInt(3))[:4])], _ = json.Marshal("0x" + hex.EncodeToString(commitWire))
	vault := stabi.NewSTSettlementVault()
	vaultAbi, err := stabi.STSettlementVaultMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	entitlementWire, err := vaultAbi.Methods["entitlement"].Outputs.Pack(stabi.STSettlementVaultEntitlement{PayoutRoot: [32]byte{41}, ArtifactHash: [32]byte{42}, Funded: big.NewInt(100), Total: big.NewInt(100), Claimed: big.NewInt(20), ExpiryBlock: 1000, Status: 2})
	if err != nil {
		t.Fatal(err)
	}
	values[hex.EncodeToString(vault.PackEntitlement(big.NewInt(7), big.NewInt(3))[:4])], _ = json.Marshal("0x" + hex.EncodeToString(entitlementWire))
	elapsed, step := &atomic.Int64{}, &atomic.Int64{}
	heads, members := &atomic.Int32{}, &atomic.Int32{}
	client := newStBlockBatchClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request stEpochRPCRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var result any
		switch request.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getBlockByNumber":
			head := heads.Add(1)
			result = stBlockBatchResult(100 + uint64(head-1))
			elapsed.Add(step.Load())
		case "eth_call":
			var call map[string]string
			var block string
			if len(request.Params) != 2 || json.Unmarshal(request.Params[0], &call) != nil || json.Unmarshal(request.Params[1], &block) != nil || block != "0x64" {
				t.Error("composite members left original finalized block")
				w.WriteHeader(400)
				return
			}
			input := call["input"]
			if input == "" {
				input = call["data"]
			}
			selector := strings.TrimPrefix(input, "0x")
			if len(selector) >= 8 {
				selector = selector[:8]
			}
			value, ok := values[selector]
			if !ok {
				t.Error("unexpected composite read selector", selector)
				w.WriteHeader(400)
				return
			}
			result = value
			members.Add(1)
			elapsed.Add(step.Load())
		default:
			t.Error("composite read attempted non-read method", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	client.coordinator = coordinator
	client.vault = vault
	client.cfg.Netuid, client.cfg.DepositHotkey = 9, [32]byte{7}
	client.cfg.SettlementVault = common.HexToAddress("0x4000000000000000000000000000000000000004")
	client.cfg.ContractAddress = common.HexToAddress("0x2000000000000000000000000000000000000002")
	base := time.Now()
	client.readHooks = stRpcReadHooks{now: func() time.Time { return base.Add(time.Duration(elapsed.Load())) }, wait: func(ctx context.Context, d time.Duration) error { elapsed.Add(int64(d)); return ctx.Err() }}
	return client, elapsed, step, heads, members
}

func TestStCompositeEpochCannotRenewMemberReadBudget(t *testing.T) {
	client, elapsed, step, heads, members := stCompositeFixture(t)
	step.Store(int64(3 * time.Second))
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	value, err := client.Epoch(owner)
	if value != nil || !errors.Is(err, context.DeadlineExceeded) || heads.Load() != 1 || members.Load() != 3 {
		t.Fatal("composite epoch renewed a member read budget", value, heads.Load(), members.Load(), err)
	}
	elapsed.Store(0)
	step.Store(0)
	heads.Store(0)
	members.Store(0)
	value, err = client.Epoch(owner)
	if err != nil || value == nil || value.Epoch != 7 || heads.Load() != 1 || members.Load() != 3 {
		t.Fatal("healthy next epoch owner did not recover", value, err)
	}
}

func TestStCompositeScalarHeadAndValueShareOneReadOwner(t *testing.T) {
	client, _, step, heads, members := stCompositeFixture(t)
	step.Store(int64(6 * time.Second))
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	value, err := client.PendingEpoch(owner)
	if value != 0 || !errors.Is(err, context.DeadlineExceeded) || heads.Load() != 1 || members.Load() != 1 {
		t.Fatal("scalar read renewed budget after selecting its original head", value, err)
	}
}

func TestStCompositeConvictionUsesOneOriginalHead(t *testing.T) {
	client, _, _, heads, members := stCompositeFixture(t)
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	value, err := client.ConvictionBeforeEpoch(owner, 7, 3)
	if err != nil || value == nil || value.Cmp(big.NewInt(50)) != 0 || heads.Load() != 1 || members.Load() != 3 {
		t.Fatal("conviction arithmetic mixed original snapshot members", value, heads.Load(), members.Load(), err)
	}
}

func TestStCompositePoolStateUsesOneOriginalHead(t *testing.T) {
	client, _, _, heads, members := stCompositeFixture(t)
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	value, err := client.PoolState(owner, 7, 3)
	if err != nil || value == nil || value.PoolTotalRao.Cmp(big.NewInt(100)) != 0 || value.ClaimedRao.Cmp(big.NewInt(20)) != 0 || !value.Finalized || heads.Load() != 1 || members.Load() != 2 {
		t.Fatal("pool state mixed original contract snapshots", value, heads.Load(), members.Load(), err)
	}
}

func TestStCompositeNativeStakeKeepsOriginalColdkeyHead(t *testing.T) {
	client, _, _, heads, members := stCompositeFixture(t)
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	value, err := client.UnaccountedStakeRao(owner)
	if err != nil || value == nil || value.Cmp(big.NewInt(500)) != 0 || heads.Load() != 1 || members.Load() != 2 {
		t.Fatal("native stake moved away from original coldkey head", value, heads.Load(), members.Load(), err)
	}
}

func TestStCompositeBindingsKeepHeadBudgetAndHealthyRecovery(t *testing.T) {
	fixture := &stBindingBatchRPC{t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"), finalized: 999, epoch: 41, startBlock: 700, closeBlock: 700}
	elapsed, step := &atomic.Int64{}, &atomic.Int64{}
	step.Store(int64(6 * time.Second))
	client := newStBlockBatchClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			t.Error(err)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var request stEpochRPCRequest
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &request) == nil && request.Method != "eth_chainId" {
			elapsed.Add(step.Load())
		}
		fixture.ServeHTTP(w, r)
	}))
	client.cfg.ContractAddress = fixture.address
	client.coordinator = stabi.NewSTCoordinator()
	base := time.Now()
	client.readHooks = stRpcReadHooks{now: func() time.Time { return base.Add(time.Duration(elapsed.Load())) }}
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	value, err := client.BindingsAt(owner, [][16]byte{{2}}, 41, 700, 700)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("binding census renewed the original head owner", value, err)
	}
	elapsed.Store(0)
	step.Store(0)
	value, err = client.BindingsAt(owner, [][16]byte{{2}}, 41, 700, 700)
	if err != nil || len(value) != 1 || !value[0].Active {
		t.Fatal("healthy next binding census did not recover", value, err)
	}
}
