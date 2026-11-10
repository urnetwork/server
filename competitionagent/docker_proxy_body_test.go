//go:build linux

// Real HTTP transport must preserve absent request bodies: an empty spool
// reader is not interchangeable with NoBody on Docker's action endpoints.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Ownership inspection and durable mutation admission remain enabled. The
// fake daemon enforces Docker's bodyless-action rule using received framing.
func TestDockerProxyPreservesAbsentBodiesForOwnedActions(t *testing.T) {
	for _, action := range []string{"start", "stop", "kill", "restart", "pause", "unpause", "wait", ""} {
		fixture := newDockerProxyTest(t)
		id := strings.Repeat("a", 64)
		calls := &atomic.Int64{}
		fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": id,
					"Config":     map[string]any{"Labels": map[string]string{cleanupJobLabel: fixture.plan.JobId, cleanupRoundLabel: fixture.plan.RoundId, dockerProxyRunLabel: fixture.plan.RunId}},
					"HostConfig": map[string]any{"CgroupParent": fixture.plan.CgroupNames[len(fixture.plan.CgroupNames)-1]}})
				return
			}
			calls.Add(1)
			content, err := io.ReadAll(r.Body)
			if err != nil || len(content) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
				http.Error(w, "bodyless Docker action received a request body", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		method, path := http.MethodPost, "/containers/"+id+"/"+action
		if action == "" {
			method, path = http.MethodDelete, "/containers/"+id
		}
		response := httptest.NewRecorder()
		fixture.proxy.workerHandler(response, httptest.NewRequest(method, "http://worker/v1.56"+path, nil))
		if response.Code != http.StatusNoContent || calls.Load() != 1 || fixture.proxy.Snapshot().UnknownOperations != 0 {
			t.Fatal("empty action body acquired chunked framing or lost custody", action, response.Code, calls.Load(), response.Body.String())
		}
	}
}

// Fixing absent bodies must not drop or change actual create payload bytes,
// length or the durable response accounting of nonempty Docker mutations.
func TestDockerProxyPreservesNonemptySealedMutationBody(t *testing.T) {
	fixture := newDockerProxyTest(t)
	calls := &atomic.Int64{}
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		content, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(content, fixture.create) || r.ContentLength != int64(len(fixture.create)) || len(r.TransferEncoding) != 0 {
			http.Error(w, "sealed create payload or framing changed", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create", bytes.NewReader(fixture.create)))
	if response.Code != http.StatusCreated || calls.Load() != 1 || fixture.proxy.Snapshot().UnknownOperations != 0 {
		t.Fatal("nonempty sealed mutation lost exact bytes or custody", response.Code, calls.Load())
	}
}
