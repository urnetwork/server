package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/replication"
)

// Prove the configured non-local service account can perform every operation
// required by the simulator's content-addressed evidence publisher.
func TestLiveBlobStoreContentAddressedCanary(t *testing.T) {
	if os.Getenv("SIM_TESTNET_LIVE_BLOB") != "1" {
		t.Skip("set SIM_TESTNET_LIVE_BLOB=1 with WARP_ENV to probe the configured server/blob store")
	}
	store, ok := LoadBlobStore()
	if !ok {
		t.Fatal("configured MinIO blob store is unavailable")
	}
	if store.Authority() == "local" {
		t.Fatalf("configured blob store is local: %q", store.Authority())
	}
	content := []byte("urnetwork sim-testnet MinIO canary v1\n")
	hash := sha256.Sum256(content)
	key := path.Join(store.Prefix(), "sim-testnet", "preflight", "sha256", fmt.Sprintf("%x", hash[:]))
	localPath := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.PutIfAbsent(ctx, key, localPath, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, content) {
		t.Fatalf("MinIO canary read=%q read_error=%v close_error=%v", actual, readErr, closeErr)
	}
	objects, err := store.List(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Key != key || objects[0].Size != int64(len(content)) {
		t.Fatalf("MinIO canary listing=%+v", objects)
	}
}

func TestLocalBlobStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStore(root, "stats")
	ctx := context.Background()

	if store.Prefix() != "stats" {
		t.Fatalf("prefix = %q, want stats", store.Prefix())
	}
	if store.Bucket() != "local" {
		t.Fatalf("bucket = %q, want local", store.Bucket())
	}

	srcPath := filepath.Join(t.TempDir(), "seg.pb.zst")
	content := []byte("hello blob content")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	key := "stats/local/findproviders2/2026-07-19/inst/1234-0.pb.zst"
	if err := store.Put(ctx, key, srcPath, "application/zstd"); err != nil {
		t.Fatalf("put: %s", err)
	}

	objects, err := store.List(ctx, "stats/local/")
	if err != nil {
		t.Fatalf("list: %s", err)
	}
	if len(objects) != 1 || objects[0].Key != key || objects[0].Size != int64(len(content)) {
		t.Fatalf("list = %+v, want one object key=%s size=%d", objects, key, len(content))
	}

	reader, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %s", err)
	}
	got, _ := io.ReadAll(reader)
	reader.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("get content = %q, want %q", got, content)
	}

	// a non-matching prefix returns empty (not everything)
	if none, err := store.List(ctx, "stats/other/"); err != nil || len(none) != 0 {
		t.Fatalf("non-matching prefix: got %v err=%v, want empty", none, err)
	}

	// a partial (in-progress) write is never listed
	if err := os.WriteFile(store.(*localBlobStore).pathFor(key)+blobPartialSuffix, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if objs, _ := store.List(ctx, "stats/local/"); len(objs) != 1 {
		t.Fatalf("partial write leaked into list: %+v", objs)
	}

	// a non-existent root is empty, not an error
	if objs, err := NewLocalBlobStore(filepath.Join(root, "nope"), "").List(ctx, ""); err != nil || len(objs) != 0 {
		t.Fatalf("non-existent root: got %v err=%v, want empty", objs, err)
	}
}

