package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

// TestDeviceRpcHandlerAuth covers the device rpc handler's auth boundary in
// isolation (no full proxy harness): a missing or malformed signed proxy id is
// rejected before any device work, and a well-formed token for a device that
// cannot be opened surfaces as unavailable. Both the `proxy` query parameter
// (the browser form) and the `Authorization: Bearer` header (the non-browser
// form) reach the same resolution.
func TestDeviceRpcHandlerAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		pdm := NewProxyDeviceManager(ctx, DefaultProxyDeviceManagerSettings())
		defer pdm.Close()

		handler := NewDeviceRpcHandler(pdm, DefaultProxySettings())
		ts := httptest.NewServer(router.NewRouter(ctx, []*router.Route{
			router.NewRoute("GET", "/device-rpc", handler.ServeHTTP),
		}))
		defer ts.Close()

		wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

		// dial returns the http status the handler produced (gorilla surfaces the
		// response on a failed upgrade)
		dial := func(query string, header http.Header) int {
			u := wsBase + "/device-rpc"
			if query != "" {
				u += "?" + query
			}
			ws, resp, err := websocket.DefaultDialer.Dial(u, header)
			if err == nil {
				ws.Close()
			}
			if resp == nil {
				t.Fatalf("dial %q: no response (err=%v)", u, err)
			}
			return resp.StatusCode
		}

		bearer := func(token string) http.Header {
			h := http.Header{}
			h.Set("Authorization", "Bearer "+token)
			return h
		}

		// a well-formed, correctly-signed token whose proxy id has no config
		unknownSigned := model.SignProxyId(server.NewId())

		// missing signed proxy id -> 401 (before any device work)
		connect.AssertEqual(t, dial("", nil), http.StatusUnauthorized)
		// malformed query token -> 401
		connect.AssertEqual(t, dial("proxy=not-a-signed-id", nil), http.StatusUnauthorized)
		// malformed Authorization Bearer -> 401
		connect.AssertEqual(t, dial("", bearer("not-a-signed-id")), http.StatusUnauthorized)
		// well-formed token, no such device (query form) -> 503
		connect.AssertEqual(t, dial("proxy="+url.QueryEscape(unknownSigned), nil), http.StatusServiceUnavailable)
		// well-formed token, no such device (Authorization Bearer form) -> 503,
		// proving the header path reaches device resolution
		connect.AssertEqual(t, dial("", bearer(unknownSigned)), http.StatusServiceUnavailable)
	})
}
