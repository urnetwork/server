// Real JWT state, Redis quota, local immutable storage and validator HTTP
// clients exercise the upload path without shared operator credentials.
package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urfoundation/sn/validator"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

// Signed tokens have genuine active network/device/client rows behind them.
func snAttemptUploadTestIdentity(t testing.TB) (*jwt.ByJwt, *jwt.ByJwt) {
	t.Helper()
	networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(t.Context(), networkId, "attempt-upload-"+networkId.String(), userId)
	model.Testing_CreateDevice(t.Context(), networkId, deviceId, clientId, "attempt-upload", "test")
	account := jwt.NewByJwt(networkId, userId, "attempt-upload", false, false)
	return account, account.Client(deviceId, clientId)
}

// Request framing is real and owns the supplied raw byte sequence.
func snAttemptUploadTestRequest(t testing.TB, token string, kind string, data []byte) *http.Request {
	t.Helper()
	query := url.Values{"kind": {kind}, "hash": {snAttemptTestHash(data)}}
	request := httptest.NewRequest(http.MethodPost, "/sn/attempt-artifact?"+query.Encode(), bytes.NewReader(data))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	contentType := "application/x-ndjson"
	if kind == "metadata" {
		contentType = "application/json"
	}
	request.Header.Set("Content-Type", contentType)
	return request
}

// All three uploaded kinds survive the actual public validator reader.
func TestSnAttemptUploadRealClientQuotaStorageAndPublicReadback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, client := snAttemptUploadTestIdentity(tb)
		store := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")
		budget := model.StAttemptUploadBudget{RequestsPerHour: 20, BytesPerHour: 2048, AccountRequestsPerHour: 20, AccountBytesPerHour: 2048}
		deployment := model.StDeploymentKey(server.NewId().String())
		var reservations atomic.Int32
		reserve := func(ctx context.Context, userId server.Id, size uint64) error {
			if userId != account.UserId {
				return errors.New("quota owner did not come from the authenticated account")
			}
			reservations.Add(1)
			return model.ReserveStAttemptUpload(ctx, deployment, userId, size, budget)
		}
		writeSlots, readSlots := make(chan struct{}, 2), make(chan struct{}, 2)
		load := func() (server.BlobStore, bool) { return store, true }
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				serveSnUploadAttemptArtifact(w, r, load, reserve, snAttemptTestBounds(), writeSlots)
			} else {
				serveSnAttemptArtifact(w, r, load, snAttemptTestBounds(), readSlots)
			}
		}))
		defer endpoint.Close()
		stream := validator.AttemptStreamV2Bounds{MaxDataBytes: 1024, MaxItems: 32, MaxChunkBytes: 256, MaxChunks: 8, MaxPages: 4, MaxPageBytes: 128, MaxDescriptorsPerPage: 2, MaxManifestBytes: 128}
		writer, err := validator.NewHTTPAttemptStreamV2Writer(endpoint.URL, validator.AttemptCutV2Bounds{MaxHeaderBytes: 128, Records: stream, Proofs: stream}, func() string { return client.Sign() })
		if err != nil {
			tb.Fatal(err)
		}
		reader := snAttemptTestValidatorReader(t, endpoint.URL)
		data := []byte("{\"sequence\":1}\n")
		hash := snAttemptTestHash(data)
		for _, kind := range []string{"metadata", "records", "proofs"} {
			if err := writer.Write(tb.Context(), kind, hash, data); err != nil {
				tb.Fatal(err)
			}
			var got []byte
			if kind == "metadata" {
				got, err = reader.ReadMetadata(tb.Context(), hash, uint64(len(data)))
			} else {
				body, openErr := reader.OpenData(tb.Context(), kind, hash, uint64(len(data)))
				if openErr != nil {
					tb.Fatal(openErr)
				}
				got, err = io.ReadAll(body)
				err = errors.Join(err, body.Close())
			}
			if err != nil || !bytes.Equal(got, data) {
				tb.Fatalf("real %s public readback differs: %v", kind, err)
			}
		}
		if reservations.Load() != 3 || len(writeSlots) != 0 || len(readSlots) != 0 {
			tb.Fatal("real upload/replay ownership census differs")
		}
	})
}

