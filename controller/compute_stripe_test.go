//go:build sidecar_pending

package controller

// Compute's Stripe purchases against a fake Stripe on the private stack's PostgreSQL and Redis (BAL-M12-08;
// DESIGN-COMPUTE §9.5, §9.11, §9.12, D-BAL-10, CF-4), the stand-in for CTEST's computetest.FakeStores until it lands: a
// card past its payer's limits cancelled and never captured; a webhook replayed crediting once; a card saved only
// where its payer turns threshold top-up on, on a customer of the network and the member; 3-D Secure asked of a
// network's first purchase above $50; compute's events kept from Pro's handlers and Pro's from compute's ledger; a
// dispute reversing the lot as a chargeback; and a refund made outside the ledger reversing it, in full, or alarming,
// in part. Under -tags sidecar_pending, since the ledger's tables are MG-C1's.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
)

// The compute product the fake's sessions sell.
const testComputeStripeProduct = "prod_synthetic_compute"

// The ledger's keys for these tests: synthetic, test only.
type testComputeSecrets struct{}

func (self testComputeSecrets) PayerHashKey() ([]byte, error) {
	return []byte("synthetic controller payer hash key, test only"), nil
}

func (self testComputeSecrets) PromoHashKey() ([]byte, error) {
	return []byte("synthetic controller promo hash key, test only"), nil
}

// A fake Stripe for compute: customers and their search, Checkout sessions and their lines, PaymentIntents with their
// card, capture and cancel, card detaches and refunds, each call counted by path.
type testComputeStripeFake struct {
	stateLock sync.Mutex
	customers []map[string]any
	// a customer's idempotency key to its index
	customerKeys map[string]int
	sessions     []map[string]any
	forms        map[string]url.Values
	intents      map[string]map[string]any
	captureKeys  map[string]int
	cancelKeys   map[string]int
	detaches     []string
	refunds      map[string][]map[string]any
	refundForms  []url.Values
	calls        map[string]int
	server       *httptest.Server
}

