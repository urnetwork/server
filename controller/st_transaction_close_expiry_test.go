// Close expiry authorizes cancellation of an original signed transaction.
// Public nonce recovery must retain the endpoint and hash that proved its nonce.
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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Only exact wire observations differ. All signed sends, receipt decoding and
// durable state changes use the existing real transaction recovery surface.
type stCloseExpiryRpc struct {
	t                  testing.TB
	base               *stTransactionReconcileRPC
	policyData         []byte
	endData            []byte
	policyResult       hexutil.Bytes
	originalEnd        uint64
	stateLock          sync.Mutex
	fault              string
	policyReads        int
	endReads           int
	finalizedReads     int
	closingReads       int
	canonicalCalls     int
	otherSelectorCalls int
	foreignReads       int
	cancelForeign      context.CancelFunc
}

// Both the policy and end returned here are ABI bytes, never an expiry verdict.
// Numeric selectors can expose another fork while the original hash stays fixed.
func (self *stCloseExpiryRpc) serve(writer http.ResponseWriter, request *http.Request, foreign bool) {
	raw, err := io.ReadAll(request.Body)
	_ = request.Body.Close()
	if err != nil {
		self.t.Error(err)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(raw))
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		self.base.ServeHTTP(writer, request)
		return
	}
	var call stEpochRPCRequest
	if err := json.Unmarshal(raw, &call); err != nil {
		self.t.Error(err)
		return
	}
	var result any
	var handled, unavailable bool
	var cancel context.CancelFunc
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if foreign {
			self.foreignReads++
			cancel = self.cancelForeign
		}
		if call.Method == "eth_getBlockByNumber" && len(call.Params) == 2 {
			var tag string
			if json.Unmarshal(call.Params[0], &tag) != nil {
				self.t.Error("malformed close fixture block selector")
				return
			}
			if tag == "finalized" {
				self.finalizedReads++
				if self.fault == "retry-closing" && self.finalizedReads > 1 {
					result, handled = stTransactionReconcileBlock(101), true
				}
			} else if tag == "0x64" && self.endReads > 0 {
				self.closingReads++
				if self.fault == "retry-closing" && self.closingReads == 1 {
					unavailable, handled = true, true
				} else if self.fault == "closing" && self.closingReads == 1 || self.fault == "retry-closing" && self.closingReads == 2 {
					block := stTransactionReconcileBlock(100)
					block["hash"] = common.Hash{0xda}.Hex()
					result, handled = block, true
				}
			}
			return
		}
		if call.Method != "eth_call" || len(call.Params) != 2 {
			return
		}
		var args struct {
			To    common.Address `json:"to"`
			Data  hexutil.Bytes  `json:"data"`
			Input hexutil.Bytes  `json:"input"`
		}
		if json.Unmarshal(call.Params[0], &args) != nil {
			self.t.Error("malformed close fixture contract call")
			return
		}
		data := args.Data
		if len(data) == 0 {
			data = args.Input
		}
		policy := bytes.Equal(data, self.policyData)
		if !policy && !bytes.Equal(data, self.endData) {
			return
		}
		var selector rpc.BlockNumberOrHash
		canonical := json.Unmarshal(call.Params[1], &selector) == nil && selector.BlockHash != nil && *selector.BlockHash == stTransactionReconcileBlockHash(100) && selector.BlockNumber == nil && selector.RequireCanonical
		if canonical {
			self.canonicalCalls++
		} else {
			self.otherSelectorCalls++
		}
		handled = true
		if policy {
			self.policyReads++
			if self.fault == "soft-policy" && self.policyReads == 1 && !foreign {
				unavailable = true
				return
			}
			result = self.policyResult
		} else {
			self.endReads++
			end := self.originalEnd
			if foreign || self.fault == "numeric-fork" && !canonical {
				end = 80
			}
			result = hexutil.Bytes(new(big.Int).SetUint64(end).FillBytes(make([]byte, 32)))
		}
	}()
	if cancel != nil {
		cancel()
	}
	if !handled {
		self.base.ServeHTTP(writer, request)
		return
	}
	_ = request.Body.Close()
	if unavailable {
		writer.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	stWalletRecoveryReply(writer, call, result, false)
}

