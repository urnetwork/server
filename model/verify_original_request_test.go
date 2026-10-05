// Real PostgreSQL request ownership survives lost replies, duplicate processes
// and rollback; immutable original signatures remain the comparison authority.
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
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// A competing live process may sample another pending hop before discovering
// the retained request. It must return the first receipt, not the new choice.
func verifyOriginalRequestTestAlternative(t testing.TB, original *VerifyOriginalTransition, key ed25519.PrivateKey) *VerifyOriginalTransition {
	t.Helper()
	body, err := DecodeVerifyOriginal(original)
	if err != nil {
		t.Fatal(err)
	}
	body.Trail.TrailId = server.NewId()
	body.Trail.Pending.ClientId = server.NewId()
	message, err := connect.BuildVerifyAssignMessage(body.Trail.ServerKeyId, connect.Id(body.Trail.TrailId), body.Trail.ServerNonce, body.Trail.Vpk, byte(body.Trail.M), []connect.Id{connect.Id(body.Trail.Hops[0].ClientId), connect.Id(body.Trail.Pending.ClientId)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(struct {
		Assign *connect.VerifyAssignResult `json:"assign,omitempty"`
	}{Assign: &connect.VerifyAssignResult{TrailId: connect.Id(body.Trail.TrailId), ServerNonce: body.Trail.ServerNonce, Trail: []connect.Id{connect.Id(body.Trail.Hops[0].ClientId)}, NextHop: connect.Id(body.Trail.Pending.ClientId), M: body.Trail.M, ServerKeyId: body.Trail.ServerKeyId, AssignSig: ed25519.Sign(key, message)}})
	if err != nil {
		t.Fatal(err)
	}
	body.ResponseJson = string(response)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &VerifyOriginalTransition{Body: raw, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), raw...))}
}

// Exact request bytes retain one original assignment after the caller loses its reply.
func TestVerifyOriginalRequestKeepsFirstTrailAcrossFreshRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		original, key := testVerifySignedOriginal(t)
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		first := RetainVerifyOriginal(t.Context(), original)
		second := RetainVerifyOriginal(t.Context(), verifyOriginalRequestTestAlternative(t, original, key))
		if !bytes.Equal(first.Body, second.Body) || !bytes.Equal(first.Signature, second.Signature) {
			t.Fatal("same original request minted a second assignment")
		}
		locator := VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature}
		if got := GetVerifyOriginalRequest(t.Context(), locator); got == nil || !bytes.Equal(got.Body, original.Body) {
			t.Fatal("fresh exact request lookup lost first original")
		}
		var count int
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM verify_original_transition WHERE original_body=$1`, original.Body).Scan(&count))
		})
		if count != 1 {
			t.Fatal("request retained duplicate original", count)
		}
	})
}

// The original client and domain are routing identities, not interchangeable
// metadata on a signature over the unchanged verification wire grammar.
func TestVerifyOriginalRequestSeparatesClientDomainAndMessage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		original, _ := testVerifySignedOriginal(t)
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		RetainVerifyOriginal(t.Context(), original)
		request := VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature}
		cases := []VerifyOriginalRequest{request, request, request}
		cases[0].ClientId = server.NewId()
		cases[1].Scope = &VerifyOriginalScope{NoId: 71}
		cases[2].Message = append([]byte(nil), request.Message...)
		cases[2].Message[len(cases[2].Message)-1] ^= 1
		for _, changed := range cases {
			if got := GetVerifyOriginalRequest(t.Context(), changed); got != nil {
				t.Fatal("foreign identity borrowed an original request")
			}
		}
	})
}

// Rolling writers still populate the bounded original lookup through the same
// database insert trigger. Two historic choices stay an explicit contradiction.
func TestVerifyOriginalRequestRetainsRollingWriterAmbiguity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		original, key := testVerifySignedOriginal(t)
		other := verifyOriginalRequestTestAlternative(t, original, key)
		for _, value := range []*VerifyOriginalTransition{original, other} {
			body, err := DecodeVerifyOriginal(value)
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO verify_original_transition(trail_id,previous_depth,observed_time,original_body,original_signature) VALUES($1,$2,$3,$4,$5)`, body.Trail.TrailId, body.PreviousDepth, server.NowUtc(), value.Body, value.Signature))
			})
		}
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		if recovered := server.HandleError(func() {
			GetVerifyOriginalRequest(t.Context(), VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature})
		}); recovered == nil {
			t.Fatal("ambiguous historical original was selected as a complete answer")
		}
	})
}

