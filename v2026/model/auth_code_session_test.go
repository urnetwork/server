package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func authCodeSessionCreator(t testing.TB) *session.ClientSession {
	t.Helper()
	ctx := t.Context()
	network, user := server.NewId(), server.NewId()
	Testing_CreateNetwork(ctx, network, "auth-code-session-"+network.String(), user)
	claims := session.NewByJwt(network, user, "auth-code-session", false, false)
	if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
		t.Fatal(err)
	}
	actor := session.NewLocalClientSession(ctx, "192.0.2.1:1", claims)
	t.Cleanup(actor.Cancel)
	return actor
}

func authCodeSessionCode(t testing.TB, actor *session.ClientSession) string {
	t.Helper()
	result, err := AuthCodeCreate(&AuthCodeCreateArgs{Uses: 1}, actor)
	if err != nil || result == nil || result.Error != nil || result.AuthCode == "" {
		t.Fatal("code creation failed", result, err)
	}
	return result.AuthCode
}

func TestAuthCodeSessionLostResponseReplayIsIndependent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		creator := authCodeSessionCreator(t)
		code := authCodeSessionCode(t, creator)
		requestId := server.NewId()
		login := session.NewLocalClientSession(t.Context(), "192.0.2.2:1", nil)
		defer login.Cancel()
		args := &AuthCodeLoginArgs{AuthCode: code, RequestId: &requestId}
		result, err := AuthCodeLogin(args, login)
		if err != nil || result == nil || result.Error != nil || result.ByJwt == "" {
			t.Fatal("redemption failed", result, err)
		}
		claims, err := session.ParseByJwt(t.Context(), result.ByJwt)
		if err != nil || claims.SessionId == nil || *claims.SessionId == *creator.ByJwt.SessionId {
			t.Fatal("redemption did not create independent session", claims, err)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var remaining int
			server.Raise(conn.QueryRow(t.Context(), `SELECT COUNT(*) FROM auth_code WHERE auth_code=$1`, code).Scan(&remaining))
			if remaining != 0 {
				t.Fatal("last-use code was not deleted")
			}
		})
		// The creator is cut off before the lost response is retried. A committed
		// independent redemption depends on its own session, not its provenance.
		observerClaims := session.NewByJwt(creator.ByJwt.NetworkId, creator.ByJwt.UserId, "auth-code-session", false, false)
		if _, err := session.MintNetworkSession(t.Context(), observerClaims, "password"); err != nil {
			t.Fatal(err)
		}
		observer := session.NewLocalClientSession(t.Context(), "192.0.2.3:1", observerClaims)
		defer observer.Cancel()
		if _, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *creator.ByJwt.SessionId, OperationId: server.NewId()}, observer); err != nil {
			t.Fatal(err)
		}
		replayed, err := AuthCodeLogin(args, login)
		if err != nil || replayed == nil || replayed.Error != nil || replayed.ByJwt != result.ByJwt {
			t.Fatal("committed redemption retry changed or lost result", replayed, err)
		}
		if err := session.ValidateByJwtState(t.Context(), claims, false); err != nil {
			t.Fatal("creator revocation killed independent redemption", err)
		}
		otherRequest := server.NewId()
		if another, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: code, RequestId: &otherRequest}, login); err != nil || another == nil || another.Error == nil || another.ByJwt != "" {
			t.Fatal("new operation consumed deleted one-use code", another, err)
		}
	})
}

func TestAuthCodeSessionRevokedCreatorCannotRedeemPendingCode(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		creator := authCodeSessionCreator(t)
		code := authCodeSessionCode(t, creator)
		// Redis cutoff precedes SQL code cleanup: the origin marker itself must
		// fence redemption, rather than relying on eventual code deactivation.
		if _, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *creator.ByJwt.SessionId, OperationId: server.NewId()}, creator); err != nil {
			t.Fatal(err)
		}
		requestId := server.NewId()
		login := session.NewLocalClientSession(t.Context(), "192.0.2.2:1", nil)
		defer login.Cancel()
		result, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: code, RequestId: &requestId}, login)
		var refusal *session.SessionError
		knownRevoked := errors.Is(err, session.ErrSessionRevoked) || errors.As(err, &refusal) && refusal.Status == 401 && refusal.Code == "session_revoked"
		if !knownRevoked || result != nil && result.ByJwt != "" {
			t.Fatal("pending code crossed creator cutoff", result, err)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var remaining int
			server.Raise(conn.QueryRow(t.Context(), `SELECT remaining_uses FROM auth_code WHERE auth_code=$1`, code).Scan(&remaining))
			if remaining != 1 {
				t.Fatal("refused redemption consumed code")
			}
		})
	})
}

