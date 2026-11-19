// zstd upload bodies through the real route: genuine JWT rows, Redis quotas,
// local immutable storage and the reserved lane's signed consent. The object's
// own length is what admission, quota and the consent see; storage keeps the
// decoded object; a validator over a lossy path sends a fraction of its bytes.
package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

// An encoded upload request, with its object's length beside it.
func snAttemptUploadZstdTestPost(t testing.TB, origin, token, header, kind string, object, wire []byte) (*http.Response, string) {
	t.Helper()
	endpoint := origin + "/sn/attempt-artifact?" + url.Values{"kind": {kind}, "hash": {snAttemptTestHash(object)}}.Encode()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set(snAttemptUploadDecodedLengthHeader, strconv.Itoa(len(object)))
	if header != "" {
		request.Header.Set(protocol.ValidatorAttemptUploadHeader, header)
	}
	contentType := "application/x-ndjson"
	if kind == "metadata" {
		contentType = "application/json"
	}
	request.Header.Set("Content-Type", contentType)
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

// The ordinary lane takes a zstd records object: the quota is charged its
// decoded length, storage keeps exactly the object its hash names, and the
// acknowledgement advertises zstd. A body that decodes to anything else is
// refused before storage.
func TestSnAttemptUploadTakesZstdAndStoresTheDecodedObject(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		store := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload-zstd")
		deployment := model.StDeploymentKey(server.NewId().String())
		budget := model.StAttemptUploadBudget{RequestsPerHour: 20, BytesPerHour: 4096, AccountRequestsPerHour: 20, AccountBytesPerHour: 4096}
		reserved := []uint64{}
		stores := 0
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveSnUploadAttemptArtifact(w, r, func() (server.BlobStore, bool) { stores++; return store, true }, func(ctx context.Context, userId server.Id, size uint64) error {
				reserved = append(reserved, size)
				return model.ReserveStAttemptUpload(ctx, deployment, userId, size, budget)
			}, snAttemptTestBounds(), make(chan struct{}, 1))
		}))
		defer endpoint.Close()
		object := []byte(strings.Repeat("{\"sequence\":1,\"disposition\":\"complete\"}\n", 6))
		wire := snAttemptUploadEncodingTestZstd(tb, object)
		if len(object) <= len(wire) {
			tb.Fatalf("the test object did not compress: %d of %d", len(wire), len(object))
		}
		response, body := snAttemptUploadZstdTestPost(tb, endpoint.URL, client.Testing_Sign(), "", "records", object, wire)
		if response.StatusCode != http.StatusNoContent || response.Header.Get("ETag") != `"`+snAttemptTestHash(object)+`"` || response.Header.Get("Accept-Encoding") != snAttemptUploadEncodingZstd {
			tb.Fatalf("zstd upload: status%d header=%v body=%q", response.StatusCode, response.Header, body)
		}
		if len(reserved) != 1 || reserved[0] != uint64(len(object)) {
			tb.Fatalf("quota charged %v, want the decoded %d", reserved, len(object))
		}
		var stored bytes.Buffer
		if _, err := startifact.ReadAttemptObjectTo(tb.Context(), store, snAttemptTestBounds(), "records", snAttemptTestHash(object), &stored); err != nil || !bytes.Equal(stored.Bytes(), object) {
			tb.Fatalf("storage kept other bytes: %v", err)
		}
		// a frame whose object is not the one its hash names
		other := []byte(strings.Repeat("{\"sequence\":2,\"disposition\":\"failed\"}\n", 6))
		storesBefore := stores
		response, _ = snAttemptUploadZstdTestPost(tb, endpoint.URL, client.Testing_Sign(), "", "records", other, wire)
		if response.StatusCode != http.StatusBadRequest || stores != storesBefore {
			tb.Fatalf("a body decoding to another object: status%d stores%d", response.StatusCode, stores-storesBefore)
		}
	})
}

// The reserved lane's consent signs the object's own length: a zstd body with
// that consent is staged and reads back exactly, and a consent that signs the
// wire length instead is refused before quota.
func TestSnReservedAttemptUploadConsentSignsTheDecodedLength(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		endpoint := newSnReservedAttemptEndpoint(tb, authority, 1)
		object := []byte("{\"object\":\"" + strings.Repeat("z", 96) + "\"}\n")
		wire := snAttemptUploadEncodingTestZstd(tb, object)
		if len(object) <= len(wire) {
			tb.Fatalf("the test object did not compress: %d of %d", len(wire), len(object))
		}
		token := endpoint.client.Testing_Sign()
		endpoint.api.SetByJwt(token)
		sessionHash, err := protocol.ValidatorAttemptUploadSessionHash(token)
		if err != nil {
			tb.Fatal(err)
		}
		intent := protocol.ValidatorAttemptUploadIntent{ActivationHash: authority.digest, VPK: authority.record.VPK, ReplicaNoID: 1, SessionHash: sessionHash, Kind: 1, ContentHash: sha256.Sum256(object), Size: uint64(len(wire)), NotAfter: uint64(time.Now().Unix()) + 25}
		wrong, err := intent.Sign(authority.key)
		if err != nil {
			tb.Fatal(err)
		}
		reads := endpoint.reads.Load()
		if response, _ := snAttemptUploadZstdTestPost(tb, endpoint.origin, token, wrong, "metadata", object, wire); response.StatusCode != http.StatusForbidden || endpoint.reads.Load() != reads {
			tb.Fatalf("a consent signing the wire length was taken: status%d", response.StatusCode)
		}
		header := snReservedAttemptIntentHeader(tb, authority, endpoint, object)
		if response, body := snAttemptUploadZstdTestPost(tb, endpoint.origin, token, header, "metadata", object, wire); response.StatusCode != http.StatusNoContent {
			tb.Fatalf("a reserved zstd upload: status%d body=%q", response.StatusCode, body)
		}
		actual, err := endpoint.reader.ReadMetadata(tb.Context(), snAttemptTestHash(object), uint64(len(object)))
		if err != nil || !bytes.Equal(actual, object) {
			tb.Fatalf("the reserved zstd object reads back as other bytes: %v", err)
		}
	})
}
