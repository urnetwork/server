// Actual deposit entry points exercise retained signatures, exact Rpc framing
// and durable database transitions. Fault phases have explicit causal edges.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Reuses the full authority/stake fixture behind an actual Http endpoint. The
// hook may fault one exact read; all untouched calls reach production decoding.
func stWalletRecoveryEndpoint(t testing.TB, fixture *stDepositCustodyRpc, hook func(http.ResponseWriter, stEpochRPCRequest) bool) (string, *ethclient.Client) {
	t.Helper()
	service := rpc.NewServer()
	if err := service.RegisterName("eth", fixture); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterName("chain", fixture); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
		if hook != nil {
			var calls []stEpochRPCRequest
			if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
				if err := json.Unmarshal(body, &calls); err != nil {
					t.Error(err)
					return
				}
			} else {
				var call stEpochRPCRequest
				if err := json.Unmarshal(body, &call); err != nil {
					t.Error(err)
					return
				}
				calls = append(calls, call)
			}
			for _, call := range calls {
				if hook(writer, call) {
					return
				}
			}
		}
		service.ServeHTTP(writer, request)
	}))
	client, err := ethclient.Dial(endpoint.URL)
	if err != nil {
		endpoint.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); endpoint.Close(); service.Stop() })
	return endpoint.URL, client
}

// Writes protocol responses instead of injecting a helper's return value.
func stWalletRecoveryReply(writer http.ResponseWriter, call stEpochRPCRequest, result any, failure bool) {
	writer.Header().Set("Content-Type", "application/json")
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
	if failure {
		delete(response, "result")
		response["error"] = map[string]any{"code": -32000, "message": "synthetic finalized authority unavailable"}
	}
	_ = json.NewEncoder(writer).Encode(response)
}

// Stores one immutable deposit plus genuinely signed fee candidates, without
// broadcasting or pretending a local return value established chain effects.
func stWalletRecoveryIntent(t testing.TB, client *CoreStClient, epoch, deadline uint64, attempts int) *model.StTransactionIntent {
	t.Helper()
	cfg := client.cfg
	data := client.coordinator.PackDeposit(new(big.Int).SetUint64(cfg.NoId), big.NewInt(100), big.NewInt(3), deadline)
	logical, err := stTransactionLogicalKey(cfg, fmt.Sprintf("deposit:%d:%d:3", epoch, cfg.NoId))
	if err != nil {
		t.Fatal(err)
	}
	intent := model.ReserveStTransactionIntent(context.Background(), logical, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, hexutil.Encode(cfg.GenesisHash[:]), strings.ToLower(crypto.PubkeyToAddress(cfg.DepositKey.PublicKey).Hex()), strings.ToLower(cfg.ContractAddress.Hex()), strings.ToLower(crypto.Keccak256Hash(data).Hex()), data, 7)
	for index := 1; index <= attempts; index++ {
		stWalletRecoveryAddAttempt(t, client, intent, index)
	}
	return model.GetStTransactionIntent(context.Background(), logical)
}

// The same helper also elects a concurrent first/last candidate at a precise
// Rpc barrier. Each row carries its actual fee, nonce, calldata and signature.
func stWalletRecoveryAddAttempt(t testing.TB, client *CoreStClient, intent *model.StTransactionIntent, number int) *model.StTransactionAttempt {
	t.Helper()
	to := common.HexToAddress(intent.ToAddress)
	price := big.NewInt(int64(10 * number))
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: intent.Nonce, To: &to, Gas: 60_000, GasPrice: price, Value: new(big.Int), Data: intent.Calldata}), types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId)), client.cfg.DepositKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	amount := price.String()
	return model.AddStTransactionAttempt(context.Background(), &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: number, Kind: model.StTxAttemptExecution, TxHash: strings.ToLower(tx.Hash().Hex()), RawTransaction: raw, GasLimit: tx.Gas(), GasPrice: &amount})
}

