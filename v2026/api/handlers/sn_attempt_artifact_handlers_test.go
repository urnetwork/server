// Typed API tests use real local storage and the production validator reader.
// Faults are injected at owned I/O boundaries, without shared service state.
package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

// Small transport budgets are independent of the complete campaign census.
func snAttemptTestBounds() startifact.AttemptObjectBounds {
	return startifact.AttemptObjectBounds{MetadataBytes: 128, RecordBytes: 256, ProofBytes: 192}
}

// Derives the identity of the complete stored wire, not a payload projection.
func snAttemptTestHash(data []byte) string { return fmt.Sprintf("0x%x", sha256.Sum256(data)) }

// Mirrors the deployed client API without importing its private test helpers.
func snAttemptTestValidatorReader(t *testing.T, origin string) *validator.HTTPAttemptStreamV2Reader {
	t.Helper()
	streamBounds := validator.AttemptStreamV2Bounds{
		MaxDataBytes: 1024, MaxItems: 32, MaxChunkBytes: 256, MaxChunks: 8,
		MaxPages: 4, MaxPageBytes: 128, MaxDescriptorsPerPage: 2, MaxManifestBytes: 128,
	}
	reader, err := validator.NewHTTPAttemptStreamV2Reader(origin, validator.AttemptCutV2Bounds{
		MaxHeaderBytes: 128, Records: streamBounds, Proofs: streamBounds,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

// Only the retrieval boundary is replaced; typed keys still use the real store.
type snAttemptTestStore struct {
	server.BlobStore
	get func(context.Context, string) (io.ReadCloser, error)
}

// Each admitted request receives its own reader and cancellation context.
func (self *snAttemptTestStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return self.get(ctx, key)
}

// The caller owns terminal close; no global reader state is replaced.
type snAttemptTestReadCloser struct {
	io.Reader
	close func() error
}

// The callback observes the actual final storage boundary.
func (self *snAttemptTestReadCloser) Close() error {
	if self.close != nil {
		return self.close()
	}
	return nil
}

// A call-local reader can expose the exact cancellation/read ordering.
type snAttemptTestReadFunc func([]byte) (int, error)

// Delegates actual reads, including joined EOF errors.
func (self snAttemptTestReadFunc) Read(value []byte) (int, error) { return self(value) }

// All three types survive real local publication and the real validator client.
func TestSnAttemptArtifactRoundTripWithRealValidatorReader(t *testing.T) {
	t.Parallel()
	store := server.NewLocalBlobStore(t.TempDir(), "attempt-api")
	data := []byte("{\"sequence\":1}\n")
	hash := snAttemptTestHash(data)
	for _, kind := range []string{"metadata", "records", "proofs"} {
		if err := startifact.PublishAttemptObject(t.Context(), store, snAttemptTestBounds(), kind, hash, data); err != nil {
			t.Fatal(err)
		}
	}
	slots := make(chan struct{}, 2)
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sn/attempt-artifact" {
			t.Errorf("reader used another endpoint: %s", r.URL.Path)
		}
		serveSnAttemptArtifact(w, r, func() (server.BlobStore, bool) { calls.Add(1); return store, true }, snAttemptTestBounds(), slots)
	}))
	defer endpoint.Close()
	reader := snAttemptTestValidatorReader(t, endpoint.URL)
	metadata, err := reader.ReadMetadata(t.Context(), hash, uint64(len(data)))
	if err != nil || !bytes.Equal(metadata, data) {
		t.Fatalf("real metadata round trip: bytes=%q error=%v", metadata, err)
	}
	for _, kind := range []string{"records", "proofs"} {
		body, err := reader.OpenData(t.Context(), kind, hash, uint64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		actual, readErr := io.ReadAll(body)
		if err := errors.Join(readErr, body.Close()); err != nil || !bytes.Equal(actual, data) {
			t.Fatalf("real %s round trip: bytes=%q error=%v", kind, actual, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("typed storage request census=%d, want3", calls.Load())
	}
}

// Hash aliases, duplicate keys and untyped parameters fail before storage loads.
func TestSnAttemptArtifactRejectsMalformedQueryBeforeStore(t *testing.T) {
	t.Parallel()
	hash := snAttemptTestHash([]byte("data"))
	calls := 0
	load := func() (server.BlobStore, bool) { calls++; return nil, false }
	for _, query := range []string{
		"", "kind=records", "hash=" + hash, "kind=Records&hash=" + hash,
		"kind=records&hash=" + hash[2:], "kind=records&hash=" + strings.ToUpper(hash),
		"kind=records&hash=0x" + strings.Repeat("0", 64),
		"kind=records&hash=" + hash + "&hash=" + hash,
		"kind=records&kind=proofs&hash=" + hash,
		"kind=records&hash=" + hash + "&size=4",
		"kind=records&hash=" + hash + ";extra=1", "kind=records&hash=%zz",
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?"+query, nil)
		serveSnAttemptArtifact(response, request, load, snAttemptTestBounds(), make(chan struct{}, 1))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %q produced status%d", query, response.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid queries reached storage %d times", calls)
	}
}

// Partial responses and non-GET methods cannot borrow the immutable endpoint.
func TestSnAttemptArtifactRejectsMethodsAndRangeBeforeStore(t *testing.T) {
	t.Parallel()
	query := url.Values{"kind": {"records"}, "hash": {snAttemptTestHash([]byte("data"))}}
	calls := 0
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut} {
		request := httptest.NewRequest(method, "/sn/attempt-artifact?"+query.Encode(), nil)
		want := http.StatusMethodNotAllowed
		if method == http.MethodGet {
			request.Header.Set("Range", "bytes=0-1")
			want = http.StatusBadRequest
		}
		response := httptest.NewRecorder()
		serveSnAttemptArtifact(response, request, func() (server.BlobStore, bool) { calls++; return nil, false }, snAttemptTestBounds(), make(chan struct{}, 1))
		if response.Code != want || (method != http.MethodGet && response.Header().Get("Allow") != http.MethodGet) {
			t.Fatalf("method %s produced status%d Allow=%q", method, response.Code, response.Header().Get("Allow"))
		}
	}
	if calls != 0 {
		t.Fatalf("invalid methods reached storage %d times", calls)
	}
}

