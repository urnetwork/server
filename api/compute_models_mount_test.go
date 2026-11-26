package api

// The model routes as the api serves them (CSRV-M11-01; DESIGN-COMPUTE §14.3, §6.5, CO-49), through Routes() and the
// router, over a local blob store holding the mirror: a served file's index and chunks with their immutable headers,
// unserved and malformed requests refused, each request counted by its caller's address as the ingress gave it, every
// credential answered as none, and, against Redis, each address's /29 limited per window by its peppered hash.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/handler"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A file put in the mirror and served: its SHA-256, its index's bytes and SHA-256, and its chunks with their hashes.
type testMountedComputeModelFile struct {
	sha256      string
	index       []byte
	indexSha256 string
	chunks      [][]byte
	chunkHashes []string
}

// Puts data in the blob store's mirror in chunks of chunkByteCount, with its index in QD-1408's canonical bytes, and
// writes its state: served, or the state given.
func putTestComputeModelFile(t testing.TB, data []byte, chunkByteCount int, state handler.ComputeModelFileState) *testMountedComputeModelFile {
	t.Helper()
	ctx := context.Background()
	mirror := server.NewComputeModelBlobStore(nil)
	directory := t.TempDir()
	digest := sha256.Sum256(data)
	file := &testMountedComputeModelFile{sha256: hex.EncodeToString(digest[:])}
	for n := 0; n*chunkByteCount < len(data); n += 1 {
		chunk := data[n*chunkByteCount : min((n+1)*chunkByteCount, len(data))]
		chunkDigest := sha256.Sum256(chunk)
		file.chunks = append(file.chunks, chunk)
		file.chunkHashes = append(file.chunkHashes, hex.EncodeToString(chunkDigest[:]))
		path := filepath.Join(directory, "chunk-"+strconv.Itoa(n))
		if err := os.WriteFile(path, chunk, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := mirror.PutChunk(ctx, file.sha256, n, path); err != nil {
			t.Fatal(err)
		}
	}
	index, err := json.Marshal(struct {
		ByteCount      int64    `json:"byte_count"`
		ChunkByteCount int64    `json:"chunk_byte_count"`
		Chunks         []string `json:"chunks"`
		Sha256         string   `json:"sha256"`
		Version        int      `json:"version"`
	}{
		ByteCount:      int64(len(data)),
		ChunkByteCount: int64(chunkByteCount),
		Chunks:         file.chunkHashes,
		Sha256:         file.sha256,
		Version:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	file.index = index
	indexDigest := sha256.Sum256(index)
	file.indexSha256 = hex.EncodeToString(indexDigest[:])
	indexPath := filepath.Join(directory, "index")
	if err := os.WriteFile(indexPath, index, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mirror.PutIndex(ctx, file.sha256, indexPath); err != nil {
		t.Fatal(err)
	}
	record := &handler.ComputeModelFileRecord{State: state, IndexSha256: file.indexSha256}
	if state == handler.ComputeModelFileStateCopied {
		record.IndexSha256 = ""
	}
	if err := mirror.PutFileRecord(ctx, file.sha256, record); err != nil {
		t.Fatal(err)
	}
	return file
}

// One count the routes asked for.
type testComputeModelCount struct {
	clientAddress string
	route         string
	window        time.Duration
	limit         int64
}

// Replaces the routes' count with one that allows every request and records what it was asked.
func setTestComputeModelAddressCount(t testing.TB) func() []testComputeModelCount {
	stateLock := sync.Mutex{}
	counts := []testComputeModelCount{}
	previous := computeModelAddressCount
	computeModelAddressCount = func(ctx context.Context, clientAddress string, route string, window time.Duration, limit int64, now time.Time) (*server.ComputeModelRouteAddressCount, error) {
		stateLock.Lock()
		defer stateLock.Unlock()
		counts = append(counts, testComputeModelCount{clientAddress: clientAddress, route: route, window: window, limit: limit})
		return &server.ComputeModelRouteAddressCount{Allowed: true, Count: 1}, nil
	}
	t.Cleanup(func() {
		computeModelAddressCount = previous
	})
	return func() []testComputeModelCount {
		stateLock.Lock()
		defer stateLock.Unlock()
		return slices.Clone(counts)
	}
}

// Sets the routes' clock to now's value for the rest of the test.
func setTestComputeModelNow(t testing.TB, now *time.Time) {
	previous := computeModelNow
	computeModelNow = func() time.Time {
		return *now
	}
	t.Cleanup(func() {
		computeModelNow = previous
	})
}

// Serves one request through the api's router, from remoteAddr, with the header's lines set.
func serveTestComputeModel(apiRouter http.Handler, method string, path string, remoteAddr string, header map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = remoteAddr
	for name, value := range header {
		request.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	return w
}

// Through Routes(): a served file's index and each chunk answer their bytes with the immutable headers, HEAD answers
// them with no body; a file the mirror is still writing and a chunk past the last answer 404, a malformed request 400;
// and every request is counted under its route's own limit by the address the ingress gave, never by a header the
// caller could choose.
func TestComputeModelRoutesServeTheMirror(t *testing.T) {
	server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 64*1024*1024)
	counted := setTestComputeModelAddressCount(t)
	data := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuvwxyz"), 3)
	file := putTestComputeModelFile(t, data, 40, handler.ComputeModelFileStateServed)
	writing := putTestComputeModelFile(t, data[:50], 40, handler.ComputeModelFileStateIndexed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, Routes())
	remoteAddr := "198.51.100.10:4000"

	index := serveTestComputeModel(apiRouter, http.MethodGet, "/compute/models/"+file.sha256+"/index", remoteAddr, nil)
	if index.Code != http.StatusOK || !bytes.Equal(index.Body.Bytes(), file.index) {
		t.Fatalf("the index: %d %q", index.Code, index.Body.String())
	}
	wantHeader := func(w *httptest.ResponseRecorder, etag string, contentType string, contentLength int) {
		t.Helper()
		header := w.Result().Header
		if header.Get("Cache-Control") != "public, max-age=31536000, immutable" || header.Get("ETag") != "\""+etag+"\"" ||
			header.Get("Content-Type") != contentType || header.Get("Content-Length") != strconv.Itoa(contentLength) ||
			header.Get("Vary") != "" || header.Get("Set-Cookie") != "" {
			t.Fatalf("headers %v", header)
		}
	}
	wantHeader(index, file.indexSha256, "application/json", len(file.index))
	head := serveTestComputeModel(apiRouter, http.MethodHead, "/compute/models/"+file.sha256+"/index", remoteAddr, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD index: %d with %d bytes", head.Code, head.Body.Len())
	}
	wantHeader(head, file.indexSha256, "application/json", len(file.index))

	for n, chunk := range file.chunks {
		path := "/compute/models/" + file.sha256 + "/chunks/" + strconv.Itoa(n)
		w := serveTestComputeModel(apiRouter, http.MethodGet, path, remoteAddr, nil)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), chunk) {
			t.Fatalf("chunk %d: %d with %d bytes", n, w.Code, w.Body.Len())
		}
		wantHeader(w, file.chunkHashes[n], "application/octet-stream", len(chunk))
		head := serveTestComputeModel(apiRouter, http.MethodHead, path, remoteAddr, nil)
		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Fatalf("HEAD chunk %d: %d", n, head.Code)
		}
		wantHeader(head, file.chunkHashes[n], "application/octet-stream", len(chunk))
	}

	for _, refused := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/compute/models/" + writing.sha256 + "/index", http.StatusNotFound},
		{http.MethodGet, "/compute/models/" + writing.sha256 + "/chunks/0", http.StatusNotFound},
		{http.MethodGet, "/compute/models/" + file.sha256 + "/chunks/3", http.StatusNotFound},
		{http.MethodGet, "/compute/models/" + strings.ToUpper(file.sha256) + "/index", http.StatusBadRequest},
		{http.MethodGet, "/compute/models/" + file.sha256 + "/chunks/01", http.StatusBadRequest},
		{http.MethodGet, "/compute/models/" + file.sha256 + "/index?x=1", http.StatusBadRequest},
		{http.MethodPost, "/compute/models/" + file.sha256 + "/index", http.StatusMethodNotAllowed},
		{http.MethodGet, "/compute/models/" + file.sha256 + "/chunks", http.StatusNotFound},
	} {
		w := serveTestComputeModel(apiRouter, refused.method, refused.path, remoteAddr, nil)
		if w.Code != refused.status {
			t.Fatalf("%s %s: %d %q, want %d", refused.method, refused.path, w.Code, w.Body.String(), refused.status)
		}
	}

	settings := handler.DefaultComputeModelRouteSettings()
	counts := counted()
	if len(counts) != 2+2*len(file.chunks)+6 {
		t.Fatalf("counted %d requests: %v", len(counts), counts)
	}
	for _, count := range counts {
		wantCount := testComputeModelCount{clientAddress: remoteAddr, route: handler.ComputeModelRouteChunk, window: settings.ChunkAddressWindow, limit: settings.ChunkAddressCount}
		if count.route == handler.ComputeModelRouteIndex {
			wantCount = testComputeModelCount{clientAddress: remoteAddr, route: handler.ComputeModelRouteIndex, window: settings.IndexAddressWindow, limit: settings.IndexAddressCount}
		}
		if count != wantCount {
			t.Fatalf("counted %+v, want %+v", count, wantCount)
		}
	}

	// the ingress's header names the caller; X-Forwarded-For, which a caller can send, does not
	serveTestComputeModel(apiRouter, http.MethodGet, "/compute/models/"+file.sha256+"/index", remoteAddr, map[string]string{
		"X-UR-Forwarded-For": "203.0.113.7:5000",
	})
	serveTestComputeModel(apiRouter, http.MethodGet, "/compute/models/"+file.sha256+"/index", remoteAddr, map[string]string{
		"X-Forwarded-For": "203.0.113.8",
	})
	counts = counted()
	if counts[len(counts)-2].clientAddress != "203.0.113.7:5000" || counts[len(counts)-1].clientAddress != remoteAddr {
		t.Fatalf("counted by %q and %q", counts[len(counts)-2].clientAddress, counts[len(counts)-1].clientAddress)
	}
}

