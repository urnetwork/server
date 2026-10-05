// Actual SQL races and rolling insert writers must choose one durable outcome.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Real original wire and server signatures share an explicit synthetic scope.
func verifyRequestClosureFixture(t testing.TB) (*VerifyOriginalTransition, protocol.ProviderAttemptRequestClosure, *protocol.ProviderAttemptClosedUnreceived, map[byte]ed25519.PublicKey) {
	t.Helper()
	original, key := testVerifySignedOriginal(t)
	body, err := DecodeVerifyOriginal(original)
	if err != nil {
		t.Fatal(err)
	}
	scope := protocol.ProviderAttemptReceiptScope{Profile: "testnet", GenesisHash: [32]byte{1}, DeploymentId: "closure-test", DeploymentKey: "945:synthetic-closure", PolicyHash: [32]byte{2}, Netuid: 521, NoId: 1}
	closure, err := protocol.SealProviderAttemptRequestClosure(t.Context(), protocol.ProviderAttemptRequestClosure{Schema: protocol.ProviderAttemptRequestCloseDomain, Scope: scope, ClientId: connect.Id(body.Trail.ClientId), Message: body.RequestMessage, RequestSignature: body.RequestSignature, CutHash: [32]byte{3}, Epoch: 7, EndBlock: 20}, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	body.Scope = VerifyRequestClosureLocator(*closure).Scope
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	original = &VerifyOriginalTransition{Body: raw, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), raw...))}
	receipt, err := protocol.SealProviderAttemptClosedUnreceived(t.Context(), *closure, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	return original, *closure, receipt, map[byte]ed25519.PublicKey{7: key.Public().(ed25519.PublicKey)}
}

// A tombstone refuses the real retention path and direct rolling-writer insert.
func TestVerifyRequestClosureFencesAssignmentAndRollingWriter(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		original, closure, receipt, keys := verifyRequestClosureFixture(t)
		first := CloseVerifyOriginalRequest(t.Context(), closure, receipt, keys)
		if first.Original != nil || first.ClosedUnreceived == nil {
			t.Fatal("closure did not retain a permanent unreceived fence")
		}
		for index := 0; index < 2; index++ {
			result := CloseVerifyOriginalRequest(t.Context(), closure, receipt, keys)
			if result.Original != nil || result.ClosedUnreceived == nil || !bytes.Equal(result.ClosedUnreceived.Signature, first.ClosedUnreceived.Signature) {
				t.Fatal("closure retry changed exact original receipt")
			}
		}
		recovered := server.HandleError(func() { RetainVerifyOriginal(t.Context(), original) })
		cause, ok := recovered.(error)
		if !ok || !errors.Is(cause, ErrVerifyRequestClosed) {
			t.Fatal("closed request acquired a late assignment", recovered)
		}
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		recovered = server.HandleError(func() {
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO verify_original_transition(trail_id,previous_depth,observed_time,original_body,original_signature) VALUES($1,$2,$3,$4,$5)`, body.Trail.TrailId, body.PreviousDepth, server.NowUtc(), original.Body, original.Signature))
			})
		})
		if recovered == nil {
			t.Fatal("rolling writer bypassed permanent request fence")
		}
		for _, sql := range []string{`UPDATE verify_original_request_closed SET receipt_body=receipt_body`, `DELETE FROM verify_original_request_closed`, `TRUNCATE verify_original_request_closed`} {
			if server.HandleError(func() {
				server.Tx(t.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(t.Context(), sql)) })
			}) == nil {
				t.Fatal("immutable closure custody accepted mutation", sql)
			}
		}
	})
}

// An actual received assignment cannot become a signed no-exposure assertion.
func TestVerifyRequestClosureReceivedOriginalWins(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		original, closure, receipt, keys := verifyRequestClosureFixture(t)
		RetainVerifyOriginal(t.Context(), original)
		result := CloseVerifyOriginalRequest(t.Context(), closure, receipt, keys)
		if result.Original == nil || result.ClosedUnreceived != nil || !bytes.Equal(result.Original.Body, original.Body) || !bytes.Equal(result.Original.Signature, original.Signature) {
			t.Fatal("received request became unreceived", result)
		}
		var count int
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM verify_original_request_closed`).Scan(&count))
		})
		if count != 0 {
			t.Fatal("original branch also retained a tombstone", count)
		}
	})
}

