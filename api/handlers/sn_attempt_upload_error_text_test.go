// Every refusal on the upload route names only its declared answer. Causes
// that quote sockets, temp paths, store endpoints or Redis scripts stay in
// the server log; ordinary and reserved lanes share the same answers.
package handlers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A store whose write fails with a path error that names its location.
type snAttemptUploadFailingStore struct {
	server.BlobStore
}

func (self *snAttemptUploadFailingStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	return false, &os.PathError{Op: "open", Path: "/synthetic/private/blob-root/" + key, Err: syscall.EACCES}
}

// Serves one ordinary-lane upload with a free slot and the given dependencies.
func serveSnAttemptUploadForAnswer(writer http.ResponseWriter, request *http.Request, store server.BlobStore, reserve func(context.Context, server.Id, uint64) error) {
	serveSnUploadAttemptArtifact(writer, request, func() (server.BlobStore, bool) { return store, store != nil }, reserve, snAttemptTestBounds(), make(chan struct{}, 1))
}

// A quota failure that declares no status, here Redis unreachable, was a 500
// carrying the Redis address and the handler's Go name.
func TestSnAttemptUploadHidesUndeclaredFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		response := snAttemptUploadTestRecorder()
		serveSnAttemptUploadForAnswer(response, snAttemptUploadTestRequest(tb, client.Testing_Sign(), "metadata", []byte("data")), nil, func(context.Context, server.Id, uint64) error {
			return errors.New("attempt upload quota Redis failure: dial tcp 192.0.2.9:6379: connection refused")
		})
		requirePrivateUnavailable(tb, response.Code, response.Body.String(), "192.0.2.9", "Redis", "serveSnAttemptUpload")
	})
}

// The store's own write error names its private location.
func TestSnAttemptUploadHidesStoreFailureCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		store := &snAttemptUploadFailingStore{BlobStore: server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")}
		response := snAttemptUploadTestRecorder()
		serveSnAttemptUploadForAnswer(response, snAttemptUploadTestRequest(tb, client.Testing_Sign(), "metadata", []byte("data")), store, func(context.Context, server.Id, uint64) error { return nil })
		requireDeclaredAnswer(tb, response.Code, response.Body.String(), http.StatusBadGateway, "Immutable upload or readback failed.", "/synthetic/private", "permission denied")
	})
}

// A body shorter than its Content-Length was answered with the reader's
// account of the read.
func TestSnAttemptUploadHidesIncompleteBodyCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		request := snAttemptUploadTestRequest(tb, client.Testing_Sign(), "metadata", []byte("data"))
		request.ContentLength++
		response := snAttemptUploadTestRecorder()
		serveSnAttemptUploadForAnswer(response, request, nil, func(context.Context, server.Id, uint64) error { return nil })
		requireDeclaredAnswer(tb, response.Code, response.Body.String(), http.StatusBadRequest, "Incomplete upload.", "Content-Length", "EOF")
	})
}

// A response writer without a read deadline cannot own the body. The writer's
// capability error was appended to the refusal.
func TestSnAttemptUploadHidesReadOwnerFailureCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		response := httptest.NewRecorder()
		serveSnAttemptUploadForAnswer(response, snAttemptUploadTestRequest(tb, client.Testing_Sign(), "metadata", []byte("data")), nil, func(context.Context, server.Id, uint64) error { return nil })
		requireDeclaredAnswer(tb, response.Code, response.Body.String(), http.StatusServiceUnavailable, "Upload read cancellation unavailable.", "not supported")
	})
}

// A quota counter Redis cannot read made the quota script fail. Its reply was
// appended to the declared 503.
func TestSnAttemptUploadHidesQuotaFailureCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		cfg := &controller.StConfig{Enabled: true, Profile: "testnet", ChainId: 945, GenesisHash: [32]byte{1}, ContractAddress: common.Address{1}, DeploymentId: "attempt-quota-synthetic", PolicyHash: [32]byte{2}, Netuid: 521, NoId: 1,
			AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 20, BytesPerHour: 2048, AccountRequestsPerHour: 20, AccountBytesPerHour: 2048}}
		controller.SetStConfig(cfg)
		tb.Cleanup(func() { controller.SetStConfig(nil) })
		// The global counter's key, as the quota model names it.
		identity := sha256.Sum256([]byte(cfg.DeploymentKey()))
		server.Redis(tb.Context(), func(r server.RedisClient) {
			server.Raise(r.HSet(tb.Context(), fmt.Sprintf("{st_attempt_upload:%x}:global", identity), "bucket", "-1", "requests", "0", "bytes", "0").Err())
		})
		response := snAttemptUploadTestRecorder()
		serveSnAttemptUploadForAnswer(response, snAttemptUploadTestRequest(tb, client.Testing_Sign(), "metadata", []byte("data")), nil, controller.StReserveAttemptUpload)
		requireDeclaredAnswer(tb, response.Code, response.Body.String(), http.StatusServiceUnavailable, "Attempt upload quota unavailable.", "quota state", "Redis")
	})
}

// The validator admission's reason for refusing a reserved upload, which can
// carry chain endpoint errors, was appended to the 403.
func TestSnReservedAttemptUploadHidesStagingRefusalCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		first, second := newSnReservedAttemptEndpoint(tb, authority, 1), newSnReservedAttemptEndpoint(tb, authority, 2)
		data := []byte("{\"refused\":1}\n")
		header := snReservedAttemptIntentHeader(tb, authority, first, data)
		// The intent names replica 1, so replica 2 refuses it.
		status, body := snReservedAttemptResponse(tb, second.origin, first.api.GetByJwt(), header, "metadata", data)
		requireDeclaredAnswer(tb, status, body, http.StatusForbidden, "Validator staging authority refused.", "consent", "replica")
	})
}