// Occupied admission is refused synchronously; pre-cancellation has no effects.
func TestSnAttemptArtifactRefusesBusyAndCanceledAdmission(t *testing.T) {
	t.Parallel()
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	calls := 0
	load := func() (server.BlobStore, bool) { calls++; return nil, false }
	request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash([]byte("data")), nil)
	busy := httptest.NewRecorder()
	serveSnAttemptArtifact(busy, request, load, snAttemptTestBounds(), slots)
	if busy.Code != http.StatusTooManyRequests || busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("busy admission lost bounded refusal: status%d", busy.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := httptest.NewRecorder()
	serveSnAttemptArtifact(canceled, request.WithContext(ctx), load, snAttemptTestBounds(), slots)
	if canceled.Code != http.StatusRequestTimeout || calls != 0 || len(slots) != 1 {
		t.Fatalf("canceled request changed ownership: status%d calls%d slots%d", canceled.Code, calls, len(slots))
	}
	<-slots
}

// Missing deployment bounds or finite admission capacity cannot enable reads.
func TestSnAttemptArtifactRequiresFiniteReaderConfiguration(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash([]byte("data")), nil)
	calls := 0
	load := func() (server.BlobStore, bool) { calls++; return nil, false }
	for _, configuration := range []struct {
		bounds startifact.AttemptObjectBounds
		slots  chan struct{}
		load   func() (server.BlobStore, bool)
	}{
		{bounds: snAttemptTestBounds(), slots: make(chan struct{}, 1)},
		{bounds: startifact.AttemptObjectBounds{}, slots: make(chan struct{}, 1), load: load},
		{bounds: snAttemptTestBounds(), load: load},
		{bounds: snAttemptTestBounds(), slots: make(chan struct{}), load: load},
	} {
		response := httptest.NewRecorder()
		serveSnAttemptArtifact(response, request, configuration.load, configuration.bounds, configuration.slots)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("unbounded configuration admitted: status%d", response.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid configuration reached storage %d times", calls)
	}
}

