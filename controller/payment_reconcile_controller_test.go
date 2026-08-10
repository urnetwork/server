package controller

// Hermetic tests for the hourly payment reconciliation task (UPGRADE.md §8).
// Every store API is a fake httptest backend behind the same seams the S5
// Play webhook tests use; the credential seams default to ABSENT so the tests
// run identically with or without a local vault.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// disableAllReconcileStores pins every store to "no credentials" and restores
// the seams on cleanup. Called first in every test so the suite is hermetic
// even on a machine whose vault carries real store credentials.
func disableAllReconcileStores(t testing.TB) {
	prevStripe := stripeReconcileHasCredentials
	prevApple := appleReconcileHasCredentials
	prevPlay := playReconcileHasCredentials
	prevSolana := solanaReconcileHasCredentials
	stripeReconcileHasCredentials = func() bool { return false }
	appleReconcileHasCredentials = func() bool { return false }
	playReconcileHasCredentials = func() bool { return false }
	solanaReconcileHasCredentials = func() bool { return false }
	t.Cleanup(func() {
		stripeReconcileHasCredentials = prevStripe
		appleReconcileHasCredentials = prevApple
		playReconcileHasCredentials = prevPlay
		solanaReconcileHasCredentials = prevSolana
	})
}

func reconcileTestSession(t testing.TB, ctx context.Context) *session.ClientSession {
	clientId := server.NewId()
	return session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: server.NewId(),
		ClientId:  &clientId,
		UserId:    server.NewId(),
	})
}

func countReconcileEvents(events []*model.PaymentReconciliationEvent, store string, action string) int {
	count := 0
	for _, event := range events {
		if event.Store == store && event.Action == action {
			count += 1
		}
	}
	return count
}

// ----- stripe fake backend -----

type stripeReconcileTestEnv struct {
	// raw invoice objects the GET /v1/invoices listing serves
	listInvoices []map[string]any
	// invoice id -> the expanded object GET /v1/invoices/{id} serves
	fullInvoices map[string]map[string]any
	failList     bool

	testServer *httptest.Server
}

func newStripeReconcileTestEnv(t testing.TB) *stripeReconcileTestEnv {
	env := &stripeReconcileTestEnv{
		fullInvoices: map[string]map[string]any{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		if env.failList {
			http.Error(w, "backend error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data":     env.listInvoices,
			"has_more": false,
		})
	})
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", func(w http.ResponseWriter, r *http.Request) {
		invoice, ok := env.fullInvoices[r.PathValue("invoiceId")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(invoice)
	})
	env.testServer = httptest.NewServer(mux)

	prevBaseUrl := stripeApiBaseUrl
	prevTokenFunc := stripeApiTokenFunc
	prevHasCredentials := stripeReconcileHasCredentials
	stripeApiBaseUrl = env.testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_reconcile" }
	stripeReconcileHasCredentials = func() bool { return true }

	t.Cleanup(func() {
		stripeApiBaseUrl = prevBaseUrl
		stripeApiTokenFunc = prevTokenFunc
		stripeReconcileHasCredentials = prevHasCredentials
		env.testServer.Close()
	})

	return env
}

// the expanded invoice shape BOTH legs read: stripeHandleInvoicePaid
// (StripeInvoiceExpanded: lines + subscription metadata) and the status leg
// (stripeReconcileInvoiceExpanded: subscription status)
func stripeTestFullInvoice(
	invoiceId string,
	subscriptionId string,
	networkId server.Id,
	periodStart time.Time,
	periodEnd time.Time,
	subscriptionStatus string,
	cancelAtPeriodEnd bool,
) map[string]any {
	return map[string]any{
		"id":       invoiceId,
		"status":   "paid",
		"customer": map[string]any{"id": "cus_reconcile", "email": ""},
		"lines": map[string]any{
			"data": []map[string]any{
				{
					"type":         "subscription",
					"subscription": subscriptionId,
					"period": map[string]any{
						"start": periodStart.Unix(),
						"end":   periodEnd.Unix(),
					},
				},
			},
		},
		"subscription": map[string]any{
			"id":                   subscriptionId,
			"status":               subscriptionStatus,
			"cancel_at_period_end": cancelAtPeriodEnd,
			"metadata": map[string]any{
				"network_id": networkId.String(),
			},
		},
	}
}

// ----- apple fake backend -----

type appleReconcileTestEnv struct {
	// original transaction id -> the statuses response the fake API serves
	statuses map[string]*appleSubscriptionStatusesResponse

	testServer *httptest.Server
}