// Missing, network-only and revoked credentials cannot cause body/quota/store I/O.
func TestSnAttemptUploadRequiresActiveClientBeforeBodyAndStorage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, client := snAttemptUploadTestIdentity(tb)
		other, otherClient := snAttemptUploadTestIdentity(tb)
		for _, fault := range []string{"absent", "network-only", "invalid", "foreign-network", "foreign-user", "duplicate-authorization", "removed-client"} {
			token := ""
			switch fault {
			case "network-only":
				token = account.Sign()
			case "invalid":
				token = "not-a-signed-client-token"
			case "foreign-network":
				changed := *client
				changed.NetworkId = other.NetworkId
				token = changed.Sign()
			case "foreign-user":
				changed := *client
				changed.UserId = other.UserId
				token = changed.Sign()
			case "duplicate-authorization":
				token = client.Sign()
			case "removed-client":
				token = client.Sign()
				server.Tx(tb.Context(), func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(tb.Context(), "UPDATE network_client SET active = false WHERE client_id = $1", *client.ClientId))
				})
			}
			reads, reservations, stores := 0, 0, 0
			request := snAttemptUploadTestRequest(tb, token, "metadata", []byte("data"))
			if fault == "duplicate-authorization" {
				request.Header.Add("Authorization", "Bearer "+otherClient.Sign())
			}
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
			response := snAttemptUploadTestRecorder()
			serveSnUploadAttemptArtifact(response, request, func() (server.BlobStore, bool) { stores++; return nil, false }, func(context.Context, server.Id, uint64) error { reservations++; return nil }, snAttemptTestBounds(), make(chan struct{}, 1))
			if response.Code != http.StatusUnauthorized || reads != 0 || reservations != 0 || stores != 0 {
				tb.Fatalf("%s unauthorized upload crossed admission: status%d reads%d quota%d stores%d", fault, response.Code, reads, reservations, stores)
			}
		}
	})
}

// Active client rotation cannot reset an account's budget, and independent
// authenticated accounts cannot spend beyond the deployment's exact ceiling.
// This is resource admission, not a claim of validator or Sybil eligibility.
func TestSnAttemptUploadAccountRotationCannotExpandDeploymentBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, first := snAttemptUploadTestIdentity(tb)
		_, second := snAttemptUploadTestIdentity(tb)
		_, third := snAttemptUploadTestIdentity(tb)
		deviceId, clientId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(tb.Context(), account.NetworkId, deviceId, clientId, "rotated-upload-client", "test")
		rotated := account.Client(deviceId, clientId)
		deployment := model.StDeploymentKey(server.NewId().String())
		budget := model.StAttemptUploadBudget{RequestsPerHour: 2, BytesPerHour: 8, AccountRequestsPerHour: 1, AccountBytesPerHour: 4}
		store := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")
		for _, item := range []struct {
			name     string
			identity *jwt.ByJwt
			status   int
		}{
			{name: "first-account", identity: first, status: http.StatusNoContent},
			{name: "same-account-new-client", identity: rotated, status: http.StatusTooManyRequests},
			{name: "second-account", identity: second, status: http.StatusNoContent},
			{name: "deployment-exhausted", identity: third, status: http.StatusTooManyRequests},
		} {
			reads, stores := 0, 0
			data := []byte("data")
			reader := bytes.NewReader(data)
			request := snAttemptUploadTestRequest(tb, item.identity.Sign(), "metadata", data)
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { reads++; return reader.Read(value) })}
			response := snAttemptUploadTestRecorder()
			slots := make(chan struct{}, 1)
			serveSnUploadAttemptArtifact(response, request,
				func() (server.BlobStore, bool) { stores++; return store, true },
				func(ctx context.Context, userId server.Id, size uint64) error {
					return model.ReserveStAttemptUpload(ctx, deployment, userId, size, budget)
				},
				snAttemptTestBounds(), slots)
			if response.Code != item.status || len(slots) != 0 {
				tb.Fatalf("%s quota boundary status%d want%d", item.name, response.Code, item.status)
			}
			if item.status == http.StatusNoContent {
				if reads == 0 || stores != 1 {
					tb.Fatalf("%s did not exercise actual admitted storage", item.name)
				}
			} else if reads != 0 || stores != 0 || response.Header().Get("Retry-After") == "" {
				tb.Fatalf("%s refusal consumed body/storage or lost its actual quota hint", item.name)
			}
		}
	})
}

