// Real HTTP reads use one finite owner; deterministic clocks exercise retry
// boundaries without waiting for a production timeout or replaying a send.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func stReadOwnerFixture(t *testing.T, transient int32) (*CoreStClient, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Id     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if request.Method != "eth_chainId" {
			t.Errorf("read owner attempted non-read method %s", request.Method)
			w.WriteHeader(400)
			return
		}
		if calls.Add(1) <= transient {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.Id, "result": "0x539"})
	}))
	t.Cleanup(endpoint.Close)
	client := &CoreStClient{cfg: &StConfig{ChainId: 1337, RpcUrls: []string{endpoint.URL}}, clients: map[string]*ethclient.Client{}}
	t.Cleanup(func() {
		client.stateLock.Lock()
		defer client.stateLock.Unlock()
		for _, c := range client.clients {
			c.Close()
		}
	})
	return client, calls
}

func TestStRpcReadOwnerDefaultAndNestedBudgetDoNotRestart(t *testing.T) {
	if stReadOperationBudget != 300*time.Second || stReadAttemptBudget < 60*time.Second || stDialTimeout < 60*time.Second || stCallTimeout < 300*time.Second || stSendTimeout != 60*time.Second {
		t.Fatal("read defaults or original send bound changed")
	}
	now := time.Unix(100, 0)
	ctx, cancel, err := beginStRpcRead(context.Background(), stRpcReadHooks{now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	scope := ctx.Value(stRpcReadScopeKey{}).(*stRpcReadScope)
	want := now.Add(300 * time.Second)
	now = now.Add(299 * time.Second)
	child, stop, err := beginStRpcRead(ctx, stRpcReadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if child != ctx || scope.deadline != want {
		t.Fatal("nested original read restarted its operation budget")
	}
	now = want
	if _, _, err := beginStRpcRead(ctx, stRpcReadHooks{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("exhausted original read owner was renewed", err)
	}
}

func TestStRpcReadOwnerActualTransientDialThenWholeReadRecovery(t *testing.T) {
	client, calls := stReadOwnerFixture(t, 1)
	now := time.Now()
	waits := 0
	client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() }}
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bodies := 0
	err := client.eachRpc(owner, func(ctx context.Context, c *ethclient.Client) error {
		bodies++
		id, err := c.ChainID(ctx)
		if err == nil && id.Uint64() != 1337 {
			return errors.New("synthetic chain mismatch")
		}
		return err
	})
	if err != nil || calls.Load() != 3 || waits != 1 || bodies != 1 {
		t.Fatal("transient dial did not recover exactly one whole original read", calls.Load(), waits, bodies, err)
	}
}

func TestStRpcReadOwnerRetriesCompleteBodyAndKeepsHardSibling(t *testing.T) {
	client, _ := stReadOwnerFixture(t, 0)
	now := time.Now()
	waits := 0
	client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(ctx context.Context, d time.Duration) error { waits++; now = now.Add(d); return ctx.Err() }}
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bodies := 0
	err := client.eachRpc(owner, func(ctx context.Context, c *ethclient.Client) error {
		bodies++
		if _, err := c.ChainID(ctx); err != nil {
			return err
		}
		if bodies == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err != nil || bodies != 2 || waits != 1 {
		t.Fatal("read retried a fragment instead of complete operation", bodies, waits, err)
	}
	hard := errors.New("original pinned policy conflict")
	bodies = 0
	waits = 0
	err = client.eachRpc(owner, func(context.Context, *ethclient.Client) error { bodies++; return errors.Join(io.EOF, hard) })
	if !errors.Is(err, hard) || bodies != 1 || waits != 0 {
		t.Fatal("coincident transport error hid original hard evidence", bodies, waits, err)
	}
}

func TestStRpcReadOwnerCancellationAndBudgetRetainPhysicalCause(t *testing.T) {
	client, _ := stReadOwnerFixture(t, 0)
	now := time.Now()
	client.readHooks = stRpcReadHooks{now: func() time.Time { return now }, wait: func(context.Context, time.Duration) error { now = now.Add(stReadOperationBudget); return nil }}
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	calls := 0
	err := client.eachRpc(owner, func(context.Context, *ethclient.Client) error { calls++; return io.ErrUnexpectedEOF })
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("read budget lost original physical cause", calls, err)
	}
	dead, stop := context.WithCancel(owner)
	stop()
	calls = 0
	err = client.eachRpc(dead, func(context.Context, *ethclient.Client) error { calls++; return nil })
	if calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled owner performed another original read", calls, err)
	}
}

type stReadCycle struct{}

func (*stReadCycle) Error() string      { return "cycle" }
func (self *stReadCycle) Unwrap() error { return self }

func TestStRpcReadOwnerBoundedCauseTreeAndTypedHttpStatus(t *testing.T) {
	if !retryableStRpcRead(rpc.HTTPError{StatusCode: 503}) || retryableStRpcRead(rpc.HTTPError{StatusCode: 401}) || retryableStRpcRead(&stReadCycle{}) || retryableStRpcRead(errors.Join(io.EOF, context.Canceled)) {
		t.Fatal("read cause classification lost bounded status/integrity precedence")
	}
	var typedNil *stReadCycle
	if retryableStRpcRead(typedNil) {
		t.Fatal("typed nil original cause was retried")
	}
}
