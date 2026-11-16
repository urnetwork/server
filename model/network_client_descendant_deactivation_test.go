package model

// A client's derived clients go with it. A derived client is retired with its
// parent's own credential (connect's ApiMultiClientGenerator, through
// localclient.Authority or /network/remove-client), and the parent's
// deactivation revokes that credential, so a removed parent used to leave its
// children active until the child reap, a client window after their own last
// auth. A network that churned hosted proxies held hundreds of thousands of
// such windows (2026-11-05), and the retired hosted reap, which reaps retired
// windows only, never saw them.

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The client's live session for its own credential, as a local authority holds
// it for its children (localclient.Authority).
func descendantTestParentSession(ctx context.Context, userSession *session.ClientSession, clientId server.Id, deviceId server.Id) *session.ClientSession {
	claims := session.NewByJwt(
		userSession.ByJwt.NetworkId,
		userSession.ByJwt.UserId,
		userSession.ByJwt.NetworkName,
		false,
		false,
	).Client(deviceId, clientId)
	return session.NewLocalClientSession(ctx, "192.0.2.1:1", claims)
}

// A child of the client, created with the client's own token.
func descendantTestChild(ctx context.Context, t testing.TB, userSession *session.ClientSession, parentClientId server.Id, deviceId server.Id) server.Id {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			SourceClientId: &parentClientId,
			Description:    "window",
			DeviceSpec:     "window",
		},
		clientTokenTestSession(ctx, userSession, parentClientId, deviceId),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, nil)
	connect.AssertNotEqual(t, result.ClientId, nil)
	return *result.ClientId
}

func descendantTestDeactivateTime(ctx context.Context, clientId server.Id) (deactivateTime *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT deactivate_time FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&deactivateTime))
	})
	return
}

func descendantTestActiveChildCount(ctx context.Context, parentClientId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM network_client WHERE source_client_id = $1 AND active = true`,
			parentClientId,
		).Scan(&count))
	})
	return
}

func TestRemoveNetworkClientDeactivatesItsActiveDescendants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		childClientId := descendantTestChild(ctx, t, userSession, parentClientId, deviceId)
		grandchildClientId := descendantTestChild(ctx, t, userSession, childClientId, deviceId)
		otherClientId, otherDeviceId := authClientTestClient(ctx, t, userSession)
		otherChildClientId := descendantTestChild(ctx, t, userSession, otherClientId, otherDeviceId)

		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: parentClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		// the answer counts the removed client, not its descendants
		connect.AssertEqual(t, result.Error, nil)

		for _, clientId := range []server.Id{parentClientId, childClientId, grandchildClientId} {
			if clientTokenTestActive(ctx, clientId) {
				t.Fatalf("client %s is still active after its ancestor was removed", clientId)
			}
		}
		// one deactivation, one clock
		parentDeactivateTime := descendantTestDeactivateTime(ctx, parentClientId)
		connect.AssertNotEqual(t, parentDeactivateTime, nil)
		for _, clientId := range []server.Id{childClientId, grandchildClientId} {
			deactivateTime := descendantTestDeactivateTime(ctx, clientId)
			if deactivateTime == nil || !deactivateTime.Equal(*parentDeactivateTime) {
				t.Fatalf("client %s deactivated at %v, its ancestor at %v", clientId, deactivateTime, *parentDeactivateTime)
			}
		}
		// another client and its child are not touched
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherClientId), true)
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherChildClientId), true)
	})
}

func TestRemoveNetworkClientByItsOwnTokenDeactivatesItsDescendants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		childClientId := descendantTestChild(ctx, t, userSession, parentClientId, deviceId)

		// an app signing out removes its own client
		result, err := RemoveNetworkClient(
			&RemoveNetworkClientArgs{ClientId: parentClientId},
			clientTokenTestSession(ctx, userSession, parentClientId, deviceId),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, clientTokenTestActive(ctx, parentClientId), false)
		connect.AssertEqual(t, clientTokenTestActive(ctx, childClientId), false)
	})
}

// A removed hosted proxy's windows are retired with it, so the retired hosted
// reap deletes them a day later. Before, they stayed active and the reap, which
// reaps retired windows only, passed them by.
func TestRemovedHostedProxyWindowsAreRetiredAndReaped(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		proxyResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "proxy",
				DeviceSpec:  "proxy",
				ProxyConfig: &ProxyConfig{
					InitialDeviceState: &ExtendedProxyDeviceState{
						ProxyDeviceState: ProxyDeviceState{
							Location: &sdk.ConnectLocation{
								ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
							},
						},
					},
				},
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, proxyResult.Error, nil)
		connect.AssertNotEqual(t, proxyResult.ClientId, nil)
		proxyClientId := *proxyResult.ClientId
		var proxyDeviceId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT device_id FROM network_client WHERE client_id = $1`,
				proxyClientId,
			).Scan(&proxyDeviceId))
		})

		// the proxy host mints a window through its local authority
		proxySession := descendantTestParentSession(ctx, userSession, proxyClientId, proxyDeviceId)
		defer proxySession.Cancel()
		windowResult, err := AuthNetworkClientFromParent(
			&AuthNetworkClientArgs{SourceClientId: &proxyClientId, Description: "window", DeviceSpec: "window"},
			proxySession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, windowResult.Error, nil)
		connect.AssertNotEqual(t, windowResult.ClientId, nil)
		windowClientId := *windowResult.ClientId

		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: proxyClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		if clientTokenTestActive(ctx, windowClientId) {
			t.Fatal("the removed proxy's window is still active")
		}

		deactivateTime := descendantTestDeactivateTime(ctx, windowClientId)
		connect.AssertNotEqual(t, deactivateTime, nil)
		removedCount, _, _ := RemoveRetiredHostedNetworkClients(
			ctx,
			deactivateTime.Add(25*time.Hour),
			RetiredHostedNetworkClientCursor{},
			DefaultRetiredHostedNetworkClientReapSettings(),
		)
		if removedCount < 1 {
			t.Fatalf("the retired hosted reap removed %d clients", removedCount)
		}
		_, _, found := authClientTestClientState(ctx, windowClientId)
		connect.AssertEqual(t, found, false)
		_, _, found = authClientTestClientState(ctx, proxyClientId)
		// the proxy keeps the 30-day reap of top-level clients
		connect.AssertEqual(t, found, true)
	})
}