// This is a known live-release availability blocker, not a quota success
// claim: arbitrary active accounts can exhaust staging for an unchanged,
// previously working validator-side session. Existing objects remain public.
func TestSnAttemptUploadAuthenticatedTrafficCanExhaustFreshValidatorStaging(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, validatorClient := snAttemptUploadTestIdentity(tb)
		_, firstAccount := snAttemptUploadTestIdentity(tb)
		_, secondAccount := snAttemptUploadTestIdentity(tb)
		deployment := model.StDeploymentKey(server.NewId().String())
		budget := model.StAttemptUploadBudget{RequestsPerHour: 3, BytesPerHour: 4096, AccountRequestsPerHour: 3, AccountBytesPerHour: 2048}
		store := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")
		writeSlots, readSlots := make(chan struct{}, 1), make(chan struct{}, 1)
		var reads, stores atomic.Int32
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				original := r.Body
				r.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { reads.Add(1); return original.Read(value) }), close: original.Close}
				serveSnUploadAttemptArtifact(w, r,
					func() (server.BlobStore, bool) { stores.Add(1); return store, true },
					func(ctx context.Context, userId server.Id, size uint64) error {
						return model.ReserveStAttemptUpload(ctx, deployment, userId, size, budget)
					},
					snAttemptTestBounds(), writeSlots)
			} else {
				serveSnAttemptArtifact(w, r, func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), readSlots)
			}
		}))
		defer endpoint.Close()
		stream := validator.AttemptStreamV2Bounds{MaxDataBytes: 1024, MaxItems: 32, MaxChunkBytes: 256, MaxChunks: 8, MaxPages: 4, MaxPageBytes: 128, MaxDescriptorsPerPage: 2, MaxManifestBytes: 128}
		bounds := validator.AttemptCutV2Bounds{MaxHeaderBytes: 128, Records: stream, Proofs: stream}
		writer, err := validator.NewHTTPAttemptStreamV2Writer(endpoint.URL, bounds, validatorClient.Sign)
		if err != nil {
			tb.Fatal(err)
		}
		prior := []byte("{\"validator-staging\":1}\n")
		if err := writer.Write(tb.Context(), "metadata", snAttemptTestHash(prior), prior); err != nil {
			tb.Fatalf("working validator session prerequisite: %v", err)
		}
		for index, identity := range []*jwt.ByJwt{firstAccount, secondAccount} {
			adversary, err := validator.NewHTTPAttemptStreamV2Writer(endpoint.URL, bounds, identity.Sign)
			if err != nil {
				tb.Fatal(err)
			}
			data := []byte(fmt.Sprintf("{\"unrelated-staging\":%d}\n", index))
			if err := adversary.Write(tb.Context(), "metadata", snAttemptTestHash(data), data); err != nil {
				tb.Fatalf("actual adversarial account admission%d: %v", index, err)
			}
		}
		beforeReads, beforeStores := reads.Load(), stores.Load()
		if beforeReads == 0 || beforeStores != 3 {
			tb.Fatal("adversarial traffic did not reach actual immutable storage")
		}
		next := []byte("{\"validator-staging\":2}\n")
		if err := writer.Write(tb.Context(), "metadata", snAttemptTestHash(next), next); err == nil || !strings.Contains(err.Error(), "status is 429") {
			tb.Fatalf("account-only admission no longer demonstrates the tracked validator staging denial: %v", err)
		}
		if reads.Load() != beforeReads || stores.Load() != beforeStores || len(writeSlots) != 0 {
			tb.Fatal("exhausted deployment admitted new body/storage work")
		}
		reader := snAttemptTestValidatorReader(t, endpoint.URL)
		if raw, err := reader.ReadMetadata(tb.Context(), snAttemptTestHash(prior), uint64(len(prior))); err != nil || !bytes.Equal(raw, prior) {
			tb.Fatalf("adversarial staging removed prior public evidence: %v", err)
		}
		if raw, err := reader.ReadMetadata(tb.Context(), snAttemptTestHash(next), uint64(len(next))); err == nil || raw != nil {
			tb.Fatal("refused validator bytes became publicly available")
		}
		if len(readSlots) != 0 {
			tb.Fatal("public readback retained its admission slot")
		}
	})
}

