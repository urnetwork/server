// Genuine authenticated Connect dispatch drives the actual Rpc, Sql and
// immutable publication path, with the real client-side processed retry owner.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/startifact"
)

// Carries the actual serialized controller result, paused before the client
// can receive it. A success cannot be inferred from the pending Sql row.
type stClientKeyRegistrationProcessedReply struct {
	result  ConnectControlResult
	err     error
	respond chan struct{}
}

// Two controlled real failures differ only in the external boundary: one
// refuses actual authority, the other fails the history path after Sql/content.
func testStClientKeyRegistrationReadinessRetry(t testing.TB, failHistory bool) {
	t.Helper()
	fixture, credential, cfg, _ := newStClientKeyRegistrationFixture(t, 1, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	var blockedHistory string
	if failHistory {
		store, ok := server.LoadBlobStore()
		config, present := server.LoadBlobStoreConfig()
		if !ok || !present || !config.Local {
			t.Fatal("actual private local store was not configured")
		}
		prefix, err := startifact.EvidenceHistoryPrefix(store, cfg.DeploymentId, fixture.snapshot().domain.Netuid, startifact.ClientKeyRegistrationEvidenceKind)
		if err != nil {
			t.Fatal(err)
		}
		blockedHistory = filepath.Join(config.LocalPath, filepath.FromSlash(prefix))
		if err := os.MkdirAll(filepath.Dir(blockedHistory), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blockedHistory, []byte("synthetic failed history directory"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		fixture.stateLock.Lock()
		fixture.base.fault = "root"
		fixture.stateLock.Unlock()
	}
	replies := make(chan stClientKeyRegistrationProcessedReply, 2)
	var requests atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/connect/control" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		actual := httptest.NewRecorder()
		router.WrapWithInputRequireClient(ConnectControl, actual, r)
		reply := stClientKeyRegistrationProcessedReply{respond: make(chan struct{}, 1)}
		reply.err = json.Unmarshal(actual.Body.Bytes(), &reply.result)
		if actual.Code != http.StatusOK {
			reply.err = errors.Join(reply.err, errors.New("authenticated control did not return its application result"))
		}
		select {
		case replies <- reply:
		case <-r.Context().Done():
			return
		}
		select {
		case <-reply.respond:
		case <-r.Context().Done():
			return
		}
		for key, values := range actual.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(actual.Code)
		_, _ = w.Write(actual.Body.Bytes())
	}))
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	control := connect.NewApiOutOfBandControl(ctx, strategy, credential.Sign(), endpoint.URL)
	settings := connect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	settings.Log = connect.NewNoopLogger()
	settings.ClientKeyRegistrationRequired = true
	seed := sha256.Sum256(credential.ClientId[:])
	settings.ClientKeySeed = seed[:]
	client := connect.NewClient(ctx, connect.Id(*credential.ClientId), control, settings)
	t.Cleanup(func() {
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		if err := client.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		if err := control.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		strategy.Close()
		endpoint.Close()
	})
	awaitReply := func() stClientKeyRegistrationProcessedReply {
		select {
		case reply := <-replies:
			return reply
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return stClientKeyRegistrationProcessedReply{}
	}
	manager := client.ClientKeyManager()
	if manager == nil {
		t.Fatal("actual processed key manager is unavailable")
	}
	first := awaitReply()
	if first.err != nil || first.result.Error == nil || manager.Registered() {
		t.Fatal("real failed registration was acknowledged", first.err, first.result.Error)
	}
	var original model.StClientKeyHistoryRecord
	if failHistory {
		history, err := model.LoadStClientKeyHistory(ctx, fixture.snapshot().domain, *credential.ClientId, model.MaxStClientKeyHistoryRegistrations, model.MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 1 {
			t.Fatal("failed history write lost original Sql registration", len(history), err)
		}
		original = history[0]
		store, ok := server.LoadBlobStore()
		if !ok {
			t.Fatal("actual store disappeared")
		}
		key, err := startifact.EvidenceContentKey(store, original.EvidenceHash)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := store.Get(ctx, key)
		if err != nil {
			t.Fatal("original content was not published before failed history", err)
		}
		raw, err := io.ReadAll(io.LimitReader(reader, startifact.MaxClientKeyEvidenceBytes+1))
		err = errors.Join(err, reader.Close())
		if err != nil || !bytes.Equal(raw, original.EvidenceBytes) {
			t.Fatal("original content bytes differ", err)
		}
		// Only the explicitly created test obstruction is removed. The signed
		// Sql row and content object remain untouched for the exact retry.
		if err := os.Remove(blockedHistory); err != nil {
			t.Fatal(err)
		}
	} else {
		stClientKeyRegistrationAssertNoRow(t, *credential.ClientId)
		fixture.stateLock.Lock()
		fixture.base.fault = ""
		fixture.stateLock.Unlock()
	}
	first.respond <- struct{}{}
	second := awaitReply()
	if second.err != nil || second.result.Error != nil || manager.Registered() {
		t.Fatal("retry completed before actual response or remained failed", second.err, second.result.Error)
	}
	retained := stClientKeyRegistrationAssertStored(t, fixture, cfg, credential, manager.PublicKey(), fixture.snapshot().boundary)
	if failHistory && (!bytes.Equal(retained.EvidenceBytes, original.EvidenceBytes) || !bytes.Equal(retained.RegistrationBytes, original.RegistrationBytes)) {
		t.Fatal("retry re-signed or replaced the original partial publication")
	}
	second.respond <- struct{}{}
	if err := manager.WaitForRegistration(ctx); err != nil {
		t.Fatal(err)
	}
	if !manager.Registered() || requests.Load() != 2 {
		t.Fatal("actual original publication did not make the current key ready exactly once", requests.Load())
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("actual publication store disappeared")
	}
	objects, err := store.List(ctx, store.Prefix())
	if err != nil || len(objects) != 2 {
		t.Fatal("registration retry changed original content/history object census", len(objects), err)
	}
	if requests, methods := fixture.counts(); requests != 9 || methods != 41 {
		t.Fatal("retry reused stale completed authority or bypassed original reads", requests, methods)
	}
}

// The source-derived regression is a real post-Sql failure hidden inside a
// successful transport response; neither the retained row nor content is ready.
func TestStClientKeyRegistrationReadinessRetriesOriginalPublication(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) { testStClientKeyRegistrationReadinessRetry(tb, true) })
}

// A genuine authority failure has no durable row and cannot become ready; a
// later request must acquire fresh authority before its original publication.
func TestStClientKeyRegistrationReadinessRetriesAuthorityFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) { testStClientKeyRegistrationReadinessRetry(tb, false) })
}
