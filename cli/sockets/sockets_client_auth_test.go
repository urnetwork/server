// T3 for the block's own Authenticator (plan-SOCK-client-auth, QD-040) on the private test Postgres and Redis that
// test-env.sh attests: real by client jwts, minted and re-minted with a sidecar_id as POST /sidecar/bind does, their
// revocation records projected by the SQL fallback as connect's recheck projects them, and the block verifying them
// against the by jwt keyring's public keys pushed as its jwt-jwks.json. The block answers GET /socket/list, a
// Client-class route that reads only the store once the caller is authenticated, so its answer is the
// authentication's: a listing, 401, denied or unavailable.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A client of a fresh network: its session, which the bind's re-mint and a removal start from.
func newClientAuthTestClient(t testing.TB, ctx context.Context, label string) *session.ClientSession {
	t.Helper()
	networkId, userId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, label, userId)
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: label})
	authResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return session.Testing_CreateClientSession(ctx, byJwt)
}

// The client's credential re-minted with sidecarId, as POST /sidecar/bind answers it, and its parsed claims.
func remintClientAuthTestToken(t testing.TB, ctx context.Context, clientSession *session.ClientSession, sidecarId server.Id) (string, *session.ByJwt) {
	t.Helper()
	signed, err := session.RemintSidecarCredential(clientSession, sidecarId)
	if err != nil {
		t.Fatal(err)
	}
	byJwt, err := session.ParseByJwt(ctx, signed)
	if err != nil {
		t.Fatal(err)
	}
	return signed, byJwt
}

// Projects byJwt's revocation record from SQL, as connect's recheck does.
func projectClientAuthTestRecord(t testing.TB, ctx context.Context, byJwt *session.ByJwt) {
	t.Helper()
	if err := session.ValidateByJwtState(ctx, byJwt, true); err != nil {
		t.Fatal(err)
	}
}

// One GET /socket/list with token: its status and, for a 200, its body.
func clientAuthTestList(t testing.TB, host *socketsHost, token string) (int, *sidecar.SocketListResult) {
	t.Helper()
	recorder := hostTestServe(host, http.MethodGet, "/socket/list", token, nil, "")
	if recorder.Code != http.StatusOK {
		return recorder.Code, nil
	}
	result := &sidecar.SocketListResult{}
	if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil {
		t.Fatal(err)
	}
	return recorder.Code, result
}

// A real block, its Authenticator the block's own: a valid token with its sidecar passes offline and the route
// answers; a token with no sidecar outside urnetwork-edge is denied; a removed client's token answers 401 at once; a
// record that cannot answer and a lost store answer unavailable with a wait, never an allow.
func TestHostAnswersAnAuthenticatedRouteWithClientTokens(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		jwks, err := session.Testing_ByJwtPublicJwks()
		if err != nil {
			t.Fatal(err)
		}
		defer server.Vault.PushSimpleResource(session.ByJwtPublicJwksResourceName, jwks)()

		listenerSession := newClientAuthTestClient(t, ctx, "sockets-client-auth")
		listenerToken, listener := remintClientAuthTestToken(t, ctx, listenerSession, server.NewId())
		projectClientAuthTestRecord(t, ctx, listener)
		unboundToken := listenerSession.ByJwt.Testing_Sign()
		unprojectedSession := newClientAuthTestClient(t, ctx, "sockets-client-auth-cold")
		unprojectedToken, _ := remintClientAuthTestToken(t, ctx, unprojectedSession, server.NewId())

		world := newHostTestWorld(t)
		host := world.newHost(t, func(deps *hostDeps) {
			deps.Authenticator = nil
		})

		code, result := clientAuthTestList(t, host, listenerToken)
		if code != http.StatusOK || result.Error != nil {
			t.Fatalf("a valid token with its sidecar answered %d, %+v", code, result)
		}
		code, result = clientAuthTestList(t, host, unboundToken)
		if code != http.StatusOK || result.Error == nil || result.Error.Code != sidecar.SocketErrorCodeDenied {
			t.Fatalf("a token with no sidecar answered %d, %+v", code, result)
		}

		code, result = clientAuthTestList(t, host, unprojectedToken)
		if code != http.StatusOK || result.Error == nil || result.Error.Code != sidecar.SocketErrorCodeUnavailable || result.Error.RetryAfterMs == 0 {
			t.Fatalf("a record that cannot answer answered %d, %+v", code, result)
		}
		func() {
			defer session.Testing_FailClientTokenRecordReads(errors.New("synthetic redis loss"))()
			code, result := clientAuthTestList(t, host, listenerToken)
			if code != http.StatusOK || result.Error == nil || result.Error.Code != sidecar.SocketErrorCodeUnavailable || result.Error.RetryAfterMs == 0 {
				t.Fatalf("a lost store answered %d, %+v", code, result)
			}
		}()

		// the client's removal, as a removal commits it: the record's tombstone first
		clientId := *listener.ClientId
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT client_id FROM network_client WHERE client_id = $1 FOR UPDATE`, clientId))
			server.Raise(session.RevokeClientRecordsInTx(ctx, tx, listener.NetworkId, []server.Id{clientId}))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, clientId))
		})
		if code, result := clientAuthTestList(t, host, listenerToken); code != http.StatusUnauthorized {
			t.Fatalf("a removed client's token answered %d, %+v", code, result)
		}
	})
}
