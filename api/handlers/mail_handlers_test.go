//go:build sidecar_pending

package handlers

// POST /mail/enable and GET /mail/status over the router (mail_handlers.go;
// MAIL-M7-22, QD-478) on a private database and Redis: a client's bearer turns
// mail on once and reads its status; a closed mxedge gate and a process with
// no dns root answer 503 with nothing written; a bearer with no client is
// refused.

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

// One call of the mail routes with authorization, its status and decoded body.
func testMailCall(t testing.TB, ctx context.Context, method string, path string, authorization string) (int, map[string]any) {
	t.Helper()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/mail/enable", MailEnable),
		router.NewRoute("GET", "/mail/status", GetMailStatus),
	})
	request := httptest.NewRequest(method, path, strings.NewReader(""))
	request.RemoteAddr = "127.0.0.1:54321"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	body := map[string]any{}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s answered no json: %q", method, path, w.Body.String())
		}
	}
	return w.Code, body
}

func TestMailEnableAndStatusRoutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := fmt.Sprintf("m%s.example.", strings.ReplaceAll(server.NewId().String()[24:], "-", ""))
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "mail-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "receiver", "mail")
		byJwt := session.NewByJwt(networkId, userId, "mail", false, false)
		clientBearer := "Bearer " + byJwt.Client(deviceId, clientId).Testing_Sign()

		// no dns root in this process: dark, before the caller is looked at
		restoreRoot := model.Testing_SetDnsUserRoot("")
		if status, _ := testMailCall(t, ctx, "POST", "/mail/enable", clientBearer); status != http.StatusServiceUnavailable {
			t.Errorf("enable with no root answered %d", status)
		}
		if status, _ := testMailCall(t, ctx, "GET", "/mail/status", ""); status != http.StatusServiceUnavailable {
			t.Errorf("status with no root answered %d", status)
		}
		restoreRoot()
		defer model.Testing_SetDnsUserRoot(root)()

		// the mxedge gate is closed by default: no enable, and a status all the same
		if status, _ := testMailCall(t, ctx, "POST", "/mail/enable", clientBearer); status != http.StatusServiceUnavailable {
			t.Errorf("enable behind a closed gate answered %d", status)
		}
		if model.GetMailNetwork(ctx, networkId) != nil {
			t.Fatal("a closed gate turned mail on")
		}
		status, body := testMailCall(t, ctx, "GET", "/mail/status", clientBearer)
		if status != http.StatusOK || body["enabled"] != false || body["storage_full"] != false || body["spent"] != false || body["direct_mx"] != false {
			t.Fatalf("status before the enable: %d %v", status, body)
		}

		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  mxedge: {enabled: true, networks: [%q]}\n", networkId))()
		if status, _ := testMailCall(t, ctx, "POST", "/mail/enable", ""); status != http.StatusUnauthorized {
			t.Errorf("enable with no bearer answered %d", status)
		}
		if status, _ := testMailCall(t, ctx, "POST", "/mail/enable", "Bearer "+byJwt.Testing_Sign()); status == http.StatusOK {
			t.Errorf("enable with a bearer that names no client answered %d", status)
		}
		if model.GetMailNetwork(ctx, networkId) != nil {
			t.Fatal("a refused caller turned mail on")
		}

		status, body = testMailCall(t, ctx, "POST", "/mail/enable", clientBearer)
		enabledTime, _ := body["enabled_time"].(float64)
		mailNetwork := model.GetMailNetwork(ctx, networkId)
		if status != http.StatusOK || mailNetwork == nil || int64(enabledTime) != mailNetwork.CreateTime.UnixMilli() {
			t.Fatalf("enable: %d %v, row %+v", status, body, mailNetwork)
		}
		label := model.GetNetworkLabel(ctx, networkId, root)
		if route, err := model.GetMailRoute(ctx, label); err != nil || route == nil || route.NetworkId != networkId {
			t.Fatalf("no route after the enable: %+v %v", route, err)
		}
		status, again := testMailCall(t, ctx, "POST", "/mail/enable", clientBearer)
		if status != http.StatusOK || again["enabled_time"] != body["enabled_time"] {
			t.Fatalf("enable again: %d %v, first %v", status, again, body)
		}
		status, body = testMailCall(t, ctx, "GET", "/mail/status", clientBearer)
		if status != http.StatusOK || body["enabled"] != true || body["direct_mx"] != false {
			t.Fatalf("status after the enable: %d %v", status, body)
		}

		// a spent network at its storage limit says so, which a receiver turns into 452 4.2.2
		if err := model.MarkNetworkSpent(ctx, networkId, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		status, body = testMailCall(t, ctx, "GET", "/mail/status", clientBearer)
		if status != http.StatusOK || body["spent"] != true || body["enabled"] != true {
			t.Fatalf("status of a spent network: %d %v", status, body)
		}
	})
}
