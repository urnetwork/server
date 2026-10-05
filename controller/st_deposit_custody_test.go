// Deterministic raw-Rpc and durable-intent regressions for isolated operator
// deposit custody. Native share flooring is modeled explicitly at inclusion.
package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Covers an empty account, partial preload and both possible runtime floors.
// Principal never absorbs the rounding allowance and inputs remain unchanged.
func TestStDepositFundingKeepsExactPrincipalAndBoundedRounding(t *testing.T) {
	for _, staged := range []int64{0, 1, 99, 100, 101, 102, 103, 500} {
		principal, balance := big.NewInt(100), big.NewInt(staged)
		funding, err := stDepositFundingAmount(principal, balance)
		if err != nil {
			t.Fatal(err)
		}
		if staged >= 102 {
			if funding.Sign() != 0 {
				t.Fatalf("already staged %d funded %s", staged, funding)
			}
		} else {
			if funding.Int64() != 103-staged {
				t.Fatalf("staged %d funded %s", staged, funding)
			}
			for loss := int64(0); loss <= 1; loss++ {
				credited := staged + funding.Int64() - loss
				if credited < 102 || credited > 103 {
					t.Fatalf("source funding with loss %d left %d", loss, credited)
				}
			}
		}
		if principal.Int64() != 100 || balance.Int64() != staged {
			t.Fatal("funding modified principal or retained balance")
		}
	}
}

// Invalid values and uint256 overflow cannot become a transfer or a wraparound.
func TestStDepositFundingRejectsInvalidAndOverflowAmounts(t *testing.T) {
	maximum := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	for _, input := range []struct{ principal, staged *big.Int }{
		{principal: nil, staged: big.NewInt(0)}, {principal: big.NewInt(0), staged: big.NewInt(0)},
		{principal: big.NewInt(-1), staged: big.NewInt(0)}, {principal: big.NewInt(1), staged: nil},
		{principal: big.NewInt(1), staged: big.NewInt(-1)}, {principal: maximum, staged: big.NewInt(0)},
		{principal: new(big.Int).Sub(maximum, big.NewInt(2)), staged: big.NewInt(0)},
	} {
		if _, err := stDepositFundingAmount(input.principal, input.staged); err == nil {
			t.Fatal("invalid funding envelope admitted")
		}
	}
}

// One endpoint owns mutable fixture state under a lock because Rpc batches may
// execute members concurrently. Fault changes occur only between joined calls.
type stDepositCustodyRpc struct {
	stClientKeyHistoryRPCFixture
	stateLock    sync.Mutex
	cfg          *StConfig
	custodyFault string
	staged       *big.Int
	source       *big.Int
	deposited    *big.Int
	depositNonce *big.Int
	accountNonce uint64
	minimumTao   uint64
	loss         int64
	lostReply    bool
	sent         []*types.Transaction
	receiptKVs   map[common.Hash]*types.Receipt
	viewCalls    int
}

// Serializes the inherited domain observation with receipt inclusion changes.
func (self *stDepositCustodyRpc) ChainId(ctx context.Context) (hexutil.Uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stClientKeyHistoryRPCFixture.ChainId(ctx)
}

// Native genesis stays independent of the Evm block-hash namespace.
func (self *stDepositCustodyRpc) GetBlockHash(ctx context.Context, block uint64) (*common.Hash, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stClientKeyHistoryRPCFixture.GetBlockHash(ctx, block)
}

// Historical receipt headers remain available after the current head advances.
func (self *stDepositCustodyRpc) GetBlockByNumber(_ context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if full {
		return nil, errors.New("unexpected full block")
	}
	block := self.boundary.Block
	if tag >= 0 {
		block = uint64(tag)
	}
	if block == 0 || block > self.boundary.Block {
		return nil, nil
	}
	result := stTransactionReconcileBlock(block)
	if self.fault == "canonical" && tag >= 0 {
		result["hash"] = common.Hash{0xff}
	}
	return result, nil
}