// An original execution is signed and persisted before either route is read.
// The second route can serve contradictory same-height policy bytes but never
// acquires custody of the selected account nonce or transaction endpoint.
func newStCloseExpiryFixture(t testing.TB, fault string, expired bool) (*CoreStClient, *model.StTransactionIntent, *stCloseExpiryRpc) {
	t.Helper()
	cfg := stTransactionReconcileConfig()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("synthetic close expiry " + t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RootKey, cfg.NoId = key, 1
	coordinator := stabi.NewSTCoordinator()
	epoch := big.NewInt(7)
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := parsed.Methods["policyAt"].Outputs.Pack(stabi.STCoordinatorPolicySnapshot{CloseGraceBlocks: 10, EpochDepositCapRao: big.NewInt(1000), CampaignDepositCapRao: big.NewInt(10000)})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &stCloseExpiryRpc{t: t, base: &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 7, receipts: map[string]*types.Receipt{}}, policyData: coordinator.PackPolicyAt(epoch), endData: coordinator.PackEpochEndBlock(epoch), policyResult: policy, originalEnd: 200, fault: fault}
	if expired {
		fixture.originalEnd = 80
	}
	selected := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fixture.serve(writer, request, false) }))
	foreign := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fixture.serve(writer, request, true) }))
	cfg.RpcUrls = []string{selected.URL, foreign.URL}
	client := &CoreStClient{cfg: cfg, coordinator: coordinator, clients: map[string]*ethclient.Client{}}
	t.Cleanup(func() {
		var clients []*ethclient.Client
		func() {
			client.stateLock.Lock()
			defer client.stateLock.Unlock()
			for _, endpoint := range client.clients {
				clients = append(clients, endpoint)
			}
			clear(client.clients)
		}()
		for _, endpoint := range clients {
			endpoint.Close()
		}
		selected.Close()
		foreign.Close()
	})
	from := crypto.PubkeyToAddress(key.PublicKey)
	calldata := coordinator.PackCloseOperatorEpoch(epoch, big.NewInt(1))
	logical, err := stTransactionLogicalKey(cfg, "close:7:1")
	if err != nil {
		t.Fatal(err)
	}
	intent := model.ReserveStTransactionIntent(t.Context(), logical, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, "0x"+hex.EncodeToString(cfg.GenesisHash[:]), strings.ToLower(from.Hex()), strings.ToLower(cfg.ContractAddress.Hex()), crypto.Keccak256Hash(calldata).Hex(), calldata, 7)
	signed, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, To: &cfg.ContractAddress, Gas: 50000, GasPrice: big.NewInt(100), Value: new(big.Int), Data: calldata}), types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	price := "100"
	model.AddStTransactionAttempt(t.Context(), &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution, TxHash: signed.Hash().Hex(), RawTransaction: raw, GasLimit: signed.Gas(), GasPrice: &price})
	return client, model.GetStTransactionIntent(t.Context(), logical), fixture
}

// A numeric same-height fork says expired, while the original canonical block
// says this signed close remains valid. Only the original execution may resume.
func TestStCloseExpiryUsesOriginalHashForPublicNonceRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		client, intent, fixture := newStCloseExpiryFixture(t, "numeric-fork", false)
		before := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		hash, err := client.CloseOperatorEpoch(t.Context(), 7, 1)
		if err != nil {
			t.Fatal("a numeric fork displaced the original valid close", err)
		}
		retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
		if retained.Status != model.StTxFinalized || retained.AttemptCount != 1 || hash != before[0].TxHash || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(t.Context(), intent.IntentId)) {
			t.Fatal("close expiry replaced its original signed execution")
		}
		func() {
			fixture.base.stateLock.Lock()
			defer fixture.base.stateLock.Unlock()
			if len(fixture.base.sent) != 1 || fixture.base.sent[0].Hash().Hex() != before[0].TxHash {
				t.Fatal("close expiry sent a cancellation or another signature")
			}
		}()
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if fixture.canonicalCalls != 2 || fixture.otherSelectorCalls != 0 || fixture.foreignReads != 0 {
			t.Fatal("expiry did not retain the original canonical read selector")
		}
	})
}

// A real policy503 retries the complete read at the selected route. Reaching
// the contradictory route cancels explicitly, so no timeout is the test oracle.
func TestStCloseExpiryTransientReadStaysOnSelectedEndpoint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		client, intent, fixture := newStCloseExpiryFixture(t, "soft-policy", false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture.cancelForeign = cancel
		now, waits := time.Now(), 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
		if _, err := client.CloseOperatorEpoch(ctx, 7, 1); err != nil {
			t.Fatal("selected close expiry read did not recover its policy", err)
		}
		retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if waits != 1 || fixture.policyReads != 2 || fixture.endReads != 1 || fixture.canonicalCalls != 3 || fixture.otherSelectorCalls != 0 || fixture.foreignReads != 0 || retained.Status != model.StTxFinalized || retained.AttemptCount != 1 {
			t.Fatal("transient expiry read borrowed another endpoint or signed a replacement")
		}
	})
}

