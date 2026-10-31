package controller

// apple.yml accepts Sandbox notifications on Main, so TestFlight and App Review
// purchases are credited through the same App Store ledger gate as real ones.
// Until 2026-10 each recorded the price as revenue on its renewal and its Pro
// balance and posted a sales notice. A sandbox purchase moves no money: the
// tester keeps Pro through the same idempotent gate, with zero revenue and no
// notice. A production purchase is credited exactly as before. The
// subscription details lookup asks the sandbox for a TestFlight subscription
// the way the payment reconciler does.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var appleSandboxRevenueProductIds = []string{"supporter_monthly_26", "supporter_yearly_26"}

// appleSandboxRevenueClaims are verified transaction claims for a $4.99
// monthly purchase. TestFlight renews every 24 hours, so the period is a day.
// An empty environment leaves the claim out, as older payloads do.
func appleSandboxRevenueClaims(networkId server.Id, transactionId string, environment string, now time.Time) map[string]any {
	claims := map[string]any{
		"appAccountToken":       networkId.String(),
		"transactionId":         transactionId,
		"originalTransactionId": transactionId,
		"transactionReason":     "PURCHASE",
		"productId":             "supporter_monthly_26",
		"purchaseDate":          float64(now.Add(-1 * time.Hour).UnixMilli()),
		"expiresDate":           float64(now.Add(23 * time.Hour).UnixMilli()),
		"price":                 float64(4990),
		"currency":              "USD",
	}
	if environment != "" {
		claims["environment"] = environment
	}
	return claims
}

// appleCreditRows are the renewal and Pro balance one App Store credit wrote.
type appleCreditRows struct {
	renewals          int
	renewalNetRevenue model.NanoCents
	balances          int
	balanceSubsidy    model.NanoCents
	balanceNetRevenue model.NanoCents
	paid              bool
	pro               bool
}

func readAppleCreditRows(t testing.TB, ctx context.Context, networkId server.Id, transactionId string) *appleCreditRows {
	t.Helper()
	rows := &appleCreditRows{}
	server.Db(ctx, func(conn server.PgConn) {
		var startTime, endTime time.Time
		err := conn.QueryRow(
			ctx,
			`
			SELECT count(*) OVER (), net_revenue_nano_cents, start_time, end_time
			FROM subscription_renewal
			WHERE network_id = $1 AND market = $2 AND transaction_id = $3
			`,
			networkId,
			model.SubscriptionMarketApple,
			transactionId,
		).Scan(&rows.renewals, &rows.renewalNetRevenue, &startTime, &endTime)
		server.Raise(err)
		err = conn.QueryRow(
			ctx,
			`
			SELECT count(*) OVER (), subsidy_net_revenue_nano_cents, net_revenue_nano_cents, paid, pro
			FROM transfer_balance
			WHERE network_id = $1 AND start_time = $2 AND end_time = $3
			`,
			networkId,
			startTime,
			endTime,
		).Scan(&rows.balances, &rows.balanceSubsidy, &rows.balanceNetRevenue, &rows.paid, &rows.pro)
		server.Raise(err)
	})
	return rows
}

// requireSandboxCredit: one renewal and one Pro balance in the window, both
// without revenue, and the network is Pro.
func requireSandboxCredit(t testing.TB, ctx context.Context, networkId server.Id, transactionId string) {
	t.Helper()
	rows := readAppleCreditRows(t, ctx, networkId, transactionId)
	if rows.renewals != 1 || rows.balances != 1 {
		t.Fatalf("sandbox credit wrote %d renewals and %d balances, want one each", rows.renewals, rows.balances)
	}
	if rows.renewalNetRevenue != 0 || rows.balanceSubsidy != 0 || rows.balanceNetRevenue != 0 {
		t.Fatalf("sandbox credit recorded revenue: renewal=%d subsidy=%d balance=%d, want zero",
			rows.renewalNetRevenue, rows.balanceSubsidy, rows.balanceNetRevenue)
	}
	if !rows.pro || rows.paid {
		t.Fatalf("sandbox balance pro=%t paid=%t, want a Pro balance whose generated paid column is false", rows.pro, rows.paid)
	}
	connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, transactionId), true)
	connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
}

// requireProductionCredit: the revenue the shared validator computes, on the
// renewal and as the balance's subsidy, exactly as before sandbox handling.
func requireProductionCredit(t testing.TB, ctx context.Context, networkId server.Id, transactionId string, claims map[string]any, now time.Time) {
	t.Helper()
	validated, err := validateAppleTransaction(AppleNotificationDecodedPayload{
		SignedDate:      now.UnixMilli(),
		TransactionInfo: claims,
	}, appleSandboxRevenueProductIds, true)
	connect.AssertEqual(t, err, nil)
	want := model.NanoCents(float64(model.UsdToNanoCents(4.99)) * 0.8)
	connect.AssertEqual(t, validated.netRevenue, want)

	rows := readAppleCreditRows(t, ctx, networkId, transactionId)
	if rows.renewals != 1 || rows.balances != 1 {
		t.Fatalf("production credit wrote %d renewals and %d balances, want one each", rows.renewals, rows.balances)
	}
	if rows.renewalNetRevenue != want || rows.balanceSubsidy != want || rows.balanceNetRevenue != 0 {
		t.Fatalf("production credit revenue renewal=%d subsidy=%d balance=%d, want %d, %d, 0",
			rows.renewalNetRevenue, rows.balanceSubsidy, rows.balanceNetRevenue, want, want)
	}
	if !rows.pro || !rows.paid {
		t.Fatalf("production balance pro=%t paid=%t, want a paid Pro balance", rows.pro, rows.paid)
	}
	connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
}