// Completes the exact retained principal in the fixture's canonical past.
func stWalletRecoveryInclude(fixture *stDepositCustodyRpc, attempt *model.StTransactionAttempt) {
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	price, _ := new(big.Int).SetString(*attempt.GasPrice, 10)
	hash := common.HexToHash(attempt.TxHash)
	fixture.receiptKVs[hash] = &types.Receipt{Type: types.LegacyTxType, Status: types.ReceiptStatusSuccessful, TxHash: hash, BlockHash: stTransactionReconcileBlockHash(99), BlockNumber: big.NewInt(99), GasUsed: attempt.GasLimit, CumulativeGasUsed: attempt.GasLimit, EffectiveGasPrice: price, Logs: []*types.Log{}}
	fixture.accountNonce = 8
	fixture.staged.SetInt64(0)
	fixture.deposited.SetInt64(100)
	fixture.depositNonce.SetInt64(4)
}

// A fresh client discards all process memory but reopens the same durable
// operator namespace and independently re-reads its retained signatures.
func stWalletRecoveryRestart(client *CoreStClient) *CoreStClient {
	return &CoreStClient{cfg: client.cfg, coordinator: client.coordinator, vault: client.vault, clients: client.clients, readHooks: client.readHooks}
}

// Provisional inclusion may advance while finality is unavailable. The exact
// original signature and all economic envelope fields must remain immutable.
func stWalletRecoveryOriginalsEqual(before, after []*model.StTransactionAttempt) bool {
	if len(before) != len(after) {
		return false
	}
	for index, original := range before {
		current := after[index]
		if current.IntentId != original.IntentId || current.Attempt != original.Attempt || current.TxHash != original.TxHash || current.Kind != original.Kind || !bytes.Equal(current.RawTransaction, original.RawTransaction) || current.GasLimit != original.GasLimit || !reflect.DeepEqual(current.GasPrice, original.GasPrice) || !reflect.DeepEqual(current.GasTipCap, original.GasTipCap) || !reflect.DeepEqual(current.GasFeeCap, original.GasFeeCap) {
			return false
		}
	}
	return true
}

// A selected endpoint's receipt cannot borrow another endpoint's finality.
// On recovery, the same public entry finalizes the original without sending.
func TestStTransactionWalletReceiptCannotBorrowEndpointFinality(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 1)
		attempt := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		stWalletRecoveryInclude(fixture, attempt)
		var failed atomic.Bool
		failed.Store(true)
		var foreignReads atomic.Int32
		selectedUrl, selected := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_getBlockByNumber" && len(call.Params) != 0 && string(call.Params[0]) == `"finalized"` && failed.Load() {
				stWalletRecoveryReply(writer, call, nil, true)
				return true
			}
			return false
		})
		foreignUrl, foreign := stWalletRecoveryEndpoint(tb, fixture, func(http.ResponseWriter, stEpochRPCRequest) bool { foreignReads.Add(1); return false })
		client.cfg.RpcUrls = []string{selectedUrl, foreignUrl}
		client.clients = map[string]*ethclient.Client{selectedUrl: selected, foreignUrl: foreign}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			tb.Fatal("foreign endpoint finalized the selected endpoint's receipt")
		}
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if retained.Status != model.StTxMined || foreignReads.Load() != 0 {
			tb.Fatalf("incomplete endpoint proof changed custody: status=%s foreign=%d", retained.Status, foreignReads.Load())
		}
		failed.Store(false)
		if _, err := stWalletRecoveryRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		retained = model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if retained.Status != model.StTxFinalized || retained.CurrentTxHash == nil || *retained.CurrentTxHash != attempt.TxHash || len(fixture.sent) != 0 {
			tb.Fatal("restart lost original receipt or submitted another deposit")
		}
	})
}

// Finalized account-nonce reads need the same endpoint and explicit canonical
// hash, even when no signed attempt exists yet.
func TestStTransactionWalletUnsignedNonceUsesCanonicalEndpoint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 0)
		fixture.accountNonce = 8
		var canonical atomic.Bool
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method != "eth_getTransactionCount" {
				return false
			}
			var selector rpc.BlockNumberOrHash
			if len(call.Params) == 2 && json.Unmarshal(call.Params[1], &selector) == nil && selector.BlockHash != nil && *selector.BlockHash == stTransactionReconcileBlockHash(100) && selector.RequireCanonical {
				canonical.Store(true)
				return false
			}
			stWalletRecoveryReply(writer, call, nil, true)
			return true
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			tb.Fatal("externally consumed unsigned nonce was executed")
		}
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if !canonical.Load() || retained.Status != model.StTxSuperseded || retained.AttemptCount != 0 || len(fixture.sent) != 0 {
			tb.Fatal("unsigned nonce was retired without canonical hash custody")
		}
	})
}

