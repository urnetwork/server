package controller

// ComputeServerStripe (DESIGN-COMPUTE §9.5, §9.11, §14.3, D-BAL-10; BAL-M12-08): sidecar/controller's
// ComputeStripeCheckout on the server, and the Stripe calls the compute branch of the Stripe webhook makes
// (compute_stripe_webhook.go), over Stripe's API with the vault's key (stripe.yml's api.token) and the config's compute
// block (stripe.yml's compute.product_id, and the checkout's urls, compute.success_url and compute.cancel_url, else
// Pro's checkout.success_url and checkout.cancel_url):
//
//	ComputeStripeCustomer  the Stripe customer of one network and one member, whose metadata, its mandate's, names
//	                       both: found by Stripe's search on that metadata and made, where none stands, under an
//	                       idempotency key of the two, which answers the same customer to a repeat within Stripe's
//	                       24 hours, by when its search finds it. A customer per member is what keeps one member's
//	                       card out of another's auto-renew (§9.5).
//	CreateCheckout         a Checkout session in payment mode on that customer: one line, the pack at a dollar a
//	                       credit on the compute product, tax added by Stripe Tax (automatic_tax, the billing address
//	                       saved to the customer); card payments only, since the payer's limits key on a card's
//	                       fingerprint; the PaymentIntent's capture_method manual, so nothing is taken until the
//	                       webhook has checked its payer; 3-D Secure requested (any) where the ledger asks for it;
//	                       setup_future_usage off_session only where the payer turns a mode on in this act; and
//	                       the session's and the PaymentIntent's metadata, which the webhook dispatches by, naming
//	                       the network, the member, the pack and the mode, never content. No client_reference_id is
//	                       set, so no Pro handler could read a network from it.
//
// Radar screens every card payment by Stripe's default. Every call to Stripe goes through a client of this
// instance's own, at its settings' url, so a test points one instance at a fake and changes nothing process-wide.
// InstallComputeStripe registers the process's instance with sidecar/controller once, for the api's compute routes
// (CSRV-M12-04's mount calls it beside InstallComputeLedgerSecrets).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stripe/stripe-go/v82"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// The metadata compute writes on its Stripe objects, which the webhook dispatches and credits by: identifiers. The
// keys are compute's own, never Pro's network_id, so no Pro handler resolves a compute object.
const (
	// On every compute session, PaymentIntent and customer: the format's version.
	ComputeStripeMetadataCompute = "urnetwork_compute"
	// The format's version, the marker's value.
	computeStripeMetadataVersion  = "1"
	computeStripeMetadataNetwork  = "compute_network_id"
	computeStripeMetadataMember   = "compute_member_id"
	computeStripeMetadataPack     = "compute_pack_credits"
	computeStripeMetadataRenew    = "compute_auto_renew"
	computeStripeMetadataBelow    = "compute_below_nano_usd"
	computeStripeMetadataTopUp    = "compute_top_up_pack_credits"
	computeStripeMetadataMonthMax = "compute_month_limit_nano_usd"
	// On a refund compute makes itself (a closed network's here, a self-serve refund's in BAL-M12-14): its reason,
	// which tells its charge.refunded apart from a refund made outside the ledger.
	ComputeStripeMetadataRefund = "compute_refund"
	// The auto-renew modes as the metadata spells them.
	computeStripeRenewThreshold = "threshold"
	computeStripeRenewMonthly   = "monthly"
)

// Stripe's words compute sends: identifiers of its API.
const (
	computeStripeCurrency       = "usd"
	computeStripeCaptureManual  = "manual"
	computeStripeOffSession     = "off_session"
	computeStripeThreeDSecure   = "any"
	computeStripeTaxExclusive   = "exclusive"
	computeStripeAddressAuto    = "auto"
	computeStripePaymentCard    = "card"
	computeStripeCentsPerCredit = 100
)

// A Stripe object's metadata names a compute purchase.
func computeStripeMarked(metadata map[string]string) bool {
	return metadata[ComputeStripeMetadataCompute] == computeStripeMetadataVersion
}

