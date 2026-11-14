package session

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func recoverySessionActor(t testing.TB) *ClientSession {
	t.Helper()
	ctx := t.Context()
	networkId, userId := server.NewId(), server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'session-recovery','password',true)`, userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, networkId, "session-recovery-"+networkId.String(), userId))
	})
	claims := NewByJwt(networkId, userId, "session-recovery", false, false)
	sid := server.NewId()
	claims.SessionId = &sid
	if _, err := RegisterNetworkSession(ctx, claims, "password", nil, false, server.NowUtc()); err != nil {
		t.Fatal(err)
	}
	actor := NewLocalClientSession(ctx, "192.0.2.1:1", claims)
	t.Cleanup(actor.Cancel)
	return actor
}

func TestSessionOperationRecoversAppliedSelfRevokeBeforeActorValidation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		op := server.NewId()
		if err := prepareSessionOperation(actor, op, "single", actor.ByJwt.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		pending, err := GetSessionOperation(actor, op)
		if err != nil || pending.State != "prepared" || pending.Status != "pending" {
			t.Fatal("unapplied request did not report pending", pending, err)
		}
		// Simulate Redis succeeding followed by loss of the SQL result commit.
		applied, err := enforceSessionRevoke(ctx, actor.ByJwt.NetworkId, op, "single", actor.ByJwt.SessionId, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(ValidateByJwtState(ctx, actor.ByJwt, false), ErrSessionRevoked) {
			t.Fatal("self-revoke actor still valid")
		}
		reader := NewLocalClientSession(ctx, "192.0.2.1:1", NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "session-recovery", false, false))
		defer reader.Cancel()
		observed, err := GetSessionOperation(reader, op)
		if err != nil || observed.State != "enforced" || observed.Status != "revoked" || observed.EventId != applied.EventId {
			t.Fatal("applied Redis cutoff falsely reported pending", observed, err)
		}
		recovered, err := EnforceSessionOperation(ctx, actor.ByJwt.NetworkId, op)
		if err != nil || recovered == nil || recovered.State != "enforced" || recovered.RevokedCount != 1 || recovered.EventId != applied.EventId || len(recovered.TargetSessionIds) != 1 || recovered.TargetSessionIds[0] != *actor.ByJwt.SessionId {
			t.Fatal("applied receipt lost to actor revalidation", recovered, err)
		}
		// Exact replay returns the original result rather than another cutoff.
		replayed, err := EnforceSessionOperation(ctx, actor.ByJwt.NetworkId, op)
		if err != nil || replayed.EventId != recovered.EventId || replayed.RevokedCount != 1 {
			t.Fatal("recovery replay changed result", replayed, err)
		}
	})
}

func TestSessionOperationUnappliedInvalidActorCancelsAndReleasesQuota(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		target := sessionFixtureCredential(actor.ByJwt.NetworkId, server.NowUtc())
		if _, err := RegisterNetworkSession(ctx, target, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		op := server.NewId()
		if err := prepareSessionOperation(actor, op, "single", target.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := enforceSessionRevoke(ctx, actor.ByJwt.NetworkId, server.NewId(), "single", actor.ByJwt.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		result, err := EnforceSessionOperation(ctx, actor.ByJwt.NetworkId, op)
		if err != nil || result == nil || result.State != "cancelled" {
			t.Fatal("dead actor remained pending", result, err)
		}
		if err = CheckSession(ctx, actor.ByJwt.NetworkId, *target.SessionId); err != nil {
			t.Fatal("cancelled request revoked its target", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var reserved bool
			server.Raise(conn.QueryRow(ctx, `SELECT quota_reserved FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`, actor.ByJwt.NetworkId, op).Scan(&reserved))
			if reserved {
				t.Fatal("cancelled request retained quota")
			}
		})
	})
}

func TestSessionOperationReplayReservesQuotaOnceAndConflictsDoNotRetarget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		op, different := server.NewId(), server.NewId()
		for i := 0; i < 2; i++ {
			if err := prepareSessionOperation(actor, op, "single", actor.ByJwt.SessionId, nil); err != nil {
				t.Fatal(err)
			}
		}
		err := prepareSessionOperation(actor, op, "single", &different, nil)
		var refusal *SessionError
		if !errors.As(err, &refusal) || refusal.Code != "operation_conflict" {
			t.Fatal("operation id retargeted", err)
		}
		server.Db(actor.Ctx, func(conn server.PgConn) {
			var count int
			var target server.Id
			server.Raise(conn.QueryRow(actor.Ctx, `SELECT count(*) FROM network_session_operation WHERE network_id=$1 AND quota_reserved AND create_time>$2`, actor.ByJwt.NetworkId, server.NowUtc().Add(-24*time.Hour)).Scan(&count))
			server.Raise(conn.QueryRow(actor.Ctx, `SELECT target_session_id FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`, actor.ByJwt.NetworkId, op).Scan(&target))
			if count != 1 || target != *actor.ByJwt.SessionId {
				t.Fatal("replay changed quota or target", count, target)
			}
		})
	})
}

func TestSessionOperationQuotaBoundariesAndNoopRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, test := range []struct {
			action string
			limit  int
		}{{"single", 100}, {"others", 20}} {
			actor := recoverySessionActor(t)
			var target, keep *server.Id
			if test.action == "single" {
				target = actor.ByJwt.SessionId
			} else {
				keep = actor.ByJwt.SessionId
			}
			var first server.Id
			for i := 0; i < test.limit; i++ {
				id := server.NewId()
				if i == 0 {
					first = id
				}
				if err := prepareSessionOperation(actor, id, test.action, target, keep); err != nil {
					t.Fatal(test.action, i, err)
				}
			}
			// Exact replay is free even while the quota is exhausted.
			if err := prepareSessionOperation(actor, first, test.action, target, keep); err != nil {
				t.Fatal("exact replay charged quota", test.action, err)
			}
			err := prepareSessionOperation(actor, server.NewId(), test.action, target, keep)
			var refusal *SessionError
			if !errors.As(err, &refusal) || refusal.Code != "session_quota_exceeded" {
				t.Fatal("quota boundary accepted extra operation", test.action, err)
			}
			if test.action == "others" {
				// Only the kept session exists, so enforcement is a known no-op.
				result, err := EnforceSessionOperation(actor.Ctx, actor.ByJwt.NetworkId, first)
				if err != nil || result.RevokedCount != 0 {
					t.Fatal("bulk no-op failed", result, err)
				}
				if err = prepareSessionOperation(actor, server.NewId(), test.action, target, keep); err != nil {
					t.Fatal("known no-op did not release reservation", err)
				}
			}
		}
	})
}
