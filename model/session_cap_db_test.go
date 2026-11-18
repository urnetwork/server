package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promdto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A network with a tracked password sign-in, the sign-in's session, and an API
// key session as session/client_session.go builds one.
type sessionCapTestNetwork struct {
	networkId     server.Id
	userId        server.Id
	signin        *session.ClientSession
	apiKeySession *session.ClientSession
}

func newSessionCapTestNetwork(t testing.TB, ctx context.Context) *sessionCapTestNetwork {
	t.Helper()
	networkId, userId := server.NewId(), server.NewId()
	Testing_CreateNetwork(ctx, networkId, "session-cap", userId)
	claims := session.NewByJwt(networkId, userId, "session-cap", false, false)
	if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
		t.Fatal(err)
	}
	apiKeySession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "session-cap", false, false))
	apiKeySession.ApiKeyAuthenticated = true
	return &sessionCapTestNetwork{
		networkId:     networkId,
		userId:        userId,
		signin:        session.Testing_CreateClientSession(ctx, claims),
		apiKeySession: apiKeySession,
	}
}

// The client a mint created and the session its token carries.
func sessionCapTestClient(t testing.TB, ctx context.Context, mint func() (*AuthNetworkClientResult, error)) (server.Id, *session.ByJwt) {
	t.Helper()
	result, err := mint()
	if err != nil || result == nil || result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		t.Fatalf("client mint: %+v %v", result, err)
	}
	claims, err := session.ParseByJwt(ctx, *result.ByClientJwt)
	if err != nil || claims.SessionId == nil {
		t.Fatal("client token carries no session", err)
	}
	return *result.ClientId, claims
}

func sessionCapMembers(t testing.TB, ctx context.Context, networkId server.Id, suffix string) map[server.Id]bool {
	t.Helper()
	members := map[server.Id]bool{}
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		values, err := r.ZRange(ctx, session.SessionKey(networkId, suffix), 0, -1).Result()
		for _, value := range values {
			if id, err := server.ParseId(value); err == nil {
				members[id] = true
			}
		}
		return err
	}))
	return members
}

func sessionCapKind(t testing.TB, ctx context.Context, networkId, sessionId server.Id) string {
	t.Helper()
	var kind string
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		kind, err = r.HGet(ctx, session.SessionKey(networkId, "s:"+sessionId.String()), "kind").Result()
		return err
	}))
	return kind
}

// Waits for the post-commit free of a removal, which runs after its commit.
func waitSessionFreed(t testing.TB, ctx context.Context, networkId, sessionId server.Id) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for sessionCapMembers(t, ctx, networkId, "z")[sessionId] {
		if time.Now().After(deadline) {
			t.Fatal("the removed client's session was not freed", sessionId)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// R-free: removing a client frees the session it owns, with no marker. The
// client-active check refuses the session's every credential. Removing a
// client never frees the sign-in session other clients share.
func TestRemovedClientFreesItsOwnSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		keyClient, keyClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "api key client", DeviceSpec: "test"}, network.apiKeySession)
		})
		connect.AssertEqual(t, session.MintKindApiKeyClient, sessionCapKind(t, ctx, network.networkId, *keyClaims.SessionId))
		deviceClient, deviceClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"}, network.signin)
		})
		connect.AssertEqual(t, *network.signin.ByJwt.SessionId, *deviceClaims.SessionId)

		before := sessionFreedCount(t, session.SessionFreePathRemoveClient)
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: keyClient}, network.signin)
		if err != nil || removed.Error != nil {
			t.Fatal(removed, err)
		}
		waitSessionFreed(t, ctx, network.networkId, *keyClaims.SessionId)
		if sessionCapMembers(t, ctx, network.networkId, "held")[*keyClaims.SessionId] {
			t.Fatal("a freed session kept its retained reservation")
		}
		if err := session.CheckSession(ctx, network.networkId, *keyClaims.SessionId); err != nil {
			t.Fatal("a free wrote a marker", err)
		}
		if err := session.ValidateByJwtState(ctx, keyClaims, true); err == nil {
			t.Fatal("the removed client's credential still validates")
		}
		connect.AssertEqual(t, 1.0, sessionFreedCount(t, session.SessionFreePathRemoveClient)-before)

		removed, err = RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: deviceClient}, network.signin)
		if err != nil || removed.Error != nil {
			t.Fatal(removed, err)
		}
		time.Sleep(200 * time.Millisecond)
		if !sessionCapMembers(t, ctx, network.networkId, "z")[*network.signin.ByJwt.SessionId] {
			t.Fatal("removing a client freed the sign-in session")
		}
	})
}

