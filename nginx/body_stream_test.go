package nginx

// Tests of a streamed body on its own: what its first read keeps for a retry
// (D7, D19), and which failures it keeps as the client's, a client's reset
// as canceled.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// A streamed body's first read keeps a declared body it returns whole, as
// nginx buffers a body its first pass reads whole: in memory, reserved under
// the store's budget, replayed byte for byte, and released with the body. It
// keeps nothing of a body the first read returns only part of, though a later
// read returns the rest, nor of a chunked body, nor without a store, nor when
// the store has no room, where the body reads on unrefused. Each is read as
// the request's writer reads it, no further than a declared length.
func TestStreamedBodyFirstReadKeepsAWholeBody(t *testing.T) {
	for _, c := range []struct {
		name              string
		body              io.Reader
		declaredLength    int64
		memoryBudgetBytes int64
		withoutStore      bool
		wantKept          bool
	}{
		{name: "declared whole", body: bytes.NewReader([]byte("whole body")), declaredLength: 10, wantKept: true},
		{name: "declared whole with its end", body: iotest.DataErrReader(bytes.NewReader([]byte("whole body"))), declaredLength: 10, wantKept: true},
		{name: "declared in two reads", body: iotest.HalfReader(bytes.NewReader([]byte("whole body"))), declaredLength: 10, wantKept: false},
		{name: "chunked whole", body: iotest.DataErrReader(bytes.NewReader([]byte("whole body"))), declaredLength: -1, wantKept: false},
		{name: "without a store", body: bytes.NewReader([]byte("whole body")), declaredLength: 10, withoutStore: true, wantKept: false},
		{name: "over the budget", body: bytes.NewReader([]byte("whole body")), declaredLength: 10, memoryBudgetBytes: 9, wantKept: false},
	} {
		settings := DefaultBodySettings()
		if 0 < c.memoryBudgetBytes {
			settings.MemoryBudgetBytes = c.memoryBudgetBytes
		}
		bodyStore, spyFiles := newSpyBodyStore(t, settings)
		keepBodyStore := bodyStore
		if c.withoutStore {
			keepBodyStore = nil
		}
		streamedBody := newForwardStreamedBody(testContext(t), c.body, c.declaredLength, 0, 0, nil, keepBodyStore)
		reader := io.Reader(streamedBody)
		if 0 <= c.declaredLength {
			reader = io.LimitReader(streamedBody, c.declaredLength)
		}
		if read, err := io.ReadAll(reader); err != nil || string(read) != "whole body" {
			t.Fatalf("%s: read %q %v, want the whole body", c.name, read, err)
		}
		if streamedBody.failed() {
			t.Errorf("%s: the body failed with %v", c.name, streamedBody.failure())
		}
		keptBody := streamedBody.kept()
		if (keptBody != nil) != c.wantKept {
			t.Fatalf("%s: kept %v, want %v", c.name, keptBody != nil, c.wantKept)
		}
		if keptBody != nil {
			if replay := readStoredBody(t, keptBody); string(replay) != "whole body" || keptBody.spilled() || keptBody.length() != 10 {
				t.Errorf("%s: kept %q, spilled %v, want the whole body in memory", c.name, replay, keptBody.spilled())
			}
			if memoryBytes, _, _, bodies := bodyStore.usage(); memoryBytes != 10 || bodies != 1 {
				t.Errorf("%s: the store reserves %d bytes for %d bodies, want the body's 10 for one", c.name, memoryBytes, bodies)
			}
		} else {
			checkStoreEmpty(t, bodyStore)
		}
		streamedBody.release()
		checkStoreEmpty(t, bodyStore)
		if spyFiles.count() != 0 {
			t.Errorf("%s: %d spill files created, want none", c.name, spyFiles.count())
		}
	}
}

// A client body read that waits on the test, then fails with a timeout, as
// a read past its deadline fails.
type heldTimeoutReader struct {
	waiting chan struct{}
	fail    chan struct{}
}

// Signals that it waits, then fails once the test lets it.
func (self *heldTimeoutReader) Read(p []byte) (int, error) {
	self.waiting <- struct{}{}
	<-self.fail
	return 0, os.ErrDeadlineExceeded
}

