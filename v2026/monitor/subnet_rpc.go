// SIGNALS.md §22.0 bounded chain reads for the subnet correctness readers.
// One session per run owns every Json-Rpc request to the enrolled endpoints:
// a fixed read-only method allowlist, a per-endpoint request budget, a byte
// cap before decoding, strict envelopes, no redirect, proxy, retry, cookie or
// decompression, and one process-wide admission per endpoint shared by every
// reader. Remote text never reaches a finding; only fixed reasons do.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	gethrpc "github.com/centrifuge/go-substrate-rpc-client/v4/gethrpc"
)

// Read-only methods and their response class. Writes such as extrinsic or
// raw-transaction submission are absent by construction.
var subnetRpcMethods = map[string]string{
	"chain_getFinalizedHead":   "small",
	"chain_getHeader":          "small",
	"chain_getBlockHash":       "small",
	"state_getRuntimeVersion":  "small",
	"state_getStorageHash":     "small",
	"state_getStorage":         "small",
	"state_getKeysPaged":       "small",
	"state_getMetadata":        "metadata",
	"eth_chainId":              "small",
	"eth_getBlockByNumber":     "small",
	"eth_getBlockByHash":       "small",
	"eth_call":                 "small",
	"eth_getCode":              "small",
	"eth_getStorageAt":         "small",
	"eth_getLogs":              "small",
	"eth_getTransactionByHash": "small",
}

// A fixed public reason with a private cause for Is/As inspection.
type subnetReadError struct {
	reason string
	cause  error
}

func (self *subnetReadError) Error() string { return "subnet read " + self.reason }
func (self *subnetReadError) Unwrap() error { return self.cause }

// The fixed reason of a read failure; anything else is unavailable.
func subnetReadReason(err error) string {
	var readErr *subnetReadError
	if errors.As(err, &readErr) {
		return readErr.reason
	}
	return "unavailable"
}

func subnetReadFailure(reason string, cause error) error {
	return &subnetReadError{reason: reason, cause: cause}
}

// One process-wide admission per endpoint keeps every §22 reader and every
// concurrent run inside one shared endpoint budget. Capacity is fixed by the
// first enrollment generation that names the endpoint.
type subnetRpcAdmission struct {
	stateLock     sync.Mutex
	endpointSlots map[string]chan struct{}
}

var subnetRpcEndpointAdmission = &subnetRpcAdmission{endpointSlots: map[string]chan struct{}{}}

func (self *subnetRpcAdmission) slots(endpoint string, capacity int) chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	slots, ok := self.endpointSlots[endpoint]
	if !ok {
		slots = make(chan struct{}, capacity)
		self.endpointSlots[endpoint] = slots
	}
	return slots
}

// A canceled wait takes no slot.
func (self *subnetRpcAdmission) acquire(ctx context.Context, endpoint string, capacity int) (func(), error) {
	slots := self.slots(endpoint, capacity)
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// An enrolled endpoint with its public ordinal. The Url is never rendered.
type subnetRpcEndpoint struct {
	url   string
	label string
}

// One logical call in a batch, with its own result and error.
type subnetRpcCall struct {
	method string
	params []any
	result any
	err    error
}

// A run-owned session. Its counters are guarded; requests may be concurrent.
type subnetRpcSession struct {
	client        grafanaHTTPClient
	limits        subnetLimits
	stateLock     sync.Mutex
	nextId        uint64
	requestCounts map[string]int
}

func newSubnetRpcSession(client grafanaHTTPClient, limits subnetLimits) *subnetRpcSession {
	return &subnetRpcSession{client: client, limits: limits, requestCounts: map[string]int{}}
}

// Reserves one Http request of the endpoint budget and the next request ids.
func (self *subnetRpcSession) reserve(endpoint subnetRpcEndpoint, ids int) (uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.requestCounts[endpoint.url] >= self.limits.maxRequestsPerEndpoint {
		return 0, subnetReadFailure("request-budget-exhausted", nil)
	}
	self.requestCounts[endpoint.url] += 1
	first := self.nextId + 1
	self.nextId += uint64(ids)
	return first, nil
}

// Requests issued to one endpoint in this session.
func (self *subnetRpcSession) requests(endpoint subnetRpcEndpoint) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.requestCounts[endpoint.url]
}

