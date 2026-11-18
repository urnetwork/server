package api

// R-refuse end to end through the router (session1118 D F1): every route that
// mints a new session answers an enforced session cap with 409, code
// session_limit_reached, and the message that points to Settings → Sessions.
// The client creation, client registration and device adoption mints raised
// the refusal inside their transaction, and the route answered 500.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The live sessions the tests fill the network with: the enforced cap.
const sessionCapRouteTestLive = 1000

// Fills the network's live session set to the cap with sessions no route
// knows, so the next new session of the network is over it.
func fillSessionCap(t testing.TB, ctx context.Context, networkId server.Id) {
	t.Helper()
	members := make([]redis.Z, sessionCapRouteTestLive)
	for i := range members {
		members[i] = redis.Z{Score: float64(server.NowUtc().AddDate(0, 0, 30).UnixMilli()), Member: server.NewId().String()}
	}
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.ZAdd(ctx, session.SessionKey(networkId, "z"), members...).Err()
	}))
}

func requireSessionLimitReached(t testing.TB, route string, status int, body string) {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("%s at the session cap: status = %d, want 409: %s", route, status, body)
	}
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatalf("%s at the session cap: body %q: %s", route, body, err)
	}
	if refusal.Code != "session_limit_reached" || !strings.Contains(refusal.Error, "Settings → Sessions") {
		t.Fatalf("%s at the session cap: body %q", route, body)
	}
}

func TestMintRoutesAnswerTheSessionCapWith409(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: sessionCapRouteTestLive, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		fixture := newRouteAccessDbFixture(t, ctx)
		fillSessionCap(t, ctx, fixture.networkId)

		// a client an API key creates starts its own session
		w := fixture.serve(http.MethodPost, "/network/auth-client", fixture.key(), map[string]any{"description": "cap", "device_spec": "cap"})
		requireSessionLimitReached(t, "POST /network/auth-client", w.Code, w.Body.String())

		// a client an API key registers starts its own session
		w = fixture.serve(http.MethodPost, "/network/register-client-v1", fixture.key(), &model.RegisterNetworkClientArgs{
			Schema: model.NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("56", 32), ScopeSha256: strings.Repeat("78", 32), Description: "cap", DeviceSpec: "cap",
		})
		requireSessionLimitReached(t, "POST /network/register-client-v1", w.Code, w.Body.String())

		// an adopted device starts its own session
		var created model.DeviceCreateAdoptCodeResult
		fixture.call(t, http.MethodPost, "/device/create-adopt-code", "", map[string]any{"device_name": "cap", "device_spec": "cap"}, http.StatusOK, &created)
		fixture.call(t, http.MethodPost, "/device/add", fixture.root(), map[string]any{"code": created.AdoptCode}, http.StatusOK, nil)
		var status model.DeviceAdoptStatusResult
		fixture.call(t, http.MethodPost, "/device/adopt-status", "", map[string]any{"adopt_code": created.AdoptCode}, http.StatusOK, &status)
		w = fixture.serve(http.MethodPost, "/device/confirm-adopt", "", map[string]any{"adopt_code": created.AdoptCode, "adopt_secret": created.AdoptSecret, "associated_network_name": status.AssociatedNetworkName})
		requireSessionLimitReached(t, "POST /device/confirm-adopt", w.Code, w.Body.String())

		// a password sign-in starts its own session
		w = fixture.serve(http.MethodPost, "/auth/login-with-password", "", map[string]any{"user_auth": fixture.networkId.String() + "@bringyour.com", "password": "password"})
		requireSessionLimitReached(t, "POST /auth/login-with-password", w.Code, w.Body.String())

		// a code login starts its own session
		var code model.AuthCodeCreateResult
		fixture.call(t, http.MethodPost, "/auth/code-create", fixture.root(), map[string]any{"uses": 1}, http.StatusOK, &code)
		w = fixture.serve(http.MethodPost, "/auth/code-login", "", map[string]any{"auth_code": code.AuthCode, "request_id": server.NewId()})
		requireSessionLimitReached(t, "POST /auth/code-login", w.Code, w.Body.String())
	})
}
