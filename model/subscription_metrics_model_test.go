package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestSubscriptionMetricsQueriesKeepBoundedAggregateContracts(t *testing.T) {
	for _, required := range []string{
		"market IN ('apple', 'google', 'stripe', 'solana')",
		"network_client.active = true",
		"network_client.source_client_id IS NULL",
		"metric_window.since_time <= network_client.auth_time",
		"GROUP BY network_id, market",
		"first_paid_deduplicated",
		"max(end_time) AS latest_end_time",
	} {
		if !strings.Contains(subscriptionMetricsAccountQuery, required) {
			t.Errorf("account query is missing %q", required)
		}
	}
	for _, required := range []string{
		"AND NOT event.dry_run",
		"event.action IN ('credited', 'ended', 'entitlement_repaired', 'refunded', 'disputed', 'revoked')",
	} {
		if !strings.Contains(subscriptionMetricsReconciliationQuery, required) {
			t.Errorf("reconciliation query is missing %q", required)
		}
	}
	for _, required := range []string{
		"transfer_balance.paid",
		"NOT transfer_balance.pro",
		"transfer_balance_code.redeem_balance_id = transfer_balance.balance_id",
		"GROUP BY metric_window.window_name,\n         GROUPING SETS ((fulfillment.source), ())",
		"('all', NULL::timestamp)",
		"'deduplicated'",
	} {
		if !strings.Contains(subscriptionMetricsDataPackQuery, required) {
			t.Errorf("data-pack query is missing %q", required)
		}
	}
	if count := strings.Count(subscriptionMetricsDataPackQuery, "FROM fulfillment"); count != 1 {
		t.Errorf("data-pack query scans its materialized fulfillment set %d times, want one scan for recent and all-time metrics", count)
	}
	for _, query := range []string{
		subscriptionMetricsAccountQuery,
		subscriptionMetricsReconciliationQuery,
		subscriptionMetricsDataPackQuery,
	} {
		selectTail := query[strings.LastIndex(query, "SELECT "):]
		for _, identifier := range []string{
			"network_id", "client_id", "event_id", "run_id", "purchase_event_id",
			"transaction_id", "purchase_token", "evidence", "details",
		} {
			if strings.Contains(selectTail, identifier) {
				t.Errorf("aggregate result tail exports identifier %q: %s", identifier, selectTail)
			}
		}
	}
}

func TestSubscriptionMetricsDataPackQueryExecutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				subscriptionMetricsDataPackQuery,
				now,
				now.Add(-24*time.Hour),
				now.Add(-7*24*time.Hour),
				now.Add(-30*24*time.Hour),
			)
			if err != nil {
				t.Fatalf("execute data-pack aggregate query: %v", err)
			}
			defer result.Close()
			for result.Next() {
				var source string
				var window string
				var fulfillmentCount int64
				var byteCount int64
				if err := result.Scan(&source, &window, &fulfillmentCount, &byteCount); err != nil {
					t.Fatalf("scan data-pack aggregate query: %v", err)
				}
			}
			if err := result.Err(); err != nil {
				t.Fatalf("read data-pack aggregate query: %v", err)
			}
		})
	})
}

