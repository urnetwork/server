package controller

// Hourly payment reconciliation -- the lost-webhook safety net (UPGRADE.md §8).
//
// Every crediting path is webhook-only, so a lost webhook is a lost credit and
// an unhandled revocation is a free subscription. The reconciler pulls payment
// truth from each store and repairs the server's subscription state in BOTH
// directions:
//
//   - store paid, server missing  -> credit, THROUGH the same idempotent gates
//     the webhooks use (the stripe_invoice ledger, the
//     apple_subscription_transaction ledger, the Play overlap re-check under
//     the purchase-token advisory lock, the Solana intent one-shot). The
//     reconciler has NO write path of its own, so a reconcile credit racing a
//     late webhook for the same event produces exactly one credit.
//   - store says the entitlement is ALREADY over (refunded, revoked, expired
//     and the end-of-period task missed it) -> end it at now. The cancelled ≠
//     expired rule: a cancel-at-period-end with time remaining is paid-through
//     and is NEVER ended -- we never claw back paid-through time.
//
// Every repair is a payment_reconciliation_event row; a run that repairs
// nothing writes only a heartbeat. Missing store credentials (local/test envs)
// skip that store with a skipped_store row, never fail. Per-store API budgets
// and panic isolation keep one broken store from starving the other three.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// paymentReconcileWindow is how far back the server-side renewal iteration
// reaches: rows active now or ended within the window are where a missed
// renewal or a missed revocation hides.
const paymentReconcileWindow = 48 * time.Hour

// paymentReconcileApiBudget caps the reconciler-initiated store API requests
// per store per run. Work left over is picked up by the next hourly run.
// Replaceable by tests.
var paymentReconcileApiBudget = 500

// paymentReconcileRenewalLimit bounds the renewal rows iterated per store per
// run.
var paymentReconcileRenewalLimit = 1000

// paymentReconcileSolanaFinalityGrace: only re-verify on-chain payments
// credited at least this long ago, so a just-credited transaction is never
// judged missing while the chain (or Helius' index) catches up.
const paymentReconcileSolanaFinalityGrace = 1 * time.Hour

// ----- store credential presence (skip-and-log seams) -----
//
// Local/test envs carry no store credentials; a store without credentials is
// skipped with a skipped_store audit row. Replaceable only by hermetic tests
// (the S5 Play seam pattern); production never mutates these.

var stripeReconcileHasCredentials = func() bool {
	_, err := server.Vault.SimpleResource("stripe.yml")
	return err == nil
}

var appleReconcileHasCredentials = func() bool {
	return appleReconcileCredentialsFunc() != nil
}

var playReconcileHasCredentials = func() bool {
	if _, err := server.Vault.SimpleResource("google.yml"); err != nil {
		return false
	}
	// the sku catalog the credit path prices from
	if _, err := server.Config.SimpleResource("play.yml"); err != nil {
		return false
	}
	return true
}

var solanaReconcileHasCredentials = func() bool {
	_, err := server.Vault.SimpleResource("helius.yml")
	return err == nil
}

// ----- apple App Store Server API client -----

// The only genuinely new credential reconciliation needs: App Store Server
// API client credentials, from vault/<env>/apple.yml --
// app_store_server_api_key_id, issuer_id, private_key (the .p8 contents).
// bundle_id and product_ids are read from the existing
// app_store_notifications block.
type appleServerApiCredentials struct {
	KeyId      string
	IssuerId   string
	PrivateKey string
	BundleId   string
	ProductIds []string
}

var appleReconcileCredentialsFunc = func() *appleServerApiCredentials {
	resource, err := server.Vault.SimpleResource("apple.yml")
	if err != nil {
		return nil
	}
	var config struct {
		KeyId                 string `yaml:"app_store_server_api_key_id"`
		IssuerId              string `yaml:"issuer_id"`
		PrivateKey            string `yaml:"private_key"`
		AppStoreNotifications *struct {
			BundleId   string   `yaml:"bundle_id"`
			ProductIds []string `yaml:"product_ids"`
		} `yaml:"app_store_notifications"`
	}
	resource.UnmarshalYaml(&config)
	if config.KeyId == "" || config.IssuerId == "" || config.PrivateKey == "" ||
		config.AppStoreNotifications == nil || config.AppStoreNotifications.BundleId == "" {
		return nil
	}
	return &appleServerApiCredentials{
		KeyId:      config.KeyId,
		IssuerId:   config.IssuerId,
		PrivateKey: config.PrivateKey,
		BundleId:   config.AppStoreNotifications.BundleId,
		ProductIds: config.AppStoreNotifications.ProductIds,
	}
}

// replaceable by tests standing up a fake App Store Server API
var appleAppStoreServerApiBaseUrl = "https://api.storekit.itunes.apple.com"

// appleServerApiAuthHeaderFunc signs the ES256 client JWT the App Store
// Server API requires. Mirrors playAuthHeaderFunc: on signing failure the
// header is simply absent and Apple answers 401, which surfaces as an error
// event. Replaceable by tests.
var appleServerApiAuthHeaderFunc = func(ctx context.Context, header http.Header) {
	creds := appleReconcileCredentialsFunc()
	if creds == nil {
		return
	}
	token, err := appleServerApiToken(creds)
	if err != nil {
		glog.Errorf("[reconcile]apple server api token: %s\n", err)
		return
	}
	header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
}

func appleServerApiToken(creds *appleServerApiCredentials) (string, error) {
	// .p8 files are PKCS8 EC keys; ParseECPrivateKeyFromPEM handles both the
	// SEC1 and PKCS8 encodings
	key, err := gojwt.ParseECPrivateKeyFromPEM([]byte(creds.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("parse App Store private key: %w", err)
	}

	now := server.NowUtc()
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, gojwt.MapClaims{
		"iss": creds.IssuerId,
		"iat": now.Unix(),
		"exp": now.Add(30 * time.Minute).Unix(),
		"aud": "appstoreconnect-v1",
		"bid": creds.BundleId,
	})
	token.Header["kid"] = creds.KeyId
	return token.SignedString(key)
}

