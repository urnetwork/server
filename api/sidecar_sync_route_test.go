package api

// §6.8's sync routes end to end through the router the API serves (PROTO-M1-10), with a real client
// token: poll and versions answer a binary PollResponse, core JSON, other topics 404; a closed push gate
// answers 503 with Retry-After; and a removed client's poll is 401 and writes nothing, not even its poll
// marker (FS-37's inputs).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

func TestSidecarSyncRoutes(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		self := newRouteAccessDbFixture(t, ctx)
		defer controller.Testing_CloseSidecarSyncLongPoll()
		clientId, clientToken := self.mintClient(t, self.root(), nil)
		bearer := "Bearer " + clientToken

		// the gate closed: 503 with Retry-After, before any authentication
		popOff := server.Testing_PushSidecarConfig("sidecar:\n  push:\n    enabled: false\n")
		w := self.serve(http.MethodGet, "/sidecar/sync/poll", bearer, nil)
		popOff()
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Fatalf("closed gate: %d %v", w.Code, w.Header())
		}

		defer server.Testing_PushSidecarConfig("sidecar:\n  push:\n    enabled: true\n")()
		if err := model.SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: 9}); err != nil {
			t.Fatal(err)
		}
		readPoll := func(path string) *protocol.PollResponse {
			t.Helper()
			w := self.serve(http.MethodGet, path, bearer, nil)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
			}
			response := &protocol.PollResponse{}
			if err := proto.Unmarshal(w.Body.Bytes(), response); err != nil {
				t.Fatal(err)
			}
			return response
		}
		first := readPoll("/sidecar/sync/poll")
		if first.Reset_ == nil || len(first.Hints) != 1 || first.Hints[0].Version != 9 || len(first.Cursor) == 0 {
			t.Fatalf("poll %v", first)
		}
		versions := readPoll("/sidecar/sync/versions")
		if versions.Reset_ == nil || !versions.Reset_.Complete || len(versions.Hints) != 0 {
			t.Fatalf("versions %v", versions)
		}
		if w := self.serve(http.MethodGet, "/sidecar/sync/poll?cursor=%21%21", bearer, nil); w.Code != http.StatusBadRequest {
			t.Errorf("a cursor that is no base64url: %d", w.Code)
		}
		// a cursor read back as the route gave it
		if err := model.SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: 10}); err != nil {
			t.Fatal(err)
		}
		second := readPoll("/sidecar/sync/poll?cursor=" + base64.RawURLEncoding.EncodeToString(first.Cursor))
		if second.Reset_ != nil || len(second.Hints) != 1 || second.Hints[0].Version != 10 {
			t.Fatalf("second poll %v", second)
		}

		var core controller.SidecarSyncCoreResult
		self.call(t, http.MethodGet, "/sidecar/sync/core", bearer, nil, http.StatusOK, &core)
		if _, err := pushservice.ParseVersion(core.Version); err != nil || core.Gates == nil {
			t.Errorf("core %+v", core)
		}
		if w := self.serve(http.MethodGet, "/sidecar/sync/team", bearer, nil); w.Code != http.StatusNotFound {
			t.Errorf("team: %d", w.Code)
		}

		// a removed client: 401, and its poll marker is not written
		var removed json.RawMessage
		self.call(t, http.MethodPost, "/network/remove-client", self.root(), map[string]any{"client_id": clientId}, http.StatusOK, &removed)
		pushRedis := model.NewSidecarPushRedis(nil)
		// the earlier polls set it: clear it, so a write by the refused polls would show
		server.Redis(ctx, func(client server.RedisClient) {
			client.Del(ctx, pushservice.HintPollKey(connect.Id(clientId)))
		})
		for _, path := range []string{"/sidecar/sync/poll", "/sidecar/sync/versions", "/sidecar/sync/core"} {
			if w := self.serve(http.MethodGet, path, bearer, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s for a removed client: %d", path, w.Code)
			}
		}
		if _, found, err := pushRedis.Get(ctx, pushservice.HintPollKey(connect.Id(clientId))); err != nil || found {
			t.Errorf("a removed client's poll wrote its marker: %t, %v", found, err)
		}
	})
}