// The Stripe API and Checkout settings.
type ComputeStripeSettings struct {
	// Stripe's API, https://api.stripe.com; a test points it at its fake.
	ApiUrl string
	// One call to Stripe's API: 30 s, past Stripe's own answers and short of a webhook's delivery timeout.
	RequestTimeout time.Duration
	// How long a Checkout session stays open unpaid: 1 hour. Stripe takes 30 minutes to 24 hours. A reading: §9.5
	// names no figure.
	CheckoutExpireTime time.Duration
	// The most customers one search of a network and member reads: 10. More than one is a duplicate an operator
	// merges; the first is the answer.
	CustomerSearchMaxCount int64
	// The most refunds of one charge the webhook reads: 100, Stripe's largest page.
	RefundListMaxCount int64
}

// The defaults, the only place one is set.
func DefaultComputeStripeSettings() *ComputeStripeSettings {
	return &ComputeStripeSettings{
		ApiUrl:                 stripe.APIURL,
		RequestTimeout:         30 * time.Second,
		CheckoutExpireTime:     1 * time.Hour,
		CustomerSearchMaxCount: 10,
		RefundListMaxCount:     100,
	}
}

// The config compute's Checkout reads: its product and where Stripe sends the person back.
type ComputeStripeConfig struct {
	// The Stripe product every pack's line names: what tells a compute line from Pro's, whose products are its skus.
	ProductId  string
	SuccessUrl string
	CancelUrl  string
}

// No Stripe key or no compute product on this host: checkouts refuse, and the webhook leaves compute's authorizations
// uncaptured.
var ErrComputeStripeConfig = errors.New("compute stripe: no Stripe key or compute product is configured")

// The config's compute block, with Pro's checkout urls where it names none; ErrComputeStripeConfig where it names no
// product or no url is set.
func computeStripeConfigFromResources() (*ComputeStripeConfig, error) {
	var c struct {
		Compute *struct {
			ProductId  string `yaml:"product_id"`
			SuccessUrl string `yaml:"success_url"`
			CancelUrl  string `yaml:"cancel_url"`
		} `yaml:"compute"`
	}
	resource, err := server.Config.SimpleResource("stripe.yml")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComputeStripeConfig, err)
	}
	if err := resource.UnmarshalYamlE(&c); err != nil {
		return nil, fmt.Errorf("%w: stripe.yml: %v", ErrComputeStripeConfig, err)
	}
	if c.Compute == nil || strings.TrimSpace(c.Compute.ProductId) == "" {
		return nil, fmt.Errorf("%w: stripe.yml names no compute.product_id", ErrComputeStripeConfig)
	}
	config := &ComputeStripeConfig{ProductId: strings.TrimSpace(c.Compute.ProductId), SuccessUrl: c.Compute.SuccessUrl, CancelUrl: c.Compute.CancelUrl}
	if config.SuccessUrl == "" || config.CancelUrl == "" {
		urls := stripeCheckoutUrls()
		config.SuccessUrl, config.CancelUrl = urls.SuccessUrl, urls.CancelUrl
	}
	if config.SuccessUrl == "" || config.CancelUrl == "" {
		return nil, fmt.Errorf("%w: stripe.yml names no checkout urls", ErrComputeStripeConfig)
	}
	return config, nil
}

// The vault's Stripe key; ErrComputeStripeConfig where it holds none.
func computeStripeKeyFromVault() (string, error) {
	key, err := vaultNonblankString("stripe.yml", "api", "token")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrComputeStripeConfig, err)
	}
	return key, nil
}

// What one ComputeServerStripe reads its credentials, its config and the ledger from. A field left nil takes the
// process's own: the vault's key, the config's block, operator's global Env, the vault's ledger keys, and active once
// InstallComputeStripe ran in a binary that links MG-C1.
type ComputeStripeDeps struct {
	ApiKey  func() (string, error)
	Config  func() (*ComputeStripeConfig, error)
	Env     func() operator.Env
	Secrets func() sidecarcontroller.ComputeLedgerSecrets
	Ledger  *model.ComputeLedgerSettings
	// Whether compute's purchases run in this process: its compute routes mounted (InstallComputeStripe) in a binary
	// that links MG-C1, the ledger's tables. Inactive, the webhook judges an event by its own metadata alone, reading
	// no ledger table and asking Stripe nothing for another's event, and leaves compute's authorizations uncaptured.
	Active func() bool
}

