// Controller sends consult the retained provider earning boundary: drift
// refuses before money moves, accepted attempts still reconcile, and an
// unavailable observation holds the same payment for a bounded retry.
package controller

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProviderBoundaryFinalCircleWaitCannotChangeEarningAsset(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		original := server.Config.RequireSimpleResource("sn.yml").Bytes()
		changed := bytes.Replace(original, []byte(cutoff.Format(time.RFC3339)), []byte(cutoff.Add(time.Hour).Format(time.RFC3339)), 1)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		sends, waits := 0, 0
		var releaseChanged func()
		defer func() {
			if releaseChanged != nil {
				releaseChanged()
			}
		}()
		client := controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			return circleTransferAfterAdmission(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}, func(context.Context) error {
				waits++
				releaseChanged = server.Config.PushSimpleResource("sn.yml", changed)
				return nil
			}, func(context.Context) (*CreateTransferTransactionResult, error) {
				sends++
				return &CreateTransferTransactionResult{Id: "synthetic-boundary-send"}, nil
			})
		})
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		result, err := AdvancePayment(args, owner)
		if !errors.Is(err, server.ErrProviderEarningBoundaryMismatch) || waits != 1 || sends != 0 || result == nil || result.Complete || result.Canceled || result.Retryable {
			t.Fatal("wait-time policy replacement admitted send or hid hard refusal", result, err, waits, sends)
		}
		retained, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || retained == nil || retained.CircleIdempotencyKey == nil || retained.PaymentRecord != nil || retained.Canceled || retained.Payout != payment.Payout {
			t.Fatal("pre-send refusal erased original request or obligation", retained, err)
		}
		originalKey := *retained.CircleIdempotencyKey
		releaseChanged()
		releaseChanged = nil
		client.CreateTransferTransactionFunc = func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			return circleTransferAfterAdmission(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}, func(context.Context) error { return nil }, func(context.Context) (*CreateTransferTransactionResult, error) {
				if key != originalKey || amount != 9.99 {
					t.Fatal("resume changed retained attempt basis", key, amount)
				}
				sends++
				return &CreateTransferTransactionResult{Id: "synthetic-boundary-send"}, nil
			})
		}
		if _, err := AdvancePayment(args, owner); err != nil || sends != 1 {
			t.Fatal("original policy did not resume same retained attempt", err, sends)
		}
		if err := requireCircleProviderPayment(owner.Ctx); err != nil {
			t.Fatal("customer transfer inherited provider boundary", err)
		}
	})
}

func TestProviderBoundaryAcceptedAttemptReconcilesDuringDrift(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		original := server.Config.RequireSimpleResource("sn.yml").Bytes()
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		accepted, _ := controllerRetainAttempt(t, owner, payment, "synthetic-already-accepted")
		before, err := GetProviderPayoutTransitionStatus(owner.Ctx)
		if err != nil || before == nil || !before.NewLegacySubmissionsAdmitted || !before.ExistingAttemptReconciliationAllowed {
			t.Fatal("prepared status did not distinguish sends and reconciliation", before, err)
		}
		changed := bytes.Replace(original, []byte(cutoff.Format(time.RFC3339)), []byte(cutoff.Add(-time.Hour).Format(time.RFC3339)), 1)
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", changed))
		sends, reads := 0, 0
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return nil, errors.New("must not resubmit accepted transfer")
		})
		client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
			reads++
			if id != *accepted.PaymentRecord {
				t.Fatal("reconciliation lost original processor identity")
			}
			return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "COMPLETE", Blockchain: "MATIC", TxHash: "0xsynthetic-boundary-confirmed"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		result, err := AdvancePayment(args, owner)
		if err != nil || result == nil || !result.Complete || result.Canceled || sends != 0 || reads != 1 {
			t.Fatal("boundary conflict blocked or resent accepted transfer", result, err, sends, reads)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got == nil || !got.Completed || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *accepted.CircleIdempotencyKey || got.PaymentRecord == nil || *got.PaymentRecord != *accepted.PaymentRecord || !got.AttributionReviewRequired {
			t.Fatal("reconciliation lost original attempt or hid policy review", got, err)
		}
		status, err := GetProviderPayoutTransitionStatus(owner.Ctx)
		if err != nil || status == nil || status.EarningBoundaryReady || status.EarningBoundaryReason == "" || status.LegacyPreCutoffPaymentsAllowed || status.NewLegacySubmissionsAdmitted || !status.ExistingAttemptReconciliationAllowed || status.NewMainnetWritesAdmitted || status.DeploymentVerified {
			t.Fatal("loaded status hid drift or claimed deployment", status, err)
		}
		if _, err := AdvancePayment(args, owner); err != nil || reads != 1 || sends != 0 {
			t.Fatal("terminal repeated reconciliation sent again", err, reads, sends)
		}
	})
}

// The retained earning-boundary statement, as every ordinary observation sends it.
const providerBoundaryReadSql = "SELECT earning_identity, identity_sha256, initial_config_sha256, prepared_at FROM provider_payout_boundary"