func newAppleReconcileTestEnv(t testing.TB, productIds []string) *appleReconcileTestEnv {
	env := &appleReconcileTestEnv{
		statuses: map[string]*appleSubscriptionStatusesResponse{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /inApps/v1/subscriptions/{transactionId}", func(w http.ResponseWriter, r *http.Request) {
		statuses, ok := env.statuses[r.PathValue("transactionId")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(statuses)
	})
	env.testServer = httptest.NewServer(mux)

	prevBaseUrl := appleAppStoreServerApiBaseUrl
	prevCredentials := appleReconcileCredentialsFunc
	prevAuthHeader := appleServerApiAuthHeaderFunc
	prevHasCredentials := appleReconcileHasCredentials
	appleAppStoreServerApiBaseUrl = env.testServer.URL
	appleReconcileCredentialsFunc = func() *appleServerApiCredentials {
		return &appleServerApiCredentials{
			KeyId:      "TESTKEY",
			IssuerId:   "test-issuer",
			PrivateKey: "unused",
			BundleId:   "network.ur.test",
			ProductIds: productIds,
		}
	}
	appleServerApiAuthHeaderFunc = func(ctx context.Context, header http.Header) {
		header.Add("Authorization", "Bearer test-token")
	}
	appleReconcileHasCredentials = func() bool { return true }

	t.Cleanup(func() {
		appleAppStoreServerApiBaseUrl = prevBaseUrl
		appleReconcileCredentialsFunc = prevCredentials
		appleServerApiAuthHeaderFunc = prevAuthHeader
		appleReconcileHasCredentials = prevHasCredentials
		env.testServer.Close()
	})

	return env
}

// appleTestJws fabricates the JWS shape the App Store Server API serves. Only
// the payload is read (the reconciler pulls it directly from Apple over an
// authenticated channel and does not re-verify the signature), so the header
// and signature are stand-ins.
func appleTestJws(t testing.TB, claims map[string]any) string {
	payload, err := json.Marshal(claims)
	connect.AssertEqual(t, err, nil)
	return fmt.Sprintf(
		"%s.%s.%s",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256"}`)),
		base64.RawURLEncoding.EncodeToString(payload),
		base64.RawURLEncoding.EncodeToString([]byte("sig")),
	)
}

func appleTestTransactionClaims(
	t testing.TB,
	networkId server.Id,
	transactionId string,
	productId string,
	purchaseTime time.Time,
	expiresTime time.Time,
) map[string]any {
	claims := map[string]any{
		"appAccountToken": networkId.String(),
		"transactionId":   transactionId,
		"productId":       productId,
		"purchaseDate":    purchaseTime.UnixMilli(),
		"expiresDate":     expiresTime.UnixMilli(),
		"price":           5000,
	}
	// Round-trip through JSON so the map carries JSON-decoded value types
	// (float64), exactly like every production claims map: the webhook
	// verifier and appleDecodeJwsPayload both produce json.Unmarshal output,
	// and appleControllerInt64Claim accepts float64/json.Number -- NOT the Go
	// int64 a raw fixture literal would carry.
	claimsJson, err := json.Marshal(claims)
	connect.AssertEqual(t, err, nil)
	var normalized map[string]any
	connect.AssertEqual(t, json.Unmarshal(claimsJson, &normalized), nil)
	return normalized
}

// appleTestCredit seeds an already-credited apple subscriber through the SAME
// ledger-gated shape the webhook uses.
func appleTestCredit(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	transactionId string,
	productId string,
	purchaseTime time.Time,
	expiresTime time.Time,
) {
	transaction := &validatedAppleTransaction{
		networkId:     networkId,
		transactionId: transactionId,
		productId:     productId,
		purchaseTime:  purchaseTime,
		expiresTime:   expiresTime,
		netRevenue:    model.UsdToNanoCents(4.0),
	}
	credited := false
	server.Tx(ctx, func(tx server.PgTx) {
		credited = appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), transaction)
	})
	connect.AssertEqual(t, credited, true)
	model.UpdateProNetwork(ctx, networkId)
}

// ----- solana fake RPC -----

type solanaReconcileTestEnv struct {
	// signatures the fake chain knows; everything else answers null
	onChain map[string]bool

	testServer *httptest.Server
}

func newSolanaReconcileTestEnv(t testing.TB) *solanaReconcileTestEnv {
	env := &solanaReconcileTestEnv{
		onChain: map[string]bool{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		if request.Method != "getSignatureStatuses" || len(request.Params) == 0 {
			http.Error(w, "unknown method", http.StatusBadRequest)
			return
		}
		signatures, _ := request.Params[0].([]any)
		value := []any{}
		for _, signature := range signatures {
			if signatureStr, ok := signature.(string); ok && env.onChain[signatureStr] {
				value = append(value, map[string]any{
					"slot":               1,
					"confirmationStatus": "finalized",
				})
			} else {
				value = append(value, nil)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"result":  map[string]any{"value": value},
		})
	})
	env.testServer = httptest.NewServer(mux)

	prevRpcUrl := solanaRpcUrlFunc
	prevHasCredentials := solanaReconcileHasCredentials
	solanaRpcUrlFunc = func() string { return env.testServer.URL }
	solanaReconcileHasCredentials = func() bool { return true }

	t.Cleanup(func() {
		solanaRpcUrlFunc = prevRpcUrl
		solanaReconcileHasCredentials = prevHasCredentials
		env.testServer.Close()
	})

	return env
}