// Compute's Stripe (see the file header). Safe for concurrent use: it holds its settings and deps alone.
type ComputeServerStripe struct {
	settings *ComputeStripeSettings
	deps     ComputeStripeDeps
}

var _ sidecarcontroller.ComputeStripeCheckout = (*ComputeServerStripe)(nil)

// An instance over settings and deps, nil taking the defaults and the process's own.
func NewComputeServerStripe(settings *ComputeStripeSettings, deps *ComputeStripeDeps) *ComputeServerStripe {
	if settings == nil {
		settings = DefaultComputeStripeSettings()
	}
	self := &ComputeServerStripe{settings: settings}
	if deps != nil {
		self.deps = *deps
	}
	if self.deps.ApiKey == nil {
		self.deps.ApiKey = computeStripeKeyFromVault
	}
	if self.deps.Config == nil {
		self.deps.Config = computeStripeConfigFromResources
	}
	if self.deps.Env == nil {
		self.deps.Env = operator.GlobalEnv
	}
	if self.deps.Secrets == nil {
		secrets := NewComputeServerSecrets(nil)
		self.deps.Secrets = func() sidecarcontroller.ComputeLedgerSecrets { return secrets }
	}
	if self.deps.Ledger == nil {
		self.deps.Ledger = model.DefaultComputeLedgerSettings()
	}
	if self.deps.Active == nil {
		self.deps.Active = func() bool {
			return computeStripeInstalled.Load() && server.SidecarMgC1Linked()
		}
	}
	return self
}

// A Stripe client of this instance's own, at its url and timeout, with the vault's key now.
func (self *ComputeServerStripe) client() (*stripe.Client, error) {
	key, err := self.deps.ApiKey()
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: self.settings.RequestTimeout}
	backend := func(backendType stripe.SupportedBackend, url string) stripe.Backend {
		return stripe.GetBackendWithConfig(backendType, &stripe.BackendConfig{URL: stripe.String(url), HTTPClient: httpClient})
	}
	return stripe.NewClient(key, stripe.WithBackends(&stripe.Backends{
		API:         backend(stripe.APIBackend, self.settings.ApiUrl),
		Connect:     backend(stripe.ConnectBackend, stripe.ConnectURL),
		Uploads:     backend(stripe.UploadsBackend, stripe.UploadsURL),
		MeterEvents: backend(stripe.MeterEventsBackend, stripe.MeterEventsURL),
	})), nil
}

// The idempotency key of the customer of networkId and memberId: what makes one customer of a repeat.
func computeStripeCustomerKey(networkId server.Id, memberId server.Id) string {
	return fmt.Sprintf("compute_customer_%s_%s", networkId, memberId)
}

// The Stripe customer of networkId and memberId, found or made (see the file header): ComputeStripeCustomer.
func (self *ComputeServerStripe) ComputeStripeCustomer(ctx context.Context, networkId server.Id, memberId server.Id) (string, error) {
	client, err := self.client()
	if err != nil {
		return "", err
	}
	return self.customer(ctx, client, networkId, memberId)
}

func (self *ComputeServerStripe) customer(ctx context.Context, client *stripe.Client, networkId server.Id, memberId server.Id) (string, error) {
	query := fmt.Sprintf(
		"metadata['%s']:'%s' AND metadata['%s']:'%s'",
		computeStripeMetadataNetwork, networkId, computeStripeMetadataMember, memberId,
	)
	search := &stripe.CustomerSearchParams{SearchParams: stripe.SearchParams{Query: query, Limit: stripe.Int64(self.settings.CustomerSearchMaxCount)}}
	read := int64(0)
	for customer, err := range client.V1Customers.Search(ctx, search) {
		if err != nil {
			return "", fmt.Errorf("compute stripe: the customer search: %w", err)
		}
		read += 1
		if customer.Metadata[computeStripeMetadataNetwork] == networkId.String() && customer.Metadata[computeStripeMetadataMember] == memberId.String() && !customer.Deleted {
			return customer.ID, nil
		}
		if self.settings.CustomerSearchMaxCount <= read {
			break
		}
	}
	params := &stripe.CustomerCreateParams{
		Metadata: map[string]string{
			ComputeStripeMetadataCompute: computeStripeMetadataVersion,
			computeStripeMetadataNetwork: networkId.String(),
			computeStripeMetadataMember:  memberId.String(),
		},
	}
	params.SetIdempotencyKey(computeStripeCustomerKey(networkId, memberId))
	customer, err := client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("compute stripe: the customer: %w", err)
	}
	return customer.ID, nil
}