// The network credential could mint a child of a removed client, active from
// birth with no lifecycle left to retire it.
func TestAuthNetworkClientRefusesAChildOfAnInactiveSource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		sourceClientId, _ := authClientTestClient(ctx, t, userSession)
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: sourceClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error, nil)

		clientCountBefore, deviceCountBefore, _ := authClientTestNetworkCounts(ctx, networkId)
		result, err := AuthNetworkClient(
			&AuthNetworkClientArgs{SourceClientId: &sourceClientId, Description: "window", DeviceSpec: "window"},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		if result == nil || result.Error == nil || result.Error.Message != "Client does not exist." || result.ClientId != nil {
			t.Fatalf("a child of a removed client answered %+v, want the refusal", result)
		}
		clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
		if clientCount != clientCountBefore || deviceCount != deviceCountBefore {
			t.Fatalf("the refusal created %d clients and %d devices", clientCount-clientCountBefore, deviceCount-deviceCountBefore)
		}
		connect.AssertEqual(t, descendantTestActiveChildCount(ctx, sourceClientId), 0)
	})
}

// A window mint that validated its parent while the parent's removal was still
// open used to commit a child the removal never saw: the parent inactive, the
// child active. The mint now holds the parent until it commits, so it waits for
// the removal and then is refused.
func TestParentMintRacingParentRemovalLeavesNoActiveChild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		parentSession := descendantTestParentSession(ctx, userSession, parentClientId, deviceId)
		defer parentSession.Cancel()

		// the removal, held open: the parent locked and deactivated, not committed
		holderConn := acquireContractLifecycleTestConnection(t, ctx)
		defer holderConn.Release()
		holder, err := holderConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = holder.Rollback(cleanup)
		}()
		holderPid := contractLifecycleTestBackendPid(t, ctx, holder)
		removedCount, err := deactivateNetworkClientsInTx(ctx, holder, []server.Id{parentClientId}, networkId)
		if err != nil || removedCount != 1 {
			t.Fatalf("held removal: count=%d err=%v", removedCount, err)
		}

		var mintResult *AuthNetworkClientResult
		var mintErr error
		mintDone := make(chan struct{})
		go func() {
			defer close(mintDone)
			mintResult, mintErr = server.HandleError2(func() (*AuthNetworkClientResult, error) {
				return AuthNetworkClientFromParent(
					&AuthNetworkClientArgs{SourceClientId: &parentClientId, Description: "window", DeviceSpec: "window"},
					parentSession,
				)
			}, func(err error) (*AuthNetworkClientResult, error) { return nil, err })
		}()

		// the mint either waits on the held parent or, holding nothing, completes
		mintWaited := false
		for !mintWaited {
			var blockedPid int32
			if err := holder.QueryRow(
				ctx,
				`SELECT COALESCE(min(pid), 0) FROM pg_locks WHERE NOT granted AND $1::int = ANY(pg_blocking_pids(pid))`,
				holderPid,
			).Scan(&blockedPid); err != nil {
				t.Fatal(err)
			}
			if blockedPid != 0 {
				mintWaited = true
				break
			}
			select {
			case <-mintDone:
				t.Fatalf("the mint completed during the parent's open removal: result=%+v err=%v", mintResult, mintErr)
			case <-ctx.Done():
				t.Fatalf("the mint neither waited nor completed: %v", ctx.Err())
			default:
				runtime.Gosched()
			}
		}

		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-mintDone:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if mintResult != nil || !errors.Is(mintErr, ErrClientParentInactive) {
			t.Fatalf("the mint after the removal answered result=%+v err=%v, want ErrClientParentInactive", mintResult, mintErr)
		}
		connect.AssertEqual(t, descendantTestActiveChildCount(ctx, parentClientId), 0)
	})
}
