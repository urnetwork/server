package controller

// st_ledger_v2_send_test.go — the v2 writes through the real durable
// transaction engine against an RPC fixture: SV-7 and the front-running rules
// of SPEC A6.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ledgerV2RPC serves the engine's RPC surface plus eth_call. Receipts are
// final at once. A call whose calldata is in reverts reverts with that error
// ("" for a revert without data); estimates never revert.
type ledgerV2RPC struct {
	t              testing.TB
	lock           sync.Mutex
	finalizedBlock uint64
	nonce          uint64
	receipts       map[string]*types.Receipt
	sent           []*types.Transaction
	events         []string
	reverts        map[string]string
	results        map[string][]byte
	// latestBlock answers "latest" when set; otherwise the finalized block does.
	// latestUnavailable fails every read of "latest".
	latestBlock       uint64
	latestUnavailable bool
	// estimates answers eth_estimateGas for a calldata; any other calldata
	// estimates 50,000. A transaction whose calldata is in needs runs out of
	// gas when its limit is below that: its receipt is failed. The next
	// withheld transactions are accepted and never included.
	estimates map[string]uint64
	needs     map[string]uint64
	withheld  int
}

func newLedgerV2RPC(t testing.TB) *ledgerV2RPC {
	return &ledgerV2RPC{t: t, finalizedBlock: 100, nonce: 7, receipts: map[string]*types.Receipt{}, reverts: map[string]string{}, results: map[string][]byte{},
		estimates: map[string]uint64{}, needs: map[string]uint64{}}
}

func ledgerV2ErrorData(name string) string {
	if name == "" {
		return ""
	}
	return hexutil.Encode(crypto.Keccak256([]byte(name + "()"))[:4])
}

func (self *ledgerV2RPC) revert(response map[string]any, name string) {
	revert := map[string]any{"code": 3, "message": "execution reverted"}
	if data := ledgerV2ErrorData(name); data != "" {
		revert["data"] = data
	}
	response["error"] = revert
}

func (self *ledgerV2RPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read RPC request: %v", err)
		return
	}
	var calls []stEpochRPCRequest
	batch := strings.HasPrefix(strings.TrimSpace(string(body)), "[")
	if batch {
		err = json.Unmarshal(body, &calls)
	} else {
		var call stEpochRPCRequest
		err = json.Unmarshal(body, &call)
		calls = append(calls, call)
	}
	if err != nil {
		self.t.Errorf("decode RPC request %s: %v", body, err)
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getBlockByNumber":
			var selector string
			json.Unmarshal(call.Params[0], &selector)
			block := self.finalizedBlock
			if selector == "latest" && self.latestUnavailable {
				response["error"] = map[string]any{"code": -32000, "message": "latest block unavailable"}
			} else if selector == "latest" && self.latestBlock != 0 {
				block = self.latestBlock
			} else if selector != "latest" && selector != "finalized" {
				block, _ = hexutil.DecodeUint64(selector)
			}
			result = stTransactionReconcileBlock(block)
		case "eth_getTransactionCount":
			result = hexutil.EncodeUint64(self.nonce)
		case "eth_gasPrice":
			result = "0x64"
		case "eth_maxPriorityFeePerGas":
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		case "eth_call", "eth_estimateGas":
			var message struct {
				Data  hexutil.Bytes `json:"data"`
				Input hexutil.Bytes `json:"input"`
			}
			json.Unmarshal(call.Params[0], &message)
			data := append(message.Data, message.Input...)
			if call.Method == "eth_estimateGas" {
				estimate := uint64(50_000)
				if known, ok := self.estimates[string(data)]; ok {
					estimate = known
				}
				result = hexutil.EncodeUint64(estimate)
				break
			}
			self.events = append(self.events, "call "+hex.EncodeToString(data[:min(4, len(data))]))
			if name, ok := self.reverts[string(data)]; ok {
				self.revert(response, name)
			} else {
				result = hexutil.Encode(self.results[string(data)])
			}
		case "eth_sendRawTransaction":
			var rawHex string
			json.Unmarshal(call.Params[0], &rawHex)
			var transaction types.Transaction
			if err := transaction.UnmarshalBinary(hexutil.MustDecode(rawHex)); err != nil {
				self.t.Errorf("decode signed transaction: %v", err)
				return
			}
			hash := strings.ToLower(transaction.Hash().Hex())
			self.sent = append(self.sent, &transaction)
			self.events = append(self.events, "send "+hex.EncodeToString(transaction.Data()[:min(4, len(transaction.Data()))]))
			result = hash
			if 0 < self.withheld {
				self.withheld--
				break
			}
			status := types.ReceiptStatusSuccessful
			if transaction.Gas() < self.needs[string(transaction.Data())] {
				status = types.ReceiptStatusFailed
			}
			self.receipts[hash] = &types.Receipt{
				Type: transaction.Type(), TxHash: transaction.Hash(), BlockHash: stTransactionReconcileBlockHash(self.finalizedBlock - 1),
				BlockNumber: new(big.Int).SetUint64(self.finalizedBlock - 1), Status: status,
				CumulativeGasUsed: transaction.Gas(), GasUsed: transaction.Gas(), EffectiveGasPrice: transaction.GasPrice(), Logs: []*types.Log{},
			}
		case "eth_getTransactionReceipt":
			var hash string
			json.Unmarshal(call.Params[0], &hash)
			result = self.receipts[strings.ToLower(hash)]
		default:
			self.t.Errorf("unexpected RPC method %s", call.Method)
			return
		}
		if _, failed := response["error"]; !failed {
			response["result"] = result
		}
		responses[index] = response
	}
	var response any = responses
	if !batch {
		response = responses[0]
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(response)
}

