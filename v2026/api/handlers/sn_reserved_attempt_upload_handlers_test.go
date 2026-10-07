// Genuine JWT rows, Redis quotas, HTTP body ownership and independent local
// stores prove that ordinary or duplicate traffic cannot spend fresh minima.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
)

// Each destination has its own real SDK session, authenticated cache, object
// namespace and request counters. No secret is shared with the source signer.
type snReservedAttemptEndpoint struct {
	origin string
	api    *sdk.Api
	client *jwt.ByJwt
	writer *validator.ValidatorReservedAttemptStreamV2Writer
	reader *validator.HTTPAttemptStreamV2Reader
	reads  atomic.Uint64
	stores atomic.Uint64
}

// Private fixtures use the same public constructors and real route admission
// as an API process; only physical storage and finite capacities are local.
func newSnReservedAttemptEndpoint(t testing.TB, authority *snReservedAttemptAuthority, replicaNoID uint64) *snReservedAttemptEndpoint {
	t.Helper()
	owner := authority.start(t, replicaNoID)
	_, client := snAttemptUploadTestIdentity(t)
	self := &snReservedAttemptEndpoint{client: client}
	store := server.NewLocalBlobStore(t.TempDir(), fmt.Sprintf("replica-%d", replicaNoID))
	ordinary := model.StDeploymentKey(fmt.Sprintf("ordinary-reserved-fixture-%d", replicaNoID))
	budget := model.StAttemptUploadBudget{RequestsPerHour: 2, BytesPerHour: 256, AccountRequestsPerHour: 2, AccountBytesPerHour: 256}
	writeSlots, readSlots := make(chan struct{}, 1), make(chan struct{}, 1)
	load := func() (server.BlobStore, bool) { self.stores.Add(1); return store, true }
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			original := r.Body
			r.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func(data []byte) (int, error) { self.reads.Add(1); return original.Read(data) }), close: original.Close}
			serveSnAttemptUpload(w, r, load, func(ctx context.Context, userID server.Id, size uint64) error {
				return model.ReserveStAttemptUpload(ctx, ordinary, userID, size, budget)
			}, snAttemptTestBounds(), writeSlots, owner)
		} else {
			serveSnAttemptArtifact(w, r, load, snAttemptTestBounds(), readSlots)
		}
	}))
	t.Cleanup(endpoint.Close)
	self.origin = endpoint.URL
	strategy := connect.NewClientStrategyWithDefaults(t.Context())
	self.api = sdk.NewApi(t.Context(), strategy, endpoint.URL)
	self.api.SetByJwt(client.Sign())
	t.Cleanup(func() {
		if err := self.api.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		strategy.Close()
	})
	stream := validator.AttemptStreamV2Bounds{MaxDataBytes: 1024, MaxItems: 32, MaxChunkBytes: 256, MaxChunks: 8, MaxPages: 4, MaxPageBytes: 128, MaxDescriptorsPerPage: 2, MaxManifestBytes: 128}
	bounds := validator.AttemptCutV2Bounds{MaxHeaderBytes: 128, Records: stream, Proofs: stream}
	var err error
	self.writer, err = validator.NewValidatorReservedAttemptStreamV2Writer(endpoint.URL, bounds, self.api.GetByJwt, authority.record, replicaNoID, 25, authority.key)
	if err != nil {
		t.Fatal(err)
	}
	self.reader, err = validator.NewHTTPAttemptStreamV2Reader(endpoint.URL, bounds)
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// The response body is always consumed and joined, including quota refusal.
func snReservedAttemptRequest(t testing.TB, origin, token, header, kind string, data []byte) int {
	t.Helper()
	endpoint := origin + "/sn/attempt-artifact?" + url.Values{"kind": {kind}, "hash": {snAttemptTestHash(data)}}.Encode()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if header != "" {
		request.Header.Set(protocol.ValidatorAttemptUploadHeader, header)
	}
	contentType := "application/json"
	if kind != "metadata" {
		contentType = "application/x-ndjson"
	}
	request.Header.Set("Content-Type", contentType)
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode
}