// ----- the tests -----

// TestPaymentReconcileSkipsStoresWithoutCredentials pins §8 principle 5: an
// env with no store credentials (local, test) skips every store with a
// visible audit row and still heartbeats -- never fails.
func TestPaymentReconcileSkipsStoresWithoutCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, len(result.SkippedStores), 4)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		for _, store := range []string{
			model.SubscriptionMarketStripe,
			model.SubscriptionMarketApple,
			model.SubscriptionMarketGoogle,
			model.SubscriptionMarketSolana,
		} {
			connect.AssertEqual(t, countReconcileEvents(events, store, model.PaymentReconcileActionSkippedStore), 1)
			// a skipped store never advances its watermark
			_, ok := model.GetPaymentReconcileWatermark(ctx, store)
			connect.AssertEqual(t, ok, false)
		}
		connect.AssertEqual(t, countReconcileEvents(events, model.PaymentReconcileStoreAll, model.PaymentReconcileActionHeartbeat), 1)
	})
}

// TestPaymentReconcileHeartbeatAndWatermarkOnNoOpRun pins §8 principle 3: a
// run that repairs nothing writes ONLY the heartbeat, and a store that
// completed advances its watermark so the next run's listing is incremental.
func TestPaymentReconcileHeartbeatAndWatermarkOnNoOpRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		newStripeReconcileTestEnv(t)

		before := server.NowUtc()
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, result.Errors, 0)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.PaymentReconcileStoreAll, model.PaymentReconcileActionHeartbeat), 1)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited), 0)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionEnded), 0)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionError), 0)

		// the completed store's watermark advanced to the run time
		watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, watermark.Before(before), false)

		// a second run advances it again
		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		watermark2, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, watermark2.Before(watermark), false)
		_ = result2
	})
}

// TestPaymentReconcileStoreFailureIsolated pins §8 principle 4: one broken
// store is recorded and skipped for the run without starving the others, and
// its watermark does NOT advance (the window is re-examined next run).
func TestPaymentReconcileStoreFailureIsolated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		stripeEnv := newStripeReconcileTestEnv(t)
		stripeEnv.failList = true
		newSolanaReconcileTestEnv(t)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 1)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionError), 1)
		connect.AssertEqual(t, countReconcileEvents(events, model.PaymentReconcileStoreAll, model.PaymentReconcileActionHeartbeat), 1)

		// the failed store's watermark did not advance; the healthy store's did
		_, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, ok, false)
		_, ok = model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, ok, true)
	})
}

// TestPaymentReconcileStripeCreditsMissedInvoice pins the stripe credit
// direction: a paid subscription invoice Stripe knows and our stripe_invoice
// ledger does not is credited THROUGH the webhook path (same network
// resolution, same ledger gate), exactly once across repeated runs.
func TestPaymentReconcileStripeCreditsMissedInvoice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		stripeEnv := newStripeReconcileTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcilestripe1", userId)

		invoiceId := "in_reconcile_missed_1"
		now := server.NowUtc()
		stripeEnv.listInvoices = []map[string]any{
			{"id": invoiceId, "total": 1000},
		}
		stripeEnv.fullInvoices[invoiceId] = stripeTestFullInvoice(
			invoiceId, "sub_reconcile_1", networkId,
			now.Add(-1*time.Hour), now.Add(30*24*time.Hour),
			"active", false,
		)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, result.Errors, 0)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited), 1)

		// the credit landed through the ledger-gated path
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		ledgerNetworkId, credited := model.GetStripeInvoiceNetworkId(ctx, invoiceId)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, ledgerNetworkId, networkId)

		// a second run finds the ledger row and credits nothing new
		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Credited, 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestPaymentReconcileStripeCreditRacesLateWebhook pins §8 principle 1: the
