package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// GetPaymentReconcileUnknownToStore is the only read the reconciler's bounded
// retry makes: whether the store object's unknown_to_store disposition is
// recorded, and how many distinct real runs answered it not found. Rows of
// another store, action, evidence or reason, rows before the lookback and
// dry-run rows count for neither.
func TestGetPaymentReconcileUnknownToStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		evidence := "unknown-to-store-" + server.NewId().String()
		reason := "transaction_not_found"
		since := server.NowUtc().Add(-time.Hour)
		notFound := map[string]any{"leg": "status", "reason": reason}

		add := func(runId server.Id, store string, action string, evidence string, details map[string]any, dryRun bool) {
			t.Helper()
			connect.AssertEqual(t, AddPaymentReconciliationEvent(ctx, &PaymentReconciliationEvent{
				RunId:    runId,
				Store:    store,
				Action:   action,
				Evidence: evidence,
				Details:  details,
				DryRun:   dryRun,
			}), nil)
		}

		recorded, errorRuns := GetPaymentReconcileUnknownToStore(ctx, SubscriptionMarketApple, evidence, reason, since)
		connect.AssertEqual(t, recorded, false)
		connect.AssertEqual(t, errorRuns, 0)

		// two not-found errors in one run count once; another run counts again
		firstRunId := server.NewId()
		add(firstRunId, SubscriptionMarketApple, PaymentReconcileActionError, evidence, notFound, false)
		add(firstRunId, SubscriptionMarketApple, PaymentReconcileActionError, evidence, notFound, false)
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionError, evidence, notFound, false)

		// none of these count
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionError, evidence, map[string]any{"leg": "status"}, false)
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionError, evidence, map[string]any{"leg": "status", "reason": "other"}, false)
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionError, evidence, nil, false)
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionError, evidence, notFound, true)
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionError, "other-"+evidence, notFound, false)
		add(server.NewId(), SubscriptionMarketGoogle, PaymentReconcileActionError, evidence, notFound, false)
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionCredited, evidence, notFound, false)

		recorded, errorRuns = GetPaymentReconcileUnknownToStore(ctx, SubscriptionMarketApple, evidence, reason, since)
		connect.AssertEqual(t, recorded, false)
		connect.AssertEqual(t, errorRuns, 2)

		// the lookback: every row is before a later since
		_, errorRuns = GetPaymentReconcileUnknownToStore(ctx, SubscriptionMarketApple, evidence, reason, server.NowUtc().Add(time.Hour))
		connect.AssertEqual(t, errorRuns, 0)

		// a dry-run disposition is not the disposition, nor is another store's
		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionUnknownToStore, evidence, notFound, true)
		add(server.NewId(), SubscriptionMarketGoogle, PaymentReconcileActionUnknownToStore, evidence, notFound, false)
		recorded, _ = GetPaymentReconcileUnknownToStore(ctx, SubscriptionMarketApple, evidence, reason, since)
		connect.AssertEqual(t, recorded, false)

		add(server.NewId(), SubscriptionMarketApple, PaymentReconcileActionUnknownToStore, evidence, notFound, false)
		recorded, errorRuns = GetPaymentReconcileUnknownToStore(ctx, SubscriptionMarketApple, evidence, reason, since)
		connect.AssertEqual(t, recorded, true)
		connect.AssertEqual(t, errorRuns, 2)
	})
}
