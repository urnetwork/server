package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promdto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

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

// The post-commit free of a removal runs before the removal's transaction
// returns (server.Tx runs its posts after the commit), so a removal that
// returned has freed what it frees.
func waitSessionFreed(t testing.TB, ctx context.Context, networkId, sessionId server.Id) {
	t.Helper()
	if sessionCapMembers(t, ctx, networkId, "z")[sessionId] {
		t.Fatal("the removed client's session was not freed", sessionId)
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
		if signed, err := session.MintHostedSession(ctx, after); signed != "" || !errors.Is(err, session.ErrByJwtInactive) {
			t.Fatal("a removed proxy bootstrapped another session, or was refused without the inactive credential error", err)
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

// The session cap's exemption is the concurrent client count's (owner
// decision 2026-11-18): for each state a connected client's provider intent
// can be in, ClientCapExemptUntil answers exactly what the count decides of
// the client. Exempt while pending or qualified with a live record and a live
// intent presence; not without intent, failed (normal, over the limit),
// without a presence, or once its presence or record has ended.
func TestClientCapExemptUntilIsTheConcurrentCountsExemption(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetProviderIntentShadowSampling(true)()
		ctx := t.Context()
		now := server.NowUtc()
		presence := 5 * time.Minute
		state := func(status ProviderIntentStatus) *ProviderIntentState {
			return &ProviderIntentState{Status: status, AttemptTime: now, GraceEndTime: now.Add(providerIntentGraceTimeout), CheckTime: now.Add(providerIntentRecheckTimeout)}
		}
		recordEnd := state(ProviderIntentStatusQualified).expireTime(now)
		for _, c := range []struct {
			name     string
			state    *ProviderIntentState
			presence time.Duration
			// the presence set directly past the record's end
			lastingPresence bool
			at              time.Time
			exempt          bool
		}{
			{name: "no intent", at: now},
			{name: "pending with its intent connection", state: state(ProviderIntentStatusPending), presence: presence, at: now, exempt: true},
			{name: "qualified with its intent connection", state: state(ProviderIntentStatusQualified), presence: presence, at: now, exempt: true},
			{name: "failed, in a normal slot", state: state(ProviderIntentStatusNormal), presence: presence, at: now},
			{name: "failed, over the limit", state: state(ProviderIntentStatusOverLimit), presence: presence, at: now},
			{name: "qualified, no intent connection", state: state(ProviderIntentStatusQualified), at: now},
			{name: "qualified, at its presence's last millisecond", state: state(ProviderIntentStatusQualified), presence: presence, at: now.Add(presence - 2*time.Millisecond), exempt: true},
			{name: "qualified, its presence ended", state: state(ProviderIntentStatusQualified), presence: presence, at: now.Add(presence)},
			{name: "qualified, its record ended", state: state(ProviderIntentStatusQualified), presence: presence, lastingPresence: true, at: recordEnd.Add(time.Second)},
		} {
			networkId := testingProviderIntentNetwork(ctx)
			clientId := testingProviderIntentConnectedClient(ctx, networkId)
			if c.state != nil {
				testingSetProviderIntentState(t, ctx, networkId, clientId, c.state, c.presence, now)
			}
			if c.lastingPresence {
				server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
					return r.ZAdd(ctx, providerIntentPresentKey(networkId), redis.Z{Score: float64(recordEnd.Add(time.Hour).UnixMilli()), Member: string(clientId.Bytes())}).Err()
				}))
			}
			_, countedExempt := getNetworkConnectedClientCountsAt(ctx, networkId, c.at)
			exemptUntil, err := ClientCapExemptUntil(ctx, networkId, clientId, c.at)
			if err != nil {
				t.Fatal(c.name, err)
			}
			if (countedExempt == 1) != c.exempt || exemptUntil.IsZero() == c.exempt {
				t.Fatalf("%s: the count's exempt clients %d, ClientCapExemptUntil %v, want exempt %t", c.name, countedExempt, exemptUntil, c.exempt)
			}
		}
	})
}