// Every custody and economics field comes from canonical ABI bytes at the
// requested hash. The inherited reader serves the immutable evidence anchor.
func (self *stDepositCustodyRpc) Call(ctx context.Context, call map[string]hexutil.Bytes, block rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.viewCalls++
	if block.BlockHash == nil || block.BlockNumber != nil || !block.RequireCanonical || *block.BlockHash != common.Hash(self.boundary.Hash) {
		return nil, errors.New("deposit view lost exact canonical hash")
	}
	data := call["data"]
	if len(data) == 0 {
		data = call["input"]
	}
	to := common.BytesToAddress(call["to"])
	coordinator := stabi.NewSTCoordinator()
	epoch := new(big.Int).SetUint64(self.boundary.Epoch)
	noId := new(big.Int).SetUint64(self.cfg.NoId)
	if to == stStakingPrecompileAddress {
		for index, coldkey := range [][32]byte{ss58.EvmMirrorPubkey(self.cfg.ContractAddress), ss58.EvmMirrorPubkey(crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey))} {
			packed, err := stPackGetStake(self.cfg.DepositHotkey, coldkey, self.cfg.Netuid)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(data, packed) {
				value := self.staged
				if index == 1 {
					value = self.source
				}
				return value.FillBytes(make([]byte, 32)), nil
			}
		}
		return nil, errors.New("wrong operator stake position")
	}
	if to == common.HexToAddress("0x0000000000000000000000000000000000000808") {
		return new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil).FillBytes(make([]byte, 32)), nil
	}
	if to == self.cfg.SettlementVault {
		if !bytes.Equal(data, stabi.NewSTSettlementVault().PackMinimumTransferTaoRao()) {
			return nil, errors.New("unknown vault read")
		}
		return new(big.Int).SetUint64(self.minimumTao).FillBytes(make([]byte, 32)), nil
	}
	if to != self.cfg.ContractAddress {
		return self.stClientKeyHistoryRPCFixture.Call(ctx, call, block)
	}
	operator := stabi.STCoordinatorOperatorVersion{Coldkey: [32]byte{0x41}, PoolHotkey: [32]byte{0x42}, DepositHotkey: self.cfg.DepositHotkey, DepositSigner: crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey), RootSigner: self.root, Active: self.fault != "inactive"}
	coldkey, reserve, owner := ss58.EvmMirrorPubkey(self.cfg.ContractAddress), self.cfg.ReserveSink, common.Address{0x61}
	allowance := big.NewInt(2)
	switch self.custodyFault {
	case "deposit-signer":
		operator.DepositSigner[0]++
	case "deposit-hotkey":
		operator.DepositHotkey[0]++
	case "root-signer":
		operator.RootSigner[0]++
	case "self-coldkey":
		coldkey[0]++
	case "reserve":
		reserve[0]++
	case "owner-key":
		owner = operator.DepositSigner
	case "rounding":
		allowance.SetInt64(3)
	}
	policyHash := self.domain.PolicyHash
	if self.fault == "policy" {
		policyHash[0]++
	}
	fields := []struct {
		method string
		data   []byte
		value  any
	}{
		{method: "operatorAt", data: coordinator.PackOperatorAt(noId, epoch), value: operator},
		{method: "policyAt", data: coordinator.PackPolicyAt(epoch), value: stabi.STCoordinatorPolicySnapshot{PolicyHash: policyHash, EpochDepositCapRao: big.NewInt(1_000), CampaignDepositCapRao: big.NewInt(10_000)}},
		{method: "selfColdkey", data: coordinator.PackSelfColdkey(), value: coldkey},
		{method: "reserveSink", data: coordinator.PackReserveSink(), value: reserve},
		{method: "nextDepositNonce", data: coordinator.PackNextDepositNonce(noId), value: self.depositNonce},
		{method: "epochEndBlock", data: coordinator.PackEpochEndBlock(epoch), value: big.NewInt(2_000)},
		{method: "epochDeposits", data: coordinator.PackEpochDeposits(epoch, noId), value: self.deposited},
		{method: "campaignReserved", data: coordinator.PackCampaignReserved(), value: self.deposited},
		{method: "RESERVE_ROUNDING_ALLOWANCE_RAO", data: coordinator.PackRESERVEROUNDINGALLOWANCERAO(), value: allowance},
		{method: "RUNTIME_SHARE_ROUNDING_ALLOWANCE_RAO", data: coordinator.PackRUNTIMESHAREROUNDINGALLOWANCERAO(), value: big.NewInt(1)},
		{method: "paused", data: coordinator.PackPaused(), value: self.custodyFault == "paused"},
		{method: "owner", data: coordinator.PackOwner(), value: owner},
	}
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		if bytes.Equal(data, field.data) {
			encoded, err := parsed.Methods[field.method].Outputs.Pack(field.value)
			if self.custodyFault == "trailing" {
				encoded = append(encoded, make([]byte, 32)...)
			}
			return encoded, err
		}
	}
	return self.stClientKeyHistoryRPCFixture.Call(ctx, call, block)
}

