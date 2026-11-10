//go:build sidecar_pending

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// GET /auth/refresh's liveness for an HTTPS-only client (plan-SDK-http-only-idle-reap, QD-224;
// SDK-M4-08): a strict_exp bearer's refresh moves its stale auth_time, so the idle sweep keeps a page
// Worker that never connects, while an unmarked client's refresh leaves auth_time as before, and a
// refresh that answers restore_required moves nothing.
func TestStrictExpRefreshMovesAuthTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		model.Testing_ResetClientAuthTimeGate()
		defer model.Testing_ResetClientAuthTimeGate()
		clientSession, _ := newSidecarBindTestClient(t, ctx, "refresh-auth-time")
		clientId := *clientSession.ByJwt.ClientId
		strictByJwt := clientSession.ByJwt.Renew()
		strictByJwt.SetStrictExp(true)
		strictExpSession := session.Testing_CreateClientSession(ctx, strictByJwt)

		authTime := func() (authTime time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT auth_time FROM network_client WHERE client_id = $1`, clientId).Scan(&authTime))
			})
			return
		}
		stale := server.NowUtc().Add(-2 * time.Hour)
		setStale := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time = $2 WHERE client_id = $1`, clientId, stale))
			})
			model.Testing_ResetClientAuthTimeGate()
		}

		// unbound: restore_required, no refresh, nothing moved
		setStale()
		if result, err := RefreshToken(strictExpSession); err != nil || result.Error == nil || result.Error.Code != RefreshTokenCodeRestoreRequired {
			t.Fatalf("unbound: %v %+v", err, result)
		}
		if !authTime().Equal(stale) {
			t.Fatal("a refresh that renewed nothing moved auth_time")
		}

		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession); err != nil {
			t.Fatal(err)
		}
		// an unmarked client's refresh leaves auth_time, as ever
		setStale()
		if result, err := RefreshToken(clientSession); err != nil || result.Error != nil || result.ByJwt == "" {
			t.Fatalf("unmarked: %v %+v", err, result)
		}
		if !authTime().Equal(stale) {
			t.Fatal("an unmarked client's refresh moved auth_time")
		}
		// a strict_exp bearer's moves it
		if result, err := RefreshToken(strictExpSession); err != nil || result.Error != nil || result.ByJwt == "" {
			t.Fatalf("strict_exp: %v %+v", err, result)
		}
		if moved := authTime(); !stale.Before(moved) || server.NowUtc().Sub(moved) > time.Minute {
			t.Fatalf("a strict_exp refresh left auth_time at %s", moved)
		}
	})
}