// Owner decision 2026-11-18 (~23:50Z): the session cap exempts exactly the
// clients the concurrent client limit exempts, a top-level client that
// declared provide intent while its qualification (by a Public provide key) is
// pending or qualified and its intent connection is live. A client with only a
// Stream key (every connected app), a hosted proxy, a network-only or
// friends-and-family provider, and a client with a Public key but no declared
// intent all count; the declared, qualified provider is exempt from both caps.
func TestOnlyIntentProvidersAreExemptFromTheSessionCap(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		defer Testing_SetProviderIntentShadowSampling(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		Testing_ClearNetworkPeersEnabledCache()
		key := []byte{1, 2, 3}
		keyClient := func(modes map[ProvideMode][]byte) (server.Id, server.Id) {
			clientId, claims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
				return AuthNetworkClient(&AuthNetworkClientArgs{Description: "client", DeviceSpec: "test"}, network.apiKeySession)
			})
			SetProvide(ctx, clientId, modes)
			return clientId, *claims.SessionId
		}
		counted := map[string]server.Id{}
		_, counted["a connected app with only a stream key"] = keyClient(map[ProvideMode][]byte{ProvideModeStream: key})
		networkOnly, networkOnlySession := keyClient(map[ProvideMode][]byte{ProvideModeNetwork: key, ProvideModeStream: key})
		counted["a network-only provider"] = networkOnlySession
		_, counted["a friends-and-family provider"] = keyClient(map[ProvideMode][]byte{ProvideModeFriendsAndFamily: key, ProvideModeStream: key})
		_, counted["a public provide key without declared intent"] = keyClient(map[ProvideMode][]byte{ProvideModePublic: key, ProvideModeStream: key})
		proxy := testAuthProxyClient(t, network.signin)
		if proxy.ClientId == nil || proxy.ByClientJwt == nil {
			t.Fatalf("hosted proxy: %+v", proxy)
		}
		SetProvide(ctx, *proxy.ClientId, map[ProvideMode][]byte{ProvideModeStream: key})
		proxyClaims, err := session.ParseByJwt(ctx, *proxy.ByClientJwt)
		if err != nil || proxyClaims.SessionId == nil {
			t.Fatal("the hosted proxy has no session of its own", err)
		}
		counted["a hosted proxy"] = *proxyClaims.SessionId
		// declared intent on its connection, a Public key, qualified
		provider, providerSession := keyClient(map[ProvideMode][]byte{ProvideModePublic: key, ProvideModeStream: key})
		now := server.NowUtc()
		testingSetProviderIntentState(t, ctx, network.networkId, provider, &ProviderIntentState{
			Status:      ProviderIntentStatusQualified,
			AttemptTime: now,
			CheckTime:   now.Add(providerIntentRecheckTimeout),
		}, 5*time.Minute, now)

		exempt := sessionCapMembers(t, ctx, network.networkId, "xz")
		for name, sessionId := range counted {
			if exempt[sessionId] {
				t.Errorf("%s is exempt from the session cap", name)
			}
		}
		if !exempt[providerSession] || len(exempt) != 1 {
			t.Fatal("the session cap's exempt sessions", exempt, "want only the qualified provider's", providerSession)
		}
		// the cap: the sign-in, the five counted clients, and one more
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: 1 + len(counted) + 1, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		if _, err := session.MintNetworkSession(ctx, session.NewByJwt(network.networkId, network.userId, "session-cap", false, false), "password"); err != nil {
			t.Fatal("the qualified provider took a place under the session cap", err)
		}

		// the concurrent client limit decides the same of the provider and of
		// a counted client, connected
		for _, clientId := range []server.Id{provider, networkOnly} {
			AddNetworkPeer(ctx, network.networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), 24*time.Hour)
		}
		normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, network.networkId, server.NowUtc())
		if normalCount != 1 || exemptCount != 1 {
			t.Fatal("the concurrent client count", normalCount, exemptCount, "want the provider exempt and the network-only client counted")
		}
	})
}

// A provider install, a top-level client created with declared provide
// intent, skips the client limits' creation gates; its own session is exempt
// from the session cap from its creation too, so a provider fleet at an
// enforced cap still installs providers. Any other new client is refused.
func TestProviderInstallIsExemptFromItsCreation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		live := len(sessionCapMembers(t, ctx, network.networkId, "z"))
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: live, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		_, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "client", DeviceSpec: "test"}, network.apiKeySession)
		var refusal *session.SessionError
		if !errors.As(err, &refusal) || refusal.Code != "session_limit_reached" {
			t.Fatal("an ordinary client at the cap", err)
		}
		_, claims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "provider", DeviceSpec: "test", ProvideIntent: true}, network.apiKeySession)
		})
		if !sessionCapMembers(t, ctx, network.networkId, "xz")[*claims.SessionId] {
			t.Fatal("a provider install's session is counted")
		}
	})
}

