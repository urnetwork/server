package handlers

// GET /sidecar/limits over the router (sidecar_limits_handlers.go; QD-937, owner decision 520) on a
// private database and Redis: a client's bearer reads its network's plan, the same tier and limits
// whether enforce_sidecar_limits is on or off, with enforced following the switch, so a daemon's local
// pre-checks go dark with api's refusals while ur status still shows the limits; a call with no bearer
// is refused.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// One GET /sidecar/limits with authorization: its status, its decoded body, and the body's raw fields.
func testSidecarLimitsCall(t testing.TB, ctx context.Context, authorization string) (int, *sidecar.QuotaLimits, map[string]json.RawMessage) {
	t.Helper()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("GET", "/sidecar/limits", GetSidecarLimits),
	})
	request := httptest.NewRequest("GET", "/sidecar/limits", strings.NewReader(""))
	request.RemoteAddr = "127.0.0.1:54321"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		return w.Code, nil, nil
	}
	limits := &sidecar.QuotaLimits{}
	if err := json.Unmarshal(w.Body.Bytes(), limits); err != nil {
		t.Fatalf("the limits answered no json: %q", w.Body.String())
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	return w.Code, limits, fields
}

func TestSidecarLimitsRouteAnswersEnforcedBesideThePlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{
			"directory_requests_pending": 20,
			"config_admissions":          50,
			"tls_endpoints":              1,
		})()
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "limits-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "daemon", "limits")
		byJwt := session.NewByJwt(networkId, userId, "limits", false, false)
		clientBearer := "Bearer " + byJwt.Client(deviceId, clientId).Testing_Sign()

		if status, _, _ := testSidecarLimitsCall(t, ctx, ""); status != http.StatusUnauthorized {
			t.Errorf("no bearer answered %d", status)
		}

		defer model.Testing_SetEnforceSidecarLimits(false)()
		status, dark, darkFields := testSidecarLimitsCall(t, ctx, clientBearer)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, dark.Tier, "free")
		connect.AssertEqual(t, dark.Enforced, false)
		connect.AssertEqual(t, dark.Limits["directory_requests_pending"], int64(20))
		connect.AssertEqual(t, dark.Limits["config_admissions"], int64(50))
		connect.AssertEqual(t, dark.Limits["tls_endpoints"], int64(1))
		// the field is written while off too, so a daemon never reads its absence as this api's answer
		connect.AssertEqual(t, string(darkFields["enforced"]), "false")

		model.Testing_SetEnforceSidecarLimits(true)
		status, enforced, enforcedFields := testSidecarLimitsCall(t, ctx, clientBearer)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, enforced.Enforced, true)
		connect.AssertEqual(t, string(enforcedFields["enforced"]), "true")
		connect.AssertEqual(t, enforced.Tier, dark.Tier)
		connect.AssertEqual(t, enforced.Limits, dark.Limits)
	})
}
