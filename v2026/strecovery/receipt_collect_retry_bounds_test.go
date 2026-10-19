// Cause trees are measured by actual unwrap work. RPC controls force complete
// replies with failed body closure through the production retry decision.
package strecovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

type receiptRetryTestSingle struct {
	cause error
	calls *int
}

func (self *receiptRetryTestSingle) Error() string { return "synthetic single cause" }
func (self *receiptRetryTestSingle) Unwrap() error {
	*self.calls++
	return self.cause
}

type receiptRetryTestMany struct {
	causes []error
	calls  *int
}

func (self *receiptRetryTestMany) Error() string { return "synthetic joined causes" }
func (self *receiptRetryTestMany) Unwrap() []error {
	*self.calls++
	return self.causes
}

// An empty wrapper cannot invent a transport timeout from its own methods.
type receiptRetryTestEmptyNetwork struct{}

func (*receiptRetryTestEmptyNetwork) Error() string   { return "synthetic empty timeout" }
func (*receiptRetryTestEmptyNetwork) Unwrap() error   { return nil }
func (*receiptRetryTestEmptyNetwork) Timeout() bool   { return true }
func (*receiptRetryTestEmptyNetwork) Temporary() bool { return true }

type receiptRetryTestIs struct{ calls *int }

func (*receiptRetryTestIs) Error() string { return "synthetic custom identity" }
func (self *receiptRetryTestIs) Is(error) bool {
	*self.calls++
	return true
}

func TestReceiptCollectorRetryDepthAndNodeWorkAreBounded(t *testing.T) {
	for _, depth := range []int{1, 32, 33, 64} {
		calls := 0
		var cause error = io.ErrUnexpectedEOF
		for index := 0; index < depth; index++ {
			cause = &receiptRetryTestSingle{cause: cause, calls: &calls}
		}
		if got := receiptCollectorRetryable(cause); got != (depth <= 32) || calls > 33 {
			t.Fatalf("depth %d exceeded bounded unwrap work or admitted unread leaves: retry=%v calls=%d", depth, got, calls)
		}
	}
	for _, count := range []int{1, 127, 128, 4096} {
		calls := 0
		causes := make([]error, count)
		for index := range causes {
			causes[index] = io.EOF
		}
		cause := &receiptRetryTestMany{causes: causes, calls: &calls}
		if got := receiptCollectorRetryable(cause); got != (count <= 127) || calls != 1 {
			t.Fatalf("width %d exceeded cause budget: retry=%v calls=%d", count, got, calls)
		}
	}
}

func TestReceiptCollectorRetryCyclesStopWithoutUnboundedIdentityWalk(t *testing.T) {
	singleCalls, manyCalls := 0, 0
	single := &receiptRetryTestSingle{calls: &singleCalls}
	single.cause = single
	many := &receiptRetryTestMany{calls: &manyCalls}
	many.causes = []error{io.EOF, many}
	if receiptCollectorRetryable(single) || singleCalls == 0 || singleCalls > 33 {
		t.Fatal("single cycle did not stop at the original traversal bound", singleCalls)
	}
	if receiptCollectorRetryable(many) || manyCalls == 0 || manyCalls > 33 {
		t.Fatal("joined cycle did not stop at the original traversal bound", manyCalls)
	}
	isCalls := 0
	if receiptCollectorRetryable(&receiptRetryTestIs{calls: &isCalls}) || isCalls != 0 {
		t.Fatal("unknown leaf delegated to unbounded custom Is traversal", isCalls)
	}
}

func TestReceiptCollectorRetryEmptyTreesAndMissingCausesRefuse(t *testing.T) {
	calls := 0
	for _, cause := range []error{
		nil,
		&receiptRetryTestMany{calls: &calls},
		&receiptRetryTestMany{causes: []error{nil, nil}, calls: &calls},
		&receiptRetryTestSingle{calls: &calls},
		&receiptRetryTestEmptyNetwork{},
	} {
		if receiptCollectorRetryable(cause) {
			t.Fatal("empty or unobserved cause tree acquired retry permission", cause)
		}
	}
	if !receiptCollectorRetryable(&receiptRetryTestMany{causes: []error{nil, io.EOF, nil}, calls: &calls}) {
		t.Fatal("nil join entries hid an actual bounded transient cause")
	}
}