func newTestComputeStripeFake(t testing.TB) *testComputeStripeFake {
	fake := &testComputeStripeFake{
		customerKeys: map[string]int{},
		forms:        map[string]url.Values{},
		intents:      map[string]map[string]any{},
		captureKeys:  map[string]int{},
		cancelKeys:   map[string]int{},
		refunds:      map[string][]map[string]any{},
		calls:        map[string]int{},
	}
	writeJson := func(w http.ResponseWriter, status int, object any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(object)
	}
	notFound := func(w http.ResponseWriter) {
		writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "not found"}})
	}
	form := func(r *http.Request) url.Values {
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		return values
	}
	metadataOf := func(values url.Values, prefix string) map[string]any {
		metadata := map[string]any{}
		for key, value := range values {
			if strings.HasPrefix(key, prefix+"[") && strings.HasSuffix(key, "]") {
				metadata[strings.TrimSuffix(strings.TrimPrefix(key, prefix+"["), "]")] = value[0]
			}
		}
		return metadata
	}
	mux := http.NewServeMux()
	count := func(pattern string, handler func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			fake.stateLock.Lock()
			fake.calls[pattern] += 1
			fake.stateLock.Unlock()
			handler(w, r)
		})
	}
	count("GET /v1/customers/search", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		data := []any{}
		query := r.URL.Query().Get("query")
		for _, customer := range fake.customers {
			metadata := customer["metadata"].(map[string]any)
			if query == fmt.Sprintf("metadata['compute_network_id']:'%s' AND metadata['compute_member_id']:'%s'", metadata["compute_network_id"], metadata["compute_member_id"]) {
				data = append(data, customer)
			}
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "search_result", "data": data, "has_more": false})
	})
	count("POST /v1/customers", func(w http.ResponseWriter, r *http.Request) {
		values := form(r)
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		key := r.Header.Get("Idempotency-Key")
		if index, ok := fake.customerKeys[key]; ok {
			writeJson(w, http.StatusOK, fake.customers[index])
			return
		}
		customer := map[string]any{"id": fmt.Sprintf("cus_synthetic_%d", len(fake.customers)+1), "object": "customer", "metadata": metadataOf(values, "metadata")}
		fake.customerKeys[key] = len(fake.customers)
		fake.customers = append(fake.customers, customer)
		writeJson(w, http.StatusOK, customer)
	})
	count("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		values := form(r)
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		id := fmt.Sprintf("cs_synthetic_%d", len(fake.sessions)+1)
		expiresAt, _ := strconv.ParseInt(values.Get("expires_at"), 10, 64)
		checkoutSession := map[string]any{
			"id": id, "object": "checkout.session", "mode": values.Get("mode"), "customer": values.Get("customer"),
			"url": "https://checkout.stripe.com/c/pay/" + id, "expires_at": expiresAt, "status": "open",
			"metadata": metadataOf(values, "metadata"), "payment_intent": nil,
		}
		fake.sessions = append(fake.sessions, checkoutSession)
		fake.forms[id] = values
		writeJson(w, http.StatusOK, checkoutSession)
	})
	count("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		data := []any{}
		for _, checkoutSession := range fake.sessions {
			if checkoutSession["payment_intent"] == r.URL.Query().Get("payment_intent") {
				data = append(data, checkoutSession)
			}
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	count("GET /v1/checkout/sessions/{sessionId}/line_items", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		values, ok := fake.forms[r.PathValue("sessionId")]
		if !ok {
			notFound(w)
			return
		}
		unitAmount, _ := strconv.ParseInt(values.Get("line_items[0][price_data][unit_amount]"), 10, 64)
		quantity, _ := strconv.ParseInt(values.Get("line_items[0][quantity]"), 10, 64)
		line := map[string]any{
			"id": "li_synthetic", "object": "item", "quantity": quantity,
			"price": map[string]any{"id": "price_synthetic", "object": "price", "product": values.Get("line_items[0][price_data][product]"), "unit_amount": unitAmount},
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": []any{line}, "has_more": false})
	})
	count("GET /v1/payment_intents/{intentId}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		intent, ok := fake.intents[r.PathValue("intentId")]
		if !ok {
			notFound(w)
			return
		}
		writeJson(w, http.StatusOK, intent)
	})
	count("POST /v1/payment_intents/{intentId}/capture", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		intent, ok := fake.intents[r.PathValue("intentId")]
		if !ok {
			notFound(w)
			return
		}
		fake.captureKeys[r.Header.Get("Idempotency-Key")] += 1
		intent["status"] = "succeeded"
		intent["amount_capturable"] = 0
		writeJson(w, http.StatusOK, intent)
	})
	count("POST /v1/payment_intents/{intentId}/cancel", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		intent, ok := fake.intents[r.PathValue("intentId")]
		if !ok {
			notFound(w)
			return
		}
		fake.cancelKeys[r.Header.Get("Idempotency-Key")] += 1
		intent["status"] = "canceled"
		writeJson(w, http.StatusOK, intent)
	})
	count("POST /v1/payment_methods/{methodId}/detach", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		id := r.PathValue("methodId")
		fake.detaches = append(fake.detaches, id)
		for _, intent := range fake.intents {
			if method := intent["payment_method"].(map[string]any); method["id"] == id {
				method["customer"] = nil
			}
		}
		writeJson(w, http.StatusOK, map[string]any{"id": id, "object": "payment_method", "type": "card"})
	})
	count("GET /v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		data := []any{}
		for _, refund := range fake.refunds[r.URL.Query().Get("charge")] {
			data = append(data, refund)
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	count("POST /v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		values := form(r)
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		fake.refundForms = append(fake.refundForms, values)
		writeJson(w, http.StatusOK, map[string]any{"id": fmt.Sprintf("re_synthetic_%d", len(fake.refundForms)), "object": "refund", "metadata": metadataOf(values, "metadata")})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// The session's form, as Stripe received it.
func (self *testComputeStripeFake) form(sessionId string) url.Values {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.forms[sessionId]
}

// Pays sessionId with a card of fingerprint, held for capture as manual capture holds it: its PaymentIntent made from
// the session's payment_intent_data, its card attached to the session's customer where the session saves it.
func (self *testComputeStripeFake) pay(sessionId string, fingerprint string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	values := self.forms[sessionId]
	intentId := fmt.Sprintf("pi_synthetic_%d", len(self.intents)+1)
	amount, _ := strconv.ParseInt(values.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	metadata := map[string]any{}
	for key, value := range values {
		if strings.HasPrefix(key, "payment_intent_data[metadata][") {
			metadata[strings.TrimSuffix(strings.TrimPrefix(key, "payment_intent_data[metadata]["), "]")] = value[0]
		}
	}
	saves := values.Get("payment_intent_data[setup_future_usage]")
	method := map[string]any{"id": "pm_synthetic_" + intentId, "object": "payment_method", "type": "card", "card": map[string]any{"fingerprint": fingerprint}}
	var setupFutureUsage any
	if saves != "" {
		method["customer"] = values.Get("customer")
		setupFutureUsage = saves
	}
	self.intents[intentId] = map[string]any{
		"id": intentId, "object": "payment_intent", "status": "requires_capture", "amount": amount, "amount_capturable": amount,
		"currency": "usd", "customer": values.Get("customer"), "metadata": metadata, "setup_future_usage": setupFutureUsage,
		"payment_method": method,
		"latest_charge":  map[string]any{"id": "ch_synthetic_" + intentId, "object": "charge", "amount": amount, "amount_refunded": 0},
	}
	for _, checkoutSession := range self.sessions {
		if checkoutSession["id"] == sessionId {
			checkoutSession["payment_intent"] = intentId
		}
	}
	return intentId
}

// An event of kind about intentId's PaymentIntent as it stands.
func (self *testComputeStripeFake) intentEvent(kind string, intentId string) *StripeWebhookArgs {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	object, _ := json.Marshal(self.intents[intentId])
	return &StripeWebhookArgs{Id: "evt_synthetic_" + kind + "_" + intentId, Type: kind, Data: &StripeEventData{Object: object}}
}

// The captures and cancels asked under key, and the cards detached.
func (self *testComputeStripeFake) captures(key string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.captureKeys[key]
}

func (self *testComputeStripeFake) cancels(key string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.cancelKeys[key]
}

func (self *testComputeStripeFake) detachCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.detaches)
}

func (self *testComputeStripeFake) customerCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.customers)
}