func (self *ledgerV2RPC) takeEvents() []string {
	self.lock.Lock()
	defer self.lock.Unlock()
	events := self.events
	self.events = nil
	return events
}

func newLedgerV2RPCClient(t testing.TB, fixture http.Handler) (*CoreStClient, *ethclient.Client, *StConfig) {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	cfg := ledgerV2TestConfig()
	cfg.RootKey = key
	listener := httptest.NewServer(fixture)
	cfg.RpcUrls = []string{listener.URL}
	rpcClient, err := ethclient.Dial(listener.URL)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rpcClient.Close()
		listener.Close()
	})
	return &CoreStClient{cfg: cfg, clients: map[string]*ethclient.Client{listener.URL: rpcClient}}, rpcClient, cfg
}

func ledgerV2TestAnswer() *StLedgerV2Answer {
	return &StLedgerV2Answer{Index: 0, Account: merkle.LedgerAccount{Coldkey: ledgerV2Coldkey(1), Cumulative: *uint256.NewInt(500)}}
}

func requireLedgerV2Intent(t testing.TB, ctx context.Context, cfg *StConfig, operation string, status string, tx *types.Transaction) {
	t.Helper()
	logicalKey, err := stTransactionLogicalKey(cfg, operation)
	if err != nil {
		t.Fatal(err)
	}
	intent := model.GetStTransactionIntent(ctx, logicalKey)
	if intent == nil || intent.Status != status {
		t.Fatalf("%s intent %+v, want %s", operation, intent, status)
	}
	if tx != nil && (intent.CurrentTxHash == nil || !strings.EqualFold(*intent.CurrentTxHash, tx.Hash().Hex()) || intent.Nonce != tx.Nonce()) {
		t.Fatalf("%s intent %+v does not own transaction %s", operation, intent, tx.Hash())
	}
}

