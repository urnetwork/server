// Public deposit calls must recover their read-only endpoint handshake before
// they can reconcile, reserve or broadcast an original durable transaction.
package controller

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The actual Http fixture supplies every authority, nonce and receipt read.
// Emptying only the client's connection cache forces the real chain handshake.
func stTransactionEndpointFixture(t testing.TB, operator uint64, hook func(http.ResponseWriter, stEpochRPCRequest) bool) (*CoreStClient, *stDepositCustodyRpc) {
	t.Helper()
	client, _, fixture := newStDepositCustodyFixture(t, operator)
	url, _ := stWalletRecoveryEndpoint(t, fixture, hook)
	client.cfg.RpcUrls = []string{url}
	client.clients = map[string]*ethclient.Client{}
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
	})
	return client, fixture
}

// A transient first handshake occurs before any nonce or signing authority.
// Its read retry reaches one real deposit and one persisted signed attempt.
func TestStTransactionEndpointTransientHandshakeReachesSingleDeposit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var handshakes atomic.Int32
		client, fixture := stTransactionEndpointFixture(t, 1, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_chainId" && handshakes.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		fixture.staged.SetInt64(102)
		now, waits := time.Now(), 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, _ time.Duration) error {
			waits++
			now = now.Add(61 * time.Second)
			return ctx.Err()
		}}
		hash, err := client.DepositCredit(t.Context(), 7, 1, big.NewInt(100))
		if err != nil {
			t.Fatal("transient endpoint handshake prevented the actual deposit", err)
		}
		logical, err := stTransactionLogicalKey(client.cfg, "deposit:7:1:3")
		if err != nil {
			t.Fatal(err)
		}
		intent := model.GetStTransactionIntent(t.Context(), logical)
		if intent == nil || intent.Status != model.StTxFinalized || intent.AttemptCount != 1 || intent.Nonce != 7 {
			t.Fatal("endpoint read retry did not retain one finalized intent", intent)
		}
		attempts := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if handshakes.Load() != 2 || waits != 1 || len(attempts) != 1 || len(fixture.sent) != 1 || hash != attempts[0].TxHash || fixture.sent[0].Hash().Hex() != hash || fixture.sent[0].Nonce() != 7 || fixture.accountNonce != 8 || fixture.deposited.Int64() != 100 || fixture.depositNonce.Int64() != 4 {
			t.Fatal("handshake retry changed original nonce, signature or principal")
		}
	})
}

// Lost process memory must not turn a retried handshake into a repeated send.
// The original canonical receipt remains the sole completed deposit authority.
func TestStTransactionEndpointTransientHandshakeRecoversStoredDeposit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var handshakes atomic.Int32
		client, fixture := stTransactionEndpointFixture(t, 1, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_chainId" && handshakes.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		intent := stWalletRecoveryIntent(t, client, 7, 1999, 1)
		before := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		stWalletRecoveryInclude(fixture, before[0])
		now, waits := time.Now(), 0
		client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error {
			waits++
			now = now.Add(delay)
			return ctx.Err()
		}}
		if _, err := client.DepositCredit(t.Context(), 7, 1, big.NewInt(100)); err != nil {
			t.Fatal("fresh endpoint failed to reconcile the original deposit", err)
		}
		retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
		after := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if handshakes.Load() != 2 || waits != 1 || retained.Status != model.StTxFinalized || retained.CurrentTxHash == nil || *retained.CurrentTxHash != before[0].TxHash || retained.AttemptCount != 1 || !stWalletRecoveryOriginalsEqual(before, after) || len(fixture.sent) != 0 || fixture.deposited.Int64() != 100 || fixture.depositNonce.Int64() != 4 {
			t.Fatal("endpoint recovery rewrote or repeated the original deposit")
		}
	})
}

// A validly framed different chain remains a hard refusal before preparation.
// Once the original chain is restored, a new call can read the retained winner.
func TestStTransactionEndpointHardHandshakePreservesStoredAttempt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var handshakes, otherReads atomic.Int32
		var wrongChain atomic.Bool
		wrongChain.Store(true)
		client, fixture := stTransactionEndpointFixture(t, 1, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method != "eth_chainId" {
				otherReads.Add(1)
				return false
			}
			handshakes.Add(1)
			if wrongChain.Load() {
				stWalletRecoveryReply(writer, call, hexutil.EncodeUint64(946), false)
				return true
			}
			return false
		})
		intent := stWalletRecoveryIntent(t, client, 7, 1999, 1)
		before := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
		waits := 0
		client.readHooks = stRpcReadHooks{wait: func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }}
		_, err := client.DepositCredit(t.Context(), 7, 1, big.NewInt(100))
		retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
		if err == nil || !strings.Contains(err.Error(), "reports chain id") || handshakes.Load() != 1 || otherReads.Load() != 0 || waits != 0 || retained.Status != intent.Status || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(t.Context(), intent.IntentId)) {
			t.Fatal("hard endpoint identity was retried or changed signed custody", err)
		}
		wrongChain.Store(false)
		stWalletRecoveryInclude(fixture, before[0])
		if _, err := client.DepositCredit(t.Context(), 7, 1, big.NewInt(100)); err != nil {
			t.Fatal("restored endpoint did not recover original receipt", err)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if handshakes.Load() != 2 || waits != 0 || len(fixture.sent) != 0 {
			t.Fatal("restored chain repeated a retained signature")
		}
	})
}

// The first real failed handshake advances a logical clock or cancels its
// parent. Neither terminal edge may restart the read owner or touch signed work.
func TestStTransactionEndpointHandshakeBoundRetainsOriginalAttempt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for index, canceled := range []bool{false, true} {
			var handshakes, otherReads atomic.Int32
			operator := uint64(index + 1)
			client, fixture := stTransactionEndpointFixture(t, operator, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
				if call.Method != "eth_chainId" {
					otherReads.Add(1)
					return false
				}
				handshakes.Add(1)
				writer.WriteHeader(http.StatusServiceUnavailable)
				return true
			})
			intent := stWalletRecoveryIntent(t, client, 7, 1999, 1)
			before := model.GetStTransactionAttempts(t.Context(), intent.IntentId)
			ctx, cancel := context.WithCancel(t.Context())
			now, waits := time.Now(), 0
			client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, _ time.Duration) error {
				waits++
				if canceled {
					cancel()
				} else {
					now = now.Add(300 * time.Second)
				}
				return ctx.Err()
			}}
			_, err := client.DepositCredit(ctx, 7, operator, big.NewInt(100))
			cancel()
			want := context.DeadlineExceeded
			if canceled {
				want = context.Canceled
			}
			retained := model.GetStTransactionIntent(t.Context(), intent.LogicalKey)
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "503") || handshakes.Load() != 1 || otherReads.Load() != 0 || waits != 1 || retained.Status != intent.Status || !stWalletRecoveryOriginalsEqual(before, model.GetStTransactionAttempts(t.Context(), intent.IntentId)) {
				t.Fatalf("endpoint handshake lost its finite owner or signed custody: canceled=%t error=%v", canceled, err)
			}
			fixture.stateLock.Lock()
			sends := len(fixture.sent)
			fixture.stateLock.Unlock()
			if sends != 0 {
				t.Fatal("failed endpoint handshake reached a transaction send")
			}
		}
	})
}