// A read whose own client_body_timeout passed timed out on the client's
// side, though the body was interrupted before the failure returned to it,
// as on h1, where Go's server cancels the request inside the timed out read
// and the exchange interrupts the body on that cancellation. A read the
// interrupt's past deadline ended before its own isn't the client's. In a
// synctest bubble, so time passes only as the test says.
func TestStreamedBodyTimeoutBeforeAnInterruptIsTheClients(t *testing.T) {
	const readTimeout = 50 * time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			name string
			// when the interrupt comes, after the read began
			interruptAfter time.Duration
			wantRefusal    bool
		}{
			{name: "the read's own deadline passed", interruptAfter: 2 * readTimeout, wantRefusal: true},
			{name: "the interrupt came first", interruptAfter: readTimeout / 2, wantRefusal: false},
		} {
			reader := &heldTimeoutReader{
				waiting: make(chan struct{}),
				fail:    make(chan struct{}),
			}
			deadlines := make(chan time.Time, 8)
			streamedBody := newForwardStreamedBody(t.Context(), reader, 100, 0, readTimeout, func(deadline time.Time) error {
				deadlines <- deadline
				return nil
			}, nil)
			readErrs := make(chan error, 1)
			go func() {
				_, err := streamedBody.Read(make([]byte, 100))
				readErrs <- err
			}()
			<-reader.waiting
			time.Sleep(c.interruptAfter)
			streamedBody.interrupt()
			// the failure returns to the body only after the interrupt
			close(reader.fail)
			if err := <-readErrs; err != os.ErrDeadlineExceeded {
				t.Errorf("%s: the read ended with %v", c.name, err)
			}
			refusal := streamedBody.failure()
			if c.wantRefusal && (refusal == nil || refusal.reason != bodyRefusalTimeout) {
				t.Errorf("%s: the body's failure is %v, want the client's timeout", c.name, refusal)
			}
			if !c.wantRefusal && refusal != nil {
				t.Errorf("%s: the body's failure is %v, want none of the client's", c.name, refusal)
			}
			if armed := <-deadlines; armed.IsZero() {
				t.Errorf("%s: the read armed no deadline", c.name)
			}
		}
	})
}

// A declared body whose reads end, without an error, short of its length, as
// the vendored http3 ends one at the stream's end, fails as the client's 400
// with an unexpected end, so its request's write fails and nothing marks it
// whole; one of unknown length, or one ending at its length, reads to its
// end.
func TestStreamedBodyEndingShortOfItsLengthFails(t *testing.T) {
	for _, c := range []struct {
		name           string
		body           io.Reader
		declaredLength int64
		wantFailure    bool
	}{
		{name: "short", body: bytes.NewReader([]byte("short")), declaredLength: 10, wantFailure: true},
		{name: "short with its end", body: iotest.DataErrReader(bytes.NewReader([]byte("short"))), declaredLength: 10, wantFailure: true},
		{name: "whole", body: bytes.NewReader([]byte("whole body")), declaredLength: 10},
		{name: "of unknown length", body: bytes.NewReader([]byte("short")), declaredLength: -1},
	} {
		streamedBody := newForwardStreamedBody(testContext(t), c.body, c.declaredLength, 0, 0, nil, nil)
		reader := io.Reader(streamedBody)
		if 0 <= c.declaredLength {
			reader = io.LimitReader(streamedBody, c.declaredLength)
		}
		_, err := io.ReadAll(reader)
		refusal := streamedBody.failure()
		if c.wantFailure {
			if err != io.ErrUnexpectedEOF {
				t.Errorf("%s: the read ended with %v, want an unexpected end", c.name, err)
			}
			if refusal == nil || refusal.status != http.StatusBadRequest || refusal.reason != bodyRefusalIncomplete {
				t.Errorf("%s: the body's failure is %v, want the client's 400", c.name, refusal)
			}
			if streamedBody.isWhole() {
				t.Errorf("%s: the short body is marked whole", c.name)
			}
		} else {
			if err != nil || refusal != nil {
				t.Errorf("%s: the read ended with %v, failure %v", c.name, err, refusal)
			}
			if !streamedBody.isWhole() {
				t.Errorf("%s: the body read to its end isn't whole", c.name)
			}
		}
	}
}

// Regression test: a streamed body whose read fails because its client reset
// it is the client's failure as canceled, with no response, while the
// request's context is still live, as an h3 client's reset of its request's
// side leaves it; the read's error passes to the request's writer. Before,
// it was an incomplete body's 400.
func TestStreamedBodyResetByItsClientIsCanceled(t *testing.T) {
	for _, resetErr := range []error{
		testConnectionResetReadError(t),
		&http3.Error{Remote: true, ErrorCode: http3.ErrCodeRequestCanceled},
	} {
		streamedBody := newForwardStreamedBody(testContext(t), io.MultiReader(bytes.NewReader([]byte("partial")), errorReader{err: resetErr}), 100, 0, 0, nil, nil)
		if _, err := io.ReadAll(streamedBody); !errors.Is(err, resetErr) {
			t.Errorf("the read ended with %v, want the reset's %v", err, resetErr)
		}
		if refusal := streamedBody.failure(); refusal == nil || refusal.status != 0 || refusal.reason != bodyRefusalCanceled {
			t.Errorf("a body its client reset (%v): failure %v, want canceled with no response", resetErr, refusal)
		}
		if streamedBody.isWhole() {
			t.Errorf("the reset body is marked whole")
		}
	}
}