// A nonce answer at a hash that changes before closing cannot retire even an
// unsigned reservation. Recovery must repeat the coherent original read first.
func TestStTransactionWalletNonceClosingConflictRetainsReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 0)
		fixture.accountNonce = 8
		var conflict atomic.Bool
		conflict.Store(true)
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if conflict.Load() && call.Method == "eth_getBlockByNumber" && len(call.Params) != 0 && string(call.Params[0]) == `"0x64"` {
				block := stTransactionReconcileBlock(100)
				block["hash"] = common.Hash{0xfc}.Hex()
				stWalletRecoveryReply(writer, call, block, false)
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		_, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if err == nil || !strings.Contains(err.Error(), "boundary changed") || retained.Status != model.StTxPrepared || retained.AttemptCount != 0 || len(fixture.sent) != 0 {
			tb.Fatal("nonce closing conflict retired original reservation", err)
		}
		conflict.Store(false)
		_, err = stWalletRecoveryRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		if err == nil || model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxSuperseded || len(fixture.sent) != 0 {
			tb.Fatal("coherent unsigned nonce recovery failed", err)
		}
	})
}

// A changed closing hash is a hard contradiction. It neither finalizes the
// original nor permits a replacement, and a later coherent read can recover.
func TestStTransactionWalletClosingFinalityConflictRetainsOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 1)
		attempt := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		stWalletRecoveryInclude(fixture, attempt)
		var conflict atomic.Bool
		conflict.Store(true)
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if conflict.Load() && call.Method == "eth_getBlockByNumber" && len(call.Params) > 0 && string(call.Params[0]) == `"0x64"` {
				block := stTransactionReconcileBlock(100)
				block["hash"] = common.Hash{0xfa}.Hex()
				stWalletRecoveryReply(writer, call, block, false)
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		before := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		_, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		if err == nil || !strings.Contains(err.Error(), "boundary changed") || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(context.Background(), intent.IntentId)) || model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxMined || len(fixture.sent) != 0 {
			tb.Fatal("closing hash contradiction finalized or rewrote original custody", err)
		}
		conflict.Store(false)
		if _, err := stWalletRecoveryRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		if model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxFinalized || len(fixture.sent) != 0 {
			tb.Fatal("coherent restart failed to recover original inclusion")
		}
	})
}

// An advanced nonce is not a receipt. Keep every signed candidate discoverable
// for either operator until its actual winner becomes available after restart.
func TestStTransactionWalletMissingReceiptRetainsSignedNonceAcrossRestart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for operator := uint64(1); operator <= 2; operator++ {
			client, _, fixture := newStDepositCustodyFixture(tb, operator)
			intent := stWalletRecoveryIntent(tb, client, 7, 1999, 3)
			fixture.accountNonce = 8
			before := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
			if _, err := client.DepositCredit(context.Background(), 7, operator, big.NewInt(100)); err == nil {
				tb.Fatal("missing receipt was accepted as a completed signed outcome")
			}
			retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
			unresolved := model.GetUnresolvedStTransactionIntents(context.Background(), intent.ChainId, intent.GenesisHash, intent.FromAddress)
			if retained.Status != model.StTxSigned || len(unresolved) != 1 || !reflect.DeepEqual(before, model.GetStTransactionAttempts(context.Background(), intent.IntentId)) || len(fixture.sent) != 0 {
				tb.Fatal("advanced nonce erased original signed liability")
			}
			stWalletRecoveryInclude(fixture, before[2])
			if _, err := stWalletRecoveryRestart(client).DepositCredit(context.Background(), 7, operator, big.NewInt(100)); err != nil {
				tb.Fatal(err)
			}
			retained = model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
			if retained.Status != model.StTxFinalized || retained.CurrentTxHash == nil || *retained.CurrentTxHash != before[2].TxHash || retained.AttemptCount != 3 || len(fixture.sent) != 0 {
				tb.Fatal("restart did not recover exact oldest winner")
			}
		}
	})
}