func TestLoadSubscriptionMetricsSnapshotDeduplicatesAccountsAndDataPacks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)

		newNetwork := func() server.Id {
			networkId := server.NewId()
			testingCreatePaymentNetworkRow(ctx, networkId)
			return networkId
		}
		addRenewal := func(networkId server.Id, market string, start time.Time, end time.Time) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: market,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         UsdToNanoCents(5),
				TransactionId:      server.NewId().String(),
			}); err != nil {
				t.Fatalf("add synthetic renewal: %v", err)
			}
		}
		addTopLevelAuth := func(networkId server.Id, authTime time.Time) {
			t.Helper()
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic-subscription-device", "synthetic")
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
					clientId,
					authTime,
				))
			})
		}

		appleOld := newNetwork()
		addRenewal(appleOld, SubscriptionMarketApple, now.Add(-40*24*time.Hour), now.Add(20*24*time.Hour))
		addTopLevelAuth(appleOld, now.Add(-35*24*time.Hour))

		googleNew := newNetwork()
		addRenewal(googleNew, SubscriptionMarketGoogle, now.Add(-2*24*time.Hour), now.Add(28*24*time.Hour))
		addRenewal(googleNew, SubscriptionMarketGoogle, now.Add(-24*time.Hour), now.Add(29*24*time.Hour))
		addTopLevelAuth(googleNew, now.Add(-2*time.Hour))

		stripeNew := newNetwork()
		addRenewal(stripeNew, SubscriptionMarketStripe, now.Add(-12*time.Hour), now.Add(29*24*time.Hour))
		addTopLevelAuth(stripeNew, now.Add(-8*24*time.Hour))

		solanaChurned := newNetwork()
		addRenewal(solanaChurned, SubscriptionMarketSolana, now.Add(-10*24*time.Hour), now.Add(-2*24*time.Hour))

		multiStore := newNetwork()
		addRenewal(multiStore, SubscriptionMarketApple, now.Add(-100*24*time.Hour), now.Add(10*24*time.Hour))
		addRenewal(multiStore, SubscriptionMarketStripe, now.Add(-5*24*time.Hour), now.Add(25*24*time.Hour))
		addTopLevelAuth(multiStore, now.Add(-30*time.Minute))

		googleChurned := newNetwork()
		addRenewal(googleChurned, SubscriptionMarketGoogle, now.Add(-20*24*time.Hour), now.Add(-12*time.Hour))

		crossStoreReturn := newNetwork()
		addRenewal(crossStoreReturn, SubscriptionMarketApple, now.Add(-25*24*time.Hour), now.Add(-20*24*time.Hour))
		addRenewal(crossStoreReturn, SubscriptionMarketGoogle, now.Add(-2*24*time.Hour), now.Add(28*24*time.Hour))
		addTopLevelAuth(crossStoreReturn, now.Add(-25*time.Hour))

		// One code-backed grant is represented in both ledgers. It must remain
		// one fulfillment; the direct grant below has no code and is the third.
		redeemedGrant := &TransferBalance{
			NetworkId:             multiStore,
			StartTime:             now.Add(-3 * time.Hour),
			EndTime:               now.Add(30 * 24 * time.Hour),
			StartBalanceByteCount: Tib,
			BalanceByteCount:      Tib,
			NetRevenue:            UsdToNanoCents(3),
			Pro:                   false,
		}
		directGrant := &TransferBalance{
			NetworkId:             googleNew,
			StartTime:             now.Add(-2 * time.Hour),
			EndTime:               now.Add(30 * 24 * time.Hour),
			StartBalanceByteCount: 4 * Tib,
			BalanceByteCount:      4 * Tib,
			NetRevenue:            UsdToNanoCents(7),
			Pro:                   false,
		}
		unpaidGrant := &TransferBalance{
			NetworkId:             googleNew,
			StartTime:             now.Add(-time.Hour),
			EndTime:               now.Add(30 * 24 * time.Hour),
			StartBalanceByteCount: 8 * Tib,
			BalanceByteCount:      8 * Tib,
			Pro:                   false,
		}
		server.Tx(ctx, func(tx server.PgTx) {
			AddTransferBalanceInTx(ctx, tx, redeemedGrant)
			AddTransferBalanceInTx(ctx, tx, directGrant)
			AddTransferBalanceInTx(ctx, tx, unpaidGrant)
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO transfer_balance_code (
					balance_code_id, create_time, start_time, end_time,
					balance_byte_count, net_revenue_nano_cents,
					balance_code_secret, purchase_event_id, purchase_record,
					purchase_email, redeem_time, redeem_balance_id, network_id
				) VALUES
					($1, $2, $2, $3, $4, $5, $6, $7, '{}', '', $2, $8, $9),
					($10, $2, $2, $3, $11, $5, $12, $13, '{}', '', NULL, NULL, NULL)`,
				server.NewId(), now.Add(-3*time.Hour), now.Add(30*24*time.Hour), Tib,
				UsdToNanoCents(3), "synthetic-code-a", "synthetic-purchase-a",
				redeemedGrant.BalanceId, multiStore,
				server.NewId(), 2*Tib, "synthetic-code-b", "synthetic-purchase-b",
			))
		})

		heartbeatTime := now.Add(-30 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, store := range SubscriptionMetricsStores() {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO payment_reconciliation_watermark (store, watermark_time, update_time) VALUES ($1, $2, $2)`,
					store,
					heartbeatTime,
				))
			}
			event := func(store string, action string, at time.Time, dryRun bool) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO payment_reconciliation_event
					 (event_id, run_id, store, action, dry_run, event_time)
					 VALUES ($1, $2, $3, $4, $5, $6)`,
					server.NewId(), server.NewId(), store, action, dryRun, at,
				))
			}
			event(PaymentReconcileStoreAll, PaymentReconcileActionHeartbeat, heartbeatTime, false)
			event(SubscriptionMarketApple, PaymentReconcileActionCredited, now.Add(-time.Hour), false)
			event(SubscriptionMarketStripe, PaymentReconcileActionEnded, now.Add(-2*time.Hour), false)
			event(SubscriptionMarketGoogle, PaymentReconcileActionRefunded, now.Add(-2*24*time.Hour), false)
			event(SubscriptionMarketSolana, PaymentReconcileActionRevoked, now.Add(-10*24*time.Hour), false)
			event(SubscriptionMarketApple, PaymentReconcileActionDisputed, now.Add(-time.Hour), true)
		})

		snapshot := LoadSubscriptionMetricsSnapshot(ctx, now)
		wantValue := func(got int64, want int64, name string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %d, want %d", name, got, want)
			}
		}

		wantValue(snapshot.ActiveAccounts[SubscriptionMarketApple], 2, "active apple")
		wantValue(snapshot.ActiveAccounts[SubscriptionMarketGoogle], 2, "active google")
		wantValue(snapshot.ActiveAccounts[SubscriptionMarketStripe], 2, "active stripe")
		wantValue(snapshot.ActiveAccounts[SubscriptionMarketSolana], 0, "active solana")
		wantValue(snapshot.ActiveAccounts[SubscriptionMetricsStoreDeduplicated], 5, "active deduplicated")
		newPaid := func(store string, window string) int64 {
			return snapshot.NewPaidAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: window}]
		}
		wantValue(newPaid(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow24Hours), 1, "new paid deduplicated 24h")
		wantValue(newPaid(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow7Days), 2, "new paid deduplicated 7d")
		wantValue(newPaid(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow30Days), 5, "new paid deduplicated 30d")
		wantValue(newPaid(SubscriptionMarketApple, SubscriptionMetricsWindow30Days), 1, "new paid apple 30d")
		wantValue(newPaid(SubscriptionMarketGoogle, SubscriptionMetricsWindow7Days), 2, "new paid google 7d")
		wantValue(newPaid(SubscriptionMarketStripe, SubscriptionMetricsWindow24Hours), 1, "new paid stripe 24h")
		wantValue(newPaid(SubscriptionMarketSolana, SubscriptionMetricsWindow30Days), 1, "new paid solana 30d")
		// crossStoreReturn first paid Apple 25 days ago, then switched to
		// Google two days ago. The Google view includes that first-in-store
		// window, while the deduplicated 7-day count remains two: googleNew
		// and stripeNew. googleNew's duplicate Google renewal counts once.

		engaged := func(store string, window string) int64 {
			return snapshot.EngagedAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: window}]
		}
		wantValue(engaged(SubscriptionMarketApple, SubscriptionMetricsWindow24Hours), 1, "engaged apple 24h")
		wantValue(engaged(SubscriptionMarketGoogle, SubscriptionMetricsWindow24Hours), 1, "engaged google 24h")
		wantValue(engaged(SubscriptionMarketStripe, SubscriptionMetricsWindow24Hours), 1, "engaged stripe 24h")
		wantValue(engaged(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow24Hours), 2, "engaged deduplicated 24h")
		wantValue(engaged(SubscriptionMarketGoogle, SubscriptionMetricsWindow7Days), 2, "engaged google 7d")
		wantValue(engaged(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow7Days), 3, "engaged deduplicated 7d")
		wantValue(engaged(SubscriptionMarketStripe, SubscriptionMetricsWindow30Days), 2, "engaged stripe 30d")
		wantValue(engaged(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow30Days), 4, "engaged deduplicated 30d")

		churned := func(store string, window string) int64 {
			return snapshot.ChurnedAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: window}]
		}
		wantValue(churned(SubscriptionMarketGoogle, SubscriptionMetricsWindow24Hours), 1, "churned google 24h")
		wantValue(churned(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow24Hours), 1, "churned deduplicated 24h")
		wantValue(churned(SubscriptionMarketSolana, SubscriptionMetricsWindow7Days), 1, "churned solana 7d")
		wantValue(churned(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow7Days), 2, "churned deduplicated 7d")
		wantValue(churned(SubscriptionMarketApple, SubscriptionMetricsWindow30Days), 1, "churned apple 30d")
		wantValue(churned(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow30Days), 2, "churned deduplicated 30d")

		reconciliation := func(store string, action string, window string) int64 {
			return snapshot.ReconciliationEvents[SubscriptionMetricsStoreActionWindow{Store: store, Action: action, Window: window}]
		}
		wantValue(reconciliation(SubscriptionMarketApple, PaymentReconcileActionCredited, SubscriptionMetricsWindow24Hours), 1, "apple credited 24h")
		wantValue(reconciliation(SubscriptionMarketStripe, PaymentReconcileActionEnded, SubscriptionMetricsWindow24Hours), 1, "stripe ended 24h")
		wantValue(reconciliation(SubscriptionMarketGoogle, PaymentReconcileActionRefunded, SubscriptionMetricsWindow7Days), 1, "google refunded 7d")
		wantValue(reconciliation(SubscriptionMarketSolana, PaymentReconcileActionRevoked, SubscriptionMetricsWindow30Days), 1, "solana revoked 30d")
		wantValue(reconciliation(SubscriptionMarketApple, PaymentReconcileActionDisputed, SubscriptionMetricsWindow24Hours), 0, "dry-run disputed")
		if snapshot.ReconciliationHeartbeat == nil || !snapshot.ReconciliationHeartbeat.Equal(heartbeatTime) {
			t.Errorf("reconciliation heartbeat = %v, want %v", snapshot.ReconciliationHeartbeat, heartbeatTime)
		}
		for _, store := range SubscriptionMetricsStores() {
			if got := snapshot.ReconciliationWatermarks[store]; !got.Equal(heartbeatTime) {
				t.Errorf("%s watermark = %v, want %v", store, got, heartbeatTime)
			}
		}

		packKey := func(source string, window string) SubscriptionMetricsDataPackWindow {
			return SubscriptionMetricsDataPackWindow{Source: source, Window: window}
		}
		wantValue(snapshot.DataPackFulfillments[packKey(SubscriptionMetricsDataPackBalanceCode, SubscriptionMetricsWindow24Hours)], 2, "balance-code fulfillments")
		wantValue(snapshot.DataPackFulfillments[packKey(SubscriptionMetricsDataPackDirect, SubscriptionMetricsWindow24Hours)], 1, "direct fulfillments")
		wantValue(snapshot.DataPackFulfillments[packKey(SubscriptionMetricsDataPackDeduplicated, SubscriptionMetricsWindow24Hours)], 3, "deduplicated fulfillments")
		wantValue(snapshot.DataPackBytes[packKey(SubscriptionMetricsDataPackDeduplicated, SubscriptionMetricsWindow24Hours)], 7*Tib, "deduplicated fulfillment bytes")
		wantValue(snapshot.DataPackFulfillments[packKey(SubscriptionMetricsDataPackDeduplicated, SubscriptionMetricsWindowAll)], 3, "all-time deduplicated fulfillments")
	})
}
