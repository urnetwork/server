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
//     Reconcile credits have no separate write path, so a reconcile credit
//     racing a late webhook for the same event produces exactly one credit.
//   - store affirmatively confirms an exact, already-credited renewal whose
//     Pro metadata is missing -> add one idempotent, zero-byte/zero-revenue Pro
//     marker for that renewal window after rechecking its live local owner.
//   - store says the entitlement is ALREADY over (refunded, revoked, expired
//     and the end-of-period task missed it) -> end it at now. The cancelled ≠
//     expired rule: a cancel-at-period-end with time remaining, or a Stripe
//     cancel inside a paid period that gave no money back, is paid-through
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

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// paymentReconcileWindow is how far back the server-side renewal iteration
// reaches: rows active now or ended within the window are where a missed
// renewal or a missed revocation hides.
const paymentReconcileWindow = 48 * time.Hour

// paymentReconcileLapsedWindow is how far back the App Store and Play legs
// re-check a subscription whose last credited window has already ended. A
// renewal that neither its notification nor a run inside paymentReconcileWindow
// credited (a refused notification, a store or credential outage longer than
// two days, a billing retry or account hold that recovers weeks after expiry)
// is still paid, and the store's status for the subscription names it. Within
// this window the subscriber keeps being checked instead of being forgotten
// two days after the lapse. A lapsed row can only be credited: the end
// direction acts only on a window that is still open.
const paymentReconcileLapsedWindow = 90 * 24 * time.Hour

// paymentReconcileApiBudget caps the reconciler-initiated store API requests
// per store per run. Work left over is picked up by the next hourly run.
// Replaceable by tests.
var paymentReconcileApiBudget = 500

// paymentReconcileRenewalLimit bounds the renewal rows iterated per store per
// run.
var paymentReconcileRenewalLimit = 1000

// appleUnknownToStoreRetryRuns bounds the retry for a renewal whose
// transaction no App Store environment the deployment accepts knows
// (TransactionIdNotFoundError from production and, where apple.yml accepts
// Sandbox, from the sandbox). The answer is retried as a status error, which
// keeps the watermark in place, so a transaction the API briefly does not
// know is not written off. Once it has persisted through this many runs
// within appleUnknownToStoreRetryLookback (about a day of hourly runs), the
// renewal is recorded once as unknown_to_store and is no longer a store
// error. On Main one pre-ledger renewal, keyed on an appTransactionId neither
// environment knows, held the App Store watermark on every run from
// 2026-10-09. Replaceable by tests.
var appleUnknownToStoreRetryRuns = 24

// appleUnknownToStoreRetryLookback is how far back the runs that spent the
// retry are counted.
const appleUnknownToStoreRetryLookback = 7 * 24 * time.Hour

// appleUnknownToStoreReason marks the status errors that spend the retry.
const appleUnknownToStoreReason = "transaction_not_found"

// paymentReconcileSolanaFinalityGrace: only re-verify on-chain payments
// credited at least this long ago, so a just-credited transaction is never
// judged missing while the chain (or Helius' index) catches up.
const paymentReconcileSolanaFinalityGrace = 1 * time.Hour

// ----- store credential presence (skip-and-log seams) -----
//
// Local/test envs carry no store credentials; a store without credentials is
// skipped with a skipped_store audit row. Replaceable only by hermetic tests
// (the S5 Play seam pattern); production never mutates these.

// simpleResourceNonblankString requires the selected YAML value to be an
// actual string scalar. Typed YAML decoding deliberately coerces numeric
// scalars into strings, which is unsafe for credential-presence checks.
func simpleResourceNonblankString(resource *server.SimpleResource, path ...string) bool {
	_, err := simpleResourceNonblankStringValue(resource, path...)
	return err == nil
}

func stripeReconcileCredentialsPresent() bool {
	resource, err := server.Vault.SimpleResource("stripe.yml")
	if err != nil {
		return false
	}
	return simpleResourceNonblankString(resource, "api", "token")
}

var stripeReconcileHasCredentials = stripeReconcileCredentialsPresent

var appleReconcileHasCredentials = func() bool {
	return appleReconcileCredentialsFunc() != nil
}

func playReconcileCredentialsPresent() bool {
	resource, err := server.Vault.SimpleResource("google.yml")
	if err != nil {
		return false
	}
	if !simpleResourceNonblankString(resource, "webhook", "package_name") ||
		!simpleResourceNonblankString(resource, "oauth", "client_id") ||
		!simpleResourceNonblankString(resource, "oauth", "client_secret") ||
		!simpleResourceNonblankString(resource, "oauth", "refresh_token") {
		return false
	}
	var credentials struct {
		Webhook struct {
			PackageName string `yaml:"package_name"`
		} `yaml:"webhook"`
		OAuth struct {
			ClientId     string `yaml:"client_id"`
			ClientSecret string `yaml:"client_secret"`
			RefreshToken string `yaml:"refresh_token"`
		} `yaml:"oauth"`
	}
	if err := resource.UnmarshalYamlE(&credentials); err != nil ||
		strings.TrimSpace(credentials.Webhook.PackageName) == "" ||
		strings.TrimSpace(credentials.OAuth.ClientId) == "" ||
		strings.TrimSpace(credentials.OAuth.ClientSecret) == "" ||
		strings.TrimSpace(credentials.OAuth.RefreshToken) == "" {
		return false
	}
	resource, err = server.Config.SimpleResource("play.yml")
	if err != nil {
		return false
	}
	var skus Skus
	return resource.UnmarshalYamlE(&skus) == nil && 0 < len(skus.Skus)
}

var playReconcileHasCredentials = playReconcileCredentialsPresent

func solanaReconcileCredentialsPresent() bool {
	resource, err := server.Vault.SimpleResource("helius.yml")
	if err != nil {
		return false
	}
	return simpleResourceNonblankString(resource, "helius", "api_key")
}

var solanaReconcileHasCredentials = solanaReconcileCredentialsPresent

// The x402 leg reads only our own settled_not_granted events and retries the
// grant locally; it needs no store credentials, so it always runs.
var x402ReconcileHasCredentials = func() bool {
	return true
}

// The Monero and Zcash legs always run: their first leg reads only our own
// receipts, and the legs that ask the wallet daemon run when its vault file is
// there (a coin that was never on has nothing for them to check).
var moneroReconcileHasCredentials = func() bool {
	return true
}

var zcashReconcileHasCredentials = func() bool {
	return true
}

// The TAO, Bitcoin and ETH legs always run, like Monero's and Zcash's: the
// first reads only our receipts, and the chain legs run when the coin's
// chain source is in the vault.
var cryptoRailReconcileHasCredentials = func() bool {
	return true
}

// ----- apple App Store Server API client -----

// The only genuinely new credential reconciliation needs: App Store Server
// API client credentials, from vault/<env>/apple.yml --
// app_store_server_api_key_id, issuer_id, private_key (the .p8 contents).
// bundle_id, product_ids and environments are read from the existing
// app_store_notifications block.
type appleServerApiCredentials struct {
	KeyId      string
	IssuerId   string
	PrivateKey string
	BundleId   string
	ProductIds []string
	// The App Store environments ("Production", "Sandbox") whose notifications
	// this deployment accepts and credits: the same list the webhook verifier
	// enforces.
	Environments []string
}

// allowsEnvironment reports whether this deployment accepts the App Store
// environment's notifications, so its renewals can name that environment's
// transactions.
func (self *appleServerApiCredentials) allowsEnvironment(environment string) bool {
	return slices.Contains(self.Environments, environment)
}

func appleReconcileCredentialsFromVault() *appleServerApiCredentials {
	resource, err := server.Vault.SimpleResource("apple.yml")
	if err != nil {
		return nil
	}
	if !simpleResourceNonblankString(resource, "app_store_server_api_key_id") ||
		!simpleResourceNonblankString(resource, "issuer_id") ||
		!simpleResourceNonblankString(resource, "private_key") ||
		!simpleResourceNonblankString(resource, "app_store_notifications", "bundle_id") {
		return nil
	}
	var config struct {
		KeyId                 string `yaml:"app_store_server_api_key_id"`
		IssuerId              string `yaml:"issuer_id"`
		PrivateKey            string `yaml:"private_key"`
		AppStoreNotifications *struct {
			BundleId     string   `yaml:"bundle_id"`
			ProductIds   []string `yaml:"product_ids"`
			Environments []string `yaml:"environments"`
		} `yaml:"app_store_notifications"`
	}
	if err := resource.UnmarshalYamlE(&config); err != nil ||
		strings.TrimSpace(config.KeyId) == "" || strings.TrimSpace(config.IssuerId) == "" || strings.TrimSpace(config.PrivateKey) == "" ||
		config.AppStoreNotifications == nil || strings.TrimSpace(config.AppStoreNotifications.BundleId) == "" {
		return nil
	}
	return &appleServerApiCredentials{
		KeyId:        config.KeyId,
		IssuerId:     config.IssuerId,
		PrivateKey:   config.PrivateKey,
		BundleId:     config.AppStoreNotifications.BundleId,
		ProductIds:   config.AppStoreNotifications.ProductIds,
		Environments: config.AppStoreNotifications.Environments,
	}
}

var appleReconcileCredentialsFunc = appleReconcileCredentialsFromVault

// replaceable by tests standing up a fake App Store Server API
var appleAppStoreServerApiBaseUrl = "https://api.storekit.itunes.apple.com"

// The sandbox App Store Server API, which holds TestFlight and App Review
// purchases. The same API key signs requests to both environments.
// Replaceable by tests standing up a fake.
var appleAppStoreServerApiSandboxBaseUrl = "https://api.storekit-sandbox.itunes.apple.com"

// The App Store environment names in notifications, signed transactions and
// apple.yml app_store_notifications.environments.
const appleEnvironmentProduction = "Production"
const appleEnvironmentSandbox = "Sandbox"

// Apple's TransactionIdNotFoundError. Production answers it for a transaction
// id that belongs to the sandbox environment, which only the sandbox API
// knows.
const appleTransactionIdNotFoundErrorCode = 4040010

// appleTransactionIdNotFound reports whether err is Apple's 404
// TransactionIdNotFoundError. Every other failure, including a 404 with a
// different error code or a body that is not Apple's error object, is not.
func appleTransactionIdNotFound(err error) bool {
	var statusErr *server.HttpStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		return false
	}
	var apiError struct {
		ErrorCode int64 `json:"errorCode"`
	}
	if json.Unmarshal([]byte(statusErr.ResponseBody), &apiError) != nil {
		return false
	}
	return apiError.ErrorCode == appleTransactionIdNotFoundErrorCode
}

// appleGetSubscriptionStatuses calls Get All Subscription Statuses for the
// transaction in the environment that baseUrl names.
func appleGetSubscriptionStatuses(
	ctx context.Context,
	baseUrl string,
	transactionId string,
) (*appleSubscriptionStatusesResponse, error) {
	statusesUrl := fmt.Sprintf(
		"%s/inApps/v1/subscriptions/%s",
		baseUrl,
		url.PathEscape(transactionId),
	)
	return server.HttpGetRequireStatusOk[*appleSubscriptionStatusesResponse](
		ctx,
		statusesUrl,
		func(header http.Header) {
			appleServerApiAuthHeaderFunc(ctx, header)
		},
		server.ResponseJsonObject[*appleSubscriptionStatusesResponse],
	)
}

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