// reconciler credits through the SAME gate the webhook uses, so a reconcile
// racing a late invoice.paid delivery for the same invoice produces exactly
// one credit between them.
func TestPaymentReconcileStripeCreditRacesLateWebhook(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		stripeEnv := newStripeReconcileTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcilestriperace", userId)

		invoiceId := "in_reconcile_race_1"
		now := server.NowUtc()
		invoiceTotal := 1000
		stripeEnv.listInvoices = []map[string]any{
			{"id": invoiceId, "total": invoiceTotal},
		}
		stripeEnv.fullInvoices[invoiceId] = stripeTestFullInvoice(
			invoiceId, "sub_reconcile_race_1", networkId,
			now.Add(-1*time.Hour), now.Add(30*24*time.Hour),
			"active", false,
		)

		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		start := make(chan struct{})
		done := make(chan error, 2)
		go func() {
			<-start
			_, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			done <- err
		}()
		go func() {
			<-start
			// the late webhook delivery of the same invoice
			_, err := stripeHandleInvoicePaid(&StripeEventInvoiceObject{
				Id:    invoiceId,
				Total: invoiceTotal,
			}, webhookSession)
			done <- err
		}()
		close(start)
		// drain BOTH workers before asserting: an assert failure FailNows the
		// attempt goroutine, and TestEnv.Run's teardown (pg pool Close) then
		// blocks on any sibling goroutine still holding a db connection -- the
		// hang is forever because close(done) is deferred behind teardown
		raceErr1 := <-done
		raceErr2 := <-done
		connect.AssertEqual(t, raceErr1, nil)
		connect.AssertEqual(t, raceErr2, nil)

		// exactly one credit between the two paths
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		var totalSubsidy model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COALESCE(SUM(subsidy_net_revenue_nano_cents), 0) FROM transfer_balance WHERE network_id = $1`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&totalSubsidy))
				}
			})
		})
		// the same float expression the credit path uses (stripeHandleInvoicePaid),
		// so the comparison cannot drift by an ulp
		connect.AssertEqual(t, totalSubsidy, model.UsdToNanoCents((1.0-0.3)*float64(invoiceTotal)/100.0))
	})
}

// TestPaymentReconcileStripeEndsRevokedNotCancelAtPeriodEnd pins the stripe
// end direction AND the cancelled ≠ expired rule: a subscription Stripe says
// is already over (canceled) is ended at now, while cancel-at-period-end with
// time remaining -- paid-through time -- is never touched.
func TestPaymentReconcileStripeEndsRevokedNotCancelAtPeriodEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		stripeEnv := newStripeReconcileTestEnv(t)

		now := server.NowUtc()

		// network A: store says the subscription is ALREADY over
		revokedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, revokedNetworkId, "reconcilestripeend1", server.NewId())
		revokedInvoiceId := "in_reconcile_end_1"
		credited, err := stripeCreditInvoicePaid(
			ctx, revokedNetworkId, revokedInvoiceId,
			model.UsdToNanoCents(7.0), now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), true)
		stripeEnv.fullInvoices[revokedInvoiceId] = stripeTestFullInvoice(
			revokedInvoiceId, "sub_reconcile_end_1", revokedNetworkId,
			now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
			"canceled", false,
		)

		// network B: cancel at period end, time remaining -- paid through
		cancelledNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, cancelledNetworkId, "reconcilestripeend2", server.NewId())
		cancelledInvoiceId := "in_reconcile_end_2"
		credited, err = stripeCreditInvoicePaid(
			ctx, cancelledNetworkId, cancelledInvoiceId,
			model.UsdToNanoCents(7.0), now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[cancelledInvoiceId] = stripeTestFullInvoice(
			cancelledInvoiceId, "sub_reconcile_end_2", cancelledNetworkId,
			now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
			"active", true,
		)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.Errors, 0)

		// the revoked network's entitlement is over NOW
		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, revokedNetworkId)), 0)
		active, _ := model.HasSubscriptionRenewal(ctx, revokedNetworkId, model.SubscriptionTypeSupporter)
		connect.AssertEqual(t, active, false)

		// the cancel-at-period-end network keeps its paid-through time
		connect.AssertEqual(t, model.IsProNetwork(ctx, cancelledNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, cancelledNetworkId)), 1)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionEnded), 1)
	})
}

// TestPaymentReconcileAppleCreditsMissedRenewal pins the apple credit
// direction: the store's latest active transaction is unknown to the
// apple_subscription_transaction ledger (a lost DID_RENEW), so the reconciler
// credits it through ProcessAppleNotification's crediting shape -- and a
// racing late notification for the SAME transaction produces exactly one
// credit.
func TestPaymentReconcileAppleCreditsMissedRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		appleEnv := newAppleReconcileTestEnv(t, []string{"supporter_monthly"})

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapple1", server.NewId())

		now := server.NowUtc()

		// the previous period, credited normally, expired an hour ago (well
		// within the reconcile window)
		oldTransactionId := "1000000000000001"
		appleTestCredit(
			t, ctx, networkId, oldTransactionId, "supporter_monthly",
			now.Add(-31*24*time.Hour), now.Add(-1*time.Hour).Add(-SubscriptionGracePeriod),
		)

		// the store's truth: the subscription renewed; the notification was lost
		newTransactionId := "1000000000000002"
		renewalClaims := appleTestTransactionClaims(
			t,
			networkId, newTransactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(29*24*time.Hour),
		)
		storeStatuses := &appleSubscriptionStatusesResponse{
			Data: []*appleSubscriptionGroup{
				{
					LastTransactions: []*appleLastTransaction{
						{
							Status:                1,
							OriginalTransactionId: oldTransactionId,
							SignedTransactionInfo: appleTestJws(t, renewalClaims),
						},
					},
				},
			},
		}
		// any transaction id of the subscription resolves the same statuses --
		// after the repair, the network's newest renewal row carries the new id
		appleEnv.statuses[oldTransactionId] = storeStatuses
		appleEnv.statuses[newTransactionId] = storeStatuses

		// race the reconcile against a late delivery of the DID_RENEW
		// notification for the same transaction
		start := make(chan struct{})
		done := make(chan error, 2)
		go func() {
			<-start
			_, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			done <- err
		}()
		go func() {
			<-start
			_, err := ProcessAppleNotification(
				ctx,
				AppleNotificationDecodedPayload{
					NotificationType: "DID_RENEW",
					NotificationUUID: server.NewId().String(),
					SignedDate:       now.UnixMilli(),
					TransactionInfo:  renewalClaims,
				},
				[]string{"supporter_monthly"},
			)
			done <- err
		}()
		close(start)
		// drain BOTH workers before asserting (see the stripe race test for why
		// asserting mid-drain can hang TestEnv.Run's teardown)
		raceErr1 := <-done
		raceErr2 := <-done
		connect.AssertEqual(t, raceErr1, nil)
		connect.AssertEqual(t, raceErr2, nil)

		// exactly one credit for the renewal transaction
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, newTransactionId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		var renewalCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND transaction_id = $2`,
				networkId,
				newTransactionId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&renewalCount))
				}
			})
		})
		connect.AssertEqual(t, renewalCount, 1)

		// a followup run has nothing left to repair
		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Credited, 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestPaymentReconcileAppleEndsRevoked pins the apple end direction: the