// Cheap canonical/type/size checks happen before any storage allocation or quota.
func TestSnAttemptUploadRejectsMalformedFramingBeforeSideEffects(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		token := client.Sign()
		for _, fault := range []string{"query", "duplicate-hash", "path", "kind", "type", "type-parameter", "encoding", "range", "partial", "chunked", "trailer", "missing-size", "too-large", "method"} {
			request := snAttemptUploadTestRequest(tb, token, "metadata", []byte("data"))
			want := http.StatusBadRequest
			switch fault {
			case "query":
				request.URL.RawQuery = ""
			case "duplicate-hash":
				request.URL.RawQuery += "&hash=" + snAttemptTestHash([]byte("data"))
			case "path":
				request.URL.RawQuery += "&path=../other"
			case "kind":
				request.URL.RawQuery = "kind=other&hash=" + snAttemptTestHash([]byte("data"))
			case "type":
				request.Header.Set("Content-Type", "text/plain")
			case "type-parameter":
				request.Header.Set("Content-Type", "application/json; charset=utf-8")
			case "encoding":
				request.Header.Set("Content-Encoding", "gzip")
			case "range":
				request.Header.Set("Range", "bytes=0-1")
			case "partial":
				request.Header.Set("Content-Range", "bytes 0-1/4")
			case "chunked":
				request.TransferEncoding = []string{"chunked"}
			case "trailer":
				request.Trailer = http.Header{"Hash": {"untrusted"}}
			case "missing-size":
				request.ContentLength = -1
				want = http.StatusRequestEntityTooLarge
			case "too-large":
				request.ContentLength = int64(snAttemptTestBounds().MetadataBytes) + 1
				want = http.StatusRequestEntityTooLarge
			case "method":
				request.Method = http.MethodPut
				want = http.StatusMethodNotAllowed
			}
			reads, reservations, stores := 0, 0, 0
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
			response := snAttemptUploadTestRecorder()
			serveSnUploadAttemptArtifact(response, request, func() (server.BlobStore, bool) { stores++; return nil, false }, func(context.Context, server.Id, uint64) error { reservations++; return nil }, snAttemptTestBounds(), make(chan struct{}, 1))
			if response.Code != want || reads != 0 || reservations != 0 || stores != 0 {
				tb.Fatalf("%s malformed upload crossed admission: status%d want%d reads%d quota%d stores%d", fault, response.Code, want, reads, reservations, stores)
			}
		}
	})
}

