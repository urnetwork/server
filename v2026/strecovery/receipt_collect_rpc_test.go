// Transport controls force repeated timeouts, exact request budgets and bad
// envelopes without contacting any host or waiting on a wall-clock schedule.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The function adapter is private to tests; production always owns its HTTP
// transport and never accepts a signer, arbitrary RPC port or alternate proxy.
type collectionTestRoundTripper func(*http.Request) (*http.Response, error)

// Implements the standard transport interface without opening a connection.
func (self collectionTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// Return a genuine timeout classification while keeping test timing explicit.
type collectionTestTimeout struct{}

// The synthetic text contains no endpoint or private payload.
func (collectionTestTimeout) Error() string { return "synthetic transport timeout" }

// Read retries handle network timeouts rather than treating them as null reads.
func (collectionTestTimeout) Timeout() bool { return true }

// Satisfy the legacy net.Error shape used by the HTTP client.
func (collectionTestTimeout) Temporary() bool { return true }

// More than three network timeouts recover under one 300-second read window;
// each new request preserves the original method and exact block selector.
func TestReceiptCollectorRpcRetriesMultipleTransportTimeouts(t *testing.T) {
	client := newReceiptCollectorRpc("http://rpc.example")
	calls, waits := 0, 0
	client.client.Transport = collectionTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Method != "debug_getRawHeader" || len(payload.Params) != 1 || !strings.Contains(string(payload.Params[0]), `"requireCanonical":true`) {
			t.Fatal("retry changed original read selector")
		}
		if calls <= 7 {
			return nil, collectionTestTimeout{}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"0xc0"}`, payload.Id))), Header: http.Header{}}, nil
	})
	client.wait = func(ctx context.Context, _ time.Duration) error {
		waits++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < time.Minute {
			t.Fatal("read lost its required minimum retry window")
		}
		return nil
	}
	var raw string
	err := client.read(context.Background(), "debug_getRawHeader", []any{collectionBlockSelector("0x" + strings.Repeat("a", 64))}, &raw)
	if err != nil || raw != "0xc0" || calls != 8 || waits != 7 {
		t.Fatalf("transport did not recover: calls %d waits %d error %v", calls, waits, err)
	}
}

// Every retry consumes the same request budget, including failed requests.
func TestReceiptCollectorRpcExhaustsSharedRequestBudget(t *testing.T) {
	client := newReceiptCollectorRpc("http://rpc.example")
	client.requests = maximumCollectionRequests - 2
	calls := 0
	client.client.Transport = collectionTestRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		if calls > 2 {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("x")), Header: http.Header{}}, nil
		}
		return nil, collectionTestTimeout{}
	})
	client.wait = func(context.Context, time.Duration) error { return nil }
	raw, err := client.call(context.Background(), "eth_chainId", []any{})
	if err == nil || raw != nil || calls != 2 || !strings.Contains(err.Error(), "shared request/response budget") {
		t.Fatalf("request allowance reset on retry: calls %d error %v", calls, err)
	}
}

// Failed status bodies count too. The response limiter consumes at most one
// extra byte to detect excess and never resets its total budget on the retry.
func TestReceiptCollectorRpcExhaustsSharedResponseBudget(t *testing.T) {
	client := newReceiptCollectorRpc("http://rpc.example")
	client.remaining = 5
	calls := 0
	client.client.Transport = collectionTestRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		if calls > 2 {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("x")), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader("four")), Header: http.Header{}}, nil
	})
	client.wait = func(context.Context, time.Duration) error { return nil }
	raw, err := client.call(context.Background(), "eth_chainId", []any{})
	if err == nil || raw != nil || calls != 2 || !strings.Contains(err.Error(), "byte budget") {
		t.Fatalf("response allowance reset on retry: calls %d error %v", calls, err)
	}
}

// Contradictions, capability errors and duplicate keys are terminal, unlike
// timeouts. A retry must not hide an integrity failure behind a later reply.
func TestReceiptCollectorRpcRefusesAmbiguousOrUnsupportedReplies(t *testing.T) {
	for _, reply := range []string{
		`{"jsonrpc":"2.0","id":1,"result":"0x1","Result":"0x2"}`,
		`{"jsonrpc":"2.0","id":2,"result":"0x1"}`,
		`{"jsonrpc":"2.0","id":1,"result":null,"error":{"code":-32601}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"unsupported"}}`,
	} {
		client := newReceiptCollectorRpc("http://rpc.example")
		calls := 0
		client.client.Transport = collectionTestRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
		})
		client.wait = func(context.Context, time.Duration) error {
			t.Fatal("integrity/capability failure retried")
			return nil
		}
		raw, err := client.call(context.Background(), "debug_getRawReceipts", []any{collectionBlockSelector("0x" + strings.Repeat("a", 64))})
		if err == nil || raw != nil || calls != 1 {
			t.Fatalf("bad reply accepted or retried: %v", err)
		}
	}
}

// Cancellation wins even after a transport completed a syntactically valid
// reply. Mutation methods fail before any transport or budget is touched.
func TestReceiptCollectorRpcCancellationAndMethodAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newReceiptCollectorRpc("http://rpc.example")
	calls := 0
	client.client.Transport = collectionTestRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)), Header: http.Header{}}, nil
	})
	if raw, err := client.call(ctx, "eth_sendRawTransaction", []any{"0x01"}); err == nil || raw != nil || calls != 0 || client.requests != 0 {
		t.Fatal("mutation method reached transport")
	}
	if raw, err := client.call(ctx, "eth_chainId", []any{}); !errors.Is(err, context.Canceled) || raw != nil || calls != 1 {
		t.Fatalf("late cancellation published reply: %v", err)
	}
}