// An unavailable object never retains a success ETag or immutable error cache.
func TestSnAttemptArtifactStorageFailureClearsAuthority(t *testing.T) {
	t.Parallel()
	base := server.NewLocalBlobStore(t.TempDir(), "attempt-api")
	for _, missingReader := range []bool{false, true} {
		store := &snAttemptTestStore{BlobStore: base, get: func(context.Context, string) (io.ReadCloser, error) {
			if missingReader {
				return nil, nil
			}
			return nil, errors.New("object read failure")
		}}
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash([]byte("data")), nil)
		serveSnAttemptArtifact(response, request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), make(chan struct{}, 1))
		if response.Code != http.StatusBadGateway || response.Header().Get("ETag") != "" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("failed storage retained authority: status%d headers%v", response.Code, response.Header())
		}
	}
}

// A failure after writing bytes must propagate the exact net/http abort signal.
func TestSnAttemptArtifactLateStorageFailureAborts(t *testing.T) {
	t.Parallel()
	data := []byte("original")
	base := server.NewLocalBlobStore(t.TempDir(), "attempt-api")
	for _, failureAt := range []string{"hash", "read", "close", "cancel-close"} {
		ctx, cancel := context.WithCancel(t.Context())
		closed := 0
		reader := &snAttemptTestReadCloser{Reader: bytes.NewReader(data), close: func() error { closed++; return nil }}
		switch failureAt {
		case "hash":
			reader.Reader = bytes.NewReader([]byte("modified"))
		case "read":
			reader.Reader = snAttemptTestReadFunc(func(value []byte) (int, error) {
				return copy(value, data), errors.Join(io.EOF, errors.New("late read failure"))
			})
		case "close":
			reader.close = func() error { closed++; return errors.New("late close failure") }
		case "cancel-close":
			reader.close = func() error { closed++; cancel(); return nil }
		}
		store := &snAttemptTestStore{BlobStore: base, get: func(context.Context, string) (io.ReadCloser, error) { return reader, nil }}
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash(data), nil).WithContext(ctx)
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			serveSnAttemptArtifact(response, request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), make(chan struct{}, 1))
		}()
		cancel()
		if recovered != http.ErrAbortHandler || closed != 1 || response.Body.Len() != len(data) {
			t.Fatalf("%s failure completed a response: recovered=%v closes%d bytes%d", failureAt, recovered, closed, response.Body.Len())
		}
	}
}

// Cancellation is forced while the real storage read owns its admission slot.
func TestSnAttemptArtifactCancellationJoinsReader(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, done := make(chan struct{}), make(chan struct{})
	var closes atomic.Int32
	store := &snAttemptTestStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "attempt-api"), get: func(ctx context.Context, _ string) (io.ReadCloser, error) {
		return &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) {
			close(entered)
			<-ctx.Done()
			return 0, ctx.Err()
		}), close: func() error { closes.Add(1); return nil }}, nil
	}}
	slots := make(chan struct{}, 1)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash([]byte("data")), nil).WithContext(ctx)
	go func() {
		defer close(done)
		serveSnAttemptArtifact(response, request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), slots)
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal("storage read never acquired ownership")
	}
	cancel()
	<-done
	if closes.Load() != 1 || len(slots) != 0 || response.Code != http.StatusBadGateway {
		t.Fatalf("canceled reader leaked ownership: closes%d slots%d status%d", closes.Load(), len(slots), response.Code)
	}
}

// An aborted request releases only its own slot; a later complete read still works.
func TestSnAttemptArtifactAdmissionSlotReleasesOnPanic(t *testing.T) {
	t.Parallel()
	data := []byte("original")
	calls := 0
	store := &snAttemptTestStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "attempt-api"), get: func(context.Context, string) (io.ReadCloser, error) {
		calls++
		if calls == 1 {
			return io.NopCloser(bytes.NewReader([]byte("modified"))), nil
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
	slots := make(chan struct{}, 1)
	request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash(data), nil)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		serveSnAttemptArtifact(httptest.NewRecorder(), request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), slots)
	}()
	response := httptest.NewRecorder()
	serveSnAttemptArtifact(response, request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), slots)
	if recovered != http.ErrAbortHandler || response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), data) || calls != 2 || len(slots) != 0 {
		t.Fatalf("aborted slot blocked or corrupted its successor: recovered%v status%d calls%d slots%d", recovered, response.Code, calls, len(slots))
	}
}