// Saturated slots and quota outages refuse without buffering the supplied body.
func TestSnAttemptUploadRefusesBusyOrUnavailableQuotaBeforeBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		for _, fault := range []string{"busy", "quota", "unavailable"} {
			request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", []byte("data"))
			reads, stores := 0, 0
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
			slots := make(chan struct{}, 1)
			if fault == "busy" {
				slots <- struct{}{}
			}
			want := http.StatusTooManyRequests
			if fault == "unavailable" {
				want = http.StatusServiceUnavailable
			}
			response := snAttemptUploadTestRecorder()
			serveSnUploadAttemptArtifact(response, request, func() (server.BlobStore, bool) { stores++; return nil, false }, func(context.Context, server.Id, uint64) error { return fmt.Errorf("%d quota refusal", want) }, snAttemptTestBounds(), slots)
			if response.Code != want || reads != 0 || stores != 0 || fault != "busy" && len(slots) != 0 {
				tb.Fatalf("%s refusal crossed body/store ownership: %d/%d/%d", fault, response.Code, reads, stores)
			}
			if fault == "busy" {
				if len(slots) != 1 || response.Header().Get("Retry-After") != "1" {
					tb.Fatal("busy refusal consumed another request's slot")
				}
				<-slots
			}
		}
	})
}

// The admitted request's body/size/type/hash are owned before quota callbacks.
func TestSnAttemptUploadOwnsAdmittedRequestAcrossQuotaBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		data := []byte("{\"original\":true}\n")
		request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", data)
		store := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")
		response := snAttemptUploadTestRecorder()
		serveSnUploadAttemptArtifact(response, request, func() (server.BlobStore, bool) { return store, true }, func(_ context.Context, _ server.Id, size uint64) error {
			if size != uint64(len(data)) {
				return errors.New("wrong quota size")
			}
			request.Body, request.ContentLength = io.NopCloser(bytes.NewReader([]byte("redirected"))), 10
			request.URL.RawQuery = "kind=proofs&hash=" + snAttemptTestHash([]byte("redirected"))
			return nil
		}, snAttemptTestBounds(), make(chan struct{}, 1))
		if response.Code != http.StatusNoContent || response.Header().Get("ETag") != `"`+snAttemptTestHash(data)+`"` {
			tb.Fatalf("quota callback redirected admitted body: %d/%s", response.Code, response.Body.String())
		}
		read := snAttemptUploadTestRecorder()
		serveSnAttemptArtifact(read, httptest.NewRequest(http.MethodGet, "/sn/attempt-artifact?kind=metadata&hash="+snAttemptTestHash(data), nil), func() (server.BlobStore, bool) { return store, true }, snAttemptTestBounds(), make(chan struct{}, 1))
		if read.Code != http.StatusOK || !bytes.Equal(read.Body.Bytes(), data) {
			tb.Fatal("admitted original bytes were not stored")
		}
	})
}

// Hash, body Close and actual immutable readback failures never acknowledge success.
func TestSnAttemptUploadRefusesBodyHashCloseAndStoredReadbackFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		for _, fault := range []string{"hash", "close", "readback"} {
			data := []byte("data")
			request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", data)
			want := http.StatusBadRequest
			if fault == "hash" {
				request.URL.RawQuery = "kind=metadata&hash=" + snAttemptTestHash([]byte("other"))
			}
			if fault == "close" {
				request.Body = &snAttemptTestReadCloser{Reader: bytes.NewReader(data), close: func() error { return errors.New("actual request body close failed") }}
			}
			base := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")
			var store server.BlobStore = base
			if fault == "readback" {
				want = http.StatusBadGateway
				store = &snAttemptTestStore{BlobStore: base, get: func(context.Context, string) (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader([]byte("other"))), nil
				}}
			}
			stores := 0
			slots := make(chan struct{}, 1)
			response := snAttemptUploadTestRecorder()
			serveSnUploadAttemptArtifact(response, request, func() (server.BlobStore, bool) { stores++; return store, true }, func(context.Context, server.Id, uint64) error { return nil }, snAttemptTestBounds(), slots)
			if response.Code != want || response.Header().Get("ETag") != "" || len(slots) != 0 || fault != "readback" && stores != 0 {
				tb.Fatalf("%s failure became an upload acknowledgement: %d stores%d", fault, response.Code, stores)
			}
		}
	})
}
