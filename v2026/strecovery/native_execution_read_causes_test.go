// Actual HTTP calls retain every cause and validate complete returned evidence
// before retry. Logical waits force budget/cancellation boundaries without sleeps.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type nativeReadTestTransport func(*http.Request) (*http.Response, error)

func (self nativeReadTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

type nativeReadTestBody struct {
	raw            []byte
	tail, closeErr error
}

func (self *nativeReadTestBody) Read(p []byte) (int, error) {
	if len(self.raw) != 0 {
		n := copy(p, self.raw)
		self.raw = self.raw[n:]
		return n, nil
	}
	if self.tail != nil {
		err := self.tail
		self.tail = nil
		return 0, err
	}
	return 0, io.EOF
}
func (self *nativeReadTestBody) Close() error { return self.closeErr }

func TestNativeExecutionRpcTransientReadAndCloseRetrySameBudget(t *testing.T) {
	for _, closeFailure := range []bool{false, true} {
		rpc := newReceiptCollectorRpc("http://native.example")
		rpc.finality = true
		calls, waits := 0, 0
		rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
			calls++
			body := &nativeReadTestBody{raw: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"original"}`, calls))}
			if calls == 1 {
				if closeFailure {
					body.closeErr = syscall.EIO
				} else {
					body.tail = io.ErrUnexpectedEOF
				}
			}
			return &http.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
		})
		rpc.wait = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }
		raw, err := rpc.call(t.Context(), "chain_getHeader", []any{"original"})
		if err != nil || string(raw) != `"original"` || calls != 2 || waits != 1 || rpc.requests != 2 {
			t.Errorf("transient read/close changed retry identity: close=%v calls=%d waits=%d raw=%s err=%v", closeFailure, calls, waits, raw, err)
		}
	}
}

func TestNativeExecutionRpcHardCauseDominatesTransientAndCompleteReply(t *testing.T) {
	permanent := errors.New("synthetic permanent transport refusal")
	cases := []struct {
		name           string
		raw            string
		tail, closeErr error
	}{
		{name: "read", raw: `{"jsonrpc":"2.0","id":1,"result":1}`, tail: errors.Join(syscall.EIO, permanent)},
		{name: "close", raw: `{"jsonrpc":"2.0","id":1,"result":1}`, closeErr: errors.Join(syscall.EIO, permanent)},
		{name: "identity", raw: `{"jsonrpc":"2.0","id":99,"result":1}`, tail: syscall.EIO},
		{name: "duplicate", raw: `{"jsonrpc":"2.0","id":1,"id":1,"result":1}`, tail: syscall.EIO},
		{name: "semantic", raw: `{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}`, tail: syscall.EIO},
	}
	for _, test := range cases {
		rpc := newReceiptCollectorRpc("http://native.example")
		rpc.finality, rpc.nativeExecution = true, true
		waits := 0
		rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: &nativeReadTestBody{raw: []byte(test.raw), tail: test.tail, closeErr: test.closeErr}, Header: http.Header{}}, nil
		})
		rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") }
		raw, err := rpc.call(t.Context(), "chain_getHeader", []any{"original"})
		if err == nil || raw != nil || waits != 0 || rpc.requests != 1 {
			t.Errorf("%s hard returned evidence was retried/admitted: waits=%d raw=%s err=%v", test.name, waits, raw, err)
		}
		if errors.Is(err, ErrNativeFinalityConflict) != (test.name == "identity" || test.name == "duplicate") {
			t.Errorf("%s complete evidence and observation causes were conflated: %v", test.name, err)
		}
		if test.name == "read" || test.name == "close" {
			if !errors.Is(err, permanent) || !errors.Is(err, syscall.EIO) {
				t.Errorf("%s lost joined causes: %v", test.name, err)
			}
		}
	}
}

func TestNativeExecutionRpcOwnerCancellationJoinsPendingRead(t *testing.T) {
	rpc := newReceiptCollectorRpc("http://native.example")
	rpc.finality = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 200, Body: &nativeReadTestBody{raw: []byte(`{"jsonrpc":"2.0",`), tail: syscall.EIO}, Header: http.Header{}}, nil
	})
	waits := 0
	rpc.wait = func(context.Context, time.Duration) error { waits++; return nil }
	raw, err := rpc.call(ctx, "chain_getHeader", nil)
	if raw != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || waits != 0 {
		t.Fatal("owner cancellation retried or lost original read cause", raw, err, waits)
	}
}

func TestNativeExecutionCaptureRetainedReadPreservesCancellation(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	_, directory := captureTestInputs(t, fixture, 0)
	store, err := openPrivatePath(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	capture := &finalityCapture{directory: store, rpc: newReceiptCollectorRpc("http://native.example"), nativeExecution: true}
	if err := capture.save(t.Context(), "request-00001.json", finalityCaptureAttempt{RequestHash: digest([]byte("retained"))}); err != nil {
		t.Fatal(err)
	}
	original, err := capture.readTestBytes(t.Context(), "request-00001.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = capture.resumeBudget(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNativeFinalityConflict) || strings.Contains(err.Error(), "reservation is invalid") {
		t.Fatal("unavailable original bytes were relabeled malformed", err)
	}
	after, err := capture.readTestBytes(t.Context(), "request-00001.json")
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("refused retained read changed original reservation", err)
	}
}

// Force a complete cryptographic contradiction plus an unavailable tail through
// the actual capture RPC, before any retry can replace its original evidence.
func TestNativeExecutionCaptureForgeryDominatesTransientTail(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := nativeExecutionCaptureTestInputs(t, fixture)
	raw, err := hex.DecodeString(rpc.certificates[config.Child.Hash][2:])
	if err != nil {
		t.Fatal(err)
	}
	raw[81] ^= 1
	rpc.certificates[config.Child.Hash] = "0x" + hex.EncodeToString(raw)
	waits := 0
	value, err := captureNativeExecutionFinality(t.Context(), fixture.checkpoint, config, directory, func(client *receiptCollectorRpc) {
		rpc.configure(client)
		transport := client.client.Transport
		client.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(request)
			if err != nil || response == nil {
				return response, err
			}
			raw, readErr := io.ReadAll(response.Body)
			if err := errors.Join(readErr, response.Body.Close()); err != nil {
				return nil, err
			}
			body := &nativeReadTestBody{raw: raw}
			if strings.HasPrefix(rpc.calls[len(rpc.calls)-1], "chain_getBlock:") {
				body.tail = syscall.EIO
			}
			response.Body = body
			return response, nil
		})
		client.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") }
	})
	if value != nil || !errors.Is(err, ErrNativeFinalityConflict) || !errors.Is(err, syscall.EIO) || waits != 0 {
		t.Fatal("complete invalid vote became an unavailable retry", value, err, waits)
	}
	if _, err := os.Stat(filepath.Join(directory, "native-proof.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflicting vote acquired a complete proof", err)
	}
}

// Truncated framing is unread data. A later complete response uses the same
// exact request selector, consumes the old budget and retains every debit.
func TestNativeExecutionCaptureTruncatedTransportRecoversSameBoundary(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := nativeExecutionCaptureTestInputs(t, fixture)
	waits, damaged := 0, false
	value, err := captureNativeExecutionFinality(t.Context(), fixture.checkpoint, config, directory, func(client *receiptCollectorRpc) {
		rpc.configure(client)
		transport := client.client.Transport
		client.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(request)
			if err != nil || response == nil || damaged {
				return response, err
			}
			if err := response.Body.Close(); err != nil {
				return nil, err
			}
			damaged = true
			response.Body = &nativeReadTestBody{raw: []byte(`{"jsonrpc":"2.0","id":1,"result":`)}
			return response, nil
		})
		client.wait = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }
	})
	if err != nil || value == nil || waits != 1 || value.Finality.Parent != config.Parent || value.Finality.Child != config.Child || len(rpc.calls) < 2 || rpc.calls[0] != rpc.calls[1] {
		t.Fatal("incomplete transport changed finality identity or authority", value, err, waits, rpc.calls)
	}
	if _, err := os.Stat(filepath.Join(directory, "read-00001.json")); err != nil {
		t.Fatal("incomplete response escaped its original retained debit", err)
	}
}

// Fully read malformed private proof bytes are positive custody conflicts;
// failed local reads never pass this decoder boundary.
func TestNativeExecutionCaptureRetainedMalformedProofIsConflict(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	config, directory := nativeExecutionCaptureTestInputs(t, fixture)
	path := filepath.Join(directory, "native-proof.json")
	original := []byte(`{"schema":`)
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	value, err := captureNativeExecutionFinality(t.Context(), fixture.checkpoint, config, directory, func(*receiptCollectorRpc) { t.Fatal("malformed original proof triggered recapture") })
	if value != nil || !errors.Is(err, ErrNativeFinalityConflict) {
		t.Fatal("complete malformed original bytes became an unavailable read", value, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("refusal rewrote original malformed custody", err)
	}
}

func TestNativeExecutionCaptureClosedLocalReadRemainsObservation(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	_, directory := nativeExecutionCaptureTestInputs(t, fixture)
	store, err := openPrivatePath(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	capture := &finalityCapture{directory: store, nativeExecution: true}
	if err := capture.save(t.Context(), "original.json", finalityCaptureAttempt{RequestHash: digest([]byte("original"))}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "original.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var original finalityCaptureAttempt
	found, err := capture.load(t.Context(), "original.json", 1024, &original)
	if found || !errors.Is(err, syscall.EBADF) || errors.Is(err, ErrNativeFinalityConflict) || original.RequestHash != "" {
		t.Fatal("failed local observation became malformed original evidence", found, original, err)
	}
	after, err := os.ReadFile(filepath.Join(directory, "original.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed local read changed retained evidence", err)
	}
}

func TestNativeExecutionRpcReturnedConflictSurvivesFailedDebitPublication(t *testing.T) {
	rpc := newReceiptCollectorRpc("http://native.example")
	rpc.finality, rpc.nativeExecution = true, true
	waits, debits := 0, 0
	rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(`{"jsonrpc":"2.0","id":2,"result":"foreign"}`)}}, nil
	})
	rpc.afterRead = func(context.Context, int, int) error { debits++; return syscall.EIO }
	rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") }
	raw, err := rpc.call(t.Context(), "chain_getHeader", nil)
	if raw != nil || !errors.Is(err, ErrNativeFinalityConflict) || !errors.Is(err, syscall.EIO) || debits != 1 || waits != 0 || rpc.requests != 1 {
		t.Fatal("failed debit publication hid complete returned contradiction", raw, err, debits, waits)
	}
}

type nativeFinalityTestCauses struct{ causes []error }

func (self *nativeFinalityTestCauses) Error() string   { return "synthetic cause tree" }
func (self *nativeFinalityTestCauses) Unwrap() []error { return self.causes }

func TestNativeExecutionFinalityCancellationTraversalIsBoundedAndExact(t *testing.T) {
	deep := error(context.Canceled)
	for index := 0; index < 40; index++ {
		deep = fmt.Errorf("layer: %w", deep)
	}
	cycle := &nativeFinalityTestCauses{}
	cycle.causes = []error{cycle}
	cases := []struct {
		err      error
		pure     bool
		conflict bool
	}{
		{err: context.Canceled, pure: true},
		{err: fmt.Errorf("owner: %w", context.DeadlineExceeded), pure: true},
		{err: errors.Join(context.Canceled, context.DeadlineExceeded), pure: true},
		{err: errors.Join(context.Canceled, errors.New("invalid signature")), conflict: true},
		{err: &nativeFinalityTestCauses{causes: []error{nil, nil}}},
		{err: &nativeFinalityTestCauses{causes: make([]error, 129)}},
		{err: deep},
		{err: cycle},
	}
	for index, test := range cases {
		if nativeFinalityCancellationOnly(test.err) != test.pure {
			t.Fatalf("case %d admitted an unbounded or non-cancellation cause tree", index)
		}
		actual := nativeFinalityVerificationError(test.err)
		if test.pure {
			if actual != test.err {
				t.Fatalf("case %d changed original cancellation", index)
			}
		} else if test.conflict && !errors.Is(actual, ErrNativeFinalityConflict) {
			t.Fatalf("case %d lost a pure verifier conflict", index)
		} else if !test.conflict && actual != test.err {
			t.Fatalf("case %d invented a conflict from an unobserved cause graph", index)
		}
	}
}

func (self *finalityCapture) readTestBytes(ctx context.Context, name string) ([]byte, error) {
	file, err := openPrivateChild(self.directory, name)
	if err != nil {
		return nil, err
	}
	raw, readErr := readOpened(ctx, file, 1024)
	return raw, errors.Join(readErr, file.Close())
}