// store says the subscription is revoked (status 5), so the active
// entitlement is ended at now -- while a subscriber in billing grace (status
// 4, still entitled per Apple) is untouched.
func TestPaymentReconcileAppleEndsRevoked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		appleEnv := newAppleReconcileTestEnv(t, []string{"supporter_monthly"})

		now := server.NowUtc()

		// network A: revoked by the store, still active on our side
		revokedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, revokedNetworkId, "reconcileappleend1", server.NewId())
		revokedTransactionId := "2000000000000001"
		appleTestCredit(
			t, ctx, revokedNetworkId, revokedTransactionId, "supporter_monthly",
			now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
		)
		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), true)
		appleEnv.statuses[revokedTransactionId] = &appleSubscriptionStatusesResponse{
			Data: []*appleSubscriptionGroup{
				{
					LastTransactions: []*appleLastTransaction{
						{
							Status:                5,
							OriginalTransactionId: revokedTransactionId,
							SignedTransactionInfo: appleTestJws(t, appleTestTransactionClaims(
								t,
								revokedNetworkId, revokedTransactionId, "supporter_monthly",
								now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
							)),
						},
					},
				},
			},
		}

		// network B: billing grace period -- Apple still grants entitlement
		graceNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, graceNetworkId, "reconcileappleend2", server.NewId())
		graceTransactionId := "2000000000000002"
		appleTestCredit(
			t, ctx, graceNetworkId, graceTransactionId, "supporter_monthly",
			now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
		)
		appleEnv.statuses[graceTransactionId] = &appleSubscriptionStatusesResponse{
			Data: []*appleSubscriptionGroup{
				{
					LastTransactions: []*appleLastTransaction{
						{
							Status:                4,
							OriginalTransactionId: graceTransactionId,
							// the latest transaction is the one already credited
							SignedTransactionInfo: appleTestJws(t, appleTestTransactionClaims(
								t,
								graceNetworkId, graceTransactionId, "supporter_monthly",
								now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
							)),
						},
					},
				},
			},
		}

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, result.Errors, 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, revokedNetworkId)), 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, graceNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, graceNetworkId)), 1)
	})
}

// TestPaymentReconcileGoogleCreditsMissedRenewal pins the google credit
// direction: the store says ACTIVE with a new expiry while our latest renewal
// row lapsed (a lost RTDN), so the reconciler runs the existing
// PlaySubscriptionRenewal path -- advisory lock, in-tx overlap re-check --
// and a repeat run credits nothing new.
func TestPaymentReconcileGoogleCreditsMissedRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		playEnv := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		prevHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = prevHasCredentials })

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileplay1", server.NewId())

		now := server.NowUtc()
		purchaseToken := "play-reconcile-token-1"

		// our side: the last credited period lapsed an hour ago
		err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-31 * 24 * time.Hour),
			EndTime:            now.Add(-1 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(3.5),
			PurchaseToken:      purchaseToken,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
		})
		connect.AssertEqual(t, err, nil)

		// the store's truth: renewed, active until next month
		playEnv.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			now.Add(-31*24*time.Hour),
			now.Add(29*24*time.Hour),
		)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, result.Errors, 0)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketGoogle, model.PaymentReconcileActionCredited), 1)

		// the overlap gate makes a repeat run a no-op
		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Credited, 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestPaymentReconcileGoogleEndsExpiredNotCancelWithTimeRemaining pins the
