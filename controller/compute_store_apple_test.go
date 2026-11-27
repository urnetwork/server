//go:build sidecar_pending

package controller

// The App Store's compute products on the private stack's PostgreSQL and Redis (BAL-M12-09; DESIGN-COMPUTE §9.5,
// §9.11, §9.12, D-BAL-8), from verified claims as Apple's pinned-root verifier answers them, with a stand-in for the
// App Store's history, until CTEST's computetest.FakeStores lands: a consumable credited once, by the app's report or
// the notification, whichever comes first; a renewal granting its month once; four upgrades in a sitting granting
// about the largest plan; a refund and a revocation reversing the lot; a purchase naming no network binding as Pro's
// offer codes do; a gone network credited nothing and recorded for support; a payer past its limits credited and
// flagged; and compute's products never reaching Pro's paths. Under -tags sidecar_pending, since the ledger's tables
// are MG-C1's.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
)

// One store test's ledger: its env over the test's database and Redis, the networks that stand, and the store deps.
type testComputeStoreEnv struct {
	t        testing.TB
	ctx      context.Context
	env      operator.Env
	clock    *sidecartest.FakeClock
	metrics  *sidecartest.FakeMetrics
	deps     *ComputeStoreDeps
	networks sync.Map
}

func newTestComputeStoreEnv(t testing.TB) *testComputeStoreEnv {
	metrics := sidecartest.NewFakeMetrics(nil)
	clock := sidecartest.NewFakeClock(server.NowUtc())
	env, err := core.NewEnv(&sidecar.ServiceHost{Redis: sidecarhost.NewRedis(nil), Metrics: metrics, Clock: clock}, &core.Grants{Db: true})
	if err != nil {
		t.Fatal(err)
	}
	store := &testComputeStoreEnv{t: t, ctx: context.Background(), env: env, clock: clock, metrics: metrics}
	store.deps = &ComputeStoreDeps{
		Env:     func() operator.Env { return env },
		Secrets: func() sidecarcontroller.ComputeLedgerSecrets { return testComputeSecrets{} },
		Active:  func() bool { return true },
		NetworkExists: func(ctx context.Context, networkId server.Id) (bool, error) {
			_, ok := store.networks.Load(networkId)
			return ok, nil
		},
	}
	return store
}

// A network that stands.
func (self *testComputeStoreEnv) network() server.Id {
	networkId := server.NewId()
	self.networks.Store(networkId, true)
	return networkId
}

// The lot of market and marketRef, nil where none was credited.
func (self *testComputeStoreEnv) lot(market sidecarmodel.ComputeMarket, marketRef string) *sidecarmodel.ComputeLot {
	self.t.Helper()
	db, err := self.env.Db()
	if err != nil {
		self.t.Fatal(err)
	}
	var lot *sidecarmodel.ComputeLot
	err = db.Tx(self.ctx, func(tx operator.DbTx) error {
		var err error
		lot, err = sidecarmodel.ComputeLotByMarketRefInTx(self.env, self.ctx, tx, market, marketRef, false)
		return err
	}, operator.DbIsolationReadCommitted, operator.DbReadOnly)
	if err != nil {
		self.t.Fatal(err)
	}
	return lot
}

