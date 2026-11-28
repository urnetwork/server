package controller

// A renewal whose transaction the App Store does not know in any environment
// the deployment accepts got a status error on every hourly run. On Main one
// pre-ledger renewal, keyed on an appTransactionId that neither production nor
// the sandbox knows, recorded an error on each of 499 runs from 2026-10-09, so
// the App Store watermark was never written and the monitor paged on it. These
// tests pin the bounded retry: a not-found answer stays a status error, tagged
// with its reason, until it has persisted through appleUnknownToStoreRetryRuns
// runs. Then it is recorded once as unknown_to_store, counted on the
// heartbeat, and no longer holds the watermark. Nothing is credited or ended
// from it, and the reconciler keeps asking the store every run.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The wire values operators and the monitor read.
const appleTestUnknownToStoreAction = "unknown_to_store"
const appleTestUnknownToStoreReason = "transaction_not_found"

// The not-found answers the bound counts before this run, so the run under
// test is the last attempt the retry allows.
const appleTestPriorNotFoundRuns = 23

// appleSeedNotFoundErrors writes count earlier status errors for the
// transaction, each in its own run, as earlier hourly runs would have.
func appleSeedNotFoundErrors(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	transactionId string,
	count int,
	reason string,
	dryRun bool,
) {
	t.Helper()
	for range count {
		details := map[string]any{
			"error": "App Store sandbox: Bad status: 404 Not Found " + appleTestTransactionIdNotFoundBody,
			"leg":   "status",
		}
		if reason != "" {
			details["reason"] = reason
		}
		err := model.AddPaymentReconciliationEvent(ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketApple,
			NetworkId: &networkId,
			Action:    model.PaymentReconcileActionError,
			Evidence:  transactionId,
			Details:   details,
			DryRun:    dryRun,
		})
		connect.AssertEqual(t, err, nil)
	}
}

func appleReconcileStatusErrorEvents(events []*model.PaymentReconciliationEvent) []*model.PaymentReconciliationEvent {
	statusErrors := []*model.PaymentReconciliationEvent{}
	for _, event := range events {
		if event.Store == model.SubscriptionMarketApple &&
			event.Action == model.PaymentReconcileActionError &&
			event.Details["leg"] == "status" {
			statusErrors = append(statusErrors, event)
		}
	}
	return statusErrors
}

func appleReconcileEventsWithAction(events []*model.PaymentReconciliationEvent, action string) []*model.PaymentReconciliationEvent {
	matching := []*model.PaymentReconciliationEvent{}
	for _, event := range events {
		if event.Store == model.SubscriptionMarketApple && event.Action == action {
			matching = append(matching, event)
		}
	}
	return matching
}

// appleReconcileHeartbeatStoreCount reads one App Store count from the run's
// durable heartbeat row.
func appleReconcileHeartbeatStoreCount(t testing.TB, events []*model.PaymentReconciliationEvent, key string) int {
	t.Helper()
	for _, event := range events {
		if event.Store != model.PaymentReconcileStoreAll || event.Action != model.PaymentReconcileActionHeartbeat {
			continue
		}
		stores, _ := event.Details["stores"].(map[string]any)
		apple, _ := stores[model.SubscriptionMarketApple].(map[string]any)
		count, _ := apple[key].(float64)
		return int(count)
	}
	t.Fatalf("the run wrote no heartbeat")
	return 0
}