// A closing hash contradiction holds the original signed liability. Once the
// original proof is restored, public defer cancels nonce7 and only then uses8.
func TestStCloseExpiryClosingConflictPreservesSignedAttempt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		client, intent, fixture := newStCloseExpiryFixture(t, "closing", true)
		before := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		_, err := client.DeferMissedEmission(t.Context(), 7, 1)
		if err == nil || !strings.Contains(err.Error(), "boundary changed during close expiry") {
			t.Fatal("changed expiry boundary authorized nonce cancellation", err)
		}
		if retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey); retained.Status != model.StTxSigned || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(t.Context(), intent.IntentId)) {
			t.Fatal("contradicted expiry changed original signed custody")
		}
		logical, err := stTransactionLogicalKey(client.cfg, "defer:7:1")
		if err != nil || model.GetStTransactionIntent(t.Context(), logical) != nil {
			t.Fatal("contradicted expiry reserved the following nonce", err)
		}
		fixture.base.stateLock.Lock()
		sends := len(fixture.base.sent)
		fixture.base.stateLock.Unlock()
		if sends != 0 {
			t.Fatal("contradicted expiry broadcast signed work")
		}
		fixture.stateLock.Lock()
		fixture.fault = ""
		fixture.stateLock.Unlock()
		if _, err := client.DeferMissedEmission(t.Context(), 7, 1); err != nil {
			t.Fatal("restored original expiry did not unblock public defer", err)
		}
		attempts := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
		next := model.GetStTransactionIntent(t.Context(), logical)
		if retained.Status != model.StTxCanceled || len(attempts) != 2 || attempts[0].Kind != model.StTxAttemptCancellation || !bytes.Equal(attempts[1].RawTransaction, before[0].RawTransaction) || next == nil || next.Status != model.StTxFinalized || next.Nonce != 8 {
			t.Fatal("restored close expiry lost original cancellation/next-nonce ordering")
		}
		fixture.base.stateLock.Lock()
		defer fixture.base.stateLock.Unlock()
		if len(fixture.base.sent) != 2 || fixture.base.sent[0].Nonce() != 7 || fixture.base.sent[0].To() == nil || *fixture.base.sent[0].To() != crypto.PubkeyToAddress(client.cfg.RootKey.PublicKey) || len(fixture.base.sent[0].Data()) != 0 || fixture.base.sent[1].Nonce() != 8 || !bytes.Equal(fixture.base.sent[1].Data(), client.coordinator.PackDeferMissedEmission(big.NewInt(7), big.NewInt(1))) {
			t.Fatal("restored expiry did not preserve exact cancellation then defer bytes")
		}
	})
}

// A closing-read outage cannot refresh the original nonce boundary. The next
// complete read must still catch its changed hash even if a newer head exists.
func TestStCloseExpiryRetryRetainsOriginalNonceBoundary(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		client, intent, fixture := newStCloseExpiryFixture(t, "retry-closing", true)
		before := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		now, waits := time.Now(), 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
		_, err := client.DeferMissedEmission(t.Context(), 7, 1)
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "boundary changed during close expiry") {
			t.Fatal("expiry retry refreshed or hid the original contradicted boundary", err)
		}
		retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
		if retained.Status != model.StTxSigned || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(t.Context(), intent.IntentId)) {
			t.Fatal("expiry retry changed original signed custody")
		}
		func() {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			if waits != 1 || fixture.finalizedReads != 1 || fixture.policyReads != 2 || fixture.endReads != 2 || fixture.closingReads != 2 || fixture.canonicalCalls != 4 || fixture.otherSelectorCalls != 0 || fixture.foreignReads != 0 {
				t.Fatal("expiry retry did not reread the complete original dependency")
			}
		}()
		fixture.base.stateLock.Lock()
		defer fixture.base.stateLock.Unlock()
		if len(fixture.base.sent) != 0 {
			t.Fatal("expiry retry broadcast after a closing contradiction")
		}
	})
}
