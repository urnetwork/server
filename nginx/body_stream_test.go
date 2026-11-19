package nginx

// Tests of a streamed body on its own: what its first read keeps for a retry
// (D7, D19).

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
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
