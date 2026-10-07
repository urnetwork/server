// Exercise the joined policy, real GET decoder and retained-attempt controller.
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProviderCompositionRetainedAttemptReadRecoversDuringBoundaryHold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		original := server.Config.RequireSimpleResource("sn.yml").Bytes()
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		accepted, _ := controllerRetainAttempt(t, owner, payment, "synthetic-composed-attempt")
		changed := bytes.Replace(original, []byte(cutoff.Format(time.RFC3339)), []byte(cutoff.Add(time.Hour).Format(time.RFC3339)), 1)
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", changed))
		started := time.Now()
		now := started
		reads, closed, sends := 0, 0, 0
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return nil, errors.New("retained attempt must not be submitted again")
		})
		readClient := paymentReadTestCircle(paymentReadHooks{
			now: func() time.Time { return now },
			wait: func(ctx context.Context, delay time.Duration) error {
				if closed != reads || delay < time.Second || delay >= 2*time.Second {
					t.Fatal("composed GET lost bounded pause/body ownership")
				}
				now = now.Add(delay)
				return ctx.Err()
			},
			do: func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.Body != nil || !strings.HasSuffix(request.URL.Path, "/"+*accepted.PaymentRecord) {
					t.Fatal("reconciliation changed processor attempt identity")
				}
				reads++
				if now.Sub(started) < 65*time.Second {
					return paymentReadTestResponse(503, "temporary processor read outage", &closed), nil
				}
				payload := fmt.Sprintf(`{"data":{"transaction":{"id":%q,"state":"COMPLETE","blockchain":"MATIC","txHash":"0xsynthetic-composed-confirmed"}}}`, *accepted.PaymentRecord)
				return paymentReadTestResponse(200, payload, &closed), nil
			},
		})
		client.GetTransactionFunc = readClient.GetTransaction
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		result, err := AdvancePayment(args, owner)
		if err != nil || result == nil || !result.Complete || result.Canceled || sends != 0 || reads < 2 || closed != reads || now.Sub(started) < 65*time.Second {
			t.Fatal("retained controller did not recover using actual GET", result, err, sends, reads, closed)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got == nil || !got.Completed || !got.AttributionReviewRequired || got.PaymentRecord == nil || *got.PaymentRecord != *accepted.PaymentRecord || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *accepted.CircleIdempotencyKey {
			t.Fatal("composed recovery replaced the original attempt or hid policy hold", got, err)
		}
		status, err := GetProviderPayoutTransitionStatus(owner.Ctx)
		if err != nil || status == nil || status.NewLegacySubmissionsAdmitted || !status.ExistingAttemptReconciliationAllowed || status.NewMainnetWritesAdmitted {
			t.Fatal("status confused held submissions with stopped reconciliation", status, err)
		}
		previousReads := reads
		if _, err := AdvancePayment(args, owner); err != nil || reads != previousReads || sends != 0 {
			t.Fatal("completed attempt was observed or submitted again", err, reads, sends)
		}
	})
}

func TestProviderCompositionCanceledReadRetainsAttemptAndCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		accepted, _ := controllerRetainAttempt(t, owner, payment, "synthetic-canceled-observation")
		retainedCtx := owner.Ctx
		ctx, cancel := context.WithCancel(retainedCtx)
		defer cancel()
		owner.Ctx = ctx
		reads, sends := 0, 0
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return nil, errors.New("must not create another attempt")
		})
		readClient := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
			reads++
			cancel()
			return nil, request.Context().Err()
		}})
		client.GetTransactionFunc = readClient.GetTransaction
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if !errors.Is(err, context.Canceled) || result == nil || result.Complete || result.Canceled || result.Retryable || reads != 1 || sends != 0 {
			t.Fatal("public controller hid cancellation or canceled the obligation", result, err, reads, sends)
		}
		server.Tx(retainedCtx, func(tx server.PgTx) {
			if err := AdvancePaymentPost(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, result, owner, tx); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled public continuation did not refuse scheduling", err)
			}
		})
		var queued int
		server.Db(retainedCtx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(retainedCtx, `SELECT COUNT(*) FROM pending_task WHERE args_json::jsonb->>'payment_id'=$1`, payment.PaymentId.String()).Scan(&queued))
		})
		if queued != 0 {
			t.Fatal("canceled read created a new continuation", queued)
		}
		got, err := model.GetPayment(retainedCtx, payment.PaymentId)
		if err != nil || got == nil || got.Completed || got.Canceled || got.PaymentRecord == nil || *got.PaymentRecord != *accepted.PaymentRecord || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *accepted.CircleIdempotencyKey {
			t.Fatal("canceled observation changed the retained payment", got, err)
		}
	})
}