// Elect the first signature after the caller's empty receipt census but before
// its advanced nonce reply. The model's row lock must preserve that signature.
func TestStTransactionWalletFirstSignatureRacePreventsSupersession(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 0)
		fixture.accountNonce = 8
		var inserted atomic.Bool
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(_ http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_getTransactionCount" && inserted.CompareAndSwap(false, true) {
				stWalletRecoveryAddAttempt(tb, client, intent, 1)
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			tb.Fatal("raced first signature was classified as unsigned")
		}
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		attempt := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		if !inserted.Load() || retained.Status != model.StTxSigned || retained.AttemptCount != 1 || attempt == nil || len(fixture.sent) != 0 {
			tb.Fatal("unsigned-only retirement erased a concurrently elected signature")
		}
		stWalletRecoveryInclude(fixture, attempt)
		if _, err := stWalletRecoveryRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil || model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxFinalized {
			tb.Fatal("raced signature was not recoverable after restart", err)
		}
	})
}

// The old signed deposit is expired. Its exact nonce is canceled before the
// current epoch receives one principal; original bytes and both nonces remain.
func TestStTransactionWalletExpiredSignedDepositCancelsBeforeCurrentCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		intent := stWalletRecoveryIntent(tb, client, 6, 99, 1)
		original := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatalf("expired signed deposit blocked current credit: %v", err)
		}
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		attempts := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		if retained.Status != model.StTxCanceled || len(attempts) != 2 || !bytes.Equal(attempts[1].RawTransaction, original.RawTransaction) || attempts[0].Kind != model.StTxAttemptCancellation {
			tb.Fatal("expired deposit lost original signature or was not canceled")
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		from := crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey)
		if len(fixture.sent) != 2 || fixture.sent[0].To() == nil || *fixture.sent[0].To() != from || fixture.sent[0].Nonce() != 7 || fixture.sent[1].Nonce() != 8 || fixture.deposited.Int64() != 100 || fixture.depositNonce.Int64() != 4 || fixture.source.Int64() != 10_000 {
			tb.Fatal("expiry repeated principal or changed original nonce custody")
		}
	})
}

// Immediate cancellation shares the original finite allowance. Exhaustion
// preserves all three signatures and their liability without another send.
func TestStTransactionWalletExpiredDepositAtAttemptCapRetainsLiability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 6, 99, stTxMaxAttempts)
		before := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		hash, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		if !errors.Is(err, errStTransactionAttemptLimit) || hash != "" && hash != before[0].TxHash || !reflect.DeepEqual(before, model.GetStTransactionAttempts(context.Background(), intent.IntentId)) || len(fixture.sent) != 0 {
			tb.Fatal("expired cancellation exceeded original attempt allowance", err)
		}
	})
}

// A concurrent worker elects attempt three during the second receipt census.
// The earlier wait decision cannot authorize attempt four after that election.
func TestStTransactionWalletConcurrentReplacementCannotExceedAttemptCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 2)
		server.Tx(context.Background(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(context.Background(), `UPDATE st_transaction_attempt SET create_time=$2 WHERE intent_id=$1`, intent.IntentId, time.Unix(1, 0)))
		})
		var reads atomic.Int32
		var sends atomic.Int32
		known := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_getTransactionReceipt" && reads.Add(1) == 3 {
				stWalletRecoveryAddAttempt(tb, client, intent, 3)
			}
			if call.Method != "eth_sendRawTransaction" {
				return false
			}
			var raw hexutil.Bytes
			var transaction types.Transaction
			if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &raw) != nil || transaction.UnmarshalBinary(raw) != nil {
				tb.Error("malformed fixture signed send")
				stWalletRecoveryReply(writer, call, nil, true)
				return true
			}
			sends.Add(1)
			if transaction.Hash().Hex() != known[0].TxHash {
				stWalletRecoveryInclude(fixture, model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId))
			}
			stWalletRecoveryReply(writer, call, transaction.Hash(), false)
			return true
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		_, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if !errors.Is(err, errStTransactionAttemptLimit) || retained.AttemptCount != 3 || len(model.GetStTransactionAttempts(context.Background(), intent.IntentId)) != 3 || sends.Load() != 1 {
			tb.Fatalf("stale replacement decision escaped durable allowance: count=%d sends=%d err=%v", retained.AttemptCount, sends.Load(), err)
		}
	})
}