// The calls to pattern.
func (self *testComputeStripeFake) callCount(pattern string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls[pattern]
}

// Every call to the fake.
func (self *testComputeStripeFake) callTotal() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	total := 0
	for _, n := range self.calls {
		total += n
	}
	return total
}

// A service that can spend credit for the networks a test opens: sales open for them alone.
type testComputeSpendable struct {
	stateLock  sync.Mutex
	networkIds map[operator.Id]bool
}

func (self *testComputeSpendable) Spendable(env operator.Env, ctx context.Context, networkId operator.Id) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.networkIds[networkId], nil
}

func (self *testComputeSpendable) open(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkIds[networkId] = true
}

// Auto-renew's taker of saved cards, recording each card it is handed.
type testComputeAutoRenewCards struct {
	stateLock sync.Mutex
	saved     []*sidecarcontroller.ComputeCardSaved
}

func (self *testComputeAutoRenewCards) CardSaved(env operator.Env, ctx context.Context, saved *sidecarcontroller.ComputeCardSaved) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, earlier := range self.saved {
		if earlier.PaymentIntentId == saved.PaymentIntentId {
			return nil
		}
	}
	self.saved = append(self.saved, saved)
	return nil
}

func (self *testComputeAutoRenewCards) cards() []*sidecarcontroller.ComputeCardSaved {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*sidecarcontroller.ComputeCardSaved{}, self.saved...)
}

// Registered once a test binary: the registries take one each.
var (
	testComputeSpendableOnce sync.Once
	testComputeSpendableSale = &testComputeSpendable{networkIds: map[operator.Id]bool{}}
	testComputeCardsOnce     sync.Once
	testComputeCards         = &testComputeAutoRenewCards{}
)

// One test's compute: the fake Stripe, the ledger's env over the test's database and Redis, and an instance of
// compute's Stripe at the fake, active.
type testComputeStripeEnv struct {
	t        testing.TB
	ctx      context.Context
	fake     *testComputeStripeFake
	env      operator.Env
	clock    *sidecartest.FakeClock
	metrics  *sidecartest.FakeMetrics
	ledger   *model.ComputeLedgerSettings
	instance *ComputeServerStripe
}

