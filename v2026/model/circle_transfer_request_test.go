// Synthetic customer intents exercise real durable request/observation custody.
package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/urnetwork/server/v2026"
)

func circleTransferTestBasis() CircleTransferBasis {
	return CircleTransferBasis{CircleUserId: server.NewId(), WalletId: server.NewId().String(), TokenId: server.NewId().String(), Blockchain: "SYNTHETIC", Destination: "synthetic-destination", Amount: 1_000_001_000}
}

func TestCustomerTransferExactDecimalAmount(t *testing.T) {
	for _, c := range []struct {
		amount NanoCents
		want   string
	}{{amount: 1000, want: "0.000001"}, {amount: 1_000_000_000, want: "1.000000"}, {amount: 1_000_001_000, want: "1.000001"}, {amount: math.MaxInt64 / 1000 * 1000, want: "9223372036.854775"}} {
		if got, err := CircleTransferAmount(c.amount); err != nil || got != c.want {
			t.Fatal("exact USDC amount changed", c.amount, got, err)
		}
	}
	for _, amount := range []NanoCents{0, -1000, 1, 999, 1001, math.MaxInt64} {
		if _, err := CircleTransferAmount(amount); err == nil {
			t.Fatal("inexact or invalid money admitted", amount)
		}
	}
}

func TestCustomerTransferConcurrentIntentRetainsOneKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network, user, request := server.NewId(), server.NewId(), server.NewId()
		basis := circleTransferTestBasis()
		start := make(chan struct{})
		values := make(chan *CircleTransferRequest, 16)
		errs := make(chan error, 16)
		var joined sync.WaitGroup
		for range 16 {
			joined.Add(1)
			go func() {
				defer joined.Done()
				<-start
				value, err := RetainCircleTransferRequest(ctx, network, user, request, basis)
				values <- value
				errs <- err
			}()
		}
		close(start)
		joined.Wait()
		close(values)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var first *CircleTransferRequest
		for value := range values {
			if value == nil {
				t.Fatal("missing retained request")
			}
			if first == nil {
				first = value
			}
			if value.IdempotencyKey != first.IdempotencyKey || value.Body != first.Body {
				t.Fatal("concurrent request created another logical transfer")
			}
		}
		key, err := uuid.Parse(first.IdempotencyKey.String())
		if err != nil || key.Version() != 4 {
			t.Fatal("provider idempotency key is not UUID v4", err)
		}
		other, err := RetainCircleTransferRequest(ctx, network, user, server.NewId(), basis)
		if err != nil || other == nil || other.IdempotencyKey == first.IdempotencyKey {
			t.Fatal("distinct identical user intents were collapsed", err)
		}
	})
}

func TestCustomerTransferRetainedTermsCannotChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network, user, request := server.NewId(), server.NewId(), server.NewId()
		basis := circleTransferTestBasis()
		original, err := RetainCircleTransferRequest(ctx, network, user, request, basis)
		if err != nil || original == nil {
			t.Fatal(err)
		}
		for _, change := range []func(*CircleTransferBasis){func(v *CircleTransferBasis) { v.Amount += 1000 }, func(v *CircleTransferBasis) { v.Destination = "different-destination" }, func(v *CircleTransferBasis) { v.WalletId = server.NewId().String() }, func(v *CircleTransferBasis) { v.CircleUserId = server.NewId() }} {
			next := basis
			change(&next)
			if _, err := RetainCircleTransferRequest(ctx, network, user, request, next); !errors.Is(err, ErrCircleTransferRequestChanged) {
				t.Fatal("same request accepted different intent", err)
			}
		}
		retained, err := GetCircleTransferRequest(ctx, network, user, request)
		if err != nil || retained == nil || retained.Body != original.Body || retained.Basis != basis {
			t.Fatal("refusal changed original request", err)
		}
		foreign, err := GetCircleTransferRequest(ctx, server.NewId(), user, request)
		if err != nil || foreign != nil {
			t.Fatal("foreign network read original request", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := BeginCircleTransferSubmission(canceled, retained); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost at request admission", err)
		}
	})
}