// A real transient receipt failure retries only reads and recovers the exact
// retained winner. It never signs another candidate or repeats the principal.
func TestStTransactionWalletTransientReceiptRetriesWithoutSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 1)
		stWalletRecoveryInclude(fixture, model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId))
		var reads atomic.Int32
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_getTransactionReceipt" && reads.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		now := time.Now()
		waits := 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatalf("public deposit failed bounded transient retry: %v", err)
		}
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if reads.Load() != 2 || waits != 1 || retained.Status != model.StTxFinalized || retained.AttemptCount != 1 || len(fixture.sent) != 0 {
			tb.Fatal("soft receipt recovery repeated signed work or failed to retry")
		}
	})
}

// The deposit custody caller adopts the finite read owner before any account
// nonce reservation. A transient whole-snapshot read still produces one debit.
func TestStTransactionWalletCustodyReadRetriesBeforeSigning(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		var faults atomic.Int32
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "chain_getBlockHash" && faults.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		now := time.Now()
		waits := 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatalf("public deposit custody read failed bounded retry: %v", err)
		}
		if faults.Load() < 2 || waits != 1 || len(fixture.sent) != 1 || fixture.accountNonce != 8 || fixture.deposited.Int64() != 100 || fixture.depositNonce.Int64() != 4 {
			tb.Fatal("custody retry repeated principal or failed to reach exact read fault")
		}
	})
}

// Fee preparation repeats its complete read after a lost quote. Neither the
// failed quote nor its retry creates another durable signed candidate.
func TestStTransactionWalletFeeReadRetriesBeforeSigning(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		var quotes atomic.Int32
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_gasPrice" && quotes.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		now := time.Now()
		waits := 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatalf("public deposit fee read failed bounded retry: %v", err)
		}
		logical, err := stTransactionLogicalKey(client.cfg, "deposit:7:1:3")
		if err != nil {
			tb.Fatal(err)
		}
		intent := model.GetStTransactionIntent(context.Background(), logical)
		if quotes.Load() != 2 || waits != 1 || intent == nil || intent.AttemptCount != 1 || intent.Status != model.StTxFinalized || len(fixture.sent) != 1 || fixture.deposited.Int64() != 100 {
			tb.Fatal("fee retry repeated signature or principal")
		}
	})
}

// A malformed or absent wire status is unknown, not a canonical revert that
// permits another generation. Both forms preserve all bytes until recovery.
func TestStTransactionWalletMalformedExecutionStatusCannotAuthorizeRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for index, fault := range []string{"absent", "invalid"} {
			operator := uint64(index + 1)
			client, _, fixture := newStDepositCustodyFixture(tb, operator)
			intent := stWalletRecoveryIntent(tb, client, 7, 1999, 1)
			attempt := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
			stWalletRecoveryInclude(fixture, attempt)
			var fail atomic.Bool
			fail.Store(true)
			url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
				if call.Method != "eth_getTransactionReceipt" || !fail.Load() {
					return false
				}
				fixture.stateLock.Lock()
				raw, err := json.Marshal(fixture.receiptKVs[common.HexToHash(attempt.TxHash)])
				fixture.stateLock.Unlock()
				var fields map[string]json.RawMessage
				if err != nil || json.Unmarshal(raw, &fields) != nil {
					tb.Error("cannot encode original fixture receipt")
					return true
				}
				if fault == "absent" {
					delete(fields, "status")
				} else {
					fields["status"] = json.RawMessage(`"0x2"`)
				}
				stWalletRecoveryReply(writer, call, fields, false)
				return true
			})
			client.cfg.RpcUrls = []string{url}
			client.clients = map[string]*ethclient.Client{url: endpoint}
			before := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
			_, err := client.DepositCredit(context.Background(), 7, operator, big.NewInt(100))
			retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
			if err == nil || !strings.Contains(err.Error(), "execution status") || retained.Status != model.StTxSigned || retained.Generation != intent.Generation || !reflect.DeepEqual(before, model.GetStTransactionAttempts(context.Background(), intent.IntentId)) || len(fixture.sent) != 0 {
				tb.Fatalf("malformed receipt status authorized another generation: fault=%s status=%s error=%v", fault, retained.Status, err)
			}
			fail.Store(false)
			if _, err := stWalletRecoveryRestart(client).DepositCredit(context.Background(), 7, operator, big.NewInt(100)); err != nil || model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxFinalized || len(fixture.sent) != 0 {
				tb.Fatal("valid original receipt did not recover malformed-status hold", err)
			}
		}
	})
}