// Account nonce is independent of the coordinator deposit nonce.
func (self *stDepositCustodyRpc) GetTransactionCount(context.Context, common.Address, rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return hexutil.Uint64(self.accountNonce), nil
}

// The legacy-price fixture keeps the signed fee reservation deterministic.
func (self *stDepositCustodyRpc) GasPrice(context.Context) (*hexutil.Big, error) {
	price := hexutil.Big(*big.NewInt(10))
	return &price, nil
}

// Pre-London fixtures do not substitute a tip quote for their real gas price.
func (self *stDepositCustodyRpc) MaxPriorityFeePerGas(context.Context) (*hexutil.Big, error) {
	return nil, errors.New("tip method unavailable")
}

// The estimator is reached only after actual custody admission.
func (self *stDepositCustodyRpc) EstimateGas(context.Context, map[string]any) (hexutil.Uint64, error) {
	return hexutil.Uint64(50_000), nil
}

// Inclusion changes both stake positions once, then exposes a canonical receipt.
// A lost send reply never removes that receipt or advances either nonce twice.
func (self *stDepositCustodyRpc) SendRawTransaction(_ context.Context, raw hexutil.Bytes) (common.Hash, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	self.sent = append(self.sent, &tx)
	if self.receiptKVs[tx.Hash()] != nil {
		return tx.Hash(), nil
	}
	from, err := types.Sender(types.LatestSignerForChainID(new(big.Int).SetUint64(self.cfg.ChainId)), &tx)
	if err != nil || from != crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey) || tx.To() == nil || tx.Nonce() != self.accountNonce || tx.Value().Sign() != 0 {
		return common.Hash{}, errors.New("wrong fixture signing origin or account nonce")
	}
	status := uint64(types.ReceiptStatusSuccessful)
	if *tx.To() == stStakingPrecompileAddress && len(tx.Data()) == 164 {
		amount := new(big.Int).SetBytes(tx.Data()[132:164])
		self.source.Sub(self.source, amount)
		self.staged.Add(self.staged, new(big.Int).Sub(amount, big.NewInt(self.loss)))
	} else if *tx.To() == self.cfg.ContractAddress && len(tx.Data()) == 132 {
		amount := new(big.Int).SetBytes(tx.Data()[36:68])
		deadline := new(big.Int).SetBytes(tx.Data()[100:132])
		if self.fault == "deposit_revert" || !deadline.IsUint64() || deadline.Uint64() < self.boundary.Block+1 {
			status = types.ReceiptStatusFailed
		} else if new(big.Int).SetBytes(tx.Data()[68:100]).Cmp(self.depositNonce) != 0 {
			return common.Hash{}, errors.New("wrong fixture deposit nonce")
		} else {
			self.staged.Sub(self.staged, new(big.Int).Add(amount, big.NewInt(2)))
			self.deposited.Add(self.deposited, amount)
			self.depositNonce.Add(self.depositNonce, big.NewInt(1))
		}
	} else if *tx.To() == from && len(tx.Data()) == 0 && tx.Gas() == 21_000 {
		// A real same-nonce cancellation consumes gas/account nonce only.
	} else {
		return common.Hash{}, errors.New("unexpected deposit execution")
	}
	self.accountNonce++
	self.boundary.Block++
	self.boundary.Hash = [32]byte(stTransactionReconcileBlockHash(self.boundary.Block))
	self.receiptKVs[tx.Hash()] = &types.Receipt{Type: tx.Type(), TxHash: tx.Hash(), BlockHash: common.Hash(self.boundary.Hash), BlockNumber: new(big.Int).SetUint64(self.boundary.Block), Status: status, GasUsed: tx.Gas(), CumulativeGasUsed: tx.Gas(), EffectiveGasPrice: tx.GasPrice(), Logs: []*types.Log{}}
	if self.lostReply {
		self.lostReply = false
		return common.Hash{}, errors.New("deterministic lost reply after inclusion")
	}
	return tx.Hash(), nil
}

