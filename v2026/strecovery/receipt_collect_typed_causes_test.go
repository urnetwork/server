// Typed missing receivers never establish transport, cancellation or missing
// certificate evidence. Real RPC and capture controls retain original debits.
package strecovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// These methods deliberately permit a nil receiver; calling them would mint
// retry permission from an absent network observation.
type receiptTypedNetwork struct{}

func (*receiptTypedNetwork) Error() string   { return "synthetic network cause" }
func (*receiptTypedNetwork) Timeout() bool   { return true }
func (*receiptTypedNetwork) Temporary() bool { return true }

// A non-pointer nil kind must also be refused before its unwrap method runs.
type receiptTypedSlice []error

func (receiptTypedSlice) Error() string   { return "synthetic slice cause" }
func (receiptTypedSlice) Unwrap() []error { return []error{io.EOF} }

// Identity matchers are never part of the cause observation contract.
type receiptTypedMatcher struct{}

func (*receiptTypedMatcher) Error() string { return "synthetic custom matcher" }
func (*receiptTypedMatcher) Is(error) bool { panic("unexpected custom Is dispatch") }
func (*receiptTypedMatcher) As(any) bool   { panic("unexpected custom As dispatch") }

// Panic is a fixture assertion rather than an uncaught process failure, so an
// old walker omission is distinguished from a setup error.
func receiptTypedNoDispatch(t *testing.T) {
	t.Helper()
	if value := recover(); value != nil {
		t.Fatalf("typed-nil cause dispatched an absent receiver: %v", value)
	}
}

func TestReceiptCollectorTypedNilLeavesCannotGrantRetry(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	var network *receiptTypedNetwork
	var dns *net.DNSError
	var slice receiptTypedSlice
	for index, cause := range []error{network, dns, slice} {
		if receiptCollectorRetryable(cause) || receiptCollectorRetryable(errors.Join(io.EOF, cause)) {
			t.Fatal("typed-nil leaf acquired retry permission", index)
		}
	}
	if !receiptCollectorRetryable(&receiptTypedNetwork{}) {
		t.Fatal("actual transient network leaf lost retry")
	}
}

func TestReceiptCollectorTypedNilWrappersCannotDispatchOrMatch(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	var single *receiptRetryTestSingle
	var many *receiptRetryTestMany
	var missing *nativeFinalityUnavailableError
	for index, cause := range []error{single, many, missing, &receiptTypedMatcher{}, errors.Join(io.EOF, single), errors.Join(many, context.DeadlineExceeded)} {
		if receiptCollectorRetryable(cause) {
			t.Fatal("missing wrapper or custom matcher acquired retry", index)
		}
	}
}

// Actual HTTP transport errors cross the standard url.Error wrapper before
// reaching the retry owner. No manual reclassification replaces that path.
func TestReceiptCollectorRpcTypedNilTransportCannotWaitOrPublish(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	var network *receiptTypedNetwork
	var single *receiptRetryTestSingle
	for index, cause := range []error{network, single, errors.Join(syscall.EIO, network)} {
		rpc := newReceiptCollectorRpc("http://retry.example")
		calls, waits, debits := 0, 0, 0
		rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, cause
		})
		rpc.afterRead = func(context.Context, int, int) error { debits++; return nil }
		rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("synthetic forbidden wait") }
		raw, err := rpc.call(t.Context(), "eth_chainId", nil)
		if raw != nil || err == nil || calls != 1 || rpc.requests != 1 || waits != 0 || debits != 1 {
			t.Fatal("typed-nil transport waited or published a result", index, calls, waits, debits)
		}
	}
}

// Complete returned bytes do not erase a failed read/close observation. Every
// profile charges them once while retaining the typed missing cause as unknown.
func TestReceiptCollectorRpcTypedNilBodyAndCloseCannotPublish(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	var network *receiptTypedNetwork
	var single *receiptRetryTestSingle
	var many *receiptRetryTestMany
	for _, profile := range []string{"receipt", "native", "proof"} {
		for _, closeFailure := range []bool{false, true} {
			for index, cause := range []error{network, single, many, errors.Join(io.EOF, network)} {
				rpc := newReceiptCollectorRpc("http://retry.example")
				method := "eth_chainId"
				if profile == "native" {
					rpc.finality, rpc.nativeExecution = true, true
					method = "chain_getHeader"
				} else if profile == "proof" {
					rpc.nativeProof = true
					method = "state_getReadProof"
				}
				waits, debits, charged := 0, 0, 0
				remaining := rpc.remaining
				original := []byte(`{"jsonrpc":"2.0","id":1,"result":"original"}`)
				rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
					body := &nativeReadTestBody{raw: append([]byte(nil), original...)}
					if closeFailure {
						body.closeErr = cause
					} else {
						body.tail = cause
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
				})
				rpc.afterRead = func(_ context.Context, _ int, size int) error { debits++; charged += size; return nil }
				rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("synthetic forbidden wait") }
				raw, err := rpc.call(t.Context(), method, []any{"original"})
				if raw != nil || err == nil || rpc.requests != 1 || waits != 0 || debits != 1 || charged != len(original) || remaining-rpc.remaining != len(original) {
					t.Fatal("typed-nil body or close waited, published or lost debit", profile, closeFailure, index, waits, debits, charged)
				}
			}
		}
	}
}