// App Store Server API "Get All Subscription Statuses" response shapes.
// https://developer.apple.com/documentation/appstoreserverapi/get_all_subscription_statuses
type appleSubscriptionStatusesResponse struct {
	Data []*appleSubscriptionGroup `json:"data"`
}

type appleSubscriptionGroup struct {
	SubscriptionGroupIdentifier string                  `json:"subscriptionGroupIdentifier"`
	LastTransactions            []*appleLastTransaction `json:"lastTransactions"`
}

// the App Store subscription statuses
// 1 active, 2 expired, 3 billing retry (expired, retrying), 4 billing grace
// period (still entitled), 5 revoked
type appleLastTransaction struct {
	Status                int    `json:"status"`
	OriginalTransactionId string `json:"originalTransactionId"`
	SignedTransactionInfo string `json:"signedTransactionInfo"`
	// the renewal info JWS: autoRenewStatus (the customer's auto-renew switch),
	// autoRenewProductId, expirationIntent. Read by the subscription details.
	SignedRenewalInfo string `json:"signedRenewalInfo"`
}

func (self *appleLastTransaction) entitled() bool {
	return self.Status == 1 || self.Status == 4
}

// appleDecodeJwsPayload decodes a JWS payload WITHOUT verifying the signature.
// This is only ever used on responses fetched directly from Apple's App Store
// Server API over TLS with our client credentials -- an authenticated pull,
// unlike webhook pushes (which arrive unauthenticated and go through the full
// pinned-root verifier in api/handlers).
func appleDecodeJwsPayload(jws string) (map[string]any, error) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed JWS")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// ----- stripe API shapes (raw HTTP, via the stripeApiBaseUrl seam) -----

type stripeReconcileSubscription struct {
	Id                string            `json:"id"`
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	Metadata          map[string]string `json:"metadata"`
}

// stripeSubscriptionOver applies the cancelled ≠ expired rule to a Stripe
// subscription status: only statuses where Stripe says the entitlement is
// ALREADY over. cancel_at_period_end with status "active" is paid-through
// time and is never acted on.
func stripeSubscriptionOver(status string) bool {
	switch status {
	case "canceled", "incomplete_expired", "unpaid":
		return true
	}
	return false
}

type stripeReconcileInvoiceExpanded struct {
	Id           string                       `json:"id"`
	Status       string                       `json:"status"`
	Subscription *stripeReconcileSubscription `json:"subscription"`
}

type stripeReconcileInvoiceList struct {
	Data    []*StripeEventInvoiceObject `json:"data"`
	HasMore bool                        `json:"has_more"`
}

// ----- solana (Helius RPC) -----

// solanaRpcUrlFunc returns the Helius RPC endpoint (with the api key from
// vault helius.yml). Replaceable by tests.
var solanaRpcUrlFunc = func() string {
	apiKey := heliusConfig()["api_key"].(string)
	return fmt.Sprintf("https://mainnet.helius-rpc.com/?api-key=%s", apiKey)
}

type solanaSignatureStatusesResponse struct {
	Result struct {
		Value []*struct {
			Slot               int64   `json:"slot"`
			ConfirmationStatus *string `json:"confirmationStatus"`
		} `json:"value"`
	} `json:"result"`
}

// ----- run orchestration -----

// PaymentReconcileRunOptions selects how a reconcile pass runs. The zero
// value (or nil) is the hourly task's behavior: all four stores, repairs
// applied.
type PaymentReconcileRunOptions struct {
	// DryRun audits without repairing: every store API READ happens for real,
	// but every write is suppressed -- no credit, no ended entitlement, no
	// unfulfilled-record clearing, and no watermark advance (a dry run must
	// not eat the incremental window a later real run needs). Each suppressed
	// repair is recorded as a would_credit / would_end audit event with the
	// same evidence and details the real repair would carry, tagged dry_run.
	DryRun bool
	// Stores limits the pass to the named stores
	// (model.SubscriptionMarketStripe | Apple | Google | Solana); empty runs
	// all four. Filtered-out stores are untouched entirely: no events, no
	// watermark.
	Stores []string
}

