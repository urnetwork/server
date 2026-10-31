package api

// The API key routes owe the security notice (owner decision 2026-10-29, "Yes,
// API keys too") end to end through the router the API serves, against the
// database: POST /account/api-key and POST /account/api-key/remove each owe it
// in their change's transaction, to the network owner's email, saying how the
// change was made, and the notice holds neither key nor the caller's address.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Serves the request as the fixture's serve does, from a browser on the origin
// when one is given.
func (self *routeAccessDbFixture) serveFromOrigin(t testing.TB, path string, authorization string, origin string, body any, result any) {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	server.Raise(err)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bodyBytes))
	r.RemoteAddr = "198.51.100.42:4000"
	r.Header.Set("Authorization", authorization)
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200: %s", path, w.Code, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatalf("POST %s: %s: %s", path, err, w.Body.String())
		}
	}
}

// The fixture's backend key, created with the root token, then a key created
// with the root token from the ur.io website and deleted with the backend's API
// key: one notice to the owner's email lists the three changes in order, each
// with how it was made, and holds no key and no IP address.
func TestApiKeyRoutesOweTheSecurityNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newRouteAccessDbFixture(t, ctx)

		var created model.CreateApiKeyResult
		f.serveFromOrigin(t, "/account/api-key", f.root(), "https://ur.io", map[string]any{"name": "website key"}, &created)
		if created.Error != nil || !strings.HasPrefix(created.ApiKey, "urn_") {
			t.Fatalf("api key = %+v", created)
		}
		var removed struct {
			Error any `json:"error"`
		}
		f.serveFromOrigin(t, "/account/api-key/remove", f.key(), "", map[string]any{"id": created.Id}, &removed)
		if removed.Error != nil {
			t.Fatalf("remove = %+v", removed)
		}

		var ownerEmail string
		var userAuths, templateJsons []string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT user_auth FROM network_user WHERE user_id = $1`, f.userId).Scan(&ownerEmail))
			result, err := conn.Query(
				ctx,
				`
					SELECT user_auth, template_json::text
					FROM account_message_outbox
					WHERE network_id = $1 AND template_name = $2 AND sent_time IS NULL
					ORDER BY create_time, message_id
				`,
				f.networkId,
				"security_api_key_changed",
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var userAuth, templateJson string
					server.Raise(result.Scan(&userAuth, &templateJson))
					userAuths = append(userAuths, userAuth)
					templateJsons = append(templateJsons, templateJson)
				}
			})
		})
		if len(templateJsons) != 1 {
			t.Fatalf("the API key routes owed %d notices, want 1", len(templateJsons))
		}
		if userAuths[0] != ownerEmail {
			t.Fatalf("the notice went to %s, want the owner's %s", userAuths[0], ownerEmail)
		}
		var notice struct {
			Changes []map[string]any `json:"changes"`
		}
		if err := json.Unmarshal([]byte(templateJsons[0]), &notice); err != nil {
			t.Fatal(err)
		}
		want := []struct {
			action string
			name   string
			via    any
		}{
			// the root token, with no client info
			{"created", "backend", nil},
			{"created", "website key", "web"},
			{"deleted", "website key", "api_key"},
		}
		if len(notice.Changes) != len(want) {
			t.Fatalf("the notice lists %s, want %d changes", templateJsons[0], len(want))
		}
		for i, w := range want {
			change := notice.Changes[i]
			if change["action"] != w.action || change["name"] != w.name || change["via"] != w.via {
				t.Fatalf("change %d = %v, want %s %q made with %v: %s", i, change, w.action, w.name, w.via, templateJsons[0])
			}
		}
		if _, ok := notice.Changes[2]["key_create_time"]; !ok {
			t.Fatalf("the delete does not say when the key was created: %s", templateJsons[0])
		}
		for _, secret := range []string{created.ApiKey, f.apiKey, "urn_", "198.51.100.42"} {
			if strings.Contains(templateJsons[0], secret) {
				t.Fatalf("the notice holds %q: %s", secret, templateJsons[0])
			}
		}
	})
}
