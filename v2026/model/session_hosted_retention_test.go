package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestHostedSessionCannotOutliveDelayedRevocationCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		ctx := t.Context()
		for _, bulk := range []bool{false, true} {
			network, user, client, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			name := "hosted-retention-" + network.String()
			Testing_CreateNetwork(ctx, network, name, user)
			Testing_CreateDevice(ctx, network, device, client, "hosted", "test")
			hosted := session.NewByJwt(network, user, name, false, false).Client(device, client)
			if _, err := session.MintHostedSession(ctx, hosted); err != nil {
				t.Fatal(err)
			}
			claims := session.NewByJwt(network, user, name, false, false)
			if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
				t.Fatal(err)
			}
			actor := session.NewLocalClientSession(ctx, "192.0.2.1:1", claims)
			defer actor.Cancel()
			operationId := server.NewId()
			var err error
			if bulk {
				_, err = session.RevokeOtherNetworkSessions(&session.RevokeOtherSessionsArgs{OperationId: operationId}, actor)
			} else {
				_, err = session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *hosted.SessionId, OperationId: operationId}, actor)
			}
			if err != nil {
				t.Fatal(err)
			}
			// Simulate passage beyond Redis retention while deliberately retaining
			// the active SQL row and unfinished operation. Natural key expiration
			// must not become permission for a loader with a fabricated fresh exp.
			server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
				return r.Del(ctx, session.SessionMarkerKey(network, *hosted.SessionId), session.SessionKey(network, "op:"+operationId.String())).Err()
			}))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_session_operation SET retain_until=$3 WHERE network_id=$1 AND operation_id=$2`, network, operationId, server.NowUtc().Add(-time.Hour)))
			})
			if err := session.ExpireSessionReceipts(ctx, server.NowUtc(), 128); err != nil {
				t.Fatal(err)
			}
			loaded, err := session.LoadByJwtFromClientId(ctx, client)
			if err != nil {
				t.Fatal(err)
			}
			if token, err := session.MintHostedSession(ctx, loaded); token != "" || !errors.Is(err, session.ErrSessionRevoked) {
				t.Fatalf("bulk=%v: hosted restart revived retired binding: %v", bulk, err)
			}
			// A Redis-applied/SQL-uncommitted operation has no durable target set.
			// Losing its receipt leaves an unknown outcome, never a new mint.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_session_operation SET status='prepared',target_session_ids='[]',result=NULL WHERE network_id=$1 AND operation_id=$2`, network, operationId))
			})
			if token, err := session.MintHostedSession(ctx, loaded); token != "" || !errors.Is(err, session.ErrAuthUnavailable) || errors.Is(err, session.ErrSessionRevoked) {
				t.Fatalf("bulk=%v: unknown prepared outcome did not fail unavailable: %v", bulk, err)
			}
		}
	})
}