func newTestComputeStripeEnv(t testing.TB) *testComputeStripeEnv {
	testComputeSpendableOnce.Do(func() {
		sidecarcontroller.RegisterComputeSpendableService("controller_test_spendable", testComputeSpendableSale)
	})
	testComputeCardsOnce.Do(func() {
		sidecarcontroller.RegisterComputeAutoRenewCards(testComputeCards)
	})
	fake := newTestComputeStripeFake(t)
	metrics := sidecartest.NewFakeMetrics(nil)
	clock := sidecartest.NewFakeClock(server.NowUtc())
	env, err := core.NewEnv(&sidecar.ServiceHost{Redis: sidecarhost.NewRedis(nil), Metrics: metrics, Clock: clock}, &core.Grants{Db: true})
	if err != nil {
		t.Fatal(err)
	}
	ledger := model.DefaultComputeLedgerSettings()
	settings := DefaultComputeStripeSettings()
	settings.ApiUrl = fake.server.URL
	instance := NewComputeServerStripe(settings, &ComputeStripeDeps{
		ApiKey: func() (string, error) { return "sk_test_synthetic_compute", nil },
		Config: func() (*ComputeStripeConfig, error) {
			return &ComputeStripeConfig{ProductId: testComputeStripeProduct, SuccessUrl: "https://ur.example/compute/success", CancelUrl: "https://ur.example/compute/cancel"}, nil
		},
		Env:     func() operator.Env { return env },
		Secrets: func() sidecarcontroller.ComputeLedgerSecrets { return testComputeSecrets{} },
		Ledger:  ledger,
		Active:  func() bool { return true },
	})
	return &testComputeStripeEnv{t: t, ctx: context.Background(), fake: fake, env: env, clock: clock, metrics: metrics, ledger: ledger, instance: instance}
}

// A network on sale, with its paying member.
func (self *testComputeStripeEnv) network() (server.Id, server.Id) {
	networkId := server.NewId()
	testComputeSpendableSale.open(networkId)
	return networkId, server.NewId()
}

// Opens a checkout through the ledger's ComputeCheckout, failing the test on an error or a refusal.
func (self *testComputeStripeEnv) checkout(request *sidecarcontroller.ComputeCheckoutRequest) *sidecarcontroller.ComputeStripeCheckoutSession {
	self.t.Helper()
	answer, err := sidecarcontroller.ComputeCheckout(self.env, self.ctx, self.ledger, testComputeSecrets{}, self.instance, request)
	if err != nil {
		self.t.Fatal(err)
	}
	if answer.Refusal != "" || answer.Session == nil {
		self.t.Fatalf("a checkout refused: %s", answer.Refusal)
	}
	return answer.Session
}

// Delivers event to the compute branch, failing the test where it is not taken or fails.
func (self *testComputeStripeEnv) deliver(event *StripeWebhookArgs) {
	self.t.Helper()
	handled, _, err := self.instance.Webhook(self.ctx, event)
	if err != nil || !handled {
		self.t.Fatalf("%s: handled %t, %v", event.Type, handled, err)
	}
}

// The Stripe lot of intentId, nil where none was credited.
func (self *testComputeStripeEnv) lot(intentId string) *model.ComputeLot {
	self.t.Helper()
	lot, err := self.instance.lotOf(self.ctx, intentId)
	if err != nil {
		self.t.Fatal(err)
	}
	return lot
}

// The payer's keyed hash of a synthetic card's fingerprint.
func (self *testComputeStripeEnv) payer(fingerprint string) []byte {
	self.t.Helper()
	hash, err := sidecarcontroller.ComputePayerHash(testComputeSecrets{}, sidecarcontroller.ComputePayerStripeCard, fingerprint)
	if err != nil {
		self.t.Fatal(err)
	}
	return hash
}

// An alarm's count by kind.
func (self *testComputeStripeEnv) alarms(kind string) float64 {
	return self.metrics.Value("urnetwork_compute_alarms_total", kind)
}

