package controller

// Tests for the currency of App Store prices. A transaction's price is in
// milliunits of its own ISO 4217 currency, and net revenue is USD: only a USD
// price converts. A price in another currency records the paid marker, with
// the entitlement and the balance grant unchanged, and the transaction ledger
// keeps its price and currency. The non-USD fixtures failed before the fix,
// which read every price as USD (KRW 3,300 recorded $2,640 of net revenue).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const appleCurrencyTestProductId = "supporter_monthly_26"

// the paid marker: a balance that keeps its entitlement but no USD revenue
var appleCurrencyTestMarker = model.UsdToNanoCents(0.01)

// The transaction claims of a 30-day subscription priced in currency, with
// the JSON-decoded value types the verifier produces. An empty currency omits
// the claim.
func appleCurrencyTestClaims(
	networkId server.Id,
	transactionId string,
	priceMilliunits int64,
	currency string,
	now time.Time,
) map[string]any {
	claims := map[string]any{
		"appAccountToken": networkId.String(),
		"transactionId":   transactionId,
		"productId":       appleCurrencyTestProductId,
		"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
		"expiresDate":     float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
		"price":           float64(priceMilliunits),
	}
	if currency != "" {
		claims["currency"] = currency
	}
	return claims
}

func appleCurrencyTestNotification(claims map[string]any, now time.Time) AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: "SUBSCRIBED",
		Subtype:          "INITIAL_BUY",
		NotificationUUID: server.NewId().String(),
		SignedDate:       now.UnixMilli(),
		TransactionInfo:  claims,
	}
}

// The revenue a credit recorded: its renewal's net revenue, and its one pro
// balance.
func appleCurrencyTestRevenue(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	transactionId string,
) (renewalNetRevenue model.NanoCents, balance *model.TransferBalance) {
	renewalCount := 0
	balances := []*model.TransferBalance{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT net_revenue_nano_cents
				FROM subscription_renewal
				WHERE network_id = $1 AND market = $2 AND transaction_id = $3
			`,
			networkId,
			model.SubscriptionMarketApple,
			transactionId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				server.Raise(result.Scan(&renewalNetRevenue))
				renewalCount += 1
			}
		})

		result, err = conn.Query(
			ctx,
			`
				SELECT
					start_time,
					end_time,
					start_balance_byte_count,
					balance_byte_count,
					net_revenue_nano_cents,
					subsidy_net_revenue_nano_cents,
					paid,
					pro
				FROM transfer_balance
				WHERE network_id = $1 AND pro
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				transferBalance := &model.TransferBalance{NetworkId: networkId}
				server.Raise(result.Scan(
					&transferBalance.StartTime,
					&transferBalance.EndTime,
					&transferBalance.StartBalanceByteCount,
					&transferBalance.BalanceByteCount,
					&transferBalance.NetRevenue,
					&transferBalance.SubsidyNetRevenue,
					&transferBalance.Paid,
					&transferBalance.Pro,
				))
				balances = append(balances, transferBalance)
			}
		})
	})
	connect.AssertEqual(t, renewalCount, 1)
	connect.AssertEqual(t, len(balances), 1)
	return renewalNetRevenue, balances[0]
}

