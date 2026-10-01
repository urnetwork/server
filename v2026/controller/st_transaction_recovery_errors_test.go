// Durable operator attempts survive inconclusive receipt reads. These tests
// use only synthetic signed bytes, isolated databases and local read-only Rpc.
package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The first explicit phase withholds one winning receipt while other hashes
// return not found. The next phase exposes its canonical inclusion unchanged.
type stRecoveryReadFixture struct {
	t          testing.TB
	stateLock  sync.Mutex
	phase      int
	fault      string
	winner     *types.Receipt
	receiptKVs map[string]int
	nonceReads int
	writeCalls int
}

// Only reads are implemented: any attempt to price, sign or submit new work is
// an immediate failure. Rpc errors are serialized rather than injected helpers.
func (self *stRecoveryReadFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var call stEpochRPCRequest
	if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
		self.t.Errorf("decode recovery request: %v", err)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	var result any
	switch call.Method {
	case "eth_chainId":
		result = hexutil.EncodeUint64(31337)
	case "eth_getTransactionReceipt":
		var hash string
		if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &hash) != nil {
			self.t.Error("invalid recovery receipt request")
			return
		}
		self.receiptKVs[hash]++
		if hash == self.winner.TxHash.Hex() {
			if self.phase == 0 && self.fault == "transport" {
				response["error"] = map[string]any{"code": -32000, "message": "synthetic receipt storage temporarily unavailable"}
			} else if self.phase == 0 && self.fault == "identity" {
				changed := *self.winner
				changed.TxHash[0] ^= 1
				result = &changed
			} else {
				result = self.winner
			}
		}
	case "eth_getBlockByNumber":
		var selector string
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &selector) != nil {
			self.t.Error("invalid recovery block request")
			return
		}
		block := uint64(100)
		if selector != "finalized" {
			var err error
			block, err = hexutil.DecodeUint64(selector)
			if err != nil {
				self.t.Errorf("decode recovery block: %v", err)
				return
			}
		}
		result = stTransactionReconcileBlock(block)
	case "eth_getTransactionCount":
		self.nonceReads++
		result = "0x8"
	default:
		self.writeCalls++
		self.t.Errorf("recovery reached forbidden method %s", call.Method)
		response["error"] = map[string]any{"code": -32601, "message": "fixture permits receipt reconciliation only"}
	}
	if _, failed := response["error"]; !failed {
		response["result"] = result
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		self.t.Errorf("encode recovery response: %v", err)
	}
}

// Both independent operator identities retain an original, a replacement and
// a cancellation under one nonce. Every byte is persisted through production.
func newStRecoveryReadFixture(t testing.TB, operator, winner, attemptCount int, fault string) (*CoreStClient, *ethclient.Client, *model.StTransactionIntent, *stRecoveryReadFixture) {
	t.Helper()
	ctx := context.Background()
	identity := fmt.Sprintf("synthetic operator recovery/%d/%d/%s", operator, winner, fault)
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte(identity)))
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	cfg := &StConfig{Profile: "mainnet", DeploymentId: "synthetic-operator-recovery", ChainId: 31337, ContractAddress: common.Address{0x24}}
	cfg.GenesisHash = [32]byte(crypto.Keccak256Hash([]byte("synthetic operator recovery genesis")))
	calldata := []byte{1, 2, 3, 4}
	intent := model.ReserveStTransactionIntent(ctx, identity, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, "0x"+hex.EncodeToString(cfg.GenesisHash[:]), strings.ToLower(from.Hex()), strings.ToLower(cfg.ContractAddress.Hex()), crypto.Keccak256Hash(calldata).Hex(), calldata, 7)
	fixture := &stRecoveryReadFixture{t: t, fault: fault, receiptKVs: map[string]int{}}
	for index := 0; index < attemptCount; index++ {
		kind, to, data, gas := model.StTxAttemptExecution, cfg.ContractAddress, calldata, uint64(50000)
		if index == 2 {
			kind, to, data, gas = model.StTxAttemptCancellation, from, nil, 21000
		}
		price := big.NewInt(int64(100 + index*25))
		transaction, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, To: &to, Value: new(big.Int), Gas: gas, GasPrice: price, Data: data}), types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId)), key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := transaction.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		priceString := price.String()
		model.AddStTransactionAttempt(ctx, &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: index + 1, Kind: kind, TxHash: transaction.Hash().Hex(), RawTransaction: raw, GasLimit: gas, GasPrice: &priceString})
		if index == winner {
			fixture.winner = &types.Receipt{Type: transaction.Type(), Status: types.ReceiptStatusSuccessful, TxHash: transaction.Hash(), BlockHash: stTransactionReconcileBlockHash(99), BlockNumber: big.NewInt(99), GasUsed: gas, CumulativeGasUsed: gas, EffectiveGasPrice: price, Logs: []*types.Log{}}
		}
	}
	endpoint := httptest.NewServer(fixture)
	t.Cleanup(endpoint.Close)
	cfg.RpcUrls = []string{endpoint.URL}
	rpcClient, err := ethclient.Dial(endpoint.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcClient.Close)
	client := &CoreStClient{cfg: cfg, clients: map[string]*ethclient.Client{endpoint.URL: rpcClient}}
	return client, rpcClient, model.GetStTransactionIntent(ctx, intent.LogicalKey), fixture
}