// A copied request is insufficient consent, and a new cut cannot replace the
// first retained closure even when its original owner signs again.
func TestVerifyRequestClosureConsentConflictAndHistoricalKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		_, closure, receipt, keys := verifyRequestClosureFixture(t)
		bad := closure
		bad.Signature = bytes.Clone(closure.Signature)
		bad.Signature[0] ^= 1
		if server.HandleError(func() { CloseVerifyOriginalRequest(t.Context(), bad, receipt, keys) }) == nil {
			t.Fatal("copied request closed without original owner consent")
		}
		first := CloseVerifyOriginalRequest(t.Context(), closure, receipt, keys)
		newKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{99}, ed25519.SeedSize))
		keys[8] = newKey.Public().(ed25519.PublicKey)
		newReceipt, err := protocol.SealProviderAttemptClosedUnreceived(t.Context(), closure, 8, newKey)
		if err != nil {
			t.Fatal(err)
		}
		retained := CloseVerifyOriginalRequest(t.Context(), closure, newReceipt, keys)
		if !bytes.Equal(retained.ClosedUnreceived.Body, first.ClosedUnreceived.Body) || !bytes.Equal(retained.ClosedUnreceived.Signature, first.ClosedUnreceived.Signature) {
			t.Fatal("server rotation replaced original closure receipt")
		}
		changed := closure
		changed.CutHash[0] ^= 1
		changedPtr, err := protocol.SealProviderAttemptRequestClosure(t.Context(), changed, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, ed25519.SeedSize)))
		if err != nil {
			t.Fatal(err)
		}
		changedReceipt, err := protocol.SealProviderAttemptClosedUnreceived(t.Context(), *changedPtr, 8, newKey)
		if err != nil {
			t.Fatal(err)
		}
		recovered := server.HandleError(func() { CloseVerifyOriginalRequest(t.Context(), *changedPtr, changedReceipt, keys) })
		cause, ok := recovered.(error)
		if !ok || !errors.Is(cause, ErrVerifyRequestClosureConflict) {
			t.Fatal("new cut replaced original closure consent", recovered)
		}
	})
}

// The real PostgreSQL wait edge proves the contender reached the same request
// fence. Cancellation rolls back only that contender, then healthy retry works.
func verifyRequestClosureContention(t testing.TB, canceled bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	original, closure, receipt, keys := verifyRequestClosureFixture(t)
	ready := make(chan int, 1)
	done := make(chan error, 1)
	waitCtx, stop := context.WithCancel(ctx)
	defer stop()
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer tx.Rollback(context.WithoutCancel(ctx))
		closeVerifyOriginalRequestInTx(ctx, tx, closure, receipt, keys)
		var ownerPid int
		server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
		go func() {
			var failure error
			server.HandleError(func() {
				server.Db(waitCtx, func(other server.PgConn) {
					contender, err := other.BeginTx(waitCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					server.Raise(err)
					defer contender.Rollback(context.WithoutCancel(waitCtx))
					var pid int
					server.Raise(contender.QueryRow(waitCtx, `SELECT pg_backend_pid()`).Scan(&pid))
					ready <- pid
					retainVerifyOriginalInTx(waitCtx, contender, original)
					server.Raise(contender.Commit(waitCtx))
				})
			}, func(err error) { failure = err })
			done <- failure
		}()
		var pid int
		select {
		case pid = <-ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		waitCloseReportDatabaseConflict(t, ctx, tx, pid, ownerPid)
		if canceled {
			stop()
			select {
			case failure := <-done:
				joined = true
				if failure == nil || (!errors.Is(failure, context.Canceled) && !server.IsDoneError(failure)) {
					t.Fatal("canceled request contender lost owner cause", failure)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		server.Raise(tx.Commit(ctx))
	})
	if !joined {
		select {
		case failure := <-done:
			joined = true
			if !errors.Is(failure, ErrVerifyRequestClosed) {
				t.Fatal("concurrent assignment escaped closed request fence", failure)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if got := CloseVerifyOriginalRequest(ctx, closure, receipt, keys); got.ClosedUnreceived == nil {
		t.Fatal("healthy closure retry lost durable fence")
	}
}

// A closing owner wins before the waiting assignment is visible anywhere.
func TestVerifyRequestClosureConcurrentAssignmentUsesSameFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { verifyRequestClosureContention(t, false) })
}

// Cancellation cannot replace or erase a closure committed by another owner.
func TestVerifyRequestClosureCanceledContenderKeepsOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { verifyRequestClosureContention(t, true) })
}

// A failed durable insert cannot return a signed absence receipt or consume
// request ownership. The identical healthy continuation still closes once.
func TestVerifyRequestClosureRollbackKeepsContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		_, closure, receipt, keys := verifyRequestClosureFixture(t)
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `CREATE FUNCTION synthetic_closure_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic closure insert failure'; END $$; CREATE TRIGGER synthetic_closure_abort BEFORE INSERT ON verify_original_request_closed FOR EACH ROW EXECUTE FUNCTION synthetic_closure_abort()`))
		})
		remove := func() {
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `DROP TRIGGER IF EXISTS synthetic_closure_abort ON verify_original_request_closed; DROP FUNCTION IF EXISTS synthetic_closure_abort()`))
			})
		}
		defer remove()
		var result *VerifyRequestClosureResult
		if recovered := server.HandleError(func() { result = CloseVerifyOriginalRequest(t.Context(), closure, receipt, keys) }); recovered == nil || result != nil {
			t.Fatal("failed closure commit delivered signed absence", recovered, result)
		}
		if original := GetVerifyOriginalRequest(t.Context(), VerifyRequestClosureLocator(closure)); original != nil {
			t.Fatal("rolled-back closure invented a received original")
		}
		remove()
		result = CloseVerifyOriginalRequest(t.Context(), closure, receipt, keys)
		if result == nil || result.ClosedUnreceived == nil || result.Original != nil {
			t.Fatal("failed closure lost healthy continuation", result)
		}
	})
}