func appleSandboxRevenueNotification(claims map[string]any, now time.Time) AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: "SUBSCRIBED",
		NotificationUUID: server.NewId().String(),
		SignedDate:       now.UnixMilli(),
		TransactionInfo:  claims,
	}
}

// The webhook credits a TestFlight purchase: Pro through the ledger gate, no
// revenue, no sales notice, and a redelivery adds nothing.
func TestAppleSandboxNotificationCreditsProWithoutRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices, restore := model.Testing_CaptureSalesPaymentNotices()
		t.Cleanup(restore)
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "applesandboxwebhook", server.NewId())
		transactionId := "2000000000000801"
		notification := appleSandboxRevenueNotification(appleSandboxRevenueClaims(networkId, transactionId, "Sandbox", now), now)

		processed, err := ProcessAppleNotification(ctx, notification, appleSandboxRevenueProductIds)
		if err != nil || !processed {
			t.Fatalf("the sandbox purchase was not credited: processed=%t err=%v", processed, err)
		}
		requireSandboxCredit(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, len(notices()), 0)

		processed, err = ProcessAppleNotification(ctx, notification, appleSandboxRevenueProductIds)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		requireSandboxCredit(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, len(notices()), 0)
	})
}

// A production purchase, with or without the environment claim, is credited
// exactly as before: the validated revenue on both rows, a paid Pro balance
// and one sales notice.
func TestAppleProductionNotificationRevenueUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices, restore := model.Testing_CaptureSalesPaymentNotices()
		t.Cleanup(restore)
		now := server.NowUtc().Truncate(time.Millisecond)

		for i, environment := range []string{"Production", ""} {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "appleproductionwebhook"+string(rune('a'+i)), server.NewId())
			transactionId := "15000000000080" + string(rune('1'+i))
			claims := appleSandboxRevenueClaims(networkId, transactionId, environment, now)

			processed, err := ProcessAppleNotification(ctx, appleSandboxRevenueNotification(claims, now), appleSandboxRevenueProductIds)
			if err != nil || !processed {
				t.Fatalf("environment %q: the purchase was not credited: processed=%t err=%v", environment, processed, err)
			}
			requireProductionCredit(t, ctx, networkId, transactionId, claims, now)

			got := notices()
			if len(got) != i+1 || got[i].RecordKey != "apple_transaction/"+transactionId {
				t.Fatalf("environment %q: %d sales notices, want one more for the purchase", environment, len(got))
			}
		}
	})
}

// The reconciler's credit leg shares the gate: a sandbox transaction keeps Pro
// without revenue or a notice, and a production one records its revenue and
// posts once.
func TestAppleReconcileCreditLegSandboxWithoutRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices, restore := model.Testing_CaptureSalesPaymentNotices()
		t.Cleanup(restore)
		now := server.NowUtc().Truncate(time.Millisecond)

		sandboxNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, sandboxNetworkId, "applesandboxreconcile", server.NewId())
		sandboxTransactionId := "2000000000000901"
		credited, networkId, err := appleReconcileCreditTransaction(
			ctx,
			appleSandboxRevenueClaims(sandboxNetworkId, sandboxTransactionId, "Sandbox", now),
			appleSandboxRevenueProductIds,
			false,
		)
		if err != nil || !credited || networkId != sandboxNetworkId {
			t.Fatalf("the reconciler did not credit the sandbox transaction: credited=%t err=%v", credited, err)
		}
		requireSandboxCredit(t, ctx, sandboxNetworkId, sandboxTransactionId)
		connect.AssertEqual(t, len(notices()), 0)

		productionNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, productionNetworkId, "appleproductionreconcile", server.NewId())
		productionTransactionId := "150000000000901"
		productionClaims := appleSandboxRevenueClaims(productionNetworkId, productionTransactionId, "Production", now)
		credited, networkId, err = appleReconcileCreditTransaction(ctx, productionClaims, appleSandboxRevenueProductIds, false)
		if err != nil || !credited || networkId != productionNetworkId {
			t.Fatalf("the reconciler did not credit the production transaction: credited=%t err=%v", credited, err)
		}
		requireProductionCredit(t, ctx, productionNetworkId, productionTransactionId, productionClaims, now)
		got := notices()
		if len(got) != 1 || got[0].RecordKey != "apple_transaction/"+productionTransactionId {
			t.Fatalf("%d sales notices after the production credit, want exactly its one", len(got))
		}
	})
}

