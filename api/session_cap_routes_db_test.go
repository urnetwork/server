package api

// R-refuse end to end through the router (session1118 D F1): every route that
// mints a new session answers an enforced session cap with 409, code
// session_limit_reached, and the message that points to Account → Sessions,
// and commits nothing of the request (session1118 C). The client creation,
// client registration and device adoption mints raised the refusal inside
// their transaction, and the route answered 500. The verify committed the
// verification before its session was refused.

import (
	"context"
	"encoding/json"
	"fmt"
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

// Reports a route's answer other than the cap's refusal, and goes on to the
// next route.
func requireSessionLimitReached(t testing.TB, route string, status int, body string) {
	t.Helper()
	if status != http.StatusConflict {
		t.Errorf("%s at the session cap: status = %d, want 409: %s", route, status, body)
		return
	}
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Errorf("%s at the session cap: body %q: %s", route, body, err)
		return
	}
	if refusal.Code != "session_limit_reached" || !strings.Contains(refusal.Error, "Account → Sessions") {
		t.Errorf("%s at the session cap: body %q", route, body)
	}
}

func sessionCapRouteCount(ctx context.Context, sql string, args ...any) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&count))
	})
	return count
}

func TestMintRoutesAnswerTheSessionCapWith409(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: sessionCapRouteTestLive, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		fixture := newRouteAccessDbFixture(t, ctx)
		// a signed-in app's own live session, from before the network reached
		// the cap
		signedIn, err := session.MintNetworkSession(ctx, session.NewByJwt(fixture.networkId, fixture.userId, "signed-in", false, false), "password")
		server.Raise(err)
		fillSessionCap(t, ctx, fixture.networkId)
		clients := sessionCapRouteCount(ctx, `SELECT count(*) FROM network_client WHERE network_id=$1`, fixture.networkId)

		// a client an API key creates starts its own session
		w := fixture.serve(http.MethodPost, "/network/auth-client", fixture.key(), map[string]any{"description": "cap", "device_spec": "cap"})
		requireSessionLimitReached(t, "POST /network/auth-client", w.Code, w.Body.String())

		// a client an API key registers starts its own session
		w = fixture.serve(http.MethodPost, "/network/register-client-v1", fixture.key(), &model.RegisterNetworkClientArgs{
			Schema: model.NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("56", 32), ScopeSha256: strings.Repeat("78", 32), Description: "cap", DeviceSpec: "cap",
		})
		requireSessionLimitReached(t, "POST /network/register-client-v1", w.Code, w.Body.String())
		if after := sessionCapRouteCount(ctx, `SELECT count(*) FROM network_client WHERE network_id=$1`, fixture.networkId); after != clients {
			t.Errorf("the refused client mints left %d clients, want %d", after, clients)
		}

		// an adopted device starts its own session
		var created model.DeviceCreateAdoptCodeResult
		fixture.call(t, http.MethodPost, "/device/create-adopt-code", "", map[string]any{"device_name": "cap", "device_spec": "cap"}, http.StatusOK, &created)
		fixture.call(t, http.MethodPost, "/device/add", fixture.root(), map[string]any{"code": created.AdoptCode}, http.StatusOK, nil)
		var status model.DeviceAdoptStatusResult
		fixture.call(t, http.MethodPost, "/device/adopt-status", "", map[string]any{"adopt_code": created.AdoptCode}, http.StatusOK, &status)
		devices := sessionCapRouteCount(ctx, `SELECT count(*) FROM device WHERE network_id=$1`, fixture.networkId)
		w = fixture.serve(http.MethodPost, "/device/confirm-adopt", "", map[string]any{"adopt_code": created.AdoptCode, "adopt_secret": created.AdoptSecret, "associated_network_name": status.AssociatedNetworkName})
		requireSessionLimitReached(t, "POST /device/confirm-adopt", w.Code, w.Body.String())
		if after := sessionCapRouteCount(ctx, `SELECT count(*) FROM device WHERE network_id=$1`, fixture.networkId); after != devices {
			t.Errorf("the refused adoption left %d devices, want %d", after, devices)
		}

		// a password sign-in starts its own session
		w = fixture.serve(http.MethodPost, "/auth/login-with-password", "", map[string]any{"user_auth": fixture.networkId.String() + "@bringyour.com", "password": "password"})
		requireSessionLimitReached(t, "POST /auth/login-with-password", w.Code, w.Body.String())

		// a code login starts its own session, and a refused one keeps the
		// code's use
		var code model.AuthCodeCreateResult
		fixture.call(t, http.MethodPost, "/auth/code-create", fixture.root(), map[string]any{"uses": 1}, http.StatusOK, &code)
		w = fixture.serve(http.MethodPost, "/auth/code-login", "", map[string]any{"auth_code": code.AuthCode, "request_id": server.NewId()})
		requireSessionLimitReached(t, "POST /auth/code-login", w.Code, w.Body.String())
		if uses := sessionCapRouteCount(ctx, `SELECT coalesce(sum(remaining_uses), 0) FROM auth_code WHERE auth_code=$1`, code.AuthCode); uses != 1 {
			t.Errorf("the refused code login left %d uses, want 1", uses)
		}

		// a signed-out verify starts its own session, and a refused one keeps
		// the code unused
		userAuth := fixture.networkId.String() + "@bringyour.com"
		verifyCode, err := model.AuthVerifyCreateCode(model.AuthVerifyCreateCodeArgs{UserAuth: userAuth}, session.Testing_CreateClientSession(ctx, nil))
		server.Raise(err)
		w = fixture.serve(http.MethodPost, "/auth/verify", "", map[string]any{"user_auth": userAuth, "verify_code": *verifyCode.VerifyCode})
		requireSessionLimitReached(t, "POST /auth/verify", w.Code, w.Body.String())
		if used := sessionCapRouteCount(ctx, `SELECT count(*) FROM user_auth_verify WHERE user_id=$1 AND used`, fixture.userId); used != 0 {
			t.Errorf("the refused verify used %d codes, want 0", used)
		}

		// a signed-in verify of the network's own sign-in method keeps the
		// caller's session: no new session, so the cap does not refuse it
		var verified model.AuthVerifyResult
		fixture.call(t, http.MethodPost, "/auth/verify", "Bearer "+signedIn, map[string]any{"user_auth": userAuth, "verify_code": *verifyCode.VerifyCode}, http.StatusOK, &verified)
		if verified.Error != nil || verified.Network == nil {
			t.Fatalf("the signed-in verify: %+v", verified)
		}
		caller, err := session.ParseByJwt(ctx, signedIn)
		server.Raise(err)
		returned, err := session.ParseByJwt(ctx, verified.Network.ByJwt)
		server.Raise(err)
		if returned.SessionId == nil || caller.SessionId == nil || *returned.SessionId != *caller.SessionId {
			t.Errorf("the signed-in verify returned session %v, want the caller's %v", returned.SessionId, caller.SessionId)
		}
	})
}