// The session cap follows a client's intent state as the concurrent client
// count does: each write of the state records the exemption on the client's
// own session, ending when its intent presence or record ends, and a presence
// refresh renews it before it lapses.
func TestIntentStateMovesTheSessionExemption(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer Testing_SetProviderIntentShadowSampling(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		client, claims := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "provider", DeviceSpec: "test"}, network.apiKeySession)
		})
		exemptEnd := func() int64 {
			var end float64
			server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
				var err error
				end, err = r.ZScore(ctx, session.SessionKey(network.networkId, "xz"), claims.SessionId.String()).Result()
				if errors.Is(err, server.RedisNil) {
					return nil
				}
				return err
			}))
			return int64(end)
		}
		if exemptEnd() != 0 {
			t.Fatal("a client without intent is exempt")
		}
		now := server.NowUtc()
		presence := 2 * time.Minute
		// an intent connection: pending, exempt until its presence ends
		if result := connectProviderIntentAt(ctx, network.networkId, client, presence, now); result.State.Status != ProviderIntentStatusPending {
			t.Fatal("the intent connection's state", result.State)
		}
		if end := exemptEnd(); end != now.Add(presence).UnixMilli() {
			t.Fatal("a pending provider is exempt until", end, "not its presence's end", now.Add(presence).UnixMilli())
		}
		testingSetProviderIntentState(t, ctx, network.networkId, client, &ProviderIntentState{
			Status:      ProviderIntentStatusQualified,
			AttemptTime: now,
			CheckTime:   now.Add(providerIntentRecheckTimeout),
		}, presence, now)
		if exemptEnd() == 0 {
			t.Fatal("a qualified provider is counted")
		}
		// a presence refresh renews the recorded end before it lapses
		later := now.Add(time.Minute)
		if observed := observeProviderIntentAt(ctx, network.networkId, client, presence, later); observed == nil || observed.Status != ProviderIntentStatusQualified {
			t.Fatal("the presence refresh", observed)
		}
		if end := exemptEnd(); end != later.Add(presence).UnixMilli() {
			t.Fatal("a refreshed provider is exempt until", end, "not its presence's end", later.Add(presence).UnixMilli())
		}
		// a failed qualification counts
		testingSetProviderIntentState(t, ctx, network.networkId, client, &ProviderIntentState{
			Status:      ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(ProviderIntentAttemptAllowance),
		}, presence, now)
		if exemptEnd() != 0 {
			t.Fatal("a failed provider is exempt")
		}
	})
}