// A payer at its new-account day's $100 is refused a $25 pack at capture: the intent is cancelled uncaptured, the card
// it saved for threshold top-up is detached, and no lot is credited; another card's pack in the same network is
// captured and credited.
func TestComputeCardPastLimitsNeverCaptured(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		elsewhere := server.NewId()
		if _, err := sidecarcontroller.ComputeCreditLot(compute.env, compute.ctx, compute.ledger, &sidecarcontroller.ComputeCreditRequest{
			NetworkId: elsewhere, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketStripe,
			MarketRef: "pi_synthetic_prior", AmountNanoUsd: 100 * model.ComputeCreditNanoUsd, PayerHash: compute.payer("fp_synthetic_spent"),
		}); err != nil {
			tb.Fatal(err)
		}
		networkId, memberId := compute.network()
		session := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{
			NetworkId: networkId, MemberId: memberId, PackCredits: 25, AutoRenew: model.ComputeAutoRenewModeThreshold,
		})
		intentId := compute.fake.pay(session.SessionId, "fp_synthetic_spent")
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		if compute.fake.callCount("POST /v1/payment_intents/{intentId}/capture") != 0 {
			tb.Fatal("a card past its payer's limits was captured")
		}
		if compute.fake.cancels("compute_cancel_"+intentId) != 1 || compute.fake.detachCount() != 1 {
			tb.Fatal("the intent was not cancelled under its key with its card detached")
		}
		if compute.lot(intentId) != nil || compute.alarms(ComputeAlarmPurchaseRefused) != 1 {
			tb.Fatal("a refused card was credited, or raised no alarm")
		}
		// a redelivery leaves the cancelled intent as it stands
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		if compute.fake.callCount("POST /v1/payment_intents/{intentId}/capture") != 0 || compute.fake.callCount("POST /v1/payment_intents/{intentId}/cancel") != 1 {
			tb.Fatal("a redelivery decided again")
		}
		// another card buys in the same network
		session = compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 25})
		intentId = compute.fake.pay(session.SessionId, "fp_synthetic_fresh")
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		lot := compute.lot(intentId)
		if compute.fake.captures("compute_capture_"+intentId) != 1 || lot == nil || lot.AmountNanoUsd != 25*model.ComputeCreditNanoUsd || lot.NetworkId != networkId {
			tb.Fatalf("a card within its limits: lot %+v", lot)
		}
	})
}

// A purchase's events delivered more than once credit its lot once, by the PaymentIntent's id: the capture's own
// credit, payment_intent.succeeded twice and the capture's event again; the payer counted once, the terms recorded with
// the first purchase.
func TestComputeWebhookReplayedCreditsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		networkId, memberId := compute.network()
		session := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 50})
		intentId := compute.fake.pay(session.SessionId, "fp_synthetic_replayed")
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		for i := 0; i < 2; i += 1 {
			compute.deliver(compute.fake.intentEvent("payment_intent.succeeded", intentId))
		}
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		if compute.fake.callCount("POST /v1/payment_intents/{intentId}/capture") != 1 {
			tb.Fatal("a replay captured again")
		}
		var lots, payerDay, settled, termsVersion int64
		var termsUser server.Id
		server.Db(compute.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(compute.ctx, `SELECT count(*) FROM compute_lot WHERE network_id = $1`, networkId).Scan(&lots))
			server.Raise(conn.QueryRow(compute.ctx, `SELECT amount_nano_usd FROM compute_payer_day WHERE payer_hash = $1`, compute.payer("fp_synthetic_replayed")).Scan(&payerDay))
			server.Raise(conn.QueryRow(compute.ctx, `SELECT settled_nano_usd FROM compute_account WHERE network_id = $1`, networkId).Scan(&settled))
			server.Raise(conn.QueryRow(compute.ctx, `SELECT terms_version, user_id FROM compute_terms WHERE network_id = $1 AND clause = 'compute'`, networkId).Scan(&termsVersion, &termsUser))
		})
		if lots != 1 || payerDay != 50*model.ComputeCreditNanoUsd || settled != 50*model.ComputeCreditNanoUsd {
			tb.Fatalf("lots %d, payer day %d, settled %d", lots, payerDay, settled)
		}
		if termsVersion != model.ComputeTermsVersion || termsUser != memberId {
			tb.Fatalf("the terms recorded %d by %s", termsVersion, termsUser)
		}
	})
}