// A partial census is never proof that an unknown transaction consumed the
// nonce. A fresh owner must still discover and reconcile all retained attempts.
func checkStRecoveryReadFailure(t testing.TB, fault string) {
	t.Helper()
	ctx := context.Background()
	for operator := 1; operator <= 2; operator++ {
		for winner := 0; winner < 3; winner++ {
			client, rpcClient, intent, fixture := newStRecoveryReadFixture(t, operator, winner, 3, fault)
			before := model.GetStTransactionAttempts(ctx, intent.IntentId)
			from := common.HexToAddress(intent.FromAddress)
			err := client.reconcileAccountIntents(ctx, rpcClient, nil, from)
			retained := model.GetStTransactionIntent(ctx, intent.LogicalKey)
			after := model.GetStTransactionAttempts(ctx, intent.IntentId)
			unresolved := model.GetUnresolvedStTransactionIntents(ctx, intent.ChainId, intent.GenesisHash, intent.FromAddress)
			if err == nil || !reflect.DeepEqual(intent, retained) || !reflect.DeepEqual(before, after) || len(unresolved) != 1 || unresolved[0].IntentId != intent.IntentId {
				t.Fatalf("incomplete receipt census omitted durable attempts: operator=%d winner=%d status=%s unresolved=%d error=%v", operator, winner, retained.Status, len(unresolved), err)
			}
			fixture.stateLock.Lock()
			if len(fixture.receiptKVs) != 3 || fixture.nonceReads != 0 || fixture.writeCalls != 0 {
				t.Errorf("failed census fell through: receipts=%d nonce=%d writes=%d", len(fixture.receiptKVs), fixture.nonceReads, fixture.writeCalls)
			}
			fixture.phase = 1
			fixture.stateLock.Unlock()
			// No in-memory intent or cached verdict is reused after the failed turn.
			restarted := &CoreStClient{cfg: client.cfg, clients: map[string]*ethclient.Client{client.cfg.RpcUrls[0]: rpcClient}}
			if err := restarted.reconcileAccountIntents(ctx, rpcClient, nil, from); err != nil {
				t.Fatal(err)
			}
			final := model.GetStTransactionIntent(ctx, intent.LogicalKey)
			status := model.StTxFinalized
			if winner == 2 {
				status = model.StTxCanceled
			}
			if final.Status != status || final.CurrentTxHash == nil || *final.CurrentTxHash != fixture.winner.TxHash.Hex() || final.AttemptCount != 3 {
				t.Fatalf("restart failed to recover exact signed winner: operator=%d winner=%d intent=%+v", operator, winner, final)
			}
			finalAttempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
			if len(finalAttempts) != len(before) {
				t.Fatal("recovery created or lost a signed attempt")
			}
			for index, original := range before {
				got := finalAttempts[index]
				if got.TxHash != original.TxHash || !reflect.DeepEqual(got.RawTransaction, original.RawTransaction) || got.GasLimit != original.GasLimit || !reflect.DeepEqual(got.GasPrice, original.GasPrice) || got.Kind != original.Kind {
					t.Fatal("recovery rewrote original signed bytes or fee liability")
				}
			}
			if err := restarted.reconcileAccountIntents(ctx, rpcClient, nil, from); err != nil {
				t.Fatal(err)
			}
			fixture.stateLock.Lock()
			if fixture.writeCalls != 0 || fixture.nonceReads != 0 {
				t.Errorf("receipt recovery allocated or submitted work: nonce=%d writes=%d", fixture.nonceReads, fixture.writeCalls)
			}
			fixture.stateLock.Unlock()
		}
	}
}

