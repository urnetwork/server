package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// S3: a payment that bought nothing is acked 200 only because a row records it
// for the operator and the reconciler. When writing that row fails, the payment
// would leave no trace at all, so the batch must fail and Helius must redeliver.
// The db is replaced by in-memory seams; no Postgres.

// solanaFakeUnfulfilledStore answers the webhook's lookups and records the
// unfulfilled rows, failing every write while recordErr is set.
type solanaFakeUnfulfilledStore struct {
	searchResults map[string]*model.PaymentIntentSearchResult
	recordErr     error
	recorded      []*model.UnfulfilledSolanaPayment
}

func (self *solanaFakeUnfulfilledStore) install(t *testing.T) {
	search := heliusSearchPaymentIntents
	completed := heliusIsSolanaPaymentCompleted
	record := heliusRecordUnfulfilledSolanaPayment
	t.Cleanup(func() {
		heliusSearchPaymentIntents = search
		heliusIsSolanaPaymentCompleted = completed
		heliusRecordUnfulfilledSolanaPayment = record
	})

	heliusSearchPaymentIntents = func(references []string, _ *session.ClientSession) (*model.PaymentIntentSearchResult, error) {
		for _, reference := range references {
			if result, ok := self.searchResults[reference]; ok {
				return result, nil
			}
		}
		return nil, nil
	}
	heliusIsSolanaPaymentCompleted = func(_ context.Context, _ string) bool {
		return false
	}
	heliusRecordUnfulfilledSolanaPayment = func(_ context.Context, payment *model.UnfulfilledSolanaPayment) error {
		if self.recordErr != nil {
			return self.recordErr
		}
		self.recorded = append(self.recorded, payment)
		return nil
	}
}

func TestSolanaWebhookUnfulfilledRecordFailureFailsTheBatch(t *testing.T) {
	networkId := server.NewId()
	underpaidReference := "unfulfilled-record-underpaid-1"

	newStore := func() *solanaFakeUnfulfilledStore {
		return &solanaFakeUnfulfilledStore{
			searchResults: map[string]*model.PaymentIntentSearchResult{
				underpaidReference: {
					NetworkId:         &networkId,
					PaymentReference:  underpaidReference,
					ExpectedAmountUsd: 40,
					SubscriptionPlan:  model.SolanaPlanYearly,
				},
			},
		}
	}

	cases := []struct {
		name        string
		transaction *SolanaTransaction
		reason      string
	}{
		{
			name:        "no_intent",
			transaction: solanaTestPayment("unknown-reference-1", "sig-record-fail-unmatched", 40),
			reason:      model.SolanaUnfulfilledReasonNoIntent,
		},
		{
			name:        "underpaid",
			transaction: solanaTestPayment(underpaidReference, "sig-record-fail-underpaid", 5),
			reason:      model.SolanaUnfulfilledReasonUnderpaid,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newStore()
			store.install(t)
			webhookSession := &session.ClientSession{Ctx: context.Background()}

			// the write fails: the delivery must not be acked
			recordErr := errors.New("synthetic unfulfilled write failure")
			store.recordErr = recordErr
			result, err := HeliusWebhook([]*SolanaTransaction{c.transaction}, webhookSession)
			if err == nil {
				t.Fatalf("%s payment acked with %q after its unfulfilled record failed to write; Helius will never redeliver it", c.name, result.Message)
			}
			connect.AssertEqual(t, errors.Is(err, recordErr), true)

			// the redelivery after the db recovers records the payment and acks
			store.recordErr = nil
			_, err = HeliusWebhook([]*SolanaTransaction{c.transaction}, webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(store.recorded), 1)
			connect.AssertEqual(t, store.recorded[0].TxSignature, c.transaction.Signature)
			connect.AssertEqual(t, store.recorded[0].Reason, c.reason)
		})
	}
}