// Missing receipts remain missing instead of being fabricated from nonce state.
func (self *stDepositCustodyRpc) GetTransactionReceipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.receiptKVs[hash], nil
}

// Each operator owns different test-only secrets and a different isolated hotkey.
func newStDepositCustodyFixture(t testing.TB, noId uint64) (*CoreStClient, *ethclient.Client, *stDepositCustodyRpc) {
	t.Helper()
	keys := make([]*ecdsa.PrivateKey, 3)
	for index := range keys {
		key, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 10*noId+uint64(index)+1))
		if err != nil {
			t.Fatal(err)
		}
		keys[index] = key
	}
	cfg := &StConfig{Profile: "testnet", Enabled: true, ChainId: 945, GenesisHash: [32]byte{1}, Netuid: 521, NoId: noId, ContractAddress: common.Address{2}, SettlementVault: common.Address{3}, ReserveSink: common.Address{4}, DeploymentId: "deposit-custody-fixture", PolicyHash: [32]byte{5}, DepositHotkey: [32]byte{byte(noId), 6}, DepositKey: keys[0], RootKey: keys[1], ArtifactKey: keys[2], DepositEpochCapRao: 1_000, DeployBlock: 10}
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), NoID: cfg.NoId, Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: cfg.PolicyHash}
	fixture := &stDepositCustodyRpc{stClientKeyHistoryRPCFixture: stClientKeyHistoryRPCFixture{domain: domain, boundary: protocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte(stTransactionReconcileBlockHash(100)), Epoch: 7}, root: crypto.PubkeyToAddress(cfg.RootKey.PublicKey)}, cfg: cfg, staged: new(big.Int), source: big.NewInt(10_000), deposited: new(big.Int), depositNonce: big.NewInt(3), accountNonce: 7, minimumTao: 1, loss: 1, receiptKVs: map[common.Hash]*types.Receipt{}}
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("eth", fixture); err != nil {
		t.Fatal(err)
	}
	if err := rpcServer.RegisterName("chain", fixture); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(rpcServer)
	rpcClient, err := ethclient.Dial(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RpcUrls = []string{httpServer.URL}
	client := &CoreStClient{cfg: cfg, coordinator: stabi.NewSTCoordinator(), vault: stabi.NewSTSettlementVault(), clients: map[string]*ethclient.Client{httpServer.URL: rpcClient}}
	t.Cleanup(func() { rpcClient.Close(); httpServer.Close(); rpcServer.Stop() })
	return client, rpcClient, fixture
}

// Independent operator keys, hotkeys and coordinator nonce observations remain
// separate even when both operators share the exact deployment graph.
func TestStDepositCustodyBindsTwoOperatorSecretsAtCanonicalBoundary(t *testing.T) {
	var signers []common.Address
	for noId := uint64(1); noId <= 2; noId++ {
		client, rpcClient, fixture := newStDepositCustodyFixture(t, noId)
		epoch := uint64(7)
		state, err := client.readDepositCustody(t.Context(), rpcClient, &epoch)
		if err != nil {
			t.Fatal(err)
		}
		if state.nonce.Int64() != 3 || state.deadline != 1_999 || state.staged.Sign() != 0 || state.source.Int64() != 10_000 || state.boundary != fixture.boundary {
			t.Fatal("deposit custody mixed economic boundaries")
		}
		if err := state.validatePrincipal(client.cfg, big.NewInt(100)); err != nil {
			t.Fatal(err)
		}
		signers = append(signers, crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey))
	}
	if signers[0] == signers[1] {
		t.Fatal("two operators reused one deposit key")
	}
}

