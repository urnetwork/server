package model

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// REVOKE-REVIEW R3, regressed by the descendant cascade of 2026-11-05: a
// session's cleanup deactivates the clients bound to it, and the cascade then
// deactivated every active descendant on their devices, including a child a
// network credential of another session had bound to that session. The child
// bound to the other session stays active; an unbound child, and the bound
// parent, end with the revoked session.
func TestSessionCleanupLeavesADescendantBoundToAnotherSession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		network, user, device := server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "session-cleanup-scope", user)
		revoked := session.NewByJwt(network, user, "session-cleanup-scope", false, false)
		other := session.NewByJwt(network, user, "session-cleanup-scope", false, false)
		for _, claims := range []*session.ByJwt{revoked, other} {
			sessionId := server.NewId()
			claims.SessionId = &sessionId
		}
		parent := server.NewId()
		Testing_CreateDevice(ctx, network, device, parent, "session-cleanup-scope", "test")
		// the children are minted after their parent, on its device
		boundElsewhere, unbound := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$2,session_create_time=$3 WHERE client_id=$1`, parent, revoked.SessionId, revoked.CreateTime))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,active,source_client_id,session_id,session_create_time) VALUES($1,$2,$3,'session-cleanup-scope',true,$4,$5,$6)`, boundElsewhere, network, device, parent, other.SessionId, other.CreateTime))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,active,source_client_id) VALUES($1,$2,$3,'session-cleanup-scope',true,$4)`, unbound, network, device, parent))
		})
		complete, err := CleanupSessionOperation(ctx, session.SessionCleanupOperation{NetworkId: network, OperationId: server.NewId(), TargetSessionIds: []server.Id{*revoked.SessionId}})
		if err != nil || !complete {
			t.Fatal("cleanup did not finish", complete, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, item := range []struct {
				name   string
				client server.Id
				active bool
			}{{"the revoked session's client", parent, false}, {"its unbound child", unbound, false}, {"its child bound to another session", boundElsewhere, true}} {
				var active bool
				server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, item.client).Scan(&active))
				if active != item.active {
					t.Errorf("%s: active %v, want %v", item.name, active, item.active)
				}
			}
		})
	})
}