// Only documented provider statuses can authorize an entitlement decision.
// Validate the complete response before selecting an entitled transaction, so
// an earlier valid row cannot hide a malformed later group. Empty lists retain
// their existing no-conclusion behavior.
func validateAppleSubscriptionStatuses(statuses *appleSubscriptionStatusesResponse) error {
	if statuses == nil {
		return errors.New("missing App Store subscription statuses")
	}
	for _, group := range statuses.Data {
		if group == nil {
			return errors.New("missing App Store subscription status group")
		}
		for _, transaction := range group.LastTransactions {
			if transaction == nil {
				return errors.New("missing App Store subscription transaction status")
			}
			switch transaction.Status {
			case 1, 2, 3, 4, 5:
			default:
				return fmt.Errorf("unsupported App Store subscription status %d", transaction.Status)
			}
		}
	}
	return nil
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
	Id                  string            `json:"id"`
	Status              string            `json:"status"`
	CancelAtPeriodEnd   bool              `json:"cancel_at_period_end"`
	Metadata            map[string]string `json:"metadata"`
	CancellationDetails struct {
		Reason string `json:"reason"`
	} `json:"cancellation_details"`
}

// Stripe's cancellation_details.reason for a cancel someone asked for: the
// customer in the billing portal, or an operator in the Dashboard or the API.
// Stripe's own cancels say payment_failed or payment_disputed.
const stripeCancellationReasonRequested = "cancellation_requested"

// stripeSubscriptionOver applies the cancelled ≠ expired rule to a Stripe
// subscription status: only statuses after which Stripe bills nothing more.
// cancel_at_period_end with status "active" is paid-through time and is never
// acted on, and neither is a cancel inside the paid period that gave no money
// back (stripeCancelLeavesPaidPeriod).
func stripeSubscriptionOver(status string) bool {
	switch status {
	case "canceled", "incomplete_expired", "unpaid":
		return true
	}
	return false
}

// stripeSubscriptionEntitled names the states where Stripe affirmatively says
// the current subscription period is entitled. past_due remains conservative:
// it may still be collecting, but it is not an authoritative active/granted
// verdict from which to reconstruct missing entitlement metadata.
func stripeSubscriptionEntitled(status string) bool {
	switch status {
	case "active", "trialing":
		return true
	}
	return false
}

type stripeReconcileInvoiceExpanded struct {
	Id     string `json:"id"`
	Status string `json:"status"`
	// in the currency's minor unit; 0 for a trial or a fully discounted invoice
	AmountPaid int64 `json:"amount_paid"`
	// the id of the charge that paid it. API versions from 2025-03-31 no longer
	// name it here.
	Charge       json.RawMessage              `json:"charge"`
	Lines        *StripeLineItems             `json:"lines"`
	Subscription *stripeReconcileSubscription `json:"subscription"`
}

