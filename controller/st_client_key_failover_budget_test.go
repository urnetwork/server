// Endpoint failover shares one Rpc work allowance. Raw Http counters prove
// that failed final witnesses remain charged on success, refusal and cancel.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/stabi"
)

// Counts real batch requests and methods before optionally refusing a response.
// Every counter is safe to inspect while the endpoint is held at a barrier.
type stClientKeyBudgetEndpoint struct {
	url      string
	requests atomic.Uint64
	methods  atomic.Uint64
}

// The existing ABI fixture supplies complete authority; only the Http boundary
// is replaced, so a late failure still spends all of that attempt's methods.
func newStClientKeyBudgetEndpoint(t *testing.T, fixture *stClientKeyHistoryRPCFixture, refuse func(uint64, http.ResponseWriter, *http.Request) bool) *stClientKeyBudgetEndpoint {
	t.Helper()
	owner := &stClientKeyBudgetEndpoint{}
	rpcServer := gethrpc.NewServer()
	for _, namespace := range []string{"eth", "chain"} {
		if err := rpcServer.RegisterName(namespace, fixture); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var methods []json.RawMessage
		if err := json.Unmarshal(body, &methods); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		owner.methods.Add(uint64(len(methods)))
		attempt := owner.requests.Add(1)
		if refuse != nil && refuse(attempt, w, request) {
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		rpcServer.ServeHTTP(w, request)
	}))
	owner.url = endpoint.URL
	t.Cleanup(func() {
		endpoint.Close()
		rpcServer.Stop()
	})
	return owner
}

// Read the dependency directly to establish exact spent bytes independently
// of the failover wrapper whose accounting this regression checks.
func stClientKeyBudgetAttempt(t *testing.T, endpoint *stClientKeyBudgetEndpoint, queries []stabi.ClientKeyAuthorityQuery, limits stabi.ClientKeyAuthorityRpcLimits) (stabi.ClientKeyAuthorityRpcWork, error) {
	t.Helper()
	client, err := ethclient.DialContext(t.Context(), endpoint.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, work, err := stabi.ReadClientKeyAuthoritiesContext(t.Context(), client, queries, limits)
	endpoint.requests.Store(0)
	endpoint.methods.Store(0)
	return work, err
}

// A complete attempt fits each allowance alone, but a late failed witness
// leaves too little for another complete attempt. No spent unit is refunded.
func TestStClientKeyFailoverConservesEachWorkBudget(t *testing.T) {
	fixture, _ := newStClientKeyFallbackEndpoint(t, "")
	failed := newStClientKeyBudgetEndpoint(t, fixture, func(request uint64, w http.ResponseWriter, _ *http.Request) bool {
		if request == 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	healthy := newStClientKeyBudgetEndpoint(t, fixture, nil)
	queries, limits := stClientKeyFallbackBatchQueries(fixture)
	failedWork, err := stClientKeyBudgetAttempt(t, failed, queries, limits)
	if err == nil || failedWork.Requests != 4 || failedWork.Bytes == 0 {
		t.Fatal("late failure did not spend the intended witness work", failedWork, err)
	}
	healthyWork, err := stClientKeyBudgetAttempt(t, healthy, queries, limits)
	if err != nil || healthyWork.Requests != 4 {
		t.Fatal("complete-authority control failed", healthyWork, err)
	}
	cases := []struct {
		name   string
		limits stabi.ClientKeyAuthorityRpcLimits
	}{
		{name: "requests", limits: stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: 7, MaximumMethods: limits.MaximumMethods, MaximumBytes: limits.MaximumBytes}},
		{name: "exhausted requests", limits: stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: failedWork.Requests, MaximumMethods: limits.MaximumMethods, MaximumBytes: limits.MaximumBytes}},
		{name: "methods", limits: stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: limits.MaximumRequests, MaximumMethods: failedWork.Methods + healthyWork.Methods - 1, MaximumBytes: limits.MaximumBytes}},
		{name: "bytes", limits: stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: limits.MaximumRequests, MaximumMethods: limits.MaximumMethods, MaximumBytes: failedWork.Bytes + healthyWork.Bytes - 1}},
	}
	for _, test := range cases {
		failed.requests.Store(0)
		failed.methods.Store(0)
		healthy.requests.Store(0)
		healthy.methods.Store(0)
		authorities, work, err := readStClientKeyAuthoritiesFailover(t.Context(), []string{failed.url, healthy.url}, queries, test.limits)
		requests := failed.requests.Load() + healthy.requests.Load()
		methods := failed.methods.Load() + healthy.methods.Load()
		if err == nil || authorities != nil || work.Requests != requests || work.Methods != methods || work.Bytes < failedWork.Bytes ||
			work.Requests > test.limits.MaximumRequests || work.Methods > test.limits.MaximumMethods || work.Bytes > test.limits.MaximumBytes {
			t.Fatalf("%s allowance refunded failed work: sent=%d/%d spent=%+v limits=%+v authorities=%d err=%v", test.name, requests, methods, work, test.limits, len(authorities), err)
		}
	}
}