// google end direction and the cancelled ≠ expired rule: EXPIRED ends the
// active entitlement now; CANCELED with paid-through time remaining is never
// touched.
func TestPaymentReconcileGoogleEndsExpiredNotCancelWithTimeRemaining(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		playEnv := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		prevHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = prevHasCredentials })

		now := server.NowUtc()

		seedActive := func(name string, purchaseToken string) server.Id {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, name, server.NewId())
			playEnv.subscriptions[purchaseToken] = playTestSubscription(
				networkId,
				"supporter_monthly",
				now.Add(-1*24*time.Hour),
				now.Add(29*24*time.Hour),
			)
			webhookSession := session.Testing_CreateClientSession(ctx, nil)
			result, err := PlaySubscriptionRenewal(&PlaySubscriptionRenewalArgs{
				NetworkId:      networkId,
				PackageName:    playPackageNameFunc(),
				SubscriptionId: "supporter_monthly",
				PurchaseToken:  purchaseToken,
			}, webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Renewed, true)
			connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
			return networkId
		}

		// network A: the store now says EXPIRED
		expiredToken := "play-reconcile-expired-1"
		expiredNetworkId := seedActive("reconcileplayend1", expiredToken)
		playEnv.subscriptions[expiredToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"

		// network B: cancelled, but the expiry is still a month out
		cancelledToken := "play-reconcile-cancelled-1"
		cancelledNetworkId := seedActive("reconcileplayend2", cancelledToken)
		playEnv.subscriptions[cancelledToken].SubscriptionState = "SUBSCRIPTION_STATE_CANCELED"

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.Errors, 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, expiredNetworkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, expiredNetworkId)), 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, cancelledNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, cancelledNetworkId)), 1)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketGoogle, model.PaymentReconcileActionEnded), 1)
	})
}

// TestPaymentReconcileSolanaCreditsResolvedUnfulfilled pins the solana credit
// direction: a recorded no_intent payment whose reference NOW resolves to an
// open intent is credited through the intent one-shot -- racing a webhook
// redelivery of the same transaction -- and the unfulfilled record is
// cleared. Exactly one credit either way.
func TestPaymentReconcileSolanaCreditsResolvedUnfulfilled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		newSolanaReconcileTestEnv(t)

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcilesolana1", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "reconcile-solana-ref-1"
		signature := "sig-reconcile-solana-1"
		amountUsd := 5.0

		// the intent exists (recreated / not yet swept) ...
		err := model.CreateSolanaPaymentIntent(reference, amountUsd, model.SolanaPlanMonthly, userSession)
		connect.AssertEqual(t, err, nil)

		// ... and the payment was recorded unfulfilled when it arrived without one
		err = model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature:         signature,
			Reason:              model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd:      amountUsd,
			ReferenceCandidates: []string{"SomeOtherAccountAAAAAAAAAAAAAAAAAAAAAAAAAAA", reference},
		})
		connect.AssertEqual(t, err, nil)

		// race the reconcile against a Helius redelivery of the same transaction
		start := make(chan struct{})
		done := make(chan error, 2)
		go func() {
			<-start
			_, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			done <- err
		}()
		go func() {
			<-start
			_, err := HeliusWebhook(
				[]*SolanaTransaction{solanaTestPayment(reference, signature, amountUsd)},
				webhookSession,
			)
			done <- err
		}()
		close(start)
		// drain BOTH workers before asserting (see the stripe race test for why
		// asserting mid-drain can hang TestEnv.Run's teardown)
		raceErr1 := <-done
		raceErr2 := <-done
		connect.AssertEqual(t, raceErr1, nil)
		connect.AssertEqual(t, raceErr2, nil)

		// exactly one credit between the two paths
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, model.IsSolanaPaymentCompleted(ctx, signature), true)

		// the record is resolved whichever path won the race; a followup run
		// (the reconciler may have lost the race before clearing) removes it
		RunPaymentReconciliation(reconcileTestSession(t, ctx))
		_, err = model.GetUnfulfilledSolanaPayment(ctx, signature)
		connect.AssertNotEqual(t, err, nil)
	})
}