// S2 R3: a session's cleanup deactivates the clients bound to it and their
// descendants, but spares a descendant an explicit re-auth bound to another
// session, whose credentials that session carries. The cascade added on
// 2026-11-05 deactivated it whatever its session.
func TestSessionCleanupSparesADescendantBoundToAnotherSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		parent, _ := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"}, network.signin)
		})
		child, _ := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: &parent, Description: "child", DeviceSpec: "test"}, network.signin)
		})
		// another sign-in re-authenticates the child: an explicit rebind to B
		claimsB := session.NewByJwt(network.networkId, network.userId, "session-cap", false, false)
		if _, err := session.MintNetworkSession(ctx, claimsB, "password"); err != nil {
			t.Fatal(err)
		}
		signinB := session.Testing_CreateClientSession(ctx, claimsB)
		_, childB := sessionCapTestClient(t, ctx, func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &child, Description: "child", DeviceSpec: "test"}, signinB)
		})
		if *childB.SessionId != *claimsB.SessionId {
			t.Fatal("the re-auth did not bind the child to B")
		}
		result, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *network.signin.ByJwt.SessionId, OperationId: server.NewId()}, signinB)
		if err != nil || result.State != "enforced" {
			t.Fatal(result, err)
		}
		if _, err := CleanupSessionOperation(ctx, session.SessionCleanupOperation{NetworkId: network.networkId, OperationId: result.OperationId, TargetSessionIds: result.TargetSessionIds}); err != nil {
			t.Fatal(err)
		}
		active := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT client_id, active FROM network_client WHERE client_id = ANY($1)`, []server.Id{parent, child})
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					var isActive bool
					server.Raise(rows.Scan(&clientId, &isActive))
					active[clientId] = isActive
				}
			})
		})
		if active[parent] {
			t.Fatal("the cleanup left the revoked session's client active")
		}
		if !active[child] {
			t.Fatal("the cleanup deactivated a descendant bound to another session")
		}
		if err := session.ValidateByJwtState(ctx, childB, true); err != nil {
			t.Fatal("the descendant's credential under B no longer validates", err)
		}
	})
}

// S2 N1: recovery reads the 32 oldest pending operations; one it cannot
// advance moves behind the others, so 33 stuck operations (here, enforced
// with their receipt and markers lost) no longer starve a 34th.
func TestSessionRecoveryRotatesStuckOperations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newSessionCapTestNetwork(t, ctx)
		target := session.NewByJwt(network.networkId, network.userId, "session-cap", false, false)
		if _, err := session.MintNetworkSession(ctx, target, "password"); err != nil {
			t.Fatal(err)
		}
		old := server.NowUtc().Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			for i := 0; i < 33; i++ {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_session_operation(network_id,operation_id,action,fingerprint,actor,credential_epoch,status,result,target_session_ids,quota_reserved,create_time,update_time,retain_until) VALUES($1,$2,'single','stuck','{}',$3,'enforced','{}',$4,false,$3,$3,$5)`, server.NewId(), server.NewId(), old.Add(time.Duration(i)*time.Second), `["`+server.NewId().String()+`"]`, server.NowUtc().Add(24*time.Hour)))
			}
		})
		// a valid single revoke, prepared after the stuck ones
		op := server.NewId()
		actor, err := json.Marshal(network.signin.ByJwt)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			var epoch time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT credential_change_time FROM network_user WHERE user_id=$1`, network.userId).Scan(&epoch))
			now := server.NowUtc()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_session_operation(network_id,operation_id,action,fingerprint,actor,credential_epoch,target_session_id,status,create_time,update_time,retain_until) VALUES($1,$2,'single','valid',$3,$4,$5,'prepared',$6,$6,$7)`, network.networkId, op, actor, epoch, *target.SessionId, now, now.Add(24*time.Hour)))
		})
		for range 2 {
			_ = RecoverSessionOperations(ctx, 32)
		}
		if !errors.Is(session.CheckSession(ctx, network.networkId, *target.SessionId), session.ErrSessionRevoked) {
			t.Fatal("stuck operations starved a later one")
		}
	})
}

// R-reset (S2 N9): a password reset retires the sign-ins but not a hosted
// proxy's own session, and the proxy keeps minting after it.
func TestHostedProxySurvivesPasswordResetAndKeepsMinting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network, user := server.NewId(), server.NewId()
		userAuth := Testing_CreateNetwork(ctx, network, "reset-proxy", user)
		signin := session.NewByJwt(network, user, "reset-proxy", false, false)
		if _, err := session.MintNetworkSession(ctx, signin, "password"); err != nil {
			t.Fatal(err)
		}
		proxy := server.NewId()
		Testing_CreateDevice(ctx, network, server.NewId(), proxy, "hosted", "test")
		loaded, err := session.LoadByJwtFromClientId(ctx, proxy)
		server.Raise(err)
		if _, err := session.MintHostedSession(ctx, loaded); err != nil {
			t.Fatal(err)
		}
		actor := session.Testing_CreateClientSession(ctx, session.NewByJwt(network, user, "reset-proxy", false, false))
		defer actor.Cancel()
		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, actor)
		if err != nil || reset.ResetCode == nil {
			t.Fatal(reset, err)
		}
		result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: "new-proxy-password"}, actor)
		if err != nil || result == nil || result.Error != nil {
			t.Fatal(result, err)
		}
		if err := RecoverSessionOperations(ctx, 32); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(session.CheckSession(ctx, network, *signin.SessionId), session.ErrSessionRevoked) {
			t.Fatal("the reset spared the sign-in")
		}
		if err := session.CheckSession(ctx, network, *loaded.SessionId); err != nil {
			t.Fatal("the reset retired the hosted proxy's session", err)
		}
		again, err := session.LoadByJwtFromClientId(ctx, proxy)
		server.Raise(err)
		if _, err := session.MintHostedSession(ctx, again); err != nil {
			t.Fatal("the proxy could not mint after the reset", err)
		}
		if *again.SessionId != *loaded.SessionId {
			t.Fatal("the proxy's session changed")
		}
		if err := session.ValidateByJwtState(ctx, again, true); err != nil {
			t.Fatal("the proxy's credential after the reset does not validate", err)
		}
	})
}