// SV-7: consecutive epochs' finalizes and consecutive roots' answers each
// broadcast their own transaction, under logical keys that name the epoch or
// the commit seq, and an answer's key also names its finalized root, so the
// same account index demanded against two roots is answered twice; every new
// operation is preflighted with eth_call before it is sent; a repeated
// operation broadcasts nothing new.
func TestStLedgerV2SendsEachOperationOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		answer := ledgerV2TestAnswer()
		older := *answer
		older.RootIndex = 1
		finalize := hex.EncodeToString(stLedgerV2SettlementAbi.PackFinalizeDistribution(big.NewInt(1))[:4])
		checkPath := hex.EncodeToString(stLedgerV2PackCheckPath(1, answer)[:4])

		hashes := map[string]string{}
		for _, send := range []struct {
			operation string
			call      func() (string, error)
		}{
			{"v2finalize:5:1", func() (string, error) { return client.LedgerV2Finalize(ctx, 5, 1) }},
			{"v2finalize:6:1", func() (string, error) { return client.LedgerV2Finalize(ctx, 6, 1) }},
			{"v2answer:1:3:0:0", func() (string, error) { return client.LedgerV2Answer(ctx, 1, 3, answer) }},
			{"v2answer:1:3:1:0", func() (string, error) { return client.LedgerV2Answer(ctx, 1, 3, &older) }},
			{"v2answer:1:4:0:0", func() (string, error) { return client.LedgerV2Answer(ctx, 1, 4, answer) }},
		} {
			hash, err := send.call()
			if err != nil {
				t.Fatalf("%s: %v", send.operation, err)
			}
			hashes[send.operation] = hash
		}
		if events := fixture.takeEvents(); fmt.Sprint(events) != fmt.Sprint([]string{"call " + finalize, "send " + finalize, "call " + finalize, "send " + finalize, "call " + checkPath, "send " + checkPath, "call " + checkPath, "send " + checkPath, "call " + checkPath, "send " + checkPath}) {
			t.Fatalf("events %v", events)
		}
		if len(fixture.sent) != 5 {
			t.Fatalf("sent %d transactions", len(fixture.sent))
		}
		for i, want := range []struct {
			operation string
			to        common.Address
			data      []byte
		}{
			{"v2finalize:5:1", cfg.SettlementV2, stLedgerV2SettlementAbi.PackFinalizeDistribution(big.NewInt(1))},
			{"v2finalize:6:1", cfg.SettlementV2, stLedgerV2SettlementAbi.PackFinalizeDistribution(big.NewInt(1))},
			{"v2answer:1:3:0:0", cfg.VaultV2, stLedgerV2PackCheckPath(1, answer)},
			{"v2answer:1:3:1:0", cfg.VaultV2, stLedgerV2PackCheckPath(1, &older)},
			{"v2answer:1:4:0:0", cfg.VaultV2, stLedgerV2PackCheckPath(1, answer)},
		} {
			tx := fixture.sent[i]
			if tx.Nonce() != uint64(7+i) || *tx.To() != want.to || string(tx.Data()) != string(want.data) || tx.Gas() != 60_000 || !strings.EqualFold(hashes[want.operation], tx.Hash().Hex()) {
				t.Fatalf("%s sent nonce %d to %s gas %d data %x", want.operation, tx.Nonce(), tx.To(), tx.Gas(), tx.Data())
			}
			requireLedgerV2Intent(t, ctx, cfg, want.operation, model.StTxFinalized, tx)
		}

		if hash, err := client.LedgerV2Finalize(ctx, 5, 1); err != nil || hash != hashes["v2finalize:5:1"] {
			t.Fatalf("repeated finalize %s, %v", hash, err)
		}
		if hash, err := client.LedgerV2Answer(ctx, 1, 3, answer); err != nil || hash != hashes["v2answer:1:3:0:0"] {
			t.Fatalf("repeated answer %s, %v", hash, err)
		}
		if events := fixture.takeEvents(); len(events) != 0 || len(fixture.sent) != 5 {
			t.Fatalf("a repeated operation was preflighted or sent: %v", events)
		}

		closeHash, err := client.LedgerV2Close(ctx, 7, 1)
		if err != nil {
			t.Fatal(err)
		}
		commitment := &StLedgerV2Commitment{Root: [32]byte{1}, Total: *uint256.NewInt(1000), LeafCount: 2, LedgerHash: [32]byte{2}}
		commitHash, err := client.LedgerV2Commit(ctx, 7, 1, 0, commitment)
		if err != nil {
			t.Fatal(err)
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2close:7:1", model.StTxFinalized, fixture.sent[5])
		requireLedgerV2Intent(t, ctx, cfg, "v2commit:7:1", model.StTxFinalized, fixture.sent[6])
		if !strings.EqualFold(closeHash, fixture.sent[5].Hash().Hex()) || !strings.EqualFold(commitHash, fixture.sent[6].Hash().Hex()) {
			t.Fatal("close or commit returned another transaction")
		}
		// A retried commit of the epoch with another root count re-drives the
		// epoch's one stored commit.
		if hash, err := client.LedgerV2Commit(ctx, 7, 1, 1, commitment); err != nil || hash != commitHash || len(fixture.sent) != 7 {
			t.Fatalf("second commit of epoch 7: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
	})
}

