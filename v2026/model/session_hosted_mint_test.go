package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestHostedSessionConcurrentBootstrapAndRestartKeepWinner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		ctx := t.Context()
		network, user, root, child := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "hosted-session", user)
		Testing_CreateDevice(ctx, network, server.NewId(), root, "root", "test")
		Testing_CreateDevice(ctx, network, server.NewId(), child, "hosted", "test")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id=$2 WHERE client_id=$1`, child, root))
		})
		const workers = 4
		credentials := make([]*session.ByJwt, workers)
		for i := range credentials {
			var err error
			credentials[i], err = session.LoadByJwtFromClientId(ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			// A loader's fabricated fresh lineage must never become the durable
			// historical lineage; concurrent starts use one sampled winner.
			credentials[i].CreateTime = time.Unix(1+int64(i), 0)
		}
		start := make(chan struct{})
		type outcome struct {
			signed string
			err    error
		}
		results := make(chan outcome, workers)
		for _, credential := range credentials {
			go func() {
				<-start
				signed, err := session.MintHostedSession(ctx, credential)
				results <- outcome{signed, err}
			}()
		}
		close(start)
		var winner *session.ByJwt
		for i := 0; i < workers; i++ {
			result := <-results
			if result.err != nil || result.signed == "" {
				t.Fatal("bootstrap failed", result.err)
			}
			claims, err := session.ParseByJwt(ctx, result.signed)
			if err != nil || claims.SessionId == nil || claims.RootClientId == nil || *claims.RootClientId != root || !claims.CreateTime.After(time.Unix(10, 0)) {
				t.Fatal("bootstrap did not mint authoritative lineage/root", claims, err)
			}
			if winner == nil {
				winner = claims
			} else if *claims.SessionId != *winner.SessionId || !claims.CreateTime.Equal(winner.CreateTime) {
				t.Fatal("concurrent bootstrap minted multiple sessions or lineages")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var sid server.Id
			var lineage time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT session_id,session_create_time FROM network_client WHERE client_id=$1`, child).Scan(&sid, &lineage))
			if sid != *winner.SessionId || !lineage.Equal(winner.CreateTime) {
				t.Fatal("winner was not persisted", sid, lineage)
			}
		})
		restarted, err := session.LoadByJwtFromClientId(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		restarted.CreateTime = winner.CreateTime.Add(time.Hour)
		if _, err := session.MintHostedSession(ctx, restarted); err != nil || restarted.SessionId == nil || *restarted.SessionId != *winner.SessionId || !restarted.CreateTime.Equal(winner.CreateTime) || restarted.RootClientId == nil || *restarted.RootClientId != root {
			t.Fatal("restart changed persisted lineage", restarted, err)
		}
		actorClaims := session.NewByJwt(network, user, "hosted-session", false, false)
		if _, err := session.MintNetworkSession(ctx, actorClaims, "password"); err != nil {
			t.Fatal(err)
		}
		actor := session.NewLocalClientSession(ctx, "192.0.2.1:1", actorClaims)
		defer actor.Cancel()
		if _, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *winner.SessionId, OperationId: server.NewId()}, actor); err != nil {
			t.Fatal(err)
		}
		// Deliberately omit SQL cleanup: an active client cannot evade its marker
		// by restarting its hosted loader and inventing another session.
		loaded, err := session.LoadByJwtFromClientId(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if signed, err := session.MintHostedSession(ctx, loaded); signed != "" || !errors.Is(err, session.ErrSessionRevoked) {
			t.Fatal("revoked hosted client bootstrapped another session", err)
		}
	})
}

func TestHostedSessionExplicitRebindFencesStaleRefresh(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		ctx := t.Context()
		network, user, client, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "hosted-rebind", user)
		Testing_CreateDevice(ctx, network, device, client, "hosted", "test")
		old := session.NewByJwt(network, user, "hosted-rebind", false, false).Client(device, client)
		if _, err := session.MintHostedSession(ctx, old); err != nil {
			t.Fatal(err)
		}
		current := session.NewByJwt(network, user, "hosted-rebind", false, false)
		if _, err := session.MintNetworkSession(ctx, current, "password"); err != nil {
			t.Fatal(err)
		}
		current = current.Client(device, client)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.LockSessionLifecycle(ctx, tx, network, false))
			server.Raise(session.AssociateSessionClientInTx(ctx, tx, current, true))
		}, server.TxReadCommitted)
		// Resume an ordinary refresh holding A after the explicit B re-auth has
		// committed. It must fail rather than overwrite B under the row lock.
		var refreshErr error
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.LockSessionLifecycle(ctx, tx, network, false))
			refreshErr = session.AssociateSessionClientInTx(ctx, tx, old, false)
		}, server.TxReadCommitted)
		var conflict *session.SessionError
		if !errors.As(refreshErr, &conflict) || conflict.Status != 409 || conflict.Code != "client_session_changed" {
			t.Fatal("stale refresh was not fenced", refreshErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var sid server.Id
			var lineage time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT session_id,session_create_time FROM network_client WHERE client_id=$1`, client).Scan(&sid, &lineage))
			if sid != *current.SessionId || !lineage.Equal(server.CodecTime(current.CreateTime)) {
				t.Fatal("stale refresh replaced explicit re-auth", sid, lineage)
			}
		})
		loaded, err := session.LoadByJwtFromClientId(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.MintHostedSession(ctx, loaded); err != nil || loaded.SessionId == nil || *loaded.SessionId != *current.SessionId {
			t.Fatal("hosted restart did not preserve explicit re-auth", loaded, err)
		}
	})
}