func TestCustomerTransferObservationCommitFailureRetainsUnknownRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		original, err := RetainCircleTransferRequest(ctx, server.NewId(), server.NewId(), server.NewId(), circleTransferTestBasis())
		if err != nil || original == nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE FUNCTION synthetic_customer_observation_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic journal refusal'; END$$;
		CREATE TRIGGER synthetic_customer_observation_failure BEFORE INSERT ON circle_transfer_observation FOR EACH ROW EXECUTE FUNCTION synthetic_customer_observation_failure()`))
		})
		challenge := server.NewId().String()
		if _, err := ObserveCircleTransfer(ctx, original, challenge, "PENDING", strings.Repeat("a", 64), 201); err == nil {
			t.Fatal("failed journal acknowledged challenge")
		}
		retained, err := GetCircleTransferRequest(ctx, original.NetworkId, original.UserId, original.RequestId)
		if err != nil || retained == nil || retained.ChallengeId != nil || retained.Body != original.Body || retained.IdempotencyKey != original.IdempotencyKey {
			t.Fatal("failed journal destroyed recovery basis", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DROP TRIGGER synthetic_customer_observation_failure ON circle_transfer_observation`))
		})
		value, err := ObserveCircleTransfer(ctx, retained, challenge, "PENDING", strings.Repeat("a", 64), 201)
		if err != nil || value == nil || value.ChallengeId == nil || *value.ChallengeId != challenge {
			t.Fatal("same exact outcome could not reconcile", err)
		}
	})
}

func TestCustomerTransferContradictionAndHistoryRemainRetained(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		original, err := RetainCircleTransferRequest(ctx, server.NewId(), server.NewId(), server.NewId(), circleTransferTestBasis())
		if err != nil || original == nil {
			t.Fatal(err)
		}
		challenge := server.NewId().String()
		for _, status := range []string{"PENDING", "IN_PROGRESS", "COMPLETE"} {
			value, err := ObserveCircleTransfer(ctx, original, challenge, status, strings.Repeat("b", 64), 200)
			if err != nil || value == nil {
				t.Fatal(err)
			}
		}
		if _, err := ObserveCircleTransfer(ctx, original, challenge, "FAILED", strings.Repeat("c", 64), 200); !errors.Is(err, ErrCircleTransferObservation) {
			t.Fatal("terminal challenge contradiction admitted", err)
		}
		retained, err := GetCircleTransferRequest(ctx, original.NetworkId, original.UserId, original.RequestId)
		if err != nil || retained == nil || !retained.ReviewRequired || retained.ChallengeStatus != "COMPLETE" {
			t.Fatal("contradiction replaced original terminal outcome", err)
		}
		if _, err := BeginCircleTransferSubmission(ctx, retained); !errors.Is(err, ErrCircleTransferObservation) {
			t.Fatal("contradictory request was resumed", err)
		}
		for _, sql := range []string{`DELETE FROM circle_transfer_observation`, `DELETE FROM circle_transfer_request`, `TRUNCATE circle_transfer_observation`, `UPDATE circle_transfer_request SET review_required=false`, `UPDATE circle_transfer_request SET idempotency_key=gen_random_uuid()`} {
			server.Db(ctx, func(conn server.PgConn) {
				if _, err := conn.Exec(ctx, sql); err == nil {
					t.Fatal("custody guard allowed history mutation", sql)
				}
			})
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM circle_transfer_observation`).Scan(&count))
			if count != 4 {
				t.Fatal("contradiction audit was lost", count)
			}
		})
	})
}

func TestCustomerTransferDisabledCustodyGuardCannotSubmit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		original, err := RetainCircleTransferRequest(ctx, server.NewId(), server.NewId(), server.NewId(), circleTransferTestBasis())
		if err != nil || original == nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE circle_transfer_request DISABLE TRIGGER circle_transfer_request_guard`))
		})
		if _, err := BeginCircleTransferSubmission(ctx, original); err == nil {
			t.Fatal("unguarded request could be submitted")
		}
		retained, err := GetCircleTransferRequest(ctx, original.NetworkId, original.UserId, original.RequestId)
		if err != nil || retained == nil || retained.SubmissionCount != 0 {
			t.Fatal("refused admission changed submission state", err)
		}
	})
}