// SPEC A6: a v2 call that someone else already did reverts at its eth_call
// preflight and takes no nonce. An intent left without a signed attempt whose
// call now reverts with a spent error, an answer naming a root vault2 does not
// hold (UnknownRoot) included, is retired by the account reconcile; a
// transient or unnamed revert leaves it to execute. A commit whose intent was
// spent is never sent again.
func TestStLedgerV2SpentIntentsAreRetired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, rpcClient, cfg := newLedgerV2RPCClient(t, fixture)
		from := crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
		answer := ledgerV2TestAnswer()
		checkPath, finalize := stLedgerV2PackCheckPath(1, answer), stLedgerV2SettlementAbi.PackFinalizeDistribution(big.NewInt(1))
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(9), big.NewInt(1))

		fixture.reverts[string(checkPath)] = "NoFault"
		if _, err := client.LedgerV2Answer(ctx, 1, 5, answer); err == nil || !strings.Contains(err.Error(), "v2answer:1:5:0:0 preflight reverts NoFault") {
			t.Fatalf("front-run answer: %v", err)
		}
		fixture.reverts[string(finalize)] = "NoPendingRoot"
		if _, err := client.LedgerV2Finalize(ctx, 8, 1); err == nil || !strings.Contains(err.Error(), "v2finalize:8:1 preflight reverts NoPendingRoot") {
			t.Fatalf("front-run finalize: %v", err)
		}
		fixture.reverts[string(closeCall)] = ""
		if _, err := client.LedgerV2Close(ctx, 9, 1); err == nil || !strings.Contains(err.Error(), "v2close:9:1 preflight failed: execution reverted") {
			t.Fatalf("unnamed revert: %v", err)
		}
		for _, operation := range []string{"v2answer:1:5:0:0", "v2finalize:8:1", "v2close:9:1"} {
			logicalKey, _ := stTransactionLogicalKey(cfg, operation)
			if intent := model.GetStTransactionIntent(ctx, logicalKey); intent != nil {
				t.Fatalf("a refused preflight reserved %+v", intent)
			}
		}
		if len(fixture.sent) != 0 {
			t.Fatalf("sent %d transactions", len(fixture.sent))
		}

		// Intents reserved before a front-run, each without a signed attempt.
		genesis := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
		reserve := func(operation string, to common.Address, calldata []byte, nonce uint64) *model.StTransactionIntent {
			logicalKey, err := stTransactionLogicalKey(cfg, operation)
			if err != nil {
				t.Fatal(err)
			}
			return model.ReserveStTransactionIntent(ctx, logicalKey, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, genesis,
				strings.ToLower(from.Hex()), strings.ToLower(to.Hex()), strings.ToLower(crypto.Keccak256Hash(calldata).Hex()), calldata, nonce)
		}
		unknown := *answer
		unknown.RootIndex = 4
		spentAnswer := reserve("v2answer:1:6:0:0", cfg.VaultV2, checkPath, 7)
		spentFinalize := reserve("v2finalize:9:1", cfg.SettlementV2, finalize, 8)
		unknownRoot := reserve("v2answer:1:6:4:0", cfg.VaultV2, stLedgerV2PackCheckPath(1, &unknown), 9)
		paused := reserve("v2close:10:1", cfg.SettlementV2, stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(10), big.NewInt(1)), 10)
		unnamed := reserve("v2close:9:1", cfg.SettlementV2, closeCall, 11)
		fixture.reverts[string(unknownRoot.Calldata)] = "UnknownRoot"
		fixture.reverts[string(paused.Calldata)] = "SettlementPaused"
		if err := client.reconcileAccountIntents(ctx, rpcClient, cfg.RootKey, from); err != nil {
			t.Fatal(err)
		}
		if len(fixture.sent) != 5 {
			t.Fatalf("sent %d transactions", len(fixture.sent))
		}
		for i, intent := range []*model.StTransactionIntent{spentAnswer, spentFinalize, unknownRoot, paused, unnamed} {
			stored := model.GetStTransactionIntent(ctx, intent.LogicalKey)
			tx := fixture.sent[i]
			cancelled := i < 3
			if wantStatus := map[bool]string{true: model.StTxCanceled, false: model.StTxFinalized}[cancelled]; stored.Status != wantStatus || tx.Nonce() != intent.Nonce {
				t.Fatalf("intent %d status %s nonce %d", i, stored.Status, tx.Nonce())
			}
			if cancelled != (*tx.To() == from && len(tx.Data()) == 0 && tx.Gas() == 21_000) {
				t.Fatalf("intent %d sent to %s data %x gas %d", i, tx.To(), tx.Data(), tx.Gas())
			}
		}

		// A commit whose intent reverted or was retired is spent for its epoch.
		commitment := &StLedgerV2Commitment{Root: [32]byte{1}, Total: *uint256.NewInt(1000), LeafCount: 2, LedgerHash: [32]byte{2}}
		commitCall := stLedgerV2SettlementAbi.PackCommitDistribution(big.NewInt(11), big.NewInt(1), big.NewInt(0), stLedgerV2CommitmentArgument(commitment))
		fixture.reverts[string(commitCall)] = "StaleRootCount"
		reserve("v2commit:11:1", cfg.SettlementV2, commitCall, 12)
		if err := client.reconcileAccountIntents(ctx, rpcClient, cfg.RootKey, from); err != nil {
			t.Fatal(err)
		}
		if _, err := client.LedgerV2Commit(ctx, 11, 1, 1, commitment); !errors.Is(err, ErrStLedgerV2CommitSpent) {
			t.Fatalf("spent commit: %v", err)
		}
		if len(fixture.sent) != 6 {
			t.Fatalf("sent %d transactions", len(fixture.sent))
		}
	})
}

