package controller

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"

	"github.com/stripe/stripe-go/v82"
	stripecheckout "github.com/stripe/stripe-go/v82/checkout/session"
)

// Buy data without signing in.
//
// POST /pay/data/checkout starts a hosted Stripe checkout for a data pack and
// hands back the url to send the customer to. The customer either names the
// network that should receive the data (the webhook then applies it on payment
// and emails an "it is on your network" note) or gives no network and receives
// a data code by email, exactly as today. Paying with USDC on Solana is
// pay_data_solana_controller.go.
//
// POST /pay/data/network-lookup answers "does a network with exactly this name
// exist" for the buy-data page. /auth/network-check is a sign-up similarity
// check and reads a name within a few characters of an existing one as taken,
// so it cannot answer that question.
//
// Both are unauthenticated and per-ip rate limited.

const (
	PayDataProviderStripe = "stripe"
)

// The checkout metadata the webhook reads. `apply_to_network` = "1" means the
// purchase was made for a named network and the data is applied on payment
// instead of only emailing a code.
const (
	payDataMetadataApplyToNetwork = "apply_to_network"
	payDataMetadataNetworkId      = "network_id"
	payDataMetadataNetworkName    = "network_name"
	payDataMetadataItemId         = "item_id"
	payDataMetadataApplyYes       = "1"
)

type PayDataCheckoutArgs struct {
	// "data_1tib" or "data_10tib"
	ItemId string `json:"item_id"`
	// "stripe"; empty means stripe
	Provider string `json:"provider"`
	// the network that should receive the data. Optional: without it the
	// customer gets a data code by email.
	NetworkName string `json:"network_name,omitempty"`
	// where the data code (or the applied note) is sent. Stripe collects the
	// email at checkout when it is not given here.
	Email string `json:"email,omitempty"`
}

type PayDataCheckoutResult struct {
	// the hosted checkout url to send the customer to
	Url      string `json:"url,omitempty"`
	Provider string `json:"provider,omitempty"`
	// the resolved network when a network name was given
	NetworkId *server.Id            `json:"network_id,omitempty"`
	Error     *PayDataCheckoutError `json:"error,omitempty"`
}

type PayDataCheckoutError struct {
	Message string `json:"message"`
}

func payDataCheckoutError(message string) *PayDataCheckoutResult {
	return &PayDataCheckoutResult{
		Error: &PayDataCheckoutError{Message: message},
	}
}

type PayDataNetworkLookupArgs struct {
	NetworkName string `json:"network_name"`
}

type PayDataNetworkLookupResult struct {
	Exists bool `json:"exists"`
	// the name as stored, when it exists
	NetworkName string `json:"network_name,omitempty"`
}

// payDataTarget is a validated checkout request: what is bought and who gets it.
type payDataTarget struct {
	ItemId    string
	ByteCount model.ByteCount
	// set when the purchase applies to a named network
	NetworkId   *server.Id
	NetworkName string
	Email       string
}

func (self *payDataTarget) applyToNetwork() bool {
	return self.NetworkId != nil
}

// -----------------------------------------------------------------------------
// per-ip limits
// -----------------------------------------------------------------------------

const (
	payDataCheckoutIpLimitPerMinute = 10
	payDataLookupIpLimitPerMinute   = 60
)

// payDataIpLimiter is a fixed one-minute window per client address, the same
// shape as the wallet validate limiter: cheap, in-process, and fail-open when
// the client address cannot be hashed (tests, local runs without the client
// secret).
type payDataIpLimiter struct {
	limitPerMinute int
	stateLock      sync.Mutex
	window         time.Time
	counts         map[[32]byte]int
}

var payDataCheckoutLimiter = &payDataIpLimiter{limitPerMinute: payDataCheckoutIpLimitPerMinute}
var payDataLookupLimiter = &payDataIpLimiter{limitPerMinute: payDataLookupIpLimitPerMinute}

func (self *payDataIpLimiter) allow(clientSession *session.ClientSession) (allow bool) {
	defer func() {
		if r := recover(); r != nil {
			allow = true
		}
	}()
	if clientSession == nil || clientSession.ClientAddress == "" {
		return true
	}
	clientAddressHash, _, err := clientSession.ClientAddressHashPort()
	if err != nil {
		return true
	}
	return self.allowHash(clientAddressHash, server.NowUtc())
}

func (self *payDataIpLimiter) allowHash(clientAddressHash [32]byte, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.window.IsZero() || time.Minute <= now.Sub(self.window) {
		self.window = now
		self.counts = map[[32]byte]int{}
	}
	self.counts[clientAddressHash] += 1
	return self.counts[clientAddressHash] <= self.limitPerMinute
}

// -----------------------------------------------------------------------------
// network lookup
// -----------------------------------------------------------------------------

func PayDataNetworkLookup(
	args *PayDataNetworkLookupArgs,
	clientSession *session.ClientSession,
) (*PayDataNetworkLookupResult, error) {
	networkName := strings.TrimSpace(args.NetworkName)
	if networkName == "" {
		return &PayDataNetworkLookupResult{Exists: false}, nil
	}
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("Too many lookups from this address. Try again in a minute.")
	}
	networkId, storedName := model.FindNetworkByName(clientSession.Ctx, networkName)
	if networkId == nil {
		return &PayDataNetworkLookupResult{Exists: false}, nil
	}
	return &PayDataNetworkLookupResult{
		Exists:      true,
		NetworkName: storedName,
	}, nil
}

// -----------------------------------------------------------------------------
// checkout
// -----------------------------------------------------------------------------

