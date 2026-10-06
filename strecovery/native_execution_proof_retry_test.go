// Original identities and finite budgets survive unavailable archive replies.
// Logical transport/wait ports force retry boundaries without clock sleeps.
package strecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeExecutionProofReaderRecoversUnavailableEnvelopes(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		raw      string
		complete bool
		version  string
	}{
		{name: "http408", status: 408, raw: `{"message":"temporarily unavailable"}`},
		{name: "http429", status: 429, raw: `{"retry_after_seconds":2}`},
		{name: "http500", status: 500, raw: `{"message":"temporarily unavailable"}`},
		{name: "http502", status: 502, raw: `{"message":"temporarily unavailable"}`},
		{name: "http503", status: 503, raw: `{"message":"temporarily unavailable"}`},
		{name: "http504", status: 504, raw: `{"message":"temporarily unavailable"}`},
		{name: "internal-error", status: 200, raw: `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"timeout"}}`},
		{name: "node-error", status: 200, raw: `{"jsonrpc":"2.0","id":1,"result":null,"error":{"code":-32000}}`},
		{name: "unidentified-error", status: 200, raw: `{"jsonrpc":"2.0","id":null,"error":{"code":-32000}}`},
		{name: "missing-result", status: 200, raw: `{"jsonrpc":"2.0","id":1}`},
		{name: "null-result", status: 200, raw: `{"jsonrpc":"2.0","id":1,"result":null}`},
		{name: "unclaimed-object", status: 200, raw: `{"message":"temporarily unavailable"}`},
		{name: "array-is-not-a-reply", status: 200, raw: `[{"jsonrpc":"2.0","id":1,"result":{}}]`},
		{name: "incomplete-json", status: 200, raw: `{"jsonrpc":"2.0",`},
		{name: "null-error", status: 200, complete: true},
		{name: "escaped-digit-version", status: 200, complete: true, version: `"\u0032.0"`},
		{name: "escaped-dot-version", status: 200, complete: true, version: `"2\u002e0"`},
	}
	for _, test := range cases {
		for _, nativeFinality := range []bool{false, true} {
			reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			parent := "0x" + strings.Repeat("17", 32)
			method := "state_getReadProof"
			params := []any{[]string{"0xa0"}, parent}
			if nativeFinality {
				reader.rpc.nativeProof = false
				reader.rpc.nativeExecution, reader.rpc.finality = true, true
				method, params = "chain_getHeader", []any{parent}
			}
			expectedParams, _ := json.Marshal(params)
			calls, waits, charged := 0, 0, 0
			reader.rpc.wait = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }
			reader.rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				var call struct {
					Id     int             `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
					t.Fatal(err)
				}
				if call.Id != calls || call.Method != method || !bytes.Equal(call.Params, expectedParams) {
					t.Fatal("retry changed original method/selector or reused an identity", call)
				}
				raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"at":%q,"proof":["0x01"]},"error":null}`, call.Id, parent)
				if test.version != "" {
					raw = strings.Replace(raw, `"jsonrpc":"2.0"`, `"jsonrpc":`+test.version, 1)
				}
				status := 200
				if calls == 1 && !test.complete {
					raw, status = test.raw, test.status
				}
				if calls > 2 {
					t.Fatal("retry did not consume the fresh valid reply")
				}
				charged += len(raw)
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(raw)}}, nil
			})
			if nativeFinality {
				var raw json.RawMessage
				raw, err = reader.rpc.call(t.Context(), method, params)
				if err == nil && !bytes.Contains(raw, []byte(parent)) {
					t.Fatal("finality returned no selected result", string(raw))
				}
			} else {
				var proof *NativeExecutionReadProof
				proof, err = reader.Read(t.Context(), parent, nil, []byte{0xa0})
				if err == nil && (proof == nil || proof.At != parent || len(proof.Proof) != 1 || proof.Proof[0] != "0x01") {
					t.Fatal("proof did not retain the successful candidate", proof)
				}
			}
			expectedCalls := 2
			if test.complete {
				expectedCalls = 1
			}
			if err != nil || calls != expectedCalls || waits != expectedCalls-1 || reader.rpc.requests != calls || reader.rpc.remaining != maximumNativeExecutionProofTransferBytes-charged {
				t.Fatalf("%s finality=%v unavailable reply changed its original budget: calls=%d waits=%d err=%v", test.name, nativeFinality, calls, waits, err)
			}
			reader.Close()
		}
	}
}

