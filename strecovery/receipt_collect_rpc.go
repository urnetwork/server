// A collection owns one bounded read-only HTTP client. Retries keep the exact
// method/selector and consume the same deadline, request and response budgets.
package strecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/urfoundation/sn/protocol"
)

const maximumCollectionReplyBytes = 16 * 1024 * 1024
const maximumCollectionTotalReplyBytes = 128 * 1024 * 1024
const maximumCollectionRequests = 32768

var errReceiptCollectorRedirect = errors.New("receipt collector refuses redirects")

// No client/cache is shared across invocations. Native capture selects its own
// read profile and durable request/response hooks; the old collector has none.
// The private wait port allows deterministic retry cancellation in tests.
type receiptCollectorRpc struct {
	url         string
	client      *http.Client
	wait        func(context.Context, time.Duration) error
	requests    int
	remaining   int
	retryWindow time.Duration
	finality    bool
	beforeRead  func(context.Context, int, []byte) error
	afterRead   func(context.Context, int, int) error
}

// Credentials and redirect/proxy routes cannot silently change the explicit
// owned endpoint. Selection remains operator input, not source approval.
func collectionRpcUrl(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.Scheme != "http" && parsed.Scheme != "https" || len(raw) > 2048 {
		return errors.New("receipt collector requires an explicit credential-free HTTP(S) endpoint without query or fragment")
	}
	return nil
}

// Every connection and response has a fixed bound inside the total collection
// deadline. Automatic redirects, environment proxies and compression are off.
func newReceiptCollectorRpc(endpoint string) *receiptCollectorRpc {
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 * 1024, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, DisableCompression: true}
	return &receiptCollectorRpc{url: endpoint, remaining: maximumCollectionTotalReplyBytes, retryWindow: 300 * time.Second,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errReceiptCollectorRedirect
		}}, wait: func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}}
}

// Result decoding may use a projection, but duplicate/case-folded keys are
// rejected over the entire reply, including fields absent from that projection.
func (self *receiptCollectorRpc) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	if self.finality {
		switch method {
		case "chain_getBlockHash", "chain_getHeader", "chain_getBlock":
		default:
			return nil, errors.New("method is outside the native finality capture read profile")
		}
	} else {
		switch method {
		case "eth_chainId", "system_chain", "chain_getBlockHash", "eth_getBlockByNumber", "eth_getTransactionReceipt", "eth_getTransactionCount", "debug_getRawHeader", "debug_getRawBlock", "debug_getRawReceipts":
		default:
			return nil, errors.New("method is outside the receipt collection read profile")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, self.retryWindow)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if self.requests >= maximumCollectionRequests || self.remaining <= 0 {
			return nil, errors.New("receipt collection exhausted its shared request/response budget")
		}
		self.requests++
		id := self.requests
		payload, err := json.Marshal(struct {
			Version string `json:"jsonrpc"`
			Id      int    `json:"id"`
			Method  string `json:"method"`
			Params  []any  `json:"params"`
		}{Version: "2.0", Id: id, Method: method, Params: params})
		if err != nil {
			return nil, err
		}
		if self.beforeRead != nil {
			if err := self.beforeRead(ctx, id, payload); err != nil {
				return nil, err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.url, bytes.NewReader(payload))
		if err != nil {
			return nil, errors.New("receipt collection request construction failed")
		}
		request.Header.Set("Content-Type", "application/json")
		response, requestErr := self.client.Do(request)
		retry := false
		var raw []byte
		if requestErr != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if errors.Is(requestErr, errReceiptCollectorRedirect) {
				return nil, errReceiptCollectorRedirect
			}
			retry = true
		} else {
			limit := min(self.remaining, maximumCollectionReplyBytes)
			raw, err = io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
			closeErr := response.Body.Close()
			self.remaining -= len(raw)
			if len(raw) > limit {
				return nil, errors.New("receipt collection response exceeds its byte budget")
			}
			if err != nil || closeErr != nil {
				retry = true
			} else if response.StatusCode != http.StatusOK {
				retry = response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504
				if !retry {
					return nil, fmt.Errorf("receipt collection %s refused HTTP status %d", method, response.StatusCode)
				}
			}
		}
		if self.afterRead != nil {
			if err := self.afterRead(ctx, id, len(raw)); err != nil {
				return nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if retry {
			if err := self.wait(ctx, time.Second); err != nil {
				return nil, err
			}
			continue
		}
		if err := protocol.ValidateUniqueJsonKeys(raw); err != nil {
			return nil, errors.New("receipt collection reply is ambiguous or malformed")
		}
		var reply struct {
			Version string          `json:"jsonrpc"`
			Id      *int            `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil || reply.Version != "2.0" || reply.Id == nil || *reply.Id != id ||
			(len(reply.Result) == 0) == (len(reply.Error) == 0) {
			return nil, errors.New("receipt collection reply identity or result/error envelope differs")
		}
		if len(reply.Error) != 0 {
			// Capability and semantic errors never become null receipts and do
			// not retry. In particular unsupported raw reads cannot downgrade.
			return nil, fmt.Errorf("receipt collection %s returned an RPC error; required read capability is unavailable", method)
		}
		return reply.Result, nil
	}
}

// Mandatory reads must not silently decode null as a zero value. Receipt null
// is handled explicitly at its sole permitted call site.
func (self *receiptCollectorRpc) read(ctx context.Context, method string, params []any, result any) error {
	raw, err := self.call(ctx, method, params)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, result) != nil {
		return fmt.Errorf("receipt collection %s result is unavailable or malformed", method)
	}
	return nil
}