// Through Routes() and every gate the route table gives them: a sidecar's token, a forwarding client's, a guest's, a
// client's, the network's, an API key, a malformed bearer and a cookie are each answered byte for byte as a request
// with none, no forwarding mark is looked up, and each request is counted by the same address.
func TestComputeModelTokenNotRead(t *testing.T) {
	server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 64*1024*1024)
	counted := setTestComputeModelAddressCount(t)
	file := putTestComputeModelFile(t, bytes.Repeat([]byte("model bytes "), 9), 50, handler.ComputeModelFileStateServed)
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	markLookupCount := 0
	setClientForwardOnly(t, func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
		markLookupCount += 1
		return true, nil
	})
	strictTokens := newStrictExpTokens()
	accessTokens := newRouteAccessTokens()
	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-guest", false, false)
	guest.LagGuest = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, Routes())
	answer := func(w *httptest.ResponseRecorder) string {
		lines := []string{strconv.Itoa(w.Code)}
		for name, values := range w.Result().Header {
			lines = append(lines, name+"="+strings.Join(values, ","))
		}
		slices.Sort(lines[1:])
		return strings.Join(lines, ";") + " " + hex.EncodeToString(w.Body.Bytes())
	}
	credentials := []map[string]string{
		{"Authorization": "Bearer " + strictTokens.strictToken},
		{"Authorization": "Bearer " + strictTokens.clientToken},
		{"Authorization": "Bearer " + accessTokens.networkToken},
		{"Authorization": "Bearer " + accessTokens.apiKey},
		{"Authorization": "Bearer " + guest.Client(server.NewId(), server.NewId()).Testing_Sign()},
		{"Authorization": "Bearer not-a-token"},
		{"Cookie": "by_jwt=" + strictTokens.strictToken},
	}
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/compute/models/" + file.sha256 + "/index"},
		{http.MethodHead, "/compute/models/" + file.sha256 + "/index"},
		{http.MethodGet, "/compute/models/" + file.sha256 + "/chunks/1"},
		{http.MethodHead, "/compute/models/" + file.sha256 + "/chunks/2"},
		{http.MethodGet, "/compute/models/" + strings.Repeat("0", 64) + "/index"},
	} {
		bare := answer(serveTestComputeModel(apiRouter, request.method, request.path, "198.51.100.10:4000", nil))
		for _, header := range credentials {
			if got := answer(serveTestComputeModel(apiRouter, request.method, request.path, "198.51.100.10:4000", header)); got != bare {
				t.Fatalf("%s %s with %.30v: %s, want as with none: %s", request.method, request.path, header, got, bare)
			}
		}
	}
	if markLookupCount != 0 {
		t.Fatalf("a forwarding client's mark was looked up %d times", markLookupCount)
	}
	for _, count := range counted() {
		if count.clientAddress != "198.51.100.10:4000" {
			t.Fatalf("counted by %q", count.clientAddress)
		}
	}
}