// The price and currency the transaction ledger kept. A NULL currency reads
// as "".
func appleCurrencyTestLedger(
	t testing.TB,
	ctx context.Context,
	transactionId string,
) (priceMilliunits int64, currency string) {
	rowCount := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT price_milliunits, COALESCE(currency, '')
				FROM apple_subscription_transaction
				WHERE transaction_id = $1
			`,
			transactionId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				server.Raise(result.Scan(&priceMilliunits, &currency))
				rowCount += 1
			}
		})
	})
	connect.AssertEqual(t, rowCount, 1)
	return
}

// Every credit path validates through validateAppleTransactionAccount, so this
// pins the conversion for the webhook, the reconciler, the client verify and
// the offer-code binding. Only a USD price is net revenue; trials keep the
// marker they always had.
func TestAppleTransactionNetRevenueConvertsOnlyUsd(t *testing.T) {
	now := server.NowUtc().Truncate(time.Millisecond)
	cases := []struct {
		currency        string
		priceMilliunits int64
		wantNetRevenue  model.NanoCents
	}{
		// 80% of $4.99
		{currency: "USD", priceMilliunits: 4_990, wantNetRevenue: model.UsdToNanoCents(3.992)},
		{currency: "usd", priceMilliunits: 4_990, wantNetRevenue: model.UsdToNanoCents(3.992)},
		// Apple's documented example, KRW 3,300
		{currency: "KRW", priceMilliunits: 3_300_000, wantNetRevenue: appleCurrencyTestMarker},
		{currency: "JPY", priceMilliunits: 800_000, wantNetRevenue: appleCurrencyTestMarker},
		{currency: "INR", priceMilliunits: 449_000, wantNetRevenue: appleCurrencyTestMarker},
		// near parity with USD, but still not USD
		{currency: "EUR", priceMilliunits: 5_990, wantNetRevenue: appleCurrencyTestMarker},
		// no currency, or a malformed one, is not USD either
		{currency: "", priceMilliunits: 4_990, wantNetRevenue: appleCurrencyTestMarker},
		{currency: "US", priceMilliunits: 4_990, wantNetRevenue: appleCurrencyTestMarker},
		{currency: "USD1", priceMilliunits: 4_990, wantNetRevenue: appleCurrencyTestMarker},
		{currency: "U$D", priceMilliunits: 4_990, wantNetRevenue: appleCurrencyTestMarker},
		// trials and free offers keep the marker in any currency
		{currency: "USD", priceMilliunits: 0, wantNetRevenue: appleCurrencyTestMarker},
		{currency: "KRW", priceMilliunits: 0, wantNetRevenue: appleCurrencyTestMarker},
	}
	for _, c := range cases {
		claims := appleCurrencyTestClaims(server.NewId(), "apple-currency-"+server.NewId().String(), c.priceMilliunits, c.currency, now)
		transaction, err := validateAppleTransaction(
			appleCurrencyTestNotification(claims, now),
			[]string{appleCurrencyTestProductId},
			true,
		)
		if err != nil {
			t.Fatalf("%q %d: %v", c.currency, c.priceMilliunits, err)
		}
		if transaction.netRevenue != c.wantNetRevenue {
			t.Errorf("%q %d: net revenue = %d, want %d", c.currency, c.priceMilliunits, transaction.netRevenue, c.wantNetRevenue)
		}
	}
}

// The validated transaction carries the price as Apple recorded it and the
// currency as a three-letter code, for the ledger.
func TestAppleTransactionKeepsPriceAndCurrency(t *testing.T) {
	now := server.NowUtc().Truncate(time.Millisecond)
	cases := []struct {
		currency        string
		priceMilliunits int64
		wantCurrency    string
	}{
		{currency: "USD", priceMilliunits: 4_990, wantCurrency: "USD"},
		{currency: "usd", priceMilliunits: 4_990, wantCurrency: "USD"},
		{currency: "KRW", priceMilliunits: 3_300_000, wantCurrency: "KRW"},
		{currency: "KRW", priceMilliunits: 0, wantCurrency: "KRW"},
		{currency: "", priceMilliunits: 4_990, wantCurrency: ""},
		{currency: "US", priceMilliunits: 4_990, wantCurrency: ""},
		{currency: "USD1", priceMilliunits: 4_990, wantCurrency: ""},
		{currency: "U$D", priceMilliunits: 4_990, wantCurrency: ""},
	}
	for _, c := range cases {
		claims := appleCurrencyTestClaims(server.NewId(), "apple-currency-"+server.NewId().String(), c.priceMilliunits, c.currency, now)
		transaction, err := validateAppleTransaction(
			appleCurrencyTestNotification(claims, now),
			[]string{appleCurrencyTestProductId},
			true,
		)
		if err != nil {
			t.Fatalf("%q %d: %v", c.currency, c.priceMilliunits, err)
		}
		if transaction.priceMilliunits != c.priceMilliunits || transaction.currency != c.wantCurrency {
			t.Errorf(
				"%q %d: price = %d %q, want %d %q",
				c.currency,
				c.priceMilliunits,
				transaction.priceMilliunits,
				transaction.currency,
				c.priceMilliunits,
				c.wantCurrency,
			)
		}
	}
}

// A KRW subscription through the notification webhook: the entitlement and
// the balance grant are exactly a USD subscription's, the renewal and the
// balance record the paid marker instead of $2,640, and the ledger keeps the
// KRW price.
func TestProcessAppleNotificationNonUsdPriceRecordsNoUsdRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-currency-krw", server.NewId())

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "apple-currency-krw-" + server.NewId().String()
		claims := appleCurrencyTestClaims(networkId, transactionId, 3_300_000, "KRW", now)

		processed, err := ProcessAppleNotification(ctx, appleCurrencyTestNotification(claims, now), []string{appleCurrencyTestProductId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		renewalNetRevenue, balance := appleCurrencyTestRevenue(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, renewalNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.SubsidyNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.NetRevenue, model.NanoCents(0))

		// the grant is unchanged: paid, pro, the supporter bytes
		connect.AssertEqual(t, balance.Paid, true)
		connect.AssertEqual(t, balance.Pro, true)
		connect.AssertEqual(t, balance.StartBalanceByteCount, RefreshSupporterTransferBalance)
		connect.AssertEqual(t, balance.BalanceByteCount, RefreshSupporterTransferBalance)

		priceMilliunits, currency := appleCurrencyTestLedger(t, ctx, transactionId)
		connect.AssertEqual(t, priceMilliunits, int64(3_300_000))
		connect.AssertEqual(t, currency, "KRW")
	})
}

// A USD subscription keeps recording 80% of its price, and the ledger keeps
// the USD price.
func TestProcessAppleNotificationUsdPriceRecordsNetRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-currency-usd", server.NewId())

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "apple-currency-usd-" + server.NewId().String()
		claims := appleCurrencyTestClaims(networkId, transactionId, 4_990, "USD", now)

		processed, err := ProcessAppleNotification(ctx, appleCurrencyTestNotification(claims, now), []string{appleCurrencyTestProductId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		renewalNetRevenue, balance := appleCurrencyTestRevenue(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, renewalNetRevenue, model.UsdToNanoCents(3.992))
		connect.AssertEqual(t, balance.SubsidyNetRevenue, model.UsdToNanoCents(3.992))
		connect.AssertEqual(t, balance.Paid, true)
		connect.AssertEqual(t, balance.Pro, true)

		priceMilliunits, currency := appleCurrencyTestLedger(t, ctx, transactionId)
		connect.AssertEqual(t, priceMilliunits, int64(4_990))
		connect.AssertEqual(t, currency, "USD")
	})
}

// The client verify path credits a JPY transaction through the same
// validator and ledger: entitled, no USD revenue, JPY price kept.
func TestVerifyAppleTransactionNonUsdPriceRecordsNoUsdRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, networkId := verifyTestSession(ctx, t, "verifyapplecurrency")

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "verify-apple-currency-" + server.NewId().String()
		claims := appleCurrencyTestClaims(networkId, transactionId, 800_000, "JPY", now)

		result, err := VerifyAppleTransactionClaims(claims, []string{appleCurrencyTestProductId}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		renewalNetRevenue, balance := appleCurrencyTestRevenue(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, renewalNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.SubsidyNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.Paid, true)

		priceMilliunits, currency := appleCurrencyTestLedger(t, ctx, transactionId)
		connect.AssertEqual(t, priceMilliunits, int64(800_000))
		connect.AssertEqual(t, currency, "JPY")
	})
}

// The payment reconciler's credit for a lost notification takes the same
// path: an INR renewal is entitled without USD revenue.
func TestAppleReconcileCreditNonUsdPriceRecordsNoUsdRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-currency-reconcile", server.NewId())

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "apple-currency-reconcile-" + server.NewId().String()
		claims := appleCurrencyTestClaims(networkId, transactionId, 449_000, "INR", now)

		credited, creditedNetworkId, err := appleReconcileCreditTransaction(ctx, claims, []string{appleCurrencyTestProductId}, false)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, creditedNetworkId, networkId)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		renewalNetRevenue, balance := appleCurrencyTestRevenue(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, renewalNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.SubsidyNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.Paid, true)

		priceMilliunits, currency := appleCurrencyTestLedger(t, ctx, transactionId)
		connect.AssertEqual(t, priceMilliunits, int64(449_000))
		connect.AssertEqual(t, currency, "INR")
	})
}

// The recurring Pro grant pro-rates the renewal's net revenue into its own
// balance, which is how a misread price reached the payout planner a second
// time. For a KRW subscriber it now carries a share of the marker, not of
// $2,640.
func TestProGrantFromNonUsdAppleSubscriptionCarriesNoUsdRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-currency-grant", server.NewId())

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "apple-currency-grant-" + server.NewId().String()
		claims := appleCurrencyTestClaims(networkId, transactionId, 3_300_000, "KRW", now)
		processed, err := ProcessAppleNotification(ctx, appleCurrencyTestNotification(claims, now), []string{appleCurrencyTestProductId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)

		// a grant window inside the 31-day renewal, so the grant's share of
		// the renewal revenue is below one
		grantStartTime := now
		grantEndTime := now.Add(7 * 24 * time.Hour)
		addedTransferBalances := model.AddProTransferBalanceToAllNetworks(ctx, grantStartTime, grantEndTime, model.Gib)
		_, granted := addedTransferBalances[networkId]
		connect.AssertEqual(t, granted, true)

		grantCount := 0
		var grantSubsidyNetRevenue model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT subsidy_net_revenue_nano_cents
					FROM transfer_balance
					WHERE network_id = $1 AND start_time = $2 AND end_time = $3
				`,
				networkId,
				grantStartTime,
				grantEndTime,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					server.Raise(result.Scan(&grantSubsidyNetRevenue))
					grantCount += 1
				}
			})
		})
		connect.AssertEqual(t, grantCount, 1)
		connect.AssertEqual(t, grantSubsidyNetRevenue < appleCurrencyTestMarker, true)
	})
}