func TestAuthCodeSessionRegistrationFailurePreservesUseAndIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		creator := authCodeSessionCreator(t)
		code := authCodeSessionCode(t, creator)
		network := creator.ByJwt.NetworkId
		// Fill live admission capacity without spending 1,000 SQL transactions.
		members := make([]redis.Z, session.NetworkSessionLiveLimit)
		remove := make([]any, len(members))
		for i := range members {
			id := server.NewId().String()
			members[i] = redis.Z{Score: float64(server.NowUtc().Add(time.Hour).UnixMilli()), Member: id}
			remove[i] = id
		}
		server.Raise(server.RedisAuth(t.Context(), func(ctx context.Context, r server.RedisClient) error {
			return r.ZAdd(ctx, session.SessionKey(network, "z"), members...).Err()
		}))
		requestId := server.NewId()
		args := &AuthCodeLoginArgs{AuthCode: code, RequestId: &requestId}
		login := session.NewLocalClientSession(t.Context(), "192.0.2.2:1", nil)
		defer login.Cancel()
		result, err := AuthCodeLogin(args, login)
		var refusal *session.SessionError
		if !errors.As(err, &refusal) || result != nil && result.ByJwt != "" {
			t.Fatal("registration at full capacity unexpectedly succeeded", result, err)
		}
		var prepared session.ByJwt
		server.Db(t.Context(), func(conn server.PgConn) {
			var remaining int
			var encoded []byte
			var status string
			server.Raise(conn.QueryRow(t.Context(), `SELECT remaining_uses FROM auth_code WHERE auth_code=$1`, code).Scan(&remaining))
			server.Raise(conn.QueryRow(t.Context(), `SELECT credential,status FROM auth_code_redemption WHERE network_id=$1 AND request_id=$2`, network, requestId).Scan(&encoded, &status))
			server.Raise(json.Unmarshal(encoded, &prepared))
			if remaining != 1 || status != "prepared" || prepared.SessionId == nil {
				t.Fatal("failed registration lost its code use or stable prepared identity")
			}
		})
		server.Raise(server.RedisAuth(t.Context(), func(ctx context.Context, r server.RedisClient) error {
			return r.ZRem(ctx, session.SessionKey(network, "z"), remove...).Err()
		}))
		retried, err := AuthCodeLogin(args, login)
		if err != nil || retried == nil || retried.Error != nil || retried.ByJwt == "" {
			t.Fatal("registration could not resume", retried, err)
		}
		claims, err := session.ParseByJwt(t.Context(), retried.ByJwt)
		if err != nil || claims.SessionId == nil || *claims.SessionId != *prepared.SessionId || !claims.CreateTime.Equal(prepared.CreateTime) || !claims.ExpiresAt.Time.Equal(prepared.ExpiresAt.Time) {
			t.Fatal("retry allocated or extended immutable redemption", claims, prepared, err)
		}
	})
}

func TestAuthCodeSessionRequestIdConflictDoesNotConsumeAnotherCode(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		creator := authCodeSessionCreator(t)
		foreign := authCodeSessionCreator(t)
		code := authCodeSessionCode(t, creator)
		otherCode := authCodeSessionCode(t, foreign)
		login := session.NewLocalClientSession(t.Context(), "192.0.2.2:1", nil)
		defer login.Cancel()
		requestId := server.NewId()
		first, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: code, RequestId: &requestId}, login)
		if err != nil || first == nil || first.Error != nil || first.ByJwt == "" {
			t.Fatal(first, err)
		}
		conflict, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: otherCode, RequestId: &requestId}, login)
		var refusal *session.SessionError
		if conflict != nil || !errors.As(err, &refusal) || refusal.Status != 409 || refusal.Code != "request_id_conflict" {
			t.Fatal("different payload reused redemption identity", conflict, err)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var remaining, receipts int
			server.Raise(conn.QueryRow(t.Context(), `SELECT remaining_uses FROM auth_code WHERE auth_code=$1`, otherCode).Scan(&remaining))
			server.Raise(conn.QueryRow(t.Context(), `SELECT COUNT(*) FROM auth_code_redemption WHERE request_id=$1`, requestId).Scan(&receipts))
			if remaining != 1 || receipts != 1 {
				t.Fatal("conflict spent a use or created a second receipt", remaining, receipts)
			}
		})
	})
}