// A card is saved for off-session use only where its payer turns threshold top-up on in that act, on the Stripe
// customer of that network and that member, and only that card reaches auto-renew; another member of the network
// pays through a customer of their own.
func TestComputeCardSavedOnlyWhenItsPayerTurnsOnAutoRenew(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		networkId, memberId := compute.network()
		plain := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 10})
		if form := compute.fake.form(plain.SessionId); form.Has("payment_intent_data[setup_future_usage]") ||
			form.Get("payment_intent_data[capture_method]") != "manual" || form.Get("automatic_tax[enabled]") != "true" ||
			form.Get("payment_method_types[0]") != "card" || form.Get("mode") != "payment" || form.Has("client_reference_id") {
			tb.Fatalf("a plain pack's session: %v", form)
		}
		renewing := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{
			NetworkId: networkId, MemberId: memberId, PackCredits: 25, AutoRenew: model.ComputeAutoRenewModeThreshold,
			BelowNanoUsd: 7 * model.ComputeCreditNanoUsd, TopUpPackCredits: 30,
		})
		form := compute.fake.form(renewing.SessionId)
		if form.Get("payment_intent_data[setup_future_usage]") != "off_session" || form.Get("customer") != compute.fake.form(plain.SessionId).Get("customer") {
			tb.Fatalf("a threshold pack's session: %v", form)
		}
		other := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: server.NewId(), PackCredits: 10})
		if compute.fake.form(other.SessionId).Get("customer") == form.Get("customer") || compute.fake.customerCount() != 2 {
			tb.Fatal("two members of a network share a customer")
		}
		before := len(testComputeCards.cards())
		for _, checkoutSession := range []*sidecarcontroller.ComputeStripeCheckoutSession{plain, renewing, other} {
			intentId := compute.fake.pay(checkoutSession.SessionId, "fp_synthetic_saved_"+checkoutSession.SessionId)
			compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
			compute.deliver(compute.fake.intentEvent("payment_intent.succeeded", intentId))
		}
		cards := testComputeCards.cards()[before:]
		if len(cards) != 1 {
			tb.Fatalf("%d cards reached auto-renew", len(cards))
		}
		card := cards[0]
		if card.NetworkId != networkId || card.PayerMemberId != memberId || card.Mode != model.ComputeAutoRenewModeThreshold ||
			card.StripeCustomerId != form.Get("customer") || card.StripePaymentMethodId == "" ||
			card.BelowNanoUsd != 7*model.ComputeCreditNanoUsd || card.PackCredits != 30 || card.MonthLimitNanoUsd != 0 {
			tb.Fatalf("the card auto-renew took: %+v", card)
		}
	})
}

// 3-D Secure is asked of a network's first purchase above $50, and of no other: not a first purchase of $50, nor a
// later one of $100, past the new account's limits.
func TestComputeThreeDSecureOnAFirstPurchaseAboveFifty(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		const threeDSecure = "payment_method_options[card][request_three_d_secure]"
		networkId, memberId := compute.network()
		first := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 100})
		if compute.fake.form(first.SessionId).Get(threeDSecure) != "any" {
			tb.Fatal("a first purchase of $100 asked no 3-D Secure")
		}
		small := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 50})
		if compute.fake.form(small.SessionId).Has(threeDSecure) {
			tb.Fatal("a first purchase of $50 asked 3-D Secure")
		}
		intentId := compute.fake.pay(small.SessionId, "fp_synthetic_first")
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		if compute.lot(intentId) == nil {
			tb.Fatal("the first purchase was not credited")
		}
		compute.clock.Advance(compute.ledger.NewAccountWindow + 24*time.Hour)
		later := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 100})
		if compute.fake.form(later.SessionId).Has(threeDSecure) {
			tb.Fatal("a later purchase of $100 asked 3-D Secure")
		}
	})
}