// TestPaymentReconcileDryRunMakesNoWrites pins the dry-run contract on the
// stripe reconciler, both directions: the store reads happen for real, every
// write is suppressed -- no credit, no ended entitlement, no watermark
// advance -- and each suppressed repair is a would_credit/would_end audit row
// tagged dry_run. A real run through the SAME entry point then performs
// exactly the repairs the dry run predicted.
func TestPaymentReconcileDryRunMakesNoWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		stripeEnv := newStripeReconcileTestEnv(t)

		now := server.NowUtc()

		// network A: a paid invoice the webhook never delivered (credit
		// direction)
		missedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, missedNetworkId, "reconciledryrun1", server.NewId())
		missedInvoiceId := "in_reconcile_dryrun_1"
		stripeEnv.listInvoices = []map[string]any{
			{"id": missedInvoiceId, "total": 1000},
		}
		stripeEnv.fullInvoices[missedInvoiceId] = stripeTestFullInvoice(
			missedInvoiceId, "sub_reconcile_dryrun_1", missedNetworkId,
			now.Add(-1*time.Hour), now.Add(30*24*time.Hour),
			"active", false,
		)

		// network B: credited on our side, the store says already over (end
		// direction)
		revokedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, revokedNetworkId, "reconciledryrun2", server.NewId())
		revokedInvoiceId := "in_reconcile_dryrun_2"
		credited, err := stripeCreditInvoicePaid(
			ctx, revokedNetworkId, revokedInvoiceId,
			model.UsdToNanoCents(7.0), now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[revokedInvoiceId] = stripeTestFullInvoice(
			revokedInvoiceId, "sub_reconcile_dryrun_2", revokedNetworkId,
			now.Add(-24*time.Hour), now.Add(29*24*time.Hour),
			"canceled", false,
		)

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{DryRun: true},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.DryRun, true)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.Errors, 0)

		// no credit landed: no balance, no pro, no stripe_invoice ledger row
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, missedNetworkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, missedNetworkId), false)
		_, invoiceCredited := model.GetStripeInvoiceNetworkId(ctx, missedInvoiceId)
		connect.AssertEqual(t, invoiceCredited, false)

		// no entitlement ended: network B keeps its balance and pro state
		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, revokedNetworkId)), 1)

		// the watermark did not advance: the dry run must not eat the
		// incremental window the later real run needs
		_, watermarkSet := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, watermarkSet, false)

		// the suppressed repairs are durable audit rows, tagged dry_run, with
		// the evidence the real repair would carry
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldCredit), 1)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldEnd), 1)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited), 0)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionEnded), 0)
		for _, event := range events {
			connect.AssertEqual(t, event.DryRun, true)
			switch event.Action {
			case model.PaymentReconcileActionWouldCredit:
				connect.AssertEqual(t, event.Evidence, missedInvoiceId)
				connect.AssertEqual(t, *event.NetworkId, missedNetworkId)
			case model.PaymentReconcileActionWouldEnd:
				connect.AssertEqual(t, *event.NetworkId, revokedNetworkId)
			}
		}

		// per-store tallies for the CLI summary
		storeResult := result.StoreResults[model.SubscriptionMarketStripe]
		connect.AssertNotEqual(t, storeResult, nil)
		connect.AssertEqual(t, storeResult.Credited, 1)
		connect.AssertEqual(t, storeResult.Ended, 1)
		connect.AssertEqual(t, 0 < storeResult.Examined, true)

		// a real run through the same entry point performs exactly what the
		// dry run predicted
		realResult, err := RunPaymentReconciliationWithOptions(reconcileTestSession(t, ctx), nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, realResult.DryRun, false)
		connect.AssertEqual(t, realResult.Credited, 1)
		connect.AssertEqual(t, realResult.Ended, 1)
		connect.AssertEqual(t, realResult.Errors, 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, missedNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, missedNetworkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, revokedNetworkId)), 0)
		_, watermarkSet = model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, watermarkSet, true)

		// the real run's events are not tagged dry_run
		for _, event := range model.GetPaymentReconciliationEvents(ctx, realResult.RunId) {
			connect.AssertEqual(t, event.DryRun, false)
		}
	})
}

// TestPaymentReconcileDryRunSolanaLeavesUnfulfilled pins the dry-run contract
// on the solana reconciler: a resolvable unfulfilled payment is reported as
// would_credit but neither credited nor cleared, and the later real run still
// finds and repairs it.
func TestPaymentReconcileDryRunSolanaLeavesUnfulfilled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		newSolanaReconcileTestEnv(t)

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconciledryrunsolana1", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		reference := "reconcile-dryrun-solana-ref-1"
		signature := "sig-reconcile-dryrun-solana-1"
		err := model.CreateSolanaPaymentIntent(reference, 5.0, model.SolanaPlanMonthly, userSession)
		connect.AssertEqual(t, err, nil)
		err = model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature:         signature,
			Reason:              model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd:      5.0,
			ReferenceCandidates: []string{reference},
		})
		connect.AssertEqual(t, err, nil)

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{DryRun: true},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, result.Errors, 0)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketSolana, model.PaymentReconcileActionWouldCredit), 1)

		// nothing was credited and the unfulfilled record is still there for
		// the real run
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, model.IsSolanaPaymentCompleted(ctx, signature), false)
		_, err = model.GetUnfulfilledSolanaPayment(ctx, signature)
		connect.AssertEqual(t, err, nil)

		// the real run repairs what the dry run predicted
		realResult, err := RunPaymentReconciliationWithOptions(reconcileTestSession(t, ctx), nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, realResult.Credited, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, model.IsSolanaPaymentCompleted(ctx, signature), true)
		_, err = model.GetUnfulfilledSolanaPayment(ctx, signature)
		connect.AssertNotEqual(t, err, nil)
	})
}