// S1: auth-code creation re-registers its creator's session (kind restored
// when the store no longer holds it). It skipped the mint clock check and
// never published the restored member to the maintenance index, so the
// network could hold a live set no review or level sample would find.
func TestAuthCodeCreateRestoredSessionIsPublished(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		creator := authCodeSessionCreator(t)
		network := creator.ByJwt.NetworkId
		// the store lost the member and the index entry (as after their keys expired)
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			for shard := 0; shard < session.SessionIndexShards; shard++ {
				if err := r.ZRem(ctx, fmt.Sprintf("{nsi_%02d}z", shard), network.String()).Err(); err != nil {
					return err
				}
			}
			return r.Del(ctx, session.SessionKey(network, "z"), session.SessionKey(network, "held"), session.SessionKey(network, "zi")).Err()
		}))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_session_index_outbox WHERE network_id=$1`, network))
		})
		authCodeSessionCode(t, creator)
		if !sessionCapMembers(t, ctx, network, "z")[*creator.ByJwt.SessionId] {
			t.Fatal("code creation did not restore the creator's session")
		}
		published := false
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			for shard := 0; shard < session.SessionIndexShards; shard++ {
				if _, err := r.ZScore(ctx, fmt.Sprintf("{nsi_%02d}z", shard), network.String()).Result(); err == nil {
					published = true
				}
			}
			return nil
		}))
		var queued int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_session_index_outbox WHERE network_id=$1`, network).Scan(&queued))
		})
		if !published && queued == 0 {
			t.Fatal("the restored session's network is neither in the index nor queued for it")
		}
	})
}

// R-refuse (session1118 D F1): an SSO sign-in at an enforced session cap
// returns the cap's refusal, which the router answers 409
// session_limit_reached. Its mint raised the refusal, which escaped the route
// as a 500.
func TestSsoSignInReturnsTheSessionCapRefusal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: 1000, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		network, user := server.NewId(), server.NewId()
		userAuth := Testing_CreateNetwork(ctx, network, "sso-cap", user)
		members := make([]redis.Z, 1000)
		for i := range members {
			members[i] = redis.Z{Score: float64(server.NowUtc().AddDate(0, 0, 30).UnixMilli()), Member: server.NewId().String()}
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.ZAdd(ctx, session.SessionKey(network, "z"), members...).Err()
		}))
		result, err := handleLoginParsedAuthJwt(&HandleLoginParsedAuthJwtArgs{
			AuthJwt:           AuthJwt{AuthType: SsoAuthTypeGoogle, UserAuth: userAuth},
			UserAuthAttemptId: UserAuthAttemptId{},
		}, ctx)
		var refusal *session.SessionError
		if !errors.As(err, &refusal) || refusal.Code != "session_limit_reached" || result != nil {
			t.Fatal("the SSO sign-in at the cap", result, err)
		}
	})
}

// Adds count live sessions no caller knows to the network's live set, and
// returns them.
func fillLiveSessions(t testing.TB, ctx context.Context, networkId server.Id, count int) []string {
	t.Helper()
	members := make([]redis.Z, count)
	ids := make([]string, count)
	for i := range members {
		ids[i] = server.NewId().String()
		members[i] = redis.Z{Score: float64(server.NowUtc().AddDate(0, 0, 30).UnixMilli()), Member: ids[i]}
	}
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.ZAdd(ctx, session.SessionKey(networkId, "z"), members...).Err()
	}))
	return ids
}

func liveSessionCount(t testing.TB, ctx context.Context, networkId server.Id) int64 {
	t.Helper()
	var live int64
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		live, err = r.ZCard(ctx, session.SessionKey(networkId, "z")).Result()
		return err
	}))
	return live
}