func TestNativeExecutionProofReaderConcreteRefusalsDominateTransientStatus(t *testing.T) {
	parent := "0x" + strings.Repeat("17", 32)
	cases := []struct {
		name       string
		raw        string
		conflict   bool
		capability bool
	}{
		{name: "identity", raw: `{"jsonrpc":"2.0","id":99,"result":{}}`, conflict: true},
		{name: "protocol-version", raw: `{"jsonrpc":"1.0","id":1,"result":{}}`, conflict: true},
		{name: "protocol-type", raw: `{"jsonrpc":2,"id":1,"result":{}}`, conflict: true},
		{name: "duplicate", raw: `{"jsonrpc":"2.0","id":1,"id":1,"result":{}}`, conflict: true},
		{name: "case-fold", raw: `{"jsonrpc":"2.0","id":1,"ID":1,"result":{}}`, conflict: true},
		{name: "mixed-result-error", raw: `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32000}}`, conflict: true},
		{name: "parent", raw: fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"at":%q,"proof":["0x01"]}}`, "0x"+strings.Repeat("18", 32)), conflict: true},
		{name: "result-type", raw: `{"jsonrpc":"2.0","id":1,"result":7}`, conflict: true},
		{name: "method-unavailable", raw: `{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}`, capability: true},
		{name: "parameters-refused", raw: `{"jsonrpc":"2.0","id":1,"result":null,"error":{"code":-32602}}`, capability: true},
	}
	for _, test := range cases {
		reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		waits := 0
		reader.rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") }
		reader.rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(test.raw), tail: syscall.EIO}}, nil
		})
		proof, err := reader.Read(t.Context(), parent, nil, nil)
		reader.Close()
		if proof != nil || waits != 0 || !errors.Is(err, syscall.EIO) || errors.Is(err, ErrNativeExecutionProofConflict) != test.conflict || errors.Is(err, errReceiptCollectorCapability) != test.capability {
			t.Fatalf("%s refusal was weakened by transient status/tail: proof=%v waits=%d err=%v", test.name, proof, waits, err)
		}
	}
	permanent := errors.New("synthetic permanent validation refusal")
	for _, hard := range []error{ErrNativeExecutionProofConflict, permanent} {
		rpc := newReceiptCollectorRpc("http://native.example")
		rpc.nativeProof = true
		waits := 0
		rpc.validateResult = func(json.RawMessage) error { return errors.Join(ErrNativeStorageIncomplete, hard) }
		rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") }
		rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)}}, nil
		})
		result, err := rpc.call(t.Context(), "state_getReadProof", nil)
		if result != nil || waits != 0 || !errors.Is(err, hard) {
			t.Fatal("joined missing coverage hid an actual hard cause", result, waits, err)
		}
	}
}

func TestNativeExecutionProofReaderDoesNotReuseCandidateAcrossReplies(t *testing.T) {
	parent := "0x" + strings.Repeat("17", 32)
	cases := []struct {
		name   string
		result string
	}{
		{name: "empty", result: `{}`},
		{name: "missing-parent", result: `{"proof":["0x02"]}`},
		{name: "missing-proof", result: fmt.Sprintf(`{"at":%q}`, parent)},
		{name: "empty-parent", result: `{"at":"","proof":["0x02"]}`},
		{name: "null-parent", result: `{"at":null,"proof":["0x02"]}`},
		{name: "null-proof", result: fmt.Sprintf(`{"at":%q,"proof":null}`, parent)},
	}
	for _, test := range cases {
		reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		calls, waits := 0, 0
		reader.rpc.wait = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }
		reader.rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			result := fmt.Sprintf(`{"at":%q,"proof":["0x01"]}`, parent)
			var tail error
			if calls == 1 {
				tail = syscall.EIO
			}
			if calls == 2 {
				result = test.result
			}
			if calls == 3 {
				result = fmt.Sprintf(`{"at":%q,"proof":["0x03"]}`, parent)
			}
			if calls > 3 {
				t.Fatal("fresh candidate was not accepted")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, calls, result)), tail: tail}}, nil
		})
		proof, err := reader.Read(t.Context(), parent, nil, nil)
		reader.Close()
		if err != nil || proof == nil || calls != 3 || waits != 2 || len(proof.Proof) != 1 || proof.Proof[0] != "0x03" {
			t.Fatalf("%s inherited a candidate across physical replies: proof=%v calls=%d waits=%d err=%v", test.name, proof, calls, waits, err)
		}
	}
}

func TestNativeExecutionProofReaderUnavailableRetryKeepsOwnerDeadline(t *testing.T) {
	reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	deadline := time.Now().Add(300 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	calls, waits := 0, 0
	reader.rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if actual, ok := request.Context().Deadline(); !ok || actual.After(deadline) {
			t.Fatal("physical request escaped the original deadline", actual)
		}
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(`{"message":"unavailable"}`)}}, nil
	})
	reader.rpc.wait = func(waitCtx context.Context, delay time.Duration) error {
		waits++
		actual, ok := waitCtx.Deadline()
		if !ok || !actual.Equal(deadline) || time.Until(actual) < time.Minute || delay <= 0 || delay > time.Until(actual) {
			t.Fatal("retry reset or clipped its owner budget", actual, delay)
		}
		if waits == 5 {
			cancel()
		}
		return waitCtx.Err()
	}
	proof, err := reader.Read(ctx, "0x"+strings.Repeat("17", 32), nil, nil)
	if proof != nil || calls != 5 || waits != 5 || !errors.Is(err, context.Canceled) || !errors.Is(err, errReceiptCollectorUnavailable) || errors.Is(err, ErrNativeExecutionProofConflict) {
		t.Fatal("unavailable retries became a contradiction or escaped cancellation", proof, calls, waits, err)
	}
}

func TestNativeExecutionProofRetryHintsStayInsideOriginalDeadline(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name      string
		hint      string
		raw       string
		remaining time.Duration
		expected  time.Duration
	}{
		{name: "default", remaining: 300 * time.Second, expected: time.Second},
		{name: "seconds", hint: "7", remaining: 300 * time.Second, expected: 7 * time.Second},
		{name: "date", hint: now.Add(9 * time.Second).Format(http.TimeFormat), remaining: 300 * time.Second, expected: 9 * time.Second},
		{name: "json", raw: `{"retry_after_seconds":11}`, remaining: 300 * time.Second, expected: 11 * time.Second},
		{name: "larger-json", hint: "7", raw: `{"retry_after_seconds":11}`, remaining: 300 * time.Second, expected: 11 * time.Second},
		{name: "large-seconds", hint: "18446744073709551615", remaining: 300 * time.Second, expected: 300 * time.Second},
		{name: "large-date", hint: now.Add(time.Hour).Format(http.TimeFormat), remaining: 300 * time.Second, expected: 300 * time.Second},
		{name: "negative", hint: "-1", raw: `{"retry_after_seconds":-2}`, remaining: 300 * time.Second, expected: time.Second},
		{name: "fraction", raw: `{"retry_after_seconds":1.5}`, remaining: 300 * time.Second, expected: time.Second},
		{name: "ambiguous", raw: `{"retry_after_seconds":2,"retry_after_seconds":99}`, remaining: 300 * time.Second, expected: time.Second},
		{name: "short-remaining", hint: "9", remaining: time.Millisecond, expected: time.Millisecond},
		{name: "expired", hint: "9", remaining: -time.Second, expected: 0},
	}
	for _, test := range cases {
		header := http.Header{}
		if test.hint != "" {
			header.Set("Retry-After", test.hint)
		}
		if actual := receiptCollectorRetryDelay(header, []byte(test.raw), now, now.Add(test.remaining)); actual != test.expected {
			t.Errorf("%s delay=%s expected=%s", test.name, actual, test.expected)
		}
	}
}

// Diagnostic formatting must not call arbitrary transport error methods.
type nativeProofDiagnosticError struct{}

func (*nativeProofDiagnosticError) Error() string {
	panic("diagnostic dispatched foreign Error method")
}

func TestNativeExecutionProofReaderFailureEvidenceRetainsBoundedAttempt(t *testing.T) {
	for _, incompleteBody := range []bool{false, true} {
		reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		var requestRaw []byte
		raw := []byte(`{"message":"` + strings.Repeat("x", MaximumNativeExecutionProofFailureReplyBytes+100) + `"}`)
		reader.rpc.wait = func(waitCtx context.Context, _ time.Duration) error { cancel(); return waitCtx.Err() }
		reader.rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
			requestRaw, err = io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var tail error
			if incompleteBody {
				tail = syscall.EIO
			}
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: &nativeReadTestBody{raw: append([]byte(nil), raw...), tail: tail}}, nil
		})
		proof, err := reader.Read(ctx, "0x"+strings.Repeat("17", 32), nil, []byte{0xa0})
		cancel()
		reader.Close()
		var failure *NativeExecutionProofReadFailure
		requestHash, replyHash := sha256.Sum256(requestRaw), sha256.Sum256(raw)
		if proof != nil || !errors.As(err, &failure) || !errors.Is(err, context.Canceled) {
			t.Fatal("refusal lost original attempt", proof, err)
		}
		if failure.Endpoint != "http://native.example" || failure.Method != "state_getReadProof" || failure.RequestId != 1 || failure.Status != 503 || !bytes.Equal(failure.Request, requestRaw) || failure.RequestSha256 != hex.EncodeToString(requestHash[:]) || failure.ReplySha256 != hex.EncodeToString(replyHash[:]) || failure.ReplyBytes != len(raw) || !failure.ReplyTruncated || failure.BodyReadComplete == incompleteBody || !bytes.Equal(failure.ReplyPrefix, raw[:MaximumNativeExecutionProofFailureReplyBytes]) {
			t.Fatal("failure snapshot changed original bytes, read completeness or identity", failure)
		}
		encoded, encodeErr := json.Marshal(failure)
		if encodeErr != nil || len(encoded) > 128*1024 || failure.Failure == "" || !bytes.Contains(encoded, []byte(`"failure"`)) || strings.Contains(err.Error(), strings.Repeat("x", 128)) {
			t.Fatal("diagnostic escaped its bounded envelope or printed reply bytes", len(encoded), encodeErr)
		}
	}
	rpc := newReceiptCollectorRpc("http://native.example")
	rpc.nativeProof = true
	foreign := &nativeProofDiagnosticError{}
	rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), closeErr: foreign}}, nil
	})
	result, err := rpc.call(t.Context(), "state_getReadProof", nil)
	if result != nil || !errors.Is(err, foreign) || !strings.Contains(err.Error(), "non-retryable cause") {
		t.Fatal("diagnostic lost a foreign cause or dispatched its formatter")
	}
}

func TestNativeExecutionProofReaderUnavailableRepliesConsumeSharedBudgets(t *testing.T) {
	for _, requestBound := range []bool{false, true} {
		reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"message":"unavailable"}`)
		if requestBound {
			reader.rpc.requests = 2*MaximumNativeExecutionStorageNodes - 1
		} else {
			reader.rpc.remaining = len(raw)
		}
		calls, waits := 0, 0
		reader.rpc.wait = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }
		reader.rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: &nativeReadTestBody{raw: append([]byte(nil), raw...)}}, nil
		})
		proof, err := reader.Read(t.Context(), "0x"+strings.Repeat("17", 32), nil, nil)
		reader.Close()
		if proof != nil || err == nil || !strings.Contains(err.Error(), "exhausted its shared request/response budget") || errors.Is(err, ErrNativeExecutionProofConflict) || calls != 1 || waits != 1 {
			t.Fatal("unavailable response reset a shared budget", requestBound, proof, calls, waits, err)
		}
	}
}