// TestPaymentReconcileStoreFilter pins the --store targeting: a filtered run
// touches ONLY the named store -- the others get no events (not even
// skipped_store) and no watermark.
func TestPaymentReconcileStoreFilter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		newStripeReconcileTestEnv(t)
		newSolanaReconcileTestEnv(t)

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		// no skipped_store rows at all: apple/google were filtered out, not
		// skipped
		connect.AssertEqual(t, len(result.SkippedStores), 0)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		for _, event := range events {
			if event.Store != model.SubscriptionMarketStripe && event.Store != model.PaymentReconcileStoreAll {
				t.Errorf("filtered run touched store %s (action %s)", event.Store, event.Action)
			}
		}

		// only the filtered store's watermark advanced
		_, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, ok, true)
		_, ok = model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, ok, false)
	})
}

// TestPaymentReconcileRunLockExcludesConcurrentRuns pins the CLI-vs-task
// mutual exclusion: while another session holds the run-level advisory lock,
// a real run reports busy instead of interleaving -- and a dry run (which
// writes nothing) still works. After release the real run proceeds.
func TestPaymentReconcileRunLockExcludesConcurrentRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)

		server.Db(ctx, func(conn server.PgConn) {
			locked := false
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`,
				paymentReconcileRunLockKey,
			).Scan(&locked))
			connect.AssertEqual(t, locked, true)
			defer conn.Exec(
				ctx,
				`SELECT pg_advisory_unlock(hashtextextended($1, 0))`,
				paymentReconcileRunLockKey,
			)

			// a real run must not interleave with the lock holder
			_, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, errors.Is(err, ErrPaymentReconcileRunInProgress), true)

			// a dry run writes nothing and runs lock-free
			dryResult, err := RunPaymentReconciliationWithOptions(
				reconcileTestSession(t, ctx),
				&PaymentReconcileRunOptions{DryRun: true},
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, dryResult.DryRun, true)
		}, server.OptNoRetry())

		// lock released: the real run proceeds
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.SkippedStores), 4)
	})
}

// TestPaymentReconcileSolanaEndsVanishedPayment pins the solana end
// direction: a credited transaction Helius' full-history lookup cannot find
// never landed on-chain -- entitlement without payment -- and is ended, while
// a payment that IS on-chain is untouched.
func TestPaymentReconcileSolanaEndsVanishedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		solanaEnv := newSolanaReconcileTestEnv(t)

		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		seedCredited := func(name string, reference string, signature string) server.Id {
			networkId := server.NewId()
			clientId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, name, userId)
			userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: networkId,
				ClientId:  &clientId,
				UserId:    userId,
			})
			err := model.CreateSolanaPaymentIntent(reference, 5.0, model.SolanaPlanMonthly, userSession)
			connect.AssertEqual(t, err, nil)
			result, err := HeliusWebhook(
				[]*SolanaTransaction{solanaTestPayment(reference, signature, 5.0)},
				webhookSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Message, "Processed 1 matching payments")
			connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

			// push the credit past the finality grace so the reconciler
			// re-verifies it
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					UPDATE subscription_renewal
					SET start_time = start_time - interval '2 hours'
					WHERE network_id = $1 AND market = $2
					`,
					networkId,
					model.SubscriptionMarketSolana,
				))
			})
			return networkId
		}

		// network A: the credited signature does not exist on-chain
		vanishedNetworkId := seedCredited("reconcilesolanaend1", "reconcile-vanish-ref-1", "sig-reconcile-vanished-1")

		// network B: the credited signature is on-chain and finalized
		healthyNetworkId := seedCredited("reconcilesolanaend2", "reconcile-healthy-ref-1", "sig-reconcile-healthy-1")
		solanaEnv.onChain["sig-reconcile-healthy-1"] = true

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.Errors, 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, vanishedNetworkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, vanishedNetworkId)), 0)

		connect.AssertEqual(t, model.IsProNetwork(ctx, healthyNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, healthyNetworkId)), 1)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketSolana, model.PaymentReconcileActionEnded), 1)
	})
}