// Every conflict is an actual changed RPC response; no trusted verdict is
// injected and no fee estimate, nonce reservation or signing can occur.
func TestStDepositCustodyRejectsWrongOperatorGraphAndAuthority(t *testing.T) {
	client, rpcClient, fixture := newStDepositCustodyFixture(t, 1)
	epoch := uint64(7)
	for _, fault := range []string{"deposit-signer", "deposit-hotkey", "root-signer", "self-coldkey", "reserve", "owner-key", "rounding", "paused", "trailing"} {
		fixture.custodyFault = fault
		if _, err := client.readDepositCustody(t.Context(), rpcClient, &epoch); err == nil {
			t.Fatalf("custody fault %s admitted", fault)
		}
	}
	fixture.custodyFault = ""
	for _, fault := range []string{"native-wrong", "native-null", "native-unavailable", "chain", "canonical", "policy", "companion", "inactive"} {
		fixture.fault = fault
		if _, err := client.readDepositCustody(t.Context(), rpcClient, &epoch); err == nil {
			t.Fatalf("authority fault %s admitted", fault)
		}
	}
	fixture.fault = ""
	epoch++
	if _, err := client.readDepositCustody(t.Context(), rpcClient, &epoch); err == nil {
		t.Fatal("another epoch admitted")
	}
}

// Native dust is refused without increasing principal or replenishing a cap.
func TestStDepositCustodyRejectsDustAndKeepsPrincipalCeilings(t *testing.T) {
	client, rpcClient, fixture := newStDepositCustodyFixture(t, 1)
	fixture.minimumTao = 10
	fixture.staged.SetInt64(101)
	state, err := client.readDepositCustody(t.Context(), rpcClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.validatePrincipal(client.cfg, big.NewInt(100)); err != nil {
		t.Fatal(err)
	}
	funding, err := stDepositFundingAmount(big.NewInt(100), state.staged)
	if err != nil || funding.Int64() != 2 {
		t.Fatalf("dust funding = %v %v", funding, err)
	}
	if err := state.validateTransfer(funding); !errors.Is(err, errStDepositBelowRuntimeMinimum) {
		t.Fatalf("dust transfer admission = %v", err)
	}
	if err := state.validatePrincipal(client.cfg, big.NewInt(1_001)); err == nil {
		t.Fatal("configured principal cap exceeded")
	}
	state.campaignReserved.SetInt64(9_901)
	if err := state.validatePrincipal(client.cfg, big.NewInt(100)); err == nil {
		t.Fatal("campaign principal cap exceeded")
	}
	state.campaignReserved.SetInt64(0)
	state.deposited.SetInt64(100)
	if err := state.validatePrincipal(client.cfg, big.NewInt(100)); !errors.Is(err, errStDepositAlreadyCredited) {
		t.Fatal("exact existing demand was not distinguished")
	}
	if err := state.validatePrincipal(client.cfg, big.NewInt(99)); err == nil || errors.Is(err, errStDepositAlreadyCredited) {
		t.Fatal("different existing demand admitted")
	}
	state.deposited.SetInt64(0)
	state.minimumTao = 102
	if err := state.validatePrincipal(client.cfg, big.NewInt(100)); !errors.Is(err, errStDepositBelowRuntimeMinimum) {
		t.Fatal("reserve second-hop rounding crossed the native floor")
	}
}

// Retained calldata and account nonce survive signer/epoch/nonce changes. A
// cancellation can still retire custody without executing the obsolete deposit.
func TestStDepositAttemptRejectsChangedAuthorityWithoutRewritingIntent(t *testing.T) {
	client, rpcClient, fixture := newStDepositCustodyFixture(t, 1)
	data, err := stPackTransferStake(ss58.EvmMirrorPubkey(client.cfg.ContractAddress), client.cfg.DepositHotkey, client.cfg.Netuid, big.NewInt(103))
	if err != nil {
		t.Fatal(err)
	}
	logicalKey, err := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
	if err != nil {
		t.Fatal(err)
	}
	intent := &model.StTransactionIntent{LogicalKey: logicalKey, DeploymentKey: client.cfg.DeploymentKey(), ChainId: client.cfg.ChainId, GenesisHash: hexutil.Encode(client.cfg.GenesisHash[:]), FromAddress: strings.ToLower(crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey).Hex()), ToAddress: strings.ToLower(stStakingPrecompileAddress.Hex()), Calldata: data, CalldataHash: crypto.Keccak256Hash(data).Hex(), Nonce: 7}
	if err := client.validateDepositAttempt(t.Context(), rpcClient, intent, model.StTxAttemptExecution); err != nil {
		t.Fatal(err)
	}
	fixture.depositNonce.SetInt64(4)
	if _, err := client.buildTransactionAttempt(t.Context(), rpcClient, client.cfg.DepositKey, intent, nil, model.StTxAttemptExecution); err == nil {
		t.Fatal("changed deposit nonce signed a retained funding intent")
	}
	fixture.depositNonce.SetInt64(3)
	fixture.custodyFault = "deposit-signer"
	if _, err := client.buildTransactionAttempt(t.Context(), rpcClient, client.cfg.DepositKey, intent, nil, model.StTxAttemptExecution); err == nil {
		t.Fatal("changed operator signed a retained funding intent")
	}
	if err := client.validateDepositAttempt(t.Context(), rpcClient, intent, model.StTxAttemptCancellation); err != nil {
		t.Fatal("obsolete deposit prevented cancellation")
	}
	if intent.Nonce != 7 || !bytes.Equal(intent.Calldata, data) || intent.LogicalKey != logicalKey {
		t.Fatal("admission failure rewrote original intent")
	}
}