// Compute's events through StripeWebhook reach no Pro handler, which would read Pro's Stripe, and write no Pro balance;
// Pro's events, whatever their amount, reach no compute ledger and ask compute's Stripe nothing.
func TestComputeEventNeverGrantsPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		proCalls := 0
		proLock := sync.Mutex{}
		pro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proLock.Lock()
			proCalls += 1
			proLock.Unlock()
			http.Error(w, "{}", http.StatusNotFound)
		}))
		defer pro.Close()
		previousBaseUrl, previousToken, previousProcess := stripeApiBaseUrl, stripeApiTokenFunc, computeStripeProcess
		stripeApiBaseUrl = pro.URL
		stripeApiTokenFunc = func() string { return "sk_test_synthetic_pro" }
		computeStripeProcess = func() *ComputeServerStripe { return compute.instance }
		defer func() {
			stripeApiBaseUrl, stripeApiTokenFunc, computeStripeProcess = previousBaseUrl, previousToken, previousProcess
		}()
		clientSession := session.Testing_CreateClientSession(compute.ctx, &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
		networkId, memberId := compute.network()
		checkoutSession := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 10})
		intentId := compute.fake.pay(checkoutSession.SessionId, "fp_synthetic_not_pro")
		sessionObject, _ := json.Marshal(map[string]any{
			"id": checkoutSession.SessionId, "object": "checkout.session", "amount_total": 1000, "payment_status": "unpaid",
			"metadata": map[string]any{ComputeStripeMetadataCompute: "1"}, "customer_details": map[string]any{"email": "buyer@ur.example"},
		})
		for _, event := range []*StripeWebhookArgs{
			{Id: "evt_synthetic_completed", Type: "checkout.session.completed", Data: &StripeEventData{Object: sessionObject}},
			compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId),
			compute.fake.intentEvent("payment_intent.succeeded", intentId),
		} {
			if _, err := StripeWebhook(event, clientSession); err != nil {
				tb.Fatalf("%s: %v", event.Type, err)
			}
		}
		if proCalls != 0 {
			tb.Fatalf("compute's events asked Pro's Stripe %d times", proCalls)
		}
		if compute.lot(intentId) == nil {
			tb.Fatal("compute's purchase was not credited")
		}
		var proBalances int64
		server.Db(compute.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(compute.ctx, `SELECT count(*) FROM transfer_balance WHERE network_id = $1`, networkId).Scan(&proBalances))
		})
		if proBalances != 0 {
			tb.Fatal("compute's purchase granted a transfer balance")
		}
		// Pro's events, at a compute pack's amount, are none of compute's
		before := compute.fake.callTotal()
		proSession, _ := json.Marshal(map[string]any{"id": "cs_synthetic_pro", "amount_total": 1000, "client_reference_id": networkId.String(), "metadata": map[string]any{"network_id": networkId.String()}})
		proIntent, _ := json.Marshal(map[string]any{"id": "pi_synthetic_pro", "amount": 1000, "metadata": map[string]any{"network_id": networkId.String()}})
		proCharge, _ := json.Marshal(map[string]any{"id": "ch_synthetic_pro", "invoice": "in_synthetic_pro", "payment_intent": "pi_synthetic_pro", "amount": 1000, "amount_refunded": 1000})
		for _, event := range []*StripeWebhookArgs{
			{Id: "evt_synthetic_pro_1", Type: "checkout.session.completed", Data: &StripeEventData{Object: proSession}},
			{Id: "evt_synthetic_pro_2", Type: "payment_intent.succeeded", Data: &StripeEventData{Object: proIntent}},
			{Id: "evt_synthetic_pro_3", Type: "payment_intent.amount_capturable_updated", Data: &StripeEventData{Object: proIntent}},
			{Id: "evt_synthetic_pro_4", Type: "charge.refunded", Data: &StripeEventData{Object: proCharge}},
			{Id: "evt_synthetic_pro_5", Type: "charge.dispute.created", Data: &StripeEventData{Object: proCharge}},
			{Id: "evt_synthetic_pro_6", Type: "invoice.paid", Data: &StripeEventData{Object: proSession}},
		} {
			handled, _, err := compute.instance.Webhook(compute.ctx, event)
			if handled || err != nil {
				tb.Fatalf("Pro's %s: handled %t, %v", event.Type, handled, err)
			}
		}
		if compute.fake.callTotal() != before || compute.lot("pi_synthetic_pro") != nil {
			tb.Fatal("Pro's events reached compute")
		}
	})
}

