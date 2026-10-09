package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestSessionCleanupPreservesReboundAndForeignClients(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		network, user := server.NewId(), server.NewId()
		foreign, foreignUser := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "session-cleanup", user)
		Testing_CreateNetwork(ctx, foreign, "session-cleanup-foreign", foreignUser)
		old := session.NewByJwt(network, user, "session-cleanup", false, false)
		current := session.NewByJwt(network, user, "session-cleanup", false, false)
		for _, claims := range []*session.ByJwt{old, current} {
			if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
				t.Fatal(err)
			}
		}
		rebound, retired, foreignClient := server.NewId(), server.NewId(), server.NewId()
		for _, item := range []struct{ network, client server.Id }{{network, rebound}, {network, retired}, {foreign, foreignClient}} {
			Testing_CreateDevice(ctx, item.network, server.NewId(), item.client, "session-cleanup", "test")
		}
		oldCode, currentCode, foreignCode := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$1 WHERE client_id=ANY($2)`, old.SessionId, []server.Id{rebound, retired, foreignClient}))
			for _, item := range []struct {
				code, network, user server.Id
				origin              *server.Id
			}{{oldCode, network, user, old.SessionId}, {currentCode, network, user, current.SessionId}, {foreignCode, foreign, foreignUser, old.SessionId}} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO auth_code(auth_code_id,network_id,user_id,auth_code,create_time,end_time,uses,remaining_uses,origin_session_id) VALUES($1,$2,$3,$4,$5,$6,1,1,$7)`, item.code, item.network, item.user, item.code.String(), server.NowUtc(), server.NowUtc().Add(time.Hour), item.origin))
			}
		})
		actor := session.NewLocalClientSession(ctx, "192.0.2.1:1", current)
		defer actor.Cancel()
		operation, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *old.SessionId, OperationId: server.NewId()}, actor)
		if err != nil {
			t.Fatal(err)
		}
		// Explicit reauthentication binds one client to the independent login B
		// after A's Redis cutoff and before its delayed SQL cleanup.
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.LockSessionLifecycle(ctx, tx, network, false))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$2,session_create_time=$3 WHERE client_id=$1`, rebound, current.SessionId, current.CreateTime))
		})
		complete, err := CleanupSessionOperation(ctx, session.SessionCleanupOperation{NetworkId: network, OperationId: operation.OperationId, TargetSessionIds: operation.TargetSessionIds})
		if err != nil || !complete {
			t.Fatal("cleanup did not finish", complete, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, item := range []struct {
				client server.Id
				active bool
			}{{rebound, true}, {retired, false}, {foreignClient, true}} {
				var active bool
				server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, item.client).Scan(&active))
				if active != item.active {
					t.Fatal("cleanup lost network/session predicate", item.client, active)
				}
			}
			for _, item := range []struct {
				code   server.Id
				active bool
			}{{oldCode, false}, {currentCode, true}, {foreignCode, true}} {
				var active bool
				var remaining int
				server.Raise(conn.QueryRow(ctx, `SELECT active,remaining_uses FROM auth_code WHERE auth_code_id=$1`, item.code).Scan(&active, &remaining))
				if active != item.active || (!item.active && remaining != 0) {
					t.Fatal("cleanup lost code origin/network predicate or failed generated-active retirement", item.code, active, remaining)
				}
			}
		})
		if !errors.Is(session.CheckSession(ctx, network, *old.SessionId), session.ErrSessionRevoked) {
			t.Fatal("rebind restored revoked session A")
		}
		if err = session.ValidateByJwtState(ctx, current, false); err != nil {
			t.Fatal("cleanup revoked independent login B", err)
		}
		status, err := session.GetSessionOperation(actor, operation.OperationId)
		if err != nil || status.State != "complete" || status.CleanupPending {
			t.Fatal("cleanup checkpoint not committed", status, err)
		}
	})
}