// Against Redis, through Routes(), on a fixed clock: an address's /29 takes the index route's count of the window and
// is answered 429 past it, with the rest of the window as Retry-After, another address of its /29 sharing the count;
// an address of another /29 and the chunk route count apart; the next window counts afresh; and Redis holds only the
// peppered hashes' counters, none naming an address or a file.
func TestComputeModelPerAddressLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 64*1024*1024)
		settings := handler.DefaultComputeModelRouteSettings()
		window := settings.IndexAddressWindow
		now := time.Unix(1_800_000_000, 0).UTC().Truncate(window).Add(20 * time.Second)
		setTestComputeModelNow(tb, &now)
		file := putTestComputeModelFile(tb, bytes.Repeat([]byte("weights "), 20), 64, handler.ComputeModelFileStateServed)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		apiRouter := router.NewRouter(ctx, Routes())
		indexPath := "/compute/models/" + file.sha256 + "/index"
		for i := range settings.IndexAddressCount {
			if w := serveTestComputeModel(apiRouter, http.MethodGet, indexPath, "198.51.100.10:4000", nil); w.Code != http.StatusOK {
				tb.Fatalf("request %d of the window: %d %q", i+1, w.Code, w.Body.String())
			}
		}
		for _, remoteAddr := range []string{"198.51.100.10:4000", "198.51.100.13:4100"} {
			refused := serveTestComputeModel(apiRouter, http.MethodGet, indexPath, remoteAddr, nil)
			if refused.Code != http.StatusTooManyRequests || refused.Result().Header.Get("Retry-After") != "40" {
				tb.Fatalf("%s past the limit: %d, Retry-After %q", remoteAddr, refused.Code, refused.Result().Header.Get("Retry-After"))
			}
			if refused.Result().Header.Get("Cache-Control") != "no-store" {
				tb.Fatalf("a 429 may be kept: %v", refused.Result().Header)
			}
		}
		if w := serveTestComputeModel(apiRouter, http.MethodGet, indexPath, "198.51.100.20:4000", nil); w.Code != http.StatusOK {
			tb.Fatalf("another /29: %d", w.Code)
		}
		if w := serveTestComputeModel(apiRouter, http.MethodGet, "/compute/models/"+file.sha256+"/chunks/0", "198.51.100.10:4000", nil); w.Code != http.StatusOK {
			tb.Fatalf("the chunk route: %d", w.Code)
		}
		now = now.Add(window)
		if w := serveTestComputeModel(apiRouter, http.MethodGet, indexPath, "198.51.100.10:4000", nil); w.Code != http.StatusOK {
			tb.Fatalf("the next window: %d", w.Code)
		}

		var keys []string
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			keys, err = r.Keys(ctx, "*").Result()
			if err != nil {
				tb.Fatal(err)
			}
		})
		hashHex := func(address string) string {
			hash := server.ClientIpHashForAddr(netip.MustParseAddr(address))
			return hex.EncodeToString(hash[:])
		}
		windowNumber := now.Add(-window).UnixMilli() / window.Milliseconds()
		want := []string{
			"compute_model_index.{address_" + hashHex("198.51.100.10") + "}.address_" + strconv.FormatInt(windowNumber, 10),
			"compute_model_index.{address_" + hashHex("198.51.100.10") + "}.address_" + strconv.FormatInt(windowNumber+1, 10),
			"compute_model_index.{address_" + hashHex("198.51.100.20") + "}.address_" + strconv.FormatInt(windowNumber, 10),
			"compute_model_chunk.{address_" + hashHex("198.51.100.10") + "}.address_" + strconv.FormatInt(windowNumber, 10),
		}
		modelKeys := []string{}
		for _, key := range keys {
			if strings.Contains(key, "198.51.100") || strings.Contains(key, file.sha256) {
				tb.Fatalf("Redis holds a key naming an address or a file: %s", key)
			}
			if strings.HasPrefix(key, "compute_model_") {
				modelKeys = append(modelKeys, key)
			}
		}
		slices.Sort(modelKeys)
		slices.Sort(want)
		if !slices.Equal(modelKeys, want) {
			tb.Fatalf("the model routes' counters = %v, want %v", modelKeys, want)
		}
	})
}
