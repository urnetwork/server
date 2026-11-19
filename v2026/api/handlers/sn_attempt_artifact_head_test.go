// The public head of an attempt object over real local storage: what an
// uploader asks before repeating an upload whose acknowledgement it lost.
package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

func snAttemptHeadTestRequest(kind string, contentHash string) *http.Request {
	return httptest.NewRequest(http.MethodHead, "/sn/attempt-artifact?kind="+kind+"&hash="+contentHash, nil)
}

// A stored object's head carries its typed content, exact length, address and
// the upload encodings this api takes, and no body.
func TestSnAttemptArtifactHeadAnswersAStoredObject(t *testing.T) {
	t.Parallel()
	store := server.NewLocalBlobStore(t.TempDir(), "attempt-api")
	data := []byte("{\"sequence\":1}\n")
	hash := snAttemptTestHash(data)
	for _, kind := range []string{"metadata", "records", "proofs"} {
		if err := startifact.PublishAttemptObject(t.Context(), store, snAttemptTestBounds(), kind, hash, data); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"metadata", "records", "proofs"} {
		response := httptest.NewRecorder()
		serveSnAttemptArtifactHead(response, snAttemptHeadTestRequest(kind, hash), func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), make(chan struct{}, 1))
		contentType := "application/x-ndjson"
		if kind == "metadata" {
			contentType = "application/json"
		}
		header := response.Result().Header
		if response.Code != http.StatusOK || header.Get("Content-Type") != contentType || header.Get("Content-Length") != strconv.Itoa(len(data)) ||
			header.Get("ETag") != `"`+hash+`"` || header.Get("Accept-Encoding") != snAttemptUploadEncodingZstd || response.Body.Len() != 0 {
			t.Fatalf("%s head: status=%d header=%v body=%d", kind, response.Code, header, response.Body.Len())
		}
	}
}

// An object that is not stored is answered 404, which nothing may cache, and
// another object's bytes never answer for it.
func TestSnAttemptArtifactHeadAnswersAbsence(t *testing.T) {
	t.Parallel()
	store := server.NewLocalBlobStore(t.TempDir(), "attempt-api")
	stored := []byte("{\"sequence\":2}\n")
	if err := startifact.PublishAttemptObject(t.Context(), store, snAttemptTestBounds(), "records", snAttemptTestHash(stored), stored); err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		snAttemptHeadTestRequest("records", snAttemptTestHash([]byte("{\"sequence\":3}\n"))),
		snAttemptHeadTestRequest("proofs", snAttemptTestHash(stored)),
	} {
		response := httptest.NewRecorder()
		serveSnAttemptArtifactHead(response, request, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), make(chan struct{}, 1))
		if response.Code != http.StatusNotFound || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("ETag") != "" {
			t.Fatalf("%s: status=%d header=%v", request.URL.RawQuery, response.Code, response.Header())
		}
	}
}

// Malformed requests, other methods, busy slots and an unavailable store are
// refused before storage, like the object's GET.
func TestSnAttemptArtifactHeadRefusesBeforeStorage(t *testing.T) {
	t.Parallel()
	hash := snAttemptTestHash([]byte("{}\n"))
	for _, c := range []struct {
		name    string
		request *http.Request
		slots   chan struct{}
		store   bool
		status  int
	}{
		{name: "query", request: httptest.NewRequest(http.MethodHead, "/sn/attempt-artifact?kind=records", nil), slots: make(chan struct{}, 1), store: true, status: http.StatusBadRequest},
		{name: "kind", request: snAttemptHeadTestRequest("other", hash), slots: make(chan struct{}, 1), store: true, status: http.StatusBadRequest},
		{name: "range", request: func() *http.Request {
			request := snAttemptHeadTestRequest("records", hash)
			request.Header.Set("Range", "bytes=0-1")
			return request
		}(), slots: make(chan struct{}, 1), store: true, status: http.StatusBadRequest},
		{name: "method", request: httptest.NewRequest(http.MethodPut, "/sn/attempt-artifact?kind=records&hash="+hash, nil), slots: make(chan struct{}, 1), store: true, status: http.StatusMethodNotAllowed},
		{name: "busy", request: snAttemptHeadTestRequest("records", hash), slots: func() chan struct{} {
			slots := make(chan struct{}, 1)
			slots <- struct{}{}
			return slots
		}(), store: true, status: http.StatusTooManyRequests},
		{name: "unavailable", request: snAttemptHeadTestRequest("records", hash), slots: make(chan struct{}, 1), store: false, status: http.StatusServiceUnavailable},
	} {
		loads := 0
		response := httptest.NewRecorder()
		serveSnAttemptArtifactHead(response, c.request, func() (server.BlobStore, bool) {
			loads++
			if !c.store {
				return nil, false
			}
			return &snAttemptTestStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "attempt-api"), get: func(context.Context, string) (io.ReadCloser, error) {
				t.Error("a head read an object")
				return nil, io.EOF
			}}, true
		}, snAttemptTestBounds(), c.slots)
		if response.Code != c.status || (c.status != http.StatusServiceUnavailable && loads != 0) {
			t.Fatalf("%s: status=%d want=%d loads=%d", c.name, response.Code, c.status, loads)
		}
	}
}
