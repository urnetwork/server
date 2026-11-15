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
)

const maximumCollectionReplyBytes = 16 * 1024 * 1024
const maximumCollectionTotalReplyBytes = 128 * 1024 * 1024
const maximumCollectionRequests = 32768

var errReceiptCollectorRedirect = errors.New("receipt collector refuses redirects")

// No client/cache is shared across invocations. Native capture selects its own
// read profile and durable request/response hooks; the old collector has none.
// The private wait port allows deterministic retry cancellation in tests.
type receiptCollectorRpc struct {
	url              string
	client           *http.Client
	wait             func(context.Context, time.Duration) error
	requests         int
	remaining        int
	retryWindow      time.Duration
	finality         bool
	requiredFinality bool
	nativeExecution  bool
	storage          bool
	nativeProof      bool
	beforeRead       func(context.Context, int, []byte) error
	afterRead        func(context.Context, int, int) error
	validateResult   func(json.RawMessage) error
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
func (self *receiptCollectorRpc) call(ctx context.Context, method string, params []any) (result json.RawMessage, resultErr error) {
	var lastRequest, lastReply []byte
	var lastId, lastStatus int
	var lastBodyReadComplete bool
	lastReason := "native proof request did not produce usable evidence"
	defer func() {
		if self.nativeProof && resultErr != nil && lastId != 0 {
			resultErr = nativeExecutionProofReadFailure(self.url, method, lastId, lastStatus, lastRequest, lastReply, lastBodyReadComplete, lastReason, resultErr)
		}
	}()
	if self.nativeProof {
		switch method {
		case "state_getReadProof", "state_getChildReadProof":
		default:
			return nil, errors.New("method is outside the native execution proof read profile")
		}
	} else if self.storage {
		switch method {
		case "chain_getBlockHash", "state_getStorage", "state_getReadProof":
		default:
			return nil, errors.New("method is outside the native storage capture read profile")
		}
	} else if self.finality {
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
		maximumRequests, maximumReply := maximumCollectionRequests, maximumCollectionReplyBytes
		if self.nativeProof {
			maximumRequests, maximumReply = 2*MaximumNativeExecutionStorageNodes, MaximumNativeExecutionProofReplyBytes
		}
		if self.requests >= maximumRequests || self.remaining <= 0 {
			lastReason = "receipt collection exhausted its shared request/response budget"
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
				lastReason = "native proof next-attempt debit reservation failed"
				return nil, err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.url, bytes.NewReader(payload))
		if err != nil {
			return nil, errors.New("receipt collection request construction failed")
		}
		request.Header.Set("Content-Type", "application/json")
		lastRequest, lastReply, lastId, lastStatus, lastBodyReadComplete = payload, nil, id, 0, false
		lastReason = "native proof physical request did not complete"
		response, requestErr := self.client.Do(request)
		var raw []byte
		var readErr, closeErr error
		status := 0
		var header http.Header
		if response != nil {
			status = response.StatusCode
			header = response.Header
			if response.Body != nil {
				limit := min(self.remaining, maximumReply)
				raw, readErr = io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
				closeErr = response.Body.Close()
				self.remaining -= len(raw)
				if len(raw) > limit {
					readErr = errors.Join(readErr, errReceiptCollectorReplyBound)
				}
			} else if requestErr == nil {
				readErr = errors.New("receipt collection response body is absent")
			}
		} else if requestErr == nil {
			requestErr = errors.New("receipt collection response is absent")
		}
		lastReply, lastStatus = raw, status
		lastBodyReadComplete = response != nil && response.Body != nil && readErr == nil
		var journalErr error
		if self.afterRead != nil {
			journalErr = self.afterRead(ctx, id, len(raw))
		}
		observationErr := errors.Join(requestErr, readErr, closeErr)
		readCauses := errors.Join(observationErr, journalErr)
		// Complete returned bytes are evidence even if their terminal read or
		// close also failed. A wrong identity/ambiguous frame must dominate a
		// transient tail; incomplete bytes alone cannot establish a conflict.
		var reply receiptCollectorReply
		complete := json.Valid(raw)
		unavailable := false
		if complete {
			var err error
			if self.nativeProof || self.nativeExecution {
				unavailable, err = decodeNativeReceiptCollectorReply(raw, id, &reply)
			} else {
				err = decodeReceiptCollectorReply(raw, id, &reply)
			}
			if err != nil {
				lastReason = err.Error()
				if self.nativeProof && !errors.Is(err, errReceiptCollectorCapability) {
					err = errors.Join(ErrNativeExecutionProofConflict, err)
				} else if self.nativeExecution && !errors.Is(err, errReceiptCollectorCapability) {
					err = nativeFinalityVerificationError(err)
				}
				return nil, errors.Join(err, readCauses)
			}
		}
		if complete && len(reply.Error) != 0 {
			return nil, errors.Join(fmt.Errorf("receipt collection %s returned an RPC error; required read capability is unavailable", method), readCauses)
		}
		if complete && !unavailable && self.validateResult != nil {
			if err := self.validateResult(reply.Result); err != nil {
				if err == ErrNativeStorageIncomplete {
					unavailable = true
				} else {
					lastReason = "native proof result conflicts with the original parent or bounded result schema"
					return nil, errors.Join(err, readCauses)
				}
			}
		}
		if journalErr != nil {
			lastReason = "native proof read debit publication failed"
			// A failed debit publication forbids another read, but cannot erase
			// a complete contradiction that was already returned above.
			return nil, readCauses
		}
		if status != 0 && status != http.StatusOK && !receiptCollectorStatusRetry(status) {
			lastReason = "native proof endpoint returned a permanent HTTP status"
			return nil, errors.Join(fmt.Errorf("receipt collection %s refused HTTP status %d", method, status), observationErr)
		}
		if observationErr != nil && !receiptCollectorRetryable(observationErr) {
			lastReason = "native proof transport or body observation contains a non-retryable cause"
			return nil, observationErr
		}
		if err := ctx.Err(); err != nil {
			lastReason = "native proof read ended under its original owner cancellation or deadline"
			if unavailable {
				return nil, errors.Join(errReceiptCollectorUnavailable, err, observationErr)
			}
			return nil, errors.Join(err, observationErr)
		}
		if observationErr != nil || status != http.StatusOK || unavailable {
			lastReason = "native RPC evidence remained unavailable under the original retry budget"
			delay := time.Second
			if self.nativeProof || self.nativeExecution {
				deadline, _ := ctx.Deadline()
				delay = receiptCollectorRetryDelay(header, raw, time.Now(), deadline)
			}
			if err := self.wait(ctx, delay); err != nil {
				if unavailable || self.nativeProof || self.nativeExecution {
					return nil, errors.Join(errReceiptCollectorUnavailable, err, observationErr)
				}
				return nil, errors.Join(err, observationErr)
			}
			continue
		}
		if !complete {
			if self.nativeProof || self.nativeExecution {
				lastReason = "native RPC reply did not contain one complete JSON envelope"
				// An incomplete transport frame establishes no decoded evidence.
				// Keep its charged bytes and original deadline while retrying.
				if err := self.wait(ctx, time.Second); err != nil {
					return nil, errors.Join(io.ErrUnexpectedEOF, err)
				}
				continue
			}
			return nil, errors.New("receipt collection reply is ambiguous or malformed")
		}
		if len(reply.Error) != 0 {
			return nil, fmt.Errorf("receipt collection %s returned an RPC error; required read capability is unavailable", method)
		}
		// The native producer requires an actual header/block/hash. Explicit null
		// is unavailable, not contradictory evidence; old receipt profiles retain
		// their existing nullable read semantics.
		if self.requiredFinality && bytes.Equal(bytes.TrimSpace(reply.Result), []byte("null")) {
			lastReason = "native RPC returned an unavailable null result"
			if err := self.wait(ctx, time.Second); err != nil {
				return nil, err
			}
			continue
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
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("receipt collection %s result is unavailable or malformed", method)
	}
	if err := json.Unmarshal(raw, result); err != nil {
		if self.nativeExecution {
			return nativeFinalityVerificationError(err)
		}
		return fmt.Errorf("receipt collection %s result is unavailable or malformed", method)
	}
	return nil
}
