package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

func TestSessionMintCommitPrecedesExclusiveRevokeCutoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		target := NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "mint-order", false, false)
		sid := server.NewId()
		target.SessionId = &sid
		registered := make(chan struct{})
		releaseCommit := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseCommit) })
		var workers sync.WaitGroup
		defer func() { release(); cancel(); workers.Wait() }()
		mintResult := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			mintResult <- sessionTx(ctx, func(tx server.PgTx) error {
				if err := LockSessionLifecycle(ctx, tx, target.NetworkId, false); err != nil {
					return err
				}
				if _, err := RegisterAndSignInTx(ctx, tx, target, "password", nil, false); err != nil {
					return err
				}
				close(registered)
				select {
				case <-releaseCommit:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-registered:
		case err := <-mintResult:
			t.Fatal("mint failed before commit barrier", err)
		case <-ctx.Done():
			t.Fatal("mint did not reach commit barrier")
		}
		revokeResult := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := RevokeNetworkSession(&RevokeSessionArgs{SessionId: sid, OperationId: server.NewId()}, actor)
			revokeResult <- err
		}()
		// Observe the real blocked lock request before releasing the mint. No
		// scheduling delay is used to manufacture the transaction interleaving.
		hash := sha256.Sum256(append([]byte("urnetwork:session-lifecycle:v1:"), target.NetworkId[:]...))
		lock := binary.BigEndian.Uint64(hash[:8])
		waiting := false
		for !waiting && ctx.Err() == nil {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=$1::oid AND objid=$2::oid AND NOT granted)`, int64(uint32(lock>>32)), int64(uint32(lock))).Scan(&waiting))
			})
			select {
			case err := <-revokeResult:
				t.Fatal("revoke crossed an uncommitted mint", err)
			default:
			}
		}
		if !waiting {
			t.Fatal("exclusive revoke did not wait for shared mint commit")
		}
		release()
		if err := <-mintResult; err != nil {
			t.Fatal(err)
		}
		if err := <-revokeResult; err != nil {
			t.Fatal(err)
		}
		if err := ValidateByJwtState(ctx, target, false); !errors.Is(err, ErrSessionRevoked) {
			t.Fatal("post-commit cutoff missed the registered mint", err)
		}
		if _, err := MintNetworkSession(ctx, target, "password"); !errors.Is(err, ErrSessionRevoked) {
			t.Fatal("revoked session reminted across the opposite lock order", err)
		}
	})
}

func TestSessionOptionalObservationFailurePreservesAcceptedCredential(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		if err := ValidateByJwtState(actor.Ctx, actor.ByJwt, false); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(actor.Ctx)
		observation := NewLocalClientSession(ctx, "192.0.2.1:1", actor.ByJwt)
		defer observation.Cancel()
		cancel() // The optional write has no remaining budget after acceptance.
		before := testutil.ToFloat64(sessionUseDrops)
		observation.ObserveAuthenticatedUse()
		if after := testutil.ToFloat64(sessionUseDrops); after != before+1 {
			t.Fatal("failed optional observation was not recorded as a bounded drop")
		}
		if err := ValidateByJwtState(actor.Ctx, actor.ByJwt, false); err != nil {
			t.Fatal("optional storage failure invalidated an accepted credential", err)
		}
		list, err := GetNetworkSessions(actor)
		if err != nil || len(list.Sessions) != 1 || list.Sessions[0].LastUsed != nil {
			t.Fatal("failed optional sample fabricated use or removed inventory", list, err)
		}
	})
}