// R-free: the 30-day idle reap (RemoveDisconnectedNetworkClients' raw mark)
// frees the marked client's own session, as a removal does.
func TestIdleReapFreesTheClientsOwnSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		keyClient, keyClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "api key client", DeviceSpec: "test"}, network.apiKeySession)
		})
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time=$2 WHERE client_id=$1`, keyClient, now.Add(-31*24*time.Hour)))
		})
		before := sessionFreedCount(t, session.SessionFreePathIdleReap)
		RemoveDisconnectedNetworkClients(ctx, now.Add(-time.Hour), now.Add(-NetworkClientReapAfterDeactivate), now.Add(-TopLevelClientIdleExpiration))
		var active bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, keyClient).Scan(&active))
		})
		if active {
			t.Fatal("the idle client was not marked")
		}
		waitSessionFreed(t, ctx, network.networkId, *keyClaims.SessionId)
		connect.AssertEqual(t, 1.0, sessionFreedCount(t, session.SessionFreePathIdleReap)-before)
	})
}

// R-free: the stale child delete hard-deletes a child that owns its session (a
// hosted proxy a client created as its child) and frees that session.
func TestStaleChildDeleteFreesAChildOwnedSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		parent, parentClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"}, network.signin)
		})
		child, childClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			result := testAuthDerivedClient(t, network.signin, parent, true)
			return result, nil
		})
		if *childClaims.SessionId == *parentClaims.SessionId {
			t.Fatal("a child hosted proxy inherited its parent's session")
		}
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time=$2 WHERE client_id=$1`, child, now.Add(-31*24*time.Hour)))
		})
		before := sessionFreedCount(t, session.SessionFreePathStaleChild)
		RemoveDisconnectedNetworkClients(ctx, now.Add(-time.Hour), now.Add(-NetworkClientReapAfterDeactivate), now.Add(-TopLevelClientIdleExpiration))
		waitSessionFreed(t, ctx, network.networkId, *childClaims.SessionId)
		connect.AssertEqual(t, 1.0, sessionFreedCount(t, session.SessionFreePathStaleChild)-before)
		if !sessionCapMembers(t, ctx, network.networkId, "z")[*parentClaims.SessionId] {
			t.Fatal("the stale child delete freed the parent's sign-in session")
		}
	})
}

// The revocation cleanup deactivates the clients of revoked sessions; it frees
// nothing (it would only find the sessions' markers).
func TestSessionCleanupRunsNoFree(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		keyClient, keyClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "api key client", DeviceSpec: "test"}, network.apiKeySession)
		})
		frees := sessionStorageCount(t, "free")
		result, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *keyClaims.SessionId, OperationId: server.NewId()}, network.signin)
		if err != nil || result.State != "enforced" {
			t.Fatal(result, err)
		}
		complete, err := CleanupSessionOperation(ctx, session.SessionCleanupOperation{NetworkId: network.networkId, OperationId: result.OperationId, TargetSessionIds: result.TargetSessionIds})
		if err != nil || !complete {
			t.Fatal("cleanup", complete, err)
		}
		var active bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, keyClient).Scan(&active))
		})
		if active {
			t.Fatal("cleanup left the client active")
		}
		time.Sleep(200 * time.Millisecond)
		if sessionStorageCount(t, "free") != frees {
			t.Fatal("the revocation cleanup ran a free")
		}
		if !errors.Is(session.CheckSession(ctx, network.networkId, *keyClaims.SessionId), session.ErrSessionRevoked) {
			t.Fatal("the revoked session lost its marker")
		}
	})
}

// The samples urnetwork_session_storage_seconds holds for an operation.
func sessionStorageCount(t testing.TB, operation string) uint64 {
	t.Helper()
	count := uint64(0)
	for _, metric := range gatheredSessionMetrics(t, "urnetwork_session_storage_seconds") {
		if metricLabel(metric, "operation") == operation {
			count += metric.GetHistogram().GetSampleCount()
		}
	}
	return count
}

// urnetwork_session_freed_on_removal_total for the path.
func sessionFreedCount(t testing.TB, path string) float64 {
	t.Helper()
	for _, metric := range gatheredSessionMetrics(t, "urnetwork_session_freed_on_removal_total") {
		if metricLabel(metric, "path") == path {
			return metric.GetCounter().GetValue()
		}
	}
	return 0
}

func gatheredSessionMetrics(t testing.TB, name string) []*promdto.Metric {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()
		}
	}
	return nil
}