// The charge that paid a renewal's invoice.
type stripeReconcileCharge struct {
	Id             string `json:"id"`
	Status         string `json:"status"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Refunded       bool   `json:"refunded"`
	Disputed       bool   `json:"disputed"`
}

// Whether the charge still holds all the money it took: it succeeded, none of
// it was refunded, and it was never disputed. Stripe keeps the invoice "paid"
// through a refund or a dispute, so only the charge can tell. An inquiry or a
// dispute the merchant won still marks it disputed.
func (self *stripeReconcileCharge) keptWholePayment() bool {
	return self.Status == "succeeded" &&
		0 < self.Amount &&
		self.AmountRefunded == 0 &&
		!self.Refunded &&
		!self.Disputed
}

// Whether the window a canceled subscription's invoice paid for may still be
// running on paid time: someone asked for the cancel (not Stripe, for a failed
// or disputed payment), the invoice took payment (stripeInvoiceTookPayment), and
// its paid period, which ends at paidPeriodEnd, has not ended. A cancel stops
// the renewals; it gives no money back. The invoice's charge decides the rest
// (keptWholePayment).
func stripeCancelLeavesPaidPeriod(
	invoice *stripeReconcileInvoiceExpanded,
	paidPeriodEnd time.Time,
	now time.Time,
) bool {
	return invoice != nil &&
		stripeInvoiceTookPayment(invoice.Status, invoice.AmountPaid) &&
		invoice.Subscription != nil &&
		invoice.Subscription.Status == "canceled" &&
		invoice.Subscription.CancellationDetails.Reason == stripeCancellationReasonRequested &&
		now.Before(paidPeriodEnd)
}

// Whether the window an invoice paid for keeps the manual-payment grace that
// its end already carries: Stripe gave up on the subscription's renewal (the
// final action kept it unpaid, or canceled it for payment_failed), and the
// invoice took payment, as for a requested cancel's paid period
// (stripeCancelLeavesPaidPeriod). A window no payment paid for (a trial) has no
// manual-payment grace to keep: it ends when Stripe gives up (owner,
// 2026-11-13). A refund or a dispute of the payment ends the window through its
// own webhook.
func stripeNonPaymentLeavesManualPaymentGrace(invoice *stripeReconcileInvoiceExpanded) bool {
	if invoice == nil || invoice.Subscription == nil {
		return false
	}
	gaveUp := invoice.Subscription.Status == "unpaid" ||
		(invoice.Subscription.Status == "canceled" &&
			invoice.Subscription.CancellationDetails.Reason == stripeCancellationReasonPaymentFailed)
	return gaveUp && stripeInvoiceTookPayment(invoice.Status, invoice.AmountPaid)
}

// Reads the charge that paid a renewal's invoice.
func stripeReconcileFetchCharge(ctx context.Context, chargeId string) (*stripeReconcileCharge, error) {
	charge, err := server.HttpGetRequireStatusOk[*stripeReconcileCharge](
		ctx,
		fmt.Sprintf("%s/v1/charges/%s", stripeApiBaseUrl, url.PathEscape(chargeId)),
		stripeAuthHeader,
		server.ResponseJsonObject[*stripeReconcileCharge],
	)
	if err != nil {
		return nil, err
	}
	if charge == nil || charge.Id != chargeId {
		return nil, fmt.Errorf("charge %s: Stripe returned another or a malformed charge", chargeId)
	}
	return charge, nil
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
			Err                any     `json:"err"`
			ConfirmationStatus *string `json:"confirmationStatus"`
		} `json:"value"`
	} `json:"result"`
}

// ----- run orchestration -----

// PaymentReconcileRunOptions selects how a reconcile pass runs. The zero
// value (or nil) is the hourly task's behavior: every store, repairs
// applied.
type PaymentReconcileRunOptions struct {
	// DryRun audits without repairing: every store API READ happens for real,
	// but every write is suppressed -- no credit, no ended entitlement, no
	// Pro-metadata repair, unfulfilled-record clearing, and no watermark
	// advance (a dry run must not eat the incremental window a later real run
	// needs). Each suppressed repair is recorded as a would_credit / would_end /
	// would_repair_entitlement audit event with the same evidence and details
	// the real repair would carry, tagged dry_run.
	DryRun bool
	// Stores limits the pass to the named stores (model.PaymentReconcileStores);
	// empty runs all of them. Filtered-out stores are untouched entirely: no
	// events, no watermark.
	Stores []string
	// The Solana store's receiver leg (solana_receiver_sweep.go); nil runs
	// it with DefaultSolanaReceiverSweepSettings.
	SolanaReceiverSweepSettings *SolanaReceiverSweepSettings
}

// PaymentReconcileStoreResult is one store's tally for the run. In a dry run,
// the action counts describe the corresponding would_* events.
type PaymentReconcileStoreResult struct {
	Examined             int `json:"examined"`
	Credited             int `json:"credited"`
	Ended                int `json:"ended"`
	EntitlementsRepaired int `json:"entitlements_repaired"`
	Errors               int `json:"errors"`
	// Sandbox counts examined App Store renewals that only the sandbox
	// environment knows (TestFlight and App Review purchases).
	Sandbox int `json:"sandbox,omitempty"`
	// UnknownToStore counts examined App Store renewals whose transaction no
	// accepted environment knows, past the bounded retry (unknown_to_store).
	UnknownToStore int `json:"unknown_to_store,omitempty"`
	// examined Stripe renewals whose subscription was canceled inside a paid
	// period nothing was refunded or disputed from, kept to the period's end
	CanceledPaidThrough int  `json:"canceled_paid_through,omitempty"`
	Skipped             bool `json:"skipped,omitempty"`
	BudgetExhausted     bool `json:"budget_exhausted,omitempty"`
}

type PaymentReconcileRunResult struct {
	RunId  server.Id `json:"run_id"`
	DryRun bool      `json:"dry_run,omitempty"`
	// In a dry run, the action counts describe the corresponding would_* events.
	Credited             int                                     `json:"credited"`
	Ended                int                                     `json:"ended"`
	EntitlementsRepaired int                                     `json:"entitlements_repaired"`
	Errors               int                                     `json:"errors"`
	SkippedStores        []string                                `json:"skipped_stores,omitempty"`
	StoreResults         map[string]*PaymentReconcileStoreResult `json:"store_results,omitempty"`
}

type paymentReconcileRun struct {
	clientSession *session.ClientSession
	connOwner     server.PgConn
	runId         server.Id
	now           time.Time
	dryRun        bool
	// the Solana receiver leg's tunables; nil reads the defaults
	solanaReceiverSweepSettings *SolanaReceiverSweepSettings

	// remaining store API budget for the store currently reconciling
	budget int

	credited             int
	ended                int
	entitlementsRepaired int
	errors               int
	skipped              []string
	// per-store detail folded into the heartbeat row
	storeDetails map[string]map[string]any
	// per-store tallies for the run result (the CLI summary)
	storeResults map[string]*PaymentReconcileStoreResult
}

func paymentReconcileStoreCanAdvanceWatermark(complete bool, dryRun bool, errorsBefore int, errorsAfter int) bool {
	return complete && !dryRun && errorsAfter == errorsBefore
}

var addPaymentReconciliationEvent = model.AddPaymentReconciliationEventInConn

func (self *paymentReconcileRun) record(
	store string,
	action string,
	networkId *server.Id,
	evidence string,
	details map[string]any,
) bool {
	switch action {
	case model.PaymentReconcileActionCredited, model.PaymentReconcileActionWouldCredit:
		self.credited += 1
		self.storeResult(store).Credited += 1
	case model.PaymentReconcileActionEnded, model.PaymentReconcileActionWouldEnd:
		self.ended += 1
		self.storeResult(store).Ended += 1
	case model.PaymentReconcileActionEntitlementRepaired, model.PaymentReconcileActionWouldRepairEntitlement:
		self.entitlementsRepaired += 1
		self.storeResult(store).EntitlementsRepaired += 1
	case model.PaymentReconcileActionError:
		self.errors += 1
		self.storeResult(store).Errors += 1
	case model.PaymentReconcileActionSkippedStore:
		self.skipped = append(self.skipped, store)
		self.storeResult(store).Skipped = true
	}
	if err := addPaymentReconciliationEvent(self.connOwner, self.clientSession.Ctx, &model.PaymentReconciliationEvent{
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
		return false
	}
	return true
}

// recordCreditUnfulfillable is the narrow exception to record's best-effort
// contract. This event is the only durable handle for a paid invoice whose
// destination is deleted or unresolved. If it is not persisted, count a store
// error so the watermark stays fixed and the ordinary overlap retries it.
// Completed credits/ends/metadata repairs retain their existing best-effort
// audit behavior: their durable business writes must not be re-executed merely
// because the secondary audit insert failed.
func (self *paymentReconcileRun) recordCreditUnfulfillable(store string, evidence string, reason string) {
	self.recordCreditUnfulfillableDetails(store, evidence, map[string]any{"reason": reason})
}

// recordCreditUnfulfillableDetails is recordCreditUnfulfillable with the
// store identities support needs to place the payment (details must carry
// "reason").
func (self *paymentReconcileRun) recordCreditUnfulfillableDetails(store string, evidence string, details map[string]any) {
	details["leg"] = "credit"
	if !self.record(
		store,
		model.PaymentReconcileActionCreditUnfulfillable,
		nil,
		evidence,
		details,
	) {
		self.errors += 1
		self.storeResult(store).Errors += 1
	}
}

var addPaymentReconciliationEventOnce = model.AddPaymentReconciliationEventOnceInConn

// recordCreditUnfulfillableOnce records a paid store purchase whose owner the
// reconciler cannot establish (details must carry "reason"). The reconciler
// never guesses an owner; the event is the durable handle the owner acts on.
// It is a disposition, not a per-run failure, so it is written once per store
// object: later runs that see the same purchase add nothing. A dry run writes
// its dry_run row like every dry-run event.
func (self *paymentReconcileRun) recordCreditUnfulfillableOnce(
	store string,
	networkId *server.Id,
	evidence string,
	details map[string]any,
) {
	details["leg"] = "credit"
	self.recordOnce(store, model.PaymentReconcileActionCreditUnfulfillable, networkId, evidence, details)
}

// recordOnce writes a terminal disposition (credit_unfulfillable,
// unknown_to_store) once per store object: later runs that see the same
// object add nothing. A dry run writes its dry_run row like every dry-run
// event.
func (self *paymentReconcileRun) recordOnce(
	store string,
	action string,
	networkId *server.Id,
	evidence string,
	details map[string]any,
) {
	if self.dryRun {
		self.record(store, action, networkId, evidence, details)
		return
	}
	if _, err := addPaymentReconciliationEventOnce(self.connOwner, self.clientSession.Ctx, &model.PaymentReconciliationEvent{
		RunId:     self.runId,
		Store:     store,
		NetworkId: networkId,
		Action:    action,
		Evidence:  evidence,
		Details:   details,
	}); err != nil {
		// the only durable handle was not written: count a store error so the
		// run shows it and the next run writes it
		glog.Errorf("[reconcile]could not record %s/%s event: %s\n", store, action, err)
		self.errors += 1
		self.storeResult(store).Errors += 1
	}
}

// Terminal destination exceptions retain durable disposition evidence. All
// provider, schema, and local persistence failures continue to block progress.
func (self *paymentReconcileRun) recordStripeCreditError(invoice *StripeEventInvoiceObject, err error) {
	evidence := invoice.Id
	var unresolved *stripeInvoiceDestinationUnresolvedError
	switch {
	case errors.Is(err, model.ErrPaymentNetworkNotFound):
		self.recordCreditUnfulfillable(model.SubscriptionMarketStripe, evidence, "destination_deleted")
	case errors.As(err, &unresolved):
		// a paid invoice that names no network (S11: no customer-email guess)
		details := unresolved.details()
		details["amount_total"] = invoice.Total
		details["currency"] = invoice.Currency
		self.recordCreditUnfulfillableDetails(model.SubscriptionMarketStripe, evidence, details)
	case errors.Is(err, errStripeInvoiceDestinationUnresolved):
		self.recordCreditUnfulfillable(model.SubscriptionMarketStripe, evidence, "destination_unresolved")
	default:
		self.record(
			model.SubscriptionMarketStripe,
			model.PaymentReconcileActionError,
			nil,
			evidence,
			map[string]any{"error": err.Error(), "leg": "credit"},
		)
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

// countSandbox counts one examined store object that only the store's sandbox
// environment knows. The count is folded into the heartbeat row, so these
// renewals stay visible without an audit row each run.
func (self *paymentReconcileRun) countSandbox(store string) {
	summary := self.storeResult(store)
	summary.Sandbox += 1
	self.storeDetail(store)["sandbox"] = summary.Sandbox
}

// countUnknownToStore counts one examined store object the store does not
// know, past the bounded retry. The count is folded into the heartbeat row,
// so the object stays visible every run after its one unknown_to_store row.
func (self *paymentReconcileRun) countUnknownToStore(store string) {
	summary := self.storeResult(store)
	summary.UnknownToStore += 1
	self.storeDetail(store)[model.PaymentReconcileActionUnknownToStore] = summary.UnknownToStore
}

// Counts one examined renewal kept on the paid time of a canceled
// subscription. A kept window writes no event of its own, so the count is
// folded into the heartbeat row.
func (self *paymentReconcileRun) countCanceledPaidThrough(store string) {
	summary := self.storeResult(store)
	summary.CanceledPaidThrough += 1
	self.storeDetail(store)["canceled_paid_through"] = summary.CanceledPaidThrough
}

// creditAction names the audit action for a credit repair: the real action,
// or its would_ dry-run form.
func (self *paymentReconcileRun) creditAction() string {
	if self.dryRun {
		return model.PaymentReconcileActionWouldCredit
	}
	return model.PaymentReconcileActionCredited
}

func (self *paymentReconcileRun) repairEntitlement(
	store string,
	renewal *model.ReconcileSubscriptionRenewal,
	evidence string,
	details map[string]any,
) {
	repaired, err := model.RepairReconciledProEntitlementInConn(self.connOwner,
		self.clientSession.Ctx,
		store,
		renewal,
		self.now,
		self.dryRun,
	)
	if err != nil {
		self.record(
			store,
			model.PaymentReconcileActionError,
			&renewal.NetworkId,
			evidence,
			map[string]any{"error": err.Error(), "leg": "entitlement_repair"},
		)
		return
	}
	if !repaired {
		return
	}
	action := model.PaymentReconcileActionEntitlementRepaired
	if self.dryRun {
		action = model.PaymentReconcileActionWouldRepairEntitlement
	}
	self.record(store, action, &renewal.NetworkId, evidence, details)
}

// paymentReconcileEndScope names the one store object the store reported
// over: the transactions it paid with (Stripe invoice ids, App Store
// transaction ids, Solana references) or its Play purchase token. An end
// touches only the renewals that object paid for; a network's other
// subscriptions in the same market keep their paid windows.
type paymentReconcileEndScope struct {
	transactionIds []string
	purchaseToken  string
}

// end applies the "store says it is already over" repair to exactly the
// scoped store object -- or, in a dry run, records the would_end without
// touching anything. Callers apply the cancelled ≠ expired rule and the
// end_time > now check BEFORE calling, so a would_end names exactly the
// entitlement a real run would have ended.
func (self *paymentReconcileRun) end(
	store string,
	networkId server.Id,
	scope paymentReconcileEndScope,
	evidence string,
	details map[string]any,
) {
	// the audit row names exactly what the end touched
	if 0 < len(scope.transactionIds) {
		details["transaction_ids"] = scope.transactionIds
	}
	if self.dryRun {
		self.record(store, model.PaymentReconcileActionWouldEnd, &networkId, evidence, details)
		return
	}
	var ended bool
	var err error
	if scope.purchaseToken != "" {
		ended, err = model.EndReconciledEntitlementForNetworkPurchaseTokenInConn(self.connOwner, self.clientSession.Ctx, networkId, store, scope.purchaseToken, self.now)
	} else {
		ended, err = model.EndReconciledEntitlementForNetworkTransactionsInConn(self.connOwner, self.clientSession.Ctx, networkId, store, scope.transactionIds, self.now)
	}
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

func (self *paymentReconcileRun) storeErrorCount(store string) int {
	if summary := self.storeResults[store]; summary != nil {
		return summary.Errors
	}
	return 0
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
	if watermark, ok := model.GetPaymentReconcileWatermarkInConn(self.connOwner, self.clientSession.Ctx, store); ok {
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

// RunPaymentReconciliation is one reconcile pass over every store with
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
		return runPaymentReconciliation(nil, clientSession, options), nil
	}

	// One real run at a time across all entry points: a session-level
	// advisory lock held on a direct connection for the run's whole duration
	// (the run spans many transactions and store API calls, so a tx-scoped
	// lock cannot cover it). Try-lock: a second caller reports busy instead
	// of queueing. OptNoRetry pins the callback to a single attempt -- the
	// pool must never re-run a completed reconcile pass on a dropped
	// connection. Every model operation reuses this session for its sequential
	// transactions. Retaining the lock while acquiring another PostgreSQL
	// connection can exhaust the pool before any reconciliation write starts.
	var result *PaymentReconcileRunResult
	locked := false
	server.MaintenanceDb(clientSession.Ctx, func(conn server.PgConn) {
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
		result = runPaymentReconciliation(conn, clientSession, options)
	}, server.OptNoRetry())
	if !locked {
		return nil, ErrPaymentReconcileRunInProgress
	}
	return result, nil
}

// paymentReconcileStoreLeg is one store's pass of a reconciliation run.
type paymentReconcileStoreLeg struct {
	store          string
	hasCredentials func() bool
	// reconcile returns complete = false when it stopped early (budget);
	// the watermark only advances on a complete, error-free pass
	reconcile func(run *paymentReconcileRun, since time.Time) (complete bool, err error)
}

// paymentReconcileStoreLegs are the stores' legs, one per
// model.PaymentReconcileStores entry in its order (a test holds them to it),
// made for each run.
func paymentReconcileStoreLegs() []paymentReconcileStoreLeg {
	return []paymentReconcileStoreLeg{
		{model.SubscriptionMarketStripe, stripeReconcileHasCredentials, reconcileStripe},
		{model.SubscriptionMarketApple, appleReconcileHasCredentials, reconcileApple},
		{model.SubscriptionMarketGoogle, playReconcileHasCredentials, reconcilePlay},
		{model.SubscriptionMarketSolana, solanaReconcileHasCredentials, reconcileSolana},
		{model.SubscriptionMarketX402, x402ReconcileHasCredentials, reconcileX402},
		{model.SubscriptionMarketMonero, moneroReconcileHasCredentials, reconcileMonero},
		{model.SubscriptionMarketZcash, zcashReconcileHasCredentials, reconcileZcash},
		{model.SubscriptionMarketBittensor, cryptoRailReconcileHasCredentials, reconcileBittensor},
		{model.SubscriptionMarketBitcoin, cryptoRailReconcileHasCredentials, reconcileBitcoin},
		{model.SubscriptionMarketEthereum, cryptoRailReconcileHasCredentials, reconcileEthereum},
	}
}

func runPaymentReconciliation(
	connOwner server.PgConn,
	clientSession *session.ClientSession,
	options *PaymentReconcileRunOptions,
) *PaymentReconcileRunResult {
	run := &paymentReconcileRun{
		clientSession:               clientSession,
		connOwner:                   connOwner,
		runId:                       server.NewId(),
		now:                         server.NowUtc(),
		dryRun:                      options.DryRun,
		solanaReceiverSweepSettings: options.SolanaReceiverSweepSettings,
		storeDetails:                map[string]map[string]any{},
		storeResults:                map[string]*PaymentReconcileStoreResult{},
	}
	if run.solanaReceiverSweepSettings == nil {
		run.solanaReceiverSweepSettings = DefaultSolanaReceiverSweepSettings()
	}

	stores := paymentReconcileStoreLegs()
	if 0 < len(options.Stores) {
		selected := map[string]bool{}
		for _, store := range options.Stores {
			selected[store] = true
		}
		stores = slices.DeleteFunc(stores, func(storeReconcile paymentReconcileStoreLeg) bool {
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
		storeErrorsBefore := run.storeErrorCount(storeReconcile.store)

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
			} else if paymentReconcileStoreCanAdvanceWatermark(
				complete,
				run.dryRun,
				storeErrorsBefore,
				run.storeErrorCount(storeReconcile.store),
			) {
				// the next run's listing starts here (minus the overlap
				// backoff); a dry run never advances the watermark -- the
				// un-eaten window is what the later real run reconciles
				if err := model.SetPaymentReconcileWatermarkInConn(connOwner, clientSession.Ctx, storeReconcile.store, run.now); err != nil {
					glog.Errorf("[reconcile]%s: could not advance watermark: %s\n", storeReconcile.store, err)
				}
			}
		}()
	}

	// every run leaves a heartbeat -- a run that repaired nothing writes ONLY
	// this row, and a missing heartbeat is how an operator sees the task died
	heartbeatDetails := map[string]any{
		"credited":              run.credited,
		"ended":                 run.ended,
		"entitlements_repaired": run.entitlementsRepaired,
		"errors":                run.errors,
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
		RunId:                run.runId,
		DryRun:               run.dryRun,
		Credited:             run.credited,
		Ended:                run.ended,
		EntitlementsRepaired: run.entitlementsRepaired,
		Errors:               run.errors,
		SkippedStores:        run.skipped,
		StoreResults:         run.storeResults,
	}
}

// ----- stripe -----
//
// decision table (observed store state -> action):
//
//	paid subscription invoice since watermark, not in stripe_invoice ledger -> credit
//	    (through stripeHandleInvoicePaid, gated by the ledger)
//	subscription status canceled / incomplete_expired / unpaid, renewal active -> end
//	    that invoice's renewal only (every open renewal is checked against its
//	    own invoice's subscription; a network's other subscriptions keep theirs)
//	canceled on request inside the invoice's paid period, its charge neither
//	    refunded nor disputed                                                  -> nothing
//	    until the paid period ends, then end (a cancel gives no money back,
//	    and the manual-payment grace is for renewals that may yet be paid)
//	unpaid, or canceled for payment_failed, exact paid window whose invoice
//	    took payment                                                           -> nothing
//	    through the manual-payment grace
//	unpaid, or canceled for payment_failed, a window no payment paid for (a
//	    trial)                                                                 -> end
//	    (owner, 2026-11-13: no grace for a network that never paid)
//	past_due (or another state short of active) on a window no payment paid
//	    for, a day past its period                                             -> end
//	    (the free period's grace, subscription_stripe_grace.go)
//	status active + cancel_at_period_end (time remaining)                      -> nothing
//	anything else                                                              -> nothing
func reconcileStripe(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe

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
			if _, credited := model.GetStripeInvoiceNetworkIdInConn(run.connOwner, ctx, invoice.Id); credited {
				// the webhook already handled this one
				continue
			}
			if !run.spend(store) {
				return false, nil
			}
			if run.dryRun {
				credit, err := stripeResolveInvoiceCredit(invoice.Id, run.clientSession)
				if err != nil {
					run.recordStripeCreditError(invoice, err)
					continue
				}
				if credit == nil {
					// a non-subscription invoice -- the real run would do nothing
					continue
				}
				if !model.NetworkExistsInConn(run.connOwner, ctx, credit.networkId) {
					// Match the real credit's lifecycle-lock refusal without
					// consuming its ledger or pretending a different destination
					// would receive the paid invoice.
					run.recordCreditUnfulfillable(store, invoice.Id, "destination_deleted")
					continue
				}
				run.record(
					store,
					model.PaymentReconcileActionWouldCredit,
					&credit.networkId,
					invoice.Id,
					map[string]any{
						"total":        invoice.Total,
						"subscription": credit.subscriptionId,
					},
				)
				continue
			}
			// the repair IS the webhook path: same network resolution, same
			// stripe_invoice ledger gate, so a racing late webhook delivery for
			// the same invoice credits exactly once between the two of them
			if _, err := stripeHandleInvoicePaidInConn(run.connOwner, invoice, run.clientSession); err != nil {
				run.recordStripeCreditError(invoice, err)
				continue
			}
			if networkId, credited := model.GetStripeInvoiceNetworkIdInConn(run.connOwner, ctx, invoice.Id); credited {
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

	// leg 2: server-side -- for every open renewal, does Stripe agree the
	// subscription that paid for it is still alive? Each renewal is checked
	// against its own invoice: a network can hold several subscriptions at
	// once, and one subscription's end must never close another's paid window.
	// An invoice can also sit on more than one network (a subscription moved
	// by support), so each network's renewal is checked on its own.
	renewals := model.GetReconcileSubscriptionRenewalsInConn(run.connOwner,
		ctx,
		store,
		run.now.Add(-paymentReconcileWindow),
		paymentReconcileRenewalLimit,
	)
	type stripeRenewalInvoice struct {
		networkId server.Id
		invoiceId string
	}
	checkedRenewalInvoices := map[stripeRenewalInvoice]bool{}
	for _, renewal := range renewals {
		if !renewal.EndTime.After(run.now) {
			// already over on our side -- nothing to end
			continue
		}
		if renewal.TransactionId == "" {
			continue
		}
		renewalInvoice := stripeRenewalInvoice{networkId: renewal.NetworkId, invoiceId: renewal.TransactionId}
		if checkedRenewalInvoices[renewalInvoice] {
			continue
		}
		checkedRenewalInvoices[renewalInvoice] = true
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
		paidPeriodEnd, paidWindow := stripeRenewalPaidPeriodEndInConn(run.connOwner, ctx, renewal, fullInvoice)
		// Preserve a verified paid window through manual-payment grace even
		// if Stripe marks the renewal unpaid or auto-cancels it early. A
		// customer-requested cancellation is not a payment failure, and a
		// window no payment paid for has no manual-payment grace.
		if paidWindow && stripeNonPaymentLeavesManualPaymentGrace(fullInvoice) {
			continue
		}
		tookPayment := stripeInvoiceTookPayment(fullInvoice.Status, fullInvoice.AmountPaid)
		if stripeSubscriptionOver(fullInvoice.Subscription.Status) {
			details := map[string]any{
				"subscription_status": fullInvoice.Subscription.Status,
				"took_payment":        tookPayment,
			}
			if reason := fullInvoice.Subscription.CancellationDetails.Reason; reason != "" {
				details["cancellation_reason"] = reason
			}
			// A cancel inside the paid period keeps the window this invoice
			// paid for until the period ends, unless its charge gave money back.
			// A refund or dispute also ends it through its own webhook.
			if paidWindow && stripeCancelLeavesPaidPeriod(fullInvoice, paidPeriodEnd, run.now) {
				details["paid_period_end"] = paidPeriodEnd.UTC().Format(time.RFC3339)
				// with no charge named, what came back can't be read: end, as before
				if chargeId := stripeIdFromRaw(fullInvoice.Charge); chargeId != "" {
					if !run.spend(store) {
						return false, nil
					}
					charge, err := stripeReconcileFetchCharge(ctx, chargeId)
					if err != nil {
						// as for an invoice that can't be read: nothing ends this run
						run.record(
							store,
							model.PaymentReconcileActionError,
							&renewal.NetworkId,
							renewal.TransactionId,
							map[string]any{"error": err.Error(), "leg": "charge", "charge": chargeId},
						)
						continue
					}
					if charge.keptWholePayment() {
						run.countCanceledPaidThrough(store)
						// the paid window is confirmed, so its Pro metadata is
						// repaired as an active subscription's is
						run.repairEntitlement(store, renewal, fullInvoice.Subscription.Id, details)
						continue
					}
					details["charge"] = charge.Id
					details["charge_amount_refunded"] = charge.AmountRefunded
					details["charge_disputed"] = charge.Disputed
				}
			}
			// only the window this invoice paid for; the subscription's other
			// open invoices are checked on their own
			run.end(
				store,
				renewal.NetworkId,
				paymentReconcileEndScope{transactionIds: []string{renewal.TransactionId}},
				fullInvoice.Subscription.Id,
				details,
			)
		} else if stripeSubscriptionEntitled(fullInvoice.Subscription.Status) {
			if paidWindow {
				run.repairEntitlement(
					store,
					renewal,
					fullInvoice.Subscription.Id,
					map[string]any{"subscription_status": fullInvoice.Subscription.Status},
				)
			}
		} else if paidWindow && !tookPayment &&
			!run.now.Before(paidPeriodEnd.Add(stripeGraceSettingsFunc().periodGrace(false))) {
			// Stripe still retries the first charge after a period no payment
			// paid for (past_due): the window gets the free period's grace, as
			// one credited now does. A window credited before that carried the
			// 30-day manual-payment grace (subscription_stripe_grace.go).
			run.end(
				store,
				renewal.NetworkId,
				paymentReconcileEndScope{transactionIds: []string{renewal.TransactionId}},
				fullInvoice.Subscription.Id,
				map[string]any{
					"subscription_status": fullInvoice.Subscription.Status,
					"took_payment":        tookPayment,
					"paid_period_end":     paidPeriodEnd.UTC().Format(time.RFC3339),
				},
			)
		}
	}

	return true, nil
}

// The end of the paid period the invoice bills, when the renewal is exactly the
// window the invoice's credit wrote on this network: the invoice's own ledger
// network, the subscription's network metadata, and a subscription line whose
// period starts the window and either ends the period the credit recorded
// (paid_end_time), or ends the window with a grace a credit wrote. paidWindow
// false otherwise. Reuse the caller's PostgreSQL session; nil selects the outer
// acquisition boundary.
func stripeRenewalPaidPeriodEndInConn(connOwner server.PgConn,
	ctx context.Context,
	renewal *model.ReconcileSubscriptionRenewal,
	invoice *stripeReconcileInvoiceExpanded,
) (paidPeriodEnd time.Time, paidWindow bool) {
	if invoice == nil || invoice.Subscription == nil || invoice.Id != renewal.TransactionId {
		return time.Time{}, false
	}
	ledgerNetworkId, credited := model.GetStripeInvoiceNetworkIdInConn(connOwner, ctx, renewal.TransactionId)
	if !credited || ledgerNetworkId != renewal.NetworkId {
		return time.Time{}, false
	}
	metadataNetwork := invoice.Subscription.Metadata["network_id"]
	if metadataNetwork != "" {
		metadataNetworkId, err := server.ParseId(metadataNetwork)
		if err != nil || metadataNetworkId != renewal.NetworkId {
			return time.Time{}, false
		}
	} else {
		// Legacy subscriptions predate immutable provider metadata. The exact
		// stripe_invoice ledger remains the committed credit owner.
	}
	if invoice.Lines == nil {
		return time.Time{}, false
	}
	for _, line := range invoice.Lines.Data {
		if line == nil || line.Type != "subscription" || line.Period == nil {
			continue
		}
		if line.Subscription != nil && *line.Subscription != invoice.Subscription.Id {
			continue
		}
		periodEnd := time.Unix(line.Period.End, 0)
		if !renewal.StartTime.Equal(time.Unix(line.Period.Start, 0)) {
			continue
		}
		// The recorded period matches whatever grace the window runs
		// (stripeGraceSettings). The end alone matches a window with the 30-day
		// manual-payment grace, or with the 24-hour window of the oldest paid
		// invoices and of a free period, as before paid_end_time was recorded.
		if (renewal.PaidEndTime != nil && renewal.PaidEndTime.Equal(periodEnd)) ||
			renewal.EndTime.Equal(periodEnd.Add(manualPaymentGracePeriod)) ||
			renewal.EndTime.Equal(periodEnd.Add(SubscriptionGracePeriod)) {
			return periodEnd, true
		}
	}
	return time.Time{}, false
}

// ----- apple -----
//
// decision table (App Store subscription status -> action):
//
//	1 active / 4 grace, latest transaction not in the ledger -> credit (through
//	    the apple_subscription_transaction gate)
//	4 grace                                                  -> also hold Pro to
//	    the renewal info's gracePeriodExpiresDate, with no revenue and no bytes
//	    (appleHoldGraceEntitlementInTx); 2 or 3 after it ends the hold
//	2 expired / 3 billing retry / 5 revoked, renewal active  -> end
//	    that renewal's paid window only (with its pre-ledger twin row); the
//	    network's other App Store subscriptions keep theirs
//	1 active with auto-renew off (cancel at period end)      -> nothing (status
//	    stays 1 until expiry; the rule falls out of the status itself)
//	production 4040010, the sandbox knows the transaction    -> nothing (a
//	    TestFlight or App Review purchase; counted as sandbox, not an error)
//	4040010 from every accepted environment                  -> status error,
//	    retried; once it persists through appleUnknownToStoreRetryRuns runs ->
//	    unknown_to_store, recorded once and counted on the heartbeat; not an
//	    error, and nothing is credited or ended from it
//
// Subscriptions whose last credited window ended up to
// paymentReconcileLapsedWindow ago are checked too; a lapsed row is only ever
// credited, never ended. A transaction the pre-ledger handler already
// credited is never credited again (appleCreditSubscriptionTransactionInTx).
//
// A deployment whose apple.yml app_store_notifications.environments lists
// Sandbox credits TestFlight and App Review purchases from their
// notifications, so its renewals can name sandbox transactions. Production
// answers 4040010 for those. The sandbox answer only establishes the
// environment: a sandbox purchase moves no money, so the reconciler credits,
// ends and repairs nothing from sandbox state. Only an answer the sandbox
// gives counts; a sandbox failure remains a status error, which keeps the
// watermark in place, and so does a sandbox miss until the bounded retry for
// a transaction no accepted environment knows is spent.
//
// The listing ignores since: every run examines each network's latest
// renewal in the lapsed window, so the watermark only records a complete,
// error-free pass.
func reconcileApple(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketApple

	creds := appleReconcileCredentialsFunc()
	if creds == nil {
		return false, errors.New("apple credentials disappeared mid-run")
	}

	// lapsed subscriptions too: a renewal the store charged but nothing
	// credited stays visible after the last credited window ends
	renewals := model.GetReconcileSubscriptionRenewalsInConn(run.connOwner,
		ctx,
		store,
		run.now.Add(-paymentReconcileLapsedWindow),
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

		statuses, err := appleGetSubscriptionStatuses(ctx, appleAppStoreServerApiBaseUrl, renewal.TransactionId)
		// every environment the deployment accepts answered
		// TransactionIdNotFoundError
		unknownToStore := false
		if appleTransactionIdNotFound(err) && creds.allowsEnvironment(appleEnvironmentSandbox) {
			// the sandbox lookup is a second store API call
			if !run.spend(store) {
				return false, nil
			}
			sandboxStatuses, sandboxErr := appleGetSubscriptionStatuses(ctx, appleAppStoreServerApiSandboxBaseUrl, renewal.TransactionId)
			if sandboxErr == nil {
				sandboxErr = validateAppleSubscriptionStatuses(sandboxStatuses)
			}
			if sandboxErr == nil {
				run.countSandbox(store)
				continue
			}
			unknownToStore = appleTransactionIdNotFound(sandboxErr)
			err = fmt.Errorf("App Store sandbox: %w", sandboxErr)
		} else if appleTransactionIdNotFound(err) {
			// without Sandbox notifications no sandbox transaction was
			// credited, so production is the only environment to ask
			unknownToStore = true
		} else if err == nil {
			err = validateAppleSubscriptionStatuses(statuses)
		}
		if err != nil {
			details := map[string]any{"error": err.Error(), "leg": "status"}
			if unknownToStore {
				if run.appleUnknownToStore(creds, renewal) {
					continue
				}
				details["reason"] = appleUnknownToStoreReason
			}
			run.record(
				store,
				model.PaymentReconcileActionError,
				&renewal.NetworkId,
				renewal.TransactionId,
				details,
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
			transaction, err := validateAppleTransactionBoundInConn(run.connOwner,
				ctx,
				AppleNotificationDecodedPayload{
					SignedDate:      run.now.UnixMilli(),
					TransactionInfo: claims,
				},
				creds.ProductIds,
				true,
			)
			if err != nil {
				entitledTransactionId, _ := appleControllerStringClaim(claims, "transactionId")
				if errors.Is(err, errAppleAccountTokenInvalid) && entitledTransactionId != "" {
					// entitled and paid, but nothing names its network (no
					// account token and no offer-code binding): never credit
					// the renewal row's network on a guess
					run.recordCreditUnfulfillableOnce(
						store,
						&renewal.NetworkId,
						entitledTransactionId,
						map[string]any{"reason": "destination_unresolved", "status": entitledTransaction.Status},
					)
					continue
				}
				run.record(
					store,
					model.PaymentReconcileActionError,
					&renewal.NetworkId,
					renewal.TransactionId,
					map[string]any{"error": err.Error(), "leg": "validate"},
				)
				continue
			}
			transactionId := transaction.transactionId
			ledgerNetworkId, transactionCredited := model.GetAppleTransactionNetworkIdInConn(run.connOwner, ctx, transactionId)
			if transactionCredited {
				if ledgerNetworkId == renewal.NetworkId && appleRenewalMatchesTransaction(renewal, transaction) {
					run.repairEntitlement(
						store,
						renewal,
						transactionId,
						map[string]any{"status": entitledTransaction.Status},
					)
				}
				if ledgerNetworkId == renewal.NetworkId && transaction.networkId == renewal.NetworkId {
					run.appleHoldGrace(store, transaction, entitledTransaction)
				}
				continue
			}
			credited, networkId, err := appleReconcileCreditTransactionInConn(run.connOwner, ctx, claims, creds.ProductIds, run.dryRun)
			if errors.Is(err, errAppleAccountNetworkMissing) {
				// the paid transaction belongs to a deleted network: never move
				// it to the renewal row's network on a guess
				run.recordCreditUnfulfillableOnce(
					store,
					&networkId,
					transactionId,
					map[string]any{"reason": "destination_deleted", "status": entitledTransaction.Status},
				)
				continue
			}
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
			} else if appleRenewalMatchesTransaction(renewal, transaction) {
				// A concurrent notification may have inserted the transaction
				// ledger after the fast check. Its normal credit creates Pro; if it
				// did not, the same exact-window repair remains idempotent.
				run.repairEntitlement(
					store,
					renewal,
					transactionId,
					map[string]any{"status": entitledTransaction.Status},
				)
			}
			continue
		}

		// end direction: the store says the entitlement is already over --
		// for the paid window this renewal's transaction belongs to, never the
		// network's other App Store subscriptions
		if renewal.EndTime.After(run.now) {
			run.end(
				store,
				renewal.NetworkId,
				paymentReconcileEndScope{transactionIds: appleWindowTransactionIds(renewals, renewal)},
				renewal.TransactionId,
				map[string]any{"status": lastStatus},
			)
		}
	}

	return true, nil
}

// Holds Pro through the billing grace period the store reports for the
// transaction (status 4), for a grace its notification missed
// (appleHoldGraceEntitlementInTx). A dry run holds nothing, and renewal info
// without a grace date holds nothing.
func (self *paymentReconcileRun) appleHoldGrace(
	store string,
	transaction *validatedAppleTransaction,
	lastTransaction *appleLastTransaction,
) {
	if self.dryRun || lastTransaction.Status != appleStatusBillingGrace {
		return
	}
	renewalClaims, err := appleDecodeJwsPayload(lastTransaction.SignedRenewalInfo)
	if err != nil {
		glog.Infof("[sub]reconcile: apple grace for transaction %s has no readable renewal info: %v\n", transaction.transactionId, err)
		return
	}
	graceExpires, ok := appleGracePeriodExpires(renewalClaims)
	if !ok {
		return
	}
	ctx := self.clientSession.Ctx
	held := false
	var holdErr error
	server.HandleError(func() {
		server.TxInConn(ctx, self.connOwner, func(tx server.PgTx) {
			held, holdErr = appleHoldGraceEntitlementInTx(tx, ctx, transaction, graceExpires, self.now)
		}, server.TxReadCommitted)
	}, func(err error) {
		held = false
		holdErr = err
	})
	if holdErr != nil {
		self.record(
			store,
			model.PaymentReconcileActionError,
			&transaction.networkId,
			transaction.transactionId,
			map[string]any{"error": holdErr.Error(), "leg": "grace"},
		)
		return
	}
	if held {
		model.UpdateProNetworkInConn(self.connOwner, ctx, transaction.networkId)
		self.record(
			store,
			model.PaymentReconcileActionEntitlementRepaired,
			&transaction.networkId,
			transaction.transactionId,
			map[string]any{
				"status":               lastTransaction.Status,
				"grace_period_expires": graceExpires.Format(time.RFC3339),
			},
		)
	}
}

// appleUnknownToStore applies the bounded retry to a renewal whose transaction
// no App Store environment the deployment accepts knows. Within the bound it
// returns false, and the caller records a status error carrying
// appleUnknownToStoreReason: the watermark stays and the run spends one
// retry. On the run that spends the last retry, and on every run once the
// disposition is recorded, it records unknown_to_store once, counts the
// renewal on the heartbeat and returns true: the renewal is no longer a store
// error. Nothing is credited or ended from the store's answer, and the next
// run asks the store again, so an answer the store gives later is reconciled
// as usual.
func (self *paymentReconcileRun) appleUnknownToStore(
	creds *appleServerApiCredentials,
	renewal *model.ReconcileSubscriptionRenewal,
) bool {
	ctx := self.clientSession.Ctx
	store := model.SubscriptionMarketApple
	recorded, errorRuns := model.GetPaymentReconcileUnknownToStoreInConn(self.connOwner,
		ctx,
		store,
		renewal.TransactionId,
		appleUnknownToStoreReason,
		self.now.Add(-appleUnknownToStoreRetryLookback),
	)
	// this run's answer is one more attempt
	if !recorded && errorRuns+1 < appleUnknownToStoreRetryRuns {
		return false
	}
	self.countUnknownToStore(store)
	if !recorded {
		environments := []string{appleEnvironmentProduction}
		if creds.allowsEnvironment(appleEnvironmentSandbox) {
			environments = append(environments, appleEnvironmentSandbox)
		}
		// a renewal credited before the transaction ledger names the
		// appTransactionId its notification carried; a ledger renewal names
		// a transaction the verified claims named
		_, ledger := model.GetAppleTransactionNetworkIdInConn(self.connOwner, ctx, renewal.TransactionId)
		self.recordOnce(
			store,
			model.PaymentReconcileActionUnknownToStore,
			&renewal.NetworkId,
			renewal.TransactionId,
			map[string]any{
				"leg":          "status",
				"reason":       appleUnknownToStoreReason,
				"environments": environments,
				"ledger":       ledger,
			},
		)
	}
	return true
}

// appleWindowTransactionIds names the renewals that hold the renewal's paid
// window on its network: the renewal itself and, for a window the pre-ledger
// notification handler also credited, that handler's row keyed on the app
// transaction id, whose end lies within a second of the ledger row's
// (HasPreLedgerAppleRenewalInTx). Ending one without the other would leave
// the window's second renewal and Pro balance open.
func appleWindowTransactionIds(
	renewals []*model.ReconcileSubscriptionRenewal,
	renewal *model.ReconcileSubscriptionRenewal,
) []string {
	transactionIds := []string{renewal.TransactionId}
	for _, other := range renewals {
		if other.NetworkId != renewal.NetworkId ||
			other.TransactionId == "" ||
			slices.Contains(transactionIds, other.TransactionId) {
			continue
		}
		if delta := other.EndTime.Sub(renewal.EndTime); -time.Second < delta && delta < time.Second {
			transactionIds = append(transactionIds, other.TransactionId)
		}
	}
	return transactionIds
}

func appleRenewalMatchesTransaction(
	renewal *model.ReconcileSubscriptionRenewal,
	transaction *validatedAppleTransaction,
) bool {
	return renewal.NetworkId == transaction.networkId &&
		renewal.TransactionId == transaction.transactionId &&
		renewal.StartTime.Equal(transaction.purchaseTime) &&
		renewal.EndTime.Equal(transaction.expiresTime.Add(SubscriptionGracePeriod))
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
	return appleReconcileCreditTransactionInConn(nil, ctx, transactionClaims, allowedProductIds, dryRun)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func appleReconcileCreditTransactionInConn(connOwner server.PgConn,
	ctx context.Context,
	transactionClaims map[string]any,
	allowedProductIds []string,
	dryRun bool,
) (credited bool, networkId server.Id, returnErr error) {
	notification := AppleNotificationDecodedPayload{
		SignedDate:      server.NowUtc().UnixMilli(),
		TransactionInfo: transactionClaims,
	}
	transaction, err := validateAppleTransactionBoundInConn(connOwner, ctx, notification, allowedProductIds, true)
	if err != nil {
		return false, server.Id{}, err
	}
	networkId = transaction.networkId

	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		if !appleNetworkExistsInTx(tx, ctx, transaction.networkId) {
			returnErr = errAppleAccountNetworkMissing
			return
		}
		if dryRun {
			// the credit gate's own read: a window the pre-ledger handler
			// already credited would not be credited again
			credited = !appleTransactionCreditedBeforeLedgerInTx(tx, ctx, transaction)
			return
		}
		credited = appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), transaction)
	}, server.TxReadCommitted)
	if returnErr != nil {
		return false, networkId, returnErr
	}

	if credited && !dryRun {
		model.UpdateProNetworkInConn(connOwner, ctx, networkId)
	}
	return credited, networkId, nil
}

// ----- google -----
//
// decision table (Play subscriptionsv2 state -> action):
//
//	ACTIVE, or CANCELED with expiry in the future (cancel at period end,
//	    paid through) -> run the existing PlaySubscriptionRenewal credit path
//	    (advisory lock + in-tx overlap re-check); Renewed = a missed renewal
//	    repaired. Never ended.
//	EXPIRED / PENDING_PURCHASE_CANCELED / 410 Gone, renewal active -> end
//	CANCELED with expiry passed, renewal active                    -> end
//	GRACE, no hold yet -> run the same path, which holds Pro through the grace
//	    period (playHoldGraceEntitlementInTx): no revenue, no bytes
//	PAUSED / ON_HOLD / PENDING                                     -> nothing
//
// Purchases whose last credited window ended up to
// paymentReconcileLapsedWindow ago are checked too; a lapsed row is only ever
// credited, never ended.
func reconcilePlay(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketGoogle
	packageName := playPackageNameFunc()

	// lapsed purchases too: an account-hold recovery or a renewal whose RTDN
	// and scheduled poll both missed stays visible after the window ends
	renewals := model.GetReconcileSubscriptionRenewalsInConn(run.connOwner,
		ctx,
		store,
		run.now.Add(-paymentReconcileLapsedWindow),
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
					run.end(store, renewal.NetworkId, paymentReconcileEndScope{purchaseToken: purchaseToken}, purchaseToken, map[string]any{"subscription_state": "GONE"})
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
				run.end(store, renewal.NetworkId, paymentReconcileEndScope{purchaseToken: purchaseToken}, purchaseToken, map[string]any{"subscription_state": sub.SubscriptionState})
			}
			continue
		}

		// a grace period whose notification and poll both missed it. A purchase
		// without an account link that is bound elsewhere is left to its owner.
		if sub.SubscriptionState == "SUBSCRIPTION_STATE_IN_GRACE_PERIOD" && !run.dryRun &&
			!playUnlinkedBoundElsewhereInConn(run.connOwner, run.clientSession, sub, renewal) {
			if !run.spend(store) {
				return false, nil
			}
			result, err := PlaySubscriptionRenewalInConn(run.connOwner,
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
					map[string]any{"error": err.Error(), "leg": "grace"},
				)
			} else if result.GraceHeld {
				run.record(
					store,
					model.PaymentReconcileActionEntitlementRepaired,
					&renewal.NetworkId,
					purchaseToken,
					map[string]any{
						"subscription_state": sub.SubscriptionState,
						"expiry_time":        result.ExpiryTime.UTC().Format(time.RFC3339),
					},
				)
			}
			continue
		}

		// not over: a CANCELED purchase here is paid through maxExpiryTime and
		// its period is entitled exactly like an active one
		if sub.SubscriptionState == "SUBSCRIPTION_STATE_ACTIVE" || sub.SubscriptionState == "SUBSCRIPTION_STATE_CANCELED" {
			if playUnlinkedBoundElsewhereInConn(run.connOwner, run.clientSession, sub, renewal) {
				// a purchase without an account link is bound to a different
				// network than this renewal row: never credit the row's
				// network, and never pick one -- record it once for the owner
				run.recordCreditUnfulfillableOnce(
					store,
					&renewal.NetworkId,
					purchaseToken,
					map[string]any{
						"reason":             "bound_to_another_network",
						"subscription_state": sub.SubscriptionState,
						"expiry_time":        maxExpiryTime.UTC().Format(time.RFC3339),
					},
				)
				continue
			}
			repairMatches, matchErr := playRenewalMatchesSubscriptionInConn(run.connOwner,
				run.clientSession,
				renewal,
				sub,
				maxExpiryTime,
			)
			if matchErr != nil {
				run.record(
					store,
					model.PaymentReconcileActionError,
					&renewal.NetworkId,
					purchaseToken,
					map[string]any{"error": matchErr.Error(), "leg": "validate"},
				)
				continue
			}
			if run.dryRun {
				// the renewal path's overlap gate, read-only: an existing
				// balance overlapping this expiry means the real run would
				// no-op, otherwise it would credit the renewal
				if _, err := model.GetOverlappingTransferBalanceInConn(run.connOwner, ctx, purchaseToken, maxExpiryTime); err != nil {
					run.record(
						store,
						model.PaymentReconcileActionWouldCredit,
						&renewal.NetworkId,
						purchaseToken,
						map[string]any{"expiry_time": maxExpiryTime.UTC().Format(time.RFC3339)},
					)
				} else if repairMatches {
					run.repairEntitlement(
						store,
						renewal,
						purchaseToken,
						map[string]any{"subscription_state": sub.SubscriptionState},
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
			result, err := PlaySubscriptionRenewalInConn(run.connOwner,
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
			} else if repairMatches {
				run.repairEntitlement(
					store,
					renewal,
					purchaseToken,
					map[string]any{"subscription_state": sub.SubscriptionState},
				)
			}
		}
	}

	return true, nil
}

// playUnlinkedBoundElsewhere: the purchase carries no account identifiers
// and its binding names a network other than the renewal row's. A purchase
// that was never bound keeps the row's network (rows credited before the
// binding existed).
func playUnlinkedBoundElsewhere(
	clientSession *session.ClientSession,
	subscription *PlaySubscription,
	renewal *model.ReconcileSubscriptionRenewal,
) bool {
	return playUnlinkedBoundElsewhereInConn(nil, clientSession, subscription, renewal)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func playUnlinkedBoundElsewhereInConn(connOwner server.PgConn,
	clientSession *session.ClientSession,
	subscription *PlaySubscription,
	renewal *model.ReconcileSubscriptionRenewal,
) bool {
	linkedNetworkId, validLink := playLinkedNetworkIdInConn(connOwner, clientSession, subscription)
	if !validLink || linkedNetworkId != nil {
		return false
	}
	networkId, bound := playPurchaseBindingLookupFunc(connOwner, clientSession.Ctx, renewal.PurchaseToken, subscription.LinkedPurchaseToken, false)
	return bound && networkId != renewal.NetworkId
}

func playRenewalMatchesSubscription(
	clientSession *session.ClientSession,
	renewal *model.ReconcileSubscriptionRenewal,
	subscription *PlaySubscription,
	maxExpiryTime time.Time,
) (bool, error) {
	return playRenewalMatchesSubscriptionInConn(nil, clientSession, renewal, subscription, maxExpiryTime)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func playRenewalMatchesSubscriptionInConn(connOwner server.PgConn,
	clientSession *session.ClientSession,
	renewal *model.ReconcileSubscriptionRenewal,
	subscription *PlaySubscription,
	maxExpiryTime time.Time,
) (bool, error) {
	startTime, err := subscription.ParseStartTime()
	if err != nil {
		return false, err
	}
	if len(subscription.LineItems) == 0 {
		return false, nil
	}
	sku, ok := playSkusFunc()[subscription.LineItems[0].ProductId]
	if !ok || !sku.Supporter {
		return false, nil
	}
	// the account link, or for a purchase without one the binding the verify
	// endpoint made (read-only: a reconcile pass never writes a binding)
	linkedNetworkId, validLink := playResolveNetworkIdInConn(connOwner, clientSession, subscription, renewal.PurchaseToken, false)
	if !validLink || linkedNetworkId == nil {
		return false, nil
	}
	return *linkedNetworkId == renewal.NetworkId &&
		renewal.StartTime.Equal(startTime) &&
		renewal.EndTime.Equal(maxExpiryTime.Add(SubscriptionGracePeriod)), nil
}

// ----- solana -----
//
// decision table:
//
//	recorded no_intent payment whose reference NOW resolves to an open intent,
//	    amount >= quote - tolerance -> credit (through the intent one-shot)
//	recorded no_intent payment already credited elsewhere -> clear the record
//	active renewal whose credited tx signature is NOT on-chain (per Helius,
//	    full-history lookup, credited > 1h and < unlink_after ago) -> end
//	underpaid records -> left for the operator (still underpaid)
//	USDC transfer into a receiver, older than MinAge, at least DustUsd,
//	    that no record names -> the webhook's decision: credit through an
//	    intent's one-shot when one matches, else recorded once (no_intent)
func reconcileSolana(run *paymentReconcileRun, since time.Time) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketSolana

	// leg 1: unfulfilled payments -- money that arrived and bought nothing.
	// The DB sweep costs no store API budget.
	unfulfilledPayments := model.ListUnfulfilledSolanaPaymentsInConn(run.connOwner,
		ctx,
		model.SolanaUnfulfilledReasonNoIntent,
		paymentReconcileRenewalLimit,
	)
	for _, payment := range unfulfilledPayments {
		run.examine(store)
		if model.IsSolanaPaymentCompletedInConn(run.connOwner, ctx, payment.TxSignature) {
			// a redelivery already credited this exact payment -- the record
			// is stale. A dry run leaves the record for the real run to clear.
			if !run.dryRun {
				model.RemoveUnfulfilledSolanaPaymentInConn(run.connOwner, ctx, payment.TxSignature)
			}
			continue
		}
		if len(payment.ReferenceCandidates) == 0 {
			continue
		}
		searchResult, err := model.SearchPaymentIntentsInConn(run.connOwner, payment.ReferenceCandidates, run.clientSession)
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
		credited, err := solanaCreditPaymentIntentInConn(run.connOwner,
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
			model.RemoveUnfulfilledSolanaPaymentInConn(run.connOwner, ctx, payment.TxSignature)
		} else if model.IsSolanaPaymentCompletedInConn(run.connOwner, ctx, payment.TxSignature) {
			// lost the race to a concurrent webhook redelivery of this same
			// payment -- credited either way, the record is resolved
			model.RemoveUnfulfilledSolanaPaymentInConn(run.connOwner, ctx, payment.TxSignature)
		}
	}

	// leg 2: verify credited payments still exist on-chain. A signature that
	// Helius' full-history lookup cannot find means the credited transaction
	// never landed (dropped or rolled back) -- entitlement without payment.
	//
	// Only payments credited within the retention window are verified: once
	// unlink_after has passed since the credit, the renewal no longer names
	// its intent, nor the intent its network (RunCryptoPaymentUnlink), and a
	// transaction that was on-chain for a month does not leave it.
	linkedSince := run.now.Add(-cryptoPayConfigFunc().UnlinkAfter)
	renewals := model.GetReconcileSubscriptionRenewalsInConn(run.connOwner,
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
		// past the retention window: unlinked, or about to be
		if renewal.StartTime.Before(linkedSince) {
			continue
		}
		if renewal.TransactionId == "" {
			continue
		}
		intentNetworkId, signature, ok := model.GetSolanaPaymentIntentCompletionInConn(run.connOwner, ctx, renewal.TransactionId)
		if !ok || intentNetworkId != renewal.NetworkId {
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
			target := targets[start+i]
			if status != nil {
				// The exact credited payment is present in authoritative chain
				// history. Only a successful confirmed/finalized execution is a
				// positive payment verdict from which to reconstruct metadata.
				if solanaStatusConfirmsPayment(status.Err, status.ConfirmationStatus) {
					run.repairEntitlement(
						store,
						target.renewal,
						target.signature,
						map[string]any{"confirmation_status": status.ConfirmationStatus},
					)
				}
				continue
			}
			// only the purchase this vanished transaction paid for
			run.end(
				store,
				target.renewal.NetworkId,
				paymentReconcileEndScope{transactionIds: []string{target.renewal.TransactionId}},
				target.signature,
				map[string]any{
					"reason":    "tx_signature_not_found_on_chain",
					"reference": target.renewal.TransactionId,
				},
			)
		}
	}

	// the receiver leg: every USDC transfer into our receivers, once, from
	// the chain (solana_receiver_sweep.go). A payment no intent names any
	// more (its delivery lost after the intent was swept) is recorded for an
	// operator, never credited off the record. It keeps its own cursors and
	// call cap, so it runs before leg 3 can spend the budget.
	reconcileSolanaReceivers(run)

	// leg 3: the chain itself, for every open USDC intent's reference: a
	// payment whose Helius delivery was lost is found and credited through the
	// webhook's own decision (mmm/BTCPAY.md §17.3, closing the gap of §2.5)
	if complete, err := reconcileSolanaChainReferences(run); err != nil || !complete {
		return complete, err
	}

	// leg 4: native SOL invoices, on the rail's own legs (credited from the
	// chain by the scanner; this re-judges held receipts and re-verifies
	// credits) under the solana store
	return reconcileCryptoAsset(run, model.CryptoAssetSol)
}

func solanaStatusConfirmsPayment(statusErr any, confirmationStatus *string) bool {
	if statusErr != nil || confirmationStatus == nil {
		return false
	}
	return *confirmationStatus == "confirmed" || *confirmationStatus == "finalized"
}

// ----- x402 -----
//
// decision table (our settled_not_granted events -> action):
//
//	unresolved, settle tx already granted       -> already_credited (resolves)
//	unresolved, not granted                     -> grant (idempotent on the settle tx) -> credited
//	unresolved, network deleted                 -> credit_unfulfillable (resolves)
//	unresolved, grant fails otherwise           -> error (retried next run)
//
// There is no store to pull from: the facilitator already settled, so the
// truth is our own record of the settle transaction (S9).

var (
	x402ListUnresolvedSettledNotGranted = func(connOwner server.PgConn, ctx context.Context, limit int) []*model.PaymentReconciliationEvent {
		return model.GetUnresolvedSettledNotGrantedEventsInConn(connOwner, ctx, model.SubscriptionMarketX402, limit)
	}
	x402ReconcileTransactionGranted = x402TransactionGrantedInConn
)

func reconcileX402(run *paymentReconcileRun, since time.Time) (bool, error) {
	store := model.SubscriptionMarketX402
	ctx := run.clientSession.Ctx

	events := x402ListUnresolvedSettledNotGranted(run.connOwner, ctx, paymentReconcileRenewalLimit)
	for _, event := range events {
		if !run.spend(store) {
			return false, nil
		}
		run.examine(store)

		transaction := event.Evidence
		if event.NetworkId == nil {
			run.recordCreditUnfulfillable(store, transaction, "destination_unresolved")
			continue
		}
		networkId := *event.NetworkId

		sku, network, err := x402SkuFromSettledNotGrantedDetails(event.Details)
		if err != nil {
			run.record(store, model.PaymentReconcileActionError, &networkId, transaction, map[string]any{
				"error": err.Error(),
				"leg":   "credit",
			})
			continue
		}
		details := map[string]any{
			"sku_id":  sku.SkuId,
			"network": network,
			"leg":     "credit",
		}

		if x402ReconcileTransactionGranted(run.connOwner, ctx, networkId, transaction) {
			run.record(store, model.PaymentReconcileActionAlreadyCredited, &networkId, transaction, details)
			continue
		}
		if run.dryRun {
			run.record(store, model.PaymentReconcileActionWouldCredit, &networkId, transaction, details)
			continue
		}

		err = func() error {
			settleResponse := &X402SettleResponse{
				Success:     true,
				Transaction: transaction,
				Network:     network,
			}
			netRevenue := model.UsdToNanoCents(sku.PriceUsd)
			// the purchase's email is not kept with the event: no receipt
			if sku.Pro {
				return x402GrantProMonthFunc(run.connOwner, ctx, networkId, sku, netRevenue, settleResponse, nil)
			}
			return x402GrantDataFunc(run.connOwner, ctx, networkId, sku, netRevenue, settleResponse, nil)
		}()
		switch {
		case errors.Is(err, model.ErrPaymentNetworkNotFound):
			run.recordCreditUnfulfillable(store, transaction, "destination_deleted")
		case err != nil:
			run.record(store, model.PaymentReconcileActionError, &networkId, transaction, map[string]any{
				"error": err.Error(),
				"leg":   "credit",
			})
		default:
			run.record(store, model.PaymentReconcileActionCredited, &networkId, transaction, details)
		}
	}
	return true, nil
}

// ----- monero and zcash (crypto_pay_*.go) -----
//
// decision table (per coin; mmm/ZCASH-CONTRACT.md §3.9):
//
//	receipt in review as short, its invoice now covered (a top-up arrived)
//	    -> credit through the one credit (the scanner does this too; this is
//	       the safety net)
//	credited receipt at final depth (or credited long ago and never final)
//	    that the daemon no longer shows with the same output and amount,
//	    from a wallet that passed its identity check, whose scanner is
//	    current and within max_lag_blocks of a known network height, and
//	    that is the final depth past the receipt (mmm/ZCASH-CONTRACT.md §3.13.2)
//	    -> orphaned; when its invoice is no longer covered by its other
//	       receipts, end what the credit granted, take the credit back and
//	       hold the receipt for review (orphaned), so `bringyourctl crypto
//	       credit` can grant it again. More than
//	       cryptoReconcileMaxOrphansPerRun at once ends none: an error for
//	       an operator (a rebuilt or resyncing wallet looks like that).
//	       A coin whose credits stand over a reorg (Bitcoin,
//	       cryptoWalletKeepsCredits) ends none: an error for an operator
//	    otherwise -> skipped and retried next run (an error when the wallet
//	       cannot be trusted at all)
//	the daemon's incoming total over a day of final blocks differs from our
//	    receipts' -> error (ledger): an output the scanner missed, for example
//	    beyond the Monero lookahead
//
// The first leg needs no daemon. The others run when the coin's vault file
// is there; with none, credited receipts awaiting re-verification are an
// error (they cannot be checked), and nothing else is to be done.

// A credited receipt not stamped final this long after the scanner last saw
// it is re-checked anyway: a credited transaction that left the chain stops
// being reported, so it would never reach final depth.
const cryptoReconcileStaleCredit = 2 * time.Hour

// The most credited receipts one run takes as gone from the chain. Real
// reorgs past the final depth are near impossible; many receipts vanishing at
// once is a wallet that was rebuilt or lost its history, and ending paid
// years on its word would be the outage.
const cryptoReconcileMaxOrphansPerRun = 3

// Blocks in a day, per coin: the ledger leg's window.
func cryptoBlocksPerDay(assetConfig *CryptoPayAssetConfig, asset string, height int64) int64 {
	switch asset {
	case model.CryptoAssetXmr:
		// 2-minute blocks
		return 720
	case model.CryptoAssetZec:
		if 0 < assetConfig.Nu7ActivationHeight && assetConfig.Nu7ActivationHeight <= height {
			// 25-second blocks after NU7
			return 3456
		}
		// 75-second blocks
		return 1152
	}
	return 0
}

func reconcileMonero(run *paymentReconcileRun, since time.Time) (bool, error) {
	return reconcileCryptoAsset(run, model.CryptoAssetXmr)
}

func reconcileZcash(run *paymentReconcileRun, since time.Time) (bool, error) {
	return reconcileCryptoAsset(run, model.CryptoAssetZec)
}

func reconcileBittensor(run *paymentReconcileRun, since time.Time) (bool, error) {
	return reconcileCryptoAsset(run, model.CryptoAssetTao)
}

func reconcileBitcoin(run *paymentReconcileRun, since time.Time) (bool, error) {
	return reconcileCryptoAsset(run, model.CryptoAssetBtc)
}

func reconcileEthereum(run *paymentReconcileRun, since time.Time) (bool, error) {
	return reconcileCryptoAsset(run, model.CryptoAssetEth)
}

func reconcileCryptoAsset(run *paymentReconcileRun, asset string) (bool, error) {
	ctx := run.clientSession.Ctx
	store := cryptoAssetMarket(asset)
	config := cryptoPayConfigFunc()
	policy := cryptoIntentPolicyFor(config, asset)

	// leg 1: receipts held as short whose invoice is now covered
	review := model.ListCryptoPaymentReceiptsByStateInConn(
		run.connOwner,
		ctx,
		asset,
		[]string{model.CryptoReceiptStateReview},
		paymentReconcileRenewalLimit,
	)
	intentIds := []server.Id{}
	seenIntents := map[server.Id]bool{}
	for _, receipt := range review {
		if receipt.IntentId == nil || !cryptoReceiptCounts(receipt) || seenIntents[*receipt.IntentId] {
			continue
		}
		seenIntents[*receipt.IntentId] = true
		intentIds = append(intentIds, *receipt.IntentId)
	}
	for _, intentId := range intentIds {
		run.examine(store)
		intent := model.GetCryptoPaymentIntentInConn(run.connOwner, ctx, intentId)
		if intent == nil || intent.CreditedAt != nil || intent.VoidedAt != nil {
			continue
		}
		receipts := model.GetCryptoPaymentReceiptsForIntentInConn(run.connOwner, ctx, intentId)
		view := cryptoEvaluateIntent(intent, receipts, policy, run.now)
		if view.Action != cryptoIntentActionCredit {
			continue
		}
		details := map[string]any{"intent_id": intentId.String(), "leg": "credit", "reason": "review_cleared"}
		if run.dryRun {
			run.record(store, model.PaymentReconcileActionWouldCredit, intent.NetworkId, intentId.String(), details)
			continue
		}
		credited, err := cryptoCreditIntentInConn(run.connOwner, run.clientSession, intent, &cryptoCredit{
			usdNanoCents: view.CreditUsd,
			receipts:     view.CreditReceipts,
			receiptUsd:   view.ReceiptUsdValue,
			late:         view.Late,
		})
		if err != nil {
			run.record(store, model.PaymentReconcileActionError, intent.NetworkId, intentId.String(), map[string]any{
				"error": err.Error(),
				"leg":   "credit",
			})
			continue
		}
		if credited {
			run.record(store, model.PaymentReconcileActionCredited, intent.NetworkId, intentId.String(), details)
		}
	}

	toVerify := model.ListCryptoPaymentReceiptsToVerifyInConn(
		run.connOwner,
		ctx,
		asset,
		run.now.Add(-paymentReconcileWindow),
		run.now.Add(-cryptoReconcileStaleCredit),
		paymentReconcileRenewalLimit,
	)
	wallet, _, err := cryptoWalletForAsset(config, asset)
	if err != nil {
		if len(toVerify) == 0 {
			return true, nil
		}
		return false, fmt.Errorf("%d credited %s receipts await re-verification, but the wallet daemon is not configured", len(toVerify), asset)
	}

	// leg 2: credited receipts must still be on the chain, by the word of a
	// wallet that can be trusted to know. A reader of final transactions only
	// (TAO's finalized blocks, SOL's finalized commitment) credited nothing
	// that can leave the chain, so it is not asked again
	irreversible := false
	if final, ok := wallet.(cryptoWalletIrreversible); ok {
		irreversible = final.Irreversible()
	}
	if 0 < len(toVerify) && !irreversible {
		cursor := model.GetCryptoScanCursorInConn(run.connOwner, ctx, asset)
		refused := ""
		if err := cryptoCheckWallet(ctx, run.connOwner, asset, wallet); err != nil {
			refused = "the wallet check failed: " + err.Error()
		} else if !cryptoCursorFresh(cursor, config, run.now) {
			refused = "the scanner is not current"
		} else if cursor.WalletHeight == nil || cursor.NetworkHeight == nil ||
			!cryptoWalletCaughtUp(config, asset, *cursor.WalletHeight, *cursor.NetworkHeight) {
			refused = "the wallet has not caught up with a known network height"
		}
		if refused != "" {
			run.record(store, model.PaymentReconcileActionError, nil, "", map[string]any{
				"error":    fmt.Sprintf("%d credited receipts not re-verified: %s", len(toVerify), refused),
				"leg":      "verify",
				"receipts": len(toVerify),
			})
		} else if !cryptoReconcileVerify(run, config, asset, store, wallet, policy, toVerify, *cursor.WalletHeight) {
			return false, nil
		}
	}

	// leg 3: the daemon's incoming total over a day of final blocks equals our
	// receipts', whenever the daemon is configured (sold or not) and its
	// scanner current: a scanner that is behind has not recorded what the
	// daemon shows (cryptoReconcileLedger checks).
	if !run.spend(store) {
		return false, nil
	}
	if err := cryptoReconcileLedger(run, config, asset, store, wallet); err != nil {
		run.record(store, model.PaymentReconcileActionError, nil, "", map[string]any{
			"error": err.Error(),
			"leg":   "ledger",
		})
	}
	return true, nil
}

// cryptoReconcileVerify is leg 2 on a wallet already found trustworthy:
// every credited receipt's transaction must still show the same output and
// amount. A receipt the wallet does not show is gone only once the wallet is
// the final depth past it; more than cryptoReconcileMaxOrphansPerRun gone at
// once ends none. complete = false when the run's budget ran out.
func cryptoReconcileVerify(
	run *paymentReconcileRun,
	config *CryptoPayConfig,
	asset string,
	store string,
	wallet cryptoWallet,
	policy *cryptoIntentPolicy,
	toVerify []*model.CryptoPaymentReceipt,
	walletHeight int64,
) (complete bool) {
	ctx := run.clientSession.Ctx
	assetConfig := config.Asset(asset)
	byTx := map[string][]*model.CryptoPaymentReceipt{}
	txIds := []string{}
	for _, receipt := range toVerify {
		if _, ok := byTx[receipt.TxId]; !ok {
			txIds = append(txIds, receipt.TxId)
		}
		byTx[receipt.TxId] = append(byTx[receipt.TxId], receipt)
	}
	type vanished struct {
		receipt *model.CryptoPaymentReceipt
		reason  string
	}
	gone := []*vanished{}
	for _, txId := range txIds {
		if !run.spend(store) {
			return false
		}
		walletReceipts, found, err := wallet.Transaction(ctx, txId)
		if err != nil {
			run.record(store, model.PaymentReconcileActionError, nil, txId, map[string]any{
				"error": err.Error(),
				"leg":   "verify",
			})
			continue
		}
		finalKeys := []model.CryptoPaymentReceiptKey{}
		for _, receipt := range byTx[txId] {
			run.examine(store)
			var present *cryptoWalletReceipt
			if found {
				for _, walletReceipt := range walletReceipts {
					if walletReceipt.OutputKey == receipt.OutputKey &&
						walletReceipt.AmountAtomic == uint64(receipt.AmountAtomic) &&
						0 < walletReceipt.BlockHeight {
						present = walletReceipt
					}
				}
			}
			if present != nil {
				if receipt.FinalAt == nil && assetConfig != nil &&
					assetConfig.FinalConfirmationsAt(int64(present.BlockHeight)) <= int64(present.Confirmations) {
					finalKeys = append(finalKeys, receipt.Key())
				}
				continue
			}
			if receipt.BlockHeight == nil || walletHeight < *receipt.BlockHeight+assetConfig.FinalConfirmationsAt(*receipt.BlockHeight) {
				// not yet the final depth behind the wallet: next run
				continue
			}
			reason := "tx_not_found"
			if found {
				reason = "output_changed"
			}
			gone = append(gone, &vanished{receipt: receipt, reason: reason})
		}
		if !run.dryRun {
			model.MarkCryptoPaymentReceiptsFinalInConn(run.connOwner, ctx, asset, finalKeys, run.now)
		}
	}
	if keeper, ok := wallet.(cryptoWalletKeepsCredits); ok && keeper.KeepsCredits() {
		// the coin's credits stand over a reorg (Bitcoin, mmm/BTCPAY.md
		// §16.7): each one the chain no longer shows is for an operator, who
		// checks the Coinbase account; nothing is ended
		for _, candidate := range gone {
			details := map[string]any{
				"error":  "a credited payment is no longer on the chain; its credit stands (credits are never taken back for this coin): check the Coinbase account",
				"leg":    "verify",
				"reason": candidate.reason,
			}
			if candidate.receipt.IntentId != nil {
				details["intent_id"] = candidate.receipt.IntentId.String()
			}
			run.record(store, model.PaymentReconcileActionError, nil, candidate.receipt.TxId, details)
			glog.Errorf("[reconcile]%s: a credited payment for invoice %v is no longer on the chain; its credit stands\n", store, candidate.receipt.IntentId)
		}
		return true
	}
	if cryptoReconcileMaxOrphansPerRun < len(gone) {
		run.record(store, model.PaymentReconcileActionError, nil, "", map[string]any{
			"error": fmt.Sprintf(
				"%d credited receipts are no longer shown by the wallet at once; nothing was ended. A rebuilt or resyncing wallet looks like this: check it, and end only what really left the chain",
				len(gone),
			),
			"leg":      "verify",
			"receipts": len(gone),
		})
		glog.Errorf("[reconcile]%s: %d credited receipts vanished at once; ended none\n", store, len(gone))
		return true
	}
	for _, candidate := range gone {
		cryptoReconcileOrphan(run, asset, store, candidate.receipt, policy, candidate.reason)
	}
	return true
}

// cryptoReconcileOrphan handles a credited receipt the daemon no longer
// shows. While the invoice's other credited receipts still cover the payment,
// the receipt is only marked orphaned; otherwise the credit's grant is ended
// at now and the receipt is held for review (orphaned).
func cryptoReconcileOrphan(
	run *paymentReconcileRun,
	asset string,
	store string,
	receipt *model.CryptoPaymentReceipt,
	policy *cryptoIntentPolicy,
	reason string,
) {
	ctx := run.clientSession.Ctx
	evidence := receipt.TxId
	if receipt.IntentId == nil {
		return
	}
	intent := model.GetCryptoPaymentIntentInConn(run.connOwner, ctx, *receipt.IntentId)
	if intent == nil || intent.CreditedAt == nil {
		return
	}
	details := map[string]any{
		"intent_id": intent.IntentId.String(),
		"leg":       "verify",
		"reason":    reason,
	}

	// what the other credited receipts still carry
	var remainingAtomic int64
	var remainingUsd model.NanoCents
	usdKnown := true
	for _, other := range model.GetCryptoPaymentReceiptsForIntentInConn(run.connOwner, ctx, intent.IntentId) {
		if other.Key() == receipt.Key() || other.State != model.CryptoReceiptStateCredited {
			continue
		}
		remainingAtomic += other.AmountAtomic
		if other.UsdValueNanoCents == nil {
			usdKnown = false
		} else {
			remainingUsd += *other.UsdValueNanoCents
		}
	}
	covered := cryptoAcceptThresholdAtomic(intent.AmountAtomic, policy.acceptPercent) <= remainingAtomic
	if usdKnown && intent.CreditedUsdNanoCents != nil && cryptoAcceptValueNanoCents(*intent.CreditedUsdNanoCents, policy.acceptPercent) <= remainingUsd {
		covered = true
	}

	if covered {
		if !run.dryRun {
			model.SetCryptoPaymentReceiptsStateInConn(run.connOwner, ctx, receipt.IntentId, []model.CryptoPaymentReceiptKey{receipt.Key()}, model.CryptoReceiptStateOrphaned, "")
		}
		return
	}
	if run.dryRun {
		run.record(store, model.PaymentReconcileActionWouldEnd, intent.NetworkId, evidence, details)
		return
	}

	market, transactionId := model.CryptoPaymentRenewalHandle(intent)
	plan := slices.Contains(cryptoPayKnownPlans, intent.Item)
	var endedNetworkIds []server.Id
	var endErr error
	server.HandleError(func() {
		server.TxInConn(ctx, run.connOwner, func(tx server.PgTx) {
			endedNetworkIds, _ = model.EndCryptoPaymentGrantInTx(tx, ctx, intent, market, transactionId, plan, run.now)
			model.SetCryptoPaymentReceiptsStateInTx(tx, ctx, receipt.IntentId, []model.CryptoPaymentReceiptKey{receipt.Key()}, model.CryptoReceiptStateReview, model.CryptoReviewReasonOrphaned)
			// the credit is taken back with what it granted: the invoice reads
			// review/orphaned, and `bringyourctl crypto credit` grants it again
			// if the payment turns out to be on the chain after all
			model.UncreditCryptoPaymentIntentInTx(tx, ctx, intent.IntentId)
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) {
		endErr = err
	})
	if endErr != nil {
		run.record(store, model.PaymentReconcileActionError, intent.NetworkId, evidence, map[string]any{
			"error":     endErr.Error(),
			"leg":       "end",
			"intent_id": intent.IntentId.String(),
		})
		return
	}
	for _, networkId := range endedNetworkIds {
		model.UpdateProNetworkInConn(run.connOwner, ctx, networkId)
	}
	run.record(store, model.PaymentReconcileActionEnded, intent.NetworkId, evidence, details)
	glog.Infof("[reconcile]%s: invoice %s paid by a transaction that left the chain; its credit was ended\n", store, intent.IntentId)
}

// cryptoReconcileLedger compares the daemon's incoming total over the last
// day of final blocks with our receipts'.
func cryptoReconcileLedger(
	run *paymentReconcileRun,
	config *CryptoPayConfig,
	asset string,
	store string,
	wallet cryptoWallet,
) error {
	ctx := run.clientSession.Ctx
	assetConfig := config.Asset(asset)
	cursor := model.GetCryptoScanCursorInConn(run.connOwner, ctx, asset)
	if cursor == nil || cursor.LastSuccessAt == nil || cursor.WalletHeight == nil ||
		3*config.ScanIntervalFor(asset) < run.now.Sub(*cursor.LastSuccessAt) {
		// the scanner is behind; the quote path already stops selling
		return nil
	}
	walletTop := *cursor.WalletHeight
	// blocks at least the final depth deep: h <= top - final
	toHeight := walletTop - assetConfig.FinalConfirmationsAt(walletTop) + 1
	fromHeight := max(0, toHeight-cryptoBlocksPerDay(assetConfig, asset, toHeight))
	if toHeight <= fromHeight {
		return nil
	}
	walletReceipts, _, err := wallet.IncomingSince(ctx, uint64(fromHeight))
	if err != nil {
		return err
	}
	var walletTotal int64
	walletCount := 0
	for _, walletReceipt := range walletReceipts {
		if fromHeight <= int64(walletReceipt.BlockHeight) && int64(walletReceipt.BlockHeight) < toHeight && 0 < walletReceipt.BlockHeight {
			walletTotal += int64(walletReceipt.AmountAtomic)
			walletCount += 1
		}
	}
	ourTotal, ourCount := model.SumCryptoPaymentReceiptsInConn(run.connOwner, ctx, asset, fromHeight, toHeight)
	run.storeDetail(store)["ledger"] = map[string]any{
		"from_height": fromHeight,
		"to_height":   toHeight,
		"receipts":    ourCount,
	}
	if walletTotal != ourTotal || walletCount != ourCount {
		return fmt.Errorf(
			"ledger mismatch over heights [%d, %d): the wallet shows %d receipts totalling %d atomic, we hold %d totalling %d",
			fromHeight, toHeight, walletCount, walletTotal, ourCount, ourTotal,
		)
	}
	return nil
}