// An exact first request can be observed at the actual database lock before
// releasing its owner; cancellation rolls back only the waiting transaction.
func verifyOriginalRequestTestContention(t testing.TB, canceled bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	original, key := testVerifySignedOriginal(t)
	other := verifyOriginalRequestTestAlternative(t, original, key)
	type observed struct {
		value *VerifyOriginalTransition
		err   error
	}
	ready := make(chan int, 1)
	done := make(chan observed, 1)
	waitingCtx, cancelWaiting := context.WithCancel(ctx)
	defer cancelWaiting()
	joined := false
	started := false
	defer func() {
		cancelWaiting()
		if started && !joined {
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
		first := retainVerifyOriginalInTx(ctx, tx, original)
		if !bytes.Equal(first.Body, original.Body) {
			t.Fatal("first original changed before lock observation")
		}
		var ownerPid int
		server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
		started = true
		go func() {
			var value observed
			server.HandleError(func() {
				server.Db(waitingCtx, func(otherConn server.PgConn) {
					otherTx, err := otherConn.BeginTx(waitingCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					server.Raise(err)
					defer otherTx.Rollback(context.WithoutCancel(waitingCtx))
					var pid int
					server.Raise(otherTx.QueryRow(waitingCtx, `SELECT pg_backend_pid()`).Scan(&pid))
					ready <- pid
					value.value = retainVerifyOriginalInTx(waitingCtx, otherTx, other)
					server.Raise(otherTx.Commit(waitingCtx))
				})
			}, func(err error) { value.err = err })
			done <- value
		}()
		var pid int
		select {
		case pid = <-ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		waitCloseReportDatabaseConflict(t, ctx, tx, pid, ownerPid)
		if canceled {
			cancelWaiting()
		}
		server.Raise(tx.Commit(ctx))
	})
	var result observed
	select {
	case result = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if canceled {
		if result.err == nil || (!errors.Is(result.err, context.Canceled) && !server.IsDoneError(result.err)) {
			t.Fatal("canceled request lock lost real owner cause", result.err)
		}
	} else if result.err != nil || result.value == nil || !bytes.Equal(result.value.Body, original.Body) {
		t.Fatal("concurrent original request did not converge", result.err)
	}
	if got := RetainVerifyOriginal(ctx, other); !bytes.Equal(got.Body, original.Body) {
		t.Fatal("healthy retry replaced the original after contention")
	}
}

// Independent live writers must converge before either can expose its sample.
func TestVerifyOriginalRequestConcurrentOwnersConverge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { verifyOriginalRequestTestContention(t, false) })
}

// The canceled contender cannot erase or replace the first successful owner.
func TestVerifyOriginalRequestCanceledContenderRecovers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { verifyOriginalRequestTestContention(t, true) })
}

// Transactional failure after request indexing leaves no consumed identity.
func TestVerifyOriginalRequestRollbackThenHealthyRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		original, _ := testVerifySignedOriginal(t)
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `CREATE FUNCTION synthetic_request_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic request outbox failure'; END $$; CREATE TRIGGER synthetic_request_abort BEFORE INSERT ON verify_original_pending FOR EACH ROW EXECUTE FUNCTION synthetic_request_abort()`))
		})
		remove := func() {
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `DROP TRIGGER IF EXISTS synthetic_request_abort ON verify_original_pending; DROP FUNCTION IF EXISTS synthetic_request_abort()`))
			})
		}
		defer remove()
		if recovered := server.HandleError(func() { RetainVerifyOriginal(t.Context(), original) }); recovered == nil {
			t.Fatal("request rollback boundary was not reached")
		}
		if got := GetVerifyOriginalRequest(t.Context(), VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature}); got != nil {
			t.Fatal("failed original transaction consumed request identity")
		}
		remove()
		if got := RetainVerifyOriginal(t.Context(), original); !bytes.Equal(got.Body, original.Body) {
			t.Fatal("rolled-back request failed healthy admission")
		}
	})
}

// A forged mutation cannot replace either the portable request index or source.
func TestVerifyOriginalRequestImmutableIndexes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		original, _ := testVerifySignedOriginal(t)
		RetainVerifyOriginal(t.Context(), original)
		for _, table := range []string{"verify_original_request", "verify_original_request_lookup"} {
			for _, verb := range []string{"DELETE FROM ", "UPDATE ", "TRUNCATE "} {
				query := verb + table
				if verb == "UPDATE " {
					query += " SET trail_id=trail_id"
				}
				if recovered := server.HandleError(func() {
					server.Tx(t.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(t.Context(), query)) })
				}); recovered == nil {
					t.Fatal("original request index mutation succeeded", query)
				}
			}
		}
	})
}