// The byte cap for a method's response.
func (self *subnetRpcSession) responseLimit(method string) int64 {
	if subnetRpcMethods[method] == "metadata" {
		return self.limits.maxMetadataBytes
	}
	return self.limits.maxResponseBytes
}

// One call. A Json null result decodes into the target as Json null does.
func (self *subnetRpcSession) call(ctx context.Context, endpoint subnetRpcEndpoint, method string, params []any, result any) error {
	calls := []subnetRpcCall{{method: method, params: params, result: result}}
	if err := self.exchange(ctx, endpoint, calls, false); err != nil {
		return err
	}
	return calls[0].err
}

// A bounded Json-Rpc batch. Transport and envelope failures fail the whole
// batch; a remote error for one call is that call's error only.
func (self *subnetRpcSession) batch(ctx context.Context, endpoint subnetRpcEndpoint, calls []subnetRpcCall) error {
	if len(calls) == 0 {
		return nil
	}
	if len(calls) > self.limits.maxBatchCalls {
		return subnetReadFailure("batch-too-large", nil)
	}
	return self.exchange(ctx, endpoint, calls, true)
}

type subnetRpcRequest struct {
	JsonRpc string `json:"jsonrpc"`
	Id      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type subnetRpcRemoteError struct {
	Code int64 `json:"code"`
}

type subnetRpcEnvelope struct {
	JsonRpc string                `json:"jsonrpc"`
	Id      json.RawMessage       `json:"id"`
	Result  json.RawMessage       `json:"result"`
	Error   *subnetRpcRemoteError `json:"error"`
}

func (self *subnetRpcSession) exchange(ctx context.Context, endpoint subnetRpcEndpoint, calls []subnetRpcCall, batch bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	limit := int64(0)
	for _, call := range calls {
		if _, ok := subnetRpcMethods[call.method]; !ok {
			return subnetReadFailure("method-not-allowed", nil)
		}
		if callLimit := self.responseLimit(call.method); limit < callLimit {
			limit = callLimit
		}
	}
	firstId, err := self.reserve(endpoint, len(calls))
	if err != nil {
		return err
	}
	requests := make([]subnetRpcRequest, len(calls))
	for i, call := range calls {
		params := call.params
		if params == nil {
			params = []any{}
		}
		requests[i] = subnetRpcRequest{JsonRpc: "2.0", Id: firstId + uint64(i), Method: call.method, Params: params}
	}
	var body []byte
	if batch {
		body, err = json.Marshal(requests)
	} else {
		body, err = json.Marshal(requests[0])
	}
	if err != nil {
		return subnetReadFailure("request-invalid", err)
	}
	release, err := subnetRpcEndpointAdmission.acquire(ctx, endpoint.url, self.limits.endpointConcurrency)
	if err != nil {
		return err
	}
	defer release()
	raw, err := self.post(ctx, endpoint, body, limit)
	if err != nil {
		return err
	}
	envelopes := []subnetRpcEnvelope{}
	if batch {
		var parts []json.RawMessage
		if err := subnetRpcDecodeStrict(raw, &parts); err != nil || len(parts) != len(calls) {
			return subnetReadFailure("envelope-invalid", err)
		}
		for _, part := range parts {
			envelope, err := subnetRpcDecodeEnvelope(part)
			if err != nil {
				return err
			}
			envelopes = append(envelopes, envelope)
		}
	} else {
		envelope, err := subnetRpcDecodeEnvelope(raw)
		if err != nil {
			return err
		}
		envelopes = append(envelopes, envelope)
	}
	// Responses may arrive in any order; each id must be answered exactly once.
	byId := map[uint64]subnetRpcEnvelope{}
	for _, envelope := range envelopes {
		id, err := strconv.ParseUint(string(envelope.Id), 10, 64)
		if err != nil || id < firstId || firstId+uint64(len(calls)) <= id {
			return subnetReadFailure("envelope-id-invalid", err)
		}
		if _, duplicate := byId[id]; duplicate {
			return subnetReadFailure("envelope-id-invalid", nil)
		}
		byId[id] = envelope
	}
	for i := range calls {
		envelope := byId[firstId+uint64(i)]
		if envelope.Error != nil {
			calls[i].err = subnetReadFailure("rpc-error", fmt.Errorf("remote code %d", envelope.Error.Code))
			continue
		}
		if err := json.Unmarshal(envelope.Result, calls[i].result); err != nil {
			calls[i].err = subnetReadFailure("result-invalid", err)
		}
	}
	return nil
}

// Strict envelope: unique keys, version 2.0, a numeric id and exactly one of
// result and error.
func subnetRpcDecodeEnvelope(raw []byte) (subnetRpcEnvelope, error) {
	var envelope subnetRpcEnvelope
	if err := subnetRpcRequireUniqueKeys(raw); err != nil {
		return envelope, subnetReadFailure("envelope-invalid", err)
	}
	if err := subnetRpcDecodeStrict(raw, &envelope); err != nil {
		return envelope, subnetReadFailure("envelope-invalid", err)
	}
	hasResult := envelope.Result != nil
	hasError := envelope.Error != nil
	if envelope.JsonRpc != "2.0" || len(envelope.Id) == 0 || hasResult == hasError {
		return envelope, subnetReadFailure("envelope-invalid", nil)
	}
	return envelope, nil
}

// One Json value with no trailing data.
func subnetRpcDecodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// Duplicate top-level keys would let a hostile endpoint shadow a result.
func subnetRpcRequireUniqueKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("envelope is not an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("duplicate or invalid key")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	return nil
}

// Posts one bounded request. Every response body is closed before return.
func (self *subnetRpcSession) post(parent context.Context, endpoint subnetRpcEndpoint, body []byte, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, self.limits.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.url, bytes.NewReader(body))
	if err != nil {
		return nil, subnetReadFailure("request-invalid", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := doScopedGrafanaRequest(self.client, request)
	if response == nil || response.Body == nil {
		if parentErr := parent.Err(); parentErr != nil {
			return nil, parentErr
		}
		return nil, subnetReadFailure("transport-unavailable", errors.Join(err, ctx.Err()))
	}
	defer response.Body.Close()
	if err != nil {
		return nil, subnetReadFailure("transport-unavailable", err)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || response.Uncompressed || response.Header.Get("Content-Encoding") != "" || mediaErr != nil || mediaType != "application/json" {
		return nil, subnetReadFailure("response-contract-invalid", nil)
	}
	if response.ContentLength > limit {
		return nil, subnetReadFailure("response-oversized", nil)
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if readErr != nil {
		if parentErr := parent.Err(); parentErr != nil {
			return nil, parentErr
		}
		return nil, subnetReadFailure("body-unavailable", readErr)
	}
	if int64(len(raw)) > limit {
		return nil, subnetReadFailure("response-oversized", nil)
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(raw)) {
		return nil, subnetReadFailure("response-length-invalid", nil)
	}
	if parentErr := parent.Err(); parentErr != nil {
		return nil, parentErr
	}
	return raw, nil
}

// The native bounded client used when no source seam is injected: standard
// certificate verification, no ambient proxy, redirect, cookie, automatic
// decompression or cross-probe connection pool.
func newSubnetRpcHttpClient(timeout time.Duration) (*http.Client, *http.Transport) {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 * 1024,
		MaxIdleConns:           4,
		MaxIdleConnsPerHost:    2,
		MaxConnsPerHost:        4,
		IdleConnTimeout:        timeout,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, transport
}

// The injected source seam shared by every subnet reader; nil selects the
// native bounded client.
func (self *probeEnv) subnetHttpClient() grafanaHTTPClient {
	return self.subnetArtifactClient
}

// Adapts one endpoint of a session to the gsrpc client so sn's reviewed
// storage readers decode through the same bounded transport. Subscriptions
// and context-free calls are refused; Url never exposes the endpoint.
type subnetNativeClient struct {
	session  *subnetRpcSession
	endpoint subnetRpcEndpoint
}

func (self *subnetNativeClient) Call(result any, method string, args ...any) error {
	return subnetReadFailure("context-required", nil)
}

func (self *subnetNativeClient) CallContext(ctx context.Context, result any, method string, args ...any) error {
	return self.session.call(ctx, self.endpoint, method, args, result)
}

func (self *subnetNativeClient) Subscribe(context.Context, string, string, string, string, any, ...any) (*gethrpc.ClientSubscription, error) {
	return nil, subnetReadFailure("subscription-refused", nil)
}

func (self *subnetNativeClient) URL() string { return "" }

func (self *subnetNativeClient) Close() {}