// The core client reads a pool's v2 state at one finalized block: the v1
// pause, vault2's pool, funding, roots, pending root, commit spacing and
// demands, and STSettlement2's approvals and quorum; each refused or
// out-of-range read is an error.
// SV-1 (chain reads)
func TestStLedgerV2ClientReadsChainState(t *testing.T) {
	ctx := context.Background()
	fixture := newLedgerV2RPC(t)
	client, _, _ := newLedgerV2RPCClient(t, fixture)
	client.coordinator = stabi.NewSTCoordinator()
	vault, settlement, coordinator := stLedgerV2VaultAbi, stLedgerV2SettlementAbi, client.coordinator
	parse := func(meta *bind.MetaData) *abi.ABI {
		parsed, err := meta.ParseABI()
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	vaultAbi, settlementAbi, coordinatorAbi := parse(&stabi.STSettlementVault2MetaData), parse(&stabi.STSettlement2MetaData), parse(&stabi.STCoordinatorMetaData)
	result := func(calldata []byte, parsed *abi.ABI, method string, values ...any) {
		out, err := parsed.Methods[method].Outputs.Pack(values...)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		fixture.results[string(calldata)] = out
	}
	no := big.NewInt(1)
	result(coordinator.PackPaused(), coordinatorAbi, "paused", true)
	result(vault.PackPools(no), vaultAbi, "pools", stabi.ISTSettlementVault2Pool{Hotkey: [32]byte{9}, FirstEpoch: 5, Registered: true})
	result(vault.PackCoordinator(), vaultAbi, "coordinator", client.cfg.SettlementV2)
	result(vault.PackCHALLENGEBLOCKS(), vaultAbi, "CHALLENGE_BLOCKS", uint64(14_400))
	result(vault.PackLastCommitBlock(no), vaultAbi, "lastCommitBlock", uint64(302_410))
	result(vault.PackLastFundedEpoch(no), vaultAbi, "lastFundedEpoch", big.NewInt(6))
	result(vault.PackRootCount(no), vaultAbi, "rootCount", big.NewInt(1))
	result(settlement.PackApprovalCount(no), settlementAbi, "approvalCount", big.NewInt(1))
	result(settlement.PackQuorum(), settlementAbi, "quorum", big.NewInt(1))
	result(vault.PackRoots(no, big.NewInt(0)), vaultAbi, "roots", stabi.ISTSettlementVault2Root{Root: [32]byte{1}, Total: big.NewInt(1000), LeafCount: 2, Epoch: 5, LedgerHash: [32]byte{2}})
	result(vault.PackPending(no), vaultAbi, "pending", stabi.ISTSettlementVault2Pending{Seq: 2, Root: [32]byte{3}, Total: big.NewInt(4000), LeafCount: 3, Epoch: 6, LedgerHash: [32]byte{4}, CommittedBlock: 302_410, OpenDemands: 1})
	result(vault.PackFundedThrough(no, big.NewInt(6)), vaultAbi, "fundedThrough", big.NewInt(4000))
	result(vault.PackDemanded(no, 2, big.NewInt(0), big.NewInt(1)), vaultAbi, "demanded", StLedgerV2DemandOpen)
	result(coordinator.PackEpochEndBlock(big.NewInt(6)), coordinatorAbi, "epochEndBlock", big.NewInt(352_800))
	result(coordinator.PackPolicyAt(big.NewInt(6)), coordinatorAbi, "policyAt", stabi.STCoordinatorPolicySnapshot{EpochBlocks: 50_400, RootCommitWindowBlocks: 1_200, EpochDepositCapRao: big.NewInt(0), CampaignDepositCapRao: big.NewInt(0)})

	state, err := client.LedgerV2State(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := &StLedgerV2State{HeadBlock: 100, Paused: true, Registered: true, FirstEpoch: 5, LastFundedEpoch: 6, RootCount: 1,
		Latest:          &StLedgerV2FinalizedRoot{Index: 0, Root: [32]byte{1}, Total: *uint256.NewInt(1000), LeafCount: 2, Epoch: 5, LedgerHash: [32]byte{2}},
		Pending:         &StLedgerV2Pending{Seq: 2, Root: [32]byte{3}, Total: *uint256.NewInt(4000), LeafCount: 3, Epoch: 6, LedgerHash: [32]byte{4}, CommittedBlock: 302_410, OpenDemands: 1},
		LastCommitBlock: 302_410, Approvals: 1, Quorum: 1, ChallengeBlocks: 14_400}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("state %+v\nwant  %+v", state, want)
	}
	if registered, first, err := client.LedgerV2Pool(ctx, 1); err != nil || !registered || first != 5 {
		t.Fatalf("pool %t %d %v", registered, first, err)
	}
	if bound, err := client.LedgerV2Coordinator(ctx); err != nil || bound != client.cfg.SettlementV2 || bound == (common.Address{}) {
		t.Fatalf("coordinator %s %v", bound, err)
	}
	if funded, err := client.LedgerV2FundedThrough(ctx, 1, 6); err != nil || funded.Uint64() != 4000 {
		t.Fatalf("funded %s %v", funded.Dec(), err)
	}
	if demand, err := client.LedgerV2Demand(ctx, 1, 2, 0, 1); err != nil || demand != StLedgerV2DemandOpen {
		t.Fatalf("demand %d %v", demand, err)
	}
	if root, err := client.LedgerV2Root(ctx, 1, 0); err != nil || !reflect.DeepEqual(root, want.Latest) {
		t.Fatalf("root %+v %v", root, err)
	}
	if end, window, err := client.LedgerV2EpochWindow(ctx, 6); err != nil || end != 352_800 || window != 1_200 {
		t.Fatalf("window %d %d %v", end, window, err)
	}

	result(vault.PackRootCount(no), vaultAbi, "rootCount", big.NewInt(0))
	result(vault.PackPending(no), vaultAbi, "pending", stabi.ISTSettlementVault2Pending{Total: big.NewInt(0)})
	if state, err := client.LedgerV2State(ctx, 1); err != nil || state.Latest != nil || state.Pending != nil {
		t.Fatalf("empty pool state %+v %v", state, err)
	}

	result(vault.PackRootCount(no), vaultAbi, "rootCount", new(big.Int).Lsh(big.NewInt(1), 64))
	if _, err := client.LedgerV2State(ctx, 1); err == nil || !strings.Contains(err.Error(), "vault2 rootCount(): st: chain value 18446744073709551616 is not a uint64") {
		t.Fatalf("oversized root count: %v", err)
	}
	result(vault.PackRootCount(no), vaultAbi, "rootCount", big.NewInt(1))
	result(coordinator.PackEpochEndBlock(big.NewInt(6)), coordinatorAbi, "epochEndBlock", new(big.Int).Lsh(big.NewInt(1), 64))
	if _, _, err := client.LedgerV2EpochWindow(ctx, 6); err == nil || !strings.Contains(err.Error(), "is not a uint64") {
		t.Fatalf("oversized epoch end: %v", err)
	}
	result(coordinator.PackEpochEndBlock(big.NewInt(6)), coordinatorAbi, "epochEndBlock", big.NewInt(352_800))
	for _, read := range []struct {
		calldata []byte
		reason   string
	}{
		{coordinator.PackPaused(), "paused():"},
		{vault.PackPools(no), "vault2 pools():"},
		{vault.PackCHALLENGEBLOCKS(), "vault2 CHALLENGE_BLOCKS():"},
		{vault.PackLastCommitBlock(no), "vault2 lastCommitBlock():"},
		{vault.PackLastFundedEpoch(no), "vault2 lastFundedEpoch():"},
		{vault.PackRootCount(no), "vault2 rootCount():"},
		{settlement.PackApprovalCount(no), "approvalCount():"},
		{settlement.PackQuorum(), "quorum():"},
		{vault.PackRoots(no, big.NewInt(0)), "vault2 roots():"},
		{vault.PackPending(no), "vault2 pending():"},
	} {
		fixture.reverts[string(read.calldata)] = "UnknownPool"
		if _, err := client.LedgerV2State(ctx, 1); err == nil || !strings.Contains(err.Error(), read.reason) {
			t.Fatalf("%s: %v", read.reason, err)
		}
		delete(fixture.reverts, string(read.calldata))
	}
	for _, refusal := range []struct {
		calldata []byte
		read     func() error
		reason   string
	}{
		{coordinator.PackPaused(), func() error { _, err := client.LedgerV2State(ctx, 1); return err }, "paused():"},
		{vault.PackPools(no), func() error { _, _, err := client.LedgerV2Pool(ctx, 1); return err }, "vault2 pools(1):"},
		{vault.PackCoordinator(), func() error { _, err := client.LedgerV2Coordinator(ctx); return err }, "vault2 coordinator():"},
		{vault.PackFundedThrough(no, big.NewInt(6)), func() error { _, err := client.LedgerV2FundedThrough(ctx, 1, 6); return err }, "vault2 fundedThrough(1, 6):"},
		{vault.PackDemanded(no, 2, big.NewInt(0), big.NewInt(1)), func() error { _, err := client.LedgerV2Demand(ctx, 1, 2, 0, 1); return err }, "vault2 demanded(1, 2, 0, 1):"},
		{vault.PackRoots(no, big.NewInt(0)), func() error { _, err := client.LedgerV2Root(ctx, 1, 0); return err }, "vault2 roots(1, 0):"},
		{coordinator.PackEpochEndBlock(big.NewInt(6)), func() error { _, _, err := client.LedgerV2EpochWindow(ctx, 6); return err }, "epochEndBlock(6):"},
		{coordinator.PackPolicyAt(big.NewInt(6)), func() error { _, _, err := client.LedgerV2EpochWindow(ctx, 6); return err }, "policyAt(6):"},
	} {
		fixture.reverts[string(refusal.calldata)] = "UnknownPool"
		if err := refusal.read(); err == nil || !strings.Contains(err.Error(), refusal.reason) {
			t.Fatalf("%s: %v", refusal.reason, err)
		}
		delete(fixture.reverts, string(refusal.calldata))
	}
	client.cfg.RpcUrls = nil
	for name, read := range map[string]func() error{
		"state":  func() error { _, err := client.LedgerV2State(ctx, 1); return err },
		"window": func() error { _, _, err := client.LedgerV2EpochWindow(ctx, 6); return err },
	} {
		if err := read(); err == nil || !strings.Contains(err.Error(), "finalized head:") {
			t.Fatalf("%s without an endpoint: %v", name, err)
		}
	}
}

type ledgerV2DataError struct{ data any }

func (self ledgerV2DataError) Error() string  { return "execution reverted" }
func (self ledgerV2DataError) ErrorData() any { return self.data }

// Reverts are named from the v2 contracts' and LedgerTree's errors.
// SV-7 (SPEC A6)
func TestStLedgerV2RevertNames(t *testing.T) {
	for _, test := range []struct {
		err  error
		name string
	}{
		{errors.New("connection refused"), "failed"},
		{ledgerV2DataError{nil}, "failed"},
		{ledgerV2DataError{"0x12"}, "failed"},
		{ledgerV2DataError{"not hex"}, "failed"},
		{ledgerV2DataError{"0x12345678"}, "reverts"},
		{ledgerV2DataError{ledgerV2ErrorData("NoFault")}, "reverts NoFault"},
		{fmt.Errorf("wrapped: %w", ledgerV2DataError{ledgerV2ErrorData("SettlementPaused")}), "reverts SettlementPaused"},
		{ledgerV2DataError{ledgerV2ErrorData("MalformedPath")}, "reverts MalformedPath"},
	} {
		if name := stLedgerV2RevertName(test.err); name != test.name {
			t.Fatalf("%v named %q, want %q", test.err, name, test.name)
		}
	}
}

// The checkPath calldata carries the finalized root's index, the account,
// its proof and the pending path exactly.
// SV-1 (demand answering)
func TestStLedgerV2PackCheckPath(t *testing.T) {
	accounts := ledgerV2Accounts(ledgerV2Coldkey(1), 500, ledgerV2Coldkey(2), 2000, ledgerV2Coldkey(3), 1500)
	tree, err := merkle.BuildLedgerSumTree(context.Background(), accounts)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := tree.Proof(1)
	path, _ := tree.Path(1)
	calldata := stLedgerV2PackCheckPath(1, &StLedgerV2Answer{RootIndex: 3, Index: 1, Account: accounts[1], Proof: proof, Path: path})
	parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	values, err := parsed.Methods["checkPath"].Inputs.Unpack(calldata[4:])
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(values)
	want, _ := json.Marshal([]any{
		big.NewInt(1),
		big.NewInt(3),
		map[string]any{"index": big.NewInt(1), "coldkey": accounts[1].Coldkey, "cumulative": big.NewInt(2000)},
		[]map[string]any{{"hash": proof[0].Hash, "sum": proof[0].Sum.ToBig()}, {"hash": proof[1].Hash, "sum": proof[1].Sum.ToBig()}},
		[]map[string]any{
			{"leftHash": path[0].LeftHash, "leftSum": path[0].LeftSum.ToBig(), "rightHash": path[0].RightHash, "rightSum": path[0].RightSum.ToBig()},
			{"leftHash": path[1].LeftHash, "leftSum": path[1].LeftSum.ToBig(), "rightHash": path[1].RightHash, "rightSum": path[1].RightSum.ToBig()},
		},
	})
	var got, expected any
	if json.Unmarshal(encoded, &got) != nil || json.Unmarshal(want, &expected) != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("checkPath arguments %s\nwant %s", encoded, want)
	}
}