// session1118 C: a signed-in caller that verifies a sign-in method of its own
// network (an email or phone added in Account) keeps its session. The verify
// started another session, which no app installs: an unused "Verification
// code" sign-in counted toward the cap, and at an enforced cap the step was
// refused.
func TestSignedInVerifyKeepsTheCallersSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: 1000, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		network, user := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "verify-own", user)
		signed, err := session.MintNetworkSession(ctx, session.NewByJwt(network, user, "verify-own", false, false), "password")
		server.Raise(err)
		caller, err := session.ParseByJwt(ctx, signed)
		server.Raise(err)
		// a phone added to the email account (one email per account)
		added := "+1 609-737-0021"
		addUnverifiedUserAuth(t, ctx, user, added)
		normalAdded, _ := NormalUserAuthV1(&added)
		// with the caller's own: the cap
		fillLiveSessions(t, ctx, network, 999)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		code, err := AuthVerifyCreateCode(AuthVerifyCreateCodeArgs{UserAuth: added}, clientSession)
		server.Raise(err)
		clientSession.Header["Authorization"] = []string{"Bearer " + signed}
		result, err := AuthVerify(AuthVerifyArgs{UserAuth: added, VerifyCode: *code.VerifyCode}, clientSession)
		if err != nil || result == nil || result.Error != nil || result.Network == nil {
			t.Fatal("the signed-in verify at the cap", result, err)
		}
		returned, err := session.ParseByJwt(ctx, result.Network.ByJwt)
		server.Raise(err)
		if returned.SessionId == nil || caller.SessionId == nil || *returned.SessionId != *caller.SessionId {
			t.Fatal("the verify started another session", returned.SessionId, caller.SessionId)
		}
		if live := liveSessionCount(t, ctx, network); live != 1000 {
			t.Fatal("live sessions after the verify", live)
		}
		var verified bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT verified FROM network_user_auth_password WHERE user_id=$1 AND user_auth=$2`, user, *normalAdded).Scan(&verified))
		})
		if !verified {
			t.Fatal("the added sign-in method was not verified")
		}
	})
}

// session1118 C: a verify the session cap refuses commits nothing of the
// verification. It committed before the session was refused: the step
// reported a refusal for a verified sign-in method, and the retry answered
// "Invalid code."
func TestVerifyRefusedAtTheCapLeavesTheCodeUnused(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: 1000, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		network, user := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "verify-cap", user)
		// a phone added to the email account (one email per account)
		added := "+1 609-737-0022"
		addUnverifiedUserAuth(t, ctx, user, added)
		normalAdded, _ := NormalUserAuthV1(&added)
		members := fillLiveSessions(t, ctx, network, 1000)
		code, err := AuthVerifyCreateCode(AuthVerifyCreateCodeArgs{UserAuth: added}, session.Testing_CreateClientSession(ctx, nil))
		server.Raise(err)
		args := AuthVerifyArgs{UserAuth: added, VerifyCode: *code.VerifyCode}
		result, err := AuthVerify(args, session.Testing_CreateClientSession(ctx, nil))
		var refusal *session.SessionError
		if !errors.As(err, &refusal) || refusal.Code != "session_limit_reached" || result != nil {
			t.Fatal("the verify at the cap", result, err)
		}
		var used int
		var verified bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM user_auth_verify WHERE user_id=$1 AND used`, user).Scan(&used))
			server.Raise(conn.QueryRow(ctx, `SELECT verified FROM network_user_auth_password WHERE user_id=$1 AND user_auth=$2`, user, *normalAdded).Scan(&verified))
		})
		if used != 0 || verified {
			t.Fatal("the refused verify committed its verification", used, verified)
		}
		// one session signed out: the same code verifies
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.ZRem(ctx, session.SessionKey(network, "z"), members[0]).Err()
		}))
		result, err = AuthVerify(args, session.Testing_CreateClientSession(ctx, nil))
		if err != nil || result == nil || result.Error != nil || result.Network == nil {
			t.Fatal("the verify after a session was signed out", result, err)
		}
	})
}

// session1118 C: an SSO sign-in the session cap refuses links no new provider
// to the account (nor notices it of one). The link committed before the
// session was refused.
func TestSsoSignInRefusedAtTheCapLinksNoProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetSessionCreationEnabled(true)()
		defer session.Testing_SetSessionCapPolicy(session.SessionCapPolicy{Live: 1000, Retained: session.NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		network, user := server.NewId(), server.NewId()
		userAuth := Testing_CreateNetwork(ctx, network, "sso-link", user)
		fillLiveSessions(t, ctx, network, 1000)
		err := func() (err error) {
			// a refusal raised out of the sign-in is the same refusal here
			defer func() {
				if recovered := recover(); recovered != nil {
					err, _ = recovered.(error)
				}
			}()
			_, err = handleLoginParsedAuthJwt(&HandleLoginParsedAuthJwtArgs{
				AuthJwt:           AuthJwt{AuthType: SsoAuthTypeGoogle, UserAuth: userAuth},
				UserAuthAttemptId: UserAuthAttemptId{},
			}, ctx)
			return
		}()
		var refusal *session.SessionError
		if !errors.As(err, &refusal) || refusal.Code != "session_limit_reached" {
			t.Fatal("the SSO sign-in at the cap", err)
		}
		var links int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_user_auth_sso WHERE user_id=$1`, user).Scan(&links))
		})
		if links != 0 {
			t.Fatal("the refused sign-in linked the provider", links)
		}
	})
}