// A record-sized object cannot consume the smaller metadata or proof budget.
func TestSnAttemptArtifactAppliesSeparateTypedLimits(t *testing.T) {
	t.Parallel()
	data := []byte("1234567")
	store := &snAttemptTestStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "attempt-api"), get: func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
	bounds := startifact.AttemptObjectBounds{MetadataBytes: 4, RecordBytes: 8, ProofBytes: 6}
	for _, kind := range []string{"metadata", "records", "proofs"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind="+kind+"&hash="+snAttemptTestHash(data), nil)
		serveSnAttemptArtifact(response, request, func() (server.BlobStore, bool) { return store, true }, bounds, make(chan struct{}, 1))
		want := http.StatusBadGateway
		if kind == "records" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("%s borrowed another typed budget: status%d", kind, response.Code)
		}
	}
}

// Flush only at the actual HTTP write boundary so failure cannot stay buffered.
type snAttemptTestFlushingWriter struct{ http.ResponseWriter }

// The underlying connection, chunk framing and final abort remain real net/http.
func (self snAttemptTestFlushingWriter) Write(data []byte) (int, error) {
	count, err := self.ResponseWriter.Write(data)
	if err == nil {
		self.ResponseWriter.(http.Flusher).Flush()
	}
	return count, err
}

// The public validator reader rejects even hash-correct bytes after a late failure.
func TestSnAttemptArtifactHTTPAbortsFailedStream(t *testing.T) {
	t.Parallel()
	data := []byte("original")
	var closes atomic.Int32
	store := &snAttemptTestStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "attempt-api"), get: func(context.Context, string) (io.ReadCloser, error) {
		return &snAttemptTestReadCloser{Reader: bytes.NewReader(data), close: func() error { closes.Add(1); return errors.New("late storage close failure") }}, nil
	}}
	slots := make(chan struct{}, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSnAttemptArtifact(snAttemptTestFlushingWriter{ResponseWriter: w}, r, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), slots)
	}))
	defer endpoint.Close()
	reader := snAttemptTestValidatorReader(t, endpoint.URL)
	actual, err := reader.ReadMetadata(t.Context(), snAttemptTestHash(data), uint64(len(data)))
	if err == nil || actual != nil || !errors.Is(err, io.ErrUnexpectedEOF) || closes.Load() != 1 {
		t.Fatalf("late failure became successful public evidence: bytes=%q error=%v closes%d", actual, err, closes.Load())
	}
}

// Two readers run simultaneously; a third is refused before obtaining storage.
func TestSnAttemptArtifactConcurrentReadersOwnIndependentSlots(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	data := []byte("complete")
	entered, release := make(chan struct{}, 2), make(chan struct{})
	done := make(chan struct{}, 2)
	var owners sync.WaitGroup
	var calls, closes atomic.Int32
	store := &snAttemptTestStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "attempt-api"), get: func(ctx context.Context, _ string) (io.ReadCloser, error) {
		calls.Add(1)
		first := true
		return &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func(value []byte) (int, error) {
			if first {
				first = false
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return 0, ctx.Err()
				}
				return copy(value, data), io.EOF
			}
			return 0, io.EOF
		}), close: func() error { closes.Add(1); return nil }}, nil
	}}
	slots := make(chan struct{}, 2)
	request := httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=records&hash="+snAttemptTestHash(data), nil).WithContext(ctx)
	responses := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	t.Cleanup(func() { cancel(); owners.Wait() })
	for _, response := range responses {
		owners.Add(1)
		go func() {
			defer owners.Done()
			defer func() { done <- struct{}{} }()
			serveSnAttemptArtifact(response, request.Clone(request.Context()), func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), slots)
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-done:
			t.Fatal("admitted reader returned before both readers owned storage")
		case <-ctx.Done():
			t.Fatal("readers did not reach the shared barrier")
		}
	}
	busy := httptest.NewRecorder()
	serveSnAttemptArtifact(busy, request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), slots)
	close(release)
	for range 2 {
		<-done
	}
	if busy.Code != http.StatusTooManyRequests || calls.Load() != 2 || closes.Load() != 2 || len(slots) != 0 {
		t.Fatalf("concurrent admission lost exact ownership: status%d calls%d closes%d slots%d", busy.Code, calls.Load(), closes.Load(), len(slots))
	}
	for _, response := range responses {
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), data) {
			t.Fatalf("independent reader failed: status%d bytes%q", response.Code, response.Body.Bytes())
		}
	}
}