// Role-key mistakes are rejected before any native or Evm observation, even
// for an in-memory configuration that bypassed the ordinary vault parser.
func TestStDepositCustodyRejectsSharedRoleKeysBeforeRpc(t *testing.T) {
	client, rpcClient, fixture := newStDepositCustodyFixture(t, 1)
	original := client.cfg.DepositKey
	for _, other := range []*ecdsa.PrivateKey{client.cfg.RootKey, client.cfg.ArtifactKey, nil} {
		client.cfg.DepositKey = other
		if _, err := client.readDepositCustody(t.Context(), rpcClient, nil); err == nil {
			t.Fatal("missing or shared deposit role admitted")
		}
	}
	client.cfg.DepositKey = original
	if fixture.viewCalls != 0 {
		t.Fatal("role-key validation consulted chain before rejecting local custody")
	}
	if _, err := client.DepositCredit(t.Context(), 7, 2, big.NewInt(100)); err == nil {
		t.Fatal("operator one accepted operator two deposit request")
	}
}

// Existing revert recovery preserves every original economic field. A later
// changed deficit is not an authorization to mutate retained transfer bytes.
func TestStDepositRetainedCalldataRejectsChangedRetryAmount(t *testing.T) {
	client, _, _ := newStDepositCustodyFixture(t, 1)
	data, err := stPackTransferStake(ss58.EvmMirrorPubkey(client.cfg.ContractAddress), client.cfg.DepositHotkey, client.cfg.Netuid, big.NewInt(103))
	if err != nil {
		t.Fatal(err)
	}
	logicalKey, err := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
	if err != nil {
		t.Fatal(err)
	}
	prior := &model.StTransactionIntent{LogicalKey: logicalKey, DeploymentKey: client.cfg.DeploymentKey(), ChainId: client.cfg.ChainId, GenesisHash: hexutil.Encode(client.cfg.GenesisHash[:]), FromAddress: crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey).Hex(), ToAddress: stStakingPrecompileAddress.Hex(), Calldata: bytes.Clone(data), CalldataHash: crypto.Keccak256Hash(data).Hex(), Nonce: 7, Status: model.StTxReverted}
	if err := stDepositRetainedCalldata(client.cfg, "deposit-fund:7:1:3", stStakingPrecompileAddress, data, prior); err != nil {
		t.Fatal(err)
	}
	data[len(data)-1]--
	if err := stDepositRetainedCalldata(client.cfg, "deposit-fund:7:1:3", stStakingPrecompileAddress, data, prior); err == nil {
		t.Fatal("different deficit overwrote original reverted intent")
	}
	if prior.Nonce != 7 || new(big.Int).SetBytes(prior.Calldata[132:164]).Int64() != 103 {
		t.Fatal("retry validation rewrote retained execution")
	}
}