func metricLabel(metric *promdto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

// R-reset (S2 N9, owner decision 4): a sign-out-all (the password reset's
// rotation and retirement) retires the sign-in but not a hosted proxy's own
// session, and the proxy keeps minting: its lineage moves past the cutoff.
// Before, the reset retired the proxy's session and its stored lineage was
// refused by the rotation, so the proxy could never mint again.
func TestHostedProxySurvivesSignOutAllAndKeepsMinting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		// a proxy from before sessions: bootstrapped at its first hosted mint
		legacyProxy := server.NewId()
		Testing_CreateDevice(ctx, network.networkId, server.NewId(), legacyProxy, "hosted", "test")
		// a proxy created with the sign-in: a session of its own
		created, _ := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return testAuthProxyClient(t, network.signin), nil
		})
		mint := func(clientId server.Id) (*session.ByJwt, string, error) {
			loaded, err := session.LoadByJwtFromClientId(ctx, clientId)
			if err != nil {
				return nil, "", err
			}
			signed, err := session.MintHostedSession(ctx, loaded)
			return loaded, signed, err
		}
		before := map[server.Id]*session.ByJwt{}
		for _, proxy := range []server.Id{legacyProxy, created} {
			claims, _, err := mint(proxy)
			if err != nil {
				t.Fatal("hosted mint before the reset", err)
			}
			before[proxy] = claims
		}
		connect.AssertEqual(t, session.MintKindLegacyProxy, sessionCapKind(t, ctx, network.networkId, *before[legacyProxy].SessionId))
		connect.AssertEqual(t, session.MintKindHostedProxy, sessionCapKind(t, ctx, network.networkId, *before[created].SessionId))

		plan, err := PlanNetworkSignOut(ctx, network.networkId, false)
		if err != nil || plan.Refusal != "" {
			t.Fatal(plan, err)
		}
		result, err := CommitNetworkSignOut(ctx, network.networkId, false, plan.Digest)
		if err != nil || result.EnforcementError != nil {
			t.Fatal(result, err)
		}
		if !errors.Is(session.CheckSession(ctx, network.networkId, *network.signin.ByJwt.SessionId), session.ErrSessionRevoked) {
			t.Fatal("the sign-out spared the sign-in")
		}
		for proxy, claims := range before {
			if err := session.CheckSession(ctx, network.networkId, *claims.SessionId); err != nil {
				t.Fatal("the sign-out retired a hosted proxy's session", err)
			}
			if err := session.ValidateByJwtState(ctx, claims, true); err == nil {
				t.Fatal("a credential minted before the rotation still validates")
			}
			renewed, signed, err := mint(proxy)
			if err != nil || signed == "" {
				t.Fatal("the proxy could not mint after the sign-out", err)
			}
			if *renewed.SessionId != *claims.SessionId || renewed.CreateTime.Before(result.Cutoff) {
				t.Fatal("the proxy's session or lineage", renewed.SessionId, renewed.CreateTime, result.Cutoff)
			}
			if err := session.ValidateByJwtState(ctx, renewed, true); err != nil {
				t.Fatal("the proxy's new credential does not validate", err)
			}
			var active bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, proxy).Scan(&active))
			})
			if !active {
				t.Fatal("the sign-out deactivated a hosted proxy")
			}
		}
	})
}

// R-reset: revoking a hosted proxy's own session ends the proxy, and it can
// never bootstrap another.
func TestHostedProxyOwnRevokeEndsItForGood(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		proxy := server.NewId()
		Testing_CreateDevice(ctx, network.networkId, server.NewId(), proxy, "hosted", "test")
		loaded, err := session.LoadByJwtFromClientId(ctx, proxy)
		server.Raise(err)
		if _, err := session.MintHostedSession(ctx, loaded); err != nil {
			t.Fatal(err)
		}
		live := len(sessionCapMembers(t, ctx, network.networkId, "z"))
		result, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *loaded.SessionId, OperationId: server.NewId()}, network.signin)
		if err != nil || result.RevokedCount != 1 {
			t.Fatal(result, err)
		}
		again, err := session.LoadByJwtFromClientId(ctx, proxy)
		server.Raise(err)
		if signed, err := session.MintHostedSession(ctx, again); signed != "" || !errors.Is(err, session.ErrSessionRevoked) {
			t.Fatal("a revoked proxy minted", err)
		}
		if _, err := CleanupSessionOperation(ctx, session.SessionCleanupOperation{NetworkId: network.networkId, OperationId: result.OperationId, TargetSessionIds: result.TargetSessionIds}); err != nil {
			t.Fatal(err)
		}
		after, err := session.LoadByJwtFromClientId(ctx, proxy)
		server.Raise(err)
		if signed, err := session.MintHostedSession(ctx, after); signed != "" || err == nil {
			t.Fatal("a removed proxy bootstrapped another session")
		}
		if got := len(sessionCapMembers(t, ctx, network.networkId, "z")); got != live-1 {
			t.Fatal("live sessions", got, live)
		}
	})
}