// appleCountEvidenceEvents counts every audit row with the action and
// evidence, across runs.
func appleCountEvidenceEvents(t testing.TB, ctx context.Context, action string, evidence string, dryRun bool) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT count(*) FROM payment_reconciliation_event
			WHERE store = $1 AND action = $2 AND evidence = $3 AND dry_run = $4
			`,
			model.SubscriptionMarketApple,
			action,
			evidence,
			dryRun,
		).Scan(&count))
	})
	return count
}

// Within the bound, a renewal no accepted environment knows is one status
// error per run. It names the store's answer, carries the reason the bound
// counts, and keeps the watermark in place. This holds where only Production
// is accepted too, and the renewal's entitlement is untouched.
func TestPaymentReconcileAppleUnknownToStoreWithinRetryIsStatusError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileappleunknownretry", server.NewId())
		transactionId := "2000000000000701"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		priorWatermark := now.Add(-6 * time.Hour)
		connect.AssertEqual(t, model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark), nil)

		for _, environments := range [][]string{{"Production", "Sandbox"}, {"Production"}} {
			env.acceptEnvironments(environments...)

			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Errors, 1)

			events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
			statusErrors := appleReconcileStatusErrorEvents(events)
			if len(statusErrors) != 1 {
				t.Fatalf("environments %v: %d status errors, want 1", environments, len(statusErrors))
			}
			message, _ := statusErrors[0].Details["error"].(string)
			if !strings.Contains(message, "4040010") {
				t.Fatalf("environments %v: status error %q does not name Apple's 4040010", environments, message)
			}
			if statusErrors[0].Details["reason"] != appleTestUnknownToStoreReason {
				t.Fatalf("environments %v: status error reason %v, want %q", environments, statusErrors[0].Details["reason"], appleTestUnknownToStoreReason)
			}
			connect.AssertEqual(t, statusErrors[0].Evidence, transactionId)
			connect.AssertEqual(t, len(appleReconcileEventsWithAction(events, appleTestUnknownToStoreAction)), 0)
			connect.AssertEqual(t, appleReconcileHeartbeatStoreCount(t, events, appleTestUnknownToStoreAction), 0)

			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			if !ok || !watermark.Equal(priorWatermark) {
				t.Fatalf("environments %v: a retried not-found answer moved the App Store watermark", environments)
			}
			connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
			connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		}
	})
}

// After the bound, the renewal is recorded once as unknown_to_store with the
// environments asked and whether the transaction is in the credit ledger. The
// run has no store error and writes the watermark, the heartbeat counts the
// renewal, and nothing is credited or ended. Later runs keep asking the store,
// still count the renewal, and record nothing more.
func TestPaymentReconcileAppleUnknownToStoreAfterRetryIsRecordedOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileappleunknownonce", server.NewId())
		transactionId := "2000000000000801"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		appleSeedNotFoundErrors(t, ctx, networkId, transactionId, appleTestPriorNotFoundRuns, appleTestUnknownToStoreReason, false)
		priorWatermark := now.Add(-6 * time.Hour)
		connect.AssertEqual(t, model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark), nil)

		for run := range 2 {
			productionRequests := env.production.requestCount(transactionId)
			sandboxRequests := env.sandbox.requestCount(transactionId)

			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			if result.Errors != 0 {
				t.Fatalf("run %d: %d store errors, want none once the retry is spent", run, result.Errors)
			}
			connect.AssertEqual(t, result.Credited, 0)
			connect.AssertEqual(t, result.Ended, 0)
			connect.AssertEqual(t, result.EntitlementsRepaired, 0)

			events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
			connect.AssertEqual(t, len(appleReconcileStatusErrorEvents(events)), 0)
			connect.AssertEqual(t, appleReconcileHeartbeatStoreCount(t, events, appleTestUnknownToStoreAction), 1)

			dispositions := appleReconcileEventsWithAction(events, appleTestUnknownToStoreAction)
			if run == 0 {
				if len(dispositions) != 1 {
					t.Fatalf("run %d: %d unknown_to_store events, want 1", run, len(dispositions))
				}
				disposition := dispositions[0]
				connect.AssertEqual(t, disposition.Evidence, transactionId)
				connect.AssertEqual(t, disposition.NetworkId != nil && *disposition.NetworkId == networkId, true)
				connect.AssertEqual(t, disposition.DryRun, false)
				connect.AssertEqual(t, disposition.Details["leg"], "status")
				connect.AssertEqual(t, disposition.Details["reason"], appleTestUnknownToStoreReason)
				connect.AssertEqual(t, disposition.Details["ledger"], true)
				environments, _ := disposition.Details["environments"].([]any)
				if len(environments) != 2 || environments[0] != "Production" || environments[1] != "Sandbox" {
					t.Fatalf("run %d: environments %v, want [Production Sandbox]", run, disposition.Details["environments"])
				}
			} else {
				connect.AssertEqual(t, len(dispositions), 0)
			}
			connect.AssertEqual(t, appleCountEvidenceEvents(t, ctx, appleTestUnknownToStoreAction, transactionId, false), 1)

			// the store is still asked every run
			connect.AssertEqual(t, env.production.requestCount(transactionId), productionRequests+1)
			connect.AssertEqual(t, env.sandbox.requestCount(transactionId), sandboxRequests+1)

			// the complete pass without a store error wrote the watermark
			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			if !ok || watermark.Equal(priorWatermark) {
				t.Fatalf("run %d: the App Store watermark stayed in place", run)
			}

			// nothing was credited or ended from the store's not-found answer
			connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
			connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		}
	})
}

// Only the not-found answer from every accepted environment spends the
// retry. A sandbox outage, a production failure, or a 404 that is not 4040010
// stays a status error however many not-found answers came before, and so do
// not-found answers that do not count: errors without the reason (written
// before it existed) and dry-run errors.
func TestPaymentReconcileAppleUnknownToStoreBoundCountsOnlyNotFound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		for i, test := range []struct {
			name              string
			prior             int
			priorReason       string
			priorDryRun       bool
			productionCode    int
			productionBody    string
			sandboxCode       int
			sandboxBody       string
			wantReason        bool
			wantSandboxAsked  bool
			wantMessagePrefix string
		}{
			{
				name: "sandbox unavailable", prior: appleTestPriorNotFoundRuns, priorReason: appleTestUnknownToStoreReason,
				sandboxCode: http.StatusInternalServerError, sandboxBody: `{"errorCode":5000001,"errorMessage":"An unknown error occurred. Please try again."}`,
				wantSandboxAsked: true, wantMessagePrefix: "App Store sandbox: ",
			},
			{
				name: "production unavailable", prior: appleTestPriorNotFoundRuns, priorReason: appleTestUnknownToStoreReason,
				productionCode: http.StatusInternalServerError, productionBody: `{"errorCode":5000001,"errorMessage":"An unknown error occurred. Please try again."}`,
			},
			{
				name: "original transaction id not found", prior: appleTestPriorNotFoundRuns, priorReason: appleTestUnknownToStoreReason,
				productionCode: http.StatusNotFound, productionBody: `{"errorCode":4040005,"errorMessage":"Original transaction id not found."}`,
			},
			{
				name: "earlier errors without the reason", prior: 30, priorReason: "",
				wantReason: true, wantSandboxAsked: true, wantMessagePrefix: "App Store sandbox: ",
			},
			{
				name: "earlier dry-run errors", prior: appleTestPriorNotFoundRuns, priorReason: appleTestUnknownToStoreReason, priorDryRun: true,
				wantReason: true, wantSandboxAsked: true, wantMessagePrefix: "App Store sandbox: ",
			},
		} {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "reconcileappleunknownbound"+string(rune('a'+i)), server.NewId())
			transactionId := "200000000000090" + string(rune('1'+i))
			appleTestCredit(
				t, ctx, networkId, transactionId, "supporter_monthly",
				now.Add(-1*time.Hour), now.Add(23*time.Hour),
			)
			appleSeedNotFoundErrors(t, ctx, networkId, transactionId, test.prior, test.priorReason, test.priorDryRun)
			if test.productionCode != 0 {
				env.production.setAnswer(transactionId, test.productionCode, test.productionBody)
			}
			if test.sandboxCode != 0 {
				env.sandbox.setAnswer(transactionId, test.sandboxCode, test.sandboxBody)
			}
			priorWatermark := now.Add(-6 * time.Hour)
			connect.AssertEqual(t, model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark), nil)

			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
			statusErrors := slices.DeleteFunc(appleReconcileStatusErrorEvents(events), func(event *model.PaymentReconciliationEvent) bool {
				return event.Evidence != transactionId
			})
			if len(statusErrors) != 1 {
				t.Fatalf("%s: %d status errors for the renewal, want 1", test.name, len(statusErrors))
			}
			message, _ := statusErrors[0].Details["error"].(string)
			if !strings.HasPrefix(message, test.wantMessagePrefix) {
				t.Fatalf("%s: status error %q, want the prefix %q", test.name, message, test.wantMessagePrefix)
			}
			if gotReason := statusErrors[0].Details["reason"] == appleTestUnknownToStoreReason; gotReason != test.wantReason {
				t.Fatalf("%s: status error reason %v, want reason %t", test.name, statusErrors[0].Details["reason"], test.wantReason)
			}
			connect.AssertEqual(t, env.sandbox.requestCount(transactionId) == 1, test.wantSandboxAsked)
			connect.AssertEqual(t, appleCountEvidenceEvents(t, ctx, appleTestUnknownToStoreAction, transactionId, false), 0)

			// each case has its own renewal; earlier ones keep erroring in
			// later runs, so the watermark stays put either way
			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			if !ok || !watermark.Equal(priorWatermark) {
				t.Fatalf("%s: a status error moved the App Store watermark", test.name)
			}
		}
	})
}

// A dry run reports what a real run would: past the bound, a dry_run
// unknown_to_store row and no store error, and no watermark. Its rows neither
// count toward the bound nor stand in for the real disposition, which the next
// real run records once.
func TestPaymentReconcileAppleUnknownToStoreDryRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileappleunknowndry", server.NewId())
		transactionId := "2000000000001001"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		appleSeedNotFoundErrors(t, ctx, networkId, transactionId, appleTestPriorNotFoundRuns, appleTestUnknownToStoreReason, false)
		priorWatermark := now.Add(-6 * time.Hour)
		connect.AssertEqual(t, model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark), nil)

		dryResult, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{DryRun: true, Stores: []string{model.SubscriptionMarketApple}},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dryResult.Errors, 0)
		dryEvents := model.GetPaymentReconciliationEvents(ctx, dryResult.RunId)
		connect.AssertEqual(t, len(appleReconcileStatusErrorEvents(dryEvents)), 0)
		connect.AssertEqual(t, len(appleReconcileEventsWithAction(dryEvents, appleTestUnknownToStoreAction)), 1)
		connect.AssertEqual(t, appleCountEvidenceEvents(t, ctx, appleTestUnknownToStoreAction, transactionId, true), 1)
		connect.AssertEqual(t, appleCountEvidenceEvents(t, ctx, appleTestUnknownToStoreAction, transactionId, false), 0)
		watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
		if !ok || !watermark.Equal(priorWatermark) {
			t.Fatalf("a dry run moved the App Store watermark")
		}

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, appleCountEvidenceEvents(t, ctx, appleTestUnknownToStoreAction, transactionId, false), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}

// A recorded disposition never suppresses the store's answer: once the App
// Store knows the transaction again, the renewal is reconciled as usual and is
// no longer counted as unknown.
func TestPaymentReconcileAppleUnknownToStoreYieldsToTheStoreAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileappleunknownyields", server.NewId())
		transactionId := "2000000000001101"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		err := model.AddPaymentReconciliationEvent(ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketApple,
			NetworkId: &networkId,
			Action:    appleTestUnknownToStoreAction,
			Evidence:  transactionId,
			Details:   map[string]any{"leg": "status", "reason": appleTestUnknownToStoreReason},
		})
		connect.AssertEqual(t, err, nil)

		// production now answers: the subscription expired
		env.production.setStatuses(transactionId, appleTestStatuses(t, 2, transactionId, appleTestTransactionClaims(
			t,
			networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)))

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Ended, 1)
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, appleReconcileHeartbeatStoreCount(t, events, appleTestUnknownToStoreAction), 0)
		connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 0)
		connect.AssertEqual(t, appleCountEvidenceEvents(t, ctx, appleTestUnknownToStoreAction, transactionId, false), 1)
	})
}