// A dispute on a compute charge is a chargeback: the lot reversed, the network's compute suspended; an inquiry, which
// takes no funds, reverses nothing; a won dispute alarms for a person's review.
func TestComputeDisputeReversesTheLot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		networkId, memberId := compute.network()
		checkoutSession := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 25})
		intentId := compute.fake.pay(checkoutSession.SessionId, "fp_synthetic_disputed")
		compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
		dispute := func(status string) *StripeWebhookArgs {
			object, _ := json.Marshal(map[string]any{"id": "dp_synthetic_" + status, "charge": "ch_synthetic_" + intentId, "payment_intent": intentId, "amount": 2500, "status": status})
			return &StripeWebhookArgs{Id: "evt_synthetic_dispute_" + status, Type: "charge.dispute.created", Data: &StripeEventData{Object: object}}
		}
		compute.deliver(dispute("warning_needs_response"))
		if lot := compute.lot(intentId); lot.ReversedTime != nil {
			tb.Fatal("an inquiry reversed the lot")
		}
		compute.deliver(dispute("needs_response"))
		lot := compute.lot(intentId)
		suspended, err := sidecarcontroller.ComputeChargebackSuspended(compute.env, compute.ctx, networkId)
		if err != nil {
			tb.Fatal(err)
		}
		if lot.ReversedTime == nil || !suspended {
			tb.Fatalf("a chargeback: reversed %v, suspended %t", lot.ReversedTime, suspended)
		}
		// funds_withdrawn after it reverses nothing twice
		withdrawn := dispute("needs_response")
		withdrawn.Type = "charge.dispute.funds_withdrawn"
		compute.deliver(withdrawn)
		won := dispute("won")
		won.Type = "charge.dispute.closed"
		compute.deliver(won)
		if compute.alarms(ComputeAlarmPurchaseDisputeWon) != 1 {
			tb.Fatal("a won dispute raised no alarm")
		}
	})
}

// A refund made outside the ledger reverses the lot when the charge is refunded in full and alarms when in part; a
// refund the ledger made itself books nothing more.
func TestComputeRefundOutsideTheLedgerReversesTheLot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		compute := newTestComputeStripeEnv(tb)
		networkId, memberId := compute.network()
		buy := func(fingerprint string) string {
			checkoutSession := compute.checkout(&sidecarcontroller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 10})
			intentId := compute.fake.pay(checkoutSession.SessionId, fingerprint)
			compute.deliver(compute.fake.intentEvent("payment_intent.amount_capturable_updated", intentId))
			return intentId
		}
		refunded := func(intentId string, amountRefunded int64, metadata map[string]any) *StripeWebhookArgs {
			chargeId := "ch_synthetic_" + intentId
			compute.fake.stateLock.Lock()
			compute.fake.refunds[chargeId] = append(compute.fake.refunds[chargeId], map[string]any{"id": "re_synthetic_" + intentId, "object": "refund", "amount": amountRefunded, "metadata": metadata})
			compute.fake.stateLock.Unlock()
			object, _ := json.Marshal(map[string]any{"id": chargeId, "payment_intent": intentId, "amount": 1000, "amount_refunded": amountRefunded})
			return &StripeWebhookArgs{Id: "evt_synthetic_refunded_" + intentId, Type: "charge.refunded", Data: &StripeEventData{Object: object}}
		}
		partial := buy("fp_synthetic_partial")
		compute.deliver(refunded(partial, 400, map[string]any{}))
		if compute.lot(partial).ReversedTime != nil || compute.alarms(ComputeAlarmPurchaseRefundPartial) != 1 {
			tb.Fatal("a partial refund outside the ledger reversed the lot, or raised no alarm")
		}
		byLedger := buy("fp_synthetic_by_ledger")
		compute.deliver(refunded(byLedger, 1000, map[string]any{ComputeStripeMetadataRefund: "self_serve"}))
		if compute.lot(byLedger).ReversedTime != nil {
			tb.Fatal("the ledger's own refund reversed its lot")
		}
		full := buy("fp_synthetic_full")
		compute.deliver(refunded(full, 1000, map[string]any{}))
		if compute.lot(full).ReversedTime == nil {
			tb.Fatal("a full refund outside the ledger left the lot")
		}
	})
}