// Ordinary authenticated accounts exhaust their real Redis pool, and repeated
// protected objects exhaust retry capacity. The same live validator then
// stages a distinct object and both independent public copies remain exact.
func TestSnReservedAttemptUploadFloodPreservesDistinctObjectAndDualReadback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		endpoints := []*snReservedAttemptEndpoint{newSnReservedAttemptEndpoint(tb, authority, 1), newSnReservedAttemptEndpoint(tb, authority, 2)}
		prior, next := []byte("{\"object\":1}\n"), []byte("{\"object\":2}\n")
		for _, endpoint := range endpoints {
			if err := endpoint.writer.Write(tb.Context(), "metadata", snAttemptTestHash(prior), prior); err != nil {
				tb.Fatalf("working protected upload prerequisite: %v", err)
			}
			for range 2 {
				_, attacker := snAttemptUploadTestIdentity(tb)
				if status := snReservedAttemptRequest(tb, endpoint.origin, attacker.Sign(), "", "metadata", []byte("{\"flood\":1}\n")); status != http.StatusNoContent {
					tb.Fatalf("ordinary flood prerequisite status%d", status)
				}
			}
			if status := snReservedAttemptRequest(tb, endpoint.origin, endpoint.client.Sign(), "", "metadata", next); status != http.StatusTooManyRequests {
				tb.Fatalf("ordinary pool was not actually exhausted: status%d", status)
			}
			for range 2 {
				refreshed := jwt.NewByJwt(endpoint.client.NetworkId, endpoint.client.UserId, "attempt-upload", false, false).Client(*endpoint.client.DeviceId, *endpoint.client.ClientId)
				endpoint.api.SetByJwt(refreshed.Sign())
				if err := endpoint.writer.Write(tb.Context(), "metadata", snAttemptTestHash(prior), prior); err != nil {
					tb.Fatalf("actual refreshed-session retry failed: %v", err)
				}
			}
			reads, stores := endpoint.reads.Load(), endpoint.stores.Load()
			if err := endpoint.writer.Write(tb.Context(), "metadata", snAttemptTestHash(prior), prior); err == nil || !strings.Contains(err.Error(), "status is 429") {
				tb.Fatalf("duplicate retry flood did not reach actual quota refusal: %v", err)
			}
			if endpoint.reads.Load() != reads || endpoint.stores.Load() != stores {
				tb.Fatal("retry refusal consumed body or storage")
			}
			if err := endpoint.writer.Write(tb.Context(), "metadata", snAttemptTestHash(next), next); err != nil {
				tb.Fatalf("ordinary or duplicate flood consumed protected fresh object: %v", err)
			}
		}
		for _, endpoint := range endpoints {
			for _, data := range [][]byte{prior, next} {
				actual, err := endpoint.reader.ReadMetadata(tb.Context(), snAttemptTestHash(data), uint64(len(data)))
				if err != nil || !bytes.Equal(actual, data) {
					tb.Fatalf("protected dual-origin public readback differs: %v", err)
				}
			}
		}
	})
}

// Independently valid JWTs still cannot transplant a signed intent across
// sessions, destinations, object bytes or current authorization state.
func TestSnReservedAttemptUploadBindsJWTReplicaAndObjectBeforeQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		first, second := newSnReservedAttemptEndpoint(tb, authority, 1), newSnReservedAttemptEndpoint(tb, authority, 2)
		data := []byte("{\"bound\":1}\n")
		token := first.api.GetByJwt()
		sessionHash, err := protocol.ValidatorAttemptUploadSessionHash(token)
		if err != nil {
			tb.Fatal(err)
		}
		intent := protocol.ValidatorAttemptUploadIntent{ActivationHash: authority.digest, VPK: authority.record.VPK, ReplicaNoID: 1, SessionHash: sessionHash, Kind: 1, ContentHash: sha256.Sum256(data), Size: uint64(len(data)), NotAfter: uint64(time.Now().Unix()) + 25}
		header, err := intent.Sign(authority.key)
		if err != nil {
			tb.Fatal(err)
		}
		for _, item := range []struct {
			name     string
			endpoint *snReservedAttemptEndpoint
			token    string
			data     []byte
			status   int
		}{
			{name: "missing client", endpoint: first, data: data, status: http.StatusUnauthorized},
			{name: "foreign session", endpoint: first, token: second.api.GetByJwt(), data: data, status: http.StatusForbidden},
			{name: "foreign destination", endpoint: second, token: token, data: data, status: http.StatusForbidden},
			{name: "different object", endpoint: first, token: token, data: []byte("{\"bound\":2}\n"), status: http.StatusForbidden},
		} {
			reads, stores := item.endpoint.reads.Load(), item.endpoint.stores.Load()
			if status := snReservedAttemptRequest(tb, item.endpoint.origin, item.token, header, "metadata", item.data); status != item.status {
				tb.Fatalf("%s crossed consent boundary: status%d", item.name, status)
			}
			if item.endpoint.reads.Load() != reads || item.endpoint.stores.Load() != stores {
				tb.Fatalf("%s reached body/store admission", item.name)
			}
		}
		if status := snReservedAttemptRequest(tb, first.origin, token, header, "metadata", data); status != http.StatusNoContent {
			tb.Fatalf("exact signed intent refused after substitutions: status%d", status)
		}
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), "UPDATE network_client SET active = false WHERE client_id = $1", *first.client.ClientId))
		})
		reads, stores := first.reads.Load(), first.stores.Load()
		if status := snReservedAttemptRequest(tb, first.origin, token, header, "metadata", data); status != http.StatusUnauthorized {
			tb.Fatalf("revoked client retained protected admission: status%d", status)
		}
		if first.reads.Load() != reads || first.stores.Load() != stores {
			tb.Fatal("revoked client reached body or storage")
		}
	})
}