func TestLocalBlobStorePutIfAbsentIsAtomicAcrossInstances(t *testing.T) {
	root := t.TempDir()
	stores := []*localBlobStore{
		NewLocalBlobStore(root, "blob").(*localBlobStore),
		NewLocalBlobStore(root, "blob").(*localBlobStore),
	}
	contents := [][]byte{[]byte("first immutable value"), []byte("second immutable value")}
	sourcePaths := make([]string, len(contents))
	for i, content := range contents {
		sourcePaths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("source-%d", i))
		if err := os.WriteFile(sourcePaths[i], content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan int, len(stores))
	release := make(chan struct{})
	for i, store := range stores {
		writerIndex := i
		store.beforeCreateCommitForTest = func() {
			entered <- writerIndex
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	type putResult struct {
		writerIndex int
		created     bool
		err         error
	}
	results := make(chan putResult, len(stores))
	for i, store := range stores {
		writerIndex := i
		go func() {
			created, err := store.PutIfAbsent(ctx, "blob/evidence.json", sourcePaths[writerIndex], "application/json")
			results <- putResult{writerIndex: writerIndex, created: created, err: err}
		}()
	}
	for range stores {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			t.Fatalf("writers did not reach the commit barrier: %v", ctx.Err())
		}
	}
	close(release)

	createdIndex := -1
	for range stores {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("writer %d: %v", result.writerIndex, result.err)
			}
			if result.created {
				if createdIndex != -1 {
					t.Fatalf("writers %d and %d both created the key", createdIndex, result.writerIndex)
				}
				createdIndex = result.writerIndex
			}
		case <-ctx.Done():
			t.Fatalf("writers did not leave the commit barrier: %v", ctx.Err())
		}
	}
	if createdIndex == -1 {
		t.Fatal("neither writer created the key")
	}
	reader, err := stores[0].Get(ctx, "blob/evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	winner, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(winner, contents[createdIndex]) {
		t.Fatalf("winner = %q, read error = %v, close error = %v", winner, readErr, closeErr)
	}
	var partialPaths []string
	if err := filepath.Walk(root, func(filePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(filePath, blobPartialSuffix) {
			partialPaths = append(partialPaths, filePath)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(partialPaths) != 0 {
		t.Fatalf("partial files remain after the race: %v", partialPaths)
	}
}

func TestLocalBlobStorePutIfAbsentPreservesCapacity(t *testing.T) {
	store := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "blob", 5)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if err := os.WriteFile(first, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("56"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := store.PutIfAbsent(ctx, "blob/first", first, "application/octet-stream")
	if err != nil || !created {
		t.Fatalf("first create = %t, %v", created, err)
	}
	if created, err := store.PutIfAbsent(ctx, "blob/second", second, "application/octet-stream"); err == nil || created {
		t.Fatalf("over-capacity create = %t, %v", created, err)
	}
	if created, err := store.PutIfAbsent(ctx, "blob/first", second, "application/octet-stream"); err != nil || created {
		t.Fatalf("existing key create = %t, %v", created, err)
	}
	reader, err := store.Get(ctx, "blob/first")
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(content) != "1234" {
		t.Fatalf("stored content = %q, read error = %v, close error = %v", content, readErr, closeErr)
	}
}

// Decodes the application-level framing emitted by SigV4 streaming uploads.
// Every chunk and the terminal chunk must be complete and carry a hex signature.
func decodeAWSSignedChunkedBody(encoded []byte) ([]byte, error) {
	const signaturePrefix = ";chunk-signature="
	decoded := make([]byte, 0, len(encoded))
	remaining := encoded
	for {
		headerEnd := bytes.Index(remaining, []byte("\r\n"))
		if headerEnd < 0 {
			return nil, errors.New("aws signed chunk header is truncated")
		}
		header := string(remaining[:headerEnd])
		remaining = remaining[headerEnd+2:]
		sizeHex, signature, ok := strings.Cut(header, signaturePrefix)
		if !ok || sizeHex == "" {
			return nil, fmt.Errorf("invalid aws signed chunk header %q", header)
		}
		for i := range sizeHex {
			sizeByte := sizeHex[i]
			if ('0' <= sizeByte && sizeByte <= '9') ||
				('a' <= sizeByte && sizeByte <= 'f') ||
				('A' <= sizeByte && sizeByte <= 'F') {
				continue
			}
			return nil, fmt.Errorf("invalid aws signed chunk size %q", sizeHex)
		}
		chunkSize, err := strconv.ParseUint(sizeHex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid aws signed chunk size %q: %w", sizeHex, err)
		}
		if len(signature) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid aws signed chunk signature length %d", len(signature))
		}
		if _, err := hex.DecodeString(signature); err != nil {
			return nil, fmt.Errorf("invalid aws signed chunk signature: %w", err)
		}
		if uint64(len(remaining)) < chunkSize {
			return nil, fmt.Errorf("aws signed chunk data is truncated: have %d bytes, want %d", len(remaining), chunkSize)
		}
		chunkSizeInt := int(chunkSize)
		decoded = append(decoded, remaining[:chunkSizeInt]...)
		remaining = remaining[chunkSizeInt:]
		if len(remaining) < 2 || remaining[0] != '\r' || remaining[1] != '\n' {
			return nil, errors.New("aws signed chunk data delimiter is missing")
		}
		remaining = remaining[2:]
		if chunkSize == 0 {
			if len(remaining) != 0 {
				return nil, fmt.Errorf("aws signed chunk body has %d trailing bytes", len(remaining))
			}
			return decoded, nil
		}
	}
}

// Malformed framing must not pass merely because it contains the expected
// object bytes before the damaged terminal chunk or trailing data.
func TestDecodeAWSSignedChunkedBodyRejectsMalformedFraming(t *testing.T) {
	payload := `{"evidence":true}`
	signature := strings.Repeat("a", sha256.Size*2)
	chunk := func(size string, data string) string {
		return size + ";chunk-signature=" + signature + "\r\n" + data + "\r\n"
	}
	terminal := chunk("0", "")
	tests := []struct {
		name    string
		encoded string
		want    string
		wantErr bool
	}{
		{name: "multiple complete chunks", encoded: chunk("c", `{"evidence":`) + chunk("5", `true}`) + terminal, want: payload},
		{name: "missing terminal chunk", encoded: chunk("11", payload), wantErr: true},
		{name: "truncated terminal header", encoded: chunk("11", payload) + "0;chunk-signature=" + signature[:len(signature)-1], wantErr: true},
		{name: "truncated data", encoded: "11;chunk-signature=" + signature + "\r\n" + payload[:len(payload)-1], wantErr: true},
		{name: "missing data delimiter", encoded: "11;chunk-signature=" + signature + "\r\n" + payload + "\n" + terminal, wantErr: true},
		{name: "invalid signature", encoded: "11;chunk-signature=" + signature[:len(signature)-1] + "g\r\n" + payload + "\r\n" + terminal, wantErr: true},
		{name: "invalid size", encoded: "11x;chunk-signature=" + signature + "\r\n" + payload + "\r\n" + terminal, wantErr: true},
		{name: "signed size", encoded: "+11;chunk-signature=" + signature + "\r\n" + payload + "\r\n" + terminal, wantErr: true},
		{name: "unsigned chunks", encoded: "11\r\n" + payload + "\r\n0\r\n\r\n", wantErr: true},
		{name: "trailing data", encoded: chunk("11", payload) + terminal + "ignored", wantErr: true},
	}
	for _, test := range tests {
		decoded, err := decodeAWSSignedChunkedBody([]byte(test.encoded))
		if test.wantErr {
			if err == nil {
				t.Errorf("%s framing passed with body %q", test.name, decoded)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s framing failed: %v", test.name, err)
			continue
		}
		if string(decoded) != test.want {
			t.Errorf("%s body = %q, want %q", test.name, decoded, test.want)
		}
	}
}

func TestMinIOBlobStorePutIfAbsentUsesConditionalRequest(t *testing.T) {
	type requestRecord struct {
		method               string
		path                 string
		ifNoneMatch          string
		contentType          string
		contentSHA256        string
		decodedContentLength string
		body                 []byte
		bodyErr              error
	}
	responseStatuses := []int{
		http.StatusOK,
		http.StatusPreconditionFailed,
		http.StatusForbidden,
		http.StatusConflict,
	}
	responseCodes := []string{"", minio.PreconditionFailed, minio.AccessDenied, "ConditionalRequestConflict"}
	var requestStateLock sync.Mutex
	var requests []requestRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, bodyErr := io.ReadAll(r.Body)
		requestStateLock.Lock()
		requests = append(requests, requestRecord{
			method:               r.Method,
			path:                 r.URL.Path,
			ifNoneMatch:          r.Header.Get("If-None-Match"),
			contentType:          r.Header.Get("Content-Type"),
			contentSHA256:        r.Header.Get("X-Amz-Content-Sha256"),
			decodedContentLength: r.Header.Get("X-Amz-Decoded-Content-Length"),
			body:                 body,
			bodyErr:              bodyErr,
		})
		responseIndex := len(requests) - 1
		requestStateLock.Unlock()
		if len(responseStatuses) <= responseIndex {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		status := responseStatuses[responseIndex]
		if status == http.StatusOK {
			w.Header().Set("ETag", `"created"`)
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>rejected</Message></Error>", responseCodes[responseIndex])
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds:        credentials.NewStaticV4("access", "secret", ""),
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &minioBlobStore{client: client, bucket: "evidence", prefix: "blob", authority: endpoint.Host}
	source := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(source, []byte(`{"evidence":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := store.PutIfAbsent(context.Background(), "blob/evidence.json", source, "application/json")
	if err != nil || !created {
		t.Fatalf("successful conditional create = %t, %v", created, err)
	}
	created, err = store.PutIfAbsent(context.Background(), "blob/evidence.json", source, "application/json")
	if err != nil || created {
		t.Fatalf("precondition conflict = %t, %v", created, err)
	}
	if created, err = store.PutIfAbsent(context.Background(), "blob/evidence.json", source, "application/json"); err == nil || created {
		t.Fatalf("access denial = %t, %v", created, err)
	}
	if created, err = store.PutIfAbsent(context.Background(), "blob/evidence.json", source, "application/json"); err == nil || created {
		t.Fatalf("non-precondition conflict = %t, %v", created, err)
	}
	requestStateLock.Lock()
	defer requestStateLock.Unlock()
	if len(requests) != len(responseStatuses) {
		t.Fatalf("request count = %d, want %d", len(requests), len(responseStatuses))
	}
	expectedBody := []byte(`{"evidence":true}`)
	for i, request := range requests {
		if request.method != http.MethodPut || request.path != "/evidence/blob/evidence.json" ||
			request.ifNoneMatch != "*" || request.contentType != "application/json" ||
			request.contentSHA256 != "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" ||
			request.decodedContentLength != strconv.Itoa(len(expectedBody)) {
			t.Errorf("request %d = %+v", i, request)
		}
		if request.bodyErr != nil {
			t.Errorf("request %d body read: %v", i, request.bodyErr)
			continue
		}
		decodedBody, err := decodeAWSSignedChunkedBody(request.body)
		if err != nil {
			t.Errorf("request %d signed body: %v", i, err)
			continue
		}
		if !bytes.Equal(decodedBody, expectedBody) {
			t.Errorf("request %d decoded body = %q, want %q", i, decodedBody, expectedBody)
		}
	}
}

func TestMinIOPreconditionConflictClassificationIsExact(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{err: minio.ErrorResponse{Code: minio.PreconditionFailed, StatusCode: http.StatusPreconditionFailed}, want: true},
		{err: minio.ErrorResponse{Code: minio.PreconditionFailed, StatusCode: http.StatusConflict}, want: false},
		{err: minio.ErrorResponse{Code: minio.AccessDenied, StatusCode: http.StatusPreconditionFailed}, want: false},
		{err: errors.New("transport failed"), want: false},
	}
	for _, test := range tests {
		if got := isMinioPreconditionConflict(test.err); got != test.want {
			t.Errorf("isMinioPreconditionConflict(%v) = %t, want %t", test.err, got, test.want)
		}
	}
}

func TestLocalBlobStoreRetainedCapability(t *testing.T) {
	store := NewLocalBlobStore(t.TempDir(), "competition").(RetainedBlobStore)
	source := filepath.Join(t.TempDir(), "artifact.json")
	content := []byte("{\"schema\":1}\n")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	retainUntil := NowUtc().Add(24 * time.Hour).Truncate(time.Second)
	proof, err := store.PutRetained(
		context.Background(), "competition/round/artifact.json", source,
		"application/json", retainUntil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Mode != "LOCAL" || proof.Size != int64(len(content)) ||
		proof.Key != "competition/round/artifact.json" || proof.RetainUntil != retainUntil {
		t.Fatalf("retention proof = %+v", proof)
	}
	reader, err := store.GetVersion(context.Background(), proof.Key, proof.VersionId)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, content) {
		t.Fatalf("retained read = %q, %v, %v", got, readErr, closeErr)
	}
	if err := store.CheckRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRetained(
		context.Background(), "competition/round/expired", source,
		"application/json", NowUtc().Add(-time.Second),
	); err == nil {
		t.Fatal("expired retention deadline accepted")
	}
}

func TestLocalBlobStoreReaper(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStore(root, "stats").(*localBlobStore)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := filepath.Join(t.TempDir(), "seg.pb.zst")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	expiredKey := "stats/local/findproviders2/2026-07-10/inst/1-0.pb.zst" // aged, has a rule -> reaped
	freshKey := "stats/local/findproviders2/2026-07-19/inst/2-0.pb.zst"   // recent, has a rule -> kept
	noRuleKey := "stats/local/otherstream/2026-07-10/inst/1-0.pb.zst"     // aged, no rule -> kept
	for _, k := range []string{expiredKey, freshKey, noRuleKey} {
		if err := store.Put(ctx, k, src, "application/zstd"); err != nil {
			t.Fatal(err)
		}
	}
	// age the two "old" objects to 48h ago (by mtime, which the reaper uses)
	old := time.Now().Add(-48 * time.Hour)
	for _, k := range []string{expiredKey, noRuleKey} {
		if err := os.Chtimes(store.pathFor(k), old, old); err != nil {
			t.Fatal(err)
		}
	}

	// findproviders2 keeps 24h; otherstream has no rule
	if err := store.SetLifecycle(ctx, []BlobLifecycleRule{
		{KeyPrefix: "stats/local/findproviders2/", TTL: 24 * time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	store.reapPass()

	if _, err := os.Stat(store.pathFor(expiredKey)); !os.IsNotExist(err) {
		t.Fatalf("expired findproviders2 object should be deleted (err=%v)", err)
	}
	if _, err := os.Stat(store.pathFor(freshKey)); err != nil {
		t.Fatalf("fresh findproviders2 object should be kept: %s", err)
	}
	if _, err := os.Stat(store.pathFor(noRuleKey)); err != nil {
		t.Fatalf("object of a stream with no rule should be kept: %s", err)
	}
}

func TestLocalBlobStoreCapacity(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "stats", 5)
	ctx := context.Background()

	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(first, []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("56"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Put(ctx, "stats/a", first, "application/octet-stream"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := store.Put(ctx, "stats/b", second, "application/octet-stream"); err == nil {
		t.Fatal("put exceeding aggregate cap succeeded")
	}
	if err := store.Put(ctx, "stats/a", replacement, "application/octet-stream"); err != nil {
		t.Fatalf("smaller replacement: %v", err)
	}
	if err := store.Put(ctx, "stats/b", second, "application/octet-stream"); err != nil {
		t.Fatalf("put after freeing capacity: %v", err)
	}
}

func TestMeasureBlobUsageAuthenticatesAllocatedPrefix(t *testing.T) {
	store := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "competition", 20)
	sourceDirectory := t.TempDir()
	firstPath := filepath.Join(sourceDirectory, "first")
	secondPath := filepath.Join(sourceDirectory, "second")
	if err := os.WriteFile(firstPath, []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("56"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "competition/one", firstPath, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "competition/two", secondPath, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "other/not-counted", secondPath, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	usage, err := MeasureBlobUsage(ctx, store, 10)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ObjectCount != 2 || usage.UsedBytes != 6 || usage.FreeBytes != 4 || usage.UsedPercent != 60 {
		t.Fatalf("blob usage = %+v", usage)
	}
	if _, err := MeasureBlobUsage(ctx, store, 0); err == nil {
		t.Fatal("zero capacity allocation passed")
	}
}

func TestEnabledReplicationTargetsRequireARealEnabledDestination(t *testing.T) {
	config := replication.Config{Rules: []replication.Rule{
		{Status: replication.Disabled, Destination: replication.Destination{Bucket: "arn:disabled"}},
		{Status: replication.Enabled, Destination: replication.Destination{Bucket: "arn:replica-b"}},
		{Status: replication.Enabled, Destination: replication.Destination{Bucket: "arn:replica-a"}},
		{Status: replication.Enabled, Destination: replication.Destination{Bucket: "arn:replica-b"}},
	}}
	targets, err := enabledReplicationTargets(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0] != "arn:replica-a" || targets[1] != "arn:replica-b" {
		t.Fatalf("replication targets = %v", targets)
	}
	if _, err := enabledReplicationTargets(replication.Config{}); err == nil {
		t.Fatal("replication config without an enabled target passed")
	}
	if _, err := enabledReplicationTargets(replication.Config{Rules: []replication.Rule{
		{Status: replication.Enabled},
	}}); err == nil {
		t.Fatal("enabled replication rule without a destination passed")
	}
}

func TestLoadBlobStoreConfigBackendSelection(t *testing.T) {
	// explicit local backend
	cleanup := Vault.PushSimpleResource("minio.yml", []byte("authority: local\npath: /tmp/sim-blob\nprefix: stats\n"))
	config, present := LoadBlobStoreConfig()
	if !present {
		cleanup()
		t.Fatal("expected present")
	}
	if !config.Local || config.LocalPath != "/tmp/sim-blob" {
		cleanup()
		t.Fatalf("authority 'local' should select local backend at /tmp/sim-blob, got %+v", config)
	}
	if config.LocalMaxBytes != DefaultLocalBlobMaxBytes {
		t.Fatalf("local max bytes = %d, want %d", config.LocalMaxBytes, DefaultLocalBlobMaxBytes)
	}
	store, ok := LoadBlobStore()
	if !ok || store.Authority() != "local:/tmp/sim-blob" {
		cleanup()
		t.Fatalf("LoadBlobStore local: ok=%t authority=%q", ok, store.Authority())
	}
	cleanup()

	// a real authority selects MinIO (not local)
	cleanup = Vault.PushSimpleResource("minio.yml", []byte("authority: minio.example.com:9000\nbucket: stats\naccess_key: k\nsecret_key: s\n"))
	defer cleanup()
	config, present = LoadBlobStoreConfig()
	if !present {
		t.Fatal("expected present")
	}
	if config.Local {
		t.Fatal("a real authority should not select the local backend")
	}
	if config.Bucket != "stats" {
		t.Fatalf("bucket = %q, want stats", config.Bucket)
	}
}

// SetLifecycle must replace only code-owned rules (blobLifecycleRuleIdPrefix)
// and preserve every foreign rule on the bucket — ops-set ILM and other envs'
// rules survive a retention apply.
func TestMergeOwnedLifecycleRules(t *testing.T) {
	opsRule := lifecycle.Rule{
		ID:         "ops-backup-expiry",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "backups/"},
		Expiration: lifecycle.Expiration{Days: 30},
	}
	otherEnvRule := lifecycle.Rule{
		ID:         blobLifecycleRuleIdPrefix + "stats-other-findproviders2",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "stats/other/findproviders2/"},
		Expiration: lifecycle.Expiration{Days: 7},
	}
	staleOwnedRule := lifecycle.Rule{
		ID:         blobLifecycleRuleIdPrefix + "stats-main-findproviders2",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "stats/main/findproviders2/"},
		Expiration: lifecycle.Expiration{Days: 14},
	}
	existing := lifecycle.NewConfiguration()
	existing.Rules = []lifecycle.Rule{opsRule, otherEnvRule, staleOwnedRule}

	owned := []lifecycle.Rule{
		{
			ID:         blobLifecycleRuleIdPrefix + "stats-main-findproviders2",
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: "stats/main/findproviders2/"},
			Expiration: lifecycle.Expiration{Days: 7},
		},
	}

	merged := mergeOwnedLifecycleRules(existing, owned)

	// replacement is by exact ID: the ops rule and another env's code-owned
	// rule (different deterministic ID) both survive; only the rule being
	// rewritten is replaced
	ids := map[string]lifecycle.Rule{}
	for _, rule := range merged {
		ids[rule.ID] = rule
	}
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged rules, got %d", len(merged))
	}
	if _, ok := ids["ops-backup-expiry"]; !ok {
		t.Fatal("foreign (ops) rule must be preserved")
	}
	if _, ok := ids[blobLifecycleRuleIdPrefix+"stats-other-findproviders2"]; !ok {
		t.Fatal("another env's code-owned rule must be preserved")
	}
	updated, ok := ids[blobLifecycleRuleIdPrefix+"stats-main-findproviders2"]
	if !ok {
		t.Fatal("owned rule must be present")
	}
	if updated.Expiration.Days != 7 {
		t.Fatalf("owned rule must be replaced with the new TTL, got %d days", updated.Expiration.Days)
	}

	// a nil existing config merges to exactly the owned set
	merged = mergeOwnedLifecycleRules(nil, owned)
	if len(merged) != 1 || merged[0].ID != owned[0].ID {
		t.Fatal("nil existing config must merge to the owned rules")
	}
}

// resolveBlobAuthority: env interpolation per the vault convention plus the
// config settings routes mapping (hostname -> route ip), fail-safe on a
// missing env var.
func TestResolveBlobAuthority(t *testing.T) {
	routes := map[string]string{
		"test-minio-host": "192.168.1.3",
	}

	// plain passthrough (raw ip, not in routes)
	resolved, ok := resolveBlobAuthority("10.0.0.9:23900", routes)
	if !ok || resolved != "10.0.0.9:23900" {
		t.Fatalf("passthrough: %q %v", resolved, ok)
	}

	// hostname in routes maps to the route ip, port kept
	resolved, ok = resolveBlobAuthority("test-minio-host:23900", routes)
	if !ok || resolved != "192.168.1.3:23900" {
		t.Fatalf("routes: %q %v", resolved, ok)
	}

	// env interpolation then routes mapping
	t.Setenv("TEST_BLOB_MINIO_HOSTNAME", "test-minio-host")
	resolved, ok = resolveBlobAuthority("{{ env:TEST_BLOB_MINIO_HOSTNAME }}:23900", routes)
	if !ok || resolved != "192.168.1.3:23900" {
		t.Fatalf("env+routes: %q %v", resolved, ok)
	}

	// a missing env var disables the store instead of panicking
	_, ok = resolveBlobAuthority("{{ env:TEST_BLOB_MINIO_HOSTNAME_UNSET }}:23900", routes)
	if ok {
		t.Fatal("missing env var must resolve to not-ok")
	}

	// empty and "local" pass through for backend selection
	resolved, ok = resolveBlobAuthority("", routes)
	if !ok || resolved != "" {
		t.Fatalf("empty: %q %v", resolved, ok)
	}
}