// Owner decision 1: a hosted proxy keeps its own listed session even when a
// sign-in creates it, so revoking that sign-in does not end it.
func TestHostedProxyCreatedBySignInHasItsOwnSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		proxy, claims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return testAuthProxyClient(t, network.signin), nil
		})
		if *claims.SessionId == *network.signin.ByJwt.SessionId {
			t.Fatal("the proxy inherited its creator's sign-in session")
		}
		connect.AssertEqual(t, session.MintKindHostedProxy, sessionCapKind(t, ctx, network.networkId, *claims.SessionId))
		other := session.Testing_CreateClientSession(ctx, func() *session.ByJwt {
			claims := session.NewByJwt(network.networkId, network.userId, "session-cap", false, false)
			if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
				t.Fatal(err)
			}
			return claims
		}())
		if _, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *network.signin.ByJwt.SessionId, OperationId: server.NewId()}, other); err != nil {
			t.Fatal(err)
		}
		if err := session.CheckSession(ctx, network.networkId, *claims.SessionId); err != nil {
			t.Fatal("revoking the creator's sign-in revoked the proxy", err)
		}
		loaded, err := session.LoadByJwtFromClientId(ctx, proxy)
		server.Raise(err)
		if _, err := session.MintHostedSession(ctx, loaded); err != nil || *loaded.SessionId != *claims.SessionId {
			t.Fatal("the proxy does not mint into its own session", err)
		}
	})
}

// Owner decision 1: an API key (or MCP identity) reissuing its client keeps
// the client's one session. Each reissue used to start another session and
// rebind the client, leaving the old one listed and unowned.
func TestApiKeyReissueKeepsTheClientsSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		client, claims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "api key client", DeviceSpec: "test"}, network.apiKeySession)
		})
		live := len(sessionCapMembers(t, ctx, network.networkId, "z"))
		for range 3 {
			_, reissued := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
				return AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &client, Description: "api key client", DeviceSpec: "test"}, network.apiKeySession)
			})
			if *reissued.SessionId != *claims.SessionId {
				t.Fatal("a reissue started another session")
			}
		}
		connect.AssertEqual(t, live, len(sessionCapMembers(t, ctx, network.networkId, "z")))
		// an MCP identity's client is an API key client too
		mcp := session.Testing_CreateClientSession(ctx, session.NewByJwt(network.networkId, network.userId, "session-cap", false, false))
		mcp.UnsignedIdentity = true
		_, mcpClaims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "mcp client", DeviceSpec: "test"}, mcp)
		})
		connect.AssertEqual(t, session.MintKindApiKeyClient, sessionCapKind(t, ctx, network.networkId, *mcpClaims.SessionId))
	})
}

// R-cap: a top-level client's provide state moves its own session in and out
// of the cap's provider exemption, at the provide and at its mints.
func TestProvideChangeMovesTheSessionExemption(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		client, claims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "provider", DeviceSpec: "test"}, network.apiKeySession)
		})
		exempt := func() bool { return sessionCapMembers(t, ctx, network.networkId, "xz")[*claims.SessionId] }
		if exempt() {
			t.Fatal("a client that does not provide is exempt")
		}
		key := []byte{1, 2, 3}
		SetProvide(ctx, client, map[ProvideMode][]byte{ProvideModeStream: key, ProvideModePublic: key})
		if !exempt() {
			t.Fatal("a public provider's session is counted")
		}
		SetProvide(ctx, client, map[ProvideMode][]byte{ProvideModeStream: key})
		if exempt() {
			t.Fatal("return traffic alone exempts a session")
		}
		// the mint records it too
		SetProvide(ctx, client, map[ProvideMode][]byte{ProvideModeStream: key, ProvideModePublic: key})
		server.Raise(session.UpdateSessionProvideExemption(ctx, network.networkId, *claims.SessionId, false))
		sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &client, Description: "provider", DeviceSpec: "test"}, network.apiKeySession)
		})
		if !exempt() {
			t.Fatal("the mint did not record the provider exemption")
		}
	})
}