// The sum of a network's lots, in credits.
func (self *testComputeStoreEnv) credited(networkId server.Id) float64 {
	self.t.Helper()
	var sum int64
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_lot WHERE network_id = $1`, networkId).Scan(&sum))
	})
	return float64(sum) / float64(sidecarmodel.ComputeCreditNanoUsd)
}

// A count a query selects.
func (self *testComputeStoreEnv) count(sql string, args ...any) int64 {
	self.t.Helper()
	var count int64
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, sql, args...).Scan(&count))
	})
	return count
}

// The App Store's history, standing in: each subscription's transactions by original transaction id.
type testComputeAppleHistory struct {
	stateLock sync.Mutex
	chains    map[string][]map[string]any
	reads     int
}

func (self *testComputeAppleHistory) add(claims map[string]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	original := claims["originalTransactionId"].(string)
	// newest first
	self.chains[original] = append([]map[string]any{claims}, self.chains[original]...)
}

func (self *testComputeAppleHistory) History(ctx context.Context, environment string, transactionId string, pageMaxCount int) ([]map[string]any, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reads += 1
	for _, chain := range self.chains {
		for _, claims := range chain {
			if claims["transactionId"] == transactionId {
				return append([]map[string]any{}, chain...), nil
			}
		}
	}
	return nil, fmt.Errorf("no transaction %s", transactionId)
}

// The App Store paths over the store env, with the history standing in.
func newTestComputeAppleStore(store *testComputeStoreEnv) (*ComputeAppleStore, *testComputeAppleHistory) {
	history := &testComputeAppleHistory{chains: map[string][]map[string]any{}}
	return NewComputeAppleStore(nil, store.deps, history), history
}

// A compute transaction's verified claims.
type testAppleTransaction struct {
	transactionId    string
	originalId       string
	productId        string
	networkId        *server.Id
	appTransactionId string
	purchase         time.Time
	reason           string
}

func (self *testAppleTransaction) claims() map[string]any {
	product, _ := sidecar.ComputeStoreProductOf(nil, self.productId)
	original := self.originalId
	if original == "" {
		original = self.transactionId
	}
	claims := map[string]any{
		"transactionId":         self.transactionId,
		"originalTransactionId": original,
		"productId":             self.productId,
		"purchaseDate":          float64(self.purchase.UnixMilli()),
		"environment":           "Production",
		"inAppOwnershipType":    "PURCHASED",
		"appTransactionId":      self.appTransactionId,
	}
	if self.networkId != nil {
		claims["appAccountToken"] = self.networkId.String()
	}
	if product != nil && product.Monthly {
		claims["type"] = computeAppleTypeSubscription
		claims["expiresDate"] = float64(self.purchase.AddDate(0, 1, 0).UnixMilli())
	} else {
		claims["type"] = computeAppleTypeConsumable
	}
	if self.reason != "" {
		claims["transactionReason"] = self.reason
	}
	return claims
}

// A notification of kind about transaction.
func testAppleNotification(kind string, subtype string, claims map[string]any) AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: kind,
		Subtype:          subtype,
		NotificationUUID: server.NewId().String(),
		SignedDate:       server.NowUtc().UnixMilli(),
		TransactionInfo:  claims,
	}
}

// A consumable is credited once, by the app's report or by ONE_TIME_CHARGE, whichever comes first: the second
// answers already_credited, or credits nothing.
func TestAppleConsumableCreditedOnceByReportOrNotification(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		networkId := store.network()
		reportFirst := (&testAppleTransaction{transactionId: "2000000000000001", productId: "compute.credits_25", networkId: &networkId, purchase: server.NowUtc()}).claims()
		result, err := apple.VerifyTransaction(store.ctx, reportFirst, networkId)
		if err != nil || result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the report: %+v %v", result, err)
		}
		processed, err := apple.Notification(store.ctx, testAppleNotification("ONE_TIME_CHARGE", "", reportFirst))
		if err != nil || processed {
			tb.Fatalf("the notification after the report: %t %v", processed, err)
		}
		notificationFirst := (&testAppleTransaction{transactionId: "2000000000000002", productId: "compute.credits_10", networkId: &networkId, purchase: server.NowUtc()}).claims()
		processed, err = apple.Notification(store.ctx, testAppleNotification("ONE_TIME_CHARGE", "", notificationFirst))
		if err != nil || !processed {
			tb.Fatalf("the notification: %t %v", processed, err)
		}
		result, err = apple.VerifyTransaction(store.ctx, notificationFirst, networkId)
		if err != nil || result.Status != VerifyPurchaseStatusAlreadyCredited {
			tb.Fatalf("the report after the notification: %+v %v", result, err)
		}
		if store.credited(networkId) != 35 || store.count(`SELECT count(*) FROM compute_lot WHERE network_id = $1 AND kind = $2`, networkId, int16(sidecarmodel.ComputeLotKindPurchase)) != 2 {
			tb.Fatalf("credited %v", store.credited(networkId))
		}
	})
}

// A subscription's SUBSCRIBED and each DID_RENEW grant a month under their own transaction ids, each once; a replay
// and the app's report of a credited month add nothing, and nothing is keyed by the original transaction id.
func TestAppleRenewalGrantsItsMonthOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		networkId := store.network()
		start := server.NowUtc().Add(-24 * time.Hour)
		first := (&testAppleTransaction{transactionId: "3000000000000001", productId: "compute.monthly_25", networkId: &networkId, purchase: start}).claims()
		renewal := (&testAppleTransaction{transactionId: "3000000000000002", originalId: "3000000000000001", productId: "compute.monthly_25", networkId: &networkId, purchase: start.AddDate(0, 1, 0), reason: computeAppleReasonRenewal}).claims()
		for _, notification := range []AppleNotificationDecodedPayload{
			testAppleNotification("SUBSCRIBED", "INITIAL_BUY", first),
			testAppleNotification("DID_RENEW", "", renewal),
			testAppleNotification("DID_RENEW", "", renewal),
		} {
			if _, err := apple.Notification(store.ctx, notification); err != nil {
				tb.Fatal(err)
			}
		}
		result, err := apple.VerifyTransaction(store.ctx, renewal, networkId)
		if err != nil || result.Status != VerifyPurchaseStatusAlreadyCredited {
			tb.Fatalf("the report of a credited month: %+v %v", result, err)
		}
		if store.credited(networkId) != 50 || store.lot(sidecarmodel.ComputeMarketAppStore, "3000000000000002").Kind != sidecarmodel.ComputeLotKindSubscription {
			tb.Fatalf("two months credited %v", store.credited(networkId))
		}
	})
}

// Four upgrades in one sitting, 10 to 25 to 50 to 100 credits a month, each taking effect at once with its prorated
// refund, grant about the largest plan's credits, not their sum.
func TestAppleUpgradesGrantTheDifference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, history := newTestComputeAppleStore(store)
		networkId := store.network()
		start := server.NowUtc()
		original := "4000000000000001"
		for i, credits := range []int64{10, 25, 50, 100} {
			claims := (&testAppleTransaction{
				transactionId: fmt.Sprintf("400000000000000%d", i+1), originalId: original,
				productId: sidecar.ComputeStoreProductId(true, credits), networkId: &networkId, purchase: start.Add(time.Duration(i) * time.Minute),
			}).claims()
			history.add(claims)
			kind, subtype := "DID_CHANGE_RENEWAL_PREF", computeAppleSubtypeUpgrade
			if i == 0 {
				kind, subtype = "SUBSCRIBED", "INITIAL_BUY"
			}
			if processed, err := apple.Notification(store.ctx, testAppleNotification(kind, subtype, claims)); err != nil || !processed {
				tb.Fatalf("change %d: %t %v", i, processed, err)
			}
		}
		credited := store.credited(networkId)
		if credited < 99.9 || 100.1 < credited {
			tb.Fatalf("four changes in a sitting credited %v credits, not about the largest plan's 100", credited)
		}
		// a downgrade takes effect at the next renewal, which grants the smaller plan whole
		downgrade := (&testAppleTransaction{
			transactionId: "4000000000000009", originalId: original, productId: "compute.monthly_10", networkId: &networkId,
			purchase: start.Add(3*time.Minute).AddDate(0, 1, 0), reason: computeAppleReasonRenewal,
		}).claims()
		if processed, err := apple.Notification(store.ctx, testAppleNotification("DID_CHANGE_RENEWAL_PREF", "DOWNGRADE", downgrade)); err != nil || processed {
			tb.Fatalf("a downgrade's notice credited: %t %v", processed, err)
		}
		if processed, err := apple.Notification(store.ctx, testAppleNotification("DID_RENEW", "", downgrade)); err != nil || !processed {
			tb.Fatalf("the renewal at the smaller plan: %t %v", processed, err)
		}
		if credited := store.credited(networkId); credited < 109.9 || 110.1 < credited {
			tb.Fatalf("after the renewal %v credits", credited)
		}
	})
}

// A REFUND or a REVOKE reverses the transaction's lot, what it paid for staying charged; a refunded transaction the
// app reports again credits nothing.
func TestAppleRefundReversesTheLot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		networkId := store.network()
		for _, kind := range []string{"REFUND", "REVOKE"} {
			claims := (&testAppleTransaction{transactionId: "5000000000000001" + kind, productId: "compute.credits_50", networkId: &networkId, purchase: server.NowUtc()}).claims()
			if _, err := apple.Notification(store.ctx, testAppleNotification("ONE_TIME_CHARGE", "", claims)); err != nil {
				tb.Fatal(err)
			}
			claims["revocationDate"] = float64(server.NowUtc().UnixMilli())
			if _, err := apple.Notification(store.ctx, testAppleNotification(kind, "", claims)); err != nil {
				tb.Fatal(err)
			}
			lot := store.lot(sidecarmodel.ComputeMarketAppStore, claims["transactionId"].(string))
			if lot == nil || lot.ReversedTime == nil {
				tb.Fatalf("%s left the lot: %+v", kind, lot)
			}
			result, err := apple.VerifyTransaction(store.ctx, claims, networkId)
			if err != nil || result.Status != VerifyPurchaseStatusInvalid {
				tb.Fatalf("a refunded transaction reported: %+v %v", result, err)
			}
		}
	})
}

// A purchase naming no network, such as an offer code redeemed in the store, binds as Pro's do: to the network whose
// app reports it first, its notification waiting for that report, and another network's report of it wrong_network.
func TestAppleNoAccountTokenBindsAsPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		reporter := store.network()
		other := store.network()
		claims := (&testAppleTransaction{transactionId: "6000000000000001", productId: "compute.monthly_10", purchase: server.NowUtc()}).claims()
		// the notification before any report has no network to credit: Apple sends it again
		if _, err := apple.Notification(store.ctx, testAppleNotification("SUBSCRIBED", "INITIAL_BUY", claims)); err == nil {
			tb.Fatal("an unbound notification was taken")
		}
		result, err := apple.VerifyTransaction(store.ctx, claims, reporter)
		if err != nil || result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the reporter: %+v %v", result, err)
		}
		if store.count(`SELECT count(*) FROM apple_offer_code_binding WHERE original_transaction_id = $1 AND network_id = $2`, "6000000000000001", reporter) != 1 {
			tb.Fatal("the report bound nothing")
		}
		result, err = apple.VerifyTransaction(store.ctx, claims, other)
		if err != nil || result.Status != VerifyPurchaseStatusWrongNetwork {
			tb.Fatalf("another network's report: %+v %v", result, err)
		}
		// its renewal follows the binding through the chain
		renewal := (&testAppleTransaction{transactionId: "6000000000000002", originalId: "6000000000000001", productId: "compute.monthly_10", purchase: server.NowUtc().AddDate(0, 1, 0), reason: computeAppleReasonRenewal}).claims()
		if processed, err := apple.Notification(store.ctx, testAppleNotification("DID_RENEW", "", renewal)); err != nil || !processed {
			tb.Fatalf("the bound renewal: %t %v", processed, err)
		}
		if store.credited(reporter) != 20 || store.credited(other) != 0 {
			tb.Fatalf("credited %v to the reporter, %v to the other", store.credited(reporter), store.credited(other))
		}
	})
}

// A purchase naming a network that is gone credits nothing; only Apple refunds, so it is recorded for support.
func TestAppleGoneNetworkCreditsNone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		gone := server.NewId()
		claims := (&testAppleTransaction{transactionId: "7000000000000001", productId: "compute.credits_100", networkId: &gone, purchase: server.NowUtc()}).claims()
		processed, err := apple.Notification(store.ctx, testAppleNotification("ONE_TIME_CHARGE", "", claims))
		if err != nil || processed {
			tb.Fatalf("a gone network: %t %v", processed, err)
		}
		if store.lot(sidecarmodel.ComputeMarketAppStore, "7000000000000001") != nil || store.credited(gone) != 0 {
			tb.Fatal("a gone network was credited")
		}
		if store.count(`SELECT count(*) FROM payment_reconciliation_event WHERE evidence = $1 AND action = 'credit_unfulfillable'`, "7000000000000001") != 1 {
			tb.Fatal("the payment was not recorded for support")
		}
	})
}

// A payer past its new account's $100 a day across networks is credited all the same, the store having taken the
// money, and flagged for review.
func TestApplePayerPastLimitsCreditedAndFlagged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		for i := 0; i < 5; i += 1 {
			networkId := store.network()
			claims := (&testAppleTransaction{
				transactionId: fmt.Sprintf("800000000000000%d", i), productId: "compute.credits_25", networkId: &networkId,
				appTransactionId: "app_synthetic_heavy", purchase: server.NowUtc(),
			}).claims()
			if processed, err := apple.Notification(store.ctx, testAppleNotification("ONE_TIME_CHARGE", "", claims)); err != nil || !processed {
				tb.Fatalf("purchase %d: %t %v", i, processed, err)
			}
			flagged := store.metrics.Value("urnetwork_compute_alarms_total", sidecarcontroller.ComputeAlarmPurchaseFlagged)
			if (i < 4 && flagged != 0) || (i == 4 && flagged != 1) {
				tb.Fatalf("purchase %d flagged %v times", i, flagged)
			}
			if store.credited(networkId) != 25 {
				tb.Fatalf("purchase %d credited %v", i, store.credited(networkId))
			}
		}
	})
}

// Compute's products reach no Pro path through the routes Pro's purchases take: no renewal, Pro balance or Pro ledger
// row; and Pro's products reach no compute path.
func TestAppleComputeNeverGrantsPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		previous := computeAppleStoreProcess
		computeAppleStoreProcess = func() *ComputeAppleStore { return apple }
		defer func() { computeAppleStoreProcess = previous }()
		networkId := store.network()
		clientSession := session.Testing_CreateClientSession(store.ctx, &session.ByJwt{NetworkId: networkId, UserId: server.NewId()})
		proProducts := []string{"supporter", "supporter_yearly"}
		consumable := (&testAppleTransaction{transactionId: "9000000000000001", productId: "compute.credits_10", networkId: &networkId, purchase: server.NowUtc()}).claims()
		result, err := VerifyAppleTransactionClaims(consumable, proProducts, clientSession)
		if err != nil || result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the routed report: %+v %v", result, err)
		}
		monthly := (&testAppleTransaction{transactionId: "9000000000000002", productId: "compute.monthly_10", networkId: &networkId, purchase: server.NowUtc()}).claims()
		notification := testAppleNotification("SUBSCRIBED", "INITIAL_BUY", monthly)
		if processed, err := ProcessAppleNotification(store.ctx, notification, proProducts); err != nil || !processed {
			tb.Fatalf("the routed notification: %t %v", processed, err)
		}
		if store.count(`SELECT count(*) FROM subscription_renewal WHERE network_id = $1`, networkId) != 0 ||
			store.count(`SELECT count(*) FROM transfer_balance WHERE network_id = $1`, networkId) != 0 ||
			store.count(`SELECT count(*) FROM apple_subscription_transaction WHERE network_id = $1`, networkId) != 0 ||
			store.count(`SELECT count(*) FROM apple_notification WHERE notification_uuid = $1`, notification.NotificationUUID) != 0 {
			tb.Fatal("a compute product reached Pro's path")
		}
		if store.credited(networkId) != 20 {
			tb.Fatalf("compute credited %v", store.credited(networkId))
		}
		// Pro's products are none of compute's
		pro := (&testAppleTransaction{transactionId: "9000000000000003", productId: "supporter", networkId: &networkId, purchase: server.NowUtc()}).claims()
		pro["expiresDate"] = float64(server.NowUtc().AddDate(0, 1, 0).UnixMilli())
		if computeAppleNotificationOf(testAppleNotification("SUBSCRIBED", "INITIAL_BUY", pro)) {
			tb.Fatal("a Pro notification routes to compute")
		}
		if result, err := apple.VerifyTransaction(store.ctx, pro, networkId); err != nil || result.Status != VerifyPurchaseStatusInvalid {
			tb.Fatalf("a Pro product in compute's path: %+v %v", result, err)
		}
		if store.lot(sidecarmodel.ComputeMarketAppStore, "9000000000000003") != nil {
			tb.Fatal("a Pro product credited compute")
		}
	})
}

// The network's monthly plan, as GET api/compute/auto-renew shows it.
func (self *testComputeStoreEnv) plan(networkId server.Id) *sidecarcontroller.ComputeMonthlyState {
	self.t.Helper()
	state, err := sidecarcontroller.ComputeAutoRenew(self.env, self.ctx, sidecarmodel.DefaultComputeLedgerSettings(), networkId)
	if err != nil {
		self.t.Fatal(err)
	}
	return state.Monthly
}

// A notification of kind about transaction, with its renewal info: auto-renew's switch and the plan that renews next.
func testAppleRenewalNotification(kind string, subtype string, claims map[string]any, autoRenewStatus int, autoRenewProductId string) AppleNotificationDecodedPayload {
	notification := testAppleNotification(kind, subtype, claims)
	notification.RenewalInfo = map[string]any{
		"originalTransactionId": claims["originalTransactionId"],
		"autoRenewStatus":       float64(autoRenewStatus),
		"autoRenewProductId":    autoRenewProductId,
	}
	return notification
}

// A subscription is the network's monthly plan as its notifications report it (BAL-M12-11): SUBSCRIBED records it by
// its original transaction id, a downgrade's DID_CHANGE_RENEWAL_PREF shows the plan that renews next and credits
// nothing, DID_CHANGE_RENEWAL_STATUS turns it off and on, EXPIRED ends it, and the app's report of a month credits
// alone, the plan left as the notifications said.
func TestApplePlanReportedToAutoRenew(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		apple, _ := newTestComputeAppleStore(store)
		networkId := store.network()
		start := server.NowUtc().Add(-24 * time.Hour)
		monthly25, monthly10 := sidecar.ComputeStoreProductId(true, 25), sidecar.ComputeStoreProductId(true, 10)
		first := (&testAppleTransaction{transactionId: "6000000000000001", productId: monthly25, networkId: &networkId, purchase: start}).claims()
		if _, err := apple.Notification(store.ctx, testAppleRenewalNotification("SUBSCRIBED", "INITIAL_BUY", first, 1, monthly25)); err != nil {
			tb.Fatal(err)
		}
		if plan := store.plan(networkId); !plan.On || plan.Market != sidecarmodel.ComputeMarketAppStore || plan.Credits != 25 {
			tb.Fatalf("subscribed: %+v", plan)
		}
		if _, err := apple.Notification(store.ctx, testAppleRenewalNotification("DID_CHANGE_RENEWAL_PREF", "DOWNGRADE", first, 1, monthly10)); err != nil {
			tb.Fatal(err)
		}
		if plan := store.plan(networkId); !plan.On || plan.Credits != 10 || store.credited(networkId) != 25 {
			tb.Fatalf("a downgrade: %+v, %v credited", plan, store.credited(networkId))
		}
		if _, err := apple.Notification(store.ctx, testAppleRenewalNotification("DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", first, 0, monthly10)); err != nil {
			tb.Fatal(err)
		}
		if plan := store.plan(networkId); plan.On {
			tb.Fatalf("auto-renew turned off: %+v", plan)
		}
		if _, err := apple.Notification(store.ctx, testAppleRenewalNotification("DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_ENABLED", first, 1, monthly10)); err != nil {
			tb.Fatal(err)
		}
		if plan := store.plan(networkId); !plan.On {
			tb.Fatalf("auto-renew turned on: %+v", plan)
		}
		if _, err := apple.Notification(store.ctx, testAppleRenewalNotification("EXPIRED", "VOLUNTARY", first, 1, monthly10)); err != nil {
			tb.Fatal(err)
		}
		if plan := store.plan(networkId); plan.On {
			tb.Fatalf("expired: %+v", plan)
		}
		renewal := (&testAppleTransaction{transactionId: "6000000000000002", originalId: "6000000000000001", productId: monthly25, networkId: &networkId, purchase: start.AddDate(0, 1, 0), reason: computeAppleReasonRenewal}).claims()
		result, err := apple.VerifyTransaction(store.ctx, renewal, networkId)
		if err != nil || result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the app's report of a month: %+v %v", result, err)
		}
		if plan := store.plan(networkId); plan.On || store.credited(networkId) != 50 {
			tb.Fatalf("after the app's report: %+v, %v credited", plan, store.credited(networkId))
		}
		if store.count(`SELECT count(*) FROM compute_auto_renew WHERE network_id = $1 AND store_ref = $2`, networkId, "6000000000000001") != 1 {
			tb.Fatal("the plan is not kept by its original transaction id")
		}
	})
}