// A serialized Rpc error must retain candidates regardless of which signed
// execution/replacement/cancellation won or which operator owns the nonce.
func TestStAccountRecoveryReadErrorRetainsEverySignedCandidate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) { checkStRecoveryReadFailure(tb, "transport") })
}

// An integrity failure is also an incomplete census and cannot be converted
// into terminal supersession by consulting an unrelated nonce observation.
func TestStAccountRecoveryMalformedReceiptRetainsEverySignedCandidate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) { checkStRecoveryReadFailure(tb, "identity") })
}

// A proved orphan can precede or follow a failed read. Neither scan order may
// erase that failure and remove all candidate signatures from later recovery.
func TestStAccountRecoveryReadErrorBesideOrphanRetainsCandidates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		for _, fault := range []string{"transport", "identity"} {
			for _, winner := range []int{0, 2} {
				client, rpcClient, intent, fixture := newStRecoveryReadFixture(tb, 3, winner, 3, fault)
				attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
				// Newest-first order places the opposite end before/after the error.
				orphan := attempts[winner]
				model.MarkStTransactionMined(ctx, intent.IntentId, orphan.Attempt, orphan.TxHash, 98, stTransactionReconcileBlockHash(98).Hex())
				from := common.HexToAddress(intent.FromAddress)
				err := client.reconcileAccountIntents(ctx, rpcClient, nil, from)
				wantError := "synthetic receipt storage temporarily unavailable"
				if fault == "identity" {
					wantError = "receipt response differs"
				}
				retained := model.GetStTransactionIntent(ctx, intent.LogicalKey)
				pending := model.GetUnresolvedStTransactionIntents(ctx, intent.ChainId, intent.GenesisHash, intent.FromAddress)
				if err == nil || !strings.Contains(err.Error(), wantError) || !strings.Contains(err.Error(), "disappeared before finality") || retained.Status != model.StTxUncertain || len(pending) != 1 {
					tb.Fatalf("orphan hid failed read: fault=%s winner=%d status=%s pending=%d error=%v", fault, winner, retained.Status, len(pending), err)
				}
				after := model.GetStTransactionAttempts(ctx, intent.IntentId)
				if len(after) != 3 || after[winner].InclusionBlock != nil || after[winner].InclusionHash != nil {
					tb.Fatal("proven orphan was not retained without its stale inclusion")
				}
				for index := range attempts {
					if !reflect.DeepEqual(after[index].RawTransaction, attempts[index].RawTransaction) || after[index].TxHash != attempts[index].TxHash {
						tb.Fatal("mixed observation rewrote a retained signature")
					}
				}
				fixture.stateLock.Lock()
				if fixture.nonceReads != 0 || fixture.writeCalls != 0 || len(fixture.receiptKVs) != 3 {
					tb.Errorf("mixed census fell through: nonce=%d writes=%d receipts=%d", fixture.nonceReads, fixture.writeCalls, len(fixture.receiptKVs))
				}
				fixture.phase = 1
				fixture.stateLock.Unlock()
				if err := client.reconcileAccountIntents(ctx, rpcClient, nil, from); err != nil {
					tb.Fatal(err)
				}
				final := model.GetStTransactionIntent(ctx, intent.LogicalKey)
				if final.CurrentTxHash == nil || *final.CurrentTxHash != fixture.winner.TxHash.Hex() || (final.Status != model.StTxFinalized && final.Status != model.StTxCanceled) {
					tb.Fatal("mixed observation prevented later exact recovery")
				}
			}
		}
	})
}

// An eligible replacement age cannot turn a canceled receipt read into new
// signing authority. No wall-clock wait is needed to force the old branch.
func TestStReplacementWaitReadErrorCannotAuthorizeReplacement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, rpcClient, intent, _ := newStRecoveryReadFixture(tb, 1, 0, 2, "transport")
		attempts := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		attempts[0].CreateTime = time.Unix(1, 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.waitFinalizedAttempt(ctx, rpcClient, intent, attempts)
		if errors.Is(err, errStReplaceTransaction) || !errors.Is(err, context.Canceled) {
			tb.Fatalf("inconclusive receipt read granted replacement: %v", err)
		}
	})
}