// PaymentReconcileStoreResult is one store's tally for the run. In a dry run
// Credited/Ended count the would_credit/would_end events.
type PaymentReconcileStoreResult struct {
	Examined        int  `json:"examined"`
	Credited        int  `json:"credited"`
	Ended           int  `json:"ended"`
	Errors          int  `json:"errors"`
	Skipped         bool `json:"skipped,omitempty"`
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`
	// stripe only: how many invoice.paid credits since the last watermark
	// resolved their network by the LEGACY customer-email fallback (S11) --
	// surfaced, never repaired, until the fallback can be retired
	EmailFallbacks int `json:"email_fallbacks,omitempty"`
}

type PaymentReconcileRunResult struct {
	RunId  server.Id `json:"run_id"`
	DryRun bool      `json:"dry_run,omitempty"`
	// in a dry run Credited/Ended count the would_credit/would_end events
	Credited      int                                     `json:"credited"`
	Ended         int                                     `json:"ended"`
	Errors        int                                     `json:"errors"`
	SkippedStores []string                                `json:"skipped_stores,omitempty"`
	StoreResults  map[string]*PaymentReconcileStoreResult `json:"store_results,omitempty"`
	// the S11 email_fallback audit rows behind StoreResults' EmailFallbacks
	// counts, so the CLI can print each one as a line
	EmailFallbackEvents []*model.PaymentReconciliationEvent `json:"email_fallback_events,omitempty"`
}

type paymentReconcileRun struct {
	clientSession *session.ClientSession
	runId         server.Id
	now           time.Time
	dryRun        bool

	// remaining store API budget for the store currently reconciling
	budget int

	credited int
	ended    int
	errors   int
	skipped  []string
	// per-store detail folded into the heartbeat row
	storeDetails map[string]map[string]any
	// per-store tallies for the run result (the CLI summary)
	storeResults map[string]*PaymentReconcileStoreResult
	// the S11 email_fallback rows surfaced this run (see reconcileStripe leg 0)
	emailFallbackEvents []*model.PaymentReconciliationEvent
}

func (self *paymentReconcileRun) record(
	store string,
	action string,
	networkId *server.Id,
	evidence string,
	details map[string]any,
) {
	switch action {
	case model.PaymentReconcileActionCredited, model.PaymentReconcileActionWouldCredit:
		self.credited += 1
		self.storeResult(store).Credited += 1
	case model.PaymentReconcileActionEnded, model.PaymentReconcileActionWouldEnd:
		self.ended += 1
		self.storeResult(store).Ended += 1
	case model.PaymentReconcileActionError:
		self.errors += 1
		self.storeResult(store).Errors += 1
	case model.PaymentReconcileActionSkippedStore:
		self.skipped = append(self.skipped, store)
		self.storeResult(store).Skipped = true
	}
	if err := model.AddPaymentReconciliationEvent(self.clientSession.Ctx, &model.PaymentReconciliationEvent{
		RunId:     self.runId,
		Store:     store,
		NetworkId: networkId,
		Action:    action,
		Evidence:  evidence,
		Details:   details,
		DryRun:    self.dryRun,
	}); err != nil {
		// the audit trail must never turn a completed repair into a failed run
		glog.Errorf("[reconcile]could not record %s/%s event: %s\n", store, action, err)
	}
}

// spend takes one unit of the current store's API budget. false means the
// budget is exhausted: stop store work for this run (the next hourly run
// continues where the watermark left off).
func (self *paymentReconcileRun) spend(store string) bool {
	if self.budget <= 0 {
		detail := self.storeDetail(store)
		detail["budget_exhausted"] = true
		self.storeResult(store).BudgetExhausted = true
		return false
	}
	self.budget -= 1
	return true
}

// examine counts one store object considered this run (a listed invoice, a
// renewal row, a purchase token, an unfulfilled payment, a credited
// signature): the denominator an audit reads the repair counts against.
func (self *paymentReconcileRun) examine(store string) {
	summary := self.storeResult(store)
	summary.Examined += 1
	self.storeDetail(store)["examined"] = summary.Examined
}

// creditAction names the audit action for a credit repair: the real action,
// or its would_ dry-run form.
func (self *paymentReconcileRun) creditAction() string {
	if self.dryRun {
		return model.PaymentReconcileActionWouldCredit
	}
	return model.PaymentReconcileActionCredited
}

// end applies the "store says it is already over" repair -- or, in a dry run,
// records the would_end without touching anything. Callers apply the
// cancelled ≠ expired rule and the end_time > now check BEFORE calling, so a
// would_end names exactly the entitlement a real run would have ended.
func (self *paymentReconcileRun) end(
	store string,
	networkId server.Id,
	evidence string,
	details map[string]any,
) {
	if self.dryRun {
		self.record(store, model.PaymentReconcileActionWouldEnd, &networkId, evidence, details)
		return
	}
	ended, err := model.EndReconciledEntitlement(self.clientSession.Ctx, networkId, store, self.now)
	if err != nil {
		self.record(
			store,
			model.PaymentReconcileActionError,
			&networkId,
			evidence,
			map[string]any{"error": err.Error(), "leg": "end"},
		)
		return
	}
	if ended {
		self.record(store, model.PaymentReconcileActionEnded, &networkId, evidence, details)
	}
}

func (self *paymentReconcileRun) storeResult(store string) *PaymentReconcileStoreResult {
	summary, ok := self.storeResults[store]
	if !ok {
		summary = &PaymentReconcileStoreResult{}
		self.storeResults[store] = summary
	}
	return summary
}

func (self *paymentReconcileRun) storeDetail(store string) map[string]any {
	detail, ok := self.storeDetails[store]
	if !ok {
		detail = map[string]any{}
		self.storeDetails[store] = detail
	}
	return detail
}

// sinceWatermark is where the store's incremental listing starts: the stored
// watermark (backed off one hour so boundary objects are never missed -- the
// crediting gates absorb the overlap), or the full reconcile window on the
// first run.
func (self *paymentReconcileRun) sinceWatermark(store string) time.Time {
	if watermark, ok := model.GetPaymentReconcileWatermark(self.clientSession.Ctx, store); ok {
		return watermark.Add(-1 * time.Hour)
	}
	return self.now.Add(-paymentReconcileWindow)
}

// ErrPaymentReconcileRunInProgress: another real reconcile run holds the
// run-level advisory lock. The task system treats the error like any task
// failure and reschedules with backoff; a CLI caller reports it and exits.
var ErrPaymentReconcileRunInProgress = errors.New("another payment reconciliation run is in progress")

// paymentReconcileRunLockKey names the run-level advisory lock every real
// (non-dry) run holds for its whole duration. The task system's
// RunOnce("payment_reconciliation") serializes task-scheduled runs against
// each other, but a manual bringyourctl run is outside the task system: this
// lock is what makes CLI-vs-task interleaving impossible regardless of entry
// point.
const paymentReconcileRunLockKey = "payment_reconciliation_run"

// RunPaymentReconciliation is one reconcile pass over all four stores with
// the default options (the hourly task's entry point). Always returns a
// result: a store failing (or panicking) is recorded and the other stores
// still run.
func RunPaymentReconciliation(clientSession *session.ClientSession) (*PaymentReconcileRunResult, error) {
	return RunPaymentReconciliationWithOptions(clientSession, nil)
}

// RunPaymentReconciliationWithOptions is RunPaymentReconciliation with
// dry-run and store selection (the bringyourctl entry point). nil options =
// defaults.
func RunPaymentReconciliationWithOptions(
	clientSession *session.ClientSession,
	options *PaymentReconcileRunOptions,
) (*PaymentReconcileRunResult, error) {
	if options == nil {
		options = &PaymentReconcileRunOptions{}
	}

	if options.DryRun {
		// a dry run writes nothing, so it cannot interleave harmfully with a
		// real run: it runs lock-free (an audit can run while the task does)
		return runPaymentReconciliation(clientSession, options), nil
	}

	// One real run at a time across ALL entry points: a session-level
	// advisory lock held on a pinned connection for the run's whole duration
	// (the run spans many transactions and store API calls, so a tx-scoped
	// lock cannot cover it). Try-lock: a second caller reports busy instead
	// of queueing. OptNoRetry pins the callback to a single attempt -- the
	// pool must never re-run a completed reconcile pass on a dropped
	// connection.
	var result *PaymentReconcileRunResult
	locked := false
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			clientSession.Ctx,
			`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`,
			paymentReconcileRunLockKey,
		).Scan(&locked))
		if !locked {
			return
		}
		defer func() {
			// the connection returns to the pool, so the session lock must be
			// released explicitly -- and with a context that survives the
			// run's cancellation, or the lock would ride the pooled
			// connection and block every later run
			unlockCtx, unlockCancel := context.WithTimeout(
				context.WithoutCancel(clientSession.Ctx),
				15*time.Second,
			)
			defer unlockCancel()
			if _, err := conn.Exec(
				unlockCtx,
				`SELECT pg_advisory_unlock(hashtextextended($1, 0))`,
				paymentReconcileRunLockKey,
			); err != nil {
				// close the connection so the backend session (and with it
				// the lock) dies instead of returning to the pool locked
				glog.Errorf("[reconcile]could not release the run lock; closing the connection: %s\n", err)
				conn.Hijack().Close(unlockCtx)
			}
		}()
		result = runPaymentReconciliation(clientSession, options)
	}, server.OptNoRetry())
	if !locked {
		return nil, ErrPaymentReconcileRunInProgress
	}
	return result, nil
}

func runPaymentReconciliation(
	clientSession *session.ClientSession,
	options *PaymentReconcileRunOptions,
) *PaymentReconcileRunResult {
	run := &paymentReconcileRun{
		clientSession: clientSession,
		runId:         server.NewId(),
		now:           server.NowUtc(),
		dryRun:        options.DryRun,
		storeDetails:  map[string]map[string]any{},
		storeResults:  map[string]*PaymentReconcileStoreResult{},
	}

	type storeReconciler struct {
		store          string
		hasCredentials func() bool
		// reconcile returns complete = false when it stopped early (budget);
		// the watermark only advances on a complete, error-free pass
		reconcile func(run *paymentReconcileRun, since time.Time) (complete bool, err error)
	}

	stores := []storeReconciler{
		{model.SubscriptionMarketStripe, stripeReconcileHasCredentials, reconcileStripe},
		{model.SubscriptionMarketApple, appleReconcileHasCredentials, reconcileApple},
		{model.SubscriptionMarketGoogle, playReconcileHasCredentials, reconcilePlay},
		{model.SubscriptionMarketSolana, solanaReconcileHasCredentials, reconcileSolana},
	}
	if 0 < len(options.Stores) {
		selected := map[string]bool{}
		for _, store := range options.Stores {
			selected[store] = true
		}
		stores = slices.DeleteFunc(stores, func(storeReconcile storeReconciler) bool {
			return !selected[storeReconcile.store]
		})
	}

	for _, storeReconcile := range stores {
		if !storeReconcile.hasCredentials() {
			glog.Infof("[reconcile]%s: no credentials in this env; skipping\n", storeReconcile.store)
			run.record(
				storeReconcile.store,
				model.PaymentReconcileActionSkippedStore,
				nil,
				"",
				map[string]any{"reason": "missing_credentials"},
			)
			continue
		}

		run.budget = paymentReconcileApiBudget
		since := run.sinceWatermark(storeReconcile.store)

		// one store's failure -- error or panic -- never stops the others
		func() {
			defer func() {
				if r := recover(); r != nil {
					glog.Errorf("[reconcile]%s: panic: %v\n", storeReconcile.store, r)
					run.record(
						storeReconcile.store,
						model.PaymentReconcileActionError,
						nil,
						"",
						map[string]any{"panic": fmt.Sprintf("%v", r)},
					)
				}
			}()

			complete, err := storeReconcile.reconcile(run, since)
			if err != nil {
				glog.Errorf("[reconcile]%s: %s\n", storeReconcile.store, err)
				run.record(
					storeReconcile.store,
					model.PaymentReconcileActionError,
					nil,
					"",
					map[string]any{"error": err.Error()},
				)
			} else if complete && !run.dryRun {
				// the next run's listing starts here (minus the overlap
				// backoff); a dry run never advances the watermark -- the
				// un-eaten window is what the later real run reconciles
				if err := model.SetPaymentReconcileWatermark(clientSession.Ctx, storeReconcile.store, run.now); err != nil {
					glog.Errorf("[reconcile]%s: could not advance watermark: %s\n", storeReconcile.store, err)
				}
			}
		}()
	}

	// every run leaves a heartbeat -- a run that repaired nothing writes ONLY
	// this row, and a missing heartbeat is how an operator sees the task died
	heartbeatDetails := map[string]any{
		"credited": run.credited,
		"ended":    run.ended,
		"errors":   run.errors,
	}
	if run.dryRun {
		heartbeatDetails["dry_run"] = true
	}
	if 0 < len(run.skipped) {
		heartbeatDetails["skipped_stores"] = run.skipped
	}
	if 0 < len(run.storeDetails) {
		heartbeatDetails["stores"] = run.storeDetails
	}
	run.record(
		model.PaymentReconcileStoreAll,
		model.PaymentReconcileActionHeartbeat,
		nil,
		"",
		heartbeatDetails,
	)

	return &PaymentReconcileRunResult{
		RunId:               run.runId,
		DryRun:              run.dryRun,
		Credited:            run.credited,
		Ended:               run.ended,
		Errors:              run.errors,
		SkippedStores:       run.skipped,
		StoreResults:        run.storeResults,
		EmailFallbackEvents: run.emailFallbackEvents,
	}
}

// ----- stripe -----
//
// decision table (observed store state -> action):
//
//	paid subscription invoice since watermark, not in stripe_invoice ledger -> credit
//	    (through stripeHandleInvoicePaid, gated by the ledger)
//	subscription status canceled / incomplete_expired / unpaid, renewal active -> end
//	status active + cancel_at_period_end (time remaining)                      -> nothing
//	anything else                                                              -> nothing
func reconcileStripe(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe

	// leg 0: surface, never repair -- every invoice.paid credit since the last
	// watermark that resolved its network by the LEGACY customer-email
	// fallback (S11). The webhook already credited (that is the point of the
	// fallback); counting it here in the heartbeat, the run result, and the
	// CLI summary is what keeps every use visible until the fallback can be
	// retired. A DB read: costs no store API budget, safe in a dry run.
	emailFallbackEvents := model.GetPaymentReconciliationEventsByAction(
		ctx,
		store,
		model.PaymentReconcileActionEmailFallback,
		since,
		paymentReconcileRenewalLimit,
	)
	if 0 < len(emailFallbackEvents) {
		run.storeResult(store).EmailFallbacks = len(emailFallbackEvents)
		run.storeDetail(store)["email_fallbacks"] = len(emailFallbackEvents)
		run.emailFallbackEvents = append(run.emailFallbackEvents, emailFallbackEvents...)
	}

	// leg 1: store-side listing -- paid invoices created since the watermark
	// whose credit never landed (the lost invoice.paid repair)
	startingAfter := ""
	for {
		if !run.spend(store) {
			return false, nil
		}

		listUrl := fmt.Sprintf(
			"%s/v1/invoices?%s",
			stripeApiBaseUrl,
			url.Values{
				"status":       []string{"paid"},
				"created[gte]": []string{fmt.Sprintf("%d", since.Unix())},
				"limit":        []string{"100"},
			}.Encode(),
		)
		if startingAfter != "" {
			listUrl = fmt.Sprintf("%s&starting_after=%s", listUrl, url.QueryEscape(startingAfter))
		}
		invoiceList, err := server.HttpGetRequireStatusOk[*stripeReconcileInvoiceList](
			ctx,
			listUrl,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[*stripeReconcileInvoiceList],
		)
		if err != nil {
			return false, fmt.Errorf("list invoices: %w", err)
		}

		for _, invoice := range invoiceList.Data {
			if invoice.Id == "" {
				continue
			}
			run.examine(store)
			if _, credited := model.GetStripeInvoiceNetworkId(ctx, invoice.Id); credited {
				// the webhook already handled this one
				continue
			}
			if !run.spend(store) {
				return false, nil
			}
			if run.dryRun {
				// resolve where the credit WOULD land without crediting: the
				// expanded invoice's subscription metadata names the network
				// for server-created checkouts (the first source the real
				// credit path reads). The deeper fallbacks (checkout-session
				// client reference, customer email) stay with the credit
				// path, so a dry-run line can show no network id where a real
				// run would still resolve one.
				fullInvoice, err := server.HttpGetRequireStatusOk[*stripeReconcileInvoiceExpanded](
					ctx,
					fmt.Sprintf(
						"%s/v1/invoices/%s?expand[]=subscription",
						stripeApiBaseUrl,
						url.PathEscape(invoice.Id),
					),
					func(header http.Header) {
						header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
					},
					server.ResponseJsonObject[*stripeReconcileInvoiceExpanded],
				)
				if err != nil {
					run.record(
						store,
						model.PaymentReconcileActionError,
						nil,
						invoice.Id,
						map[string]any{"error": err.Error(), "leg": "credit"},
					)
					continue
				}
				if fullInvoice.Subscription == nil {
					// a non-subscription invoice -- the real run would do nothing
					continue
				}
				var networkId *server.Id
				if id, err := server.ParseId(fullInvoice.Subscription.Metadata["network_id"]); err == nil {
					networkId = &id
				}
				run.record(
					store,
					model.PaymentReconcileActionWouldCredit,
					networkId,
					invoice.Id,
					map[string]any{
						"total":        invoice.Total,
						"subscription": fullInvoice.Subscription.Id,
					},
				)
				continue
			}
			// the repair IS the webhook path: same network resolution, same
			// stripe_invoice ledger gate, so a racing late webhook delivery for
			// the same invoice credits exactly once between the two of them
			if _, err := stripeHandleInvoicePaid(invoice, run.clientSession); err != nil {
				run.record(
					store,
					model.PaymentReconcileActionError,
					nil,
					invoice.Id,
					map[string]any{"error": err.Error(), "leg": "credit"},
				)
				continue
			}
			if networkId, credited := model.GetStripeInvoiceNetworkId(ctx, invoice.Id); credited {
				run.record(
					store,
					model.PaymentReconcileActionCredited,
					&networkId,
					invoice.Id,
					map[string]any{"total": invoice.Total},
				)
			}
			// not credited and no error: a non-subscription invoice -- nothing to do
		}

		if !invoiceList.HasMore || len(invoiceList.Data) == 0 {
			break
		}
		startingAfter = invoiceList.Data[len(invoiceList.Data)-1].Id
	}

	// leg 2: server-side -- for networks we think are actively subscribed,
	// does Stripe agree the subscription is still alive?
	renewals := model.GetReconcileSubscriptionRenewals(
		ctx,
		store,
		run.now.Add(-paymentReconcileWindow),
		paymentReconcileRenewalLimit,
	)
	seenNetworkIds := map[server.Id]bool{}
	for _, renewal := range renewals {
		if seenNetworkIds[renewal.NetworkId] {
			continue
		}
		seenNetworkIds[renewal.NetworkId] = true
		if !renewal.EndTime.After(run.now) {
			// already over on our side -- nothing to end
			continue
		}
		if renewal.TransactionId == "" {
			continue
		}
		run.examine(store)
		if !run.spend(store) {
			return false, nil
		}

		invoiceUrl := fmt.Sprintf(
			"%s/v1/invoices/%s?expand[]=subscription",
			stripeApiBaseUrl,
			renewal.TransactionId,
		)
		fullInvoice, err := server.HttpGetRequireStatusOk[*stripeReconcileInvoiceExpanded](
			ctx,
			invoiceUrl,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[*stripeReconcileInvoiceExpanded],
		)
		if err != nil {
			run.record(
				store,
				model.PaymentReconcileActionError,
				&renewal.NetworkId,
				renewal.TransactionId,
				map[string]any{"error": err.Error(), "leg": "status"},
			)
			continue
		}
		if fullInvoice.Subscription == nil {
			continue
		}
		if stripeSubscriptionOver(fullInvoice.Subscription.Status) {
			run.end(
				store,
				renewal.NetworkId,
				fullInvoice.Subscription.Id,
				map[string]any{"subscription_status": fullInvoice.Subscription.Status},
			)
		}
	}

	return true, nil
}

// ----- apple -----
//
// decision table (App Store subscription status -> action):
//
//	1 active / 4 grace, latest transaction not in the ledger -> credit (through
//	    the apple_subscription_transaction gate)
//	2 expired / 3 billing retry / 5 revoked, renewal active  -> end
//	1 active with auto-renew off (cancel at period end)      -> nothing (status
//	    stays 1 until expiry; the rule falls out of the status itself)
func reconcileApple(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketApple

	creds := appleReconcileCredentialsFunc()
	if creds == nil {
		return false, errors.New("apple credentials disappeared mid-run")
	}

	renewals := model.GetReconcileSubscriptionRenewals(
		ctx,
		store,
		run.now.Add(-paymentReconcileWindow),
		paymentReconcileRenewalLimit,
	)
	seenNetworkIds := map[server.Id]bool{}
	for _, renewal := range renewals {
		if seenNetworkIds[renewal.NetworkId] {
			continue
		}
		seenNetworkIds[renewal.NetworkId] = true
		if renewal.TransactionId == "" {
			continue
		}
		run.examine(store)
		if !run.spend(store) {
			return false, nil
		}

		statusesUrl := fmt.Sprintf(
			"%s/inApps/v1/subscriptions/%s",
			appleAppStoreServerApiBaseUrl,
			url.PathEscape(renewal.TransactionId),
		)
		statuses, err := server.HttpGetRequireStatusOk[*appleSubscriptionStatusesResponse](
			ctx,
			statusesUrl,
			func(header http.Header) {
				appleServerApiAuthHeaderFunc(ctx, header)
			},
			server.ResponseJsonObject[*appleSubscriptionStatusesResponse],
		)
		if err != nil {
			run.record(
				store,
				model.PaymentReconcileActionError,
				&renewal.NetworkId,
				renewal.TransactionId,
				map[string]any{"error": err.Error(), "leg": "status"},
			)
			continue
		}

		// across all groups, the subscription is entitled if ANY last
		// transaction is active or in grace
		var entitledTransaction *appleLastTransaction
		anyTransaction := false
		lastStatus := 0
		for _, group := range statuses.Data {
			for _, lastTransaction := range group.LastTransactions {
				anyTransaction = true
				lastStatus = lastTransaction.Status
				if lastTransaction.entitled() {
					entitledTransaction = lastTransaction
					break
				}
			}
			if entitledTransaction != nil {
				break
			}
		}
		if !anyTransaction {
			// nothing to conclude either way
			continue
		}

		if entitledTransaction != nil {
			// credit direction: the store's latest entitled transaction may be a
			// renewal whose notification never arrived
			claims, err := appleDecodeJwsPayload(entitledTransaction.SignedTransactionInfo)
			if err != nil {
				run.record(
					store,
					model.PaymentReconcileActionError,
					&renewal.NetworkId,
					renewal.TransactionId,
					map[string]any{"error": err.Error(), "leg": "decode"},
				)
				continue
			}
			transactionId, _ := appleControllerStringClaim(claims, "transactionId")
			if transactionId == "" || model.IsAppleTransactionCredited(ctx, transactionId) {
				continue
			}
			credited, networkId, err := appleReconcileCreditTransaction(ctx, claims, creds.ProductIds, run.dryRun)
			if err != nil {
				run.record(
					store,
					model.PaymentReconcileActionError,
					&renewal.NetworkId,
					transactionId,
					map[string]any{"error": err.Error(), "leg": "credit"},
				)
				continue
			}
			if credited {
				run.record(
					store,
					run.creditAction(),
					&networkId,
					transactionId,
					map[string]any{"status": entitledTransaction.Status},
				)
			}
			continue
		}

		// end direction: the store says the entitlement is already over
		if renewal.EndTime.After(run.now) {
			run.end(
				store,
				renewal.NetworkId,
				renewal.TransactionId,
				map[string]any{"status": lastStatus},
			)
		}
	}

	return true, nil
}

// appleReconcileCreditTransaction validates the store-reported transaction
// claims with the SAME validator the webhook path uses and credits through
// the apple_subscription_transaction gate -- ProcessAppleNotification's
// crediting shape, sans the notification ledger (there is no notification;
// the ledger row's notification_uuid is a fresh provenance id). dryRun runs
// the identical validation and network-exists reads but suppresses the
// credit: credited = true then means "would credit".
func appleReconcileCreditTransaction(
	ctx context.Context,
	transactionClaims map[string]any,
	allowedProductIds []string,
	dryRun bool,
) (credited bool, networkId server.Id, returnErr error) {
	notification := AppleNotificationDecodedPayload{
		SignedDate:      server.NowUtc().UnixMilli(),
		TransactionInfo: transactionClaims,
	}
	transaction, err := validateAppleTransaction(notification, allowedProductIds, true)
	if err != nil {
		return false, server.Id{}, err
	}
	networkId = transaction.networkId

	server.Tx(ctx, func(tx server.PgTx) {
		if !appleNetworkExistsInTx(tx, ctx, transaction.networkId) {
			returnErr = errors.New("App Store account token does not name an existing network")
			return
		}
		if dryRun {
			credited = true
			return
		}
		credited = appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), transaction)
	})
	if returnErr != nil {
		return false, networkId, returnErr
	}

	if credited && !dryRun {
		model.UpdateProNetwork(ctx, networkId)
	}
	return credited, networkId, nil
}

// ----- google -----
//
// decision table (Play subscriptionsv2 state -> action):
//
//	ACTIVE -> run the existing PlaySubscriptionRenewal credit path (advisory
//	    lock + in-tx overlap re-check); Renewed = a missed renewal repaired
//	EXPIRED / PENDING_PURCHASE_CANCELED / 410 Gone, renewal active -> end
//	CANCELED with expiry in the future (cancel at period end)      -> nothing
//	CANCELED with expiry passed, renewal active                    -> end
//	PAUSED / ON_HOLD / GRACE / PENDING                             -> nothing
func reconcilePlay(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketGoogle
	packageName := playPackageNameFunc()

	renewals := model.GetReconcileSubscriptionRenewals(
		ctx,
		store,
		run.now.Add(-paymentReconcileWindow),
		paymentReconcileRenewalLimit,
	)
	seenPurchaseTokens := map[string]bool{}
	for _, renewal := range renewals {
		purchaseToken := renewal.PurchaseToken
		if purchaseToken == "" || seenPurchaseTokens[purchaseToken] {
			continue
		}
		seenPurchaseTokens[purchaseToken] = true
		run.examine(store)
		if !run.spend(store) {
			return false, nil
		}

		subUrl := fmt.Sprintf(
			"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
			playPublisherApiBaseUrl,
			packageName,
			purchaseToken,
		)
		sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
			ctx,
			subUrl,
			func(header http.Header) {
				playAuthHeaderFunc(ctx, header)
			},
			server.ResponseJsonObject[*PlaySubscription],
		)
		if err != nil {
			if v, ok := err.(*server.HttpStatusError); ok && v.StatusCode == 410 {
				// the purchase is gone from the store entirely
				if renewal.EndTime.After(run.now) {
					run.end(store, renewal.NetworkId, purchaseToken, map[string]any{"subscription_state": "GONE"})
				}
				continue
			}
			run.record(
				store,
				model.PaymentReconcileActionError,
				&renewal.NetworkId,
				purchaseToken,
				map[string]any{"error": err.Error(), "leg": "status"},
			)
			continue
		}
		if len(sub.LineItems) == 0 {
			continue
		}

		maxExpiryTime := sub.LineItems[0].RequireExpiryTime()
		for _, item := range sub.LineItems[1:] {
			if maxExpiryTime.Before(item.RequireExpiryTime()) {
				maxExpiryTime = item.RequireExpiryTime()
			}
		}

		over := false
		switch sub.SubscriptionState {
		case "SUBSCRIPTION_STATE_EXPIRED", "SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED":
			over = true
		case "SUBSCRIPTION_STATE_CANCELED":
			// cancelled ≠ expired: a cancel with paid-through time remaining
			// keeps its entitlement until expiry
			over = !maxExpiryTime.After(run.now)
		}

		if over {
			if renewal.EndTime.After(run.now) {
				run.end(store, renewal.NetworkId, purchaseToken, map[string]any{"subscription_state": sub.SubscriptionState})
			}
			continue
		}

		if sub.SubscriptionState == "SUBSCRIPTION_STATE_ACTIVE" {
			if run.dryRun {
				// the renewal path's overlap gate, read-only: an existing
				// balance overlapping this expiry means the real run would
				// no-op, otherwise it would credit the renewal
				if _, err := model.GetOverlappingTransferBalance(ctx, purchaseToken, maxExpiryTime); err != nil {
					run.record(
						store,
						model.PaymentReconcileActionWouldCredit,
						&renewal.NetworkId,
						purchaseToken,
						map[string]any{"expiry_time": maxExpiryTime.UTC().Format(time.RFC3339)},
					)
				}
				continue
			}
			// credit direction: run the existing renewal path -- it re-fetches,
			// takes the purchase-token advisory lock, and re-checks the overlap
			// inside the credit tx, so a racing RTDN delivery credits exactly once
			if !run.spend(store) {
				return false, nil
			}
			result, err := PlaySubscriptionRenewal(
				&PlaySubscriptionRenewalArgs{
					NetworkId:      renewal.NetworkId,
					PackageName:    packageName,
					SubscriptionId: sub.LineItems[0].ProductId,
					PurchaseToken:  purchaseToken,
				},
				run.clientSession,
			)
			if err != nil {
				run.record(
					store,
					model.PaymentReconcileActionError,
					&renewal.NetworkId,
					purchaseToken,
					map[string]any{"error": err.Error(), "leg": "credit"},
				)
				continue
			}
			if result.Renewed {
				run.record(
					store,
					model.PaymentReconcileActionCredited,
					&renewal.NetworkId,
					purchaseToken,
					map[string]any{"expiry_time": result.ExpiryTime.UTC().Format(time.RFC3339)},
				)
			}
		}
	}

	return true, nil
}

// ----- solana -----
//
// decision table:
//
//	recorded no_intent payment whose reference NOW resolves to an open intent,
//	    amount >= quote - tolerance -> credit (through the intent one-shot)
//	recorded no_intent payment already credited elsewhere -> clear the record
//	active renewal whose credited tx signature is NOT on-chain (per Helius,
//	    full-history lookup, credited > 1h ago) -> end
//	underpaid records -> left for the operator (still underpaid)
func reconcileSolana(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketSolana

	// leg 1: unfulfilled payments -- money that arrived and bought nothing.
	// The DB sweep costs no store API budget.
	unfulfilledPayments := model.ListUnfulfilledSolanaPayments(
		ctx,
		model.SolanaUnfulfilledReasonNoIntent,
		paymentReconcileRenewalLimit,
	)
	for _, payment := range unfulfilledPayments {
		run.examine(store)
		if model.IsSolanaPaymentCompleted(ctx, payment.TxSignature) {
			// a redelivery already credited this exact payment -- the record
			// is stale. A dry run leaves the record for the real run to clear.
			if !run.dryRun {
				model.RemoveUnfulfilledSolanaPayment(ctx, payment.TxSignature)
			}
			continue
		}
		if len(payment.ReferenceCandidates) == 0 {
			continue
		}
		searchResult, err := model.SearchPaymentIntents(payment.ReferenceCandidates, run.clientSession)
		if err != nil || searchResult == nil {
			continue
		}
		// same amount rule as the webhook: underpaying the resolved quote still
		// buys nothing
		if solanaAmountTolerance < searchResult.ExpectedAmountUsd-payment.TokenAmountUsd {
			continue
		}
		if run.dryRun {
			run.record(
				store,
				model.PaymentReconcileActionWouldCredit,
				searchResult.NetworkId,
				payment.TxSignature,
				map[string]any{"reference": searchResult.PaymentReference},
			)
			continue
		}
		credited, err := solanaCreditPaymentIntent(
			run.clientSession,
			searchResult,
			payment.TxSignature,
			payment.TokenAmountUsd,
		)
		if err != nil {
			run.record(
				store,
				model.PaymentReconcileActionError,
				searchResult.NetworkId,
				payment.TxSignature,
				map[string]any{"error": err.Error(), "leg": "credit"},
			)
			continue
		}
		if credited {
			run.record(
				store,
				model.PaymentReconcileActionCredited,
				searchResult.NetworkId,
				payment.TxSignature,
				map[string]any{"reference": searchResult.PaymentReference},
			)
			model.RemoveUnfulfilledSolanaPayment(ctx, payment.TxSignature)
		} else if model.IsSolanaPaymentCompleted(ctx, payment.TxSignature) {
			// lost the race to a concurrent webhook redelivery of this same
			// payment -- credited either way, the record is resolved
			model.RemoveUnfulfilledSolanaPayment(ctx, payment.TxSignature)
		}
	}

	// leg 2: verify credited payments still exist on-chain. A signature that
	// Helius' full-history lookup cannot find means the credited transaction
	// never landed (dropped or rolled back) -- entitlement without payment.
	renewals := model.GetReconcileSubscriptionRenewals(
		ctx,
		store,
		run.now.Add(-paymentReconcileWindow),
		paymentReconcileRenewalLimit,
	)
	type verifyTarget struct {
		renewal   *model.ReconcileSubscriptionRenewal
		signature string
	}
	targets := []*verifyTarget{}
	signatures := []string{}
	for _, renewal := range renewals {
		if !renewal.EndTime.After(run.now) {
			continue
		}
		// only re-verify well past finality, so a fresh credit is never judged
		// missing while the index catches up
		if renewal.StartTime.After(run.now.Add(-paymentReconcileSolanaFinalityGrace)) {
			continue
		}
		if renewal.TransactionId == "" {
			continue
		}
		signature, ok := model.GetSolanaPaymentIntentSignature(ctx, renewal.TransactionId)
		if !ok {
			continue
		}
		run.examine(store)
		targets = append(targets, &verifyTarget{renewal: renewal, signature: signature})
		signatures = append(signatures, signature)
	}

	// getSignatureStatuses takes up to 256 signatures per call
	for start := 0; start < len(targets); start += 256 {
		end := min(start+256, len(targets))
		if !run.spend(store) {
			return false, nil
		}

		statuses, err := server.HttpPostRequireStatusOk(
			ctx,
			solanaRpcUrlFunc(),
			map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "getSignatureStatuses",
				"params": []any{
					signatures[start:end],
					map[string]any{"searchTransactionHistory": true},
				},
			},
			server.NoCustomHeaders,
			server.ResponseJsonObject[*solanaSignatureStatusesResponse],
		)
		if err != nil {
			return false, fmt.Errorf("getSignatureStatuses: %w", err)
		}
		if len(statuses.Result.Value) != end-start {
			return false, fmt.Errorf(
				"getSignatureStatuses answered %d statuses for %d signatures",
				len(statuses.Result.Value), end-start,
			)
		}

		for i, status := range statuses.Result.Value {
			if status != nil {
				// the payment is on-chain -- all is well
				continue
			}
			target := targets[start+i]
			run.end(
				store,
				target.renewal.NetworkId,
				target.signature,
				map[string]any{
					"reason":    "tx_signature_not_found_on_chain",
					"reference": target.renewal.TransactionId,
				},
			)
		}
	}

	return true, nil
}