// The shared gate, without a database: a sandbox credit posts no notice and
// its ledger row still gates a second credit; a production credit posts once.
func TestAppleSandboxCreditPostsNoSalesNotice(t *testing.T) {
	notices := captureSalesNotices(t)
	ctx := context.Background()
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)

	sandbox := appleSalesTransaction(networkId, "2000000000001001")
	sandbox.environment = "Sandbox"
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), sandbox), true)
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), sandbox), false)
	connect.AssertEqual(t, len(notices()), 0)

	production := appleSalesTransaction(networkId, "1000")
	production.environment = "Production"
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), production), true)
	got := notices()
	connect.AssertEqual(t, len(got), 1)
	connect.AssertEqual(t, got[0].RecordKey, "apple_transaction/1000")
}

// The validator reads the environment claim, and only Sandbox zeroes the
// credited revenue. The validated revenue itself is unchanged.
func TestAppleTransactionEnvironmentDecidesCreditRevenue(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	networkId := server.NewId()
	want := model.NanoCents(float64(model.UsdToNanoCents(4.99)) * 0.8)
	for _, test := range []struct {
		environment string
		credit      model.NanoCents
	}{
		{environment: "Sandbox", credit: 0},
		{environment: "Production", credit: want},
		{environment: "", credit: want},
	} {
		validated, err := validateAppleTransaction(AppleNotificationDecodedPayload{
			SignedDate:      now.UnixMilli(),
			TransactionInfo: appleSandboxRevenueClaims(networkId, "1000", test.environment, now),
		}, appleSandboxRevenueProductIds, true)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validated.environment, test.environment)
		connect.AssertEqual(t, validated.netRevenue, want)
		connect.AssertEqual(t, validated.sandbox(), test.environment == "Sandbox")
		connect.AssertEqual(t, validated.creditNetRevenue(), test.credit)
	}
}

// ----- subscription details -----

// A TestFlight subscription's details come from the sandbox after production
// answers 4040010, where apple.yml accepts Sandbox.
func TestAppleLookupSubscriptionStateAsksSandboxForTestFlight(t *testing.T) {
	env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
	now := server.NowUtc().Truncate(time.Millisecond)
	transactionId := "2000000000001101"
	env.sandbox.setStatuses(transactionId, appleTestActiveSandboxStatuses(t, server.NewId(), transactionId, now))

	state, err := appleLookupSubscriptionState(t.Context(), transactionId)
	if err != nil || state == nil {
		t.Fatalf("the TestFlight subscription had no state: state=%+v err=%v", state, err)
	}
	connect.AssertEqual(t, state.Active, true)
	if state.EndTime == nil || !state.EndTime.Equal(now.Add(23*time.Hour)) {
		t.Fatalf("the TestFlight subscription ends at %v, want the sandbox expiry", state.EndTime)
	}
	connect.AssertEqual(t, env.production.requestCount(transactionId), 1)
	connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 1)
}

// Without Sandbox accepted, a production 4040010 stays the lookup's error and
// the sandbox is never asked.
func TestAppleLookupSubscriptionStateSandboxRequiresAcceptedEnvironment(t *testing.T) {
	env := newAppleSandboxReconcileTestEnv(t, "Production")
	now := server.NowUtc().Truncate(time.Millisecond)
	transactionId := "2000000000001201"
	env.sandbox.setStatuses(transactionId, appleTestActiveSandboxStatuses(t, server.NewId(), transactionId, now))

	state, err := appleLookupSubscriptionState(t.Context(), transactionId)
	if err == nil || state != nil || !strings.Contains(err.Error(), "4040010") || strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("lookup without Sandbox accepted: state=%+v err=%v, want the production 4040010", state, err)
	}
	connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 0)
}

// Every other production failure stays the lookup's error without a sandbox
// request.
func TestAppleLookupSubscriptionStateSandboxOnlyForTransactionIdNotFound(t *testing.T) {
	env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
	now := server.NowUtc().Truncate(time.Millisecond)
	transactionId := "2000000000001301"
	env.sandbox.setStatuses(transactionId, appleTestActiveSandboxStatuses(t, server.NewId(), transactionId, now))
	env.production.setAnswer(transactionId, http.StatusInternalServerError, `{"errorCode":5000001,"errorMessage":"An unknown error occurred. Please try again."}`)

	state, err := appleLookupSubscriptionState(t.Context(), transactionId)
	if err == nil || state != nil || strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("lookup after a production outage: state=%+v err=%v, want the production error", state, err)
	}
	connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 0)
}

// A sandbox miss is the lookup's error, naming the sandbox.
func TestAppleLookupSubscriptionStateSandboxMissIsAnError(t *testing.T) {
	env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
	transactionId := "2000000000001401"

	state, err := appleLookupSubscriptionState(t.Context(), transactionId)
	if err == nil || state != nil || !strings.Contains(err.Error(), "App Store sandbox: ") || !strings.Contains(err.Error(), "4040010") {
		t.Fatalf("lookup missing in both environments: state=%+v err=%v, want the sandbox's 4040010", state, err)
	}
	connect.AssertEqual(t, env.production.requestCount(transactionId), 1)
	connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 1)
}