// POST /auth/verify takes an optional bearer (the API spec marks it so, for
// the site and the apps, which verify an added sign-in method signed in, and
// whose sign-in dialog sends a signed-in page's token). A live network token
// of the verified network and user renews the caller's own session; any other
// bearer is served as a signed-out caller, never refused, and the verify
// starts a new session, as before. A wrong code is answered as before.
func TestAuthVerifyTakesAnOptionalBearer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		fixture := newRouteAccessDbFixture(t, ctx)
		userAuth := fixture.networkId.String() + "@bringyour.com"
		mint := func(networkId server.Id, userId server.Id) (string, *session.ByJwt) {
			claims := session.NewByJwt(networkId, userId, "verify", false, false)
			signed, err := session.MintNetworkSession(ctx, claims, "password")
			server.Raise(err)
			return signed, claims
		}
		// each code from its own address: a code's creation counts as a sign-in
		// attempt of its address, which only a verify from that address clears
		codes := 0
		verify := func(name string, authorization string) *session.ByJwt {
			t.Helper()
			codes += 1
			creator := session.Testing_CreateClientSession(ctx, nil)
			creator.ClientAddress = fmt.Sprintf("198.51.100.%d:443", codes)
			code, err := model.AuthVerifyCreateCode(model.AuthVerifyCreateCodeArgs{UserAuth: userAuth}, creator)
			server.Raise(err)
			var verified model.AuthVerifyResult
			fixture.call(t, http.MethodPost, "/auth/verify", authorization, map[string]any{"user_auth": userAuth, "verify_code": *code.VerifyCode}, http.StatusOK, &verified)
			if verified.Error != nil || verified.Network == nil {
				t.Fatalf("%s: verify %+v", name, verified)
			}
			returned, err := session.ParseByJwt(ctx, verified.Network.ByJwt)
			server.Raise(err)
			if returned.NetworkId != fixture.networkId || returned.SessionId == nil {
				t.Fatalf("%s: the verify returned %+v", name, returned)
			}
			return returned
		}

		own, ownClaims := mint(fixture.networkId, fixture.userId)
		if returned := verify("the signed-in caller", "Bearer "+own); *returned.SessionId != *ownClaims.SessionId {
			t.Error("a signed-in caller's verify started another session")
		}

		otherNetwork, otherUser := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, otherNetwork, "verify-other", otherUser)
		other, _ := mint(otherNetwork, otherUser)
		ended, endedClaims := mint(fixture.networkId, fixture.userId)
		if _, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *endedClaims.SessionId, OperationId: server.NewId()}, session.Testing_CreateClientSession(ctx, ownClaims)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name          string
			authorization string
		}{
			{"no bearer", ""},
			{"a malformed bearer", "Bearer not-a-token"},
			// a page signed in to another account signs in to this one
			{"another network's token", "Bearer " + other},
			{"an ended session's token", "Bearer " + ended},
			{"an API key", fixture.key()},
		} {
			returned := verify(c.name, c.authorization)
			if *returned.SessionId == *ownClaims.SessionId || *returned.SessionId == *endedClaims.SessionId {
				t.Errorf("%s: the verify returned an existing session", c.name)
			}
		}

		// a wrong code is answered as before, 200 with the error, whatever
		// bearer the request carries: never a 401, which ends the sdk's
		// sign-in. Four failed attempts, under the five an address may make in
		// five minutes (model.AttemptFailedCountThreshold).
		for _, c := range []struct {
			name          string
			authorization string
		}{
			{"the signed-in caller", "Bearer " + own},
			{"another network's token", "Bearer " + other},
			{"an ended session's token", "Bearer " + ended},
			{"a malformed bearer", "Bearer not-a-token"},
		} {
			var refused model.AuthVerifyResult
			fixture.call(t, http.MethodPost, "/auth/verify", c.authorization, map[string]any{"user_auth": userAuth, "verify_code": "wrong-code"}, http.StatusOK, &refused)
			if refused.Error == nil || refused.Network != nil {
				t.Errorf("%s: a wrong code answered %+v", c.name, refused)
			}
		}
	})
}