// Expiry is considered only after the original receipt census. A principal
// that landed by its old deadline is reconciled, never canceled or sent again.
func TestStTransactionWalletExpiredDeadlineReconcilesOriginalWinnerFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 99, 1)
		original := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		stWalletRecoveryInclude(fixture, original)
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		retained := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if retained.Status != model.StTxFinalized || retained.AttemptCount != 1 || retained.CurrentTxHash == nil || *retained.CurrentTxHash != original.TxHash || len(fixture.sent) != 0 {
			tb.Fatal("expired deadline hid original canonical winner or repeated principal")
		}
	})
}

// A retry retains the first finalized hash. After a closing-read outage, a
// different hash at that height is hard even if a fresh head would look valid.
func TestStTransactionWalletRetryRetainsOriginalFinalizedBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 1)
		stWalletRecoveryInclude(fixture, model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId))
		var finalReads, closingReads atomic.Int32
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method != "eth_getBlockByNumber" || len(call.Params) == 0 {
				return false
			}
			if string(call.Params[0]) == `"finalized"` {
				if finalReads.Add(1) > 1 {
					stWalletRecoveryReply(writer, call, stTransactionReconcileBlock(101), false)
					return true
				}
			}
			if string(call.Params[0]) == `"0x64"` {
				if closingReads.Add(1) == 1 {
					writer.WriteHeader(http.StatusServiceUnavailable)
				} else {
					block := stTransactionReconcileBlock(100)
					block["hash"] = common.Hash{0xfb}.Hex()
					stWalletRecoveryReply(writer, call, block, false)
				}
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		now := time.Now()
		waits := 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
		before := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		_, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		if err == nil || !strings.Contains(err.Error(), "boundary changed") || finalReads.Load() != 1 || closingReads.Load() != 2 || waits != 1 || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(context.Background(), intent.IntentId)) || model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxMined || len(fixture.sent) != 0 {
			tb.Fatal("retry refreshed a contradicted original finalized boundary", err)
		}
	})
}

// Logical elapsed time exhausts the original read owner without sleeping. The
// public entry must retain signed custody and the actual transport cause.
func TestStTransactionWalletReadExhaustionRetainsOriginalAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		intent := stWalletRecoveryIntent(tb, client, 7, 1999, 1)
		var reads atomic.Int32
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_getTransactionReceipt" {
				reads.Add(1)
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		client.cfg.RpcUrls = []string{url}
		client.clients = map[string]*ethclient.Client{url: endpoint}
		now := time.Now()
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(context.Context, time.Duration) error { now = now.Add(300 * time.Second); return nil }}
		before := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		_, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "503") || reads.Load() != 1 || !reflect.DeepEqual(before, model.GetStTransactionAttempts(context.Background(), intent.IntentId)) || len(fixture.sent) != 0 {
			tb.Fatal("read exhaustion renewed its budget or erased original signed custody", err)
		}
	})
}

// Construction and nested receipt reads retain the production 300-second
// owner and at least 60 seconds per attempt, without extending a caller bound.
func TestStTransactionWalletReadAttemptAndNestedBudgets(t *testing.T) {
	client, endpoint, _ := newStDepositCustodyFixture(t, 1)
	if stReadOperationBudget != 300*time.Second || stReadAttemptBudget < 60*time.Second {
		t.Fatal("transaction read budget changed")
	}
	now := time.Now()
	client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(context.Context, time.Duration) error { now = now.Add(300 * time.Second); return nil }}
	owner, stop, err := beginStRpcRead(context.Background(), client.readHooks)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ownerDeadline, _ := owner.Deadline()
	calls := 0
	err = client.readTransactionRpc(owner, endpoint, func(ctx context.Context) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(ownerDeadline.Add(-(stReadOperationBudget - stReadAttemptBudget))) || deadline.After(ownerDeadline) {
			t.Fatal("original transaction read attempt did not receive its finite 60-second bound")
		}
		return io.ErrUnexpectedEOF
	})
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("nested transaction read renewed its original budget", err)
	}
}
