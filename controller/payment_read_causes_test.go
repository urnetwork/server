// Actual public reads retain malformed cause trees without acquiring retries.
package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Constant text permits real cyclic unwraps without recursive fixture logging.
type paymentCauseTestOne struct{ cause error }

func (self *paymentCauseTestOne) Error() string { return "synthetic payment cause" }
func (self *paymentCauseTestOne) Unwrap() error { return self.cause }

// Keep malformed nil branches visible to the production inspector.
type paymentCauseTestMany struct{ causes []error }

func (self *paymentCauseTestMany) Error() string   { return "synthetic payment joined cause" }
func (self *paymentCauseTestMany) Unwrap() []error { return self.causes }

// Is/As claims from arbitrary transport errors cannot authorize continuation.
type paymentCauseTestMatch struct{}

func (self *paymentCauseTestMatch) Error() string { return "synthetic payment matcher" }
func (self *paymentCauseTestMatch) Is(error) bool { panic("payment custom Is invoked") }
func (self *paymentCauseTestMatch) As(any) bool   { panic("payment custom As invoked") }

// Use the same bounded malformed shapes at the public GET and receipt census.
func paymentCauseTestMalformed(leaf error) []error {
	cycle := &paymentCauseTestOne{}
	cycle.cause = cycle
	deep := leaf
	for range 40 {
		deep = &paymentCauseTestOne{cause: deep}
	}
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = leaf
	}
	var typedNil *paymentCauseTestOne
	return []error{cycle, deep, &paymentCauseTestMany{causes: wide}, &paymentCauseTestMany{},
		&paymentCauseTestMany{causes: []error{nil, nil}}, &paymentCauseTestOne{}, typedNil, &paymentCauseTestMatch{}}
}

// Real Circle entry/attempt wrappers must not convert unknown trees to retries.
func TestPaymentReadCircleBoundsMalformedTransportCauses(t *testing.T) {
	for index, cause := range paymentCauseTestMalformed(context.DeadlineExceeded) {
		calls, waits := 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return context.Canceled },
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				calls++
				return nil, cause
			},
		})
		result, err := client.GetTransaction(t.Context(), paymentReadTestId)
		retained := false
		for _, node := range server.InspectErrorCauses(err).Nodes {
			retained = retained || node.Err == cause
		}
		if result != nil || err == nil || calls != 1 || waits != 0 || !retained {
			t.Fatal("malformed public GET cause retried or lost original custody", index, calls, waits, retained)
		}
	}
}

// An EOF from decoding is distinct from a transport EOF even in one join.
func TestPaymentReadCauseOriginsRetainHardAndCancellationDominance(t *testing.T) {
	transport := &paymentReadTransportError{cause: io.EOF}
	status := &paymentReadStatusError{cause: &server.HttpStatusError{StatusCode: http.StatusServiceUnavailable}, retryAfter: "120"}
	if !retryablePaymentReadError(errors.Join(transport, status), false) {
		t.Fatal("typed independent transport/status observations lost retry permission")
	}
	for _, hard := range []error{io.EOF, context.Canceled, &paymentReadCloseError{cause: context.DeadlineExceeded}, &paymentCauseTestMatch{}} {
		if retryablePaymentReadError(errors.Join(transport, status, hard), false) {
			t.Fatal("hard or unowned cause borrowed sibling transport permission")
		}
	}
	now := time.Unix(1, 0)
	if got := paymentReadRetryAfter(status, now, time.Minute); got != time.Minute {
		t.Fatal("bounded retry-after lost its original owner deadline", got)
	}
	cycle := &paymentCauseTestOne{}
	cycle.cause = cycle
	if got := paymentReadRetryAfter(errors.Join(cycle, status), now, time.Minute); got != 0 {
		t.Fatal("incomplete cause census acquired retry-after permission", got)
	}
}

// Only completed typed receipt observations establish absence of all candidates.
func TestStReceiptCensusRejectsMalformedAndMixedAbsenceCauses(t *testing.T) {
	orphan := &stOrphanedTransactionReceiptError{message: "synthetic observed orphan"}
	for _, cause := range []error{nil, orphan, &paymentCauseTestOne{cause: orphan}, errors.Join(orphan, orphan)} {
		if !stTransactionReceiptCensusComplete(cause) {
			t.Fatal("completed receipt absence was withheld")
		}
	}
	causes := append(paymentCauseTestMalformed(orphan), errors.Join(orphan, context.DeadlineExceeded), errors.Join(orphan, context.Canceled))
	for index, cause := range causes {
		if stTransactionReceiptCensusComplete(cause) {
			t.Fatal("incomplete receipt census authorized replacement", index)
		}
	}
}