// The metadata of request's session and PaymentIntent: the network, the member, the pack and the mode with its
// settings, never content.
func computeStripeCheckoutMetadata(request *sidecarcontroller.ComputeStripeCheckoutRequest) map[string]string {
	metadata := map[string]string{
		ComputeStripeMetadataCompute: computeStripeMetadataVersion,
		computeStripeMetadataNetwork: request.NetworkId.String(),
		computeStripeMetadataMember:  request.MemberId.String(),
		computeStripeMetadataPack:    strconv.FormatInt(request.PackCredits, 10),
	}
	switch request.AutoRenew {
	case model.ComputeAutoRenewModeThreshold:
		metadata[computeStripeMetadataRenew] = computeStripeRenewThreshold
		metadata[computeStripeMetadataBelow] = strconv.FormatInt(request.BelowNanoUsd, 10)
		metadata[computeStripeMetadataTopUp] = strconv.FormatInt(request.TopUpPackCredits, 10)
		metadata[computeStripeMetadataMonthMax] = strconv.FormatInt(request.MonthLimitNanoUsd, 10)
	case model.ComputeAutoRenewModeMonthly:
		metadata[computeStripeMetadataRenew] = computeStripeRenewMonthly
	}
	return metadata
}

// See sidecarcontroller.ComputeStripeCheckout: request's session (see the file header). A host with no key or no
// compute product answers ErrComputeStripeUnavailable, which the route answers 503.
func (self *ComputeServerStripe) CreateCheckout(ctx context.Context, request *sidecarcontroller.ComputeStripeCheckoutRequest) (*sidecarcontroller.ComputeStripeCheckoutSession, error) {
	if request.PackCredits <= 0 || request.AmountNanoUsd != request.PackCredits*model.ComputeCreditNanoUsd {
		return nil, fmt.Errorf("%w: a pack is priced at a dollar a credit", sidecarcontroller.ErrComputeCheckoutArgs)
	}
	config, err := self.deps.Config()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sidecarcontroller.ErrComputeStripeUnavailable, err)
	}
	client, err := self.client()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sidecarcontroller.ErrComputeStripeUnavailable, err)
	}
	customerId, err := self.customer(ctx, client, request.NetworkId, request.MemberId)
	if err != nil {
		return nil, err
	}
	metadata := computeStripeCheckoutMetadata(request)
	intentMetadata := map[string]string{}
	for key, value := range metadata {
		intentMetadata[key] = value
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode:     stripe.String(string(stripe.CheckoutSessionModePayment)),
		Customer: stripe.String(customerId),
		CustomerUpdate: &stripe.CheckoutSessionCreateCustomerUpdateParams{
			Address: stripe.String(computeStripeAddressAuto),
		},
		AutomaticTax: &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(true)},
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:    stripe.String(computeStripeCurrency),
				Product:     stripe.String(config.ProductId),
				UnitAmount:  stripe.Int64(request.PackCredits * computeStripeCentsPerCredit),
				TaxBehavior: stripe.String(computeStripeTaxExclusive),
			},
			Quantity: stripe.Int64(1),
		}},
		PaymentMethodTypes: []*string{stripe.String(computeStripePaymentCard)},
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			CaptureMethod: stripe.String(computeStripeCaptureManual),
			Metadata:      intentMetadata,
		},
		Metadata:   metadata,
		SuccessURL: stripe.String(config.SuccessUrl),
		CancelURL:  stripe.String(config.CancelUrl),
		ExpiresAt:  stripe.Int64(server.NowUtc().Add(self.settings.CheckoutExpireTime).Unix()),
	}
	if request.SaveCard {
		params.PaymentIntentData.SetupFutureUsage = stripe.String(computeStripeOffSession)
	}
	if request.ThreeDSecure {
		params.PaymentMethodOptions = &stripe.CheckoutSessionCreatePaymentMethodOptionsParams{
			Card: &stripe.CheckoutSessionCreatePaymentMethodOptionsCardParams{RequestThreeDSecure: stripe.String(computeStripeThreeDSecure)},
		}
	}
	session, err := client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the checkout session: %w", err)
	}
	if session.URL == "" || session.ExpiresAt <= 0 {
		return nil, errors.New("compute stripe: a checkout session with no url or expiry")
	}
	return &sidecarcontroller.ComputeStripeCheckoutSession{
		SessionId:  session.ID,
		Url:        session.URL,
		ExpireTime: time.Unix(session.ExpiresAt, 0).UTC(),
	}, nil
}