// Records how PostgreSQL ended each retained earning-boundary read on the
// traced fixture pools. Its methods are safe for concurrent use.
type providerBoundaryReadTrace struct {
	stateLock sync.Mutex
	readErrs  []error
}

// Marks a boundary read so its completion can be attributed to it.
type providerBoundaryReadKey struct{}

// Only the exact boundary statement is marked; every other statement passes through.
func (self *providerBoundaryReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), providerBoundaryReadSql) {
		return context.WithValue(ctx, providerBoundaryReadKey{}, true)
	}
	return ctx
}

// Retains a marked read's completion error, nil when the read succeeded.
func (self *providerBoundaryReadTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(providerBoundaryReadKey{}).(bool); marked {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readErrs = append(self.readErrs, data.Err)
	}
}

// Completion errors of the boundary reads observed so far, in order.
func (self *providerBoundaryReadTrace) snapshot() []error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readErrs)
}

// A boundary read that exhausts its physical statement budget holds the
// payment for one bounded continuation of the same attempt. Ordinary
// observations read only provider_payout_boundary (never migration_catalog),
// so an access-exclusive lock on that table is the barrier: the actual SELECT
// can end only when PostgreSQL cancels it, and the trace proves that read, not
// another statement, hit the timeout.
func TestProviderBoundaryDatabaseObservationTimeoutSchedulesSamePayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		sends := 0
		controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return &CreateTransferTransactionResult{Id: "synthetic-after-observation-recovery"}, nil
		})
		ctx, cancel := context.WithTimeout(owner.Ctx, 30*time.Second)
		defer cancel()
		owner.Ctx = ctx
		database := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName).RequireString("db")
		if !strings.HasPrefix(database, "test_") {
			t.Fatal("physical timeout control requires private test database")
		}
		databaseSql := pgx.Identifier{database}.Sanitize()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER DATABASE `+databaseSql+` SET statement_timeout='100ms'`))
		}, server.OptReadWrite())
		server.PgReset()
		// The scope must exist before the blocker takes a pool connection.
		trace := &providerBoundaryReadTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		blocker, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			_ = scope.Close()
			t.Fatal(err)
		}
		lock, err := blocker.Begin(ctx)
		if err != nil {
			blocker.Release()
			_ = scope.Close()
			t.Fatal(err)
		}
		finish := func() {
			if blocker != nil {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cleanupCancel()
				_ = lock.Rollback(cleanupCtx)
				_, resetErr := blocker.Exec(cleanupCtx, `ALTER DATABASE `+databaseSql+` RESET statement_timeout`)
				blocker.Release()
				blocker = nil
				// Every traced user has returned; restore the fixture pools before reset.
				scopeErr := scope.Close()
				server.PgReset()
				if resetErr != nil || scopeErr != nil {
					t.Error("private database timeout cleanup", resetErr, scopeErr)
				}
			}
		}
		defer finish()
		if _, err := lock.Exec(ctx, `LOCK TABLE provider_payout_boundary IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		result, err := AdvancePayment(args, owner)
		finish()
		reads := trace.snapshot()
		if len(reads) == 0 {
			t.Fatal("payment path issued no retained boundary read under the barrier")
		}
		for _, readErr := range reads {
			var timeout *pgconn.PgError
			if !errors.As(readErr, &timeout) || timeout.Code != pgerrcode.QueryCanceled || !strings.Contains(timeout.Message, "statement timeout") {
				t.Fatal("held boundary read did not end at its physical statement budget", readErr)
			}
		}
		if err != nil || result == nil || result.Complete || result.Canceled || !result.Retryable || !strings.Contains(result.HeldReason, "statement timeout") || sends != 0 || owner.Ctx.Err() != nil {
			t.Fatal("temporary observation ended or sent retained payment", result, err, sends)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got == nil || got.Canceled || got.Completed || got.Payout != payment.Payout || got.CircleIdempotencyKey != nil || got.PaymentRecord != nil {
			t.Fatal("temporary schema read changed financial state", got, err)
		}
		before := server.NowUtc()
		for i := 0; i < 2; i++ {
			server.Tx(owner.Ctx, func(tx server.PgTx) {
				server.Raise(AdvancePaymentPost(args, result, owner, tx))
			})
		}
		after := server.NowUtc()
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var count int
			var earliest, latest time.Time
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT COUNT(*),MIN(run_at),MAX(run_at) FROM pending_task WHERE args_json::jsonb->>'payment_id'=$1`, payment.PaymentId.String()).Scan(&count, &earliest, &latest))
			if count != 1 || earliest.Before(before.Add(5*time.Minute)) || latest.After(after.Add(30*time.Minute)) {
				t.Fatal("observation outage did not schedule bounded same-payment continuation", count, earliest, latest)
			}
		})
		if _, err := AdvancePayment(args, owner); err != nil || sends != 1 {
			t.Fatal("same retained payment could not continue after read recovery", err, sends)
		}
	})
}