// A present receiver still recovers automatically within the same deadline;
// retaining the first failed close does not narrow healthy transient policy.
func TestReceiptCollectorRpcPresentTypedNetworkRecoversOriginalRead(t *testing.T) {
	rpc := newReceiptCollectorRpc("http://retry.example")
	calls, waits := 0, 0
	ownerDeadline := time.Now().Add(300 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), ownerDeadline)
	defer cancel()
	rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		current, ok := request.Context().Deadline()
		if !ok || current.After(ownerDeadline) || time.Until(current) <= 0 || time.Until(current) > rpc.client.Timeout {
			t.Fatal("typed transient request escaped its physical or original owner deadline")
		}
		body := &nativeReadTestBody{raw: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"original"}`, calls))}
		if calls == 1 {
			body.closeErr = errors.Join(&receiptTypedNetwork{}, syscall.EIO)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
	})
	rpc.wait = func(waitCtx context.Context, _ time.Duration) error {
		waits++
		if current, ok := waitCtx.Deadline(); !ok || !current.Equal(ownerDeadline) {
			t.Fatal("typed transient recovery reset the original logical owner deadline", current)
		}
		return waitCtx.Err()
	}
	raw, err := rpc.call(ctx, "eth_chainId", []any{"original"})
	if err != nil || string(raw) != `"original"` || calls != 2 || waits != 1 {
		t.Fatal("actual typed transient did not recover original read", calls, waits, err)
	}
}

// Incomplete graphs with no observed verifier refusal remain unknown, while
// a real hard sibling cannot disappear behind cancellation or a missing node.
func TestNativeFinalityTypedNilNeverInventsCancellationOrConflict(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	var single *receiptRetryTestSingle
	var many *receiptRetryTestMany
	var leaf *receiptTypedNetwork
	for index, cause := range []error{single, many, leaf, errors.Join(context.Canceled, single)} {
		if nativeFinalityCancellationOnly(cause) {
			t.Fatal("typed-nil finality cause invented pure cancellation", index)
		}
		result := nativeFinalityVerificationError(cause)
		if result != cause {
			t.Fatal("unobserved finality cause invented evidence conflict", index)
		}
	}
	hard := errors.New("synthetic invalid signature")
	for _, cause := range []error{errors.Join(context.Canceled, single, hard), errors.Join(hard, many)} {
		found := false
		for _, node := range server.InspectErrorCauses(nativeFinalityVerificationError(cause)).Nodes {
			found = found || node.Err == ErrNativeFinalityConflict
		}
		if !found {
			t.Fatal("observed verifier refusal lost precedence to missing sibling")
		}
	}
	if !nativeFinalityCancellationOnly(errors.Join(context.Canceled, context.DeadlineExceeded)) {
		t.Fatal("complete cancellation observations lost their original classification")
	}
}

// Missing certificate retry does not invoke Is or unwrap an absent receiver.
// Its all-leaves rule also excludes cancellation or an actual joined conflict.
func TestNativeFinalityUnavailableRequiresCompleteObservedCause(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	var missing *nativeFinalityUnavailableError
	var single *receiptRetryTestSingle
	calls := 0
	cycle := &receiptRetryTestSingle{calls: &calls}
	cycle.cause = cycle
	for index, cause := range []error{missing, single, &receiptTypedMatcher{}, cycle,
		errors.Join(ErrNativeFinalityUnavailable, missing), errors.Join(ErrNativeFinalityUnavailable, context.Canceled), errors.Join(ErrNativeFinalityUnavailable, ErrNativeFinalityConflict)} {
		if nativeFinalityUnavailableOnly(cause) {
			t.Fatal("unobserved or mixed finality cause acquired missing-certificate retry", index)
		}
	}
	if calls != 32 || !nativeFinalityUnavailableOnly(&nativeFinalityUnavailableError{reason: "synthetic missing certificate"}) {
		t.Fatal("missing-certificate traversal lost bounded work or real missing evidence", calls)
	}
}

// The public capture writes its actual request/debit journal, refuses the
// unknown transport tail, then resumes the same authority once reads recover.
func TestNativeExecutionCaptureTypedNilCloseRetainsJournalWithoutProof(t *testing.T) {
	defer receiptTypedNoDispatch(t)
	f := receiptFinalityTestFixture(t)
	peer := captureTestRpcFixture(f)
	config, directory := nativeExecutionCaptureTestInputs(t, f)
	var missing *receiptTypedNetwork
	waits := 0
	result, err := captureNativeExecutionFinality(t.Context(), f.checkpoint, config, directory, func(client *receiptCollectorRpc) {
		peer.configure(client)
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
			response.Body = &nativeReadTestBody{raw: raw, closeErr: missing}
			return response, nil
		})
		client.wait = func(context.Context, time.Duration) error { waits++; return errors.New("synthetic forbidden wait") }
	})
	if result != nil || err == nil || waits != 0 || len(peer.calls) != 1 {
		t.Fatal("typed-nil capture close retried or published finality", waits, len(peer.calls))
	}
	for _, node := range server.InspectErrorCauses(err).Nodes {
		if node.Err == ErrNativeFinalityConflict {
			t.Fatal("unknown capture close became an invented evidence conflict")
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "native-proof.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown capture close published complete proof", err)
	}
	original := map[string][]byte{}
	for _, name := range []string{"native-capture.json", "request-00001.json", "read-00001.json"} {
		raw, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal("failed capture lost original authority or charged read", name, err)
		}
		original[name] = raw
	}
	result, err = captureNativeExecutionFinality(t.Context(), f.checkpoint, config, directory, peer.configure)
	if err != nil || result == nil || result.Finality.Parent != config.Parent || result.Finality.Child != config.Child {
		t.Fatal("healthy capture could not resume original charged authority", err)
	}
	for name, before := range original {
		after, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("healthy recovery rewrote original authority or debit", name, err)
		}
	}
}