// One compute purchase as its PaymentIntent's metadata names it.
type computeStripePurchase struct {
	networkId   server.Id
	memberId    server.Id
	packCredits int64
	// The mode its payer turned on in that act, zero for none, with threshold's settings.
	autoRenew         model.ComputeAutoRenewMode
	belowNanoUsd      int64
	topUpPackCredits  int64
	monthLimitNanoUsd int64
}

// What the pack credits, in nano-USD: a credit a dollar.
func (self *computeStripePurchase) amountNanoUsd() int64 {
	return self.packCredits * model.ComputeCreditNanoUsd
}

// The purchase metadata names, refused where it is not compute's whole format or names a pack the settings do not
// sell: a compute object no checkout of ours made credits nothing.
func computeStripePurchaseOf(ledger *model.ComputeLedgerSettings, metadata map[string]string) (*computeStripePurchase, error) {
	if !computeStripeMarked(metadata) {
		return nil, errors.New("compute stripe: not a compute purchase")
	}
	networkId, err := server.ParseId(metadata[computeStripeMetadataNetwork])
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the purchase's network: %w", err)
	}
	memberId, err := server.ParseId(metadata[computeStripeMetadataMember])
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the purchase's member: %w", err)
	}
	purchase := &computeStripePurchase{networkId: networkId, memberId: memberId}
	if purchase.packCredits, err = strconv.ParseInt(metadata[computeStripeMetadataPack], 10, 64); err != nil {
		return nil, fmt.Errorf("compute stripe: the purchase's pack: %w", err)
	}
	found := false
	for _, credits := range ledger.StripePackCredits {
		found = found || credits == purchase.packCredits
	}
	if !found {
		return nil, fmt.Errorf("compute stripe: a pack of %d credits is not sold", purchase.packCredits)
	}
	switch metadata[computeStripeMetadataRenew] {
	case "":
	case computeStripeRenewThreshold:
		purchase.autoRenew = model.ComputeAutoRenewModeThreshold
		for key, target := range map[string]*int64{
			computeStripeMetadataBelow:    &purchase.belowNanoUsd,
			computeStripeMetadataTopUp:    &purchase.topUpPackCredits,
			computeStripeMetadataMonthMax: &purchase.monthLimitNanoUsd,
		} {
			value, err := strconv.ParseInt(metadata[key], 10, 64)
			if err != nil || value < 0 {
				return nil, fmt.Errorf("compute stripe: the purchase's %s", key)
			}
			*target = value
		}
	default:
		return nil, fmt.Errorf("compute stripe: no auto-renew mode %q rides a pack", metadata[computeStripeMetadataRenew])
	}
	return purchase, nil
}

var (
	installComputeStripeOnce sync.Once
	// Whether InstallComputeStripe ran: what makes the process's instance active.
	computeStripeInstalled atomic.Bool
)

// Registers the process's compute Stripe with sidecar/controller, once a process, and makes its webhook branch active:
// a second call does nothing.
func InstallComputeStripe() {
	installComputeStripeOnce.Do(func() {
		sidecarcontroller.RegisterComputeStripeCheckout(computeStripeProcess())
		computeStripeInstalled.Store(true)
	})
}

// The process's instance, which the webhook and the checkout route share. Replaceable only by the hermetic webhook
// tests, which point it at a fake Stripe and their own env; production never mutates it.
var computeStripeProcess = sync.OnceValue(func() *ComputeServerStripe {
	return NewComputeServerStripe(nil, nil)
})