// A restarted worker resolves the existing account nonce before computing a
// remaining deficit. Its original intent becomes finalized without a successor.
func TestStDepositCustodyReconcilesPreparedFundingBeforeNewSizing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		ctx := context.Background()
		data, err := stPackTransferStake(ss58.EvmMirrorPubkey(client.cfg.ContractAddress), client.cfg.DepositHotkey, client.cfg.Netuid, big.NewInt(103))
		if err != nil {
			t.Fatal(err)
		}
		logicalKey, err := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
		if err != nil {
			t.Fatal(err)
		}
		prior := model.ReserveStTransactionIntent(ctx, logicalKey, client.cfg.Profile, client.cfg.DeploymentId, client.cfg.DeploymentKey(), client.cfg.ChainId, hexutil.Encode(client.cfg.GenesisHash[:]), strings.ToLower(crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey).Hex()), strings.ToLower(stStakingPrecompileAddress.Hex()), strings.ToLower(crypto.Keccak256Hash(data).Hex()), data, 7)
		if _, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil {
			t.Fatal(err)
		}
		stored := model.GetStTransactionIntent(ctx, logicalKey)
		if stored == nil || stored.IntentId != prior.IntentId || stored.Nonce != 7 || stored.Generation != prior.Generation || stored.Status != model.StTxFinalized || !bytes.Equal(stored.Calldata, data) {
			t.Fatal("recovery replaced or failed to finalize original funding intent")
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 1 || fixture.accountNonce != 8 || fixture.staged.Int64() != 102 {
			t.Fatal("recovery funded the same deficit again")
		}
	})
}

// The actual server staging/credit entries send only one exact transfer and
// one principal deposit, preserving two independent nonce domains and fees.
func TestStDepositCustodyStagesRoundingOnceAndCreditsExactPrincipal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		ctx := context.Background()
		first, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100))
		if err != nil || first == "" {
			t.Fatalf("stage = %q %v", first, err)
		}
		if again, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil || again != "" {
			t.Fatalf("staged retry = %q %v", again, err)
		}
		credit, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100))
		if err != nil || credit == "" {
			t.Fatalf("credit = %q %v", credit, err)
		}
		if _, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); err != nil {
			t.Fatal(err)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 2 || fixture.source.Int64() != 9_897 || fixture.staged.Sign() != 0 || fixture.deposited.Int64() != 100 || fixture.accountNonce != 9 || fixture.depositNonce.Int64() != 4 {
			t.Fatal("deposit repeated funding or misattributed principal")
		}
		for index, tx := range fixture.sent {
			if tx.Nonce() != uint64(7+index) || tx.GasPrice().Int64() != 10 || tx.Gas() != 60_000 {
				t.Fatal("deposit account nonce or fee reservation changed")
			}
		}
	})
}

// A deterministic transport failure after inclusion is reconciled immediately;
// a later worker never funds the already changed native stake position again.
func TestStDepositCustodyLostFundingReplyDoesNotRepeatTransfer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		fixture.lostReply = true
		if _, err := client.stageDepositPrincipal(context.Background(), 7, big.NewInt(100)); err != nil {
			t.Fatal(err)
		}
		if _, err := client.stageDepositPrincipal(context.Background(), 7, big.NewInt(100)); err != nil {
			t.Fatal(err)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 1 || fixture.accountNonce != 8 || fixture.source.Int64() != 9_897 || fixture.staged.Int64() != 102 {
			t.Fatal("lost reply repeated an already included transfer")
		}
	})
}

// A historical success with too little actual staging remains visible and
// cannot be repaired by silently rewriting its amount or reserving a new nonce.
func TestStDepositCustodyRetainsUnderfundedSuccessWithoutAnotherTransfer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		fixture.loss = 2
		if _, err := client.stageDepositPrincipal(context.Background(), 7, big.NewInt(100)); err != nil {
			t.Fatal(err)
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			t.Fatal("short native staging reached a deposit signature")
		}
		if _, err := client.stageDepositPrincipal(context.Background(), 7, big.NewInt(100)); err == nil || !strings.Contains(err.Error(), "original intent preserved") {
			t.Fatalf("underfunded retry = %v", err)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 1 || fixture.accountNonce != 8 || fixture.deposited.Sign() != 0 {
			t.Fatal("underfunding allocated another execution nonce")
		}
	})
}
