package api

// GET /sn/top and GET /sn/top/me through the router the API serves, with real
// tokens: the board is public and a token only adds the caller's own row;
// the detail needs a token. Run with
// `cd server/api && ../test.sh -run SnTopRoute`.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func snTopRouteGet(t testing.TB, routes *router.Router, path string, token string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, r)
	var body map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %s: %s", path, err, w.Body.String())
		}
	}
	return w.Code, body
}

func TestSnTopRouteDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		routes := router.NewRouter(ctx, Routes())
		networkId := server.NewId()
		userId := server.NewId()
		name := fmt.Sprintf("sntoproute%s", strings.ReplaceAll(networkId.String(), "-", "")[:10])
		model.Testing_CreateNetwork(ctx, networkId, name, userId)
		token := session.NewByJwt(networkId, userId, name, false, false).Testing_Sign()

		// public: no token needed
		code, body := snTopRouteGet(t, routes, "/sn/top", "")
		if code != http.StatusOK {
			t.Fatalf("GET /sn/top = %d", code)
		}
		snapshot, ok := body["snapshot"].(map[string]any)
		if !ok || snapshot["head_seats"] != float64(200) || snapshot["binding_open"] != false {
			t.Fatalf("snapshot = %v", body["snapshot"])
		}
		if rows, ok := body["rows"].([]any); !ok || len(rows) != 0 {
			t.Fatalf("rows = %v", body["rows"])
		}
		for _, key := range []string{"me", "next"} {
			if value, present := body[key]; !present || value != nil {
				t.Fatalf("%s must be present and null: %v", key, body)
			}
		}
		// a token is optional, and a bad one is refused, not ignored
		if code, _ := snTopRouteGet(t, routes, "/sn/top?limit=10&filter=unclaimed", token); code != http.StatusOK {
			t.Fatalf("GET /sn/top with a token = %d", code)
		}
		if code, _ := snTopRouteGet(t, routes, "/sn/top", "not-a-token"); code != http.StatusUnauthorized {
			t.Fatalf("GET /sn/top with a bad token = %d", code)
		}
		for _, path := range []string{"/sn/top?filter=everything", "/sn/top?limit=many", "/sn/top?cursor=nope"} {
			if code, _ := snTopRouteGet(t, routes, path, ""); code != http.StatusBadRequest {
				t.Fatalf("GET %s = %d, want 400", path, code)
			}
		}
		if code, _ := snTopRouteGet(t, routes, "/sn/top?snapshot="+server.NewId().String(), ""); code != http.StatusNotFound {
			t.Fatalf("GET /sn/top with an unknown snapshot = %d, want 404", code)
		}

		// the detail needs a token
		if code, _ := snTopRouteGet(t, routes, "/sn/top/me", ""); code != http.StatusUnauthorized {
			t.Fatalf("GET /sn/top/me without a token = %d", code)
		}
		code, body = snTopRouteGet(t, routes, "/sn/top/me", token)
		if code != http.StatusOK {
			t.Fatalf("GET /sn/top/me = %d", code)
		}
		// the board has no snapshot yet: it gives no reason, and a provider
		// is never told to turn providing on
		me, ok := body["me"].(map[string]any)
		if reason, present := me["not_measurable_reason"]; !ok || !present || reason != "" {
			t.Fatalf("me = %v", body["me"])
		}
		row, ok := me["row"].(map[string]any)
		if !ok || row["is_me"] != true || row["position"] != float64(0) || row["label"] != "not_measurable" {
			t.Fatalf("me.row = %v", me["row"])
		}
		if _, leaked := row["network_id"]; leaked {
			t.Fatalf("me.row carries a network id: %v", row)
		}
		if history, ok := me["history"].([]any); !ok || len(history) != 0 {
			t.Fatalf("me.history = %v", me["history"])
		}
	})
}
