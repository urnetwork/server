// Provider status parsing must never turn missing or unsupported authority
// into an entitlement end, in either reconciliation or subscription details.
package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Serves exact JSON so omitted fields and null members reach the actual decoder.
func appleStatusResponseFixture(t testing.TB) func(string) {
	t.Helper()
	newAppleReconcileTestEnv(t, []string{"supporter_monthly"})
	var response atomic.Value
	response.Store(`{"data":[]}`)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response.Load().(string))
	}))
	previousUrl := appleAppStoreServerApiBaseUrl
	appleAppStoreServerApiBaseUrl = backend.URL
	t.Cleanup(func() {
		appleAppStoreServerApiBaseUrl = previousUrl
		backend.Close()
	})
	return func(body string) { response.Store(body) }
}

// Covers omitted and unsupported statuses, plus incomplete response members.
func invalidAppleStatusResponses() []struct{ name, body string } {
	return []struct{ name, body string }{
		{name: "omitted", body: `{"data":[{"lastTransactions":[{}]}]}`},
		{name: "zero", body: `{"data":[{"lastTransactions":[{"status":0}]}]}`},
		{name: "unknown", body: `{"data":[{"lastTransactions":[{"status":91}]}]}`},
		{name: "negative", body: `{"data":[{"lastTransactions":[{"status":-1}]}]}`},
		{name: "null response", body: `null`},
		{name: "null group", body: `{"data":[null]}`},
		{name: "null transaction", body: `{"data":[{"lastTransactions":[null]}]}`},
		{name: "terminal with unknown", body: `{"data":[{"lastTransactions":[{"status":2},{"status":91}]}]}`},
		{name: "entitled before unknown", body: `{"data":[{"lastTransactions":[{"status":1}]},{"lastTransactions":[{"status":91}]}]}`},
	}
}

// Runs the financial owner, including its provider HTTP decoding and watermark.
func checkAppleStatusRefusal(t *testing.T, dryRun bool) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(tb)()
		setResponse := appleStatusResponseFixture(tb)
		newSolanaReconcileTestEnv(tb)
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-apple-status", server.NewId())
		const transactionId = "synthetic-apple-status-transaction"
		expires := now.Add(29 * 24 * time.Hour)
		appleTestCredit(tb, ctx, networkId, transactionId, "supporter_monthly", now.Add(-24*time.Hour), expires)
		priorWatermark := now.Add(-6 * time.Hour)
		server.Raise(model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark))
		for _, test := range invalidAppleStatusResponses() {
			setResponse(test.body)
			result, err := RunPaymentReconciliationWithOptions(reconcileTestSession(tb, ctx), &PaymentReconcileRunOptions{DryRun: dryRun})
			if err != nil || result == nil {
				tb.Fatalf("%s: reconciliation failed outside the store boundary: %v", test.name, err)
			}
			if result.Ended != 0 || result.Credited != 0 || result.EntitlementsRepaired != 0 || !model.IsProNetwork(ctx, networkId) {
				tb.Fatalf("%s: unsupported provider status ended or changed a paid entitlement: ended=%d credited=%d repaired=%d", test.name, result.Ended, result.Credited, result.EntitlementsRepaired)
			}
			if result.Errors != 1 {
				tb.Fatalf("%s: invalid status must remain one store error, got %d", test.name, result.Errors)
			}
			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			if !ok || !watermark.Equal(priorWatermark) {
				tb.Fatalf("%s: invalid status advanced the successful Apple watermark", test.name)
			}
			renewals := model.GetReconcileSubscriptionRenewals(ctx, model.SubscriptionMarketApple, now.Add(-time.Hour), 10)
			if len(renewals) != 1 || !renewals[0].EndTime.Equal(expires.Add(SubscriptionGracePeriod)) || len(model.GetActiveTransferBalances(ctx, networkId)) != 1 {
				tb.Fatalf("%s: invalid status changed the paid renewal or its balance", test.name)
			}
			events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
			statusErrors := 0
			for _, event := range events {
				if event.Store == model.SubscriptionMarketApple && event.Action == model.PaymentReconcileActionError && event.Details["leg"] == "status" && event.DryRun == dryRun {
					statusErrors++
				}
			}
			if statusErrors != 1 {
				tb.Fatalf("%s: malformed provider response was not audited at the status boundary", test.name)
			}
			if !dryRun {
				if _, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketSolana); !ok {
					tb.Fatalf("%s: Apple refusal prevented the healthy sibling store from completing", test.name)
				}
			}
		}
	})
}

// Unknown provider status cannot authorize revoking a paid local subscription.
func TestPaymentReconcileAppleInvalidStatusPreservesEntitlement(t *testing.T) {
	checkAppleStatusRefusal(t, false)
}

// Dry-run reports the same refusal, rather than proposing an unsafe would_end.
func TestPaymentReconcileAppleInvalidStatusDryRunRefusesEnd(t *testing.T) {
	checkAppleStatusRefusal(t, true)
}

// The adjacent HTTP consumer must expose unavailable status, not inactive state.
func TestAppleSubscriptionLookupRefusesInvalidStatus(t *testing.T) {
	setResponse := appleStatusResponseFixture(t)
	for _, test := range invalidAppleStatusResponses() {
		setResponse(test.body)
		state, err := appleLookupSubscriptionState(t.Context(), "synthetic-apple-status-transaction")
		if err == nil || state != nil {
			t.Fatalf("%s: invalid provider evidence became a subscription state: state=%+v error=%v", test.name, state, err)
		}
	}
}

// Expired and billing-retry remain affirmative terminal evidence; revoked and
// provider grace are covered by TestPaymentReconcileAppleEndsRevoked.
func TestPaymentReconcileAppleKnownTerminalStatusStillEnds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(tb)()
		setResponse := appleStatusResponseFixture(tb)
		now := server.NowUtc().Truncate(time.Millisecond)
		for _, status := range []int{2, 3} {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-terminal-%d", status), server.NewId())
			appleTestCredit(tb, ctx, networkId, fmt.Sprintf("synthetic-terminal-transaction-%d", status), "supporter_monthly", now.Add(-24*time.Hour), now.Add(29*24*time.Hour))
			setResponse(fmt.Sprintf(`{"data":[{"lastTransactions":[{"status":%d}]}]}`, status))
			result, err := RunPaymentReconciliation(reconcileTestSession(tb, ctx))
			if err != nil || result.Ended != 1 || result.Errors != 0 || model.IsProNetwork(ctx, networkId) {
				tb.Fatalf("known terminal status %d no longer ends its active entitlement: result=%+v error=%v", status, result, err)
			}
		}
	})
}