// The claims of a yearly subscription priced in currency.
func appleCurrencyTestYearlyClaims(
	networkId server.Id,
	transactionId string,
	priceMilliunits int64,
	currency string,
	now time.Time,
) map[string]any {
	claims := appleCurrencyTestClaims(networkId, transactionId, priceMilliunits, currency, now)
	claims["productId"] = "supporter_yearly_26"
	claims["expiresDate"] = float64(now.Add(365 * 24 * time.Hour).UnixMilli())
	return claims
}

// Apple's $10,000 maximum bounds a USD price only. A price in a currency with
// large units is a valid purchase, which the bound read as dollars and refused
// (no entitlement for a paid KRW, IDR or VND subscription).
func TestAppleTransactionPriceBoundAppliesOnlyToUsd(t *testing.T) {
	now := server.NowUtc().Truncate(time.Millisecond)
	cases := []struct {
		currency        string
		priceMilliunits int64
		valid           bool
	}{
		// a year in KRW, IDR and VND, and a month in VND
		{currency: "KRW", priceMilliunits: 66_000_000, valid: true},
		{currency: "IDR", priceMilliunits: 1_199_000_000, valid: true},
		{currency: "VND", priceMilliunits: 1_290_000_000, valid: true},
		{currency: "VND", priceMilliunits: 129_000_000, valid: true},
		// Apple's highest USD price, and a cent above it
		{currency: "USD", priceMilliunits: 10_000_000, valid: true},
		{currency: "USD", priceMilliunits: 10_000_010, valid: false},
	}
	for _, c := range cases {
		claims := appleCurrencyTestYearlyClaims(server.NewId(), "apple-currency-"+server.NewId().String(), c.priceMilliunits, c.currency, now)
		transaction, err := validateAppleTransaction(
			appleCurrencyTestNotification(claims, now),
			[]string{"supporter_yearly_26"},
			true,
		)
		if (err == nil) != c.valid {
			t.Errorf("%q %d: valid = %t, want %t (%v)", c.currency, c.priceMilliunits, err == nil, c.valid, err)
			continue
		}
		if err == nil && c.currency != "USD" && transaction.netRevenue != appleCurrencyTestMarker {
			t.Errorf("%q %d: net revenue = %d, want the marker", c.currency, c.priceMilliunits, transaction.netRevenue)
		}
	}
}

// A KRW yearly purchase through the notification webhook is entitled, with the
// marker for revenue and the KRW price on the ledger.
func TestProcessAppleNotificationLargeUnitPriceIsEntitled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-currency-krw-yearly", server.NewId())

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "apple-currency-krw-yearly-" + server.NewId().String()
		claims := appleCurrencyTestYearlyClaims(networkId, transactionId, 66_000_000, "KRW", now)

		processed, err := ProcessAppleNotification(ctx, appleCurrencyTestNotification(claims, now), []string{"supporter_yearly_26"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		renewalNetRevenue, balance := appleCurrencyTestRevenue(t, ctx, networkId, transactionId)
		connect.AssertEqual(t, renewalNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.SubsidyNetRevenue, appleCurrencyTestMarker)
		connect.AssertEqual(t, balance.Paid, true)
		connect.AssertEqual(t, balance.Pro, true)
		connect.AssertEqual(t, balance.StartBalanceByteCount, RefreshSupporterTransferBalance)

		priceMilliunits, currency := appleCurrencyTestLedger(t, ctx, transactionId)
		connect.AssertEqual(t, priceMilliunits, int64(66_000_000))
		connect.AssertEqual(t, currency, "KRW")
	})
}
