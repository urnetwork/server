package handlers

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/urnetwork/server/v2026"
)

// The v1 sweep record's hash: vault1 commits it as epoch 4's artifact hash,
// and the record is a ledger object, so no payout artifact key holds it.
const snArtifactStatusSweepHash = "sha256:f741722e2d3c37b83763b4127221562bc175ad414a64acae7936afffd6b307c8"

// Answers every read of the payout artifact namespace with one fixed outcome.
// Only Prefix and Get are reached by SnArtifact.
type snArtifactStatusStore struct {
	server.BlobStore
	get  func(key string) (io.ReadCloser, error)
	keys []string
}

func (self *snArtifactStatusStore) Prefix() string {
	return "blob"
}

func (self *snArtifactStatusStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	self.keys = append(self.keys, key)
	return self.get(key)
}

// MinIO's GetObject opens lazily: a missing key surfaces at the first Read.
type snArtifactStatusReader struct {
	err error
}

func (self snArtifactStatusReader) Read([]byte) (int, error) {
	return 0, self.err
}

func (self snArtifactStatusReader) Close() error {
	return nil
}

func serveSnArtifactStatus(t *testing.T, store *snArtifactStatusStore, hash string) *httptest.ResponseRecorder {
	t.Helper()
	previous := loadSnArtifactBlobStore
	loadSnArtifactBlobStore = func() (server.BlobStore, bool) { return store, true }
	t.Cleanup(func() { loadSnArtifactBlobStore = previous })
	response := httptest.NewRecorder()
	SnArtifact(response, httptest.NewRequest(http.MethodGet, "/sn/artifact?hash="+hash, nil))
	return response
}

// A hash the store holds nothing under is a missing resource, not a failed
// upstream: before this fix every lookup of the sweep record's hash answered
// 502. Store failures and stored bytes that fail their recheck stay 502.
func TestSnArtifactStatusSeparatesMissingFromStoreFailure(t *testing.T) {
	noSuchKey := minio.ErrorResponse{StatusCode: http.StatusNotFound, Code: minio.NoSuchKey, Message: "The specified key does not exist."}
	for _, test := range []struct {
		name   string
		get    func(key string) (io.ReadCloser, error)
		status int
	}{
		{
			name: "minio key missing at first read",
			get: func(string) (io.ReadCloser, error) {
				return snArtifactStatusReader{err: noSuchKey}, nil
			},
			status: http.StatusNotFound,
		},
		{
			name: "minio key missing at open",
			get: func(string) (io.ReadCloser, error) {
				return nil, &noSuchKey
			},
			status: http.StatusNotFound,
		},
		{
			name: "local file missing",
			get: func(key string) (io.ReadCloser, error) {
				return nil, &fs.PathError{Op: "open", Path: key, Err: fs.ErrNotExist}
			},
			status: http.StatusNotFound,
		},
		{
			name: "bucket missing",
			get: func(string) (io.ReadCloser, error) {
				return snArtifactStatusReader{err: minio.ErrorResponse{StatusCode: http.StatusNotFound, Code: "NoSuchBucket"}}, nil
			},
			status: http.StatusBadGateway,
		},
		{
			name: "store throttled",
			get: func(string) (io.ReadCloser, error) {
				return snArtifactStatusReader{err: minio.ErrorResponse{StatusCode: http.StatusServiceUnavailable, Code: "SlowDownRead"}}, nil
			},
			status: http.StatusBadGateway,
		},
		{
			name: "store unreachable",
			get: func(string) (io.ReadCloser, error) {
				return nil, errors.New("dial tcp 192.0.2.1:23900: connect: connection refused")
			},
			status: http.StatusBadGateway,
		},
		{
			name: "stored bytes are not a payout artifact",
			get: func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(`{"schema":"urnetwork/st-v1-sweep-record/v1"}`)), nil
			},
			status: http.StatusBadGateway,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &snArtifactStatusStore{get: test.get}
			response := serveSnArtifactStatus(t, store, snArtifactStatusSweepHash)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %q", response.Code, test.status, response.Body.String())
			}
			want := "blob/st/v1/content/sha256/" + strings.TrimPrefix(snArtifactStatusSweepHash, "sha256:") + ".json"
			if len(store.keys) != 1 || store.keys[0] != want {
				t.Fatalf("read keys = %q, want [%q]", store.keys, want)
			}
			if test.status != http.StatusOK && (response.Header().Get("ETag") != "" || response.Header().Get("Cache-Control") != "") {
				t.Fatalf("a failed lookup carried immutable caching headers: %v", response.Header())
			}
			if test.status == http.StatusNotFound && !strings.Contains(response.Body.String(), "/sn/ledger/object") {
				t.Fatalf("missing artifact body does not point at ledger objects: %q", response.Body.String())
			}
		})
	}
}

// A malformed hash is refused before any store read, as before.
func TestSnArtifactStatusMalformedHashIsBadRequest(t *testing.T) {
	store := &snArtifactStatusStore{get: func(string) (io.ReadCloser, error) {
		t.Fatal("a malformed hash reached the store")
		return nil, nil
	}}
	response := serveSnArtifactStatus(t, store, "sha256:f741")
	if response.Code != http.StatusBadRequest || len(store.keys) != 0 {
		t.Fatalf("status = %d, reads = %d", response.Code, len(store.keys))
	}
}