func TestReceiptCollectorRetryKnownTransportAndHardCausesRemainDistinct(t *testing.T) {
	// The actual failed read was explicitly retryable even though both of
	// Errno's optional network hints are false. The fallback cannot veto it.
	var eioNetwork net.Error = syscall.EIO
	if eioNetwork.Timeout() || eioNetwork.Temporary() {
		t.Fatal("EIO precedence fixture no longer exercises negative network hints")
	}
	permanent := errors.New("synthetic permanent evidence refusal")
	for _, cause := range []error{
		context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
		syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		syscall.EPIPE, syscall.EAGAIN, syscall.EINTR, syscall.EMFILE,
		syscall.ENFILE, syscall.ENOMEM, syscall.EIO,
		&net.DNSError{Err: "synthetic resolver unavailable", IsTemporary: true},
	} {
		if !receiptCollectorRetryable(fmt.Errorf("owned operation: %w", cause)) {
			t.Fatal("bounded existing transport cause lost retry", cause)
		}
		for _, joined := range []error{errors.Join(cause, permanent), errors.Join(permanent, cause), errors.Join(cause, context.Canceled)} {
			if receiptCollectorRetryable(joined) {
				t.Fatal("adjacent transient cause overrode a permanent refusal or canceled owner", joined)
			}
		}
	}
	if receiptCollectorRetryable(&net.DNSError{Err: "synthetic missing name", IsNotFound: true}) {
		t.Fatal("permanent name absence acquired transient retry")
	}
}

func TestReceiptCollectorRpcEmptyAndExcessiveCausesCannotWaitOrPublish(t *testing.T) {
	calls := 0
	var deep error = syscall.EIO
	for index := 0; index < 64; index++ {
		deep = &receiptRetryTestSingle{cause: deep, calls: &calls}
	}
	for _, cause := range []error{
		&receiptRetryTestMany{causes: []error{nil, nil}, calls: &calls},
		deep,
		errors.Join(syscall.EIO, errors.New("synthetic retained identity conflict")),
	} {
		rpc := newReceiptCollectorRpc("http://native.example")
		rpc.finality, rpc.nativeExecution = true, true
		waits := 0
		rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &nativeReadTestBody{
				raw: []byte(`{"jsonrpc":"2.0","id":1,"result":"original"}`), closeErr: cause,
			}}, nil
		})
		rpc.wait = func(context.Context, time.Duration) error {
			waits++
			return errors.New("synthetic forbidden retry")
		}
		raw, err := rpc.call(t.Context(), "chain_getHeader", []any{"original"})
		if raw != nil || err == nil || rpc.requests != 1 || waits != 0 {
			t.Fatal("unclassified close evidence was retried or published", string(raw), err, rpc.requests, waits)
		}
	}
}

func TestReceiptCollectorRpcBoundedJoinedTransientRetainsOriginalDeadline(t *testing.T) {
	rpc := newReceiptCollectorRpc("http://native.example")
	rpc.finality, rpc.nativeExecution = true, true
	calls, waits, chargedBytes := 0, 0, 0
	ownerDeadline := time.Now().Add(300 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), ownerDeadline)
	defer cancel()
	originalRemaining := rpc.remaining
	rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := request.Context().Deadline()
		// Each HTTP attempt has its own shorter physical timeout. It may
		// move between attempts while remaining inside the same owner.
		if !ok || deadline.After(ownerDeadline) || time.Until(deadline) <= 0 || time.Until(deadline) > rpc.client.Timeout {
			t.Fatal("production request escaped its physical or original owner deadline")
		}
		raw := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"original"}`, calls))
		chargedBytes += len(raw)
		body := &nativeReadTestBody{raw: raw}
		if calls == 1 {
			body.closeErr = errors.Join(syscall.EIO, context.DeadlineExceeded)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
	})
	rpc.wait = func(waitCtx context.Context, _ time.Duration) error {
		waits++
		if deadline, ok := waitCtx.Deadline(); !ok || !deadline.Equal(ownerDeadline) {
			t.Fatal("retry replaced the original logical owner deadline", deadline)
		}
		return waitCtx.Err()
	}
	raw, err := rpc.call(ctx, "chain_getHeader", []any{"original"})
	if err != nil || string(raw) != `"original"` || calls != 2 || waits != 1 || rpc.requests != 2 || rpc.remaining != originalRemaining-chargedBytes {
		t.Fatal("bounded transient recovery changed original request budget or evidence", string(raw), err, calls, waits, rpc.remaining)
	}
}
