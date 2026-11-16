//go:build sidecar_pending

package handlers

// POST and DELETE /mail/direct-mx over the router (mail_direct_mx_handlers.go;
// MAIL-M7-23) on a private database and Redis: dark with no dns root, before
// the caller is looked at; a client token, a sidecar's, refused; a network
// credential of no Owner or Admin refused; a body naming no receiver refused
// before anything is written. The model's tests run the writes themselves
// (mail_direct_mx_routes_model_test.go).

import (
	"context"
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

// One call of the direct-MX routes with authorization and body, its status.
func testMailDirectMxCall(t testing.TB, ctx context.Context, method string, authorization string, body string) int {
	t.Helper()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/mail/direct-mx", PutMailDirectMx),
		router.NewRoute("DELETE", "/mail/direct-mx", DeleteMailDirectMx),
	})
	request := httptest.NewRequest(method, "/mail/direct-mx", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:54321"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	return w.Code
}

func TestMailDirectMxRoutesRefuseWhoIsNotATeamAdmin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := fmt.Sprintf("m%s.example.", strings.ReplaceAll(server.NewId().String()[24:], "-", ""))
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "direct-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "receiver", "mail")
		byJwt := session.NewByJwt(networkId, userId, "direct", false, false)
		clientBearer := "Bearer " + byJwt.Client(deviceId, clientId).Testing_Sign()
		networkBearer := "Bearer " + byJwt.Testing_Sign()
		body := `{"receivers":[{"receiver_sidecar_id":"` + server.NewId().String() + `","address":"192.0.2.25"}]}`

		restoreRoot := model.Testing_SetDnsUserRoot("")
		for _, method := range []string{"POST", "DELETE"} {
			if status := testMailDirectMxCall(t, ctx, method, networkBearer, body); status != http.StatusServiceUnavailable {
				t.Errorf("%s with no root answered %d", method, status)
			}
		}
		restoreRoot()
		defer model.Testing_SetDnsUserRoot(root)()

		for _, method := range []string{"POST", "DELETE"} {
			if status := testMailDirectMxCall(t, ctx, method, "", body); status != http.StatusUnauthorized {
				t.Errorf("%s with no bearer answered %d", method, status)
			}
			if status := testMailDirectMxCall(t, ctx, method, clientBearer, body); status != http.StatusForbidden {
				t.Errorf("%s with a client token answered %d", method, status)
			}
			// the network's owner of record is no team member: a personal network has none, and direct MX is a Team's
			if status := testMailDirectMxCall(t, ctx, method, networkBearer, body); status != http.StatusForbidden {
				t.Errorf("%s from no Team admin answered %d", method, status)
			}
		}
		if rows := model.ListMailDirectMx(ctx, networkId); len(rows) != 0 {
			t.Fatalf("a refused caller wrote %+v", rows)
		}
	})
}