// Sufficient total capacity preserves healthy failover, and all-failed paths
// report the same cumulative work instead of returning an empty account.
func TestStClientKeyFailoverReportsAllSpentWork(t *testing.T) {
	fixture, _ := newStClientKeyFallbackEndpoint(t, "")
	refuse := func(request uint64, w http.ResponseWriter, _ *http.Request) bool {
		if request == 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	first := newStClientKeyBudgetEndpoint(t, fixture, refuse)
	healthy := newStClientKeyBudgetEndpoint(t, fixture, nil)
	failed := newStClientKeyBudgetEndpoint(t, fixture, refuse)
	queries, limits := stClientKeyFallbackBatchQueries(fixture)
	firstWork, firstErr := stClientKeyBudgetAttempt(t, first, queries, limits)
	healthyWork, healthyErr := stClientKeyBudgetAttempt(t, healthy, queries, limits)
	failedWork, failedErr := stClientKeyBudgetAttempt(t, failed, queries, limits)
	if firstErr == nil || failedErr == nil || healthyErr != nil {
		t.Fatal("direct endpoint controls differ from their intended outcomes", firstErr, healthyErr, failedErr)
	}
	for _, test := range []struct {
		endpoint *stClientKeyBudgetEndpoint
		work     stabi.ClientKeyAuthorityRpcWork
		healthy  bool
	}{
		{endpoint: healthy, work: healthyWork, healthy: true},
		{endpoint: failed, work: failedWork},
	} {
		first.requests.Store(0)
		first.methods.Store(0)
		authorities, work, err := readStClientKeyAuthoritiesFailover(t.Context(), []string{first.url, test.endpoint.url}, queries, limits)
		want := stabi.ClientKeyAuthorityRpcWork{Requests: firstWork.Requests + test.work.Requests, Methods: firstWork.Methods + test.work.Methods, Bytes: firstWork.Bytes + test.work.Bytes}
		if work != want || (err == nil) != test.healthy {
			t.Fatalf("cumulative failover work=%+v, want %+v, healthy=%t err=%v", work, want, test.healthy, err)
		}
		if test.healthy {
			if len(authorities) != 1 || authorities[0].Operator.RootSigner != fixture.root || authorities[0].Query != queries[0] {
				t.Fatal("healthy fallback lost complete endpoint authority", authorities)
			}
		} else if authorities != nil {
			t.Fatal("failed endpoints returned partial authority", authorities)
		}
	}
}

// Cancellation is forced after three positive batches and admission of the
// final witness. The next endpoint must receive no request, and spent work stays.
func TestStClientKeyFailoverCancellationRetainsSpentWork(t *testing.T) {
	fixture, _ := newStClientKeyFallbackEndpoint(t, "")
	entered := make(chan struct{})
	release := make(chan struct{})
	first := newStClientKeyBudgetEndpoint(t, fixture, func(request uint64, w http.ResponseWriter, _ *http.Request) bool {
		if request == 4 {
			close(entered)
			<-release
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	healthy := newStClientKeyBudgetEndpoint(t, fixture, nil)
	queries, limits := stClientKeyFallbackBatchQueries(fixture)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var work stabi.ClientKeyAuthorityRpcWork
	var err error
	go func() {
		_, work, err = readStClientKeyAuthoritiesFailover(ctx, []string{first.url, healthy.url}, queries, limits)
		close(done)
	}()
	<-entered
	cancel()
	close(release)
	<-done
	if !errors.Is(err, context.Canceled) || work.Requests != 4 || work.Methods != first.methods.Load() || work.Bytes == 0 || healthy.requests.Load() != 0 {
		t.Fatal("cancellation refunded work or started another endpoint", work, healthy.requests.Load(), err)
	}
}