func PayDataCheckout(
	args *PayDataCheckoutArgs,
	clientSession *session.ClientSession,
) (*PayDataCheckoutResult, error) {
	target, provider, errMessage := payDataValidate(args)
	if errMessage != "" {
		return payDataCheckoutError(errMessage), nil
	}

	if !payDataCheckoutLimiter.allow(clientSession) {
		return payDataCheckoutError("Too many checkout attempts from this address. Try again in a minute."), nil
	}

	networkNameArg := strings.TrimSpace(args.NetworkName)
	if networkNameArg != "" {
		networkId, storedName := model.FindNetworkByName(clientSession.Ctx, networkNameArg)
		if networkId == nil {
			return payDataCheckoutError(fmt.Sprintf("No network named %s", networkNameArg)), nil
		}
		target.NetworkId = networkId
		target.NetworkName = storedName
	}

	checkoutUrl, errMessage := payDataStripeCheckout(target)
	if errMessage != "" {
		return payDataCheckoutError(errMessage), nil
	}

	glog.Infof(
		"[paydata]%s checkout started for %s (network %v)\n",
		provider, target.ItemId, target.NetworkId,
	)

	return &PayDataCheckoutResult{
		Url:       checkoutUrl,
		Provider:  provider,
		NetworkId: target.NetworkId,
	}, nil
}

// payDataValidate checks the request shape before anything is looked up or
// counted against the rate limit. The returned target has no network yet.
func payDataValidate(args *PayDataCheckoutArgs) (target payDataTarget, provider string, errMessage string) {
	itemId := strings.TrimSpace(args.ItemId)
	byteCount, ok := stripeDataPackByteCount(itemId)
	if !ok {
		return target, "", "Unknown item."
	}

	provider = strings.ToLower(strings.TrimSpace(args.Provider))
	switch provider {
	case "", PayDataProviderStripe:
		// the request shape keeps the field; stripe is the only hosted checkout
		provider = PayDataProviderStripe
	default:
		return target, "", "Unknown provider."
	}

	email := strings.TrimSpace(args.Email)
	if email != "" && !payDataValidEmail(email) {
		return target, "", "That email address does not look right."
	}

	target = payDataTarget{
		ItemId:    itemId,
		ByteCount: byteCount,
		Email:     email,
	}
	return target, provider, ""
}

// payDataValidEmail is a shape check only: the address is where a paid code is
// sent, so an obviously broken one is refused before money moves.
func payDataValidEmail(email string) bool {
	if len(email) > 254 {
		return false
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	return strings.Contains(email[at+1:], ".")
}

// payDataSuccessUrl is where the customer lands after paying. The configured
// success url (which carries Stripe's session id placeholder) gets the item and,
// when the purchase applies to a network, the network name, so the page can say
// what happened without another lookup.
func payDataSuccessUrl(successUrl string, itemId string, networkName string) string {
	query := url.Values{}
	query.Set("item", itemId)
	if networkName != "" {
		query.Set("network", networkName)
	}
	separator := "?"
	if strings.Contains(successUrl, "?") {
		separator = "&"
	}
	return successUrl + separator + query.Encode()
}

// -----------------------------------------------------------------------------
// stripe
// -----------------------------------------------------------------------------

// payDataStripeMetadata is what checkout.session.completed carries back so the
// webhook knows the purchase was for a named network.
func payDataStripeMetadata(target payDataTarget) map[string]string {
	metadata := map[string]string{
		payDataMetadataItemId: target.ItemId,
	}
	if target.applyToNetwork() {
		metadata[payDataMetadataApplyToNetwork] = payDataMetadataApplyYes
		metadata[payDataMetadataNetworkId] = target.NetworkId.String()
		metadata[payDataMetadataNetworkName] = target.NetworkName
	}
	return metadata
}

// payDataStripeSessionParams builds the hosted checkout session. The line items
// come from the same product and price lookups the signed-in checkout uses, so
// the webhook fulfils both the same way (sku by product id).
func payDataStripeSessionParams(
	target payDataTarget,
	urls StripeCheckoutUrls,
	lineItems []*stripe.CheckoutSessionLineItemParams,
) *stripe.CheckoutSessionParams {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems:  lineItems,
		SuccessURL: stripe.String(payDataSuccessUrl(urls.SuccessUrl, target.ItemId, target.NetworkName)),
		CancelURL:  stripe.String(urls.CancelUrl),
		Metadata:   payDataStripeMetadata(target),
	}
	if target.applyToNetwork() {
		// the fulfilment webhook resolves the network from this, exactly as it
		// does for a signed-in checkout
		params.ClientReferenceID = stripe.String(target.NetworkId.String())
	}
	if target.Email != "" {
		params.CustomerEmail = stripe.String(target.Email)
	}
	return params
}

func payDataStripeCheckout(target payDataTarget) (checkoutUrl string, errMessage string) {
	urls := stripeCheckoutUrls()
	if urls.SuccessUrl == "" || urls.CancelUrl == "" {
		// refuse rather than hand a customer to Stripe with no way back
		glog.Errorf("[paydata]stripe checkout urls are not configured\n")
		return "", "Checkout is not configured."
	}

	lineItems, errMessage := stripeDataPackLineItems(target.ItemId)
	if errMessage != "" {
		return "", errMessage
	}

	params := payDataStripeSessionParams(target, urls, lineItems)

	stripe.Key = stripeApiTokenFunc()
	checkoutSession, err := stripecheckout.New(params)
	if err != nil {
		glog.Errorf("[paydata]could not create stripe checkout session for %s: %s\n", target.ItemId, err)
		return "", "Could not start checkout. Please try again."
	}
	if checkoutSession.URL == "" {
		glog.Errorf("[paydata]stripe checkout session %s has no url\n", checkoutSession.ID)
		return "", "Could not start checkout. Please try again."
	}
	return checkoutSession.URL, ""
}
